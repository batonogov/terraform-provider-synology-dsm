package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"
)

type User struct {
	Name        string
	Description string
	Email       string
	Disabled    bool
	ExpireDate  string
	TwoFactor   bool
	Groups      []string
	UID         int
}

type CreateUserRequest struct {
	Name        string
	Password    string
	Description string
	Email       string
	Disabled    bool
	ExpireDate  string
	Groups      []string
}

type UpdateUserRequest struct {
	Name        string
	NewName     string
	Password    string
	Description string
	Email       string
	Disabled    *bool
	ExpireDate  *string
	Groups      []string
}

// DSM account state lives in a single "expired" field rather than a boolean.
const (
	userExpiredNormal = "normal"
	userExpiredNow    = "now"
)

// expiredParam renders the account state DSM expects. disabled wins over an
// expiry date: an account switched off should stay off regardless of any date.
func expiredParam(disabled bool, expireDate string) string {
	switch {
	case disabled:
		return userExpiredNow
	case expireDate != "":
		return toDSMDate(expireDate)
	default:
		return userExpiredNormal
	}
}

// toDSMDate converts YYYY-MM-DD to the YYYY/M/D form DSM accepts. An ISO date
// with dashes is rejected outright with error 3103. Anything unparseable is
// passed through so DSM reports the problem rather than the provider guessing.
func toDSMDate(iso string) string {
	t, err := time.Parse("2006-01-02", iso)
	if err != nil {
		return iso
	}
	return fmt.Sprintf("%d/%d/%d", t.Year(), int(t.Month()), t.Day())
}

// fromDSMDate converts DSM's YYYY/M/D back to YYYY-MM-DD. DSM always answers
// without leading zeros no matter how the value was written, so normalising
// here is what keeps a configured "2027-03-05" from drifting against the
// "2027/3/5" that comes back.
func fromDSMDate(dsm string) string {
	t, err := time.Parse("2006/1/2", dsm)
	if err != nil {
		return dsm
	}
	return t.Format("2006-01-02")
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
	// "disabled" is silently ignored by DSM 7 — account state travels in
	// "expired". See .pi/recon-user-fields-2026-08-07.md.
	params.Set("expired", expiredParam(req.Disabled, req.ExpireDate))

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

// userAdditionalFields are the attributes DSM only returns when asked for.
// "expired" carries the account state (normal / now / an expiry date) and
// "2fa_status" whether two-factor auth is on.
const userAdditionalFields = `["description","email","expired","groups","2fa_status"]`

func (c *Client) GetUser(ctx context.Context, name string) (*User, error) {
	params := url.Values{}
	params.Set("offset", "0")
	params.Set("limit", "-1")
	params.Set("additional", userAdditionalFields)

	data, err := c.DoAPI(ctx, "SYNO.Core.User", "1", "list", params)
	if err != nil {
		return nil, fmt.Errorf("get user %q: %w", name, err)
	}

	var result struct {
		Users []json.RawMessage `json:"users"`
		Total int               `json:"total"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse user list response: %w", err)
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
	if req.Disabled != nil || req.ExpireDate != nil {
		disabled := req.Disabled != nil && *req.Disabled
		expireDate := ""
		if req.ExpireDate != nil {
			expireDate = *req.ExpireDate
		}
		params.Set("expired", expiredParam(disabled, expireDate))
	}
	if len(req.Groups) > 0 {
		groupsJSON, _ := json.Marshal(req.Groups)
		params.Set("groups", string(groupsJSON))
	}

	// The method is "set". SYNO.Core.User has no "update" method at all — it
	// answers 103 ("method not exist"), so updates built that way never applied.
	_, err := c.DoAPI(ctx, "SYNO.Core.User", "1", "set", params)
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
	// DSM 7 no longer returns "disabled"; account state comes from "expired",
	// which is either "normal", "now", or an expiry date.
	if v, ok := m["disabled"].(bool); ok {
		u.Disabled = v
	}
	if v, ok := m["expired"].(string); ok {
		switch v {
		case userExpiredNow:
			u.Disabled = true
		case userExpiredNormal, "":
			// Active with no expiry date.
		default:
			u.ExpireDate = fromDSMDate(v)
		}
	}
	if v, ok := m["2fa_status"].(bool); ok {
		u.TwoFactor = v
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
			// DSM may return groups either as a plain string name or as an
			// object {"name": "...", ...}. Handle both to avoid silently
			// dropping group membership on refresh.
			if s, ok := g.(string); ok {
				u.Groups = append(u.Groups, s)
			} else if obj, ok := g.(map[string]interface{}); ok {
				if name, ok := obj["name"].(string); ok && name != "" {
					u.Groups = append(u.Groups, name)
				}
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
