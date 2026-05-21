package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

type UserQuota struct {
	Username  string
	QuotaSize int64
	QuotaUsed int64
}

type SetUserQuotaRequest struct {
	ShareName string
	Username  string
	QuotaSize int64
}

func (c *Client) ListUserQuotas(ctx context.Context, shareName string) ([]UserQuota, error) {
	params := url.Values{}
	params.Set("name", shareName)
	params.Set("offset", "0")
	params.Set("limit", "-1")

	data, err := c.DoAPI(ctx, "SYNO.Core.Share.Quota", "1", "list", params)
	if err != nil {
		return nil, fmt.Errorf("list user quotas for %q: %w", shareName, err)
	}

	var result struct {
		Items []json.RawMessage `json:"items"`
		Total int               `json:"total"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse user quota list: %w", err)
	}

	quotas := make([]UserQuota, 0, len(result.Items))
	for _, raw := range result.Items {
		q, err := parseUserQuota(raw)
		if err != nil {
			continue
		}
		quotas = append(quotas, *q)
	}
	return quotas, nil
}

func (c *Client) GetUserQuota(ctx context.Context, shareName, username string) (*UserQuota, error) {
	quotas, err := c.ListUserQuotas(ctx, shareName)
	if err != nil {
		return nil, err
	}

	for i := range quotas {
		if quotas[i].Username == username {
			return &quotas[i], nil
		}
	}

	return nil, fmt.Errorf("quota not found for user %q on share %q", username, shareName)
}

func (c *Client) SetUserQuota(ctx context.Context, req SetUserQuotaRequest) (*UserQuota, error) {
	quotas, err := c.ListUserQuotas(ctx, req.ShareName)
	if err != nil {
		return nil, err
	}

	found := false
	for i := range quotas {
		if quotas[i].Username == req.Username {
			quotas[i].QuotaSize = req.QuotaSize
			found = true
			break
		}
	}
	if !found {
		quotas = append(quotas, UserQuota{
			Username:  req.Username,
			QuotaSize: req.QuotaSize,
		})
	}

	if err := c.setAllQuotas(ctx, req.ShareName, quotas); err != nil {
		return nil, fmt.Errorf("set quota for %q on %q: %w", req.Username, req.ShareName, err)
	}

	return c.GetUserQuota(ctx, req.ShareName, req.Username)
}

func (c *Client) DeleteUserQuota(ctx context.Context, shareName, username string) error {
	quotas, err := c.ListUserQuotas(ctx, shareName)
	if err != nil {
		return err
	}

	found := false
	for i := range quotas {
		if quotas[i].Username == username {
			quotas[i].QuotaSize = 0
			found = true
			break
		}
	}

	if !found {
		return nil
	}

	if err := c.setAllQuotas(ctx, shareName, quotas); err != nil {
		return fmt.Errorf("delete quota for %q on %q: %w", username, shareName, err)
	}
	return nil
}

func (c *Client) setAllQuotas(ctx context.Context, shareName string, quotas []UserQuota) error {
	payload := buildQuotaPayload(quotas)

	params := url.Values{}
	params.Set("name", shareName)
	params.Set("quotas", payload)

	_, err := c.DoAPI(ctx, "SYNO.Core.Share.Quota", "1", "set", params)
	return err
}

func buildQuotaPayload(quotas []UserQuota) string {
	items := make([]map[string]interface{}, len(quotas))
	for i, q := range quotas {
		items[i] = map[string]interface{}{
			"username":   q.Username,
			"quota_size": q.QuotaSize,
		}
	}
	raw, _ := json.Marshal(items)
	return string(raw)
}

func parseUserQuota(raw json.RawMessage) (*UserQuota, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}

	q := &UserQuota{}

	if v, ok := m["username"].(string); ok {
		q.Username = v
	} else if v, ok := m["name"].(string); ok {
		q.Username = v
	}
	if v, ok := m["quota_size"].(float64); ok {
		q.QuotaSize = int64(v)
	}
	if v, ok := m["quota_used"].(float64); ok {
		q.QuotaUsed = int64(v)
	}

	return q, nil
}

func BuildUserQuotaID(shareName, username string) string {
	return fmt.Sprintf("%s:%s", shareName, username)
}

func ParseUserQuotaID(id string) (shareName, username string, err error) {
	parts := strings.SplitN(id, ":", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid user quota ID %q: expected format share_name:username", id)
	}
	return parts[0], parts[1], nil
}
