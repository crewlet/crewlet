package chatlet_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/config"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
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
	db      store.ReplicatedHandle
	log     *js.DomainLog
	store   *chatlet.Store
	gate    *chatlet.GateWriter
	applier *chatlet.Applier
	waiter  *chatWaiter

	// signer and verifier are the log's record signing pair: every record
	// the publisher appends is sealed, and the drain opens each frame
	// before the domain decodes its body, as the framework's own loop does.
	signer   *statelog.Signer
	verifier *statelog.Verifier

	consumed uint64

	// brokerShift is added to every broker instant the drain applies a
	// record at, for a case about what a MONTH of the broker's clock does
	// to the rows: the log's own instants are the embedded broker's, and
	// the horizons the applier prunes by are measured against them.
	brokerShift time.Duration
}

// tune amends the store's options, for the settings that are FOUNDER POLICY
// rather than properties of the harness — a company's default visibility is
// the only one so far, and it has two values that must both be exercised.
func newWriteRound(t *testing.T, roster chatlet.Roster,
	tune ...func(*chatlet.Options)) *writeRound {
	t.Helper()
	// THE BROKER DECLARES ITS SIZE, for brokertest's reason: left to derive
	// its cap from the runner's free space it takes three quarters of it,
	// and would refuse even this log's floor on a runner with too little
	// free, whatever the case under it does.
	q, err := js.Open(t.Context(), js.Config{StoreDir: t.TempDir(),
		StoreMaxBytes: brokertest.StoreMaxBytes})
	if err != nil {
		t.Fatalf("open a broker: %v", err)
	}
	t.Cleanup(func() {
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("stop the broker: %v", err)
		}
	})
	spec := chatlet.Domain{}.Stream()
	// THE CEILING IS THE ONE FIELD THIS HARNESS OVERRIDES, and it is the
	// floor Tier A holds stream.chat_log_max_bytes to: the smallest log any
	// node runs, which is the one the gate reserve takes the largest share
	// of, so every write here is admitted against the tightest ordinary
	// ceiling a production log has — and a reservation an embedded broker
	// in a temporary directory can grant, where [chatlet.ChatLogMaxBytes] may
	// not be. Everything else is the domain's own declaration, its largest
	// record included.
	if err := q.EnsureDomainStream(t.Context(), js.DomainStream{
		Name: spec.Name, Subjects: spec.Subjects, MaxBytes: config.ChatLogMaxBytesFloor,
		MaxMessageBytes: spec.MaxAppendBytes(), Duplicates: spec.Duplicates,
	}); err != nil {
		t.Fatalf("provision the log: %v", err)
	}
	log, err := q.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	node, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"),
		store.Options{}, 1)
	t.Cleanup(func() {
		if err := node.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})

	rows, err := chatlet.NewRows(db.Reader(), spec)
	if err != nil {
		t.Fatalf("build the read seam: %v", err)
	}
	fence := chatlet.NewFence(db.Reader(), "node-a")
	// The published trim floor is zero on a fleet that has never trimmed,
	// which is the state every new company is in — and the state in which
	// an absent anchor really does mean an unclaimed address. The log's own
	// first sequence is the fence's other bound and its last the check that
	// this node is on this log at all, both read from the stream the way
	// the engine reads them.
	fence.Floor = func(context.Context, uint32) (uint64, error) { return 0, nil }
	fence.Ends = func(ctx context.Context) (statelog.LogEnds, error) {
		first, last, err := log.Bounds(ctx)
		return statelog.LogEnds{First: first, Last: last}, err
	}
	waiter := &chatWaiter{}
	// THIS NODE'S CHECKPOINT IS THE WAITER'S, which is what the harness's
	// own applier advances — the position the end is compared against.
	fence.Committed = waiter.Committed
	// THE LOG'S GATE RESERVE, reading its usage from the stream as the
	// engine's does, so every write here is admitted as a production one is.
	reserve, err := statelog.NewReserve(spec,
		func(ctx context.Context) (statelog.Usage, error) {
			stats, err := log.Stats(ctx)
			return statelog.Usage{Bytes: stats.Bytes, MaxBytes: stats.MaxBytes}, err
		})
	if err != nil {
		t.Fatalf("build the gate reserve: %v", err)
	}
	signer, verifier := testSigner(t), testVerifier(t)
	publisher, err := statelog.NewPublisher(statelog.Deps{
		Domain: chatlet.Domain{}, Spec: spec, Log: log,
		Records: statelog.VerifiedLog{Log: log, Verifier: verifier},
		Rows:    rows, Fence: fence, Signer: signer,
		Gates: chatlet.NewGates(db.Reader()), Waiter: waiter,
		Voids: waiter, Identity: waiter, NodeID: "node-a",
		Admission:     reserve,
		Generation:    func() uint32 { return 0 },
		ResolveBudget: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("build the publisher: %v", err)
	}
	opts := chatlet.Options{
		Publisher: publisher, DB: db.Reader(), Roster: roster,
		Now: func() time.Time { return chatAt },
	}
	for _, amend := range tune {
		amend(&opts)
	}
	rooms, err := chatlet.NewStore(opts)
	if err != nil {
		t.Fatalf("build the store: %v", err)
	}
	// THE NODE GATE'S WRITER IS GIVEN THE PUBLISHER AND THE CLOCK AND
	// NOTHING ELSE — no roster and no estate — which is how the engine's
	// node gate builds it on a node where no chat roster is wired.
	gate, err := chatlet.NewGateWriter(chatlet.GateOptions{
		Publisher: publisher, Now: func() time.Time { return chatAt },
	})
	if err != nil {
		t.Fatalf("build the gate writer: %v", err)
	}
	return &writeRound{
		t: t, db: db, log: log, store: rooms, gate: gate,
		applier: chatlet.NewApplier("node-a", nil), waiter: waiter,
		signer: signer, verifier: verifier,
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
	spec := chatlet.Domain{}.Stream()
	for seq := r.consumed + 1; seq <= last; seq++ {
		_, payload, storedAt, ok, err := r.log.At(r.t.Context(), seq)
		if err != nil {
			r.t.Fatalf("read record %d: %v", seq, err)
		}
		if !ok {
			continue
		}
		storedAt = storedAt.Add(r.brokerShift)
		// THE FRAME FIRST, exactly as the framework's loop does it: a
		// record is signed on its way to the appender, and what a domain
		// decodes is the BODY.
		body, verdict := r.verifier.Open(payload)
		if verdict != statelog.Verified {
			r.t.Fatalf("record %d did not verify: %s", seq, verdict)
		}
		env, err := chatlet.DecodeEnvelope(body)
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
			Payload:  body,
			StoredAt: storedAt,
		}
		if err := r.db.Tx(r.t.Context(), func(tx *sql.Tx) error {
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
			// THE WIRE SUBJECT, as the framework's own applier records
			// it: the ledger's subject is what a write resolving an op id
			// compares against, so a harness that wrote the bare one
			// would have every write it resolves refused as a reuse.
			if _, err := tx.ExecContext(r.t.Context(), `
				INSERT INTO chat_ops (op_id, subject, position, applied_at, stored_at)
				VALUES (?,?,?,?,?) ON CONFLICT (op_id) DO NOTHING`,
				env.OpID, spec.SubjectPrefix+"."+env.Subject.String(),
				record.Position.Packed(), store.EncodeTime(storedAt),
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
func (r *writeRound) room(actor chatlet.Actor, name string, seats ...string) chatlet.Written {
	r.t.Helper()
	got, err := r.store.CreateChannel(r.t.Context(), actor, chatlet.NewChannel{
		Name: name, Kind: chatlet.KindPublic, Members: members(seats...),
	})
	if err != nil {
		r.t.Fatalf("create #%s: %v", name, err)
	}
	r.drain()
	return got
}

// orgChart is the engine making and maintaining a unit's room — the one
// author a unit room may have.
var orgChart = chatlet.Actor{Handle: "org-chart", Kind: chatlet.AuthorSystem}

// chartActivation is the instant the org chart these rounds make unit rooms
// from was activated.
var chartActivation = time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)

// unitRoom makes a unit's room as the org chart does, and applies it.
func (r *writeRound) unitRoom(name, unit string, seats ...string) chatlet.Written {
	r.t.Helper()
	got, err := r.store.CreateChannel(r.t.Context(), orgChart, chatlet.NewChannel{
		Name: name, Kind: chatlet.KindUnit, Unit: unit, Members: members(seats...),
		ActivatedAt: chartActivation,
	})
	if err != nil {
		r.t.Fatalf("create the unit room #%s: %v", name, err)
	}
	r.drain()
	return got
}

// say posts one message under a request's key and applies it.
func (r *writeRound) say(actor chatlet.Actor, channelID, body, key string) chatlet.Written {
	r.t.Helper()
	got, err := r.store.Post(r.t.Context(), keyed(actor, requested(key)), channelID, chatlet.NewMessage{
		Body: body})
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
	if err := r.db.Read(r.t.Context(), func(tx *sql.Tx) error {
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
func (r *writeRound) lastRecord() chatlet.MutationRecord {
	r.t.Helper()
	last, err := r.log.End(r.t.Context())
	if err != nil {
		r.t.Fatalf("read the log's end: %v", err)
	}
	_, payload, _, ok, err := r.log.At(r.t.Context(), last)
	if err != nil || !ok {
		r.t.Fatalf("read record %d: %v (present: %v)", last, err, ok)
	}
	body, verdict := r.verifier.Open(payload)
	if verdict != statelog.Verified {
		r.t.Fatalf("record %d did not verify: %s", last, verdict)
	}
	record, err := chatlet.Decode(body)
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

// Voided voids nothing: this harness applies by hand and never re-anchors.
func (w *chatWaiter) Voided(uint32, uint64) (statelog.Reason, bool) { return "", false }

// StreamIdentity is always the live stream: this harness never rebuilds its
// log, so every position the waiter holds is a sequence on it.
func (w *chatWaiter) StreamIdentity() error { return nil }
func (w *chatWaiter) Truncated() error      { return nil }

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

// person is a `kind: human` seat, which is who every principal of a person's
// resolves to.
func person(handle string) chatlet.Actor {
	return chatlet.Actor{Handle: handle, Kind: chatlet.AuthorHuman}
}

// seat is an agent in a turn. The run is provenance only: what makes its
// message ids derivable is the key its call carries ([chatlet.Actor.OpKey]).
func seat(handle, run string) chatlet.Actor {
	return chatlet.Actor{Handle: handle, Kind: chatlet.AuthorAgent, TurnID: run}
}

// keyed is actor carrying a caller's own operation key.
func keyed(actor chatlet.Actor, key string) chatlet.Actor {
	actor.OpKey = key
	return actor
}

// requestedAt is when every request key in these cases began: the test
// binary's start, so a ledger loss recorded "now" is after it and one recorded
// a retention ago is before it, as for any request a person just made.
var requestedAt = time.Now().UTC().Truncate(time.Millisecond)

// requested is the key a request carries — an operation id the engine
// minted, as every surface that takes one holds it to — derived from a name
// and begun at [requestedAt], so one name is one key.
func requested(seed string) string {
	return statelog.DeriveOpID(requestedAt, "", "crewlet.chat.test", seed)
}

// viaToken is a person acting through one of their own tokens: their seat,
// plus the token's own id as the credential.
func viaToken(handle string) chatlet.Actor {
	return chatlet.Actor{
		Handle: handle, Kind: chatlet.AuthorHuman, OperatorID: "tok-1",
	}
}

// credential is a credential acting as itself, under its own login: what
// erases, prunes and sets a room's retention, and speaks in no room.
func credential(login string) chatlet.Actor {
	return chatlet.Actor{Handle: login, Kind: chatlet.AuthorOperator, OperatorID: login}
}

// testSigner is this suite's record signer.
//
// EVERY RECORD ON EVERY STATE LOG IS SIGNED, so a write authority built
// without one is refused at construction rather than producing records the
// fleet would refuse one at a time. The key is a fixture: what these cases are
// about is what the domain writes, and the signature's own rules are certified
// in [statelog].
func testSigner(t *testing.T) *statelog.Signer {
	t.Helper()
	signer, err := statelog.NewSigner(chatlet.Domain{}.Name(),
		statelog.OneKey("k1", "test-material"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return signer
}

// testVerifier opens what [testSigner] sealed.
func testVerifier(t *testing.T) *statelog.Verifier {
	t.Helper()
	verifier, err := statelog.NewVerifier(chatlet.Domain{}.Name(),
		statelog.OneKey("k1", "test-material"))
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return verifier
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

	first, err := r.store.Post(t.Context(), keyed(person("jane"), requested("k-1")), room.Channel.ID,
		chatlet.NewMessage{Body: "are we shipping?"})
	if err != nil {
		t.Fatalf("jane's post: %v", err)
	}
	// NOT DRAINED: this node has not applied jane's message when bob
	// decides.
	second, err := r.store.Post(t.Context(), keyed(person("bob"), requested("k-2")), room.Channel.ID,
		chatlet.NewMessage{Body: "friday"})
	if err != nil {
		t.Fatalf("bob's post, decided from a snapshot without jane's: %v — a "+
			"message that loses a race is a message somebody typed and lost",
			err)
	}
	for _, got := range []chatlet.Written{first, second} {
		if got.Outcome.Outcome != statelog.OutcomePending &&
			got.Outcome.Outcome != statelog.OutcomeApplied {
			t.Fatalf("outcome = %q, want applied or pending: the record is "+
				"durable either way", got.Outcome.Outcome)
		}
		if got.Outcome.Rounds != 1 {
			t.Errorf("a post took %d compare-and-set rounds — a sequenced "+
				"write forms no expectation, so there is nothing for it to "+
				"lose", got.Outcome.Rounds)
		}
	}
	r.drain()

	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE channel_id = ? AND event = ''`,
		room.Channel.ID); got != 2 {
		t.Fatalf("the room holds %d messages, want 2", got)
	}
	// AND THE SEQUENCE IS CONTIGUOUS, which is what lets a browser detect
	// a dropped live frame and refetch exactly the hole. The room's opening
	// line (who joined it) took the numbers before them.
	opening := r.count(`SELECT COUNT(*) FROM chat_messages WHERE channel_id = ? AND event <> ''`,
		room.Channel.ID)
	if opening != 1 {
		t.Fatalf("the room opened with %d event lines, want the one saying who joined", opening)
	}
	for seq, id := range map[int]string{opening + 1: first.Message.ID, opening + 2: second.Message.ID} {
		if got := r.count(
			`SELECT COUNT(*) FROM chat_messages WHERE id = ? AND channel_seq = ?`,
			id, seq); got != 1 {
			t.Errorf("message %s is not at sequence %d — the applier mints it "+
				"from log order, and a hole in it is indistinguishable from a "+
				"frame a viewer missed", id, seq)
		}
	}
}

// A RETRIED POST WRITES ONE MESSAGE, AND IS ANSWERED WITH THE ONE THAT LANDED.
//
// A post arbitrates nothing, so there is no broker rejection to tell a retry
// from a second remark: what makes it idempotent is that the gesture derives
// the SAME message id and the SAME operation id from the caller's key and what
// it asked to say. Two identifiers for one thing would be two places for a
// retry to disagree with itself, so they are one value.
//
// AND THE RETRY'S ANSWER IS THE ROWS, NOT THE REQUEST. The ledger answers a
// resubmission before any decision runs, so the message it reports is read
// back — and the retries here name a colleague the first submission did not,
// which is what a roster that moved between a call and its retry does to the
// mentions a surface resolved from the same words. That is the only way to
// tell an answer read from the rows from one echoed off the call: a read-back
// that came back empty, or one that reported what the retry asked for, would
// tell the person their remark reached somebody it never named.
//
// A DIFFERENT REMARK UNDER THE SAME KEY IS A SECOND MESSAGE. The words are part
// of what the key derives from, which is how one request is one call: the act
// transport derives a request's operation from the call it carried, so a body
// that changed is a different call even where a client kept its request id.
func TestARetriedPostWritesOneMessage(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	room := r.room(person("jane"), "launch", "eng")

	first := r.say(person("jane"), room.Channel.ID, "are we shipping?", "submit-7")
	if first.Outcome.Collapsed {
		t.Fatalf("the first submission answered %+v — it decided the message, "+
			"so nothing was collapsed", first.Outcome)
	}
	if minted, ok := statelog.OpMintedAt(first.ChangeID); !ok || !minted.Equal(requestedAt) {
		t.Fatalf("the post's operation %s carries the instant %v (minted: %v), "+
			"want %s when the request began — an id with none is answered "+
			"`unknown` on every node whose ledger ever lost a row",
			first.ChangeID, minted, ok, requestedAt)
	}
	var landed struct {
		body, author string
		createdAt    int64
	}
	if err := r.db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `
			SELECT body, author_handle, created_at FROM chat_messages
			WHERE id = ?`, first.Message.ID).Scan(
			&landed.body, &landed.author, &landed.createdAt)
	}); err != nil {
		t.Fatalf("read the message that landed: %v", err)
	}

	for i := range 2 {
		got, err := r.store.Post(t.Context(), keyed(person("jane"), requested("submit-7")), room.Channel.ID,
			chatlet.NewMessage{Body: "are we shipping?", Mentions: []string{"eng"}})
		if err != nil {
			t.Fatalf("retry %d: %v", i+1, err)
		}
		r.drain()
		switch {
		case got.Message.ID != first.Message.ID:
			t.Fatalf("retry %d derived %s, and the submission %s — one "+
				"submission is one id", i+1, got.Message.ID, first.Message.ID)
		case got.ChangeID != got.Message.ID:
			t.Fatalf("the record's operation id is %s and the message's id is "+
				"%s — one message is one operation, and two identifiers for "+
				"one thing are two places for a retry to disagree with itself",
				got.ChangeID, got.Message.ID)
		case got.Outcome.Outcome != statelog.OutcomeApplied || !got.Outcome.Collapsed:
			t.Fatalf("retry %d answered %+v, want applied and collapsed — the "+
				"ledger held the operation, so no decision of this call's was "+
				"stored", i+1, got.Outcome)
		case got.Message.Body != landed.body:
			t.Fatalf("retry %d answered with the body %q, and the message that "+
				"landed says %q", i+1, got.Message.Body, landed.body)
		case len(got.Message.Mentions) != 0:
			t.Fatalf("retry %d answered naming %v, which is what IT asked for — "+
				"the message that landed named nobody, and a retry's answer is "+
				"the rows, never the request", i+1, got.Message.Mentions)
		case got.Message.Author != landed.author || got.Message.AuthorKind != chatlet.AuthorHuman:
			t.Fatalf("retry %d answered with author %q (%s), want %q", i+1,
				got.Message.Author, got.Message.AuthorKind, landed.author)
		case got.Message.CreatedAt.IsZero() ||
			store.EncodeTime(got.Message.CreatedAt) != landed.createdAt:
			// THROUGH THE COLUMN'S OWN ENCODING, which is coarser than
			// the document's: the row and the document carry one broker
			// instant at two precisions.
			t.Fatalf("retry %d answered created at %v, and the row says %v — "+
				"a message read back carries the broker's instant", i+1,
				got.Message.CreatedAt, store.DecodeTime(landed.createdAt))
		case got.Channel.Name != "launch":
			t.Fatalf("retry %d answered in room %+v, want #launch read back",
				i+1, got.Channel)
		case got.Revision == 0:
			t.Fatalf("retry %d answered revision 0, which is the one value a "+
				"caller cannot act on", i+1)
		}
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE channel_id = ? AND event = ''`,
		room.Channel.ID); got != 1 {
		t.Fatalf("the room holds %d copies of one remark, want 1 — a person "+
			"who pressed send once said it once", got)
	}

	other := r.say(person("jane"), room.Channel.ID, "are we shipping today?", "submit-7")
	switch {
	case other.Message.ID == first.Message.ID:
		t.Fatalf("a different remark under the same key took the first one's "+
			"id %s — it would be answered as its retry and never said", other.Message.ID)
	case other.Outcome.Collapsed:
		t.Fatalf("a different remark was answered as another copy: %+v", other.Outcome)
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE channel_id = ? AND event = ''`,
		room.Channel.ID); got != 2 {
		t.Fatalf("the room holds %d messages after two different remarks, want 2", got)
	}
}

// A RETRY THE BROKER COLLAPSED IS NOT ANSWERED WITH ITS OWN DECISION.
//
// The other collapsed answer: the earlier copy is on the log and this node has
// not applied it, so the ledger holds no row and the retry DECIDES — naming a
// colleague the copy that landed did not — and its append is collapsed onto
// the copy by the broker's duplicate window. That decision was never stored,
// so the message this call formed is not the message that exists, and
// reporting it would tell the person their remark reached somebody it never
// named. The answer is the identity alone until this node applies the copy.
func TestARetryTheBrokerCollapsedIsNotAnsweredWithItsOwnDecision(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	room := r.room(person("jane"), "launch", "eng")

	// NOT DRAINED: the first copy is on the log and nowhere else.
	first, err := r.store.Post(t.Context(), keyed(person("jane"), requested("submit-7")), room.Channel.ID,
		chatlet.NewMessage{Body: "are we shipping?"})
	if err != nil {
		t.Fatalf("the first submission: %v", err)
	}
	retry, err := r.store.Post(t.Context(), keyed(person("jane"), requested("submit-7")), room.Channel.ID,
		chatlet.NewMessage{Body: "are we shipping?", Mentions: []string{"eng"}})
	if err != nil {
		t.Fatalf("the retry: %v", err)
	}
	switch {
	case !retry.Outcome.Collapsed:
		t.Fatalf("the retry answered %+v — the broker collapsed it onto the "+
			"first copy, so its decision was never stored", retry.Outcome)
	case retry.Message.ID != first.Message.ID:
		t.Fatalf("the retry answered message %s, and the first copy is %s",
			retry.Message.ID, first.Message.ID)
	case retry.Message.Body != "" || len(retry.Message.Mentions) != 0:
		t.Fatalf("the retry answered %q naming %v, which is what IT decided — "+
			"this node has not applied the copy on the log, so the identity is "+
			"all it can say", retry.Message.Body, retry.Message.Mentions)
	}
	r.drain()
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE channel_id = ? AND event = ''`,
		room.Channel.ID); got != 1 {
		t.Fatalf("the room holds %d messages after a collapsed retry, want 1", got)
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_mentions WHERE message_id = ?`,
		first.Message.ID); got != 0 {
		t.Fatalf("the message holds %d mentions — the copy that landed named "+
			"nobody, and the retry's decision was never stored", got)
	}
}

// A POST THIS NODE'S LEDGER CANNOT VOUCH FOR NAMES NO MESSAGE.
//
// Once the operation ledger may have lost rows to its retention sweep — a
// turn whose trigger predates the ledger's thirty-day sweep — a post whose
// operation was minted before that point is answered `unknown`
// ([statelog.Result.Unvouched]), with no error, on BOTH of the framework's
// arms: a refusal its decision took on rows that may hold the operation's own
// first application, and a decision that ran to completion and was then never
// published. The second is the one that reads as a success.
// Its decision formed a whole message out of the request — id, body, author —
// and a caller handed that would report a remark as said that is nowhere on the
// log; the first would report one posted into a room that does not exist, or
// into one that takes no new messages. So every arm answers with the operation
// and nothing else: no message, no room, no revision.
//
// THE BOUND IS PAST EVERY MINT INSTANT, so the case holds whatever grammar the
// post's operation id takes — it is the answer to an unvouched post that is
// under test here, not which ids fall before the bound.
func TestAPostTheLedgerCannotVouchForNamesNoMessage(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	live := r.room(person("jane"), "launch")
	closed := r.room(person("jane"), "retro")
	archived := true
	if _, err := r.store.PatchChannel(t.Context(), person("jane"),
		closed.Channel.ID, chatlet.ChannelPatch{Archived: &archived}, chatlet.ChannelAuthority{Archive: true}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	r.drain()
	r.loseLedgerBefore(time.Now().Add(24 * time.Hour))
	before, err := r.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}

	for _, c := range []struct{ name, room string }{
		{"a room that does not exist", "no-such-room"},
		{"an archived room", closed.Channel.ID},
		// THE ARM THAT LOOKS LIKE A SUCCESS: nothing refuses, the
		// decision forms the message, and the framework declines to
		// publish it.
		{"a live room", live.Channel.ID},
	} {
		got, err := r.store.Post(t.Context(), keyed(person("jane"), requested("k-"+c.room)), c.room,
			chatlet.NewMessage{Body: "anybody there?"})
		switch {
		case err != nil:
			t.Fatalf("%s: a post the ledger cannot vouch for is answered "+
				"unknown, not refused: %v", c.name, err)
		case got.Outcome.Outcome != statelog.OutcomeUnknown || !got.Outcome.Unvouched:
			t.Fatalf("%s: answered %+v, want unknown and unvouched", c.name,
				got.Outcome)
		case got.Outcome.Collapsed:
			t.Fatalf("%s: answered collapsed — nothing landed under this "+
				"operation that this node knows of", c.name)
		}
		unvouchedAnswer(t, c.name, got)
	}
	if after, err := r.log.End(t.Context()); err != nil || after != before {
		t.Fatalf("the log's end moved from %d to %d (err %v) — an unvouched "+
			"post publishes nothing", before, after, err)
	}
}

// EVERY GESTURE THIS NODE'S LEDGER CANNOT VOUCH FOR ANSWERS WITH ITS OPERATION
// ALONE.
//
// The post is not special: every gesture's decision fills the answer — the
// room it read, the message it formed, the revision it read the room at —
// before the framework says whether the decision may stand, and the room and
// the revision are as much a description of rows that may hold the
// operation's own first application as the message is. Each gesture is
// driven down the arm it reaches: the ones that change something to the
// decision the framework declines to publish, and an edit by somebody who did
// not write the message to the refusal.
func TestEveryGestureTheLedgerCannotVouchForAnswersWithItsOperationAlone(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	room := r.room(person("jane"), "launch", "eng").Channel.ID
	said := r.say(person("jane"), room, "are we shipping?", "k-1").Message.ID
	r.loseLedgerBefore(time.Now().Add(24 * time.Hour))
	before, err := r.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}

	ctx := t.Context()
	topic := "ship it"
	system := chatlet.Actor{Handle: "retention", Kind: chatlet.AuthorSystem}
	for _, c := range []struct {
		name  string
		write func() (chatlet.Written, error)
	}{
		{"create a room", func() (chatlet.Written, error) {
			return r.store.CreateChannel(ctx, person("jane"),
				chatlet.NewChannel{Name: "standup", Kind: chatlet.KindPublic})
		}},
		{"open a direct conversation", func() (chatlet.Written, error) {
			got, wrote, err := r.store.OpenDirect(ctx, person("jane"),
				[]string{"bob"})
			if wrote {
				t.Errorf("open a direct conversation: reported a write beside "+
					"%+v — nothing this node can vouch for was written",
					got.Outcome)
			}
			return got, err
		}},
		{"patch the room", func() (chatlet.Written, error) {
			return r.store.PatchChannel(ctx, person("jane"), room,
				chatlet.ChannelPatch{Topic: &topic}, chatlet.ChannelAuthority{})
		}},
		{"set the members", func() (chatlet.Written, error) {
			return r.store.SetMembers(ctx, person("jane"), room,
				members("eng", "jane", "bob"))
		}},
		{"join", func() (chatlet.Written, error) {
			return r.store.Join(ctx, person("bob"), room)
		}},
		{"leave", func() (chatlet.Written, error) {
			return r.store.Leave(ctx, person("jane"), room)
		}},
		{"edit", func() (chatlet.Written, error) {
			return r.store.Edit(ctx, person("jane"), room, said,
				chatlet.EditMessage{Body: "are we shipping today?"})
		}},
		{"edit somebody else's message", func() (chatlet.Written, error) {
			return r.store.Edit(ctx, person("bob"), room, said,
				chatlet.EditMessage{Body: "words jane never said"})
		}},
		{"delete", func() (chatlet.Written, error) {
			return r.store.Delete(ctx, person("jane"), room, said, chatlet.MessageAuthority{})
		}},
		{"react", func() (chatlet.Written, error) {
			return r.store.React(ctx, person("jane"), room, said, ":eyes:")
		}},
		{"erase", func() (chatlet.Written, error) {
			return r.store.Erase(ctx, viaToken("jane"), statelog.NewOpID(time.Now(), ""), room,
				[]string{said}, "said in the wrong room")
		}},
		{"prune", func() (chatlet.Written, error) {
			return r.store.Prune(ctx, system, statelog.NewOpID(time.Now(), ""), room, chatAt.Add(-time.Hour))
		}},
	} {
		got, err := c.write()
		switch {
		case err != nil:
			t.Fatalf("%s: a write the ledger cannot vouch for is answered "+
				"unknown, not refused: %v", c.name, err)
		case got.Outcome.Outcome != statelog.OutcomeUnknown || !got.Outcome.Unvouched:
			t.Fatalf("%s: answered %+v, want unknown and unvouched", c.name,
				got.Outcome)
		}
		unvouchedAnswer(t, c.name, got)
	}
	if after, err := r.log.End(t.Context()); err != nil || after != before {
		t.Fatalf("the log's end moved from %d to %d (err %v) — an unvouched "+
			"write publishes nothing", before, after, err)
	}
}

// unvouchedAnswer holds an unvouched answer to the operation and nothing else.
func unvouchedAnswer(t *testing.T, name string, got chatlet.Written) {
	t.Helper()
	switch {
	case got.ChangeID == "" || got.ChangeID != got.Outcome.OpID:
		t.Fatalf("%s: answered operation %q beside an outcome for %q — the "+
			"operation is the one thing an unvouched answer names, because it "+
			"is what another node finishes", name, got.ChangeID, got.Outcome.OpID)
	case !reflect.ValueOf(got.Message).IsZero():
		t.Fatalf("%s: answered message %+v — this node cannot say which "+
			"remark, if any, exists, and a caller would report it as said",
			name, got.Message)
	case !reflect.ValueOf(got.Channel).IsZero():
		t.Fatalf("%s: answered room %+v — what a decision read on rows that "+
			"may hold the operation's own application is not an answer",
			name, got.Channel)
	case got.Revision != 0:
		t.Fatalf("%s: answered revision %d — zero is \"nobody answered\", "+
			"which is exactly what an unvouched answer is", name, got.Revision)
	}
}

// loseLedgerBefore records that this node's operation ledger may have lost
// every row for an operation minted before the instant, which is what its
// retention sweep leaves behind.
func (r *writeRound) loseLedgerBefore(at time.Time) {
	r.t.Helper()
	if err := r.db.Tx(r.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.t.Context(), `
			INSERT INTO statelog_ops_lost (ops_table, lost_before) VALUES (?, ?)`,
			chatlet.Domain{}.OpsTable(), store.EncodeTime(at))
		return err
	}); err != nil {
		r.t.Fatalf("record the ledger's loss: %v", err)
	}
}

// A SEAT POSTING TWICE IN ONE TURN WRITES TWO MESSAGES.
//
// A turn legitimately says more than one thing — an answer, and then a note
// about what it did — so a message id derived from the turn ALONE would make
// the second remark overwrite the first, everywhere, silently. The key's seed
// is the turn's unit of work and its repeat count is the run's own call log's
// (turnctx.CallLog: how many earlier calls to the same tool asked for
// something else), which is why a re-run that makes the same calls in the same
// order still writes each message once, and why the same words said again
// AFTER a different remark are a third message rather than the first one's
// retry. The keys below are what that derivation hands these calls, one per
// (tool, count) — the turn derives them, and this store holds each to one
// message ([chatlet.Actor.OpKey]).
func TestASeatPostingTwiceInOneTurnWritesTwoMessages(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents())
	room := r.room(person("jane"), "launch", "eng")

	post := func(tool string, repeat int, body string) chatlet.Written {
		t.Helper()
		key := statelog.DeriveOpID(requestedAt, "", "crewlet.chat.test", "wk-9",
			tool, fmt.Sprintf("repeat:%d", repeat))
		got, err := r.store.Post(t.Context(), keyed(seat("eng", "run-1"), key),
			room.Channel.ID, chatlet.NewMessage{Body: body})
		if err != nil {
			t.Fatalf("the turn's post %q: %v", body, err)
		}
		r.drain()
		return got
	}
	messages := func() int {
		t.Helper()
		return r.count(`SELECT COUNT(*) FROM chat_messages WHERE channel_id = ? AND event = ''`,
			room.Channel.ID)
	}
	first := post("post_message", 0, "on it")
	second := post("post_message", 1, "opened ENG-14")
	if first.Message.ID == second.Message.ID {
		t.Fatalf("both of the turn's messages derived %s — the second remark "+
			"would overwrite the first on every node", first.Message.ID)
	}
	if got := messages(); got != 2 {
		t.Fatalf("one turn's two remarks wrote %d messages, want 2", got)
	}

	// AND THE SAME CALL, MADE AGAIN BY A RE-RUN TURN, IS THE SAME MESSAGE.
	again := post("post_message", 0, "on it")
	if again.Message.ID != first.Message.ID || !again.Outcome.Collapsed {
		t.Fatalf("the re-run turn's first call derived %s (%+v) and the "+
			"original derived %s", again.Message.ID, again.Outcome, first.Message.ID)
	}
	if got := messages(); got != 2 {
		t.Fatalf("a re-run turn left the room with %d messages, want 2", got)
	}

	// THE SAME WORDS AFTER A DIFFERENT REMARK ARE A THIRD: the log counts
	// one earlier call to the tool that asked for something else.
	third := post("post_message", 1, "on it")
	if third.Message.ID == first.Message.ID || third.Outcome.Collapsed {
		t.Fatalf("\"on it\" said again after another remark took the first "+
			"one's id %s — it is answered as that remark's retry and never said",
			first.Message.ID)
	}
	// AND THROUGH ANOTHER TOOL THEY ARE ANOTHER MESSAGE. send_dm posts
	// exactly as post_message does, and each tool's calls are counted
	// apart, so without the tool in the key a direct message saying what
	// the first post said would be dropped as its retry.
	direct := post("send_dm", 0, "on it")
	if direct.Message.ID == first.Message.ID || direct.Outcome.Collapsed {
		t.Fatalf("the same words through send_dm took post_message's id %s",
			first.Message.ID)
	}
	if got := messages(); got != 4 {
		t.Fatalf("the turn said four things and the room holds %d messages", got)
	}
}

// ONE COUNT IN ANOTHER PLACE IS ANOTHER GESTURE: the room a message is said
// in, the message a reply answers, and the message and emoji a reaction names
// are each part of what the gesture IS.
//
// A turn's repeat count can hand two different calls the SAME number. A seat
// that says "on it" in #launch, then in #ops, then in #launch again counts the
// second and the third 1 each — one earlier call asked for something else
// before both — and the two differ only by the room. So does a reply under
// one message, under another, then under the first again, and a reaction
// that differs from the one before it only by its emoji — and a request's
// key names the request rather than the call, so one key can arrive with any
// of them. They are told apart by these parts and nothing else, so every one
// of them is held here, at the store, under one key: drop one from the
// identity ([statelog.KeyedOpID]'s object or content) and the second gesture
// is answered as the first's retry, `applied`, and never said.
func TestOneCountInAnotherPlaceIsAnotherGesture(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents())
	launch := r.room(person("jane"), "launch", "eng").Channel.ID
	ops := r.room(person("jane"), "ops", "eng").Channel.ID
	key := func(tool string) string { return requested("wk-place:" + tool) }
	actor := seat("eng", "run-1")
	fresh := func(name string, got chatlet.Written, err error) chatlet.Written {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Outcome.Collapsed {
			t.Fatalf("%s was answered as an earlier gesture's retry (%+v) — it "+
				"differs from that one only by what this case changed", name, got.Outcome)
		}
		r.drain()
		return got
	}

	// THE ROOM.
	inLaunch, err := r.store.Post(t.Context(), keyed(actor, key("post_message")), launch,
		chatlet.NewMessage{Body: "on it"})
	first := fresh(`"on it" in #launch`, inLaunch, err)
	inOps, err := r.store.Post(t.Context(), keyed(actor, key("post_message")), ops,
		chatlet.NewMessage{Body: "on it"})
	elsewhere := fresh(`"on it" in #ops`, inOps, err)
	if elsewhere.Message.ID == first.Message.ID || elsewhere.Message.ChannelID != ops {
		t.Fatalf(`"on it" in #ops answered %s in %s — #launch's message is %s`,
			elsewhere.Message.ID, elsewhere.Message.ChannelID, first.Message.ID)
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE channel_id = ? AND event = ''`,
		ops); got != 1 {
		t.Fatalf("#ops holds %d messages, want the one said there", got)
	}

	// THE MESSAGE A REPLY ANSWERS.
	other := r.say(person("jane"), launch, "and the cache?", "k-other").Message.ID
	under := func(root string) chatlet.Written {
		t.Helper()
		got, err := r.store.Reply(t.Context(), keyed(actor, key("reply_in_thread")), launch, root,
			chatlet.NewMessage{Body: "ok"})
		return fresh(`"ok" under `+root, got, err)
	}
	underFirst, underOther := under(first.Message.ID), under(other)
	if underOther.Message.ID == underFirst.Message.ID ||
		underOther.Message.ThreadRoot != other {
		t.Fatalf(`"ok" under %s answered %s in the thread of %q — the reply `+
			"under %s is %s", other, underOther.Message.ID,
			underOther.Message.ThreadRoot, first.Message.ID, underFirst.Message.ID)
	}

	// THE EMOJI, AND THE MESSAGE IT IS PUT ON.
	react := func(messageID, emoji string) {
		t.Helper()
		got, err := r.store.React(t.Context(), keyed(actor, key("react_to_message")),
			launch, messageID, emoji)
		fresh(emoji+" on "+messageID, got, err)
	}
	react(first.Message.ID, ":+1:")
	react(first.Message.ID, ":eyes:")
	react(other, ":eyes:")
	reactions := func(messageID string) int {
		t.Helper()
		return r.count(`SELECT COUNT(*) FROM chat_reactions WHERE message_id = ?`,
			messageID)
	}
	if got := reactions(first.Message.ID); got != 2 {
		t.Fatalf("%s carries %d reactions, want :+1: and :eyes:", first.Message.ID, got)
	}
	if got := reactions(other); got != 1 {
		t.Fatalf("%s carries %d reactions, want :eyes:", other, got)
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

	for _, actor := range []chatlet.Actor{person("bob"), viaToken("bob")} {
		_, err := r.store.Edit(t.Context(), actor, room.Channel.ID,
			said.Message.ID, chatlet.EditMessage{Body: "we ship on monday"})
		if !errors.Is(err, chatlet.ErrForbidden) {
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
		said.Message.ID, chatlet.EditMessage{Body: "we ship on monday"}); err != nil {
		t.Fatalf("the author's own edit: %v", err)
	}
	r.drain()
	if got := r.count(
		`SELECT COUNT(*) FROM chat_messages WHERE id = ? AND body = ?`,
		said.Message.ID, "we ship on monday"); got != 1 {
		t.Fatalf("the author's edit did not land")
	}
}

// A MESSAGE IS REFUSED FOR WHAT IT SAYS BY ONE RULE, which a gesture asks
// before it writes anything and the post asks again.
//
// send_dm opens a conversation and then says the message in it, and a message
// refused only once the open had landed left a room in every participant's
// list holding nothing. So the half of a post's refusals that needs no row is
// exported as [chatlet.NewMessage.Validate] — and it is the SAME check the post
// runs, which is what this holds: each refusal Validate answers is the one the
// post answers, word for word, and a message it accepts is one the post says.
func TestAMessageIsRefusedForWhatItSaysBeforeAnyRoomIsRead(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	room := r.room(person("jane"), "launch")
	many := func(n int, of string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf(of, i)
		}
		return out
	}
	for name, in := range map[string]chatlet.NewMessage{
		"nothing at all":        {},
		"a body over the cap":   {Body: strings.Repeat("x", chatlet.MaxBody+1)},
		"links over the cap":    {Body: "see", Links: many(chatlet.MaxLinks+1, "https://example.com/%d")},
		"an empty link":         {Body: "see", Links: []string{""}},
		"a link to no address":  {Body: "see", Links: []string{"#/worklet/ENG-1"}},
		"mentions over the cap": {Body: "all of you", Mentions: many(chatlet.MaxMentions+1, "seat-%d")},
	} {
		t.Run(name, func(t *testing.T) {
			refused := in.Validate()
			if !errors.Is(refused, chatlet.ErrInvalid) {
				t.Fatalf("Validate answered %v, want a field refusal", refused)
			}
			_, err := r.store.Post(t.Context(), keyed(person("jane"),
				requested("k-"+name)), room.Channel.ID, in)
			if err == nil || err.Error() != refused.Error() {
				t.Fatalf("the post answered %v where Validate answered %v — two "+
					"rules for one message", err, refused)
			}
		})
	}
	ok := chatlet.NewMessage{Body: "fine", Links: []string{"https://example.com/plan"}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("Validate refused a message the post says: %v", err)
	}
}

// AN EDIT THAT NAMES NO LINKS KEEPS THEM, and one that names a list — an empty
// one included — replaces them.
//
// The links are not derived from the body the way the mentions are, so an edit
// that fixed a typo without re-sending them used to remove every one: the
// gesture a person makes most often destroyed the part of the message they had
// not touched. The links the message ends with are resolved against the row in
// the edit's own snapshot and carried on the record, so every node writes them.
// And an edit that empties the body while keeping links is a message that
// still says something, while one that empties both is refused as the delete
// it would be.
func TestAnEditThatNamesNoLinksKeepsThem(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	room := r.room(person("jane"), "launch")
	said, err := r.store.Post(t.Context(), keyed(person("jane"), requested("k-1")), room.Channel.ID, chatlet.NewMessage{
		Body: "the plan is up", Links: []string{"https://example.com/plan"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	r.drain()
	links := func() int {
		return r.count(`SELECT COUNT(*) FROM chat_messages WHERE id = ? AND links = ?`,
			said.Message.ID, `["https://example.com/plan"]`)
	}

	if _, err := r.store.Edit(t.Context(), person("jane"), room.Channel.ID,
		said.Message.ID, chatlet.EditMessage{Body: "the plan is up (v2)"}); err != nil {
		t.Fatalf("an edit naming no links: %v", err)
	}
	r.drain()
	if links() != 1 {
		t.Fatalf("an edit that named no links removed the message's links")
	}

	// THE BODY MAY GO WHILE THE LINKS STAY: the message still points at
	// something, so it is not the delete an edit may not be.
	if _, err := r.store.Edit(t.Context(), person("jane"), room.Channel.ID,
		said.Message.ID, chatlet.EditMessage{}); err != nil {
		t.Fatalf("an edit emptying the body of a message with links: %v", err)
	}
	r.drain()
	if links() != 1 {
		t.Fatalf("an edit emptying only the body removed the links")
	}

	// AN EMPTY LIST IS HOW THEY ARE REMOVED — refused here, because the
	// body is already gone and nothing would be left.
	none := []string{}
	if _, err := r.store.Edit(t.Context(), person("jane"), room.Channel.ID,
		said.Message.ID, chatlet.EditMessage{Links: &none}); !errors.Is(err, chatlet.ErrInvalid) {
		t.Fatalf("an edit leaving the message empty answered %v, want invalid", err)
	}
	if _, err := r.store.Edit(t.Context(), person("jane"), room.Channel.ID,
		said.Message.ID, chatlet.EditMessage{Body: "plan withdrawn", Links: &none}); err != nil {
		t.Fatalf("an edit removing the links: %v", err)
	}
	r.drain()
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE id = ? AND links = ''`,
		said.Message.ID); got != 1 {
		t.Fatalf("an edit naming an empty list left the links in place")
	}

	// AND A MESSAGE WITH NO LINKS, EMPTIED BY AN EDIT THAT KEEPS NONE, IS
	// REFUSED INSIDE THE SNAPSHOT — the one place that can see there are
	// none to keep.
	bare := r.say(person("jane"), room.Channel.ID, "nothing to link", "k-2")
	if _, err := r.store.Edit(t.Context(), person("jane"), room.Channel.ID,
		bare.Message.ID, chatlet.EditMessage{}); !errors.Is(err, chatlet.ErrInvalid) {
		t.Fatalf("an edit emptying a message with no links answered %v, want invalid", err)
	}
}

// A LINK IS AN ADDRESS EVERY READER OPENS: an absolute http, https or mailto
// address, refused otherwise by the post, the reply and the edit alike, naming
// the entry to change, and stored exactly as it was sent.
//
// A message's links are read on a person's screen, by a seat in the thread it
// is handed and by any client that prints them, and a link that resolves
// against whichever page shows it, or one in a scheme the dashboard draws as
// words, is one none of them can follow. Each shape below sits second in its
// list, behind a link the room takes, so the refusal has to name the entry
// rather than the list, and two that are not addresses are named in the one
// refusal. Mutation: answer nil from checkLink once its size is checked, and
// every gesture below is said; name the list rather than the entry, and every
// refusal names `links`; stop at the first link that is not one, and the post
// with two names only the first; store the parsed address's own spelling, and
// the uppercase scheme comes back lowered.
func TestALinkIsAnAddressEveryReaderOpens(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	room := r.room(person("jane"), "launch")
	root := r.say(person("jane"), room.Channel.ID, "the plan is up", "k-root")
	const taken = "https://example.com/plan"
	for name, link := range map[string]string{
		"a relative reference":          "docs/plan.md",
		"a path on no host":             "/worklet/ENG-1",
		"a scheme-relative reference":   "//example.com/plan",
		"an app route":                  "#/worklet/ENG-1",
		"a script":                      "javascript:void(0)",
		"inline data":                   "data:text/html,plan",
		"a file on one machine's disk":  "file:///etc/hosts",
		"another scheme with a host":    "ftp://example.com/plan",
		"a web address with no host":    "https:///plan",
		"a web address with no slashes": "https:example.com/plan",
		"a port and no host":            "https://:443/plan",
		"a mail link to nobody":         "mailto:",
		"a mail link written as a host": "mailto://jane@example.com",
		"a space inside":                "https://example.com/the plan",
		"a space before":                " https://example.com/plan",
		"two addresses on two lines":    "https://example.com/a\nhttps://example.com/b",
		"an escape that is not one":     "https://example.com/%zz",
	} {
		links := []string{taken, link}
		for gesture, write := range map[string]func() error{
			"post": func() error {
				_, err := r.store.Post(t.Context(), keyed(person("jane"),
					requested("k-post-"+name)), room.Channel.ID,
					chatlet.NewMessage{Body: "see", Links: links})
				return err
			},
			"reply": func() error {
				_, err := r.store.Reply(t.Context(), keyed(person("jane"),
					requested("k-reply-"+name)), room.Channel.ID, root.Message.ID,
					chatlet.NewMessage{Body: "see", Links: links})
				return err
			},
			"edit": func() error {
				_, err := r.store.Edit(t.Context(), person("jane"), room.Channel.ID,
					root.Message.ID, chatlet.EditMessage{Body: "see", Links: &links})
				return err
			},
		} {
			var refusal *chatlet.InvalidError
			if err := write(); !errors.As(err, &refusal) || refusal.Field != "links[1]" {
				t.Errorf("%s: the %s linking to %q answered %v, want a refusal of "+
					"links[1]", name, gesture, link, err)
			}
		}
	}
	r.drain()
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE channel_id = ? AND event = ''`,
		room.Channel.ID); got != 1 {
		t.Fatalf("the room holds %d messages after every refused gesture, want "+
			"the one said before them", got)
	}
	// EVERY LINK THAT IS NOT ONE IS NAMED IN THE ONE REFUSAL, each by its
	// entry, and the link the room takes is not among them.
	_, err := r.store.Post(t.Context(), keyed(person("jane"), requested("k-several")),
		room.Channel.ID, chatlet.NewMessage{Body: "see",
			Links: []string{"/plan", taken, "ftp://example.com/plan"}})
	var several *chatlet.InvalidError
	if !errors.As(err, &several) || several.Field != "links" ||
		!strings.Contains(several.Why, "`links[0]`") ||
		!strings.Contains(several.Why, "`links[2]`") ||
		strings.Contains(several.Why, "`links[1]`") {
		t.Errorf("a post with two links that are no address answered %v, want both "+
			"entries named in one refusal and the address between them left out", err)
	}
	// THE REFUSAL OF ANOTHER SCHEME NAMES EVERY SCHEME A LINK IS WRITTEN IN,
	// so its sentence cannot fall behind the set the check reads.
	refused := chatlet.NewMessage{Body: "see", Links: []string{"ftp://example.com/plan"}}.Validate()
	if refused == nil {
		t.Fatalf("an ftp link is taken")
	}
	other := chatlet.Sentence(refused)
	words := strings.FieldsFunc(other, func(r rune) bool { return r < 'a' || r > 'z' })
	for _, scheme := range chatlet.LinkSchemes() {
		if !slices.Contains(words, scheme) {
			t.Errorf("an ftp link is refused with %q, which does not name %s", other, scheme)
		}
	}

	// AND THE THREE SCHEMES ARE STORED AS SENT, case, query and fragment
	// included, by a post and by an edit.
	stored := func(id string) []string {
		t.Helper()
		var raw string
		if err := r.db.Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(),
				`SELECT links FROM chat_messages WHERE id = ?`, id).Scan(&raw)
		}); err != nil {
			t.Fatalf("read the links of %s: %v", id, err)
		}
		var links []string
		if err := json.Unmarshal([]byte(raw), &links); err != nil {
			t.Fatalf("the links column holds %q, which is not a list: %v", raw, err)
		}
		return links
	}
	// Each call is handed a copy, so a check that rewrote the list it was
	// given could not rewrite what the row is compared against.
	sent := []string{
		"http://intranet.example/runbook",
		"HTTPS://Example.com/Plan?v=2&view=full#risks",
		"mailto:ops@example.com?subject=launch",
	}
	said, err := r.store.Post(t.Context(), keyed(person("jane"), requested("k-sent")),
		room.Channel.ID, chatlet.NewMessage{Body: "the sources", Links: slices.Clone(sent)})
	if err != nil {
		t.Fatalf("a post linking to an http, an https and a mailto address: %v", err)
	}
	r.drain()
	if got := stored(said.Message.ID); !slices.Equal(got, sent) {
		t.Errorf("the post stored the links %q, want %q as sent", got, sent)
	}
	slices.Reverse(sent)
	edited := slices.Clone(sent)
	if _, err := r.store.Edit(t.Context(), person("jane"), room.Channel.ID,
		said.Message.ID, chatlet.EditMessage{Body: "the sources", Links: &edited}); err != nil {
		t.Fatalf("an edit naming the same three links: %v", err)
	}
	r.drain()
	if got := stored(said.Message.ID); !slices.Equal(got, sent) {
		t.Errorf("the edit stored the links %q, want %q as sent", got, sent)
	}
}

// ONLY A PERSON OR A CREDENTIAL MAY ERASE, and the rule is in the DECIDE rather
// than only at whichever route was the way in.
//
// An erase is the one gesture that destroys a row. An author that could erase
// its own messages could erase the evidence of what it did — so a seat is
// refused, and so is the engine, whose one destruction is the retention duty's
// prune; and the refusal happens where every route has to pass through: a
// seat's own words are what a compliance erase is about. Who among the people
// and credentials may is the authority table's, decided before the store is
// reached. Mutation: admit a seat or the engine in [chatlet.Store.Erase]'s decide
// and its case below erases the message.
func TestOnlyAPersonOrACredentialMayErase(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	room := r.room(person("jane"), "launch", "eng")
	said := r.say(seat("eng", "turn-0"), room.Channel.ID, "my card number is 1234", "k-1")

	for name, actor := range map[string]chatlet.Actor{
		"a seat, erasing its own message": seat("eng", "turn-1"),
		"the engine":                      {Handle: "retention", Kind: chatlet.AuthorSystem},
	} {
		if _, err := r.store.Erase(t.Context(), actor,
			statelog.NewOpID(time.Now(), ""), room.Channel.ID,
			[]string{said.Message.ID}, "leaked"); !errors.Is(err, chatlet.ErrForbidden) {
			t.Fatalf("%s published an erase: %v", name, err)
		}
	}
	r.drain()
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE id = ?`,
		said.Message.ID); got != 1 {
		t.Fatalf("a refused erase removed the message anyway")
	}

	if _, err := r.store.Erase(t.Context(), viaToken("jane"), statelog.NewOpID(time.Now(), ""), room.Channel.ID,
		[]string{said.Message.ID}, "leaked"); err != nil {
		t.Fatalf("the person's erase: %v", err)
	}
	r.drain()
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE id = ?`,
		said.Message.ID); got != 0 {
		t.Fatalf("the person's erase left the row in place")
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
// The record states exactly what it removes — so an id from another room
// would be answered `applied`, and audited as an erase of that message, for a
// redaction that did not happen.
func TestAnEraseNamingAnotherRoomsMessageIsRefused(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	launch := r.room(person("jane"), "launch")
	other := r.room(person("jane"), "random")
	said := r.say(person("jane"), other.Channel.ID, "unrelated", "k-1")

	_, err := r.store.Erase(t.Context(), viaToken("jane"), statelog.NewOpID(time.Now(), ""), launch.Channel.ID,
		[]string{said.Message.ID}, "wrong room")
	if !errors.Is(err, chatlet.ErrNotFound) {
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

	if _, err := r.store.Post(t.Context(), keyed(person("jane"), requested("k-1")), room.Channel.ID,
		chatlet.NewMessage{
			Body:     "@eng @sarah can you look?",
			Mentions: []string{"eng", "sarah"}}); err != nil {
		t.Fatalf("post: %v", err)
	}
	notify := r.lastRecord().Notify
	if notify == nil {
		t.Fatal("a message anybody can answer carried no routing snapshot")
	}
	if got := chatlet.RecipientsOf(chatlet.Candidates(notify)); len(got) != 1 ||
		got[0].Handle != "eng" || got[0].Reason != chatlet.ReasonMention {
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

// A UNIT ROOM'S ANSWERER IS ITS LEAD, RESOLVED AT THE WRITE AND CARRIED ON THE
// RECORD.
//
// The routing arithmetic is never handed a roster to look an answerer up in:
// the node that derives a wake from a record is rarely the node that wrote it,
// so an answerer resolved at READ time would be whoever leads the unit when
// the record is read rather than who led it when somebody spoke. The decide
// asks the roster ONCE, for the room and its unit, and copies the answer onto
// the record. This is the only path in the engine that resolves one.
//
// The answerer it feeds is the narrow one: a PERSON spoke to a room and the
// arithmetic reached nobody else at all. And the roster is asked about the
// ROOM as well as its unit, since whose a unit room is belongs to the chart;
// a roster that answered for the unit alone is held to it by
// [TestARoomNoUnitAnswersForWakesNoLead].
func TestAUnitRoomsAnswererIsItsLeadResolvedAtTheWrite(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane").withLead("eng", "engineering", "amir"))
	room := r.unitRoom("engineering", "eng")

	if _, err := r.store.Post(t.Context(), keyed(person("jane"), requested("k-1")), room.Channel.ID,
		chatlet.NewMessage{
			Body: "who is picking this up?"}); err != nil {
		t.Fatalf("post: %v", err)
	}
	notify := r.lastRecord().Notify
	if notify == nil {
		t.Fatal("a person's post in a unit room carried no routing snapshot")
	}
	if notify.Answerer != "amir" {
		t.Fatalf("the record carries answerer %q, want \"amir\": resolved at "+
			"the write, a lead is who led the unit when somebody spoke; resolved "+
			"later it is whoever leads it now", notify.Answerer)
	}
	if got := chatlet.RecipientsOf(chatlet.Candidates(notify)); len(got) != 1 ||
		got[0].Handle != "amir" || got[0].Reason != chatlet.ReasonAnswerer {
		t.Fatalf("the record wakes %v, want amir alone as the room's answerer "+
			"(nobody else in this room was listening, and a unit nobody "+
			"answers is the case the answerer exists for)", got)
	}
}

// A ROOM NO UNIT ANSWERS FOR WAKES NO LEAD.
//
// A unit that stops declaring its room — or leaves the chart — leaves a room
// the chart staffs with nobody, and the roster says so by answering no lead
// for it. Its unit's lead must not go on being woken, and obliged to answer,
// from a room no prompt names as theirs: the record carries no answerer and
// routes nobody. The CONTROL is the same post in the unit's other room, which
// the roster still names a lead for — a write path that asked about the unit
// alone answers both or neither.
func TestARoomNoUnitAnswersForWakesNoLead(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane").withLead("eng", "engineering", "amir"))
	released := r.unitRoom("old-eng", "eng")
	current := r.unitRoom("engineering", "eng")
	for _, tc := range []struct {
		room *chatlet.Written
		lead string
	}{
		{&released, ""},
		{&current, "amir"},
	} {
		if _, err := r.store.Post(t.Context(), keyed(person("jane"), requested("k-"+tc.room.Channel.Name)), tc.room.Channel.ID,
			chatlet.NewMessage{
				Body: "anyone?"}); err != nil {
			t.Fatalf("post in #%s: %v", tc.room.Channel.Name, err)
		}
		notify := r.lastRecord().Notify
		if notify == nil {
			t.Fatalf("a person's post in #%s carried no routing snapshot",
				tc.room.Channel.Name)
		}
		var answered []string
		for _, c := range chatlet.RecipientsOf(chatlet.Candidates(notify)) {
			if c.Reason == chatlet.ReasonAnswerer {
				answered = append(answered, c.Handle)
			}
		}
		want := []string(nil)
		if tc.lead != "" {
			want = []string{tc.lead}
		}
		if notify.Answerer != tc.lead || !slices.Equal(answered, want) {
			t.Errorf("#%s carries answerer %q and wakes %v as it, want %q and %v",
				tc.room.Channel.Name, notify.Answerer, answered, tc.lead, want)
		}
	}
}

// A PERSON IS NEVER ANSWERED BY SILENCE IN A ROOM MADE BY HAND.
//
// The failure this holds shut: a person posted in a public room six agents
// had joined, none of them following every message, named nobody, and the
// routing woke nobody at all, with nothing to say so. A room nobody's chart
// names still has a seat responsible for it: its most senior agent member,
// resolved by the roster at the write and carried on the record like a unit's
// lead.
//
// EACH CASE IS ALSO A WRITER AND PARSER AGREEMENT. The record carries the
// routed set and the answerer, and the parser re-routes from the record
// through the same arithmetic; with the roster unmoved, the two must name the
// same seats for the same reasons.
func TestAHandMadeRoomIsAnsweredByItsMostSeniorAgentMember(t *testing.T) {
	t.Parallel()
	roster := agents().withPeople("jane", "bob").withRank("ceo", "cto", "amir", "dev")
	r := newWriteRound(t, roster)
	open := r.room(person("jane"), "engineering", "dev", "amir", "cto", "bob")
	closed, err := r.store.CreateChannel(t.Context(), person("jane"), chatlet.NewChannel{
		Name: "leads", Kind: chatlet.KindPrivate, Members: members("dev", "amir"),
	})
	if err != nil {
		t.Fatalf("create the private room: %v", err)
	}
	r.drain()
	people := r.room(person("jane"), "people", "bob")

	for _, tc := range []struct {
		name     string
		actor    chatlet.Actor
		room     chatlet.Written
		answerer string
	}{
		// The most senior of the room's agents: ceo ranks higher but
		// is not in the room, and the people in it run no turn.
		{"a public room", person("jane"), open, "cto"},
		{"a private room, answered by a member who can read it", person("jane"), closed, "amir"},
		{"a room with no agent member", person("jane"), people, ""},
		// A COMPANY MUST NOT TALK TO ITSELF: an agent's post that
		// reached nobody has reached exactly who it should.
		{"an agent's own post", seat("dev", "run-1"), open, ""},
	} {
		r.say(tc.actor, tc.room.Channel.ID, "where are we on the release?",
			"k-"+tc.name)
		notify := r.lastRecord().Notify
		if notify == nil {
			t.Fatalf("%s: the post carried no routing snapshot", tc.name)
		}
		if notify.Answerer != tc.answerer {
			t.Errorf("%s: the record carries answerer %q, want %q", tc.name,
				notify.Answerer, tc.answerer)
		}
		want := []chatlet.Recipient(nil)
		if tc.answerer != "" {
			want = []chatlet.Recipient{{Handle: tc.answerer, Reason: chatlet.ReasonAnswerer}}
		}
		if !slices.Equal(notify.Recipients, want) {
			t.Errorf("%s: the post wakes %v, want %v", tc.name, notify.Recipients, want)
		}
		reRouted := chatlet.RecipientsOf(chatlet.Route(chatlet.Candidates(notify),
			roster.Seat, tc.actor.Name()))
		if !slices.Equal(reRouted, notify.Recipients) {
			t.Errorf("%s: the writer routed %v and the parser re-routed %v from "+
				"the record it wrote", tc.name, notify.Recipients, reRouted)
		}
	}
}

// A PERSON'S `@channel` ASKS EVERY AGENT IN THE ROOM, AT THE WRITE.
//
// The address is a request when a person writes it and news when an agent
// does, and the record carries the reason each recipient was routed under, so
// the verdict every later reader takes from it is fixed here. The people in
// the room are addressable and never woken, and the author is never woken by
// their own message.
func TestAPersonsChannelAddressAsksEveryAgentAtTheWrite(t *testing.T) {
	t.Parallel()
	roster := agents().withPeople("jane", "bob").withRank("cto", "amir", "dev")
	r := newWriteRound(t, roster)
	room := r.room(person("jane"), "engineering", "dev", "amir", "cto", "bob")

	for _, tc := range []struct {
		actor  chatlet.Actor
		reason chatlet.Reason
		woken  []string
	}{
		{person("jane"), chatlet.ReasonCollectiveRequest, []string{"amir", "cto", "dev"}},
		{seat("dev", "run-1"), chatlet.ReasonCollective, []string{"amir", "cto"}},
	} {
		if _, err := r.store.Post(t.Context(), keyed(tc.actor, requested("k-"+tc.actor.Handle)),
			room.Channel.ID, chatlet.NewMessage{
				Body: "@channel status by end of day, please", Collective: true,
			}); err != nil {
			t.Fatalf("post as %s: %v", tc.actor.Handle, err)
		}
		r.drain()
		notify := r.lastRecord().Notify
		var got []string
		for _, c := range notify.Recipients {
			if c.Reason != tc.reason {
				t.Errorf("%s's `@channel` woke %s under %q, want %q",
					tc.actor.Handle, c.Handle, c.Reason, tc.reason)
			}
			got = append(got, c.Handle)
		}
		slices.Sort(got)
		if !slices.Equal(got, tc.woken) {
			t.Errorf("%s's `@channel` woke %v, want %v", tc.actor.Handle, got, tc.woken)
		}
	}
}

// THE ENGINE IS NOT IN THE UNIT ROOM IT CREATES.
//
// A unit's room is made by the org chart under the system actor's name, and
// a room's author is otherwise always a founding member — so every unit room
// held `org-chart` as a member: a handle that is no seat, listed among the
// room's participants until the next apply's membership write stated the
// chart's set and took it back out. A person or a seat that makes a room is
// still in it.
func TestTheEngineIsNotInTheUnitRoomItCreates(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	engine := chatlet.Actor{Handle: "org-chart", Kind: chatlet.AuthorSystem}
	unit, err := r.store.CreateChannel(t.Context(), engine, chatlet.NewChannel{
		Name: "engineering", Kind: chatlet.KindUnit, Unit: "eng",
		Members: members("dana", "erin"), ActivatedAt: chartActivation,
	})
	if err != nil {
		t.Fatalf("create the unit room: %v", err)
	}
	r.drain()
	inRoom := func(room, handle string) bool {
		t.Helper()
		return r.count(`SELECT COUNT(*) FROM chat_members
			WHERE channel_id = ? AND handle = ?`, room, handle) == 1
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_members WHERE channel_id = ?`,
		unit.Channel.ID); got != 2 || inRoom(unit.Channel.ID, "org-chart") {
		t.Fatalf("the unit room holds %d members, the engine among them: %v — "+
			"want the chart's two", got, inRoom(unit.Channel.ID, "org-chart"))
	}

	made := r.room(person("jane"), "launch", "dana")
	if !inRoom(made.Channel.ID, "jane") {
		t.Fatal("a person who made a room is not in it")
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
		string(chatlet.KindDM)); got != 1 {
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
	if !errors.Is(err, chatlet.ErrForbidden) {
		t.Fatalf("a conversation's participants were patched: %v", err)
	}
	// SOMEBODY OUTSIDE IT IS TOLD IT IS NOT THERE, as a reader is: its id
	// is derived from the two people in it, so "you may not" would say they
	// talk.
	if _, err := r.store.Join(t.Context(), person("bob"), opened.Channel.ID); !errors.
		Is(err, chatlet.ErrNotFound) {
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
		room.Channel.ID, chatlet.ChannelPatch{Archived: &archived}, chatlet.ChannelAuthority{Archive: true}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	r.drain()

	_, err := r.store.Post(t.Context(), keyed(person("jane"), requested("k-2")), room.Channel.ID,
		chatlet.NewMessage{Body: "anybody there?"})
	if !errors.Is(err, chatlet.ErrArchived) {
		t.Fatalf("an archived room took a new message: %v", err)
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE channel_id = ? AND event = ''`,
		room.Channel.ID); got != 1 {
		t.Fatalf("the archived room holds %d messages, want the one said "+
			"before it closed", got)
	}
}

// AN ARCHIVE CLOSES A ROOM TO NEW MESSAGES AND TO NOTHING ELSE: what was
// already said stays its author's to correct or take back and an operator's
// to take down, and a reaction is still taken. Freezing them would make
// moderating a closed room an un-archive, which reopens it to every post.
func TestAnArchivedRoomStillTakesChangesToWhatWasSaid(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	room := r.room(person("jane"), "launch", "bob")
	mine := r.say(person("jane"), room.Channel.ID, "shipping fridya", "k-1")
	bobs := r.say(person("bob"), room.Channel.ID, "the key is hunter2", "k-2")

	archived := true
	if _, err := r.store.PatchChannel(t.Context(), person("jane"),
		room.Channel.ID, chatlet.ChannelPatch{Archived: &archived}, chatlet.ChannelAuthority{Archive: true}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	r.drain()

	if _, err := r.store.Reply(t.Context(), keyed(person("jane"), requested("k-3")), room.Channel.ID,
		mine.Message.ID, chatlet.NewMessage{Body: "and friday it is"},
	); !errors.Is(err, chatlet.ErrArchived) {
		t.Fatalf("an archived room took a reply: %v", err)
	}
	if _, err := r.store.Edit(t.Context(), keyed(person("jane"), requested("k-4")), room.Channel.ID,
		mine.Message.ID, chatlet.EditMessage{Body: "shipping friday"},
	); err != nil {
		t.Fatalf("an author could not correct what she said before the archive: %v", err)
	}
	if _, err := r.store.React(t.Context(), keyed(person("bob"), requested("k-5")), room.Channel.ID,
		mine.Message.ID, ":tada:"); err != nil {
		t.Fatalf("an archived room refused a reaction: %v", err)
	}
	if _, err := r.store.Delete(t.Context(), keyed(viaToken("ops"), requested("k-6")), room.Channel.ID,
		bobs.Message.ID, chatlet.MessageAuthority{Moderate: true}); err != nil {
		t.Fatalf("a moderator could not take a leaked key out of an archived room "+
			"without reopening it: %v", err)
	}
	r.drain()
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE id = ? AND body = ? `+
		`AND edited_at IS NOT NULL`, mine.Message.ID, "shipping friday"); got != 1 {
		t.Fatal("the edit did not land")
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_reactions WHERE message_id = ?`,
		mine.Message.ID); got != 1 {
		t.Fatal("the reaction did not land")
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE id = ? AND `+
		`deleted_at IS NOT NULL`, bobs.Message.ID); got != 1 {
		t.Fatal("the delete did not land")
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_channels WHERE id = ? AND `+
		`archived_at IS NOT NULL`, room.Channel.ID); got != 1 {
		t.Fatal("a change to what was said reopened the room")
	}
}

// A PATCH THAT CHANGES NOTHING PUBLISHES NOTHING AND IS STILL A SUCCESS.
//
// "Nothing changed" is an answer a caller acts on, and the room it did not
// change is what that caller reads out of it. The alternative — a record and
// a live frame for a change nobody made — is what an empty patch is refused
// for one step earlier.
func TestANoOpPatchAnswersAppliedWithTheRoomItDidNotChange(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	room := r.room(person("jane"), "launch")
	topic := "shipping on friday"
	if _, err := r.store.PatchChannel(t.Context(), person("jane"),
		room.Channel.ID, chatlet.ChannelPatch{Topic: &topic}, chatlet.ChannelAuthority{}); err != nil {
		t.Fatalf("set the topic: %v", err)
	}
	r.drain()
	before := r.logEnd()

	got, err := r.store.PatchChannel(t.Context(), person("jane"),
		room.Channel.ID, chatlet.ChannelPatch{Topic: &topic}, chatlet.ChannelAuthority{})
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
	if after := r.logEnd(); after != before {
		t.Errorf("a patch that changed nothing published %d record(s)", after-before)
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

	_, err := r.store.CreateChannel(t.Context(), person("bob"), chatlet.NewChannel{
		Name: "  LAUNCH ", Kind: chatlet.KindPublic,
	})
	if !errors.Is(err, chatlet.ErrNameTaken) {
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
	if err := r.db.Tx(t.Context(), func(tx *sql.Tx) error {
		for i := range chatlet.MaxChannels - 1 {
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
		chatlet.NewChannel{Name: "the-last-one", Kind: chatlet.KindPublic}); err != nil {
		t.Fatalf("the room at the cap was refused: %v — the cap is the number "+
			"of rooms a company may HOLD, not the number below it", err)
	}
	r.drain()

	_, err := r.store.CreateChannel(t.Context(), person("jane"),
		chatlet.NewChannel{Name: "one-too-many", Kind: chatlet.KindPublic})
	if err == nil || !strings.Contains(err.Error(), fmt.Sprint(chatlet.MaxChannels)) {
		t.Fatalf("the room above the cap was answered with %v — a refusal that "+
			"does not name the number leaves a founder guessing which of their "+
			"rooms to archive", err)
	}
}

// A CREDENTIAL ACTING AS ITSELF SPEAKS IN NO ROOM, AND THE REFUSAL SAYS WHAT
// TO DO — while what is not said in a room's voice stays open to it.
//
// A room renders a speaker by SEAT: a person reads, is mentioned and is woken
// as the human seat they hold, so a line authored by a credential's login is a
// speaker nobody can reply to, mention or find in the membership. Every gesture
// said in a room refuses one, naming the remedy — a seat to act as. An erase, a
// prune and a room's retention are not said in a room, and an admin credential
// acting as itself makes them under its own login.
func TestACredentialActingAsItselfSpeaksInNoRoom(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	room := r.room(person("jane"), "launch").Channel.ID
	said := r.say(person("jane"), room, "the key is hunter2", "k-said").Message.ID
	ci := credential("token:ci")
	topic := "the daily"
	for name, write := range map[string]func() error{
		"a post": func() error {
			_, err := r.store.Post(t.Context(), ci, room, chatlet.NewMessage{Body: "hello"})
			return err
		},
		"a reply": func() error {
			_, err := r.store.Reply(t.Context(), ci, room, said,
				chatlet.NewMessage{Body: "hello"})
			return err
		},
		"a reaction": func() error {
			_, err := r.store.React(t.Context(), ci, room, said, ":eyes:")
			return err
		},
		"a join": func() error {
			_, err := r.store.Join(t.Context(), ci, room)
			return err
		},
		"a patch": func() error {
			_, err := r.store.PatchChannel(t.Context(), ci, room,
				chatlet.ChannelPatch{Topic: &topic}, chatlet.ChannelAuthority{})
			return err
		},
		"a create": func() error {
			_, err := r.store.CreateChannel(t.Context(), ci,
				chatlet.NewChannel{Name: "pipelines", Kind: chatlet.KindPublic})
			return err
		},
		"a direct conversation": func() error {
			_, _, err := r.store.OpenDirect(t.Context(), ci, []string{"jane"})
			return err
		},
		"a moderator's delete": func() error {
			_, err := r.store.Delete(t.Context(), ci, room, said,
				chatlet.MessageAuthority{Moderate: true})
			return err
		},
	} {
		err := write()
		if !errors.Is(err, chatlet.ErrForbidden) {
			t.Fatalf("%s by a credential acting as itself answered %v, want "+
				"ErrForbidden", name, err)
		}
		if !strings.Contains(chatlet.Sentence(err), "seat") {
			t.Fatalf("%s's refusal reads %q and says nothing about the seat to "+
				"act as", name, chatlet.Sentence(err))
		}
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE event = ''`); got != 1 {
		t.Fatalf("a credential's refused gestures left %d messages, want jane's one", got)
	}

	// NOBODY AT ALL IS REFUSED NAMING THE FIELD, whatever the kind.
	if _, err := r.store.Post(t.Context(), chatlet.Actor{Kind: chatlet.AuthorHuman},
		room, chatlet.NewMessage{Body: "hello"}); !errors.Is(err, chatlet.ErrInvalid) ||
		!strings.Contains(err.Error(), "actor.handle") {
		t.Fatalf("a write naming nobody answered %v, want a refusal of "+
			"actor.handle", err)
	}

	// WHAT IS NOT SAID IN A ROOM STAYS OPEN TO IT.
	days := 30
	if _, err := r.store.SetRetention(t.Context(), ci, room, &days); err != nil {
		t.Fatalf("an admin credential could not set a room's retention: %v", err)
	}
	r.drain()
	if _, err := r.store.Erase(t.Context(), ci, statelog.NewOpID(time.Now(), ""),
		room, []string{said}, "a credential in a public room"); err != nil {
		t.Fatalf("an admin credential could not erase what was pasted: %v", err)
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
		room.Channel.ID, root.Message.ID, chatlet.NewMessage{Body: "yes"})
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	r.drain()
	second, err := r.store.Reply(t.Context(), keyed(person("bob"), requested("k-2")), room.Channel.ID,
		first.Message.ID, chatlet.NewMessage{Body: "friday?"})
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
	if got := chatlet.RecipientsOf(chatlet.Candidates(notify)); len(got) != 1 ||
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

// AN EDIT THAT NAMES A COLLEAGUE BRINGS THEM THE ANSWER.
//
// Being named subscribes you to the thread — that is the whole producer of a
// follow, so somebody named in a question hears its answer. An edit that names
// a colleague the words before it did not wakes them for the question
// ([chatlet.Store.Edit]), and it has to subscribe them by the same rule: without
// the row, the seat it asked heard the question and then nothing of what
// anybody said back to it.
func TestAnEditThatNamesAColleagueSubscribesThemToTheThread(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	room := r.room(person("jane"), "launch", "eng", "ops")
	root := r.say(person("jane"), room.Channel.ID, "who owns the deploy?", "k-1")

	if _, err := r.store.Edit(t.Context(), keyed(person("jane"), requested("k-2")), room.Channel.ID,
		root.Message.ID, chatlet.EditMessage{
			Body: "@eng who owns the deploy?", Mentions: []string{"eng"}}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if got := chatlet.RecipientsOf(chatlet.Candidates(r.lastRecord().Notify)); len(got) != 1 ||
		got[0].Handle != "eng" || got[0].Reason != chatlet.ReasonMention {
		t.Fatalf("the edit wakes %v, want eng under a mention", got)
	}
	r.drain()

	if _, err := r.store.Reply(t.Context(), seat("ops", "turn-1"),
		room.Channel.ID, root.Message.ID,
		chatlet.NewMessage{Body: "the platform team"}); err != nil {
		t.Fatalf("reply: %v", err)
	}
	notify := r.lastRecord().Notify
	if notify == nil {
		t.Fatal("a reply carried no routing snapshot")
	}
	if got := chatlet.RecipientsOf(chatlet.Candidates(notify)); len(got) != 1 ||
		got[0].Handle != "eng" || got[0].Reason != chatlet.ReasonFollow {
		t.Fatalf("the answer wakes %v, want eng as a follower of the thread "+
			"the edit named it in", got)
	}
}

// A NAME TAKEN BACK STOPS THE THREAD WAKING WHOEVER IT NAMED — unless the
// thread still names them.
//
// A follow is derived from the thread's mentions: being named subscribes you,
// so the subscription lasts exactly as long as something in the thread names
// you. Every gesture that withdraws a name has to take it back — an edit that
// drops the name, a delete, an erase — or a colleague named by mistake and
// corrected away a minute later is woken by every reply the thread ever gets,
// each its own launch in a box, with no unfollow in this build's
// vocabulary to stop it. And a follow is the THREAD's, not the
// message's, so a seat the root or another reply still names keeps it.
func TestANameTakenBackStopsTheThreadWakingWhomItNamed(t *testing.T) {
	t.Parallel()
	takebacks := []struct {
		name string
		take func(r *writeRound, room, message string) error
	}{
		{"an edit that drops the name", func(r *writeRound, room, message string) error {
			_, err := r.store.Edit(t.Context(), keyed(person("jane"), requested("k-take")), room, message,
				chatlet.EditMessage{Body: "can you answer?"})
			return err
		}},
		{"a delete", func(r *writeRound, room, message string) error {
			_, err := r.store.Delete(t.Context(), keyed(person("jane"), requested("k-take")), room, message, chatlet.MessageAuthority{})
			return err
		}},
		{"an erase", func(r *writeRound, room, message string) error {
			_, err := r.store.Erase(t.Context(), viaToken("jane"),
				statelog.NewOpID(time.Now(), ""), room, []string{message},
				"named the wrong team")
			return err
		}},
	}
	// THE NAME ARRIVES EITHER WAY a follow is written — with the reply, or by
	// an edit of it adding a name the words before it did not have — and the
	// thread names the seat NOWHERE ELSE, or still names it in its ROOT or in
	// ANOTHER REPLY, each of which the derivation reads separately.
	cases := []struct {
		name   string
		byEdit bool
		still  string
	}{
		{"named when posted, the thread naming nobody else", false, ""},
		{"named by an edit, the thread naming nobody else", true, ""},
		{"named when posted, the root still naming them", false, "root"},
		{"named when posted, another reply still naming them", false, "reply"},
	}
	for _, take := range takebacks {
		for _, c := range cases {
			t.Run(take.name+", "+c.name, func(t *testing.T) {
				t.Parallel()
				takeBack(t, take.take, take.name, c.byEdit, c.still)
			})
		}
	}
}

// takeBack is one case of [TestANameTakenBackStopsTheThreadWakingWhomItNamed]:
// jane asks a question in #launch, a reply of hers names eng, `take` takes that
// reply's name back, and ops answers in the thread. `still` is where else the
// thread names eng: nowhere (""), its "root", or another "reply".
func takeBack(t *testing.T, take func(r *writeRound, room, message string) error,
	gesture string, byEdit bool, still string) {

	t.Helper()
	r := newWriteRound(t, agents().withPeople("jane"))
	room := r.room(person("jane"), "launch", "eng", "ops")
	question := chatlet.NewMessage{Body: "who owns the deploy?"}
	if still == "root" {
		question.Body = "@eng who owns the deploy?"
		question.Mentions = []string{"eng"}
	}
	root, err := r.store.Post(t.Context(), keyed(person("jane"), requested("k-1")),
		room.Channel.ID, question)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	r.drain()
	if still == "reply" {
		if _, err := r.store.Reply(t.Context(), keyed(person("jane"), requested("k-sibling")), room.Channel.ID,
			root.Message.ID, chatlet.NewMessage{
				Body: "@eng this one is yours", Mentions: []string{"eng"}}); err != nil {
			t.Fatalf("the other reply naming eng: %v", err)
		}
		r.drain()
	}
	ask := chatlet.NewMessage{Body: "@eng can you answer?",
		Mentions: []string{"eng"}}
	if byEdit {
		ask = chatlet.NewMessage{Body: "can you answer?"}
	}
	named, err := r.store.Reply(t.Context(), keyed(person("jane"), requested("k-2")),
		room.Channel.ID, root.Message.ID, ask)
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	r.drain()
	if byEdit {
		if _, err := r.store.Edit(t.Context(), keyed(person("jane"), requested("k-3")), room.Channel.ID,
			named.Message.ID, chatlet.EditMessage{
				Body: "@eng can you answer?", Mentions: []string{"eng"}}); err != nil {
			t.Fatalf("the edit naming eng: %v", err)
		}
		r.drain()
	}

	if err := take(r, room.Channel.ID, named.Message.ID); err != nil {
		t.Fatalf("%s: %v", gesture, err)
	}
	r.drain()

	if _, err := r.store.Reply(t.Context(), seat("ops", "turn-1"),
		room.Channel.ID, root.Message.ID,
		chatlet.NewMessage{Body: "the platform team"}); err != nil {
		t.Fatalf("the answer: %v", err)
	}
	notify := r.lastRecord().Notify
	if notify == nil {
		t.Fatal("a reply carried no routing snapshot")
	}
	got := chatlet.Reason("")
	for _, recipient := range chatlet.RecipientsOf(chatlet.Candidates(notify)) {
		if recipient.Handle == "eng" {
			got = recipient.Reason
		}
	}
	switch {
	case still != "" && got != chatlet.ReasonFollow:
		t.Fatalf("the answer wakes eng as %q, want %q — the thread's %s "+
			"still names eng, and a follow is the thread's rather than the "+
			"message's", got, chatlet.ReasonFollow, still)
	case still == "" && got != "":
		t.Fatalf("the answer wakes eng as %q after %s took back the only "+
			"message in the thread naming it", got, gesture)
	}
}

// A ROOM OF A KIND THIS BUILD DOES NOT KNOW REFUSES EVERY WRITE, which is the
// same answer [chatlet.Visible] gives a reader and the one the two halves have to
// agree on.
//
// This is the ROLLING-UPGRADE room, and it is reachable rather than
// theoretical: [chatlet.DecodeMutation]'s decoder deliberately does not validate,
// because an applier reading off the log must apply what the fleet accepted —
// so a newer node's create lands in this node's rows with a kind this build
// cannot classify. `privateRoom` is a two-valued question asked of an open
// enum, so such a room falls out of it as "not private", and an authorization
// that defaults to yes when the rule is unknown is the one direction of this
// that cannot be walked back: the read half refuses to serve the transcript
// while the write half accepts posts into it.
//
// THE ROOM IS BUILT FROM A RAW RECORD rather than through the store, because
// the store is exactly what cannot mint one: [chatlet.Store.CreateChannel]
// refuses a kind that is not `Named` and `OpenDirect` one that is not
// `Direct`. A record on the log is the only way in, which is the whole point.
func TestARoomOfAnUnknownKindRefusesEveryWrite(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))

	const id = "0bb6f0aa-7e42-4c7e-9f1e-2c6a1d5b3e70"
	const kind = chatlet.Kind("guild")
	if kind.Valid() {
		t.Fatalf("%q became a kind this build knows — pick one it does not, "+
			"or this case certifies nothing", kind)
	}
	r.peerRecord(chatlet.MutationRecord{
		RecordEnvelope: chatlet.RecordEnvelope{
			V: chatlet.RecordVersion, OpID: "5e2c1b90-0000-7000-8000-00000000c001",
			Subject: chatlet.ChannelSubject(id), Op: chatlet.OpCreate,
			CreatedAt: chatAt, Writer: "node-b",
			Scope: chatlet.ScopeSet{Subject: true},
		},
		Actor: "jane", ActorKind: chatlet.AuthorHuman,
		Mutation: mustJSON(t, chatlet.ChannelCreate{
			V: chatlet.DocumentVersion, ChannelID: id, Kind: kind, Name: "guild-hall",
			Members:   []chatlet.Member{{Handle: "jane"}},
			CreatedBy: "jane", CreatedByKind: chatlet.AuthorHuman,
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
	//
	// AND IT IS THIS NODE'S REFUSAL, NOT A PERMISSION: ErrUnknownKind and
	// never ErrForbidden, because the remedy is this node's upgrade, after
	// which the same call lands — a caller told "forbidden" stops asking.
	_, err := r.store.Post(t.Context(), keyed(person("jane"), requested("k-1")), id,
		chatlet.NewMessage{Body: "hello?"})
	if !errors.Is(err, chatlet.ErrUnknownKind) || errors.Is(err, chatlet.ErrForbidden) {
		t.Fatalf("a post into a room of an unknown kind: %v", err)
	}
	if !strings.Contains(err.Error(), string(kind)) {
		t.Fatalf("the refusal does not name the kind it could not read: %v", err)
	}
	if !strings.Contains(chatlet.Sentence(err), "Upgrade this node") ||
		strings.HasPrefix(chatlet.Sentence(err), "chatlet:") {

		t.Fatalf("the refusal reads to a person as %q", chatlet.Sentence(err))
	}
	// AND IT NEVER SAYS THE ROOM'S NAME: this node cannot tell who may read
	// the room, so it cannot tell whether the caller may know what it is
	// called either.
	if said := chatlet.Sentence(err); strings.Contains(said, "guild-hall") ||
		!strings.Contains(said, "this room") {
		t.Fatalf("the refusal names a room whose readers this node cannot state: %q", said)
	}
	if _, err := r.store.Join(t.Context(), person("jane"), id); !errors.Is(
		err, chatlet.ErrUnknownKind) {
		t.Fatalf("a join of a room of an unknown kind: %v", err)
	}
	if _, err := r.store.PatchChannel(t.Context(), person("jane"), id,
		chatlet.ChannelPatch{V: chatlet.DocumentVersion, Topic: strptr("ours now")},
		chatlet.ChannelAuthority{},
	); !errors.Is(err, chatlet.ErrUnknownKind) {
		t.Fatalf("a patch of a room of an unknown kind: %v", err)
	}
	r.drain()
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE channel_id = ? AND event = ''`,
		id); got != 0 {
		t.Fatalf("%d messages landed in a room this build cannot classify", got)
	}

	// AND A CHANGE TO WHAT WAS ALREADY SAID THERE IS REFUSED TOO. The newer
	// peer said something in its room, and this node applied it; neither its
	// author's edit or take-back nor an operator's moderation is admitted,
	// because each is a write into a room whose readership this build cannot
	// state — authorship only says the room's unknown rule once let her in.
	r.peerRecord(chatlet.MutationRecord{
		RecordEnvelope: chatlet.RecordEnvelope{
			V: chatlet.RecordVersion, OpID: "5e2c1b90-0000-7000-8000-00000000c002",
			Subject: chatlet.MessageSubject(id), Op: chatlet.OpPost,
			CreatedAt: chatAt, Writer: "node-b",
			Scope: chatlet.ScopeSet{Subject: true},
		},
		Actor: "jane", ActorKind: chatlet.AuthorHuman,
		Mutation: mustJSON(t, chatlet.MessagePost{
			V: chatlet.DocumentVersion, MessageID: "said-by-the-peer",
			Body: "the guild meets at noon", Author: "jane",
			AuthorKind: chatlet.AuthorHuman,
		}),
	})
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE id = ?`,
		"said-by-the-peer"); got != 1 {
		t.Fatalf("the peer's message did not land, so the changes below are " +
			"refused for having nothing to change")
	}
	for _, change := range []struct {
		name string
		do   func() error
	}{
		{"its author's edit", func() error {
			_, err := r.store.Edit(t.Context(), keyed(person("jane"), requested("k-e")), id, "said-by-the-peer",
				chatlet.EditMessage{Body: "the guild meets at one"})
			return err
		}},
		{"its author's delete", func() error {
			_, err := r.store.Delete(t.Context(), keyed(person("jane"), requested("k-d")), id, "said-by-the-peer", chatlet.MessageAuthority{})
			return err
		}},
		// MODERATION DOES NOT ADMIT ONE EITHER: a moderator's verdict
		// is about a message's author, and says nothing about a room
		// whose readership this build cannot state.
		{"a moderator's delete", func() error {
			_, err := r.store.Delete(t.Context(), keyed(viaToken("ops"), requested("k-o")), id, "said-by-the-peer", chatlet.MessageAuthority{Moderate: true})
			return err
		}},
		{"its author's reaction", func() error {
			_, err := r.store.React(t.Context(), keyed(person("jane"), requested("k-r")), id, "said-by-the-peer",
				":tada:")
			return err
		}},
		// AND THE THREE DESTRUCTIONS, which reach past membership and so
		// take no reachability check — the unknown-kind refusal is the
		// one guard they share with every other gesture.
		{"an admin's erase", func() error {
			_, err := r.store.Erase(t.Context(), viaToken("ops"),
				statelog.NewOpID(time.Now(), ""), id, []string{"said-by-the-peer"}, "leaked")
			return err
		}},
		{"an admin's prune", func() error {
			_, err := r.store.Prune(t.Context(), credential("ops"),
				statelog.NewOpID(time.Now(), ""), id, chatAt.Add(-time.Hour))
			return err
		}},
		{"an admin's retention", func() error {
			days := 30
			_, err := r.store.SetRetention(t.Context(), credential("ops"), id, &days)
			return err
		}},
	} {
		if err := change.do(); !errors.Is(err, chatlet.ErrUnknownKind) ||
			errors.Is(err, chatlet.ErrForbidden) {

			t.Errorf("%s in a room of an unknown kind: %v", change.name, err)
		}
	}
	r.drain()
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE id = ? AND `+
		`edited_at IS NULL AND deleted_at IS NULL`, "said-by-the-peer"); got != 1 {
		t.Fatal("a message in a room this build cannot classify was changed")
	}
}

// peerRecord publishes a record this build could not have minted and applies
// it, which is what a newer node in the fleet looks like from here.
func (r *writeRound) peerRecord(rec chatlet.MutationRecord) {
	r.t.Helper()
	body, err := chatlet.Encode(rec)
	if err != nil {
		r.t.Fatalf("encode the peer's record: %v", err)
	}
	// SIGNED, as every peer's record is: the fleet shares the log's key, and
	// an unsigned one is a record every node refuses as tampered.
	subject := topics.ChatLogSubject(string(rec.Subject.Kind), rec.Subject.ID)
	if _, _, err := r.log.Append(r.t.Context(), subject, rec.OpID, nil,
		r.signer.Seal(body)); err != nil {
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
		chatlet.NewChannel{Name: "board", Kind: chatlet.KindPrivate})
	if err != nil {
		t.Fatalf("create the private room: %v", err)
	}
	r.drain()
	room := made.Channel.ID
	said := r.say(person("jane"), room, "the number is confidential", "k-1")

	// REFUSED AS NOT THERE, which is what a reader outside it is told.
	if _, err := r.store.React(t.Context(), person("bob"), room,
		said.Message.ID, ":eyes:"); !errors.Is(err, chatlet.ErrNotFound) {
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
