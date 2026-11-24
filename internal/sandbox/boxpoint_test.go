package sandbox

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// AN OPENCODE CONTINUATION'S FIRST REQUEST IS JUDGED IN ITS BOX. OpenCode
// compacts a session before a request where its count of it with the brief is
// at least the point its box sets for the run's model, which only the box can
// say, so the runner hands the launch both tellings and the launch hands the
// held one only where the box's point proves it whole.

// pointAt is a box's point at tokens, the engine's own pair's 170,000 where
// the cases say nothing else: OpenCode 2.3.2's on a 200,000-token limit,
// anchored on its provider as OpenCode's is.
func pointAt(tokens int64) BoxPoint {
	return BoxPoint{Release: "2.3.2", Tokens: tokens, Charge: 4, Anchored: true}
}

// briefOf is a brief of n bytes.
func briefOf(n int) string { return strings.Repeat("b", n) }

// OPENCODE SENDS WHOLE STRICTLY BELOW THE POINT. It compacts where its count is
// at least the point, and the count is at most the bound; the brief is charged
// at a quarter of its bytes rounded up, never under OpenCode's own rounded
// estimate. The engine's pair: an 8,001-byte brief is 2,001 tokens, so at a
// point of 170,000 a bound of 167,998 sends whole and 167,999 does not;
// equality is not whole. Compaction off sends whole whatever the count; a
// marked point sends nothing whole.
//
// Mutation: compare with <= rather than <, round the brief down, or read a
// marked point's Tokens, and a case goes red.
func TestOpenCodeSendsWholeStrictlyBelowThePoint(t *testing.T) {
	t.Parallel()
	brief := briefOf(8001)
	for _, tc := range []struct {
		name  string
		point BoxPoint
		bound int64
		want  bool
	}{
		{name: "one token under, with the brief", point: pointAt(170000), bound: 167998, want: true},
		{name: "at the point, with the brief", point: pointAt(170000), bound: 167999},
		{name: "compaction off, far past", point: BoxPoint{Release: "2.3.2", Never: true}, bound: 900000,
			want: true},
		{name: "a marked point, far under", point: BoxPoint{Tokens: 170000, Mark: PointDefaulted}, bound: 10},
		{name: "a point at or below zero", point: pointAt(0), bound: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			count := &ResumedCount{Tokens: tc.bound}
			if got := tc.point.SendsWhole(count, "stub", "stub", brief); got != tc.want {
				t.Errorf("SendsWhole(bound %d, brief of 8,001 bytes) at %+v = %v, want %v", tc.bound, tc.point,
					got, tc.want)
			}
		})
	}
	if got := Charge(4).Tokens(8001); got != 2001 {
		t.Errorf("8,001 bytes at four a token = %d, want 2001", got)
	}
}

// ANOTHER PROVIDER'S COUNT IS NO BOUND. OpenCode anchors its count on a reply
// of the run's current provider alone, so a count anchored on another
// provider's reply, or on a provider nobody recorded, bounds nothing: it may
// compact, however far under the point. A count with no bound (a mark, none
// at all) is the same.
//
// Mutation: drop the provider comparison, or read "" as matching, and a case
// goes red.
func TestAnotherProvidersCountIsNoBound(t *testing.T) {
	t.Parallel()
	point := pointAt(170000)
	for _, tc := range []struct {
		name               string
		count              *ResumedCount
		counted, launching string
		want               bool
	}{
		{name: "the launch's own provider", count: &ResumedCount{Tokens: 1000}, counted: "stub",
			launching: "stub", want: true},
		{name: "another provider's reply", count: &ResumedCount{Tokens: 1000}, counted: "anthropic",
			launching: "stub"},
		{name: "no provider recorded", count: &ResumedCount{Tokens: 1000}, launching: "stub"},
		{name: "a marked count", count: &ResumedCount{Tokens: 1000, Mark: CountUnanchored}, counted: "stub",
			launching: "stub"},
		{name: "no count", counted: "stub", launching: "stub"},
	} {
		if got := point.SendsWhole(tc.count, tc.counted, tc.launching, "brief"); got != tc.want {
			t.Errorf("%s: SendsWhole = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// opencodeKept is [exactRelaunch]'s relaunch with the summarized telling beside
// the held one, the count its launch judges by, and the launch's model: what a
// runner hands a launch whose OpenCode session's first request is the box's to
// judge. The point is read by the rig's runner ([FakeRunner.Point]).
func opencodeKept(t *testing.T, interrupted bool, version string) (*coordRig, PendingRun, LaunchRequest) {
	t.Helper()
	rig, opened, req := exactRelaunch(t, interrupted, version)
	req.Model = "stub/model"
	req.Session.Exact = nil
	req.Session.Summarized = &SummarizedTelling{Brief: summarizedSameBox, Replaced: summarizedNewBox,
		Told: toldAsSummary}
	req.Session.Count, req.Session.CountProvider = &ResumedCount{Tokens: 100000}, "stub"
	return rig, opened, req
}

// The summarized telling's briefs and what it told, told apart from the held
// one's.
const (
	summarizedSameBox = "carry on in the box you left, told as a session that may be a summary"
	summarizedNewBox  = "carry on in a box that replaced yours, told as a session that may be a summary"
)

var toldAsSummary = BriefTold{Omitted: []int{7}, Own: []Lead{answerLead("told as a summary")}}

// mayCompactLines is every launch's `executor_session_may_compact` line logs holds.
func mayCompactLines(logs *syncBuffer) []string {
	var lines []string
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, "msg=executor_session_may_compact") {
			lines = append(lines, line)
		}
	}
	return lines
}

// AN OPENCODE CONTINUATION THAT MAY COMPACT IS HANDED THE SUMMARIZED BRIEF.
// Where the box's point does not prove the held brief whole, the launch hands
// the summarized telling's brief for its box, records its told set, says so in
// its decision (so the run's guard arms nothing), and logs
// executor_session_may_compact once; where it does, the held one, and nothing
// is logged. The same on the in-place path, the copy's own box.
//
// Mutation: hand the held brief whatever the point, record the held telling's
// told set beside the summarized brief, leave the decision's Summarized
// false, or log nothing, and a case goes red.
//
// Not parallel: it captures the process-wide logger ([captureLogs]).
func TestAnOpenCodeContinuationThatMayCompactIsHandedTheSummarizedBrief(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reused bool
		point  int64
		brief  string
		told   BriefTold
		summed bool
	}{
		{name: "a new box whose point the held brief reaches", point: 100007, brief: summarizedNewBox,
			told: toldAsSummary, summed: true},
		{name: "its own box whose point the held brief reaches", reused: true, point: 100007,
			brief: summarizedSameBox, told: toldAsSummary, summed: true},
		{name: "a new box whose point is far past", point: 170000, brief: savePointNewBox,
			told: toldBySavePoint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			rig, opened, req := opencodeKept(t, true, "2.3.2")
			rig.runner.Point = func(PointAsk) (BoxPoint, error) { return pointAt(tc.point), nil }
			got := rig.coordinator.continueSession(t.Context(), req, opened, NewFakeSandbox("box-1"),
				rig.runner, tc.reused, false)
			t.Cleanup(got.release)
			if got.brief != tc.brief || !sameTold(got.told, tc.told) || got.decision().Summarized != tc.summed {
				t.Fatalf("the launch hands %q telling %+v, summarized %v; want %q telling %+v, summarized %v",
					got.brief, got.told, got.decision().Summarized, tc.brief, tc.told, tc.summed)
			}
			lines := mayCompactLines(logs)
			if !tc.summed {
				if len(lines) != 0 {
					t.Errorf("a launch handing the held brief logged %q", lines)
				}
				return
			}
			if len(lines) != 1 || !strings.Contains(lines[0], "path=kept") ||
				!strings.Contains(lines[0], "point=100007") {
				t.Errorf("the launch logged %q, want one may-compact line naming the kept path and its point",
					lines)
			}
		})
	}
	// THE COPY IN ITS OWN BOX, in a company that keeps no session: no capture
	// names the copy's writer, and the job before's record does.
	t.Run("the copy in its own box", func(t *testing.T) {
		logs := captureLogs(t)
		rig := newCoordRig(t)
		rig.runner.Point = func(PointAsk) (BoxPoint, error) { return pointAt(100007), nil }
		req := launchReq("t1")
		req.Model = "stub/model"
		req.Continue = &Continuation{SessionID: "sess-own", Brief: savePointSameBox, Told: toldBySavePoint,
			Summarized: summarizedSameBox, SummarizedTold: toldAsSummary,
			Count: &ResumedCount{Tokens: 100000}, CountProvider: "stub"}
		opened := PendingRun{TurnID: "t1", AgentHandle: "swe", LaunchSeq: 2, Prior: PriorBox{Writer: "2.3.2"}}
		got := rig.coordinator.continueSession(t.Context(), req, opened, NewFakeSandbox("box-1"), rig.runner,
			true, false)
		t.Cleanup(got.release)
		if got.brief != summarizedSameBox || !sameTold(got.told, toldAsSummary) || !got.decision().Summarized {
			t.Fatalf("the copy is handed %q telling %+v, summarized %v; want the summarized brief",
				got.brief, got.told, got.decision().Summarized)
		}
		asks := rig.runner.PointAsks()
		if len(asks) != 1 || asks[0].Writer != "2.3.2" || asks[0].Release != "" {
			t.Errorf("the copy's box was asked %+v, want one ask of the writer the job before's record "+
				"names, in its box, its release asked", asks)
		}
		if lines := mayCompactLines(logs); len(lines) != 1 || !strings.Contains(lines[0], "path=in_place") {
			t.Errorf("the launch logged %q, want one may-compact line naming the in-place path", lines)
		}
	})
}

// sameTold reports whether a and b told the same of the round.
func sameTold(a, b BriefTold) bool { return reflect.DeepEqual(a, b) }

// THE OWN COPY IS READ TOO. A kept capture its harness ended in this very box
// is continued without asking the box whether it carries the session
// ([SessionRestorer.CheckCarry]), but its first request is judged all the
// same: the box is asked its point, its release asked with it, and the
// writer's release beside the ask.
//
// Mutation: read the point only after CheckCarry, and a case goes red.
func TestTheOwnCopyIsReadToo(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		interrupted bool
		reused      bool
		release     string
	}{
		{name: "its own box, the harness ended there", reused: true},
		{name: "its own box, the copy replaced", interrupted: true, reused: true, release: "2.3.2"},
		{name: "a new box", interrupted: true, release: "2.3.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig, opened, req := opencodeKept(t, tc.interrupted, "2.3.2")
			rig.runner.Point = func(PointAsk) (BoxPoint, error) { return pointAt(170000), nil }
			got := rig.coordinator.continueSession(t.Context(), req, opened, NewFakeSandbox("box-1"),
				rig.runner, tc.reused, false)
			t.Cleanup(got.release)
			asks := rig.runner.PointAsks()
			if len(asks) != 1 {
				t.Fatalf("the box was asked its point %d times, want once", len(asks))
			}
			ask := asks[0]
			if ask.Release != tc.release || ask.Writer != "2.1.310" || ask.Model != "stub/model" {
				t.Errorf("the box was asked %+v, want release %q of writer 2.1.310 for stub/model", ask,
					tc.release)
			}
		})
	}
}

// WHAT THE BOX HANDS OVER BESIDE THE BRIEF IS CHARGED. What the box adds to
// the first request beside the brief (BoxBytes: Gemini CLI's session context,
// Qwen Code's dated reminder) is charged with it, at the harness's rate: a
// brief that sends whole with nothing handed over may compact with it.
//
// Mutation: leave BoxBytes out of SendsWhole, and this goes red.
func TestWhatTheBoxHandsOverBesideTheBriefIsCharged(t *testing.T) {
	t.Parallel()
	count := &ResumedCount{Tokens: 160000}
	brief := briefOf(4000)
	if !pointAt(170000).SendsWhole(count, "stub", "stub", brief) {
		t.Fatal("a session 9,000 tokens under its point is not sent whole")
	}
	point := pointAt(170000)
	point.BoxBytes = 4 * 9000
	if point.SendsWhole(count, "stub", "stub", brief) {
		t.Error("a session whose box hands 9,000 tokens over beside the brief is sent whole")
	}
}

// A FAILED READ HANDS THE SUMMARIZED BRIEF. No point is no proof: a box that
// could not be asked, a runner that reads no point, and every mark are read
// as a session that may compact, and the summarized brief is handed.
//
// Mutation: read no point as whole, and a case goes red.
func TestAFailedReadHandsTheSummarizedBrief(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		point func(PointAsk) (BoxPoint, error)
	}{
		{name: "the box could not be asked", point: func(PointAsk) (BoxPoint, error) {
			return BoxPoint{}, errors.New("the box is gone")
		}},
		{name: "a mark", point: func(PointAsk) (BoxPoint, error) {
			return BoxPoint{Tokens: 900000, Mark: PointDefaulted}, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig, opened, req := opencodeKept(t, true, "2.3.2")
			rig.runner.Point = tc.point
			got := rig.coordinator.continueSession(t.Context(), req, opened, NewFakeSandbox("box-1"),
				rig.runner, false, false)
			t.Cleanup(got.release)
			if got.brief != summarizedNewBox || !got.decision().Summarized {
				t.Errorf("the launch hands %q, want the summarized brief", got.brief)
			}
		})
	}
	t.Run("a runner that reads none", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		req := launchReq("t1")
		req.Model = "stub/model"
		req.Continue = &Continuation{SessionID: "sess-own", Brief: savePointSameBox, Told: toldBySavePoint,
			Summarized: summarizedSameBox, SummarizedTold: toldAsSummary,
			Count: &ResumedCount{Tokens: 1000}, CountProvider: "stub"}
		// UNDER THE ENTRY ITS JOB RAN UNDER, which a runner wrapped to its
		// interface alone, with no route of its own measured, holds it to.
		req.Entry = "entry-a"
		opened := PendingRun{TurnID: "t1", AgentHandle: "swe", LaunchSeq: 2,
			Prior: PriorBox{Writer: "2.3.2", Entry: "entry-a"}}
		// ITS RUNNER INTERFACE ALONE, so no read of a point is offered.
		got := rig.coordinator.continueSession(t.Context(), req, opened, NewFakeSandbox("box-1"),
			struct{ Runner }{rig.runner}, true, false)
		t.Cleanup(got.release)
		if got.brief != summarizedSameBox || !got.decision().Summarized || len(rig.runner.PointAsks()) != 0 {
			t.Errorf("the launch hands %q after %d asks, want the summarized brief and none", got.brief,
				len(rig.runner.PointAsks()))
		}
	})
}

// AN OPENCODE TELLING BY A READING IS JUDGED ON ITS OWN BRIEF. A brief told by
// a reading drops held calls and may add sections of its own, so it is
// ordered against neither default: the box's point is asked of it, and it is
// handed only where it sends whole; where it does not, the held brief is
// judged on its own and handed where it does, and executor_session_holdings_unused
// says why the reading's was not.
//
// Mutation: judge the reading's brief by the held one's verdict, and a case
// goes red.
//
// Not parallel: it captures the process-wide logger ([captureLogs]).
func TestAnOpenCodeTellingByAReadingIsJudgedOnItsOwnBrief(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reading string
		want    string
		unused  bool
	}{
		{name: "a short reading's brief", reading: readingNewBox, want: readingNewBox},
		{name: "a reading's brief past the point", reading: briefOf(4 * 20000), want: savePointNewBox, unused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			rig, opened, req := opencodeKept(t, true, readFor)
			req.Session.Exact = &ExactTelling{Resumer: readFor, Brief: tc.reading, Replaced: tc.reading,
				Told: toldByReading}
			rig.runner.Point = func(PointAsk) (BoxPoint, error) { return pointAt(110000), nil }
			got := rig.coordinator.continueSession(t.Context(), req, opened, NewFakeSandbox("box-1"),
				rig.runner, false, false)
			t.Cleanup(got.release)
			if got.brief != tc.want || got.decision().Summarized {
				t.Fatalf("the launch hands %q (summarized %v), want %q", got.brief, got.decision().Summarized,
					tc.want)
			}
			lines := unusedLines(logs)
			if tc.unused != (len(lines) == 1 && strings.Contains(lines[0], exactMayCompact)) {
				t.Errorf("the launch logged %q, want a line saying the reading's brief may compact: %v", lines,
					tc.unused)
			}
		})
	}
}

// A SUMMARIZED TELL IS BOUNDED. The summarized tellings' told sets are held to
// a round's log's calls as every brief's are ([MaxBridgeCalls]): past it the
// launch is refused naming the set; a whole log's calls pass.
//
// Mutation: leave either summarized set out of the bound, and a case goes red.
func TestASummarizedTellIsBounded(t *testing.T) {
	t.Parallel()
	past := BriefTold{Omitted: make([]int, MaxBridgeCalls+1)}
	within := BriefTold{Omitted: make([]int, MaxBridgeCalls)}
	for _, tc := range []struct {
		field string
		req   func(BriefTold) LaunchRequest
	}{
		{field: "session.summarized.told", req: func(told BriefTold) LaunchRequest {
			return LaunchRequest{Session: &SessionContinuation{Summarized: &SummarizedTelling{Told: told}}}
		}},
		{field: "continue.summarized", req: func(told BriefTold) LaunchRequest {
			return LaunchRequest{Continue: &Continuation{SummarizedTold: told}}
		}},
	} {
		if err := tc.req(past).toldBounded(); err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("a %s past a log's calls = %v, want refused naming it", tc.field, err)
		}
		if err := tc.req(within).toldBounded(); err != nil {
			t.Errorf("a %s of a whole log's calls is refused: %v", tc.field, err)
		}
	}
}

// AN UNANCHORED POINT IGNORES THE PROVIDER: a Gemini CLI or Qwen Code count
// names no provider and is that harness's whatever the launch's model, so it
// is judged against the point with none named; an anchored point (OpenCode's)
// still bounds nothing by a count of no provider.
//
// Mutation: keep the provider check for every point, and the first row goes
// red; drop it for an anchored one, and the second does.
func TestAnUnanchoredPointIgnoresTheProvider(t *testing.T) {
	t.Parallel()
	count := &ResumedCount{Tokens: 1000}
	for name, tc := range map[string]struct {
		point BoxPoint
		want  bool
	}{
		"unanchored": {point: BoxPoint{Tokens: 167000, Charge: 1}, want: true},
		"anchored":   {point: BoxPoint{Tokens: 167000, Charge: 1, Anchored: true}},
	} {
		if got := tc.point.SendsWhole(count, "", "anthropic", "Continue."); got != tc.want {
			t.Errorf("%s: a count of 1,000 tokens naming no provider sends whole at %+v: %v, want %v", name,
				tc.point, got, tc.want)
		}
	}
}
