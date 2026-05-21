package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func setupSharePermissionTestServer() (*Client, *httptest.Server) {
	mux := http.NewServeMux()

	var mu sync.Mutex
	userPerms := []map[string]interface{}{
		{"name": "admin", "is_readonly": false, "is_writable": true, "is_deny": false, "inherit": "rw"},
		{"name": "john", "is_readonly": true, "is_writable": false, "is_deny": false, "inherit": "r"},
	}
	groupPerms := []map[string]interface{}{
		{"name": "administrators", "is_readonly": false, "is_writable": true, "is_deny": false, "inherit": "rw"},
	}

	mux.HandleFunc("/webapi/entry.cgi", func(w http.ResponseWriter, r *http.Request) {
		api := r.URL.Query().Get("api")
		method := r.URL.Query().Get("method")

		switch {
		case api == "SYNO.Core.Share.Permission" && method == "list":
			ugType := r.URL.Query().Get("user_group_type")

			mu.Lock()
			var items []map[string]interface{}
			switch ugType {
			case "local_user":
				items = userPerms
			case "local_group":
				items = groupPerms
			default:
				items = []map[string]interface{}{}
			}
			mu.Unlock()

			raw, _ := json.Marshal(map[string]interface{}{"items": items, "total": len(items)})
			json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		case api == "SYNO.Core.Share.Permission" && method == "set":
			ugType := r.URL.Query().Get("user_group_type")
			permStr := r.URL.Query().Get("permissions")

			var parsed []map[string]interface{}
			json.Unmarshal([]byte(permStr), &parsed)

			mu.Lock()
			switch ugType {
			case "local_user":
				userPerms = parsed
			case "local_group":
				groupPerms = parsed
			}
			mu.Unlock()

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
	client.sessionID = "test-sid"

	return client, server
}

func TestClient_ListSharePermissions(t *testing.T) {
	client, server := setupSharePermissionTestServer()
	defer server.Close()

	perms, err := client.ListSharePermissions(context.Background(), "data", "local_user")
	if err != nil {
		t.Fatalf("ListSharePermissions failed: %v", err)
	}
	if len(perms) != 2 {
		t.Fatalf("expected 2 permissions, got %d", len(perms))
	}
	if perms[0].Name != "admin" {
		t.Errorf("expected first permission admin, got %q", perms[0].Name)
	}
	if !perms[0].IsWritable {
		t.Errorf("expected admin to be writable")
	}
	if !perms[1].IsReadonly {
		t.Errorf("expected john to be readonly")
	}
}

func TestClient_GetSharePermission(t *testing.T) {
	client, server := setupSharePermissionTestServer()
	defer server.Close()

	perm, err := client.GetSharePermission(context.Background(), "data", "local_user", "john")
	if err != nil {
		t.Fatalf("GetSharePermission failed: %v", err)
	}
	if perm.Name != "john" {
		t.Errorf("expected name john, got %q", perm.Name)
	}
	if !perm.IsReadonly {
		t.Errorf("expected john to be readonly")
	}
}

func TestClient_GetSharePermission_NotFound(t *testing.T) {
	client, server := setupSharePermissionTestServer()
	defer server.Close()

	_, err := client.GetSharePermission(context.Background(), "data", "local_user", "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent permission")
	}
}

func TestClient_SetSharePermission_New(t *testing.T) {
	client, server := setupSharePermissionTestServer()
	defer server.Close()

	perm, err := client.SetSharePermission(context.Background(), SetSharePermissionRequest{
		ShareName:     "data",
		UserGroupType: "local_user",
		PrincipalName: "jane",
		Permission:    "read_write",
	})
	if err != nil {
		t.Fatalf("SetSharePermission failed: %v", err)
	}
	if perm.Name != "jane" {
		t.Errorf("expected name jane, got %q", perm.Name)
	}
}

func TestClient_SetSharePermission_Update(t *testing.T) {
	client, server := setupSharePermissionTestServer()
	defer server.Close()

	perm, err := client.SetSharePermission(context.Background(), SetSharePermissionRequest{
		ShareName:     "data",
		UserGroupType: "local_user",
		PrincipalName: "john",
		Permission:    "read_write",
	})
	if err != nil {
		t.Fatalf("SetSharePermission update failed: %v", err)
	}
	if perm.Name != "john" {
		t.Errorf("expected name john, got %q", perm.Name)
	}
	if !perm.IsWritable {
		t.Errorf("expected john to be writable after update, got readonly=%v writable=%v deny=%v", perm.IsReadonly, perm.IsWritable, perm.IsDeny)
	}
}

func TestClient_SetSharePermission_InvalidPermission(t *testing.T) {
	client, server := setupSharePermissionTestServer()
	defer server.Close()

	_, err := client.SetSharePermission(context.Background(), SetSharePermissionRequest{
		ShareName:     "data",
		UserGroupType: "local_user",
		PrincipalName: "john",
		Permission:    "invalid",
	})
	if err == nil {
		t.Fatal("expected error for invalid permission")
	}
}

func TestClient_DeleteSharePermission(t *testing.T) {
	client, server := setupSharePermissionTestServer()
	defer server.Close()

	if err := client.DeleteSharePermission(context.Background(), "data", "local_user", "john"); err != nil {
		t.Fatalf("DeleteSharePermission failed: %v", err)
	}
}

func TestBuildSharePermissionID(t *testing.T) {
	id := BuildSharePermissionID("data", "local_user", "john")
	if id != "data:local_user:john" {
		t.Errorf("expected data:local_user:john, got %q", id)
	}
}

func TestParseSharePermissionID(t *testing.T) {
	shareName, ugType, principal, err := ParseSharePermissionID("data:local_user:john")
	if err != nil {
		t.Fatalf("ParseSharePermissionID failed: %v", err)
	}
	if shareName != "data" {
		t.Errorf("expected share name data, got %q", shareName)
	}
	if ugType != "local_user" {
		t.Errorf("expected type local_user, got %q", ugType)
	}
	if principal != "john" {
		t.Errorf("expected principal john, got %q", principal)
	}
}

func TestParseSharePermissionID_Invalid(t *testing.T) {
	_, _, _, err := ParseSharePermissionID("invalid")
	if err == nil {
		t.Fatal("expected error for invalid ID")
	}
}

func TestPermissionFromFlags(t *testing.T) {
	tests := []struct {
		name string
		perm SharePermission
		want string
	}{
		{"deny", SharePermission{IsDeny: true}, "no_access"},
		{"readonly", SharePermission{IsReadonly: true}, "read_only"},
		{"writable", SharePermission{IsWritable: true}, "read_write"},
		{"none", SharePermission{}, "no_access"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PermissionFromFlags(tt.perm)
			if got != tt.want {
				t.Errorf("permissionFromFlags(%+v) = %q, want %q", tt.perm, got, tt.want)
			}
		})
	}
}
