package queries

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/usage"
	"github.com/crewlet/crewlet/internal/worklet"
)

// firesPage is how many fires one read of the week takes, and maxFirePages how
// many reads it takes before it names the source read in part.
const (
	firesPage    = 200
	maxFirePages = 10
)

// turnMoves is the tracker's read of which turns moved work.
type turnMoves interface {
	TurnsMoved(ctx context.Context, q worklet.TurnsMovedQuery) (map[string]bool, error)
}

// scheduleFires is every fire in the window before now whose turn is known, by
// its schedule, and whether that turn moved work: read from what the turn wrote
// to the tracker, never from the turn's own account of itself.
func (s Sources) scheduleFires(ctx context.Context, now time.Time) ([]advisor.ScheduleFire, error) {
	moves, tracked := s.Work.(turnMoves)
	switch {
	case s.Usage == nil:
		return nil, errors.New("queries: no usage half to read the schedules' fires from")
	case !tracked:
		return nil, errors.New("queries: this node reads no tracker for what the fires' turns wrote")
	}
	runs, err := s.weekOfFires(ctx, now)
	if err != nil {
		return nil, err
	}
	var turns []string
	for _, r := range runs {
		if r.TurnID != "" {
			turns = append(turns, r.TurnID)
		}
	}
	moved, err := moves.TurnsMoved(ctx, worklet.TurnsMovedQuery{Turns: turns, Level: statelog.ReadStale})
	if err != nil {
		return nil, err
	}
	chart := s.chartHandles()
	var out []advisor.ScheduleFire
	for _, r := range runs {
		if r.TurnID == "" {
			continue
		}
		fire := advisor.ScheduleFire{Name: r.Name, Moved: moved[r.TurnID]}
		switch types.ScheduleScope(r.ScopeType) {
		case types.ScheduleScopeRole:
			seat, ok := chart[r.ScopeID]
			if !ok || seat.handle == "" {
				continue
			}
			fire.Seat = seat.handle
		case types.ScheduleScopeUnit:
			fire.Unit = r.ScopeID
		default:
			continue
		}
		out = append(out, fire)
	}
	return out, nil
}

// weekOfFires is every fire sent in the window before now, newest first, read
// a page at a time back to the window's start; past the bound it is an error.
func (s Sources) weekOfFires(ctx context.Context, now time.Time) ([]usage.ScheduleRun, error) {
	since := now.Add(-advisor.Window)
	q := usage.ScheduleRunsQuery{Outcome: string(schedule.OutcomeFired), Limit: firesPage}
	var out []usage.ScheduleRun
	for page := 0; page < maxFirePages; page++ {
		runs, more, err := s.Usage.ScheduleRuns(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("queries: read the week's schedule fires: %w", err)
		}
		for _, r := range runs {
			if r.FiredAt.Before(since) {
				return out, nil
			}
			if !r.FiredAt.After(now) {
				out = append(out, r)
			}
		}
		if !more || len(runs) == 0 {
			return out, nil
		}
		last := runs[len(runs)-1]
		q.BeforeAt, q.BeforeKey = last.FiredAt, last.Key()
	}
	return nil, fmt.Errorf("queries: the week holds more than %d schedule fires", firesPage*maxFirePages)
}
