package provider

import (
	"context"
	"fmt"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

func NewSharedFolderResource() resource.Resource {
	return &sharedFolderResource{}
}

type sharedFolderResource struct {
	client *client.Client
}

type sharedFolderResourceModel struct {
	ID                  types.String `tfsdk:"id"`
	Name                types.String `tfsdk:"name"`
	VolPath             types.String `tfsdk:"vol_path"`
	Description         types.String `tfsdk:"description"`
	Hidden              types.Bool   `tfsdk:"hidden"`
	EnableRecycleBin    types.Bool   `tfsdk:"enable_recycle_bin"`
	RecycleBinAdminOnly types.Bool   `tfsdk:"recycle_bin_admin_only"`
	EnableShareCompress types.Bool   `tfsdk:"enable_share_compress"`
	EnableShareCow      types.Bool   `tfsdk:"enable_share_cow"`
	ShareQuota          types.Int64  `tfsdk:"share_quota"`
	UUID                types.String `tfsdk:"uuid"`
}

func (r *sharedFolderResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_shared_folder"
}

func (r *sharedFolderResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a shared folder on Synology DSM.",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Unique identifier for the shared folder (name).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Name of the shared folder.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"vol_path": schema.StringAttribute{
				Required:    true,
				Description: "Volume path (e.g. /volume1).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"description": schema.StringAttribute{
				Optional:    true,
				Description: "Description of the shared folder.",
			},
			"hidden": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "Hide the shared folder in network browsing.",
			},
			"enable_recycle_bin": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(true),
				Description: "Enable recycle bin for the shared folder.",
			},
			"recycle_bin_admin_only": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(true),
				Description: "Restrict recycle bin access to administrators. Only meaningful when `enable_recycle_bin` is true.",
			},
			"enable_share_compress": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				Description: "Enable file compression on the shared folder (Btrfs volumes). DSM can only turn this " +
					"on when the folder is created, so switching it from `false` to `true` forces replacement — " +
					"**which destroys the folder and its contents**. Turning it off is applied in place.",
				PlanModifiers: []planmodifier.Bool{
					requiresReplaceWhenEnabled(),
				},
			},
			"enable_share_cow": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				Description: "Enable data checksum for advanced data integrity, DSM's copy-on-write setting " +
					"(Btrfs volumes). This is what backs file self-healing and snapshot consistency. As with " +
					"`enable_share_compress`, DSM only accepts it at creation time, so turning it on forces " +
					"replacement — **which destroys the folder and its contents**. Turning it off is applied in place.",
				PlanModifiers: []planmodifier.Bool{
					requiresReplaceWhenEnabled(),
				},
			},
			"share_quota": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(0),
				Description: "Quota for the whole shared folder, **in gigabytes**. `0` means unlimited. " +
					"DSM accepts this as `share_quota` and reports it back as `quota_value`.",
				Validators: []validator.Int64{
					newInt64AtLeastValidator(0),
				},
			},
			"uuid": schema.StringAttribute{
				Computed:    true,
				Description: "UUID assigned by DSM.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

func (r *sharedFolderResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *sharedFolderResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan sharedFolderResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Creating DSM shared folder", map[string]interface{}{
		"name": plan.Name.ValueString(),
	})

	share, err := r.client.CreateShare(ctx, shareRequestFromPlan(plan, plan.Name.ValueString(), plan.VolPath.ValueString()))
	if err != nil {
		resp.Diagnostics.AddError(
			"Failed to create shared folder",
			err.Error(),
		)
		return
	}

	applyShareToPlan(&plan, share)

	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

// ValidateConfig rejects a combination DSM itself refuses: compression on a
// share without the copy-on-write/checksum feature. Caught here, it surfaces at
// plan time with an explanation instead of a bare API failure during apply.
func (r *sharedFolderResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config sharedFolderResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Unknown values are resolved later; an unset enable_share_cow defaults to
	// false, which is exactly the combination to reject.
	if config.EnableShareCompress.IsUnknown() || config.EnableShareCow.IsUnknown() {
		return
	}
	if !config.EnableShareCompress.ValueBool() {
		return
	}

	if config.EnableShareCow.IsNull() || !config.EnableShareCow.ValueBool() {
		resp.Diagnostics.AddAttributeError(
			path.Root("enable_share_compress"),
			"enable_share_compress requires enable_share_cow",
			"DSM refuses to create a compressed shared folder unless data checksum for advanced data integrity "+
				"(copy-on-write) is enabled as well. Set enable_share_cow = true alongside enable_share_compress.",
		)
	}
}

// requiresReplaceWhenEnabled forces replacement only when a flag goes from off
// to on. DSM accepts enable_share_compress and enable_share_cow when a share is
// created and silently ignores them afterwards (the set call answers success
// while the value stays false), so an in-place enable would leave Terraform
// reporting a state DSM never adopted. Disabling does work in place, so that
// direction must not destroy the folder.
func requiresReplaceWhenEnabled() planmodifier.Bool {
	const description = "Requires replacement when enabled, because DSM only accepts this setting at creation time."
	return boolplanmodifier.RequiresReplaceIf(replaceIfTurnedOn, description, description)
}

// replaceIfTurnedOn is the predicate behind requiresReplaceWhenEnabled: replace
// only on the false -> true transition.
func replaceIfTurnedOn(_ context.Context, req planmodifier.BoolRequest, resp *boolplanmodifier.RequiresReplaceIfFuncResponse) {
	resp.RequiresReplace = !req.StateValue.ValueBool() && req.PlanValue.ValueBool()
}

// shareRequestFromPlan maps a planned resource model onto a client request.
// name and volPath are passed separately because an update must keep the ones
// already in state rather than take them from the plan.
func shareRequestFromPlan(plan sharedFolderResourceModel, name, volPath string) client.CreateShareRequest {
	return client.CreateShareRequest{
		Name:                name,
		VolPath:             volPath,
		Description:         plan.Description.ValueString(),
		Hidden:              plan.Hidden.ValueBool(),
		EnableRecycleBin:    plan.EnableRecycleBin.ValueBool(),
		RecycleBinAdminOnly: plan.RecycleBinAdminOnly.ValueBool(),
		EnableShareCompress: plan.EnableShareCompress.ValueBool(),
		EnableShareCow:      plan.EnableShareCow.ValueBool(),
		ShareQuota:          plan.ShareQuota.ValueInt64(),
	}
}

// applyShareToPlan copies the values DSM reported back into the model, so state
// reflects what the NAS actually stored rather than what was requested.
func applyShareToPlan(plan *sharedFolderResourceModel, share *client.Share) {
	plan.ID = types.StringValue(share.Name)
	plan.UUID = types.StringValue(share.UUID)
	plan.Hidden = types.BoolValue(share.Hidden)
	plan.EnableRecycleBin = types.BoolValue(share.EnableRecycleBin)
	plan.RecycleBinAdminOnly = types.BoolValue(share.RecycleBinAdminOnly)
	plan.EnableShareCompress = types.BoolValue(share.EnableShareCompress)
	plan.EnableShareCow = types.BoolValue(share.EnableShareCow)
	plan.ShareQuota = types.Int64Value(share.ShareQuota)
}

func (r *sharedFolderResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state sharedFolderResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	name := state.ID.ValueString()
	if name == "" {
		name = state.Name.ValueString()
	}

	tflog.Debug(ctx, "Reading DSM shared folder", map[string]interface{}{
		"name": name,
	})

	share, err := r.client.GetShare(ctx, name)
	if err != nil {
		resp.Diagnostics.AddError(
			"Failed to read shared folder",
			err.Error(),
		)
		return
	}

	state.Name = types.StringValue(share.Name)
	state.Description = nullableString(share.Description)
	state.VolPath = types.StringValue(share.VolPath)
	applyShareToPlan(&state, share)

	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *sharedFolderResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan sharedFolderResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var state sharedFolderResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Updating DSM shared folder", map[string]interface{}{
		"name": state.Name.ValueString(),
	})

	share, err := r.client.UpdateShare(
		ctx,
		state.Name.ValueString(),
		shareRequestFromPlan(plan, state.Name.ValueString(), state.VolPath.ValueString()),
	)
	if err != nil {
		resp.Diagnostics.AddError(
			"Failed to update shared folder",
			err.Error(),
		)
		return
	}

	applyShareToPlan(&plan, share)

	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *sharedFolderResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state sharedFolderResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Deleting DSM shared folder", map[string]interface{}{
		"name": state.Name.ValueString(),
	})

	if err := r.client.DeleteShare(ctx, state.Name.ValueString()); err != nil {
		resp.Diagnostics.AddError(
			"Failed to delete shared folder",
			err.Error(),
		)
		return
	}
}

func (r *sharedFolderResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
