package provider

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	pathpkg "path"
	"strings"
	"unicode/utf8"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	tfpath "github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

func NewFileResource() resource.Resource {
	return &fileResource{}
}

type fileResource struct {
	client *client.Client
}

type fileResourceModel struct {
	ID               types.String `tfsdk:"id"`
	SharePath        types.String `tfsdk:"share_path"`
	Name             types.String `tfsdk:"name"`
	Content          types.String `tfsdk:"content"`
	ContentBase64    types.String `tfsdk:"content_base64"`
	ContentWO        types.String `tfsdk:"content_wo"`
	ContentBase64WO  types.String `tfsdk:"content_base64_wo"`
	ContentWOVersion types.Int64  `tfsdk:"content_wo_version"`
	Checksum         types.String `tfsdk:"checksum"`
	Size             types.Int64  `tfsdk:"size"`
	posixPermissionsModel
}

// fileContentChecksumKey names the private-state entry holding the checksum of
// the content this provider last wrote. In write-only mode the content is not
// in state, so it is the only thing left to compare a refreshed `checksum`
// against — which is what turns an out-of-band edit into a plan.
const fileContentChecksumKey = "content_checksum"

func (r *fileResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_file"
}

func (r *fileResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Uploads a file into a Synology shared folder through File Station. " +
			"Intended for configuration files that a container project or a service reads from disk, " +
			"so that secrets no longer have to be smuggled through Docker Compose YAML.\n\n" +
			"The POSIX mode and ownership the file lands with are reported in `posix_mode`, `posix_owner`, " +
			"`posix_uid` and friends, but cannot be set: DSM exposes no API that writes them. On a shared " +
			"folder in Synology ACL mode the mode is typically `\"000\"`, which is invisible to DSM itself " +
			"and to SMB but denies every container that bind-mounts the path and does not run as root. " +
			"Fixing it needs a `chmod` on the NAS — over SSH, or through a `dsm_scheduled_task` running as " +
			"root if that is to stay in the configuration.\n\n" +
			"**Importing a file that is going to be managed write-only writes its content to state once.** " +
			"`terraform import` is followed by a refresh, and a refresh has no access to the configuration: the only " +
			"marker for write-only mode is `content_wo_version`, which reaches state on the first apply and not before. " +
			"Treat a secret imported that way as having been in state, and rotate it — or create the file rather than " +
			"importing it.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Absolute File Station path of the file, for example `/containers/seaweedfs/conf/s3.json`.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"share_path": schema.StringAttribute{
				Required: true,
				Description: "File Station directory that receives the file, for example `/containers/seaweedfs/conf`. " +
					"This is not an absolute volume path such as `/volume1/containers/...`. Missing directories are created, " +
					"but the shared folder itself must already exist.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "File name inside `share_path`.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"content": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				Description: "UTF-8 file content. Exactly one of `content`, `content_base64`, `content_wo` or " +
					"`content_base64_wo` must be set. The value is stored in Terraform state; use `content_wo` when the " +
					"file holds credentials, or an encrypted remote state backend otherwise.",
			},
			"content_base64": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				Description: "Base64-encoded file content, for binary files or content that is not valid UTF-8. " +
					"Stored in Terraform state; the write-only counterpart is `content_base64_wo`.",
			},
			"content_wo": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				WriteOnly: true,
				Description: "UTF-8 file content that is never written to Terraform state or to the plan file " +
					"(a write-only argument, Terraform 1.11 and later). Requires `content_wo_version`. " +
					"Because Terraform cannot diff a value it does not store, an edit to the configured content is " +
					"only sent to DSM when `content_wo_version` changes — but an edit made *outside* Terraform is " +
					"still detected, because `checksum` is compared against the checksum of the last write.\n\n" +
					"Note what that repair writes: the value the configuration holds *now*. A value edited but " +
					"deliberately left un-versioned is not staged anywhere the provider can reach, so an out-of-band " +
					"edit repaired later publishes it.",
			},
			"content_base64_wo": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				WriteOnly: true,
				Description: "Base64-encoded file content that is never written to Terraform state, for binary secrets. " +
					"The write-only counterpart of `content_base64`; requires `content_wo_version`.",
			},
			"content_wo_version": schema.Int64Attribute{
				Optional: true,
				Description: "Version counter for `content_wo` / `content_base64_wo`. Required with either of them and " +
					"rejected without them. Increment it to re-send the content to DSM. It is also the marker that keeps " +
					"the file content out of state on refresh: a resource imported before `content_wo` was adopted holds " +
					"its content in state until the first apply that sets both attributes.",
			},
			"checksum": schema.StringAttribute{
				Computed: true,
				Description: "SHA-256 checksum (hex) of the file content stored on DSM. Changes when the file is edited outside Terraform, " +
					"which is the drift signal `content_wo` relies on.\n\n" +
					"It stays in state even when the content does not. That is harmless for a configuration file, but a file holding " +
					"nothing but a short secret is a value an attacker with access to state could search for offline — write such a " +
					"secret through a resource whose state is itself protected, or accept the exposure knowingly.",
			},
			"size": schema.Int64Attribute{
				Computed:    true,
				Description: "Size of the file on DSM, in bytes.",
			},
		},
	}
	maps.Copy(resp.Schema.Attributes, posixPermissionAttributes())
}

func (r *fileResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config fileResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	set := 0
	for _, attribute := range []types.String{config.Content, config.ContentBase64, config.ContentWO, config.ContentBase64WO} {
		if !attribute.IsNull() {
			set++
		}
	}
	switch {
	case set > 1:
		resp.Diagnostics.AddError(
			"Conflicting file content",
			"Set exactly one of `content`, `content_base64`, `content_wo` or `content_base64_wo`. "+
				"The `_base64` forms are for binary files; the `_wo` forms keep the content out of Terraform state.",
		)
	case set == 0:
		resp.Diagnostics.AddError(
			"Missing file content",
			"One of `content`, `content_base64`, `content_wo` or `content_base64_wo` must be set.",
		)
	}

	// The version companion is what makes a write-only value usable: Terraform
	// cannot diff a value it never stored, so the counter is the only way to ask
	// for a rewrite — and its presence in state is what tells Read to keep the
	// content out of state.
	writeOnlySet := !config.ContentWO.IsNull() || !config.ContentBase64WO.IsNull()
	versionSet := !config.ContentWOVersion.IsNull()
	switch {
	case writeOnlySet && !versionSet:
		resp.Diagnostics.AddAttributeError(
			tfpath.Root("content_wo_version"),
			"Missing content_wo_version",
			"`content_wo_version` is required with `content_wo` or `content_base64_wo`. Start at `1` and increment it whenever the content should be written to DSM again.",
		)
	case !writeOnlySet && versionSet:
		resp.Diagnostics.AddAttributeError(
			tfpath.Root("content_wo_version"),
			"Unused content_wo_version",
			"`content_wo_version` only applies to `content_wo` and `content_base64_wo`. Remove it, or move the content to the write-only attribute.",
		)
	}

	for _, encoded := range []struct {
		attribute string
		value     types.String
	}{
		{"content_base64", config.ContentBase64},
		{"content_base64_wo", config.ContentBase64WO},
	} {
		if encoded.value.IsNull() || encoded.value.IsUnknown() {
			continue
		}
		if _, err := base64.StdEncoding.DecodeString(encoded.value.ValueString()); err != nil {
			resp.Diagnostics.AddAttributeError(
				tfpath.Root(encoded.attribute),
				"Invalid base64 content",
				fmt.Sprintf("`%s` must be standard base64-encoded data: %s. Use `base64encode()` or `filebase64()`.", encoded.attribute, err),
			)
		}
	}

	if !config.SharePath.IsNull() && !config.SharePath.IsUnknown() {
		sharePath := config.SharePath.ValueString()
		clean := pathpkg.Clean(sharePath)
		if !strings.HasPrefix(sharePath, "/") || clean != sharePath || clean == "/" {
			resp.Diagnostics.AddAttributeError(
				tfpath.Root("share_path"),
				"Invalid File Station directory",
				"Use a normalized absolute File Station directory such as `/containers/seaweedfs/conf`, without a trailing slash.",
			)
		}
		if dsmVolumePath.MatchString(clean) {
			resp.Diagnostics.AddAttributeError(
				tfpath.Root("share_path"),
				"Volume path is not a File Station path",
				"Use `/containers/seaweedfs/conf`, not `/volume1/containers/seaweedfs/conf`.",
			)
		}
	}

	if !config.Name.IsNull() && !config.Name.IsUnknown() {
		name := config.Name.ValueString()
		if name == "" || strings.TrimSpace(name) != name || strings.Contains(name, "/") || name == "." || name == ".." {
			resp.Diagnostics.AddAttributeError(
				tfpath.Root("name"),
				"Invalid file name",
				"`name` must be a single file name: non-empty, without `/`, and without leading or trailing whitespace. Put directories in `share_path`.",
			)
		}
	}
}

func (r *fileResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	dsmClient := clientFromProviderData(req.ProviderData, &resp.Diagnostics)
	if dsmClient == nil {
		return
	}

	r.client = dsmClient
}

// ModifyPlan resolves the two computed attributes an apply rewrites, and is
// what lets a write-only file heal after an edit made outside Terraform.
//
// Terraform carries computed attributes forward from prior state when it builds
// a plan, so a file whose content is changing would otherwise plan the old
// `checksum` and be handed a different one after apply — which Terraform
// reports as "Provider produced inconsistent result after apply". Marking them
// unknown also produces the diff that write-only mode has no other way to
// express: with the content absent from state, the version counter and a
// checksum that no longer matches the last write are the only signals left.
func (r *fileResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	// Create has no prior state to compare against, destroy has no plan.
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}

	var state, plan fileResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	lastWritten := lastChecksum(ctx, req.Private, fileContentChecksumKey, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	if !fileContentWillChange(state, plan, lastWritten) {
		return
	}

	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, tfpath.Root("checksum"), types.StringUnknown())...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, tfpath.Root("size"), types.Int64Unknown())...)
	// Update re-reads the permissions from File Station, so they are planned as
	// unknown for the same reason: a value carried forward from prior state that
	// apply then contradicts is an error, not a warning.
	for _, attribute := range []struct {
		name  string
		value attr.Value
	}{
		{"posix_mode", types.StringUnknown()},
		{"posix_owner", types.StringUnknown()},
		{"posix_group", types.StringUnknown()},
		{"posix_uid", types.Int64Unknown()},
		{"posix_gid", types.Int64Unknown()},
		{"acl_mode", types.BoolUnknown()},
	} {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, tfpath.Root(attribute.name), attribute.value)...)
	}
}

// fileContentWillChange reports whether an apply is going to write the file
// again.
//
// With the content in state, Terraform's own diff answers the question. In
// write-only mode it is not in state, which leaves two signals: the version
// counter the practitioner controls, and a checksum on DSM that no longer
// matches what this provider last wrote — an edit made outside Terraform, which
// an apply repairs by writing the configured content back.
func fileContentWillChange(state, plan fileResourceModel, lastWritten string) bool {
	switch {
	case !plan.Content.Equal(state.Content), !plan.ContentBase64.Equal(state.ContentBase64):
		return true
	case !plan.ContentWOVersion.Equal(state.ContentWOVersion):
		return true
	case !fileWriteOnly(plan):
		return false
	}
	// Nothing to compare against before the first write-only apply, and nothing
	// to repair while the remote checksum still matches it.
	return lastWritten != "" && !state.Checksum.IsNull() && lastWritten != state.Checksum.ValueString()
}

// fileWriteOnly reports whether the file tracks its content through the
// write-only attributes. content_wo_version is the marker: it is required with
// them, forbidden without them, and — unlike the content itself — reaches state.
func fileWriteOnly(model fileResourceModel) bool {
	return !model.ContentWOVersion.IsNull()
}

// clearFileWriteOnly keeps the write-only values out of state. The framework
// nullifies them before the response leaves the provider anyway; doing it here
// as well means the guarantee is visible at the place that writes state.
func clearFileWriteOnly(model *fileResourceModel) {
	model.ContentWO = types.StringNull()
	model.ContentBase64WO = types.StringNull()
}

func (r *fileResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan fileResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Write-only values live in the configuration and nowhere else: the plan
	// carries them as null.
	var config fileResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	content, err := fileContentBytes(plan.Content, plan.ContentBase64, config.ContentWO, config.ContentBase64WO)
	if err != nil {
		resp.Diagnostics.AddError("Invalid file content", err.Error())
		return
	}

	filePath := client.FilePath(plan.SharePath.ValueString(), plan.Name.ValueString())
	tflog.Info(ctx, "Uploading DSM file", map[string]interface{}{"path": filePath, "bytes": len(content)})

	if err := r.client.UploadFile(ctx, plan.SharePath.ValueString(), plan.Name.ValueString(), content); err != nil {
		resp.Diagnostics.AddError("Failed to upload file", fileErrorDetail(err))
		return
	}

	// The checksum recorded is of the bytes sent, unlike dsm_container_project,
	// which records the document DSM reports back: File Station stores a file
	// verbatim, while a compose document passes through Container Manager and
	// can come back normalized. If a DSM build ever turns out not to be
	// byte-exact, this is where a permanent false drift would come from.
	checksum := fileChecksum(content)
	plan.ID = types.StringValue(filePath)
	plan.Checksum = types.StringValue(checksum)
	plan.Size = types.Int64Value(int64(len(content)))
	// Read back rather than left unknown: a computed attribute must be resolved
	// by the end of apply, and the whole point of these is to show what DSM
	// actually did with the file.
	plan.apply(readPathPermissions(ctx, r.client, filePath))
	clearFileWriteOnly(&plan)

	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
	rememberChecksum(ctx, resp.Private, fileContentChecksumKey, checksum, &resp.Diagnostics)
}

func (r *fileResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state fileResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	filePath := state.ID.ValueString()
	if filePath == "" {
		filePath = client.FilePath(state.SharePath.ValueString(), state.Name.ValueString())
	}

	tflog.Debug(ctx, "Reading DSM file", map[string]interface{}{"path": filePath})

	info, err := r.client.GetFileInfo(ctx, filePath)
	if errors.Is(err, client.ErrFileNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read file", fileErrorDetail(err))
		return
	}
	if info.IsDir {
		resp.Diagnostics.AddError(
			"Path is a directory",
			fmt.Sprintf("%q is a directory on DSM, not a file. Point `share_path`/`name` at a file, or remove the directory.", filePath),
		)
		return
	}

	// The content is read back rather than trusted from state: that is what
	// turns an out-of-band edit into a plan instead of silent divergence.
	content, err := r.client.DownloadFile(ctx, filePath)
	if errors.Is(err, client.ErrFileNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read file content", fileErrorDetail(err))
		return
	}

	// Every attribute is set from the remote file, including share_path and
	// name, which an imported resource does not have in state yet.
	dir, name := pathpkg.Split(info.Path)
	state.ID = types.StringValue(info.Path)
	state.SharePath = types.StringValue(strings.TrimSuffix(dir, "/"))
	state.Name = types.StringValue(name)
	if fileWriteOnly(state) {
		// The whole point of the write-only attributes: the bytes were read back
		// for the checksum and are dropped here rather than persisted.
		state.Content = types.StringNull()
		state.ContentBase64 = types.StringNull()
	} else {
		applyFileContent(&state, content)
	}
	checksum := fileChecksum(content)
	state.Checksum = types.StringValue(checksum)
	state.Size = types.Int64Value(int64(len(content)))
	state.apply(info.Permissions)

	// A resource this provider never wrote — imported, re-imported after a
	// `state rm`, or carried over from a version without private state — has no
	// reference point, and without one an out-of-band edit would go unnoticed
	// forever. Adopt what DSM currently holds as the baseline, once: writing it
	// on every refresh would move the goalposts and detect nothing.
	if lastChecksum(ctx, req.Private, fileContentChecksumKey, &resp.Diagnostics) == "" {
		rememberChecksum(ctx, resp.Private, fileContentChecksumKey, checksum, &resp.Diagnostics)
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *fileResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan fileResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var config fileResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	content, err := fileContentBytes(plan.Content, plan.ContentBase64, config.ContentWO, config.ContentBase64WO)
	if err != nil {
		resp.Diagnostics.AddError("Invalid file content", err.Error())
		return
	}

	filePath := client.FilePath(plan.SharePath.ValueString(), plan.Name.ValueString())
	tflog.Info(ctx, "Updating DSM file", map[string]interface{}{"path": filePath, "bytes": len(content)})

	if err := r.client.UploadFile(ctx, plan.SharePath.ValueString(), plan.Name.ValueString(), content); err != nil {
		resp.Diagnostics.AddError("Failed to upload file", fileErrorDetail(err))
		return
	}

	checksum := fileChecksum(content)
	plan.ID = types.StringValue(filePath)
	plan.Checksum = types.StringValue(checksum)
	plan.Size = types.Int64Value(int64(len(content)))
	plan.apply(readPathPermissions(ctx, r.client, filePath))
	clearFileWriteOnly(&plan)

	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
	rememberChecksum(ctx, resp.Private, fileContentChecksumKey, checksum, &resp.Diagnostics)
}

func (r *fileResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state fileResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	filePath := state.ID.ValueString()
	if filePath == "" {
		filePath = client.FilePath(state.SharePath.ValueString(), state.Name.ValueString())
	}

	tflog.Info(ctx, "Deleting DSM file", map[string]interface{}{"path": filePath})

	// A file somebody else already removed is not a failure to destroy.
	if err := r.client.DeleteFile(ctx, filePath); err != nil && !errors.Is(err, client.ErrFileNotFound) {
		resp.Diagnostics.AddError("Failed to delete file", fileErrorDetail(err))
	}
}

func (r *fileResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// The import ID is the absolute File Station path; Read derives share_path,
	// name, and the content form from it.
	resource.ImportStatePassthroughID(ctx, tfpath.Root("id"), req, resp)
}

// fileContentBytes resolves the configured content into the bytes to upload.
// The plain attributes come from the plan and the write-only ones from the
// configuration, which is the only place Terraform puts them.
func fileContentBytes(content, contentBase64, contentWO, contentBase64WO types.String) ([]byte, error) {
	decodeBase64 := func(attribute string, value types.String) ([]byte, error) {
		decoded, err := base64.StdEncoding.DecodeString(value.ValueString())
		if err != nil {
			return nil, fmt.Errorf("`%s` is not valid base64 data: %w", attribute, err)
		}
		return decoded, nil
	}

	switch {
	case knownString(contentBase64WO):
		return decodeBase64("content_base64_wo", contentBase64WO)
	case knownString(contentWO):
		return []byte(contentWO.ValueString()), nil
	case knownString(contentBase64):
		return decodeBase64("content_base64", contentBase64)
	case knownString(content):
		return []byte(content.ValueString()), nil
	default:
		return nil, errors.New("one of `content`, `content_base64`, `content_wo` or `content_base64_wo` must be set")
	}
}

func knownString(value types.String) bool {
	return !value.IsNull() && !value.IsUnknown()
}

// applyFileContent writes the bytes read from DSM back into the model in the
// same form the practitioner configured, so a matching file produces no diff.
//
// Binary content can only be represented as base64: Terraform strings must be
// valid UTF-8. When a file tracked through `content` turns out not to be valid
// UTF-8 any more, the model falls back to `content_base64` — which shows up as
// a diff on `content` and re-uploads the configured value.
func applyFileContent(model *fileResourceModel, content []byte) {
	if !model.ContentBase64.IsNull() || !utf8.Valid(content) {
		model.ContentBase64 = types.StringValue(base64.StdEncoding.EncodeToString(content))
		model.Content = types.StringNull()
		return
	}
	model.Content = types.StringValue(string(content))
	model.ContentBase64 = types.StringNull()
}

func fileChecksum(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// fileErrorDetail turns a bare File Station code into something actionable.
// The meanings come from Synology's File Station API guide; they are kept here
// rather than in the client's error table, which is reserved for codes verified
// against real DSM hardware.
func fileErrorDetail(err error) string {
	message := err.Error()
	switch {
	case errors.Is(err, client.ErrFileNotFound):
		return message + "\n\nVerify the shared folder and directory exist in File Station."
	case client.IsAPIError(err, 102, 103, 104):
		return message + "\n\nThe File Station API is unavailable. Install and start the `FileStation` package on the NAS."
	case client.IsAPIError(err, 105, 407):
		return message + "\n\nDSM denied the file operation (code 105/407). Use an administrator account and check the shared folder permissions."
	case client.IsAPIError(err, 408):
		return message + "\n\nDSM reports the path does not exist (code 408). The shared folder in `share_path` must already exist — File Station creates missing subdirectories, but not the shared folder itself; declare a `dsm_shared_folder` resource for it."
	case client.IsAPIError(err, 414):
		return message + "\n\nDSM reports the destination file already exists (code 414). Import it with `terraform import` instead of creating it again."
	case client.IsAPIError(err, 415, 416):
		return message + "\n\nDSM could not store the file (code 415/416): the volume is out of space or the share quota is exhausted."
	case client.IsAPIError(err, 418, 419):
		return message + "\n\nDSM rejected the file name (code 418/419). Avoid characters DSM reserves, such as `\\ : ? \" < > |`."
	default:
		return message
	}
}
