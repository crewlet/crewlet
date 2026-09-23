// One schedule's own dispatch history.

package queries

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/events/types"
)

// MaxScheduleRuns bounds one schedule's history page.
//
// FIFTY, which is [RecentRunsLimit]'s figure for the company-wide feed — but
// spent on ONE schedule rather than shared across every one of them, and that
// is the whole point of this question. A company with twenty hourly schedules
// fills the shared page in two and a half hours, so "did the standup fire this
// week" was unanswerable while every row of the answer sat in the table.
//
// Fifty fires is a fortnight of a twice-daily schedule and two days of an
// hourly one, which is the span somebody debugging a schedule actually reads.
//
// Read with ONE MORE as the evidence, so `truncated` says the ledger holds
// older fires of this schedule rather than that the page happened to fill.
// Those older fires are NOT served: this question takes no cursor, because
// the ledger's narrowed listing ([ScheduleRuns.RecentFor]) takes none, and
// they stay in this node's `scheduled_runs` table until its retention sweep.
const MaxScheduleRuns = 50

// scheduleRuns answers one schedule's recent fires.
//
// THE SCOPE TUPLE IS THE SCHEDULE'S IDENTITY, and it is exactly what the
// `schedules` listing already carries on every row — so a caller hands back
// what it was given rather than composing a key. A schedule name alone is not
// an identity: two units can both declare a `standup`, and a read keyed on the
// name would merge two teams' histories into one list.
func (s Sources) scheduleRuns(ctx context.Context, p Params) (any, error) {
	scope := types.ScheduleScope(strings.TrimSpace(p.String("scope_type")))
	if !scope.Valid() {
		return nil, badParams("scope_type", string(scope), names(types.ScheduleScopes()))
	}
	scopeID := strings.TrimSpace(p.String("scope_id"))
	name := strings.TrimSpace(p.String("name"))
	switch {
	case scopeID == "":
		return nil, badParams("scope_id", "", nil)
	case name == "":
		return nil, badParams("name", "", nil)
	}

	limit := Clamp(p.Int("limit", 0), MaxScheduleRuns, MaxScheduleRuns)
	runs, err := s.Runs.RecentFor(ctx, scope, scopeID, name, limit+1)
	if err != nil {
		return nil, err
	}
	// THE EVIDENCE ROW: one read past the page, dropped here. A page that
	// filled is otherwise indistinguishable from a schedule that has fired
	// exactly that many times.
	truncated := len(runs) > limit
	if truncated {
		runs = runs[:limit]
	}
	out := make([]map[string]any, 0, len(runs))
	for _, run := range runs {
		out = append(out, runRow(run))
	}
	return map[string]any{
		"scope_type":    string(scope),
		"scope_id":      scopeID,
		"schedule_name": name,
		"runs":          out,
		"truncated":     truncated,
	}, nil
}
