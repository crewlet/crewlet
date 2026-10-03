package pages_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// chartAt is the packed position on the org chart's log of the nth chart a
// case derives containers from, in the order the log holds them.
func chartAt(n int) int64 {
	return statelog.Position{Generation: 1, Seq: uint64(100 + 10*n)}.Packed()
}

// ensure writes one container's settings from one chart position and applies what
// it wrote, reporting whether it wrote anything.
func (r *roundTrip) ensure(at int64, key, name, purpose string) bool {
	r.t.Helper()
	_, changed, err := r.store.EnsureContainer(r.t.Context(), at, key, name, purpose)
	if err != nil {
		r.t.Fatalf("EnsureContainer %s: %v", key, err)
	}
	r.drain()
	return changed
}

// container reads one container back as a listing serves it.
func (r *roundTrip) container(key string) pages.Container {
	r.t.Helper()
	for _, c := range r.containers() {
		if c.Key == key {
			return c.Container
		}
	}
	r.t.Fatalf("container %s is not listed", key)
	return pages.Container{}
}

// A CONTAINER IS STAMPED WITH THE CHART POSITION ITS SETTINGS CAME FROM, and an
// older chart position applied late writes nothing.
//
// A node whose chart applier is behind used to rewrite every container's name
// and purpose back to its own old ones — the same walk-back the chart's
// projects had — because nothing on the row said which chart had written it.
func TestAnOlderChartDoesNotWalkAContainerBack(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	newer := chartAt(0) + 3

	if !r.ensure(newer, "ENG", "Platform", "ships it") {
		t.Fatal("the first write wrote nothing — want a create")
	}
	if got, want := r.container("ENG").ChartPosition, newer; got != want {
		t.Fatalf("the container is stamped %d, want the chart position's own %d", got, want)
	}
	end := r.logEnd()

	// THE EARLIER CHART, three records older, arriving second.
	if r.ensure(chartAt(0), "ENG", "Engineering", "builds it") {
		t.Error("a chart three records older than the one applied wrote — it " +
			"walks the newer names back")
	}
	if got := r.logEnd(); got != end {
		t.Errorf("the older chart position put %d record(s) on the log", got-end)
	}
	if c := r.container("ENG"); c.Name != "Platform" || c.Purpose != "ships it" {
		t.Errorf("the container reads (%q, %q), want the newer chart position's", c.Name, c.Purpose)
	}

	// AND A NEWER ONE STILL WRITES, even with nothing but the position to say:
	// a container not re-stamped would let a chart position between the two
	// walk it back.
	if !r.ensure(chartAt(1), "ENG", "Platform", "ships it") {
		t.Error("a later chart position with the same settings wrote nothing — the " +
			"stamp would stay at the older chart position")
	}
	if got, want := r.container("ENG").ChartPosition, chartAt(1); got != want {
		t.Errorf("the container is stamped %d after the later chart position, want %d", got, want)
	}
}

// A REAPPLY OF ONE CHART POSITION WRITES NOTHING, AND SETS RIGHT WHAT AN EQUAL
// CHART POSITION WALKED BACK.
//
// Every boot of every node reapplies the chart it holds, so a reapply that
// wrote would be a record per boot per container. But two nodes deriving at
// one position can race, and the one that lost can land its settings second —
// so a reapply that finds DIFFERENT settings at its own stamp writes them
// back.
func TestAReapplyWritesOnlyWhatDiffers(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	at := chartAt(0)
	r.ensure(at, "ENG", "Platform", "ships it")
	end := r.logEnd()
	if r.ensure(at, "ENG", "Platform", "ships it") {
		t.Error("reapplying one chart position wrote")
	}
	if got := r.logEnd(); got != end {
		t.Errorf("reapplying one chart position put %d record(s) on the log", got-end)
	}

	if !r.ensure(at, "ENG", "Engineering", "builds it") {
		t.Fatal("the premise: settings that differ at an equal stamp are written")
	}
	if !r.ensure(at, "ENG", "Platform", "ships it") {
		t.Error("the reapply of the current chart position did not set its settings back")
	}
	if c := r.container("ENG"); c.Name != "Platform" {
		t.Errorf("the container is named %q, want Platform", c.Name)
	}
}

// A CONTAINER WRITE MUST NAME ITS CHART POSITION. There is no honest default: a
// zero position would stamp every container as older than any chart.
func TestAContainerWriteRefusesToGuessItsChartPosition(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	end := r.logEnd()
	_, _, err := r.store.EnsureContainer(t.Context(), 0, "ENG", "Engineering", "")
	if !errors.Is(err, pages.ErrNoChartPosition) {
		t.Fatalf("a container write with no chart position = %v, want ErrNoChartPosition", err)
	}
	if got := r.logEnd(); got != end {
		t.Errorf("a refused write put %d record(s) on the log", got-end)
	}
}

// A CONTAINER'S CREATION INSTANT SURVIVES ITS UPDATES.
//
// The applier wrote the document's `created_at` from each record's own instant,
// so a listing — which reads the document — reported a container as created
// whenever it was last renamed, while the row's own column kept the first.
func TestAContainerKeepsItsCreationThroughAnUpdate(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.ensure(chartAt(0), "ENG", "Engineering", "")
	created := r.container("ENG").CreatedAt
	if created.IsZero() {
		t.Fatal("the created container has no creation instant")
	}
	// A LATER RECORD, which the broker stores at a later instant.
	time.Sleep(10 * time.Millisecond)
	r.ensure(chartAt(1), "ENG", "Platform", "")
	if got := r.container("ENG").CreatedAt; !got.Equal(created) {
		t.Errorf("the container reads as created at %s after an update, want %s",
			got, created)
	}
}

// A CONTAINER RECORD IS WRITTEN AT THE VERSION THAT CARRIES ITS POSITION, AND
// NOTHING ELSE IS RAISED.
//
// A build that reads only version 1 decodes a container's settings by dropping
// the field it does not know and applies the rest — the walk-back the position
// exists to stop, on the node that cannot read it. At version 3 that build
// retains the record instead. Every other shape stays at 1, because a retained
// record holds back every later record nested under its scope.
func TestAContainerRecordCarriesTheVersionThatAddedItsPosition(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if got := (pages.Domain{}).RecordVersion(); got != 3 {
		t.Fatalf("this build reads record version %d, want 3", got)
	}
	r.ensure(chartAt(0), "ENG", "Engineering", "")
	if env := r.envelopeAt(r.logEnd()); env.V != 3 {
		t.Errorf("a container record carries version %d, want 3", env.V)
	}
	r.write(pages.Actor{Handle: "ops-1", Kind: pages.AuthorOperator},
		pages.NewPage{Container: "ENG", Title: "a page"})
	if env := r.envelopeAt(r.logEnd()); env.V != 1 {
		t.Errorf("a page record carries version %d, want 1 — raising every "+
			"shape stalls an older node's whole knowledge base for the upgrade", env.V)
	}

	// THE TWO RECORDS NO DECIDE BUILDS, each with an envelope of its own —
	// which is where "every record is written at this build's version"
	// hides. Both were once raised with the constant.
	barrier, err := pages.EncodeBarrier(statelog.Envelope{Kind: statelog.BarrierKind})
	if err != nil {
		t.Fatalf("encode a barrier: %v", err)
	}
	if env, err := pages.DecodeEnvelope(barrier); err != nil || env.V != 1 {
		t.Errorf("a barrier carries version %d (%v), want 1 — an older node "+
			"retains every one, a deferral row per linearizable read", env.V, err)
	}
	generation, _, err := pages.GenerationRecord{}.GenerationRecord(statelog.GenerationFacts{
		Generation: 2, By: "ops-1", Writer: "node-a", At: wednesday,
	})
	if err != nil {
		t.Fatalf("encode a generation: %v", err)
	}
	if env, err := pages.DecodeEnvelope(generation.Payload); err != nil || env.V != 1 {
		t.Errorf("a generation record carries version %d (%v), want 1 — an older "+
			"node retains it and never makes the transition", env.V, err)
	}
}

// A BUILD THAT READS ONLY VERSION 1 RETAINS A CONTAINER RECORD RATHER THAN
// APPLYING HALF OF IT — through the real framework loop, over the real log.
//
// And it goes on applying the records it can read: a page in another
// container is not held back by the one it had to retain.
func TestAnOlderBuildRetainsAContainerRecord(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.ensure(chartAt(0), "ENG", "Engineering", "builds it")
	r.write(pages.Actor{Handle: "ops-1", Kind: pages.AuthorOperator},
		pages.NewPage{Container: "OPS", Title: "runbook"})
	// A BARRIER, which every build must apply: it is appended for every
	// linearizable read, on every node, through the whole upgrade.
	barrier, err := pages.EncodeBarrier(statelog.Envelope{Kind: statelog.BarrierKind})
	if err != nil {
		t.Fatalf("encode a barrier: %v", err)
	}
	r.appendSigned(pages.Domain{}.Stream().SubjectPrefix+"."+
		pages.BarrierSubject().String(), "", barrier)
	count := r.olderNodeApplies()
	if n := count(`SELECT COUNT(*) FROM pages_containers WHERE key = 'ENG'`); n != 0 {
		t.Errorf("the older node applied the container record — %d row(s), with "+
			"no position on it and no guard behind it", n)
	}
	if n := count(`SELECT COUNT(*) FROM pages_log_deferred`); n != 1 {
		t.Errorf("the older node retained %d record(s), want the container's one "+
			"— and never the barrier", n)
	}
	if n := count(`SELECT COUNT(*) FROM pages_heads WHERE container = 'OPS'`); n != 1 {
		t.Errorf("the older node holds %d page(s) in OPS, want the one it can read", n)
	}
}

// olderNodeApplies runs a node of a build that reads only record version 1
// over this harness's log, through the real framework loop, until it has
// consumed everything on it — and hands back a counter over its rows.
func (r *roundTrip) olderNodeApplies() func(query string) int {
	r.t.Helper()
	older := r.newOlderNode()
	older.run(versionOneBuild{}, nil)
	return older.count
}

// olderNode is a second node over this harness's log, on its own store: a
// build that reads only record version 1 until it is upgraded.
type olderNode struct {
	r  *roundTrip
	db *store.DB

	// next is the first sequence its next loop has not been handed.
	next uint64
}

// newOlderNode opens the node's own store.
func (r *roundTrip) newOlderNode() *olderNode {
	t := r.t
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "older.db"),
		store.Options{PinnedWriters: 1})
	if err != nil {
		t.Fatalf("open the older node's store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &olderNode{r: r, db: db, next: 1}
}

// run drives the node's framework loop as domain until it has consumed
// everything on the log and settled holds — the loop's own start included,
// which is where a build that reads more reprocesses what it retained.
func (o *olderNode) run(domain statelog.Domain, settled func() bool) {
	t := o.r.t
	t.Helper()
	end := o.r.logEnd()
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain:   domain,
		Verifier: testVerifier(t, pages.Domain{}),
		Applier:  pages.NewApplier("node-older", nil, nil),
		Fetch:    &logFetch{log: o.r.log, next: o.next},
		Log:      o.r.log,
		Node:     o.db,
		DB:       o.db.Replicated(),
	})
	if err != nil {
		t.Fatalf("build the older node's applier: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	deadline := time.Now().Add(20 * time.Second)
	for runner.Committed().Seq < end || (settled != nil && !settled()) {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("the older node reached %d of %d", runner.Committed().Seq, end)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("the older node's loop: %v", err)
	}
	o.next = end + 1
}

// count runs one counting query over the node's rows.
func (o *olderNode) count(query string) int {
	t := o.r.t
	t.Helper()
	var n int
	if err := o.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), query).Scan(&n)
	}); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// container reads one container's document off the node's own rows.
func (o *olderNode) container(key string) (pages.Container, bool) {
	t := o.r.t
	t.Helper()
	var document []byte
	err := o.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT document FROM pages_containers WHERE key = ?`, key).Scan(&document)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return pages.Container{}, false
	case err != nil:
		t.Fatalf("read container %s: %v", key, err)
	}
	c, err := pages.DecodeContainer(document)
	if err != nil {
		t.Fatalf("decode container %s: %v", key, err)
	}
	return c, true
}

// A RE-STAMP OF UNCHANGED SETTINGS CARRIES ITS STAMP AT THE VERSION THAT ADDED
// IT, and a node still on the previous build holds it back rather than
// applying it without the stamp — then lands it, stamp and all, once upgraded.
//
// A row an older build wrote carries no stamp, and every later chart position
// moves it, so the first upgraded node re-stamps every chart-named container
// on its first apply. Those records were once written at version 1, so that an
// older node would apply them and not hold back the page writes in the space.
// Applied there, the stamp is the one thing dropped — and it is the guard:
// after that node's upgrade its row is the only unstamped copy in the fleet,
// and the first older chart it applies before re-stamping walks the
// settings back for every node, since appliers apply what a writer decided.
// Retained, the record lands with its stamp the moment the node reads it.
func TestARestampIsHeldBackRatherThanAppliedWithoutItsStamp(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// THE ROW AN OLDER BUILD WROTE: the settings, and no stamp.
	r.publishRaw(pages.MutationRecord{
		RecordEnvelope: pages.RecordEnvelope{
			V: 1, OpID: statelog.NewOpID(time.Now(), "older"),
			Subject: pages.ContainerSubject("ENG"), Op: pages.OpPatch,
			CreatedAt: wednesday, Writer: "node-older",
			Scope: pages.ScopeSet{Subject: true},
		},
		Mutation: []byte(`{"v":1,"key":"ENG","name":"Engineering"}`),
	})
	r.drain()

	if !r.ensure(chartAt(1), "ENG", "Engineering", "") {
		t.Fatal("the premise: an unstamped row is re-stamped by the first chart position")
	}
	if env := r.envelopeAt(r.logEnd()); env.V != 3 {
		t.Errorf("a re-stamp of unchanged settings carries version %d, want 3 — "+
			"a build reading 1 would apply it and drop the stamp", env.V)
	}
	stamped := chartAt(1)
	if c := r.container("ENG"); c.ChartPosition != stamped {
		t.Errorf("the re-stamp stamped the row %d, want %d", c.ChartPosition, stamped)
	}
	r.write(pages.Actor{Handle: "ops-1", Kind: pages.AuthorOperator},
		pages.NewPage{Container: "ENG", Title: "a page"})

	older := r.newOlderNode()
	older.run(versionOneBuild{}, nil)
	if c, ok := older.container("ENG"); !ok || c.ChartPosition != 0 {
		t.Errorf("the older node's ENG row is (%+v, held %v), want the unstamped "+
			"one the older build wrote — it cannot have read a stamp", c, ok)
	}
	if n := older.count(`SELECT COUNT(*) FROM pages_log_deferred`); n == 0 {
		t.Error("the older node retained nothing — it applied the re-stamp " +
			"without its stamp")
	}
	if n := older.count(`SELECT COUNT(*) FROM pages_heads WHERE container = 'ENG'`); n != 0 {
		t.Errorf("the older node holds %d page(s) in ENG past a container "+
			"record it retained", n)
	}

	// UPGRADED, it lands what it held back — the stamp included — and the
	// page written in the space behind it.
	older.run(pages.Domain{}, func() bool {
		return older.count(`SELECT COUNT(*) FROM pages_log_deferred`) == 0
	})
	if c, _ := older.container("ENG"); c.ChartPosition != stamped {
		t.Errorf("the upgraded node's ENG row is stamped %d, want the re-stamp's "+
			"%d — its guard has nothing to refuse a stale chart with",
			c.ChartPosition, stamped)
	}
	if n := older.count(`SELECT COUNT(*) FROM pages_heads WHERE container = 'ENG'`); n != 1 {
		t.Errorf("the upgraded node holds %d page(s) in ENG, want the one written there", n)
	}

	// THE GUARD STILL HOLDS on the row the re-stamp stamped: a chart position
	// older than it leaves the settings alone.
	if r.ensure(chartAt(0), "ENG", "Walked Back", "") {
		t.Error("a chart position older than the re-stamp rewrote the container")
	}
	// AND A REAL CHANGE IS WRITTEN AT THE VERSION THAT CARRIES ITS STAMP TOO.
	if !r.ensure(chartAt(2), "ENG", "Platform", "") {
		t.Fatal("a rename under a later chart position wrote nothing")
	}
	if env := r.envelopeAt(r.logEnd()); env.V != 3 {
		t.Errorf("a change to a container's settings carries version %d, want 3", env.V)
	}
}

// A VERSION-1 CONTAINER RECORD — an older node's — APPLIES AS POSITION 0, older
// than every chart this build stamps, so the next chart's write replaces it.
func TestAnOlderNodesContainerRecordAppliesAsPositionZero(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.publishRaw(pages.MutationRecord{
		RecordEnvelope: pages.RecordEnvelope{
			V: 1, OpID: statelog.NewOpID(time.Now(), "older"),
			Subject: pages.ContainerSubject("ENG"), Op: pages.OpPatch,
			CreatedAt: wednesday, Writer: "node-older",
			Scope: pages.ScopeSet{Subject: true},
		},
		Mutation: []byte(`{"v":1,"key":"ENG","name":"Engineering"}`),
	})
	r.drain()
	if c := r.container("ENG"); c.Name != "Engineering" || c.ChartPosition != 0 {
		t.Fatalf("an older node's container record applied as (%q, %d), want "+
			"(Engineering, 0)", c.Name, c.ChartPosition)
	}
	if !r.ensure(chartAt(0), "ENG", "Platform", "") {
		t.Fatal("the first stamped chart position did not replace an unstamped container")
	}
	if c := r.container("ENG"); c.Name != "Platform" {
		t.Errorf("the container is named %q, want Platform", c.Name)
	}
}

// TWO NODES APPLYING ONE CHART POSITION PUT ONE RECORD ON THE LOG — the second
// decided a create on rows that did not have the first's yet, lost the
// broker's arbitration, and re-decided on the rows the winner wrote. And it
// says it wrote nothing, because only its last round is what it did.
func TestTwoNodesEnsuringOneContainerWriteOnce(t *testing.T) {
	t.Parallel()
	a := newRoundTrip(t)
	b := newRoundTripOn(t, a.log, openNodeStore(t, "node-b.db"), "node-b")
	b.applyWhileWriting()

	if _, changed, err := a.store.EnsureContainer(t.Context(), chartAt(0),
		"ENG", "Engineering", ""); err != nil || !changed {
		t.Fatalf("node a's write = (%v, %v), want a create", changed, err)
	}
	end := a.logEnd()
	// NODE B HAS NOT APPLIED NODE A'S RECORD.
	_, changed, err := b.store.EnsureContainer(t.Context(), chartAt(0),
		"ENG", "Engineering", "")
	if err != nil {
		t.Fatalf("node b's write: %v — losing the arbitration to an identical "+
			"write is not a failure", err)
	}
	if changed {
		t.Error("node b reports it wrote — its first round lost the arbitration " +
			"and its second found the settings already there")
	}
	if got := a.logEnd(); got != end {
		t.Errorf("node b put %d record(s) on the log for settings node a had "+
			"already written", got-end)
	}
}

// A WRITE WHOSE OUTCOME IS UNKNOWN IS AN ERROR, never a success the caller
// logs as applied: the next apply is what decides it again, and only a caller
// told so can say that.
func TestAnUnknownContainerWriteIsAnError(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// THE LEDGER HAS LOST ROWS UP TO AN HOUR FROM NOW, so it can vouch for
	// no operation minted before then — which is every one this call mints.
	if err := statelog.RecordLedgerLoss(t.Context(), r.db.Replicated(),
		pages.Domain{}, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("record the ledger's watermark: %v", err)
	}
	_, changed, err := r.store.EnsureContainer(t.Context(), chartAt(0),
		"ENG", "Engineering", "")
	if err == nil {
		t.Fatalf("an unknown outcome was reported as success (changed %v)", changed)
	}
}

// versionOneBuild is this domain as a build that reads only record version 1
// sees it.
type versionOneBuild struct{ pages.Domain }

func (versionOneBuild) RecordVersion() int { return 1 }

// logFetch hands a framework loop every record on a harness's log, in order.
type logFetch struct {
	log  *js.DomainLog
	mu   sync.Mutex
	next uint64
}

func (f *logFetch) Fetch(ctx context.Context, maxMessages, _ int,
	wait time.Duration) ([]statelog.Message, error) {

	end, err := f.log.End(ctx)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	var out []statelog.Message
	for f.next <= end && len(out) < maxMessages {
		_, payload, storedAt, ok, err := f.log.At(ctx, f.next)
		if err != nil {
			f.mu.Unlock()
			return nil, err
		}
		if ok {
			out = append(out, statelog.Message{
				Seq: f.next, StoredAt: storedAt, Payload: payload,
				Ack: func() error { return nil },
			})
		}
		f.next++
	}
	f.mu.Unlock()
	if len(out) == 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(min(wait, 50*time.Millisecond)):
		}
	}
	return out, nil
}

func (f *logFetch) Pending(ctx context.Context) (uint64, error) {
	end, err := f.log.End(ctx)
	if err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if end+1 <= f.next {
		return 0, nil
	}
	return end + 1 - f.next, nil
}

// logEnd is the log's last sequence.
func (r *roundTrip) logEnd() uint64 {
	r.t.Helper()
	end, err := r.log.End(r.t.Context())
	if err != nil {
		r.t.Fatalf("read the log's end: %v", err)
	}
	return end
}

// envelopeAt decodes the envelope of the record at seq.
func (r *roundTrip) envelopeAt(seq uint64) pages.RecordEnvelope {
	r.t.Helper()
	env, err := pages.DecodeEnvelope(r.recordAt(seq))
	if err != nil {
		r.t.Fatalf("decode record %d: %v", seq, err)
	}
	return env
}

// publishRaw appends one hand-built record, as another build's writer would —
// SIGNED, because that build is still one of this fleet's nodes and seals what
// it writes under the fleet's key.
func (r *roundTrip) publishRaw(rec pages.MutationRecord) {
	r.t.Helper()
	body, err := pages.Encode(rec)
	if err != nil {
		r.t.Fatalf("encode: %v", err)
	}
	r.appendSigned(pages.Domain{}.Stream().SubjectPrefix+"."+rec.Subject.String(),
		rec.OpID, body)
}
