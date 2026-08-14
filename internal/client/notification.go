package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
)

// SYNO.Core.Notification.Mail is only partially documented. The wire contract
// below follows the stevefulme1/ansible-synology-dsm dsm_mail module, which is
// in production use against DSM 7: flat form parameters on set, GET-friendly
// read of the same names. The separate Mail.Auth API exists too, but Mail.set
// accepts the credentials directly, so one call is enough.
const (
	notificationMailAPI = "SYNO.Core.Notification.Mail"
)

// NotificationMailConfig is the outgoing-mail configuration DSM uses for every
// notification it sends: task scheduler failures, storage degradation, security
// advisories.
type NotificationMailConfig struct {
	Enabled      bool
	SMTPServer   string
	SMTPPort     int
	Sender       string
	UseTLS       bool
	SMTPAuth     bool
	SMTPUsername string
	// SMTPPassword is write-only: DSM does not return it, so the configured
	// value is the only record, exactly like certificate key material.
	SMTPPasswordSet bool
}

// GetNotificationMail reads the outgoing mail configuration.
func (c *Client) GetNotificationMail(ctx context.Context) (*NotificationMailConfig, error) {
	body, err := c.DoAPI(ctx, notificationMailAPI, "1", "get", url.Values{})
	if err != nil {
		return nil, fmt.Errorf("read mail notification settings: %w", err)
	}

	var entry map[string]interface{}
	if err := json.Unmarshal(body, &entry); err != nil {
		return nil, fmt.Errorf("decode mail notification settings: %w", err)
	}

	config := &NotificationMailConfig{
		Enabled:      flexibleBool(entry["enable"]),
		SMTPServer:   stringValue(entry, "smtp_server"),
		SMTPPort:     int(flexibleInt(entry["smtp_port"], 587)),
		Sender:       stringValue(entry, "sender"),
		UseTLS:       flexibleBool(entry["use_tls"]),
		SMTPAuth:     flexibleBool(entry["smtp_auth"]),
		SMTPUsername: stringValue(entry, "smtp_username"),
	}
	return config, nil
}

// SetNotificationMailRequest is an outgoing-mail write. Fields left at their
// zero value are omitted from the request; the caller decides between a full
// object and a partial one.
type SetNotificationMailRequest struct {
	Enable       bool
	SMTPServer   string
	SMTPPort     int
	Sender       string
	UseTLS       bool
	SMTPAuth     bool
	SMTPUsername string
	// SMTPPassword, when non-empty, is sent as smtp_password. DSM never returns
	// it, so there is no read-back: a non-empty configured value is always
	// pushed on every write that carries credentials.
	SMTPPassword string
}

// SetNotificationMail writes the outgoing mail configuration. The whole
// configuration is sent on every write rather than a diff: DSM keeps no
// transaction here, and a half-written SMTP setup fails loudly at exactly the
// moment a notification is needed.
func (c *Client) SetNotificationMail(ctx context.Context, req SetNotificationMailRequest) error {
	params := url.Values{}
	params.Set("enable", boolParam(req.Enable))
	if req.SMTPServer != "" {
		params.Set("smtp_server", req.SMTPServer)
		params.Set("smtp_port", strconv.Itoa(req.SMTPPort))
		params.Set("sender", req.Sender)
		params.Set("use_tls", boolParam(req.UseTLS))
	}
	if req.SMTPAuth {
		params.Set("smtp_auth", boolParam(true))
		params.Set("smtp_username", req.SMTPUsername)
		if req.SMTPPassword != "" {
			params.Set("smtp_password", req.SMTPPassword)
		}
	} else {
		params.Set("smtp_auth", boolParam(false))
	}

	if _, err := c.DoAPIPost(ctx, notificationMailAPI, "1", "set", params); err != nil {
		return fmt.Errorf("write mail notification settings: %w", err)
	}
	return nil
}

// flexibleInt accepts the several ways DSM renders an integer: real JSON
// number, numeric string.
func flexibleInt(value interface{}, fallback int) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case string:
		if parsed, err := strconv.ParseInt(typed, 10, 64); err == nil {
			return parsed
		}
	case int:
		return int64(typed)
	case int64:
		return typed
	}
	return int64(fallback)
}
