package statelog_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// reanchorCreated is the live stream's creation instant, WITH a fraction of a
// second: the broker reports nanoseconds, and a confirmation check that only
// held at whole seconds refused the very value the CLI told the operator to
// paste back.
var reanchorCreated = time.Date(2023, 11, 14, 22, 13, 20, 123_456_789, time.UTC)

// keyedCreated is the instant the probe domain's rows were keyed to before the
// stream was rebuilt.
var keyedCreated = time.Date(2023, 11, 1, 9, 0, 0, 500_000_000, time.UTC)

func reanchorInputs() statelog.ReanchorInputs {
	return statelog.ReanchorInputs{
		Stream:           probeStream,
		StreamCreatedAt:  reanchorCreated,
		KeyedTo:          keyedCreated,
		FirstSeq:         0,
		PeersHydrated:    0,
		Position:         9_000,
		Highest:          9_000,
		RegisterReadable: true,
		Generation:       1,
		ClaimsIdentity:   true,
	}
}

func confirmed() statelog.ReanchorGuard {
	return statelog.ReanchorGuard{Confirm: statelog.ConfirmationOf(reanchorCreated)}
}

// EVERY GUARD REFUSES FOR ITS OWN REASON, and each one names a mistake that
// cannot be undone.
func TestAReanchorRefusesEveryWayItCanBeWrong(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		in    statelog.ReanchorInputs
		guard statelog.ReanchorGuard
		names string
	}{
		"no confirmation at all": {
			in: reanchorInputs(), guard: statelog.ReanchorGuard{},
			names: "no undo",
		},
		"a confirmation for another estate": {
			in:    reanchorInputs(),
			guard: statelog.ReanchorGuard{Confirm: "2020-01-01T00:00:00Z"},
			names: "wrong estate",
		},
		// THE RIGHT SECOND IS NOT THE RIGHT STREAM. Two streams rebuilt in
		// the same second are two streams, and the identity is compared at
		// the microsecond — so is the confirmation.
		"the right second of the wrong instant": {
			in:    reanchorInputs(),
			guard: statelog.ReanchorGuard{Confirm: reanchorCreated.Truncate(time.Second).Format(time.RFC3339)},
			names: statelog.ConfirmationOf(reanchorCreated),
		},
		"a stream whose instant is unknown": {
			in: func() statelog.ReanchorInputs {
				in := reanchorInputs()
				in.StreamCreatedAt = time.Time{}
				return in
			}(),
			guard: statelog.ReanchorGuard{Confirm: "0001-01-01T00:00:00Z"},
			names: "unknown",
		},
		"a peer is hydrated on the live stream": {
			in: func() statelog.ReanchorInputs {
				in := reanchorInputs()
				in.PeersHydrated = 1
				return in
			}(),
			guard: confirmed(),
			names: "identity claim is violated",
		},
		"this is not the most caught-up node": {
			in: func() statelog.ReanchorInputs {
				in := reanchorInputs()
				in.Position = 4_000
				return in
			}(),
			guard: confirmed(),
			names: "what the reanchor discards",
		},
		"the register could not be read": {
			in: func() statelog.ReanchorInputs {
				in := reanchorInputs()
				in.RegisterReadable = false
				return in
			}(),
			guard: confirmed(),
			names: "force flag",
		},
		"the last generation the packed form can carry": {
			in: func() statelog.ReanchorInputs {
				in := reanchorInputs()
				in.Generation = statelog.MaxGeneration
				return in
			}(),
			guard: confirmed(),
			names: "last the packed",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := statelog.PermitReanchor(tc.in, tc.guard)
			if !errors.Is(err, statelog.ErrReanchorRefused) {
				t.Fatalf("PermitReanchor = %v, want a refusal", err)
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("the refusal does not say %q: %v", tc.names, err)
			}
		})
	}

	// A CLEAN ONE PROCEEDS, to one past the domain's own generation.
	gen, err := statelog.PermitReanchor(reanchorInputs(), confirmed())
	if err != nil {
		t.Fatalf("a clean reanchor was refused: %v", err)
	}
	if gen != 2 {
		t.Fatalf("the new generation is %d, want 2 — it is DERIVED LOCALLY from "+
			"the domain's own checkpoint, because this verb runs when the "+
			"broker estate is exactly what was lost", gen)
	}
	// THE SAME INSTANT, SPELLED ANOTHER WAY, is the same confirmation: it is
	// parsed, never compared as text.
	elsewhere := reanchorCreated.In(time.FixedZone("UTC+2", 2*60*60)).Format(time.RFC3339Nano)
	if _, err := statelog.PermitReanchor(reanchorInputs(),
		statelog.ReanchorGuard{Confirm: elsewhere}); err != nil {
		t.Fatalf("the live instant written in another zone (%s) was refused: %v",
			elsewhere, err)
	}

	// AND A FORCE OVERRIDES THE POSITION RULE and nothing else: an
	// operator can know something the register does not say, and cannot
	// know that two hydrated peers will not diverge.
	behind := reanchorInputs()
	behind.Position = 4_000
	if _, err := statelog.PermitReanchor(behind, statelog.ReanchorGuard{
		Confirm: confirmed().Confirm, Force: true,
	}); err != nil {
		t.Fatalf("a forced reanchor was refused: %v", err)
	}
	hydrated := reanchorInputs()
	hydrated.PeersHydrated = 1
	if _, err := statelog.PermitReanchor(hydrated, statelog.ReanchorGuard{
		Confirm: confirmed().Confirm, Force: true,
	}); err == nil {
		t.Fatal("a forced reanchor ran with a hydrated peer — that is not a " +
			"judgement an operator can make, because the divergence it produces " +
			"is silent and there is no log left to reconcile from")
	}
}

// A DOMAIN THAT CLAIMS NO IDENTITY IS NOT HELD TO THE FLEET GUARDS.
//
// Both guards protect the claim that two nodes at one checkpoint hold the same
// rows, and the vectors make no such claim: every node re-anchoring its own
// copy is the recovery, and refusing the second one would strand it on a
// stream it can neither read nor write. The confirmation still binds — running
// the verb on the wrong estate is a mistake whatever the domain claims.
func TestADomainClaimingNoIdentityIsNotHeldToTheFleetGuards(t *testing.T) {
	t.Parallel()
	in := reanchorInputs()
	in.ClaimsIdentity = false
	in.PeersHydrated = 2
	in.Position = 10
	in.RegisterReadable = false
	gen, err := statelog.PermitReanchor(in, confirmed())
	if err != nil {
		t.Fatalf("a reanchor of a domain claiming no identity was refused by a "+
			"guard that protects an identity claim: %v", err)
	}
	if gen != 2 {
		t.Fatalf("the new generation is %d, want 2", gen)
	}
	if _, err := statelog.PermitReanchor(in, statelog.ReanchorGuard{
		Confirm: "2020-01-01T00:00:00Z",
	}); !errors.Is(err, statelog.ErrReanchorRefused) {
		t.Fatalf("a wrong confirmation was accepted for a domain claiming no "+
			"identity: %v", err)
	}
}

// ---- the transition's fakes ------------------------------------------ //

// reanchorLog is a live log as the transition sees it: create-only appends per
// subject, the per-subject probe, and a creation instant read on every call.
type reanchorLog struct {
	mu       sync.Mutex
	subjects map[string][]byte
	seq      uint64
	appends  int
	created  []time.Time // one per CreatedAt call; the last one repeats
	reads    int
	order    *[]string
	// lose makes the next append go unanswered, having landed or not.
	lose, loseLanded bool
}

func newReanchorLog(order *[]string, created ...time.Time) *reanchorLog {
	if len(created) == 0 {
		created = []time.Time{reanchorCreated}
	}
	return &reanchorLog{subjects: map[string][]byte{}, created: created, order: order}
}

func (l *reanchorLog) Append(_ context.Context, subject, _ string, expect *uint64,
	body []byte) (uint64, bool, error) {

	l.mu.Lock()
	defer l.mu.Unlock()
	l.appends++
	if l.order != nil {
		*l.order = append(*l.order, "append")
	}
	if l.lose {
		l.lose = false
		if l.loseLanded {
			l.seq++
			l.subjects[subject] = body
		}
		return 0, false, errors.New("the broker did not answer")
	}
	if _, held := l.subjects[subject]; held && expect != nil && *expect == 0 {
		return 0, false, &natsjs.APIError{
			ErrorCode: natsjs.JSErrCodeStreamWrongLastSequence,
			Code:      400, Description: "wrong last sequence",
		}
	}
	l.seq++
	l.subjects[subject] = body
	return l.seq, false, nil
}

func (l *reanchorLog) LastSeq(_ context.Context, subject string) (uint64, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, held := l.subjects[subject]; held {
		return l.seq, true, nil
	}
	return 0, false, nil
}

func (l *reanchorLog) CreatedAt(context.Context) (time.Time, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.order != nil {
		*l.order = append(*l.order, "identity")
	}
	i := min(l.reads, len(l.created)-1)
	l.reads++
	return l.created[i], nil
}

func (l *reanchorLog) records() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.subjects)
}

// probeGeneration is the probe domain's generation record: its own subject, and a
// body that names the generation.
type probeGeneration struct{ keeps bool }

func (p probeGeneration) GenerationRecord(f statelog.GenerationFacts) (statelog.GenerationRecord, bool, error) {
	if !p.keeps {
		return statelog.GenerationRecord{}, false, nil
	}
	return statelog.GenerationRecord{
		Subject: statelog.Subject{Kind: "generation", ID: fmt.Sprint(f.Generation)},
		OpID:    fmt.Sprintf("reanchor:%d", f.Generation),
		Payload: []byte(fmt.Sprintf(`{"gen":%d,"by":%q,"writer":%q}`,
			f.Generation, f.By, f.Writer)),
	}, true, nil
}

// reanchorConsumer records where it was moved to, and can refuse.
type reanchorConsumer struct {
	mu     sync.Mutex
	after  []uint64
	fail   error
	order  *[]string
	during func()
}

func (c *reanchorConsumer) Reset(_ context.Context, after uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.order != nil {
		*c.order = append(*c.order, "consumer")
	}
	if c.during != nil {
		c.during()
	}
	if c.fail != nil {
		return c.fail
	}
	c.after = append(c.after, after)
	return nil
}

// reanchorRunner records what the runner was re-keyed to.
type reanchorRunner struct {
	mu      sync.Mutex
	at      []statelog.Position
	created []time.Time
	order   *[]string
	during  func()
}

func (r *reanchorRunner) Reanchored(at statelog.Position, created time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.order != nil {
		*r.order = append(*r.order, "runner")
	}
	if r.during != nil {
		r.during()
	}
	r.at = append(r.at, at)
	r.created = append(r.created, created)
	return nil
}

// reanchorFixture is one domain's transition over a real store.
type reanchorFixture struct {
	db       *store.DB
	log      *reanchorLog
	consumer *reanchorConsumer
	runner   *reanchorRunner
	order    []string
}

func newReanchorFixture(t *testing.T) *reanchorFixture {
	t.Helper()
	f := &reanchorFixture{db: reanchorStore(t)}
	f.log = newReanchorLog(&f.order)
	f.consumer = &reanchorConsumer{order: &f.order}
	f.runner = &reanchorRunner{order: &f.order}
	return f
}

func (f *reanchorFixture) deps(domain statelog.Domain) statelog.ReanchorDeps {
	return statelog.ReanchorDeps{
		Domain: domain, Stream: f.log, Record: probeGeneration{keeps: true},
		Consumer: f.consumer, Runner: f.runner, DB: f.db.Replicated(),
		By: "ops-1", NodeID: "node-a",
	}
}

// reanchorStore is a node with a replicated estate and no cursor yet.
func reanchorStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"), store.Options{
		PinnedWriters: 1,
	})
	if err != nil {
		t.Fatalf("open a store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	return db
}

// seedCursor writes a domain's committed checkpoint, which is what a node that
// has been running holds.
func seedCursor(t *testing.T, db *store.DB, stream string, at statelog.Position,
	created time.Time) {

	t.Helper()
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO statelog_cursor
				(stream, generation, seq, stream_created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (stream) DO UPDATE SET
				generation = excluded.generation, seq = excluded.seq,
				stream_created_at = excluded.stream_created_at`,
			stream, at.Generation, at.Seq, store.EncodeTime(created),
			store.EncodeTime(created))
		return err
	}); err != nil {
		t.Fatalf("seed %s's cursor: %v", stream, err)
	}
}

// cursorOf reads a stream's committed checkpoint back, whole.
func cursorOf(t *testing.T, db *store.DB, stream string) (statelog.Position, time.Time) {
	t.Helper()
	at, created, found, err := statelog.CursorFor(t.Context(), db.Replicated(), stream)
	if err != nil {
		t.Fatalf("read %s's cursor: %v", stream, err)
	}
	if !found {
		t.Fatalf("%s has no cursor", stream)
	}
	return at, created
}

// secondProbeDomain is a second domain on its own log, for the one case a
// single registered domain cannot show: which of several streams a reanchor
// moves.
type secondProbeDomain struct{ probeDomain }

const secondProbeStream = "CREWLET_SECOND_PROBE_LOG"

func (secondProbeDomain) Name() string { return "second_probe" }

func (secondProbeDomain) Stream() statelog.StreamSpec {
	spec := probeDomain{}.Stream()
	spec.Name = secondProbeStream
	spec.Subjects = []string{"crewlet.secondprobe.log.>"}
	spec.SubjectPrefix = "crewlet.secondprobe.log"
	return spec
}

// ---- the cases ------------------------------------------------------- //

// A REANCHOR MOVES THE ONE DOMAIN IT NAMED, and every other domain's
// checkpoint stays exactly where it was.
//
// The transition used to write every registered domain's checkpoint from the
// one stream the operator named: the other domains came out keyed to that
// stream's creation instant and positioned in its sequence space, so their
// next boot found their own streams "recreated" and stopped their appliers —
// and re-anchoring one of them did the same to the first. Two domains on two
// streams here, because one domain hides exactly that.
func TestAReanchorMovesOnlyTheDomainItNamed(t *testing.T) {
	t.Parallel()
	f := newReanchorFixture(t)
	if err := f.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), probeDDL)
		return err
	}); err != nil {
		t.Fatalf("create the probe tables: %v", err)
	}
	secondCreated := time.Date(2023, 10, 2, 8, 30, 0, 250_000_000, time.UTC)
	secondAt := statelog.Position{Stream: secondProbeStream, Generation: 1, Seq: 5_000}
	seedCursor(t, f.db, probeStream,
		statelog.Position{Stream: probeStream, Generation: 1, Seq: 9_000}, keyedCreated)
	seedCursor(t, f.db, secondProbeStream, secondAt, secondCreated)

	in := reanchorInputs()
	in.FirstSeq = 42
	gen, err := statelog.Reanchor(t.Context(), f.deps(probeDomain{}), in, confirmed())
	if err != nil {
		t.Fatalf("Reanchor: %v", err)
	}
	if gen != 2 {
		t.Fatalf("reanchored to generation %d, want 2", gen)
	}

	// THE NAMED DOMAIN: the next generation, one below the live stream's
	// first surviving sequence, keyed to the live instant.
	at, created := cursorOf(t, f.db, probeStream)
	if want := (statelog.Position{Stream: probeStream, Generation: 2, Seq: 41}); at != want {
		t.Fatalf("the re-anchored checkpoint is %s, want %s", at, want)
	}
	if statelog.IdentityOf(created, reanchorCreated, true) != statelog.StreamSame {
		t.Fatalf("the re-anchored checkpoint is keyed to %s, want the live %s — "+
			"the next boot would find this domain recreated again", created, reanchorCreated)
	}

	// THE OTHER DOMAIN: its generation, its sequence and its instant, untouched.
	other, otherCreated := cursorOf(t, f.db, secondProbeStream)
	if other != secondAt {
		t.Fatalf("the other domain's checkpoint moved to %s, want %s — a reanchor "+
			"of one stream positioned another in its sequence space", other, secondAt)
	}
	if !otherCreated.Equal(secondCreated.Truncate(time.Microsecond)) {
		t.Fatalf("the other domain's checkpoint is keyed to %s, want its own %s",
			otherCreated, secondCreated)
	}
	if state := statelog.IdentityOf(otherCreated, secondCreated, true); state != statelog.StreamSame {
		t.Fatalf("the other domain's own stream now reads as %s against its "+
			"checkpoint — its next boot would stop its applier", state)
	}

	// AND ITS APPLIER RUNS: a runner built over the other domain's own live
	// instant loads the untouched checkpoint and does not stop.
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain: secondProbeDomain{}, Applier: newProbeApplier(),
		Fetch: newProbeFetch(), DB: f.db.Replicated(), Generation: 1,
		StreamCreatedAt: secondCreated,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	errs := make(chan error, 1)
	go func() { errs <- runner.Run(ctx) }()
	for runner.Committed() != secondAt && runner.Stopped() == nil && ctx.Err() == nil {
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-errs
	if err := runner.Stopped(); err != nil {
		t.Fatalf("the other domain's applier stopped after a reanchor it was not "+
			"part of: %v", err)
	}
	if err := runner.StreamIdentity(); err != nil {
		t.Fatalf("the other domain's writes would refuse after a reanchor it was "+
			"not part of: %v", err)
	}
	if got := runner.Committed(); got != secondAt {
		t.Fatalf("the other domain's applier stands at %s, want %s", got, secondAt)
	}
}

// THE STEPS RUN IN THE ORDER THE CRASH MATRIX ASSUMES.
//
// The record goes out before anything is committed, so a crash after it is
// repaired by re-running; the instant is read again AFTER it, so the record's
// stream is established rather than hoped for; the consumer moves before the
// checkpoint, so a consumer that cannot be moved leaves nothing committed; and
// the runner is re-keyed only to a checkpoint that has committed.
func TestTheReanchorsStepsRunInTheOrderItsCrashMatrixAssumes(t *testing.T) {
	t.Parallel()
	f := newReanchorFixture(t)
	seedCursor(t, f.db, probeStream,
		statelog.Position{Stream: probeStream, Generation: 1, Seq: 9_000}, keyedCreated)
	generationNow := func() uint32 {
		at, _ := cursorOf(t, f.db, probeStream)
		return at.Generation
	}
	f.consumer.during = func() {
		if got := generationNow(); got != 1 {
			t.Errorf("the consumer was moved after the checkpoint (generation %d) — "+
				"a consumer that then failed to move would leave a committed "+
				"checkpoint over a consumer at the old stream's position", got)
		}
	}
	f.runner.during = func() {
		if got := generationNow(); got != 2 {
			t.Errorf("the runner was re-keyed with the checkpoint at generation %d, "+
				"want the committed 2", got)
		}
	}
	if _, err := statelog.Reanchor(t.Context(), f.deps(probeDomain{}),
		reanchorInputs(), confirmed()); err != nil {
		t.Fatalf("Reanchor: %v", err)
	}
	want := []string{"append", "identity", "consumer", "runner"}
	if strings.Join(f.order, ",") != strings.Join(want, ",") {
		t.Fatalf("the steps ran as %v, want %v — the order IS the crash matrix",
			f.order, want)
	}
	if len(f.consumer.after) != 1 || f.consumer.after[0] != 0 {
		t.Fatalf("the consumer was moved to %v, want [0] — one below a fresh "+
			"stream's first sequence", f.consumer.after)
	}
	if len(f.runner.created) != 1 || !f.runner.created[0].Equal(reanchorCreated) {
		t.Fatalf("the runner was re-keyed to %v, want the confirmed %s",
			f.runner.created, reanchorCreated)
	}
}

// A CONSUMER THAT CANNOT BE MOVED LEAVES NOTHING COMMITTED.
//
// The broker will not move a consumer's start on its own, and one left at the
// old checkpoint on a rebuilt stream starts past every record the applier then
// waits for. Committed over it, the domain would resume into a hole.
func TestAReanchorWhoseConsumerCannotMoveCommitsNothing(t *testing.T) {
	t.Parallel()
	f := newReanchorFixture(t)
	before := statelog.Position{Stream: probeStream, Generation: 1, Seq: 9_000}
	seedCursor(t, f.db, probeStream, before, keyedCreated)
	f.consumer.fail = errors.New("the metadata group did not answer")

	_, err := statelog.Reanchor(t.Context(), f.deps(probeDomain{}), reanchorInputs(), confirmed())
	if err == nil || !strings.Contains(err.Error(), "re-running") {
		t.Fatalf("Reanchor = %v, want a failure saying a re-run repeats it", err)
	}
	if at, created := cursorOf(t, f.db, probeStream); at != before ||
		statelog.IdentityOf(created, keyedCreated, true) != statelog.StreamSame {
		t.Fatalf("the checkpoint moved to %s keyed to %s over a consumer that was "+
			"never moved", at, created)
	}
	if len(f.runner.at) != 0 {
		t.Fatalf("the runner was re-keyed to %v by a transition that committed nothing",
			f.runner.at)
	}
}

// A STREAM REBUILT AGAIN WHILE THE REANCHOR RAN COMMITS NOTHING, and the
// refusal names the instant to confirm instead.
func TestAStreamRebuiltAgainDuringAReanchorCommitsNothing(t *testing.T) {
	t.Parallel()
	f := newReanchorFixture(t)
	before := statelog.Position{Stream: probeStream, Generation: 1, Seq: 9_000}
	seedCursor(t, f.db, probeStream, before, keyedCreated)
	again := reanchorCreated.Add(time.Minute)
	f.log = newReanchorLog(&f.order, again)

	_, err := statelog.Reanchor(t.Context(), f.deps(probeDomain{}), reanchorInputs(), confirmed())
	if !errors.Is(err, statelog.ErrReanchorRefused) {
		t.Fatalf("Reanchor = %v, want a refusal", err)
	}
	if !strings.Contains(err.Error(), statelog.ConfirmationOf(again)) {
		t.Fatalf("the refusal does not name the instant to confirm now: %v", err)
	}
	if at, _ := cursorOf(t, f.db, probeStream); at != before {
		t.Fatalf("the checkpoint moved to %s, keyed to a stream the broker no "+
			"longer serves", at)
	}
	if len(f.consumer.after) != 0 || len(f.runner.at) != 0 {
		t.Fatalf("the consumer (%v) or the runner (%v) moved for a transition that "+
			"committed nothing", f.consumer.after, f.runner.at)
	}
}

// A REANCHOR INTERRUPTED AFTER ITS RECORD IS FINISHED BY RE-RUNNING IT.
//
// The re-run derives the SAME generation — nothing committed moved the
// checkpoint it derives from — races its own earlier record at an expectation
// of zero, is refused, finds the record there and carries on. One record on
// the subject, one transition.
func TestAReanchorInterruptedAfterItsRecordIsFinishedByRerunningIt(t *testing.T) {
	t.Parallel()
	f := newReanchorFixture(t)
	seedCursor(t, f.db, probeStream,
		statelog.Position{Stream: probeStream, Generation: 1, Seq: 9_000}, keyedCreated)
	f.consumer.fail = errors.New("the process died here")
	if _, err := statelog.Reanchor(t.Context(), f.deps(probeDomain{}),
		reanchorInputs(), confirmed()); err == nil {
		t.Fatal("the interrupted attempt reported success")
	}
	if f.log.records() != 1 {
		t.Fatalf("the interrupted attempt left %d record(s), want its one", f.log.records())
	}

	f.consumer.fail = nil
	gen, err := statelog.Reanchor(t.Context(), f.deps(probeDomain{}), reanchorInputs(), confirmed())
	if err != nil {
		t.Fatalf("the re-run failed: %v — a record already on the subject is the "+
			"re-run racing itself, which first-writer-wins exists to settle", err)
	}
	if gen != 2 {
		t.Fatalf("the re-run moved to generation %d, want the same 2", gen)
	}
	if f.log.records() != 1 || f.log.appends != 2 {
		t.Fatalf("%d record(s) after %d append(s), want one record from two "+
			"attempts", f.log.records(), f.log.appends)
	}
	if at, _ := cursorOf(t, f.db, probeStream); at.Generation != 2 {
		t.Fatalf("the checkpoint is at %s after the re-run", at)
	}

	// AND AN APPEND NOBODY ANSWERED IS RESOLVED BY ASKING THE SUBJECT: one
	// that did not land fails the attempt rather than committing a
	// transition whose record nobody can find.
	g := newReanchorFixture(t)
	seedCursor(t, g.db, probeStream,
		statelog.Position{Stream: probeStream, Generation: 1, Seq: 9_000}, keyedCreated)
	g.log.lose = true
	if _, err := statelog.Reanchor(t.Context(), g.deps(probeDomain{}),
		reanchorInputs(), confirmed()); err == nil || !strings.Contains(err.Error(), "nothing landed") {
		t.Fatalf("an unanswered append that never landed = %v, want a failure "+
			"saying nothing landed", err)
	}
	if at, _ := cursorOf(t, g.db, probeStream); at.Generation != 1 {
		t.Fatalf("the checkpoint moved to %s with no record behind it", at)
	}
	h := newReanchorFixture(t)
	seedCursor(t, h.db, probeStream,
		statelog.Position{Stream: probeStream, Generation: 1, Seq: 9_000}, keyedCreated)
	h.log.lose, h.log.loseLanded = true, true
	if _, err := statelog.Reanchor(t.Context(), h.deps(probeDomain{}),
		reanchorInputs(), confirmed()); err != nil {
		t.Fatalf("an unanswered append that DID land failed the transition: %v", err)
	}
}

// AN EVICTED NODE CANNOT REANCHOR: the record it would append is dropped by
// every applier, and so is everything it writes after.
func TestAnEvictedNodeCannotReanchor(t *testing.T) {
	t.Parallel()
	f := newReanchorFixture(t)
	deps := f.deps(probeDomain{})
	deps.Evicted = func(context.Context) (bool, error) { return true, nil }
	_, err := statelog.Reanchor(t.Context(), deps, reanchorInputs(), confirmed())
	if !errors.Is(err, statelog.ErrReanchorRefused) {
		t.Fatalf("Reanchor on an evicted node = %v, want a refusal", err)
	}
	if !strings.Contains(err.Error(), "readmits") {
		t.Errorf("the refusal does not name the readmission: %v", err)
	}
	if f.log.appends != 0 {
		t.Fatalf("an evicted node appended %d record(s)", f.log.appends)
	}
}

// A DOMAIN THAT KEEPS NO GENERATION RECORD STILL MOVES: the vectors declare
// none, and a recreated vector log is recovered by the same checkpoint move.
func TestADomainKeepingNoGenerationRecordStillMoves(t *testing.T) {
	t.Parallel()
	f := newReanchorFixture(t)
	deps := f.deps(probeDomain{})
	deps.Record = probeGeneration{keeps: false}
	gen, err := statelog.Reanchor(t.Context(), deps, reanchorInputs(), confirmed())
	if err != nil {
		t.Fatalf("Reanchor: %v", err)
	}
	if f.log.appends != 0 {
		t.Fatalf("a domain keeping no record appended %d", f.log.appends)
	}
	if at, _ := cursorOf(t, f.db, probeStream); at.Generation != gen {
		t.Fatalf("the checkpoint is at %s, want generation %d", at, gen)
	}
}

// A REANCHOR IS REPORTED ONCE, AND THE COMPLETION NAMES WHAT IT DISCARDED —
// and which ONE domain and stream it moved.
//
// `prev_last_seq_seen` is the high-water mark of the history the reanchor
// walked away from, which is the one fact nothing after it can reconstruct.
func TestAReanchorIsReportedOnceNamingWhatItDiscarded(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	f := newReanchorFixture(t)
	deps := f.deps(probeDomain{})
	deps.Logger = slog.New(slog.NewJSONHandler(&buf, nil))
	in := reanchorInputs()
	in.Highest = 9_000

	if _, err := statelog.Reanchor(t.Context(), deps, in, confirmed()); err != nil {
		t.Fatalf("Reanchor: %v", err)
	}
	started := logRecords(t, buf.Bytes(), "statelog_reanchor_started")
	if len(started) != 1 {
		t.Fatalf("%d statelog_reanchor_started lines, want one", len(started))
	}
	done := logRecords(t, buf.Bytes(), "statelog_reanchored")
	if len(done) != 1 {
		t.Fatalf("%d statelog_reanchored lines, want one: %s", len(done), buf.String())
	}
	for key, want := range map[string]any{
		"domain":             probeDomain{}.Name(),
		"generation":         float64(2),
		"cursor":             float64(0),
		"prev_last_seq_seen": float64(9_000),
		"stream":             probeStream,
	} {
		if done[0][key] != want {
			t.Errorf("statelog_reanchored %s = %v, want %v", key, done[0][key], want)
		}
	}
	if _, listed := done[0]["streams"]; listed {
		t.Errorf("statelog_reanchored still lists `streams` — a reanchor moves one "+
			"stream, and a list reads as though it moved several: %v", done[0])
	}
	domain := probeDomain{}.Name()
	if started[0]["stream"] != probeStream || started[0]["domain"] != domain {
		t.Errorf("statelog_reanchor_started names %v/%v, want %s/%s",
			started[0]["domain"], started[0]["stream"], domain, probeStream)
	}
	detail, _ := done[0]["detail"].(string)
	for _, want := range []string{"not recovered", "no other domain", "without a restart"} {
		if !strings.Contains(detail, want) {
			t.Errorf("statelog_reanchored does not say %q: %q", want, detail)
		}
	}
}

// AN ANCHOR BELOW THE NEW GENERATION WRITES AT AN EXPECTATION OF ZERO.
//
// This is why a reanchor rewrites no object's version and no anchor: an anchor
// from before the transition is "no anchor at this generation" to the
// publisher, which asks the broker and publishes at zero against a subject the
// adopted stream has never held.
func TestAnAnchorBelowTheNewGenerationWritesAtZero(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	subject := statelog.Subject{Kind: "task", ID: "left-behind"}
	h.rows.stage(subject, statelog.Position{
		Stream: probeStream, Generation: 1, Seq: 500,
	})
	h.gen.Store(2)

	if _, err := h.write(subject, "op-after-reanchor", "hello"); err != nil {
		t.Fatalf("a write on a row from before the reanchor failed: %v", err)
	}
	expects := h.appends.expectations()
	if len(expects) != 1 {
		t.Fatalf("the write took %d append(s), want one", len(expects))
	}
	if expects[0] == nil {
		t.Fatal("an arbitrated write formed no expectation at all")
	}
	if *expects[0] != 0 {
		t.Fatalf("the write formed an expectation of %d against a subject the "+
			"new stream has never held: an anchor below the current generation "+
			"names a sequence in a dead number space, and publishing at it is "+
			"refused for ever", *expects[0])
	}
}

// A RE-ANCHORED RUNNER FOLLOWS THE ADOPTED STREAM, WITH NO RESTART.
//
// The runner that stopped on a recreated stream is the one every subsystem on
// the node holds, so the reanchor re-keys THAT runner and its loop is started
// again: it loads the checkpoint the transition committed and applies the
// adopted stream from its head, in the new generation. Before this the verdict
// could not be cleared inside the process, and a runner resumed over the new
// checkpoint would have placed the adopted stream's records in the generation
// it had just left.
//
// TWO WAYS TO MEET A REBUILT STREAM, and the runner differs between them: built
// at a boot AFTER the rebuild it already carries the live instant and stopped
// at its checkpoint, while one that was running when a live reading found the
// rebuild still carries the instant it was built with — which is the one a
// re-key has to replace, or its next run compares the new checkpoint against
// the old stream and stops again.
func TestAReanchoredRunnerFollowsTheAdoptedStream(t *testing.T) {
	t.Parallel()
	born := time.Date(2026, 9, 10, 12, 0, 0, 123_456_789, time.UTC)
	rebuilt := born.Add(time.Hour)
	for name, meet := range map[string]func(t *testing.T, h *applyHarness){
		"rebuilt between two boots": func(t *testing.T, h *applyHarness) {
			h.rebuild(probeDomain{}, rebuilt)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if err := h.runner.Run(ctx); !errors.Is(err, statelog.ErrStreamRecreated) {
				t.Fatalf("Run on the rebuilt stream = %v, want the recreation stop", err)
			}
		},
		"rebuilt under the running loop": func(t *testing.T, h *applyHarness) {
			if !h.runner.ObserveStream(rebuilt) {
				t.Fatal("a live reading of the rebuilt stream established nothing")
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newApplyHarness(t, probeDomain{})
			h.rebuild(probeDomain{}, born)
			h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
			h.fetch.offer(2, env(2, "edit", "b", "op-2", 1))
			if err := h.run(2); err != nil {
				t.Fatalf("run: %v", err)
			}
			meet(t, h)
			reanchorTheRunner(t, h, born, rebuilt)
		})
	}
}

// reanchorTheRunner re-anchors the harness's runner onto the stream rebuilt at
// rebuilt, and asserts the loop follows it.
func reanchorTheRunner(t *testing.T, h *applyHarness, born, rebuilt time.Time) {
	t.Helper()
	old := h.runner.Committed()
	if !errors.Is(h.runner.StreamIdentity(), statelog.ErrStreamRecreated) {
		t.Fatalf("before the reanchor the identity is %v, want the rebuild",
			h.runner.StreamIdentity())
	}

	// THE REANCHOR, through the real runner.
	var order []string
	in := reanchorInputs()
	in.StreamCreatedAt, in.KeyedTo, in.Generation, in.Position, in.Highest =
		rebuilt, born, old.Generation, old.Seq, old.Seq
	gen, err := statelog.Reanchor(t.Context(), statelog.ReanchorDeps{
		Domain: probeDomain{}, Stream: newReanchorLog(&order, rebuilt),
		Record: probeGeneration{keeps: true}, Consumer: &reanchorConsumer{},
		Runner: h.runner, DB: h.db.Replicated(), NodeID: "node-a",
	}, in, statelog.ReanchorGuard{Confirm: statelog.ConfirmationOf(rebuilt)})
	if err != nil {
		t.Fatalf("Reanchor: %v", err)
	}
	if err := h.runner.StreamIdentity(); err != nil {
		t.Fatalf("the re-anchored runner still refuses its writes: %v", err)
	}
	if got := h.runner.Committed(); got.Generation != gen || got.Seq != 0 {
		t.Fatalf("the re-anchored runner stands at %s, want generation %d "+
			"sequence 0", got, gen)
	}
	if !h.runner.StreamCreatedAt().Equal(rebuilt) {
		t.Fatalf("the runner is keyed to %s, want the adopted %s",
			h.runner.StreamCreatedAt(), rebuilt)
	}

	// A READING OF THE END TAKEN AGAINST THE OLD CHECKPOINT says nothing
	// about the new one: the heartbeat that read the runner before the
	// reanchor and the log after it must not refuse the adopted stream.
	if established, _ := h.runner.ObserveEnd(old, 0); established {
		t.Fatal("a reading paired with the pre-reanchor checkpoint established " +
			"a verdict, which would refuse every write until the adopted log " +
			"reached a sequence the node will never stand at again")
	}
	if err := h.runner.StreamIdentity(); err != nil {
		t.Fatalf("after a pre-reanchor reading the identity is %v", err)
	}

	// THE LOOP RESUMES on the adopted stream, from its head, in the new
	// generation — the same runner, with no rebuild.
	h.fetch.offer(1, env(1, "edit", "c", "op-3", 1))
	h.fetch.offer(2, env(2, "edit", "d", "op-4", 1))
	if err := h.run(2); err != nil {
		t.Fatalf("the re-anchored loop: %v", err)
	}
	if err := h.runner.Stopped(); err != nil {
		t.Fatalf("the re-anchored loop stopped: %v", err)
	}
	var adopted []statelog.Position
	for _, at := range h.applier.seen() {
		if at.Generation > old.Generation || at.Seq > old.Seq {
			adopted = append(adopted, at)
		}
	}
	if len(adopted) != 2 {
		t.Fatalf("applied %v from the adopted stream, want its two records", adopted)
	}
	for _, at := range adopted {
		if at.Generation != gen {
			t.Fatalf("an adopted record applied at %s, want generation %d — in the "+
				"old generation it sorts below every row it should supersede", at, gen)
		}
	}
	if at, created := cursorOf(t, h.db, probeStream); at.Generation != gen || at.Seq != 2 ||
		statelog.IdentityOf(created, rebuilt, true) != statelog.StreamSame {
		t.Fatalf("the committed checkpoint is %s keyed to %s, want generation %d "+
			"sequence 2 on the adopted stream", at, created, gen)
	}
}
