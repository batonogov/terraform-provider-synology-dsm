package provider

import (
	"context"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

func NewUserHomeServiceDataSource() datasource.DataSource {
	return &userHomeServiceDataSource{}
}

type userHomeServiceDataSource struct {
	client *client.Client
}

type userHomeServiceDataSourceModel struct {
	ID                  types.String `tfsdk:"id"`
	Enable              types.Bool   `tfsdk:"enable"`
	Location            types.String `tfsdk:"location"`
	EnableRecycleBin    types.Bool   `tfsdk:"enable_recycle_bin"`
	EnableDomain        types.Bool   `tfsdk:"enable_domain"`
	EnableLDAP          types.Bool   `tfsdk:"enable_ldap"`
	Encryption          types.Int64  `tfsdk:"encryption"`
	PersonalPhotoEnable types.Bool   `tfsdk:"personal_photo_enable"`
}

func (d *userHomeServiceDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_user_home_service"
}

func (d *userHomeServiceDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Read-only data source exposing the state of the DSM user home service.",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Fixed identifier of this singleton data source (`user_home_service`).",
			},
			"enable": schema.BoolAttribute{
				Computed:    true,
				Description: "Whether the user home service is enabled.",
			},
			"location": schema.StringAttribute{
				Computed:    true,
				Description: "Volume path hosting the `homes` shared folder, e.g. `/volume1`. Empty if the service was never enabled.",
			},
			"enable_recycle_bin": schema.BoolAttribute{
				Computed:    true,
				Description: "Whether the recycle bin is enabled on the `homes` shared folder.",
			},
			"enable_domain": schema.BoolAttribute{
				Computed:    true,
				Description: "Whether home folders are enabled for domain users.",
			},
			"enable_ldap": schema.BoolAttribute{
				Computed:    true,
				Description: "Whether home folders are enabled for LDAP users.",
			},
			"encryption": schema.Int64Attribute{
				Computed:    true,
				Description: "Encryption mode reported by DSM for the `homes` shared folder (`0` means unencrypted).",
			},
			"personal_photo_enable": schema.BoolAttribute{
				Computed:    true,
				Description: "Whether personal Synology Photos space is enabled. Requires the Synology Photos package.",
			},
		},
	}
}

func (d *userHomeServiceDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	dsmClient := clientFromProviderData(req.ProviderData, &resp.Diagnostics)
	if dsmClient == nil {
		return
	}

	d.client = dsmClient
}

func (d *userHomeServiceDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config userHomeServiceDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Debug(ctx, "Reading DSM user home service data source")

	svc, err := d.client.GetUserHomeService(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read user home service", userHomeErrorDetail(err))
		return
	}

	config.ID = types.StringValue(userHomeServiceID)
	config.Enable = types.BoolValue(svc.Enable)
	config.Location = types.StringValue(svc.Location)
	config.EnableRecycleBin = types.BoolValue(svc.EnableRecycleBin)
	config.EnableDomain = types.BoolValue(svc.EnableDomain)
	config.EnableLDAP = types.BoolValue(svc.EnableLDAP)
	config.Encryption = types.Int64Value(svc.Encryption)
	config.PersonalPhotoEnable = types.BoolValue(svc.PersonalPhotoEnable)

	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}
