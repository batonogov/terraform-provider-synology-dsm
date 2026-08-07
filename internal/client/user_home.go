package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"
)

// Poll cadence for the asynchronous form of SYNO.Core.User.Home set. Variables
// rather than constants so tests can shrink them without waiting real seconds.
var (
	userHomeTaskPollInterval = 3 * time.Second
	userHomeTaskTimeout      = 5 * time.Minute
)

// UserHomeService is the global DSM user home service: it controls whether
// per-user home folders (/volume1/homes/<username>) exist at all.
type UserHomeService struct {
	Enable              bool
	Location            string
	EnableRecycleBin    bool
	EnableDomain        bool
	EnableLDAP          bool
	Encryption          int64
	PersonalPhotoEnable bool
}

type SetUserHomeServiceRequest struct {
	Enable           bool
	Location         string
	EnableRecycleBin bool
	Force            bool
}

func (c *Client) GetUserHomeService(ctx context.Context) (*UserHomeService, error) {
	additional, _ := json.Marshal([]string{"personal_photo_enable"})

	params := url.Values{}
	params.Set("additional", string(additional))

	data, err := c.DoAPI(ctx, "SYNO.Core.User.Home", "1", "get", params)
	if err != nil {
		return nil, fmt.Errorf("get user home service: %w", err)
	}

	return parseUserHomeService(data)
}

// SetUserHomeService enables or disables the user home service. On virtual DSM
// the call completes synchronously, but a physical NAS may return a task_id
// (creating the homes subvolume is a background job) — in that case we poll
// status until the task reports finish.
func (c *Client) SetUserHomeService(ctx context.Context, req SetUserHomeServiceRequest) (*UserHomeService, error) {
	params := url.Values{}
	params.Set("enable", boolParam(req.Enable))

	// location is required when enabling and must be a volume PATH ("/volume1").
	// A bare "volume1" is rejected with error 3101. When disabling, DSM keeps the
	// previously configured location, so we omit it: passing extra fields to a
	// disable call has been observed to trigger 3103.
	if req.Enable {
		// An empty location is left out entirely rather than sent as "": DSM then
		// answers 3103 ("location missing"), which names the actual problem,
		// instead of 3101 ("bad location format").
		if req.Location != "" {
			params.Set("location", req.Location)
		}
		params.Set("enable_recycle_bin", boolParam(req.EnableRecycleBin))
		if req.Force {
			params.Set("force", boolParam(true))
		}
	}

	data, err := c.DoAPIPost(ctx, "SYNO.Core.User.Home", "1", "set", params)
	if err != nil {
		return nil, fmt.Errorf("set user home service: %w", err)
	}

	if taskID := parseUserHomeTaskID(data); taskID != "" {
		if err := c.waitUserHomeTask(ctx, taskID); err != nil {
			return nil, err
		}
	}

	return c.GetUserHomeService(ctx)
}

// waitUserHomeTask polls SYNO.Core.User.Home status until the task finishes,
// the context is cancelled, or userHomeTaskTimeout elapses.
func (c *Client) waitUserHomeTask(ctx context.Context, taskID string) error {
	deadline := time.Now().Add(userHomeTaskTimeout)

	for {
		params := url.Values{}
		params.Set("task_id", taskID)

		data, err := c.DoAPI(ctx, "SYNO.Core.User.Home", "1", "status", params)
		if err != nil {
			return fmt.Errorf("poll user home task %q: %w", taskID, err)
		}

		var result struct {
			Finish bool `json:"finish"`
		}
		if err := json.Unmarshal(data, &result); err != nil {
			return fmt.Errorf("parse user home task status: %w", err)
		}
		if result.Finish {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("user home task %q did not finish within %s", taskID, userHomeTaskTimeout)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(userHomeTaskPollInterval):
		}
	}
}

func parseUserHomeTaskID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var result struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return ""
	}
	return result.TaskID
}

func parseUserHomeService(raw json.RawMessage) (*UserHomeService, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}

	s := &UserHomeService{}

	if v, ok := m["enable"].(bool); ok {
		s.Enable = v
	}
	if v, ok := m["location"].(string); ok {
		s.Location = v
	}
	if v, ok := m["enable_recycle_bin"].(bool); ok {
		s.EnableRecycleBin = v
	}
	if v, ok := m["enable_domain"].(bool); ok {
		s.EnableDomain = v
	}
	if v, ok := m["enable_ldap"].(bool); ok {
		s.EnableLDAP = v
	}
	if v, ok := m["encryption"].(float64); ok {
		s.Encryption = int64(v)
	}
	if additional, ok := m["additional"].(map[string]interface{}); ok {
		if v, ok := additional["personal_photo_enable"].(bool); ok {
			s.PersonalPhotoEnable = v
		}
	}

	return s, nil
}
