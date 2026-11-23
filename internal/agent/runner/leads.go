package runner

import (
	"context"
	"math"
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
// A brief opens with its lead: an answer ([Asked]), a send-back's review
// ([prompts.BuildContinuedRound]) or the turn's task ([prompts.BuildNextTurn]).
// A session whose harness did not end its own run ([Runner.copyMayLackBrief],
// [SessionBasis.MayLackBrief]) may lack that brief, so the launch keeps each
// brief's leads beside the run's record (sandbox.BriefTold), and a continuation
// of such a session is told them again ([prompts.LeadsToldAgain]) and marks them
// told in turn. IN WORDS TRUE OF ANY SESSION OF THE WORK ("a run of this work
// asked"), never "you" or "this box": a lead may reach a session that never
// asked, a copy of another job, and a summary is told every lead, another
// lineage's too, so no record says which session a lead's brief went to.

// copyMayLackBrief reports whether, by the record, the session copy in the
// job's own box may lack its job's brief: unless the job's harness is known to
// have ended its own run ([Resume.HarnessEnd]). It is the record's half of
// [sessionHolds]' verdict, which also weighs a restart in the job and whether
// its harness may compact the copy; every telling of the in-place path reads
// that one verdict.
func (r *Runner) copyMayLackBrief() bool {
	return r.cfg.Resume == nil || !r.cfg.Resume.HarnessEnd.EndedOwnRun()
}

// toldAgain is what a brief to a session that may lack its job's brief tells
// again of that brief's leads ([Resume.Leads]). section tells them
// ([prompts.LeadsToldAgain]), "" for none. refs is every lead it was to tell,
// for the job's record to mark told in turn (sandbox.BriefTold.Told), one not
// opened here and a review [reviewOf] no longer keeps among them, so the next
// continuation says each in its place, never silently; unread, the numbers of
// those it could not read (sandbox.BriefTold.ToldUnread, [Resume.LeadsUnread]).
// held is the held calls its leads tell the outcome of, and the calls they tell
// their runs parked on ([toldOf]), which no list of the round's calls repeats
// ([Runner.calledAfter]); rendered, how many entries of the closed rounds, from
// the first, a task told again renders as work done (the most of any), whose
// calls no list repeats ([Runner.closedShown]).
type toldAgain struct {
	section  string
	refs     []sandbox.LeadRef
	unread   []int
	held     []toldHeld
	rendered int
}

// leadsAgain tells again, oldest first, the leads a session that may lack its
// job's brief was told ([toldAgain]), each by [Runner.leadAgain] under token b
// to a run in round current, with how many earlier ones are not kept
// ([Resume.LeadsCut]). Where all says the session may be a summary
// ([sessionHolds.summarized]) it tells every kept lead: a compaction can drop
// any, and the record cannot say which. EACH ONCE: an answer a task told here
// carries ([answersNested]) is told only under that task, else a held call's
// outcome is two messages and the call is made twice; its ref is still handed
// on if the section tells it of its own account, for a later brief whose task
// does not open. One never marked told stays unmarked: that task carries it on.
// BUILT BEFORE THE BRIEF'S LEAD: a task's prior-work ledger can spend the seat's
// auxiliary model, charged before the relaunch's last ask of the seat's budget.
func (r *Runner) leadsAgain(ctx context.Context, history []ledger.Iteration, current int,
	b provenance.Boundary, all bool) toldAgain {
	if r.cfg.Resume == nil {
		return toldAgain{}
	}
	var (
		// The unread ones are lacked as surely: said among the leads not
		// kept and handed on, for a later read to tell.
		out      = toldAgain{unread: slices.Clone(r.cfg.Resume.LeadsUnread)}
		rendered []string
		tells    = oneAsk(r.cfg.Resume.Leads, func(told ResumeLead) bool { return told.Told || all })
		nested   = answersNested(r.cfg.Resume.Leads, tells)
	)
	for _, told := range r.cfg.Resume.Leads {
		if !tells(told) {
			continue
		}
		lead := told.Lead
		out.refs = append(out.refs, sandbox.LeadRef{Job: lead.Job, Kind: lead.Kind})
		if nested.carries(lead) {
			continue
		}
		if lead.Unread == "" && lead.Answer != nil {
			out.held = append(out.held, toldOf(lead)...)
		}
		text, held, shown := r.leadAgain(ctx, told, nested, history, current, b)
		rendered, out.held = append(rendered, text), append(out.held, held...)
		out.rendered = max(out.rendered, shown)
	}
	out.section = prompts.LeadsToldAgain(rendered, r.cfg.Resume.LeadsCut)
	return out
}

// tasksTold is the places ([sandbox.Lead.Seq]) of the task leads a section
// tells again, oldest first: which of them carries each earlier answer
// ([tasksTold.carries], [Runner.taskAgain]).
type tasksTold []int64

// answersNested is the task leads among leads that the section tells (per
// tells), and so the answers they carry: only a task that opens here, which
// [Runner.taskAgain] renders; one that does not is said unreadable and carries
// nothing.
func answersNested(leads []ResumeLead, tells func(ResumeLead) bool) tasksTold {
	var tasks tasksTold
	for _, told := range leads {
		if tells(told) && (toldAsTask(told.Lead) || toldAsFresh(told.Lead)) {
			tasks = append(tasks, told.Lead.Seq)
		}
	}
	slices.Sort(tasks)
	return tasks
}

// toldAsTask reports whether lead is told again as the turn's task, its
// answers nested under it ([Runner.taskAgain]): a task lead that opened here.
// ONE TEST for [answersNested] and [Runner.leadAgain]: were they to disagree,
// the answers that task carries would be told nowhere.
func toldAsTask(lead sandbox.Lead) bool {
	return lead.Unread == "" && lead.Kind == sandbox.LeadKindTask && lead.Task != nil
}

// toldAsFresh reports whether lead is told again as the task a fresh brief
// opened with, its answers nested under it as a task lead's are
// ([toldAsTask], [Runner.freshAgain]): a fresh lead that opened here.
func toldAsFresh(lead sandbox.Lead) bool {
	return lead.Unread == "" && lead.Kind == sandbox.LeadKindFresh && lead.Task != nil
}

// oneAsk is tells narrowed so that a section tells the turn's ask once (see
// the package doc): no fresh lead where a task lead that opens here is told,
// else one, the newest that opens here, or the newest where none does.
func oneAsk(leads []ResumeLead, tells func(ResumeLead) bool) func(ResumeLead) bool {
	kept := -1
	for i, told := range leads {
		switch {
		case !tells(told):
		case toldAsTask(told.Lead):
			return func(t ResumeLead) bool { return tells(t) && t.Lead.Kind != sandbox.LeadKindFresh }
		case told.Lead.Kind == sandbox.LeadKindFresh &&
			(kept < 0 || toldAsFresh(told.Lead) || !toldAsFresh(leads[kept].Lead)):
			kept = i
		}
	}
	if kept < 0 {
		return tells
	}
	job := leads[kept].Lead.Job
	return func(t ResumeLead) bool {
		return tells(t) && (t.Lead.Kind != sandbox.LeadKindFresh || t.Lead.Job == job)
	}
}

// carries reports whether a task told again carries lead, an answer: whether
// any such task's job came after the answer's ([sandbox.Lead.Seq]). Only the
// OLDEST of them tells it ([tasksTold.from]), else a summary told several tasks
// tells one answer under each. Never an answer of the task's own job, which
// carried that job on and is told after it as a lead of its own.
func (t tasksTold) carries(lead sandbox.Lead) bool {
	return answerKind(lead.Kind) && len(t) > 0 && lead.Seq < t[len(t)-1]
}

// answerKind reports whether a lead of kind is told by [answerAgain]: a
// person's answer, or the engine's to a wait or to calls the run parked on,
// each sealed under a kind of its own (sandbox.LeadKindCalled).
func answerKind(kind sandbox.LeadKind) bool {
	return kind == sandbox.LeadKindAnswer || kind == sandbox.LeadKindCalled || kind == sandbox.LeadKindWaited
}

// from is where the task told again at place starts carrying earlier answers
// ([answersBetween]): the place of the newest task told before it, or the
// least place for the oldest.
func (t tasksTold) from(place int64) int64 {
	from := int64(math.MinInt64)
	for _, earlier := range t {
		if earlier < place {
			from = earlier
		}
	}
	return from
}

// leadAgain is one lead told again, under its own heading, to a run in round
// current: an answer by [answerAgain]; a review as its send-back told it
// ([prompts.ContinuedRound.Again]); the task by [Runner.taskAgain], from where
// tasks says the one before it leaves off; else [prompts.LeadUnread]. A review
// or task nests by its builder's headings ([prompts.Prompt.Nested]), never
// moving a byte of the words inside, which are somebody else's. held and
// rendered are the task's alone ([toldAgain], [withHeld]).
//
// A REVIEW NOT KEPT ([reviewOf]) IS SAID IN ITS PLACE ([prompts.ReviewNotKept]):
// read off its round's notes it says nothing, as unreadable it claims a copy
// the engine holds, and among the leads not kept it would sort before the task
// it can follow, which would then seem to stand over it.
func (r *Runner) leadAgain(ctx context.Context, told ResumeLead, tasks tasksTold, history []ledger.Iteration,
	current int, b provenance.Boundary) (text string, held []toldHeld, rendered int) {

	lead := told.Lead
	switch {
	case lead.Unread != "":
	case answerKind(lead.Kind) && lead.Answer != nil:
		return answerAgain(lead.Kind, *lead.Answer, told.Result, answerAt{}, b), nil, 0
	case lead.Kind == sandbox.LeadKindReview:
		if notes, found := reviewOf(history, lead.Round); found {
			return prompts.BuildContinuedRound(prompts.ContinuedRound{Task: r.cfg.Task, Review: notes,
				Again: true}).Nested().Text, nil, 0
		}
		return prompts.ReviewNotKept, nil, 0
	case toldAsFresh(lead):
		return r.freshAgain(ctx, lead, tasks.from(lead.Seq), history, current, b)
	case toldAsTask(lead):
		return r.taskAgain(ctx, lead, tasks.from(lead.Seq), history, current, b)
	}
	return prompts.LeadUnread, nil, 0
}

// reviewOf is the review notes that closed round iteration of history, and
// whether history keeps that round whole ([Runner.sendBackLead]). A span a park
// folded ([ledger.Iteration.Through]) keeps calls by tool and no prose
// (internal/agent/execstate), so a round in one, its first included, is not
// kept, never read as one whose review said nothing.
func reviewOf(history []ledger.Iteration, iteration int) (string, bool) {
	for _, round := range history {
		if round.Iteration == iteration && round.Through == 0 {
			return round.ReviewNotes, true
		}
	}
	return "", false
}

// taskAgain is a task lead told again to a run in round current: where the turn
// began after the base turn ([Resume.Base], defused under b), as
// [prompts.NextTurnAgain] tells any session; the turn's ask with only the closed
// rounds its brief rendered ([Runner.turnAsk], [roundsThrough]), since a later
// round's calls are told by their own rule; a review a park's span has taken
// since is "", the span's words saying it is gone. Then the answers that brief
// told, in its order ([earlierAnswers]): of jobs before the lead's own, from
// from on so two tasks never tell one ([tasksTold.from]), even where its read of
// them failed (sandbox.TurnLeads.Unread). Told apart they would sort before the
// task and seem overridden by it. held is the held calls they tell the outcome
// of; rendered, how many entries of history the task renders as work done,
// whose calls no list of the brief repeats ([Runner.closedShown]).
func (r *Runner) taskAgain(ctx context.Context, lead sandbox.Lead, from int64, history []ledger.Iteration,
	current int, b provenance.Boundary) (string, []toldHeld, int) {

	m := prompts.NextTurn{Fire: notify.Scheduled(r.cfg.Turn.ConversationKey)}
	if base := r.cfg.Resume.Base; base != nil {
		m.BaseTurn, m.EndedAt, m.CapturedAt = provenance.Neutralise(b, base.Turn), base.EndedAt, base.CapturedAt
		m.WentOn, m.Lost, m.Interrupted = base.WentOn, base.Lost, base.Interrupted
	}
	return r.askAgain(ctx, prompts.NextTurnAgain(m), lead, from, history, current, b)
}

// freshAgain is a fresh lead told again, to a run working in round current:
// the turn's ask as the fresh brief rendered it, with the answers that brief
// told, as [Runner.taskAgain] tells a task's, under [prompts.FreshTaskAgain]
// rather than a turn boundary, which that brief never said.
func (r *Runner) freshAgain(ctx context.Context, lead sandbox.Lead, from int64, history []ledger.Iteration,
	current int, b provenance.Boundary) (string, []toldHeld, int) {
	return r.askAgain(ctx, prompts.FreshTaskAgain, lead, from, history, current, b)
}

// askAgain is the turn's ask a task or fresh lead's brief rendered, told again
// under heading ([Runner.taskAgain], [Runner.freshAgain]).
func (r *Runner) askAgain(ctx context.Context, heading string, lead sandbox.Lead, from int64,
	history []ledger.Iteration, current int, b provenance.Boundary) (string, []toldHeld, int) {

	task := *lead.Task
	rendered := roundsThrough(history, task.Rounds)
	reviewed := ""
	if task.Reviewed > 0 {
		reviewed, _ = reviewOf(history, task.Reviewed)
	}
	told := heading + "\n\n" + r.turnAsk(ctx, rendered, reviewed).Nested().Text
	answers := earlierAnswers(answersBetween(r.cfg.Resume.Leads, from, lead.Seq), current, rendered, 0, false, b)
	if answers.section.Text != "" {
		told += "\n\n" + answers.section.Nested().Text
	}
	return told, answers.held, len(rendered)
}

// roundsThrough is the entries of history up to round, the last one a brief
// rendered ([sandbox.LeadTask.Rounds]): each whose first round is at or before
// it. BY ROUND NUMBER, never entry count: a later park's span
// ([ledger.Iteration.Through]) makes entries fewer than rounds. A span running
// past round is included: it holds rounds the brief rendered, whose calls are
// told no other way, so it also tells a later round's calls, counted by tool.
func roundsThrough(history []ledger.Iteration, round int) []ledger.Iteration {
	n := 0
	for n < len(history) && history[n].Iteration <= round {
		n++
	}
	return history[:n]
}

// turnAsk is the turn's ask as a fresh brief renders it ([Runner.executorBrief])
// with no note: the task (with notes where a review sent the round back), the
// prior-work block of history, the earlier turns' endings and the seat's access
// changes. Notes are told again by their own rule ([Runner.toldNotes]), never
// inside a lead.
func (r *Runner) turnAsk(ctx context.Context, history []ledger.Iteration, notes string) prompts.Prompt {
	return prompts.BuildPhaseUserMessage(prompts.UserMessage{
		TaskDescription:   r.taskFor(notes),
		PriorWork:         r.priorWork(ctx, history),
		Endings:           r.cfg.Endings,
		CapabilityChanges: r.cfg.CapabilityChanges,
	})
}

// answerAt is where an answer stands in the brief that tells it ([answerAgain]):
// named, its round as the heading names it ([prompts.AnswerRound]), "" where the
// brief names none; round, that round; done, the entry telling that round's
// calls as work done in the same brief, nil for none ([doneFor]). A held call
// made in that round is then among the entry's calls, and a closed round's list
// keeps no held id to leave it out by, so the answer defers to the list where
// it says how the call ended ([heldIn], [prompts.HeldCallListed]) rather than
// tell the call twice; where the entry may only count it, the answer tells its
// outcome as one of the entry's calls ([prompts.HeldCallCounted]).
type answerAt struct {
	named string
	round int
	done  *ledger.Iteration
}

// heldListing is how the entry a brief tells a round's calls under, as work
// already done, holds the call an answer of that round let be made ([heldIn]).
type heldListing int

const (
	// heldUnlisted: no entry stands for the round, or it holds no call of the
	// tool (the call's record did not land): the answer tells the outcome.
	heldUnlisted heldListing = iota
	// heldListed: the round kept whole lists the call, and its line says how
	// it ended: the answer points to it ([prompts.HeldCallListed]).
	heldListed
	// heldCounted: the entry may count the call rather than list it, so it may
	// not say how the call ended: the answer tells that, as one of the entry's
	// calls ([prompts.HeldCallCounted]).
	heldCounted
)

// doneFor is the entry of done, the closed rounds a brief tells as work already
// done, that stands for round: the round kept whole, or the span of rounds a
// park folded it into ([ledger.Iteration.Through]), nil where none does.
func doneFor(done []ledger.Iteration, round int) *ledger.Iteration {
	for i := range done {
		if done[i].Iteration <= round && round <= max(done[i].Iteration, done[i].Through) {
			return &done[i]
		}
	}
	return nil
}

// heldIn is how done, the entry telling a round's calls as work done
// ([doneFor]), holds a call to tool an answer of that round let be made.
//
// LISTED ONLY WHERE NOTHING BUT THE LIST CAN HOLD IT: a round's list keeps no
// held id, so the round must be kept whole, list a sealed call of the tool
// ([ledger.Call.Withheld]), and hold nothing that could count it instead. A
// span lists no call; a tally counts by tool, listing none (calls the record
// dropped, or all a park folded in; internal/agent/execstate), and one naming
// no tool may hold any; a read past the cap is counted
// ([ledger.FormatOptions.MaxReadCalls]). Pointed to such an entry, the answer
// would cite a list that does not say how the call ended, and its outcome would
// be told nowhere.
func heldIn(done *ledger.Iteration, tool string) heldListing {
	switch {
	case done == nil:
		return heldUnlisted
	case done.Through != 0, slices.Contains(done.Reads, tool),
		slices.ContainsFunc(done.Elided, func(e ledger.Elided) bool {
			return e.Calls > 0 && (e.Name == "" || e.Name == tool)
		}):
		return heldCounted
	case slices.ContainsFunc(done.Calls, func(c ledger.Call) bool { return c.Withheld && c.Name == tool }):
		return heldListed
	}
	return heldUnlisted
}

// answerAgain is an answer lead of kind told again under fence token b,
// standing where at says: the question and the answer, the withdrawal, the
// refused reply, the held call and its outcome, the wait's account or the
// calls that returned, in words true of any session of the work (the file
// comment says why). result is what a held call returned, opened, nil where it
// did not open here.
//
// A REPLY IS FENCED UNDER WHO WROTE IT, AN ANSWER BY TURN DEFUSED, as the lead
// was ([Asked.answered]): the run reads a chat reply as its sender's
// information and an answer by turn as its answerer's own instruction
// (ADR-0040, ADR-0068), whichever relaunch tells it.
func answerAgain(kind sandbox.LeadKind, a sandbox.LeadAnswer, result *sandbox.HeldResult, at answerAt,
	b provenance.Boundary) string {

	told := answerItself(kind, a, result, at, b)
	if len(a.Called) == 0 || kind == sandbox.LeadKindCalled {
		return told
	}
	// EACH CALL THE RUN PARKED ON BESIDE IT, once, as its own calls.
	return told + "\n\n" + calledAgain(a, "### Calls of this work that ran on", b)
}

// calledAgain is the calls a lead's run parked on, told again under heading,
// as the relaunch told them ([Asked.calledAccount]), defused under b. The
// heading never says they returned: the account says of each whether it did
// or its outcome is not known, which the lead keeps nothing else of.
func calledAgain(a sandbox.LeadAnswer, heading string, b provenance.Boundary) string {
	told := heading + "\n" + provenance.Neutralise(b, a.Returned)
	if a.ReturnedCut > 0 {
		told += "\n\nThis account is cut here: " + kept.SizeUp(a.ReturnedCut) + " more of it is not kept to " +
			"tell again."
	}
	return told
}

// answerItself is [answerAgain] but for the calls its run parked on beside an
// answer that was not theirs.
func answerItself(kind sandbox.LeadKind, a sandbox.LeadAnswer, result *sandbox.HeldResult, at answerAt,
	b provenance.Boundary) string {

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
		listing := heldUnlisted
		if h.outcome.kind == heldMade || h.outcome.kind == heldUnread {
			listing = heldIn(at.done, h.Tool)
		}
		switch listing {
		case heldListed:
			w.WriteString(prompts.HeldCallListed(at.round))
		case heldCounted:
			w.WriteString(prompts.HeldCallCounted(at.round, at.done.Iteration, at.done.Through) + h.told(b))
		default:
			w.WriteString(h.told(b))
		}
	case a.Waited != nil:
		w.WriteString("### A run this work waited for" + at.named + "\n" +
			Asked{Question: a.Question, Waited: a.Waited}.waited(b, "A run of this work"))
		return w.String()
	case kind == sandbox.LeadKindCalled:
		w.WriteString(calledAgain(a, "### Calls this work waited for"+at.named, b))
		return w.String()
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
// brief to a session that may lack this one: the question, the words cut to
// [sandbox.MaxLeadWordsBytes] with the cut counted, who answered and who wrote
// them, and a held call's outcome and result as its row sealed it. BUILT ONCE
// THE CALL IS MADE ([makeHeld]), so a later brief tells the outcome this
// relaunch told. A refused reply names no held call ([Asked.tellsHeld]). The
// engine's answer to a wait or to calls that returned is sealed under a kind
// of its own, which an older build tells as unreadable, never as a person's.
func (a *Asked) lead() *sandbox.Lead {
	words, cut := keptWords(a.Answer)
	answer := &sandbox.LeadAnswer{Question: a.Question, Words: words, Cut: cut, By: a.By, Label: a.Label,
		Withdrawn: a.Withdrawn, Waited: a.Waited, Called: a.Called}
	kind := sandbox.LeadKindAnswer
	switch {
	case a.Waited != nil:
		kind = sandbox.LeadKindWaited
	case a.CallsReturned:
		kind, answer.Words, answer.Cut = sandbox.LeadKindCalled, "", 0
	}
	if len(a.Called) > 0 {
		// WHAT THE CALLS RETURNED, kept as an answer's words are.
		answer.Returned, answer.ReturnedCut = keptWords(a.calledAccount("A run of this work", "its"))
	}
	if r := a.Refused; r != nil {
		answer.Refused = &sandbox.LeadRefused{Messages: r.Messages, Bytes: r.Bytes, MaxBytes: r.MaxBytes}
	} else if a.Held != nil {
		answer.Held = a.Held.lead()
	}
	return &sandbox.Lead{Kind: kind, Answer: answer}
}

// lead is the held call as its answer's lead keeps it ([sandbox.LeadHeld]): the
// call, the person's choice, what became of it ([makeHeld]) and what it returned
// as the run's row sealed it.
func (h *HeldCall) lead() *sandbox.LeadHeld {
	return &sandbox.LeadHeld{Tool: h.Tool, Server: h.Server, HeldID: h.HeldID, Choice: h.Choice,
		Outcome: h.outcome.kind.leadOutcome(), Failed: h.outcome.result.Failed, Why: h.outcome.why,
		Result: h.kept}
}

// leadOutcome is k as a lead keeps it. A call not yet made (no lead is built of
// one) and any other kind map to an outcome not known, the safe direction.
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

// heldAgain is the held call a lead kept, shaped for [HeldCall.what] and
// [HeldCall.told]: result is its result, opened here; a call made whose result
// did not open here is told as recorded but unreadable on this node, with
// whether it failed; an outcome this build does not know, as effect unknown.
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
// turn left opens with, kept beside the run's record for round, the round the
// launch runs: the turn's task, with the last closed round its prior-work block
// rendered ([lastRound]), and, where notes is a review that sent the round back,
// that review's round, history's last ([Runner.taskFor]).
func taskLead(round int, history []ledger.Iteration, notes string) []sandbox.Lead {
	task := &sandbox.LeadTask{Rounds: lastRound(history)}
	if strings.TrimSpace(notes) != "" && len(history) > 0 {
		task.Reviewed = history[len(history)-1].Iteration
	}
	return []sandbox.Lead{{Kind: sandbox.LeadKindTask, Round: round, Task: task}}
}

// freshLead is the lead a fresh brief opens a session of the run's own with, as
// the run keeps it beside its record: the turn's task, with the same facts as
// a task lead's ([taskLead]), for round, the round the launch runs.
func freshLead(round int, history []ledger.Iteration, notes string) []sandbox.Lead {
	lead := taskLead(round, history, notes)
	lead[0].Kind = sandbox.LeadKindFresh
	return lead
}

// lastRound is the number of the last closed round history holds, zero for none:
// its last entry's own, or the last round of the span a park folded it into
// ([ledger.Iteration.Through]). A number, never how many entries history holds,
// which a park that folds rounds into a span makes fewer ([roundsThrough]).
func lastRound(history []ledger.Iteration) int {
	if len(history) == 0 {
		return 0
	}
	last := history[len(history)-1]
	return max(last.Iteration, last.Through)
}

// withHeld is leadHeld, the held calls a brief's own lead tells the outcome of,
// plus those its told-again leads tell ([toldAgain.held]): no list of the
// brief's calls tells an approved call of any of them ([Runner.calledAfter]) or
// relists a closed round's withheld call of their round and tool
// ([Runner.closedShown]).
func withHeld(leadHeld []toldHeld, again toldAgain) []toldHeld {
	if len(again.held) == 0 {
		return leadHeld
	}
	return slices.Concat(leadHeld, again.held)
}

// THE TURN'S ANSWERS, TOLD TO A RUN THAT HOLDS NONE OF THEM.
//
// A run that starts a session of its own (a fresh run carrying a round on, a
// send-back that starts one, the session its conversation's last turn left)
// holds no earlier brief, and the prior-work ledger carries no person's reply,
// by design: a review's evidence is the run's calls ([RefusedReply]). So its
// brief tells every answer kept beside the run's record
// (sandbox.PendingStore.Leads), oldest first, each naming its round, before
// what carries the round on now ([Runner.turnsAnswers], [answerAgain]); and a
// task told again carries the answers its brief told, even where that brief's
// read of them failed ([Runner.taskAgain]).

// answersTold is the answers a brief tells ([earlierAnswers]): the section that
// tells them, and the held and parked calls they tell what became of ([toldOf]),
// which no list of the round's calls in that brief tells ([Runner.calledAfter]).
type answersTold struct {
	section prompts.Prompt
	held    []toldHeld
}

// turnsAnswers is the turn's answers as the fresh brief of a run in round
// current tells them ([earlierAnswers]), over history, the closed rounds its
// prior-work block lists: every answer lead kept beside the run's record,
// oldest first, told or not, and how many earlier ones are not kept, past the
// bound or unreadable ([Resume.LeadsCut]). None without a collection driving
// the segment: a turn's first launch, which no answer has carried on yet.
// BUILT BEFORE THE ROUND'S CALLS ARE TOLD, so an approved call a held answer
// tells is in no list of them ([Runner.calledAfter]).
func (r *Runner) turnsAnswers(current int, history []ledger.Iteration, b provenance.Boundary) answersTold {
	if r.cfg.Resume == nil {
		return answersTold{}
	}
	return earlierAnswers(r.cfg.Resume.Leads, current, history, r.cfg.Resume.LeadsCut, true, b)
}

// earlierAnswers is the answer leads among leads as a brief tells them to a run
// in round current, under token b, oldest first: each by [answerAgain], headed
// with its round ([prompts.AnswerRound]), one not opened here said in its place
// ([prompts.EarlierUnread]), with cut earlier leads not kept, as
// [prompts.EarlierAnswers] tells them, fresh or told again. listed is the
// closed rounds the same brief tells the calls of: an answer's held call is
// among its round's, so the answer defers to that list ([answerAt], [heldIn]).
//
// EVERY KIND OF ANSWER: a withdrawal, a refused reply and an allowed or denied
// held call are each what a person or the seat told the work, and a run told
// none of them could ask again what was answered, or call again what was
// denied.
func earlierAnswers(leads []ResumeLead, current int, listed []ledger.Iteration, cut int, fresh bool,
	b provenance.Boundary) answersTold {

	var (
		out      answersTold
		rendered []prompts.EarlierLead
	)
	for _, told := range leads {
		lead := told.Lead
		if !answerKind(lead.Kind) {
			continue
		}
		named := prompts.AnswerRound(lead.Round, current)
		kind := earlierKind(lead.Kind)
		if lead.Unread != "" || lead.Answer == nil {
			rendered = append(rendered, prompts.EarlierLead{Text: prompts.EarlierUnread(kind, named), Kind: kind})
			continue
		}
		out.held = append(out.held, toldOf(lead)...)
		at := answerAt{named: named, round: lead.Round, done: doneFor(listed, lead.Round)}
		rendered = append(rendered, prompts.EarlierLead{Text: answerAgain(lead.Kind, *lead.Answer, told.Result, at,
			b), Kind: kind})
	}
	out.section = prompts.EarlierAnswers(rendered, cut, fresh)
	return out
}

// earlierKind is what a lead of kind told among the earlier ones was, for the
// section's intro ([prompts.EarlierAnswers]): the engine's account of the run
// a wait was for or of the calls the run parked on, never a person's answer.
func earlierKind(kind sandbox.LeadKind) prompts.EarlierKind {
	switch kind {
	case sandbox.LeadKindWaited:
		return prompts.EarlierWait
	case sandbox.LeadKindCalled:
		return prompts.EarlierCalls
	}
	return prompts.EarlierAnswer
}

// toldOf is the calls lead, an answer lead opened here, tells what became of,
// which no list of its brief tells: its held call ([sandbox.LeadAnswer.Held]),
// by id and tool, but where a refused reply carried it on, which tells only
// the refusal; and every call its run parked on ([sandbox.LeadAnswer.Called],
// [parkedOn]). Each of the round the answer carried on, where it was made.
func toldOf(lead sandbox.Lead) []toldHeld {
	var out []toldHeld
	if a := lead.Answer; a.Held != nil && a.Refused == nil {
		out = append(out, toldHeld{id: a.Held.HeldID, tool: a.Held.Tool, round: lead.Round})
	}
	return append(out, parkedOn(lead.Answer.Called, lead.Round)...)
}

// answersBetween is the leads whose job came before place, the task lead's job's
// place among the turn's launches ([sandbox.Lead.Seq]), and at or after from,
// where the task told before it leaves off ([tasksTold.from]): the leads that
// task's brief told, or said not kept ([Runner.taskAgain]), that no earlier
// task of the section tells.
func answersBetween(leads []ResumeLead, from, place int64) []ResumeLead {
	var out []ResumeLead
	for _, lead := range leads {
		if from <= lead.Lead.Seq && lead.Lead.Seq < place {
			out = append(out, lead)
		}
	}
	return out
}
