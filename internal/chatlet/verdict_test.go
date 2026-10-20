package chatlet_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A MESSAGE RECORD IS DECIDED WHERE IT IS APPLIED, AND ITS WRITER IS TOLD.
//
// Each case below is the same race: a record ahead of the writer's on the log,
// appended and not yet applied, so the writer decides on rows that do not hold
// it and publishes — and the applier, which holds both in the log's order,
// refuses the writer's record at its position. Before the verdict, every one
// of them was answered `applied` for a record that did nothing.

// peerAppend appends a record another node of the fleet wrote and does NOT
// apply it, returning the log's end after it.
func (r *writeRound) peerAppend(rec chatlet.MutationRecord) uint64 {
	r.t.Helper()
	body, err := chatlet.Encode(rec)
	if err != nil {
		r.t.Fatalf("encode the peer's record: %v", err)
	}
	subject := topics.ChatLogSubject(string(rec.Subject.Kind), rec.Subject.ID)
	if _, _, err := r.log.Append(r.t.Context(), subject, rec.OpID, nil,
		r.signer.Seal(body)); err != nil {
		r.t.Fatalf("append the peer's record: %v", err)
	}
	end, err := r.log.End(r.t.Context())
	if err != nil {
		r.t.Fatalf("read the log's end: %v", err)
	}
	return end
}

// raced runs a gesture whose decision is taken before a peer's record ahead of
// it is applied: the peer's record is appended, the gesture decides on rows
// without it and publishes, and only then does this node's applier reach both,
// in the log's order.
func (r *writeRound) raced(peer chatlet.MutationRecord,
	gesture func(ctx context.Context) (chatlet.Written, error)) (chatlet.Written, error) {

	r.t.Helper()
	ahead := r.peerAppend(peer)
	type answer struct {
		w   chatlet.Written
		err error
	}
	done := make(chan answer, 1)
	go func() {
		w, err := gesture(r.t.Context())
		done <- answer{w, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		end, err := r.log.End(r.t.Context())
		if err != nil {
			r.t.Fatalf("read the log's end: %v", err)
		}
		if end > ahead {
			break
		}
		select {
		case a := <-done:
			r.t.Fatalf("the gesture answered before it appended, on rows that did "+
				"not hold the peer's record — nothing below is under test: %+v, %v",
				a.w, a.err)
		default:
		}
		if time.Now().After(deadline) {
			r.t.Fatal("the gesture never appended its record")
		}
		time.Sleep(time.Millisecond)
	}
	r.drain()
	a := <-done
	return a.w, a.err
}

// peerOn is a record another node wrote on subject.
func peerOn(t *testing.T, subject chatlet.Subject, op chatlet.OpKind, opID string,
	payload any) chatlet.MutationRecord {

	t.Helper()
	return chatlet.MutationRecord{
		RecordEnvelope: chatlet.RecordEnvelope{
			V: chatlet.RecordVersion, OpID: opID, Subject: subject, Op: op,
			CreatedAt: chatAt, Writer: "node-b",
			Scope: chatlet.ScopeSet{Subject: true},
		},
		Actor: "grace", ActorKind: chatlet.AuthorAgent,
		Mutation: mustJSON(t, payload),
	}
}

// verdictOf is the verdict this node's applier wrote for an operation.
func (r *writeRound) verdictOf(opID string) chatlet.Verdict {
	r.t.Helper()
	var v string
	if err := r.db.Read(r.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(r.t.Context(),
			`SELECT verdict FROM chat_verdicts WHERE op_id = ? ORDER BY position LIMIT 1`,
			opID).Scan(&v)
	}); err != nil {
		r.t.Fatalf("read the verdict of %s: %v", opID, err)
	}
	return chatlet.Verdict(v)
}

// refused holds a refused gesture's answer: the domain's sentinel, the
// verdict on the answer and in the rows, and no message named.
func refused(t *testing.T, r *writeRound, what string, got chatlet.Written, err error,
	sentinel error, verdict chatlet.Verdict) {

	t.Helper()
	if !errors.Is(err, sentinel) {
		t.Fatalf("%s was answered %v, want %v — a record the applier refused "+
			"answered as said is a remark the writer believes is in the room", what,
			err, sentinel)
	}
	if got.Verdict != verdict || r.verdictOf(got.ChangeID) != verdict {
		t.Errorf("%s: the answer carries verdict %q and the rows %q, want %q", what,
			got.Verdict, r.verdictOf(got.ChangeID), verdict)
	}
	if got.Message.ID != "" {
		t.Errorf("%s: a refused gesture names message %q", what, got.Message.ID)
	}
	if got.Outcome.Outcome != statelog.OutcomeApplied {
		t.Errorf("%s: the outcome is %q — the record was consumed, and the refusal "+
			"is what it did", what, got.Outcome.Outcome)
	}
}

// A REACTION PAST THE CAP IS REFUSED, and the writer says so: the thirty-
// second emoji landed from another node while this one's snapshot still held
// thirty-one, so its thirty-third was published — and is answered with the
// refusal the writer would have given had its snapshot held the thirty-second,
// which a later reaction on the same message is, before anything is appended.
//
// Mutation: answer [chatlet.VerdictReactionsFull] as landed in Store.decided, or
// drop the verdict read, and the race answers applied.
func TestAReactionPastTheCapIsRefusedAndTheWriterIsTold(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	room := r.room(person("jane"), "launch")
	said := r.say(person("jane"), room.Channel.ID, "ship it", "k-1")
	react := func(i int, emoji string) chatlet.MutationRecord {
		return peerOn(t, chatlet.MessageSubject(room.Channel.ID), chatlet.OpReact,
			fmt.Sprintf("5e2c1b90-0000-7000-8000-%012d", i), chatlet.Reaction{
				V: chatlet.DocumentVersion, MessageID: said.Message.ID, Emoji: emoji,
				By: "grace", Cap: chatlet.MaxReactionEmoji,
			})
	}
	for i := range chatlet.MaxReactionEmoji - 1 {
		r.peerRecord(react(i, fmt.Sprintf(":e%d:", i)))
	}

	got, err := r.raced(react(chatlet.MaxReactionEmoji, ":the-last-one:"),
		func(ctx context.Context) (chatlet.Written, error) {
			return r.store.React(ctx, keyed(person("jane"), requested("k-2")),
				room.Channel.ID, said.Message.ID, ":one-too-many:")
		})
	refused(t, r, "a reaction past the cap", got, err, chatlet.ErrInvalid,
		chatlet.VerdictReactionsFull)
	if n := r.count(`SELECT COUNT(DISTINCT emoji) FROM chat_reactions WHERE message_id = ?`,
		said.Message.ID); n != chatlet.MaxReactionEmoji {
		t.Errorf("the message carries %d distinct emoji, want %d", n, chatlet.MaxReactionEmoji)
	}

	// AND THE NEXT ONE IS REFUSED BEFORE IT IS APPENDED, in the same words:
	// this snapshot holds the message full.
	before, err := r.log.End(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.store.React(t.Context(), keyed(person("jane"), requested("k-3")),
		room.Channel.ID, said.Message.ID, ":another:")
	if !errors.Is(err, chatlet.ErrInvalid) {
		t.Fatalf("a reaction on a full message was answered %v", err)
	}
	if after, err := r.log.End(t.Context()); err != nil || after != before {
		t.Errorf("a reaction this snapshot holds full was appended (%d -> %d, %v)",
			before, after, err)
	}
}

// AN EDIT OF A MESSAGE TAKEN BACK AHEAD OF IT IS REFUSED: the delete landed
// from another node while this one's snapshot still held the words, so the
// edit was published — and writing text into the tombstone would undo a
// deletion nobody asked to undo, so the applier refuses it and the writer
// answers not found, as it would have with the delete in hand.
//
// Mutation: drop the tombstone arm's verdict in applyEdit (answer it applied)
// and the writer reports an edit that changed nothing.
func TestAnEditOfAMessageTakenBackAheadOfItIsRefused(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	room := r.room(person("jane"), "launch")
	said := r.say(person("jane"), room.Channel.ID, "ship it friday", "k-1")

	got, err := r.raced(peerOn(t, chatlet.MessageSubject(room.Channel.ID), chatlet.OpDelete,
		"5e2c1b90-0000-7000-8000-00000000d001", chatlet.MessageDelete{
			V: chatlet.DocumentVersion, MessageID: said.Message.ID,
			DeletedBy: "jane", DeletedByKind: chatlet.AuthorHuman,
		}),
		func(ctx context.Context) (chatlet.Written, error) {
			return r.store.Edit(ctx, keyed(person("jane"), requested("k-2")),
				room.Channel.ID, said.Message.ID, chatlet.EditMessage{Body: "ship it monday"})
		})
	refused(t, r, "an edit of a tombstone", got, err, chatlet.ErrNotFound,
		chatlet.VerdictDeleted)
	if n := r.count(`SELECT COUNT(*) FROM chat_messages WHERE id = ? AND body = '' AND `+
		`deleted_at IS NOT NULL AND edited_at IS NULL`, said.Message.ID); n != 1 {
		t.Error("the edit wrote into the tombstone")
	}
}

// A POST INTO A ROOM ARCHIVED AHEAD OF IT IS REFUSED, and so is its retry: the
// archive landed from another node while this one's snapshot held the room
// open, so the post was published — and is in no room, so the writer answers
// archived, and the same call made again is answered from the ledger with the
// first copy's verdict rather than as said.
//
// Mutation: read the verdict only where the answer is not collapsed and the
// retry answers applied; drop the archived arm in applyPost and the message
// lands in an archived room.
func TestAPostIntoARoomArchivedAheadOfItIsRefusedAndSoIsItsRetry(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	room := r.room(person("jane"), "launch")
	archived := true
	post := func(ctx context.Context) (chatlet.Written, error) {
		return r.store.Post(ctx, keyed(person("jane"), requested("k-1")), room.Channel.ID,
			chatlet.NewMessage{Body: "anybody there?"})
	}
	got, err := r.raced(peerOn(t, chatlet.ChannelSubject(room.Channel.ID), chatlet.OpPatch,
		"5e2c1b90-0000-7000-8000-00000000a001",
		chatlet.ChannelPatch{V: chatlet.DocumentVersion, Archived: &archived}), post)
	refused(t, r, "a post into a room archived ahead of it", got, err, chatlet.ErrArchived,
		chatlet.VerdictArchived)
	if n := r.count(`SELECT COUNT(*) FROM chat_messages WHERE channel_id = ?`,
		room.Channel.ID); n != 0 {
		t.Fatalf("the archived room holds %d messages", n)
	}

	retried, err := post(t.Context())
	if !errors.Is(err, chatlet.ErrArchived) || !retried.Outcome.Collapsed {
		t.Fatalf("the retry was answered %+v, %v — want the first copy's refusal, "+
			"from the ledger", retried, err)
	}
}

// A POST BY AN AUTHOR TAKEN OUT OF A PRIVATE ROOM AHEAD OF IT IS REFUSED: the
// membership that left her out landed from another node while this one's
// snapshot still had her in, and a room that admits its members only takes no
// word from somebody who is not one at the post's position.
//
// Mutation: stop carrying MembersOnly on the post and the message lands.
func TestAPostByAnAuthorTakenOutOfAPrivateRoomAheadOfItIsRefused(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "kim"))
	room, err := r.store.CreateChannel(t.Context(), person("jane"), chatlet.NewChannel{
		Name: "deal-room", Kind: chatlet.KindPrivate, Members: members("kim"),
	})
	if err != nil {
		t.Fatalf("create the private room: %v", err)
	}
	r.drain()

	got, err := r.raced(peerOn(t, chatlet.ChannelSubject(room.Channel.ID), chatlet.OpMembers,
		"5e2c1b90-0000-7000-8000-00000000b001", chatlet.MemberSet{
			V: chatlet.DocumentVersion, Members: []chatlet.Member{{Handle: "kim"}},
		}),
		func(ctx context.Context) (chatlet.Written, error) {
			return r.store.Post(ctx, keyed(person("jane"), requested("k-1")),
				room.Channel.ID, chatlet.NewMessage{Body: "my last word"})
		})
	refused(t, r, "a post by an author taken out of the room", got, err,
		chatlet.ErrForbidden, chatlet.VerdictNotMember)
	if n := r.count(`SELECT COUNT(*) FROM chat_messages WHERE channel_id = ?`,
		room.Channel.ID); n != 0 {
		t.Fatalf("the private room holds %d messages from an author not in it", n)
	}
}

// A REFUSED POST WAKES NOBODY, and an undecided one wakes as it always did.
//
// The change feed derives a wake from what this node's applier decided: a post
// it refused is in no room, so a seat woken for it would open the room to
// answer words nobody can read. Where this node has not decided the record —
// its applier further behind than one pull, or the decisions unreadable — the
// wake is the record's, because a feed that waited for an applier would make
// the company's notifications wait on the slowest data node.
//
// Mutation: ignore the verdict in Translator.landed and the refused post
// wakes; withhold the wake where nothing is decided and the lagging case goes
// quiet.
func TestARefusedPostWakesNobodyAndAnUndecidedOneWakesAsBefore(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	room := r.room(person("jane"), "launch", "grace")
	said, err := r.store.Post(t.Context(), keyed(person("jane"), requested("k-1")),
		room.Channel.ID, chatlet.NewMessage{Body: "@grace can you look?",
			Mentions: []string{"grace"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	r.drain()
	saidAt := r.consumed
	archived := true
	_, err = r.raced(peerOn(t, chatlet.ChannelSubject(room.Channel.ID), chatlet.OpPatch,
		"5e2c1b90-0000-7000-8000-00000000a002",
		chatlet.ChannelPatch{V: chatlet.DocumentVersion, Archived: &archived}),
		func(ctx context.Context) (chatlet.Written, error) {
			return r.store.Post(ctx, keyed(person("jane"), requested("k-2")),
				room.Channel.ID, chatlet.NewMessage{Body: "@grace still there?",
					Mentions: []string{"grace"}})
		})
	if !errors.Is(err, chatlet.ErrArchived) {
		t.Fatalf("the raced post was answered %v, so nothing below is under test", err)
	}
	refusedAt := r.consumed

	decided := chatlet.NewTranslator(chatlet.NodeDecisions{Estate: r.db.Reader(),
		Applied: r.waiter})
	if _, wake, err := decided.Translate(t.Context(), r.feedRecord(refusedAt)); err != nil ||
		wake {
		t.Errorf("a post the applier refused woke somebody (%v)", err)
	}
	if _, wake, err := decided.Translate(t.Context(), r.feedRecord(saidAt)); err != nil ||
		!wake {
		t.Errorf("a post the applier applied woke nobody (%v) — %s", err, said.ChangeID)
	}

	// AN APPLIER FURTHER BEHIND THAN ONE PULL is not waited for, and the
	// wake is the record's.
	lagging := chatlet.NewTranslator(chatlet.NodeDecisions{Estate: r.db.Reader(),
		Applied: behind{}})
	started := time.Now()
	rec := r.feedRecord(refusedAt)
	rec.Position += uint64(chatlet.DecisionLagRecords) + 1
	if _, wake, err := lagging.Translate(t.Context(), rec); err != nil || !wake {
		t.Errorf("a post this node has not decided woke nobody (%v)", err)
	}
	if took := time.Since(started); took >= chatlet.DecisionWait {
		t.Errorf("the feed waited %v for an applier a pull behind the record", took)
	}

	// AND DECISIONS THAT CANNOT BE READ FAIL OPEN, rather than leave a
	// person untold.
	unread := chatlet.NewTranslator(chatlet.NodeDecisions{})
	if _, wake, err := unread.Translate(t.Context(), r.feedRecord(refusedAt)); err != nil ||
		!wake {
		t.Errorf("a post whose verdict could not be read woke nobody (%v)", err)
	}
}

// behind is an applier at the start of the log.
type behind struct{}

func (behind) Committed() statelog.Position {
	return statelog.Position{Stream: chatlet.Domain{}.Stream().Name}
}

func (behind) WaitCommitted(ctx context.Context, _ statelog.Position) error {
	<-ctx.Done()
	return ctx.Err()
}

// feedRecord is the record at seq as the change feed is handed it.
func (r *writeRound) feedRecord(seq uint64) changefeed.Record {
	r.t.Helper()
	_, payload, _, ok, err := r.log.At(r.t.Context(), seq)
	if err != nil || !ok {
		r.t.Fatalf("read record %d: %v (present: %v)", seq, err, ok)
	}
	body, verdict := r.verifier.Open(payload)
	if verdict != statelog.Verified {
		r.t.Fatalf("record %d did not verify: %s", seq, verdict)
	}
	env, err := chatlet.DecodeEnvelope(body)
	if err != nil {
		r.t.Fatalf("decode record %d: %v", seq, err)
	}
	return changefeed.Record{ID: env.OpID, Position: seq,
		Stream: chatlet.Domain{}.Stream().Name, Gen: uint64(env.Gen),
		Key: env.Subject.String(), Payload: body}
}
