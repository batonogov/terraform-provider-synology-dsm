package provider

import (
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
)

// TestPosixModeString pins the rendering of DSM's mode number. DSM prints the
// octal digits as a decimal number, so the value is formatted, never converted:
// treating 755 as a bitmask would render it as "1363".
func TestPosixModeString(t *testing.T) {
	tests := []struct {
		mode int
		want string
	}{
		{mode: 0, want: "000"},
		{mode: 644, want: "644"},
		{mode: 755, want: "755"},
		{mode: 777, want: "777"},
		// Setuid, setgid and sticky bits arrive as a fourth digit and are kept.
		{mode: 1777, want: "1777"},
	}

	for _, tt := range tests {
		if got := posixModeString(tt.mode); got != tt.want {
			t.Errorf("posixModeString(%d) = %q, want %q", tt.mode, got, tt.want)
		}
	}
}

func TestPosixPermissionsModel_Apply(t *testing.T) {
	var model posixPermissionsModel
	model.apply(&client.PathPermissions{
		PosixMode: 750,
		IsACLMode: true,
		Owner:     "terraform",
		Group:     "users",
		UID:       1027,
		GID:       100,
	})

	if model.PosixMode.ValueString() != "750" || !model.ACLMode.ValueBool() {
		t.Errorf("mode/acl not applied: %+v", model)
	}
	if model.PosixOwner.ValueString() != "terraform" || model.PosixGroup.ValueString() != "users" {
		t.Errorf("ownership not applied: %+v", model)
	}
	if model.PosixUID.ValueInt64() != 1027 || model.PosixGID.ValueInt64() != 100 {
		t.Errorf("uid/gid not applied: %+v", model)
	}
}

// TestPosixPermissionsModel_ApplyNilClearsPrevious matters on refresh: a model
// carrying values from an earlier read must not keep reporting them once DSM
// stops answering, or state would assert a mode nobody can verify.
func TestPosixPermissionsModel_ApplyNilClearsPrevious(t *testing.T) {
	var model posixPermissionsModel
	model.apply(&client.PathPermissions{PosixMode: 755, Owner: "root", UID: 0})
	model.apply(nil)

	if !model.PosixMode.IsNull() || !model.PosixOwner.IsNull() || !model.PosixGroup.IsNull() {
		t.Errorf("string attributes must be null: %+v", model)
	}
	if !model.PosixUID.IsNull() || !model.PosixGID.IsNull() || !model.ACLMode.IsNull() {
		t.Errorf("numeric and bool attributes must be null: %+v", model)
	}
}

// TestPosixPermissionAttributes_MatchAcrossSchemas guards the duplication: the
// resource and data source halves are written out separately because the
// framework keeps them in different packages, and an attribute added to one and
// forgotten in the other would leave the shared model unmappable.
func TestPosixPermissionAttributes_MatchAcrossSchemas(t *testing.T) {
	resourceAttrs := posixPermissionAttributes()
	dataSourceAttrs := posixPermissionDataSourceAttributes()

	if len(resourceAttrs) != len(dataSourceAttrs) {
		t.Fatalf("attribute counts differ: %d resource, %d data source", len(resourceAttrs), len(dataSourceAttrs))
	}
	for name, attr := range resourceAttrs {
		other, ok := dataSourceAttrs[name]
		if !ok {
			t.Errorf("%q is missing from the data source schema", name)
			continue
		}
		if !attr.GetType().Equal(other.GetType()) {
			t.Errorf("%q has type %s in the resource and %s in the data source", name, attr.GetType(), other.GetType())
		}
		if attr.GetDescription() != other.GetDescription() {
			t.Errorf("%q is described differently in the two schemas", name)
		}
	}
}
