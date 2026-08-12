package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// ntpState is the mock DSM's view of SYNO.Core.Region.NTP. The initial values
// mirror the real `get` payload captured on DSM 7.4-90075 (issue #57).
type ntpState struct {
	mu        sync.Mutex
	timezone  string
	enableNTP string
	server    string
	date      string
	hour      float64
	minute    float64
	second    float64
	timestamp float64
}

func newNTPState() *ntpState {
	return &ntpState{
		timezone:  "Amman",
		enableNTP: "ntp",
		server:    "time.google.com",
		date:      "2026/8/12",
		hour:      16,
		minute:    8,
		second:    34,
		timestamp: 1786540114,
	}
}

func (s *ntpState) snapshot() map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]interface{}{
		"date":       s.date,
		"enable_ntp": s.enableNTP,
		"hour":       s.hour,
		"minute":     s.minute,
		"now":        "Wed Aug 12 16:08:34 2026\n",
		"second":     s.second,
		"server":     s.server,
		"timestamp":  s.timestamp,
		"timezone":   s.timezone,
	}
}

// ntpSetRequest records one observed `set` call: every parameter it carried,
// plus the transport details. Presence matters as much as the values — the point
// of the read-modify-write is that nothing gets dropped, and the point of
// omitting the clock fields is that nothing extra gets added.
type ntpSetRequest struct {
	values map[string]string
	keys   []string
	method string
	sid    string
}

func (r ntpSetRequest) has(key string) bool {
	_, ok := r.values[key]
	return ok
}

// ntpConfigKeys are the parameters a `set` must carry: the configuration, and
// only the configuration.
var ntpConfigKeys = []string{"timezone", "enable_ntp", "server"}

// ntpClockKeys are the clock readings `get` returns alongside the configuration.
// Sending them back is DSM's "set the time manually" path (the library exports a
// separate SYNONtpSetWithModifiedTime entry point), so a config write must not.
var ntpClockKeys = []string{"date", "hour", "minute", "second", "now", "timestamp"}

// setupNTPTestServer builds a mock DSM implementing get/set/listzone for
// SYNO.Core.Region.NTP. Observed `set` calls are appended to *observed, and the
// state is mutated so a subsequent `get` reflects the write.
func setupNTPTestServer(t *testing.T, state *ntpState, observed *[]ntpSetRequest) *Client {
	t.Helper()

	var obsMu sync.Mutex
	mux := http.NewServeMux()

	mux.HandleFunc("/webapi/entry.cgi", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = r.ParseForm()
		}
		method := r.FormValue("method")
		if method == "" {
			method = r.URL.Query().Get("method")
		}

		switch method {
		case "get":
			raw, _ := json.Marshal(state.snapshot())
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		case "listzone":
			raw, _ := json.Marshal(map[string]interface{}{
				"zonedata": []map[string]interface{}{
					{"value": "Amman", "offset": 10800},
					{"value": "Moscow", "offset": 10800},
					{"value": "Amsterdam", "offset": 3600},
					{"value": "New York", "offset": -18000},
				},
			})
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		case "set":
			req := ntpSetRequest{
				values: map[string]string{},
				method: r.Method,
				sid:    r.URL.Query().Get("_sid"),
			}
			for key := range r.Form {
				switch key {
				case "api", "version", "method", "_sid", "SynoToken":
					continue
				}
				req.values[key] = r.FormValue(key)
				req.keys = append(req.keys, key)
			}

			obsMu.Lock()
			*observed = append(*observed, req)
			obsMu.Unlock()

			// DSM refuses a request that does not carry the whole configuration
			// (issue #57). Reproduce that so a client regression that starts
			// sending a partial set fails loudly rather than passing against a
			// lenient mock.
			for _, key := range ntpConfigKeys {
				if !req.has(key) {
					_ = json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 5701}})
					return
				}
			}

			state.mu.Lock()
			state.timezone = req.values["timezone"]
			state.enableNTP = req.values["enable_ntp"]
			state.server = req.values["server"]
			state.mu.Unlock()

			_ = json.NewEncoder(w).Encode(APIResponse{Success: true})

		default:
			_ = json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 103}})
		}
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	c := NewClient(server.URL, "admin", "", false)
	c.setSession("test-sid", "test-token")

	return c
}

func TestClient_GetSystemSettings(t *testing.T) {
	state := newNTPState()
	var observed []ntpSetRequest
	c := setupNTPTestServer(t, state, &observed)

	settings, err := c.GetSystemSettings(context.Background())
	if err != nil {
		t.Fatalf("GetSystemSettings: %v", err)
	}

	if settings.Timezone != "Amman" {
		t.Errorf("Timezone = %q, want Amman", settings.Timezone)
	}
	if !settings.NTPEnabled {
		t.Error(`NTPEnabled = false, want true (DSM reports enable_ntp:"ntp")`)
	}
	if settings.NTPMode != "ntp" {
		t.Errorf("NTPMode = %q, want the raw DSM value ntp", settings.NTPMode)
	}
	if settings.NTPServer != "time.google.com" {
		t.Errorf("NTPServer = %q, want time.google.com", settings.NTPServer)
	}
	if settings.Date != "2026/8/12" {
		t.Errorf("Date = %q, want 2026/8/12", settings.Date)
	}
	if settings.Hour != 16 || settings.Minute != 8 || settings.Second != 34 {
		t.Errorf("clock = %d:%d:%d, want 16:8:34", settings.Hour, settings.Minute, settings.Second)
	}
	if settings.Timestamp != 1786540114 {
		t.Errorf("Timestamp = %d, want 1786540114", settings.Timestamp)
	}
}

// TestClient_SetSystemSettings_TimezoneOnlySendsFullConfig is the central test
// for this API: the caller changes one field, but DSM must receive the whole
// configuration. A partial `set` is what produces error 5701 on real hardware.
func TestClient_SetSystemSettings_TimezoneOnlySendsFullConfig(t *testing.T) {
	state := newNTPState()
	var observed []ntpSetRequest
	c := setupNTPTestServer(t, state, &observed)

	tz := "Moscow"
	settings, err := c.SetSystemSettings(context.Background(), SetSystemSettingsRequest{Timezone: &tz})
	if err != nil {
		t.Fatalf("SetSystemSettings: %v", err)
	}

	if len(observed) != 1 {
		t.Fatalf("observed %d set calls, want 1", len(observed))
	}
	got := observed[0]

	for _, key := range ntpConfigKeys {
		if !got.has(key) {
			t.Errorf("set request dropped %q; read-modify-write must resend the whole configuration", key)
		}
	}

	// The requested change...
	if got.values["timezone"] != "Moscow" {
		t.Errorf("timezone = %q, want Moscow", got.values["timezone"])
	}
	// ...and everything the caller did not touch, carried over from `get`.
	if got.values["enable_ntp"] != "ntp" {
		t.Errorf("enable_ntp = %q, want the value get reported (ntp)", got.values["enable_ntp"])
	}
	if got.values["server"] != "time.google.com" {
		t.Errorf("server = %q, want time.google.com", got.values["server"])
	}

	if settings.Timezone != "Moscow" {
		t.Errorf("returned Timezone = %q, want Moscow", settings.Timezone)
	}
	if settings.NTPServer != "time.google.com" {
		t.Errorf("returned NTPServer = %q, want it preserved", settings.NTPServer)
	}
}

// TestClient_SetSystemSettings_OmitsClockFields guards the distinction between
// configuring the time service and setting the clock. `get` hands back
// date/hour/minute/second/now/timestamp, but echoing those into `set` is the
// manual time-setting path — a config change must not move the NAS clock.
func TestClient_SetSystemSettings_OmitsClockFields(t *testing.T) {
	state := newNTPState()
	var observed []ntpSetRequest
	c := setupNTPTestServer(t, state, &observed)

	tz := "Moscow"
	if _, err := c.SetSystemSettings(context.Background(), SetSystemSettingsRequest{Timezone: &tz}); err != nil {
		t.Fatalf("SetSystemSettings: %v", err)
	}

	for _, key := range ntpClockKeys {
		if observed[0].has(key) {
			t.Errorf("set request carried clock field %q=%q; that is the manual time-setting path, "+
				"not a configuration change", key, observed[0].values[key])
		}
	}

	// And nothing beyond the three configuration fields at all.
	if len(observed[0].keys) != len(ntpConfigKeys) {
		t.Errorf("set request carried %v, want exactly %v", observed[0].keys, ntpConfigKeys)
	}
	// In particular not ntp_server, which does not exist on this API.
	if observed[0].has("ntp_server") {
		t.Error("set request carried ntp_server; the parameter is called server")
	}
}

// TestClient_SetSystemSettings_UsesPostWithSessionInQuery pins the transport
// contract: DSM validates the session from the URL, not the POST body.
func TestClient_SetSystemSettings_UsesPostWithSessionInQuery(t *testing.T) {
	state := newNTPState()
	var observed []ntpSetRequest
	c := setupNTPTestServer(t, state, &observed)

	tz := "Moscow"
	if _, err := c.SetSystemSettings(context.Background(), SetSystemSettingsRequest{Timezone: &tz}); err != nil {
		t.Fatalf("SetSystemSettings: %v", err)
	}

	if observed[0].method != http.MethodPost {
		t.Errorf("set used %s, want POST", observed[0].method)
	}
	if observed[0].sid != "test-sid" {
		t.Errorf("_sid in query = %q, want test-sid", observed[0].sid)
	}
}

// TestClient_SetSystemSettings_FallsBackToGet covers the hedge against the one
// thing no source could settle: whether this API's `set` wants POST or GET. A
// firmware that answers 105 to the POST must still get the change applied.
func TestClient_SetSystemSettings_FallsBackToGet(t *testing.T) {
	var methods []string
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = r.ParseForm()
		}
		method := r.FormValue("method")
		if method == "" {
			method = r.URL.Query().Get("method")
		}

		if method == "get" {
			raw, _ := json.Marshal(newNTPState().snapshot())
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})
			return
		}

		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()

		if r.Method == http.MethodPost {
			_ = json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 105}})
			return
		}
		if r.URL.Query().Get("timezone") != "Moscow" {
			t.Errorf("GET fallback lost the payload: timezone = %q", r.URL.Query().Get("timezone"))
		}
		_ = json.NewEncoder(w).Encode(APIResponse{Success: true})
	}))
	defer server.Close()

	c := NewClient(server.URL, "admin", "", false)
	c.setSession("test-sid", "test-token")

	tz := "Moscow"
	if _, err := c.SetSystemSettings(context.Background(), SetSystemSettingsRequest{Timezone: &tz}); err != nil {
		t.Fatalf("SetSystemSettings: %v", err)
	}

	if len(methods) != 2 || methods[0] != http.MethodPost || methods[1] != http.MethodGet {
		t.Errorf("set attempts = %v, want [POST GET]", methods)
	}
}

// TestClient_SetSystemSettings_DoesNotFallBackOn5701 is the other half: 5701 is
// a verdict on the payload, so retrying it over GET would only produce a second
// identical failure and a misleading error.
func TestClient_SetSystemSettings_DoesNotFallBackOn5701(t *testing.T) {
	var attempts int
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = r.ParseForm()
		}
		method := r.FormValue("method")
		if method == "" {
			method = r.URL.Query().Get("method")
		}
		if method == "get" {
			raw, _ := json.Marshal(newNTPState().snapshot())
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})
			return
		}

		mu.Lock()
		attempts++
		mu.Unlock()
		_, _ = w.Write([]byte(`{"success":false,"error":{"code":5701,"errors":{"desc":"parameter bad"}}}`))
	}))
	defer server.Close()

	c := NewClient(server.URL, "admin", "", false)
	c.setSession("test-sid", "test-token")

	tz := "Nowhere"
	_, err := c.SetSystemSettings(context.Background(), SetSystemSettingsRequest{Timezone: &tz})
	if err == nil {
		t.Fatal("expected an error")
	}
	if attempts != 1 {
		t.Errorf("set attempted %d times, want 1: 5701 must not trigger the verb fallback", attempts)
	}
	if !IsAPIError(err, 5701) {
		t.Fatalf("expected a typed 5701 error, got %v", err)
	}
	if !strings.Contains(err.Error(), "5701") {
		t.Errorf("rendered error should mention the code, got: %v", err)
	}
	if !strings.Contains(err.Error(), "full parameter set") {
		t.Errorf("rendered error should explain the cause, got: %v", err)
	}
}

func TestClient_SetSystemSettings_EnableNTPUsesConfirmedValue(t *testing.T) {
	state := newNTPState()
	state.enableNTP = "manual"
	var observed []ntpSetRequest
	c := setupNTPTestServer(t, state, &observed)

	enabled := true
	settings, err := c.SetSystemSettings(context.Background(), SetSystemSettingsRequest{NTPEnabled: &enabled})
	if err != nil {
		t.Fatalf("SetSystemSettings: %v", err)
	}

	if observed[0].values["enable_ntp"] != "ntp" {
		t.Errorf("enable_ntp = %q, want ntp", observed[0].values["enable_ntp"])
	}
	if !settings.NTPEnabled {
		t.Error("returned NTPEnabled = false, want true")
	}
}

// TestClient_SetSystemSettings_DisableReusesDSMsOwnValue is the point of keeping
// the raw mode string: when the NAS has already been in the disabled state, DSM
// has told us what it calls it, and that beats any guess.
func TestClient_SetSystemSettings_DisableReusesDSMsOwnValue(t *testing.T) {
	state := newNTPState()
	state.enableNTP = "off" // whatever DSM happens to call it
	var observed []ntpSetRequest
	c := setupNTPTestServer(t, state, &observed)

	enabled := false
	if _, err := c.SetSystemSettings(context.Background(), SetSystemSettingsRequest{NTPEnabled: &enabled}); err != nil {
		t.Fatalf("SetSystemSettings: %v", err)
	}

	if observed[0].values["enable_ntp"] != "off" {
		t.Errorf("enable_ntp = %q, want DSM's own disabled value (off)", observed[0].values["enable_ntp"])
	}
}

func TestClient_SetSystemSettings_DisableFromEnabledUsesFallback(t *testing.T) {
	state := newNTPState()
	var observed []ntpSetRequest
	c := setupNTPTestServer(t, state, &observed)

	enabled := false
	if _, err := c.SetSystemSettings(context.Background(), SetSystemSettingsRequest{NTPEnabled: &enabled}); err != nil {
		t.Fatalf("SetSystemSettings: %v", err)
	}

	if observed[0].values["enable_ntp"] != "manual" {
		t.Errorf("enable_ntp = %q, want the inferred fallback (manual)", observed[0].values["enable_ntp"])
	}
	// Turning synchronisation off must not blank the configured server or zone.
	if observed[0].values["server"] != "time.google.com" {
		t.Errorf("server = %q, want it preserved", observed[0].values["server"])
	}
	if observed[0].values["timezone"] != "Amman" {
		t.Errorf("timezone = %q, want it preserved", observed[0].values["timezone"])
	}
}

func TestClient_SetSystemSettings_ServerOnly(t *testing.T) {
	state := newNTPState()
	var observed []ntpSetRequest
	c := setupNTPTestServer(t, state, &observed)

	server := "pool.ntp.org"
	settings, err := c.SetSystemSettings(context.Background(), SetSystemSettingsRequest{NTPServer: &server})
	if err != nil {
		t.Fatalf("SetSystemSettings: %v", err)
	}

	if observed[0].values["server"] != "pool.ntp.org" {
		t.Errorf("server = %q, want pool.ntp.org", observed[0].values["server"])
	}
	if observed[0].values["timezone"] != "Amman" {
		t.Errorf("timezone = %q, want it preserved", observed[0].values["timezone"])
	}
	if settings.NTPServer != "pool.ntp.org" {
		t.Errorf("returned NTPServer = %q, want pool.ntp.org", settings.NTPServer)
	}
}

// TestClient_SetSystemSettings_NoChangesStillRoundTrips covers the degenerate
// case: an empty request must still be a well-formed full write, not a partial
// one that DSM would reject.
func TestClient_SetSystemSettings_NoChangesStillRoundTrips(t *testing.T) {
	state := newNTPState()
	var observed []ntpSetRequest
	c := setupNTPTestServer(t, state, &observed)

	if _, err := c.SetSystemSettings(context.Background(), SetSystemSettingsRequest{}); err != nil {
		t.Fatalf("SetSystemSettings: %v", err)
	}

	for _, key := range ntpConfigKeys {
		if !observed[0].has(key) {
			t.Errorf("set request dropped %q", key)
		}
	}
	if observed[0].values["timezone"] != "Amman" {
		t.Errorf("timezone = %q, want Amman unchanged", observed[0].values["timezone"])
	}
	if observed[0].values["enable_ntp"] != "ntp" {
		t.Errorf("enable_ntp = %q, want ntp unchanged", observed[0].values["enable_ntp"])
	}
}

// TestClient_SetSystemSettings_ConcurrentWritesDoNotClobber guards the lock
// around the read-modify-write. Without it, two callers changing different
// fields both read the old state and the loser's change disappears.
func TestClient_SetSystemSettings_ConcurrentWritesDoNotClobber(t *testing.T) {
	state := newNTPState()
	var observed []ntpSetRequest
	c := setupNTPTestServer(t, state, &observed)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		tz := "Moscow"
		if _, err := c.SetSystemSettings(context.Background(), SetSystemSettingsRequest{Timezone: &tz}); err != nil {
			t.Errorf("set timezone: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		srv := "pool.ntp.org"
		if _, err := c.SetSystemSettings(context.Background(), SetSystemSettingsRequest{NTPServer: &srv}); err != nil {
			t.Errorf("set server: %v", err)
		}
	}()

	wg.Wait()

	final, err := c.GetSystemSettings(context.Background())
	if err != nil {
		t.Fatalf("GetSystemSettings: %v", err)
	}
	if final.Timezone != "Moscow" {
		t.Errorf("Timezone = %q, want Moscow: a concurrent write clobbered it", final.Timezone)
	}
	if final.NTPServer != "pool.ntp.org" {
		t.Errorf("NTPServer = %q, want pool.ntp.org: a concurrent write clobbered it", final.NTPServer)
	}
}

func TestClient_ListTimezones(t *testing.T) {
	state := newNTPState()
	var observed []ntpSetRequest
	c := setupNTPTestServer(t, state, &observed)

	zones, err := c.ListTimezones(context.Background())
	if err != nil {
		t.Fatalf("ListTimezones: %v", err)
	}

	if len(zones) != 4 {
		t.Fatalf("got %d zones, want 4", len(zones))
	}
	if zones[1].Value != "Moscow" || zones[1].Offset != 10800 {
		t.Errorf("zones[1] = %+v, want {Moscow 10800}", zones[1])
	}
	if zones[3].Offset != -18000 {
		t.Errorf("negative offsets must survive: got %d", zones[3].Offset)
	}
}

func TestParseTimezones(t *testing.T) {
	t.Run("skips entries without a name", func(t *testing.T) {
		zones, err := parseTimezones(json.RawMessage(`{"zonedata":[{"value":"","offset":0},{"value":"Moscow","offset":10800}]}`))
		if err != nil {
			t.Fatalf("parseTimezones: %v", err)
		}
		if len(zones) != 1 || zones[0].Value != "Moscow" {
			t.Errorf("got %+v, want only Moscow", zones)
		}
	})

	t.Run("missing zonedata is empty, not an error", func(t *testing.T) {
		zones, err := parseTimezones(json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("parseTimezones: %v", err)
		}
		if len(zones) != 0 {
			t.Errorf("got %+v, want none", zones)
		}
	})

	t.Run("malformed", func(t *testing.T) {
		if _, err := parseTimezones(json.RawMessage(`not json`)); err == nil {
			t.Error("expected an error")
		}
	})
}

func TestResolveNTPMode(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }

	tests := []struct {
		name    string
		current SystemSettings
		want    *bool
		expect  string
	}{
		{
			name:    "no change echoes DSM's own value",
			current: SystemSettings{NTPMode: "ntp", NTPEnabled: true},
			want:    nil,
			expect:  "ntp",
		},
		{
			name:    "no change echoes an unfamiliar disabled value too",
			current: SystemSettings{NTPMode: "whatever", NTPEnabled: false},
			want:    nil,
			expect:  "whatever",
		},
		{
			name:    "no change with nothing reported falls back on the flag",
			current: SystemSettings{NTPMode: "", NTPEnabled: true},
			want:    nil,
			expect:  "ntp",
		},
		{
			name:    "enabling uses the confirmed value",
			current: SystemSettings{NTPMode: "manual", NTPEnabled: false},
			want:    boolPtr(true),
			expect:  "ntp",
		},
		{
			name:    "disabling an already-disabled NAS reuses its value",
			current: SystemSettings{NTPMode: "off", NTPEnabled: false},
			want:    boolPtr(false),
			expect:  "off",
		},
		{
			name:    "disabling an enabled NAS needs the fallback",
			current: SystemSettings{NTPMode: "ntp", NTPEnabled: true},
			want:    boolPtr(false),
			expect:  "manual",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveNTPMode(tt.current, tt.want); got != tt.expect {
				t.Errorf("resolveNTPMode = %q, want %q", got, tt.expect)
			}
		})
	}
}

func TestParseNTPEnabled(t *testing.T) {
	tests := []struct {
		name  string
		value interface{}
		want  bool
	}{
		{"DSM 7.4 string mode", "ntp", true},
		{"manual mode", "manual", false},
		{"empty string", "", false},
		{"bool true", true, true},
		{"bool false", false, false},
		{"numeric truthy", float64(1), true},
		{"numeric zero", float64(0), false},
		{"unexpected type", []interface{}{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseNTPEnabled(tt.value); got != tt.want {
				t.Errorf("parseNTPEnabled(%v) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestParseSystemSettings(t *testing.T) {
	t.Run("real DSM 7.4 payload", func(t *testing.T) {
		raw := json.RawMessage(`{"date":"2026/8/12","enable_ntp":"ntp","hour":16,"minute":8,
			"now":"Wed Aug 12 16:08:34 2026\n","second":34,"server":"time.google.com",
			"timestamp":1786540114,"timezone":"Amman"}`)

		s, err := parseSystemSettings(raw)
		if err != nil {
			t.Fatalf("parseSystemSettings: %v", err)
		}
		if s.Timezone != "Amman" || !s.NTPEnabled || s.NTPServer != "time.google.com" {
			t.Errorf("unexpected parse: %+v", s)
		}
		if s.Now != "Wed Aug 12 16:08:34 2026\n" {
			t.Errorf("Now = %q", s.Now)
		}
	})

	t.Run("unknown mode string is kept verbatim and read as disabled", func(t *testing.T) {
		s, err := parseSystemSettings(json.RawMessage(`{"enable_ntp":"something_else"}`))
		if err != nil {
			t.Fatalf("parseSystemSettings: %v", err)
		}
		if s.NTPEnabled {
			t.Error("NTPEnabled = true, want false for an unrecognised mode")
		}
		if s.NTPMode != "something_else" {
			t.Errorf("NTPMode = %q, want it preserved verbatim", s.NTPMode)
		}
	})

	t.Run("empty object", func(t *testing.T) {
		s, err := parseSystemSettings(json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("parseSystemSettings: %v", err)
		}
		if s.Timezone != "" || s.NTPEnabled || s.NTPServer != "" || s.NTPMode != "" {
			t.Errorf("expected zero values, got %+v", s)
		}
	})

	t.Run("malformed", func(t *testing.T) {
		if _, err := parseSystemSettings(json.RawMessage(`not json`)); err == nil {
			t.Error("expected an error")
		}
	})
}

func TestSystemSettingsParams(t *testing.T) {
	params := systemSettingsParams(SystemSettings{
		Timezone:  "Moscow",
		NTPMode:   "ntp",
		NTPServer: "time.google.com",
		// Clock readings that must not leak into the request.
		Date:      "2026/8/12",
		Hour:      16,
		Minute:    8,
		Second:    34,
		Timestamp: 1786540114,
	})

	want := map[string]string{
		"timezone":   "Moscow",
		"enable_ntp": "ntp",
		"server":     "time.google.com",
	}
	for key, value := range want {
		if got := params.Get(key); got != value {
			t.Errorf("params[%q] = %q, want %q", key, got, value)
		}
	}
	if len(params) != len(want) {
		t.Errorf("params carries %d keys (%v), want exactly %d", len(params), params, len(want))
	}
}
