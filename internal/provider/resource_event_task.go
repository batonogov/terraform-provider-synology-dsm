package provider

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

func NewEventTaskResource() resource.Resource {
	return &eventTaskResource{}
}

type eventTaskResource struct {
	client             *client.Client
	allowTaskExecution bool
}

type eventTaskResourceModel struct {
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

func (r *eventTaskResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_event_task"
}

func (r *eventTaskResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a task DSM runs when the NAS boots or shuts down, rather than on a schedule.\n\n" + taskExecutionWarning,

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Task name, which is also DSM's identifier for an event task.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Required: true,
				Description: "Task name shown in Task Scheduler. DSM addresses event tasks by name and gives them no numeric id, " +
					"so renaming one replaces it.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"user": schema.StringAttribute{
				Required: true,
				Description: "Account DSM runs the command as. There is no default: naming the account is how a configuration states, in the open, " +
					"which privileges the command gets. `root` gives it full control of the NAS and routes the call through DSM's privileged " +
					"`SYNO.Core.EventScheduler.Root` API, which additionally re-confirms the provider password. " +
					"Changing this forces a new task, because the owner selects which API namespace DSM will accept the change through.",
				PlanModifiers: []planmodifier.String{
					// See dsm_scheduled_task: switching owner in place would send the
					// modification to the wrong API namespace for the task DSM holds.
					stringplanmodifier.RequiresReplace(),
				},
			},
			"event": schema.StringAttribute{
				Required: true,
				Description: "System event that triggers the task: `bootup` or `shutdown`. A `bootup` task runs unattended on every restart, " +
					"including one nobody asked for.",
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
			"depends_on_tasks": schema.ListAttribute{
				Optional:    true,
				ElementType: types.StringType,
				Description: "Names of other tasks that must finish before this one starts. Order is preserved as given.",
			},
			"owner_uid": schema.Int64Attribute{
				Computed: true,
				Description: "Numeric uid DSM records for the owning account. DSM identifies an event task's owner by uid rather than by name, " +
					"so it is reported here for reference.",
			},
		},
	}
}

func (r *eventTaskResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data := providerDataFrom(req.ProviderData, &resp.Diagnostics)
	if data == nil {
		return
	}
	r.client = data.client
	r.allowTaskExecution = data.allowTaskExecution
}

// ModifyPlan enforces the provider opt-in and the privilege warnings. See the
// equivalent method on dsm_scheduled_task for why this is not ValidateConfig.
func (r *eventTaskResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	if r.client == nil {
		// See dsm_scheduled_task: the provider is not configured yet, so the
		// opt-in has not been read and must not be reported as missing.
		return
	}

	var plan eventTaskResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	checkTaskExecutionAllowed(r.allowTaskExecution, "dsm_event_task", &resp.Diagnostics)
	warnAboutTaskPrivileges(r.client, plan.User, plan.Name, &resp.Diagnostics)

	// Validate through the same builder the apply path uses, so a bad event or a
	// notification without an address is refused during plan rather than
	// halfway through an apply.
	eventTaskRequestFromModel(ctx, &plan, &resp.Diagnostics)
}

func (r *eventTaskResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan eventTaskResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Defence in depth: the gate is enforced during plan, but a privilege check
	// this consequential should not depend on a single call site.
	checkTaskExecutionAllowed(r.allowTaskExecution, "dsm_event_task", &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Warn(ctx, "Creating DSM event task that executes a command on the NAS", map[string]interface{}{
		"name":  plan.Name.ValueString(),
		"user":  plan.User.ValueString(),
		"event": plan.Event.ValueString(),
	})

	// Build the request first. Passing the builder as a call argument would let
	// Go evaluate it *and* reach DSM before any diagnostic it raised could be
	// checked, creating a root-capable task that no state entry tracks.
	createReq, ok := eventTaskRequestFromModel(ctx, &plan, &resp.Diagnostics)
	if !ok {
		return
	}

	task, err := r.client.CreateEventTask(ctx, createReq)
	if err != nil && task != nil {
		// DSM created the task but the read-back failed. Record what is known
		// before reporting: an untracked boot task keeps running as root and
		// destroy cannot remove it.
		applyEventTaskToModel(ctx, &plan, task, &resp.Diagnostics)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		resp.Diagnostics.AddError(
			"Event task was created but could not be read back",
			eventTaskErrorDetail(err)+"\n\nThe task has been recorded in state from the configuration so that it stays managed. "+
				"Run `terraform plan` to reconcile it with what DSM actually stored.",
		)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to create event task", eventTaskErrorDetail(err))
		return
	}

	applyEventTaskToModel(ctx, &plan, task, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *eventTaskResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state eventTaskResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	name := state.ID.ValueString()
	if name == "" {
		name = state.Name.ValueString()
	}

	task, err := r.client.GetEventTask(ctx, name)
	if errors.Is(err, client.ErrEventTaskNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read event task", eventTaskErrorDetail(err))
		return
	}

	applyEventTaskToModel(ctx, &state, task, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *eventTaskResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan eventTaskResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	checkTaskExecutionAllowed(r.allowTaskExecution, "dsm_event_task", &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Warn(ctx, "Updating DSM event task that executes a command on the NAS", map[string]interface{}{
		"name": plan.Name.ValueString(),
		"user": plan.User.ValueString(),
	})

	// Built before the call for the same reason as in Create: an argument-position
	// builder would have already reached DSM by the time its diagnostics could be
	// inspected.
	updateReq, ok := eventTaskRequestFromModel(ctx, &plan, &resp.Diagnostics)
	if !ok {
		return
	}

	// DSM's set replaces the whole task, so the request carries the complete
	// desired state rather than a delta.
	task, err := r.client.UpdateEventTask(ctx, updateReq)
	if err != nil {
		resp.Diagnostics.AddError("Failed to update event task", eventTaskErrorDetail(err))
		return
	}

	applyEventTaskToModel(ctx, &plan, task, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *eventTaskResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state eventTaskResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	name := state.Name.ValueString()
	if name == "" {
		name = state.ID.ValueString()
	}

	tflog.Info(ctx, "Deleting DSM event task", map[string]interface{}{"name": name})

	if err := r.client.DeleteEventTask(ctx, name); err != nil {
		resp.Diagnostics.AddError("Failed to delete event task", eventTaskErrorDetail(err))
	}
}

func (r *eventTaskResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// eventTaskRequestFromModel builds the client request and reports whether it is
// safe to send. It owns the validation so that plan and apply enforce exactly
// the same rules — the same arrangement dsm_scheduled_task uses.
func eventTaskRequestFromModel(ctx context.Context, model *eventTaskResourceModel, diags *diag.Diagnostics) (client.EventTaskRequest, bool) {
	var dependsOn []string
	if !model.DependsOnTasks.IsNull() && !model.DependsOnTasks.IsUnknown() {
		diags.Append(model.DependsOnTasks.ElementsAs(ctx, &dependsOn, false)...)
	}

	req := client.EventTaskRequest{
		Name:            model.Name.ValueString(),
		Owner:           model.User.ValueString(),
		Event:           model.Event.ValueString(),
		Command:         model.Command.ValueString(),
		Enabled:         model.Enabled.ValueBool(),
		NotifyEmail:     model.NotifyEmail.ValueString(),
		NotifyOnFailure: model.NotifyOnFailure.ValueBool(),
		DependsOn:       dependsOn,
	}

	if !model.Event.IsUnknown() && !slices.Contains(client.EventTriggers, req.Event) {
		diags.AddAttributeError(
			path.Root("event"),
			"Unsupported event",
			fmt.Sprintf("DSM triggers event tasks on %s. Use a dsm_scheduled_task for anything time-based.", strings.Join(client.EventTriggers, " or ")),
		)
	}
	if model.NotifyOnFailure.ValueBool() && req.NotifyEmail == "" && !model.NotifyEmail.IsUnknown() {
		diags.AddAttributeError(
			path.Root("notify_on_failure"),
			"notify_on_failure requires notify_email",
			"DSM cannot send a failure notification without an address. Set notify_email or remove notify_on_failure.",
		)
	}

	return req, !diags.HasError()
}

func applyEventTaskToModel(ctx context.Context, model *eventTaskResourceModel, task *client.EventTask, diags *diag.Diagnostics) {
	model.ID = types.StringValue(task.Name)
	model.Name = types.StringValue(task.Name)
	model.Event = types.StringValue(task.Event)
	model.Command = types.StringValue(task.Command)
	model.Enabled = types.BoolValue(task.Enabled)
	model.NotifyEmail = nullableString(task.NotifyEmail)
	model.NotifyOnFailure = types.BoolValue(task.NotifyOnFailure)
	model.OwnerUID = types.Int64Value(int64(task.OwnerUID))

	// Read is expected to populate every state field. DSM reports the owner
	// through the uid map it stores, but this API's read path is the least
	// verified part of the contract, so handle a missing owner explicitly rather
	// than silently leaving a required attribute null: keep whatever the prior
	// state or plan held, and say so when there is nothing to keep, which is the
	// case on a fresh import.
	switch {
	case task.Owner != "":
		model.User = types.StringValue(task.Owner)
	case !model.User.IsNull() && !model.User.IsUnknown():
		// Leave the known value in place.
	default:
		model.User = types.StringNull()
		diags.AddWarning(
			"DSM did not report the owner of this event task",
			fmt.Sprintf("Task %q was read back without an owner, so `user` could not be populated. Set `user` in the configuration to the account "+
				"the task should run as; the next apply will write it to DSM.", task.Name),
		)
	}

	if len(task.DependsOn) == 0 {
		model.DependsOnTasks = types.ListNull(types.StringType)
		return
	}
	dependsOn, listDiags := types.ListValueFrom(ctx, types.StringType, task.DependsOn)
	diags.Append(listDiags...)
	model.DependsOnTasks = dependsOn
}

func eventTaskErrorDetail(err error) string {
	message := err.Error()
	switch {
	case errors.Is(err, client.ErrEventTaskNotFound):
		return message + "\n\nVerify the task name in DSM Task Scheduler. Existing tasks must be imported before Terraform can manage them."
	case client.IsAPIError(err, 102, 103, 104):
		return message + "\n\nDSM did not recognise the Event Scheduler API, method, or version. This provider targets DSM 7.x."
	case client.IsAPIError(err, 105):
		return message + "\n\nDSM denied the operation. Event tasks require an administrator account, and creating a root-owned task additionally requires the provider password to be correct so DSM can re-confirm it."
	default:
		return message
	}
}
