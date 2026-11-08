package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
)

// A SESSION'S SAVE POINT IS A COUNT OF THE CALLS ITS ROUND'S LOG TOOK. Every
// call the log takes is numbered as it is appended (BridgeCall.Appended), a
// capture states how many the log had taken when it read it
// (SessionCapture.Through), and a call is after the save exactly where its
// number is past that count — whatever its place among the calls the harness
// made, and however many places the log's bound dropped below it.

// ordinalOf is the place the call named name was appended at in calls, and
// whether calls lists it.
func ordinalOf(calls []BridgeCall, name string) (int, bool) {
	i := slices.IndexFunc(calls, func(c BridgeCall) bool { return c.Name == name })
	if i < 0 {
		return 0, false
	}
	return calls[i].Appended, true
}

// EVERY APPENDED CALL IS NUMBERED IN ITS ROUND, one past the call the log took
// before it, through whatever the log does with its calls: a long round's middle
// dropped by the bound, where each listed call keeps the number it was taken at
// and the numbers the log no longer lists are exactly the calls it counts
// without listing; the calls a session owed, carried by a later append, each
// numbered where the log took it, and one the log already lists neither taken
// nor numbered again; and the call a person approved, which every reader's copy
// numbers as the next job's opening then numbers it, before any call of that
// job.
//
// Mutation: number a call by what the log lists alone (len(Calls)+1), or by
// its place (Seq), and the dropped middle's case goes red; number a call the
// log already lists again, or an owed call where its session first asked, and
// the carried case does; number the approved call anywhere but the one append,
// and its copy and its opening disagree.
func TestEveryAppendedCallIsNumberedInItsRound(t *testing.T) {
	t.Parallel()
	t.Run("past the bound, its middle dropped", func(t *testing.T) {
		t.Parallel()
		const taken = MaxBridgeCalls + 60
		var log BridgeLog
		for i := 1; i <= taken; i++ {
			// ITS PLACE IS NOT ITS NUMBER: the harness made the calls in
			// another order than the log took them.
			log.appendCall(BridgeCall{Name: fmt.Sprintf("c%03d", i), Seq: taken + 1 - i}, nil)
		}
		if log.Appends() != taken || len(log.Calls) != MaxBridgeCalls || log.Elided != taken-MaxBridgeCalls {
			t.Fatalf("the log took %d calls, lists %d and counts %d without listing, want %d, %d and %d",
				log.Appends(), len(log.Calls), log.Elided, taken, MaxBridgeCalls, taken-MaxBridgeCalls)
		}
		listed := map[int]bool{}
		for _, call := range log.Calls {
			want, err := strconv.Atoi(strings.TrimPrefix(call.Name, "c"))
			if err != nil {
				t.Fatalf("a call the case did not make: %q", call.Name)
			}
			if call.Appended != want {
				t.Errorf("%s is numbered %d, want %d: the place the log took it at", call.Name, call.Appended,
					want)
			}
			listed[call.Appended] = true
		}
		unlisted := 0
		for n := 1; n <= taken; n++ {
			if !listed[n] {
				unlisted++
			}
		}
		if unlisted != log.Elided {
			t.Errorf("%d of the round's numbers are listed by no call, and the log counts %d calls it does not "+
				"list: the two are the same calls", unlisted, log.Elided)
		}
	})
	t.Run("owed calls carried by a later append", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		store := NewCoordStore(memory.NewFleet())
		opened, err := store.BeginLaunch(ctx, PendingRun{TurnID: "t1", AgentID: sweID, AgentHandle: "swe",
			Harness: "claude-code", Round: "r1"}, Fence{})
		if err != nil {
			t.Fatalf("BeginLaunch: %v", err)
		}
		call := func(seq int) BridgeCall { return BridgeCall{Name: fmt.Sprintf("c%d", seq), Seq: seq} }
		appended := func(a BridgeAppend) {
			t.Helper()
			a.Launch = opened.LaunchID
			if ok, err := store.AppendBridgeCall(ctx, "t1", a); err != nil || !ok {
				t.Fatalf("AppendBridgeCall = %v, %v", ok, err)
			}
		}
		appended(BridgeAppend{Call: call(1)})
		// THE SESSION'S TWO EARLIER APPENDS THAT DID NOT LAND, and one that
		// did and lost its answer, carried by its fourth call.
		appended(BridgeAppend{Call: call(4),
			Owed: []OwedAppend{{Call: call(2)}, {Call: call(3)}, {Call: call(1)}}})
		// AND THE FOURTH ASKED AGAIN, which landed and lost its answer.
		appended(BridgeAppend{Call: call(4)})
		log, err := store.BridgeLog(ctx, opened)
		if err != nil {
			t.Fatalf("BridgeLog: %v", err)
		}
		if log.Appends() != 4 {
			t.Fatalf("the log took %d calls, want 4: each once", log.Appends())
		}
		for i := 1; i <= 4; i++ {
			if got, ok := ordinalOf(log.Calls, fmt.Sprintf("c%d", i)); !ok || got != i {
				t.Errorf("c%d is numbered %d (listed %v), want %d", i, got, ok, i)
			}
		}
	})
	t.Run("the call a person approved", func(t *testing.T) {
		t.Parallel()
		var log BridgeLog
		log.Round = "r1"
		for i := 1; i <= MaxBridgeCalls+50; i++ {
			log.appendCall(BridgeCall{Name: fmt.Sprintf("c%03d", i), Seq: i}, nil)
		}
		approved := BridgeCall{Name: "tr_close_issue", Approved: true, HeldID: "held-1",
			At: time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)}
		read := log.withHeld(PendingRun{Round: "r1", HeldCall: &HeldCall{ID: "held-1", MadeAt: approved.At,
			Call: &approved}})
		opening := log.opened("job-2", logOpening{round: "r1", owed: &OwedCall{Round: "r1", Call: approved}})
		inRead, _ := ordinalOf(read.Calls, approved.Name)
		inOpening, _ := ordinalOf(opening.Calls, approved.Name)
		if want := log.Appends() + 1; inRead != want || inOpening != want {
			t.Fatalf("a reader's copy numbers the approved call %d and the opening that appends it %d, want "+
				"both %d: the log's next append", inRead, inOpening, want)
		}
		opening.appendCall(BridgeCall{Name: "next-job", Seq: opening.LastSeq() + 1}, nil)
		if got, _ := ordinalOf(opening.Calls, "next-job"); got != inOpening+1 {
			t.Errorf("the next job's first call is numbered %d, want %d: after the call the opening appended "+
				"first", got, inOpening+1)
		}
	})
}

// keptAfter collects turn t1's finished job, whose round's log took a call at
// each place in before ahead of the collection, and a call at each place in
// during while the collection's take read the box — after the capture had read
// the log to place the session — and answers the capture the row keeps and the
// job's log as the collection left it.
func keptAfter(t *testing.T, before, during []int) (SessionCapture, BridgeLog) {
	t.Helper()
	rig, _, _ := keepingRig(t)
	run := rig.launchKept(t, "t1")
	appendAt := func(seq int) {
		if ok, err := rig.pending.AppendBridgeCall(context.Background(), "t1", BridgeAppend{Launch: run.LaunchID,
			Call: BridgeCall{Name: "slack_post", Seq: seq}}); err != nil || !ok {
			t.Errorf("appending the call at %d: %v, %v", seq, ok, err)
		}
	}
	for _, seq := range before {
		appendAt(seq)
	}
	session := rig.runner.Session
	session.Taking = func() {
		for _, seq := range during {
			appendAt(seq)
		}
	}
	rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-1", HarnessVersion: "2.1.310",
		Captured: &session.Archive})
	collectHeld(t, rig, "t1")
	held := rig.get("t1")
	if held.Kept == nil {
		t.Fatalf("the row keeps no capture of the job: %+v", held.Capture)
	}
	log, err := rig.pending.BridgeLog(t.Context(), held)
	if err != nil {
		t.Fatalf("BridgeLog: %v", err)
	}
	return *held.Kept, log
}

// A CAPTURE'S SAVE POINT IS THE APPEND COUNT ITS LOG READ SAW. The capture reads
// the round's log before it takes the box, and states how many calls the log
// had taken then: not the highest place it lists, nor how many it lists, which a
// long round's dropped middle and a harness's own order set apart from the
// count; and a call answered while the box is read, after the log was, is
// numbered past the save point, so a continuation is told it. A log no call
// reached is a save point of none, stated; a capture refused before the log was
// read states none, "not known".
//
// Mutation: place the session by the log's highest place (LastSeq) or by what
// it lists (len(Calls)), read the log after the take, or state no save point
// for a log no call reached, and a case goes red; state one on a refusal made
// before the log was read, and its case does.
func TestACapturesSavePointIsTheAppendCountItsLogReadSaw(t *testing.T) {
	t.Parallel()
	t.Run("a long round, and a call answered as the box was read", func(t *testing.T) {
		t.Parallel()
		var before []int
		for i := 1; i <= MaxBridgeCalls+30; i++ {
			// EVERY OTHER PLACE, so the highest is twice the count.
			before = append(before, 2*i)
		}
		kept, log := keptAfter(t, before, []int{1})
		through, placed := kept.SavePoint()
		if want := MaxBridgeCalls + 30; !placed || through != want {
			t.Fatalf("the capture states save point %d (stated %v), want %d: the calls the log had taken when "+
				"it was read, not the %d it lists nor its highest place %d", through, placed, want,
				MaxBridgeCalls, 2*want)
		}
		i := slices.IndexFunc(log.Calls, func(c BridgeCall) bool { return c.Seq == 1 })
		switch {
		case i < 0:
			t.Error("the log does not list the call answered as the box was read")
		case log.Calls[i].Appended <= through:
			t.Errorf("the call answered as the box was read is numbered %d, want past the save point %d",
				log.Calls[i].Appended, through)
		}
	})
	t.Run("a log no call reached", func(t *testing.T) {
		t.Parallel()
		kept, _ := keptAfter(t, nil, nil)
		if through, placed := kept.SavePoint(); !placed || through != 0 {
			t.Fatalf("the capture states save point %d (stated %v), want 0, stated: a session that holds none "+
				"of the round's calls", through, placed)
		}
	})
	t.Run("a refusal before the log was read", func(t *testing.T) {
		t.Parallel()
		rig, _, _ := keepingRig(t)
		rig.launchKept(t, "t1")
		rig.runner.Finish(finishedOutOfContext(rig.runner.Session))
		collectHeld(t, rig, "t1")
		capture := rig.get("t1").Capture
		if capture == nil || capture.State != CaptureRefused {
			t.Fatalf("the row holds %+v, want the capture refused", capture)
		}
		if through, placed := capture.SavePoint(); placed {
			t.Errorf("a capture refused before its log was read states save point %d, want none: not known",
				through)
		}
	})
}

// A STAMP STILL CARRIES THE ARRIVAL SAVE POINT FOR AN OLDER PEER. A build before
// the count reads a capture's place as the highest bridged call its log listed
// and the places below it the log held no call at (bridge_seq, bridge_open), so
// in a fleet mid-upgrade every stamp carries them too, on the wire under those
// keys, read off the SAME read as the count — so the two state one save point,
// and a call answered while the box is read is open to that build as it is
// after the count to this one.
//
// Mutation: drop either key from the stamp, or place them off a read of the log
// after the take, and this goes red.
func TestAStampStillCarriesTheArrivalSavePointForAnOlderPeer(t *testing.T) {
	t.Parallel()
	// THE THIRD CALL IN FLIGHT AT THE READ, answered as the box was read.
	kept, _ := keptAfter(t, []int{1, 2, 4}, []int{3})
	through, _ := kept.SavePoint()
	if through != 3 || kept.BridgeSeq != 4 || !slices.Equal(kept.BridgeOpen, []int{3}) {
		t.Fatalf("the stamp states save point %d, highest call %d and open places %v, want 3, 4 and [3]: one "+
			"read of the log", through, kept.BridgeSeq, kept.BridgeOpen)
	}
	raw, err := json.Marshal(kept)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"bridge_seq":4`, `"bridge_open":[3]`, `"bridge_through":3`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("the stamp on the wire carries no %s: %s", key, raw)
		}
	}
}

// A KEPT STAMP WITH NO SAVE POINT IS NOT CONTINUED. A build before the count
// stamped its captures with the places it read the log at alone, from which no
// count of the calls after the save can be read, so a relaunch of this build
// does not continue one: it starts a session of its own, saying why. A save
// point of no call is a save point, continued as any is.
//
// Mutation: read a missing save point as 0, or refuse a save point of 0, and
// a case goes red.
func TestAKeptStampWithNoSavePointIsNotContinued(t *testing.T) {
	t.Parallel()
	key := objstore.KeyAt(time.Now())
	row := executorkeep.TurnRow{Handle: "swe", Turn: "t1", Harness: "claude-code", SessionID: "s-1",
		ArchiveFormat: FakeArchiveFormat, Object: objstore.Object{Key: key, Size: 1}, LaunchSeq: 2, CaptureSeq: 1}
	for name, tc := range map[string]struct {
		through   *int
		continues bool
	}{
		"an older build's, which states none": {through: nil},
		"one saved before any call":           {through: new(0), continues: true},
		"one saved after calls":               {through: new(7), continues: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			kept := SessionCapture{LaunchSeq: 2, CaptureSeq: 1, State: CaptureLanded, Object: key.String(),
				Through: tc.through, BridgeSeq: 7}
			run := PendingRun{TurnID: "t1", LaunchSeq: 2, Capture: &kept, Kept: &kept}
			got := ContinuityOf(row, &run, 2, allRestores{})
			if got.Continues != tc.continues {
				t.Fatalf("the relaunch continues %v (%s), want %v", got.Continues, got.Why, tc.continues)
			}
			if !tc.continues && !strings.Contains(got.Why, "states no save point") {
				t.Errorf("the relaunch says %q, want why: its stamp states no save point", got.Why)
			}
		})
	}
}

// A SAVE IS PLACED AT THE NEWEST SAVE POINT ITS LAUNCH IS KNOWN TO HOLD, AND AT
// NONE IT IS NOT. A save is packed while its job works, and Claude Code makes a
// call before it writes it, so nothing read off the log as the save is taken
// says what the session holds: the save takes the newest of its launch's own
// landed save and the capture the launch continued, where that capture counts
// this round's calls, or the round's start. The places an older build reads come
// from the same capture as the point, copied rather than shared.
//
// Mutation: take the turn's kept capture of an earlier launch for this launch's
// save, the continued capture's point across a new round or from a row that
// names none, or the first candidate rather than the newest, and a case goes
// red; place the older build's places off another capture, or share its slice,
// and the copied case does; leave the places a stamp already carried where no
// capture places it, and every case placed at zero does.
func TestASaveIsPlacedAtTheNewestSavePointItsLaunchIsKnownToHold(t *testing.T) {
	t.Parallel()
	// saved is a capture of the job launch (its ordinal seq) at save point
	// through, placed for an older build at seq of the highest call and open
	// at the places below it.
	saved := func(launch string, seq int64, through *int, highest int, open ...int) *SessionCapture {
		return &SessionCapture{LaunchSeq: seq, CaptureSeq: 1, Launch: launch, State: CaptureLanded,
			Through: through, BridgeSeq: highest, BridgeOpen: open}
	}
	const job1, job2 = "job-1", "job-2"
	for name, tc := range map[string]struct {
		round     string
		kept      *SessionCapture
		continued *SessionCapture
		through   int
		highest   int
		open      []int
	}{
		"a launch that continued nothing and has saved nothing": {round: job2},
		"a launch that carried its round on from the job before's capture": {round: job1,
			continued: saved(job1, 1, new(5), 7, 6), through: 5, highest: 7, open: []int{6}},
		"a launch that began a new round from the job before's capture": {round: job2,
			continued: saved(job1, 1, new(5), 7, 6)},
		"a launch whose own save landed": {round: job1, kept: saved(job2, 2, new(9), 11),
			continued: saved(job1, 1, new(5), 7), through: 9, highest: 11},
		"the turn's kept capture an earlier launch took": {round: job2, kept: saved(job1, 1, new(9), 11)},
		"its own landed save stating no save point, an older build's": {round: job1,
			kept: saved(job2, 2, nil, 11), continued: saved(job1, 1, new(5), 7), through: 5, highest: 7},
		"a row that names no round, an older build's": {continued: saved(job1, 1, new(5), 7)},
		"a continued capture newer than its own landed save": {round: job1, kept: saved(job2, 2, new(4), 4),
			continued: saved(job1, 1, new(6), 8, 7), through: 6, highest: 8, open: []int{7}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			run := PendingRun{TurnID: "t1", LaunchID: job2, LaunchSeq: 2, Round: tc.round, Kept: tc.kept,
				Capture: tc.kept}
			// A STAMP THAT ALREADY CARRIES PLACES, as one built from a copy of
			// another capture would: placed at zero, it carries none of them.
			save := SessionCapture{BridgeSeq: 99, BridgeOpen: []int{98}}
			save.placeAtNewest(savePlaces{run: run, continued: tc.continued})
			through, placed := save.SavePoint()
			if !placed || through != tc.through {
				t.Fatalf("the save is placed at %d (stated %v), want %d, stated", through, placed, tc.through)
			}
			if save.BridgeSeq != tc.highest || !slices.Equal(save.BridgeOpen, tc.open) {
				t.Errorf("the save carries %d and %v for an older build, want %d and %v: the places of the "+
					"capture its point is", save.BridgeSeq, save.BridgeOpen, tc.highest, tc.open)
			}
			if len(save.BridgeOpen) > 0 {
				save.BridgeOpen[0] = -1
				for _, source := range []*SessionCapture{tc.kept, tc.continued} {
					if source != nil && slices.Contains(source.BridgeOpen, -1) {
						t.Error("the save shares its open places with the capture it was placed by")
					}
				}
			}
		})
	}
}

// A CAPTURE IS PLACED BY ITS LOG READ ONLY WHERE ITS HARNESS ENDED ITS OWN RUN.
// A harness can make a call, and have it answered, before it writes the call to
// its transcript (Claude Code 2.1.310, measured: it writes a message only once
// the whole of it has streamed), so the calls its round's log had taken before
// the pack say what the archive holds only of a job whose harness went cleanly
// by itself, its script packing it. Every other capture is placed as a save is,
// at the newest save point its launch is known to hold: a job still working,
// taken as it stands; and every job CUT OFF — its harness killed, past its
// end's grace or by a signal its script packed after; told to stop by its end;
// ended before its script packed, packed by the read; or lost, its box read by
// the loss, which cannot say how its job went. Each is placed at the round's
// start, its launch holding no landed capture and the capture it continued
// unknown — or at its own launch's landed capture where it holds one, with that
// capture's places for an older build — so a relaunch continuing it is told
// every call its archive may lack.
//
// Mutation: place a capture of a job cut off at its log read, as a job that is
// over was, and its cases count calls its archive may lack; place every
// capture as a save is, and the job that ended its own run tells again every
// call its session holds; place one at zero whatever its launch holds, and the
// case whose launch kept a capture goes red.
func TestACaptureIsPlacedByItsLogReadOnlyWhereItsHarnessEndedItsOwnRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// capped launches the turn with its wall-clock cap passed, so its
		// collection ends the job first.
		capped bool
		// finished is what a job reports that ended by itself, and ended what
		// one reports once its end ended it, each holding the archive its
		// script packed; nil for neither.
		finished, ended func(archive *SessionArchive) Result
		// unstoppable is a job its end cannot stop, taken as it stands.
		unstoppable bool
		// lost reads the job's box as a loss's read does, not as a collection.
		lost bool
		// own lands a capture of the job's own launch before the take.
		own bool
		// final and cutOff are how the take saw the job.
		final, cutOff    bool
		through, highest int
		open             []int
	}{
		{name: "a job whose harness ended its own run, its script packing it",
			finished: func(a *SessionArchive) Result {
				return Result{Success: true, Text: "done", SessionID: "s-1", HarnessVersion: "2.1.310", Captured: a}
			},
			final: true, through: 3, highest: 3},
		{name: "a job whose harness was killed, its script packing after it",
			finished: func(a *SessionArchive) Result {
				return Result{Text: "killed", SessionID: "s-1", HarnessVersion: "2.1.310", Captured: a,
					HarnessCutOff: true}
			},
			final: true, cutOff: true},
		{name: "a job its turn's cap ended, its harness killed past its grace", capped: true,
			ended: func(a *SessionArchive) Result {
				return Result{SessionID: "s-1", Captured: a, HarnessCutOff: true, JobEnd: JobEndKilled}
			},
			final: true, cutOff: true},
		{name: "a job its turn's cap ended, its harness told to stop", capped: true,
			ended: func(a *SessionArchive) Result {
				return Result{SessionID: "s-1", Captured: a, HarnessCutOff: true, JobEnd: JobEndAsked}
			},
			final: true, cutOff: true},
		{name: "a job its turn's cap ended before its script packed, packed by the read", capped: true,
			final: true, cutOff: true},
		{name: "a job its end could not stop, taken as it stands", capped: true, unstoppable: true, cutOff: true},
		{name: "a lost job, its box read by the loss", lost: true, final: true, cutOff: true},
		{name: "a job cut off whose own launch had kept a capture", capped: true, own: true,
			ended: func(a *SessionArchive) Result {
				return Result{SessionID: "s-1", Captured: a, HarnessCutOff: true, JobEnd: JobEndKilled}
			},
			final: true, cutOff: true, through: 2, highest: 2, open: []int{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig, _, _ := keepingRig(t)
			deadline := time.Time{}
			if tc.capped {
				deadline = rig.now.Add(-time.Minute)
			}
			launched := rig.launchUntil("t1", deadline)
			rig.handOver(t, "t1", HandedSecrets{Values: []string{handedToken}})
			rig.coordinator.countRun("swe", StatusRunning)
			// THREE CALLS ANSWERED before the take reads the log, any of which
			// the harness may not have written.
			for seq := 1; seq <= 3; seq++ {
				if ok, err := rig.pending.AppendBridgeCall(t.Context(), "t1", BridgeAppend{Launch: launched.LaunchID,
					Call: BridgeCall{Name: "slack_post", Seq: seq}}); err != nil || !ok {
					t.Fatalf("appending the call at %d: %v, %v", seq, ok, err)
				}
			}
			if tc.own {
				own := SessionCapture{LaunchSeq: launched.LaunchSeq, CaptureSeq: 1, Launch: launched.LaunchID,
					State: CaptureLanded, At: rig.now, Through: new(2), BridgeSeq: 2, BridgeOpen: []int{1},
					Object: objstore.KeyAt(rig.now).String(), Harness: "claude-code", SessionID: "s-1",
					ArchiveFormat: FakeArchiveFormat}
				if took, err := rig.pending.StampCapture(t.Context(), "t1", own); err != nil || !took {
					t.Fatalf("landing the launch's own capture = %v, %v", took, err)
				}
			}
			archive := rig.runner.Session.Archive
			switch {
			case tc.finished != nil:
				rig.runner.Finish(tc.finished(&archive))
			case tc.ended != nil:
				rig.runner.EndsAs(tc.ended(&archive))
			}
			if tc.unstoppable {
				// A SCRIPT THAT HEARS THE REQUEST AND GOES ON, and a kill past
				// the wait that reaches nothing of the job.
				rig.coordinator.endWait = 50 * time.Millisecond
				rig.runner.EndFunc = func(context.Context, Sandbox, RunHandle) (EndAsk, error) {
					return EndAsked, nil
				}
				rig.provider.Box(launched.SandboxID).KillJobFunc = func(context.Context, string) error {
					return ErrNoJobGroup
				}
			}
			if tc.lost {
				spooled, _ := rig.coordinator.readLost(t.Context(), rig.get("t1"))
				rig.coordinator.keepLater(spooled)
			} else {
				// COLLECTED, the resume after it refused, so the row stays for
				// the case.
				rig.resumer.failWith(errors.New("held for the case to read the row"))
				payload, ev := rig.completion("t1")
				if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
					t.Logf("OnCompleted: %v", err)
				}
			}
			rig.awaitKeeps()
			kept := rig.get("t1").Kept
			if kept == nil || kept.State != CaptureLanded || kept.CaptureSeq != nextSeq(tc.own) ||
				kept.Final != tc.final || kept.Interrupted != tc.cutOff {
				t.Fatalf("the row keeps %+v, want the take's capture landed, final: %v, cut off: %v", kept,
					tc.final, tc.cutOff)
			}
			through, placed := kept.SavePoint()
			if !placed || through != tc.through {
				t.Errorf("the capture is placed at %d (stated %v), want %d", through, placed, tc.through)
			}
			if kept.BridgeSeq != tc.highest || !slices.Equal(kept.BridgeOpen, tc.open) {
				t.Errorf("the capture carries %d and %v for an older build, want %d and %v: the places of the "+
					"point it states", kept.BridgeSeq, kept.BridgeOpen, tc.highest, tc.open)
			}
		})
	}
}

// nextSeq is the place among its launch's captures the take's capture takes:
// the first, or the one after the capture the launch already kept.
func nextSeq(kept bool) int64 {
	if kept {
		return 2
	}
	return 1
}
