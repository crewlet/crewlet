package engine_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// WHAT FOLLOWS A PUBLISHED COMPANY, WHEN THE PUBLISH IS A HIRE.
//
// A company used to be published by exactly one gesture — a config apply —
// and everything derived from one was rebuilt on that edge. The org chart is a
// log now, so a hire, a move, a rename and a first schedule each publish a
// company too, and they arrive on a completely different path.
//
// Every case here is about the list being ONE list. A step that followed only
// the apply is a seat that exists in the org tree and nowhere else: not
// addressable, no mailbox, no project, on no dashboard, firing no schedule —
// and then all five correcting themselves at once when somebody happens to
// change a provider, which is the shape that makes a cause impossible to find.

// A HIRE IS ADDRESSABLE, WITHOUT ANY CONFIG ACTIVATION.
//
// The party registry is derived from one org and answers for it permanently,
// so a company with a new seat needs a new registry. Rebuilt only on an apply,
// every lookup of a seat hired this morning answers "nobody matches" — the
// same answer a stranger gets, so nothing fails and nothing is logged.
func TestAHiredSeatIsAddressableWithoutAnApply(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, seedCompanyDoc)})
	readChart(t, e)

	if _, ok := e.Registry().ByHandle("cfo"); ok {
		t.Fatal("the registry already answers for a seat nobody has hired")
	}
	before := e.Registry().Len()
	if err := hire(t, e, "cfo"); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.Registry().ByHandle("cfo"); !ok {
		t.Fatalf("the hire reached the org tree and not the party registry: "+
			"%d parties, was %d", e.Registry().Len(), before)
	}
}

// AND IT REACHES EVERY OPEN SOCKET.
//
// The dashboard's roster, org tree and tool catalogue are all derived from the
// company, so no event will ever correct them and an overlay merge cannot
// express a seat going away. The hook that re-sends them was wired to the
// config apply and to nothing else, so a founder hiring somebody watched the
// screen not change — and it stayed not-changed until the next activation,
// which on a company nobody is reconfiguring is never.
func TestAHireReachesTheCompanyPublishedHook(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, seedCompanyDoc)})
	readChart(t, e)

	var published atomic.Int64
	e.SetOnCompanyPublished(func(context.Context) { published.Add(1) })

	if err := hire(t, e, "cfo"); err != nil {
		t.Fatal(err)
	}
	// AT LEAST ONCE, not exactly once: a hire is TWO records — the
	// structure on the subject the whole tree arbitrates on, then the
	// seat's own content — and each of them publishes a company this node
	// serves. A dashboard that rendered the first is showing a real state
	// of the company and is meant to see the second.
	settled := published.Load()
	if settled == 0 {
		t.Fatal("a hire fired the company-published hook not at all: the seat " +
			"is in the org tree and on no open dashboard")
	}

	// AND A REFRESH THAT FINDS NOTHING NEW PUBLISHES NOTHING. This runs on
	// every committed record and on a thirty-second timer, so a hook that
	// fired unconditionally would re-send the whole roster, org tree and
	// tool catalogue to every open dashboard twice a minute for ever.
	for range 3 {
		if _, err := engine.RefreshChartForTest(t.Context(), e); err != nil {
			t.Fatalf("refresh: %v", err)
		}
	}
	if got := published.Load(); got != settled {
		t.Fatalf("three refreshes over an unchanged chart fired the hook %d "+
			"further times", got-settled)
	}
}

// A HIRED SEAT GETS A MAILBOX, and that is what makes it able to receive work
// at all.
//
// A durable subscription IS a seat's mailbox: it exists without a consumer and
// retains what is published while nothing is attached. Until something creates
// it, every event published to that seat is DROPPED rather than retained — so
// a hire whose mailbox waited for the next config activation would silently
// lose everything anybody sent the new seat in between.
func TestAHiredSeatGetsAMailbox(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, seedCompanyDoc)})
	readChart(t, e)

	if err := hire(t, e, "cfo"); err != nil {
		t.Fatal(err)
	}
	// THE BROKER'S OWN ANSWER rather than the node's set: what this is
	// about is whether the subscription exists, and the set is this
	// process's belief about that.
	deadline := time.Now().Add(20 * time.Second)
	for {
		if slices.Contains(mailboxHandles(t, e), "cfo") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the hired seat has no mailbox after 20s; the broker holds %v",
				mailboxHandles(t, e))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A SEAT'S TOOL CREDENTIALS RIDE THE CHART, so changing one reaches the seat's
// own children with no lease release and no restart.
//
// # Why this was silently broken
//
// Which children a seat runs is [seatSpecs] over its `mcp_env` and its name,
// and both of those are chart-owned. Nothing but a LEASE ACQUISITION ever
// recomputed them: the apply's refile re-cloned the registry and re-filed
// whatever the bridge was already running, so a rotated credential took effect
// when the process restarted or when the seat happened to move to another
// node. Which looks exactly like a vendor that will not accept the new key.
func TestARotatedSeatCredentialReachesItsOwnChildren(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{
		Company: parsedCompany(t, mcpCompany(false, "SEAT_TOKEN"))})
	claimed(t, e, 2)

	if got := describes(t, e, "ceo", "tracker_probe"); !strings.Contains(got, "ceo-secret") {
		t.Fatalf("the CEO's child started with %q, want its own credential", got)
	}
	rotateSeatCredential(t, e, "ceo", "ceo-rotated")

	// THE SAME SEAT, STILL HELD, still on the same lease: the child is
	// replaced and nothing else about the seat is.
	deadline := time.Now().Add(20 * time.Second)
	for {
		got := describes(t, e, "ceo", "tracker_probe")
		if strings.Contains(got, "ceo-rotated") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the CEO's child still reports %q after 20s — a chart write "+
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

// rotateSeatCredential writes one seat's tool credentials to the chart and
// waits for this node's view to carry them.
//
// THROUGH THE CHART AND NOT THROUGH AN APPLY, which is the whole point: a
// seat's `mcp_env` is chart-owned, so rotating one is a record on the chart's
// own log. A content write publishes the seat WHOLE, so this re-encodes the
// runtime document from the seat the engine is actually running rather than
// composing a fresh one — anything it left out would be a field the rotation
// silently cleared.
func rotateSeatCredential(t *testing.T, e *engine.Engine, handle, token string) {
	t.Helper()
	writer := e.ChartWriter()
	if writer == nil {
		t.Fatal("this engine runs no chart writer")
	}
	seat := e.Company().Org.Role(handle)
	if seat == nil {
		t.Fatalf("no seat %q in the published company", handle)
	}
	content := *seat
	content.MCPEnv = org.MCPEnv{"tracker": {"SEAT_TOKEN": token}}
	runtime, err := org.SeatRuntime(&content)
	if err != nil {
		t.Fatalf("encode %s's runtime: %v", handle, err)
	}
	if _, err := writer.WriteSeat(t.Context(), "test:rotate:"+handle+":"+token,
		chart.SeatContent{
			Handle: handle, Kind: chart.SeatAgent, Unit: seatUnitKey(t, e, handle),
			Name: seat.Name, Runtime: runtime,
		}); err != nil {
		t.Fatalf("rotate %s's credential: %v", handle, err)
	}
	if _, err := engine.RefreshChartForTest(t.Context(), e); err != nil {
		t.Fatalf("refresh after rotating %s: %v", handle, err)
	}
}

// seatUnitKey is the unit a seat's ROW places it in.
//
// The row and not the published tree: a content write states the unit it
// believes the seat is in and the decide REFUSES a value that disagrees with
// the stored one, so this has to be the stored answer.
func seatUnitKey(t *testing.T, e *engine.Engine, handle string) string {
	t.Helper()
	for _, row := range readChart(t, e).Seats {
		if row.Handle == handle {
			return row.UnitKey
		}
	}
	t.Fatalf("no chart row for seat %q", handle)
	return ""
}

// A COMPANY WHOSE SEATS HOLD SLACK APPS DECLARES SLACK.
//
// # What this is guarding
//
// One caller asks this, and it DELETES a surface's fleet status row on false.
// Slack's row is the only place the engine records the public base its Request
// URLs were set against — Slack serves no way to read that URL back — so that
// row is the single warning an operator ever gets that a moved address has
// stranded every agent's app.
//
// The company-level `slack:` block is working-indicator settings a company
// using Slack heavily may never write, and each agent's app lives on the SEAT.
// A seat is org chart content, so the answer has to be composed over the
// company's CHART: the settings document a revision stores has no `roles:` in
// it at all, and a walk of that answers no for every company on earth.
//
// THE SEAT IS HIRED AFTER BOOT, which is what makes this a case rather than a
// coincidence: the document this node was built from declares Slack nowhere
// and never changes, so a walk of the document cannot produce the true answer
// by accident.
func TestSlackIsDeclaredByASeatsOwnApp(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, seedCompanyDoc)})
	readChart(t, e)

	if e.Company().DeclaresIntegration("slack") {
		t.Fatal("a company using Slack nowhere was reported as declaring it, " +
			"so its status row would never be cleaned up and a later " +
			"reconnect would inherit the address of the company before it")
	}
	if err := hireWithSlack(t, e, "cfo"); err != nil {
		t.Fatal(err)
	}
	if seat := e.Company().Org.Role("cfo"); seat == nil || seat.Slack.IsZero() {
		t.Fatal("the seat's own Slack app never reached this node's chart view")
	}
	if !e.Company().DeclaresIntegration("slack") {
		t.Fatal("a company whose seat holds a Slack app was reported as not " +
			"declaring Slack, which deletes the only record of the address " +
			"that app delivers to")
	}
}

// hireWithSlack hires a seat carrying its own Slack app, on the chart.
func hireWithSlack(t *testing.T, e *engine.Engine, handle string) error {
	t.Helper()
	if err := hire(t, e, handle); err != nil {
		return err
	}
	seat := e.Company().Org.Role(handle)
	if seat == nil {
		return fmt.Errorf("the hire of %s never reached the view", handle)
	}
	content := *seat
	content.Slack = org.SlackIdentity{
		BotToken: "xoxb-" + handle, SigningSecret: "shhh-" + handle,
	}
	runtime, err := org.SeatRuntime(&content)
	if err != nil {
		return fmt.Errorf("encode %s's runtime: %w", handle, err)
	}
	if _, err := e.ChartWriter().WriteSeat(t.Context(), "test:slack:"+handle,
		chart.SeatContent{
			Handle: handle, Kind: chart.SeatAgent, Unit: seatUnitKey(t, e, handle),
			Name: seat.Name, Runtime: runtime,
		}); err != nil {
		return fmt.Errorf("give %s a slack app: %w", handle, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := engine.RefreshChartForTest(t.Context(), e); err != nil {
			return fmt.Errorf("refresh after giving %s a slack app: %w", handle, err)
		}
		if got := e.Company().Org.Role(handle); got != nil && !got.Slack.IsZero() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s's slack app never reached the view", handle)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
