package provider

import (
	"context"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

func NewEventTaskDataSource() datasource.DataSource {
	return &eventTaskDataSource{}
}

type eventTaskDataSource struct {
	client *client.Client
}

type eventTaskDataSourceModel struct {
	ID              types.String `tfsdk:"id"`
	Name            types.String `tfsdk:"name"`
	User            types.String `tfsdk:"user"`
	Event           types.String `tfsdk:"event"`
	Command         types.String `tfsdk:"command"`
	Enabled         types.Bool   `tfsdk:"enabled"`
	NotifyEmail     types.String `tfsdk:"notify_email"`
	NotifyOnFailure types.Bool   `tfsdk:"notify_on_failure"`
	DependsOnTasks  types.List   `tfsdk:"depends_on_tasks"`
	OwnerUID        types.Int64  `tfsdk:"owner_uid"`
}

func (d *eventTaskDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_event_task"
}

func (d *eventTaskDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Reads an existing boot or shutdown task from DSM Task Scheduler.\n\n" +
			"Reading a task executes nothing, so this data source is not gated behind `allow_task_execution`. It is a useful way to audit what a " +
			"NAS already runs unattended on every restart.",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Task name, which is also DSM's identifier for an event task.",
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Task name to look up.",
			},
			"user": schema.StringAttribute{
				Computed:    true,
				Description: "Account DSM runs the command as.",
			},
			"event": schema.StringAttribute{
				Computed:    true,
				Description: "System event that triggers the task: `bootup` or `shutdown`.",
			},
			"command": schema.StringAttribute{
				Computed:    true,
				Description: "Shell command DSM executes.",
			},
			"enabled": schema.BoolAttribute{
				Computed:    true,
				Description: "Whether the task is active.",
			},
			"notify_email": schema.StringAttribute{
				Computed:    true,
				Description: "Address DSM emails when the task runs, or null when notification is disabled.",
			},
			"notify_on_failure": schema.BoolAttribute{
				Computed:    true,
				Description: "Whether DSM notifies only on failure rather than after every run.",
			},
			"depends_on_tasks": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				Description: "Names of tasks that must finish before this one starts.",
			},
			"owner_uid": schema.Int64Attribute{
				Computed:    true,
				Description: "Numeric uid DSM records for the owning account. `0` is root.",
			},
		},
	}
}

func (d *eventTaskDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	dsmClient := clientFromProviderData(req.ProviderData, &resp.Diagnostics)
	if dsmClient == nil {
		return
	}
	d.client = dsmClient
}

func (d *eventTaskDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config eventTaskDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Debug(ctx, "Reading DSM event task data source", map[string]interface{}{"name": config.Name.ValueString()})

	task, err := d.client.GetEventTask(ctx, config.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Failed to read event task", eventTaskErrorDetail(err))
		return
	}

	config.ID = types.StringValue(task.Name)
	config.Name = types.StringValue(task.Name)
	config.User = types.StringValue(task.Owner)
	config.Event = types.StringValue(task.Event)
	config.Command = types.StringValue(task.Command)
	config.Enabled = types.BoolValue(task.Enabled)
	config.NotifyEmail = nullableString(task.NotifyEmail)
	config.NotifyOnFailure = types.BoolValue(task.NotifyOnFailure)
	config.OwnerUID = types.Int64Value(int64(task.OwnerUID))

	dependsOn, listDiags := types.ListValueFrom(ctx, types.StringType, task.DependsOn)
	resp.Diagnostics.Append(listDiags...)
	if resp.Diagnostics.HasError() {
		return
	}
	config.DependsOnTasks = dependsOn

	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}
