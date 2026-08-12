package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

func NewCertificateLetsEncryptResource() resource.Resource {
	return &certificateLetsEncryptResource{}
}

type certificateLetsEncryptResource struct {
	client *client.Client
}

type certificateLetsEncryptResourceModel struct {
	ID              types.String `tfsdk:"id"`
	Description     types.String `tfsdk:"description"`
	Domain          types.String `tfsdk:"domain"`
	AltNames        types.Set    `tfsdk:"alt_names"`
	Email           types.String `tfsdk:"email"`
	SetAsDefault    types.Bool   `tfsdk:"set_as_default"`
	ForceDestroy    types.Bool   `tfsdk:"force_destroy"`
	ExpiresAt       types.String `tfsdk:"expires_at"`
	Subject         types.String `tfsdk:"subject"`
	SubjectAltNames types.List   `tfsdk:"subject_alt_names"`
	Issuer          types.String `tfsdk:"issuer"`
	IsDefault       types.Bool   `tfsdk:"is_default"`
	Renewable       types.Bool   `tfsdk:"renewable"`
	Services        types.List   `tfsdk:"services"`
}

func (r *certificateLetsEncryptResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_certificate_lets_encrypt"
}

func (r *certificateLetsEncryptResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Has DSM obtain a certificate from Let's Encrypt over ACME. Use `dsm_certificate` instead to import a certificate " +
			"issued elsewhere.\n\n" +
			"**Issuance depends on the outside world.** Let's Encrypt validates every name from the public internet, so each one has to " +
			"resolve to this NAS and inbound TCP/80 has to reach it. DSM runs the whole ACME exchange inside a single request and answers " +
			"only when it is finished, so `apply` blocks for tens of seconds and occasionally minutes. Because issuance consumes a " +
			"rate-limited external service, prefer `terraform apply -target` while working the configuration out, and read the diagnostic " +
			"carefully on failure — it lists the conditions that have to hold.\n\n" +
			"DSM hardcodes the Let's Encrypt production directory, so there is no staging option and no way to point this at another ACME " +
			"provider. DSM also renews on its own schedule, so no key material ever reaches Terraform state — unlike `dsm_certificate`, " +
			"this resource stores no secrets. Destroying a certificate that is still assigned to a service is refused.",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Certificate identifier assigned by DSM.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"description": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Name shown in DSM under Control Panel > Security > Certificate. Defaults to `domain`.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"domain": schema.StringAttribute{
				Required:    true,
				Description: "Primary domain to certify. It becomes the certificate's common name. Changing it requires a new certificate.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			// A set rather than a list: the order of subject alternative names in a
			// certificate carries no meaning, and modelling it as a list would turn
			// a reordering — or DSM reporting them in its own order after an import
			// — into a forced reissue.
			"alt_names": schema.SetAttribute{
				Optional:    true,
				Computed:    true,
				ElementType: types.StringType,
				Description: "Additional subject alternative names. Every one of them is validated independently, so every one must resolve to this NAS. Changing the set requires a new certificate.",
				PlanModifiers: []planmodifier.Set{
					// A certificate's SAN set is fixed at issuance; changing it means
					// asking the CA for a different certificate. Modelling that as an
					// in-place update would hide a full reissue behind an innocent diff.
					setplanmodifier.RequiresReplace(),
				},
			},
			"email": schema.StringAttribute{
				Required: true,
				Description: "Contact address registered with the ACME account. Let's Encrypt uses it for expiry and policy notices.\n\n" +
					"DSM does not expose this value, so it cannot be read back: after `terraform import` it is whatever the configuration says. " +
					"Changing it does not force a new certificate — reissuing to correct a contact address would spend a Let's Encrypt rate-limit " +
					"slot for no benefit — so the new value is recorded in state and takes effect at the next issuance.",
				// Deliberately not RequiresReplace. An attribute that can never be
				// read back must not be able to trigger a destroy: after an import it
				// is null in state, and a forced replacement there would destroy a
				// working certificate and burn a rate-limit slot to recreate it.
			},
			"set_as_default": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "Make this the DSM default certificate, used by every service without an explicit assignment.",
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
				Description: "Expiry of the certificate, RFC 3339 in UTC — the attribute to alert on. Let's Encrypt issues for 90 days and DSM renews automatically, " +
					"so a value that stops moving forward is the signal that renewal has stopped working. Unlike `dsm_certificate`, this is read from the date DSM reports: " +
					"the certificate never leaves the NAS, so there is no PEM here to parse.",
			},
			"subject": schema.StringAttribute{
				Computed:    true,
				Description: "Subject common name reported by DSM.",
			},
			"subject_alt_names": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				Description: "Subject alternative names present in the issued certificate, as reported by DSM.",
			},
			"issuer": schema.StringAttribute{
				Computed:    true,
				Description: "Issuer common name reported by DSM.",
			},
			"is_default": schema.BoolAttribute{
				Computed:    true,
				Description: "Whether DSM currently treats this as the default certificate.",
			},
			"renewable": schema.BoolAttribute{
				Computed:    true,
				Description: "Whether DSM considers the certificate renewable. A Let's Encrypt certificate that reports `false` will not be renewed automatically.",
			},
			"services": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				Description: "DSM service identifiers currently served by this certificate. A non-empty list is what makes destroy refuse unless `force_destroy` is set.",
			},
		},
	}
}

func (r *certificateLetsEncryptResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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
func (r *certificateLetsEncryptResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() || req.State.Raw.IsNull() {
		return
	}

	var plan, state certificateLetsEncryptResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	planDefaultReclaim(ctx, &resp.Plan, plan.SetAsDefault, state.IsDefault, &resp.Diagnostics)
}

func (r *certificateLetsEncryptResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan certificateLetsEncryptResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	altNames, diags := stringSliceFromSet(ctx, plan.AltNames)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	description := plan.Description.ValueString()
	if description == "" {
		description = plan.Domain.ValueString()
	}

	tflog.Info(ctx, "Requesting a Let's Encrypt certificate from DSM", map[string]interface{}{
		"domain":    plan.Domain.ValueString(),
		"alt_names": altNames,
	})

	certificate, err := r.client.CreateLetsEncryptCertificate(ctx, client.LetsEncryptRequest{
		Domain:      plan.Domain.ValueString(),
		AltNames:    altNames,
		Email:       plan.Email.ValueString(),
		Description: description,
	})
	if err != nil {
		resp.Diagnostics.AddError(
			"Failed to obtain a Let's Encrypt certificate",
			letsEncryptErrorDetail(err, plan.Domain.ValueString(), altNames),
		)
		return
	}

	// Claiming the default is a separate call rather than a flag on create: the
	// create method is not known to accept one, and getting that wrong would
	// silently leave the wrong certificate serving DSM. Doing it afterwards is
	// also what DSM's own control panel does.
	if plan.SetAsDefault.ValueBool() && !certificate.IsDefault {
		if err := r.client.SetDefaultCertificate(ctx, certificate.ID, certificate.Description); err != nil {
			resp.Diagnostics.AddError("Failed to set the certificate as default", certificateErrorDetail(err))
			return
		}
		if refreshed, refreshErr := r.client.GetCertificate(ctx, certificate.ID); refreshErr == nil {
			certificate = refreshed
		}
	}

	resp.Diagnostics.Append(applyLetsEncryptToModel(ctx, &plan, certificate)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *certificateLetsEncryptResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state certificateLetsEncryptResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	certificate, err := r.client.GetCertificate(ctx, state.ID.ValueString())
	if errors.Is(err, client.ErrCertificateNotFound) {
		// DSM renews these certificates on its own, and a renewal can replace the
		// certificate object rather than update it in place — at which point the id
		// in state points at nothing. Dropping the resource here would make
		// Terraform ask Let's Encrypt for another certificate, which is both
		// unnecessary and rate-limited, so the common name is tried first and the
		// new id adopted if DSM still serves the domain.
		certificate, err = r.client.GetCertificateBySubject(ctx, state.Domain.ValueString())
		if err == nil {
			tflog.Info(ctx, "Adopting a DSM-renewed Let's Encrypt certificate under its new id", map[string]interface{}{
				"domain": state.Domain.ValueString(),
				"old_id": state.ID.ValueString(),
				"new_id": certificate.ID,
			})
		}
	}
	if err != nil {
		if errors.Is(err, client.ErrCertificateNotFound) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Failed to read certificate", certificateErrorDetail(err))
		return
	}

	resp.Diagnostics.Append(applyLetsEncryptToModel(ctx, &state, certificate)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

// Update covers only the attributes DSM can change without a reissue: the
// description and which certificate is the default. Everything that is baked
// into the certificate itself is marked RequiresReplace in the schema.
func (r *certificateLetsEncryptResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan certificateLetsEncryptResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var state certificateLetsEncryptResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id := state.ID.ValueString()

	// The contact address lives in the ACME account, not in anything DSM exposes
	// or lets us change. Saying nothing would leave the operator believing the
	// change took effect; forcing a reissue to apply it would cost a rate-limit
	// slot. So it is recorded and explained.
	if !state.Email.IsNull() && plan.Email.ValueString() != state.Email.ValueString() {
		resp.Diagnostics.AddWarning(
			"The ACME contact address was not sent to DSM",
			fmt.Sprintf("`email` changed from %q to %q. DSM exposes no way to update the contact address of an already-issued "+
				"certificate, so the new value is recorded in Terraform state and will be used the next time this certificate is "+
				"issued — after a `terraform destroy`/`apply` cycle, or if you change `domain` or `alt_names`.",
				state.Email.ValueString(), plan.Email.ValueString()),
		)
	}

	description := plan.Description.ValueString()
	if description == "" {
		description = plan.Domain.ValueString()
	}
	// Only ever set the default, never clear it: DSM always has exactly one
	// default certificate, and dropping the flag here would leave it with none.
	// Handing the default to a different certificate is that other resource's
	// job.
	claimDefault := plan.SetAsDefault.ValueBool() && !state.IsDefault.ValueBool()

	if description != state.Description.ValueString() || claimDefault {
		if err := r.client.SetCertificateAttributes(ctx, id, description, claimDefault); err != nil {
			resp.Diagnostics.AddError("Failed to update certificate", certificateErrorDetail(err))
			return
		}
	}

	certificate, err := r.client.GetCertificate(ctx, id)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read certificate", certificateErrorDetail(err))
		return
	}

	resp.Diagnostics.Append(applyLetsEncryptToModel(ctx, &plan, certificate)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *certificateLetsEncryptResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state certificateLetsEncryptResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	deleteCertificate(ctx, r.client, state.ID.ValueString(), state.ForceDestroy.ValueBool(), &resp.Diagnostics)
}

func (r *certificateLetsEncryptResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// applyLetsEncryptToModel copies DSM's view of the certificate into the model.
//
// It has to fill in `domain` and `alt_names` as well as the computed
// attributes, even though they are configuration rather than output. Both are
// RequiresReplace, and after `terraform import` state holds nothing but the id:
// leaving them null makes the very next plan read `domain = null ->
// "cloud.example.com" # forces replacement` and destroy a perfectly good
// certificate to reissue it — spending a Let's Encrypt rate-limit slot for
// nothing, and getting stuck outright if the certificate is assigned to a
// service, because the destroy guard will refuse. `email` is the exception: DSM
// does not report it, so state keeps whatever the configuration last said.
func applyLetsEncryptToModel(ctx context.Context, model *certificateLetsEncryptResourceModel, certificate *client.Certificate) (diags diag.Diagnostics) {
	model.ID = types.StringValue(certificate.ID)
	model.Description = types.StringValue(certificate.Description)
	model.Subject = types.StringValue(certificate.Subject)
	model.Issuer = types.StringValue(certificate.Issuer)
	model.IsDefault = types.BoolValue(certificate.IsDefault)
	model.Renewable = types.BoolValue(certificate.Renewable)

	// The common name is the domain that was requested, so it reconstructs
	// `domain` exactly. If DSM reported no subject at all, the configured value
	// is kept rather than blanked — a null here would force a replacement.
	if certificate.Subject != "" {
		model.Domain = types.StringValue(certificate.Subject)
	}

	subjectAltNames, altDiags := stringListValue(ctx, certificate.SubjectAltNames)
	diags.Append(altDiags...)
	model.SubjectAltNames = subjectAltNames

	// `alt_names` is the SAN list minus the common name: DSM repeats the common
	// name in sub_alt_name, but the configuration lists only the extras.
	extras := make([]string, 0, len(certificate.SubjectAltNames))
	for _, name := range certificate.SubjectAltNames {
		if name == certificate.Subject {
			continue
		}
		extras = append(extras, name)
	}
	altNameSet, setDiags := types.SetValueFrom(ctx, types.StringType, extras)
	diags.Append(setDiags...)
	model.AltNames = altNameSet

	services, serviceDiags := stringListValue(ctx, certificateServiceIDs(certificate))
	diags.Append(serviceDiags...)
	model.Services = services

	// No PEM is available for a certificate DSM issued and keeps to itself, so
	// the reported date is the only source here.
	expiresAt, expiryDiags := certificateExpiresAt("", certificate)
	diags.Append(expiryDiags...)
	model.ExpiresAt = expiresAt

	return diags
}

// stringSliceFromSet is the set counterpart of stringSliceFromList: null and
// unknown mean "not set" rather than "empty".
func stringSliceFromSet(ctx context.Context, set types.Set) ([]string, diag.Diagnostics) {
	if set.IsNull() || set.IsUnknown() {
		return nil, nil
	}
	var values []string
	diags := set.ElementsAs(ctx, &values, false)
	return values, diags
}
