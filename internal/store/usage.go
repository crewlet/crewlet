package store

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// What one node's seats and schedules did in one company day, read from this
// node's own records — the INPUT to the usage domain's publisher.
//
// # Why this is here and the record is not
//
// The rows are the node estate's: the audit log this node's own events were
// written to, and the dispatch ledger its scheduler claimed fires in. Nothing
// here reads the replicated estate, and nothing here decides what a record
// looks like — [internal/usage] folds these values into one record per
// (node, day, object) and publishes it, and every node's applier writes it.
// Keeping the SQL here is what lets the estate boundary's static walk see it,
// and keeping the record there is what keeps this package from knowing about
// a stream.
//
// # A day is a half-open instant range
//
// The caller cuts it on the company's clock ([internal/period]) and hands the
// two instants in. Every read below is `>= start AND < end` on its own time
// column, so a moment at the boundary belongs to exactly one day and two
// adjacent days never count one event twice.

// UsageWindow is one company day as the reads here take it, with the two
// review decisions the turn statistics are stated in.
//
// THE DECISIONS ARE ARGUMENTS rather than literals here, because the phase
// vocabulary belongs to the turn loop and this package must not import it: a
// spelling written here is the one place a wire value would silently stop
// matching, which is how [phaseCompleted] and its siblings came to be taken
// from their payload types rather than typed.
type UsageWindow struct {
	Start, End time.Time

	// Accepted is the reviewer decision that ends a turn well, and SentBack
	// the one that loops it back with a correction.
	Accepted, SentBack string
}

// UsageDay is everything a node's store says about one day.
type UsageDay struct {
	// Seats are the seats with any spend, turn or read in the window,
	// ordered by agent id.
	Seats []UsageSeat

	// Schedules are the schedules this node's scheduler fired in the
	// window, ordered by scope and name.
	Schedules []UsageSchedule
}

// UsageSeat is one seat's day on this node.
type UsageSeat struct {
	AgentID string

	// Handle and Role name the seat as that day's own records did. Either
	// may be empty — a day with only phase records carries a role and no
	// handle — and the publisher fills the handle from the organisation
	// where it can.
	Handle string
	Role   string

	Tokens []UsageTokens
	Turns  UsageTurns
	Reads  []UsageRead
}

// UsageTokens is one (phase, worker, model, provider key) cell of a seat's
// spend: what it cost and how many completed calls spent it.
type UsageTokens struct {
	Phase, Worker, Model, ProviderKey string

	Input, Output, CacheRead, CacheWrite, Total, Calls int64
}

// UsageTurns is a seat's turns that ENDED in the window on this node.
//
// A turn belongs to the day its final completion falls in — the one record
// that is not a suspension — because that is when it stopped costing time and
// started being a result. A turn still parked on a coding run has ended
// nowhere yet and counts nowhere.
type UsageTurns struct {
	// Count is the turns that ended; Failed the ones whose final summary
	// says they failed.
	Count, Failed int64

	// Reviewed is the turns a reviewer judged at least once, FirstPass the
	// ones whose every review accepted — which, since an acceptance ends
	// the loop, is the ones accepted by their first — and SentBack is the
	// number of REVIEWS that sent work back, so a turn returned twice
	// counts two.
	Reviewed, FirstPass, SentBack int64

	// Durations is each ended turn's own measurement, every segment
	// summed, in the order the turns ended.
	Durations []time.Duration

	// LastEndedAt is the newest end in the window, zero when none ended.
	LastEndedAt time.Time
}

// UsageRead is one (backend, page, via) a seat reached in the window, with
// the newest such read's context.
type UsageRead struct {
	Backend, PageID, Via string

	Count  int64
	LastAt time.Time

	LastTurnID, LastWorkKey, LastQuery string
}

// UsageSchedule is one schedule's fires on this node in the window.
type UsageSchedule struct {
	ScopeType, ScopeID, Name string

	Fires []UsageFire
}

// UsageFire is one dispatch the scheduler recorded.
type UsageFire struct {
	At time.Time

	// Target is the seat it was dispatched to, empty for a catchup that
	// was skipped and so resolved no runner; Outcome is the dispatch
	// ledger's own word for it.
	Target, Outcome string

	// TraceID is the trace the fire was published under, and TurnID the
	// turn this node ran under that trace — empty where the turn ran on a
	// peer, whose event log this node cannot read. The trace names it in
	// either case.
	TraceID, TurnID string
}

// UsageMark is a cheap fingerprint of what a day holds, which is how the
// publisher tells a day that moved from one that did not without deriving it.
//
// A COUNT AND A NEWEST INSTANT over each source, because either alone misses
// a case: a count cannot see a row replaced by another, and a newest instant
// cannot see a late row written with an earlier timestamp.
type UsageMark struct {
	Events, LastEvent int64
	Fires, LastFire   int64
}

// usageEventTypes are the four records a day is derived from, taken from their
// payload types for the reason [phaseCompleted] is.
func usageEventTypes() []any {
	return []any{
		phaseCompleted,
		turnCompleted,
		types.AgentTurnCompleted{}.EventType(),
		types.KnowledgeRead{}.EventType(),
	}
}

// UsageMark fingerprints a window.
func (d *DB) UsageMark(ctx context.Context, w UsageWindow) (UsageMark, error) {
	if d == nil || d.sql == nil {
		return UsageMark{}, ErrNoEstate
	}
	var m UsageMark
	kinds := usageEventTypes()
	holders := strings.TrimSuffix(strings.Repeat("?,", len(kinds)), ",")
	args := append(slices.Clone(kinds), EncodeTime(w.Start), EncodeTime(w.End))
	if err := d.sql.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(MAX(event_time), 0)
		  FROM crewlet_events
		 WHERE event_type IN (`+holders+`)
		   AND event_time >= ? AND event_time < ?`, args...).
		Scan(&m.Events, &m.LastEvent); err != nil {
		return UsageMark{}, fmt.Errorf("store: fingerprint the usage day's events: %w", err)
	}
	if err := d.sql.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(MAX(fired_at), 0)
		  FROM scheduled_runs
		 WHERE fired_at >= ? AND fired_at < ?`,
		EncodeTime(w.Start), EncodeTime(w.End)).Scan(&m.Fires, &m.LastFire); err != nil {
		return UsageMark{}, fmt.Errorf("store: fingerprint the usage day's fires: %w", err)
	}
	return m, nil
}

// UsageForDay derives one window's usage from this node's records.
//
// Deterministic for a given store: every collection comes back in a stable
// order, so two derivations of an unchanged day are equal value for value —
// which is what lets the publisher compare a derivation against the last one
// it published and publish nothing when they agree.
func (d *DB) UsageForDay(ctx context.Context, w UsageWindow) (UsageDay, error) {
	if d == nil || d.sql == nil {
		return UsageDay{}, ErrNoEstate
	}
	if !w.Start.Before(w.End) {
		return UsageDay{}, fmt.Errorf("store: a usage window must end after it "+
			"starts, and %s..%s does not", w.Start, w.End)
	}
	seats := map[string]*UsageSeat{}
	seat := func(id string) *UsageSeat {
		s, ok := seats[id]
		if !ok {
			s = &UsageSeat{AgentID: id}
			seats[id] = s
		}
		return s
	}
	name := func(s *UsageSeat, handle, role string) {
		if s.Handle == "" {
			s.Handle = handle
		}
		if s.Role == "" {
			s.Role = role
		}
	}

	if err := d.usageTokens(ctx, w, seat, name); err != nil {
		return UsageDay{}, err
	}
	if err := d.usageTurns(ctx, w, seat, name); err != nil {
		return UsageDay{}, err
	}
	if err := d.usageReads(ctx, w, seat, name); err != nil {
		return UsageDay{}, err
	}
	schedules, err := d.usageSchedules(ctx, w)
	if err != nil {
		return UsageDay{}, err
	}

	out := UsageDay{Schedules: schedules}
	for _, id := range slices.Sorted(maps.Keys(seats)) {
		out.Seats = append(out.Seats, *seats[id])
	}
	return out, nil
}

// usageTokens folds the window's phase completions into (phase, worker, model,
// provider key) cells per seat, off the columns node/0015 and node/0030
// promoted — never the payload.
func (d *DB) usageTokens(ctx context.Context, w UsageWindow,
	seat func(string) *UsageSeat, name func(*UsageSeat, string, string)) error {

	rows, err := d.sql.QueryContext(ctx, `
		SELECT agent_id, MAX(agent_role), phase, worker, model, provider_key,
		       SUM(input_tokens), SUM(output_tokens),
		       SUM(cache_read_tokens), SUM(cache_write_tokens),
		       SUM(total_tokens), COUNT(*)
		  FROM crewlet_events
		 WHERE event_type = ? AND event_time >= ? AND event_time < ?
		   AND agent_id != ''
		 GROUP BY agent_id, phase, worker, model, provider_key
		 ORDER BY agent_id, phase, worker, model, provider_key`,
		phaseCompleted, EncodeTime(w.Start), EncodeTime(w.End))
	if err != nil {
		return fmt.Errorf("store: read the usage day's spend: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			id, role string
			t        UsageTokens
		)
		if err := rows.Scan(&id, &role, &t.Phase, &t.Worker, &t.Model, &t.ProviderKey,
			&t.Input, &t.Output, &t.CacheRead, &t.CacheWrite, &t.Total, &t.Calls); err != nil {
			return fmt.Errorf("store: scan the usage day's spend: %w", err)
		}
		s := seat(id)
		name(s, "", role)
		s.Tokens = append(s.Tokens, t)
	}
	return rows.Err()
}

// usageTurns reads the turns that ENDED in the window and folds each one's
// records — wherever in time they fall — into its statistics.
//
// TWO STATEMENTS rather than a join, because the second one's rows are not in
// the window: a turn that ended at 00:05 reviewed at 23:58 the day before, and
// its phases are found by its id rather than by the day. Both are index seeks
// — (event_type, event_time) for the first, (turn_id, …) for the second.
func (d *DB) usageTurns(ctx context.Context, w UsageWindow,
	seat func(string) *UsageSeat, name func(*UsageSeat, string, string)) error {

	type ended struct {
		agent, handle, role string
		at                  int64
	}
	endedTurns := map[string]ended{}
	rows, err := d.sql.QueryContext(ctx, `
		SELECT turn_id, MAX(agent_id), MAX(event_time),
		       MAX(COALESCE(json_extract(payload, '$.agent_handle'), '')),
		       MAX(agent_role)
		  FROM crewlet_events
		 WHERE event_type = ? AND event_time >= ? AND event_time < ?
		   AND turn_id != '' AND agent_id != '' AND `+suspendedExpr+` = 0
		 GROUP BY turn_id`,
		turnCompleted, EncodeTime(w.Start), EncodeTime(w.End))
	if err != nil {
		return fmt.Errorf("store: read the usage day's ended turns: %w", err)
	}
	for rows.Next() {
		var id string
		var e ended
		if scanErr := rows.Scan(&id, &e.agent, &e.at, &e.handle, &e.role); scanErr != nil {
			_ = rows.Close()
			return fmt.Errorf("store: scan the usage day's ended turns: %w", scanErr)
		}
		endedTurns[id] = e
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if len(endedTurns) == 0 {
		return nil
	}

	// EVERY RECORD OF EACH ENDED TURN, by id: the duration of every
	// segment, whether its final summary failed, and what its reviews
	// decided. The phase and the decision are the review record's own.
	review := string(types.PhaseReview)
	rows, err = d.sql.QueryContext(ctx, `
		SELECT turn_id,
		       SUM(CASE WHEN event_type = ?
		                THEN COALESCE(json_extract(payload, '$.duration_ms'), 0)
		                ELSE 0 END),
		       MAX(CASE WHEN event_type = ? AND `+suspendedExpr+` = 0
		                 AND COALESCE(json_extract(payload, '$.failed'), 0) = 1
		                THEN 1 ELSE 0 END),
		       SUM(CASE WHEN event_type = ? AND phase = ? THEN 1 ELSE 0 END),
		       SUM(CASE WHEN event_type = ? AND phase = ?
		                 AND COALESCE(json_extract(payload, '$.decision'), '') = ?
		                THEN 1 ELSE 0 END),
		       SUM(CASE WHEN event_type = ? AND phase = ?
		                 AND COALESCE(json_extract(payload, '$.decision'), '') != ?
		                THEN 1 ELSE 0 END)
		  FROM crewlet_events
		 WHERE turn_id IN (
		       SELECT turn_id FROM crewlet_events
		        WHERE event_type = ? AND event_time >= ? AND event_time < ?
		          AND turn_id != '' AND `+suspendedExpr+` = 0)
		 GROUP BY turn_id
		 ORDER BY turn_id`,
		turnCompleted,
		types.AgentTurnCompleted{}.EventType(),
		phaseCompleted, review,
		phaseCompleted, review, w.SentBack,
		phaseCompleted, review, w.Accepted,
		turnCompleted, EncodeTime(w.Start), EncodeTime(w.End))
	if err != nil {
		return fmt.Errorf("store: read the usage day's turn records: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type folded struct {
		id                  string
		durationMS          int64
		failed              int64
		reviews, sentBack   int64
		notAccepted         int64
		endedAt             int64
		agent, handle, role string
	}
	var turns []folded
	for rows.Next() {
		var f folded
		if err := rows.Scan(&f.id, &f.durationMS, &f.failed, &f.reviews,
			&f.sentBack, &f.notAccepted); err != nil {
			return fmt.Errorf("store: scan the usage day's turn records: %w", err)
		}
		e := endedTurns[f.id]
		f.endedAt, f.agent, f.handle, f.role = e.at, e.agent, e.handle, e.role
		turns = append(turns, f)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// IN THE ORDER THEY ENDED, ties broken by id, so the durations list is
	// the same list on every derivation.
	slices.SortFunc(turns, func(a, b folded) int {
		if a.endedAt != b.endedAt {
			if a.endedAt < b.endedAt {
				return -1
			}
			return 1
		}
		return strings.Compare(a.id, b.id)
	})
	for _, f := range turns {
		s := seat(f.agent)
		name(s, f.handle, f.role)
		s.Turns.Count++
		s.Turns.Failed += f.failed
		if f.reviews > 0 {
			s.Turns.Reviewed++
			if f.notAccepted == 0 {
				s.Turns.FirstPass++
			}
		}
		s.Turns.SentBack += f.sentBack
		s.Turns.Durations = append(s.Turns.Durations,
			time.Duration(f.durationMS)*time.Millisecond)
		if at := DecodeTime(f.endedAt); at.After(s.Turns.LastEndedAt) {
			s.Turns.LastEndedAt = at
		}
	}
	return nil
}

// usageReads folds the window's knowledge reads into one row per (backend,
// page, via) per seat.
//
// ONE ROW PER PAGE A READ REACHED, through `json_each` over the record's own
// page list, because a search that surfaced six pages is one read of each of
// six pages — and folding in Go rather than in a GROUP BY is what lets the
// newest read's turn, work key and query travel with the count without a
// window function.
func (d *DB) usageReads(ctx context.Context, w UsageWindow,
	seat func(string) *UsageSeat, name func(*UsageSeat, string, string)) error {

	rows, err := d.sql.QueryContext(ctx, `
		SELECT e.agent_id,
		       COALESCE(json_extract(e.payload, '$.agent_handle'), ''),
		       e.agent_role,
		       COALESCE(json_extract(e.payload, '$.backend'), ''),
		       COALESCE(json_extract(e.payload, '$.via'), ''),
		       COALESCE(json_extract(e.payload, '$.turn_id'), ''),
		       COALESCE(json_extract(e.payload, '$.work_key'), ''),
		       COALESCE(json_extract(e.payload, '$.query'), ''),
		       e.event_time,
		       COALESCE(json_extract(p.value, '$.id'), '')
		  FROM crewlet_events e, json_each(e.payload, '$.pages') p
		 WHERE e.event_type = ? AND e.event_time >= ? AND e.event_time < ?
		   AND e.agent_id != ''
		 ORDER BY e.event_time, e.event_id`,
		types.KnowledgeRead{}.EventType(), EncodeTime(w.Start), EncodeTime(w.End))
	if err != nil {
		return fmt.Errorf("store: read the usage day's knowledge reads: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type key struct{ agent, backend, page, via string }
	folded := map[key]*UsageRead{}
	for rows.Next() {
		var (
			agent, handle, role, backend, via string
			turn, workKey, query, page        string
			at                                int64
		)
		if err := rows.Scan(&agent, &handle, &role, &backend, &via, &turn,
			&workKey, &query, &at, &page); err != nil {
			return fmt.Errorf("store: scan the usage day's knowledge reads: %w", err)
		}
		if page == "" || via == "" {
			// A PAGE NOTHING CAN ADDRESS is not a page any reader can
			// count, which is the producer's own rule; a row that
			// slipped past it is skipped rather than keyed on nothing.
			continue
		}
		s := seat(agent)
		name(s, handle, role)
		k := key{agent, backend, page, via}
		r, ok := folded[k]
		if !ok {
			r = &UsageRead{Backend: backend, PageID: page, Via: via}
			folded[k] = r
		}
		r.Count++
		// ORDERED BY TIME, so the last row seen is the newest read.
		r.LastAt = DecodeTime(at)
		r.LastTurnID, r.LastWorkKey, r.LastQuery = turn, workKey, query
	}
	if err := rows.Err(); err != nil {
		return err
	}
	keys := slices.SortedFunc(maps.Keys(folded), func(a, b key) int {
		return strings.Compare(a.agent+"\x00"+a.backend+"\x00"+a.page+"\x00"+a.via,
			b.agent+"\x00"+b.backend+"\x00"+b.page+"\x00"+b.via)
	})
	for _, k := range keys {
		s := seat(k.agent)
		s.Reads = append(s.Reads, *folded[k])
	}
	return nil
}

// usageSchedules reads the fires this node's scheduler recorded in the window.
//
// THE TURN IS FOUND BY THE TRACE, and only where this node ran it: a fire is
// dispatched to a seat's mailbox and whichever node holds that seat runs the
// turn, so a turn on a peer is in the peer's event log. The trace names it
// either way.
func (d *DB) usageSchedules(ctx context.Context, w UsageWindow) ([]UsageSchedule, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT r.scope_type, r.scope_id, r.schedule_name, r.target_handle,
		       r.fired_at, r.outcome, r.trace_id,
		       COALESCE((SELECT e.turn_id FROM crewlet_events e
		                  WHERE r.trace_id != '' AND e.trace_id = r.trace_id
		                    AND e.turn_id != ''
		                  ORDER BY e.event_time, e.event_id LIMIT 1), '')
		  FROM scheduled_runs r
		 WHERE r.fired_at >= ? AND r.fired_at < ?
		 ORDER BY r.scope_type, r.scope_id, r.schedule_name, r.fired_at, r.target_handle`,
		EncodeTime(w.Start), EncodeTime(w.End))
	if err != nil {
		return nil, fmt.Errorf("store: read the usage day's schedule fires: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []UsageSchedule
	for rows.Next() {
		var (
			scopeType, scopeID, name string
			f                        UsageFire
			at                       int64
		)
		if err := rows.Scan(&scopeType, &scopeID, &name, &f.Target, &at,
			&f.Outcome, &f.TraceID, &f.TurnID); err != nil {
			return nil, fmt.Errorf("store: scan the usage day's schedule fires: %w", err)
		}
		f.At = DecodeTime(at)
		if n := len(out); n > 0 && out[n-1].ScopeType == scopeType &&
			out[n-1].ScopeID == scopeID && out[n-1].Name == name {
			out[n-1].Fires = append(out[n-1].Fires, f)
			continue
		}
		out = append(out, UsageSchedule{ScopeType: scopeType, ScopeID: scopeID,
			Name: name, Fires: []UsageFire{f}})
	}
	return out, rows.Err()
}
