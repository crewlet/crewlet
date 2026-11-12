package atlasdomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

// ---- claims ------------------------------------------------------------- //

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
		out.Claims, err = scanLive(rows)
		if err != nil {
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

// LiveOf reads every live claim one voice holds: what an integration's read is
// diffed against before it is recorded (ADR-0129). Stale by default on the
// duty's side, since the read decides nothing a snapshot would not decide
// again: a claim it misses is written once more, which the applier takes once.
func (r *Reader) LiveOf(ctx context.Context, src atlas.Source, fresh statelog.Freshness) (Live, error) {
	var out Live
	answer, err := r.log.Read(ctx, fresh.Query(DomainScope(), true), func(tx *sql.Tx) error {
		out = Live{}
		rows, err := tx.QueryContext(ctx, `
			SELECT claim, observed_at, position FROM atlas_claims INDEXED BY atlas_claims_source_idx
			WHERE replaced = 0 AND source_kind = ? AND source_name = ? ORDER BY position LIMIT ?`,
			string(src.Kind), src.Name, MaxLiveClaims+1)
		if err != nil {
			return err
		}
		out.Claims, err = scanLive(rows)
		if err != nil {
			return err
		}
		if len(out.Claims) > MaxLiveClaims {
			return fmt.Errorf("atlasdomain: %s/%s holds more than %d live claims, past what one read "+
				"answers", src.Kind, src.Name, MaxLiveClaims)
		}
		return nil
	})
	if err != nil {
		return Live{}, fmt.Errorf("atlasdomain: read %s/%s's live claims: %w", src.Kind, src.Name, err)
	}
	out.Position, out.Complete = answer.Position, answer.Complete
	return out, nil
}

// scanLive reads live claims off rows of (claim, observed_at, position), and
// closes them.
func scanLive(rows *sql.Rows) ([]atlas.Claim, error) {
	var out []atlas.Claim
	for rows.Next() {
		var (
			body               string
			observed, position int64
			wire               Claim
		)
		if err := rows.Scan(&body, &observed, &position); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(body), &wire); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("the claim at %d does not decode: %w", position, err)
		}
		out = append(out, wire.vocabulary(store.DecodeTime(observed), uint64(position)))
	}
	return out, errors.Join(rows.Err(), rows.Close())
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
