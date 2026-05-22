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

func NewUserQuotaDataSource() datasource.DataSource {
	return &userQuotaDataSource{}
}

type userQuotaDataSource struct {
	client *client.Client
}

type userQuotaDataSourceModel struct {
	ID        types.String `tfsdk:"id"`
	ShareName types.String `tfsdk:"share_name"`
	Username  types.String `tfsdk:"username"`
	QuotaSize types.Int64  `tfsdk:"quota_size"`
	QuotaUsed types.Int64  `tfsdk:"quota_used"`
}

func (d *userQuotaDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_user_quota"
}

func (d *userQuotaDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Read-only data source for looking up an existing DSM user quota.",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Unique identifier: share_name:username.",
			},
			"share_name": schema.StringAttribute{
				Required:    true,
				Description: "Name of the shared folder.",
			},
			"username": schema.StringAttribute{
				Required:    true,
				Description: "Username to look up.",
			},
			"quota_size": schema.Int64Attribute{
				Computed:    true,
				Description: "Quota size in bytes. 0 means unlimited.",
			},
			"quota_used": schema.Int64Attribute{
				Computed:    true,
				Description: "Current space usage in bytes.",
			},
		},
	}
}

func (d *userQuotaDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *userQuotaDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config userQuotaDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Debug(ctx, "Reading DSM user quota data source", map[string]interface{}{
		"share_name": config.ShareName.ValueString(),
		"username":   config.Username.ValueString(),
	})

	q, err := d.client.GetUserQuota(ctx,
		config.ShareName.ValueString(),
		config.Username.ValueString(),
	)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read user quota", err.Error())
		return
	}

	config.ID = types.StringValue(client.BuildUserQuotaID(
		config.ShareName.ValueString(),
		config.Username.ValueString(),
	))
	config.QuotaSize = types.Int64Value(q.QuotaSize)
	config.QuotaUsed = types.Int64Value(q.QuotaUsed)

	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}
