package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func newPackageTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	c := NewClient(server.URL, "admin", "password", false)
	c.setSession("test-sid", "test-token")
	return c, server
}

func packageRequest(r *http.Request) (api, method string) {
	_ = r.ParseForm()
	api = r.FormValue("api")
	method = r.FormValue("method")
	return api, method
}

func writePackageResponse(w http.ResponseWriter, data interface{}) {
	raw, _ := json.Marshal(data)
	_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})
}

func TestClient_ListPackages_ParsesNestedAdditional(t *testing.T) {
	client, server := newPackageTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		api, method := packageRequest(r)
		if api != "SYNO.Core.Package" || method != "list" {
			t.Fatalf("unexpected request: %s %s", api, method)
		}
		if version := r.FormValue("version"); version != "2" {
			t.Errorf("version = %q, want 2", version)
		}
		if additional := r.FormValue("additional"); !strings.Contains(additional, "ctl_uninstall") {
			t.Errorf("additional does not request ctl_uninstall: %s", additional)
		}
		writePackageResponse(w, map[string]interface{}{
			"total": 1,
			"packages": []interface{}{map[string]interface{}{
				"id": "ContainerManager", "name": "Container Manager", "version": "24.0.2-1535",
				"additional": map[string]interface{}{
					"status": "running", "description": "Containers", "maintainer": "Synology Inc.", "ctl_uninstall": 1,
				},
			}},
		})
	})
	defer server.Close()

	packages, err := client.ListPackages(context.Background())
	if err != nil {
		t.Fatalf("ListPackages failed: %v", err)
	}
	if len(packages) != 1 {
		t.Fatalf("got %d packages, want 1", len(packages))
	}
	pkg := packages[0]
	if pkg.ID != "ContainerManager" || pkg.Name != "Container Manager" || pkg.Version != "24.0.2-1535" {
		t.Errorf("unexpected identity fields: %+v", pkg)
	}
	if !pkg.Running() || !pkg.CanUninstall || pkg.Maintainer != "Synology Inc." {
		t.Errorf("nested additional fields were not flattened: %+v", pkg)
	}
}

func TestClient_GetPackage_NotFound(t *testing.T) {
	client, server := newPackageTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writePackageResponse(w, map[string]interface{}{"packages": []interface{}{}})
	})
	defer server.Close()

	_, err := client.GetPackage(context.Background(), "Missing")
	if !errors.Is(err, ErrPackageNotFound) {
		t.Fatalf("expected ErrPackageNotFound, got %v", err)
	}
}

func TestClient_ListPackageCatalog_FallsBackWhenV2IsEmpty(t *testing.T) {
	var versions []string
	client, server := newPackageTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = packageRequest(r)
		version := r.FormValue("version")
		versions = append(versions, version)
		if version == "2" {
			writePackageResponse(w, map[string]interface{}{"packages": []interface{}{}})
			return
		}
		writePackageResponse(w, map[string]interface{}{
			"data": []interface{}{map[string]interface{}{
				"package": "ContainerManager", "version": "24.0.2", "size": "1024", "beta": "false",
			}},
		})
	})
	defer server.Close()

	catalog, err := client.listPackageCatalog(context.Background())
	if err != nil {
		t.Fatalf("listPackageCatalog failed: %v", err)
	}
	if len(catalog) != 1 || catalog[0].ID != "ContainerManager" || catalog[0].Size != 1024 {
		t.Fatalf("unexpected catalog: %+v", catalog)
	}
	if len(versions) != 2 || versions[0] != "2" || versions[1] != "1" {
		t.Errorf("versions = %v, want [2 1]", versions)
	}
}

func TestClient_InstallPackage_ResolvesQueueAndWaits(t *testing.T) {
	restore := shrinkPackagePolling(t)
	defer restore()

	type installCall struct {
		name, volume, run, sid, token string
	}
	var mu sync.Mutex
	installed := map[string]string{}
	var calls []installCall

	client, server := newPackageTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		api, method := packageRequest(r)
		switch {
		case api == "SYNO.Core.Package" && method == "list":
			mu.Lock()
			packages := make([]interface{}, 0, len(installed))
			for id, status := range installed {
				packages = append(packages, map[string]interface{}{
					"id": id, "name": id, "version": "1.0", "additional": map[string]interface{}{"status": status, "ctl_uninstall": true},
				})
			}
			mu.Unlock()
			writePackageResponse(w, map[string]interface{}{"packages": packages})

		case api == "SYNO.Core.Package.Server" && method == "list":
			writePackageResponse(w, map[string]interface{}{"packages": []interface{}{
				map[string]interface{}{"package": "DockerDependency", "version": "1.0", "source": "synology"},
				map[string]interface{}{"package": "ContainerManager", "version": "2.0", "source": "synology"},
			}})

		case api == "SYNO.Core.Package.Installation" && method == "check":
			if r.FormValue("id") != "ContainerManager" || r.FormValue("version") != "2" {
				t.Errorf("unexpected installation check: id=%q version=%q", r.FormValue("id"), r.FormValue("version"))
			}
			writePackageResponse(w, map[string]interface{}{"is_occupied": false, "volume_path": "/volume1"})

		case api == "SYNO.Core.Package.Installation" && method == "get_queue":
			if pkgs := r.FormValue("pkgs"); !strings.Contains(pkgs, `"pkg":"ContainerManager"`) {
				t.Errorf("queue request does not contain package id: %s", pkgs)
			}
			writePackageResponse(w, map[string]interface{}{"queue": []interface{}{
				map[string]interface{}{"pkg": "DockerDependency", "volume": "/volume2"},
				map[string]interface{}{"pkg": "ContainerManager"},
			}})

		case api == "SYNO.Core.Package.Installation" && method == "install":
			call := installCall{
				name: r.FormValue("name"), volume: r.FormValue("volume_path"), run: r.FormValue("installrunpackage"),
				sid: r.URL.Query().Get("_sid"), token: r.URL.Query().Get("SynoToken"),
			}
			status := "stop"
			if call.run == "true" {
				status = "running"
			}
			mu.Lock()
			calls = append(calls, call)
			installed[call.name] = status
			mu.Unlock()
			writePackageResponse(w, map[string]interface{}{"progress": 0})

		default:
			t.Fatalf("unexpected request: %s %s", api, method)
		}
	})
	defer server.Close()

	pkg, err := client.InstallPackage(context.Background(), "ContainerManager", "/volume1", false)
	if err != nil {
		t.Fatalf("InstallPackage failed: %v", err)
	}
	if pkg.ID != "ContainerManager" || pkg.Running() {
		t.Errorf("unexpected installed package: %+v", pkg)
	}
	if len(calls) != 2 {
		t.Fatalf("got %d install calls, want 2: %+v", len(calls), calls)
	}
	if calls[0].name != "DockerDependency" || calls[0].volume != "/volume2" || calls[0].run != "true" {
		t.Errorf("unexpected dependency install: %+v", calls[0])
	}
	if calls[1].name != "ContainerManager" || calls[1].volume != "/volume1" || calls[1].run != "false" {
		t.Errorf("unexpected requested package install: %+v", calls[1])
	}
	for _, call := range calls {
		if call.sid != "test-sid" || call.token != "test-token" {
			t.Errorf("POST auth must be in query string: %+v", call)
		}
	}
}

func TestClient_SetPackageRunning(t *testing.T) {
	restore := shrinkPackagePolling(t)
	defer restore()

	status := "stop"
	client, server := newPackageTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		api, method := packageRequest(r)
		switch {
		case api == "SYNO.Core.Package.Control" && method == "start":
			if r.FormValue("id") != "ContainerManager" {
				t.Errorf("id = %q", r.FormValue("id"))
			}
			status = "running"
			writePackageResponse(w, map[string]interface{}{})
		case api == "SYNO.Core.Package" && method == "list":
			writePackageResponse(w, map[string]interface{}{"packages": []interface{}{
				map[string]interface{}{"id": "ContainerManager", "additional": map[string]interface{}{"status": status}},
			}})
		default:
			t.Fatalf("unexpected request: %s %s", api, method)
		}
	})
	defer server.Close()

	pkg, err := client.SetPackageRunning(context.Background(), "ContainerManager", true)
	if err != nil {
		t.Fatalf("SetPackageRunning failed: %v", err)
	}
	if !pkg.Running() {
		t.Errorf("package not running: %+v", pkg)
	}
}

func TestClient_SetPackageRunning_FailsImmediatelyWhenBroken(t *testing.T) {
	restore := shrinkPackagePolling(t)
	defer restore()

	client, server := newPackageTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		api, method := packageRequest(r)
		switch {
		case api == "SYNO.Core.Package.Control" && method == "start":
			writePackageResponse(w, map[string]interface{}{})
		case api == "SYNO.Core.Package" && method == "list":
			writePackageResponse(w, map[string]interface{}{"packages": []interface{}{
				map[string]interface{}{"id": "Broken", "additional": map[string]interface{}{"status": "broken"}},
			}})
		default:
			t.Fatalf("unexpected request: %s %s", api, method)
		}
	})
	defer server.Close()

	_, err := client.SetPackageRunning(context.Background(), "Broken", true)
	if err == nil || !strings.Contains(err.Error(), "is broken") {
		t.Fatalf("expected broken-package error, got %v", err)
	}
}

func TestClient_UninstallPackage(t *testing.T) {
	restore := shrinkPackagePolling(t)
	defer restore()

	installed := true
	client, server := newPackageTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		api, method := packageRequest(r)
		switch {
		case api == "SYNO.Core.Package" && method == "list":
			packages := []interface{}{}
			if installed {
				packages = append(packages, map[string]interface{}{
					"id": "Demo", "additional": map[string]interface{}{"status": "stop", "ctl_uninstall": true},
				})
			}
			writePackageResponse(w, map[string]interface{}{"packages": packages})
		case api == "SYNO.Core.Package.Uninstallation" && method == "uninstall":
			if r.FormValue("id") != "Demo" {
				t.Errorf("id = %q", r.FormValue("id"))
			}
			installed = false
			writePackageResponse(w, map[string]interface{}{})
		default:
			t.Fatalf("unexpected request: %s %s", api, method)
		}
	})
	defer server.Close()

	if err := client.UninstallPackage(context.Background(), "Demo"); err != nil {
		t.Fatalf("UninstallPackage failed: %v", err)
	}
}

func TestClient_UninstallPackage_RefusesSystemPackage(t *testing.T) {
	client, server := newPackageTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writePackageResponse(w, map[string]interface{}{"packages": []interface{}{
			map[string]interface{}{"id": "FileStation", "additional": map[string]interface{}{"status": "running", "ctl_uninstall": false}},
		}})
	})
	defer server.Close()

	err := client.UninstallPackage(context.Background(), "FileStation")
	if err == nil || !strings.Contains(err.Error(), "cannot be uninstalled") {
		t.Fatalf("expected system package refusal, got %v", err)
	}
}

func TestParsePackage_ToleratesTopLevelFields(t *testing.T) {
	pkg, err := parsePackage(json.RawMessage(`{
		"id":"Demo","name":"Demo Package","version":"1.2.3","status":"stop",
		"description":"Example","maintainer":"Example Corp","ctl_uninstall":"true"
	}`))
	if err != nil {
		t.Fatalf("parsePackage failed: %v", err)
	}
	if pkg.ID != "Demo" || pkg.Status != "stop" || !pkg.CanUninstall || pkg.Description != "Example" {
		t.Errorf("unexpected package: %+v", pkg)
	}
}

func shrinkPackagePolling(t *testing.T) func() {
	t.Helper()
	previousPoll, previousTimeout := packagePollInterval, packageTaskTimeout
	packagePollInterval = time.Millisecond
	packageTaskTimeout = time.Second
	return func() {
		packagePollInterval = previousPoll
		packageTaskTimeout = previousTimeout
	}
}
