package engine

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A REANCHOR'S INPUTS NAME THE STREAM THEY WERE READ FROM, and its instant is
// the LIVE one.
//
// The creation instant, the first sequence, this node's position and the
// fleet's high-water mark are each a fact about ONE log, the one the operator
// named. Every domain, because the operator may name any of them.
func TestAReanchorsInputsNameTheStreamTheyWereReadFrom(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	s := e.native.log
	if len(s.order) == 0 {
		t.Fatal("the node runs no domain, so nothing below is checked")
	}
	for _, name := range s.order {
		running := s.domains[name]
		want := running.domain.Stream().Name
		in, err := e.reanchorInputs(t.Context(), running)
		if err != nil {
			t.Fatalf("read %s's inputs: %v", name, err)
		}
		if in.Stream != want {
			t.Errorf("a reanchor of %s reads its inputs naming stream %q, want %q",
				name, in.Stream, want)
		}
		stats, err := running.log.Stats(t.Context())
		if err != nil {
			t.Fatalf("read %s's stream: %v", name, err)
		}
		if !in.StreamCreatedAt.Equal(stats.CreatedAt.UTC()) {
			t.Errorf("%s's inputs carry %s and the broker reports %s", name,
				in.StreamCreatedAt, stats.CreatedAt)
		}
		if in.ClaimsIdentity != running.domain.ClaimsIdentity() {
			t.Errorf("%s's inputs say ClaimsIdentity=%v, and the domain says %v",
				name, in.ClaimsIdentity, running.domain.ClaimsIdentity())
		}
	}
}

// THE FLEET'S HIGH-WATER MARK IS READ IN THIS DOMAIN'S OWN GENERATION.
//
// It is what "only the most caught-up node may reanchor" compares this node's
// position against, and a sequence at another generation is a number in
// another space: a peer that has already re-anchored reports the adopted
// stream's sequences, a peer behind a reanchor a dead stream's. Compared as
// though they were this generation's, the first made the most caught-up node
// look behind and the second hid a node that was genuinely further along.
func TestAReanchorsHighWaterMarkIsReadInItsOwnGeneration(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	running := e.native.log.Domain(tracker.Domain{}.Name())
	own := running.runner.Committed()
	name := running.domain.Name()
	for _, row := range []struct {
		node string
		at   coord.DomainPosition
	}{
		// ANOTHER GENERATION, far along its own stream, and hydrated on
		// nothing — so only the high-water mark could count it.
		{"elsewhere", coord.DomainPosition{Generation: own.Generation + 1, Seq: 99_999}},
		// THIS GENERATION, further along than this node.
		{"ahead", coord.DomainPosition{Generation: own.Generation, Seq: own.Seq + 50}},
	} {
		if err := e.backends.Fleet.PutPositions(t.Context(), coord.NodePositions{
			NodeID: row.node, At: time.Now().UTC(),
			Domains: map[string]coord.DomainPosition{name: row.at},
		}); err != nil {
			t.Fatalf("publish %s's row: %v", row.node, err)
		}
	}
	in, err := e.reanchorInputs(t.Context(), running)
	if err != nil {
		t.Fatalf("reanchorInputs: %v", err)
	}
	if in.Highest != own.Seq+50 {
		t.Fatalf("the high-water mark is %d, want %d — the peer at generation %d "+
			"reports a sequence in another number space", in.Highest, own.Seq+50,
			own.Generation+1)
	}
}

// A STREAM THAT CANNOT BE READ REFUSES THE REANCHOR, rather than keying the
// checkpoint to an instant nobody read at a first sequence of zero.
func TestAReanchorOfAStreamThatCannotBeReadRefuses(t *testing.T) {
	t.Parallel()
	e, js := aRunningNode(t)
	stream := search.Domain{}.Stream().Name
	if err := js.DeleteStream(t.Context(), stream); err != nil {
		t.Fatalf("delete the stream: %v", err)
	}
	_, err := e.Reanchor(t.Context(), ReanchorRequest{
		Stream: stream, Confirm: statelog.ConfirmationOf(time.Now()), By: "ops-1",
	})
	if !errors.Is(err, statelog.ErrReanchorRefused) ||
		!strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("Reanchor over a stream that cannot be read = %v, want a refusal "+
			"saying so", err)
	}
	if _, _, err := e.ReanchorStatus(t.Context(), stream); err == nil {
		t.Fatal("ReanchorStatus answered an instant for a stream it could not read")
	}
}

// A LOG RECREATED BETWEEN BOOTS IS RE-ANCHORED WITHOUT A RESTART, and the
// other domains never notice.
//
// Every half of the verb had broken this: it moved every domain's checkpoint
// from the one stream named, so the pages and vector logs came out keyed to the
// tracker's instant and stopped at the next boot; its version reset failed on
// its first statement; its record went through a publisher whose fence refuses
// exactly while a stream is recreated; and nothing cleared the verdict or
// started the loop again, so even a reanchor that worked needed a restart.
func TestALogRecreatedBetweenBootsIsReanchoredWithoutARestart(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	trackerStream := tracker.Domain{}.Stream().Name

	// FIRST BOOT: history on the tracker's log.
	e, back := bootNode(t, &b, cfg)
	waitUntil(t, 20*time.Second, "the node to admit seats", e.NativeHydrated)
	if res, err := e.native.writer.EvictNode(t.Context(), "op-before", "node-x"); err != nil ||
		res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("a write before the rebuild: %+v, %v", res, err)
	}
	// EVERY OTHER DOMAIN'S CHECKPOINT, as its applier left it.
	untouched := map[string]cursorRow{}
	for _, name := range e.native.log.order {
		if name == (tracker.Domain{}).Name() {
			continue
		}
		untouched[name] = readCursorRow(t, e, name)
	}
	js := jetStreamOn(t, back)
	e.Stop(context.Background())
	if err := js.DeleteStream(t.Context(), trackerStream); err != nil {
		t.Fatalf("delete the tracker's log: %v", err)
	}
	back.Close(context.Background())
	time.Sleep(2 * time.Millisecond)

	// SECOND BOOT: the tracker stops on the recreated log.
	e2, back2 := bootNode(t, &b, cfg)
	running := e2.native.log.Domain(tracker.Domain{}.Name())
	waitUntil(t, 10*time.Second, "the tracker to stop on its recreated log", func() bool {
		return errors.Is(running.runner.Stopped(), statelog.ErrStreamRecreated)
	})

	// THE STATUS NAMES THE LIVE INSTANT, which is what the operator confirms.
	stats, err := running.log.Stats(t.Context())
	if err != nil {
		t.Fatalf("read the recreated log: %v", err)
	}
	live, generation, err := e2.ReanchorStatus(t.Context(), trackerStream)
	if err != nil {
		t.Fatalf("ReanchorStatus: %v", err)
	}
	if !live.Equal(stats.CreatedAt.UTC()) || generation != 0 {
		t.Fatalf("ReanchorStatus = %s at generation %d, want the live %s at 0",
			live, generation, stats.CreatedAt)
	}

	gen, err := e2.Reanchor(t.Context(), ReanchorRequest{
		Stream: trackerStream, Confirm: statelog.ConfirmationOf(live), By: "ops-1",
	})
	if err != nil {
		t.Fatalf("Reanchor: %v", err)
	}
	if gen != 1 {
		t.Fatalf("reanchored to generation %d, want 1", gen)
	}

	// WITHOUT A RESTART: the same runner resumes, the node admits seats, and
	// a write lands on the adopted log.
	waitUntil(t, 10*time.Second, "the tracker's applier to resume", func() bool {
		return running.runner.Stopped() == nil && running.runner.StreamIdentity() == nil &&
			running.runner.Committed().Generation == gen
	})
	waitUntil(t, 20*time.Second, "the node to admit seats again", e2.NativeHydrated)
	res, err := e2.native.writer.EvictNode(t.Context(), "op-after", "node-y")
	if err != nil || res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("a write after the reanchor: %+v, %v — the domain was re-anchored "+
			"and does not serve", res, err)
	}
	if res.Position.Generation != gen {
		t.Fatalf("the write landed at %s, want generation %d", res.Position, gen)
	}
	// AND THE RECORD OF IT, applied from the adopted log by the resumed loop.
	waitUntil(t, 10*time.Second, "the generation record to apply", func() bool {
		return countRows(t, e2, `SELECT COUNT(*) FROM tracker_log_generations
			WHERE generation = 1 AND by = 'ops-1'`) == 1
	})

	// NO OTHER DOMAIN MOVED.
	for name, before := range untouched {
		if got := readCursorRow(t, e2, name); got != before {
			t.Errorf("%s's checkpoint moved from %+v to %+v during a reanchor of "+
				"the tracker's log", name, before, got)
		}
		other := e2.native.log.Domain(name).runner
		if err := other.StreamIdentity(); err != nil {
			t.Errorf("%s refuses after a reanchor of another log: %v", name, err)
		}
		if err := other.Stopped(); err != nil {
			t.Errorf("%s's applier stopped after a reanchor of another log: %v", name, err)
		}
	}
	e2.Stop(context.Background())
	back2.Close(context.Background())

	// THIRD BOOT: every applier comes up on its own stream.
	e3, _ := bootNode(t, &b, cfg)
	waitUntil(t, 20*time.Second, "the node to admit seats after a restart", e3.NativeHydrated)
	for _, name := range e3.native.log.order {
		runner := e3.native.log.Domain(name).runner
		waitUntil(t, 10*time.Second, name+"'s applier to load its checkpoint", func() bool {
			return runner.Stopped() != nil || runner.Committed().Seq > 0 ||
				name == (search.Domain{}).Name()
		})
		if err := runner.Stopped(); err != nil {
			t.Errorf("%s's applier stopped after the restart: %v", name, err)
		}
		if err := runner.StreamIdentity(); err != nil {
			t.Errorf("%s refuses after the restart: %v", name, err)
		}
	}
	if got := e3.native.log.Domain(tracker.Domain{}.Name()).runner.Committed().Generation; got != gen {
		t.Fatalf("the tracker came back at generation %d, want %d", got, gen)
	}
}

// A LOG REBUILT UNDER A RUNNING NODE IS RE-ANCHORED WITH THE INSTANT ITS
// REFUSAL NAMES.
//
// The refusal every read and write gives names the LIVE instant; the verb used
// to demand the instant sampled at boot, so following the refusal's own advice
// was refused — and confirming the boot instant keyed the checkpoint to a
// stream that no longer existed, which the next boot found recreated again.
func TestALogRebuiltUnderARunningNodeIsReanchoredWithTheInstantItsRefusalNames(t *testing.T) {
	t.Parallel()
	e, js := aRunningNode(t)
	s := e.native.log
	running := s.Domain(tracker.Domain{}.Name())
	if res, err := e.native.writer.EvictNode(t.Context(), "op-before", "node-x"); err != nil ||
		res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("a write before the rebuild: %+v, %v", res, err)
	}

	rebuildLog(t, js, tracker.Domain{}.Stream())
	s.publishPositions(t.Context())
	_, err := e.native.writer.EvictNode(t.Context(), "op-refused", "node-y")
	requireRebuiltLogRefusal(t, err)
	named := regexp.MustCompile(`was created at (\S+), so`).FindStringSubmatch(err.Error())
	if named == nil {
		t.Fatalf("the refusal names no live instant: %v", err)
	}
	live, _, err := e.ReanchorStatus(t.Context(), tracker.Domain{}.Stream().Name)
	if err != nil {
		t.Fatalf("ReanchorStatus: %v", err)
	}
	if statelog.ConfirmationOf(live) != named[1] {
		t.Fatalf("ReanchorStatus names %s and the refusal %s — the operator would "+
			"be asked to confirm a stream the refusal does not name",
			statelog.ConfirmationOf(live), named[1])
	}

	gen, err := e.Reanchor(t.Context(), ReanchorRequest{
		Stream: tracker.Domain{}.Stream().Name, Confirm: named[1], By: "ops-1",
	})
	if err != nil {
		t.Fatalf("Reanchor confirming the instant the refusal named: %v", err)
	}
	waitUntil(t, 10*time.Second, "the tracker to resume", func() bool {
		return running.runner.StreamIdentity() == nil &&
			running.runner.Committed().Generation == gen
	})
	res, err := e.native.writer.EvictNode(t.Context(), "op-after", "node-y")
	if err != nil || res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("a write after the reanchor: %+v, %v", res, err)
	}
	if got := running.runner.StreamCreatedAt(); statelog.IdentityOf(got, live, true) != statelog.StreamSame {
		t.Fatalf("the runner is keyed to %s, want the rebuilt stream's %s", got, live)
	}
	// AND THE CHECKPOINT IS KEYED TO IT, which is what the next boot compares.
	row := readCursorRow(t, e, tracker.Domain{}.Name())
	if statelog.IdentityOf(row.created, live, true) != statelog.StreamSame {
		t.Fatalf("the checkpoint is keyed to %s, want the rebuilt stream's %s — the "+
			"next boot would find it recreated again", row.created, live)
	}
}

// A REANCHOR OF THE KNOWLEDGE BASE'S LOG IS THE KNOWLEDGE BASE'S.
//
// It published the TRACKER's generation record on the tracker's log and wrote
// the tracker's audit row, whatever stream was named. Now the pages log gets
// the pages record and the pages audit row, the tracker's checkpoint, audit and
// rows are untouched, and a page written before the reanchor is written again
// after it — at an expectation of zero, from an anchor in the generation the
// log left.
func TestAReanchorOfThePagesLogIsThePagesOwn(t *testing.T) {
	t.Parallel()
	e, js := aRunningNode(t)
	s := e.native.log
	store := e.native.pages
	if store == nil {
		t.Fatal("the node runs no knowledge base")
	}
	if _, _, err := store.EnsureContainer(t.Context(), "ENG", "Engineering", "before"); err != nil {
		t.Fatalf("a page write before the rebuild: %v", err)
	}
	if res, err := e.native.writer.EvictNode(t.Context(), "op-tracker", "node-x"); err != nil ||
		res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("a tracker write: %+v, %v", res, err)
	}
	trackerBefore := readCursorRow(t, e, tracker.Domain{}.Name())

	rebuildLog(t, js, pages.Domain{}.Stream())
	s.publishPositions(t.Context())
	stream := pages.Domain{}.Stream().Name
	live, _, err := e.ReanchorStatus(t.Context(), stream)
	if err != nil {
		t.Fatalf("ReanchorStatus: %v", err)
	}
	gen, err := e.Reanchor(t.Context(), ReanchorRequest{
		Stream: stream, Confirm: statelog.ConfirmationOf(live), By: "ops-1",
	})
	if err != nil {
		t.Fatalf("Reanchor: %v", err)
	}

	// THE PAGES LOG CARRIES THE PAGES RECORD, applied as the pages audit row.
	waitUntil(t, 10*time.Second, "the pages generation record to apply", func() bool {
		return countRows(t, e, `SELECT COUNT(*) FROM pages_log_generations
			WHERE generation = 1 AND by = 'operator:ops-1'`) == 1
	})
	// AND THE TRACKER NEVER HEARD OF IT.
	if n := countRows(t, e, `SELECT COUNT(*) FROM tracker_log_generations`); n != 0 {
		t.Fatalf("a reanchor of the pages log wrote %d tracker audit row(s)", n)
	}
	if got := readCursorRow(t, e, tracker.Domain{}.Name()); got != trackerBefore {
		t.Fatalf("the tracker's checkpoint moved from %+v to %+v", trackerBefore, got)
	}
	trackerRunner := s.Domain(tracker.Domain{}.Name()).runner
	if err := trackerRunner.StreamIdentity(); err != nil {
		t.Fatalf("the tracker refuses after a reanchor of the pages log: %v", err)
	}
	if res, err := e.native.writer.EvictNode(t.Context(), "op-tracker-after", "node-y"); err != nil ||
		res.Outcome != statelog.OutcomeApplied || res.Position.Generation != 0 {
		t.Fatalf("a tracker write after a pages reanchor: %+v, %v — want applied "+
			"in the tracker's own generation 0", res, err)
	}

	// A PAGE ROW FROM BEFORE IS WRITTEN AGAIN, in the new generation.
	waitUntil(t, 10*time.Second, "the pages applier to resume", func() bool {
		r := s.Domain(pages.Domain{}.Name()).runner
		return r.StreamIdentity() == nil && r.Committed().Generation == gen
	})
	if _, changed, err := store.EnsureContainer(t.Context(), "ENG", "Engineering", "after"); err != nil || !changed {
		t.Fatalf("a page write after the reanchor: changed %v, %v", changed, err)
	}
	waitUntil(t, 10*time.Second, "the page write to apply in the new generation", func() bool {
		return countRows(t, e, `SELECT COUNT(*) FROM pages_containers
			WHERE key = 'ENG' AND purpose = 'after' AND version >= ?`,
			int64(gen)*statelog.GenerationStride) == 1
	})
}

// ONE DOMAIN'S LOOP HALTS AND RESUMES WITHOUT THE OTHERS.
//
// A reanchor moves one domain's checkpoint, whose only other writer is that
// domain's loop — so that loop, and only that one, is ended for the length of
// the transition: a batch it committed after the transition's transaction
// would write the old generation back. Ending every domain's loop, which is
// all the set's own halt could do, would pause the whole node for one log.
func TestHaltingOneApplierLeavesTheOthersRunning(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	s := e.native.log
	halted := s.Domain(tracker.Domain{}.Name())
	other := s.Domain(pages.Domain{}.Name())

	if !s.haltApplier(halted.domain.Name()) {
		t.Fatal("halting a running loop reported that none was running")
	}
	if s.haltApplier(halted.domain.Name()) {
		t.Fatal("halting it again reported a loop still running")
	}
	stalled := barrierOn(t, halted)
	moved := barrierOn(t, other)
	waitUntil(t, 10*time.Second, "the other domain to apply", func() bool {
		return other.runner.Committed().Seq >= moved
	})
	// A LOOP THAT IS RUNNING APPLIES A BARRIER IN MILLISECONDS, so a second
	// with nothing applied is a loop that is not running.
	time.Sleep(time.Second)
	if got := halted.runner.Committed().Seq; got >= stalled {
		t.Fatalf("the halted domain applied through %d, past the %d appended "+
			"while it was halted — its loop never stopped", got, stalled)
	}

	s.resumeApplier(t.Context(), halted.domain.Name())
	waitUntil(t, 10*time.Second, "the halted domain to resume", func() bool {
		return halted.runner.Committed().Seq >= stalled
	})
}

// A REFUSED REANCHOR LEAVES THE DOMAIN SERVING, as promptly as before it.
//
// The transition halts the domain's loop before it reads anything, so a
// refusal — a mistyped confirmation is the common one — has to put the loop
// back: not merely start it, but start it on a consumer with no pull left
// pending from the fetch the halt ended, or the next record goes to a reader
// that is gone and the domain waits out the acknowledgement window for it.
func TestARefusedReanchorLeavesTheDomainServing(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	_, err := e.Reanchor(t.Context(), ReanchorRequest{
		Stream: tracker.Domain{}.Stream().Name, Confirm: "2020-01-01T00:00:00Z",
		By: "ops-1",
	})
	if !errors.Is(err, statelog.ErrReanchorRefused) {
		t.Fatalf("a reanchor confirming the wrong instant = %v, want a refusal", err)
	}
	res, err := e.native.writer.EvictNode(t.Context(), "op-after-refusal", "node-x")
	if err != nil || res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("a write after a refused reanchor: %+v, %v — the domain's loop "+
			"was not put back as it was", res, err)
	}
}

// ---- helpers -------------------------------------------------------- //

// cursorRow is one domain's checkpoint row, whole.
type cursorRow struct {
	at      statelog.Position
	created time.Time
}

func readCursorRow(t *testing.T, e *Engine, domain string) cursorRow {
	t.Helper()
	stream := e.native.log.Domain(domain).domain.Stream().Name
	at, created, _, err := statelog.CursorFor(t.Context(), e.backends.Store.Replicated(), stream)
	if err != nil {
		t.Fatalf("read %s's checkpoint: %v", domain, err)
	}
	return cursorRow{at: at, created: created}
}

func countRows(t *testing.T, e *Engine, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.backends.Store.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), query, args...).Scan(&n)
	}); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// bootNode starts one engine over a bootstrap, closing it with the test.
func bootNode(t *testing.T, b *config.Bootstrap, cfg *config.Company) (*Engine, *Backends) {
	t.Helper()
	back, err := OpenBackends(t.Context(), b, cfg)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	e, err := New(t.Context(), Options{Bootstrap: b, Company: cfg, Backends: back})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	return e, back
}

// jetStreamOn is a JetStream handle on a node's own embedded broker.
func jetStreamOn(t *testing.T, back *Backends) natsjs.JetStream {
	t.Helper()
	q, ok := back.Queue.(interface{ Conn() *nats.Conn })
	if !ok {
		t.Fatalf("the stream is %T, not the JetStream backend", back.Queue)
	}
	js, err := natsjs.New(q.Conn())
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return js
}
