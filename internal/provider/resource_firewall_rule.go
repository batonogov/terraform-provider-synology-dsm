package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

func NewFirewallRuleResource() resource.Resource {
	return &firewallRuleResource{}
}

type firewallRuleResource struct {
	client *client.Client
}

type firewallRuleResourceModel struct {
	ID                types.String `tfsdk:"id"`
	Profile           types.String `tfsdk:"profile"`
	Adapter           types.String `tfsdk:"adapter"`
	Name              types.String `tfsdk:"name"`
	Priority          types.Int64  `tfsdk:"priority"`
	Action            types.String `tfsdk:"action"`
	Protocol          types.String `tfsdk:"protocol"`
	Ports             types.List   `tfsdk:"ports"`
	Source            types.List   `tfsdk:"source"`
	Enabled           types.Bool   `tfsdk:"enabled"`
	AllowLockout      types.Bool   `tfsdk:"allow_lockout"`
	AllowEmptyRuleSet types.Bool   `tfsdk:"allow_empty_rule_set"`
}

func (r *firewallRuleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_firewall_rule"
}

func (r *firewallRuleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages one rule in a Synology DSM firewall profile. " +
			"Rules are matched top to bottom and the first match wins, so `priority` is part of the policy, not a cosmetic detail.",
		MarkdownDescription: "Manages one rule in a Synology DSM firewall profile.\n\n" +
			"DSM evaluates a profile's rules top to bottom and stops at the first match, so the order of rules *is* the policy. " +
			"This resource therefore requires an explicit `priority`, which is the rule's zero-based position in the profile's list.\n\n" +
			"Rules created in the same apply end up in priority order regardless of the order Terraform writes them, so " +
			"`depends_on` between rules is not needed.\n\n" +
			"~> **Managed rules take their positions first.** Rules created in the DSM UI are never reordered against each " +
			"other and keep everything this provider does not model, but they fill the positions no managed rule claims — so " +
			"a rule managed here with `priority = 0` moves a hand-written rule at the top of the list down one. For a firewall " +
			"that is a change of policy, not of presentation: check the resulting order in DSM after the first apply against " +
			"a profile you did not create from Terraform.\n\n" +
			"~> **The firewall can lock you out.** Before every write the provider replays the resulting rule set against its own " +
			"DSM session and refuses the change if that session would be denied. Set `allow_lockout = true` to override. " +
			"Deleting the last rule of an enabled profile is refused for the same reason; override with `allow_empty_rule_set = true`.\n\n" +
			"~> The DSM firewall API is undocumented. This resource was written against reverse-engineered field names and has not " +
			"been verified against physical hardware; see the provider README before using it on a NAS you cannot reach physically.",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Unique identifier: profile:adapter:name.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"profile": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString("default"),
				Description: "Firewall profile the rule belongs to.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"adapter": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(client.FirewallAdapterGlobal),
				Description: "Network interface the rule applies to, such as `eth0`, or `global` for every interface. " +
					"DSM evaluates the `global` table before the table of the interface a connection actually arrived on.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				Required: true,
				Description: "Rule name. DSM shows this in the Description column, and this provider uses it as the rule's " +
					"identity within a profile and adapter, so it must be unique there.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"priority": schema.Int64Attribute{
				Required: true,
				Description: "Zero-based position of the rule in the profile's list. Lower numbers are evaluated first. " +
					"Every write lays out the whole list from the configured priorities, so rules created in one apply end up " +
					"in priority order however Terraform schedules them — no depends_on required. Number the rules of one " +
					"profile and adapter contiguously from 0, counting any rules created outside Terraform: a priority past " +
					"the end of the list cannot be honoured, and reading the rule reports its actual position, so both that " +
					"and a reordering done in DSM show up as a diff.",
			},
			"action": schema.StringAttribute{
				Required:    true,
				Description: "What to do with matching traffic: allow or deny.",
				Validators: []validator.String{
					newStringOneOfValidator(client.FirewallActionAllow, client.FirewallActionDeny),
				},
			},
			"protocol": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString(client.FirewallProtocolAll),
				Description: "Protocol the rule matches: tcp, udp, icmp, or all. Note that DSM's `all` covers TCP and UDP only, not ICMP.",
				Validators: []validator.String{
					newStringOneOfValidator(
						client.FirewallProtocolTCP,
						client.FirewallProtocolUDP,
						client.FirewallProtocolICMP,
						client.FirewallProtocolAll,
					),
				},
			},
			"ports": schema.ListAttribute{
				Optional:    true,
				ElementType: types.StringType,
				Description: "Destination ports or ranges, for example [\"8443\", \"8000-8100\"]. Omit to match every port.",
			},
			"source": schema.ListAttribute{
				Optional:    true,
				ElementType: types.StringType,
				Description: "Source addresses for the rule. A single entry may be an address, a CIDR such as 10.210.0.0/16, " +
					"or a dashed range such as 10.0.0.1-10.0.0.9; several entries must all be plain addresses, because that is " +
					"the only multi-value form DSM stores. Omit to match any source.",
			},
			"enabled": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(true),
				Description: "Whether the rule is active. A disabled rule stays in the list but is skipped when matching.",
			},
			"allow_lockout": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				Description: "Apply the change even if the provider determines it would deny its own DSM session. " +
					"Only meaningful when you have another way back into the NAS.",
			},
			"allow_empty_rule_set": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				Description: "Allow destroying this rule when it is the last one in an enabled profile. " +
					"Without it, that destroy is refused because an enabled profile with no rules denies everything.",
			},
		},
	}
}

func (r *firewallRuleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	dsmClient := clientFromProviderData(req.ProviderData, &resp.Diagnostics)
	if dsmClient == nil {
		return
	}

	r.client = dsmClient
}

func (r *firewallRuleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan firewallRuleResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Creating DSM firewall rule", map[string]interface{}{
		"profile": plan.Profile.ValueString(),
		"adapter": plan.Adapter.ValueString(),
		"name":    plan.Name.ValueString(),
	})

	r.write(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *firewallRuleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state firewallRuleResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	profile := state.Profile.ValueString()
	adapter := state.Adapter.ValueString()
	name := state.Name.ValueString()

	if id := state.ID.ValueString(); id != "" {
		p, a, n, err := client.ParseFirewallRuleID(id)
		if err == nil {
			profile, adapter, name = p, a, n
		}
	}

	tflog.Debug(ctx, "Reading DSM firewall rule", map[string]interface{}{
		"profile": profile,
		"adapter": adapter,
		"name":    name,
	})

	placement, err := r.client.GetFirewallRulePlacement(ctx, profile, adapter, name)
	if err != nil {
		if removeIfGone(ctx, resp, err, "firewall rule") {
			return
		}
		resp.Diagnostics.AddError("Failed to read firewall rule", err.Error())
		return
	}
	rule := placement.Rule

	// Checked here rather than during the write, and before the configured value
	// is overwritten below. A write cannot tell "this priority is out of range"
	// apart from "the other rules of this apply do not exist yet", because it
	// cannot know what Terraform is about to create. A refresh can: the list is
	// whole and nothing is being created alongside.
	appendUnreachablePriorityWarning(&resp.Diagnostics, profile, adapter, name,
		state.Priority, rule.Priority, placement.RuleCount)

	state.ID = types.StringValue(client.BuildFirewallRuleID(profile, adapter, name))
	state.Profile = types.StringValue(profile)
	state.Adapter = types.StringValue(adapter)
	state.Name = types.StringValue(rule.Name)
	// The actual index, not the configured one: that is what makes a reordering
	// performed outside Terraform visible as a diff instead of silently changing
	// the effective policy.
	state.Priority = types.Int64Value(int64(rule.Priority))
	state.Action = types.StringValue(rule.Action)
	state.Protocol = types.StringValue(rule.Protocol)
	state.Enabled = types.BoolValue(rule.Enabled)

	ports, diags := stringListOrNull(ctx, rule.Ports)
	resp.Diagnostics.Append(diags...)
	state.Ports = ports

	sources, diags := stringListOrNull(ctx, rule.Sources)
	resp.Diagnostics.Append(diags...)
	state.Source = sources

	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *firewallRuleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan firewallRuleResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Updating DSM firewall rule", map[string]interface{}{
		"profile": plan.Profile.ValueString(),
		"adapter": plan.Adapter.ValueString(),
		"name":    plan.Name.ValueString(),
	})

	r.write(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

// write performs the create/update half of the resource, which is identical for
// both: DSM has no notion of adding versus editing a rule, only of writing a
// profile's whole list.
func (r *firewallRuleResource) write(ctx context.Context, plan *firewallRuleResourceModel, diags *diag.Diagnostics) {
	ports, d := stringSliceFromList(ctx, plan.Ports)
	diags.Append(d...)
	sources, d := stringSliceFromList(ctx, plan.Source)
	diags.Append(d...)
	if diags.HasError() {
		return
	}

	result, err := r.client.SetFirewallRule(ctx, client.SetFirewallRuleRequest{
		Profile: plan.Profile.ValueString(),
		Adapter: plan.Adapter.ValueString(),
		Rule: client.FirewallRule{
			Name:     plan.Name.ValueString(),
			Enabled:  plan.Enabled.ValueBool(),
			Action:   plan.Action.ValueString(),
			Protocol: plan.Protocol.ValueString(),
			Ports:    ports,
			Sources:  sources,
			Priority: int(plan.Priority.ValueInt64()),
		},
		AllowLockout: plan.AllowLockout.ValueBool(),
	})
	if err != nil {
		appendFirewallDiagnostic(diags, "Failed to write firewall rule", err)
		return
	}
	appendLockoutWarning(diags, result.LockoutWarning)
	appendOrderConflictWarning(diags, result.OrderConflict)

	plan.ID = types.StringValue(client.BuildFirewallRuleID(
		plan.Profile.ValueString(),
		plan.Adapter.ValueString(),
		plan.Name.ValueString(),
	))

	// Priority in state stays as planned — returning the achieved index here
	// would be an inconsistent-result error from Terraform. The next refresh
	// reports the actual position, so a priority the profile is too short to
	// honour turns into an ordinary diff instead of a silent surprise.
}

func (r *firewallRuleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state firewallRuleResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Deleting DSM firewall rule", map[string]interface{}{
		"profile": state.Profile.ValueString(),
		"adapter": state.Adapter.ValueString(),
		"name":    state.Name.ValueString(),
	})

	result, err := r.client.DeleteFirewallRule(ctx,
		state.Profile.ValueString(),
		state.Adapter.ValueString(),
		state.Name.ValueString(),
		client.DeleteFirewallRuleOptions{
			AllowLockout:      state.AllowLockout.ValueBool(),
			AllowEmptyRuleSet: state.AllowEmptyRuleSet.ValueBool(),
		},
	)
	if err != nil {
		appendFirewallDiagnostic(&resp.Diagnostics, "Failed to delete firewall rule", err)
		return
	}
	appendLockoutWarning(&resp.Diagnostics, result.LockoutWarning)
}

func (r *firewallRuleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	profile, adapter, name, err := client.ParseFirewallRuleID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &firewallRuleResourceModel{
		ID:                types.StringValue(req.ID),
		Profile:           types.StringValue(profile),
		Adapter:           types.StringValue(adapter),
		Name:              types.StringValue(name),
		AllowLockout:      types.BoolValue(false),
		AllowEmptyRuleSet: types.BoolValue(false),
	})...)
}

// appendLockoutWarning surfaces an inconclusive lockout check. The write already
// happened by the time this is called — the point is that the operator must not
// read a silent success as "the provider checked and it is fine".
func appendLockoutWarning(diags *diag.Diagnostics, warning *client.IndeterminateLockoutError) {
	if warning == nil {
		return
	}
	diags.AddWarning(
		"Firewall lockout check was inconclusive",
		warning.Error()+
			"\n\nThe change was applied anyway, because the uncertainty comes from a rule this provider does not model "+
			"(a GeoIP or application-preset rule, typically) rather than from the change itself. Confirm you can still "+
			"reach DSM before closing this session.",
	)
}

// appendOrderConflictWarning reports two rules configured for the same position.
//
// The provider places rules by their configured priority rather than by the
// order Terraform happens to write them, which makes the resulting policy
// reproducible — but two rules cannot share one position, and for a firewall
// "below" is a different policy, so the tie is named rather than resolved out
// of sight.
func appendOrderConflictWarning(diags *diag.Diagnostics, conflict *client.FirewallOrderConflict) {
	if conflict == nil {
		return
	}
	diags.AddWarning(
		"Two firewall rules are configured with the same priority",
		conflict.Error()+
			"\n\nDSM matches rules top to bottom and stops at the first match, so which of the two comes first is a policy "+
			"decision this configuration has not made. Give each rule of a profile and adapter its own priority.",
	)
}

// appendUnreachablePriorityWarning reports a priority the profile is too short
// to hold.
//
// Priority is a position in DSM's list, not a sort key, so priorities numbered
// 10/20/30 — the usual way of leaving room between entries — cannot be honoured
// by a profile of three rules. The provider still orders those three correctly,
// but the position it reports on refresh will never equal the configured number,
// and applying the resulting diff cannot close the gap: there is nothing to put
// in the positions in between. Left unsaid, that reads as ordinary drift that
// the next apply will settle, and it never settles.
//
// Silent when the rule has no configured priority yet (a fresh import), and
// silent whenever the number is within reach — including when the rule merely
// sits somewhere else, which is real drift and shows up as an ordinary diff.
func appendUnreachablePriorityWarning(
	diags *diag.Diagnostics,
	profile, adapter, name string,
	configured types.Int64,
	actual, ruleCount int,
) {
	if configured.IsNull() || configured.IsUnknown() || ruleCount == 0 {
		return
	}
	last := int64(ruleCount - 1)
	if configured.ValueInt64() <= last {
		return
	}

	diags.AddWarning(
		"Firewall rule priority is beyond the end of the profile",
		fmt.Sprintf(
			"Rule %q is configured with priority %d, but profile %q adapter %q holds %d rule(s), so the highest position "+
				"that exists is %d. The rule sits at position %d.\n\n"+
				"Priority is the rule's position in DSM's list, not a sort key, so this diff will not settle by itself: "+
				"the next plan will show priority %d changing to %d, and applying it leaves the rule exactly where it is. "+
				"The rules that do exist are still ordered as configured. To clear it, number the rules of this profile "+
				"and adapter contiguously from 0 — counting any rules created in DSM itself — or add the missing rules.",
			name, configured.ValueInt64(), profile, adapter, ruleCount, last, actual,
			configured.ValueInt64(), actual),
	)
}

// appendFirewallDiagnostic turns the client's safety errors into diagnostics
// that say what to do about them.
func appendFirewallDiagnostic(diags *diag.Diagnostics, summary string, err error) {
	var lockout *client.LockoutError
	if errors.As(err, &lockout) {
		diags.AddError(
			"Refusing a firewall change that would lock out Terraform",
			lockout.Error()+
				"\n\nNothing was written. Either widen the rule so the address above keeps reaching the DSM port, or set "+
				"allow_lockout = true if losing this session is intended and you have another way back in.",
		)
		return
	}

	var empty *client.EmptyRuleSetError
	if errors.As(err, &empty) {
		diags.AddError(
			"Refusing to leave the firewall enabled with no rules",
			empty.Error()+
				"\n\nNothing was written. Turn the firewall off in DSM first, keep at least one allow rule, or set "+
				"allow_empty_rule_set = true if you really mean to deny everything.",
		)
		return
	}

	diags.AddError(summary, err.Error())
}
