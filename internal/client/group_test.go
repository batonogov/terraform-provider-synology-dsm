package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func setupGroupTestServer() (*Client, *httptest.Server) {
	mux := http.NewServeMux()

	mux.HandleFunc("/webapi/entry.cgi", func(w http.ResponseWriter, r *http.Request) {
		api := r.URL.Query().Get("api")
		method := r.URL.Query().Get("method")

		switch {
		case api == "SYNO.Core.Group" && method == "create":
			json.NewEncoder(w).Encode(APIResponse{Success: true})

		case api == "SYNO.Core.Group" && method == "get":
			name := r.URL.Query().Get("name")
			groups := []map[string]interface{}{
				{"name": name, "description": "Test group", "gid": 65536},
			}
			raw, _ := json.Marshal(map[string]interface{}{"groups": groups})
			json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		case api == "SYNO.Core.Group" && method == "list":
			groups := []map[string]interface{}{
				{"name": "administrators", "gid": 101, "description": "System default admin group"},
				{"name": "users", "gid": 100, "description": "System default group"},
			}
			raw, _ := json.Marshal(map[string]interface{}{"groups": groups, "total": len(groups)})
			json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		case api == "SYNO.Core.Group" && method == "set":
			name := r.URL.Query().Get("new_name")
			if name == "" {
				name = r.URL.Query().Get("name")
			}
			groups := []map[string]interface{}{
				{"name": name, "gid": 65536},
			}
			raw, _ := json.Marshal(map[string]interface{}{"groups": groups})
			json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		case api == "SYNO.Core.Group" && method == "delete":
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

func TestClient_CreateGroup(t *testing.T) {
	client, server := setupGroupTestServer()
	defer server.Close()

	group, err := client.CreateGroup(context.Background(), CreateGroupRequest{
		Name:        "developers",
		Description: "Development team",
	})
	if err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}
	if group.Name != "developers" {
		t.Errorf("expected name developers, got %q", group.Name)
	}
}

func TestClient_GetGroup(t *testing.T) {
	client, server := setupGroupTestServer()
	defer server.Close()

	group, err := client.GetGroup(context.Background(), "administrators")
	if err != nil {
		t.Fatalf("GetGroup failed: %v", err)
	}
	if group.Name != "administrators" {
		t.Errorf("expected name administrators, got %q", group.Name)
	}
	if group.GID != 65536 {
		t.Errorf("expected GID 65536, got %d", group.GID)
	}
}

func TestClient_ListGroups(t *testing.T) {
	client, server := setupGroupTestServer()
	defer server.Close()

	groups, err := client.ListGroups(context.Background())
	if err != nil {
		t.Fatalf("ListGroups failed: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}
	if groups[0].Name != "administrators" {
		t.Errorf("expected first group administrators, got %q", groups[0].Name)
	}
}

func TestClient_UpdateGroup(t *testing.T) {
	client, server := setupGroupTestServer()
	defer server.Close()

	group, err := client.UpdateGroup(context.Background(), "developers", UpdateGroupRequest{
		Description: "Updated description",
	})
	if err != nil {
		t.Fatalf("UpdateGroup failed: %v", err)
	}
	if group.Name != "developers" {
		t.Errorf("expected name developers, got %q", group.Name)
	}
}

func TestClient_UpdateGroup_Rename(t *testing.T) {
	client, server := setupGroupTestServer()
	defer server.Close()

	group, err := client.UpdateGroup(context.Background(), "developers", UpdateGroupRequest{
		NewName: "engineers",
	})
	if err != nil {
		t.Fatalf("UpdateGroup rename failed: %v", err)
	}
	if group.Name != "engineers" {
		t.Errorf("expected name engineers, got %q", group.Name)
	}
}

func TestClient_DeleteGroup(t *testing.T) {
	client, server := setupGroupTestServer()
	defer server.Close()

	if err := client.DeleteGroup(context.Background(), "developers"); err != nil {
		t.Fatalf("DeleteGroup failed: %v", err)
	}
}
