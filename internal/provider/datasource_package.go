package provider

import (
	"context"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func NewPackageDataSource() datasource.DataSource {
	return &packageDataSource{}
}

type packageDataSource struct {
	client *client.Client
}

type packageDataSourceModel struct {
	ID           types.String `tfsdk:"id"`
	Name         types.String `tfsdk:"name"`
	DisplayName  types.String `tfsdk:"display_name"`
	Version      types.String `tfsdk:"version"`
	Status       types.String `tfsdk:"status"`
	Running      types.Bool   `tfsdk:"running"`
	Description  types.String `tfsdk:"description"`
	Maintainer   types.String `tfsdk:"maintainer"`
	CanUninstall types.Bool   `tfsdk:"can_uninstall"`
}

func (d *packageDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_package"
}

func (d *packageDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Looks up an installed Synology DSM package by its Package Center identifier.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Package identifier used by DSM Package Center.",
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Package Center identifier to look up, for example `ContainerManager` or `FileStation`.",
			},
			"display_name": schema.StringAttribute{Computed: true, Description: "Human-readable package name."},
			"version":      schema.StringAttribute{Computed: true, Description: "Installed package version."},
			"status":       schema.StringAttribute{Computed: true, Description: "Raw lifecycle status reported by DSM."},
			"running":      schema.BoolAttribute{Computed: true, Description: "Whether the package is running."},
			"description":  schema.StringAttribute{Computed: true, Description: "Package description."},
			"maintainer":   schema.StringAttribute{Computed: true, Description: "Package maintainer."},
			"can_uninstall": schema.BoolAttribute{
				Computed:    true,
				Description: "Whether DSM allows this package to be uninstalled.",
			},
		},
	}
}

func (d *packageDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	dsmClient := clientFromProviderData(req.ProviderData, &resp.Diagnostics)
	if dsmClient == nil {
		return
	}
	d.client = dsmClient
}

func (d *packageDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config packageDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	pkg, err := d.client.GetPackage(ctx, config.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Failed to read DSM package", packageErrorDetail(err))
		return
	}

	config.ID = types.StringValue(pkg.ID)
	config.Name = types.StringValue(pkg.ID)
	config.DisplayName = types.StringValue(pkg.Name)
	config.Version = types.StringValue(pkg.Version)
	config.Status = types.StringValue(pkg.Status)
	config.Running = types.BoolValue(pkg.Running())
	config.Description = types.StringValue(pkg.Description)
	config.Maintainer = types.StringValue(pkg.Maintainer)
	config.CanUninstall = types.BoolValue(pkg.CanUninstall)
	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}
