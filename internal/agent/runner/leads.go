package runner

import (
	"context"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/toolname"
)

// WHAT CARRIED A SESSION ON, TOLD AGAIN TO ONE THAT MAY LACK IT.
//
// A brief opens with what carries its session on: the answer to the run's
// question ([Asked]), a review that sent the round back
// ([prompts.BuildContinuedRound]), or, for a job that continued the session its
// conversation's last turn left, where the turn begins and the turn's task
// ([prompts.BuildNextTurn], [Runner.baseSession]). A session is known to hold
// that brief only where its harness ended its own run: the copy a job's own box
// holds whose harness may have been cut off ([Runner.copyMayLackBrief]), and a
// capture cut off ([SessionBasis.MayLackBrief]), may lack it, and lack the lead
// it opened with. Told every call and every note again, such a session was
// never told the lead: no record kept it. A session cut off before its harness
// wrote its brief was then told "continue from where you are", and continued
// the conversation's earlier turn, or the round before the review.
//
// So the launch hands the leads each brief opens with to the run's leads beside
// its record (sandbox.BriefTold.Own), and the leads its brief told again
// (sandbox.BriefTold.Told), which the job's record marks: a continuation of a
// session that may lack its job's brief is told again the leads that brief told
// ([Resume.Leads]), after the section that says it was cut off and before its
// calls, under a header that says the session may already show them
// ([prompts.LeadsToldAgain]); and hands them on, as leads its own brief told
// again, so a continuation of ITS session, which may lack its brief too, tells
// them in turn.
//
// IN WORDS TRUE OF ANY SESSION OF THE WORK, never in the form the lead was told.
// A lead told again may reach a session that never asked the question, a copy of
// another job of the lineage, or the session its conversation's last turn left
// that was handed the answer in a fresh run's words: told "you stopped to ask",
// it is told something its session never did; told to check a branch out on a
// fresh machine, or that it is in a new box, it is told of a box it is not in.
// So an answer is "a run of this work asked … answered", a held call "a run of
// this work called", and the turn's task keeps its boundary and none of its
// box's claims ([prompts.NextTurnAgain]). Each is told under the relaunching
// segment's token: a chat reply fenced under who wrote it, an answer by turn and
// every engine word defused, as the lead it was.

// copyMayLackBrief reports whether the copy of the session the job's own box
// holds may lack the brief its job was handed by its record's word: where the
// job's harness is not known to have ended its own run ([Resume.HarnessEnd]),
// cut off or never said. THE RECORD'S HALF of what the in-place path's telling
// may count held: every telling of the path reads the one value decided from
// it, a restart in the job and whether its harness may compact the copy beside
// it ([sessionHolds]), since that verdict needs the brief this is asked before.
func (r *Runner) copyMayLackBrief() bool {
	return r.cfg.Resume == nil || !r.cfg.Resume.HarnessEnd.EndedOwnRun()
}

// toldAgain is what a brief to a session that may lack its job's brief tells
// again of the leads that brief told ([Resume.Leads]): the section that tells
// them ([prompts.LeadsToldAgain]), "" for none; which they were, for the job's
// record to mark told in turn (sandbox.BriefTold.Told), one that does not open
// here among them, since the brief was to tell it; the ones it was to tell and
// could not read, by their numbers, marked told in turn the same way
// (sandbox.BriefTold.ToldUnread, [Resume.LeadsUnread]); and the held call every
// held lead among them tells what became of, which no list of the round's
// calls in the same brief tells ([Runner.calledAfter]).
type toldAgain struct {
	section string
	refs    []sandbox.LeadRef
	unread  []int
	held    []string
}

// leadsAgain is what a brief to a session that may lack its job's brief tells it
// again of the leads that brief told ([toldAgain]): each told lead, oldest first,
// in words of its own ([Runner.leadAgain]) under the relaunching segment's token
// b, to a run working in round current, and how many earlier leads are not kept
// ([Resume.LeadsCut]). EVERY LEAD THE RECORD KEEPS, told or not, where all says
// the session may be a summary ([sessionHolds.summarized]): a compaction can
// drop every lead the session held from earlier briefs, and the record cannot
// say which those were.
//
// BUILT BEFORE THE BRIEF'S LEAD, beside the calls: telling the turn's task again
// renders the prior-work ledger of the rounds its brief rendered, which can
// spend the seat's auxiliary model, charged before the relaunch's last ask of
// the seat's budget.
func (r *Runner) leadsAgain(ctx context.Context, history []ledger.Iteration, current int,
	b provenance.Boundary, all bool) toldAgain {
	if r.cfg.Resume == nil {
		return toldAgain{}
	}
	var (
		// AND THE ONES IT COULD NOT READ, by their numbers, said among the
		// leads not kept and handed on beside the ones it tells: this
		// session lacks them as surely, and a later read tells them.
		out      = toldAgain{unread: slices.Clone(r.cfg.Resume.LeadsUnread)}
		rendered []string
	)
	for _, told := range r.cfg.Resume.Leads {
		if !told.Told && !all {
			continue
		}
		lead := told.Lead
		out.refs = append(out.refs, sandbox.LeadRef{Job: lead.Job, Kind: lead.Kind})
		if lead.Unread == "" && lead.Answer != nil && lead.Answer.Held != nil && lead.Answer.Refused == nil {
			out.held = append(out.held, lead.Answer.Held.HeldID)
		}
		text, held := r.leadAgain(ctx, told, history, current, b)
		rendered, out.held = append(rendered, text), append(out.held, held...)
	}
	out.section = prompts.LeadsToldAgain(rendered, r.cfg.Resume.LeadsCut)
	return out
}

// leadAgain is one lead told again, under a heading of its own, to a run working
// in round current: an answer in words of its own ([answerAgain]); a review as
// its send-back told it, on the notes of the round it closed
// ([prompts.BuildContinuedRound]); the turn's task with its boundary, the closed
// rounds its brief rendered and the answers it told, or was to tell where its
// read of them failed ([Runner.taskAgain]); and one that did not open here, or
// that this build cannot render, as a message that cannot be read here
// ([prompts.LeadUnread]). What a review or a task recomposes is nested under
// the lead by the headings its builder wrote, never a byte of the words they
// hold moved ([prompts.Prompt.Nested]): the task, the review's notes and the
// ledger's quoted output are somebody else's words. held is the held calls a
// task told again tells what became of among its answers, which no list of
// the round's calls in the same brief tells ([withHeld]).
func (r *Runner) leadAgain(ctx context.Context, told ResumeLead, history []ledger.Iteration, current int,
	b provenance.Boundary) (text string, held []string) {

	lead := told.Lead
	switch {
	case lead.Unread != "":
	case lead.Kind == sandbox.LeadKindAnswer && lead.Answer != nil:
		return answerAgain(*lead.Answer, told.Result, answerAt{}, b), nil
	case lead.Kind == sandbox.LeadKindReview:
		if notes, found := reviewOf(history, lead.Round); found {
			return r.sendBackLead(notes).Nested().Text, nil
		}
	case lead.Kind == sandbox.LeadKindTask && lead.Task != nil:
		return r.taskAgain(ctx, lead, history, current, b)
	}
	return prompts.LeadUnread, nil
}

// reviewOf is the review notes that closed round iteration of history, and
// whether history holds that round: what a send-back's lead told its session
// ([Runner.sendBackLead]).
func reviewOf(history []ledger.Iteration, iteration int) (string, bool) {
	for _, round := range history {
		if round.Iteration == iteration {
			return round.ReviewNotes, true
		}
	}
	return "", false
}

// taskAgain is a task lead told again, to a run working in round current: where
// the turn began, as [prompts.NextTurnAgain] tells it of the session its
// conversation's last turn left ([Resume.Base]), then the turn's ask as that
// brief told it, with only the closed rounds its prior-work block rendered
// ([Runner.turnAsk]): a round closed after it is in the session, or is told by
// its own rule, and told here too, round 1's calls would be told in the task's
// ledger and again in the calls the brief lists after it. The base's turn and
// home are the engine's record of that session, defused under b.
//
// AND THEN THE ANSWERS THAT BRIEF TOLD, after the ask, as that brief told them
// ([earlierAnswers]): the brief that continued the session its conversation's
// last turn left is the turn's fresh brief whole, which tells the turn's
// answers after its ask ([Runner.turnsAnswers]): every answer lead whose job
// came before the lead's own ([sandbox.Lead.Seq]), the only ones the run kept
// when that brief was built, and never the lead's own carry, which is told
// after it as a lead of its own. Recomposed here, never marked told on their
// own, so they are told again in that brief's order: marked told apart, every
// answer of an earlier job sorts before the task among the run's leads, and,
// the later of two that differ standing, the task told again would stand over
// the answers its own brief told after it. Where that brief's read of the
// run's leads failed, it said them as not kept rather than told them
// (sandbox.TurnLeads.Unread), and a read that gives them back tells them here
// all the same: the session lacks them as surely. held is the held calls those
// answers tell what became of.
func (r *Runner) taskAgain(ctx context.Context, lead sandbox.Lead, history []ledger.Iteration, current int,
	b provenance.Boundary) (string, []string) {

	task := *lead.Task
	m := prompts.NextTurn{Fire: notify.Scheduled(r.cfg.Turn.ConversationKey)}
	if base := r.cfg.Resume.Base; base != nil {
		m.BaseTurn, m.EndedAt, m.CapturedAt = provenance.Neutralise(b, base.Turn), base.EndedAt, base.CapturedAt
		m.WentOn, m.Lost, m.Interrupted = base.WentOn, base.Lost, base.Interrupted
	}
	rendered := history[:min(max(task.Rounds, 0), len(history))]
	reviewed := ""
	if task.Reviewed > 0 {
		reviewed, _ = reviewOf(history, task.Reviewed)
	}
	told := prompts.NextTurnAgain(m) + "\n\n" + r.turnAsk(ctx, rendered, reviewed).Nested().Text
	answers := earlierAnswers(answersBefore(r.cfg.Resume.Leads, lead.Seq), current, rendered, 0, false, b)
	if answers.section.Text != "" {
		told += "\n\n" + answers.section.Nested().Text
	}
	return told, answers.held
}

// turnAsk is the turn's own ask as a fresh brief renders it ([Runner.executorBrief])
// with no note in it: the task, carrying notes where a review sent the round
// back, the prior-work block of history, how the conversation's earlier turns
// ended and what changed in the seat's access. The notes are told again by their
// own rule ([Runner.toldNotes]), never inside a lead.
func (r *Runner) turnAsk(ctx context.Context, history []ledger.Iteration, notes string) prompts.Prompt {
	return prompts.BuildPhaseUserMessage(prompts.UserMessage{
		TaskDescription:   r.taskFor(notes),
		PriorWork:         r.priorWork(ctx, history),
		Endings:           r.cfg.Endings,
		CapabilityChanges: r.cfg.CapabilityChanges,
	})
}

// answerAt is where an answer told stands in the brief that tells it, for the
// words that tell it ([answerAgain]): named, the round it came in as its heading
// names it ([prompts.AnswerRound]), "" in a brief that names no answer's round;
// and listed, that round where the same brief lists its calls as work already
// done (a fresh brief's prior-work block, or the one a task told again renders),
// zero where it does not. A held call made is then told among those calls, and
// its answer points to them ([prompts.HeldCallListed]): told by both, one call
// would be told twice, the second time as a call of its own, which no list of a
// round's calls does for the call a lead tells ([Runner.calledAfter]). A closed
// round's list keeps no held call's id to leave the call out by, so the answer
// is the one that defers.
type answerAt struct {
	named  string
	listed int
}

// answerAgain is an answer lead told again, under fence token b: the run's
// question and the answer, or the withdrawal, the refused reply or the held call
// and what became of it, as a run of this work was told them, in words true of
// any session of the work (the file comment says why), standing where at says.
// result is what a held call returned, opened, nil where it did not open here.
//
// A REPLY IS FENCED UNDER WHO WROTE IT, AN ANSWER BY TURN DEFUSED, as the lead
// it was ([Asked.answered]): the run reads a chat reply as information from its
// sender and an answer by turn as its answerer's own instruction (ADR-0040,
// ADR-0068), whichever relaunch tells it.
func answerAgain(a sandbox.LeadAnswer, result *sandbox.HeldResult, at answerAt, b provenance.Boundary) string {
	var w strings.Builder
	switch {
	case a.Held != nil && a.Refused == nil:
		h := heldAgain(*a.Held, result)
		called := "A run of this work called `" + provenance.Neutralise(b, h.Tool) + "` on the `" +
			provenance.Neutralise(b, h.Server) + "` MCP server, which asks a person before each call."
		if a.Withdrawn {
			w.WriteString("### A call this work's seat withdrew" + at.named + "\n" + called +
				" The seat withdrew it before anybody answered: " + provenance.Neutralise(b, a.Words) +
				"\n\nThe call was not made.")
			return w.String()
		}
		w.WriteString("### A call a person answered" + at.named + "\n" + called + " " + h.what(heldBy(a.By, b)) +
			"\n\n")
		if said := strings.TrimSpace(a.Words); said != "" {
			w.WriteString("They said: " + provenance.Neutralise(b, said) + "\n\n")
		}
		if made := h.outcome.kind == heldMade || h.outcome.kind == heldUnread; made && at.listed > 0 {
			w.WriteString(prompts.HeldCallListed(at.listed))
		} else {
			w.WriteString(h.told(b))
		}
	case a.Refused != nil:
		refused := RefusedReply{Messages: a.Refused.Messages, Bytes: a.Refused.Bytes, MaxBytes: a.Refused.MaxBytes}
		w.WriteString("### A reply too large to reach this work" + at.named + "\n" + askedAgain(a, b) +
			answererAgain(a.By, b) + " replied, but " + refused.said() + ", so the engine did not take it as the " +
			"answer, and the run was given none of it.")
		return w.String()
	case a.Withdrawn:
		w.WriteString("### A question this work's seat withdrew" + at.named + "\n" + askedAgain(a, b) +
			"The seat withdrew it before anybody answered: " + provenance.Neutralise(b, a.Words))
		return w.String()
	default:
		w.WriteString("### An answer to a question this work asked" + at.named + "\n" + askedAgain(a, b))
		if a.Label != nil {
			w.WriteString(answererAgain(a.By, b) + " answered:\n" + a.Label.Fence(b, a.Words))
		} else {
			w.WriteString(answererAgain(a.By, b) + " answered: " + provenance.Neutralise(b, a.Words))
		}
	}
	if a.Cut > 0 {
		w.WriteString("\n\nTheir answer is cut here: " + kept.SizeUp(a.Cut) + " more of it is not kept to tell " +
			"again. If you need the rest and the session above does not show it, ask them again with `" +
			toolname.AskAndWait + "`.")
	}
	return w.String()
}

// askedAgain is the run's own question as a lead told again reads it back, its
// line and a blank one after it, "" where the lead kept none.
func askedAgain(a sandbox.LeadAnswer, b provenance.Boundary) string {
	if a.Question == "" {
		return ""
	}
	return "A run of this work asked: " + provenance.Neutralise(b, a.Question) + "\n\n"
}

// answererAgain is who answered, as a lead told again names them: never "the
// person you asked", which a session that never asked would be told.
func answererAgain(by string, b provenance.Boundary) string {
	if by != "" {
		return provenance.Neutralise(b, by)
	}
	return "The person it asked"
}

// heldBy is who answered a held call, as a lead told again names them.
func heldBy(by string, b provenance.Boundary) string {
	if by != "" {
		return provenance.Neutralise(b, by)
	}
	return "A person"
}

// keptWords is an answer's words as its lead keeps them, cut to
// [sandbox.MaxLeadWordsBytes] with the cut marked, and how many bytes of them
// it does not keep.
func keptWords(words string) (string, int) {
	kept := textcut.Within(words, sandbox.MaxLeadWordsBytes)
	if kept == words {
		return words, 0
	}
	return kept, len(words) - len(strings.TrimSuffix(kept, "…"))
}

// lead is the answer as its lead keeps it ([sandbox.LeadAnswer]), for a later
// brief to a session of the work that may lack the brief this answer opens to
// tell again: the question, the words cut to [sandbox.MaxLeadWordsBytes] with
// the cut counted, who answered and who wrote them, and of a held call what
// became of it and what it returned as its row sealed it. BUILT ONCE THE CALL
// IS MADE ([makeHeld]), so the outcome a later brief tells is the one this
// relaunch's lead told. A refused reply names no held call, as its lead tells
// none ([Asked.tellsHeld]).
func (a *Asked) lead() *sandbox.Lead {
	words, cut := keptWords(a.Answer)
	answer := &sandbox.LeadAnswer{Question: a.Question, Words: words, Cut: cut, By: a.By, Label: a.Label,
		Withdrawn: a.Withdrawn}
	if r := a.Refused; r != nil {
		answer.Refused = &sandbox.LeadRefused{Messages: r.Messages, Bytes: r.Bytes, MaxBytes: r.MaxBytes}
	} else if a.Held != nil {
		answer.Held = a.Held.lead()
	}
	return &sandbox.Lead{Kind: sandbox.LeadKindAnswer, Answer: answer}
}

// lead is the held call as its answer's lead keeps it ([sandbox.LeadHeld]): the
// call, the person's choice, what became of it ([makeHeld]) and what it returned
// as the run's row sealed it.
func (h *HeldCall) lead() *sandbox.LeadHeld {
	return &sandbox.LeadHeld{Tool: h.Tool, Server: h.Server, HeldID: h.HeldID, Choice: h.Choice,
		Outcome: h.outcome.kind.leadOutcome(), Failed: h.outcome.result.Failed, Why: h.outcome.why,
		Result: h.kept}
}

// leadOutcome is k as a lead keeps it: a call not yet made, which no lead is
// built of, and every kind past those as one whose outcome is not known, the
// safe direction.
func (k heldKind) leadOutcome() sandbox.LeadHeldOutcome {
	switch k {
	case heldMade:
		return sandbox.LeadHeldMade
	case heldUnread:
		return sandbox.LeadHeldMadeUnread
	case heldDenied:
		return sandbox.LeadHeldDenied
	case heldNotMade:
		return sandbox.LeadHeldNotMade
	}
	return sandbox.LeadHeldUnknown
}

// heldAgain is the held call a lead kept, in the shape a held call's outcome is
// told in ([HeldCall.what], [HeldCall.told]): result is what it returned, opened
// here, and a call made whose result did not open here is told as one whose
// result is recorded and cannot be read on this node, with whether it failed.
// An outcome this build does not know is told as one whose effect is not known.
func heldAgain(l sandbox.LeadHeld, result *sandbox.HeldResult) *HeldCall {
	h := &HeldCall{Tool: l.Tool, Server: l.Server, HeldID: l.HeldID, Choice: l.Choice}
	switch l.Outcome {
	case sandbox.LeadHeldMade, sandbox.LeadHeldMadeUnread:
		h.outcome = heldOutcome{kind: heldUnread, result: sandbox.HeldResult{Failed: l.Failed}}
		if result != nil {
			h.outcome = heldOutcome{kind: heldMade, result: *result}
		}
	case sandbox.LeadHeldDenied:
		h.outcome = heldOutcome{kind: heldDenied}
	case sandbox.LeadHeldNotMade:
		h.outcome = heldOutcome{kind: heldNotMade, why: l.Why}
	default:
		h.outcome = heldOutcome{kind: heldUnknown}
	}
	return h
}

// ownLeads is the leads a brief opens with of its own, each carrying the round
// it carries on, nils left out: what the launch hands the run's leads beside
// its record (sandbox.BriefTold.Own).
func ownLeads(round int, leads ...*sandbox.Lead) []sandbox.Lead {
	var out []sandbox.Lead
	for _, lead := range leads {
		if lead != nil {
			own := *lead
			own.Round = round
			out = append(out, own)
		}
	}
	return out
}

// reviewLead is the lead a send-back's continuation of the kept session opens
// with, as the run keeps it beside its record: the review of the round it
// closed, history's last, which a later brief recomposes from that round's
// notes ([Runner.leadAgain]). None where history holds no closed round.
func reviewLead(history []ledger.Iteration) []sandbox.Lead {
	if len(history) == 0 {
		return nil
	}
	return []sandbox.Lead{{Kind: sandbox.LeadKindReview, Round: history[len(history)-1].Iteration}}
}

// taskLead is the lead a continuation of the session its conversation's last
// turn left opens with, as the run keeps it beside its record: the turn's task,
// told with every closed round of history the brief's prior-work block rendered
// and, where notes is a review that sent the round back, carrying the review of
// the round it closed, history's last ([Runner.taskFor]), for round, the round
// the launch runs.
func taskLead(round int, history []ledger.Iteration, notes string) []sandbox.Lead {
	task := &sandbox.LeadTask{Rounds: len(history)}
	if strings.TrimSpace(notes) != "" && len(history) > 0 {
		task.Reviewed = history[len(history)-1].Iteration
	}
	return []sandbox.Lead{{Kind: sandbox.LeadKindTask, Round: round, Task: task}}
}

// withHeld is leadHeld, the held calls a brief's own lead tells what became of,
// and the ones every lead it tells again tells ([toldAgain.held]): no list of
// the round's calls in that brief tells the call a person approved for any of
// them ([Runner.calledAfter]).
func withHeld(leadHeld []string, again toldAgain) []string {
	if len(again.held) == 0 {
		return leadHeld
	}
	return slices.Concat(leadHeld, again.held)
}

// THE TURN'S ANSWERS, TOLD TO A RUN THAT HOLDS NONE OF THEM.
//
// A run that starts a session of its own holds none of the briefs before it: a
// fresh run carrying a round on (a loss, an answer or a replaced sign-in whose
// job's session its launch continues neither way, and a provider's refusal of
// the session its job continued, which no launch continues), a send-back that
// starts one, and the session its conversation's last turn left, handed either
// brief whole. Its brief told it the turn's task, its closed rounds' calls and
// reviews and every note, and of the answers a person gave the work only the
// newest, the one carrying its round on now, if any: the prior-work ledger
// carries calls, intents, text and reviews, never a person's reply, by design,
// since a review's evidence is the run's calls ([RefusedReply]). So an answer
// that carried an earlier job on, in this round or a round a review sent back,
// an instruction a person gave the work, was lost to the next session that
// started afresh.
//
// So such a brief tells every answer the run keeps beside its record, the
// turn's whole (sandbox.PendingStore.Leads), before what carries its round on
// now, oldest first, each naming the round it came in ([Runner.turnsAnswers]),
// in the words a lead is told again in ([answerAgain]); and the task a base
// continuation's lead keeps, told again to a session that may lack that brief,
// carries them as that brief told them, and all the same where that brief's
// read of them failed ([Runner.taskAgain]).

// answersTold is the answers a brief tells ([earlierAnswers]): the section that
// tells them, and the held calls they tell what became of, which no list of the
// round's calls in that brief tells ([Runner.calledAfter]).
type answersTold struct {
	section prompts.Prompt
	held    []string
}

// turnsAnswers is the turn's answers as the fresh brief of a run working in
// round current tells them ([earlierAnswers]), over history, the closed rounds
// its prior-work block lists as work already done: every answer lead the run
// keeps beside its record, oldest first, told or not by its job's brief, and
// how many earlier leads are not kept, past the bound or because the store
// could not give them back ([Resume.LeadsCut]). None where no collection
// drives the segment: a turn's first launch, which no answer has carried on
// yet.
//
// BUILT BEFORE THE ROUND'S CALLS ARE TOLD, so the call a person approved that a
// held answer among them tells is in no list of them ([Runner.calledAfter]).
func (r *Runner) turnsAnswers(current int, history []ledger.Iteration, b provenance.Boundary) answersTold {
	if r.cfg.Resume == nil {
		return answersTold{}
	}
	return earlierAnswers(r.cfg.Resume.Leads, current, history, r.cfg.Resume.LeadsCut, true, b)
}

// earlierAnswers is the answer leads among leads told by a brief to a run working
// in round current, under the relaunching segment's token b, in the order the
// run keeps them, oldest first: each in the words a lead is told again in
// ([answerAgain]), its heading naming the round it came in
// ([prompts.AnswerRound]), and one that did not open here said in its place
// ([prompts.AnswerUnread]); with how many earlier leads are not kept (cut), as
// [prompts.EarlierAnswers] tells them, fresh or told again.
//
// listed is the closed rounds the same brief lists the calls of as work already
// done: a held call an answer of one of them approved was made in that round,
// and is among its calls, so the answer points there rather than tell its
// outcome a second time ([answerAt]).
//
// EVERY KIND OF ANSWER, as a lead keeps it: a withdrawal, a refused reply and a
// held call a person allowed or denied are each what a person, or the seat
// itself, told the work, and a run told none of them could ask again what was
// answered, or call again what was denied.
func earlierAnswers(leads []ResumeLead, current int, listed []ledger.Iteration, cut int, fresh bool,
	b provenance.Boundary) answersTold {

	var (
		out      answersTold
		rendered []string
	)
	for _, told := range leads {
		lead := told.Lead
		if lead.Kind != sandbox.LeadKindAnswer {
			continue
		}
		named := prompts.AnswerRound(lead.Round, current)
		if lead.Unread != "" || lead.Answer == nil {
			rendered = append(rendered, prompts.AnswerUnread(named))
			continue
		}
		if held := lead.Answer.Held; held != nil && lead.Answer.Refused == nil {
			out.held = append(out.held, held.HeldID)
		}
		at := answerAt{named: named}
		if slices.ContainsFunc(listed, func(closed ledger.Iteration) bool { return closed.Iteration == lead.Round }) {
			at.listed = lead.Round
		}
		rendered = append(rendered, answerAgain(*lead.Answer, told.Result, at, b))
	}
	out.section = prompts.EarlierAnswers(rendered, cut, fresh)
	return out
}

// answersBefore is the leads of leads whose job came before place, the place
// among the turn's launches of the job that kept a task lead ([sandbox.Lead.Seq]):
// the leads the run kept when that job's brief was built, which that brief told
// the answers of, or said as not kept where its read of them failed
// ([Runner.taskAgain]).
func answersBefore(leads []ResumeLead, place int64) []ResumeLead {
	var out []ResumeLead
	for _, lead := range leads {
		if lead.Lead.Seq < place {
			out = append(out, lead)
		}
	}
	return out
}
