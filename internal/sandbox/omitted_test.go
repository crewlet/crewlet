package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// WHAT A JOB'S RECORD KEEPS OF WHAT ITS BRIEF LEFT OUT ([BriefTold]). A brief
// that tells a round's calls past the prior-work ledger's read cap counts the
// reads past it rather than listing them, so the session it is handed holds
// them only as a count: the launch records which reads they were on the job it
// opens, by the brief its run read, and a capture of that job's session placed
// by its own read states them, so every later continuation tells them.

// toldApart is a launch's continuation with each brief's left-out set told
// apart: the copy's brief left out the round's first call, the fresh one its
// second.
func toldApart(c *Continuation) *Continuation {
	c.Told, c.Fresh = BriefTold{Omitted: []int{1}}, BriefTold{Omitted: []int{2}}
	return c
}

// A LAUNCH RECORDS THE OMITTED CALLS OF EACH BRIEF. Which brief a launch hands
// its run is the launch's to decide, once it has its box, so its request carries
// what each brief left out of the round it carries on, and its record keeps the
// one of the brief it handed (the copy's in the asking job's own box, the kept
// capture's in a box that replaced that one's, the fresh one's where its box was
// reclaimed) and, beside a brief that continues a session, the fresh one's too,
// which its box hands the run where it cannot take the session up after all.
//
// Mutation: record every launch's fresh set as the brief's, read the kept
// capture's set off another brief, or record the fresh set beside a fresh brief
// as well, and a case goes red.
func TestALaunchRecordsTheOmittedCallsOfEachBrief(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		reap         bool
		brief        BriefKind
		told, beside []int
	}{
		"the asking job's own copy, in its box":   {brief: BriefContinued, told: []int{1}, beside: []int{2}},
		"a session of its own, the box reclaimed": {reap: true, brief: BriefFresh, told: []int{2}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			askAndAnswer(t, rig, tc.reap, func(ctx context.Context, _ PendingRun, asked *AskedRun) {
				req := continuing(asked)
				toldApart(req.Continue)
				if _, err := rig.launchVia(ctx, rig.manager, req); err != nil {
					t.Errorf("relaunch: %v", err)
				}
			})
			got := rig.get("t1").Launch
			if got.Brief != tc.brief || !slices.Equal(got.BriefOmitted, tc.told) ||
				!slices.Equal(got.FreshOmitted, tc.beside) {
				t.Errorf("the relaunch recorded brief %q, its set %v and the fresh one's %v; want %q, %v and %v",
					got.Brief, got.BriefOmitted, got.FreshOmitted, tc.brief, tc.told, tc.beside)
			}
		})
	}
	t.Run("the turn's kept capture, in a box that replaced its own", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, restore, kept := keptTurn(t)
			req := continuingKept(kept)
			req.Session.Told = BriefTold{Omitted: []int{3}}
			req.Continue = toldApart(&Continuation{})
			if _, err := launchRestoring(t, rig, restore, req); err != nil {
				t.Fatalf("relaunch: %v", err)
			}
			got := rig.get("t1").Launch
			if got.Brief != BriefContinuedNewBox || !slices.Equal(got.BriefOmitted, []int{3}) ||
				!slices.Equal(got.FreshOmitted, []int{2}) {
				t.Errorf("the relaunch recorded brief %q, its set %v and the fresh one's %v; want the new-box "+
					"continuation's [3], beside the fresh brief's [2]", got.Brief, got.BriefOmitted, got.FreshOmitted)
			}
		})
	})
}

// A NEW ROUND RECORDS NO OMITTED CALL. A reviewer's send-back continues the kept
// session of the collected job, whose brief tells the collected round's calls,
// but opens a new round, whose log numbers its calls from its start: a set of
// the collected round's places, read against the new round's log, would name
// calls the new log holds under those numbers, which no brief left out. So a
// launch that carries no round on records none, whatever its briefs left out.
//
// Mutation: record the brief's set where the launch carries no round on, and
// the send-back's job names calls of a round it never told.
func TestANewRoundRecordsNoOmittedCall(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, restore, kept := keptTurn(t)
		req := continuingKept(kept)
		req.Session.Told = BriefTold{Omitted: []int{3}}
		if _, err := launchRestoring(t, rig, restore, req); err != nil {
			t.Fatalf("relaunch: %v", err)
		}
		if got := rig.get("t1").Launch; got.Brief != BriefContinuedNewBox || len(got.BriefOmitted) != 0 ||
			len(got.FreshOmitted) != 0 {
			t.Errorf("a send-back recorded brief %q and %v, %v left out; want the new-box continuation and none",
				got.Brief, got.BriefOmitted, got.FreshOmitted)
		}
	})
}

// A FRESH LAUNCH READS NO CONTINUATION IT WAS NOT GIVEN. A turn's first launch
// and a send-back that continues no session carry no round on and name no
// continuation, so the fresh brief's set is read off a continuation only where
// the request carries one: such a launch takes none, and launches.
//
// Mutation: read the fresh brief's set off the request's continuation whether
// or not it carries one, and a first launch fails.
func TestAFreshLaunchReadsNoContinuationItWasNotGiven(t *testing.T) {
	t.Parallel()
	if told := freshLaunch(LaunchRequest{Brief: "the turn's first brief"}).told; len(told.Omitted) != 0 {
		t.Errorf("a first launch reads %v left out off a continuation it was not given", told.Omitted)
	}
	carried := freshLaunch(LaunchRequest{Brief: "the reseed", Continue: toldApart(&Continuation{})}).told
	if !slices.Equal(carried.Omitted, []int{2}) {
		t.Errorf("a fresh launch carrying a round on reads %v left out, want the fresh brief's [2]",
			carried.Omitted)
	}
	rig := newWaiterRig(t)
	if _, err := rig.launchVia(t.Context(), rig.manager, launchReq("t1")); err != nil {
		t.Fatalf("a first launch: %v", err)
	}
	if got := rig.get("t1").Launch; got.Brief != BriefFresh || len(got.BriefOmitted)+len(got.FreshOmitted) != 0 {
		t.Errorf("a first launch recorded brief %q and %v, %v left out, want a fresh one and none", got.Brief,
			got.BriefOmitted, got.FreshOmitted)
	}
}

// A LAUNCH WHOSE BRIEFS LEFT OUT MORE THAN A ROUND'S LOG LISTS IS REFUSED, BEFORE
// ANYTHING IS OPENED. A brief tells at most the calls a round's log lists, so a
// set past [MaxBridgeCalls] names calls no log holds, and would ride the job's
// record for nothing: the launch is refused naming the field, before its row is
// written. So is a capture stating such a set ([SessionCapture.Bounded]).
//
// Mutation: drop the bound from the launch, or from the capture's, and a case
// goes red.
func TestALaunchWhoseBriefsLeftOutMoreThanALogListsIsRefused(t *testing.T) {
	t.Parallel()
	past := BriefTold{Omitted: make([]int, MaxBridgeCalls+1)}
	for field, set := range map[string]func(*LaunchRequest){
		"continue.told":  func(r *LaunchRequest) { r.Continue = &Continuation{Told: past} },
		"continue.fresh": func(r *LaunchRequest) { r.Continue = &Continuation{Fresh: past} },
		"session.told":   func(r *LaunchRequest) { r.Session = &SessionContinuation{Base: true, Told: past} },
	} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			rig := newWaiterRig(t)
			req := launchReq("t1")
			set(&req)
			_, err := rig.launchVia(t.Context(), rig.manager, req)
			if err == nil || !strings.Contains(err.Error(), field) {
				t.Fatalf("a launch whose %s is past a log's calls = %v, want refused naming it", field, err)
			}
			if _, found, getErr := rig.pending.Get(t.Context(), "t1"); getErr != nil || found {
				t.Errorf("the refused launch opened a row (%v, %v)", found, getErr)
			}
		})
	}
	within := BriefTold{Omitted: make([]int, MaxBridgeCalls)}
	if err := (LaunchRequest{Continue: &Continuation{Told: within, Fresh: within}}).toldBounded(); err != nil {
		t.Errorf("a set of a whole log's calls is refused: %v", err)
	}
	if err := (SessionCapture{Omitted: past.Omitted}).Bounded(); !errors.Is(err, ErrCaptureUnbounded) {
		t.Errorf("a capture stating %d reads left out = %v, want ErrCaptureUnbounded", len(past.Omitted), err)
	}
}

// A RECORD READS THE OMITTED CALLS OF THE BRIEF ITS RUN READ. The launch hands
// its run one brief and, beside one that continues a session, the fresh one its
// box falls back to, and only the box says which it read: so the record is read
// by its brief, and a brief that continues a session by the session its box
// said it started. Where the box never said, said it in a word this build does
// not know, or the record names a brief this build does not know, the run read
// one of the two and the record cannot say which: both, told again rather than
// never. A record a build before this one wrote names none.
//
// Mutation: read a continuing brief's set whatever its box said, or a word this
// build does not know as the session it continued, and a row goes red.
func TestARecordReadsTheOmittedCallsOfTheBriefItsRunRead(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		brief BriefKind
		start SessionStart
		want  []int
	}{
		"a fresh brief":                                  {brief: BriefFresh, want: []int{1, 3}},
		"a continued session its box took up":            {brief: BriefContinued, start: SessionStartContinued, want: []int{1, 3}},
		"a continued session its box started its own of": {brief: BriefContinuedNewBox, start: SessionStartFresh, want: []int{2, 3}},
		"a continued session its box never said of":      {brief: BriefContinuedConversation, want: []int{1, 2, 3}},
		"a word this build does not know":                {brief: BriefContinued, start: "a-later-builds-start", want: []int{1, 2, 3}},
		"a brief this build does not know":               {brief: "a-later-builds-brief", start: SessionStartContinued, want: []int{1, 2, 3}},
	} {
		record := LaunchRecord{Brief: tc.brief, SessionStart: tc.start, BriefOmitted: []int{1, 3},
			FreshOmitted: []int{2, 3}}
		if got := record.OmittedCalls(); !slices.Equal(got, tc.want) {
			t.Errorf("%s: the record reads %v left out, want %v", name, got, tc.want)
		}
	}
	if got := (LaunchRecord{}).OmittedCalls(); got != nil {
		t.Errorf("an older build's record reads %v left out, want none", got)
	}
}

// AN OWN-RUN CAPTURE STATES THE READS ITS BRIEF LEFT OUT. A capture of a job
// whose harness ended its own run is placed by its read of the round's log,
// which counts every call the log had taken as one its session holds, the
// reads its job's brief counted without listing them among them, which it holds
// only as that count: so the capture states those, off the job's record by the
// brief its run read, and a relaunch continuing it tells them. A capture placed
// at the newest point its launch is known to hold states that capture's, copied
// with its point, and one placed at the round's start, which holds no read to
// owe, none.
//
// Mutation: place an own-run capture by its read without its job's set, or one
// placed at an earlier capture's point without that capture's, or share that
// capture's set, and a check goes red.
func TestAnOwnRunCaptureStatesTheReadsItsBriefLeftOut(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := keepingRig(t)
		launched := rig.launchUntil("t1", time.Time{})
		// THE JOB'S RECORD AS ITS LAUNCH'S ATTACH WROTE IT: a fresh brief that
		// counted the round's second call without listing it.
		if err := rig.pending.AttachSandbox(t.Context(), "t1", BoxRef{SandboxID: launched.SandboxID,
			CommandID: launched.CommandID, Harness: "claude-code", Brief: BriefFresh, BriefOmitted: []int{2}},
			rigLease); err != nil {
			t.Fatalf("AttachSandbox: %v", err)
		}
		rig.handOver(t, "t1", HandedSecrets{Values: []string{handedToken}})
		rig.coordinator.countRun("swe", StatusRunning)
		for seq := 1; seq <= 3; seq++ {
			if ok, err := rig.pending.AppendBridgeCall(t.Context(), "t1", BridgeAppend{Launch: launched.LaunchID,
				Call: BridgeCall{Name: "read_page", Seq: seq}}); err != nil || !ok {
				t.Fatalf("appending the call at %d: %v, %v", seq, ok, err)
			}
		}
		archive := rig.runner.Session.Archive
		rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-1", HarnessVersion: "2.1.310",
			Captured: &archive})
		rig.resumer.failWith(errors.New("held for the case to read the row"))
		payload, ev := rig.completion("t1")
		if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
			t.Logf("OnCompleted: %v", err)
		}
		rig.awaitKeeps()
		kept := rig.get("t1").Kept
		if kept == nil || kept.State != CaptureLanded || kept.Interrupted {
			t.Fatalf("the row keeps %+v, want the job's own-run capture landed", kept)
		}
		if through, _ := kept.SavePoint(); through != 3 || !slices.Equal(kept.Omitted, []int{2}) {
			t.Errorf("the capture is placed at %d stating %v left out, want 3 and its brief's [2]", through,
				kept.Omitted)
		}
	})

	own := &SessionCapture{LaunchSeq: 2, CaptureSeq: 1, Launch: "job-2", State: CaptureLanded, Through: new(9),
		Omitted: []int{4}}
	cutOff := SessionCapture{Omitted: []int{99}}
	cutOff.placeAtNewest(savePlaces{run: PendingRun{TurnID: "t1", LaunchID: "job-2", LaunchSeq: 2, Kept: own,
		Capture: own}})
	if !slices.Equal(cutOff.Omitted, []int{4}) {
		t.Errorf("a capture placed at its launch's own capture's point states %v left out, want that capture's [4]",
			cutOff.Omitted)
	}
	if cutOff.Omitted[0] = -1; own.Omitted[0] == -1 {
		t.Error("the capture shares its set with the capture it was placed at")
	}
	start := SessionCapture{Omitted: []int{99}}
	start.placeAtNewest(savePlaces{run: PendingRun{TurnID: "t1", LaunchID: "job-2", LaunchSeq: 2}})
	if len(start.Omitted) != 0 {
		t.Errorf("a capture placed at the round's start states %v left out, want none", start.Omitted)
	}
}

// AN OLDER BUILD'S WRITE CARRIES A JOB'S OMITTED CALLS, AND A CAPTURE'S. A fleet
// mid-upgrade rewrites the job's record, and the capture stamped on the row, on
// a build that does not know the fields (a status, a lease, a bridged call),
// and the relaunch that reads them may run on this build: so each rides the
// record under its own key, and a build that does not declare that key keeps
// it verbatim and writes it back (wirekeys.go), whatever it decoded of the
// rest.
//
// Mutation: leave a field off the wire, and no older build's record holds a key
// of it; drop the carried keys from a section's encode (marshalCarrying), and
// the sets are gone after the older build's write.
func TestAnOlderBuildsWriteCarriesAJobsOmittedCalls(t *testing.T) {
	t.Parallel()
	type plainRecord LaunchRecord
	type plainCapture SessionCapture
	for name, tc := range map[string]struct {
		value any
		typ   reflect.Type
		keys  []string
		plain func(raw []byte, older map[string]bool) (written []byte, err error)
		check func(t *testing.T, written []byte)
	}{
		"the job's record": {
			value: LaunchRecord{Model: "m", BriefOmitted: []int{13, 14}, FreshOmitted: []int{15}},
			typ:   reflect.TypeFor[LaunchRecord](), keys: []string{"brief_omitted", "fresh_omitted"},
			plain: func(raw []byte, older map[string]bool) ([]byte, error) {
				var read plainRecord
				extra, err := unmarshalCarrying(raw, &read, older)
				if err != nil {
					return nil, err
				}
				// ITS STRUCT HAS NO SUCH FIELD, so what it writes is the
				// rest and what it carried.
				read.BriefOmitted, read.FreshOmitted, read.Model = nil, nil, "m2"
				return marshalCarrying(read, older, extra)
			},
			check: func(t *testing.T, written []byte) {
				var back LaunchRecord
				if err := json.Unmarshal(written, &back); err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(back.BriefOmitted, []int{13, 14}) || !slices.Equal(back.FreshOmitted, []int{15}) ||
					back.Model != "m2" {
					t.Errorf("after the older build's write the job's record reads %v, %v and model %q: %s",
						back.BriefOmitted, back.FreshOmitted, back.Model, written)
				}
			},
		},
		"a capture": {
			value: SessionCapture{LaunchSeq: 1, CaptureSeq: 1, Through: new(30), Omitted: []int{5, 6}},
			typ:   reflect.TypeFor[SessionCapture](), keys: []string{"bridge_omitted"},
			plain: func(raw []byte, older map[string]bool) ([]byte, error) {
				var read plainCapture
				extra, err := unmarshalCarrying(raw, &read, older)
				if err != nil {
					return nil, err
				}
				read.Omitted, read.CaptureSeq = nil, 2
				return marshalCarrying(read, older, extra)
			},
			check: func(t *testing.T, written []byte) {
				var back SessionCapture
				if err := json.Unmarshal(written, &back); err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(back.Omitted, []int{5, 6}) || back.CaptureSeq != 2 {
					t.Errorf("after the older build's write the capture reads %v at %d: %s", back.Omitted,
						back.CaptureSeq, written)
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			// THE OLDER BUILD'S SECTION: every key this one declares but the
			// sets'.
			declared := wireKeys(tc.typ)
			older := maps.Clone(declared)
			for _, key := range tc.keys {
				if !declared[key] {
					t.Fatalf("the section declares no key %s: %v", key, declared)
				}
				delete(older, key)
			}
			written, err := tc.plain(raw, older)
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, written)
		})
	}
}
