package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"
)

// ErrFileNotFound reports that a File Station path does not exist, so a caller
// can turn a missing file into "drop it from state" instead of a hard failure.
var ErrFileNotFound = errors.New("file not found")

// fileStationNoSuchFile is File Station's "no such file or directory" code. It
// arrives either as a regular API error or, for getinfo, as a per-entry code
// inside an otherwise successful response.
const fileStationNoSuchFile = 408

// maxManagedFileSize bounds both directions of a transfer. The content of a
// managed file is kept in Terraform state, so this resource is meant for
// configuration-sized payloads; refusing larger files with a clear message
// beats silently bloating state or exhausting memory during a refresh.
const maxManagedFileSize = 16 << 20

// FileInfo is the File Station metadata needed to decide whether a managed file
// still exists and is still a file.
type FileInfo struct {
	Path  string
	Name  string
	Size  int64
	IsDir bool
	// Permissions is nil when DSM did not report the perm/owner block, which is
	// what happens when the caller did not ask for it or the DSM build does not
	// return it. A missing block is not an error: it means "unknown", and every
	// consumer of it is read-only.
	Permissions *PathPermissions
}

// PathPermissions is the ownership and POSIX mode DSM reports for a path.
//
// It is read-only on purpose: no DSM HTTP API writes POSIX bits or ownership.
// SYNO.FileStation.Property.set accepts a mode parameter under every spelling
// tried and changes nothing, SYNO.Core.ACL manages Synology ACLs rather than
// mode bits, and SYNO.FileStation.Property.ACLOwner exposes get only. The
// findings are in .pi/recon-posix-mode-2026-08-13.md.
type PathPermissions struct {
	// PosixMode is the mode as DSM prints it: the decimal digits are the octal
	// digits, so 755 means rwxr-xr-x and 0 means no POSIX access at all. It is
	// not a bitmask — do not compare it against 0o755.
	PosixMode int
	// IsACLMode reports that the path takes its access rules from a Synology
	// ACL. This is the usual reason PosixMode is 0: DSM keeps the real rules in
	// the ACL, and anything that only consults POSIX bits — a Docker bind mount,
	// for one — sees no access.
	IsACLMode bool
	Owner     string
	Group     string
	UID       int64
	GID       int64
}

// UploadFile writes content to dirPath/name, creating the parent directories
// and replacing any existing file.
//
// The destination name comes from the multipart filename, not from path: DSM
// treats the path field as the target *directory*. Unlike SYNO.FileStation.List
// and .Delete, which take a JSON array of paths, the upload path is a raw form
// value.
func (c *Client) UploadFile(ctx context.Context, dirPath, name string, content []byte) error {
	if len(content) > maxManagedFileSize {
		return fmt.Errorf("file %q is %d bytes, which exceeds the %d byte limit for Terraform-managed files", name, len(content), maxManagedFileSize)
	}

	fields := url.Values{}
	fields.Set("path", dirPath)
	fields.Set("create_parents", "true")
	// Without overwrite DSM fails with 414 as soon as the file exists, which
	// would make every update fail; Terraform owns the file, so replacing it is
	// always the intent.
	fields.Set("overwrite", "true")

	data, err := c.uploadRequest(ctx, "SYNO.FileStation.Upload", "2", "upload", fields, name, content)
	if err != nil {
		return fmt.Errorf("upload file %q to %q: %w", name, dirPath, err)
	}

	// DSM reports a refused upload inside a successful envelope (blSkip) rather
	// than as an error, so a silent no-op would otherwise look like success.
	var result struct {
		Skipped bool `json:"blSkip"`
	}
	if len(data) > 0 && json.Unmarshal(data, &result) == nil && result.Skipped {
		return fmt.Errorf("upload file %q to %q: DSM skipped the upload", name, dirPath)
	}
	return nil
}

// GetFileInfo reads File Station metadata for a single path, including the
// ownership and POSIX mode DSM reports for it.
func (c *Client) GetFileInfo(ctx context.Context, filePath string) (*FileInfo, error) {
	params := url.Values{}
	params.Set("path", jsonStringArray(filePath))
	params.Set("additional", `["size","type","perm","owner"]`)

	data, err := c.DoAPI(ctx, "SYNO.FileStation.List", "2", "getinfo", params)
	if err != nil {
		if IsAPIError(err, fileStationNoSuchFile) {
			return nil, fmt.Errorf("%w: %q", ErrFileNotFound, filePath)
		}
		return nil, fmt.Errorf("get file info %q: %w", filePath, err)
	}

	info, err := parseFileInfo(data, filePath)
	if err != nil {
		return nil, err
	}
	return info, nil
}

// DownloadFile returns the bytes DSM currently stores at filePath. Reading the
// content back is what makes out-of-band edits visible as drift.
func (c *Client) DownloadFile(ctx context.Context, filePath string) ([]byte, error) {
	params := url.Values{}
	params.Set("path", jsonStringArray(filePath))
	params.Set("mode", "download")

	var content []byte
	err := c.retryFileTransfer(ctx, func(ctx context.Context) error {
		var attemptErr error
		content, attemptErr = c.downloadAttempt(ctx, params)
		return attemptErr
	})
	if err != nil {
		if IsAPIError(err, fileStationNoSuchFile) {
			return nil, fmt.Errorf("%w: %q", ErrFileNotFound, filePath)
		}
		return nil, fmt.Errorf("download file %q: %w", filePath, err)
	}
	return content, nil
}

// DeleteFile removes a single path. A path that is already gone is reported as
// ErrFileNotFound so callers can treat destroy as idempotent.
func (c *Client) DeleteFile(ctx context.Context, filePath string) error {
	params := url.Values{}
	params.Set("path", jsonStringArray(filePath))
	// Deliberately not recursive: this client manages single files, and a
	// recursive delete would empty a whole directory tree if the path ever
	// pointed at a directory — after someone replaced the file with one out of
	// band, for instance. DSM does not need the flag to remove a plain file, so
	// the only thing it could buy here is unintended data loss.
	params.Set("recursive", "false")

	if _, err := c.DoAPI(ctx, "SYNO.FileStation.Delete", "2", "delete", params); err != nil {
		if IsAPIError(err, fileStationNoSuchFile) {
			return fmt.Errorf("%w: %q", ErrFileNotFound, filePath)
		}
		return fmt.Errorf("delete file %q: %w", filePath, err)
	}
	return nil
}

// multipartPart is one part of a multipart body. An empty Filename makes it a
// plain text field; anything else makes it a file part.
//
// The parts are an ordered slice rather than a map because order is load
// bearing and differs per API: File Station needs every text field ahead of the
// file, while certificate import is known to work with the files first. Neither
// order is documented, so each caller states its own.
type multipartPart struct {
	field    string
	filename string
	value    []byte
}

func textPart(field, value string) multipartPart {
	return multipartPart{field: field, value: []byte(value)}
}

func filePart(field, filename string, content []byte) multipartPart {
	return multipartPart{field: field, filename: filename, value: content}
}

// uploadRequest performs a File Station upload: every text field first — the
// api/version/method triple included, because File Station reads them from the
// body as well as the URL — and the file part strictly last.
func (c *Client) uploadRequest(ctx context.Context, api, version, method string, fields url.Values, filename string, content []byte) (json.RawMessage, error) {
	textFields := url.Values{}
	maps.Copy(textFields, fields)
	textFields.Set("api", api)
	textFields.Set("version", version)
	textFields.Set("method", method)

	// Sorted so the wire format is deterministic and diffable in tests.
	parts := make([]multipartPart, 0, len(textFields)+1)
	for _, key := range slices.Sorted(maps.Keys(textFields)) {
		parts = append(parts, textPart(key, textFields.Get(key)))
	}
	// DSM's uploader consumes the stream sequentially and ignores anything that
	// follows the file content, so a trailing parameter would be silently lost.
	parts = append(parts, filePart("file", filename, content))

	return c.multipartRequest(ctx, api, version, method, parts)
}

// multipartRequest performs a multipart/form-data POST. The regular request
// path cannot be reused: it form-encodes every parameter, and the APIs that
// take file content (File Station uploads, certificate import) only accept it
// as a multipart part.
func (c *Client) multipartRequest(ctx context.Context, api, version, method string, parts []multipartPart) (json.RawMessage, error) {
	var data json.RawMessage
	err := c.retryFileTransfer(ctx, func(ctx context.Context) error {
		var attemptErr error
		data, attemptErr = c.uploadAttempt(ctx, api, version, method, parts)
		return attemptErr
	})
	return data, err
}

func (c *Client) uploadAttempt(ctx context.Context, api, version, method string, parts []multipartPart) (json.RawMessage, error) {
	body, contentType, err := buildUploadBody(parts)
	if err != nil {
		return nil, err
	}

	// api/version/method are repeated in the query string on purpose: DSM
	// dispatches the request from the URL and only then parses the body, so an
	// upload that carries them exclusively in the multipart body is rejected on
	// some DSM builds. _sid and SynoToken must be in the query string for the
	// same reason as every other POST (see executeRequest).
	query := url.Values{}
	query.Set("api", api)
	query.Set("version", version)
	query.Set("method", method)
	sid, token := c.session()
	if sid != "" {
		query.Set("_sid", sid)
	}
	if token != "" {
		query.Set("SynoToken", token)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/webapi/entry.cgi?"+query.Encode(), body)
	if err != nil {
		return nil, fmt.Errorf("create upload request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(payload))
	}

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read upload response: %w", err)
	}
	return decodeAPIEnvelope(payload, api)
}

// buildUploadBody writes the parts in the order the caller gave them.
func buildUploadBody(parts []multipartPart) (io.Reader, string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	for _, part := range parts {
		if part.filename == "" {
			if err := writer.WriteField(part.field, string(part.value)); err != nil {
				return nil, "", fmt.Errorf("write upload field %q: %w", part.field, err)
			}
			continue
		}
		formFile, err := writer.CreateFormFile(part.field, part.filename)
		if err != nil {
			return nil, "", fmt.Errorf("create upload file part %q: %w", part.field, err)
		}
		if _, err := formFile.Write(part.value); err != nil {
			return nil, "", fmt.Errorf("write upload content for %q: %w", part.field, err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("finalize upload body: %w", err)
	}
	return &body, writer.FormDataContentType(), nil
}

func (c *Client) downloadAttempt(ctx context.Context, params url.Values) ([]byte, error) {
	query := c.buildParams("SYNO.FileStation.Download", "2", "download", params)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/webapi/entry.cgi?"+query.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("create download request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(payload))
	}

	// Read one byte past the limit so an oversized file is reported rather than
	// truncated into state.
	content, err := io.ReadAll(io.LimitReader(resp.Body, maxManagedFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("read download response: %w", err)
	}
	if len(content) > maxManagedFileSize {
		return nil, fmt.Errorf("file is larger than the %d byte limit for Terraform-managed files", maxManagedFileSize)
	}

	if apiErr := downloadErrorEnvelope(resp, content); apiErr != nil {
		return nil, apiErr
	}
	return content, nil
}

// downloadErrorEnvelope distinguishes a failed download from a file that just
// happens to contain JSON. A real download is served as an attachment, so the
// presence of Content-Disposition settles it; only without that header is the
// body examined for an API error envelope.
func downloadErrorEnvelope(resp *http.Response, body []byte) error {
	if resp.Header.Get("Content-Disposition") != "" {
		return nil
	}
	var envelope struct {
		Success *bool     `json:"success"`
		Error   *APIError `json:"error,omitempty"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Success == nil || *envelope.Success {
		return nil
	}
	if envelope.Error != nil {
		envelope.Error.API = "SYNO.FileStation.Download"
		return envelope.Error
	}
	return errors.New("download returned success=false with no error details")
}

// retryFileTransfer applies the same backoff and single re-login policy as
// doRequestWithRetry to requests that cannot go through executeRequest, because
// their body is multipart or raw file content rather than a JSON envelope.
func (c *Client) retryFileTransfer(ctx context.Context, attempt func(context.Context) error) error {
	var lastErr error
	reloggedIn := false

	for i := range maxRetries {
		if i > 0 {
			delay := time.Duration(math.Pow(2, float64(i-1))) * retryBaseDelay
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		err := attempt(ctx)
		if err == nil {
			return nil
		}
		lastErr = err

		if isSessionExpiredError(err) {
			if reloggedIn {
				return err
			}
			if relErr := c.relogin(ctx); relErr != nil {
				return fmt.Errorf("re-login after expired session: %w (original: %v)", relErr, err)
			}
			reloggedIn = true
			continue
		}
		if isTransientError(err) {
			continue
		}
		return err
	}

	return fmt.Errorf("max retries exceeded: %w", lastErr)
}

func decodeAPIEnvelope(payload []byte, api string) (json.RawMessage, error) {
	var resp APIResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if !resp.Success {
		if resp.Error != nil {
			resp.Error.API = api
			return nil, resp.Error
		}
		return nil, errors.New("api returned success=false with no error details")
	}
	return resp.Data, nil
}

// parseFileInfo unpacks a getinfo response. DSM answers for a missing path with
// a successful envelope whose entry carries a per-file code instead of failing
// the whole request, so the entry has to be inspected.
func parseFileInfo(raw json.RawMessage, filePath string) (*FileInfo, error) {
	var result struct {
		Files []map[string]interface{} `json:"files"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("parse file info %q: %w", filePath, err)
	}
	if len(result.Files) == 0 {
		return nil, fmt.Errorf("%w: %q", ErrFileNotFound, filePath)
	}

	entry := result.Files[0]
	if code, ok := entry["code"].(float64); ok && code != 0 {
		if int(code) == fileStationNoSuchFile {
			return nil, fmt.Errorf("%w: %q", ErrFileNotFound, filePath)
		}
		return nil, fmt.Errorf("get file info %q: %w", filePath, &APIError{Code: int(code), API: "SYNO.FileStation.List"})
	}

	info := &FileInfo{
		Path:  stringValue(entry, "path"),
		Name:  stringValue(entry, "name"),
		IsDir: boolValue(entry, "isdir"),
	}
	if info.Path == "" {
		info.Path = filePath
	}
	if info.Name == "" {
		info.Name = path.Base(filePath)
	}
	if additional, ok := entry["additional"].(map[string]interface{}); ok {
		if size, ok := additional["size"].(float64); ok {
			info.Size = int64(size)
		}
		info.Permissions = parsePathPermissions(additional)
	}
	return info, nil
}

// parsePathPermissions reads the perm/owner block of a getinfo entry. It
// returns nil when neither is present, so "DSM did not say" stays
// distinguishable from "mode 000, owned by root" — which is exactly the state
// this data exists to make visible.
func parsePathPermissions(additional map[string]interface{}) *PathPermissions {
	perm, hasPerm := additional["perm"].(map[string]interface{})
	owner, hasOwner := additional["owner"].(map[string]interface{})
	if !hasPerm && !hasOwner {
		return nil
	}

	permissions := &PathPermissions{}
	if hasPerm {
		if posix, ok := perm["posix"].(float64); ok {
			permissions.PosixMode = int(posix)
		}
		if aclMode, ok := perm["is_acl_mode"].(bool); ok {
			permissions.IsACLMode = aclMode
		}
	}
	if hasOwner {
		permissions.Owner = stringValue(owner, "user")
		permissions.Group = stringValue(owner, "group")
		if uid, ok := owner["uid"].(float64); ok {
			permissions.UID = int64(uid)
		}
		if gid, ok := owner["gid"].(float64); ok {
			permissions.GID = int64(gid)
		}
	}
	return permissions
}

// GetPathPermissions reads the ownership and POSIX mode of any File Station
// path, including a shared folder root such as "/containers".
//
// It exists separately from GetFileInfo because a shared folder is managed
// through SYNO.Core.Share, which reports none of this: DSM's own share API has
// no notion of the POSIX bits its shares land on disk with.
func (c *Client) GetPathPermissions(ctx context.Context, filePath string) (*PathPermissions, error) {
	info, err := c.GetFileInfo(ctx, filePath)
	if err != nil {
		return nil, err
	}
	return info.Permissions, nil
}

func boolValue(object map[string]interface{}, key string) bool {
	value, _ := object[key].(bool)
	return value
}

// jsonStringArray renders paths the way File Station expects them: a JSON array
// of strings, even for a single path.
func jsonStringArray(values ...string) string {
	raw, _ := json.Marshal(values)
	return string(raw)
}

// FilePath joins a File Station directory and a file name into the absolute
// path DSM uses to identify the file.
func FilePath(dirPath, name string) string {
	return strings.TrimSuffix(path.Clean(dirPath), "/") + "/" + name
}
