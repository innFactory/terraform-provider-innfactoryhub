package provider

import (
	"fmt"
	"net/http"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// Only the Hub's own "not found" codes mean the object is gone. A generic
// 404 — a proxy page, a wrong path, another service — must not drop the
// resource from state, or the next apply would issue a second (unbilled)
// licence or register the environment again.
var foreign404s = []struct {
	name string
	body string
}{
	{name: "generic code", body: `{"error":{"code":"not_found","message":"Not Found"}}`},
	{name: "html page", body: `<html><body>404 page not found</body></html>`},
	{name: "other resource's code", body: `{"error":{"code":"environment_not_found","message":"x"}}`},
}

func TestLicenseReadKeepsStateOnForeign404(t *testing.T) {
	for _, tc := range foreign404s {
		t.Run(tc.name, func(t *testing.T) {
			hub := newLicenseHub(t)
			useFakeHubEnv(t)
			cfg := licenseConfig(hub, gatewayLicense())
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testProviderFactories(),
				CheckDestroy:             checkLicensesEnded(hub),
				Steps: []resource.TestStep{
					{Config: cfg},
					{
						PreConfig:   func() { hub.failPath(http.MethodGet, "", http.StatusNotFound, tc.body) },
						Config:      cfg,
						PlanOnly:    true,
						ExpectError: regexp.MustCompile(`(?s)Read licence failed.*404`),
					},
					{
						PreConfig:        func() { hub.failPath("", "", 0, "") },
						Config:           cfg,
						ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
					},
				},
			})
			if ids := hub.licenseIDsByStatus(""); len(ids) != 1 {
				t.Fatalf("licences %v, want exactly the original one", ids)
			}
		})
	}
}

func TestEnvironmentReadKeepsStateOnForeign404(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{name: "generic code", body: `{"error":{"code":"not_found","message":"Not Found"}}`},
		{name: "html page", body: `<html>404</html>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := newFakeHub(t)
			useFakeHubEnv(t)
			cfg := envConfig(hub, azureTenantLine(), "Alpha AI Gateway", "gpt.alpha.example", "azure")
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testProviderFactories(),
				CheckDestroy:             checkEnvironmentGone(hub),
				Steps: []resource.TestStep{
					{Config: cfg},
					{
						PreConfig:   func() { hub.failPath(http.MethodGet, "", http.StatusNotFound, tc.body) },
						Config:      cfg,
						PlanOnly:    true,
						ExpectError: regexp.MustCompile(`(?s)Read environment failed.*404`),
					},
					{
						PreConfig:        func() { hub.failPath("", "", 0, "") },
						Config:           cfg,
						ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
					},
				},
			})
		})
	}
}

// Destroy tolerates only the Hub's own "not found": a foreign 404 says
// nothing about whether the licence was ended, the environment deleted or
// the key revoked.
func TestDestroyFailsOnForeign404(t *testing.T) {
	body := `<html>404 page not found</html>`
	cases := []struct {
		name, cfg, method, wantErr string
		hub                        func(t *testing.T) *fakeHub
	}{
		{name: "licence", method: http.MethodDelete, wantErr: `(?s)End licence failed.*404`,
			hub: newLicenseHub, cfg: "license"},
		{name: "environment", method: http.MethodDelete, wantErr: `(?s)Delete environment failed.*404`,
			hub: newFakeHub, cfg: "environment"},
		{name: "api key", method: http.MethodPost, wantErr: `(?s)Revoke API key failed.*404`,
			hub: newFakeHub, cfg: "api_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := tc.hub(t)
			useFakeHubEnv(t)
			cfg := map[string]string{
				"license":     licenseConfig(hub, gatewayLicense()),
				"environment": envConfig(hub, azureTenantLine(), "Alpha AI Gateway", "gpt.alpha.example", "azure"),
				"api_key":     maintenanceKeyConfig(hub.providerBlock(), ""),
			}[tc.cfg]
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testProviderFactories(),
				Steps: []resource.TestStep{
					{Config: cfg},
					{
						PreConfig:   func() { hub.failPath(tc.method, "", http.StatusNotFound, body) },
						Config:      cfg,
						Destroy:     true,
						ExpectError: regexp.MustCompile(tc.wantErr),
					},
					{
						// Clean up for the harness' own destroy.
						PreConfig: func() { hub.failPath("", "", 0, "") },
						Config:    cfg,
						Check:     func(*terraform.State) error { return nil },
					},
				},
			})
		})
	}
}

// The Hub's own codes still drop the resource (another Hub, lost data).
func TestHubNotFoundCodesDropState(t *testing.T) {
	for code, want := range map[string]bool{
		"license_not_found":     true,
		"environment_not_found": false,
		"not_found":             false,
	} {
		err := &apiError{Status: http.StatusNotFound, Code: code}
		if got := isHubNotFound(err, "license_not_found"); got != want {
			t.Errorf("isHubNotFound(404 %s, license_not_found) = %v, want %v", code, got, want)
		}
	}
	if isHubNotFound(&apiError{Status: http.StatusConflict, Code: "license_not_found"}, "license_not_found") {
		t.Error("a 409 is not a 404")
	}
	if isHubNotFound(fmt.Errorf("wrapped: %w", &apiError{Status: 404, Code: "license_not_found"}), "license_not_found") == false {
		t.Error("wrapped errors must be inspected")
	}
}
