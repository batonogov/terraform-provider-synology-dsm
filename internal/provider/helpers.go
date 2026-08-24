package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strconv"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
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

// privateChecksumStore is the part of the framework's private state that the
// write-only resources use. It is an interface because the concrete type lives
// in an internal framework package, which a provider cannot name — and because
// a test can then supply its own store.
type privateChecksumStore interface {
	SetKey(ctx context.Context, key string, value []byte) diag.Diagnostics
	GetKey(ctx context.Context, key string) ([]byte, diag.Diagnostics)
}

// rememberChecksum records the checksum of what a resource just wrote.
//
// Private state is where a write-only resource keeps it: the configured value
// itself is never stored, so this is the only thing a later refresh can compare
// the remote object against.
func rememberChecksum(ctx context.Context, store privateChecksumStore, key, checksum string, diags *diag.Diagnostics) {
	if privateStoreUnavailable(store) {
		return
	}
	diags.Append(store.SetKey(ctx, key, privateChecksumValue(checksum))...)
}

// lastChecksum reads back what rememberChecksum stored. An absent entry reports
// no checksum rather than an error: a resource created before this existed, or
// imported, simply has no reference point yet.
func lastChecksum(ctx context.Context, store privateChecksumStore, key string, diags *diag.Diagnostics) string {
	if privateStoreUnavailable(store) {
		return ""
	}
	raw, readDiags := store.GetKey(ctx, key)
	diags.Append(readDiags...)
	return parsePrivateChecksum(raw)
}

// privateStoreUnavailable reports the case a unit test creates and an RPC never
// does: a response whose private state was left nil. The framework's type
// tolerates that on read but answers a write with a diagnostic, and a typed nil
// inside an interface is not caught by a plain `== nil`.
func privateStoreUnavailable(store privateChecksumStore) bool {
	if store == nil {
		return true
	}
	value := reflect.ValueOf(store)
	return value.Kind() == reflect.Pointer && value.IsNil()
}

// privateChecksumValue encodes a checksum for private state, which the
// framework requires to be valid JSON. A checksum is hex, so quoting is all it
// takes — and json.Marshal's unreachable error branch is worth avoiding here,
// because an empty value would tell the framework to *delete* the key.
func privateChecksumValue(checksum string) []byte {
	return []byte(strconv.Quote(checksum))
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

// removeIfGone implements the half of Terraform's Read contract that this
// provider used to get wrong (issue #131): when the remote object no longer
// exists, Read drops the resource from state and returns no error, so the next
// plan schedules a re-create.
//
// The distinction it draws is the entire point, and it cuts both ways.
//
// Erroring on a deleted object is not a local failure. A plan refreshes every
// resource before it plans anything, so one resource whose Read fails aborts the
// whole plan — including the resources that would have corrected the drift — and
// the only way out is to edit the state file by hand.
//
// Removing a resource on any *other* failure is worse. A timeout, an expired
// session, a permission refusal or a malformed request says nothing about
// whether the object is there; treating those as "gone" would drop a live
// resource out of the practitioner's state and plan a re-create of something
// that already exists — which, for a shared folder or a firewall rule, is a
// destructive change nobody asked for. Those keep surfacing as errors, loudly.
//
// The dividing line lives in the client: only a *client.NotFoundError, built
// where DSM answered and the object was demonstrably not in the answer, reaches
// here. See client.NotFoundError.
//
// It reports whether it handled the error, so callers read:
//
//	if err != nil {
//		if removeIfGone(ctx, resp, err, "firewall rule") {
//			return
//		}
//		resp.Diagnostics.AddError("Failed to read firewall rule", err.Error())
//		return
//	}
func removeIfGone(ctx context.Context, resp *resource.ReadResponse, err error, kind string) bool {
	if !client.IsNotFound(err) {
		return false
	}

	tflog.Warn(ctx, "Removing a resource from state: it no longer exists in DSM", map[string]interface{}{
		"kind":   kind,
		"reason": err.Error(),
	})
	resp.State.RemoveResource(ctx)
	return true
}
