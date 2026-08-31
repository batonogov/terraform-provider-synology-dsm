package provider

import (
	"context"
	"fmt"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	tfpath "github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

// registryCredentialResource manages one Container Manager → Registry entry:
// a registry endpoint with credentials the Docker daemon on the NAS uses to
// pull private images.
//
// DSM exposes no update call for a registry entry (its own UI edits by
// delete-and-recreate), so every change — including a password rotation via
// password_wo_version — replaces the entry.
func NewRegistryCredentialResource() resource.Resource {
	return &registryCredentialResource{}
}

type registryCredentialResource struct {
	client *client.Client
}

type registryCredentialModel struct {
	ID                    types.String `tfsdk:"id"`
	URL                   types.String `tfsdk:"url"`
	Name                  types.String `tfsdk:"name"`
	Username              types.String `tfsdk:"username"`
	PasswordWO            types.String `tfsdk:"password_wo"`
	PasswordWOVersion     types.Int64  `tfsdk:"password_wo_version"`
	EnableTrustSelfSigned types.Bool   `tfsdk:"enable_trust_self_signed"`
}

func (r *registryCredentialResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_registry_credential"
}

func (r *registryCredentialResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a Container Manager registry entry: registry URL plus the " +
			"credentials the Docker daemon on the NAS uses to pull private images.\n\n" +
			"The password is a write-only argument and never reaches Terraform state — " +
			"DSM itself does not return it either. There is no update call in DSM's " +
			"Registry API (its own UI edits by delete-and-recreate), so any change, " +
			"including a password rotation, replaces the entry.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Registry URL, used as the entry identifier.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"url": schema.StringAttribute{
				Required:    true,
				Description: "Registry endpoint, for example `https://registry.example.com`. Identifies the entry; changing it replaces the resource.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Display name of the registry entry in Container Manager.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"username": schema.StringAttribute{
				Required:    true,
				Description: "Login for the registry. Reported back by DSM, so an edit made outside Terraform shows up as drift.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"password_wo": schema.StringAttribute{
				Required:  true,
				Sensitive: true,
				WriteOnly: true,
				Description: "Registry password. Write-only: never written to Terraform state or the plan file " +
					"(Terraform 1.11 and later). Requires `password_wo_version`. Because Terraform cannot diff a value " +
					"it does not store, a changed password is only sent to DSM when `password_wo_version` changes.",
			},
			"password_wo_version": schema.Int64Attribute{
				Required:    true,
				Description: "Version counter for `password_wo`. Increment it to rotate the password; doing so replaces the registry entry, which is also how Container Manager's UI applies an edit.",
			},
			"enable_trust_self_signed": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "Trust a self-signed certificate of the registry. Defaults to `false`.",
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.RequiresReplace(),
				},
			},
		},
	}
}

func (r *registryCredentialResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	dsmClient := clientFromProviderData(req.ProviderData, &resp.Diagnostics)
	if dsmClient == nil {
		return
	}
	r.client = dsmClient
}

func (r *registryCredentialResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan registryCredentialModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := r.client.CreateRegistry(ctx, plan.Name.ValueString(), plan.URL.ValueString(),
		plan.Username.ValueString(), plan.PasswordWO.ValueString(), plan.EnableTrustSelfSigned.ValueBool())
	if err != nil {
		resp.Diagnostics.AddError("failed to create registry credential", err.Error())
		return
	}

	plan.ID = plan.URL
	// Write-only arguments must not be echoed into state.
	plan.PasswordWO = types.StringNull()
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	tflog.Trace(ctx, "created registry credential", map[string]any{"url": plan.URL.ValueString()})
}

func (r *registryCredentialResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state registryCredentialModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Импорт передаёт только id (= URL): выводим из него остальное.
	if state.URL.IsNull() && !state.ID.IsNull() {
		state.URL = state.ID
	}
	registry, err := r.client.GetRegistryByURL(ctx, state.URL.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("failed to read registry credential", err.Error())
		return
	}
	if registry == nil {
		resp.State.RemoveResource(ctx)
		return
	}

	state.Name = types.StringValue(registry.Name)
	state.Username = types.StringValue(registry.Username)
	state.EnableTrustSelfSigned = types.BoolValue(registry.EnableTrustSSC)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *registryCredentialResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// Every attribute that can change carries RequiresReplace; the framework
	// routes updates here only when nothing replaceable moved. Persist the
	// (identical) plan values so state stays consistent.
	var plan registryCredentialModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.PasswordWO = types.StringNull()
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *registryCredentialResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state registryCredentialModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.client.DeleteRegistry(ctx, state.Name.ValueString(), state.URL.ValueString()); err != nil {
		resp.Diagnostics.AddError("failed to delete registry credential", fmt.Errorf("%w", err).Error())
		return
	}
	tflog.Trace(ctx, "deleted registry credential", map[string]any{"url": state.URL.ValueString()})
}

func (r *registryCredentialResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// The import ID is the registry URL; Read adopts the entry by it.
	resource.ImportStatePassthroughID(ctx, tfpath.Root("id"), req, resp)
}
