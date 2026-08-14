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
	ID                   types.String `tfsdk:"id"`
	Name                 types.String `tfsdk:"name"`
	SharePath            types.String `tfsdk:"share_path"`
	ComposeYAML          types.String `tfsdk:"compose_yaml"`
	ComposeYAMLWO        types.String `tfsdk:"compose_yaml_wo"`
	ComposeYAMLWOVersion types.Int64  `tfsdk:"compose_yaml_wo_version"`
	ComposeChecksum      types.String `tfsdk:"compose_yaml_checksum"`
	Running              types.Bool   `tfsdk:"running"`
	DeleteOnDestroy      types.Bool   `tfsdk:"delete_on_destroy"`
	Path                 types.String `tfsdk:"path"`
	Status               types.String `tfsdk:"status"`
	ContainerIDs         types.List   `tfsdk:"container_ids"`
}

// composeChecksumKey names the private-state entry holding the checksum of the
// compose document this provider last wrote. With compose_yaml_wo the document
// itself is not in state, so it is the only thing a refreshed
// compose_yaml_checksum can be compared against.
const composeChecksumKey = "compose_checksum"

func (r *containerProjectResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_container_project"
}

func (r *containerProjectResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Creates and manages a Docker Compose project in Synology Container Manager. " +
			"Destroy leaves the project and its workloads intact unless `delete_on_destroy` is explicitly enabled.\n\n" +
			"Two things about bind mounts are worth knowing before writing the compose file, because neither " +
			"fails in a way that points at the cause.\n\n" +
			"Every Synology shared folder contains an `@eaDir` metadata directory, so bind-mounting a share " +
			"root at a path a service expects to find empty fails — PostgreSQL's `initdb` reports \"directory " +
			"exists but is not empty\". Mount a subdirectory of the share instead.\n\n" +
			"A bind mount also enforces POSIX mode bits rather than the Synology ACL. A shared folder created " +
			"through DSM normally has mode `000` with its real rules in the ACL, which DSM and SMB honour and " +
			"Docker does not — so a container running as anything but root cannot read or write it. " +
			"`dsm_shared_folder` and `dsm_file` report the mode in `posix_mode`, but no DSM API can change it; " +
			"that needs a `chmod` on the NAS.\n\n" +
			"**Importing a project that is going to be managed through `compose_yaml_wo` writes its compose document " +
			"to state once.** `terraform import` is followed by a refresh, and a refresh has no access to the " +
			"configuration: the only marker for write-only mode is `compose_yaml_wo_version`, which reaches state on " +
			"the first apply and not before. Treat credentials in an imported document as having been in state.",
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
				Optional:  true,
				Sensitive: true,
				Description: "Docker Compose YAML managed by Container Manager. Exactly one of `compose_yaml` or " +
					"`compose_yaml_wo` must be set. Sensitive output is redacted, but this form of the value is still " +
					"stored in Terraform state; use `compose_yaml_wo` when the document carries credentials.\n\n" +
					"**Relative bind mounts do not work the way plain Docker leads you to expect.** A mount such as " +
					"`./conf:/conf` fails the build with `Bind mount failed: '/volume1/.../conf' does not exist`, because " +
					"Container Manager does not create host directories. Use a named volume, point the mount at a path " +
					"that already exists, or create the file with `dsm_file` first — which also keeps configuration and " +
					"secrets out of the compose document.",
			},
			"compose_yaml_wo": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				WriteOnly: true,
				Description: "Docker Compose YAML that is never written to Terraform state or to the plan file " +
					"(a write-only argument, Terraform 1.11 and later). Requires `compose_yaml_wo_version`. " +
					"Because Terraform cannot diff a value it does not store, an edit to the configured document is only " +
					"sent to DSM when `compose_yaml_wo_version` changes — but a project edited *outside* Terraform is " +
					"still detected, because `compose_yaml_checksum` is compared against the checksum of the last write.",
			},
			"compose_yaml_wo_version": schema.Int64Attribute{
				Optional: true,
				Description: "Version counter for `compose_yaml_wo`. Required with it and rejected without it. " +
					"Increment it to send the compose document to DSM again. It is also the marker that keeps the " +
					"document out of state on refresh: a project imported before `compose_yaml_wo` was adopted holds " +
					"its compose document in state until the first apply that sets both attributes.",
			},
			"compose_yaml_checksum": schema.StringAttribute{
				Computed: true,
				Description: "SHA-256 checksum (hex) of the compose document Container Manager holds. Changes when the " +
					"project is edited outside Terraform, which is the drift signal `compose_yaml_wo` relies on. " +
					"It stays in state even when the document does not; a compose file is rarely guessable, but the checksum " +
					"is still a value somebody with access to state could search against candidate documents.",
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
	for _, document := range []struct {
		attribute string
		value     types.String
	}{
		{"compose_yaml", config.ComposeYAML},
		{"compose_yaml_wo", config.ComposeYAMLWO},
	} {
		if !document.value.IsNull() && !document.value.IsUnknown() && strings.TrimSpace(document.value.ValueString()) == "" {
			resp.Diagnostics.AddAttributeError(
				tfpath.Root(document.attribute),
				"Empty Docker Compose configuration",
				fmt.Sprintf("`%s` must contain a Docker Compose document.", document.attribute),
			)
		}
	}

	composeSet := !config.ComposeYAML.IsNull()
	writeOnlySet := !config.ComposeYAMLWO.IsNull()
	switch {
	case composeSet && writeOnlySet:
		resp.Diagnostics.AddError(
			"Conflicting Docker Compose configuration",
			"Set either `compose_yaml` or `compose_yaml_wo`, not both. The write-only form keeps the document out of Terraform state.",
		)
	case !composeSet && !writeOnlySet:
		resp.Diagnostics.AddError(
			"Missing Docker Compose configuration",
			"One of `compose_yaml` or `compose_yaml_wo` must be set.",
		)
	}

	// The version companion is what makes a write-only value usable: Terraform
	// cannot diff a value it never stored, so the counter is the only way to ask
	// for a rewrite — and its presence in state is what tells Read to keep the
	// compose document out of state.
	versionSet := !config.ComposeYAMLWOVersion.IsNull()
	switch {
	case writeOnlySet && !versionSet:
		resp.Diagnostics.AddAttributeError(
			tfpath.Root("compose_yaml_wo_version"),
			"Missing compose_yaml_wo_version",
			"`compose_yaml_wo_version` is required with `compose_yaml_wo`. Start at `1` and increment it whenever the document should be sent to DSM again.",
		)
	case !writeOnlySet && versionSet:
		resp.Diagnostics.AddAttributeError(
			tfpath.Root("compose_yaml_wo_version"),
			"Unused compose_yaml_wo_version",
			"`compose_yaml_wo_version` only applies to `compose_yaml_wo`. Remove it, or move the document to the write-only attribute.",
		)
	}
}

// ModifyPlan resolves the computed attributes an apply rewrites, and is what
// lets a write-only project heal after an edit made outside Terraform.
//
// Terraform carries computed attributes forward from prior state when it builds
// a plan, so a project whose compose document is changing would otherwise plan
// the old checksum and be handed a different one after apply — which Terraform
// reports as "Provider produced inconsistent result after apply". Marking them
// unknown also produces the diff write-only mode has no other way to express.
func (r *containerProjectResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	// Create has no prior state to compare against, destroy has no plan.
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}

	var state, plan containerProjectResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	lastWritten, diags := req.Private.GetKey(ctx, composeChecksumKey)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	composeChanges := composeWillChange(state, plan, parsePrivateChecksum(lastWritten))
	// Any planned change at all reaches Update, and Update rebuilds the project
	// and reports a fresh status and container ids — that holds for starting and
	// stopping it, and even for the Terraform-only delete_on_destroy flag. The
	// drift case is the other way round: nothing in the plan differs, and the
	// rewrite is asked for by a checksum that no longer matches the last write.
	if !composeChanges && req.Plan.Raw.Equal(req.State.Raw) {
		return
	}

	// Everything the update reports back is planned as unknown rather than
	// carried forward from the previous state, which apply would then contradict.
	if composeChanges {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, tfpath.Root("compose_yaml_checksum"), types.StringUnknown())...)
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, tfpath.Root("status"), types.StringUnknown())...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, tfpath.Root("container_ids"), types.ListUnknown(types.StringType))...)
}

// composeWillChange reports whether an apply is going to write the compose
// document again. See fileContentWillChange: the reasoning is the same, with
// the compose document in place of the file content.
func composeWillChange(state, plan containerProjectResourceModel, lastWritten string) bool {
	switch {
	case !plan.ComposeYAML.Equal(state.ComposeYAML):
		return true
	case !plan.ComposeYAMLWOVersion.Equal(state.ComposeYAMLWOVersion):
		return true
	case !composeWriteOnly(plan):
		return false
	}
	return lastWritten != "" && !state.ComposeChecksum.IsNull() && lastWritten != state.ComposeChecksum.ValueString()
}

// composeWriteOnly reports whether the project tracks its compose document
// through the write-only attribute. compose_yaml_wo_version is the marker: it is
// required with it, forbidden without it, and does reach state.
func composeWriteOnly(model containerProjectResourceModel) bool {
	return !model.ComposeYAMLWOVersion.IsNull()
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

	// Write-only values live in the configuration and nowhere else: the plan
	// carries them as null.
	var config containerProjectResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	compose, ok := composeDocument(plan, config, &resp.Diagnostics)
	if !ok {
		return
	}

	tflog.Info(ctx, "Creating Container Manager project", map[string]interface{}{"name": plan.Name.ValueString(), "share_path": plan.SharePath.ValueString()})
	project, err := r.client.CreateContainerProject(ctx, plan.Name.ValueString(), plan.SharePath.ValueString(), compose, plan.Running.ValueBool())
	if err != nil {
		resp.Diagnostics.AddError("Failed to create Container Manager project", containerProjectErrorDetail(err))
		return
	}
	applyContainerProjectToResourceModel(ctx, &plan, project, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	// The checksum remembered is the one DSM reports back, not the one that was
	// sent: comparing a later refresh against the document Container Manager
	// actually stores is what keeps a normalizing DSM from looking like drift.
	// Private state is nil only when a test drives the method directly; every RPC
	// the framework serves initializes it.
	if resp.Private != nil {
		resp.Diagnostics.Append(resp.Private.SetKey(ctx, composeChecksumKey, privateChecksumValue(plan.ComposeChecksum.ValueString()))...)
	}
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

	var config containerProjectResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	compose, ok := composeDocument(plan, config, &resp.Diagnostics)
	if !ok {
		return
	}

	project, err := r.client.UpdateContainerProject(ctx, plan.ID.ValueString(), compose, plan.Running.ValueBool())
	if err != nil {
		resp.Diagnostics.AddError("Failed to update Container Manager project", containerProjectErrorDetail(err))
		return
	}
	applyContainerProjectToResourceModel(ctx, &plan, project, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Private != nil {
		resp.Diagnostics.Append(resp.Private.SetKey(ctx, composeChecksumKey, privateChecksumValue(plan.ComposeChecksum.ValueString()))...)
	}
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

// composeDocument resolves the compose document to send to DSM. The plain
// attribute comes from the plan and the write-only one from the configuration,
// which is the only place Terraform puts it.
func composeDocument(plan, config containerProjectResourceModel, diags *diag.Diagnostics) (string, bool) {
	switch {
	case knownString(config.ComposeYAMLWO):
		return config.ComposeYAMLWO.ValueString(), true
	case knownString(plan.ComposeYAML):
		return plan.ComposeYAML.ValueString(), true
	default:
		diags.AddError(
			"Missing Docker Compose configuration",
			"One of `compose_yaml` or `compose_yaml_wo` must be set. A write-only value reaches the provider only "+
				"during apply, so an ephemeral source that produced no value leaves nothing to send to Container Manager.",
		)
		return "", false
	}
}

func applyContainerProjectToResourceModel(ctx context.Context, model *containerProjectResourceModel, project *client.ContainerProject, diags *diag.Diagnostics) {
	model.ID = types.StringValue(project.ID)
	model.Name = types.StringValue(project.Name)
	model.SharePath = types.StringValue(project.SharePath)
	model.ComposeChecksum = types.StringValue(fileChecksum([]byte(project.ComposeYAML)))
	if composeWriteOnly(*model) {
		// The whole point of compose_yaml_wo: the document was read back for the
		// checksum and is dropped here rather than persisted.
		model.ComposeYAML = types.StringNull()
		model.ComposeYAMLWO = types.StringNull()
	} else {
		model.ComposeYAML = types.StringValue(project.ComposeYAML)
	}
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
