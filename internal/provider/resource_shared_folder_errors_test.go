package provider

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
)

func TestSharedFolderErrorDetail(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		share    string
		contains []string
	}{
		{
			name:     "3301 explains the collision and how to adopt it",
			err:      fmt.Errorf("create share %q: %w", "containers", &client.APIError{Code: 3301, API: "SYNO.Core.Share"}),
			share:    "containers",
			contains: []string{"already exists", "3301", "terraform import dsm_shared_folder.containers containers"},
		},
		{
			name:     "105 names the account requirement",
			err:      fmt.Errorf("create share %q: %w", "data", &client.APIError{Code: 105, API: "SYNO.Core.Share"}),
			share:    "data",
			contains: []string{"permission", "administrator"},
		},
		{
			name:     "3328 is left to the client wording",
			err:      fmt.Errorf("create share %q: %w", "data", &client.APIError{Code: 3328, API: "SYNO.Core.Share"}),
			share:    "data",
			contains: []string{"another share operation was in progress"},
		},
		{
			name:     "transport errors pass through",
			err:      errors.New("http request: connection refused"),
			share:    "data",
			contains: []string{"connection refused"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sharedFolderErrorDetail(tt.err, tt.share)
			for _, want := range tt.contains {
				if !strings.Contains(got, want) {
					t.Errorf("detail should mention %q, got:\n%s", want, got)
				}
			}
			if !strings.Contains(got, tt.err.Error()) {
				t.Errorf("original message must survive, got:\n%s", got)
			}
		})
	}
}

// TestTerraformIdentifier covers share names DSM allows but Terraform does not
// accept verbatim in a resource address — the import hint has to stay copyable.
func TestTerraformIdentifier(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"containers", "containers"},
		{"s3-data", "s3-data"},
		{"my share", "my_share"},
		{"data.2024", "data_2024"},
		{"2024", "_2024"},
		{"-lead", "_-lead"},
		{"", "example"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := terraformIdentifier(tt.in); got != tt.want {
				t.Errorf("terraformIdentifier(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
