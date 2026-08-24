package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

type Group struct {
	Name        string
	Description string
	GID         int
}

type CreateGroupRequest struct {
	Name        string
	Description string
}

type UpdateGroupRequest struct {
	NewName     string
	Description string
}

func (c *Client) CreateGroup(ctx context.Context, req CreateGroupRequest) (*Group, error) {
	params := url.Values{}
	params.Set("name", req.Name)

	if req.Description != "" {
		params.Set("description", req.Description)
	}

	_, err := c.DoAPI(ctx, "SYNO.Core.Group", "1", "create", params)
	if err != nil {
		return nil, fmt.Errorf("create group %q: %w", req.Name, err)
	}

	return c.GetGroup(ctx, req.Name)
}

func (c *Client) GetGroup(ctx context.Context, name string) (*Group, error) {
	params := url.Values{}
	params.Set("name", name)

	data, err := c.DoAPI(ctx, "SYNO.Core.Group", "1", "get", params)
	if err != nil {
		if absenceConfirmedBy(ctx, err, func(ctx context.Context) (bool, error) {
			return c.groupExists(ctx, name)
		}) {
			return nil, &NotFoundError{Kind: "group", Name: name}
		}
		return nil, fmt.Errorf("get group %q: %w", name, err)
	}

	var result struct {
		Groups []json.RawMessage `json:"groups"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse group get response: %w", err)
	}

	for _, raw := range result.Groups {
		g, err := parseGroup(raw)
		if err != nil {
			continue
		}
		if g.Name == name {
			return g, nil
		}
	}

	// DSM answered with a well-formed payload that does not contain the group:
	// established absence.
	return nil, &NotFoundError{Kind: "group", Name: name}
}

func (c *Client) ListGroups(ctx context.Context) ([]Group, error) {
	params := url.Values{}
	params.Set("offset", "0")
	params.Set("limit", "-1")

	data, err := c.DoAPI(ctx, "SYNO.Core.Group", "1", "list", params)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}

	var result struct {
		Groups []json.RawMessage `json:"groups"`
		Total  int               `json:"total"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse group list: %w", err)
	}

	groups := make([]Group, 0, len(result.Groups))
	for _, raw := range result.Groups {
		g, err := parseGroup(raw)
		if err != nil {
			continue
		}
		groups = append(groups, *g)
	}
	return groups, nil
}

func (c *Client) UpdateGroup(ctx context.Context, name string, req UpdateGroupRequest) (*Group, error) {
	params := url.Values{}
	params.Set("name", name)

	if req.NewName != "" {
		params.Set("new_name", req.NewName)
	}
	params.Set("description", req.Description)

	_, err := c.DoAPI(ctx, "SYNO.Core.Group", "1", "set", params)
	if err != nil {
		return nil, fmt.Errorf("update group %q: %w", name, err)
	}

	updatedName := name
	if req.NewName != "" {
		updatedName = req.NewName
	}
	return c.GetGroup(ctx, updatedName)
}

func (c *Client) DeleteGroup(ctx context.Context, name string) error {
	namesJSON, _ := json.Marshal([]string{name})

	params := url.Values{}
	params.Set("name", string(namesJSON))

	_, err := c.DoAPI(ctx, "SYNO.Core.Group", "1", "delete", params)
	if err != nil {
		return fmt.Errorf("delete group %q: %w", name, err)
	}
	return nil
}

func parseGroup(raw json.RawMessage) (*Group, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}

	g := &Group{}

	if v, ok := m["name"].(string); ok {
		g.Name = v
	}
	if v, ok := m["description"].(string); ok {
		g.Description = v
	}
	if gid, ok := m["gid"].(float64); ok {
		g.GID = int(gid)
	}

	return g, nil
}

// groupExists answers the same question as GetGroup through `list`, which is
// what makes a failed `get` decidable: DSM has no documented code for a group
// that is not there.
func (c *Client) groupExists(ctx context.Context, name string) (bool, error) {
	groups, err := c.ListGroups(ctx)
	if err != nil {
		return false, err
	}
	for i := range groups {
		if groups[i].Name == name {
			return true, nil
		}
	}
	return false, nil
}
