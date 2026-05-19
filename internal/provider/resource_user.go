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

func NewUserResource() resource.Resource {
	return &userResource{}
}

type userResource struct {
	client *client.Client
}

type userResourceModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Password    types.String `tfsdk:"password"`
	Description types.String `tfsdk:"description"`
	Email       types.String `tfsdk:"email"`
	Disabled    types.Bool   `tfsdk:"disabled"`
	Groups      types.List   `tfsdk:"groups"`
	UID         types.Int64  `tfsdk:"uid"`
}

func (r *userResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_user"
}

func (r *userResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a user account on Synology DSM.",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Unique identifier for the user (username).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Username for the account.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"password": schema.StringAttribute{
				Required:    true,
				Sensitive:   true,
				Description: "Password for the account.",
			},
			"description": schema.StringAttribute{
				Optional:    true,
				Description: "Description of the user account.",
			},
			"email": schema.StringAttribute{
				Optional:    true,
				Description: "Email address for the user.",
			},
			"disabled": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "Whether the account is disabled.",
			},
			"groups": schema.ListAttribute{
				Optional:    true,
				ElementType: types.StringType,
				Description: "List of group names the user belongs to.",
			},
			"uid": schema.Int64Attribute{
				Computed:    true,
				Description: "User ID assigned by DSM.",
			},
		},
	}
}

func (r *userResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *userResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan userResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Creating DSM user", map[string]interface{}{
		"name": plan.Name.ValueString(),
	})

	var groups []string
	if !plan.Groups.IsNull() && !plan.Groups.IsUnknown() {
		resp.Diagnostics.Append(plan.Groups.ElementsAs(ctx, &groups, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	user, err := r.client.CreateUser(ctx, client.CreateUserRequest{
		Name:        plan.Name.ValueString(),
		Password:    plan.Password.ValueString(),
		Description: plan.Description.ValueString(),
		Email:       plan.Email.ValueString(),
		Disabled:    plan.Disabled.ValueBool(),
		Groups:      groups,
	})
	if err != nil {
		resp.Diagnostics.AddError(
			"Failed to create user",
			err.Error(),
		)
		return
	}

	plan.ID = types.StringValue(user.Name)
	plan.UID = types.Int64Value(int64(user.UID))

	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *userResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state userResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Debug(ctx, "Reading DSM user", map[string]interface{}{
		"name": state.Name.ValueString(),
	})

	user, err := r.client.GetUser(ctx, state.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError(
			"Failed to read user",
			err.Error(),
		)
		return
	}

	state.Description = types.StringValue(user.Description)
	state.Email = types.StringValue(user.Email)
	state.Disabled = types.BoolValue(user.Disabled)
	state.UID = types.Int64Value(int64(user.UID))

	if len(user.Groups) > 0 {
		groups, diags := types.ListValueFrom(ctx, types.StringType, user.Groups)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
		state.Groups = groups
	} else {
		state.Groups = types.ListNull(types.StringType)
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *userResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan userResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var state userResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Updating DSM user", map[string]interface{}{
		"name": state.Name.ValueString(),
	})

	var groups []string
	if !plan.Groups.IsNull() && !plan.Groups.IsUnknown() {
		resp.Diagnostics.Append(plan.Groups.ElementsAs(ctx, &groups, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	disabled := plan.Disabled.ValueBool()
	user, err := r.client.UpdateUser(ctx, state.Name.ValueString(), client.UpdateUserRequest{
		Password:    plan.Password.ValueString(),
		Description: plan.Description.ValueString(),
		Email:       plan.Email.ValueString(),
		Disabled:    &disabled,
		Groups:      groups,
	})
	if err != nil {
		resp.Diagnostics.AddError(
			"Failed to update user",
			err.Error(),
		)
		return
	}

	plan.ID = types.StringValue(user.Name)
	plan.UID = types.Int64Value(int64(user.UID))

	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *userResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state userResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Deleting DSM user", map[string]interface{}{
		"name": state.Name.ValueString(),
	})

	if err := r.client.DeleteUser(ctx, state.Name.ValueString()); err != nil {
		resp.Diagnostics.AddError(
			"Failed to delete user",
			err.Error(),
		)
		return
	}
}

func (r *userResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
