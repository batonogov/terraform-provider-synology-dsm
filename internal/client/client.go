package client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultHTTPTimeout = 30 * time.Second
	maxRetries         = 3
	retryBaseDelay     = 500 * time.Millisecond
)

type APIResponse struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   *APIError       `json:"error,omitempty"`
}

type APIError struct {
	Code int `json:"code"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("synology api error: code %d", e.Code)
}

type Client struct {
	httpClient *http.Client
	baseURL    string
	username   string
	password   string
	// sessionID and synoToken are read on every API call (buildParams) and
	// written on Login/re-login. sessMu (RWMutex) guards their access: readers
	// take RLock (concurrent), writers take Lock. This prevents the data race
	// where a concurrent re-login updates the session while another goroutine
	// is reading it for a request.
	sessionID string
	synoToken string
	sessMu    sync.RWMutex
	// loginMu serializes concurrent re-login attempts: if several goroutines hit
	// a 119 at once, only one Login is in flight at a time. Waiters still perform
	// their own Login after acquiring the lock (each new SID is valid), so a 119
	// storm results in up to N logins rather than 1; this trades a little extra
	// auth traffic for simplicity. Distinct from sessMu so that holding loginMu
	// during Login's network I/O does not block request param building.
	loginMu sync.Mutex
	// mu serializes read-modify-write sequences for APIs that DSM exposes as a
	// whole-list "set" (share permissions, user quotas). Without it, Terraform's
	// default parallelism (-parallelism=10) causes lost updates: each resource
	// reads the full list, mutates one entry, and writes it back, so concurrent
	// writers clobber each other. See SetSharePermission / SetUserQuota.
	mu sync.Mutex
}

func NewClient(host, username, password string, insecureTLS bool) *Client {
	transport := &http.Transport{}
	if insecureTLS {
		transport.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}

	return &Client{
		httpClient: &http.Client{
			Timeout:   defaultHTTPTimeout,
			Transport: transport,
		},
		baseURL:  strings.TrimRight(host, "/"),
		username: username,
		password: password,
	}
}

func (c *Client) Login(ctx context.Context) error {
	params := url.Values{}
	params.Set("api", "SYNO.API.Auth")
	params.Set("version", "7")
	params.Set("method", "login")
	params.Set("account", c.username)
	params.Set("passwd", c.password)
	params.Set("format", "sid")
	params.Set("enable_syno_token", "yes")

	resp, err := c.doGetRequest(ctx, params)
	if err != nil {
		return fmt.Errorf("login failed: %w", err)
	}

	var result struct {
		SID       string `json:"sid"`
		SynoToken string `json:"synotoken"`
	}
	if err := json.Unmarshal(resp.Data, &result); err != nil {
		return fmt.Errorf("parse login response: %w", err)
	}

	c.setSession(result.SID, result.SynoToken)
	return nil
}

// setSession updates the stored session credentials under sessMu (write lock).
func (c *Client) setSession(sid, token string) {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	c.sessionID = sid
	c.synoToken = token
}

// session returns a snapshot of the current session credentials under sessMu
// (read lock). Callers must not hold the returned values across a re-login if
// they need a consistent pair with a single request — use them immediately.
func (c *Client) session() (sid, token string) {
	c.sessMu.RLock()
	defer c.sessMu.RUnlock()
	return c.sessionID, c.synoToken
}

func (c *Client) Logout(ctx context.Context) error {
	params := url.Values{}
	params.Set("api", "SYNO.API.Auth")
	params.Set("version", "7")
	params.Set("method", "logout")

	_, err := c.doGetRequest(ctx, params)
	c.setSession("", "")
	return err
}

func (c *Client) DoAPI(ctx context.Context, api, version, method string, extraParams url.Values) (json.RawMessage, error) {
	params := c.buildParams(api, version, method, extraParams)

	resp, err := c.doRequestWithRetry(ctx, params, http.MethodGet)
	if err != nil {
		return nil, err
	}
	return resp.Data, nil
}

func (c *Client) DoAPIPost(ctx context.Context, api, version, method string, extraParams url.Values) (json.RawMessage, error) {
	params := c.buildParams(api, version, method, extraParams)

	resp, err := c.doRequestWithRetry(ctx, params, http.MethodPost)
	if err != nil {
		return nil, err
	}
	return resp.Data, nil
}

func (c *Client) buildParams(api, version, method string, extraParams url.Values) url.Values {
	params := url.Values{}
	params.Set("api", api)
	params.Set("version", version)
	params.Set("method", method)

	sid, token := c.session()
	if sid != "" {
		params.Set("_sid", sid)
	}
	if token != "" {
		params.Set("SynoToken", token)
	}

	for k, vs := range extraParams {
		for _, v := range vs {
			params.Set(k, v)
		}
	}

	return params
}

func (c *Client) doGetRequest(ctx context.Context, params url.Values) (*APIResponse, error) {
	return c.executeRequest(ctx, params, http.MethodGet)
}

func (c *Client) executeRequest(ctx context.Context, params url.Values, httpMethod string) (*APIResponse, error) {
	var req *http.Request
	var err error

	endpoint := c.baseURL + "/webapi/entry.cgi"

	switch httpMethod {
	case http.MethodPost:
		queryParams := url.Values{}
		if params.Get("_sid") != "" {
			queryParams.Set("_sid", params.Get("_sid"))
		}
		if params.Get("SynoToken") != "" {
			queryParams.Set("SynoToken", params.Get("SynoToken"))
		}
		params.Del("_sid")
		params.Del("SynoToken")

		reqURL := endpoint
		if len(queryParams) > 0 {
			reqURL += "?" + queryParams.Encode()
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(params.Encode()))
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	default:
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+params.Encode(), nil)
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var apiResp APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if !apiResp.Success {
		if apiResp.Error != nil {
			return nil, fmt.Errorf("api error %d: %w", apiResp.Error.Code, apiResp.Error)
		}
		return nil, fmt.Errorf("api returned success=false with no error details")
	}

	return &apiResp, nil
}

func (c *Client) doRequestWithRetry(ctx context.Context, params url.Values, httpMethod string) (*APIResponse, error) {
	var lastErr error

	for attempt := range maxRetries {
		if attempt > 0 {
			delay := time.Duration(math.Pow(2, float64(attempt-1))) * retryBaseDelay
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		resp, err := c.executeRequest(ctx, params, httpMethod)
		if err != nil {
			lastErr = err
			// Error 119 = "SID not found or invalid" (session expired). DSM
			// sessions are short-lived; a long apply can outlive the SID.
			// Re-login once and retry with a fresh session.
			if isSessionExpiredError(err) {
				if relErr := c.relogin(ctx); relErr != nil {
					return nil, fmt.Errorf("re-login after expired session: %w (original: %v)", relErr, err)
				}
				params = c.refreshSessionParams(params)
				continue
			}
			if isTransientError(err) {
				continue
			}
			return nil, err
		}
		return resp, nil
	}

	return nil, fmt.Errorf("max retries exceeded: %w", lastErr)
}

// relogin serializes concurrent re-login attempts via loginMu (so only one
// goroutine performs the network Login) and re-authenticates. It does NOT hold
// loginMu across request param building: sessMu independently guards the
// session fields, so other goroutines keep building requests with a consistent
// (possibly stale) session snapshot during the re-login.
func (c *Client) relogin(ctx context.Context) error {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	return c.Login(ctx)
}

// refreshSessionParams rebuilds the auth params (_sid, SynoToken) after a
// re-login, preserving all other query parameters.
func (c *Client) refreshSessionParams(params url.Values) url.Values {
	sid, token := c.session()
	if sid != "" {
		params.Set("_sid", sid)
	} else {
		params.Del("_sid")
	}
	if token != "" {
		params.Set("SynoToken", token)
	} else {
		params.Del("SynoToken")
	}
	return params
}

func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "temporary") ||
		strings.Contains(msg, "EOF")
}

// isSessionExpiredError detects DSM API error 119 ("SID not found or invalid"),
// which signals that the session has expired and a re-login is required.
func isSessionExpiredError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "api error 119")
}

func boolParam(v bool) string {
	return strconv.FormatBool(v)
}
