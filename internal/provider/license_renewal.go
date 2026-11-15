package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
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
// the portal no longer changes Terraform's licences. An unbilled licence that
// expires soon or has expired gets a longer end planned (an in-place update:
// same licence, same key); a licence suspended in the portal only warns —
// that is an emergency stop Terraform must not undo. Trials are never renewed. A licence on a contract
// position follows its contract and is never touched. Ended licences and
// revoked keys are handled through rotation_trigger (see ModifyPlan).
func (r *licenseResource) planRenewal(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse, state, plan licenseModel) {
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
	// A contract position decides the term; nothing to renew from Terraform.
	if !plan.ContractPositionID.IsNull() || !state.ContractPositionID.IsNull() {
		return
	}
	unbilledReason, ok := knownString(plan.UnbilledReason)
	if !ok || !plan.UnbilledReason.Equal(state.UnbilledReason) {
		return // billing link being changed by the configuration itself
	}
	current := state.UnbilledMaxUntil.ValueString()
	if !plan.UnbilledMaxUntil.IsUnknown() && !plan.UnbilledMaxUntil.Equal(state.UnbilledMaxUntil) {
		return // the configuration already sets a new end
	}
	if unbilledReason == "trial" {
		planTrialEnd(resp, state, current, reason)
		return
	}
	limit, ok := unbilledLimit(unbilledReason, renewalClock())
	if !ok {
		return
	}
	next := limit.Format(isoDateLayout)
	if next <= current {
		return // already at the longest term: nothing a renewal could add
	}

	what := "expires on " + current
	if reason == renewalExpired {
		what = "expired on " + current
	}
	var configured types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("unbilled_max_until"), &configured)...)
	switch {
	case !configured.IsNull():
		// A date the operator pinned is theirs: a short trial may be
		// intended, so this only warns.
		resp.Diagnostics.AddAttributeWarning(path.Root("unbilled_max_until"), "Unbilled licence ends soon",
			fmt.Sprintf("Licence %s %s (%s) and unbilled_max_until is set to %s in the configuration, so Terraform does not renew it. "+
				"Set a later unbilled_max_until (at most %s for %s), or remove the attribute to let Terraform renew it.",
				state.ID.ValueString(), what, unbilledReason, configured.ValueString(), next, unbilledReason))
	case !plan.AutoRenewUnbilled.IsNull() && !plan.AutoRenewUnbilled.ValueBool():
		resp.Diagnostics.AddAttributeError(path.Root("auto_renew_unbilled"), "Unbilled licence needs renewing",
			fmt.Sprintf("Licence %s %s (%s) and auto_renew_unbilled is false. Set auto_renew_unbilled = true to let Terraform extend it to %s, "+
				"set unbilled_max_until to a later date (at most %s), or put the licence on a contract position (contract_position_id).",
				state.ID.ValueString(), what, unbilledReason, next, next))
	default:
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("unbilled_max_until"), types.StringValue(next))...)
		// The framework marks Hub-computed values unknown for a change it
		// sees in the configuration; this change is ours, so do it here.
		for _, name := range []string{"status", "valid_until", "renewal_reason"} {
			resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root(name), types.StringUnknown())...)
		}
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("renewal_due"), types.BoolUnknown())...)
		resp.Diagnostics.AddAttributeWarning(path.Root("unbilled_max_until"), "Renewing an unbilled licence",
			fmt.Sprintf("Licence %s %s (%s). This apply extends it to %s in place; the licence key stays. "+
				"Set auto_renew_unbilled = false to stop that, or contract_position_id to bill it.", state.ID.ValueString(), what, unbilledReason, next))
	}
}

// planTrialEnd never renews a trial: one that is renewed over and over would
// be a free licence nobody notices. It warns before the end and fails after.
func planTrialEnd(resp *resource.ModifyPlanResponse, state licenseModel, end, reason string) {
	const action = "bind it to a contract position (contract_position_id) or deliberately set unbilled_max_until"
	if reason == renewalExpired {
		resp.Diagnostics.AddAttributeError(path.Root("unbilled_max_until"), "Trial licence expired",
			fmt.Sprintf("Trial licence %s expired on %s. Terraform does not renew trials: %s.", state.ID.ValueString(), end, action))
		return
	}
	resp.Diagnostics.AddAttributeWarning(path.Root("unbilled_max_until"), "Trial licence ends soon",
		fmt.Sprintf("Trial licence %s ends on %s. Terraform does not renew trials: %s.", state.ID.ValueString(), end, action))
}

// planBillingLink keeps the AI Hub's billing link. Staff may link a licence
// to a contract position in the portal after Terraform issued it, and
// Terraform must not undo that: while the Hub has a position and the
// configuration names none (it still says `unbilled_reason`), the plan keeps
// the Hub's position (unbilled_reason as configured is ignored) and only warns. A configuration that names a different
// position is taken over. The billing attributes are computed, so every
// case sets them explicitly (state is nil for a create).
func planBillingLink(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse, state *licenseModel) {
	var cfgPosition, cfgReason types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("contract_position_id"), &cfgPosition)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("unbilled_reason"), &cfgReason)...)
	if resp.Diagnostics.HasError() || cfgPosition.IsUnknown() || cfgReason.IsUnknown() {
		return // known after apply: nothing to compare
	}
	set := func(name string, v types.String) {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root(name), v)...)
	}
	switch {
	case !cfgPosition.IsNull():
		// A position in the configuration: taken over (the same as the Hub's
		// changes nothing).
		set("contract_position_id", cfgPosition)
		set("unbilled_reason", types.StringNull())
		set("unbilled_max_until", types.StringNull())
	case state != nil && !state.ContractPositionID.IsNull() && state.Status.ValueString() != licenseStatusEnded:
		// unbilled_reason stays as configured (Terraform requires it); Read keeps
		// it in state while the Hub's position wins, so nothing differs.
		set("contract_position_id", state.ContractPositionID)
		if !cfgReason.IsNull() {
			resp.Diagnostics.AddAttributeWarning(path.Root("unbilled_reason"), "Billing is linked to a contract position in the AI Hub – Terraform leaves it unchanged",
				fmt.Sprintf("Licence %s is linked to contract position %s in the AI Hub (set in the portal). unbilled_reason = %q in the configuration is ignored. "+
					"Set contract_position_id = %q to match, or a different position to relink it.", state.ID.ValueString(), state.ContractPositionID.ValueString(),
					cfgReason.ValueString(), state.ContractPositionID.ValueString()))
		}
	default:
		set("contract_position_id", types.StringNull())
	}
}
