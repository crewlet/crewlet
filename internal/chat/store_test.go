package chat_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chat"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE WRITE AUTHORITY, END TO END: a record reaches the broker, the applier
// and this node's rows.
//
// Nothing between them is mocked — a real embedded broker, a real store, the
// shipped domain and the shipped applier — because every property here is a
// claim about the three of them AGREEING. The two that could not be reached
// any other way are the ones this domain is unusual for: that two people
// talking in one room contend over nothing, and that the same gesture repeated
// writes one message.

// chatAt is the writer's own pinned clock. Nothing it stamps reaches a row —
// every instant this domain stores is the broker's — so it appears in these
// tests only where a record's envelope is read back.
var chatAt = time.Date(2031, 4, 2, 3, 14, 0, 0, time.UTC)

type writeRound struct {
	t       *testing.T
	db      *store.DB
	log     *js.DomainLog
	store   *chat.Store
	applier *chat.Applier
	waiter  *chatWaiter

	consumed uint64
}

// tune amends the store's options, for the settings that are FOUNDER POLICY
// rather than properties of the harness — a company's default visibility is
// the only one so far, and it has two values that must both be exercised.
func newWriteRound(t *testing.T, roster chat.Roster,
	tune ...func(*chat.Options)) *writeRound {
	t.Helper()
	q, err := js.Open(t.Context(), js.Config{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open a broker: %v", err)
	}
	t.Cleanup(func() {
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("stop the broker: %v", err)
		}
	})
	spec := chat.Domain{}.Stream()
	// THE CEILING IS THE ONE FIELD THIS HARNESS OVERRIDES, and it is not a
	// property under test: [chat.ChatLogMaxBytes] is sized for a year of a
	// real company's conversation, and an embedded broker in a temporary
	// directory refuses to reserve it.
	if err := q.EnsureDomainStream(t.Context(), js.DomainStream{
		Name: spec.Name, Subjects: spec.Subjects, MaxBytes: 16 << 20,
		Duplicates: spec.Duplicates,
	}); err != nil {
		t.Fatalf("provision the log: %v", err)
	}
	log, err := q.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
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

	rows, err := chat.NewRows(db)
	if err != nil {
		t.Fatalf("build the read seam: %v", err)
	}
	fence := chat.NewFence(db, "node-a")
	// The published trim floor is zero on a fleet that has never trimmed,
	// which is the state every new company is in — and the state in which
	// an absent anchor really does mean an unclaimed address.
	fence.Floor = func(context.Context) (uint64, error) { return 0, nil }
	waiter := &chatWaiter{}
	publisher, err := statelog.NewPublisher(statelog.Deps{
		Domain: chat.Domain{}, Log: log, Rows: rows, Fence: fence,
		Gates: chat.NewGates(db), Waiter: waiter, NodeID: "node-a",
		Generation:    func() uint32 { return 0 },
		ResolveBudget: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("build the publisher: %v", err)
	}
	opts := chat.Options{
		Publisher: publisher, DB: db, Roster: roster,
		Now: func() time.Time { return chatAt },
	}
	for _, amend := range tune {
		amend(&opts)
	}
	rooms, err := chat.NewStore(opts)
	if err != nil {
		t.Fatalf("build the store: %v", err)
	}
	return &writeRound{
		t: t, db: db, log: log, store: rooms,
		applier: chat.NewApplier("node-a", nil), waiter: waiter,
	}
}

// drain consumes every record the broker holds beyond what this node has
// applied, exactly as the framework's own loop does — one transaction per
// record, carrying the rows, the operation id, the anchor and the checkpoint
// together.
func (r *writeRound) drain() {
	r.t.Helper()
	last, err := r.log.End(r.t.Context())
	if err != nil {
		r.t.Fatalf("read the log's end: %v", err)
	}
	spec := chat.Domain{}.Stream()
	for seq := r.consumed + 1; seq <= last; seq++ {
		_, payload, storedAt, ok, err := r.log.At(r.t.Context(), seq)
		if err != nil {
			r.t.Fatalf("read record %d: %v", seq, err)
		}
		if !ok {
			continue
		}
		env, err := chat.DecodeEnvelope(payload)
		if err != nil {
			r.t.Fatalf("decode record %d: %v", seq, err)
		}
		record := statelog.Record{
			Envelope: statelog.Envelope{
				V: env.V, Kind: string(env.Subject.Kind),
				Subject: statelog.Subject{
					Kind: string(env.Subject.Kind), ID: env.Subject.ID,
				},
				Op: string(env.Op), OpID: env.OpID, Gen: env.Gen,
				Writer: env.Writer, Scope: env.Scope.Resolve(env.Subject),
			},
			Position: statelog.Position{
				Stream: spec.Name, Generation: env.Gen, Seq: seq,
			},
			Payload:  payload,
			StoredAt: storedAt,
		}
		if err := r.db.Replicated().Tx(r.t.Context(), func(tx *sql.Tx) error {
			reason, gated, err := r.applier.Gated(r.t.Context(), tx, record)
			if err != nil {
				return err
			}
			if gated {
				r.t.Logf("record %d gated: %s", seq, reason)
				return nil
			}
			if _, err := r.applier.Apply(r.t.Context(), tx, record,
				statelog.ApplyOptions{Now: chatAt, StoredAt: storedAt}); err != nil {
				return err
			}
			if _, err := tx.ExecContext(r.t.Context(), `
				INSERT INTO chat_ops (op_id, subject, position, applied_at)
				VALUES (?,?,?,?) ON CONFLICT (op_id) DO NOTHING`,
				env.OpID, env.Subject.String(), record.Position.Packed(),
				store.EncodeTime(storedAt)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(r.t.Context(), `
				INSERT INTO statelog_anchor (stream, subject, anchor)
				VALUES (?,?,?)
				ON CONFLICT (stream, subject) DO UPDATE SET
					anchor = MAX(anchor, excluded.anchor)`,
				record.Position.Stream,
				spec.SubjectPrefix+"."+env.Subject.String(),
				record.Position.Packed()); err != nil {
				return err
			}
			_, err = tx.ExecContext(r.t.Context(), `
				INSERT INTO statelog_cursor
					(stream, generation, seq, stream_created_at, updated_at)
				VALUES (?,?,?,0,0)
				ON CONFLICT (stream) DO UPDATE SET
					generation = excluded.generation, seq = excluded.seq`,
				record.Position.Stream, int64(record.Position.Generation),
				int64(record.Position.Seq))
			return err
		}); err != nil {
			r.t.Fatalf("apply record %d: %v", seq, err)
		}
		// THE POST-COMMIT HALF, which the framework runs after every
		// committed batch and never inside the transaction.
		r.applier.Committed(r.t.Context())
		r.consumed = seq
		r.waiter.reach(record.Position)
	}
}

// room makes a public room and applies it.
func (r *writeRound) room(actor chat.Actor, name string, seats ...string) chat.Written {
	r.t.Helper()
	got, err := r.store.CreateChannel(r.t.Context(), actor, chat.NewChannel{
		Name: name, Kind: chat.KindPublic, Members: members(seats...),
	})
	if err != nil {
		r.t.Fatalf("create #%s: %v", name, err)
	}
	r.drain()
	return got
}

// say posts one message from a person and applies it.
func (r *writeRound) say(actor chat.Actor, channelID, body, key string) chat.Written {
	r.t.Helper()
	got, err := r.store.Post(r.t.Context(), actor, channelID, chat.NewMessage{
		Body: body, OperationID: key,
	})
	if err != nil {
		r.t.Fatalf("post %q: %v", body, err)
	}
	r.drain()
	return got
}

// count is one number out of this node's own rows.
func (r *writeRound) count(query string, args ...any) int {
	r.t.Helper()
	var n int
	if err := r.db.Replicated().Read(r.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(r.t.Context(), query, args...).Scan(&n)
	}); err != nil {
		r.t.Fatalf("count (%s): %v", query, err)
	}
	return n
}

// lastRecord is the newest record on the log, decoded.
//
// READ BACK OFF THE BROKER rather than out of a row, because the properties it
// is used for are about what the WRITER put on the wire: a routing snapshot is
// not a column anywhere, and an absent one is the whole content of "this wakes
// nobody".
func (r *writeRound) lastRecord() chat.MutationRecord {
	r.t.Helper()
	last, err := r.log.End(r.t.Context())
	if err != nil {
		r.t.Fatalf("read the log's end: %v", err)
	}
	_, payload, _, ok, err := r.log.At(r.t.Context(), last)
	if err != nil || !ok {
		r.t.Fatalf("read record %d: %v (present: %v)", last, err, ok)
	}
	record, err := chat.Decode(payload)
	if err != nil {
		r.t.Fatalf("decode record %d: %v", last, err)
	}
	return record
}

// chatWaiter is this node's own applier as the publisher sees it.
type chatWaiter struct {
	mu sync.Mutex
	at statelog.Position
}

func (w *chatWaiter) reach(p statelog.Position) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if p.Packed() > w.at.Packed() {
		w.at = p
	}
}

func (w *chatWaiter) Committed() statelog.Position {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.at
}

func (w *chatWaiter) WaitCommitted(ctx context.Context, p statelog.Position) error {
	for {
		if w.Committed().Packed() >= p.Packed() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func (w *chatWaiter) WaitApplied(ctx context.Context, _ statelog.ScopeSet,
	p statelog.Position) error {
	return w.WaitCommitted(ctx, p)
}

// person is a `kind: human` seat, which is who a bearer token resolves to.
func person(handle string) chat.Actor {
	return chat.Actor{Handle: handle, Kind: chat.AuthorHuman}
}

// seat is an agent in a turn, which is what makes its message id derivable.
func seat(handle, turn string) chat.Actor {
	return chat.Actor{Handle: handle, Kind: chat.AuthorAgent, TurnID: turn}
}

// operatorFor is a bound operator token: the person it is bound to, plus the
// token's own id.
func operatorFor(handle string) chat.Actor {
	return chat.Actor{
		Handle: handle, Kind: chat.AuthorOperator, OperatorID: "tok-1",
	}
}

// TWO PEOPLE TALKING IN ONE ROOM CONTEND OVER NOTHING.
//
// A post carries NO expectation: the records commute, so neither writer is
// deciding against the other's state. The case that proves it is the second
// post decided from a snapshot that has NOT applied the first — which is
// exactly what an arbitrated subject cannot serve, because its expectation
// would be the anchor this node has not moved yet, and the writer would wait
// for an applier that only runs when this test lets it.
//
// The room's sequence is still contiguous afterwards, because the APPLIER
// mints it from log order rather than the writer computing it.
func TestTwoPeopleTalkingInOneRoomNeverContend(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	room := r.room(person("jane"), "launch", "eng")

	first, err := r.store.Post(t.Context(), person("jane"), room.Channel.ID,
		chat.NewMessage{Body: "are we shipping?", OperationID: "k-1"})
	if err != nil {
		t.Fatalf("jane's post: %v", err)
	}
	// NOT DRAINED: this node has not applied jane's message when bob
	// decides.
	second, err := r.store.Post(t.Context(), person("bob"), room.Channel.ID,
		chat.NewMessage{Body: "friday", OperationID: "k-2"})
	if err != nil {
		t.Fatalf("bob's post, decided from a snapshot without jane's: %v — a "+
			"message that loses a race is a message somebody typed and lost",
			err)
	}
	for _, got := range []chat.Written{first, second} {
		if got.Outcome.Outcome != statelog.OutcomePending &&
			got.Outcome.Outcome != statelog.OutcomeApplied {
			t.Fatalf("outcome = %q, want applied or pending: the record is "+
				"durable either way", got.Outcome.Outcome)
		}
		if got.Outcome.Rounds != 1 {
			t.Errorf("a post took %d compare-and-set rounds — an additive "+
				"write forms no expectation, so there is nothing for it to "+
				"lose", got.Outcome.Rounds)
		}
	}
	r.drain()

	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE channel_id = ?`,
		room.Channel.ID); got != 2 {
		t.Fatalf("the room holds %d messages, want 2", got)
	}
	// AND THE SEQUENCE IS CONTIGUOUS, which is what lets a browser detect
	// a dropped live frame and refetch exactly the hole.
	for seq, id := range map[int]string{1: first.Message.ID, 2: second.Message.ID} {
		if got := r.count(
			`SELECT COUNT(*) FROM chat_messages WHERE id = ? AND channel_seq = ?`,
			id, seq); got != 1 {
			t.Errorf("message %s is not at sequence %d — the applier mints it "+
				"from log order, and a hole in it is indistinguishable from a "+
				"frame a viewer missed", id, seq)
		}
	}
}

// A RETRIED POST WRITES ONE MESSAGE.
//
// A post arbitrates nothing, so there is no broker rejection to tell a retry
// from a second remark: what makes it idempotent is that the gesture derives
// the SAME message id and the SAME operation id from what the caller can
// prove. Two identifiers for one thing would be two places for a retry to
// disagree with itself, so they are one value.
func TestARetriedPostWritesOneMessage(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	room := r.room(person("jane"), "launch")

	var ids []string
	for range 3 {
		got := r.say(person("jane"), room.Channel.ID, "are we shipping?", "submit-7")
		ids = append(ids, got.Message.ID)
		if got.ChangeID != got.Message.ID {
			t.Fatalf("the record's operation id is %s and the message's id is "+
				"%s — one message is one operation, and two identifiers for "+
				"one thing are two places for a retry to disagree with itself",
				got.ChangeID, got.Message.ID)
		}
	}
	if ids[0] != ids[1] || ids[1] != ids[2] {
		t.Fatalf("one submission derived three ids: %v", ids)
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE channel_id = ?`,
		room.Channel.ID); got != 1 {
		t.Fatalf("the room holds %d copies of one remark, want 1 — a person "+
			"who pressed send once said it once", got)
	}
}

// A SEAT POSTING TWICE IN ONE TURN WRITES TWO MESSAGES.
//
// A turn legitimately says more than one thing — an answer, and then a note
// about what it did — so a message id derived from the turn ALONE would make
// the second remark overwrite the first, everywhere, silently. The ordinal is
// the caller's own call counter, which is why a re-run turn that makes the
// same calls in the same order still writes each message once.
func TestASeatPostingTwiceInOneTurnWritesTwoMessages(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents())
	room := r.room(person("jane"), "launch", "eng")

	post := func(ordinal int, body string) chat.Written {
		t.Helper()
		got, err := r.store.Post(t.Context(), seat("eng", "turn-9"),
			room.Channel.ID, chat.NewMessage{Body: body, Ordinal: ordinal})
		if err != nil {
			t.Fatalf("the turn's post %d: %v", ordinal, err)
		}
		r.drain()
		return got
	}
	first := post(0, "on it")
	second := post(1, "opened ENG-14")
	if first.Message.ID == second.Message.ID {
		t.Fatalf("both of the turn's messages derived %s — the second remark "+
			"would overwrite the first on every node", first.Message.ID)
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE channel_id = ?`,
		room.Channel.ID); got != 2 {
		t.Fatalf("one turn's two remarks wrote %d messages, want 2", got)
	}

	// AND THE SAME CALL, MADE AGAIN BY A RE-RUN TURN, IS THE SAME MESSAGE.
	again := post(0, "on it")
	if again.Message.ID != first.Message.ID {
		t.Fatalf("the re-run turn's first call derived %s and the original "+
			"derived %s", again.Message.ID, first.Message.ID)
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE channel_id = ?`,
		room.Channel.ID); got != 2 {
		t.Fatalf("a re-run turn left the room with %d messages, want 2", got)
	}
}

// ONLY THE AUTHOR MAY EDIT A MESSAGE, and it is refused against the row read
// in the decision's own snapshot.
//
// A remark somebody else can rewrite is a remark attributed to a person who
// did not make it — on a row that outlives the thread and is quoted in every
// wake it produced. Removing somebody else's words is a delete and destroying
// them is an erase; both record WHO, which is exactly what an edit cannot.
func TestOnlyTheAuthorMayEditAMessage(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	room := r.room(person("jane"), "launch")
	said := r.say(person("jane"), room.Channel.ID, "we ship on friday", "k-1")

	for _, actor := range []chat.Actor{person("bob"), operatorFor("bob")} {
		_, err := r.store.Edit(t.Context(), actor, room.Channel.ID,
			said.Message.ID, chat.EditMessage{Body: "we ship on monday"})
		if !errors.Is(err, chat.ErrForbidden) {
			t.Fatalf("a %s rewrote somebody else's message: %v", actor.Kind, err)
		}
	}
	r.drain()
	if got := r.count(
		`SELECT COUNT(*) FROM chat_messages WHERE id = ? AND body = ?`,
		said.Message.ID, "we ship on friday"); got != 1 {
		t.Fatalf("the message no longer reads as its author wrote it")
	}

	if _, err := r.store.Edit(t.Context(), person("jane"), room.Channel.ID,
		said.Message.ID, chat.EditMessage{Body: "we ship on monday"}); err != nil {
		t.Fatalf("the author's own edit: %v", err)
	}
	r.drain()
	if got := r.count(
		`SELECT COUNT(*) FROM chat_messages WHERE id = ? AND body = ?`,
		said.Message.ID, "we ship on monday"); got != 1 {
		t.Fatalf("the author's edit did not land")
	}
}

// ONLY AN OPERATOR MAY ERASE, and the rule is in the DECIDE rather than only
// at whichever route was the way in.
//
// An erase is the one gesture that destroys a row. An author that could erase
// its own messages could erase the evidence of what it did — so a seat and a
// person are both refused, and the refusal happens where every route has to
// pass through.
func TestOnlyAnOperatorMayErase(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	room := r.room(person("jane"), "launch", "eng")
	said := r.say(person("jane"), room.Channel.ID, "my card number is 1234", "k-1")

	for _, actor := range []chat.Actor{person("jane"), seat("eng", "turn-1")} {
		_, err := r.store.Erase(t.Context(), actor, room.Channel.ID,
			[]string{said.Message.ID}, "leaked")
		if !errors.Is(err, chat.ErrForbidden) {
			t.Fatalf("a %s published an erase: %v", actor.Kind, err)
		}
	}
	r.drain()
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE id = ?`,
		said.Message.ID); got != 1 {
		t.Fatalf("a refused erase removed the message anyway")
	}

	if _, err := r.store.Erase(t.Context(), operatorFor("jane"), room.Channel.ID,
		[]string{said.Message.ID}, "leaked"); err != nil {
		t.Fatalf("the operator's erase: %v", err)
	}
	r.drain()
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE id = ?`,
		said.Message.ID); got != 0 {
		t.Fatalf("the operator's erase left the row in place")
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_deletions WHERE message_id = ?`,
		said.Message.ID); got != 1 {
		t.Fatalf("the erase wrote no marker — a redelivered post would " +
			"resurrect what the company destroyed, and no node could tell it " +
			"from a message that never existed")
	}
}

// AN ERASE THAT NAMES A MESSAGE IN ANOTHER ROOM IS REFUSED.
//
// The record states exactly what it removes, and the COUNT it carries is what
// the audit row renders — so an id from another room would file a redaction
// that did not happen.
func TestAnEraseNamingAnotherRoomsMessageIsRefused(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	launch := r.room(person("jane"), "launch")
	other := r.room(person("jane"), "random")
	said := r.say(person("jane"), other.Channel.ID, "unrelated", "k-1")

	_, err := r.store.Erase(t.Context(), operatorFor("jane"), launch.Channel.ID,
		[]string{said.Message.ID}, "wrong room")
	if !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("an erase reached across rooms: %v", err)
	}
}

// THE ROUTING IS RESOLVED INSIDE THE DECIDE AND COPIED ONTO THE RECORD.
//
// The node that wins a feed message may be one whose applier has not reached
// the post, so a parser that read the room's membership instead would route
// from a room it has not seen. What rides the record is the routed set — each
// recipient under its strongest reason — plus the mentions, which are what a
// card and a mention feed render even for somebody the wake deliberately does
// not reach.
func TestAPostCarriesTheRoutingItsRoomSupported(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "sarah"))
	room := r.room(person("jane"), "launch", "eng", "ops", "sarah")

	if _, err := r.store.Post(t.Context(), person("jane"), room.Channel.ID,
		chat.NewMessage{
			Body: "@eng @sarah can you look?", OperationID: "k-1",
			Mentions: []string{"eng", "sarah"},
		}); err != nil {
		t.Fatalf("post: %v", err)
	}
	notify := r.lastRecord().Notify
	if notify == nil {
		t.Fatal("a message anybody can answer carried no routing snapshot")
	}
	if got := chat.RecipientsOf(chat.Candidates(notify)); len(got) != 1 ||
		got[0].Handle != "eng" || got[0].Reason != chat.ReasonMention {
		t.Fatalf("the record wakes %v, want eng alone under a mention — sarah "+
			"is a person, and a wake runs a turn", got)
	}
	if !strings.Contains(strings.Join(notify.Mentions, ","), "sarah") {
		t.Errorf("the record names %v as mentioned — a mention of somebody the "+
			"routing does not wake is still a mention, and a card that showed "+
			"only the woken would render the message as though it had never "+
			"named them", notify.Mentions)
	}
	if notify.WakesTruncated {
		t.Errorf("a message naming two people reported its wake set truncated")
	}
}

// A UNIT ROOM'S LEAD IS RESOLVED AT THE WRITE AND RIDES THE RECORD.
//
// The routing arithmetic is never handed a roster to look a lead up in: the
// node that derives a wake from a record is rarely the node that wrote it, so
// a lead resolved at READ time would be whoever leads the unit when the record
// is read rather than who led it when somebody spoke. The decide asks the
// roster ONCE, for the room's own unit, and copies the answer onto the record.
// This is the only path in the engine that resolves one.
//
// The fallback it feeds is the narrow one: a PERSON spoke to a unit room and
// the arithmetic reached nobody else at all.
func TestAUnitRoomsLeadIsResolvedAtTheWrite(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane").withLead("eng", "amir"))
	room, err := r.store.CreateChannel(t.Context(), person("jane"), chat.NewChannel{
		Name: "engineering", Kind: chat.KindUnit, Unit: "eng",
	})
	if err != nil {
		t.Fatalf("create the unit room: %v", err)
	}
	r.drain()

	if _, err := r.store.Post(t.Context(), person("jane"), room.Channel.ID,
		chat.NewMessage{
			Body: "who is picking this up?", OperationID: "k-1",
		}); err != nil {
		t.Fatalf("post: %v", err)
	}
	notify := r.lastRecord().Notify
	if notify == nil {
		t.Fatal("a person's post in a unit room carried no routing snapshot")
	}
	if notify.Lead != "amir" {
		t.Fatalf("the record carries lead %q, want \"amir\" — resolved at the "+
			"write, a lead is who led the unit when somebody spoke; resolved "+
			"later it is whoever leads it now", notify.Lead)
	}
	if got := chat.RecipientsOf(chat.Candidates(notify)); len(got) != 1 ||
		got[0].Handle != "amir" || got[0].Reason != chat.ReasonLeadFallback {
		t.Fatalf("the record wakes %v, want amir alone under the lead "+
			"fallback — nobody else in this room was listening, and a unit "+
			"nobody answers is the case the fallback exists for", got)
	}
}

// AN IMPORT WAKES NOBODY, AND A SECOND PASS OVER ONE ARCHIVE WRITES NOTHING
// NEW.
//
// A year of somebody's Slack is a year of posts that already happened: a wake
// per message would page the whole company about conversations it has already
// had, at once. The rule is the record's SHAPE — no routing snapshot at all —
// because a flag would be read after a version-gated decode that a newer
// build's record does not survive.
func TestAnImportWakesNobodyAndRunsTwice(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	room := r.room(person("jane"), "launch", "eng")

	from := &chat.Imported{
		Source: "slack", VendorID: "1699999999.000100", Author: "eng",
		AuthorKind: chat.AuthorAgent,
		AuthoredAt: time.Date(2030, 11, 14, 9, 0, 0, 0, time.UTC),
	}
	first, err := r.store.Post(t.Context(), operatorFor("jane"), room.Channel.ID,
		chat.NewMessage{Body: "@jane ping", Mentions: []string{"jane"}, Imported: from})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if got := r.lastRecord(); got.Notify != nil {
		t.Fatalf("an imported message carried a routing snapshot: %+v — a "+
			"migration would page the company about a year of conversations "+
			"it has already had", got.Notify)
	}
	r.drain()

	second, err := r.store.Post(t.Context(), operatorFor("jane"), room.Channel.ID,
		chat.NewMessage{Body: "@jane ping", Mentions: []string{"jane"}, Imported: from})
	if err != nil {
		t.Fatalf("the second pass over one export: %v — an interrupted import "+
			"is re-run rather than resumed from a position somebody wrote down",
			err)
	}
	r.drain()
	if first.Message.ID != second.Message.ID {
		t.Fatalf("two passes over one message derived %s and %s",
			first.Message.ID, second.Message.ID)
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE channel_id = ?`,
		room.Channel.ID); got != 1 {
		t.Fatalf("two passes over one export wrote %d messages, want 1", got)
	}
	// AND THE CONVERSATION RENDERS THE YEAR IT ACTUALLY HAPPENED.
	if got := r.count(`SELECT COUNT(*) FROM chat_messages
		WHERE id = ? AND authored_at > 0 AND imported_source = ?`,
		first.Message.ID, "slack"); got != 1 {
		t.Errorf("the imported message kept no provenance, so a year of " +
			"history renders at the instant it was replayed")
	}
}

// A DIRECT CONVERSATION OPENS ONCE, FROM EITHER SIDE.
//
// Its id IS the participant set, so two people opening it from two nodes
// derive one room and the loser is told it already exists — which is the right
// answer, because it is the conversation. The second value reports that
// nothing was written, so a seat that opens the conversation on every send
// does not log a new one each time.
func TestADirectConversationOpensOnceFromEitherSide(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))

	first, created, err := r.store.OpenDirect(t.Context(), person("jane"),
		[]string{"eng"})
	if err != nil || !created {
		t.Fatalf("open a conversation: %v (created: %v)", err, created)
	}
	r.drain()

	// THE OTHER SIDE, NAMING THE PARTICIPANTS IN THE OTHER ORDER.
	second, created, err := r.store.OpenDirect(t.Context(), seat("eng", "turn-1"),
		[]string{"jane"})
	if err != nil {
		t.Fatalf("the other side's open: %v", err)
	}
	if created {
		t.Errorf("the second open reported that it created the conversation, " +
			"so a seat that opens it on every send logs a new one each time")
	}
	if second.Channel.ID != first.Channel.ID {
		t.Fatalf("two sides of one conversation derived %s and %s",
			first.Channel.ID, second.Channel.ID)
	}
	if second.Outcome.Outcome != statelog.OutcomeApplied {
		t.Errorf("opening a conversation that is already there answered %q — "+
			"an empty outcome is none of the three a caller may act on, and "+
			"the room is in hand", second.Outcome.Outcome)
	}
	if second.Revision == 0 {
		t.Errorf("the answer carries revision 0, which is both \"this node " +
			"has applied nothing\" and \"nobody answered\"")
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_channels WHERE kind = ?`,
		string(chat.KindDM)); got != 1 {
		t.Fatalf("one conversation between two people wrote %d rooms", got)
	}
}

// A DIRECT CONVERSATION'S MEMBERSHIP CANNOT BE PATCHED.
//
// Its identity IS its participant set: adding somebody does not widen this
// room, it names a different one — which every node would derive a different
// id for. A membership record here would leave one room whose id no longer
// describes who is in it.
func TestADirectConversationsMembershipCannotBePatched(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	opened, _, err := r.store.OpenDirect(t.Context(), person("jane"), []string{"eng"})
	if err != nil {
		t.Fatalf("open a conversation: %v", err)
	}
	r.drain()

	_, err = r.store.SetMembers(t.Context(), person("jane"), opened.Channel.ID,
		members("jane", "eng", "ops"))
	if !errors.Is(err, chat.ErrForbidden) {
		t.Fatalf("a conversation's participants were patched: %v", err)
	}
	if _, err := r.store.Join(t.Context(), person("bob"), opened.Channel.ID); !errors.
		Is(err, chat.ErrForbidden) {
		t.Fatalf("somebody let themselves into a conversation: %v", err)
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_members WHERE channel_id = ?`,
		opened.Channel.ID); got != 2 {
		t.Fatalf("the conversation holds %d participants, want the two its id "+
			"was derived from", got)
	}
}

// AN ARCHIVED ROOM KEEPS EVERY WORD AND TAKES NO NEW ONES.
func TestAPostIntoAnArchivedRoomIsRefused(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	room := r.room(person("jane"), "launch")
	r.say(person("jane"), room.Channel.ID, "shipping", "k-1")

	archived := true
	if _, err := r.store.PatchChannel(t.Context(), person("jane"),
		room.Channel.ID, chat.ChannelPatch{Archived: &archived}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	r.drain()

	_, err := r.store.Post(t.Context(), person("jane"), room.Channel.ID,
		chat.NewMessage{Body: "anybody there?", OperationID: "k-2"})
	if !errors.Is(err, chat.ErrArchived) {
		t.Fatalf("an archived room took a new message: %v", err)
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE channel_id = ?`,
		room.Channel.ID); got != 1 {
		t.Fatalf("the archived room holds %d messages, want the one said "+
			"before it closed", got)
	}
}

// A PATCH THAT CHANGES NOTHING PUBLISHES NOTHING AND IS STILL A SUCCESS.
//
// "Nothing changed" is an answer a caller acts on, and the room it did not
// change is what that caller reads out of it. The alternative — a record, a
// history row and a live frame for a change nobody made — is what an empty
// patch is refused for one step earlier.
func TestANoOpPatchAnswersAppliedWithTheRoomItDidNotChange(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	room := r.room(person("jane"), "launch")
	topic := "shipping on friday"
	if _, err := r.store.PatchChannel(t.Context(), person("jane"),
		room.Channel.ID, chat.ChannelPatch{Topic: &topic}); err != nil {
		t.Fatalf("set the topic: %v", err)
	}
	r.drain()
	before := r.count(`SELECT COUNT(*) FROM chat_history WHERE channel_id = ?`,
		room.Channel.ID)

	got, err := r.store.PatchChannel(t.Context(), person("jane"),
		room.Channel.ID, chat.ChannelPatch{Topic: &topic})
	if err != nil {
		t.Fatalf("the idempotent patch: %v", err)
	}
	r.drain()
	if got.Outcome.Outcome != statelog.OutcomeApplied {
		t.Errorf("outcome = %q, want %q: a patch that changes no field is one "+
			"the caller should be told landed", got.Outcome.Outcome,
			statelog.OutcomeApplied)
	}
	if got.Channel.ID != room.Channel.ID || got.Channel.Topic != topic {
		t.Errorf("an idempotent patch answered with %+v, want the room it read",
			got.Channel)
	}
	if got.Revision == 0 {
		t.Errorf("an idempotent patch reported revision 0 while reporting " +
			"success — the row did not move, so the only number there is is " +
			"the one the decision read")
	}
	if after := r.count(`SELECT COUNT(*) FROM chat_history WHERE channel_id = ?`,
		room.Channel.ID); after != before {
		t.Errorf("the room's activity went from %d entries to %d for a change "+
			"nobody made", before, after)
	}
}

// A CHANNEL NAME IS AN ADDRESS, AND IT CANNOT BE TAKEN TWICE.
//
// The name is the subject the create arbitrates on, so two people typing
// `#launch` contend at the broker and exactly one wins — where two fresh uuids
// would not contend at all and the company would hold two rooms with one name,
// every mention of which is a coin flip.
func TestAChannelNameIsAnAddressAndCannotBeTakenTwice(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	first := r.room(person("jane"), "launch")

	_, err := r.store.CreateChannel(t.Context(), person("bob"), chat.NewChannel{
		Name: "  LAUNCH ", Kind: chat.KindPublic,
	})
	if !errors.Is(err, chat.ErrNameTaken) {
		t.Fatalf("a second room took the same address: %v", err)
	}
	r.drain()
	if got := r.count(`SELECT COUNT(*) FROM chat_channels`); got != 1 {
		t.Fatalf("the company holds %d rooms, want 1", got)
	}
	if got := r.count(
		`SELECT COUNT(*) FROM chat_channel_names WHERE channel_id = ?`,
		first.Channel.ID); got != 1 {
		t.Fatalf("the address is held by %d claims, want 1", got)
	}
}

// THE THOUSAND-AND-FIRST ROOM IS REFUSED, NAMING THE CAP.
//
// Nothing in a record can count the company's rooms, so the cap is a fact
// about the estate and the only place it can be refused is inside a snapshot
// of one.
func TestTheRoomAboveTheCapIsRefusedNamingIt(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	// SEEDED DIRECTLY, which is the one place these tests write a
	// replicated row the applier did not: a thousand real creates is a
	// thousand broker round trips to assert a count, and what is under
	// test is the decision, not the applier that would write them.
	if err := r.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		for i := range chat.MaxChannels - 1 {
			if _, err := tx.ExecContext(t.Context(), `
				INSERT INTO chat_channels
					(id, name, name_norm, kind, created_at, version, document)
				VALUES (?, ?, ?, 'public', 0, 0, X'7B7D')`,
				fmt.Sprintf("seeded-%04d", i), fmt.Sprintf("room-%04d", i),
				fmt.Sprintf("room-%04d", i)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the company's rooms: %v", err)
	}

	if _, err := r.store.CreateChannel(t.Context(), person("jane"),
		chat.NewChannel{Name: "the-last-one", Kind: chat.KindPublic}); err != nil {
		t.Fatalf("the room at the cap was refused: %v — the cap is the number "+
			"of rooms a company may HOLD, not the number below it", err)
	}
	r.drain()

	_, err := r.store.CreateChannel(t.Context(), person("jane"),
		chat.NewChannel{Name: "one-too-many", Kind: chat.KindPublic})
	if err == nil || !strings.Contains(err.Error(), fmt.Sprint(chat.MaxChannels)) {
		t.Fatalf("the room above the cap was answered with %v — a refusal that "+
			"does not name the number leaves a founder guessing which of their "+
			"rooms to archive", err)
	}
}

// AN OPERATOR TOKEN BOUND TO NO SEAT WRITES NOTHING, AND THE REFUSAL NAMES
// WHAT TO SET.
//
// A person in chat IS a seat: the server resolves a bearer token to the `kind:
// human` seat bound to it, and a caller may never name one. So an unbound
// token has nothing to post AS — and "an actor needs a handle" would send an
// operator looking for a handle to type, which is exactly what this surface
// never accepts.
func TestAnUnboundOperatorTokenWritesNothing(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	room := r.room(person("jane"), "launch")

	_, err := r.store.Post(t.Context(), chat.Actor{
		Kind: chat.AuthorOperator, OperatorID: "tok-1",
	}, room.Channel.ID, chat.NewMessage{Body: "hello", OperationID: "k-1"})
	if !errors.Is(err, chat.ErrInvalid) {
		t.Fatalf("an unbound token posted: %v", err)
	}
	if !strings.Contains(err.Error(), "contact.crewlet_operator_id") {
		t.Fatalf("the refusal reads %q and names no field to set", err)
	}
}

// A REPLY TO A REPLY IS A REPLY TO THE THREAD, and the thread's own author is
// asked rather than merely told.
//
// A thread here is one level deep by construction, which is what makes "this
// thread" a range scan rather than a recursive walk — so the root is resolved
// from the message being answered rather than taken from a caller who may have
// a reply's id in hand.
func TestAReplyToAReplyIsAReplyToTheThread(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	room := r.room(person("jane"), "launch", "eng")
	root := r.say(person("jane"), room.Channel.ID, "are we shipping?", "k-1")

	first, err := r.store.Reply(t.Context(), seat("eng", "turn-1"),
		room.Channel.ID, root.Message.ID, chat.NewMessage{Body: "yes"})
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	r.drain()
	second, err := r.store.Reply(t.Context(), person("bob"), room.Channel.ID,
		first.Message.ID, chat.NewMessage{Body: "friday?", OperationID: "k-2"})
	if err != nil {
		t.Fatalf("reply to the reply: %v", err)
	}
	if second.Message.ThreadRoot != root.Message.ID {
		t.Fatalf("the second reply hangs off %s, want the thread's root %s",
			second.Message.ThreadRoot, root.Message.ID)
	}
	notify := r.lastRecord().Notify
	if notify == nil {
		t.Fatal("a reply carried no routing snapshot")
	}
	// THE THREAD'S AUTHOR IS A PERSON HERE, so the wake reaches the seat
	// that spoke in it — and the record still says the thread is jane's.
	if got := chat.RecipientsOf(chat.Candidates(notify)); len(got) != 1 ||
		got[0].Handle != "eng" {
		t.Fatalf("the reply wakes %v, want the seat that spoke in the thread", got)
	}
	r.drain()
	if got := r.count(`SELECT COUNT(*) FROM chat_messages
		WHERE channel_id = ? AND thread_root = ?`,
		room.Channel.ID, root.Message.ID); got != 2 {
		t.Fatalf("the thread holds %d replies, want 2", got)
	}
}

// A ROOM OF A KIND THIS BUILD DOES NOT KNOW REFUSES EVERY WRITE, which is the
// same answer [chat.Visible] gives a reader and the one the two halves have to
// agree on.
//
// This is the ROLLING-UPGRADE room, and it is reachable rather than
// theoretical: [chat.DecodeMutation]'s decoder deliberately does not validate,
// because an applier reading off the log must apply what the fleet accepted —
// so a newer node's create lands in this node's rows with a kind this build
// cannot classify. `privateRoom` is a two-valued question asked of an open
// enum, so such a room falls out of it as "not private", and an authorization
// that defaults to yes when the rule is unknown is the one direction of this
// that cannot be walked back: the read half refuses to serve the transcript
// while the write half accepts posts into it.
//
// THE ROOM IS BUILT FROM A RAW RECORD rather than through the store, because
// the store is exactly what cannot mint one: [chat.Store.CreateChannel]
// refuses a kind that is not `Named` and `OpenDirect` one that is not
// `Direct`. A record on the log is the only way in, which is the whole point.
func TestARoomOfAnUnknownKindRefusesEveryWrite(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))

	const id = "0bb6f0aa-7e42-4c7e-9f1e-2c6a1d5b3e70"
	const kind = chat.Kind("guild")
	if kind.Valid() {
		t.Fatalf("%q became a kind this build knows — pick one it does not, "+
			"or this case certifies nothing", kind)
	}
	r.peerRecord(chat.MutationRecord{
		RecordEnvelope: chat.RecordEnvelope{
			V: chat.RecordVersion, OpID: "5e2c1b90-0000-7000-8000-00000000c001",
			Subject: chat.ChannelSubject(id), Op: chat.OpCreate,
			CreatedAt: chatAt, Writer: "node-b",
			Scope: chat.ScopeSet{Subject: true},
		},
		Actor: "jane", ActorKind: chat.AuthorHuman,
		Mutation: mustJSON(t, chat.ChannelCreate{
			V: chat.DocumentVersion, ChannelID: id, Kind: kind,
			Members:   []chat.Member{{Handle: "jane"}},
			CreatedBy: "jane", CreatedByKind: chat.AuthorHuman,
		}),
	})
	if got := r.count(`SELECT COUNT(*) FROM chat_channels WHERE id = ? AND kind = ?`,
		id, string(kind)); got != 1 {
		t.Fatalf("the peer's room did not land, so nothing below is under test")
	}
	// THE ROW SAYS PUBLIC, which is what makes the Go guard load-bearing:
	// the applier derived the column from the same two-valued question and
	// had no better answer either.
	if got := r.count(`SELECT COUNT(*) FROM chat_channels WHERE id = ? AND private = 0`,
		id); got != 1 {
		t.Fatalf("the peer's room stored a privacy this build claimed to know")
	}

	// A MEMBER IS REFUSED TOO. Membership is what would have let jane in if
	// the room were merely private, so refusing her is what says the guard
	// is about the unknown RULE rather than about this actor.
	_, err := r.store.Post(t.Context(), person("jane"), id,
		chat.NewMessage{Body: "hello?", OperationID: "k-1"})
	if !errors.Is(err, chat.ErrForbidden) {
		t.Fatalf("a post into a room of an unknown kind: %v", err)
	}
	if !strings.Contains(err.Error(), string(kind)) {
		t.Fatalf("the refusal does not name the kind it could not read: %v", err)
	}
	if _, err := r.store.Join(t.Context(), person("jane"), id); !errors.Is(
		err, chat.ErrForbidden) {
		t.Fatalf("a join of a room of an unknown kind: %v", err)
	}
	if _, err := r.store.PatchChannel(t.Context(), person("jane"), id,
		chat.ChannelPatch{V: chat.DocumentVersion, Topic: strptr("ours now")},
	); !errors.Is(err, chat.ErrForbidden) {
		t.Fatalf("a patch of a room of an unknown kind: %v", err)
	}
	r.drain()
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE channel_id = ?`,
		id); got != 0 {
		t.Fatalf("%d messages landed in a room this build cannot classify", got)
	}
}

// peerRecord publishes a record this build could not have minted and applies
// it, which is what a newer node in the fleet looks like from here.
func (r *writeRound) peerRecord(rec chat.MutationRecord) {
	r.t.Helper()
	body, err := chat.Encode(rec)
	if err != nil {
		r.t.Fatalf("encode the peer's record: %v", err)
	}
	subject := topics.ChatLogSubject(string(rec.Subject.Kind), rec.Subject.ID)
	if _, _, err := r.log.Append(r.t.Context(), subject, rec.OpID, nil,
		body); err != nil {
		r.t.Fatalf("append the peer's record: %v", err)
	}
	r.drain()
}

// mustJSON is a payload as a record carries it.
func mustJSON(t *testing.T, payload any) []byte {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode the payload: %v", err)
	}
	return body
}

// strptr is a patch field that is set, which is what the pointer is for.
func strptr(s string) *string { return &s }

// A PRIVATE ROOM REFUSES AN OUTSIDER'S REACTION, which is the gesture that
// would otherwise be the one way to write into a conversation you cannot open.
//
// An edit and a delete are gated on AUTHORSHIP, which is strictly narrower for
// an outsider — nobody is the author of a message in a room they were never in
// — and a post takes the shared reachability rule. A reaction has no gate of
// its own: without the shared one, a handle that knows a room id and a message
// id attaches an attributable, durable row inside somebody else's private
// conversation, and every reader of that room then renders it.
func TestAPrivateRoomRefusesAnOutsidersReaction(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	made, err := r.store.CreateChannel(t.Context(), person("jane"),
		chat.NewChannel{Name: "board", Kind: chat.KindPrivate})
	if err != nil {
		t.Fatalf("create the private room: %v", err)
	}
	r.drain()
	room := made.Channel.ID
	said := r.say(person("jane"), room, "the number is confidential", "k-1")

	if _, err := r.store.React(t.Context(), person("bob"), room,
		said.Message.ID, ":eyes:"); !errors.Is(err, chat.ErrForbidden) {
		t.Fatalf("an outsider reacted in a private room: %v", err)
	}
	r.drain()
	if got := r.count(`SELECT COUNT(*) FROM chat_reactions WHERE message_id = ?`,
		said.Message.ID); got != 0 {
		t.Fatalf("%d reactions landed from outside the room", got)
	}

	// THE MEMBER'S OWN REACTION STILL LANDS, which is what says the guard
	// is about the room rather than about reactions.
	if _, err := r.store.React(t.Context(), person("jane"), room,
		said.Message.ID, ":eyes:"); err != nil {
		t.Fatalf("a member's own reaction: %v", err)
	}
	r.drain()
	if got := r.count(`SELECT COUNT(*) FROM chat_reactions
		WHERE message_id = ? AND handle = ?`, said.Message.ID, "jane"); got != 1 {
		t.Fatalf("a member's reaction did not land")
	}
}
