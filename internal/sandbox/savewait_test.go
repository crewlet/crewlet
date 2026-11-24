package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
)

// overtakenSave reserves on turnID's row, its job running, a save of the job
// at the next place, taken under taker's lease at epoch ("" for a stamp naming
// none), and stamps the job's final take at the place after it, begun under
// the same lease and then come out as final: a save still uploading when the
// read that collected its job took the job's final session. It answers the
// save as reserved and the final take as stamped, failing the case unless the
// row holds the final take as its newest capture and the save under way
// beside it.
func overtakenSave(t *testing.T, rig *coordRig, turnID, taker string, epoch int64,
	final CaptureState) (SessionCapture, SessionCapture) {

	t.Helper()
	run := rig.get(turnID)
	save := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: nextCaptureSeq(run), Launch: run.LaunchID,
		State: CaptureBegun, Interrupted: true, Through: new(0), At: rig.now, Taker: taker, TakerEpoch: epoch,
		Harness: "claude-code", HarnessVersion: "2.1.310", SessionID: "s-1", ArchiveFormat: FakeArchiveFormat}
	if took, err := rig.pending.ReserveSave(t.Context(), turnID, save); err != nil || !took {
		t.Fatalf("reserving the save = %v, %v; want it taken", took, err)
	}
	begun := save
	begun.CaptureSeq, begun.Final, begun.Interrupted, begun.At = save.CaptureSeq+1, true, false, rig.now.Add(time.Second)
	if took, err := rig.pending.StampCapture(t.Context(), turnID, begun); err != nil || !took {
		t.Fatalf("stamping the final take begun = %v, %v; want it taken", took, err)
	}
	overtaking := begun
	if final != CaptureBegun {
		overtaking = comeOut(begun, final)
		if took, err := rig.pending.StampCapture(t.Context(), turnID, overtaking); err != nil || !took {
			t.Fatalf("stamping the final take %s = %v, %v; want it taken", final, took, err)
		}
	}
	got := rig.get(turnID)
	if newest := got.Capture; newest == nil || !newest.sameTake(overtaking) || newest.State != final {
		t.Fatalf("the premise: the row's newest capture is %+v, want the final take %s", newest, final)
	}
	if final != CaptureLanded && (got.Saving == nil || !got.Saving.sameTake(save) || got.Saving.State != CaptureBegun) {
		t.Fatalf("the premise: the row's save under way is %+v, want the save begun beside the final take",
			got.Saving)
	}
	return save, overtaking
}

// comeOut is take come out as state, as its taker stamps it: landed or
// unresolved naming the object it uploaded, refused saying why.
func comeOut(take SessionCapture, state CaptureState) SessionCapture {
	take.State = state
	switch state {
	case CaptureLanded, CaptureUnresolved:
		take.Object, take.ObjectHash = objstore.KeyAt(take.At).String(), strings.Repeat("a", 64)
		take.ObjectSize, take.ContentKey = 1024, "k1"
	case CaptureRefused:
		take.Reason = "the object store had no room for the session"
	}
	return take
}

// stampOut stamps take come out as state on turnID's row ([comeOut]), which
// must take it.
func stampOut(t *testing.T, rig *coordRig, turnID string, take SessionCapture, state CaptureState) SessionCapture {
	t.Helper()
	return stampOutIn(t.Context(), t, rig, turnID, take, state)
}

// stampOutIn is stampOut from inside a call the coordinator made, under ctx.
func stampOutIn(ctx context.Context, t *testing.T, rig *coordRig, turnID string, take SessionCapture,
	state CaptureState) SessionCapture {

	t.Helper()
	out := comeOut(take, state)
	if took, err := rig.pending.StampCapture(ctx, turnID, out); err != nil || !took {
		t.Fatalf("stamping (%d, %d) %s = %v, %v; want it taken", out.LaunchSeq, out.CaptureSeq, state, took, err)
	}
	return out
}

// owingEnd records on turnID's row the end its turn owes, as the ending that
// decided it does, and answers the row.
func owingEnd(t *testing.T, rig *coordRig, turnID string) PendingRun {
	t.Helper()
	if owed, err := rig.coordinator.OweEnd(t.Context(), turnID, executorkeep.Ending{
		Outcome: executorkeep.OutcomeCompleted}); err != nil || !owed {
		t.Fatalf("OweEnd = %v, %v", owed, err)
	}
	return rig.get(turnID)
}

// AN END WAITS FOR A SAVE A FINAL TAKE OVERTOOK. A save of a job still working
// was uploading when the read that collected the job took its final session in
// the place after it, and that final take's keep was refused: the save is the
// newest session of the job anybody keeps, and the turn's end tombstones the
// turn, so an end recorded before the save lands refuses it. The end's wait
// holds until the save lands, and the end then names the final take unkept,
// says the turn went on past the session its row names, and names that final
// take its last word; the run's record keeps the save.
//
// Mutation: leave the save under way out of the end's wait (captureInFlight),
// and the end is recorded at once, before the save lands, its record keeping
// nothing.
func TestAnEndWaitsForASaveAFinalTakeOvertook(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, writes, _ := keepingRig(t)
		rig.launchKept(t, "t1")
		save, final := overtakenSave(t, rig, "t1", "", 0, CaptureRefused)
		owing := owingEnd(t, rig, "t1")
		landing := make(chan struct{})
		go func() {
			defer close(landing)
			time.Sleep(50 * time.Millisecond)
			_, _ = rig.pending.StampCapture(context.Background(), "t1", comeOut(save, CaptureLanded))
		}()
		started := time.Now()
		waited, found, err := rig.coordinator.awaitEndCaptures(t.Context(), owing)
		elapsed := time.Since(started)
		<-landing
		if err != nil || !found {
			t.Fatalf("the end's wait = found %v, %v", found, err)
		}
		if kept := waited.Kept; kept == nil || !kept.samePlace(save) || kept.State != CaptureLanded {
			t.Fatalf("the end's wait returned after %s with the record keeping %+v, want it held until the "+
				"save under way landed and the record keeps it", elapsed, kept)
		}
		if _, err := rig.coordinator.publishEnd(t.Context(), waited); err != nil {
			t.Fatalf("publishEnd: %v", err)
		}
		ends := writes.endedRecords()
		if len(ends) != 1 {
			t.Fatalf("recorded %d ends, want the turn's one", len(ends))
		}
		end := ends[0].Ending
		if u := end.Unkept; u == nil || u.LaunchSeq != final.LaunchSeq || u.CaptureSeq != final.CaptureSeq {
			t.Errorf("the end names %+v unkept, want the refused final take (%d, %d)", u, final.LaunchSeq,
				final.CaptureSeq)
		}
		if !end.WentOn {
			t.Error("the end says the turn did not go on past the save its row names")
		}
		if w := end.LastWord; w == nil || w.LaunchSeq != final.LaunchSeq || w.CaptureSeq != final.CaptureSeq {
			t.Errorf("the end names %+v its last word, want the refused final take", w)
		}
	})
}

// AN END DOES NOT WAIT FOR A SAVE A KEPT CAPTURE SUPERSEDED. A build without
// the save's section carries it on the row as it found it, and lets nothing of
// it go: a row it wrote can hold a save still young and begun beside the job's
// final take, landed and kept. That save's landing can change nothing (the
// record keeps the newer capture, and the log refuses a capture no newer than
// its row's), so the end does not wait for it, by its place rather than its
// age: it records at once, naming nothing unkept.
//
// Mutation: wait on any begun save, whatever the row keeps, and the end waits
// out its whole bound on a save nothing will settle.
func TestAnEndDoesNotWaitForASaveAKeptCaptureSuperseded(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, writes, _ := keepingRig(t)
		rig.launchKept(t, "t1")
		save, final := overtakenSave(t, rig, "t1", "", 0, CaptureLanded)
		// THE OLDER BUILD'S ROW: the save it carried, as it carried it.
		if _, ok, err := rig.pending.runs.MutateSandboxRun(t.Context(), "t1",
			func(current coord.Record) (coord.SandboxRunChange, error) {
				var row PendingRun
				if err := json.Unmarshal(current.Value, &row); err != nil {
					return coord.SandboxRunChange{}, err
				}
				row.Saving = &save
				raw, err := json.Marshal(row)
				return coord.SandboxRunChange{Put: raw}, err
			}); err != nil || !ok {
			t.Fatalf("writing the row as a build without the save's section = %v, %v", ok, err)
		}
		owing := owingEnd(t, rig, "t1")
		if owing.Saving == nil || !owing.Saving.sameTake(save) || owing.Kept == nil || !owing.Kept.samePlace(final) {
			t.Fatalf("the premise: the row keeps %+v beside the save %+v, want the final take kept beside the "+
				"save the older build carried", owing.Kept, owing.Saving)
		}
		started := time.Now()
		waited, found, err := rig.coordinator.awaitEndCaptures(t.Context(), owing)
		if elapsed := time.Since(started); err != nil || !found || elapsed > 0 {
			t.Fatalf("the end's wait = found %v, %v after %s; want it over at once", found, err, elapsed)
		}
		if _, err := rig.coordinator.publishEnd(t.Context(), waited); err != nil {
			t.Fatalf("publishEnd: %v", err)
		}
		if ends := writes.endedRecords(); len(ends) != 1 || ends[0].Ending.Unkept != nil || ends[0].Ending.WentOn {
			t.Fatalf("ended %+v, want the turn's one end naming nothing unkept: its final take is kept", ends)
		}
	})
}

// A SAVE WHOSE TAKER'S LEASE LAPSED IS SETTLED FROM THE TURN'S ROW, as the
// newest capture is: LANDED and kept where the turn's row on the executor log
// names its place, REFUSED and let go where it does not, rather than waited out
// to its budget. A pass that finds both the newest capture and the save begun
// under a gone taker reads the turn's row once and the lease once, and settles
// both.
//
// Mutation: settle the newest capture alone, and the end's wait runs to its
// bound on the save; read the row or the lease once per stamp, and the pass
// reads twice.
func TestASaveWhoseTakersLeaseLapsedIsSettledFromTheTurnsRow(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		final CaptureState // the overtaking final take's stamp
		named bool         // the turn's row on the log names the save's place
	}{
		{name: "its record landed", final: CaptureRefused, named: true},
		{name: "its record never landed", final: CaptureRefused},
		{name: "beside a final take whose taker is gone too", final: CaptureBegun, named: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				store := newTakerStore()
				rig, _, _ := keepingRigWith(t, taking(thisProcess, store))
				restore := &keptRestore{rows: map[string]executorkeep.TurnRow{}, noRowFound: true}
				rig.coordinator.restore = restore
				rig.launchKept(t, "t1")
				lease := claimTaker(t, store, takingProcess)
				save, final := overtakenSave(t, rig, "t1", takingProcess, lease.Epoch, c.final)
				if c.named {
					onTheLog(restore, landedRow(t, save))
				}
				lapse(store)
				leaseReads := store.readsOf(takingProcess)
				started := time.Now()
				_, found, err := rig.coordinator.awaitEndCaptures(t.Context(), rig.get("t1"))
				if elapsed := time.Since(started); err != nil || !found || elapsed > 0 {
					t.Fatalf("the end's wait = found %v, %v after %s; want it over at its first pass", found, err,
						elapsed)
				}
				got := rig.get("t1")
				if c.named {
					if kept := got.Kept; kept == nil || !kept.sameTake(save) || kept.State != CaptureLanded ||
						kept.ContextTokens != 42_000 {
						t.Errorf("the row keeps %+v, want the save landed from the turn's row", kept)
					}
				} else if got.Kept != nil {
					t.Errorf("the row keeps %+v, want nothing: the save's record never landed", got.Kept)
				}
				if got.savingTake() != nil {
					t.Errorf("the row still holds the save under way %+v, want it settled and let go", got.Saving)
				}
				if newest := got.Capture; newest == nil || !newest.sameTake(final) || newest.State != CaptureRefused {
					t.Errorf("the row's newest capture is %+v, want the final take refused", newest)
				}
				if asked := restore.askedFor(); len(asked) != 1 {
					t.Errorf("the turn's row was read %d times, want once for the pass", len(asked))
				}
				if reads := store.readsOf(takingProcess) - leaseReads; reads != 1 {
					t.Errorf("the taker's lease was read %d times, want once for the pass", reads)
				}
			})
		})
	}
}

// AN UNRESOLVED SAVE IS ASKED AGAIN BEFORE A RELAUNCH OR AN END DECIDES: beside
// a refused final take it is the job's newest session anybody kept, so the end
// asks it before it records and the relaunch before it decides, which then
// continues it and names the turn's own session to its run. The end asks it
// rather than wait for it: nothing of it is being kept any more, and only an
// ask can say whether the log took it.
//
// Mutation: ask the newest capture alone, and the end records beside a record
// keeping nothing and the relaunch continues nothing; count a save awaiting its
// answer as a keep in flight, and the end waits its whole bound first.
func TestAnUnresolvedSaveIsAskedAgainBeforeARelaunchOrAnEndDecides(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		end  bool
	}{
		{name: "by the turn's end", end: true},
		{name: "by a relaunch"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, writes, _ := keepingRig(t)
				rig.runner.Carries = true
				run := rig.launchKept(t, "t1")
				save, final := overtakenSave(t, rig, "t1", "", 0, CaptureBegun)
				// THE LOG TOOK THE SAVE'S RECORD, which an ask lands.
				rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{
					"t1": keptRow(t, comeOut(save, CaptureLanded))}}
				stampOut(t, rig, "t1", save, CaptureUnresolved)
				stampOut(t, rig, "t1", final, CaptureRefused)
				asked := len(writes.captured())
				if c.end {
					started := time.Now()
					waited, found, err := rig.coordinator.awaitEndCaptures(t.Context(), owingEnd(t, rig, "t1"))
					if elapsed := time.Since(started); err != nil || !found || elapsed > 0 {
						t.Fatalf("the end's wait = found %v, %v after %s; want it over at once beside a save "+
							"whose record awaits its answer", found, err, elapsed)
					}
					if _, err := rig.coordinator.publishEnd(t.Context(), waited); err != nil {
						t.Fatalf("publishEnd: %v", err)
					}
					ends := writes.endedRecords()
					if len(ends) != 1 || ends[0].Ending.Unkept == nil || ends[0].Ending.Unkept.CaptureSeq !=
						final.CaptureSeq || !ends[0].Ending.WentOn {
						t.Errorf("ended %+v, want the turn's one end naming the refused final take unkept, "+
							"and the turn gone on past the save", ends)
					}
				} else {
					lostWithItsBox(t, rig, run)
					continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, ""))
					if continued.KeptTurn != "t1" {
						t.Errorf("the relaunch names %q to its run as a session to read, want the turn's own, "+
							"whose save its ask landed", continued.KeptTurn)
					}
					if why != "" || !continued.Capture.sameTake(save) {
						t.Errorf("the relaunch continues %+v (%q), want the save its ask landed, past the refused "+
							"final take", continued, why)
					}
				}
				records := writes.captured()[asked:]
				if len(records) != 1 || records[0].err != nil || records[0].Stamp.CaptureSeq != save.CaptureSeq {
					t.Fatalf("the log was asked %+v, want the save's record asked again and landed", records)
				}
				if kept := rig.get("t1").Kept; kept == nil || !kept.sameTake(save) || kept.State != CaptureLanded {
					t.Errorf("the row keeps %+v, want the save the ask landed", kept)
				}
			})
		})
	}
}

// A RELAUNCH WAITS BRIEFLY FOR A SAVE OF THE JOB IT FOLLOWS, as for its newest
// capture: beside a refused final take the save is the job's newest session
// anybody may keep. Landed, the relaunch continues it and names the turn's own
// session to its run; still being kept when the wait runs out, it is named by
// its own place, never by the refused final take.
//
// Mutation: leave the save out of the relaunch's wait, and the relaunch decides
// at once and names the refused final take.
func TestARelaunchWaitsBrieflyForASaveOfTheJobItFollows(t *testing.T) {
	t.Parallel()
	t.Run("kept while it waits", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, _ := keepingRig(t)
			rig.runner.Carries = true
			rig.coordinator.relaunchWait = 10 * time.Second
			run := rig.launchKept(t, "t1")
			save, _ := overtakenSave(t, rig, "t1", "", 0, CaptureRefused)
			// THE LOG TAKES THE SAVE'S RECORD as its keep lands.
			rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{
				"t1": keptRow(t, comeOut(save, CaptureLanded))}}
			lostWithItsBox(t, rig, run)
			landing := make(chan struct{})
			go func() {
				defer close(landing)
				time.Sleep(20 * time.Millisecond)
				_, _ = rig.pending.StampCapture(context.Background(), "t1", comeOut(save, CaptureLanded))
			}()
			started := time.Now()
			continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, ""))
			elapsed := time.Since(started)
			<-landing
			if elapsed < 20*time.Millisecond || continued.KeptTurn != "t1" {
				t.Fatalf("the relaunch decided after %s (%q), naming %q to its run, want it to wait for the "+
					"save and name the turn's own session the save keeps", elapsed, why, continued.KeptTurn)
			}
			if kept := rig.get("t1").Kept; kept == nil || !kept.sameTake(save) {
				t.Errorf("the row keeps %+v, want the save that landed while the relaunch waited", kept)
			}
			if why != "" || !continued.Capture.sameTake(save) {
				t.Errorf("the relaunch continues %+v (%q), want the save that landed while it waited, past the "+
					"refused final take", continued, why)
			}
		})
	})
	t.Run("still being kept", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, _ := keepingRig(t)
			rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{}}
			rig.coordinator.relaunchWait = 50 * time.Millisecond
			run := rig.launchKept(t, "t1")
			save, _ := overtakenSave(t, rig, "t1", "", 0, CaptureRefused)
			lostWithItsBox(t, rig, run)
			_, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, ""))
			if want := fmt.Sprintf("(launch %d, capture %d) was still being kept", save.LaunchSeq,
				save.CaptureSeq); !strings.Contains(why, want) {
				t.Fatalf("the relaunch answered %q, want it naming the save it waited on: %q", why, want)
			}
		})
	})
}

// A RELAUNCH WAITS ONLY FOR A CAPTURE ITS DECISION DEPENDS ON, since its wait
// holds the seat's frame and an admission slot: a capture or save of the job it
// follows, and an earlier job's only while the turn has kept nothing. Beside a
// kept capture it decides at once; with none kept, it waits for an earlier
// job's save and continues the base once the save is refused, none once it
// lands, and names a save still under way as the earlier job's.
//
// Mutation: wait on every stamp whatever its job, and the relaunch beside a
// kept capture waits for nothing that changes it; wait only on the followed
// job's, and it continues the base beside a save about to end it; or name an
// earlier job's keep as the followed job's.
func TestARelaunchWaitsOnlyForACaptureItsDecisionDependsOn(t *testing.T) {
	t.Parallel()
	for name, keeping := range map[string]func(t *testing.T, rig *coordRig) SessionCapture{
		"an earlier job's capture beside a capture the turn kept": func(t *testing.T, rig *coordRig) SessionCapture {
			rig.launchKept(t, "t1")
			kept := keptSave(t, rig, "t1")
			begun := kept
			begun.CaptureSeq, begun.State, begun.At = kept.CaptureSeq+1, CaptureBegun, rig.coordinator.now()
			begun.Object, begun.ObjectHash, begun.ObjectSize, begun.ContentKey = "", "", 0, ""
			if took, err := rig.pending.StampCapture(t.Context(), "t1", begun); err != nil || !took {
				t.Fatalf("stamping the earlier job's capture begun = %v, %v", took, err)
			}
			return begun
		},
		"an earlier job's save beside a capture the turn kept": func(t *testing.T, rig *coordRig) SessionCapture {
			rig.launchKept(t, "t1")
			keptSave(t, rig, "t1")
			save, _ := overtakenSave(t, rig, "t1", "", 0, CaptureRefused)
			return save
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, _, _ := keepingRig(t)
				rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{}}
				rig.coordinator.relaunchWait = 10 * time.Second
				stamp := keeping(t, rig)
				// THE JOB AFTER IT, which left no capture: the relaunch follows that one.
				if _, err := rig.pending.BeginLaunch(t.Context(), PendingRun{TurnID: "t1", AgentHandle: "swe",
					AgentID: sweID, Role: "SWE", Harness: "claude-code"}, rigLease); err != nil {
					t.Fatalf("the second job: %v", err)
				}
				if waits := stampInFlight(rig.get("t1"), rig.coordinator.now(), anyStamp); waits == nil ||
					!waits.sameTake(stamp) {
					t.Fatalf("the premise: the turn's end would wait on %+v, want the earlier job's keep", waits)
				}
				asked := time.Now()
				_, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, ""))
				if waited := time.Since(asked); waited >= captureWaitTick {
					t.Errorf("the relaunch waited %s on an earlier job's keep beside a capture the turn kept", waited)
				}
				if !strings.Contains(why, "(launch 2) has left no capture") || strings.Contains(why, "still being kept") {
					t.Errorf("the relaunch says %q, want the job it follows having left no capture, and no keep "+
						"waited on", why)
				}
			})
		})
	}
	for name, lands := range map[string]bool{
		"an earlier job's save refused while the relaunch waits, the turn having kept none": false,
		"an earlier job's save kept while the relaunch waits, the turn having kept none":    true,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, logbook, _ := conversingRig(t)
				rig.coordinator.relaunchWait = 10 * time.Second
				read := frozenReadOf(logbook, logbook.endedTurn(t, "b1", "s-base", "the base's transcript"))
				ctx := t.Context()
				if _, err := rig.coordinator.Launch(ctx, rig.manager, continuingBase("t2", read)); err != nil {
					t.Fatalf("the first launch: %v", err)
				}
				rig.suspend("t2")
				save, _ := overtakenSave(t, rig, "t2", "", 0, CaptureRefused)
				if _, err := rig.pending.BeginLaunch(ctx, PendingRun{TurnID: "t2", AgentHandle: "swe",
					AgentID: sweID, Role: "SWE", Harness: "claude-code"}, rigLease); err != nil {
					t.Fatalf("the second job: %v", err)
				}
				outcome := CaptureRefused
				if lands {
					outcome = CaptureLanded
				}
				settled := make(chan struct{})
				go func() {
					defer close(settled)
					time.Sleep(20 * time.Millisecond)
					_, _ = rig.pending.StampCapture(context.Background(), "t2", comeOut(save, outcome))
				}()
				asked := time.Now()
				continued, why := rig.coordinator.ContinuedCapture(ctx, "t2", RestorerUnder(rig.runner, ""))
				waited := time.Since(asked)
				<-settled
				if waited < 20*time.Millisecond {
					t.Fatalf("the relaunch decided after %s (%q), before the earlier job's save it depends on "+
						"was %s", waited, why, outcome)
				}
				switch {
				case !lands && (why != "" || continued.Base == nil || continued.Base.Turn != "b1"):
					t.Errorf("the relaunch continues base %+v (%q), want the base the turn's row still holds, the "+
						"save refused", continued.Base, why)
				case lands && (continued.Base != nil || !strings.Contains(why, "kept a session of its own")):
					t.Errorf("the relaunch continues base %+v (%q), want none, the turn having kept the save",
						continued.Base, why)
				}
			})
		})
	}
	t.Run("an earlier job's save still being kept past the wait, the turn having kept none", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, _ := keepingRig(t)
			rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{}}
			rig.coordinator.relaunchWait = 50 * time.Millisecond
			rig.launchKept(t, "t1")
			save, _ := overtakenSave(t, rig, "t1", "", 0, CaptureRefused)
			if _, err := rig.pending.BeginLaunch(t.Context(), PendingRun{TurnID: "t1", AgentHandle: "swe",
				AgentID: sweID, Role: "SWE", Harness: "claude-code"}, rigLease); err != nil {
				t.Fatalf("the second job: %v", err)
			}
			_, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, ""))
			earlier := fmt.Sprintf("a capture of an earlier job of the turn (launch %d, capture %d) was still "+
				"being kept", save.LaunchSeq, save.CaptureSeq)
			if !strings.Contains(why, "(launch 2) has left no capture") || !strings.Contains(why, earlier) {
				t.Errorf("the relaunch says %q, want the job it follows having left no capture, and the earlier "+
					"job's keep it waited on named as that: %q", why, earlier)
			}
		})
	})
}
