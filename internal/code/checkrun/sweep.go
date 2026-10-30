package checkrun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// WorkerCheckRunners is the home's sweep's name as internal/code/home's loop
// registers it, and as crewlet.code.home.passes counts its turns.
const WorkerCheckRunners = "check_runners"

// SweepPolls is how many polls past its check's own timeout and a setup's
// grace a running attempt's claim may age before the home takes its runner for
// lost: FOUR, a minute at CheckPoll — the poll at which its runner finds the
// command past its timeout, FinishPolls polls of asking for its ending again,
// and one more for the clocks of two nodes and a loaded one's lateness. A
// runner holding an attempt ends it by its claim, the setup's grace, its
// timeout and that much, or it is not running it: a node restarted mid-run,
// in the git order again but holding nothing.
const SweepPolls = 4

// SweeperOptions build a [Sweeper]. Every field but Logger and Now is
// required.
type SweeperOptions struct {
	// Estate is the code log, read and written through the router.
	Estate Estate
	// Order is the repositories' git orders.
	Order Order
	// Boxes is this node's current sandbox catalogue — what a lost runner's
	// remote box is killed through — false where it configures none.
	Boxes func() (Boxes, bool)
	// Poll is a runner's poll, which the slack is SweepPolls of: CheckPoll in
	// production, a suite's shorter one.
	Poll time.Duration
	// Logger nil is the package's own; Now nil the wall clock in UTC.
	Logger *slog.Logger
	Now    func() time.Time
}

// Sweeper is a repository's HOME's sweep of the checks whose runner is gone:
// a worker of internal/code/home's loop (its Worker: Name, Due, Work), which
// this package does not import and the engine registers. The loop asks it,
// at every pass and every nudge, for the repositories holding a running
// attempt and works each one this node is home to.
type Sweeper struct {
	o   SweeperOptions
	log *slog.Logger
	now func() time.Time
}

// NewSweeper builds the home's sweep, refusing what it cannot work without.
func NewSweeper(o SweeperOptions) (*Sweeper, error) {
	var missing []string
	if o.Estate == nil {
		missing = append(missing, "Estate")
	}
	if o.Order == nil {
		missing = append(missing, "Order")
	}
	if o.Boxes == nil {
		missing = append(missing, "Boxes")
	}
	if o.Poll <= 0 {
		missing = append(missing, "Poll")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrOptions, strings.Join(missing, ", "))
	}
	s := &Sweeper{o: o, log: o.Logger, now: o.Now}
	if s.log == nil {
		s.log = logging.Get("code.checkrun")
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	return s, nil
}

// Name is [WorkerCheckRunners].
func (s *Sweeper) Name() string { return WorkerCheckRunners }

// freshAt is the level a read at floor is asked at: `session` beside it, and
// stale where it is zero — the home's own rule.
func freshAt(floor statelog.Position) statelog.Freshness {
	if floor.IsZero() {
		return stale
	}
	return statelog.FloorAt(statelog.ReadSession, floor)
}

// Due is one page of the repositories holding a running attempt, by id after
// cursor.
func (s *Sweeper) Due(ctx context.Context, floor statelog.Position, cursor string) ([]string, string, error) {
	a, err := s.o.Estate.Read(ctx, code.ReadQuery{Kind: code.ReadChecksRunning, Cursor: cursor,
		ChecksRunning: &code.ChecksRunningQuery{}, Fresh: freshAt(floor)})
	if err != nil {
		return nil, "", err
	}
	return a.DueRepos, a.Next, nil
}

// Work sweeps repoID's running attempts: each whose runner has left the
// repository's git order, or whose claim has outlived its check's timeout,
// the setup's grace and the slack, is ended `errored runner_lost` — and its
// box killed where it is a remote one. A container box on a node that left is
// that node's container runtime's, reclaimed by its own reaping. An attempt a
// person's CI reports runs on no runner of the engine's, and is not swept.
//
// AN ENDING ASKED BY TWO HOMES — a presence view split for a moment — is one
// write: its operation id is derived from the attempt and its claim, so the
// second collapses onto the first.
func (s *Sweeper) Work(ctx context.Context, repoID string, floor statelog.Position) error {
	order, err := s.o.Order.GitOrder(repoID)
	if err != nil {
		return fmt.Errorf("checkrun: the git order of repository %s: %w", repoID, err)
	}
	a, err := s.o.Estate.Read(ctx, code.ReadQuery{Kind: code.ReadChecksRunning,
		ChecksRunning: &code.ChecksRunningQuery{RepoID: repoID}, Fresh: freshAt(floor)})
	if err != nil {
		return err
	}
	var failed []error
	for _, row := range a.Checks {
		why := s.lost(row, order)
		if why == "" {
			continue
		}
		if err := s.end(ctx, row, why); err != nil {
			failed = append(failed, err)
			continue
		}
		s.kill(ctx, row)
	}
	return errors.Join(failed...)
}

// lost is why row's runner is gone, "" where it is not.
func (s *Sweeper) lost(row code.CheckRow, order []string) string {
	switch {
	case row.State != code.CheckStateRunning || row.Placement == code.PlacementExternal || row.Runner == "":
		return ""
	case !slices.Contains(order, row.Runner):
		return fmt.Sprintf("its runner %s left the repository's git nodes while it ran", row.Runner)
	}
	budget := time.Duration(row.TimeoutSeconds)*time.Second + code.CheckBoxGrace + SweepPolls*s.o.Poll
	if s.now().Sub(row.StartedAt) > budget {
		return fmt.Sprintf("its runner %s held it %s past its timeout and its box's setup without ending it",
			row.Runner, (s.now().Sub(row.StartedAt) - budget + SweepPolls*s.o.Poll).Round(time.Second))
	}
	return ""
}

// end records row's attempt `errored runner_lost` with why.
func (s *Sweeper) end(ctx context.Context, row code.CheckRow, why string) error {
	opID := statelog.DeriveOpID(row.StartedAt, "code_check_lost", row.RepoID, row.OID, row.Target, row.Name,
		strconv.Itoa(row.Attempt), row.Runner)
	res, err := s.o.Estate.Checks(ctx, code.ChecksRequest{OpID: opID, Finished: &code.CheckFinished{
		CheckKey: row.CheckKey, State: code.CheckStateErrored, Reason: code.CheckReasonRunnerLost,
		DurationMs: max(s.now().Sub(row.StartedAt).Milliseconds(), 0), Summary: summaryOf(why),
		FinishedAt: s.now()}})
	switch {
	case err != nil:
		return fmt.Errorf("checkrun: end attempt %d of check %s of %s as its runner's lost: %w", row.Attempt,
			row.Name, row.OID, err)
	case res.Refusal == code.CheckRefusalBusy || res.Refusal == code.CheckRefusalUnavailable ||
		res.Refusal == code.CheckRefusalNotApplied:
		return fmt.Errorf("checkrun: end attempt %d of check %s of %s as its runner's lost: %s: %s",
			row.Attempt, row.Name, row.OID, res.Refusal, res.Detail)
	}
	s.log.InfoContext(ctx, "code_check_runner_lost", "repo_id", row.RepoID, "oid", row.OID, "check", row.Name,
		"attempt", row.Attempt, "runner", row.Runner, "detail", why)
	return nil
}

// kill ends a lost runner's REMOTE box through its provider: a box of the
// catalogue's e2b cell, which no other node's teardown reaches and which
// bills until its TTL. Best effort — a box already gone is no error, and one
// that cannot be reached is reclaimed by its TTL.
func (s *Sweeper) kill(ctx context.Context, row code.CheckRow) {
	if row.Placement != code.PlacementE2B || row.Box == "" {
		return
	}
	boxes, ok := s.o.Boxes()
	if !ok || boxes == nil {
		return
	}
	provider, err := boxes.Provider(sandbox.Placement(config.PlacementE2B))
	if err == nil {
		err = provider.Kill(ctx, row.Box)
	}
	if err != nil {
		s.log.WarnContext(ctx, "code_check_box_not_killed", "repo_id", row.RepoID, "check", row.Name,
			"box", row.Box, "error", err.Error(), "detail", "its TTL reclaims it")
	}
}
