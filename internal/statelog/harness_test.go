package statelog_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The fakes below are the framework's own seams and nothing else. A real
// domain arrives eleven steps later; what is under test here is the write
// authority's branching, which is decided entirely by what these seams
// answer — so a fake that can be driven into each answer is a stronger test
// than a real domain that can only reach the easy ones.

const (
	probeStream = "CREWLET_PROBE_LOG"
	probePrefix = "crewlet.probe.log"
)

// probeDomain is a domain with one table and a strict log.
type probeDomain struct{}

func (probeDomain) Name() string { return "probe" }

func (probeDomain) Stream() statelog.StreamSpec {
	return statelog.StreamSpec{
		Name:            probeStream,
		Subjects:        []string{probePrefix + ".>"},
		SubjectPrefix:   probePrefix,
		MaxBytes:        16 << 20,
		Duplicates:      2 * time.Minute,
		Replay:          statelog.ReplayStrict,
		ArbitratedKinds: []string{"object"},
	}
}

func (probeDomain) RecordVersion() int { return 1 }

// Envelope decodes the half every build can read.
//
// A REAL DECODER, not a constant: the envelope carries the version, the
// subject, the scope and the operation id, and every branch of the apply loop
// turns on one of them. A fake that answered a constant would exercise one
// path and report the rest green.
func (probeDomain) Envelope(payload []byte) (statelog.Envelope, error) {
	var env statelog.Envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return statelog.Envelope{}, err
	}
	return env, nil
}

func (probeDomain) InstallsGate(statelog.Envelope) bool { return false }

func (probeDomain) Tables() map[string]statelog.TableClass {
	return map[string]statelog.TableClass{
		"probe_objects": statelog.Replicated,
		"probe_ops":     statelog.Local,
	}
}

func (probeDomain) DeferredTable() string { return "probe_log_deferred" }
func (probeDomain) ScopeIndex() string    { return "probe_deferred_scope" }
func (probeDomain) OpsTable() string      { return "probe_ops" }
func (probeDomain) ReadinessInput() bool  { return true }
func (probeDomain) ClaimsIdentity() bool  { return true }

// applier stands in for this node's own apply loop: what it has committed,
// and which operations it has written rows for.
type applier struct {
	mu        sync.Mutex
	committed statelog.Position
	ops       map[string]statelog.Position

	// auto makes this node apply its own record the instant it is
	// acknowledged, which is the ordinary branch. Off, the node is
	// behind and every resolution has to say so.
	auto bool

	// applyErr makes WaitApplied fail, which is what a node that has
	// stopped applying looks like from the write path.
	applyErr error

	// frozen stops WaitCommitted advancing, which is what a node that
	// cannot catch up looks like: every round re-decides against the
	// same anchor and the round budget is what ends it.
	frozen bool

	// stalled makes WaitCommitted BLOCK, which is the other half of the
	// same situation and a different failure: frozen models an applier
	// that answers instantly and never moves, and this one models an
	// applier that does not answer at all. Only the second exercises the
	// wait's own budget, and without it an unbounded wait there looks
	// exactly like a fast one.
	stalled bool
}

func newApplier() *applier {
	return &applier{
		committed: statelog.Position{Stream: probeStream, Generation: 1},
		ops:       map[string]statelog.Position{},
		auto:      true,
	}
}

// landed is what the appender calls when the broker acknowledges a record.
func (a *applier) landed(opID string, at statelog.Position) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.auto {
		return
	}
	if a.committed.Packed() < at.Packed() {
		a.committed = at
	}
	if opID != "" {
		a.ops[opID] = at
	}
}

// advance moves this node's committed position without recording an
// operation, which is what applying a PEER's record looks like.
func (a *applier) advance(at statelog.Position) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.committed = at
}

func (a *applier) Committed() statelog.Position {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.committed
}

func (a *applier) WaitCommitted(ctx context.Context, p statelog.Position) error {
	a.mu.Lock()
	stalled := a.stalled
	if !a.frozen && a.committed.Packed() < p.Packed() {
		a.committed = p
	}
	a.mu.Unlock()
	if stalled {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (a *applier) WaitApplied(ctx context.Context, _ statelog.ScopeSet, p statelog.Position) error {
	a.mu.Lock()
	err, committed := a.applyErr, a.committed
	a.mu.Unlock()
	if err != nil {
		return err
	}
	if committed.Packed() >= p.Packed() {
		return nil
	}
	// BEHIND, and the wait is what the caller's budget is spent on. The
	// context carries that budget, so blocking on it is what a real
	// applier that never catches up does.
	<-ctx.Done()
	return ctx.Err()
}

func (a *applier) Op(_ context.Context, opID string) (statelog.Position, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	at, ok := a.ops[opID]
	return at, ok, nil
}

// fakeRows hands back one snapshot the test composed.
type fakeRows struct {
	*applier
	log   statelog.Appender
	mu    sync.Mutex
	snap  statelog.Snap
	calls int

	// override, when a subject has one, is the anchor the snapshot
	// reports while this node's applier has written nothing newer for
	// it — which is how a test stages a row whose anchor is TRIMMED or
	// left behind by a reanchor, the two states the durable row genuinely
	// reaches and a derived value never does.
	override map[string]statelog.Position

	// decideErr, when set, is what the domain's own decision returns.
	decideErr error
}

func (r *fakeRows) Snapshot(ctx context.Context, subj statelog.Subject, _ statelog.ScopeSet,
	decide func(*sql.Tx) (statelog.Decision, error)) (statelog.Snap, error) {
	r.mu.Lock()
	snap, err := r.snap, r.decideErr
	staged, override := r.override[subj.String()]
	r.calls++
	r.mu.Unlock()
	// THE ANCHOR IS WHATEVER THIS NODE'S APPLIER LAST WROTE for this
	// subject. A staged override stands for a row the applier wrote
	// BEFORE a trim or a reanchor and has not written since — so it holds
	// only while the applier has nothing newer, which is the state that
	// makes the trimmed-anchor and stale-generation branches reachable at
	// all.
	snap.Anchor = r.anchorFor(ctx, subj)
	if override && snap.Anchor.Seq == 0 {
		snap.Anchor = staged
	}
	if err != nil {
		return statelog.Snap{}, err
	}
	// The fake holds no transaction, which is honest: what is under test
	// is the framework's branching on what the snapshot SAYS.
	d, derr := decide(nil)
	if derr != nil {
		return statelog.Snap{}, derr
	}
	snap.Decision = d
	return snap, nil
}

// anchorFor derives the arbitration anchor the way a real applier writes it:
// the subject's last record AT OR BELOW this node's committed position. A
// record a peer wrote and this node has not applied is not this node's
// anchor, which is exactly the state a lost race leaves behind.
func (r *fakeRows) anchorFor(ctx context.Context, subj statelog.Subject) statelog.Position {
	committed := r.applier.Committed()
	last, found, err := r.log.LastSeq(ctx, probePrefix+"."+subj.String())
	if err != nil || !found || last > committed.Seq {
		return statelog.Position{Stream: probeStream, Generation: committed.Generation}
	}
	return statelog.Position{Stream: probeStream, Generation: committed.Generation, Seq: last}
}

func (r *fakeRows) stage(subj statelog.Subject, at statelog.Position) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.override == nil {
		r.override = map[string]statelog.Position{}
	}
	r.override[subj.String()] = at
}

func (r *fakeRows) set(fn func(*statelog.Snap)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(&r.snap)
}

func (r *fakeRows) snapshots() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// fakeFence is the two fences, each drivable into its own refusal.
type fakeFence struct {
	mu       sync.Mutex
	evicted  bool
	evictErr error
	zeroErr  error
	zeroes   atomic.Int64
}

func (f *fakeFence) Evicted(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.evicted, f.evictErr
}

func (f *fakeFence) ClearForZero(_ context.Context, _ statelog.Position) error {
	f.zeroes.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.zeroErr
}

// fakeGates answers the two questions that make an absent operation mean
// something other than "somebody else won".
type fakeGates struct {
	mu      sync.Mutex
	reason  statelog.Reason
	gated   bool
	adopted time.Time
}

func (g *fakeGates) GatedAt(context.Context, statelog.Subject, string, string, statelog.Position) (statelog.Reason, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reason, g.gated, nil
}

func (g *fakeGates) AdoptedAt(context.Context) (time.Time, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.adopted, !g.adopted.IsZero(), nil
}

// countingAppender wraps the real broker so a test can assert that a fenced
// write never reached it — which is the only way to state "refused BEFORE the
// append" as an assertion rather than as a hope.
type countingAppender struct {
	inner   statelog.Appender
	applier *applier
	gen     func() uint32

	appends atomic.Int64
	lastSeq atomic.Int64

	mu sync.Mutex
	// expects is every expectation an append carried, in order. A nil
	// entry is an additive write, which forms none.
	expects []*uint64
	// failNext, when set, is returned instead of publishing — which is
	// how the ambiguous path is reached without unplugging a broker.
	failNext error
	// swallow drops the append silently and returns failNext, so the
	// record genuinely never lands.
	swallow bool
}

func (c *countingAppender) Append(ctx context.Context, subject, msgID string, expect *uint64, body []byte) (uint64, bool, error) {
	c.appends.Add(1)
	c.mu.Lock()
	fail, swallow := c.failNext, c.swallow
	c.failNext = nil
	c.expects = append(c.expects, expect)
	c.mu.Unlock()
	if fail != nil && swallow {
		return 0, false, fail
	}
	seq, dup, err := c.inner.Append(ctx, subject, msgID, expect, body)
	if err != nil {
		return seq, dup, err
	}
	c.lastSeq.Store(int64(seq))
	c.applier.landed(msgID, statelog.Position{Stream: probeStream, Generation: c.gen(), Seq: seq})
	if fail != nil {
		// THE RECORD LANDED AND THE ANSWER DID NOT, which is the whole
		// content of an ambiguous publish.
		return 0, false, fail
	}
	return seq, dup, nil
}

func (c *countingAppender) LastSeq(ctx context.Context, subject string) (uint64, bool, error) {
	return c.inner.LastSeq(ctx, subject)
}

// expectations is every expectation the publisher formed, in order.
func (c *countingAppender) expectations() []*uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*uint64(nil), c.expects...)
}

func (c *countingAppender) fail(err error, swallow bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failNext, c.swallow = err, swallow
}

// harness is one publisher over a real embedded broker.
type harness struct {
	t       *testing.T
	q       *js.Queue
	pub     *statelog.Publisher
	rows    *fakeRows
	fence   *fakeFence
	gates   *fakeGates
	applier *applier
	appends *countingAppender
	log     *js.DomainLog
	gen     atomic.Uint32
}

func newHarness(t *testing.T) *harness { return newHarnessFor(t, probeDomain{}) }

// newHarnessFor builds a publisher over a real embedded broker for one
// domain, so a case can vary what the domain DECLARES — which is what
// decides several of the write path's branches.
func newHarnessFor(t *testing.T, domain statelog.Domain) *harness {
	t.Helper()
	// A STORE DIRECTORY, ALWAYS. An embedded broker with none keeps its
	// streams in memory, and every property this framework rests on is
	// about a stream that survives.
	q, err := js.Open(t.Context(), js.Config{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open a broker: %v", err)
	}
	t.Cleanup(func() {
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("stop the broker: %v", err)
		}
	})
	spec := domain.Stream()
	if err := q.EnsureDomainStream(t.Context(), js.DomainStream{
		Name:       spec.Name,
		Subjects:   spec.Subjects,
		MaxBytes:   spec.MaxBytes,
		Duplicates: spec.Duplicates,
	}); err != nil {
		t.Fatalf("provision the log: %v", err)
	}
	log, err := q.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}

	h := &harness{t: t, q: q, log: log, applier: newApplier()}
	h.gen.Store(1)
	h.rows = &fakeRows{applier: h.applier, log: log}
	h.fence = &fakeFence{}
	h.gates = &fakeGates{}
	h.appends = &countingAppender{inner: log, applier: h.applier, gen: h.gen.Load}

	pub, err := statelog.NewPublisher(statelog.Deps{
		Domain:        domain,
		Log:           h.appends,
		Rows:          h.rows,
		Fence:         h.fence,
		Gates:         h.gates,
		Waiter:        h.applier,
		NodeID:        "node-a",
		Generation:    h.gen.Load,
		ResolveBudget: 250 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	h.pub = pub
	return h
}

// write is one ordinary arbitrated write of body onto subject.
func (h *harness) write(subject statelog.Subject, opID string, body string) (statelog.Result, error) {
	h.t.Helper()
	return h.pub.Publish(h.t.Context(), statelog.Request{
		Subject:  subject,
		Scope:    statelog.ScopeSet{Paths: []string{subject.String()}},
		OpID:     opID,
		MintedAt: time.Now(),
		Pattern:  statelog.PatternArbitrated,
		Decide: func(*sql.Tx) (statelog.Decision, error) {
			return statelog.Decision{Payload: []byte(body), Version: 1}, nil
		},
	})
}

// anchorAt stages a durable row whose anchor is seq in the current
// generation — a row the applier wrote and the world has since moved past.
func (h *harness) anchorAt(subj statelog.Subject, seq uint64) {
	h.rows.stage(subj, statelog.Position{
		Stream: probeStream, Generation: h.gen.Load(), Seq: seq,
	})
}
