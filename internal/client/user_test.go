package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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

		case api == "SYNO.Core.User" && method == "update":
			json.NewEncoder(w).Encode(APIResponse{Success: true})

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
	client.sessionID = "test-sid"

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
