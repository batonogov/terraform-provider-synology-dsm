package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func newReverseProxyTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c := NewClient(server.URL, "admin", "password", false)
	c.setSession("test-sid", "test-token")
	return c, server
}

func writeReverseProxyData(w http.ResponseWriter, data interface{}) {
	raw, _ := json.Marshal(data)
	_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})
}

func writeReverseProxyError(w http.ResponseWriter, code int) {
	_ = json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: code}})
}

// capturedEntry is the payload shape confirmed against DSM 7.x: a single form
// value named "entry" holding the whole rule as a JSON document.
func decodeSentEntry(t *testing.T, r *http.Request) map[string]interface{} {
	t.Helper()
	if err := r.ParseForm(); err != nil {
		t.Fatalf("ParseForm: %v", err)
	}
	raw := r.FormValue("entry")
	if raw == "" {
		t.Fatal("request carried no entry parameter; DSM answers 4151 for a flattened payload")
	}
	var entry map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		t.Fatalf("entry is not JSON: %v (%q)", err, raw)
	}
	return entry
}

// storedEntry is a realistic DSM list item, matching the published DSM 7.x
// capture including the "_key" field DSM keeps for its own bookkeeping.
func storedEntry(uuid, description string) map[string]interface{} {
	return map[string]interface{}{
		"UUID":        uuid,
		"_key":        "dsm-internal-" + uuid,
		"description": description,
		"frontend": map[string]interface{}{
			"acl":      nil,
			"fqdn":     "cloud.example.com",
			"port":     443,
			"protocol": 1,
			"https":    map[string]interface{}{"hsts": true},
		},
		"backend": map[string]interface{}{
			"fqdn":     "localhost",
			"port":     8080,
			"protocol": 0,
		},
		"proxy_connect_timeout":  60,
		"proxy_read_timeout":     60,
		"proxy_send_timeout":     60,
		"proxy_http_version":     1,
		"proxy_intercept_errors": false,
		"customize_headers": []interface{}{
			map[string]interface{}{"name": "Upgrade", "value": "$http_upgrade"},
			map[string]interface{}{"name": "Connection", "value": "$connection_upgrade"},
			map[string]interface{}{"name": "X-Forwarded-Proto", "value": "$scheme"},
		},
	}
}

func sampleReverseProxy() ReverseProxy {
	return ReverseProxy{
		Description:     "Nextcloud",
		Source:          ReverseProxyEndpoint{Protocol: "HTTPS", Hostname: "cloud.example.com", Port: 443},
		Destination:     ReverseProxyEndpoint{Protocol: "HTTP", Hostname: "localhost", Port: 8080},
		HSTS:            true,
		CustomHeaders:   append(ReverseProxyWebSocketHeaders(), ReverseProxyHeader{Name: "X-Forwarded-Proto", Value: "$scheme"}),
		ConnectTimeout:  60,
		ReadTimeout:     60,
		SendTimeout:     60,
		InterceptErrors: false,
	}
}

func TestClient_ListReverseProxies_ParsesCapturedPayload(t *testing.T) {
	c, _ := newReverseProxyTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.FormValue("api"); got != "SYNO.Core.AppPortal.ReverseProxy" {
			t.Errorf("api = %q", got)
		}
		if got := r.FormValue("version"); got != "1" {
			t.Errorf("version = %q, want 1", got)
		}
		if got := r.FormValue("method"); got != "list" {
			t.Errorf("method param = %q, want list", got)
		}
		// The session must travel in the query string for POST, not the body.
		if got := r.URL.Query().Get("_sid"); got != "test-sid" {
			t.Errorf("_sid query param = %q, want test-sid", got)
		}
		writeReverseProxyData(w, map[string]interface{}{
			"entries": []interface{}{storedEntry("uuid-1", "Nextcloud")},
		})
	})

	entries, err := c.ListReverseProxies(context.Background())
	if err != nil {
		t.Fatalf("ListReverseProxies: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	entry := entries[0]
	if entry.UUID != "uuid-1" || entry.Description != "Nextcloud" {
		t.Errorf("identity = %q/%q", entry.UUID, entry.Description)
	}
	if entry.Source != (ReverseProxyEndpoint{Protocol: "HTTPS", Hostname: "cloud.example.com", Port: 443}) {
		t.Errorf("source = %+v", entry.Source)
	}
	if entry.Destination != (ReverseProxyEndpoint{Protocol: "HTTP", Hostname: "localhost", Port: 8080}) {
		t.Errorf("destination = %+v", entry.Destination)
	}
	if !entry.HSTS {
		t.Error("hsts should be true")
	}
	if entry.HTTP2 != nil {
		t.Errorf("http2 = %v, want nil when DSM omits the key", *entry.HTTP2)
	}
	if entry.ConnectTimeout != 60 || entry.ReadTimeout != 60 || entry.SendTimeout != 60 || entry.HTTPVersion != 1 {
		t.Errorf("timeouts/version = %+v", entry)
	}
	want := []ReverseProxyHeader{
		{Name: "Upgrade", Value: "$http_upgrade"},
		{Name: "Connection", Value: "$connection_upgrade"},
		{Name: "X-Forwarded-Proto", Value: "$scheme"},
	}
	if !reflect.DeepEqual(entry.CustomHeaders, want) {
		t.Errorf("custom headers = %+v, want %+v", entry.CustomHeaders, want)
	}
}

func TestClient_ReverseProxy_ParsesHTTP2AndACLWhenPresent(t *testing.T) {
	stored := storedEntry("uuid-1", "Nextcloud")
	frontend := stored["frontend"].(map[string]interface{})
	frontend["acl"] = "acl-uuid-9"
	frontend["https"] = map[string]interface{}{"hsts": true, "http2": true}

	c, _ := newReverseProxyTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeReverseProxyData(w, map[string]interface{}{"entries": []interface{}{stored}})
	})

	entry, err := c.GetReverseProxy(context.Background(), "uuid-1")
	if err != nil {
		t.Fatalf("GetReverseProxy: %v", err)
	}
	if entry.HTTP2 == nil || !*entry.HTTP2 {
		t.Errorf("http2 = %v, want a non-nil true", entry.HTTP2)
	}
	if entry.ACLProfileUUID != "acl-uuid-9" {
		t.Errorf("acl = %q", entry.ACLProfileUUID)
	}
}

func TestClient_CreateReverseProxy_SendsJSONEntry(t *testing.T) {
	var created map[string]interface{}
	var sent map[string]interface{}

	c, _ := newReverseProxyTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.FormValue("method") {
		case "list":
			entries := []interface{}{}
			if created != nil {
				entries = append(entries, created)
			}
			writeReverseProxyData(w, map[string]interface{}{"entries": entries})
		case "create":
			sent = decodeSentEntry(t, r)
			created = storedEntry("uuid-new", "Nextcloud")
			// DSM's create answers success without a usable UUID.
			writeReverseProxyData(w, map[string]interface{}{})
		default:
			t.Fatalf("unexpected method %q", r.FormValue("method"))
		}
	})

	entry := sampleReverseProxy()
	got, err := c.CreateReverseProxy(context.Background(), entry)
	if err != nil {
		t.Fatalf("CreateReverseProxy: %v", err)
	}
	if got.UUID != "uuid-new" {
		t.Errorf("read-back UUID = %q, want uuid-new", got.UUID)
	}

	if _, ok := sent["UUID"]; ok {
		t.Error("create must not send a UUID; DSM assigns it")
	}
	if sent["description"] != "Nextcloud" {
		t.Errorf("description = %v", sent["description"])
	}
	frontend, _ := sent["frontend"].(map[string]interface{})
	if frontend["fqdn"] != "cloud.example.com" || frontend["port"] != float64(443) {
		t.Errorf("frontend = %+v", frontend)
	}
	if frontend["protocol"] != float64(1) {
		t.Errorf("frontend protocol = %v, want 1 (HTTPS)", frontend["protocol"])
	}
	if acl, ok := frontend["acl"]; !ok || acl != nil {
		t.Errorf("frontend acl = %v (present=%v), want explicit null", acl, ok)
	}
	https, _ := frontend["https"].(map[string]interface{})
	if https["hsts"] != true {
		t.Errorf("frontend https = %+v", https)
	}
	if _, ok := https["http2"]; ok {
		t.Error("http2 must not be sent unless the caller asked for it: the key is inferred, not confirmed")
	}
	backend, _ := sent["backend"].(map[string]interface{})
	if backend["fqdn"] != "localhost" || backend["port"] != float64(8080) || backend["protocol"] != float64(0) {
		t.Errorf("backend = %+v", backend)
	}
	if sent["proxy_http_version"] != float64(1) {
		t.Errorf("proxy_http_version = %v, want DSM's default of 1", sent["proxy_http_version"])
	}
	headers, _ := sent["customize_headers"].([]interface{})
	if len(headers) != 3 {
		t.Fatalf("customize_headers = %+v, want 3 entries", headers)
	}
	first, _ := headers[0].(map[string]interface{})
	if first["name"] != "Upgrade" || first["value"] != "$http_upgrade" {
		t.Errorf("first header = %+v", first)
	}
}

func TestClient_CreateReverseProxy_SendsHTTP2WhenRequested(t *testing.T) {
	var sent map[string]interface{}
	created := false
	c, _ := newReverseProxyTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("method") == "create" {
			sent = decodeSentEntry(t, r)
			created = true
			// This DSM build does echo the new UUID, so no read-back by
			// description is needed.
			writeReverseProxyData(w, map[string]interface{}{"UUID": "uuid-new"})
			return
		}
		entries := []interface{}{}
		if created {
			entries = append(entries, storedEntry("uuid-new", "Nextcloud"))
		}
		writeReverseProxyData(w, map[string]interface{}{"entries": entries})
	})

	entry := sampleReverseProxy()
	enabled := true
	entry.HTTP2 = &enabled
	if _, err := c.CreateReverseProxy(context.Background(), entry); err != nil {
		t.Fatalf("CreateReverseProxy: %v", err)
	}

	frontend, _ := sent["frontend"].(map[string]interface{})
	https, _ := frontend["https"].(map[string]interface{})
	if https["http2"] != true {
		t.Errorf("frontend.https = %+v, want http2 true", https)
	}
}

func TestClient_CreateReverseProxy_RejectsDuplicateDescription(t *testing.T) {
	c, _ := newReverseProxyTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("method") != "list" {
			t.Fatalf("create must not be attempted for a duplicate description")
		}
		writeReverseProxyData(w, map[string]interface{}{"entries": []interface{}{storedEntry("uuid-1", "Nextcloud")}})
	})

	_, err := c.CreateReverseProxy(context.Background(), sampleReverseProxy())
	if err == nil {
		t.Fatal("expected a duplicate-description error")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %q, want it to mention the conflict", err)
	}
}

// A create whose response was lost can still have landed on DSM; the retry then
// reports 4154 for an entry this call itself created.
func TestClient_CreateReverseProxy_AdoptsEntryOn4154(t *testing.T) {
	listCalls := 0
	c, _ := newReverseProxyTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.FormValue("method") {
		case "list":
			listCalls++
			if listCalls == 1 {
				writeReverseProxyData(w, map[string]interface{}{"entries": []interface{}{}})
				return
			}
			writeReverseProxyData(w, map[string]interface{}{"entries": []interface{}{storedEntry("uuid-1", "Nextcloud")}})
		case "create":
			writeReverseProxyError(w, 4154)
		}
	})

	entry, err := c.CreateReverseProxy(context.Background(), sampleReverseProxy())
	if err != nil {
		t.Fatalf("CreateReverseProxy should adopt the entry DSM already stored: %v", err)
	}
	if entry.UUID != "uuid-1" {
		t.Errorf("UUID = %q, want uuid-1", entry.UUID)
	}
}

func TestClient_UpdateReverseProxy_PreservesUnmodelledFields(t *testing.T) {
	var sent map[string]interface{}
	c, _ := newReverseProxyTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.FormValue("method") {
		case "list":
			writeReverseProxyData(w, map[string]interface{}{"entries": []interface{}{storedEntry("uuid-1", "Nextcloud")}})
		case "update":
			sent = decodeSentEntry(t, r)
			writeReverseProxyData(w, map[string]interface{}{})
		}
	})

	entry := sampleReverseProxy()
	entry.UUID = "uuid-1"
	entry.Destination.Port = 9090
	if _, err := c.UpdateReverseProxy(context.Background(), entry); err != nil {
		t.Fatalf("UpdateReverseProxy: %v", err)
	}

	if sent["UUID"] != "uuid-1" {
		t.Errorf("update must target the entry by UUID, got %v", sent["UUID"])
	}
	if sent["_key"] != "dsm-internal-uuid-1" {
		t.Errorf("_key = %v, want the value DSM stored to survive the round trip", sent["_key"])
	}
	backend, _ := sent["backend"].(map[string]interface{})
	if backend["port"] != float64(9090) {
		t.Errorf("backend port = %v, want the updated 9090", backend["port"])
	}
}

func TestClient_UpdateReverseProxy_RequiresUUID(t *testing.T) {
	c, _ := newReverseProxyTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeReverseProxyData(w, map[string]interface{}{"entries": []interface{}{}})
	})
	if _, err := c.UpdateReverseProxy(context.Background(), sampleReverseProxy()); err == nil {
		t.Fatal("expected an error when UUID is missing")
	}
}

func TestClient_DeleteReverseProxy_SendsUUIDArray(t *testing.T) {
	var uuids string
	c, _ := newReverseProxyTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.FormValue("method") {
		case "list":
			writeReverseProxyData(w, map[string]interface{}{"entries": []interface{}{storedEntry("uuid-1", "Nextcloud")}})
		case "delete":
			uuids = r.FormValue("uuids")
			writeReverseProxyData(w, map[string]interface{}{})
		}
	})

	if err := c.DeleteReverseProxy(context.Background(), "uuid-1"); err != nil {
		t.Fatalf("DeleteReverseProxy: %v", err)
	}
	if uuids != `["uuid-1"]` {
		t.Errorf("uuids = %q, want a JSON array even for a single entry", uuids)
	}
}

func TestClient_DeleteReverseProxy_MissingEntryIsNoOp(t *testing.T) {
	c, _ := newReverseProxyTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("method") == "delete" {
			t.Fatal("delete must not be sent for an entry that is already gone")
		}
		writeReverseProxyData(w, map[string]interface{}{"entries": []interface{}{}})
	})
	if err := c.DeleteReverseProxy(context.Background(), "uuid-gone"); err != nil {
		t.Fatalf("deleting a missing entry should be a no-op, got %v", err)
	}
}

func TestClient_GetReverseProxy_NotFound(t *testing.T) {
	c, _ := newReverseProxyTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeReverseProxyData(w, map[string]interface{}{"entries": []interface{}{}})
	})
	if _, err := c.GetReverseProxy(context.Background(), "uuid-1"); !errors.Is(err, ErrReverseProxyNotFound) {
		t.Fatalf("err = %v, want ErrReverseProxyNotFound", err)
	}
	if _, err := c.GetReverseProxyByDescription(context.Background(), "Nextcloud"); !errors.Is(err, ErrReverseProxyNotFound) {
		t.Fatalf("err = %v, want ErrReverseProxyNotFound", err)
	}
}

func TestClient_AccessControlProfiles(t *testing.T) {
	c, _ := newReverseProxyTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if got := r.FormValue("api"); got != "SYNO.Core.AppPortal.AccessControl" {
			t.Errorf("api = %q", got)
		}
		writeReverseProxyData(w, map[string]interface{}{
			"entries": []interface{}{
				map[string]interface{}{"UUID": "acl-1", "name": "internal-only"},
				map[string]interface{}{"UUID": "acl-2", "name": "public"},
			},
		})
	})

	profile, err := c.FindAccessControlProfileByName(context.Background(), "internal-only")
	if err != nil {
		t.Fatalf("FindAccessControlProfileByName: %v", err)
	}
	if profile.UUID != "acl-1" {
		t.Errorf("UUID = %q, want acl-1", profile.UUID)
	}

	byUUID, err := c.FindAccessControlProfileByUUID(context.Background(), "acl-2")
	if err != nil {
		t.Fatalf("FindAccessControlProfileByUUID: %v", err)
	}
	if byUUID.Name != "public" {
		t.Errorf("name = %q, want public", byUUID.Name)
	}

	if _, err := c.FindAccessControlProfileByName(context.Background(), "missing"); !errors.Is(err, ErrAccessControlProfileNotFound) {
		t.Fatalf("err = %v, want ErrAccessControlProfileNotFound", err)
	}
}

func TestReverseProxyProtocolCodec(t *testing.T) {
	for _, input := range []string{"HTTPS", "https", " Https "} {
		code, err := EncodeReverseProxyProtocol(input)
		if err != nil || code != 1 {
			t.Errorf("EncodeReverseProxyProtocol(%q) = %d, %v", input, code, err)
		}
	}
	if code, err := EncodeReverseProxyProtocol("http"); err != nil || code != 0 {
		t.Errorf("http encoded to %d, %v", code, err)
	}
	if _, err := EncodeReverseProxyProtocol("tcp"); err == nil {
		t.Error("expected an error for an unsupported protocol")
	}
	if name, err := DecodeReverseProxyProtocol(1); err != nil || name != "HTTPS" {
		t.Errorf("DecodeReverseProxyProtocol(1) = %q, %v", name, err)
	}
	if _, err := DecodeReverseProxyProtocol(7); err == nil {
		t.Error("an unknown protocol code must be an error, not a silent HTTP default")
	}
}

// TestClient_CreateReverseProxy_Concurrent guards the reverseProxyMu contract.
//
// The API is per-entry rather than a whole-list set, but DSM keeps every entry
// in a single datastore that it rewrites on each mutation, and the client's own
// create is a list → create → list sequence. The fake below models that
// datastore faithfully: it snapshots the entry list, yields, then writes the
// snapshot back with the new entry appended. Without the client mutex,
// concurrent creates clobber each other and entries disappear.
func TestClient_CreateReverseProxy_Concurrent(t *testing.T) {
	var (
		mu      sync.Mutex
		entries []map[string]interface{}
		nextID  int
	)

	c, _ := newReverseProxyTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.FormValue("method") {
		case "list":
			mu.Lock()
			snapshot := make([]interface{}, 0, len(entries))
			for _, entry := range entries {
				snapshot = append(snapshot, entry)
			}
			mu.Unlock()
			writeReverseProxyData(w, map[string]interface{}{"entries": snapshot})
		case "create":
			sent := decodeSentEntry(t, r)

			mu.Lock()
			snapshot := append([]map[string]interface{}(nil), entries...)
			id := nextID
			nextID++
			mu.Unlock()

			// The window a concurrent writer would slip into.
			time.Sleep(time.Millisecond)

			sent["UUID"] = fmt.Sprintf("uuid-%d", id)
			mu.Lock()
			entries = append(snapshot, sent)
			mu.Unlock()

			writeReverseProxyData(w, map[string]interface{}{})
		default:
			t.Fatalf("unexpected method %q", r.FormValue("method"))
		}
	})

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	start := make(chan struct{})
	for i := range n {
		go func() {
			defer wg.Done()
			<-start
			entry := sampleReverseProxy()
			entry.Description = fmt.Sprintf("entry-%d", i)
			entry.Source.Hostname = fmt.Sprintf("app-%d.example.com", i)
			_, _ = c.CreateReverseProxy(context.Background(), entry)
		}()
	}
	close(start)
	wg.Wait()

	stored, err := c.ListReverseProxies(context.Background())
	if err != nil {
		t.Fatalf("ListReverseProxies: %v", err)
	}
	present := map[string]bool{}
	uuids := map[string]bool{}
	for _, entry := range stored {
		present[entry.Description] = true
		uuids[entry.UUID] = true
	}
	for i := range n {
		description := fmt.Sprintf("entry-%d", i)
		if !present[description] {
			t.Errorf("lost update: %q missing after concurrent create (have %d of %d)", description, len(stored), n)
		}
	}
	if len(uuids) != len(stored) {
		t.Errorf("got %d entries but only %d distinct UUIDs", len(stored), len(uuids))
	}
}
