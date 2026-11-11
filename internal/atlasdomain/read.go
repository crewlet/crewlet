package atlasdomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Reader answers questions about the atlas from this node's own applied rows,
// through the domain's read authority, at the level each caller asks. Nothing
// here folds: the claims go to [atlas.Fold] beside the ones computed at read.
type Reader struct {
	db  store.ReplicatedReader
	log *statelog.Reader
}

// ReaderOptions configure a reader.
type ReaderOptions struct {
	DB store.ReplicatedReader

	// Log is this domain's read authority, required: without it every read
	// level is a label rather than a guarantee.
	Log *statelog.Reader
}

// NewReader builds the atlas read side.
func NewReader(opts ReaderOptions) (*Reader, error) {
	switch {
	case opts.DB.IsZero():
		return nil, errors.New("atlasdomain: a reader needs the replicated estate")
	case opts.Log == nil:
		return nil, errors.New("atlasdomain: a reader needs its domain's read authority")
	}
	return &Reader{db: opts.DB, log: opts.Log}, nil
}

// DefaultRead is the level a person's read of the atlas takes: their own
// writes, wherever they made them.
func DefaultRead() statelog.Freshness { return statelog.Freshness{Level: statelog.ReadSession} }

// stale is the level the engine's own duties read at: a nomination decided
// again in the write's own snapshot.
var stale = statelog.Freshness{Level: statelog.ReadStale}

// rowScanner is one row, from a query or a query row.
type rowScanner interface{ Scan(dest ...any) error }

// ---- claims ------------------------------------------------------------- //

// claimColumns is every column a [ClaimRow] is read from, in [scanClaim]'s
// order: the claim itself as its record's JSON, and what the log says of it.
const claimColumns = `claim_id, entry_id, claim, observed_at, by, by_kind, at, position,
	replaced, superseded_at, superseded_position`

// ClaimRow is one claim as the log recorded it.
type ClaimRow struct {
	// ID is the claim's own id, Entry its entry's.
	ID    string
	Entry string

	// Claim is what it says, in the vocabulary's own type, its Position the
	// record's.
	Claim atlas.Claim

	// By is who wrote the record.
	By iam.Actor

	// At is the broker's instant for the record, Position its place on the log.
	At       time.Time
	Position statelog.Position

	// Replaced is true for a claim a newer claim of the same voice replaced,
	// ReplacedAt when, and ReplacedBy the record that did.
	Replaced   bool
	ReplacedAt time.Time
	ReplacedBy statelog.Position
}

// scanClaim reads one claim row.
func scanClaim(row rowScanner) (ClaimRow, error) {
	var (
		c                                              ClaimRow
		body, byKind                                   string
		observed, at, position, replacedAt, replacedBy int64
		replaced                                       int
		wire                                           Claim
	)
	if err := row.Scan(&c.ID, &c.Entry, &body, &observed, &c.By.Name, &byKind, &at, &position,
		&replaced, &replacedAt, &replacedBy); err != nil {
		return ClaimRow{}, err
	}
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		return ClaimRow{}, fmt.Errorf("claim %s does not decode: %w", c.ID, err)
	}
	stream := Domain{}.Stream().Name
	c.Claim = wire.vocabulary(store.DecodeTime(observed), uint64(position))
	c.By.Kind = iam.ActorKind(byKind)
	c.At, c.Position = store.DecodeTime(at), statelog.Unpack(stream, position)
	if replaced == 1 {
		c.Replaced, c.ReplacedAt = true, store.DecodeTime(replacedAt)
		c.ReplacedBy = statelog.Unpack(stream, replacedBy)
	}
	return c, nil
}

// Live is every claim the atlas holds now: the newest of each voice on each
// slot of each entry, every one of which [atlas.Fold] reads.
type Live struct {
	Claims []atlas.Claim

	// Position is where the answer is current through, and Complete false
	// where this node holds a record it could not decode.
	Position statelog.Position
	Complete bool
}

// Live reads every live claim, at the caller's level: what the atlas folds
// beside the claims it computes when it is read.
//
// EACH FROM ITS OWN RECORD'S JSON, one column a row: the driver's cost is per
// column, and twenty-odd of them over the reference company's twenty thousand
// claims cost ten times what one does (BenchmarkAtlasLiveRead).
func (r *Reader) Live(ctx context.Context, fresh statelog.Freshness) (Live, error) {
	var out Live
	answer, err := r.log.Read(ctx, fresh.Query(DomainScope(), true), func(tx *sql.Tx) error {
		out = Live{}
		rows, err := tx.QueryContext(ctx, `
			SELECT claim, observed_at, position FROM atlas_claims INDEXED BY atlas_claims_live_idx
			WHERE replaced = 0 ORDER BY position LIMIT ?`, MaxLiveClaims+1)
		if err != nil {
			return err
		}
		for rows.Next() {
			var (
				body               string
				observed, position int64
				wire               Claim
			)
			if err := rows.Scan(&body, &observed, &position); err != nil {
				_ = rows.Close()
				return err
			}
			if err := json.Unmarshal([]byte(body), &wire); err != nil {
				_ = rows.Close()
				return fmt.Errorf("the claim at %d does not decode: %w", position, err)
			}
			out.Claims = append(out.Claims, wire.vocabulary(store.DecodeTime(observed), uint64(position)))
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		if len(out.Claims) > MaxLiveClaims {
			return fmt.Errorf("atlasdomain: the atlas holds more than %d live claims, past what one "+
				"read answers", MaxLiveClaims)
		}
		return nil
	})
	if err != nil {
		return Live{}, fmt.Errorf("atlasdomain: read the atlas's live claims: %w", err)
	}
	out.Position, out.Complete = answer.Position, answer.Complete
	return out, nil
}

// ---- one entry ---------------------------------------------------------- //

// EntryRow is one entry the log holds a claim about.
type EntryRow struct {
	ID  string
	Ref atlas.Ref

	// Version is the position of the newest record on the entry: the tag a
	// person's edit names as its ifMatch.
	Version uint64

	// Retired is when a person's newest word on the entry itself retired it,
	// zero while it stands.
	Retired time.Time

	// At is when the log first took a claim on it.
	At time.Time
}

// Entry reads one entry by its id, and whether the log holds any claim on it.
func (r *Reader) Entry(ctx context.Context, id string, fresh statelog.Freshness) (EntryRow, bool, error) {
	if !IsEntryID(id) {
		return EntryRow{}, false, invalid("%q is not an entry id", id)
	}
	var (
		out   EntryRow
		found bool
	)
	_, err := r.log.Read(ctx, fresh.Query(EntryScope(id), true), func(tx *sql.Tx) error {
		var (
			kind, key              string
			version, at, retiredAt int64
			retired                int
		)
		err := tx.QueryRowContext(ctx, `
			SELECT kind, key, version, retired, retired_at, at FROM atlas_entries WHERE entry_id = ?`,
			id).Scan(&kind, &key, &version, &retired, &retiredAt, &at)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			out, found = EntryRow{}, false
			return nil
		case err != nil:
			return err
		}
		out = EntryRow{ID: id, Ref: atlas.Ref{Kind: atlas.Kind(kind), Key: key},
			Version: uint64(version), At: store.DecodeTime(at)}
		if retired == 1 {
			out.Retired = store.DecodeTime(retiredAt)
		}
		found = true
		return nil
	})
	if err != nil {
		return EntryRow{}, false, fmt.Errorf("atlasdomain: read entry %s: %w", id, err)
	}
	return out, found, nil
}

// HistoryPage is one page of an entry's claims, newest first, the ones a newer
// claim replaced included.
type HistoryPage struct {
	Claims []ClaimRow

	// Next is the cursor of the page after this one, empty on the last.
	Next string

	Position statelog.Position
	Complete bool
}

// History pages the claims recorded on one entry, newest first, from cursor:
// what changed on it, when, and who said so, for as long as the retention
// keeps it.
func (r *Reader) History(ctx context.Context, id, cursor string, limit int,
	fresh statelog.Freshness) (HistoryPage, error) {

	if !IsEntryID(id) {
		return HistoryPage{}, invalid("%q is not an entry id", id)
	}
	before, size, err := page(cursor, limit)
	if err != nil {
		return HistoryPage{}, err
	}
	var out HistoryPage
	answer, err := r.log.Read(ctx, fresh.Query(EntryScope(id), true), func(tx *sql.Tx) error {
		out = HistoryPage{}
		rows, err := tx.QueryContext(ctx, `SELECT `+claimColumns+`
			FROM atlas_claims INDEXED BY atlas_claims_entry_idx
			WHERE entry_id = ? AND position < ? ORDER BY position DESC LIMIT ?`, id, before, size+1)
		if err != nil {
			return err
		}
		for rows.Next() {
			c, scanErr := scanClaim(rows)
			if scanErr != nil {
				_ = rows.Close()
				return scanErr
			}
			out.Claims = append(out.Claims, c)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	if err != nil {
		return HistoryPage{}, fmt.Errorf("atlasdomain: read entry %s's history: %w", id, err)
	}
	if len(out.Claims) > size {
		out.Claims = out.Claims[:size]
		out.Next = cursorOf(out.Claims[size-1].Position)
	}
	out.Position, out.Complete = answer.Position, answer.Complete
	return out, nil
}

// ---- purges ------------------------------------------------------------- //

// PurgeRow is one purge: what it erased, by its id or its digest and never its
// value, who ran it and when, and how many records it has kept from coming back.
type PurgeRow struct {
	// Entry is true for an entry's purge, false for a value's; ID is the
	// entry's id or the value's digest.
	Entry bool
	ID    string

	By iam.Actor
	At time.Time

	// Blocked is how many records the purge has kept out since, and
	// LastBlocked the newest.
	Blocked     int
	LastBlocked time.Time
}

// Purges reads every purge, newest first: the record an administrator's purge
// keeps of itself.
func (r *Reader) Purges(ctx context.Context, fresh statelog.Freshness) ([]PurgeRow, error) {
	var out []PurgeRow
	_, err := r.log.Read(ctx, fresh.Query(DomainScope(), true), func(tx *sql.Tx) error {
		out = nil
		rows, err := tx.QueryContext(ctx, `
			SELECT kind, id, by, by_kind, at, rejects, last_reject_at FROM atlas_purges
			ORDER BY position DESC LIMIT ?`, MaxPurges)
		if err != nil {
			return err
		}
		for rows.Next() {
			var (
				p              PurgeRow
				kind, byKind   string
				at, lastReject int64
			)
			if scanErr := rows.Scan(&kind, &p.ID, &p.By.Name, &byKind, &at, &p.Blocked,
				&lastReject); scanErr != nil {
				_ = rows.Close()
				return scanErr
			}
			p.Entry, p.By.Kind, p.At = kind == string(markerEntry), iam.ActorKind(byKind), store.DecodeTime(at)
			if lastReject != 0 {
				p.LastBlocked = store.DecodeTime(lastReject)
			}
			out = append(out, p)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	if err != nil {
		return nil, fmt.Errorf("atlasdomain: read the purges: %w", err)
	}
	return out, nil
}

// ---- the engine's duties ------------------------------------------------ //

// minAtStatements find the oldest instant at or after a bound and before
// another that an expiry would take, one indexed probe each.
var minAtStatements = []string{
	`SELECT MIN(superseded_at) FROM atlas_claims INDEXED BY atlas_claims_expiry_idx
		WHERE replaced = 1 AND superseded_at >= ? AND superseded_at < ?`,
	`SELECT MIN(last_at) FROM atlas_entries INDEXED BY atlas_entries_retired_idx
		WHERE retired = 1 AND last_at >= ? AND last_at < ?`,
}

// ExpirableHours lists, oldest first and at most limit of them, the hours that
// hold history past cutoff an expiry would take: what the retention duty
// nominates. A stale read: each expiry is decided again in its own snapshot.
func (r *Reader) ExpirableHours(ctx context.Context, cutoff time.Time, limit int) ([]time.Time, error) {
	if limit <= 0 {
		return nil, nil
	}
	var out []time.Time
	end := store.EncodeTime(cutoff)
	_, err := r.log.Read(ctx, stale.Query(DomainScope(), true), func(tx *sql.Tx) error {
		out = nil
		from := int64(0)
		for len(out) < limit {
			oldest := sql.NullInt64{}
			for _, statement := range minAtStatements {
				var at sql.NullInt64
				if err := tx.QueryRowContext(ctx, statement, from, end).Scan(&at); err != nil {
					return err
				}
				if at.Valid && (!oldest.Valid || at.Int64 < oldest.Int64) {
					oldest = at
				}
			}
			if !oldest.Valid {
				return nil
			}
			hour := store.DecodeTime(oldest.Int64).Truncate(ExpiryBucket)
			out = append(out, hour)
			from = store.EncodeTime(hour.Add(ExpiryBucket))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("atlasdomain: list the hours past the retention: %w", err)
	}
	return out, nil
}

// ---- paging ------------------------------------------------------------- //

// page reads a cursor and a limit: before is the packed position a page starts
// below, the largest for none.
func page(cursor string, limit int) (before int64, size int, err error) {
	switch {
	case limit < 0 || limit > MaxPageSize:
		return 0, 0, invalid("a page holds 1 to %d rows", MaxPageSize)
	case limit == 0:
		limit = DefaultPageSize
	}
	if cursor == "" {
		return int64(1<<63 - 1), limit, nil
	}
	before, err = strconv.ParseInt(cursor, 10, 64)
	if err != nil || before <= 0 {
		return 0, 0, invalid("the cursor %q is not one a page answered", cursor)
	}
	return before, limit, nil
}

// cursorOf is the cursor of the page below position p.
func cursorOf(p statelog.Position) string { return strconv.FormatInt(p.Packed(), 10) }
