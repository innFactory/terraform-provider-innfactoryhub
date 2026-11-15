package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

// Reasons of the AI Hub's `renewal.reason` the provider acts on.
const (
	renewalExpiresSoon = "expires_soon"
	renewalExpired     = "expired"
	renewalSuspended   = "suspended"
)

// addMonthsClamped is today plus n months with the day of the month clamped
// to the target month (31 Jan + 1 month = 28/29 Feb), as the Hub counts —
// time.AddDate would roll over into the next month and overshoot the limit.
func addMonthsClamped(t time.Time, months int) time.Time {
	first := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, months, 0)
	last := first.AddDate(0, 1, -1).Day()
	return time.Date(first.Year(), first.Month(), min(t.Day(), last), 0, 0, 0, 0, time.UTC)
}

// unbilledLimit is the latest `unbilled.max_until` the Hub accepts for a
// reason when counted from today: partner and internal 12
// months (trial 60 days is only the Hub's limit; trials are not renewed). today is the UTC date, never ahead of the Hub's Berlin date, so
// the result is never beyond the Hub's limit.
func unbilledLimit(reason string, today time.Time) (time.Time, bool) {
	switch reason {
	case "trial":
		return today.AddDate(0, 0, 60), true
	case "partner", "internal":
		return addMonthsClamped(today, 12), true
	}
	return time.Time{}, false
}

// renewalClock is the date renewals count from; tests replace it.
var renewalClock = func() time.Time {
	n := time.Now().UTC()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

// planRenewal acts on the AI Hub's renewal verdict (Spec 003 §5.3), because
// the portal does not extend licences issued by Terraform. It decides by the
// billing link IN THE HUB (read into private state), never by what Terraform
// applied or what the configuration says:
//   - the Hub has a contract position: never renewed, never an error (the term
//     follows the contract); an expired one only warns;
//   - the Hub has a trial: never renewed; expires_soon warns; expired is an
//     error only if the configuration is trial too and the Hub link is the one
//     Terraform applied, otherwise a warning;
//   - the Hub has partner or internal: renewed with the Hub's reason (position
//     and reason are never changed). unbilled_max_until is planned unknown and
//     counted by Update on the day of the apply, so a saved plan survives
//     midnight. A pin in the configuration prevents it only while the Hub link
//     is the applied one; a date staff set in the portal is no pin.
//
// A licence suspended in the portal only warns (emergency stop). Ended
// licences and revoked keys are handled through rotation_trigger (see
// ModifyPlan). Renewal happens only when Terraform plans and applies inside
// the Hub's renewal window (14 days): plan and apply at least weekly.
func (r *licenseResource) planRenewal(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse, state, plan licenseModel) {
	setRenewalMarker(ctx, resp, "")
	reason := state.RenewalReason.ValueString()
	if reason == renewalSuspended && state.Status.ValueString() == "suspended" {
		resp.Diagnostics.AddAttributeWarning(path.Root("status"), "Licence suspended in the AI Hub – Terraform does not intervene",
			fmt.Sprintf("Licence %s was suspended in the portal (an emergency stop). Terraform neither resumes nor renews it. "+
				"Resume it in the portal when the reason is gone.", state.ID.ValueString()))
		return
	}
	if reason != renewalExpiresSoon && reason != renewalExpired {
		return
	}
	expired := reason == renewalExpired
	applied := appliedLink(state)
	hubLink, ok := readHubLink(ctx, req.Private)
	if !ok {
		hubLink = applied
	}
	current := state.ValidUntil.ValueString()
	if current == "" {
		current = deref(applied.MaxUntil)
	}
	what := "ends on " + current
	if expired {
		what = "expired on " + current
	}
	if hubLink.Position != nil {
		// The term follows the contract: nothing for Terraform to renew.
		if expired {
			resp.Diagnostics.AddAttributeWarning(path.Root("status"), "Contract or position expired – clarify in the portal",
				fmt.Sprintf("Licence %s hangs on contract position %s in the AI Hub and %s. Its term follows the contract; Terraform does not renew it. "+
					"Clarify the contract or the position in the portal.", state.ID.ValueString(), *hubLink.Position, what))
		}
		return
	}
	if hubLink.Reason == nil {
		return
	}
	hubReason := *hubLink.Reason
	// The configuration is changing the billing link itself: that applies, no renewal now.
	if !plan.ContractPositionID.Equal(state.ContractPositionID) || !plan.UnbilledReason.Equal(state.UnbilledReason) ||
		(!plan.UnbilledMaxUntil.IsUnknown() && !plan.UnbilledMaxUntil.Equal(state.UnbilledMaxUntil)) {
		return
	}
	var cfgUntil types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("unbilled_max_until"), &cfgUntil)...)
	if cfgUntil.IsUnknown() {
		return
	}
	cfg, _ := configuredBilling(ctx, req.Config)
	matches := applied.equal(hubLink)

	if hubReason == "trial" {
		cfgTrial := cfg.Reason != nil && *cfg.Reason == "trial" && cfg.Position == nil
		planTrialEnd(resp, state, what, expired, expired && cfgTrial && matches)
		return
	}
	limit, ok := unbilledLimit(hubReason, renewalClock())
	if !ok {
		return
	}
	next := limit.Format(isoDateLayout)
	if next <= current {
		return // already at the longest term: nothing a renewal could add
	}

	// The ways out are the same wherever Terraform does not renew.
	options := fmt.Sprintf("set unbilled_max_until to a later date (at most %s for %s), or put the licence on a contract position (contract_position_id)", next, hubReason)
	summary := "Unbilled licence ends soon"
	if expired {
		summary = "Unbilled licence expired"
	}
	switch {
	case cfg.MaxUntil != nil && matches:
		// A date the operator pinned (and Terraform applied) is theirs.
		diagnose(resp, expired, path.Root("unbilled_max_until"), summary,
			fmt.Sprintf("Licence %s %s (%s) and unbilled_max_until is pinned to %s in the configuration, so Terraform does not renew it. "+
				"Remove the attribute to let Terraform renew it, or %s.", state.ID.ValueString(), what, hubReason, *cfg.MaxUntil, options))
	case !plan.AutoRenewUnbilled.IsNull() && !plan.AutoRenewUnbilled.ValueBool():
		diagnose(resp, expired, path.Root("auto_renew_unbilled"), summary,
			fmt.Sprintf("Licence %s %s (%s) and auto_renew_unbilled is false. Set auto_renew_unbilled = true to let Terraform extend it, or %s.",
				state.ID.ValueString(), what, hubReason, options))
	default:
		// The date is computed in Update, at apply time.
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("unbilled_max_until"), types.StringUnknown())...)
		// The framework marks Hub-computed values unknown for a change it
		// sees in the configuration; this change is ours, so do it here.
		for _, name := range []string{"status", "valid_until", "renewal_reason"} {
			resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root(name), types.StringUnknown())...)
		}
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("renewal_due"), types.BoolUnknown())...)
		setRenewalMarker(ctx, resp, hubReason)
		resp.Diagnostics.AddAttributeWarning(path.Root("unbilled_max_until"), "Renewing an unbilled licence",
			fmt.Sprintf("Licence %s %s (%s in the AI Hub). This apply extends it in place — expected %s, counted on the day of the apply; the licence key, "+
				"reason and billing link stay. Set auto_renew_unbilled = false to stop that, or contract_position_id to bill it.",
				state.ID.ValueString(), what, hubReason, next))
	}
}

// setRenewalMarker tells Update (through private state) that the plan renews
// the licence with the Hub's reason ("" = no renewal).
func setRenewalMarker(ctx context.Context, resp *resource.ModifyPlanResponse, reason string) {
	if resp.Private == nil {
		return
	}
	raw, _ := json.Marshal(reason)
	resp.Diagnostics.Append(resp.Private.SetKey(ctx, privateRenewal, raw)...)
}

// renewalReasonOf reads the marker (in Update); "" means no renewal.
func renewalReasonOf(ctx context.Context, priv privateReader) string {
	if priv == nil {
		return ""
	}
	raw, _ := priv.GetKey(ctx, privateRenewal)
	var reason string
	if len(raw) == 0 || json.Unmarshal(raw, &reason) != nil {
		return ""
	}
	return reason
}

// diagnose adds an error (expired) or a warning (ends soon).
func diagnose(resp *resource.ModifyPlanResponse, isError bool, p path.Path, summary, detail string) {
	if isError {
		resp.Diagnostics.AddAttributeError(p, summary, detail)
		return
	}
	resp.Diagnostics.AddAttributeWarning(p, summary, detail)
}

// planTrialEnd never renews a trial: one that is renewed over and over would
// be a free licence nobody notices. It warns before the end; after it, it
// fails only if asError (the configuration is a trial too and the Hub link is
// the applied one), otherwise it warns.
func planTrialEnd(resp *resource.ModifyPlanResponse, state licenseModel, what string, expired, asError bool) {
	limit, _ := unbilledLimit("trial", renewalClock())
	action := fmt.Sprintf("bind it to a contract position (contract_position_id), or set unbilled_max_until deliberately (at most %s, 60 days from today), "+
		"or have staff link or extend it in the portal", limit.Format(isoDateLayout))
	summary := "Trial licence ends soon"
	if expired {
		summary = "Trial licence expired"
	}
	diagnose(resp, asError, path.Root("unbilled_max_until"), summary,
		fmt.Sprintf("Trial licence %s %s. Terraform does not renew trials: %s.", state.ID.ValueString(), what, action))
}

// billingConfig is the billing link as last configured (and applied) by
// Terraform, kept in private state. It tells a change of the configuration
// (which wins) from a change staff made in the portal (which Terraform
// leaves alone). Nil values are unset attributes.
type billingConfig struct {
	Position *string `json:"position"`
	Reason   *string `json:"reason"`
	MaxUntil *string `json:"max_until"`
}

const (
	privateBillingConfig = "billing_config" // billingConfig as last configured
	privateHubBilling    = "hub_billing"    // text: the Hub's link, when it differs from state
	privateHubLink       = "hub_link"       // billingConfig: the Hub's billing link as Read saw it
	privateRenewal       = "renewal"        // the Hub's reason, when the plan renews the licence
	privateImported      = "imported"       // set by import: no remembered configuration yet
)

type privateReader interface {
	GetKey(ctx context.Context, key string) ([]byte, diag.Diagnostics)
}

type privateWriter interface {
	SetKey(ctx context.Context, key string, value []byte) diag.Diagnostics
}

func stringPtr(v types.String) *string {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	s := v.ValueString()
	return &s
}

func equalStrPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func (b billingConfig) equal(o billingConfig) bool {
	return equalStrPtr(b.Position, o.Position) && equalStrPtr(b.Reason, o.Reason) && equalStrPtr(b.MaxUntil, o.MaxUntil)
}

// configuredBilling reads the billing attributes from a configuration.
func configuredBilling(ctx context.Context, cfg interface {
	GetAttribute(context.Context, path.Path, any) diag.Diagnostics
}) (billingConfig, diag.Diagnostics) {
	var pos, reason, until types.String
	var diags diag.Diagnostics
	diags.Append(cfg.GetAttribute(ctx, path.Root("contract_position_id"), &pos)...)
	diags.Append(cfg.GetAttribute(ctx, path.Root("unbilled_reason"), &reason)...)
	diags.Append(cfg.GetAttribute(ctx, path.Root("unbilled_max_until"), &until)...)
	return billingConfig{Position: stringPtr(pos), Reason: stringPtr(reason), MaxUntil: stringPtr(until)}, diags
}

// rememberBillingConfig stores the configured billing link in private state
// (Create and Update).
func rememberBillingConfig(ctx context.Context, cfg interface {
	GetAttribute(context.Context, path.Path, any) diag.Diagnostics
}, priv privateWriter, diags *diag.Diagnostics) {
	b, d := configuredBilling(ctx, cfg)
	diags.Append(d...)
	if d.HasError() {
		return
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return
	}
	diags.Append(priv.SetKey(ctx, privateBillingConfig, raw)...)
}

// lastBillingConfig reads what rememberBillingConfig stored; ok is false for
// state from before it existed (or an import).
func lastBillingConfig(ctx context.Context, priv privateReader) (b billingConfig, ok bool) {
	raw, _ := priv.GetKey(ctx, privateBillingConfig)
	if len(raw) == 0 || json.Unmarshal(raw, &b) != nil {
		return billingConfig{}, false
	}
	return b, true
}

// describeLink is the billing link of a licence as text.
func describeLink(position, reason, maxUntil *string) string {
	switch {
	case position != nil:
		return "contract position " + *position
	case reason != nil:
		return "unbilled (" + *reason + ") until " + deref(maxUntil)
	}
	return "no billing link"
}

func deref(s *string) string {
	if s == nil {
		return "-"
	}
	return *s
}

// planBillingLink keeps the AI Hub's billing link. Staff may change it in the
// portal after Terraform issued the licence (link a position, set another
// reason) and Terraform must not undo that: as long as the configuration is
// the one Terraform applied last (private state), the Hub's link stays — Read
// then keeps the applied link in state and records the Hub's in private
// state, so the plan is empty and only warns about the difference. A
// changed configuration wins. Without private state (state from before it
// existed, or an import) the rule is simpler: while the Hub has a position
// and the configuration names none, the Hub's position stays and the plan
// warns. The billing attributes are computed, so every case sets them
// explicitly (state is nil for a create).
func planBillingLink(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse, state *licenseModel) {
	cfg, d := configuredBilling(ctx, req.Config)
	resp.Diagnostics.Append(d...)
	var cfgPosition, cfgReason types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("contract_position_id"), &cfgPosition)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("unbilled_reason"), &cfgReason)...)
	if resp.Diagnostics.HasError() || cfgPosition.IsUnknown() || cfgReason.IsUnknown() {
		return // known after apply: nothing to compare
	}
	set := func(name string, v types.String) {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root(name), v)...)
	}
	var last billingConfig
	var hasLast bool
	if state != nil && req.Private != nil {
		last, hasLast = lastBillingConfig(ctx, req.Private)
	}
	switch {
	case !cfgPosition.IsNull():
		// A position in the configuration is taken over (the Hub's own
		// position changes nothing); an unchanged one that staff moved is kept
		// by Read, so it is equal to state here.
		set("contract_position_id", cfgPosition)
		set("unbilled_reason", types.StringNull())
		set("unbilled_max_until", types.StringNull())
	case state == nil || state.Status.ValueString() == licenseStatusEnded:
		set("contract_position_id", types.StringNull())
	case hasLast && last.equal(cfg):
		// Configuration unchanged: whatever the state holds stays.
		set("contract_position_id", state.ContractPositionID)
	case hasLast:
		// Configuration changed to unbilled: it wins over a position.
		set("contract_position_id", types.StringNull())
		// A pin removed from the configuration: the end follows the Hub (no
		// write; state takes the Hub's date).
		if last.MaxUntil != nil && cfg.MaxUntil == nil && cfg.Reason != nil && equalStrPtr(last.Reason, cfg.Reason) {
			if hl, ok := readHubLink(ctx, req.Private); ok && hl.Position == nil && equalStrPtr(hl.Reason, cfg.Reason) && hl.MaxUntil != nil {
				set("unbilled_max_until", types.StringValue(*hl.MaxUntil))
			}
		}
	case !state.ContractPositionID.IsNull():
		// No private state: the Hub's position wins over a reason.
		set("contract_position_id", state.ContractPositionID)
		resp.Diagnostics.AddAttributeWarning(path.Root("unbilled_reason"), "Billing is linked to a contract position in the AI Hub – Terraform leaves it unchanged",
			fmt.Sprintf("Licence %s is linked to contract position %s in the AI Hub (set in the portal). unbilled_reason = %q in the configuration is ignored. "+
				"Set contract_position_id = %q to match, or a different position to relink it.", state.ID.ValueString(), state.ContractPositionID.ValueString(),
				cfgReason.ValueString(), state.ContractPositionID.ValueString()))
	default:
		set("contract_position_id", types.StringNull())
	}
	// Differences between the Hub and what Terraform applied (set by Read).
	if state != nil && req.Private != nil {
		if hub := hubBillingDiff(ctx, req.Private); hub != "" && len(resp.RequiresReplace) > 0 {
			resp.Diagnostics.AddWarning("Replacing the licence drops a billing link set in the portal",
				fmt.Sprintf("Licence %s: the AI Hub has %s (changed in the portal). The replacement is issued from the configuration, so that link is dropped.",
					state.ID.ValueString(), hub))
		} else if hub != "" {
			resp.Diagnostics.AddWarning("Billing in the AI Hub differs from the configuration – Terraform leaves it unchanged",
				fmt.Sprintf("Licence %s: the AI Hub has %s (changed in the portal); Terraform applied %s. It stays that way until you change the billing "+
					"attributes in the configuration.", state.ID.ValueString(), hub,
					describeLink(stringPtr(state.ContractPositionID), stringPtr(state.UnbilledReason), stringPtr(state.UnbilledMaxUntil))))
		}
	}
}

// logHubBillingDiff records, for the log only, a difference between the Hub's
// billing link and the applied one.
func logHubBillingDiff(ctx context.Context, id, hub string) {
	tflog.Info(ctx, "license_billing_link_differs", map[string]any{"license_id": id, "hub": hub})
}

// hubBillingDiff reads what Read recorded: the Hub's billing link as text
// when it differs from the applied one, else "".
func hubBillingDiff(ctx context.Context, priv privateReader) string {
	raw, _ := priv.GetKey(ctx, privateHubBilling)
	var text string
	if len(raw) == 0 || json.Unmarshal(raw, &text) != nil {
		return ""
	}
	return text
}

// recordHubBillingDiff stores the Hub's link as text (JSON string), "" when
// there is no difference.
func recordHubBillingDiff(ctx context.Context, priv privateWriter, text string, diags *diag.Diagnostics) {
	raw, _ := json.Marshal(text)
	diags.Append(priv.SetKey(ctx, privateHubBilling, raw)...)
}

// hubLinkOf is the billing link of a licence as the Hub reports it.
func hubLinkOf(l licenseDTO) billingConfig {
	b := billingConfig{Position: l.ContractPositionID}
	if l.Unbilled != nil {
		reason, until := l.Unbilled.Reason, l.Unbilled.MaxUntil
		b.Reason, b.MaxUntil = &reason, &until
	}
	return b
}

// appliedLink is the billing link state holds (what Terraform applied).
func appliedLink(m licenseModel) billingConfig {
	return billingConfig{Position: stringPtr(m.ContractPositionID), Reason: stringPtr(m.UnbilledReason), MaxUntil: stringPtr(m.UnbilledMaxUntil)}
}

// linkDiverged reports whether the Hub's link differs from the applied one in
// a way that counts: another position, another reason of an unbilled
// licence, or — for an end Terraform pinned — another end.
func linkDiverged(applied, hub, last billingConfig) bool {
	if !equalStrPtr(hub.Position, applied.Position) {
		return true
	}
	if hub.Reason != nil && !equalStrPtr(hub.Reason, applied.Reason) {
		return true
	}
	return last.MaxUntil != nil && hub.MaxUntil != nil && applied.MaxUntil != nil && *hub.MaxUntil != *applied.MaxUntil
}

// recordHubLink stores the Hub's link (renewal decides by it) and, when it
// differs from the applied one, its text for the plan's warning.
func recordHubLink(ctx context.Context, priv privateWriter, hub licenseDTO, differs bool, diags *diag.Diagnostics) {
	if priv == nil {
		return
	}
	hl := hubLinkOf(hub)
	raw, _ := json.Marshal(hl)
	diags.Append(priv.SetKey(ctx, privateHubLink, raw)...)
	text := ""
	if differs {
		text = describeLink(hl.Position, hl.Reason, hl.MaxUntil)
		logHubBillingDiff(ctx, hub.LicenseID, text)
	}
	recordHubBillingDiff(ctx, priv, text, diags)
}

// readHubLink reads what recordHubLink stored.
func readHubLink(ctx context.Context, priv privateReader) (billingConfig, bool) {
	if priv == nil {
		return billingConfig{}, false
	}
	raw, _ := priv.GetKey(ctx, privateHubLink)
	var b billingConfig
	if len(raw) == 0 || json.Unmarshal(raw, &b) != nil {
		return billingConfig{}, false
	}
	return b, true
}

// rememberedFromState derives the configuration to remember for state that
// has none (written by an earlier version): the reason as configured, a
// position only where no reason stands next to it (a position next to a
// reason is the Hub's, kept by the simple rule). The end is not remembered —
// the configuration may or may not pin it.
func rememberedFromState(m licenseModel) billingConfig {
	b := billingConfig{Reason: stringPtr(m.UnbilledReason)}
	if b.Reason == nil {
		b.Position = stringPtr(m.ContractPositionID)
	}
	return b
}

func storeBillingConfig(ctx context.Context, priv privateWriter, b billingConfig, diags *diag.Diagnostics) {
	if priv == nil {
		return
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return
	}
	diags.Append(priv.SetKey(ctx, privateBillingConfig, raw)...)
}

func privateFlag(ctx context.Context, priv privateReader, key string) bool {
	if priv == nil {
		return false
	}
	raw, _ := priv.GetKey(ctx, key)
	return len(raw) > 0
}

// lastBillingConfigOf is the configuration as it stands (for recording after
// an update, where it is what was just applied).
func lastBillingConfigOf(ctx context.Context, cfg interface {
	GetAttribute(context.Context, path.Path, any) diag.Diagnostics
}) (billingConfig, bool) {
	b, d := configuredBilling(ctx, cfg)
	return b, !d.HasError()
}
