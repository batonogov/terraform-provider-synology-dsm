package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
)

// sweeperMock serves the list/delete calls the sweepers make and records every
// name they asked to delete.
type sweeperMock struct {
	mu      sync.Mutex
	deleted []string
}

func (m *sweeperMock) record(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleted = append(m.deleted, name)
}

func (m *sweeperMock) names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]string(nil), m.deleted...)
	sort.Strings(out)
	return out
}

// newSweeperTestServer serves one listing under the given key ("shares",
// "users" or "groups") and accepts deletes for it.
func newSweeperTestServer(t *testing.T, listKey string, items []map[string]interface{}) (*sweeperMock, *httptest.Server) {
	t.Helper()

	mock := &sweeperMock{}
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
		case api == "SYNO.API.Auth" && method == "login":
			json.NewEncoder(w).Encode(client.APIResponse{
				Success: true,
				Data:    json.RawMessage(`{"sid":"sweep-sid","synotoken":"sweep-token"}`),
			})

		case method == "list":
			raw, _ := json.Marshal(map[string]interface{}{listKey: items, "total": len(items)})
			json.NewEncoder(w).Encode(client.APIResponse{Success: true, Data: raw})

		case method == "delete":
			// Both a bare name and a JSON array of names are used by the client
			// depending on the resource, so accept either.
			name := r.URL.Query().Get("name")
			var names []string
			if err := json.Unmarshal([]byte(name), &names); err == nil {
				for _, n := range names {
					mock.record(n)
				}
			} else {
				mock.record(name)
			}
			json.NewEncoder(w).Encode(client.APIResponse{Success: true})

		default:
			json.NewEncoder(w).Encode(client.APIResponse{Success: false, Error: &client.APIError{Code: 101}})
		}
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	t.Setenv("SYNOLOGY_DSM_HOST", server.URL)
	t.Setenv("SYNOLOGY_DSM_USERNAME", "admin")
	t.Setenv("SYNOLOGY_DSM_PASSWORD", "")

	return mock, server
}

// TestSweepers_OnlyTouchTestResources is the safety net that matters: a sweeper
// runs against whatever DSM the environment points at, which may well be a real
// NAS. It must delete acceptance-test leftovers and nothing else.
func TestSweepers_OnlyTouchTestResources(t *testing.T) {
	tests := []struct {
		name    string
		listKey string
		items   []map[string]interface{}
		sweep   func(string) error
		want    []string
	}{
		{
			name:    "shared folders",
			listKey: "shares",
			items: []map[string]interface{}{
				{"name": "homes", "vol_path": "/volume1"},
				{"name": "team-data", "vol_path": "/volume1"},
				{"name": "tfacctestfolder", "vol_path": "/volume1"},
				{"name": "tfacctestfolderimp", "vol_path": "/volume1"},
				{"name": "not-tfacctest", "vol_path": "/volume1"},
			},
			sweep: sweepSharedFolders,
			want:  []string{"tfacctestfolder", "tfacctestfolderimp"},
		},
		{
			name:    "users",
			listKey: "users",
			items: []map[string]interface{}{
				{"name": "admin", "uid": float64(1024)},
				{"name": "john.doe", "uid": float64(1025)},
				{"name": "tfacctestuser", "uid": float64(1026)},
			},
			sweep: sweepUsers,
			want:  []string{"tfacctestuser"},
		},
		{
			name:    "groups",
			listKey: "groups",
			items: []map[string]interface{}{
				{"name": "users", "gid": float64(100)},
				{"name": "developers", "gid": float64(101)},
				{"name": "tfacctestgroup", "gid": float64(102)},
			},
			sweep: sweepGroups,
			want:  []string{"tfacctestgroup"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock, _ := newSweeperTestServer(t, tt.listKey, tt.items)

			if err := tt.sweep("all"); err != nil {
				t.Fatalf("sweeper failed: %v", err)
			}

			got := mock.names()
			if len(got) != len(tt.want) {
				t.Fatalf("deleted %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("deleted %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// TestSweeperClient_RequiresHost keeps a sweeper from silently doing nothing
// (or worse, hitting an unintended default) when the environment is not set.
func TestSweeperClient_RequiresHost(t *testing.T) {
	t.Setenv("SYNOLOGY_DSM_HOST", "")

	if _, err := sweeperClient(); err == nil {
		t.Fatal("expected an error when SYNOLOGY_DSM_HOST is unset")
	}
}
