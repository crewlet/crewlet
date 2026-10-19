package queries

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
	"github.com/crewlet/crewlet/internal/usage"
)

// THE SPEND ANSWERS, `tokens` and `token_series`, and where each one reads.
//
// TWO SOURCES, one aggregation. The live projection holds the phase records of
// its own rolling window and answers that instantly — it is what the `tokens`
// push carries and what the dashboard's live views read. EVERY NAMED WINDOW is
// company days read from the replicated `usage` domain (ADR-0020): every
// node's day, so the answer is the same on whichever node is asked, reaches
// back the domain's 181 days rather than the event log's 30, and still counts
// a node that has left the fleet. It replaced a scan of THIS node's event log,
// which answered for a third of a three-node fleet under the company's name and
// drew a ninety-day chart over thirty days of rows.
//
// The two are folded by internal/tokens into one shape, so a reader moving
// from the live window to a named one compares like with like.

// spendWindow is a named spend window as its parameters asked for it.
type spendWindow struct {
	Range   tokens.Range
	Horizon tokens.Horizon

	// AgentID and Person are the one seat `seat=<handle>` narrows to, by
	// the key its days are filed under — both empty for the whole company.
	//
	// An AGENT seat is its agent id, resolved through the chart to the id
	// every node derives for it ([Sources.seatAgentID]); a HUMAN seat is
	// its handle, the key a person's day is filed under (the usage domain's
	// person kind), since a human seat has no agent id. A handle the chart
	// no longer holds sets BOTH — the id derived from it and the handle —
	// because the chart can no longer say which kind it was, and one
	// handle names one seat, so at most one of the two finds anything.
	AgentID string
	Person  string
}

// seatOnly reports whether the window is narrowed to an agent seat the chart
// names as one, which no person's day can belong to.
func (w spendWindow) seatOnly() bool { return w.AgentID != "" && w.Person == "" }

// personOnly reports whether the window is narrowed to a human seat the chart
// names as one, which no agent seat's day can belong to.
func (w spendWindow) personOnly() bool { return w.Person != "" && w.AgentID == "" }

// DefaultSpendRangeDays is the window a named spend question covers when it
// names none: the company's today, the one day the live projection's rolling
// window does not line up with.
const DefaultSpendRangeDays = 1

// namesAWindow reports whether the parameters ask for anything but the live,
// unfiltered window.
func namesAWindow(p Params) bool {
	for _, key := range []string{"days", "since", "until", "seat", "previous"} {
		if p.Has(key) {
			return true
		}
	}
	return false
}

// spendWindowOf reads the window a spend question names.
//
// `days` (1..[tokens.MaxSpendRangeDays], the company days ending today) OR
// `since` and `until` (two company-local dates, both INCLUSIVE — "1 June to
// 8 June" is eight days); `previous` shifts either back by its own length;
// `seat` is a handle — an agent seat's, resolved to its agent id through the
// chart, or a person's human seat ([Sources.spendSeat]). Every refusal names
// the parameter to change.
func (s Sources) spendWindowOf(p Params) (spendWindow, error) {
	loc := s.zone()
	now := s.clock()
	today := tokens.LastDays(1, now, loc).Last
	w := spendWindow{Horizon: tokens.NewHorizon(usage.HorizonDays, today)}

	since := strings.TrimSpace(p.String("since"))
	until := strings.TrimSpace(p.String("until"))
	switch {
	case p.Has("days") && (since != "" || until != ""):
		return w, fmt.Errorf("%w: name the window by days or by since and until, "+
			"not both", ErrBadParams)
	case since != "" || until != "":
		if since == "" || until == "" {
			return w, fmt.Errorf("%w: since and until come as a pair — two company "+
				"dates, both inclusive (since=2026-06-01&until=2026-06-08)", ErrBadParams)
		}
		r, err := tokens.DaysBetween(since, until, loc)
		if err != nil {
			return w, fmt.Errorf("%w: since and until are company dates written "+
				"2026-06-01, the first no later than the second: %w", ErrBadParams, err)
		}
		if n := r.Days(); n > tokens.MaxSpendRangeDays {
			return w, refuseAs(tokens.ErrWindowLength, "%s to %s is %d days, and a "+
				"spend window is at most %d — bring since and until closer together",
				r.First.Label, r.Last.Label, n, tokens.MaxSpendRangeDays)
		}
		w.Range = r
	default:
		days := DefaultSpendRangeDays
		if p.Has("days") {
			days = p.Int("days", 0)
			if days < 1 || days > tokens.MaxSpendRangeDays {
				return w, refuseAs(tokens.ErrWindowLength, "days is %v, and a spend "+
					"window is 1 to %d company days — ask for at most %d",
					p.Values()["days"], tokens.MaxSpendRangeDays, tokens.MaxSpendRangeDays)
			}
		}
		w.Range = tokens.LastDays(days, now, loc)
	}
	if !w.Horizon.Admits(w.Range) {
		return w, refuseAs(tokens.ErrOutOfRange, "the window starts %s, before %s — "+
			"the oldest day the %d-day spend history still holds on any node; move "+
			"since to %s or later", w.Range.First.Label, w.Horizon.Floor,
			w.Horizon.Days, w.Horizon.Floor)
	}
	if p.Bool("previous", false) {
		prev := w.Range.Previous()
		if !w.Horizon.Admits(prev) {
			// NAMING `days`, because that is the knob a reader turns: the
			// window before a long enough one starts past the floor, and
			// the fix is a shorter window, not a different comparison.
			return w, refuseAs(tokens.ErrOutOfRange, "the window before these %d "+
				"days starts %s, before %s — the oldest day the %d-day spend history "+
				"holds; ask for fewer days", w.Range.Days(), prev.First.Label,
				w.Horizon.Floor, w.Horizon.Days)
		}
		w.Range = prev
	}

	if err := s.spendSeat(p, &w); err != nil {
		return w, err
	}
	return w, nil
}

// spendSeat narrows w to the seat `seat=<handle>` names — see
// [spendWindow.AgentID].
//
// A PERSON'S SEAT NARROWS TO THEIR SPEND rather than being refused, unlike the
// turn and phase questions ([Sources.seatAgentID]): a person takes no turn, but
// what the operator surface's auxiliary model spent for them is filed under
// their human seat's handle, so "what did this person cost" has an answer and
// it is theirs alone.
func (s Sources) spendSeat(p Params, w *spendWindow) error {
	handle := strings.TrimSpace(p.String("seat"))
	if handle == "" {
		return nil
	}
	if organization := s.organization(); organization != nil && org.ValidHandle(handle) {
		if role := organization.Role(handle); role != nil && role.IsHuman() {
			w.Person = handle
			return nil
		}
	}
	agentID, err := s.seatAgentID(handle)
	if err != nil {
		return err
	}
	w.AgentID = agentID
	if organization := s.organization(); organization != nil && organization.Role(handle) == nil {
		w.Person = handle
	}
	return nil
}

// spendCells reads a window's company-day rows from the usage domain.
func (s Sources) spendCells(ctx context.Context, w spendWindow) ([]tokens.Cell, []tokens.SeatDay, error) {
	q := usage.SpendQuery{From: w.Range.First.Label, To: w.Range.Last.Label, AgentID: w.AgentID}
	if w.personOnly() {
		// A PERSON'S WINDOW holds no agent seat's day, and an empty
		// AgentID asks for every one.
		return s.personCells(ctx, w, q, nil, nil)
	}
	rows, err := usage.Spend(ctx, s.Usage, q)
	if err != nil {
		return nil, nil, usageErr(err)
	}
	turns, err := usage.SeatTurns(ctx, s.Usage, q)
	if err != nil {
		return nil, nil, usageErr(err)
	}
	cells := make([]tokens.Cell, 0, len(rows))
	for _, r := range rows {
		cells = append(cells, tokens.Cell{
			Day: r.Day, AgentID: r.AgentID, Handle: r.Handle, Role: r.Role,
			Phase: r.Phase, Worker: r.Worker, Model: r.Model, ProviderKey: r.ProviderKey,
			Bucket: tokens.Bucket{
				InputTokens: int(r.Input), OutputTokens: int(r.Output), TotalTokens: int(r.Total),
				CacheReadTokens: int(r.CacheRead), CacheWriteTokens: int(r.CacheWrite),
				Calls: int(r.Calls),
			},
		})
	}
	seats := make([]tokens.SeatDay, 0, len(turns))
	for _, r := range turns {
		seats = append(seats, tokens.SeatDay{
			Day: r.Day, AgentID: r.AgentID, Handle: r.Handle, Role: r.Role,
			Turns: int(r.Turns), Failed: int(r.Failed),
		})
	}
	if w.seatOnly() {
		// AN AGENT SEAT'S WINDOW holds no person's day, and an empty
		// Person asks for every one.
		return cells, seats, nil
	}
	return s.personCells(ctx, w, q, cells, seats)
}

// personCells adds to a window's cells EVERY PERSON'S, which no seat's row
// carries: what the auxiliary model spent for a human seat — a question on the
// operator surface — named by the seat's handle, so a window narrowed to that
// handle is the person's alone.
func (s Sources) personCells(ctx context.Context, w spendWindow, q usage.SpendQuery,
	cells []tokens.Cell, seats []tokens.SeatDay) ([]tokens.Cell, []tokens.SeatDay, error) {

	people, err := usage.PersonSpend(ctx, s.Usage, usage.PersonQuery{
		From: q.From, To: q.To, Person: w.Person,
	})
	if err != nil {
		return nil, nil, usageErr(err)
	}
	for _, r := range people {
		cells = append(cells, tokens.Cell{
			Day: r.Day, Handle: r.Person, Role: r.Role, Person: true,
			Phase: r.Phase, Worker: r.Worker, Model: r.Model, ProviderKey: r.ProviderKey,
			Bucket: tokens.Bucket{
				InputTokens: int(r.Input), OutputTokens: int(r.Output), TotalTokens: int(r.Total),
				CacheReadTokens: int(r.CacheRead), CacheWriteTokens: int(r.CacheWrite),
				Calls: int(r.Calls),
			},
		})
	}
	return cells, seats, nil
}

// usageErr is a usage read's failure as this surface reports it: an estate
// that is not open YET — an adoption swaps the replicated file between a
// rename and a reopen — is "not available, retry", never a broken server.
func usageErr(err error) error {
	if errors.Is(err, store.ErrNoEstate) {
		return fmt.Errorf("%w: the replicated estate is being reopened: %w", ErrUnavailable, err)
	}
	return err
}

// tokens answers the spend breakdown.
//
// The LIVE window — no `days`, no dates, no seat, no `previous` — is the
// projection's, answered from memory: a page load that read the estate would
// put a query on the critical path of every tab, for an answer already held.
// Anything else names company days, and is read from the usage domain.
func (s Sources) tokens(ctx context.Context, p Params) (any, error) {
	rollup, err := s.rollup(ctx, p)
	if err != nil || readsConfig(ctx) {
		return rollup, err
	}
	return rollup.WithoutEntries(), nil
}

// readsConfig reports whether the asking principal may read the company's
// configuration (an admin): which providers.llm entries it configures, and so
// what each of them served. A spend answer is everybody's; its per-entry half
// is not.
func readsConfig(ctx context.Context) bool {
	principal, how := iam.From(ctx)
	return how == iam.Resolved && principal.Can(iam.GrantAdmin)
}

// rollup is the spend breakdown [Sources.tokens] answers, whole.
func (s Sources) rollup(ctx context.Context, p Params) (tokens.Rollup, error) {
	if !namesAWindow(p) {
		// The window this rollup actually covers, reported rather than
		// assumed: the projection ages its window on its own clock and
		// names the two instants it cut it at.
		window := s.State.Spend()
		return tokens.Aggregate(window.Records, tokens.Options{
			Seats:       s.Seats(),
			Since:       window.Since,
			Until:       window.Until,
			RecentTurns: Clamp(p.Int("recent_turns", 0), tokens.DefaultRecentTurns, tokens.MaxRecentTurns),
		}), nil
	}
	w, err := s.spendWindowOf(p)
	if err != nil {
		return tokens.Rollup{}, err
	}
	opts := tokens.DailyOptions{Range: w.Range, AgentID: w.AgentID, Seats: s.Seats(), Horizon: w.Horizon}
	if s.Usage == nil {
		// A registry wired without the usage domain (a caller asking only
		// the projection's questions) cannot see this window. The honest
		// answer is an EMPTY rollup labelled with the window asked for, not
		// the live one relabelled, which would put a week's heading over a
		// day's numbers.
		return w.label(tokens.FoldDaily(nil, nil, opts)), nil
	}
	cells, seats, err := s.spendCells(ctx, w)
	if err != nil {
		return tokens.Rollup{}, err
	}
	return w.label(tokens.FoldDaily(cells, seats, opts)), nil
}

// label names the person a window was narrowed to on its rollup, which the
// fold names only by an agent id ([tokens.Rollup.Seat] is the seat's handle
// now, and a person's human seat has no agent id to look it up by). Its
// AgentID stays absent: a person has none, and an id here would be a link to
// a seat page no agent holds.
func (w spendWindow) label(r tokens.Rollup) tokens.Rollup {
	if w.personOnly() {
		r.Seat = w.Person
	}
	return r
}

// tokenSeries answers the spend WITH A TIME AXIS.
//
// `tokens` answers what a window cost, split six ways, and every one of its
// rows is a sum over the whole window — so it cannot say when. That is the
// question a cost explorer is for: a total with no shape is a number nobody
// can act on, and the shape is what a spike, a runaway loop or a quiet weekend
// looks like.
//
// A SECOND QUESTION rather than a flag on the first, because the two answers
// have different shapes: one is a breakdown and one is a series, and a query
// that returned either depending on a parameter is a client branch at every
// call site. It has no live path: its buckets are company days and ISO weeks,
// which only the usage domain holds.
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
	if group.AdminOnly() && !readsConfig(ctx) {
		// ITS BANDS ARE THE COMPANY'S ENTRIES, which only an admin reads
		// (see [Sources.tokens]); every other grouping is everybody's.
		return nil, NewRefusal("token_series by "+string(group), authz.Decision{
			Reason: authz.ReasonNoGrant, Grants: []iam.Grant{iam.GrantAdmin}})
	}
	interval := tokens.Interval(p.String("bucket"))
	if interval == "" {
		interval = tokens.IntervalDay
	}
	if !interval.Valid() {
		return nil, fmt.Errorf("%w: bucket must be one of %v, and %q is not — a "+
			"company day is the finest thing every data node's usage agrees on",
			ErrBadParams, tokens.Intervals, interval)
	}
	w, err := s.spendWindowOf(p)
	if err != nil {
		return nil, err
	}
	cells, _, err := s.spendCells(ctx, w)
	if err != nil {
		return nil, err
	}
	return tokens.BucketDaily(cells, tokens.SeriesOptions{
		Group: group, Interval: interval, Range: w.Range,
		Groups:  Clamp(p.Int("groups", 0), tokens.DefaultSeriesGroups, tokens.MaxSeriesGroups),
		Seats:   s.Seats(),
		AgentID: w.AgentID,
		Horizon: w.Horizon,
	}), nil
}

// Seats is the running organisation as the spend rollups name seats: every
// agent seat by its agent id and every human seat by its [tokens.PersonKey],
// each with its handle, its name and the unit it sits in directly. Empty when
// this node runs no company, which names and links nothing rather than
// guessing.
//
// KEYED BY AGENT ID, the one identifier a spend record and a usage row carry
// that neither a rename nor a namesake can move. It was keyed by role name,
// which two seats may share: both of their rows linked to whichever seat the
// map was built from last, and a per-unit band keyed on a unit's NAME pooled
// two teams that shared one.
//
// Exported because the live stream needs the same directory for the rollup it
// pushes: two derivations of it is how a pushed row and a queried one come to
// link to different pages.
func (s Sources) Seats() tokens.Seats {
	out := tokens.Seats{}
	organization := s.organization()
	if organization == nil {
		return out
	}
	// EVERY SEAT THE COMPANY RUNS: a walk of the document's top-level
	// list missed every seat inside a unit.
	for role := range organization.AllRoles() {
		seat := tokens.Seat{Handle: role.Handle(), Name: role.Name}
		// Only the DIRECT unit, not the chain: a band per nesting level
		// would count the same spend once for the team and again for the
		// department above it, and a stacked chart whose bands sum to more
		// than the total is unreadable.
		if unit := organization.UnitFor(role); unit != nil {
			seat.UnitKey, seat.UnitName = unit.Key(), unit.Name
		}
		// A HUMAN SEAT UNDER ITS PERSON KEY: a person's spend names the
		// human seat it was made for, and a human seat has no agent id —
		// so without this row a person's spend printed under whatever
		// name its newest day carried and never under the seat's own.
		if role.IsHuman() {
			out[tokens.PersonKey(role.Handle())] = seat
			continue
		}
		id, ok := organization.AgentIDFor(role)
		if !ok {
			continue
		}
		out[id.String()] = seat
	}
	return out
}
