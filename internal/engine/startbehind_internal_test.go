package engine

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// behindAtStartCompany has TWO seats, so the pass that takes them is one pass
// claiming more than one: a pass cut short after its first claim leaves a node
// that holds a seat and is still short of its share.
const behindAtStartCompany = `
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
  - name: CTO
    handle: cto
    llm: zulu
`

// A NODE BEHIND ITS LOG WHEN IT STARTS TAKES ITS SEATS AT THE FIRST PASS IT IS
// ADMITTED TO — not at Start's own pass, and not never.
//
// # Why this is staged rather than waited for
//
// Start runs a placement pass before it returns, and a node with a record
// unapplied on its log claims nothing on it. On a fresh node that is a race
// the node runs against itself: the object collector's first collection
// appends a read barrier to the tracker's log at about the moment Start runs,
// and the barrier is unapplied for the millisecond between its append and its
// apply. TestStartAndStopAreSurvivable lost that race once under the full
// suite and, waiting one interval from Start's return, ran out between the
// next pass's two claims. Halting the applier holds that millisecond still,
// so the pass Start runs meets it every time.
//
// # What the interval is set to, and why that changes nothing asserted
//
// A tenth of a second, so the pass after Start's comes inside the case rather
// than five seconds on: the claim is about WHICH pass takes the seats, and no
// interval makes Start's own pass admit a node that is behind.
func TestANodeBehindAtStartTakesItsSeatsAtTheFirstPassItIsAdmittedTo(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	SeedStore(t, &b)
	cfg, err := config.ParseCompany([]byte(behindAtStartCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	e, err := New(t.Context(), Options{
		Bootstrap: &b, Company: cfg, sweepEvery: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })

	q, ok := e.backends.Queue.(*jetstream.Queue)
	if !ok {
		t.Fatalf("the stream is %T, not the JetStream backend", e.backends.Queue)
	}
	spec := tracker.Domain{}.Stream()
	log, err := q.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	s := e.native.Load().log
	name := tracker.Domain{}.Name()
	running := s.Domain(name)
	if running == nil {
		t.Fatal("the tracker domain is not running")
	}
	waitUntil(t, 20*time.Second, "the node to admit seats", hydrated(t, e))
	waitUntil(t, 20*time.Second, "the applier to drain its log", running.runner.Drained)

	// THE COLLECTOR'S BARRIER, caught between its append and its apply.
	s.haltApplier(name)
	at := running.runner.Committed()
	body, err := tracker.EncodeBarrier(statelog.Envelope{
		V: statelog.BarrierVersion, Kind: statelog.BarrierKind,
		Subject: statelog.Subject{Kind: statelog.BarrierKind}, Gen: at.Generation,
		Scope: statelog.ScopeSet{Paths: []string{statelog.BarrierScope}},
	})
	if err != nil {
		t.Fatalf("encode a barrier: %v", err)
	}
	if _, _, err := log.Append(t.Context(),
		spec.SubjectPrefix+"."+statelog.BarrierKind, "", nil, body); err != nil {
		t.Fatalf("append a barrier: %v", err)
	}

	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	host := e.node.Host()
	first, ok := host.LastSweep()
	if !ok {
		t.Fatal("Start returned without running a placement pass")
	}
	// START'S OWN PASS IS WITHHELD, which is the state the test above has
	// to wait out: were it not, the case below would prove nothing.
	if !first.Withheld || len(host.Held()) != 0 {
		t.Fatalf("Start's pass over a log with a record unapplied claimed %v "+
			"(withheld: %t), so a seat attached to a copy that is behind",
			host.Held(), first.Withheld)
	}

	s.resumeApplier(t.Context(), name)
	waitUntil(t, 20*time.Second, "a placement pass this node is admitted to", func() bool {
		pass, ok := host.LastSweep()
		return ok && !pass.Withheld
	})
	if got := host.Held(); !slices.Equal(got, []string{"ceo", "cto"}) {
		t.Errorf("the first pass the node was admitted to left it holding %v, "+
			"want every seat: a node that started behind its log stays short "+
			"of its share after it has caught up", got)
	}
}
