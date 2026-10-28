package toolloop

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/mcp"
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
// call left unanswered as it parks the phase: each is the same rule it
// was, enforced at the one place every call now passes. What is new is only
// what a CLI's loop forces:
//
//   - THE RUN IS ENDED, NOT ASKED TO END. A state the phase reaches mid-run —
//     finished, parked, out of rounds, a note waiting, a stop — closes
//     [dispatcher.Done], and the backend stops its CLI. Every answer the CLI
//     would act on from then is WITHHELD (held until it is stopped), so it
//     makes no further request it would be billed for; the loop folds the
//     real results into the conversation itself.
//   - A RESPONSE'S CALLS ARE RUN AS ONE, AS A ROUND'S WERE. A run that must
//     end over one of a response's calls ends once the response's other calls
//     have reached the engine, not at the first: a CLI that waits on each
//     answer before its next call would otherwise never make the rest, and
//     the model asked for them in the same breath — a delivery written beside
//     its submission, a call made while a note arrived. Until then the
//     deciding answer goes back, so the next call comes; the last answer is
//     the one withheld, since a CLI asks its model again only once every call
//     of the response is answered.
//   - CALLS ARRIVE TOGETHER. A CLI may issue a response's calls at once, in
//     any order. Ordinary calls run side by side; a call that ENDS the phase
//     or PARKS it — a terminator, a detached call — runs alone, after every
//     other call its response declared has run, so a delivery the model made
//     beside its submission is recorded before the submission is judged.
//   - A CALL IS THE MODEL'S, AS IT ASKED FOR IT. The engine runs a call only
//     under a name it offers now, and only the name the model asked for: a CLI
//     that rewrites a name its list does not hold to one it does would
//     otherwise run a tool nobody chose.
//   - A CALL IS THE PHASE'S, NOT THE CONNECTION'S. It runs under the phase's
//     context, so a CLI that gives up on it, or dies, cancels nothing a tool
//     is half-way through writing; its result is recorded either way.
//   - A CALL IS ITS RUN'S. Each run's session hands its calls to that run
//     alone ([runCalls]), and a run that is over takes none: a call that
//     reaches it after its release is refused, never run.

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
	// stateRefused is a run its token budget refuses to let go on: an end
	// only ever DECIDED ([dispatcher.pend]), and taken as [stateCut] with
	// [dispatcher.pendErr]. Ranked under a finish and a park because what it
	// refuses is the CLI's next request, and a phase a call finished or
	// parked makes none.
	stateRefused
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
// record never shows within it is placed on the request after the last one the
// record did show (see [dispatcher.placeLocked]) rather than held for nothing.
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

// notOffered is the record of a call naming a tool this phase does not offer.
func notOffered(name string) string {
	return name + " was NOT run: it is not one of the tools offered here."
}

// rewritten is the record of a call the CLI made under another name than the
// one the model asked for.
func rewritten(asked, sent string) string {
	return fmt.Sprintf("%s was NOT run: it is not one of the tools offered here, and the call "+
		"that reached the engine named %s instead, which was not asked for.", asked, sent)
}

// callRec is one call that reached the dispatcher.
type callRec struct {
	seq   int
	id    string
	name  string
	args  map[string]any
	round int
	run   *run

	// asked is the name the model asked for, where the record showed the
	// call — what the conversation records it as; different from name only
	// on a call the CLI rewrote.
	asked string

	// provisional marks a call whose response the record had not shown when
	// it was placed: its round is the request after the last one shown,
	// until the record shows it ([dispatcher.Observe] adopts it).
	provisional bool

	// exclusive marks a call that ends or parks the phase: it runs alone.
	exclusive bool

	// argsErr is why the arguments could not be read; the call is answered
	// with it and never run.
	argsErr string

	// exec is the call as the surface admitted it, bound to its place in the
	// run's call log ([Surface.Admit]) — nil for a call whose arguments did
	// not parse, which nothing runs.
	exec Admitted

	started time.Time
	took    time.Duration
	result  ToolResult
	settled bool
	// refused marks a call answered without running.
	refused bool
	// admitted marks a call the dispatcher let run.
	admitted bool
	// waiting marks an admitted call its CLI run is still waiting on — not
	// yet settled, and not answered "still running" ([dispatcher.awaitedLocked]).
	waiting bool
	// answered marks a call the CLI was answered "still running" while it
	// goes on: done, as far as its response is concerned.
	answered bool

	// turn and slot are the observed response this call is, and which of
	// its calls — nil where the CLI's record never showed it.
	turn *observedTurn
	slot int
}

// recorded is the name the record carries the call under: the model's.
func (c *callRec) recorded() string {
	if c.asked != "" {
		return c.asked
	}
	return c.name
}

// observedTurn is one model request as the CLI's record showed it.
type observedTurn struct {
	id    string
	round int
	text  string
	calls []llm.TurnCall
	// joined marks a call that reached the host as this slot.
	joined []bool
	// expect marks a call the CLI will make to the host: one of the host's
	// tools, offered when the response was read. A CLI refuses any other name
	// itself — or rewrites it — so nothing waits for it.
	expect []bool
	model  string
	stop   llm.StopReason
	usage  *llm.Usage
	at     time.Time
	start  time.Time
}

// awaiting reports a call of the response the CLI has yet to make.
func (t *observedTurn) awaiting(except int, skip func(name string) bool) bool {
	for i, tc := range t.calls {
		if i == except || !t.expect[i] || t.joined[i] {
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
	opts  llm.ServeOptions
	start time.Time
	// released is closed when the backend releases the run: its CLI is gone,
	// and the run takes no call ([runCalls]).
	released chan struct{}
	// synthetic is the round an unobserved run's calls are placed on.
	synthetic int
	// waiting is how many of the run's calls its CLI is waiting on.
	waiting int
	// ctx is the context the run was served under, whose VALUES every call
	// of the run carries — see [callContext].
	ctx context.Context
}

// over reports whether the run has been released.
func (r *run) over() bool {
	select {
	case <-r.released:
		return true
	default:
		return false
	}
}

// runCalls is one CLI run's side of the dispatcher: what the run's session
// hands each call to.
//
// ONE PER RUN, so a call is the run's that delivered it. Handed the phase's
// dispatcher instead, a session's call was taken for whichever run was out when
// it arrived — or for the last one, after every run had ended — and run there:
// a call that reached the dispatcher once its own run was over (one already
// past the session as the run was released) ran a tool for a run that no
// longer existed, as the bridge's per-opening dispatcher never does.
type runCalls struct {
	d *dispatcher
	r *run
}

var _ toolsession.Dispatcher = (*runCalls)(nil)

func (v *runCalls) Offered() []toolsession.Tool { return v.d.Offered() }
func (v *runCalls) Revision() uint64            { return v.d.Revision() }

// Call decides and runs one call the run's CLI made — see [dispatcher.call].
func (v *runCalls) Call(in toolsession.Call) toolsession.Result { return v.d.call(v.r, in) }

// callContext is the context one call of a run runs under: the VALUES of the
// context the backend served the run under, and the PHASE's cancellation.
//
// The values, because the backend puts there what a call of ITS run needs and
// only a call of its run may have: the CLI backend lends the run's process slot
// to a model call made inside one of its calls (a condensing pass, a judge, on
// the seat's own entry), which without it waits for a slot only the run can
// free — at max_concurrent 1, a deadlock. The served context descends from the
// phase's, so the phase's own values (the turn, the seat, its span) come with
// it, and are read from the phase where a backend served from elsewhere.
//
// The phase's cancellation and never the run's: a run that ends — its CLI
// stopped, a fallback taking over — ends nothing a tool is half-way through.
type callContext struct {
	served context.Context // values only: context.WithoutCancel
	phase  context.Context
}

func newCallContext(served, phase context.Context) context.Context {
	if served == nil {
		return phase
	}
	return callContext{served: context.WithoutCancel(served), phase: phase}
}

func (c callContext) Deadline() (time.Time, bool) { return c.phase.Deadline() }
func (c callContext) Done() <-chan struct{}       { return c.phase.Done() }
func (c callContext) Err() error                  { return c.phase.Err() }
func (c callContext) Value(key any) any {
	// The served context answers no cancellation key (WithoutCancel), so
	// context.Cause and AfterFunc find the phase's.
	if v := c.served.Value(key); v != nil {
		return v
	}
	return c.phase.Value(key)
}

// dispatcher is the loop's [llm.ToolHost] and [toolsession.Dispatcher].
type dispatcher struct {
	cfg         Config
	ctx         context.Context
	terminators map[string]struct{}

	// gate is held SHARED by every ordinary call while it runs and
	// EXCLUSIVELY by a call that ends or parks the phase, which therefore
	// runs alone — see the file's doc.
	gate sync.RWMutex

	mu   sync.Mutex
	cond *sync.Cond

	// --- the phase, across every run of this loop invocation ---
	state  dstate
	cutErr error
	// pendErr is the refusal a decided [stateRefused] end is taken with.
	pendErr error
	done    chan struct{}

	// pend is an end the run has reached and not yet taken — waiting for the
	// rest of the response that decided it, pendRound — and stateOpen when
	// there is none. See the file's doc.
	pend      dstate
	pendRound int

	turns     []*observedTurn
	calls     []*callRec
	runs      []*run
	rounds    int // model requests so far, on this invocation's scale
	spent     llm.Usage
	suspended *callRec

	// active is a run in progress — between [dispatcher.reopen] and
	// [dispatcher.finish] — and began when it was started.
	active bool
	began  time.Time
	// invCalls, invRuns and invTurns are where the current run's calls, CLI
	// runs and responses start in calls, runs and turns.
	invCalls, invRuns, invTurns int
	// invRounds is the last round before the current CLI run's first.
	invRounds int

	// offeredRev and offeredNames are the surface's names at a revision, so
	// a call is checked against what is offered without rendering it.
	offeredRev   uint64
	offeredNames map[string]struct{}

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
		done:        make(chan struct{}),
		rounds:      firstRound,
		invRounds:   firstRound,
	}
	d.cond = sync.NewCond(&d.mu)
	for _, name := range cfg.TerminateAfter {
		d.terminators[name] = struct{}{}
	}
	return d
}

var _ llm.ToolHost = (*dispatcher)(nil)

// --- llm.ToolHost ----------------------------------------------------------

// Serve opens a session for one CLI run.
func (d *dispatcher) Serve(ctx context.Context, opts llm.ServeOptions) (llm.Served, error) {
	if d.cfg.Host == nil {
		return llm.Served{}, fmt.Errorf("toolloop: %s offers tools but has no session host",
			d.cfg.Surface.Phase())
	}
	if !opts.Concurrency.Valid() {
		// A value nothing here knows how to wait on: a profile the backend
		// should have refused when it loaded.
		return llm.Served{}, fmt.Errorf("toolloop: unknown call concurrency %q (want %q, %q or %q)",
			opts.Concurrency, llm.CallsAtOnce, llm.CallsReadOnlyAtOnce, llm.CallsInTurn)
	}
	title := d.cfg.ServerTitle
	if title == "" {
		title = d.cfg.Surface.Phase()
	}
	r := &run{opts: opts, start: time.Now(), released: make(chan struct{}), ctx: ctx}
	// THE RUN'S CONTEXT for its connection, which is the run's as much as its
	// calls are; no call runs under it — see [callContext].
	session, err := d.cfg.Host.Open(ctx, config.ToolServerName, title, &runCalls{d: d, r: r})
	if err != nil {
		return llm.Served{}, err
	}
	if opts.Relists {
		// The answer to an activation waits for the CLI's re-list, so its
		// next request offers the tool — see [toolsession.Server.HoldAnswersForRelist].
		session.Server().HoldAnswersForRelist()
	}
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
			turn.calls, turn.joined, turn.expect = t.Calls, grown.joined, grown.expect
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
		if d.pend != stateOpen && turn.round > d.pendRound {
			// THE CLI ASKED ITS MODEL AGAIN: the response that decided the
			// run's end is over, whatever of it never came.
			d.endLocked()
		}
	}
	d.adoptLocked(turn)
	d.reviewLocked(turn)
	d.endIfCompleteLocked()
	d.cond.Broadcast()
	d.mu.Unlock()
	d.frame()
}

// expectLocked marks which of a response's calls the CLI will make to the host.
func (d *dispatcher) expectLocked(t *observedTurn) {
	t.joined = make([]bool, len(t.calls))
	t.expect = make([]bool, len(t.calls))
	for i, c := range t.calls {
		t.expect[i] = c.Host && d.offeredLocked(c.Name)
	}
}

// adoptLocked joins the calls placed before the record showed their response
// to it, now that it does: that response is the round they were placed on.
// One that is not this response's moves on to the request after it.
func (d *dispatcher) adoptLocked(t *observedTurn) {
	for _, c := range d.calls[d.invCalls:] {
		if !c.provisional {
			continue
		}
		if i, ok := d.matchLocked(t, c); ok {
			d.joinLocked(c, t, i)
			c.round, c.provisional = t.round, false
			continue
		}
		if c.round <= d.rounds {
			c.round = d.rounds + 1
		}
	}
}

// reviewLocked decides what a response read means for the run: the budget's
// last round, or a note waiting while it asks for calls.
func (d *dispatcher) reviewLocked(t *observedTurn) {
	switch {
	case t.round > d.cfg.MaxRounds:
		// A REQUEST THE BUDGET DID NOT ALLOW: nothing of it runs.
		d.exhaustLocked()
		return
	case t.round == d.cfg.MaxRounds && len(t.calls) > 0:
		// THE LAST ROUND THE BUDGET ALLOWS asked for tools: once its calls
		// have all come, the run ends before the CLI can make another
		// request — at once, where none of them is the host's.
		d.decideLocked(stateExhausted, t.round)
	}
	if len(t.calls) > 0 && d.cfg.Steer != nil && d.cfg.Steer.Waiting() {
		// A NOTE WAITING while the model asks for more: the response's
		// calls run, and the next run reads the note before anything else.
		d.decideLocked(statePausing, t.round)
	}
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

// turnByID is the current run's request of the CLI's id. THIS RUN'S ONLY: an
// id is the CLI's, unique within its process at most — a backend that numbers
// a CLI's requests itself numbers each run's from one — so a later run's
// request read as an earlier run's was merged into it.
func (d *dispatcher) turnByID(id string) *observedTurn {
	if id == "" {
		return nil
	}
	for _, t := range d.turns[d.invTurns:] {
		if t.id == id {
			return t
		}
	}
	return nil
}

// roundOfRequest is the round of the current run's request of the CLI's id,
// where the host has been told of it.
func (d *dispatcher) roundOfRequest(id string) (int, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t := d.turnByID(id); t != nil {
		return t.round, true
	}
	return 0, false
}

// Spent charges what the run has spent.
func (d *dispatcher) Spent(u llm.Usage) {
	d.mu.Lock()
	d.spent.Input += u.Input
	d.spent.Output += u.Output
	d.spent.CacheRead += u.CacheRead
	d.spent.CacheWrite += u.CacheWrite
	d.mu.Unlock()
	if err := Charge(d.ctx, d.cfg.Budget, u.Total()); err != nil {
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
// was ended for a reason the next run of the loop answers — including one
// still waiting for the rest of the response that decided it.
func (d *dispatcher) Halted() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state != stateOpen || d.pend != stateOpen
}

// Pending is what the phase did since the loop last folded it.
func (d *dispatcher) Pending() []llm.Message {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.foldLockedPending()
}

// --- what each run's session serves ([runCalls]) ---------------------------

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

// offeredLocked reports whether the surface offers name now. Read through the
// surface's revision, so a call costs one integer unless the offer moved —
// rendering it deep-clones every active tool's schema.
func (d *dispatcher) offeredLocked(name string) bool {
	rev := d.cfg.Surface.Revision()
	if d.offeredNames == nil || rev != d.offeredRev {
		names := map[string]struct{}{}
		for _, def := range d.cfg.Surface.ToolDefs() {
			names[def.Name] = struct{}{}
		}
		d.offeredNames, d.offeredRev = names, rev
	}
	_, ok := d.offeredNames[name]
	return ok
}

// call decides and runs one call r's CLI made.
//
// The MCP request's context is not this call's, and the session does not hand
// it over ([toolsession.Dispatcher.Call]): a CLI that gives up on the call, or
// dies, abandons only the delivery of its answer. The call runs under the
// phase's context to its end and is recorded ([callContext]), and the
// session's release waits for it.
func (d *dispatcher) call(r *run, in toolsession.Call) toolsession.Result {
	c, refusal := d.admit(r, in)
	if c == nil {
		// A RUN THAT IS OVER takes no call — see [runCalls]. Answered, and on
		// no record: no run of the phase made it.
		return toolsession.Result{Output: refusal, Failed: true}
	}
	if refusal != "" {
		d.tellRefused(c, refusal)
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
		d.tellRefused(c, why)
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

// admit records a call r's CLI made and decides whether it may run at all — or
// answers no record at all for a call r is over for, which no run of the phase
// made.
func (d *dispatcher) admit(r *run, in toolsession.Call) (*callRec, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r.over() {
		return nil, notRun(in.Name)
	}
	c := &callRec{
		seq: len(d.calls) + 1, name: in.Name, args: in.Arguments,
		argsErr: in.ArgumentsError, run: r, exclusive: d.exclusive(in.Name),
	}
	if c.args == nil {
		c.args = map[string]any{}
	}
	if c.argsErr == "" {
		// ITS PLACE IN THE RUN'S CALL LOG, fixed here, in the order the calls
		// arrived — see [Surface.Admit]. AT ARRIVAL, before the wait that
		// joins the call to its response: two calls of one response that
		// both wait for the CLI's record wake together and take this lock
		// again in whichever order the scheduler picks, which is the racing
		// order this exists to remove. So a call refused below is in the log
		// too; that can only raise a later count, and a raised count is a
		// fresh operation rather than a write collapsed onto another.
		c.exec = d.cfg.Surface.Admit(c.name, c.args)
	}
	c.id = callIDFromMeta(in.Meta, c.run)
	d.placeLocked(c)
	if c.argsErr == "" && c.turn != nil {
		// THE ARGUMENTS THE MODEL WROTE, as the CLI's record shows them. A
		// CLI may hand its server something else for arguments that are not
		// an object — Claude Code turns an array into an object of its
		// elements — and the call is answered as one whose arguments did
		// not parse, below, rather than run on an object nobody wrote.
		c.argsErr = c.turn.calls[c.slot].ArgumentsError
	}
	if c.id == "" {
		c.id = fmt.Sprintf("call_%d", c.seq)
	}
	d.calls = append(d.calls, c)

	// Each refusal names its class: the run is not taking calls (not
	// running), the call names nothing offered here (not found), or its
	// arguments are wrong (invalid) — for a surface that is not a model.
	refuse := func(class mcp.Refusal, why string) (*callRec, string) {
		d.settleLocked(c, ToolResult{Output: why, Failed: true, Cause: class}, true)
		d.endIfCompleteLocked()
		return c, why
	}
	if d.state != stateOpen {
		return refuse(mcp.RefusalNotRunning, notRun(c.recorded()))
	}
	// THE FENCE, BEFORE ANY CALL: a node whose lease moved stops at the
	// next call, not the next model request, because the calls are what
	// reach outside the engine.
	//
	// NOT THE BUDGET. A call spends no model tokens, and its request was
	// charged when the CLI's record showed it — a charge the meter refused
	// cut the run then, and none of its calls runs. A refusal that became
	// certain since — a sibling's spend on a shared slice, or this
	// request's own exact fill — refuses the request the CLI would make
	// NEXT, which is asked where that request is decided, as a call's
	// answer goes back ([dispatcher.conclude]). Asked here, it cut a
	// request's calls the meter had already paid for, and recorded a
	// refusal of a model call nobody was about to make.
	if d.cfg.Fence != nil {
		if err := d.cfg.Fence(); err != nil {
			d.cutLocked(err)
			return refuse(mcp.RefusalNotRunning, notRun(c.recorded()))
		}
	}
	// PAST THE ROUND BUDGET: the CLI made a request the budget did not
	// allow — its record reached the engine after the request's calls did,
	// or it records none. Nothing of it runs.
	if c.round > d.cfg.MaxRounds {
		d.exhaustLocked()
		return refuse(mcp.RefusalNotRunning, notRun(c.recorded()))
	}
	if d.pend != stateOpen {
		if c.round != d.pendRound {
			// A call of a later request than the one that decided the
			// run's end: the CLI moved on.
			d.endLocked()
			return refuse(mcp.RefusalNotRunning, notRun(c.recorded()))
		}
		if d.pend == stateFinished && c.exclusive {
			// A second submission, or a park, beside a submission that
			// already finished the phase.
			return refuse(mcp.RefusalNotRunning, notRun(c.recorded()))
		}
	}
	// ONLY WHAT IS OFFERED, UNDER THE NAME THE MODEL ASKED FOR. A session
	// that listed a tool before the surface stopped offering it still
	// routes the name here, and a CLI that rewrote a name the model wrote
	// made a call nobody chose.
	if c.asked != "" && c.asked != c.name {
		return refuse(mcp.RefusalNotFound, rewritten(c.asked, c.name))
	}
	if !d.offeredLocked(c.name) {
		return refuse(mcp.RefusalNotFound, notOffered(c.name))
	}
	if c.argsErr != "" {
		// ARGUMENTS THAT DID NOT PARSE ARE ANSWERED, NOT RUN: running the
		// call on an empty object is a write the model never asked for.
		return refuse(mcp.RefusalInvalid, UnparsedArguments(c.name, c.argsErr))
	}
	c.admitted = true
	d.awaitedLocked(c, true)
	return c, ""
}

// recheck is admit's verdict again, for a call that waited for the gate: the
// phase may have ended, or finished, in the meantime.
func (d *dispatcher) recheck(c *callRec) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state == stateOpen && (d.pend != stateFinished || !c.exclusive) {
		return ""
	}
	why := notRun(c.recorded())
	d.settleLocked(c, ToolResult{Output: why, Failed: true, Cause: mcp.RefusalNotRunning}, true)
	d.endIfCompleteLocked()
	return why
}

// callIDFromMeta is the CLI's id for a call, where it sent one with it — under
// the key its run declared ([llm.ServeOptions.CallIDMeta]) and no other. A CLI
// that sends none has its calls joined to their responses by name and
// arguments.
func callIDFromMeta(meta map[string]any, r *run) string {
	if r == nil || r.opts.CallIDMeta == "" {
		return ""
	}
	id, _ := meta[r.opts.CallIDMeta].(string)
	return id
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
	// A CLI WHOSE RECORD SHOWS CALLS ONLY AFTER THEY RAN is never waited
	// for: the response that asked for this call is recorded once it is
	// answered, so a wait would hold every call for the whole of [joinWait]
	// and gain nothing — the call is placed now, and joined when the record
	// arrives ([dispatcher.adoptLocked]).
	wait := joinWait
	if r != nil && r.opts.RecordsCallsLate {
		wait = 0
	}
	deadline := time.Now().Add(wait)
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
	// THE RECORD HAS NOT SHOWN IT: the call is the request after the last
	// one the record did show, until the record catches up and says which
	// response it was ([dispatcher.adoptLocked]). Counted no sooner, so a
	// response whose calls all came before its record is one round, not one
	// per call.
	c.round, c.provisional = d.rounds+1, true
}

// findLocked is the response's call this call is, in the CLI's record: by the
// CLI's own id where it sent one; else by name and arguments; else — a CLI
// that rewrote a name its list does not hold to one it does — the one call of
// a name no tool here answers to, with the very same arguments.
func (d *dispatcher) findLocked(c *callRec) (*observedTurn, int, bool) {
	// THIS RUN'S RESPONSES ONLY: a call an earlier run's response asked for
	// and never made is not one a later run makes.
	for _, t := range d.turns[d.invTurns:] {
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
	for i, tc := range t.calls {
		if t.joined[i] || !tc.Host || t.expect[i] || tc.Name == c.name || (c.id != "" && tc.ID != "") {
			continue
		}
		if sameArgs(tc.Arguments, c.args) {
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
	if asked := t.calls[slot].Name; asked != c.name {
		c.asked = asked
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
	detacher, ok := d.cfg.Surface.(Detacher)
	return ok && detacher.Detached(name)
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
		arriving := c.turn != nil && time.Now().Before(deadline) &&
			c.turn.awaiting(c.slot, func(name string) bool { return !d.mayArriveBeside(c, name) })
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

// mayArriveBeside reports whether the CLI could make a call of name while the
// exclusive call c is still unanswered — the only calls the barrier waits to
// arrive. Another call that ends or parks the phase never is: it waits its own
// turn at the gate. On a CLI that makes calls one at a time none is, and on one
// that runs only read-only tools side by side, only a read-only call beside a
// read-only c is; any other sibling the CLI makes once c is answered, so
// waiting for it would hold c for [joinWait] and gain nothing.
func (d *dispatcher) mayArriveBeside(c *callRec, name string) bool {
	if d.exclusive(name) {
		return false
	}
	var concurrency llm.CallConcurrency
	if c.run != nil {
		concurrency = c.run.opts.Concurrency
	}
	switch concurrency {
	case llm.CallsInTurn:
		return false
	case llm.CallsReadOnlyAtOnce:
		readOnly := func(n string) bool { return d.cfg.Surface.AnnotationsOf(n).ReadOnly == mcp.Yes }
		return readOnly(c.name) && readOnly(name)
	default:
		return true
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
// request's — with the values its run was served under ([callContext]) — and
// turns a panic into the phase's end.
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
				"and the phase was stopped.", Failed: true, Cause: mcp.RefusalInternalError}
		}
		d.mu.Lock()
		c.took = time.Since(c.started)
		d.mu.Unlock()
	}()
	d.frame()
	ctx := d.ctx
	if c.run != nil {
		ctx = newCallContext(c.run.ctx, d.ctx)
	}
	// Every call is told the round it runs in ([CallRound]).
	out, err := c.exec(withCallRound(ctx, c.round), llm.ToolCall{
		ID: c.id, Name: c.name, Arguments: c.args,
	})
	if err != nil {
		// THE SURFACE ITSELF FAILED — a torn-down turn. The phase cannot
		// go on.
		d.cut(fmt.Errorf("toolloop: surface failed on %q: %w", c.name, err))
		return ToolResult{Output: c.name + " failed: the phase was stopped before it could answer.",
			Failed: true, Cause: mcp.RefusalNotRunning}
	}
	return out
}

// honour is a call's result as this phase can take it: a suspend in a phase
// that never persists a partial conversation cannot be resumed, so it is
// refused — and the model told so — on the record and the answer alike.
// FORBIDDEN, the class a surface that is not a model reads: the tool may not
// park a phase from here, whatever it was asked to do.
func (d *dispatcher) honour(res ToolResult) ToolResult {
	if res.Suspend && !d.cfg.AllowSuspend {
		res.Suspend = false
		res.Failed = true
		res.Cause = mcp.RefusalForbidden
		res.Output = "This tool cannot suspend in this phase. " + res.Output
	}
	return res
}

// conclude records a call's result and decides what it means for the run.
func (d *dispatcher) conclude(c *callRec, res ToolResult, before uint64) {
	// THE FENCE AGAIN, once the tool has returned: what used to be the next
	// round's check, before the model reads the result.
	if d.cfg.Fence != nil {
		if err := d.cfg.Fence(); err != nil {
			d.cut(err)
		}
	}

	d.mu.Lock()
	d.settleLocked(c, res, false)
	_, terminator := d.terminators[c.name]
	switch {
	case d.state.ended():
	case res.Suspend:
		// THE PARK IS AT ONCE: its call stays unanswered, so nothing more
		// of its response can come.
		d.state, d.pend, d.suspended = stateParked, stateOpen, c
		d.closeDoneLocked()
	case terminator && !res.Failed:
		// A SUBMISSION THAT RAN FINISHED THE PHASE, whatever the run was
		// ending for meanwhile — a call answered "still running" returns
		// after its run ended, and a next run would submit again.
		if d.state != stateOpen {
			d.state, d.pend = stateFinished, stateOpen
		} else {
			d.decideLocked(stateFinished, c.round)
		}
	case d.state == stateOpen:
		d.afterCallLocked(c, before)
	}
	continues := d.state == stateOpen && d.pend == stateOpen
	d.mu.Unlock()

	// THE BUDGET, ONLY WHERE THE CLI GOES ON: the answers to its response
	// are what let it ask its model again, so a refusal already certain
	// refuses that request — and is asked only then, since [Refusal]
	// records the refusal of the call its caller was about to make. A call
	// that finished, parked or ended the run is followed by no request, and
	// the phase it decided stands.
	//
	// DECIDED, NOT TAKEN: the CLI asks again only once every call of the
	// response is answered, so the response's other calls — paid for with
	// it — still run, and a submission among them still finishes the phase.
	var refused error
	if continues {
		refused = Refusal(d.ctx, d.cfg.Budget)
	}
	d.mu.Lock()
	if refused != nil && d.state == stateOpen && stateRefused > d.pend {
		d.pend, d.pendRound, d.pendErr = stateRefused, c.round, refused
	}
	d.endIfCompleteLocked()
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
// run: a note waiting, an activation the CLI will not see.
func (d *dispatcher) afterCallLocked(c *callRec, before uint64) {
	if d.cfg.Steer != nil && d.cfg.Steer.Waiting() {
		d.decideLocked(statePausing, c.round)
	}
	if r := c.run; r != nil && !r.opts.Relists && d.cfg.Surface.Revision() != before {
		d.decideLocked(stateRelist, c.round)
	}
}

// decideLocked records that the run must end in st over the response of round:
// the end is taken once the response's other calls have come
// ([dispatcher.endIfCompleteLocked]). Of two ends one response reaches, the
// one that settles more is kept — a submission over a note, the budget over an
// activation.
func (d *dispatcher) decideLocked(st dstate, round int) {
	if d.state != stateOpen {
		return
	}
	if st > d.pend {
		d.pend, d.pendRound = st, round
	}
}

// endIfCompleteLocked takes the decided end once nothing more of the response
// that decided it is to come: every call the record shows the CLI making to the
// host has reached it. A CLI asks its model again only once every call of the
// response is answered, so the call that completes the response is the one
// whose answer is withheld.
func (d *dispatcher) endIfCompleteLocked() {
	if d.pend == stateOpen || d.state != stateOpen {
		return
	}
	if r := d.currentRunLocked(); r != nil && r.opts.ObservesRequests {
		if t := d.turnOfRoundLocked(d.pendRound); t != nil && t.awaiting(-1, nil) {
			return
		}
	}
	d.endLocked()
}

// endLocked takes the decided end now.
func (d *dispatcher) endLocked() {
	if d.pend == stateOpen || d.state != stateOpen {
		return
	}
	d.state, d.pend = d.pend, stateOpen
	if d.state == stateRefused {
		d.state, d.cutErr = stateCut, d.pendErr
	}
	d.closeDoneLocked()
}

func (d *dispatcher) exhaustLocked() {
	if d.state == stateOpen && d.pend != stateFinished {
		d.state, d.pend = stateExhausted, stateOpen
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
	d.awaitedLocked(c, false)
	d.cond.Broadcast()
}

// parkLocally answers a call "still running" and ends the run; the call goes
// on, and the loop folds its result into the next run.
func (d *dispatcher) parkLocally(c *callRec) {
	d.mu.Lock()
	c.answered = true
	d.awaitedLocked(c, false)
	d.decideLocked(stateLocalPark, c.round)
	d.endIfCompleteLocked()
	d.cond.Broadcast()
	d.mu.Unlock()
	d.frame()
}

// busyLocked tells the run's backend the engine's tools started or stopped
// running.
// awaitedLocked counts an admitted call in or out of what its CLI run is
// waiting on, and tells the run's backend when the first call starts and the
// last one ends ([llm.ServeOptions.OnCall]).
//
// PER RUN, and only while the CLI waits: a call answered "still running" goes
// on, but its CLI has its answer and is working — or has been stopped — and a
// call an earlier run made is no call of this one. Counted for the phase
// instead, a call parked by one run held the next run's clock stopped and its
// login unchecked for as long as the call ran, and the next run's first call
// was never announced.
func (d *dispatcher) awaitedLocked(c *callRec, waiting bool) {
	r := c.run
	if r == nil || !c.admitted || c.waiting == waiting {
		return
	}
	c.waiting = waiting
	if waiting {
		r.waiting++
	} else {
		r.waiting--
	}
	if r.opts.OnCall != nil && ((waiting && r.waiting == 1) || (!waiting && r.waiting == 0)) {
		r.opts.OnCall(waiting)
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
	d.state, d.cutErr, d.pend = stateCut, err, stateOpen
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

// UnparsedArguments is the result a call whose arguments did not parse is
// answered with: what went wrong, and what to do about it, in words a model
// acts on.
//
// EXPORTED FOR THE BRIDGE, which answers an executor run's calls with it: the
// rule — such a call is answered, never run on an empty object — is this
// loop's, and a second copy of its wording is the one a model would learn to
// fix differently depending on where its CLI ran.
func UnparsedArguments(name, why string) string {
	return fmt.Sprintf("%s was NOT run: its arguments could not be read (%s). "+
		"Call it again with its arguments as one complete JSON object.", name, why)
}

// tellRefused tells a surface that keeps a record of its own ([Refuser]) that
// one of its calls was answered without running, and why.
func (d *dispatcher) tellRefused(c *callRec, why string) {
	if r, ok := d.cfg.Surface.(Refuser); ok {
		r.Refused(d.ctx, llm.ToolCall{ID: c.id, Name: c.name, Arguments: c.args}, why)
	}
}
