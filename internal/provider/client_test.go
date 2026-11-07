package provider

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// Secrets used across the redaction tests. The JWT is a syntactically valid
// compact JWS (header {"alg":"EdDSA","typ":"license+jwt","kid":"ifai-lic-1"}).
const (
	secretAK       = "ak_k3y1d0000001.QWxhZGRpbjpvcGVuIHNlc2FtZQ"
	secretLK       = "lk_l1c3ns3k3y01.c2VjcmV0LWxpY2Vuc2Uta2V5LXNlY3JldA"
	secretJWT      = "eyJhbGciOiJFZERTQSIsInR5cCI6ImxpY2Vuc2Urand0Iiwia2lkIjoiaWZhaS1saWMtMSJ9.eyJzdWIiOiJsaWNfMTIzIiwiYXVkIjoiYWktZ2F0ZXdheSJ9.c2lnbmF0dXJlLWJ5dGVzLXNpZ25hdHVyZS1ieXRlcw"
	secretLongTail = "ak_k3y1d0000002.0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ_-0123456789abcdefghijklmnopqrstuvwxyz"
)

func TestRedact(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		in       string
		secrets  []string
		mustKeep []string
	}{
		{
			name:     "system api key",
			in:       "Authorization: Bearer " + secretAK + " rejected",
			secrets:  []string{secretAK, "QWxhZGRpbjpvcGVuIHNlc2FtZQ"},
			mustKeep: []string{"Authorization: Bearer", "rejected"},
		},
		{
			name:     "licence key",
			in:       "license key " + secretLK + " is suspended",
			secrets:  []string{secretLK, "c2VjcmV0LWxpY2Vuc2Uta2V5LXNlY3JldA"},
			mustKeep: []string{"license key", "is suspended"},
		},
		{
			name:     "licence jwt",
			in:       `{"token":"` + secretJWT + `"}`,
			secrets:  []string{secretJWT, "c2lnbmF0dXJlLWJ5dGVzLXNpZ25hdHVyZS1ieXRlcw", "eyJzdWIiOiJsaWNfMTIzIiwiYXVkIjoiYWktZ2F0ZXdheSJ9"},
			mustKeep: []string{`{"token":"`},
		},
		{
			name:    "secret longer than 64 characters is redacted completely",
			in:      "key=" + secretLongTail,
			secrets: []string{secretLongTail, "abcdefghijklmnopqrstuvwxyz"},
		},
		{
			name:    "all shapes in one message",
			in:      secretAK + " / " + secretLK + " / " + secretJWT,
			secrets: []string{secretAK, secretLK, secretJWT},
		},
		{
			name:     "harmless text stays readable",
			in:       "hub api 404 tenant_not_found: No tenant has identifiers.azure_tenant_ids containing \"0f5c9c58\"",
			mustKeep: []string{"tenant_not_found", "identifiers.azure_tenant_ids", "0f5c9c58"},
		},
		{
			name:     "key ids without secret are not secrets",
			in:       "key ak_abc revoked",
			mustKeep: []string{"key ak_abc revoked"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := redact(tc.in)
			for _, s := range tc.secrets {
				if strings.Contains(got, s) {
					t.Errorf("redact leaked %q in %q", s, got)
				}
			}
			for _, k := range tc.mustKeep {
				if !strings.Contains(got, k) {
					t.Errorf("redact removed harmless %q from %q", k, got)
				}
			}
		})
	}
}

// Every error path of the client must come back scrubbed, whether the Hub
// answers with the structured envelope or with a raw proxy page.
func TestClientErrorsAreRedacted(t *testing.T) {
	leaky := secretAK + " " + secretLK + " " + secretJWT
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{name: "structured message", status: http.StatusBadRequest, body: `{"error":{"code":"bad_request","message":"echo: ` + leaky + `"}}`},
		{name: "structured code", status: http.StatusConflict, body: `{"error":{"code":"` + secretLK + `","message":"x"}}`},
		{name: "unstructured proxy page", status: http.StatusBadGateway, body: "<html>upstream said Authorization: Bearer " + leaky + "</html>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hub := newFakeHub(t)
			hub.failAll(tc.status, tc.body)
			c := newClient(hub.URL(), testAdminKey, "test")

			err := c.do(context.Background(), http.MethodGet, "/api/v1/admin/environments/env_1", nil, nil, nil)
			if err == nil {
				t.Fatal("expected an error")
			}
			msg := err.Error()
			for _, s := range []string{secretAK, secretLK, secretJWT, "QWxhZGRpbjpvcGVuIHNlc2FtZQ"} {
				if strings.Contains(msg, s) {
					t.Errorf("error leaked %q: %s", s, msg)
				}
			}
			var ae *apiError
			if !errors.As(err, &ae) || ae.Status != tc.status {
				t.Errorf("expected *apiError with status %d, got %T %v", tc.status, err, err)
			}
		})
	}
}

func TestClientWithoutAdminKeySendsNothing(t *testing.T) {
	t.Parallel()
	hub := newFakeHub(t)
	c := newClient(hub.URL(), "", "test")

	err := c.do(context.Background(), http.MethodGet, "/api/v1/admin/tenants/lookup", nil, nil, nil)
	if !errors.Is(err, errMissingAdminKey) {
		t.Fatalf("expected errMissingAdminKey, got %v", err)
	}
	if n := hub.callCount(); n != 0 {
		t.Fatalf("client sent %d request(s) without an admin key", n)
	}
}

// A trailing slash on the endpoint must not produce `//api/v1/…` paths.
func TestClientJoinsEndpointWithTrailingSlash(t *testing.T) {
	t.Parallel()
	hub := newFakeHub(t)
	c := newClient(hub.URL()+"/", testAdminKey, "test")

	var env envelope[tenantLookupResponse]
	q := map[string][]string{"tenant_id": {alphaTenantID}}
	if err := c.do(context.Background(), http.MethodGet, "/api/v1/admin/tenants/lookup", q, nil, &env); err != nil {
		t.Fatalf("lookup failed: %v", err)
	}
	if env.Data.Tenant.TenantID != alphaTenantID {
		t.Fatalf("got tenant %q", env.Data.Tenant.TenantID)
	}
	// ServeMux would silently redirect `//api/…` for GET; assert the wire path.
	if got := hub.allCalls(); len(got) != 1 || got[0] != "GET /api/v1/admin/tenants/lookup" {
		t.Fatalf("unexpected request path(s): %v", got)
	}
}

// Transport errors echo the request URL; they are redacted too, and the
// underlying cause stays inspectable.
func TestClientTransportErrorsAreRedacted(t *testing.T) {
	t.Parallel()
	hub := newFakeHub(t)
	base := hub.URL()
	hub.srv.Close() // connection refused from here on

	c := newClient(base, testAdminKey, "test")
	err := c.do(context.Background(), http.MethodGet, "/api/v1/admin/environments/"+secretLK, nil, nil, nil)
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), secretLK) {
		t.Fatalf("transport error leaked the key: %v", err)
	}
	var re *redactedErr
	if !errors.As(err, &re) || errors.Unwrap(re) == nil {
		t.Fatalf("expected a redactedErr wrapping the cause, got %T", err)
	}
}

// The Hub explains many rejections in error.details (e.g. the editions that
// exist); the client renders them, capped and redacted like the message.
func TestClientErrorsCarryDetails(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		want     string
		mustSkip []string
	}{
		{
			name: "details rendered",
			body: `{"error":{"code":"edition_unknown","message":"Edition x gibt es nicht","details":{"available":["all","basic"]}}}`,
			want: `hub api 422 edition_unknown: Edition x gibt es nicht (details: {"available":["all","basic"]})`,
		},
		{
			name: "no details",
			body: `{"error":{"code":"license_ended","message":"Die Lizenz ist beendet"}}`,
			want: `hub api 422 license_ended: Die Lizenz ist beendet`,
		},
		{
			name:     "details redacted",
			body:     `{"error":{"code":"bad_request","message":"x","details":{"echo":"` + secretLK + `"}}}`,
			mustSkip: []string{secretLK},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hub := newFakeHub(t)
			hub.failAll(http.StatusUnprocessableEntity, tc.body)
			c := newClient(hub.URL(), testAdminKey, "test")

			err := c.do(context.Background(), http.MethodGet, "/api/v1/admin/licenses/lic_1", nil, nil, nil)
			if err == nil {
				t.Fatal("expected an error")
			}
			if tc.want != "" && err.Error() != tc.want {
				t.Fatalf("error\n got: %s\nwant: %s", err, tc.want)
			}
			for _, s := range tc.mustSkip {
				if strings.Contains(err.Error(), s) {
					t.Fatalf("error leaked %q: %s", s, err)
				}
			}
		})
	}
}
