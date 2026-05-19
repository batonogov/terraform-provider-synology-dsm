package provider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
)

func TestUserDataSource_Metadata(t *testing.T) {
	d := NewUserDataSource()

	req := datasource.MetadataRequest{
		ProviderTypeName: "dsm",
	}
	resp := &datasource.MetadataResponse{}

	d.Metadata(t.Context(), req, resp)

	if resp.TypeName != "dsm_user" {
		t.Errorf("expected type name dsm_user, got %q", resp.TypeName)
	}
}

func TestUserDataSource_Schema(t *testing.T) {
	d := NewUserDataSource()

	req := datasource.SchemaRequest{}
	resp := &datasource.SchemaResponse{}

	d.Schema(t.Context(), req, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned errors: %v", resp.Diagnostics)
	}

	attrs := resp.Schema.GetAttributes()
	requiredAttrs := []string{"name"}
	for _, attr := range requiredAttrs {
		if _, ok := attrs[attr]; !ok {
			t.Errorf("missing required attribute %q", attr)
		}
	}

	computedAttrs := []string{"id", "description", "email", "disabled", "groups", "uid"}
	for _, attr := range computedAttrs {
		if _, ok := attrs[attr]; !ok {
			t.Errorf("missing computed attribute %q", attr)
		}
	}
}

func TestUserDataSource_Configure_NilProviderData(t *testing.T) {
	ds := NewUserDataSource().(*userDataSource)

	req := datasource.ConfigureRequest{}
	resp := &datasource.ConfigureResponse{}

	ds.Configure(t.Context(), req, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("should not error on nil ProviderData: %v", resp.Diagnostics)
	}
	if ds.client != nil {
		t.Error("client should remain nil when ProviderData is nil")
	}
}

func TestUserDataSource_Read_viaClient(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/webapi/entry.cgi", func(w http.ResponseWriter, r *http.Request) {
		api := r.URL.Query().Get("api")
		method := r.URL.Query().Get("method")

		switch {
		case api == "SYNO.API.Auth" && method == "login":
			json.NewEncoder(w).Encode(client.APIResponse{
				Success: true,
				Data:    json.RawMessage(`{"sid":"test-sid","synotoken":"test-token"}`),
			})
		case api == "SYNO.Core.User" && method == "get":
			data := map[string]interface{}{
				"name":        "admin",
				"description": "Test user",
				"email":       "admin@example.com",
				"disabled":    false,
				"uid":         1024,
				"groups":      []string{"users", "admin"},
			}
			raw, _ := json.Marshal(data)
			json.NewEncoder(w).Encode(client.APIResponse{Success: true, Data: raw})
		default:
			json.NewEncoder(w).Encode(client.APIResponse{
				Success: false,
				Error:   &client.APIError{Code: 101},
			})
		}
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	c := client.NewClient(server.URL, "admin", "password", false)
	c.Login(t.Context())

	user, err := c.GetUser(t.Context(), "admin")
	if err != nil {
		t.Fatalf("GetUser failed: %v", err)
	}

	if user.Name != "admin" {
		t.Errorf("expected name admin, got %q", user.Name)
	}
	if user.Description != "Test user" {
		t.Errorf("expected description 'Test user', got %q", user.Description)
	}
	if user.Email != "admin@example.com" {
		t.Errorf("expected email admin@example.com, got %q", user.Email)
	}
	if user.Disabled {
		t.Error("expected disabled false")
	}
	if user.UID != 1024 {
		t.Errorf("expected uid 1024, got %d", user.UID)
	}
	if len(user.Groups) != 2 || user.Groups[0] != "users" || user.Groups[1] != "admin" {
		t.Errorf("expected groups [users admin], got %v", user.Groups)
	}
}
