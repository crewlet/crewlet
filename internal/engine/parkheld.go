package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// WHETHER A PARK HOLDS A SEAT'S INBOX, asked of the node holding the seat: the
// idle sweep's last question before it closes a quiet channel ([colleagueOwed]).
// A park is a hold in the holder's queue client and an alarm in its memory
// (seatpark.go), so only the holder can say it holds one. The sweep runs on
// whichever node has the maintenance duty, whose recount sees neither a budget
// park on a launch floor above the static one in a window with room for the
// static floor (budgetpark.go) nor a provider park (providerpark.go), and would
// close an ask under the reply still coming. So the lease's incarnation is asked
// ([topics.HeldPark]), as relaunchtarget.go does. UNKNOWN IS NEVER "NOT OWED":
// an unreadable lease or a silent holder is an error, sparing the channel for
// the tick, since closing cannot be undone. A seat no node holds is in no park:
// its next holder decides afresh from the shared counters ([Engine.budgetParked]).

// heldParkBudget bounds the sweep's wait for the holder's answer: the fleet
// read budget every scatter here answers inside ([eventfan.FleetReadBudget]),
// for a broker round trip a healthy holder answers in milliseconds.
const heldParkBudget = eventfan.FleetReadBudget

// heldParkWireVersion is the request and reply shape this build speaks.
// Evolution is ADDITIVE within a version: a key this build does not know is
// dropped on decode, which a rolling upgrade needs and nothing more.
const heldParkWireVersion = 1

// heldParkAsk is the sweep's question, addressed to the incarnation the seat's
// lease named: only that process answers, so a holder that restarted since —
// whose parks went with the process — is silent rather than answered for by
// its successor.
type heldParkAsk struct {
	Version int    `json:"v"`
	Owner   string `json:"owner"`
	Seat    string `json:"seat"`
}

// heldParkReply is the holder's answer: whether one of its parks holds the
// seat's inbox. It names its owner because a scatter's replies carry no
// sender.
type heldParkReply struct {
	Version int    `json:"v"`
	Owner   string `json:"owner"`
	Parked  bool   `json:"parked,omitempty"`
}

// errHeldParkNotAddressed is a question for another incarnation: answered with
// nothing, since its asker waits on the one it named.
var errHeldParkNotAddressed = errors.New("engine: the park question is addressed to another incarnation")

// errHeldParkUnanswered is a holder that said nothing within the budget.
var errHeldParkUnanswered = errors.New("engine: the node holding the seat did not say whether a park holds its inbox")

// inboxParks is the one question [colleagueOwed] asks of the node holding a
// seat: whether one of its parks holds the seat's inbox. [heldParks] is the
// implementation.
type inboxParks interface {
	parked(ctx context.Context, seat uuid.UUID) (bool, error)
}

var _ inboxParks = heldParks{}

// heldParks is this node as it asks, and answers, whether a park holds a seat's
// inbox: owner is this process's incarnation, which a seat's lease names; local
// this node's own parks; queue nil, or leases nil, a node that answers every
// question itself.
type heldParks struct {
	owner  string
	queue  holderAsker
	leases holderLeases
	local  func(seat uuid.UUID) bool
	budget time.Duration
	now    func() time.Time
}

// heldParks is this node's asker and answerer of whether a park holds a seat's
// inbox.
func (e *Engine) heldParks() heldParks {
	p := heldParks{local: e.inboxParkedHere, budget: heldParkBudget, now: time.Now}
	if e.node == nil {
		return p
	}
	p.owner = e.node.Owner()
	// A NODE WITH NO BROKER IS THE FLEET, and holds every seat there is: it
	// answers every question itself.
	if e.backends != nil && e.backends.Queue != nil && e.backends.Coord != nil {
		p.queue, p.leases = e.backends.Queue, e.backends.Coord
	}
	return p
}

// inboxParkedHere reports whether one of this node's parks holds seat's inbox:
// the budget's, on whatever floor it waits for room for, or the provider's.
func (e *Engine) inboxParkedHere(seat uuid.UUID) bool {
	return e.budgetParks.parking(seat) != nil || e.providerParks.parking(seat) != nil
}

// parked is whether a park of the node holding seat holds its inbox: this
// node's own parks where it holds the seat, or answers every question itself,
// and otherwise the holder's answer, asked over the broker.
func (p heldParks) parked(ctx context.Context, seat uuid.UUID) (bool, error) {
	if p.queue == nil || p.leases == nil {
		return p.local(seat), nil
	}
	lease, err := p.leases.Get(ctx, coord.SeatResource(seat))
	switch {
	case err != nil:
		// UNKNOWN, never "held by nobody": an unreadable lease table says
		// nothing about where the seat is.
		return false, fmt.Errorf("engine: which node holds seat %s could not be read: %w", seat, err)
	case lease == nil || !lease.Live(p.now()):
		// HELD BY NOBODY, and so parked by nobody — see the file comment.
		return false, nil
	case lease.Owner == p.owner:
		return p.local(seat), nil
	}
	return p.ask(ctx, lease.Owner, seat)
}

// ask puts the question to owner, the incarnation holding seat, and reads its
// answer.
func (p heldParks) ask(ctx context.Context, owner string, seat uuid.UUID) (bool, error) {
	node := config.IncarnationNode(owner)
	raw, err := json.Marshal(heldParkAsk{Version: heldParkWireVersion, Owner: owner, Seat: seat.String()})
	if err != nil {
		return false, fmt.Errorf("engine: the park question for %s, which holds seat %s, could not be "+
			"written: %w", node, seat, err)
	}
	askCtx, cancel := context.WithTimeout(ctx, p.budget)
	defer cancel()
	replies, err := p.queue.Ask(askCtx, topics.HeldPark, raw, 1)
	if err != nil {
		return false, fmt.Errorf("engine: %s, which holds seat %s, could not be asked whether a park holds "+
			"its inbox: %w", node, seat, err)
	}
	for _, body := range replies {
		var reply heldParkReply
		if json.Unmarshal(body, &reply) != nil || reply.Owner != owner {
			continue
		}
		return reply.Parked, nil
	}
	return false, fmt.Errorf("%w: %s, which holds seat %s, said nothing within %s", errHeldParkUnanswered,
		node, seat, p.budget)
}

// answer is one question, answered from this node's own parks where it is
// addressed to this process's incarnation, and with nothing where it is not.
func (p heldParks) answer(raw []byte) ([]byte, error) {
	var question heldParkAsk
	if err := json.Unmarshal(raw, &question); err != nil {
		return nil, fmt.Errorf("engine: a park question this build cannot read: %w", err)
	}
	if question.Owner == "" || question.Owner != p.owner {
		return nil, errHeldParkNotAddressed
	}
	seat, err := uuid.Parse(question.Seat)
	if err != nil {
		return nil, fmt.Errorf("engine: a park question naming no seat this build can read: %w", err)
	}
	return json.Marshal(heldParkReply{Version: heldParkWireVersion, Owner: p.owner, Parked: p.local(seat)})
}

// armHeldParks makes this node an answerer of whether one of its parks holds
// the inbox of a seat it holds.
//
// EVERY NODE SERVES, in every mode, and only the addressed incarnation
// answers: a seat can be held by a node that holds no maintenance duty, and
// the duty can be on any node. Armed after the node, whose incarnation a
// seat's lease names.
func (e *Engine) armHeldParks(ctx context.Context) error {
	if e.backends == nil || e.backends.Queue == nil || e.node == nil {
		return nil
	}
	answerer := e.heldParks()
	stop, err := e.backends.Queue.Serve(ctx, topics.HeldPark, queue.ConcurrentAnswers,
		func(_ context.Context, raw []byte) ([]byte, error) { return answerer.answer(raw) })
	if err != nil {
		return fmt.Errorf("engine: answer whether a park holds a seat's inbox: %w", err)
	}
	e.stopHeldParkServe = stop
	return nil
}

// stopHeldParks withdraws this node as an answerer, before the broker closes.
func (e *Engine) stopHeldParks(ctx context.Context) {
	if e.stopHeldParkServe == nil {
		return
	}
	if err := e.stopHeldParkServe(context.WithoutCancel(ctx)); err != nil {
		log.WarnContext(ctx, "held_park_answerer_not_withdrawn", "error", err)
	}
	e.stopHeldParkServe = nil
}
