package provider

import (
	"context"

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
