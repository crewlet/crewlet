package api_test

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
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

// THE ROLE THE PERSON ASKED FOR IS REFUSED AT ITS PREVIEW, AND THE UNIT WRITE
// THAT ADDS IT APPLIES.
//
// The request that found this: "add a new agent role of 'senior software
// engineer' and name it crewlet-swe", proposed as PUT /config/roles/crewlet-swe.
// A seat has a place in the chart its path cannot name, so the route never
// creates one by address, and a proposal of it was a card the person approved
// only for the API to refuse it `400 not_creatable`, every time. The route
// declares that where it is mounted ([authz.Absent], from the table its handler
// decides by), so the preview refuses the step at once, with the route's own
// sentence about where a seat is added, and draws no card. The model's next
// plan, the unit the seat sits in with the seat added, is a proposal bound to
// the unit's revision that applies once approved. Mutation: propose a create
// for every 404 again, or let the route declare nothing, and this fails.
func TestTheAskedForRoleIsRefusedAtPreviewAndItsUnitWriteApplies(t *testing.T) {
	t.Parallel()
	fx, read := principlesApp(t)
	_, err := genericTools(t, fx)[catalog.ProposeAPIChangeTool].Preview(t.Context(),
		readOver(fx, read), time.Now(), map[string]any{
			"route": "PUT /config/roles/{id}", "path": map[string]any{"id": "crewlet-swe"},
			"body": map[string]any{"handle": "crewlet-swe", "name": "Senior Software Engineer"}})
	var refused *catalog.PreviewRefusal
	if !errors.As(err, &refused) || refused.Refusal.Code != string(httpjson.CodeNotCreatable) ||
		!strings.Contains(refused.Refusal.Hint, "PUT /config/units/{id}") {
		t.Fatalf("the role's preview answered %v, want %s naming the unit write",
			err, httpjson.CodeNotCreatable)
	}

	// THE PREVIEW SAYS WHAT THE ROUTE WOULD HAVE SAID: the same create, had
	// the person approved it, is refused with the same sentence.
	create := exec.Request{Method: http.MethodPut, Path: "/config/roles/crewlet-swe",
		IfNoneMatch: exec.AnyRepresentation,
		Body:        []byte(`{"handle":"crewlet-swe","name":"Senior Software Engineer","_summary":"add"}`)}
	answer, err := fx.disp.Do(t.Context(), fx.derive(t, approvedFor(create, authz.ActionOrgWrite)), create)
	if err != nil || answer.Status != http.StatusBadRequest ||
		exec.RefusalOf(answer).Hint != refused.Refusal.Hint {
		t.Errorf("the route answered the create %d %s, not the preview's %q", answer.Status,
			answer.Body, refused.Refusal.Hint)
	}

	unit := map[string]any{"id": "engineering", "name": "Engineering", "roles": []any{
		map[string]any{"name": "CTO", "handle": "cto", "llm": "zulu"},
		map[string]any{"name": "Senior Software Engineer", "handle": "crewlet-swe", "llm": "zulu"},
	}, "_summary": "add the senior software engineer"}
	p, req := prepareChange(t, fx, read, map[string]any{
		"route": "PUT /config/units/{id}", "path": map[string]any{"id": "engineering"},
		"body": unit})
	if r, err := proposal.ParseRevision(p.SourceRevision); err != nil ||
		r.Kind == proposal.RevisionCreate || req.IfMatch == "" {
		t.Fatalf("the unit write is bound to %q under If-Match %q, want the unit's revision",
			p.SourceRevision, req.IfMatch)
	}
	resp, err := fx.disp.Do(t.Context(), fx.derive(t, approvedFor(req, authz.Action(p.Act))), req)
	if err != nil || (resp.Status != http.StatusOK && resp.Status != http.StatusCreated) {
		t.Fatalf("the approved unit write answered %d %s, %v", resp.Status, resp.Body, err)
	}
	if status, body := fx.asPerson(t, exec.Request{Method: http.MethodGet,
		Path: "/config/roles/crewlet-swe"}); status != http.StatusOK ||
		!strings.Contains(string(body), "Senior Software Engineer") {
		t.Errorf("GET /config/roles/crewlet-swe = %d %s after the unit write", status, body)
	}
}

// EVERY PUT A PREVIEW CAN FIND EMPTY SAYS WHAT IT DOES THERE.
//
// A preview reads a PUT's path through its GET twin, and when nothing is there
// it proposes a create only where the route declares that it creates
// ([authz.Absent]). So every PUT the assistant may name whose path holds an
// address and whose GET twin exists declares one or the other, here, as the
// App mounts it: a route added without it would be refused at every empty
// address, a create it does take included. And for /config's entities the
// declaration is held to the handler: a create-only write at a missing id is
// refused `not_creatable` exactly where the route says it adds elsewhere.
// Mutation: drop a route's declaration, or declare a seat creatable, and this
// names the route.
func TestEveryPUTAPreviewCanFindEmptyDeclaresWhatItDoes(t *testing.T) {
	t.Parallel()
	routes := everySurfaceApp(t).AssistantRoutes()
	fx, _ := principlesApp(t)
	gets := map[string]bool{}
	for _, r := range routes {
		gets[r.Pattern] = true
	}
	for _, r := range routes {
		if r.Method() != http.MethodPut || !strings.Contains(r.Path(), "{") ||
			!gets[http.MethodGet+" "+r.Path()] {
			continue
		}
		if r.Absent == nil {
			t.Errorf("%s declares nothing about an empty address (authz.Policy.Absent)", r.Pattern)
			continue
		}
		kind, entity := strings.CutPrefix(r.Path(), "/config/")
		if !entity {
			continue
		}
		kind = strings.TrimSuffix(kind, "/{id}")
		create := exec.Request{Method: http.MethodPut, Path: "/config/" + kind + "/nobody-here",
			Query: url.Values{"dry_run": {"true"}}, IfNoneMatch: exec.AnyRepresentation,
			Body: []byte(`{"name":"nobody-here"}`)}
		answer, err := fx.disp.Do(t.Context(), fx.derive(t, approvedFor(create, r.Action)), create)
		if err != nil {
			t.Fatalf("%s: %v", r.Pattern, err)
		}
		refusedHere := exec.RefusalOf(answer).Code == string(httpjson.CodeNotCreatable)
		if refusedHere == r.Absent.Creates ||
			(refusedHere && exec.RefusalOf(answer).Hint != r.Absent.Elsewhere) {
			t.Errorf("%s declares %+v, and its handler answered a create %d %s",
				r.Pattern, *r.Absent, answer.Status, answer.Body)
		}
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
