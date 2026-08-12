package provider

import (
	"reflect"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

// TestEveryResourceStoresTheClient walks the provider's own registration lists
// rather than a hand-written one, so a resource added later is covered without
// anyone remembering to add it here.
//
// This exists because the type handed to Configure is checked at runtime, not by
// the compiler. When ProviderData changed from *client.Client to
// *dsmProviderData, seven resources and data sources kept the old assertion:
// everything still built, every existing test still passed, and each of them
// would have failed with "Unexpected Provider Data" on the first real apply.
func TestEveryResourceStoresTheClient(t *testing.T) {
	providerData := &dsmProviderData{
		client:             client.NewClient("https://nas:5001", "admin", "", false),
		allowTaskExecution: true,
	}

	p := New("test")().(*synologyProvider)

	for _, newResource := range p.Resources(t.Context()) {
		r := newResource()
		configurable, ok := r.(resource.ResourceWithConfigure)
		if !ok {
			t.Errorf("%T does not implement ResourceWithConfigure, so it can never reach DSM", r)
			continue
		}

		resp := &resource.ConfigureResponse{}
		configurable.Configure(t.Context(), resource.ConfigureRequest{ProviderData: providerData}, resp)
		if resp.Diagnostics.HasError() {
			t.Errorf("%T rejected the provider data: %v", r, resp.Diagnostics)
			continue
		}
		assertClientStored(t, configurable)
	}

	for _, newDataSource := range p.DataSources(t.Context()) {
		d := newDataSource()
		configurable, ok := d.(datasource.DataSourceWithConfigure)
		if !ok {
			t.Errorf("%T does not implement DataSourceWithConfigure, so it can never reach DSM", d)
			continue
		}

		resp := &datasource.ConfigureResponse{}
		configurable.Configure(t.Context(), datasource.ConfigureRequest{ProviderData: providerData}, resp)
		if resp.Diagnostics.HasError() {
			t.Errorf("%T rejected the provider data: %v", d, resp.Diagnostics)
			continue
		}
		assertClientStored(t, configurable)
	}
}

// TestEveryResourceToleratesNilProviderData covers the other call Terraform
// makes: during validation it configures with nil ProviderData. A resource that
// treats that as an error would fail every plan, and one that dereferences it
// would panic.
func TestEveryResourceToleratesNilProviderData(t *testing.T) {
	p := New("test")().(*synologyProvider)

	for _, newResource := range p.Resources(t.Context()) {
		r := newResource()
		if configurable, ok := r.(resource.ResourceWithConfigure); ok {
			resp := &resource.ConfigureResponse{}
			configurable.Configure(t.Context(), resource.ConfigureRequest{ProviderData: nil}, resp)
			if resp.Diagnostics.HasError() {
				t.Errorf("%T errored on nil provider data: %v", r, resp.Diagnostics)
			}
		}
	}

	for _, newDataSource := range p.DataSources(t.Context()) {
		d := newDataSource()
		if configurable, ok := d.(datasource.DataSourceWithConfigure); ok {
			resp := &datasource.ConfigureResponse{}
			configurable.Configure(t.Context(), datasource.ConfigureRequest{ProviderData: nil}, resp)
			if resp.Diagnostics.HasError() {
				t.Errorf("%T errored on nil provider data: %v", d, resp.Diagnostics)
			}
		}
	}
}

// assertClientStored reads the unexported client field by reflection. Every
// resource keeps it under the same name, and checking the stored value is the
// only way to tell "accepted the provider data" from "actually wired up".
func assertClientStored(t *testing.T, target any) {
	t.Helper()

	value := reflect.ValueOf(target)
	if value.Kind() == reflect.Ptr {
		value = value.Elem()
	}
	field := value.FieldByName("client")
	if !field.IsValid() {
		t.Errorf("%T has no client field", target)
		return
	}
	if field.IsNil() {
		t.Errorf("%T accepted the provider data but stored no client", target)
	}
}
