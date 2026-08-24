package provider

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
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

func NewFirewallResource() resource.Resource {
	return &firewallResource{}
}

type firewallResource struct {
	client *client.Client
}

type firewallResourceModel struct {
	ID                     types.String `tfsdk:"id"`
	Profile                types.String `tfsdk:"profile"`
	Enabled                types.Bool   `tfsdk:"enabled"`
	DefaultPolicy          types.Map    `tfsdk:"default_policy"`
	DefaultPolicyEffective types.Map    `tfsdk:"default_policy_effective"`
	ActiveProfile          types.String `tfsdk:"active_profile"`
	AllowLockout           types.Bool   `tfsdk:"allow_lockout"`
	AllowEmptyRuleSet      types.Bool   `tfsdk:"allow_empty_rule_set"`
	DisableOnDestroy       types.Bool   `tfsdk:"disable_on_destroy"`
}

func (r *firewallResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_firewall"
}

func (r *firewallResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages the Synology DSM firewall at profile level: the global on/off switch, which profile is in " +
			"force, and that profile's default policy for traffic no rule matches. Individual rules are managed with " +
			"`dsm_firewall_rule`.",
		MarkdownDescription: "Manages the Synology DSM firewall at profile level — the settings that decide what a rule set " +
			"actually *means*:\n\n" +
			"* `enabled` — the global switch. While it is `false` no rule has any effect.\n" +
			"* `profile` — the profile in force. Rules in any other profile are stored but not enforced.\n" +
			"* `default_policy` — what DSM does with traffic that matches no rule (the \"Allow access\" / \"Deny access\" " +
			"choice at the bottom of the firewall page).\n\n" +
			"Individual rules are managed with `dsm_firewall_rule`.\n\n" +
			"~> **The firewall can lock you out, and this resource is the fastest way to do it.** Before switching the " +
			"firewall on, switching profiles, or tightening a default policy, the provider replays the resulting profile " +
			"against its own DSM session and refuses the change if that session would be denied. Set `allow_lockout = true` " +
			"to override. Recovering from a real lockout needs physical access to the NAS.\n\n" +
			"~> `SYNO.Core.Security.Firewall` is undocumented, and this provider has never run its **write** against a " +
			"NAS. The method and its two fields are confirmed from Synology's own webapi descriptor and firewall " +
			"library; the HTTP verb and the string encoding are not, so the provider tries each in turn and reports " +
			"clearly if DSM refuses them all. See the README before using this on a NAS you cannot reach physically.\n\n" +
			"~> `default_policy` is written by sending the whole profile back, so rules already in the profile travel with " +
			"it. They are handed back to DSM exactly as DSM sent them, which needs no encoding and is what the DSM web " +
			"interface itself does; the policy round trip is confirmed on 7.2.2. What the provider will not do is *render* " +
			"a rule into an adapter-keyed profile — no known encoding survives DSM's request parser — so a write that would " +
			"have to modify a rule is refused instead. See " +
			"[issue #130](https://github.com/batonogov/terraform-provider-synology-dsm/issues/130).",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Identifier of this resource: the managed profile's name.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"profile": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString("default"),
				Description: "Firewall profile this resource manages and puts in force. Only one `dsm_firewall` resource " +
					"should exist per DSM host: the switch and the active profile are NAS-wide.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"enabled": schema.BoolAttribute{
				Required: true,
				Description: "Whether the DSM firewall is switched on. Required rather than defaulted: turning a firewall " +
					"on or off is not something a configuration should do implicitly.",
			},
			"default_policy": schema.MapAttribute{
				Optional:    true,
				ElementType: types.StringType,
				Description: "What DSM does with traffic that matches no rule, keyed by network adapter — `allow`, `deny`, " +
					"or `none`. DSM stores this per adapter (`adapterPolicyMap`), not once per profile, so this is a map " +
					"rather than a single value. Adapters left out of the map keep whatever DSM has; use " +
					"`default_policy_effective` to see all of them. The `global` pseudo-interface is a pre-table rather " +
					"than an interface and normally carries `none`.",
				Validators: []validator.Map{
					newMapValuesOneOfValidator(
						client.FirewallPolicyAllow,
						client.FirewallPolicyDeny,
						client.FirewallPolicyNone,
					),
				},
			},
			"default_policy_effective": schema.MapAttribute{
				Computed:    true,
				ElementType: types.StringType,
				Description: "Every adapter's actual default policy as DSM holds it, including adapters this resource does " +
					"not manage. Read-only companion to `default_policy`, for auditing the whole profile.",
			},
			"active_profile": schema.StringAttribute{
				Computed: true,
				Description: "The profile DSM currently has in force. Normally equal to `profile`; a difference means the " +
					"active profile was changed outside Terraform, and the next apply switches it back.",
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
				Description: "Allow switching the firewall on while the profile in force has no rules at all. Without it " +
					"that is refused, because an enabled profile with no rules falls through to each adapter's default " +
					"policy and typically denies everything.",
			},
			"disable_on_destroy": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				Description: "Whether `terraform destroy` switches the firewall off. Defaults to `false`: removing a " +
					"security control from Terraform's management is not a reason to remove it from the NAS, and silently " +
					"disabling a firewall is the one destroy behaviour nobody would want by accident. The profile and its " +
					"rules are never deleted either way.",
			},
		},
	}
}

func (r *firewallResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	dsmClient := clientFromProviderData(req.ProviderData, &resp.Diagnostics)
	if dsmClient == nil {
		return
	}

	r.client = dsmClient
}

// ModifyPlan marks the computed attributes an apply rewrites as unknown, and is
// what turns an out-of-band profile switch into a plan.
//
// Terraform carries a computed attribute forward from prior state when the plan
// does not say otherwise. Two consequences matter here. First, an apply that
// changes `default_policy_effective` while the plan promised the old value is an
// "inconsistent result after apply" error. Second — and this is the useful part —
// `active_profile` differing from `profile` is drift that no *configured*
// attribute reflects, so without marking it unknown Terraform would report the
// difference during refresh and then plan no change at all.
func (r *firewallResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	// Destroy, or create: nothing carried forward to correct.
	if req.Plan.Raw.IsNull() || req.State.Raw.IsNull() {
		return
	}

	var plan, state firewallResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	willWrite := !plan.Enabled.Equal(state.Enabled) ||
		!plan.Profile.Equal(state.Profile) ||
		!plan.DefaultPolicy.Equal(state.DefaultPolicy) ||
		state.ActiveProfile.ValueString() != plan.Profile.ValueString()

	if !willWrite {
		return
	}

	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("active_profile"), types.StringUnknown())...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("default_policy_effective"),
		types.MapUnknown(types.StringType))...)
}

func (r *firewallResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan firewallResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Configuring the DSM firewall", map[string]interface{}{
		"profile": plan.Profile.ValueString(),
		"enabled": plan.Enabled.ValueBool(),
	})

	r.apply(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *firewallResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state firewallResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	profileName := state.Profile.ValueString()
	if id := state.ID.ValueString(); id != "" {
		profileName = id
	}

	tflog.Debug(ctx, "Reading the DSM firewall configuration", map[string]interface{}{"profile": profileName})

	settings, err := r.client.GetFirewallSettings(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read firewall settings", err.Error())
		return
	}
	profile, err := r.client.GetFirewallProfile(ctx, profileName)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read firewall profile", err.Error())
		return
	}

	state.ID = types.StringValue(profileName)
	state.Profile = types.StringValue(profileName)
	state.Enabled = types.BoolValue(settings.Enabled)
	state.ActiveProfile = types.StringValue(settings.ActiveProfile)

	effective, diags := stringMapValue(ctx, profile.DefaultPolicyNames())
	resp.Diagnostics.Append(diags...)
	state.DefaultPolicyEffective = effective

	managed, diags := refreshManagedPolicy(ctx, state.DefaultPolicy, profile)
	resp.Diagnostics.Append(diags...)
	state.DefaultPolicy = managed

	// Present in state only after this provider wrote them; an imported resource
	// starts from the safe defaults.
	if state.AllowLockout.IsNull() {
		state.AllowLockout = types.BoolValue(false)
	}
	if state.AllowEmptyRuleSet.IsNull() {
		state.AllowEmptyRuleSet = types.BoolValue(false)
	}
	if state.DisableOnDestroy.IsNull() {
		state.DisableOnDestroy = types.BoolValue(false)
	}

	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *firewallResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan firewallResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Updating the DSM firewall", map[string]interface{}{
		"profile": plan.Profile.ValueString(),
		"enabled": plan.Enabled.ValueBool(),
	})

	r.apply(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

// apply writes the plan to DSM and fills the computed attributes from what DSM
// reports back. Create and Update are identical: DSM has one firewall, and this
// resource asserts its configuration rather than adding anything to it.
func (r *firewallResource) apply(ctx context.Context, plan *firewallResourceModel, diags *diag.Diagnostics) {
	policy, d := stringMapFromValue(ctx, plan.DefaultPolicy)
	diags.Append(d...)
	if diags.HasError() {
		return
	}

	result, err := r.client.SetFirewall(ctx, client.SetFirewallRequest{
		Profile:           plan.Profile.ValueString(),
		Enabled:           plan.Enabled.ValueBool(),
		DefaultPolicy:     policy,
		AllowLockout:      plan.AllowLockout.ValueBool(),
		AllowEmptyRuleSet: plan.AllowEmptyRuleSet.ValueBool(),
	})
	if err != nil {
		appendFirewallSettingsDiagnostic(diags, "Failed to configure the firewall", err)
		return
	}
	appendLockoutWarning(diags, result.LockoutWarning)

	plan.ID = types.StringValue(plan.Profile.ValueString())
	plan.Enabled = types.BoolValue(result.Settings.Enabled)
	plan.ActiveProfile = types.StringValue(result.Settings.ActiveProfile)

	effective, d := stringMapValue(ctx, result.Profile.DefaultPolicyNames())
	diags.Append(d...)
	plan.DefaultPolicyEffective = effective
}

// Delete leaves the firewall exactly as it is unless disable_on_destroy says
// otherwise.
//
// The asymmetry with dsm_user_home_service, where the destroy default is to undo
// the change, is deliberate. There the risk is leaving a service switched on that
// the configuration turned on; here the change being undone would *remove* a
// security control, and a `terraform destroy` that quietly opens a NAS to the
// network is not a default anybody would choose knowingly. Rules and profiles are
// never touched either way — they are `dsm_firewall_rule`'s to remove.
func (r *firewallResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state firewallResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !state.DisableOnDestroy.ValueBool() {
		tflog.Info(ctx, "Removing dsm_firewall from state; the DSM firewall is left switched as it is",
			map[string]interface{}{"enabled": state.Enabled.ValueBool()})
		resp.Diagnostics.AddWarning(
			"The DSM firewall was left as it is",
			"dsm_firewall has been removed from state, but the firewall itself is unchanged and still enforcing its "+
				"rules. That is the default because destroying a Terraform resource should not silently switch a "+
				"security control off. Set disable_on_destroy = true if you want destroy to turn the firewall off.",
		)
		return
	}

	tflog.Info(ctx, "Switching the DSM firewall off on destroy", map[string]interface{}{
		"profile": state.Profile.ValueString(),
	})

	// Switching the firewall off can never deny a packet, so neither guard has
	// anything to say about it; passing the opt-ins keeps a stale rule set from
	// blocking the disable.
	if _, err := r.client.SetFirewall(ctx, client.SetFirewallRequest{
		Profile:           state.Profile.ValueString(),
		Enabled:           false,
		AllowLockout:      true,
		AllowEmptyRuleSet: true,
	}); err != nil {
		appendFirewallSettingsDiagnostic(&resp.Diagnostics, "Failed to switch the firewall off", err)
	}
}

func (r *firewallResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// The import ID is the profile name; everything else is read back from DSM.
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("profile"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("allow_lockout"), false)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("allow_empty_rule_set"), false)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("disable_on_destroy"), false)...)
}

// refreshManagedPolicy re-reads the adapters the configuration actually manages.
//
// Only the keys already in state are looked up: reporting every adapter DSM has
// would turn a partial map into a permanent diff. A key DSM no longer records is
// dropped rather than kept at its old value — the difference from the
// configuration is then a plan, which rewrites it, instead of a comfortable lie
// in state.
func refreshManagedPolicy(ctx context.Context, managed types.Map, profile *client.FirewallProfile) (types.Map, diag.Diagnostics) {
	var diags diag.Diagnostics
	if managed.IsNull() || managed.IsUnknown() {
		return managed, diags
	}

	elements := managed.Elements()
	refreshed := make(map[string]string, len(elements))
	for adapter := range elements {
		if name, ok := profile.DefaultPolicyName(adapter); ok {
			refreshed[adapter] = name
		}
	}

	value, d := stringMapValue(ctx, refreshed)
	diags.Append(d...)
	return value, diags
}

func stringMapValue(ctx context.Context, values map[string]string) (types.Map, diag.Diagnostics) {
	if values == nil {
		values = map[string]string{}
	}
	return types.MapValueFrom(ctx, types.StringType, values)
}

func stringMapFromValue(ctx context.Context, value types.Map) (map[string]string, diag.Diagnostics) {
	var diags diag.Diagnostics
	if value.IsNull() || value.IsUnknown() {
		return nil, diags
	}

	out := make(map[string]string, len(value.Elements()))
	diags.Append(value.ElementsAs(ctx, &out, false)...)
	return out, diags
}

// appendFirewallSettingsDiagnostic explains the two refusals this resource can
// hit. It is separate from appendFirewallDiagnostic because the way out of each
// is different when the change is "switch the firewall on" rather than "write a
// rule".
func appendFirewallSettingsDiagnostic(diags *diag.Diagnostics, summary string, err error) {
	var lockout *client.LockoutError
	if errors.As(err, &lockout) {
		diags.AddError(
			"Refusing a firewall change that would lock out Terraform",
			lockout.Error()+
				"\n\nNothing was written. Add a rule that lets the address above reach the DSM port — "+
				"`dsm_firewall_rule` with the firewall still switched off is the safe order — or set "+
				"allow_lockout = true if losing this session is intended and you have another way back in.",
		)
		return
	}

	var empty *client.EmptyRuleSetError
	if errors.As(err, &empty) {
		diags.AddError(
			"Refusing to switch the firewall on with no rules",
			empty.Error()+
				"\n\nNothing was written. Create the rules first and switch the firewall on in a second step, or set "+
				"allow_empty_rule_set = true if denying everything is really the intent.",
		)
		return
	}

	var unsupported *client.FirewallRuleWriteUnsupportedError
	if errors.As(err, &unsupported) {
		diags.AddError(
			"This DSM's firewall rules cannot be written by the provider",
			unsupported.Error()+
				"\n\nNothing was written. Rules DSM sent are handed back untouched, so an ordinary default-policy change goes "+
				"through even on a profile full of rules; this refusal means the write would have had to render a rule — one "+
				"is being created or edited, or an entry in the profile could not be read and would have been dropped. Make "+
				"that rule change in Control Panel -> Security -> Firewall for now, and see "+
				"https://github.com/batonogov/terraform-provider-synology-dsm/issues/130 for the one capture that would lift "+
				"this.",
		)
		return
	}

	var notPersisted *client.FirewallSettingsNotPersistedError
	if errors.As(err, &notPersisted) {
		diags.AddError(
			"DSM accepted the firewall settings and did not keep them",
			notPersisted.Error()+
				"\n\nThe call was made and answered with success; reading the settings straight back showed something else, "+
				"so this apply is reported as a failure rather than recorded as done. The write contract for "+
				"SYNO.Core.Security.Firewall is reverse-engineered and has never been verified against physical hardware.\n\n"+
				"Check Control Panel -> Security -> Firewall for what the NAS actually has, and please report this at "+
				"https://github.com/batonogov/terraform-provider-synology-dsm/issues with your exact DSM version, the NAS "+
				"model, and whether the account is the built-in admin (issue #130).",
		)
		return
	}

	diags.AddError(summary, err.Error())
}

// mapValuesOneOfValidator is the map counterpart of stringOneOfValidator: it
// checks every value of a map attribute against a fixed set and names the key
// that failed, which matters when the key is an adapter the operator typed.
type mapValuesOneOfValidator struct {
	allowed []string
}

func newMapValuesOneOfValidator(allowed ...string) mapValuesOneOfValidator {
	return mapValuesOneOfValidator{allowed: allowed}
}

func (v mapValuesOneOfValidator) Description(_ context.Context) string {
	return fmt.Sprintf("every value must be one of: %s", strings.Join(v.allowed, ", "))
}

func (v mapValuesOneOfValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v mapValuesOneOfValidator) ValidateMap(ctx context.Context, req validator.MapRequest, resp *validator.MapResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}

	values := make(map[string]string, len(req.ConfigValue.Elements()))
	resp.Diagnostics.Append(req.ConfigValue.ElementsAs(ctx, &values, false)...)
	if resp.Diagnostics.HasError() {
		return
	}

	for key, value := range values {
		if slices.Contains(v.allowed, value) {
			continue
		}
		resp.Diagnostics.AddAttributeError(
			req.Path.AtMapKey(key),
			"Invalid value",
			fmt.Sprintf("value for %q must be one of [%s], got: %q", key, strings.Join(v.allowed, ", "), value),
		)
	}
}
