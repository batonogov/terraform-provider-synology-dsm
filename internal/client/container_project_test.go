package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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
	if err := readContainerProjectActionStream(response); err == nil || !strings.Contains(err.Error(), "api error 105") {
		t.Fatalf("expected streamed API error, got %v", err)
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
