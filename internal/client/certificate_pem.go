package client

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNoCertificateInPEM reports that a PEM blob carried no CERTIFICATE block.
var ErrNoCertificateInPEM = errors.New("no CERTIFICATE block found in PEM data")

// LeafCertificate picks the end-entity certificate out of a PEM bundle.
//
// Taking the first block would be the convention (RFC 8446 orders a chain
// leaf-first) but not a guarantee: `cat chain.pem cert.pem` is a common enough
// way to build a bundle, and reading the intermediate as the leaf would report
// the CA's expiry — typically years later than the certificate that actually
// stops working. Silently returning a date a decade out is worse than any
// parse error, because an alert built on it never fires.
//
// So the leaf is identified structurally: it is the certificate that nothing
// else in the bundle was issued by. Ties are broken towards a non-CA
// certificate, and a bundle that is genuinely ambiguous falls back to the first
// block rather than failing.
func LeafCertificate(pemData string) (*x509.Certificate, error) {
	certificates, err := parseCertificateBundle(pemData)
	if err != nil {
		return nil, err
	}
	if len(certificates) == 1 {
		return certificates[0], nil
	}

	var candidates []*x509.Certificate
	for i, candidate := range certificates {
		if !signsAnother(candidate, certificates, i) {
			candidates = append(candidates, candidate)
		}
	}
	// Every certificate signing another means the bundle is a loop or a set of
	// unrelated self-signed certificates; there is nothing better than the
	// conventional order in that case.
	if len(candidates) == 0 {
		return certificates[0], nil
	}
	for _, candidate := range candidates {
		if !candidate.IsCA {
			return candidate, nil
		}
	}
	return candidates[0], nil
}

// signsAnother reports whether cert issued any other certificate in the bundle.
// The subject/issuer DER is compared rather than the common name so that two
// CAs sharing a display name are not confused, and the certificate's own index
// is skipped so a self-signed certificate does not exclude itself.
func signsAnother(cert *x509.Certificate, bundle []*x509.Certificate, self int) bool {
	for i, other := range bundle {
		if i == self {
			continue
		}
		if bytes.Equal(other.RawIssuer, cert.RawSubject) {
			return true
		}
	}
	return false
}

func parseCertificateBundle(pemData string) ([]*x509.Certificate, error) {
	var certificates []*x509.Certificate
	rest := []byte(pemData)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		parsed, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse certificate: %w", err)
		}
		certificates = append(certificates, parsed)
	}
	if len(certificates) == 0 {
		return nil, ErrNoCertificateInPEM
	}
	return certificates, nil
}

// CertificateExpiry returns the NotAfter of the leaf certificate in a PEM
// bundle, in UTC.
//
// This is deliberately computed from the certificate rather than from DSM's
// own valid_till field: the API renders dates in OpenSSL's locale-flavoured
// text form ("Nov 27 09:44:00 2026 GMT"), which is both lossy and a moving
// target across DSM versions, while the certificate itself is the authority on
// when it stops being valid. Callers that have no PEM to hand (a Let's Encrypt
// certificate DSM issued and never hands back) fall back to
// ParseDSMCertificateTime.
func CertificateExpiry(pemData string) (time.Time, error) {
	leaf, err := LeafCertificate(pemData)
	if err != nil {
		return time.Time{}, err
	}
	return leaf.NotAfter.UTC(), nil
}

// dsmCertificateTimeLayouts are the shapes DSM has been observed to use for
// valid_from/valid_till. The first is OpenSSL's ASN1_TIME_print output, which
// is what DSM 7 returns; the others are defensive, because this string is
// presentation rather than a documented format and has no stability guarantee.
var dsmCertificateTimeLayouts = []string{
	"Jan 2 15:04:05 2006 MST",
	"Jan 02 15:04:05 2006 MST",
	"Jan 2 15:04:05 2006",
	time.RFC3339,
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"2006/01/02 15:04:05",
}

// ParseDSMCertificateTime interprets a DSM valid_from/valid_till string.
//
// OpenSSL pads single-digit days with a space ("Nov  2 ..."), so the field is
// collapsed to single spaces before matching; Go's reference layouts do not
// model the padding.
func ParseDSMCertificateTime(value string) (time.Time, error) {
	normalized := strings.Join(strings.Fields(value), " ")
	if normalized == "" {
		return time.Time{}, errors.New("empty certificate timestamp")
	}
	for _, layout := range dsmCertificateTimeLayouts {
		if parsed, err := time.Parse(layout, normalized); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised DSM certificate timestamp %q", value)
}
