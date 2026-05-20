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
	httpClient  *http.Client
	baseURL     string
	username    string
	password    string
	sessionID   string
	synoToken   string
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

	resp, err := c.doRequest(ctx, params)
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

	c.sessionID = result.SID
	c.synoToken = result.SynoToken
	return nil
}

func (c *Client) Logout(ctx context.Context) error {
	params := url.Values{}
	params.Set("api", "SYNO.API.Auth")
	params.Set("version", "7")
	params.Set("method", "logout")

	_, err := c.doRequest(ctx, params)
	c.sessionID = ""
	return err
}

func (c *Client) DoAPI(ctx context.Context, api, version, method string, extraParams url.Values) (json.RawMessage, error) {
	params := url.Values{}
	params.Set("api", api)
	params.Set("version", version)
	params.Set("method", method)

	if c.sessionID != "" {
		params.Set("_sid", c.sessionID)
	}
	if c.synoToken != "" {
		params.Set("SynoToken", c.synoToken)
	}

	for k, vs := range extraParams {
		for _, v := range vs {
			params.Set(k, v)
		}
	}

	resp, err := c.doRequestWithRetry(ctx, params)
	if err != nil {
		return nil, err
	}
	return resp.Data, nil
}

func (c *Client) doRequest(ctx context.Context, params url.Values) (*APIResponse, error) {
	reqURL := c.baseURL + "/webapi/entry.cgi?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
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

func (c *Client) doRequestWithRetry(ctx context.Context, params url.Values) (*APIResponse, error) {
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

		resp, err := c.doRequest(ctx, params)
		if err != nil {
			lastErr = err
			if isTransientError(err) {
				continue
			}
			return nil, err
		}
		return resp, nil
	}

	return nil, fmt.Errorf("max retries exceeded: %w", lastErr)
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

func (c *Client) DoAPIPost(ctx context.Context, api, version, method string, extraParams url.Values) (json.RawMessage, error) {
	params := url.Values{}
	params.Set("api", api)
	params.Set("version", version)
	params.Set("method", method)

	if c.sessionID != "" {
		params.Set("_sid", c.sessionID)
	}
	if c.synoToken != "" {
		params.Set("SynoToken", c.synoToken)
	}

	for k, vs := range extraParams {
		for _, v := range vs {
			params.Set(k, v)
		}
	}

	resp, err := c.doPostRequestWithRetry(ctx, params)
	if err != nil {
		return nil, err
	}
	return resp.Data, nil
}

func (c *Client) doPostRequest(ctx context.Context, params url.Values) (*APIResponse, error) {
	reqURL := c.baseURL + "/webapi/entry.cgi"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(params.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

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

func (c *Client) doPostRequestWithRetry(ctx context.Context, params url.Values) (*APIResponse, error) {
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

		resp, err := c.doPostRequest(ctx, params)
		if err != nil {
			lastErr = err
			if isTransientError(err) {
				continue
			}
			return nil, err
		}
		return resp, nil
	}

	return nil, fmt.Errorf("max retries exceeded: %w", lastErr)
}

func boolParam(v bool) string {
	return strconv.FormatBool(v)
}
