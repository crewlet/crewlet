package mcpbridge

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/toolsession"
)

// ending is how an opening ended, which is what its box is told of a call the
// end answers, or refuses after it: whether the run is over for the box, or
// the box goes on calling, served from then on by another opening. See the
// package doc, "An opening ends without waiting for its calls".
//
// A NAMED STRING for the log line that names it ([binding.endKeeping]); it
// never crosses a wire, so it has no unknown value to read.
type ending string

const (
	// runEnded is an end after which the opening's run is over for its box:
	// the run settled, was stopped, reclaimed or cut, or its launch failed
	// ([Bridge.Close]); or a relaunch replaced the job the opening served
	// ([Bridge.Open] of another job of the run), and its own box carries the
	// run on.
	runEnded ending = "run_ended"

	// boxGoesOn is an end after which the opening's box goes on running and
	// calling: the seat left this node ([Bridge.ReleaseSeat]), and its next
	// holder serves the box; or this node opened the same job again, a
	// recovery ([Bridge.Open] of the same job), and the new opening serves
	// it.
	boxGoesOn ending = "box_goes_on"
)

// The answers a call is handed as the opening it was made on ends. Each says
// only what holds as it is written: the call was admitted and begun before the
// end, so its tool runs to its end and its effect is still to come, which is
// why it must not be made again. NEITHER SAYS WHERE ITS RESULT GOES: its
// identity is on the run's log from before it ran, and its result lands there
// only where the log still takes its update — a run whose record is deleted
// before the call returns, or a store that refuses the update until the call's
// bound, leaves it with none (see the package doc) — so a promise that the
// result reaches the run's log would not hold for every call so answered.
const (
	// endedRunningNote answers a call still running as its opening ends where
	// the run ended for its box. NOT A FAILURE, as the tool loop's own "still
	// running" is not: see the package doc.
	endedRunningNote = "This run has ended; this call is still running, so do not make it again. " +
		"End your turn now."

	// stillRunningNote answers a call still running as its opening ends where
	// the box goes on calling. Not a failure either.
	stillRunningNote = "This call is still running; do not make it again."

	// closingNote answers a call that reaches an opening after its end where
	// the box goes on: the call never ran, and its box's next call reaches the
	// opening that serves it now. Failed, as [endedNote] is: nothing ran.
	closingNote = "This call was not run: the session it reached was closing. " +
		"Call it again if it is still needed."

	// unrecordedNote answers a call whose entry the run's log did not take
	// ahead of it ([binding.writeAhead]): a coordination store that refused every
	// attempt within the ledger's bound, or a log holding as many calls in
	// flight as it keeps. The call never ran, so making it again is safe:
	// FAILED, as closingNote and endedNote are, and in closingNote's shape,
	// with its own reason — the call could not be recorded, so it did not run
	// — since a call that ran with no record of it could be counted by no
	// later call of the run (see the package doc, "A call is on its run's
	// record before it runs").
	unrecordedNote = "This call was not run: it could not be recorded. " +
		"Call it again if it is still needed."
)

// stillRunning is the answer of a call whose tool is still running as an
// opening ends this way.
func (e ending) stillRunning() string {
	if e == boxGoesOn {
		return stillRunningNote
	}
	return endedRunningNote
}

// notRun is the answer of a call that reaches an opening after it ended this
// way, which never runs it.
func (e ending) notRun() string {
	if e == boxGoesOn {
		return closingNote
	}
	return endedNote
}

// endAnswerWait bounds how long an opening's end waits for the answers of its
// calls to be written before it closes the box's sessions on it
// ([binding.endKeeping]): FIVE SECONDS.
//
// The end's own answers to the calls it holds are written at once: the call's
// beat is stopped and the SDK writes the answer as any, and a relayed one lands
// in the relay's buffer, which never blocks below its bound
// (internal/httprelay). What the bound is sized for is a call whose tool
// returned just before the end, whose own answer follows its record: the
// update of its entry on the run's log and its retries (internal/engine's
// appendRetryWaits, 1.75 seconds of waits between four attempts) and, after an
// activation, the hold for the box's re-list (internal/toolsession's
// relistWait, two seconds) — under four seconds on a coordination store that
// answers. FIVE, as the write ahead of a call and the record of a job's
// session start are given (internal/engine's beginBudget and
// sessionStartRecordBudget), and for their reason: a store that has not
// answered in five is in an outage this wait would not see the end of. Past
// it the end waits no longer: an answer still on its way is one a box is not
// reading, and the box's sessions are closed behind the end once it is out
// (see the package doc). NOTHING HOLDS THE SUM TO THE BOUND: both waits live in
// other packages, one of them the engine's, which this one cannot import, so
// moving either is reading this again.
//
// A HOOK'S TAKE OF THE RUN'S NOTES, under way as the end comes, is waited for
// within it too ([binding.holdHook]): a take is bounded by [steerTakeBudget],
// the same five seconds, started as the take starts, so the answer of a take
// begun before the end is due within this wait wherever the store answers. A
// take whose store stalls past its budget is waited for behind the end, as any
// answer past this bound is, and is never refused while its box still waits
// for it. A box that gave the call up, or whose connection failed, waits for
// no answer, and its request's end lets the hold go before any write. Nothing
// holds the two to each other either: a take's budget raised past this wait
// would leave more ends returning with their close behind them, never an
// answer its box waits for refused.
//
// Not a setting: nothing about a deployment moves what it is sized for.
const endAnswerWait = 5 * time.Second

// bridgedCall is one call an opening took, from its arrival until its
// answer is out ([binding.Call]), and in the bridge's registry until it is
// recorded ([bridgedCalls]).
type bridgedCall struct {
	// run is the run the call was made for.
	run string
	// opening is the opening that took it.
	opening *binding
	// over is the call's request's [toolsession.Call.Over]: closed once its
	// answer is out. Nil for a call no request carries.
	over <-chan struct{}
	// recorded is closed once the call's record step has run
	// ([binding.conclude]): what [Bridge.AwaitCalls] waits on.
	recorded chan struct{}

	// own, cutByEnd and note are under the opening's mu, and decided once:
	// own where the call's own answer answers it — its tool returned while
	// the opening was live ([binding.claim]), or its entry was refused
	// ([binding.writeAhead]); cutByEnd where the opening's end answered it with
	// note first, and closed cut — and unrun where that answer is that the
	// call was not run, its entry still being written as the end came, which
	// is a failure: nothing ran.
	own      bool
	cutByEnd bool
	unrun    bool
	note     string
	cut      chan struct{}

	// begun is under the opening's mu, and began closed with it: the call's
	// entry is on the run's log, the call admitted to the run's call log, and
	// its tool runs ([binding.writeAhead]). Until then an end answers it as not
	// run, and it never runs.
	begun bool
	began chan struct{}

	// admitAfter is the decision of the call that reached the opening just
	// before this one — closed once that call was admitted to the run's call
	// log or refused — and nil for the opening's first; decided is this call's
	// own, closed once it is. Set as the call is held, under the opening's mu:
	// so the calls an opening admits are admitted in the order they arrived,
	// whichever entry's write came back first ([binding.writeAhead]).
	admitAfter <-chan struct{}
	decided    chan struct{}
}

// answeredLocked reports whether the call's answer is decided: its own, or
// its opening's end's. Under the opening's mu.
func (c *bridgedCall) answeredLocked() bool { return c.own || c.cutByEnd }

// endsAnswer is the answer the opening's end handed the call: not a failure
// for a call it answered as still running (see the package doc), and a failure
// for one it answered as not run (unrun). Read only once cut is closed, which
// the end does after it sets the note.
func (c *bridgedCall) endsAnswer() toolsession.Result {
	return toolsession.Result{Output: c.note, Failed: c.unrun}
}

// bridgedCalls is the bridge's registry of the calls it runs: every call an
// opening of it took, from its arrival until its record step has run,
// whatever became of the opening — and every close of an opening's sessions
// its end left to finish behind it.
//
// THE BRIDGE OWNS THOSE: [binding.Call] starts the goroutine that runs a call's
// tool and records it, under the run's context, which nothing cancels, and the
// tool's own bound ends it ([binding.conclude]); a close behind an end ends
// once the answers it waits for are out, at the latest when the box's
// connection ends. [Bridge.Shutdown] stops neither. [Bridge.AwaitCalls] waits
// for the calls, as a node's drain does once it has given its seats back.
type bridgedCalls struct {
	mu  sync.Mutex
	set map[*bridgedCall]struct{}

	// closing is every close of an opening's sessions its end left running
	// ([binding.endKeeping]).
	closing sync.WaitGroup
}

func newBridgedCalls() *bridgedCalls {
	return &bridgedCalls{set: map[*bridgedCall]struct{}{}}
}

// add registers a call its opening just took.
func (r *bridgedCalls) add(c *bridgedCall) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.set[c] = struct{}{}
}

// done lets go of a call whose record step has run, and says so.
func (r *bridgedCalls) done(c *bridgedCall) {
	r.mu.Lock()
	delete(r.set, c)
	r.mu.Unlock()
	close(c.recorded)
}

// running is every call the bridge runs now.
func (r *bridgedCalls) running() []*bridgedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*bridgedCall, 0, len(r.set))
	for c := range r.set {
		out = append(out, c)
	}
	return out
}

// AwaitCalls waits until every call this bridge runs whose opening has ended
// by the time it is asked has been recorded ([binding.conclude]), or until ctx
// ends: what a node's drain asks once it has given its seats back
// (internal/engine's Engine.Drain), which ended every seat's openings, so a
// call those ends answered as still running makes its record before the node
// tears down what it writes through. NOTHING ELSE IS WAITED FOR, by this or by
// anything: a call on an opening still serving when this is asked — the
// Dashboard Assistant's deep work, whose opening ends with its own run and
// answers its calls as any end does — runs on to its own bound, and a process
// stopped first ends it, and so does a call admitted after this is asked. No
// bound of its own: ctx's is the caller's, and a call's own bound ends it
// (wait.go). Nil-safe.
func (b *Bridge) AwaitCalls(ctx context.Context) {
	if b == nil {
		return
	}
	var waiting []*bridgedCall
	for _, c := range b.calls.running() {
		if c.opening.over() {
			waiting = append(waiting, c)
		}
	}
	if len(waiting) == 0 {
		return
	}
	runs := make([]string, 0, len(waiting))
	seen := map[string]bool{}
	for _, c := range waiting {
		if !seen[c.run] {
			seen[c.run] = true
			runs = append(runs, c.run)
		}
	}
	log.InfoContext(ctx, "mcp_bridge_calls_draining", "calls", len(waiting), "run_ids", runs,
		"detail", "tool calls still running as their runs' sessions on the bridge ended; "+
			"waiting for each to return and be recorded")
	for n, c := range waiting {
		select {
		case <-c.recorded:
		case <-ctx.Done():
			log.WarnContext(ctx, "mcp_bridge_calls_drain_cut", "calls", len(waiting)-n,
				"error", ctx.Err().Error(),
				"detail", "stopped waiting for tool calls still running; each goes on until "+
					"its own bound or this process ends, and one the process ends is on its run's record "+
					"by its entry alone, its outcome unknown")
			return
		}
	}
	log.InfoContext(ctx, "mcp_bridge_calls_drained", "calls", len(waiting))
}

// answersOut waits until no answer the opening gave is on its way — the
// answer of every call it holds ([binding.holdLocked]) and of every call it
// answered without running ([binding.holdRefusalLocked]), until each one's
// request is over ([toolsession.Call.Over]) — or until bound fires, and
// reports whether it found none on its way. A nil bound waits as long as that
// takes. A hook's call is one it answered without running
// ([binding.holdHook]), and a request it let open a session is held so too
// ([binding.holdSessionOpen]).
//
// EACH PASS READS THE OPENING AGAIN, under its lock, so an answer given while
// it waited — the refusal of a call that reached the opening after its end —
// is waited for too. One given after the pass that found none is written only
// where its write comes before the close that follows: the SDK writes no
// answer on a session whose close has begun.
func (o *binding) answersOut(bound <-chan time.Time) bool {
	for {
		o.mu.Lock()
		pending := o.answeringLocked()
		o.mu.Unlock()
		if len(pending) == 0 {
			return true
		}
		for _, over := range pending {
			select {
			case <-over:
			case <-bound:
				return false
			}
		}
	}
}

// answeringLocked is the request's end of every answer the opening gave that
// is still on its way ([binding.answersOut]), under mu; it lets go of every
// refusal whose answer is out.
func (o *binding) answeringLocked() []<-chan struct{} {
	o.refusals = slices.DeleteFunc(o.refusals, gone)
	pending := slices.Clone(o.refusals)
	for c := range o.running {
		if c.over != nil && !gone(c.over) {
			pending = append(pending, c.over)
		}
	}
	return pending
}

// end refuses every later call on the opening, answers every call of it still
// running, and closes every MCP session a box holds on it once those answers
// are out ([binding.endKeeping]). Nil-safe, and safe to call twice.
//
// Closing a server session runs the handler's own removal hook, so the session
// leaves the SDK's table as well as ending on the wire — which is what makes
// [Bridge.Close] release the surface rather than merely hide it.
func (o *binding) end(how ending) { o.endKeeping(nil, how) }

// endKeeping is [binding.end], closing the opening's note inbox unless it is
// keep — the inbox an opening replacing this one serves too — and telling the
// box how the opening ended (how).
//
// IT WAITS FOR NO CALL. Under the opening's lock it refuses every later call
// and answers every call it took whose tool has not returned, in the words of
// how it ended ([ending.stillRunning]) — and a call whose entry is still being
// written ahead of it, or that waits for its admission ([binding.writeAhead]),
// which it answers as not run, as a call reaching the opening after its end is
// ([ending.notRun]), failed, and which never runs; each call's handler stops
// its beat and hands that answer to the SDK as any. It then waits for every
// answer of the opening still on its way to be out ([binding.answersOut]) —
// those; the answer of a call whose tool returned before the end, which its own
// result answers once its record is made; and every answer the opening gave
// without running a call, the refusal of one that reached it after its end
// among them — for at most [endAnswerWait], and only then closes the box's
// sessions: the SDK's close of a session waits for every request in flight on
// it, and writes no answer once it has begun. Past that bound the sessions are
// closed behind it ([bridgedCalls]), once those answers are out, and the end
// returns. A hook's answer is one the opening gave without running a call, and
// is waited for so ([binding.holdHook]); and so is a request the opening let
// open a session before its end ([binding.holdSessionOpen]), whose session is
// then among those it closes.
//
// NOTHING IS OWED THE RUN'S LOG AS IT ENDS: every call the opening ran has its
// identity there from before it ran ([Ledger.Begin]), and a call still running
// makes its one update itself as its tool returns.
//
// ONCE: an end that finds the opening ended already returns at once.
func (o *binding) endKeeping(keep SteerInbox, how ending) {
	if o == nil {
		return
	}
	o.mu.Lock()
	if o.ended {
		o.mu.Unlock()
		return
	}
	o.ended, o.endedAs = true, how
	cut, unbegun := 0, 0
	for c := range o.running {
		if c.answeredLocked() {
			continue
		}
		c.cutByEnd, c.note = true, how.stillRunning()
		if !c.begun {
			c.note, c.unrun = how.notRun(), true
			unbegun++
		}
		close(c.cut)
		cut++
	}
	inbox := o.steer
	o.steer = nil
	o.mu.Unlock()
	if inbox != nil && inbox != keep {
		inbox.Close()
	}
	if cut > 0 {
		log.InfoContext(o.ctx, "mcp_bridge_answered_at_end", "run_id", o.session.RunID,
			"launch_id", o.id, "seat", o.session.Handle, "calls", cut, "not_begun", unbegun,
			"ending", string(how),
			"detail", "the box was answered that each call is still running, and each whose entry "+
				"was still being written that it was not run; each running call runs on to its own "+
				"end and is recorded as one nobody waited for")
	}
	timer := time.NewTimer(endAnswerWait)
	defer timer.Stop()
	if o.answersOut(timer.C) {
		o.closeSessions()
	} else {
		o.calls.closing.Go(func() {
			o.answersOut(nil)
			o.closeSessions()
		})
		log.WarnContext(o.ctx, "mcp_bridge_close_deferred", "run_id", o.session.RunID,
			"launch_id", o.id, "seat", o.session.Handle, "after", endAnswerWait.String(),
			"detail", "an answer to the box was still on its way: still being made, as a hook's take "+
				"waiting on its store, or written and not yet read; the box's sessions on this opening "+
				"are closed once it is out, or once the box's connection ends")
	}
}

// closeSessions closes every MCP session a box holds on the opening.
func (o *binding) closeSessions() {
	for _, ep := range o.order {
		for ss := range ep.server.MCP().Sessions() {
			if err := ss.Close(); err != nil {
				log.Debug("mcp_bridge_session_close", "error", err)
			}
		}
	}
}

// holdLocked holds a call the opening just took, under mu: with the
// opening, whose end answers it and waits for its answer, and with the bridge,
// which runs its tool to its end and records it ([bridgedCalls]). over is the
// call's request's [toolsession.Call.Over]. And it lets go of every call the
// opening held whose answer is decided and out.
func (o *binding) holdLocked(over <-chan struct{}) *bridgedCall {
	for held := range o.running {
		if held.answeredLocked() && (held.over == nil || gone(held.over)) {
			delete(o.running, held)
		}
	}
	c := &bridgedCall{run: o.session.RunID, opening: o, over: over,
		recorded: make(chan struct{}), cut: make(chan struct{}), began: make(chan struct{})}
	o.running[c] = struct{}{}
	o.calls.add(c)
	return c
}

// holdRefusalLocked holds, under mu, the answer of a call the opening answers
// without running it, until the call's request is over: over is its
// [toolsession.Call.Over], and nil, a call no request carries, holds nothing.
// The opening's end waits for it before it closes the box's sessions
// ([binding.answersOut]), which write no answer once their close has begun —
// so the refusal of a call that reaches the opening after its end, which
// tells the box what the end means, reaches the box rather than the close's
// "request terminated without response". A hook's answer is held the same way
// ([binding.holdHook]): the opening runs no tool for a hook's call either. And
// it lets go of every such answer that is out.
func (o *binding) holdRefusalLocked(over <-chan struct{}) {
	o.refusals = slices.DeleteFunc(o.refusals, gone)
	if over != nil {
		o.refusals = append(o.refusals, over)
	}
}

// holdSessionOpen holds a request that opens a session on the opening
// ([opensSession]) for the opening's end until the MCP handler has returned
// it, as the answer of a call it refuses is held ([binding.holdRefusalLocked]),
// and reports whether the opening may serve it: false, holding nothing, once
// the opening has ended. served is closed once the handler has returned the
// request ([Bridge.serveMCP]).
//
// THE SDK OPENS THE SESSION AS IT SERVES THE REQUEST, and the end closes the
// sessions the opening's servers hold as it finds them once the answers it
// waits for are out ([binding.closeSessions]). So the end waits for the request:
// its session is then one of those it closes, and the request's answer, an
// initialize's, reaches the box before the close, which writes none. A
// session opened after the close began would be one nothing ever closed.
//
// THE HANDLER'S RETURN, NEVER THE REQUEST'S CONTEXT. The context ends before
// it where the box gave the request up, its connection failed, or the node
// that relayed it closed it, and the handler goes on: still opening the
// session, which it would then file after the end's close had looked, or
// still answering on it, whose close waits for that answer, as the SDK's close
// of a session waits for every request in flight on it, and so held the end
// past its bound. By the handler's return the session is filed among the
// server's, or closed.
//
// UNDER ONE LOCK WITH THE END, which marks the opening ended under it before
// it waits for anything ([binding.endKeeping]): a request either came first,
// and the end waits for it, or finds the opening ended, and is never served.
func (o *binding) holdSessionOpen(served <-chan struct{}) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ended {
		return false
	}
	o.holdRefusalLocked(served)
	return true
}

// holdHook holds the answer of a hook's call for the opening's end until the
// call's request is over, as the answer of a call the opening refuses is held
// ([binding.holdRefusalLocked]), and reads the opening's note inbox under the
// same lock: nil once the opening has ended, or where it has none. over is
// the request's context's Done, as a call's [toolsession.Call.Over] is:
// closed once the answer's write has returned, or BEFORE ANY WRITE where the
// box gave the call up or its connection failed. The SDK cancels the
// request's context on the box's `notifications/cancelled` while the answer is
// still to come, and on the connection's failure (its reader's exit, a broken
// writer, go-sdk v1.8.0). The hold then lets go with the answer unwritten, and
// an end may close the box's sessions under it, which loses nothing the hold
// could keep: a box that gave the call up discards a late answer (measured on
// Claude Code, [steerTakeBudget]), and a failed connection carries none.
//
// A HOOK'S CALL NEVER REACHES [binding.Call], which holds every answer it
// gives: the opening's own server answers it in a middleware of its own,
// steer_take's ([binding.steerMiddleware]) and skill_loaded's
// ([binding.skillMiddleware]). So an end once waited for no hook's answer,
// and one that came after the end began to close the box's sessions was
// refused by the SDK, which writes no answer on a session whose close has
// begun. A take whose write had landed on the run's record answered into that
// close: the record held its notes handed to a step that never had them, and
// nothing said otherwise, since a box sends no cancel for an answer it never
// got. And the hook's call failed, its request ended with no answer, which no
// probe has measured a harness's handling of: what was measured is a hook's
// call answered with an error, which Claude Code answers by dropping and
// re-initialising the whole bridge (see steer.go's note inbox).
//
// THE HOLD FIRST, THE INBOX AFTER, UNDER ONE LOCK: an end either came before,
// and the hook takes nothing from the inbox, or it finds the hold, and waits
// for the hook's answer ([binding.answersOut]) before it closes the box's
// sessions. HELD PAST THE END TOO, as the refusal of a call after it is: the
// empty answer of a hook that reaches the opening while its end waits reaches
// the box, rather than the close's failure.
func (o *binding) holdHook(over <-chan struct{}) SteerInbox {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.holdRefusalLocked(over)
	if o.ended {
		return nil
	}
	return o.steer
}
