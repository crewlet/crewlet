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
// last question the idle sweep asks before it closes a quiet channel
// ([colleagueOwed]).
//
// # Asked of the holder, never decided where the question lands
//
// A park is a hold in the holding node's queue client and an alarm in its
// memory (seatpark.go): WHO HAS TO AGREE ON IT is that node alone, so only that
// node can say it holds one. The sweep runs once across the fleet, under the
// maintenance duty, on whichever node holds the duty — seldom the seat's — and
// two parks it must see are a recount of nothing it can read: a seat parked on
// a floor its first launch was refused below that is larger than the static
// one (its brief beside the static floor, or the context of the conversation's
// session it continues beside its brief), whose window has room for the static
// floor all the while (budgetpark.go; a launch held at the static floor, or at
// the room below it, was refused in a window the recount reads as short of the
// static floor too), and a seat every model of whose chain is limited, which no
// counter records (providerpark.go). Judged by the duty node's own parks,
// either was no park at all on every node but its holder, and an ask waiting
// behind it was closed under the answer still coming: a reply the asker never
// sees, refused on a closed channel.
//
// So the question goes to the holder, as what a launch of a seat's turns is
// launched on does (relaunchtarget.go): this node's own parks where it holds
// the seat, and otherwise the incarnation the seat's lease names, over an
// ephemeral scatter only that incarnation answers ([topics.HeldPark]).
//
// # Unknown is never "not owed"
//
// A lease that cannot be read, and a holder that does not answer within the
// fleet read budget — one that stopped, or a build that does not serve the
// question — are an error, which the sweep spares the channel on for the tick:
// closing is the step nothing can undo. A seat NO NODE HOLDS is in no park: a
// park lives in its holder's process, and the node that takes the seat decides
// again from the shared counters on the first delivery it is handed, which the
// sweep's own recount of them answers for ([Engine.budgetParked]).

// heldParkBudget bounds how long the sweep waits for the holder's answer: the
// fleet read budget every scatter in this engine answers inside
// ([eventfan.FleetReadBudget]) — a round trip on the broker and a look at the
// holder's own parks, which a healthy node answers in milliseconds.
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
// the duty can be on any node. After the node, whose incarnation a seat's
// lease names.
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
