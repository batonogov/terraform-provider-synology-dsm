package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// reloginTestServer builds a mock DSM that:
//   - answers login with a fresh SID each call (so we can count re-logins);
//   - returns error 119 for a given number of "real" calls, then success.
//
// It returns the server, the client (already logged in), and counters to
// observe login attempts and 119 responses.
type reloginFixture struct {
	server   *httptest.Server
	client   *Client
	logins   atomic.Int64
	calls119 atomic.Int64
}

func newReloginFixture(t *testing.T, fail119Times int32) *reloginFixture {
	t.Helper()
	f := &reloginFixture{}

	mux := http.NewServeMux()
	mux.HandleFunc("/webapi/entry.cgi", func(w http.ResponseWriter, r *http.Request) {
		api := r.URL.Query().Get("api")
		method := r.URL.Query().Get("method")

		switch {
		case api == "SYNO.API.Auth" && method == "login":
			f.logins.Add(1)
			n := f.logins.Load()
			json.NewEncoder(w).Encode(APIResponse{
				Success: true,
				Data:    json.RawMessage(fmt.Sprintf(`{"sid":"sid-%d","synotoken":"tok-%d"}`, n, n)),
			})

		case api == "SYNO.Core.User" && method == "list":
			// Fail with 119 for the first `fail119Times` real (non-login) calls.
			if f.calls119.Add(1) <= int64(fail119Times) {
				json.NewEncoder(w).Encode(APIResponse{
					Success: false,
					Error:   &APIError{Code: 119},
				})
				return
			}
			data := `{"users":[{"name":"admin","uid":1024}],"total":1}`
			json.NewEncoder(w).Encode(APIResponse{Success: true, Data: json.RawMessage(data)})

		default:
			json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 101}})
		}
	})

	f.server = httptest.NewServer(mux)
	f.client = NewClient(f.server.URL, "admin", "password", false)
	if err := f.client.Login(context.Background()); err != nil {
		t.Fatalf("initial login: %v", err)
	}
	// NOTE: logins is NOT reset. Initial login produced sid-1; each subsequent
	// re-login increments it, so callers can assert on the absolute SID value.
	return f
}

// TestClient_DoAPI_ReloginOn119 proves that an expired session (error 119)
// triggers a transparent re-login and a successful retry of the original call.
func TestClient_DoAPI_ReloginOn119(t *testing.T) {
	f := newReloginFixture(t, 1)
	defer f.server.Close()

	data, err := f.client.DoAPI(context.Background(), "SYNO.Core.User", "1", "list", nil)
	if err != nil {
		t.Fatalf("DoAPI after 119 should succeed via re-login, got: %v", err)
	}
	if !strings.Contains(string(data), "admin") {
		t.Fatalf("unexpected payload: %s", data)
	}
	// Initial login (sid-1) + one re-login (sid-2).
	if got := f.logins.Load(); got != 2 {
		t.Fatalf("expected 2 logins total (1 initial + 1 re-login), got %d", got)
	}
	sid, _ := f.client.session()
	if sid != "sid-2" {
		t.Fatalf("expected session updated to sid-2 after re-login, got %q", sid)
	}
}

// TestClient_DoAPI_119BoundedRetries proves that a persistent 119 (every call
// fails) does NOT cause an infinite loop: it terminates after maxRetries.
func TestClient_DoAPI_119BoundedRetries(t *testing.T) {
	f := newReloginFixture(t, 1_000_000) // always fail
	defer f.server.Close()

	_, err := f.client.DoAPI(context.Background(), "SYNO.Core.User", "1", "list", nil)
	if err == nil {
		t.Fatal("expected error when 119 is persistent, got nil")
	}
	// Persistent 119: each of maxRetries attempts re-logins (successfully) then
	// retries, and the call still fails. The terminal error reports exhaustion
	// with the 119 context (not "re-login", which only appears if Login fails).
	if !strings.Contains(err.Error(), "max retries exceeded") {
		t.Fatalf("error should report retries exhausted, got: %v", err)
	}
	if !strings.Contains(err.Error(), "api error 119") {
		t.Fatalf("error should carry the 119 context, got: %v", err)
	}
	// 1 initial login + maxRetries re-login attempts.
	if got := f.logins.Load(); got != int64(maxRetries+1) {
		t.Fatalf("expected %d logins total (1 initial + %d re-logins), got %d", maxRetries+1, maxRetries, got)
	}
}

// TestClient_DoAPIPost_ReloginKeepsSessionInQueryString verifies that after a
// re-login, a retried POST carries _sid and SynoToken in the QUERY STRING (per
// DSM requirement), not in the body.
func TestClient_DoAPIPost_ReloginKeepsSessionInQueryString(t *testing.T) {
	var postSid, postToken string
	var sawPost atomic.Bool

	mux := http.NewServeMux()
	var logins atomic.Int64
	var calls atomic.Int64
	mux.HandleFunc("/webapi/entry.cgi", func(w http.ResponseWriter, r *http.Request) {
		// For POST, api/method travel in the body; _sid/SynoToken in the query.
		if r.Method == http.MethodPost {
			_ = r.ParseForm()
		}
		api := r.FormValue("api")
		if api == "" {
			api = r.URL.Query().Get("api")
		}
		method := r.FormValue("method")
		if method == "" {
			method = r.URL.Query().Get("method")
		}
		switch {
		case api == "SYNO.API.Auth" && method == "login":
			logins.Add(1)
			n := logins.Load()
			json.NewEncoder(w).Encode(APIResponse{
				Success: true,
				Data:    json.RawMessage(fmt.Sprintf(`{"sid":"post-sid-%d","synotoken":"post-tok-%d"}`, n, n)),
			})
		case api == "SYNO.Core.Share" && method == "create":
			if calls.Add(1) == 1 {
				json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 119}})
				return
			}
			// Inspect the retried POST: session must be in the QUERY STRING.
			postSid = r.URL.Query().Get("_sid")
			postToken = r.URL.Query().Get("SynoToken")
			sawPost.Store(true)
			json.NewEncoder(w).Encode(APIResponse{Success: true})
		default:
			json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 101}})
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := NewClient(server.URL, "admin", "password", false)
	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}

	_, err := c.DoAPIPost(context.Background(), "SYNO.Core.Share", "1", "create", nil)
	if err != nil {
		t.Fatalf("DoAPIPost should succeed after re-login, got: %v", err)
	}
	if !sawPost.Load() {
		t.Fatal("expected a retried POST to reach the server")
	}
	if postSid != "post-sid-2" {
		t.Fatalf("retried POST should carry fresh _sid in query string, got %q", postSid)
	}
	if postToken != "post-tok-2" {
		t.Fatalf("retried POST should carry fresh SynoToken in query string, got %q", postToken)
	}
}

// TestClient_DoAPI_ReloginFailure verifies that when the re-login itself fails
// (e.g. credentials revoked), the error preserves BOTH the re-login failure
// (wrapped) and the original 119 context.
func TestClient_DoAPI_ReloginFailure(t *testing.T) {
	mux := http.NewServeMux()
	var realCalls atomic.Int64
	mux.HandleFunc("/webapi/entry.cgi", func(w http.ResponseWriter, r *http.Request) {
		api := r.URL.Query().Get("api")
		method := r.URL.Query().Get("method")
		switch {
		case api == "SYNO.API.Auth" && method == "login":
			// First login succeeds; subsequent re-logins fail with 400.
			if realCalls.Add(1) >= 1 && r.URL.Query().Get("account") != "" && realCalls.Load() > 1 {
				json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 400}})
				return
			}
			json.NewEncoder(w).Encode(APIResponse{Success: true, Data: json.RawMessage(`{"sid":"s1","synotoken":"t1"}`)})
		default:
			json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 119}})
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := NewClient(server.URL, "admin", "password", false)
	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("initial login: %v", err)
	}

	_, err := c.DoAPI(context.Background(), "SYNO.Core.User", "1", "list", nil)
	if err == nil {
		t.Fatal("expected error when re-login fails")
	}
	if !strings.Contains(err.Error(), "re-login") {
		t.Fatalf("error should mention re-login failure, got: %v", err)
	}
	if !strings.Contains(err.Error(), "api error 119") {
		t.Fatalf("error should preserve original 119 context, got: %v", err)
	}
}

// TestClient_SetSharePermission_Concurrent proves the provider-level mutex (B4)
// prevents lost updates when many permissions are set on the same share in
// parallel. Run with -race to also verify no data race on session fields.
func TestClient_SetSharePermission_Concurrent(t *testing.T) {
	client, server := setupSharePermissionTestServer()
	defer server.Close()

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			_, _ = client.SetSharePermission(context.Background(), SetSharePermissionRequest{
				ShareName:     "data",
				UserGroupType: "local_user",
				PrincipalName: fmt.Sprintf("user-%d", i),
				Permission:    "read_write",
			})
		}()
	}
	close(start)
	wg.Wait()

	perms, err := client.ListSharePermissions(context.Background(), "data", "local_user")
	if err != nil {
		t.Fatalf("ListSharePermissions: %v", err)
	}
	// Without the mutex, concurrent list→modify→set clobbered each other and
	// far fewer than n principals would survive. With the mutex all n persist.
	got := map[string]bool{}
	for _, p := range perms {
		got[p.Name] = true
	}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("user-%d", i)
		if !got[name] {
			t.Errorf("lost update: principal %q missing after concurrent set (have %d of %d)",
				name, len(perms), n)
		}
	}
}

// TestClient_SetUserQuota_Concurrent mirrors the above for user quotas (B4).
func TestClient_SetUserQuota_Concurrent(t *testing.T) {
	client, server := setupUserQuotaTestServer()
	defer server.Close()

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			_, _ = client.SetUserQuota(context.Background(), SetUserQuotaRequest{
				ShareName: "data",
				Username:  fmt.Sprintf("user-%d", i),
				QuotaSize: int64(i+1) * 1024,
			})
		}()
	}
	close(start)
	wg.Wait()

	quotas, err := client.ListUserQuotas(context.Background(), "data")
	if err != nil {
		t.Fatalf("ListUserQuotas: %v", err)
	}
	got := map[string]bool{}
	for _, q := range quotas {
		got[q.Username] = true
	}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("user-%d", i)
		if !got[name] {
			t.Errorf("lost update: quota for %q missing (have %d of %d)", name, len(quotas), n)
		}
	}
}

// TestClient_DeleteSharePermission_Concurrent verifies the Delete path is also
// serialized by c.mu. Delete uses the same list -> filter -> set-all RMW as Set,
// so a missing lock would let concurrent deletes clobber each other and leave
// principals that should have been removed. We first seed n principals, then
// delete all of them in parallel, and assert none survive.
func TestClient_DeleteSharePermission_Concurrent(t *testing.T) {
	client, server := setupSharePermissionTestServer()
	defer server.Close()

	// Seed n principals on a clean slate.
	for i := 0; i < 50; i++ {
		_, err := client.SetSharePermission(context.Background(), SetSharePermissionRequest{
			ShareName:     "data",
			UserGroupType: "local_user",
			PrincipalName: fmt.Sprintf("del-%d", i),
			Permission:    "read_write",
		})
		if err != nil {
			t.Fatalf("seed SetSharePermission(%d): %v", i, err)
		}
	}

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			_ = client.DeleteSharePermission(context.Background(), "data", "local_user", fmt.Sprintf("del-%d", i))
		}()
	}
	close(start)
	wg.Wait()

	perms, err := client.ListSharePermissions(context.Background(), "data", "local_user")
	if err != nil {
		t.Fatalf("ListSharePermissions: %v", err)
	}
	for _, p := range perms {
		if strings.HasPrefix(p.Name, "del-") {
			t.Errorf("principal %q survived concurrent delete (have %d remaining)", p.Name, len(perms))
		}
	}
}

// TestClient_DeleteUserQuota_Concurrent mirrors the above for quotas.
func TestClient_DeleteUserQuota_Concurrent(t *testing.T) {
	client, server := setupUserQuotaTestServer()
	defer server.Close()

	for i := 0; i < 50; i++ {
		_, err := client.SetUserQuota(context.Background(), SetUserQuotaRequest{
			ShareName: "data",
			Username:  fmt.Sprintf("del-%d", i),
			QuotaSize: int64(i+1) * 1024,
		})
		if err != nil {
			t.Fatalf("seed SetUserQuota(%d): %v", i, err)
		}
	}

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			_ = client.DeleteUserQuota(context.Background(), "data", fmt.Sprintf("del-%d", i))
		}()
	}
	close(start)
	wg.Wait()

	quotas, err := client.ListUserQuotas(context.Background(), "data")
	if err != nil {
		t.Fatalf("ListUserQuotas: %v", err)
	}
	for _, q := range quotas {
		if !strings.HasPrefix(q.Username, "del-") {
			continue
		}
		// DSM quota delete semantics = reset QuotaSize to 0 (unlimited), not removal
		// of the principal from the list (unlike share permissions). Assert the reset.
		if q.QuotaSize != 0 {
			t.Errorf("quota for %q not reset to 0 after concurrent delete: size=%d (have %d remaining)",
				q.Username, q.QuotaSize, len(quotas))
		}
	}
}

// TestClient_ConcurrentRelogin exercises the session-locking discipline under
// contention. Many goroutines build requests (reading sessionID/synoToken via
// sessMu.RLock in buildParams) while exactly one of them triggers a re-login
// (writing those fields via sessMu.Lock in setSession). Under -race this catches
// any regression that reintroduces a bare read/write of the session fields on
// the re-login path. It is NOT a stress test of loginMu's multi-waiter path
// (only one goroutine 119s here); SetSharePermission/SetUserQuota_Concurrent
// cover the mu RMW path separately.
func TestClient_ConcurrentRelogin(t *testing.T) {
	mux := http.NewServeMux()
	var logins atomic.Int64
	var firstReal atomic.Bool
	firstReal.Store(true)
	mux.HandleFunc("/webapi/entry.cgi", func(w http.ResponseWriter, r *http.Request) {
		api := r.URL.Query().Get("api")
		method := r.URL.Query().Get("method")
		switch {
		case api == "SYNO.API.Auth" && method == "login":
			n := logins.Add(1)
			json.NewEncoder(w).Encode(APIResponse{
				Success: true,
				Data:    json.RawMessage(fmt.Sprintf(`{"sid":"c-sid-%d","synotoken":"c-tok-%d"}`, n, n)),
			})
		case api == "SYNO.Core.User" && method == "list":
			// First real call 119s (triggers re-login across goroutines); later calls succeed.
			if firstReal.CompareAndSwap(true, false) {
				json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 119}})
				return
			}
			data := `{"users":[{"name":"admin","uid":1024}],"total":1}`
			json.NewEncoder(w).Encode(APIResponse{Success: true, Data: json.RawMessage(data)})
		default:
			json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 101}})
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := NewClient(server.URL, "admin", "password", false)
	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("initial login: %v", err)
	}

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	start := make(chan struct{})
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start
			if _, err := c.DoAPI(context.Background(), "SYNO.Core.User", "1", "list", nil); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent DoAPI failed: %v", err)
	}
	// Exactly one goroutine receives a 119 and one re-login follows, so total
	// logins == 2. The meaningful check is -race cleanliness (20 concurrent
	// readers of the session fields racing the one writer during re-login).
	if got := logins.Load(); got != 2 {
		t.Errorf("expected exactly 2 logins (1 initial + 1 re-login), got %d", got)
	}
}

// TestIsSessionExpiredError covers the string-based 119 detection.
func TestIsSessionExpiredError(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"api error 119: synology api error: code 119", true},
		{"api error 102: synology api error: code 102", false},
		// Regression: codes 1190, 1191, 11900 contain "api error 119" as a substring
		// but must NOT be treated as session-expired (they would trigger a bogus re-login).
		{"api error 1190: synology api error: code 1190", false},
		{"api error 1191: synology api error: code 1191", false},
		{"api error 11900: synology api error: code 11900", false},
		{"http request: connection refused", false},
		{"", false},
	}
	for _, c := range cases {
		err := fmt.Errorf("%s", c.in)
		if got := isSessionExpiredError(err); got != c.want {
			t.Errorf("isSessionExpiredError(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	// nil must not panic.
	if isSessionExpiredError(nil) {
		t.Error("isSessionExpiredError(nil) should be false")
	}
}
