package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
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

// TestSharedFolderValidateConfig_DescriptionLength covers #65: DSM caps a share
// description at 64 characters and refuses anything longer with a bare 3300,
// which reads as a concurrency problem. Catching it at plan time is the point.
func TestSharedFolderValidateConfig_DescriptionLength(t *testing.T) {
	tests := []struct {
		name        string
		description string
		wantError   bool
	}{
		{"empty", "", false},
		{"at the limit", strings.Repeat("a", 64), false},
		{"one over", strings.Repeat("a", 65), true},
		// The limit counts characters, not bytes: 64 Cyrillic characters are
		// 128 bytes and DSM accepts them.
		{"64 cyrillic characters are 128 bytes and fine", strings.Repeat("я", 64), false},
		{"65 cyrillic characters", strings.Repeat("я", 65), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewSharedFolderResource().(*sharedFolderResource)
			resp := &resource.ValidateConfigResponse{}
			r.ValidateConfig(t.Context(), resource.ValidateConfigRequest{
				Config: sharedFolderConfigWithDescription(t, tt.description),
			}, resp)

			if got := resp.Diagnostics.HasError(); got != tt.wantError {
				t.Fatalf("HasError = %v, want %v: %v", got, tt.wantError, resp.Diagnostics)
			}
			if tt.wantError && !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), "64 characters") {
				t.Errorf("the error should state the limit, got: %s", resp.Diagnostics.Errors()[0].Detail())
			}
		})
	}
}

// sharedFolderConfigWithDescription builds a minimal valid config carrying the
// given description, so the length rule can be exercised on its own.
func sharedFolderConfigWithDescription(t *testing.T, description string) tfsdk.Config {
	t.Helper()

	sch := sharedFolderSchema(t).Schema
	objType := sch.Type().TerraformType(context.Background()).(tftypes.Object)

	values := map[string]tftypes.Value{}
	for name, typ := range objType.AttributeTypes {
		switch name {
		case "name":
			values[name] = tftypes.NewValue(tftypes.String, "team-data")
		case "vol_path":
			values[name] = tftypes.NewValue(tftypes.String, "/volume1")
		case "description":
			values[name] = tftypes.NewValue(tftypes.String, description)
		default:
			values[name] = tftypes.NewValue(typ, nil)
		}
	}

	return tfsdk.Config{Schema: sch, Raw: tftypes.NewValue(objType, values)}
}
