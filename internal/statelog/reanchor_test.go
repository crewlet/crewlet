package statelog_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
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
		PeersReanchored:  0,
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
		"a peer has already re-anchored the stream": {
			in: func() statelog.ReanchorInputs {
				in := reanchorInputs()
				in.PeersReanchored = 1
				return in
			}(),
			guard: confirmed(),
			names: "already re-anchored",
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
	plan, err := statelog.PermitReanchor(reanchorInputs(), confirmed())
	gen := plan.Generation
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
	// make a second reanchor of one stream keep the first one's rows.
	behind := reanchorInputs()
	behind.Position = 4_000
	if _, err := statelog.PermitReanchor(behind, statelog.ReanchorGuard{
		Confirm: confirmed().Confirm, Force: true,
	}); err != nil {
		t.Fatalf("a forced reanchor was refused: %v", err)
	}
	reanchored := reanchorInputs()
	reanchored.PeersReanchored = 1
	if _, err := statelog.PermitReanchor(reanchored, statelog.ReanchorGuard{
		Confirm: confirmed().Confirm, Force: true,
	}); err == nil {
		t.Fatal("a forced reanchor ran over a peer that already re-anchored the " +
			"stream — that is not a judgement an operator can make, because the " +
			"divergence it produces is silent and there is no log left to " +
			"reconcile from")
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
	in.PeersReanchored = 2
	in.Position = 10
	in.RegisterReadable = false
	plan, err := statelog.PermitReanchor(in, confirmed())
	gen := plan.Generation
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

// A REANCHOR NAMES THE CASE IT ANSWERS, AND EACH CASE PUTS THE CHECKPOINT IN
// ITS OWN PLACE.
//
// Recreated — the live stream is another stream than the rows are keyed to —
// goes one below its first surviving sequence, because the rows hold none of
// it. Restored — the SAME stream, ending below the checkpoint — goes at the
// log's end, because the rows hold every record the copy kept: one below the
// first record would replay that whole prefix into a generation that outranks
// every row, rolling every object back to the copy. A same-stream log that
// reaches the checkpoint is neither, and there is nothing to re-anchor.
func TestAReanchorNamesTheCaseItAnswers(t *testing.T) {
	t.Parallel()
	restored := func() statelog.ReanchorInputs {
		in := reanchorInputs()
		// THE SAME INSTANT AS THE ROW KEEPS IT — microseconds — which is
		// the same stream as the broker's nanoseconds.
		in.KeyedTo = reanchorCreated.Truncate(time.Microsecond)
		in.FirstSeq, in.LastSeq, in.Position = 1, 7_000, 9_000
		return in
	}
	for name, tc := range map[string]struct {
		in     statelog.ReanchorInputs
		want   statelog.ReanchorCase
		cursor uint64
		refuse string
		// skip is how many generations past the next one the plan opens:
		// the ones an evicted node abandoned.
		skip uint32
	}{
		"a recreated stream still holding its first records": {
			in: func() statelog.ReanchorInputs {
				in := reanchorInputs()
				in.FirstSeq, in.LastSeq = 1, 30
				return in
			}(),
			want: statelog.ReanchorRecreated, cursor: 0,
		},
		"a recreated stream already trimmed": {
			in: func() statelog.ReanchorInputs {
				in := reanchorInputs()
				in.FirstSeq, in.LastSeq = 42, 60
				return in
			}(),
			want: statelog.ReanchorRecreated, cursor: 41,
		},
		"a recreated stream nobody has written": {
			in: reanchorInputs(), want: statelog.ReanchorRecreated, cursor: 0,
		},
		"rows keyed to no stream at all": {
			in: func() statelog.ReanchorInputs {
				in := reanchorInputs()
				in.KeyedTo = time.Time{}
				in.FirstSeq, in.LastSeq = 5, 9
				return in
			}(),
			want: statelog.ReanchorRecreated, cursor: 4,
		},
		"a restored copy ending below the checkpoint": {
			in: restored(), want: statelog.ReanchorRestored, cursor: 7_000,
		},
		"a restored copy the trim has since shortened": {
			in: func() statelog.ReanchorInputs {
				in := restored()
				in.FirstSeq = 6_500
				return in
			}(),
			want: statelog.ReanchorRestored, cursor: 7_000,
		},
		"the same stream ending exactly at the checkpoint": {
			in: func() statelog.ReanchorInputs {
				in := restored()
				in.LastSeq = in.Position
				return in
			}(),
			refuse: "nothing to re-anchor",
		},
		"the same stream past the checkpoint": {
			in: func() statelog.ReanchorInputs {
				in := restored()
				in.LastSeq = in.Position + 50
				return in
			}(),
			refuse: "nothing to re-anchor",
		},
		// A RESTORED COPY WRITTEN PAST THE CHECKPOINT no longer ends below
		// it, and holds another record at its sequence: still the restored
		// case, followed from where the log now ends.
		"a restored copy written past the checkpoint in another history": {
			in: func() statelog.ReanchorInputs {
				in := restored()
				in.LastSeq, in.Diverged = in.Position+50, true
				return in
			}(),
			want: statelog.ReanchorRestored, cursor: 9_050,
		},
		"a restored copy written exactly to the checkpoint in another history": {
			in: func() statelog.ReanchorInputs {
				in := restored()
				in.LastSeq, in.Diverged = in.Position, true
				return in
			}(),
			want: statelog.ReanchorRestored, cursor: 9_000,
		},
		// A REBUILT STREAM holds another record at every sequence, and it is
		// the instant that says so first.
		"a recreated stream is never read as a divergence": {
			in: func() statelog.ReanchorInputs {
				in := reanchorInputs()
				in.FirstSeq, in.LastSeq, in.Diverged = 42, 60, true
				return in
			}(),
			want: statelog.ReanchorRecreated, cursor: 41,
		},
		"the same stream continuing in a generation only an evicted node held": {
			in: func() statelog.ReanchorInputs {
				in := restored()
				in.LastSeq = in.Position + 50
				in.Abandoned = in.Generation + 1
				return in
			}(),
			want: statelog.ReanchorAbandoned, cursor: 9_000, skip: 1,
		},
		"a restored copy an evicted node had already re-anchored": {
			in: func() statelog.ReanchorInputs {
				in := restored()
				in.Abandoned = in.Generation + 3
				return in
			}(),
			want: statelog.ReanchorRestored, cursor: 7_000, skip: 3,
		},
		"a recreated stream an evicted node had already re-anchored": {
			in: func() statelog.ReanchorInputs {
				in := reanchorInputs()
				in.FirstSeq, in.LastSeq = 42, 60
				in.Abandoned = in.Generation + 1
				return in
			}(),
			want: statelog.ReanchorRecreated, cursor: 41, skip: 1,
		},
		"an evicted node at this node's own generation abandoned nothing": {
			in: func() statelog.ReanchorInputs {
				in := restored()
				in.LastSeq = in.Position + 50
				in.Abandoned = in.Generation
				return in
			}(),
			refuse: "nothing to re-anchor",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			which, cursor, err := tc.in.Case()
			plan, permitErr := statelog.PermitReanchor(tc.in, confirmed())
			if tc.refuse != "" {
				for _, e := range []error{err, permitErr} {
					if !errors.Is(e, statelog.ErrReanchorRefused) ||
						!strings.Contains(e.Error(), tc.refuse) {
						t.Fatalf("= %v, want a refusal saying %q", e, tc.refuse)
					}
				}
				return
			}
			if err != nil || permitErr != nil {
				t.Fatalf("Case = %v, PermitReanchor = %v, want %s", err, permitErr, tc.want)
			}
			if which != tc.want || cursor != tc.cursor {
				t.Fatalf("Case = %s at %d, want %s at %d", which, cursor, tc.want, tc.cursor)
			}
			if !which.Valid() {
				t.Fatalf("%q is not a valid case", which)
			}
			if want := (statelog.ReanchorPlan{
				Generation: tc.in.Generation + 1 + tc.skip, Case: tc.want,
				Cursor: tc.cursor, From: tc.in.Generation,
			}); plan != want {
				t.Fatalf("PermitReanchor = %+v, want %+v — the plan is the case the "+
					"status names", plan, want)
			}
		})
	}
	for _, bad := range []statelog.ReanchorCase{"", "rebuilt", "Restored"} {
		if bad.Valid() {
			t.Errorf("%q reads as a valid case", bad)
		}
	}
}

// ---- the transition's fakes ------------------------------------------ //

// reanchorLog is a live log as the transition sees it: create-only appends per
// subject, the per-subject probe, a read of one record back by its sequence,
// and a creation instant read on every call.
type reanchorLog struct {
	mu      sync.Mutex
	bySeq   map[uint64]reanchorRecord
	last    map[string]uint64 // subject → its last sequence
	ids     map[string]uint64 // message id → the sequence it landed at
	seq     uint64
	appends int
	created []time.Time // one per CreatedAt call; the last one repeats
	reads   int
	order   *[]string
	// lose makes the next append go unanswered, having landed or not.
	lose, loseLanded bool
	// dedupeFirst is the CLUSTERED broker's order: a message id already in
	// the duplicate window is acknowledged as a duplicate of the record it
	// landed as before any expectation is checked. The solo broker — the
	// default here — checks the expectation first and refuses instead.
	dedupeFirst bool
}

// reanchorRecord is one record on the fake log.
type reanchorRecord struct {
	subject string
	body    []byte
}

func newReanchorLog(order *[]string, created ...time.Time) *reanchorLog {
	if len(created) == 0 {
		created = []time.Time{reanchorCreated}
	}
	return &reanchorLog{
		bySeq: map[uint64]reanchorRecord{}, last: map[string]uint64{},
		ids: map[string]uint64{}, created: created, order: order,
	}
}

func (l *reanchorLog) Append(_ context.Context, subject, msgID string, expect *uint64,
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
			l.landLocked(subject, msgID, body)
		}
		return 0, false, errors.New("the broker did not answer")
	}
	if seq, seen := l.ids[msgID]; seen && l.dedupeFirst {
		return seq, true, nil
	}
	if _, held := l.last[subject]; held && expect != nil && *expect == 0 {
		return 0, false, &natsjs.APIError{
			ErrorCode: natsjs.JSErrCodeStreamWrongLastSequence,
			Code:      400, Description: "wrong last sequence",
		}
	}
	return l.landLocked(subject, msgID, body), false, nil
}

// landLocked stores one record. The caller holds mu.
func (l *reanchorLog) landLocked(subject, msgID string, body []byte) uint64 {
	l.seq++
	l.bySeq[l.seq] = reanchorRecord{subject: subject, body: body}
	l.last[subject] = l.seq
	l.ids[msgID] = l.seq
	return l.seq
}

// put lands a record as a PEER'S append would, outside the transition.
func (l *reanchorLog) put(subject, msgID string, body []byte) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.landLocked(subject, msgID, body)
}

func (l *reanchorLog) LastSeq(_ context.Context, subject string) (uint64, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	seq, held := l.last[subject]
	return seq, held, nil
}

func (l *reanchorLog) At(_ context.Context, seq uint64) (string, []byte, time.Time, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, held := l.bySeq[seq]
	if !held {
		return "", nil, time.Time{}, false, nil
	}
	return rec.subject, rec.body, reanchorCreated.Add(time.Duration(seq) * time.Second), true, nil
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
	return len(l.bySeq)
}

// probeGeneration is the probe domain's generation record: its own subject, and a
// body that names the generation.
type probeGeneration struct{ keeps bool }

func (p probeGeneration) GenerationSubject(gen uint32) (statelog.Subject, bool) {
	return statelog.Subject{Kind: "generation", ID: fmt.Sprint(gen)}, p.keeps
}

func (p probeGeneration) GenerationRecord(f statelog.GenerationFacts) (statelog.GenerationRecord, bool, error) {
	if !p.keeps {
		return statelog.GenerationRecord{}, false, nil
	}
	return statelog.GenerationRecord{
		Subject: statelog.Subject{Kind: "generation", ID: fmt.Sprint(f.Generation)},
		OpID:    f.OpID(),
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
	names   []time.Time
	order   *[]string
	during  func()
}

func (r *reanchorRunner) Reanchored(at statelog.Position, created, storedAt time.Time) error {
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
	r.names = append(r.names, storedAt)
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
	plan, err := statelog.Reanchor(t.Context(), f.deps(probeDomain{}), in, confirmed())
	gen := plan.Generation
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
	secondFetch := newProbeFetch()
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain: secondProbeDomain{}, Applier: newProbeApplier(),
		Fetch: secondFetch, Log: secondFetch, Node: f.db, DB: f.db.Replicated(),
		Checkpoint:      statelog.Position{Generation: 1},
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

// A RESTORED LOG IS RE-ANCHORED AT ITS END: the consumer, the checkpoint and
// the runner all go to the last sequence the copy holds, and the completion
// line names the case.
//
// The rows already hold every record the copy kept, so nothing below its end
// is applied again — and the generation record, appended after the end was
// read, is the first thing the resumed applier reads.
func TestARestoredLogIsReanchoredAtItsEnd(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	f := newReanchorFixture(t)
	seedCursor(t, f.db, probeStream,
		statelog.Position{Stream: probeStream, Generation: 1, Seq: 9_000}, reanchorCreated)
	deps := f.deps(probeDomain{})
	deps.Logger = slog.New(slog.NewJSONHandler(&buf, nil))
	in := reanchorInputs()
	in.KeyedTo = reanchorCreated.Truncate(time.Microsecond)
	in.FirstSeq, in.LastSeq = 1, 7_000
	// THE RESTORED COPY'S LAST RECORD, at the sequence the new checkpoint
	// goes to.
	f.log.seq = 6_999
	f.log.put("probe.object.last", "op-last", []byte(`{}`))

	plan, err := statelog.Reanchor(t.Context(), deps, in, confirmed())
	if err != nil {
		t.Fatalf("Reanchor: %v", err)
	}
	want := statelog.Position{Stream: probeStream, Generation: 2, Seq: 7_000}
	if plan.Case != statelog.ReanchorRestored || plan.Cursor != want.Seq {
		t.Fatalf("the plan is %+v, want the restored case at %d", plan, want.Seq)
	}
	// THE NEW CHECKPOINT NAMES THE LOG'S OWN RECORD there — in the row and
	// in the runner — so what the applier verifies the log against from here
	// on is the history it now follows, not the one the rows were derived
	// from.
	_, _, named, _, _ := f.log.At(t.Context(), want.Seq)
	cp, _, err := statelog.CheckpointOf(t.Context(), f.db.Replicated(), probeStream)
	if err != nil {
		t.Fatalf("read the checkpoint: %v", err)
	}
	if named.IsZero() || !cp.StoredAt.Equal(named.Truncate(time.Microsecond)) {
		t.Fatalf("the reanchored checkpoint names %s, want the log's record at "+
			"%d, stored at %s", cp.StoredAt, want.Seq, named)
	}
	if len(f.runner.names) != 1 || !f.runner.names[0].Equal(named) {
		t.Fatalf("the runner was told its checkpoint names %v, want %s",
			f.runner.names, named)
	}
	if at, created := cursorOf(t, f.db, probeStream); at != want ||
		statelog.IdentityOf(created, reanchorCreated, true) != statelog.StreamSame {
		t.Fatalf("the checkpoint is %s keyed to %s, want %s keyed to the same "+
			"stream — one below the first record would replay the whole copy", at,
			created, want)
	}
	if len(f.consumer.after) != 1 || f.consumer.after[0] != want.Seq {
		t.Fatalf("the consumer was moved to %v, want [%d]", f.consumer.after, want.Seq)
	}
	if len(f.runner.at) != 1 || f.runner.at[0] != want {
		t.Fatalf("the runner was re-keyed to %v, want %s", f.runner.at, want)
	}
	for _, event := range []string{"statelog_reanchor_started", "statelog_reanchored"} {
		lines := logRecords(t, buf.Bytes(), event)
		if len(lines) != 1 || lines[0]["case"] != string(statelog.ReanchorRestored) {
			t.Fatalf("%s = %v, want one line naming the restored case", event, lines)
		}
	}
	done := logRecords(t, buf.Bytes(), "statelog_reanchored")
	if detail, _ := done[0]["detail"].(string); !strings.Contains(detail, "restored") ||
		!strings.Contains(detail, "replays none") {
		t.Errorf("statelog_reanchored does not say the copy was replayed from its "+
			"end: %q", detail)
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
	plan, err := statelog.Reanchor(t.Context(), f.deps(probeDomain{}), reanchorInputs(), confirmed())
	gen := plan.Generation
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
	plan, err := statelog.Reanchor(t.Context(), deps, reanchorInputs(), confirmed())
	gen := plan.Generation
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
		"case":               string(statelog.ReanchorRecreated),
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

// A WRITE AFTER A RESTORED LOG'S REANCHOR ARBITRATES ON THE RECORD ITS ROWS
// ALREADY HOLD.
//
// The restored case puts the new generation's checkpoint at the log's END, so
// every record the copy kept is below it and was never consumed in the new
// generation: the subject's anchor is still the one from the generation before.
// The publisher used to read that as "a peer wrote this and I have not applied
// it" and wait for the applier to reach the subject's last record — which it
// already stood past, so the wait returned at once, the fresh snapshot read the
// same anchor, and every write to every object the copy kept spent its sixteen
// rounds and came back a conflict nobody was causing.
func TestAWriteAfterARestoredReanchorArbitratesOnTheRecordItsRowsHold(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	subject := probeSubject("kept")

	// THE RESTORED COPY: the subject's own record, and another object's
	// after it, so the log ends past the subject.
	kept, _, err := h.log.Append(t.Context(), probePrefix+".object.kept", "op-old",
		nil, []byte("old"))
	if err != nil {
		t.Fatalf("the copy's record for the subject: %v", err)
	}
	end, _, err := h.log.Append(t.Context(), probePrefix+".object.other", "op-other",
		nil, []byte("other"))
	if err != nil {
		t.Fatalf("the copy's later record: %v", err)
	}

	// THE REANCHOR: generation 2, its checkpoint at the log's end, and the
	// row's anchor left where the generation before put it.
	h.gen.Store(2)
	h.applier.advance(statelog.Position{Stream: probeStream, Generation: 2, Seq: end})
	h.rows.pin(subject, statelog.Position{Stream: probeStream, Generation: 1, Seq: kept})

	res, err := h.write(subject, "op-after-restore", "new")
	if err != nil {
		t.Fatalf("a write to an object the restored copy kept = %v — the rows hold "+
			"its last record, so it is the expectation, not a position to wait for", err)
	}
	if res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("outcome = %q, want applied", res.Outcome)
	}
	expects := h.appends.expectations()
	if len(expects) != 1 || expects[0] == nil || *expects[0] != kept {
		t.Fatalf("the write appended with expectations %v, want one at %d — the "+
			"subject's last record, which the checkpoint covers", derefAll(expects), kept)
	}
	if got := h.rows.snapshots(); got != 1 {
		t.Fatalf("the write took %d snapshots, want one: nothing was behind", got)
	}
}

// A RECORD THE SNAPSHOT'S CHECKPOINT DOES NOT COVER IS WAITED FOR, never
// taken as the expectation — whatever the generation of the anchor below it.
//
// The rule above is sound only because the rows the decision read hold the
// record it expects against. A subject whose last record is past the snapshot's
// checkpoint, or a checkpoint in another generation's number space, is a
// record those rows have not seen: expecting it would publish a decision
// taken without it, and the broker would accept it.
func TestARecordTheCheckpointDoesNotCoverIsWaitedFor(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		// committed is where this node's checkpoint stands when the write
		// takes its first snapshot, given the peer's record at seq.
		committed func(seq uint64) statelog.Position
	}{
		"a peer's record past the checkpoint": {
			committed: func(seq uint64) statelog.Position {
				return statelog.Position{Stream: probeStream, Generation: 2, Seq: seq - 1}
			},
		},
		"a checkpoint still in the generation before": {
			committed: func(seq uint64) statelog.Position {
				return statelog.Position{Stream: probeStream, Generation: 1, Seq: seq + 10}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			subject := probeSubject("a")
			peer, _, err := h.log.Append(t.Context(), probePrefix+".object.a", "peer-op",
				nil, []byte("peer"))
			if err != nil {
				t.Fatalf("the peer's write: %v", err)
			}
			h.gen.Store(2)
			h.applier.advance(tc.committed(peer))
			h.rows.stage(subject, statelog.Position{Stream: probeStream, Generation: 1, Seq: 500})

			if _, err := h.write(subject, "op-1", "mine"); err != nil {
				t.Fatalf("write: %v", err)
			}
			if got := h.rows.snapshots(); got != 2 {
				t.Fatalf("the write took %d snapshot(s), want two — it must wait for "+
					"the peer's record at %d and decide again, because the first "+
					"snapshot's rows had not seen it", got, peer)
			}
		})
	}
}

// runOnce runs a runner's loop to its end and answers why it ended, with a
// budget that is its own verdict: a loop still running when it expires is one
// that did not stop.
//
// SYNCHRONOUS, rather than [applyHarness.run]'s poll of Stopped, because a
// re-run resets that field only once its goroutine is scheduled — a poll can
// read the PREVIOUS run's stop and report a re-run that never stopped as one
// that did.
func runOnce(t *testing.T, runner *statelog.Runner) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	err := runner.Run(ctx)
	if ctx.Err() != nil {
		t.Fatalf("the loop was still running after 3s (then %v) — it did not stop", err)
	}
	return err
}

// derefAll renders a list of expectations for a failure message.
func derefAll(expects []*uint64) []string {
	out := make([]string, len(expects))
	for i, e := range expects {
		if e == nil {
			out[i] = "none"
			continue
		}
		out[i] = fmt.Sprint(*e)
	}
	return out
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
	// THE ROWS ARE STILL THE LOST STREAM'S, whichever way the rebuild was met
	// and whichever instant the runner carries as its own — and that is the
	// instant a position published to the fleet has to name, or a peer
	// compares this node's sequences with the new stream's.
	if got := h.runner.KeyedTo(); statelog.IdentityOf(born, got, true) != statelog.StreamSame {
		t.Fatalf("before the reanchor the rows read as keyed to %s, want the lost "+
			"stream's %s", got, born)
	}

	// THE REANCHOR, through the real runner.
	var order []string
	in := reanchorInputs()
	in.StreamCreatedAt, in.KeyedTo, in.Generation, in.Position, in.Highest =
		rebuilt, born, old.Generation, old.Seq, old.Seq
	plan, err := statelog.Reanchor(t.Context(), statelog.ReanchorDeps{
		Domain: probeDomain{}, Stream: newReanchorLog(&order, rebuilt),
		Record: probeGeneration{keeps: true}, Consumer: &reanchorConsumer{},
		Runner: h.runner, DB: h.db.Replicated(), NodeID: "node-a",
	}, in, statelog.ReanchorGuard{Confirm: statelog.ConfirmationOf(rebuilt)})
	if err != nil {
		t.Fatalf("Reanchor: %v", err)
	}
	gen := plan.Generation
	if err := h.runner.StreamIdentity(); err != nil {
		t.Fatalf("the re-anchored runner still refuses its writes: %v", err)
	}
	if got := h.runner.Committed(); got.Generation != gen || got.Seq != 0 {
		t.Fatalf("the re-anchored runner stands at %s, want generation %d "+
			"sequence 0", got, gen)
	}
	if !h.runner.StreamCreatedAt().Equal(rebuilt) || !h.runner.KeyedTo().Equal(rebuilt) {
		t.Fatalf("the runner is keyed to %s (rows %s), want the adopted %s",
			h.runner.StreamCreatedAt(), h.runner.KeyedTo(), rebuilt)
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

// anonymousProbe is the probe domain claiming no identity, which is what the
// vectors are: their nodes re-anchor their own copies one at a time.
type anonymousProbe struct{ probeDomain }

func (anonymousProbe) ClaimsIdentity() bool { return false }

// A PEER THAT RE-ANCHORED THE LOG PAST THIS NODE'S GENERATION IS ESTABLISHED
// FROM THE FLEET'S NUMBER, and refuses the domain's reads and writes.
//
// A generation moves only by a reanchor, so a peer at a later one holds the
// rows the log now continues from — and a node left behind whose own readings
// see nothing wrong (below a restored broker's end, or past it once something
// wrote beyond its checkpoint) carried on serving and arbitrating from rows the
// fleet had left. Never for a domain claiming no identity, where a peer ahead
// is a per-node recovery in progress.
func TestAPeerReanchoringPastThisNodeIsEstablishedFromTheFleet(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	at := h.runner.Committed()
	if h.runner.ObserveFleetGeneration(at.Generation) {
		t.Fatal("the fleet at this node's own generation established a verdict")
	}
	if err := h.runner.StreamIdentity(); err != nil {
		t.Fatalf("the control refuses: %v", err)
	}
	if !h.runner.ObserveFleetGeneration(at.Generation + 1) {
		t.Fatal("a fleet one generation ahead established nothing")
	}
	if h.runner.ObserveFleetGeneration(at.Generation + 2) {
		t.Fatal("a second reading established the verdict a second time — the " +
			"engine's line about it would repeat every heartbeat")
	}
	err := h.runner.StreamIdentity()
	if !errors.Is(err, statelog.ErrGenerationPassed) {
		t.Fatalf("StreamIdentity = %v, want the passed generation", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("generation %d", at.Generation+2)) {
		t.Errorf("the refusal does not name the fleet's newest generation: %v", err)
	}
	// A RUN OVER ROWS THE FLEET HAS LEFT STOPS BEFORE IT FETCHES — which is
	// what a rejoin that found no donor starts again.
	if err := runOnce(t, h.runner); !errors.Is(err, statelog.ErrGenerationPassed) {
		t.Fatalf("a run after the verdict = %v, want a stop over the passed generation", err)
	}

	// AND A LOOP ALREADY RUNNING WHEN THE VERDICT LANDS APPLIES NOTHING MORE,
	// whatever generation the next record says: a node ahead of a restored
	// broker's end never sees the reanchor's own record, and a record need
	// not carry its writer's generation.
	m := newApplyHarness(t, probeDomain{})
	m.fetch.offer(1, env(1, "edit", "a", "op-a", 1))
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.runner.Run(ctx) }()
	for m.runner.Committed().Seq < 1 && ctx.Err() == nil {
		time.Sleep(2 * time.Millisecond)
	}
	if !m.runner.ObserveFleetGeneration(m.runner.Committed().Generation + 1) {
		t.Fatal("the fleet ahead of a running loop established nothing")
	}
	m.fetch.offer(2, env(2, "edit", "b", "op-b", 1))
	select {
	case err := <-done:
		if !errors.Is(err, statelog.ErrGenerationPassed) {
			t.Fatalf("the running loop ended with %v, want a stop over the passed "+
				"generation", err)
		}
	case <-ctx.Done():
		t.Fatal("the running loop never stopped after the verdict landed")
	}
	if got := m.runner.Committed(); got.Seq != 1 {
		t.Fatalf("the loop stands at %s, want sequence 1 — it applied a record into "+
			"rows the fleet has left", got)
	}

	anon := newApplyHarness(t, anonymousProbe{})
	if anon.runner.ObserveFleetGeneration(anon.runner.Committed().Generation + 1) {
		t.Fatal("a domain claiming no identity took a peer's generation as a " +
			"history it had lost — its nodes re-anchor their own copies")
	}
	if err := anon.runner.StreamIdentity(); err != nil {
		t.Fatalf("a domain claiming no identity refuses over a peer ahead: %v", err)
	}
}

// A RECORD FROM A GENERATION THIS NODE NEVER ENTERED STOPS THE APPLIER BEFORE
// IT IS APPLIED.
//
// Its writer stood in that generation, so a peer re-anchored the log and this
// node did not — and the reanchor's own record is the first thing the new
// generation puts on the log, so this reading sees the move the moment it
// reaches the node, where the positions register is a heartbeat away. Applied,
// the new generation's records would land on rows missing whatever the
// re-anchoring node held past the log. A domain claiming no identity applies it.
func TestARecordFromALaterGenerationStopsTheApplier(t *testing.T) {
	t.Parallel()
	later := func(seq uint64, id string) statelog.Envelope {
		e := env(seq, "edit", id, "op-"+id, 1)
		e.Gen = 2
		return e
	}
	h := newApplyHarness(t, probeDomain{})
	h.fetch.offer(1, env(1, "edit", "a", "op-a", 1))
	if err := h.run(1); err != nil {
		t.Fatalf("run: %v", err)
	}
	h.fetch.offer(2, later(2, "b"))
	err := h.run(2)
	if !errors.Is(err, statelog.ErrStopped) || !errors.Is(err, statelog.ErrGenerationPassed) {
		t.Fatalf("run = %v, want a stop over the passed generation", err)
	}
	if got := h.runner.Committed(); got.Seq != 1 {
		t.Fatalf("the applier stands at %s, want sequence 1 — the record from "+
			"generation 2 was applied into rows from generation 1", got)
	}
	if err := h.runner.StreamIdentity(); !errors.Is(err, statelog.ErrGenerationPassed) {
		t.Fatalf("StreamIdentity = %v, want the passed generation: the stop ends "+
			"the loop, and the reads and writes refuse on this", err)
	}
	// AND A RE-RUN STOPS AGAIN, which is what keeps a rejoin that found no
	// donor from resuming it into the old rows.
	if err := runOnce(t, h.runner); !errors.Is(err, statelog.ErrGenerationPassed) {
		t.Fatalf("a re-run = %v, want the same stop", err)
	}

	anon := newApplyHarness(t, anonymousProbe{})
	anon.fetch.offer(1, env(1, "edit", "a", "op-a", 1))
	anon.fetch.offer(2, later(2, "b"))
	if err := anon.run(2); err != nil {
		t.Fatalf("a domain claiming no identity refused a record from a later "+
			"generation: %v", err)
	}
}

// A JOIN RE-KEYS THE RUNNER TO THE FLEET'S HISTORY, AND ONLY TO IT.
//
// Both verdicts used to outlive the adoption that answers them — the runner
// stopped again on the checkpoint the join had just installed, so a node a peer
// re-anchored past had no way back but deleting its database. They are
// re-derived from the file as the join left it rather than cleared, because a
// join that replaced nothing must change nothing: rows still keyed to another
// stream keep the recreation, and rows still below the fleet's generation keep
// the passed one.
func TestAJoinReKeysTheRunnerOnlyToTheFleetsHistory(t *testing.T) {
	t.Parallel()
	born := time.Date(2026, 9, 10, 12, 0, 0, 123_456_789, time.UTC)
	rebuilt := born.Add(time.Hour)
	fresh := func(t *testing.T) *applyHarness {
		t.Helper()
		h := newApplyHarness(t, probeDomain{})
		h.rebuild(probeDomain{}, born)
		h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
		if err := h.run(1); err != nil {
			t.Fatalf("run: %v", err)
		}
		if !h.runner.ObserveStream(rebuilt) || !h.runner.ObserveFleetGeneration(2) {
			t.Fatal("the rebuild and the peer's generation established nothing")
		}
		return h
	}
	old := func(h *applyHarness) statelog.Position { return h.runner.Committed() }

	// NOTHING REPLACED: the rows are the lost stream's, in the old generation.
	h := fresh(t)
	if err := h.runner.Rejoined(old(h), born, rebuilt); err != nil {
		t.Fatalf("Rejoined: %v", err)
	}
	if err := h.runner.StreamIdentity(); !errors.Is(err, statelog.ErrStreamRecreated) {
		t.Fatalf("after a join that replaced nothing the identity is %v, want the "+
			"rebuild still", err)
	}

	// THE LIVE STREAM, STILL IN THE OLD GENERATION: the recreation clears and
	// the passed generation does not.
	h = fresh(t)
	if err := h.runner.Rejoined(old(h), rebuilt, rebuilt); err != nil {
		t.Fatalf("Rejoined: %v", err)
	}
	if err := h.runner.StreamIdentity(); !errors.Is(err, statelog.ErrGenerationPassed) {
		t.Fatalf("rows on the live stream below the fleet's generation read %v, "+
			"want the passed generation still", err)
	}

	// THE FLEET'S HISTORY: the live stream at the fleet's generation. Both
	// clear, the runner is keyed to the live stream, and its loop runs on.
	h = fresh(t)
	adopted := statelog.Position{Stream: probeStream, Generation: 2, Seq: 1}
	seedCursor(t, h.db, probeStream, adopted, rebuilt)
	if err := h.runner.Rejoined(adopted, rebuilt.Truncate(time.Microsecond), rebuilt); err != nil {
		t.Fatalf("Rejoined: %v", err)
	}
	if err := h.runner.StreamIdentity(); err != nil {
		t.Fatalf("a runner re-keyed to the fleet's history still refuses: %v", err)
	}
	if got := h.runner.StreamCreatedAt(); !got.Equal(rebuilt) {
		t.Fatalf("the runner is keyed to %s, want the live %s", got, rebuilt)
	}
	h.fetch.offer(2, func() statelog.Envelope {
		e := env(2, "edit", "b", "op-2", 1)
		e.Gen = 2
		return e
	}())
	if err := h.run(2); err != nil {
		t.Fatalf("the loop over the adopted checkpoint: %v", err)
	}
	if got := h.runner.Committed(); got != (statelog.Position{Stream: probeStream, Generation: 2, Seq: 2}) {
		t.Fatalf("the loop stands at %s, want generation 2 sequence 2", got)
	}
}

// A RE-RUN JUDGES ITS VERDICTS AGAINST THE CHECKPOINT IT LOADS.
//
// An adoption replaces every row, and the join then re-keys the runner to them
// — but the restore of an estate a failed join left closed judges the file
// against a live instant it may be unable to read, and the re-key does not
// happen. A loop that trusted the verdicts it held stopped again on every
// re-run, over rows that were the fleet's history, and nothing short of deleting
// the database released it. A row that still bears a verdict out keeps it.
func TestARerunJudgesItsVerdictsAgainstTheCheckpointItLoads(t *testing.T) {
	t.Parallel()
	born := time.Date(2026, 9, 10, 12, 0, 0, 123_456_789, time.UTC)
	rebuilt := born.Add(time.Hour)
	fresh := func(t *testing.T) *applyHarness {
		t.Helper()
		h := newApplyHarness(t, probeDomain{})
		h.rebuild(probeDomain{}, born)
		h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
		if err := h.run(1); err != nil {
			t.Fatalf("run: %v", err)
		}
		return h
	}
	own := statelog.Position{Stream: probeStream, Generation: 1, Seq: 1}
	adopted := statelog.Position{Stream: probeStream, Generation: 2, Seq: 1}

	// THE PASSED GENERATION: rows still below it keep it, and rows at it — a
	// file an adoption replaced — clear it.
	h := fresh(t)
	if !h.runner.ObserveFleetGeneration(2) {
		t.Fatal("the peer's generation established nothing")
	}
	if err := runOnce(t, h.runner); !errors.Is(err, statelog.ErrGenerationPassed) {
		t.Fatalf("a re-run over rows still in generation 1 = %v, want the passed "+
			"generation", err)
	}
	seedCursor(t, h.db, probeStream, adopted, born)
	h.fetch.offer(2, func() statelog.Envelope {
		e := env(2, "edit", "b", "op-2", 1)
		e.Gen = 2
		return e
	}())
	if err := rerun(t, h, 2); err != nil {
		t.Fatalf("a re-run over rows the fleet's generation replaced = %v — it "+
			"stopped on a verdict about rows the file no longer holds", err)
	}
	if err := h.runner.StreamIdentity(); err != nil {
		t.Fatalf("after the re-run the identity is %v, want none", err)
	}

	// THE RECREATION, established while the loop ran: rows still keyed to the
	// lost stream keep it, and rows keyed to the live one clear it and re-key
	// the runner to that stream.
	h = fresh(t)
	if !h.runner.ObserveStream(rebuilt) {
		t.Fatal("the rebuild established nothing")
	}
	if err := runOnce(t, h.runner); !errors.Is(err, statelog.ErrStreamRecreated) {
		t.Fatalf("a re-run over rows keyed to the lost stream = %v, want the "+
			"rebuild", err)
	}
	seedCursor(t, h.db, probeStream, own, rebuilt)
	h.fetch.offer(2, env(2, "edit", "b", "op-2", 1))
	if err := rerun(t, h, 2); err != nil {
		t.Fatalf("a re-run over rows keyed to the live stream = %v", err)
	}
	if got := h.runner.StreamCreatedAt(); !got.Equal(rebuilt) {
		t.Fatalf("the runner is keyed to %s after the re-run, want the live %s — "+
			"the next reading would find the stream recreated again", got, rebuilt)
	}
	if got := h.runner.KeyedTo(); !got.Equal(rebuilt) {
		t.Fatalf("the runner publishes its rows as keyed to %s, want %s", got, rebuilt)
	}
}

// A RECREATION VERDICT THE LIVE INSTANT CONTRADICTS IS NAMED, AND ONLY THEN.
//
// A runner that never read the live instant — its re-key after an adoption did
// not happen — judges rows keyed to that stream as another stream's, because
// the only instant it has is the one it was built with. The loop then stops as
// recreated and a reanchor refuses those rows, so the heartbeat has to be told
// the verdict is wrong for the join that re-keys it to be asked for.
func TestARecreationVerdictTheLiveInstantContradictsIsNamed(t *testing.T) {
	t.Parallel()
	born := time.Date(2026, 9, 10, 12, 0, 0, 123_456_789, time.UTC)
	live := born.Add(time.Hour)
	other := live.Add(time.Hour)
	h := newApplyHarness(t, probeDomain{})
	h.rebuild(probeDomain{}, born)
	if h.runner.RecreationStale(live) {
		t.Fatal("a runner holding no verdict reads as holding a stale one")
	}
	// THE DONOR'S FILE: rows keyed to the live stream, which this runner has
	// never read.
	at := statelog.Position{Stream: probeStream, Generation: 1, Seq: 4}
	seedCursor(t, h.db, probeStream, at, live)
	if err := runOnce(t, h.runner); !errors.Is(err, statelog.ErrStreamRecreated) {
		t.Fatalf("the loop over rows keyed to a stream it never read = %v, want "+
			"the recreation it judges them by", err)
	}
	switch {
	case !h.runner.RecreationStale(live):
		t.Fatal("the live instant the rows are keyed to does not contradict the verdict")
	case h.runner.RecreationStale(born):
		t.Fatal("the instant the runner was built with reads as contradicting it")
	case h.runner.RecreationStale(other):
		t.Fatal("a third stream reads as contradicting it — the rows are not its")
	}
	// AND THE JOIN'S RE-KEY, which is what the heartbeat asks for, clears it.
	if err := h.runner.Rejoined(at, live.Truncate(time.Microsecond), live); err != nil {
		t.Fatalf("Rejoined: %v", err)
	}
	if err := h.runner.StreamIdentity(); err != nil {
		t.Fatalf("after the re-key the identity is %v, want none", err)
	}
	if h.runner.RecreationStale(live) {
		t.Fatal("a cleared verdict still reads as stale")
	}
}

// rerun runs a loop that STOPPED before and waits for it to reach want, or
// returns what ended it.
//
// NOT [applyHarness.run], which polls [statelog.Runner.Stopped] from the moment
// it starts the loop: the previous run's stop is still recorded until the new
// run clears it, so that poll can read the old stop as this run's.
func rerun(t *testing.T, h *applyHarness, want uint64) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	errs := make(chan error, 1)
	go func() { errs <- h.runner.Run(ctx) }()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if h.runner.Committed().Seq >= want {
			cancel()
			<-errs
			return nil
		}
		select {
		case err := <-errs:
			return err
		case <-time.After(2 * time.Millisecond):
		}
	}
	cancel()
	<-errs
	return fmt.Errorf("the applier reached %s, want sequence %d",
		h.runner.Committed(), want)
}

// A GENERATION ANOTHER NODE OPENED IS NEVER OPENED AGAIN, FORCE OR NO FORCE.
//
// Two nodes that re-anchor independently each derive the same next generation
// from their own checkpoints — the register could not be read and both forced,
// or each read it before the other's row said anything. Exactly one generation
// record lands on the subject, and the other append loses. The transition used
// to read "the subject is taken" as its own earlier attempt and carry on, so
// both committed a checkpoint in the generation over their own rows: two
// histories under one number, which nothing on the log could reconcile. Now the
// record that landed is read back, and one this node did not write refuses —
// however the loss was reported: refused outright, unanswered and found by the
// probe, or acknowledged as a duplicate by a clustered broker whose window
// matched a message id before it checked the expectation.
func TestAGenerationAnotherNodeOpenedIsNeverOpenedAgain(t *testing.T) {
	t.Parallel()
	peer := func(t *testing.T, writer string) statelog.GenerationRecord {
		t.Helper()
		rec, _, err := probeGeneration{keeps: true}.GenerationRecord(statelog.GenerationFacts{
			Generation: 2, By: "ops-2", Writer: writer,
		})
		if err != nil {
			t.Fatalf("encode the peer's generation record: %v", err)
		}
		return rec
	}
	subject := func(rec statelog.GenerationRecord) string {
		return probeDomain{}.Stream().SubjectPrefix + "." + rec.Subject.String()
	}
	// THE MOST PERMISSIVE THE GUARD CAN BE: the register unreadable and the
	// operator forcing it, so nothing but the read-back stands in the way.
	forced := func() (statelog.ReanchorInputs, statelog.ReanchorGuard) {
		in := reanchorInputs()
		in.RegisterReadable = false
		guard := confirmed()
		guard.Force = true
		return in, guard
	}
	for _, c := range []struct {
		name  string
		stage func(t *testing.T, f *reanchorFixture)
	}{
		{"refused outright", func(t *testing.T, f *reanchorFixture) {
			rec := peer(t, "node-b")
			f.log.put(subject(rec), rec.OpID, rec.Payload)
		}},
		{"unanswered, and found by the probe", func(t *testing.T, f *reanchorFixture) {
			rec := peer(t, "node-b")
			f.log.put(subject(rec), rec.OpID, rec.Payload)
			f.log.lose = true
		}},
		{"acknowledged as a duplicate of the peer's", func(t *testing.T, f *reanchorFixture) {
			// A MESSAGE ID BOTH NODES WOULD HAVE USED, as the domains' ids
			// once were: the clustered broker matches it in its window
			// and never reaches the expectation.
			rec := peer(t, "node-b")
			own, _, err := probeGeneration{keeps: true}.GenerationRecord(statelog.GenerationFacts{
				Generation: 2, Writer: "node-a",
			})
			if err != nil {
				t.Fatalf("encode this node's record: %v", err)
			}
			f.log.put(subject(rec), own.OpID, rec.Payload)
			f.log.dedupeFirst = true
		}},
		{"written by a node that did not say who it was", func(t *testing.T, f *reanchorFixture) {
			rec := peer(t, "")
			f.log.put(subject(rec), rec.OpID, rec.Payload)
		}},
		{"a record whose envelope does not decode", func(t *testing.T, f *reanchorFixture) {
			rec := peer(t, "node-b")
			f.log.put(subject(rec), rec.OpID, []byte("not an envelope"))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newReanchorFixture(t)
			before := statelog.Position{Stream: probeStream, Generation: 1, Seq: 9_000}
			seedCursor(t, f.db, probeStream, before, keyedCreated)
			c.stage(t, f)
			in, guard := forced()
			_, err := statelog.Reanchor(t.Context(), f.deps(probeDomain{}), in, guard)
			if !errors.Is(err, statelog.ErrReanchorRefused) {
				t.Fatalf("a reanchor onto a generation another node opened = %v, "+
					"want a refusal", err)
			}
			if strings.Contains(c.name, "refused outright") && !strings.Contains(err.Error(), "node-b") {
				t.Errorf("the refusal does not name the node that opened it: %v", err)
			}
			if at, _ := cursorOf(t, f.db, probeStream); at != before {
				t.Fatalf("the checkpoint moved to %s over another node's generation", at)
			}
			if len(f.consumer.after) != 0 || len(f.runner.at) != 0 {
				t.Fatalf("the consumer was moved to %v and the runner re-keyed to %v "+
					"over another node's generation", f.consumer.after, f.runner.at)
			}
		})
	}

	// AND THIS NODE'S OWN EARLIER RECORD IS STILL CARRIED ON FROM, whichever
	// way the broker reports it: the re-run of an interrupted transition.
	for _, dedupe := range []bool{false, true} {
		f := newReanchorFixture(t)
		seedCursor(t, f.db, probeStream,
			statelog.Position{Stream: probeStream, Generation: 1, Seq: 9_000}, keyedCreated)
		own := peer(t, "node-a")
		f.log.put(subject(own), own.OpID, own.Payload)
		f.log.dedupeFirst = dedupe
		in, guard := forced()
		plan, err := statelog.Reanchor(t.Context(), f.deps(probeDomain{}), in, guard)
		if err != nil {
			t.Fatalf("a re-run finding its own record (duplicate window first: %v) "+
				"= %v", dedupe, err)
		}
		if at, _ := cursorOf(t, f.db, probeStream); at.Generation != plan.Generation {
			t.Fatalf("the re-run left the checkpoint at %s", at)
		}
	}
}

// AN ABANDONED GENERATION'S RECORDS ARE VOID WHEREVER ITS REANCHOR IS FOLLOWED.
//
// A node that re-anchored and was evicted before anybody adopted from it left
// records on the log in a generation whose history is on no disk the fleet
// still has. The reanchor that skips past it follows the log from the rows'
// own checkpoint, so those records are ahead of it — and applied into these
// rows they would mix two histories. The transition says which generations it
// abandoned on the checkpoint, and the applier drops their records: consumed,
// the anchor moved to them (they are the subject's last messages on the
// broker), and applied into no row. Records in every other generation apply.
func TestAnAbandonedGenerationsRecordsAreVoidWhereItsReanchorIsFollowed(t *testing.T) {
	t.Parallel()

	// THE TRANSITION WRITES THE RANGE: rows at generation 1, a log that
	// holds everything past their checkpoint, and an evicted node that
	// opened generation 2.
	f := newReanchorFixture(t)
	seedCursor(t, f.db, probeStream,
		statelog.Position{Stream: probeStream, Generation: 1, Seq: 10}, reanchorCreated)
	in := reanchorInputs()
	in.KeyedTo = reanchorCreated.Truncate(time.Microsecond)
	in.Position, in.FirstSeq, in.LastSeq, in.Highest = 10, 1, 20, 10
	in.Abandoned = 2
	plan, err := statelog.Reanchor(t.Context(), f.deps(probeDomain{}), in, confirmed())
	if err != nil {
		t.Fatalf("Reanchor: %v", err)
	}
	if plan.Case != statelog.ReanchorAbandoned || plan.Generation != 3 ||
		plan.Cursor != 10 || plan.From != 1 {
		t.Fatalf("the plan is %+v, want the abandoned case into generation 3 at the "+
			"rows' own checkpoint, 10, abandoning what lies between 1 and 3", plan)
	}
	var after, before int64
	if err := f.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `
			SELECT void_after, void_before FROM statelog_cursor WHERE stream = ?`,
			probeStream).Scan(&after, &before)
	}); err != nil {
		t.Fatalf("read the checkpoint: %v", err)
	}
	if after != 1 || before != 3 {
		t.Fatalf("the checkpoint abandons (%d, %d), want (1, 3)", after, before)
	}

	// THE APPLIER DROPS THE ABANDONED GENERATION'S RECORDS, over a checkpoint
	// carrying that range — and only those.
	h := newApplyHarness(t, probeDomain{})
	if err := h.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO statelog_cursor
				(stream, generation, seq, stream_created_at, updated_at,
				 void_after, void_before)
			VALUES (?, 3, 0, ?, ?, 1, 3)`,
			probeStream, store.EncodeTime(reanchorCreated), store.EncodeTime(reanchorCreated))
		return err
	}); err != nil {
		t.Fatalf("seed the checkpoint: %v", err)
	}
	stamped := func(seq uint64, id string, gen uint32) statelog.Envelope {
		e := env(seq, "edit", id, "op-"+id, 1)
		e.Gen = gen
		return e
	}
	h.fetch.offer(1, stamped(1, "abandoned", 2))
	h.fetch.offer(2, stamped(2, "older", 1))
	h.fetch.offer(3, stamped(3, "current", 3))
	if err := h.run(3); err != nil {
		t.Fatalf("run: %v", err)
	}
	h.applier.mu.Lock()
	applied := slices.Clone(h.applier.applied)
	h.applier.mu.Unlock()
	want := []statelog.Position{
		{Stream: probeStream, Generation: 3, Seq: 2},
		{Stream: probeStream, Generation: 3, Seq: 3},
	}
	if !slices.Equal(applied, want) {
		t.Fatalf("applied %v, want %v — a record written in the abandoned "+
			"generation was applied into these rows, or a record in another was not",
			applied, want)
	}
	var anchor int64
	if err := h.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT anchor FROM statelog_anchor WHERE subject = ?`,
			probePrefix+".object.abandoned").Scan(&anchor)
	}); err != nil {
		t.Fatalf("read the void record's anchor: %v", err)
	}
	if want := (statelog.Position{Stream: probeStream, Generation: 3, Seq: 1}); anchor != want.Packed() {
		t.Fatalf("the void record's subject is anchored at packed %d, want %d — it "+
			"is still that subject's last message on the broker, and a writer "+
			"expecting anything else is refused for ever", anchor, want.Packed())
	}
}

// A NODE'S STANDING ON A LOG IS READ OFF THE LOG, whether or not its applier
// has reached it.
//
// A node a peer re-anchored past stops before the first record of the new
// generation, so the eviction of that peer never reaches its rows — and that
// eviction is exactly what it must see for the peer's generation to stop
// being the fleet's. The last record on the node's eviction subject is its
// standing: an eviction, or the readmission that inverted one.
func TestANodesStandingIsReadOffTheLog(t *testing.T) {
	t.Parallel()
	log := newReanchorLog(nil)
	subject := probePrefix + ".eviction.node-x"
	put := func(evicts bool) {
		log.put(subject, fmt.Sprintf("op-%d", log.records()),
			[]byte(fmt.Sprintf(`{"evicts":%t}`, evicts)))
	}
	ask := func() (bool, bool) {
		t.Helper()
		evicted, found, err := statelog.EvictedOnLog(t.Context(), evictingProbe{}, log, "node-x")
		if err != nil {
			t.Fatalf("EvictedOnLog: %v", err)
		}
		return evicted, found
	}
	if _, found := ask(); found {
		t.Fatal("a node never gated reads as having a standing")
	}
	put(true)
	if evicted, found := ask(); !found || !evicted {
		t.Fatalf("after its eviction the node reads evicted=%v found=%v", evicted, found)
	}
	put(false)
	if evicted, found := ask(); !found || evicted {
		t.Fatalf("after its readmission the node reads evicted=%v found=%v — the "+
			"LAST record is its standing", evicted, found)
	}
	// A DOMAIN WITH NO GATE has nothing to read.
	if _, found, err := statelog.EvictedOnLog(t.Context(), probeDomain{}, log, "node-x"); err != nil || found {
		t.Fatalf("a domain with no eviction gate answered found=%v, %v", found, err)
	}
}

// evictingProbe is the probe domain with an eviction gate on its log.
type evictingProbe struct{ probeDomain }

func (evictingProbe) EvictionSubject(node string) statelog.Subject {
	return statelog.Subject{Kind: "eviction", ID: node}
}

func (evictingProbe) Evicts(payload []byte) (bool, error) {
	var body struct {
		Evicts bool `json:"evicts"`
	}
	err := json.Unmarshal(payload, &body)
	return body.Evicts, err
}

// WHO OPENED EACH GENERATION IS READ OFF THE LOG, past what the register knows.
//
// A reanchor appends its record before it commits and before it publishes its
// position, so a node that died in between opened a generation no row names —
// and a later reanchor deriving that number from the register alone lost to
// its record on every attempt.
func TestWhoOpenedEachGenerationIsReadOffTheLog(t *testing.T) {
	t.Parallel()
	log := newReanchorLog(nil)
	open := func(gen uint32, writer string) {
		rec, _, err := probeGeneration{keeps: true}.GenerationRecord(statelog.GenerationFacts{
			Generation: gen, Writer: writer,
		})
		if err != nil {
			t.Fatalf("encode generation %d: %v", gen, err)
		}
		log.put(probePrefix+"."+rec.Subject.String(), rec.OpID, rec.Payload)
	}
	open(2, "node-b")
	open(3, "node-c")
	open(5, "node-e")
	read := func(above, through uint32) map[uint32]string {
		t.Helper()
		got, err := statelog.GenerationOpeners(t.Context(), probeDomain{},
			probeGeneration{keeps: true}, log, above, through)
		if err != nil {
			t.Fatalf("GenerationOpeners: %v", err)
		}
		return got
	}
	if got, want := read(1, 1), map[uint32]string{2: "node-b", 3: "node-c"}; !maps.Equal(got, want) {
		t.Fatalf("above 1 = %v, want %v — every generation opened on the log past "+
			"what the register named, up to the first that holds nothing", got, want)
	}
	if got, want := read(1, 5), map[uint32]string{2: "node-b", 3: "node-c", 5: "node-e"}; !maps.Equal(got, want) {
		t.Fatalf("through 5 = %v, want %v — a generation the register names is read "+
			"across a gap below it", got, want)
	}
	if got := read(5, 5); len(got) != 0 {
		t.Fatalf("above 5 = %v, want nothing", got)
	}
	if got, err := statelog.GenerationOpeners(t.Context(), probeDomain{},
		probeGeneration{keeps: false}, log, 1, 5); err != nil || len(got) != 0 {
		t.Fatalf("a domain keeping no generation record = %v, %v, want nothing", got, err)
	}
}
