package provider

import (
	"context"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func NewContainerProjectDataSource() datasource.DataSource {
	return &containerProjectDataSource{}
}

type containerProjectDataSource struct {
	client *client.Client
}

type containerProjectDataSourceModel struct {
	ID              types.String `tfsdk:"id"`
	Name            types.String `tfsdk:"name"`
	SharePath       types.String `tfsdk:"share_path"`
	ComposeYAML     types.String `tfsdk:"compose_yaml"`
	ComposeChecksum types.String `tfsdk:"compose_yaml_checksum"`
	Running         types.Bool   `tfsdk:"running"`
	Path            types.String `tfsdk:"path"`
	Status          types.String `tfsdk:"status"`
	ContainerIDs    types.List   `tfsdk:"container_ids"`
}

func (d *containerProjectDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_container_project"
}

func (d *containerProjectDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Looks up an existing Synology Container Manager project by name.",
		Attributes: map[string]schema.Attribute{
			"id":         schema.StringAttribute{Computed: true, Description: "Project UUID assigned by Container Manager."},
			"name":       schema.StringAttribute{Required: true, Description: "Container Manager project name to look up."},
			"share_path": schema.StringAttribute{Computed: true, Description: "File Station path of the project directory."},
			"compose_yaml": schema.StringAttribute{
				Computed:  true,
				Sensitive: true,
				Description: "Docker Compose YAML stored by Container Manager. Reading it puts the document — credentials and all — " +
					"into the state of the configuration that declares this data source, which defeats `compose_yaml_wo` on the " +
					"resource side. Use `compose_yaml_checksum` when all that is needed is to notice the document changing.",
			},
			"compose_yaml_checksum": schema.StringAttribute{
				Computed:    true,
				Description: "SHA-256 checksum (hex) of the compose document, for observing changes without reading the document itself.",
			},
			"running":       schema.BoolAttribute{Computed: true, Description: "Whether the project is running."},
			"path":          schema.StringAttribute{Computed: true, Description: "Absolute volume path of the project directory."},
			"status":        schema.StringAttribute{Computed: true, Description: "Raw project lifecycle status."},
			"container_ids": schema.ListAttribute{Computed: true, ElementType: types.StringType, Description: "Container identifiers associated with the project."},
		},
	}
}

func (d *containerProjectDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	dsmClient := clientFromProviderData(req.ProviderData, &resp.Diagnostics)
	if dsmClient == nil {
		return
	}
	d.client = dsmClient
}

func (d *containerProjectDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config containerProjectDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	project, err := d.client.GetContainerProjectByName(ctx, config.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Failed to read Container Manager project", containerProjectErrorDetail(err))
		return
	}
	config.ID = types.StringValue(project.ID)
	config.Name = types.StringValue(project.Name)
	config.SharePath = types.StringValue(project.SharePath)
	config.ComposeYAML = types.StringValue(project.ComposeYAML)
	config.ComposeChecksum = types.StringValue(fileChecksum([]byte(project.ComposeYAML)))
	config.Running = types.BoolValue(project.Running())
	config.Path = types.StringValue(project.Path)
	config.Status = types.StringValue(project.Status)
	containerIDs, diags := types.ListValueFrom(ctx, types.StringType, project.ContainerIDs)
	resp.Diagnostics.Append(diags...)
	config.ContainerIDs = containerIDs
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}
