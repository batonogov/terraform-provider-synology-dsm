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
	ID            types.String `tfsdk:"id"`
	SharePath     types.String `tfsdk:"share_path"`
	Name          types.String `tfsdk:"name"`
	Content       types.String `tfsdk:"content"`
	ContentBase64 types.String `tfsdk:"content_base64"`
	Checksum      types.String `tfsdk:"checksum"`
	Size          types.Int64  `tfsdk:"size"`
	posixPermissionsModel
}

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
			"root if that is to stay in the configuration.",
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
				Description: "UTF-8 file content. Conflicts with `content_base64`. The value is stored in Terraform state; " +
					"use an encrypted remote state backend when the file holds credentials.",
			},
			"content_base64": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				Description: "Base64-encoded file content, for binary files or content that is not valid UTF-8. " +
					"Conflicts with `content`.",
			},
			"checksum": schema.StringAttribute{
				Computed:    true,
				Description: "SHA-256 checksum (hex) of the file content stored on DSM. Changes when the file is edited outside Terraform.",
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

	contentSet := !config.Content.IsNull()
	base64Set := !config.ContentBase64.IsNull()
	switch {
	case contentSet && base64Set:
		resp.Diagnostics.AddError(
			"Conflicting file content",
			"Set either `content` or `content_base64`, not both. Use `content_base64` only for binary files.",
		)
	case !contentSet && !base64Set:
		resp.Diagnostics.AddError(
			"Missing file content",
			"One of `content` or `content_base64` must be set.",
		)
	}

	if base64Set && !config.ContentBase64.IsUnknown() {
		if _, err := base64.StdEncoding.DecodeString(config.ContentBase64.ValueString()); err != nil {
			resp.Diagnostics.AddAttributeError(
				tfpath.Root("content_base64"),
				"Invalid base64 content",
				fmt.Sprintf("`content_base64` must be standard base64-encoded data: %s. Use `base64encode()` or `filebase64()`.", err),
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

func (r *fileResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan fileResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	content, err := fileContentBytes(plan.Content, plan.ContentBase64)
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

	plan.ID = types.StringValue(filePath)
	plan.Checksum = types.StringValue(fileChecksum(content))
	plan.Size = types.Int64Value(int64(len(content)))
	// Read back rather than left unknown: a computed attribute must be resolved
	// by the end of apply, and the whole point of these is to show what DSM
	// actually did with the file.
	plan.apply(readPathPermissions(ctx, r.client, filePath))

	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
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
	applyFileContent(&state, content)
	state.Checksum = types.StringValue(fileChecksum(content))
	state.Size = types.Int64Value(int64(len(content)))
	state.apply(info.Permissions)

	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *fileResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan fileResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	content, err := fileContentBytes(plan.Content, plan.ContentBase64)
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

	plan.ID = types.StringValue(filePath)
	plan.Checksum = types.StringValue(fileChecksum(content))
	plan.Size = types.Int64Value(int64(len(content)))
	plan.apply(readPathPermissions(ctx, r.client, filePath))

	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
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
func fileContentBytes(content, contentBase64 types.String) ([]byte, error) {
	switch {
	case !contentBase64.IsNull() && !contentBase64.IsUnknown():
		decoded, err := base64.StdEncoding.DecodeString(contentBase64.ValueString())
		if err != nil {
			return nil, fmt.Errorf("`content_base64` is not valid base64 data: %w", err)
		}
		return decoded, nil
	case !content.IsNull() && !content.IsUnknown():
		return []byte(content.ValueString()), nil
	default:
		return nil, errors.New("one of `content` or `content_base64` must be set")
	}
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
