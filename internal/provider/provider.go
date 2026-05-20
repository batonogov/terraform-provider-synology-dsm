package provider

import (
	"context"
	"fmt"
	"os"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

const (
	providerVersion = "0.1.0"
)

func New() provider.Provider {
	return &synologyProvider{}
}

type synologyProvider struct{}

type synologyProviderModel struct {
	Host     types.String `tfsdk:"host"`
	Username types.String `tfsdk:"username"`
	Password types.String `tfsdk:"password"`
	Insecure types.Bool   `tfsdk:"insecure"`
}

func (p *synologyProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "dsm"
	resp.Version = providerVersion
}

func (p *synologyProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Provider for managing Synology DSM as a corporate file cloud.",
		Attributes: map[string]schema.Attribute{
			"host": schema.StringAttribute{
				Required:    true,
				Description: "Synology DSM URL (e.g. https://diskstation:5001)",
			},
			"username": schema.StringAttribute{
				Required:    true,
				Description: "DSM administrator username",
			},
			"password": schema.StringAttribute{
				Required:    true,
				Sensitive:   true,
				Description: "DSM administrator password",
			},
			"insecure": schema.BoolAttribute{
				Optional:    true,
				Description: "Skip TLS certificate verification (for self-signed certs)",
			},
		},
	}
}

func (p *synologyProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var config synologyProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	host := config.Host.ValueString()
	if host == "" {
		host = os.Getenv("SYNOLOGY_DSM_HOST")
	}

	username := config.Username.ValueString()
	if username == "" {
		username = os.Getenv("SYNOLOGY_DSM_USERNAME")
	}

	password := config.Password.ValueString()
	if password == "" {
		password = os.Getenv("SYNOLOGY_DSM_PASSWORD")
	}

	if host == "" {
		resp.Diagnostics.AddError("Missing host", "Set host in provider config or SYNOLOGY_DSM_HOST env var")
		return
	}
	if username == "" {
		resp.Diagnostics.AddError("Missing username", "Set username in provider config or SYNOLOGY_DSM_USERNAME env var")
		return
	}
	if password == "" {
		resp.Diagnostics.AddError("Missing password", "Set password in provider config or SYNOLOGY_DSM_PASSWORD env var")
		return
	}

	insecure := config.Insecure.ValueBool()

	tflog.Info(ctx, "Connecting to Synology DSM", map[string]interface{}{
		"host": host,
	})

	dsmClient := client.NewClient(host, username, password, insecure)

	if err := dsmClient.Login(ctx); err != nil {
		resp.Diagnostics.AddError(
			"Failed to connect to Synology DSM",
			fmt.Sprintf("Login failed: %s", err),
		)
		return
	}

	tflog.Info(ctx, "Successfully connected to Synology DSM")

	resp.ResourceData = dsmClient
	resp.DataSourceData = dsmClient
}

func (p *synologyProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewUserResource,
		NewGroupResource,
		NewSharedFolderResource,
	}
}

func (p *synologyProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewUserDataSource,
		NewGroupDataSource,
		NewSharedFolderDataSource,
	}
}
