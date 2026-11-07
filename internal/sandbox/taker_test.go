package sandbox

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
)

// The processes the cases below are taken by: this node's own, node-a's that
// took the capture, and node-a's next, which holds node-a's presence once the
// first has stopped.
const (
	thisProcess      = "node-b:1"
	takingProcess    = "node-a:1"
	restartedProcess = "node-a:2"
)

// nodePresence is the fleet's roster as a case sets it ([PresenceRoster]): the
// process holding each node's presence lease, and every node it was asked
// about.
type nodePresence struct {
	mu    sync.Mutex
	held  map[string]string
	asked []string
}

func (p *nodePresence) Holder(_ context.Context, node string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = append(p.asked, node)
	return p.held[node], nil
}

func (p *nodePresence) askedOf() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.asked)
}

// presenceOf is a roster in which holder holds node-a's presence, and nobody
// does for "".
func presenceOf(holder string) *nodePresence {
	return &nodePresence{held: map[string]string{"node-a": holder}}
}

// takenBy stamps on t1's row a capture of its job at the place after kept,
// begun by taker at the rig's now and naming no object yet: a take whose
// process stopped before it stamped what came of it, or is still keeping it.
func takenBy(t *testing.T, rig *coordRig, kept SessionCapture, taker string) SessionCapture {
	t.Helper()
	begun := kept
	begun.CaptureSeq, begun.State, begun.At, begun.Taker = kept.CaptureSeq+1, CaptureBegun, rig.coordinator.now(), taker
	begun.Object, begun.ObjectHash, begun.ObjectSize, begun.ContentKey, begun.ContextTokens = "", "", 0, "", 0
	if took, err := rig.pending.StampCapture(t.Context(), "t1", begun); err != nil || !took {
		t.Fatalf("stamping the begun capture = %v, %v", took, err)
	}
	return begun
}

// landedRow is the turn's row on the executor log once begun's record landed:
// it names begun's place, an object of its own, the key that sealed it and what
// the session's last request carried — what only the keep that recorded it
// knew.
func landedRow(t *testing.T, begun SessionCapture) executorkeep.TurnRow {
	t.Helper()
	landed := begun
	landed.Object, landed.ObjectHash, landed.ObjectSize = objstore.KeyAt(begun.At.Add(time.Second)).String(),
		"h2", 2048
	row := keptRow(t, landed)
	row.ContentKey, row.ContextTokens = "k2", 42_000
	return row
}

// onTheLog makes row the turn's row on the executor log, as a record that
// lands makes it.
func onTheLog(restore *keptRestore, row executorkeep.TurnRow) {
	restore.mu.Lock()
	defer restore.mu.Unlock()
	restore.rows[row.Turn] = row
}

// A CAPTURE WHOSE TAKER IS GONE IS SETTLED FROM THE TURN'S ROW. Every stamp of a
// take names the process that took it, and a take dies with its process,
// leaving its place begun. Once that process has stopped — its node runs
// another process now, or the run was lost with its node, which left the
// fleet — what came of the capture is read off the turn's row on the executor
// log, which has seen every record the process published: LANDED where the row
// names its place, the stamp written from the row (the object, the key that
// sealed it, what its last request carried) and kept as the turn's; REFUSED
// where it does not, saying its taker stopped, the turn's kept capture left as
// it was.
//
// Mutation: drop the taker from a take's stamp, and the premise fails; read a
// node held by another process as the taker's, or drop the loss verdict, and a
// stopped process's capture is left begun; stamp it refused whatever the row
// names, and a capture the log took is read as lost; or leave the row's object
// off the landed stamp, and the turn keeps a capture naming none.
func TestACaptureWhoseTakerIsGoneIsSettledFromTheTurnsRow(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		holder string // the process holding node-a's presence
		lost   bool   // the run's loss verdict: node-a left the fleet
		landed bool   // the begun capture's record is on the log
	}{
		{name: "its node runs another process, and its record landed", holder: restartedProcess, landed: true},
		{name: "its node runs another process, and its record never landed", holder: restartedProcess},
		{name: "its node left the fleet, and its record never landed", lost: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rig, restore, kept := keptTurnBy(t, thisProcess)
			if kept.Taker != thisProcess {
				t.Fatalf("the premise: the collection's capture names %q as its taker, want this process's %q",
					kept.Taker, thisProcess)
			}
			rig.coordinator.presence = presenceOf(c.holder)
			begun := takenBy(t, rig, kept, takingProcess)
			var row executorkeep.TurnRow
			if c.landed {
				row = landedRow(t, begun)
				onTheLog(restore, row)
			}
			run := rig.get("t1")
			if c.lost {
				run.Lost = &JobLoss{Cause: LossLaunchNodeGone, Node: "node-a", At: rig.now}
			}
			var looked SessionCapture
			settled, found := rig.coordinator.settleGoneTaker(t.Context(), time.Time{}, run, &looked)
			if !found {
				t.Fatal("the run's row was read as gone")
			}
			got := rig.get("t1")
			if got.Capture == nil || !got.Capture.sameTake(begun) || settled.Capture == nil ||
				settled.Capture.State != got.Capture.State {
				t.Fatalf("the row holds %+v (answered %+v), want the begun take settled", got.Capture,
					settled.Capture)
			}
			if !c.landed {
				if got.Capture.State != CaptureRefused ||
					!strings.Contains(got.Capture.Reason, "its taker node-a stopped before it was kept") {
					t.Errorf("the capture stands %s (%q), want it refused for its stopped taker",
						got.Capture.State, got.Capture.Reason)
				}
				if got.Kept == nil || !got.Kept.samePlace(kept) {
					t.Errorf("the row keeps %+v, want the turn's kept capture left as it was", got.Kept)
				}
				return
			}
			if got.Capture.State != CaptureLanded || got.Capture.Object != row.Object.Key.String() ||
				got.Capture.ObjectSize != row.Object.Size || got.Capture.ContentKey != "k2" ||
				got.Capture.ContextTokens != 42_000 || got.Capture.Taker != takingProcess {
				t.Errorf("the capture stands %+v, want it landed as the log's row names it, its taker kept",
					got.Capture)
			}
			if got.Kept == nil || !got.Kept.samePlace(begun) || got.Kept.State != CaptureLanded {
				t.Errorf("the row keeps %+v, want the landed capture kept as the turn's", got.Kept)
			}
		})
	}
}

// AN END DOES NOT WAIT OUT A CAPTURE WHOSE TAKER IS GONE. The turn's end waits
// for its captures to settle, since its tombstone refuses a record that lands
// after it; a capture whose process stopped before it stamped what came of it
// never settles by itself, and was waited for as long as a capture can take —
// about twenty-one minutes, the seat's mail held behind it — and then named
// begun and never finished. Its node runs another process now, so the end's
// wait settles it from the turn's row at its first pass, and records the end
// naming it refused because its taker stopped.
//
// IN A SYNCTEST BUBBLE, so the wait is measured on the bubble's clock, which
// only the wait's own timers advance.
//
// Mutation: drop the settle from the end's wait ([Coordinator.awaitCapture]),
// and the end is recorded twenty-one minutes on, naming the capture begun and
// never finished.
func TestAnEndDoesNotWaitOutACaptureWhoseTakerIsGone(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rig, writes, _ := keepingRig(t)
		rig.coordinator.incarnation = thisProcess
		rig.coordinator.presence = presenceOf(restartedProcess)
		rig.coordinator.captureTick = time.Second
		run := rig.launchKept(t, "t1")
		// THE JOB'S COLLECTION, TAKEN BY node-a's PROCESS, which stopped
		// before it stamped what came of it: a take of the ended job, so the
		// loss's ending reads its box no more.
		begun := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID,
			State: CaptureBegun, Final: true, At: rig.coordinator.now(), Taker: takingProcess,
			Harness: "claude-code", SessionID: "s-1"}
		if took, err := rig.pending.StampCapture(t.Context(), "t1", begun); err != nil || !took {
			t.Fatalf("stamping the begun capture = %v, %v", took, err)
		}
		// THE TURN'S ROW ON THE LOG, its begin's: it names no capture.
		rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{
			"t1": {Handle: "swe", Turn: "t1"}}}
		started := time.Now()
		rig.coordinator.finish(t.Context(), rig.get("t1"), ending{fence: fenceOf(run),
			note: &failureNote{reason: string(LossBoxGone), detail: "the box is gone from its host"}})
		// THE ENDING'S OWN RETRY, off the frame that decided it, which waits
		// for the turn's captures and records the end.
		for range 3 {
			if rig.fireRetries() == 0 {
				break
			}
		}
		waited := time.Since(started)

		ends := writes.endedRecords()
		if len(ends) != 1 {
			t.Fatalf("recorded %d ends, want the lost turn's one", len(ends))
		}
		unkept := ends[0].Ending.Unkept
		if unkept == nil || unkept.LaunchSeq != run.LaunchSeq || unkept.CaptureSeq != 1 ||
			!strings.Contains(unkept.Reason, "its taker node-a stopped before it was kept") {
			t.Errorf("the end names %+v unkept, want the capture refused for its stopped taker", unkept)
		}
		if waited >= time.Minute {
			t.Errorf("the end waited %s for a capture whose taker had stopped, want one read of the log", waited)
		}
		if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
			t.Error("the lost run's record was left behind its end")
		}
	})
}

// A RELAUNCH SETTLES A CAPTURE WHOSE TAKER IS GONE BEFORE IT DECIDES. A job lost
// with its node's process — its box gone with the restart that replaced it —
// is relaunched within a minute or two, and the newest capture of it can be one
// that process was taking, still stamped begun. The relaunch waited its
// fifteen seconds for it, said it was still being kept and started a session
// of its own — beside a record the log may well have taken. Its taker's node
// runs another process now, so the relaunch's wait settles it from the turn's
// row at once: one the row names landed, and is continued; one it does not was
// refused, which the relaunch says. A taker that still runs is waited for, as
// before.
//
// Mutation: drop the settle from the relaunch's wait, and the first two cases
// say the capture is still being kept; settle a capture whose taker still
// runs, and the third is decided under a keep still under way.
func TestARelaunchSettlesACaptureWhoseTakerIsGoneBeforeItDecides(t *testing.T) {
	t.Parallel()
	relaunch := func(t *testing.T, holder string, landed bool,
		wait time.Duration) (*coordRig, SessionCapture, ContinuedSession, string) {

		t.Helper()
		rig, restore, kept := keptTurnBy(t, thisProcess)
		rig.coordinator.presence = presenceOf(holder)
		rig.coordinator.relaunchWait = wait
		begun := takenBy(t, rig, kept, takingProcess)
		if landed {
			onTheLog(restore, landedRow(t, begun))
		}
		continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", rig.runner)
		return rig, begun, continued, why
	}
	t.Run("its record landed", func(t *testing.T) {
		t.Parallel()
		rig, begun, continued, why := relaunch(t, restartedProcess, true, 2*time.Second)
		if why != "" || !continued.Capture.samePlace(begun) || continued.Capture.State != CaptureLanded ||
			continued.Context != 42_000 {
			t.Fatalf("the relaunch continues %+v (%q), want the capture the stopped process landed, of "+
				"42000 tokens", continued, why)
		}
		if kept := rig.get("t1").Kept; kept == nil || !kept.samePlace(begun) {
			t.Errorf("the row keeps %+v, want the capture the relaunch continued", kept)
		}
	})
	t.Run("its record never landed", func(t *testing.T) {
		t.Parallel()
		rig, begun, continued, why := relaunch(t, restartedProcess, false, 2*time.Second)
		if !strings.Contains(why, "its taker node-a stopped before it was kept") ||
			strings.Contains(why, "still being kept") || continued.Capture.CaptureSeq != 0 {
			t.Fatalf("the relaunch continues %+v (%q), want none, said: its taker stopped", continued, why)
		}
		if c := rig.get("t1").Capture; c == nil || !c.sameTake(begun) || c.State != CaptureRefused {
			t.Errorf("the row holds %+v, want the begun capture refused", c)
		}
	})
	t.Run("its taker still runs", func(t *testing.T) {
		t.Parallel()
		rig, begun, _, why := relaunch(t, takingProcess, true, 50*time.Millisecond)
		if !strings.Contains(why, "still being kept") {
			t.Fatalf("the relaunch answered %q, want the capture of a running taker waited for", why)
		}
		if c := rig.get("t1").Capture; c == nil || !c.sameTake(begun) || c.State != CaptureBegun {
			t.Errorf("the row holds %+v, want the running taker's capture left begun", c)
		}
	})
}

// AN UNRESOLVED CAPTURE IS ASKED AGAIN BEFORE A RELAUNCH DECIDES. A capture whose
// record was sent and never answered may have landed; only the turn's end asked
// it again, so a relaunch that followed read it as never answered and started a
// session of its own beside a record the log had taken. The relaunch asks it
// again under its own operation, as the end does, within its own wait: landed,
// it is continued.
//
// Mutation: drop the ask from the relaunch ([Coordinator.ContinuedCapture]),
// and it says the capture's record was never answered and continues nothing.
func TestAnUnresolvedCaptureIsAskedAgainBeforeARelaunchDecides(t *testing.T) {
	t.Parallel()
	rig, writes, _ := keepingRig(t)
	rig.runner.Carries = true
	rig.launchKept(t, "t1")
	session := rig.runner.Session
	writes.answer(errors.New("estate: the write was sent to a data node that did not answer"))
	rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-1", HarnessVersion: "2.1.310",
		Captured: &session.Archive})
	collectHeld(t, rig, "t1")
	unresolved := rig.get("t1").Capture
	if unresolved == nil || unresolved.State != CaptureUnresolved {
		t.Fatalf("the premise: the row holds %+v, want its capture unresolved", unresolved)
	}
	writes.answer(nil)
	rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{"t1": keptRow(t, *unresolved)}}

	continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", rig.runner)
	if why != "" || !continued.Capture.samePlace(*unresolved) || continued.Capture.State != CaptureLanded {
		t.Fatalf("the relaunch continues %+v (%q), want the capture its ask landed", continued, why)
	}
	if kept := rig.get("t1").Kept; kept == nil || !kept.samePlace(*unresolved) || kept.State != CaptureLanded {
		t.Errorf("the row keeps %+v, want the capture the relaunch's ask landed", kept)
	}
	captures := writes.captured()
	if len(captures) < 2 {
		t.Fatalf("the log was asked %d times, want the collection's attempts and the relaunch's ask", len(captures))
	}
	last := captures[len(captures)-1]
	if last.err != nil || last.operation() != captures[0].operation() {
		t.Errorf("the log was last asked %+v, want the capture asked again under its own operation and landed",
			last)
	}
}

// A TAKER WHOSE PRESENCE LAPSED IS SETTLED UNDER IT ONLY WHERE ITS RECORD
// LANDED. A node gives its presence up at the first step of a drain, while its
// keeps still upload, so a taker whose node holds no presence may be a process
// still keeping its capture. Where the turn's row already names the capture,
// it landed, and the relaunch adopts it and continues it; the taker's own
// landed stamp, coming after, changes nothing. Where the row does not, the
// capture is waited for as before, and continued once the taker's keep lands —
// refused under a taker that is draining, the turn's end could tombstone the
// turn before that keep's record landed, and lose the turn's last session. And
// the row is read for it once while its taker is away, never at every pass of
// the wait: each read is a barrier on the executor log.
//
// Mutation: refuse a capture whose taker only holds no presence, and the last
// two cases refuse a keep still under way; read the row at every pass, and the
// second reads it at every tick; leave such a capture unread, and the first
// waits out the relaunch and starts fresh beside a capture the log took.
func TestATakerWhosePresenceLapsedIsSettledUnderItOnlyWhereItsRecordLanded(t *testing.T) {
	t.Parallel()
	t.Run("its record landed", func(t *testing.T) {
		t.Parallel()
		rig, restore, kept := keptTurnBy(t, thisProcess)
		rig.coordinator.presence = presenceOf("")
		rig.coordinator.relaunchWait = 2 * time.Second
		begun := takenBy(t, rig, kept, takingProcess)
		row := landedRow(t, begun)
		onTheLog(restore, row)
		continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", rig.runner)
		if why != "" || !continued.Capture.samePlace(begun) {
			t.Fatalf("the relaunch continues %+v (%q), want the capture the log names", continued, why)
		}
		// THE TAKER'S OWN STAMP, as its keep lands after all.
		own := begun
		own.State, own.Object, own.ObjectHash, own.ObjectSize = CaptureLanded, row.Object.Key.String(),
			string(row.Object.Hash), row.Object.Size
		if _, err := rig.pending.StampCapture(t.Context(), "t1", own); err != nil {
			t.Fatalf("the taker's own stamp: %v", err)
		}
		if got := rig.get("t1"); got.Kept == nil || !got.Kept.samePlace(begun) || got.Kept.State != CaptureLanded ||
			got.Kept.ContextTokens != 42_000 {
			t.Errorf("the row keeps %+v after the taker's own stamp, want the capture adopted as it was", got.Kept)
		}
	})
	t.Run("its record lands while the relaunch waits", func(t *testing.T) {
		t.Parallel()
		rig, restore, kept := keptTurnBy(t, thisProcess)
		rig.coordinator.presence = presenceOf("")
		rig.coordinator.relaunchWait = 10 * time.Second
		rig.coordinator.captureTick = 5 * time.Millisecond
		begun := takenBy(t, rig, kept, takingProcess)
		row := landedRow(t, begun)
		own := begun
		own.State, own.Object, own.ObjectHash, own.ObjectSize = CaptureLanded, row.Object.Key.String(),
			string(row.Object.Hash), row.Object.Size
		landing := make(chan struct{})
		go func() {
			defer close(landing)
			// THE KEEP LANDS once the wait has read the row for its absent
			// taker and gone round several passes after — or, should it never
			// read it, after a while, so the case reports that instead of
			// hanging — never on a timer that a stalled runner could let run
			// out before the wait's first pass.
			giveUp := time.Now().Add(5 * time.Second)
			for len(restore.askedFor()) == 0 && time.Now().Before(giveUp) {
				time.Sleep(time.Millisecond)
			}
			time.Sleep(50 * time.Millisecond)
			onTheLog(restore, row)
			_, _ = rig.pending.StampCapture(t.Context(), "t1", own)
		}()
		continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", rig.runner)
		<-landing
		if why != "" || !continued.Capture.samePlace(begun) || continued.Capture.State != CaptureLanded {
			t.Fatalf("the relaunch continues %+v (%q), want the capture its draining taker landed", continued, why)
		}
		if asked := restore.askedFor(); len(asked) != 2 {
			t.Errorf("the log was read %d times for the turn's row, want twice: once while its taker was away, "+
				"once for the decision", len(asked))
		}
	})
	t.Run("its record has not landed when the relaunch decides", func(t *testing.T) {
		t.Parallel()
		rig, _, kept := keptTurnBy(t, thisProcess)
		rig.coordinator.presence = presenceOf("")
		rig.coordinator.relaunchWait = 50 * time.Millisecond
		begun := takenBy(t, rig, kept, takingProcess)
		if _, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", rig.runner); !strings.Contains(why,
			"still being kept") {
			t.Fatalf("the relaunch answered %q, want the capture of a taker without its presence waited for", why)
		}
		if c := rig.get("t1").Capture; c == nil || !c.sameTake(begun) || c.State != CaptureBegun {
			t.Errorf("the row holds %+v, want the capture left begun for its taker to settle", c)
		}
	})
}

// A STAMP WITH NO TAKER IS SETTLED BY ITS AGE ALONE. An older build names no
// process on its stamps, so nothing says whether the take that left one begun
// still runs: a relaunch waits for it its fifteen seconds as before, and a
// stamp older than a capture can take is read as one whose node stopped or
// whose budget ran out — the roster never asked, and the log never read, for a
// process nobody named.
//
// Mutation: ask the roster of a stamp with no taker, or settle one from the
// log, and a check below goes red.
func TestAStampWithNoTakerIsSettledByAge(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		age  time.Duration
		why  string
	}{
		{name: "younger than a capture can take", why: "still being kept"},
		{name: "older than a capture can take", age: SessionCaptureBudget,
			why: "it was still being kept, or was begun and never finished"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rig, restore, kept := keptTurnBy(t, thisProcess)
			presence := presenceOf(restartedProcess)
			rig.coordinator.presence = presence
			rig.coordinator.relaunchWait = 50 * time.Millisecond
			begun := takenBy(t, rig, kept, "")
			rig.now = rig.now.Add(c.age)
			if _, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", rig.runner); !strings.Contains(why,
				c.why) {
				t.Fatalf("the relaunch answered %q, want %q", why, c.why)
			}
			if got := rig.get("t1").Capture; got == nil || !got.sameTake(begun) || got.State != CaptureBegun {
				t.Errorf("the row holds %+v, want the older build's capture left begun", got)
			}
			if asked := presence.askedOf(); len(asked) != 0 {
				t.Errorf("the roster was asked of %v, want nothing asked of a stamp that names no taker", asked)
			}
			if asked := restore.askedFor(); len(asked) != 0 {
				t.Errorf("the log was read %v, want nothing read for a stamp that names no taker", asked)
			}
		})
	}
}
