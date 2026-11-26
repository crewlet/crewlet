// Package runanswer is who may answer a parked run, stated once: the verb and
// the object the engine decides a person's answer on, for the call a run holds
// for approval and for the question it asked.
//
// # One authority, read by the write and by every read
//
// The approvals route (`POST /mcp-approvals/{id}`) decides an answer to a held
// call, and the `answer_executor` tool decides an answer to a run's question.
// The screens that list parked runs ask the same thing before a person presses
// anything: may this reader answer it? A read that kept its own copy of the
// rule would offer a button the write refuses, or hide a run the reader could
// have unblocked, and nothing would say the two had drifted. So both writes
// and every read take the verb and the object from here, and a read decides it
// with [authz.Decide] as the write would, at the read's proof ([May]).
//
// # Which verb
//
//   - A held merge or deploy (`release`): [authz.ActionReleaseApprove], on the
//     run's seat, naming the people its delegated-decisions rule escalates to.
//   - A held operate command (`ssh`): [authz.ActionSSHOperateApprove], naming
//     the SSH approvers and the people its rule escalates to; never a lead's.
//   - Any other held call: [authz.ActionToolsApprove], on the run's seat: its
//     lead's or an administrator's.
//   - A question: [authz.ActionExecutorAnswer], on the requester's own seat
//     where the caller is the requester (the rule's self arm), and on the
//     run's seat otherwise, where its lead and an administrator are admitted.
//   - A fold-in, whether another writer's request joins a running job's work
//     ([sandbox.FoldIn]): a question's verb and object, since the answer
//     decides whose request the requester's apps serve (ADR-0109).
//
// It is a LEAF above authz, config and the sandbox's run row: the API's
// approvals, the agent tools and the queries import it, and it imports none
// of them.
package runanswer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// ErrUnreadableSeat is a run whose row names no seat id this build can read,
// so the seat an answer would be decided on cannot be named.
var ErrUnreadableSeat = errors.New("runanswer: the run names no seat id this build can read")

// Authority is the verb and the object one answer to a run is decided on.
type Authority struct {
	Action authz.Action
	Object authz.Object
}

// HeldCall is who may answer the call run holds for approval. company is the
// running document, nil where this node holds none: the people a rule names
// are then nobody, and administrators and leads still decide.
func HeldCall(run sandbox.PendingRun, company *config.Company) Authority {
	held := run.HeldCall
	switch {
	case held != nil && held.Approvers == sandbox.HeldApproversRelease:
		var named []string
		if company != nil {
			named = company.DelegatedDecisions.EscalateFor(delegated.Kind(held.Act))
		}
		return Authority{Action: authz.ActionReleaseApprove,
			Object: authz.Object{Kind: authz.KindPerson, ID: run.AgentID, Owner: run.AgentHandle, Named: named}}
	case held != nil && held.Approvers == sandbox.HeldApproversAdmin:
		// AN ADMINISTRATOR'S ALONE: no owner, so no lead arm, and nobody named.
		return Authority{Action: GrowthApprove(delegated.Kind(held.Act)),
			Object: authz.Object{Kind: authz.KindPerson, ID: run.AgentID}}
	case held != nil && held.Approvers == sandbox.HeldApproversSSH:
		// NEVER A LEAD'S: the object names no owner, so the lead arm has
		// nothing to admit on.
		return Authority{Action: authz.ActionSSHOperateApprove,
			Object: authz.Object{Kind: authz.KindPerson, ID: run.AgentID, Named: company.SSHMayApprove()}}
	}
	return Authority{Action: authz.ActionToolsApprove,
		Object: authz.Object{Kind: authz.KindPerson, ID: run.AgentID, Owner: run.AgentHandle}}
}

// GrowthApprove is the verb a growth act of kind is approved under; an act
// that is no growth act gets the admin verb, the strictest of the three.
func GrowthApprove(kind delegated.Kind) authz.Action {
	switch kind {
	case delegated.KindAdSpend:
		return authz.ActionGrowthSpendApprove
	case delegated.KindPublicPost:
		return authz.ActionGrowthPublicApprove
	}
	return authz.ActionGrowthAdminApprove
}

// Question is who may answer the question run asked, as caller — the seat
// handle the caller acts as, "" for one holding none: the requester's own
// seat where the caller is the requester, and the run's seat otherwise.
// [ErrUnreadableSeat] for a row with no seat id this build can read.
func Question(run sandbox.PendingRun, caller string) (Authority, error) {
	seat, err := uuid.Parse(run.AgentID)
	if err != nil || seat == uuid.Nil {
		return Authority{}, fmt.Errorf("run %s names seat id %q: %w", run.TurnID, run.AgentID, ErrUnreadableSeat)
	}
	return QuestionOf(seat, run.AgentHandle, run.Requester, caller), nil
}

// QuestionOf is [Question] over a run's parties already read: its seat by id
// and handle, and the seat whose wake started its turn.
func QuestionOf(seat uuid.UUID, handle, requester, caller string) Authority {
	owner := handle
	if requester != "" && caller == requester {
		owner = requester
	}
	return Authority{Action: authz.ActionExecutorAnswer,
		Object: authz.Object{Kind: authz.KindPerson, ID: seat.String(), Owner: owner}}
}

// FoldIn is who may answer a fold-in on run, as caller: [Question]'s rule.
// [ErrUnreadableSeat] for a row with no seat id this build can read.
func FoldIn(run sandbox.PendingRun, caller string) (Authority, error) { return Question(run, caller) }

// Of is who may answer what run waits on now: its held call where it holds
// one, its question otherwise.
func Of(run sandbox.PendingRun, company *config.Company, caller string) (Authority, error) {
	if run.HeldCall != nil {
		return HeldCall(run, company), nil
	}
	return Question(run, caller)
}

// May decides whether p may answer what run waits on now, as the write would
// decide it but at the read's proof ([authz.WithCheck]): a person whose
// step-up has lapsed may still answer, and is asked for a fresh one when they
// press. An undecidable answer is the decision's own ([authz.Decision.Unknown]).
func May(ctx context.Context, p iam.Principal, run sandbox.PendingRun, company *config.Company,
	chart authz.Chart, now time.Time) (authz.Decision, error) {

	// THE NAME THE ANSWER TOOL DECIDES THE SELF ARM BY: the caller's record
	// ([iam.RecordOwner], the seat of a bound person), as [iam.ActorFor] names
	// them to the tool.
	a, err := Of(run, company, iam.RecordOwner(p))
	if err != nil {
		return authz.Decision{}, err
	}
	return authz.Decide(authz.WithCheck(ctx), p, a.Action, a.Object, chart, now), nil
}

// Waiting is where a parked run stands for one reader: whom it is put to, and
// whether the reader may answer it.
type Waiting struct {
	// PutTo is who it waits on now: its delegates while a delegated call
	// waits on them, else the people its question is put to.
	PutTo []string
	// PutToYou is whether it is put to the reader's own record.
	PutToYou bool
	// MayAnswer is whether the reader may answer it ([May]).
	MayAnswer bool
	// Escalated is a delegated call its delegates escalated to people, and
	// Overdue one still with its delegates past the company's deadline
	// ([delegated.Limits]).
	Escalated bool
	Overdue   bool
}

// NeedsYou is whether the run waits on the reader: put to them, or theirs to
// answer and already escalated by its delegates or past their deadline. A run
// they may answer that waits on somebody else in good time is listed apart,
// and asks nothing of them yet.
func (w Waiting) NeedsYou() bool {
	return w.PutToYou || (w.MayAnswer && (w.Escalated || w.Overdue))
}

// Delegation is whether a held call is with its delegates, escalated, or past
// deadline (the company's, [delegated.Limits]) at now by node's clock, never
// before it has passed in real time ([sandbox.HeldCall.WaitedFor]).
func Delegation(run sandbox.PendingRun, deadline time.Duration, node string, now time.Time) (withDelegates,
	escalated, overdue bool) {
	held := run.HeldCall
	if held == nil || held.Delegation == nil {
		return false, false, false
	}
	if held.Delegation.Escalated != nil {
		return false, true, false
	}
	return true, false, held.WaitedFor(node, now, time.Time{}, deadline)
}
