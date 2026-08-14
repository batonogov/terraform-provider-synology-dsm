package provider

import (
	"context"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

const notificationMailID = "notification_mail"

func NewNotificationMailResource() resource.Resource {
	return &notificationMailResource{}
}

type notificationMailResource struct {
	client *client.Client
}

type notificationMailResourceModel struct {
	ID           types.String `tfsdk:"id"`
	Enabled      types.Bool   `tfsdk:"enabled"`
	SMTPServer   types.String `tfsdk:"smtp_server"`
	SMTPPort     types.Int64  `tfsdk:"smtp_port"`
	Sender       types.String `tfsdk:"sender"`
	UseTLS       types.Bool   `tfsdk:"use_tls"`
	SMTPAuth     types.Bool   `tfsdk:"smtp_auth"`
	SMTPUsername types.String `tfsdk:"smtp_username"`
	SMTPPassword types.String `tfsdk:"smtp_password"`
}

func (r *notificationMailResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_notification_mail"
}

func (r *notificationMailResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages the outgoing SMTP configuration DSM uses for every notification it sends — " +
			"Task Scheduler failures, storage degradation, security advisories. This is the transport; " +
			"per-task recipients (`notify_email`) are separate and stay on the task resources.\n\n" +
			"The SMTP password is write-only: DSM never returns it, so the configured value is the only record " +
			"and is re-sent on every write that enables authentication. Use an app password where the provider allows.\n\n" +
			"This is a NAS-wide singleton: declare at most one instance per DSM host.",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Always `notification_mail`.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"enabled": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Whether mail notifications are switched on. Defaults to `true`.",
			},
			"smtp_server": schema.StringAttribute{
				Required:    true,
				Description: "Outgoing SMTP server hostname or IP.",
			},
			"smtp_port": schema.Int64Attribute{
				Optional:    true,
				Computed:    true,
				Description: "SMTP port. Defaults to `587` when DSM reports nothing.",
			},
			"sender": schema.StringAttribute{
				Required:    true,
				Description: "Sender address DSM puts on every notification.",
			},
			"use_tls": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Use TLS (STARTTLS) towards the SMTP server. Defaults to `true`.",
			},
			"smtp_auth": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Authenticate to the SMTP server with username and password. Defaults to `false`.",
			},
			"smtp_username": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "SMTP authentication username. Used only when `smtp_auth` is true.",
			},
			"smtp_password": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				Description: "SMTP authentication password (sensitive). DSM never returns it, so drift on this " +
					"attribute cannot be detected — set it and it is pushed on the next write. " + sensitiveStateWarning,
				PlanModifiers: []planmodifier.String{
					// Never let a write-only value produce a diff on its own.
					stringplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

func (r *notificationMailResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	dsmClient := clientFromProviderData(req.ProviderData, &resp.Diagnostics)
	if dsmClient == nil {
		return
	}

	r.client = dsmClient
}

func (r *notificationMailResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan notificationMailResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.write(ctx, &plan, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *notificationMailResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state notificationMailResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	config, err := r.client.GetNotificationMail(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read mail notification settings", err.Error())
		return
	}

	state.ID = types.StringValue(notificationMailID)
	state.Enabled = types.BoolValue(config.Enabled)
	state.SMTPServer = types.StringValue(config.SMTPServer)
	state.SMTPPort = types.Int64Value(int64(config.SMTPPort))
	state.Sender = types.StringValue(config.Sender)
	state.UseTLS = types.BoolValue(config.UseTLS)
	state.SMTPAuth = types.BoolValue(config.SMTPAuth)
	if config.SMTPUsername != "" || state.SMTPUsername.IsNull() {
		state.SMTPUsername = types.StringValue(config.SMTPUsername)
	}
	// smtp_password is write-only: keep whatever state holds.

	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *notificationMailResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan notificationMailResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.write(ctx, &plan, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *notificationMailResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	// Removing the resource disables mail notifications rather than clearing
	// the SMTP configuration: DSM keeps the server settings, and the next
	// declaration can re-enable without re-entering credentials.
	var state notificationMailResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.client.SetNotificationMail(ctx, client.SetNotificationMailRequest{
		Enable:     false,
		SMTPServer: state.SMTPServer.ValueString(),
		SMTPPort:   int(state.SMTPPort.ValueInt64()),
		Sender:     state.Sender.ValueString(),
		UseTLS:     state.UseTLS.ValueBool(),
		SMTPAuth:   false,
	}); err != nil {
		// The notification transport being unremovable must not wedge
		// destroys of unrelated resources in the same state.
		tflog.Warn(ctx, "Failed to disable mail notification on destroy", map[string]interface{}{
			"error": err.Error(),
		})
	}

	resp.State.RemoveResource(ctx)
}

func (r *notificationMailResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *notificationMailResource) write(ctx context.Context, plan *notificationMailResourceModel, diags *diag.Diagnostics) {
	enabled := true
	if !plan.Enabled.IsNull() {
		enabled = plan.Enabled.ValueBool()
	}
	useTLS := true
	if !plan.UseTLS.IsNull() {
		useTLS = plan.UseTLS.ValueBool()
	}

	setReq := client.SetNotificationMailRequest{
		Enable:       enabled,
		SMTPServer:   plan.SMTPServer.ValueString(),
		SMTPPort:     int(plan.SMTPPort.ValueInt64()),
		Sender:       plan.Sender.ValueString(),
		UseTLS:       useTLS,
		SMTPAuth:     plan.SMTPAuth.ValueBool(),
		SMTPUsername: plan.SMTPUsername.ValueString(),
		SMTPPassword: plan.SMTPPassword.ValueString(),
	}

	if err := r.client.SetNotificationMail(ctx, setReq); err != nil {
		diags.AddError("Failed to write mail notification settings", err.Error())
		return
	}

	config, err := r.client.GetNotificationMail(ctx)
	if err != nil {
		diags.AddError("Failed to read mail notification settings", err.Error())
		return
	}

	plan.ID = types.StringValue(notificationMailID)
	plan.Enabled = types.BoolValue(config.Enabled)
	plan.SMTPServer = types.StringValue(config.SMTPServer)
	plan.SMTPPort = types.Int64Value(int64(config.SMTPPort))
	plan.Sender = types.StringValue(config.Sender)
	plan.UseTLS = types.BoolValue(config.UseTLS)
	plan.SMTPAuth = types.BoolValue(config.SMTPAuth)
	plan.SMTPUsername = types.StringValue(config.SMTPUsername)
	// Write-only password: keep the planned value in state.
}
