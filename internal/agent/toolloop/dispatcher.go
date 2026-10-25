package toolloop

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/toolsession"
)

var log = logging.Get("agent.toolloop")

// The dispatcher is a phase's tools as a CLI calls them, and every gate those
// calls pass — the half of the loop that used to be `runCalls`, moved to where
// the calls now arrive: a CLI's own loop, over MCP, at a time of its choosing,
// sometimes two at once.
//
// What did not move is any rule. The fence before every call, the budget
// before every model request, a person's note entering as a user message and
// never inside a tool's result, a phase that finishes by a call, the round
// budget, arguments that did not parse answered rather than run, a detached
// run that parks the phase with its call unanswered: each is the same rule it
// was, enforced at the one place every call now passes. What is new is only
// what a CLI's loop forces:
//
//   - THE RUN IS ENDED, NOT ASKED TO END. A state the phase reaches mid-run —
//     finished, parked, out of rounds, a note waiting, a stop — closes
//     [dispatcher.Done], and the backend stops its CLI. Every answer the CLI
//     would act on from then is WITHHELD (held until it is stopped), so it
//     makes no further request it would be billed for; the loop folds the
//     real results into the conversation itself.
//   - CALLS ARRIVE TOGETHER. A CLI may issue a response's calls at once, in
//     any order. Ordinary calls run side by side; a call that ENDS the phase
//     or PARKS it — a terminator, a detached run — runs alone, after every
//     other call its response declared has run, so a delivery the model made
//     beside its submission is recorded before the submission is judged.
//   - A CALL IS THE PHASE'S, NOT THE CONNECTION'S. It runs under the phase's
//     context, so a CLI that gives up on it, or dies, cancels nothing a tool
//     is half-way through writing; its result is recorded either way.

// dstate is where the phase stands, as the dispatcher sees it.
type dstate int

const (
	// stateOpen is a phase still working.
	stateOpen dstate = iota
	// statePausing is a run ended for a person's note, which the next run
	// reads as a user message.
	statePausing
	// stateRelist is a run ended over an activation its CLI would never see.
	stateRelist
	// stateLocalPark is a run ended over a call too long to hold the CLI
	// for: the call goes on, and the next run is handed its result.
	stateLocalPark
	// stateExhausted is a phase whose round budget is spent with the model
	// still asking for tools.
	stateExhausted
	// stateFinished is a phase a terminator ended.
	stateFinished
	// stateParked is a phase a detached tool suspended, its call unanswered.
	stateParked
	// stateCut is a phase stopped by its fence, a person, its budget or a
	// panic — [dispatcher.cutErr] says which.
	stateCut
)

// ended reports whether the phase itself is over: nothing continues it in
// this loop, by any member of the chain.
func (s dstate) ended() bool {
	return s == stateFinished || s == stateParked || s == stateCut
}

// joinWait is how long a call waits for the CLI's own record of the response
// that issued it, and a call that ends or parks the phase for the response's
// other calls to arrive.
//
// The call and the record race: the record is read off the CLI's stream or a
// file it writes, and the call arrives over a socket, and either can be first
// by milliseconds. Two seconds is three orders of magnitude over that race and
// still short against the model's own latency; a call whose response the
// record never shows within it is placed by the rule for one it did not show
// (see [dispatcher.placeLocked]) rather than held for nothing.
// Calls a CLI issues at once arrive within the same milliseconds, so the same
// bound serves the barrier: a sibling that has not come by then is one the CLI
// will make only once this call is answered.
const joinWait = 2 * time.Second

// parkMargin is how far ahead of a CLI's own tool timeout a running call is
// answered "still running" — the time the answer takes to reach the CLI and the
// CLI to act on it before its own clock would fire. A tenth of the timeout,
// at least five seconds: the measured timeouts run from sixty seconds (kimi,
// pi) to hours, and a fixed margin would be most of a short one.
func parkMargin(timeout time.Duration) time.Duration {
	return max(5*time.Second, timeout/10)
}

// stillRunning is what a CLI is told when a call it made will end the run
// before it returns: the run is about to be stopped, and the next one is handed
// the call's real result.
const stillRunning = "This call is still running. End your turn now: you will be " +
	"resumed with its result."

// pausingNote is what a CLI is handed for an answer withheld from a run that
// has been stopped — which it never reads. It names no note: a note is read as
// a USER message, never inside a tool's result, where any tool's output could
// imitate one.
const pausingNote = "Stop here and end your turn: you will be resumed."

// notRun is the record of a call the model asked for that never ran because
// its run was ending: what the next run, or a resumed one, reads as its answer.
func notRun(name string) string {
	return name + " was NOT run: the run ended before it could run. Call it again if it is still needed."
}

// callRec is one call that reached the dispatcher.
type callRec struct {
	seq   int
	id    string
	name  string
	args  map[string]any
	round int
	run   *run

	// exclusive marks a call that ends or parks the phase: it runs alone.
	exclusive bool

	// argsErr is why the arguments could not be read; the call is answered
	// with it and never run.
	argsErr string

	started time.Time
	took    time.Duration
	result  ToolResult
	settled bool
	// refused marks a call answered without running.
	refused bool
	// admitted marks a call counted in flight, which its settling uncounts.
	admitted bool
	// answered marks a call the CLI was answered "still running" while it
	// goes on: done, as far as its response is concerned.
	answered bool

	// turn and slot are the observed response this call is, and which of
	// its calls — nil where the CLI's record never showed it.
	turn *observedTurn
	slot int
}

// observedTurn is one model request as the CLI's record showed it.
type observedTurn struct {
	id    string
	round int
	text  string
	calls []llm.TurnCall
	// joined marks a call that reached the host as this slot.
	joined []bool
	model  string
	stop   llm.StopReason
	usage  *llm.Usage
	at     time.Time
	start  time.Time
}

// awaiting reports a call of the response the CLI has yet to make.
func (t *observedTurn) awaiting(except int, skip func(name string) bool) bool {
	for i, tc := range t.calls {
		if i == except || !tc.Host || t.joined[i] {
			continue
		}
		if skip != nil && skip(tc.Name) {
			continue
		}
		return true
	}
	return false
}

// run is one CLI run on the dispatcher: one [llm.ToolHost.Serve].
type run struct {
	opts     llm.ServeOptions
	session  *toolsession.Session
	start    time.Time
	released chan struct{}
	// synthetic is the round an unobserved run's calls are placed on.
	synthetic int
}

// dispatcher is the loop's [llm.ToolHost] and [toolsession.Dispatcher].
type dispatcher struct {
	cfg         Config
	ctx         context.Context
	terminators map[string]struct{}
	parkLocal   map[string]struct{}

	// gate is held SHARED by every ordinary call while it runs and
	// EXCLUSIVELY by a call that ends or parks the phase, which therefore
	// runs alone — see the file's doc.
	gate sync.RWMutex

	mu   sync.Mutex
	cond *sync.Cond

	// --- the phase, across every run of this loop invocation ---
	state  dstate
	cutErr error
	done   chan struct{}

	turns     []*observedTurn
	calls     []*callRec
	runs      []*run
	rounds    int // model requests so far, on this invocation's scale
	inflight  int
	spent     llm.Usage
	suspended *callRec
	payload   map[string]any
	lastBusy  bool

	// active is a run in progress — between [dispatcher.reopen] and
	// [dispatcher.finish] — and began when it was started.
	active bool
	began  time.Time
	// invCalls, invRuns and invTurns are where the current run's calls, CLI
	// runs and responses start in calls, runs and turns.
	invCalls, invRuns, invTurns int

	// folded is how much of turns and calls the loop has already turned
	// into conversation, so [dispatcher.Pending] and the next fold start
	// after it.
	foldedTurns, foldedCalls int

	// onFrame publishes the live view, under no lock of the dispatcher's.
	onFrame func()
}

func newDispatcher(ctx context.Context, cfg Config, firstRound int) *dispatcher {
	d := &dispatcher{
		cfg:         cfg,
		ctx:         ctx,
		terminators: make(map[string]struct{}, len(cfg.TerminateAfter)),
		parkLocal:   make(map[string]struct{}, len(cfg.ParkLocally)),
		done:        make(chan struct{}),
		rounds:      firstRound,
	}
	d.cond = sync.NewCond(&d.mu)
	for _, name := range cfg.TerminateAfter {
		d.terminators[name] = struct{}{}
	}
	for _, name := range cfg.ParkLocally {
		d.parkLocal[name] = struct{}{}
	}
	return d
}

var (
	_ llm.ToolHost           = (*dispatcher)(nil)
	_ toolsession.Dispatcher = (*dispatcher)(nil)
)

// --- llm.ToolHost ----------------------------------------------------------

// Serve opens a session for one CLI run.
func (d *dispatcher) Serve(ctx context.Context, opts llm.ServeOptions) (llm.Served, error) {
	if d.cfg.Host == nil {
		return llm.Served{}, fmt.Errorf("toolloop: %s offers tools but has no session host",
			d.cfg.Surface.Phase())
	}
	title := d.cfg.ServerTitle
	if title == "" {
		title = d.cfg.Surface.Phase()
	}
	session, err := d.cfg.Host.Open(d.ctx, ServerName, title, d)
	if err != nil {
		return llm.Served{}, err
	}
	r := &run{opts: opts, session: session, start: time.Now(), released: make(chan struct{})}
	d.mu.Lock()
	d.runs = append(d.runs, r)
	d.mu.Unlock()
	command, args := d.cfg.Host.Command(session)
	var once sync.Once
	return llm.Served{
		Command: command,
		Args:    args,
		Release: func() bool {
			once.Do(func() {
				// The withheld answers go first — a handler waiting on
				// them is what Retire would otherwise wait for — and the
				// session after, joining whatever is still running.
				close(r.released)
				session.Retire()
			})
			return session.Connected()
		},
	}, nil
}

// ServerName is the name a CLI lists the engine's tools under, and so the
// prefix its model sees them with (`mcp__crewlet__…`). The same name a coding
// run's box lists them under, so a seat's tools are spelled one way.
const ServerName = "crewlet"

// Observe records one model request.
func (d *dispatcher) Observe(t llm.Turn) {
	d.mu.Lock()
	now := time.Now()
	turn := d.turnByID(t.ID)
	if turn != nil {
		// The same request read again, further along: take what is new.
		if len(t.Calls) > len(turn.calls) {
			grown := &observedTurn{calls: t.Calls}
			d.expectLocked(grown)
			copy(grown.joined, turn.joined)
			turn.calls, turn.joined = t.Calls, grown.joined
		}
		if t.Text != "" {
			turn.text = t.Text
		}
		if t.Usage != nil {
			turn.usage = t.Usage
		}
		if t.Model != "" {
			turn.model = t.Model
		}
		if t.Stop != "" {
			turn.stop = t.Stop
		}
	} else {
		d.rounds++
		turn = &observedTurn{
			id: t.ID, round: d.rounds, text: t.Text, calls: t.Calls, model: t.Model,
			stop: t.Stop, usage: t.Usage, at: now, start: d.modelStartLocked(now),
		}
		d.expectLocked(turn)
		d.turns = append(d.turns, turn)
	}
	d.cond.Broadcast()
	d.mu.Unlock()
	d.frame()
}

// expectLocked readies a response's record of which of its calls have come.
func (d *dispatcher) expectLocked(t *observedTurn) {
	t.joined = make([]bool, len(t.calls))
}

// modelStartLocked is when the model began the response observed at now: the
// latest of the current run's start, the previous response and the last call
// that returned — the moment the CLI had everything it needed to ask.
func (d *dispatcher) modelStartLocked(now time.Time) time.Time {
	start := d.began
	if r := d.currentRunLocked(); r != nil && r.start.After(start) {
		start = r.start
	}
	if n := len(d.turns); n > 0 && d.turns[n-1].at.After(start) {
		start = d.turns[n-1].at
	}
	for _, c := range d.calls[d.invCalls:] {
		if c.settled && !c.refused && c.started.Add(c.took).After(start) {
			start = c.started.Add(c.took)
		}
	}
	if start.IsZero() || start.After(now) {
		return now
	}
	return start
}

func (d *dispatcher) turnByID(id string) *observedTurn {
	if id == "" {
		return nil
	}
	for _, t := range d.turns {
		if t.id == id {
			return t
		}
	}
	return nil
}

// Spent charges what the run has spent.
func (d *dispatcher) Spent(u llm.Usage) {
	d.mu.Lock()
	d.spent.Input += u.Input
	d.spent.Output += u.Output
	d.spent.CacheRead += u.CacheRead
	d.spent.CacheWrite += u.CacheWrite
	d.mu.Unlock()
	if err := charge(d.ctx, d.cfg.Budget, u.Total()); err != nil {
		// The tokens are spent and recorded whichever way the meter
		// answered; what a refusal decides is what FOLLOWS — no further
		// call runs and no further request is made.
		d.cut(err)
	}
}

// Done is closed when the run must end.
func (d *dispatcher) Done() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.done
}

// Halted reports a run no other member of a chain may continue: any end the
// run has reached is one the loop answers itself — the phase ended, or the run
// was ended for a reason the next run of the loop answers.
func (d *dispatcher) Halted() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state != stateOpen
}

// Pending is what the phase did since the loop last folded it.
func (d *dispatcher) Pending() []llm.Message {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.foldLockedPending()
}

// --- toolsession.Dispatcher -----------------------------------------------

// Offered is what the CLI is offered now.
func (d *dispatcher) Offered() []toolsession.Tool {
	defs := d.cfg.Surface.ToolDefs()
	out := make([]toolsession.Tool, 0, len(defs))
	for _, def := range defs {
		out = append(out, toolsession.Tool{Def: def, Annotations: d.cfg.Surface.AnnotationsOf(def.Name)})
	}
	return out
}

// Revision is the surface's.
func (d *dispatcher) Revision() uint64 { return d.cfg.Surface.Revision() }

// Call decides and runs one call the CLI made.
//
// The MCP request's context is not this call's: a CLI that gives up on the
// call, or dies, abandons only the delivery of its answer. The call runs under
// the phase's context to its end and is recorded, and the session's release
// waits for it.
func (d *dispatcher) Call(_ context.Context, in toolsession.Call) toolsession.Result {
	c, refusal := d.admit(in)
	if refusal != "" {
		return d.withheld(c.run, toolsession.Result{Output: refusal, Failed: true})
	}
	if c.exclusive {
		d.awaitSiblings(c)
		d.gate.Lock()
	} else {
		d.gate.RLock()
	}
	// ADMITTED AGAIN UNDER THE GATE: the phase may have ended while this
	// call waited for an exclusive one to run.
	if why := d.recheck(c); why != "" {
		d.unlockGate(c.exclusive)
		return d.withheld(c.run, toolsession.Result{Output: why, Failed: true})
	}

	// The revision BEFORE the call, so an activation it made is told from a
	// surface that did not move.
	before := d.cfg.Surface.Revision()
	result := make(chan ToolResult, 1)
	go func() {
		defer d.unlockGate(c.exclusive)
		res := d.honour(d.execute(c))
		// CONCLUDED HERE, on the call's own goroutine and UNDER THE GATE:
		// a call answered "still running" is recorded when it returns,
		// whoever is still waiting for it, and what a call decides — a
		// submission that finished the phase, a park — is decided before
		// the next call can take the gate and run as if it had not.
		d.conclude(c, res, before)
		result <- res
	}()

	if _, longOne := d.parkLocal[c.name]; longOne {
		// A CALL THAT WAITS ON OTHER MODEL RUNS — delegated workers — ends
		// the run at once: the CLI would hold its slot, and its memory,
		// idle for as long as they take.
		d.parkLocally(c)
		return d.withheld(c.run, toolsession.Result{Output: stillRunning})
	}
	var timeout <-chan time.Time
	if r := c.run; r != nil && r.opts.ToolTimeout > 0 {
		timer := time.NewTimer(r.opts.ToolTimeout - parkMargin(r.opts.ToolTimeout))
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case res := <-result:
		d.frame()
		return d.withheld(c.run, toolsession.Result{Output: res.Output, Failed: res.Failed})
	case <-timeout:
		d.parkLocally(c)
		return d.withheld(c.run, toolsession.Result{Output: stillRunning})
	}
}

func (d *dispatcher) unlockGate(exclusive bool) {
	if exclusive {
		d.gate.Unlock()
	} else {
		d.gate.RUnlock()
	}
}

// admit records a call and decides whether it may run at all.
func (d *dispatcher) admit(in toolsession.Call) (*callRec, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := &callRec{
		seq: len(d.calls) + 1, name: in.Name, args: in.Arguments,
		argsErr: in.ArgumentsError, run: d.currentRunLocked(), exclusive: d.exclusive(in.Name),
	}
	if c.args == nil {
		c.args = map[string]any{}
	}
	c.id = callIDFromMeta(in.Meta)
	d.placeLocked(c)
	if c.id == "" {
		c.id = fmt.Sprintf("call_%d", c.seq)
	}
	d.calls = append(d.calls, c)

	refuse := func(why string) (*callRec, string) {
		d.settleLocked(c, ToolResult{Output: why, Failed: true}, true)
		return c, why
	}
	if d.state != stateOpen {
		return refuse(notRun(c.name))
	}
	// THE FENCE, BEFORE ANY CALL, and the budget's standing refusal:
	// a node whose lease moved stops at the next call, not the next
	// model request, because the calls are what reach outside the engine.
	if d.cfg.Fence != nil {
		if err := d.cfg.Fence(); err != nil {
			d.cutLocked(err)
			return refuse(notRun(c.name))
		}
	}
	if err := Refusal(d.ctx, d.cfg.Budget); err != nil {
		d.cutLocked(err)
		return refuse(notRun(c.name))
	}
	// PAST THE ROUND BUDGET: the CLI made a request the budget did not
	// allow — its record reached the engine after the request's calls did,
	// or it records none. Nothing of it runs.
	if c.round > d.cfg.MaxRounds {
		d.exhaustLocked()
		return refuse(notRun(c.name))
	}
	if c.argsErr != "" {
		// ARGUMENTS THAT DID NOT PARSE ARE ANSWERED, NOT RUN: running the
		// call on an empty object is a write the model never asked for.
		return refuse(unparsedArguments(c.name, c.argsErr))
	}
	c.admitted = true
	d.inflight++
	if d.inflight == 1 {
		d.busyLocked(true)
	}
	return c, ""
}

// recheck is admit's verdict again, for a call that waited for the gate: the
// phase may have ended in the meantime.
func (d *dispatcher) recheck(c *callRec) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state == stateOpen {
		return ""
	}
	why := notRun(c.name)
	d.settleLocked(c, ToolResult{Output: why, Failed: true}, true)
	return why
}

// callIDFromMeta is the CLI's id for a call, where it sent one with it.
//
// The keys are the ones the measured CLIs use: Claude Code names its own
// tool_use id, Codex and Muse Code the model's call id. Every other CLI sends
// none, and its calls are joined to their responses by name and arguments.
func callIDFromMeta(meta map[string]any) string {
	for _, key := range []string{"claudecode/toolUseId", "callId"} {
		if id, ok := meta[key].(string); ok && id != "" {
			return id
		}
	}
	return ""
}

// placeLocked joins a call to the response that asked for it and sets its
// round, waiting a moment for the CLI's record where it has not shown the
// response yet.
func (d *dispatcher) placeLocked(c *callRec) {
	r := c.run
	if r != nil && !r.opts.ObservesRequests {
		// A CLI WHOSE RECORD SHOWS NO REQUESTS: the whole run is one round.
		if r.synthetic == 0 {
			d.rounds++
			r.synthetic = d.rounds
		}
		c.round = r.synthetic
		return
	}
	deadline := time.Now().Add(joinWait)
	for {
		if t, i, ok := d.findLocked(c); ok {
			d.joinLocked(c, t, i)
			c.round = t.round
			return
		}
		if !time.Now().Before(deadline) {
			break
		}
		d.waitLocked(time.Until(deadline))
	}
	// THE RECORD NEVER SHOWED IT: the call belongs to the latest response,
	// or to the next one if that response's calls have all arrived.
	if n := len(d.turns); n > 0 {
		if last := d.turns[n-1]; last.awaiting(-1, nil) {
			c.round = last.round
			return
		}
	}
	d.rounds++
	c.round = d.rounds
}

// findLocked is the response's call this call is, in the CLI's record: by the
// CLI's own id where it sent one, else by name and arguments.
func (d *dispatcher) findLocked(c *callRec) (*observedTurn, int, bool) {
	for _, t := range d.turns {
		if i, ok := d.matchLocked(t, c); ok {
			return t, i, true
		}
	}
	return nil, 0, false
}

func (d *dispatcher) matchLocked(t *observedTurn, c *callRec) (int, bool) {
	for i, tc := range t.calls {
		if t.joined[i] || !tc.Host {
			continue
		}
		if c.id != "" && tc.ID != "" {
			if tc.ID == c.id {
				return i, true
			}
			continue
		}
		if tc.Name == c.name && sameArgs(tc.Arguments, c.args) {
			return i, true
		}
	}
	return 0, false
}

// joinLocked makes c the slot'th call of t.
func (d *dispatcher) joinLocked(c *callRec, t *observedTurn, slot int) {
	t.joined[slot] = true
	c.turn, c.slot = t, slot
	if c.id == "" {
		c.id = t.calls[slot].ID
	}
}

// sameArgs compares two argument objects by their canonical JSON.
func sameArgs(a, b map[string]any) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ja) == string(jb)
}

// waitLocked waits on the dispatcher's condition, at most for. sync.Cond has
// no timeout, so a timer broadcasts.
func (d *dispatcher) waitLocked(within time.Duration) {
	if within <= 0 {
		return
	}
	t := time.AfterFunc(within, func() {
		d.mu.Lock()
		d.cond.Broadcast()
		d.mu.Unlock()
	})
	d.cond.Wait()
	t.Stop()
}

// currentRunLocked is the CLI run of the loop's current run, or nil before
// one was served.
func (d *dispatcher) currentRunLocked() *run {
	if n := len(d.runs); n > d.invRuns {
		return d.runs[n-1]
	}
	return nil
}

// exclusive reports a call that ends or parks the phase: it runs alone.
func (d *dispatcher) exclusive(name string) bool {
	if _, ok := d.terminators[name]; ok {
		return true
	}
	return d.cfg.Surface.Detached(name)
}

// awaitSiblings holds an exclusive call until every other ordinary call of its
// response has RUN — every one that arrived settled, and every one the record
// shows the CLI making arrived — so a delivery is recorded before the
// submission beside it is judged. Settled, not merely arrived: a sibling that
// has arrived and not yet taken its share of the gate would otherwise queue
// behind this call. The wait for arrivals is bounded — a CLI that runs a
// response's calls one after another sends the next only once this one is
// answered — and the wait for running calls is not, since they hold the gate
// this call needs anyway.
func (d *dispatcher) awaitSiblings(c *callRec) {
	d.mu.Lock()
	defer d.mu.Unlock()
	deadline := time.Now().Add(joinWait)
	for d.state == stateOpen {
		running := false
		for _, other := range d.calls[d.invCalls:] {
			if other != c && other.round == c.round && other.admitted && !other.settled &&
				!other.answered && !other.exclusive {
				running = true
				break
			}
		}
		arriving := c.turn != nil && time.Now().Before(deadline) && c.turn.awaiting(c.slot, d.exclusive)
		switch {
		case running:
			d.cond.Wait()
		case arriving:
			d.waitLocked(time.Until(deadline))
		default:
			return
		}
	}
}

func (d *dispatcher) turnOfRoundLocked(round int) *observedTurn {
	for _, t := range d.turns {
		if t.round == round {
			return t
		}
	}
	return nil
}

// execute runs one admitted call under the PHASE's context, never the MCP
// request's, and turns a panic into the phase's end.
func (d *dispatcher) execute(c *callRec) (res ToolResult) {
	d.mu.Lock()
	c.started = time.Now()
	d.mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			perr := turn.Recovered(r)
			log.ErrorContext(d.ctx, "tool_panicked", "tool", c.name, "panic", perr.Value,
				"stack", perr.Stack)
			d.cut(perr)
			res = ToolResult{Output: c.name + " failed: the tool stopped with an internal error, " +
				"and the phase was stopped.", Failed: true}
		}
		d.mu.Lock()
		c.took = time.Since(c.started)
		d.mu.Unlock()
	}()
	d.frame()
	out, err := d.cfg.Surface.Execute(withCallRound(d.ctx, c.round), llm.ToolCall{
		ID: c.id, Name: c.name, Arguments: c.args,
	})
	if err != nil {
		// THE SURFACE ITSELF FAILED — a torn-down turn. The phase cannot
		// go on.
		d.cut(fmt.Errorf("toolloop: surface failed on %q: %w", c.name, err))
		return ToolResult{Output: c.name + " failed: the phase was stopped before it could answer.",
			Failed: true}
	}
	return out
}

// honour is a call's result as this phase can take it: a suspend in a phase
// that never persists a partial conversation cannot be resumed, so it is
// refused — and the model told so — on the record and the answer alike.
func (d *dispatcher) honour(res ToolResult) ToolResult {
	if res.Suspend && !d.cfg.AllowSuspend {
		res.Suspend = false
		res.Failed = true
		res.Output = "This tool cannot suspend in this phase. " + res.Output
	}
	return res
}

// conclude records a call's result and decides what it means for the run.
func (d *dispatcher) conclude(c *callRec, res ToolResult, before uint64) {
	// THE FENCE AND THE BUDGET AGAIN, once the tool has returned: what used
	// to be the next round's check, before the model reads the result.
	if d.cfg.Fence != nil {
		if err := d.cfg.Fence(); err != nil {
			d.cut(err)
		}
	}
	if err := Refusal(d.ctx, d.cfg.Budget); err != nil {
		d.cut(err)
	}

	d.mu.Lock()
	d.settleLocked(c, res, false)
	_, terminator := d.terminators[c.name]
	switch {
	case d.state.ended():
	case res.Suspend:
		// THE PARK IS AT ONCE: its call stays unanswered, so nothing more
		// of its response can come.
		d.state, d.suspended, d.payload = stateParked, c, res.SuspendPayload
		d.closeDoneLocked()
	case terminator && !res.Failed:
		// A SUBMISSION THAT RAN FINISHED THE PHASE, whatever the run was
		// ending for meanwhile — a call answered "still running" returns
		// after its run ended, and a next run would submit again.
		if d.state != stateOpen {
			d.state = stateFinished
		} else {
			d.decideLocked(stateFinished, c.round)
		}
	case d.state == stateOpen:
		d.afterCallLocked(c, before)
	}
	d.mu.Unlock()
}

// withheld is answer, unless the run is ending: then the answer waits until
// the CLI is stopped, so it does not read it and ask its model again — a
// request it would be billed for, made after the run's end was decided. The
// loop folds the real result into the conversation itself.
func (d *dispatcher) withheld(r *run, answer toolsession.Result) toolsession.Result {
	d.mu.Lock()
	ending := d.state != stateOpen
	d.mu.Unlock()
	if !ending || r == nil {
		return answer
	}
	select {
	case <-r.released:
	case <-d.ctx.Done():
	}
	return toolsession.Result{Output: pausingNote}
}

// afterCallLocked decides what an ordinary call's completion means for the
// run: the budget's last round, a note waiting, an activation the CLI will
// not see.
func (d *dispatcher) afterCallLocked(c *callRec, before uint64) {
	if r := c.run; r != nil && r.opts.ObservesRequests {
		d.settleRoundLocked(c.round)
	}
	if d.cfg.Steer != nil && d.cfg.Steer.Waiting() {
		d.decideLocked(statePausing, c.round)
	}
	if r := c.run; r != nil && !r.opts.Relists && d.cfg.Surface.Revision() != before {
		d.decideLocked(stateRelist, c.round)
	}
}

// decideLocked ends the run in st, over the response of round.
func (d *dispatcher) decideLocked(st dstate, _ int) {
	if d.state != stateOpen {
		return
	}
	d.state = st
	d.closeDoneLocked()
}

// settleRoundLocked ends the run once the LAST round the budget allows has
// had every call it asked for: the CLI must not make another request.
func (d *dispatcher) settleRoundLocked(round int) {
	if round != d.cfg.MaxRounds || d.state != stateOpen {
		return
	}
	for _, c := range d.calls {
		if c.round == round && !c.settled {
			return
		}
	}
	if t := d.turnOfRoundLocked(round); t != nil && t.awaiting(-1, nil) {
		return
	}
	d.exhaustLocked()
}

func (d *dispatcher) exhaustLocked() {
	if d.state == stateOpen {
		d.state = stateExhausted
		d.closeDoneLocked()
	}
}

// settleLocked records a call's result, once: a call answered "still running"
// is settled by its own goroutine, and every other path is idempotent beside
// it.
func (d *dispatcher) settleLocked(c *callRec, res ToolResult, refused bool) {
	if c.settled {
		return
	}
	c.result, c.settled, c.refused = res, true, refused
	if c.admitted {
		d.inflight--
		if d.inflight == 0 {
			d.busyLocked(false)
		}
	}
	d.cond.Broadcast()
}

// parkLocally answers a call "still running" and ends the run; the call goes
// on, and the loop folds its result into the next run.
func (d *dispatcher) parkLocally(c *callRec) {
	d.mu.Lock()
	c.answered = true
	d.decideLocked(stateLocalPark, c.round)
	d.cond.Broadcast()
	d.mu.Unlock()
	d.frame()
}

// busyLocked tells the run's backend the engine's tools started or stopped
// running.
func (d *dispatcher) busyLocked(busy bool) {
	if busy == d.lastBusy {
		return
	}
	d.lastBusy = busy
	if r := d.currentRunLocked(); r != nil && r.opts.OnCall != nil {
		r.opts.OnCall(busy)
	}
}

// cut ends the phase over err.
func (d *dispatcher) cut(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cutLocked(err)
}

func (d *dispatcher) cutLocked(err error) {
	if d.state.ended() {
		return
	}
	d.state, d.cutErr = stateCut, err
	d.closeDoneLocked()
}

func (d *dispatcher) closeDoneLocked() {
	select {
	case <-d.done:
	default:
		close(d.done)
	}
	d.cond.Broadcast()
}

// frame publishes the live view.
func (d *dispatcher) frame() {
	if d.onFrame != nil {
		d.onFrame()
	}
}

// unparsedArguments is the result a call whose arguments did not parse is
// answered with: what went wrong, and what to do about it, in words a model
// acts on.
func unparsedArguments(name, why string) string {
	return fmt.Sprintf("%s was NOT run: its arguments could not be read (%s). "+
		"Call it again with its arguments as one complete JSON object.", name, why)
}
