package provider

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &synologyProvider{version: version}
	}
}

type synologyProvider struct {
	version string
}

type synologyProviderModel struct {
	Host     types.String `tfsdk:"host"`
	Username types.String `tfsdk:"username"`
	Password types.String `tfsdk:"password"`
	Insecure types.Bool   `tfsdk:"insecure"`
	Timeout  types.String `tfsdk:"timeout"`
}

func (p *synologyProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "dsm"
	resp.Version = p.version
}

func (p *synologyProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Provider for managing Synology DSM packages, Container Manager projects, files in shared folders, reverse proxy entries, system settings, users, groups, shared folders, permissions, quotas, and user home service.",
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
			"timeout": schema.StringAttribute{
				Optional: true,
				Description: "Timeout for ordinary DSM requests, as a Go duration such as `60s` or `2m`. Defaults to `30s`. " +
					"Raise it for slower hardware or a NAS under load. Lifecycle operations that block while DSM works — " +
					"creating a Container Manager project, writing a compose file — always get at least five minutes " +
					"regardless of this value, because their duration is bounded by the NAS rather than by the network.",
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
	if config.Password.IsNull() {
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
	if config.Password.IsNull() {
		if _, ok := os.LookupEnv("SYNOLOGY_DSM_PASSWORD"); !ok {
			resp.Diagnostics.AddError("Missing password", "Set password in provider config or SYNOLOGY_DSM_PASSWORD env var")
			return
		}
	}

	insecure := config.Insecure.ValueBool()

	tflog.Info(ctx, "Connecting to Synology DSM", map[string]interface{}{
		"host": host,
	})

	var timeout time.Duration
	if raw := config.Timeout.ValueString(); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			resp.Diagnostics.AddAttributeError(
				path.Root("timeout"),
				"Invalid timeout",
				fmt.Sprintf("`timeout` must be a positive Go duration such as `60s` or `2m`, got %q.", raw),
			)
			return
		}
		timeout = parsed
	}

	dsmClient := client.NewClientWithTimeout(host, username, password, insecure, timeout)

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
		NewSharePermissionResource,
		NewUserQuotaResource,
		NewUserHomeServiceResource,
		NewPackageResource,
		NewContainerProjectResource,
		NewFileResource,
		NewSystemSettingsResource,
		NewReverseProxyResource,
	}
}

func (p *synologyProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewUserDataSource,
		NewGroupDataSource,
		NewSharedFolderDataSource,
		NewSharePermissionDataSource,
		NewUserQuotaDataSource,
		NewUserHomeServiceDataSource,
		NewPackageDataSource,
		NewContainerProjectDataSource,
		NewSystemSettingsDataSource,
		NewReverseProxyDataSource,
	}
}
