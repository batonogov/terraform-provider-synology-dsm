package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"
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

// shareConcurrentMutationCode is DSM's rejection of a share mutation that
// overlaps another one. It means "not now" and nothing else, so it gets the
// full retry budget.
const shareConcurrentMutationCode = 3328

// shareGeneralRejectionCode is DSM's catch-all refusal for share requests. It
// comes back while an earlier mutation is still settling (issue #50) — but also
// for a request DSM will never accept, such as a description over 64 characters
// (issue #65). Since the two are indistinguishable on the wire, it is retried
// briefly: enough to ride out settling, not so long that a malformed request
// takes fifteen seconds to report what it could have reported at once.
const shareGeneralRejectionCode = 3300

var shareBusyCodes = []int{shareGeneralRejectionCode, shareConcurrentMutationCode}

// Retry budget for busy responses. Variables so tests can shrink them; the
// defaults span roughly 15 seconds, which covers the settling delays observed
// on DS1525+ / DSM 7.4.
var (
	shareBusyAttempts  = 5
	shareBusyBaseDelay = time.Second
	// Attempts allowed for the ambiguous 3300 before it is taken at face value.
	shareGeneralRejectionAttempts = 2
)

// mutateShare runs a share mutation with the two properties DSM requires:
// mutations do not overlap, and a busy response is waited out rather than
// surfaced. Callers must not hold shareMu themselves.
//
// This retries DSM's answers; doRequestWithRetry independently retries
// transport failures underneath, so a fully broken network can cost
// shareBusyAttempts × maxRetries requests before the error surfaces.
func (c *Client) mutateShare(ctx context.Context, do func() error) error {
	c.shareMu.Lock()
	defer c.shareMu.Unlock()

	var err error
	for attempt := range shareBusyAttempts {
		if attempt > 0 {
			delay := shareBusyBaseDelay * time.Duration(1<<(attempt-1))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		if err = do(); err == nil {
			return nil
		}
		if !IsAPIError(err, shareBusyCodes...) {
			return err
		}
		// 3300 also answers requests DSM considers malformed, which no amount of
		// waiting fixes. Give it a couple of attempts for the settling case, then
		// report it instead of spending the whole budget on a lost cause.
		if IsAPIError(err, shareGeneralRejectionCode) && attempt+1 >= shareGeneralRejectionAttempts {
			return err
		}
	}
	return err
}

func (c *Client) CreateShare(ctx context.Context, req CreateShareRequest) (*Share, error) {
	params := url.Values{}
	params.Set("name", req.Name)
	params.Set("shareinfo", buildShareInfo(req))

	err := c.mutateShare(ctx, func() error {
		_, err := c.DoAPIPost(ctx, "SYNO.Core.Share", "1", "create", params)
		return err
	})
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
		if absenceConfirmedBy(ctx, err, func(ctx context.Context) (bool, error) {
			return c.shareExists(ctx, name)
		}) {
			return nil, &NotFoundError{Kind: "shared folder", Name: name}
		}
		return nil, fmt.Errorf("get share %q: %w", name, err)
	}

	share, err := parseShare(data)
	if err != nil {
		return nil, err
	}
	// A successful envelope carrying no share is DSM saying the name is unknown,
	// not a parse failure: parseShare fills in what it finds and leaves the rest
	// zeroed, so an empty name is the only signal there is.
	if share.Name == "" {
		return nil, &NotFoundError{Kind: "shared folder", Name: name}
	}
	return share, nil
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

	err := c.mutateShare(ctx, func() error {
		_, err := c.DoAPIPost(ctx, "SYNO.Core.Share", "1", "set", params)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("update share %q: %w", name, err)
	}

	return c.GetShare(ctx, req.Name)
}

func (c *Client) DeleteShare(ctx context.Context, name string) error {
	namesJSON, _ := json.Marshal([]string{name})

	params := url.Values{}
	params.Set("name", string(namesJSON))

	err := c.mutateShare(ctx, func() error {
		_, err := c.DoAPI(ctx, "SYNO.Core.Share", "1", "delete", params)
		return err
	})
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

// shareExists answers the same question as GetShare through `list`, which is
// what makes a failed `get` decidable: DSM has no documented code for a shared
// folder that is not there.
func (c *Client) shareExists(ctx context.Context, name string) (bool, error) {
	shares, err := c.ListShares(ctx)
	if err != nil {
		return false, err
	}
	for i := range shares {
		if shares[i].Name == name {
			return true, nil
		}
	}
	return false, nil
}
