package api_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
)

// genericTools are the generic API tools over fx's own route table, for its
// person, by name.
func genericTools(t *testing.T, fx *assistApp) map[string]catalog.Tool {
	t.Helper()
	table, err := catalog.NewRouteTable(fx.app.AssistantRoutes())
	if err != nil {
		t.Fatalf("the App's assistant routes: %v", err)
	}
	out := map[string]catalog.Tool{}
	for _, tool := range catalog.GenericTools(table, fx.principal(t), nil) {
		out[tool.Name] = tool
	}
	return out
}

// unitRoute is the per-entity route the Org builder's lead mode saves a unit
// through, and what an agent seat is added to a unit on.
const unitRoute = "PUT /config/units/{id}"

// newSeat is the seat the person asks for: an agent seat with a senior
// engineer's responsibilities, on the company's model.
var newSeat = map[string]any{
	"name": "crewlet-swe", "handle": "crewlet-swe", "llm": "zulu",
	"responsibilities": []any{"Design, build and review the engine's services",
		"Mentor the team on code quality and testing"},
}

// unitNamed reads one unit as the person.
func unitNamed(t *testing.T, fx *assistApp, id string) map[string]any {
	t.Helper()
	status, body := fx.asPerson(t, exec.Request{Method: http.MethodGet,
		Path: "/config/units/" + id})
	if status != http.StatusOK {
		t.Fatalf("GET /config/units/%s = %d %s", id, status, body)
	}
	var unit map[string]any
	if err := json.Unmarshal(body, &unit); err != nil {
		t.Fatal(err)
	}
	return unit
}

// seated reports whether unit holds a seat with handle.
func seated(unit map[string]any, handle string) bool {
	roles, _ := unit["roles"].([]any)
	return slices.ContainsFunc(roles, func(r any) bool {
		role, _ := r.(map[string]any)
		return role["handle"] == handle
	})
}

// prepareSeat looks the route up, reads the unit and previews the seat's
// addition through the read grant, the way a turn does, and answers the
// preview and the request an approval of it sends.
func prepareSeat(t *testing.T, fx *assistApp, read exec.Grant) (proposal.Preview, exec.Request) {
	t.Helper()
	tools := genericTools(t, fx)

	// LOOKED UP, NOT GUESSED: the route is listed and documented.
	listing, err := tools[catalog.DescribeAPITool].Docs(catalog.Args{"search": "units"})
	if err != nil || !strings.Contains(listing, unitRoute) {
		t.Fatalf("describe_api does not list %s: %v\n%s", unitRoute, err, listing)
	}
	doc, err := tools[catalog.DescribeAPITool].Docs(catalog.Args{"route": unitRoute})
	if err != nil || !strings.Contains(doc, "config.org.write") ||
		!strings.Contains(doc, "confirm it is them") {
		t.Fatalf("describe_api's documentation of %s: %v\n%s", unitRoute, err, doc)
	}

	// THE UNIT AS IT IS, through the read grant.
	reader := tools[catalog.APIRequestTool]
	args := map[string]any{"route": "GET /config/units/{id}",
		"path": map[string]any{"id": "engineering"}}
	req, err := reader.Request(args)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := fx.disp.Do(t.Context(), read, req)
	if err != nil || resp.Status != http.StatusOK {
		t.Fatalf("api_request read the unit: %d %s, %v", resp.Status, resp.Body, err)
	}
	shaped, err := catalog.Shape(reader, args, resp, provenance.NewBoundary(), time.Now())
	if err != nil || !strings.Contains(shaped.Model, "cto") {
		t.Fatalf("the model was handed %q, %v, not the unit", shaped.Model, err)
	}
	var unit map[string]any
	if err := json.Unmarshal(resp.Body, &unit); err != nil {
		t.Fatal(err)
	}

	// THE CHANGE: the whole unit back, with the seat added, as a PUT
	// replaces it.
	roles, _ := unit["roles"].([]any)
	unit["roles"] = append(roles, newSeat)
	unit["_summary"] = "add crewlet-swe, a senior software engineer"
	p, err := tools[catalog.ProposeAPIChangeTool].Preview(t.Context(), readOver(fx, read),
		time.Now(), map[string]any{"route": unitRoute,
			"path": map[string]any{"id": "engineering"}, "body": unit})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	return p, proposal.RequestOf(assistantdomain.Proposal{Method: p.Request.Method,
		Path: p.Request.Path, Query: p.Request.Query, Body: p.Request.Body,
		SourceRevision: p.SourceRevision})
}

// AN AGENT SEAT IS ADDED TO THE ORG THROUGH THE GENERIC CHANGE, AS ITS PERSON.
//
// The request that started this: an administrator on the Org chart asks for a
// seat named crewlet-swe with a senior software engineer's responsibilities.
// The turn looks the Org builder's own unit route up, reads the unit through
// its read grant, and proposes the unit back with the seat added. The preview
// is the server's: the request, and the new seat's fields as added rows under
// the unit it lands in (its name, its responsibilities), bound to the unit's
// entity tag, decided by `config.org.write` and saying beforehand that the
// approval asks a step-up. The read grant cannot send it; the approved request,
// carried on an exec grant bound to it under the If-Match the revision names,
// lands, and the person reads the seat in the unit. A second change prepared
// against the unit before it is refused by the route (409), and the revision
// the approval re-reads has moved. Mutation: put config.org.write on the
// human-only list and the approved seat is refused; drop the entity tag from
// the preview's revision and the stale change overwrites.
func TestAnAgentSeatIsAddedThroughTheGenericChange(t *testing.T) {
	t.Parallel()
	fx, read := principlesApp(t)
	if seated(unitNamed(t, fx, "engineering"), "crewlet-swe") {
		t.Fatal("the fixture already holds the seat")
	}
	p, req := prepareSeat(t, fx, read)

	if p.Act != string(authz.ActionOrgWrite) || !p.StepUp ||
		p.Target != "/config/units/engineering" {
		t.Errorf("the preview is decided by %q, step-up %v, about %q", p.Act, p.StepUp, p.Target)
	}
	added := map[string]string{}
	for _, c := range p.Changes {
		if c.Kind == wire.ChangeAdded {
			added[c.Field] = c.To
		}
	}
	if added["roles.crewlet-swe.name"] != "crewlet-swe" ||
		!strings.Contains(added["roles.crewlet-swe.responsibilities"], "Mentor the team") {
		t.Errorf("the preview's added rows are %v, want the new seat and its "+
			"responsibilities", added)
	}
	if slices.ContainsFunc(p.Changes, func(c wire.Change) bool {
		return strings.HasPrefix(c.Field, "roles.cto")
	}) {
		t.Errorf("the preview shows a change to the seat it leaves alone: %+v", p.Changes)
	}
	if r, err := proposal.ParseRevision(p.SourceRevision); err != nil ||
		r.Kind != proposal.RevisionEntity || !exec.ValidEntityTag(req.IfMatch) {
		t.Fatalf("the change is bound to %q and sent under If-Match %q", p.SourceRevision,
			req.IfMatch)
	}

	if resp, err := fx.disp.Do(t.Context(), read, req); err != nil ||
		resp.Status != http.StatusForbidden {
		t.Errorf("the turn's read grant added the seat: %d %s, %v", resp.Status, resp.Body, err)
	}
	resp, err := fx.disp.Do(t.Context(), fx.derive(t, approvedFor(req, authz.ActionOrgWrite)), req)
	if err != nil || resp.Status != http.StatusCreated {
		t.Fatalf("the approved seat answered %d %s, %v", resp.Status, resp.Body, err)
	}
	if !seated(unitNamed(t, fx, "engineering"), "crewlet-swe") {
		t.Fatal("the approved seat is not in the unit")
	}

	// STALE: the same change, prepared against the unit before the seat.
	again := req
	resp, err = fx.disp.Do(t.Context(), fx.derive(t, approvedFor(again, authz.ActionOrgWrite)),
		again)
	if err != nil || resp.Status != http.StatusConflict ||
		exec.RefusalOf(resp).Code != string(httpjson.CodeRevisionAdvanced) {
		t.Errorf("a change prepared against the old unit answered %d %s, %v, want 409 %s",
			resp.Status, resp.Body, err, httpjson.CodeRevisionAdvanced)
	}
	if now, err := catalog.Revision(t.Context(), readOver(fx, read), p.SourceRevision); err != nil ||
		now == p.SourceRevision {
		t.Errorf("the unit's revision re-read as %q (%v), the one the preview bound", now, err)
	}
}

// AN AGENT SEAT'S ADDITION ASKS THE PERSON'S OWN STEP-UP: `config.org.write`
// asks the step-up window, so an approved seat on an old sign-in is refused
// `step_up_required` through the assistant exactly as at the keyboard, and the
// unit stays as it is. Mutation: give config.org.write no recency and the
// seat lands on a week-old sign-in.
func TestAnAgentSeatAsksTheStepUp(t *testing.T) {
	t.Parallel()
	fx, read := principlesApp(t)
	_, req := prepareSeat(t, fx, read)
	identity := fx.rows[fx.person]
	identity.Session.ProvedAt = time.Now().Add(-24 * time.Hour)
	fx.rows[fx.person] = identity
	g, err := deriveOrNil(fx, approvedFor(req, authz.ActionOrgWrite))
	if err == nil {
		resp, sendErr := fx.disp.Do(t.Context(), g, req)
		if sendErr != nil || resp.Status != http.StatusForbidden ||
			exec.RefusalOf(resp).Code != string(httpjson.CodeStepUpRequired) {
			t.Errorf("an approved seat on an old sign-in answered %d %s, %v, want 403 %s",
				resp.Status, resp.Body, sendErr, httpjson.CodeStepUpRequired)
		}
	}
	if seated(unitNamed(t, fx, "engineering"), "crewlet-swe") {
		t.Error("the seat was added on an old sign-in")
	}
}

// A HUMAN-ONLY ROUTE IS NEVER OFFERED. Over the route table of an App with
// every surface mounted, the generic tools cannot name a route whose verb is on
// the human-only list (inviting somebody, which answers an invitation link;
// minting a token; revealing a secret; a frame's view ticket), and can name the
// directory's ordinary edits beside them, which an administrator approves.
// Every route the list keeps out is still refused by its own row if an exec
// grant reached it anyway (TestEveryGuardedRouteAdmitsTheAssistantAsItsPerson).
// Mutation: drop iam.signin.write from the human-only list and the invitation
// is offered.
func TestAHumanOnlyRouteIsNeverOffered(t *testing.T) {
	t.Parallel()
	table, err := catalog.NewRouteTable(everySurfaceApp(t).AssistantRoutes())
	if err != nil {
		t.Fatal(err)
	}
	person := iam.Principal{Kind: iam.KindPerson, Stage: iam.StageActive,
		Grants: []iam.Grant{iam.GrantAdmin}}
	change := catalog.GenericTools(table, person, nil)[2]
	for _, never := range []string{"POST /iam/invitations", "POST /iam/credentials",
		"POST /artifacts/{id}/tickets", "PUT /secrets/{name}", "POST /iam/invalidate-all"} {
		if _, offered := table.Lookup(never); offered {
			t.Errorf("%s is on the human-only list and the generic tools may name it", never)
		}
		if _, err := change.Request(map[string]any{"route": never,
			"path": map[string]any{"id": "x", "name": "x"}, "body": map[string]any{}}); err == nil {
			t.Errorf("the generic change built a request on %s", never)
		}
	}
	for _, open := range []string{"PATCH /iam/people/{id}", "DELETE /iam/people/{id}",
		"PUT /config/units/{id}", "GET /secrets"} {
		if _, offered := table.Lookup(open); !offered {
			t.Errorf("%s is its person's own and the generic tools may not name it", open)
		}
	}
}

// A TURN ADDS THE SEAT IN SEVERAL STEPS, AND STOPS AT THE PROPOSAL.
//
// The same request asked in a real turn of the real orchestrator, the
// scripted model calling the generic tools the bound catalogue offers, as a
// model would: look the route up, read the unit, propose it back with the seat.
// The docs step sends nothing and is done; the read is the turn's own request
// through its read grant, done; the change is a proposal waiting for approval,
// carded from the server's preview under the verb the route is decided by, and
// nothing in the unit has changed. Mutation: let the surface draft a routed
// write under the tool's own act and the proposal names no verb; send the
// change instead of proposing it and the seat lands unapproved.
func TestATurnProposesTheSeatAndChangesNothing(t *testing.T) {
	t.Parallel()
	fx, _ := principlesApp(t)
	tools := &catalog.API{}
	if err := tools.Bind(fx.app.AssistantRoutes()); err != nil {
		t.Fatal(err)
	}
	unit := unitNamed(t, fx, "engineering")
	roles, _ := unit["roles"].([]any)
	unit["roles"] = append(roles, newSeat)
	unit["_summary"] = "add crewlet-swe, a senior software engineer"
	at := map[string]any{"id": "engineering"}
	person := fx.principal(t)
	rig := assistanttest.NewRig(t, evals.RigOptions{Rounds: []evals.Round{
		evals.Call("c-docs", catalog.DescribeAPITool, map[string]any{"route": unitRoute}),
		evals.Call("c-read", catalog.APIRequestTool, map[string]any{
			"route": "GET /config/units/{id}", "path": at}),
		evals.Call("c-change", catalog.ProposeAPIChangeTool, map[string]any{
			"route": unitRoute, "path": at, "body": unit}),
	}, Person: &person, Catalogue: tools, Dispatcher: fx.disp, Revoker: fx.d})
	onAppClock(rig)
	frames := askOne(t, rig, fx, "add a seat", nil)

	for tool, want := range map[string]wire.StepState{
		catalog.DescribeAPITool: wire.StepDone, catalog.APIRequestTool: wire.StepDone,
		catalog.ProposeAPIChangeTool: wire.StepWaiting,
	} {
		if got := lastStep(t, frames, tool); got.State != want {
			t.Errorf("%s's step reads %s, want %s", tool, got.State, want)
		}
	}
	var card *wire.ProposalCard
	for _, f := range frames {
		if f.Kind == wire.EventCard && f.Card != nil && f.Card.Card.Type == wire.CardProposal {
			card = f.Card.Card.Proposal
		}
	}
	if card == nil || card.Tool != catalog.ProposeAPIChangeTool || !card.StepUp ||
		card.State != wire.ProposalProposed {
		t.Fatalf("the turn's proposal card is %+v", card)
	}
	if !slices.ContainsFunc(card.Changes, func(c wire.Change) bool {
		return c.Field == "roles.crewlet-swe.name" && c.Kind == wire.ChangeAdded
	}) {
		t.Errorf("the card does not show the new seat: %+v", card.Changes)
	}
	proposals := rig.Store.Proposals()
	if len(proposals) != 1 || proposals[0].Act != string(authz.ActionOrgWrite) {
		t.Errorf("the turn recorded %+v, want one proposal decided by %s", proposals,
			authz.ActionOrgWrite)
	}
	if seated(unitNamed(t, fx, "engineering"), "crewlet-swe") {
		t.Error("the seat landed before anybody approved it")
	}
}
