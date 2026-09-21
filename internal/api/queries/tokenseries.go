package queries

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
)

// tokenSeries answers the spend WITH A TIME AXIS.
//
// `tokens` answers what a window cost, split five ways, and every one of its
// rows is a sum over the whole window — so it cannot say when. That is the
// question a cost explorer is for: a total with no shape is a number nobody
// can act on, and the shape is what a spike, a runaway loop or a quiet weekend
// looks like.
//
// A SECOND QUESTION rather than a flag on the first, because the two answers
// have different shapes: one is a breakdown and one is a series, and a query
// that returned either depending on a parameter is a client branch at every
// call site.
//
// It reads the event store rather than the live projection even for the live
// window. The projection holds at most a day, bucketed hourly that is 24 bars
// — and the moment a reader widens the range past it the axis would have to
// switch sources mid-chart, which is the seam a comparison is read across.
func (s Sources) tokenSeries(ctx context.Context, p Params) (any, error) {
	group := tokens.Group(p.String("group"))
	if group == "" {
		group = tokens.GroupPhase
	}
	if !group.Valid() {
		return nil, fmt.Errorf("%w: group must be one of %v, and %q is not — a "+
			"chart legended by one dimension over another's bands is worse "+
			"than an error", ErrBadParams, tokens.Groups, group)
	}
	interval := tokens.Interval(p.String("bucket"))
	if interval == "" {
		interval = tokens.IntervalHour
	}
	if !interval.Valid() {
		return nil, fmt.Errorf("%w: bucket must be one of %v, and %q is not",
			ErrBadParams, tokens.Intervals, interval)
	}

	since, err := instantParam(p, "since")
	if err != nil {
		return nil, err
	}
	until, err := instantParam(p, "until")
	if err != nil {
		return nil, err
	}
	if !since.IsZero() && !until.IsZero() && !until.After(since) {
		return nil, fmt.Errorf("%w: until (%s) is not after since (%s) — the "+
			"window is half-open, so an empty one has no buckets at all",
			ErrBadParams, until.Format(time.RFC3339), since.Format(time.RFC3339))
	}
	// COMPARE-TO-PREVIOUS is a shift of the window, computed where the
	// bucketing is rather than in the browser: a client subtracting in
	// local time produces two windows of different lengths across a DST
	// boundary, and the chart then reports a change nobody made. It needs
	// both edges — there is no "the previous N days back from now".
	if p.Bool("previous", false) {
		if since.IsZero() || until.IsZero() {
			return nil, fmt.Errorf("%w: previous needs both since and until, "+
				"because the window before an open-ended one has no length",
				ErrBadParams)
		}
		since, until = tokens.PreviousWindow(since, until)
	}

	q := store.PhaseTokenQuery{
		SinceDays: p.Int("since_days", 0),
		Since:     since, Until: until,
		AgentRole: s.roleOf(p.String("agent_role")),
	}
	// The window the STORE will serve, which is what the answer is labelled
	// with: a request further back than the table's retention is floored,
	// and an axis headed with the year it was asked for over a month of
	// bars is a lie about the bars.
	covered, coveredUntil := q.Window(s.clock())

	records, err := s.Events.PhaseTokens(ctx, q)
	if err != nil {
		return nil, err
	}
	return tokens.Bucketed(records, tokens.SeriesOptions{
		Group: group, Interval: interval,
		Since: covered, Until: coveredUntil,
		Groups:  Clamp(p.Int("groups", 0), tokens.DefaultSeriesGroups, tokens.MaxSeriesGroups),
		Handles: s.RoleHandles(),
		Units:   s.RoleUnits(),
	}), nil
}

// RoleUnits maps each seat's role name to the unit holding it, for the
// per-unit band of a series.
//
// Only the DIRECT unit, not the chain: a band per nesting level would count
// the same spend once for the team and again for the department above it, and
// a stacked chart whose bands sum to more than the total is unreadable. A seat
// at the root of the chart is absent, which [tokens.Group] places in its own
// band rather than pooling with the records that carried no role at all.
//
// Exported for the reason [Sources.RoleHandles] is: two derivations of "which
// unit is this seat in" is how a chart and a filter come to disagree.
func (s Sources) RoleUnits() map[string]string {
	out := map[string]string{}
	if s.Company == nil {
		return out
	}
	company, roster := s.Company()
	if company == nil {
		return out
	}
	// THE COMPANY'S OWN ORG, derived from this node's chart rows rather
	// than re-resolved from the document: a stored revision carries no
	// seats at all, so the derivation this replaced answered an EMPTY
	// organization for every running company.
	if roster == nil {
		return out
	}
	for role := range roster.AllRoles() {
		if unit := roster.UnitFor(role); unit != nil {
			out[role.Name] = unit.Name
		}
	}
	return out
}
