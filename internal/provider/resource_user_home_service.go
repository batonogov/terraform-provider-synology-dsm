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
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

// userHomeServiceID is the fixed ID of this singleton resource: the user home
// service is a single NAS-wide setting, so there is nothing to key it by.
const userHomeServiceID = "user_home_service"

func NewUserHomeServiceResource() resource.Resource {
	return &userHomeServiceResource{}
}

type userHomeServiceResource struct {
	client *client.Client
}

type userHomeServiceResourceModel struct {
	ID               types.String `tfsdk:"id"`
	Enable           types.Bool   `tfsdk:"enable"`
	Location         types.String `tfsdk:"location"`
	EnableRecycleBin types.Bool   `tfsdk:"enable_recycle_bin"`
	Force            types.Bool   `tfsdk:"force"`
	DisableOnDestroy types.Bool   `tfsdk:"disable_on_destroy"`
}

func (r *userHomeServiceResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_user_home_service"
}

func (r *userHomeServiceResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages the DSM user home service, which creates per-user home folders under the `homes` shared folder. " +
			"This is a single NAS-wide setting, so only one instance of this resource should exist per DSM host.",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Fixed identifier of this singleton resource (`user_home_service`).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"location": schema.StringAttribute{
				Required: true,
				Description: "Volume path hosting the `homes` shared folder, e.g. `/volume1`. " +
					"Must be a path: a bare volume name such as `volume1` is rejected by DSM with error 3101.",
			},
			"enable": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(true),
				Description: "Whether the user home service is enabled. Defaults to `true`.",
			},
			"enable_recycle_bin": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "Enable the recycle bin on the `homes` shared folder. Defaults to `false`.",
			},
			"force": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				Description: "Pass the `force` flag to DSM to proceed past soft warnings (for example when Synology Drive " +
					"or Photos already depend on the home service). Defaults to `false`. Not read back from DSM.",
			},
			"disable_on_destroy": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				Description: "Whether `terraform destroy` should actually turn the service off. Defaults to `false`, " +
					"in which case destroy only drops the resource from state and leaves DSM untouched. Disabling the " +
					"service is a NAS-wide action: it takes personal folders away from every user and breaks Synology " +
					"Drive and Photos, which depend on them. Files under the `homes` shared folder are not deleted either way.",
			},
		},
	}
}

func (r *userHomeServiceResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *userHomeServiceResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan userHomeServiceResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.apply(ctx, &plan, &resp.Diagnostics, "create")
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *userHomeServiceResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state userHomeServiceResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Debug(ctx, "Reading DSM user home service")

	svc, err := r.client.GetUserHomeService(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read user home service", userHomeErrorDetail(err))
		return
	}

	state.ID = types.StringValue(userHomeServiceID)
	state.Enable = types.BoolValue(svc.Enable)
	state.Location = types.StringValue(svc.Location)
	state.EnableRecycleBin = types.BoolValue(svc.EnableRecycleBin)

	// force and disable_on_destroy steer this provider's behaviour and have no
	// counterpart in DSM, so they are carried over from prior state. After an
	// import there is no prior state, hence the explicit defaults.
	if state.Force.IsNull() {
		state.Force = types.BoolValue(false)
	}
	if state.DisableOnDestroy.IsNull() {
		state.DisableOnDestroy = types.BoolValue(false)
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *userHomeServiceResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan userHomeServiceResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.apply(ctx, &plan, &resp.Diagnostics, "update")
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *userHomeServiceResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state userHomeServiceResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !state.DisableOnDestroy.ValueBool() {
		tflog.Warn(ctx, "Leaving the DSM user home service enabled: disable_on_destroy is false. "+
			"Set disable_on_destroy = true if Terraform should turn the service off on destroy.")
		return
	}

	tflog.Info(ctx, "Disabling DSM user home service")

	if _, err := r.client.SetUserHomeService(ctx, client.SetUserHomeServiceRequest{Enable: false}); err != nil {
		resp.Diagnostics.AddError("Failed to disable user home service", userHomeErrorDetail(err))
	}
}

func (r *userHomeServiceResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// The service is a singleton, so the supplied ID carries no information —
	// the whole state is read back from DSM. Any ID is accepted for convenience
	// and normalised to the fixed one.
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), userHomeServiceID)...)
}

// apply writes the planned configuration to DSM and refreshes the plan with the
// values DSM reports back.
func (r *userHomeServiceResource) apply(ctx context.Context, plan *userHomeServiceResourceModel, diags *diag.Diagnostics, action string) {
	tflog.Info(ctx, "Applying DSM user home service", map[string]interface{}{
		"action":             action,
		"enable":             plan.Enable.ValueBool(),
		"location":           plan.Location.ValueString(),
		"enable_recycle_bin": plan.EnableRecycleBin.ValueBool(),
	})

	svc, err := r.client.SetUserHomeService(ctx, client.SetUserHomeServiceRequest{
		Enable:           plan.Enable.ValueBool(),
		Location:         plan.Location.ValueString(),
		EnableRecycleBin: plan.EnableRecycleBin.ValueBool(),
		Force:            plan.Force.ValueBool(),
	})
	if err != nil {
		diags.AddError(fmt.Sprintf("Failed to %s user home service", action), userHomeErrorDetail(err))
		return
	}

	plan.ID = types.StringValue(userHomeServiceID)
	plan.Enable = types.BoolValue(svc.Enable)
	plan.EnableRecycleBin = types.BoolValue(svc.EnableRecycleBin)

	// DSM keeps the configured location even while the service is off, and
	// reports it verbatim. Trust the plan when DSM has nothing to report (a
	// never-enabled service returns an empty location).
	if svc.Location != "" {
		plan.Location = types.StringValue(svc.Location)
	}
}

// userHomeErrorDetail augments raw DSM error codes with the cause, which the
// numeric codes alone do not convey.
func userHomeErrorDetail(err error) string {
	msg := err.Error()

	switch {
	case strings.Contains(msg, "api error 3101"):
		return msg + "\n\nDSM error 3101 usually means `location` is not a volume path. " +
			"Use `/volume1`, not `volume1`."
	case strings.Contains(msg, "api error 3103"):
		return msg + "\n\nDSM error 3103 means the `location` parameter is missing. " +
			"It is required whenever `enable` is true."
	case strings.Contains(msg, "api error 119"):
		return msg + "\n\nSYNO.Core.User.Home also returns error 119 when the session is valid but the account " +
			"is not the built-in `admin`. Connect the provider with the built-in administrator account, or enable " +
			"the service manually in DSM: Control Panel → User & Group → Advanced → User Home."
	default:
		return msg
	}
}
