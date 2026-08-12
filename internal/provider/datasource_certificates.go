package provider

import (
	"context"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

func NewCertificatesDataSource() datasource.DataSource {
	return &certificatesDataSource{}
}

type certificatesDataSource struct {
	client *client.Client
}

type certificatesDataSourceModel struct {
	ID           types.String            `tfsdk:"id"`
	Description  types.String            `tfsdk:"description"`
	Certificates []certificateEntryModel `tfsdk:"certificates"`
}

type certificateEntryModel struct {
	ID              types.String `tfsdk:"id"`
	Description     types.String `tfsdk:"description"`
	Subject         types.String `tfsdk:"subject"`
	SubjectAltNames types.List   `tfsdk:"subject_alt_names"`
	Issuer          types.String `tfsdk:"issuer"`
	ExpiresAt       types.String `tfsdk:"expires_at"`
	IsDefault       types.Bool   `tfsdk:"is_default"`
	SelfSigned      types.Bool   `tfsdk:"self_signed"`
	Renewable       types.Bool   `tfsdk:"renewable"`
	Services        types.List   `tfsdk:"services"`
}

func (d *certificatesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_certificates"
}

func (d *certificatesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Lists the certificates installed on Synology DSM, optionally narrowed to one description. " +
			"Useful for alerting on `expires_at` across every certificate on the NAS, including the ones Terraform does not manage — " +
			"the self-signed certificate DSM ships with, for instance. No private key is ever returned by DSM, so nothing sensitive is read here.",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Identifier for this data source result: the description filter, or `all` when unfiltered.",
			},
			"description": schema.StringAttribute{
				Optional:    true,
				Description: "Return only certificates whose DSM description matches this value exactly. Omit to return all of them. DSM does not require descriptions to be unique, so a filter may still match several.",
			},
			"certificates": schema.ListNestedAttribute{
				Computed:    true,
				Description: "Matching certificates, in the order DSM lists them.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id": schema.StringAttribute{
							Computed:    true,
							Description: "Certificate identifier assigned by DSM. This is the value `terraform import` takes.",
						},
						"description": schema.StringAttribute{
							Computed:    true,
							Description: "Name shown in the DSM certificate control panel.",
						},
						"subject": schema.StringAttribute{
							Computed:    true,
							Description: "Subject common name.",
						},
						"subject_alt_names": schema.ListAttribute{
							Computed:    true,
							ElementType: types.StringType,
							Description: "Subject alternative names.",
						},
						"issuer": schema.StringAttribute{
							Computed:    true,
							Description: "Issuer common name.",
						},
						"expires_at": schema.StringAttribute{
							Computed: true,
							Description: "Expiry, RFC 3339 in UTC — the attribute to alert on. Null if DSM reported a date this provider could not parse; " +
								"a warning then names the raw value.",
						},
						"is_default": schema.BoolAttribute{
							Computed:    true,
							Description: "Whether this is the DSM default certificate.",
						},
						"self_signed": schema.BoolAttribute{
							Computed:    true,
							Description: "Whether DSM considers the certificate self-signed.",
						},
						"renewable": schema.BoolAttribute{
							Computed:    true,
							Description: "Whether DSM can renew the certificate itself, which is true for the ones it obtained over ACME.",
						},
						"services": schema.ListAttribute{
							Computed:    true,
							ElementType: types.StringType,
							Description: "DSM service identifiers currently served by this certificate.",
						},
					},
				},
			},
		},
	}
}

func (d *certificatesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	dsmClient := clientFromProviderData(req.ProviderData, &resp.Diagnostics)
	if dsmClient == nil {
		return
	}

	d.client = dsmClient
}

func (d *certificatesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config certificatesDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	filter := config.Description.ValueString()

	tflog.Debug(ctx, "Listing DSM certificates", map[string]interface{}{
		"description": filter,
	})

	certificates, err := d.client.ListCertificates(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to list certificates", certificateErrorDetail(err))
		return
	}

	config.Certificates = make([]certificateEntryModel, 0, len(certificates))
	for i := range certificates {
		certificate := certificates[i]
		if filter != "" && certificate.Description != filter {
			continue
		}

		entry := certificateEntryModel{
			ID:          types.StringValue(certificate.ID),
			Description: types.StringValue(certificate.Description),
			Subject:     types.StringValue(certificate.Subject),
			Issuer:      types.StringValue(certificate.Issuer),
			IsDefault:   types.BoolValue(certificate.IsDefault),
			SelfSigned:  types.BoolValue(certificate.SelfSigned),
			Renewable:   types.BoolValue(certificate.Renewable),
		}

		altNames, diags := stringListValue(ctx, certificate.SubjectAltNames)
		resp.Diagnostics.Append(diags...)
		entry.SubjectAltNames = altNames

		services, diags := stringListValue(ctx, certificateServiceIDs(&certificate))
		resp.Diagnostics.Append(diags...)
		entry.Services = services

		expiresAt, diags := certificateExpiresAt("", &certificate)
		resp.Diagnostics.Append(diags...)
		entry.ExpiresAt = expiresAt

		config.Certificates = append(config.Certificates, entry)
	}

	if resp.Diagnostics.HasError() {
		return
	}

	if filter != "" {
		config.ID = types.StringValue(filter)
	} else {
		config.ID = types.StringValue("all")
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}
