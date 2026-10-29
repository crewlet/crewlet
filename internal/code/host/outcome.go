package host

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/textcut"
)

// WHAT GIT IS TOLD A PUSH DID: `ok` for a ref ONLY from the push's own applied
// row. A proposal answered `pending` is durable and not yet applied here, and
// a durable record a gate then drops on every node is not a push that landed
// — so pending is waited on, at most Timing.PendingGrace, and never reported
// as ok (ADR-0054).

// answered is one command as git is told it.
type answered struct {
	Ref    string
	OK     bool
	Reason code.Refusal
	Detail string
}

// line is the command's proc-receive report line: `ok <ref>`, or `ng <ref>
// <sentence>`, the sentence the command's Detail where one was formed and
// `<code>: <the code's sentence>` otherwise.
//
// THE LINE FITS ONE PKT-LINE with the newline the report adds (receive-pack's
// own report adds it to what the hook answers): the ref WHOLE, because git
// matches a status to its command by the ref's name, and the sentence cut to
// the room the ref leaves. A ref is read off a command line that held both ids
// beside it, so it is at most maxPktPayload less 82 bytes and that room is
// never under 77 — but a ref past code.MaxRefNameBytes is a command (refused
// bad_ref), and the longest a pkt-line carries left a full sentence a line a
// few bytes past git's largest, which git refuses to read: the push's whole
// report lost to a protocol error rather than one sentence cut.
func (a answered) line() string {
	if a.OK {
		return "ok " + a.Ref
	}
	var words string
	switch {
	case a.Reason == "":
		// A REFUSAL ONLY THE RECEIVER NAMES — a command list past what one
		// push may carry — has no code of the domain's. A token bound to
		// nobody is no such refusal: its grant answers not_permitted.
		words = a.Detail
	case a.Detail == "":
		words = string(a.Reason) + ": " + a.Reason.Sentence()
	default:
		words = string(a.Reason) + ": " + a.Detail
	}
	head := "ng " + a.Ref + " "
	return head + textcut.Within(sentence(words), maxPktPayload-len("\n")-len(head))
}

// answersFor is cmds as git is told them: ok and noop are ok, ng carries its
// reason and its sentence.
func answersFor(cmds []code.Command) []answered {
	out := make([]answered, len(cmds))
	for i, c := range cmds {
		out[i] = answered{Ref: c.Ref, OK: c.Result == code.ResultOK || c.Result == code.ResultNoop,
			Reason: c.Reason, Detail: c.Detail}
	}
	return out
}

// refuseRest is cmds with every command not already ng refused under reason
// with detail.
func refuseRest(cmds []code.Command, reason code.Refusal, detail string) []code.Command {
	out := make([]code.Command, len(cmds))
	for i, c := range cmds {
		if c.Result != code.ResultNG {
			c.Result, c.Reason, c.Detail = code.ResultNG, reason, detail
		}
		out[i] = c
	}
	return out
}

// silence is the estate router's own statement about a walk that ran nothing
// (estate.ErrUnserved.Silent): whether a node it asked went silent — and so may
// have run the proposal it never answered. Declared here, by the consumer,
// because this package imports no estate.
type silence interface{ Silent() bool }

// verdict is what one push came to: what git is told for each command, and
// the record's outcome.
type verdict struct {
	commands []code.Command
	outcome  types.AuditOutcome
	refusal  AuditRefusal
	position statelog.Position
	// applied: the push's row exists — its pack may be adopted.
	applied bool
	// change is what the push's change came to (code.PushAnswer.Change),
	// nil where the answer says nothing of one.
	change *code.ChangeAnswer
}

// decided is the verdict of a landing's proposal's answer (Estate.Propose):
// every command's answer and the audit's outcome, the pending wait included,
// and what the proposal's change came to where it carried one (asked).
func (s *Server) decided(ctx context.Context, l *landing, proposed []code.Command, asked bool,
	res code.ProposeResult, err error) verdict {

	var quiet silence
	switch {
	case err != nil && ctx.Err() != nil:
		// THE CLIENT HUNG UP: there is nobody to answer, and the batch may
		// still land — the committer runs it under its own lifetime.
		return verdict{commands: refuseRest(proposed, code.RefusalUnknown, ""), outcome: types.AuditUnknown}
	case errors.Is(err, code.ErrInvalidProposal):
		s.log.Error("code_push_invalid", "repo", l.Synced.Repo.Name, "push_id", l.PushID, "error", err)
		return verdict{commands: refuseRest(proposed, code.RefusalUnavailable,
			"the engine refused its own proposal: "+sentence(err.Error())), outcome: types.AuditFailed}
	case err != nil && errors.As(err, &quiet) && !quiet.Silent():
		// NOTHING RAN IT: every node the walk asked answered that it
		// would not, so the push did not land and a re-run lands it.
		return verdict{commands: refuseRest(proposed, code.RefusalUnavailable,
			fmt.Sprintf("no data node took push %s: %s", l.PushID, sentence(err.Error()))),
			outcome: types.AuditRefused, refusal: AuditRefusal(code.RefusalUnavailable)}
	case err != nil:
		// A NODE WENT SILENT, or the error does not say: it may have run
		// the push, so whether it landed is unknown — never "nothing took
		// it", which a re-run would then report as up to date.
		return verdict{commands: refuseRest(proposed, code.RefusalUnknown,
			fmt.Sprintf("no data node answered for push %s; whether it landed is unknown — fetch, then "+
				"re-run git push", l.PushID)), outcome: types.AuditUnknown}
	case res.Answer.Refused != "":
		detail := res.Answer.Detail
		return verdict{commands: refuseRest(res.Answer.Commands, res.Answer.Refused, detail),
			outcome: types.AuditRefused, refusal: AuditRefusal(res.Answer.Refused)}
	case res.Outcome == statelog.OutcomePending:
		return s.awaitPending(ctx, l, proposed, asked, res.Answer.Change, res.Position)
	case res.Outcome == statelog.OutcomeUnknown:
		return verdict{commands: refuseRest(proposed, code.RefusalUnknown,
			fmt.Sprintf("outcome unknown (op %s); fetch, then re-run git push", res.OpID)),
			outcome: types.AuditUnknown, change: res.Answer.Change}
	}
	// APPLIED, or answered with no record of its own: the commands are the
	// row's (or the decision's where every one was noop or ng).
	row := res.Outcome == statelog.OutcomeApplied || res.Answer.Collapsed
	v := settled(res.Answer.Commands, res.Position, row, res.Answer.Collapsed)
	v.change = res.Answer.Change
	return v
}

// settled is the verdict of a push whose commands are final — row reports that
// its push row exists: applied with the row's position when any command moved
// a ref or the answer collapsed onto an earlier row; applied with no position
// when every command was a noop; refused under the first code otherwise.
func settled(cmds []code.Command, at statelog.Position, row, collapsed bool) verdict {
	v := verdict{commands: cmds, applied: row}
	moved, noop := false, true
	var first code.Refusal
	for _, c := range cmds {
		switch c.Result {
		case code.ResultOK:
			moved, noop = true, false
		case code.ResultNG:
			noop = false
			if first == "" {
				first = c.Reason
			}
		}
	}
	switch {
	case moved || collapsed:
		v.outcome, v.position = types.AuditApplied, at
	case noop:
		v.outcome = types.AuditApplied
	default:
		v.outcome, v.refusal = types.AuditRefused, AuditRefusal(first)
	}
	return v
}

// awaitPending waits for a push durable at at to apply, reading its outcome
// until Timing.PendingGrace after the proposal returned: found, its row's
// commands are the answer, and its change what the outcome read beside the
// row and the decision said (resolvedChange); not found at or past at, a gate
// dropped the record and the push applied nowhere; any error — a node behind,
// nobody answering — is read again after a wait that doubles from
// PendingFirst to PendingMax; and a grace spent is `pending`, never `ok`, its
// change the decision's, tentative as the push is.
func (s *Server) awaitPending(ctx context.Context, l *landing, proposed []code.Command, asked bool,
	tentative *code.ChangeAnswer, at statelog.Position) verdict {

	l.Progress("waiting for the push to apply")
	deadline := s.now().Add(s.o.Timing.PendingGrace)
	wait := s.o.Timing.PendingFirst
	for {
		out, err := s.o.Estate.PushOutcome(ctx, code.OutcomeQuery{RepoID: l.Synced.Repo.ID, PushID: l.PushID,
			At: at, Change: asked})
		switch {
		case err == nil && out.Found:
			v := settled(out.Push.Commands, out.Push.Position, true, false)
			v.change = resolvedChange(asked, tentative, out, l.PushID)
			return v
		case err == nil && out.Position.Packed() >= at.Packed():
			return verdict{commands: refuseRest(proposed, code.RefusalNotApplied,
				fmt.Sprintf("the push was durable at %s and applied nowhere; re-run git push", at)),
				outcome: types.AuditRefused, refusal: AuditRefusal(code.RefusalNotApplied)}
		}
		left := deadline.Sub(s.now())
		if left <= 0 || ctx.Err() != nil {
			return verdict{commands: refuseRest(proposed, code.RefusalPending,
				fmt.Sprintf("durable at %s, unresolved here; re-run git push (a push that landed reports "+
					"up to date)", at)), outcome: types.AuditPending, position: at, change: tentative}
		}
		timer := time.NewTimer(min(wait, left))
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
		wait = min(2*wait, s.o.Timing.PendingMax)
	}
}
