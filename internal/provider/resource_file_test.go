package provider

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func fileResourceSchema(t *testing.T) schema.Schema {
	t.Helper()
	resp := resource.SchemaResponse{}
	NewFileResource().Schema(t.Context(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned errors: %v", resp.Diagnostics)
	}
	return resp.Schema
}

// fileObject builds a full resource object, leaving every attribute that is not
// named in values as null.
func fileObject(t *testing.T, sch schema.Schema, values map[string]tftypes.Value) tftypes.Value {
	t.Helper()
	objType := sch.Type().TerraformType(context.Background()).(tftypes.Object)
	attrs := map[string]tftypes.Value{}
	for name, typ := range objType.AttributeTypes {
		if value, ok := values[name]; ok {
			attrs[name] = value
			continue
		}
		attrs[name] = tftypes.NewValue(typ, nil)
	}
	return tftypes.NewValue(objType, attrs)
}

func tfString(value string) tftypes.Value {
	return tftypes.NewValue(tftypes.String, value)
}

func TestFileResource_Schema(t *testing.T) {
	sch := fileResourceSchema(t)
	if diags := sch.ValidateImplementation(t.Context()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %v", diags)
	}

	attrs := sch.GetAttributes()
	for _, name := range []string{"share_path", "name"} {
		if attr := attrs[name]; attr == nil || !attr.IsRequired() {
			t.Errorf("%s must be required", name)
		}
	}
	for _, name := range []string{"content", "content_base64", "content_wo", "content_base64_wo"} {
		attr := attrs[name]
		if attr == nil || !attr.IsOptional() {
			t.Errorf("%s must be optional: the four content attributes are alternatives", name)
			continue
		}
		if !attr.IsSensitive() {
			t.Errorf("%s must be sensitive: files carry credentials", name)
		}
	}
	// The write-only pair is what keeps a secret out of state; a lost WriteOnly
	// flag would silently start persisting it.
	for _, name := range []string{"content_wo", "content_base64_wo"} {
		attr := attrs[name]
		if attr == nil || !attr.IsWriteOnly() {
			t.Errorf("%s must be write-only", name)
			continue
		}
		if attr.IsComputed() {
			t.Errorf("%s must not be computed: Terraform cannot compute a value it never stores", name)
		}
	}
	if attr := attrs["content_wo_version"]; attr == nil || !attr.IsOptional() || attr.IsWriteOnly() {
		t.Error("content_wo_version must be an optional, non-write-only counter — it is the marker that survives in state")
	}
	for _, name := range []string{"id", "checksum", "size"} {
		if attr := attrs[name]; attr == nil || !attr.IsComputed() {
			t.Errorf("%s must be computed", name)
		}
	}
}

func TestFileResource_MetadataAndConfigure(t *testing.T) {
	r := NewFileResource().(*fileResource)
	metadata := &resource.MetadataResponse{}
	r.Metadata(t.Context(), resource.MetadataRequest{ProviderTypeName: "dsm"}, metadata)
	if metadata.TypeName != "dsm_file" {
		t.Errorf("type name = %q, want dsm_file", metadata.TypeName)
	}

	wrong := &resource.ConfigureResponse{}
	r.Configure(t.Context(), resource.ConfigureRequest{ProviderData: "wrong"}, wrong)
	if !wrong.Diagnostics.HasError() {
		t.Fatal("expected diagnostic for wrong provider data")
	}

	r = NewFileResource().(*fileResource)
	empty := &resource.ConfigureResponse{}
	r.Configure(t.Context(), resource.ConfigureRequest{}, empty)
	if empty.Diagnostics.HasError() || r.client != nil {
		t.Fatalf("nil provider data should be ignored: %v", empty.Diagnostics)
	}
}

func TestFileResource_ValidateConfig(t *testing.T) {
	sch := fileResourceSchema(t)
	tests := []struct {
		name      string
		values    map[string]tftypes.Value
		wantError bool
	}{
		{
			name:   "text content",
			values: map[string]tftypes.Value{"share_path": tfString("/containers/conf"), "name": tfString("s3.json"), "content": tfString("{}")},
		},
		{
			name:   "base64 content",
			values: map[string]tftypes.Value{"share_path": tfString("/containers/conf"), "name": tfString("cert.p12"), "content_base64": tfString("aGVsbG8=")},
		},
		{
			name:   "share root",
			values: map[string]tftypes.Value{"share_path": tfString("/containers"), "name": tfString("Caddyfile"), "content": tfString("x")},
		},
		{
			name:      "both content forms",
			values:    map[string]tftypes.Value{"share_path": tfString("/containers/conf"), "name": tfString("s3.json"), "content": tfString("{}"), "content_base64": tfString("aGVsbG8=")},
			wantError: true,
		},
		{
			name: "write-only content with version",
			values: map[string]tftypes.Value{"share_path": tfString("/containers/conf"), "name": tfString("s3.json"),
				"content_wo": tfString("{}"), "content_wo_version": tftypes.NewValue(tftypes.Number, 1)},
		},
		{
			name: "write-only base64 content with version",
			values: map[string]tftypes.Value{"share_path": tfString("/containers/conf"), "name": tfString("cert.p12"),
				"content_base64_wo": tfString("aGVsbG8="), "content_wo_version": tftypes.NewValue(tftypes.Number, 1)},
		},
		{
			name:      "write-only content without version",
			values:    map[string]tftypes.Value{"share_path": tfString("/containers/conf"), "name": tfString("s3.json"), "content_wo": tfString("{}")},
			wantError: true,
		},
		{
			name: "version without write-only content",
			values: map[string]tftypes.Value{"share_path": tfString("/containers/conf"), "name": tfString("s3.json"),
				"content": tfString("{}"), "content_wo_version": tftypes.NewValue(tftypes.Number, 1)},
			wantError: true,
		},
		{
			name: "plain and write-only content together",
			values: map[string]tftypes.Value{"share_path": tfString("/containers/conf"), "name": tfString("s3.json"),
				"content": tfString("{}"), "content_wo": tfString("{}"), "content_wo_version": tftypes.NewValue(tftypes.Number, 1)},
			wantError: true,
		},
		{
			name: "invalid write-only base64",
			values: map[string]tftypes.Value{"share_path": tfString("/containers/conf"), "name": tfString("cert.p12"),
				"content_base64_wo": tfString("not base64!"), "content_wo_version": tftypes.NewValue(tftypes.Number, 1)},
			wantError: true,
		},
		{
			name:      "no content",
			values:    map[string]tftypes.Value{"share_path": tfString("/containers/conf"), "name": tfString("s3.json")},
			wantError: true,
		},
		{
			name:      "invalid base64",
			values:    map[string]tftypes.Value{"share_path": tfString("/containers/conf"), "name": tfString("cert.p12"), "content_base64": tfString("not base64!")},
			wantError: true,
		},
		{
			name:      "volume path",
			values:    map[string]tftypes.Value{"share_path": tfString("/volume1/containers/conf"), "name": tfString("s3.json"), "content": tfString("{}")},
			wantError: true,
		},
		{
			name:      "relative path",
			values:    map[string]tftypes.Value{"share_path": tfString("containers/conf"), "name": tfString("s3.json"), "content": tfString("{}")},
			wantError: true,
		},
		{
			name:      "trailing slash",
			values:    map[string]tftypes.Value{"share_path": tfString("/containers/conf/"), "name": tfString("s3.json"), "content": tfString("{}")},
			wantError: true,
		},
		{
			name:      "directory in name",
			values:    map[string]tftypes.Value{"share_path": tfString("/containers"), "name": tfString("conf/s3.json"), "content": tfString("{}")},
			wantError: true,
		},
		{
			name:      "empty name",
			values:    map[string]tftypes.Value{"share_path": tfString("/containers/conf"), "name": tfString(""), "content": tfString("{}")},
			wantError: true,
		},
	}

	r := NewFileResource().(*fileResource)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &resource.ValidateConfigResponse{}
			r.ValidateConfig(t.Context(), resource.ValidateConfigRequest{
				Config: tfsdk.Config{Schema: sch, Raw: fileObject(t, sch, tt.values)},
			}, resp)
			if tt.wantError != resp.Diagnostics.HasError() {
				t.Fatalf("HasError = %v, want %v: %v", resp.Diagnostics.HasError(), tt.wantError, resp.Diagnostics)
			}
		})
	}
}

// fileReadServer answers the two calls Read makes: getinfo for existence and
// download for the content that drift detection compares against.
func fileReadServer(t *testing.T, filePath string, content []byte, isDir bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("api") {
		case "SYNO.FileStation.List":
			raw, _ := json.Marshal(map[string]interface{}{"files": []map[string]interface{}{{
				"path": filePath, "name": filePath[strings.LastIndex(filePath, "/")+1:], "isdir": isDir,
				"additional": map[string]interface{}{
					"size": len(content),
					// The shape issue #94 reports: an ACL-mode path whose POSIX
					// bits are cleared, which is what a bind mount enforces.
					"perm":  map[string]interface{}{"posix": 0, "is_acl_mode": true},
					"owner": map[string]interface{}{"user": "terraform", "group": "users", "uid": 1027, "gid": 100},
				},
			}}})
			_, _ = w.Write(append(append([]byte(`{"success":true,"data":`), raw...), '}'))
		case "SYNO.FileStation.Download":
			w.Header().Set("Content-Disposition", `attachment; filename="file"`)
			_, _ = w.Write(content)
		default:
			t.Errorf("unexpected API call: %s", r.URL.RawQuery)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func readFileResource(t *testing.T, server *httptest.Server, prior map[string]tftypes.Value) (fileResourceModel, *resource.ReadResponse) {
	t.Helper()
	sch := fileResourceSchema(t)
	raw := fileObject(t, sch, prior)
	r := &fileResource{client: client.NewClient(server.URL, "admin", "", false)}

	resp := &resource.ReadResponse{State: tfsdk.State{Schema: sch, Raw: raw}}
	r.Read(t.Context(), resource.ReadRequest{State: tfsdk.State{Schema: sch, Raw: raw}}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read returned errors: %v", resp.Diagnostics)
	}

	var model fileResourceModel
	if resp.State.Raw.IsNull() {
		return model, resp
	}
	if diags := resp.State.Get(t.Context(), &model); diags.HasError() {
		t.Fatalf("reading state failed: %v", diags)
	}
	return model, resp
}

func sha256Hex(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// TestFileResource_Read_DetectsChecksumDrift is the point of downloading the
// file on refresh: an edit made outside Terraform has to land in state.
func TestFileResource_Read_DetectsChecksumDrift(t *testing.T) {
	const remote = "edited outside terraform\n"
	server := fileReadServer(t, "/containers/conf/s3.json", []byte(remote), false)

	model, _ := readFileResource(t, server, map[string]tftypes.Value{
		"id":         tfString("/containers/conf/s3.json"),
		"share_path": tfString("/containers/conf"),
		"name":       tfString("s3.json"),
		"content":    tfString("original\n"),
		"checksum":   tfString(sha256Hex("original\n")),
		"size":       tftypes.NewValue(tftypes.Number, 9),
	})

	if model.Content.ValueString() != remote {
		t.Errorf("content = %q, want the bytes DSM stores", model.Content.ValueString())
	}
	if model.Checksum.ValueString() != sha256Hex(remote) {
		t.Errorf("checksum = %q, want the checksum of the remote content", model.Checksum.ValueString())
	}
	if model.Checksum.ValueString() == sha256Hex("original\n") {
		t.Error("checksum must change when the file changed on DSM")
	}
	if model.Size.ValueInt64() != int64(len(remote)) {
		t.Errorf("size = %d, want %d", model.Size.ValueInt64(), len(remote))
	}
	if !model.ContentBase64.IsNull() {
		t.Error("content_base64 must stay null while the file is tracked as text")
	}
}

// TestFileResource_Read_PopulatesEveryFieldAfterImport covers the import path,
// where state carries nothing but the file path.
func TestFileResource_Read_PopulatesEveryFieldAfterImport(t *testing.T) {
	const remote = "{\"identities\":[]}\n"
	server := fileReadServer(t, "/containers/seaweedfs/conf/s3.json", []byte(remote), false)

	model, _ := readFileResource(t, server, map[string]tftypes.Value{
		"id": tfString("/containers/seaweedfs/conf/s3.json"),
	})

	if model.SharePath.ValueString() != "/containers/seaweedfs/conf" {
		t.Errorf("share_path = %q, want the directory derived from the import id", model.SharePath.ValueString())
	}
	if model.Name.ValueString() != "s3.json" {
		t.Errorf("name = %q, want s3.json", model.Name.ValueString())
	}
	if model.Content.ValueString() != remote {
		t.Errorf("content = %q, want the downloaded content", model.Content.ValueString())
	}
	if model.Checksum.ValueString() != sha256Hex(remote) || model.Size.ValueInt64() != int64(len(remote)) {
		t.Errorf("checksum/size not populated: %+v", model)
	}
}

// TestFileResource_Read_ReportsPosixPermissions covers the read-only block from
// issue #94. The values travel through an embedded struct in the model, which
// the framework only resolves by reflection — a rename that breaks the mapping
// shows up here as null attributes rather than at apply time on a real NAS.
func TestFileResource_Read_ReportsPosixPermissions(t *testing.T) {
	server := fileReadServer(t, "/containers/conf/s3.json", []byte("{}\n"), false)

	model, _ := readFileResource(t, server, map[string]tftypes.Value{
		"id": tfString("/containers/conf/s3.json"),
	})

	if model.PosixMode.ValueString() != "000" {
		t.Errorf("posix_mode = %q, want %q — a cleared mode must not render as \"0\"", model.PosixMode.ValueString(), "000")
	}
	if !model.ACLMode.ValueBool() {
		t.Error("acl_mode must report that the path takes its rules from a Synology ACL")
	}
	if model.PosixOwner.ValueString() != "terraform" || model.PosixGroup.ValueString() != "users" {
		t.Errorf("owner/group = %q/%q, want terraform/users", model.PosixOwner.ValueString(), model.PosixGroup.ValueString())
	}
	if model.PosixUID.ValueInt64() != 1027 || model.PosixGID.ValueInt64() != 100 {
		t.Errorf("uid/gid = %d/%d, want 1027/100", model.PosixUID.ValueInt64(), model.PosixGID.ValueInt64())
	}
}

// TestFileResource_Read_PosixPermissionsStayNullWhenUnreported keeps the block
// best effort: File Station can be absent or restricted, and a file this
// provider did manage to read must not be reported as broken because of it.
func TestFileResource_Read_PosixPermissionsStayNullWhenUnreported(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("api") {
		case "SYNO.FileStation.List":
			raw, _ := json.Marshal(map[string]interface{}{"files": []map[string]interface{}{{
				"path": "/containers/conf/s3.json", "name": "s3.json", "isdir": false,
				"additional": map[string]interface{}{"size": 3},
			}}})
			_, _ = w.Write(append(append([]byte(`{"success":true,"data":`), raw...), '}'))
		case "SYNO.FileStation.Download":
			w.Header().Set("Content-Disposition", `attachment; filename="file"`)
			_, _ = w.Write([]byte("{}\n"))
		default:
			t.Errorf("unexpected API call: %s", r.URL.RawQuery)
		}
	}))
	t.Cleanup(server.Close)

	model, _ := readFileResource(t, server, map[string]tftypes.Value{
		"id": tfString("/containers/conf/s3.json"),
	})

	if !model.PosixMode.IsNull() || !model.ACLMode.IsNull() || !model.PosixUID.IsNull() {
		t.Errorf("unreported permissions must stay null, got %+v", model.posixPermissionsModel)
	}
}

func TestFileResource_Read_KeepsBase64Form(t *testing.T) {
	remote := []byte{0x00, 0x01, 0xff}
	server := fileReadServer(t, "/containers/conf/cert.p12", remote, false)

	model, _ := readFileResource(t, server, map[string]tftypes.Value{
		"id":             tfString("/containers/conf/cert.p12"),
		"share_path":     tfString("/containers/conf"),
		"name":           tfString("cert.p12"),
		"content_base64": tfString(base64.StdEncoding.EncodeToString([]byte{0x00})),
	})

	if model.ContentBase64.ValueString() != base64.StdEncoding.EncodeToString(remote) {
		t.Errorf("content_base64 = %q, want the encoded remote bytes", model.ContentBase64.ValueString())
	}
	if !model.Content.IsNull() {
		t.Error("content must stay null while the file is tracked as base64")
	}
}

func TestFileResource_Read_RemovesMissingFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":false,"error":{"code":408}}`))
	}))
	defer server.Close()

	_, resp := readFileResource(t, server, map[string]tftypes.Value{
		"id":         tfString("/containers/conf/s3.json"),
		"share_path": tfString("/containers/conf"),
		"name":       tfString("s3.json"),
		"content":    tfString("x"),
	})

	if !resp.State.Raw.IsNull() {
		t.Error("a file deleted outside Terraform must be dropped from state, not reported as an error")
	}
}

func TestFileResource_Read_RefusesDirectory(t *testing.T) {
	sch := fileResourceSchema(t)
	server := fileReadServer(t, "/containers/conf", nil, true)
	raw := fileObject(t, sch, map[string]tftypes.Value{
		"id":         tfString("/containers/conf"),
		"share_path": tfString("/containers"),
		"name":       tfString("conf"),
		"content":    tfString("x"),
	})

	r := &fileResource{client: client.NewClient(server.URL, "admin", "", false)}
	resp := &resource.ReadResponse{State: tfsdk.State{Schema: sch, Raw: raw}}
	r.Read(t.Context(), resource.ReadRequest{State: tfsdk.State{Schema: sch, Raw: raw}}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error when the managed path is a directory")
	}
}

func TestFileContentBytes(t *testing.T) {
	null := types.StringNull()
	tests := []struct {
		name            string
		content         types.String
		contentBase64   types.String
		contentWO       types.String
		contentBase64WO types.String
		want            []byte
		wantError       bool
	}{
		{name: "text", content: types.StringValue("hello"), contentBase64: null, contentWO: null, contentBase64WO: null, want: []byte("hello")},
		{name: "base64", content: null, contentBase64: types.StringValue("AAH/"), contentWO: null, contentBase64WO: null, want: []byte{0x00, 0x01, 0xff}},
		{name: "empty file is valid", content: types.StringValue(""), contentBase64: null, contentWO: null, contentBase64WO: null, want: []byte{}},
		{name: "write-only text", content: null, contentBase64: null, contentWO: types.StringValue("s3cr3t"), contentBase64WO: null, want: []byte("s3cr3t")},
		{name: "write-only base64", content: null, contentBase64: null, contentWO: null, contentBase64WO: types.StringValue("AAH/"), want: []byte{0x00, 0x01, 0xff}},
		{name: "invalid base64", content: null, contentBase64: types.StringValue("!!"), contentWO: null, contentBase64WO: null, wantError: true},
		{name: "invalid write-only base64", content: null, contentBase64: null, contentWO: null, contentBase64WO: types.StringValue("!!"), wantError: true},
		{name: "nothing set", content: null, contentBase64: null, contentWO: null, contentBase64WO: null, wantError: true},
		// A write-only value reaches the provider only during apply. If it did not
		// arrive, uploading the empty string would silently truncate the file.
		{name: "unknown write-only value is not content", content: null, contentBase64: null, contentWO: types.StringUnknown(), contentBase64WO: null, wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fileContentBytes(tt.content, tt.contentBase64, tt.contentWO, tt.contentBase64WO)
			if tt.wantError {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("fileContentBytes failed: %v", err)
			}
			if string(got) != string(tt.want) {
				t.Errorf("bytes = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestFileResource_Create_WriteOnlyContentStaysOutOfState is the whole point of
// issue #104: the secret reaches DSM, and state keeps nothing but its checksum.
func TestFileResource_Create_WriteOnlyContentStaysOutOfState(t *testing.T) {
	const secret = `{"secretKey":"s3cr3t"}`
	var uploaded string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("api") {
		case "SYNO.FileStation.Upload":
			body, _ := io.ReadAll(r.Body)
			uploaded = string(body)
			_, _ = w.Write([]byte(`{"success":true,"data":{}}`))
		case "SYNO.FileStation.List":
			_, _ = w.Write([]byte(`{"success":true,"data":{"files":[{"path":"/containers/conf/s3.json","name":"s3.json","isdir":false}]}}`))
		default:
			t.Errorf("unexpected API call: %s", r.URL.RawQuery)
		}
	}))
	t.Cleanup(server.Close)

	sch := fileResourceSchema(t)
	// The plan is what Terraform hands a provider during apply: write-only
	// attributes are null in it, and only the configuration carries the value.
	plan := fileObject(t, sch, map[string]tftypes.Value{
		"share_path":         tfString("/containers/conf"),
		"name":               tfString("s3.json"),
		"content_wo_version": tftypes.NewValue(tftypes.Number, 1),
	})
	config := fileObject(t, sch, map[string]tftypes.Value{
		"share_path":         tfString("/containers/conf"),
		"name":               tfString("s3.json"),
		"content_wo":         tfString(secret),
		"content_wo_version": tftypes.NewValue(tftypes.Number, 1),
	})

	r := &fileResource{client: client.NewClient(server.URL, "admin", "", false)}
	resp := &resource.CreateResponse{State: tfsdk.State{Schema: sch, Raw: plan}}
	r.Create(t.Context(), resource.CreateRequest{
		Plan:   tfsdk.Plan{Schema: sch, Raw: plan},
		Config: tfsdk.Config{Schema: sch, Raw: config},
	}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Create returned errors: %v", resp.Diagnostics)
	}

	if !strings.Contains(uploaded, secret) {
		t.Fatal("the write-only value must be read from the configuration and uploaded")
	}

	var model fileResourceModel
	if diags := resp.State.Get(t.Context(), &model); diags.HasError() {
		t.Fatalf("reading state failed: %v", diags)
	}
	if !model.Content.IsNull() || !model.ContentBase64.IsNull() || !model.ContentWO.IsNull() {
		t.Errorf("no form of the content may reach state, got %+v", model)
	}
	if model.Checksum.ValueString() != sha256Hex(secret) {
		t.Errorf("checksum = %q, want the checksum of the uploaded content", model.Checksum.ValueString())
	}
	if model.Size.ValueInt64() != int64(len(secret)) {
		t.Errorf("size = %d, want %d", model.Size.ValueInt64(), len(secret))
	}
	if model.ContentWOVersion.ValueInt64() != 1 {
		t.Errorf("content_wo_version = %d, want the configured 1 — it is the marker Read keys off", model.ContentWOVersion.ValueInt64())
	}
}

// TestFileResource_Read_KeepsWriteOnlyContentOutOfState covers the refresh side:
// the file is downloaded for its checksum, and the bytes are then dropped.
func TestFileResource_Read_KeepsWriteOnlyContentOutOfState(t *testing.T) {
	const remote = "{\"secretKey\":\"rotated\"}\n"
	server := fileReadServer(t, "/containers/conf/s3.json", []byte(remote), false)

	model, _ := readFileResource(t, server, map[string]tftypes.Value{
		"id":                 tfString("/containers/conf/s3.json"),
		"share_path":         tfString("/containers/conf"),
		"name":               tfString("s3.json"),
		"content_wo_version": tftypes.NewValue(tftypes.Number, 1),
		"checksum":           tfString(sha256Hex("previous")),
	})

	if !model.Content.IsNull() || !model.ContentBase64.IsNull() {
		t.Errorf("refresh must not persist the content of a write-only file, got %+v", model)
	}
	if model.Checksum.ValueString() != sha256Hex(remote) {
		t.Errorf("checksum = %q, want the checksum of what DSM stores — that is the drift signal", model.Checksum.ValueString())
	}
}

func TestFileContentWillChange(t *testing.T) {
	writeOnlyState := fileResourceModel{
		ContentWOVersion: types.Int64Value(1),
		Checksum:         types.StringValue(sha256Hex("written")),
	}
	tests := []struct {
		name        string
		state       fileResourceModel
		plan        fileResourceModel
		lastWritten string
		want        bool
	}{
		{
			name:  "content edited in configuration",
			state: fileResourceModel{Content: types.StringValue("old")},
			plan:  fileResourceModel{Content: types.StringValue("new")},
			want:  true,
		},
		{
			name:  "nothing changed",
			state: fileResourceModel{Content: types.StringValue("same"), Checksum: types.StringValue(sha256Hex("same"))},
			plan:  fileResourceModel{Content: types.StringValue("same"), Checksum: types.StringValue(sha256Hex("same"))},
			want:  false,
		},
		{
			name:  "write-only version bumped",
			state: writeOnlyState,
			plan:  fileResourceModel{ContentWOVersion: types.Int64Value(2)},
			want:  true,
		},
		{
			name:        "write-only file edited outside terraform",
			state:       fileResourceModel{ContentWOVersion: types.Int64Value(1), Checksum: types.StringValue(sha256Hex("edited on the NAS"))},
			plan:        writeOnlyState,
			lastWritten: sha256Hex("written"),
			want:        true,
		},
		{
			name:        "write-only file still matches the last write",
			state:       writeOnlyState,
			plan:        writeOnlyState,
			lastWritten: sha256Hex("written"),
			want:        false,
		},
		{
			// A resource that predates the private-state entry, or one that was
			// imported: with nothing to compare against, a rewrite would be a guess.
			name:  "write-only file with no remembered checksum",
			state: writeOnlyState,
			plan:  writeOnlyState,
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fileContentWillChange(tt.state, tt.plan, tt.lastWritten); got != tt.want {
				t.Errorf("fileContentWillChange = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestFileResource_ModifyPlan_MarksRewrittenAttributesUnknown: Terraform carries
// computed attributes forward from prior state, so a plan that leaves the old
// checksum in place both hides the rewrite and trips the "inconsistent result
// after apply" check.
func TestFileResource_ModifyPlan_MarksRewrittenAttributesUnknown(t *testing.T) {
	sch := fileResourceSchema(t)
	state := fileObject(t, sch, map[string]tftypes.Value{
		"id":                 tfString("/containers/conf/s3.json"),
		"share_path":         tfString("/containers/conf"),
		"name":               tfString("s3.json"),
		"content_wo_version": tftypes.NewValue(tftypes.Number, 1),
		"checksum":           tfString(sha256Hex("written")),
		"size":               tftypes.NewValue(tftypes.Number, 7),
	})
	bumped := fileObject(t, sch, map[string]tftypes.Value{
		"id":                 tfString("/containers/conf/s3.json"),
		"share_path":         tfString("/containers/conf"),
		"name":               tfString("s3.json"),
		"content_wo_version": tftypes.NewValue(tftypes.Number, 2),
		"checksum":           tfString(sha256Hex("written")),
		"size":               tftypes.NewValue(tftypes.Number, 7),
	})

	tests := []struct {
		name        string
		state       tftypes.Value
		plan        tftypes.Value
		wantUnknown bool
	}{
		{name: "version bumped", state: state, plan: bumped, wantUnknown: true},
		{name: "no change", state: state, plan: state},
		{name: "create", state: tftypes.NewValue(sch.Type().TerraformType(t.Context()), nil), plan: bumped},
		{name: "destroy", state: state, plan: tftypes.NewValue(sch.Type().TerraformType(t.Context()), nil)},
	}

	r := NewFileResource().(*fileResource)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &resource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: sch, Raw: tt.plan}}
			r.ModifyPlan(t.Context(), resource.ModifyPlanRequest{
				State: tfsdk.State{Schema: sch, Raw: tt.state},
				Plan:  tfsdk.Plan{Schema: sch, Raw: tt.plan},
			}, resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("ModifyPlan returned errors: %v", resp.Diagnostics)
			}
			if resp.Plan.Raw.IsNull() {
				return
			}

			var planned fileResourceModel
			if diags := resp.Plan.Get(t.Context(), &planned); diags.HasError() {
				t.Fatalf("reading plan failed: %v", diags)
			}
			if planned.Checksum.IsUnknown() != tt.wantUnknown || planned.Size.IsUnknown() != tt.wantUnknown {
				t.Errorf("checksum/size unknown = %v/%v, want %v", planned.Checksum.IsUnknown(), planned.Size.IsUnknown(), tt.wantUnknown)
			}
			// Update re-reads them from File Station, so they cannot be planned as
			// the values prior state happened to hold.
			if planned.PosixMode.IsUnknown() != tt.wantUnknown || planned.ACLMode.IsUnknown() != tt.wantUnknown {
				t.Errorf("posix_mode/acl_mode unknown = %v/%v, want %v", planned.PosixMode.IsUnknown(), planned.ACLMode.IsUnknown(), tt.wantUnknown)
			}
		})
	}
}

func TestPrivateChecksumRoundTrip(t *testing.T) {
	checksum := sha256Hex("content")
	if got := parsePrivateChecksum(privateChecksumValue(checksum)); got != checksum {
		t.Errorf("round trip = %q, want %q", got, checksum)
	}
	for _, raw := range [][]byte{nil, {}, []byte("not json")} {
		if got := parsePrivateChecksum(raw); got != "" {
			t.Errorf("parsePrivateChecksum(%q) = %q, want an empty checksum", raw, got)
		}
	}
}

// TestApplyFileContent_FallsBackToBase64ForBinary documents why a text file that
// turned binary switches representation: Terraform strings must be valid UTF-8.
func TestApplyFileContent_FallsBackToBase64ForBinary(t *testing.T) {
	model := fileResourceModel{Content: types.StringValue("text"), ContentBase64: types.StringNull()}
	applyFileContent(&model, []byte{0xff, 0xfe})

	if !model.Content.IsNull() {
		t.Error("content must be cleared when the file cannot be represented as a string")
	}
	if model.ContentBase64.ValueString() != base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe}) {
		t.Errorf("content_base64 = %q", model.ContentBase64.ValueString())
	}
}

func TestFileErrorDetail(t *testing.T) {
	tests := []struct {
		err      error
		contains []string
	}{
		{fmt.Errorf("read: %w", client.ErrFileNotFound), []string{"file not found", "File Station"}},
		{&client.APIError{Code: 103, API: "SYNO.FileStation.Upload"}, []string{"FileStation", "package"}},
		{&client.APIError{Code: 105, API: "SYNO.FileStation.Upload"}, []string{"administrator", "permissions"}},
		{&client.APIError{Code: 408, API: "SYNO.FileStation.Upload"}, []string{"dsm_shared_folder", "408"}},
		{&client.APIError{Code: 414, API: "SYNO.FileStation.Upload"}, []string{"terraform import", "414"}},
		{&client.APIError{Code: 416, API: "SYNO.FileStation.Upload"}, []string{"space", "quota"}},
		{&client.APIError{Code: 419, API: "SYNO.FileStation.Upload"}, []string{"file name"}},
		{errors.New("http request: connection refused"), []string{"connection refused"}},
	}

	for _, tt := range tests {
		got := fileErrorDetail(tt.err)
		for _, want := range tt.contains {
			if !strings.Contains(got, want) {
				t.Errorf("detail for %v should mention %q, got:\n%s", tt.err, want, got)
			}
		}
		if !strings.Contains(got, tt.err.Error()) {
			t.Errorf("original message must survive, got:\n%s", got)
		}
	}
}
