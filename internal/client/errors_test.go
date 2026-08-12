package client

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIError_Error(t *testing.T) {
	tests := []struct {
		name string
		err  *APIError
		want string
	}{
		{
			name: "api-specific code wins over the common table",
			err:  &APIError{Code: 3301, API: "SYNO.Core.Share"},
			want: "a share with this name already exists (code 3301)",
		},
		{
			name: "common code without an API",
			err:  &APIError{Code: 105},
			want: "the session does not have permission for this operation (code 105)",
		},
		{
			name: "common code for an API with its own table",
			err:  &APIError{Code: 105, API: "SYNO.Core.Share"},
			want: "the session does not have permission for this operation (code 105)",
		},
		{
			name: "unknown code stays diagnosable",
			err:  &APIError{Code: 1234, API: "SYNO.Core.Share"},
			want: "unexpected DSM error (code 1234)",
		},
		{
			// 3101 means "invalid home location" only for User.Home. Reporting that
			// sentence for another API would send the reader down the wrong path.
			name: "code is not borrowed across APIs",
			err:  &APIError{Code: 3101, API: "SYNO.Core.Share"},
			want: "unexpected DSM error (code 3101)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestAPIError_NoDuplication is the regression for the reported message
// "create share \"x\": api error 3328: synology api error: code 3328" — the code
// appeared twice and the meaning zero times.
func TestAPIError_NoDuplication(t *testing.T) {
	err := fmt.Errorf("create share %q: %w", "s3-data", &APIError{Code: 3328, API: "SYNO.Core.Share"})
	got := err.Error()

	if n := strings.Count(got, "3328"); n != 1 {
		t.Errorf("code should appear exactly once, appeared %d times: %s", n, got)
	}
	if strings.Contains(got, "synology api error") {
		t.Errorf("the old redundant prefix should be gone: %s", got)
	}
	if !strings.Contains(got, "another share operation was in progress") {
		t.Errorf("message should explain the code: %s", got)
	}
	if !strings.Contains(got, `create share "s3-data"`) {
		t.Errorf("caller context must survive: %s", got)
	}
}

func TestIsAPIError(t *testing.T) {
	apiErr := &APIError{Code: 3301, API: "SYNO.Core.Share"}

	tests := []struct {
		name  string
		err   error
		codes []int
		want  bool
	}{
		{"direct match", apiErr, []int{3301}, true},
		{"wrapped match", fmt.Errorf("create share: %w", apiErr), []int{3301}, true},
		{"doubly wrapped match", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", apiErr)), []int{3301}, true},
		{"one of several codes", apiErr, []int{3300, 3301, 3328}, true},
		{"no match", apiErr, []int{3300}, false},
		{"no codes given", apiErr, nil, false},
		{"not an API error", errors.New("connection refused"), []int{3301}, false},
		{"nil error", nil, []int{3301}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsAPIError(tt.err, tt.codes...); got != tt.want {
				t.Errorf("IsAPIError = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestExecuteRequest_TagsErrorWithAPI proves the API name is attached from the
// request, which is what makes codes above 3000 interpretable.
func TestExecuteRequest_TagsErrorWithAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":false,"error":{"code":3301}}`))
	}))
	defer server.Close()

	c := NewClient(server.URL, "admin", "", false)
	_, err := c.CreateShare(t.Context(), CreateShareRequest{Name: "existing", VolPath: "/volume1"})
	if err == nil {
		t.Fatal("expected an error")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error should unwrap to *APIError, got %T: %v", err, err)
	}
	if apiErr.API != "SYNO.Core.Share" {
		t.Errorf("API = %q, want SYNO.Core.Share", apiErr.API)
	}
	if !strings.Contains(err.Error(), "a share with this name already exists") {
		t.Errorf("message should explain 3301, got: %v", err)
	}
}
