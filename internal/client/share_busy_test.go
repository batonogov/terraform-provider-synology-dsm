package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// shrinkShareBusyBackoff collapses the retry delays so the busy-path tests run
// in milliseconds rather than the ~15 seconds the production budget spans.
func shrinkShareBusyBackoff(t *testing.T) {
	t.Helper()
	origDelay, origAttempts := shareBusyBaseDelay, shareBusyAttempts
	shareBusyBaseDelay = time.Millisecond
	t.Cleanup(func() {
		shareBusyBaseDelay = origDelay
		shareBusyAttempts = origAttempts
	})
}

// busyShareServer answers the first failCount share mutations with the given
// DSM code and succeeds afterwards, mimicking a NAS that is still settling.
func busyShareServer(t *testing.T, code int, failCount int32) (*Client, *httptest.Server, *atomic.Int32) {
	t.Helper()
	var mutations atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api := r.URL.Query().Get("api")
		method := r.URL.Query().Get("method")
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			_ = r.ParseForm()
			api = r.FormValue("api")
			method = r.FormValue("method")
		}

		if api == "SYNO.Core.Share" && method == "get" {
			raw, _ := json.Marshal(map[string]interface{}{"name": r.URL.Query().Get("name"), "vol_path": "/volume1"})
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})
			return
		}

		if mutations.Add(1) <= failCount {
			_ = json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: code}})
			return
		}
		raw, _ := json.Marshal(map[string]interface{}{"name": "tfacctest"})
		_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})
	}))

	return NewClient(server.URL, "admin", "", false), server, &mutations
}

// TestCreateShare_RetriesBusyCodes is the regression for the reported failure:
// three shares in one apply, two rejected with 3328 while the third succeeded.
func TestCreateShare_RetriesBusyCodes(t *testing.T) {
	for _, code := range shareBusyCodes {
		t.Run(fmt.Sprintf("code %d", code), func(t *testing.T) {
			shrinkShareBusyBackoff(t)
			c, server, mutations := busyShareServer(t, code, 2)
			defer server.Close()

			share, err := c.CreateShare(context.Background(), CreateShareRequest{Name: "tfacctest", VolPath: "/volume1"})
			if err != nil {
				t.Fatalf("create should survive a busy DSM, got: %v", err)
			}
			if share == nil {
				t.Fatal("expected the share to be read back")
			}
			if got := mutations.Load(); got != 3 {
				t.Errorf("expected 2 rejected attempts then success (3 total), got %d", got)
			}
		})
	}
}

func TestUpdateAndDeleteShare_RetryBusyCodes(t *testing.T) {
	t.Run("update", func(t *testing.T) {
		shrinkShareBusyBackoff(t)
		c, server, mutations := busyShareServer(t, 3328, 1)
		defer server.Close()

		if _, err := c.UpdateShare(context.Background(), "tfacctest", CreateShareRequest{Name: "tfacctest", VolPath: "/volume1"}); err != nil {
			t.Fatalf("update should survive a busy DSM, got: %v", err)
		}
		if got := mutations.Load(); got != 2 {
			t.Errorf("expected 1 retry (2 attempts), got %d", got)
		}
	})

	// Deletion matters as much as creation: the reporter saw 3300 on a create
	// that ran right after a batch of deletes.
	t.Run("delete", func(t *testing.T) {
		shrinkShareBusyBackoff(t)
		c, server, mutations := busyShareServer(t, 3300, 1)
		defer server.Close()

		if err := c.DeleteShare(context.Background(), "tfacctest"); err != nil {
			t.Fatalf("delete should survive a busy DSM, got: %v", err)
		}
		if got := mutations.Load(); got != 2 {
			t.Errorf("expected 1 retry (2 attempts), got %d", got)
		}
	})
}

// TestCreateShare_DoesNotRetryPermanentErrors guards the other direction: a
// name collision is a real answer, and retrying it would turn an instant,
// actionable failure into a 15-second one.
func TestCreateShare_DoesNotRetryPermanentErrors(t *testing.T) {
	shrinkShareBusyBackoff(t)
	c, server, mutations := busyShareServer(t, 3301, 99)
	defer server.Close()

	_, err := c.CreateShare(context.Background(), CreateShareRequest{Name: "tfacctest", VolPath: "/volume1"})
	if !IsAPIError(err, 3301) {
		t.Fatalf("expected the 3301 to surface, got: %v", err)
	}
	if got := mutations.Load(); got != 1 {
		t.Errorf("a permanent error must not be retried, saw %d attempts", got)
	}
}

// TestCreateShare_SurfacesBusyAfterBudget checks that an exhausted budget still
// reports the DSM code rather than a generic retry-exhaustion message.
func TestCreateShare_SurfacesBusyAfterBudget(t *testing.T) {
	shrinkShareBusyBackoff(t)
	c, server, mutations := busyShareServer(t, 3328, 99)
	defer server.Close()

	_, err := c.CreateShare(context.Background(), CreateShareRequest{Name: "tfacctest", VolPath: "/volume1"})
	if !IsAPIError(err, 3328) {
		t.Fatalf("expected the busy code to survive the retries, got: %v", err)
	}
	if got := int(mutations.Load()); got != shareBusyAttempts {
		t.Errorf("expected %d attempts, got %d", shareBusyAttempts, got)
	}
}

func TestMutateShare_HonoursContextCancellation(t *testing.T) {
	origDelay := shareBusyBaseDelay
	shareBusyBaseDelay = time.Hour // force the test to hang unless ctx wins
	t.Cleanup(func() { shareBusyBaseDelay = origDelay })

	c, server, _ := busyShareServer(t, 3328, 99)
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() {
		_, err := c.CreateShare(ctx, CreateShareRequest{Name: "tfacctest", VolPath: "/volume1"})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected the cancellation to surface")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not interrupt the backoff")
	}
}

// TestShareMutations_AreSerialized is the core of the fix: DSM rejects
// overlapping share mutations, so the client must never issue two at once even
// when Terraform asks for ten in parallel.
func TestShareMutations_AreSerialized(t *testing.T) {
	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api := r.URL.Query().Get("api")
		method := r.URL.Query().Get("method")
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			_ = r.ParseForm()
			api = r.FormValue("api")
			method = r.FormValue("method")
		}

		if api == "SYNO.Core.Share" && (method == "create" || method == "set" || method == "delete") {
			mu.Lock()
			inFlight++
			if inFlight > maxInFlight {
				maxInFlight = inFlight
			}
			mu.Unlock()

			// Hold the request open so any overlap is observable.
			time.Sleep(5 * time.Millisecond)

			mu.Lock()
			inFlight--
			mu.Unlock()
		}

		raw, _ := json.Marshal(map[string]interface{}{"name": r.URL.Query().Get("name"), "vol_path": "/volume1"})
		_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})
	}))
	defer server.Close()

	c := NewClient(server.URL, "admin", "", false)

	const n = 12
	var wg sync.WaitGroup
	wg.Add(n)
	start := make(chan struct{})
	for i := range n {
		go func() {
			defer wg.Done()
			<-start
			switch i % 3 {
			case 0:
				_, _ = c.CreateShare(context.Background(), CreateShareRequest{Name: "tfacctest", VolPath: "/volume1"})
			case 1:
				_, _ = c.UpdateShare(context.Background(), "tfacctest", CreateShareRequest{Name: "tfacctest", VolPath: "/volume1"})
			case 2:
				_ = c.DeleteShare(context.Background(), "tfacctest")
			}
		}()
	}
	close(start)
	wg.Wait()

	mu.Lock()
	peak := maxInFlight
	mu.Unlock()
	if peak != 1 {
		t.Errorf("share mutations overlapped: %d were in flight at once, DSM allows 1", peak)
	}
}

// TestShareReads_AreNotSerialized keeps the lock narrow: reads are safe to run
// concurrently, and serialising them would slow every refresh for no reason.
func TestShareReads_AreNotSerialized(t *testing.T) {
	release := make(chan struct{})
	var concurrent atomic.Int32
	reached := make(chan struct{}, 2)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if concurrent.Add(1) <= 2 {
			reached <- struct{}{}
			<-release // hold both reads open simultaneously
		}
		raw, _ := json.Marshal(map[string]interface{}{"name": r.URL.Query().Get("name"), "vol_path": "/volume1"})
		_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})
	}))
	defer server.Close()

	c := NewClient(server.URL, "admin", "", false)
	for range 2 {
		go func() { _, _ = c.GetShare(context.Background(), "tfacctest") }()
	}

	// Both reads must reach the server before either is allowed to finish.
	for range 2 {
		select {
		case <-reached:
		case <-time.After(2 * time.Second):
			t.Fatal("reads were serialised: the second never reached the server")
		}
	}
	close(release)
}
