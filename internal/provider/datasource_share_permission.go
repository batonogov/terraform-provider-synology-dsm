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

func NewSharePermissionDataSource() datasource.DataSource {
	return &sharePermissionDataSource{}
}

type sharePermissionDataSource struct {
	client *client.Client
}

type sharePermissionDataSourceModel struct {
	ID            types.String `tfsdk:"id"`
	ShareName     types.String `tfsdk:"share_name"`
	UserGroupType types.String `tfsdk:"user_group_type"`
	PrincipalName types.String `tfsdk:"principal_name"`
	Permission    types.String `tfsdk:"permission"`
}

func (d *sharePermissionDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_share_permission"
}

func (d *sharePermissionDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Read-only data source for looking up an existing DSM share permission.",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Unique identifier: share_name:user_group_type:principal_name.",
			},
			"share_name": schema.StringAttribute{
				Required:    true,
				Description: "Name of the shared folder.",
			},
			"user_group_type": schema.StringAttribute{
				Required:    true,
				Description: "Type of principal: local_user or local_group.",
			},
			"principal_name": schema.StringAttribute{
				Required:    true,
				Description: "User or group name.",
			},
			"permission": schema.StringAttribute{
				Computed:    true,
				Description: "Permission level: read_only, read_write, or no_access.",
			},
		},
	}
}

func (d *sharePermissionDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *sharePermissionDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config sharePermissionDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Debug(ctx, "Reading DSM share permission data source", map[string]interface{}{
		"share_name":      config.ShareName.ValueString(),
		"user_group_type": config.UserGroupType.ValueString(),
		"principal_name":  config.PrincipalName.ValueString(),
	})

	perm, err := d.client.GetSharePermission(ctx,
		config.ShareName.ValueString(),
		config.UserGroupType.ValueString(),
		config.PrincipalName.ValueString(),
	)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read share permission", err.Error())
		return
	}

	config.ID = types.StringValue(client.BuildSharePermissionID(
		config.ShareName.ValueString(),
		config.UserGroupType.ValueString(),
		config.PrincipalName.ValueString(),
	))
	config.Permission = types.StringValue(client.PermissionFromFlags(*perm))

	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}
