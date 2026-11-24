package engine

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The runs in flight around an executor's run, as it sees and reaches them
// (ADR-0176): the read its launch's brief lists and `list_runs` answers, a
// message `message_run` files on another run's record as an item of its own
// sealed in [SteerRunDomain], and the park `wait_for_run` records, answered by
// this node once the run waited for leaves its seat's slots (runwaits.go).
// Every refusal is decided here, on the rows; the tools only word the call.

// runsDesk is the engine's [builtin.RunsDesk] and the runner's
// [runner.RunsAround], over this node's sandbox runtime as it stands at each
// call.
type runsDesk struct {
	engine *Engine
	tools  []tools.Callable

	// links is the references of the items linked to an item; nil is the
	// engine's tracker ([runsDesk.linkedTo]), and a case hands its own.
	links func(ctx context.Context, item types.WorkItem) []string
}

var (
	_ builtin.RunsDesk  = (*runsDesk)(nil)
	_ runner.RunsAround = (*runsDesk)(nil)
)

// newRunsDesk is the desk and its three tools, nil without an engine.
func newRunsDesk(e *Engine) *runsDesk {
	if e == nil {
		return nil
	}
	d := &runsDesk{engine: e}
	for _, tool := range builtin.NewRunTools(d) {
		d.tools = append(d.tools, tool)
	}
	return d
}

// Tools is the three tools a run is offered while runs are in flight around it.
func (d *runsDesk) Tools() []tools.Callable { return d.tools }

// maxRunTitleRunes bounds the title a run is listed by: the first line of what
// its turn was asked, cut at A HUNDRED AND TWENTY characters, a line of a list
// a launch's brief carries whole, beside up to a seat's cap of others; the
// whole ask is the other run's own.
const maxRunTitleRunes = 120

// runCallOutcome is what became of one call of the run tools, the
// [metrics.SeatRunCalls] counter's `outcome`: listed, sent, waited, resumed,
// failed, or refused for a [builtin.RunRefusal] ("refused_" and its reason).
type runCallOutcome string

const (
	runListed  runCallOutcome = "listed"
	runSent    runCallOutcome = "sent"
	runWaited  runCallOutcome = "waited"
	runResumed runCallOutcome = "resumed"
	runFailed  runCallOutcome = "failed"
)

// refusedOutcome is the outcome of a call refused for reason.
func refusedOutcome(reason builtin.RunRefusal) runCallOutcome {
	return runCallOutcome("refused_" + string(reason))
}

// runCallOutcomes is every outcome but a refusal's.
var runCallOutcomes = []runCallOutcome{runListed, runSent, runWaited, runResumed, runFailed}

// Valid reports whether o is one this build counts.
func (o runCallOutcome) Valid() bool {
	if slices.Contains(runCallOutcomes, o) {
		return true
	}
	reason, ok := strings.CutPrefix(string(o), "refused_")
	return ok && builtin.RunRefusal(reason).Valid()
}

// runCallTool is the counter's `tool`: one of the three, or the engine's answer
// to a wait.
type runCallTool string

const (
	runToolList    runCallTool = runCallTool(builtin.ListRunsTool)
	runToolMessage runCallTool = runCallTool(builtin.MessageRunTool)
	runToolWait    runCallTool = runCallTool(builtin.WaitForRunTool)
	runToolAnswer  runCallTool = "wait_answered"
)

// runCallTools is every `tool` the counter counts.
var runCallTools = []runCallTool{runToolList, runToolMessage, runToolWait, runToolAnswer}

// Valid reports whether t is one this build counts.
func (t runCallTool) Valid() bool { return slices.Contains(runCallTools, t) }

// count counts one call ([metrics.SeatRunCalls]) and says it, refusals at INFO
// with the reason and the runs named.
func (d *runsDesk) count(ctx context.Context, tool runCallTool, outcome runCallOutcome, caller, target string,
	err error) {
	if !tool.Valid() || !outcome.Valid() {
		log.ErrorContext(ctx, "seat_run_outcome_unknown", "tool", string(tool), "outcome", string(outcome))
		return
	}
	d.engine.metrics.Add(metrics.SeatRunCalls, 1, metrics.Attrs{"tool": string(tool), "outcome": string(outcome)})
	attrs := []any{"tool", string(tool), "outcome", string(outcome), "turn_id", caller, "target", target}
	if err != nil {
		attrs = append(attrs, "error", err.Error())
	}
	log.InfoContext(ctx, "seat_run_call", attrs...)
}

// refuse is the refusal for reason, said as said, counted for tool.
func (d *runsDesk) refuse(ctx context.Context, tool runCallTool, caller, target string,
	reason builtin.RunRefusal, said string) error {
	d.count(ctx, tool, refusedOutcome(reason), caller, target, nil)
	return &builtin.RunRefused{Reason: reason, Said: said}
}

// aroundRun is one run in flight around a run, and how it bears on that
// run's work: "" for a run of its own seat, a [prompts.RunRelation] for
// another seat's.
type aroundRun struct {
	run      sandbox.PendingRun
	relation prompts.RunRelation
}

// runsAround is turnID's run and every run in flight around it, by turn id:
// every other run of its seat, in one listing of the seat's runs, and other
// seats' runs on its work item, in its conversation, on an item linked to its
// item, on what it created or wrote or having created or written its item or
// conversation, in one listing of the fleet's
// ([sandbox.PendingStore.ListActiveOn]). A nil caller is a run with no row, or
// a node with no sandbox runtime.
func (d *runsDesk) runsAround(ctx context.Context, turnID string) (*sandbox.PendingRun, map[string]aroundRun,
	error) {
	rt := d.engine.sandbox.Load()
	if rt == nil {
		return nil, nil, nil
	}
	caller, found, err := rt.pending.Get(ctx, turnID)
	if err != nil || !found {
		return nil, nil, err
	}
	runs, err := rt.pending.ListActiveForSeat(ctx, caller.AgentHandle)
	if err != nil {
		return nil, nil, err
	}
	out := make(map[string]aroundRun, len(runs))
	for _, run := range runs {
		if inFlight(run, turnID) {
			out[run.TurnID] = aroundRun{run: run}
		}
	}
	on := d.workOf(ctx, caller)
	others, err := rt.pending.ListActiveOn(ctx, on)
	if err != nil {
		return nil, nil, err
	}
	for _, run := range others {
		if inFlight(run, turnID) {
			out[run.TurnID] = aroundRun{run: run, relation: relationOf(caller, run, on)}
		}
	}
	return &caller, out, nil
}

// inFlight reports whether run is one a run of turn other than it is shown:
// in a stage, its ending undecided.
func inFlight(run sandbox.PendingRun, turn string) bool {
	return run.TurnID != turn && run.Ending == nil && sandbox.StageOf(run) != ""
}

// workOf is the work other seats' runs are found on for caller's run: its
// item and the items the tracker links to it, its conversation where that is
// one two seats can share (never a schedule's, which each seat it fans out to
// works on its own, nor an event's own fallback), and what its calls created
// or wrote, as its row keeps it ([sandbox.PendingRun.WroteKeys]): no read
// beyond the row runsAround holds.
func (d *runsDesk) workOf(ctx context.Context, caller sandbox.PendingRun) sandbox.RunsOn {
	on := sandbox.RunsOn{Except: caller.AgentHandle, Wrote: caller.WroteKeys}
	if kind := conversationKind(caller.ConversationKey); kind != "" && kind != "schedule" {
		on.Conversation = caller.ConversationKey
	}
	if caller.WorkItem != nil {
		on.Item, on.Linked = caller.WorkItem.Ref(), d.linkedTo(ctx, *caller.WorkItem)
	}
	return on
}

// relationOf is how run, another seat's found on the work on names, bears on
// caller's: the same item first, then the same conversation, then work
// caller's calls wrote, then run's calls having written caller's work, else an
// item linked to caller's. A write names the work more nearly than a link.
func relationOf(caller, run sandbox.PendingRun, on sandbox.RunsOn) prompts.RunRelation {
	switch {
	case caller.WorkItem != nil && run.WorkItem != nil && run.WorkItem.Ref() == caller.WorkItem.Ref():
		return prompts.RelationSameItem
	case on.Conversation != "" && run.ConversationKey == on.Conversation:
		return prompts.RelationSameConversation
	case on.OnWritten(run):
		return prompts.RelationOnWhatYouWrote
	case on.WrittenBy(run):
		return prompts.RelationWroteWhatYouAreOn
	}
	return prompts.RelationLinkedItem
}

// maxLinkedItems bounds the items linked to a run's item whose runs it is
// shown: SIXTEEN, the item's own dependencies, links and duplicates. An item
// linked to more is a hub, an epic, whose links name the work around it
// rather than this run's.
const maxLinkedItems = 16

// linkedKinds are the relations an item's runs are related by: a dependency,
// an untyped link and a duplicate; never a page's or an atlas entry's.
var linkedKinds = []worklet.RelationKind{worklet.RelationWaitingOn, worklet.RelationLinked,
	worklet.RelationDuplicates}

// linkedTo is the references of the items the tracker links item to, either
// way, at most [maxLinkedItems]: the engine's own tracker's alone, which keeps
// its links; none where the read fails, which is said, and the run is shown
// the runs on its own item and conversation.
func (d *runsDesk) linkedTo(ctx context.Context, item types.WorkItem) []string {
	if d.links != nil {
		return d.links(ctx, item)
	}
	if item.Backend != types.WorkWorklet || item.ID == "" {
		return nil
	}
	halves, ok := d.engine.trackerHalves()
	if !ok {
		return nil
	}
	detail, err := halves.reader.Task(ctx, item.ID, worklet.DetailWants{Links: true},
		statelog.Freshness{Level: statelog.DefaultReadLevel(statelog.SurfaceSeat)})
	if err != nil {
		log.WarnContext(ctx, "seat_run_links_unread", "item", item.ID, "error", err.Error(),
			"detail", "the items linked to the run's item could not be read, so it is shown no run on them")
		return nil
	}
	var out []string
	for _, link := range detail.Links {
		ref := types.WorkItem{Backend: types.WorkWorklet, ID: link.Other}.Ref()
		if slices.Contains(linkedKinds, link.Kind) && link.Other != "" && !slices.Contains(out, ref) &&
			len(out) < maxLinkedItems {
			out = append(out, ref)
		}
	}
	return out
}

// seatRuns is [runsDesk.runsAround] as runs alone, by turn id.
func (d *runsDesk) seatRuns(ctx context.Context, turnID string) (*sandbox.PendingRun,
	map[string]sandbox.PendingRun, error) {
	caller, around, err := d.runsAround(ctx, turnID)
	if caller == nil || err != nil {
		return caller, nil, err
	}
	out := make(map[string]sandbox.PendingRun, len(around))
	for turn, a := range around {
		out[turn] = a.run
	}
	return caller, out, nil
}

// Around is every other run of turnID's seat in flight, oldest first, each as
// [aroundOf] shows it to turnID's run.
func (d *runsDesk) Around(ctx context.Context, turnID string) ([]prompts.RunAround, error) {
	caller, runs, err := d.runsAround(ctx, turnID)
	if err != nil {
		d.count(ctx, runToolList, runFailed, turnID, "", err)
		return nil, fmt.Errorf("engine: reading the runs around run %s: %w", turnID, err)
	}
	if caller == nil {
		return nil, nil
	}
	ordered := make([]aroundRun, 0, len(runs))
	for _, run := range runs {
		ordered = append(ordered, run)
	}
	// THE SEAT'S OWN FIRST, then other seats', each oldest first.
	slices.SortFunc(ordered, func(a, b aroundRun) int {
		if own := cmp.Compare(otherSeat(a), otherSeat(b)); own != 0 {
			return own
		}
		if c := a.run.CreatedAt.Compare(b.run.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.run.TurnID, b.run.TurnID)
	})
	out := make([]prompts.RunAround, 0, len(ordered))
	for _, run := range ordered {
		line := d.aroundOf(*caller, run.run)
		line.Relation = run.relation
		out = append(out, line)
	}
	if len(out) > 0 {
		d.count(ctx, runToolList, runListed, turnID, "", nil)
	}
	return out, nil
}

// aroundOf is run as caller's run is shown it: its turn, seat, stage and item,
// and only where it works for caller's requester its conversation's kind,
// branch, start and title, the title under what its turn was asked's label
// ([provenance.ExecutorSession]); another requester's task carries their data
// (ADR-0109).
func (d *runsDesk) aroundOf(caller, run sandbox.PendingRun) prompts.RunAround {
	out := prompts.RunAround{
		Turn: run.TurnID, Seat: run.AgentHandle, OwnSeat: run.AgentHandle == caller.AgentHandle,
		Stage: string(sandbox.StageOf(run)), Item: itemOf(run), SameRequester: sandbox.SameRequester(caller, run),
		TakesMessages: d.takesMessages(run),
	}
	if !out.SameRequester {
		return out
	}
	out.Started, out.Branch = run.CreatedAt, run.Branch
	out.Conversation = conversationKind(run.ConversationKey)
	out.SameConversation = run.ConversationKey != "" && run.ConversationKey == caller.ConversationKey
	out.Title, out.Label = titleOf(run.TaskDescription), provenance.ExecutorSession(run.AgentHandle)
	return out
}

// takesMessages reports whether run reads a message between its steps now:
// launching or running with no ending decided, under a harness that reads
// notes here.
func (d *runsDesk) takesMessages(run sandbox.PendingRun) bool {
	rt := d.engine.sandbox.Load()
	return rt != nil && run.Ending == nil && slices.Contains(sandbox.ItemStatuses, run.Status) &&
		rt.coordinator.ReadsNotes(run.Harness)
}

// itemOf is the work item run is charged to as a line names it: its key where
// the tracker gives one, else its reference; "" for none.
func itemOf(run sandbox.PendingRun) string {
	switch item := run.WorkItem; {
	case item == nil:
		return ""
	case item.Key != "":
		return item.Key
	default:
		return item.Ref()
	}
}

// conversationKind is the kind of conversation key is, never the key: its
// source where it names one, `schedule` for a schedule's, "" for none or an
// event's own fallback, which is no conversation.
func conversationKind(key string) string {
	switch {
	case key == "" || strings.HasPrefix(key, notify.EventPrefix):
		return ""
	case notify.Scheduled(key):
		return "schedule"
	}
	source, _, ok := strings.Cut(key, ":")
	if !ok {
		return ""
	}
	return source
}

// titleOf is the first line of task, within [maxRunTitleRunes], its cut said.
func titleOf(task string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(task), "\n")
	line = strings.TrimSpace(line)
	if utf8.RuneCountInString(line) <= maxRunTitleRunes {
		return line
	}
	return string([]rune(line)[:maxRunTitleRunes]) + "…"
}

// runMessageNamespace derives a message's id from its sender's turn and job,
// its target and its words, so a retried call files one message.
var runMessageNamespace = uuid.MustParse("4f7c2a1e-8b3d-5c6e-9a0f-1d2e3f4a5b6c")

// runMessageID is the id of the message text that job of sender sends target.
func runMessageID(sender, job, target, text string) string {
	digest := sha256.Sum256([]byte(text))
	return "msg-" + uuid.NewSHA1(runMessageNamespace,
		[]byte(sender+"/"+job+"/"+target+"/"+fmt.Sprintf("%x", digest))).String()
}

// Message files text from turnID's job on target's run as an item of its own
// (ADR-0176): only between two runs of one requester, to a run working under a
// harness that reads notes, never to the caller, within the sending job's
// [sandbox.MaxJobRunSends] and the receiving job's
// [sandbox.MaxJobRunMessages]. The send is counted before the filing, and
// given back where the filing is refused, so no message is counted twice nor
// sent past the bound.
func (d *runsDesk) Message(ctx context.Context, turnID, job, target, text string) error {
	caller, runs, err := d.seatRuns(ctx, turnID)
	switch {
	case err != nil:
		d.count(ctx, runToolMessage, runFailed, turnID, target, err)
		return fmt.Errorf("engine: reading the runs around run %s: %w", turnID, err)
	case caller == nil:
		return d.refuse(ctx, runToolMessage, turnID, target, builtin.RunRefusedCallerDone,
			"This run's record is gone, so it sends nothing now. End your turn.")
	case target == turnID:
		return d.refuse(ctx, runToolMessage, turnID, target, builtin.RunRefusedSelf,
			"That turn is this run's own: a message goes to another run.")
	}
	run, ok := runs[target]
	if !ok {
		return d.refuse(ctx, runToolMessage, turnID, target, builtin.RunRefusedUnknown,
			"No run in flight around this one has turn "+target+": call `"+builtin.ListRunsTool+
				"` for the runs there are.")
	}
	if refusal := d.messageRefusal(ctx, *caller, run); refusal != nil {
		return refusal
	}
	return d.file(ctx, *caller, run, job, text)
}

// messageRefusal is why run takes no message from caller's run, nil where it
// does: another requester's, not working, or under a harness reading no notes.
func (d *runsDesk) messageRefusal(ctx context.Context, caller, run sandbox.PendingRun) error {
	switch {
	case !sandbox.SameRequester(caller, run):
		return d.refuse(ctx, runToolMessage, caller.TurnID, run.TurnID, builtin.RunRefusedRequester,
			"Run "+run.TurnID+" works for another requester. A run's words carry its requester's data, and no "+
				"label decides whom data is for, so no message passes between runs working for different "+
				"people. If its work bears on yours, tell your requester instead: say it in your final reply.")
	case run.AgentHandle != caller.AgentHandle && !d.reaches(ctx, caller.AgentHandle, run.AgentHandle):
		return d.refuse(ctx, runToolMessage, caller.TurnID, run.TurnID, builtin.RunRefusedUnreachable,
			unreachableSaid(run))
	case run.Ending == nil && run.WaitsOnOwnCalls() && !slices.Contains(sandbox.ItemStatuses, run.Status):
		return d.refuse(ctx, runToolMessage, caller.TurnID, run.TurnID, builtin.RunRefusedNotWorking,
			"Run "+run.TurnID+" is between its steps, carried on at once from a call of its own, and a message "+
				"reaches a run only while it works. Send it again in a moment.")
	case run.Ending != nil || !slices.Contains(sandbox.ItemStatuses, run.Status):
		return d.refuse(ctx, runToolMessage, caller.TurnID, run.TurnID, builtin.RunRefusedNotWorking,
			"Run "+run.TurnID+" is "+strings.ReplaceAll(string(sandbox.StageOf(run)), "_", " ")+", not working "+
				"now, and a message reaches a run only while it works. Leave it to its requester, or wait for it "+
				"with `"+builtin.WaitForRunTool+"` where your work has to come after its.")
	case !d.takesMessages(run):
		return d.refuse(ctx, runToolMessage, caller.TurnID, run.TurnID, builtin.RunRefusedHarness,
			"Run "+run.TurnID+"'s harness reads no message between its steps, so nothing you send reaches it.")
	}
	return nil
}

// file counts the send, seals text as caller's message to run's current job
// and files it, giving the send back where the filing is refused.
func (d *runsDesk) file(ctx context.Context, caller, run sandbox.PendingRun, job, text string) error {
	rt, steers := d.engine.sandbox.Load(), d.engine.runSteers
	if rt == nil || steers == nil {
		err := errors.New("engine: this node holds no run steers to file a message with")
		d.count(ctx, runToolMessage, runFailed, caller.TurnID, run.TurnID, err)
		return err
	}
	id := runMessageID(caller.TurnID, job, run.TurnID, text)
	if heldAlready(run, id) {
		// A RETRY of a message filed: it is one message, counted once.
		d.count(ctx, runToolMessage, runSent, caller.TurnID, run.TurnID, nil)
		return nil
	}
	if err := rt.pending.CountRunSend(ctx, caller.TurnID, job, 1); err != nil {
		switch {
		case errors.Is(err, sandbox.ErrRunSendsSpent):
			return d.refuse(ctx, runToolMessage, caller.TurnID, run.TurnID, builtin.RunRefusedBound,
				fmt.Sprintf("This launch has sent %d messages to other runs, the most one sends. Nothing was "+
					"sent: carry on, and say what is left in your final reply.", sandbox.MaxJobRunSends))
		case errors.Is(err, sandbox.ErrWakeClosed):
			return d.refuse(ctx, runToolMessage, caller.TurnID, run.TurnID, builtin.RunRefusedCallerDone,
				"This run is no longer working in the launch that called, so it sends nothing. End your turn.")
		}
		d.count(ctx, runToolMessage, runFailed, caller.TurnID, run.TurnID, err)
		return fmt.Errorf("engine: counting run %s's message: %w", caller.TurnID, err)
	}
	// THE REQUESTER RIDES THE MESSAGE as an ask's does (ADR-0109): composed
	// from the sending run's row, never from what its model wrote.
	w := steer.Wake{ID: id, By: sandbox.RequesterName(caller), At: d.engine.now().UTC(), Text: text,
		WorkingFor: notify.WorkingForKey(sandbox.CarrierOf(caller)),
		Label:      provenance.Executor(caller.AgentHandle), FromRun: caller.TurnID, FromSeat: caller.AgentHandle}
	err := steers.fileRunMessage(ctx, run.TurnID, run.LaunchID, w)
	if err == nil {
		d.count(ctx, runToolMessage, runSent, caller.TurnID, run.TurnID, nil)
		return nil
	}
	if back := rt.pending.CountRunSend(context.WithoutCancel(ctx), caller.TurnID, job, -1); back != nil {
		log.WarnContext(ctx, "seat_run_send_kept", "turn_id", caller.TurnID, "target", run.TurnID,
			"error", back.Error(), "detail", "a message no run took is still counted against the launch's bound")
	}
	switch {
	case errors.Is(err, sandbox.ErrRunMessagesSpent), errors.Is(err, sandbox.ErrWakesSpent),
		errors.Is(err, sandbox.ErrWakeTooLarge):
		return d.refuse(ctx, runToolMessage, caller.TurnID, run.TurnID, builtin.RunRefusedBound,
			fmt.Sprintf("Run %s takes no more messages in this launch: it holds %d from other runs, or every "+
				"message its record carries. Nothing was sent.", run.TurnID, sandbox.MaxJobRunMessages))
	case errors.Is(err, sandbox.ErrWakeClosed):
		return d.refuse(ctx, runToolMessage, caller.TurnID, run.TurnID, builtin.RunRefusedNotWorking,
			"Run "+run.TurnID+" stopped working before your message reached it. Nothing was sent.")
	}
	d.count(ctx, runToolMessage, runFailed, caller.TurnID, run.TurnID, err)
	return fmt.Errorf("engine: filing run %s's message for run %s: %w", caller.TurnID, run.TurnID, err)
}

// heldAlready reports whether run's record holds the message named id.
func heldAlready(run sandbox.PendingRun, id string) bool {
	return slices.ContainsFunc(run.Steer.Notes, func(n sandbox.RunNote) bool {
		return n.IsWake() && slices.ContainsFunc(n.Wake.Wakes, func(w sandbox.SealedWake) bool { return w.ID == id })
	})
}

// fileRunMessage seals w, a message from another run, for job of turnID's run
// and files it ([sandbox.PendingStore.FileWake]): never cut, a seal past an
// item's bytes refused.
func (s *runSteers) fileRunMessage(ctx context.Context, turnID, job string, w steer.Wake) error {
	sealed, err := s.sealWake(turnID, job, w)
	if err != nil {
		return err
	}
	if len(sealed.Sealed) > sandbox.MaxWakeItemBytes {
		return fmt.Errorf("engine: message %s for run %s: %w", w.ID, turnID, sandbox.ErrWakeTooLarge)
	}
	_, err = s.store.FileWake(ctx, turnID, sandbox.WakeFiling{Launch: job, Wake: sealed})
	return err
}

// Wait records turnID's job parking until target leaves its seat's slots
// (ADR-0176): on a run of its seat holding a slot, never the caller, never one
// that waits on the caller however many runs between; the run's requester is
// told why its work waits ([Engine.tellRunWaits]).
func (d *runsDesk) Wait(ctx context.Context, turnID, job, target, reason, branch string) error {
	caller, runs, err := d.seatRuns(ctx, turnID)
	switch {
	case err != nil:
		d.count(ctx, runToolWait, runFailed, turnID, target, err)
		return fmt.Errorf("engine: reading the runs around run %s: %w", turnID, err)
	case caller == nil:
		return d.refuse(ctx, runToolWait, turnID, target, builtin.RunRefusedCallerDone,
			"This run's record is gone, so it waits on nothing. End your turn.")
	case target == turnID:
		return d.refuse(ctx, runToolWait, turnID, target, builtin.RunRefusedSelf,
			"That turn is this run's own: a run waits for another.")
	}
	run, ok := runs[target]
	switch {
	case !ok:
		return d.refuse(ctx, runToolWait, turnID, target, builtin.RunRefusedUnknown,
			"No run in flight around this one has turn "+target+": call `"+builtin.ListRunsTool+
				"` for the runs there are.")
	case run.AgentHandle != caller.AgentHandle && !sandbox.SameRequester(*caller, run):
		return d.refuse(ctx, runToolWait, turnID, target, builtin.RunRefusedRequester,
			"Run "+target+" is another seat's, working for another requester: a run waits on another seat's "+
				"run only where both work for one requester. Tell your requester instead.")
	case run.AgentHandle != caller.AgentHandle && !d.reaches(ctx, caller.AgentHandle, run.AgentHandle):
		return d.refuse(ctx, runToolWait, turnID, target, builtin.RunRefusedUnreachable, unreachableSaid(run))
	case !slices.Contains(sandbox.Holding, run.Status) && !run.WaitsOnOwnCalls():
		// A RUN PARKED ON ITS OWN CALLS is carried straight on, so it is
		// waited for as a working one (internal/sandbox's callwait.go).
		return d.refuse(ctx, runToolWait, turnID, target, builtin.RunRefusedNotWorking,
			"Run "+target+" is "+strings.ReplaceAll(string(sandbox.StageOf(run)), "_", " ")+", so it holds no "+
				"slot of its seat, and nothing says when it would carry on. Carry on with your own work, or ask "+
				"your requester.")
	case d.waitCycle(ctx, turnID, target, withRun(runs, *caller)):
		return d.refuse(ctx, runToolWait, turnID, target, builtin.RunRefusedCycle,
			"Run "+target+" waits, directly or through other runs, on this one: waiting for it would leave "+
				"each run waiting on the other. Carry on with your own work, or tell it what you need with `"+
				builtin.MessageRunTool+"`.")
	}
	ask := sandbox.Ask{Question: sandbox.RunWaitQuestion(reason), Branch: branch,
		Context: "This run waits for run " + target + " of " + run.AgentHandle + " to leave its seat's slots; " +
			"the engine answers it with how that run went.",
		Wait: &sandbox.RunWait{Turn: target, Seat: run.AgentHandle, Reason: reason, Item: itemOf(run),
			SameRequester: sandbox.SameRequester(*caller, run)}}
	if invalid := ask.Validate(); invalid != nil {
		// THE RUN'S BRANCH, the one thing it wrote that the ask checks beside
		// the reason the tool held already.
		return d.refuse(ctx, runToolWait, turnID, target, builtin.RunRefusedInvalid,
			"The wait was refused: "+invalid.Error()+". Nothing was recorded.")
	}
	rt := d.engine.sandbox.Load()
	outcome, err := rt.pending.RecordAsk(ctx, turnID, job, ask)
	switch {
	case err != nil:
		d.count(ctx, runToolWait, runFailed, turnID, target, err)
		return fmt.Errorf("engine: recording run %s's wait: %w", turnID, err)
	case outcome == sandbox.AskAlreadyAsked:
		return d.refuse(ctx, runToolWait, turnID, target, builtin.RunRefusedCallerDone,
			"This run already asked a question, or waits on a run or on a call of its own, in this launch, and "+
				"that stands. End your turn.")
	case outcome != sandbox.AskRecorded:
		return d.refuse(ctx, runToolWait, turnID, target, builtin.RunRefusedCallerDone,
			"This run is no longer working in the launch that called, so it waits on nothing. End your turn.")
	}
	d.count(ctx, runToolWait, runWaited, turnID, target, nil)
	d.engine.tellRunWaits(ctx, *caller, job, ask.Wait)
	return nil
}

// withRun is runs with run in it, the caller's own, for the cycle walk.
func withRun(runs map[string]sandbox.PendingRun, run sandbox.PendingRun) map[string]sandbox.PendingRun {
	out := make(map[string]sandbox.PendingRun, len(runs)+1)
	for turn, r := range runs {
		out[turn] = r
	}
	out[run.TurnID] = run
	return out
}

// otherSeat orders a's run after the caller's own seat's: 1 for another
// seat's, 0 for its own.
func otherSeat(a aroundRun) int {
	if a.relation == "" {
		return 0
	}
	return 1
}

// unreachableSaid is why run, another seat's, cannot be reached from the
// caller's.
func unreachableSaid(run sandbox.PendingRun) string {
	return "Run " + run.TurnID + " is " + run.AgentHandle + "'s, which your seat may not reach as a colleague's " +
		"ask does: tell your requester what bears on its work instead."
}

// reaches reports whether from's seat may reach to's as a colleague's ask does
// (ADR-0176's amendment), in the chart this node runs now ([reachesIn]).
func (d *runsDesk) reaches(ctx context.Context, from, to string) bool {
	c := d.engine.Company()
	if c == nil || c.Org == nil {
		return false
	}
	ok, err := reachesIn(ctx, c.Org, from, to)
	if err != nil {
		log.WarnContext(ctx, "seat_run_reach_undecided", "seat", from, "target", to, "error", err.Error(),
			"detail", "whether the seat may reach the other could not be decided, so it is refused")
	}
	return ok
}

// reachesIn is the A2A reachability `a2a_ask` is held to, asked of o for one
// seat reaching another: the seat to is an agent's the A2A directory addresses
// ([agentSeats]), never from's own, and the chart's authority offers from's
// seat the ask's verb, as the tool's gate decides it ([builtin.Admits]).
func reachesIn(ctx context.Context, o *org.Organization, from, to string) (bool, error) {
	seat := o.SeatByHandle(from)
	if seat == nil || from == to {
		return false, nil
	}
	if _, agent := (agentSeats{org: o}).SeatInbox(to); !agent {
		return false, nil
	}
	ctx = iam.WithPrincipal(ctx, turnctx.SeatPrincipal(o, seat))
	switch err := builtin.Admits(ctx, builtin.Decide(orgchart.Of(o)), builtin.A2AAskTool); {
	case err == nil:
		return true, nil
	case errors.Is(err, builtin.ErrRefused):
		return false, nil
	default:
		return false, err
	}
}

// waitCycle is [sandbox.WaitCycle] over runs, the caller's and the runs
// around it, reading a run of another seat a chain reaches that runs does not
// hold: at most [sandbox.MaxRunWaitHops] reads, never a walk of every seat's
// rows. A run that cannot be read ends the chain as one waiting on nobody: the
// wait it would refuse cannot hang (ADR-0176).
func (d *runsDesk) waitCycle(ctx context.Context, caller, target string, runs map[string]sandbox.PendingRun) bool {
	rt := d.engine.sandbox.Load()
	return sandbox.WaitCycle(caller, target, func(turn string) (sandbox.PendingRun, bool) {
		if run, ok := runs[turn]; ok || rt == nil {
			return run, ok
		}
		run, found, err := rt.pending.Get(ctx, turn)
		return run, found && err == nil
	})
}
