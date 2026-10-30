package engine

import (
	"context"
	"errors"
	"log/slog"
	"slices"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// boxRuns is the code host's [host.Runs] — the coding runs' rows as a box's
// admission reads them — over the sandbox's pending store on the FLEET'S
// coordination store.
//
// BUILT ON EVERY NODE THAT SERVES GIT, whether or not it runs a sandbox
// runtime of its own: a box's run may have been launched by any node, and its
// row is readable from every one (sandbox.NewCoordStore over the fleet's
// backend, the store the sandbox runtime builds over), so the git node a box
// dials judges it by the same row the launching node wrote.
type boxRuns struct {
	pending sandbox.PendingStore
	// stopped is the engine's one reading of a seat's stop (stoppedBy).
	stopped func(ctx context.Context, seat string) (by string, err error)
	log     *slog.Logger
}

var _ host.Runs = boxRuns{}

// codeBoxRuns is the code host's view of this engine's coding runs: their rows
// on the fleet's coordination store, and a seat's stop as this node reads it
// (stoppedBy) — the one reading every bridge session a launch opens carries
// too (agentLauncher.bridgeSession), so a box's pushes and its run's bridged
// writes are refused, and let through again, together.
func (e *Engine) codeBoxRuns(log *slog.Logger) boxRuns {
	return boxRuns{pending: sandbox.NewCoordStore(e.backends.Fleet), stopped: e.stoppedBy, log: log}
}

// Run is the row of the run whose turn is turnID, as the host judges a box by
// it: an error is the store unreachable, never "no run".
//
// LIVE is a row whose job is in its box: LAUNCHING — the clone steps run
// during the launch, before the job is marked suspended — or RUNNING, with no
// ending decided. A row parked on a question (awaiting, reseed, answered) or
// resumed has no job in its box: a parked job's box is paused, and a resumed
// one's job has ended while its collection reads what it pushed.
func (b boxRuns) Run(ctx context.Context, turnID string) (host.BoxRun, bool, error) {
	run, found, err := b.pending.Get(ctx, turnID)
	if err != nil || !found {
		return host.BoxRun{}, false, err
	}
	out := host.BoxRun{
		Ref:    host.BoxRunRef{TurnID: run.TurnID, LaunchID: run.LaunchID},
		Live:   boxLive(run),
		Status: run.Status,
		Seat:   run.AgentHandle,
		Floor:  b.floorOf(run),
		Provenance: code.RunProvenance{TurnID: run.TurnID, RunID: run.LaunchID, Model: run.Launch.Model,
			CodingAgent: run.CodingAgent},
	}
	if run.WorkItem != nil {
		out.Provenance.WorkBackend, out.Provenance.WorkKey = string(run.WorkItem.Backend), run.WorkItem.Key
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
// the launch recorded, and — for a value a newer build wrote that this one
// does not know — READ, never more: a grant this build cannot read is held
// to the narrow direction, as an unknown tier caps at nothing.
func boxAccess(a sandbox.RepoAccess) code.Access {
	if a == sandbox.RepoWrite {
		return code.AccessWrite
	}
	return code.AccessRead
}

// floorOf is run's code floor as a position on the code log: ZERO for a row
// that names none, and for one whose token this build cannot read or that
// names another log — a zero floor reads LINEARIZABLE, the safe direction,
// and the box's next ref answer raises the row to a readable one — logged
// WARN code_box_floor_unreadable, since only a writer's bug or a newer
// build's spelling puts one there.
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

// RaiseFloor raises the run's code floor to pos, and only while ref's launch
// is the row's: a compare-and-set on the row (sandbox.PendingStore's
// RaiseCodeFloor), writing only where pos is past the floor as it stands — a
// floor this build cannot read is raised over, as Run reads it as none.
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

// errForeignFloor is a code floor on another log than the code log's.
var errForeignFloor = errors.New("engine: a coding run's code floor names a position on another log than " +
	"the code log")

// stoppedBy is the engine's ONE reading of a seat's STOP — who paused the
// seat and asked its running work to stop (coord.SeatPause's By, the person
// a bound credential pauses as, ADR-0032) — read off this node's pause watch:
// "" for a seat not paused, paused without stop_running, or a handle the
// running company resolves to no agent seat (as pauseOfHandle answers it),
// and errPausesUnread before the watch's first complete answer, UNKNOWN and
// never "not stopped". Every node runs the watch from boot, so a git node
// that serves no seat answers too.
//
// The code host refuses a coding box's push on it (host.Runs.Stopped), and
// the run's bridge session its bridged calls of every tool not proven
// read-only (mcpbridge.Session.Stopped, set by agentLauncher.bridgeSession):
// a detached coding run is deliberately not fenced by a pause — it outlives
// its turn — so without them a box went on pushing to the company's
// repositories, and calling the seat's tools that write, as the seat after a
// person stopped the seat "because it was doing damage". ONE function for
// both edges, so the two are stopped, and let through again, together.
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
