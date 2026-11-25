package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A HAND-OVER IS HELD TO HALF THE ROOM BELOW THE CHAIN'S LEAST WINDOW: half of
// what its compaction point leaves beside the executor floor, at four bytes a
// token; the fallback window where no member states one; nothing where the
// floor leaves no room; and never past what a summary is kept at.
//
// Mutation: hand the whole room, read an unknown window as none, or let the
// bound pass the kept one, and a case goes red.
func TestAHandOverIsHeldToHalfTheRoomBelowTheWindow(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		window sandbox.ExecutorWindow
		floor  int
		want   int
	}{
		"a stated window": {window: sandbox.ExecutorWindow{Standard: 200_000, Reserve: 20_000, Buffer: 13_000,
			Known: true}, floor: 25_000, want: (200_000 - 20_000 - 13_000 - 25_000) / 2 * 4},
		"no member states one":  {floor: 25_000, want: (handOverFallbackWindow - 25_000) / 2 * 4},
		"a floor past the room": {window: sandbox.ExecutorWindow{Standard: 20_000, Known: true}, floor: 25_000},
		"a window past the kept bound": {window: sandbox.ExecutorWindow{Standard: 10_000_000, Known: true},
			floor: 25_000, want: executorkeep.SummaryMaxBytes},
	} {
		if got := handOverBytes(c.window, c.floor); got != c.want {
			t.Errorf("%s: the hand-over holds %d bytes, want %d", name, got, c.want)
		}
	}
}

// summaries is a [summarySource] holding each turn's row's summary, at a row
// naming the capture (1, 1), and its text.
type summaries struct {
	rows    map[string]*executorkeep.Summary
	texts   map[string]string
	rowErr  error
	readErr error
	asked   []string
}

func (s *summaries) lostRow(_ context.Context, turn string) (*executorkeep.Summary, executorkeep.CapturePlace,
	error) {
	s.asked = append(s.asked, turn)
	return s.rows[turn], executorkeep.CapturePlace{LaunchSeq: 1, CaptureSeq: 1}, s.rowErr
}

func (s *summaries) readSummary(_ context.Context, turn string, _ executorkeep.Summary) (string, error) {
	return s.texts[turn], s.readErr
}

// A RUN IS HANDED THE LOST SESSION'S SUMMARY BY ITS END. The verdict's session
// names it: the run's own turn's record where its own session was lost, the
// lost turn's row otherwise, which `read_session` then reads. Past the bound
// its end is handed, marked cut; one kept and not read is said unread, never an
// error; none, or no verdict, hands nothing. A continuation of an older capture
// of the same turn is told it newer only where its place is later.
//
// Mutation: hand the own turn's for another turn's loss, hand its opening past
// the bound, fail on an unread one, or call one at the same place newer, and a
// case goes red.
func TestARunIsHandedTheLostSessionsSummaryByItsEnd(t *testing.T) {
	t.Parallel()
	window := sandbox.ExecutorWindow{Standard: 1_000, Known: true}
	const floor = 200 // (1,000 - 200) / 2 tokens: 1,600 bytes
	at := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	summary := func(launch, capture int64, cut bool) *executorkeep.Summary {
		return &executorkeep.Summary{At: at, Bytes: 9_000, Cut: cut,
			Place: executorkeep.CapturePlace{LaunchSeq: launch, CaptureSeq: capture}}
	}
	long := strings.Repeat("a", 400) + strings.Repeat("z", 1_600)
	lost := func(loss executorkeep.Loss, lostTurn string) *executorkeep.NotContinued {
		n := executorkeep.Lost(loss, executorkeep.InsteadFresh, "")
		n.LostTurn = lostTurn
		return n
	}
	olderCapture := executorkeep.Lost(executorkeep.LossCaptureLate, executorkeep.InsteadOlder, "")
	olderCapture.Older = &executorkeep.CapturePlace{LaunchSeq: 1, CaptureSeq: 2}
	headLoss := lost(executorkeep.LossTooLarge, "t-head")
	headLoss.Instead, headLoss.OlderTurn = executorkeep.InsteadOlder, "t-head"
	for name, c := range map[string]struct {
		verdict *executorkeep.NotContinued
		own     *executorkeep.Summary
		src     summaries
		unread  bool
		want    *runner.LostSummary
	}{
		"the turn's own, past the bound": {verdict: lost(executorkeep.LossTooLarge, ""),
			own: summary(1, 3, false), src: summaries{texts: map[string]string{"t-run": long}},
			want: &runner.LostSummary{Turn: "t-run", Cut: true, Newer: true}},
		"another turn's, whole": {verdict: lost(executorkeep.LossBoxLost, "t-lost"), own: summary(1, 3, false),
			src: summaries{rows: map[string]*executorkeep.Summary{"t-lost": summary(2, 1, true)},
				texts: map[string]string{"t-lost": "## Next\nship it", "t-run": long}},
			want: &runner.LostSummary{Text: "## Next\nship it", Turn: "t-lost", Cut: true, KeptCut: true,
				Readable: true, Newer: true}},
		"a later capture than the one continued": {verdict: olderCapture, own: summary(1, 3, false),
			src:  summaries{texts: map[string]string{"t-run": "later"}},
			want: &runner.LostSummary{Text: "later", Turn: "t-run", Newer: true}},
		"found at the capture continued": {verdict: olderCapture, own: summary(1, 2, false),
			src:  summaries{texts: map[string]string{"t-run": "same"}},
			want: &runner.LostSummary{Text: "same", Turn: "t-run"}},
		"the head's own newer capture lost": {verdict: headLoss,
			src: summaries{rows: map[string]*executorkeep.Summary{"t-head": summary(1, 1, false)},
				texts: map[string]string{"t-head": "head"}},
			want: &runner.LostSummary{Text: "head", Turn: "t-head", Readable: true}},
		"a row that could not be read": {verdict: lost(executorkeep.LossBoxLost, "t-lost"),
			src: summaries{rowErr: errors.New("no data node answered")}, unread: true},
		"a summary that could not be read": {verdict: lost(executorkeep.LossTooLarge, ""), own: summary(1, 3, false),
			src: summaries{readErr: errors.New("the object is gone")}, unread: true},
		"none kept":  {verdict: lost(executorkeep.LossBoxLost, "t-lost")},
		"no verdict": {own: summary(1, 3, false)},
	} {
		basis := runner.SessionBasis{NotContinued: c.verdict, Window: window}
		src := c.src
		got, err := handedSummary(t.Context(), &src, basis, c.own, "t-run", floor)
		if got.SummaryUnread != c.unread || (err != nil) != c.unread {
			t.Errorf("%s: unread %v (%v), want %v", name, got.SummaryUnread, err, c.unread)
		}
		switch s := got.Summary; {
		case c.want == nil && s != nil:
			t.Errorf("%s: handed %+v, want none", name, s)
		case c.want == nil:
		case s == nil:
			t.Errorf("%s: handed none, want %+v", name, c.want)
		case s.Turn != c.want.Turn || s.Cut != c.want.Cut || s.KeptCut != c.want.KeptCut ||
			s.Readable != c.want.Readable || s.Newer != c.want.Newer || !s.At.Equal(at):
			t.Errorf("%s: handed %+v, want %+v", name, s, c.want)
		case c.want.Text != "" && s.Text != c.want.Text:
			t.Errorf("%s: handed %q, want %q", name, s.Text, c.want.Text)
		case c.want.Text == "" && (len(s.Text) > 1_600 || !strings.HasSuffix(s.Text, "zzz") ||
			strings.Contains(s.Text, "a")):
			t.Errorf("%s: handed %d bytes, want the end of the summary within 1,600", name, len(s.Text))
		}
		if c.verdict == nil && len(src.asked) != 0 {
			t.Errorf("%s: read rows %v for a launch that lost nothing", name, src.asked)
		}
	}
}

// A FLOOR THAT DROPS THE SESSION TELLS THE FRESH BRIEF AGAIN UNDER ITS OWN
// VERDICT: a launch whose seat cannot carry the session, or whose member runs
// another harness, hands its run the fresh brief told under that loss, with
// what it hands of the summary; one that still offers the box's own copy hands
// the brief as built, which its box tells at its decision.
//
// Mutation: keep the brief built before the drop, or tell it again while a
// copy is still offered, and a case goes red.
func TestAFloorDropTellsTheFreshBriefAgain(t *testing.T) {
	t.Parallel()
	seat := &org.Role{Name: "Engineer", DeclaredHandle: "swe"}
	l := &agentLauncher{engine: &Engine{}, seat: seat, turn: &turnctx.Turn{RunID: "run-1", Seat: seat}}
	member := harnessProvider(t, chainMember{key: "head", agent: executor.ClaudeCodeName, model: "claude-haiku-4-5"})
	tell := func(v *executorkeep.NotContinued) sandbox.FreshBrief {
		return sandbox.FreshBrief{Text: "told under " + string(v.Loss), Handover: sandbox.HandoverHanded}
	}
	for name, c := range map[string]struct {
		harness string
		context int64
		inPlace bool
		brief   string
	}{
		"the seat cannot carry it":   {harness: executor.ClaudeCodeName, context: 600_000, brief: "told under budget_floor"},
		"its member runs another":    {harness: executor.CodexName, context: 1_000, brief: "told under harness_changed"},
		"the box's copy still stays": {harness: executor.CodexName, context: 1_000, inPlace: true, brief: "as built"},
	} {
		req := runner.AgentRunRequest{Brief: "as built", Handover: sandbox.HandoverNone, FreshFor: tell,
			Session: &runner.SessionContinuation{Brief: "carry on", Basis: runner.SessionBasis{LaunchSeq: 1,
				CaptureSeq: 1, SessionID: "s-1", Harness: c.harness, ContextTokens: c.context}}}
		if c.inPlace {
			req.Continue = &runner.Continuation{SessionID: "s-copy", Brief: "carry on in place"}
		}
		got, _ := l.continuedFloor(t.Context(), req, member, true)
		if got.Session != nil || got.Brief != c.brief ||
			(c.brief != "as built") != (got.Handover == sandbox.HandoverHanded) {
			t.Errorf("%s: the launch offers %v and hands %q (%s), want no session and %q", name, got.Session,
				got.Brief, got.Handover, c.brief)
		}
	}
}
