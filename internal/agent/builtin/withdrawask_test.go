package builtin_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// resolverFake records the questions resolved through it, and as whom.
type resolverFake struct {
	resolved []string
	by       []string
}

func (r *resolverFake) as(actor builtin.Actor) builtin.AskResolver {
	r.by = append(r.by, actor.Handle)
	return r
}

func (r *resolverFake) ResolveComment(_ context.Context, _, _, _, commentID string,
	_ *worklet.Notify) (worklet.WriteResult, error) {
	r.resolved = append(r.resolved, commentID)
	return worklet.WriteResult{Outcome: statelog.OutcomeApplied, Version: 13}, nil
}

// withdrawRig is withdraw_ask over a tracker whose ENG-1 holds a question the
// CTO asked of legal, and a desk holding runs.
func withdrawRig(t *testing.T, runs map[string]sandbox.PendingRun) (tools.Callable, *fakeTracker,
	*resolverFake, *deskFake) {

	t.Helper()
	return withdrawRigAuthorized(t, runs, nil)
}

// withdrawRigAuthorized is [withdrawRig] decided by authorize, nil for the real
// table's decision.
func withdrawRigAuthorized(t *testing.T, runs map[string]sandbox.PendingRun,
	authorize builtin.Authorizer) (tools.Callable, *fakeTracker, *resolverFake, *deskFake) {

	t.Helper()
	trk := newFakeTracker()
	detail := trk.tasks["ENG-1"]
	detail.Comments = []worklet.Comment{{ID: "c-ask", Task: "i1", Author: "agent-cto",
		Ask: "legal", Body: "May we restart the database on the production server?",
		Decision: &decision.Decision{Question: "Restart it?"}}}
	trk.tasks["ENG-1"] = detail
	res := &resolverFake{}
	desk := &deskFake{runs: runs}
	tool := registered(t, builtin.Deps{
		Work:      builtin.WorkDeps{Reader: trk, Writer: trk.as},
		Withdraw:  builtin.WithdrawDeps{Resolver: res.as, Desk: desk},
		Authorize: authorize,
	}, builtin.WithdrawAskTool)
	return tool, trk, res, desk
}

// A SEAT WITHDRAWS ITS OWN HELD CALL AS DENY, saying why: the call is never
// made, the run is carried on told it withdrew it, and the answer names the
// call and the question it withdraws, given by the seat itself.
//
// Mutation: deliver the withdrawal without the held call's id and the seat
// holding the run records no answer to the call.
func TestASeatWithdrawsItsOwnHeldCallAsDeny(t *testing.T) {
	t.Parallel()
	tool, _, _, desk := withdrawRig(t, map[string]sandbox.PendingRun{"t-1": {
		TurnID: "t-1", AgentID: "a-cto", AgentHandle: "agent-cto", LaunchID: "l-1",
		Status: sandbox.StatusAwaiting, HeldCall: &sandbox.HeldCall{ID: "h-1", Tool: "ssh_operate"},
	}})
	got := callFor(t, tool, turnFor(t, "agent-cto"), map[string]any{"run": "t-1",
		"reason": "My access to the server is read only now, so this call cannot run."})
	if got.Failed || len(desk.delivered) != 1 {
		t.Fatalf("the withdrawal answered %s with %d deliveries", got.Output, len(desk.delivered))
	}
	given := desk.delivered[0]
	if given.TurnID != "t-1" || given.LaunchID != "l-1" || given.Approval != "h-1" ||
		given.Choice != sandbox.HeldDeny || given.AnsweredBy != "agent-cto" ||
		given.AnsweredByKind != string(iam.ActorAgent) || !strings.Contains(given.Answer, "read only") {
		t.Errorf("the withdrawal delivered %+v, want Deny on h-1 by the seat with its reason", given)
	}
}

// A SEAT CANNOT WITHDRAW ANOTHER SEAT'S RUN, nor one that waits on nobody: the
// run reads as none, and nothing is delivered.
//
// Mutation: drop the owner check and the CTO denies the CEO's call.
func TestASeatCannotWithdrawAnotherSeatsRun(t *testing.T) {
	t.Parallel()
	tool, _, _, desk := withdrawRig(t, map[string]sandbox.PendingRun{
		"t-ceo": {TurnID: "t-ceo", AgentHandle: "agent-ceo", Status: sandbox.StatusAwaiting,
			HeldCall: &sandbox.HeldCall{ID: "h-2"}},
		"t-running": {TurnID: "t-running", AgentHandle: "agent-cto", Status: sandbox.StatusRunning,
			Question: "Which branch?"},
	})
	for _, run := range []string{"t-ceo", "t-running", "t-none"} {
		got := callFor(t, tool, turnFor(t, "agent-cto"), map[string]any{"run": run, "reason": "No longer needed."})
		if !got.Failed {
			t.Errorf("withdrawing %s answered %s, want a refusal", run, got.Output)
		}
	}
	if len(desk.delivered) != 0 {
		t.Errorf("refused withdrawals delivered %+v", desk.delivered)
	}
}

// A SEAT WITHDRAWS ITS OWN QUESTION ON A WORK ITEM, SAYING WHY: the reason is
// posted in the question's thread, where the person asked reads it, and the
// question is resolved as its author.
//
// Mutation: resolve without posting the reason and the person reads a question
// closed with no word of why.
func TestASeatWithdrawsItsOwnQuestionSayingWhy(t *testing.T) {
	t.Parallel()
	tool, trk, res, _ := withdrawRig(t, nil)
	reason := "Updated after my access changed: I can only read the server now, so I will not restart it."
	got := callFor(t, tool, turnFor(t, "agent-cto"), map[string]any{"item": "ENG-1", "ask": "c-ask",
		"reason": reason})
	if got.Failed {
		t.Fatalf("the withdrawal answered %s", got.Output)
	}
	if len(trk.patched) != 1 || trk.patched[0].Comment == nil || trk.patched[0].Comment.Body != reason ||
		trk.patched[0].Comment.ReplyTo == nil || *trk.patched[0].Comment.ReplyTo != "c-ask" {
		t.Fatalf("the reason was posted as %+v, want a reply to the question", trk.patched)
	}
	if len(res.resolved) != 1 || res.resolved[0] != "c-ask" || res.by[0] != "agent-cto" {
		t.Errorf("resolved %v as %v, want the question as its author", res.resolved, res.by)
	}
}

// A SEAT CANNOT WITHDRAW A QUESTION IT DID NOT ASK: another seat's question is
// refused before anything is written, and so is a withdrawal with no reason.
//
// Mutation: drop the author check and the CEO closes the CTO's question.
func TestASeatCannotWithdrawAQuestionItDidNotAsk(t *testing.T) {
	t.Parallel()
	tool, trk, res, _ := withdrawRig(t, nil)
	if got := callFor(t, tool, turnFor(t, "agent-ceo"), map[string]any{"item": "ENG-1",
		"ask": "c-ask", "reason": "Not needed."}); !got.Failed {
		t.Errorf("another seat's withdrawal answered %s", got.Output)
	}
	if got := callFor(t, tool, turnFor(t, "agent-cto"), map[string]any{"item": "ENG-1",
		"ask": "c-ask"}); !got.Failed {
		t.Errorf("a withdrawal with no reason answered %s", got.Output)
	}
	if len(trk.patched) != 0 || len(res.resolved) != 0 {
		t.Errorf("refused withdrawals wrote %+v and resolved %v", trk.patched, res.resolved)
	}
}

// A SEAT WITHDRAWS ITS OWN QUESTION WHOEVER WOKE THE TURN: the asker's arm, as
// answering is the asked person's, decided by authorship and never by the
// Worklet app, so a turn woken by somebody kept off Worklet still withdraws it,
// as it withdraws a held call.
//
// Mutation: gate the withdrawal on the comment verb and it is refused here.
func TestASeatWithdrawsItsOwnQuestionWhoeverWokeTheTurn(t *testing.T) {
	t.Parallel()
	table := builtin.Decide(authz.NoChart{})
	noWorklet := func(ctx context.Context, action authz.Action, object authz.Object) error {
		if action == authz.ActionWorkComment {
			return fmt.Errorf("%w: whoever woke this turn does not use Worklet", builtin.ErrRefused)
		}
		return table(ctx, action, object)
	}
	tool, trk, res, _ := withdrawRigAuthorized(t, nil, noWorklet)
	got := callFor(t, tool, turnFor(t, "agent-cto"), map[string]any{"item": "ENG-1", "ask": "c-ask",
		"reason": "Updated after my access changed: I can only read the server now."})
	if got.Failed || len(trk.patched) != 1 || len(res.resolved) != 1 {
		t.Errorf("the withdrawal answered %s with %d reasons and %v resolved, want it withdrawn",
			got.Output, len(trk.patched), res.resolved)
	}
}
