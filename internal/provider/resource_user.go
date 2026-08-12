package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
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
	ExpireDate  types.String `tfsdk:"expire_date"`
	TwoFactor   types.Bool   `tfsdk:"two_factor_enabled"`
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
				Optional:  true,
				Sensitive: true,
				Description: "Password for the account. Required when creating a user; DSM never returns it, " +
					"so it may be omitted when adopting an existing account with `terraform import`.",
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
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				Description: "Whether the account is disabled. Mutually exclusive with `expire_date`: DSM stores " +
					"both in a single field, so a disabled account cannot also carry an expiry date.",
			},
			"expire_date": schema.StringAttribute{
				Optional: true,
				Description: "Date the account expires, as `YYYY-MM-DD` (for example `2027-03-05`). " +
					"Omit for an account that never expires. Mutually exclusive with `disabled`.",
			},
			"two_factor_enabled": schema.BoolAttribute{
				Computed: true,
				Description: "Whether two-factor authentication is enabled for this account. Read-only: " +
					"DSM manages 2FA through a separate API (`SYNO.Core.OTP`), not through user attributes.",
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"groups": schema.ListAttribute{
				Optional:    true,
				ElementType: types.StringType,
				Description: "List of group names the user belongs to.",
			},
			"uid": schema.Int64Attribute{
				Computed:    true,
				Description: "User ID assigned by DSM.",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

func (r *userResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	dsmClient := clientFromProviderData(req.ProviderData, &resp.Diagnostics)
	if dsmClient == nil {
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
		ExpireDate:  plan.ExpireDate.ValueString(),
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
	plan.TwoFactor = types.BoolValue(user.TwoFactor)

	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *userResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state userResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	name := state.ID.ValueString()
	if name == "" {
		name = state.Name.ValueString()
	}

	tflog.Debug(ctx, "Reading DSM user", map[string]interface{}{
		"name": name,
	})

	user, err := r.client.GetUser(ctx, name)
	if err != nil {
		resp.Diagnostics.AddError(
			"Failed to read user",
			err.Error(),
		)
		return
	}

	state.ID = types.StringValue(user.Name)
	state.Name = types.StringValue(user.Name)
	state.Description = nullableString(user.Description)
	state.Email = nullableString(user.Email)
	state.Disabled = types.BoolValue(user.Disabled)
	state.ExpireDate = nullableString(user.ExpireDate)
	state.TwoFactor = types.BoolValue(user.TwoFactor)
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
	expireDate := plan.ExpireDate.ValueString()
	user, err := r.client.UpdateUser(ctx, state.Name.ValueString(), client.UpdateUserRequest{
		Password:    plan.Password.ValueString(),
		Description: plan.Description.ValueString(),
		Email:       plan.Email.ValueString(),
		Disabled:    &disabled,
		ExpireDate:  &expireDate,
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
	plan.TwoFactor = types.BoolValue(user.TwoFactor)

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

// ValidateConfig enforces the two rules DSM's own data model implies but its
// API does not report clearly.
func (r *userResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config userResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// DSM keeps the account state in a single "expired" field, so "switched
	// off" and "expires on a date" cannot both hold.
	if config.Disabled.ValueBool() && !config.ExpireDate.IsNull() && !config.ExpireDate.IsUnknown() {
		resp.Diagnostics.AddAttributeError(
			path.Root("expire_date"),
			"disabled and expire_date are mutually exclusive",
			"DSM stores the account state in one field, so an account cannot be both disabled and carry an "+
				"expiry date. Drop expire_date to keep the account disabled, or set disabled = false to use the date.",
		)
	}

	if !config.ExpireDate.IsNull() && !config.ExpireDate.IsUnknown() {
		if _, err := time.Parse("2006-01-02", config.ExpireDate.ValueString()); err != nil {
			resp.Diagnostics.AddAttributeError(
				path.Root("expire_date"),
				"Invalid expire_date format",
				fmt.Sprintf("expire_date must be YYYY-MM-DD (for example 2027-03-05), got %q.",
					config.ExpireDate.ValueString()),
			)
		}
	}
}

// ModifyPlan requires a password when the user is being created. The attribute
// itself is optional so that `terraform import` of an existing account does not
// leave a permanently dirty plan — DSM never returns the password, so there is
// nothing to import it from.
func (r *userResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	// Null state means create; null plan means destroy. Only the former needs a
	// password.
	if !req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}

	var plan userResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if plan.Password.IsNull() {
		resp.Diagnostics.AddAttributeError(
			path.Root("password"),
			"password is required when creating a user",
			"DSM cannot create an account without a password. It may only be omitted for a user adopted "+
				"with terraform import, since DSM never returns an existing password.",
		)
	}
}
