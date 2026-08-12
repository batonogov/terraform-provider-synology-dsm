package provider

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

func TestSystemSettingsResource_Metadata(t *testing.T) {
	r := NewSystemSettingsResource()

	resp := &resource.MetadataResponse{}
	r.Metadata(t.Context(), resource.MetadataRequest{ProviderTypeName: "dsm"}, resp)

	if resp.TypeName != "dsm_system_settings" {
		t.Errorf("expected type name dsm_system_settings, got %q", resp.TypeName)
	}
}

func TestSystemSettingsResource_Schema(t *testing.T) {
	r := NewSystemSettingsResource()

	resp := &resource.SchemaResponse{}
	r.Schema(t.Context(), resource.SchemaRequest{}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned errors: %v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(t.Context()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %v", diags)
	}

	attrs := resp.Schema.GetAttributes()

	// Every managed attribute is Optional+Computed: DSM always has a value for
	// each of them, and a config that omits one must keep DSM's rather than
	// showing a perpetual diff.
	for _, name := range []string{"timezone", "ntp_enabled", "ntp_server"} {
		attr, ok := attrs[name]
		if !ok {
			t.Errorf("missing attribute %q", name)
			continue
		}
		if attr.IsRequired() {
			t.Errorf("attribute %q must not be required: every setting is optional", name)
		}
		if !attr.IsOptional() {
			t.Errorf("attribute %q must be optional", name)
		}
		if !attr.IsComputed() {
			t.Errorf("attribute %q must be computed: DSM supplies a value when the config omits it", name)
		}
	}

	idAttr, ok := attrs["id"]
	if !ok {
		t.Fatal("missing attribute id")
	}
	if !idAttr.IsComputed() || idAttr.IsRequired() || idAttr.IsOptional() {
		t.Error("id must be computed only")
	}
}

func TestSystemSettingsResource_Configure_NilProviderData(t *testing.T) {
	r := &systemSettingsResource{}

	resp := &resource.ConfigureResponse{}
	r.Configure(t.Context(), resource.ConfigureRequest{}, resp)

	if resp.Diagnostics.HasError() {
		t.Errorf("nil provider data must be tolerated, got: %v", resp.Diagnostics)
	}
	if r.client != nil {
		t.Error("client should stay nil")
	}
}

func TestSystemSettingsResource_Configure_WrongType(t *testing.T) {
	r := &systemSettingsResource{}

	resp := &resource.ConfigureResponse{}
	r.Configure(t.Context(), resource.ConfigureRequest{ProviderData: "not a client"}, resp)

	if !resp.Diagnostics.HasError() {
		t.Error("expected an error for the wrong provider data type")
	}
}

// TestSystemSettingsResource_DeleteIsNoOp pins the documented behaviour: there
// is no sane value to reset the NAS clock configuration to, so destroy only
// drops the resource from state. A nil client would panic if Delete called DSM.
func TestSystemSettingsResource_DeleteIsNoOp(t *testing.T) {
	r := &systemSettingsResource{}

	resp := &resource.DeleteResponse{}
	r.Delete(t.Context(), resource.DeleteRequest{}, resp)

	if resp.Diagnostics.HasError() {
		t.Errorf("delete must not fail: %v", resp.Diagnostics)
	}
}

func TestSystemSettingsDataSource_Metadata(t *testing.T) {
	d := NewSystemSettingsDataSource()

	resp := &datasource.MetadataResponse{}
	d.Metadata(t.Context(), datasource.MetadataRequest{ProviderTypeName: "dsm"}, resp)

	if resp.TypeName != "dsm_system_settings" {
		t.Errorf("expected type name dsm_system_settings, got %q", resp.TypeName)
	}
}

func TestSystemSettingsDataSource_Schema(t *testing.T) {
	d := NewSystemSettingsDataSource()

	resp := &datasource.SchemaResponse{}
	d.Schema(t.Context(), datasource.SchemaRequest{}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned errors: %v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(t.Context()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %v", diags)
	}

	attrs := resp.Schema.GetAttributes()
	for _, name := range []string{"id", "timezone", "ntp_enabled", "ntp_server", "current_date", "current_time", "timestamp"} {
		attr, ok := attrs[name]
		if !ok {
			t.Errorf("missing attribute %q", name)
			continue
		}
		if !attr.IsComputed() {
			t.Errorf("data source attribute %q must be computed", name)
		}
	}
}

func TestSystemSettingsDataSource_Configure_WrongType(t *testing.T) {
	d := &systemSettingsDataSource{}

	resp := &datasource.ConfigureResponse{}
	d.Configure(t.Context(), datasource.ConfigureRequest{ProviderData: 42}, resp)

	if !resp.Diagnostics.HasError() {
		t.Error("expected an error for the wrong provider data type")
	}
}

// stubZoneLister stands in for DSM's listzone method. zones == nil with err set
// covers the firmware where the call is unavailable.
type stubZoneLister struct {
	zones []client.Timezone
	err   error
	calls int
}

func (s *stubZoneLister) ListTimezones(_ context.Context) ([]client.Timezone, error) {
	s.calls++
	return s.zones, s.err
}

func dsmZones() []client.Timezone {
	return []client.Timezone{
		{Value: "Amman", Offset: 10800},
		{Value: "Moscow", Offset: 10800},
		{Value: "New York", Offset: -18000},
	}
}

// TestSystemSettingsErrorDetail_NoZoneList covers the degraded path: DSM either
// has no listzone or refused it, so the hint falls back to reading the shape of
// the name the user supplied.
func TestSystemSettingsErrorDetail_NoZoneList(t *testing.T) {
	tests := []struct {
		name       string
		lister     timezoneLister
		err        error
		timezone   string
		wantSubstr []string
		notSubstr  []string
	}{
		{
			name:     "5701 with an IANA-looking zone suggests the DSM name",
			lister:   nil,
			err:      &client.APIError{Code: 5701, API: "SYNO.Core.Region.NTP"},
			timezone: "Europe/Moscow",
			wantSubstr: []string{
				"5701",
				`Try "Moscow" instead of "Europe/Moscow"`,
			},
		},
		{
			name:       "5701 with an underscored IANA zone spells it out",
			lister:     &stubZoneLister{err: errors.New("listzone unavailable")},
			err:        &client.APIError{Code: 5701, API: "SYNO.Core.Region.NTP"},
			timezone:   "America/New_York",
			wantSubstr: []string{`Try "New York"`},
		},
		{
			name:       "5701 with a plain zone points at Control Panel",
			lister:     nil,
			err:        &client.APIError{Code: 5701, API: "SYNO.Core.Region.NTP"},
			timezone:   "Nowhere",
			wantSubstr: []string{"Regional Options"},
			notSubstr:  []string{"instead of"},
		},
		{
			name:       "5701 without a requested zone still corrects the wording",
			lister:     nil,
			err:        &client.APIError{Code: 5701, API: "SYNO.Core.Region.NTP"},
			timezone:   "",
			wantSubstr: []string{"does not mean the session expired"},
			notSubstr:  []string{"Regional Options"},
		},
		{
			name:       "119 explains the built-in admin restriction",
			lister:     nil,
			err:        &client.APIError{Code: 119, API: "SYNO.Core.Region.NTP"},
			timezone:   "Moscow",
			wantSubstr: []string{"built-in administrator account"},
		},
		{
			name:       "other codes are passed through verbatim",
			lister:     nil,
			err:        &client.APIError{Code: 105, API: "SYNO.Core.Region.NTP"},
			timezone:   "Europe/Moscow",
			wantSubstr: []string{"105"},
			notSubstr:  []string{"Try", "session expired"},
		},
		{
			name:       "non-API errors are passed through verbatim",
			lister:     nil,
			err:        errors.New("connection refused"),
			timezone:   "Europe/Moscow",
			wantSubstr: []string{"connection refused"},
			notSubstr:  []string{"Try"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := systemSettingsErrorDetail(t.Context(), tt.lister, tt.err, tt.timezone)
			for _, want := range tt.wantSubstr {
				if !strings.Contains(got, want) {
					t.Errorf("detail should contain %q, got: %s", want, got)
				}
			}
			for _, unwanted := range tt.notSubstr {
				if strings.Contains(got, unwanted) {
					t.Errorf("detail should not contain %q, got: %s", unwanted, got)
				}
			}
		})
	}
}

// TestSystemSettingsErrorDetail_WithZoneList is the good path: DSM can list its
// own zones, so the diagnostic stops guessing and quotes the real name.
func TestSystemSettingsErrorDetail_WithZoneList(t *testing.T) {
	tests := []struct {
		name       string
		timezone   string
		wantSubstr []string
		notSubstr  []string
	}{
		{
			name:       "IANA identifier is resolved against the real list",
			timezone:   "Europe/Moscow",
			wantSubstr: []string{`does not know the time zone "Europe/Moscow"`, `contains "Moscow"`},
		},
		{
			name:       "underscored IANA identifier is resolved too",
			timezone:   "America/New_York",
			wantSubstr: []string{`contains "New York"`},
		},
		{
			name:       "a valid zone points the blame elsewhere",
			timezone:   "Moscow",
			wantSubstr: []string{"does recognise the time zone", "NTP server"},
			notSubstr:  []string{"does not know"},
		},
		{
			name:       "an unrecognisable zone says so instead of guessing",
			timezone:   "Atlantis",
			wantSubstr: []string{"nothing in its list is close"},
			notSubstr:  []string{"does recognise"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lister := &stubZoneLister{zones: dsmZones()}
			got := systemSettingsErrorDetail(t.Context(), lister,
				&client.APIError{Code: 5701, API: "SYNO.Core.Region.NTP"}, tt.timezone)

			if lister.calls != 1 {
				t.Errorf("listzone called %d times, want 1", lister.calls)
			}
			for _, want := range tt.wantSubstr {
				if !strings.Contains(got, want) {
					t.Errorf("detail should contain %q, got: %s", want, got)
				}
			}
			for _, unwanted := range tt.notSubstr {
				if strings.Contains(got, unwanted) {
					t.Errorf("detail should not contain %q, got: %s", unwanted, got)
				}
			}
		})
	}
}

// TestSystemSettingsErrorDetail_ZoneListNotConsultedForOtherCodes keeps the
// enrichment off the path of errors it cannot explain — a failing NAS should not
// field an extra request per diagnostic.
func TestSystemSettingsErrorDetail_ZoneListNotConsultedForOtherCodes(t *testing.T) {
	lister := &stubZoneLister{zones: dsmZones()}

	systemSettingsErrorDetail(t.Context(), lister, &client.APIError{Code: 105, API: "SYNO.Core.Region.NTP"}, "Moscow")

	if lister.calls != 0 {
		t.Errorf("listzone called %d times for a non-5701 error, want 0", lister.calls)
	}
}

// TestSystemSettingsErrorDetail_WrappedError proves the hint keys off the error
// code rather than the rendered message, so wrapping in the client layer does
// not lose it.
func TestSystemSettingsErrorDetail_WrappedError(t *testing.T) {
	wrapped := errors.Join(errors.New("set system settings"), &client.APIError{Code: 5701, API: "SYNO.Core.Region.NTP"})

	got := systemSettingsErrorDetail(t.Context(), nil, wrapped, "Europe/Berlin")
	if !strings.Contains(got, `Try "Berlin"`) {
		t.Errorf("expected the timezone hint through a wrapped error, got: %s", got)
	}
}

func TestSuggestDSMTimezone(t *testing.T) {
	tests := []struct {
		in    string
		want  string
		wantK bool
	}{
		{"Europe/Moscow", "Moscow", true},
		{"America/New_York", "New York", true},
		{"America/Argentina/Buenos_Aires", "Buenos Aires", true},
		{"Moscow", "", false},
		{"", "", false},
		{"Europe/", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, ok := suggestDSMTimezone(tt.in)
			if ok != tt.wantK || got != tt.want {
				t.Errorf("suggestDSMTimezone(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.wantK)
			}
		})
	}
}
