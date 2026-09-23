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

	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// activation is the instant of the nth configuration activation of a case, in
// the order they were made.
func activation(n int) time.Time {
	return time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Minute)
}

// ensure writes one container's settings from one activation and applies what
// it wrote, reporting whether it wrote anything.
func (r *roundTrip) ensure(at time.Time, key, name, purpose string) bool {
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

// A CONTAINER IS STAMPED WITH THE ACTIVATION ITS SETTINGS CAME FROM, and an
// older activation applied late writes nothing.
//
// A node that boots on a revision the fleet has since replaced used to rewrite
// every container's name and purpose back to its own old ones — the same
// walk-back the chart's projects had — because nothing on the row said which
// configuration had written it.
func TestAnOlderActivationDoesNotWalkAContainerBack(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	newer := activation(0).Add(300 * time.Millisecond)

	if !r.ensure(newer, "ENG", "Platform", "ships it") {
		t.Fatal("the first write wrote nothing — want a create")
	}
	if got, want := r.container("ENG").ChartEpoch, configplane.ActivationStamp(newer); got != want {
		t.Fatalf("the container is stamped %d, want the activation's own %d", got, want)
	}
	end := r.logEnd()

	// THE EARLIER ACTIVATION, arriving second, inside the same second.
	if r.ensure(activation(0), "ENG", "Engineering", "builds it") {
		t.Error("an activation 300ms older than the one applied wrote — it " +
			"walks the newer names back")
	}
	if got := r.logEnd(); got != end {
		t.Errorf("the older activation put %d record(s) on the log", got-end)
	}
	if c := r.container("ENG"); c.Name != "Platform" || c.Purpose != "ships it" {
		t.Errorf("the container reads (%q, %q), want the newer activation's", c.Name, c.Purpose)
	}

	// AND A NEWER ONE STILL WRITES, even with nothing but the epoch to say:
	// a container not re-stamped would let an activation between the two
	// walk it back.
	if !r.ensure(activation(1), "ENG", "Platform", "ships it") {
		t.Error("a later activation with the same settings wrote nothing — the " +
			"stamp would stay at the older activation")
	}
	if got, want := r.container("ENG").ChartEpoch, configplane.ActivationStamp(activation(1)); got != want {
		t.Errorf("the container is stamped %d after the later activation, want %d", got, want)
	}
}

// A REAPPLY OF ONE ACTIVATION WRITES NOTHING, AND SETS RIGHT WHAT AN EQUAL
// ACTIVATION WALKED BACK.
//
// Every boot of every node reapplies the activation it holds, so a reapply that
// wrote would be a record per boot per container. But two activations inside
// one millisecond share a stamp, and the one that lost the race can land its
// settings second — so a reapply that finds DIFFERENT settings at its own
// stamp writes them back.
func TestAReapplyWritesOnlyWhatDiffers(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	at := activation(0)
	r.ensure(at, "ENG", "Platform", "ships it")
	end := r.logEnd()
	if r.ensure(at, "ENG", "Platform", "ships it") {
		t.Error("reapplying one activation wrote")
	}
	if got := r.logEnd(); got != end {
		t.Errorf("reapplying one activation put %d record(s) on the log", got-end)
	}

	if !r.ensure(at, "ENG", "Engineering", "builds it") {
		t.Fatal("the premise: settings that differ at an equal stamp are written")
	}
	if !r.ensure(at, "ENG", "Platform", "ships it") {
		t.Error("the reapply of the current activation did not set its settings back")
	}
	if c := r.container("ENG"); c.Name != "Platform" {
		t.Errorf("the container is named %q, want Platform", c.Name)
	}
}

// A CONTAINER WRITE MUST NAME ITS ACTIVATION. There is no honest default: the
// zero instant would stamp every container as older than any configuration.
func TestAContainerWriteRefusesToGuessItsActivation(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	end := r.logEnd()
	_, _, err := r.store.EnsureContainer(t.Context(), time.Time{}, "ENG", "Engineering", "")
	if !errors.Is(err, pages.ErrNoActivation) {
		t.Fatalf("a container write with no activation = %v, want ErrNoActivation", err)
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
	r.ensure(activation(0), "ENG", "Engineering", "")
	created := r.container("ENG").CreatedAt
	if created.IsZero() {
		t.Fatal("the created container has no creation instant")
	}
	// A LATER RECORD, which the broker stores at a later instant.
	time.Sleep(10 * time.Millisecond)
	r.ensure(activation(1), "ENG", "Platform", "")
	if got := r.container("ENG").CreatedAt; !got.Equal(created) {
		t.Errorf("the container reads as created at %s after an update, want %s",
			got, created)
	}
}

// A CONTAINER RECORD IS WRITTEN AT THE VERSION THAT CARRIES ITS EPOCH, AND
// NOTHING ELSE IS RAISED.
//
// A build that reads only version 1 decodes a container's settings by dropping
// the field it does not know and applies the rest — the walk-back the epoch
// exists to stop, on the node that cannot read it. At version 2 that build
// retains the record instead. Every other shape stays at 1, because a retained
// record holds back every later record nested under its scope.
func TestAContainerRecordCarriesTheVersionThatAddedItsEpoch(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if got := (pages.Domain{}).RecordVersion(); got != 2 {
		t.Fatalf("this build reads record version %d, want 2", got)
	}
	r.ensure(activation(0), "ENG", "Engineering", "")
	if env := r.envelopeAt(r.logEnd()); env.V != 2 {
		t.Errorf("a container record carries version %d, want 2", env.V)
	}
	r.write(pages.Actor{Handle: "ops-1", Kind: pages.AuthorOperator},
		pages.NewPage{Container: "ENG", Title: "a page"})
	if env := r.envelopeAt(r.logEnd()); env.V != 1 {
		t.Errorf("a page record carries version %d, want 1 — raising every "+
			"shape stalls an older node's whole knowledge base for the upgrade", env.V)
	}

	// THE TWO RECORDS NO DECIDE BUILDS, each with an envelope of its own —
	// which is where "every record is written at this build's version"
	// hides. Both were raised to 2 with the constant.
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
	r.ensure(activation(0), "ENG", "Engineering", "builds it")
	r.write(pages.Actor{Handle: "ops-1", Kind: pages.AuthorOperator},
		pages.NewPage{Container: "OPS", Title: "runbook"})
	// A BARRIER, which every build must apply: it is appended for every
	// linearizable read, on every node, through the whole upgrade.
	barrier, err := pages.EncodeBarrier(statelog.Envelope{Kind: statelog.BarrierKind})
	if err != nil {
		t.Fatalf("encode a barrier: %v", err)
	}
	if _, _, err := r.log.Append(t.Context(), pages.Domain{}.Stream().SubjectPrefix+
		"."+pages.BarrierSubject().String(), "", nil, barrier); err != nil {
		t.Fatalf("append a barrier: %v", err)
	}
	end := r.logEnd()

	older, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "older.db"),
		store.Options{PinnedWriters: 1})
	if err != nil {
		t.Fatalf("open the older node's store: %v", err)
	}
	t.Cleanup(func() { _ = older.Close() })
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain:  versionOneBuild{},
		Applier: pages.NewApplier("node-older", nil, nil),
		Fetch:   &logFetch{log: r.log, next: 1},
		DB:      older.Replicated(),
	})
	if err != nil {
		t.Fatalf("build the older node's applier: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	deadline := time.Now().Add(20 * time.Second)
	for runner.Committed().Seq < end {
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

	count := func(query string) int {
		t.Helper()
		var n int
		if err := older.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(), query).Scan(&n)
		}); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return n
	}
	if n := count(`SELECT COUNT(*) FROM pages_containers WHERE key = 'ENG'`); n != 0 {
		t.Errorf("the older node applied the container record — %d row(s), with "+
			"no epoch on it and no guard behind it", n)
	}
	if n := count(`SELECT COUNT(*) FROM pages_log_deferred`); n != 1 {
		t.Errorf("the older node retained %d record(s), want the container's one "+
			"— and never the barrier", n)
	}
	if n := count(`SELECT COUNT(*) FROM pages_heads WHERE container = 'OPS'`); n != 1 {
		t.Errorf("the older node holds %d page(s) in OPS, want the one it can read", n)
	}
}

// A VERSION-1 CONTAINER RECORD — an older node's — APPLIES AS EPOCH 0, older
// than every configuration this build stamps, so the next activation's write
// replaces it.
func TestAnOlderNodesContainerRecordAppliesAsEpochZero(t *testing.T) {
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
	if c := r.container("ENG"); c.Name != "Engineering" || c.ChartEpoch != 0 {
		t.Fatalf("an older node's container record applied as (%q, %d), want "+
			"(Engineering, 0)", c.Name, c.ChartEpoch)
	}
	if !r.ensure(activation(0), "ENG", "Platform", "") {
		t.Fatal("the first stamped activation did not replace an unstamped container")
	}
	if c := r.container("ENG"); c.Name != "Platform" {
		t.Errorf("the container is named %q, want Platform", c.Name)
	}
}

// TWO NODES APPLYING ONE ACTIVATION PUT ONE RECORD ON THE LOG — the second
// decided a create on rows that did not have the first's yet, lost the
// broker's arbitration, and re-decided on the rows the winner wrote. And it
// says it wrote nothing, because only its last round is what it did.
func TestTwoNodesEnsuringOneContainerWriteOnce(t *testing.T) {
	t.Parallel()
	a := newRoundTrip(t)
	b := newRoundTripOn(t, a.log, openNodeStore(t, "node-b.db"), "node-b")
	b.applyWhileWriting()

	if _, changed, err := a.store.EnsureContainer(t.Context(), activation(0),
		"ENG", "Engineering", ""); err != nil || !changed {
		t.Fatalf("node a's write = (%v, %v), want a create", changed, err)
	}
	end := a.logEnd()
	// NODE B HAS NOT APPLIED NODE A'S RECORD.
	_, changed, err := b.store.EnsureContainer(t.Context(), activation(0),
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
	_, changed, err := r.store.EnsureContainer(t.Context(), activation(0),
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

// publishRaw appends one hand-built record, as another build's writer would.
func (r *roundTrip) publishRaw(rec pages.MutationRecord) {
	r.t.Helper()
	body, err := pages.Encode(rec)
	if err != nil {
		r.t.Fatalf("encode: %v", err)
	}
	subject := pages.Domain{}.Stream().SubjectPrefix + "." + rec.Subject.String()
	if _, _, err := r.log.Append(r.t.Context(), subject, rec.OpID, nil, body); err != nil {
		r.t.Fatalf("append: %v", err)
	}
}
