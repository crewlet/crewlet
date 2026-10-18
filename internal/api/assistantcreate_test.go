package api_test

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/url"
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
	"github.com/crewlet/crewlet/internal/iam"
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

// EVERY POST SAYS WHAT IT DOES, AND ITS CARD SAYS IT AFTER IT.
//
// A POST's method says nothing of what it does, so the assistant titled every
// POST a create: a person approving a purge read "Create purge" under "Approve
// and create", in the ordinary tone. Walked over every guarded POST the App
// mounts, the ones the assistant never names included, each declares what it
// does ([authz.Effect]), and the act route, mounted outside every router, is
// declared a call where the table binds it. Over every POST the assistant may
// name, the card says it: a destruction destructive with what goes said first,
// and nothing else; a create titled as one, and nothing that does not create.
// Mutation: mount a POST that declares nothing, declare an erase a rewrite, or
// rate a POST by its method, and this names the route.
func TestEveryPOSTSaysWhatItDoes(t *testing.T) {
	t.Parallel()
	app := everySurfaceApp(t)
	posts := 0
	for pattern, p := range app.Policies() {
		if !strings.HasPrefix(pattern, http.MethodPost+" ") {
			continue
		}
		posts++
		if !p.Effect.Valid() || p.Effect == authz.EffectCalls {
			t.Errorf("%s declares %q: say what it does (authz.Policy's Creating, Rewriting, "+
				"Destroying or Reading)", pattern, p.Effect)
		}
	}
	if posts < 40 {
		t.Fatalf("walked %d guarded POSTs; the App's routes are not being read", posts)
	}
	for _, destroys := range []string{"POST /worklet/items/{key}/purge",
		"POST /doclet/pages/{id}/purge", "POST /artifacts/{id}/purge",
		"POST /chatlet/channels/{channel_id}/erase", "POST /chatlet/channels/{channel_id}/prune"} {
		if got := app.Policies()[destroys].Effect; got != authz.EffectDestroys {
			t.Errorf("%s declares %q, and it destroys what it names", destroys, got)
		}
	}

	routes := app.AssistantRoutes()
	table, err := catalog.NewRouteTable(routes)
	if err != nil {
		t.Fatal(err)
	}
	admin := iam.Principal{Kind: iam.KindPerson, Stage: iam.StageActive,
		Grants: []iam.Grant{iam.GrantAdmin}}
	change := catalog.GenericTools(table, admin, nil)[2]
	read := func(_ context.Context, r exec.Request) (exec.Response, error) {
		return exec.Response{Status: http.StatusOK, Body: []byte(`{"valid":true}`)}, nil
	}
	cards := 0
	for _, r := range table.Routes() {
		if r.Method() != http.MethodPost {
			continue
		}
		if r.Pattern == catalog.ActRoute {
			if r.Effect != authz.EffectCalls {
				t.Errorf("the act route is bound as %q, and it calls a tool", r.Effect)
			}
			continue
		}
		path := map[string]any{}
		for _, segment := range strings.Split(r.Path(), "/") {
			if name, wild := strings.CutPrefix(segment, "{"); wild {
				path[strings.TrimSuffix(strings.TrimSuffix(name, "}"), "...")] = "example"
			}
		}
		p, err := change.Preview(t.Context(), read, time.Now(), map[string]any{
			"route": r.Pattern, "path": path, "body": map[string]any{"name": "Example"}})
		if err != nil {
			continue
		}
		cards++
		destructive := p.Risk == wire.RiskDestructive
		creates := strings.HasPrefix(p.Title, "Create ")
		switch {
		case (r.Effect == authz.EffectDestroys) != destructive:
			t.Errorf("%s destroys %v, and its card %q is at risk %q", r.Pattern,
				r.Effect == authz.EffectDestroys, p.Title, p.Risk)
		case destructive && (!strings.HasPrefix(p.Summary, "This ") || p.Note == ""):
			t.Errorf("%s destroys, and its card says %q, %q", r.Pattern, p.Summary, p.Note)
		case creates && r.Effect != authz.EffectCreates:
			t.Errorf("%s %s, and its card is titled %q", r.Pattern, r.Effect, p.Title)
		}
	}
	if cards < 20 {
		t.Errorf("only %d POST cards drawn; the walk read nothing", cards)
	}
	// THE THREE THE QUESTION WAS ABOUT, as the person reads them, each from
	// what it sends: the room is named by nothing this read answers, so in
	// neutral words.
	room := map[string]any{"channel_id": "7f3c2a9e-4b1d-4c8e-9a6f-2d5e8b1c0a47"}
	for route, want := range map[string]struct {
		args                    map[string]any
		title, approve, summary string
	}{
		"POST /chatlet/channels/{channel_id}/erase": {map[string]any{"path": room,
			"query": map[string]any{"confirm": "2"},
			"body":  map[string]any{"message_ids": []any{"m1", "m2"}, "reason": "A key was pasted."}},
			"Erase 2 messages from the room", "Approve and erase",
			"This erases 2 messages from the room for good."},
		"POST /chatlet/channels/{channel_id}/prune": {map[string]any{"path": room,
			"query": map[string]any{"cutoff": "2031-01-01T00:00:00Z",
				"confirm": "2031-01-01T00:00:00Z"}},
			"Delete what the room said before 1 January 2031", "Approve and delete",
			"This deletes everything said in the room before 1 January 2031 for good."},
		"POST /worklet/items": {map[string]any{"body": map[string]any{"name": "Example"}},
			"Create item Example", "Approve and create", ""},
	} {
		args := maps.Clone(want.args)
		args["route"] = route
		p, err := change.Preview(t.Context(), read, time.Now(), args)
		if err != nil || p.Title != want.title || p.ApproveLabel != want.approve ||
			p.Summary != want.summary {
			t.Errorf("%s reads %q / %q: %q (%v)", route, p.Title, p.ApproveLabel, p.Summary, err)
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
