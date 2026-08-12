package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
)

// adoptTestServer models a DSM that already holds one share, refusing to create
// it again the way a real NAS does (3301) and recording whether the adoption
// went on to apply the configuration.
type adoptTestServer struct {
	existingVolPath string
	setCalls        atomic.Int32
	lastRecycleBin  atomic.Bool
}

func newAdoptTestServer(t *testing.T, existingVolPath string) (*client.Client, *adoptTestServer) {
	t.Helper()
	state := &adoptTestServer{existingVolPath: existingVolPath}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api := r.URL.Query().Get("api")
		method := r.URL.Query().Get("method")
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			_ = r.ParseForm()
			api = r.FormValue("api")
			method = r.FormValue("method")
		}

		switch {
		case api == "SYNO.Core.Share" && method == "create":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false, "error": map[string]int{"code": 3301},
			})

		case api == "SYNO.Core.Share" && method == "set":
			state.setCalls.Add(1)
			var info map[string]interface{}
			_ = json.Unmarshal([]byte(r.FormValue("shareinfo")), &info)
			if v, ok := info["enable_recycle_bin"].(bool); ok {
				state.lastRecycleBin.Store(v)
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "data": map[string]string{"name": "containers"},
			})

		case api == "SYNO.Core.Share" && method == "get":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"data": map[string]interface{}{
					"name": "containers", "vol_path": state.existingVolPath, "uuid": "uuid-existing",
					"enable_recycle_bin": state.lastRecycleBin.Load(),
				},
			})

		default:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false, "error": map[string]int{"code": 101},
			})
		}
	}))
	t.Cleanup(server.Close)

	return client.NewClient(server.URL, "admin", "", false), state
}

// TestAdoptExistingShare_AppliesConfiguration is the point of the feature: the
// existing folder is taken over AND brought in line with the configuration, so
// the next plan is empty rather than showing a diff.
func TestAdoptExistingShare_AppliesConfiguration(t *testing.T) {
	c, state := newAdoptTestServer(t, "/volume1")

	share, err := adoptExistingShare(context.Background(), c, client.CreateShareRequest{
		Name:             "containers",
		VolPath:          "/volume1",
		EnableRecycleBin: false,
	})
	if err != nil {
		t.Fatalf("adoption should succeed, got: %v", err)
	}
	if share.Name != "containers" {
		t.Errorf("share name = %q, want containers", share.Name)
	}
	if got := state.setCalls.Load(); got != 1 {
		t.Errorf("expected the configuration to be applied once, saw %d set calls", got)
	}
	if state.lastRecycleBin.Load() {
		t.Error("the configured enable_recycle_bin=false should have reached DSM")
	}
}

// TestAdoptExistingShare_RefusesVolumeMismatch: DSM cannot move a share between
// volumes, and vol_path forces replacement — adopting across volumes would hand
// the user a resource whose next plan offers to destroy the folder.
func TestAdoptExistingShare_RefusesVolumeMismatch(t *testing.T) {
	c, state := newAdoptTestServer(t, "/volume2")

	_, err := adoptExistingShare(context.Background(), c, client.CreateShareRequest{
		Name:    "containers",
		VolPath: "/volume1",
	})
	if err == nil {
		t.Fatal("adoption across volumes must fail")
	}
	for _, want := range []string{"/volume1", "/volume2", "cannot move"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
	if got := state.setCalls.Load(); got != 0 {
		t.Errorf("nothing should have been written to the mismatched share, saw %d set calls", got)
	}
}

// TestSharedFolderSchema_AdoptExisting keeps the attribute wired up and, more
// importantly, keeps it opt-in: defaulting to adoption would let `destroy`
// delete a folder full of data the practitioner never told Terraform to own.
func TestSharedFolderSchema_AdoptExisting(t *testing.T) {
	attrs := sharedFolderSchema(t).Schema.GetAttributes()

	attr, ok := attrs["adopt_existing"]
	if !ok {
		t.Fatal("adopt_existing must be part of the schema")
	}
	if !attr.IsOptional() || !attr.IsComputed() {
		t.Error("adopt_existing should be optional with a computed default")
	}
	if desc := attr.GetDescription(); !strings.Contains(desc, "destroy") {
		t.Error("the description must warn that an adopted folder can later be destroyed")
	}
}
