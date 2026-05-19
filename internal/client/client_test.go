package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_Login(t *testing.T) {
	sid := "test-session-id-123"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("method") == "login" {
			resp := APIResponse{
				Success: true,
				Data:    json.RawMessage(`{"sid":"` + sid + `"}`),
			}
			json.NewEncoder(w).Encode(resp)
			return
		}
		if r.URL.Query().Get("method") == "logout" {
			json.NewEncoder(w).Encode(APIResponse{Success: true})
			return
		}
		resp := APIResponse{Success: false, Error: &APIError{Code: 101}}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient(server.URL, "admin", "password", false)

	if err := client.Login(context.Background()); err != nil {
		t.Fatalf("Login failed: %v", err)
	}

	if client.sessionID != sid {
		t.Errorf("expected sessionID %q, got %q", sid, client.sessionID)
	}

	if err := client.Logout(context.Background()); err != nil {
		t.Fatalf("Logout failed: %v", err)
	}

	if client.sessionID != "" {
		t.Errorf("expected empty sessionID after logout, got %q", client.sessionID)
	}
}

func TestClient_LoginFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := APIResponse{Success: false, Error: &APIError{Code: 400}}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient(server.URL, "admin", "wrong", false)

	if err := client.Login(context.Background()); err == nil {
		t.Fatal("expected login to fail, got nil error")
	}
}

func TestClient_DoAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api") == "SYNO.Core.User" && r.URL.Query().Get("method") == "list" {
			data := `{"users":[{"name":"admin","uid":1024}],"total":1}`
			resp := APIResponse{Success: true, Data: json.RawMessage(data)}
			json.NewEncoder(w).Encode(resp)
			return
		}
		resp := APIResponse{Success: false, Error: &APIError{Code: 102}}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient(server.URL, "admin", "password", false)
	client.sessionID = "test-sid"

	data, err := client.DoAPI(context.Background(), "SYNO.Core.User", "1", "list", nil)
	if err != nil {
		t.Fatalf("DoAPI failed: %v", err)
	}

	var result struct {
		Users []struct {
			Name string `json:"name"`
		} `json:"users"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("parse response: %v", err)
	}

	if result.Total != 1 {
		t.Errorf("expected total 1, got %d", result.Total)
	}
	if len(result.Users) != 1 || result.Users[0].Name != "admin" {
		t.Errorf("unexpected users: %+v", result.Users)
	}
}

func TestNewClient_InsecureTLS(t *testing.T) {
	client := NewClient("https://diskstation:5001", "admin", "pass", true)
	if client.baseURL != "https://diskstation:5001" {
		t.Errorf("expected baseURL to be preserved, got %q", client.baseURL)
	}
}

func TestIsTransientError(t *testing.T) {
	tests := []struct {
		err  string
		want bool
	}{
		{"connection refused", true},
		{"timeout reading body", true},
		{"temporary failure", true},
		{"unexpected EOF", true},
		{"invalid credentials", false},
		{"api error 400", false},
	}
	for _, tt := range tests {
		got := isTransientError(fmt.Errorf("%s", tt.err))
		if got != tt.want {
			t.Errorf("isTransientError(%q) = %v, want %v", tt.err, got, tt.want)
		}
	}
}
