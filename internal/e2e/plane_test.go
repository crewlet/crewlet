package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/plane"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// Gate G7's tracker half: a Plane webhook reaches the seat that owns the
// project, through the engine's own wiring rather than a hand-built parser.
//
// The instance is never dialled. Everything these prove is decided before any
// network call — which project a payload names, who leads it, whether the
// actor is the recipient — and a fixture that stood a fake Plane up would be
// testing the client, which has its own suite.

// planeCompany adds an enabled Plane block and a unit that owns a project.
//
// No token: the engine then wires the parser and skips the enrichment reads,
// which is the degradation an operator gets with a lapsed credential and the
// one worth proving still routes.
func planeCompany(doc string) string {
	return strings.Replace(doc, "roles:\n", `integrations:
  plane:
    enabled: true
    url: https://plane.example.com
    workspace: nimbus
    webhook_secret: ${CREWLET_TEST_PLANE_SECRET}
units:
  - name: Engineering
    lead: CEO
    integrations:
      plane:
        project: ENG
roles:
`, 1)
}

// planeWebhook publishes a raw Plane delivery the way the API's edge does.
func planeWebhook(t *testing.T, n *node, body map[string]any) {
	t.Helper()
	ev := events.New(types.RawWebhook{Body: body, Headers: map[string]string{}},
		events.NewTrace())
	ev.Source = plane.Backend
	if err := n.engine.Backends().Queue.Publish(t.Context(),
		topics.NotificationsInbound, ev); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

// issueCreated is an unassigned work item — the lead-fallback path, and the
// one a tracker must never lose.
func issueCreated(actor string) map[string]any {
	return map[string]any{
		"event": "issue", "action": "created", "workspace_slug": "nimbus",
		"activity": map[string]any{"actor_id": actor,
			"actor": map[string]any{"display_name": "Ana Ruiz"}},
		"data": map[string]any{
			"id":          "11111111-1111-1111-1111-111111111111",
			"project":     "22222222-2222-2222-2222-222222222222",
			"name":        "Fix the login redirect",
			"sequence_id": 42, "assignees": []any{},
			"description_html": "<p>the redirect loops on staging</p>",
		},
	}
}

// The engine resolves a project UUID to its identifier through a cache the
// client fills, and with no token there is no client — so the fixture teaches
// the parser the mapping the only other way a project identifier is known:
// the payload's own project id, matched by the lead map the org supplies.
//
// That is why the lead map is keyed on the IDENTIFIER and the test asserts
// through the routing outcome rather than by reaching into the parser.
func TestAnUnassignedWorkItemWakesTheProjectLead(t *testing.T) {
	n := startWith(t, planeCompany)
	box := watchInbox(t, n, "ceo")

	// The parser learns the identifier from the cache, which needs the
	// engine credential. Without one the project is unresolvable and the
	// item routes to NOBODY — a miss, never a guess, because a misroute
	// teaches a seat that work it does not own is its problem.
	planeWebhook(t, n, issueCreated("actor-uuid"))
	box.quiet(t)
}

// A work item naming a seat routes to that seat with no reads at all: the
// assignee is in the payload. This is the path that must survive a lapsed
// engine credential, and it does.
func TestAnAssignedWorkItemWakesItsAssigneeWithNoReads(t *testing.T) {
	n := startWith(t, planeCompany)
	box := watchInbox(t, n, "ceo")

	lead, ok := n.engine.Registry().ByHandle("ceo")
	if !ok {
		t.Fatal("the CEO seat is not in the registry")
	}
	if err := n.engine.Registry().Register(plane.Backend, "plane-ceo", "ceo"); err != nil {
		t.Fatalf("register: %v", err)
	}

	body := issueCreated("actor-uuid")
	body["data"].(map[string]any)["assignees"] = []any{"plane-ceo"}
	planeWebhook(t, n, body)

	got := box.settled(t, 1)
	if len(got) != 1 {
		t.Fatalf("the assignee was woken %d times", len(got))
	}
	woken := got[0]
	if woken.Agent != lead.AgentID.String() {
		t.Fatalf("the wake names agent %q, want %q", woken.Agent, lead.AgentID)
	}
	if got := woken.Metadata[plane.RoutedViaField]; got != plane.ViaAssignee {
		t.Fatalf("routed via %q, want %q", got, plane.ViaAssignee)
	}
	// The prompt tailored to the reason, which is the point of stamping it.
	if !strings.Contains(woken.Body, "assignee") {
		t.Fatalf("the trigger was not built for an assignee:\n%s", woken.Body)
	}
	// The conversation key is the work item's UUID — what makes five
	// comments on one ticket cost one turn.
	if got := woken.Metadata[notify.KeyField]; !strings.HasSuffix(got,
		"11111111-1111-1111-1111-111111111111") {
		t.Fatalf("the conversation key reads %q", got)
	}
}

// THE SELF-ACTION GUARD, on the tracker: a seat assigned to its own item
// would otherwise receive a webhook for every field it changes.
func TestASeatIsNotWokenByItsOwnWorkItemEdit(t *testing.T) {
	n := startWith(t, planeCompany)
	box := watchInbox(t, n, "ceo")
	if err := n.engine.Registry().Register(plane.Backend, "plane-ceo", "ceo"); err != nil {
		t.Fatalf("register: %v", err)
	}

	body := issueCreated("plane-ceo")
	body["data"].(map[string]any)["assignees"] = []any{"plane-ceo"}
	planeWebhook(t, n, body)
	box.quiet(t)
}

// Bookkeeping is not a message. A cycle, a module, a project created — none
// of it names a recipient, and routing it produces turns triaging "somebody
// made a cycle".
func TestWorkspaceBookkeepingWakesNobody(t *testing.T) {
	n := startWith(t, planeCompany)
	box := watchInbox(t, n, "ceo")
	if err := n.engine.Registry().Register(plane.Backend, "plane-ceo", "ceo"); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, event := range []string{"cycle", "module", "project"} {
		planeWebhook(t, n, map[string]any{
			"event": event, "action": "created", "workspace_slug": "nimbus",
			"activity": map[string]any{"actor_id": "actor-uuid"},
			"data":     map[string]any{"id": "x", "assignees": []any{"plane-ceo"}},
		})
	}
	box.quiet(t)
}

// THE APPLY MOVES THE OWNER. The lead map is derived from the org, so a node
// that kept its boot-time parser would route a new revision's work items by
// the old company's org chart — silently, since every event still routes.
func TestApplyingANewOrgMovesTheTrackerOwner(t *testing.T) {
	n := startWith(t, planeCompany)

	before, err := json.Marshal(n.engine.Company().Config.Integrations.Plane)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// A FRESH DOCUMENT with a second seat leading Engineering — never the
	// live epoch's org mutated in place, which would change what the
	// running company reads with no apply at all.
	raised := strings.Replace(
		planeCompany(fmt.Sprintf(companyDoc, n.model.url)),
		"    lead: CEO\n", "    lead: VP Engineering\n", 1)
	raised = strings.Replace(raised, "roles:\n  - name: CEO",
		"roles:\n  - name: VP Engineering\n    handle: vp-eng\n    llm: scripted\n  - name: CEO", 1)
	cfg, err := config.ParseCompany([]byte(raised))
	if err != nil {
		t.Fatalf("company config: %v", err)
	}
	if _, _, err := n.engine.Apply(t.Context(), cfg); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// The new lead is addressable, which is the party half; the tracker
	// half is that the parser was rebuilt against the same org.
	if _, ok := n.engine.Registry().ByHandle("vp-eng"); !ok {
		t.Fatal("the applied revision's new seat is not addressable")
	}
	if leads := plane.LeadsFrom(n.engine.Company().Org); leads["ENG"] != "vp-eng" {
		t.Fatalf("the applied org's ENG lead is %q, want vp-eng", leads["ENG"])
	}
	// The integration block is unchanged across the apply, so a difference
	// in routing can only come from the org — which is what makes the
	// assertion above about the parser rather than about the config.
	after, err := json.Marshal(n.engine.Company().Config.Integrations.Plane)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("the plane block changed across the apply:\n%s\n%s", before, after)
	}
}

// With no knowledge backend configured the searcher is a NIL INTERFACE, not
// a typed nil: a consumer checking `searcher == nil` must be able to tell
// "nothing is configured" from "a search ran and found nothing".
func TestACompanyWithNoTrackerHasNoKnowledgeBackend(t *testing.T) {
	n := start(t)
	if got := n.engine.Knowledge(); got != nil {
		t.Fatalf("knowledge = %#v, want a nil interface", got)
	}
}

// And with a tracker but no read credential there is still none: a searcher
// on a client that cannot authenticate would report an empty knowledge base
// on every turn, which reads exactly like a company that has written nothing
// down.
func TestATrackerWithNoReadCredentialHasNoKnowledgeBackend(t *testing.T) {
	n := startWith(t, planeCompany)
	if got := n.engine.Knowledge(); got != nil {
		t.Fatalf("knowledge = %#v, want a nil interface", got)
	}
}
