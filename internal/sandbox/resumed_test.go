package sandbox

import (
	"slices"
	"testing"
)

// THE STANDARD WINDOW IS THE HARNESS'S OWN, model by model and route by route,
// as Claude Code 2.1.310 and 2.1.322 hold a run to it where nothing served or
// opted into raises it: claude-opus-4-6 and claude-sonnet-4-6 at 200,000 on every
// route though their pin is 1,000,000; an alias at the window of the model it
// names on the route (`opus` claude-opus-4-6 on Foundry, `sonnet` and `haiku`
// older models off Anthropic's API); no model and `default` at 200,000 on every
// route, the account's default possibly one of those; a route this build does
// not know as the least of every route off Anthropic's. The pin never moves.
//
// Mutation: take the pin for the standard window, or read every route as
// Anthropic's, and a row goes red.
func TestTheStandardWindowIsTheHarnessesOwn(t *testing.T) {
	t.Parallel()
	type row struct {
		model    string
		route    ClaudeRoute
		tokens   int
		standard int
	}
	for _, r := range []row{
		{"claude-opus-4-6", ClaudeRouteAnthropic, 1_000_000, 200_000},
		{"claude-sonnet-4-6", ClaudeRouteBedrock, 1_000_000, 200_000},
		{"claude-opus-4-6[1m]", ClaudeRouteAnthropic, 1_000_000, 1_000_000},
		{"claude-sonnet-4-5", ClaudeRouteAnthropic, 200_000, 200_000},
		{"claude-fable-5-2", ClaudeRouteVertex, 1_000_000, 1_000_000},
		{"claude-mythos-5-2", ClaudeRouteAnthropic, 1_000_000, 1_000_000},
		{"claude-opus-5-5", ClaudeRouteFoundry, 1_000_000, 1_000_000},
		{"opus", ClaudeRouteAnthropic, 1_000_000, 1_000_000},
		{"opus", ClaudeRouteBedrock, 1_000_000, 1_000_000},
		{"opus", ClaudeRouteFoundry, 1_000_000, 200_000},
		{"opus", ClaudeRouteElsewhere, 1_000_000, 200_000},
		{"best", ClaudeRouteVertex, 1_000_000, 1_000_000},
		{"best", ClaudeRouteFoundry, 1_000_000, 200_000},
		{"sonnet", ClaudeRouteAnthropic, 1_000_000, 1_000_000},
		{"sonnet", ClaudeRouteBedrock, 1_000_000, 200_000},
		{"haiku", ClaudeRouteVertex, 1_000_000, 200_000},
		{"fable", ClaudeRouteElsewhere, 1_000_000, 1_000_000},
		{"opusplan", ClaudeRouteAnthropic, 1_000_000, 1_000_000},
		{"opusplan", ClaudeRouteBedrock, 1_000_000, 200_000},
		{"default", ClaudeRouteAnthropic, 1_000_000, 200_000},
		{"", ClaudeRouteBedrock, 1_000_000, 200_000},
		{"sonnet", ClaudeRoute("a-later-builds-route"), 1_000_000, 200_000},
	} {
		got := ClaudeCodeWindowOn(r.model, r.route)
		if got.Tokens != r.tokens || got.Standard != r.standard || !got.Known {
			t.Errorf("ClaudeCodeWindowOn(%q, %s) = %+v, want the pin %d and the standard window %d, known",
				r.model, r.route, got, r.tokens, r.standard)
		}
	}
}

// SENDS WHOLE IS THE HARNESS'S THRESHOLD: Claude Code compacts where its count of
// the conversation, the brief and what the resume attaches reaches its window less
// its reply reserve and its compaction buffer, so a bound whose sum with the
// brief's charge and the turn's attachments is below that point sends whole, and
// one at it does not, the harness compacting where its count is at least the
// threshold. At a 200,000-token window the point is 167,000.
//
// Mutation: compare with <=, leave the brief's charge or the turn's attachments
// out, or judge under the pin, and a case goes red.
func TestSendsWholeIsTheHarnessesThreshold(t *testing.T) {
	t.Parallel()
	w := ClaudeCodeWindowOn("claude-opus-4-6", ClaudeRouteAnthropic)
	const point = 167_000
	if got := CompactionPoint(200_000); got != point {
		t.Fatalf("the point at a 200,000-token window is %d, want %d", got, point)
	}
	brief := "0123456789" // ten bytes, five tokens at ClaudeCharge
	below := &ResumedCount{Tokens: point - claudeTurnAttachments - 5 - 1}
	at := &ResumedCount{Tokens: point - claudeTurnAttachments - 5}
	if !w.SendsWhole(below, brief) {
		t.Errorf("a bound one token below the point with its brief compacts: %+v", below)
	}
	if w.SendsWhole(at, brief) {
		t.Errorf("a bound at the point with its brief sends whole: %+v", at)
	}
	if !w.SendsWhole(at, "") {
		t.Errorf("the same bound with no brief compacts: the brief's charge is not what tips it")
	}
}

// NO BOUND, OR A WINDOW THIS BUILD DOES NOT KNOW, MAY COMPACT: no count, a mark,
// a figure that is no count, an unknown model, and a known window whose standard
// one is not stated each read as a session its harness may compact first,
// whatever the brief.
//
// Mutation: read a mark as a bound of 0, or a standard window of 0 as one, and a
// case goes red.
func TestNoBoundOrAnUnknownWindowMayCompact(t *testing.T) {
	t.Parallel()
	known := ClaudeCodeWindow("claude-opus-5-5")
	for name, tc := range map[string]struct {
		w     ExecutorWindow
		count *ResumedCount
	}{
		"no count":               {w: known},
		"a mark":                 {w: known, count: &ResumedCount{Mark: CountMedia}},
		"a later build's mark":   {w: known, count: &ResumedCount{Mark: "a-later-builds-mark"}},
		"no figure":              {w: known, count: &ResumedCount{}},
		"an unknown model":       {w: ClaudeCodeWindow("claude-next-preview"), count: &ResumedCount{Tokens: 10}},
		"no standard window":     {w: ExecutorWindow{Tokens: 1_000_000, Known: true}, count: &ResumedCount{Tokens: 10}},
		"a zero window, unknown": {count: &ResumedCount{Tokens: 10}},
	} {
		if tc.w.SendsWhole(tc.count, "") {
			t.Errorf("%s: sends whole", name)
		}
	}
}

// A REPORTED WINDOW BELOW THE STANDARD DECIDES: the run that left the copy reported
// holding its model to 200,000 tokens (the account's default, an alias its box
// retargeted, a switch that holds every model there), so a count of 150,000 under
// a standard window of 1,000,000 may compact.
//
// Mutation: ignore the reported window, and this goes red.
func TestAReportedWindowBelowTheStandardDecides(t *testing.T) {
	t.Parallel()
	w := ClaudeCodeWindow("claude-opus-5-5")
	if w.SendsWhole(&ResumedCount{Tokens: 150_000, Window: 200_000}, "") {
		t.Errorf("a count of 150,000 its run reported a 200,000-token window for sends whole under %+v", w)
	}
}

// A REPORTED WINDOW NEVER RAISES THE STANDARD: a figure a box writes can add
// telling and never remove it, so a run that reports 1,000,000 for a model the
// harness holds to 200,000 is judged at 200,000.
//
// Mutation: take the larger of the two, and this goes red.
func TestAReportedWindowNeverRaisesTheStandard(t *testing.T) {
	t.Parallel()
	w := ClaudeCodeWindow("claude-sonnet-4-5")
	if w.SendsWhole(&ResumedCount{Tokens: 150_000, Window: 1_000_000}, "") {
		t.Errorf("a count of 150,000 reported under 1,000,000 sends whole under %+v", w)
	}
}

// EVERY MARK AND ROUTE THIS BUILD WRITES IS VALID, `compacted` among the marks,
// and a word it does not know is not: it reads as a mark all the same.
//
// Mutation: leave a word out of its list, and a case goes red.
func TestEveryCountMarkAndRouteIsValid(t *testing.T) {
	t.Parallel()
	for _, m := range []CountMark{CountUnread, CountRelease, CountPrecompact, CountCompacted, CountUsage,
		CountCleared, CountPDF, CountMedia} {
		if !m.Valid() {
			t.Errorf("%q is not valid", m)
		}
	}
	for _, r := range []ClaudeRoute{ClaudeRouteAnthropic, ClaudeRouteBedrock, ClaudeRouteVertex, ClaudeRouteFoundry,
		ClaudeRouteElsewhere} {
		if !r.Valid() {
			t.Errorf("%q is not valid", r)
		}
	}
	if CountMark("a-later-builds-mark").Valid() || ClaudeRoute("a-later-builds-route").Valid() {
		t.Error("a word this build does not know is valid")
	}
	if !slices.Contains(CountMarks, CountCompacted) {
		t.Error("a compaction with no reply after it is no mark")
	}
}

// THE WORSE OF TWO COUNTS IS THE ONE THAT TELLS MORE: none where either is none,
// since either may be the copy resumed; a mark, the first in precedence, where
// either holds one; else the larger bound, beside the smaller window either
// reported.
//
// Mutation: take the first count's bound, or the larger window, and a case goes
// red.
func TestTheWorseOfTwoCountsIsTheOneThatTellsMore(t *testing.T) {
	t.Parallel()
	small, large := &ResumedCount{Tokens: 10_000, Window: 1_000_000}, &ResumedCount{Tokens: 160_000, Window: 200_000}
	if got := small.Worse(large); got == nil || got.Tokens != 160_000 || got.Window != 200_000 || got.Mark != "" {
		t.Errorf("the worse of %+v and %+v is %+v, want 160,000 under 200,000", small, large, got)
	}
	marked := &ResumedCount{Mark: CountMedia}
	if got := large.Worse(marked); got == nil || got.Mark != CountMedia || got.Tokens != 0 {
		t.Errorf("the worse of a bound and a mark is %+v, want the mark", got)
	}
	if got := (&ResumedCount{Mark: CountMedia}).Worse(&ResumedCount{Mark: CountUnread}); got.Mark != CountUnread {
		t.Errorf("the worse of two marks is %q, want the first in precedence", got.Mark)
	}
	if got := small.Worse(nil); got != nil {
		t.Errorf("the worse of a count and none is %+v, want none", got)
	}
}

// A JOB THAT RESTARTED ITS SESSION IS KNOWN BY ITS NEWEST RESTART'S IDENTITY: the
// newest place its thread restarts at, as a whole read of the job's copy finds
// it, against the one the job's launch handed it. A compaction the job made moves
// it; a trim that kept the newest restart does not, however many older ones it
// dropped; a fresh session that restarted, and a basis not known where the
// thread restarts anywhere, have restarted; a count marked for what its bytes
// cannot bound is read by its restart as a bound is, since the mark says nothing
// of where the thread restarts; and a count that did not read the thread whole
// cannot say, which is read as restarted.
//
// Mutation: drop the comparison, compare a count of restarts, read an unread
// count as not restarted, or a marked one as unread, and a case goes red.
func TestAJobThatRestartedItsSessionIsKnownByItsNewestRestart(t *testing.T) {
	t.Parallel()
	read := func(restart string) *ResumedCount { return &ResumedCount{Tokens: 1, Restart: new(restart)} }
	marked := func(restart string) *ResumedCount { return &ResumedCount{Mark: CountMedia, Restart: new(restart)} }
	for name, tc := range map[string]struct {
		basis *string
		count *ResumedCount
		want  bool
	}{
		"a job that compacted":                {basis: new("u-1"), count: read("u-9"), want: true},
		"a trim with no restart of the job's": {basis: new("u-1"), count: read("u-1")},
		"a fresh session that restarted":      {basis: new(""), count: read("u-9"), want: true},
		"a fresh session that did not":        {basis: new(""), count: read("")},
		"a basis not known, a restart":        {count: read("u-9"), want: true},
		"a basis not known, none":             {count: read("")},
		"a marked count, the restart handed":  {basis: new("u-1"), count: marked("u-1")},
		"a marked count, a restart of its own": {basis: new("u-1"), count: marked("u-9"),
			want: true},
		"a count that did not read the whole": {basis: new("u-1"), count: &ResumedCount{Mark: CountUnread},
			want: true},
		"no count": {basis: new("u-1"), want: true},
	} {
		if got := RestartedSince(tc.basis, tc.count); got != tc.want {
			t.Errorf("%s: restarted %v, want %v", name, got, tc.want)
		}
	}
}
