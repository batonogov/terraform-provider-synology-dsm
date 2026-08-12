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

func NewSystemSettingsDataSource() datasource.DataSource {
	return &systemSettingsDataSource{}
}

type systemSettingsDataSource struct {
	client *client.Client
}

type systemSettingsDataSourceModel struct {
	ID          types.String `tfsdk:"id"`
	Timezone    types.String `tfsdk:"timezone"`
	NTPEnabled  types.Bool   `tfsdk:"ntp_enabled"`
	NTPServer   types.String `tfsdk:"ntp_server"`
	CurrentDate types.String `tfsdk:"current_date"`
	CurrentTime types.String `tfsdk:"current_time"`
	Timestamp   types.Int64  `tfsdk:"timestamp"`
}

func (d *systemSettingsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_system_settings"
}

func (d *systemSettingsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Read-only data source exposing the DSM system date and time settings, including the NAS clock " +
			"as DSM currently reports it.",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Fixed identifier of this singleton data source (`system_settings`).",
			},
			"timezone": schema.StringAttribute{
				Computed:    true,
				Description: "DSM time zone name, for example `Moscow`. Synology's own naming, not an IANA identifier.",
			},
			"ntp_enabled": schema.BoolAttribute{
				Computed:    true,
				Description: "Whether the NAS clock is synchronised with an NTP server.",
			},
			"ntp_server": schema.StringAttribute{
				Computed:    true,
				Description: "Configured NTP server. DSM keeps the last value even while synchronisation is off.",
			},
			"current_date": schema.StringAttribute{
				Computed: true,
				Description: "Date on the NAS as DSM reports it, in DSM's own `YYYY/M/D` form without leading " +
					"zeros (for example `2026/8/12`).",
			},
			"current_time": schema.StringAttribute{
				Computed: true,
				Description: "Time of day on the NAS as `HH:MM:SS`. This is a reading taken when the data source " +
					"was refreshed, not a setting.",
			},
			"timestamp": schema.Int64Attribute{
				Computed:    true,
				Description: "NAS clock as a Unix timestamp at the moment the data source was refreshed.",
			},
		},
	}
}

func (d *systemSettingsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *systemSettingsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config systemSettingsDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Debug(ctx, "Reading DSM system settings data source")

	settings, err := d.client.GetSystemSettings(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read system settings", systemSettingsErrorDetail(ctx, d.client, err, ""))
		return
	}

	config.ID = types.StringValue(systemSettingsID)
	config.Timezone = types.StringValue(settings.Timezone)
	config.NTPEnabled = types.BoolValue(settings.NTPEnabled)
	config.NTPServer = types.StringValue(settings.NTPServer)
	config.CurrentDate = types.StringValue(settings.Date)
	config.CurrentTime = types.StringValue(fmt.Sprintf("%02d:%02d:%02d", settings.Hour, settings.Minute, settings.Second))
	config.Timestamp = types.Int64Value(settings.Timestamp)

	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}
