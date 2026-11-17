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

// Every case here holds that the ONE list of steps a published company is
// followed by ([engine.Engine]'s followCompany) runs on an apply as well as at
// boot: a boot-only step leaves an added seat in the org tree and nowhere else.

// orgCompanyDoc is a company with one unit, one seat in it and one at the
// root.
const orgCompanyDoc = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
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
// addressable, has a mailbox (until one exists every event to the seat is
// DROPPED), and reaches every open dashboard through the published hook.
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
	// The broker's answer, not the node's set, which is only this
	// process's belief that the subscription exists.
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
// the seat's own children with no lease release and no restart; otherwise a
// rotated key looks exactly like a vendor refusing it.
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

	// The same seat, still held on the same lease: only the child is
	// replaced.
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
	// And the peer seat is untouched: each seat has its own bridge, since
	// one template's children share tool names, and a reconcile reaching
	// across would stop a colleague's child mid-turn.
	if got := describes(t, e, "cto", "tracker_probe"); !strings.Contains(got, "cto-secret") {
		t.Errorf("the CTO's child reports %q — the CEO's rotation reached it", got)
	}
}

// mailboxHandles is every seat the broker holds an INBOX mailbox for, named
// by the handle the running company gives it. Subject and group together
// identify one: a consumer on the subject under another group is somebody
// else's, and the control subscription is not an inbox. A mailbox whose seat
// the company no longer has is reported by id, so a leak stays visible.
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
