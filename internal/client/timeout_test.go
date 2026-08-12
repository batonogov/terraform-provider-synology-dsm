package client

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIsTimeoutError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"wrapped deadline", errors.Join(errors.New("http request"), context.DeadlineExceeded), true},
		{"net timeout", &net.OpError{Err: &timeoutError{}}, true},
		{"api error is an answer, not a timeout", &APIError{Code: 3300}, false},
		{"plain error", errors.New("connection refused"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsTimeoutError(tt.err); got != tt.want {
				t.Errorf("IsTimeoutError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

type timeoutError struct{}

func (e *timeoutError) Error() string { return "i/o timeout" }
func (e *timeoutError) Timeout() bool { return true }

// TestSlowCallUsesLongerTimeout covers the first half of #70: a lifecycle call
// must not be cut off by the read timeout, because its duration is set by how
// long DSM takes rather than by the network.
func TestSlowCallUsesLongerTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Longer than the ordinary timeout configured below, shorter than the
		// slow one.
		time.Sleep(120 * time.Millisecond)
		raw, _ := json.Marshal(map[string]string{"name": "app"})
		_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})
	}))
	defer server.Close()

	c := NewClientWithTimeout(server.URL, "admin", "", false, 40*time.Millisecond)
	// The production slow timeout is minutes; shorten it so the test stays fast
	// while still exceeding the handler's delay.
	c.slowClient.Timeout = 3 * time.Second

	if _, err := c.DoAPIPost(context.Background(), "SYNO.Docker.Project", "1", "update", nil); !IsTimeoutError(err) {
		t.Fatalf("an ordinary call should hit the short timeout, got: %v", err)
	}
	if _, err := c.DoAPIPost(WithSlowCall(context.Background()), "SYNO.Docker.Project", "1", "update", nil); err != nil {
		t.Fatalf("a slow call should survive the same delay, got: %v", err)
	}
}

func TestNewClientWithTimeout_FloorsTheSlowClient(t *testing.T) {
	c := NewClientWithTimeout("http://nas", "admin", "", false, time.Second)
	if c.httpClient.Timeout != time.Second {
		t.Errorf("ordinary timeout = %s, want 1s", c.httpClient.Timeout)
	}
	// A short read timeout must not drag lifecycle calls down with it.
	if c.slowClient.Timeout != defaultSlowTimeout {
		t.Errorf("slow timeout = %s, want the %s floor", c.slowClient.Timeout, defaultSlowTimeout)
	}

	// A generous read timeout raises the slow one rather than lowering it.
	c = NewClientWithTimeout("http://nas", "admin", "", false, 30*time.Minute)
	if c.slowClient.Timeout != 30*time.Minute {
		t.Errorf("slow timeout = %s, want it to follow the configured value", c.slowClient.Timeout)
	}

	// Zero means "use the default", not "no timeout".
	c = NewClientWithTimeout("http://nas", "admin", "", false, 0)
	if c.httpClient.Timeout != defaultHTTPTimeout {
		t.Errorf("zero timeout = %s, want the %s default", c.httpClient.Timeout, defaultHTTPTimeout)
	}
}
