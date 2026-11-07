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
	"strings"
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
// The key is NEVER logged. Error texts are redacted before reaching
// diagnostics so a misbehaving upstream proxy that echoes the Authorization
// header into a 502 page cannot leak the bearer — or a licence key or
// licence token the Hub returned — into CI logs.
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
		// Tolerate `https://hub.innfactory.ai/` — paths always start with "/".
		base:      strings.TrimRight(endpoint, "/"),
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
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details"`
}

// Error is always redacted, including code and details: the fields come
// from the wire and are not trusted to be free of secrets.
func (e *apiError) Error() string {
	msg := fmt.Sprintf("hub api %d", e.Status)
	if e.Code != "" {
		msg += " " + e.Code
	}
	msg += ": " + e.Message
	if d := compactDetails(e.Details); d != "" {
		msg += " (details: " + d + ")"
	}
	return redact(msg)
}

// compactDetails renders error.details on one line, redacted and then
// capped like an unstructured body (redacting first means the cap cannot
// leave an unmatched fragment of a secret behind). The Hub puts the
// actionable part there, e.g. the editions that exist or the licence that
// already holds a position.
func compactDetails(raw json.RawMessage) string {
	var buf bytes.Buffer
	if len(raw) == 0 || json.Compact(&buf, raw) != nil || buf.String() == "null" {
		return ""
	}
	s := redact(buf.String())
	if len(s) > maxBodyForError {
		s = s[:maxBodyForError] + "…"
	}
	return s
}

// Helper to detect "not found" so callers can map it to RemoveFromState.
func isNotFound(err error) bool {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Status == http.StatusNotFound
	}
	return false
}

// secretPatterns match every credential shape the provider handles so we
// can strip them from any text we are about to surface as a diagnostic:
//   - `ak_<id>.<secret>` — Hub API keys (the provider's own bearer)
//   - `lk_<id>.<secret>` — AI Hub licence keys
//   - `eyJ….….…`        — compact JWS (licence, AI-Prices, revocation JWTs)
//
// Conservative: cuts anything that even looks like the shape, including
// substrings, and never caps the length (a capped pattern leaves the tail of
// a long secret behind). RE2 guarantees linear time on hostile input.
var secretPatterns = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`(ak|lk)_[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`), "${1}_***REDACTED***"},
	{regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*`), "***REDACTED_JWT***"},
}

// redact removes anything that looks like a key or token from an arbitrary
// string. Used before propagating a 4xx/5xx body — a misbehaving reverse
// proxy or upstream debug page that echoes the request's Authorization
// header would otherwise put the bearer into a Terraform error message and
// thus into CI logs.
func redact(s string) string {
	for _, p := range secretPatterns {
		s = p.re.ReplaceAllString(s, p.with)
	}
	return s
}

// Cap on how much of an unstructured error body we propagate. 512 bytes is
// plenty to identify the cause but small enough to keep stack traces and
// debug dumps from leaking through.
const maxBodyForError = 512

// errMissingAdminKey is returned by every operation when neither
// provider.admin_key nor INNFACTORYHUB_ADMIN_KEY is set.
var errMissingAdminKey = errors.New("missing admin key: set provider.admin_key, or export INNFACTORYHUB_ADMIN_KEY with a system-scope key (`ak_…`, permission write) minted in the Hub admin UI")

func (c *httpClient) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	if c.adminKey == "" {
		return errMissingAdminKey
	}
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
		return fmt.Errorf("read body: %w", redactErr(err))
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
			ae.Details = env.Error.Details
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
		return fmt.Errorf("decode response: %w", redactErr(err))
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
