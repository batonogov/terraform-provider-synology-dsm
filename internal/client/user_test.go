package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func setupTestServer() (*Client, *httptest.Server) {
	mux := http.NewServeMux()

	mux.HandleFunc("/webapi/entry.cgi", func(w http.ResponseWriter, r *http.Request) {
		api := r.URL.Query().Get("api")
		method := r.URL.Query().Get("method")

		switch {
		case api == "SYNO.API.Auth" && method == "login":
			json.NewEncoder(w).Encode(APIResponse{
				Success: true,
				Data:    json.RawMessage(`{"sid":"test-sid"}`),
			})

		case api == "SYNO.API.Auth" && method == "logout":
			json.NewEncoder(w).Encode(APIResponse{Success: true})

		case api == "SYNO.Core.User" && method == "create":
			json.NewEncoder(w).Encode(APIResponse{Success: true})

		case api == "SYNO.Core.User" && method == "get":
			name := r.URL.Query().Get("name")
			users := []map[string]interface{}{
				{
					"name":        name,
					"description": "Test user",
					"email":       name + "@example.com",
					"disabled":    false,
					"uid":         1024,
					"groups":      []string{"users"},
				},
			}
			raw, _ := json.Marshal(map[string]interface{}{"users": users})
			json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		case api == "SYNO.Core.User" && method == "list":
			users := []map[string]interface{}{
				{"name": "admin", "uid": 1024, "description": "Administrator"},
				{"name": "john", "uid": 1025, "description": "John Doe"},
			}
			raw, _ := json.Marshal(map[string]interface{}{"users": users, "total": len(users)})
			json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		// Mirrors the real API: there is no "update" method, only "set".
		case api == "SYNO.Core.User" && method == "set":
			json.NewEncoder(w).Encode(APIResponse{Success: true})

		case api == "SYNO.Core.User" && method == "update":
			json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 103}})

		case api == "SYNO.Core.User" && method == "delete":
			json.NewEncoder(w).Encode(APIResponse{Success: true})

		default:
			json.NewEncoder(w).Encode(APIResponse{
				Success: false,
				Error:   &APIError{Code: 101},
			})
		}
	})

	server := httptest.NewServer(mux)
	client := NewClient(server.URL, "admin", "password", false)
	client.setSession("test-sid", "")

	return client, server
}

func TestClient_CreateUser(t *testing.T) {
	client, server := setupTestServer()
	defer server.Close()

	user, err := client.CreateUser(context.Background(), CreateUserRequest{
		Name:        "john",
		Password:    "secret123",
		Description: "John Doe",
		Email:       "john@example.com",
	})
	if err != nil {
		t.Fatalf("CreateUser failed: %v", err)
	}
	if user.Name != "john" {
		t.Errorf("expected name john, got %q", user.Name)
	}
}

func TestClient_GetUser(t *testing.T) {
	client, server := setupTestServer()
	defer server.Close()

	user, err := client.GetUser(context.Background(), "admin")
	if err != nil {
		t.Fatalf("GetUser failed: %v", err)
	}
	if user.Name != "admin" {
		t.Errorf("expected name admin, got %q", user.Name)
	}
	if user.UID != 1024 {
		t.Errorf("expected UID 1024, got %d", user.UID)
	}
}

func TestClient_ListUsers(t *testing.T) {
	client, server := setupTestServer()
	defer server.Close()

	users, err := client.ListUsers(context.Background())
	if err != nil {
		t.Fatalf("ListUsers failed: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(users))
	}
	if users[0].Name != "admin" {
		t.Errorf("expected first user admin, got %q", users[0].Name)
	}
}

func TestClient_UpdateUser(t *testing.T) {
	client, server := setupTestServer()
	defer server.Close()

	disabled := true
	user, err := client.UpdateUser(context.Background(), "john", UpdateUserRequest{
		Description: "Updated",
		Disabled:    &disabled,
	})
	if err != nil {
		t.Fatalf("UpdateUser failed: %v", err)
	}
	if user.Name != "john" {
		t.Errorf("expected name john, got %q", user.Name)
	}
}

func TestClient_DeleteUser(t *testing.T) {
	client, server := setupTestServer()
	defer server.Close()

	if err := client.DeleteUser(context.Background(), "john"); err != nil {
		t.Fatalf("DeleteUser failed: %v", err)
	}
}

// TestParseUser_GroupsAsObjects verifies that parseUser accepts the real DSM
// 7.2/7.3 format where "groups" is an array of objects {name, ...} rather
// than an array of plain strings. Previously only strings were handled, which
// silently dropped all group membership on refresh (issue M5).
func TestParseUser_GroupsAsObjects(t *testing.T) {
	raw := json.RawMessage(`{
		"name": "john",
		"uid": 1025,
		"description": "John Doe",
		"groups": [
			{"name": "administrators", "inherited": false},
			{"name": "users", "inherited": true}
		]
	}`)
	u, err := parseUser(raw)
	if err != nil {
		t.Fatalf("parseUser: %v", err)
	}
	want := []string{"administrators", "users"}
	if len(u.Groups) != len(want) {
		t.Fatalf("expected %d groups, got %d: %v", len(want), len(u.Groups), u.Groups)
	}
	for i, g := range want {
		if u.Groups[i] != g {
			t.Errorf("group[%d] = %q, want %q", i, u.Groups[i], g)
		}
	}
}

// TestParseUser_GroupsMixed verifies both string and object entries are parsed.
func TestParseUser_GroupsMixed(t *testing.T) {
	raw := json.RawMessage(`{
		"name": "jane",
		"groups": ["string-group", {"name": "object-group"}]
	}`)
	u, err := parseUser(raw)
	if err != nil {
		t.Fatalf("parseUser: %v", err)
	}
	if len(u.Groups) != 2 {
		t.Fatalf("expected 2 groups (mixed), got %d: %v", len(u.Groups), u.Groups)
	}
}

// TestParseUser_GroupsEmpty verifies empty array does not error and yields no groups.
func TestParseUser_GroupsEmpty(t *testing.T) {
	raw := json.RawMessage(`{"name":"bob","groups":[]}`)
	u, err := parseUser(raw)
	if err != nil {
		t.Fatalf("parseUser: %v", err)
	}
	if len(u.Groups) != 0 {
		t.Errorf("expected 0 groups, got %d", len(u.Groups))
	}
}

// TestParseUser_ObjectWithoutName verifies a group object missing "name" is skipped, not panic.
func TestParseUser_GroupsWithoutName(t *testing.T) {
	raw := json.RawMessage(`{"name":"bob","groups":[{"gid":100}]}`)
	u, err := parseUser(raw)
	if err != nil {
		t.Fatalf("parseUser: %v", err)
	}
	if len(u.Groups) != 0 {
		t.Errorf("expected 0 groups when none have name, got %d", len(u.Groups))
	}
}

// expiredCapture records the "expired" parameter of the last create/set call,
// which is how DSM actually stores account state.
type expiredCapture struct {
	mu     sync.Mutex
	last   string
	method string
	seen   bool
}

func (c *expiredCapture) record(method, expired string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.method = method
	c.last = expired
	c.seen = true
}

func (c *expiredCapture) get() (method, expired string, seen bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.method, c.last, c.seen
}

// setupExpiredCaptureServer answers create/set/list and records what account
// state was requested. list echoes it back so a round trip can be asserted.
func setupExpiredCaptureServer(t *testing.T) (*Client, *httptest.Server, *expiredCapture) {
	t.Helper()

	capture := &expiredCapture{}
	state := struct {
		mu      sync.Mutex
		expired string
	}{expired: "normal"}

	mux := http.NewServeMux()
	mux.HandleFunc("/webapi/entry.cgi", func(w http.ResponseWriter, r *http.Request) {
		api := r.URL.Query().Get("api")
		method := r.URL.Query().Get("method")

		switch {
		case api == "SYNO.Core.User" && (method == "create" || method == "set"):
			if expired := r.URL.Query().Get("expired"); expired != "" {
				capture.record(method, expired)
				state.mu.Lock()
				state.expired = expired
				state.mu.Unlock()
			}
			json.NewEncoder(w).Encode(APIResponse{Success: true})

		case api == "SYNO.Core.User" && method == "list":
			state.mu.Lock()
			expired := state.expired
			state.mu.Unlock()
			// DSM always answers without leading zeros, whatever was written.
			if t, err := time.Parse("2006/1/2", expired); err == nil {
				expired = fmt.Sprintf("%d/%d/%d", t.Year(), int(t.Month()), t.Day())
			}
			users := []map[string]interface{}{
				{"name": "john", "uid": 1025, "expired": expired, "2fa_status": true},
			}
			raw, _ := json.Marshal(map[string]interface{}{"users": users, "total": 1})
			json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		default:
			json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 101}})
		}
	})

	server := httptest.NewServer(mux)
	client := NewClient(server.URL, "admin", "password", false)
	client.setSession("test-sid", "")
	return client, server, capture
}

// TestClient_CreateUser_AccountState is the regression guard for the disabled
// flag, which used to be sent as "disabled" — a parameter DSM 7 accepts and
// ignores, so disabled users were created active.
func TestClient_CreateUser_AccountState(t *testing.T) {
	tests := []struct {
		name        string
		disabled    bool
		expireDate  string
		wantExpired string
	}{
		{name: "active account", wantExpired: "normal"},
		{name: "disabled account", disabled: true, wantExpired: "now"},
		{name: "expiry date is converted to DSM format", expireDate: "2027-03-05", wantExpired: "2027/3/5"},
		{name: "disabled wins over a date", disabled: true, expireDate: "2027-03-05", wantExpired: "now"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, server, capture := setupExpiredCaptureServer(t)
			defer server.Close()

			_, err := client.CreateUser(context.Background(), CreateUserRequest{
				Name:       "john",
				Password:   "secret",
				Disabled:   tt.disabled,
				ExpireDate: tt.expireDate,
			})
			if err != nil {
				t.Fatalf("CreateUser failed: %v", err)
			}

			_, expired, seen := capture.get()
			if !seen {
				t.Fatal("expected an expired parameter to be sent")
			}
			if expired != tt.wantExpired {
				t.Errorf("expired = %q, want %q", expired, tt.wantExpired)
			}
		})
	}
}

// TestClient_UpdateUser_UsesSetMethod guards the second half of the same bug:
// SYNO.Core.User has no "update" method (it answers 103), so updates built that
// way silently did nothing.
func TestClient_UpdateUser_UsesSetMethod(t *testing.T) {
	client, server, capture := setupExpiredCaptureServer(t)
	defer server.Close()

	disabled := true
	if _, err := client.UpdateUser(context.Background(), "john", UpdateUserRequest{
		Description: "updated",
		Disabled:    &disabled,
	}); err != nil {
		t.Fatalf("UpdateUser failed: %v", err)
	}

	method, expired, seen := capture.get()
	if !seen {
		t.Fatal("expected the update to send an expired parameter")
	}
	if method != "set" {
		t.Errorf("update must use the set method, got %q", method)
	}
	if expired != "now" {
		t.Errorf("expired = %q, want \"now\"", expired)
	}
}

// TestClient_UpdateUser_ExpiryRoundTrip proves a configured date survives the
// round trip despite DSM answering without leading zeros.
func TestClient_UpdateUser_ExpiryRoundTrip(t *testing.T) {
	client, server, _ := setupExpiredCaptureServer(t)
	defer server.Close()

	expireDate := "2027-03-05"
	user, err := client.UpdateUser(context.Background(), "john", UpdateUserRequest{
		ExpireDate: &expireDate,
	})
	if err != nil {
		t.Fatalf("UpdateUser failed: %v", err)
	}

	if user.ExpireDate != "2027-03-05" {
		t.Errorf("ExpireDate = %q, want 2027-03-05", user.ExpireDate)
	}
	if user.Disabled {
		t.Error("an account with an expiry date is not disabled")
	}
	if !user.TwoFactor {
		t.Error("expected TwoFactor to be read from 2fa_status")
	}
}

func TestParseUser_AccountState(t *testing.T) {
	tests := []struct {
		name           string
		payload        string
		wantDisabled   bool
		wantExpireDate string
	}{
		{name: "normal is active", payload: `{"name":"a","expired":"normal"}`},
		{name: "now is disabled", payload: `{"name":"a","expired":"now"}`, wantDisabled: true},
		{
			name:           "a date becomes ISO",
			payload:        `{"name":"a","expired":"2027/3/5"}`,
			wantExpireDate: "2027-03-05",
		},
		{
			name:           "a padded date is accepted too",
			payload:        `{"name":"a","expired":"2027/03/05"}`,
			wantExpireDate: "2027-03-05",
		},
		{name: "empty expired is active", payload: `{"name":"a","expired":""}`},
		{name: "missing expired is active", payload: `{"name":"a"}`},
		{
			name:           "an unparseable value is passed through untouched",
			payload:        `{"name":"a","expired":"whenever"}`,
			wantExpireDate: "whenever",
		},
		{name: "legacy disabled flag still honoured", payload: `{"name":"a","disabled":true}`, wantDisabled: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := parseUser(json.RawMessage(tt.payload))
			if err != nil {
				t.Fatalf("parseUser failed: %v", err)
			}
			if u.Disabled != tt.wantDisabled {
				t.Errorf("Disabled = %v, want %v", u.Disabled, tt.wantDisabled)
			}
			if u.ExpireDate != tt.wantExpireDate {
				t.Errorf("ExpireDate = %q, want %q", u.ExpireDate, tt.wantExpireDate)
			}
		})
	}
}

func TestDateConversion(t *testing.T) {
	tests := []struct {
		name   string
		iso    string
		dsm    string
		oneWay bool // DSM -> ISO only; DSM drops leading zeros on the way back
	}{
		{name: "single digit month and day", iso: "2027-03-05", dsm: "2027/3/5"},
		{name: "double digit month and day", iso: "2027-12-31", dsm: "2027/12/31"},
		{name: "padded DSM form is read too", iso: "2027-03-05", dsm: "2027/03/05", oneWay: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fromDSMDate(tt.dsm); got != tt.iso {
				t.Errorf("fromDSMDate(%q) = %q, want %q", tt.dsm, got, tt.iso)
			}
			if tt.oneWay {
				return
			}
			if got := toDSMDate(tt.iso); got != tt.dsm {
				t.Errorf("toDSMDate(%q) = %q, want %q", tt.iso, got, tt.dsm)
			}
		})
	}

	// Garbage is passed through rather than guessed at, so DSM reports the error.
	if got := toDSMDate("not-a-date"); got != "not-a-date" {
		t.Errorf("toDSMDate should pass unparseable input through, got %q", got)
	}
	if got := fromDSMDate("normal"); got != "normal" {
		t.Errorf("fromDSMDate should pass unparseable input through, got %q", got)
	}
}

func TestExpiredParam(t *testing.T) {
	tests := []struct {
		name       string
		disabled   bool
		expireDate string
		want       string
	}{
		{name: "plain active account", want: "normal"},
		{name: "disabled", disabled: true, want: "now"},
		{name: "expiry date", expireDate: "2027-03-05", want: "2027/3/5"},
		{name: "disabled takes precedence", disabled: true, expireDate: "2027-03-05", want: "now"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := expiredParam(tt.disabled, tt.expireDate); got != tt.want {
				t.Errorf("expiredParam(%v, %q) = %q, want %q", tt.disabled, tt.expireDate, got, tt.want)
			}
		})
	}
}
