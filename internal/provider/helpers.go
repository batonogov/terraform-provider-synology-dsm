package provider

import "github.com/hashicorp/terraform-plugin-framework/types"

// nullableString returns a null string when the value is empty, otherwise a
// string value. This normalizes the "" vs null mismatch for optional string
// attributes: DSM returns an empty string for attributes that were not set,
// but a Terraform config that omits such an attribute represents it as null.
// Without this normalization, refreshing a resource whose optional string was
// not configured produces a spurious "update in-place" diff of "" -> null.
func nullableString(s string) types.String {
	if s == "" {
		return types.StringNull()
	}
	return types.StringValue(s)
}
