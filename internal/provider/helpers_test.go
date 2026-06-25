package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestNullableString(t *testing.T) {
	// Empty string must normalize to null (the core drift fix).
	empty := nullableString("")
	if !empty.IsNull() {
		t.Errorf("nullableString(\"\") should be null, got %q", empty.ValueString())
	}

	// Non-empty keeps its value.
	v := nullableString("engineering")
	if v.IsNull() {
		t.Error("nullableString(\"engineering\") should not be null")
	}
	if v.ValueString() != "engineering" {
		t.Errorf("expected \"engineering\", got %q", v.ValueString())
	}

	// Sanity: function returns a String type.
	var _ types.String = nullableString("x")
}
