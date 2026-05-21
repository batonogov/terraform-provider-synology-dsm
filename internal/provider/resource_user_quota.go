package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

func NewUserQuotaResource() resource.Resource {
	return &userQuotaResource{}
}

type userQuotaResource struct {
	client *client.Client
}

type userQuotaResourceModel struct {
	ID         types.String `tfsdk:"id"`
	ShareName  types.String `tfsdk:"share_name"`
	Username   types.String `tfsdk:"username"`
	QuotaSize  types.Int64  `tfsdk:"quota_size"`
	QuotaUsed  types.Int64  `tfsdk:"quota_used"`
}

func (r *userQuotaResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_user_quota"
}

func (r *userQuotaResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a user quota on a shared folder in Synology DSM.",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Unique identifier: share_name:username.",
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
			"username": schema.StringAttribute{
				Required:    true,
				Description: "Username to set quota for.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"quota_size": schema.Int64Attribute{
				Required:    true,
				Description: "Quota size in bytes. 0 means unlimited.",
				Validators: []validator.Int64{
					newInt64AtLeastValidator(0),
				},
			},
			"quota_used": schema.Int64Attribute{
				Computed:    true,
				Description: "Current space usage in bytes.",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

func (r *userQuotaResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *userQuotaResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan userQuotaResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Creating DSM user quota", map[string]interface{}{
		"share_name": plan.ShareName.ValueString(),
		"username":   plan.Username.ValueString(),
		"quota_size": plan.QuotaSize.ValueInt64(),
	})

	q, err := r.client.SetUserQuota(ctx, client.SetUserQuotaRequest{
		ShareName: plan.ShareName.ValueString(),
		Username:  plan.Username.ValueString(),
		QuotaSize: plan.QuotaSize.ValueInt64(),
	})
	if err != nil {
		resp.Diagnostics.AddError("Failed to create user quota", err.Error())
		return
	}

	plan.ID = types.StringValue(client.BuildUserQuotaID(
		plan.ShareName.ValueString(),
		plan.Username.ValueString(),
	))
	plan.QuotaSize = types.Int64Value(q.QuotaSize)
	plan.QuotaUsed = types.Int64Value(q.QuotaUsed)

	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *userQuotaResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state userQuotaResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	shareName := state.ShareName.ValueString()
	username := state.Username.ValueString()

	if state.ID.ValueString() != "" {
		sn, un, err := client.ParseUserQuotaID(state.ID.ValueString())
		if err == nil {
			shareName = sn
			username = un
		}
	}

	tflog.Debug(ctx, "Reading DSM user quota", map[string]interface{}{
		"share_name": shareName,
		"username":   username,
	})

	q, err := r.client.GetUserQuota(ctx, shareName, username)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read user quota", err.Error())
		return
	}

	state.ID = types.StringValue(client.BuildUserQuotaID(shareName, username))
	state.ShareName = types.StringValue(shareName)
	state.Username = types.StringValue(username)
	state.QuotaSize = types.Int64Value(q.QuotaSize)
	state.QuotaUsed = types.Int64Value(q.QuotaUsed)

	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *userQuotaResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan userQuotaResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Updating DSM user quota", map[string]interface{}{
		"share_name": plan.ShareName.ValueString(),
		"username":   plan.Username.ValueString(),
		"quota_size": plan.QuotaSize.ValueInt64(),
	})

	q, err := r.client.SetUserQuota(ctx, client.SetUserQuotaRequest{
		ShareName: plan.ShareName.ValueString(),
		Username:  plan.Username.ValueString(),
		QuotaSize: plan.QuotaSize.ValueInt64(),
	})
	if err != nil {
		resp.Diagnostics.AddError("Failed to update user quota", err.Error())
		return
	}

	plan.ID = types.StringValue(client.BuildUserQuotaID(
		plan.ShareName.ValueString(),
		plan.Username.ValueString(),
	))
	plan.QuotaSize = types.Int64Value(q.QuotaSize)
	plan.QuotaUsed = types.Int64Value(q.QuotaUsed)

	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *userQuotaResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state userQuotaResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Deleting DSM user quota", map[string]interface{}{
		"share_name": state.ShareName.ValueString(),
		"username":   state.Username.ValueString(),
	})

	if err := r.client.DeleteUserQuota(ctx,
		state.ShareName.ValueString(),
		state.Username.ValueString(),
	); err != nil {
		resp.Diagnostics.AddError("Failed to delete user quota", err.Error())
		return
	}
}

func (r *userQuotaResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, ":", 2)
	if len(parts) != 2 {
		resp.Diagnostics.AddError(
			"Invalid import ID",
			"Expected format: share_name:username",
		)
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &userQuotaResourceModel{
		ID:        types.StringValue(req.ID),
		ShareName: types.StringValue(parts[0]),
		Username:  types.StringValue(parts[1]),
	})...)
}

type int64AtLeastValidator struct {
	min int64
}

func newInt64AtLeastValidator(min int64) int64AtLeastValidator {
	return int64AtLeastValidator{min: min}
}

func (v int64AtLeastValidator) Description(_ context.Context) string {
	return fmt.Sprintf("must be at least %d", v.min)
}

func (v int64AtLeastValidator) MarkdownDescription(_ context.Context) string {
	return v.Description(nil)
}

func (v int64AtLeastValidator) ValidateInt64(_ context.Context, req validator.Int64Request, resp *validator.Int64Response) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}

	if req.ConfigValue.ValueInt64() < v.min {
		resp.Diagnostics.AddError(
			"Invalid value",
			fmt.Sprintf("value must be at least %d, got: %d", v.min, req.ConfigValue.ValueInt64()),
		)
	}
}
