package queries

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// weekOf lists every row of eventType in the window before now, newest first, a page at a time.
// A fleet that did not answer whole, or a week past the bound, is an error: the source is then
// unread, never read in part as though whole.
func (s Sources) weekOf(ctx context.Context, now time.Time, eventType string, failed *bool) ([]store.EventRecord, error) {
	if s.Events == nil {
		return nil, errors.New("queries: this node reads no event log")
	}
	q := store.ListQuery{Type: eventType, Failed: failed, Since: now.Add(-advisor.Window), Until: now,
		Limit: failedRunsPage, At: now}
	var rows []store.EventRecord
	for page := 0; ; page++ {
		if page == maxFailedRunPages {
			return nil, fmt.Errorf("queries: the week holds more than %d %s records",
				failedRunsPage*maxFailedRunPages, eventType)
		}
		listing, coverage, err := s.Events.List(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("queries: list the week's %s records: %w", eventType, err)
		}
		if !coverage.Complete {
			return nil, fmt.Errorf("queries: not every node answered for its %s records", eventType)
		}
		rows = append(rows, listing.Rows...)
		if !listing.More || len(listing.Rows) == 0 {
			return rows, nil
		}
		oldest := listing.Rows[len(listing.Rows)-1]
		q.Before = &store.Cursor{Time: oldest.Time, ID: oldest.ID}
	}
}

// endedRuns is every executor run in the window that ended for memory or on its round cap, by
// agent handle, off the listing's `error_kind` tag, `tokens` and `calls`. A box's memory ceiling
// is in the run's payload alone, so it is read once per seat, off its newest such run.
func (s Sources) endedRuns(ctx context.Context, now time.Time) ([]advisor.EndedRun, error) {
	failed := true
	rows, err := s.weekOf(ctx, now, types.ExecutorCollected{}.EventType(), &failed)
	if err != nil {
		return nil, err
	}
	chart := s.chartHandles()
	ceilings := map[string]int64{}
	var out []advisor.EndedRun
	for _, rec := range rows {
		kind := rec.Tags["error_kind"]
		seat, charted := chart[rec.AgentID]
		if !charted || (kind != types.ExecutorErrorOutOfMemory && kind != types.ExecutorErrorMaxTurns) {
			continue
		}
		run := advisor.EndedRun{Seat: seat.handle, Kind: kind, Turn: rec.Tags["turn_id"],
			Tokens: int64(rec.Tokens), Rounds: int64(rec.Calls)}
		if kind == types.ExecutorErrorOutOfMemory {
			ceiling, read := ceilings[seat.handle]
			if !read {
				if ceiling, err = s.memoryCeiling(ctx, rec.ID); err != nil {
					return nil, err
				}
				ceilings[seat.handle] = ceiling
			}
			run.MemoryBytes = ceiling
		}
		out = append(out, run)
	}
	return out, nil
}

// memoryCeiling is the box's memory ceiling one run's record names.
func (s Sources) memoryCeiling(ctx context.Context, id string) (int64, error) {
	rec, _, err := s.Events.ByID(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("queries: read run %s: %w", id, err)
	}
	var run types.ExecutorCollected
	if err := json.Unmarshal(rec.Payload, &run); err != nil {
		return 0, fmt.Errorf("queries: decode run %s: %w", id, err)
	}
	return run.MemoryBytes, nil
}

// budgetStops is every turn in the window a token budget refused, by seat handle, oldest first.
// When its window resets is in the record's payload alone, so it is read once per stop: a
// refusal inside a window already read is that stop's.
func (s Sources) budgetStops(ctx context.Context, now time.Time) ([]advisor.BudgetStop, error) {
	rows, err := s.weekOf(ctx, now, types.BudgetExhausted{}.EventType(), nil)
	if err != nil {
		return nil, err
	}
	slices.Reverse(rows)
	chart := s.chartHandles()
	covered := map[string]advisor.BudgetStop{}
	var out []advisor.BudgetStop
	for _, rec := range rows {
		seat, charted := chart[rec.AgentID]
		if !charted {
			continue
		}
		stop := advisor.BudgetStop{Seat: seat.handle, Turn: rec.Tags["turn_id"], At: rec.Time}
		if last, ok := covered[seat.handle]; ok && rec.Time.Before(last.Resets) {
			stop.Company, stop.Period, stop.Ceiling, stop.Resets = last.Company, last.Period, last.Ceiling, last.Resets
			out = append(out, stop)
			continue
		}
		full, _, err := s.Events.ByID(ctx, rec.ID)
		if err != nil {
			return nil, fmt.Errorf("queries: read budget refusal %s: %w", rec.ID, err)
		}
		var refused types.BudgetExhausted
		if err := json.Unmarshal(full.Payload, &refused); err != nil {
			return nil, fmt.Errorf("queries: decode budget refusal %s: %w", rec.ID, err)
		}
		stop.Company = refused.BudgetType == types.BudgetScopeOrg
		stop.Period, stop.Ceiling = refused.Period, int64(refused.MaxTokens)
		if resets, perr := time.Parse(time.RFC3339, refused.ResetsAt); perr == nil {
			stop.Resets = resets
		}
		covered[seat.handle] = stop
		out = append(out, stop)
	}
	return out, nil
}

// uncheckedMerges is every merge request in the window before now a delegate
// allowed and the engine put to people because the target branch requires no
// check, read off the access log's verdicts, each request once.
func (s Sources) uncheckedMerges(ctx context.Context, now time.Time) ([]advisor.UncheckedMerge, error) {
	if s.Delegated == nil {
		return nil, nil
	}
	q := accessdomain.VerdictQuery{Kind: delegated.KindPRMerge, Verdict: delegated.Escalate,
		Since: now.Add(-advisor.Window).UTC(), Until: now.UTC(), Limit: accessdomain.MaxPageSize}
	seen := map[string]bool{}
	var out []advisor.UncheckedMerge
	for page := 0; ; page++ {
		if page == maxFailedRunPages {
			return nil, fmt.Errorf("queries: the week holds more than %d merge verdicts",
				accessdomain.MaxPageSize*maxFailedRunPages)
		}
		verdicts, err := s.Delegated.Verdicts(ctx, q, statelog.Freshness{Level: statelog.ReadStale})
		if err != nil {
			return nil, fmt.Errorf("queries: read the week's merge verdicts: %w", err)
		}
		for _, v := range verdicts.Rows {
			if seen[v.Request] || !slices.Contains(v.Overruled, delegated.UnmetNoChecks) {
				continue
			}
			seen[v.Request] = true
			// A SUBJECT THIS NODE'S KEYRING NO LONGER OPENS names no repository.
			subject, _, _, err := s.Delegated.OpenVerdict(v)
			if err != nil {
				continue
			}
			change := mergeChange(subject)
			repo, github, ok := advisor.ChangeRepo(change)
			if !ok {
				continue
			}
			out = append(out, advisor.UncheckedMerge{Request: v.Request, Change: change, Repo: repo, GitHub: github})
		}
		if verdicts.Next == "" {
			return out, nil
		}
		q.Cursor = verdicts.Next
	}
}

// mergeChange is the change a merge's subject names: "merge <change> at
// <head> (<method>)", as the access log keeps it.
func mergeChange(subject string) string {
	rest, ok := strings.CutPrefix(subject, "merge ")
	if !ok {
		return ""
	}
	change, _, _ := strings.Cut(rest, " at ")
	return change
}

// waitingWakes is each agent seat's wakes queued now, by handle, where its
// holder reported them.
func (s Sources) waitingWakes() map[string]int {
	if s.State == nil {
		return nil
	}
	out := map[string]int{}
	for id, seat := range s.chartHandles() {
		if overlay := s.State.AgentOverlay(id); overlay != nil && overlay.Waiting != nil {
			out[seat.handle] = overlay.Waiting.Priority + overlay.Waiting.Background
		}
	}
	return out
}
