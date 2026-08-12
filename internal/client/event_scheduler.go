package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// ErrEventTaskNotFound is returned when DSM has no event-triggered task with
// the requested name.
var ErrEventTaskNotFound = errors.New("event task not found")

// Event Scheduler wire contract.
//
// SYNO.Core.EventScheduler is undocumented and its payload is shaped quite
// differently from SYNO.Core.TaskScheduler, despite the two sitting side by
// side in the same DSM UI:
//
//   - Every method is version 1.
//   - A task is addressed by task_name. There is no numeric id, so a task
//     cannot be renamed — a new name is a new task.
//   - The payload is flat. There is no schedule and no extra object: the
//     command goes in operation (with operation_type "script"), and the
//     notification settings are top-level parameters.
//   - owner is a JSON object mapping the account's numeric uid to its name,
//     for example {"0":"root"}, rather than the plain username TaskScheduler
//     takes.
//   - String parameters are JSON-quoted inside the form value, the same
//     convention SYNO.Docker.Project uses. notify_mail in particular is
//     documented by the python reference client as failing without the quotes.
//
// Transcribed from N4S4/synology-api event_scheduler.py and cross-checked
// against synology-community/go-synology. Not verified against hardware here.
const (
	apiEventScheduler     = "SYNO.Core.EventScheduler"
	apiEventSchedulerRoot = "SYNO.Core.EventScheduler.Root"

	eventSchedulerVersion = "1"

	// eventOperationTypeScript is the only operation type this provider creates.
	eventOperationTypeScript = "script"
)

// EventTriggers are the two events DSM can run a task on. DSM validates this
// server-side; the client checks first so the error names the allowed values.
var EventTriggers = []string{"bootup", "shutdown"}

// EventTask is a task DSM runs when a system event occurs, rather than on a
// schedule.
type EventTask struct {
	Name            string
	Owner           string
	OwnerUID        int
	Event           string
	Enabled         bool
	Command         string
	NotifyEmail     string
	NotifyOnFailure bool
	// DependsOn names tasks that must have completed before this one runs. DSM
	// encodes the list as "[Task A][Task B]" rather than as JSON.
	DependsOn []string
}

type EventTaskRequest struct {
	Name    string
	Owner   string
	Event   string
	Command string
	Enabled bool
	// NotifyEmail empty disables notification: DSM cannot notify without an
	// address, so notify_enable is derived rather than exposed separately.
	NotifyEmail     string
	NotifyOnFailure bool
	DependsOn       []string
}

func (c *Client) ListEventTasks(ctx context.Context) ([]EventTask, error) {
	// DSM has no list method on SYNO.Core.EventScheduler. Task Scheduler's list
	// returns event tasks alongside scheduled ones, but without the event, the
	// command, or the owner uid — enough to enumerate names, not to describe a
	// task. Each name is therefore resolved through the event API's own get.
	//
	// INFERRED: that event tasks are the entries carrying no id is the only
	// distinction the list response offers, and it is not confirmed against
	// hardware.
	tasks, err := c.listTasks(ctx)
	if err != nil {
		return nil, err
	}

	events := make([]EventTask, 0)
	for _, task := range tasks {
		if task.ID != 0 || task.Name == "" {
			continue
		}
		event, err := c.GetEventTask(ctx, task.Name)
		if err != nil {
			// A task that disappeared between the list and the get is a benign
			// race and is simply skipped. Anything else — a expired session, a
			// permission refusal, a transient network failure — must not be
			// silently turned into a shorter list, because a caller that acts on
			// that list (a sweeper, an audit) would conclude the task is absent.
			if errors.Is(err, ErrEventTaskNotFound) {
				continue
			}
			return nil, fmt.Errorf("list event tasks: %w", err)
		}
		events = append(events, *event)
	}
	return events, nil
}

func (c *Client) GetEventTask(ctx context.Context, name string) (*EventTask, error) {
	params := url.Values{}
	params.Set("task_name", jsonQuoted(name))

	data, err := c.DoAPI(ctx, apiEventScheduler, eventSchedulerVersion, "get", params)
	if err != nil {
		return nil, fmt.Errorf("get event task %q: %w", name, err)
	}

	task, err := parseEventTask(data)
	if err != nil {
		return nil, fmt.Errorf("parse event task %q: %w", name, err)
	}
	if task.Name == "" {
		// DSM answers an unknown name with an empty payload rather than an error.
		return nil, fmt.Errorf("%w: %q", ErrEventTaskNotFound, name)
	}
	return task, nil
}

func (c *Client) CreateEventTask(ctx context.Context, req EventTaskRequest) (*EventTask, error) {
	if err := c.writeEventTask(ctx, "create", req); err != nil {
		return nil, fmt.Errorf("create event task %q: %w", req.Name, err)
	}

	task, err := c.GetEventTask(ctx, req.Name)
	if err != nil {
		// The task now exists on the NAS. Returning nil here would leave a task
		// that runs on every boot, as root, with nothing tracking it and no way
		// for destroy to remove it — so hand back what was requested alongside
		// the error and let the caller record it.
		return eventTaskFromRequest(req), fmt.Errorf("event task %q was created but could not be read back: %w", req.Name, err)
	}
	return task, nil
}

// eventTaskFromRequest reconstructs what was just asked for, for use when DSM
// accepts a create but the read-back fails. Only ever returned with an error.
func eventTaskFromRequest(req EventTaskRequest) *EventTask {
	return &EventTask{
		Name:            req.Name,
		Owner:           req.Owner,
		Event:           req.Event,
		Enabled:         req.Enabled,
		Command:         req.Command,
		NotifyEmail:     req.NotifyEmail,
		NotifyOnFailure: req.NotifyOnFailure,
		DependsOn:       req.DependsOn,
	}
}

// UpdateEventTask rewrites an existing task. DSM's set replaces every field, so
// the request must carry the complete desired state. The task name is the
// primary key and cannot change.
func (c *Client) UpdateEventTask(ctx context.Context, req EventTaskRequest) (*EventTask, error) {
	if err := c.writeEventTask(ctx, "set", req); err != nil {
		return nil, fmt.Errorf("update event task %q: %w", req.Name, err)
	}
	return c.GetEventTask(ctx, req.Name)
}

func (c *Client) writeEventTask(ctx context.Context, method string, req EventTaskRequest) error {
	if !slices.Contains(EventTriggers, req.Event) {
		return fmt.Errorf("unsupported event %q: use %s", req.Event, strings.Join(EventTriggers, " or "))
	}

	owner, err := c.eventTaskOwner(ctx, req.Owner)
	if err != nil {
		return err
	}
	ownerJSON, err := json.Marshal(owner)
	if err != nil {
		return fmt.Errorf("encode event task owner: %w", err)
	}

	// DSM expects the dependency list as concatenated bracketed names, not JSON.
	var dependsOn strings.Builder
	for _, name := range req.DependsOn {
		dependsOn.WriteString("[" + name + "]")
	}

	params := url.Values{}
	params.Set("task_name", jsonQuoted(req.Name))
	params.Set("owner", string(ownerJSON))
	params.Set("event", jsonQuoted(req.Event))
	params.Set("depend_on_task", jsonQuoted(dependsOn.String()))
	params.Set("enable", boolParam(req.Enabled))
	params.Set("notify_enable", boolParam(req.NotifyEmail != ""))
	params.Set("notify_mail", jsonQuoted(req.NotifyEmail))
	params.Set("notify_if_error", boolParam(req.NotifyOnFailure))
	params.Set("operation", jsonQuoted(req.Command))
	params.Set("operation_type", jsonQuoted(eventOperationTypeScript))

	api := apiEventScheduler
	if req.Owner == "root" {
		token, err := c.passwordConfirmToken(ctx)
		if err != nil {
			return fmt.Errorf("confirm password for root-owned event task: %w", err)
		}
		params.Set("SynoConfirmPWToken", token)
		api = apiEventSchedulerRoot
	}

	// POST for the same reason as scheduled tasks: the command is arbitrary
	// user input of unbounded length.
	_, err = c.DoAPIPost(ctx, api, eventSchedulerVersion, method, params)
	return err
}

// eventTaskOwner builds DSM's {uid: name} owner map. root is uid 0 and is not a
// DSM user account, so it is special-cased; every other account is resolved
// through SYNO.Core.User to get its real uid.
func (c *Client) eventTaskOwner(ctx context.Context, name string) (map[string]string, error) {
	if name == "root" {
		return map[string]string{"0": "root"}, nil
	}

	user, err := c.GetUser(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("resolve uid for event task owner %q: %w", name, err)
	}
	return map[string]string{strconv.Itoa(user.UID): user.Name}, nil
}

func (c *Client) DeleteEventTask(ctx context.Context, name string) error {
	params := url.Values{}
	params.Set("task_name", jsonQuoted(name))

	if _, err := c.DoAPI(ctx, apiEventScheduler, eventSchedulerVersion, "delete", params); err != nil {
		return fmt.Errorf("delete event task %q: %w", name, err)
	}
	return nil
}

// RunEventTask triggers the task immediately, without waiting for its event.
func (c *Client) RunEventTask(ctx context.Context, name string) error {
	params := url.Values{}
	params.Set("task_name", jsonQuoted(name))

	if _, err := c.DoAPI(ctx, apiEventScheduler, eventSchedulerVersion, "run", params); err != nil {
		return fmt.Errorf("run event task %q: %w", name, err)
	}
	return nil
}

func parseEventTask(raw json.RawMessage) (*EventTask, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}

	task := &EventTask{
		Name:            stringField(m, "task_name"),
		Event:           stringField(m, "event"),
		Enabled:         boolField(m, "enable"),
		Command:         stringField(m, "operation"),
		NotifyEmail:     stringField(m, "notify_mail"),
		NotifyOnFailure: boolField(m, "notify_if_error"),
	}
	if task.Name == "" {
		// INFERRED: DSM's read path is expected to echo the same key it accepts,
		// but the reference implementations never captured a get response for
		// this API. Accept "name" as well rather than reporting a task that
		// plainly exists as missing.
		task.Name = stringField(m, "name")
	}
	if task.Command == "" {
		task.Command = stringField(m, "script")
	}
	if !boolField(m, "notify_enable") {
		task.NotifyEmail = ""
	}

	// owner comes back as the same {uid: name} map that is written.
	if owner, ok := m["owner"].(map[string]interface{}); ok {
		for uid, name := range owner {
			if named, ok := name.(string); ok {
				task.Owner = named
				task.OwnerUID, _ = strconv.Atoi(uid)
				break
			}
		}
	} else if owner := stringField(m, "owner"); owner != "" {
		task.Owner = owner
	}

	task.DependsOn = parseDependOnTask(stringField(m, "depend_on_task"))

	return task, nil
}

// parseDependOnTask splits DSM's "[Task A][Task B]" encoding back into names.
func parseDependOnTask(value string) []string {
	if value == "" {
		return nil
	}
	var names []string
	for _, part := range strings.Split(value, "[") {
		name := strings.TrimSuffix(part, "]")
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// jsonQuoted wraps a value in JSON string quotes, escaping as needed. Several
// DSM APIs expect string parameters in this form inside an ordinary form value.
func jsonQuoted(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		// json.Marshal of a string cannot fail; fall back rather than panic.
		return `"` + value + `"`
	}
	return string(encoded)
}
