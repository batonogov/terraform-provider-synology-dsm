package provider

import (
	"strings"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestScheduledTaskResource_Metadata(t *testing.T) {
	r := NewScheduledTaskResource()
	resp := &resource.MetadataResponse{}
	r.Metadata(t.Context(), resource.MetadataRequest{ProviderTypeName: "dsm"}, resp)
	if resp.TypeName != "dsm_scheduled_task" {
		t.Errorf("type name = %q, want dsm_scheduled_task", resp.TypeName)
	}
}

func TestScheduledTaskResource_Schema(t *testing.T) {
	r := NewScheduledTaskResource()
	resp := &resource.SchemaResponse{}
	r.Schema(t.Context(), resource.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned errors: %v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(t.Context()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %v", diags)
	}

	attrs := resp.Schema.GetAttributes()
	for _, name := range []string{"name", "user", "command"} {
		if attr := attrs[name]; attr == nil || !attr.IsRequired() {
			t.Errorf("%s must be required", name)
		}
	}

	// user has no default on purpose: which account a root-capable command runs
	// as must be stated in the configuration, not inherited silently.
	if attrs["user"].IsComputed() {
		t.Error("user must not be computed; a default would hide the privilege choice")
	}

	// command must stay visible in plan output so a reviewer can read what the
	// NAS is about to be told to run.
	if attrs["command"].IsSensitive() {
		t.Error("command must not be sensitive; hiding it removes the only review surface for a root command")
	}

	if _, ok := resp.Schema.GetBlocks()["schedule"]; !ok {
		t.Error("schedule block is missing")
	}
}

func TestScheduledTaskResource_SchemaWarnsAboutRootExecution(t *testing.T) {
	r := NewScheduledTaskResource()
	resp := &resource.SchemaResponse{}
	r.Schema(t.Context(), resource.SchemaRequest{}, resp)

	description := resp.Schema.GetDescription()
	for _, phrase := range []string{"runs a shell command on the NAS", "allow_task_execution", "root"} {
		if !strings.Contains(description, phrase) {
			t.Errorf("resource description must mention %q so the risk reaches the generated documentation", phrase)
		}
	}
}

func TestEventTaskResource_Schema(t *testing.T) {
	r := NewEventTaskResource()
	metadata := &resource.MetadataResponse{}
	r.Metadata(t.Context(), resource.MetadataRequest{ProviderTypeName: "dsm"}, metadata)
	if metadata.TypeName != "dsm_event_task" {
		t.Errorf("type name = %q, want dsm_event_task", metadata.TypeName)
	}

	resp := &resource.SchemaResponse{}
	r.Schema(t.Context(), resource.SchemaRequest{}, resp)
	if diags := resp.Schema.ValidateImplementation(t.Context()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %v", diags)
	}

	attrs := resp.Schema.GetAttributes()
	for _, name := range []string{"name", "user", "event", "command"} {
		if attr := attrs[name]; attr == nil || !attr.IsRequired() {
			t.Errorf("%s must be required", name)
		}
	}
	if attrs["command"].IsSensitive() {
		t.Error("command must not be sensitive")
	}
	if !strings.Contains(resp.Schema.GetDescription(), "allow_task_execution") {
		t.Error("resource description must mention the provider opt-in")
	}
}

func TestTaskDataSourceSchemas(t *testing.T) {
	for name, ds := range map[string]datasource.DataSource{
		"dsm_scheduled_task": NewScheduledTaskDataSource(),
		"dsm_event_task":     NewEventTaskDataSource(),
	} {
		metadata := &datasource.MetadataResponse{}
		ds.Metadata(t.Context(), datasource.MetadataRequest{ProviderTypeName: "dsm"}, metadata)
		if metadata.TypeName != name {
			t.Errorf("type name = %q, want %q", metadata.TypeName, name)
		}

		resp := &datasource.SchemaResponse{}
		ds.Schema(t.Context(), datasource.SchemaRequest{}, resp)
		if diags := resp.Schema.ValidateImplementation(t.Context()); diags.HasError() {
			t.Fatalf("%s schema failed framework validation: %v", name, diags)
		}
		if attr := resp.Schema.GetAttributes()["name"]; attr == nil || !attr.IsRequired() {
			t.Errorf("%s data source must take a required name", name)
		}
	}
}

func TestTaskResourcesConfigureRejectWrongProviderData(t *testing.T) {
	scheduled := NewScheduledTaskResource().(*scheduledTaskResource)
	wrong := &resource.ConfigureResponse{}
	scheduled.Configure(t.Context(), resource.ConfigureRequest{ProviderData: "wrong"}, wrong)
	if !wrong.Diagnostics.HasError() {
		t.Error("expected a diagnostic for wrong provider data")
	}

	empty := &resource.ConfigureResponse{}
	scheduled = NewScheduledTaskResource().(*scheduledTaskResource)
	scheduled.Configure(t.Context(), resource.ConfigureRequest{}, empty)
	if empty.Diagnostics.HasError() || scheduled.client != nil {
		t.Errorf("nil provider data should be ignored: %v", empty.Diagnostics)
	}

	// The opt-in must arrive from provider configuration, not default to on.
	configured := &resource.ConfigureResponse{}
	scheduled = NewScheduledTaskResource().(*scheduledTaskResource)
	scheduled.Configure(t.Context(), resource.ConfigureRequest{
		ProviderData: &dsmProviderData{client: client.NewClient("https://nas", "admin", "", false), allowTaskExecution: true},
	}, configured)
	if configured.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", configured.Diagnostics)
	}
	if !scheduled.allowTaskExecution || scheduled.client == nil {
		t.Error("provider data was not applied to the resource")
	}
}

func TestEnvBoolOnlyAcceptsClearAffirmatives(t *testing.T) {
	const name = "SYNOLOGY_DSM_TEST_ALLOW"

	for _, value := range []string{"true", "TRUE", "1", "yes", " true "} {
		t.Setenv(name, value)
		if !envBool(name) {
			t.Errorf("%q should enable the flag", value)
		}
	}
	// A typo must never be read as permission to run commands on the NAS.
	for _, value := range []string{"", "false", "0", "no", "ture", "on"} {
		t.Setenv(name, value)
		if envBool(name) {
			t.Errorf("%q must not enable the flag", value)
		}
	}
}

func TestCheckTaskExecutionAllowed(t *testing.T) {
	var allowed diag.Diagnostics
	checkTaskExecutionAllowed(true, "dsm_scheduled_task", &allowed)
	if allowed.HasError() {
		t.Errorf("an enabled provider must not produce an error: %v", allowed)
	}

	var refused diag.Diagnostics
	checkTaskExecutionAllowed(false, "dsm_scheduled_task", &refused)
	if !refused.HasError() {
		t.Fatal("task resources must be refused unless the provider opts in")
	}

	detail := refused.Errors()[0].Detail()
	for _, phrase := range []string{"allow_task_execution", "SYNOLOGY_DSM_ALLOW_TASK_EXECUTION", "dsm_scheduled_task", "root"} {
		if !strings.Contains(detail, phrase) {
			t.Errorf("refusal must explain %q, got: %s", phrase, detail)
		}
	}
}

func TestWarnAboutTaskPrivileges(t *testing.T) {
	httpsClient := client.NewClient("https://nas:5001", "admin", "pw", false)
	httpClient := client.NewClient("http://nas:5000", "admin", "pw", false)

	var quiet diag.Diagnostics
	warnAboutTaskPrivileges(httpsClient, types.StringValue("operator"), types.StringValue("backup"), &quiet)
	if len(quiet.Warnings()) != 0 {
		t.Errorf("a non-root task should not warn: %v", quiet.Warnings())
	}

	var root diag.Diagnostics
	warnAboutTaskPrivileges(httpsClient, types.StringValue("root"), types.StringValue("backup"), &root)
	if len(root.Warnings()) != 1 {
		t.Fatalf("a root task must warn exactly once over HTTPS, got %d", len(root.Warnings()))
	}
	if !strings.Contains(root.Warnings()[0].Summary(), "root") {
		t.Errorf("warning should name the privilege: %s", root.Warnings()[0].Summary())
	}

	var insecure diag.Diagnostics
	warnAboutTaskPrivileges(httpClient, types.StringValue("root"), types.StringValue("backup"), &insecure)
	if len(insecure.Warnings()) != 2 {
		t.Fatalf("a root task over plain HTTP must also warn about the password confirmation, got %d warnings", len(insecure.Warnings()))
	}
}

func TestApplyScheduledTaskToModel(t *testing.T) {
	model := &scheduledTaskResourceModel{}
	var diags diag.Diagnostics

	applyScheduledTaskToModel(t.Context(), model, &client.ScheduledTask{
		ID:        7,
		Name:      "prune",
		Owner:     "root",
		RealOwner: "root",
		Enabled:   true,
		Command:   "/bin/true",
		Schedule: client.TaskSchedule{
			Frequency:  "weekly",
			DaysOfWeek: []string{"sunday"},
			Hour:       4,
			Minute:     30,
		},
	}, &diags, applyForWrite)

	if diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if model.ID.ValueString() != "7" {
		t.Errorf("id = %q, want the DSM task id as a string", model.ID.ValueString())
	}
	if model.RealOwner.ValueString() != "root" {
		t.Error("real_owner must be recorded; DSM needs it to address the task later")
	}
	if model.NotifyEmail.ValueString() != "" || !model.NotifyEmail.IsNull() {
		t.Error("an absent notification address should be null, not an empty string")
	}
	if model.Schedule == nil || model.Schedule.Frequency.ValueString() != "weekly" {
		t.Fatalf("schedule was not mapped: %#v", model.Schedule)
	}
	if !model.Schedule.WeekOfMonth.IsNull() {
		t.Error("week_of_month must be null for a weekly task so it matches a config that omits it")
	}
}

func TestApplyScheduledTaskToModelRejectsUnsupportedSchedule(t *testing.T) {
	model := &scheduledTaskResourceModel{}
	var diags diag.Diagnostics

	// A one-shot DSM task: the client reports an empty frequency rather than
	// guessing, and the resource must surface that instead of writing a wrong
	// schedule into state.
	applyScheduledTaskToModel(t.Context(), model, &client.ScheduledTask{
		ID:   7,
		Name: "one-shot",
	}, &diags, applyForWrite)

	if !diags.HasError() {
		t.Fatal("expected an error for a schedule the provider cannot represent")
	}
	if !strings.Contains(diags.Errors()[0].Detail(), "one-shot") {
		t.Errorf("error should explain the situation, got: %s", diags.Errors()[0].Detail())
	}
}

func TestApplyEventTaskToModel(t *testing.T) {
	model := &eventTaskResourceModel{}
	var diags diag.Diagnostics

	applyEventTaskToModel(t.Context(), model, &client.EventTask{
		Name:      "restore-mounts",
		Owner:     "root",
		OwnerUID:  0,
		Event:     "bootup",
		Enabled:   true,
		Command:   "/volume1/scripts/restore-mounts.sh",
		DependsOn: []string{"mount-check"},
	}, &diags)

	if diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if model.ID.ValueString() != "restore-mounts" {
		t.Errorf("id = %q, want the task name, which is DSM's identifier for event tasks", model.ID.ValueString())
	}
	if model.OwnerUID.ValueInt64() != 0 {
		t.Errorf("owner_uid = %d, want 0 for root", model.OwnerUID.ValueInt64())
	}
	if model.DependsOnTasks.IsNull() {
		t.Error("depends_on_tasks should carry the dependency list")
	}
}

func TestScheduledTaskRequestRejectsNotifyOnFailureWithoutAddress(t *testing.T) {
	model := &scheduledTaskResourceModel{
		Name:            types.StringValue("prune"),
		User:            types.StringValue("root"),
		Command:         types.StringValue("/bin/true"),
		Enabled:         types.BoolValue(true),
		NotifyOnFailure: types.BoolValue(true),
		NotifyEmail:     types.StringNull(),
		Schedule: &taskScheduleModel{
			Frequency:   types.StringValue("daily"),
			DayOfWeek:   types.SetNull(types.StringType),
			WeekOfMonth: types.SetNull(types.StringType),
		},
	}

	var diags diag.Diagnostics
	if _, ok := scheduledTaskRequestFromModel(t.Context(), model, &diags); ok {
		t.Fatal("expected the request to be rejected")
	}
	if !strings.Contains(diags.Errors()[0].Summary(), "notify_on_failure requires notify_email") {
		t.Errorf("unexpected diagnostic: %s", diags.Errors()[0].Summary())
	}
}

func TestScheduledTaskRequestValidatesScheduleThroughClient(t *testing.T) {
	model := &scheduledTaskResourceModel{
		Name:    types.StringValue("prune"),
		User:    types.StringValue("root"),
		Command: types.StringValue("/bin/true"),
		Schedule: &taskScheduleModel{
			// weekly without any day: rejected by the client's encoding rules, which
			// the resource reuses rather than duplicating.
			Frequency:   types.StringValue("weekly"),
			DayOfWeek:   types.SetNull(types.StringType),
			WeekOfMonth: types.SetNull(types.StringType),
		},
	}

	var diags diag.Diagnostics
	if _, ok := scheduledTaskRequestFromModel(t.Context(), model, &diags); ok {
		t.Fatal("expected the schedule to be rejected")
	}
	if !strings.Contains(diags.Errors()[0].Detail(), "day_of_week") {
		t.Errorf("unexpected diagnostic: %s", diags.Errors()[0].Detail())
	}
}
