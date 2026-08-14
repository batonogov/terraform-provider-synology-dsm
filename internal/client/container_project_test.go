package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newContainerProjectTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	c := NewClient(server.URL, "admin", "password", false)
	c.setSession("test-sid", "test-token")
	return c, server
}

func containerProjectRequest(r *http.Request) (api, method string) {
	_ = r.ParseForm()
	return r.FormValue("api"), r.FormValue("method")
}

func writeContainerProjectResponse(w http.ResponseWriter, data interface{}) {
	raw, _ := json.Marshal(data)
	_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})
}

func writeContainerProjectError(w http.ResponseWriter, code int) {
	_ = json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: code}})
}

func TestClient_ListContainerProjects_ParsesUUIDMap(t *testing.T) {
	client, server := newContainerProjectTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		api, method := containerProjectRequest(r)
		if r.Method != http.MethodPost || api != "SYNO.Docker.Project" || method != "list" {
			t.Fatalf("unexpected request: %s %s %s", r.Method, api, method)
		}
		writeContainerProjectResponse(w, map[string]interface{}{
			"project-uuid": map[string]interface{}{
				"name": "s3-storage", "share_path": "/docker/s3-storage", "path": "/volume1/docker/s3-storage",
				"status": "RUNNING", "container_ids": []string{"container-1", "container-2"},
			},
		})
	})
	defer server.Close()

	projects, err := client.ListContainerProjects(context.Background())
	if err != nil {
		t.Fatalf("ListContainerProjects failed: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("got %d projects, want 1", len(projects))
	}
	project := projects[0]
	if project.ID != "project-uuid" || project.Name != "s3-storage" || project.SharePath != "/docker/s3-storage" || !project.Running() {
		t.Errorf("unexpected project: %+v", project)
	}
	if !reflect.DeepEqual(project.ContainerIDs, []string{"container-1", "container-2"}) {
		t.Errorf("container IDs = %v", project.ContainerIDs)
	}
}

func TestClient_GetContainerProject_FallsBackToShareInfo(t *testing.T) {
	const compose = "services:\n  object-storage:\n    image: minio/minio:latest\n"
	client, server := newContainerProjectTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		api, method := containerProjectRequest(r)
		switch method {
		case "get":
			if api != "SYNO.Docker.Project" || r.FormValue("id") != "project-uuid" {
				t.Errorf("unexpected get: api=%q id=%q", api, r.FormValue("id"))
			}
			writeContainerProjectResponse(w, map[string]interface{}{
				"id": "project-uuid", "name": "s3-storage", "share_path": "/docker/s3-storage",
				"path": "/volume1/docker/s3-storage", "status": "STOPPED",
			})
		case "get_share_info":
			if got := r.FormValue("path"); got != `"/volume1/docker/s3-storage"` {
				t.Errorf("path = %q, want JSON-quoted absolute path", got)
			}
			writeContainerProjectResponse(w, map[string]interface{}{"content": compose, "compose_path": "/volume1/docker/s3-storage/compose.yaml"})
		default:
			t.Fatalf("unexpected request: %s %s", api, method)
		}
	})
	defer server.Close()

	project, err := client.GetContainerProject(context.Background(), "project-uuid")
	if err != nil {
		t.Fatalf("GetContainerProject failed: %v", err)
	}
	if project.ComposeYAML != compose {
		t.Errorf("compose = %q, want %q", project.ComposeYAML, compose)
	}
}

func TestClient_GetContainerProject_NotFound(t *testing.T) {
	client, server := newContainerProjectTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeContainerProjectError(w, 2104)
	})
	defer server.Close()

	_, err := client.GetContainerProject(context.Background(), "missing")
	if !errors.Is(err, ErrContainerProjectNotFound) {
		t.Fatalf("expected ErrContainerProjectNotFound, got %v", err)
	}
}

func TestParseContainerProject_AcceptsUUIDKeyedResponse(t *testing.T) {
	project, err := parseContainerProject(json.RawMessage(`{
		"project-uuid":{"name":"app","status":"RUNNING","share_path":"/docker/app"}
	}`), "project-uuid")
	if err != nil {
		t.Fatalf("parseContainerProject failed: %v", err)
	}
	if project.ID != "project-uuid" || project.Name != "app" || !project.Running() {
		t.Fatalf("unexpected keyed project: %+v", project)
	}
}

func TestClient_CreateContainerProject_UsesDSMSequence(t *testing.T) {
	restore := shrinkContainerProjectPolling(t)
	defer restore()

	const compose = "services:\n  object-storage:\n    image: minio/minio:latest\n"
	created := false
	storedCompose := ""
	status := "STOPPED"
	var actions []string

	client, server := newContainerProjectTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		api, method := containerProjectRequest(r)
		if r.Method == http.MethodPost && (r.URL.Query().Get("_sid") != "test-sid" || r.URL.Query().Get("SynoToken") != "test-token") {
			t.Errorf("POST auth missing from query: %s", r.URL.RawQuery)
		}
		switch {
		case api == "SYNO.Docker.Project" && method == "list":
			projects := map[string]interface{}{}
			if created {
				projects["project-uuid"] = map[string]interface{}{
					"id": "project-uuid", "name": "s3-storage", "share_path": "/docker/s3-storage",
					"path": "/volume1/docker/s3-storage", "status": status, "content": storedCompose,
				}
			}
			writeContainerProjectResponse(w, projects)
		case api == "SYNO.FileStation.CreateFolder" && method == "create":
			if r.FormValue("folder_path") != "/docker" || r.FormValue("name") != "s3-storage" || r.FormValue("force_parent") != "true" {
				t.Errorf("unexpected folder request: %v", r.Form)
			}
			actions = append(actions, "folder")
			writeContainerProjectResponse(w, map[string]interface{}{})
		case api == "SYNO.Docker.Project" && method == "create":
			if r.FormValue("name") != `"s3-storage"` || r.FormValue("share_path") != `"/docker/s3-storage"` || r.FormValue("content") != `""` {
				t.Errorf("create strings must be JSON-quoted: %v", r.Form)
			}
			created = true
			actions = append(actions, "create")
			writeContainerProjectResponse(w, map[string]interface{}{})
		case api == "SYNO.Docker.Project" && method == "update":
			if r.FormValue("id") != "project-uuid" || r.FormValue("content") != compose {
				t.Errorf("compose update must be raw form content: id=%q content=%q", r.FormValue("id"), r.FormValue("content"))
			}
			storedCompose = r.FormValue("content")
			actions = append(actions, "update")
			writeContainerProjectResponse(w, map[string]interface{}{})
		case api == "SYNO.Docker.Project" && method == "build":
			actions = append(actions, "build")
			status = "STOPPED"
			writeContainerProjectResponse(w, map[string]interface{}{})
		case api == "SYNO.Docker.Project" && method == "start":
			actions = append(actions, "start")
			status = "RUNNING"
			writeContainerProjectResponse(w, map[string]interface{}{})
		case api == "SYNO.Docker.Project" && method == "get":
			writeContainerProjectResponse(w, map[string]interface{}{
				"id": "project-uuid", "name": "s3-storage", "share_path": "/docker/s3-storage",
				"path": "/volume1/docker/s3-storage", "status": status, "content": storedCompose,
			})
		default:
			t.Fatalf("unexpected request: %s %s", api, method)
		}
	})
	defer server.Close()

	project, err := client.CreateContainerProject(context.Background(), "s3-storage", "/docker/s3-storage", compose, true)
	if err != nil {
		t.Fatalf("CreateContainerProject failed: %v", err)
	}
	if !project.Running() || project.ComposeYAML != compose {
		t.Errorf("unexpected project: %+v", project)
	}
	if !reflect.DeepEqual(actions, []string{"folder", "create", "update", "build", "start"}) {
		t.Errorf("actions = %v", actions)
	}
}

func TestClient_CreateContainerProject_RejectsExistingName(t *testing.T) {
	client, server := newContainerProjectTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, method := containerProjectRequest(r)
		if method != "list" {
			t.Fatalf("existing project must be detected before mutation, got %q", method)
		}
		writeContainerProjectResponse(w, map[string]interface{}{
			"project-uuid": map[string]interface{}{"id": "project-uuid", "name": "existing"},
		})
	})
	defer server.Close()

	_, err := client.CreateContainerProject(context.Background(), "existing", "/docker/existing", "services: {}", true)
	if err == nil || !strings.Contains(err.Error(), "import it instead") {
		t.Fatalf("expected import guidance, got %v", err)
	}
}

func TestClient_UpdateContainerProject_RebuildsAndRestarts(t *testing.T) {
	restore := shrinkContainerProjectPolling(t)
	defer restore()

	const oldCompose = "services:\n  app:\n    image: example/app:1\n"
	const newCompose = "services:\n  app:\n    image: example/app:2\n"
	storedCompose := oldCompose
	status := "RUNNING"
	var actions []string

	client, server := newContainerProjectTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		api, method := containerProjectRequest(r)
		if api != "SYNO.Docker.Project" {
			t.Fatalf("unexpected API %q", api)
		}
		switch method {
		case "get":
			writeContainerProjectResponse(w, map[string]interface{}{
				"id": "project-uuid", "name": "app", "share_path": "/docker/app", "status": status, "content": storedCompose,
			})
		case "stop":
			actions = append(actions, "stop")
			status = "STOPPED"
			writeContainerProjectResponse(w, map[string]interface{}{})
		case "update":
			actions = append(actions, "update")
			storedCompose = r.FormValue("content")
			writeContainerProjectResponse(w, map[string]interface{}{})
		case "build":
			actions = append(actions, "build")
			writeContainerProjectResponse(w, map[string]interface{}{})
		case "start":
			actions = append(actions, "start")
			status = "RUNNING"
			writeContainerProjectResponse(w, map[string]interface{}{})
		default:
			t.Fatalf("unexpected method %q", method)
		}
	})
	defer server.Close()

	project, err := client.UpdateContainerProject(context.Background(), "project-uuid", newCompose, true)
	if err != nil {
		t.Fatalf("UpdateContainerProject failed: %v", err)
	}
	if project.ComposeYAML != newCompose || !project.Running() {
		t.Errorf("unexpected project: %+v", project)
	}
	if !reflect.DeepEqual(actions, []string{"stop", "update", "build", "start"}) {
		t.Errorf("actions = %v", actions)
	}
}

// TestClient_SetContainerProjectRunning covers the path a caller takes when it
// has no compose document to offer: managed write-only, the document is not in
// state and its source may be gone. Nothing may be written or rebuilt.
func TestClient_SetContainerProjectRunning(t *testing.T) {
	restore := shrinkContainerProjectPolling(t)
	defer restore()

	status := "RUNNING"
	var actions []string
	client, server := newContainerProjectTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, method := containerProjectRequest(r)
		switch method {
		case "get":
			writeContainerProjectResponse(w, map[string]interface{}{"id": "project-uuid", "name": "app", "status": status, "content": "services: {}\n"})
		case "stop":
			actions = append(actions, "stop")
			status = "STOPPED"
			writeContainerProjectResponse(w, map[string]interface{}{})
		default:
			t.Fatalf("unexpected method %q — a running-state change must not touch the compose document", method)
		}
	})
	defer server.Close()

	project, err := client.SetContainerProjectRunning(context.Background(), "project-uuid", false)
	if err != nil {
		t.Fatalf("SetContainerProjectRunning failed: %v", err)
	}
	if project.Running() {
		t.Errorf("project reports running after a stop: %+v", project)
	}
	if !reflect.DeepEqual(actions, []string{"stop"}) {
		t.Errorf("actions = %v, want a single stop", actions)
	}

	// Already in the requested state: nothing to do at all.
	actions = nil
	if _, err := client.SetContainerProjectRunning(context.Background(), "project-uuid", false); err != nil {
		t.Fatalf("SetContainerProjectRunning failed: %v", err)
	}
	if len(actions) != 0 {
		t.Errorf("actions = %v, want none", actions)
	}
}

func TestClient_UpdateContainerProject_ChangesRunningStateWithoutBuild(t *testing.T) {
	restore := shrinkContainerProjectPolling(t)
	defer restore()

	const compose = "services: {}\n"
	status := "STOPPED"
	var actions []string
	client, server := newContainerProjectTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, method := containerProjectRequest(r)
		switch method {
		case "get":
			writeContainerProjectResponse(w, map[string]interface{}{"id": "project-uuid", "name": "app", "status": status, "content": compose})
		case "start":
			actions = append(actions, "start")
			status = "RUNNING"
			writeContainerProjectResponse(w, map[string]interface{}{})
		default:
			t.Fatalf("unexpected method %q", method)
		}
	})
	defer server.Close()

	if _, err := client.UpdateContainerProject(context.Background(), "project-uuid", compose, true); err != nil {
		t.Fatalf("UpdateContainerProject failed: %v", err)
	}
	if !reflect.DeepEqual(actions, []string{"start"}) {
		t.Errorf("actions = %v", actions)
	}
}

func TestClient_ContainerProjectAction_FallsBackToStream(t *testing.T) {
	client, server := newContainerProjectTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, method := containerProjectRequest(r)
		switch method {
		case "build":
			if r.Method != http.MethodPost {
				t.Errorf("direct action method = %s, want POST", r.Method)
			}
			writeContainerProjectError(w, 103)
		case "build_stream":
			if r.Method != http.MethodGet {
				t.Errorf("stream action method = %s, want GET", r.Method)
			}
			if r.FormValue("id") != `"project-uuid"` {
				t.Errorf("stream id = %q, want JSON-quoted UUID", r.FormValue("id"))
			}
			_, _ = w.Write([]byte("{\"success\":true,\"data\":{\"message\":\"building\"}}\n"))
			_, _ = w.Write([]byte("{\"progress\":100}\n"))
		default:
			t.Fatalf("unexpected method %q", method)
		}
	})
	defer server.Close()

	if err := client.runContainerProjectAction(context.Background(), "project-uuid", "build"); err != nil {
		t.Fatalf("stream fallback failed: %v", err)
	}
}

func TestReadContainerProjectActionStream_ReturnsAPIError(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("data: {\"success\":false,\"error\":{\"code\":105}}\n")),
	}
	_, err := readContainerProjectActionStream(response)
	if !IsAPIError(err, 105) {
		t.Fatalf("expected streamed API error 105, got %v", err)
	}
}

func TestClient_DeleteContainerProject_PreservesContent(t *testing.T) {
	restore := shrinkContainerProjectPolling(t)
	defer restore()

	exists := true
	client, server := newContainerProjectTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, method := containerProjectRequest(r)
		switch method {
		case "get":
			if !exists {
				writeContainerProjectError(w, 2104)
				return
			}
			writeContainerProjectResponse(w, map[string]interface{}{"id": "project-uuid", "name": "app", "content": "services: {}", "status": "STOPPED"})
		case "delete":
			if r.FormValue("id") != "project-uuid" || r.FormValue("preserve_content") != "true" {
				t.Errorf("unsafe delete params: %v", r.Form)
			}
			exists = false
			writeContainerProjectResponse(w, map[string]interface{}{})
		default:
			t.Fatalf("unexpected method %q", method)
		}
	})
	defer server.Close()

	if err := client.DeleteContainerProject(context.Background(), "project-uuid"); err != nil {
		t.Fatalf("DeleteContainerProject failed: %v", err)
	}
}

func TestClient_CreateContainerProject_RejectsSharedFolderRoot(t *testing.T) {
	client, server := newContainerProjectTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, method := containerProjectRequest(r)
		if method != "list" {
			t.Fatalf("unexpected method %q", method)
		}
		writeContainerProjectResponse(w, map[string]interface{}{})
	})
	defer server.Close()

	_, err := client.CreateContainerProject(context.Background(), "invalid", "/docker", "services: {}", false)
	if err == nil || !strings.Contains(err.Error(), "inside a DSM shared folder") {
		t.Fatalf("expected nested-directory error, got %v", err)
	}
}

func shrinkContainerProjectPolling(t *testing.T) func() {
	t.Helper()
	originalInterval := containerProjectPollInterval
	originalTimeout := containerProjectTaskTimeout
	containerProjectPollInterval = time.Millisecond
	containerProjectTaskTimeout = 100 * time.Millisecond
	return func() {
		containerProjectPollInterval = originalInterval
		containerProjectTaskTimeout = originalTimeout
	}
}

// TestReadContainerProjectActionStream_CollectsOutput covers #67: the plain-text
// lines are Container Manager's own diagnosis, and dropping them is what left
// users with a bare "unexpected DSM error (code 2202)".
func TestReadContainerProjectActionStream_CollectsOutput(t *testing.T) {
	body := strings.Join([]string{
		`data: {"success":true}`,
		` Container edge-caddy-config-1  Starting`,
		`Error response from daemon: Bind mount failed: '/volume1/containers/edge/conf' does not exist`,
		`Exit Code: 1`,
		`data: {"success":false,"error":{"code":2202}}`,
	}, "\n")

	response := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	output, err := readContainerProjectActionStream(response)
	if !IsAPIError(err, 2202) {
		t.Fatalf("expected the streamed 2202, got %v", err)
	}
	if len(output) != 3 {
		t.Fatalf("expected the three text lines, got %d: %v", len(output), output)
	}
	if !strings.Contains(strings.Join(output, "\n"), "Bind mount failed") {
		t.Errorf("the actual reason must survive: %v", output)
	}
}

// TestFormatProjectDiagnostics_KeepsTheTail: a failing build ends with the
// reason, so the tail is what matters; the image-pull progress before it would
// bury the diagnostic.
func TestFormatProjectDiagnostics_KeepsTheTail(t *testing.T) {
	if got := formatProjectDiagnostics(nil); got != "" {
		t.Errorf("no output should add nothing to the error, got %q", got)
	}

	lines := make([]string, 0, maxProjectDiagnosticLines+5)
	for i := range maxProjectDiagnosticLines + 5 {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	lines[len(lines)-1] = "Exit Code: 1"

	got := formatProjectDiagnostics(lines)
	if !strings.Contains(got, "Exit Code: 1") {
		t.Errorf("the last line must be kept: %s", got)
	}
	if strings.Contains(got, "line 0") {
		t.Errorf("the oldest lines should be dropped: %s", got)
	}
	if n := strings.Count(got, "\n  "); n != maxProjectDiagnosticLines {
		t.Errorf("expected %d retained lines, got %d", maxProjectDiagnosticLines, n)
	}
}

// TestContainerProject_RunningAcceptsWarning covers #69: a compose file with a
// one-shot init container settles on WARNING — the init container exited 0 and
// the long-lived services are up. Treating that as "not running yet" cost a
// ten-minute timeout and failed an apply whose services were fine.
func TestContainerProject_RunningAcceptsWarning(t *testing.T) {
	tests := []struct {
		status           string
		running          bool
		partiallyRunning bool
	}{
		{"RUNNING", true, false},
		{"running", true, false},
		{"WARNING", true, true},
		{"warning", true, true},
		{"STOPPED", false, false},
		{"BUILD_FAILED", false, false},
		{"", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			project := ContainerProject{Status: tt.status}
			if got := project.Running(); got != tt.running {
				t.Errorf("Running() = %v, want %v", got, tt.running)
			}
			if got := project.PartiallyRunning(); got != tt.partiallyRunning {
				t.Errorf("PartiallyRunning() = %v, want %v", got, tt.partiallyRunning)
			}
		})
	}
}

// TestWaitForContainerProject_SettlesOnWarning is the end-to-end half of #69:
// the wait must return as soon as DSM reports WARNING, not poll until the
// deadline.
func TestWaitForContainerProject_SettlesOnWarning(t *testing.T) {
	restore := shrinkContainerProjectPolling(t)
	defer restore()

	var polls atomic.Int32
	client, server := newContainerProjectTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, method := containerProjectRequest(r)
		if method != "get" {
			t.Fatalf("unexpected method %q", method)
		}
		// STARTING first, then the steady WARNING an init container leaves behind.
		status := "WARNING"
		if polls.Add(1) == 1 {
			status = "STARTING"
		}
		writeContainerProjectResponse(w, map[string]interface{}{
			"id": "project-uuid", "name": "gitlab-runner", "share_path": "/docker/gitlab-runner",
			"path": "/volume1/docker/gitlab-runner", "status": status, "content": "services: {}\n",
		})
	})
	defer server.Close()

	project, err := client.waitForContainerProjectRunningState(context.Background(), "project-uuid", true)
	if err != nil {
		t.Fatalf("wait should settle on WARNING, got: %v", err)
	}
	if project.Status != "WARNING" {
		t.Errorf("status = %q, want WARNING", project.Status)
	}
	if got := polls.Load(); got != 2 {
		t.Errorf("expected the wait to stop as soon as the status settled, polled %d times", got)
	}
}

// TestUpdateContainerProjectCompose_RecoversFromLateResponse is the core of #70:
// DSM finished the write and answered too late. Treating that as a failure left
// the project on the NAS but out of state, and every later apply then failed
// with "already exists; import it instead".
func TestUpdateContainerProjectCompose_RecoversFromLateResponse(t *testing.T) {
	const compose = "services:\n  app:\n    image: nginx\n"
	var updates atomic.Int32

	client, server := newContainerProjectTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, method := containerProjectRequest(r)
		switch method {
		case "update":
			// Answer far too late: the client gives up while DSM completes the write.
			updates.Add(1)
			time.Sleep(150 * time.Millisecond)
			writeContainerProjectResponse(w, map[string]interface{}{})
		case "get":
			// The write did land, which is exactly the situation being recovered.
			stored := ""
			if updates.Load() > 0 {
				stored = compose
			}
			writeContainerProjectResponse(w, map[string]interface{}{
				"id": "project-uuid", "name": "app", "share_path": "/docker/app",
				"path": "/volume1/docker/app", "status": "STOPPED", "content": stored,
			})
		default:
			t.Fatalf("unexpected method %q", method)
		}
	})
	defer server.Close()

	client.slowClient.Timeout = 30 * time.Millisecond

	if err := client.updateContainerProjectCompose(context.Background(), "project-uuid", compose); err != nil {
		t.Fatalf("a late response with the write completed must not fail: %v", err)
	}
}

// TestUpdateContainerProjectCompose_TimeoutWithoutWriteStillFails guards the
// other direction: recovery must confirm the content, not assume it.
func TestUpdateContainerProjectCompose_TimeoutWithoutWriteStillFails(t *testing.T) {
	client, server := newContainerProjectTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, method := containerProjectRequest(r)
		switch method {
		case "update":
			time.Sleep(150 * time.Millisecond)
			writeContainerProjectResponse(w, map[string]interface{}{})
		case "get":
			// DSM still holds the old compose: the write really did not happen.
			writeContainerProjectResponse(w, map[string]interface{}{
				"id": "project-uuid", "name": "app", "share_path": "/docker/app",
				"path": "/volume1/docker/app", "status": "STOPPED", "content": "services: {}\n",
			})
		default:
			t.Fatalf("unexpected method %q", method)
		}
	})
	defer server.Close()

	client.slowClient.Timeout = 30 * time.Millisecond

	err := client.updateContainerProjectCompose(context.Background(), "project-uuid", "services:\n  app:\n    image: nginx\n")
	if err == nil {
		t.Fatal("a timeout with no write must still fail")
	}
	if !IsTimeoutError(err) {
		t.Errorf("the timeout should survive as the cause, got: %v", err)
	}
}

// TestClient_CreateContainerProject_SettlesWhenBuildLeavesWarning is issue #101.
//
// A compose file with a one-shot container leaves the project in WARNING as
// soon as the build has run: the container did its work and exited. The create
// path then waited for the project to be *not running* before starting it —
// and WARNING counts as running since #73, so the wait could only end at the
// ten-minute deadline, failing an apply whose services were about to be fine.
func TestClient_CreateContainerProject_SettlesWhenBuildLeavesWarning(t *testing.T) {
	restore := shrinkContainerProjectPolling(t)
	defer restore()

	const compose = "services:\n  app:\n    image: nextcloud:33-fpm-alpine\n  provision:\n    image: nextcloud:33-fpm-alpine\n    restart: \"no\"\n"
	created := false
	storedCompose := ""
	// STOPPED until the build runs; WARNING from then on, which is what DSM
	// reports once the one-shot container has exited 0.
	status := "STOPPED"
	var actions []string

	client, server := newContainerProjectTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		api, method := containerProjectRequest(r)
		project := map[string]interface{}{
			"id": "project-uuid", "name": "nextcloud", "share_path": "/docker/nextcloud",
			"path": "/volume1/docker/nextcloud", "status": status, "content": storedCompose,
		}
		switch {
		case api == "SYNO.Docker.Project" && method == "list":
			projects := map[string]interface{}{}
			if created {
				projects["project-uuid"] = project
			}
			writeContainerProjectResponse(w, projects)
		case api == "SYNO.FileStation.CreateFolder" && method == "create":
			writeContainerProjectResponse(w, map[string]interface{}{})
		case api == "SYNO.Docker.Project" && method == "create":
			created = true
			writeContainerProjectResponse(w, map[string]interface{}{})
		case api == "SYNO.Docker.Project" && method == "update":
			storedCompose = r.FormValue("content")
			writeContainerProjectResponse(w, map[string]interface{}{})
		case api == "SYNO.Docker.Project" && method == "build":
			actions = append(actions, "build")
			status = "WARNING"
			writeContainerProjectResponse(w, map[string]interface{}{})
		case api == "SYNO.Docker.Project" && method == "start":
			actions = append(actions, "start")
			// Still WARNING after start: the one-shot container stays exited.
			status = "WARNING"
			writeContainerProjectResponse(w, map[string]interface{}{})
		case api == "SYNO.Docker.Project" && method == "get":
			writeContainerProjectResponse(w, project)
		default:
			t.Fatalf("unexpected request: %s %s", api, method)
		}
	})
	defer server.Close()

	result, err := client.CreateContainerProject(context.Background(), "nextcloud", "/docker/nextcloud", compose, true)
	if err != nil {
		t.Fatalf("create must not wait out the deadline on WARNING: %v", err)
	}
	if result.Status != "WARNING" || !result.Running() {
		t.Errorf("unexpected project: %+v", result)
	}
	// The build must not swallow the start: a project that never starts would
	// pass a "did it settle" check while doing nothing.
	if !reflect.DeepEqual(actions, []string{"build", "start"}) {
		t.Errorf("actions = %v, want the build to be followed by a start", actions)
	}
}

// TestContainerProjectReached_WarningCountsAsStopped covers the same trap in the
// other direction: with running = false nothing ever issues a start, so a
// project left in WARNING by its build would wait out the deadline too.
func TestContainerProjectReached_WarningCountsAsStopped(t *testing.T) {
	tests := []struct {
		status  string
		running bool
		want    bool
	}{
		{status: "WARNING", running: true, want: true},
		{status: "WARNING", running: false, want: true},
		{status: "RUNNING", running: true, want: true},
		// Only an outright RUNNING keeps "stopped" unreached.
		{status: "RUNNING", running: false, want: false},
		{status: "STOPPED", running: false, want: true},
		{status: "STOPPED", running: true, want: false},
		// Transient statuses are never the destination, in either direction.
		{status: "BUILDING", running: true, want: false},
		{status: "BUILDING", running: false, want: false},
		{status: "STOPPING", running: false, want: false},
	}

	for _, tt := range tests {
		got := containerProjectReached(&ContainerProject{Status: tt.status}, tt.running)
		if got != tt.want {
			t.Errorf("containerProjectReached(%q, running=%v) = %v, want %v", tt.status, tt.running, got, tt.want)
		}
	}
}
