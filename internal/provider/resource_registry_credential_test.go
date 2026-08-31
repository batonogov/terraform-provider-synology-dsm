package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"reflect"
)

func TestRegistryCredentialResource_Metadata(t *testing.T) {
	r := NewRegistryCredentialResource()

	req := resource.MetadataRequest{
		ProviderTypeName: "dsm",
	}
	resp := &resource.MetadataResponse{}

	r.Metadata(t.Context(), req, resp)

	if resp.TypeName != "dsm_registry_credential" {
		t.Errorf("expected type name dsm_registry_credential, got %q", resp.TypeName)
	}
}

func TestRegistryCredentialResource_Schema(t *testing.T) {
	r := NewRegistryCredentialResource()

	req := resource.SchemaRequest{}
	resp := &resource.SchemaResponse{}

	r.Schema(t.Context(), req, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned errors: %v", resp.Diagnostics)
	}

	attrs := resp.Schema.GetAttributes()

	for _, attr := range []string{"url", "name", "username", "password_wo", "password_wo_version"} {
		if _, ok := attrs[attr]; !ok {
			t.Errorf("required attribute %q missing", attr)
		}
	}

	// Секрет обязан быть write-only: в состоянии ему не место.
	password, ok := attrs["password_wo"]
	if !ok {
		t.Fatal("password_wo missing")
	}
	if !password.IsSensitive() {
		t.Errorf("password_wo must be sensitive")
	}

	// Пароль нельзя прочитать из DSM, а update-вызова у реестров нет:
	// изменяемые атрибуты обязаны заменять запись.
	// Конструктор возвращает unexported тип — сравниваем через reflect.
	want := reflect.TypeOf(stringplanmodifier.RequiresReplace())
	for _, name := range []string{"url", "name", "username"} {
		attr := attrs[name].(schema.StringAttribute)
		found := false
		for _, m := range attr.PlanModifiers {
			if reflect.TypeOf(m) == want {
				found = true
			}
		}
		if !found {
			t.Errorf("attribute %q must carry RequiresReplace (DSM has no update call)", name)
		}
	}
	if trust := attrs["enable_trust_self_signed"].(schema.BoolAttribute); len(trust.PlanModifiers) == 0 {
		t.Errorf("enable_trust_self_signed must carry RequiresReplace (DSM has no update call)")
	}
}
