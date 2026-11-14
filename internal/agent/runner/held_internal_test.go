package runner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/toolargs"
)

// closeIssue is an MCP-shaped tool at Needs approval that counts its calls and
// names its definition.
type closeIssue struct {
	digest string
	calls  []map[string]any
}

func (c *closeIssue) Name() string               { return "tr_close_issue" }
func (c *closeIssue) Description() string        { return "Close an issue." }
func (c *closeIssue) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (c *closeIssue) Digest() string             { return c.digest }
func (c *closeIssue) Call(_ context.Context, args map[string]any) (tools.Result, error) {
	c.calls = append(c.calls, args)
	return tools.Result{Output: "closed #7"}, nil
}

// ledgerCommit is a [HeldCommit] over one row's mark, as the store keeps it:
// marked once, the result recorded once, with what the surface answered. Each
// attempt's claim reads the row as it stands when the attempt asks.
type ledgerCommit struct {
	marked   bool
	result   *sandbox.HeldResult
	answered toolloop.ToolResult
	refuse   error
	recorded int
	// unseen is a mark the attempt's claim did not read: [ledgerCommit.Marked]
	// answers the call unmarked, and [ledgerCommit.Begin] finds it marked.
	unseen bool
	// unread is a result recorded that the node the attempt runs on cannot
	// open: the call is answered made, with whether it failed alone.
	unread bool
}

// prior is how the row holds the call once some attempt marked it.
func (l *ledgerCommit) prior() HeldPrior {
	switch {
	case l.result != nil && l.unread:
		return HeldPrior{Made: true, Unread: true, Result: sandbox.HeldResult{Failed: l.result.Failed}}
	case l.result != nil:
		return HeldPrior{Made: true, Result: *l.result}
	}
	return HeldPrior{}
}

func (l *ledgerCommit) Marked(context.Context) (HeldPrior, bool) {
	if !l.marked || l.unseen {
		return HeldPrior{}, false
	}
	return l.prior(), true
}

func (l *ledgerCommit) Begin(context.Context) (HeldPrior, error) {
	if l.refuse != nil {
		return HeldPrior{}, l.refuse
	}
	if !l.marked {
		l.marked = true
		return HeldPrior{First: true}, nil
	}
	return l.prior(), nil
}

func (l *ledgerCommit) Record(_ context.Context, result sandbox.HeldResult,
	answered toolloop.ToolResult) (sandbox.HeldKept, error) {

	if l.result == nil {
		l.result, l.answered = &result, answered
	}
	l.recorded++
	return sandbox.HeldKept{}, nil
}

// heldSurface is an executor's surface holding tool, filed at Needs approval.
func heldSurface(t *testing.T, tool *closeIssue) *tools.Surface {
	t.Helper()
	reg := tools.NewRegistry()
	if err := reg.RegisterWith(tool, tools.Origin("tracker"), tools.Annotations{},
		func(e *tools.Entry) { e.NeedsApproval = true }); err != nil {
		t.Fatal(err)
	}
	return tools.NewSurface("execute", reg.Snapshot(), nil)
}

// A HELD CALL RUNS ONCE AFTER ONCE, AND NEVER AFTER DENY. The call a person
// allowed is made through the surface the run carries on with, with the
// arguments they approved, and its result handed to the run; every later
// attempt at the same answer finds it marked and hands over the result the
// first recorded, or, where the first stopped before recording one, says the
// call may or may not have taken effect. Neither makes it again. A denied call
// is never made, and the run is told it was refused as forbidden.
//
// THE RECORD IS HANDED WHAT THE SURFACE ANSWERED, beside the result: the call
// is recorded among its round's calls as the tool that made it answered it
// ([HeldCommit.Record]), and every reader of whether the turn replied reads it
// there.
//
// AND THE MARK IS WHAT DECIDES IT, not the claim's read of the row: a mark the
// attempt's claim did not see, which the store's answer to the mark reports as
// not the first, is answered from as any other.
//
// Mutation: make the call before [HeldCommit.Begin] marks it, or on a mark that
// is not the first, and the mark the claim missed has the tool called twice;
// hand the record anything but the surface's own answer, and the call is
// recorded as nobody's.
func TestAHeldCallRunsOnceAfterOnceAndNeverAfterDeny(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	tool := &closeIssue{digest: "sha256:d1"}
	surface := heldSurface(t, tool)
	commit := &ledgerCommit{}
	held := func(choice string) *HeldCall {
		return &HeldCall{Tool: "tr_close_issue", Server: "tracker", Digest: "sha256:d1",
			Args: map[string]any{"id": "7"}, Choice: choice, Commit: commit}
	}

	first := held(sandbox.HeldOnce)
	if err := makeHeld(ctx, surface, first); err != nil {
		t.Fatal(err)
	}
	if len(tool.calls) != 1 || tool.calls[0]["id"] != "7" || commit.recorded != 1 {
		t.Fatalf("the approved call ran %d time(s) with %v, recorded %d, want once with the approved "+
			"arguments", len(tool.calls), tool.calls, commit.recorded)
	}
	if got := commit.answered; got.Origin != tools.Origin("tracker") || got.Failed ||
		!strings.Contains(got.Output, "closed #7") {
		t.Fatalf("the record was handed %+v of the call, want what the tracker's tool answered", got)
	}
	told := Asked{Held: first}.Continuation(provenance.NewBoundary())
	if !strings.Contains(told, "allowed it this once") || !strings.Contains(told, "closed #7") {
		t.Fatalf("the run is told %q, want the answer and what the call returned", told)
	}

	again := held(sandbox.HeldOnce)
	if err := makeHeld(ctx, surface, again); err != nil {
		t.Fatal(err)
	}
	if len(tool.calls) != 1 {
		t.Fatalf("a second attempt at the same answer made the call again (%d calls)", len(tool.calls))
	}
	if told := (Asked{Held: again}).Reseed(provenance.NewBoundary()); !strings.Contains(told, "closed #7") {
		t.Fatalf("a later attempt tells a fresh run %q, want the first attempt's result", told)
	}

	lost := &ledgerCommit{marked: true}
	unknown := &HeldCall{Tool: "tr_close_issue", Server: "tracker", Digest: "sha256:d1",
		Choice: sandbox.HeldAlways, Commit: lost}
	if err := makeHeld(ctx, surface, unknown); err != nil {
		t.Fatal(err)
	}
	if len(tool.calls) != 1 ||
		!strings.Contains(Asked{Held: unknown}.Continuation(provenance.NewBoundary()), "may or may not have taken effect") {
		t.Fatalf("a call an earlier attempt marked and recorded nothing for was made again or not said: %d calls",
			len(tool.calls))
	}

	for told, missed := range map[string]*ledgerCommit{
		"closed #7":                        {marked: true, unseen: true, result: &sandbox.HeldResult{Output: "closed #7"}},
		"may or may not have taken effect": {marked: true, unseen: true},
	} {
		late := &HeldCall{Tool: "tr_close_issue", Server: "tracker", Digest: "sha256:d1",
			Args: map[string]any{"id": "7"}, Choice: sandbox.HeldOnce, Commit: missed}
		if err := makeHeld(ctx, surface, late); err != nil {
			t.Fatal(err)
		}
		if got := (Asked{Held: late}).Continuation(provenance.NewBoundary()); len(tool.calls) != 1 ||
			!strings.Contains(got, told) {
			t.Fatalf("a mark the claim missed left %d calls and told the run %q, want the call not made "+
				"again and %q said", len(tool.calls), got, told)
		}
	}

	denied := held(sandbox.HeldDeny)
	denied.Commit = &ledgerCommit{}
	if err := makeHeld(ctx, surface, denied); err != nil {
		t.Fatal(err)
	}
	if len(tool.calls) != 1 || denied.Commit.(*ledgerCommit).marked {
		t.Fatal("a denied call was made, or marked as being made")
	}
	if told := (Asked{Held: denied}).Continuation(provenance.NewBoundary()); !strings.Contains(told, "forbidden") {
		t.Fatalf("a denied call tells the run %q, want it refused as forbidden", told)
	}
}

// A CALL IS MADE ONLY AS IT WAS APPROVED: a tool whose definition changed
// since it was held, one no longer filed, or arguments that did not open are
// never made, and nothing is marked, so the run may call again and ask afresh.
// A mark that cannot be written is the resume's to retry, with nothing made.
//
// Mutation: drop the digest check, and the changed tool is called.
func TestAHeldCallIsMadeOnlyAsItWasApproved(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	tool := &closeIssue{digest: "sha256:d2"}
	surface := heldSurface(t, tool)
	for name, held := range map[string]*HeldCall{
		"its definition changed": {Tool: "tr_close_issue", Digest: "sha256:d1"},
		"it is no longer filed":  {Tool: "tr_delete_issue", Digest: "sha256:d2"},
		"its arguments did not open": {Tool: "tr_close_issue", Digest: "sha256:d2",
			Unopened: "its arguments could not be opened"},
	} {
		commit := &ledgerCommit{}
		held.Choice, held.Commit, held.Server = sandbox.HeldOnce, commit, "tracker"
		if err := makeHeld(ctx, surface, held); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(tool.calls) != 0 || commit.marked {
			t.Fatalf("%s: the call was made or marked", name)
		}
		if told := (Asked{Held: held}).Continuation(provenance.NewBoundary()); !strings.Contains(told, "was not made") {
			t.Fatalf("%s: the run is told %q, want the call not made", name, told)
		}
	}
	refused := &HeldCall{Tool: "tr_close_issue", Digest: "sha256:d2", Choice: sandbox.HeldOnce,
		Commit: &ledgerCommit{refuse: errors.New("the store is down")}}
	if err := makeHeld(ctx, surface, refused); err == nil || len(tool.calls) != 0 {
		t.Fatalf("a mark that could not be written = %v with %d calls, want an error and no call",
			err, len(tool.calls))
	}
}

// holdsBack is a skill guard that holds tool back, as a required skill the
// session has not loaded holds back every tool it covers.
type holdsBack struct{ tool string }

func (g holdsBack) Check(tool, _ string) string {
	if tool == g.tool {
		return "load the tool skill `tracker-rules` before calling `" + tool + "`."
	}
	return ""
}

func (holdsBack) Observe(string, toolargs.Args) {}

// A CALL AN EARLIER ATTEMPT MARKED IS ANSWERED FROM ITS MARK, WHATEVER CHANGED
// SINCE. An attempt at an answer can make the call and be refused after it, at
// the launch (the floor of the session it continues, every member sitting
// out), and the answer is attempted again later, through a surface rebuilt
// from the live tools and skills: a skill made required in between, the tool
// blocked or its definition changed, or arguments this node cannot open. None
// of that is asked of a call already marked. The later attempt hands the run
// the result the first recorded, or, where the first stopped before it
// recorded one, says the call may or may not have taken effect. It never says
// the call was not made, which sends the run to ask the person again for a
// call that was made, and it never makes it again.
//
// Mutation: ask the arguments' seal, the tool's lookup, its digest or the guard
// before the mark answers, and the case that changed it tells the run the call
// was not made.
func TestACallAnEarlierAttemptMarkedIsAnsweredFromItsMark(t *testing.T) {
	t.Parallel()
	for name, since := range map[string]func(t *testing.T, tool *closeIssue, held *HeldCall) *tools.Surface{
		"a skill now holds the tool back": func(t *testing.T, tool *closeIssue, _ *HeldCall) *tools.Surface {
			return heldSurface(t, tool).WithGuard(holdsBack{tool: "tr_close_issue"})
		},
		"its definition changed": func(t *testing.T, _ *closeIssue, _ *HeldCall) *tools.Surface {
			return heldSurface(t, &closeIssue{digest: "sha256:d2"})
		},
		"it is no longer filed": func(*testing.T, *closeIssue, *HeldCall) *tools.Surface {
			return tools.NewSurface("execute", tools.NewRegistry().Snapshot(), nil)
		},
		"its arguments do not open here": func(t *testing.T, tool *closeIssue, held *HeldCall) *tools.Surface {
			held.Unopened = "its arguments could not be opened"
			return heldSurface(t, tool)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			tool := &closeIssue{digest: "sha256:d1"}
			attempt := func(commit *ledgerCommit) *HeldCall {
				return &HeldCall{Tool: "tr_close_issue", Server: "tracker", Digest: "sha256:d1",
					Args: map[string]any{"id": "7"}, Choice: sandbox.HeldOnce, Commit: commit}
			}
			// THE FIRST ATTEMPT MAKES THE CALL, as nothing has changed yet, and
			// records what it returned.
			recorded := &ledgerCommit{}
			if err := makeHeld(ctx, heldSurface(t, tool), attempt(recorded)); err != nil {
				t.Fatal(err)
			}
			for told, commit := range map[string]*ledgerCommit{
				"closed #7": recorded,
				// A FIRST ATTEMPT STOPPED once it marked the call, before it
				// recorded anything.
				"may or may not have taken effect": {marked: true},
			} {
				again := attempt(commit)
				if err := makeHeld(ctx, since(t, tool, again), again); err != nil {
					t.Fatal(err)
				}
				got := Asked{Held: again}.Continuation(provenance.NewBoundary())
				if !strings.Contains(got, told) || strings.Contains(got, "was not made") {
					t.Errorf("a later attempt tells the run %q, want %q, as the mark holds it", got, told)
				}
			}
			if len(tool.calls) != 1 {
				t.Fatalf("the call was made %d time(s) over every attempt, want once", len(tool.calls))
			}
		})
	}
}

// ONLY THE EXECUTOR'S SURFACE HOLDS A CALL FOR APPROVAL (ADR-0077): it alone
// has a run whose row the call parks on, so every other phase's surface is
// built with no holder and refuses the call as forbidden.
//
// Mutation: hand the holder to every phase, and this is red.
func TestOnlyTheExecutorsSurfaceHoldsACallForApproval(t *testing.T) {
	t.Parallel()
	hold := holdNothing{}
	r := &Runner{cfg: Config{Hold: hold}}
	for _, ph := range phase.All {
		got := r.holderFor(ph)
		if ph == phase.Execute && got != tools.Holder(hold) {
			t.Errorf("the executor's surface holds with %v, want the runner's holder", got)
		}
		if ph != phase.Execute && got != nil {
			t.Errorf("the %s phase's surface holds with %v, want none", ph, got)
		}
	}
	if (&Runner{}).holderFor(phase.Execute) != nil {
		t.Error("a runner built with no holder gave the executor's surface one")
	}
}

// holdNothing is a [tools.Holder] for a test that only asks where one goes.
type holdNothing struct{}

func (holdNothing) Hold(context.Context, *turnctx.Turn, tools.HeldCall) (tools.Result, bool) {
	return tools.Result{}, false
}

// AN APPROVED CALL WHOSE OUTPUT NO RECORD KEEPS HANDS IT TO THE RUN ALONE: the
// run is told what the tool printed, and the row's held result keeps its size
// and digest, which a later attempt is told in its place.
//
// Mutation: record the result as the run is told it, and the row keeps what a
// server printed.
func TestAnApprovedCallsUnkeptOutputReachesTheRunAlone(t *testing.T) {
	t.Parallel()
	tool := &closeIssue{digest: "sha256:d1"}
	reg := tools.NewRegistry()
	if err := reg.RegisterWith(tool, tools.Origin("tracker"), tools.Annotations{},
		func(e *tools.Entry) { e.NeedsApproval = true }, tools.KeepsNoOutput); err != nil {
		t.Fatal(err)
	}
	surface := tools.NewSurface("execute", reg.Snapshot(), nil)
	commit := &ledgerCommit{}
	held := &HeldCall{Tool: "tr_close_issue", Server: "tracker", Digest: "sha256:d1",
		Args: map[string]any{"id": "7"}, Choice: sandbox.HeldOnce, Commit: commit}
	if err := makeHeld(t.Context(), surface, held); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(held.outcome.result.Output, "closed #7") {
		t.Fatalf("the run is told %q, want what the tool printed", held.outcome.result.Output)
	}
	if commit.result == nil || strings.Contains(commit.result.Output, "closed #7") ||
		!strings.Contains(commit.result.Output, "output not kept") {
		t.Fatalf("the row keeps %+v, want the output's size and digest alone", commit.result)
	}
}

// A MADE CALL WHOSE RESULT CANNOT BE READ HERE IS TOLD AS MADE. An earlier
// attempt at the answer made the call a person allowed and recorded what it
// returned, and the node carrying the run on holds no key that record was
// sealed under. The run is told the call was made, and whether the tool
// reported a failure, which is what the round's log lists of the call — never
// that the node making it stopped before its result was recorded, which it did
// not, and which sends the run to doubt a call it need not. A later attempt
// tells the call by this lead alone: no list of the round's calls in the brief
// it leads tells it again. And it is not made again.
//
// Mutation: tell an unreadable result as no result recorded, or as a result
// that returned nothing, and a case goes red.
func TestAMadeCallWhoseResultCannotBeReadHereIsToldAsMade(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		failed bool
		said   string
	}{
		"the tool reported success":   {said: "reported success, so it ALREADY RAN"},
		"the tool reported a failure": {failed: true, said: "reported a failure"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tool := &closeIssue{digest: "sha256:d1"}
			held := &HeldCall{Tool: "tr_close_issue", Server: "tracker", Digest: "sha256:d1",
				Choice: sandbox.HeldOnce, Commit: &ledgerCommit{marked: true, unread: true,
					result: &sandbox.HeldResult{Failed: tc.failed}}}
			if err := makeHeld(t.Context(), heldSurface(t, tool), held); err != nil {
				t.Fatal(err)
			}
			got := Asked{Held: held}.Continuation(provenance.NewBoundary())
			if !strings.Contains(got, "The call was made, and the tool "+tc.said) ||
				!strings.Contains(got, "cannot be read on the node carrying this run on") ||
				strings.Contains(got, "may or may not have taken effect") {
				t.Errorf("a made call whose result cannot be read here is told:\n%s\nwant it made, %q, and "+
					"its result recorded but unreadable here", got, tc.said)
			}
			if len(tool.calls) != 0 {
				t.Errorf("the call was made again (%d calls)", len(tool.calls))
			}
		})
	}
}
