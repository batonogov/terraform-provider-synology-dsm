package provider

import (
	"context"
	"fmt"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	dschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

// Descriptions for the read-only POSIX attributes. They are shared verbatim by
// the file resource, the shared folder resource, and the shared folder data
// source, so the explanation of DSM's behaviour is written once.
//
// The explanation matters more than usual here: a practitioner who sees
// posix_mode = "000" needs to know both why DSM did that and that this provider
// cannot change it, or the obvious next step is to file the same bug again.
const (
	posixModeDescription = "POSIX mode of the path on disk, as DSM reports it — the digits are octal, so `\"755\"` " +
		"is `rwxr-xr-x` and `\"000\"` means no POSIX access at all. **Read-only:** no DSM API writes POSIX bits, " +
		"so this provider can show the mode but not set it. `\"000\"` together with `acl_mode = true` is DSM's " +
		"normal state for a folder whose access rules live in a Synology ACL; it breaks anything that consults " +
		"POSIX bits instead, notably Docker bind mounts. Null when File Station is unavailable or does not " +
		"report the path."
	posixOwnerDescription = "Name of the POSIX owner of the path on disk. Read-only."
	posixGroupDescription = "Name of the POSIX group of the path on disk. Read-only."
	posixUIDDescription   = "Numeric owner id of the path on disk. Read-only; useful for matching the uid a " +
		"container runs as."
	posixGIDDescription = "Numeric group id of the path on disk. Read-only."
	aclModeDescription  = "Whether the path takes its access rules from a Synology ACL rather than from POSIX " +
		"mode bits. When true, `posix_mode` is usually `\"000\"` and says nothing about who may actually read " +
		"the path through DSM — but it is exactly what a Docker bind mount enforces. Read-only."
)

// posixPermissionAttributes is the resource-schema half of the read-only block.
// The two halves are spelled out separately because the framework keeps
// resource and data source attributes in different packages; the descriptions
// are shared so the two cannot drift apart in wording.
func posixPermissionAttributes() map[string]rschema.Attribute {
	return map[string]rschema.Attribute{
		"posix_mode": rschema.StringAttribute{
			Computed:    true,
			Description: posixModeDescription,
		},
		"posix_owner": rschema.StringAttribute{
			Computed:    true,
			Description: posixOwnerDescription,
		},
		"posix_group": rschema.StringAttribute{
			Computed:    true,
			Description: posixGroupDescription,
		},
		"posix_uid": rschema.Int64Attribute{
			Computed:    true,
			Description: posixUIDDescription,
		},
		"posix_gid": rschema.Int64Attribute{
			Computed:    true,
			Description: posixGIDDescription,
		},
		"acl_mode": rschema.BoolAttribute{
			Computed:    true,
			Description: aclModeDescription,
		},
	}
}

// posixPermissionDataSourceAttributes is the data-source half of the same block.
func posixPermissionDataSourceAttributes() map[string]dschema.Attribute {
	return map[string]dschema.Attribute{
		"posix_mode": dschema.StringAttribute{
			Computed:    true,
			Description: posixModeDescription,
		},
		"posix_owner": dschema.StringAttribute{
			Computed:    true,
			Description: posixOwnerDescription,
		},
		"posix_group": dschema.StringAttribute{
			Computed:    true,
			Description: posixGroupDescription,
		},
		"posix_uid": dschema.Int64Attribute{
			Computed:    true,
			Description: posixUIDDescription,
		},
		"posix_gid": dschema.Int64Attribute{
			Computed:    true,
			Description: posixGIDDescription,
		},
		"acl_mode": dschema.BoolAttribute{
			Computed:    true,
			Description: aclModeDescription,
		},
	}
}

// posixPermissionsModel is the block of read-only attributes every resource
// that owns a path on disk exposes. Embedding it keeps the tfsdk tags, the
// schema, and the assignment in one place instead of three copies drifting.
type posixPermissionsModel struct {
	PosixMode  types.String `tfsdk:"posix_mode"`
	PosixOwner types.String `tfsdk:"posix_owner"`
	PosixGroup types.String `tfsdk:"posix_group"`
	PosixUID   types.Int64  `tfsdk:"posix_uid"`
	PosixGID   types.Int64  `tfsdk:"posix_gid"`
	ACLMode    types.Bool   `tfsdk:"acl_mode"`
}

// apply copies what DSM reported into the model. A nil argument clears every
// attribute to null, which is the honest answer when File Station could not be
// asked: reporting a stale mode would be worse than reporting none.
func (m *posixPermissionsModel) apply(permissions *client.PathPermissions) {
	if permissions == nil {
		m.PosixMode = types.StringNull()
		m.PosixOwner = types.StringNull()
		m.PosixGroup = types.StringNull()
		m.PosixUID = types.Int64Null()
		m.PosixGID = types.Int64Null()
		m.ACLMode = types.BoolNull()
		return
	}

	m.PosixMode = types.StringValue(posixModeString(permissions.PosixMode))
	m.PosixOwner = nullableString(permissions.Owner)
	m.PosixGroup = nullableString(permissions.Group)
	m.PosixUID = types.Int64Value(permissions.UID)
	m.PosixGID = types.Int64Value(permissions.GID)
	m.ACLMode = types.BoolValue(permissions.IsACLMode)
}

// posixModeString renders DSM's mode number the way a reader expects to see it.
// DSM prints the octal digits as a decimal number (777, 755, 0), so the value is
// formatted rather than converted, and padded to three digits so "000" does not
// arrive as "0". A path carrying setuid or sticky bits comes back with four
// digits and is left as is.
func posixModeString(mode int) string {
	return fmt.Sprintf("%03d", mode)
}

// readPathPermissions fetches the permissions of a path without letting a
// failure fail the caller.
//
// This is deliberately best effort. The attributes it feeds are informational,
// while File Station is a package that can be absent, stopped, or denied to the
// account in use — so a resource whose own API call succeeded must not be
// reported as broken because an extra read for a read-only attribute did not.
func readPathPermissions(ctx context.Context, c *client.Client, filePath string) *client.PathPermissions {
	permissions, err := c.GetPathPermissions(ctx, filePath)
	if err != nil {
		tflog.Debug(ctx, "Could not read POSIX permissions", map[string]interface{}{
			"path":  filePath,
			"error": err.Error(),
		})
		return nil
	}
	return permissions
}
