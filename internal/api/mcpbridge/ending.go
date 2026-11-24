package mcpbridge

import (
	"context"
	"crypto/rand"
	"net/http"
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

// endAnswerWait bounds how long an opening's end waits for its calls'
// answers to be written before it closes the box's sessions on it
// ([binding.endKeeping]): FIVE SECONDS. Sized for a call whose tool returned
// just before the end, whose answer follows its record's update and retries
// and a re-list's hold (under four seconds on a store that answers), and for
// a hook's take begun before the end ([steerTakeBudget], the same five); a
// store silent for five is in an outage this wait would not see the end of.
// Past it the end waits no longer, and the box's sessions close behind it once
// the answers are out. NOTHING HOLDS THE SUM TO THE BOUND, since those waits
// live in other packages: moving one is reading the package doc's "The end's
// bound" again, which holds the sizing. Not a setting: nothing about a
// deployment moves what it is sized for.
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

	// own, cutFirst and note are under the opening's mu, and decided once:
	// own where the call's own answer answers it — its tool returned while
	// the opening was live ([binding.claim]), or its entry was refused
	// ([binding.writeAhead]); cutFirst where the opening answered it with note
	// before its tool returned, and closed cut: its end ([binding.endKeeping])
	// or its harness's ceiling ([binding.park]) — and unrun where that answer
	// is that the call was not run, its entry still being written as the end
	// came, which is a failure: nothing ran.
	own      bool
	cutFirst bool
	unrun    bool
	note     string
	cut      chan struct{}
	// marked, set with cutFirst by a park, is closed once the park's mark on
	// the run's row is decided ([binding.park]); nil for every other call.
	marked chan struct{}

	// name and digest are the call's tool and its arguments' digest, and
	// request the request it came on, set as it is held; waiters is the
	// request of each call its harness sent again that joined it
	// ([binding.join]), under the opening's mu. settled is closed once its own
	// answer is decided, final: what each joined request is answered.
	name, digest string
	request      callRequest
	waiters      []callRequest
	settled      chan struct{}
	final        toolsession.Result

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
// the opening's before its tool returned. Under the opening's mu.
func (c *bridgedCall) answeredLocked() bool { return c.own || c.cutFirst }

// cutAnswer is the answer the opening handed the call before its tool
// returned: not a failure for a call it answered as still running (see the
// package doc), and a failure for one its end answered as not run (unrun).
// Read only once cut is closed, which is done after the note is set.
func (c *bridgedCall) cutAnswer() toolsession.Result {
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

	// answering, nil in production, runs in each call's handler before it waits
	// on the call, handed the call's recorded channel (export_test.go).
	answering func(recorded <-chan struct{})

	// joined, nil in production, runs as a call sent again joins the call still
	// running ([binding.join], export_test.go).
	joined func()

	// posts is every POST being served, by the mark its request carries
	// ([postHeader]): closed where the box dropped it ([bridgedCalls.watchPost]).
	posts map[string]chan struct{}
}

// postHeader marks a POST the bridge serves with a key of its own, so a call
// that came on it can find whether the box dropped it ([bridgedCalls.lostOf]).
// Always the bridge's: a box-sent one is dropped.
const postHeader = "Crewlet-Call-Request"

// watchPost marks r, a request to the MCP endpoint, where it is a POST, and
// answers its key, "" for none. The SDK cancels nothing of a call whose POST
// the box dropped (go-sdk v1.8.0 keeps the stream for a resumption no harness
// of the bridge's makes), so this is the one place that loss is seen.
func (r *bridgedCalls) watchPost(req *http.Request) string {
	req.Header.Del(postHeader)
	if req.Method != http.MethodPost {
		return ""
	}
	key := rand.Text()
	req.Header.Set(postHeader, key)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.posts == nil {
		r.posts = map[string]chan struct{}{}
	}
	r.posts[key] = make(chan struct{})
	return key
}

// postEnded lets go of the POST key marks, saying it was dropped where lost:
// its handler returned with its request's context ended, the box gone before
// its answers were written.
func (r *bridgedCalls) postEnded(key string, lost bool) {
	if key == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if ch, ok := r.posts[key]; ok {
		if lost {
			close(ch)
		}
		delete(r.posts, key)
	}
}

// lostOf is closed once the box drops the POST header marks, nil where it
// marks none being served.
func (r *bridgedCalls) lostOf(header http.Header) <-chan struct{} {
	key := header.Get(postHeader)
	if key == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.posts[key]
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
// anything: a call on an opening still serving when this is asked runs on to
// its own bound, and a process stopped first ends it, and so does a call
// admitted after this is asked. No bound of its own: ctx's is the caller's, and
// a call's own bound ends it (wait.go). Nil-safe.
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

// answersOut waits until no answer the opening gave is on its way, or until
// bound fires (a nil bound waits as long as that takes), and reports whether
// it found none: the answer of every call it holds ([binding.holdLocked]) and
// of every call it answered without running ([binding.holdRefusalLocked]), a
// hook's ([binding.holdHook]) and a request it let open a session
// ([binding.holdSessionOpen]) among them, each until its request is over
// ([toolsession.Call.Over]).
//
// EACH PASS READS THE OPENING AGAIN, under its lock, so an answer given while
// it waited (the refusal of a call reaching it after its end) is waited for
// too; one given after the pass that found none is written only if its write
// beats the close that follows, since the SDK writes none once a close begins.
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
// IT WAITS FOR NO CALL: under the opening's lock it refuses every later call,
// answers each call it took whose tool has not returned as still running
// ([ending.stillRunning]) and each still writing its entry or awaiting its
// admission ([binding.writeAhead]) as not run ([ending.notRun]), then waits at
// most [endAnswerWait] for the opening's answers ([binding.answersOut]) before
// it closes the box's sessions. NOTHING IS OWED THE RUN'S LOG AS IT ENDS. Why,
// in the package doc ("An opening ends without waiting for its calls"). ONCE:
// an end that finds the opening ended already returns at once.
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
		c.cutFirst, c.note = true, how.stillRunning()
		if !c.begun {
			c.note, c.unrun = how.notRun(), true
			unbegun++
		}
		close(c.cut)
		cut++
	}
	inbox := o.steer
	o.steer, o.endedSteer = nil, inbox
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
		recorded: make(chan struct{}), cut: make(chan struct{}), began: make(chan struct{}),
		settled: make(chan struct{})}
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
// it, as a refused call's answer is held ([binding.holdRefusalLocked]), and
// reports whether the opening may serve it: false, holding nothing, once the
// opening has ended. served is closed once the handler has returned the
// request ([Bridge.serveMCP]): THE HANDLER'S RETURN, NEVER THE REQUEST'S
// CONTEXT, which ends first where the box gave the request up, its connection
// failed or a relay closed it, while the handler still opens or answers on the
// session. UNDER ONE LOCK WITH THE END, which marks the opening ended before it
// waits: a request either came first and is waited for, or is never served.
// The package doc's "A session opened as its opening ends" has why.
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
// call's request is over (over is its context's Done, as a call's
// [toolsession.Call.Over] is), as a refused call's answer is held
// ([binding.holdRefusalLocked]), and reads the opening's note inbox under the
// same lock: nil once the opening has ended, or where it has none. A hook's
// call never reaches [binding.Call], which holds every other answer. THE HOLD
// FIRST, THE INBOX AFTER, UNDER ONE LOCK: an end either came before, and the
// hook takes nothing, or it finds the hold and waits for the answer before it
// closes the box's sessions. HELD PAST THE END TOO, so a late hook's empty
// answer reaches the box rather than the close's failure. A cancel or a failed
// connection lets the hold go unwritten, which loses nothing; the package
// doc's "A hook's answer, held for the end" has why.
func (o *binding) holdHook(over <-chan struct{}) SteerInbox {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.holdRefusalLocked(over)
	if o.ended {
		return nil
	}
	return o.steer
}

// cancelInbox is the inbox a hook's cancel reaches ([SteerInbox.Cancelled]):
// the opening's while it serves, and the one its end took off it after, so a
// cancel whose POST reached the opening as it ended is still heard. The inbox
// acts on the job it was bound to, closed or not.
func (o *binding) cancelInbox() SteerInbox {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.steer != nil {
		return o.steer
	}
	return o.endedSteer
}
