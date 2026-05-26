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
		// Accepted by default.
		{"default", "https://hub.innfactory.cloud", false, false},
		{"subdomain", "https://staging.innfactory.cloud", false, false},
		{"subdomain-deep", "https://eu1.staging.innfactory.cloud", false, false},
		{"https-port", "https://hub.innfactory.cloud:8443", false, false},
		// Refused without opt-in.
		{"http-hub", "http://hub.innfactory.cloud", false, true},
		{"https-evil", "https://evil.example.com", false, true},
		{"https-typo", "https://hub.innfact0ry.cloud", false, true},
		{"https-raw-ip", "https://10.0.0.1", false, true},
		{"empty", "", false, true},
		{"not-a-url", "::::", false, true},
		// Opt-in mode lets anything through.
		{"http-hub-opt-in", "http://hub.innfactory.cloud", true, false},
		{"localhost-opt-in", "http://localhost:8080", true, false},
		{"raw-ip-opt-in", "https://10.0.0.1", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
