package chat_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The applier's own guards, over a real replicated estate.
//
// [statelogtest] certifies what the FRAMEWORK relies on — that an applier is
// deterministic, that it is idempotent at a position, the deferral contract,
// the table classes. What it cannot know is what a CONVERSATION means, and
// every case here is one of those: the room's minted sequence, the tombstone
// that keeps a thread's root, the marker that makes an erase permanent, the
// prune's time range, and the live batch a re-run transaction must not push
// twice.
//
// THE HELPERS ARE PREFIXED `apply…` because this package's suites are several
// files written against one test package, and a `harness` here would be a
// collision with the write path's rather than a shared fixture.

// applyBrokerAt is the broker's own instant for a record, which is the ONLY
// clock this domain writes. Every case that needs a second instant states it.
var applyBrokerAt = time.Date(2031, 4, 2, 3, 14, 0, 0, time.UTC)

// applyHarness is one node's estate, its applier and its live observer.
type applyHarness struct {
	t            *testing.T
	db           *store.DB
	applier      *chat.Applier
	live         *applyRecorder
	seq          uint64
	maxVariables int
}

// applyRecorder is the live seam, captured.
type applyRecorder struct{ batches [][]chat.Applied }

func (r *applyRecorder) Applied(batch []chat.Applied) {
	r.batches = append(r.batches, batch)
}

// frames is every [chat.Applied] the observer has been handed, in the order
// it was handed them.
func (r *applyRecorder) frames() []chat.Applied {
	var out []chat.Applied
	for _, batch := range r.batches {
		out = append(out, batch...)
	}
	return out
}

func newApplyHarness(t *testing.T) *applyHarness {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"),
		store.Options{PinnedWriters: 1})
	if err != nil {
		t.Fatalf("open a store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	live := &applyRecorder{}
	return &applyHarness{
		t: t, db: db, applier: chat.NewApplier("node-a", live), live: live,
		maxVariables: db.Replicated().Caps().MaxVariables,
	}
}

// apply runs one record at the next position and commits it.
func (h *applyHarness) apply(rec chat.MutationRecord) (int, statelog.Reason, error) {
	h.t.Helper()
	return h.applyStoredAt(rec, applyBrokerAt)
}

// applyStoredAt runs one record at the next position with a stated broker
// instant, which is what the retention prune's range is decidable against.
func (h *applyHarness) applyStoredAt(rec chat.MutationRecord, storedAt time.Time) (
	int, statelog.Reason, error) {

	h.t.Helper()
	h.seq++
	return h.applyAt(rec, h.seq, storedAt)
}

// applyAt runs one record at a stated position, so a case can REDELIVER a
// record at a later one — which is what a reanchor and a redelivered
// acknowledgement both look like from here.
func (h *applyHarness) applyAt(rec chat.MutationRecord, seq uint64,
	storedAt time.Time) (rows int, gate statelog.Reason, err error) {

	h.t.Helper()
	err = h.db.Replicated().Tx(h.t.Context(), func(tx *sql.Tx) error {
		n, reason, _, aErr := h.applyIn(tx, rec, seq, storedAt)
		rows, gate = n, reason
		return aErr
	})
	if err == nil {
		h.applier.Committed(h.t.Context())
	}
	return rows, gate, err
}

// applyIn is the transaction body: the gate, then the apply. It is separate so
// a case can run it TWICE — once in a transaction it rolls back and once in
// one it commits, which is exactly what the store does to a conflicted
// transaction.
func (h *applyHarness) applyIn(tx *sql.Tx, rec chat.MutationRecord, seq uint64,
	storedAt time.Time) (rows int, gate statelog.Reason, gated bool, err error) {

	h.t.Helper()
	record := applyStatelogRecord(h.t, rec, seq, storedAt)
	reason, dropped, err := h.applier.Gated(h.t.Context(), tx, record)
	if err != nil {
		return 0, "", false, err
	}
	if dropped {
		return 0, reason, true, nil
	}
	n, err := h.applier.Apply(h.t.Context(), tx, record, statelog.ApplyOptions{
		Now: storedAt, StoredAt: storedAt, MaxVariables: h.maxVariables,
	})
	return n, "", false, err
}

// applyStatelogRecord is what the framework hands an applier.
func applyStatelogRecord(t *testing.T, rec chat.MutationRecord, seq uint64,
	storedAt time.Time) statelog.Record {

	t.Helper()
	body, err := chat.Encode(rec)
	if err != nil {
		t.Fatalf("encode the record: %v", err)
	}
	return statelog.Record{
		Envelope: statelog.Envelope{
			V: rec.V, Kind: string(rec.Subject.Kind),
			Subject: statelog.Subject{
				Kind: string(rec.Subject.Kind), ID: rec.Subject.ID,
			},
			Op: string(rec.Op), OpID: rec.OpID, Gen: rec.Gen,
			Writer: rec.Writer, Scope: rec.Scope.Resolve(rec.Subject),
		},
		Position: statelog.Position{
			Stream: topics.ChatLogStream, Generation: rec.Gen, Seq: seq,
		},
		Payload:  body,
		StoredAt: storedAt,
	}
}

// applyRecord builds one record with its payload encoded.
func applyRecord(subject chat.Subject, op chat.OpKind, opID string, payload any,
	scope chat.ScopeSet) chat.MutationRecord {

	rec := chat.MutationRecord{
		RecordEnvelope: chat.RecordEnvelope{
			V: chat.RecordVersion, OpID: opID, Subject: subject, Op: op,
			CreatedAt: applyBrokerAt, Gen: 1, Writer: "node-a", Scope: scope,
		},
		Actor: "ada", ActorKind: chat.AuthorHuman,
	}
	if payload != nil {
		body, err := json.Marshal(payload)
		if err != nil {
			panic(err)
		}
		rec.Mutation = body
	}
	return rec
}

// applyCreateRoom builds the create of one named public room.
//
// THE SCOPE NAMES BOTH OBJECTS, which is what a create actually touches: the
// address it claimed and the room it made. Everything else in this file
// touches exactly its own subject.
func applyCreateRoom(channelID, name string, members ...chat.Member) chat.MutationRecord {
	return applyRecord(chat.ChannelNameSubject(name), chat.OpCreate,
		"op-create-"+channelID, chat.ChannelCreate{
			V: chat.DocumentVersion, ChannelID: channelID, Kind: chat.KindPublic,
			Name: name, Topic: "shipping", Members: members,
			CreatedBy: "ada", CreatedByKind: chat.AuthorHuman,
		}, chat.ScopeSet{Terms: []chat.ScopeTerm{
			{Kind: chat.TermName, ID: chat.ChannelToken(name)},
			{Kind: chat.TermChannel, ID: channelID},
		}})
}

// applyPostRecord builds one post into a room.
func applyPostRecord(channelID, messageID, body, threadRoot string,
	mentions ...string) chat.MutationRecord {

	return applyRecord(chat.MessageSubject(channelID), chat.OpPost,
		"op-post-"+messageID, chat.MessagePost{
			V: chat.DocumentVersion, MessageID: messageID, Body: body,
			ThreadRoot: threadRoot, Mentions: mentions, Author: "ada",
			AuthorKind: chat.AuthorHuman,
		}, chat.ScopeSet{Subject: true})
}

// applyEraseRecord builds one operator erase.
func applyEraseRecord(channelID, opID, by string, ids ...string) chat.MutationRecord {
	rec := applyRecord(chat.ChannelSubject(channelID), chat.OpErase, opID,
		chat.MessageErase{
			V: chat.DocumentVersion, ChannelID: channelID, MessageIDs: ids,
			Count: len(ids), Reason: "leaked a key", By: by,
			ByKind: chat.AuthorOperator,
		}, chat.ScopeSet{Subject: true})
	rec.Actor, rec.ActorKind = by, chat.AuthorOperator
	return rec
}

// ---- reading the estate back ------------------------------------------- //

func (h *applyHarness) count(table string) int {
	h.t.Helper()
	var n int
	if err := h.db.Replicated().Read(h.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(h.t.Context(),
			`SELECT COUNT(*) FROM `+table).Scan(&n)
	}); err != nil {
		h.t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func (h *applyHarness) scalar(query string, args ...any) string {
	h.t.Helper()
	var out sql.NullString
	if err := h.db.Replicated().Read(h.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(h.t.Context(), query, args...).Scan(&out)
	}); err != nil {
		return ""
	}
	return out.String
}

func (h *applyHarness) number(query string, args ...any) int64 {
	h.t.Helper()
	var out sql.NullInt64
	if err := h.db.Replicated().Read(h.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(h.t.Context(), query, args...).Scan(&out)
	}); err != nil {
		h.t.Fatalf("read %q: %v", query, err)
	}
	return out.Int64
}

// applyReplicatedTables is every table this domain's records rebuild, and it
// is [chat.ReproducibleTables] rather than a list of its own — a table added
// there and forgotten here would be one the determinism cases never compared.
var applyReplicatedTables = chat.ReproducibleTables

// dump renders one table's rows IN ROWID ORDER, which is insertion order.
//
// THE ORDER IS PART OF WHAT IS COMPARED, not an artefact of the query: the
// identity claim is a checksum over the replicated FILE, so a collection
// written in whatever order a writer's map produced is a divergence although
// every row is the same. Comparing sets would pass on exactly that bug.
func (h *applyHarness) dump(table string) string {
	h.t.Helper()
	var out strings.Builder
	if err := h.db.Replicated().Read(h.t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(h.t.Context(),
			`SELECT * FROM `+table+` ORDER BY rowid`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		columns, err := rows.Columns()
		if err != nil {
			return err
		}
		for rows.Next() {
			cells := make([]any, len(columns))
			into := make([]any, len(columns))
			for i := range cells {
				into[i] = &cells[i]
			}
			if err := rows.Scan(into...); err != nil {
				return err
			}
			for i, name := range columns {
				fmt.Fprintf(&out, "%s=%v ", name, cells[i])
			}
			out.WriteString("\n")
		}
		return rows.Err()
	}); err != nil {
		h.t.Fatalf("dump %s: %v", table, err)
	}
	return out.String()
}

// estate is every replicated table, rendered.
func (h *applyHarness) estate() string {
	h.t.Helper()
	var out strings.Builder
	for _, table := range applyReplicatedTables {
		fmt.Fprintf(&out, "-- %s\n%s", table, h.dump(table))
	}
	return out.String()
}

// ---- the cases --------------------------------------------------------- //

// TestACreateWritesTheClaimTheRoomItsMembersAndItsHistoryTogether.
//
// THE WHOLE REASON THIS DOMAIN EXISTS. On a coordination bucket a create was a
// two-key sequence — claim the name, then write the room — with an orphan
// claim as its crash state and a grace rule for stepping over the debris.
// Here it is one record and one transaction, so there is no window in which a
// name is held by a room that was never written.
func TestACreateWritesTheClaimTheRoomItsMembersAndItsHistoryTogether(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	if _, gate, err := h.apply(applyCreateRoom("room-1", "launch",
		chat.Member{Handle: "ada"},
		chat.Member{Handle: "grace", FollowAll: true})); err != nil || gate != "" {
		t.Fatalf("apply a create: %v (gate %q)", err, gate)
	}
	for table, want := range map[string]int{
		"chat_channel_names": 1, "chat_channels": 1, "chat_members": 2,
		"chat_history": 1,
	} {
		if got := h.count(table); got != want {
			t.Errorf("%s holds %d rows, want %d — a create is ONE transaction "+
				"and a half-written one is the crash state this domain removes",
				table, got, want)
		}
	}
	// THE CLAIM CARRIES THE NORMALISED NAME BESIDE ITS TOKEN, because a
	// digest has no inverse and an operator asking "why can this name not
	// be used" needs the name.
	if got := h.scalar(`SELECT name_norm FROM chat_channel_names`); got != "launch" {
		t.Errorf("the claim holds %q, so nothing can say what address is taken", got)
	}
	// THE HISTORY ROW'S KIND IS THE (kind, op) PAIR. The op alone would
	// not do: `create` names both a room and the address it was claimed
	// under, and a filter that could not tell them apart would report a
	// company's rooms twice.
	want := chat.HistoryKind(chat.KindChannelName, chat.OpCreate)
	if got := h.scalar(`SELECT kind FROM chat_history`); got != want {
		t.Errorf("the history row's kind is %q, want %q", got, want)
	}
	// A PUBLIC ROOM IS NOT PRIVATE, and the column is what every
	// visibility filter is a predicate on.
	if got := h.number(`SELECT private FROM chat_channels`); got != 0 {
		t.Errorf("a public room stored private=%d", got)
	}
}

// TestARecordThatArbitratedOneAddressAndClaimsAnotherIsRefused.
//
// THE SUBJECT IS THE ADDRESS, and this is what makes that true rather than
// conventional. Without it a writer takes one name at the broker — where
// exactly one writer can — and writes a different one into every node's row,
// leaving the arbitrated name held by nothing and the written name held by
// two rooms.
func TestARecordThatArbitratedOneAddressAndClaimsAnotherIsRefused(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	rec := applyCreateRoom("room-1", "launch")
	// The subject stays; the payload's name moves.
	var payload chat.ChannelCreate
	if err := json.Unmarshal(rec.Mutation, &payload); err != nil {
		t.Fatal(err)
	}
	payload.Name = "something-else"
	body, _ := json.Marshal(payload)
	rec.Mutation = body

	if _, _, err := h.apply(rec); err == nil {
		t.Fatal("a record took one address at the broker and wrote another — " +
			"the arbitrated name is now held by nothing")
	}
	if got := h.count("chat_channels"); got != 0 {
		t.Errorf("%d rooms were written anyway", got)
	}
}

// TestTheRoomsSequenceIsContiguousAndARedeliveredPostConsumesNoNumber.
//
// The per-channel sequence is what lets a browser tell a live frame it can
// apply from a HOLE it must refetch: holding 41 and handed 43, it asks for 42.
// A redelivery that consumed a number would put a hole in that sequence which
// no message will ever fill, so every viewer refetches a message that does not
// exist, for ever.
func TestTheRoomsSequenceIsContiguousAndARedeliveredPostConsumesNoNumber(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	if _, _, err := h.apply(applyCreateRoom("room-1", "launch")); err != nil {
		t.Fatalf("create: %v", err)
	}
	first := applyPostRecord("room-1", "msg-1", "we ship friday", "")
	if _, _, err := h.apply(first); err != nil {
		t.Fatalf("post: %v", err)
	}
	if _, _, err := h.apply(applyPostRecord("room-1", "msg-2", "confirmed", "")); err != nil {
		t.Fatalf("post: %v", err)
	}
	for id, want := range map[string]int64{"msg-1": 1, "msg-2": 2} {
		if got := h.number(
			`SELECT channel_seq FROM chat_messages WHERE id = ?`, id); got != want {
			t.Errorf("%s landed at sequence %d, want %d — the numbers are "+
				"contiguous per room or a browser cannot tell a dropped frame "+
				"from the end of the conversation", id, got, want)
		}
	}

	// THE REDELIVERY, at a LATER position: the same message id, which is
	// what a re-acknowledged publish looks like after a reanchor.
	rows, _, err := h.applyAt(first, h.seq+1, applyBrokerAt)
	if err != nil {
		t.Fatalf("redeliver a post: %v", err)
	}
	if rows != 0 {
		t.Errorf("a redelivered post wrote %d rows", rows)
	}
	if got := h.number(`SELECT message_seq FROM chat_channels WHERE id = ?`,
		"room-1"); got != 2 {
		t.Fatalf("the room's high-water mark is %d after a redelivery, want 2 "+
			"— a redelivered post must NOT consume a second number", got)
	}
	if got := h.count("chat_messages"); got != 2 {
		t.Errorf("the room holds %d messages after a redelivery, want 2", got)
	}
}

// TestAPostMovesTheRoomsScopedThroughAndNeverItsVersion.
//
// A post arbitrates on the room's MESSAGE subject, so the room's own broker
// expectation is still its last CHANNEL record. A post that stamped `version`
// would poison that expectation — every later patch would be decided against a
// sequence the room's own subject never reached — and the room's topic could
// not be changed again until somebody posted.
func TestAPostMovesTheRoomsScopedThroughAndNeverItsVersion(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	if _, _, err := h.apply(applyCreateRoom("room-1", "launch")); err != nil {
		t.Fatalf("create: %v", err)
	}
	created := h.number(`SELECT version FROM chat_channels WHERE id = ?`, "room-1")
	if _, _, err := h.apply(applyPostRecord("room-1", "msg-1", "hello", "")); err != nil {
		t.Fatalf("post: %v", err)
	}
	if got := h.number(`SELECT version FROM chat_channels WHERE id = ?`,
		"room-1"); got != created {
		t.Errorf("a post moved the room's version from %d to %d — the room's "+
			"own expectation is now a position its subject never reached",
			created, got)
	}
	if got := h.number(`SELECT scoped_through FROM chat_channels WHERE id = ?`,
		"room-1"); got <= created {
		t.Errorf("a post left scoped_through at %d — a read barrier compares "+
			"MAX of the two, so a room that took a foreign-subject write and "+
			"said nothing is one a linearizable read serves stale", got)
	}
}

// TestATombstoneKeepsTheRowSoAReplyStillResolves.
//
// THE ROW STAYS. A thread is one level deep and hangs off its root, so a
// delete that removed the row would make every reply in that thread
// unreachable — a conversation that loses its first message loses all of it.
// The mentions go with the body, because a mention row IS the @-mention feed
// and an empty message sitting in somebody's list of things that named them is
// a notification for text that no longer exists.
func TestATombstoneKeepsTheRowSoAReplyStillResolves(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	if _, _, err := h.apply(applyCreateRoom("room-1", "launch")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := h.apply(applyPostRecord("room-1", "msg-1",
		"@grace can you check the key rotation", "", "grace")); err != nil {
		t.Fatalf("post: %v", err)
	}
	if _, _, err := h.apply(applyPostRecord("room-1", "msg-2", "on it",
		"msg-1")); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if got := h.count("chat_mentions"); got != 1 {
		t.Fatalf("a mention wrote %d rows", got)
	}

	tombstone := applyRecord(chat.MessageSubject("room-1"), chat.OpDelete,
		"op-delete-1", chat.MessageDelete{
			V: chat.DocumentVersion, MessageID: "msg-1", DeletedBy: "ada",
			DeletedByKind: chat.AuthorHuman,
		}, chat.ScopeSet{Subject: true})
	if _, gate, err := h.apply(tombstone); err != nil || gate != "" {
		t.Fatalf("tombstone: %v (gate %q)", err, gate)
	}
	if got := h.count("chat_messages"); got != 2 {
		t.Fatalf("the room holds %d messages after a tombstone, want 2 — the "+
			"reply hung off msg-1 has lost its root", got)
	}
	if got := h.scalar(`SELECT body FROM chat_messages WHERE id = ?`, "msg-1"); got != "" {
		t.Errorf("a tombstoned message still reads %q", got)
	}
	if got := h.number(`SELECT deleted_at FROM chat_messages WHERE id = ?`,
		"msg-1"); got != store.EncodeTime(applyBrokerAt) {
		t.Errorf("a tombstone stamped deleted_at=%d, want the broker's own "+
			"instant", got)
	}
	if got := h.count("chat_mentions"); got != 0 {
		t.Errorf("%d mention rows survive a tombstone, so a message with no "+
			"body is still in somebody's mention feed", got)
	}
	// THE REPLY STILL RESOLVES ITS ROOT, which is the whole of what the
	// row is kept for.
	if got := h.scalar(`SELECT thread_root FROM chat_messages WHERE id = ?`,
		"msg-2"); got != "msg-1" {
		t.Errorf("the reply's root reads %q", got)
	}

	// AND AN EDIT OF A TOMBSTONE WRITES NOTHING: the body was removed on
	// purpose, and restoring text into it would undo a deletion nobody
	// asked to undo.
	edit := applyRecord(chat.MessageSubject("room-1"), chat.OpEdit, "op-edit-1",
		chat.MessageEdit{
			V: chat.DocumentVersion, MessageID: "msg-1", Body: "back again",
			EditedBy: "ada", EditedByKind: chat.AuthorHuman,
		}, chat.ScopeSet{Subject: true})
	if rows, _, err := h.apply(edit); err != nil || rows != 0 {
		t.Fatalf("an edit of a tombstone wrote %d rows (%v)", rows, err)
	}
	if got := h.scalar(`SELECT body FROM chat_messages WHERE id = ?`, "msg-1"); got != "" {
		t.Errorf("an edit restored %q into a tombstone", got)
	}
}

// TestAnErasedMessageIsNeverResurrectedByAReplay.
//
// An erase is the one gesture that destroys a message row. The marker is what
// makes that permanent: without it a redelivered post — or a replay from zero
// on a node that was away — writes the message straight back, and a company
// that redacted a leaked credential still has it on one member.
func TestAnErasedMessageIsNeverResurrectedByAReplay(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	if _, _, err := h.apply(applyCreateRoom("room-1", "launch")); err != nil {
		t.Fatalf("create: %v", err)
	}
	post := applyPostRecord("room-1", "msg-1", "the key is in the runbook", "",
		"grace")
	if _, _, err := h.apply(post); err != nil {
		t.Fatalf("post: %v", err)
	}
	if _, gate, err := h.apply(applyEraseRecord("room-1", "op-erase-a", "ops-on-call",
		"msg-1")); err != nil || gate != "" {
		t.Fatalf("erase: %v (gate %q)", err, gate)
	}
	for table, want := range map[string]int{
		"chat_messages": 0, "chat_mentions": 0, "chat_deletions": 1,
	} {
		if got := h.count(table); got != want {
			t.Fatalf("%s holds %d rows after an erase, want %d", table, got, want)
		}
	}

	// THE REPLAY. Same record, later position — which is what a
	// redelivered acknowledgement and a reanchored stream both look like.
	rows, _, err := h.applyAt(post, h.seq+1, applyBrokerAt)
	if err != nil {
		t.Fatalf("replay the post: %v", err)
	}
	if rows != 0 || h.count("chat_messages") != 0 {
		t.Fatalf("a replayed post wrote %d rows and the room holds %d "+
			"messages — an erased message came back", rows, h.count("chat_messages"))
	}
}

// TestTheErasingRecordIsNotBlockedByItsOwnMarker, and a SECOND erase does not
// restate the first erasure's attribution.
//
// Two facts about the marker, which the applier consults BY OPERATION ID
// rather than by presence. A record must not read its OWN effect as somebody
// else's: a redelivered erase re-asserts its deletes and its marker rather
// than being turned away by them, and it is not gated either — a gesture whose
// acknowledgement was lost must not resolve as "applied nowhere" while the
// destruction it asked for did happen.
//
// And the first erasure is the FACT: who removed somebody's words, and when,
// is what an operator audit reads, so a later erase naming the same message
// leaves it alone. TWO GUARDS HOLD THAT ONE — the skip in [applyErase] and the
// marker's own conflict clause — so the assertion below survives either being
// removed alone, which is defence in depth rather than a case that cannot
// fail: remove the marker write, restate it on conflict AND drop the skip, or
// gate the erase on its own marker, and it goes red.
func TestTheErasingRecordIsNotBlockedByItsOwnMarker(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	if _, _, err := h.apply(applyCreateRoom("room-1", "launch")); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, id := range []string{"msg-1", "msg-2"} {
		if _, _, err := h.apply(applyPostRecord("room-1", id, "said "+id, "")); err != nil {
			t.Fatalf("post %s: %v", id, err)
		}
	}
	first := applyEraseRecord("room-1", "op-erase-a", "ops-alice", "msg-1")
	if _, _, err := h.apply(first); err != nil {
		t.Fatalf("erase: %v", err)
	}

	// THE RECORD'S OWN REDELIVERY IS NOT GATED BY THE MARKER IT WROTE.
	if _, gate, err := h.applyAt(first, h.seq+1, applyBrokerAt); err != nil {
		t.Fatalf("redeliver the erase: %v", err)
	} else if gate != "" {
		t.Fatalf("the erase was gated by the marker it wrote (%q)", gate)
	}
	if got := h.count("chat_deletions"); got != 1 {
		t.Fatalf("a redelivered erase left %d markers, want 1", got)
	}

	// A SECOND ERASE, BY SOMEBODY ELSE, naming one message the first
	// destroyed and one it did not.
	second := applyEraseRecord("room-1", "op-erase-b", "ops-bob", "msg-1", "msg-2")
	if _, _, err := h.apply(second); err != nil {
		t.Fatalf("second erase: %v", err)
	}
	if got := h.scalar(`SELECT by FROM chat_deletions WHERE message_id = ?`,
		"msg-1"); got != "ops-alice" {
		t.Errorf("msg-1's marker names %q — the first erasure is the fact an "+
			"audit reads, and a second gesture must not take credit for it", got)
	}
	if got := h.scalar(`SELECT by FROM chat_deletions WHERE message_id = ?`,
		"msg-2"); got != "ops-bob" {
		t.Errorf("msg-2's marker names %q, want the operator who erased it", got)
	}
	if got := h.count("chat_messages"); got != 0 {
		t.Errorf("%d messages survive two erases", got)
	}
}

// TestTwoEstatesFedOneLogHoldIdenticalRowsAndApplyingTwiceEqualsOnce.
//
// THE IDENTITY CLAIM, exercised. Two nodes deriving one SQL state from one
// ordered stream must produce byte-identical tables — including the ORDER rows
// were inserted in, because the claim is a checksum over the file rather than
// over a set — and a redelivered record must change nothing at all.
//
// The second estate is fed the same records with a REDELIVERY spliced into
// them, which is the shape a real fleet sees: one node's broker replays an
// acknowledgement the other's did not.
func TestTwoEstatesFedOneLogHoldIdenticalRowsAndApplyingTwiceEqualsOnce(t *testing.T) {
	t.Parallel()
	log := func(h *applyHarness, redeliver bool) {
		h.t.Helper()
		create := applyCreateRoom("room-1", "launch",
			chat.Member{Handle: "grace", FollowAll: true},
			chat.Member{Handle: "ada"},
			chat.Member{Handle: "alan"})
		if _, _, err := h.apply(create); err != nil {
			h.t.Fatalf("create: %v", err)
		}
		root := applyPostRecord("room-1", "msg-1", "who owns the rotation?", "",
			"grace", "alan")
		if _, _, err := h.apply(root); err != nil {
			h.t.Fatalf("post: %v", err)
		}
		if redeliver {
			// A REDELIVERY IN THE MIDDLE OF THE LOG. It must
			// consume no sequence number and write no row, or the
			// two estates diverge from here on.
			if _, _, err := h.applyAt(root, h.seq+1, applyBrokerAt); err != nil {
				h.t.Fatalf("redeliver: %v", err)
			}
		}
		reply := applyPostRecord("room-1", "msg-2", "I do", "msg-1")
		reply.Actor, reply.ActorKind = "grace", chat.AuthorAgent
		var payload chat.MessagePost
		if err := json.Unmarshal(reply.Mutation, &payload); err != nil {
			h.t.Fatal(err)
		}
		payload.Author, payload.AuthorKind = "grace", chat.AuthorAgent
		body, _ := json.Marshal(payload)
		reply.Mutation = body
		if _, _, err := h.apply(reply); err != nil {
			h.t.Fatalf("reply: %v", err)
		}
		topic := "rotation"
		patch := applyRecord(chat.ChannelSubject("room-1"), chat.OpPatch,
			"op-patch-1", chat.ChannelPatch{V: chat.DocumentVersion, Topic: &topic},
			chat.ScopeSet{Subject: true})
		if _, _, err := h.apply(patch); err != nil {
			h.t.Fatalf("patch: %v", err)
		}
		members := applyRecord(chat.ChannelSubject("room-1"), chat.OpMembers,
			"op-members-1", chat.MemberSet{
				V: chat.DocumentVersion,
				Members: []chat.Member{
					{Handle: "grace", FollowAll: true}, {Handle: "alan"},
				},
			}, chat.ScopeSet{Subject: true})
		if _, _, err := h.apply(members); err != nil {
			h.t.Fatalf("members: %v", err)
		}
	}

	one, two := newApplyHarness(t), newApplyHarness(t)
	log(one, false)
	log(two, true)
	if got, want := two.estate(), one.estate(); got != want {
		t.Errorf("two estates fed one log hold different rows.\n"+
			"--- the node with no redelivery\n%s\n--- the node with one\n%s",
			want, got)
	}
	// AND THE MEMBERSHIP CONVERGED IN BOTH DIRECTIONS: the record carried
	// the set whole, so `ada` — who is in the founding set and not in the
	// second — has lost her row on both.
	if got := one.count("chat_members"); got != 2 {
		t.Errorf("the room holds %d members after a whole-set replacement, "+
			"want 2 — a membership that only ever grows is a private room "+
			"somebody was removed from and can still read", got)
	}
}

// TestAPruneDeletesTheSameRowsInTwoEstates.
//
// Chat is the first domain that DELETES CONTENT on a horizon, and the prune is
// a RECORD carrying a cutoff instant rather than a local sweep. "Older than a
// year" evaluated against each node's own clock deletes a different set on
// every node, for ever, and nothing would ever report it — where a cutoff on
// the log is one number every applier compares against the broker's own stored
// timestamps.
func TestAPruneDeletesTheSameRowsInTwoEstates(t *testing.T) {
	t.Parallel()
	old := applyBrokerAt.Add(-400 * 24 * time.Hour)
	cutoff := applyBrokerAt.Add(-365 * 24 * time.Hour)

	log := func(h *applyHarness) {
		h.t.Helper()
		if _, _, err := h.applyStoredAt(applyCreateRoom("room-1", "launch"),
			old); err != nil {
			h.t.Fatalf("create: %v", err)
		}
		// TWO MESSAGES BELOW THE CUTOFF, one of them a thread with a
		// reply, a mention and a reaction hanging off it — so every
		// table the prune ranges over has a row to lose.
		if _, _, err := h.applyStoredAt(applyPostRecord("room-1", "msg-1",
			"last year's incident", "", "grace"), old); err != nil {
			h.t.Fatalf("post: %v", err)
		}
		if _, _, err := h.applyStoredAt(applyPostRecord("room-1", "msg-2",
			"and the follow-up", "msg-1"), old); err != nil {
			h.t.Fatalf("reply: %v", err)
		}
		react := applyRecord(chat.MessageSubject("room-1"), chat.OpReact,
			"op-react-1", chat.Reaction{
				V: chat.DocumentVersion, MessageID: "msg-1", Emoji: ":eyes:",
				By: "grace",
			}, chat.ScopeSet{Subject: true})
		if _, _, err := h.applyStoredAt(react, old); err != nil {
			h.t.Fatalf("react: %v", err)
		}
		// AND ONE ABOVE IT, which must survive.
		if _, _, err := h.applyStoredAt(applyPostRecord("room-1", "msg-3",
			"this year's", ""), applyBrokerAt); err != nil {
			h.t.Fatalf("post: %v", err)
		}
		prune := applyRecord(chat.ChannelSubject("room-1"), chat.OpPrune,
			"op-prune-1", chat.Prune{V: chat.DocumentVersion, Cutoff: cutoff},
			chat.ScopeSet{Subject: true})
		if _, _, err := h.applyStoredAt(prune, applyBrokerAt); err != nil {
			h.t.Fatalf("prune: %v", err)
		}
	}

	one, two := newApplyHarness(t), newApplyHarness(t)
	log(one)
	log(two)
	if got, want := two.estate(), one.estate(); got != want {
		t.Errorf("a prune left two estates holding different rows.\n"+
			"--- one\n%s\n--- two\n%s", want, got)
	}
	if got := one.count("chat_messages"); got != 1 {
		t.Fatalf("%d messages survive the prune, want the one above the "+
			"cutoff", got)
	}
	if got := one.scalar(`SELECT id FROM chat_messages`); got != "msg-3" {
		t.Errorf("the surviving message is %q, want the one stored above the "+
			"cutoff", got)
	}
	for _, table := range []string{
		"chat_mentions", "chat_reactions", "chat_thread_participants",
		"chat_follows",
	} {
		if got := one.count(table); got != 0 {
			t.Errorf("%s holds %d rows after the prune — each carries the "+
				"parent message's own channel and instant precisely so the "+
				"range delete reaches it on an index", table, got)
		}
	}
	// THE MARKERS ARE NOT PRUNED, and nothing here wrote one — but the
	// rule is worth stating where the table list is: a horizon on a
	// deletion marker is the day a replayed post resurrects what a company
	// erased.
	if got := one.count("chat_deletions"); got != 0 {
		t.Errorf("a prune wrote %d deletion markers", got)
	}
}

// TestARerunTransactionBodyObservesOneChangePerPosition.
//
// The store's transactions are OPTIMISTIC: a conflicted one is rolled back and
// its body runs again with the same records in the same order. An accumulator
// that appended would then hand a live surface the same message twice — which
// every viewer renders as the same remark said twice — so it is keyed on the
// record's own position, where a re-run overwrites its own earlier entry.
//
// And the other half: a batch that FAILED must leave nothing behind.
// [Applier.Committed] runs only on success, so an entry gathered by a
// transaction that never committed would drain on a LATER commit and announce
// rows nobody wrote.
func TestARerunTransactionBodyObservesOneChangePerPosition(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	if _, _, err := h.apply(applyCreateRoom("room-1", "launch")); err != nil {
		t.Fatalf("create: %v", err)
	}
	h.live.batches = nil

	post := applyPostRecord("room-1", "msg-1", "we ship friday", "")
	seq := h.seq + 1
	// THE FIRST RUN IS ROLLED BACK, which is exactly what the store does
	// to a conflicted transaction: the rows go, and the body runs again.
	rollback := errors.New("conflict")
	if err := h.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		if _, _, _, err := h.applyIn(tx, post, seq, applyBrokerAt); err != nil {
			return err
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatalf("the rolled-back transaction returned %v", err)
	}
	if err := h.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, _, _, err := h.applyIn(tx, post, seq, applyBrokerAt)
		return err
	}); err != nil {
		t.Fatalf("the re-run transaction: %v", err)
	}
	h.applier.Committed(t.Context())

	frames := h.live.frames()
	if len(frames) != 1 {
		t.Fatalf("a re-run transaction body pushed %d live frames for one "+
			"record, want 1 — every viewer renders the second as the same "+
			"message said twice", len(frames))
	}
	if frames[0].MessageID != "msg-1" || frames[0].ChannelSeq != 1 {
		t.Errorf("the frame names message %q at sequence %d, want msg-1 at 1",
			frames[0].MessageID, frames[0].ChannelSeq)
	}

	// A FAILED BATCH LEAVES NOTHING TO DRAIN. The post below names a room
	// this node does not have, which is a malformed record under a strict
	// replay — so the transaction fails after the good record in it
	// already noted a frame.
	h.live.batches = nil
	if err := h.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		if _, _, _, err := h.applyIn(tx,
			applyPostRecord("room-1", "msg-2", "second", ""), h.seq+2,
			applyBrokerAt); err != nil {
			return err
		}
		_, _, _, err := h.applyIn(tx,
			applyPostRecord("room-404", "msg-3", "nowhere", ""), h.seq+3,
			applyBrokerAt)
		return err
	}); err == nil {
		t.Fatal("a post into a room this node has no row for was applied")
	}
	h.applier.Committed(t.Context())
	if got := h.live.frames(); len(got) != 0 {
		t.Errorf("a transaction that never committed drained %d live frames, "+
			"which announce rows nobody wrote", len(got))
	}
}

// TestAnEvictedNodesRecordsApplyNowhere, and a readmission takes it back.
//
// The fence depends on nothing but the log's own order and the record's
// envelope, which is what makes it hold when coordination cannot be reached at
// all — and a wedged coordination path is a precondition of an eviction being
// permitted in the first place.
func TestAnEvictedNodesRecordsApplyNowhere(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	evict := applyRecord(chat.EvictionSubject("node-b"), chat.OpEviction,
		"op-evict", chat.Eviction{
			V: chat.GateRecordVersion, NodeID: "node-b", EvictedBy: "ops",
			EvictedAt: applyBrokerAt,
		}, chat.ScopeSet{Subject: true})
	if _, _, err := h.apply(evict); err != nil {
		t.Fatalf("evict: %v", err)
	}

	from := applyCreateRoom("room-1", "launch")
	from.Writer = "node-b"
	_, gate, err := h.apply(from)
	if err != nil {
		t.Fatalf("apply an evicted node's record: %v", err)
	}
	if gate != statelog.ReasonEvicted {
		t.Fatalf("an evicted node's record applied (gate %q)", gate)
	}
	if got := h.count("chat_channels"); got != 0 {
		t.Fatalf("%d rooms were written by an evicted node", got)
	}

	readmit := applyRecord(chat.EvictionSubject("node-b"), chat.OpEviction,
		"op-readmit", chat.Eviction{
			V: chat.GateRecordVersion, NodeID: "node-b", EvictedBy: "ops",
			EvictedAt: applyBrokerAt, Readmitted: true,
		}, chat.ScopeSet{Subject: true})
	if _, _, err := h.apply(readmit); err != nil {
		t.Fatalf("readmit: %v", err)
	}
	if _, gate, err := h.apply(from); err != nil || gate != "" {
		t.Fatalf("a readmitted node's record was still fenced: %v (gate %q)",
			err, gate)
	}
	if got := h.count("chat_channels"); got != 1 {
		t.Errorf("a readmitted node wrote %d rooms, want 1", got)
	}
}

// TestASecondImportOfOneArchiveWritesNothingNew.
//
// A company moving onto native chat brings a year of its Slack with it, and an
// import that is interrupted has to be safe to simply run again. The guard is
// the VENDOR'S OWN id rather than somebody remembering where the first pass
// stopped — and an imported message renders at the instant it was said,
// through `authored_at`, while the prune still ranges on the broker's own.
func TestASecondImportOfOneArchiveWritesNothingNew(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	if _, _, err := h.apply(applyCreateRoom("room-1", "launch")); err != nil {
		t.Fatalf("create: %v", err)
	}
	said := time.Date(2029, 11, 5, 9, 0, 0, 0, time.UTC)
	imported := func(messageID string) chat.MutationRecord {
		rec := applyRecord(chat.MessageSubject("room-1"), chat.OpPost,
			"op-import-"+messageID, chat.MessagePost{
				V: chat.DocumentVersion, MessageID: messageID,
				Body: "we shipped", Author: "ada", AuthorKind: chat.AuthorHuman,
				Imported: &chat.Imported{
					Source: "slack", VendorID: "1699174800.000100",
					Author: "ada", AuthorKind: chat.AuthorHuman,
					AuthoredAt: said,
				},
			}, chat.ScopeSet{Subject: true})
		// AN IMPORT WAKES NOBODY, which is the record's own shape
		// rather than a flag: a year of conversations the company has
		// already had must not page it all at once.
		rec.Notify = nil
		return rec
	}
	if _, _, err := h.apply(imported("msg-1")); err != nil {
		t.Fatalf("import: %v", err)
	}
	if got := h.number(`SELECT authored_at FROM chat_messages WHERE id = ?`,
		"msg-1"); got != store.EncodeTime(said) {
		t.Errorf("an imported message stored authored_at=%d — a year of "+
			"history would render at the instant it was replayed", got)
	}
	if got := h.number(`SELECT created_at FROM chat_messages WHERE id = ?`,
		"msg-1"); got != store.EncodeTime(applyBrokerAt) {
		t.Errorf("an imported message stored created_at=%d, want the broker's "+
			"own instant — the prune ranges on it", got)
	}

	// THE SECOND PASS, which mints a different message id for the same
	// vendor message, exactly as a re-run export would.
	rows, _, err := h.apply(imported("msg-2"))
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if rows != 0 || h.count("chat_messages") != 1 {
		t.Errorf("a second pass over one archive wrote %d rows and the room "+
			"holds %d messages — an interrupted import cannot be run again",
			rows, h.count("chat_messages"))
	}
	// AND THE ROOM'S SEQUENCE DID NOT MOVE for a message it did not write.
	if got := h.number(`SELECT message_seq FROM chat_channels WHERE id = ?`,
		"room-1"); got != 1 {
		t.Errorf("the room's high-water mark is %d after a refused import, "+
			"want 1", got)
	}
}

// TestTheThirtyThirdDistinctReactionIsDeclinedDeterministically.
//
// [chat.MaxReactionEmoji] is the applier's to enforce, and the record's shape
// is what forces it there: a reaction is a single TOGGLE so that two people
// reacting never overwrite each other, and a toggle cannot see the set it is
// joining. The applier holds the message's rows in its own transaction, so it
// reaches the same answer on every node — which a cap read from one node's
// rows at write time could not promise.
func TestTheThirtyThirdDistinctReactionIsDeclinedDeterministically(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	if _, _, err := h.apply(applyCreateRoom("room-1", "launch")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := h.apply(applyPostRecord("room-1", "msg-1", "ship it", "")); err != nil {
		t.Fatalf("post: %v", err)
	}
	react := func(emoji, by string, removed bool) chat.MutationRecord {
		return applyRecord(chat.MessageSubject("room-1"), chat.OpReact,
			"op-react-"+emoji+"-"+by, chat.Reaction{
				V: chat.DocumentVersion, MessageID: "msg-1", Emoji: emoji,
				By: by, Removed: removed,
			}, chat.ScopeSet{Subject: true})
	}
	for i := range chat.MaxReactionEmoji {
		if _, _, err := h.apply(react(fmt.Sprintf(":e%d:", i), "ada", false)); err != nil {
			t.Fatalf("react %d: %v", i, err)
		}
	}
	// ONE MORE PERSON ON AN EMOJI THE MESSAGE ALREADY CARRIES IS STILL
	// ADMITTED: the cap is on how many MARKS a message carries, and a
	// hundred people agreeing with one thumb is one mark.
	if rows, _, err := h.apply(react(":e0:", "grace", false)); err != nil || rows != 1 {
		t.Fatalf("a second reactor on an existing emoji wrote %d rows (%v)",
			rows, err)
	}
	rows, _, err := h.apply(react(":one-too-many:", "ada", false))
	if err != nil {
		t.Fatalf("the thirty-third emoji: %v", err)
	}
	if rows != 0 {
		t.Errorf("the thirty-third distinct emoji wrote %d rows — every one is "+
			"a row replicated to every node, and past the cap a message's "+
			"reactions are longer than the message", rows)
	}
	if got := h.number(
		`SELECT COUNT(DISTINCT emoji) FROM chat_reactions WHERE message_id = ?`,
		"msg-1"); got != int64(chat.MaxReactionEmoji) {
		t.Errorf("the message carries %d distinct emoji, want %d",
			got, chat.MaxReactionEmoji)
	}

	// AND A TOGGLE REMOVES EXACTLY ONE PERSON'S MARK, not the emoji.
	if _, _, err := h.apply(react(":e0:", "ada", true)); err != nil {
		t.Fatalf("unreact: %v", err)
	}
	if got := h.number(
		`SELECT COUNT(*) FROM chat_reactions WHERE message_id = ? AND emoji = ?`,
		"msg-1", ":e0:"); got != 1 {
		t.Errorf("%d marks survive one person removing theirs, want grace's", got)
	}
}

// TestAThreadSubscribesWhoSpokeInItAndWhoWasNamedInIt.
//
// TWO TABLES, BECAUSE THEY ANSWER TWO QUESTIONS. A participant has SPOKEN in
// the thread and a reply reaches them as [chat.ReasonReply], which obliges
// nothing; a follower was NAMED in it and hears the replies without having
// said anything. Collapsing them would make every remark in a thread a
// standing subscription to it — a room of a hundred people answering a
// heading fix.
//
// Both are DERIVED HERE rather than authored by the writer, because
// participation is a fact about rows rather than a claim a record can make.
func TestAThreadSubscribesWhoSpokeInItAndWhoWasNamedInIt(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	if _, _, err := h.apply(applyCreateRoom("room-1", "launch")); err != nil {
		t.Fatalf("create: %v", err)
	}
	// A ROOT NAMING SOMEBODY. Nobody has spoken in the thread yet, so
	// there is no participant row — and a room's every post is not a
	// thread.
	if _, _, err := h.apply(applyPostRecord("room-1", "msg-1",
		"@grace who owns this?", "", "grace")); err != nil {
		t.Fatalf("post: %v", err)
	}
	if got := h.count("chat_thread_participants"); got != 0 {
		t.Errorf("a room post wrote %d participant rows — a second row per "+
			"message in the highest-volume table in the engine, for a thread "+
			"that mostly never happens", got)
	}
	if got := h.count("chat_follows"); got != 1 {
		t.Fatalf("being named wrote %d follow rows, want 1", got)
	}
	if got := h.scalar(`SELECT reason FROM chat_follows`); got != string(chat.ReasonMention) {
		t.Errorf("the follow's reason is %q, want %q — the reason is what "+
			"decides whether the next wake obliges an answer",
			got, chat.ReasonMention)
	}

	// THE FIRST REPLY makes it a thread, and brings the root's author in
	// with it: they have spoken in it, and a reply to the thread they
	// started is a question put back to them.
	reply := applyPostRecord("room-1", "msg-2", "I do", "msg-1")
	var payload chat.MessagePost
	if err := json.Unmarshal(reply.Mutation, &payload); err != nil {
		t.Fatal(err)
	}
	payload.Author, payload.AuthorKind = "grace", chat.AuthorAgent
	body, _ := json.Marshal(payload)
	reply.Mutation = body
	if _, _, err := h.apply(reply); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if got := h.count("chat_thread_participants"); got != 2 {
		t.Fatalf("a reply wrote %d participant rows, want the replier and the "+
			"root's author", got)
	}
	// THE ROOT'S OWN INSTANT IS WHAT THE PRUNE RANGES ON, carried onto
	// every child row precisely so the delete is an index seek.
	if got := h.number(`SELECT DISTINCT root_at FROM chat_thread_participants`); got !=
		store.EncodeTime(applyBrokerAt) {
		t.Errorf("a participant row carries root_at=%d, want the root "+
			"message's own instant", got)
	}
}
