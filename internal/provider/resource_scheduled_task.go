package provider

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

// taskExecutionWarning is the description prefix both task resources carry. It
// is deliberately the first thing a reader sees in the generated documentation:
// unlike every other resource in this provider, these two turn write access to
// a Terraform configuration into root-level code execution on the NAS.
const taskExecutionWarning = "**This resource runs a shell command on the NAS.** DSM executes it as the account named in `user`, which for most " +
	"automation is `root`. Anyone who can change this configuration, approve a pull request that touches it, or influence a variable that reaches " +
	"`command` can therefore run arbitrary code on the NAS with that account's privileges. Treat the configuration as a privileged artifact: it is " +
	"equivalent to shell access. The resource is refused unless the provider sets `allow_task_execution = true`. " +
	"`command` is intentionally not marked sensitive so that the command stays visible in `terraform plan` output where it can be reviewed."

func NewScheduledTaskResource() resource.Resource {
	return &scheduledTaskResource{}
}

type scheduledTaskResource struct {
	client             *client.Client
	allowTaskExecution bool
}

type scheduledTaskResourceModel struct {
	ID              types.String       `tfsdk:"id"`
	Name            types.String       `tfsdk:"name"`
	User            types.String       `tfsdk:"user"`
	Command         types.String       `tfsdk:"command"`
	Enabled         types.Bool         `tfsdk:"enabled"`
	NotifyEmail     types.String       `tfsdk:"notify_email"`
	NotifyOnFailure types.Bool         `tfsdk:"notify_on_failure"`
	RealOwner       types.String       `tfsdk:"real_owner"`
	Schedule        *taskScheduleModel `tfsdk:"schedule"`
}

type taskScheduleModel struct {
	Frequency             types.String `tfsdk:"frequency"`
	DayOfWeek             types.Set    `tfsdk:"day_of_week"`
	WeekOfMonth           types.Set    `tfsdk:"week_of_month"`
	Hour                  types.Int64  `tfsdk:"hour"`
	Minute                types.Int64  `tfsdk:"minute"`
	RepeatIntervalHours   types.Int64  `tfsdk:"repeat_interval_hours"`
	RepeatIntervalMinutes types.Int64  `tfsdk:"repeat_interval_minutes"`
	RepeatUntilHour       types.Int64  `tfsdk:"repeat_until_hour"`
}

func (r *scheduledTaskResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_scheduled_task"
}

func (r *scheduledTaskResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a user-defined script task in DSM Task Scheduler.\n\n" + taskExecutionWarning,

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Numeric task identifier assigned by DSM, as a string.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Task name shown in Task Scheduler.",
			},
			"user": schema.StringAttribute{
				Required: true,
				Description: "Account DSM runs the command as. There is no default: naming the account is how a configuration states, in the open, " +
					"which privileges the command gets. `root` gives it full control of the NAS and routes the call through DSM's privileged " +
					"`SYNO.Core.TaskScheduler.Root` API, which additionally re-confirms the provider password. " +
					"Changing this forces a new task, because the owner selects which API namespace DSM will accept the change through.",
				PlanModifiers: []planmodifier.String{
					// The owner is not an ordinary attribute: it decides whether a
					// modification goes to SYNO.Core.TaskScheduler or to its .Root
					// counterpart. Editing root -> operator in place would send an
					// unprivileged `set` for a task DSM holds as root-owned, which
					// fails mid-apply at best. Replacing the task keeps the privilege
					// transition explicit and atomic.
					stringplanmodifier.RequiresReplace(),
				},
			},
			"command": schema.StringAttribute{
				Required: true,
				Description: "Shell command DSM executes. Prefer a literal command over one interpolated from variables or remote data: an " +
					"interpolated command moves the privilege boundary to whoever controls that input.",
			},
			"enabled": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(true),
				Description: "Whether the task is active. A disabled task keeps its definition but never fires.",
			},
			"notify_email": schema.StringAttribute{
				Optional:    true,
				Description: "Address DSM emails when the task runs. Leaving this unset disables notification entirely.",
			},
			"notify_on_failure": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "Send the notification only when the command exits with an error, instead of after every run. Requires `notify_email`.",
			},
			"real_owner": schema.StringAttribute{
				Computed: true,
				Description: "Account DSM records as the owner of the task entry, which is not always the account the command runs as. " +
					"DSM requires it to address the task on update and delete.",
			},
		},

		Blocks: map[string]schema.Block{
			"schedule": schema.SingleNestedBlock{
				Description: "When the task runs. Required.\n\n" +
					"DSM Task Scheduler can express a daily, weekly, or monthly-by-weekday recurrence — it has no notion of a day of the month, " +
					"of specific months, or of a general cron expression. One-shot tasks that run on a single fixed date are not modelled here, " +
					"because a single past date does not describe an ongoing desired state.",
				Attributes: map[string]schema.Attribute{
					"frequency": schema.StringAttribute{
						Optional: true,
						Description: "`daily`, `weekly`, or `monthly`. `weekly` runs on the days listed in `day_of_week`; `monthly` runs on those " +
							"weekdays within the weeks listed in `week_of_month`, for example the first Sunday of each month.",
					},
					"day_of_week": schema.SetAttribute{
						Optional:    true,
						ElementType: types.StringType,
						Description: "Weekday names such as `sunday` or `monday`. Required for `weekly` and `monthly`; rejected for `daily`, which always runs every day.",
					},
					"week_of_month": schema.SetAttribute{
						Optional:    true,
						ElementType: types.StringType,
						Description: "Weeks of the month: `first`, `second`, `third`, `fourth`, or `last`. Required for `monthly` and rejected otherwise.",
					},
					"hour": schema.Int64Attribute{
						Optional:    true,
						Computed:    true,
						Default:     int64default.StaticInt64(0),
						Description: "Hour the task starts, 0-23.",
					},
					"minute": schema.Int64Attribute{
						Optional:    true,
						Computed:    true,
						Default:     int64default.StaticInt64(0),
						Description: "Minute the task starts, 0-59.",
					},
					"repeat_interval_hours": schema.Int64Attribute{
						Optional:    true,
						Computed:    true,
						Default:     int64default.StaticInt64(0),
						Description: "Re-run the task every this many hours within the same day, 0-23. `0` disables same-day repeats. Mutually exclusive with `repeat_interval_minutes`.",
					},
					"repeat_interval_minutes": schema.Int64Attribute{
						Optional:    true,
						Computed:    true,
						Default:     int64default.StaticInt64(0),
						Description: "Re-run the task every this many minutes within the same day. DSM accepts only 1, 5, 10, 15, 20, or 30; `0` disables same-day repeats. Mutually exclusive with `repeat_interval_hours`.",
					},
					"repeat_until_hour": schema.Int64Attribute{
						Optional:    true,
						Computed:    true,
						Description: "Last hour of the day at which a same-day repeat may still fire, 0-23. Defaults to `hour`.",
					},
				},
			},
		},
	}
}

func (r *scheduledTaskResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data := providerDataFrom(req.ProviderData, &resp.Diagnostics)
	if data == nil {
		return
	}
	r.client = data.client
	r.allowTaskExecution = data.allowTaskExecution
}

// ModifyPlan enforces the provider-level opt-in and warns about the privileges
// the planned task will hold.
//
// The check belongs here rather than in ValidateConfig because Terraform
// validates resource configuration before the provider is configured, so
// ValidateConfig cannot see allow_task_execution. Failing during plan means a
// team that leaves the flag off never gets as far as an apply.
//
// Destroy is deliberately not gated: removing a task executes nothing, and a
// configuration that has switched the flag back off must still be able to clean
// up the tasks it already created.
func (r *scheduledTaskResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	if r.client == nil {
		// The provider has not been configured yet, which happens when its own
		// configuration depends on values that are still unknown. Reporting the
		// opt-in as missing here would be wrong: it has not been read yet.
		return
	}

	var plan scheduledTaskResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	checkTaskExecutionAllowed(r.allowTaskExecution, "dsm_scheduled_task", &resp.Diagnostics)
	warnAboutTaskPrivileges(r.client, plan.User, plan.Name, &resp.Diagnostics)

	if plan.Schedule == nil {
		resp.Diagnostics.AddAttributeError(
			path.Root("schedule"),
			"Missing schedule block",
			"A dsm_scheduled_task requires a schedule block. Use a dsm_event_task instead for a task that runs on boot or shutdown.",
		)
		return
	}

	// Validate the schedule here rather than only on the way to DSM. An invalid
	// frequency or an out-of-range hour is knowable from the configuration
	// alone, and a resource that can execute code as root is the last place to
	// discover a typo halfway through an apply, with some resources already
	// created.
	validateScheduleAtPlan(ctx, plan.Schedule, &resp.Diagnostics)
}

// validateScheduleAtPlan runs the client's own encoding rules over a planned
// schedule, skipping any check whose inputs are still unknown. Reusing
// ValidateTaskSchedule keeps plan-time and request-time rules from drifting
// apart, which is what makes the promise in its doc comment true.
func validateScheduleAtPlan(ctx context.Context, schedule *taskScheduleModel, diags *diag.Diagnostics) {
	if scheduleHasUnknowns(schedule) {
		// A value computed from another resource is only knowable at apply time;
		// the request path still validates it before anything reaches DSM.
		return
	}

	var ignored diag.Diagnostics
	if _, err := client.ValidateTaskSchedule(taskScheduleFromModel(ctx, schedule, &ignored)); err != nil {
		diags.AddAttributeError(path.Root("schedule"), "Invalid schedule", err.Error())
	}
}

func scheduleHasUnknowns(schedule *taskScheduleModel) bool {
	return schedule.Frequency.IsUnknown() ||
		schedule.DayOfWeek.IsUnknown() ||
		schedule.WeekOfMonth.IsUnknown() ||
		schedule.Hour.IsUnknown() ||
		schedule.Minute.IsUnknown() ||
		schedule.RepeatIntervalHours.IsUnknown() ||
		schedule.RepeatIntervalMinutes.IsUnknown() ||
		schedule.RepeatUntilHour.IsUnknown()
}

// taskScheduleFromModel converts the Terraform model into the client's
// vocabulary. A null repeat_until_hour stays nil rather than becoming 0: the
// client treats those differently, and collapsing them would turn "end the
// window at the start hour" into "end it at midnight".
func taskScheduleFromModel(ctx context.Context, schedule *taskScheduleModel, diags *diag.Diagnostics) client.TaskSchedule {
	converted := client.TaskSchedule{
		Frequency:             schedule.Frequency.ValueString(),
		DaysOfWeek:            stringSetValues(ctx, schedule.DayOfWeek, diags),
		WeeksOfMonth:          stringSetValues(ctx, schedule.WeekOfMonth, diags),
		Hour:                  int(schedule.Hour.ValueInt64()),
		Minute:                int(schedule.Minute.ValueInt64()),
		RepeatIntervalHours:   int(schedule.RepeatIntervalHours.ValueInt64()),
		RepeatIntervalMinutes: int(schedule.RepeatIntervalMinutes.ValueInt64()),
	}
	if !schedule.RepeatUntilHour.IsNull() && !schedule.RepeatUntilHour.IsUnknown() {
		repeatUntilHour := int(schedule.RepeatUntilHour.ValueInt64())
		converted.RepeatUntilHour = &repeatUntilHour
	}
	return converted
}

func (r *scheduledTaskResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan scheduledTaskResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Defence in depth: the gate is enforced during plan, but a privilege check
	// this consequential should not depend on a single call site.
	checkTaskExecutionAllowed(r.allowTaskExecution, "dsm_scheduled_task", &resp.Diagnostics)
	createReq, ok := scheduledTaskRequestFromModel(ctx, &plan, &resp.Diagnostics)
	if !ok || resp.Diagnostics.HasError() {
		return
	}

	tflog.Warn(ctx, "Creating DSM scheduled task that executes a command on the NAS", map[string]interface{}{
		"name": plan.Name.ValueString(),
		"user": plan.User.ValueString(),
	})

	task, err := r.client.CreateScheduledTask(ctx, createReq)
	if err != nil && task != nil {
		// DSM created the task but the read-back failed. Record what is known
		// before reporting the error: without a state entry the task keeps
		// running on the NAS, as root, and destroy cannot remove it.
		applyScheduledTaskToModel(ctx, &plan, task, &resp.Diagnostics, applyForWrite)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		resp.Diagnostics.AddError(
			"Scheduled task was created but could not be read back",
			scheduledTaskErrorDetail(err)+"\n\nThe task has been recorded in state from the configuration so that it stays managed. "+
				"Run `terraform plan` to reconcile it with what DSM actually stored.",
		)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to create scheduled task", scheduledTaskErrorDetail(err))
		return
	}

	applyScheduledTaskToModel(ctx, &plan, task, &resp.Diagnostics, applyForWrite)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *scheduledTaskResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state scheduledTaskResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	task, err := lookupScheduledTask(ctx, r.client, state.ID.ValueString(), state.Name.ValueString())
	if errors.Is(err, client.ErrScheduledTaskNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read scheduled task", scheduledTaskErrorDetail(err))
		return
	}

	applyScheduledTaskToModel(ctx, &state, task, &resp.Diagnostics, applyForRead)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *scheduledTaskResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan scheduledTaskResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var state scheduledTaskResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id, err := strconv.Atoi(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid scheduled task id in state", fmt.Sprintf("Expected a numeric DSM task id, got %q.", state.ID.ValueString()))
		return
	}

	checkTaskExecutionAllowed(r.allowTaskExecution, "dsm_scheduled_task", &resp.Diagnostics)
	createReq, ok := scheduledTaskRequestFromModel(ctx, &plan, &resp.Diagnostics)
	if !ok || resp.Diagnostics.HasError() {
		return
	}

	tflog.Warn(ctx, "Updating DSM scheduled task that executes a command on the NAS", map[string]interface{}{
		"name": plan.Name.ValueString(),
		"user": plan.User.ValueString(),
	})

	// DSM's set replaces the whole task, so the request carries the complete
	// desired state rather than a delta.
	task, err := r.client.UpdateScheduledTask(ctx, id, client.UpdateScheduledTaskRequest{
		CreateScheduledTaskRequest: createReq,
		RealOwner:                  state.RealOwner.ValueString(),
	})
	if err != nil {
		resp.Diagnostics.AddError("Failed to update scheduled task", scheduledTaskErrorDetail(err))
		return
	}

	applyScheduledTaskToModel(ctx, &plan, task, &resp.Diagnostics, applyForWrite)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *scheduledTaskResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state scheduledTaskResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id, err := strconv.Atoi(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid scheduled task id in state", fmt.Sprintf("Expected a numeric DSM task id, got %q.", state.ID.ValueString()))
		return
	}

	realOwner := state.RealOwner.ValueString()
	if realOwner == "" {
		realOwner = state.User.ValueString()
	}

	tflog.Info(ctx, "Deleting DSM scheduled task", map[string]interface{}{"name": state.Name.ValueString(), "id": id})

	if err := r.client.DeleteScheduledTask(ctx, id, realOwner); err != nil {
		resp.Diagnostics.AddError("Failed to delete scheduled task", scheduledTaskErrorDetail(err))
	}
}

// ImportState accepts either the numeric DSM task id or the task name. Names
// are not unique in DSM, so an ambiguous name is rejected with an error naming
// the candidate ids rather than importing an arbitrary one.
func (r *scheduledTaskResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// lookupScheduledTask resolves a task from whatever identity state holds. After
// an import by name, ID carries the name rather than a number.
func lookupScheduledTask(ctx context.Context, c *client.Client, id, name string) (*client.ScheduledTask, error) {
	if numeric, err := strconv.Atoi(id); err == nil {
		return c.GetScheduledTask(ctx, numeric)
	}
	if id != "" {
		return c.GetScheduledTaskByName(ctx, id)
	}
	if name != "" {
		return c.GetScheduledTaskByName(ctx, name)
	}
	return nil, client.ErrScheduledTaskNotFound
}

func scheduledTaskRequestFromModel(ctx context.Context, model *scheduledTaskResourceModel, diags *diag.Diagnostics) (client.CreateScheduledTaskRequest, bool) {
	req := client.CreateScheduledTaskRequest{
		Name:            model.Name.ValueString(),
		Owner:           model.User.ValueString(),
		Command:         model.Command.ValueString(),
		Enabled:         model.Enabled.ValueBool(),
		NotifyEmail:     model.NotifyEmail.ValueString(),
		NotifyOnFailure: model.NotifyOnFailure.ValueBool(),
	}

	if model.Schedule == nil {
		diags.AddAttributeError(path.Root("schedule"), "Missing schedule block", "A dsm_scheduled_task requires a schedule block.")
		return req, false
	}

	req.Schedule = taskScheduleFromModel(ctx, model.Schedule, diags)

	if model.NotifyOnFailure.ValueBool() && model.NotifyEmail.ValueString() == "" {
		diags.AddAttributeError(
			path.Root("notify_on_failure"),
			"notify_on_failure requires notify_email",
			"DSM cannot send a failure notification without an address. Set notify_email or remove notify_on_failure.",
		)
		return req, false
	}

	// The schedule is validated by the client, which owns the DSM encoding
	// rules; surfacing that error here keeps the two from drifting apart.
	if _, err := client.ValidateTaskSchedule(req.Schedule); err != nil {
		diags.AddAttributeError(path.Root("schedule"), "Invalid schedule", err.Error())
		return req, false
	}

	return req, !diags.HasError()
}

// applyMode controls what happens when DSM reports a schedule this provider
// cannot represent.
type applyMode int

const (
	// applyForWrite is used after create and update, where an unrepresentable
	// schedule means the provider wrote something it cannot read back — a bug
	// worth failing on.
	applyForWrite applyMode = iota
	// applyForRead is used during refresh, where the same situation means an
	// administrator changed the task in DSM's UI. Failing there would break
	// every subsequent plan, including the destroy that would clean it up,
	// stranding the user on `-refresh=false` or `state rm`.
	applyForRead
)

func applyScheduledTaskToModel(ctx context.Context, model *scheduledTaskResourceModel, task *client.ScheduledTask, diags *diag.Diagnostics, mode applyMode) {
	model.ID = types.StringValue(strconv.Itoa(task.ID))
	model.Name = types.StringValue(task.Name)
	model.User = types.StringValue(task.Owner)
	model.Command = types.StringValue(task.Command)
	model.Enabled = types.BoolValue(task.Enabled)
	model.NotifyEmail = nullableString(task.NotifyEmail)
	model.NotifyOnFailure = types.BoolValue(task.NotifyOnFailure)
	model.RealOwner = types.StringValue(task.RealOwner)

	if task.Schedule.Frequency == "" {
		detail := fmt.Sprintf("DSM task %q uses a schedule this provider cannot represent — most likely a one-shot task tied to a single date. "+
			"Manage it in DSM instead, or recreate it with a daily, weekly, or monthly schedule.", task.Name)
		if mode == applyForWrite {
			diags.AddError("Unsupported task schedule", detail)
			return
		}
		// On refresh, report the drift but keep the resource usable: the recorded
		// schedule becomes null, so the next plan offers to put the configured
		// schedule back, and destroy still works.
		diags.AddWarning("Task schedule was changed outside Terraform", detail+
			"\n\nTerraform has recorded the schedule as unset. The next plan will offer to restore the schedule from the configuration.")
	}

	dayOfWeek, dayDiags := types.SetValueFrom(ctx, types.StringType, task.Schedule.DaysOfWeek)
	diags.Append(dayDiags...)
	weekOfMonth, weekDiags := types.SetValueFrom(ctx, types.StringType, task.Schedule.WeeksOfMonth)
	diags.Append(weekDiags...)

	model.Schedule = &taskScheduleModel{
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
		model.Schedule.RepeatUntilHour = types.Int64Value(int64(*task.Schedule.RepeatUntilHour))
	}

	// DSM reports an empty selection as an empty set; a configuration that
	// omitted the attribute holds null. Normalize so the two agree.
	if len(task.Schedule.DaysOfWeek) == 0 {
		model.Schedule.DayOfWeek = types.SetNull(types.StringType)
	}
	if len(task.Schedule.WeeksOfMonth) == 0 {
		model.Schedule.WeekOfMonth = types.SetNull(types.StringType)
	}
}

func stringSetValues(ctx context.Context, set types.Set, diags *diag.Diagnostics) []string {
	if set.IsNull() || set.IsUnknown() {
		return nil
	}
	var values []string
	diags.Append(set.ElementsAs(ctx, &values, false)...)
	return values
}

// checkTaskExecutionAllowed enforces the provider opt-in shared by both task
// resources.
func checkTaskExecutionAllowed(allowed bool, resourceType string, diags *diag.Diagnostics) {
	if allowed {
		return
	}
	diags.AddError(
		"Task execution is not enabled for this provider",
		fmt.Sprintf("%s creates a DSM task that runs a shell command on the NAS, normally as root. Because that turns write access to this "+
			"Terraform configuration into code execution on the NAS, the provider refuses these resources unless the capability is enabled "+
			"deliberately.\n\nSet `allow_task_execution = true` in the provider block, or export "+
			"`SYNOLOGY_DSM_ALLOW_TASK_EXECUTION=true`, once you are satisfied that everyone who can change this configuration is trusted with "+
			"root on the NAS.", resourceType),
	)
}

// warnAboutTaskPrivileges surfaces, at plan time, what the task will be able to
// do — and whether confirming the provider password to obtain it will cross an
// unencrypted connection.
func warnAboutTaskPrivileges(c *client.Client, user, name types.String, diags *diag.Diagnostics) {
	if user.IsNull() || user.IsUnknown() || user.ValueString() != "root" {
		return
	}

	diags.AddWarning(
		"Task will run as root",
		fmt.Sprintf("Task %q runs as root, so its command has full control of the NAS: every share, every package, and the logs that would "+
			"record what it did. This is DSM's privileged task API, not an ordinary account. Review the command in the plan output before applying.",
			name.ValueString()),
	)

	if c != nil && !c.UsesTLS() {
		diags.AddWarning(
			"Provider password will be sent over an unencrypted connection",
			"Creating a root-owned task requires DSM to re-confirm the provider password, and this provider is configured with a plain `http://` "+
				"host. The password will cross the network in clear text. Use an `https://` host for the DSM API.",
		)
	}
}

func scheduledTaskErrorDetail(err error) string {
	message := err.Error()
	switch {
	case errors.Is(err, client.ErrScheduledTaskNotFound):
		return message + "\n\nVerify the task id or name in DSM Task Scheduler. Existing tasks must be imported before Terraform can manage them."
	case client.IsAPIError(err, 102, 103, 104):
		return message + "\n\nDSM did not recognise the Task Scheduler API, method, or version. This provider targets DSM 7.x; DSM 6.x uses a different encoding for task schedules."
	case client.IsAPIError(err, 105):
		return message + "\n\nDSM denied the operation. Task Scheduler requires an administrator account, and creating a root-owned task additionally requires the provider password to be correct so DSM can re-confirm it."
	default:
		return message
	}
}
