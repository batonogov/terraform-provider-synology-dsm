package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// ErrScheduledTaskNotFound is returned when DSM has no task with the requested
// id. Task Scheduler answers a missing id with an empty payload rather than a
// dedicated error code, so the client synthesises this error instead.
var ErrScheduledTaskNotFound = &NotFoundError{Kind: "scheduled task"}

// Task Scheduler wire contract.
//
// SYNO.Core.TaskScheduler is undocumented. The parameter names, versions, and
// the schedule encoding below were transcribed from three independent working
// implementations that agree with each other: the python `synology-api`
// library (task_scheduler.py), synology-community/go-synology, and 007revad's
// task_setup.sh, whose comments record what a real DS218 running DSM 7
// accepted and rejected. Nothing here has been verified against hardware by
// this repository; the field comments flag which parts are transcription and
// which are inference.
//
// The API is versioned per method, which is unusual for DSM: list is version 3,
// get/create/set are version 4, and delete/run/set_enable are version 2.
// Version 4 on list does not work — it answers with nothing usable.
const (
	apiTaskScheduler     = "SYNO.Core.TaskScheduler"
	apiTaskSchedulerRoot = "SYNO.Core.TaskScheduler.Root"

	taskSchedulerListVersion   = "3"
	taskSchedulerConfigVersion = "4"
	taskSchedulerActionVersion = "2"

	// taskTypeScript is the task type DSM uses for "user-defined script" tasks,
	// which is the only type this provider creates.
	taskTypeScript = "script"
)

// ScheduledTask is a user-defined script task in DSM Task Scheduler.
//
// Owner and RealOwner are both reported by DSM and are usually identical. DSM
// requires RealOwner on get, set, and delete, so it is carried here rather than
// re-derived: a task created through the UI under one account can report a
// different real owner, and guessing wrong makes DSM answer with an empty task.
type ScheduledTask struct {
	ID              int
	Name            string
	Owner           string
	RealOwner       string
	Enabled         bool
	Command         string
	NotifyEmail     string
	NotifyOnFailure bool
	// Type is the DSM task type, for example "script" or "service". Tasks of
	// other types are listed but never created or modified by this provider.
	Type     string
	Schedule TaskSchedule
}

// TaskSchedule is the recurring schedule of a task, in the vocabulary this
// provider exposes rather than DSM's numeric encoding. encodeSchedule and
// parseSchedule translate between the two.
type TaskSchedule struct {
	// Frequency is one of daily, weekly, monthly.
	Frequency string
	// DaysOfWeek holds lowercase English weekday names. Daily tasks cover every
	// day; monthly tasks combine these with WeeksOfMonth ("the first Sunday").
	DaysOfWeek []string
	// WeeksOfMonth holds first, second, third, fourth, or last. Monthly only.
	WeeksOfMonth []string
	Hour         int
	Minute       int
	// RepeatIntervalHours and RepeatIntervalMinutes drive DSM's "continue
	// running within the same day" option; zero on both means the task runs once
	// per scheduled day. DSM accepts only a fixed set of minute intervals.
	RepeatIntervalHours   int
	RepeatIntervalMinutes int
	// RepeatUntilHour is DSM's last_work_hour: the last hour of the day at which
	// a same-day repeat may still fire.
	//
	// It is a pointer because "unset" and "0" are genuinely different. Unset
	// means "end the window at the start hour", which is what DSM's UI stores
	// for a task with no same-day repeat. A literal 0 with a later start hour is
	// a contradiction — the window would close before the task first runs — and
	// is rejected rather than quietly rewritten, because rewriting it makes
	// Terraform report an inconsistent result after apply.
	RepeatUntilHour *int
}

// CreateScheduledTaskRequest describes a task to create. Owner is the account
// DSM runs the command as; naming "root" routes the call through the .Root API
// and therefore grants the command full privileges on the NAS.
type CreateScheduledTaskRequest struct {
	Name            string
	Owner           string
	Command         string
	Enabled         bool
	NotifyEmail     string
	NotifyOnFailure bool
	Schedule        TaskSchedule
}

// UpdateScheduledTaskRequest is the same payload plus the identity DSM needs to
// address the existing task. DSM's set replaces every field, so callers must
// send the complete desired state.
type UpdateScheduledTaskRequest struct {
	CreateScheduledTaskRequest
	RealOwner string
}

// DSM repeat_date encodings for recurring schedules (schedule.date_type = 0).
// The one-shot encodings (date_type = 1: no_repeat, yearly, every N months) are
// deliberately not exposed: a task that runs on a single fixed date does not
// describe an ongoing desired state, which is what Terraform manages.
const (
	repeatDateDaily   = 1001
	repeatDateWeekly  = 1002
	repeatDateMonthly = 1003
)

// weekdayNames maps DSM's week_day digits to names. 0 is Sunday.
var weekdayNames = []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}

// weeksOfMonth are the values DSM accepts in monthly_week.
var weeksOfMonth = []string{"first", "second", "third", "fourth", "last"}

// ValidRepeatIntervalMinutes lists the same-day repeat intervals DSM's UI
// offers. DSM stores the allowed set per task in repeat_min_store_config; the
// values here are the ones the UI presents, and anything else is rejected
// client-side rather than silently ignored by DSM.
var ValidRepeatIntervalMinutes = []int{1, 5, 10, 15, 20, 30}

// ListScheduledTasks returns the scheduled tasks known to DSM.
//
// DSM's list mixes scheduled and event-triggered tasks into one array and tells
// them apart only by the presence of an id, so event tasks are filtered out
// here and surfaced by ListEventTasks instead.
func (c *Client) ListScheduledTasks(ctx context.Context) ([]ScheduledTask, error) {
	all, err := c.listTasks(ctx)
	if err != nil {
		return nil, err
	}

	tasks := make([]ScheduledTask, 0, len(all))
	for _, task := range all {
		if task.ID == 0 {
			continue
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

// listTasks returns every entry of DSM's task list, scheduled and event-driven
// alike.
func (c *Client) listTasks(ctx context.Context) ([]ScheduledTask, error) {
	params := url.Values{}
	params.Set("sort_by", "next_trigger_time")
	params.Set("sort_direction", "ASC")
	params.Set("offset", "0")
	// INFERRED: limit=-1 means "everything" across the DSM APIs this provider
	// already uses (user, group, share). The reference implementations pass a
	// finite page size here instead, so if a NAS with many tasks ever returns a
	// truncated list, this is the line to revisit.
	params.Set("limit", "-1")

	data, err := c.DoAPI(ctx, apiTaskScheduler, taskSchedulerListVersion, "list", params)
	if err != nil {
		return nil, fmt.Errorf("list scheduled tasks: %w", err)
	}

	var result struct {
		Tasks []json.RawMessage `json:"tasks"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse scheduled task list: %w", err)
	}

	tasks := make([]ScheduledTask, 0, len(result.Tasks))
	for _, raw := range result.Tasks {
		task, err := parseScheduledTaskSummary(raw)
		if err != nil {
			continue
		}
		tasks = append(tasks, *task)
	}
	return tasks, nil
}

// GetScheduledTask returns the full configuration of a task.
//
// It lists first because get requires real_owner, which only list and get
// report — and get cannot be called without it. The extra round trip is the
// price of an API that needs part of its own answer as input.
func (c *Client) GetScheduledTask(ctx context.Context, id int) (*ScheduledTask, error) {
	summary, err := c.findScheduledTask(ctx, func(t ScheduledTask) bool { return t.ID == id })
	if err != nil {
		return nil, err
	}
	return c.getScheduledTaskConfig(ctx, summary.ID, summary.RealOwner)
}

// GetScheduledTaskByName resolves a task by its DSM name. Task names are not
// unique in DSM, so an ambiguous name is an error rather than a coin flip.
func (c *Client) GetScheduledTaskByName(ctx context.Context, name string) (*ScheduledTask, error) {
	tasks, err := c.ListScheduledTasks(ctx)
	if err != nil {
		return nil, err
	}

	var matches []ScheduledTask
	for _, task := range tasks {
		if task.Name == name {
			matches = append(matches, task)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("%w: %q", ErrScheduledTaskNotFound, name)
	case 1:
		return c.getScheduledTaskConfig(ctx, matches[0].ID, matches[0].RealOwner)
	default:
		ids := make([]string, 0, len(matches))
		for _, match := range matches {
			ids = append(ids, strconv.Itoa(match.ID))
		}
		return nil, fmt.Errorf("scheduled task name %q is ambiguous: DSM has %d tasks with this name (ids %s); use the numeric id instead",
			name, len(matches), strings.Join(ids, ", "))
	}
}

func (c *Client) findScheduledTask(ctx context.Context, match func(ScheduledTask) bool) (*ScheduledTask, error) {
	tasks, err := c.ListScheduledTasks(ctx)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		if match(tasks[i]) {
			return &tasks[i], nil
		}
	}
	return nil, ErrScheduledTaskNotFound
}

func (c *Client) getScheduledTaskConfig(ctx context.Context, id int, realOwner string) (*ScheduledTask, error) {
	params := url.Values{}
	params.Set("id", strconv.Itoa(id))
	params.Set("real_owner", realOwner)

	data, err := c.DoAPI(ctx, apiTaskScheduler, taskSchedulerConfigVersion, "get", params)
	if err != nil {
		return nil, fmt.Errorf("get scheduled task %d: %w", id, err)
	}

	task, err := parseScheduledTaskConfig(data)
	if err != nil {
		return nil, fmt.Errorf("parse scheduled task %d: %w", id, err)
	}
	if task.ID == 0 {
		task.ID = id
	}
	if task.RealOwner == "" {
		task.RealOwner = realOwner
	}
	if task.Name == "" {
		return nil, fmt.Errorf("%w: id %d", ErrScheduledTaskNotFound, id)
	}
	return task, nil
}

func (c *Client) CreateScheduledTask(ctx context.Context, req CreateScheduledTaskRequest) (*ScheduledTask, error) {
	params, err := scheduledTaskParams(req)
	if err != nil {
		return nil, err
	}
	params.Set("real_owner", req.Owner)
	params.Set("type", taskTypeScript)

	api, err := c.taskSchedulerAPI(ctx, req.Owner, params)
	if err != nil {
		return nil, err
	}

	// POST rather than GET: the command is arbitrary user input and can be long
	// enough to overrun a URL. The reference python client uses GET here and
	// works, but only because its scripts are short.
	data, err := c.DoAPIPost(ctx, api, taskSchedulerConfigVersion, "create", params)
	if err != nil {
		return nil, fmt.Errorf("create scheduled task %q: %w", req.Name, err)
	}

	var result struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse create scheduled task response: %w", err)
	}
	if result.ID == 0 {
		return nil, fmt.Errorf("create scheduled task %q: DSM did not return a task id", req.Name)
	}

	// Read back through GetScheduledTask, which lists first to learn the real
	// owner. Guessing that it equals req.Owner is wrong in exactly the case this
	// resource exists for: creating a root-owned task while authenticated as
	// admin. DSM answers a get with the wrong real_owner by returning an empty
	// task, which would surface as "not found" for a task it had just created.
	task, err := c.GetScheduledTask(ctx, result.ID)
	if err != nil {
		// The task exists — DSM returned its id — so the caller must be able to
		// record it even though the read-back failed. Returning a nil task here
		// would leave a root-owned task running on the NAS that no Terraform
		// state knows about, and that destroy would never remove.
		return scheduledTaskFromRequest(req, result.ID), fmt.Errorf(
			"scheduled task %q was created as id %d but could not be read back: %w", req.Name, result.ID, err)
	}
	return task, nil
}

// scheduledTaskFromRequest reconstructs what was just asked for, for use when
// DSM accepts a create but the read-back fails. The values are the requested
// ones rather than DSM's, and RealOwner is the caller's best guess — which is
// why this is only ever returned alongside an error.
func scheduledTaskFromRequest(req CreateScheduledTaskRequest, id int) *ScheduledTask {
	schedule := req.Schedule
	if schedule.RepeatUntilHour == nil {
		// Mirror the default encodeSchedule applied on the way out, so the value
		// recorded in state is the one DSM was actually told to store.
		lastWorkHour := schedule.Hour
		schedule.RepeatUntilHour = &lastWorkHour
	}

	return &ScheduledTask{
		ID:              id,
		Name:            req.Name,
		Owner:           req.Owner,
		RealOwner:       req.Owner,
		Enabled:         req.Enabled,
		Command:         req.Command,
		NotifyEmail:     req.NotifyEmail,
		NotifyOnFailure: req.NotifyOnFailure,
		Type:            taskTypeScript,
		Schedule:        schedule,
	}
}

func (c *Client) UpdateScheduledTask(ctx context.Context, id int, req UpdateScheduledTaskRequest) (*ScheduledTask, error) {
	params, err := scheduledTaskParams(req.CreateScheduledTaskRequest)
	if err != nil {
		return nil, err
	}
	params.Set("id", strconv.Itoa(id))
	realOwner := req.RealOwner
	if realOwner == "" {
		realOwner = req.Owner
	}
	params.Set("real_owner", realOwner)

	api, err := c.taskSchedulerAPI(ctx, req.Owner, params)
	if err != nil {
		return nil, err
	}

	// set replaces every field of the task, so callers must always send the
	// complete desired state; there is no partial update.
	if _, err := c.DoAPIPost(ctx, api, taskSchedulerConfigVersion, "set", params); err != nil {
		return nil, fmt.Errorf("update scheduled task %d: %w", id, err)
	}

	return c.getScheduledTaskConfig(ctx, id, realOwner)
}

// DeleteScheduledTask removes a task. DSM takes a JSON array here so that the
// UI can delete a multi-selection in one call; a single-element array is the
// only shape this provider needs.
func (c *Client) DeleteScheduledTask(ctx context.Context, id int, realOwner string) error {
	tasks, err := json.Marshal([]map[string]interface{}{{
		"id":         id,
		"real_owner": realOwner,
	}})
	if err != nil {
		return fmt.Errorf("encode scheduled task selection: %w", err)
	}

	params := url.Values{}
	params.Set("tasks", string(tasks))

	if _, err := c.DoAPI(ctx, apiTaskScheduler, taskSchedulerActionVersion, "delete", params); err != nil {
		return fmt.Errorf("delete scheduled task %d: %w", id, err)
	}
	return nil
}

// RunScheduledTask triggers a task immediately, independently of its schedule.
func (c *Client) RunScheduledTask(ctx context.Context, id int, realOwner string) error {
	tasks, err := json.Marshal([]map[string]interface{}{{
		"id":         id,
		"real_owner": realOwner,
	}})
	if err != nil {
		return fmt.Errorf("encode scheduled task selection: %w", err)
	}

	params := url.Values{}
	params.Set("tasks", string(tasks))

	if _, err := c.DoAPI(ctx, apiTaskScheduler, taskSchedulerActionVersion, "run", params); err != nil {
		return fmt.Errorf("run scheduled task %d: %w", id, err)
	}
	return nil
}

// taskSchedulerAPI picks the API namespace for a mutation and, for root-owned
// tasks, adds the password confirmation token DSM demands.
//
// This is the privilege boundary: SYNO.Core.TaskScheduler.Root is what makes a
// task run with full privileges, and DSM guards it by requiring the caller to
// re-confirm the account password. The client therefore only reaches for it
// when the caller explicitly asked for owner "root".
func (c *Client) taskSchedulerAPI(ctx context.Context, owner string, params url.Values) (string, error) {
	if owner != "root" {
		return apiTaskScheduler, nil
	}

	token, err := c.passwordConfirmToken(ctx)
	if err != nil {
		return "", fmt.Errorf("confirm password for root-owned task: %w", err)
	}
	params.Set("SynoConfirmPWToken", token)
	return apiTaskSchedulerRoot, nil
}

// passwordConfirmToken re-confirms the provider's own password and returns the
// short-lived token DSM requires for privileged operations. The token's
// lifetime is undocumented, so one is fetched per privileged call rather than
// cached.
//
// The password is sent as a plain form value. DSM's own web client RSA-encrypts
// it through SYNO.API.Encryption when the connection is not HTTPS; this client
// does not, matching go-synology. Over a plain-HTTP host that means the
// password crosses the wire in the clear on this one call — which is why
// root-owned tasks over HTTP get an explicit warning from the resource layer
// rather than a silent downgrade.
func (c *Client) passwordConfirmToken(ctx context.Context) (string, error) {
	params := url.Values{}
	params.Set("password", c.password)

	data, err := c.DoAPIPost(ctx, "SYNO.Core.User.PasswordConfirm", "2", "auth", params)
	if err != nil {
		return "", fmt.Errorf("password confirmation: %w", err)
	}

	var result struct {
		Token string `json:"SynoConfirmPWToken"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("parse password confirmation response: %w", err)
	}
	if result.Token == "" {
		return "", errors.New("password confirmation: DSM returned an empty SynoConfirmPWToken")
	}
	return result.Token, nil
}

// scheduledTaskParams builds the parameters shared by create and set. DSM takes
// schedule and extra as JSON documents inside form values, not as flattened
// parameters.
func scheduledTaskParams(req CreateScheduledTaskRequest) (url.Values, error) {
	schedule, err := encodeSchedule(req.Schedule)
	if err != nil {
		return nil, err
	}
	scheduleJSON, err := json.Marshal(schedule)
	if err != nil {
		return nil, fmt.Errorf("encode task schedule: %w", err)
	}

	// notify_enable is derived rather than exposed: DSM has no way to send a
	// notification without an address, so an empty address is the off switch.
	extraJSON, err := json.Marshal(map[string]interface{}{
		"notify_enable":   req.NotifyEmail != "",
		"notify_mail":     req.NotifyEmail,
		"notify_if_error": req.NotifyOnFailure,
		"script":          req.Command,
	})
	if err != nil {
		return nil, fmt.Errorf("encode task payload: %w", err)
	}

	params := url.Values{}
	params.Set("name", req.Name)
	params.Set("owner", req.Owner)
	params.Set("enable", boolParam(req.Enabled))
	params.Set("schedule", string(scheduleJSON))
	params.Set("extra", string(extraJSON))
	return params, nil
}

// ValidateTaskSchedule reports whether a schedule can be expressed in DSM's
// encoding. It exists so the resource layer can reject a bad schedule during
// plan with the same rules the request path applies, rather than duplicating
// them and letting the two drift apart.
func ValidateTaskSchedule(schedule TaskSchedule) (map[string]interface{}, error) {
	return encodeSchedule(schedule)
}

// encodeSchedule translates a TaskSchedule into DSM's numeric schedule
// document. Field meanings:
//
//	date_type       0 for a recurring schedule, 1 for a single date
//	repeat_date     1001 daily, 1002 weekly, 1003 monthly-by-weekday
//	week_day        comma-separated day numbers, 0 = Sunday (not a bitmask)
//	monthly_week    weeks of the month, e.g. ["first","third"]
//	hour/minute     start time
//	repeat_hour     same-day repeat interval in hours, 0 = off
//	repeat_min      same-day repeat interval in minutes, 0 = off
//	last_work_hour  last hour at which a same-day repeat may fire
//	version         the schedule format version, echoed by DSM on read
//
// Two details are worth recording because the reference implementations differ:
//
//   - The nested "version": 4 is what a real DSM 7 accepted. The python client
//     omits it and is reported working, but only the payload carrying it has an
//     explicit hardware confirmation, and DSM's own get response includes it.
//   - monthly_week is sent as a JSON array. The python client double-encodes it
//     as a string nested inside this document; DSM tolerates that, but every
//     other implementation — and DSM's own read path — uses an array.
func encodeSchedule(schedule TaskSchedule) (map[string]interface{}, error) {
	// Frequency is checked before anything derived from it, so an unknown value
	// is reported as such rather than as a confusing consequence.
	var repeatDate int
	switch schedule.Frequency {
	case "daily":
		repeatDate = repeatDateDaily
	case "weekly":
		repeatDate = repeatDateWeekly
	case "monthly":
		repeatDate = repeatDateMonthly
	default:
		return nil, fmt.Errorf("unsupported schedule frequency %q: use daily, weekly, or monthly", schedule.Frequency)
	}

	days, err := encodeWeekDays(schedule)
	if err != nil {
		return nil, err
	}

	weeks := schedule.WeeksOfMonth
	if weeks == nil {
		weeks = []string{}
	}
	if schedule.Frequency != "monthly" && len(weeks) > 0 {
		return nil, fmt.Errorf("week_of_month only applies to a monthly schedule, not %q", schedule.Frequency)
	}
	for _, week := range weeks {
		if !slices.Contains(weeksOfMonth, week) {
			return nil, fmt.Errorf("unsupported week_of_month %q: use %s", week, strings.Join(weeksOfMonth, ", "))
		}
	}
	if schedule.Frequency == "monthly" && len(weeks) == 0 {
		return nil, errors.New("a monthly schedule requires at least one week_of_month value")
	}

	if schedule.Hour < 0 || schedule.Hour > 23 {
		return nil, fmt.Errorf("hour must be between 0 and 23, got %d", schedule.Hour)
	}
	if schedule.Minute < 0 || schedule.Minute > 59 {
		return nil, fmt.Errorf("minute must be between 0 and 59, got %d", schedule.Minute)
	}
	if schedule.RepeatIntervalHours < 0 || schedule.RepeatIntervalHours > 23 {
		return nil, fmt.Errorf("repeat_interval_hours must be between 0 and 23, got %d", schedule.RepeatIntervalHours)
	}
	if schedule.RepeatIntervalMinutes != 0 && !slices.Contains(ValidRepeatIntervalMinutes, schedule.RepeatIntervalMinutes) {
		return nil, fmt.Errorf("repeat_interval_minutes must be one of %s, or 0 to disable, got %d",
			joinInts(ValidRepeatIntervalMinutes), schedule.RepeatIntervalMinutes)
	}
	if schedule.RepeatIntervalHours > 0 && schedule.RepeatIntervalMinutes > 0 {
		return nil, errors.New("repeat_interval_hours and repeat_interval_minutes are mutually exclusive")
	}

	// DSM stores the end of the repeat window even when no repeat is configured;
	// it then equals the start hour, which is what its UI sends.
	lastWorkHour := schedule.Hour
	if schedule.RepeatUntilHour != nil {
		lastWorkHour = *schedule.RepeatUntilHour
		if lastWorkHour < 0 || lastWorkHour > 23 {
			return nil, fmt.Errorf("repeat_until_hour must be between 0 and 23, got %d", lastWorkHour)
		}
		// A window that closes before the task starts would silently reduce a
		// repeating task to a single run, which is the opposite of what asking
		// for a repeat means.
		if lastWorkHour < schedule.Hour {
			return nil, fmt.Errorf("repeat_until_hour (%d) must not be earlier than hour (%d): the repeat window would close before the task first runs",
				lastWorkHour, schedule.Hour)
		}
	}

	return map[string]interface{}{
		"date_type":      0,
		"repeat_date":    repeatDate,
		"week_day":       days,
		"monthly_week":   weeks,
		"hour":           schedule.Hour,
		"minute":         schedule.Minute,
		"repeat_hour":    schedule.RepeatIntervalHours,
		"repeat_min":     schedule.RepeatIntervalMinutes,
		"last_work_hour": lastWorkHour,
		"version":        4,
	}, nil
}

// encodeWeekDays renders DSM's week_day string. A daily schedule always covers
// every weekday, which is how DSM itself represents "daily": repeat_date 1001
// with all seven days selected.
func encodeWeekDays(schedule TaskSchedule) (string, error) {
	if schedule.Frequency == "daily" {
		if len(schedule.DaysOfWeek) > 0 {
			return "", errors.New("day_of_week does not apply to a daily schedule, which always runs every day")
		}
		return "0,1,2,3,4,5,6", nil
	}

	if len(schedule.DaysOfWeek) == 0 {
		return "", fmt.Errorf("a %s schedule requires at least one day_of_week value", schedule.Frequency)
	}

	seen := map[int]bool{}
	numbers := make([]int, 0, len(schedule.DaysOfWeek))
	for _, day := range schedule.DaysOfWeek {
		index := -1
		for i, name := range weekdayNames {
			if strings.EqualFold(day, name) {
				index = i
				break
			}
		}
		if index < 0 {
			return "", fmt.Errorf("unknown day_of_week %q: use %s", day, strings.Join(weekdayNames, ", "))
		}
		if seen[index] {
			continue
		}
		seen[index] = true
		numbers = append(numbers, index)
	}

	// DSM writes the days in ascending order; matching that keeps a refreshed
	// task byte-identical to what was sent and avoids a phantom diff.
	sort.Ints(numbers)
	rendered := make([]string, len(numbers))
	for i, number := range numbers {
		rendered[i] = strconv.Itoa(number)
	}
	return strings.Join(rendered, ","), nil
}

// parseScheduledTaskSummary reads one entry of the list response. List omits
// the schedule and the command; only get returns those.
func parseScheduledTaskSummary(raw json.RawMessage) (*ScheduledTask, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}

	// An entry with no id is an event-triggered task; callers filter on that
	// rather than treating it as malformed.
	return &ScheduledTask{
		ID:        intField(m, "id"),
		Name:      stringField(m, "name"),
		Owner:     stringField(m, "owner"),
		RealOwner: stringField(m, "real_owner"),
		Enabled:   boolField(m, "enable"),
		Type:      stringField(m, "type"),
	}, nil
}

func parseScheduledTaskConfig(raw json.RawMessage) (*ScheduledTask, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}

	task := &ScheduledTask{
		ID:        intField(m, "id"),
		Name:      stringField(m, "name"),
		Owner:     stringField(m, "owner"),
		RealOwner: stringField(m, "real_owner"),
		Enabled:   boolField(m, "enable"),
		Type:      stringField(m, "type"),
	}

	if extra, ok := m["extra"].(map[string]interface{}); ok {
		task.Command = stringField(extra, "script")
		task.NotifyEmail = stringField(extra, "notify_mail")
		task.NotifyOnFailure = boolField(extra, "notify_if_error")
		// notify_enable off means DSM ignores the stored address; report it as
		// unset so Terraform state matches the effective configuration.
		if !boolField(extra, "notify_enable") {
			task.NotifyEmail = ""
		}
	}

	if schedule, ok := m["schedule"].(map[string]interface{}); ok {
		task.Schedule = parseSchedule(schedule)
	}

	return task, nil
}

// parseSchedule converts DSM's numeric schedule back into the provider's
// vocabulary. An unrecognised repeat_date leaves Frequency empty, which the
// resource layer reports rather than silently mapping to "daily": tasks created
// in DSM's UI can use one-shot schedules this provider does not model.
func parseSchedule(m map[string]interface{}) TaskSchedule {
	schedule := TaskSchedule{
		Hour:                  intField(m, "hour"),
		Minute:                intField(m, "minute"),
		RepeatIntervalHours:   intField(m, "repeat_hour"),
		RepeatIntervalMinutes: intField(m, "repeat_min"),
	}
	// DSM always reports a concrete last_work_hour, so the read side is never
	// "unset" even when the caller left it so on the way in.
	lastWorkHour := intField(m, "last_work_hour")
	schedule.RepeatUntilHour = &lastWorkHour

	switch intField(m, "repeat_date") {
	case repeatDateDaily:
		schedule.Frequency = "daily"
	case repeatDateWeekly:
		schedule.Frequency = "weekly"
	case repeatDateMonthly:
		schedule.Frequency = "monthly"
	}

	if days := stringField(m, "week_day"); days != "" && schedule.Frequency != "daily" {
		for _, part := range strings.Split(days, ",") {
			index, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || index < 0 || index >= len(weekdayNames) {
				continue
			}
			schedule.DaysOfWeek = append(schedule.DaysOfWeek, weekdayNames[index])
		}
	}

	// This client writes monthly_week as an array and DSM reads it back as one.
	// The string case is here for the python reference client's double-encoded
	// form, which DSM also accepts and therefore may already be stored on a NAS
	// whose tasks were created with that library.
	switch value := m["monthly_week"].(type) {
	case []interface{}:
		for _, item := range value {
			if week, ok := item.(string); ok {
				schedule.WeeksOfMonth = append(schedule.WeeksOfMonth, week)
			}
		}
	case string:
		if value != "" {
			var weeks []string
			if err := json.Unmarshal([]byte(value), &weeks); err == nil {
				schedule.WeeksOfMonth = append(schedule.WeeksOfMonth, weeks...)
			}
		}
	}

	return schedule
}

// The helpers below mirror parseX() elsewhere in this package: DSM responses are
// loose JSON, so fields are read defensively rather than through typed structs.
func stringField(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func intField(m map[string]interface{}, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case string:
		if parsed, err := strconv.Atoi(v); err == nil {
			return parsed
		}
	}
	return 0
}

func boolField(m map[string]interface{}, key string) bool {
	switch v := m[key].(type) {
	case bool:
		return v
	case string:
		parsed, err := strconv.ParseBool(v)
		return err == nil && parsed
	}
	return false
}

func joinInts(values []int) string {
	rendered := make([]string, len(values))
	for i, value := range values {
		rendered[i] = strconv.Itoa(value)
	}
	return strings.Join(rendered, ", ")
}
