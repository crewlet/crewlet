package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// A person's note to a running turn, carried out on the node running it.
//
// # One box per turn, and only its node answers
//
// Every turn this node runs opens a [steer.Box] for as long as it runs, filed
// here under the turn's run id. Every node serves [topics.SeatSteer]; a note
// scattered there names a turn, and the one node whose desk holds that turn
// answers — every other node answers nothing, which the asker reads as a node
// the turn is not on. That is the whole routing: nothing looks up where a turn
// runs, because the turn's own node is the only one that can say.
//
// # A turn that ended is still answered for a while
//
// A person steers a turn they saw running, and the turn may end in the second
// between their click and the note's arrival. Answered by nobody, that note
// reads as `unknown` — the broker lost it, try again — when the true answer is
// that the turn is over. So a closed turn's id is REMEMBERED, for the last
// [steerClosedMemory] turns this node ran, and a note for one is answered
// `closed`. Past that the node forgets it, and the asker says `unknown`, which
// is honest about what nobody could confirm.
//
// # What becomes of a note is on the turn's record
//
// The node that took the request answers `pending` and can see nothing more.
// A note the turn reads is recorded `delivered` by the runner at the round
// that read it; one it never reads is recorded `expired` here, when the turn's
// box is closed — on every path out of the turn, a park included, because a
// parked turn resumes as a new segment with a new box and its next round is
// not the one the note was sent to.

// steerClosedMemory is how many ended turns a node remembers, to answer a note
// that arrived just after its turn did `closed` rather than not at all.
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

// steerDesk is this node's open turns' note boxes, and the ended turns it
// still answers for.
type steerDesk struct {
	mu     sync.Mutex
	open   map[string]steerTurn
	closed map[string]string
	order  []string
}

type steerTurn struct {
	box    *steer.Box
	handle string
}

func newSteerDesk() *steerDesk {
	return &steerDesk{open: map[string]steerTurn{}, closed: map[string]string{}}
}

// open files a turn's box under its run id. A box whose runtime cannot read a
// note is filed too, so a note for it is answered `unsupported` rather than
// not at all.
func (d *steerDesk) register(runID, handle string, box *steer.Box) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.open[runID] = steerTurn{box: box, handle: handle}
	// A resumed segment reuses its run id: it is running again, not ended.
	delete(d.closed, runID)
}

// close ends a turn's box and remembers the turn, returning the notes it took
// and never read.
func (d *steerDesk) close(runID string) []steer.Note {
	d.mu.Lock()
	t, ok := d.open[runID]
	if !ok {
		d.mu.Unlock()
		return nil
	}
	delete(d.open, runID)
	if _, seen := d.closed[runID]; !seen {
		d.order = append(d.order, runID)
	}
	d.closed[runID] = t.handle
	for len(d.order) > steerClosedMemory {
		delete(d.closed, d.order[0])
		d.order = d.order[1:]
	}
	d.mu.Unlock()
	// Closed OUTSIDE the desk's lock: an offer racing the close holds the
	// box's own lock, and the box's answer is what the race settles on.
	return t.box.Close()
}

// offer hands a note to the turn it names, if this node runs or remembers it.
func (d *steerDesk) offer(turnID string, n steer.Note) (steer.Status, string, error) {
	d.mu.Lock()
	t, running := d.open[turnID]
	handle, ended := d.closed[turnID]
	d.mu.Unlock()
	switch {
	case running:
		return t.box.Offer(n), t.handle, nil
	case ended:
		return steer.StatusClosed, handle, nil
	}
	return "", "", errNotThisNode
}

// answer is the serve handler: one scattered note, answered only by the node
// that runs or remembers its turn.
func (d *steerDesk) answer(_ context.Context, raw []byte) ([]byte, error) {
	var req steer.Request
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("engine: a steer request this build cannot read: %w", err)
	}
	turnID := strings.TrimSpace(req.TurnID)
	if turnID == "" {
		return nil, errors.New("engine: a steer request names no turn")
	}
	// THE SAME RULE THE TOOL ENFORCED, held again where the note lands: a
	// peer on another build may bound a note differently, and the turn is
	// what the bound protects.
	if err := steer.Validate(req.Note); err != nil {
		return nil, fmt.Errorf("engine: a steer request for %s: %w", turnID, err)
	}
	status, handle, err := d.offer(turnID, steer.Note{
		ID: req.NoteID, Text: strings.TrimSpace(req.Note), By: req.By, BySeat: req.BySeat,
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(steer.Reply{
		Version: steer.WireVersion, TurnID: turnID, AgentHandle: handle, Status: status,
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

// steerBox opens the box a turn on handle runs with: one that takes notes when
// the executor is the engine's own loop, and one that answers every note
// `unsupported` when the executor runs as a coding CLI's own agentic loop —
// its rounds are the CLI's, and no round boundary is the engine's to reach.
func steerBox(agentRun runner.AgentLauncher) *steer.Box {
	if agentRun != nil {
		return steer.Unsupported()
	}
	return steer.New()
}

// openSteer files a built turn's box, and returns what the turn calls on every
// path out: close the box and record every note it took and never read.
//
// Filed only once the runner exists, so a turn whose runner could not be built
// never answers a note it could not have read.
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
	e.steers.register(runID, handle, box)
	return func(ctx context.Context) {
		if expired := e.steers.close(runID); len(expired) > 0 {
			r.SteerExpired(context.WithoutCancel(ctx), expired)
		}
	}
}
