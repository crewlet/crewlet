package chatlet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The applier is a PURE FUNCTION of the log, so N nodes derive byte-identical tables:
//  1. It reads no config and no clock; everything arrives in [statelog.ApplyOptions].
//  2. It writes only what the record says, except the room's sequence below.
//  3. Every derived instant is a max over a set, never a fold over arrival order.
//  4. Every upsert carries its version guard and SKIPS on a stale one: a
//     constraint violation would abort identically on every node and stall the log.
//
// A message's `channel_seq` is MINTED from the room's high-water mark inside this
// transaction; a writer cannot carry it (two would compute the same number). So a
// message record's scope is its channel: a deferral must block the room, or a node
// stepping over it would give the next post the deferred one's number. See [TermKind].
// No wake is published here ([github.com/crewlet/crewlet/internal/changefeed] derives it); only the browser's live frame is ([Observer]).

// Applier writes this node's copy of the company's conversation.
type Applier struct {
	// NodeID is this node's own id, which the eviction gate compares a
	// record's writer against.
	NodeID string

	// live is the post-commit accumulator. Never nil; a nil OBSERVER
	// inside it is the no-op a test that renders no room runs.
	live *accumulator
}

// NewApplier builds the chat applier for one node.
//
// obs may be nil, and is called AFTER the transaction commits — never from
// inside it, because the store's transactions are optimistic and a conflicted
// one re-runs its body, so a callback in Apply would fire twice for one
// record.
func NewApplier(nodeID string, obs Observer) *Applier {
	return &Applier{NodeID: nodeID, live: newAccumulator(obs)}
}

// Committed is the post-commit half: it drains the live accumulator.
//
// THE ONLY CONSEQUENCE OF A CHAT RECORD THAT IS NOT A ROW. The framework calls
// this once, after the commit — so an entry gathered by a transaction that
// FAILED would otherwise drain here for rows that were never written, which is
// what [accumulator.discard] exists to prevent.
func (a *Applier) Committed(context.Context) { a.live.drain() }

// Gated reports a record that must produce no rows at all.
//
// ONE GATE IS ANSWERABLE HERE: THE EVICTION GATE. A record written by a node the
// fleet evicted before the record's position is dropped everywhere. It reads only
// the log's order and the ENVELOPE, so it holds when coordination is unreachable
// and for a record this build cannot decode.
//
// The erase marker is not a gate: a message record's subject names the ROOM
// ([KindMessage]), so the ids to compare against [chat_deletions] live in a
// PAYLOAD, unreadable at an unknown version; a gate decoding it would say "not
// gated" for exactly the records a rolling upgrade cannot read. [messageErased]
// consults it where the payload is decoded.
func (a *Applier) Gated(ctx context.Context, tx *sql.Tx, rec statelog.Record) (
	statelog.Reason, bool, error) {

	if rec.Writer == "" {
		return "", false, nil
	}
	var from, readmitted sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT from_position, readmitted_position
		FROM chat_evictions WHERE node_id = ?`, rec.Writer).
		Scan(&from, &readmitted)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		a.live.discard()
		return "", false, fmt.Errorf("chatlet: read the eviction gate for node "+
			"%s: %w", rec.Writer, err)
	}
	// THE WINDOW IS HALF-OPEN AT BOTH ENDS, and both ends matter: a record
	// at or below the eviction's own position was written while the node
	// was still counted, and one at or above a readmission is written by a
	// node the fleet has taken back.
	at := rec.Position.Packed()
	evicted := from.Valid && at > from.Int64
	back := readmitted.Valid && at >= readmitted.Int64
	if evicted && !back {
		return statelog.ReasonEvicted, true, nil
	}
	return "", false, nil
}

// Apply writes one record's rows.
//
// The dispatch is on the SUBJECT KIND and then on the OP, and every pair this
// build writes has a case — including the one that writes nothing, which is a
// case rather than a default so that a kind added later without one is a
// visible hole rather than a silently ignored record.
func (a *Applier) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record,
	opts statelog.ApplyOptions) (int, error) {

	rows, err := a.apply(ctx, tx, rec, opts)
	if err != nil {
		// THE LIVE BATCH GOES WITH THE TRANSACTION. Committed runs only
		// on success, so entries gathered by a batch that failed would
		// drain on a later commit and announce rows nobody wrote.
		a.live.discard()
	}
	return rows, err
}

func (a *Applier) apply(ctx context.Context, tx *sql.Tx, rec statelog.Record,
	opts statelog.ApplyOptions) (int, error) {

	record, err := Decode(rec.Payload)
	if err != nil {
		return 0, fmt.Errorf("chatlet: decode the record at %s: %w", rec.Position, err)
	}
	at := applyContext{
		record:       record,
		position:     rec.Position,
		packed:       rec.Position.Packed(),
		brokerAt:     opts.StoredAt,
		epoch:        opts.Epoch,
		maxVariables: opts.MaxVariables,
	}

	switch ObjectKind(rec.Subject.Kind) {
	case KindBarrier:
		// THE READ INDEX'S OWN APPEND. It writes no row on any node, and
		// that is its entire content: it exists so a linearizable read
		// has a quorum-committed position to wait through.
		return 0, nil
	case KindTrimmed:
		// THE TRIM'S OWN RECORD. The framework removes this log's anchors
		// below the floor its subject names, in this same transaction
		// (ADR-0080); this domain writes nothing for it.
		return 0, nil
	case KindEviction:
		return a.applyEviction(ctx, tx, at)
	case KindGeneration:
		return a.applyGeneration(ctx, tx, at)
	case KindChannelName:
		return a.applyChannelName(ctx, tx, at)
	case KindChannel:
		return a.applyChannel(ctx, tx, at)
	case KindMessage:
		return a.applyMessage(ctx, tx, at)
	}
	// A KIND THIS BUILD DOES NOT KNOW REACHES HERE ONLY BY WAY OF A RECORD
	// AT A VERSION IT CAN READ, which is a writer publishing a kind it
	// never declared. Retaining it would file it under a kind nothing will
	// ever apply; failing is what makes the writer's mistake visible.
	return 0, fmt.Errorf("chatlet: %s is not a kind this build applies, and the "+
		"record at %s claims version %d — a kind is declared before it is "+
		"published", rec.Subject.Kind, rec.Position, record.V)
}

// applyContext is what every case needs, gathered once.
type applyContext struct {
	record   MutationRecord
	position statelog.Position

	// packed is the composed position, which is the version every object
	// row's guard compares against.
	packed int64

	// brokerAt is the broker's own instant for this record. THE APPLIER
	// NEVER READS A CLOCK: this is what makes an effective instant
	// byte-identical on every node, and it is what the retention prune's
	// time range is decidable against.
	//
	// It is the ONLY instant this domain writes. The record's own authored
	// instant reaches no row: two people posting a second apart on nodes
	// whose clocks disagree by a minute would otherwise render against an
	// order the log does not have, differently on every node.
	brokerAt time.Time

	epoch map[string]any

	// maxVariables is the engine's probed bind-parameter limit, carried
	// here because it is what sizes a multi-row INSERT and an applier
	// holds a transaction and nothing else.
	//
	// IT IS NOT AN INPUT TO WHAT THE ROWS SAY: it decides how many
	// statements a collection is written in, never which rows land or in
	// what order, so two nodes probing different limits still write
	// byte-identical tables. A ZERO is an unset field rather than a limit
	// — [store.RowsPerInsert] falls to one row per statement.
	maxVariables int
}

// subject is the record's own subject.
func (c applyContext) subject() Subject { return c.record.Subject }

// live builds the frame for a record that changed a room, filled in by the
// caller with whatever is particular to its own case.
func (c applyContext) live(channelID string) Applied {
	return Applied{
		Position:  c.position,
		Subject:   c.subject(),
		Op:        c.record.Op,
		OpID:      c.record.OpID,
		ChannelID: channelID,
		Actor:     c.record.Actor,
		ActorKind: c.record.ActorKind,
		At:        c.brokerAt,
		Notify:    c.record.Notify,
	}
}

// applyEviction records a node's removal from this log, or its readmission.
//
// A READMISSION IS AN INVERSE COMMIT rather than a delete, so an eviction's
// whole history survives a replay — and a node that was evicted, readmitted
// and evicted again reads correctly rather than as one long absence.
func (a *Applier) applyEviction(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	e, ok := payload.(Eviction)
	if !ok {
		return 0, fmt.Errorf("chatlet: the eviction at %s carries a %T",
			at.position, payload)
	}
	if e.NodeID == "" {
		return 0, fmt.Errorf("chatlet: the eviction at %s names no node", at.position)
	}
	if e.Readmitted {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		res, err := tx.ExecContext(ctx, `
			UPDATE chat_evictions
			SET readmitted_position = ?, version = ?
			WHERE node_id = ? AND version < ?`,
			at.packed, at.packed, e.NodeID, at.packed)
		if err != nil {
			return 0, fmt.Errorf("chatlet: readmit node %s at %s: %w",
				e.NodeID, at.position, err)
		}
		n, _ := res.RowsAffected()
		return int(n), nil
	}
	// A SECOND EVICTION CLEARS THE READMISSION, which is what makes the
	// evicted-readmitted-evicted sequence read as three facts rather than
	// as one window with a hole in it.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO chat_evictions
			(node_id, at, by, from_position, readmitted_position, version)
		VALUES (?, ?, ?, ?, NULL, ?)
		ON CONFLICT (node_id) DO UPDATE SET
			at = excluded.at, by = excluded.by,
			from_position = excluded.from_position,
			readmitted_position = NULL,
			version = excluded.version
		WHERE excluded.version > chat_evictions.version`,
		e.NodeID, store.EncodeTime(at.brokerAt), e.EvictedBy, at.packed, at.packed)
	if err != nil {
		return 0, fmt.Errorf("chatlet: evict node %s at %s: %w",
			e.NodeID, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// applyGeneration writes a reanchor's audit row.
func (a *Applier) applyGeneration(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	g, ok := payload.(Generation)
	if !ok {
		return 0, fmt.Errorf("chatlet: the generation record at %s carries a %T",
			at.position, payload)
	}
	// CREATE-ONLY. Two operators deriving the same number race at the
	// broker and exactly one wins; the loser's record never applies, so a
	// conflict here is a redelivery of the winner's.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO chat_log_generations
			(generation, at, by, new_stream_created_at, prev_last_seq_seen,
			 record_id)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (generation) DO NOTHING`,
		g.Generation, store.EncodeTime(at.brokerAt), g.By,
		store.EncodeTime(g.StreamCreatedAt), int64(g.PrevHighest),
		at.record.OpID)
	if err != nil {
		return 0, fmt.Errorf("chatlet: record generation %d at %s: %w",
			g.Generation, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// messageErased reports whether a compliance erase destroyed this message —
// EXCEPTING the record that wrote the marker, by its own operation id.
//
// THE MARKER OUTLIVES EVERY OTHER ROW the message had, so a node can tell a
// message that never existed from one the company destroyed. Every path that
// writes a message row asks this first, so a late redelivery cannot resurrect it.
//
// BY OPERATION ID, NOT OP KIND: "any erase" would let a SECOND erase through,
// and by committed sequence would fail for a republished copy after a reanchor,
// leaving its holder unable to act on its own record. Only the erase that wrote
// the marker can carry its op id.
func messageErased(ctx context.Context, tx *sql.Tx, messageID, opID string) (bool, error) {
	var author sql.NullString
	err := tx.QueryRowContext(ctx,
		`SELECT op_id FROM chat_deletions WHERE message_id = ?`, messageID).
		Scan(&author)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("chatlet: read the erase marker for message "+
			"%s: %w", messageID, err)
	}
	// THE MARKER'S OWN AUTHOR is the one record that may still act: the
	// erase that wrote the marker has to be able to remove the rows it
	// is erasing. Named rather than negated inline, because the De
	// Morgan form of this reads as three unrelated refusals instead of
	// one exception.
	ownMarker := author.Valid && opID != "" && author.String == opID
	return !ownMarker, nil
}

// nullInstant is a nullable time column's value: NULL for an absent one.
//
// A POINTER RATHER THAN A ZERO TIME, because every nullable instant in this
// schema — `edited_at`, `deleted_at`, `archived_at` — distinguishes "never"
// from "at the zero instant", and [store.EncodeTime] of a zero time is a very
// large negative number that every range predicate reads as long ago.
func nullInstant(t *time.Time) any {
	if t == nil {
		return nil
	}
	return store.EncodeTime(*t)
}
