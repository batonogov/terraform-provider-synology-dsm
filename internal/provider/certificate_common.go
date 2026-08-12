package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// sensitiveStateWarning is repeated on every attribute that carries key
// material. Terraform redacts sensitive values in plan output but stores them
// verbatim in state, and a private key in a shared state file is worth being
// blunt about rather than burying in a note.
const sensitiveStateWarning = "**This value is stored in Terraform state in clear text.** Terraform redacts it from plan and apply output, " +
	"but state is not encrypted: use an encrypted remote state backend and restrict access to it, or keep the key in a secret manager " +
	"and pass it in through a variable that is never written to disk."

// certificateExpiresAt renders a certificate's expiry as RFC 3339 in UTC.
//
// The PEM is preferred over DSM's own valid_till whenever the configuration
// carries it: DSM renders dates as OpenSSL text in an unspecified locale and
// has no documented stability guarantee for that string, while the certificate
// is definitionally the authority on its own validity window. Certificates DSM
// issued itself (Let's Encrypt) have no PEM here, so those fall back to the
// reported string.
func certificateExpiresAt(pemData string, certificate *client.Certificate) (types.String, diag.Diagnostics) {
	var diags diag.Diagnostics

	var reported time.Time
	var reportedErr error
	if certificate != nil {
		reported, reportedErr = certificate.ExpiresAt()
	}

	if strings.TrimSpace(pemData) != "" {
		expiry, err := client.CertificateExpiry(pemData)
		if err == nil {
			// The configured PEM wins, but a disagreement with DSM is worth saying
			// out loud: it means the certificate behind this id is not the one in
			// the configuration — replaced on the NAS out of band, most likely — and
			// expires_at would otherwise keep reporting the configured certificate's
			// date forever, which is exactly the date nobody is being served.
			if reportedErr == nil && !expiry.Equal(reported) {
				diags.AddWarning(
					"DSM is serving a different certificate than the configuration describes",
					fmt.Sprintf("The configured certificate expires at %s, but DSM reports %s for the certificate under this id. "+
						"Something replaced it outside Terraform. `expires_at` reports the configured value (%s); re-apply to put the "+
						"configured certificate back, or import the one DSM actually holds.",
						expiry.Format(rfc3339), reported.Format(rfc3339), expiry.Format(rfc3339)),
				)
			}
			return types.StringValue(expiry.Format(rfc3339)), diags
		}
		diags.AddWarning(
			"Could not read the expiry from the certificate",
			fmt.Sprintf("Falling back to the date DSM reports. Parsing the configured certificate failed: %s", err),
		)
	}

	if certificate == nil {
		return types.StringNull(), diags
	}

	if reportedErr != nil {
		diags.AddWarning(
			"Could not determine when the certificate expires",
			fmt.Sprintf("DSM reported valid_till as %q, which this provider could not parse: %s. "+
				"expires_at is unset; alerting on it will not work until this is resolved.", certificate.ValidTill, reportedErr),
		)
		return types.StringNull(), diags
	}
	return types.StringValue(reported.Format(rfc3339)), diags
}

// rfc3339 is spelled out rather than taken from the time package so the format
// stays obvious at the call sites that feed monitoring.
const rfc3339 = "2006-01-02T15:04:05Z07:00"

// certificateServiceIDs returns the DSM service identifiers a certificate is
// assigned to, sorted so the attribute does not churn between refreshes.
// Identifiers rather than display names: display names are localised, these are
// what stays comparable in a plan.
func certificateServiceIDs(certificate *client.Certificate) []string {
	ids := make([]string, 0, len(certificate.Services))
	for _, service := range certificate.Services {
		if service.Service != "" {
			ids = append(ids, service.Service)
		}
	}
	sort.Strings(ids)
	return ids
}

func stringListValue(ctx context.Context, values []string) (types.List, diag.Diagnostics) {
	if values == nil {
		values = []string{}
	}
	return types.ListValueFrom(ctx, types.StringType, values)
}

// planDefaultReclaim makes a certificate that should be the DSM default, but is
// not, produce a plan.
//
// Without this, `set_as_default = true` is a one-shot: if something takes the
// default away outside Terraform, `is_default` reads false on the next refresh,
// nothing in the configuration changed, and Terraform reports no drift at all.
// Marking the computed `is_default` unknown gives the plan something to show
// and gets Update called, which re-claims it.
func planDefaultReclaim(ctx context.Context, plan tfsdkPlan, setAsDefault types.Bool, isDefault types.Bool, diags *diag.Diagnostics) {
	if !setAsDefault.ValueBool() || isDefault.IsNull() || isDefault.ValueBool() {
		return
	}
	diags.Append(plan.SetAttribute(ctx, path.Root("is_default"), types.BoolUnknown())...)
}

// tfsdkPlan is the slice of *tfsdk.Plan that planDefaultReclaim needs, named so
// both resources can share the helper without depending on the request type.
type tfsdkPlan interface {
	SetAttribute(ctx context.Context, path path.Path, value interface{}) diag.Diagnostics
}

// certificateInUseError is the refusal that keeps DSM from being left with a
// service that has no certificate. It is deliberately specific: the services by
// name, and both ways out.
func certificateInUseError(certificate *client.Certificate) (summary, detail string) {
	names := certificate.ServiceNames()
	sort.Strings(names)

	summary = "Refusing to delete a certificate that is still in use"
	detail = fmt.Sprintf(
		"DSM certificate %q (id %s) is still assigned to %d service(s):\n  - %s\n\n"+
			"Deleting it would leave those services without a certificate, so this provider will not do it.\n\n"+
			"Resolve it in one of these ways:\n"+
			"  1. Reassign the services to another certificate first — in DSM: Control Panel > Security > Certificate > Settings, "+
			"pick a different certificate for each service listed above, then run terraform destroy again.\n"+
			"  2. Replace this certificate in place instead of destroying it: change `certificate`/`private_key` on the same resource, "+
			"which keeps the id and the service assignments.\n"+
			"  3. If DSM should keep the certificate and Terraform should only stop managing it, run "+
			"`terraform state rm <address>`.\n"+
			"  4. If you accept that the listed services lose their certificate, set `force_destroy = true` on the resource and apply that first.",
		certificate.Description, certificate.ID, len(names), strings.Join(names, "\n  - "))

	if certificate.IsDefault {
		detail += "\n\nThis is also the DSM default certificate: anything without an explicit assignment falls back to it."
	}
	return summary, detail
}

// certificateErrorDetail turns a DSM code into something a reader can act on.
// Only codes whose meaning is not obvious from the client's own rendering are
// annotated here, and the annotations live in the provider rather than in the
// client's verified error table because they have not been confirmed against
// real hardware.
func certificateErrorDetail(err error) string {
	message := err.Error()
	switch {
	case client.IsAPIError(err, 102):
		return message + "\n\nThe certificate API is unavailable on this DSM. It exists on DSM 7.x; Virtual DSM and older releases may not expose it."
	case client.IsAPIError(err, 103):
		return message + "\n\nOn the certificate APIs this usually means the request reached DSM without a valid SynoToken rather than that the method is missing. " +
			"Check that the provider is talking to DSM 7.x over a session it established itself."
	case client.IsAPIError(err, 105):
		return message + "\n\nThe provider account may not manage certificates. Use an account in the DSM administrators group."
	case client.IsAPIError(err, 101):
		return message + "\n\nDSM rejected a parameter. For an import, the usual cause is a private key that does not match the certificate, " +
			"a passphrase-protected key (DSM only accepts unencrypted keys), or a chain in the wrong order."
	case client.IsAPIError(err, 5514):
		return message + "\n\nCheck that `certificate` and `private_key` are the pair produced together; a mismatched pair is the single most common import failure."
	case client.IsAPIError(err, 5517):
		return message + "\n\n`intermediate` must chain to `certificate`. A stale chain file left over from a previous issuer is the usual cause."
	case client.IsAPIError(err, 5516):
		return message + "\n\nThe PEM must be UTF-8. A file that went through a Windows editor, or one carrying a byte-order mark, is rejected."
	default:
		return message
	}
}

// letsEncryptErrorDetail explains an issuance failure in the terms that
// actually cause it. DSM returns a bare code for an ACME failure, and "code
// 101" tells a reader nothing about a domain that does not resolve.
func letsEncryptErrorDetail(err error, domain string, altNames []string) string {
	names := append([]string{domain}, altNames...)

	return fmt.Sprintf("%s\n\n"+
		"DSM runs the whole ACME exchange itself and reports a failure as a bare code, so the cause has to be inferred. "+
		"Let's Encrypt validates every name over the public internet, which means all of the following must hold for %s:\n"+
		"  - every name resolves publicly to this NAS\n"+
		"  - inbound TCP/80 reaches the NAS from the internet (HTTP-01 validation; port forwarding and any upstream firewall included)\n"+
		"  - DSM's reverse proxy or Web Station is not intercepting /.well-known/acme-challenge/\n"+
		"  - the rate limits for the registered domain have not been hit (Let's Encrypt allows a limited number of certificates per week)\n"+
		"  - the NAS clock is correct and it can reach the ACME directory outbound over TCP/443\n\n"+
		"DSM's own log carries the ACME-level reason: Control Panel > Security > Certificate, and /var/log/messages on the NAS.",
		certificateErrorDetail(err), strings.Join(names, ", "))
}
