// A schedule's fires, the company's and one schedule's own.

package queries

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/usage"
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
	runs, more, err := usage.ScheduleRuns(ctx, s.Usage, usage.ScheduleRunsQuery{
		ScopeType: string(scope), ScopeID: scopeID, Name: name, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	names := s.scopeNames()
	return map[string]any{
		"scope_type":    string(scope),
		"scope_id":      scopeID,
		"scope_name":    names.of(scope, scopeID),
		"schedule_name": name,
		"runs":          scheduleRunRows(names, runs),
		// SAYS WHEN IT CUT, like every other page in this tree: a page
		// that filled is indistinguishable from a schedule that has fired
		// exactly that many times. The read takes one row past the page,
		// so a schedule that has fired exactly fifty times is not called
		// cut.
		"truncated": more,
	}, nil
}

// recentRuns is the company's newest fires across every schedule, or an empty
// list.
//
// DEGRADES rather than fails: the configured schedules are the half an
// operator opens this screen for, and refusing to show them because the
// history is unreadable would blank the page over its footnote.
func (s Sources) recentRuns(ctx context.Context) []map[string]any {
	if s.Usage == nil {
		return []map[string]any{}
	}
	runs, _, err := usage.ScheduleRuns(ctx, s.Usage, usage.ScheduleRunsQuery{Limit: RecentRunsLimit})
	if err != nil {
		log.WarnContext(ctx, "schedule_history_unreadable", "error", err)
		return []map[string]any{}
	}
	return scheduleRunRows(s.scopeNames(), runs)
}

// scheduleRunRows is a page of fires as both schedule answers send it.
//
// ONE SHAPE for the company's recent runs and one schedule's history, because
// the screen draws both with one row and joins the first onto its schedules by
// the identity the second is asked by.
func scheduleRunRows(names scopeNames, runs []usage.ScheduleRun) []map[string]any {
	out := make([]map[string]any, 0, len(runs))
	for _, run := range runs {
		out = append(out, map[string]any{
			"scope_type":    run.ScopeType,
			"scope_id":      run.ScopeID,
			"scope_name":    names.of(types.ScheduleScope(run.ScopeType), run.ScopeID),
			"schedule_name": run.Name,
			// THE FIRE LABEL IS THE IDENTITY of one fire — the minute it
			// was scheduled for — which is what tells a catchup fire from
			// the tick that should have run it.
			"fire_label":    run.FireLabel,
			"target_handle": run.Target,
			"scheduled_at":  isoOrEmpty(run.ScheduledAt),
			"fired_at":      isoOrEmpty(run.FiredAt),
			// WHAT BECAME OF THE FIRE, not only what the scheduler did:
			// `skipped_superseded` is a fire its seat did not run because
			// a later fire of the schedule ran in its place.
			"outcome":  run.Outcome,
			"trace_id": run.TraceID,
		})
	}
	return out
}
