package provider

import (
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

// dsmVolumePath matches an absolute volume path such as /volume1/docker. File
// Station APIs address the same directory as /docker, so a volume path in a
// share_path is always a mistake worth catching at plan time.
var dsmVolumePath = regexp.MustCompile(`^/volume[0-9]+(?:/|$)`)

// nullableString returns a null string when the value is empty, otherwise a
// string value. This normalizes the "" vs null mismatch for optional string
// attributes: DSM returns an empty string for attributes that were not set,
// but a Terraform config that omits such an attribute represents it as null.
// Without this normalization, refreshing a resource whose optional string was
// not configured produces a spurious "update in-place" diff of "" -> null.
//
// Tradeoff: because DSM cannot represent an intentionally-empty string vs an
// unset one, a config that explicitly sets description/email to "" will also
// be normalized to null and show a perpetual diff. This is the accepted
// tradeoff for fixing the far more common omitted-attribute case.
func nullableString(s string) types.String {
	if s == "" {
		return types.StringNull()
	}
	return types.StringValue(s)
}
