package provider

import (
	"context"
	"fmt"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
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
	ID               types.String `tfsdk:"id"`
	Name             types.String `tfsdk:"name"`
	VolPath          types.String `tfsdk:"vol_path"`
	Description      types.String `tfsdk:"description"`
	Hidden           types.Bool   `tfsdk:"hidden"`
	EnableRecycleBin types.Bool   `tfsdk:"enable_recycle_bin"`
	UUID             types.String `tfsdk:"uuid"`
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

	share, err := r.client.CreateShare(ctx, client.CreateShareRequest{
		Name:             plan.Name.ValueString(),
		VolPath:          plan.VolPath.ValueString(),
		Description:      plan.Description.ValueString(),
		Hidden:           plan.Hidden.ValueBool(),
		EnableRecycleBin: plan.EnableRecycleBin.ValueBool(),
	})
	if err != nil {
		resp.Diagnostics.AddError(
			"Failed to create shared folder",
			err.Error(),
		)
		return
	}

	plan.ID = types.StringValue(share.Name)
	plan.UUID = types.StringValue(share.UUID)

	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
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

	state.ID = types.StringValue(share.Name)
	state.Name = types.StringValue(share.Name)
	state.Description = nullableString(share.Description)
	state.VolPath = types.StringValue(share.VolPath)
	state.Hidden = types.BoolValue(share.Hidden)
	state.EnableRecycleBin = types.BoolValue(share.EnableRecycleBin)
	state.UUID = types.StringValue(share.UUID)

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

	share, err := r.client.UpdateShare(ctx, state.Name.ValueString(), client.CreateShareRequest{
		Name:             state.Name.ValueString(),
		VolPath:          state.VolPath.ValueString(),
		Description:      plan.Description.ValueString(),
		Hidden:           plan.Hidden.ValueBool(),
		EnableRecycleBin: plan.EnableRecycleBin.ValueBool(),
	})
	if err != nil {
		resp.Diagnostics.AddError(
			"Failed to update shared folder",
			err.Error(),
		)
		return
	}

	plan.ID = types.StringValue(share.Name)
	plan.UUID = types.StringValue(share.UUID)

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
