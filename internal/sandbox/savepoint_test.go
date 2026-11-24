package sandbox

import (
	"cmp"
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

// EVERY APPENDED CALL IS NUMBERED IN ITS ROUND, one past the call before it:
// past the bound, a listed call keeps its number and the unlisted numbers are
// the calls counted without listing; a call whose entry was written ahead is
// numbered as its update lands, in finishing order, and one already listed is
// never numbered again; a person's approved call is numbered once, as the next
// job's opening, before any call of that job.
//
// Mutation: number by len(Calls)+1 or by Seq, and the dropped middle's case
// goes red; number a listed call again, or a call as its entry is written, and
// the updates' case does; number the approved call anywhere but the one append,
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
	t.Run("calls numbered as their updates land", func(t *testing.T) {
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
		// TWO CALLS BEGUN, their entries written ahead of them: numbered by
		// none, since no session holds the outcome of a call in flight.
		appended(BridgeAppend{Step: AppendBegin, Call: call(1)})
		appended(BridgeAppend{Step: AppendBegin, Call: call(2)})
		// THE SECOND FINISHES FIRST, and the first's update is asked again,
		// having landed and lost its answer.
		appended(BridgeAppend{Call: call(2)})
		appended(BridgeAppend{Call: call(1)})
		appended(BridgeAppend{Call: call(1)})
		log, err := store.BridgeLog(ctx, opened)
		if err != nil {
			t.Fatalf("BridgeLog: %v", err)
		}
		if log.Appends() != 2 || len(log.InFlight) != 0 {
			t.Fatalf("the log took %d calls and holds %d in flight, want 2, each once, and none",
				log.Appends(), len(log.InFlight))
		}
		for name, want := range map[string]int{"c2": 1, "c1": 2} {
			if got, ok := ordinalOf(log.Calls, name); !ok || got != want {
				t.Errorf("%s is numbered %d (listed %v), want %d: the order the updates landed in", name, got, ok,
					want)
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
		opening, _ := log.opened("job-2", logOpening{round: "r1", owed: &OwedCall{Round: "r1", Call: approved}})
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
//
// Built on [keepingRig], and so only in a bubble ([inBubble]).
//
//fixedwait:bubble
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
	session.Taking = func(context.Context) {
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

// A CAPTURE'S SAVE POINT IS THE APPEND COUNT ITS LOG READ SAW, read before the
// box is taken: not the highest place listed nor how many are listed, which a
// dropped middle and the harness's own order set apart from the count. A call
// answered while the box is read is past the save point, so a continuation is
// told it. A log no call reached states a save point of zero; a capture refused
// before the log was read states none, "not known".
//
// Mutation: place the session by LastSeq or len(Calls), read the log after the
// take, or state no save point for a log no call reached, and a case goes red;
// state one on a refusal made before the log was read, and its case does.
func TestACapturesSavePointIsTheAppendCountItsLogReadSaw(t *testing.T) {
	t.Parallel()
	t.Run("a long round, and a call answered as the box was read", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
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
	})
	t.Run("a log no call reached", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			kept, _ := keptAfter(t, nil, nil)
			if through, placed := kept.SavePoint(); !placed || through != 0 {
				t.Fatalf("the capture states save point %d (stated %v), want 0, stated: a session that holds none "+
					"of the round's calls", through, placed)
			}
		})
	})
	t.Run("a refusal before the log was read", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
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
	inBubble(t, func(t *testing.T) {
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
	})
}

// A KEPT CAPTURE SAVED BEFORE ANY CALL IS CONTINUED, as one saved after calls
// is: a save point of no call is a save point.
//
// Mutation: refuse a save point of 0, and a case goes red.
func TestAKeptCaptureSavedBeforeAnyCallIsContinued(t *testing.T) {
	t.Parallel()
	key := objstore.KeyAt(time.Now())
	row := executorkeep.TurnRow{Handle: "swe", Turn: "t1", Harness: "claude-code", SessionID: "s-1",
		ArchiveFormat: FakeArchiveFormat, Object: objstore.Object{Key: key, Size: 1}, LaunchSeq: 2, CaptureSeq: 1}
	for name, tc := range map[string]struct {
		through *int
	}{
		"one saved before any call": {through: new(0)},
		"one saved after calls":     {through: new(7)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			kept := SessionCapture{LaunchSeq: 2, CaptureSeq: 1, State: CaptureLanded, Object: key.String(),
				Through: tc.through, BridgeSeq: 7}
			run := PendingRun{TurnID: "t1", LaunchSeq: 2, Capture: &kept, Kept: &kept}
			got := ContinuityOf(row, &run, 2, allRestores{})
			if !got.Continues {
				t.Fatalf("the relaunch does not continue it (%s), want it continued", got.Why)
			}
		})
	}
}

// A CAPTURE POINT RISES ONLY WHERE ITS TAKE NAMES THE HELD CAPTURE'S NEWEST
// COMPACTION (savepoint.go). Reserved at zero, a capture is raised once its
// take's count is stamped, to the newest point stated by its launch's own
// landed capture or by the capture it continued (the job before's, taken up by
// its box), where that capture names the same compaction. Omitted is the
// launch's own at or below the point, LateOmitted the launch's whole; an older
// build's places stay zero and nil. Every other case stays at zero.
//
// Mutation: compare counts, not identities; drop the SessionStart, launch or
// bounds check; accept an unknown basis; carry launchKept's point unchecked;
// take Omitted off the carried capture or past the point, or LateOmitted short
// of the launch's; or copy BridgeSeq.
func TestACapturePointRisesOnlyWhereItsTakeNamesTheHeldCapturesNewestCompaction(t *testing.T) {
	t.Parallel()
	const job = "job-2"
	// own is the launch's own landed capture at through, naming newest.
	own := func(through int, newest *string) *SessionCapture {
		return &SessionCapture{LaunchSeq: 2, CaptureSeq: 1, Launch: job, State: CaptureLanded,
			Through: new(through), BridgeSeq: 11, BridgeOpen: []int{10}, Omitted: []int{4},
			ResumedCount: &ResumedCount{Tokens: 1, Restart: newest}}
	}
	// continued is the launch's record of a continued capture at 7 of the job
	// before, taken up by its box, naming basis.
	continued := func(basis *string) LaunchRecord {
		return LaunchRecord{Brief: BriefContinued, SessionStart: SessionStartContinued, BasisRestart: basis,
			BriefOmitted: []int{3, 12}, BriefLateOmitted: []int{5, 13},
			Continued: &ContinuedPoint{LaunchSeq: 1, Through: 7}}
	}
	past := make([]int, MaxBridgeCalls+1)
	for i := range past {
		past[i] = i + 1
	}
	for name, tc := range map[string]struct {
		harness string
		// launchSeq is the job's launch, 2 where unset.
		launchSeq int64
		take      *string
		launch    LaunchRecord
		kept      *SessionCapture
		through   int
		omitted   []int
	}{
		"the continued capture, neither compacted": {take: new(""), launch: continued(new("")), through: 7,
			omitted: []int{3}},
		"the continued capture, one compaction": {take: new("msg_1"), launch: continued(new("msg_1")),
			through: 7, omitted: []int{3}},
		"an OpenCode capture, one compaction": {harness: "opencode", take: new("msg_1"),
			launch: continued(new("msg_1")), through: 7, omitted: []int{3}},
		"a compaction since the continued capture": {take: new("msg_2"), launch: continued(new("msg_1"))},
		"a take whose identity was not read":       {launch: continued(new("msg_1"))},
		"a basis not known":                        {take: new(""), launch: continued(nil)},
		"a box that started a session of its own": {take: new("msg_1"), launch: func() LaunchRecord {
			l := continued(new("msg_1"))
			l.SessionStart = SessionStartFresh
			return l
		}()},
		"a box that never said": {take: new("msg_1"), launch: func() LaunchRecord {
			l := continued(new("msg_1"))
			l.SessionStart = ""
			return l
		}()},
		"a word this build does not know": {take: new("msg_1"), launch: func() LaunchRecord {
			l := continued(new("msg_1"))
			l.SessionStart = "a-later-builds-start"
			return l
		}()},
		"a point of a launch before the job before": {take: new("msg_1"), launch: func() LaunchRecord {
			l := continued(new("msg_1"))
			l.Continued.LaunchSeq = 0
			return l
		}()},
		"a point of no launch, on the first": {launchSeq: 1, take: new("msg_1"), launch: func() LaunchRecord {
			l := continued(new("msg_1"))
			l.Continued.LaunchSeq = 0
			return l
		}()},
		"a point of this very launch": {take: new("msg_1"), launch: func() LaunchRecord {
			l := continued(new("msg_1"))
			l.Continued.LaunchSeq = 2
			return l
		}()},
		"its own landed capture, newer and agreeing": {take: new("msg_1"), launch: continued(new("msg_1")),
			kept: own(9, new("msg_1")), through: 9, omitted: []int{3}},
		"its own landed capture, of another compaction": {take: new("msg_1"), launch: continued(new("msg_1")),
			kept: own(9, new("msg_0")), through: 7, omitted: []int{3}},
		"its own landed capture alone": {take: new("msg_1"), launch: LaunchRecord{BriefOmitted: []int{3, 12}},
			kept: own(12, new("msg_1")), through: 12, omitted: []int{3, 12}},
		"its own landed capture, its identity not read": {take: new("msg_1"), launch: LaunchRecord{},
			kept: own(9, nil)},
		"a raise past what a stamp holds": {take: new("msg_1"), launch: func() LaunchRecord {
			l := continued(new("msg_1"))
			l.BriefOmitted = past
			l.Continued.Through = len(past)
			return l
		}()},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			run := PendingRun{TurnID: "t1", LaunchID: job, LaunchSeq: cmp.Or(tc.launchSeq, 2), Launch: tc.launch,
				Kept: tc.kept, Capture: tc.kept}
			capture := SessionCapture{Harness: cmp.Or(tc.harness, "claude-code"), BridgeSeq: 99,
				BridgeOpen: []int{98}, Omitted: []int{97}, LateOmitted: []int{96}}
			capture.placeAtZero(run.Launch)
			capture.ResumedCount = &ResumedCount{Tokens: 1, Restart: tc.take}
			capture.placeAtNewest(savePlaces{run: run, resumeRead: true})
			if through, stated := capture.SavePoint(); !stated || through != tc.through {
				t.Errorf("the capture is placed at %d (stated %v), want %d", through, stated, tc.through)
			}
			if !slices.Equal(capture.Omitted, tc.omitted) {
				t.Errorf("the capture states %v left out, want %v", capture.Omitted, tc.omitted)
			}
			if want := tc.launch.LateOmittedCalls(); !slices.Equal(capture.LateOmitted, want) {
				t.Errorf("the capture states late calls %v left out, want its launch's whole, %v",
					capture.LateOmitted, want)
			}
			if capture.BridgeSeq != 0 || capture.BridgeOpen != nil {
				t.Errorf("the capture carries %d and %v for an older build, want zero and none, which tells it "+
					"every call", capture.BridgeSeq, capture.BridgeOpen)
			}
		})
	}
}

// EVERY CAPTURE NO READ PLACES IS RESERVED AT ZERO, whatever places its stamp
// already carries: a stated zero, no place an older build reads and no read
// left out, so a continuation of it is told every call of the round; and every
// late call its launch's brief counted, which a continuation credits no telling
// of, at whatever point (internal/agent/runner's heldByTelling).
//
// Mutation: keep a stamp's places, state no point, or reserve with fewer late
// calls than the launch's, and this goes red.
func TestEveryCaptureNoReadPlacesIsReservedAtZero(t *testing.T) {
	t.Parallel()
	capture := SessionCapture{Through: new(9), BridgeSeq: 11, BridgeOpen: []int{10}, Omitted: []int{3},
		LateOmitted: []int{2}}
	launch := LaunchRecord{Brief: BriefFresh, BriefOmitted: []int{4}, BriefLateOmitted: []int{5, 13}}
	capture.placeAtZero(launch)
	if through, stated := capture.SavePoint(); !stated || through != 0 || capture.BridgeSeq != 0 ||
		capture.BridgeOpen != nil || capture.Omitted != nil {
		t.Errorf("the capture is reserved at %d (stated %v), %d, %v and %v, want a stated zero and nothing else",
			through, stated, capture.BridgeSeq, capture.BridgeOpen, capture.Omitted)
	}
	if !slices.Equal(capture.LateOmitted, []int{5, 13}) {
		t.Errorf("the capture is reserved stating late calls %v left out, want its launch's [5 13]",
			capture.LateOmitted)
	}
	if capture.LateOmitted[0] = -1; launch.BriefLateOmitted[0] == -1 {
		t.Error("the capture shares its late calls with its launch's record")
	}
}

// A CLAUDE CODE SAVE OUTSIDE THE RESUME READ SET IS NEVER RAISED. Its session
// stands for the calls at or below a raised point only while it grows between
// two captures with one newest compaction, and a Claude Code release withdraws
// messages in place in ways this build read only for the releases whose
// resume it read. So a capture whose archive another release wrote stays at
// zero, its identities read and equal and a continued point above zero
// notwithstanding; the gate is Claude Code's alone.
//
// Mutation: drop the release gate, and the first case is raised.
func TestAClaudeCodeSaveOutsideTheResumeReadSetIsNeverRaised(t *testing.T) {
	t.Parallel()
	run := PendingRun{TurnID: "t1", LaunchID: "job-2", LaunchSeq: 2, Launch: LaunchRecord{
		SessionStart: SessionStartContinued, BasisRestart: new("u-1"),
		Continued: &ContinuedPoint{LaunchSeq: 1, Through: 7}}}
	for name, tc := range map[string]struct {
		harness    string
		resumeRead bool
		through    int
	}{
		"Claude Code, a release whose resume was not read": {harness: "claude-code"},
		"Claude Code, a release whose resume was read":     {harness: "claude-code", resumeRead: true, through: 7},
		"OpenCode": {harness: "opencode", through: 7},
	} {
		save := SessionCapture{Harness: tc.harness, Interrupted: true,
			ResumedCount: &ResumedCount{Tokens: 1, Restart: new("u-1")}}
		save.placeAtZero(run.Launch)
		save.placeAtNewest(savePlaces{run: run, resumeRead: tc.resumeRead,
			needsResumeRead: testThreadTraits[tc.harness].PlacesWithResumeRead})
		if through, _ := save.SavePoint(); through != tc.through {
			t.Errorf("%s: the save is placed at %d, want %d", name, through, tc.through)
		}
	}
}

// A CAPTURE IS PLACED BY ITS LOG READ ONLY WHERE ITS HARNESS ENDED ITS OWN RUN
// (savepoint.go's file comment). Every other one (a job still working, and
// every job CUT OFF: killed, told to stop by its end, ended before its script
// packed, or lost) is reserved at zero and raised after its take to its own
// launch's landed capture's point where both name one newest compaction, with
// no place an older build reads, so a relaunch is told every call its archive
// may lack.
//
// Mutation: place a cut-off capture at its log read, every capture at its
// launch's newest save point, or one at zero whatever its launch holds, and a
// case goes red.
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
			final: true, cutOff: true, through: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
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
						ArchiveFormat: FakeArchiveFormat, ResumedCount: &ResumedCount{Tokens: 1, Restart: new("u-1")}}
					// THE TAKE NAMES THE SAME NEWEST COMPACTION, of a release whose
					// resume was read.
					rig.runner.Session.Count = &ResumedCount{Tokens: 1, Restart: new("u-1")}
					rig.runner.Session.ResumeRead = true
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
					spooled, _, _ := rig.coordinator.readLost(t.Context(), rig.get("t1"))
					rig.coordinator.keepLater(t.Context(), spooled)
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

// ONLY A CAPTURE ITS LOG READ PLACES READS THE LOG. A job whose harness ended
// its own run is placed by its read of the round's log, so a log the store
// cannot read refuses its capture, and a refusal after the read states the
// count the read saw. A job cut off is placed off its row, at the newest save
// point its launch is known to hold, and reads no log: one the store cannot
// read refuses nothing of it, and a refusal of it states no save point, "not
// known".
//
// Mutation: read the log for a job cut off and refuse its capture where the
// read fails, and its unread case is refused; state a save point on the
// refusal of a capture no read places, and its refused case states one; state
// none on the refusal of one its read placed, and that case states none.
func TestOnlyACaptureItsLogReadPlacesReadsTheLog(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// cutOff is a job whose harness was killed, its script packing after
		// it; else one whose harness ended its own run.
		cutOff bool
		// unread fails every read of the round's log, and untaken the take.
		unread, untaken bool
		// state is how the capture comes out, and through the save point it
		// states, nil for none.
		state   CaptureState
		through *int
	}{
		{name: "a job whose harness ended its own run, its log unread", unread: true, state: CaptureRefused},
		{name: "a job whose harness was killed, its log unread", cutOff: true, unread: true,
			state: CaptureLanded, through: new(0)},
		{name: "a job whose harness ended its own run, its take refused", untaken: true, state: CaptureRefused,
			through: new(3)},
		{name: "a job whose harness was killed, its take refused", cutOff: true, untaken: true,
			state: CaptureRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, _, _ := keepingRig(t)
				launched := rig.launchKept(t, "t1")
				// THREE CALLS ANSWERED before the take, which a read would count.
				for seq := 1; seq <= 3; seq++ {
					if ok, err := rig.pending.AppendBridgeCall(t.Context(), "t1", BridgeAppend{Launch: launched.LaunchID,
						Call: BridgeCall{Name: "slack_post", Seq: seq}}); err != nil || !ok {
						t.Fatalf("appending the call at %d: %v, %v", seq, ok, err)
					}
				}
				if tc.unread {
					rig.coordinator.pending = &refusingStore{inner: rig.coordinator.pending, refuse: []string{"TurnLog"}}
				}
				if tc.untaken {
					rig.runner.Session.TakeErr = errors.New("the box's session could not be read")
				}
				archive := rig.runner.Session.Archive
				rig.runner.Finish(Result{Success: !tc.cutOff, Text: "done", SessionID: "s-1", HarnessVersion: "2.1.310",
					Captured: &archive, HarnessCutOff: tc.cutOff})
				// COLLECTED, the resume after it refused, so the row stays for the
				// case.
				rig.resumer.failWith(errors.New("held for the case to read the row"))
				payload, ev := rig.completion("t1")
				if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
					t.Logf("OnCompleted: %v", err)
				}
				rig.awaitKeeps()
				capture := rig.get("t1").Capture
				if capture == nil || capture.State != tc.state || capture.Interrupted != tc.cutOff {
					t.Fatalf("the row holds %+v, want the capture %s, cut off: %v", capture, tc.state, tc.cutOff)
				}
				through, placed := capture.SavePoint()
				switch {
				case tc.through == nil && placed:
					t.Errorf("the capture states save point %d, want none: not known", through)
				case tc.through != nil && (!placed || through != *tc.through):
					t.Errorf("the capture states save point %d (stated %v), want %d", through, placed, *tc.through)
				}
			})
		})
	}
}
