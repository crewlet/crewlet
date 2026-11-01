package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// What one node's seats, its schedules and the people its auxiliary model spent
// for did in one company day, read from this node's own records — the INPUT to
// the usage domain's publisher.
//
// # Why this is here and the record is not
//
// The rows are the node estate's: the audit log this node's own events were
// written to, and the dispatch ledger its scheduler claimed fires in. Nothing
// here reads the replicated estate, and nothing here decides what a record
// looks like — [internal/usage] folds these values into one record per
// (node, day, object) and publishes it, and every data node's applier writes
// it.
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
//
// # A day counts the rows this node KEEPS
//
// A data node's log also holds the custody batches of nodes without `data`
// (internal/observe), and a batch it has written and not yet settled may be
// another node's to keep: until it settles, two logs hold its rows. Every
// count here therefore leaves out the rows of a batch in flight ([keptRowSQL]) —
// the PUBLISHED half of the rule every count in this package obeys (see
// unsettled.go) — so what a node publishes is what it keeps and a fleet's sum
// holds every row once.
//
// # A row changes only the day it falls on
//
// Every COUNT of a day is read off rows INSIDE it: a spend cell, a read and a
// person's spend off their own records, and a turn's statistics off the ONE
// record that ends it, which carries the whole turn ([types.TurnTally]) — its
// reviews, its send-backs and every segment's duration, wherever and on
// whichever node those happened. So a batch settled as kept makes its rows
// count on the days they fall on and on no other day: today and yesterday see
// it in their fingerprints, whose count it moves, and an older day through the
// mark [EventLog.SettleCustody] lowers in the settle's own transaction
// ([DB.UsageRederive]). A batch settled as NOT kept loses rows no count ever
// held: it changes no count of any day, and at most a fire's label (below).
//
// TWO EXCEPTIONS, each stated where it is read. A turn whose ending record
// carries NO tally — ended by a build before [types.TurnTally], or by a resume
// of a turn such a build parked, whose earlier segments nobody measured — is
// read off its records by its id, as it always was ([DB.usageLegacyTurns]),
// and those reach back to wherever the turn STARTED — days, since a park lasts
// as long as its executor, which nothing stops on a timer, or as long as a
// person takes to answer: so for such a turn a row changes the day the turn
// ended as well as its own. A kept settle that lands only that earlier row
// marks the earlier day, and the publisher derives every day from a mark
// through the day before yesterday — so an ending day older than that is
// derived with it, and one that is still today or yesterday is not until its
// own fingerprint next moves: a residual of reading the old shape, which ends
// with the turns it covers. And the event log's sweep can remove the earliest
// of those records while the end's day is still inside its read floor, so a
// later derivation reads the turn shorter, with fewer reviews; the publisher
// publishes every day joined with what the fleet already holds of it, which
// keeps what the earlier derivation counted (internal/usage).
//
// And a schedule fire's turn LABEL ([DB.usageSchedules]) is read off any row
// of the fire's trace, at any time, kept or in flight, and is never counted,
// fingerprinted or marked: a custody write can set it and a not-kept settle
// clear it without the day being derived again, so a derivation in between
// may publish a label a later one drops.

// UsageWindow is one company day as the reads here take it: a half-open
// instant range the caller cut on the company's clock, with the two review
// decisions a turn's statistics are counted in where its ending record states
// none ([DB.usageLegacyTurns]).
//
// THE DECISIONS ARE ARGUMENTS rather than literals here, because the phase
// vocabulary belongs to the turn loop and this package must not import it: a
// spelling written here is the one place a wire value would silently stop
// matching, which is how [phaseCompleted] and its siblings came to be taken
// from their payload types rather than typed. [DB.UsageForDay] refuses a
// window without them, since an empty one counts every review as unaccepted
// and none as a send-back.
type UsageWindow struct {
	Start, End time.Time

	// Accepted is the reviewer decision that ends a turn well, and SentBack
	// the one that loops it back with a correction. Read only for a turn
	// whose ending record carries no [types.TurnTally].
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

	// People are the PEOPLE the auxiliary model spent for in the window —
	// a person's questions on the operator surface, a pass on a unit a
	// person leads — ordered by handle. A person is no seat: no agent id,
	// no turns, no reads, only spend.
	People []UsagePerson
}

// UsagePerson is one person's auxiliary spend on this node in one day: the
// handle of the human seat it was spent for, that seat's role as the records
// named it, and the cells.
type UsagePerson struct {
	Handle string
	Role   string
	Tokens []UsageTokens
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
// started being a result. A turn still parked on its executor has ended
// nowhere yet and counts nowhere. Every figure below is read off that one
// record ([types.TurnTally]), never off the turn's other records, which a
// custody group spreads over the data nodes a second at a time and a resume
// on another node leaves behind — except for a turn whose ending record
// states no tally, which is read off its records as it always was
// ([DB.usageLegacyTurns]).
type UsageTurns struct {
	// Count is the turns that ended; Failed the ones the record that ended
	// them states failed ([types.TurnTally].Failed), or — for a turn whose
	// end states no tally — whose final summary says they failed.
	Count, Failed int64

	// Reviewed is the turns a reviewer judged at least once, FirstPass the
	// ones whose every review accepted — which, since an acceptance ends
	// the loop, is the ones accepted by their first — and SentBack is the
	// number of REVIEWS that sent work back, so a turn returned twice
	// counts two.
	Reviewed, FirstPass, SentBack int64

	// Durations is each ended turn's own measurement, every segment
	// summed by the turn itself, in the order the turns ended.
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
// cannot see a late row written with an earlier timestamp. Over the rows this
// node KEEPS, as the day itself is: a custody batch settled as kept moves the
// count of every day its rows fall on, and those are the only days it changes
// (see the file doc) — so a settle that lands today or yesterday re-derives
// it through this, and one that lands on an older day through the mark.
type UsageMark struct {
	Events, LastEvent int64
	Fires, LastFire   int64
}

// usageEventTypes are the seven records a day is derived from, taken from their
// payload types for the reason [phaseCompleted] is: the phase completions, the
// collected launches and the auxiliary records the spend is summed from, the
// completions a turn's figures are read off, the turn's SUMMARY record
// (`agent_turn_completed`), and the knowledge reads. The auxiliary record is
// one of them so the fingerprint moves when it lands: a flush writes it up to
// one ledger interval after its last call, stamped with that call's instant,
// and a day whose only change was that row would otherwise never be
// re-derived. A collected launch is one for the reason every spend record is:
// it is the executor's spend, published when its box is read rather than when
// any phase of its turn completes. The summary record is one for the same
// reason as the auxiliary record: a turn whose ending record states no tally
// is read off its records ([DB.usageLegacyTurns]), whether it failed off the
// summary, and a summary row that lands in a later custody batch than its
// turn's end must move the fingerprint — and the settle's mark — when it does.
// The superseded fire's record is the seventh, for the auxiliary record's
// reason: it is what turns a fire's outcome from fired into skipped
// ([DB.usageSchedules]), it lands when the seat's node merges the fires (long
// after the scheduler wrote its row), and a day whose only change was that
// record would otherwise never be derived again.
func usageEventTypes() []any {
	return []any{
		phaseCompleted,
		launchSpendType,
		auxiliarySpendType,
		turnCompleted,
		types.AgentTurnCompleted{}.EventType(),
		types.KnowledgeRead{}.EventType(),
		types.ScheduledFireSuperseded{}.EventType(),
	}
}

// usageTypesIn is `event_type IN (…)` over [usageEventTypes], and its
// arguments.
func usageTypesIn() (string, []any) {
	kinds := usageEventTypes()
	holders := strings.TrimSuffix(strings.Repeat("?,", len(kinds)), ",")
	return "event_type IN (" + holders + ")", slices.Clone(kinds)
}

// usageMarkSQL is the statement [DB.UsageMark] fingerprints a window's events
// with, and its arguments.
//
// ONE BRANCH PER EVENT TYPE rather than `event_type IN (…)`, because the
// planner seeks only the type for an IN list and walks every row of each type
// the log holds, where an equality on the type seeks the window as well
// (TestEveryUsageCountSeeksItsWindow) — and this runs twice a tick.
func usageMarkSQL(w UsageWindow) (string, []any) {
	var branches []string
	var args []any
	for _, kind := range usageEventTypes() {
		branches = append(branches, `SELECT e.event_time FROM crewlet_events AS e
		 WHERE e.event_type = ? AND e.event_time >= ? AND e.event_time < ?
		   AND `+keptRowSQL)
		args = append(args, kind, EncodeTime(w.Start), EncodeTime(w.End))
	}
	return `SELECT COUNT(*), COALESCE(MAX(event_time), 0) FROM (
		` + strings.Join(branches, "\n\t\t UNION ALL ") + `)`, args
}

// UsageMark fingerprints a window.
func (d *DB) UsageMark(ctx context.Context, w UsageWindow) (UsageMark, error) {
	if d == nil || d.sql == nil {
		return UsageMark{}, ErrNoEstate
	}
	var m UsageMark
	query, args := usageMarkSQL(w)
	if err := d.sql.QueryRowContext(ctx, query, args...).
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

// UsageRederive is what custody settles ask of the usage publisher: derive
// every day from the one Since falls in again, because rows that count now
// were not counted when those days were last published.
type UsageRederive struct {
	// Since is the oldest instant of a usage row a settle has made count and
	// the publisher has not cleared.
	Since time.Time

	// Seq is the newest mark's sequence, which is what lets the publisher
	// clear exactly the marks it acted on ([DB.ClearUsageRederive]): a settle
	// that lands while it derives is stamped above it, and stays.
	Seq int64
}

// UsageRederive reads the marks: the oldest instant a kept custody settle has
// made count since the publisher last cleared what it read, and false when no
// settle has changed a day since then.
//
// THREE ANSWERS, because "nothing to re-derive" and "this node could not say"
// send the publisher opposite ways: on an error it clears nothing.
func (d *DB) UsageRederive(ctx context.Context) (UsageRederive, bool, error) {
	if d == nil || d.sql == nil {
		return UsageRederive{}, false, ErrNoEstate
	}
	var since, seq sql.NullInt64
	if err := d.sql.QueryRowContext(ctx,
		`SELECT MIN(since), MAX(seq) FROM usage_rederive`).Scan(&since, &seq); err != nil {
		return UsageRederive{}, false, fmt.Errorf("store: read the usage re-derive marks: %w", err)
	}
	if !since.Valid || !seq.Valid {
		return UsageRederive{}, false, nil
	}
	return UsageRederive{Since: DecodeTime(since.Int64), Seq: seq.Int64}, true, nil
}

// ClearUsageRederive clears every mark at or below seq — the newest the caller
// read and acted on — reporting whether it cleared any.
//
// AT OR BELOW, NEVER ALL: a settle can land between the read and the clear,
// and every settle stamps its mark above every mark there is, so the one that
// landed survives and the next pass derives its day. A plain delete would
// forget that day for good. And the marks it does clear go WHOLE, each with
// its own oldest instant: one row the whole table shared, lowered by a settle
// ten days back and then moved by every settle of today's rows after it,
// would survive every clear, and the publisher would derive the ten days again
// on every tick for as long as a keeper was taking batches. The publisher is the only
// thing that clears, one tick at a time, so between its read and its clear the
// marks it read can be moved by a settle but never removed — and a mark a
// settle moved is stamped above what was read, so this leaves it.
func (d *DB) ClearUsageRederive(ctx context.Context, seq int64) (bool, error) {
	if d == nil || d.sql == nil {
		return false, ErrNoEstate
	}
	res, err := d.sql.ExecContext(ctx, `DELETE FROM usage_rederive WHERE seq <= ?`, seq)
	if err != nil {
		return false, fmt.Errorf("store: clear the usage re-derive marks: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: clear the usage re-derive marks: %w", err)
	}
	return n > 0, nil
}

// markUsageRederiveSQL is the statement a kept settle reads the oldest usage
// row of its batch with: the batch's own in-flight identities, each a seek of
// the log — driven from them by the CROSS JOIN, as [unsettledSQL] is, so a
// settle never walks the log.
//
// OF A USAGE ROW ONLY: a batch of rows no day is derived from — a
// notification's outcome, a chat message — changes no day, and marking it
// would have the publisher derive every day back to it for nothing.
func markUsageRederiveSQL(batchID string) (string, []any) {
	types, kinds := usageTypesIn()
	return `SELECT MIN(e.event_time)
	  FROM custody_unsettled_rows AS u CROSS JOIN crewlet_events AS e
	 WHERE u.batch_id = ?
	   AND e.event_time = u.row_time
	   AND e.event_id = u.row_id
	   AND e.` + types, append([]any{batchID}, kinds...)
}

// usageMarkDay is a mark's row: the UTC day its oldest instant falls on, in
// the store's own microseconds.
const usageMarkDay = int64(24 * time.Hour / time.Microsecond)

// markUsageRederive marks the oldest usage row of a batch being settled as
// kept, on the settle's own transaction.
//
// ONTO THE ROW OF THE UTC DAY THAT ROW FALLS ON, lowering it, and STAMPED
// ABOVE EVERY MARK THERE IS: the stamp is what keeps a settle that lands while
// the publisher derives out of the clear that follows ([DB.ClearUsageRederive]),
// and the day is what keeps that settle from carrying an older settle's day
// with it — a settle of today's rows moves today's row and leaves last week's
// to be cleared. A row a day bounds the table by the days the log holds.
func markUsageRederive(ctx context.Context, tx *sql.Tx, batchID string) error {
	query, args := markUsageRederiveSQL(batchID)
	var since sql.NullInt64
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&since); err != nil {
		return fmt.Errorf("read its oldest usage row: %w", err)
	}
	if !since.Valid {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO usage_rederive (day, since, seq)
		VALUES (?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM usage_rederive))
		ON CONFLICT (day) DO UPDATE SET since = MIN(since, excluded.since),
		                                seq   = excluded.seq`,
		floorDiv(since.Int64, usageMarkDay), since.Int64); err != nil {
		return fmt.Errorf("mark the days it changed: %w", err)
	}
	return nil
}

// floorDiv is a / b rounded towards minus infinity, so an instant before the
// epoch falls on the day it falls on rather than the one after.
func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// UsageWatermark reads the oldest day the usage publisher still owes a
// derivation of on its own account — see [DB.SetUsageWatermark] — and false
// when it has never written one.
//
// THREE ANSWERS, for [DB.UsageRederive]'s reason: a publisher that could not
// read it must not write over it either, or the day it named is gone.
func (d *DB) UsageWatermark(ctx context.Context) (time.Time, bool, error) {
	if d == nil || d.sql == nil {
		return time.Time{}, false, ErrNoEstate
	}
	var since int64
	err := d.sql.QueryRowContext(ctx,
		`SELECT since FROM usage_watermark WHERE id = 1`).Scan(&since)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return time.Time{}, false, nil
	case err != nil:
		return time.Time{}, false, fmt.Errorf("store: read the usage watermark: %w", err)
	}
	return DecodeTime(since), true, nil
}

// SetUsageWatermark records the start of the oldest day the usage publisher
// has not confirmed published whole: the day before the newest it
// fingerprinted, or an older one a tick could not settle. Every day from it on
// is derived again when the publisher next starts, which is how the tail of
// the day a node stopped in — written after its last tick — reaches the fleet
// however long the node was down.
func (d *DB) SetUsageWatermark(ctx context.Context, since time.Time) error {
	if d == nil || d.sql == nil {
		return ErrNoEstate
	}
	if _, err := d.sql.ExecContext(ctx, `
		INSERT INTO usage_watermark (id, since) VALUES (1, ?)
		ON CONFLICT (id) DO UPDATE SET since = excluded.since`, EncodeTime(since)); err != nil {
		return fmt.Errorf("store: write the usage watermark: %w", err)
	}
	return nil
}

// UsageRepublished reads the usage log's generation on every data node, in the
// usage publisher's canonical spelling, as it last republished every day its
// event log holds under — and false when it never has.
//
// THREE ANSWERS, for [DB.UsageRederive]'s reason: a publisher that could not
// read what it last answered must not take a reanchor it has not answered for
// one it has, nor republish every day on every tick.
func (d *DB) UsageRepublished(ctx context.Context) (string, bool, error) {
	if d == nil || d.sql == nil {
		return "", false, ErrNoEstate
	}
	var generations string
	err := d.sql.QueryRowContext(ctx,
		`SELECT generations FROM usage_republished WHERE id = 1`).Scan(&generations)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("store: read the usage generations last republished under: %w", err)
	}
	return generations, true, nil
}

// SetUsageRepublished records the usage log's generation on every data node
// that the usage publisher has answered: every day its event log holds is
// published again under them — or, where they moved without any node
// re-anchoring, nothing needed publishing. See the migration that made the
// table for why it is kept across a restart.
func (d *DB) SetUsageRepublished(ctx context.Context, generations string) error {
	if d == nil || d.sql == nil {
		return ErrNoEstate
	}
	if _, err := d.sql.ExecContext(ctx, `
		INSERT INTO usage_republished (id, generations) VALUES (1, ?)
		ON CONFLICT (id) DO UPDATE SET generations = excluded.generations`, generations); err != nil {
		return fmt.Errorf("store: write the usage generations republished under: %w", err)
	}
	return nil
}

// UsageDayWindow reads the window this node's usage publisher first cut the
// company day `label` on ([DB.PinUsageDay]), and false when it has cut none.
//
// THREE ANSWERS, for [DB.UsageRederive]'s reason: a publisher that could not
// read the recorded cut must not cut the day afresh either, or two derivations
// of one label would count different instants.
func (d *DB) UsageDayWindow(ctx context.Context, label string) (UsageWindow, bool, error) {
	if d == nil || d.sql == nil {
		return UsageWindow{}, false, ErrNoEstate
	}
	var start, end int64
	err := d.sql.QueryRowContext(ctx,
		`SELECT start_at, end_at FROM usage_day_windows WHERE label = ?`, label).Scan(&start, &end)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return UsageWindow{}, false, nil
	case err != nil:
		return UsageWindow{}, false, fmt.Errorf("store: read the cut of usage day %s: %w", label, err)
	}
	return UsageWindow{Start: DecodeTime(start), End: DecodeTime(end)}, true, nil
}

// UsageDayContaining reads the recorded cut an instant falls in — its label
// and its window — and false when no recorded cut holds it.
//
// AT MOST ONE: every cut is recorded abutting its neighbours' ([DB.PinUsageDay]
// is handed it that way), so the recorded windows never overlap.
func (d *DB) UsageDayContaining(ctx context.Context, t time.Time) (string, UsageWindow, bool, error) {
	if d == nil || d.sql == nil {
		return "", UsageWindow{}, false, ErrNoEstate
	}
	var (
		label      string
		start, end int64
	)
	at := EncodeTime(t)
	err := d.sql.QueryRowContext(ctx, `
		SELECT label, start_at, end_at FROM usage_day_windows
		 WHERE start_at <= ? AND end_at > ?
		 ORDER BY start_at DESC LIMIT 1`, at, at).Scan(&label, &start, &end)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", UsageWindow{}, false, nil
	case err != nil:
		return "", UsageWindow{}, false, fmt.Errorf("store: read the usage day %s falls in: %w",
			t.UTC().Format(time.RFC3339), err)
	}
	return label, UsageWindow{Start: DecodeTime(start), End: DecodeTime(end)}, true, nil
}

// PinUsageDay records the window the company day `label` is cut on, unless
// one is recorded already, and answers whichever is: THE FIRST CUT WINS, so
// every later derivation of the label — on whatever clock the company keeps
// by then — counts the instants its first one did. See node/0050.
//
// The window is the caller's to choose; this keeps it. A window that does not
// end after it starts is refused rather than recorded.
func (d *DB) PinUsageDay(ctx context.Context, label string, w UsageWindow) (UsageWindow, error) {
	if d == nil || d.sql == nil {
		return UsageWindow{}, ErrNoEstate
	}
	if !w.Start.Before(w.End) {
		return UsageWindow{}, fmt.Errorf("store: usage day %s cut on %s..%s ends before "+
			"it starts", label, w.Start, w.End)
	}
	if _, err := d.sql.ExecContext(ctx, `
		INSERT INTO usage_day_windows (label, start_at, end_at) VALUES (?, ?, ?)
		ON CONFLICT (label) DO NOTHING`,
		label, EncodeTime(w.Start), EncodeTime(w.End)); err != nil {
		return UsageWindow{}, fmt.Errorf("store: record the cut of usage day %s: %w", label, err)
	}
	pinned, ok, err := d.UsageDayWindow(ctx, label)
	if err != nil {
		return UsageWindow{}, err
	}
	if !ok {
		return UsageWindow{}, fmt.Errorf("store: the cut of usage day %s was recorded and "+
			"does not read back", label)
	}
	return pinned, nil
}

// UsageForDay derives one window's usage from the records this node KEEPS
// (see the file doc).
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
	if w.Accepted == "" || w.SentBack == "" {
		return UsageDay{}, fmt.Errorf("store: a usage window must name the review " +
			"decisions a turn's statistics are counted in (Accepted, SentBack) — a " +
			"turn whose ending record states no tally is counted from its reviews, " +
			"and with none named every review reads as unaccepted")
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
	people, err := d.usagePeople(ctx, w)
	if err != nil {
		return UsageDay{}, err
	}

	out := UsageDay{Schedules: schedules, People: people}
	for _, id := range slices.Sorted(maps.Keys(seats)) {
		out.Seats = append(out.Seats, *seats[id])
	}
	return out, nil
}

// usageTokens folds the window's spend records — the phase completions, the
// collected launches and the auxiliary records — into (phase, worker, model,
// provider key) cells per seat, off the columns node/0015, node/0032 and
// node/0040 promoted, never the payload. A launch's cell is phase `execute`
// and an auxiliary record's is phase `auxiliary` with its purpose as the
// worker, so neither takes a cell shape of its own.
//
// CALLS ARE PROVIDER CALLS — the `calls` column's sum, not a count of rows —
// since one coalesced auxiliary row stands for many (node/0040).
//
// A SEAT'S, by its agent id: a person's auxiliary spend names no agent and is
// no seat's cell — it is [DB.usagePeople]'s.
func (d *DB) usageTokens(ctx context.Context, w UsageWindow,
	seat func(string) *UsageSeat, name func(*UsageSeat, string, string)) error {

	query, args := usageTokensSQL(w)
	rows, err := d.sql.QueryContext(ctx, query, args...)
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

// usageTokensSQL is [DB.usageTokens]'s statement and its arguments.
//
// ONE BRANCH PER SPEND TYPE, folded outside, for [usageMarkSQL]'s reason: an
// `event_type IN (…)` seeks only the type, and every older day the publisher
// derives again would walk thirty days of spend rows for one day's.
func usageTokensSQL(w UsageWindow) (string, []any) {
	kinds := spendTypes()
	var branches []string
	var args []any
	for _, kind := range kinds {
		branches = append(branches, `SELECT e.agent_id, e.agent_role, e.phase, e.worker,
		       e.model, e.provider_key, e.input_tokens, e.output_tokens,
		       e.cache_read_tokens, e.cache_write_tokens, e.total_tokens, e.calls
		  FROM crewlet_events AS e
		 WHERE e.event_type = ? AND e.event_time >= ? AND e.event_time < ?
		   AND e.agent_id != '' AND `+keptRowSQL)
		args = append(args, kind, EncodeTime(w.Start), EncodeTime(w.End))
	}
	return `
		SELECT agent_id, MAX(agent_role), phase, worker, model, provider_key,
		       SUM(input_tokens), SUM(output_tokens),
		       SUM(cache_read_tokens), SUM(cache_write_tokens),
		       SUM(total_tokens), SUM(calls)
		  FROM (` + strings.Join(branches, "\n\t\t UNION ALL ") + `)
		 GROUP BY agent_id, phase, worker, model, provider_key
		 ORDER BY agent_id, phase, worker, model, provider_key`, args
}

// usagePeople folds the window's auxiliary records spent for a PERSON into
// (phase, worker, model, provider key) cells per person, the cell shape a
// seat's spend has.
//
// A PERSON'S RECORD names no agent, and names the person as its envelope's
// ACTOR — a column already, so the person is grouped on without reading a
// payload; the person's role is the one payload key read, on these rows only,
// for [phaseTokenSQL]'s reason. Ordered by handle and cell, so an unchanged
// day derives identical records.
func (d *DB) usagePeople(ctx context.Context, w UsageWindow) ([]UsagePerson, error) {
	query, args := usagePeopleSQL(w)
	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read the usage day's people: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []UsagePerson
	for rows.Next() {
		var (
			handle, role string
			t            UsageTokens
		)
		if err := rows.Scan(&handle, &role, &t.Phase, &t.Worker, &t.Model, &t.ProviderKey,
			&t.Input, &t.Output, &t.CacheRead, &t.CacheWrite, &t.Total, &t.Calls); err != nil {
			return nil, fmt.Errorf("store: scan the usage day's people: %w", err)
		}
		if len(out) == 0 || out[len(out)-1].Handle != handle {
			out = append(out, UsagePerson{Handle: handle})
		}
		p := &out[len(out)-1]
		if p.Role == "" {
			p.Role = role
		}
		p.Tokens = append(p.Tokens, t)
	}
	return out, rows.Err()
}

// usagePeopleSQL is [DB.usagePeople]'s statement and its arguments.
func usagePeopleSQL(w UsageWindow) (string, []any) {
	return `
		SELECT e.actor, MAX(COALESCE(json_extract(e.payload, '$.actor_role'), '')),
		       e.phase, e.worker, e.model, e.provider_key,
		       SUM(e.input_tokens), SUM(e.output_tokens),
		       SUM(e.cache_read_tokens), SUM(e.cache_write_tokens),
		       SUM(e.total_tokens), SUM(e.calls)
		  FROM crewlet_events AS e
		 WHERE e.event_type = ? AND e.event_time >= ? AND e.event_time < ?
		   AND e.agent_id = '' AND e.actor != '' AND ` + keptRowSQL + `
		 GROUP BY e.actor, e.phase, e.worker, e.model, e.provider_key
		 ORDER BY e.actor, e.phase, e.worker, e.model, e.provider_key`,
		[]any{auxiliarySpendType, EncodeTime(w.Start), EncodeTime(w.End)}
}

// usageTurns reads the turns that ENDED in the window, each off the one record
// that ended it.
//
// THAT RECORD STATES THE WHOLE TURN ([types.TurnTally]): its every segment's
// duration, whether it failed, how many reviews it took and what they decided
// — counted by the turn as it ran, and carried across a park on the suspended
// conversation. So nothing here reads a tallied turn's other records, and that
// is the point: a stateless node's events reach the data nodes in custody
// batches a second apart, each kept by whichever node won its claim, and a
// turn resumed on another node leaves its first segment there — so the node
// holding the end held only part of the rest, counted its review count, its
// send-backs and its duration short, and the node holding the rest counted
// none of them. It also keeps every row a tallied turn's day is derived from
// INSIDE the day, which is what makes the event log's floor exact for it
// (internal/usage).
//
// A TURN WHOSE ENDING RECORD STATES NO TALLY is not a turn that measured zero:
// it is one ended by a build before the tally — every `turn_completed` already
// in a log at the upgrade, and every one a peer still on that build publishes
// during a rolling upgrade — or by a resume of a turn such a build parked,
// which cannot state the segments before the park (internal/engine leaves the
// tally off it). Read as zeros, every such turn would publish a zero-length,
// unreviewed, unfailed turn over the history the old build published, on the
// first derivation after the upgrade and for good. So such a turn is read off
// its records by its id, as the build before the tally read every turn
// ([DB.usageLegacyTurns]) — and those records reach back to wherever the turn
// started, which nothing bounds, so the sweep can remove the earliest of them
// while the end's day is inside the read floor and the turn then reads
// shorter here; the publisher's join with what the fleet holds keeps what an
// earlier derivation counted (internal/usage). Where one ending is recorded
// twice, once with a tally and once without, the tally is read.
func (d *DB) usageTurns(ctx context.Context, w UsageWindow,
	seat func(string) *UsageSeat, name func(*UsageSeat, string, string)) error {

	type ended struct {
		id                        string
		agent, handle, role       string
		at, durationMS            int64
		failed, reviews, sentBack int64
		unaccepted, tallied       int64
	}
	turns, err := func() ([]ended, error) {
		query, args := usageEndedTurnsSQL(w)
		rows, err := d.sql.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("store: read the usage day's ended turns: %w", err)
		}
		defer func() { _ = rows.Close() }()
		var out []ended
		for rows.Next() {
			var e ended
			if err := rows.Scan(&e.id, &e.agent, &e.at, &e.handle, &e.role, &e.durationMS,
				&e.failed, &e.reviews, &e.sentBack, &e.unaccepted, &e.tallied); err != nil {
				return nil, fmt.Errorf("store: scan the usage day's ended turns: %w", err)
			}
			out = append(out, e)
		}
		return out, rows.Err()
	}()
	if err != nil {
		return err
	}
	var untallied []string
	for _, e := range turns {
		if e.tallied == 0 {
			untallied = append(untallied, e.id)
		}
	}
	if len(untallied) > 0 {
		legacy, err := d.usageLegacyTurns(ctx, w, untallied)
		if err != nil {
			return err
		}
		for i := range turns {
			if turns[i].tallied != 0 {
				continue
			}
			// A TURN WITH NO KEPT RECORD of its own beyond the end keeps
			// the zeros the end's own row reads as — which is what the
			// build before the tally published for it too.
			f := legacy[turns[i].id]
			turns[i].durationMS, turns[i].failed = f.durationMS, f.failed
			turns[i].reviews, turns[i].sentBack = f.reviews, f.sentBack
			turns[i].unaccepted = f.unaccepted
		}
	}
	// IN THE ORDER THEY ENDED, ties broken by id, so the durations list is
	// the same list on every derivation.
	slices.SortFunc(turns, func(a, b ended) int {
		if a.at != b.at {
			if a.at < b.at {
				return -1
			}
			return 1
		}
		return strings.Compare(a.id, b.id)
	})
	for _, e := range turns {
		s := seat(e.agent)
		name(s, e.handle, e.role)
		s.Turns.Count++
		s.Turns.Failed += e.failed
		if e.reviews > 0 {
			s.Turns.Reviewed++
			if e.unaccepted == 0 {
				s.Turns.FirstPass++
			}
		}
		s.Turns.SentBack += e.sentBack
		s.Turns.Durations = append(s.Turns.Durations,
			time.Duration(e.durationMS)*time.Millisecond)
		if at := DecodeTime(e.at); at.After(s.Turns.LastEndedAt) {
			s.Turns.LastEndedAt = at
		}
	}
	return nil
}

// usageEndedTurnsSQL is [DB.usageTurns]'s statement — the turns that ended in
// the window, each with the whole-turn figures its ending record states and
// whether it states any — and its arguments.
//
// GROUPED BY TURN, the largest of each figure taken, because the one ending
// can be recorded twice — a resume re-run after a failure publishes its end
// again — and both copies state the same turn. TALLIED is whether ANY copy
// states the turn: a copy without one reads every figure as zero, so the
// largest is the copy that has one.
func usageEndedTurnsSQL(w UsageWindow) (string, []any) {
	return `
		SELECT e.turn_id, MAX(e.agent_id), MAX(e.event_time),
		       MAX(COALESCE(json_extract(e.payload, '$.agent_handle'), '')),
		       MAX(e.agent_role),
		       MAX(COALESCE(json_extract(e.payload, '$.turn.duration_ms'), 0)),
		       MAX(CASE WHEN COALESCE(json_extract(e.payload, '$.turn.failed'), 0)
		                THEN 1 ELSE 0 END),
		       MAX(COALESCE(json_extract(e.payload, '$.turn.reviews'), 0)),
		       MAX(COALESCE(json_extract(e.payload, '$.turn.sent_back'), 0)),
		       MAX(COALESCE(json_extract(e.payload, '$.turn.unaccepted'), 0)),
		       MAX(CASE WHEN json_type(e.payload, '$.turn') IS NULL THEN 0 ELSE 1 END)
		  FROM crewlet_events AS e
		 WHERE e.event_type = ? AND e.event_time >= ? AND e.event_time < ?
		   AND e.turn_id != '' AND e.agent_id != ''
		   AND COALESCE(json_extract(e.payload, '$.suspended'), 0) = 0
		   AND ` + keptRowSQL + `
		 GROUP BY e.turn_id`,
		[]any{turnCompleted, EncodeTime(w.Start), EncodeTime(w.End)}
}

// legacyTurn is one untallied turn's figures, folded from its records.
type legacyTurn struct {
	durationMS, failed, reviews, sentBack, unaccepted int64
}

// usageLegacyTurns folds the records of turns whose ending record states no
// tally into their statistics, keyed by turn id — the reading every turn had
// before [types.TurnTally], kept as a permanent reader for that shape (see
// [DB.usageTurns]).
//
// EVERY KEPT RECORD OF EACH TURN, by id and wherever in time it falls: the
// duration of every segment (each `turn_completed`, the parked ones included),
// whether its final summary failed, and what its reviews decided. A row of a
// batch in flight is left out ([keptRowSQL]), as from every count.
func (d *DB) usageLegacyTurns(ctx context.Context, w UsageWindow, ids []string) (
	map[string]legacyTurn, error) {

	query, args, err := usageLegacyTurnsSQL(w, ids)
	if err != nil {
		return nil, err
	}
	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read the records of the usage day's untallied turns: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]legacyTurn, len(ids))
	for rows.Next() {
		var id string
		var f legacyTurn
		if err := rows.Scan(&id, &f.durationMS, &f.failed, &f.reviews, &f.sentBack,
			&f.unaccepted); err != nil {
			return nil, fmt.Errorf("store: scan the records of the usage day's untallied turns: %w", err)
		}
		out[id] = f
	}
	return out, rows.Err()
}

// usageLegacyTurnsSQL is [DB.usageLegacyTurns]'s statement and its arguments:
// the turns named bound as ONE JSON array, so the statement's variables do not
// grow with them, and each sought by the `(turn_id, …)` index rather than by
// the window — the records are found by the turn, not by the day. The array's
// value is CAST to text because the planner seeks the index only for a typed
// value: compared bare, it walks the whole index once per turn
// (TestEveryUsageCountSeeksItsWindow).
func usageLegacyTurnsSQL(w UsageWindow, ids []string) (string, []any, error) {
	raw, err := json.Marshal(ids)
	if err != nil {
		return "", nil, fmt.Errorf("store: encode the untallied turns: %w", err)
	}
	review := string(types.PhaseReview)
	return `
		SELECT e.turn_id,
		       SUM(CASE WHEN e.event_type = ?
		                THEN COALESCE(json_extract(e.payload, '$.duration_ms'), 0)
		                ELSE 0 END),
		       MAX(CASE WHEN e.event_type = ?
		                 AND COALESCE(json_extract(e.payload, '$.suspended'), 0) = 0
		                 AND COALESCE(json_extract(e.payload, '$.failed'), 0) = 1
		                THEN 1 ELSE 0 END),
		       SUM(CASE WHEN e.event_type = ? AND e.phase = ? THEN 1 ELSE 0 END),
		       SUM(CASE WHEN e.event_type = ? AND e.phase = ?
		                 AND COALESCE(json_extract(e.payload, '$.decision'), '') = ?
		                THEN 1 ELSE 0 END),
		       SUM(CASE WHEN e.event_type = ? AND e.phase = ?
		                 AND COALESCE(json_extract(e.payload, '$.decision'), '') != ?
		                THEN 1 ELSE 0 END)
		  FROM json_each(?) AS j CROSS JOIN crewlet_events AS e
		 WHERE e.turn_id = CAST(j.value AS TEXT) AND ` + keptRowSQL + `
		 GROUP BY e.turn_id`,
		[]any{
			turnCompleted,
			types.AgentTurnCompleted{}.EventType(),
			phaseCompleted, review,
			phaseCompleted, review, w.SentBack,
			phaseCompleted, review, w.Accepted,
			string(raw),
		}, nil
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

	query, args := usageReadsSQL(w)
	rows, err := d.sql.QueryContext(ctx, query, args...)
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

// usageReadsSQL is [DB.usageReads]'s statement and its arguments.
func usageReadsSQL(w UsageWindow) (string, []any) {
	return `
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
		   AND e.agent_id != '' AND ` + keptRowSQL + `
		 ORDER BY e.event_time, e.event_id`,
		[]any{types.KnowledgeRead{}.EventType(), EncodeTime(w.Start), EncodeTime(w.End)}
}

// scheduleOutcomeFired is the dispatch ledger's word for a fire it sent
// (schedule.OutcomeFired). Spelled here because the scheduler's package
// depends on this one; the ledger's contract suite holds the word.
const scheduleOutcomeFired = "fired"

// usageSchedules reads the fires this node's scheduler recorded in the window.
//
// THE TURN IS FOUND BY THE TRACE, and only where this node ran it: a fire is
// dispatched to a seat's mailbox and whichever node holds that seat runs the
// turn, so a turn on a peer is in the peer's event log. The trace names it
// either way.
//
// NOT THROUGH THE KEPT-ROWS PREDICATE, and that is the one read here that
// deliberately does not take it: `scheduled_runs` is this node's scheduler's
// own ledger, which custody never writes, and the turn is a LABEL rather than
// a count — the turn a trace ran is that turn whichever data node keeps its
// rows, so a row of a batch in flight names it as truly as a kept one does.
// The price is stated in the file doc: the label is read off any row of the
// trace at any time, and is neither fingerprinted nor marked, so a custody
// write can set it and a settle as not kept clear it with no derivation of the
// day following — a record published in between may carry a label a later one
// drops. A kept settle never moves it: the rows were already read while in
// flight.
//
// AND A FIRE THE SEAT DID NOT RUN IS FOUND THE SAME WAY, as a label off its
// own trace: a fire the seat's node superseded with a later one of the same
// schedule carries a [types.ScheduledFireSuperseded] record under its trace,
// and reads as [types.ScheduleOutcomeSuperseded] rather than as a dispatch
// with no turn behind it. Only a FIRED row can be superseded; a skip was
// never sent.
func (d *DB) usageSchedules(ctx context.Context, w UsageWindow) ([]UsageSchedule, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT r.scope_type, r.scope_id, r.schedule_name, r.target_handle,
		       r.fired_at,
		       CASE WHEN r.outcome = ? AND r.trace_id != '' AND EXISTS (
		                SELECT 1 FROM crewlet_events s
		                 WHERE s.trace_id = r.trace_id AND s.event_type = ?)
		            THEN ? ELSE r.outcome END,
		       r.trace_id,
		       COALESCE((SELECT e.turn_id FROM crewlet_events e
		                  WHERE r.trace_id != '' AND e.trace_id = r.trace_id
		                    AND e.turn_id != ''
		                  ORDER BY e.event_time, e.event_id LIMIT 1), '')
		  FROM scheduled_runs r
		 WHERE r.fired_at >= ? AND r.fired_at < ?
		 ORDER BY r.scope_type, r.scope_id, r.schedule_name, r.fired_at, r.target_handle`,
		scheduleOutcomeFired, types.ScheduledFireSuperseded{}.EventType(),
		types.ScheduleOutcomeSuperseded, EncodeTime(w.Start), EncodeTime(w.End))
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
