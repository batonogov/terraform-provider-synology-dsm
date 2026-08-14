package provider

import (
	"context"
	"errors"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

func NewCertificateServiceResource() resource.Resource {
	return &certificateServiceResource{}
}

type certificateServiceResource struct {
	client *client.Client
}

type certificateServiceResourceModel struct {
	ID            types.String `tfsdk:"id"`
	Service       types.String `tfsdk:"service"`
	CertificateID types.String `tfsdk:"certificate_id"`
}

func (r *certificateServiceResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_certificate_service"
}

func (r *certificateServiceResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Binds a certificate to an individual DSM service — the step that otherwise has to be done by hand in " +
			"Control Panel > Security > Certificate > Settings. This closes the declarative TLS pipeline: " +
			"`dsm_certificate_lets_encrypt` (or `dsm_certificate`) issues the material, `dsm_reverse_proxy` creates the listener, " +
			"and this resource binds the two together.\n\n" +
			"Binding a service restarts it inside DSM. Rebinding to a different certificate is an in-place update; " +
			"destroy unbinds nothing — DSM cannot serve a service without a certificate, so removal leaves whatever binding is " +
			"in force and only drops the resource from state.",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Identifier of the binding: the service identifier.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"service": schema.StringAttribute{
				Required: true,
				Description: "DSM service identifier to bind, exactly as it appears in a certificate's `services` list — " +
					"e.g. `default` for the DSM web UI, or the reverse-proxy service name such as `synology.at.caddy`. " +
					"The `dsm_certificates` data source lists every service identifier currently in use. " +
					"Forces replacement if changed.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"certificate_id": schema.StringAttribute{
				Required: true,
				Description: "Id of the certificate to serve the service with — `dsm_certificate.id` or " +
					"`dsm_certificate_lets_encrypt.id`. Changing it rebinds in place; the service keeps running through " +
					"the swap.",
			},
		},
	}
}

func (r *certificateServiceResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	dsmClient := clientFromProviderData(req.ProviderData, &resp.Diagnostics)
	if dsmClient == nil {
		return
	}

	r.client = dsmClient
}

func (r *certificateServiceResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan certificateServiceResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	serviceID := plan.Service.ValueString()
	certificateID := plan.CertificateID.ValueString()

	// The certificate must exist before it can serve anything; the error names
	// it rather than the service so the fix is obvious.
	if _, err := r.client.GetCertificate(ctx, certificateID); err != nil {
		resp.Diagnostics.AddError("Failed to read certificate", certificateErrorDetail(err))
		return
	}

	binding, err := r.client.FindCertificateServiceBinding(ctx, serviceID)
	if err != nil && !errors.Is(err, client.ErrCertificateServiceNotFound) {
		resp.Diagnostics.AddError("Failed to read certificate service", err.Error())
		return
	}

	oldID := ""
	if binding != nil {
		if binding.CertificateID == certificateID {
			// Already bound, perhaps by hand or by an earlier apply that did not
			// record it. Nothing to write — DSM rejects a no-op set by moving the
			// service to the DSM default.
			tflog.Info(ctx, "Certificate service already bound to requested certificate", map[string]interface{}{
				"service": serviceID,
			})
			plan.ID = types.StringValue(serviceID)
			resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
			return
		}
		oldID = binding.CertificateID
	}

	if err := r.client.SetCertificateService(ctx, bindingServiceObject(binding), oldID, certificateID); err != nil {
		resp.Diagnostics.AddError("Failed to bind certificate to service", err.Error())
		return
	}

	plan.ID = types.StringValue(serviceID)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *certificateServiceResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state certificateServiceResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	binding, err := r.client.FindCertificateServiceBinding(ctx, state.Service.ValueString())
	if err != nil {
		if errors.Is(err, client.ErrCertificateServiceNotFound) {
			// The service disappeared — the reverse proxy entry was deleted, or
			// the package providing it was uninstalled. Recreating is the right
			// answer, exactly like a vanished certificate.
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Failed to read certificate service", err.Error())
		return
	}

	state.CertificateID = types.StringValue(binding.CertificateID)
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *certificateServiceResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan certificateServiceResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var state certificateServiceResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	serviceID := plan.Service.ValueString()
	certificateID := plan.CertificateID.ValueString()

	// `service` is RequiresReplace, so an Update here means only the certificate
	// changed: rebind in place.
	if _, err := r.client.GetCertificate(ctx, certificateID); err != nil {
		resp.Diagnostics.AddError("Failed to read certificate", certificateErrorDetail(err))
		return
	}

	binding, err := r.client.FindCertificateServiceBinding(ctx, serviceID)
	if err != nil && !errors.Is(err, client.ErrCertificateServiceNotFound) {
		resp.Diagnostics.AddError("Failed to read certificate service", err.Error())
		return
	}

	if binding != nil && binding.CertificateID == certificateID {
		plan.ID = types.StringValue(serviceID)
		resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
		return
	}

	oldID := ""
	if binding != nil {
		oldID = binding.CertificateID
	}

	if err := r.client.SetCertificateService(ctx, bindingServiceObject(binding), oldID, certificateID); err != nil {
		resp.Diagnostics.AddError("Failed to rebind certificate", err.Error())
		return
	}

	plan.ID = types.StringValue(serviceID)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *certificateServiceResource) Delete(_ context.Context, _ resource.DeleteRequest, resp *resource.DeleteResponse) {
	// Deliberately a no-op. DSM cannot serve a service without a certificate:
	// unbinding would leave the service on the NAS default at best and broken at
	// worst. Removing the resource only drops it from state, in the same spirit
	// as dsm_package's uninstall_on_destroy=false default.
	resp.State.RemoveResource(context.Background())
}

func (r *certificateServiceResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// bindingServiceObject returns the raw service object DSM requires in a write.
// When no binding exists yet the service object has to come from the new
// certificate's own services list — DSM lists there every service it could
// serve, bound or not.
func bindingServiceObject(binding *client.CertificateServiceBinding) map[string]interface{} {
	if binding != nil {
		return binding.Service
	}
	return nil
}
