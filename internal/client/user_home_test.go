package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// userHomeState is the mock DSM's view of the user home service. It mirrors the
// real payload observed on virtual DSM (see .pi/recon-user-home-2026-08-07.md).
type userHomeState struct {
	mu               sync.Mutex
	enable           bool
	location         string
	enableRecycleBin bool
}

func (s *userHomeState) snapshot() map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]interface{}{
		"enable":             s.enable,
		"location":           s.location,
		"enable_recycle_bin": s.enableRecycleBin,
		"enable_domain":      false,
		"enable_ldap":        false,
		"encryption":         float64(0),
		"remote_location":    "",
		"userhome_in_s2s":    false,
	}
}

// setRequest records the parameters of a single observed "set" call.
type setRequest struct {
	present map[string]bool
	values  map[string]string
	sidInQuery
}

type sidInQuery struct {
	sid   string
	token string
}

// setupUserHomeTestServer builds a mock DSM implementing get/set for
// SYNO.Core.User.Home. Observed set calls are appended to *observed.
func setupUserHomeTestServer(t *testing.T, state *userHomeState, observed *[]setRequest) (*Client, *httptest.Server) {
	t.Helper()

	var mu sync.Mutex
	mux := http.NewServeMux()

	mux.HandleFunc("/webapi/entry.cgi", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = r.ParseForm()
		}
		api := r.FormValue("api")
		if api == "" {
			api = r.URL.Query().Get("api")
		}
		method := r.FormValue("method")
		if method == "" {
			method = r.URL.Query().Get("method")
		}

		switch {
		case api == "SYNO.Core.User.Home" && method == "get":
			data := state.snapshot()
			if additional := r.URL.Query().Get("additional"); strings.Contains(additional, "personal_photo_enable") {
				data["additional"] = map[string]interface{}{"personal_photo_enable": true}
			}
			raw, _ := json.Marshal(data)
			json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		case api == "SYNO.Core.User.Home" && method == "set":
			req := setRequest{
				present: map[string]bool{},
				values:  map[string]string{},
				sidInQuery: sidInQuery{
					sid:   r.URL.Query().Get("_sid"),
					token: r.URL.Query().Get("SynoToken"),
				},
			}
			for _, key := range []string{"enable", "location", "enable_recycle_bin", "force"} {
				if _, ok := r.PostForm[key]; ok {
					req.present[key] = true
					req.values[key] = r.PostFormValue(key)
				}
			}
			mu.Lock()
			*observed = append(*observed, req)
			mu.Unlock()

			// Mirror the real API: location is mandatory when enabling, and it
			// must be a path, not a bare volume name.
			enable := req.values["enable"] == "true"
			if enable && !req.present["location"] {
				json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 3103}})
				return
			}
			if enable && !strings.HasPrefix(req.values["location"], "/") {
				json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 3101}})
				return
			}

			state.mu.Lock()
			state.enable = enable
			if enable {
				state.location = req.values["location"]
				state.enableRecycleBin = req.values["enable_recycle_bin"] == "true"
			}
			state.mu.Unlock()

			json.NewEncoder(w).Encode(APIResponse{Success: true})

		default:
			json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 101}})
		}
	})

	server := httptest.NewServer(mux)
	client := NewClient(server.URL, "admin", "password", false)
	client.setSession("test-sid", "test-token")

	return client, server
}

func TestClient_GetUserHomeService(t *testing.T) {
	state := &userHomeState{enable: true, location: "/volume1", enableRecycleBin: true}
	var observed []setRequest
	client, server := setupUserHomeTestServer(t, state, &observed)
	defer server.Close()

	svc, err := client.GetUserHomeService(context.Background())
	if err != nil {
		t.Fatalf("GetUserHomeService failed: %v", err)
	}
	if !svc.Enable {
		t.Error("expected Enable true")
	}
	if svc.Location != "/volume1" {
		t.Errorf("expected Location /volume1, got %q", svc.Location)
	}
	if !svc.EnableRecycleBin {
		t.Error("expected EnableRecycleBin true")
	}
	// personal_photo_enable lives under "additional" and is only returned when
	// requested — GetUserHomeService always asks for it.
	if !svc.PersonalPhotoEnable {
		t.Error("expected PersonalPhotoEnable true from the additional block")
	}
}

func TestClient_GetUserHomeService_Disabled(t *testing.T) {
	state := &userHomeState{enable: false, location: ""}
	var observed []setRequest
	client, server := setupUserHomeTestServer(t, state, &observed)
	defer server.Close()

	svc, err := client.GetUserHomeService(context.Background())
	if err != nil {
		t.Fatalf("GetUserHomeService failed: %v", err)
	}
	if svc.Enable {
		t.Error("expected Enable false")
	}
	if svc.Location != "" {
		t.Errorf("expected empty Location, got %q", svc.Location)
	}
}

func TestClient_SetUserHomeService_Enable(t *testing.T) {
	state := &userHomeState{}
	var observed []setRequest
	client, server := setupUserHomeTestServer(t, state, &observed)
	defer server.Close()

	svc, err := client.SetUserHomeService(context.Background(), SetUserHomeServiceRequest{
		Enable:           true,
		Location:         "/volume1",
		EnableRecycleBin: true,
	})
	if err != nil {
		t.Fatalf("SetUserHomeService failed: %v", err)
	}
	if !svc.Enable || svc.Location != "/volume1" || !svc.EnableRecycleBin {
		t.Fatalf("state not applied: %+v", svc)
	}

	if len(observed) != 1 {
		t.Fatalf("expected exactly 1 set call, got %d", len(observed))
	}
	got := observed[0]
	if got.values["enable"] != "true" {
		t.Errorf("expected enable=true, got %q", got.values["enable"])
	}
	if got.values["location"] != "/volume1" {
		t.Errorf("expected location=/volume1, got %q", got.values["location"])
	}
	if got.values["enable_recycle_bin"] != "true" {
		t.Errorf("expected enable_recycle_bin=true, got %q", got.values["enable_recycle_bin"])
	}
	// DSM validates the session from the query string on POST, not the body.
	if got.sid != "test-sid" || got.token != "test-token" {
		t.Errorf("expected _sid/SynoToken in query string, got sid=%q token=%q", got.sid, got.token)
	}
}

// TestClient_SetUserHomeService_DisableOmitsLocation pins the disable payload:
// passing location (or other fields) alongside enable=false has been observed to
// trigger error 3103 on real DSM, so the client must send enable only.
func TestClient_SetUserHomeService_DisableOmitsLocation(t *testing.T) {
	state := &userHomeState{enable: true, location: "/volume1"}
	var observed []setRequest
	client, server := setupUserHomeTestServer(t, state, &observed)
	defer server.Close()

	svc, err := client.SetUserHomeService(context.Background(), SetUserHomeServiceRequest{
		Enable:   false,
		Location: "/volume1", // must be ignored by the client
	})
	if err != nil {
		t.Fatalf("SetUserHomeService failed: %v", err)
	}
	if svc.Enable {
		t.Error("expected service disabled")
	}

	got := observed[0]
	if got.values["enable"] != "false" {
		t.Errorf("expected enable=false, got %q", got.values["enable"])
	}
	for _, key := range []string{"location", "enable_recycle_bin", "force"} {
		if got.present[key] {
			t.Errorf("%s must not be sent when disabling, got %q", key, got.values[key])
		}
	}
}

func TestClient_SetUserHomeService_ForceFlag(t *testing.T) {
	tests := []struct {
		name      string
		force     bool
		wantSent  bool
		wantValue string
	}{
		{name: "force true is sent", force: true, wantSent: true, wantValue: "true"},
		{name: "force false is omitted", force: false, wantSent: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &userHomeState{}
			var observed []setRequest
			client, server := setupUserHomeTestServer(t, state, &observed)
			defer server.Close()

			_, err := client.SetUserHomeService(context.Background(), SetUserHomeServiceRequest{
				Enable:   true,
				Location: "/volume1",
				Force:    tt.force,
			})
			if err != nil {
				t.Fatalf("SetUserHomeService failed: %v", err)
			}
			if got := observed[0].present["force"]; got != tt.wantSent {
				t.Fatalf("force sent = %v, want %v", got, tt.wantSent)
			}
			if tt.wantSent && observed[0].values["force"] != tt.wantValue {
				t.Errorf("expected force=%s, got %q", tt.wantValue, observed[0].values["force"])
			}
		})
	}
}

// TestClient_SetUserHomeService_MissingLocation reproduces error 3103: enabling
// without a location.
func TestClient_SetUserHomeService_MissingLocation(t *testing.T) {
	state := &userHomeState{}
	var observed []setRequest
	client, server := setupUserHomeTestServer(t, state, &observed)
	defer server.Close()

	_, err := client.SetUserHomeService(context.Background(), SetUserHomeServiceRequest{Enable: true})
	if err == nil {
		t.Fatal("expected error 3103 when enabling without location")
	}
	if !strings.Contains(err.Error(), "3103") {
		t.Fatalf("expected 3103 in error, got: %v", err)
	}
}

// TestClient_SetUserHomeService_BareVolumeRejected pins the format of location:
// DSM wants the path "/volume1"; a bare "volume1" yields 3101. The client passes
// the value through, so this guards the documented contract of the field.
func TestClient_SetUserHomeService_BareVolumeRejected(t *testing.T) {
	state := &userHomeState{}
	var observed []setRequest
	client, server := setupUserHomeTestServer(t, state, &observed)
	defer server.Close()

	_, err := client.SetUserHomeService(context.Background(), SetUserHomeServiceRequest{
		Enable:   true,
		Location: "volume1",
	})
	if err == nil {
		t.Fatal("expected error 3101 for a bare volume name")
	}
	if !strings.Contains(err.Error(), "3101") {
		t.Fatalf("expected 3101 in error, got: %v", err)
	}
}

// setupAsyncUserHomeServer mocks the asynchronous variant: set returns a task_id
// and status reports finish=false until finishAfter polls have elapsed.
func setupAsyncUserHomeServer(t *testing.T, finishAfter int64, polls *atomic.Int64) (*Client, *httptest.Server) {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/webapi/entry.cgi", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = r.ParseForm()
		}
		api := r.FormValue("api")
		if api == "" {
			api = r.URL.Query().Get("api")
		}
		method := r.FormValue("method")
		if method == "" {
			method = r.URL.Query().Get("method")
		}

		switch {
		case api == "SYNO.Core.User.Home" && method == "set":
			json.NewEncoder(w).Encode(APIResponse{
				Success: true,
				Data:    json.RawMessage(`{"task_id":"userhome_admin"}`),
			})

		case api == "SYNO.Core.User.Home" && method == "status":
			if r.URL.Query().Get("task_id") == "" {
				json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 114}})
				return
			}
			finish := polls.Add(1) >= finishAfter
			raw, _ := json.Marshal(map[string]interface{}{"finish": finish})
			json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		case api == "SYNO.Core.User.Home" && method == "get":
			raw, _ := json.Marshal(map[string]interface{}{
				"enable": true, "location": "/volume1", "enable_recycle_bin": false,
			})
			json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		default:
			json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 101}})
		}
	})

	server := httptest.NewServer(mux)
	client := NewClient(server.URL, "admin", "password", false)
	client.setSession("test-sid", "test-token")
	return client, server
}

// TestClient_SetUserHomeService_AsyncTask covers the physical-NAS path: set
// hands back a task_id and the client must poll status until finish=true before
// reporting success.
func TestClient_SetUserHomeService_AsyncTask(t *testing.T) {
	restore := shrinkUserHomePolling(t)
	defer restore()

	var polls atomic.Int64
	client, server := setupAsyncUserHomeServer(t, 3, &polls)
	defer server.Close()

	svc, err := client.SetUserHomeService(context.Background(), SetUserHomeServiceRequest{
		Enable:   true,
		Location: "/volume1",
	})
	if err != nil {
		t.Fatalf("SetUserHomeService failed: %v", err)
	}
	if !svc.Enable {
		t.Error("expected Enable true after the task finished")
	}
	if got := polls.Load(); got != 3 {
		t.Errorf("expected 3 status polls before finish, got %d", got)
	}
}

// TestClient_SetUserHomeService_AsyncTimeout proves the poll loop is bounded:
// a task that never finishes fails instead of hanging forever.
func TestClient_SetUserHomeService_AsyncTimeout(t *testing.T) {
	restore := shrinkUserHomePolling(t)
	defer restore()
	userHomeTaskTimeout = 20 * time.Millisecond

	var polls atomic.Int64
	client, server := setupAsyncUserHomeServer(t, 1_000_000, &polls) // never finishes
	defer server.Close()

	_, err := client.SetUserHomeService(context.Background(), SetUserHomeServiceRequest{
		Enable:   true,
		Location: "/volume1",
	})
	if err == nil {
		t.Fatal("expected timeout error for a task that never finishes")
	}
	if !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("expected a timeout message, got: %v", err)
	}
}

// TestClient_SetUserHomeService_AsyncContextCancel proves the poll loop honours
// context cancellation (Terraform cancelling an apply).
func TestClient_SetUserHomeService_AsyncContextCancel(t *testing.T) {
	restore := shrinkUserHomePolling(t)
	defer restore()

	var polls atomic.Int64
	client, server := setupAsyncUserHomeServer(t, 1_000_000, &polls)
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(15 * time.Millisecond)
		cancel()
	}()

	_, err := client.SetUserHomeService(ctx, SetUserHomeServiceRequest{
		Enable:   true,
		Location: "/volume1",
	})
	if err == nil {
		t.Fatal("expected error after context cancellation")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("expected context cancellation, got: %v", err)
	}
}

// shrinkUserHomePolling makes the poll cadence test-fast and returns a restore
// func for the package-level knobs.
func shrinkUserHomePolling(t *testing.T) func() {
	t.Helper()
	prevInterval, prevTimeout := userHomeTaskPollInterval, userHomeTaskTimeout
	userHomeTaskPollInterval = 2 * time.Millisecond
	userHomeTaskTimeout = 5 * time.Second
	return func() {
		userHomeTaskPollInterval = prevInterval
		userHomeTaskTimeout = prevTimeout
	}
}

// TestClient_GetUserHomeService_PermissionDenied covers the 119-means-not-admin
// case: SYNO.Core.User.Home answers 119 for a non-built-in admin account, and
// the client must surface it instead of looping on re-login.
func TestClient_GetUserHomeService_PermissionDenied(t *testing.T) {
	var logins atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/webapi/entry.cgi", func(w http.ResponseWriter, r *http.Request) {
		api := r.URL.Query().Get("api")
		method := r.URL.Query().Get("method")

		if api == "SYNO.API.Auth" && method == "login" {
			logins.Add(1)
			json.NewEncoder(w).Encode(APIResponse{
				Success: true,
				Data:    json.RawMessage(`{"sid":"fresh-sid","synotoken":"fresh-token"}`),
			})
			return
		}
		json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 119}})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := NewClient(server.URL, "operator", "password", false)
	client.setSession("test-sid", "test-token")

	_, err := client.GetUserHomeService(context.Background())
	if err == nil {
		t.Fatal("expected error 119 to surface")
	}
	if !strings.Contains(err.Error(), "119") {
		t.Fatalf("expected 119 in error, got: %v", err)
	}
	// Exactly one re-login attempt, then the error is reported as is.
	if got := logins.Load(); got != 1 {
		t.Errorf("expected exactly 1 re-login attempt, got %d", got)
	}
}

func TestParseUserHomeService(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    UserHomeService
		wantErr bool
	}{
		{
			name:    "full payload as returned by DSM 7.2",
			payload: `{"enable":true,"enable_domain":true,"enable_ldap":true,"enable_recycle_bin":true,"encryption":2,"location":"/volume2","remote_location":"","userhome_in_s2s":false,"additional":{"personal_photo_enable":true}}`,
			want: UserHomeService{
				Enable: true, Location: "/volume2", EnableRecycleBin: true,
				EnableDomain: true, EnableLDAP: true, Encryption: 2, PersonalPhotoEnable: true,
			},
		},
		{
			name:    "service disabled",
			payload: `{"enable":false,"location":"","enable_recycle_bin":false,"encryption":0}`,
			want:    UserHomeService{},
		},
		{
			name:    "missing fields default to zero values",
			payload: `{}`,
			want:    UserHomeService{},
		},
		{
			name:    "unexpected field types are ignored, not fatal",
			payload: `{"enable":"yes","location":42,"encryption":"none"}`,
			want:    UserHomeService{},
		},
		{
			name:    "malformed json",
			payload: `{not json`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseUserHomeService(json.RawMessage(tt.payload))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected a parse error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseUserHomeService failed: %v", err)
			}
			if *got != tt.want {
				t.Errorf("got %+v, want %+v", *got, tt.want)
			}
		})
	}
}

func TestParseUserHomeTaskID(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{name: "async response", payload: `{"task_id":"userhome_admin"}`, want: "userhome_admin"},
		{name: "synchronous response has no task", payload: `{}`, want: ""},
		{name: "empty body", payload: ``, want: ""},
		{name: "null data", payload: `null`, want: ""},
		{name: "malformed json is not a task", payload: `{oops`, want: ""},
		{name: "wrong type is not a task", payload: `{"task_id":42}`, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseUserHomeTaskID(json.RawMessage(tt.payload)); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
