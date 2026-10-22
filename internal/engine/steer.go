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
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// A person's note to a running turn, carried out on the node running it.
//
// # One box per first segment, and only its node answers
//
// A turn that has not launched its coding run yet holds its notes in a
// [steer.Box], filed here under the turn's run id for as long as its first
// segment runs. Every node serves [topics.SeatSteer]; a note scattered there
// names a turn, and the one node whose desk holds that turn answers — every
// other node answers nothing, which the asker reads as a node the turn is not
// on. That is the whole routing: nothing looks up where a turn runs, because
// the turn's own node is the only one that can say.
//
// # From the launch on, the run's record holds the turn's notes
//
// Once the turn's coding run has launched, its notes wait on the run's record
// in the coordination store, which every node reads (runsteer.go): the asking
// node files a note there itself, and asks nobody. The launch HANDS THE BOX
// OVER IN THE WRITE THAT CREATES THE RECORD ([steerDesk.handOver]): the box's
// intake ends before that write, and every note it took and nobody read rides
// it ([sandbox.LaunchRequest.HandedNotes]), filed waiting after the brief's —
// so no note a person files on the record can land ahead of one the box took
// first, nor use up the bounds a note the box accepted was already counted
// against: every reader takes in the record's order, and a note is the later
// word. While that write is in flight the turn is HANDING, and an offer that
// reaches the desk waits for it; then the turn's entry here is kept as HANDED
// — so a note that read "no record" a moment before the launch wrote one, and
// was scattered here, is filed on the record by this node and answered as the
// record answers it, and a probe is answered with the turn's seat. A launch
// that fails before the write hands the box its notes back
// ([steer.Box.Reopen]), and the turn is the box's again. An offer is never
// answered `closed` for a turn that is running: one that met the box as its
// intake ended looks again and finds the turn handing, handed or the box's.
//
// # A turn that ended is still answered for a while
//
// A person steers a turn they saw running, and the turn may end in the second
// between their click and the note's arrival. Answered by nobody, that note
// reads as `unknown` — the broker lost it, try again — when the true answer is
// that the turn is over. So an ended turn's id is REMEMBERED, for the last
// [steerClosedMemory] turns this node ran — a first segment that closed its
// box, and a segment resuming the turn from its run that ended it
// ([steerDesk.remember]) — and a note for one is answered `closed`. Past that
// the node forgets it, and the asker says `unknown`, which is honest about what
// nobody could confirm.
//
// # Only a note the fleet signed is offered
//
// The note is offered to the turn as the person it names, on the asking
// node's decision that they may steer it — so the desk takes only what the
// fleet signed, bound to an id it has not taken and a deadline still ahead
// (internal/signedask, the [steer.Channel] wire). Anything else is answered by
// nobody, which the asker hears as it hears a node the turn is not on — except
// a request signed under a key this node does not hold for a turn it runs,
// which is answered [steer.Reply.Unverified], so the person is told the fleet's
// keyring disagrees rather than that nobody knows.
//
// # What becomes of a note is on the turn's record
//
// The node that took the request answers `pending` and can see nothing more.
// A note the turn reads is recorded `delivered` where it was read; one a first
// segment's box held when the segment ended without launching a run is recorded
// `expired` here, as the box is closed; and one that waits on a run's record
// when the turn ends is recorded expired by the run's ending (runsteer.go).

// steerClosedMemory is how many ended turns a node remembers, to answer a note
// that arrived just after its turn did `closed` rather than not at all — and how
// many turns whose box it handed to a run's record it files a late offer for.
//
// 256: a note races a turn's end by seconds, and a node running its seats at
// full tilt ends a turn every few seconds at most — so 256 is several minutes
// of even a busy node's turns, far past any race a click can lose, for a few
// kilobytes of ids. Count-bounded rather than timed, because a node's memory
// is what has to stay bounded and a count bounds it directly.
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

	// wire signs and verifies the steer channel, both halves — this node's
	// operator surface asks with it ([Engine.SteerWire]) and the desk
	// answers with it — and taken is the ids the desk has taken.
	wire  *steerWire
	taken *signedask.Guard
	now   func() time.Time

	// runs files a handed turn's notes on its run's record. Nil on a desk
	// built without one — a case driving the scatter alone — which hands
	// nothing over.
	runs *runSteers
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
// handed to its run's record rather than closed with the turn.
type endedTurn struct {
	steerSeat
	handed bool
}

// newSteerDesk builds this node's desk over the fleet keyring every state-log
// record is signed under. The keyring is required on every node, so this fails
// only on a bootstrap [config.Bootstrap.Validate] would already have refused.
func newSteerDesk(ring statelog.Keyring) (*steerDesk, error) {
	wire, err := signedask.New[steer.Request, steer.Reply](steer.Channel, ring)
	if err != nil {
		return nil, fmt.Errorf("engine: the steer channel's envelopes: %w", err)
	}
	return &steerDesk{open: map[string]steerTurn{}, closed: map[string]endedTurn{},
		wire: wire, taken: signedask.NewGuard(), now: time.Now}, nil
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
// remembered as handed: its notes are the record's, and its ending reports
// what no job ran with.
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
	// box's own lock, and the box's answer is what the race settles on.
	return t.box.Close()
}

// remember answers notes to a turn a segment resuming it from its run has just
// ended `closed`, for a while: the run's record is deleted with the run, and a
// note that arrives after that reaches the scatter, which only a node that
// remembers the turn answers.
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
// about to write: the turn is marked HANDING, the box's intake ended
// ([steer.Box.HandOver]), and the notes it took and nobody read are answered,
// for the launch to file in the write that creates the record
// ([sandbox.LaunchRequest.HandedNotes]). The launch then says whether that
// write landed — [handOver.recorded] or [handOver.unrecorded], exactly one, on
// every path — and until it does an offer to the turn waits. Nil, which hands
// nothing and settles nothing, for a turn this desk holds no box for: a segment
// that resumed the turn files none.
//
// HANDING BEFORE THE BOX'S INTAKE ENDS, under the desk's lock: an offer that
// found the box open and was answered `closed` by it looks again and finds the
// turn handing ([steerDesk.offer]), so no offer racing the launch is told the
// turn has ended — nor filed on a record ahead of the notes the box held.
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
// and the turn is remembered as handed — every later note is filed on the
// record. The box is closed: it holds nothing, and nothing will read it again.
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
// the write that would have created the record, so the box's notes are the
// box's again, at its front ([steer.Box.Reopen]), and the turn is the box's —
// read by whatever reads it next, or reported expired when the segment closes
// it. A box closed meanwhile reports them expired here.
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
// into its box, or onto its run's record where its box was handed there — and,
// while a launch is handing the box over, once the launch has said which.
func (d *steerDesk) offer(ctx context.Context, turnID string, n steer.Note) (steer.Status, steer.Reads, steerSeat, error) {
	for {
		entry := d.find(turnID)
		switch {
		case !entry.present:
			return "", "", steerSeat{}, errNotThisNode
		case entry.open != nil && entry.open.handing != nil:
			// THE BOX IS BEING HANDED to the record its launch is writing:
			// filed now, the note could land ahead of the ones the box held,
			// which that write carries. It waits for the write.
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
				// THE BOX CLOSED UNDER THE OFFER: its turn is being handed
				// to the run's record, or ended. The desk says which.
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
// is, taking nothing — see [steer.Request.Probe]. A turn being handed to its
// run's record, or handed there, is answered accepted: a person's note to it is
// decided on the record from here, and the probe's asker reads it there next
// time.
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
// that runs or remembers its turn — and only where the fleet signed it.
func (d *steerDesk) answer(ctx context.Context, raw []byte) ([]byte, error) {
	admitted := d.wire.Admit(raw, d.taken, d.now())
	switch admitted.Disposition {
	case signedask.Tampered:
		// NOT THE FLEET'S: answered by nobody, and at ERROR — no node will
		// ever take it, so whoever sent it reached the broker and is not
		// the fleet.
		log.ErrorContext(ctx, "steer_request_refused", "disposition", string(admitted.Disposition),
			"detail", admitted.Why+"; nothing was offered")
		return nil, errors.New("engine: a steer request the fleet did not sign")
	case signedask.KeyUnknown:
		return d.unverified(ctx, admitted)
	case signedask.Undecodable:
		return nil, fmt.Errorf("engine: a steer request this build cannot read: %s", admitted.Why)
	case signedask.Stale:
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
		// THE SAME RULE THE TOOL ENFORCED, held again where the note
		// lands: a peer on another build may bound a note differently,
		// and the turn is what the bound protects.
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

// unverified answers a request signed under a key this node does not hold —
// but only for a turn this node runs or remembers, read off the UNTRUSTED body
// for that alone: every other node stays silent, as it does for any note not
// its own, and the one the turn is on says why it took nothing, bound to the
// id the asker sent, rather than leave the person reading `unknown`.
func (d *steerDesk) unverified(ctx context.Context, admitted signedask.Admission[steer.Request]) ([]byte, error) {
	turnID := strings.TrimSpace(admitted.Request.TurnID)
	if turnID == "" || admitted.ID == "" {
		return nil, errors.New("engine: a steer request under an unheld key that names nothing")
	}
	if !d.find(turnID).present {
		return nil, errNotThisNode
	}
	log.WarnContext(ctx, "steer_request_refused", "disposition", string(admitted.Disposition),
		"turn_id", turnID, "detail", admitted.Why+"; nothing was offered")
	return d.wire.SealReply(steer.Reply{
		Version: steer.WireVersion, Request: admitted.ID, TurnID: turnID,
		Unverified: fmt.Sprintf("the node running turn %s does not hold the key the "+
			"note is signed under (it holds %s) — add the key to its secrets.keys",
			turnID, words.And(d.wire.HeldKeys())),
	})
}

// armSteer makes this node an answerer for notes to the turns it runs.
func (e *Engine) armSteer(ctx context.Context) error {
	if e.backends == nil || e.backends.Queue == nil {
		// A node with no broker has no peer to be asked by: a note to its
		// turns can only come from its own operator surface, which asks
		// the same queue and so cannot exist either.
		return nil
	}
	stop, err := e.backends.Queue.Serve(ctx, topics.SeatSteer, e.steers.answer)
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
// every path out: close the box and record every note it took and never read.
//
// Filed only once the runner exists, so a turn whose runner could not be built
// never answers a note it could not have read. A segment that resumes a turn
// from its coding run files no box: its notes are on the run's record.
func (e *Engine) openSteer(runID, handle string, box *steer.Box,
	r *runner.Runner,
) func(context.Context) {
	if e.steers == nil {
		// An engine assembled without a desk (a test driving a turn
		// directly) answers no note, and still closes what it opened.
		return func(ctx context.Context) {
			if expired := box.Close(); len(expired) > 0 {
				r.SteerExpired(context.WithoutCancel(ctx), expired)
			}
		}
	}
	// THE SEAT BY ITS ID beside its handle: who may steer the turn is
	// decided on the seat. A handle the running company resolves to no
	// agent seat files an empty id, which a decision reads as a seat it
	// cannot name.
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
