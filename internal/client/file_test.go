package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newFileTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c := NewClient(server.URL, "admin", "password", false)
	c.setSession("test-sid", "test-token")
	return c, server
}

func writeFileAPIResponse(w http.ResponseWriter, data interface{}) {
	raw, _ := json.Marshal(data)
	_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})
}

func writeFileAPIError(w http.ResponseWriter, code int) {
	_ = json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: code}})
}

// uploadedPart is one multipart part, captured in the order DSM would see it.
type uploadedPart struct {
	name     string
	filename string
	value    string
}

func readUploadParts(t *testing.T, r *http.Request) []uploadedPart {
	t.Helper()
	reader, err := r.MultipartReader()
	if err != nil {
		t.Fatalf("request is not multipart: %v", err)
	}

	var parts []uploadedPart
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read multipart part: %v", err)
		}
		value, err := io.ReadAll(part)
		if err != nil {
			t.Fatalf("read multipart value: %v", err)
		}
		parts = append(parts, uploadedPart{name: part.FormName(), filename: part.FileName(), value: string(value)})
	}
	return parts
}

func partValue(parts []uploadedPart, name string) string {
	for _, part := range parts {
		if part.name == name {
			return part.value
		}
	}
	return ""
}

func TestClient_UploadFile_SendsMultipartBodyWithFileLast(t *testing.T) {
	const content = "{\"identities\":[{\"name\":\"nextcloud\"}]}\n"
	var parts []uploadedPart
	var query string

	client, _ := newFileTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if contentType := r.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "multipart/form-data") {
			t.Errorf("Content-Type = %q, want multipart/form-data", contentType)
		}
		query = r.URL.RawQuery
		parts = readUploadParts(t, r)
		writeFileAPIResponse(w, map[string]interface{}{"blSkip": false, "file": "s3.json"})
	})

	if err := client.UploadFile(context.Background(), "/containers/seaweedfs/conf", "s3.json", []byte(content)); err != nil {
		t.Fatalf("UploadFile failed: %v", err)
	}

	// DSM validates the session from the URL, never from the body.
	for _, want := range []string{"_sid=test-sid", "SynoToken=test-token", "api=SYNO.FileStation.Upload", "method=upload"} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q missing %q", query, want)
		}
	}

	if got := partValue(parts, "path"); got != "/containers/seaweedfs/conf" {
		t.Errorf("path = %q, want the raw destination directory (not JSON-quoted)", got)
	}
	if got := partValue(parts, "create_parents"); got != "true" {
		t.Errorf("create_parents = %q, want true", got)
	}
	if got := partValue(parts, "overwrite"); got != "true" {
		t.Errorf("overwrite = %q, want true", got)
	}
	if got := partValue(parts, "api"); got != "SYNO.FileStation.Upload" {
		t.Errorf("api field = %q", got)
	}
	if got := partValue(parts, "version"); got != "2" {
		t.Errorf("version field = %q", got)
	}
	if got := partValue(parts, "method"); got != "upload" {
		t.Errorf("method field = %q", got)
	}

	// DSM's uploader parses the stream sequentially and ignores anything after
	// the file, so the file part must be last and must carry the target name.
	last := parts[len(parts)-1]
	if last.name != "file" {
		t.Fatalf("last part = %q, want the file part last", last.name)
	}
	if last.filename != "s3.json" {
		t.Errorf("file part filename = %q, want s3.json (DSM takes the destination name from here)", last.filename)
	}
	if last.value != content {
		t.Errorf("uploaded content = %q, want %q", last.value, content)
	}
}

func TestClient_UploadFile_ReportsSkippedUpload(t *testing.T) {
	client, _ := newFileTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = readUploadParts(t, r)
		writeFileAPIResponse(w, map[string]interface{}{"blSkip": true, "file": "s3.json"})
	})

	err := client.UploadFile(context.Background(), "/containers/conf", "s3.json", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "skipped") {
		t.Fatalf("a skipped upload must not look like success, got %v", err)
	}
}

func TestClient_UploadFile_SurfacesAPIError(t *testing.T) {
	client, _ := newFileTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = readUploadParts(t, r)
		writeFileAPIError(w, 407)
	})

	err := client.UploadFile(context.Background(), "/containers/conf", "s3.json", []byte("x"))
	if !IsAPIError(err, 407) {
		t.Fatalf("expected API error 407, got %v", err)
	}
}

func TestClient_UploadFile_RejectsOversizedContent(t *testing.T) {
	client, _ := newFileTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("oversized content must be rejected before any request is sent")
	})

	err := client.UploadFile(context.Background(), "/containers/conf", "big.bin", bytes.Repeat([]byte("a"), maxManagedFileSize+1))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected a size limit error, got %v", err)
	}
}

// TestClient_UploadFile_RelogsInOnExpiredSession covers the retry path the
// multipart requests need on their own: they bypass executeRequest, so the
// session handling has to be re-established rather than inherited.
func TestClient_UploadFile_RelogsInOnExpiredSession(t *testing.T) {
	uploads := 0
	logins := 0

	client, _ := newFileTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api") == "SYNO.API.Auth" {
			logins++
			writeFileAPIResponse(w, map[string]interface{}{"sid": "fresh-sid", "synotoken": "fresh-token"})
			return
		}
		uploads++
		if uploads == 1 {
			writeFileAPIError(w, 119)
			return
		}
		if got := r.URL.Query().Get("_sid"); got != "fresh-sid" {
			t.Errorf("retry used _sid %q, want the re-issued session", got)
		}
		_ = readUploadParts(t, r)
		writeFileAPIResponse(w, map[string]interface{}{"blSkip": false})
	})

	if err := client.UploadFile(context.Background(), "/containers/conf", "s3.json", []byte("x")); err != nil {
		t.Fatalf("UploadFile failed: %v", err)
	}
	if logins != 1 || uploads != 2 {
		t.Errorf("logins = %d, uploads = %d; want a single re-login and one retry", logins, uploads)
	}
}

func TestClient_GetFileInfo_ParsesEntry(t *testing.T) {
	client, _ := newFileTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if r.Method != http.MethodGet || query.Get("api") != "SYNO.FileStation.List" || query.Get("method") != "getinfo" {
			t.Fatalf("unexpected request: %s %s %s", r.Method, query.Get("api"), query.Get("method"))
		}
		if got := query.Get("path"); got != `["/containers/conf/s3.json"]` {
			t.Errorf("path = %q, want a JSON array of paths", got)
		}
		writeFileAPIResponse(w, map[string]interface{}{"files": []map[string]interface{}{{
			"path": "/containers/conf/s3.json", "name": "s3.json", "isdir": false,
			"additional": map[string]interface{}{"size": 42},
		}}})
	})

	info, err := client.GetFileInfo(context.Background(), "/containers/conf/s3.json")
	if err != nil {
		t.Fatalf("GetFileInfo failed: %v", err)
	}
	if info.Path != "/containers/conf/s3.json" || info.Name != "s3.json" || info.Size != 42 || info.IsDir {
		t.Errorf("unexpected info: %+v", info)
	}
}

// TestClient_GetFileInfo_NotFound covers both shapes DSM uses for a missing
// path: a failed envelope, and a successful envelope whose entry carries the
// per-file code instead.
func TestClient_GetFileInfo_NotFound(t *testing.T) {
	tests := []struct {
		name  string
		write func(http.ResponseWriter)
	}{
		{
			name:  "api error",
			write: func(w http.ResponseWriter) { writeFileAPIError(w, 408) },
		},
		{
			name: "per-entry code",
			write: func(w http.ResponseWriter) {
				writeFileAPIResponse(w, map[string]interface{}{"files": []map[string]interface{}{{
					"path": "/containers/conf/s3.json", "code": 408,
				}}})
			},
		},
		{
			name: "empty file list",
			write: func(w http.ResponseWriter) {
				writeFileAPIResponse(w, map[string]interface{}{"files": []map[string]interface{}{}})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, _ := newFileTestClient(t, func(w http.ResponseWriter, _ *http.Request) { tt.write(w) })
			_, err := client.GetFileInfo(context.Background(), "/containers/conf/s3.json")
			if !errors.Is(err, ErrFileNotFound) {
				t.Fatalf("expected ErrFileNotFound, got %v", err)
			}
		})
	}
}

func TestClient_GetFileInfo_ReportsDirectory(t *testing.T) {
	client, _ := newFileTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeFileAPIResponse(w, map[string]interface{}{"files": []map[string]interface{}{{
			"path": "/containers/conf", "name": "conf", "isdir": true,
		}}})
	})

	info, err := client.GetFileInfo(context.Background(), "/containers/conf")
	if err != nil {
		t.Fatalf("GetFileInfo failed: %v", err)
	}
	if !info.IsDir {
		t.Error("isdir must survive parsing so callers can refuse to manage a directory")
	}
}

func TestClient_DownloadFile_ReturnsRawBytes(t *testing.T) {
	// A JSON payload proves the download is not mistaken for an API envelope.
	const content = `{"success": false, "identities": []}`

	client, _ := newFileTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("api") != "SYNO.FileStation.Download" || query.Get("mode") != "download" {
			t.Fatalf("unexpected download request: %s", r.URL.RawQuery)
		}
		if got := query.Get("path"); got != `["/containers/conf/s3.json"]` {
			t.Errorf("path = %q, want a JSON array of paths", got)
		}
		w.Header().Set("Content-Disposition", `attachment; filename="s3.json"`)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte(content))
	})

	data, err := client.DownloadFile(context.Background(), "/containers/conf/s3.json")
	if err != nil {
		t.Fatalf("DownloadFile failed: %v", err)
	}
	if string(data) != content {
		t.Errorf("content = %q, want %q", data, content)
	}
}

func TestClient_DownloadFile_SurfacesAPIError(t *testing.T) {
	client, _ := newFileTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeFileAPIError(w, 408)
	})

	_, err := client.DownloadFile(context.Background(), "/containers/conf/s3.json")
	if !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("expected ErrFileNotFound, got %v", err)
	}
}

func TestClient_DeleteFile(t *testing.T) {
	var query string
	client, _ := newFileTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		if got := r.URL.Query().Get("path"); got != `["/containers/conf/s3.json"]` {
			t.Errorf("path = %q, want a JSON array of paths", got)
		}
		writeFileAPIResponse(w, map[string]interface{}{})
	})

	if err := client.DeleteFile(context.Background(), "/containers/conf/s3.json"); err != nil {
		t.Fatalf("DeleteFile failed: %v", err)
	}
	if !strings.Contains(query, "api=SYNO.FileStation.Delete") || !strings.Contains(query, "method=delete") {
		t.Errorf("unexpected delete request: %s", query)
	}
}

// TestClient_DeleteFile_IsNotRecursive pins down a safety property rather than
// a DSM requirement: this client manages single files, so a delete must never
// be able to empty a directory tree — which is what would happen if the path
// pointed at a directory after an out-of-band change.
func TestClient_DeleteFile_IsNotRecursive(t *testing.T) {
	var recursive string
	client, _ := newFileTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		recursive = r.URL.Query().Get("recursive")
		writeFileAPIResponse(w, map[string]interface{}{})
	})

	if err := client.DeleteFile(context.Background(), "/containers/conf/s3.json"); err != nil {
		t.Fatalf("DeleteFile failed: %v", err)
	}
	if recursive != "false" {
		t.Errorf("recursive = %q, want false: a file resource must not delete directory trees", recursive)
	}
}

func TestClient_DeleteFile_MissingFileIsReportedAsNotFound(t *testing.T) {
	client, _ := newFileTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeFileAPIError(w, 408)
	})

	err := client.DeleteFile(context.Background(), "/containers/conf/s3.json")
	if !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("expected ErrFileNotFound, got %v", err)
	}
}

func TestFilePath(t *testing.T) {
	tests := []struct {
		dir, name, want string
	}{
		{"/containers/conf", "s3.json", "/containers/conf/s3.json"},
		{"/containers/conf/", "s3.json", "/containers/conf/s3.json"},
		{"/containers", "Caddyfile", "/containers/Caddyfile"},
	}
	for _, tt := range tests {
		if got := FilePath(tt.dir, tt.name); got != tt.want {
			t.Errorf("FilePath(%q, %q) = %q, want %q", tt.dir, tt.name, got, tt.want)
		}
	}
}
