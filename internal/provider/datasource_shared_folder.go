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

func NewSharedFolderDataSource() datasource.DataSource {
	return &sharedFolderDataSource{}
}

type sharedFolderDataSource struct {
	client *client.Client
}

type sharedFolderDataSourceModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	VolPath     types.String `tfsdk:"vol_path"`
	UUID        types.String `tfsdk:"uuid"`
}

func (d *sharedFolderDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_shared_folder"
}

func (d *sharedFolderDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Read-only data source for looking up an existing DSM shared folder.",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Unique identifier for the shared folder (name).",
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Shared folder name to look up.",
			},
			"description": schema.StringAttribute{
				Computed:    true,
				Description: "Description of the shared folder.",
			},
			"vol_path": schema.StringAttribute{
				Computed:    true,
				Description: "Volume path (e.g. /volume1).",
			},
			"uuid": schema.StringAttribute{
				Computed:    true,
				Description: "UUID assigned by DSM.",
			},
		},
	}
}

func (d *sharedFolderDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *sharedFolderDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config sharedFolderDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Debug(ctx, "Reading DSM shared folder data source", map[string]interface{}{
		"name": config.Name.ValueString(),
	})

	share, err := d.client.GetShare(ctx, config.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError(
			"Failed to read shared folder",
			err.Error(),
		)
		return
	}

	config.ID = types.StringValue(share.Name)
	config.Description = types.StringValue(share.Description)
	config.VolPath = types.StringValue(share.VolPath)
	config.UUID = types.StringValue(share.UUID)

	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}
