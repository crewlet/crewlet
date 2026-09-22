package chart

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE READ SIDE, and the one thing every answer here carries that a plain SQL
// read does not: THE POSITION IT WAS TRUE AS OF.
//
// # Why a position rather than a boolean
//
// This node's rows are derived from an ordered log, and it may be behind. The
// question a caller actually has is not "are you caught up" — which is a
// snapshot of a moving thing and false by the time it is read — but "what did
// you know when you answered this". A position answers that, and it composes:
// a caller that wrote at position P and then reads can require the answer to
// include P, and a caller comparing two answers can tell which is newer.
//
// A HYDRATION BOOLEAN CANNOT DO EITHER. It was the shape this engine used
// before the framework, and every surface that rendered one had to decide for
// itself what "not hydrated" meant — so a board showed an empty list, a roster
// showed nobody, and neither said why.
//
// # Every read goes through the framework, including the ones that look local
//
// A read that opened its own transaction would be a level that is a LABEL: it
// would report `session` while serving whatever this node happens to hold, and
// a degradation invisible in the answer is worse than a refusal. So the
// framework's reader is required at construction, and a read below the
// requested level REFUSES rather than degrading.

// Reader answers questions about the chart, at a stated position.
type Reader struct {
	db  *store.DB
	log *statelog.Reader

	committed func() statelog.Position
	lag       func() time.Duration
}

// ReaderOptions is what a reader is built from.
type ReaderOptions struct {
	DB *store.DB

	// Log is this domain's read authority. REQUIRED: without it every read
	// level is a label rather than a guarantee.
	Log *statelog.Reader

	// Committed is this node's applied position. Nil answers the zero
	// position, which is what a reader with no runner behind it has and
	// what a read then honestly reports.
	Committed func() statelog.Position

	// Lag is how far behind the log this node's chart applier is, as a
	// duration.
	//
	// A DURATION AND NOT THE RECORD COUNT [Answer.Lag] carries, because
	// its reader compares it against [statelog.StallGrace] — the one
	// threshold in this engine for "behind enough to matter" — and a
	// count cannot be compared against a length of time without knowing
	// how fast this node applies. The engine measures that; this package
	// is handed the answer rather than deriving a second one.
	//
	// Nil answers zero, which is what a reader with no runner behind it
	// honestly has: nothing to be behind.
	Lag func() time.Duration
}

// NewReader builds the chart's read side.
func NewReader(opts ReaderOptions) (*Reader, error) {
	if opts.DB == nil {
		return nil, errors.New("chart: a reader needs the replicated estate")
	}
	if opts.Log == nil {
		return nil, errors.New("chart: a reader needs its domain's read " +
			"authority — without it every read level is a label rather than " +
			"a guarantee, and a degradation invisible in the answer is worse " +
			"than a refusal")
	}
	r := &Reader{db: opts.DB, log: opts.Log,
		committed: opts.Committed, lag: opts.Lag}
	if r.committed == nil {
		r.committed = func() statelog.Position { return statelog.Position{} }
	}
	if r.lag == nil {
		r.lag = func() time.Duration { return 0 }
	}
	return r, nil
}

// At is the position this node's rows were derived through, which every answer
// here is true as of.
func (r *Reader) At() statelog.Position { return r.committed() }

// Lag is how far behind the log this node's chart applier is.
func (r *Reader) Lag() time.Duration { return r.lag() }

// Answer is what every read here carries beside its rows.
//
// ONE STRUCT EMBEDDED IN EVERY RESULT rather than three fields each result
// remembers to fill, because a surface renders them together — "as of position
// P, 4 records behind" — and a result that carried the position and not the
// lag would render a claim with no scale beside it.
type Answer struct {
	// Level is the level this answer was SERVED at, which a caller compares
	// against what it asked for. They are equal or the read refused: this
	// domain does not degrade.
	Level statelog.ReadLevel

	// Position is what the answer is true as of.
	Position statelog.Position

	// Lag is how far behind the log's end this node was, and NIL when the
	// broker could not say.
	//
	// A POINTER because "zero records behind" and "nobody could tell me how
	// far behind" are different facts, and a surface that rendered the
	// second as the first would show a caught-up node during exactly the
	// outage in which it is not.
	Lag *uint64
}

// answerFrom is the framework's answer in this domain's own shape.
//
// ONE CONVERSION rather than three fields copied at each of the five reads,
// because a read that filled the position and forgot the lag renders a claim
// with no scale beside it — and it would do so silently.
func answerFrom(served statelog.Answer) Answer {
	return Answer{Level: served.Level, Position: served.Position, Lag: served.Lag}
}

// Chart is the whole authored structure, as one read.
//
// THE WHOLE THING IN ONE ANSWER, which is affordable precisely here: a company
// has hundreds of units and seats rather than the hundreds of thousands of rows
// the tracker holds. Every derivation over a chart is a WALK — lead
// inheritance, unit expansion, who manages whom — and a walk served by a query
// per ancestor is N round trips against a copy that may move between them.
type Chart struct {
	Answer

	Units []Unit
	Seats []Seat

	// Manages is every authored edge, keyed by the seat that authored it.
	Manages map[string][]string

	// Leads is every unit's authored lead, keyed by unit.
	Leads map[string]string
}

// Read answers the whole chart at the requested level.
func (r *Reader) Read(ctx context.Context, fresh statelog.Freshness) (Chart, error) {
	if fresh.Level == "" {
		return Chart{}, errors.New("chart: this read names no level — a " +
			"surface resolves an absent read_level to its own default before " +
			"it reads")
	}
	var out Chart
	// THE WHOLE-CHART SCOPE IS THE DOMAIN, and that is honest rather than
	// lazy: a read of the entire structure is one that every deferred
	// record concerns, so "is this complete" has exactly one correct answer
	// and it is not "yes, apart from the part I could not see".
	served, err := r.log.Read(ctx, fresh.Query(ReadScope("", ""), false),
		func(tx *sql.Tx) error {
			var err error
			if out.Units, err = readUnits(ctx, tx); err != nil {
				return err
			}
			if out.Seats, err = readSeats(ctx, tx); err != nil {
				return err
			}
			if out.Manages, err = readManages(ctx, tx); err != nil {
				return err
			}
			out.Leads, err = readLeads(ctx, tx)
			return err
		})
	if err != nil {
		return Chart{}, err
	}
	out.Answer = answerFrom(served)
	return out, nil
}

// UnitDetail is one unit with what a card renders beside it.
type UnitDetail struct {
	Answer

	Unit Unit

	// Children and Seats are what this unit directly holds. NOT the
	// subtree: a card shows one level, and a caller that wants the subtree
	// walks [Reader.Read]'s whole chart rather than asking this N times.
	Children []Unit
	Seats    []Seat

	History []Change
}

// Unit answers one unit by its key, or by a key it used to answer to.
func (r *Reader) Unit(ctx context.Context, key string, fresh statelog.Freshness) (
	UnitDetail, error) {

	if fresh.Level == "" {
		return UnitDetail{}, errors.New("chart: this read names no level")
	}
	key = NormalizeKey(key)
	var out UnitDetail
	served, err := r.log.Read(ctx, fresh.Query(ReadScope(key, ""), false),
		func(tx *sql.Tx) error {
			unit, found, err := resolveUnit(ctx, tx, key)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("chart: no unit answers to %q: %w",
					key, ErrNotFound)
			}
			out.Unit = unit
			if out.Children, err = readUnitsWhere(ctx, tx,
				`parent_key = ?`, unit.Key); err != nil {
				return err
			}
			if out.Seats, err = readSeatsWhere(ctx, tx,
				`unit_key = ?`, unit.Key); err != nil {
				return err
			}
			out.History, err = readHistory(ctx, tx, KindUnit, unit.Key)
			return err
		})
	if err != nil {
		return UnitDetail{}, err
	}
	out.Answer = answerFrom(served)
	return out, nil
}

// SeatDetail is one seat with its own history.
type SeatDetail struct {
	Answer

	Seat    Seat
	Manages []string
	History []Change
}

// Seat answers one seat by its handle, or by a handle it used to answer to.
func (r *Reader) Seat(ctx context.Context, handle string, fresh statelog.Freshness) (
	SeatDetail, error) {

	if fresh.Level == "" {
		return SeatDetail{}, errors.New("chart: this read names no level")
	}
	handle = NormalizeKey(handle)
	var out SeatDetail
	// THE SCOPE NAMES THE SEAT AND NOT ITS UNIT, because the unit is a row
	// this read has not taken yet — and a scope that guessed would file the
	// probe under a team the seat may have left.
	served, err := r.log.Read(ctx, fresh.Query(ReadScope("", handle), false),
		func(tx *sql.Tx) error {
			seat, found, err := resolveSeat(ctx, tx, handle)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("chart: no seat answers to %q: %w",
					handle, ErrNotFound)
			}
			out.Seat = seat
			if out.Manages, err = readManagesOf(ctx, tx, seat.Handle); err != nil {
				return err
			}
			out.History, err = readHistory(ctx, tx, KindSeat, seat.Handle)
			return err
		})
	if err != nil {
		return SeatDetail{}, err
	}
	out.Answer = answerFrom(served)
	return out, nil
}

// ErrNotFound reports an address nothing in the chart answers to.
//
// A SENTINEL because a surface has to tell it from a read that could not be
// SERVED: one is a 404 and the other is a 503, and collapsing them would have
// a node that is merely behind reporting that a team does not exist.
var ErrNotFound = errors.New("chart: no such object")

// --- resolving an address ---------------------------------------------------- //

// resolveUnit answers a unit by its key or by a key it used to answer to.
//
// THE CURRENT KEY FIRST, ALWAYS. A former key goes on resolving until something
// else claims it, and when something has, the claimant wins: the alternative is
// that creating a unit named after a retired one silently resolves to the old
// object, for ever, on every node.
func resolveUnit(ctx context.Context, tx *sql.Tx, key string) (Unit, bool, error) {
	unit, found, err := readUnit(ctx, tx, key)
	if err != nil || found {
		return unit, found, err
	}
	return byFormerKey(ctx, tx, "chart_units", key, DecodeUnit)
}

// addressHolder is the object that ALREADY answers to this address — live or
// retired — named as it is addressed now, or empty when nothing does.
//
// TWO CALLERS THAT MUST AGREE: the claim's decide, which refuses an address
// somebody else holds, and the apply, which declines one. Written twice they
// would eventually differ about whether a RETIRED address counts, and the two
// answers are a refused rename and a stalled domain.
//
// It answers with the holder's CURRENT address rather than a boolean, because
// the one claim that must still be allowed is a rename BACK: an object moving
// onto an address it used to answer to is claiming something that already
// resolves to it, and a bare "held" could not tell that from a collision.
func addressHolder(ctx context.Context, tx *sql.Tx, kind ObjectKind,
	address string) (string, bool, error) {

	switch kind {
	case KindUnit:
		unit, found, err := resolveUnit(ctx, tx, address)
		return unit.Key, found, err
	case KindSeat:
		seat, found, err := resolveSeat(ctx, tx, address)
		return seat.Handle, found, err
	}
	return "", false, nil
}

// resolveSeat is [resolveUnit] for a seat.
func resolveSeat(ctx context.Context, tx *sql.Tx, handle string) (Seat, bool, error) {
	seat, found, err := readSeat(ctx, tx, handle)
	if err != nil || found {
		return seat, found, err
	}
	return byFormerKey(ctx, tx, "chart_seats", handle, DecodeSeat)
}

// byFormerKey scans for an object that used to answer to this address.
//
// A SCAN, DELIBERATELY, and it is what [Unit.FormerKeys]'s own doc argues for:
// a company has tens or hundreds of objects, not the hundreds of thousands the
// tracker's key directory was built for, so this reads a table a page of memory
// holds. A child table and its index would be machinery bought for a read that
// is already free, and one more thing an apply has to keep in step.
//
// IT RUNS ONLY AFTER THE DIRECT LOOKUP MISSED, so the ordinary read pays
// nothing for it at all.
func byFormerKey[T any](ctx context.Context, tx *sql.Tx, table, key string,
	decode func([]byte) (T, error)) (T, bool, error) {

	var zero T
	rows, err := tx.QueryContext(ctx,
		`SELECT former_keys_json, document FROM `+table+
			` WHERE former_keys_json <> '[]'`)
	if err != nil {
		return zero, false, fmt.Errorf("chart: scan %s for a retired address "+
			"%q: %w", table, key, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var raw string
		var document []byte
		if err := rows.Scan(&raw, &document); err != nil {
			return zero, false, fmt.Errorf("chart: read a %s row: %w", table, err)
		}
		var former []string
		if err := json.Unmarshal([]byte(raw), &former); err != nil {
			// A ROW WHOSE LIST WILL NOT DECODE IS SKIPPED rather than
			// failing the read: it is one object's retired addresses,
			// and refusing the whole lookup over it would take out
			// every resolution on the node.
			continue
		}
		for _, was := range former {
			if was != key {
				continue
			}
			out, err := decode(document)
			if err != nil {
				return zero, false, err
			}
			return out, true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return zero, false, fmt.Errorf("chart: scan %s: %w", table, err)
	}
	return zero, false, nil
}

// --- the row readers ---------------------------------------------------------- //

func readUnits(ctx context.Context, tx *sql.Tx) ([]Unit, error) {
	return readUnitsWhere(ctx, tx, "1 = 1")
}

func readUnitsWhere(ctx context.Context, tx *sql.Tx, where string, args ...any) (
	[]Unit, error) {
	return readDocuments(ctx, tx,
		`SELECT document FROM chart_units WHERE `+where+` ORDER BY key`,
		args, DecodeUnit)
}

func readSeats(ctx context.Context, tx *sql.Tx) ([]Seat, error) {
	return readSeatsWhere(ctx, tx, "1 = 1")
}

func readSeatsWhere(ctx context.Context, tx *sql.Tx, where string, args ...any) (
	[]Seat, error) {
	return readDocuments(ctx, tx,
		`SELECT document FROM chart_seats WHERE `+where+` ORDER BY handle`,
		args, DecodeSeat)
}

// readDocuments decodes one table's stored documents, in the query's own order.
//
// ORDERED BY THE ADDRESS in every caller, not because a surface needs it but
// because an unordered read of a set is one whose answer depends on the
// planner: two nodes that hold identical rows would hand a caller two
// different lists, and nothing above them could tell that from a chart that
// had actually changed.
func readDocuments[T any](ctx context.Context, tx *sql.Tx, query string,
	args []any, decode func([]byte) (T, error)) ([]T, error) {

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("chart: read %q: %w", query, err)
	}
	defer func() { _ = rows.Close() }()
	var out []T
	for rows.Next() {
		var document []byte
		if err := rows.Scan(&document); err != nil {
			return nil, fmt.Errorf("chart: read a row: %w", err)
		}
		value, err := decode(document)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chart: read %q: %w", query, err)
	}
	return out, nil
}

// readManages is every authored edge, by the seat that authored it.
func readManages(ctx context.Context, tx *sql.Tx) (map[string][]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT manager, target FROM chart_manages ORDER BY manager, target`)
	if err != nil {
		return nil, fmt.Errorf("chart: read the manages edges: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]string{}
	for rows.Next() {
		var manager, target string
		if err := rows.Scan(&manager, &target); err != nil {
			return nil, fmt.Errorf("chart: read a manages edge: %w", err)
		}
		out[manager] = append(out[manager], target)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chart: read the manages edges: %w", err)
	}
	return out, nil
}

// readManagesOf is one seat's authored edges.
func readManagesOf(ctx context.Context, tx *sql.Tx, handle string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT target FROM chart_manages WHERE manager = ? ORDER BY target`,
		handle)
	if err != nil {
		return nil, fmt.Errorf("chart: read what %s manages: %w", handle, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			return nil, fmt.Errorf("chart: read a manages edge: %w", err)
		}
		out = append(out, target)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chart: read what %s manages: %w", handle, err)
	}
	return out, nil
}

// readLeads is every unit's authored lead.
func readLeads(ctx context.Context, tx *sql.Tx) (map[string]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT unit_key, handle FROM chart_leads ORDER BY unit_key`)
	if err != nil {
		return nil, fmt.Errorf("chart: read the lead edges: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var unit, handle string
		if err := rows.Scan(&unit, &handle); err != nil {
			return nil, fmt.Errorf("chart: read a lead edge: %w", err)
		}
		out[unit] = handle
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chart: read the lead edges: %w", err)
	}
	return out, nil
}

// HistoryLimit bounds one object's rendered history.
//
// FIFTY, which is what a card shows before it needs a "show more" nobody has
// built: an object's whole history is unbounded in principle — a seat edited
// daily for three years is a thousand rows — and a read that returned all of it
// would put a megabyte on the wire for a panel that renders ten lines.
const HistoryLimit = 50

// readHistory is one object's own history, newest first.
func readHistory(ctx context.Context, tx *sql.Tx, kind ObjectKind, id string) (
	[]Change, error) {

	rows, err := tx.QueryContext(ctx, `
		SELECT document FROM chart_history
		WHERE object_kind = ? AND object_id = ?
		ORDER BY created_at DESC, id DESC
		LIMIT ?`, string(kind), id, HistoryLimit)
	if err != nil {
		return nil, fmt.Errorf("chart: read the history of %s %s: %w",
			kind, id, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Change
	for rows.Next() {
		var document []byte
		if err := rows.Scan(&document); err != nil {
			return nil, fmt.Errorf("chart: read a history row: %w", err)
		}
		change, err := DecodeChange(document)
		if err != nil {
			return nil, err
		}
		out = append(out, change)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chart: read the history of %s %s: %w",
			kind, id, err)
	}
	return out, nil
}

// History is the company-wide reorganisation feed, newest first.
//
// # Why it is a read of its own and not a walk of the object histories
//
// What somebody asks here is "what has been happening to this company" — who
// moved, who was hired, which team was dissolved — and that question is about
// the ORDER changes landed in across every object. Assembling it from N
// per-object reads would merge N ordered lists in a surface, which is both
// the wrong place for the merge and a read whose cost grows with the size of
// the chart rather than with the size of the answer.
//
// The index this drives (`chart_history_created_idx`) shipped with the
// domain's first migration, naming this feed in its own comment, and had no
// reader at all until this method: a second index on a table is not free, and
// one nothing drives is a cost with no benefit that reads as coverage.
//
// QUIET ENTRIES ARE INCLUDED. A quiet change is one that woke nobody — a
// config import applying a revision, a duty tidying a tombstone — and that is
// exactly what somebody reading this feed is trying to account for: a
// structure that changed with no notification is the change hardest to
// explain afterwards.
func (r *Reader) History(ctx context.Context, limit int,
	fresh statelog.Freshness) ([]Change, Answer, error) {

	if fresh.Level == "" {
		return nil, Answer{}, errors.New("chart: this read names no level")
	}
	if limit <= 0 || limit > HistoryLimit {
		limit = HistoryLimit
	}
	var out []Change
	// THE WHOLE-CHART SCOPE, for [Reader.Read]'s reason: a feed across every
	// object is a read every deferred record concerns, so "is this complete"
	// has one correct answer.
	served, err := r.log.Read(ctx, fresh.Query(ReadScope("", ""), false),
		func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(ctx, `
				SELECT document FROM chart_history
				ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
			if err != nil {
				return fmt.Errorf("chart: read the company's history: %w", err)
			}
			defer func() { _ = rows.Close() }()
			out = nil
			for rows.Next() {
				var document []byte
				if err := rows.Scan(&document); err != nil {
					return fmt.Errorf("chart: read a history row: %w", err)
				}
				change, err := DecodeChange(document)
				if err != nil {
					return err
				}
				out = append(out, change)
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("chart: read the company's history: %w", err)
			}
			return nil
		})
	if err != nil {
		return nil, Answer{}, err
	}
	return out, answerFrom(served), nil
}

// --- the import ledger ---------------------------------------------------------- //

// Import is one config revision's landing on this log.
type Import struct {
	Revision string
	Position statelog.Position
	At       int64
	By       string
	Objects  int
	RecordID string
}

// Imports is the ledger, newest first.
//
// THE OPERATOR SURFACE for the question a chart nobody expected prompts: which
// revision is this company's structure actually running, and when did it land.
func (r *Reader) Imports(ctx context.Context, limit int,
	fresh statelog.Freshness) ([]Import, Answer, error) {

	if fresh.Level == "" {
		return nil, Answer{}, errors.New("chart: this read names no level")
	}
	if limit <= 0 || limit > HistoryLimit {
		limit = HistoryLimit
	}
	var out []Import
	served, err := r.log.Read(ctx, fresh.Query(ReadScope("", ""), false),
		func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(ctx, `
				SELECT revision, position, at, by, objects, record_id
				FROM chart_import_ledger
				ORDER BY position DESC LIMIT ?`, limit)
			if err != nil {
				return fmt.Errorf("chart: read the import ledger: %w", err)
			}
			defer func() { _ = rows.Close() }()
			out = nil
			for rows.Next() {
				var one Import
				var packed int64
				if err := rows.Scan(&one.Revision, &packed, &one.At, &one.By,
					&one.Objects, &one.RecordID); err != nil {
					return fmt.Errorf("chart: read an import row: %w", err)
				}
				one.Position = statelog.Unpack(
					Domain{}.Stream().Name, packed)
				out = append(out, one)
			}
			return rows.Err()
		})
	if err != nil {
		return nil, Answer{}, err
	}
	return out, answerFrom(served), nil
}

// Import is one revision's own ledger row, and whether the chart has ever
// applied it.
//
// KEYED RATHER THAN FILTERED OUT OF [Reader.Imports], because the ledger is
// keyed on the revision and the listing is bounded: a revision older than the
// listing's limit would come back "never imported", which is the one answer
// this question must never give wrongly — it is what the control plane's
// re-activation gesture turns on.
func (r *Reader) Import(ctx context.Context, revision string,
	fresh statelog.Freshness) (Import, bool, Answer, error) {

	if fresh.Level == "" {
		return Import{}, false, Answer{}, errors.New("chart: this read names no level")
	}
	var out Import
	var found bool
	served, err := r.log.Read(ctx, fresh.Query(ReadScope("", ""), false),
		func(tx *sql.Tx) error {
			var packed int64
			err := tx.QueryRowContext(ctx, `
				SELECT revision, position, at, by, objects, record_id
				FROM chart_import_ledger WHERE revision = ?`, revision).
				Scan(&out.Revision, &packed, &out.At, &out.By, &out.Objects,
					&out.RecordID)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				found = false
				return nil
			case err != nil:
				return fmt.Errorf("chart: read the import of %s: %w", revision, err)
			}
			out.Position = statelog.Unpack(Domain{}.Stream().Name, packed)
			found = true
			return nil
		})
	if err != nil {
		return Import{}, false, Answer{}, err
	}
	return out, found, answerFrom(served), nil
}

// Removed reports whether an address the chart no longer names was removed,
// and why.
//
// ITS OWN READ rather than an absence from a listing, because "this team was
// dissolved in March, merged into infrastructure" and "there is no such team"
// are different answers to a person asking where their work went.
func (r *Reader) Removed(ctx context.Context, ref ObjectRef,
	fresh statelog.Freshness) (Removal, bool, Answer, error) {

	if fresh.Level == "" {
		return Removal{}, false, Answer{}, errors.New("chart: this read names no level")
	}
	var out Removal
	var found bool
	served, err := r.log.Read(ctx, fresh.Query(ReadScope("", ""), false),
		func(tx *sql.Tx) error {
			err := tx.QueryRowContext(ctx, `
				SELECT at, record_id, actor, actor_kind, reason
				FROM chart_removed WHERE object_kind = ? AND object_id = ?`,
				string(ref.Kind), NormalizeKey(ref.ID)).
				Scan(&out.At, &out.RecordID, &out.Actor, &out.ActorKind,
					&out.Reason)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				found = false
				return nil
			case err != nil:
				return fmt.Errorf("chart: read the removal of %s: %w", ref, err)
			}
			out.Object = ObjectRef{Kind: ref.Kind, ID: NormalizeKey(ref.ID)}
			found = true
			return nil
		})
	if err != nil {
		return Removal{}, false, Answer{}, err
	}
	return out, found, answerFrom(served), nil
}

// Removal is what a tombstone holds.
type Removal struct {
	Object    ObjectRef
	At        int64
	RecordID  string
	Actor     string
	ActorKind string
	Reason    string
}

// ReadLevelFor resolves an absent level for one surface.
//
// IT LIVES HERE rather than in the surface, so the four that read this domain
// cannot disagree about what "unspecified" means — which is the shape that had
// a dashboard reading `stale` and an API reading `session` for one question.
func ReadLevelFor(asked string) statelog.ReadLevel {
	level := statelog.ReadLevel(strings.TrimSpace(asked))
	if level.Valid() {
		return level
	}
	// SESSION IS THE DEFAULT, which is the level that makes "I wrote it
	// and then read it" work. Stale would be cheaper and would have a
	// founder who just moved a seat reading a chart that does not show it.
	return statelog.ReadSession
}
