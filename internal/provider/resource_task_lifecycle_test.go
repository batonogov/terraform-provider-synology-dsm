package provider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// These tests drive the resources' Create, Update, Delete, and ModifyPlan
// against a recording DSM stub. The state-changing paths are where a mistake
// costs a stray root-capable task on the NAS, so they are exercised directly
// rather than only through their helpers.

// dsmStub records every request the provider makes and answers with whatever
// the test needs.
type dsmStub struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []map[string]string
}

func (s *dsmStub) record(r *http.Request) map[string]string {
	_ = r.ParseForm()
	params := map[string]string{}
	for key := range r.Form {
		params[key] = r.Form.Get(key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, params)
	return params
}

// calls returns the DSM methods invoked, in order.
func (s *dsmStub) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	methods := make([]string, 0, len(s.requests))
	for _, req := range s.requests {
		methods = append(methods, req["method"])
	}
	return methods
}

func (s *dsmStub) request(method string) (map[string]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, req := range s.requests {
		if req["method"] == method {
			return req, true
		}
	}
	return nil, false
}

// newDSMStub answers the full happy path for a scheduled task owned by root and
// created from an admin session, plus the event task calls.
func newDSMStub(t *testing.T, readBackFails bool) *dsmStub {
	t.Helper()
	stub := &dsmStub{}

	mux := http.NewServeMux()
	mux.HandleFunc("/webapi/entry.cgi", func(w http.ResponseWriter, r *http.Request) {
		params := stub.record(r)

		switch params["method"] {
		case "auth":
			raw, _ := json.Marshal(map[string]interface{}{"SynoConfirmPWToken": "confirm-token"})
			_ = json.NewEncoder(w).Encode(client.APIResponse{Success: true, Data: raw})

		case "create":
			if strings.Contains(params["api"], "EventScheduler") {
				_ = json.NewEncoder(w).Encode(client.APIResponse{Success: true})
				return
			}
			raw, _ := json.Marshal(map[string]interface{}{"id": 42})
			_ = json.NewEncoder(w).Encode(client.APIResponse{Success: true, Data: raw})

		case "set":
			_ = json.NewEncoder(w).Encode(client.APIResponse{Success: true})

		case "list":
			if readBackFails {
				_ = json.NewEncoder(w).Encode(client.APIResponse{Success: false, Error: &client.APIError{Code: 105}})
				return
			}
			raw, _ := json.Marshal(map[string]interface{}{
				"tasks": []map[string]interface{}{
					// real_owner deliberately differs from the requesting account.
					{"id": 42, "name": "prune", "owner": "root", "real_owner": "root", "enable": true, "type": "script"},
				},
			})
			_ = json.NewEncoder(w).Encode(client.APIResponse{Success: true, Data: raw})

		case "get":
			if !strings.Contains(params["api"], "EventScheduler") && params["real_owner"] != "root" {
				// DSM answers a get carrying the wrong real_owner with an empty
				// task rather than an error.
				_ = json.NewEncoder(w).Encode(client.APIResponse{Success: true, Data: json.RawMessage(`{}`)})
				return
			}
			if strings.Contains(params["api"], "EventScheduler") {
				raw, _ := json.Marshal(map[string]interface{}{
					"task_name": "restore-mounts",
					"event":     "bootup",
					"enable":    true,
					"operation": "/volume1/scripts/restore-mounts.sh",
					"owner":     map[string]interface{}{"0": "root"},
				})
				_ = json.NewEncoder(w).Encode(client.APIResponse{Success: true, Data: raw})
				return
			}
			raw, _ := json.Marshal(map[string]interface{}{
				"id": 42, "name": "prune", "owner": "root", "real_owner": "root", "enable": true, "type": "script",
				"extra": map[string]interface{}{"script": "/bin/true"},
				"schedule": map[string]interface{}{
					"repeat_date": 1001, "week_day": "0,1,2,3,4,5,6", "hour": 4, "minute": 30, "last_work_hour": 4,
				},
			})
			_ = json.NewEncoder(w).Encode(client.APIResponse{Success: true, Data: raw})

		case "delete":
			_ = json.NewEncoder(w).Encode(client.APIResponse{Success: true})

		default:
			_ = json.NewEncoder(w).Encode(client.APIResponse{Success: false, Error: &client.APIError{Code: 101}})
		}
	})

	stub.server = httptest.NewServer(mux)
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *dsmStub) providerData(allowTaskExecution bool) *dsmProviderData {
	c := client.NewClient(s.server.URL, "admin", "hunter2", false)
	return &dsmProviderData{client: c, allowTaskExecution: allowTaskExecution}
}

func scheduledTaskResourceFor(t *testing.T, stub *dsmStub, allow bool) (*scheduledTaskResource, rschema.Schema) {
	t.Helper()
	r := NewScheduledTaskResource().(*scheduledTaskResource)
	configure := &resource.ConfigureResponse{}
	r.Configure(t.Context(), resource.ConfigureRequest{ProviderData: stub.providerData(allow)}, configure)
	if configure.Diagnostics.HasError() {
		t.Fatalf("configure failed: %v", configure.Diagnostics)
	}

	schemaResp := &resource.SchemaResponse{}
	r.Schema(t.Context(), resource.SchemaRequest{}, schemaResp)
	return r, schemaResp.Schema
}

func eventTaskResourceFor(t *testing.T, stub *dsmStub, allow bool) (*eventTaskResource, rschema.Schema) {
	t.Helper()
	r := NewEventTaskResource().(*eventTaskResource)
	configure := &resource.ConfigureResponse{}
	r.Configure(t.Context(), resource.ConfigureRequest{ProviderData: stub.providerData(allow)}, configure)
	if configure.Diagnostics.HasError() {
		t.Fatalf("configure failed: %v", configure.Diagnostics)
	}

	schemaResp := &resource.SchemaResponse{}
	r.Schema(t.Context(), resource.SchemaRequest{}, schemaResp)
	return r, schemaResp.Schema
}

func planFor(t *testing.T, s rschema.Schema, model any) tfsdk.Plan {
	t.Helper()
	plan := tfsdk.Plan{Schema: s}
	if diags := plan.Set(t.Context(), model); diags.HasError() {
		t.Fatalf("building plan: %v", diags)
	}
	return plan
}

func stateFor(t *testing.T, s rschema.Schema, model any) tfsdk.State {
	t.Helper()
	state := tfsdk.State{Schema: s}
	if diags := state.Set(t.Context(), model); diags.HasError() {
		t.Fatalf("building state: %v", diags)
	}
	return state
}

func nullState(t *testing.T, s rschema.Schema) tfsdk.State {
	t.Helper()
	return tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(t.Context()), nil)}
}

func validScheduledTaskModel() *scheduledTaskResourceModel {
	return &scheduledTaskResourceModel{
		ID:              types.StringValue("42"),
		Name:            types.StringValue("prune"),
		User:            types.StringValue("root"),
		Command:         types.StringValue("/bin/true"),
		Enabled:         types.BoolValue(true),
		NotifyEmail:     types.StringNull(),
		NotifyOnFailure: types.BoolValue(false),
		RealOwner:       types.StringValue("root"),
		Schedule: &taskScheduleModel{
			Frequency:             types.StringValue("daily"),
			DayOfWeek:             types.SetNull(types.StringType),
			WeekOfMonth:           types.SetNull(types.StringType),
			Hour:                  types.Int64Value(4),
			Minute:                types.Int64Value(30),
			RepeatIntervalHours:   types.Int64Value(0),
			RepeatIntervalMinutes: types.Int64Value(0),
			RepeatUntilHour:       types.Int64Null(),
		},
	}
}

func validEventTaskModel() *eventTaskResourceModel {
	return &eventTaskResourceModel{
		ID:              types.StringValue("restore-mounts"),
		Name:            types.StringValue("restore-mounts"),
		User:            types.StringValue("root"),
		Event:           types.StringValue("bootup"),
		Command:         types.StringValue("/volume1/scripts/restore-mounts.sh"),
		Enabled:         types.BoolValue(true),
		NotifyEmail:     types.StringNull(),
		NotifyOnFailure: types.BoolValue(false),
		DependsOnTasks:  types.ListNull(types.StringType),
		OwnerUID:        types.Int64Value(0),
	}
}

// --- the opt-in gate on the apply path ----------------------------------

// TestScheduledTaskCreateRefusedWithoutOptIn pins the defence-in-depth check in
// Create. Deleting it must fail a test: the plan-time gate is one call site,
// and a privilege boundary should not rest on a single one.
func TestScheduledTaskCreateRefusedWithoutOptIn(t *testing.T) {
	stub := newDSMStub(t, false)
	r, s := scheduledTaskResourceFor(t, stub, false)

	resp := &resource.CreateResponse{State: nullState(t, s)}
	r.Create(t.Context(), resource.CreateRequest{Plan: planFor(t, s, validScheduledTaskModel())}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("create must be refused when allow_task_execution is false")
	}
	if calls := stub.calls(); len(calls) != 0 {
		t.Errorf("a refused create must not reach DSM, but it called %v", calls)
	}
}

func TestScheduledTaskUpdateRefusedWithoutOptIn(t *testing.T) {
	stub := newDSMStub(t, false)
	r, s := scheduledTaskResourceFor(t, stub, false)

	model := validScheduledTaskModel()
	resp := &resource.UpdateResponse{State: stateFor(t, s, model)}
	r.Update(t.Context(), resource.UpdateRequest{
		Plan:  planFor(t, s, model),
		State: stateFor(t, s, model),
	}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("update must be refused when allow_task_execution is false")
	}
	if calls := stub.calls(); len(calls) != 0 {
		t.Errorf("a refused update must not reach DSM, but it called %v", calls)
	}
}

func TestEventTaskCreateRefusedWithoutOptIn(t *testing.T) {
	stub := newDSMStub(t, false)
	r, s := eventTaskResourceFor(t, stub, false)

	resp := &resource.CreateResponse{State: nullState(t, s)}
	r.Create(t.Context(), resource.CreateRequest{Plan: planFor(t, s, validEventTaskModel())}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("create must be refused when allow_task_execution is false")
	}
	if calls := stub.calls(); len(calls) != 0 {
		t.Errorf("a refused create must not reach DSM, but it called %v", calls)
	}
}

func TestEventTaskUpdateRefusedWithoutOptIn(t *testing.T) {
	stub := newDSMStub(t, false)
	r, s := eventTaskResourceFor(t, stub, false)

	model := validEventTaskModel()
	resp := &resource.UpdateResponse{State: stateFor(t, s, model)}
	r.Update(t.Context(), resource.UpdateRequest{Plan: planFor(t, s, model), State: stateFor(t, s, model)}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("update must be refused when allow_task_execution is false")
	}
	if calls := stub.calls(); len(calls) != 0 {
		t.Errorf("a refused update must not reach DSM, but it called %v", calls)
	}
}

// --- blocker: an invalid request must not reach DSM ----------------------

// TestEventTaskCreateRejectsBadRequestBeforeCallingDSM is the regression test
// for building the request in argument position. Go evaluates arguments before
// the call, so a builder that only reported its problem through diagnostics had
// already created the task on the NAS by the time anyone looked — leaving a
// root task that no state entry tracks.
//
// The invalid input here is notify_on_failure without an address, deliberately:
// it is caught only by the resource-level builder. An unsupported event would
// not prove anything, because the client rejects that one before it opens a
// connection, so the bug would stay invisible.
func TestEventTaskCreateRejectsBadRequestBeforeCallingDSM(t *testing.T) {
	stub := newDSMStub(t, false)
	r, s := eventTaskResourceFor(t, stub, true)

	model := validEventTaskModel()
	model.NotifyOnFailure = types.BoolValue(true)
	model.NotifyEmail = types.StringNull()

	resp := &resource.CreateResponse{State: nullState(t, s)}
	r.Create(t.Context(), resource.CreateRequest{Plan: planFor(t, s, model)}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("notify_on_failure without an address must be rejected")
	}
	if calls := stub.calls(); len(calls) != 0 {
		t.Fatalf("nothing may reach DSM once the request is known to be invalid, but it called %v", calls)
	}
}

// TestEventTaskCreateRejectsUnsupportedEvent covers the validation itself.
func TestEventTaskCreateRejectsUnsupportedEvent(t *testing.T) {
	stub := newDSMStub(t, false)
	r, s := eventTaskResourceFor(t, stub, true)

	model := validEventTaskModel()
	model.Event = types.StringValue("midnight")

	resp := &resource.CreateResponse{State: nullState(t, s)}
	r.Create(t.Context(), resource.CreateRequest{Plan: planFor(t, s, model)}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("an unsupported event must be rejected")
	}
	if calls := stub.calls(); len(calls) != 0 {
		t.Errorf("a rejected event must not reach DSM, but it called %v", calls)
	}
}

func TestEventTaskUpdateRejectsBadRequestBeforeCallingDSM(t *testing.T) {
	stub := newDSMStub(t, false)
	r, s := eventTaskResourceFor(t, stub, true)

	model := validEventTaskModel()
	model.NotifyOnFailure = types.BoolValue(true)
	model.NotifyEmail = types.StringNull()

	resp := &resource.UpdateResponse{State: stateFor(t, s, validEventTaskModel())}
	r.Update(t.Context(), resource.UpdateRequest{
		Plan:  planFor(t, s, model),
		State: stateFor(t, s, validEventTaskModel()),
	}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("notify_on_failure without an address must be rejected")
	}
	if calls := stub.calls(); len(calls) != 0 {
		t.Fatalf("nothing may reach DSM once the request is known to be invalid, but it called %v", calls)
	}
}

// --- blocker: a created task must never be orphaned ---------------------

// TestScheduledTaskCreateRecordsStateWhenReadBackFails is the regression test
// for losing a task DSM has already created. Without a state entry the task
// keeps running as root and destroy cannot remove it.
func TestScheduledTaskCreateRecordsStateWhenReadBackFails(t *testing.T) {
	stub := newDSMStub(t, true) // list fails, so the read-back cannot succeed
	r, s := scheduledTaskResourceFor(t, stub, true)

	model := validScheduledTaskModel()
	model.ID = types.StringUnknown()
	model.RealOwner = types.StringUnknown()

	resp := &resource.CreateResponse{State: nullState(t, s)}
	r.Create(t.Context(), resource.CreateRequest{Plan: planFor(t, s, model)}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("the read-back failure should be reported")
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("state must record the task DSM already created, or it is orphaned on the NAS")
	}

	var saved scheduledTaskResourceModel
	if diags := resp.State.Get(t.Context(), &saved); diags.HasError() {
		t.Fatalf("reading back saved state: %v", diags)
	}
	if saved.ID.ValueString() != "42" {
		t.Errorf("state id = %q, want the id DSM assigned so destroy can address it", saved.ID.ValueString())
	}
	if saved.Command.ValueString() != "/bin/true" {
		t.Errorf("state command = %q", saved.Command.ValueString())
	}
}

// TestScheduledTaskCreateUsesRealOwnerFromDSM is the regression test for
// guessing that real_owner equals the requested owner. The stub answers a get
// carrying the wrong real_owner with an empty task, exactly as DSM does.
func TestScheduledTaskCreateUsesRealOwnerFromDSM(t *testing.T) {
	stub := newDSMStub(t, false)
	r, s := scheduledTaskResourceFor(t, stub, true)

	// The owner is an ordinary account while DSM records root as the real owner,
	// which is the arrangement that made guessing wrong. With user = "root" the
	// guess would happen to be correct and prove nothing.
	model := validScheduledTaskModel()
	model.User = types.StringValue("operator")
	model.ID = types.StringUnknown()
	model.RealOwner = types.StringUnknown()

	resp := &resource.CreateResponse{State: nullState(t, s)}
	r.Create(t.Context(), resource.CreateRequest{Plan: planFor(t, s, model)}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("create should succeed: %v", resp.Diagnostics)
	}

	var saved scheduledTaskResourceModel
	if diags := resp.State.Get(t.Context(), &saved); diags.HasError() {
		t.Fatalf("reading back saved state: %v", diags)
	}
	if saved.RealOwner.ValueString() != "root" {
		t.Errorf("real_owner = %q, want the value DSM reported", saved.RealOwner.ValueString())
	}

	get, ok := stub.request("get")
	if !ok {
		t.Fatal("create should read the task back")
	}
	if get["real_owner"] != "root" {
		t.Errorf("get real_owner = %q, want the value resolved from list rather than the requested owner", get["real_owner"])
	}
}

// --- blocker: schedule validation during plan ---------------------------

func TestScheduledTaskModifyPlanValidatesSchedule(t *testing.T) {
	cases := map[string]func(*taskScheduleModel){
		"unknown frequency": func(s *taskScheduleModel) { s.Frequency = types.StringValue("hourly") },
		"hour out of range": func(s *taskScheduleModel) { s.Hour = types.Int64Value(25) },
		"minute out of range": func(s *taskScheduleModel) {
			s.Minute = types.Int64Value(60)
		},
		"bad minute interval": func(s *taskScheduleModel) {
			s.RepeatIntervalMinutes = types.Int64Value(7)
		},
		"conflicting repeat intervals": func(s *taskScheduleModel) {
			s.RepeatIntervalHours = types.Int64Value(2)
			s.RepeatIntervalMinutes = types.Int64Value(30)
		},
		"repeat window closes before the start": func(s *taskScheduleModel) {
			s.Hour = types.Int64Value(9)
			s.RepeatUntilHour = types.Int64Value(3)
		},
		"weekly without days": func(s *taskScheduleModel) {
			s.Frequency = types.StringValue("weekly")
		},
		"daily with days": func(s *taskScheduleModel) {
			set, _ := types.SetValueFrom(t.Context(), types.StringType, []string{"monday"})
			s.DayOfWeek = set
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			stub := newDSMStub(t, false)
			r, s := scheduledTaskResourceFor(t, stub, true)

			model := validScheduledTaskModel()
			mutate(model.Schedule)

			plan := planFor(t, s, model)
			resp := &resource.ModifyPlanResponse{Plan: plan}
			r.ModifyPlan(t.Context(), resource.ModifyPlanRequest{Plan: plan}, resp)

			if !resp.Diagnostics.HasError() {
				t.Fatal("an impossible schedule must be rejected during plan, not halfway through an apply")
			}
		})
	}
}

func TestScheduledTaskModifyPlanAcceptsValidSchedule(t *testing.T) {
	stub := newDSMStub(t, false)
	r, s := scheduledTaskResourceFor(t, stub, true)

	plan := planFor(t, s, validScheduledTaskModel())
	resp := &resource.ModifyPlanResponse{Plan: plan}
	r.ModifyPlan(t.Context(), resource.ModifyPlanRequest{Plan: plan}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("a valid schedule should plan cleanly: %v", resp.Diagnostics)
	}
	// A root task still warns, which is the point of the warning.
	if len(resp.Diagnostics.Warnings()) == 0 {
		t.Error("a root-owned task should warn during plan")
	}
}

func TestScheduledTaskModifyPlanRefusedWithoutOptIn(t *testing.T) {
	stub := newDSMStub(t, false)
	r, s := scheduledTaskResourceFor(t, stub, false)

	plan := planFor(t, s, validScheduledTaskModel())
	resp := &resource.ModifyPlanResponse{Plan: plan}
	r.ModifyPlan(t.Context(), resource.ModifyPlanRequest{Plan: plan}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("plan must fail when allow_task_execution is false")
	}
	if !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), "allow_task_execution") {
		t.Errorf("the refusal should name the flag: %s", resp.Diagnostics.Errors()[0].Detail())
	}
}

func TestEventTaskModifyPlanRefusedWithoutOptIn(t *testing.T) {
	stub := newDSMStub(t, false)
	r, s := eventTaskResourceFor(t, stub, false)

	plan := planFor(t, s, validEventTaskModel())
	resp := &resource.ModifyPlanResponse{Plan: plan}
	r.ModifyPlan(t.Context(), resource.ModifyPlanRequest{Plan: plan}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("plan must fail when allow_task_execution is false")
	}
	if !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), "allow_task_execution") {
		t.Errorf("the refusal should name the flag: %s", resp.Diagnostics.Errors()[0].Detail())
	}
}

// TestEventTaskModifyPlanValidatesRequest pins that the event resource performs
// the same plan-time validation as its scheduled counterpart, through the same
// builder the apply path uses.
func TestEventTaskModifyPlanValidatesRequest(t *testing.T) {
	cases := map[string]func(*eventTaskResourceModel){
		"unsupported event": func(m *eventTaskResourceModel) { m.Event = types.StringValue("midnight") },
		"notify without an address": func(m *eventTaskResourceModel) {
			m.NotifyOnFailure = types.BoolValue(true)
			m.NotifyEmail = types.StringNull()
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			stub := newDSMStub(t, false)
			r, s := eventTaskResourceFor(t, stub, true)

			model := validEventTaskModel()
			mutate(model)

			plan := planFor(t, s, model)
			resp := &resource.ModifyPlanResponse{Plan: plan}
			r.ModifyPlan(t.Context(), resource.ModifyPlanRequest{Plan: plan}, resp)

			if !resp.Diagnostics.HasError() {
				t.Fatal("the problem must be reported during plan, not halfway through an apply")
			}
		})
	}
}

// --- happy paths --------------------------------------------------------

func TestEventTaskCreateSucceeds(t *testing.T) {
	stub := newDSMStub(t, false)
	r, s := eventTaskResourceFor(t, stub, true)

	model := validEventTaskModel()
	model.ID = types.StringUnknown()
	model.OwnerUID = types.Int64Unknown()

	resp := &resource.CreateResponse{State: nullState(t, s)}
	r.Create(t.Context(), resource.CreateRequest{Plan: planFor(t, s, model)}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("create failed: %v", resp.Diagnostics)
	}

	var saved eventTaskResourceModel
	if diags := resp.State.Get(t.Context(), &saved); diags.HasError() {
		t.Fatalf("reading back saved state: %v", diags)
	}
	if saved.ID.ValueString() != "restore-mounts" {
		t.Errorf("id = %q, want the task name", saved.ID.ValueString())
	}
	if saved.User.ValueString() != "root" || saved.OwnerUID.ValueInt64() != 0 {
		t.Errorf("owner = %q uid %d, want root uid 0", saved.User.ValueString(), saved.OwnerUID.ValueInt64())
	}

	create, ok := stub.request("create")
	if !ok {
		t.Fatal("create should reach DSM")
	}
	if !strings.Contains(create["api"], "EventScheduler.Root") {
		t.Errorf("api = %q, want the privileged namespace for a root-owned task", create["api"])
	}
}

func TestScheduledTaskUpdateSucceeds(t *testing.T) {
	stub := newDSMStub(t, false)
	r, s := scheduledTaskResourceFor(t, stub, true)

	plan := validScheduledTaskModel()
	plan.Command = types.StringValue("/bin/false")

	resp := &resource.UpdateResponse{State: stateFor(t, s, validScheduledTaskModel())}
	r.Update(t.Context(), resource.UpdateRequest{
		Plan:  planFor(t, s, plan),
		State: stateFor(t, s, validScheduledTaskModel()),
	}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("update failed: %v", resp.Diagnostics)
	}

	set, ok := stub.request("set")
	if !ok {
		t.Fatal("update should reach DSM")
	}
	if set["id"] != "42" {
		t.Errorf("set id = %q, want the id held in state", set["id"])
	}
	// real_owner must come from state, not be re-derived from the plan.
	if set["real_owner"] != "root" {
		t.Errorf("set real_owner = %q, want the value recorded in state", set["real_owner"])
	}
}

// --- delete -------------------------------------------------------------

func TestScheduledTaskDeleteSendsIDAndRealOwner(t *testing.T) {
	stub := newDSMStub(t, false)
	r, s := scheduledTaskResourceFor(t, stub, true)

	resp := &resource.DeleteResponse{State: stateFor(t, s, validScheduledTaskModel())}
	r.Delete(t.Context(), resource.DeleteRequest{State: stateFor(t, s, validScheduledTaskModel())}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("delete failed: %v", resp.Diagnostics)
	}

	del, ok := stub.request("delete")
	if !ok {
		t.Fatal("delete should reach DSM")
	}
	var selection []map[string]interface{}
	if err := json.Unmarshal([]byte(del["tasks"]), &selection); err != nil {
		t.Fatalf("tasks is not JSON: %v", err)
	}
	if selection[0]["id"] != float64(42) || selection[0]["real_owner"] != "root" {
		t.Errorf("delete selection = %#v, want id 42 owned by root", selection[0])
	}
}

// TestScheduledTaskDeleteWorksWithoutOptIn pins the deliberate asymmetry:
// removing a task executes nothing, so a configuration that has since turned
// the flag off must still be able to clean up what it created.
func TestScheduledTaskDeleteWorksWithoutOptIn(t *testing.T) {
	stub := newDSMStub(t, false)
	r, s := scheduledTaskResourceFor(t, stub, false)

	resp := &resource.DeleteResponse{State: stateFor(t, s, validScheduledTaskModel())}
	r.Delete(t.Context(), resource.DeleteRequest{State: stateFor(t, s, validScheduledTaskModel())}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("destroy must not be gated: %v", resp.Diagnostics)
	}
	if _, ok := stub.request("delete"); !ok {
		t.Error("delete should still reach DSM")
	}
}

func TestEventTaskDeleteWorksWithoutOptIn(t *testing.T) {
	stub := newDSMStub(t, false)
	r, s := eventTaskResourceFor(t, stub, false)

	resp := &resource.DeleteResponse{State: stateFor(t, s, validEventTaskModel())}
	r.Delete(t.Context(), resource.DeleteRequest{State: stateFor(t, s, validEventTaskModel())}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("destroy must not be gated: %v", resp.Diagnostics)
	}
}

// --- refresh must survive a schedule changed in DSM's UI -----------------

// TestScheduledTaskReadSurvivesUnsupportedSchedule pins that an administrator
// switching the task to a fixed date in DSM does not brick the workspace.
// Failing during refresh would break every later plan, including the destroy
// that would clean it up.
func TestScheduledTaskReadSurvivesUnsupportedSchedule(t *testing.T) {
	stub := &dsmStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/webapi/entry.cgi", func(w http.ResponseWriter, r *http.Request) {
		params := stub.record(r)
		switch params["method"] {
		case "list":
			raw, _ := json.Marshal(map[string]interface{}{
				"tasks": []map[string]interface{}{
					{"id": 42, "name": "prune", "owner": "root", "real_owner": "root", "enable": true, "type": "script"},
				},
			})
			_ = json.NewEncoder(w).Encode(client.APIResponse{Success: true, Data: raw})
		default:
			// date_type 1 with repeat_date 0: a one-shot task on a fixed date.
			raw, _ := json.Marshal(map[string]interface{}{
				"id": 42, "name": "prune", "owner": "root", "real_owner": "root", "enable": true, "type": "script",
				"extra":    map[string]interface{}{"script": "/bin/true"},
				"schedule": map[string]interface{}{"date_type": 1, "repeat_date": 0, "date": "2026/9/11"},
			})
			_ = json.NewEncoder(w).Encode(client.APIResponse{Success: true, Data: raw})
		}
	})
	stub.server = httptest.NewServer(mux)
	t.Cleanup(stub.server.Close)

	r, s := scheduledTaskResourceFor(t, stub, true)

	resp := &resource.ReadResponse{State: stateFor(t, s, validScheduledTaskModel())}
	r.Read(t.Context(), resource.ReadRequest{State: stateFor(t, s, validScheduledTaskModel())}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("refresh must not fail on a schedule changed outside Terraform: %v", resp.Diagnostics)
	}
	if len(resp.Diagnostics.Warnings()) == 0 {
		t.Error("the drift should still be reported as a warning")
	}

	var saved scheduledTaskResourceModel
	if diags := resp.State.Get(t.Context(), &saved); diags.HasError() {
		t.Fatalf("reading back saved state: %v", diags)
	}
	if saved.ID.ValueString() != "42" {
		t.Error("the resource must stay in state so that destroy can remove it")
	}
	if !saved.Schedule.Frequency.IsNull() {
		t.Errorf("frequency = %q, want null so the next plan restores the configured schedule", saved.Schedule.Frequency.ValueString())
	}
}
