package provider

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"time"
)

// API keys and key permissions of the fake Hub, modelled on the AI Hub
// (`/api/v1/admin/api-keys`, key-scope gate):
//   - the provider's bearer decides what it may do: a full key (read, write),
//     a `licensing` key (read, licensing) or a `read_licensing` key
//   - licensing keys reach only environments, licences (incl. key
//     rotation), GET /tenants/lookup and organization keys; anything else is
//     403 forbidden_key_scope. They create organization keys only as
//     ["read_maintenance"] and list and revoke organization keys only.
//   - read_licensing keys: GET on the same routes; writes are 403
//     forbidden_read_only_key
//   - no API key creates a system key (403 forbidden_key_minting) unless the
//     fake plays innfactoryHUB (mintSystemKeys)
//   - organization keys: read and/or read_maintenance, optional expires_at
//     at most 24 months ahead; system keys expire after at most 12 months
//     (default 12 months)

// Bearers the fake Hub accepts besides testAdminKey.
const (
	testLicensingKey     = "ak_licensing01.bGljZW5zaW5nLWtleS1zZWNyZXQ"
	testReadLicensingKey = "ak_readlicns01.cmVhZC1saWNlbnNpbmcta2V5"
)

type fakeCaller struct {
	permissions []string
}

type callerKey struct{}

var fakeCallers = map[string]fakeCaller{
	"Bearer " + testAdminKey:         {permissions: []string{"read", "write"}},
	"Bearer " + testLicensingKey:     {permissions: []string{"read", "licensing"}},
	"Bearer " + testReadLicensingKey: {permissions: []string{"read_licensing"}},
}

func (c fakeCaller) has(p string) bool { return slices.Contains(c.permissions, p) }

// licensingRoute is what licensing and read_licensing keys may reach.
var licensingRoute = regexp.MustCompile(`^/api/v1/admin/(environments(/[^/]+)?|licenses(/[^/]+(/keys/rotate)?)?|tenants/lookup|api-keys(/[^/]+(/revoke)?)?)$`)

// gate mirrors the Hub's keyScopeGate and write gate for restricted keys.
func (c fakeCaller) gate(method, path string) *fakeHTTPError {
	licensing := c.has("licensing") && !c.has("write")
	readLicensing := c.has("read_licensing") && !c.has("write") && !licensing
	if !licensing && !readLicensing {
		return nil
	}
	if !licensingRoute.MatchString(path) {
		return &fakeHTTPError{status: http.StatusForbidden, code: "forbidden_key_scope", message: "API key scope does not cover this request"}
	}
	if readLicensing && method != http.MethodGet {
		return &fakeHTTPError{status: http.StatusForbidden, code: "forbidden_read_only_key", message: "API key is read-only"}
	}
	return nil
}

func (c fakeCaller) licensingOnly() bool { return c.has("licensing") && !c.has("write") }

func callerOf(r *http.Request) fakeCaller {
	c, _ := r.Context().Value(callerKey{}).(fakeCaller)
	return c
}

type fakeAPIKey struct {
	KeyID         string   `json:"key_id"`
	Name          string   `json:"name"`
	Scope         string   `json:"scope"`
	TenantID      *string  `json:"tenant_id"`
	Permissions   []string `json:"permissions"`
	DisplayPrefix string   `json:"display_prefix"`
	CreatedAt     string   `json:"created_at"`
	RevokedAt     *string  `json:"revoked_at"`
	ExpiresAt     *string  `json:"expires_at"`
}

// --- inspection and out-of-band helpers (all lock-protected) ---

// apiKeyIDs lists the keys that are not revoked.
func (h *fakeHub) apiKeyIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var ids []string
	for id, k := range h.apiKeys {
		if k.RevokedAt == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func (h *fakeHub) apiKey(id string) (fakeAPIKey, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	k, ok := h.apiKeys[id]
	if !ok {
		return fakeAPIKey{}, false
	}
	return *k, true
}

// allowSystemKeyMinting makes the fake behave like innfactoryHUB, where a
// system key with write may create system keys.
func (h *fakeHub) allowSystemKeyMinting() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.mintSystemKeys = true
}

// revokeAPIKeyOutOfBand revokes a key as an admin in the portal would.
func (h *fakeHub) revokeAPIKeyOutOfBand(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	at := nowISO()
	h.apiKeys[id].RevokedAt = &at
}

// deleteAPIKeyOutOfBand revokes and deletes a key (the portal deletes only
// revoked keys, 30 days after revocation).
func (h *fakeHub) deleteAPIKeyOutOfBand(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.apiKeys, id)
}

// expireAPIKeyOutOfBand moves the expiry of a key into the past.
func (h *fakeHub) expireAPIKeyOutOfBand(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	at := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	h.apiKeys[id].ExpiresAt = &at
}

// --- handlers ---

// listAPIKeys mirrors GET /api-keys; licensing keys see organization keys only.
func (h *fakeHub) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	orgOnly := callerOf(r).licensingOnly()
	h.mu.Lock()
	keys := make([]fakeAPIKey, 0, len(h.apiKeys))
	for _, k := range h.apiKeys {
		if !orgOnly || k.Scope == "organization" {
			keys = append(keys, *k)
		}
	}
	h.mu.Unlock()
	writeData(w, http.StatusOK, map[string]any{"keys": keys})
}

var fakeKeyPermissions = []string{"read", "read_accounting", "write", "licensing", "read_licensing", "read_maintenance"}

// fakeKeyRules mirrors assertScopeAndPermissions of the Hub.
func fakeKeyRules(scope string, tenantID *string, perms []string) *fakeHTTPError {
	has := func(p string) bool { return slices.Contains(perms, p) }
	if len(perms) == 0 {
		return badRequest("permissions: at least one")
	}
	for _, p := range perms {
		if !slices.Contains(fakeKeyPermissions, p) {
			return badRequest("unknown permission " + p)
		}
	}
	switch {
	case scope != "system" && scope != "organization":
		return badRequest("scope: system | organization")
	case scope == "organization" && (tenantID == nil || *tenantID == ""):
		return badRequest("tenant_id required for organization-scoped keys")
	case scope == "system" && tenantID != nil:
		return badRequest("system-scoped keys must not carry a tenant_id")
	case scope == "organization" && has("write"):
		return &fakeHTTPError{status: http.StatusBadRequest, code: "forbidden_org_write", message: "Organization-scoped keys are read-only for now"}
	case scope == "organization" && (has("licensing") || has("read_licensing")):
		return &fakeHTTPError{status: http.StatusBadRequest, code: "forbidden_org_licensing", message: "licensing is a system-scope permission"}
	case scope == "system" && has("read_maintenance"):
		return badRequest("read_maintenance is an organization-scope permission")
	}
	return nil
}

// fakeKeyExpiry applies the Hub's limits: in the future; system keys at most
// (and by default) 12 months, organization keys at most 24 months.
func fakeKeyExpiry(scope string, raw *string) (*string, *fakeHTTPError) {
	now := time.Now().UTC()
	months := 24
	if scope == "system" {
		months = 12
	}
	latest := now.AddDate(0, months, 0)
	if raw == nil {
		if scope == "system" {
			v := latest.Format(time.RFC3339Nano)
			return &v, nil
		}
		return nil, nil
	}
	at, err := time.Parse(time.RFC3339Nano, *raw)
	switch {
	case err != nil:
		return nil, badRequest("expires_at: ISO date-time with offset")
	case !at.After(now):
		return nil, badRequest("expires_at must be in the future")
	case at.After(latest):
		return nil, &fakeHTTPError{status: http.StatusBadRequest, code: "bad_request",
			message: fmt.Sprintf("Keys of scope %s expire after at most %d months", scope, months), details: map[string]any{"max_expires_at": latest.Format(time.RFC3339Nano)}}
	}
	// The Hub stores a timestamp and returns it in its own format.
	v := at.UTC().Format("2006-01-02T15:04:05.000Z")
	return &v, nil
}

func (h *fakeHub) createAPIKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string   `json:"name"`
		Scope       string   `json:"scope"`
		TenantID    *string  `json:"tenant_id"`
		Permissions []string `json:"permissions"`
		ExpiresAt   *string  `json:"expires_at"`
	}
	if herr := decodeStrict(r, &body); herr != nil {
		herr.write(w)
		return
	}
	caller := callerOf(r)
	h.mu.Lock()
	mintSystem := h.mintSystemKeys
	h.mu.Unlock()
	if body.Scope == "system" && !mintSystem {
		(&fakeHTTPError{status: http.StatusForbidden, code: "forbidden_key_minting", message: "System keys can only be created by staff signed in with TOTP"}).write(w)
		return
	}
	if caller.licensingOnly() && (body.Scope != "organization" || !slices.Equal(body.Permissions, []string{"read_maintenance"})) {
		(&fakeHTTPError{status: http.StatusForbidden, code: "forbidden_key_scope", message: "licensing keys create organization keys with read_maintenance only"}).write(w)
		return
	}
	if herr := fakeKeyRules(body.Scope, body.TenantID, body.Permissions); herr != nil {
		herr.write(w)
		return
	}
	expiresAt, herr := fakeKeyExpiry(body.Scope, body.ExpiresAt)
	if herr != nil {
		herr.write(w)
		return
	}
	h.mu.Lock()
	h.nextID++
	id := fmt.Sprintf("KEY%09d", h.nextID)
	k := &fakeAPIKey{
		KeyID: id, Name: body.Name, Scope: body.Scope, TenantID: body.TenantID, Permissions: body.Permissions,
		DisplayPrefix: "ak_" + id[:5], CreatedAt: nowISO(), ExpiresAt: expiresAt,
	}
	h.apiKeys[id] = k
	created := *k
	h.mu.Unlock()
	writeData(w, http.StatusCreated, map[string]any{"key": created, "plaintext": "ak_" + id + ".plaintextsecret"})
}

// apiKeyFor finds a key the caller may act on; licensing keys act on
// organization keys only. Caller holds mu.
func (h *fakeHub) apiKeyFor(r *http.Request) (*fakeAPIKey, *fakeHTTPError) {
	k, ok := h.apiKeys[r.PathValue("id")]
	if !ok {
		return nil, &fakeHTTPError{status: http.StatusNotFound, code: "api_key_not_found", message: "API key not found"}
	}
	if callerOf(r).licensingOnly() && k.Scope != "organization" {
		return nil, &fakeHTTPError{status: http.StatusForbidden, code: "forbidden_key_scope", message: "API key scope does not cover this request"}
	}
	return k, nil
}

// revokeAPIKey mirrors POST /api-keys/:id/revoke: idempotent, the first
// revocation time stays.
func (h *fakeHub) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	k, herr := h.apiKeyFor(r)
	if herr != nil {
		herr.write(w)
		return
	}
	if k.RevokedAt == nil {
		at := nowISO()
		k.RevokedAt = &at
	}
	writeData(w, http.StatusOK, map[string]any{"ok": true})
}

// deleteAPIKey mirrors DELETE /api-keys/:id (only revoked keys; the Hub's
// 30-day cooldown is not modelled).
func (h *fakeHub) deleteAPIKey(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	k, herr := h.apiKeyFor(r)
	if herr != nil {
		herr.write(w)
		return
	}
	if k.RevokedAt == nil {
		badRequest("must_revoke_first").write(w)
		return
	}
	delete(h.apiKeys, k.KeyID)
	writeData(w, http.StatusOK, map[string]any{"ok": true, "deleted": k.KeyID})
}
