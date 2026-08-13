package provider

import (
	"context"
	"errors"
	"fmt"
	pathpkg "path"
	"strings"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	tfpath "github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

func NewContainerProjectResource() resource.Resource {
	return &containerProjectResource{}
}

type containerProjectResource struct {
	client *client.Client
}

type containerProjectResourceModel struct {
	ID              types.String `tfsdk:"id"`
	Name            types.String `tfsdk:"name"`
	SharePath       types.String `tfsdk:"share_path"`
	ComposeYAML     types.String `tfsdk:"compose_yaml"`
	Running         types.Bool   `tfsdk:"running"`
	DeleteOnDestroy types.Bool   `tfsdk:"delete_on_destroy"`
	Path            types.String `tfsdk:"path"`
	Status          types.String `tfsdk:"status"`
	ContainerIDs    types.List   `tfsdk:"container_ids"`
}

func (r *containerProjectResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_container_project"
}

func (r *containerProjectResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Creates and manages a Docker Compose project in Synology Container Manager. " +
			"Destroy leaves the project and its workloads intact unless `delete_on_destroy` is explicitly enabled.\n\n" +
			"Two things about bind mounts are worth knowing before writing the compose file, because neither " +
			"fails in a way that points at the cause:\n\n" +
			"* **Every Synology shared folder contains an `@eaDir` metadata directory.** Bind-mounting a share " +
			"root at a path a service expects to find empty therefore fails — PostgreSQL's `initdb` reports " +
			"\"directory exists but is not empty\". Mount a subdirectory of the share instead.\n" +
			"* **A bind mount enforces POSIX mode bits, not the Synology ACL.** A shared folder created through " +
			"DSM normally has mode `000` with its real rules in the ACL, which DSM and SMB honour and Docker " +
			"does not — so a container running as anything but root cannot read or write it. `dsm_shared_folder` " +
			"and `dsm_file` report the mode in `posix_mode`, but no DSM API can change it; that needs a `chmod` " +
			"on the NAS.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Project UUID assigned by Container Manager.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Unique Container Manager project name.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"share_path": schema.StringAttribute{
				Required: true,
				Description: "File Station path for the project directory, for example `/docker/s3-storage`. " +
					"This is not an absolute volume path such as `/volume1/docker/s3-storage`.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"compose_yaml": schema.StringAttribute{
				Required:  true,
				Sensitive: true,
				Description: "Docker Compose YAML managed by Container Manager. Sensitive output is redacted, but the value is still stored in Terraform state; " +
					"use an encrypted remote state backend and avoid embedding long-lived secrets.\n\n" +
					"**Relative bind mounts do not work the way plain Docker leads you to expect.** A mount such as " +
					"`./conf:/conf` fails the build with `Bind mount failed: '/volume1/.../conf' does not exist`, because " +
					"Container Manager does not create host directories. Use a named volume, point the mount at a path " +
					"that already exists, or create the file with `dsm_file` first — which also keeps configuration and " +
					"secrets out of the compose document.",
			},
			"running": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(true),
				Description: "Whether the project should be running. Compose changes stop, rebuild, and restore the project deterministically.",
			},
			"delete_on_destroy": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				Description: "Delete the project from Container Manager during `terraform destroy`. Defaults to `false`, so destroy only removes it from Terraform state. " +
					"When enabled, the provider asks DSM to preserve the project directory, but DSM may still remove project containers, networks, and related data.",
			},
			"path": schema.StringAttribute{
				Computed:    true,
				Description: "Absolute volume path reported by DSM for the project directory.",
			},
			"status": schema.StringAttribute{
				Computed: true,
				Description: "Raw project lifecycle status reported by Container Manager. `WARNING` means some containers " +
					"are running and some are not — the steady state of a compose file with a one-shot init container, " +
					"which exits once its work is done. The provider treats it as running and raises a Terraform warning, " +
					"since the same status also covers a container that failed to start. Assert on this attribute if a " +
					"deployment must have every container up.",
			},
			"container_ids": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				Description: "Container identifiers currently associated with the project.",
			},
		},
	}
}

func (r *containerProjectResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config containerProjectResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !config.Name.IsNull() && !config.Name.IsUnknown() {
		name := strings.TrimSpace(config.Name.ValueString())
		if name == "" || name != config.Name.ValueString() || strings.Contains(name, "/") {
			resp.Diagnostics.AddAttributeError(tfpath.Root("name"), "Invalid project name", "Project name must be non-empty, cannot contain `/`, and cannot start or end with whitespace.")
		}
	}
	if !config.SharePath.IsNull() && !config.SharePath.IsUnknown() {
		sharePath := config.SharePath.ValueString()
		clean := pathpkg.Clean(sharePath)
		if !strings.HasPrefix(sharePath, "/") || clean != sharePath || clean == "/" || len(strings.Split(strings.TrimPrefix(clean, "/"), "/")) < 2 {
			resp.Diagnostics.AddAttributeError(
				tfpath.Root("share_path"),
				"Invalid File Station project path",
				"Use a normalized directory inside a DSM shared folder, such as `/docker/s3-storage`. Do not use a volume path or a trailing slash.",
			)
		}
		if dsmVolumePath.MatchString(clean) {
			resp.Diagnostics.AddAttributeError(tfpath.Root("share_path"), "Volume path is not a File Station path", "Use `/docker/s3-storage`, not `/volume1/docker/s3-storage`.")
		}
	}
	if !config.ComposeYAML.IsNull() && !config.ComposeYAML.IsUnknown() && strings.TrimSpace(config.ComposeYAML.ValueString()) == "" {
		resp.Diagnostics.AddAttributeError(tfpath.Root("compose_yaml"), "Empty Docker Compose configuration", "`compose_yaml` must contain a Docker Compose document.")
	}
}

func (r *containerProjectResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	dsmClient := clientFromProviderData(req.ProviderData, &resp.Diagnostics)
	if dsmClient == nil {
		return
	}
	r.client = dsmClient
}

func (r *containerProjectResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan containerProjectResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Creating Container Manager project", map[string]interface{}{"name": plan.Name.ValueString(), "share_path": plan.SharePath.ValueString()})
	project, err := r.client.CreateContainerProject(ctx, plan.Name.ValueString(), plan.SharePath.ValueString(), plan.ComposeYAML.ValueString(), plan.Running.ValueBool())
	if err != nil {
		resp.Diagnostics.AddError("Failed to create Container Manager project", containerProjectErrorDetail(err))
		return
	}
	applyContainerProjectToResourceModel(ctx, &plan, project, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *containerProjectResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state containerProjectResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	lookup := state.ID.ValueString()
	if lookup == "" {
		lookup = state.Name.ValueString()
	}
	project, err := r.client.GetContainerProject(ctx, lookup)
	if errors.Is(err, client.ErrContainerProjectNotFound) {
		name := state.Name.ValueString()
		if name == "" {
			name = lookup
		}
		project, err = r.client.GetContainerProjectByName(ctx, name)
	}
	if errors.Is(err, client.ErrContainerProjectNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read Container Manager project", containerProjectErrorDetail(err))
		return
	}

	if state.DeleteOnDestroy.IsNull() || state.DeleteOnDestroy.IsUnknown() {
		state.DeleteOnDestroy = types.BoolValue(false)
	}
	applyContainerProjectToResourceModel(ctx, &state, project, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *containerProjectResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan containerProjectResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	project, err := r.client.UpdateContainerProject(ctx, plan.ID.ValueString(), plan.ComposeYAML.ValueString(), plan.Running.ValueBool())
	if err != nil {
		resp.Diagnostics.AddError("Failed to update Container Manager project", containerProjectErrorDetail(err))
		return
	}
	applyContainerProjectToResourceModel(ctx, &plan, project, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *containerProjectResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state containerProjectResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !state.DeleteOnDestroy.ValueBool() {
		tflog.Warn(ctx, "Leaving Container Manager project intact because delete_on_destroy is false", map[string]interface{}{"name": state.Name.ValueString()})
		return
	}

	tflog.Warn(ctx, "Deleting Container Manager project", map[string]interface{}{"name": state.Name.ValueString(), "id": state.ID.ValueString()})
	if err := r.client.DeleteContainerProject(ctx, state.ID.ValueString()); err != nil {
		resp.Diagnostics.AddError("Failed to delete Container Manager project", containerProjectErrorDetail(err))
	}
}

func (r *containerProjectResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// UUID and project name are both accepted. Read normalizes either form to
	// the UUID returned by DSM.
	resource.ImportStatePassthroughID(ctx, tfpath.Root("id"), req, resp)
}

func applyContainerProjectToResourceModel(ctx context.Context, model *containerProjectResourceModel, project *client.ContainerProject, diags *diag.Diagnostics) {
	model.ID = types.StringValue(project.ID)
	model.Name = types.StringValue(project.Name)
	model.SharePath = types.StringValue(project.SharePath)
	model.ComposeYAML = types.StringValue(project.ComposeYAML)
	model.Running = types.BoolValue(project.Running())
	model.Path = types.StringValue(project.Path)
	model.Status = types.StringValue(project.Status)
	containerIDs, listDiags := types.ListValueFrom(ctx, types.StringType, project.ContainerIDs)
	diags.Append(listDiags...)
	model.ContainerIDs = containerIDs

	// A project counts as running while some of its containers are not, because
	// that is the steady state of a one-shot init container. Say so rather than
	// letting it pass silently: the same status covers a container that died for
	// a reason worth knowing about.
	if project.PartiallyRunning() {
		diags.AddWarning(
			"Container Manager project is only partially running",
			fmt.Sprintf("Project %q reports status %q: some of its containers are not running while others are.\n\n"+
				"This is expected when the compose file uses a one-shot init container — it exits once its work is done. "+
				"If none was intended, inspect the project in Container Manager: a container that failed to start leaves "+
				"the project in the same status.", project.Name, project.Status),
		)
	}
}

func containerProjectErrorDetail(err error) string {
	message := err.Error()
	switch {
	case errors.Is(err, client.ErrContainerProjectNotFound):
		return message + "\n\nVerify the project name or UUID in Container Manager. Existing projects must be imported before Terraform can manage them."
	case strings.Contains(message, "already exists"):
		return message + "\n\nImport the existing project by name or UUID instead of creating a second project with the same name."
	case client.IsAPIError(err, 102, 103):
		return message + "\n\nThe Container Manager project API is unavailable. Install and start the `ContainerManager` package on compatible physical Synology hardware; Virtual DSM does not provide this package."
	case client.IsAPIError(err, 105):
		return message + "\n\nDSM denied the Container Manager operation. Use an administrator account and verify access to the project shared folder."
	default:
		return message
	}
}
