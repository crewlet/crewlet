package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
)

// passedOverLines are what a relaunch's verdict says it went past
// ([Coordinator.passedOver]): its detail, where it continues an older capture
// of its job, said as [executorkeep.LossCaptureLate] where a newer one may
// still be kept, and the newer one's own loss where it will not be.
func passedOverLines(continued ContinuedSession) []string {
	n := continued.NotContinued
	if n == nil || n.Instead != executorkeep.InsteadOlder || n.Older == nil ||
		*n.Older != (executorkeep.CapturePlace{LaunchSeq: continued.Capture.LaunchSeq,
			CaptureSeq: continued.Capture.CaptureSeq}) {
		return nil
	}
	return []string{string(n.Loss) + ": " + n.Detail}
}

// keptWhileWorking stamps on t1's row, landed and kept, a capture of its
// running job taken while the job still worked, stating its save point, and
// puts the turn's row on the executor log naming it: the job's newest kept
// capture, beside which a case then stamps a newer one. It answers the capture
// and the restore that continues it.
func keptWhileWorking(t *testing.T, rig *coordRig) (SessionCapture, *keptRestore) {
	t.Helper()
	run := rig.get("t1")
	kept := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID, State: CaptureLanded,
		Interrupted: true, Through: new(0), At: rig.now, Harness: "claude-code", HarnessVersion: "2.1.310",
		SessionID: "s-1", ArchiveFormat: FakeArchiveFormat, Object: objstore.KeyAt(rig.now).String(),
		ObjectHash: strings.Repeat("a", 64), ObjectSize: 14, ContentKey: "k1", ContextTokens: 40_000,
		NextRequestTokens: 43_000, NextRequestOlderTokens: 32_000}
	if took, err := rig.pending.StampCapture(t.Context(), "t1", kept); err != nil || !took {
		t.Fatalf("stamping the capture taken as the job worked = %v, %v", took, err)
	}
	if got := rig.get("t1"); got.Kept == nil || !got.Kept.samePlace(kept) {
		t.Fatalf("the premise: the row keeps %+v, want the capture taken as the job worked", got.Kept)
	}
	restore := &keptRestore{rows: map[string]executorkeep.TurnRow{"t1": keptRow(t, kept)},
		archives: map[string][]byte{kept.Object: []byte("the transcript")}}
	rig.coordinator.restore = restore
	return kept, restore
}

// launchAfter is the launch that follows a relaunch's probe asked to continue
// want, as t1's record now opens it (one launch past the job it follows), in
// box: the job's own where reused, offered offered, or another and none.
func launchAfter(t *testing.T, rig *coordRig, want SessionCapture, reused bool,
	offered *Continuation) continuedLaunch {

	t.Helper()
	opened := rig.get("t1")
	opened.LaunchSeq++
	req := continuingKept(want)
	req.Continue = offered
	got := rig.coordinator.continueSession(t.Context(), req, opened, NewFakeSandbox("box-2"), rig.runner,
		reused, false)
	t.Cleanup(got.release)
	return got
}

// A RELAUNCH CONTINUES THE NEWEST KEPT CAPTURE OF ITS JOB PAST ONE NOT KEPT:
// past a newest capture refused, unfinished or unanswered past its window, it
// continues the older one the job kept, at its own next-request estimate, told
// every call after it, its verdict naming the newer one passed over, its loss
// and why. One whose taker stopped is settled from the turn's row first, and
// continued where the log took it. A job that kept none before its newest
// starts without one; its copy is answered as that newest measured and
// estimated it. In a synctest bubble.
//
// Mutation: continue only the job's newest capture, drop the verdict that names
// the one passed over, or answer the copy's estimate off the kept capture, and
// a case goes red.
func TestARelaunchContinuesTheNewestKeptCaptureOfItsJobPastOneNotKept(t *testing.T) {
	for _, c := range []struct {
		name string
		// newer stamps the job's capture after kept on the rig's row,
		// answering it.
		newer func(t *testing.T, rig *coordRig, restore *keptRestore, store *takerStore,
			kept SessionCapture) SessionCapture
		// landed is the newer one settled landed and continued; else line is
		// what the verdict passing it over says of it.
		landed bool
		line   string
	}{
		{name: "refused", line: "(launch 1, capture 2) was not kept: refused: past the seat's budget",
			newer: func(t *testing.T, rig *coordRig, _ *keptRestore, _ *takerStore, kept SessionCapture) SessionCapture {
				refused := kept
				refused.CaptureSeq, refused.State, refused.Final = 2, CaptureRefused, true
				refused.Reason, refused.ContextTokens = "past the seat's budget", 61_000
				refused.NextRequestTokens, refused.NextRequestOlderTokens = 64_000, 48_000
				if took, err := rig.pending.StampCapture(t.Context(), "t1", refused); err != nil || !took {
					t.Fatalf("stamping the refused capture = %v, %v", took, err)
				}
				return refused
			}},
		{name: "begun with no taker, past what a capture can take",
			line: "(launch 1, capture 2) was not kept: it was still being kept, or was begun and never finished",
			newer: func(t *testing.T, rig *coordRig, _ *keptRestore, _ *takerStore, kept SessionCapture) SessionCapture {
				begun := takenBy(t, rig, kept, "", 0)
				rig.now = rig.now.Add(SessionCaptureBudget)
				return begun
			}},
		{name: "begun under a lapsed lease, settled refused",
			line: "(launch 1, capture 2) was not kept: refused: its taker node-a stopped before it was kept",
			newer: func(t *testing.T, rig *coordRig, _ *keptRestore, store *takerStore, kept SessionCapture) SessionCapture {
				begun := takenBy(t, rig, kept, takingProcess, claimTaker(t, store, takingProcess).Epoch)
				lapse(store)
				return begun
			}},
		{name: "unanswered past the window a record may name it in",
			line: "(launch 1, capture 2) was not kept: refused: " + staleUpload,
			newer: func(t *testing.T, rig *coordRig, _ *keptRestore, _ *takerStore, kept SessionCapture) SessionCapture {
				// THE LOG DECIDES THE WINDOW, on its own clock: the relaunch asks
				// the record again whatever its key's age, and the log refuses it.
				if writes, ok := rig.coordinator.sessions.(*sessionWrites); ok {
					rig.coordinator.sessions = windowedLog{sessionWrites: writes,
						now: func() time.Time { return rig.now }}
				}
				begun := takenBy(t, rig, kept, "", 0)
				unanswered := begun
				unanswered.State = CaptureUnresolved
				unanswered.Object = objstore.KeyAt(rig.now.Add(-objstore.RecordWithin - 2*objstore.AssumedClockSkew -
					time.Minute)).String()
				if took, err := rig.pending.StampCapture(t.Context(), "t1", unanswered); err != nil || !took {
					t.Fatalf("stamping the unanswered capture = %v, %v", took, err)
				}
				return unanswered
			}},
		{name: "begun under a lapsed lease, its record on the log", landed: true,
			newer: func(t *testing.T, rig *coordRig, restore *keptRestore, store *takerStore,
				kept SessionCapture) SessionCapture {
				begun := takenBy(t, rig, kept, takingProcess, claimTaker(t, store, takingProcess).Epoch)
				onTheLog(restore, landedRow(t, begun))
				lapse(store)
				return begun
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			inBubble(t, func(t *testing.T) {
				store := newTakerStore()
				rig, _, _ := keepingRigWith(t, taking(thisProcess, store))
				rig.runner.Carries = true
				rig.coordinator.relaunchWait = 2 * time.Second
				rig.launchKept(t, "t1")
				kept, restore := keptWhileWorking(t, rig)
				newer := c.newer(t, rig, restore, store, kept)

				continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, ""))
				lines := passedOverLines(continued)
				if c.landed {
					if why != "" || !continued.Capture.samePlace(newer) || continued.Capture.State != CaptureLanded {
						t.Fatalf("the relaunch continues %+v (%q), want the newer capture the log took", continued,
							why)
					}
					if len(lines) != 0 {
						t.Errorf("a relaunch that continued the job's newest capture passed over %q", lines)
					}
					return
				}
				if why != "" || !continued.Capture.samePlace(kept) || continued.Capture.ContextTokens != 40_000 {
					t.Fatalf("the relaunch continues %+v (%q), want the capture the job kept before its newest, "+
						"at its own 40000 tokens", continued, why)
				}
				if c.name == "refused" && (continued.CopyContext != 61_000 ||
					continued.CopyNextRequest != NextRequest{Tokens: 64_000, OlderTokens: 48_000} ||
					continued.Capture.NextRequest() != kept.NextRequest()) {
					t.Errorf("the answer says the job's copy carried %d tokens and sends %+v next, the kept capture "+
						"%+v; want the 61000 and 64000 its newest capture measured, the kept one its own %+v",
						continued.CopyContext, continued.CopyNextRequest, continued.Capture.NextRequest(),
						kept.NextRequest())
				}
				// THE VERDICT NAMES THE OLDER CAPTURE CONTINUED ([passedOverLines]).
				if len(lines) != 1 || !strings.Contains(lines[0], c.line) {
					t.Fatalf("said %q, want one verdict saying %q of the capture continued, (1, 1)", lines, c.line)
				}
			})
		})
	}
	t.Run("the job kept none before its newest", func(t *testing.T) {
		inBubble(t, func(t *testing.T) {
			rig, _, _ := keepingRig(t)
			rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{}}
			run := rig.launchKept(t, "t1")
			refused := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID,
				State: CaptureRefused, Reason: "past the seat's budget", At: rig.now, Harness: "claude-code",
				SessionID: "s-1", ContextTokens: 61_000}
			if took, err := rig.pending.StampCapture(t.Context(), "t1", refused); err != nil || !took {
				t.Fatalf("stamping the refused capture = %v, %v", took, err)
			}
			continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, ""))
			if !strings.Contains(why, "was not kept: refused: past the seat's budget, and the job kept none "+
				"before it") || continued.Capture.CaptureSeq != 0 || continued.Capture.ContextTokens != 0 {
				t.Fatalf("the relaunch continues %+v (%q), want none, said", continued, why)
			}
			if continued.CopyContext != 61_000 {
				t.Errorf("the answer says the job's copy carried %d tokens, want the 61000 its newest capture "+
					"measured, for the floor of a launch that continues it in its box", continued.CopyContext)
			}
			if lines := passedOverLines(continued); len(lines) != 0 {
				t.Errorf("a relaunch that continued nothing passed over %q", lines)
			}
		})
	})
}

// A RELAUNCH CONTINUES THE NEWEST KEPT CAPTURE BESIDE A NEWER ONE STILL IN
// FLIGHT: it waits briefly for the newer one (or a save), asks its record again
// first, and continues it where it lands; past the wait it continues the older
// one and says it did, `capture_late`, naming the newer it left in flight.
// Where the newer lands between the probe and the launch, the launch's own
// check refuses the older and the run starts fresh or continues its box's
// copy. An ask that can land is asked in time, and a keep under way beside an
// earlier job's kept capture is said still being kept. In a synctest bubble.
//
// Mutation: refuse the older beside one in flight, skip the wait, drop the
// launch's check or the verdict, ask only with nothing else in flight, skip the
// ask on an early end, or read an earlier job's capture as the followed job's.
func TestARelaunchContinuesTheNewestKeptCaptureBesideANewerOneStillInFlight(t *testing.T) {
	const wait = 2 * time.Second
	// continuesKept fails t unless p's relaunch continued the capture its job
	// kept, having waited its whole wait, and logged one line naming each of
	// said, the newer ones it went past.
	continuesKept := func(t *testing.T, p probed, said ...string) {
		t.Helper()
		if p.why != "" || !p.continued.Capture.samePlace(p.kept) {
			t.Fatalf("the relaunch continues %+v (%q), want the capture its job kept", p.continued, p.why)
		}
		if p.waited < wait {
			t.Errorf("the relaunch decided %s after it asked, want its whole wait of %s for the newer one", p.waited,
				wait)
		}
		if len(p.lines) != 1 {
			t.Fatalf("logged %q, want one line naming what the relaunch went past", p.lines)
		}
		for _, s := range said {
			if !strings.Contains(p.lines[0], s) {
				t.Errorf("the line %q does not say %q", p.lines[0], s)
			}
		}
	}
	// thenLaunched fails t unless the launch that follows, in another box,
	// restores the capture the job kept.
	thenLaunched := func(t *testing.T, p probed) {
		t.Helper()
		got := launchAfter(t, p.rig, p.kept, false, nil)
		if got.kind != BriefContinuedNewBox || got.session != "s-1" || got.carried == nil {
			t.Fatalf("the launch that follows hands a %s brief on session %q carrying %+v, want the kept "+
				"capture restored", got.kind, got.session, got.carried)
		}
	}
	stillKept := fmt.Sprintf("and the relaunch did not wait past %s for it", wait)

	t.Run("a newer capture still being kept, its taker's lease held", func(t *testing.T) {
		inBubble(t, func(t *testing.T) {
			var newer SessionCapture
			p := probeBeside(t, wait, func(p *probed, epoch int64) { newer = takenBy(t, p.rig, p.kept, takingProcess, epoch) })
			continuesKept(t, p, "the newest capture of the job the relaunch follows (launch 1, capture 2) may "+
				"still be kept: it is still being kept", stillKept)
			if got := p.rig.get("t1").Capture; got == nil || !got.sameTake(newer) || got.State != CaptureBegun {
				t.Errorf("the row holds %+v, want the newer capture still being kept", got)
			}
			if read := p.restore.askedFor(); len(read) != 2 {
				t.Errorf("the turn's row was read %v, want once past the wait and once to decide", read)
			}
			thenLaunched(t, p)
		})
	})
	t.Run("a newer capture whose record is still unanswered when asked", func(t *testing.T) {
		inBubble(t, func(t *testing.T) {
			var asked int
			p := probeBeside(t, wait, func(p *probed, _ int64) {
				p.writes.answer(errors.New("estate: the write was sent to a data node that did not answer"))
				unanswered := takenBy(t, p.rig, p.kept, "", 0)
				unanswered.State, unanswered.Object = CaptureUnresolved, objstore.KeyAt(p.rig.now).String()
				if took, err := p.rig.pending.StampCapture(t.Context(), "t1", unanswered); err != nil || !took {
					t.Fatalf("stamping the unanswered capture = %v, %v", took, err)
				}
				asked = len(p.writes.captured())
			})
			if len(p.writes.captured()) <= asked {
				t.Errorf("the log was asked nothing more by the relaunch, want the newer capture's record asked")
			}
			if p.why != "" || !p.continued.Capture.samePlace(p.kept) {
				t.Fatalf("the relaunch continues %+v (%q), want the capture its job kept", p.continued, p.why)
			}
			if len(p.lines) != 1 || !strings.Contains(p.lines[0], "(launch 1, capture 2) may still be kept: its "+
				"record was sent and not yet answered") {
				t.Fatalf("logged %q, want one line naming the newer capture awaiting its answer", p.lines)
			}
			if got := p.rig.get("t1").Capture; got == nil || got.State != CaptureUnresolved {
				t.Errorf("the row holds %+v, want the newer capture still awaiting its answer", got)
			}
			thenLaunched(t, p)
		})
	})
	t.Run("a save still being kept beside a final take refused", func(t *testing.T) {
		inBubble(t, func(t *testing.T) {
			var save SessionCapture
			p := probeBeside(t, wait, func(p *probed, epoch int64) {
				save, _ = overtakenSave(t, p.rig, "t1", takingProcess, epoch, CaptureRefused)
			})
			continuesKept(t, p, "the newest capture of the job the relaunch follows (launch 1, capture 3) was "+
				"not kept: refused", "its save under way (launch 1, capture 2) may still be kept: it is still "+
				"being kept", stillKept)
			if got := p.rig.get("t1").Saving; got == nil || !got.sameTake(save) || got.State != CaptureBegun {
				t.Errorf("the row's save under way is %+v, want it still being kept", got)
			}
			thenLaunched(t, p)
		})
	})
	t.Run("a newer capture refused, a read standing by for it", func(t *testing.T) {
		inBubble(t, func(t *testing.T) {
			p := probeBeside(t, wait, func(p *probed, epoch int64) {
				run := p.rig.get("t1")
				refused := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 2, Launch: run.LaunchID,
					State: CaptureRefused, Reason: "the session could not be uploaded", Final: true,
					At: p.rig.now, Harness: "claude-code", SessionID: "s-1"}
				if took, err := p.rig.pending.StampCapture(t.Context(), "t1", refused); err != nil || !took {
					t.Fatalf("stamping the refused capture = %v, %v", took, err)
				}
				standby := refused
				standby.State, standby.Reason, standby.At = CaptureBegun, "", refused.At.Add(time.Second)
				standby.Taker, standby.TakerEpoch = takingProcess, epoch
				if stood, err := p.rig.pending.StandBy(t.Context(), "t1", standby); err != nil || !stood {
					t.Fatalf("standing by = %v, %v", stood, err)
				}
			})
			continuesKept(t, p, "(launch 1, capture 2) may still be kept: a read stands by to keep it in the "+
				"next place", stillKept)
			if got := p.rig.get("t1").Capture; got == nil || got.StandbyAt.IsZero() {
				t.Errorf("the row holds %+v, want the read still standing by", got)
			}
			thenLaunched(t, p)
		})
	})
	t.Run("a newer capture that lands while the relaunch waits", func(t *testing.T) {
		inBubble(t, func(t *testing.T) {
			var landed SessionCapture
			p := probeBeside(t, wait, func(p *probed, epoch int64) {
				begun := takenBy(t, p.rig, p.kept, takingProcess, epoch)
				landed = comeOut(begun, CaptureLanded)
				row := keptRow(t, landed)
				go func() {
					time.Sleep(20 * time.Millisecond)
					onTheLog(p.restore, row)
					_, _ = p.rig.pending.StampCapture(context.Background(), "t1", landed)
				}()
			})
			if p.why != "" || !p.continued.Capture.samePlace(landed) {
				t.Fatalf("the relaunch continues %+v (%q), want the newer capture that landed while it waited",
					p.continued, p.why)
			}
			if len(p.lines) != 0 {
				t.Errorf("a relaunch that continued the job's newest capture passed over %q", p.lines)
			}
		})
	})
	t.Run("a newer capture whose record goes unanswered while the relaunch waits", func(t *testing.T) {
		inBubble(t, func(t *testing.T) {
			var landed SessionCapture
			p := probeBeside(t, wait, func(p *probed, epoch int64) {
				// A TICK SHORTER THAN THE WAIT, so the wait reads the keep's
				// end before it runs out.
				p.rig.coordinator.captureTick = wait / 20
				begun := takenBy(t, p.rig, p.kept, takingProcess, epoch)
				landed = comeOut(begun, CaptureLanded)
				row, unanswered := keptRow(t, landed), comeOut(begun, CaptureUnresolved)
				go func() {
					time.Sleep(20 * time.Millisecond)
					onTheLog(p.restore, row)
					_, _ = p.rig.pending.StampCapture(context.Background(), "t1", unanswered)
				}()
			})
			if p.why != "" || !p.continued.Capture.samePlace(landed) || p.continued.Capture.State != CaptureLanded {
				t.Fatalf("the relaunch continues %+v (%q), want the newer capture its ask landed once the wait "+
					"ended", p.continued, p.why)
			}
			records := p.writes.captured()
			if len(records) == 0 || records[len(records)-1].err != nil ||
				records[len(records)-1].Stamp.CaptureSeq != landed.CaptureSeq {
				t.Errorf("the log was asked %+v, want the newer capture's record asked again, and landed", records)
			}
			if p.waited >= wait {
				t.Errorf("the relaunch decided %s after it asked, want it once the keep ended, before its whole "+
					"wait of %s", p.waited, wait)
			}
			if len(p.lines) != 0 {
				t.Errorf("a relaunch that continued the job's newest capture passed over %q", p.lines)
			}
		})
	})
	t.Run("the re-check race", func(t *testing.T) {
		for _, c := range []struct {
			name string
			// land makes newer the job's newest kept capture after the
			// probe: on the record (and so on the log), or on the log alone,
			// its record taken ahead of its stamp.
			land    func(t *testing.T, p probed, newer SessionCapture)
			reused  bool
			offered *Continuation
			kind    BriefKind
			session string
			reason  string
			outcome string
		}{
			{name: "landed on the record, in another box", land: landedOnTheRecord, kind: BriefFresh,
				reason: "not the one its launch was asked to continue", outcome: startsItsOwn},
			{name: "landed on the record, in the job's own box offered its copy", land: landedOnTheRecord,
				reused: true, offered: &Continuation{SessionID: "s-1", Brief: "carry the box's copy on"},
				kind: BriefContinued, session: "s-1", reason: "not the one its launch was asked to continue",
				outcome: continuesBoxCopy},
			{name: "on the log ahead of its stamp, in another box", kind: BriefFresh,
				land: func(t *testing.T, p probed, newer SessionCapture) {
					onTheLog(p.restore, keptRow(t, comeOut(newer, CaptureLanded)))
				},
				reason: "the executor log names the turn's capture (launch 1, capture 2)", outcome: startsItsOwn},
		} {
			t.Run(c.name, func(t *testing.T) {
				inBubble(t, func(t *testing.T) {
					var newer SessionCapture
					p := probeBeside(t, wait, func(p *probed, epoch int64) {
						newer = takenBy(t, p.rig, p.kept, takingProcess, epoch)
					})
					if p.why != "" || !p.continued.Capture.samePlace(p.kept) {
						t.Fatalf("the premise: the probe continues %+v (%q), want the capture the job kept",
							p.continued, p.why)
					}
					c.land(t, p, newer)
					got := launchAfter(t, p.rig, p.kept, c.reused, c.offered)
					if got.kind != c.kind || got.session != c.session || got.carried != nil {
						t.Fatalf("the launch hands a %s brief on session %q carrying %+v, want a %s brief on %q "+
							"carrying nothing", got.kind, got.session, got.carried, c.kind, c.session)
					}
					switch said := got.notContinued; {
					case c.outcome == continuesBoxCopy && said != nil:
						t.Errorf("the launch going on in its box's copy says %+v, want nothing lost", said)
					case c.outcome == startsItsOwn && (said == nil || said.Instead != executorkeep.InsteadFresh ||
						!strings.Contains(said.Detail, c.reason)):
						t.Errorf("the launch says %+v, want the kept capture refused saying %q, the run fresh",
							said, c.reason)
					}
				})
			})
		}
	})
	t.Run("the job kept none before the capture still being kept", func(t *testing.T) {
		inBubble(t, func(t *testing.T) {
			store := newTakerStore()
			rig, _, _ := keepingRigWith(t, taking(thisProcess, store))
			rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{}}
			rig.coordinator.relaunchWait = wait
			run := rig.launchKept(t, "t1")
			before := SessionCapture{LaunchSeq: run.LaunchSeq, Launch: run.LaunchID, Interrupted: true,
				Through: new(0), Harness: "claude-code", SessionID: "s-1", ArchiveFormat: FakeArchiveFormat}
			takenBy(t, rig, before, takingProcess, claimTaker(t, store, takingProcess).Epoch)
			continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, ""))
			if !strings.Contains(why, "the capture of the job the relaunch follows (launch 1, capture 1) was "+
				"still being kept") || continued.Capture.CaptureSeq != 0 {
				t.Fatalf("the relaunch continues %+v (%q), want none, naming the capture still being kept",
					continued, why)
			}
			if lines := passedOverLines(continued); len(lines) != 0 {
				t.Errorf("a relaunch that continued nothing passed over %q", lines)
			}
		})
	})
	t.Run("the job kept none before the capture still being kept, an earlier job of the turn kept one",
		func(t *testing.T) {
			inBubble(t, func(t *testing.T) {
				store := newTakerStore()
				rig, _, _ := keepingRigWith(t, taking(thisProcess, store))
				rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{}}
				rig.coordinator.relaunchWait = wait
				rig.launchKept(t, "t1")
				earlier := keptSave(t, rig, "t1")
				if _, err := rig.pending.BeginLaunch(t.Context(), PendingRun{TurnID: "t1", AgentHandle: "swe",
					AgentID: sweID, Role: "SWE", Harness: "claude-code"}, rigLease); err != nil {
					t.Fatalf("the second job: %v", err)
				}
				run := rig.get("t1")
				if run.LaunchSeq != earlier.LaunchSeq+1 || run.Kept == nil || !run.Kept.samePlace(earlier) {
					t.Fatalf("the premise: launch %d keeps %+v, want the next launch beside the earlier job's "+
						"kept capture", run.LaunchSeq, run.Kept)
				}
				before := SessionCapture{LaunchSeq: run.LaunchSeq, Launch: run.LaunchID, Interrupted: true,
					Through: new(0), Harness: "claude-code", SessionID: "s-1", ArchiveFormat: FakeArchiveFormat}
				takenBy(t, rig, before, takingProcess, claimTaker(t, store, takingProcess).Epoch)
				continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, ""))
				if want := fmt.Sprintf("the capture of the job the relaunch follows (launch %d, capture 1) was "+
					"still being kept %s after the relaunch asked for it", run.LaunchSeq, wait); !strings.Contains(why,
					want) || continued.Capture.CaptureSeq != 0 {
					t.Fatalf("the relaunch continues %+v (%q), want none, saying %q", continued, why, want)
				}
				if lines := passedOverLines(continued); len(lines) != 0 {
					t.Errorf("a relaunch that continued nothing passed over %q", lines)
				}
			})
		})
	t.Run("an unanswered save beside a final take still being kept", func(t *testing.T) {
		inBubble(t, func(t *testing.T) {
			var save SessionCapture
			p := probeBeside(t, wait, func(p *probed, epoch int64) {
				save, _ = overtakenSave(t, p.rig, "t1", takingProcess, epoch, CaptureBegun)
				stampOut(t, p.rig, "t1", save, CaptureUnresolved)
				onTheLog(p.restore, keptRow(t, comeOut(save, CaptureLanded)))
			})
			if p.why != "" || !p.continued.Capture.sameTake(save) || p.continued.Capture.State != CaptureLanded {
				t.Fatalf("the relaunch continues %+v (%q), want the save its ask landed, not the capture kept "+
					"before it", p.continued, p.why)
			}
			if len(p.lines) != 1 || !strings.Contains(p.lines[0], "(launch 1, capture 3) may still be kept: it is "+
				"still being kept") {
				t.Errorf("logged %q, want one line naming the final take still being kept", p.lines)
			}
		})
	})
}

// probed is a relaunch's probe of t1 as a case read it: the rig, its record
// writes and its restore, the capture the job kept, what the probe continues
// and why not, how long it took, and the lines naming a capture it went past
// ([passedOverLines]).
type probed struct {
	rig       *coordRig
	writes    *sessionWrites
	restore   *keptRestore
	kept      SessionCapture
	continued ContinuedSession
	why       string
	waited    time.Duration
	lines     []string
}

// probeBeside builds t1 with the capture its running job kept and a taker
// lease node-a holds, its relaunch waiting at most wait; has stamp put beside
// that capture what the case stamps, handed node-a's epoch; and probes the
// relaunch as its caller does.
//
// Built on [keepingRig], and so only in a bubble ([inBubble]).
//
//fixedwait:bubble
func probeBeside(t *testing.T, wait time.Duration, stamp func(p *probed, epoch int64)) probed {
	t.Helper()
	store := newTakerStore()
	rig, writes, _ := keepingRigWith(t, taking(thisProcess, store))
	rig.runner.Carries = true
	rig.coordinator.relaunchWait = wait
	rig.launchKept(t, "t1")
	kept, restore := keptWhileWorking(t, rig)
	p := probed{rig: rig, writes: writes, restore: restore, kept: kept}
	stamp(&p, claimTaker(t, store, takingProcess).Epoch)
	started := time.Now()
	p.continued, p.why = rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, ""))
	p.waited = time.Since(started)
	p.lines = passedOverLines(p.continued)
	return p
}

// landedOnTheRecord lands newer as its keep would: its record on the log, the
// turn's row naming it and holding its archive, and its stamp landed on the
// run's record, which then keeps it.
func landedOnTheRecord(t *testing.T, p probed, newer SessionCapture) {
	t.Helper()
	landed := comeOut(newer, CaptureLanded)
	p.restore.mu.Lock()
	p.restore.archives[landed.Object] = []byte("the newer transcript")
	p.restore.mu.Unlock()
	onTheLog(p.restore, keptRow(t, landed))
	stampOut(t, p.rig, "t1", newer, CaptureLanded)
	if kept := p.rig.get("t1").Kept; kept == nil || !kept.samePlace(newer) {
		t.Fatalf("the premise: the row keeps %+v, want the newer capture landed", kept)
	}
}
