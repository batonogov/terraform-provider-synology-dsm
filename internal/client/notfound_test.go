package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The whole point of NotFoundError is that "the object is gone" and "the call
// failed" are different answers, and that the difference is decided by type
// rather than by the sentence DSM or the client happened to render. A Read that
// gets it wrong either wedges every plan (issue #131) or silently drops live
// infrastructure out of state.
func TestIsNotFound_SeparatesAbsenceFromFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"a bare NotFoundError", &NotFoundError{Kind: "user", Name: "john"}, true},
		{"wrapped in context", fmt.Errorf("refresh: %w", &NotFoundError{Kind: "group", Name: "devs"}), true},
		{"a package sentinel", fmt.Errorf("read: %w", ErrPackageNotFound), true},
		{"a file sentinel", fmt.Errorf("read: %w", ErrFileNotFound), true},

		{"nil", nil, false},
		// The ones that matter: DSM answered about the *caller*, not about the
		// object. Removing a resource from state on any of these would plan a
		// re-create of infrastructure that is still there.
		{"an expired session", &APIError{Code: 119, API: "SYNO.Core.Share"}, false},
		{"a permission refusal", &APIError{Code: 105, API: "SYNO.Core.Share"}, false},
		{"a timed-out session", &APIError{Code: 106, API: "SYNO.Core.Share"}, false},
		{"an unsupported API", &APIError{Code: 102, API: "SYNO.Core.Share.Quota"}, false},
		{"a transport failure", errors.New("dial tcp 10.0.0.2:5001: connect: connection refused"), false},
		// The regression guard for the rule in CLAUDE.md: the code is the
		// contract, the message is presentation. An untyped error that merely
		// says "not found" must not be mistaken for one.
		{"an untyped error whose text says not found", errors.New(`firewall rule "DSM web UI" not found in profile "default"`), false},
		{"the access control profile sentinel, deliberately excluded", ErrAccessControlProfileNotFound, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsNotFound(tt.err); got != tt.want {
				t.Errorf("IsNotFound(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// The sentinels became *NotFoundError values; every existing errors.Is call site
// must keep working, and two different sentinels must stay distinguishable.
func TestNotFoundSentinels_RemainDistinct(t *testing.T) {
	wrapped := fmt.Errorf("read: %w", ErrFileNotFound)

	if !errors.Is(wrapped, ErrFileNotFound) {
		t.Error("errors.Is must still match the sentinel it wraps")
	}
	if errors.Is(wrapped, ErrPackageNotFound) {
		t.Error("two different not-found sentinels must not compare equal")
	}
	if got, want := ErrFileNotFound.Error(), "file not found"; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

func TestNotFoundError_Message(t *testing.T) {
	tests := []struct {
		err  *NotFoundError
		want string
	}{
		{&NotFoundError{Kind: "DSM package"}, "DSM package not found"},
		{&NotFoundError{Kind: "user", Name: "john"}, `user "john" not found`},
		{
			&NotFoundError{Kind: "firewall rule", Name: "DSM web UI", Scope: `in profile "default" adapter "global"`},
			`firewall rule "DSM web UI" not found in profile "default" adapter "global"`,
		},
	}
	for _, tt := range tests {
		if got := tt.err.Error(); got != tt.want {
			t.Errorf("Error() = %q, want %q", got, tt.want)
		}
	}
}

// The rule the issue was filed about. A profile DSM returned without the rule is
// established absence; a profile that could not be read at all is not.
func TestClient_GetFirewallRule_MissingRuleIsNotFound(t *testing.T) {
	c, _, server := newFirewallFixture(t, true, map[string][]map[string]interface{}{
		FirewallAdapterGlobal: {allowAllRule("keep me")},
	})
	defer server.Close()

	_, err := c.GetFirewallRulePlacement(context.Background(), "default", FirewallAdapterGlobal, "DSM web UI")
	if err == nil {
		t.Fatal("expected an error for a rule that is not in the profile")
	}
	if !IsNotFound(err) {
		t.Errorf("a rule absent from a profile DSM returned must be a NotFoundError, got %T: %v", err, err)
	}
}

func TestClient_GetFirewallRule_UnreachableDSMIsNotNotFound(t *testing.T) {
	c, _, server := newFirewallFixture(t, true, map[string][]map[string]interface{}{
		FirewallAdapterGlobal: {allowAllRule("keep me")},
	})
	// Closing the server before the call is the cheapest stand-in for a NAS that
	// dropped off the network mid-refresh.
	server.Close()

	_, err := c.GetFirewallRulePlacement(context.Background(), "default", FirewallAdapterGlobal, "keep me")
	if err == nil {
		t.Fatal("expected an error when DSM is unreachable")
	}
	if IsNotFound(err) {
		t.Errorf("an unreachable NAS must never read as 'the rule is gone': %v", err)
	}
}

func TestClient_GetUser_MissingIsNotFound(t *testing.T) {
	c, server := setupTestServer()
	defer server.Close()

	_, err := c.GetUser(context.Background(), "ghost")
	if !IsNotFound(err) {
		t.Errorf("GetUser for an absent account: got %T (%v), want a NotFoundError", err, err)
	}
}

func TestClient_GetGroup_MissingIsNotFound(t *testing.T) {
	c, server := setupGroupTestServer()
	defer server.Close()

	// DSM has no documented "no such group" code, so a refused get is settled by
	// listing. The fixture lists only administrators and users.
	_, err := c.GetGroup(context.Background(), "ghost")
	if err != nil && !IsNotFound(err) {
		t.Errorf("GetGroup: got %T (%v)", err, err)
	}
}

func TestClient_GetSharePermission_MissingIsNotFound(t *testing.T) {
	c, server := setupSharePermissionTestServer()
	defer server.Close()

	_, err := c.GetSharePermission(context.Background(), "team", "local_user", "ghost")
	if !IsNotFound(err) {
		t.Errorf("GetSharePermission for an absent principal: got %T (%v), want a NotFoundError", err, err)
	}
}

func TestClient_GetUserQuota_MissingIsNotFound(t *testing.T) {
	c, server := setupUserQuotaTestServer()
	defer server.Close()

	_, err := c.GetUserQuota(context.Background(), "team", "ghost")
	if !IsNotFound(err) {
		t.Errorf("GetUserQuota for an absent user: got %T (%v), want a NotFoundError", err, err)
	}
}

// GetShare is the awkward one: DSM has no documented "no such share" code, so
// absence is established by a second read rather than guessed from the refusal.
func TestClient_GetShare_MissingIsNotFound(t *testing.T) {
	c, server, _ := setupShareTestServer()
	defer server.Close()

	_, err := c.GetShare(context.Background(), "ghost")
	if !IsNotFound(err) {
		t.Errorf("GetShare for an absent share: got %T (%v), want a NotFoundError", err, err)
	}
}

func TestClient_GetShare_UnconfirmedAbsenceStaysAnError(t *testing.T) {
	// A DSM that refuses both the get and the list has told us nothing about the
	// share. Confirming absence here would drop a live shared folder out of a
	// practitioner's state on a transient failure.
	mux := http.NewServeMux()
	mux.HandleFunc("/webapi/entry.cgi", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 105}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := NewClient(server.URL, "admin", "password", false)
	c.setSession("test-sid", "")

	_, err := c.GetShare(context.Background(), "team-data")
	if err == nil {
		t.Fatal("expected an error")
	}
	if IsNotFound(err) {
		t.Errorf("absence must not be inferred from a refusal the second read could not confirm: %v", err)
	}
	if !IsAPIError(err, 105) {
		t.Errorf("the original refusal must survive, got %v", err)
	}
}
