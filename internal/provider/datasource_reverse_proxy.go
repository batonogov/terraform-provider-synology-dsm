package provider

import (
	"context"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

func NewReverseProxyDataSource() datasource.DataSource {
	return &reverseProxyDataSource{}
}

type reverseProxyDataSource struct {
	client *client.Client
}

type reverseProxyDataSourceModel struct {
	ID                     types.String `tfsdk:"id"`
	Description            types.String `tfsdk:"description"`
	SourceProtocol         types.String `tfsdk:"source_protocol"`
	SourceHostname         types.String `tfsdk:"source_hostname"`
	SourcePort             types.Int64  `tfsdk:"source_port"`
	DestinationProtocol    types.String `tfsdk:"destination_protocol"`
	DestinationHostname    types.String `tfsdk:"destination_hostname"`
	DestinationPort        types.Int64  `tfsdk:"destination_port"`
	WebSocket              types.Bool   `tfsdk:"websocket"`
	HTTP2                  types.Bool   `tfsdk:"http2"`
	HSTS                   types.Bool   `tfsdk:"hsts"`
	CustomHeaders          types.Map    `tfsdk:"custom_headers"`
	AccessControlProfile   types.String `tfsdk:"access_control_profile"`
	AccessControlProfileID types.String `tfsdk:"access_control_profile_id"`
	ConnectTimeout         types.Int64  `tfsdk:"proxy_connect_timeout"`
	ReadTimeout            types.Int64  `tfsdk:"proxy_read_timeout"`
	SendTimeout            types.Int64  `tfsdk:"proxy_send_timeout"`
	InterceptErrors        types.Bool   `tfsdk:"proxy_intercept_errors"`
}

func (d *reverseProxyDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_reverse_proxy"
}

func (d *reverseProxyDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Looks up an existing DSM reverse proxy entry by description. " +
			"The nested source and destination values are flattened here because a data source has nothing to configure.",
		Attributes: map[string]schema.Attribute{
			"id":                        schema.StringAttribute{Computed: true, Description: "Reverse proxy entry UUID assigned by DSM."},
			"description":               schema.StringAttribute{Required: true, Description: "Entry description to look up."},
			"source_protocol":           schema.StringAttribute{Computed: true, Description: "Source protocol, `HTTP` or `HTTPS`."},
			"source_hostname":           schema.StringAttribute{Computed: true, Description: "Source hostname served by DSM."},
			"source_port":               schema.Int64Attribute{Computed: true, Description: "Source TCP port."},
			"destination_protocol":      schema.StringAttribute{Computed: true, Description: "Destination protocol, `HTTP` or `HTTPS`."},
			"destination_hostname":      schema.StringAttribute{Computed: true, Description: "Destination hostname or IP address."},
			"destination_port":          schema.Int64Attribute{Computed: true, Description: "Destination TCP port."},
			"websocket":                 schema.BoolAttribute{Computed: true, Description: "Whether the WebSocket upgrade headers are present."},
			"http2":                     schema.BoolAttribute{Computed: true, Description: "Whether HTTP/2 is enabled on the source listener. Reported as `false` when DSM does not expose the flag."},
			"hsts":                      schema.BoolAttribute{Computed: true, Description: "Whether HSTS is enabled on the source listener."},
			"custom_headers":            schema.MapAttribute{Computed: true, ElementType: types.StringType, Description: "Custom request headers, excluding the WebSocket pair reported by `websocket`."},
			"access_control_profile":    schema.StringAttribute{Computed: true, Description: "Name of the attached access control profile, if any."},
			"access_control_profile_id": schema.StringAttribute{Computed: true, Description: "UUID of the attached access control profile, or an empty string."},
			"proxy_connect_timeout":     schema.Int64Attribute{Computed: true, Description: "Seconds to wait when connecting to the destination."},
			"proxy_read_timeout":        schema.Int64Attribute{Computed: true, Description: "Seconds to wait for a response from the destination."},
			"proxy_send_timeout":        schema.Int64Attribute{Computed: true, Description: "Seconds to wait while sending a request to the destination."},
			"proxy_intercept_errors":    schema.BoolAttribute{Computed: true, Description: "Whether DSM replaces destination error responses with its own."},
		},
	}
}

func (d *reverseProxyDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	dsmClient := clientFromProviderData(req.ProviderData, &resp.Diagnostics)
	if dsmClient == nil {
		return
	}
	d.client = dsmClient
}

func (d *reverseProxyDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config reverseProxyDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	entry, err := d.client.GetReverseProxyByDescription(ctx, config.Description.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Failed to read reverse proxy entry", reverseProxyErrorDetail(err))
		return
	}

	config.ID = types.StringValue(entry.UUID)
	config.Description = types.StringValue(entry.Description)
	config.SourceProtocol = types.StringValue(entry.Source.Protocol)
	config.SourceHostname = types.StringValue(entry.Source.Hostname)
	config.SourcePort = types.Int64Value(entry.Source.Port)
	config.DestinationProtocol = types.StringValue(entry.Destination.Protocol)
	config.DestinationHostname = types.StringValue(entry.Destination.Hostname)
	config.DestinationPort = types.Int64Value(entry.Destination.Port)
	config.HSTS = types.BoolValue(entry.HSTS)
	config.HTTP2 = types.BoolValue(entry.HTTP2 != nil && *entry.HTTP2)
	config.ConnectTimeout = types.Int64Value(entry.ConnectTimeout)
	config.ReadTimeout = types.Int64Value(entry.ReadTimeout)
	config.SendTimeout = types.Int64Value(entry.SendTimeout)
	config.InterceptErrors = types.BoolValue(entry.InterceptErrors)

	websocket, customHeaders := splitReverseProxyHeaders(entry.CustomHeaders)
	config.WebSocket = types.BoolValue(websocket)
	headers, diags := types.MapValueFrom(ctx, types.StringType, customHeaders)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	config.CustomHeaders = headers

	config.AccessControlProfileID = types.StringValue(entry.ACLProfileUUID)
	config.AccessControlProfile = types.StringNull()
	if entry.ACLProfileUUID != "" {
		profile, profileErr := d.client.FindAccessControlProfileByUUID(ctx, entry.ACLProfileUUID)
		if profileErr != nil {
			tflog.Warn(ctx, "Could not resolve reverse proxy access control profile name", map[string]interface{}{
				"uuid":  entry.ACLProfileUUID,
				"error": profileErr.Error(),
			})
		} else {
			config.AccessControlProfile = types.StringValue(profile.Name)
		}
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}
