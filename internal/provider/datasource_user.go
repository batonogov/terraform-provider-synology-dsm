package provider

import (
	"context"
	"fmt"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

func NewUserDataSource() datasource.DataSource {
	return &userDataSource{}
}

type userDataSource struct {
	client *client.Client
}

type userDataSourceModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	Email       types.String `tfsdk:"email"`
	Disabled    types.Bool   `tfsdk:"disabled"`
	Groups      types.List   `tfsdk:"groups"`
	UID         types.Int64  `tfsdk:"uid"`
}

func (d *userDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_user"
}

func (d *userDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Read-only data source for looking up an existing DSM user.",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Unique identifier for the user (username).",
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Username to look up.",
			},
			"description": schema.StringAttribute{
				Computed:    true,
				Description: "Description of the user account.",
			},
			"email": schema.StringAttribute{
				Computed:    true,
				Description: "Email address for the user.",
			},
			"disabled": schema.BoolAttribute{
				Computed:    true,
				Description: "Whether the account is disabled.",
			},
			"groups": schema.ListAttribute{
				Computed:    true,
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

func (d *userDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

	d.client = dsmClient
}

func (d *userDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config userDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Debug(ctx, "Reading DSM user data source", map[string]interface{}{
		"name": config.Name.ValueString(),
	})

	user, err := d.client.GetUser(ctx, config.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError(
			"Failed to read user",
			err.Error(),
		)
		return
	}

	config.ID = types.StringValue(user.Name)
	config.Description = types.StringValue(user.Description)
	config.Email = types.StringValue(user.Email)
	config.Disabled = types.BoolValue(user.Disabled)
	config.UID = types.Int64Value(int64(user.UID))

	if len(user.Groups) > 0 {
		groups, diags := types.ListValueFrom(ctx, types.StringType, user.Groups)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
		config.Groups = groups
	} else {
		config.Groups = types.ListNull(types.StringType)
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}
