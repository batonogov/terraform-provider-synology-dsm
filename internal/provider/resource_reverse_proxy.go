package provider

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	tfpath "github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

// DSM's Application Portal restricts custom header names to letters, digits and
// hyphens. Enforcing it here turns a generic DSM rejection into a pointed error.
var reverseProxyHeaderName = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

const reverseProxyDefaultTimeout = 60

func NewReverseProxyResource() resource.Resource {
	return &reverseProxyResource{}
}

type reverseProxyResource struct {
	client *client.Client
}

type reverseProxyEndpointModel struct {
	Protocol types.String `tfsdk:"protocol"`
	Hostname types.String `tfsdk:"hostname"`
	Port     types.Int64  `tfsdk:"port"`
}

type reverseProxyResourceModel struct {
	ID                     types.String               `tfsdk:"id"`
	Description            types.String               `tfsdk:"description"`
	Source                 *reverseProxyEndpointModel `tfsdk:"source"`
	Destination            *reverseProxyEndpointModel `tfsdk:"destination"`
	WebSocket              types.Bool                 `tfsdk:"websocket"`
	HTTP2                  types.Bool                 `tfsdk:"http2"`
	HSTS                   types.Bool                 `tfsdk:"hsts"`
	CustomHeaders          types.Map                  `tfsdk:"custom_headers"`
	AccessControlProfile   types.String               `tfsdk:"access_control_profile"`
	AccessControlProfileID types.String               `tfsdk:"access_control_profile_id"`
	ConnectTimeout         types.Int64                `tfsdk:"proxy_connect_timeout"`
	ReadTimeout            types.Int64                `tfsdk:"proxy_read_timeout"`
	SendTimeout            types.Int64                `tfsdk:"proxy_send_timeout"`
	InterceptErrors        types.Bool                 `tfsdk:"proxy_intercept_errors"`
}

func (r *reverseProxyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_reverse_proxy"
}

func (r *reverseProxyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a reverse proxy entry in Control Panel → Login Portal → Advanced → Reverse Proxy. " +
			"DSM identifies entries by a UUID it assigns on creation; `description` is the only human-readable handle and must be unique.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Reverse proxy entry UUID assigned by DSM.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"description": schema.StringAttribute{
				Required: true,
				Description: "Entry description shown in DSM. DSM has no separate name field, so this doubles as the entry's " +
					"human-readable identity and must be unique across reverse proxy entries.",
			},
			"websocket": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				Description: "Add the WebSocket upgrade headers (`Upgrade: $http_upgrade` and `Connection: $connection_upgrade`). " +
					"This is the same pair DSM's own WebSocket preset inserts, and it is stored alongside `custom_headers`.",
			},
			"http2": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				Description: "Enable HTTP/2 on the source listener. Requires an `HTTPS` source. " +
					"Not every DSM build reports this flag back; when DSM omits it the configured value is kept instead of showing drift.",
			},
			"hsts": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "Send `Strict-Transport-Security` on the source listener. Requires an `HTTPS` source.",
			},
			"custom_headers": schema.MapAttribute{
				Optional:    true,
				ElementType: types.StringType,
				Description: "Additional request headers forwarded to the destination, keyed by header name. " +
					"Values may use nginx variables, for example `X-Forwarded-Proto = \"$scheme\"`. " +
					"Header names may contain only letters, digits, and `-`. Use `websocket` rather than setting `Upgrade`/`Connection` here.",
			},
			"access_control_profile": schema.StringAttribute{
				Optional: true,
				Description: "Name of the Login Portal access control profile to apply to this entry. " +
					"The profile must already exist; this provider resolves it to the UUID DSM stores.",
			},
			"access_control_profile_id": schema.StringAttribute{
				Computed:    true,
				Description: "UUID of the applied access control profile, or an empty string when no profile is attached.",
			},
			"proxy_connect_timeout": schema.Int64Attribute{
				Optional:    true,
				Computed:    true,
				Default:     int64default.StaticInt64(reverseProxyDefaultTimeout),
				Description: "Seconds to wait when connecting to the destination.",
			},
			"proxy_read_timeout": schema.Int64Attribute{
				Optional:    true,
				Computed:    true,
				Default:     int64default.StaticInt64(reverseProxyDefaultTimeout),
				Description: "Seconds to wait for a response from the destination.",
			},
			"proxy_send_timeout": schema.Int64Attribute{
				Optional:    true,
				Computed:    true,
				Default:     int64default.StaticInt64(reverseProxyDefaultTimeout),
				Description: "Seconds to wait while sending a request to the destination.",
			},
			"proxy_intercept_errors": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "Let DSM replace destination error responses with its own error pages.",
			},
		},
		Blocks: map[string]schema.Block{
			// Terraform blocks are optional at the protocol level, so requiredness
			// is enforced in ValidateConfig rather than in the schema.
			"source": schema.SingleNestedBlock{
				Description: "Public-facing listener DSM serves. Exactly one `source` block must be declared.",
				Attributes:  reverseProxyEndpointAttributes("Source", "cloud.example.com", "HTTPS"),
			},
			"destination": schema.SingleNestedBlock{
				Description: "Service requests are proxied to. Exactly one `destination` block must be declared.",
				Attributes:  reverseProxyEndpointAttributes("Destination", "localhost", "HTTP"),
			},
		},
	}
}

func reverseProxyEndpointAttributes(side, hostExample, protocolExample string) map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"protocol": schema.StringAttribute{
			Required:    true,
			Description: fmt.Sprintf("%s protocol: `HTTP` or `HTTPS` (for example `%s`). Matching is case-insensitive.", side, protocolExample),
		},
		"hostname": schema.StringAttribute{
			Required:    true,
			Description: fmt.Sprintf("%s hostname or IP address, for example `%s`. Do not include a scheme, port, or path.", side, hostExample),
		},
		"port": schema.Int64Attribute{
			Required:    true,
			Description: fmt.Sprintf("%s TCP port.", side),
		},
	}
}

func (r *reverseProxyResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config reverseProxyResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !config.Description.IsNull() && !config.Description.IsUnknown() {
		description := config.Description.ValueString()
		if strings.TrimSpace(description) == "" {
			resp.Diagnostics.AddAttributeError(tfpath.Root("description"), "Empty description",
				"DSM uses the description as the entry's only human-readable identity, so it cannot be blank.")
		}
	}

	validateReverseProxyEndpoint(config.Source, "source", &resp.Diagnostics)
	validateReverseProxyEndpoint(config.Destination, "destination", &resp.Diagnostics)

	// HSTS and HTTP/2 are properties of a TLS listener; DSM only offers them for
	// an HTTPS source, and nginx would have nowhere to apply them otherwise.
	if config.Source != nil && !config.Source.Protocol.IsNull() && !config.Source.Protocol.IsUnknown() {
		if !strings.EqualFold(config.Source.Protocol.ValueString(), client.ReverseProxyProtocolHTTPS) {
			for attribute, value := range map[string]types.Bool{"hsts": config.HSTS, "http2": config.HTTP2} {
				if value.ValueBool() {
					resp.Diagnostics.AddAttributeError(tfpath.Root(attribute), "Requires an HTTPS source",
						fmt.Sprintf("`%s` applies to a TLS listener, so `source.protocol` must be `HTTPS`.", attribute))
				}
			}
		}
	}

	for _, timeout := range []struct {
		name  string
		value types.Int64
	}{
		{"proxy_connect_timeout", config.ConnectTimeout},
		{"proxy_read_timeout", config.ReadTimeout},
		{"proxy_send_timeout", config.SendTimeout},
	} {
		if timeout.value.IsNull() || timeout.value.IsUnknown() {
			continue
		}
		if timeout.value.ValueInt64() < 1 {
			resp.Diagnostics.AddAttributeError(tfpath.Root(timeout.name), "Invalid timeout",
				fmt.Sprintf("`%s` must be at least 1 second.", timeout.name))
		}
	}

	if !config.CustomHeaders.IsNull() && !config.CustomHeaders.IsUnknown() {
		headers := map[string]types.String{}
		resp.Diagnostics.Append(config.CustomHeaders.ElementsAs(ctx, &headers, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		for name := range headers {
			if !reverseProxyHeaderName.MatchString(name) {
				resp.Diagnostics.AddAttributeError(tfpath.Root("custom_headers"), "Invalid header name",
					fmt.Sprintf("DSM accepts only letters, digits, and `-` in a custom header name; %q is not valid.", name))
			}
			if config.WebSocket.ValueBool() && isReverseProxyWebSocketHeaderName(name) {
				resp.Diagnostics.AddAttributeError(tfpath.Root("custom_headers"), "Header conflicts with websocket",
					fmt.Sprintf("`websocket = true` already sets %q. Remove it from `custom_headers` or disable `websocket`.", name))
			}
		}
	}
}

func validateReverseProxyEndpoint(endpoint *reverseProxyEndpointModel, block string, diags *diag.Diagnostics) {
	if endpoint == nil {
		diags.AddAttributeError(tfpath.Root(block), "Missing required block",
			fmt.Sprintf("A `%s` block with `protocol`, `hostname`, and `port` is required.", block))
		return
	}

	if !endpoint.Protocol.IsNull() && !endpoint.Protocol.IsUnknown() {
		if _, err := client.EncodeReverseProxyProtocol(endpoint.Protocol.ValueString()); err != nil {
			diags.AddAttributeError(tfpath.Root(block).AtName("protocol"), "Invalid protocol", err.Error())
		}
	}
	if !endpoint.Hostname.IsNull() && !endpoint.Hostname.IsUnknown() {
		hostname := endpoint.Hostname.ValueString()
		switch {
		case strings.TrimSpace(hostname) == "":
			diags.AddAttributeError(tfpath.Root(block).AtName("hostname"), "Empty hostname", "A hostname or IP address is required.")
		case strings.Contains(hostname, "/") || strings.Contains(hostname, ":") || hostname != strings.TrimSpace(hostname):
			diags.AddAttributeError(tfpath.Root(block).AtName("hostname"), "Invalid hostname",
				"Use a bare hostname or IP address such as `cloud.example.com`, without a scheme, port, or path.")
		}
	}
	if !endpoint.Port.IsNull() && !endpoint.Port.IsUnknown() {
		if port := endpoint.Port.ValueInt64(); port < 1 || port > 65535 {
			diags.AddAttributeError(tfpath.Root(block).AtName("port"), "Invalid port", "Port must be between 1 and 65535.")
		}
	}
}

func (r *reverseProxyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	dsmClient := clientFromProviderData(req.ProviderData, &resp.Diagnostics)
	if dsmClient == nil {
		return
	}
	r.client = dsmClient
}

func (r *reverseProxyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan reverseProxyResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	entry := r.buildReverseProxy(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Creating DSM reverse proxy entry", map[string]interface{}{"description": plan.Description.ValueString()})
	created, err := r.client.CreateReverseProxy(ctx, entry)
	if err != nil {
		resp.Diagnostics.AddError("Failed to create reverse proxy entry", reverseProxyErrorDetail(err))
		return
	}

	r.applyReverseProxy(ctx, &plan, created, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *reverseProxyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state reverseProxyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	entry, err := r.client.GetReverseProxy(ctx, state.ID.ValueString())
	if errors.Is(err, client.ErrReverseProxyNotFound) {
		// An import may have supplied the description instead of the UUID, and a
		// UUID-based lookup after an out-of-band recreate can miss for the same
		// entry. Fall back to the description before declaring the entry gone.
		description := state.Description.ValueString()
		if description == "" {
			description = state.ID.ValueString()
		}
		entry, err = r.client.GetReverseProxyByDescription(ctx, description)
	}
	if errors.Is(err, client.ErrReverseProxyNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read reverse proxy entry", reverseProxyErrorDetail(err))
		return
	}

	r.applyReverseProxy(ctx, &state, entry, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *reverseProxyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan reverseProxyResourceModel
	var state reverseProxyResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	entry := r.buildReverseProxy(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	entry.UUID = state.ID.ValueString()

	updated, err := r.client.UpdateReverseProxy(ctx, entry)
	if err != nil {
		resp.Diagnostics.AddError("Failed to update reverse proxy entry", reverseProxyErrorDetail(err))
		return
	}

	r.applyReverseProxy(ctx, &plan, updated, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *reverseProxyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state reverseProxyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Deleting DSM reverse proxy entry", map[string]interface{}{"id": state.ID.ValueString()})
	if err := r.client.DeleteReverseProxy(ctx, state.ID.ValueString()); err != nil {
		resp.Diagnostics.AddError("Failed to delete reverse proxy entry", reverseProxyErrorDetail(err))
	}
}

func (r *reverseProxyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// The DSM-assigned UUID is the canonical import ID. Read also accepts an
	// entry description here and normalizes it to the UUID.
	resource.ImportStatePassthroughID(ctx, tfpath.Root("id"), req, resp)
}

// buildReverseProxy renders the plan into the client's entry representation,
// resolving the access control profile name to the UUID DSM stores.
func (r *reverseProxyResource) buildReverseProxy(ctx context.Context, model *reverseProxyResourceModel, diags *diag.Diagnostics) client.ReverseProxy {
	entry := client.ReverseProxy{
		Description:     model.Description.ValueString(),
		HSTS:            model.HSTS.ValueBool(),
		ConnectTimeout:  model.ConnectTimeout.ValueInt64(),
		ReadTimeout:     model.ReadTimeout.ValueInt64(),
		SendTimeout:     model.SendTimeout.ValueInt64(),
		InterceptErrors: model.InterceptErrors.ValueBool(),
	}
	if model.Source != nil {
		entry.Source = client.ReverseProxyEndpoint{
			Protocol: model.Source.Protocol.ValueString(),
			Hostname: model.Source.Hostname.ValueString(),
			Port:     model.Source.Port.ValueInt64(),
		}
	}
	if model.Destination != nil {
		entry.Destination = client.ReverseProxyEndpoint{
			Protocol: model.Destination.Protocol.ValueString(),
			Hostname: model.Destination.Hostname.ValueString(),
			Port:     model.Destination.Port.ValueInt64(),
		}
	}

	// Only send http2 when it is switched on. The key is inferred rather than
	// observed on the wire (see internal/client/reverse_proxy.go), so a
	// configuration that leaves HTTP/2 off never exercises it.
	if model.HTTP2.ValueBool() {
		enabled := true
		entry.HTTP2 = &enabled
	}

	customHeaders := map[string]string{}
	if !model.CustomHeaders.IsNull() && !model.CustomHeaders.IsUnknown() {
		diags.Append(model.CustomHeaders.ElementsAs(ctx, &customHeaders, false)...)
		if diags.HasError() {
			return entry
		}
	}
	entry.CustomHeaders = buildReverseProxyHeaders(model.WebSocket.ValueBool(), customHeaders)

	if profile := model.AccessControlProfile.ValueString(); profile != "" {
		resolved, err := r.client.FindAccessControlProfileByName(ctx, profile)
		if err != nil {
			diags.AddAttributeError(tfpath.Root("access_control_profile"), "Access control profile not found", reverseProxyErrorDetail(err))
			return entry
		}
		entry.ACLProfileUUID = resolved.UUID
	}

	return entry
}

// applyReverseProxy writes every state field from the DSM entry. The prior model
// is used to keep the configured spelling of protocols and to hold on to values
// DSM does not report back.
func (r *reverseProxyResource) applyReverseProxy(ctx context.Context, model *reverseProxyResourceModel, entry *client.ReverseProxy, diags *diag.Diagnostics) {
	model.ID = types.StringValue(entry.UUID)
	model.Description = types.StringValue(entry.Description)
	model.Source = applyReverseProxyEndpoint(model.Source, entry.Source)
	model.Destination = applyReverseProxyEndpoint(model.Destination, entry.Destination)
	model.HSTS = types.BoolValue(entry.HSTS)
	model.ConnectTimeout = types.Int64Value(entry.ConnectTimeout)
	model.ReadTimeout = types.Int64Value(entry.ReadTimeout)
	model.SendTimeout = types.Int64Value(entry.SendTimeout)
	model.InterceptErrors = types.BoolValue(entry.InterceptErrors)

	// DSM builds that do not persist http2 report nothing at all rather than
	// false. Reporting false in that case would put the resource in a permanent
	// "update in-place" loop, so keep what the configuration asked for.
	switch {
	case entry.HTTP2 != nil:
		model.HTTP2 = types.BoolValue(*entry.HTTP2)
	case model.HTTP2.IsNull() || model.HTTP2.IsUnknown():
		model.HTTP2 = types.BoolValue(false)
	}

	websocket, customHeaders := splitReverseProxyHeaders(entry.CustomHeaders)
	model.WebSocket = types.BoolValue(websocket)
	if len(customHeaders) == 0 {
		model.CustomHeaders = types.MapNull(types.StringType)
	} else {
		value, mapDiags := types.MapValueFrom(ctx, types.StringType, customHeaders)
		diags.Append(mapDiags...)
		model.CustomHeaders = value
	}

	model.AccessControlProfileID = types.StringValue(entry.ACLProfileUUID)
	if entry.ACLProfileUUID == "" {
		model.AccessControlProfile = types.StringNull()
		return
	}
	profile, err := r.client.FindAccessControlProfileByUUID(ctx, entry.ACLProfileUUID)
	if err != nil {
		// Keep the configured name rather than failing the whole read: the entry
		// itself is intact and the UUID is still in state.
		tflog.Warn(ctx, "Could not resolve reverse proxy access control profile name", map[string]interface{}{
			"uuid":  entry.ACLProfileUUID,
			"error": err.Error(),
		})
		return
	}
	model.AccessControlProfile = types.StringValue(profile.Name)
}

func applyReverseProxyEndpoint(prior *reverseProxyEndpointModel, endpoint client.ReverseProxyEndpoint) *reverseProxyEndpointModel {
	applied := &reverseProxyEndpointModel{
		Hostname: types.StringValue(endpoint.Hostname),
		Port:     types.Int64Value(endpoint.Port),
	}
	if prior != nil {
		applied.Protocol = preserveProtocolCase(prior.Protocol, endpoint.Protocol)
	} else {
		applied.Protocol = types.StringValue(endpoint.Protocol)
	}
	return applied
}

// preserveProtocolCase keeps the spelling the configuration used. DSM stores the
// protocol as an integer, so `https` in a configuration would otherwise be read
// back as `HTTPS` and produce a permanent diff.
func preserveProtocolCase(prior types.String, decoded string) types.String {
	if !prior.IsNull() && !prior.IsUnknown() && strings.EqualFold(prior.ValueString(), decoded) {
		return prior
	}
	return types.StringValue(decoded)
}

// buildReverseProxyHeaders renders the websocket toggle and the custom header
// map into DSM's ordered array. Custom headers are sorted by name so that the
// payload is stable across applies.
func buildReverseProxyHeaders(websocket bool, custom map[string]string) []client.ReverseProxyHeader {
	headers := make([]client.ReverseProxyHeader, 0, len(custom)+2)
	if websocket {
		headers = append(headers, client.ReverseProxyWebSocketHeaders()...)
	}
	names := make([]string, 0, len(custom))
	for name := range custom {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		headers = append(headers, client.ReverseProxyHeader{Name: name, Value: custom[name]})
	}
	return headers
}

// splitReverseProxyHeaders is the inverse of buildReverseProxyHeaders: it
// recognises DSM's WebSocket preset and reports the rest as custom headers.
func splitReverseProxyHeaders(headers []client.ReverseProxyHeader) (bool, map[string]string) {
	preset := map[string]string{}
	for _, header := range client.ReverseProxyWebSocketHeaders() {
		preset[strings.ToLower(header.Name)] = header.Value
	}

	matched := 0
	for _, header := range headers {
		if value, ok := preset[strings.ToLower(header.Name)]; ok && value == header.Value {
			matched++
		}
	}
	websocket := matched == len(preset)

	custom := map[string]string{}
	for _, header := range headers {
		if websocket && isReverseProxyWebSocketHeaderName(header.Name) {
			continue
		}
		custom[header.Name] = header.Value
	}
	return websocket, custom
}

func isReverseProxyWebSocketHeaderName(name string) bool {
	for _, header := range client.ReverseProxyWebSocketHeaders() {
		if strings.EqualFold(header.Name, name) {
			return true
		}
	}
	return false
}

func reverseProxyErrorDetail(err error) string {
	message := err.Error()
	switch {
	case errors.Is(err, client.ErrReverseProxyNotFound):
		return message + "\n\nCheck the entry UUID or description in Control Panel → Login Portal → Advanced → Reverse Proxy. Existing entries must be imported before Terraform can manage them."
	case errors.Is(err, client.ErrAccessControlProfileNotFound):
		return message + "\n\nCreate the profile in Control Panel → Login Portal → Advanced → Access Control Profile first; this provider does not create profiles."
	case strings.Contains(message, "already exists"):
		return message + "\n\nDescriptions must be unique. Import the existing entry by its UUID instead of creating a second one."
	case client.IsAPIError(err, 102, 103, 104):
		return message + "\n\nThe reverse proxy API is unavailable on this DSM. It was developed against DSM 7.x; older releases expose a different Application Portal API."
	case client.IsAPIError(err, 105):
		return message + "\n\nDSM denied the operation. Reverse proxy entries can only be managed by an administrator account."
	default:
		return message
	}
}
