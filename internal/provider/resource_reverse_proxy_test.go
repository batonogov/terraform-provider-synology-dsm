package provider

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func reverseProxySchema(t *testing.T) resource.SchemaResponse {
	t.Helper()
	r := NewReverseProxyResource()
	resp := resource.SchemaResponse{}
	r.Schema(t.Context(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned errors: %v", resp.Diagnostics)
	}
	return resp
}

func TestReverseProxyResource_Schema(t *testing.T) {
	resp := reverseProxySchema(t)
	if diags := resp.Schema.ValidateImplementation(t.Context()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %v", diags)
	}

	attrs := resp.Schema.GetAttributes()
	if attr := attrs["description"]; attr == nil || !attr.IsRequired() {
		t.Error("description must be required")
	}
	for _, name := range []string{"websocket", "http2", "hsts", "proxy_connect_timeout", "proxy_read_timeout", "proxy_send_timeout", "proxy_intercept_errors"} {
		attr := attrs[name]
		if attr == nil || !attr.IsOptional() || !attr.IsComputed() {
			t.Errorf("%s must be optional and computed so it can carry a default", name)
		}
	}
	for _, name := range []string{"custom_headers", "access_control_profile"} {
		if attr := attrs[name]; attr == nil || !attr.IsOptional() || attr.IsRequired() {
			t.Errorf("%s must be optional", name)
		}
	}
	for _, name := range []string{"id", "access_control_profile_id"} {
		if attr := attrs[name]; attr == nil || !attr.IsComputed() {
			t.Errorf("%s must be computed", name)
		}
	}

	blocks := resp.Schema.GetBlocks()
	for _, name := range []string{"source", "destination"} {
		block, ok := blocks[name]
		if !ok {
			t.Fatalf("%s must be a nested block, matching the HCL in the feature request", name)
		}
		nested := block.GetNestedObject().GetAttributes()
		for _, attribute := range []string{"protocol", "hostname", "port"} {
			if attr := nested[attribute]; attr == nil || !attr.IsRequired() {
				t.Errorf("%s.%s must be required", name, attribute)
			}
		}
	}
}

func TestReverseProxyResource_MetadataAndConfigure(t *testing.T) {
	r := NewReverseProxyResource().(*reverseProxyResource)
	metadata := &resource.MetadataResponse{}
	r.Metadata(t.Context(), resource.MetadataRequest{ProviderTypeName: "dsm"}, metadata)
	if metadata.TypeName != "dsm_reverse_proxy" {
		t.Errorf("type name = %q, want dsm_reverse_proxy", metadata.TypeName)
	}

	wrong := &resource.ConfigureResponse{}
	r.Configure(t.Context(), resource.ConfigureRequest{ProviderData: "wrong"}, wrong)
	if !wrong.Diagnostics.HasError() {
		t.Fatal("expected diagnostic for wrong provider data")
	}

	r = NewReverseProxyResource().(*reverseProxyResource)
	empty := &resource.ConfigureResponse{}
	r.Configure(t.Context(), resource.ConfigureRequest{}, empty)
	if empty.Diagnostics.HasError() || r.client != nil {
		t.Fatalf("nil provider data should be ignored: %v", empty.Diagnostics)
	}
}

type reverseProxyEndpointOpts struct {
	protocol string
	hostname string
	port     int64
}

type reverseProxyConfigOpts struct {
	description    string
	source         *reverseProxyEndpointOpts
	destination    *reverseProxyEndpointOpts
	websocket      bool
	http2          bool
	hsts           bool
	customHeaders  map[string]string
	connectTimeout *int64
}

func defaultReverseProxyConfigOpts() reverseProxyConfigOpts {
	return reverseProxyConfigOpts{
		description: "Nextcloud",
		source:      &reverseProxyEndpointOpts{protocol: "HTTPS", hostname: "cloud.example.com", port: 443},
		destination: &reverseProxyEndpointOpts{protocol: "HTTP", hostname: "localhost", port: 8080},
	}
}

func reverseProxyConfig(t *testing.T, opts reverseProxyConfigOpts) tfsdk.Config {
	t.Helper()
	sch := reverseProxySchema(t).Schema
	objType := sch.Type().TerraformType(t.Context()).(tftypes.Object)

	endpoint := func(typ tftypes.Type, e *reverseProxyEndpointOpts) tftypes.Value {
		if e == nil {
			return tftypes.NewValue(typ, nil)
		}
		return tftypes.NewValue(typ, map[string]tftypes.Value{
			"protocol": tftypes.NewValue(tftypes.String, e.protocol),
			"hostname": tftypes.NewValue(tftypes.String, e.hostname),
			"port":     tftypes.NewValue(tftypes.Number, e.port),
		})
	}

	values := map[string]tftypes.Value{}
	for name, typ := range objType.AttributeTypes {
		switch name {
		case "description":
			values[name] = tftypes.NewValue(tftypes.String, opts.description)
		case "source":
			values[name] = endpoint(typ, opts.source)
		case "destination":
			values[name] = endpoint(typ, opts.destination)
		case "websocket":
			values[name] = tftypes.NewValue(tftypes.Bool, opts.websocket)
		case "http2":
			values[name] = tftypes.NewValue(tftypes.Bool, opts.http2)
		case "hsts":
			values[name] = tftypes.NewValue(tftypes.Bool, opts.hsts)
		case "custom_headers":
			if opts.customHeaders == nil {
				values[name] = tftypes.NewValue(typ, nil)
				continue
			}
			headers := map[string]tftypes.Value{}
			for key, value := range opts.customHeaders {
				headers[key] = tftypes.NewValue(tftypes.String, value)
			}
			values[name] = tftypes.NewValue(typ, headers)
		case "proxy_connect_timeout":
			if opts.connectTimeout == nil {
				values[name] = tftypes.NewValue(typ, nil)
				continue
			}
			values[name] = tftypes.NewValue(tftypes.Number, *opts.connectTimeout)
		default:
			values[name] = tftypes.NewValue(typ, nil)
		}
	}
	return tfsdk.Config{Schema: sch, Raw: tftypes.NewValue(objType, values)}
}

func TestReverseProxyResource_ValidateConfig(t *testing.T) {
	zero := int64(0)
	tests := []struct {
		name      string
		mutate    func(*reverseProxyConfigOpts)
		wantError string
	}{
		{name: "valid"},
		{
			name:   "lowercase protocol is accepted",
			mutate: func(o *reverseProxyConfigOpts) { o.source.protocol = "https"; o.destination.protocol = "http" },
		},
		{
			name:      "blank description",
			mutate:    func(o *reverseProxyConfigOpts) { o.description = "  " },
			wantError: "Empty description",
		},
		{
			name:      "missing source block",
			mutate:    func(o *reverseProxyConfigOpts) { o.source = nil },
			wantError: "Missing required block",
		},
		{
			name:      "missing destination block",
			mutate:    func(o *reverseProxyConfigOpts) { o.destination = nil },
			wantError: "Missing required block",
		},
		{
			name:      "unsupported protocol",
			mutate:    func(o *reverseProxyConfigOpts) { o.destination.protocol = "tcp" },
			wantError: "Invalid protocol",
		},
		{
			name:      "hostname carrying a scheme",
			mutate:    func(o *reverseProxyConfigOpts) { o.source.hostname = "https://cloud.example.com" },
			wantError: "Invalid hostname",
		},
		{
			name:      "hostname carrying a port",
			mutate:    func(o *reverseProxyConfigOpts) { o.destination.hostname = "localhost:8080" },
			wantError: "Invalid hostname",
		},
		{
			name:      "port out of range",
			mutate:    func(o *reverseProxyConfigOpts) { o.source.port = 70000 },
			wantError: "Invalid port",
		},
		{
			name:      "hsts on an http source",
			mutate:    func(o *reverseProxyConfigOpts) { o.source.protocol = "HTTP"; o.hsts = true },
			wantError: "Requires an HTTPS source",
		},
		{
			name:      "http2 on an http source",
			mutate:    func(o *reverseProxyConfigOpts) { o.source.protocol = "HTTP"; o.http2 = true },
			wantError: "Requires an HTTPS source",
		},
		{
			name:   "forwarded headers are valid",
			mutate: func(o *reverseProxyConfigOpts) { o.customHeaders = map[string]string{"X-Forwarded-Proto": "$scheme"} },
		},
		{
			name:      "header name with an invalid character",
			mutate:    func(o *reverseProxyConfigOpts) { o.customHeaders = map[string]string{"X_Forwarded_Proto": "$scheme"} },
			wantError: "Invalid header name",
		},
		{
			name: "websocket header duplicated by hand",
			mutate: func(o *reverseProxyConfigOpts) {
				o.websocket = true
				o.customHeaders = map[string]string{"Upgrade": "$http_upgrade"}
			},
			wantError: "Header conflicts with websocket",
		},
		{
			name:   "websocket header without the websocket flag is allowed",
			mutate: func(o *reverseProxyConfigOpts) { o.customHeaders = map[string]string{"Upgrade": "$http_upgrade"} },
		},
		{
			name:      "zero timeout",
			mutate:    func(o *reverseProxyConfigOpts) { o.connectTimeout = &zero },
			wantError: "Invalid timeout",
		},
	}

	r := NewReverseProxyResource().(*reverseProxyResource)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := defaultReverseProxyConfigOpts()
			if tt.mutate != nil {
				tt.mutate(&opts)
			}
			resp := &resource.ValidateConfigResponse{}
			r.ValidateConfig(t.Context(), resource.ValidateConfigRequest{Config: reverseProxyConfig(t, opts)}, resp)

			if tt.wantError == "" {
				if resp.Diagnostics.HasError() {
					t.Fatalf("unexpected error: %v", resp.Diagnostics)
				}
				return
			}
			if !resp.Diagnostics.HasError() {
				t.Fatalf("expected an error containing %q", tt.wantError)
			}
			if !strings.Contains(diagnosticsText(resp.Diagnostics), tt.wantError) {
				t.Fatalf("diagnostics %v do not mention %q", resp.Diagnostics, tt.wantError)
			}
		})
	}
}

func diagnosticsText(diags diag.Diagnostics) string {
	var builder strings.Builder
	for _, d := range diags {
		builder.WriteString(d.Summary())
		builder.WriteString("\n")
		builder.WriteString(d.Detail())
		builder.WriteString("\n")
	}
	return builder.String()
}

func TestReverseProxyHeaders_RoundTrip(t *testing.T) {
	headers := buildReverseProxyHeaders(true, map[string]string{
		"X-Real-IP":         "$remote_addr",
		"X-Forwarded-Proto": "$scheme",
	})

	// The websocket pair goes first, then custom headers sorted by name, so the
	// payload does not churn between applies.
	want := []client.ReverseProxyHeader{
		{Name: "Upgrade", Value: "$http_upgrade"},
		{Name: "Connection", Value: "$connection_upgrade"},
		{Name: "X-Forwarded-Proto", Value: "$scheme"},
		{Name: "X-Real-IP", Value: "$remote_addr"},
	}
	if !reflect.DeepEqual(headers, want) {
		t.Fatalf("headers = %+v, want %+v", headers, want)
	}

	websocket, custom := splitReverseProxyHeaders(headers)
	if !websocket {
		t.Error("the websocket preset should be recognised on the way back")
	}
	if !reflect.DeepEqual(custom, map[string]string{"X-Forwarded-Proto": "$scheme", "X-Real-IP": "$remote_addr"}) {
		t.Errorf("custom headers = %+v", custom)
	}
}

func TestSplitReverseProxyHeaders_PartialPresetIsNotWebSocket(t *testing.T) {
	// Only half the preset is present, so it is a hand-written header rather than
	// DSM's websocket toggle and must survive as a custom header.
	websocket, custom := splitReverseProxyHeaders([]client.ReverseProxyHeader{
		{Name: "Upgrade", Value: "$http_upgrade"},
	})
	if websocket {
		t.Error("a single upgrade header is not the websocket preset")
	}
	if custom["Upgrade"] != "$http_upgrade" {
		t.Errorf("custom = %+v, want the header preserved", custom)
	}
}

func TestPreserveProtocolCase(t *testing.T) {
	// DSM stores the protocol as an integer, so the configured spelling has to be
	// carried through or every plan would show a diff.
	if got := preserveProtocolCase(types.StringValue("https"), "HTTPS"); got.ValueString() != "https" {
		t.Errorf("got %q, want the configured lowercase spelling back", got.ValueString())
	}
	if got := preserveProtocolCase(types.StringNull(), "HTTPS"); got.ValueString() != "HTTPS" {
		t.Errorf("got %q, want the canonical spelling after import", got.ValueString())
	}
	if got := preserveProtocolCase(types.StringValue("https"), "HTTP"); got.ValueString() != "HTTP" {
		t.Errorf("got %q: a real protocol change must show as drift", got.ValueString())
	}
}

func TestReverseProxyErrorDetail(t *testing.T) {
	tests := []struct {
		err      error
		contains []string
	}{
		{errors.Join(client.ErrReverseProxyNotFound, errors.New(`"uuid-1"`)), []string{"uuid-1", "imported"}},
		{client.ErrAccessControlProfileNotFound, []string{"Access Control Profile"}},
		{errors.New(`entry "Nextcloud" already exists`), []string{"already exists", "unique"}},
		{&client.APIError{Code: 102, API: "SYNO.Core.AppPortal.ReverseProxy"}, []string{"DSM 7.x"}},
		{&client.APIError{Code: 105, API: "SYNO.Core.AppPortal.ReverseProxy"}, []string{"administrator"}},
		{&client.APIError{Code: 4151, API: "SYNO.Core.AppPortal.ReverseProxy"}, []string{"JSON-encoded string"}},
		{errors.New("connection refused"), []string{"connection refused"}},
	}
	for _, tt := range tests {
		got := reverseProxyErrorDetail(tt.err)
		for _, want := range tt.contains {
			if !strings.Contains(got, want) {
				t.Errorf("detail %q does not contain %q", got, want)
			}
		}
	}
}

func TestReverseProxyDataSource_Schema(t *testing.T) {
	d := NewReverseProxyDataSource()
	metadata := &datasource.MetadataResponse{}
	d.Metadata(t.Context(), datasource.MetadataRequest{ProviderTypeName: "dsm"}, metadata)
	if metadata.TypeName != "dsm_reverse_proxy" {
		t.Errorf("type name = %q, want dsm_reverse_proxy", metadata.TypeName)
	}

	resp := &datasource.SchemaResponse{}
	d.Schema(t.Context(), datasource.SchemaRequest{}, resp)
	if diags := resp.Schema.ValidateImplementation(t.Context()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %v", diags)
	}
	attrs := resp.Schema.GetAttributes()
	if !attrs["description"].IsRequired() {
		t.Error("description must be required")
	}
	for _, name := range []string{
		"id", "source_protocol", "source_hostname", "source_port",
		"destination_protocol", "destination_hostname", "destination_port",
		"websocket", "http2", "hsts", "custom_headers",
		"access_control_profile", "access_control_profile_id",
		"proxy_connect_timeout", "proxy_read_timeout", "proxy_send_timeout", "proxy_intercept_errors",
	} {
		attr, ok := attrs[name]
		if !ok || !attr.IsComputed() {
			t.Errorf("%s must be computed", name)
		}
	}
}
