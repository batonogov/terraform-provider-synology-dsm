package client

import (
	"net/url"
	"strings"
)

// Debug logging of the DSM exchange exists because the alternative is what a
// user of this provider actually hit: a resource that reported success, a NAS
// that disagreed, and a TF_LOG trace carrying three lines from this module and
// not one request or response body. Diagnosing an undocumented API without
// seeing the wire is guesswork, and every wire contract in this package was
// reconstructed by someone who could see it.
//
// The logs go out at DEBUG, so they are off during ordinary applies and appear
// with TF_LOG=DEBUG.

// maxLoggedBodyBytes caps what a single log line can carry. Responses from
// SYNO.Core.Package.list or a File Station download of a 16 MiB file would
// otherwise bury the exchange the reader is looking for, and Terraform's log
// sink keeps everything in one stream.
const maxLoggedBodyBytes = 8 << 10

// redactedValue is what stands in for a secret. It is deliberately not the
// empty string: "the provider sent a password here" and "the provider sent
// nothing here" are different facts, and the second one is a bug worth seeing.
const redactedValue = "<redacted>"

// secretParams are the request parameters whose values must never reach a log.
//
// Two kinds live here. Credentials — a DSM account password, an LDAP bind
// password, a share encryption key — are obvious. Session material is the less
// obvious half and matters just as much: a _sid or a SynoToken lifted from a
// debug log is a working DSM session, and practitioners paste these logs into
// issues. That is exactly how this file came to exist, so it must not turn a
// bug report into a credential leak.
var secretParams = map[string]bool{
	"passwd":             true,
	"password":           true,
	"new_password":       true,
	"encrypt_pwd":        true,
	"bind_pw":            true,
	"smtp_pwd":           true,
	"_sid":               true,
	"SynoToken":          true,
	"SynoConfirmPWToken": true,
	"otp_code":           true,
	"device_id":          true,
}

// bulkParams carry a document rather than a setting: a file's contents, a
// compose project, a certificate and its private key. They are summarised by
// length instead of being printed, because printing them is both useless for
// diagnosing an API contract and the fastest way to write a private key into a
// log file.
var bulkParams = map[string]bool{
	"content":     true,
	"key":         true,
	"cert":        true,
	"inter_cert":  true,
	"compose":     true,
	"raw_content": true,
}

// redactParams renders request parameters for a log line: secrets replaced,
// documents summarised, everything else printed as sent.
//
// Everything else is printed *as sent* on purpose. The parameters this provider
// gets wrong are the ordinary ones — a JSON-quoted profile name, an adapter key
// DSM does not keep, a shareinfo field that is accepted and ignored — and a log
// that hides them to be safe would hide the bug too.
func redactParams(params url.Values) map[string]interface{} {
	out := make(map[string]interface{}, len(params))
	for key, values := range params {
		switch {
		case secretParams[key]:
			out[key] = redactedValue
		case bulkParams[key]:
			out[key] = summariseValue(values)
		default:
			out[key] = truncate(strings.Join(values, ","))
		}
	}
	return out
}

// summariseValue reports a document's size rather than its content.
func summariseValue(values []string) string {
	size := 0
	for _, v := range values {
		size += len(v)
	}
	return redactedValue + " (" + itoa(size) + " bytes)"
}

// truncate keeps a long value from swallowing the log, and says that it did.
// A silently cut response would send the reader looking for a malformed payload
// that is only malformed in the log.
func truncate(s string) string {
	if len(s) <= maxLoggedBodyBytes {
		return s
	}
	return s[:maxLoggedBodyBytes] + "... (truncated, " + itoa(len(s)) + " bytes total)"
}

// itoa avoids pulling strconv in for one call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// looksLikeHTML reports whether a response body is a web page rather than an
// API envelope. DSM serves its own 404 page when synoscgi dies mid-request, so
// this is the difference between "the payload crashed the handler" and a
// mystifying JSON syntax error.
func looksLikeHTML(body []byte) bool {
	head := strings.TrimSpace(string(body))
	if len(head) > 512 {
		head = head[:512]
	}
	lower := strings.ToLower(head)
	return strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<html")
}
