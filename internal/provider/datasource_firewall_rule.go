package provider

import (
	"context"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

func NewFirewallRuleDataSource() datasource.DataSource {
	return &firewallRuleDataSource{}
}

type firewallRuleDataSource struct {
	client *client.Client
}

type firewallRuleDataSourceModel struct {
	ID              types.String `tfsdk:"id"`
	Profile         types.String `tfsdk:"profile"`
	Adapter         types.String `tfsdk:"adapter"`
	Name            types.String `tfsdk:"name"`
	Priority        types.Int64  `tfsdk:"priority"`
	Action          types.String `tfsdk:"action"`
	Protocol        types.String `tfsdk:"protocol"`
	Ports           types.List   `tfsdk:"ports"`
	Source          types.List   `tfsdk:"source"`
	Enabled         types.Bool   `tfsdk:"enabled"`
	FirewallEnabled types.Bool   `tfsdk:"firewall_enabled"`
	ProfileActive   types.Bool   `tfsdk:"profile_active"`
}

func (d *firewallRuleDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_firewall_rule"
}

func (d *firewallRuleDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Read-only data source for looking up one rule in a Synology DSM firewall profile.",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Unique identifier: profile:adapter:name.",
			},
			"profile": schema.StringAttribute{
				Required:    true,
				Description: "Firewall profile the rule belongs to.",
			},
			"adapter": schema.StringAttribute{
				Required:    true,
				Description: "Network interface the rule applies to, or `global` for every interface.",
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Rule name, shown as Description in the DSM UI.",
			},
			"priority": schema.Int64Attribute{
				Computed:    true,
				Description: "Zero-based position of the rule in its adapter's list. Lower numbers are evaluated first.",
			},
			"action": schema.StringAttribute{
				Computed:    true,
				Description: "What happens to matching traffic: allow or deny.",
			},
			"protocol": schema.StringAttribute{
				Computed:    true,
				Description: "Protocol the rule matches: tcp, udp, icmp, or all.",
			},
			"ports": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				Description: "Destination ports or ranges. Null when the rule matches every port, or when it selects ports through a DSM service preset this provider does not model.",
			},
			"source": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				Description: "Source addresses. Null when the rule matches any source, or when it selects sources by GeoIP country, which this provider does not model.",
			},
			"enabled": schema.BoolAttribute{
				Computed:    true,
				Description: "Whether the rule is active.",
			},
			"firewall_enabled": schema.BoolAttribute{
				Computed:    true,
				Description: "Whether the DSM firewall is switched on at all. A rule in a profile has no effect while this is false.",
			},
			"profile_active": schema.BoolAttribute{
				Computed:    true,
				Description: "Whether this profile is the one currently in force. Rules in a standby profile are stored but not enforced.",
			},
		},
	}
}

func (d *firewallRuleDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	dsmClient := clientFromProviderData(req.ProviderData, &resp.Diagnostics)
	if dsmClient == nil {
		return
	}

	d.client = dsmClient
}

func (d *firewallRuleDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config firewallRuleDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	profileName := config.Profile.ValueString()
	adapter := config.Adapter.ValueString()
	name := config.Name.ValueString()

	tflog.Debug(ctx, "Reading DSM firewall rule data source", map[string]interface{}{
		"profile": profileName,
		"adapter": adapter,
		"name":    name,
	})

	settings, err := d.client.GetFirewallSettings(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read firewall settings", err.Error())
		return
	}

	rule, err := d.client.GetFirewallRule(ctx, profileName, adapter, name)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read firewall rule", err.Error())
		return
	}

	config.ID = types.StringValue(client.BuildFirewallRuleID(profileName, adapter, name))
	config.Priority = types.Int64Value(int64(rule.Priority))
	config.Action = types.StringValue(rule.Action)
	config.Protocol = types.StringValue(rule.Protocol)
	config.Enabled = types.BoolValue(rule.Enabled)
	config.FirewallEnabled = types.BoolValue(settings.Enabled)
	config.ProfileActive = types.BoolValue(settings.ActiveProfile == profileName)

	ports, diags := stringListOrNull(ctx, rule.Ports)
	resp.Diagnostics.Append(diags...)
	config.Ports = ports

	sources, diags := stringListOrNull(ctx, rule.Sources)
	resp.Diagnostics.Append(diags...)
	config.Source = sources

	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}
