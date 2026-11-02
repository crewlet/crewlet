package chatlet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE VERDICT'S WRITE HALF: the applier recording what it decided about a
// message record, inside the record's own apply transaction, and pruning what
// has passed the horizon by the record's broker instant. Beside the applier
// rather than beside [Verdict] because this is a write of the replicated
// estate, which only an applier makes — and the walk that holds that rule
// reads the applier's files by name.

// VerdictRetention is how long a verdict is kept, by the broker's clock.
//
// THE OPERATION LEDGER'S OWN HORIZON ([statelog.OpsRetention], thirty days),
// because every read of a verdict happens inside it: the writer reads its own
// the moment its applier reaches the record, a retry the ledger answers is
// answered only while its operation is inside the mint horizon
// ([statelog.MintHorizon], a day inside this), and a second copy of an
// operation — the one the applier itself reads it for — is published inside
// that same window. Kept for less, a retry of a refused post would read a
// verdict that was gone and be answered with nothing; kept for longer, it is
// rows nobody can read. Measured by the broker's instant rather than any
// node's clock, which is what lets every node prune the same rows.
const VerdictRetention = statelog.OpsRetention

// verdictPruneBatch is how many expired verdicts one record's apply removes at
// most.
//
// SIXTY-FOUR, and the bound is what keeps a record's apply cheap after a quiet
// month, when everything older than the horizon is due at once. Every message
// record writes ONE verdict and removes up to sixty-four, so the table cannot
// outgrow its horizon however the traffic is shaped — the prune falls behind
// only for a moment after a burst, and the next sixty-four records catch it
// up.
const verdictPruneBatch = 64

// writeVerdict records what the applier decided about the record at, and
// prunes the verdicts past the horizon by the record's own broker instant.
//
// ON CONFLICT DO NOTHING: the position is the row id, and the only record that
// can write one is the record at it, whose verdict is a pure function of the
// record and the rows — so a conflict is the same row, never another.
//
// # The prune says how far it reached
//
// A PRUNED VERDICT AND A DROPPED RECORD LEAVE THE SAME ABSENCE, and a reader
// told one when it was the other answers a month-old post as one every node's
// gate dropped. So the prune records the highest position it removed
// (`chat_verdicts_lost`, replicated migration 0070) in the statement list that
// removes it — read first, over the very rows the delete then takes, inside
// one transaction — and [verdictIn] answers an absent verdict at or below it
// as one nothing here can vouch for. The framework's ledger keeps its own
// watermark the same way and for the same reason ([statelog.Rows.LostBefore]).
// It moves only forward: the largest of what the row held and this prune.
func writeVerdict(ctx context.Context, tx *sql.Tx, at applyContext, channelID string,
	v Verdict) (int, error) {

	res, err := tx.ExecContext(ctx, verdictInsertStatement, at.packed, at.record.OpID,
		channelID, string(v), store.EncodeTime(at.brokerAt))
	if err != nil {
		return 0, fmt.Errorf("chatlet: record the verdict of the %s at %s: %w",
			at.record.Op, at.position, err)
	}
	written, _ := res.RowsAffected()
	if at.brokerAt.IsZero() {
		return int(written), nil
	}
	cutoff := store.EncodeTime(at.brokerAt.Add(-VerdictRetention))
	var through sql.NullInt64
	err = tx.QueryRowContext(ctx, verdictPruneReachStatement, cutoff,
		verdictPruneBatch).Scan(&through)
	if err != nil {
		return 0, fmt.Errorf("chatlet: read how far the prune past the horizon at %s "+
			"reaches: %w", at.position, err)
	}
	if !through.Valid {
		// NOTHING IS PAST THE HORIZON: no verdict goes, and the
		// watermark stays where the last prune left it.
		return int(written), nil
	}
	res, err = tx.ExecContext(ctx, verdictPruneStatement, cutoff, verdictPruneBatch)
	if err != nil {
		return 0, fmt.Errorf("chatlet: prune the verdicts past the horizon at %s: %w",
			at.position, err)
	}
	pruned, _ := res.RowsAffected()
	res, err = tx.ExecContext(ctx, verdictLostStatement, through.Int64)
	if err != nil {
		return 0, fmt.Errorf("chatlet: record that the verdicts through %s were "+
			"pruned at %s: %w", statelog.Unpack(at.position.Stream, through.Int64),
			at.position, err)
	}
	marked, _ := res.RowsAffected()
	return int(written + pruned + marked), nil
}

// decidedBefore reports whether an operation already has a verdict at a
// position below the record at: the question a second copy of it is answered
// by. BELOW, never anywhere: the record's own verdict, met again by a replay of
// the same position, is its own decision rather than an earlier one.
func decidedBefore(ctx context.Context, tx *sql.Tx, at applyContext) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, verdictDecidedStatement,
		at.record.OpID, at.packed).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("chatlet: read whether operation %s was decided before %s: %w",
			at.record.OpID, at.position, err)
	}
	return true, nil
}

// The statements over the verdicts and the prune's watermark, named so the
// plan gate holds each to the index it was written for
// (verdictplan_internal_test.go).
const (
	verdictInsertStatement = `
		INSERT INTO chat_verdicts (position, op_id, channel_id, verdict, at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (position) DO NOTHING`

	verdictDecidedStatement = `
		SELECT 1 FROM chat_verdicts WHERE op_id = ? AND position < ? LIMIT 1`

	verdictReadStatement = `
		SELECT verdict FROM chat_verdicts WHERE op_id = ?
		ORDER BY position LIMIT 1`

	verdictPruneStatement = `
		DELETE FROM chat_verdicts WHERE position IN (
			SELECT position FROM chat_verdicts WHERE at < ?
			ORDER BY at, position LIMIT ?)`

	// The highest position [verdictPruneStatement] is about to remove: the
	// same rows, in the same order, under the same bound.
	verdictPruneReachStatement = `
		SELECT MAX(position) FROM (
			SELECT position FROM chat_verdicts WHERE at < ?
			ORDER BY at, position LIMIT ?)`

	verdictLostStatement = `
		INSERT INTO chat_verdicts_lost (id, through) VALUES (1, ?)
		ON CONFLICT (id) DO UPDATE SET through = MAX(through, excluded.through)`

	verdictLostReadStatement = `
		SELECT through FROM chat_verdicts_lost WHERE id = 1`
)
