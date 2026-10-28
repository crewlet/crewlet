package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A box on an engine host is its launching node's (internal/sandbox,
// boxhost.go), and this file is what the rest of the fleet reaches it through.
//
// # A read of the box, asked of its host
//
// A node that holds a run's seat and not its box — the seat moved while the
// job worked — collects the run by asking the box's host to read it back
// ([sandbox.BoxReader]). Every node serves [topics.SandboxBoxRead] and only the
// node the request names answers. The request is an INSTRUCTION — the host
// pauses the box a collection reads — so it crosses a channel the fleet signs
// (internal/signedask), and the host makes the read only where the run's own
// row still licenses it ([sandbox.Coordinator.ReadHostedBox]).
//
// # A nudge, told to whichever waiter drives the run
//
// A run that submits its work over its bridge is about to exit, and a node
// that leaves a box to its host wants it reclaimed now: both are said with a
// [sandbox.Nudge], told to this node's own waiters at once and broadcast
// ([topics.SandboxNudge]) to every other node's. A hint, never an instruction —
// what a waiter does with it is what its own pass would do, sooner — so it
// travels unsigned on the ephemeral broadcast.

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
	// budget is how long an ask waits for the host: [sandbox.BoxReadBudget],
	// held on the value so a case can watch a host stay silent without
	// waiting a minute for it.
	budget time.Duration
}

// newBoxReads builds this node's half of the box-read channel over the fleet
// keyring every state-log record is signed under.
func newBoxReads(ring statelog.Keyring, node string, q queue.EventQueue,
	host func() *sandbox.Coordinator) (*boxReads, error) {
	wire, err := signedask.New[sandbox.BoxReadRequest, sandbox.BoxReadReply](sandbox.BoxReadChannel, ring)
	if err != nil {
		return nil, fmt.Errorf("engine: the box-read channel's envelopes: %w", err)
	}
	return &boxReads{node: node, queue: q, wire: wire, taken: signedask.NewGuard(), host: host,
		budget: sandbox.BoxReadBudget}, nil
}

var _ sandbox.BoxReader = (*boxReads)(nil)

// ReadBox asks the node whose host run's box is on to read it back — see
// [sandbox.BoxReader].
func (b *boxReads) ReadBox(ctx context.Context, run sandbox.PendingRun,
	read sandbox.BoxRead) (sandbox.Result, bool, error) {

	req := sandbox.BoxReadRequest{
		Version: sandbox.BoxReadWireVersion, ID: signedask.NewID(),
		Deadline: time.Now().Add(b.budget),
		Node:     run.LaunchNode, TurnID: run.TurnID, LaunchID: run.LaunchID,
		SandboxID: run.SandboxID, Read: read,
	}
	sealed, err := b.wire.SealRequest(req)
	if err != nil {
		return sandbox.Result{}, false, err
	}
	askCtx, cancel := context.WithDeadline(ctx, req.Deadline)
	defer cancel()
	replies, err := b.queue.Ask(askCtx, topics.SandboxBoxRead, sealed, 1)
	if err != nil {
		// THIS NODE COULD NOT ASK: no more evidence about the box than a
		// host that did not answer, and handled as one.
		return sandbox.Result{}, false, fmt.Errorf("%w: asking %s: %w",
			sandbox.ErrBoxHostSilent, run.LaunchNode, err)
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
		run.LaunchNode, b.budget)
}

// errBoxReadNotHere is a box read addressed to another node: the serve answers
// nothing for it.
var errBoxReadNotHere = errors.New("engine: the box read is addressed to another node")

// answer is the serve handler: one scattered box read, answered only by the
// node it names — and only where the fleet signed it.
func (b *boxReads) answer(ctx context.Context, raw []byte) ([]byte, error) {
	admitted := b.wire.Admit(raw, b.taken, time.Now())
	switch admitted.Disposition {
	case signedask.Admitted:
	case signedask.Tampered:
		log.ErrorContext(ctx, "sandbox_box_read_refused", "disposition", string(admitted.Disposition),
			"detail", admitted.Why+"; no box was read")
		return nil, errors.New("engine: a box read the fleet did not sign")
	default:
		// A KEY THIS NODE DOES NOT HOLD, a build it cannot read, a request
		// past its deadline or taken already: nothing is read, and only the
		// node it names says so — the asker hears a host that did not
		// answer, and the host's own next poll announces the run again.
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
	host := b.host()
	if host == nil {
		return b.seal(sandbox.BoxReadReply{Version: sandbox.BoxReadWireVersion, Request: req.ID,
			Node: b.node, Error: "this node runs no sandbox runtime to read the box with"})
	}
	// THE ASKER'S BUDGET, its share of it: a read still running when the
	// asker stopped listening is an answer nobody reads.
	readCtx, cancel := context.WithTimeout(ctx,
		time.Duration(float64(time.Until(req.Deadline))*sandbox.BoxReadAnswerShare))
	defer cancel()
	return b.seal(sandbox.AnswerBoxRead(readCtx, host, req))
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
// host, and a listener for the nudges its waiters act on. In every mode, as the
// tails are ([Engine.armSandboxTails]): a box on this host can be asked about
// whatever this node is doing.
func (e *Engine) armSandboxHost(ctx context.Context) error {
	if e.boxReads == nil {
		return nil
	}
	stopReads, err := e.backends.Queue.Serve(ctx, topics.SandboxBoxRead, e.boxReads.answer)
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

// nudgeRun tells whichever waiter drives a run to look at it now: this node's
// own at once, and every other node's over the broadcast. Best effort: a nudge
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

// heardNudge is the listener: a peer's nudge, told to this node's waiters. Its
// own were told first-hand.
func (e *Engine) heardNudge(_ string, payload []byte) {
	var n sandboxNudge
	if err := json.Unmarshal(payload, &n); err != nil || n.From == e.id {
		return
	}
	e.deliverNudge(n.Nudge)
}

// deliverNudge tells this node's waiters, each of which ignores a run it does
// not drive.
func (e *Engine) deliverNudge(n sandbox.Nudge) {
	for _, w := range []*sandbox.Waiter{e.sandboxWaiter.Load(), e.sandboxHostWaiter.Load()} {
		if w != nil {
			w.Nudge(n)
		}
	}
}
