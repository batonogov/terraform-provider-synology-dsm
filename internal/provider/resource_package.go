package provider

import (
	"context"
	"errors"
	"strings"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

func NewPackageResource() resource.Resource {
	return &packageResource{}
}

type packageResource struct {
	client *client.Client
}

type packageResourceModel struct {
	ID                 types.String `tfsdk:"id"`
	Name               types.String `tfsdk:"name"`
	Volume             types.String `tfsdk:"volume"`
	Running            types.Bool   `tfsdk:"running"`
	UninstallOnDestroy types.Bool   `tfsdk:"uninstall_on_destroy"`
	DisplayName        types.String `tfsdk:"display_name"`
	Version            types.String `tfsdk:"version"`
	Status             types.String `tfsdk:"status"`
	Description        types.String `tfsdk:"description"`
	Maintainer         types.String `tfsdk:"maintainer"`
	CanUninstall       types.Bool   `tfsdk:"can_uninstall"`
}

func (r *packageResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_package"
}

func (r *packageResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Installs and controls a package from Synology DSM Package Center. Existing packages are adopted automatically. " +
			"Package removal is disabled by default because uninstalling an application can also remove its configuration and data.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Package identifier used by DSM Package Center.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Package Center identifier, for example `ContainerManager` or `FileStation`.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"volume": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString("/volume1"),
				Description: "Volume path used for a new installation, for example `/volume1`. DSM does not expose the installed volume through this API.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"running": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(true),
				Description: "Whether the package should be running. Changes start or stop the installed package in place.",
			},
			"uninstall_on_destroy": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				Description: "Permanently uninstall the package when this resource is destroyed. Defaults to `false`; destroy then only " +
					"removes the package from Terraform state. Enable with care because DSM packages may remove configuration or application data.",
			},
			"display_name": schema.StringAttribute{
				Computed:    true,
				Description: "Human-readable package name reported by DSM.",
			},
			"version": schema.StringAttribute{
				Computed:    true,
				Description: "Installed package version.",
			},
			"status": schema.StringAttribute{
				Computed:    true,
				Description: "Raw lifecycle status reported by DSM, such as `running`, `stop`, or `broken`.",
			},
			"description": schema.StringAttribute{
				Computed:    true,
				Description: "Package description reported by DSM.",
			},
			"maintainer": schema.StringAttribute{
				Computed:    true,
				Description: "Package maintainer reported by DSM.",
			},
			"can_uninstall": schema.BoolAttribute{
				Computed:    true,
				Description: "Whether DSM allows this package to be uninstalled. System packages usually report `false`.",
			},
		},
	}
}

func (r *packageResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config packageResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() || config.Volume.IsNull() || config.Volume.IsUnknown() {
		return
	}
	if !strings.HasPrefix(config.Volume.ValueString(), "/volume") {
		resp.Diagnostics.AddAttributeError(
			path.Root("volume"),
			"Invalid DSM volume path",
			"The package installation volume must be a DSM volume path such as `/volume1`.",
		)
	}
}

func (r *packageResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	dsmClient := clientFromProviderData(req.ProviderData, &resp.Diagnostics)
	if dsmClient == nil {
		return
	}
	r.client = dsmClient
}

func (r *packageResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan packageResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id := plan.Name.ValueString()
	tflog.Info(ctx, "Ensuring DSM package is installed", map[string]interface{}{"name": id})

	pkg, err := r.client.GetPackage(ctx, id)
	if errors.Is(err, client.ErrPackageNotFound) {
		pkg, err = r.client.InstallPackage(ctx, id, plan.Volume.ValueString(), plan.Running.ValueBool())
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to install DSM package", packageErrorDetail(err))
		return
	}

	if pkg.Running() != plan.Running.ValueBool() {
		pkg, err = r.client.SetPackageRunning(ctx, id, plan.Running.ValueBool())
		if err != nil {
			resp.Diagnostics.AddError("Failed to set DSM package state", packageErrorDetail(err))
			return
		}
	}

	applyPackageToResourceModel(&plan, pkg)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *packageResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state packageResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id := state.ID.ValueString()
	if id == "" {
		id = state.Name.ValueString()
	}
	pkg, err := r.client.GetPackage(ctx, id)
	if errors.Is(err, client.ErrPackageNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read DSM package", packageErrorDetail(err))
		return
	}

	if state.Volume.IsNull() || state.Volume.IsUnknown() {
		state.Volume = types.StringValue("/volume1")
	}
	if state.UninstallOnDestroy.IsNull() || state.UninstallOnDestroy.IsUnknown() {
		state.UninstallOnDestroy = types.BoolValue(false)
	}
	applyPackageToResourceModel(&state, pkg)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *packageResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan packageResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var state packageResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var pkg *client.Package
	var err error
	if state.Running.ValueBool() != plan.Running.ValueBool() {
		pkg, err = r.client.SetPackageRunning(ctx, plan.Name.ValueString(), plan.Running.ValueBool())
	} else {
		// A provider-only flag such as uninstall_on_destroy may be the only
		// changed attribute; avoid needlessly restarting/stopping the package.
		pkg, err = r.client.GetPackage(ctx, plan.Name.ValueString())
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to update DSM package", packageErrorDetail(err))
		return
	}
	applyPackageToResourceModel(&plan, pkg)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *packageResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state packageResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !state.UninstallOnDestroy.ValueBool() {
		tflog.Warn(ctx, "Leaving DSM package installed because uninstall_on_destroy is false", map[string]interface{}{
			"name": state.Name.ValueString(),
		})
		return
	}

	tflog.Warn(ctx, "Uninstalling DSM package", map[string]interface{}{"name": state.Name.ValueString()})
	if err := r.client.UninstallPackage(ctx, state.Name.ValueString()); err != nil {
		resp.Diagnostics.AddError("Failed to uninstall DSM package", packageErrorDetail(err))
	}
}

func (r *packageResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func applyPackageToResourceModel(model *packageResourceModel, pkg *client.Package) {
	model.ID = types.StringValue(pkg.ID)
	model.Name = types.StringValue(pkg.ID)
	model.DisplayName = types.StringValue(pkg.Name)
	model.Version = types.StringValue(pkg.Version)
	model.Status = types.StringValue(pkg.Status)
	model.Description = types.StringValue(pkg.Description)
	model.Maintainer = types.StringValue(pkg.Maintainer)
	model.Running = types.BoolValue(pkg.Running())
	model.CanUninstall = types.BoolValue(pkg.CanUninstall)
}

func packageErrorDetail(err error) string {
	message := err.Error()
	switch {
	case errors.Is(err, client.ErrPackageNotFound):
		return message + "\n\nVerify the exact Package Center identifier and that the package supports this NAS model and DSM version."
	case client.IsAPIError(err, 103):
		return message + "\n\nThe Package Center method is unavailable. Virtual DSM blocks package installation; " +
			"on physical hardware, also verify that the package is compatible with the NAS model."
	case client.IsAPIError(err, 105):
		return message + "\n\nThe provider account is not allowed to control packages. Use an administrator account."
	default:
		return message
	}
}
