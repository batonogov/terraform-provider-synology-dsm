package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// shareStore is the mock DSM's share table. Values mirror what a real DSM
// returns, including the write/read name mismatch for the quota
// (share_quota in, quota_value out).
type shareStore struct {
	mu     sync.Mutex
	shares map[string]map[string]interface{}
	// lastMethod records the method used by the most recent write, so tests can
	// assert that updates go through "set" rather than "create".
	lastMethod string
}

func newShareStore() *shareStore {
	return &shareStore{
		shares: map[string]map[string]interface{}{
			"homes": {
				"name": "homes", "desc": "Home directories", "vol_path": "/volume1", "uuid": "uuid-1",
				"hidden": false, "enable_recycle_bin": true, "recycle_bin_admin_only": false,
				"enable_share_compress": false, "enable_share_cow": true, "quota_value": float64(0),
			},
			"music": {
				"name": "music", "desc": "Music folder", "vol_path": "/volume1", "uuid": "uuid-2",
			},
		},
	}
}

// applyShareInfo stores a shareinfo payload the way DSM does: share_quota is
// persisted under the quota_value key.
func (s *shareStore) applyShareInfo(raw string, method string) (string, bool) {
	var info map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		return "", false
	}

	name, _ := info["name"].(string)
	if name == "" {
		return "", false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.lastMethod = method

	existing, ok := s.shares[name]
	if !ok {
		existing = map[string]interface{}{"uuid": "uuid-new"}
	}
	for k, v := range info {
		if k == "share_quota" {
			existing["quota_value"] = v
			continue
		}
		existing[k] = v
	}
	s.shares[name] = existing
	return name, true
}

func setupShareTestServer() (*Client, *httptest.Server, *shareStore) {
	store := newShareStore()
	mux := http.NewServeMux()

	mux.HandleFunc("/webapi/entry.cgi", func(w http.ResponseWriter, r *http.Request) {
		api := r.URL.Query().Get("api")
		method := r.URL.Query().Get("method")

		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			r.ParseForm()
			api = r.FormValue("api")
			method = r.FormValue("method")
		}

		switch {
		case api == "SYNO.Core.Share" && method == "create":
			// Real DSM refuses to create over an existing share.
			var info map[string]interface{}
			json.Unmarshal([]byte(r.FormValue("shareinfo")), &info)
			name, _ := info["name"].(string)

			store.mu.Lock()
			_, exists := store.shares[name]
			store.mu.Unlock()
			if exists {
				json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 3301}})
				return
			}

			created, ok := store.applyShareInfo(r.FormValue("shareinfo"), "create")
			if !ok {
				json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 101}})
				return
			}
			raw, _ := json.Marshal(map[string]interface{}{"name": created})
			json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		case api == "SYNO.Core.Share" && method == "set":
			updated, ok := store.applyShareInfo(r.FormValue("shareinfo"), "set")
			if !ok {
				json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 101}})
				return
			}
			raw, _ := json.Marshal(map[string]interface{}{"name": updated})
			json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		case api == "SYNO.Core.Share" && method == "get":
			name := r.URL.Query().Get("name")
			store.mu.Lock()
			share, ok := store.shares[name]
			store.mu.Unlock()
			if !ok {
				json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 3301}})
				return
			}
			raw, _ := json.Marshal(share)
			json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		case api == "SYNO.Core.Share" && method == "list":
			store.mu.Lock()
			list := make([]map[string]interface{}, 0, len(store.shares))
			for _, name := range []string{"homes", "music"} {
				if s, ok := store.shares[name]; ok {
					list = append(list, s)
				}
			}
			store.mu.Unlock()
			raw, _ := json.Marshal(map[string]interface{}{"shares": list, "total": len(list)})
			json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		case api == "SYNO.Core.Share" && method == "delete":
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

	return client, server, store
}

func TestClient_CreateShare(t *testing.T) {
	client, server, _ := setupShareTestServer()
	defer server.Close()

	share, err := client.CreateShare(context.Background(), CreateShareRequest{
		Name:             "team-data",
		VolPath:          "/volume1",
		Description:      "Team data folder",
		EnableRecycleBin: true,
	})
	if err != nil {
		t.Fatalf("CreateShare failed: %v", err)
	}
	if share.Name != "team-data" {
		t.Errorf("expected name team-data, got %q", share.Name)
	}
}

// TestClient_CreateShare_ExtendedAttributes checks that every extended field
// reaches DSM and comes back in the parsed share.
func TestClient_CreateShare_ExtendedAttributes(t *testing.T) {
	client, server, _ := setupShareTestServer()
	defer server.Close()

	share, err := client.CreateShare(context.Background(), CreateShareRequest{
		Name:                "team-data",
		VolPath:             "/volume1",
		Description:         "Team data folder",
		Hidden:              true,
		EnableRecycleBin:    true,
		RecycleBinAdminOnly: true,
		EnableShareCompress: true,
		EnableShareCow:      true,
		ShareQuota:          100,
	})
	if err != nil {
		t.Fatalf("CreateShare failed: %v", err)
	}

	if !share.Hidden {
		t.Error("expected Hidden true")
	}
	if !share.EnableRecycleBin {
		t.Error("expected EnableRecycleBin true")
	}
	if !share.RecycleBinAdminOnly {
		t.Error("expected RecycleBinAdminOnly true")
	}
	if !share.EnableShareCompress {
		t.Error("expected EnableShareCompress true")
	}
	if !share.EnableShareCow {
		t.Error("expected EnableShareCow true")
	}
	if share.ShareQuota != 100 {
		t.Errorf("expected ShareQuota 100, got %d", share.ShareQuota)
	}
}

// TestBuildShareInfo pins the exact payload sent to DSM, including the
// share_quota key name and the absence of name_org.
func TestBuildShareInfo(t *testing.T) {
	payload := buildShareInfo(CreateShareRequest{
		Name:                "team-data",
		VolPath:             "/volume1",
		Description:         "desc",
		Hidden:              true,
		EnableRecycleBin:    true,
		RecycleBinAdminOnly: false,
		EnableShareCompress: true,
		EnableShareCow:      false,
		ShareQuota:          42,
	})

	var got map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		t.Fatalf("shareinfo is not valid JSON: %v", err)
	}

	want := map[string]interface{}{
		"name":                   "team-data",
		"vol_path":               "/volume1",
		"desc":                   "desc",
		"hidden":                 true,
		"enable_recycle_bin":     true,
		"recycle_bin_admin_only": false,
		"enable_share_compress":  true,
		"enable_share_cow":       false,
		"share_quota":            float64(42),
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("shareinfo[%q] = %v, want %v", k, got[k], v)
		}
	}

	// name_org was part of the broken update path; it must be gone.
	if _, ok := got["name_org"]; ok {
		t.Error("shareinfo must not carry name_org: DSM answers 3301 to that")
	}
	if len(got) != len(want) {
		t.Errorf("shareinfo has %d keys, want exactly %d: %v", len(got), len(want), got)
	}
}

func TestClient_GetShare(t *testing.T) {
	client, server, _ := setupShareTestServer()
	defer server.Close()

	share, err := client.GetShare(context.Background(), "homes")
	if err != nil {
		t.Fatalf("GetShare failed: %v", err)
	}
	if share.Name != "homes" {
		t.Errorf("expected name homes, got %q", share.Name)
	}
	if share.VolPath != "/volume1" {
		t.Errorf("expected vol_path /volume1, got %q", share.VolPath)
	}
	if share.UUID != "uuid-1" {
		t.Errorf("expected uuid uuid-1, got %q", share.UUID)
	}
	if !share.EnableShareCow {
		t.Error("expected EnableShareCow true for homes")
	}
}

func TestClient_ListShares(t *testing.T) {
	client, server, _ := setupShareTestServer()
	defer server.Close()

	shares, err := client.ListShares(context.Background())
	if err != nil {
		t.Fatalf("ListShares failed: %v", err)
	}
	if len(shares) != 2 {
		t.Fatalf("expected 2 shares, got %d", len(shares))
	}
	if shares[0].Name != "homes" {
		t.Errorf("expected first share homes, got %q", shares[0].Name)
	}
}

// TestClient_UpdateShare_UsesSetMethod is the regression guard for the core
// bug: updates used to go through "create" with name_org, which real DSM
// rejects with 3301, so no update ever took effect.
func TestClient_UpdateShare_UsesSetMethod(t *testing.T) {
	client, server, store := setupShareTestServer()
	defer server.Close()

	share, err := client.UpdateShare(context.Background(), "homes", CreateShareRequest{
		Name:                "homes",
		VolPath:             "/volume1",
		Description:         "Updated description",
		Hidden:              true,
		EnableRecycleBin:    true,
		RecycleBinAdminOnly: true,
		EnableShareCompress: true,
		ShareQuota:          50,
	})
	if err != nil {
		t.Fatalf("UpdateShare failed: %v", err)
	}

	store.mu.Lock()
	lastMethod := store.lastMethod
	store.mu.Unlock()
	if lastMethod != "set" {
		t.Fatalf("update must use the set method, got %q", lastMethod)
	}

	if share.Description != "Updated description" {
		t.Errorf("expected updated description, got %q", share.Description)
	}
	if !share.Hidden {
		t.Error("expected Hidden true after update")
	}
	if !share.RecycleBinAdminOnly {
		t.Error("expected RecycleBinAdminOnly true after update")
	}
	if !share.EnableShareCompress {
		t.Error("expected EnableShareCompress true after update")
	}
	if share.ShareQuota != 50 {
		t.Errorf("expected ShareQuota 50 after update, got %d", share.ShareQuota)
	}
}

// TestClient_UpdateShare_ClearsFlags proves settings can be turned back off —
// the direction that silently failed before, since nothing was applied at all.
func TestClient_UpdateShare_ClearsFlags(t *testing.T) {
	client, server, _ := setupShareTestServer()
	defer server.Close()

	ctx := context.Background()
	if _, err := client.UpdateShare(ctx, "homes", CreateShareRequest{
		Name: "homes", VolPath: "/volume1",
		EnableShareCompress: true, EnableShareCow: true, ShareQuota: 10,
	}); err != nil {
		t.Fatalf("first UpdateShare failed: %v", err)
	}

	share, err := client.UpdateShare(ctx, "homes", CreateShareRequest{
		Name: "homes", VolPath: "/volume1",
		EnableShareCompress: false, EnableShareCow: false, ShareQuota: 0,
	})
	if err != nil {
		t.Fatalf("second UpdateShare failed: %v", err)
	}

	if share.EnableShareCompress {
		t.Error("expected EnableShareCompress cleared")
	}
	if share.EnableShareCow {
		t.Error("expected EnableShareCow cleared")
	}
	if share.ShareQuota != 0 {
		t.Errorf("expected ShareQuota cleared to 0, got %d", share.ShareQuota)
	}
}

func TestClient_DeleteShare(t *testing.T) {
	client, server, _ := setupShareTestServer()
	defer server.Close()

	if err := client.DeleteShare(context.Background(), "team-data"); err != nil {
		t.Fatalf("DeleteShare failed: %v", err)
	}
}

// TestParseShare covers the field-name quirks: recyclebin vs
// enable_recycle_bin, and quota_value vs share_quota.
func TestParseShare(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    Share
		wantErr bool
	}{
		{
			name:    "quota read back under quota_value",
			payload: `{"name":"a","vol_path":"/volume1","quota_value":100}`,
			want:    Share{Name: "a", VolPath: "/volume1", ShareQuota: 100},
		},
		{
			name:    "quota under share_quota is also accepted",
			payload: `{"name":"a","share_quota":7}`,
			want:    Share{Name: "a", ShareQuota: 7},
		},
		{
			name:    "quota_value wins when both are present",
			payload: `{"name":"a","quota_value":100,"share_quota":7}`,
			want:    Share{Name: "a", ShareQuota: 100},
		},
		{
			name:    "recycle bin under the enable_recycle_bin key",
			payload: `{"name":"a","enable_recycle_bin":true}`,
			want:    Share{Name: "a", EnableRecycleBin: true},
		},
		{
			name:    "recycle bin under DSM's recyclebin alias",
			payload: `{"name":"a","recyclebin":true}`,
			want:    Share{Name: "a", EnableRecycleBin: true},
		},
		{
			name:    "all extended flags",
			payload: `{"name":"a","desc":"d","vol_path":"/volume2","uuid":"u","hidden":true,"enable_recycle_bin":true,"recycle_bin_admin_only":true,"enable_share_compress":true,"enable_share_cow":true,"quota_value":1024}`,
			want: Share{
				Name: "a", Description: "d", VolPath: "/volume2", UUID: "u",
				Hidden: true, EnableRecycleBin: true, RecycleBinAdminOnly: true,
				EnableShareCompress: true, EnableShareCow: true, ShareQuota: 1024,
			},
		},
		{
			name:    "unexpected types are ignored, not fatal",
			payload: `{"name":"a","hidden":"yes","quota_value":"lots"}`,
			want:    Share{Name: "a"},
		},
		{
			name:    "malformed json",
			payload: `{nope`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseShare(json.RawMessage(tt.payload))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected a parse error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseShare failed: %v", err)
			}
			if *got != tt.want {
				t.Errorf("got %+v, want %+v", *got, tt.want)
			}
		})
	}
}
