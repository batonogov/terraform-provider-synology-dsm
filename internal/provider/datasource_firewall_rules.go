package provider

import (
	"context"
	"fmt"
	"sort"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

func NewFirewallRulesDataSource() datasource.DataSource {
	return &firewallRulesDataSource{}
}

type firewallRulesDataSource struct {
	client *client.Client
}

type firewallRulesDataSourceModel struct {
	ID              types.String             `tfsdk:"id"`
	Profile         types.String             `tfsdk:"profile"`
	FirewallEnabled types.Bool               `tfsdk:"firewall_enabled"`
	ProfileActive   types.Bool               `tfsdk:"profile_active"`
	DefaultPolicy   types.Map                `tfsdk:"default_policy"`
	Rules           []firewallRuleEntryModel `tfsdk:"rules"`
}

type firewallRuleEntryModel struct {
	Adapter  types.String `tfsdk:"adapter"`
	Name     types.String `tfsdk:"name"`
	Priority types.Int64  `tfsdk:"priority"`
	Action   types.String `tfsdk:"action"`
	Protocol types.String `tfsdk:"protocol"`
	Ports    types.List   `tfsdk:"ports"`
	Source   types.List   `tfsdk:"source"`
	Enabled  types.Bool   `tfsdk:"enabled"`
}

func (d *firewallRulesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_firewall_rules"
}

func (d *firewallRulesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Lists every rule in a Synology DSM firewall profile, across all adapters, in evaluation order — " +
			"the whole picture for a security audit in one read. Read-only; use `dsm_firewall_rule` to manage individual rules.",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Identifier for this data source result: the profile name.",
			},
			"profile": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Firewall profile to read. Defaults to `default`.",
			},
			"firewall_enabled": schema.BoolAttribute{
				Computed:    true,
				Description: "Whether the DSM firewall is switched on at all. No rule has any effect while this is false.",
			},
			"profile_active": schema.BoolAttribute{
				Computed:    true,
				Description: "Whether this profile is the one currently in force. Rules in a standby profile are stored but not enforced.",
			},
			"default_policy": schema.MapAttribute{
				Computed:    true,
				ElementType: types.StringType,
				Description: "What the profile does with traffic that matches no rule, keyed by network adapter: " +
					"`allow`, `deny`, or `none`. This is the \"Allow access\" / \"Deny access\" choice at the bottom of the " +
					"DSM firewall page, and it decides the meaning of the whole rule list — a list of allow rules above a " +
					"`deny` default is a whitelist, the same list above an `allow` default enforces nothing. DSM stores it " +
					"per adapter rather than once per profile. The `global` pseudo-interface is a pre-table rather than an " +
					"interface and normally carries `none`. Manage it with `dsm_firewall`.",
			},
			"rules": schema.ListNestedAttribute{
				Computed:    true,
				Description: "All rules in the profile. Adapters are ordered with `global` first, then the rest alphabetically; within an adapter, rules appear in evaluation order.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"adapter": schema.StringAttribute{
							Computed:    true,
							Description: "Network interface the rule applies to, or `global` for every interface.",
						},
						"name": schema.StringAttribute{
							Computed:    true,
							Description: "Rule name, shown as Description in the DSM UI — the rule's identity within its adapter.",
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
					},
				},
			},
		},
	}
}

func (d *firewallRulesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	dsmClient := clientFromProviderData(req.ProviderData, &resp.Diagnostics)
	if dsmClient == nil {
		return
	}

	d.client = dsmClient
}

func (d *firewallRulesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config firewallRulesDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	profileName := config.Profile.ValueString()
	if profileName == "" {
		profileName = "default"
		config.Profile = types.StringValue(profileName)
	}

	tflog.Debug(ctx, "Reading DSM firewall rules data source", map[string]interface{}{
		"profile": profileName,
	})

	settings, err := d.client.GetFirewallSettings(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read firewall settings", err.Error())
		return
	}

	profile, err := d.client.GetFirewallProfile(ctx, profileName)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read firewall profile", err.Error())
		return
	}

	config.ID = types.StringValue(fmt.Sprintf("firewall-rules:%s", profileName))
	config.FirewallEnabled = types.BoolValue(settings.Enabled)
	config.ProfileActive = types.BoolValue(settings.ActiveProfile == profileName)

	defaultPolicy, diags := stringMapValue(ctx, profile.DefaultPolicyNames())
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	config.DefaultPolicy = defaultPolicy

	ruleTotal := 0
	for _, rules := range profile.Rules {
		ruleTotal += len(rules)
	}
	entries := make([]firewallRuleEntryModel, 0, ruleTotal)
	for _, adapter := range sortedAdapterNames(profile) {
		for _, rule := range profile.Rules[adapter] {
			entry := firewallRuleEntryModel{
				Adapter:  types.StringValue(adapter),
				Name:     types.StringValue(rule.Name),
				Priority: types.Int64Value(int64(rule.Priority)),
				Action:   types.StringValue(rule.Action),
				Protocol: types.StringValue(rule.Protocol),
				Enabled:  types.BoolValue(rule.Enabled),
			}

			ports, diags := stringListOrNull(ctx, rule.Ports)
			resp.Diagnostics.Append(diags...)
			entry.Ports = ports

			sources, diags := stringListOrNull(ctx, rule.Sources)
			resp.Diagnostics.Append(diags...)
			entry.Source = sources

			if resp.Diagnostics.HasError() {
				return
			}

			entries = append(entries, entry)
		}
	}
	config.Rules = entries

	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}

// sortedAdapterNames orders adapter names with global first, then the rest
// alphabetically, so the audit output is deterministic and the catch-all
// adapter reads before the interface-specific ones.
func sortedAdapterNames(profile *client.FirewallProfile) []string {
	names := make([]string, 0, len(profile.Rules))
	for name := range profile.Rules {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if (names[i] == "global") != (names[j] == "global") {
			return names[i] == "global"
		}
		return names[i] < names[j]
	})
	return names
}
