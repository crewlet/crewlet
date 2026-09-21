package chat

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The applier, and the four rules that make it a PURE FUNCTION of the log.
//
// N nodes derive one SQL state from one ordered stream, and the claim that
// their tables are byte-identical is checkable — a checksum over the
// replicated file. That claim survives exactly as long as these four hold, and
// they are the tracker's and the wiki's because they are properties of the
// FRAMEWORK rather than of any one domain:
//
//  1. IT READS NO CONFIG AND NO CLOCK. Everything it would otherwise read
//     arrives in [statelog.ApplyOptions]: the batch's instant, the broker's own
//     timestamp, and the epoch values the domain declared it reads.
//  2. IT WRITES ONLY WHAT THE RECORD SAYS — with one exception this domain
//     names below, which is the room's sequence.
//  3. EVERY DERIVED INSTANT IS A MAX OVER A SET, never a fold over an arrival
//     order. Two nodes at one checkpoint have seen the same set and a different
//     order, so a fold gives them different answers.
//  4. EVERY UPSERT CARRIES ITS VERSION GUARD, and the guard is a SKIP rather
//     than an error: a redelivered record is ordinary traffic, and a constraint
//     violation inside this transaction would abort it identically on every
//     node and stall the whole fleet's log.
//
// # The one value this applier MINTS rather than copies
//
// Rule 2 has an exception here that neither of the other domains has: a
// message's `channel_seq` is minted from the ROOM'S OWN high-water mark, read
// inside this transaction, rather than carried on the record. A writer cannot
// carry it — posts are additive, so two writers in one room form no
// expectation and would compute the same number — and the applier can, because
// every node applies the same records in the same order.
//
// That is why a message record's declared SCOPE is its channel: a deferral on
// one post must block the room rather than one message in it, or a node that
// stepped over a deferred post would hand the next one the number the deferred
// record should have had, and its rows would differ from every other node's for
// the life of the deployment. See the package doc and [TermKind].
//
// # What is NOT here
//
// NO WAKE IS PUBLISHED. A wake is derived from the committed record by
// something that outlives the writer — see
// [github.com/crewlet/crewlet/internal/changefeed] — rather than published by
// the applying goroutine as a courtesy. What IS published from here is the
// live frame a browser renders, which is a different promise made to a
// different audience: see [Observer].

// Applier writes this node's copy of the company's conversation.
type Applier struct {
	// NodeID is this node's own id, which the eviction gate compares a
	// record's writer against.
	NodeID string

	// live is the post-commit accumulator. Never nil; a nil OBSERVER
	// inside it is the no-op every test and every node without a
	// dashboard attached runs.
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
// ONE GATE IS ANSWERABLE HERE, and which one is decided by what a gate is
// allowed to read rather than by what this domain would like to check:
//
// THE EVICTION GATE. A record written by a node the fleet evicted before the
// record's own position is dropped everywhere. It depends on nothing but the
// log's own order and the record's ENVELOPE, which is what makes it the fence
// that holds when coordination cannot be reached at all — and what makes it
// answerable for a record this build cannot decode.
//
// # Why the erase marker is not a gate, although the wiki's purge is
//
// The wiki gates a purged page here because A PAGE RECORD'S SUBJECT NAMES ITS
// OWN PAGE. A chat MESSAGE record's subject names the ROOM — that is the whole
// hot-path design, [KindMessage] — so the message ids a gate would compare
// against [chat_deletions] live in a PAYLOAD, which a record at an unknown
// version does not let anyone read. A gate that decoded the payload would
// answer "not gated" for exactly the records a rolling upgrade cannot read,
// which is the one case it exists for.
//
// So the erase marker is consulted where the payload is decoded: in the apply,
// by [messageErased], before any path writes or changes a message row. The
// wiki states the same asymmetry about its title records, and for the same
// reason.
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
		return "", false, fmt.Errorf("chat: read the eviction gate for node "+
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
		return 0, fmt.Errorf("chat: decode the record at %s: %w", rec.Position, err)
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
	return 0, fmt.Errorf("chat: %s is not a kind this build applies, and the "+
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
	// It is also the ONLY instant this domain writes, with exactly one
	// exception — an IMPORTED message's `authored_at`, which is display
	// only and which nothing here orders on. The record's own authored
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

// excerpt is what a card renders for this change, and it comes from the
// ROUTING SNAPSHOT or from nowhere.
//
// THE APPLIER INVENTS NO TEXT. A quiet record — an import, a prune, anything
// that wakes nobody — carries no excerpt, and cutting one out of the payload
// here would be a second spelling of [Excerpt] that could disagree with the
// one the wake renders.
func (c applyContext) excerpt() string {
	if c.record.Notify == nil {
		return ""
	}
	return c.record.Notify.Excerpt
}

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
		return 0, fmt.Errorf("chat: the eviction at %s carries a %T",
			at.position, payload)
	}
	if e.NodeID == "" {
		return 0, fmt.Errorf("chat: the eviction at %s names no node", at.position)
	}
	if e.Readmitted {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		res, err := tx.ExecContext(ctx, `
			UPDATE chat_evictions
			SET readmitted_position = ?, version = ?
			WHERE node_id = ? AND version < ?`,
			at.packed, at.packed, e.NodeID, at.packed)
		if err != nil {
			return 0, fmt.Errorf("chat: readmit node %s at %s: %w",
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
		return 0, fmt.Errorf("chat: evict node %s at %s: %w",
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
		return 0, fmt.Errorf("chat: the generation record at %s carries a %T",
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
		return 0, fmt.Errorf("chat: record generation %d at %s: %w",
			g.Generation, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// HistoryKind is what a history row's `kind` column holds: the (subject kind,
// op) PAIR, rendered.
//
// ONE SPELLING, EXPORTED, because two readers that cannot see each other
// compare against it — the applier writes it and every activity filter and
// audit window selects on it. Written twice, the filter for "rooms created"
// stops matching the rows the applier writes and the screen goes quietly
// empty.
//
// THE PAIR RATHER THAN A VOCABULARY OF ITS OWN, which is why this domain has
// no change-kind enum at all: the op alone would not do it, because `create`
// names both a room and the address it was claimed under, and a filter that
// could not tell them apart would report a company's rooms twice. See
// [ObjectKind.RecordsHistory].
func HistoryKind(kind ObjectKind, op OpKind) string {
	return string(kind) + "." + string(op)
}

// writeHistory writes one entry of what an activity feed and a digest render
// from.
//
// CREATE-ONLY ON THE OPERATION ID, which is what makes it idempotent without a
// version guard: the row is the record's own account of itself, so a
// redelivery has nothing to add and nothing to correct.
func (a *Applier) writeHistory(ctx context.Context, tx *sql.Tx, at applyContext,
	channelID, messageID string) (int, error) {

	if at.record.OpID == "" {
		// NO OP ID, NO HISTORY ROW. The only record without one is the
		// barrier, which never reaches here — so this is a writer that
		// omitted it, and inventing an id would make the row
		// unrepeatable across a replay.
		return 0, fmt.Errorf("chat: the record at %s writes a history entry "+
			"and carries no operation id, which is what the row is keyed on",
			at.position)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO chat_history
			(id, channel_id, message_id, kind, actor, actor_kind, excerpt,
			 broker_at, log_stream, log_generation, log_seq)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		at.record.OpID, channelID, messageID,
		HistoryKind(at.subject().Kind, at.record.Op),
		at.record.Actor, string(at.record.ActorKind), at.excerpt(),
		store.EncodeTime(at.brokerAt), at.position.Stream,
		at.position.Generation, int64(at.position.Seq))
	if err != nil {
		return 0, fmt.Errorf("chat: write the history entry for room %s at "+
			"%s: %w", channelID, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// messageErased reports whether a compliance erase destroyed this message —
// EXCEPTING the record that wrote the marker, by its own operation id.
//
// THE MARKER OUTLIVES EVERY OTHER ROW the message had, which is what lets a
// node tell a message that never existed from one the company destroyed. Every
// path that would write or change a message row asks this first, so a
// redelivery months later cannot resurrect what an operator deliberately
// removed.
//
// BY OPERATION ID AND NOT BY OP KIND. "Any erase" would let a SECOND erase
// through, and an erase is the one gesture that destroys rows; by the
// committed sequence would fail for a republished copy after a reanchor,
// leaving a node holding only that copy unable to act on its own record at
// all. The only record that can legitimately carry a marker's op id is the
// erase that wrote it.
func messageErased(ctx context.Context, tx *sql.Tx, messageID, opID string) (bool, error) {
	var author sql.NullString
	err := tx.QueryRowContext(ctx,
		`SELECT op_id FROM chat_deletions WHERE message_id = ?`, messageID).
		Scan(&author)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("chat: read the erase marker for message "+
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
