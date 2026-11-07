package provider

import (
	"context"
	"errors"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// strList wraps types.ListValueFrom for []string with the boilerplate of
// appending the diag and returning a null list on error.
func strList(ctx context.Context, diags *diag.Diagnostics, in []string) types.List {
	v, d := types.ListValueFrom(ctx, types.StringType, in)
	diags.Append(d...)
	if d.HasError() {
		return types.ListNull(types.StringType)
	}
	return v
}

// withHint renders err for a diagnostic and, when the Hub answered with an
// error code listed in hints, appends the matching remediation text.
func withHint(err error, hints map[string]string) string {
	msg := err.Error()
	var ae *apiError
	if errors.As(err, &ae) {
		if hint, ok := hints[ae.Code]; ok {
			msg += "\n\n" + hint
		}
	}
	return msg
}
