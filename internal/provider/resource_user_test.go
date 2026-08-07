package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func userSchema(t *testing.T) resource.SchemaResponse {
	t.Helper()

	r := NewUserResource()
	resp := resource.SchemaResponse{}
	r.Schema(t.Context(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned errors: %v", resp.Diagnostics)
	}
	return resp
}

func TestUserResource_Schema(t *testing.T) {
	resp := userSchema(t)

	if diags := resp.Schema.ValidateImplementation(t.Context()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %v", diags)
	}

	attrs := resp.Schema.GetAttributes()

	// password must NOT be required: DSM never returns it, so a required
	// attribute leaves `terraform import` with a permanently dirty plan.
	pwd, ok := attrs["password"]
	if !ok {
		t.Fatal("missing attribute password")
	}
	if pwd.IsRequired() {
		t.Error("password must be optional so imported users do not carry a dirty plan")
	}
	if !pwd.IsSensitive() {
		t.Error("password must be sensitive")
	}

	expire, ok := attrs["expire_date"]
	if !ok {
		t.Fatal("missing attribute expire_date")
	}
	if !expire.IsOptional() {
		t.Error("expire_date must be optional")
	}

	twoFactor, ok := attrs["two_factor_enabled"]
	if !ok {
		t.Fatal("missing attribute two_factor_enabled")
	}
	if !twoFactor.IsComputed() {
		t.Error("two_factor_enabled must be computed: DSM manages 2FA elsewhere")
	}
	if twoFactor.IsOptional() {
		t.Error("two_factor_enabled must not be settable")
	}
}

// userConfig builds a tfsdk.Config carrying just the attributes the validator
// looks at.
func userConfig(t *testing.T, disabled *bool, expireDate *string, password *string) tfsdk.Config {
	t.Helper()

	sch := userSchema(t).Schema
	objType := sch.Type().TerraformType(context.Background()).(tftypes.Object)

	values := map[string]tftypes.Value{}
	for name, typ := range objType.AttributeTypes {
		switch {
		case name == "name":
			values[name] = tftypes.NewValue(tftypes.String, "john.doe")
		case name == "disabled" && disabled != nil:
			values[name] = tftypes.NewValue(tftypes.Bool, *disabled)
		case name == "expire_date" && expireDate != nil:
			values[name] = tftypes.NewValue(tftypes.String, *expireDate)
		case name == "password" && password != nil:
			values[name] = tftypes.NewValue(tftypes.String, *password)
		default:
			values[name] = tftypes.NewValue(typ, nil)
		}
	}

	return tfsdk.Config{Schema: sch, Raw: tftypes.NewValue(objType, values)}
}

// TestUserResource_ValidateConfig covers the rules DSM's single "expired" field
// implies: an account cannot be both switched off and carry a date, and the
// date has to be in the one format DSM accepts.
func TestUserResource_ValidateConfig(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }
	strPtr := func(s string) *string { return &s }

	tests := []struct {
		name       string
		disabled   *bool
		expireDate *string
		wantError  bool
	}{
		{name: "plain account"},
		{name: "disabled only", disabled: boolPtr(true)},
		{name: "expiry date only", expireDate: strPtr("2027-03-05")},
		{name: "disabled false with a date", disabled: boolPtr(false), expireDate: strPtr("2027-03-05")},
		{name: "disabled with a date is rejected", disabled: boolPtr(true), expireDate: strPtr("2027-03-05"), wantError: true},
		{name: "DSM-style date is rejected", expireDate: strPtr("2027/3/5"), wantError: true},
		{name: "nonsense date is rejected", expireDate: strPtr("tomorrow"), wantError: true},
		{name: "reversed date is rejected", expireDate: strPtr("05-03-2027"), wantError: true},
	}

	r := NewUserResource().(*userResource)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &resource.ValidateConfigResponse{}
			r.ValidateConfig(
				t.Context(),
				resource.ValidateConfigRequest{Config: userConfig(t, tt.disabled, tt.expireDate, nil)},
				resp,
			)

			if tt.wantError && !resp.Diagnostics.HasError() {
				t.Fatal("expected a validation error")
			}
			if !tt.wantError && resp.Diagnostics.HasError() {
				t.Fatalf("unexpected validation error: %v", resp.Diagnostics)
			}
		})
	}
}

// TestUserResource_ModifyPlan_PasswordOnCreate pins the compromise that makes
// import clean: password is optional in the schema but still mandatory when the
// user is actually being created.
func TestUserResource_ModifyPlan_PasswordOnCreate(t *testing.T) {
	strPtr := func(s string) *string { return &s }
	sch := userSchema(t).Schema
	objType := sch.Type().TerraformType(context.Background()).(tftypes.Object)
	nullState := tfsdk.State{Schema: sch, Raw: tftypes.NewValue(objType, nil)}

	tests := []struct {
		name      string
		password  *string
		wantError bool
	}{
		{name: "create with a password", password: strPtr("secret")},
		{name: "create without a password is rejected", wantError: true},
	}

	r := NewUserResource().(*userResource)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := userConfig(t, nil, nil, tt.password)
			resp := &resource.ModifyPlanResponse{}
			r.ModifyPlan(
				t.Context(),
				resource.ModifyPlanRequest{
					State: nullState,
					Plan:  tfsdk.Plan{Schema: sch, Raw: cfg.Raw},
				},
				resp,
			)

			if tt.wantError && !resp.Diagnostics.HasError() {
				t.Fatal("expected an error about the missing password")
			}
			if !tt.wantError && resp.Diagnostics.HasError() {
				t.Fatalf("unexpected error: %v", resp.Diagnostics)
			}
		})
	}
}

// TestUserResource_ModifyPlan_NoPasswordNeededOnUpdate confirms the check only
// fires on create — an imported user with no password in config must still be
// updatable.
func TestUserResource_ModifyPlan_NoPasswordNeededOnUpdate(t *testing.T) {
	sch := userSchema(t).Schema
	objType := sch.Type().TerraformType(context.Background()).(tftypes.Object)
	cfg := userConfig(t, nil, nil, nil)

	// A non-null state means this is an update, not a create.
	existingState := tfsdk.State{Schema: sch, Raw: cfg.Raw}
	_ = objType

	r := NewUserResource().(*userResource)
	resp := &resource.ModifyPlanResponse{}
	r.ModifyPlan(
		t.Context(),
		resource.ModifyPlanRequest{
			State: existingState,
			Plan:  tfsdk.Plan{Schema: sch, Raw: cfg.Raw},
		},
		resp,
	)

	if resp.Diagnostics.HasError() {
		t.Fatalf("updating a user without a password in config must be allowed: %v", resp.Diagnostics)
	}
}

// TestUserResource_ModifyPlan_DestroyIsIgnored guards against demanding a
// password while tearing the resource down.
func TestUserResource_ModifyPlan_DestroyIsIgnored(t *testing.T) {
	sch := userSchema(t).Schema
	objType := sch.Type().TerraformType(context.Background()).(tftypes.Object)
	cfg := userConfig(t, nil, nil, nil)

	r := NewUserResource().(*userResource)
	resp := &resource.ModifyPlanResponse{}
	r.ModifyPlan(
		t.Context(),
		resource.ModifyPlanRequest{
			State: tfsdk.State{Schema: sch, Raw: cfg.Raw},
			Plan:  tfsdk.Plan{Schema: sch, Raw: tftypes.NewValue(objType, nil)},
		},
		resp,
	)

	if resp.Diagnostics.HasError() {
		t.Fatalf("destroy must not require a password: %v", resp.Diagnostics)
	}
}
