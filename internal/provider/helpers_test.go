package provider

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// The handle format is the Hub's: trimmed, lower case, 1–40 characters of
// a-z, 0-9 and inner hyphens.
func TestCompanygptHandleFormat(t *testing.T) {
	cases := []struct {
		value string
		valid bool
	}{
		{value: "innfactory", valid: true},
		{value: "lmu", valid: true},
		{value: "ksob", valid: true},
		{value: "a", valid: true},
		{value: "7", valid: true},
		{value: "alpha-staging", valid: true},
		{value: "a--b", valid: true},
		{value: "x1-2y", valid: true},
		{value: strings.Repeat("a", 40), valid: true},
		{value: strings.Repeat("a", 41)},
		{value: ""},
		{value: "-alpha"},
		{value: "alpha-"},
		{value: "-"},
		{value: "Alpha"},
		{value: "ALPHA"},
		{value: " alpha"},
		{value: "alpha "},
		{value: "alpha\n"},
		{value: "al pha"},
		{value: "alpha_prod"},
		{value: "alpha.prod"},
		{value: "älpha"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%q", tc.value), func(t *testing.T) {
			var resp validator.StringResponse
			companygptHandle.ValidateString(context.Background(), validator.StringRequest{ConfigValue: types.StringValue(tc.value)}, &resp)
			if got := !resp.Diagnostics.HasError(); got != tc.valid {
				t.Fatalf("valid = %v, want %v (%v)", got, tc.valid, resp.Diagnostics)
			}
		})
	}
}
