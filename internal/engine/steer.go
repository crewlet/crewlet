package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/signedask"
)

// A person's note to a running turn, carried on the node running it
// (ADR-0067).
//
// Until the turn launches its executor, its notes wait in its first segment's
// [steer.Box], filed here under the run id. Every node serves
// [topics.SeatSteer], and only the node whose desk holds the turn answers; the
// rest stay silent, which the asker reads as a node the turn is not on. That is
// the whole routing: nothing looks up where a turn runs.
//
// From the launch on, its notes wait on the run's record, which any node files
// on (runsteer.go); the box is handed over in the write that creates that
// record ([steerDesk.handOver]).

// steerClosedMemory is how many ended or handed turns a node remembers
// ([steerDesk.close], [steerDesk.remember]). A note can arrive seconds after its
// turn ended, and answered by nobody it reads as `unknown` (lost, try again)
// when the truth is that the turn is over, so a remembered turn is answered
// `closed`. Past it the asker says `unknown`, honest about what nobody could
// confirm.
//
// 256: even a busy node ends a turn every few seconds at most, so this is
// minutes of turns, far past any race a click can lose, for a few kilobytes of
// ids. Counted rather than timed: memory is what has to stay bounded.
const steerClosedMemory = 256

// errNotThisNode is a note for a turn this node is not running and does not
// remember. The serve answers nothing for it.
var errNotThisNode = errors.New("engine: that turn is not running on this node")

// steerDesk is this node's first segments' note boxes, and the turns it still
// answers for: ended, or handed to their run's record.
type steerDesk struct {
	mu     sync.Mutex
	open   map[string]steerTurn
	closed map[string]endedTurn
	order  []string
	gen    uint64

	// wire signs and verifies the steer channel both ways: this node's
	// operator surface asks with it ([Engine.SteerWire]), the desk answers
	// with it. taken is the ids the desk has taken.
	wire  *steerWire
	taken *signedask.Guard
	now   func() time.Time

	// runs files a handed turn's notes on its run's record. Nil on a desk
	// built without one (a case driving the scatter alone), which hands
	// nothing over.
	runs *runSteers

	// node is this node's id, which an unverified answer names: the asking
	// node logs it as the node to add the key on.
	node string
}

// steerWire is the steer channel's envelopes ([steer.Channel]).
type steerWire = signedask.Channel[steer.Request, steer.Reply]

// steerSeat is the seat a turn runs on: its derived agent id, which is what a
// person's authority to steer the turn is decided on, and its handle when the
// turn was opened, for the person reading the answer.
type steerSeat struct {
	agent  string
	handle string
}

// steerTurn is a first segment the desk holds a box for: its seat, how the
// notes the box took and nobody read are recorded expired, and — while a launch
// is handing the box to the record it writes — what an offer waits on.
type steerTurn struct {
	box *steer.Box
	steerSeat
	expire func(context.Context, []steer.Note)

	// handing is closed when the launch handing the box over says whether
	// the record holds its notes; nil while nothing is being handed.
	handing chan struct{}

	// gen is the desk's count when the entry last changed, so an offer the
	// box answered closed can tell a box that changed hands under it — look
	// again — from one that is simply closed.
	gen uint64
}

// endedTurn is a turn the desk remembers: its seat, and whether its box was
// handed to its run's record rather than closed with the turn. A handed turn's
// late note (one that read "no record" just before the launch wrote it) is
// filed on the record by this node and answered as the record answers it.
type endedTurn struct {
	steerSeat
	handed bool
}

// newSteerDesk builds node's desk over the fleet keyring every state-log
// record is signed under. The keyring is required on every node, so this fails
// only on a bootstrap [config.Bootstrap.Validate] would already have refused.
func newSteerDesk(node string, ring recordseal.Keyring) (*steerDesk, error) {
	wire, err := signedask.New[steer.Request, steer.Reply](steer.Channel, ring)
	if err != nil {
		return nil, fmt.Errorf("engine: the steer channel's envelopes: %w", err)
	}
	return &steerDesk{open: map[string]steerTurn{}, closed: map[string]endedTurn{},
		wire: wire, taken: signedask.NewGuard(), now: time.Now, node: node}, nil
}

// SteerWire is this node's steer channel, for the operator surface that asks
// on it ([builtin.SteerDeps.Wire]) — the same envelopes this node's desk
// answers with, built once. Nil on an engine assembled without a desk.
func (e *Engine) SteerWire() *signedask.Channel[steer.Request, steer.Reply] {
	if e.steers == nil {
		return nil
	}
	return e.steers.wire
}

// register files a first segment's box under its run id, with how the notes it
// took and nobody read are recorded expired.
func (d *steerDesk) register(runID string, seat steerSeat, box *steer.Box,
	expire func(context.Context, []steer.Note),
) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.gen++
	d.open[runID] = steerTurn{box: box, steerSeat: seat, expire: expire, gen: d.gen}
	delete(d.closed, runID)
}

// rememberLocked files an ended turn, bounded to [steerClosedMemory].
func (d *steerDesk) rememberLocked(runID string, t endedTurn) {
	if _, seen := d.closed[runID]; !seen {
		d.order = append(d.order, runID)
	}
	d.closed[runID] = t
	for len(d.order) > steerClosedMemory {
		delete(d.closed, d.order[0])
		d.order = d.order[1:]
	}
}

// close ends a first segment's box and remembers the turn, returning the notes
// it took and never read. A turn whose box was handed to its run's record stays
// remembered as handed: its notes are the record's, whose ending reports what
// no job ran with.
func (d *steerDesk) close(runID string) []steer.Note {
	d.mu.Lock()
	t, ok := d.open[runID]
	if !ok {
		d.mu.Unlock()
		return nil
	}
	delete(d.open, runID)
	d.rememberLocked(runID, endedTurn{steerSeat: t.steerSeat})
	d.mu.Unlock()
	// Closed OUTSIDE the desk's lock: an offer racing the close holds the
	// box's lock, and the box's answer settles the race.
	return t.box.Close()
}

// remember keeps a turn a segment resuming it from its run has just ended, so
// a note to it is answered `closed` for a while: the run's record is deleted
// with the run, and a later note reaches the scatter, which only a node
// remembering the turn answers.
func (d *steerDesk) remember(runID string, seat steerSeat) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, running := d.open[runID]; running {
		return
	}
	d.rememberLocked(runID, endedTurn{steerSeat: seat})
}

// handOver is one launch's hand-over of a first segment's box to the run record
// the launch writes — see [steerDesk.handOver].
type handOver struct {
	desk  *steerDesk
	runID string
	turn  steerTurn
	notes []steer.Note
	once  sync.Once
}

// handOver begins handing a first segment's box to the record its launch is
// about to write: the box's unread notes ride the write that creates it
// ([sandbox.LaunchRequest.HandedNotes]), so no note filed on the record lands
// ahead of one the box took first, or spends bounds those were counted against.
// The launch then calls exactly one of [handOver.recorded] or
// [handOver.unrecorded] on every path, and an offer to the turn waits until it
// does. Nil, settling nothing, for a turn with no box here: a resumed segment.
//
// The turn is marked HANDING under the desk's lock BEFORE the box's intake ends
// ([steer.Box.HandOver]), so an offer the closing box answered `closed` looks
// again ([steerDesk.offer]) and is never told a running turn has ended.
func (d *steerDesk) handOver(runID string) *handOver {
	if d == nil || d.runs == nil {
		return nil
	}
	d.mu.Lock()
	t, ok := d.open[runID]
	if !ok || t.handing != nil {
		d.mu.Unlock()
		return nil
	}
	d.gen++
	t.handing, t.gen = make(chan struct{}), d.gen
	d.open[runID] = t
	d.mu.Unlock()
	return &handOver{desk: d, runID: runID, turn: t, notes: t.box.HandOver()}
}

// held is the notes the box held unread as the hand-over began, oldest first.
func (h *handOver) held() []steer.Note {
	if h == nil {
		return nil
	}
	return h.notes
}

// recorded settles the hand-over as LANDED: the record holds the box's notes,
// the turn is remembered as handed so every later note is filed there, and the
// empty box is closed.
func (h *handOver) recorded() {
	if h == nil {
		return
	}
	h.once.Do(func() {
		d := h.desk
		d.mu.Lock()
		if t, ok := d.open[h.runID]; ok && t.handing == h.turn.handing {
			delete(d.open, h.runID)
			d.rememberLocked(h.runID, endedTurn{steerSeat: h.turn.steerSeat, handed: true})
		}
		d.mu.Unlock()
		h.turn.box.Close()
		close(h.turn.handing)
	})
}

// unrecorded settles the hand-over as NEVER WRITTEN: the launch failed before
// the record's write, so the notes go back to the front of the box
// ([steer.Box.Reopen]) and the turn is the box's again. A box closed meanwhile
// reports them expired here.
func (h *handOver) unrecorded(ctx context.Context) {
	if h == nil {
		return
	}
	h.once.Do(func() {
		lost := h.turn.box.Reopen(h.notes)
		d := h.desk
		d.mu.Lock()
		if t, ok := d.open[h.runID]; ok && t.handing == h.turn.handing {
			d.gen++
			t.handing, t.gen = nil, d.gen
			d.open[h.runID] = t
		}
		d.mu.Unlock()
		close(h.turn.handing)
		if len(lost) > 0 && h.turn.expire != nil {
			h.turn.expire(context.WithoutCancel(ctx), lost)
		}
	})
}

// deskEntry is what the desk holds for a turn.
type deskEntry struct {
	open    *steerTurn
	ended   *endedTurn
	present bool
}

// find is the desk's entry for a turn.
func (d *steerDesk) find(turnID string) deskEntry {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, running := d.open[turnID]; running {
		return deskEntry{open: &t, present: true}
	}
	if t, ended := d.closed[turnID]; ended {
		return deskEntry{ended: &t, present: true}
	}
	return deskEntry{}
}

// offer hands a note to the turn it names, if this node runs or remembers it:
// into its box, or onto its run's record where the box was handed there; while
// a launch is handing the box over, once the launch has said which.
func (d *steerDesk) offer(ctx context.Context, turnID string, n steer.Note) (steer.Status, steer.Reads, steerSeat, error) {
	for {
		entry := d.find(turnID)
		switch {
		case !entry.present:
			return "", "", steerSeat{}, errNotThisNode
		case entry.open != nil && entry.open.handing != nil:
			// Filed now, the note could land on the record ahead of the box's
			// notes, which the launch's write carries: it waits for the write.
			select {
			case <-entry.open.handing:
				continue
			case <-ctx.Done():
				return "", "", steerSeat{}, fmt.Errorf("engine: a note to turn %s waited for its "+
					"run's record to be written: %w", turnID, ctx.Err())
			}
		case entry.open != nil:
			status := entry.open.box.Offer(n)
			if status == steer.StatusClosed && d.changed(turnID, entry.open.gen) {
				// The box closed under the offer, handed over or ended: the
				// desk says which.
				continue
			}
			return status, steer.ReadsNextStep, entry.open.steerSeat, nil
		case entry.ended.handed && d.runs != nil:
			filed, err := d.runs.File(ctx, turnID, n)
			if err != nil {
				return "", "", steerSeat{}, fmt.Errorf("engine: filing a note on turn %s's run "+
					"record: %w", turnID, err)
			}
			return filed.Status, filed.Reads, entry.ended.steerSeat, nil
		default:
			return steer.StatusClosed, "", entry.ended.steerSeat, nil
		}
	}
}

// changed reports whether the desk's entry for a turn is no longer the open
// one of generation gen: the box an offer read changed hands under it.
func (d *steerDesk) changed(turnID string, gen uint64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	t, open := d.open[turnID]
	return !open || t.gen != gen
}

// probe answers what an offer to the turn would be answered, and whose turn it
// is, taking nothing ([steer.Request.Probe]). A turn being handed to its run's
// record, or handed, is answered accepted: a note to it is decided on the
// record from here, where the asker reads it next time.
func (d *steerDesk) probe(turnID string) (steer.Status, steerSeat, error) {
	entry := d.find(turnID)
	switch {
	case !entry.present:
		return "", steerSeat{}, errNotThisNode
	case entry.open != nil && entry.open.handing != nil:
		return steer.StatusAccepted, entry.open.steerSeat, nil
	case entry.open != nil:
		return entry.open.box.Status(), entry.open.steerSeat, nil
	case entry.ended.handed:
		return steer.StatusAccepted, entry.ended.steerSeat, nil
	default:
		return steer.StatusClosed, entry.ended.steerSeat, nil
	}
}

// answer is the serve handler: one scattered note, answered only by the node
// that runs or remembers its turn. The note is offered as the person it names,
// on the asking node's decision that they may steer the turn, so only what the
// fleet signed is taken, under an id not yet taken and a deadline ahead (the
// [steer.Channel] wire). Anything else is answered by nobody, like a turn not
// on this node, except a key this node does not hold ([steerDesk.unverified]).
func (d *steerDesk) answer(ctx context.Context, raw []byte) ([]byte, error) {
	admitted := d.wire.Admit(ctx, raw, d.taken, d.now())
	switch admitted.Disposition {
	case signedask.Tampered:
		// Logged at ERROR: no node will ever take it, so its sender reached
		// the broker without being the fleet.
		log.ErrorContext(ctx, "steer_request_refused", "disposition", string(admitted.Disposition),
			"detail", admitted.Why+"; nothing was offered")
		return nil, errors.New("engine: a steer request the fleet did not sign")
	case signedask.KeyUnknown:
		return d.unverified(ctx, admitted)
	case signedask.Undecodable:
		return nil, fmt.Errorf("engine: a steer request this build cannot read: %s", admitted.Why)
	case signedask.Stale, signedask.Unrecorded:
		log.WarnContext(ctx, "steer_request_refused", "disposition", string(admitted.Disposition),
			"turn_id", admitted.Request.TurnID, "detail", admitted.Why)
		return nil, fmt.Errorf("engine: a steer request not to be taken now: %s", admitted.Why)
	}
	req := admitted.Request
	turnID := strings.TrimSpace(req.TurnID)
	if turnID == "" {
		return nil, errors.New("engine: a steer request names no turn")
	}
	var (
		status steer.Status
		reads  steer.Reads
		seat   steerSeat
		err    error
	)
	if req.Probe {
		// A PROBE OFFERS NOTHING, so there is no note to bound.
		status, seat, err = d.probe(turnID)
	} else {
		// The tool's rule, held again where the note lands: a peer on
		// another build may bound a note differently, and the bound
		// protects the turn.
		if err = steer.Validate(req.Note); err != nil {
			return nil, fmt.Errorf("engine: a steer request for %s: %w", turnID, err)
		}
		status, reads, seat, err = d.offer(ctx, turnID, steer.Note{
			ID: req.NoteID, Text: strings.TrimSpace(req.Note),
			By: req.By, ByKind: req.ByKind, OperatorID: req.OperatorID,
		})
	}
	if err != nil {
		return nil, err
	}
	return d.wire.SealReply(steer.Reply{
		Version: steer.WireVersion, Request: req.ID, TurnID: turnID,
		Agent: seat.agent, AgentHandle: seat.handle, Status: status, Reads: reads,
	})
}

// unverified answers a request signed under a key this node does not hold, for
// a turn this node runs or remembers (read off the UNTRUSTED body for that
// alone): it says why nothing was taken, bound to the asker's id, so the person
// is told the keyring disagrees rather than read `unknown`. Every other node
// stays silent, as for any note not its own.
func (d *steerDesk) unverified(ctx context.Context, admitted signedask.Admission[steer.Request]) ([]byte, error) {
	turnID := strings.TrimSpace(admitted.Request.TurnID)
	if turnID == "" || admitted.ID == "" {
		return nil, errors.New("engine: a steer request under an unheld key that names nothing")
	}
	if !d.find(turnID).present {
		return nil, errNotThisNode
	}
	// The remedy is the operator's, so it is said in this node's log; the
	// person steering is told only that the note was not taken.
	log.WarnContext(ctx, "steer_request_refused", "disposition", string(admitted.Disposition),
		"turn_id", turnID, "held_keys", d.wire.HeldKeys(),
		"detail", admitted.Why+"; nothing was offered. Add the key the fleet "+
			"signs with to this node's secrets.keys")
	return d.wire.SealReply(steer.Reply{
		Version: steer.WireVersion, Request: admitted.ID, TurnID: turnID,
		Unverified: &steer.Unverified{Node: d.node, HeldKeys: d.wire.HeldKeys()},
	})
}

// armSteer makes this node an answerer for notes to the turns it runs.
func (e *Engine) armSteer(ctx context.Context) error {
	if e.backends == nil || e.backends.Queue == nil {
		// No broker, no asker: even this node's own operator surface asks
		// over the queue.
		return nil
	}
	stop, err := e.backends.Queue.Serve(ctx, topics.SeatSteer, queue.ConcurrentAnswers, e.steers.answer)
	if err != nil {
		return err
	}
	e.stopSteerServe = stop
	return nil
}

// stopSteer withdraws this node as an answerer, before the broker closes.
func (e *Engine) stopSteer(ctx context.Context) {
	if e.stopSteerServe == nil {
		return
	}
	if err := e.stopSteerServe(context.WithoutCancel(ctx)); err != nil {
		log.WarnContext(ctx, "steer_answerer_not_withdrawn", "error", err)
	}
	e.stopSteerServe = nil
}

// openSteer files a first segment's box, and returns what the turn calls on
// every path out: close the box and record expired every note it took and
// nobody read. (A read note is recorded `delivered` where it was read; one
// waiting on a run's record is expired by the run's ending, runsteer.go.)
//
// Filed only once the runner exists, so a turn whose runner could not be built
// never answers a note it could not have read. A segment that resumes a turn
// from its executor files no box: its notes are on the run's record.
func (e *Engine) openSteer(runID, handle string, box *steer.Box,
	r *runner.Runner,
) func(context.Context) {
	if e.steers == nil {
		// No desk (a test driving a turn directly): answer no note, but
		// still close what was opened.
		return func(ctx context.Context) {
			if expired := box.Close(); len(expired) > 0 {
				r.SteerExpired(context.WithoutCancel(ctx), expired)
			}
		}
	}
	// Who may steer the turn is decided on the seat's id. A handle that
	// resolves to no agent seat files an empty id, which a decision reads
	// as a seat it cannot name.
	seat := steerSeat{handle: handle}
	if id, err := e.seatID(handle); err == nil {
		seat.agent = id.String()
	}
	e.steers.register(runID, seat, box, r.SteerExpired)
	return func(ctx context.Context) {
		if expired := e.steers.close(runID); len(expired) > 0 {
			r.SteerExpired(context.WithoutCancel(ctx), expired)
		}
	}
}
