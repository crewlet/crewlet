package tracker_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A RETRIED CREATE ON A NODE THAT IS BEHIND NEVER TAKES ANOTHER TASK'S KEY.
//
// Two nodes over one log. Node a files ENG-1 and ENG-2, and then an operation
// whose counter step lands — it takes ENG-3 — and whose task step is refused.
// Node b has applied ENG-1 and nothing after it when the caller retries that
// operation there: b's mint decides ENG-2 from its stale counter, the broker
// refuses it against the earlier copy, and the refusal's answer is lost. The
// probe finds the earlier copy, b catches up and its ledger records the
// operation — and the publisher answered that as the application of b's own
// decision, so the retry filed its task as ENG-2, beside the task that is.
func TestARetriedCreateOnANodeBehindTakesAKeyOfItsOwn(t *testing.T) {
	t.Parallel()
	a := newRoundTrip(t)
	a.applyWhileWriting()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node-b.db"),
		store.Options{PinnedWriters: 1})
	if err != nil {
		t.Fatalf("open node b's store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	b := newRoundTripOn(t, a.broker, a.log, db, "node-b")

	create := func(op, id string) {
		t.Helper()
		if _, err := a.writer.CreateTask(t.Context(), op, newTask(id), nil); err != nil {
			t.Fatalf("file %s on node a: %v", id, err)
		}
	}
	create("op-t0", "t0")
	b.drain()
	create("op-t1", "t1")

	// THE EARLIER COPY, on node a: its counter step lands and its task step
	// is refused, as a full stream refuses it.
	lossyA, logA := a.lossyWriter(t)
	op := statelog.NewOpID(time.Now(), "create")
	logA.refuse("retried")
	if _, err := lossyA.CreateTask(t.Context(), op, newTask("retried"), nil); err == nil {
		t.Fatal("the premise: the earlier copy's task step should have been refused")
	}
	logA.refuse("")

	// THE RETRY, on node b.
	b.applyWhileWriting()
	lossyB, logB := b.lossyWriter(t)
	logB.dropAck(".counter.ENG")
	got, err := lossyB.CreateTask(t.Context(), op, newTask("retried"), nil)
	if err != nil {
		t.Fatalf("the retry on node b: %v", err)
	}
	b.drain()

	keys := map[string]string{}
	for _, id := range []string{"t0", "t1", "retried"} {
		task := oneTask(t, b, id)
		if other, taken := keys[task.Key]; taken {
			t.Fatalf("%s and %s are both %s — the retry built on the number its "+
				"stale counter implied, which another task already holds",
				other, id, task.Key)
		}
		keys[task.Key] = id
	}
	if filed := oneTask(t, b, "retried"); got.Key != filed.Key {
		t.Errorf("the retry answered key %q and filed the task as %q", got.Key, filed.Key)
	}
}

// A FRESH MINT WHOSE ANSWER IT CANNOT PROVE ITS OWN IS MINTED AGAIN, and the
// create still files one task under a key nobody else holds.
//
// A lost acknowledgement the ledger resolves is collapsed now, whoever's copy
// it names — so a create's own counter step, and then the fresh mint it
// resumes on, each come back as a number that cannot be built on. Each is a
// gap; the create takes the next.
func TestAFreshMintAnsweredFromTheLedgerIsMintedAgain(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	filedTask(t, r, "t0")
	lossy, lost := r.lossyWriter(t)

	// THE CREATE'S OWN COUNTER STEP, AND THE FIRST FRESH MINT, each lose
	// their acknowledgement.
	lost.dropAcksFor(".counter.ENG", 2)
	got, err := lossy.CreateTask(t.Context(), statelog.NewOpID(time.Now(), "create"),
		newTask("x"), nil)
	if err != nil {
		t.Fatalf("a create whose mints lost their answers: %v", err)
	}
	r.drain()
	filed := oneTask(t, r, "x")
	if filed.Key == oneTask(t, r, "t0").Key || got.Key != filed.Key {
		t.Fatalf("the create answered %q and filed %q beside t0's %q",
			got.Key, filed.Key, oneTask(t, r, "t0").Key)
	}
	if filed.Key != "ENG-4" {
		t.Errorf("x is %s, want ENG-4 — ENG-2 and ENG-3 are the two mints "+
			"nobody could prove were this create's", filed.Key)
	}
}

// AND A BROKER DROPPING EVERY ANSWER ENDS THE CREATE rather than spending
// numbers without limit.
func TestFreshMintsStopAfterTheirBound(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	lossy, lost := r.lossyWriter(t)
	end := r.logEnd(t)
	lost.dropAcksFor(".counter.ENG", 100)
	if _, err := lossy.CreateTask(t.Context(), statelog.NewOpID(time.Now(), "create"),
		newTask("x"), nil); err == nil {
		t.Fatal("a create whose every mint lost its answer filed a task")
	}
	// ITS OWN COUNTER STEP AND THREE FRESH MINTS, and nothing more.
	if got := r.logEnd(t) - end; got != 4 {
		t.Errorf("one create against a broker dropping every answer put %d "+
			"counter record(s) on the log, want 4", got)
	}
}
