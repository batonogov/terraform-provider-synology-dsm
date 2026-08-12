package provider

import (
	"context"
	"errors"

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

func NewCertificateResource() resource.Resource {
	return &certificateResource{}
}

type certificateResource struct {
	client *client.Client
}

type certificateResourceModel struct {
	ID              types.String `tfsdk:"id"`
	Description     types.String `tfsdk:"description"`
	Certificate     types.String `tfsdk:"certificate"`
	PrivateKey      types.String `tfsdk:"private_key"`
	Intermediate    types.String `tfsdk:"intermediate"`
	SetAsDefault    types.Bool   `tfsdk:"set_as_default"`
	ForceDestroy    types.Bool   `tfsdk:"force_destroy"`
	ExpiresAt       types.String `tfsdk:"expires_at"`
	Subject         types.String `tfsdk:"subject"`
	SubjectAltNames types.List   `tfsdk:"subject_alt_names"`
	Issuer          types.String `tfsdk:"issuer"`
	IsDefault       types.Bool   `tfsdk:"is_default"`
	SelfSigned      types.Bool   `tfsdk:"self_signed"`
	Services        types.List   `tfsdk:"services"`
}

func (r *certificateResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_certificate"
}

func (r *certificateResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Imports an externally issued certificate into Synology DSM — from Vault, ACM, an internal CA, or a file on disk. " +
			"Use `dsm_certificate_lets_encrypt` instead to have DSM obtain the certificate itself.\n\n" +
			"**The private key is stored in Terraform state in clear text.** There is no way around it: Terraform has to keep the " +
			"configured value to detect drift, and DSM never returns a private key, so it cannot be re-read either. Treat the state file " +
			"as a secret — encrypted remote backend, restricted access, and rotation if it ever leaks.\n\n" +
			"Rotating a certificate is an in-place update: changing `certificate` and `private_key` reuses the same DSM certificate id, " +
			"so every service already assigned to it keeps working. Destroying a certificate that is still assigned to a service is " +
			"refused, because DSM would be left serving that service without a certificate.",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Certificate identifier assigned by DSM.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"description": schema.StringAttribute{
				Required:    true,
				Description: "Name shown in DSM under Control Panel > Security > Certificate. DSM does not require it to be unique, but a unique value makes the certificate findable.",
			},
			"certificate": schema.StringAttribute{
				Required:    true,
				Sensitive:   true,
				Description: "PEM-encoded certificate. If the file also contains the chain, the leaf must come first. " + sensitiveStateWarning,
			},
			"private_key": schema.StringAttribute{
				Required:  true,
				Sensitive: true,
				Description: "PEM-encoded private key matching `certificate`. DSM only accepts an unencrypted key — a passphrase-protected key is rejected. " +
					sensitiveStateWarning,
			},
			"intermediate": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "PEM-encoded intermediate chain, leaf-adjacent first. Omit it only for a certificate signed directly by a root the clients already trust. " + sensitiveStateWarning,
			},
			"set_as_default": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				Description: "Make this the DSM default certificate, used by every service without an explicit assignment. DSM has exactly one default; setting it here takes it away from whichever certificate held it. " +
					"Turning it back to `false` does not undo that — DSM cannot be left without a default, so another certificate has to claim it. Read `is_default` for what DSM actually reports.",
			},
			"force_destroy": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				Description: "Allow destroy to remove the certificate even while DSM services are still assigned to it. Defaults to `false`, in which case destroy fails and names the services. " +
					"Setting this to `true` accepts that those services are left without a certificate.",
			},
			"expires_at": schema.StringAttribute{
				Computed: true,
				Description: "Expiry of the certificate, RFC 3339 in UTC — the attribute to alert on. It is read from the certificate itself rather than from DSM's " +
					"reported date, so it does not depend on how a particular DSM version formats it.",
			},
			"subject": schema.StringAttribute{
				Computed:    true,
				Description: "Subject common name reported by DSM.",
			},
			"subject_alt_names": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				Description: "Subject alternative names reported by DSM.",
			},
			"issuer": schema.StringAttribute{
				Computed:    true,
				Description: "Issuer common name reported by DSM.",
			},
			"is_default": schema.BoolAttribute{
				Computed:    true,
				Description: "Whether DSM currently treats this as the default certificate.",
			},
			"self_signed": schema.BoolAttribute{
				Computed:    true,
				Description: "Whether DSM considers the certificate self-signed.",
			},
			"services": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				Description: "DSM service identifiers currently served by this certificate. A non-empty list is what makes destroy refuse unless `force_destroy` is set.",
			},
		},
	}
}

func (r *certificateResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	dsmClient := clientFromProviderData(req.ProviderData, &resp.Diagnostics)
	if dsmClient == nil {
		return
	}

	r.client = dsmClient
}

// ModifyPlan re-claims the DSM default when something took it away out of band.
func (r *certificateResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() || req.State.Raw.IsNull() {
		return
	}

	var plan, state certificateResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	planDefaultReclaim(ctx, &resp.Plan, plan.SetAsDefault, state.IsDefault, &resp.Diagnostics)
}

func (r *certificateResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan certificateResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Importing certificate into DSM", map[string]interface{}{
		"description": plan.Description.ValueString(),
	})

	certificate, err := r.client.ImportCertificate(ctx, client.ImportCertificateRequest{
		Description:  plan.Description.ValueString(),
		Certificate:  plan.Certificate.ValueString(),
		PrivateKey:   plan.PrivateKey.ValueString(),
		Intermediate: plan.Intermediate.ValueString(),
		SetAsDefault: plan.SetAsDefault.ValueBool(),
	})
	if err != nil {
		resp.Diagnostics.AddError("Failed to import certificate", certificateErrorDetail(err))
		return
	}

	resp.Diagnostics.Append(applyCertificateToModel(ctx, &plan, certificate)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *certificateResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state certificateResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	certificate, err := r.client.GetCertificate(ctx, state.ID.ValueString())
	if err != nil {
		if errors.Is(err, client.ErrCertificateNotFound) {
			// Someone removed the certificate in DSM. Recreating it is the right
			// answer, and the material to do that is in the configuration.
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Failed to read certificate", certificateErrorDetail(err))
		return
	}

	resp.Diagnostics.Append(applyCertificateToModel(ctx, &state, certificate)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *certificateResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan certificateResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var state certificateResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id := state.ID.ValueString()
	materialChanged := plan.Certificate.ValueString() != state.Certificate.ValueString() ||
		plan.PrivateKey.ValueString() != state.PrivateKey.ValueString() ||
		plan.Intermediate.ValueString() != state.Intermediate.ValueString()
	// Only ever claim the default, never surrender it: DSM keeps exactly one, and
	// there is no way to ask for none.
	claimDefault := plan.SetAsDefault.ValueBool() && !state.IsDefault.ValueBool()

	var certificate *client.Certificate
	var err error

	switch {
	case materialChanged:
		tflog.Info(ctx, "Replacing DSM certificate material in place", map[string]interface{}{"id": id})

		// Re-importing against the existing id is what makes rotation safe: DSM
		// swaps the material behind the same certificate entry, so the services
		// assigned to it keep serving without a gap. Creating a new certificate and
		// deleting the old one would drop those assignments.
		certificate, err = r.client.ImportCertificate(ctx, client.ImportCertificateRequest{
			ID:           id,
			Description:  plan.Description.ValueString(),
			Certificate:  plan.Certificate.ValueString(),
			PrivateKey:   plan.PrivateKey.ValueString(),
			Intermediate: plan.Intermediate.ValueString(),
			SetAsDefault: plan.SetAsDefault.ValueBool(),
		})
		if err != nil {
			resp.Diagnostics.AddError("Failed to replace certificate", certificateErrorDetail(err))
			return
		}
	default:
		// Nothing about the key material changed, so there is no reason to push it
		// again: an import makes DSM reload the certificate and can restart its web
		// server, which is a heavy price for renaming a certificate or handing it
		// the default flag.
		tflog.Info(ctx, "Updating DSM certificate attributes", map[string]interface{}{"id": id})

		if err := r.client.SetCertificateAttributes(ctx, id, plan.Description.ValueString(), claimDefault); err != nil {
			resp.Diagnostics.AddError("Failed to update certificate", certificateErrorDetail(err))
			return
		}
		certificate, err = r.client.GetCertificate(ctx, id)
		if err != nil {
			resp.Diagnostics.AddError("Failed to read certificate", certificateErrorDetail(err))
			return
		}
	}

	resp.Diagnostics.Append(applyCertificateToModel(ctx, &plan, certificate)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *certificateResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state certificateResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	deleteCertificate(ctx, r.client, state.ID.ValueString(), state.ForceDestroy.ValueBool(), &resp.Diagnostics)
}

func (r *certificateResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// applyCertificateToModel copies DSM's view of the certificate into the model.
// `certificate`, `private_key`, and `intermediate` are deliberately left alone:
// DSM never returns key material, so the configured value is the only record
// of it.
func applyCertificateToModel(ctx context.Context, model *certificateResourceModel, certificate *client.Certificate) (diags diag.Diagnostics) {
	model.ID = types.StringValue(certificate.ID)
	model.Description = types.StringValue(certificate.Description)
	model.Subject = types.StringValue(certificate.Subject)
	model.Issuer = types.StringValue(certificate.Issuer)
	model.IsDefault = types.BoolValue(certificate.IsDefault)
	model.SelfSigned = types.BoolValue(certificate.SelfSigned)

	altNames, altDiags := stringListValue(ctx, certificate.SubjectAltNames)
	diags.Append(altDiags...)
	model.SubjectAltNames = altNames

	services, serviceDiags := stringListValue(ctx, certificateServiceIDs(certificate))
	diags.Append(serviceDiags...)
	model.Services = services

	expiresAt, expiryDiags := certificateExpiresAt(model.Certificate.ValueString(), certificate)
	diags.Append(expiryDiags...)
	model.ExpiresAt = expiresAt

	return diags
}

// deleteCertificate is shared by both certificate resources: the guard against
// removing a certificate that is still serving something is the same regardless
// of how the certificate got there.
func deleteCertificate(ctx context.Context, dsmClient *client.Client, id string, force bool, diags *diag.Diagnostics) {
	certificate, err := dsmClient.GetCertificate(ctx, id)
	if err != nil {
		if errors.Is(err, client.ErrCertificateNotFound) {
			// Already gone; destroy has nothing to do.
			return
		}
		diags.AddError("Failed to read certificate before deleting it", certificateErrorDetail(err))
		return
	}

	// The service list is re-read here rather than taken from state on purpose:
	// an assignment made in the DSM UI after the last refresh is exactly the case
	// this guard exists for.
	if len(certificate.Services) > 0 && !force {
		summary, detail := certificateInUseError(certificate)
		diags.AddError(summary, detail)
		return
	}

	tflog.Info(ctx, "Deleting DSM certificate", map[string]interface{}{
		"id":       id,
		"forced":   force,
		"services": len(certificate.Services),
	})

	if err := dsmClient.DeleteCertificate(ctx, id); err != nil {
		diags.AddError("Failed to delete certificate", certificateErrorDetail(err))
	}
}
