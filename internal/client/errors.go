package client

import (
	"context"
	"errors"
	"fmt"
)

// APIError is a DSM API failure carrying the numeric code DSM reported.
//
// API records which API produced the code, because DSM reuses numbers above
// 3000 across APIs: 3101 means "invalid home location" for SYNO.Core.User.Home
// and nothing of the sort elsewhere. It is filled in by the request layer and
// is deliberately not part of the wire format.
type APIError struct {
	Code int    `json:"code"`
	API  string `json:"-"`
}

// Error renders the code as a sentence rather than a bare number. Unknown codes
// still surface the number so an unrecognised failure stays diagnosable.
func (e *APIError) Error() string {
	if desc, ok := describeAPIError(e.API, e.Code); ok {
		return fmt.Sprintf("%s (code %d)", desc, e.Code)
	}
	return fmt.Sprintf("unexpected DSM error (code %d)", e.Code)
}

// IsAPIError reports whether err, or any error it wraps, is a DSM API error
// with one of the given codes. Prefer this over matching the rendered message:
// the wording is presentation, the code is the contract.
func IsAPIError(err error, codes ...int) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	for _, code := range codes {
		if apiErr.Code == code {
			return true
		}
	}
	return false
}

// commonAPIErrors are the codes DSM returns from every API.
var commonAPIErrors = map[int]string{
	100: "unknown DSM error",
	101: "invalid parameter",
	102: "the requested API does not exist",
	103: "the requested method does not exist",
	104: "the requested API version is not supported",
	105: "the session does not have permission for this operation",
	106: "the session timed out",
	107: "the session was interrupted by a duplicate login",
	119: "the session is invalid or has expired",
}

// apiSpecificErrors are codes whose meaning was confirmed against real DSM
// hardware. Only verified codes belong here — a wrong sentence is worse than a
// bare number, because it sends the reader off in the wrong direction.
var apiSpecificErrors = map[string]map[int]string{
	"SYNO.Core.Share": {
		// 3300 is DSM's general-purpose refusal for share requests, so the
		// description must not commit to one cause. It is returned both while an
		// earlier mutation is still settling (issue #50) and for a request DSM
		// considers malformed — a description over 64 characters, for one
		// (issue #65). The first wording named only the transient case and sent
		// readers hunting for a concurrency problem that was not there.
		3300: "DSM rejected the share request. This can mean an earlier share operation is still settling, or that a field exceeds what DSM accepts — descriptions are capped at 64 characters",
		3301: "a share with this name already exists",
		3328: "DSM rejected the request because another share operation was in progress; this usually clears on retry",
	},
	"SYNO.API.Auth": {
		400: "invalid account or password",
		401: "the account is disabled",
		402: "permission denied",
		403: "two-factor authentication is required for this account",
		404: "the two-factor authentication code is invalid",
	},
	"SYNO.Core.User": {
		3106: "user not found",
	},
	"SYNO.Core.User.Home": {
		3101: "invalid home location: it must be a volume path such as /volume1, not a bare volume name",
		3103: "the location parameter is required whenever the home service is enabled",
	},
	"SYNO.Core.Region.NTP": {
		// Confirmed on DSM 7.4-90075 (issue #57): a `set` carrying only
		// `timezone` is answered with 5701 and desc "parameter bad". The
		// accompanying message DSM renders ("Please sign in to DSM again") is
		// misleading — the session is fine, the parameter set is incomplete.
		5701: "DSM rejected the time settings as incomplete or malformed: SYNO.Core.Region.NTP set requires the full parameter set (timezone, enable_ntp and server together), not just the field being changed, and every value must be one DSM recognises",
	},
	// These two were verified against real DSM 7.x by third parties rather than
	// by this project — 4151 by a published DSM 7.x probe of the API, 4154 by a
	// production controller that handles it explicitly. Both are consistent
	// across sources; see the source list at the top of reverse_proxy.go.
	"SYNO.Core.AppPortal.ReverseProxy": {
		4151: "the reverse proxy entry is missing or was not sent as a JSON-encoded string",
		4154: "a reverse proxy entry with this description already exists",
	},
	// The 55xx family belongs to certificate handling. The wording comes from
	// DSM's own webman translation strings, which two independent clients
	// extracted byte-identically; 5510, 5512, and 5517 are additionally confirmed
	// by acme.sh bug reports against real hardware.
	"SYNO.Core.Certificate": {
		5510: "the certificate file is not a valid certificate",
		5511: "the private key file is not a valid private key",
		5512: "the intermediate certificate file is not valid",
		5513: "the certificate uses an unsupported cipher",
		5514: "the private key does not match the certificate",
		5515: "the certificate was not issued by a trusted authority",
		5516: "the uploaded file must be UTF-8 encoded",
		5517: "the certificate could not be verified against the supplied intermediate chain",
		5518: "DSA keys are not supported",
		5519: "the certificate signing request is invalid",
		5534: "the key is too short: DSM requires more than 1024 bits",
	},
	"SYNO.Core.Certificate.LetsEncrypt": {
		5520: "no response from the Let's Encrypt server",
		5521: "Let's Encrypt could not validate the domain, usually because inbound TCP/80 does not reach this NAS",
		5522: "the issuer could not validate the domain",
		5523: "too many ACME accounts have been registered from this IP address",
		5524: "too many certificates have been requested for this domain recently (a Let's Encrypt rate limit)",
		5525: "the email address is invalid",
		5526: "a parameter value is invalid",
		5527: "the Let's Encrypt server is busy",
		5528: "wildcard certificates are only supported for Synology DDNS domains",
		5529: "the domain is not publicly resolvable",
		5530: "the domain could not be reached: check the public IP address, any reverse proxy, and the firewall",
	},
}

// describeAPIError resolves a code to a sentence, preferring the API-specific
// meaning over the shared one.
func describeAPIError(api string, code int) (string, bool) {
	if perAPI, ok := apiSpecificErrors[api]; ok {
		if desc, ok := perAPI[code]; ok {
			return desc, true
		}
	}
	desc, ok := commonAPIErrors[code]
	return desc, ok
}

// NotFoundError reports that DSM answered the request and the object simply is
// not there.
//
// It exists to keep one distinction sharp, because Terraform's Read contract
// hangs off it: an object that is gone from DSM must leave state, so the next
// plan re-creates it, while every other failure — a broken connection, an
// expired session, a permission refusal, a malformed request — must surface as
// an error. Collapsing the two in either direction is a real bug: erroring on a
// deleted object breaks `terraform plan` for the whole configuration (issue
// #131), and treating a network failure as "gone" silently drops a resource
// that still exists and plans a re-create of it.
//
// Only construct one where absence is *established*, never where it is merely
// plausible: DSM answered, the answer was well formed, and the object was not
// in it. When DSM refuses in a way that leaves the question open, return the
// refusal.
type NotFoundError struct {
	// Kind is the sort of object, in the words the message should use
	// ("firewall rule", "user", "DSM package").
	Kind string
	// Name identifies the object; empty for a singleton.
	Name string
	// Scope is an optional trailing qualifier such as
	// `in profile "default" adapter "global"`.
	Scope string
}

func (e *NotFoundError) Error() string {
	switch {
	case e.Name == "" && e.Scope == "":
		return fmt.Sprintf("%s not found", e.Kind)
	case e.Scope == "":
		return fmt.Sprintf("%s %q not found", e.Kind, e.Name)
	default:
		return fmt.Sprintf("%s %q not found %s", e.Kind, e.Name, e.Scope)
	}
}

// IsNotFound reports whether err, or any error it wraps, means "DSM answered,
// and the object is not there".
//
// Deliberately no *APIError code is mapped here. A DSM code such as 105
// ("no permission") or 119 ("session invalid") describes the caller, not the
// object, and a resource that removed itself from state on those would delete
// working infrastructure from the practitioner's state file on a transient
// failure. Where a code does establish absence for one API — SYNO.Core.User's
// 3106, say — the client turns it into a *NotFoundError at that call site,
// where the API is known.
func IsNotFound(err error) bool {
	var notFound *NotFoundError
	return errors.As(err, &notFound)
}

// absenceConfirmedBy reports whether an ambiguous failure from a `get` really
// means the object is gone, by asking a second, independent read.
//
// DSM has no documented "no such object" code for SYNO.Core.Group or
// SYNO.Core.Share, and the rendered message is presentation rather than
// contract, so absence is never inferred from the failure itself — it is
// confirmed by listing, an API this provider already depends on. Two properties
// keep the confirmation safe in the direction that matters:
//
//   - it only runs when DSM itself answered, i.e. when the failure carries an
//     *APIError. A connection that never got a reply is not an answer about the
//     object, and is returned unchanged;
//   - if the second read also fails — expired session, unreachable NAS —
//     nothing is confirmed and the caller keeps the original error. Never the
//     other way round: a Read that dropped a resource from state on a network
//     blip would plan a re-create of infrastructure that is still running.
func absenceConfirmedBy(ctx context.Context, err error, exists func(context.Context) (bool, error)) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	found, listErr := exists(ctx)
	return listErr == nil && !found
}
