package provider

import "testing"

// validateEndpoint is the credential-leak boundary — it decides whether the
// system admin key will be sent to a given URL. Exactly the two hub hosts are
// trusted; everything else needs the explicit opt-out naming that very host,
// and even then plain http only reaches the local machine.
func TestValidateEndpoint(t *testing.T) {
	t.Parallel()
	type endpointCase struct {
		name     string
		endpoint string
		insecure bool
		wantErr  bool
		allowed  string // INNFACTORYHUB_INSECURE_ENDPOINT_HOST
	}
	cases := []endpointCase{
		// Accepted by default: exactly the two hub hosts.
		{"innfactoryhub", "https://hub.innfactory.cloud", false, false, ""},
		{"ai-hub", "https://hub.innfactory.ai", false, false, ""},
		{"trailing-slash", "https://hub.innfactory.cloud/", false, false, ""},
		{"ai-default-port", "https://hub.innfactory.ai:443", false, false, ""},
		{"ai-uppercase", "https://HUB.INNFACTORY.AI", false, false, ""},
		// Other names in the same domains are refused (SEC-009).
		{"cloud-subdomain", "https://staging.innfactory.cloud", false, true, ""},
		{"cloud-subdomain-deep", "https://eu1.staging.innfactory.cloud", false, true, ""},
		{"cloud-admin-host", "https://admin.hub.innfactory.cloud", false, true, ""},
		{"cloud-apex", "https://innfactory.cloud", false, true, ""},
		{"ai-apex", "https://innfactory.ai", false, true, ""},
		{"ai-subdomain-deep", "https://preview.hub.innfactory.ai", false, true, ""},
		{"ai-other-subdomain", "https://license.innfactory.ai", false, true, ""},
		{"ai-other-port", "https://hub.innfactory.ai:8443", false, true, ""},
		{"cloud-other-port", "https://hub.innfactory.cloud:8443", false, true, ""},
		// Refused without opt-in.
		{"http-hub", "http://hub.innfactory.cloud", false, true, ""},
		{"http-ai-hub", "http://hub.innfactory.ai", false, true, ""},
		{"https-evil", "https://evil.example.com", false, true, ""},
		{"https-typo", "https://hub.innfact0ry.cloud", false, true, ""},
		{"https-raw-ip", "https://10.0.0.1", false, true, ""},
		{"empty", "", false, true, ""},
		{"not-a-url", "::::", false, true, ""},
		{"no-scheme", "hub.innfactory.ai", false, true, ""},
		// Look-alike hosts that only contain an allowed name.
		{"ai-suffix-spoof", "https://hub.innfactory.ai.evil.com", false, true, ""},
		{"cloud-suffix-spoof", "https://hub.innfactory.cloud.evil.com", false, true, ""},
		{"ai-prefix-spoof", "https://evilhub.innfactory.ai", false, true, ""},
		{"ai-userinfo-spoof", "https://hub.innfactory.ai@evil.com", false, true, ""},
		{"ai-in-path", "https://evil.com/hub.innfactory.ai", false, true, ""},
		{"ai-other-tld", "https://hub.innfactory.ai.de", false, true, ""},
		{"ai-trailing-dot", "https://hub.innfactory.ai.", false, true, ""},
		// The endpoint is a bare origin: no credentials, path, query or fragment.
		{"ai-userinfo", "https://user:pass@hub.innfactory.ai", false, true, ""},
		{"ai-path", "https://hub.innfactory.ai/api/v1", false, true, ""},
		{"ai-query", "https://hub.innfactory.ai/?x=1", false, true, ""},
		{"ai-fragment", "https://hub.innfactory.ai/#x", false, true, ""},
		// Opt-in: the named https host, plain http only to the local machine.
		{"https-other-opt-in", "https://staging.example.com", true, false, "staging.example.com"},
		{"raw-ip-opt-in", "https://10.0.0.1", true, false, "10.0.0.1"},
		{"localhost-opt-in", "http://localhost:8080", true, false, "localhost"},
		{"loopback-v4-opt-in", "http://127.0.0.1:5100", true, false, "127.0.0.1"},
		{"loopback-v6-opt-in", "http://[::1]:5100", true, false, "::1"},
		{"opt-in-host-case", "https://Staging.Example.com", true, false, "STAGING.example.com"},
		{"http-hub-opt-in", "http://hub.innfactory.cloud", true, true, "hub.innfactory.cloud"},
		{"http-raw-ip-opt-in", "http://10.0.0.1", true, true, "10.0.0.1"},
		{"http-localhost-spoof-opt-in", "http://localhost.evil.com", true, true, "localhost.evil.com"},
		// =1 alone is not enough: the host must be named, and match.
		{"opt-in-without-host", "https://staging.example.com", true, true, ""},
		{"opt-in-other-host", "https://evil.example.com", true, true, "staging.example.com"},
		{"opt-in-host-is-suffix", "https://evil-staging.example.com", true, true, "staging.example.com"},
		{"host-without-opt-in", "https://staging.example.com", false, true, "staging.example.com"},
		// The trusted hosts need no opt-in, with or without it.
		{"trusted-with-opt-in", "https://hub.innfactory.ai", true, false, "localhost"},
		// Opt-in never makes a non-URL acceptable.
		{"not-a-url-opt-in", "::::", true, true, "localhost"},
		{"ftp-opt-in", "ftp://localhost", true, true, "localhost"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateEndpoint(tc.endpoint, tc.insecure, tc.allowed)
			if tc.wantErr && err == nil {
				t.Fatalf("validateEndpoint(%q, insecure=%v, host=%q): expected error, got nil", tc.endpoint, tc.insecure, tc.allowed)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateEndpoint(%q, insecure=%v, host=%q): unexpected error: %v", tc.endpoint, tc.insecure, tc.allowed, err)
			}
		})
	}
}
