package runner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
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
// marked once, the result recorded once.
type ledgerCommit struct {
	marked   bool
	result   *sandbox.HeldResult
	refuse   error
	recorded int
}

func (l *ledgerCommit) Begin(context.Context) (HeldPrior, error) {
	if l.refuse != nil {
		return HeldPrior{}, l.refuse
	}
	if !l.marked {
		l.marked = true
		return HeldPrior{First: true}, nil
	}
	if l.result != nil {
		return HeldPrior{Made: true, Result: *l.result}, nil
	}
	return HeldPrior{}, nil
}

func (l *ledgerCommit) Record(_ context.Context, result sandbox.HeldResult) error {
	if l.result == nil {
		l.result = &result
	}
	l.recorded++
	return nil
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
// Mutation: make the call before [HeldCommit.Begin] marks it, or on a mark that
// is not the first, and the second attempt calls the tool twice.
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
