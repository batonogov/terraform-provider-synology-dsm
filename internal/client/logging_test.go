package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestRedactParams_HidesCredentialsAndSessions is the test that matters most in
// this file. Debug logs get pasted into GitHub issues -- that is what they are
// for -- so a log that carried a password or a live session id would turn a bug
// report into a credential leak.
func TestRedactParams_HidesCredentialsAndSessions(t *testing.T) {
	params := url.Values{}
	params.Set("api", "SYNO.API.Auth")
	params.Set("method", "login")
	params.Set("account", "terraform")
	params.Set("passwd", "hunter2")
	params.Set("password", "hunter2")
	params.Set("_sid", "AbCdEf123456")
	params.Set("SynoToken", "token-value")
	params.Set("SynoConfirmPWToken", "confirm-token")
	params.Set("encrypt_pwd", "share-secret")
	params.Set("bind_pw", "ldap-secret")

	got := redactParams(params)

	for _, secret := range []string{"hunter2", "AbCdEf123456", "token-value", "confirm-token", "share-secret", "ldap-secret"} {
		for key, value := range got {
			if rendered, ok := value.(string); ok && strings.Contains(rendered, secret) {
				t.Errorf("parameter %q leaked secret %q as %q", key, secret, rendered)
			}
		}
	}

	for _, key := range []string{"passwd", "password", "_sid", "SynoToken", "SynoConfirmPWToken", "encrypt_pwd", "bind_pw"} {
		if got[key] != redactedValue {
			t.Errorf("%s = %v, want %q", key, got[key], redactedValue)
		}
	}
}

// TestRedactParams_KeepsOrdinaryParameters guards the other direction. The
// parameters this provider gets wrong are the ordinary ones -- a JSON-quoted
// name, an adapter key DSM does not keep -- so redacting broadly would hide the
// bug the log exists to find.
func TestRedactParams_KeepsOrdinaryParameters(t *testing.T) {
	params := url.Values{}
	params.Set("api", "SYNO.Core.Security.Firewall.Profile")
	params.Set("method", "set")
	params.Set("profile", `{"name":"default","global":{"policy":"none","rules":[]}}`)
	params.Set("profile_applying", "false")

	got := redactParams(params)

	if got["profile"] != `{"name":"default","global":{"policy":"none","rules":[]}}` {
		t.Errorf("profile was not logged verbatim: %v", got["profile"])
	}
	if got["api"] != "SYNO.Core.Security.Firewall.Profile" {
		t.Errorf("api = %v", got["api"])
	}
}

// TestRedactParams_SummarisesDocuments covers the third case: a file's contents
// or a private key is neither a secret to redact by name nor something worth
// printing. Its size is the only part that helps diagnose an API contract.
func TestRedactParams_SummarisesDocuments(t *testing.T) {
	params := url.Values{}
	params.Set("content", strings.Repeat("x", 4096))
	params.Set("key", "-----BEGIN PRIVATE KEY-----\nMIIEv...")

	got := redactParams(params)

	content, _ := got["content"].(string)
	if !strings.HasPrefix(content, redactedValue) || !strings.Contains(content, "4096 bytes") {
		t.Errorf("content = %q, want a redacted summary naming its size", content)
	}
	key, _ := got["key"].(string)
	if strings.Contains(key, "BEGIN PRIVATE KEY") {
		t.Errorf("private key material reached the log: %q", key)
	}
}

func TestTruncate_SaysThatItTruncated(t *testing.T) {
	long := strings.Repeat("y", maxLoggedBodyBytes+100)

	got := truncate(long)

	if len(got) <= maxLoggedBodyBytes {
		t.Fatalf("truncate returned %d bytes, expected the cap plus a note", len(got))
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("a cut value must say it was cut: %q", got[len(got)-60:])
	}
	if short := truncate("small"); short != "small" {
		t.Errorf("truncate(%q) = %q, want it untouched", "small", short)
	}
}

// TestLooksLikeHTML covers the crash signature this provider has to name rather
// than surface as a JSON syntax error: DSM serves its own 404 page when
// synoscgi dies mid-request, which is a real failure mode of the firewall APIs.
func TestLooksLikeHTML(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"synology 404 page", "<!DOCTYPE html>\n<html>\n<head>", true},
		{"bare html", "<html><body>nope</body></html>", true},
		{"leading whitespace", "\n  <!doctype HTML><html>", true},
		{"api envelope", `{"success":true,"data":{}}`, false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksLikeHTML([]byte(tc.body)); got != tc.want {
				t.Errorf("looksLikeHTML(%q) = %t, want %t", tc.body, got, tc.want)
			}
		})
	}
}

// TestExecuteRequest_HTMLResponseIsNamedForWhatItIs is the end-to-end half:
// a DSM that crashes mid-request answers with a web page, and the error the
// caller sees must say so. Before this, the failure surfaced as
// "invalid character '<' looking for beginning of value", which reads like a
// provider bug rather than a NAS that fell over.
func TestExecuteRequest_HTMLResponseIsNamedForWhatItIs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("method") == "login" || r.FormValue("method") == "login" {
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: json.RawMessage(`{"sid":"s"}`)})
			return
		}
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Query().Get("crash") == "502" {
			w.WriteHeader(http.StatusBadGateway)
		}
		_, _ = w.Write([]byte("<!DOCTYPE html>\n<html><body>Sorry, the page you are looking for is not found.</body></html>"))
	}))
	defer server.Close()

	c := NewClient(server.URL, "admin", "password", false)
	if err := c.Login(t.Context()); err != nil {
		t.Fatalf("login: %v", err)
	}

	_, err := c.DoAPI(t.Context(), "SYNO.Core.Security.Firewall.Profile", "1", "set", url.Values{})

	if err == nil {
		t.Fatal("expected an error for an HTML response")
	}
	if !strings.Contains(err.Error(), "HTML page") || !strings.Contains(err.Error(), "crashes") {
		t.Errorf("error does not explain the crash: %v", err)
	}
	if !strings.Contains(err.Error(), "TF_LOG=DEBUG") {
		t.Errorf("error should point at the debug logs that now exist: %v", err)
	}
}

// TestExecuteRequest_HTMLWithErrorStatus is the shape a real DSM actually
// returns: HTTP 502 and a web page. Captured from virtual DSM 7.2.2, where a
// firewall profile carrying any non-empty rule array crashes synoscgi. It must
// not surface as a bare status code plus a page of HTML.
func TestExecuteRequest_HTMLWithErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("method") == "login" || r.FormValue("method") == "login" {
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: json.RawMessage(`{"sid":"s"}`)})
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<!DOCTYPE html>\n<html><body>error</body></html>"))
	}))
	defer server.Close()

	c := NewClient(server.URL, "admin", "password", false)
	if err := c.Login(t.Context()); err != nil {
		t.Fatalf("login: %v", err)
	}

	_, err := c.DoAPI(t.Context(), "SYNO.Core.Security.Firewall.Profile", "1", "set", url.Values{})

	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "crashes") {
		t.Errorf("error should name the status and the crash: %v", err)
	}
	if strings.Contains(err.Error(), "<!DOCTYPE") {
		t.Errorf("the HTML page itself must not be pasted into the error: %v", err)
	}
}
