package provider

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"
)

// Creates are idempotent towards the AI Hub (SEC-014): every create carries
// a fresh Idempotency-Key, and a create whose outcome the provider never
// saw is sent once more with the same key, so the Hub replays the first one
// instead of creating a second object. The key lives only for that create:
// Terraform keeps nothing of a create that failed.

// createRetryPause separates the two attempts of a create.
const createRetryPause = time.Second

// newIdempotencyKey returns a random UUID v4.
func newIdempotencyKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("idempotency key: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // RFC 9562 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// outcomeUnknown reports a write whose result the provider never saw: no
// response, an unreadable one, or a 5xx (e.g. the hosting layer timing out
// while the Hub carried on). A cancelled apply is not retried.
func outcomeUnknown(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil || errors.Is(err, errMissingAdminKey) {
		return false
	}
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Status >= http.StatusInternalServerError
	}
	return true
}

// create POSTs body with an Idempotency-Key. If the outcome stays unknown it
// runs beforeRetry (if any) and sends the same request once more; it reports
// whether it retried.
func (c *httpClient) create(ctx context.Context, path string, body, out any, beforeRetry func()) (retried bool, err error) {
	key, err := newIdempotencyKey()
	if err != nil {
		return false, err
	}
	err = c.send(ctx, http.MethodPost, path, nil, body, out, key)
	if !outcomeUnknown(ctx, err) {
		return false, err
	}
	tflog.Warn(ctx, "create_outcome_unknown_retrying", map[string]any{"path": path, "error": err.Error()})
	if beforeRetry != nil {
		beforeRetry()
	}
	if perr := pause(ctx, createRetryPause); perr != nil {
		return false, err
	}
	return true, c.send(ctx, http.MethodPost, path, nil, body, out, key)
}

// pause waits for d unless ctx ends first.
func pause(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
