package engine

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// rowsOnly is a pending store holding the rows a case puts, answering Get and
// RaiseCodeFloor as the coordination store's does — the two calls boxRuns
// makes — and nothing else.
type rowsOnly struct {
	sandbox.PendingStore
	rows map[string]sandbox.PendingRun
}

func (s *rowsOnly) Get(_ context.Context, turnID string) (sandbox.PendingRun, bool, error) {
	run, ok := s.rows[turnID]
	return run, ok, nil
}

func (s *rowsOnly) RaiseCodeFloor(_ context.Context, turnID, launchID string,
	raise func(current string) (string, bool)) (bool, error) {
	run, ok := s.rows[turnID]
	if !ok || run.LaunchID != launchID || run.Ending != nil {
		return false, nil
	}
	next, write := raise(run.CodeFloor)
	if !write || next == run.CodeFloor {
		return false, nil
	}
	run.CodeFloor = next
	s.rows[turnID] = run
	return true, nil
}

// codeAt is a position on the code log.
func codeAt(seq uint64) statelog.Position {
	return statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: seq}
}

// A BOX'S RUN IS LIVE while its job is in its box: launching — the clone steps
// run during the launch — or running, with no ending decided; never a row
// parked on a question, answered, reseeded or resumed, whose box holds no job
// that may push. Its grants map to the code host's, an access this build does
// not know read as read and never more, and its pushes are stamped with the
// row's turn, launch, work item, model and coding agent.
//
// Mutation: count awaiting_clarification live — its row admits a box — red.
func TestBoxRunsJudgesLiveness(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status string
		ended  bool
		live   bool
	}{
		{sandbox.StatusLaunching, false, true},
		{sandbox.StatusRunning, false, true},
		{sandbox.StatusRunning, true, false},
		{sandbox.StatusAwaiting, false, false},
		{sandbox.StatusAnswered, false, false},
		{sandbox.StatusReseed, false, false},
		{sandbox.StatusResumed, false, false},
	} {
		turn, launch := uuid.Must(uuid.NewV7()).String(), uuid.NewString()
		row := sandbox.PendingRun{TurnID: turn, LaunchID: launch, Status: tc.status, AgentHandle: "alice",
			CodingAgent: "claude-code", Launch: sandbox.LaunchRecord{Model: "m"},
			WorkItem: &types.WorkItem{Backend: "native", Key: "ENG-1"},
			Repos: []sandbox.RunRepo{{Repo: "web", RepoID: uuid.NewString(), Branch: "seats/alice/x",
				Access: sandbox.RepoWrite}, {Repo: "api", RepoID: uuid.NewString(), Access: "admin"}}}
		if tc.ended {
			row.Ending = &sandbox.RecordedEnding{ID: "e"}
		}
		b := boxRuns{pending: &rowsOnly{rows: map[string]sandbox.PendingRun{turn: row}}, log: slog.Default()}
		got, found, err := b.Run(t.Context(), turn)
		if err != nil || !found {
			t.Fatalf("%s: found %v, %v", tc.status, found, err)
		}
		if got.Live != tc.live {
			t.Errorf("a %s row (ended %v) reads live %v, want %v", tc.status, tc.ended, got.Live, tc.live)
		}
		want := host.BoxRun{Ref: host.BoxRunRef{TurnID: turn, LaunchID: launch}, Live: tc.live, Status: tc.status,
			Seat: "alice", Provenance: code.RunProvenance{TurnID: turn, RunID: launch, WorkBackend: "native",
				WorkKey: "ENG-1", Model: "m", CodingAgent: "claude-code"},
			Repos: []host.BoxGrant{
				{RepoID: row.Repos[0].RepoID, Name: "web", Access: code.AccessWrite, Branch: "refs/heads/seats/alice/x"},
				{RepoID: row.Repos[1].RepoID, Name: "api", Access: code.AccessRead, Branch: "refs/heads/"},
			}}
		if got.Ref != want.Ref || got.Seat != want.Seat || got.Provenance != want.Provenance ||
			len(got.Repos) != 2 || got.Repos[0] != want.Repos[0] || got.Repos[1].Access != code.AccessRead {
			t.Errorf("a %s row reads %+v, want %+v", tc.status, got, want)
		}
	}
	if _, found, err := (boxRuns{pending: &rowsOnly{}, log: slog.Default()}).Run(t.Context(), "gone"); found ||
		err != nil {
		t.Errorf("a row nobody holds reads found %v, %v", found, err)
	}
}

// A SEAT'S STOP IS READ ONE WAY, BY THE SEAT'S ID: stoppedBy answers the
// person whose pause asked the seat's running work to stop — what the code
// host refuses a coding box's push by — "" for a seat not paused, for a pause
// without stop_running and for a handle the running company holds no agent
// seat for, and errPausesUnread on a node that has not read the pauses yet,
// never "not stopped".
//
// Mutations: answer "" before the watch's first answer — the unread node
// reads not stopped — red; answer a pause without stop_running — the paused
// seat reads stopped — red.
func TestStoppedByIsWhoAskedTheSeatsWorkToStop(t *testing.T) {
	t.Parallel()
	unread, _ := engineOn(t)
	if _, err := unread.stoppedBy(t.Context(), "ceo"); !errors.Is(err, errPausesUnread) {
		t.Errorf("stoppedBy before any answer = %v, want errPausesUnread", err)
	}
	e, _, fleet := pausingEngine(t)
	ctx := t.Context()
	if by, err := e.stoppedBy(ctx, "ceo"); err != nil || by != "" {
		t.Errorf("a seat not paused reads stopped by %q (%v)", by, err)
	}
	p := pauseSeat(t, fleet, ceoSeat, false)
	waitFor(t, "the copy to see the pause", func() bool {
		_, paused, _ := e.pauseOf(ceoSeat)
		return paused
	})
	if by, err := e.stoppedBy(ctx, "ceo"); err != nil || by != "" {
		t.Errorf("a pause without stop_running reads stopped by %q (%v)", by, err)
	}
	p.StopRunning = true
	if _, ok, err := fleet.UpdateSeatPause(ctx, p); err != nil || !ok {
		t.Fatalf("UpdateSeatPause = (%v, %v)", ok, err)
	}
	waitFor(t, "the copy to see the stop", func() bool {
		by, _ := e.stoppedBy(ctx, "ceo")
		return by != ""
	})
	if by, err := e.stoppedBy(ctx, "ceo"); err != nil || by != "jane" {
		t.Errorf("the stopped seat reads stopped by %q (%v), want jane", by, err)
	}
	if by, err := e.stoppedBy(ctx, "nobody-here"); err != nil || by != "" {
		t.Errorf("a handle the company holds no seat for reads stopped by %q (%v)", by, err)
	}
}

// A FLOOR THIS BUILD CANNOT READ IS NONE: a token that does not parse, or one
// naming a position on another log, reads as the zero floor — which a box
// reads past LINEARIZABLE, the safe direction — and the next raise writes a
// readable one over it; a raise never lowers a floor, and never writes for
// a superseded launch.
//
// Mutation: answer a floor on another log as it reads — the box's session
// read is refused for a foreign floor — red.
func TestBoxRunsReadsAnUnreadableFloorAsNone(t *testing.T) {
	t.Parallel()
	turn, launch := uuid.Must(uuid.NewV7()).String(), uuid.NewString()
	store := &rowsOnly{rows: map[string]sandbox.PendingRun{}}
	b := boxRuns{pending: store, log: slog.Default()}
	ref := host.BoxRunRef{TurnID: turn, LaunchID: launch}
	for _, floor := range []string{"not a position", "CREWLET_TRACKER_LOG@1:7"} {
		store.rows[turn] = sandbox.PendingRun{TurnID: turn, LaunchID: launch, Status: sandbox.StatusRunning,
			CodeFloor: floor}
		got, _, err := b.Run(t.Context(), turn)
		if err != nil || !got.Floor.IsZero() {
			t.Errorf("a floor %q reads %s (%v), want none", floor, got.Floor, err)
		}
		if err := b.RaiseFloor(t.Context(), ref, codeAt(5)); err != nil {
			t.Fatal(err)
		}
		if got := store.rows[turn].CodeFloor; got != codeAt(5).String() {
			t.Errorf("a raise over the floor %q wrote %q, want %s", floor, got, codeAt(5))
		}
	}
	if err := b.RaiseFloor(t.Context(), ref, codeAt(3)); err != nil {
		t.Fatal(err)
	}
	if got := store.rows[turn].CodeFloor; got != codeAt(5).String() {
		t.Errorf("a raise below the floor wrote %q", got)
	}
	if err := b.RaiseFloor(t.Context(), host.BoxRunRef{TurnID: turn, LaunchID: uuid.NewString()},
		codeAt(9)); err != nil {
		t.Fatal(err)
	}
	if got := store.rows[turn].CodeFloor; got != codeAt(5).String() {
		t.Errorf("a superseded launch's raise wrote %q", got)
	}
	if got, _, _ := b.Run(t.Context(), turn); got.Floor != codeAt(5) {
		t.Errorf("the raised floor reads %s, want %s", got.Floor, codeAt(5))
	}
}
