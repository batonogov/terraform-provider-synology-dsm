package provider

import (
	"context"
	"strconv"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

func NewScheduledTaskDataSource() datasource.DataSource {
	return &scheduledTaskDataSource{}
}

type scheduledTaskDataSource struct {
	client *client.Client
}

type scheduledTaskDataSourceModel struct {
	ID              types.String       `tfsdk:"id"`
	Name            types.String       `tfsdk:"name"`
	User            types.String       `tfsdk:"user"`
	Command         types.String       `tfsdk:"command"`
	Enabled         types.Bool         `tfsdk:"enabled"`
	NotifyEmail     types.String       `tfsdk:"notify_email"`
	NotifyOnFailure types.Bool         `tfsdk:"notify_on_failure"`
	RealOwner       types.String       `tfsdk:"real_owner"`
	Type            types.String       `tfsdk:"type"`
	Schedule        *taskScheduleModel `tfsdk:"schedule"`
}

func (d *scheduledTaskDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_scheduled_task"
}

func (d *scheduledTaskDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Reads an existing task from DSM Task Scheduler.\n\n" +
			"Reading a task executes nothing, so this data source is not gated behind `allow_task_execution`. It is a useful way to see what a " +
			"NAS is already configured to run — including tasks created by hand in DSM — before deciding whether to manage them from Terraform.",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Numeric task identifier assigned by DSM, as a string.",
			},
			"name": schema.StringAttribute{
				Required: true,
				Description: "Task name to look up. DSM does not require task names to be unique; an ambiguous name is an error rather than " +
					"an arbitrary match.",
			},
			"user": schema.StringAttribute{
				Computed:    true,
				Description: "Account DSM runs the command as.",
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
			"real_owner": schema.StringAttribute{
				Computed:    true,
				Description: "Account DSM records as the owner of the task entry.",
			},
			"type": schema.StringAttribute{
				Computed: true,
				Description: "DSM task type, such as `script` or `service`. Only `script` tasks can be managed by the `dsm_scheduled_task` " +
					"resource; the others are DSM's own built-in maintenance tasks.",
			},
			"schedule": schema.SingleNestedAttribute{
				Computed:    true,
				Description: "When the task runs.",
				Attributes: map[string]schema.Attribute{
					"frequency": schema.StringAttribute{
						Computed:    true,
						Description: "`daily`, `weekly`, or `monthly`.",
					},
					"day_of_week": schema.SetAttribute{
						Computed:    true,
						ElementType: types.StringType,
						Description: "Weekday names the task runs on.",
					},
					"week_of_month": schema.SetAttribute{
						Computed:    true,
						ElementType: types.StringType,
						Description: "Weeks of the month a monthly task runs in.",
					},
					"hour": schema.Int64Attribute{
						Computed:    true,
						Description: "Hour the task starts.",
					},
					"minute": schema.Int64Attribute{
						Computed:    true,
						Description: "Minute the task starts.",
					},
					"repeat_interval_hours": schema.Int64Attribute{
						Computed:    true,
						Description: "Same-day repeat interval in hours, or 0 when disabled.",
					},
					"repeat_interval_minutes": schema.Int64Attribute{
						Computed:    true,
						Description: "Same-day repeat interval in minutes, or 0 when disabled.",
					},
					"repeat_until_hour": schema.Int64Attribute{
						Computed:    true,
						Description: "Last hour of the day at which a same-day repeat may still fire.",
					},
				},
			},
		},
	}
}

func (d *scheduledTaskDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	dsmClient := clientFromProviderData(req.ProviderData, &resp.Diagnostics)
	if dsmClient == nil {
		return
	}
	d.client = dsmClient
}

func (d *scheduledTaskDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config scheduledTaskDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Debug(ctx, "Reading DSM scheduled task data source", map[string]interface{}{"name": config.Name.ValueString()})

	task, err := d.client.GetScheduledTaskByName(ctx, config.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Failed to read scheduled task", scheduledTaskErrorDetail(err))
		return
	}

	config.ID = types.StringValue(strconv.Itoa(task.ID))
	config.Name = types.StringValue(task.Name)
	config.User = types.StringValue(task.Owner)
	config.Command = types.StringValue(task.Command)
	config.Enabled = types.BoolValue(task.Enabled)
	config.NotifyEmail = nullableString(task.NotifyEmail)
	config.NotifyOnFailure = types.BoolValue(task.NotifyOnFailure)
	config.RealOwner = types.StringValue(task.RealOwner)
	config.Type = types.StringValue(task.Type)

	dayOfWeek, dayDiags := types.SetValueFrom(ctx, types.StringType, task.Schedule.DaysOfWeek)
	resp.Diagnostics.Append(dayDiags...)
	weekOfMonth, weekDiags := types.SetValueFrom(ctx, types.StringType, task.Schedule.WeeksOfMonth)
	resp.Diagnostics.Append(weekDiags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Unlike the resource, the data source reports an unrecognised schedule as an
	// empty frequency rather than failing: a read-only view of a task DSM
	// supports but this provider cannot create is still useful information.
	config.Schedule = &taskScheduleModel{
		Frequency:             nullableString(task.Schedule.Frequency),
		DayOfWeek:             dayOfWeek,
		WeekOfMonth:           weekOfMonth,
		Hour:                  types.Int64Value(int64(task.Schedule.Hour)),
		Minute:                types.Int64Value(int64(task.Schedule.Minute)),
		RepeatIntervalHours:   types.Int64Value(int64(task.Schedule.RepeatIntervalHours)),
		RepeatIntervalMinutes: types.Int64Value(int64(task.Schedule.RepeatIntervalMinutes)),
		RepeatUntilHour:       types.Int64Null(),
	}
	if task.Schedule.RepeatUntilHour != nil {
		config.Schedule.RepeatUntilHour = types.Int64Value(int64(*task.Schedule.RepeatUntilHour))
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}
