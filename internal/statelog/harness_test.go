package statelog_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
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
func (probeDomain) FeedGroup() string     { return "" }

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

	// foreign is what StreamIdentity answers: nil while this node's
	// positions are on the live stream, and a recreation once a reading
	// found them not to be.
	foreign error
}

func (a *applier) StreamIdentity() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.foreign
}

// rebuilt is what a reading of a rebuilt log establishes about this node.
func (a *applier) rebuilt() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.foreign = fmt.Errorf("%w: the probe log was rebuilt under this node",
		statelog.ErrStreamRecreated)
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

	// pinned, when a subject has one, is the anchor the snapshot reports
	// WHATEVER this node's applier has committed — the state a restored
	// log's reanchor leaves: the subject's record is at or below the
	// checkpoint, and the anchor the row holds for it is in the generation
	// before, because the new generation's checkpoint was PLACED at the log's
	// end rather than reached by consuming it. The fake's own derivation
	// reads every record at or below the checkpoint as consumed in the
	// current generation, which is exactly the invariant that transition
	// breaks.
	pinned map[string]statelog.Position

	// decideErr, when set, is what the domain's own decision returns.
	decideErr error

	// afterSnapshot, when set, runs once a snapshot has been taken and
	// before the publisher acts on it — which is how a test lands a
	// record on this node's applier between a decision and the checks
	// made about it.
	afterSnapshot func()
}

func (r *fakeRows) Snapshot(ctx context.Context, subj statelog.Subject, _ statelog.ScopeSet,
	opID string, decide func(*sql.Tx, statelog.Position) (statelog.Decision, error)) (statelog.Snap, error) {
	// THE OPERATION FIRST, exactly as the real snapshot reads it: an
	// operation this node's ledger holds is answered and not decided.
	if held, ok, _ := r.applier.Op(ctx, opID); ok {
		r.mu.Lock()
		r.calls++
		r.mu.Unlock()
		return statelog.Snap{Held: held, HeldOK: true}, nil
	}
	r.mu.Lock()
	snap, err := r.snap, r.decideErr
	staged, override := r.override[subj.String()]
	pinned, pin := r.pinned[subj.String()]
	after := r.afterSnapshot
	r.calls++
	r.mu.Unlock()
	// THE CHECKPOINT THE ROWS ARE AT, which a real snapshot reads in the
	// same transaction as the decision: the checkpoint commits with the
	// rows, so it is this node's committed position at this instant.
	snap.Checkpoint = r.applier.Committed()
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
	if pin {
		snap.Anchor = pinned
	}
	if err != nil {
		return statelog.Snap{}, err
	}
	// The fake holds no transaction, which is honest: what is under test
	// is the framework's branching on what the snapshot SAYS. The
	// checkpoint it hands the decision is the one it reports, which is the
	// real snapshot's contract: one read, stamped and returned.
	d, derr := decide(nil, snap.Checkpoint)
	if derr != nil {
		return statelog.Snap{}, derr
	}
	snap.Decision = d
	if after != nil {
		after()
	}
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

// pin makes the snapshot report at as the subject's anchor whatever the applier
// has committed — see [fakeRows.pinned].
func (r *fakeRows) pin(subj statelog.Subject, at statelog.Position) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pinned == nil {
		r.pinned = map[string]statelog.Position{}
	}
	r.pinned[subj.String()] = at
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

	// reads, when set, is what the fence's own read of the live log
	// finds — which is where a real fence hands the stream's creation
	// instant to the applier, and so where a rebuild since the last
	// heartbeat is first seen.
	reads func()

	// cursors is every cursor ClearForZero was asked about, in order.
	cursors []statelog.Position

	// zero, when set, is the REAL zero fence this fake answers
	// ClearForZero with, over whatever reads a case composes — so the
	// refusal the publisher receives is the one a domain's fence produces,
	// not one a test typed out.
	zero *statelog.ZeroFence
}

func (f *fakeFence) Evicted(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.evicted, f.evictErr
}

func (f *fakeFence) ClearForZero(ctx context.Context, cursor statelog.Position) error {
	f.zeroes.Add(1)
	f.mu.Lock()
	f.cursors = append(f.cursors, cursor)
	reads, err, zero := f.reads, f.zeroErr, f.zero
	f.mu.Unlock()
	if reads != nil {
		reads()
	}
	if zero != nil {
		return zero.ClearForZero(ctx, cursor)
	}
	return err
}

// asked is every cursor ClearForZero was asked about, in order.
func (f *fakeFence) asked() []statelog.Position {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]statelog.Position(nil), f.cursors...)
}

// fakeGates answers the two questions that make an absent operation mean
// something other than "somebody else won".
type fakeGates struct {
	mu      sync.Mutex
	reason  statelog.Reason
	gated   bool
	adopted time.Time

	// adoptedReads counts AdoptedAt calls, and adoptFrom, when non-zero,
	// is the read from which the adoption is REPORTED — which is how a
	// case lands an adoption in the middle of one write, between the
	// check its decision passed and the resolution of its append.
	adoptedReads int
	adoptFrom    int
}

func (g *fakeGates) GatedAt(context.Context, statelog.Subject, string, string, statelog.Position) (statelog.Reason, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reason, g.gated, nil
}

func (g *fakeGates) AdoptedAt(context.Context) (time.Time, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.adoptedReads++
	if g.adoptFrom > 0 && g.adoptedReads < g.adoptFrom {
		return time.Time{}, false, nil
	}
	return g.adopted, !g.adopted.IsZero(), nil
}

// adopt is what installing a donated snapshot does to this node's ledger: the
// artefact arrives with the ledger SCRUBBED, so every row this node's applier
// had written is gone, and the adoption is recorded at `at`.
func (h *harness) adopt(at time.Time) {
	h.applier.mu.Lock()
	h.applier.ops = map[string]statelog.Position{}
	h.applier.mu.Unlock()
	h.gates.mu.Lock()
	h.gates.adopted = at
	h.gates.mu.Unlock()
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
	// beforeLastSeq, when set, runs as the publisher asks the broker for a
	// subject's last message and before the broker answers — which is how
	// a case moves the log between the write's snapshot and the moment it
	// learns what the subject holds.
	beforeLastSeq func()
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
	c.mu.Lock()
	before := c.beforeLastSeq
	c.mu.Unlock()
	if before != nil {
		before()
	}
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
	metrics *metrics.Recorder
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
	// A REAL RECORDER, because what the publisher counts a refusal as is an
	// operator's only view of which remedy a fleet needs, and a case that
	// asserts it has no other witness.
	if h.metrics, err = metrics.New(); err != nil {
		t.Fatalf("recorder: %v", err)
	}

	pub, err := statelog.NewPublisher(statelog.Deps{
		Domain:        domain,
		Log:           h.appends,
		Rows:          h.rows,
		Fence:         h.fence,
		Gates:         h.gates,
		Waiter:        h.applier,
		Identity:      h.applier,
		Metrics:       h.metrics,
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
		Subject: subject,
		Scope:   statelog.ScopeSet{Paths: []string{subject.String()}},
		OpID:    opID,
		Pattern: statelog.PatternArbitrated,
		Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			return statelog.Decision{Payload: probeRecord(stamp, opID, body), Version: 1}, nil
		},
	})
}

// probeRecord is one probe-domain record carrying the stamp its decision was
// handed and the op id it is published under — the two things the publisher
// refuses a record without — plus an opaque body.
//
// THROUGH [probeDomain.Envelope]'s OWN SHAPE, because that reader is what the
// publisher decodes the record with: a helper that wrote any other shape would
// be testing a record no domain publishes.
func probeRecord(stamp statelog.Stamp, opID, body string) []byte {
	payload, err := json.Marshal(struct {
		statelog.Envelope
		Body string
	}{
		Envelope: statelog.Envelope{
			V: 1, Kind: "object", OpID: opID,
			Gen: stamp.Gen, Writer: stamp.Writer,
		},
		Body: body,
	})
	if err != nil {
		panic(fmt.Sprintf("encode a probe record: %v", err))
	}
	return payload
}

// anchorAt stages a durable row whose anchor is seq in the current
// generation — a row the applier wrote and the world has since moved past.
func (h *harness) anchorAt(subj statelog.Subject, seq uint64) {
	h.rows.stage(subj, statelog.Position{
		Stream: probeStream, Generation: h.gen.Load(), Seq: seq,
	})
}
