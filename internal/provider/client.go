package provider

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"
)

// Hub admin API client used by every resource + data source. Wraps net/http
// with a User-Agent that identifies the provider version (so Hub audit_log
// can attribute changes), a 30s timeout, structured tflog tracing, and
// TLS 1.2+ enforcement.
//
// Authentication: the system-scope `ak_…` key is sent as
// `Authorization: Bearer ak_<id>.<secret>` exactly as the Hub expects.
// The key is NEVER logged. Error bodies are redacted before reaching
// diagnostics so a misbehaving upstream proxy that echoes the Authorization
// header into a 502 page cannot leak the bearer into CI logs.
type httpClient struct {
	base      string
	adminKey  string
	userAgent string
	http      *http.Client
}

func newClient(endpoint, adminKey, version string) *httpClient {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &httpClient{
		base:      endpoint,
		adminKey:  adminKey,
		userAgent: fmt.Sprintf("terraform-provider-innfactoryhub/%s", version),
		http: &http.Client{
			Timeout:   30 * time.Second,
			Transport: tr,
		},
	}
}

type apiError struct {
	Status  int
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *apiError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("hub api %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("hub api %d: %s", e.Status, e.Message)
}

// Helper to detect "not found" so callers can map it to RemoveFromState.
func isNotFound(err error) bool {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Status == http.StatusNotFound
	}
	return false
}

// Matches the `ak_<id>.<secret>` wire shape so we can strip the bearer from
// any text we are about to surface as a diagnostic. Conservative: cuts
// anything that even looks like the prefix, including substrings.
var akKeyPattern = regexp.MustCompile(`ak_[A-Za-z0-9_-]{1,64}\.[A-Za-z0-9_-]{1,64}`)

// redact removes anything that looks like an API key from an arbitrary
// string. Used before propagating a 4xx/5xx body — a misbehaving reverse
// proxy or upstream debug page that echoes the request's Authorization
// header would otherwise put the bearer into a Terraform error message and
// thus into CI logs.
func redact(s string) string {
	return akKeyPattern.ReplaceAllString(s, "ak_***REDACTED***")
}

// Cap on how much of an unstructured error body we propagate. 512 bytes is
// plenty to identify the cause but small enough to keep stack traces and
// debug dumps from leaking through.
const maxBodyForError = 512

func (c *httpClient) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal body: %w", err)
		}
		reqBody = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, u, reqBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.adminKey)
	req.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	// Log only the path — not the query string — at trace level. The query
	// can contain customer cloud handles which are not secrets but still
	// sensitive in shared CI logs.
	tflog.Trace(ctx, "hub_request", map[string]any{
		"method": method,
		"path":   path,
	})

	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", redactErr(err))
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}

	tflog.Trace(ctx, "hub_response", map[string]any{
		"status": res.StatusCode,
		"bytes":  len(raw),
	})

	if res.StatusCode >= 400 {
		ae := &apiError{Status: res.StatusCode}
		var env struct {
			Error apiError `json:"error"`
		}
		if json.Unmarshal(raw, &env) == nil && env.Error.Code != "" {
			ae.Code = env.Error.Code
			ae.Message = redact(env.Error.Message)
		} else {
			// Unstructured body — cap + redact. Avoid raw upstream HTML, proxy
			// debug pages, etc. leaking the bearer into CI logs.
			snippet := string(raw)
			if len(snippet) > maxBodyForError {
				snippet = snippet[:maxBodyForError] + "…"
			}
			ae.Message = redact(snippet)
		}
		return ae
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// redactErr wraps an error such that any printed form runs through redact().
// Used on transport errors where Go's stdlib sometimes echoes the URL —
// which won't contain the bearer, but better safe.
type redactedErr struct{ inner error }

func (r *redactedErr) Error() string { return redact(r.inner.Error()) }
func (r *redactedErr) Unwrap() error { return r.inner }

func redactErr(err error) error {
	if err == nil {
		return nil
	}
	return &redactedErr{inner: err}
}

// envelope is the Hub's response wrapper: `{ data: <T>, request_id }`.
// Resources unmarshal into envelope[<their shape>].
type envelope[T any] struct {
	Data      T      `json:"data"`
	RequestID string `json:"request_id"`
}
