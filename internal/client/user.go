package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

type User struct {
	Name        string
	Description string
	Email       string
	Disabled    bool
	Groups      []string
	UID         int
}

type CreateUserRequest struct {
	Name        string
	Password    string
	Description string
	Email       string
	Disabled    bool
	Groups      []string
}

type UpdateUserRequest struct {
	Name        string
	NewName     string
	Password    string
	Description string
	Email       string
	Disabled    *bool
	Groups      []string
}

func (c *Client) CreateUser(ctx context.Context, req CreateUserRequest) (*User, error) {
	params := url.Values{}
	params.Set("name", req.Name)
	params.Set("password", req.Password)

	if req.Description != "" {
		params.Set("description", req.Description)
	}
	if req.Email != "" {
		params.Set("email", req.Email)
	}
	params.Set("disabled", boolParam(req.Disabled))

	if len(req.Groups) > 0 {
		groupsJSON, _ := json.Marshal(req.Groups)
		params.Set("groups", string(groupsJSON))
	}

	_, err := c.DoAPI(ctx, "SYNO.Core.User", "1", "create", params)
	if err != nil {
		return nil, fmt.Errorf("create user %q: %w", req.Name, err)
	}

	return c.GetUser(ctx, req.Name)
}

func (c *Client) GetUser(ctx context.Context, name string) (*User, error) {
	params := url.Values{}
	params.Set("name", name)

	data, err := c.DoAPI(ctx, "SYNO.Core.User", "1", "get", params)
	if err != nil {
		return nil, fmt.Errorf("get user %q: %w", name, err)
	}

	var result struct {
		Users []json.RawMessage `json:"users"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse user get response: %w", err)
	}

	for _, raw := range result.Users {
		u, err := parseUser(raw)
		if err != nil {
			continue
		}
		if u.Name == name {
			return u, nil
		}
	}

	return nil, fmt.Errorf("user %q not found", name)
}

func (c *Client) ListUsers(ctx context.Context) ([]User, error) {
	params := url.Values{}
	params.Set("offset", "0")
	params.Set("limit", "-1")

	data, err := c.DoAPI(ctx, "SYNO.Core.User", "1", "list", params)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}

	var result struct {
		Users []json.RawMessage `json:"users"`
		Total int               `json:"total"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse user list: %w", err)
	}

	users := make([]User, 0, len(result.Users))
	for _, raw := range result.Users {
		u, err := parseUser(raw)
		if err != nil {
			continue
		}
		users = append(users, *u)
	}
	return users, nil
}

func (c *Client) UpdateUser(ctx context.Context, username string, req UpdateUserRequest) (*User, error) {
	params := url.Values{}
	params.Set("name", username)

	if req.NewName != "" {
		params.Set("new_name", req.NewName)
	}
	if req.Password != "" {
		params.Set("password", req.Password)
	}
	if req.Description != "" {
		params.Set("description", req.Description)
	}
	if req.Email != "" {
		params.Set("email", req.Email)
	}
	if req.Disabled != nil {
		params.Set("disabled", boolParam(*req.Disabled))
	}
	if len(req.Groups) > 0 {
		groupsJSON, _ := json.Marshal(req.Groups)
		params.Set("groups", string(groupsJSON))
	}

	_, err := c.DoAPI(ctx, "SYNO.Core.User", "1", "update", params)
	if err != nil {
		return nil, fmt.Errorf("update user %q: %w", username, err)
	}

	name := username
	if req.NewName != "" {
		name = req.NewName
	}
	return c.GetUser(ctx, name)
}

func (c *Client) DeleteUser(ctx context.Context, name string) error {
	params := url.Values{}
	params.Set("name", name)

	_, err := c.DoAPI(ctx, "SYNO.Core.User", "1", "delete", params)
	if err != nil {
		return fmt.Errorf("delete user %q: %w", name, err)
	}
	return nil
}

func parseUser(raw json.RawMessage) (*User, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}

	u := &User{}

	if v, ok := m["name"].(string); ok {
		u.Name = v
	}
	if v, ok := m["user_name"].(string); ok {
		u.Name = v
	}
	if v, ok := m["description"].(string); ok {
		u.Description = v
	}
	if v, ok := m["email"].(string); ok {
		u.Email = v
	}
	if v, ok := m["disabled"].(bool); ok {
		u.Disabled = v
	}
	if uid, ok := m["uid"].(json.Number); ok {
		n, _ := uid.Int64()
		u.UID = int(n)
	}
	if uid, ok := m["uid"].(float64); ok {
		u.UID = int(uid)
	}
	if groups, ok := m["groups"].([]interface{}); ok {
		for _, g := range groups {
			if s, ok := g.(string); ok {
				u.Groups = append(u.Groups, s)
			}
		}
	}

	return u, nil
}

// UserIDByName finds a user's UID by name.
func (c *Client) UserIDByName(ctx context.Context, name string) (int, error) {
	u, err := c.GetUser(ctx, name)
	if err != nil {
		return 0, err
	}
	return u.UID, nil
}
