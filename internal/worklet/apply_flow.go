package worklet

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/store"
)

// applyFlow records an item's flow, or a notice of it, into `tracker_flow`
// (ADR-0162), and writes the notice's one inbox row.
//
// GATED ON ITS KEY'S INSERT, as a turn is on its id's: a record carrying a key
// already recorded, published again by a duty that had not yet read its first
// copy, writes nothing, so a notice reaches an inbox once. It touches no row of
// the item's own: the item's version, history and `updated_at` are what the
// flow is read from, and a record moving them would change the state it
// records.
func (a *Applier) applyFlow(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	var flow FlowRecord
	if err := decodePayload(c.record.Mutation, &flow); err != nil {
		return 0, fmt.Errorf("worklet: decode the flow record at %s: %w", c.position, err)
	}
	if flow.ID == "" || flow.Task != c.subject().ID {
		return 0, fmt.Errorf("worklet: the flow record at %s keys %q and names item %q "+
			"on subject %s: a writer's bug, since the writer refuses both",
			c.position, flow.ID, flow.Task, c.subject())
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO tracker_flow
			(id, task_id, state, cause, since, until, seat, notice, told, backfilled,
			 log_seq, log_stream, log_generation, created_at, recorded_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (id) DO NOTHING`,
		flow.ID, flow.Task, string(flow.State), string(flow.Cause),
		nullableTime(flow.Since), nullableTime(flow.Until), flow.Seat,
		string(flow.Notice), flow.Told, boolInt(flow.Backfilled),
		c.packed, c.position.Stream, c.position.Generation,
		store.EncodeTime(c.authored()), store.EncodeTime(c.brokerAt))
	if err != nil {
		return 0, fmt.Errorf("worklet: record the flow at %s: %w", c.position, err)
	}
	inserted, err := affected(res)
	if err != nil || inserted == 0 {
		// A redelivery, or the same fact published twice.
		return 0, err
	}
	notify := c.record.Notify
	if notify == nil {
		return inserted, nil
	}
	noticed, err := a.writeInbox(ctx, tx, c, flow.ID, notify.Snapshot.Key)
	if err != nil {
		return 0, err
	}
	return inserted + noticed, nil
}
