package client

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestGetNotificationMail(t *testing.T) {
	c := newNotificationTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api") != notificationMailAPI || r.URL.Query().Get("method") != "get" {
			t.Errorf("unexpected call: %s %s", r.URL.Query().Get("api"), r.URL.Query().Get("method"))
		}
		writeNotificationResponse(w, map[string]interface{}{
			"enable":        true,
			"smtp_server":   "smtp.example.com",
			"smtp_port":     587,
			"sender":        "nas@example.com",
			"use_tls":       true,
			"smtp_auth":     true,
			"smtp_username": "nas@example.com",
		})
	})

	config, err := c.GetNotificationMail(t.Context())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !config.Enabled || config.SMTPServer != "smtp.example.com" || config.SMTPPort != 587 {
		t.Errorf("unexpected config: %+v", config)
	}
	if !config.SMTPAuth || config.SMTPUsername != "nas@example.com" {
		t.Errorf("auth not parsed: %+v", config)
	}
}

func TestGetNotificationMail_StringPortAndDefaults(t *testing.T) {
	c := newNotificationTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// DSM renders numbers inconsistently; a string port must parse, and a
		// missing port must fall back rather than become 0.
		writeNotificationResponse(w, map[string]interface{}{
			"enable":      "false",
			"smtp_server": "relay.local",
			"smtp_port":   "25",
			"sender":      "nas@local",
		})
	})

	config, err := c.GetNotificationMail(t.Context())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if config.Enabled {
		t.Error("string false must parse as disabled")
	}
	if config.SMTPPort != 25 {
		t.Errorf("string port 25 not parsed: %d", config.SMTPPort)
	}
	if config.SMTPAuth {
		t.Error("absent smtp_auth must default to false")
	}
}

func TestSetNotificationMail_RequestShape(t *testing.T) {
	var form url.Values

	c := newNotificationTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		form = r.PostForm
		writeNotificationResponse(w, map[string]interface{}{})
	})

	err := c.SetNotificationMail(t.Context(), SetNotificationMailRequest{
		Enable:       true,
		SMTPServer:   "smtp.example.com",
		SMTPPort:     465,
		Sender:       "nas@example.com",
		UseTLS:       true,
		SMTPAuth:     true,
		SMTPUsername: "nas@example.com",
		SMTPPassword: "app-password",
	})
	if err != nil {
		t.Fatalf("set: %v", err)
	}

	want := map[string]string{
		"enable":        "true",
		"smtp_server":   "smtp.example.com",
		"smtp_port":     "465",
		"sender":        "nas@example.com",
		"use_tls":       "true",
		"smtp_auth":     "true",
		"smtp_username": "nas@example.com",
		"smtp_password": "app-password",
	}
	for key, value := range want {
		if got := form.Get(key); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
}

func TestSetNotificationMail_DisableOnly(t *testing.T) {
	var form url.Values

	c := newNotificationTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
		writeNotificationResponse(w, map[string]interface{}{})
	})

	if err := c.SetNotificationMail(t.Context(), SetNotificationMailRequest{Enable: false}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := form.Get("enable"); got != "false" {
		t.Errorf("enable = %q, want false", got)
	}
	if _, present := form["smtp_server"]; present {
		t.Error("a disable-only write must not carry server fields")
	}
	if got := form.Get("smtp_auth"); got != "false" {
		t.Errorf("smtp_auth = %q, want false (explicit)", got)
	}
}

func newNotificationTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c := NewClient(server.URL, "admin", "password", false)
	c.setSession("test-sid", "test-token")
	return c
}

func writeNotificationResponse(w http.ResponseWriter, data interface{}) {
	writeCertAPIResponse(w, data)
}
