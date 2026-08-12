package client

import (
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
