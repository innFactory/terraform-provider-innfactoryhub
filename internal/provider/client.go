package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"
)

// Hub admin API client used by every resource + data source. Wraps net/http
// with a User-Agent that identifies the provider version (so Hub audit_log
// can attribute changes), a 30s timeout, and structured tflog tracing.
//
// Authentication: the system-scope `ak_…` key is sent as
// `Authorization: Bearer ak_<id>.<secret>` exactly as the Hub expects.
// The key is NEVER logged — see `tflog.MaskFieldValuesStrings` set up in
// resource Configure().
type httpClient struct {
	base       string
	adminKey   string
	userAgent  string
	http       *http.Client
}

func newClient(endpoint, adminKey, version string) *httpClient {
	return &httpClient{
		base:      endpoint,
		adminKey:  adminKey,
		userAgent: fmt.Sprintf("terraform-provider-innfactoryhub/%s", version),
		http:      &http.Client{Timeout: 30 * time.Second},
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

	tflog.Trace(ctx, "hub_request", map[string]any{
		"method": method,
		"url":    u,
	})

	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
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
		ae := &apiError{Status: res.StatusCode, Message: string(raw)}
		var env struct {
			Error apiError `json:"error"`
		}
		if json.Unmarshal(raw, &env) == nil && env.Error.Code != "" {
			ae.Code = env.Error.Code
			ae.Message = env.Error.Message
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

// envelope is the Hub's response wrapper: `{ data: <T>, request_id }`.
// Resources unmarshal into envelope[<their shape>].
type envelope[T any] struct {
	Data      T      `json:"data"`
	RequestID string `json:"request_id"`
}
