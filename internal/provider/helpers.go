package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// dsmVolumePath matches an absolute volume path such as /volume1/docker. File
// Station APIs address the same directory as /docker, so a volume path in a
// share_path is always a mistake worth catching at plan time.
var dsmVolumePath = regexp.MustCompile(`^/volume[0-9]+(?:/|$)`)

// dsmProviderData is what the provider hands to every resource and data source
// through ProviderData. It is a struct rather than a bare *client.Client
// because some resources need provider-level configuration and not just an API
// client: dsm_scheduled_task and dsm_event_task are gated behind
// allow_task_execution, and a resource cannot reach the provider instance any
// other way.
type dsmProviderData struct {
	client *client.Client
	// allowTaskExecution mirrors the provider's allow_task_execution attribute.
	// It gates the task scheduler resources, which run arbitrary commands on the
	// NAS. See resource_scheduled_task.go for the reasoning.
	allowTaskExecution bool
}

// providerDataFrom unwraps ProviderData, reporting a diagnostic when the
// framework passes something unexpected. It returns nil when configuration
// cannot proceed, which includes the normal "provider not configured yet" case
// where ProviderData is nil.
func providerDataFrom(providerData any, diags *diag.Diagnostics) *dsmProviderData {
	if providerData == nil {
		return nil
	}
	data, ok := providerData.(*dsmProviderData)
	if !ok {
		diags.AddError(
			"Unexpected Provider Data",
			fmt.Sprintf("Expected *dsmProviderData, got: %T", providerData),
		)
		return nil
	}
	return data
}

// clientFromProviderData is the common case: a resource or data source that
// needs the API client and nothing else.
func clientFromProviderData(providerData any, diags *diag.Diagnostics) *client.Client {
	data := providerDataFrom(providerData, diags)
	if data == nil {
		return nil
	}
	return data.client
}

// privateChecksumValue encodes a checksum for the resource's private state,
// which the framework requires to be valid JSON.
//
// Private state is where a write-only resource keeps the checksum of what it
// last wrote: the configured value itself is never stored, so this is the only
// thing a later refresh can compare the remote object against.
func privateChecksumValue(checksum string) []byte {
	encoded, err := json.Marshal(checksum)
	if err != nil {
		// A string always marshals; the branch exists so the caller needs no
		// error path.
		return nil
	}
	return encoded
}

// parsePrivateChecksum reads back what privateChecksumValue stored. An absent or
// unreadable entry reports no checksum rather than an error: private state is a
// cache of the last write, and a resource that predates it must keep working.
func parsePrivateChecksum(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var checksum string
	if err := json.Unmarshal(raw, &checksum); err != nil {
		return ""
	}
	return checksum
}

// nullableString returns a null string when the value is empty, otherwise a
// string value. This normalizes the "" vs null mismatch for optional string
// attributes: DSM returns an empty string for attributes that were not set,
// but a Terraform config that omits such an attribute represents it as null.
// Without this normalization, refreshing a resource whose optional string was
// not configured produces a spurious "update in-place" diff of "" -> null.
//
// Tradeoff: because DSM cannot represent an intentionally-empty string vs an
// unset one, a config that explicitly sets description/email to "" will also
// be normalized to null and show a perpetual diff. This is the accepted
// tradeoff for fixing the far more common omitted-attribute case.
func nullableString(s string) types.String {
	if s == "" {
		return types.StringNull()
	}
	return types.StringValue(s)
}

// stringSliceFromList converts an optional list attribute to a Go slice,
// treating null and unknown as "not set" rather than as an empty list. For the
// firewall rule that distinction is not cosmetic: an omitted `ports` means every
// port, so a spurious empty list would read the same as the wildcard.
func stringSliceFromList(ctx context.Context, list types.List) ([]string, diag.Diagnostics) {
	if list.IsNull() || list.IsUnknown() {
		return nil, nil
	}
	var out []string
	diags := list.ElementsAs(ctx, &out, false)
	return out, diags
}

// stringListOrNull is the inverse: an empty slice becomes a null list, so a
// wildcard read back from DSM matches a config that simply omitted the argument
// instead of showing a permanent [] -> null diff.
func stringListOrNull(ctx context.Context, values []string) (types.List, diag.Diagnostics) {
	if len(values) == 0 {
		return types.ListNull(types.StringType), nil
	}
	return types.ListValueFrom(ctx, types.StringType, values)
}
