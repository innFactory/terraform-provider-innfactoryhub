package provider

import (
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

const envAddr = "innfactoryhub_environment.gw"

// envConfig renders one environment. customer is the raw HCL line(s) that
// bind it to the customer, e.g. `azure_tenant_id = "…"`.
func envConfig(hub *fakeHub, customer, name, domain, cloud string) string {
	return hub.providerBlock() + fmt.Sprintf(`
resource "innfactoryhub_environment" "gw" {
  %s
  name   = %q
  domain = %q
  cloud  = %q
}
`, customer, name, domain, cloud)
}

func azureTenantLine() string { return fmt.Sprintf("azure_tenant_id = %q", alphaAzureTenantID) }

func handleLine(handle string) string { return fmt.Sprintf("companygpt_handle = %q", handle) }

// checkEnvironmentGone is the CheckDestroy for every environment test.
func checkEnvironmentGone(hub *fakeHub) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if ids := hub.environmentIDs(); len(ids) != 0 {
			return fmt.Errorf("environments left after destroy: %v", ids)
		}
		return nil
	}
}

// The CompanyGPT rollout path: register the gateway environment by the
// customer's Azure (Entra ID) tenant, let the Hub resolve the customer.
func TestEnvironmentCreateByAzureTenantID(t *testing.T) {
	hub := newFakeHub(t)
	useFakeHubEnv(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkEnvironmentGone(hub),
		Steps: []resource.TestStep{{
			Config: envConfig(hub, azureTenantLine(), "Alpha AI Gateway", "gpt.alpha.example", "azure"),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttrSet(envAddr, "id"),
				resource.TestCheckResourceAttr(envAddr, "tenant_id", alphaTenantID),
				resource.TestCheckResourceAttr(envAddr, "azure_tenant_id", alphaAzureTenantID),
				resource.TestCheckResourceAttr(envAddr, "name", "Alpha AI Gateway"),
				resource.TestCheckResourceAttr(envAddr, "domain", "gpt.alpha.example"),
				resource.TestCheckResourceAttr(envAddr, "cloud", "azure"),
				resource.TestCheckNoResourceAttr(envAddr, "gcp_project_id"),
				func(s *terraform.State) error {
					want := map[string]any{
						"lookup": map[string]any{"azure_tenant_id": alphaAzureTenantID},
						"name":   "Alpha AI Gateway",
						"domain": "gpt.alpha.example",
						"cloud":  "azure",
					}
					got := hub.lastBody("POST /api/v1/admin/environments")
					if !reflect.DeepEqual(got, want) {
						return fmt.Errorf("POST body\n got: %v\nwant: %v", got, want)
					}
					id := s.RootModule().Resources[envAddr].Primary.ID
					if _, ok := hub.environment(id); !ok {
						return fmt.Errorf("environment %s not stored in the Hub", id)
					}
					return nil
				},
			),
		}},
	})
}

// A second run without config changes must neither plan nor send writes.
func TestEnvironmentNoChangesMeansNoWrites(t *testing.T) {
	hub := newFakeHub(t)
	useFakeHubEnv(t)
	cfg := envConfig(hub, azureTenantLine(), "Alpha AI Gateway", "gpt.alpha.example", "azure")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkEnvironmentGone(hub),
		Steps: []resource.TestStep{
			{
				Config: cfg,
				Check: func(*terraform.State) error {
					hub.resetCalls()
					return nil
				},
			},
			{
				Config:   cfg,
				PlanOnly: true,
				Check: func(*terraform.State) error {
					if w := hub.writeCalls(); len(w) != 0 {
						return fmt.Errorf("unchanged config sent writes: %v", w)
					}
					return nil
				},
			},
		},
	})
	if w := hub.writeCalls(); len(w) != 1 || w[0][:7] != "DELETE " {
		t.Fatalf("expected only the final destroy to write, got %v", w)
	}
}

// Renaming or moving the installation to another host must update the
// environment in place — licences hang off its id.
func TestEnvironmentUpdateNameAndDomainInPlace(t *testing.T) {
	hub := newFakeHub(t)
	useFakeHubEnv(t)

	var id string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkEnvironmentGone(hub),
		Steps: []resource.TestStep{
			{
				Config: envConfig(hub, azureTenantLine(), "Alpha AI Gateway", "gpt.alpha.example", "azure"),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(envAddr, "id", &id),
					func(*terraform.State) error { hub.resetCalls(); return nil },
				),
			},
			{
				Config: envConfig(hub, azureTenantLine(), "Alpha Gateway PROD", "ai.alpha.example", "azure"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(envAddr, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(envAddr, tfjsonpath.New("id"), knownvalue.NotNull()),
						plancheck.ExpectKnownValue(envAddr, tfjsonpath.New("tenant_id"), knownvalue.StringExact(alphaTenantID)),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(envAddr, "name", "Alpha Gateway PROD"),
					resource.TestCheckResourceAttr(envAddr, "domain", "ai.alpha.example"),
					resource.TestCheckResourceAttr(envAddr, "tenant_id", alphaTenantID),
					resource.TestCheckResourceAttr(envAddr, "azure_tenant_id", alphaAzureTenantID),
					func(s *terraform.State) error {
						if got := s.RootModule().Resources[envAddr].Primary.ID; got != id {
							return fmt.Errorf("id changed from %s to %s", id, got)
						}
						want := map[string]any{"name": "Alpha Gateway PROD", "domain": "ai.alpha.example"}
						if got := hub.lastBody("PATCH " + environmentPath(id)); !reflect.DeepEqual(got, want) {
							return fmt.Errorf("PATCH body\n got: %v\nwant: %v", got, want)
						}
						if w := hub.writeCalls(); len(w) != 1 || w[0] != "PATCH "+environmentPath(id) {
							return fmt.Errorf("expected exactly one PATCH, got %v", w)
						}
						stored, _ := hub.environment(id)
						if stored.Name != "Alpha Gateway PROD" || stored.Domain != "ai.alpha.example" {
							return fmt.Errorf("Hub not updated: %+v", stored)
						}
						return nil
					},
				),
			},
		},
	})
}

// cloud and the customer binding are identity: changing them must create a
// new environment instead of silently re-pointing the existing one.
func TestEnvironmentReplacement(t *testing.T) {
	tenantLine := func(id string) string { return fmt.Sprintf("tenant_id = %q", id) }
	cases := []struct {
		name         string
		fromCustomer string
		fromCloud    string
		toCustomer   string
		toCloud      string
		wantTenant   string
	}{
		{
			name:         "cloud changes",
			fromCustomer: azureTenantLine(), fromCloud: "azure",
			toCustomer: azureTenantLine(), toCloud: "other",
			wantTenant: alphaTenantID,
		},
		{
			name:         "lookup identifier kind changes",
			fromCustomer: azureTenantLine(), fromCloud: "azure",
			toCustomer: fmt.Sprintf("gcp_project_id = %q", alphaGcpProject), toCloud: "azure",
			wantTenant: alphaTenantID,
		},
		{
			name:         "lookup replaced by tenant_id",
			fromCustomer: azureTenantLine(), fromCloud: "azure",
			toCustomer: tenantLine(alphaTenantID), toCloud: "azure",
			wantTenant: alphaTenantID,
		},
		{
			name:         "tenant_id replaced by lookup",
			fromCustomer: tenantLine(alphaTenantID), fromCloud: "azure",
			toCustomer: fmt.Sprintf("azure_tenant_domain = %q", alphaTenantDomain), toCloud: "azure",
			wantTenant: alphaTenantID,
		},
		{
			name:         "tenant_id changes",
			fromCustomer: tenantLine(alphaTenantID), fromCloud: "gcp",
			toCustomer: tenantLine(betaTenantID), toCloud: "gcp",
			wantTenant: betaTenantID,
		},
		{
			name:         "lookup identifier replaced by companygpt_handle",
			fromCustomer: azureTenantLine(), fromCloud: "azure",
			toCustomer: handleLine(alphaHandle), toCloud: "azure",
			wantTenant: alphaTenantID,
		},
		{
			name:         "companygpt_handle of the same customer changes",
			fromCustomer: handleLine(alphaHandle), fromCloud: "azure",
			toCustomer: handleLine(alphaStagingHandle), toCloud: "azure",
			wantTenant: alphaTenantID,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newFakeHub(t)
			useFakeHubEnv(t)

			var firstID string
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testProviderFactories(),
				CheckDestroy:             checkEnvironmentGone(hub),
				Steps: []resource.TestStep{
					{
						Config: envConfig(hub, tc.fromCustomer, "Gateway", "gpt.alpha.example", tc.fromCloud),
						Check:  captureAttr(envAddr, "id", &firstID),
					},
					{
						Config: envConfig(hub, tc.toCustomer, "Gateway", "gpt.alpha.example", tc.toCloud),
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PreApply: []plancheck.PlanCheck{
								plancheck.ExpectResourceAction(envAddr, plancheck.ResourceActionDestroyBeforeCreate),
							},
						},
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr(envAddr, "tenant_id", tc.wantTenant),
							resource.TestCheckResourceAttr(envAddr, "cloud", tc.toCloud),
							func(s *terraform.State) error {
								ids := hub.environmentIDs()
								newID := s.RootModule().Resources[envAddr].Primary.ID
								if len(ids) != 1 || ids[0] != newID || newID == firstID {
									return fmt.Errorf("expected only the new environment (old %s), Hub has %v, state %s", firstID, ids, newID)
								}
								return nil
							},
						),
					},
				},
			})
		})
	}
}

// Import by environment id restores every attribute, including the lookup
// identifier or CompanyGPT handle the environment was registered with.
func TestEnvironmentImportRoundTrip(t *testing.T) {
	for name, customer := range map[string]string{
		"azure_tenant_id":   azureTenantLine(),
		"companygpt_handle": handleLine(alphaStagingHandle),
	} {
		t.Run(name, func(t *testing.T) {
			hub := newFakeHub(t)
			useFakeHubEnv(t)
			cfg := envConfig(hub, customer, "Alpha AI Gateway", "gpt.alpha.example", "azure")

			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testProviderFactories(),
				CheckDestroy:             checkEnvironmentGone(hub),
				Steps: []resource.TestStep{
					{Config: cfg},
					{
						Config:            cfg,
						ResourceName:      envAddr,
						ImportState:       true,
						ImportStateVerify: true,
					},
				},
			})
		})
	}
}

// An environment an admin created in the portal is adopted by importing it
// into a config that binds it by tenant_id; the follow-up plan is empty.
func TestEnvironmentImportPortalCreated(t *testing.T) {
	hub := newFakeHub(t)
	useFakeHubEnv(t)
	hub.seedEnvironment(fakeEnvironment{
		EnvironmentID: "env_portal01", TenantID: alphaTenantID, Name: "Alpha Bestand",
		Domain: "chat.alpha.example", Cloud: "gcp", CreatedBy: "admin",
		CreatedAt: nowISO(), UpdatedAt: nowISO(),
	})
	cfg := envConfig(hub, fmt.Sprintf("tenant_id = %q", alphaTenantID), "Alpha Bestand", "chat.alpha.example", "gcp")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkEnvironmentGone(hub),
		Steps: []resource.TestStep{
			{
				Config:             cfg,
				ResourceName:       envAddr,
				ImportState:        true,
				ImportStateId:      "env_portal01",
				ImportStatePersist: true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 {
						return fmt.Errorf("expected one imported instance, got %d", len(states))
					}
					a := states[0].Attributes
					want := map[string]string{
						"id": "env_portal01", "tenant_id": alphaTenantID, "name": "Alpha Bestand",
						"domain": "chat.alpha.example", "cloud": "gcp",
					}
					for k, v := range want {
						if a[k] != v {
							return fmt.Errorf("imported %s = %q, want %q", k, a[k], v)
						}
					}
					return nil
				},
			},
			{
				Config: cfg,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

func TestEnvironmentImportUnknownID(t *testing.T) {
	hub := newFakeHub(t)
	useFakeHubEnv(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		Steps: []resource.TestStep{{
			Config:        envConfig(hub, azureTenantLine(), "x", "gpt.alpha.example", "azure"),
			ResourceName:  envAddr,
			ImportState:   true,
			ImportStateId: "env_missing",
			ExpectError:   regexp.MustCompile(`(?i)non-existent|not exist`),
		}},
	})
}

// Invalid configuration is rejected at plan time, before any Hub call.
func TestEnvironmentConfigValidation(t *testing.T) {
	cases := []struct {
		name     string
		customer string
		envName  string
		domain   string
		cloud    string
		wantErr  string
	}{
		{name: "no customer binding", customer: "", envName: "gw", domain: "gpt.alpha.example", cloud: "azure", wantErr: `(?s)Missing Attribute Configuration.*Exactly one of these attributes`},
		{
			name:     "two lookup identifiers",
			customer: azureTenantLine() + fmt.Sprintf("\n  gcp_project_id = %q", alphaGcpProject),
			envName:  "gw", domain: "gpt.alpha.example", cloud: "azure", wantErr: `Invalid Attribute Combination`,
		},
		{
			name:     "tenant_id and lookup identifier",
			customer: azureTenantLine() + fmt.Sprintf("\n  tenant_id = %q", alphaTenantID),
			envName:  "gw", domain: "gpt.alpha.example", cloud: "azure", wantErr: `Invalid Attribute Combination`,
		},
		{name: "unknown cloud", customer: azureTenantLine(), envName: "gw", domain: "gpt.alpha.example", cloud: "openstack", wantErr: `(?s)cloud.*value must be one of`},
		{name: "blank name", customer: azureTenantLine(), envName: "   ", domain: "gpt.alpha.example", cloud: "azure", wantErr: `(?s)name.*must not be blank`},
		{name: "blank identifier", customer: `azure_tenant_id = "  "`, envName: "gw", domain: "gpt.alpha.example", cloud: "azure", wantErr: `(?s)azure_tenant_id.*must not be blank`},
		{name: "domain with scheme", customer: azureTenantLine(), envName: "gw", domain: "https://gpt.alpha.example", cloud: "azure", wantErr: `(?s)domain.*host name`},
		{name: "domain with path", customer: azureTenantLine(), envName: "gw", domain: "gpt.alpha.example/chat", cloud: "azure", wantErr: `(?s)domain.*host name`},
		{name: "domain with port", customer: azureTenantLine(), envName: "gw", domain: "gpt.alpha.example:8443", cloud: "azure", wantErr: `(?s)domain.*host name`},
		{name: "domain upper case", customer: azureTenantLine(), envName: "gw", domain: "GPT.alpha.example", cloud: "azure", wantErr: `(?s)domain.*host name`},
		{name: "domain without dot", customer: azureTenantLine(), envName: "gw", domain: "gateway", cloud: "azure", wantErr: `(?s)domain.*host name`},
		{
			name:     "companygpt_handle and lookup identifier",
			customer: handleLine(alphaHandle) + "\n  " + azureTenantLine(),
			envName:  "gw", domain: "gpt.alpha.example", cloud: "azure", wantErr: `Invalid Attribute Combination`,
		},
		{name: "companygpt_handle upper case", customer: handleLine("Alpha"), envName: "gw", domain: "gpt.alpha.example", cloud: "azure", wantErr: `(?s)companygpt_handle.*must be a CompanyGPT handle`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newFakeHub(t)
			useFakeHubEnv(t)

			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testProviderFactories(),
				Steps: []resource.TestStep{{
					Config:      envConfig(hub, tc.customer, tc.envName, tc.domain, tc.cloud),
					PlanOnly:    true,
					ExpectError: regexp.MustCompile(tc.wantErr),
				}},
			})
			if n := hub.callCount(); n != 0 {
				t.Fatalf("invalid config reached the Hub (%d request(s))", n)
			}
		})
	}
}

// Every supported lookup kind is sent as {lookup: {<kind>: <value>}} and
// resolves to the customer; tenant_id is sent top-level.
func TestEnvironmentCreateBodyPerCustomerBinding(t *testing.T) {
	cases := []struct {
		name     string
		attr     string
		value    string
		wantBody map[string]any
	}{
		{name: "tenant_id", attr: "tenant_id", value: alphaTenantID, wantBody: map[string]any{"tenant_id": alphaTenantID}},
		{name: "companygpt_handle", attr: "companygpt_handle", value: alphaHandle},
		{name: "azure_tenant_id", attr: "azure_tenant_id", value: alphaAzureTenantID},
		{name: "azure_tenant_domain", attr: "azure_tenant_domain", value: alphaTenantDomain},
		{name: "azure_subscription_id", attr: "azure_subscription_id", value: alphaSubscription},
		{name: "gcp_project_id", attr: "gcp_project_id", value: alphaGcpProject},
		{name: "aws_account_id", attr: "aws_account_id", value: alphaAwsAccount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newFakeHub(t)
			useFakeHubEnv(t)
			want := map[string]any{"name": "gw", "domain": "gpt.alpha.example", "cloud": "aws"}
			if tc.wantBody != nil {
				for k, v := range tc.wantBody {
					want[k] = v
				}
			} else {
				want["lookup"] = map[string]any{tc.attr: tc.value}
			}

			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testProviderFactories(),
				CheckDestroy:             checkEnvironmentGone(hub),
				Steps: []resource.TestStep{{
					Config: envConfig(hub, fmt.Sprintf("%s = %q", tc.attr, tc.value), "gw", "gpt.alpha.example", "aws"),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr(envAddr, "tenant_id", alphaTenantID),
						resource.TestCheckResourceAttr(envAddr, tc.attr, tc.value),
						func(*terraform.State) error {
							if got := hub.lastBody("POST /api/v1/admin/environments"); !reflect.DeepEqual(got, want) {
								return fmt.Errorf("POST body\n got: %v\nwant: %v", got, want)
							}
							return nil
						},
					),
				}},
			})
		})
	}
}

// Hub rejections surface their code plus a hint on how to fix them, and
// nothing ends up in state.
func TestEnvironmentCreateRejectedByHub(t *testing.T) {
	cases := []struct {
		name     string
		customer string
		seed     *fakeEnvironment
		wantErr  string
	}{
		{
			name:     "identifier unknown",
			customer: `azure_tenant_id = "00000000-0000-0000-0000-000000000000"`,
			wantErr:  `(?s)404 tenant_not_found.*No\s+customer\s+in\s+the\s+Hub\s+carries\s+this\s+identifier`,
		},
		{
			name:     "companygpt_handle unknown",
			customer: handleLine("nobody"),
			wantErr:  `(?s)404 tenant_not_found.*CompanyGPT\s+handle.*bind\s+the\s+environment\s+with\s+tenant_id`,
		},
		{
			name:     "identifier shared by two customers",
			customer: fmt.Sprintf("gcp_project_id = %q", sharedGcpProject),
			wantErr:  `(?s)409 ambiguous_lookup.*bind\s+the\s+environment\s+with\s+tenant_id`,
		},
		{
			name:     "environment already registered",
			customer: azureTenantLine(),
			seed: &fakeEnvironment{
				EnvironmentID: "env_exists01", TenantID: alphaTenantID, Name: "old",
				Domain: "gpt.alpha.example", Cloud: "azure", CreatedBy: "admin",
			},
			wantErr: `(?s)409 environment_exists.*terraform\s+import`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newFakeHub(t)
			useFakeHubEnv(t)
			if tc.seed != nil {
				hub.seedEnvironment(*tc.seed)
			}

			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testProviderFactories(),
				Steps: []resource.TestStep{{
					Config:      envConfig(hub, tc.customer, "gw", "gpt.alpha.example", "azure"),
					ExpectError: regexp.MustCompile(tc.wantErr),
				}},
			})
			wantLeft := 0
			if tc.seed != nil {
				wantLeft = 1
			}
			if ids := hub.environmentIDs(); len(ids) != wantLeft {
				t.Fatalf("expected %d environment(s) in the Hub, got %v", wantLeft, ids)
			}
		})
	}
}

// An environment deleted outside Terraform is dropped from state on refresh
// and planned for re-creation.
func TestEnvironmentDeletedOutsideTerraformIsRecreated(t *testing.T) {
	hub := newFakeHub(t)
	useFakeHubEnv(t)
	cfg := envConfig(hub, azureTenantLine(), "gw", "gpt.alpha.example", "azure")

	var id string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkEnvironmentGone(hub),
		Steps: []resource.TestStep{
			{
				Config: cfg,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(envAddr, "id", &id),
					func(*terraform.State) error { hub.removeEnvironment(id); return nil },
				),
				ExpectNonEmptyPlan: true,
			},
			{
				Config: cfg,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(envAddr, plancheck.ResourceActionCreate)},
				},
			},
		},
	})
}

// A proxy that echoes the bearer (and any licence key or token) into its
// error page must not leak them into Terraform diagnostics.
func TestEnvironmentDiagnosticsAreRedacted(t *testing.T) {
	hub := newFakeHub(t)
	useFakeHubEnv(t)
	hub.failAll(http.StatusBadGateway, "<html>echo Bearer "+testAdminKey+" "+secretLK+" "+secretJWT+" end</html>")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		Steps: []resource.TestStep{{
			Config: envConfig(hub, azureTenantLine(), "gw", "gpt.alpha.example", "azure"),
			// Each secret is replaced in place: a leak would put the secret,
			// not the placeholder, between "Bearer" and "end".
			ExpectError: regexp.MustCompile(`(?s)502.*echo\s+Bearer\s+ak_\*\*\*REDACTED\*\*\*\s+lk_\*\*\*REDACTED\*\*\*\s+\*\*\*REDACTED_JWT\*\*\*\s+end`),
		}},
	})
}

// A rejected PATCH surfaces the Hub's code and leaves state untouched, so a
// later run with the original config plans nothing.
func TestEnvironmentUpdateRejectedByHub(t *testing.T) {
	hub := newFakeHub(t)
	useFakeHubEnv(t)
	original := envConfig(hub, azureTenantLine(), "gw", "gpt.alpha.example", "azure")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkEnvironmentGone(hub),
		Steps: []resource.TestStep{
			{Config: original},
			{
				PreConfig: func() {
					hub.failMethod(http.MethodPatch, http.StatusConflict, `{"error":{"code":"domain_in_use","message":"domain belongs to another environment"}}`)
				},
				Config:      envConfig(hub, azureTenantLine(), "gw", "taken.alpha.example", "azure"),
				ExpectError: regexp.MustCompile(`(?s)Update environment failed.*409 domain_in_use`),
			},
			{
				PreConfig: func() { hub.failMethod("", 0, "") },
				Config:    original,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

// The CompanyGPT rollout by handle: the apply key ["read", "licensing"]
// looks the customer up and registers the environment by companygpt_handle;
// the plan key ["read_licensing"] looks it up again and plans nothing.
func TestEnvironmentByHandleWithLicensingKeys(t *testing.T) {
	hub := newFakeHub(t)
	useFakeHubEnv(t)
	cfg := func(key string) string {
		return tenantConfig(hub.providerBlockWithKey(key), handleLine(alphaHandle)) + fmt.Sprintf(`
resource "innfactoryhub_environment" "gw" {
  companygpt_handle = %q
  name              = "Alpha AI Gateway (${data.innfactoryhub_tenant.self.legal_name})"
  domain            = "gpt.alpha.example"
  cloud             = "azure"
}
`, alphaHandle)
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkEnvironmentGone(hub),
		Steps: []resource.TestStep{
			{
				Config: cfg(testLicensingKey),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(envAddr, "tenant_id", alphaTenantID),
					resource.TestCheckResourceAttr(envAddr, "companygpt_handle", alphaHandle),
					resource.TestCheckResourceAttr(envAddr, "name", "Alpha AI Gateway (Alpha Demo GmbH)"),
					resource.TestCheckNoResourceAttr(envAddr, "azure_tenant_id"),
					func(s *terraform.State) error {
						id := s.RootModule().Resources[envAddr].Primary.ID
						want := &fakeIdentifier{Type: "companygpt_handle", Value: alphaHandle}
						if e, _ := hub.environment(id); !reflect.DeepEqual(e.IdentifierUsed, want) {
							return fmt.Errorf("environment %s registered with %+v, want %+v", id, e.IdentifierUsed, want)
						}
						return nil
					},
					func(*terraform.State) error { hub.resetCalls(); return nil },
				),
			},
			{
				Config: cfg(testReadLicensingKey),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: func(*terraform.State) error {
					if w := hub.writeCalls(); len(w) != 0 {
						return fmt.Errorf("plan wrote: %v", w)
					}
					return nil
				},
			},
			// Destroy with the apply key; the read-only key cannot delete.
			{Config: cfg(testLicensingKey)},
		},
	})
}

// applyRemote takes the binding from identifier_used, so an import restores
// companygpt_handle like any other lookup identifier.
func TestEnvironmentApplyRemoteIdentifierUsed(t *testing.T) {
	cases := []struct {
		name       string
		used       *environmentIdentifier
		wantHandle types.String
		wantAzure  types.String
	}{
		{
			name:       "companygpt_handle",
			used:       &environmentIdentifier{Type: "companygpt_handle", Value: alphaHandle},
			wantHandle: types.StringValue(alphaHandle), wantAzure: types.StringNull(),
		},
		{
			name:       "cloud identifier",
			used:       &environmentIdentifier{Type: "azure_tenant_id", Value: alphaAzureTenantID},
			wantHandle: types.StringNull(), wantAzure: types.StringValue(alphaAzureTenantID),
		},
		{
			name:       "tenant_id clears every lookup",
			used:       &environmentIdentifier{Type: "tenant_id", Value: alphaTenantID},
			wantHandle: types.StringNull(), wantAzure: types.StringNull(),
		},
		{
			name:       "unknown kind keeps state",
			used:       &environmentIdentifier{Type: "stackit_project_id", Value: "x"},
			wantHandle: types.StringValue(betaHandle), wantAzure: types.StringNull(),
		},
		{
			name:       "no identifier_used keeps state",
			wantHandle: types.StringValue(betaHandle), wantAzure: types.StringNull(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := environmentModel{CompanygptHandle: types.StringValue(betaHandle), AzureTenantID: types.StringNull()}
			m.applyRemote(environmentDTO{
				EnvironmentID: "env_1", TenantID: alphaTenantID, Name: "gw", Domain: "gpt.alpha.example", Cloud: "azure",
				IdentifierUsed: tc.used,
			})
			if !m.CompanygptHandle.Equal(tc.wantHandle) || !m.AzureTenantID.Equal(tc.wantAzure) {
				t.Fatalf("companygpt_handle %v, azure_tenant_id %v; want %v, %v", m.CompanygptHandle, m.AzureTenantID, tc.wantHandle, tc.wantAzure)
			}
		})
	}
}
