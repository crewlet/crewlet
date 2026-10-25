package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/authz"
)

// prepareChange previews one generic change through the read grant, the way a
// turn does, and answers the preview and the request its approval sends.
func prepareChange(t *testing.T, fx *assistApp, read exec.Grant,
	args map[string]any) (proposal.Preview, exec.Request) {

	t.Helper()
	p, err := genericTools(t, fx)[catalog.ProposeAPIChangeTool].Preview(t.Context(),
		readOver(fx, read), time.Now(), args)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	return p, proposal.RequestOf(assistantdomain.Proposal{Method: p.Request.Method,
		Path: p.Request.Path, Query: p.Request.Query, Body: p.Request.Body,
		SourceRevision: p.SourceRevision})
}

// THE ROLE THE PERSON ASKED FOR IS PREPARED AS A CREATE, AND THE ROUTE SAYS
// WHERE A SEAT IS ADDED.
//
// The request that found this: "add a new agent role of 'senior software
// engineer' and name it crewlet-swe", proposed as PUT /config/roles/crewlet-swe
// with the seat's handle and name. Its read answers 404, so the preview draws
// a create (every field added) bound to that absence, and the approval sends it
// under `If-None-Match: *`. It used to be drawn as an edit and sent as a plain
// PUT, refused `404 no_such_entity` after the person approved it, with a hint
// to send the create header. A seat, though, has a place in the chart its path
// cannot name, so the route answers the create `400 not_creatable` with the
// write that adds one (the unit it sits in), which the turn is handed, and no
// seat is added. Mutation: preview the 404 as an edit, or answer a missing
// seat's PUT with the create-only hint, and this fails.
func TestTheAskedForRoleIsPreparedAsACreate(t *testing.T) {
	t.Parallel()
	fx, read := principlesApp(t)
	p, req := prepareChange(t, fx, read, map[string]any{
		"route": "PUT /config/roles/{id}", "path": map[string]any{"id": "crewlet-swe"},
		"body": map[string]any{"handle": "crewlet-swe", "name": "Senior Software Engineer"}})

	if r, err := proposal.ParseRevision(p.SourceRevision); err != nil ||
		r.Kind != proposal.RevisionCreate || req.IfNoneMatch != exec.AnyRepresentation ||
		req.IfMatch != "" || p.Title != "Create through the API" {
		t.Fatalf("the role is %q bound to %q, sent under If-Match %q and If-None-Match %q",
			p.Title, p.SourceRevision, req.IfMatch, req.IfNoneMatch)
	}
	for _, c := range p.Changes {
		if c.Kind != wire.ChangeFact && c.Kind != wire.ChangeAdded {
			t.Errorf("the create draws %s as %s, want every field added", c.Field, c.Kind)
		}
	}

	resp, err := fx.disp.Do(t.Context(), fx.derive(t, approvedFor(req, authz.Action(p.Act))), req)
	refusal := exec.RefusalOf(resp)
	if err != nil || resp.Status != http.StatusBadRequest ||
		refusal.Code != string(httpjson.CodeNotCreatable) ||
		!strings.Contains(refusal.Hint, "PUT /config/units/{id}") {
		t.Fatalf("the approved role answered %d %s, %v, want 400 %s naming the unit write",
			resp.Status, resp.Body, err, httpjson.CodeNotCreatable)
	}
	if status, _ := fx.asPerson(t, exec.Request{Method: http.MethodGet,
		Path: "/config/roles/crewlet-swe"}); status != http.StatusNotFound {
		t.Errorf("GET /config/roles/crewlet-swe = %d after a refused create, want 404", status)
	}
}

// A CREATABLE ENTITY IS CREATED THROUGH THE GENERIC CHANGE, AND A CREATE THAT
// RACED ONE IS REFUSED.
//
// An MCP server nothing carries yet is previewed as a create, sent under
// `If-None-Match: *`, and lands; the person reads it. The same create approved
// again (prepared against the absence another write has since filled) is
// refused `412 entity_exists` by the route rather than replacing the server,
// and the approval's re-read of the create's revision has moved. Mutation:
// send the create without its precondition, and the second write replaces the
// first.
func TestACreatableEntityIsCreatedAndARacedCreateIsRefused(t *testing.T) {
	t.Parallel()
	fx, read := principlesApp(t)
	p, req := prepareChange(t, fx, read, map[string]any{
		"route": "PUT /config/mcp-servers/{id}", "path": map[string]any{"id": "linear"},
		"body": map[string]any{"name": "linear", "command": "linear-mcp",
			"_summary": "add the linear server"}})
	if req.IfNoneMatch != exec.AnyRepresentation {
		t.Fatalf("the server's create is sent under If-None-Match %q", req.IfNoneMatch)
	}
	approve := func() exec.Response {
		t.Helper()
		resp, err := fx.disp.Do(t.Context(), fx.derive(t, approvedFor(req, authz.Action(p.Act))), req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := approve(); resp.Status != http.StatusCreated && resp.Status != http.StatusOK {
		t.Fatalf("the approved create answered %d %s", resp.Status, resp.Body)
	}
	if status, body := fx.asPerson(t, exec.Request{Method: http.MethodGet,
		Path: "/config/mcp-servers/linear"}); status != http.StatusOK ||
		!strings.Contains(string(body), "linear-mcp") {
		t.Fatalf("GET /config/mcp-servers/linear = %d %s after the create", status, body)
	}

	resp := approve()
	if resp.Status != http.StatusPreconditionFailed ||
		exec.RefusalOf(resp).Code != string(httpjson.CodeEntityExists) {
		t.Errorf("a create raced by another answered %d %s, want 412 %s", resp.Status,
			resp.Body, httpjson.CodeEntityExists)
	}
	if now, err := catalog.Revision(t.Context(), readOver(fx, read), p.SourceRevision); err != nil ||
		now == p.SourceRevision {
		t.Errorf("the create's revision re-read as %q (%v), the absence it was bound to", now, err)
	}
}
