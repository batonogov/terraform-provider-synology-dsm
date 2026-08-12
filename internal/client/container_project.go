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

var ErrContainerProjectNotFound = errors.New("Container Manager project not found")

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

func (p ContainerProject) Running() bool {
	return strings.EqualFold(p.Status, "running")
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
	if _, err := c.DoAPIPost(ctx, "SYNO.Docker.Project", "1", "create", params); err != nil {
		return nil, fmt.Errorf("create Container Manager project %q: %w", name, err)
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
	if _, err := c.waitForContainerProjectRunningState(ctx, project.ID, false); err != nil {
		return nil, err
	}
	if running {
		if err := c.runContainerProjectAction(ctx, project.ID, "start"); err != nil {
			return nil, err
		}
	}

	return c.waitForContainerProject(ctx, project.ID, func(p *ContainerProject) bool {
		return p.Running() == running && !isTransientContainerProjectStatus(p.Status)
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
		if _, err := c.waitForContainerProjectRunningState(ctx, id, false); err != nil {
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
		return p.Running() == running && !isTransientContainerProjectStatus(p.Status)
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
	if _, err := c.DoAPIPost(ctx, "SYNO.Docker.Project", "1", "update", params); err != nil {
		return fmt.Errorf("update compose content for Container Manager project %q: %w", id, err)
	}
	return nil
}

func (c *Client) runContainerProjectAction(ctx context.Context, id, action string) error {
	params := url.Values{}
	params.Set("id", id)
	if _, err := c.DoAPIPost(ctx, "SYNO.Docker.Project", "1", action, params); err != nil {
		// Older Container Manager builds expose lifecycle operations only as
		// newline-delimited streaming methods. Current builds support the direct
		// POST methods, so use those first and fall back only when DSM says the
		// method does not exist.
		if IsAPIError(err, 103) {
			if streamErr := c.streamContainerProjectAction(ctx, id, action+"_stream"); streamErr == nil {
				return nil
			} else {
				return fmt.Errorf("%s Container Manager project %q: direct method: %v; streaming method: %w", action, id, err, streamErr)
			}
		}
		return fmt.Errorf("%s Container Manager project %q: %w", action, id, err)
	}
	return nil
}

func (c *Client) streamContainerProjectAction(ctx context.Context, id, method string) error {
	for attempt := range 2 {
		params := c.buildParams("SYNO.Docker.Project", "1", method, url.Values{"id": []string{jsonString(id)}})
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/webapi/entry.cgi?"+params.Encode(), nil)
		if err != nil {
			return fmt.Errorf("create streaming request: %w", err)
		}

		streamClient := *c.httpClient
		streamClient.Timeout = containerProjectTaskTimeout
		response, err := streamClient.Do(req)
		if err != nil {
			return fmt.Errorf("streaming HTTP request: %w", err)
		}
		streamErr := readContainerProjectActionStream(response)
		if closeErr := response.Body.Close(); streamErr == nil && closeErr != nil {
			streamErr = closeErr
		}
		if isSessionExpiredError(streamErr) && attempt == 0 {
			if err := c.relogin(ctx); err != nil {
				return fmt.Errorf("re-login before streaming retry: %w", err)
			}
			continue
		}
		return streamErr
	}
	return errors.New("streaming project action exhausted retries")
}

func readContainerProjectActionStream(response *http.Response) error {
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected streaming status %d", response.StatusCode)
	}

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
		if err := json.Unmarshal([]byte(line), &envelope); err != nil || envelope.Success == nil || *envelope.Success {
			continue
		}
		if envelope.Error != nil {
			envelope.Error.API = "SYNO.Docker.Project"
			return envelope.Error
		}
		return errors.New("streaming API returned success=false with no error details")
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read streaming response: %w", err)
	}
	return nil
}

func (c *Client) waitForContainerProjectRunningState(ctx context.Context, id string, running bool) (*ContainerProject, error) {
	return c.waitForContainerProject(ctx, id, func(project *ContainerProject) bool {
		return project.Running() == running && !isTransientContainerProjectStatus(project.Status)
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
