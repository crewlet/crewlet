package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/words"
)

// A box is its launching node's (internal/sandbox, boxhost.go); this file is
// how the fleet reaches it: a read of the box asked of its host
// ([sandbox.BoxReader], [topics.SandboxBoxRead], answered only by the node
// named), and a nudge to its waiter. A read pauses the box, so it crosses the
// fleet-signed channel (internal/signedask) and the host reads only where the
// run's row licenses it ([sandbox.Coordinator.ReadHostedBox]).
// [sandbox.BoxReadRequest.BudgetMS] carries the asker's wait as a duration the
// host counts on its own clock: an instant read on another machine's clock
// overran the asker's give-up or refused every read. The instant only binds a
// replay ([sandbox.BoxReadClockAllowance]). The host registers wider than it
// holds ([boxReadAnswers]), answering busy past that, so no read waits unseen.
// A [sandbox.Nudge] is a hint ([topics.SandboxNudge]), so it travels unsigned.

// boxReadWire is the box-read channel's envelopes ([sandbox.BoxReadChannel]).
type boxReadWire = signedask.Channel[sandbox.BoxReadRequest, sandbox.BoxReadReply]

// boxReads asks a box's host to read it back, and answers for the boxes on
// this node's host.
type boxReads struct {
	node  string
	queue queue.EventQueue
	wire  *boxReadWire
	// taken is the ids of the requests this node has answered.
	taken *signedask.Guard
	// host is this node's coordinator, current: the runtime is published
	// after the node is built, and replaced never.
	host func() *sandbox.Coordinator
	// budget is how long an ask of a run's box waits for the host, by what
	// the read may do there: [sandbox.BoxReadBudget], held on the value so a
	// case can watch a host stay silent without waiting minutes for it.
	budget func(sandbox.PendingRun, sandbox.BoxRead) time.Duration
	// now is the clock this node's half of the channel binds and admits on:
	// the instant a request it seals is bound to, and the clock it judges a
	// request it takes by. time.Now, held on the value so a case can set two
	// nodes' clocks apart. Only those two: no wait is ever counted on it,
	// since a context runs on the runtime's own clock.
	now func() time.Time
	// answerRead is the read a host makes for an admitted request:
	// [sandbox.AnswerBoxRead], held on the value as budget is, so a case can
	// watch when a read starts and how long it is given.
	answerRead func(context.Context, *sandbox.Coordinator, sandbox.BoxReadRequest) sandbox.BoxReadReply
	// slots are the reads of this node's boxes it makes at once for its
	// peers, and the reads waiting for one.
	slots *boxReadSlots
}

// newBoxReads builds this node's half of the box-read channel over the fleet
// keyring every state-log record is signed under.
func newBoxReads(ring recordseal.Keyring, node string, q queue.EventQueue,
	host func() *sandbox.Coordinator) (*boxReads, error) {
	wire, err := signedask.New[sandbox.BoxReadRequest, sandbox.BoxReadReply](sandbox.BoxReadChannel, ring,
		signedask.ClockAllowance(sandbox.BoxReadClockAllowance))
	if err != nil {
		return nil, fmt.Errorf("engine: the box-read channel's envelopes: %w", err)
	}
	return &boxReads{node: node, queue: q, wire: wire, taken: signedask.NewGuard(), host: host,
		budget: boxReadBudget, now: time.Now, answerRead: sandbox.AnswerBoxRead,
		slots: newBoxReadSlots(boxReadsHeld, boxReadsWaiting)}, nil
}

var _ sandbox.BoxReader = (*boxReads)(nil)

// boxReadBudget is [sandbox.BoxReadBudget] of run's read, by what the runner of
// its harness settles reading its main thread: every node runs this build's
// runners, so the asker's say what the host's do.
func boxReadBudget(run sandbox.PendingRun, read sandbox.BoxRead) time.Duration {
	return sandbox.BoxReadBudget(run, read, runnerThreadTraits()[run.Harness])
}

// runnerThreadTraits is what each harness's runner settles reading its main
// thread, by name.
var runnerThreadTraits = sync.OnceValue(func() map[string]sandbox.ThreadTraits {
	traits := map[string]sandbox.ThreadTraits{}
	for name, runner := range sandboxRunners() {
		traits[name] = sandbox.ThreadTraitsOf(runner)
	}
	return traits
})

// ReadBox asks the node whose host run's box is on to read it back — see
// [sandbox.BoxReader].
func (b *boxReads) ReadBox(ctx context.Context, run sandbox.PendingRun,
	read sandbox.BoxRead) (sandbox.Result, bool, error) {

	// THE ASKER CARRIES ITS OWN REMAINING WAIT, the budget or its context's
	// where that ends sooner, so the host's share is of what this asker will
	// actually wait, counted from the host's receipt on the host's own clock:
	// a host that took the budget for an asker that gives up sooner would go
	// on reading — taking a session out of the box — for an answer nobody
	// reads, beside the asker's next attempt at the same read.
	wait := b.budget(run, read)
	if asked, ok := ctx.Deadline(); ok {
		wait = min(wait, time.Until(asked)) // the context's own clock
	}
	if wait < time.Millisecond {
		// NOTHING LEFT TO ASK WITH: a request carrying no wait is one every
		// host refuses, so it never crosses.
		return sandbox.Result{}, false, fmt.Errorf("%w: %s, with no time left to ask",
			sandbox.ErrBoxNotAsked, run.LaunchNode)
	}
	req := sandbox.BoxReadRequest{
		Version: sandbox.BoxReadWireVersion, ID: signedask.NewID(),
		Deadline: b.now().Add(wait), BudgetMS: wait.Milliseconds(),
		Node: run.LaunchNode, TurnID: run.TurnID, LaunchID: run.LaunchID,
		SandboxID: run.SandboxID, Read: read,
	}
	sealed, err := b.wire.SealRequest(req)
	if err != nil {
		return sandbox.Result{}, false, err
	}
	askCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	replies, err := b.queue.Ask(askCtx, topics.SandboxBoxRead, sealed, 1)
	if err != nil {
		// THIS NODE COULD NOT ASK: no evidence about the box, and none about
		// its host, handled as a host that did not answer.
		return sandbox.Result{}, false, fmt.Errorf("%w: asking %s: %w",
			sandbox.ErrBoxNotAsked, run.LaunchNode, err)
	}
	for _, body := range replies {
		rep, why, err := b.wire.OpenReply(body, req.ID)
		switch {
		case err != nil:
			log.WarnContext(ctx, "sandbox_box_read_reply_unread", "turn_id", run.TurnID,
				"node", run.LaunchNode, "error", err.Error())
			continue
		case why != "":
			log.WarnContext(ctx, "sandbox_box_read_reply_refused", "turn_id", run.TurnID,
				"node", run.LaunchNode, "detail", why)
			continue
		case rep.Node != run.LaunchNode:
			continue
		}
		if err := rep.Err(); err != nil {
			return sandbox.Result{}, false, err
		}
		return *rep.Result, rep.Ended, nil
	}
	return sandbox.Result{}, false, fmt.Errorf("%w: %s, within %v", sandbox.ErrBoxHostSilent,
		run.LaunchNode, wait)
}

// errBoxReadNotHere is a box read addressed to another node: the serve answers
// nothing for it.
var errBoxReadNotHere = errors.New("engine: the box read is addressed to another node")

// errBoxReadOtherWire is a box read this build cannot read whole: the serve
// answers nothing for it.
var errBoxReadOtherWire = errors.New("engine: a box read of another build's wire")

// answer is the serve handler: one scattered box read, answered only by the
// node it names — and only where the fleet signed it.
func (b *boxReads) answer(ctx context.Context, raw []byte) ([]byte, error) {
	// THE REQUEST'S ARRIVAL at this process, on its runtime clock, stamped by
	// the queue before any wait for one of the registration's slots
	// (queue.Arrived): every wait the read is counted for runs from here.
	arrived := queue.Arrived(ctx)
	admitted := b.wire.Admit(ctx, raw, b.taken, b.now())
	switch admitted.Disposition {
	case signedask.Admitted:
	case signedask.Tampered:
		log.ErrorContext(ctx, "sandbox_box_read_refused", "disposition", string(admitted.Disposition),
			"detail", admitted.Why+"; no box was read")
		return nil, errors.New("engine: a box read the fleet did not sign")
	default:
		// A KEY THIS NODE DOES NOT HOLD, a build it cannot read, a request
		// past its deadline and the channel's allowance for two clocks, or
		// taken already: nothing is read, and only the node it names says
		// so — the asker hears a host that did not answer, and the host's
		// own waiter announces the run again on its schedule.
		if admitted.Request.Node == b.node {
			log.WarnContext(ctx, "sandbox_box_read_refused", "disposition", string(admitted.Disposition),
				"turn_id", admitted.Request.TurnID, "detail", admitted.Why+"; no box was read")
		}
		return nil, fmt.Errorf("engine: a box read not to be taken: %s", admitted.Why)
	}
	req := admitted.Request
	if req.Node != b.node {
		return nil, errBoxReadNotHere
	}
	share, carriesWait := req.AnswerBudget(0)
	if !carriesWait || req.Version != sandbox.BoxReadWireVersion || len(req.Unread()) > 0 {
		// ANOTHER BUILD'S READ: nothing is read rather than read short, and
		// the asker hears a host that did not answer, as under a key this
		// node does not hold.
		log.WarnContext(ctx, "sandbox_box_read_refused", "disposition", "wire", "turn_id", req.TurnID,
			"detail", otherWire(req)+"; no box was read — finish the upgrade")
		return nil, errBoxReadOtherWire
	}
	host := b.host()
	if host == nil {
		return b.seal(sandbox.BoxReadReply{Version: sandbox.BoxReadWireVersion, Request: req.ID,
			Node: b.node, Error: "this node runs no sandbox runtime to read the box with"})
	}
	waited, held := b.slots.take(ctx, arrived, share)
	if !held && errors.Is(ctx.Err(), context.Canceled) {
		// THIS NODE WITHDREW AS AN ANSWERER while the read waited for a slot,
		// draining or restarting: the host is going, not busy. Answered busy,
		// its asker would hold the host present and start its silence again,
		// asking a node on its way out; answered withdrawn, it hears now a
		// host that did not answer (sandbox.BoxReadReply.Withdrawn), as it
		// does of a read the withdrawal cut.
		log.InfoContext(ctx, "sandbox_box_read_withdrawn", "turn_id", req.TurnID, "read", string(req.Read),
			"waited", waited.String(), "detail", "this node stopped answering box reads while the read "+
				"waited for one of its slots; nothing was read, and the asker hears a host that did not answer")
		return b.seal(sandbox.BoxReadReply{Version: sandbox.BoxReadWireVersion, Request: req.ID,
			Node: b.node, Withdrawn: true, Error: "this node stopped answering box reads while the read " +
				"waited for one of its slots"})
	}
	if !held {
		log.InfoContext(ctx, "sandbox_box_read_busy", "turn_id", req.TurnID, "read", string(req.Read),
			"waited", waited.String(), "detail", "this node was making as many reads of its boxes as it "+
				"makes at once with as many more waiting, or this read's share of its asker's wait ran out "+
				"while it waited for one; nothing was read, the asker was answered busy and asks again")
		return b.seal(sandbox.BoxReadReply{Version: sandbox.BoxReadWireVersion, Request: req.ID,
			Node: b.node, Busy: true})
	}
	defer b.slots.leave()
	// WHAT IS LEFT OF THE READ'S SHARE of its asker's wait: a read still
	// running when the asker stopped listening is an answer nobody reads.
	// Counted from the request's arrival on this node's clock, so a read that
	// waited for a slot ends at the same instant as its wait for one would
	// have, and the asker's last quarter stays whole for the crossings
	// ([sandbox.BoxReadRequest.AnswerBudget]). Beneath it, this node's own
	// lifetime as an answerer, which its withdrawal cancels: a read that ends
	// cut by that is answered as the host stopping
	// (sandbox.BoxReadReply.Withdrawn), never as the box's failure.
	readFor, _ := req.AnswerBudget(waited)
	readCtx, cancel := context.WithTimeout(ctx, readFor)
	defer cancel()
	return b.seal(b.answerRead(readCtx, host, req))
}

// otherWire says why req is a read this build does not take: the version it
// was sent at, the wait it carried, and the members this build does not
// declare.
func otherWire(req sandbox.BoxReadRequest) string {
	why := fmt.Sprintf("a box read of wire version %d carrying a wait of %d ms, where this build reads "+
		"version %d carrying a wait", req.Version, req.BudgetMS, sandbox.BoxReadWireVersion)
	if unread := req.Unread(); len(unread) > 0 {
		why += fmt.Sprintf(", with the members %s this build does not declare", words.And(unread))
	}
	return why
}

// seal signs a reply, its text held to the first of [sandbox.BoxReadCarries]
// whose sealed reply the broker carries — the last whatever it weighs, whose
// arithmetic is the run record's own.
func (b *boxReads) seal(reply sandbox.BoxReadReply) ([]byte, error) {
	if reply.Result == nil {
		return b.wire.SealReply(reply)
	}
	whole := *reply.Result
	var sealed []byte
	for _, budget := range sandbox.BoxReadCarries {
		carried := sandbox.CarriedResult(whole, budget)
		reply.Result = &carried
		var err error
		if sealed, err = b.wire.SealReply(reply); err != nil {
			return nil, err
		}
		if len(sealed) <= queue.MaxPayloadBytes {
			break
		}
	}
	return sealed, nil
}

// boxReadsHeld is how many reads of the boxes on this node's host it makes at
// once for its peers: as many as its registration answered at once while the
// registration's own slots bounded them (queue.ConcurrentAnswers), so a host
// below that reads exactly as it did. What they hold in bytes is the node's
// answer budget's to bound: each collection holds its weight of it until its
// reply is written (internal/sandbox/executor's collectionWeight).
const boxReadsHeld = queue.ConcurrentAnswers

// boxReadsWaiting is how many reads wait for one of those at once, each
// counting its wait against its own share of its asker's wait, so a read that
// waited reads only for what is left. A request past them is answered busy at
// once, rather than waiting where its wait could not be counted; one still
// waiting when its share runs out is answered busy then, leaving the rest of
// its asker's wait for the answer to cross.
//
// A JUDGMENT: one more round of reads behind the round in flight, so a drain's
// brief saturation (every box on a host becoming a remote collection at once,
// which answering busy would cost a lost run's session) is served in turn. A
// deeper backlog of minutes-long reads is a host that cannot keep up, whose
// askers are better told now. Waiting costs a goroutine and a decoded request.
const boxReadsWaiting = boxReadsHeld

// boxReadAnswers is the box reads' registration: every read held and waiting,
// and as many again as a registration answers at once for those answered at
// once — busy, another node's, refused — so no box read waits unseen in the
// broker's buffer for a slot, where its wait would not be counted
// ([queue.EventQueue.Serve], "A WAITING REQUEST IS HELD IN A BUFFER").
func boxReadAnswers(held, waiting int) int { return held + waiting + queue.ConcurrentAnswers }

// boxReadSlots bounds the reads of this node's boxes it makes at once for its
// peers, and the reads waiting for one.
//
// NOT A queue.AnswerGate, which counts its waiters but neither refuses past a
// bound nor says how long one waited: here a read past the waiting bound is
// answered busy at once, and a read that waited is handed only what is left of
// its share.
type boxReadSlots struct {
	// held is a token per read made now; its capacity is how many at once.
	held chan struct{}

	mu sync.Mutex
	// waiting is how many reads wait for a token now, and bound how many
	// may.
	waiting, bound int
}

// newBoxReadSlots is held reads at once, with waiting more waiting for one.
func newBoxReadSlots(held, waiting int) *boxReadSlots {
	return &boxReadSlots{held: make(chan struct{}, held), bound: waiting}
}

// take holds one read for a request that arrived at arrived, whose share of
// its asker's wait is share, both on this node's runtime clock: at once where
// one is free; else, while fewer than the bound wait, as soon as one is freed
// with some of its share left, and no later than share after its arrival, so
// a busy answer still has the rest of its asker's wait to cross. waited is how long the request had been
// here when it was held or refused; held is false for a read refused —
// past the waiting bound, its share spent waiting, or its registration
// stopping — which the caller answers busy, and true for one the caller
// [boxReadSlots.leave]s once read.
func (s *boxReadSlots) take(ctx context.Context, arrived time.Time, share time.Duration) (
	waited time.Duration, held bool) {
	select {
	case s.held <- struct{}{}:
		return s.keep(arrived, share)
	default:
	}
	s.mu.Lock()
	if s.waiting >= s.bound {
		s.mu.Unlock()
		return time.Since(arrived), false
	}
	s.waiting++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.waiting--
		s.mu.Unlock()
	}()
	spent := time.NewTimer(time.Until(arrived.Add(share)))
	defer spent.Stop()
	select {
	case s.held <- struct{}{}:
		return s.keep(arrived, share)
	case <-spent.C:
	case <-ctx.Done():
	}
	return time.Since(arrived), false
}

// keep holds the read just given a slot only while some of its share is left,
// and otherwise frees the slot and refuses it as one whose share ran out: a
// slot taken in the instant the share ran out, or for a request whose share
// had run out before it was taken, would hand the caller a read with no time
// to make, an error its asker reads as the box unread where busy is the truth.
func (s *boxReadSlots) keep(arrived time.Time, share time.Duration) (waited time.Duration, held bool) {
	if waited = time.Since(arrived); waited < share {
		return waited, true
	}
	s.leave()
	return waited, false
}

// leave frees a read [boxReadSlots.take] held.
func (s *boxReadSlots) leave() { <-s.held }

// boxReader is this node's way of asking a box's host to read it back, or nil
// on a node with no broker — which is the whole fleet, so every box is its own.
// A NIL INTERFACE there, never one holding a nil pointer.
func (e *Engine) boxReader() sandbox.BoxReader {
	if e.boxReads == nil {
		return nil
	}
	return e.boxReads
}

// armSandboxHost makes this node an answerer of box reads for the boxes on its
// host, and a listener for the nudges its waiter acts on. In every mode, as the
// tails are ([Engine.armExecutorTails]): a box on this host can be asked about
// whatever this node is doing. Its registration is wider than the reads it
// makes at once ([boxReadAnswers]), which its own slots bound
// ([boxReadsHeld], [boxReadsWaiting]).
func (e *Engine) armSandboxHost(ctx context.Context) error {
	if e.boxReads == nil {
		return nil
	}
	stopReads, err := e.backends.Queue.Serve(ctx, topics.SandboxBoxRead,
		boxReadAnswers(boxReadsHeld, boxReadsWaiting), e.boxReads.answer)
	if err != nil {
		return fmt.Errorf("engine: serve box reads: %w", err)
	}
	e.stopBoxReadServe = stopReads
	stopNudges, err := e.backends.Queue.Listen(ctx, topics.SandboxNudge, e.heardNudge)
	if err != nil {
		return fmt.Errorf("engine: listen for sandbox nudges: %w", err)
	}
	e.stopNudgeListen = stopNudges
	return nil
}

// stopSandboxHost withdraws this node as an answerer of box reads and a
// listener for nudges, before the seats are released and its boxes stop being
// driven.
func (e *Engine) stopSandboxHost(ctx context.Context) {
	for name, stop := range map[string]*queue.Unsubscribe{
		"box_reads": &e.stopBoxReadServe, "nudges": &e.stopNudgeListen,
	} {
		if *stop == nil {
			continue
		}
		if err := (*stop)(context.WithoutCancel(ctx)); err != nil {
			log.WarnContext(ctx, "sandbox_host_answerer_not_withdrawn", "what", name, "error", err)
		}
		*stop = nil
	}
}

// sandboxNudge is a [sandbox.Nudge] as it crosses the broker: with the node
// that sent it, which hears its own nudge first-hand ([Engine.nudgeRun]).
type sandboxNudge struct {
	sandbox.Nudge
	From string `json:"from"`
}

// nudgeRun tells the waiter that drives a run to look at it now: this node's
// at once, and every other node's over the broadcast. Best effort: a nudge
// lost is the run's next pass.
func (e *Engine) nudgeRun(ctx context.Context, n sandbox.Nudge) {
	e.deliverNudge(n) //nolint:contextcheck // what a nudge starts runs on the waiter's own lifetime, never on the bridged call that sent it
	if e.backends == nil || e.backends.Queue == nil {
		return
	}
	payload, err := json.Marshal(sandboxNudge{Nudge: n, From: e.id})
	if err == nil {
		err = e.backends.Queue.Broadcast(ctx, topics.SandboxNudge, payload)
	}
	if err != nil {
		log.DebugContext(ctx, "sandbox_nudge_unsent", "turn_id", n.TurnID, "why", string(n.Why),
			"error", err.Error())
	}
}

// heardNudge is the listener: a peer's nudge, told to this node's waiter. Its
// own were told first-hand.
func (e *Engine) heardNudge(_ string, payload []byte) {
	var n sandboxNudge
	if err := json.Unmarshal(payload, &n); err != nil || n.From == e.id {
		return
	}
	e.deliverNudge(n.Nudge)
}

// deliverNudge tells this node's waiter, which ignores a run it does not drive.
func (e *Engine) deliverNudge(n sandbox.Nudge) {
	if w := e.sandboxWaiter.Load(); w != nil {
		w.Nudge(n)
	}
}
