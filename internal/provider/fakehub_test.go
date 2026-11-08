package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

// testAdminKey is the bearer the fake Hub accepts. Shaped like a real
// system key so redaction tests can look for it.
const testAdminKey = "ak_testkey0001.c2VjcmV0LXNlY3JldC1zZWNyZXQ"

// fakeHub is an in-memory stand-in for the Hub admin API (innfactoryHUB on
// innfactory.cloud and the innFactory AI Hub on innfactory.ai share the
// same paths and envelope). It implements just enough of the contract for
// the provider: tenant lookup, API keys, and environments.
//
// Every handler runs on httptest's goroutines; mu guards all state.
type fakeHub struct {
	srv *httptest.Server

	mu           sync.Mutex
	tenants      []fakeTenant
	environments map[string]*fakeEnvironment
	apiKeys      map[string]*fakeAPIKey
	licenses     map[string]*fakeLicense
	licenseKeys  []*fakeLicenseKey
	positions    map[string]*fakePosition
	calls        []string // "METHOD /path" in arrival order
	audits       []string // X-Audit-Context per call, "" when absent
	bodies       map[string][]map[string]any
	nextID       int
	// mintSystemKeys lets API keys create system keys, as innfactoryHUB
	// does; the AI Hub refuses (403 forbidden_key_minting).
	mintSystemKeys bool
	// errorBody, when set, is returned with errorStatus for every request
	// whose method matches errorMethod ("" = every method) and whose path
	// ends with errorPath ("" = every path).
	errorMethod string
	errorPath   string
	errorStatus int
	errorBody   string
	// errorTimes limits the injected failure to that many requests (0 =
	// until cleared).
	errorTimes int
	// dropPath: the next dropTimes POSTs whose path ends with dropPath are
	// processed, but the connection is closed instead of answering — a
	// response lost in transit.
	dropPath  string
	dropTimes int
	// idempotency maps an Idempotency-Key to what its first request
	// created ("environment:<id>" or "license:<id>").
	idempotency map[string]string
	// idempotencyKeys lists the Idempotency-Key of every call ("" = none).
	idempotencyKeys []string
}

type fakeTenant struct {
	TenantID    string              `json:"tenant_id"`
	LegalName   string              `json:"legal_name"`
	Identifiers map[string][]string `json:"identifiers"`
}

type fakeIdentifier struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type fakeEnvironment struct {
	EnvironmentID  string          `json:"environment_id"`
	TenantID       string          `json:"tenant_id"`
	Name           string          `json:"name"`
	Domain         string          `json:"domain"`
	Cloud          string          `json:"cloud"`
	IdentifierUsed *fakeIdentifier `json:"identifier_used"`
	CreatedBy      string          `json:"created_by"`
	CreatedAt      string          `json:"created_at"`
	UpdatedAt      string          `json:"updated_at"`
}

// Seeded customers. tnt_alpha is reachable via every lookup kind;
// shared-proj belongs to two tenants and must be rejected as ambiguous.
const (
	alphaTenantID      = "tnt_alpha"
	alphaAzureTenantID = "0f5c9c58-1c1e-4c51-9f0e-7c1f2a3b4c5d"
	alphaSubscription  = "a1b2c3d4-0000-4000-8000-000000000001"
	alphaGcpProject    = "alpha-prod"
	alphaAwsAccount    = "123456789012"
	alphaTenantDomain  = "alpha-demo.onmicrosoft.com"
	betaTenantID       = "tnt_beta"
	sharedGcpProject   = "shared-proj"
)

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	h := &fakeHub{
		tenants: []fakeTenant{
			{TenantID: alphaTenantID, LegalName: "Alpha Demo GmbH", Identifiers: map[string][]string{
				"gcp_project_ids":        {alphaGcpProject},
				"azure_subscription_ids": {alphaSubscription},
				"azure_tenant_ids":       {alphaAzureTenantID},
				"azure_tenant_domains":   {alphaTenantDomain},
				"aws_account_ids":        {alphaAwsAccount},
			}},
			{TenantID: betaTenantID, LegalName: "Beta GmbH", Identifiers: map[string][]string{
				"gcp_project_ids": {sharedGcpProject},
			}},
			{TenantID: "tnt_gamma", LegalName: "Gamma GmbH", Identifiers: map[string][]string{
				"gcp_project_ids": {sharedGcpProject},
			}},
		},
		environments: map[string]*fakeEnvironment{},
		apiKeys:      map[string]*fakeAPIKey{},
		licenses:     map[string]*fakeLicense{},
		positions:    seededPositions(),
		bodies:       map[string][]map[string]any{},
		idempotency:  map[string]string{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/admin/tenants/lookup", h.lookupTenant)
	mux.HandleFunc("GET /api/v1/admin/api-keys", h.listAPIKeys)
	mux.HandleFunc("POST /api/v1/admin/api-keys", h.createAPIKey)
	mux.HandleFunc("POST /api/v1/admin/api-keys/{id}/revoke", h.revokeAPIKey)
	mux.HandleFunc("DELETE /api/v1/admin/api-keys/{id}", h.deleteAPIKey)
	mux.HandleFunc("GET /api/v1/admin/environments", h.listEnvironments)
	mux.HandleFunc("POST /api/v1/admin/environments", h.createEnvironment)
	mux.HandleFunc("GET /api/v1/admin/environments/{id}", h.getEnvironment)
	mux.HandleFunc("PATCH /api/v1/admin/environments/{id}", h.patchEnvironment)
	mux.HandleFunc("DELETE /api/v1/admin/environments/{id}", h.deleteEnvironment)
	mux.HandleFunc("GET /api/v1/admin/licenses", h.listLicenses)
	mux.HandleFunc("POST /api/v1/admin/licenses", h.createLicense)
	mux.HandleFunc("GET /api/v1/admin/licenses/{id}", h.getLicense)
	mux.HandleFunc("PATCH /api/v1/admin/licenses/{id}", h.patchLicense)
	mux.HandleFunc("DELETE /api/v1/admin/licenses/{id}", h.endLicense)
	mux.HandleFunc("POST /api/v1/admin/licenses/{id}/keys/rotate", h.rotateLicenseKey)

	h.srv = httptest.NewServer(h.middleware(mux))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *fakeHub) URL() string { return h.srv.URL }

func (h *fakeHub) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := r.Method + " " + r.URL.Path
		// Keep a decoded copy for assertions and hand the raw bytes on.
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var body map[string]any
		decoded := json.Unmarshal(raw, &body) == nil

		audit := r.Header.Get("X-Audit-Context")
		h.mu.Lock()
		h.calls = append(h.calls, call)
		h.audits = append(h.audits, audit)
		h.idempotencyKeys = append(h.idempotencyKeys, r.Header.Get("Idempotency-Key"))
		if decoded {
			h.bodies[call] = append(h.bodies[call], body)
		}
		status, errBody := h.errorStatus, h.errorBody
		failThis := (h.errorMethod == "" || h.errorMethod == r.Method) && strings.HasSuffix(r.URL.Path, h.errorPath)
		if status != 0 && failThis && h.errorTimes > 0 {
			if h.errorTimes--; h.errorTimes == 0 {
				h.errorStatus = 0
			}
		}
		drop := r.Method == http.MethodPost && h.dropTimes > 0 && strings.HasSuffix(r.URL.Path, h.dropPath)
		if drop {
			h.dropTimes--
		}
		h.mu.Unlock()

		if status != 0 && failThis {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(errBody))
			return
		}
		caller, ok := fakeCallers[r.Header.Get("Authorization")]
		if !ok {
			writeError(w, http.StatusUnauthorized, "invalid_api_key", "API key invalid")
			return
		}
		if herr := caller.gate(r.Method, r.URL.Path); herr != nil {
			herr.write(w)
			return
		}
		if !fakeAuditContext.MatchString(audit) {
			writeError(w, http.StatusBadRequest, "bad_request", "X-Audit-Context: at most 256 printable ASCII characters")
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), callerKey{}, caller))
		if drop {
			next.ServeHTTP(httptest.NewRecorder(), r)
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

// fakeAuditContext is the Hub's rule for X-Audit-Context: at most 256
// printable ASCII characters, no line breaks.
var fakeAuditContext = regexp.MustCompile(`^[\x20-\x7e]{0,256}$`)

// --- inspection helpers (all lock-protected) ---

// auditContexts lists the X-Audit-Context of every call in arrival order.
func (h *fakeHub) auditContexts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.audits...)
}

// writeCalls lists every non-GET request the provider sent.
func (h *fakeHub) writeCalls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, c := range h.calls {
		if !strings.HasPrefix(c, "GET ") {
			out = append(out, c)
		}
	}
	return out
}

func (h *fakeHub) allCalls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

func (h *fakeHub) callCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

func (h *fakeHub) resetCalls() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls, h.audits, h.idempotencyKeys = nil, nil, nil
}

func (h *fakeHub) lastBody(methodPath string) map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.bodies[methodPath]
	if len(b) == 0 {
		return nil
	}
	return b[len(b)-1]
}

func (h *fakeHub) environment(id string) (fakeEnvironment, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.environments[id]
	if !ok {
		return fakeEnvironment{}, false
	}
	return *e, true
}

func (h *fakeHub) environmentIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	ids := make([]string, 0, len(h.environments))
	for id := range h.environments {
		ids = append(ids, id)
	}
	return ids
}

// seedEnvironment registers an environment out-of-band (e.g. created by an
// admin in the portal) so tests can import it.
func (h *fakeHub) seedEnvironment(e fakeEnvironment) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cp := e
	h.environments[e.EnvironmentID] = &cp
}

// removeEnvironment deletes out-of-band to simulate drift.
func (h *fakeHub) removeEnvironment(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.environments, id)
}

// failAll makes every subsequent request answer with status + raw body.
func (h *fakeHub) failAll(status int, raw string) { h.failMethod("", status, raw) }

// failTimes fails the next n requests with the given method and path suffix.
func (h *fakeHub) failTimes(n int, method, suffix string, status int, raw string) {
	h.failPath(method, suffix, status, raw)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.errorTimes = n
}

// dropResponses processes the next n POSTs to a path ending with suffix but
// closes the connection instead of answering.
func (h *fakeHub) dropResponses(n int, suffix string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropPath, h.dropTimes = suffix, n
}

// idempotencyKeysOf lists the Idempotency-Key of every call to methodPath.
func (h *fakeHub) idempotencyKeysOf(methodPath string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for i, c := range h.calls {
		if c == methodPath {
			out = append(out, h.idempotencyKeys[i])
		}
	}
	return out
}

// failMethod fails only requests with the given method; status 0 clears it.
func (h *fakeHub) failMethod(method string, status int, raw string) {
	h.failPath(method, "", status, raw)
}

// failPath fails only requests with the given method whose path ends with
// suffix; status 0 clears it.
func (h *fakeHub) failPath(method, suffix string, status int, raw string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.errorMethod, h.errorPath, h.errorStatus, h.errorBody, h.errorTimes = method, suffix, status, raw, 0
}

// --- handlers (callers do NOT hold mu) ---

var lookupFields = map[string]string{
	"gcp_project_id":        "gcp_project_ids",
	"azure_subscription_id": "azure_subscription_ids",
	"azure_tenant_id":       "azure_tenant_ids",
	"azure_tenant_domain":   "azure_tenant_domains",
	"aws_account_id":        "aws_account_ids",
}

// resolveTenant mirrors GET /tenants/lookup semantics: tenant_id is a direct
// fetch; every other key scans the identifier arrays. Caller holds mu.
func (h *fakeHub) resolveTenant(kind, value string) (fakeTenant, int, string) {
	if kind == "tenant_id" {
		for _, t := range h.tenants {
			if t.TenantID == value {
				return t, 0, ""
			}
		}
		return fakeTenant{}, http.StatusNotFound, "tenant_not_found"
	}
	field, ok := lookupFields[kind]
	if !ok {
		return fakeTenant{}, http.StatusBadRequest, "bad_request"
	}
	var hits []fakeTenant
	for _, t := range h.tenants {
		for _, v := range t.Identifiers[field] {
			if v == value {
				hits = append(hits, t)
			}
		}
	}
	switch len(hits) {
	case 0:
		return fakeTenant{}, http.StatusNotFound, "tenant_not_found"
	case 1:
		return hits[0], 0, ""
	default:
		return fakeTenant{}, http.StatusConflict, "ambiguous_lookup"
	}
}

func (h *fakeHub) lookupTenant(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var kind, value string
	n := 0
	for _, k := range []string{"tenant_id", "gcp_project_id", "azure_subscription_id", "azure_tenant_id", "azure_tenant_domain", "aws_account_id"} {
		if v := q.Get(k); v != "" {
			kind, value = k, v
			n++
		}
	}
	if n != 1 {
		writeError(w, http.StatusBadRequest, "bad_request", "exactly one identifier is required")
		return
	}
	h.mu.Lock()
	t, status, code := h.resolveTenant(kind, value)
	h.mu.Unlock()
	if status != 0 {
		writeError(w, status, code, "lookup failed")
		return
	}
	ids := map[string][]string{}
	for _, f := range []string{"gcp_project_ids", "azure_subscription_ids", "azure_tenant_ids", "azure_tenant_domains", "aws_account_ids"} {
		ids[f] = append([]string{}, t.Identifiers[f]...)
	}
	writeData(w, http.StatusOK, map[string]any{"tenant": map[string]any{
		"tenant_id": t.TenantID, "legal_name": t.LegalName, "identifiers": ids,
	}})
}

func (h *fakeHub) listEnvironments(w http.ResponseWriter, _ *http.Request) {
	h.mu.Lock()
	envs := make([]fakeEnvironment, 0, len(h.environments))
	for _, e := range h.environments {
		envs = append(envs, *e)
	}
	h.mu.Unlock()
	writeData(w, http.StatusOK, map[string]any{"environments": envs})
}

var validClouds = map[string]bool{"azure": true, "gcp": true, "aws": true, "stackit": true, "other": true}

// createEnvironment implements POST /environments:
// body {lookup: {<one identifier>} | tenant_id, name, domain, cloud}.
func (h *fakeHub) createEnvironment(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Lookup   map[string]string `json:"lookup"`
		TenantID *string           `json:"tenant_id"`
		Name     string            `json:"name"`
		Domain   string            `json:"domain"`
		Cloud    string            `json:"cloud"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if (body.TenantID == nil) == (len(body.Lookup) == 0) || len(body.Lookup) > 1 {
		writeError(w, http.StatusBadRequest, "bad_request", "exactly one of lookup (with one identifier) or tenant_id is required")
		return
	}
	if body.Name == "" || body.Domain == "" || !validClouds[body.Cloud] {
		writeError(w, http.StatusBadRequest, "bad_request", "name, domain and a valid cloud are required")
		return
	}
	used := fakeIdentifier{Type: "tenant_id"}
	if body.TenantID != nil {
		used.Value = *body.TenantID
	}
	for k, v := range body.Lookup {
		used = fakeIdentifier{Type: k, Value: v}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	idemKey, herr := fakeIdempotencyKey(r)
	if herr != nil {
		herr.write(w)
		return
	}
	if prior, ok := h.idempotency[idemKey]; ok && idemKey != "" {
		// Replay: the same environment, nothing new.
		writeData(w, http.StatusOK, map[string]any{"environment": *h.environments[strings.TrimPrefix(prior, "environment:")]})
		return
	}
	t, status, code := h.resolveTenant(used.Type, used.Value)
	if status != 0 {
		writeError(w, status, code, "no unique customer for "+used.Type)
		return
	}
	for _, e := range h.environments {
		if e.TenantID == t.TenantID && e.Domain == body.Domain {
			writeError(w, http.StatusConflict, "environment_exists", "environment "+e.EnvironmentID+" already uses this domain")
			return
		}
	}
	h.nextID++
	now := nowISO()
	e := &fakeEnvironment{
		EnvironmentID: fmt.Sprintf("env_%06d", h.nextID), TenantID: t.TenantID,
		Name: body.Name, Domain: body.Domain, Cloud: body.Cloud, IdentifierUsed: &used,
		CreatedBy: "terraform", CreatedAt: now, UpdatedAt: now,
	}
	h.environments[e.EnvironmentID] = e
	if idemKey != "" {
		h.idempotency[idemKey] = "environment:" + e.EnvironmentID
	}
	writeData(w, http.StatusCreated, map[string]any{"environment": *e})
}

func (h *fakeHub) getEnvironment(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	e, ok := h.environments[r.PathValue("id")]
	var cp fakeEnvironment
	if ok {
		cp = *e
	}
	h.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "environment_not_found", "environment not found")
		return
	}
	writeData(w, http.StatusOK, map[string]any{"environment": cp})
}

// patchEnvironment accepts only the mutable fields name and domain.
func (h *fakeHub) patchEnvironment(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	for k := range body {
		if k != "name" && k != "domain" {
			writeError(w, http.StatusBadRequest, "bad_request", "field "+k+" is immutable")
			return
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.environments[r.PathValue("id")]
	if !ok {
		writeError(w, http.StatusNotFound, "environment_not_found", "environment not found")
		return
	}
	if v, ok := body["name"].(string); ok {
		e.Name = v
	}
	if v, ok := body["domain"].(string); ok {
		e.Domain = v
	}
	e.UpdatedAt = nowISO()
	writeData(w, http.StatusOK, map[string]any{"environment": *e})
}

// deleteEnvironment deletes hard, but only without licences that are not
// ended (409 environment_in_use, details.license_ids) — as the Hub does.
func (h *fakeHub) deleteEnvironment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.environments[id]; !ok {
		writeError(w, http.StatusNotFound, "environment_not_found", "environment not found")
		return
	}
	var inUse []string
	for _, l := range h.licenses {
		if l.EnvironmentID == id && l.Status != "ended" {
			inUse = append(inUse, l.LicenseID)
		}
	}
	if len(inUse) > 0 {
		(&fakeHTTPError{status: http.StatusConflict, code: "environment_in_use", message: "Umgebung hat laufende Lizenzen",
			details: map[string]any{"license_ids": inUse}}).write(w)
		return
	}
	delete(h.environments, id)
	writeData(w, http.StatusOK, map[string]any{"ok": true})
}

// fakeUUID is the Hub's check of Idempotency-Key (a UUID, RFC 9562 variant).
var fakeUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// fakeIdempotencyKey reads the optional Idempotency-Key ("" = none).
func fakeIdempotencyKey(r *http.Request) (string, *fakeHTTPError) {
	k := r.Header.Get("Idempotency-Key")
	if k != "" && !fakeUUID.MatchString(k) {
		return "", badRequest("Idempotency-Key must be a UUID")
	}
	return k, nil
}

// --- wire helpers ---

func writeData(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data": data,
		"meta": map[string]any{"request_id": "req_test", "generated_at": nowISO(), "api_version": "v1"},
	})
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": message}})
}

func nowISO() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// --- provider wiring for terraform-plugin-testing ---

func testProviderFactories() map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"innfactoryhub": providerserver.NewProtocol6WithError(New("test")()),
	}
}

// useFakeHubEnv permits the plain-http loopback endpoint of the fake Hub and
// clears any admin key exported in the developer's shell. Uses t.Setenv, so
// callers must not run in parallel.
func useFakeHubEnv(t *testing.T) {
	t.Helper()
	t.Setenv("INNFACTORYHUB_INSECURE_ENDPOINT", "1")
	t.Setenv("INNFACTORYHUB_ADMIN_KEY", "")
	t.Setenv("INNFACTORYHUB_ENDPOINT", "")
	t.Setenv("INNFACTORYHUB_AUDIT_CONTEXT", "")
}

// providerBlock renders a provider configuration against the fake Hub.
func (h *fakeHub) providerBlock() string {
	return fmt.Sprintf(`
provider "innfactoryhub" {
  endpoint  = %q
  admin_key = %q
}
`, h.URL(), testAdminKey)
}

// providerBlockWithKey renders a provider configuration with another key.
func (h *fakeHub) providerBlockWithKey(key string) string {
	return fmt.Sprintf(`
provider "innfactoryhub" {
  endpoint  = %q
  admin_key = %q
}
`, h.URL(), key)
}

// providerBlockWithoutKey points at the fake Hub but configures no key.
func (h *fakeHub) providerBlockWithoutKey() string {
	return fmt.Sprintf(`
provider "innfactoryhub" {
  endpoint = %q
}
`, h.URL())
}
