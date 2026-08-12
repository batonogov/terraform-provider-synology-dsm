package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func intPtr(v int) *int { return &v }

// capturedRequest records what the client actually put on the wire. The point
// of these tests is the request, not the response: the DSM Task Scheduler API
// is undocumented, so the encoding is the part that can silently rot.
type capturedRequest struct {
	Method string
	Form   map[string]string
	// Query and Body are kept apart because DSM's session handling depends on
	// the difference: _sid and SynoToken are only honoured in the query string.
	Query url.Values
	Body  url.Values
}

// taskParam reads a parameter regardless of whether the client sent it as a
// query parameter or a form body value.
func (c capturedRequest) taskParam(name string) string {
	return c.Form[name]
}

func recordRequest(r *http.Request) capturedRequest {
	_ = r.ParseForm()
	captured := capturedRequest{
		Method: r.Method,
		Form:   map[string]string{},
		Query:  r.URL.Query(),
		Body:   r.PostForm,
	}
	for key := range r.Form {
		captured.Form[key] = r.Form.Get(key)
	}
	return captured
}

// taskSchedulerServer answers the Task Scheduler calls the client makes and
// records every request for inspection.
func taskSchedulerServer(t *testing.T, requests *[]capturedRequest) (*Client, *httptest.Server) {
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

		case strings.HasPrefix(api, "SYNO.Core.TaskScheduler") && method == "create":
			raw, _ := json.Marshal(map[string]interface{}{"id": 42})
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		case strings.HasPrefix(api, "SYNO.Core.TaskScheduler") && method == "set":
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true})

		case api == "SYNO.Core.TaskScheduler" && method == "list":
			raw, _ := json.Marshal(map[string]interface{}{
				"tasks": []map[string]interface{}{
					{"id": 42, "name": "prune", "owner": "root", "real_owner": "root", "enable": true, "type": "script"},
					// An event task: no id. It must not appear as a scheduled task.
					{"name": "restore-mounts", "owner": "root", "real_owner": "root", "enable": true},
				},
				"total": 2,
			})
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		case api == "SYNO.Core.TaskScheduler" && method == "get":
			raw, _ := json.Marshal(map[string]interface{}{
				"id":         42,
				"name":       "prune",
				"owner":      "root",
				"real_owner": "root",
				"enable":     true,
				"type":       "script",
				"extra": map[string]interface{}{
					"notify_enable":   true,
					"notify_if_error": true,
					"notify_mail":     "ops@example.com",
					"script":          "/usr/local/bin/docker system prune -af",
				},
				"schedule": map[string]interface{}{
					"date_type":      0,
					"repeat_date":    1002,
					"week_day":       "0,3",
					"monthly_week":   []string{},
					"hour":           4,
					"minute":         30,
					"repeat_hour":    0,
					"repeat_min":     0,
					"last_work_hour": 4,
					"version":        4,
				},
			})
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		case api == "SYNO.Core.TaskScheduler" && method == "delete":
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

func findRequest(t *testing.T, requests []capturedRequest, method string) capturedRequest {
	t.Helper()
	for _, req := range requests {
		if req.taskParam("method") == method {
			return req
		}
	}
	t.Fatalf("no request with method %q was sent; got %d requests", method, len(requests))
	return capturedRequest{}
}

func TestClient_CreateScheduledTask_WireFormat(t *testing.T) {
	var requests []capturedRequest
	c, server := taskSchedulerServer(t, &requests)
	defer server.Close()

	_, err := c.CreateScheduledTask(context.Background(), CreateScheduledTaskRequest{
		Name:            "prune",
		Owner:           "operator",
		Command:         "/usr/local/bin/docker system prune -af",
		Enabled:         true,
		NotifyEmail:     "ops@example.com",
		NotifyOnFailure: true,
		Schedule: TaskSchedule{
			Frequency:  "weekly",
			DaysOfWeek: []string{"wednesday", "sunday"},
			Hour:       4,
			Minute:     30,
		},
	})
	if err != nil {
		t.Fatalf("CreateScheduledTask failed: %v", err)
	}

	create := findRequest(t, requests, "create")

	if create.Method != http.MethodPost {
		t.Errorf("create should be POST, got %s", create.Method)
	}
	if got := create.taskParam("api"); got != "SYNO.Core.TaskScheduler" {
		t.Errorf("api = %q, want the unprivileged SYNO.Core.TaskScheduler for a non-root owner", got)
	}
	if got := create.taskParam("version"); got != "4" {
		t.Errorf("version = %q, want 4", got)
	}
	if got := create.taskParam("type"); got != "script" {
		t.Errorf("type = %q, want script", got)
	}
	if got := create.taskParam("owner"); got != "operator" {
		t.Errorf("owner = %q, want operator", got)
	}
	if got := create.taskParam("real_owner"); got != "operator" {
		t.Errorf("real_owner = %q, want operator", got)
	}
	if got := create.taskParam("enable"); got != "true" {
		t.Errorf("enable = %q, want true", got)
	}
	if got := create.taskParam("SynoConfirmPWToken"); got != "" {
		t.Errorf("a non-root task must not carry a password confirmation token, got %q", got)
	}

	var extra map[string]interface{}
	if err := json.Unmarshal([]byte(create.taskParam("extra")), &extra); err != nil {
		t.Fatalf("extra is not JSON: %v", err)
	}
	wantExtra := map[string]interface{}{
		"notify_enable":   true,
		"notify_if_error": true,
		"notify_mail":     "ops@example.com",
		"script":          "/usr/local/bin/docker system prune -af",
	}
	if !reflect.DeepEqual(extra, wantExtra) {
		t.Errorf("extra = %#v, want %#v", extra, wantExtra)
	}

	var schedule map[string]interface{}
	if err := json.Unmarshal([]byte(create.taskParam("schedule")), &schedule); err != nil {
		t.Fatalf("schedule is not JSON: %v", err)
	}
	wantSchedule := map[string]interface{}{
		"date_type":   float64(0),
		"repeat_date": float64(1002),
		// Days are rendered as ascending DSM digits with Sunday = 0, regardless of
		// the order the caller listed them in.
		"week_day":       "0,3",
		"monthly_week":   []interface{}{},
		"hour":           float64(4),
		"minute":         float64(30),
		"repeat_hour":    float64(0),
		"repeat_min":     float64(0),
		"last_work_hour": float64(4),
		"version":        float64(4),
	}
	if !reflect.DeepEqual(schedule, wantSchedule) {
		t.Errorf("schedule = %#v, want %#v", schedule, wantSchedule)
	}
}

func TestClient_CreateScheduledTask_RootUsesPrivilegedAPI(t *testing.T) {
	var requests []capturedRequest
	c, server := taskSchedulerServer(t, &requests)
	defer server.Close()

	_, err := c.CreateScheduledTask(context.Background(), CreateScheduledTaskRequest{
		Name:     "prune",
		Owner:    "root",
		Command:  "/bin/true",
		Enabled:  true,
		Schedule: TaskSchedule{Frequency: "daily", Hour: 4},
	})
	if err != nil {
		t.Fatalf("CreateScheduledTask failed: %v", err)
	}

	confirm := findRequest(t, requests, "auth")
	if got := confirm.taskParam("api"); got != "SYNO.Core.User.PasswordConfirm" {
		t.Errorf("password confirmation api = %q", got)
	}
	if got := confirm.taskParam("password"); got != "hunter2" {
		t.Errorf("password confirmation should send the provider password, got %q", got)
	}

	create := findRequest(t, requests, "create")
	if got := create.taskParam("api"); got != "SYNO.Core.TaskScheduler.Root" {
		t.Errorf("api = %q, want SYNO.Core.TaskScheduler.Root for a root-owned task", got)
	}
	if got := create.taskParam("SynoConfirmPWToken"); got != "confirm-token" {
		t.Errorf("SynoConfirmPWToken = %q, want the token returned by PasswordConfirm", got)
	}
}

func TestClient_CreateScheduledTask_DailyCoversEveryWeekday(t *testing.T) {
	var requests []capturedRequest
	c, server := taskSchedulerServer(t, &requests)
	defer server.Close()

	_, err := c.CreateScheduledTask(context.Background(), CreateScheduledTaskRequest{
		Name:     "healthcheck",
		Owner:    "operator",
		Command:  "/bin/true",
		Enabled:  true,
		Schedule: TaskSchedule{Frequency: "daily", Hour: 2, RepeatIntervalMinutes: 15, RepeatUntilHour: intPtr(20)},
	})
	if err != nil {
		t.Fatalf("CreateScheduledTask failed: %v", err)
	}

	var schedule map[string]interface{}
	if err := json.Unmarshal([]byte(findRequest(t, requests, "create").taskParam("schedule")), &schedule); err != nil {
		t.Fatalf("schedule is not JSON: %v", err)
	}
	if schedule["week_day"] != "0,1,2,3,4,5,6" {
		t.Errorf("daily week_day = %v, want every day", schedule["week_day"])
	}
	if schedule["repeat_date"] != float64(repeatDateDaily) {
		t.Errorf("daily repeat_date = %v, want %d", schedule["repeat_date"], repeatDateDaily)
	}
	if schedule["repeat_min"] != float64(15) {
		t.Errorf("repeat_min = %v, want 15", schedule["repeat_min"])
	}
	if schedule["last_work_hour"] != float64(20) {
		t.Errorf("last_work_hour = %v, want 20", schedule["last_work_hour"])
	}
}

func TestClient_GetScheduledTask(t *testing.T) {
	var requests []capturedRequest
	c, server := taskSchedulerServer(t, &requests)
	defer server.Close()

	task, err := c.GetScheduledTask(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetScheduledTask failed: %v", err)
	}

	// get needs real_owner, which only the list response supplies.
	get := findRequest(t, requests, "get")
	if got := get.taskParam("real_owner"); got != "root" {
		t.Errorf("get real_owner = %q, want the value taken from list", got)
	}
	if got := get.taskParam("version"); got != "4" {
		t.Errorf("get version = %q, want 4", got)
	}

	if task.Command != "/usr/local/bin/docker system prune -af" {
		t.Errorf("command = %q", task.Command)
	}
	if task.NotifyEmail != "ops@example.com" || !task.NotifyOnFailure {
		t.Errorf("notification = %q / %v", task.NotifyEmail, task.NotifyOnFailure)
	}
	if task.Schedule.Frequency != "weekly" {
		t.Errorf("frequency = %q, want weekly", task.Schedule.Frequency)
	}
	if !reflect.DeepEqual(task.Schedule.DaysOfWeek, []string{"sunday", "wednesday"}) {
		t.Errorf("days = %#v, want sunday and wednesday", task.Schedule.DaysOfWeek)
	}
	if task.Schedule.Hour != 4 || task.Schedule.Minute != 30 {
		t.Errorf("start time = %d:%d, want 4:30", task.Schedule.Hour, task.Schedule.Minute)
	}
}

func TestClient_ListScheduledTasksExcludesEventTasks(t *testing.T) {
	var requests []capturedRequest
	c, server := taskSchedulerServer(t, &requests)
	defer server.Close()

	tasks, err := c.ListScheduledTasks(context.Background())
	if err != nil {
		t.Fatalf("ListScheduledTasks failed: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want only the one carrying an id", len(tasks))
	}
	if tasks[0].Name != "prune" {
		t.Errorf("task = %q, want prune", tasks[0].Name)
	}

	list := findRequest(t, requests, "list")
	if got := list.taskParam("version"); got != "3" {
		t.Errorf("list version = %q, want 3 (version 4 does not work for list)", got)
	}
}

func TestClient_UpdateScheduledTaskSendsIDAndRealOwner(t *testing.T) {
	var requests []capturedRequest
	c, server := taskSchedulerServer(t, &requests)
	defer server.Close()

	_, err := c.UpdateScheduledTask(context.Background(), 42, UpdateScheduledTaskRequest{
		CreateScheduledTaskRequest: CreateScheduledTaskRequest{
			Name:     "prune",
			Owner:    "operator",
			Command:  "/bin/true",
			Enabled:  false,
			Schedule: TaskSchedule{Frequency: "monthly", DaysOfWeek: []string{"sunday"}, WeeksOfMonth: []string{"first"}},
		},
		RealOwner: "root",
	})
	if err != nil {
		t.Fatalf("UpdateScheduledTask failed: %v", err)
	}

	set := findRequest(t, requests, "set")
	if got := set.taskParam("id"); got != "42" {
		t.Errorf("set id = %q, want 42", got)
	}
	if got := set.taskParam("real_owner"); got != "root" {
		t.Errorf("set real_owner = %q, want the value carried in state, not the owner", got)
	}
	if got := set.taskParam("owner"); got != "operator" {
		t.Errorf("set owner = %q, want operator", got)
	}
	if got := set.taskParam("enable"); got != "false" {
		t.Errorf("set enable = %q, want false", got)
	}
	if got := set.taskParam("type"); got != "" {
		t.Errorf("set must not send type, got %q", got)
	}

	var schedule map[string]interface{}
	if err := json.Unmarshal([]byte(set.taskParam("schedule")), &schedule); err != nil {
		t.Fatalf("schedule is not JSON: %v", err)
	}
	if schedule["repeat_date"] != float64(repeatDateMonthly) {
		t.Errorf("repeat_date = %v, want %d", schedule["repeat_date"], repeatDateMonthly)
	}
	if !reflect.DeepEqual(schedule["monthly_week"], []interface{}{"first"}) {
		t.Errorf("monthly_week = %#v, want a JSON array holding first", schedule["monthly_week"])
	}
}

func TestClient_DeleteScheduledTask(t *testing.T) {
	var requests []capturedRequest
	c, server := taskSchedulerServer(t, &requests)
	defer server.Close()

	if err := c.DeleteScheduledTask(context.Background(), 42, "root"); err != nil {
		t.Fatalf("DeleteScheduledTask failed: %v", err)
	}

	del := findRequest(t, requests, "delete")
	if got := del.taskParam("version"); got != "2" {
		t.Errorf("delete version = %q, want 2", got)
	}

	var selection []map[string]interface{}
	if err := json.Unmarshal([]byte(del.taskParam("tasks")), &selection); err != nil {
		t.Fatalf("tasks is not a JSON array: %v", err)
	}
	want := []map[string]interface{}{{"id": float64(42), "real_owner": "root"}}
	if !reflect.DeepEqual(selection, want) {
		t.Errorf("tasks = %#v, want %#v", selection, want)
	}
}

func TestEncodeScheduleRejectsImpossibleSchedules(t *testing.T) {
	cases := map[string]struct {
		schedule TaskSchedule
		wantErr  string
	}{
		"unknown frequency": {
			schedule: TaskSchedule{Frequency: "hourly"},
			wantErr:  "unsupported schedule frequency",
		},
		"weekly without days": {
			schedule: TaskSchedule{Frequency: "weekly"},
			wantErr:  "requires at least one day_of_week",
		},
		"daily with days": {
			schedule: TaskSchedule{Frequency: "daily", DaysOfWeek: []string{"monday"}},
			wantErr:  "does not apply to a daily schedule",
		},
		"unknown weekday": {
			schedule: TaskSchedule{Frequency: "weekly", DaysOfWeek: []string{"caturday"}},
			wantErr:  "unknown day_of_week",
		},
		"monthly without weeks": {
			schedule: TaskSchedule{Frequency: "monthly", DaysOfWeek: []string{"monday"}},
			wantErr:  "requires at least one week_of_month",
		},
		"weeks on a weekly schedule": {
			schedule: TaskSchedule{Frequency: "weekly", DaysOfWeek: []string{"monday"}, WeeksOfMonth: []string{"first"}},
			wantErr:  "only applies to a monthly schedule",
		},
		"unsupported minute interval": {
			schedule: TaskSchedule{Frequency: "daily", RepeatIntervalMinutes: 7},
			wantErr:  "repeat_interval_minutes must be one of",
		},
		"both repeat intervals": {
			schedule: TaskSchedule{Frequency: "daily", RepeatIntervalHours: 2, RepeatIntervalMinutes: 30},
			wantErr:  "mutually exclusive",
		},
		"hour out of range": {
			schedule: TaskSchedule{Frequency: "daily", Hour: 24},
			wantErr:  "hour must be between 0 and 23",
		},
		"minute out of range": {
			schedule: TaskSchedule{Frequency: "daily", Minute: 60},
			wantErr:  "minute must be between 0 and 59",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ValidateTaskSchedule(tc.schedule)
			if err == nil {
				t.Fatalf("expected an error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseScheduleReadsMonthlyWeekInEitherEncoding(t *testing.T) {
	// DSM returns monthly_week as an array, but the reference python client
	// writes it as a JSON string. Both must read back the same.
	asArray := parseSchedule(map[string]interface{}{
		"repeat_date":  float64(1003),
		"week_day":     "1",
		"monthly_week": []interface{}{"first", "third"},
	})
	asString := parseSchedule(map[string]interface{}{
		"repeat_date":  float64(1003),
		"week_day":     "1",
		"monthly_week": `["first","third"]`,
	})

	want := []string{"first", "third"}
	if !reflect.DeepEqual(asArray.WeeksOfMonth, want) {
		t.Errorf("array form = %#v, want %#v", asArray.WeeksOfMonth, want)
	}
	if !reflect.DeepEqual(asString.WeeksOfMonth, want) {
		t.Errorf("string form = %#v, want %#v", asString.WeeksOfMonth, want)
	}
	if asArray.Frequency != "monthly" {
		t.Errorf("frequency = %q, want monthly", asArray.Frequency)
	}
}

func TestParseScheduleLeavesUnsupportedFrequencyEmpty(t *testing.T) {
	// A one-shot task DSM supports but this provider does not model.
	schedule := parseSchedule(map[string]interface{}{
		"date_type":   float64(1),
		"repeat_date": float64(0),
		"date":        "2026/9/11",
	})
	if schedule.Frequency != "" {
		t.Errorf("frequency = %q, want empty so callers can report it rather than guess", schedule.Frequency)
	}
}

func TestParseScheduledTaskConfigIgnoresDisabledNotification(t *testing.T) {
	raw, _ := json.Marshal(map[string]interface{}{
		"id":   1,
		"name": "example",
		"extra": map[string]interface{}{
			// DSM keeps the address after notification is switched off.
			"notify_enable": false,
			"notify_mail":   "stale@example.com",
			"script":        "/bin/true",
		},
	})

	task, err := parseScheduledTaskConfig(raw)
	if err != nil {
		t.Fatalf("parseScheduledTaskConfig failed: %v", err)
	}
	if task.NotifyEmail != "" {
		t.Errorf("notify_email = %q, want empty when notify_enable is false", task.NotifyEmail)
	}
}

// TestClient_PostRequestsKeepSessionInQueryString guards the provider-wide
// invariant that DSM only accepts _sid and SynoToken as URL parameters. A POST
// that carries them in the body instead is answered with error 119, which looks
// exactly like an expired session and sends debugging in the wrong direction.
func TestClient_PostRequestsKeepSessionInQueryString(t *testing.T) {
	var requests []capturedRequest
	c, server := taskSchedulerServer(t, &requests)
	defer server.Close()

	_, err := c.CreateScheduledTask(context.Background(), CreateScheduledTaskRequest{
		Name:     "prune",
		Owner:    "operator",
		Command:  "/bin/true",
		Enabled:  true,
		Schedule: TaskSchedule{Frequency: "daily"},
	})
	if err != nil {
		t.Fatalf("CreateScheduledTask failed: %v", err)
	}

	create := findRequest(t, requests, "create")
	if create.Method != http.MethodPost {
		t.Fatalf("expected a POST, got %s", create.Method)
	}

	if got := create.Query.Get("_sid"); got != "test-sid" {
		t.Errorf("_sid must travel in the query string, got %q there", got)
	}
	if got := create.Query.Get("SynoToken"); got != "test-token" {
		t.Errorf("SynoToken must travel in the query string, got %q there", got)
	}
	// And must not be duplicated into the body, where DSM ignores them.
	if _, ok := create.Body["_sid"]; ok {
		t.Error("_sid must not be sent in the POST body")
	}
	if _, ok := create.Body["SynoToken"]; ok {
		t.Error("SynoToken must not be sent in the POST body")
	}
	// The actual payload does belong in the body.
	if create.Body.Get("schedule") == "" {
		t.Error("schedule should be in the POST body")
	}
}

// realOwnerServer models the case this resource exists for: an admin session
// creating a root-owned task. DSM records real_owner as root, and answers a get
// carrying any other real_owner with an empty task rather than an error.
func realOwnerServer(t *testing.T, requests *[]capturedRequest, readBackFails bool) (*Client, *httptest.Server) {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/webapi/entry.cgi", func(w http.ResponseWriter, r *http.Request) {
		captured := recordRequest(r)
		*requests = append(*requests, captured)

		api := captured.taskParam("api")
		method := captured.taskParam("method")

		switch {
		case api == "SYNO.Core.User.PasswordConfirm":
			raw, _ := json.Marshal(map[string]interface{}{"SynoConfirmPWToken": "confirm-token"})
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		case strings.HasPrefix(api, "SYNO.Core.TaskScheduler") && method == "create":
			raw, _ := json.Marshal(map[string]interface{}{"id": 42})
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		case method == "list":
			if readBackFails {
				_ = json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 105}})
				return
			}
			raw, _ := json.Marshal(map[string]interface{}{
				"tasks": []map[string]interface{}{
					{"id": 42, "name": "prune", "owner": "root", "real_owner": "root", "enable": true, "type": "script"},
				},
			})
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		case method == "get":
			// The defining behaviour: a mismatched real_owner yields nothing.
			if captured.taskParam("real_owner") != "root" {
				_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: json.RawMessage(`{}`)})
				return
			}
			raw, _ := json.Marshal(map[string]interface{}{
				"id": 42, "name": "prune", "owner": "root", "real_owner": "root", "enable": true, "type": "script",
				"extra":    map[string]interface{}{"script": "/bin/true"},
				"schedule": map[string]interface{}{"repeat_date": 1001, "hour": 4, "last_work_hour": 4},
			})
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		default:
			_ = json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 101}})
		}
	})

	server := httptest.NewServer(mux)
	c := NewClient(server.URL, "admin", "hunter2", false)
	c.setSession("test-sid", "test-token")
	return c, server
}

// TestClient_CreateScheduledTaskResolvesRealOwner is the regression test for
// assuming real_owner equals the requested owner. Creating a root-owned task
// while authenticated as admin is precisely when the two differ, and guessing
// made DSM report the task it had just created as missing.
func TestClient_CreateScheduledTaskResolvesRealOwner(t *testing.T) {
	var requests []capturedRequest
	c, server := realOwnerServer(t, &requests, false)
	defer server.Close()

	task, err := c.CreateScheduledTask(context.Background(), CreateScheduledTaskRequest{
		Name:     "prune",
		Owner:    "root",
		Command:  "/bin/true",
		Enabled:  true,
		Schedule: TaskSchedule{Frequency: "daily", Hour: 4},
	})
	if err != nil {
		t.Fatalf("CreateScheduledTask failed: %v", err)
	}
	if task.RealOwner != "root" {
		t.Errorf("real_owner = %q, want the value DSM reports through list", task.RealOwner)
	}

	// The read-back must consult list rather than reuse the requested owner.
	if _, ok := findRequestIfPresent(requests, "list"); !ok {
		t.Error("create should look the real owner up through list before calling get")
	}
	get := findRequest(t, requests, "get")
	if got := get.taskParam("real_owner"); got != "root" {
		t.Errorf("get real_owner = %q, want root", got)
	}
}

// TestClient_CreateScheduledTaskReturnsTaskWhenReadBackFails covers the other
// half of the same hazard: DSM has already created the task, so a failed
// read-back must still hand the caller something to record. Returning only an
// error would leave a root-owned task on the NAS that no state tracks and that
// destroy cannot remove.
func TestClient_CreateScheduledTaskReturnsTaskWhenReadBackFails(t *testing.T) {
	var requests []capturedRequest
	c, server := realOwnerServer(t, &requests, true)
	defer server.Close()

	task, err := c.CreateScheduledTask(context.Background(), CreateScheduledTaskRequest{
		Name:     "prune",
		Owner:    "root",
		Command:  "/bin/true",
		Enabled:  true,
		Schedule: TaskSchedule{Frequency: "daily", Hour: 4},
	})

	if err == nil {
		t.Fatal("expected the read-back failure to be reported")
	}
	if task == nil {
		t.Fatal("a task created on the NAS must be returned even when it cannot be read back")
	}
	if task.ID != 42 {
		t.Errorf("id = %d, want the id DSM assigned so the task stays addressable", task.ID)
	}
	if task.Name != "prune" || task.Command != "/bin/true" {
		t.Errorf("fallback task should describe what was requested, got %+v", task)
	}
	if !strings.Contains(err.Error(), "created") {
		t.Errorf("error should make clear the task exists: %v", err)
	}
}

func findRequestIfPresent(requests []capturedRequest, method string) (capturedRequest, bool) {
	for _, req := range requests {
		if req.taskParam("method") == method {
			return req, true
		}
	}
	return capturedRequest{}, false
}

func TestEncodeScheduleRepeatWindow(t *testing.T) {
	t.Run("unset ends the window at the start hour", func(t *testing.T) {
		encoded, err := ValidateTaskSchedule(TaskSchedule{Frequency: "daily", Hour: 9, RepeatIntervalHours: 2})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if encoded["last_work_hour"] != 9 {
			t.Errorf("last_work_hour = %v, want the start hour", encoded["last_work_hour"])
		}
	})

	t.Run("explicit value is preserved", func(t *testing.T) {
		encoded, err := ValidateTaskSchedule(TaskSchedule{Frequency: "daily", Hour: 9, RepeatIntervalHours: 2, RepeatUntilHour: intPtr(18)})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if encoded["last_work_hour"] != 18 {
			t.Errorf("last_work_hour = %v, want 18", encoded["last_work_hour"])
		}
	})

	t.Run("a window closing before the start is rejected", func(t *testing.T) {
		// Silently rewriting this to the start hour is what produced "provider
		// produced inconsistent result after apply".
		_, err := ValidateTaskSchedule(TaskSchedule{Frequency: "daily", Hour: 9, RepeatUntilHour: intPtr(0)})
		if err == nil {
			t.Fatal("expected repeat_until_hour earlier than hour to be rejected")
		}
		if !strings.Contains(err.Error(), "must not be earlier than hour") {
			t.Errorf("error = %q", err)
		}
	})
}
