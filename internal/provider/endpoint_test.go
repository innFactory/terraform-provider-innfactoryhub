package provider

import "testing"

// validateEndpoint is the credential-leak boundary — it decides whether the
// system admin key will be sent to a given URL. Test every refused shape
// AND the explicit opt-out path.
func TestValidateEndpoint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		endpoint string
		insecure bool
		wantErr  bool
	}{
		// Accepted by default: innfactory.cloud (innfactoryHUB).
		{"default", "https://hub.innfactory.cloud", false, false},
		{"subdomain", "https://staging.innfactory.cloud", false, false},
		{"subdomain-deep", "https://eu1.staging.innfactory.cloud", false, false},
		{"https-port", "https://hub.innfactory.cloud:8443", false, false},
		{"cloud-apex", "https://innfactory.cloud", false, false},
		{"trailing-slash", "https://hub.innfactory.cloud/", false, false},
		// Accepted by default: innfactory.ai (innFactory AI Hub).
		{"ai-hub", "https://hub.innfactory.ai", false, false},
		{"ai-apex", "https://innfactory.ai", false, false},
		{"ai-subdomain-deep", "https://preview.hub.innfactory.ai", false, false},
		{"ai-port", "https://hub.innfactory.ai:443", false, false},
		{"ai-uppercase", "https://HUB.INNFACTORY.AI", false, false},
		// Refused without opt-in.
		{"http-hub", "http://hub.innfactory.cloud", false, true},
		{"http-ai-hub", "http://hub.innfactory.ai", false, true},
		{"https-evil", "https://evil.example.com", false, true},
		{"https-typo", "https://hub.innfact0ry.cloud", false, true},
		{"https-raw-ip", "https://10.0.0.1", false, true},
		{"empty", "", false, true},
		{"not-a-url", "::::", false, true},
		// Look-alike hosts that only contain an allowed domain.
		{"ai-suffix-spoof", "https://innfactory.ai.evil.com", false, true},
		{"ai-hub-suffix-spoof", "https://hub.innfactory.ai.evil.com", false, true},
		{"cloud-suffix-spoof", "https://hub.innfactory.cloud.evil.com", false, true},
		{"ai-prefix-spoof", "https://evilinnfactory.ai", false, true},
		{"cloud-prefix-spoof", "https://notinnfactory.cloud", false, true},
		{"ai-userinfo-spoof", "https://hub.innfactory.ai@evil.com", false, true},
		{"ai-in-path", "https://evil.com/hub.innfactory.ai", false, true},
		{"ai-other-tld", "https://hub.innfactory.ai.de", false, true},
		{"ai-trailing-dot", "https://hub.innfactory.ai.", false, true},
		// Opt-in mode lets anything through.
		{"http-hub-opt-in", "http://hub.innfactory.cloud", true, false},
		{"localhost-opt-in", "http://localhost:8080", true, false},
		{"raw-ip-opt-in", "https://10.0.0.1", true, false},
		// Opt-in never makes a non-URL acceptable.
		{"not-a-url-opt-in", "::::", true, true},
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
