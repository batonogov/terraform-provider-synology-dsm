package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

var ErrContainerProjectNotFound = &NotFoundError{Kind: "Container Manager project"}

var (
	containerProjectPollInterval = 2 * time.Second
	containerProjectTaskTimeout  = 10 * time.Minute
)

// ContainerProject is a Docker Compose project managed by DSM Container
// Manager. SharePath is the File Station path (for example /docker/s3-storage),
// while Path is the corresponding absolute volume path reported by DSM.
type ContainerProject struct {
	ID           string
	Name         string
	SharePath    string
	Path         string
	ComposeYAML  string
	Status       string
	ContainerIDs []string
}

// Running reports whether the project counts as up.
//
// WARNING counts. Container Manager reports it when some containers are not
// running while others are — which is the steady state of any compose file with
// a one-shot init container: the init container writes its config, exits 0, and
// never runs again while the long-lived services carry on. That pattern is the
// documented Compose idiom for setup work, so treating WARNING as "not yet
// running" meant waiting the full ten-minute timeout for a state that had
// already settled, and failing an apply whose services were running fine
// (issue #69).
//
// The cost is that a genuinely broken container also leaves the project in
// WARNING and no longer fails the apply. That is reported instead: the resource
// raises a Terraform warning naming the status, and `status` is in state for
// anyone who wants to assert on it.
func (p ContainerProject) Running() bool {
	switch strings.ToUpper(p.Status) {
	case statusRunning, statusWarning:
		return true
	default:
		return false
	}
}

// statusWarning is DSM's "some containers are up, some are not" status.
const statusWarning = "WARNING"

// PartiallyRunning reports whether the project is up but not every container is
// running — the case worth telling the user about.
func (p ContainerProject) PartiallyRunning() bool {
	return strings.EqualFold(p.Status, statusWarning)
}

// ListContainerProjects returns all projects known to Container Manager. DSM
// represents this collection as an object keyed by project UUID, rather than
// as a conventional JSON array.
func (c *Client) ListContainerProjects(ctx context.Context) ([]ContainerProject, error) {
	data, err := c.DoAPIPost(ctx, "SYNO.Docker.Project", "1", "list", nil)
	if err != nil {
		return nil, fmt.Errorf("list Container Manager projects: %w", err)
	}

	projects, err := parseContainerProjectList(data)
	if err != nil {
		return nil, fmt.Errorf("parse Container Manager project list: %w", err)
	}
	return projects, nil
}

func (c *Client) GetContainerProject(ctx context.Context, id string) (*ContainerProject, error) {
	params := url.Values{}
	params.Set("id", id)

	data, err := c.DoAPIPost(ctx, "SYNO.Docker.Project", "1", "get", params)
	if err != nil {
		if isContainerProjectMissingError(err) {
			return nil, fmt.Errorf("%w: %q", ErrContainerProjectNotFound, id)
		}
		return nil, fmt.Errorf("get Container Manager project %q: %w", id, err)
	}

	project, err := parseContainerProject(data, id)
	if err != nil {
		return nil, fmt.Errorf("parse Container Manager project %q: %w", id, err)
	}
	if project.ID == "" {
		project.ID = id
	}
	if project.Name == "" {
		return nil, fmt.Errorf("%w: %q", ErrContainerProjectNotFound, id)
	}

	// Some DSM builds omit compose content from get and expose it only through
	// get_share_info. Keep the fallback here so resources and data sources see a
	// consistent project regardless of the DSM minor version.
	if project.ComposeYAML == "" {
		projectPath := project.Path
		if projectPath == "" {
			projectPath = project.SharePath
		}
		if projectPath != "" {
			compose, shareInfoErr := c.getContainerProjectCompose(ctx, projectPath)
			if shareInfoErr != nil {
				return nil, shareInfoErr
			}
			project.ComposeYAML = compose
		}
	}

	return project, nil
}

func (c *Client) GetContainerProjectByName(ctx context.Context, name string) (*ContainerProject, error) {
	project, err := c.findContainerProjectByName(ctx, name)
	if err != nil {
		return nil, err
	}
	return c.GetContainerProject(ctx, project.ID)
}

func (c *Client) findContainerProjectByName(ctx context.Context, name string) (*ContainerProject, error) {
	projects, err := c.ListContainerProjects(ctx)
	if err != nil {
		return nil, err
	}
	for i := range projects {
		if projects[i].Name == name {
			return &projects[i], nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrContainerProjectNotFound, name)
}

// CreateContainerProject follows the sequence used by DSM's own UI: create the
// File Station directory, register an empty project, write the compose file,
// build it, then optionally start it.
func (c *Client) CreateContainerProject(ctx context.Context, name, sharePath, composeYAML string, running bool) (*ContainerProject, error) {
	c.containerProjectMu.Lock()
	defer c.containerProjectMu.Unlock()

	if _, err := c.findContainerProjectByName(ctx, name); err == nil {
		return nil, fmt.Errorf("Container Manager project %q already exists; import it instead", name)
	} else if !errors.Is(err, ErrContainerProjectNotFound) {
		return nil, err
	}

	if err := c.createContainerProjectFolder(ctx, sharePath); err != nil {
		return nil, err
	}

	params := url.Values{}
	params.Set("name", jsonString(name))
	params.Set("share_path", jsonString(sharePath))
	params.Set("content", jsonString(""))
	params.Set("enable_service_portal", "false")
	params.Set("service_portal_name", jsonString(""))
	params.Set("service_portal_port", "0")
	params.Set("service_portal_protocol", jsonString(""))
	if _, err := c.DoAPIPost(WithSlowCall(ctx), "SYNO.Docker.Project", "1", "create", params); err != nil {
		// As with the compose write, a timeout here means "no answer", not "no
		// project": DSM may have created it and simply replied late. Look before
		// giving up, or the next apply inherits an orphan it has to import by hand.
		if !IsTimeoutError(err) {
			return nil, fmt.Errorf("create Container Manager project %q: %w", name, err)
		}
		if _, found := c.findContainerProjectByName(ctx, name); found != nil {
			return nil, fmt.Errorf("create Container Manager project %q: %w", name, err)
		}
	}

	project, err := c.findContainerProjectByName(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("find newly created Container Manager project %q: %w", name, err)
	}
	if err := c.updateContainerProjectCompose(ctx, project.ID, composeYAML); err != nil {
		return nil, err
	}
	if err := c.runContainerProjectAction(ctx, project.ID, "build"); err != nil {
		return nil, err
	}
	if _, err := c.waitForContainerProjectSettled(ctx, project.ID); err != nil {
		return nil, err
	}
	if running {
		if err := c.runContainerProjectAction(ctx, project.ID, "start"); err != nil {
			return nil, err
		}
	}

	return c.waitForContainerProject(ctx, project.ID, func(p *ContainerProject) bool {
		return containerProjectReached(p, running)
	})
}

// SetContainerProjectRunning starts or stops a project without touching its
// compose document.
//
// UpdateContainerProject decides whether to rebuild by comparing the document
// it is handed against the one DSM holds, which means a caller that only wants
// to start or stop a project has to supply the current document — and a caller
// managing the document write-only does not have it. Asking for the running
// state on its own removes both the guesswork and the window in which a
// re-read document could be stale.
func (c *Client) SetContainerProjectRunning(ctx context.Context, id string, running bool) (*ContainerProject, error) {
	c.containerProjectMu.Lock()
	defer c.containerProjectMu.Unlock()

	project, err := c.GetContainerProject(ctx, id)
	if err != nil {
		return nil, err
	}

	if project.Running() != running {
		action := "stop"
		if running {
			action = "start"
		}
		if err := c.runContainerProjectAction(ctx, id, action); err != nil {
			return nil, err
		}
	}

	return c.waitForContainerProject(ctx, id, func(p *ContainerProject) bool {
		return containerProjectReached(p, running)
	})
}

// UpdateContainerProject applies compose changes in a deterministic order. A
// running project is stopped before rebuilding and restored to the requested
// running state afterwards.
func (c *Client) UpdateContainerProject(ctx context.Context, id, composeYAML string, running bool) (*ContainerProject, error) {
	c.containerProjectMu.Lock()
	defer c.containerProjectMu.Unlock()

	project, err := c.GetContainerProject(ctx, id)
	if err != nil {
		return nil, err
	}

	composeChanged := project.ComposeYAML != composeYAML
	if composeChanged {
		if project.Running() {
			if err := c.runContainerProjectAction(ctx, id, "stop"); err != nil {
				return nil, err
			}
			if _, err := c.waitForContainerProjectRunningState(ctx, id, false); err != nil {
				return nil, err
			}
		}
		if err := c.updateContainerProjectCompose(ctx, id, composeYAML); err != nil {
			return nil, err
		}
		if err := c.runContainerProjectAction(ctx, id, "build"); err != nil {
			return nil, err
		}
		if _, err := c.waitForContainerProjectSettled(ctx, id); err != nil {
			return nil, err
		}
		if running {
			if err := c.runContainerProjectAction(ctx, id, "start"); err != nil {
				return nil, err
			}
		}
	} else if project.Running() != running {
		action := "stop"
		if running {
			action = "start"
		}
		if err := c.runContainerProjectAction(ctx, id, action); err != nil {
			return nil, err
		}
	}

	return c.waitForContainerProject(ctx, id, func(p *ContainerProject) bool {
		return containerProjectReached(p, running)
	})
}

// DeleteContainerProject removes a project from Container Manager while
// preserving its project directory and compose file. Container lifecycle and
// named-volume data may still be affected by DSM, so callers must expose this
// operation only behind an explicit destructive opt-in.
func (c *Client) DeleteContainerProject(ctx context.Context, id string) error {
	c.containerProjectMu.Lock()
	defer c.containerProjectMu.Unlock()

	if _, err := c.GetContainerProject(ctx, id); errors.Is(err, ErrContainerProjectNotFound) {
		return nil
	} else if err != nil {
		return err
	}

	params := url.Values{}
	params.Set("id", id)
	params.Set("preserve_content", "true")
	if _, err := c.DoAPIPost(ctx, "SYNO.Docker.Project", "1", "delete", params); err != nil {
		return fmt.Errorf("delete Container Manager project %q: %w", id, err)
	}

	deadline := time.Now().Add(containerProjectTaskTimeout)
	for {
		_, err := c.GetContainerProject(ctx, id)
		if errors.Is(err, ErrContainerProjectNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("wait for Container Manager project %q deletion: %w", id, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Container Manager project %q was not deleted within %s", id, containerProjectTaskTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(containerProjectPollInterval):
		}
	}
}

func (c *Client) createContainerProjectFolder(ctx context.Context, sharePath string) error {
	clean := path.Clean(sharePath)
	parent, name := path.Split(clean)
	parent = strings.TrimSuffix(parent, "/")
	if parent == "" {
		parent = "/"
	}
	if name == "" || parent == "/" {
		return fmt.Errorf("project share path %q must name a directory inside a DSM shared folder", sharePath)
	}

	params := url.Values{}
	params.Set("folder_path", parent)
	params.Set("name", name)
	params.Set("force_parent", "true")
	if _, err := c.DoAPIPost(ctx, "SYNO.FileStation.CreateFolder", "2", "create", params); err != nil {
		return fmt.Errorf("create project directory %q: %w", clean, err)
	}
	return nil
}

func (c *Client) updateContainerProjectCompose(ctx context.Context, id, composeYAML string) error {
	params := url.Values{}
	params.Set("id", id)
	params.Set("content", composeYAML)

	_, err := c.DoAPIPost(WithSlowCall(ctx), "SYNO.Docker.Project", "1", "update", params)
	if err == nil {
		return nil
	}

	// A late response is not a failed write. DSM blocks this call while Container
	// Manager settles, and on a busy NAS the answer can arrive after the client
	// gave up — with the compose file already written. Treating that as an error
	// left a project on the NAS but not in state, and every later apply then hit
	// "already exists; import it instead" (issue #70).
	if IsTimeoutError(err) {
		if project, getErr := c.GetContainerProject(ctx, id); getErr == nil && project.ComposeYAML == composeYAML {
			return nil
		}
	}
	return fmt.Errorf("update compose content for Container Manager project %q: %w", id, err)
}

// maxProjectDiagnosticLines bounds how much streamed output is attached to an
// error. A failing build ends with the reason; the pull progress before it is
// noise in a Terraform diagnostic.
const maxProjectDiagnosticLines = 12

func (c *Client) runContainerProjectAction(ctx context.Context, id, action string) error {
	params := url.Values{}
	params.Set("id", id)
	if _, err := c.DoAPIPost(ctx, "SYNO.Docker.Project", "1", action, params); err != nil {
		// Older Container Manager builds expose lifecycle operations only as
		// newline-delimited streaming methods. Current builds support the direct
		// POST methods, so use those first and fall back only when DSM says the
		// method does not exist.
		if IsAPIError(err, 103) {
			output, streamErr := c.streamContainerProjectAction(ctx, id, action+"_stream")
			if streamErr == nil {
				return nil
			}
			return fmt.Errorf("%s Container Manager project %q: direct method: %v; streaming method: %w%s",
				action, id, err, streamErr, formatProjectDiagnostics(output))
		}

		// The direct methods answer a failed build with a bare code — 2202 for a
		// build that could not start its containers — while Container Manager's
		// own explanation is only available through the streaming variant. Ask for
		// it, so the diagnostic names the actual problem instead of a number.
		if detail := c.diagnoseContainerProjectAction(ctx, id, action); detail != "" {
			return fmt.Errorf("%s Container Manager project %q: %w%s", action, id, err, detail)
		}
		return fmt.Errorf("%s Container Manager project %q: %w", action, id, err)
	}
	return nil
}

// diagnoseContainerProjectAction re-runs a failed action through its streaming
// variant purely to collect DSM's explanation, and returns it preformatted for
// an error message (empty if nothing useful came back).
//
// This does repeat the action. For the failures it is meant to explain that is
// harmless — a build that failed on a missing bind mount fails the same way
// again, which is precisely what makes the output reproducible — and the cost is
// paid only on a path that is already an error. Actions with nothing to explain
// are left alone.
func (c *Client) diagnoseContainerProjectAction(ctx context.Context, id, action string) string {
	switch action {
	case "build", "start":
	default:
		return ""
	}

	output, _ := c.streamContainerProjectAction(ctx, id, action+"_stream")
	return formatProjectDiagnostics(output)
}

// formatProjectDiagnostics renders the tail of a streamed action for inclusion
// in an error message.
func formatProjectDiagnostics(output []string) string {
	if len(output) == 0 {
		return ""
	}
	if len(output) > maxProjectDiagnosticLines {
		output = output[len(output)-maxProjectDiagnosticLines:]
	}
	return "\n\nContainer Manager reported:\n  " + strings.Join(output, "\n  ")
}

func (c *Client) streamContainerProjectAction(ctx context.Context, id, method string) ([]string, error) {
	for attempt := range 2 {
		params := c.buildParams("SYNO.Docker.Project", "1", method, url.Values{"id": []string{jsonString(id)}})
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/webapi/entry.cgi?"+params.Encode(), nil)
		if err != nil {
			return nil, fmt.Errorf("create streaming request: %w", err)
		}

		streamClient := *c.httpClient
		streamClient.Timeout = containerProjectTaskTimeout
		response, err := streamClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("streaming HTTP request: %w", err)
		}
		output, streamErr := readContainerProjectActionStream(response)
		if closeErr := response.Body.Close(); streamErr == nil && closeErr != nil {
			streamErr = closeErr
		}
		if isSessionExpiredError(streamErr) && attempt == 0 {
			if err := c.relogin(ctx); err != nil {
				return nil, fmt.Errorf("re-login before streaming retry: %w", err)
			}
			continue
		}
		return output, streamErr
	}
	return nil, errors.New("streaming project action exhausted retries")
}

// readContainerProjectActionStream consumes a streamed lifecycle response and
// returns both the plain-text lines DSM emitted and the failure, if any.
//
// The text lines are the whole point of the streaming variant: they carry
// Container Manager's own diagnosis ("Bind mount failed: '/volume1/...' does not
// exist"), which the non-streaming methods reduce to a bare code such as 2202.
func readContainerProjectActionStream(response *http.Response) ([]string, error) {
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected streaming status %d", response.StatusCode)
	}

	var output []string

	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if line == "" {
			continue
		}
		var envelope struct {
			Success *bool     `json:"success"`
			Error   *APIError `json:"error,omitempty"`
		}
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			// Not an envelope at all: this is the build output itself, which is
			// exactly what the caller needs to explain a failure.
			output = append(output, line)
			continue
		}
		if envelope.Success == nil || *envelope.Success {
			continue
		}
		if envelope.Error != nil {
			envelope.Error.API = "SYNO.Docker.Project"
			return output, envelope.Error
		}
		return output, errors.New("streaming API returned success=false with no error details")
	}
	if err := scanner.Err(); err != nil {
		return output, fmt.Errorf("read streaming response: %w", err)
	}
	return output, nil
}

func (c *Client) waitForContainerProjectRunningState(ctx context.Context, id string, running bool) (*ContainerProject, error) {
	return c.waitForContainerProject(ctx, id, func(project *ContainerProject) bool {
		return containerProjectReached(project, running)
	})
}

// containerProjectReached decides whether a project has arrived at the
// requested running state. It is deliberately asymmetric about WARNING.
//
// WARNING means "some containers are running and some are not", which is a
// steady state in both directions: a project with a one-shot container sits
// there while up, and a project that was never started sits there too once that
// container has run and exited. Requiring WARNING to clear before a project
// counts as stopped therefore waits for something that never happens — the
// ten-minute timeout of issue #101, in the direction the build path does not
// cover.
//
// So only an outright RUNNING blocks "stopped". The cost is that stopping a
// project that stays in WARNING — because a container refused to die — is
// reported as success. That is the same trade #73 accepted in the other
// direction, and the status is in state either way for anyone who wants to
// assert on it.
func containerProjectReached(project *ContainerProject, running bool) bool {
	if isTransientContainerProjectStatus(project.Status) {
		return false
	}
	if running {
		return project.Running()
	}
	return !strings.EqualFold(project.Status, statusRunning)
}

// statusRunning is DSM's "every container is up" status.
const statusRunning = "RUNNING"

// waitForContainerProjectSettled waits for the status to stop being transient
// without judging whether the project is up.
//
// This is what a build must wait for. Waiting for "not running" there was a
// target the project can never reach once a one-shot container has run: the
// build leaves it in WARNING, WARNING counts as running (see Running), and so
// the wait burned the full ten-minute timeout and failed an apply whose
// services were fine — the exact failure #73 set out to fix, displaced one step
// earlier in the sequence (issue #101).
func (c *Client) waitForContainerProjectSettled(ctx context.Context, id string) (*ContainerProject, error) {
	return c.waitForContainerProject(ctx, id, func(project *ContainerProject) bool {
		return !isTransientContainerProjectStatus(project.Status)
	})
}

func (c *Client) waitForContainerProject(ctx context.Context, id string, done func(*ContainerProject) bool) (*ContainerProject, error) {
	deadline := time.Now().Add(containerProjectTaskTimeout)
	for {
		project, err := c.GetContainerProject(ctx, id)
		if err != nil {
			return nil, err
		}
		if isFailedContainerProjectStatus(project.Status) {
			return nil, fmt.Errorf("Container Manager project %q entered failure status %q", id, project.Status)
		}
		if done(project) {
			return project, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("Container Manager project %q did not reach the requested state within %s (last status: %q)", id, containerProjectTaskTimeout, project.Status)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(containerProjectPollInterval):
		}
	}
}

func (c *Client) getContainerProjectCompose(ctx context.Context, projectPath string) (string, error) {
	params := url.Values{}
	params.Set("path", jsonString(projectPath))
	data, err := c.DoAPIPost(ctx, "SYNO.Docker.Project", "1", "get_share_info", params)
	if err != nil {
		return "", fmt.Errorf("read compose content for project at %q: %w", projectPath, err)
	}
	var result struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("parse compose content for project at %q: %w", projectPath, err)
	}
	return result.Content, nil
}

func parseContainerProjectList(raw json.RawMessage) ([]ContainerProject, error) {
	var root interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}

	if object, ok := root.(map[string]interface{}); ok {
		for _, wrapper := range []string{"projects", "list"} {
			if nested, exists := object[wrapper]; exists {
				root = nested
				break
			}
		}
	}

	projects := make([]ContainerProject, 0)
	switch collection := root.(type) {
	case map[string]interface{}:
		for key, value := range collection {
			object, ok := value.(map[string]interface{})
			if !ok {
				continue
			}
			project := parseContainerProjectMap(object)
			if project.ID == "" {
				project.ID = key
			}
			if project.Name != "" {
				projects = append(projects, project)
			}
		}
	case []interface{}:
		for _, value := range collection {
			object, ok := value.(map[string]interface{})
			if !ok {
				continue
			}
			project := parseContainerProjectMap(object)
			if project.Name != "" {
				projects = append(projects, project)
			}
		}
	default:
		return nil, fmt.Errorf("unexpected project collection type %T", root)
	}
	return projects, nil
}

func parseContainerProject(raw json.RawMessage, fallbackID string) (*ContainerProject, error) {
	var object map[string]interface{}
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	for _, wrapper := range []string{"project", "data"} {
		if nested, ok := object[wrapper].(map[string]interface{}); ok {
			object = nested
			break
		}
	}
	if nested, ok := object[fallbackID].(map[string]interface{}); ok {
		object = nested
	} else if stringValue(object, "name") == "" && len(object) == 1 {
		for _, value := range object {
			if nested, ok := value.(map[string]interface{}); ok {
				object = nested
			}
		}
	}
	project := parseContainerProjectMap(object)
	if project.ID == "" {
		project.ID = fallbackID
	}
	return &project, nil
}

func parseContainerProjectMap(object map[string]interface{}) ContainerProject {
	status := stringValue(object, "status")
	if status == "" {
		status = stringValue(object, "state")
	}
	project := ContainerProject{
		ID:          stringValue(object, "id"),
		Name:        stringValue(object, "name"),
		SharePath:   stringValue(object, "share_path"),
		Path:        stringValue(object, "path"),
		ComposeYAML: stringValue(object, "content"),
		Status:      status,
	}
	for _, key := range []string{"container_ids", "containerIds"} {
		if values, ok := object[key].([]interface{}); ok {
			for _, value := range values {
				if id, ok := value.(string); ok && id != "" {
					project.ContainerIDs = append(project.ContainerIDs, id)
				}
			}
		}
	}
	if values, ok := object["containers"].([]interface{}); ok && len(project.ContainerIDs) == 0 {
		for _, value := range values {
			switch container := value.(type) {
			case string:
				project.ContainerIDs = append(project.ContainerIDs, container)
			case map[string]interface{}:
				if id := stringValue(container, "id"); id != "" {
					project.ContainerIDs = append(project.ContainerIDs, id)
				}
			}
		}
	}
	return project
}

func stringValue(object map[string]interface{}, key string) string {
	value, _ := object[key].(string)
	return value
}

func jsonString(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func isContainerProjectMissingError(err error) bool {
	return IsAPIError(err, 2104, 408)
}

func isTransientContainerProjectStatus(status string) bool {
	switch strings.ToUpper(status) {
	case "BUILDING", "CREATING", "STARTING", "STOPPING", "UPDATING", "DELETING":
		return true
	default:
		return false
	}
}

func isFailedContainerProjectStatus(status string) bool {
	switch strings.ToUpper(status) {
	case "ERROR", "FAILED", "BROKEN":
		return true
	default:
		return false
	}
}
