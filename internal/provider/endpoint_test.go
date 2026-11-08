package provider

import "testing"

// validateEndpoint is the credential-leak boundary — it decides whether the
// system admin key will be sent to a given URL. Exactly the two hub hosts are
// trusted; everything else needs the explicit opt-out, and even then plain
// http only reaches the local machine.
func TestValidateEndpoint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		endpoint string
		insecure bool
		wantErr  bool
	}{
		// Accepted by default: exactly the two hub hosts.
		{"innfactoryhub", "https://hub.innfactory.cloud", false, false},
		{"ai-hub", "https://hub.innfactory.ai", false, false},
		{"trailing-slash", "https://hub.innfactory.cloud/", false, false},
		{"ai-default-port", "https://hub.innfactory.ai:443", false, false},
		{"ai-uppercase", "https://HUB.INNFACTORY.AI", false, false},
		// Other names in the same domains are refused (SEC-009).
		{"cloud-subdomain", "https://staging.innfactory.cloud", false, true},
		{"cloud-subdomain-deep", "https://eu1.staging.innfactory.cloud", false, true},
		{"cloud-admin-host", "https://admin.hub.innfactory.cloud", false, true},
		{"cloud-apex", "https://innfactory.cloud", false, true},
		{"ai-apex", "https://innfactory.ai", false, true},
		{"ai-subdomain-deep", "https://preview.hub.innfactory.ai", false, true},
		{"ai-other-subdomain", "https://license.innfactory.ai", false, true},
		{"ai-other-port", "https://hub.innfactory.ai:8443", false, true},
		{"cloud-other-port", "https://hub.innfactory.cloud:8443", false, true},
		// Refused without opt-in.
		{"http-hub", "http://hub.innfactory.cloud", false, true},
		{"http-ai-hub", "http://hub.innfactory.ai", false, true},
		{"https-evil", "https://evil.example.com", false, true},
		{"https-typo", "https://hub.innfact0ry.cloud", false, true},
		{"https-raw-ip", "https://10.0.0.1", false, true},
		{"empty", "", false, true},
		{"not-a-url", "::::", false, true},
		{"no-scheme", "hub.innfactory.ai", false, true},
		// Look-alike hosts that only contain an allowed name.
		{"ai-suffix-spoof", "https://hub.innfactory.ai.evil.com", false, true},
		{"cloud-suffix-spoof", "https://hub.innfactory.cloud.evil.com", false, true},
		{"ai-prefix-spoof", "https://evilhub.innfactory.ai", false, true},
		{"ai-userinfo-spoof", "https://hub.innfactory.ai@evil.com", false, true},
		{"ai-in-path", "https://evil.com/hub.innfactory.ai", false, true},
		{"ai-other-tld", "https://hub.innfactory.ai.de", false, true},
		{"ai-trailing-dot", "https://hub.innfactory.ai.", false, true},
		// The endpoint is a bare origin: no credentials, path, query or fragment.
		{"ai-userinfo", "https://user:pass@hub.innfactory.ai", false, true},
		{"ai-path", "https://hub.innfactory.ai/api/v1", false, true},
		{"ai-query", "https://hub.innfactory.ai/?x=1", false, true},
		{"ai-fragment", "https://hub.innfactory.ai/#x", false, true},
		// Opt-in: any https host, plain http only to the local machine.
		{"https-other-opt-in", "https://staging.example.com", true, false},
		{"raw-ip-opt-in", "https://10.0.0.1", true, false},
		{"localhost-opt-in", "http://localhost:8080", true, false},
		{"loopback-v4-opt-in", "http://127.0.0.1:5100", true, false},
		{"loopback-v6-opt-in", "http://[::1]:5100", true, false},
		{"http-hub-opt-in", "http://hub.innfactory.cloud", true, true},
		{"http-raw-ip-opt-in", "http://10.0.0.1", true, true},
		{"http-localhost-spoof-opt-in", "http://localhost.evil.com", true, true},
		// Opt-in never makes a non-URL acceptable.
		{"not-a-url-opt-in", "::::", true, true},
		{"ftp-opt-in", "ftp://localhost", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateEndpoint(tc.endpoint, tc.insecure)
			if tc.wantErr && err == nil {
				t.Fatalf("validateEndpoint(%q, insecure=%v): expected error, got nil", tc.endpoint, tc.insecure)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateEndpoint(%q, insecure=%v): unexpected error: %v", tc.endpoint, tc.insecure, err)
			}
		})
	}
}
