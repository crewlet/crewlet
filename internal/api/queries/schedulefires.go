package queries

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
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

// fireSettles is how long a fire's turn is given to finish before its fire is
// counted: a turn still running has not moved anything yet.
const fireSettles = time.Hour

// The tracker's, the pages' and the code host's reads of which turns moved
// work there.
type (
	trackerMoves interface {
		TurnsMoved(ctx context.Context, q worklet.TurnsMovedQuery) (map[string]bool, error)
	}
	pageMoves interface {
		TurnsMovedPages(ctx context.Context, q doclet.TurnsMovedQuery) (map[string]bool, error)
	}
	codeMoves interface {
		TurnsMovedCode(ctx context.Context, q gitlet.TurnsMovedQuery) (map[string]bool, error)
	}
)

// movesOf is one half's read of which of a set of turns moved work there.
type movesOf func(ctx context.Context, turns []string) (map[string]bool, error)

// scheduleFires is every settled fire in the window before now whose turn is
// known, by its schedule, and whether that turn moved work: read from what the
// turn wrote to the tracker, the pages and the code host, never from the
// turn's own account of itself. A company whose tracker is a vendor's is not
// judged: its fires are none.
func (s Sources) scheduleFires(ctx context.Context, now time.Time) ([]advisor.ScheduleFire, error) {
	if s.Usage == nil {
		return nil, errors.New("queries: no usage half to read the schedules' fires from")
	}
	halves, judged, err := s.moveReads()
	if err != nil || !judged {
		return nil, err
	}
	runs, err := s.weekOfFires(ctx, now)
	if err != nil {
		return nil, err
	}
	var turns []string
	settled := runs[:0]
	for _, r := range runs {
		if r.TurnID != "" && !r.FiredAt.After(now.Add(-fireSettles)) {
			turns = append(turns, r.TurnID)
			settled = append(settled, r)
		}
	}
	moved := map[string]bool{}
	for _, read := range halves {
		some, rerr := read(ctx, turns)
		if rerr != nil {
			return nil, rerr
		}
		for turn := range some {
			moved[turn] = true
		}
	}
	chart := s.chartHandles()
	var out []advisor.ScheduleFire
	for _, r := range settled {
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

// moveReads is the read of each half this company runs on the engine, and
// false when its tracker is a vendor's. A wiki or code host that is not the
// engine's has no rows to read; its writers are left out by the advisor.
func (s Sources) moveReads() ([]movesOf, bool, error) {
	var out []movesOf
	switch kept, err := keptHere(s.Work); {
	case err != nil:
		return nil, false, err
	case !kept:
		return nil, false, nil
	}
	tracker, ok := s.Work.(trackerMoves)
	if !ok {
		return nil, false, errors.New("queries: this node reads no tracker for what the fires' turns wrote")
	}
	out = append(out, func(ctx context.Context, turns []string) (map[string]bool, error) {
		return tracker.TurnsMoved(ctx, worklet.TurnsMovedQuery{Turns: turns, Level: statelog.ReadStale})
	})
	switch kept, err := keptHere(s.Pages); {
	case err != nil:
		return nil, false, err
	case kept:
		pages, served := s.Pages.(pageMoves)
		if !served {
			return nil, false, errors.New("queries: this node reads no pages for what the fires' turns wrote")
		}
		out = append(out, func(ctx context.Context, turns []string) (map[string]bool, error) {
			return pages.TurnsMovedPages(ctx, doclet.TurnsMovedQuery{Turns: turns, Level: statelog.ReadStale})
		})
	}
	if s.Code == nil {
		return out, true, nil
	}
	reads, err := s.Code.CodeReads()
	var notFound *NotFound
	switch {
	case errors.As(err, &notFound):
		return out, true, nil
	case err != nil:
		return nil, false, err
	}
	host, ok := reads.(codeMoves)
	if !ok {
		return nil, false, errors.New("queries: this node reads no code for what the fires' turns wrote")
	}
	out = append(out, func(ctx context.Context, turns []string) (map[string]bool, error) {
		return host.TurnsMovedCode(ctx, gitlet.TurnsMovedQuery{Turns: turns, Level: statelog.ReadStale})
	})
	return out, true, nil
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
