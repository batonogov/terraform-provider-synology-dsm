package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

// systemSettingsID is the fixed ID of this singleton resource: DSM date/time
// settings are NAS-wide, so there is nothing to key them by.
const systemSettingsID = "system_settings"

func NewSystemSettingsResource() resource.Resource {
	return &systemSettingsResource{}
}

type systemSettingsResource struct {
	client *client.Client
}

type systemSettingsResourceModel struct {
	ID         types.String `tfsdk:"id"`
	Timezone   types.String `tfsdk:"timezone"`
	NTPEnabled types.Bool   `tfsdk:"ntp_enabled"`
	NTPServer  types.String `tfsdk:"ntp_server"`
}

func (r *systemSettingsResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_system_settings"
}

func (r *systemSettingsResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages the DSM system date and time settings (Control Panel -> Regional Options -> Time): " +
			"time zone and NTP synchronisation. These are NAS-wide settings, so only one instance of this resource " +
			"should exist per DSM host.\n\n" +
			"Every attribute is optional: an attribute left out of the configuration keeps whatever DSM currently has, " +
			"and is recorded in state so it does not show as a diff. DSM rejects a partial write, so the provider always " +
			"reads the current settings and sends the complete set back with the requested changes applied.\n\n" +
			"`terraform destroy` only removes the resource from state: the NAS clock configuration is never reset.",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Fixed identifier of this singleton resource (`system_settings`).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"timezone": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Description: "DSM time zone name, for example `Moscow` or `Amman`. These are Synology's own zone " +
					"names, not IANA identifiers: use `Moscow`, not `Europe/Moscow`. The value is passed through to " +
					"DSM unchanged; an unknown name is rejected by DSM with error 5701.",
			},
			"ntp_enabled": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Description: "Whether the NAS clock is synchronised with an NTP server. When `false`, DSM keeps the " +
					"clock set manually. Note that DSM encodes this as a mode string rather than a boolean and only " +
					"the enabled value is confirmed, so setting this to `false` relies on a value the provider has " +
					"inferred; it emits a warning when it does.",
			},
			"ntp_server": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Description: "NTP server to synchronise with, for example `time.google.com` or `pool.ntp.org`. " +
					"DSM keeps the last configured server even while `ntp_enabled` is `false`.",
			},
		},
	}
}

func (r *systemSettingsResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	dsmClient, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Provider Data",
			fmt.Sprintf("Expected *client.Client, got: %T", req.ProviderData),
		)
		return
	}

	r.client = dsmClient
}

func (r *systemSettingsResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan systemSettingsResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var config systemSettingsResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.apply(ctx, &config, &plan, &resp.Diagnostics, "create")
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *systemSettingsResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state systemSettingsResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Debug(ctx, "Reading DSM system settings")

	settings, err := r.client.GetSystemSettings(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read system settings", systemSettingsErrorDetail(ctx, r.client, err, ""))
		return
	}

	state.ID = types.StringValue(systemSettingsID)
	state.Timezone = types.StringValue(settings.Timezone)
	state.NTPEnabled = types.BoolValue(settings.NTPEnabled)
	state.NTPServer = types.StringValue(settings.NTPServer)

	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *systemSettingsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan systemSettingsResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var config systemSettingsResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.apply(ctx, &config, &plan, &resp.Diagnostics, "update")
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

// Delete is deliberately a no-op. There is no "unset" for a time zone or an NTP
// server: resetting them would mean inventing a value, and getting the NAS clock
// wrong breaks certificate validation, scheduled tasks and log correlation.
// Removing the resource therefore only drops it from state.
func (r *systemSettingsResource) Delete(ctx context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
	tflog.Info(ctx, "Removing dsm_system_settings from state; DSM date and time settings are left unchanged")
}

func (r *systemSettingsResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// A singleton: the supplied ID carries no information, so any value is
	// accepted and normalised to the fixed one. The rest of the state is read
	// back from DSM.
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), systemSettingsID)...)
}

// apply writes the configured attributes to DSM and refreshes the plan with what
// DSM reports back.
//
// Only attributes present in the *configuration* are sent as changes; the rest
// are left to DSM. The plan is not a reliable source here: for an
// Optional+Computed attribute the plan carries the prior state value when the
// config is null, which would make Terraform re-assert settings the user never
// asked to manage.
func (r *systemSettingsResource) apply(ctx context.Context, config, plan *systemSettingsResourceModel, diags *diag.Diagnostics, action string) {
	req := client.SetSystemSettingsRequest{}

	if !config.Timezone.IsNull() && !config.Timezone.IsUnknown() {
		v := config.Timezone.ValueString()
		req.Timezone = &v
	}
	if !config.NTPEnabled.IsNull() && !config.NTPEnabled.IsUnknown() {
		v := config.NTPEnabled.ValueBool()
		req.NTPEnabled = &v

		if !v {
			// Only the enabled spelling of DSM's `enable_ntp` field has ever
			// been captured from hardware ("ntp"). Turning synchronisation off
			// therefore relies on a value the provider has inferred, unless the
			// NAS is already off and has shown us its own. Say so rather than
			// let it fail mysteriously later.
			diags.AddAttributeWarning(
				path.Root("ntp_enabled"),
				"Disabling NTP uses an inferred DSM value",
				"DSM reports its clock synchronisation mode as a string, and only the enabled value (\"ntp\") has "+
					"been confirmed against hardware. If DSM rejects this change with error 5701, set the mode "+
					"once in Control Panel -> Regional Options -> Time; the provider then reuses DSM's own value "+
					"for the disabled state.",
			)
		}
	}
	if !config.NTPServer.IsNull() && !config.NTPServer.IsUnknown() {
		v := config.NTPServer.ValueString()
		req.NTPServer = &v
	}

	tflog.Info(ctx, "Applying DSM system settings", map[string]interface{}{
		"action":          action,
		"timezone_set":    req.Timezone != nil,
		"ntp_enabled_set": req.NTPEnabled != nil,
		"ntp_server_set":  req.NTPServer != nil,
	})

	settings, err := r.client.SetSystemSettings(ctx, req)
	if err != nil {
		requested := ""
		if req.Timezone != nil {
			requested = *req.Timezone
		}
		diags.AddError(fmt.Sprintf("Failed to %s system settings", action), systemSettingsErrorDetail(ctx, r.client, err, requested))
		return
	}

	plan.ID = types.StringValue(systemSettingsID)
	plan.Timezone = types.StringValue(settings.Timezone)
	plan.NTPEnabled = types.BoolValue(settings.NTPEnabled)
	plan.NTPServer = types.StringValue(settings.NTPServer)
}

// systemSettingsErrorDetail turns DSM's misleading error text into something
// actionable. DSM renders 5701 as "please sign in to DSM again", but the session
// is fine: the request was malformed or a value was not one DSM knows.
//
// zoneLister is consulted only on 5701 and only to enrich the message; any
// failure there is ignored, because a broken hint must not replace a real error.
// It may be nil.
func systemSettingsErrorDetail(ctx context.Context, zoneLister timezoneLister, err error, requestedTimezone string) string {
	msg := err.Error()

	switch {
	case client.IsAPIError(err, 119):
		return msg + "\n\nSYNO.Core.Region.NTP has also been reported to answer 119 for administrator accounts " +
			"other than the built-in `admin`, even with a valid session — the same restriction as " +
			"SYNO.Core.User.Home. Connect the provider with the built-in administrator account, or set the time " +
			"in DSM: Control Panel -> Regional Options -> Time."

	case !client.IsAPIError(err, 5701):
		return msg
	}

	detail := msg + "\n\nDSM reports 5701 for a time settings request it cannot apply. " +
		"Despite the wording DSM attaches to this code, it does not mean the session expired."

	if requestedTimezone == "" {
		return detail
	}

	return detail + timezoneHint(ctx, zoneLister, requestedTimezone)
}

// timezoneLister is the slice of the DSM client this package needs to enumerate
// zone names. Narrowing it to an interface keeps the hint testable without a
// live DSM.
type timezoneLister interface {
	ListTimezones(ctx context.Context) ([]client.Timezone, error)
}

// timezoneHint explains a rejected time zone as concretely as the NAS allows:
// with the real zone list when DSM will produce one, and with a name-shape
// heuristic when it will not.
func timezoneHint(ctx context.Context, zoneLister timezoneLister, requested string) string {
	if zoneLister != nil {
		if zones, err := zoneLister.ListTimezones(ctx); err == nil && len(zones) > 0 {
			if timezoneKnown(zones, requested) {
				// The zone is valid, so it is not what DSM objected to.
				return fmt.Sprintf("\n\nDSM does recognise the time zone %q, so the rejected part of the request "+
					"is something else — most likely the NTP server.", requested)
			}
			if suggestion, ok := closestTimezone(zones, requested); ok {
				return fmt.Sprintf("\n\nDSM does not know the time zone %q. Its own list contains %q — "+
					"DSM uses Synology's short names, not IANA identifiers.", requested, suggestion)
			}
			return fmt.Sprintf("\n\nDSM does not know the time zone %q, and nothing in its list is close. "+
				"DSM uses Synology's short names such as %q, not IANA identifiers; the exact spelling is the one "+
				"shown in Control Panel -> Regional Options -> Time.", requested, zones[0].Value)
		}
	}

	if suggestion, ok := suggestDSMTimezone(requested); ok {
		return fmt.Sprintf("\n\nThe most likely cause is the time zone name: DSM uses Synology's short names, "+
			"not IANA identifiers. Try %q instead of %q.", suggestion, requested)
	}
	return fmt.Sprintf("\n\nCheck that %q is a time zone name DSM knows: the exact spelling is the one "+
		"shown in Control Panel -> Regional Options -> Time.", requested)
}

func timezoneKnown(zones []client.Timezone, requested string) bool {
	for _, z := range zones {
		if strings.EqualFold(z.Value, requested) {
			return true
		}
	}
	return false
}

// closestTimezone looks for the DSM name behind an IANA-shaped identifier:
// "Europe/Moscow" -> "Moscow", "America/New_York" -> "New York". Only an exact
// (case-insensitive) hit on the real list is offered — a fuzzy near-miss would
// be a worse hint than none.
func closestTimezone(zones []client.Timezone, requested string) (string, bool) {
	candidates := []string{requested}
	if bare, ok := suggestDSMTimezone(requested); ok {
		candidates = append(candidates, bare)
	}

	for _, candidate := range candidates {
		for _, z := range zones {
			if strings.EqualFold(z.Value, candidate) {
				return z.Value, true
			}
		}
	}
	return "", false
}

// suggestDSMTimezone maps an IANA-looking identifier ("Europe/Moscow") to the
// bare name DSM is likely to expect ("Moscow"). It is a hint for an error
// message only — nothing is rewritten behind the user's back, because the DSM
// zone list is not a plain suffix of the IANA one.
func suggestDSMTimezone(tz string) (string, bool) {
	if tz == "" || !strings.Contains(tz, "/") {
		return "", false
	}

	parts := strings.Split(tz, "/")
	last := parts[len(parts)-1]
	if last == "" {
		return "", false
	}

	// IANA writes multi-word zones with underscores ("America/New_York"); DSM
	// spells them out.
	return strings.ReplaceAll(last, "_", " "), true
}
