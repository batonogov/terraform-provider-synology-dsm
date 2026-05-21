package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

func NewSharePermissionResource() resource.Resource {
	return &sharePermissionResource{}
}

type sharePermissionResource struct {
	client *client.Client
}

type sharePermissionResourceModel struct {
	ID            types.String `tfsdk:"id"`
	ShareName     types.String `tfsdk:"share_name"`
	UserGroupType types.String `tfsdk:"user_group_type"`
	PrincipalName types.String `tfsdk:"principal_name"`
	Permission    types.String `tfsdk:"permission"`
}

func (r *sharePermissionResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_share_permission"
}

func (r *sharePermissionResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a share permission on Synology DSM.",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Unique identifier: share_name:user_group_type:principal_name.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"share_name": schema.StringAttribute{
				Required:    true,
				Description: "Name of the shared folder.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"user_group_type": schema.StringAttribute{
				Required:    true,
				Description: "Type of principal: local_user or local_group.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
				Validators: []validator.String{
					newStringOneOfValidator("local_user", "local_group"),
				},
			},
			"principal_name": schema.StringAttribute{
				Required:    true,
				Description: "User or group name.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"permission": schema.StringAttribute{
				Required:    true,
				Description: "Permission level: read_only, read_write, or no_access.",
				Validators: []validator.String{
					newStringOneOfValidator("read_only", "read_write", "no_access"),
				},
			},
		},
	}
}

func (r *sharePermissionResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *sharePermissionResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan sharePermissionResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Creating DSM share permission", map[string]interface{}{
		"share_name":      plan.ShareName.ValueString(),
		"user_group_type": plan.UserGroupType.ValueString(),
		"principal_name":  plan.PrincipalName.ValueString(),
	})

	perm, err := r.client.SetSharePermission(ctx, client.SetSharePermissionRequest{
		ShareName:     plan.ShareName.ValueString(),
		UserGroupType: plan.UserGroupType.ValueString(),
		PrincipalName: plan.PrincipalName.ValueString(),
		Permission:    plan.Permission.ValueString(),
	})
	if err != nil {
		resp.Diagnostics.AddError("Failed to create share permission", err.Error())
		return
	}

	plan.ID = types.StringValue(client.BuildSharePermissionID(
		plan.ShareName.ValueString(),
		plan.UserGroupType.ValueString(),
		plan.PrincipalName.ValueString(),
	))
	plan.Permission = types.StringValue(client.PermissionFromFlags(*perm))

	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *sharePermissionResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state sharePermissionResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	shareName := state.ShareName.ValueString()
	ugType := state.UserGroupType.ValueString()
	principal := state.PrincipalName.ValueString()

	if state.ID.ValueString() != "" {
		sn, ugt, pn, err := client.ParseSharePermissionID(state.ID.ValueString())
		if err == nil {
			shareName = sn
			ugType = ugt
			principal = pn
		}
	}

	tflog.Debug(ctx, "Reading DSM share permission", map[string]interface{}{
		"share_name": shareName,
		"principal":  principal,
	})

	perm, err := r.client.GetSharePermission(ctx, shareName, ugType, principal)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read share permission", err.Error())
		return
	}

	state.ID = types.StringValue(client.BuildSharePermissionID(shareName, ugType, principal))
	state.ShareName = types.StringValue(shareName)
	state.UserGroupType = types.StringValue(ugType)
	state.PrincipalName = types.StringValue(principal)
	state.Permission = types.StringValue(client.PermissionFromFlags(*perm))

	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *sharePermissionResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan sharePermissionResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Updating DSM share permission", map[string]interface{}{
		"share_name":     plan.ShareName.ValueString(),
		"principal_name": plan.PrincipalName.ValueString(),
	})

	perm, err := r.client.SetSharePermission(ctx, client.SetSharePermissionRequest{
		ShareName:     plan.ShareName.ValueString(),
		UserGroupType: plan.UserGroupType.ValueString(),
		PrincipalName: plan.PrincipalName.ValueString(),
		Permission:    plan.Permission.ValueString(),
	})
	if err != nil {
		resp.Diagnostics.AddError("Failed to update share permission", err.Error())
		return
	}

	plan.ID = types.StringValue(client.BuildSharePermissionID(
		plan.ShareName.ValueString(),
		plan.UserGroupType.ValueString(),
		plan.PrincipalName.ValueString(),
	))
	plan.Permission = types.StringValue(client.PermissionFromFlags(*perm))

	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *sharePermissionResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state sharePermissionResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Deleting DSM share permission", map[string]interface{}{
		"share_name":     state.ShareName.ValueString(),
		"principal_name": state.PrincipalName.ValueString(),
	})

	if err := r.client.DeleteSharePermission(ctx,
		state.ShareName.ValueString(),
		state.UserGroupType.ValueString(),
		state.PrincipalName.ValueString(),
	); err != nil {
		resp.Diagnostics.AddError("Failed to delete share permission", err.Error())
		return
	}
}

func (r *sharePermissionResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, ":", 3)
	if len(parts) != 3 {
		resp.Diagnostics.AddError(
			"Invalid import ID",
			"Expected format: share_name:user_group_type:principal_name",
		)
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &sharePermissionResourceModel{
		ID:            types.StringValue(req.ID),
		ShareName:     types.StringValue(parts[0]),
		UserGroupType: types.StringValue(parts[1]),
		PrincipalName: types.StringValue(parts[2]),
	})...)
}

// stringOneOfValidator validates that a string attribute is one of the allowed values.

type stringOneOfValidator struct {
	allowed []string
}

func newStringOneOfValidator(allowed ...string) stringOneOfValidator {
	return stringOneOfValidator{allowed: allowed}
}

func (v stringOneOfValidator) Description(_ context.Context) string {
	return fmt.Sprintf("must be one of: %s", strings.Join(v.allowed, ", "))
}

func (v stringOneOfValidator) MarkdownDescription(_ context.Context) string {
	return v.Description(nil)
}

func (v stringOneOfValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}

	val := req.ConfigValue.ValueString()
	for _, a := range v.allowed {
		if val == a {
			return
		}
	}
	resp.Diagnostics.AddError(
		"Invalid value",
		fmt.Sprintf("value must be one of [%s], got: %q", strings.Join(v.allowed, ", "), val),
	)
}
