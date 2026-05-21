package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

type SharePermission struct {
	Name       string
	IsReadonly bool
	IsWritable bool
	IsDeny     bool
	Inherit    string
}

type SetSharePermissionRequest struct {
	ShareName     string
	UserGroupType string
	PrincipalName string
	Permission    string
}

func (c *Client) ListSharePermissions(ctx context.Context, shareName, userGroupType string) ([]SharePermission, error) {
	params := url.Values{}
	params.Set("name", shareName)
	params.Set("offset", "0")
	params.Set("limit", "-1")
	params.Set("action", "enum")
	params.Set("user_group_type", userGroupType)

	data, err := c.DoAPI(ctx, "SYNO.Core.Share.Permission", "1", "list", params)
	if err != nil {
		return nil, fmt.Errorf("list share permissions for %q: %w", shareName, err)
	}

	var result struct {
		Items []json.RawMessage `json:"items"`
		Total int               `json:"total"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse share permission list: %w", err)
	}

	perms := make([]SharePermission, 0, len(result.Items))
	for _, raw := range result.Items {
		p, err := parseSharePermission(raw)
		if err != nil {
			continue
		}
		perms = append(perms, *p)
	}
	return perms, nil
}

func (c *Client) GetSharePermission(ctx context.Context, shareName, userGroupType, principalName string) (*SharePermission, error) {
	perms, err := c.ListSharePermissions(ctx, shareName, userGroupType)
	if err != nil {
		return nil, err
	}

	for i := range perms {
		if perms[i].Name == principalName {
			return &perms[i], nil
		}
	}

	return nil, fmt.Errorf("permission not found for %s %q on share %q", userGroupType, principalName, shareName)
}

func (c *Client) SetSharePermission(ctx context.Context, req SetSharePermissionRequest) (*SharePermission, error) {
	perms, err := c.ListSharePermissions(ctx, req.ShareName, req.UserGroupType)
	if err != nil {
		return nil, err
	}

	found := false
	for i := range perms {
		if perms[i].Name == req.PrincipalName {
			applyPermissionMap(&perms[i], req.Permission)
			found = true
			break
		}
	}
	if !found {
		p := SharePermission{Name: req.PrincipalName}
		applyPermissionMap(&p, req.Permission)
		perms = append(perms, p)
	}

	if err := c.setAllPermissions(ctx, req.ShareName, req.UserGroupType, perms); err != nil {
		return nil, fmt.Errorf("set share permission for %q on %q: %w", req.PrincipalName, req.ShareName, err)
	}

	return c.GetSharePermission(ctx, req.ShareName, req.UserGroupType, req.PrincipalName)
}

func (c *Client) DeleteSharePermission(ctx context.Context, shareName, userGroupType, principalName string) error {
	perms, err := c.ListSharePermissions(ctx, shareName, userGroupType)
	if err != nil {
		return err
	}

	filtered := make([]SharePermission, 0, len(perms))
	for _, p := range perms {
		if p.Name != principalName {
			filtered = append(filtered, p)
		}
	}

	if err := c.setAllPermissions(ctx, shareName, userGroupType, filtered); err != nil {
		return fmt.Errorf("delete share permission for %q on %q: %w", principalName, shareName, err)
	}
	return nil
}

func (c *Client) setAllPermissions(ctx context.Context, shareName, userGroupType string, perms []SharePermission) error {
	payload := buildPermissionPayload(perms)

	params := url.Values{}
	params.Set("name", shareName)
	params.Set("user_group_type", userGroupType)
	params.Set("permissions", payload)

	_, err := c.DoAPI(ctx, "SYNO.Core.Share.Permission", "1", "set", params)
	return err
}

func applyPermissionMap(p *SharePermission, permission string) {
	p.IsReadonly = false
	p.IsWritable = false
	p.IsDeny = false

	switch permission {
	case "read_only":
		p.IsReadonly = true
	case "read_write":
		p.IsWritable = true
	case "no_access":
		p.IsDeny = true
	}
}

func PermissionFromFlags(p SharePermission) string {
	switch {
	case p.IsDeny:
		return "no_access"
	case p.IsReadonly:
		return "read_only"
	case p.IsWritable:
		return "read_write"
	default:
		return "no_access"
	}
}

func buildPermissionPayload(perms []SharePermission) string {
	items := make([]map[string]interface{}, len(perms))
	for i, p := range perms {
		items[i] = map[string]interface{}{
			"name":        p.Name,
			"is_readonly": p.IsReadonly,
			"is_writable": p.IsWritable,
			"is_deny":     p.IsDeny,
		}
	}
	raw, _ := json.Marshal(items)
	return string(raw)
}

func parseSharePermission(raw json.RawMessage) (*SharePermission, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}

	p := &SharePermission{}

	if v, ok := m["name"].(string); ok {
		p.Name = v
	}
	if v, ok := m["is_readonly"].(bool); ok {
		p.IsReadonly = v
	}
	if v, ok := m["is_writable"].(bool); ok {
		p.IsWritable = v
	}
	if v, ok := m["is_deny"].(bool); ok {
		p.IsDeny = v
	}
	if v, ok := m["inherit"].(string); ok {
		p.Inherit = v
	}

	return p, nil
}

func ParseSharePermissionID(id string) (shareName, userGroupType, principalName string, err error) {
	parts := strings.SplitN(id, ":", 3)
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("invalid share permission ID %q: expected format share_name:user_group_type:principal_name", id)
	}
	return parts[0], parts[1], parts[2], nil
}

func BuildSharePermissionID(shareName, userGroupType, principalName string) string {
	return fmt.Sprintf("%s:%s:%s", shareName, userGroupType, principalName)
}
