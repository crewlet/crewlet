package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/workkey"
)

// A busy seat's fold-in (ADR-0109's amendment "another writer's request joins
// only on approval"): a request the triage found in another writer's wake
// (ADR-0168) is put, once per wake and job, to the people who may let it join
// the running work on its requester's apps: the run's requester, where a
// person, and the seat's leads ([Engine.askFoldIn]). The question rides the
// run's row ([sandbox.FoldIn]) and is answered through `answer_executor`;
// approved, the wake joins as a steer note under the approver's name
// ([Engine.fileFoldIn]); refused, or unanswered when the job ends, it waits
// for a turn of its own, which it is still on the inbox for. The model never
// decides it. A person whose wake waits is told why, once ([Engine.tellWaiting]).

// foldInNamespace derives a fold-in's id from its turn, job and wake, so the
// question asked again is the same question.
var foldInNamespace = uuid.MustParse("8d4c2a7e-5f1b-5e3a-9c6d-1b7e4f2a9c35")

// foldInNoteNamespace derives the steer note an approved fold-in is filed as
// from its turn and wake, so a filing made again is the same note.
var foldInNoteNamespace = uuid.MustParse("2f6b9d1c-4e8a-5b7c-8d3e-6a1f9c4b2e07")

// Why a request stays, beside ADR-0168's, as seat_wake_left_queued says.
const (
	stayNoApprover    = "no_approver"    // neither the requester nor a lead of the seat is a person
	stayFoldInsSpent  = "foldins_spent"  // the job asked every fold-in it asks
	stayFoldInRefused = "foldin_refused" // a person decided it runs on its own
	stayFoldInExpired = "foldin_expired" // its job ended before anybody answered
)

// foldable reports whether a request from o, of facts, may be put as a
// fold-in: never the requester's own (the fixed rule joins those), and from a
// person or an agent seat by its handle, never a credential acting as no seat,
// whom nobody is asked anything for (ADR-0109).
func foldable(o wakeOrigin, f wakeFacts) bool {
	return !f.requester && o.login == "" && o.requester != ""
}

// approversOf is whom a fold-in of asker's request on run is put to: the run's
// requester where a person holds that seat, then the seat's leads nearest
// first ([leadChain]), people alone, never the asker, at most
// [sandbox.MaxFoldInPutTo]. An administrator may answer one too, as a run's
// question (internal/runanswer), but is not put it.
func (e *Engine) approversOf(run sandbox.PendingRun, asker string) []string {
	company := e.Company()
	if company == nil || company.Org == nil {
		return nil
	}
	o := company.Org
	var out []string
	add := func(handle string) {
		r := o.Role(handle)
		if r == nil || !r.IsHuman() || r.Handle() == asker || slices.Contains(out, r.Handle()) {
			return
		}
		out = append(out, r.Handle())
	}
	add(run.Requester)
	for _, handle := range leadChain(o, o.SeatByHandle(run.AgentHandle)) {
		add(handle)
	}
	return out[:min(len(out), sandbox.MaxFoldInPutTo)]
}

// askFoldIn puts w, a request left for a turn of its own, to the people who may
// let it join run's work ([approversOf]), on the row ([sandbox.PendingStore.AskFoldIn]).
// True where the wake waits on that question (asked, or a store that did not
// answer asked again next read); false, and why, where none is asked and it
// stays for a turn of its own.
func (e *Engine) askFoldIn(ctx context.Context, handle string, run sandbox.PendingRun, w steer.Wake,
	ev *events.Event, memo *routeMemo) (bool, string) {

	putTo := e.approversOf(run, w.By)
	if len(putTo) == 0 {
		return false, stayNoApprover
	}
	f := sandbox.FoldIn{ID: uuid.NewSHA1(foldInNamespace, []byte(run.TurnID+"/"+run.LaunchID+"/"+w.ID)).String(),
		Launch: run.LaunchID, Wake: w.ID, Asker: w.By, AskerKind: w.ByKind, Conversation: w.Conversation,
		Words: w.Text, Cut: w.Cut, Label: w.Label, PutTo: putTo}
	for {
		got, err := e.runSteers.store.AskFoldIn(ctx, run.TurnID, f)
		switch {
		case errors.Is(err, sandbox.ErrFoldInTooLarge) && f.Words != "":
			// THE WORDS ARE AN EXCERPT an approver reads, the delivery what
			// joins: halved until the row holds it, the cut said.
			runes := []rune(f.Words)
			keep := len(runes) / 2
			f.Words, f.Cut = string(runes[:keep]), f.Cut+len(runes)-keep
			continue
		case err == nil:
			memo.ask(run, w.ID)
			e.noteRouted(ctx, handle, run, w.ID, routeLeftQueued, ruleFoldInAsked, "", nil)
			log.InfoContext(ctx, "seat_wake_foldin_asked", "seat", handle, "turn_id", run.TurnID,
				"job", run.LaunchID, "wake_id", w.ID, "fold_in", got.ID, "asker", got.Asker, "put_to", got.PutTo)
			e.tellWaiting(ctx, handle, ev, w.ID, w.ByKind == iam.ActorHuman, approvalLine(got.PutTo))
			return true, ""
		case errors.Is(err, sandbox.ErrFoldInsSpent):
			return false, stayFoldInsSpent
		case errors.Is(err, sandbox.ErrFoldInClosed), errors.Is(err, sandbox.ErrFoldInTooLarge):
			return false, stayRefused
		}
		// THE STORE DID NOT ANSWER: undecided, so the next read asks again.
		log.WarnContext(ctx, "seat_wake_foldin_not_asked", "seat", handle, "turn_id", run.TurnID,
			"wake_id", w.ID, "error", err.Error(),
			"detail", "the run's record could not be reached; the next read asks again")
		return true, ""
	}
}

// followFoldIn acts on f, the fold-in run's job asked for w, as its row holds
// it now: an open one waits; a refusal leaves the wake for a turn of its own;
// an approval files it ([Engine.fileFoldIn]).
func (e *Engine) followFoldIn(ctx context.Context, handle string, run sandbox.PendingRun, w steer.Wake,
	f sandbox.FoldIn, ev *events.Event, memo *routeMemo) {

	memo.ask(run, w.ID)
	switch f.Answer {
	case sandbox.FoldInApprove:
		e.fileFoldIn(ctx, handle, run, w, f, ev, memo)
	case sandbox.FoldInRefuse:
		memo.mark(run.LaunchID, w.ID)
		memo.settle(run.LaunchID, w.ID)
		e.noteRouted(ctx, handle, run, w.ID, routeLeftQueued, ruleFoldInRefused, stayFoldInRefused, nil)
		log.InfoContext(ctx, "seat_wake_foldin_refused", "seat", handle, "turn_id", run.TurnID,
			"job", run.LaunchID, "wake_id", w.ID, "fold_in", f.ID, "approver", f.AnsweredBy,
			"detail", "the request runs as a turn of its own once the running work ends")
		e.tellWaiting(ctx, handle, ev, w.ID, w.ByKind == iam.ActorHuman, runsAfterLine)
	}
}

// fileFoldIn files approved fold-in f as ONE steer note under its approver's
// name (ADR-0067's seal and bounds, [prompts.FoldInNote]): who asked, who
// approved, and the asker's words in their fence, read from the delivery on
// the inbox, never the row's excerpt. An accepted note claims the wake there
// ([runSteers.took], [foldedKey]), as a lead's note does; one the turn holds
// no room for waits for the next read; past the turn's note cap it stays.
func (e *Engine) fileFoldIn(ctx context.Context, handle string, run sandbox.PendingRun, w steer.Wake,
	f sandbox.FoldIn, ev *events.Event, memo *routeMemo) {

	const rule = ruleFoldInApproved

	if e.runSteers.completions == nil {
		// NO NOTE IT COULD NOT CLAIM: its message would run again after it.
		memo.mark(run.LaunchID, w.ID)
		memo.settle(run.LaunchID, w.ID)
		e.noteRouted(ctx, handle, run, w.ID, routeLeftQueued, rule, stayNoLedger, nil)
		return
	}
	note := steer.Note{ID: uuid.NewSHA1(foldInNoteNamespace, []byte(run.TurnID+"/"+w.ID)).String(),
		Text: foldInText(w, f.AnsweredBy), By: f.AnsweredBy, ByKind: f.AnsweredByKind, At: f.AnsweredAt}
	if owed := turn.ParseReply(w.Owes); w.Owes != "" && owed.Valid() && owed.Awaited() {
		note.Owes = owed.String()
	}
	filed, err := e.runSteers.File(ctx, run.TurnID, note)
	if err != nil {
		log.WarnContext(ctx, "seat_wake_not_filed", "seat", handle, "turn_id", run.TurnID, "note_id", note.ID,
			"error", err.Error(), "detail", "the run's record could not be reached; the next read files the "+
				"approved request again")
		return
	}
	switch filed.Status {
	case steer.StatusAccepted:
		if err := e.runSteers.took(ctx, handle, run.TurnID, foldedKey, w); err != nil {
			log.WarnContext(ctx, "steer_wake_unclaimed", "turn_id", run.TurnID, "wake_id", w.ID,
				"error", err.Error(), "detail", "the approved request was filed, and the completion ledger did "+
					"not record its message worked: that message also runs as a turn of its own")
		}
		e.runSteers.seeThrough(ctx, handle, w)
		e.runSteers.handOver(ctx, handle, run.TurnID, chatlet.TakenJoined, w)
		memo.mark(run.LaunchID, w.ID)
		memo.settle(run.LaunchID, w.ID)
		e.noteRouted(ctx, handle, run, w.ID, routeJoined, rule, "", nil)
		log.InfoContext(ctx, "seat_wake_foldin_approved", "seat", handle, "turn_id", run.TurnID,
			"job", run.LaunchID, "wake_id", w.ID, "fold_in", f.ID, "asker", f.Asker, "approver", f.AnsweredBy,
			"note_id", note.ID)
	case steer.StatusFull:
		log.DebugContext(ctx, "seat_wake_foldin_held", "seat", handle, "turn_id", run.TurnID, "wake_id", w.ID,
			"detail", "the turn holds as many unread notes as it takes; the approved request waits for the next read")
	default:
		why := stayNotesSpent
		if filed.Status == steer.StatusClosed {
			why = stayRefused
		}
		memo.mark(run.LaunchID, w.ID)
		memo.settle(run.LaunchID, w.ID)
		e.noteRouted(ctx, handle, run, w.ID, routeRefused, rule, why,
			fmt.Errorf("the run's record answered %s", filed.Status))
		e.tellWaiting(ctx, handle, ev, w.ID, w.ByKind == iam.ActorHuman, runsAfterLine)
	}
}

// foldInText is approver's note for w ([prompts.FoldInNote]) under a fence
// token minted for it, its words cut until the note is one a note may be
// ([steer.MaxNoteRunes]), every character cut said.
func foldInText(w steer.Wake, approver string) string {
	b := provenance.NewBoundary()
	for {
		text := prompts.FoldInNote(w, approver, b)
		over := utf8.RuneCountInString(text) - steer.MaxNoteRunes
		runes := []rune(w.Text)
		if over <= 0 || len(runes) == 0 {
			return text
		}
		keep := max(len(runes)-over, 0)
		w.Text, w.Cut = string(runes[:keep]), w.Cut+len(runes)-keep
	}
}

// expireFoldIns says, for every fold-in memo saw asked of a job runs no longer
// holds that nobody's answer settled, that it closed unanswered: its job ended,
// so no later approval folds it into later work, and its wake runs as a turn
// of its own. Before [routeMemo.prune] forgets the job.
func (e *Engine) expireFoldIns(ctx context.Context, handle string, runs []sandbox.PendingRun, memo *routeMemo) {
	for job, wakes := range memo.foldIns {
		if slices.ContainsFunc(runs, func(run sandbox.PendingRun) bool { return run.LaunchID == job }) {
			continue
		}
		for _, wake := range slices.Sorted(maps.Keys(wakes)) {
			ended := sandbox.PendingRun{TurnID: wakes[wake], LaunchID: job}
			e.noteRouted(ctx, handle, ended, wake, routeLeftQueued, ruleFoldInExpired, stayFoldInExpired, nil)
			log.InfoContext(ctx, "seat_wake_foldin_expired", "seat", handle, "turn_id", ended.TurnID, "job", job,
				"wake_id", wake, "detail", "its job ended before anybody answered; the request runs as a turn of "+
					"its own")
		}
		delete(memo.foldIns, job)
	}
}

// routerEnded says, as handle's router ends, which fold-ins it saw asked closed
// unanswered with their job ([Engine.expireFoldIns]): read on a context of its
// own, since the run ending is what ended the router, within
// [routerStreamBudget]. A node stopping finds the run still there and says
// nothing, and the seat's next holder follows the question.
func (e *Engine) routerEnded(ctx context.Context, handle string, memo *routeMemo) {
	if len(memo.foldIns) == 0 {
		return
	}
	read, cancel := context.WithTimeout(context.WithoutCancel(ctx), routerStreamBudget)
	defer cancel()
	runs, err := e.runSteers.store.ListActiveForSeat(read, handle)
	if err != nil {
		log.DebugContext(read, "seat_router_runs_unread", "seat", handle, "error", err.Error())
		return
	}
	e.expireFoldIns(read, handle, routableRuns(runs), memo)
}

// foldedKey is the completion-ledger mark of a delivery an approved fold-in
// filed as its steer note on a running turn ([skipFolded]).
func foldedKey(trigger string) string { return workkey.Derive([]string{"wake-folded", trigger}) }

// foldedReason is a folded skip's reason, naming the turn where the ledger does.
func foldedReason(turnID string) string {
	return "a person approved adding it to " + runningTurn(turnID) + ", where it was filed as their steer note: " +
		"the turn's agent_turn_steered record says what became of it"
}

// The lines a waiting person is told ([Engine.tellWaiting]), beside the seat's
// name, as the queued indicator reads.
const runsAfterLine = "is working on other work; this runs after it"

// approvalLine is the line telling a person their request waits for one of
// putTo to approve it joining the running work.
func approvalLine(putTo []string) string {
	said := make([]words.Said, 0, len(putTo))
	for _, handle := range putTo {
		said = append(said, words.Said(handle))
	}
	return "is waiting for " + words.Or(said) + " to approve adding this to the work it is doing"
}

// waitsTold is what each waiting wake was told, by seat and wake, so a person
// is told each reason once however often the seat's inbox is read; forgotten
// once the wake runs or joins ([Engine.settleWaiting]).
type waitsTold struct {
	mu   sync.Mutex
	told map[string]string
}

// tell reports whether why is news for wake of handle, and remembers it.
func (t *waitsTold) tell(handle, wake, why string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := handle + "\x00" + wake
	if t.told[key] == why {
		return false
	}
	if t.told == nil {
		t.told = map[string]string{}
	}
	t.told[key] = why
	return true
}

// forget drops what wakes of handle were told.
func (t *waitsTold) forget(handle string, wakes []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, wake := range wakes {
		delete(t.told, handle+"\x00"+wake)
	}
}

// forgetSeat drops what every wake of handle was told: the node no longer
// runs the seat, and its indicators came down with it.
func (t *waitsTold) forgetSeat(handle string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for key := range t.told {
		if strings.HasPrefix(key, handle+"\x00") {
			delete(t.told, key)
		}
	}
}

// first reports whether wake of handle was told no reason yet, and remembers
// why where it was not: a reason already told stands.
func (t *waitsTold) first(handle, wake, why string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := handle + "\x00" + wake
	if _, told := t.told[key]; told {
		return false
	}
	if t.told == nil {
		t.told = map[string]string{}
	}
	t.told[key] = why
	return true
}

// tellWaiting tells the person who wrote ev, wake waiting behind handle's
// running work, why it waits, once per wake and reason, on the conversation's
// queued indicator ([notify.Statuses.Explain]). Only a person's message, the
// one a queued line is drawn for.
func (e *Engine) tellWaiting(ctx context.Context, handle string, ev *events.Event, wake string, person bool,
	why string) {
	e.explainWait(ctx, handle, ev, wake, person, why, e.waitsTold.tell)
}

// explainWait is [Engine.tellWaiting] where news says why is news for wake.
func (e *Engine) explainWait(ctx context.Context, handle string, ev *events.Event, wake string, person bool,
	why string, news func(handle, wake, why string) bool) {
	if ev == nil || !person {
		return
	}
	n, ok := events.DataAs[*types.ExternalNotification](ev)
	if !ok || n == nil || !news(handle, wake, why) {
		return
	}
	e.Status().Explain(context.WithoutCancel(ctx), handle, wake, n.Metadata, why)
}
