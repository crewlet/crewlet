package statelog

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// WHAT A RESTORED LOG HOLDS THAT THE ROWS DO NOT.
//
// A reanchor of a broker restored from an older copy follows the log from its
// END ([ReanchorRestored]): the rows already hold every record the copy kept,
// and replaying them into a new generation would roll every object back. What
// that assumes is that the log holds nothing ELSE — and it can. A node whose
// rows were the copy's age sees a log that is its own history and writes it
// before anybody re-anchors, and every one of those records sits below the end
// the reanchor follows from: applied on no node, and lost to whoever it was
// acknowledged to, with nothing anywhere saying so.
//
// # Why one record answers for all of them
//
// On a restored log every record past the copy was written after the restore,
// so the records these rows hold are a prefix of it and the ones they do not
// are its tail. So the question is asked of the NEWEST record that writes rows:
// if these rows applied it, nothing written after the restore wrote any, and
// following the log from its end discards nothing but records that write
// nothing. A barrier is the one kind that writes nothing whatever its history —
// a linearizable read's append, which a node whose rows are the copy's age
// goes on making — and is stepped over.
//
// # How "these rows applied it" is established
//
// From the operation ledger, by the operation, the position AND the broker's
// storage instant ([tables.appliedRecord]) — or from the retained records, by
// position and instant, for one this build could not read. The ledger TRAVELS
// inside every snapshot ([Domain.OpsTable]), instant and all, so a record
// these rows hold because a donor applied it is named there exactly as one
// this node applied itself: every node writes the same row from the same
// record, and the broker's instant is the record's own rather than a node's.
//
// Every answer that is not "yes" is "no", and "no" is not always conclusive.
// The ledger can LOSE rows — to its own sweep ([OpsRetention]), or with a
// snapshot from a donor on an older build, which scrubbed it — and it says how
// far back it may have ([Rows.LostBefore]): for an operation minted before
// that watermark, absence says nothing, and the record is named with the
// watermark ([TailRecord.LedgerLostBefore]) so the refusal can say its rows
// may hold it after all. The other false "no" is a record a gate dropped,
// which writes no row. Each of them makes the reanchor refuse when it need
// not, and the operator's word ([ReanchorGuard.Discard]) is what a false
// refusal costs; none of them can make it skip a record without saying so,
// which is the one failure this exists to rule out.

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
	// the instant this node's operation ledger may have lost rows from — so
	// its absence there says nothing, and these rows may hold the record
	// after all. Zero when the ledger's silence is conclusive: it has lost
	// nothing, or nothing from before this operation. See the file doc.
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

// UnheldTail walks a log back from last to first and answers the newest record
// on it that writes rows and that this node's rows — at generation gen — do not
// hold, or nil when every such record on it is one they hold (or it has none).
// See this file's doc for why that one record is the whole question.
//
// A record the log cannot say anything about — its envelope does not decode —
// is an error: whether it writes rows cannot be told, and a reanchor that
// cannot tell refuses rather than guesses.
func UnheldTail(ctx context.Context, d Domain, db Estate, log LogReader, gen uint32,
	first, last uint64) (*TailRecord, error) {

	t, err := newTables(d)
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
		if env.Subject.Kind == BarrierKind {
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
			// NOT HELD, and whether that is conclusive is the watermark's
			// to say — read in the same transaction as the ledger, so a
			// sweep cannot land between the two. An operation id that
			// carries no instant reads as minted at the zero one, before
			// every loss, exactly as the publisher reads it.
			bound, lost, readErr := t.lostBefore(ctx, tx)
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
