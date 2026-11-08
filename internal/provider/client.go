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
// can attribute changes), an optional X-Audit-Context (who ran Terraform —
// e.g. the CI run), a 30s timeout, structured tflog tracing, TLS 1.2+
// enforcement, and no redirects.
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
	// auditContext is sent as X-Audit-Context when non-empty; set it only
	// through sanitizeAuditContext.
	auditContext string
	http         *http.Client
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
			// Never follow a redirect: Go keeps the Authorization header on a
			// same-host redirect, even from https to http or to another port.
			// The Hub API does not redirect, so any 3xx is reported as an error.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// maxAuditContext is the Hub's limit for X-Audit-Context.
const maxAuditContext = 256

// sanitizeAuditContext makes s safe for the X-Audit-Context header and the
// Hub's audit log: printable ASCII only (control characters such as line
// breaks become spaces, anything else '?'), no secrets, at most 256
// characters. A blank result means "send no header".
func sanitizeAuditContext(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f:
			return ' '
		case r > 0x7e:
			return '?'
		}
		return r
	}, s)
	s = strings.TrimSpace(redact(s))
	if len(s) > maxAuditContext {
		s = strings.TrimSpace(s[:maxAuditContext])
	}
	return s
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

// Hub error codes that say the object itself does not exist. Only these
// drop a resource from state: any other 404 (a proxy page, a wrong path,
// another service) is an error, or the next apply would create the object a
// second time.
const (
	codeLicenseNotFound     = "license_not_found"
	codeEnvironmentNotFound = "environment_not_found"
	codeAPIKeyNotFound      = "api_key_not_found"
)

// isHubNotFound reports a 404 carrying the Hub's own "not found" code for
// the object, e.g. license_not_found.
func isHubNotFound(err error, code string) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound && ae.Code == code
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
var errMissingAdminKey = errors.New("missing admin key: set provider.admin_key, or export INNFACTORYHUB_ADMIN_KEY with a system-scope key (`ak_…`) " +
	"minted in the Hub admin UI — on the innFactory AI Hub [\"read\", \"licensing\"] to apply and [\"read_licensing\"] for plans, on innfactoryHUB one with write")

func (c *httpClient) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	return c.send(ctx, method, path, query, body, out, "")
}

// send is do with an optional Idempotency-Key (see create).
func (c *httpClient) send(ctx context.Context, method, path string, query url.Values, body, out any, idempotencyKey string) error {
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
	if c.auditContext != "" {
		req.Header.Set("X-Audit-Context", c.auditContext)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
		// net/http would silently resend a request with this header after
		// some connection errors; the resource retries on its own instead,
		// after looking up what the first attempt may have created.
		req.GetBody = nil
	}

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

	if res.StatusCode >= 300 && res.StatusCode < 400 {
		return redirectError(res)
	}
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

// redirectError reports a 3xx the client refused to follow. The target is
// shown (redacted and capped) so a misconfigured endpoint is easy to spot.
func redirectError(res *http.Response) *apiError {
	loc := res.Header.Get("Location")
	if len(loc) > maxBodyForError {
		loc = loc[:maxBodyForError] + "…"
	}
	return &apiError{
		Status:  res.StatusCode,
		Message: redact(fmt.Sprintf("unexpected redirect to %q refused — the endpoint must be the Hub itself, e.g. https://hub.innfactory.ai", loc)),
	}
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
