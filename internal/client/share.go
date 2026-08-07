package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

type Share struct {
	Name                string
	Description         string
	VolPath             string
	UUID                string
	Hidden              bool
	EnableRecycleBin    bool
	RecycleBinAdminOnly bool
	EnableShareCompress bool
	EnableShareCow      bool
	ShareQuota          int64
}

type CreateShareRequest struct {
	Name                string
	VolPath             string
	Description         string
	Hidden              bool
	EnableRecycleBin    bool
	RecycleBinAdminOnly bool
	EnableShareCompress bool
	EnableShareCow      bool
	ShareQuota          int64
}

func buildShareInfo(req CreateShareRequest) string {
	m := map[string]interface{}{
		"name":                   req.Name,
		"vol_path":               req.VolPath,
		"desc":                   req.Description,
		"hidden":                 req.Hidden,
		"enable_recycle_bin":     req.EnableRecycleBin,
		"recycle_bin_admin_only": req.RecycleBinAdminOnly,
		"enable_share_compress":  req.EnableShareCompress,
		"enable_share_cow":       req.EnableShareCow,
		// Written as share_quota, read back as quota_value. Unit is GB.
		"share_quota": req.ShareQuota,
	}
	raw, _ := json.Marshal(m)
	return string(raw)
}

func (c *Client) CreateShare(ctx context.Context, req CreateShareRequest) (*Share, error) {
	params := url.Values{}
	params.Set("name", req.Name)
	params.Set("shareinfo", buildShareInfo(req))

	_, err := c.DoAPIPost(ctx, "SYNO.Core.Share", "1", "create", params)
	if err != nil {
		return nil, fmt.Errorf("create share %q: %w", req.Name, err)
	}

	return c.GetShare(ctx, req.Name)
}

// shareAdditionalFields are the extra attributes DSM only returns when asked
// for explicitly. "recyclebin" is DSM's own spelling for the recycle bin flag,
// which comes back as enable_recycle_bin.
var shareAdditionalFields = []string{
	"hidden",
	"recyclebin",
	"enable_share_compress",
	"enable_share_cow",
	"share_quota",
}

func (c *Client) GetShare(ctx context.Context, name string) (*Share, error) {
	additional, _ := json.Marshal(shareAdditionalFields)

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

// UpdateShare applies new settings to an existing share.
//
// It uses the "set" method, not "create" with name_org: DSM rejects the latter
// with error 3301 ("share already exists"), so an update built that way never
// took effect.
func (c *Client) UpdateShare(ctx context.Context, name string, req CreateShareRequest) (*Share, error) {
	params := url.Values{}
	params.Set("name", name)
	params.Set("shareinfo", buildShareInfo(req))

	_, err := c.DoAPIPost(ctx, "SYNO.Core.Share", "1", "set", params)
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
	if v, ok := m["recycle_bin_admin_only"].(bool); ok {
		s.RecycleBinAdminOnly = v
	}
	if v, ok := m["enable_share_compress"].(bool); ok {
		s.EnableShareCompress = v
	}
	if v, ok := m["enable_share_cow"].(bool); ok {
		s.EnableShareCow = v
	}
	// The quota is written as share_quota but read back as quota_value.
	if v, ok := m["quota_value"].(float64); ok {
		s.ShareQuota = int64(v)
	} else if v, ok := m["share_quota"].(float64); ok {
		s.ShareQuota = int64(v)
	}

	return s, nil
}
