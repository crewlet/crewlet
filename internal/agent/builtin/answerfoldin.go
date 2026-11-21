package builtin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/runanswer"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/words"
)

// A fold-in's answer through answer_executor (ADR-0109's amendment "another
// writer's request joins only on approval"): whether another writer's request,
// waiting behind a running job, joins that job's work on its requester's apps.
// Decided as the run's question is ([runanswer.FoldIn]): its requester, a lead
// of its seat, or an administrator, a person and never the assistant; never
// the request's own asker. Recorded on the run's row at once
// ([sandbox.AnswerDesk.AnswerFoldIn]), so the answer says what became of it.

// foldInEnded is the refusal for a fold-in whose work is over: the request it
// asked about runs as a turn of its own, and an answer now joins nothing.
const foldInEnded = "The work this request would have joined has ended; the request runs as its own turn. " +
	"Nothing was answered."

// answerFoldIn answers fold-in id on turnID's run as actor with choice, the
// question read with launchID; answer must be empty, since a fold-in is decided
// by its choice alone.
func (t *answerExecutor) answerFoldIn(ctx context.Context, actor Actor, turnID, launchID, id, choice,
	answer string) tools.Result {

	decided := sandbox.FoldInAnswer(choice)
	switch {
	case !decided.Valid():
		return failed(fmt.Sprintf(AnswerExecutorTool+" answers a fold-in with `choice` %s.",
			words.Or(sandbox.FoldInAnswers)))
	case answer != "":
		return failed("A fold-in is answered by its `choice` alone: leave `answer` out.")
	}
	run, found, err := t.deps.Desk.Run(ctx, turnID)
	switch {
	case err != nil:
		return nodeFailure(ctx, AnswerExecutorTool, err, fmt.Sprintf(AnswerExecutorTool+" could not read "+
			"the run of turn %s", clip(turnID)), "This does NOT mean it is gone, and nothing was answered.")
	case !found:
		return refused(tools.RefusalNotRunning, foldInEnded)
	}
	at := slices.IndexFunc(run.FoldIns, func(f sandbox.FoldIn) bool { return f.ID == id })
	if at < 0 || run.FoldIns[at].Launch != run.LaunchID || run.LaunchID != launchID ||
		!slices.Contains(sandbox.ItemStatuses, run.Status) {
		return refused(tools.RefusalNotRunning, foldInEnded)
	}
	f := run.FoldIns[at]
	switch {
	case !f.Open():
		return refused(tools.RefusalAlreadyAnswered, foldInAnsweredAlready(f))
	case !f.AnswerableBy(actor.Handle):
		return refused(tools.RefusalForbidden, "You asked for this work: whether it joins work running for "+
			"somebody else is theirs or a lead's to decide. Nothing was answered.")
	}
	authority, err := runanswer.FoldIn(run, actor.Handle)
	if err != nil {
		return faulted(ctx, AnswerExecutorTool, err, fmt.Sprintf(AnswerExecutorTool+" cannot answer the "+
			"fold-in of turn %s: its record names no seat id this build can read: %s.", clip(turnID), faultSaid))
	}
	if refusal := askAuthority(ctx, t.authorize, authority.Action, authority.Object); refusal != nil {
		return *refusal
	}
	got, err := t.deps.Desk.AnswerFoldIn(ctx, run, id, sandbox.FoldInAnswered{Answer: decided, By: actor.Handle,
		ByKind: iam.ActorKind(actor.Kind)})
	switch {
	case errors.Is(err, sandbox.ErrFoldInClosed):
		return refused(tools.RefusalNotRunning, foldInEnded)
	case errors.Is(err, sandbox.ErrFoldInAnswered):
		return refused(tools.RefusalAlreadyAnswered, foldInAnsweredAlready(got))
	case err != nil:
		return nodeFailure(ctx, AnswerExecutorTool, err, fmt.Sprintf(AnswerExecutorTool+" could not record "+
			"the answer to the fold-in of turn %s", clip(turnID)), "It may or may not be recorded: answering "+
			"again is safe, since the first answer is the one kept.")
	}
	said := "The request joins the running work at the run's next step, as a note naming who asked and you."
	if decided == sandbox.FoldInRefuse {
		said = "The request waits, and runs as a turn of its own once the running work ends."
	}
	result, err := jsonResult(map[string]any{"turn_id": run.TurnID, "agent_handle": run.AgentHandle,
		"launch_id": run.LaunchID, "fold_in": got.ID, "asker": got.Asker, "choice": string(got.Answer),
		"outcome": said})
	if err != nil {
		return faulted(ctx, AnswerExecutorTool, err, "the answer was recorded, and this reply could not be "+
			"written: "+faultSaid)
	}
	return result
}

// foldInAnsweredAlready is the refusal for a fold-in answered before: by whom,
// when, and what was decided.
func foldInAnsweredAlready(f sandbox.FoldIn) string {
	by := f.AnsweredBy
	if by == "" {
		by = "someone else"
	}
	when := ""
	if !f.AnsweredAt.IsZero() {
		when = " at " + f.AnsweredAt.UTC().Format(time.RFC3339)
	}
	what := "approved it joining the running work"
	if f.Answer == sandbox.FoldInRefuse {
		what = "left it to run as a turn of its own"
	}
	return fmt.Sprintf("This request was already answered: %s %s%s, so your answer was not used.", by, what, when)
}
