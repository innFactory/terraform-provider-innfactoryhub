package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// Licence side of the fake Hub, modelled on the AI Hub's licenseApi
// (`/api/v1/admin/licenses`, OpenAPI components License, LicenseCreateBody,
// LicensePatchBody, LicenseKeyInfo). Semantics mirrored from the Hub:
//   - exactly one of contract_position_id or unbilled (400 billing_link_required)
//   - domains trimmed, lower-cased, de-duplicated; FQDN or *.FQDN (422 invalid_domain)
//   - features default to all features of the edition and come back in edition order
//   - PATCH merges only the given params; no change means no new revision;
//     the same position or the same reason without max_until is a no-op
//   - DELETE ends the licence (status ended) and frees the position; repeatable
//   - the plaintext key exists only in the create and rotation responses
//   - POST …/keys/rotate issues a new key and revokes every earlier one, also
//     for suspended or expired licences, never for ended ones (409 license_ended)
//   - POST with an Idempotency-Key seen before (24 h): 409 idempotent_replay
//     with details.license_id, and never the key again
//   - params.air_gapped defaults to false (also for licences stored before it
//     existed). Every caller here is an API key, and keys never switch it on:
//     create with true or PATCH false -> true is 403 air_gapped_staff_only,
//     nothing written, checked before anything else on create and before the
//     product check on PATCH; an unchanged true and switching it off are
//     fine. Staff switch it on in the portal (setAirGappedOutOfBand,
//     seedLicense). true only for ai-gateway (422 air_gapped_unsupported with
//     details.product)

// Seeded fixtures for licence tests.
const (
	alphaEnvID        = "env_alpha_gw"
	alphaStagingEnvID = "env_alpha_stg"
	betaEnvID         = "env_beta_gw"
	alphaPositionID   = "pos_alpha_gw0001"
	alphaPosition2ID  = "pos_alpha_gw0002"
	alphaPricesPosID  = "pos_alpha_pr0001"
	betaPositionID    = "pos_beta_gw00001"
	positionEndDate   = "2027-12-31"
	position2EndDate  = "2028-06-30"
)

type fakeLicenseParams struct {
	Domains      []string `json:"domains"`
	MaxUsers     *int64   `json:"max_users"`
	MaxInstances int64    `json:"max_instances"`
	EditionID    string   `json:"edition_id"`
	Features     []string `json:"features"`
	Entitlements []string `json:"entitlements"`
	AirGapped    bool     `json:"air_gapped"`
}

type fakeUnbilled struct {
	Reason   string `json:"reason"`
	MaxUntil string `json:"max_until"`
}

type fakeLicense struct {
	LicenseID          string            `json:"license_id"`
	TenantID           string            `json:"tenant_id"`
	EnvironmentID      string            `json:"environment_id"`
	Product            string            `json:"product"`
	ContractPositionID *string           `json:"contract_position_id"`
	ContractID         *string           `json:"contract_id"`
	Unbilled           *fakeUnbilled     `json:"unbilled"`
	Params             fakeLicenseParams `json:"params"`
	ValidFrom          string            `json:"valid_from"`
	ValidUntil         string            `json:"valid_until"`
	AutoRenew          bool              `json:"auto_renew"`
	Status             string            `json:"status"`
	Revision           int               `json:"revision"`
	CreatedBySource    string            `json:"created_by_source"`
	CreatedAt          string            `json:"created_at"`
	CreatedBy          string            `json:"created_by"`
	UpdatedAt          string            `json:"updated_at"`
	UpdatedBy          string            `json:"updated_by"`
	EndedAt            *string           `json:"ended_at"`
	EndedBy            *string           `json:"ended_by"`
}

// fakeLicenseKey is a LicenseKeyInfo plus what only the fake knows.
type fakeLicenseKey struct {
	KeyID         string  `json:"key_id"`
	DisplayPrefix string  `json:"display_prefix"`
	CreatedAt     string  `json:"created_at"`
	CreatedBy     string  `json:"created_by"`
	RevokedAt     *string `json:"revoked_at"`
	LastUsedAt    *string `json:"last_used_at"`
	licenseID     string
	plaintext     string
}

type fakePosition struct {
	TenantID       string
	ContractID     string
	LicenseProduct string
	EndDate        string
	LicenseID      string // "" = free
}

type fakeEdition struct {
	ID       string
	Features []string
}

// fakeEditions is the catalog: licence product → editions with their
// features in edition order.
var fakeEditions = map[string][]fakeEdition{
	"ai-gateway": {
		{ID: "all", Features: []string{"mcp", "flows", "agents", "companion", "guardrails", "insights"}},
		{ID: "basic", Features: []string{"mcp", "flows"}},
	},
	"ai-prices":              {{ID: "all", Features: []string{"prices"}}},
	"companygpt-maintenance": {{ID: "all", Features: []string{}}},
}

var allGatewayFeatures = fakeEditions["ai-gateway"][0].Features

func seededPositions() map[string]*fakePosition {
	return map[string]*fakePosition{
		alphaPositionID:  {TenantID: alphaTenantID, ContractID: "ctr_alpha00001", LicenseProduct: "ai-gateway", EndDate: positionEndDate},
		alphaPosition2ID: {TenantID: alphaTenantID, ContractID: "ctr_alpha00001", LicenseProduct: "ai-gateway", EndDate: position2EndDate},
		alphaPricesPosID: {TenantID: alphaTenantID, ContractID: "ctr_alpha00001", LicenseProduct: "ai-prices", EndDate: positionEndDate},
		betaPositionID:   {TenantID: betaTenantID, ContractID: "ctr_beta000001", LicenseProduct: "ai-gateway", EndDate: positionEndDate},
	}
}

// newLicenseHub is a fake Hub with the environments licence tests bind to.
func newLicenseHub(t *testing.T) *fakeHub {
	t.Helper()
	h := newFakeHub(t)
	for _, e := range []fakeEnvironment{
		{EnvironmentID: alphaEnvID, TenantID: alphaTenantID, Name: "Alpha Gateway", Domain: "gpt.alpha.example", Cloud: "azure"},
		{EnvironmentID: alphaStagingEnvID, TenantID: alphaTenantID, Name: "Alpha Staging", Domain: "stg.alpha.example", Cloud: "azure"},
		{EnvironmentID: betaEnvID, TenantID: betaTenantID, Name: "Beta Gateway", Domain: "gpt.beta.example", Cloud: "gcp"},
	} {
		e.CreatedBy, e.CreatedAt, e.UpdatedAt = "admin", nowISO(), nowISO()
		h.seedEnvironment(e)
	}
	return h
}

// --- inspection helpers (all lock-protected) ---

func (h *fakeHub) license(id string) (fakeLicense, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l, ok := h.licenses[id]
	if !ok {
		return fakeLicense{}, false
	}
	return *l, true
}

// licenseIDsByStatus lists licence ids whose status matches ("" = all).
func (h *fakeHub) licenseIDsByStatus(status string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var ids []string
	for id, l := range h.licenses {
		if status == "" || l.Status == status {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// issuedKeys returns the plaintext of every key the Hub ever minted for
// the licence, oldest first.
func (h *fakeHub) issuedKeys(licenseID string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, k := range h.licenseKeys {
		if k.licenseID == licenseID {
			out = append(out, k.plaintext)
		}
	}
	return out
}

func (h *fakeHub) positionLicense(positionID string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.positions[positionID].LicenseID
}

// seedLicense stores a licence created outside Terraform (e.g. in the
// portal) without a key, so tests can import it.
func (h *fakeHub) seedLicense(l fakeLicense) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cp := l
	h.licenses[l.LicenseID] = &cp
	if l.ContractPositionID != nil {
		h.positions[*l.ContractPositionID].LicenseID = l.LicenseID
	}
}

// forgetLicense deletes a licence out of band (the Hub itself never does)
// so tests can provoke 404 on read and delete.
func (h *fakeHub) forgetLicense(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if l, ok := h.licenses[id]; ok && l.ContractPositionID != nil {
		h.positions[*l.ContractPositionID].LicenseID = ""
	}
	delete(h.licenses, id)
}

// endLicenseOutOfBand ends a licence as an admin in the portal would.
func (h *fakeHub) endLicenseOutOfBand(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.endLicenseLocked(h.licenses[id], "admin")
}

// tamperReplay makes every idempotent replay name licenceID instead of the
// licence the key created — a broken or hostile Hub.
func (h *fakeHub) tamperReplay(licenseID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.replayAs = licenseID
}

// suspendLicenseOutOfBand suspends a licence as an admin in the portal would.
func (h *fakeHub) suspendLicenseOutOfBand(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.licenses[id].Status = "suspended"
}

// rotateKeyOutOfBand rotates the licence key as an admin or the customer in
// the portal would; the new key never reaches Terraform.
func (h *fakeHub) rotateKeyOutOfBand(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rotateKeyLocked(id)
}

// setAirGappedOutOfBand switches air-gap operation as an admin in the
// portal would (e.g. to download a licence file).
func (h *fakeHub) setAirGappedOutOfBand(id string, on bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l := h.licenses[id]
	l.Params.AirGapped = on
	l.Revision++
}

// activeKeyIDs lists the unrevoked key ids of a licence.
func (h *fakeHub) activeKeyIDs(licenseID string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, k := range h.licenseKeys {
		if k.licenseID == licenseID && k.RevokedAt == nil {
			out = append(out, k.KeyID)
		}
	}
	return out
}

// --- handlers ---

// fakeParamsInput keeps "absent" apart from "null" for max_users.
type fakeParamsInput struct {
	Domains      []string        `json:"domains"`
	MaxUsers     json.RawMessage `json:"max_users"`
	MaxInstances *int64          `json:"max_instances"`
	EditionID    *string         `json:"edition_id"`
	Features     []string        `json:"features"`
	Entitlements []string        `json:"entitlements"`
	AirGapped    *bool           `json:"air_gapped"`
}

type fakeUnbilledInput struct {
	Reason   string  `json:"reason"`
	MaxUntil *string `json:"max_until"`
}

type fakeLicenseBody struct {
	EnvironmentID      string             `json:"environment_id"`
	Product            string             `json:"product"`
	Params             *fakeParamsInput   `json:"params"`
	ContractPositionID *string            `json:"contract_position_id"`
	Unbilled           *fakeUnbilledInput `json:"unbilled"`
}

type fakeHTTPError struct {
	status  int
	code    string
	message string
	details any
}

func (e *fakeHTTPError) write(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.status)
	body := map[string]any{"code": e.code, "message": e.message}
	if e.details != nil {
		body["details"] = e.details
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"error": body})
}

func badRequest(msg string) *fakeHTTPError {
	return &fakeHTTPError{status: http.StatusBadRequest, code: "bad_request", message: msg}
}

// decodeStrict rejects unknown fields like the Hub's strict zod schemas, so a
// misspelt attribute in the provider fails loudly.
func decodeStrict(r *http.Request, dst any) *fakeHTTPError {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return badRequest(err.Error())
	}
	return nil
}

var (
	fakeLabel     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	fakeNameToken = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)
	fakeRefID     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	fakeNumeric   = regexp.MustCompile(`^\d+$`)
)

func fakeIsHostName(v string) bool {
	labels := strings.Split(v, ".")
	if len(v) > 253 || len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if !fakeLabel.MatchString(l) {
			return false
		}
	}
	return !fakeNumeric.MatchString(labels[len(labels)-1])
}

func fakeLicenseDomains(in []string) ([]string, *fakeHTTPError) {
	if len(in) < 1 || len(in) > 50 {
		return nil, badRequest("domains: 1 to 50 entries")
	}
	var out, invalid []string
	for _, d := range in {
		d = strings.ToLower(strings.TrimSpace(d))
		if slices.Contains(out, d) {
			continue
		}
		out = append(out, d)
		if !fakeIsHostName(strings.TrimPrefix(d, "*.")) {
			invalid = append(invalid, d)
		}
	}
	if len(invalid) > 0 {
		return nil, &fakeHTTPError{status: http.StatusUnprocessableEntity, code: "invalid_domain",
			message: "Domains: FQDN (gpt.kunde.de) oder *.FQDN (*.staging.kunde.de)", details: map[string]any{"domains": invalid}}
	}
	return out, nil
}

func fakeTokens(in []string, limit int) ([]string, *fakeHTTPError) {
	if len(in) > limit {
		return nil, badRequest("too many entries")
	}
	var out []string
	for _, v := range in {
		v = strings.ToLower(strings.TrimSpace(v))
		if !fakeNameToken.MatchString(v) {
			return nil, badRequest("invalid token " + v)
		}
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out, nil
}

func fakeMaxUsers(raw json.RawMessage) (*int64, *fakeHTTPError) {
	if string(raw) == "null" {
		return nil, nil
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil || n < 1 || n > 1_000_000 {
		return nil, badRequest("max_users: integer 1..1000000 or null")
	}
	return &n, nil
}

// fakeResolveFeatures mirrors resolveFeatures in the Hub's licensing package.
func fakeResolveFeatures(product, editionID string, requested []string) ([]string, *fakeHTTPError) {
	var available []string
	for _, e := range fakeEditions[product] {
		available = append(available, e.ID)
		if e.ID != editionID {
			continue
		}
		if requested == nil {
			return append([]string{}, e.Features...), nil
		}
		var outside []string
		for _, f := range requested {
			if !slices.Contains(e.Features, f) {
				outside = append(outside, f)
			}
		}
		if len(outside) > 0 {
			return nil, &fakeHTTPError{status: http.StatusUnprocessableEntity, code: "feature_not_in_edition",
				message: "Funktionen außerhalb der Edition", details: map[string]any{"features": outside}}
		}
		out := []string{}
		for _, f := range e.Features {
			if slices.Contains(requested, f) {
				out = append(out, f)
			}
		}
		return out, nil
	}
	return nil, &fakeHTTPError{status: http.StatusUnprocessableEntity, code: "edition_unknown",
		message: fmt.Sprintf("Edition %s gibt es für dieses Produkt nicht", editionID), details: map[string]any{"available": available}}
}

func fakeToday() time.Time { return time.Now().UTC().Truncate(24 * time.Hour) }

const isoDay = "2006-01-02"

// fakeUnbilledLink applies the default and the limit per reason: trial 30
// (at most 60) days, partner/internal 12 months.
func fakeUnbilledLink(in fakeUnbilledInput) (*fakeUnbilled, *fakeHTTPError) {
	today := fakeToday()
	var def, limit time.Time
	switch in.Reason {
	case "trial":
		def, limit = today.AddDate(0, 0, 30), today.AddDate(0, 0, 60)
	case "partner", "internal":
		def, limit = today.AddDate(0, 12, 0), today.AddDate(0, 12, 0)
	default:
		return nil, badRequest("unbilled.reason: trial | partner | internal")
	}
	if in.MaxUntil == nil {
		return &fakeUnbilled{Reason: in.Reason, MaxUntil: def.Format(isoDay)}, nil
	}
	at, err := time.Parse(isoDay, *in.MaxUntil)
	switch {
	case err != nil:
		return nil, badRequest("unbilled.max_until: YYYY-MM-DD")
	case at.Before(today):
		return nil, &fakeHTTPError{status: http.StatusBadRequest, code: "unbilled_max_in_past", message: "Die Höchstlaufzeit liegt in der Vergangenheit"}
	case at.After(limit):
		return nil, &fakeHTTPError{status: http.StatusBadRequest, code: "unbilled_max_exceeded",
			message: fmt.Sprintf("Höchstlaufzeit für %s ist %s", in.Reason, limit.Format(isoDay)), details: map[string]any{"max_until_limit": limit.Format(isoDay)}}
	}
	return &fakeUnbilled{Reason: in.Reason, MaxUntil: *in.MaxUntil}, nil
}

// fakeAirGapStaffOnly mirrors assertMayEnableAirGap for an API-key caller:
// switching air-gap operation on (before false, after true) is for staff in
// the portal only.
func fakeAirGapStaffOnly(before, after bool) *fakeHTTPError {
	if after && !before {
		return &fakeHTTPError{status: http.StatusForbidden, code: "air_gapped_staff_only",
			message: "Air-Gap-Betrieb schalten nur Mitarbeiter im Portal ein, nie ein API-Key"}
	}
	return nil
}

// fakeAirGapSupported mirrors assertAirGapSupported: air-gap operation (a
// licence file instead of the online check) exists only for the AI Gateway.
func fakeAirGapSupported(product string, airGapped bool) *fakeHTTPError {
	if airGapped && product != "ai-gateway" {
		return &fakeHTTPError{status: http.StatusUnprocessableEntity, code: "air_gapped_unsupported",
			message: "Air-Gap-Betrieb gibt es nur für AI-Gateway-Lizenzen", details: map[string]any{"product": product}}
	}
	return nil
}

func billingLinkRequired() *fakeHTTPError {
	return &fakeHTTPError{status: http.StatusBadRequest, code: "billing_link_required",
		message: "Genau eins angeben: contract_position_id (abgerechnet) oder unbilled {reason: trial|partner|internal, max_until?}"}
}

// fakeBillingInput enforces "at most one"; it reports whether one is given.
func fakeBillingInput(b fakeLicenseBody) (bool, *fakeHTTPError) {
	if b.ContractPositionID != nil && b.Unbilled != nil {
		return false, billingLinkRequired()
	}
	return b.ContractPositionID != nil || b.Unbilled != nil, nil
}

// applyBillingLocked resolves the billing link onto l. Caller holds mu.
func (h *fakeHub) applyBillingLocked(l *fakeLicense, b fakeLicenseBody) *fakeHTTPError {
	if b.Unbilled != nil {
		u, herr := fakeUnbilledLink(*b.Unbilled)
		if herr != nil {
			return herr
		}
		l.ContractPositionID, l.ContractID, l.Unbilled = nil, nil, u
		l.ValidUntil, l.AutoRenew = u.MaxUntil, false
		return nil
	}
	id := *b.ContractPositionID
	p, ok := h.positions[id]
	if !ok {
		return &fakeHTTPError{status: http.StatusNotFound, code: "position_not_found", message: "Vertragsposition nicht gefunden", details: map[string]any{"contract_position_id": id}}
	}
	if p.TenantID != l.TenantID {
		return &fakeHTTPError{status: http.StatusConflict, code: "position_tenant_mismatch", message: "Die Vertragsposition gehört zu einem anderen Kunden als die Umgebung"}
	}
	if p.LicenseProduct != l.Product {
		return &fakeHTTPError{status: http.StatusConflict, code: "position_product_mismatch", message: "Die Vertragsposition ist kein " + l.Product + "-Produkt"}
	}
	if p.LicenseID != "" && p.LicenseID != l.LicenseID {
		return &fakeHTTPError{status: http.StatusConflict, code: "position_already_linked", message: "Die Vertragsposition ist bereits mit einer Lizenz verknüpft", details: map[string]any{"license_id": p.LicenseID}}
	}
	contract := p.ContractID
	l.ContractPositionID, l.ContractID, l.Unbilled = &id, &contract, nil
	l.ValidUntil, l.AutoRenew = p.EndDate, false
	return nil
}

// linkPositionLocked moves the licence's position link from old to the
// licence's current position. Caller holds mu.
func (h *fakeHub) linkPositionLocked(l *fakeLicense, old *string) {
	if old != nil {
		h.positions[*old].LicenseID = ""
	}
	if l.ContractPositionID != nil {
		h.positions[*l.ContractPositionID].LicenseID = l.LicenseID
	}
}

func (h *fakeHub) createLicense(w http.ResponseWriter, r *http.Request) {
	var body fakeLicenseBody
	if herr := decodeStrict(r, &body); herr != nil {
		herr.write(w)
		return
	}
	if !fakeRefID.MatchString(body.EnvironmentID) || fakeEditions[body.Product] == nil || body.Params == nil {
		badRequest("environment_id, product and params are required").write(w)
		return
	}
	p := body.Params
	if p.MaxUsers == nil || p.EditionID == nil {
		badRequest("params.max_users and params.edition_id are required").write(w)
		return
	}
	airGapped := p.AirGapped != nil && *p.AirGapped
	if herr := fakeAirGapStaffOnly(false, airGapped); herr != nil {
		herr.write(w)
		return
	}
	if ok, herr := fakeBillingInput(body); herr != nil {
		herr.write(w)
		return
	} else if !ok {
		billingLinkRequired().write(w)
		return
	}
	if herr := fakeAirGapSupported(body.Product, airGapped); herr != nil {
		herr.write(w)
		return
	}
	domains, herr := fakeLicenseDomains(p.Domains)
	if herr != nil {
		herr.write(w)
		return
	}
	maxUsers, herr := fakeMaxUsers(p.MaxUsers)
	if herr != nil {
		herr.write(w)
		return
	}
	maxInstances := int64(5)
	if p.MaxInstances != nil {
		maxInstances = *p.MaxInstances
	}
	if maxInstances < 1 || maxInstances > 100 {
		badRequest("max_instances: 1..100").write(w)
		return
	}
	entitlements, herr := fakeTokens(p.Entitlements, 50)
	if herr != nil {
		herr.write(w)
		return
	}
	var requested []string
	if p.Features != nil {
		if requested, herr = fakeTokens(p.Features, 100); herr != nil {
			herr.write(w)
			return
		}
		if requested == nil {
			requested = []string{}
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	idemKey, herr := fakeIdempotencyKey(r)
	if herr != nil {
		herr.write(w)
		return
	}
	if prior, ok := h.idempotency[idemKey]; ok && idemKey != "" {
		replayed := strings.TrimPrefix(prior, "license:")
		if h.replayAs != "" {
			replayed = h.replayAs
		}
		(&fakeHTTPError{status: http.StatusConflict, code: "idempotent_replay", message: "Diese Anlage wurde bereits ausgeführt",
			details: map[string]any{"license_id": replayed}}).write(w)
		return
	}
	env, ok := h.environments[body.EnvironmentID]
	if !ok {
		(&fakeHTTPError{status: http.StatusNotFound, code: "environment_not_found", message: "Umgebung nicht gefunden"}).write(w)
		return
	}
	h.nextID++
	now := nowISO()
	l := &fakeLicense{
		LicenseID: fmt.Sprintf("lic_%012d", h.nextID), TenantID: env.TenantID, EnvironmentID: env.EnvironmentID,
		Product: body.Product, ValidFrom: fakeToday().Format(isoDay), Status: "active", Revision: 1,
		CreatedBySource: "terraform", CreatedAt: now, CreatedBy: "apikey:test", UpdatedAt: now, UpdatedBy: "apikey:test",
	}
	if herr := h.applyBillingLocked(l, body); herr != nil {
		herr.write(w)
		return
	}
	features, herr := fakeResolveFeatures(body.Product, *p.EditionID, requested)
	if herr != nil {
		herr.write(w)
		return
	}
	if entitlements == nil {
		entitlements = []string{}
	}
	l.Params = fakeLicenseParams{Domains: domains, MaxUsers: maxUsers, MaxInstances: maxInstances,
		EditionID: *p.EditionID, Features: features, Entitlements: entitlements, AirGapped: airGapped}
	h.licenses[l.LicenseID] = l
	if idemKey != "" {
		h.idempotency[idemKey] = "license:" + l.LicenseID
	}
	h.linkPositionLocked(l, nil)
	key := h.mintLicenseKeyLocked(l.LicenseID)
	writeData(w, http.StatusCreated, map[string]any{"license": *l, "license_key": key})
}

// mintLicenseKeyLocked issues `lk_<12>.<32>`. Caller holds mu.
func (h *fakeHub) mintLicenseKeyLocked(licenseID string) string {
	n := len(h.licenseKeys) + 1
	id := fmt.Sprintf("K%011d", n)
	plaintext := fmt.Sprintf("lk_%s.%s", id, fmt.Sprintf("s3cr3t%026d", n))
	h.licenseKeys = append(h.licenseKeys, &fakeLicenseKey{
		KeyID: id, DisplayPrefix: plaintext[:8], CreatedAt: nowISO(), CreatedBy: "apikey:test",
		licenseID: licenseID, plaintext: plaintext,
	})
	return plaintext
}

// rotateKeyLocked revokes every key of the licence and issues a new one.
// Caller holds mu.
func (h *fakeHub) rotateKeyLocked(licenseID string) (string, fakeLicenseKey) {
	at := nowISO()
	for _, k := range h.licenseKeys {
		if k.licenseID == licenseID && k.RevokedAt == nil {
			k.RevokedAt = &at
		}
	}
	plaintext := h.mintLicenseKeyLocked(licenseID)
	return plaintext, *h.licenseKeys[len(h.licenseKeys)-1]
}

// rotateLicenseKey mirrors POST /licenses/:id/keys/rotate → {license_key, key}.
func (h *fakeHub) rotateLicenseKey(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l, ok := h.licenses[r.PathValue("id")]
	if !ok {
		(&fakeHTTPError{status: http.StatusNotFound, code: "license_not_found", message: "Lizenz nicht gefunden"}).write(w)
		return
	}
	if l.Status == "ended" {
		(&fakeHTTPError{status: http.StatusConflict, code: "license_ended", message: "Die Lizenz ist beendet"}).write(w)
		return
	}
	plaintext, key := h.rotateKeyLocked(l.LicenseID)
	writeData(w, http.StatusOK, map[string]any{"license_key": plaintext, "key": key})
}

// listLicenses mirrors GET /licenses?tenant_id=&environment_id=&status=.
func (h *fakeHub) listLicenses(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []fakeLicense{}
	for _, l := range h.licenses {
		if (q.Get("tenant_id") == "" || q.Get("tenant_id") == l.TenantID) &&
			(q.Get("environment_id") == "" || q.Get("environment_id") == l.EnvironmentID) &&
			(q.Get("status") == "" || q.Get("status") == l.Status) {
			out = append(out, *l)
		}
	}
	writeData(w, http.StatusOK, map[string]any{"licenses": out, "hints": []any{}})
}

func (h *fakeHub) getLicense(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l, ok := h.licenses[r.PathValue("id")]
	if !ok {
		(&fakeHTTPError{status: http.StatusNotFound, code: "license_not_found", message: "Lizenz nicht gefunden"}).write(w)
		return
	}
	keys := []fakeLicenseKey{}
	for _, k := range h.licenseKeys {
		if k.licenseID == l.LicenseID {
			keys = append(keys, *k)
		}
	}
	writeData(w, http.StatusOK, map[string]any{"license": *l, "keys": keys})
}

// patchLicense mirrors updateLicense: only given params change, the billing
// link changes only when it really differs, no change = no new revision.
func (h *fakeHub) patchLicense(w http.ResponseWriter, r *http.Request) {
	var body fakeLicenseBody
	if herr := decodeStrict(r, &body); herr != nil {
		herr.write(w)
		return
	}
	if body.EnvironmentID != "" || body.Product != "" {
		badRequest("environment_id and product are immutable").write(w)
		return
	}
	hasBilling, herr := fakeBillingInput(body)
	if herr != nil {
		herr.write(w)
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	cur, ok := h.licenses[r.PathValue("id")]
	if !ok {
		(&fakeHTTPError{status: http.StatusNotFound, code: "license_not_found", message: "Lizenz nicht gefunden"}).write(w)
		return
	}
	if cur.Status == "ended" {
		(&fakeHTTPError{status: http.StatusConflict, code: "license_ended", message: "Die Lizenz ist beendet"}).write(w)
		return
	}
	next := *cur
	next.Params.Domains = slices.Clone(cur.Params.Domains)

	if hasBilling && fakeChangesBilling(cur, body) {
		if herr := h.applyBillingLocked(&next, body); herr != nil {
			herr.write(w)
			return
		}
	}
	if p := body.Params; p != nil {
		if herr := fakeMergeParams(&next, p); herr != nil {
			herr.write(w)
			return
		}
	}
	if herr := fakeAirGapStaffOnly(cur.Params.AirGapped, next.Params.AirGapped); herr != nil {
		herr.write(w)
		return
	}
	if herr := fakeAirGapSupported(next.Product, next.Params.AirGapped); herr != nil {
		herr.write(w)
		return
	}
	if reflect.DeepEqual(next, *cur) {
		writeData(w, http.StatusOK, map[string]any{"license": *cur})
		return
	}
	next.Revision++
	next.UpdatedAt = nowISO()
	old := cur.ContractPositionID
	*cur = next
	h.linkPositionLocked(cur, old)
	writeData(w, http.StatusOK, map[string]any{"license": *cur})
}

func fakeChangesBilling(cur *fakeLicense, b fakeLicenseBody) bool {
	if b.ContractPositionID != nil {
		return cur.ContractPositionID == nil || *cur.ContractPositionID != *b.ContractPositionID
	}
	return !(cur.Unbilled != nil && cur.Unbilled.Reason == b.Unbilled.Reason && b.Unbilled.MaxUntil == nil)
}

func fakeMergeParams(l *fakeLicense, p *fakeParamsInput) *fakeHTTPError {
	if p.Domains != nil {
		d, herr := fakeLicenseDomains(p.Domains)
		if herr != nil {
			return herr
		}
		l.Params.Domains = d
	}
	if p.MaxUsers != nil {
		n, herr := fakeMaxUsers(p.MaxUsers)
		if herr != nil {
			return herr
		}
		l.Params.MaxUsers = n
	}
	if p.MaxInstances != nil {
		if *p.MaxInstances < 1 || *p.MaxInstances > 100 {
			return badRequest("max_instances: 1..100")
		}
		l.Params.MaxInstances = *p.MaxInstances
	}
	if p.Entitlements != nil {
		e, herr := fakeTokens(p.Entitlements, 50)
		if herr != nil {
			return herr
		}
		if e == nil {
			e = []string{}
		}
		l.Params.Entitlements = e
	}
	if p.AirGapped != nil {
		l.Params.AirGapped = *p.AirGapped
	}
	editionChanged := p.EditionID != nil && *p.EditionID != l.Params.EditionID
	if !editionChanged && p.Features == nil {
		return nil
	}
	if p.EditionID != nil {
		l.Params.EditionID = *p.EditionID
	}
	var requested []string
	if p.Features != nil {
		var herr *fakeHTTPError
		if requested, herr = fakeTokens(p.Features, 100); herr != nil {
			return herr
		}
		if requested == nil {
			requested = []string{}
		}
	}
	f, herr := fakeResolveFeatures(l.Product, l.Params.EditionID, requested)
	if herr != nil {
		return herr
	}
	l.Params.Features = f
	return nil
}

// endLicense mirrors DELETE: status ended, position freed, repeatable.
func (h *fakeHub) endLicense(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l, ok := h.licenses[r.PathValue("id")]
	if !ok {
		(&fakeHTTPError{status: http.StatusNotFound, code: "license_not_found", message: "Lizenz nicht gefunden"}).write(w)
		return
	}
	h.endLicenseLocked(l, "apikey:test")
	writeData(w, http.StatusOK, map[string]any{"license": *l})
}

// endLicenseLocked ends l once. Caller holds mu.
func (h *fakeHub) endLicenseLocked(l *fakeLicense, by string) {
	if l.Status == "ended" {
		return
	}
	at := nowISO()
	l.Status, l.EndedAt, l.EndedBy, l.UpdatedAt = "ended", &at, &by, at
	if l.ContractPositionID != nil && h.positions[*l.ContractPositionID].LicenseID == l.LicenseID {
		h.positions[*l.ContractPositionID].LicenseID = ""
	}
}

// jsonEqual compares two JSON-ish values after a round trip, so tests can
// compare decoded request bodies with literal expectations.
func jsonEqual(a, b any) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	var va, vb any
	if json.NewDecoder(bytes.NewReader(ja)).Decode(&va) != nil || json.NewDecoder(bytes.NewReader(jb)).Decode(&vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}
