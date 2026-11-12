package atlasdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE PURGE GATE. A purge erases an entry or a value everywhere and leaves a
// MARKER that no later record gets past: a claim on a purged entry, a claim
// naming one, a claim carrying a purged value, and a claim on an entry whose key
// is a purged value all land as nothing on every node, the purge's own record
// excepted. Permanent: a purge is what a person reaches for when a value must
// not be kept, and a marker that lapsed would let the next report bring it back.

// markerKind is what a purge marker names: an entry by its id, or a value by the
// digest of its text.
type markerKind string

const (
	markerEntry markerKind = "entry"
	markerValue markerKind = "value"
)

// marker is one purge marker a record is held to.
type marker struct {
	kind markerKind
	id   string
}

// markersOf is every marker a record on subj is held to: its own subject's,
// and for a claim, every marker [claimMarkers] names. A payload this build
// cannot decode yields its subject's marker alone: such a record is deferred,
// and asked again by the build that can read it.
func markersOf(subj statelog.Subject, payload []byte) []marker {
	switch ObjectKind(subj.Kind) {
	case KindEntry:
	case KindValue:
		return []marker{{markerValue, subj.ID}}
	default:
		return nil
	}
	own := []marker{{markerEntry, subj.ID}}
	if len(payload) == 0 {
		return own
	}
	rec, err := Decode(payload)
	if err != nil || rec.Op != OpClaim {
		return own
	}
	mutation, err := DecodeMutation(rec)
	if err != nil {
		return own
	}
	claim, ok := mutation.(Claim)
	if !ok {
		return own
	}
	return claimMarkers(subj.ID, claim)
}

// claimMarkers is every marker a claim on entry is held to: the entry's own,
// the entry it names, and as the value marker that would erase each, its key,
// its target's key and its value.
func claimMarkers(entry string, claim Claim) []marker {
	out := []marker{{markerEntry, entry}}
	if claim.Target != nil {
		out = append(out, marker{markerEntry, claim.Target.ID})
	}
	values := []string{Digest(claim.Entry.Key)}
	if claim.Target != nil {
		values = append(values, Digest(claim.Target.Key))
	}
	if claim.Value != nil {
		values = append(values, Digest(claim.Value.Text))
	}
	for _, d := range values {
		if m := (marker{markerValue, d}); !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	return out
}

// heldBy is the first marker of markers a record under opID is gated by: one in
// place whose purge is not this record. Read in the caller's transaction.
func heldBy(ctx context.Context, tx *sql.Tx, markers []marker, opID string) (marker, bool, error) {
	for _, m := range markers {
		var author string
		err := tx.QueryRowContext(ctx, `
			SELECT purge_record_id FROM atlas_purges WHERE kind = ? AND id = ?`,
			string(m.kind), m.id).Scan(&author)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return marker{}, false, fmt.Errorf("atlasdomain: read the purge gate for %s %s: %w",
				m.kind, m.id, err)
		}
		if author != opID {
			return m, true, nil
		}
	}
	return marker{}, false, nil
}

// evictedAt reports whether a record at packed, written by writer, falls in
// writer's eviction window. THE WINDOW IS HALF-OPEN AT BOTH ENDS: a record at
// or below the eviction's own position was written while the node was counted,
// and one at or above a readmission by a node the fleet took back.
func evictedAt(ctx context.Context, tx *sql.Tx, writer string, packed int64) (bool, error) {
	var from, readmitted sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT from_position, readmitted_position FROM atlas_evictions
		WHERE node_id = ?`, writer).Scan(&from, &readmitted)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("atlasdomain: read the eviction gate for node %s: %w", writer, err)
	}
	evicted := from.Valid && packed > from.Int64
	back := readmitted.Valid && packed >= readmitted.Int64
	return evicted && !back, nil
}
