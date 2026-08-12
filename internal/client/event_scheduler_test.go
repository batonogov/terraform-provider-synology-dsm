package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func eventSchedulerServer(t *testing.T, requests *[]capturedRequest) (*Client, *httptest.Server) {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/webapi/entry.cgi", func(w http.ResponseWriter, r *http.Request) {
		captured := recordRequest(r)
		*requests = append(*requests, captured)

		api := captured.taskParam("api")
		method := captured.taskParam("method")

		switch {
		case api == "SYNO.Core.User.PasswordConfirm" && method == "auth":
			raw, _ := json.Marshal(map[string]interface{}{"SynoConfirmPWToken": "confirm-token"})
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		case api == "SYNO.Core.User" && method == "list":
			raw, _ := json.Marshal(map[string]interface{}{
				"users": []map[string]interface{}{{"name": "operator", "uid": 1026}},
				"total": 1,
			})
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		case api == "SYNO.Core.User" && method == "get":
			raw, _ := json.Marshal(map[string]interface{}{
				"users": []map[string]interface{}{{"name": "operator", "uid": 1026}},
			})
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		case strings.HasPrefix(api, "SYNO.Core.EventScheduler") && (method == "create" || method == "set"):
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true})

		case api == "SYNO.Core.EventScheduler" && method == "get":
			raw, _ := json.Marshal(map[string]interface{}{
				"task_name":       "restore-mounts",
				"event":           "bootup",
				"enable":          true,
				"operation":       "/volume1/scripts/restore-mounts.sh",
				"operation_type":  "script",
				"owner":           map[string]interface{}{"0": "root"},
				"notify_enable":   false,
				"notify_mail":     "",
				"notify_if_error": false,
				"depend_on_task":  "[mount-check][network-up]",
			})
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		case api == "SYNO.Core.EventScheduler" && method == "delete":
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true})

		default:
			_ = json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 101}})
		}
	})

	server := httptest.NewServer(mux)
	c := NewClient(server.URL, "admin", "hunter2", false)
	c.setSession("test-sid", "test-token")
	return c, server
}

func TestClient_CreateEventTask_WireFormat(t *testing.T) {
	var requests []capturedRequest
	c, server := eventSchedulerServer(t, &requests)
	defer server.Close()

	_, err := c.CreateEventTask(context.Background(), EventTaskRequest{
		Name:      "restore-mounts",
		Owner:     "root",
		Event:     "bootup",
		Command:   "/volume1/scripts/restore-mounts.sh",
		Enabled:   true,
		DependsOn: []string{"mount-check", "network-up"},
	})
	if err != nil {
		t.Fatalf("CreateEventTask failed: %v", err)
	}

	create := findRequest(t, requests, "create")

	if create.Method != http.MethodPost {
		t.Errorf("create should be POST, got %s", create.Method)
	}
	if got := create.taskParam("api"); got != "SYNO.Core.EventScheduler.Root" {
		t.Errorf("api = %q, want the privileged API for a root-owned task", got)
	}
	if got := create.taskParam("version"); got != "1" {
		t.Errorf("version = %q, want 1", got)
	}
	if got := create.taskParam("SynoConfirmPWToken"); got != "confirm-token" {
		t.Errorf("SynoConfirmPWToken = %q", got)
	}

	// Event Scheduler takes JSON-quoted strings, unlike Task Scheduler.
	for param, want := range map[string]string{
		"task_name":      `"restore-mounts"`,
		"event":          `"bootup"`,
		"operation":      `"/volume1/scripts/restore-mounts.sh"`,
		"operation_type": `"script"`,
		"notify_mail":    `""`,
		"depend_on_task": `"[mount-check][network-up]"`,
	} {
		if got := create.taskParam(param); got != want {
			t.Errorf("%s = %s, want %s", param, got, want)
		}
	}

	// Booleans stay unquoted.
	if got := create.taskParam("enable"); got != "true" {
		t.Errorf("enable = %q, want an unquoted true", got)
	}
	if got := create.taskParam("notify_enable"); got != "false" {
		t.Errorf("notify_enable = %q, want false when no address is configured", got)
	}

	// owner is a uid-to-name map, not a plain username.
	var owner map[string]string
	if err := json.Unmarshal([]byte(create.taskParam("owner")), &owner); err != nil {
		t.Fatalf("owner is not JSON: %v", err)
	}
	if !reflect.DeepEqual(owner, map[string]string{"0": "root"}) {
		t.Errorf("owner = %#v, want root as uid 0", owner)
	}
}

func TestClient_CreateEventTask_ResolvesOwnerUID(t *testing.T) {
	var requests []capturedRequest
	c, server := eventSchedulerServer(t, &requests)
	defer server.Close()

	_, err := c.CreateEventTask(context.Background(), EventTaskRequest{
		Name:            "notify",
		Owner:           "operator",
		Event:           "shutdown",
		Command:         "/bin/true",
		Enabled:         true,
		NotifyEmail:     "ops@example.com",
		NotifyOnFailure: true,
	})
	if err != nil {
		t.Fatalf("CreateEventTask failed: %v", err)
	}

	create := findRequest(t, requests, "create")

	if got := create.taskParam("api"); got != "SYNO.Core.EventScheduler" {
		t.Errorf("api = %q, want the unprivileged API for a non-root owner", got)
	}
	if got := create.taskParam("SynoConfirmPWToken"); got != "" {
		t.Errorf("a non-root task must not carry a password confirmation token, got %q", got)
	}

	var owner map[string]string
	if err := json.Unmarshal([]byte(create.taskParam("owner")), &owner); err != nil {
		t.Fatalf("owner is not JSON: %v", err)
	}
	// The uid must come from DSM rather than being assumed; hardcoding 0 would
	// silently claim the account is root.
	if !reflect.DeepEqual(owner, map[string]string{"1026": "operator"}) {
		t.Errorf("owner = %#v, want the uid DSM reports for the account", owner)
	}

	if got := create.taskParam("notify_enable"); got != "true" {
		t.Errorf("notify_enable = %q, want true once an address is set", got)
	}
	if got := create.taskParam("notify_mail"); got != `"ops@example.com"` {
		t.Errorf("notify_mail = %s, want a quoted address", got)
	}
	if got := create.taskParam("notify_if_error"); got != "true" {
		t.Errorf("notify_if_error = %q, want true", got)
	}
}

func TestClient_CreateEventTask_RejectsUnknownEvent(t *testing.T) {
	var requests []capturedRequest
	c, server := eventSchedulerServer(t, &requests)
	defer server.Close()

	_, err := c.CreateEventTask(context.Background(), EventTaskRequest{
		Name:    "whenever",
		Owner:   "root",
		Event:   "midnight",
		Command: "/bin/true",
	})
	if err == nil {
		t.Fatal("expected an error for an unsupported event")
	}
	if !strings.Contains(err.Error(), "bootup or shutdown") {
		t.Errorf("error = %q, want it to name the supported events", err)
	}
	if len(requests) != 0 {
		t.Errorf("a rejected event must not reach DSM, but %d requests were sent", len(requests))
	}
}

func TestClient_GetEventTask(t *testing.T) {
	var requests []capturedRequest
	c, server := eventSchedulerServer(t, &requests)
	defer server.Close()

	task, err := c.GetEventTask(context.Background(), "restore-mounts")
	if err != nil {
		t.Fatalf("GetEventTask failed: %v", err)
	}

	get := findRequest(t, requests, "get")
	if got := get.taskParam("task_name"); got != `"restore-mounts"` {
		t.Errorf("task_name = %s, want a quoted name", got)
	}

	if task.Owner != "root" || task.OwnerUID != 0 {
		t.Errorf("owner = %q uid %d, want root uid 0", task.Owner, task.OwnerUID)
	}
	if task.Event != "bootup" {
		t.Errorf("event = %q", task.Event)
	}
	if task.Command != "/volume1/scripts/restore-mounts.sh" {
		t.Errorf("command = %q", task.Command)
	}
	if !reflect.DeepEqual(task.DependsOn, []string{"mount-check", "network-up"}) {
		t.Errorf("depends_on = %#v, want the bracketed list split back into names", task.DependsOn)
	}
}

func TestClient_GetEventTask_MissingTask(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/webapi/entry.cgi", func(w http.ResponseWriter, _ *http.Request) {
		// DSM answers an unknown name with an empty object rather than an error.
		_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: json.RawMessage(`{}`)})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := NewClient(server.URL, "admin", "", false)
	c.setSession("test-sid", "")

	_, err := c.GetEventTask(context.Background(), "absent")
	if err == nil {
		t.Fatal("expected an error for a missing task")
	}
	if !strings.Contains(err.Error(), ErrEventTaskNotFound.Error()) {
		t.Errorf("error = %q, want it to wrap ErrEventTaskNotFound so callers can drop the resource from state", err)
	}
}

func TestClient_UpdateEventTask(t *testing.T) {
	var requests []capturedRequest
	c, server := eventSchedulerServer(t, &requests)
	defer server.Close()

	_, err := c.UpdateEventTask(context.Background(), EventTaskRequest{
		Name:    "restore-mounts",
		Owner:   "operator",
		Event:   "shutdown",
		Command: "/bin/true",
		Enabled: false,
	})
	if err != nil {
		t.Fatalf("UpdateEventTask failed: %v", err)
	}

	set := findRequest(t, requests, "set")
	if got := set.taskParam("task_name"); got != `"restore-mounts"` {
		t.Errorf("task_name = %s, want the quoted name, which is the primary key", got)
	}
	if got := set.taskParam("event"); got != `"shutdown"` {
		t.Errorf("event = %s", got)
	}
	if got := set.taskParam("enable"); got != "false" {
		t.Errorf("enable = %q, want false", got)
	}
	if got := set.taskParam("api"); got != "SYNO.Core.EventScheduler" {
		t.Errorf("api = %q, want the unprivileged namespace for a non-root owner", got)
	}
}

func TestClient_DeleteEventTask(t *testing.T) {
	var requests []capturedRequest
	c, server := eventSchedulerServer(t, &requests)
	defer server.Close()

	if err := c.DeleteEventTask(context.Background(), "restore-mounts"); err != nil {
		t.Fatalf("DeleteEventTask failed: %v", err)
	}

	del := findRequest(t, requests, "delete")
	if got := del.taskParam("task_name"); got != `"restore-mounts"` {
		t.Errorf("task_name = %s, want a quoted name", got)
	}
	if got := del.taskParam("api"); got != "SYNO.Core.EventScheduler" {
		t.Errorf("api = %q", got)
	}
}

func TestParseDependOnTask(t *testing.T) {
	cases := map[string][]string{
		"":                       nil,
		"[one]":                  {"one"},
		"[one][two]":             {"one", "two"},
		"[with space][an-other]": {"with space", "an-other"},
	}
	for input, want := range cases {
		if got := parseDependOnTask(input); !reflect.DeepEqual(got, want) {
			t.Errorf("parseDependOnTask(%q) = %#v, want %#v", input, got, want)
		}
	}
}

// TestClient_CreateEventTaskReturnsTaskWhenReadBackFails mirrors the scheduled
// task case: DSM has already created a task that will run on every boot, so a
// failed read-back must still yield something the caller can record.
func TestClient_CreateEventTaskReturnsTaskWhenReadBackFails(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/webapi/entry.cgi", func(w http.ResponseWriter, r *http.Request) {
		captured := recordRequest(r)
		switch captured.taskParam("method") {
		case "auth":
			raw, _ := json.Marshal(map[string]interface{}{"SynoConfirmPWToken": "confirm-token"})
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})
		case "create":
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true})
		default: // the read-back
			_ = json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 105}})
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := NewClient(server.URL, "admin", "hunter2", false)
	c.setSession("test-sid", "test-token")

	task, err := c.CreateEventTask(context.Background(), EventTaskRequest{
		Name:    "restore-mounts",
		Owner:   "root",
		Event:   "bootup",
		Command: "/volume1/scripts/restore-mounts.sh",
		Enabled: true,
	})

	if err == nil {
		t.Fatal("expected the read-back failure to be reported")
	}
	if task == nil {
		t.Fatal("a task created on the NAS must be returned even when it cannot be read back")
	}
	if task.Name != "restore-mounts" || task.Event != "bootup" {
		t.Errorf("fallback task should describe what was requested, got %+v", task)
	}
}

// TestClient_ListEventTasksPropagatesFailures pins that a per-task lookup
// failure is reported rather than quietly shortening the list. A sweeper or an
// audit acting on a truncated list would conclude a task is absent when it is
// merely unreadable.
func TestClient_ListEventTasksPropagatesFailures(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/webapi/entry.cgi", func(w http.ResponseWriter, r *http.Request) {
		captured := recordRequest(r)
		switch captured.taskParam("method") {
		case "list":
			raw, _ := json.Marshal(map[string]interface{}{
				"tasks": []map[string]interface{}{
					{"name": "restore-mounts", "owner": "root", "enable": true},
				},
			})
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})
		default:
			// A session that expired between the list and the get.
			_ = json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 105}})
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := NewClient(server.URL, "admin", "", false)
	c.setSession("test-sid", "")

	tasks, err := c.ListEventTasks(context.Background())
	if err == nil {
		t.Fatalf("expected the lookup failure to surface, got %d tasks and no error", len(tasks))
	}
	if !IsAPIError(err, 105) {
		t.Errorf("error should carry the DSM code, got %v", err)
	}
}

// TestClient_ListEventTasksSkipsVanishedTasks is the complement: a task that
// disappeared between the list and the get is a benign race, not a failure.
func TestClient_ListEventTasksSkipsVanishedTasks(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/webapi/entry.cgi", func(w http.ResponseWriter, r *http.Request) {
		captured := recordRequest(r)
		if captured.taskParam("method") == "list" {
			raw, _ := json.Marshal(map[string]interface{}{
				"tasks": []map[string]interface{}{
					{"name": "gone", "owner": "root", "enable": true},
				},
			})
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})
			return
		}
		// DSM's "no such task" shape: success with an empty payload.
		_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: json.RawMessage(`{}`)})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := NewClient(server.URL, "admin", "", false)
	c.setSession("test-sid", "")

	tasks, err := c.ListEventTasks(context.Background())
	if err != nil {
		t.Fatalf("a vanished task should not fail the listing: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("got %d tasks, want the vanished one skipped", len(tasks))
	}
}

func TestJSONQuotedEscapes(t *testing.T) {
	// A command containing quotes must not break out of the parameter.
	if got := jsonQuoted(`echo "hi"`); got != `"echo \"hi\""` {
		t.Errorf("jsonQuoted = %s, want the inner quotes escaped", got)
	}
}
