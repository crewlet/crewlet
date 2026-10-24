package engine_test

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// WHAT FOLLOWS AN APPLIED REVISION.
//
// A company is published by one gesture — a config apply, and the boot that
// installs the first one — and everything derived from it is brought up to it
// by ONE list of steps ([engine.Engine]'s followCompany). Every case here is
// about that list being run on the apply as well as at boot: a step that ran
// only at boot is a seat that exists in the org tree and nowhere else — not
// addressable, no mailbox, on no dashboard — until the process restarts, which
// is the shape that makes a cause impossible to find.

// orgCompanyDoc is a company with one unit, one seat in it and one at the
// root.
const orgCompanyDoc = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    manages: [dev]
units:
  - name: Engineering
    id: eng
    channel: c-eng
    roles:
      - name: Dev
        handle: dev
        llm: zulu
`

// withCFO is orgCompanyDoc with a CFO seat added at the root.
var withCFO = strings.Replace(orgCompanyDoc, "units:\n",
	"  - name: CFO\n    handle: cfo\n    llm: zulu\nunits:\n", 1)

// AN APPLIED REVISION IS THE RUNNING ORG, in one apply: a seat it adds is
// addressable, has a mailbox, and reaches every open dashboard.
//
// The party registry is derived from one org and answers for it permanently,
// so a company with a new seat needs a new registry; a durable subscription IS
// a seat's mailbox, and until something creates it every event published to
// that seat is DROPPED rather than retained; and the dashboard's roster is
// derived from the company, so only the published hook corrects it.
//
// Mutation: drop the mailbox step from the list the apply runs and the
// mailbox half fails; drop the parties step and the registry half does.
func TestAnAppliedRevisionIsTheRunningOrg(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, orgCompanyDoc)})

	if _, ok := e.Registry().ByHandle("cfo"); ok {
		t.Fatal("the registry already answers for a seat nobody has added")
	}
	var published atomic.Int64
	e.SetOnCompanyPublished(func(context.Context) { published.Add(1) })

	if _, _, err := e.Apply(t.Context(), parsedCompany(t, withCFO), time.Now()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if e.Company().Org.AgentSeatByHandle("cfo") == nil {
		t.Fatal("the applied revision's seat is not in the running org")
	}
	if _, ok := e.Registry().ByHandle("cfo"); !ok {
		t.Error("the apply reached the org tree and not the party registry")
	}
	if got := published.Load(); got != 1 {
		t.Errorf("one apply fired the company-published hook %d times, want once", got)
	}
	// THE BROKER'S OWN ANSWER rather than the node's set: what this is
	// about is whether the subscription exists, and the set is this
	// process's belief about that.
	deadline := time.Now().Add(20 * time.Second)
	for !slices.Contains(mailboxHandles(t, e), "cfo") {
		if time.Now().After(deadline) {
			t.Fatalf("the added seat has no mailbox after 20s; the broker holds %v",
				mailboxHandles(t, e))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A SEAT'S TOOL CREDENTIALS ARE REPLACED BY AN APPLY, so changing one reaches
// the seat's own children with no lease release and no restart.
//
// Which children a seat runs is the seat's `mcp_env` and its name. Nothing but
// a LEASE ACQUISITION used to recompute them: the apply re-filed whatever the
// bridge was already running, so a rotated credential took effect when the
// process restarted or when the seat happened to move to another node — which
// looks exactly like a vendor that will not accept the new key.
func TestARotatedSeatCredentialReachesItsOwnChildren(t *testing.T) {
	t.Parallel()
	doc := mcpCompany(false, "SEAT_TOKEN")
	e := newEngine(t, engine.Options{Company: parsedCompany(t, doc)})
	claimed(t, e, 2)

	if got := describes(t, e, "ceo", "tracker_probe"); !strings.Contains(got, "ceo-secret") {
		t.Fatalf("the CEO's child started with %q, want its own credential", got)
	}
	rotated := parsedCompany(t, strings.Replace(doc, "ceo-secret", "ceo-rotated", 1))
	if _, _, err := e.Apply(t.Context(), rotated, time.Now()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// THE SAME SEAT, STILL HELD, still on the same lease: the child is
	// replaced and nothing else about the seat is.
	deadline := time.Now().Add(20 * time.Second)
	for {
		got := describes(t, e, "ceo", "tracker_probe")
		if strings.Contains(got, "ceo-rotated") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the CEO's child still reports %q after 20s — the apply "+
				"reached the org tree and not the seat's children", got)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !slices.Contains(e.Node().Host().Held(), "ceo") {
		t.Error("the seat was released to pick up its own credential; the " +
			"whole point is that it is not")
	}
	// AND THE PEER SEAT IS UNTOUCHED. Each seat has its own bridge because
	// two children of one template publish the same tool names, and a
	// retire-then-reconcile that reached across would stop a colleague's
	// child while it was serving a turn.
	if got := describes(t, e, "cto", "tracker_probe"); !strings.Contains(got, "cto-secret") {
		t.Errorf("the CTO's child reports %q — the CEO's rotation reached it", got)
	}
}

// mailboxHandles is every seat the broker holds an INBOX mailbox for, named
// by the handle the running company gives it.
//
// THE PAIR IDENTIFIES ONE, never the subject alone: a consumer on a seat's
// inbox subject under some other group belongs to somebody else. The control
// subscription is deliberately not counted — a seat whose control
// subscription exists is not a seat whose inbox does.
//
// The subject carries the seat's ID, so this resolves each one back through
// the company. A mailbox whose seat the company no longer has is reported by
// its id: that is genuinely all this node can say about it, and dropping it
// would make a leaked mailbox invisible to the one assertion that looks.
func mailboxHandles(t *testing.T, e *engine.Engine) []string {
	t.Helper()
	subs, err := e.Backends().Queue.ListSubscriptions(t.Context(),
		topics.AgentInboxPrefix+">")
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	byID := map[uuid.UUID]string{}
	if c := e.Company(); c != nil && c.Org != nil {
		for _, s := range c.Seats() {
			byID[s.ID] = s.Handle
		}
	}
	var out []string
	for _, sub := range subs {
		id, ok := topics.SeatFromInbox(sub.Topic)
		if !ok || sub.Group != topics.AgentInboxGroup(id) {
			continue
		}
		if handle, named := byID[id]; named {
			out = append(out, handle)
			continue
		}
		out = append(out, id.String())
	}
	return out
}
