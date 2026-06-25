package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func setupUserQuotaTestServer() (*Client, *httptest.Server) {
	mux := http.NewServeMux()

	var mu sync.Mutex
	quotas := []map[string]interface{}{
		{"username": "admin", "quota_size": float64(0), "quota_used": float64(0)},
		{"username": "john", "quota_size": float64(1073741824), "quota_used": float64(536870912)},
	}

	mux.HandleFunc("/webapi/entry.cgi", func(w http.ResponseWriter, r *http.Request) {
		api := r.URL.Query().Get("api")
		method := r.URL.Query().Get("method")

		switch {
		case api == "SYNO.Core.Share.Quota" && method == "list":
			mu.Lock()
			items := quotas
			mu.Unlock()

			raw, _ := json.Marshal(map[string]interface{}{"items": items, "total": len(items)})
			json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		case api == "SYNO.Core.Share.Quota" && method == "set":
			quotaStr := r.URL.Query().Get("quotas")

			var parsed []map[string]interface{}
			json.Unmarshal([]byte(quotaStr), &parsed)

			mu.Lock()
			quotas = parsed
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
	client.setSession("test-sid", "")

	return client, server
}

func TestClient_ListUserQuotas(t *testing.T) {
	client, server := setupUserQuotaTestServer()
	defer server.Close()

	quotas, err := client.ListUserQuotas(context.Background(), "data")
	if err != nil {
		t.Fatalf("ListUserQuotas failed: %v", err)
	}
	if len(quotas) != 2 {
		t.Fatalf("expected 2 quotas, got %d", len(quotas))
	}
	if quotas[0].Username != "admin" {
		t.Errorf("expected first quota admin, got %q", quotas[0].Username)
	}
	if quotas[0].QuotaSize != 0 {
		t.Errorf("expected admin quota_size 0 (unlimited), got %d", quotas[0].QuotaSize)
	}
	if quotas[1].QuotaSize != 1073741824 {
		t.Errorf("expected john quota_size 1073741824, got %d", quotas[1].QuotaSize)
	}
}

func TestClient_GetUserQuota(t *testing.T) {
	client, server := setupUserQuotaTestServer()
	defer server.Close()

	q, err := client.GetUserQuota(context.Background(), "data", "john")
	if err != nil {
		t.Fatalf("GetUserQuota failed: %v", err)
	}
	if q.Username != "john" {
		t.Errorf("expected username john, got %q", q.Username)
	}
	if q.QuotaSize != 1073741824 {
		t.Errorf("expected quota_size 1073741824, got %d", q.QuotaSize)
	}
	if q.QuotaUsed != 536870912 {
		t.Errorf("expected quota_used 536870912, got %d", q.QuotaUsed)
	}
}

func TestClient_GetUserQuota_NotFound(t *testing.T) {
	client, server := setupUserQuotaTestServer()
	defer server.Close()

	_, err := client.GetUserQuota(context.Background(), "data", "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent quota")
	}
}

func TestClient_SetUserQuota_New(t *testing.T) {
	client, server := setupUserQuotaTestServer()
	defer server.Close()

	q, err := client.SetUserQuota(context.Background(), SetUserQuotaRequest{
		ShareName: "data",
		Username:  "jane",
		QuotaSize: 2147483648,
	})
	if err != nil {
		t.Fatalf("SetUserQuota new failed: %v", err)
	}
	if q.Username != "jane" {
		t.Errorf("expected username jane, got %q", q.Username)
	}
	if q.QuotaSize != 2147483648 {
		t.Errorf("expected quota_size 2147483648, got %d", q.QuotaSize)
	}
}

func TestClient_SetUserQuota_Update(t *testing.T) {
	client, server := setupUserQuotaTestServer()
	defer server.Close()

	q, err := client.SetUserQuota(context.Background(), SetUserQuotaRequest{
		ShareName: "data",
		Username:  "john",
		QuotaSize: 2147483648,
	})
	if err != nil {
		t.Fatalf("SetUserQuota update failed: %v", err)
	}
	if q.Username != "john" {
		t.Errorf("expected username john, got %q", q.Username)
	}
	if q.QuotaSize != 2147483648 {
		t.Errorf("expected quota_size 2147483648 after update, got %d", q.QuotaSize)
	}
}

func TestClient_DeleteUserQuota(t *testing.T) {
	client, server := setupUserQuotaTestServer()
	defer server.Close()

	if err := client.DeleteUserQuota(context.Background(), "data", "john"); err != nil {
		t.Fatalf("DeleteUserQuota failed: %v", err)
	}
}

func TestClient_DeleteUserQuota_NotFound(t *testing.T) {
	client, server := setupUserQuotaTestServer()
	defer server.Close()

	if err := client.DeleteUserQuota(context.Background(), "data", "nonexistent"); err != nil {
		t.Fatalf("DeleteUserQuota for nonexistent user should not error, got: %v", err)
	}
}

func TestBuildUserQuotaID(t *testing.T) {
	id := BuildUserQuotaID("data", "john")
	if id != "data:john" {
		t.Errorf("expected data:john, got %q", id)
	}
}

func TestParseUserQuotaID(t *testing.T) {
	shareName, username, err := ParseUserQuotaID("data:john")
	if err != nil {
		t.Fatalf("ParseUserQuotaID failed: %v", err)
	}
	if shareName != "data" {
		t.Errorf("expected share name data, got %q", shareName)
	}
	if username != "john" {
		t.Errorf("expected username john, got %q", username)
	}
}

func TestParseUserQuotaID_Invalid(t *testing.T) {
	_, _, err := ParseUserQuotaID("invalid")
	if err == nil {
		t.Fatal("expected error for invalid ID")
	}
}
