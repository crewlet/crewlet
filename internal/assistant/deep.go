package assistant

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/textcut"
)

// DEEP WORK (ADR-0054): a turn that hands its task to a coding CLI running its
// own harness, whose only way into Crewlet is this turn's own surface.
//
// # One step, and every call it makes is the turn's
//
// A run is ONE STEP of the turn: [catalog.StartDeepWorkTool], whose frame is
// re-sent as the run progresses ([wire.StepEvent.Deep]). Every Crewlet call the
// run makes arrives over the engine's bridge at [deepBridge], which executes
// it through a [surface] of this very turn, so it is the person's request
// under the turn's read grant, drawn as a step nested under the run's
// ([wire.StepEvent.Parent]) and recorded like any other; a write becomes the
// same proposal quick mode makes. The bridge adds nothing to what a call may
// do: it only refuses more of them (past the run's step cap, or once a change
// waits for the person) and keeps the turn's start_deep_work tool from the run.
//
// # Suspending around an approval
//
// A proposal cannot suspend a vendor's loop the way it suspends the engine's,
// so the run is told its change is waiting and to end; its box and its CLI
// session are kept ([DeepWork.Keep]) and the turn parks on the run's step with
// the box named on the parked record. The person's decision resumes the turn,
// which continues the run in the same box and session with the decision as
// its next message ([turn.continueDeep]), and only the run's end answers the
// step. A turn that ends parked any other way discards the box.
//
// # Its limits
//
// A run's wall clock is its config's, never past the turn's read grant (every
// call it makes is sent on that grant); its Crewlet calls and its CLI's rounds
// are capped by its steps, and its tokens by its token cap, all three counted
// across the continuations of one task. Its tokens are charged to the turn's
// meter when it ends, and a Stop of the turn kills it.

// errDeepTimeout ends a run at its wall clock.
var errDeepTimeout = errors.New("assistant: the deep-work run reached its time limit")

// DeepMinRun is the least time a run is started with: one minute, enough for a
// box to start and one round to read and answer. Less is refused, with the
// person told to ask again on a fresh grant.
const DeepMinRun = time.Minute

// DeepWrapUp is how long a turn's clock runs past its run's deadline: the
// collection of the run's files and the model's rounds that answer from its
// report, at the twenty-five seconds a round takes, with room for a retry.
const DeepWrapUp = 3 * time.Minute

// deepShowEvery bounds how often a run's step is re-sent while it works: once
// a second, which a person reads as live, against a CLI that may write many
// lines a second.
const deepShowEvery = time.Second

// deepState is a turn's deep work: what a segment hands a run, and the run in
// flight.
type deepState struct {
	// parent is the index of the deep step whose run is in flight; every
	// step made while it is set nests under it. Under the turn's mu.
	parent int
	// clock is the segment's wall clock, which a run extends.
	clock *segmentClock
	meter toolloop.BudgetMeter
	tools []catalog.Tool
	// attachments are the files the question carried.
	attachments []StoredFile
	// parked is the run a park record carries: set by a run that ended on
	// a proposal, in the segment that parks on it.
	parked *parkedDeep
}

// parkedDeep is what a run parked on a proposal keeps for its continuation.
type parkedDeep struct {
	Box DeepBox `json:"box"`
	// Step, Label and Goal are the run's step as the turn shows it.
	Step  int    `json:"step"`
	Label string `json:"label"`
	Goal  string `json:"goal"`
	// StartedAt is when the task's first run began.
	StartedAt time.Time `json:"started_at"`
	// Calls and Spent are what the task's runs have used of its caps.
	Calls int `json:"calls"`
	Spent int `json:"spent"`
	// Runs is how many runs the task has made: the next run's number.
	Runs int `json:"runs"`
	// Decisions is what became of each change the task proposed, in the
	// order the person decided them, in the engine's own words
	// ([resultOf]): the records the turn's model reads when the task ends,
	// since the run's report is the run's own account of them.
	Decisions []string `json:"decisions,omitempty"`
}

// segmentClock is a segment's wall clock: it ends the segment as timed out at
// its deadline, which a deep-work run may move later and nothing moves back.
type segmentClock struct {
	mu    sync.Mutex
	until time.Time
	timer *time.Timer
}

// startClock ends the segment through expire after d.
func startClock(now time.Time, d time.Duration, expire func()) *segmentClock {
	return &segmentClock{until: now.Add(d), timer: time.AfterFunc(d, expire)}
}

// extend moves the deadline to until, if that is later.
func (c *segmentClock) extend(now, until time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !until.After(c.until) {
		return
	}
	c.until = until
	c.timer.Reset(until.Sub(now))
}

// stop ends the clock with its segment.
func (c *segmentClock) stop() {
	if c != nil {
		c.timer.Stop()
	}
}

// deepCall is one run to make: a new task, or a parked one's continuation.
type deepCall struct {
	step  stepClock
	goal  string
	brief string
	prior parkedDeep
	cont  *DeepBox
}

// deep starts a run for a [catalog.StartDeepWorkTool] call.
func (s *surface) deep(ctx context.Context, tool catalog.Tool, args map[string]any) (toolloop.ToolResult, error) {
	goal, plan, err := catalog.DeepTask(args)
	if err != nil {
		return s.refuseBefore(ctx, tool, args, "Not started", err), nil
	}
	if s.t.deepParent() != 0 {
		return s.refuseBefore(ctx, tool, args, "Not started",
			errors.New("a deep-work run is already in progress in this turn")), nil
	}
	if err := s.t.svc.deepAvailable(ctx); err != nil {
		return s.refuseBefore(ctx, tool, args, "Not available", err), nil
	}
	step := s.t.stepStart(tool, args, s.cardOptions(ctx), nil, "")
	return s.t.runDeep(ctx, deepCall{step: step, goal: goal, brief: deepBrief(goal, plan, s.t.deep.attachments, s.t.page, s.t.boundary),
		prior: parkedDeep{Step: step.Index, Label: step.Label, Goal: goal, StartedAt: step.started.UTC()}})
}

// continueDeep continues the run a parked turn waited on, with decision (the
// engine's own words for what became of its proposal) as its next message.
func (t *turn) continueDeep(ctx context.Context, keep parkedDeep, decision string) (toolloop.ToolResult, error) {
	// ON THE TASK'S CLOCK, NOT THIS RUN'S: the card measures its time from the
	// task's start ([wire.DeepProgress.StartedAt]), so a step whose duration
	// counted from the continuation drew "6s" of a task that had already run
	// for half a minute.
	started := keep.StartedAt
	if started.IsZero() {
		started = t.svc.now()
	}
	step := stepClock{started: started, StepEvent: wire.StepEvent{
		StepID: assistantdomain.StepID(t.id, keep.Step), TurnID: t.id, Index: keep.Step,
		Tool: catalog.StartDeepWorkTool, Label: keep.Label, State: wire.StepRunning}}
	step.StartedAt = keep.StartedAt
	box := keep.Box
	if err := t.svc.deepAvailable(ctx); err != nil {
		if t.svc.deep != nil {
			t.svc.deep.Discard(context.WithoutCancel(ctx), box)
		}
		t.stepEnd(ctx, step, wire.StepFailed, "Not continued", nil, 0, 0)
		return failed(mcp.Classify(mcp.RefusalUnavailable, err), "Deep work could not be "+
			"continued: "+redact.Secrets(err.Error())+". Tell the person what was done so far."), nil
	}
	brief := "The person decided on the change you proposed. " + decision + "\n\n" +
		"Continue the task from where you stopped. If nothing is left to do, end with " +
		"your report."
	if t.page != nil {
		// THE SCREEN AS IT IS NOW, since a change the person applied may
		// have moved the draft the run read when it started.
		brief += "\n\n" + screenNow(t.boundary, t.page)
	}
	var cont *DeepBox
	if box.Sandbox != "" {
		cont = &box
	} else {
		// THE BOX WAS NOT KEPT, so the run starts afresh: it is told its
		// goal again, and that its earlier files are gone.
		brief = "Goal: " + keep.Goal + "\n\nAn earlier run of this task proposed a change " +
			"and its files are no longer available. " + brief
	}
	return t.runDeep(ctx, deepCall{step: step, goal: keep.Goal, prior: keep, cont: cont,
		brief: brief})
}

// runDeep makes one run and answers the step it is.
func (t *turn) runDeep(ctx context.Context, dc deepCall) (toolloop.ToolResult, error) {
	cfg, err := t.svc.deepConfig()
	if err != nil {
		return t.deepRefused(ctx, dc, err), nil
	}
	now := t.svc.now()
	deadline := now.Add(cfg.Timeout())
	if t.read != nil {
		if lim := t.read.Expires().Add(-GrantMargin); lim.Before(deadline) {
			deadline = lim
		}
	}
	if deadline.Sub(now) < DeepMinRun {
		return t.deepRefused(ctx, dc, errors.New("too little of this turn's time is left to "+
			"start deep work; ask again to give it a fresh start")), nil
	}
	t.deep.clock.extend(now, deadline.Add(DeepWrapUp))
	// A DURATION, measured on the service's clock: the deadline is the
	// grant's, and the grant's instant is on that clock.
	runCtx, cancel := context.WithTimeoutCause(ctx, deadline.Sub(now), errDeepTimeout)
	defer cancel()

	run := &deepRun{t: t, step: dc.step, max: cfg.Steps(), budget: cfg.Tokens(),
		spent: dc.prior.Spent, progress: wire.DeepProgress{State: wire.DeepStarting, Goal: dc.goal,
			StartedAt: dc.prior.StartedAt, DeadlineAt: deadline.UTC(), Steps: dc.prior.Calls,
			MaxSteps: cfg.Steps()}}
	run.progress.BudgetPercent = run.percent(0)
	run.show(true)
	bridge := &deepBridge{run: run, ctx: runCtx, cancel: cancel,
		inner: newSurface(t, withoutDeep(t.deep.tools)), calls: dc.prior.Calls}
	t.setDeepParent(dc.step.Index)
	defer t.setDeepParent(0)

	inputs, err := t.deepInputs(ctx, dc.cont == nil)
	if err != nil {
		return t.deepRefused(ctx, dc, err), nil
	}
	c, _ := t.svc.company()
	runID := deepRunID(t.id, dc.prior.Step, dc.prior.Runs)
	outcome, err := t.svc.deep.Run(runCtx, DeepRun{
		ID:    runID,
		Owner: t.person.ID, Seat: t.person.Seat, Turn: t.id,
		System: deepSystem(c, t.person), Brief: dc.brief, Continue: dc.cont,
		Inputs: inputs, Surface: bridge, Timeout: deadline.Sub(now),
		Steps: max(cfg.Steps()-dc.prior.Calls, 1), Tokens: max(cfg.Tokens()-dc.prior.Spent, 1),
		Progress: run.observe,
	})
	run.stop()
	spent := outcome.InputTokens + outcome.OutputTokens
	run.addSpent(spent)
	if chargeErr := toolloop.Charge(context.WithoutCancel(ctx), t.deep.meter, spent); chargeErr != nil {
		t.svc.deep.Discard(context.WithoutCancel(ctx), outcome.Box)
		run.end(wire.DeepFailed, wire.StepFailed, "Budget spent", "The token budget is spent.")
		return toolloop.ToolResult{}, chargeErr
	}
	if fatal := bridge.fatalErr(); fatal != nil {
		t.svc.deep.Discard(context.WithoutCancel(ctx), outcome.Box)
		run.end(wire.DeepFailed, wire.StepFailed, "Ended", "")
		return toolloop.ToolResult{}, fatal
	}
	if ctx.Err() != nil {
		t.svc.deep.Discard(context.WithoutCancel(ctx), outcome.Box)
		run.end(wire.DeepStopped, wire.StepStopped, "Stopped", "")
		return toolloop.ToolResult{}, context.Cause(ctx)
	}
	if err != nil {
		t.svc.log.WarnContext(ctx, "assistant_deep_unstarted", "turn", t.id,
			"error", redact.Secrets(err.Error()))
		run.end(wire.DeepFailed, wire.StepFailed, "Could not start", redact.Secrets(err.Error()))
		return failed(mcp.Classify(mcp.RefusalUnavailable, err), "Deep work could not start: "+
			redact.Secrets(err.Error())+". Tell the person, and answer what you can without it."), nil
	}
	files, notes := t.deliver(ctx, runID, outcome.Files)
	prior := dc.prior
	prior.Box, prior.Calls, prior.Spent, prior.Runs = outcome.Box, bridge.used(), run.spentNow(), prior.Runs+1
	if pending := bridge.waiting(); pending != "" {
		if err := t.svc.deep.Keep(context.WithoutCancel(ctx), outcome.Box); err != nil {
			t.svc.log.WarnContext(ctx, "assistant_deep_box_unkept", "turn", t.id,
				"error", redact.Secrets(err.Error()), "detail", "the approval will continue "+
					"the task in a fresh box with no files of the run's own")
			prior.Box = DeepBox{}
		}
		t.mu.Lock()
		t.deep.parked = &prior
		t.mu.Unlock()
		run.end(wire.DeepWaiting, wire.StepWaiting, "Waiting for approval", "")
		return toolloop.ToolResult{Suspend: true, Output: "Deep work proposed a change and " +
			"stopped for the person's decision. Nothing has changed yet."}, nil
	}
	t.svc.deep.Discard(context.WithoutCancel(ctx), outcome.Box)
	state, stepState, summary, reason := deepEnding(outcome, len(files))
	run.end(state, stepState, summary, reason)
	t.tainted(catalog.Tool{Fence: true, Source: deepReport.Source}, deepReport.Trust)
	return toolloop.ToolResult{Output: deepAnswer(t.boundary, outcome, files, notes),
		Failed: state != wire.DeepFinished, Cause: deepCause(outcome)}, nil
}

// deepCause is the class of a run that did not finish, so a surface that is
// not a model can act on it; nil for a run that finished. A run held to its
// token cap spent what it may, and one past its clock or ended early is a
// condition of the run, which asking again on a fresh grant may clear.
func deepCause(o DeepOutcome) error {
	switch o.End {
	case DeepEndFinished:
		return nil
	case DeepEndTokens:
		return mcp.Classify(mcp.RefusalBudgetExhausted,
			errors.New("assistant: the deep-work run reached its token limit"))
	case DeepEndTimeout:
		return mcp.Classify(mcp.RefusalUnavailable, errDeepTimeout)
	}
	return mcp.Classify(mcp.RefusalUnavailable, errors.New("assistant: the deep-work run "+
		"ended before it finished: "+redact.Secrets(o.Reason)))
}

// deepRefused ends a run's step before any run started, and answers the model
// why.
func (t *turn) deepRefused(ctx context.Context, dc deepCall, err error) toolloop.ToolResult {
	message := redact.Secrets(err.Error())
	t.stepEnd(ctx, dc.step, wire.StepRefused, "Not started", &wire.Refusal{
		Code: cards.CodeDeepUnavailable, Message: message}, 0, 0)
	return failed(mcp.Classify(mcp.RefusalUnavailable, err), "Deep work did not start: "+
		message+". Answer what you can without it, and tell the person why.")
}

// deepEnding is how a run that did not park ended, as its step shows it.
func deepEnding(o DeepOutcome, files int) (wire.DeepState, wire.StepState, string, string) {
	switch o.End {
	case DeepEndFinished:
		switch files {
		case 0:
			return wire.DeepFinished, wire.StepDone, "Done", ""
		case 1:
			return wire.DeepFinished, wire.StepDone, "1 file", ""
		}
		return wire.DeepFinished, wire.StepDone, strconv.Itoa(files) + " files", ""
	case DeepEndTimeout:
		return wire.DeepFailed, wire.StepFailed, "Time limit reached",
			"The run reached its time limit before it finished."
	case DeepEndTokens:
		return wire.DeepFailed, wire.StepFailed, "Budget limit reached",
			"The run reached its token limit before it finished."
	case DeepEndStopped:
		return wire.DeepStopped, wire.StepStopped, "Stopped", ""
	}
	reason := strings.TrimSpace(o.Reason)
	if reason == "" {
		reason = "The run ended without its report."
	}
	return wire.DeepFailed, wire.StepFailed, "Did not finish", redact.Secrets(reason)
}

// deepReport is the label a run's report is read under, decided once for the
// fence the model reads it in and for the taint every proposal after it
// carries: a coding CLI's own words, written after it read pages and files
// nobody in the company wrote (ADR-0054), which nobody in the company vouches
// for.
var deepReport = provenance.Label{Source: provenance.SourceDeepWork, Trust: provenance.TrustExternal}

// deepAnswer is what the model reads of a run that did not park: how it ended,
// its report fenced as the run's own ([deepReport]), and the files it
// delivered by id, which a later proposal names to put one into Crewlet.
func deepAnswer(b provenance.Boundary, o DeepOutcome, files []wire.FileRef, notes []string) string {
	var out strings.Builder
	switch o.End {
	case DeepEndFinished:
		out.WriteString("Deep work finished.")
	case DeepEndTimeout:
		out.WriteString("Deep work reached its time limit before it finished.")
	case DeepEndTokens:
		out.WriteString("Deep work reached its token limit before it finished.")
	default:
		out.WriteString("Deep work ended before it finished: " + withoutPaths(redact.Secrets(o.Reason)))
	}
	// NO HOST PATH REACHES THE MODEL, so none can reach its answer: a run
	// that names where it wrote a file is cut to the file's name, which is
	// the name the delivered files below carry.
	if report := strings.TrimSpace(withoutPaths(redact.Secrets(o.Report))); report != "" {
		if len(report) > ModelResultBytes {
			report = textcut.Within(report, ModelResultBytes)
		}
		out.WriteString(" Its report:\n" + deepReport.Fence(b, report))
	}
	if len(files) > 0 {
		out.WriteString("\nFiles it delivered, shown to the person to download:")
		for _, f := range files {
			fmt.Fprintf(&out, "\n- %s (%s, file id %s)", f.Name, f.MediaType, f.FileID)
		}
	}
	for _, n := range notes {
		out.WriteString("\n" + n)
	}
	return out.String()
}

// deliver keeps the files a run left in its outbox and shows them on one card.
// A file of a type no download may carry, or past the size cap, is not kept,
// and a note says so.
func (t *turn) deliver(ctx context.Context, runID string, delivered []DeepFile) ([]wire.FileRef, []string) {
	var refs []wire.FileRef
	var notes []string
	for i, f := range delivered {
		if i >= wire.DeliveredFilesPerRun {
			notes = append(notes, fmt.Sprintf("%d more files were left out: a run delivers "+
				"at most %d.", len(delivered)-i, wire.DeliveredFilesPerRun))
			break
		}
		mediaType, name, err := SniffFile(f.Name, f.Content, wire.FileMediaTypes)
		if err != nil {
			notes = append(notes, "Not delivered: "+redact.Secrets(err.Error()))
			continue
		}
		ref, err := t.svc.files.Store(ctx, t.write().Step("file", strconv.Itoa(i)), NewFile{
			ID:           DeliveredFileID(t.id, runID, name),
			Conversation: t.conversation, Turn: t.id, Origin: wire.FileDelivered,
			Name: name, MediaType: mediaType, Content: f.Content})
		if err != nil {
			t.svc.log.ErrorContext(ctx, "assistant_deep_file_unkept", "turn", t.id,
				"error", redact.Secrets(err.Error()))
			notes = append(notes, "Not delivered: "+name+" could not be kept.")
			continue
		}
		refs = append(refs, ref)
	}
	if len(refs) > 0 {
		t.emitCard(ctx, wire.Card{Type: wire.CardFile, File: &wire.FileCard{Files: refs}}, nil)
	}
	return refs, notes
}

// deepInputs are the question's attachments as a new run is handed them. A
// continuation's box already holds them.
func (t *turn) deepInputs(ctx context.Context, fresh bool) ([]DeepFile, error) {
	if !fresh {
		return nil, nil
	}
	out := make([]DeepFile, 0, len(t.deep.attachments))
	for _, a := range t.deep.attachments {
		_, content, err := t.svc.files.Open(ctx, t.person.ID, t.conversation, a.FileID)
		if err != nil {
			return nil, fmt.Errorf("the attachment %s could not be read: %w", a.Name, err)
		}
		out = append(out, DeepFile{Name: a.Name, Content: content})
	}
	return out, nil
}

// DeliveredFileID is the id a file a run delivered is kept under: derived from
// the run and the file's name, so a retried record of one delivery is one file,
// and the id a later proposal names is the one the card showed.
func DeliveredFileID(turn uuid.UUID, runID, name string) uuid.UUID {
	return uuid.NewSHA1(turn, []byte("delivered:"+runID+":"+name))
}

// deepRunID names one run: its turn, its step and its number within the task.
func deepRunID(turn uuid.UUID, step, run int) string {
	return "assistant-" + turn.String() + "-" + strconv.Itoa(step) + "-" + strconv.Itoa(run)
}

// withoutDeep is tools without the escalation tool: a run never starts a run.
func withoutDeep(tools []catalog.Tool) []catalog.Tool {
	return slices.DeleteFunc(slices.Clone(tools), func(t catalog.Tool) bool {
		return t.Kind == catalog.KindDeep
	})
}

// setDeepParent marks the deep step every step made now nests under.
func (t *turn) setDeepParent(index int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.deep.parent = index
}

// deepParent is the deep step in flight, 0 for none.
func (t *turn) deepParent() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.deep.parent
}

// deepAvailable is why this node cannot run deep work for the company now.
func (s *Service) deepAvailable(ctx context.Context) error {
	if _, err := s.deepConfig(); err != nil {
		return err
	}
	return s.deep.Ready(ctx)
}

// deepConfig is the company's deep work, or why it has none here.
func (s *Service) deepConfig() (config.AssistantDeepWork, error) {
	if s.deep == nil || s.files == nil {
		return config.AssistantDeepWork{}, errors.New("this node runs no deep work")
	}
	c, _ := s.company()
	if c == nil {
		return config.AssistantDeepWork{}, errors.New("this node runs no company yet")
	}
	if _, _, err := c.AssistantDeepWork(); err != nil {
		return config.AssistantDeepWork{}, err
	}
	return *c.Assistant.DeepWork, nil
}

// withDeep is tools with the escalation tool, where this node can run deep work
// for the company now.
func (s *Service) withDeep(ctx context.Context, tools []catalog.Tool) []catalog.Tool {
	if s.deepAvailable(ctx) != nil {
		return tools
	}
	return append(slices.Clone(tools), catalog.DeepWorkTool())
}

// deepRun is one run's step while it works.
type deepRun struct {
	t      *turn
	step   stepClock
	max    int
	budget int

	mu       sync.Mutex
	progress wire.DeepProgress
	// spent is the task's tokens before this run, plus what this run has
	// reported so far once it reports.
	spent int
	// shown and shownState are when the step was last re-sent and in which
	// state: a change of state is always shown.
	shown      time.Time
	shownState wire.DeepState
	stopped    bool
}

// observe takes one live reading of the run.
func (r *deepRun) observe(p DeepProgress) {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.progress.State = wire.DeepRunning
	for _, line := range p.Lines {
		// IN THE PERSON'S WORDS ([personLine]): no host path, no tool's
		// own name, and no call to Crewlet the nested steps already list.
		if shown, ok := personLine(redact.Secrets(line)); ok {
			r.progress.Activity = append(r.progress.Activity, textcut.Within(shown, lineMaxBytes))
		}
	}
	if n := len(r.progress.Activity); n > wire.DeepActivityLines {
		r.progress.Activity = slices.Clone(r.progress.Activity[n-wire.DeepActivityLines:])
	}
	r.progress.BudgetPercent = r.percentLocked(p.Spent)
	r.mu.Unlock()
	r.show(false)
}

// lineMaxBytes bounds one activity line: a command's first line, not its
// output.
const lineMaxBytes = 240

// percent is the task's share of its token cap with spent more from this run.
func (r *deepRun) percent(spent int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.percentLocked(spent)
}

func (r *deepRun) percentLocked(spent int) int {
	if r.budget <= 0 {
		return 0
	}
	return min(100, (r.spent+spent)*100/r.budget)
}

// addSpent adds a finished run's tokens to the task's.
func (r *deepRun) addSpent(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spent += n
	r.progress.BudgetPercent = r.percentLocked(0)
}

// spentNow is the task's tokens so far.
func (r *deepRun) spentNow() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.spent
}

// counted shows one more Crewlet call.
func (r *deepRun) counted(calls int) {
	r.mu.Lock()
	r.progress.Steps = calls
	r.mu.Unlock()
	r.show(false)
}

// stop ends live readings: nothing a late one says is shown.
func (r *deepRun) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = true
}

// show re-sends the run's step, at most once per [deepShowEvery] unless now.
func (r *deepRun) show(now bool) {
	r.mu.Lock()
	at := r.t.svc.now()
	if !now && r.progress.State == r.shownState && at.Sub(r.shown) < deepShowEvery {
		r.mu.Unlock()
		return
	}
	r.shown, r.shownState = at, r.progress.State
	ev := r.step.StepEvent
	progress := r.progress
	progress.Activity = slices.Clone(progress.Activity)
	r.mu.Unlock()
	ev.State, ev.Deep = wire.StepRunning, &progress
	r.t.firstOutput()
	r.t.emit(wire.Frame{Kind: wire.EventStep, Step: &ev})
}

// end closes the run's step in its final state, recorded like any step.
func (r *deepRun) end(state wire.DeepState, stepState wire.StepState, summary, reason string) {
	r.mu.Lock()
	r.stopped = true
	r.progress.State, r.progress.Reason = state, withoutPaths(reason)
	if state == wire.DeepFinished {
		// A FINISHED RUN'S LAST LINES ARE ITS REPORT, which the person
		// reads once, in the assistant's answer and the files it carded:
		// left on the card as well, it was a second long summary, cut
		// mid-line.
		r.progress.Activity = nil
	}
	progress := r.progress
	progress.Activity = slices.Clone(progress.Activity)
	r.mu.Unlock()
	step := r.step
	step.Deep = &progress
	r.t.stepEnd(r.t.ctx, step, stepState, summary, nil, 0, 0)
}

// deepBridge is what the engine's bridge serves a run: the turn's own surface,
// every call the person's request, refused past the run's step cap and once a
// change waits for the person.
type deepBridge struct {
	run    *deepRun
	ctx    context.Context
	cancel context.CancelFunc
	inner  *surface

	// writes holds a write's call whole, so two changes proposed at once
	// are one proposal and one refusal, never two proposals a single park
	// cannot wait on.
	writes sync.Mutex

	mu      sync.Mutex
	calls   int
	pending string
	fatal   error
}

var _ DeepSurface = (*deepBridge)(nil)

// Revision is constant: a run's tools never change while it works.
func (b *deepBridge) Revision() uint64 { return 1 }

// ToolDefs are the turn's tools but the escalation.
func (b *deepBridge) ToolDefs() []llm.ToolDef { return b.inner.ToolDefs() }

// AnnotationsOf is a tool's hints, the turn's own surface's.
func (b *deepBridge) AnnotationsOf(name string) mcp.Annotations { return b.inner.AnnotationsOf(name) }

// Admit binds one bridged call to run once ([DeepSurface]).
func (b *deepBridge) Admit(string, map[string]any) toolloop.Admitted { return b.Execute }

// deepWaitingNote is what a run is told once its change waits for the person.
const deepWaitingNote = "Proposed: the change is shown to the person for their approval, " +
	"and nothing has changed yet. Make no other call. End your run now with two " +
	"sentences: what you did and what is left. You will be continued in this same " +
	"session with their decision."

// Execute carries out one bridged call through the turn's surface.
func (b *deepBridge) Execute(reqCtx context.Context, call llm.ToolCall) (toolloop.ToolResult, error) {
	tool := b.inner.byName[call.Name]
	write := tool.Kind == catalog.KindWrite || tool.Kind == catalog.KindPageAction
	if write {
		b.writes.Lock()
		defer b.writes.Unlock()
	}
	b.mu.Lock()
	switch {
	case b.fatal != nil:
		b.mu.Unlock()
		return failed(mcp.RefusalUnavailable, "This run has ended. End it now."), nil
	case b.pending != "":
		b.mu.Unlock()
		return failed(mcp.RefusalConflict, "A change you proposed is waiting for the "+
			"person's decision. Make no other call: end your run now with two sentences on "+
			"what you did and what is left."), nil
	case b.calls >= b.run.max:
		b.mu.Unlock()
		return failed(mcp.RefusalUnavailable, "This run has made every Crewlet call it may. "+
			"End it now with your report."), nil
	}
	b.calls++
	calls := b.calls
	b.mu.Unlock()
	b.run.counted(calls)

	// THE RUN'S CONTEXT, NOT THE REQUEST'S: the call is the turn's, carrying
	// its values and ended by its Stop, and also ended when the box's own
	// request is.
	ctx, cancel := context.WithCancel(b.ctx)
	defer cancel()
	stop := context.AfterFunc(reqCtx, cancel)
	defer stop()
	res, err := b.inner.Execute(ctx, call) //nolint:contextcheck // the run's own context, ended by the box's request too; see above
	if err != nil {
		b.mu.Lock()
		b.fatal = err
		b.mu.Unlock()
		b.cancel()
		return toolloop.ToolResult{}, err
	}
	if res.Suspend {
		b.mu.Lock()
		b.pending = call.Name
		b.mu.Unlock()
		return toolloop.ToolResult{Output: deepWaitingNote}, nil
	}
	return res, nil
}

// used is how many Crewlet calls the task has made.
func (b *deepBridge) used() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// waiting is the tool whose proposal the run stopped on, "" for none.
func (b *deepBridge) waiting() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pending
}

// fatalErr is a call's error that ended the run: the turn's grant ending, or
// its own cancellation.
func (b *deepBridge) fatalErr() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fatal
}

// answerParkedDeep continues the run a resumed segment parked on, with the
// decision its loop was about to read: answered (false) puts the run's end in
// place of the decision as the parked call's answer, and the loop carries on;
// a run that parks again, or fails the turn, ends the segment with the result
// conclude records.
func (t *turn) answerParkedDeep(ctx context.Context, r *loopRun) (*toolloop.Result, bool, error) {
	n := len(r.messages)
	if n == 0 || r.messages[n-1].Role != llm.RoleTool {
		return &toolloop.Result{Messages: r.messages}, false, errors.New("assistant: a " +
			"resumed deep-work segment carries no answer to the call it parked on")
	}
	call := r.messages[n-1]
	decision := r.decision
	if decision == "" {
		decision = call.Content
	}
	// THE DECISION JOINS THE TASK'S RECORDS before the run continues, so a
	// run that parks again keeps it, and the task's end carries every one.
	keep := *r.deep
	keep.Decisions = append(slices.Clone(keep.Decisions), decision)
	res, err := t.continueDeep(ctx, keep, decision)
	before := slices.Clone(r.messages[:n-1])
	switch {
	case err != nil:
		return &toolloop.Result{Messages: before}, false, err
	case res.Suspend:
		return &toolloop.Result{Messages: before, Suspended: true,
			PendingToolCallID: call.ToolCallID, PendingToolName: call.Name}, true, nil
	}
	r.messages[n-1].Content = joinBlocks(withDecisions(res.Output, keep.Decisions), r.screen)
	r.messages[n-1].Failed = res.Failed
	return nil, false, nil
}

// withDecisions is a task's end as the turn's model reads it: the run's own
// answer, then what became of each change the task proposed, from the
// engine's records. The run is told the decision and writes a report, but
// that report is the run's account, fenced as untrusted; without the records
// the model could only repeat it, or hedge on an outcome the engine knows.
func withDecisions(output string, decisions []string) string {
	if len(decisions) == 0 {
		return output
	}
	var out strings.Builder
	out.WriteString(output)
	out.WriteString("\n\nWhat became of the changes this task proposed, from the engine's own " +
		"records. These are facts: report them as such, whatever the run's report says.")
	for i, d := range decisions {
		fmt.Fprintf(&out, "\n%d. %s", i+1, d)
	}
	return out.String()
}

// attachmentsOf are the files a question names, each one the person uploaded
// to this conversation as an attachment, or the refusal of the question.
func (s *Service) attachmentsOf(ctx context.Context, ts TurnStart) ([]StoredFile, error) {
	if len(ts.Attachments) == 0 {
		return nil, nil
	}
	if s.files == nil {
		return nil, invalid("attachments are not taken on this node: ask without them")
	}
	if len(ts.Attachments) > wire.AttachmentsPerTurn {
		return nil, invalid("a question carries at most %d attachments, and this one names %d",
			wire.AttachmentsPerTurn, len(ts.Attachments))
	}
	out := make([]StoredFile, 0, len(ts.Attachments))
	for _, id := range ts.Attachments {
		if slices.ContainsFunc(out, func(f StoredFile) bool { return f.FileID == id }) {
			continue
		}
		f, err := s.files.Stat(ctx, ts.Person.ID, ts.Conversation, id)
		switch {
		case errors.Is(err, ErrNotFound):
			return nil, invalid("attachment %s is no file of this conversation: upload it again", id)
		case err != nil:
			return nil, &Unavailable{RetryAfter: 2 * time.Second, Cause: err}
		case f.Origin != wire.FileAttached || !slices.Contains(wire.AttachmentMediaTypes, f.MediaType):
			return nil, invalid("attachment %s is not an image the person attached", id)
		}
		out = append(out, f)
	}
	return out, nil
}

// refsOf are files as a prompt and a message name them.
func refsOf(files []StoredFile) []wire.FileRef {
	out := make([]wire.FileRef, 0, len(files))
	for _, f := range files {
		out = append(out, f.FileRef)
	}
	return out
}

// offersDeep reports whether tools offer the escalation.
func offersDeep(tools []catalog.Tool) bool {
	return slices.ContainsFunc(tools, func(t catalog.Tool) bool { return t.Kind == catalog.KindDeep })
}

// dropParkedDeep discards the box of a parked turn that ends with no approval to
// continue it: asked past, stopped, expired, or withdrawn.
func (s *Service) dropParkedDeep(ctx context.Context, keep parked) {
	if keep.Deep == nil || s.deep == nil || keep.Deep.Box.Sandbox == "" {
		return
	}
	s.deep.Discard(context.WithoutCancel(ctx), keep.Deep.Box)
}
