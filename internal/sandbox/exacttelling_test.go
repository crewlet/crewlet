package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
)

// A BRIEF TOLD BY A READING ([ExactTelling]) is told only the calls a reading
// of what the kept capture holds did not find it holding, read for the release
// that wrote the session, since the relaunch's box is acquired after its briefs
// are written. So the launch hands it only to a box whose harness answers that
// release before its brief is chosen, and the brief told by the save point,
// which tells more and never less, to every other box.

// The briefs a relaunch's continuation carries in these cases, each told apart:
// told by the save point, and told by a reading, for the box the session was
// last worked in and for any other.
const (
	savePointSameBox = "carry on in the box you left"
	savePointNewBox  = "carry on in a box that replaced yours"
	readingSameBox   = "carry on in the box you left, told only what your session lacks"
	readingNewBox    = "carry on in a box that replaced yours, told only what your session lacks"
	// readFor is the release the reading was read for: the one that wrote
	// the session, as [keptRow] records it.
	readFor = "2.1.310"
)

// toldBySavePoint and toldByReading are what each telling left out of the round
// and opened with, told apart.
var (
	toldBySavePoint = BriefTold{Omitted: []int{5}, Own: []Lead{answerLead("told by the save point")}}
	toldByReading   = BriefTold{Omitted: []int{3}, Own: []Lead{answerLead("told by the reading")}}
)

// exactRelaunch is a relaunch of t1 continuing its kept capture, its harness
// cut off where interrupted, in a box whose harness answers version: the rig,
// the run's record as the launch opened the relaunch's job, and the request,
// its continuation carrying the briefs told by the save point and, beside them,
// by a reading read for [readFor].
func exactRelaunch(t *testing.T, interrupted bool, version string) (*coordRig, PendingRun, LaunchRequest) {
	t.Helper()
	rig := newCoordRig(t)
	rig.coordinator.spoolDir = t.TempDir()
	rig.runner.Carries, rig.runner.CarryVersion = true, version
	key := objstore.KeyAt(time.Now())
	kept := SessionCapture{LaunchSeq: 1, CaptureSeq: 1, State: CaptureLanded, Final: true,
		Interrupted: interrupted, Through: new(0), Object: key.String(),
		ObjectHash: strings.Repeat("a", 64), ObjectSize: 14}
	rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{"t1": keptRow(t, kept)},
		archives: map[string][]byte{key.String(): []byte("the transcript")}}
	opened := PendingRun{TurnID: "t1", AgentHandle: "swe", LaunchSeq: 2, Capture: &kept, Kept: &kept}
	req := continuingKept(kept)
	req.Session.Told = toldBySavePoint
	req.Session.Exact = &ExactTelling{Resumer: readFor, Brief: readingSameBox, Replaced: readingNewBox,
		Told: toldByReading}
	return rig, opened, req
}

// unusedLines is every `executor_session_holdings_unused` line logs holds, its
// quoted values' escaped quotes read back as the quotes they are.
func unusedLines(logs *syncBuffer) []string {
	var lines []string
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, "msg=executor_session_holdings_unused") {
			lines = append(lines, strings.ReplaceAll(line, `\"`, `"`))
		}
	}
	return lines
}

// A BRIEF TOLD BY A READING IS HANDED ONLY TO A BOX THAT ANSWERS ITS RELEASE.
// A session holds what the release that resumes it hands its model, so a box
// whose harness answers the release the reading was read for gets the reading's
// brief (same-box where last worked in, else new-box), its told set riding the
// launch; a box answering another release (even the archive writer's) or an
// unreadable one gets the save point's brief, which tells more, and the launch
// says why once, naming both releases. The session continues either way.
// Mutation: compare the reading's release with the archive's writer, hand the
// reading's brief whatever the box answers, record the save point's told set
// beside it, or log nothing: red.
//
// Not parallel: it captures the process-wide logger ([captureLogs]).
func TestABriefToldByAReadingIsHandedOnlyToABoxThatAnswersItsRelease(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reused  bool
		answers string
		brief   string
		told    BriefTold
		why     string
	}{
		{name: "its own box, of the release read for", reused: true, answers: readFor, brief: readingSameBox,
			told: toldByReading},
		{name: "a new box, of the release read for", answers: readFor, brief: readingNewBox, told: toldByReading},
		{name: "its own box, of another release", reused: true, answers: "2.1.322", brief: savePointSameBox,
			told: toldBySavePoint, why: `the box's harness answered "2.1.322", and the reading was read for "2.1.310"`},
		{name: "a new box, of another release", answers: "2.1.322", brief: savePointNewBox,
			told: toldBySavePoint, why: `the box's harness answered "2.1.322", and the reading was read for "2.1.310"`},
		{name: "a new box whose release is unread", answers: "", brief: savePointNewBox, told: toldBySavePoint,
			why: `answered no release this build reads, and the reading was read for "2.1.310"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			rig, opened, req := exactRelaunch(t, true, tc.answers)
			got := rig.coordinator.continueSession(t.Context(), req, opened, NewFakeSandbox("box-1"),
				rig.runner, tc.reused, false)
			t.Cleanup(got.release)
			kind := BriefContinuedNewBox
			if tc.reused {
				kind = BriefContinued
			}
			if got.kind != kind || got.brief != tc.brief || got.session != "s-1" || got.carried == nil {
				t.Fatalf("the launch hands a %s brief %q, session %q, carrying %v; want a %s brief %q continuing "+
					"the kept session", got.kind, got.brief, got.session, got.carried != nil, kind, tc.brief)
			}
			if !slices.Equal(got.told.Omitted, tc.told.Omitted) || !slices.Equal(got.told.Own, tc.told.Own) {
				t.Errorf("the launch records %+v told, want %+v: the told set of the brief it hands", got.told, tc.told)
			}
			if want := tc.why == ""; got.exact != want || got.decision().Exact != want {
				t.Errorf("the launch says it handed the brief told by a reading: %v, %v; want %v", got.exact,
					got.decision().Exact, want)
			}
			lines := unusedLines(logs)
			switch {
			case tc.why == "" && len(lines) != 0:
				t.Errorf("a launch that handed the brief told by the reading logged %q", lines)
			case tc.why != "" && (len(lines) != 1 || !strings.Contains(lines[0], tc.why) ||
				!strings.Contains(lines[0], exactUnusedOutcome) || !strings.Contains(lines[0], "turn_id=t1") ||
				!strings.Contains(lines[0], "agent=swe")):
				t.Errorf("the launch logged %q; want one line naming t1, swe, why (%q) and what it hands", lines,
					tc.why)
			}
		})
	}
}

// A READING OF A CAPTURE THE BOX'S COPY WENT PAST IS SAID UNUSED. In its job's
// own box, a kept capture taken while the job still worked is passed over for
// the box's copy, which went on past it ([copyWentPast]), so a brief told by a
// reading of it is not handed, and the launch says so once, naming the copy.
// Said only where the copy is what the launch continues; unsaid where no
// reading was carried, and in another box, where the capture is restored.
//
// Mutation: log nothing for the copy, or log it with no reading or where the
// launch starts fresh, and a case goes red.
//
// Not parallel: it captures the process-wide logger ([captureLogs]).
func TestAReadingOfACaptureTheBoxsCopyWentPastIsSaidUnused(t *testing.T) {
	const copyBrief = "carry the box's copy on"
	copyTold := BriefTold{Omitted: []int{7}}
	for _, tc := range []struct {
		name string
		// reused is the job's own box; read carries a brief told by a
		// reading beside the continuation; ranOut marks the copy's session
		// one its job ran out of context in.
		reused, read, ranOut bool
		kind                 BriefKind
		brief, session       string
		told                 BriefTold
		// exact is the brief told by the reading handed; unused, the
		// reading said unused.
		exact, unused bool
	}{
		{name: "its own box, the continuation read", reused: true, read: true, kind: BriefContinued,
			brief: copyBrief, session: "s-1", told: copyTold, unused: true},
		{name: "its own box, nothing read", reused: true, kind: BriefContinued, brief: copyBrief,
			session: "s-1", told: copyTold},
		{name: "its own box, the copy's session ran out", reused: true, read: true, ranOut: true,
			kind: BriefFresh, brief: "the fresh brief"},
		{name: "a new box, the continuation read", read: true, kind: BriefContinuedNewBox,
			brief: readingNewBox, session: "s-1", told: toldByReading, exact: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			rig, opened, req := exactRelaunch(t, true, readFor)
			// A SAVE: taken while its job still worked, of the session the
			// box's copy is.
			save := *opened.Kept
			save.Final, save.SessionID = false, "s-1"
			opened.Capture, opened.Kept = &save, &save
			if tc.ranOut {
				opened.ExhaustedSessions = []string{"s-1"}
			}
			if !tc.read {
				req.Session.Exact = nil
			}
			req.Continue = &Continuation{SessionID: "s-1", Brief: copyBrief, Told: copyTold}
			got := rig.coordinator.continueSession(t.Context(), req, opened, NewFakeSandbox("box-1"),
				rig.runner, tc.reused, false)
			t.Cleanup(got.release)
			if got.kind != tc.kind || got.brief != tc.brief || got.session != tc.session || got.exact != tc.exact {
				t.Fatalf("the launch hands a %s brief %q on session %q (by a reading: %v); want a %s brief %q on "+
					"%q (%v)", got.kind, got.brief, got.session, got.exact, tc.kind, tc.brief, tc.session, tc.exact)
			}
			if !slices.Equal(got.told.Omitted, tc.told.Omitted) {
				t.Errorf("the launch records %v left out, want %v: the told set of the brief it hands",
					got.told.Omitted, tc.told.Omitted)
			}
			lines := unusedLines(logs)
			switch {
			case !tc.unused && len(lines) != 0:
				t.Errorf("the launch logged %q, want no reading said unused", lines)
			case tc.unused && (len(lines) != 1 || !strings.Contains(lines[0], copyWentPastUnused) ||
				!strings.Contains(lines[0], exactUnusedOutcome) || !strings.Contains(lines[0], "turn_id=t1") ||
				!strings.Contains(lines[0], "agent=swe")):
				t.Errorf("the launch logged %q; want one line naming t1, swe, the copy it continues (%q) and "+
					"what it hands", lines, copyWentPastUnused)
			}
		})
	}
}

// AN EMPTY BRIEF TOLD BY A READING FALLS BACK TO THE BRIEF WITHOUT. A telling
// may write no brief for the box the launch got; that is no reason to refuse a
// session the brief told by the save point continues: the launch hands that
// brief, records its told set, and says why it did not hand the reading's.
//
// Mutation: refuse the session where the brief told by the reading for its box
// is empty, or hand the empty brief, and a case goes red.
//
// Not parallel: it captures the process-wide logger ([captureLogs]).
func TestAnEmptyBriefToldByAReadingFallsBackToTheBriefWithout(t *testing.T) {
	for _, reused := range []bool{true, false} {
		name := map[bool]string{true: "its own box", false: "a new box"}[reused]
		t.Run(name, func(t *testing.T) {
			logs := captureLogs(t)
			rig, opened, req := exactRelaunch(t, true, readFor)
			req.Session.Exact.Brief, req.Session.Exact.Replaced = "", ""
			got := rig.coordinator.continueSession(t.Context(), req, opened, NewFakeSandbox("box-1"),
				rig.runner, reused, false)
			t.Cleanup(got.release)
			want := map[bool]string{true: savePointSameBox, false: savePointNewBox}[reused]
			if got.brief != want || got.session != "s-1" || got.exact ||
				!slices.Equal(got.told.Omitted, toldBySavePoint.Omitted) {
				t.Fatalf("the launch hands brief %q for session %q (by a reading: %v), %v left out; want %q for "+
					"the kept session, by the save point, %v left out", got.brief, got.session, got.exact,
					got.told.Omitted, want, toldBySavePoint.Omitted)
			}
			if lines := unusedLines(logs); len(lines) != 1 ||
				!strings.Contains(lines[0], "the launch was given no brief told by the reading for this box") {
				t.Errorf("the launch logged %q, want one line saying it had no brief told by the reading", lines)
			}
		})
	}
}

// A KEPT SESSION THE BOX STILL HOLDS RECORDS WHAT ITS BRIEF LEFT OUT. Where the
// job before ran in this very box and its harness ended its own run, the box's
// own copy is continued without asking its harness anything: no release was
// asked, and no reading is read of such a capture, so the brief told by the save
// point is handed, and the launch still records what that brief left out and
// opened with, as every continuation does, and logs nothing of a reading.
//
// Mutation: hand the brief told by a reading on this path, or set the told
// record only where the session is fetched, and this goes red.
//
// Not parallel: it captures the process-wide logger ([captureLogs]).
func TestAKeptSessionTheBoxStillHoldsRecordsWhatItsBriefLeftOut(t *testing.T) {
	logs := captureLogs(t)
	rig, opened, req := exactRelaunch(t, false, readFor)
	req.Continue = &Continuation{Fresh: BriefTold{Omitted: []int{2}}}
	got := rig.coordinator.continueSession(t.Context(), req, opened, NewFakeSandbox("box-1"),
		rig.runner, true, false)
	t.Cleanup(got.release)
	if got.kind != BriefContinued || got.brief != savePointSameBox || got.carried != nil || got.exact {
		t.Fatalf("the launch hands a %s brief %q (by a reading: %v), carrying %v; want the box's own copy "+
			"continued with the brief told by the save point", got.kind, got.brief, got.exact, got.carried != nil)
	}
	brief, fresh := got.omitted(req)
	if !slices.Equal(brief.Omitted, toldBySavePoint.Omitted) || !slices.Equal(fresh.Omitted, []int{2}) {
		t.Errorf("the job records %v left out beside the fresh brief's %v, want %v beside [2]", brief.Omitted,
			fresh.Omitted, toldBySavePoint.Omitted)
	}
	if !slices.Equal(got.told.Own, toldBySavePoint.Own) {
		t.Errorf("the job seals %+v as its brief's own leads, want %+v", got.told.Own, toldBySavePoint.Own)
	}
	if lines := unusedLines(logs); len(lines) != 0 {
		t.Errorf("a launch that asked no release logged %q", lines)
	}
}

// A JOB RECORDS WHAT THE BRIEF IT WAS HANDED LEFT OUT. Which of the briefs a
// relaunch hands its run is decided once the box has answered its release, so
// the job's record follows the brief handed: the reads the brief told by a
// reading left out, and the leads it opened with sealed for the job and marked
// told, where the box answers the release the reading was read for; the save
// point's where it answers another.
//
// Mutation: record the save point's told set whatever brief is handed, and the
// first case goes red; the reading's whatever, and the second.
func TestAJobRecordsWhatTheBriefItWasHandedLeftOut(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		answers string
		told    BriefTold
	}{
		"a box of the release read for": {answers: readFor, told: toldByReading},
		"a box of another release":      {answers: "2.1.322", told: toldBySavePoint},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, restore, kept := keptTurn(t)
				rig.runner.CarryVersion = tc.answers
				req := continuingKept(kept)
				req.Session.Told = toldBySavePoint
				req.Session.Exact = &ExactTelling{Resumer: readFor, Brief: readingSameBox, Replaced: readingNewBox,
					Told: toldByReading}
				req.Continue = &Continuation{Fresh: BriefTold{Omitted: []int{2}}}
				if _, err := launchRestoring(t, rig, restore, req); err != nil {
					t.Fatalf("relaunch: %v", err)
				}
				got := rig.get("t1")
				if !slices.Equal(got.Launch.BriefOmitted, tc.told.Omitted) ||
					!slices.Equal(got.Launch.FreshOmitted, []int{2}) {
					t.Errorf("the job records %v left out beside the fresh brief's %v, want %v beside [2]",
						got.Launch.BriefOmitted, got.Launch.FreshOmitted, tc.told.Omitted)
				}
				read, err := rig.pending.Leads(t.Context(), got)
				if err != nil {
					t.Fatalf("reading the run's leads: %v", err)
				}
				if len(read.Kept) != 1 || !slices.Equal(got.Launch.LeadMarks, []int{read.Kept[0].Appended}) {
					t.Fatalf("the run keeps %+v, the job marking %v; want the brief's one lead, marked",
						read.Kept, got.Launch.LeadMarks)
				}
				own, err := rigLeads().Open("t1", read.Kept[0])
				if want := tc.told.Own[0].Answer.Words; err != nil || own.Job != got.LaunchID ||
					own.Answer == nil || own.Answer.Words != want {
					t.Errorf("the job's lead is %+v, %v; want %q sealed for job %s", own, err, want, got.LaunchID)
				}
			})
		})
	}
}

// A LAUNCH SAYS WHETHER IT HANDED THE BRIEF TOLD BY A READING. Its caller arms
// the run's guard and publishes the prompt by the brief the run read, so the
// launch's decision says whether that brief is the one told by a reading, and
// the run is started with it: in a box of the release read for, it is; in any
// other, it is not, and the run starts on the brief told by the save point.
//
// Mutation: leave Exact out of the decision, or start the run on the brief told
// by the save point where the decision says otherwise, and a case goes red.
func TestALaunchSaysWhetherItHandedTheBriefToldByAReading(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		answers string
		exact   bool
		brief   string
	}{
		"a box of the release read for": {answers: readFor, exact: true, brief: readingNewBox},
		"a box of another release":      {answers: "2.1.322", brief: savePointNewBox},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, restore, kept := keptTurn(t)
				rig.runner.CarryVersion = tc.answers
				req := continuingKept(kept)
				req.Session.Exact = &ExactTelling{Resumer: readFor, Brief: readingSameBox, Replaced: readingNewBox}
				var decided []LaunchDecision
				req.Decided = func(d LaunchDecision) { decided = append(decided, d) }
				if _, err := launchRestoring(t, rig, restore, req); err != nil {
					t.Fatalf("relaunch: %v", err)
				}
				want := LaunchDecision{Brief: BriefContinuedNewBox, Kept: true, Exact: tc.exact}
				if len(decided) != 1 || decided[0] != want {
					t.Errorf("the launch's caller was told %+v, want once %+v", decided, want)
				}
				started := rig.runner.Started()
				if last := started[len(started)-1]; last.Brief != tc.brief || last.Resume != "s-1" {
					t.Errorf("the run started on brief %q continuing %q, want %q continuing the kept session",
						last.Brief, last.Resume, tc.brief)
				}
			})
		})
	}
}

// A LAUNCH REFUSES A BRIEF TOLD BY A READING THAT LEFT OUT MORE THAN A LOG
// LISTS. The brief told by a reading tells at most a round's log's calls, as
// every brief does, so its told set is held to [MaxBridgeCalls] as the others
// are: past it, the launch is refused naming the field, before its row is
// written; a whole log's calls pass.
//
// Mutation: leave the told set of the brief told by a reading out of the bound,
// and this goes red.
func TestALaunchRefusesABriefToldByAReadingThatLeftOutMoreThanALog(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	req := launchReq("t1")
	req.Session = &SessionContinuation{LaunchSeq: 1, CaptureSeq: 1, Brief: "b", Replaced: "r",
		Exact: &ExactTelling{Resumer: readFor, Brief: "b", Replaced: "r",
			Told: BriefTold{Omitted: make([]int, MaxBridgeCalls+1)}}}
	_, err := rig.launchVia(t.Context(), rig.manager, req)
	if err == nil || !strings.Contains(err.Error(), "session.exact.told") {
		t.Fatalf("a launch whose brief told by a reading left out more than a log lists = %v, want refused "+
			"naming session.exact.told", err)
	}
	if _, found, getErr := rig.pending.Get(t.Context(), "t1"); getErr != nil || found {
		t.Errorf("the refused launch opened a row (%v, %v)", found, getErr)
	}
	req.Session.Exact.Told.Omitted = make([]int, MaxBridgeCalls)
	if err := req.toldBounded(); err != nil {
		t.Errorf("a brief told by a reading that left out a whole log's calls is refused: %v", err)
	}
}

// THE FAKE RUNNER READS HOLDINGS AS SCRIPTED. A case reads a session's holdings
// through the real [Coordinator.HoldingsOf] on a [FakeRunner]: it records every
// ask; unscripted, it answers no reading and opens nothing; scripted, it opens
// the archive once, reads it to its end, closes it, and answers the script of
// the archive and the ask, or the open's failure. And its harness answers its
// release whichever way it answers whether it continues a session, as a real
// runner reads the release first.
//
// Mutation: answer the script before reading the stream to its end, leave the
// stream open, record no ask, or answer the release only where the session is
// continued, and a check goes red.
func TestTheFakeRunnerReadsHoldingsAsScripted(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	ask := HoldingsAsk{Resumer: readFor, Calls: []LoggedCall{{ID: "toolu_1", Asked: true, Output: "ok"}}}
	archive := SessionArchive{Harness: "claude-code", SessionID: "s-1", Format: FakeArchiveFormat}

	runner := NewFakeRunner("claude-code")
	opens := 0
	opener := func(context.Context) (io.ReadCloser, error) {
		opens++
		return io.NopCloser(strings.NewReader("never read")), nil
	}
	if got, err := runner.ReadHoldings(ctx, archive, opener, ask); err == nil || got.Read || opens != 0 {
		t.Errorf("an unscripted reading answered %+v, %v, opening %d; want no reading, nothing opened", got, err,
			opens)
	}

	body := &closeSpy{Reader: bytes.NewReader([]byte("the session's archive"))}
	var readFrom SessionArchive
	runner.Holdings = func(a SessionArchive, asked HoldingsAsk) (SessionHoldings, error) {
		readFrom = a
		if body.Len() != 0 || !body.closed {
			return SessionHoldings{}, errors.New("answered before the stream was read to its end and closed")
		}
		return readingOf(map[string]CallRecords{asked.Calls[0].ID: recordsOf("ok", false)}), nil
	}
	got, err := runner.ReadHoldings(ctx, archive, func(context.Context) (io.ReadCloser, error) {
		return body, nil
	}, ask)
	if err != nil || !got.Read || readFrom != archive || !body.closed {
		t.Fatalf("a scripted reading answered %+v, %v, of %+v, the stream closed %v; want the script's reading "+
			"of the archive, its stream closed", got, err, readFrom, body.closed)
	}
	if held, _ := got.Holds(ask.Calls[0]); !held {
		t.Errorf("the scripted reading holds no call it scripted held")
	}
	failed := errors.New("the object store is down")
	if _, err := runner.ReadHoldings(ctx, archive, func(context.Context) (io.ReadCloser, error) {
		return nil, failed
	}, ask); !errors.Is(err, failed) {
		t.Errorf("a reading whose open failed answered %v, want the open's failure", err)
	}
	if asks := runner.HoldingsAsks(); len(asks) != 3 || asks[2].Resumer != readFor {
		t.Errorf("the runner recorded %+v asks, want each of the three", asks)
	}

	runner.Carries, runner.CarryVersion = true, readFor
	if check, err := runner.CheckCarry(ctx, nil, archive); err != nil || !check.Continues ||
		check.Version != readFor {
		t.Errorf("a box that continues the session answered %+v, %v; want it continuing, on %s", check, err,
			readFor)
	}
	runner.CarryWhy = "this box runs another harness's login"
	if check, _ := runner.CheckCarry(ctx, nil, archive); check.Continues || check.Version != readFor {
		t.Errorf("a box that cannot continue the session answered %+v, want its release beside why", check)
	}
	if check, _ := runner.CheckCarry(ctx, nil, SessionArchive{Harness: "opencode"}); check.Continues ||
		check.Version != readFor {
		t.Errorf("a box asked of a session it does not restore answered %+v, want its release beside why", check)
	}
}

// closeSpy is a stream that says whether it was closed.
type closeSpy struct {
	*bytes.Reader
	closed bool
}

func (c *closeSpy) Close() error {
	c.closed = true
	return nil
}
