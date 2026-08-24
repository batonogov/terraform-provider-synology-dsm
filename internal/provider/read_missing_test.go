package provider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// These tests pin down the two halves of Terraform's Read contract, which this
// provider used to get wrong in one direction (issue #131):
//
//   - an object DSM no longer has leaves state without a diagnostic, so the
//     next plan re-creates it instead of aborting the whole refresh;
//   - an object DSM could not be *asked* about stays in state and the failure
//     surfaces as an error, so a network blip never turns into a silent
//     "terraform forgot about your shared folder".
//
// Every resource below has both cases, because getting only the first one right
// is how a provider deletes live infrastructure from a practitioner's state.

// resourceSchema renders a resource's own schema, which the state fixtures need.
func resourceSchema(t *testing.T, r resource.Resource) rschema.Schema {
	t.Helper()
	resp := &resource.SchemaResponse{}
	r.Schema(t.Context(), resource.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema: %v", resp.Diagnostics)
	}
	return resp.Schema
}

// dsmMock serves one canned handler on DSM's entry.cgi.
func dsmMock(t *testing.T, handler func(api, method string, r *http.Request, w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/webapi/entry.cgi", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		api := r.FormValue("api")
		method := r.FormValue("method")
		if api == "" {
			api = r.URL.Query().Get("api")
			method = r.URL.Query().Get("method")
		}
		handler(api, method, r, w)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func writeData(w http.ResponseWriter, data any) {
	raw, _ := json.Marshal(data)
	_ = json.NewEncoder(w).Encode(client.APIResponse{Success: true, Data: raw})
}

func writeAPIFailure(w http.ResponseWriter, code int) {
	_ = json.NewEncoder(w).Encode(client.APIResponse{Success: false, Error: &client.APIError{Code: code}})
}

func testClientFor(server *httptest.Server) *client.Client {
	return client.NewClient(server.URL, "admin", "", false)
}

// assertGone fails unless Read removed the resource and reported no error.
func assertGone(t *testing.T, resp *resource.ReadResponse) {
	t.Helper()
	if resp.Diagnostics.HasError() {
		t.Fatalf("a missing object must not fail the refresh: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Error("a missing object must be removed from state so the next plan re-creates it")
	}
}

// assertKept fails unless Read reported an error and left state intact.
func assertKept(t *testing.T, resp *resource.ReadResponse) {
	t.Helper()
	if !resp.Diagnostics.HasError() {
		t.Fatal("a failure that says nothing about the object must surface as an error")
	}
	if resp.State.Raw.IsNull() {
		t.Error("a resource must never be dropped from state because DSM could not be reached")
	}
}

// --- firewall rule: the resource the issue was filed against -----------------

func firewallRuleState(t *testing.T, s rschema.Schema) *resource.ReadResponse {
	t.Helper()
	model := &firewallRuleResourceModel{
		ID:                types.StringValue(client.BuildFirewallRuleID("default", "global", "DSM web UI")),
		Profile:           types.StringValue("default"),
		Adapter:           types.StringValue("global"),
		Name:              types.StringValue("DSM web UI"),
		Priority:          types.Int64Value(0),
		Action:            types.StringValue("allow"),
		Protocol:          types.StringValue("tcp"),
		Ports:             types.ListNull(types.StringType),
		Source:            types.ListNull(types.StringType),
		Enabled:           types.BoolValue(true),
		AllowLockout:      types.BoolValue(false),
		AllowEmptyRuleSet: types.BoolValue(false),
	}
	return &resource.ReadResponse{State: stateFor(t, s, model)}
}

func readFirewallRule(t *testing.T, c *client.Client) *resource.ReadResponse {
	t.Helper()
	r := &firewallRuleResource{client: c}
	s := resourceSchema(t, r)
	resp := firewallRuleState(t, s)
	r.Read(t.Context(), resource.ReadRequest{State: firewallRuleState(t, s).State}, resp)
	return resp
}

func TestFirewallRuleResource_ReadRemovesRuleDeletedInDSM(t *testing.T) {
	// The profile is there and readable; the rule in state simply is not in it.
	server := dsmMock(t, func(api, method string, _ *http.Request, w http.ResponseWriter) {
		switch {
		case api == "SYNO.Core.Security.Firewall.Profile" && method == "get":
			writeData(w, map[string]any{
				"name":             "default",
				"rules":            map[string]any{"global": []any{}},
				"adapterPolicyMap": map[string]any{"global": 2},
			})
		default:
			writeData(w, map[string]any{"enable_firewall": true, "profile_name": "default"})
		}
	})

	assertGone(t, readFirewallRule(t, testClientFor(server)))
}

func TestFirewallRuleResource_ReadKeepsRuleWhenDSMIsUnreachable(t *testing.T) {
	server := dsmMock(t, func(_, _ string, _ *http.Request, _ http.ResponseWriter) {})
	c := testClientFor(server)
	server.Close()

	assertKept(t, readFirewallRule(t, c))
}

func TestFirewallRuleResource_ReadKeepsRuleWhenSessionExpired(t *testing.T) {
	// 119 is about the session, not the rule. Before this distinction existed a
	// resource could have been dropped from state on a stale SID.
	server := dsmMock(t, func(api, method string, _ *http.Request, w http.ResponseWriter) {
		if api == "SYNO.API.Auth" && method == "login" {
			writeAPIFailure(w, 400)
			return
		}
		writeAPIFailure(w, 119)
	})

	assertKept(t, readFirewallRule(t, testClientFor(server)))
}

// --- group -------------------------------------------------------------------

func readGroup(t *testing.T, c *client.Client) *resource.ReadResponse {
	t.Helper()
	r := &groupResource{client: c}
	s := resourceSchema(t, r)
	model := &groupResourceModel{
		ID:          types.StringValue("devs"),
		Name:        types.StringValue("devs"),
		Description: types.StringNull(),
		GID:         types.Int64Value(65536),
	}
	resp := &resource.ReadResponse{State: stateFor(t, s, model)}
	r.Read(t.Context(), resource.ReadRequest{State: stateFor(t, s, model)}, resp)
	return resp
}

func TestGroupResource_ReadRemovesGroupDeletedInDSM(t *testing.T) {
	// DSM answers the get with an empty groups array, and the list agrees.
	server := dsmMock(t, func(api, method string, _ *http.Request, w http.ResponseWriter) {
		if api == "SYNO.Core.Group" && (method == "get" || method == "list") {
			writeData(w, map[string]any{"groups": []any{}, "total": 0})
			return
		}
		writeAPIFailure(w, 101)
	})

	assertGone(t, readGroup(t, testClientFor(server)))
}

func TestGroupResource_ReadKeepsGroupWhenListCannotConfirm(t *testing.T) {
	// The get is refused and the list is refused too: nothing about the group
	// has been established, so the refusal must reach the practitioner.
	server := dsmMock(t, func(api, method string, _ *http.Request, w http.ResponseWriter) {
		if api == "SYNO.API.Auth" && method == "login" {
			writeAPIFailure(w, 400)
			return
		}
		writeAPIFailure(w, 105)
	})

	assertKept(t, readGroup(t, testClientFor(server)))
}

// --- share permission --------------------------------------------------------

func readSharePermission(t *testing.T, c *client.Client) *resource.ReadResponse {
	t.Helper()
	r := &sharePermissionResource{client: c}
	s := resourceSchema(t, r)
	model := &sharePermissionResourceModel{
		ID:            types.StringValue(client.BuildSharePermissionID("team", "local_user", "john")),
		ShareName:     types.StringValue("team"),
		UserGroupType: types.StringValue("local_user"),
		PrincipalName: types.StringValue("john"),
		Permission:    types.StringValue("rw"),
	}
	resp := &resource.ReadResponse{State: stateFor(t, s, model)}
	r.Read(t.Context(), resource.ReadRequest{State: stateFor(t, s, model)}, resp)
	return resp
}

func TestSharePermissionResource_ReadRemovesPrincipalDeletedInDSM(t *testing.T) {
	server := dsmMock(t, func(api, method string, _ *http.Request, w http.ResponseWriter) {
		if api == "SYNO.Core.Share.Permission" && method == "list" {
			writeData(w, map[string]any{"items": []any{}, "total": 0})
			return
		}
		writeAPIFailure(w, 101)
	})

	assertGone(t, readSharePermission(t, testClientFor(server)))
}

func TestSharePermissionResource_ReadKeepsPermissionWhenShareCannotBeListed(t *testing.T) {
	// A share that cannot be listed is a failure, not an answer: the permission
	// may well still be there. Note the asymmetry with the case above — an empty
	// list is evidence, a refused list is not.
	server := dsmMock(t, func(api, method string, _ *http.Request, w http.ResponseWriter) {
		if api == "SYNO.API.Auth" && method == "login" {
			writeAPIFailure(w, 400)
			return
		}
		writeAPIFailure(w, 105)
	})

	assertKept(t, readSharePermission(t, testClientFor(server)))
}

// --- shared folder -----------------------------------------------------------

func readSharedFolder(t *testing.T, c *client.Client) *resource.ReadResponse {
	t.Helper()
	r := &sharedFolderResource{client: c}
	s := resourceSchema(t, r)
	model := &sharedFolderResourceModel{
		ID:      types.StringValue("team-data"),
		Name:    types.StringValue("team-data"),
		VolPath: types.StringValue("/volume1"),
	}
	resp := &resource.ReadResponse{State: stateFor(t, s, model)}
	r.Read(t.Context(), resource.ReadRequest{State: stateFor(t, s, model)}, resp)
	return resp
}

func TestSharedFolderResource_ReadRemovesShareDeletedInDSM(t *testing.T) {
	// DSM refuses the get for an unknown name; the list settles it.
	server := dsmMock(t, func(api, method string, _ *http.Request, w http.ResponseWriter) {
		switch {
		case api == "SYNO.Core.Share" && method == "list":
			writeData(w, map[string]any{"shares": []any{}, "total": 0})
		default:
			writeAPIFailure(w, 3301)
		}
	})

	assertGone(t, readSharedFolder(t, testClientFor(server)))
}

func TestSharedFolderResource_ReadKeepsShareWhenDSMIsUnreachable(t *testing.T) {
	server := dsmMock(t, func(_, _ string, _ *http.Request, _ http.ResponseWriter) {})
	c := testClientFor(server)
	server.Close()

	assertKept(t, readSharedFolder(t, c))
}
