package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"sync"
	"testing"
	"time"
)

// HOW A COLLECTION RECORDS THE WAY ITS JOB'S HARNESS ENDED. The read that
// collects a job writes on the job's own record whether its harness ended its
// own run — over, gone cleanly by itself, its script packing it — or was cut
// off, by the line its capture is placed by, whether or not it takes or keeps a
// capture of the session: a relaunch that carries the round on in the job's box
// continues the copy that box holds, which no capture describes, and a copy
// whose harness was cut off may lack calls it made and had answered. Cut off
// are a harness killed, its script packing after it; one told to stop by the
// end its turn's cap asked of the job; a job its cap ended before its harness
// started; and a job lost with its engine after it asked a person, read as its
// loss is carried on and parked on its question for the answer that carries it
// on in its box. And the resume that follows on the claim — a round a replaced
// sign-in carries on, in the same frame — reads it off the claim, which was
// read before the record was written. CUT OFF ONCE, CUT OFF FOR THE JOB: a read
// that finds the harness ended its own run, of a job an earlier read found cut
// off — a collection made again — leaves the record and the claim cut off.
//
// Mutation: write no record, record every job's harness as ending its own run,
// record it only where the job's session is kept, leave the claim's copy as it
// was read, or set the claim's copy to the read's end over a cut-off one, and a
// case goes red.
func TestACollectionRecordsHowItsJobsHarnessEnded(t *testing.T) {
	t.Parallel()
	archive := SessionArchive{Harness: "claude-code", SessionID: "s-1", Format: 1, Version: "2.1.310",
		Size: 14, Path: "/home/user/.crewlet/session-capture"}
	for _, tc := range []struct {
		name string
		// keeps collects in a company that keeps sessions, its capture taken
		// and kept; else in one that keeps none, where nothing is taken.
		keeps bool
		// capped launches the turn with its wall-clock cap passed, so its
		// collection ends the job first.
		capped bool
		// finished is what a job reports that ended by itself, and ended what
		// one reports once its end ended it; nil for neither.
		finished, ended func() Result
		// lostAsking is a job that asked a person and was lost with its
		// engine: its loss's collection parks it, and no turn resumes.
		lostAsking bool
		// before is what an earlier read of the job recorded, "" for none.
		before HarnessEnd
		want   HarnessEnd
	}{
		{name: "a job whose harness ended its own run, in a company that keeps no session",
			finished: func() Result {
				return Result{Success: true, Text: "done", SessionID: "s-1", Captured: &archive}
			},
			want: HarnessEndOwnRun},
		{name: "a job whose harness ended its own run, its session kept", keeps: true,
			finished: func() Result {
				return Result{Success: true, Text: "done", SessionID: "s-1", Captured: &archive}
			},
			want: HarnessEndOwnRun},
		{name: "a job whose harness was killed, its script packing after it",
			finished: func() Result {
				return Result{Text: "killed", SessionID: "s-1", Captured: &archive, HarnessCutOff: true}
			},
			want: HarnessEndCutOff},
		{name: "a job its turn's cap ended, its harness told to stop", capped: true,
			ended: func() Result { return Result{SessionID: "s-1", Captured: &archive, JobEnd: JobEndAsked} },
			want:  HarnessEndCutOff},
		{name: "a job its turn's cap ended before its harness started", capped: true,
			ended: func() Result { return Result{JobEnd: JobEndBefore} },
			want:  HarnessEndCutOff},
		{name: "a job lost with its engine after it asked, parked on its question", lostAsking: true,
			want: HarnessEndCutOff},
		{name: "a job an earlier read found cut off, read again ending its own run",
			before: HarnessEndCutOff,
			finished: func() Result {
				return Result{Success: true, Text: "done", SessionID: "s-1", Captured: &archive}
			},
			want: HarnessEndCutOff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				var rig *coordRig
				if tc.keeps {
					rig, _, _ = keepingRig(t)
				} else {
					rig = newCoordRig(t)
				}
				if tc.lostAsking {
					asked := whichBranch()
					if err := askLoseAndClaim(t, rig, &asked, Result{}); err != nil {
						t.Fatalf("OnCompleted: %v", err)
					}
					if got := rig.get("t1"); got.Status != StatusAwaiting || got.Launch.HarnessEnd != tc.want {
						t.Fatalf("the lost job is %s and its record says %q, want it parked on its question, %q",
							got.Status, got.Launch.HarnessEnd, tc.want)
					}
					return
				}
				deadline := time.Time{}
				if tc.capped {
					deadline = rig.now.Add(-time.Minute)
				}
				rig.launchUntil("t1", deadline)
				if tc.before != "" {
					if _, err := rig.pending.MarkHarnessEnd(t.Context(), "t1", rig.get("t1").LaunchID,
						EndRead{End: tc.before}); err != nil {
						t.Fatalf("MarkHarnessEnd: %v", err)
					}
				}
				rig.coordinator.countRun("swe", StatusRunning)
				if tc.keeps {
					rig.handOver(t, "t1", HandedSecrets{Values: []string{handedToken}})
				}
				switch {
				case tc.finished != nil:
					rig.runner.Finish(tc.finished())
				case tc.ended != nil:
					rig.runner.EndsAs(tc.ended())
				}
				// COLLECTED, and read as the resume on the claim runs: the job's
				// record as the store holds it, and the claim the resume was handed.
				// The resume then refuses, so a row its turn's cap has not passed
				// stays for the case; one past its cap is ended.
				type read struct{ stored, claimed HarnessEnd }
				var (
					mu    sync.Mutex
					reads []read
				)
				rig.resumer.failWith(errors.New("held for the case to read the row"))
				rig.resumer.during = func(ctx context.Context, run PendingRun) {
					stored := rig.getIn(ctx, "t1").Launch.HarnessEnd
					mu.Lock()
					defer mu.Unlock()
					reads = append(reads, read{stored: stored, claimed: run.Launch.HarnessEnd})
				}
				payload, ev := rig.completion("t1")
				if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
					t.Logf("OnCompleted: %v", err)
				}
				rig.awaitKeeps()
				mu.Lock()
				defer mu.Unlock()
				if len(reads) != 1 {
					t.Fatalf("the turn was resumed %d times, want once", len(reads))
				}
				if reads[0].stored != tc.want {
					t.Errorf("the job's record says its harness ended %q, want %q", reads[0].stored, tc.want)
				}
				if reads[0].claimed != tc.want {
					t.Errorf("the resume on the claim read %q, want the end the collection recorded, %q",
						reads[0].claimed, tc.want)
				}
				if tc.keeps {
					if kept := rig.get("t1").Kept; kept == nil || kept.State != CaptureLanded {
						t.Errorf("the row keeps %+v, want the collection's capture landed beside the record", kept)
					}
				}
			})
		})
	}
}

// AN OLDER BUILD'S WRITE CARRIES A JOB'S HARNESS END. A fleet mid-upgrade
// rewrites the job's record on a build that does not know the field — a status,
// a lease, a bridged call, the park the answer waits on — and the relaunch the
// answer drives may run on this build: so the end rides the record under its own
// key, and a build that does not declare that key keeps it verbatim and writes it
// back (wirekeys.go), whatever it decoded of the rest.
//
// Mutation: leave the field off the wire, and no older build's record holds a key
// of it; drop the carried keys from the record's encode (marshalCarrying), and
// the end is gone after the older build's write.
func TestAnOlderBuildsWriteCarriesAJobsHarnessEnd(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(LaunchRecord{Model: "m", HarnessEnd: HarnessEndCutOff})
	if err != nil {
		t.Fatal(err)
	}
	// THE OLDER BUILD'S RECORD: every key this one declares but the end's.
	declared := wireKeys(reflect.TypeFor[LaunchRecord]())
	if !declared["harness_end"] {
		t.Fatalf("the job's record declares no key harness_end: %v", declared)
	}
	older := maps.Clone(declared)
	delete(older, "harness_end")
	type plain LaunchRecord
	var read plain
	extra, err := unmarshalCarrying(raw, &read, older)
	if err != nil {
		t.Fatal(err)
	}
	// ITS STRUCT HAS NO SUCH FIELD, so what it writes is the rest and what it
	// carried.
	read.HarnessEnd, read.Model = "", "m2"
	written, err := marshalCarrying(read, older, extra)
	if err != nil {
		t.Fatal(err)
	}
	var back LaunchRecord
	if err := json.Unmarshal(written, &back); err != nil {
		t.Fatal(err)
	}
	if back.HarnessEnd != HarnessEndCutOff || back.Model != "m2" {
		t.Errorf("after the older build's write the job's record reads %q and model %q, want %q beside "+
			"its own write: %s", back.HarnessEnd, back.Model, HarnessEndCutOff, written)
	}
}

// A READ IS NEVER RECORDED OVER A WORD THIS BUILD DOES NOT KNOW. A later build
// may record an end of its own on the job's record, which this build leaves as
// it is: recorded over with an own run, a copy that build found lacking would be
// told nothing; with a cut-off end, that build's word would become this one's.
// So a job's record, read again, takes the read's end only where it held
// nothing or an own run and the read found an end this build knows; a cut-off
// end and a word this build does not know stand; and a read that found nothing
// it knows changes nothing. The coordination store records by the same rule.
//
// Mutation: record a read over a word this build does not know, an own run over
// a cut-off end, or a read this build does not know, and a row or the store's
// case goes red.
func TestAReadIsNeverRecordedOverAWordThisBuildDoesNotKnow(t *testing.T) {
	t.Parallel()
	const later HarnessEnd = "a-later-builds-end"
	for _, tc := range []struct{ held, read, want HarnessEnd }{
		{"", HarnessEndOwnRun, HarnessEndOwnRun},
		{"", HarnessEndCutOff, HarnessEndCutOff},
		{"", "", ""},
		{"", later, ""},
		{HarnessEndOwnRun, HarnessEndOwnRun, HarnessEndOwnRun},
		{HarnessEndOwnRun, HarnessEndCutOff, HarnessEndCutOff},
		{HarnessEndOwnRun, later, HarnessEndOwnRun},
		{HarnessEndCutOff, HarnessEndOwnRun, HarnessEndCutOff},
		{HarnessEndCutOff, "", HarnessEndCutOff},
		{later, HarnessEndOwnRun, later},
		{later, HarnessEndCutOff, later},
		{later, "", later},
	} {
		if got := tc.held.after(tc.read); got != tc.want {
			t.Errorf("a record holding %q, read as %q, holds %q, want %q", tc.held, tc.read, got, tc.want)
		}
	}
	// THE STORE BY THE SAME RULE: a later build's word on the job's record
	// stands against either end this build records.
	rig := newCoordRig(t)
	launch := rig.launch("t1").LaunchID
	if _, _, err := rig.pending.mutate(t.Context(), "t1", func(run *PendingRun) bool {
		run.Launch.HarnessEnd = later
		return true
	}); err != nil {
		t.Fatalf("writing a later build's end: %v", err)
	}
	for _, end := range HarnessEnds {
		if marked, err := rig.pending.MarkHarnessEnd(t.Context(), "t1", launch, EndRead{End: end}); err != nil ||
			marked {
			t.Errorf("MarkHarnessEnd(%q) over a later build's end = %v, %v, want nothing changed", end, marked,
				err)
		}
		if got := rig.get("t1").Launch.HarnessEnd; got != later {
			t.Errorf("after MarkHarnessEnd(%q) the job's record says %q, want the later build's %q", end, got,
				later)
		}
	}
}

// heldMarks is a coordination store that holds every record of how a job's
// harness ended until the context it was asked under ends, as a store slow past
// every deadline does, noting for each how many sessions the runner had taken by
// then and the deadline it was asked under.
type heldMarks struct {
	PendingStore
	taken func() int

	mu    sync.Mutex
	marks []heldMark
}

// heldMark is one record [heldMarks] was asked for.
type heldMark struct {
	takes    int
	deadline time.Time
	bounded  bool
}

func (h *heldMarks) MarkHarnessEnd(ctx context.Context, _, _ string, _ EndRead) (bool, error) {
	deadline, bounded := ctx.Deadline()
	h.mu.Lock()
	h.marks = append(h.marks, heldMark{takes: h.taken(), deadline: deadline, bounded: bounded})
	h.mu.Unlock()
	<-ctx.Done()
	return false, ctx.Err()
}

// asked is every record h was asked for, in order.
func (h *heldMarks) asked() []heldMark {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]heldMark(nil), h.marks...)
}

// A JOB'S HARNESS END IS RECORDED AFTER ITS TAKE, WITHIN THE ROOM THE READ LEAVES
// ITS RECORDS. The read's budget holds no time of its own for the record: asked
// for before the take, a store slow to answer it cut the take's time; past the
// room the read leaves the take's own records (recordsBy), the box's pause and
// the read's answer. So the read that collects a job asks for the record once
// its take is done, under a deadline no later than its own less the pause's
// grace, and a store that never answers it still leaves the box its pause.
//
// IN A SYNCTEST BUBBLE: the take spends twenty seconds of the bubble's clock, so
// the record's own grace, counted from where it is asked, ends past the read's
// room for it, and the store holds the record until its deadline, which the
// bubble's clock reaches exactly.
//
// Mutation: ask for the record before the take, or under a context of its own
// that the read's room does not bound, and a check goes red.
func TestAJobsHarnessEndIsRecordedAfterItsTakeWithinTheReadsRoom(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		var held *heldMarks
		rig, _, _ := keepingRigWith(t, func(o *CoordinatorOptions) {
			held = &heldMarks{PendingStore: o.Pending}
			o.Pending = held
		})
		held.taken = func() int { return len(rig.runner.Takes()) }
		rig.launchKept(t, "t1")
		session := rig.runner.Session
		session.Taking = func() { time.Sleep(20 * time.Second) }
		rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
		// THE READ'S DEADLINE: its take's own ends five seconds in, and the
		// room it leaves its records thirty-five seconds in, before a record
		// asked once the take is done would end by its own grace.
		deadline := time.Now().Add(2*discardGrace + 5*time.Second)
		ctx, cancel := context.WithDeadline(t.Context(), deadline)
		defer cancel()
		if _, _, err := rig.coordinator.readBox(ctx, rig.get("t1"), BoxReadCollect); err != nil {
			t.Fatalf("readBox: %v", err)
		}
		rig.awaitKeeps()
		marks := held.asked()
		if len(marks) != 1 {
			t.Fatalf("the read asked for the record %d times, want once", len(marks))
		}
		if marks[0].takes != 1 {
			t.Errorf("the record was asked for with %d sessions taken, want once the take was done",
				marks[0].takes)
		}
		if room := deadline.Add(-discardGrace); !marks[0].bounded || marks[0].deadline.After(room) {
			t.Errorf("the record was asked for under a deadline of %v (bounded %v), want one no later than %v, "+
				"the read's own less the pause's grace", marks[0].deadline, marks[0].bounded, room)
		}
		if run := rig.get("t1"); run.PausedAt.IsZero() {
			t.Error("the box's pause was not recorded after a record the store never answered")
		}
	})
}
