package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/host"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// boxRuns is Gitlet's [host.Runs]: the executors' rows as a box's admission
// reads them, from the sandbox's pending store on the FLEET'S coordination
// store. Built on every node that serves git, sandbox runtime or not: any node
// may have launched a box's run, so the Gitlet node a box dials judges it by
// the same row the launching node wrote.
type boxRuns struct {
	pending sandbox.PendingStore
	// stopped is the engine's one reading of a seat's stop (stoppedBy).
	stopped func(ctx context.Context, seat string) (by string, err error)
	log     *slog.Logger
}

var _ host.Runs = boxRuns{}

// codeBoxRuns is Gitlet's view of this engine's executors: their rows on the
// fleet's coordination store, and a seat's stop (stoppedBy) off the same pause
// record a launch's bridge fence reads (Engine.seatFence), so a box's pushes
// and its run's bridged calls are refused, and let through again, together.
func (e *Engine) codeBoxRuns(log *slog.Logger) boxRuns {
	return boxRuns{pending: sandbox.NewCoordStore(e.backends.Fleet), stopped: e.stoppedBy, log: log}
}

// Run is the row of the run whose turn is turnID, as the host judges a box by
// it: an error is the store unreachable, never "no run". Every request a box
// sends reads it, a blobless clone's lazy blob fetch included, so it moves the
// row alone (a few kilobytes), never the job's bridged-call log kept beside it
// (sandbox.BridgeLog).
//
// LIVE is a row whose job is in its box: LAUNCHING (a job's first calls, its
// clones among them, can precede the launch marking it suspended) or RUNNING,
// with no ending decided. A row parked on a question (awaiting, reseed,
// answered) or resumed has no job in its box: a parked box is paused, and a
// resumed job has ended while its collection reads what it pushed.
func (b boxRuns) Run(ctx context.Context, turnID string) (host.BoxRun, bool, error) {
	run, found, err := b.pending.Get(ctx, turnID)
	if err != nil || !found {
		return host.BoxRun{}, false, err
	}
	out := host.BoxRun{
		Ref:        host.BoxRunRef{TurnID: run.TurnID, LaunchID: run.LaunchID},
		Live:       boxLive(run),
		Status:     run.Status,
		Seat:       run.AgentHandle,
		Ceiling:    run.CodeCeiling,
		WorkingFor: run.CodeWorkingFor,
		Floor:      b.floorOf(run),
		Provenance: gitlet.RunProvenance{TurnID: run.TurnID, RunID: run.LaunchID, Model: run.Launch.Model,
			Harness: run.Harness},
		Holds: make(map[string]bool, len(run.CodeHolds)),
	}
	for _, key := range run.CodeHolds {
		out.Holds[key] = true
	}
	if run.WorkItem != nil {
		// The item's id and project beside its label: a change the box's push
		// opens carries its item by them (gitlet.RunProvenance.WorkID,
		// .WorkProject), as code_open_change and the collection's open do.
		out.Provenance.WorkBackend, out.Provenance.WorkKey = string(run.WorkItem.Backend), run.WorkItem.Key
		out.Provenance.WorkID, out.Provenance.WorkProject = run.WorkItem.ID, run.WorkItem.Project
	}
	for _, r := range run.Repos {
		out.Repos = append(out.Repos, host.BoxGrant{RepoID: r.RepoID, Name: r.Repo,
			Access: boxAccess(r.Access), Branch: "refs/heads/" + r.Branch})
	}
	return out, true, nil
}

// boxLive is whether run's job is in its box (Run's LIVE).
func boxLive(run sandbox.PendingRun) bool {
	return run.Ending == nil && slices.Contains([]string{sandbox.StatusLaunching, sandbox.StatusRunning}, run.Status)
}

// boxAccess is what a box may do with a repository its run was given: what
// the launch recorded, or READ for a value a newer build wrote that this one
// does not know — an unreadable grant is held to the narrow direction.
func boxAccess(a sandbox.RepoAccess) gitlet.Access {
	if a == sandbox.RepoWrite {
		return gitlet.AccessWrite
	}
	return gitlet.AccessRead
}

// floorOf is run's code floor as a position on the code log. No floor, an
// unreadable token or another log's position all read ZERO, which reads
// LINEARIZABLE, the safe direction (the box's next ref answer raises it); the
// last two log WARN code_box_floor_unreadable, as only a writer's bug or a
// newer build's spelling puts one there.
func (b boxRuns) floorOf(run sandbox.PendingRun) statelog.Position {
	if run.CodeFloor == "" {
		return statelog.Position{}
	}
	pos, err := statelog.ParsePosition(run.CodeFloor)
	if err == nil && pos.Stream != topics.CodeLogStream {
		err = errForeignFloor
	}
	if err != nil {
		b.log.Warn("code_box_floor_unreadable", "turn_id", run.TurnID, "launch_id", run.LaunchID,
			"floor", run.CodeFloor, "error", err)
		return statelog.Position{}
	}
	return pos
}

// RaiseFloor raises the run's code floor to pos while ref's launch is the
// row's, in a compare-and-set (sandbox.PendingStore's RaiseCodeFloor) that
// writes only where pos is past the current floor; an unreadable floor is
// raised over, as Run reads it as none.
func (b boxRuns) RaiseFloor(ctx context.Context, ref host.BoxRunRef, pos statelog.Position) error {
	if pos.Stream != topics.CodeLogStream {
		return errForeignFloor
	}
	_, err := b.pending.RaiseCodeFloor(ctx, ref.TurnID, ref.LaunchID, func(current string) (string, bool) {
		if cur, perr := statelog.ParsePosition(current); perr == nil && holds(cur, pos) {
			return "", false
		}
		return pos.String(), true
	})
	return err
}

// holds reports whether floor is at or past pos, on pos's log.
func holds(floor, pos statelog.Position) bool {
	before, err := floor.Before(pos)
	return err == nil && !before
}

// Stopped is who stopped seat, as this node's pause watch holds it (stoppedBy).
func (b boxRuns) Stopped(ctx context.Context, seat string) (string, error) {
	return b.stopped(ctx, seat)
}

// Hold joins keys (the seat branches a box's push names) to the code holds on
// ref's run's row in one compare-and-set (sandbox.PendingStore's AddCodeHolds):
// the set its turn's holds are written to and Gitlet's retention reads, keyed
// by the TURN alone, since the holds are the turn's across its launches. A row
// gone or with its ending decided holds nothing new and is NO ERROR: a box's
// push can outlive what its run will read again, unlike a turn's own request,
// which holdWriter refuses there. sandbox.ErrCodeHoldsFull is the row at its
// bound, nothing written; any other error is the store unreachable.
func (b boxRuns) Hold(ctx context.Context, ref host.BoxRunRef, keys []string) error {
	if _, err := b.pending.AddCodeHolds(ctx, ref.TurnID, keys); err != nil {
		return fmt.Errorf("engine: hold %d seat branches on run %s: %w", len(keys), ref.TurnID, err)
	}
	return nil
}

// errForeignFloor is a code floor on another log than the code log's.
var errForeignFloor = errors.New("engine: an executor's code floor names a position on another log than " +
	"the code log")

// stoppedBy is Gitlet's reading of a seat's STOP: who paused the seat and
// asked its running work to stop (coord.SeatPause's By, ADR-0032), off this
// node's pause watch, which every node runs from boot. "" is a seat not
// paused, paused without stop_running, or a handle naming no agent seat
// (pauseOfHandle); errPausesUnread before the watch's first complete answer
// is UNKNOWN, never "not stopped", since a Gitlet node serves boxes from boot.
//
// Gitlet refuses a box's push on it (host.Runs.Stopped), or a stopped seat's
// box would push on: a box's git is not a bridged call, so the bridge fence
// (mcpbridge.Session.Fence, Engine.seatFence, via stopFor) never sees it. One
// record and one StopRunning stop both edges, and let them through, together;
// only the fence never meets an unread watch (no turn runs before screening).
func (e *Engine) stoppedBy(_ context.Context, handle string) (string, error) {
	seat, err := e.seatID(handle)
	if err != nil {
		if _, known := e.pauseOfHandle(handle); !known {
			return "", errPausesUnread
		}
		return "", nil
	}
	p, paused, known := e.pauseOf(seat)
	switch {
	case !known:
		return "", errPausesUnread
	case !paused || !p.StopRunning:
		return "", nil
	}
	return p.By, nil
}
