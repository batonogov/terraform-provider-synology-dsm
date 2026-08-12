package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// SystemSettings is the NAS-wide date/time configuration exposed by
// SYNO.Core.Region.NTP. A single DSM host has exactly one of these.
//
// The shape mirrors what `method=get` returns on DSM 7.4-90075:
//
//	{"date":"2026/8/12","enable_ntp":"ntp","hour":16,"minute":8,
//	 "now":"Wed Aug 12 16:08:34 2026\n","second":34,
//	 "server":"time.google.com","timestamp":1786540114,"timezone":"Amman"}
type SystemSettings struct {
	// Timezone is DSM's own zone name ("Moscow", "Amman"), not an IANA
	// identifier ("Europe/Moscow"). SYNO.Core.Region.NTP `listzone` enumerates
	// the accepted values; see ListTimezones.
	Timezone string

	// NTPEnabled reports whether the clock is synchronised with an NTP server,
	// derived from NTPMode.
	NTPEnabled bool
	// NTPMode is the raw `enable_ntp` value DSM reported. DSM encodes the
	// synchronisation mode as a string ("ntp"), not a boolean, and only the
	// enabled spelling has ever been observed on hardware. Keeping the raw value
	// lets a write echo back exactly what DSM said instead of inventing one.
	NTPMode string
	// NTPServer is the configured time server. DSM keeps the last configured
	// value even while synchronisation is off. The parameter is `server`;
	// `ntp_server` does not exist on this API.
	NTPServer string

	// Clock readings that travel in the same payload. They are the NAS's current
	// wall clock, NOT configuration, and are deliberately never sent back — see
	// systemSettingsParams.
	Date      string // DSM format, no leading zeros: "2026/8/12"
	Hour      int64
	Minute    int64
	Second    int64
	Now       string // human-readable, trailing newline included by DSM
	Timestamp int64
}

// Timezone is one entry of SYNO.Core.Region.NTP `listzone`: the DSM zone name
// and its offset from GMT in seconds.
type Timezone struct {
	Value  string
	Offset int64
}

// SetSystemSettingsRequest describes a partial change. Nil fields are left as
// DSM currently has them: SetSystemSettings performs a read-modify-write
// because a partial `set` is rejected (see below).
type SetSystemSettingsRequest struct {
	Timezone   *string
	NTPEnabled *bool
	NTPServer  *string
}

func (c *Client) GetSystemSettings(ctx context.Context) (*SystemSettings, error) {
	data, err := c.DoAPI(ctx, "SYNO.Core.Region.NTP", "1", "get", url.Values{})
	if err != nil {
		return nil, fmt.Errorf("get system settings: %w", err)
	}

	return parseSystemSettings(data)
}

// ListTimezones enumerates the zone names DSM accepts, via `listzone`. Used to
// turn an unknown time zone into an actionable message rather than a bare 5701.
func (c *Client) ListTimezones(ctx context.Context) ([]Timezone, error) {
	data, err := c.DoAPI(ctx, "SYNO.Core.Region.NTP", "1", "listzone", url.Values{})
	if err != nil {
		return nil, fmt.Errorf("list timezones: %w", err)
	}

	return parseTimezones(data)
}

// SetSystemSettings applies a partial change to the NAS date/time configuration.
//
// DSM rejects a `set` that carries only the field being changed: sending just
// `timezone` answers with error 5701 ("parameter bad"), confirmed on DSM
// 7.4-90075. The whole configuration has to be present, so this method reads the
// current state, overlays the requested changes, and writes everything back —
// the same read-modify-write shape used for share permissions and user quotas.
//
// The read and the write are serialised under c.mu: with Terraform's default
// parallelism two concurrent callers would otherwise both read the old state
// and the second write would silently undo the first.
func (c *Client) SetSystemSettings(ctx context.Context, req SetSystemSettingsRequest) (*SystemSettings, error) {
	c.mu.Lock()

	current, err := c.GetSystemSettings(ctx)
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}

	desired := *current
	if req.Timezone != nil {
		desired.Timezone = *req.Timezone
	}
	if req.NTPServer != nil {
		desired.NTPServer = *req.NTPServer
	}
	desired.NTPMode = resolveNTPMode(*current, req.NTPEnabled)

	if err := c.setRegionNTP(ctx, systemSettingsParams(desired)); err != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("set system settings: %w", err)
	}
	c.mu.Unlock()

	return c.GetSystemSettings(ctx)
}

// setRegionNTP issues the `set` call, POST first and GET as a fallback.
//
// Which verb this API wants is genuinely unknown: no public capture of DSM's own
// Control Panel request exists, and this provider needs both verbs elsewhere —
// SYNO.Core.Share `set` requires POST, SYNO.Core.User `set` works over GET.
// Rather than pick one and be wrong on somebody's firmware, try POST and fall
// back on the two codes that mean "wrong shape of call" rather than "bad
// values": 105 (no permission for this operation, which DSM also returns for the
// wrong HTTP method) and 103 (method does not exist). A 5701 is a genuine
// rejection of the payload and is never retried.
func (c *Client) setRegionNTP(ctx context.Context, params url.Values) error {
	_, err := c.DoAPIPost(ctx, "SYNO.Core.Region.NTP", "1", "set", params)
	if err == nil {
		return nil
	}
	if !IsAPIError(err, 103, 105) {
		return err
	}

	_, getErr := c.DoAPI(ctx, "SYNO.Core.Region.NTP", "1", "set", params)
	if getErr != nil {
		// Report the original POST failure: it is the one that reflects the
		// intended call, and surfacing the fallback's error would send the
		// reader after the wrong problem.
		return err
	}
	return nil
}

// systemSettingsParams renders the parameter set for `set`.
//
// It carries exactly the three configuration fields and NOT the clock readings
// (date/hour/minute/second/now/timestamp) that `get` returns alongside them.
// That distinction is load-bearing: DSM's own SYNO.Core.Region.so exports two
// separate entry points, SYNONtpSet and SYNONtpSetWithModifiedTime, so echoing
// the clock back is the "set the time manually" path rather than a harmless
// round-trip. Configuration is written without touching the clock.
//
// The parameter names are `timezone`, `enable_ntp` and `server` — taken from the
// symbol table of that same library, which contains `server` and has no
// `ntp_server` at all.
//
// CAVEAT: the requirement that all three travel together is inferred, not
// confirmed. The one confirmed data point is the negative: a request carrying
// only `timezone` is rejected with 5701 (issue #57, DSM 7.4-90075).
func systemSettingsParams(s SystemSettings) url.Values {
	params := url.Values{}
	params.Set("timezone", s.Timezone)
	params.Set("enable_ntp", s.NTPMode)
	params.Set("server", s.NTPServer)

	return params
}

const (
	// ntpEnabledValue is the only `enable_ntp` value ever observed from DSM.
	ntpEnabledValue = "ntp"
	// ntpDisabledFallback is a guess, used only when DSM has not shown us its
	// own spelling for the disabled state. See resolveNTPMode.
	ntpDisabledFallback = "manual"
)

// resolveNTPMode decides what to send as `enable_ntp`.
//
// The enabled spelling is confirmed ("ntp"). The disabled one is not: no capture
// of a DSM with NTP switched off has been published, and /etc/synoinfo.conf's
// run_ntp_client="no" is the backing store, not necessarily the API value.
//
// So the value DSM itself last reported is preferred whenever it applies:
//   - no change requested -> echo the current mode verbatim;
//   - enabling -> the confirmed "ntp";
//   - disabling a NAS that is already off -> echo its own disabled spelling,
//     which is authoritative even though we cannot name it in advance;
//   - disabling a NAS that is on -> the only case that needs a guess.
func resolveNTPMode(current SystemSettings, want *bool) string {
	if want == nil {
		if current.NTPMode != "" {
			return current.NTPMode
		}
		if current.NTPEnabled {
			return ntpEnabledValue
		}
		return ntpDisabledFallback
	}

	if *want {
		return ntpEnabledValue
	}

	if current.NTPMode != "" && !current.NTPEnabled {
		return current.NTPMode
	}
	return ntpDisabledFallback
}

// parseNTPEnabled copes with both spellings DSM has been seen to use: the
// string mode ("ntp") observed on DSM 7.4, and a plain boolean, which costs
// nothing to accept and avoids a silent misread if a firmware changes it.
func parseNTPEnabled(v interface{}) bool {
	switch value := v.(type) {
	case bool:
		return value
	case string:
		switch value {
		case ntpEnabledValue, "true", "yes", "1":
			return true
		default:
			return false
		}
	case float64:
		return value != 0
	default:
		return false
	}
}

// ntpModeString renders the raw enable_ntp value as DSM sent it, so a write can
// echo it back unchanged. A boolean form is normalised to the string spelling.
func ntpModeString(v interface{}) string {
	switch value := v.(type) {
	case string:
		return value
	case bool:
		if value {
			return ntpEnabledValue
		}
		return ntpDisabledFallback
	default:
		return ""
	}
}

func parseSystemSettings(raw json.RawMessage) (*SystemSettings, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse system settings: %w", err)
	}

	s := &SystemSettings{}

	if v, ok := m["timezone"].(string); ok {
		s.Timezone = v
	}
	if v, ok := m["enable_ntp"]; ok {
		s.NTPEnabled = parseNTPEnabled(v)
		s.NTPMode = ntpModeString(v)
	}
	if v, ok := m["server"].(string); ok {
		s.NTPServer = v
	}
	if v, ok := m["date"].(string); ok {
		s.Date = v
	}
	if v, ok := m["hour"].(float64); ok {
		s.Hour = int64(v)
	}
	if v, ok := m["minute"].(float64); ok {
		s.Minute = int64(v)
	}
	if v, ok := m["second"].(float64); ok {
		s.Second = int64(v)
	}
	if v, ok := m["now"].(string); ok {
		s.Now = v
	}
	if v, ok := m["timestamp"].(float64); ok {
		s.Timestamp = int64(v)
	}

	return s, nil
}

// parseTimezones unpacks the `listzone` payload:
// {"zonedata":[{"value":"Moscow","offset":10800}, ...]}.
func parseTimezones(raw json.RawMessage) ([]Timezone, error) {
	var result struct {
		ZoneData []struct {
			Value  string  `json:"value"`
			Offset float64 `json:"offset"`
		} `json:"zonedata"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("parse timezone list: %w", err)
	}

	zones := make([]Timezone, 0, len(result.ZoneData))
	for _, z := range result.ZoneData {
		if z.Value == "" {
			continue
		}
		zones = append(zones, Timezone{Value: z.Value, Offset: int64(z.Offset)})
	}
	return zones, nil
}
