package statelog

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// A restored reanchor follows the log from its END ([ReanchorRestored]),
// assuming the log holds nothing these rows lack; but a node whose rows were
// the copy's age may have written past it, records then lost silently. Past
// the copy every record postdates the restore, so the rows hold a prefix of the
// log and the question is its NEWEST row-writing record. Framework records
// (barriers; trimmed records, ADR-0080) write no domain row and carry no
// operation id, so they are stepped over. "Held": the ledger, which travels in
// snapshots ([Domain.OpsTable]) so a donor's applies count, has the operation,
// position AND broker instant ([tables.appliedRecord]), or a retained record
// matches. A "no" may be false (lost ledger rows, [TailRecord.LedgerLostBefore];
// a gate dropped the record), costing a refusal the operator overrides
// ([ReanchorGuard.Discard]); no answer skips a record unsaid.

// TailRecord is one record on a log, as a restored reanchor names it: the
// newest record that writes rows and that this node's rows do not hold.
type TailRecord struct {
	Seq      uint64    `json:"seq"`
	Kind     string    `json:"kind"`
	Subject  string    `json:"subject"`
	Writer   string    `json:"writer,omitempty"`
	OpID     string    `json:"op_id,omitempty"`
	StoredAt time.Time `json:"stored_at"`

	// LedgerLostBefore is set when the record's operation was minted before
	// the instant this node's ledger may have lost rows from ([Rows.LostBefore]),
	// so its absence there says nothing and these rows may hold the record
	// after all. Zero when the ledger's silence is conclusive.
	LedgerLostBefore time.Time `json:"ledger_lost_before,omitzero"`
}

// String names the record for a refusal.
func (r TailRecord) String() string {
	writer := r.Writer
	if writer == "" {
		writer = "a writer that did not say who it was"
	}
	return fmt.Sprintf("%s on %s at sequence %d, written by %s as operation %q",
		r.Kind, r.Subject, r.Seq, writer, r.OpID)
}

// UnheldTail walks a log back from last to first and answers its newest record
// that writes rows and that this node's rows, at generation gen, do not hold,
// or nil when they hold it (or there is none); this file's doc says why that
// one record is the whole question.
//
// A record whose envelope does not decode is an error: whether it writes rows
// cannot be told, and a reanchor refuses rather than guesses. log is a
// [VerifiedLog], since the broker holds signed frames that decode as nothing.
func UnheldTail(ctx context.Context, d Domain, spec StreamSpec, db ReadEstate, log VerifiedReader,
	gen uint32, first, last uint64) (*TailRecord, error) {

	t, err := newTables(d, spec)
	if err != nil {
		return nil, err
	}
	for seq := last; seq >= max(first, 1); seq-- {
		_, payload, storedAt, ok, readErr := log.At(ctx, seq)
		if readErr != nil {
			return nil, fmt.Errorf("statelog: read %s's record at %d: %w", t.stream, seq, readErr)
		}
		if !ok {
			// A HOLE: nothing at this sequence writes anything.
			continue
		}
		env, decodeErr := d.Envelope(payload)
		if decodeErr != nil {
			return nil, fmt.Errorf("statelog: %s's record at %d does not decode, so "+
				"whether it writes rows cannot be told: %w", t.stream, seq, decodeErr)
		}
		if frameworkRecord(env.Subject.Kind) {
			continue
		}
		at := Position{Stream: t.stream, Generation: gen, Seq: seq}
		held := false
		var lostBefore time.Time
		if err := db.Read(ctx, func(tx *sql.Tx) error {
			var readErr error
			if held, readErr = t.appliedRecord(ctx, tx, env.OpID, at, storedAt); readErr != nil || held {
				return readErr
			}
			if held, readErr = t.retainedRecord(ctx, tx, at, storedAt); readErr != nil || held {
				return readErr
			}
			// NOT HELD; the watermark, read in the ledger's transaction so
			// no sweep lands between, says whether that is conclusive. An
			// id with no instant reads as minted at zero, before every
			// loss, as the publisher reads it.
			bound, lost, readErr := t.lostBefore(ctx, tx, env.Subject.Kind)
			if readErr == nil && lost && mintedAt(env.OpID).Before(bound) {
				lostBefore = bound.UTC()
			}
			return readErr
		}); err != nil {
			return nil, err
		}
		if held {
			return nil, nil
		}
		return &TailRecord{
			Seq: seq, Kind: env.Kind, Subject: env.Subject.String(), Writer: env.Writer,
			OpID: env.OpID, StoredAt: storedAt.UTC(), LedgerLostBefore: lostBefore,
		}, nil
	}
	return nil, nil
}
