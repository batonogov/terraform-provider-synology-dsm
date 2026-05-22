package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

type Share struct {
	Name             string
	Description      string
	VolPath          string
	UUID             string
	Hidden           bool
	EnableRecycleBin bool
}

type CreateShareRequest struct {
	Name             string
	VolPath          string
	Description      string
	Hidden           bool
	EnableRecycleBin bool
}

func buildShareInfo(req CreateShareRequest, nameOrg string) string {
	m := map[string]interface{}{
		"name":                   req.Name,
		"vol_path":               req.VolPath,
		"desc":                   req.Description,
		"hidden":                 req.Hidden,
		"enable_recycle_bin":     req.EnableRecycleBin,
		"recycle_bin_admin_only": true,
		"hide_unreadable":        false,
		"enable_share_compress":  false,
		"enable_share_cow":       false,
		"share_quota":            0,
	}
	if nameOrg != "" {
		m["name_org"] = nameOrg
	}
	raw, _ := json.Marshal(m)
	return string(raw)
}

func (c *Client) CreateShare(ctx context.Context, req CreateShareRequest) (*Share, error) {
	params := url.Values{}
	params.Set("name", req.Name)
	params.Set("shareinfo", buildShareInfo(req, ""))

	_, err := c.DoAPIPost(ctx, "SYNO.Core.Share", "1", "create", params)
	if err != nil {
		return nil, fmt.Errorf("create share %q: %w", req.Name, err)
	}

	return c.GetShare(ctx, req.Name)
}

func (c *Client) GetShare(ctx context.Context, name string) (*Share, error) {
	additional, _ := json.Marshal([]string{"hidden", "recyclebin"})

	params := url.Values{}
	params.Set("name", name)
	params.Set("additional", string(additional))

	data, err := c.DoAPI(ctx, "SYNO.Core.Share", "1", "get", params)
	if err != nil {
		return nil, fmt.Errorf("get share %q: %w", name, err)
	}

	return parseShare(data)
}

func (c *Client) ListShares(ctx context.Context) ([]Share, error) {
	params := url.Values{}
	params.Set("shareType", "all")
	params.Set("additional", "[]")

	data, err := c.DoAPI(ctx, "SYNO.Core.Share", "1", "list", params)
	if err != nil {
		return nil, fmt.Errorf("list shares: %w", err)
	}

	var result struct {
		Shares []json.RawMessage `json:"shares"`
		Total  int               `json:"total"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse share list: %w", err)
	}

	shares := make([]Share, 0, len(result.Shares))
	for _, raw := range result.Shares {
		s, err := parseShare(raw)
		if err != nil {
			continue
		}
		shares = append(shares, *s)
	}
	return shares, nil
}

func (c *Client) UpdateShare(ctx context.Context, name string, req CreateShareRequest) (*Share, error) {
	params := url.Values{}
	params.Set("name", req.Name)
	params.Set("shareinfo", buildShareInfo(req, name))

	_, err := c.DoAPIPost(ctx, "SYNO.Core.Share", "1", "create", params)
	if err != nil {
		return nil, fmt.Errorf("update share %q: %w", name, err)
	}

	return c.GetShare(ctx, req.Name)
}

func (c *Client) DeleteShare(ctx context.Context, name string) error {
	namesJSON, _ := json.Marshal([]string{name})

	params := url.Values{}
	params.Set("name", string(namesJSON))

	_, err := c.DoAPI(ctx, "SYNO.Core.Share", "1", "delete", params)
	if err != nil {
		return fmt.Errorf("delete share %q: %w", name, err)
	}
	return nil
}

func parseShare(raw json.RawMessage) (*Share, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}

	s := &Share{}

	if v, ok := m["name"].(string); ok {
		s.Name = v
	}
	if v, ok := m["desc"].(string); ok {
		s.Description = v
	}
	if v, ok := m["vol_path"].(string); ok {
		s.VolPath = v
	}
	if v, ok := m["uuid"].(string); ok {
		s.UUID = v
	}
	if v, ok := m["hidden"].(bool); ok {
		s.Hidden = v
	}
	if v, ok := m["enable_recycle_bin"].(bool); ok {
		s.EnableRecycleBin = v
	} else if v, ok := m["recyclebin"].(bool); ok {
		s.EnableRecycleBin = v
	}

	return s, nil
}
