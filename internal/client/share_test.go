package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func setupShareTestServer() (*Client, *httptest.Server) {
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
			name := r.FormValue("name")
			data := map[string]interface{}{
				"name": name,
			}
			raw, _ := json.Marshal(data)
			json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		case api == "SYNO.Core.Share" && method == "get":
			name := r.URL.Query().Get("name")
			data := map[string]interface{}{
				"name":       name,
				"desc":       "Test share",
				"vol_path":   "/volume1",
				"uuid":       "test-uuid-1234",
				"hidden":     false,
				"enable_recycle_bin": true,
			}
			raw, _ := json.Marshal(data)
			json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		case api == "SYNO.Core.Share" && method == "list":
			shares := []map[string]interface{}{
				{"name": "homes", "desc": "Home directories", "vol_path": "/volume1", "uuid": "uuid-1"},
				{"name": "music", "desc": "Music folder", "vol_path": "/volume1", "uuid": "uuid-2"},
			}
			raw, _ := json.Marshal(map[string]interface{}{"shares": shares, "total": len(shares)})
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
	client.sessionID = "test-sid"

	return client, server
}

func TestClient_CreateShare(t *testing.T) {
	client, server := setupShareTestServer()
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

func TestClient_GetShare(t *testing.T) {
	client, server := setupShareTestServer()
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
	if share.UUID != "test-uuid-1234" {
		t.Errorf("expected uuid test-uuid-1234, got %q", share.UUID)
	}
}

func TestClient_ListShares(t *testing.T) {
	client, server := setupShareTestServer()
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

func TestClient_UpdateShare(t *testing.T) {
	client, server := setupShareTestServer()
	defer server.Close()

	share, err := client.UpdateShare(context.Background(), "team-data", CreateShareRequest{
		Name:        "team-data",
		VolPath:     "/volume1",
		Description: "Updated description",
	})
	if err != nil {
		t.Fatalf("UpdateShare failed: %v", err)
	}
	if share.Name != "team-data" {
		t.Errorf("expected name team-data, got %q", share.Name)
	}
}

func TestClient_DeleteShare(t *testing.T) {
	client, server := setupShareTestServer()
	defer server.Close()

	if err := client.DeleteShare(context.Background(), "team-data"); err != nil {
		t.Fatalf("DeleteShare failed: %v", err)
	}
}
