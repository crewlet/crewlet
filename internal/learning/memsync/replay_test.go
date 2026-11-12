package memsync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A SEAT'S MEMORY IS REPLAYED A TABLE AT A TIME, IN THE REGISTRY'S ORDER
// (crewlet/crewlet#1051).
//
// The cases below publish a seat through the syncer, as the node holding it
// does, onto the package's broker, and hydrate a store that never held it, as
// the node placement moves the seat to does.

// sequenceOf is the stream sequence of the changelog's one message on subject.
func sequenceOf(t *testing.T, conn *nats.Conn, subject string) uint64 {
	t.Helper()
	stream, err := jsOn(t, conn).Stream(t.Context(), topics.MemoryStream)
	if err != nil {
		t.Fatalf("read the changelog: %v", err)
	}
	msg, err := stream.GetLastMsgForSubject(t.Context(), subject)
	if err != nil {
		t.Fatalf("read the changelog's message on %s: %v", subject, err)
	}
	return msg.Sequence
}

// publishOn puts row on subject, whatever the row holds, as no publish of this
// package would.
func publishOn(t *testing.T, conn *nats.Conn, subject string, row Row) {
	t.Helper()
	body, err := encode(row)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jsOn(t, conn).Publish(t.Context(), subject, body); err != nil {
		t.Fatalf("publish a %s row on %s: %v", row.Table, subject, err)
	}
}

// storeHolds is how many rows db holds across every carried table.
func storeHolds(t *testing.T, db *store.DB) int {
	t.Helper()
	held := 0
	for _, spec := range tables {
		held += countRows(t, db, spec.name)
	}
	return held
}

// A SKILL REPUBLISHED AFTER ITS VERSION HYDRATES ONTO A STORE THAT NEVER HELD
// THE SEAT.
//
// Every publish sends a seat's skills again, whole, and a skill's version
// once, so from the seat's second publish on the changelog, which keeps one
// message per subject, holds the skill's only copy after its version. One
// replay of the seat in the changelog's order wrote the version first, and on
// a store that did not hold the skill its foreign key refused the fetch and
// the seat with it: on a node that never held the seat, which is where
// placement moves it, on a satellite at every boot, and on a node that lost
// its store. Replayed a table at a time in the registry's order, every skill
// is written before its versions whatever order the changelog holds them in:
// the version and its skill a few rows apart, in one fetch of the single
// replay, and with three hundred notes between them, in two.
//
// Mutation: replay the seat in one pass in the changelog's order, or replay
// the tables out of the registry's order, and this goes red.
func TestASkillRepublishedAfterItsVersionHydratesOntoAStoreThatNeverHeldTheSeat(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// between is how many diary notes the seat writes between its two
		// publishes, which the second carries between the version's message
		// and the skill's new one.
		between int
	}{
		{"in one fetch", 0},
		{"in two fetches", 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conn := broker(t)
			holder := openStore(t)
			seedMemory(t, holder)
			syncer := syncerOn(t, holder, conn)
			if _, err := syncer.Publish(t.Context(), seat.Handle); err != nil {
				t.Fatalf("publish: %v", err)
			}
			var notes []string
			if tc.between > 0 {
				notes = writeNotes(t, holder, tc.between, 0)
			}
			if _, err := syncer.Publish(t.Context(), seat.Handle); err != nil {
				t.Fatalf("publish again: %v", err)
			}

			skill := sequenceOf(t, conn, subjectOf(registered(t, "synthesized_skills"), "s1"))
			version := sequenceOf(t, conn, subjectOf(registered(t, "synthesized_skill_versions"), "v1"))
			if skill <= version {
				t.Fatalf("the changelog holds the skill at %d and its version at %d: this case "+
					"needs the skill's only copy after its version, as every publish after the "+
					"first leaves it", skill, version)
			}
			if len(notes) > 0 {
				diary := registered(t, "agent_diary")
				first := sequenceOf(t, conn, subjectOf(diary, notes[0]))
				last := sequenceOf(t, conn, subjectOf(diary, notes[len(notes)-1]))
				if first <= version || last >= skill || len(notes) <= fetchBatch {
					t.Fatalf("the %d notes are at %d..%d, the version at %d and the skill at %d: "+
						"this case needs more than a fetch's %d rows between the two",
						len(notes), first, last, version, skill, fetchBatch)
				}
			}

			fresh := openStore(t)
			carried, err := syncerOn(t, fresh, conn).Hydrate(t.Context(), seat.Handle)
			if err != nil {
				t.Fatalf("a store that never held the seat refused it: %v", err)
			}
			if want := len(tables) + tc.between; carried != want {
				t.Errorf("the hydration carried %d rows, want %d", carried, want)
			}
			for _, spec := range tables {
				want := 1
				if spec.name == "agent_diary" {
					want += tc.between
				}
				if got := countRows(t, fresh, spec.name); got != want {
					t.Errorf("%s holds %d rows after the hydration, want %d", spec.name, got, want)
				}
			}
			var skillID string
			if err := fresh.SQL().QueryRowContext(t.Context(),
				"SELECT skill_id FROM synthesized_skill_versions WHERE id = 'v1'").Scan(&skillID); err != nil {
				t.Fatalf("the skill's version did not arrive: %v", err)
			}
			if skillID != "s1" {
				t.Errorf("the version arrived for skill %q, want s1", skillID)
			}
		})
	}
}

// A FAILED FETCH REPORTS NONE OF THE ROWS ITS TRANSACTION ROLLED BACK.
//
// A hydration writes each fetch in one transaction, so a row the store refuses
// rolls back every row of its fetch. They were counted as they were written,
// inside the transaction, so a hydration that failed reported rows it had
// rolled back: with the store holding none of the seat's rows, it said it had
// carried some. A fetch's rows are counted once its transaction commits, so a
// failed hydration reports exactly the rows it left in the store. The row the
// store refuses here is an episode carrying nothing but its id, which breaks
// the table's NOT NULL columns, behind the seat's own episode in one fetch and
// behind the diary.
//
// Mutation: count each row as it is written, inside the transaction, and this
// goes red.
func TestAFailedFetchReportsNoneOfTheRowsItsTransactionRolledBack(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	holder := openStore(t)
	seedMemory(t, holder)
	if _, err := syncerOn(t, holder, conn).Publish(t.Context(), seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	}
	refused := Row{Table: "episodes", Values: map[string]any{"id": "e9"}}
	publishOn(t, conn, registered(t, "episodes").subject(seat.AgentID, refused.Values), refused)

	fresh := openStore(t)
	carried, err := syncerOn(t, fresh, conn).Hydrate(t.Context(), seat.Handle)
	if err == nil {
		t.Fatalf("a hydration whose episode the store refuses reported success with %d rows", carried)
	}
	held := storeHolds(t, fresh)
	if carried != held {
		t.Fatalf("the failed hydration reported %d rows carried and left %d in the store: it "+
			"counted rows its transaction rolled back (%v)", carried, held, err)
	}
	if held == 0 {
		t.Fatalf("the failed hydration left no row in the store; this case needs one it "+
			"committed before the fetch the store refused (%v)", err)
	}
	if got := countRows(t, fresh, "episodes"); got != 0 {
		t.Errorf("the store holds %d episodes after the fetch that held the refused one rolled "+
			"back, want none", got)
	}
}

// A VERSION WHOSE SKILL THE CHANGELOG DOES NOT HOLD IS LEFT OUT, NOT REFUSED.
//
// A skill's versions are replayed after every skill the changelog holds, so a
// version whose skill is still not in the store is one whose skill the
// changelog does not hold at all, as here — a skill written after a publish
// read the seat's skills and refined before it read their versions, by a node
// that stopped holding the seat before a publish carried the skill — or one
// the store skipped for holding another skill of the seat's under its name.
// Written, the version broke its foreign key and refused the seat, on every
// node whose store lacked the skill, and for good once the store that held it
// was gone, over a row nothing reads without its skill. It is left out, every
// other row is carried, and the hydration's memory_rows_not_carried line
// counts it, the one word anywhere that a carried row was dropped.
//
// Mutation: write a version without looking for its skill, or leave it out
// without counting it, and this goes red.
func TestAVersionWhoseSkillTheChangelogDoesNotHoldIsLeftOutNotRefused(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	holder := openStore(t)
	seedMemory(t, holder)
	if _, err := syncerOn(t, holder, conn).Publish(t.Context(), seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	}
	stream, err := jsOn(t, conn).Stream(t.Context(), topics.MemoryStream)
	if err != nil {
		t.Fatalf("read the changelog: %v", err)
	}
	if err := stream.Purge(t.Context(), jetstream.WithPurgeSubject(
		subjectOf(registered(t, "synthesized_skills"), "s1"))); err != nil {
		t.Fatalf("take the skill off the changelog: %v", err)
	}

	fresh := openStore(t)
	syncer := syncerOn(t, fresh, conn)
	var said []notCarried
	syncer.leftOut = func(n notCarried) { said = append(said, n) }
	carried, err := syncer.Hydrate(t.Context(), seat.Handle)
	if err != nil {
		t.Fatalf("a version whose skill the changelog does not hold refused the seat: %v", err)
	}
	if len(said) != 1 || len(said[0].orphaned) != 1 || said[0].orphaned["synthesized_skill_versions"] != 1 {
		t.Errorf("the hydration said it left out %+v, want once the 1 skill version whose skill it "+
			"did not find", said)
	}
	absent := map[string]bool{"synthesized_skills": true, "synthesized_skill_versions": true}
	want := 0
	for _, spec := range tables {
		n := 1
		if absent[spec.name] {
			n = 0
		}
		want += n
		if got := countRows(t, fresh, spec.name); got != n {
			t.Errorf("%s holds %d rows after the hydration, want %d", spec.name, got, n)
		}
	}
	if carried != want {
		t.Errorf("the hydration carried %d rows, want %d: every row but the skill's version", carried, want)
	}
}

// EVERY REFERENCE BETWEEN CARRIED TABLES NAMES A PARENT REPLAYED FIRST.
//
// A hydration writes each table's rows in the table's own turn, so a child's
// rows follow every parent the changelog holds only where the registry lists
// the parent first, and a child whose parent is nowhere is left out, rather
// than refusing the seat, only where its entry names the parent. The schema is
// where a reference is declared, so the registry is held to it: every foreign
// key of a carried table is to a carried table, named on the child's entry,
// and listed before it.
//
// Mutation: list synthesized_skill_versions before synthesized_skills, or
// drop its parent, and this goes red.
func TestEveryReferenceBetweenCarriedTablesNamesAParentReplayedFirst(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	place := make(map[string]int, len(tables))
	for i, spec := range tables {
		place[spec.name] = i
	}
	references := 0
	for i, spec := range tables {
		var declared []reference
		rows, err := db.SQL().QueryContext(t.Context(),
			`SELECT "table", "from", "to" FROM pragma_foreign_key_list('`+spec.name+`')`)
		if err != nil {
			t.Fatalf("read %s's references: %v", spec.name, err)
		}
		for rows.Next() {
			var ref reference
			if err := rows.Scan(&ref.table, &ref.column, &ref.key); err != nil {
				t.Fatalf("read %s's references: %v", spec.name, err)
			}
			declared = append(declared, ref)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read %s's references: %v", spec.name, err)
		}
		_ = rows.Close()
		references += len(declared)

		switch {
		case len(declared) > 1:
			t.Errorf("%s references %d tables, and an entry names one parent", spec.name, len(declared))
		case len(declared) == 0:
			if spec.parent != nil {
				t.Errorf("%s names %s as its parent, and the schema declares no reference",
					spec.name, spec.parent.table)
			}
		default:
			ref := declared[0]
			at, carried := place[ref.table]
			switch {
			case !carried:
				t.Errorf("%s references %s, which memsync does not carry: a store that never "+
					"held the seat would refuse every row of it", spec.name, ref.table)
			case spec.parent == nil || *spec.parent != ref:
				t.Errorf("%s references %s(%s) by %s, and its entry names %+v as its parent",
					spec.name, ref.table, ref.key, ref.column, spec.parent)
			case at >= i:
				t.Errorf("%s is listed at %d, and its parent %s at %d: a hydration would write "+
					"its rows before their parents", spec.name, i, ref.table, at)
			}
		}
	}
	if references == 0 {
		t.Fatal("the schema declares no reference between memory tables, so this case holds " +
			"nothing: synthesized_skill_versions references synthesized_skills")
	}
}

// A ROW ON ANOTHER TABLE'S SUBJECTS IS REFUSED, NOT WRITTEN OUT OF ITS TURN.
//
// A replay reads one table's subjects and writes what it reads in that table's
// turn, and the turns' order is what writes a skill before its versions. A row
// whose body names another table is one no build publishes, since a row's
// subject is built from its own table's name, and written in the turn it was
// read in it would be written out of its own: it is refused, as a row that
// does not decode is. The row here, on the diary's subjects, says it is an
// episode, and holds every column either table requires — a whole diary note
// and a whole episode at once, whose `kind` both tables take — so that the
// store would take it written as either: only the refusal fails the
// hydration, and only by name.
//
// Mutation: write what a replay reads without holding each row's table to the
// replay's, as the table its subjects name or as the one its body names, and
// this goes red.
func TestARowOnAnotherTablesSubjectsIsRefused(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	holder := openStore(t)
	seedMemory(t, holder)
	if _, err := syncerOn(t, holder, conn).Publish(t.Context(), seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	}
	misrouted := Row{Table: "episodes", Values: map[string]any{
		// The diary's.
		"id": "d9", "agent_id": seat.AgentID, "kind": "diary_long",
		"content": "a note that says it is an episode", "source": "reflect", "created_at": 1,
		// The episodes'.
		"agent_handle": seat.Handle, "agent_role": "Engineer", "turn_id": "t9",
		"started_at": 1, "ended_at": 2, "plan_summary": "plan", "task_summary": "task",
		"review_outcome": "done", "duration_ms": 1,
	}}
	publishOn(t, conn, registered(t, "agent_diary").subject(seat.AgentID, misrouted.Values), misrouted)

	carried, err := syncerOn(t, openStore(t), conn).Hydrate(t.Context(), seat.Handle)
	if !errors.Is(err, errMisroutedRow) {
		t.Fatalf("a hydration that read an episode on the diary's subjects answered (%d, %v), "+
			"want the row refused for naming another table than its subjects", carried, err)
	}
}

// fetchGate holds back the rows of a hydration's first fetch until the case
// opens it, and says when the hydration first waits on them.
type fetchGate struct {
	waiting chan struct{} // closed when the hydration first reads a fetch's rows
	open    chan struct{} // closed by the case to let the rows through
	once    sync.Once
}

// gatedJS is a JetStream whose replay consumers' fetches pass through gate.
type gatedJS struct {
	jetstream.JetStream
	gate *fetchGate
}

func (j gatedJS) CreateConsumer(ctx context.Context, stream string,
	cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {

	consumer, err := j.JetStream.CreateConsumer(ctx, stream, cfg)
	if err != nil {
		return nil, err
	}
	return gatedConsumer{Consumer: consumer, gate: j.gate}, nil
}

type gatedConsumer struct {
	jetstream.Consumer
	gate *fetchGate
}

func (c gatedConsumer) Fetch(batch int, opts ...jetstream.FetchOpt) (jetstream.MessageBatch, error) {
	fetched, err := c.Consumer.Fetch(batch, opts...)
	if err != nil {
		return nil, err
	}
	return gatedBatch{MessageBatch: fetched, gate: c.gate}, nil
}

type gatedBatch struct {
	jetstream.MessageBatch
	gate *fetchGate
}

// Messages is the fetch's rows, handed over once the gate is open: until then
// the fetch is on the wire, as a slow link or a copy behind its leader keeps
// it.
func (b gatedBatch) Messages() <-chan jetstream.Msg {
	b.gate.once.Do(func() { close(b.gate.waiting) })
	<-b.gate.open
	return b.MessageBatch.Messages()
}

// A HYDRATION HOLDS NO WRITE WHILE A FETCH IS ON THE WIRE.
//
// A fetch hands its rows over as they cross the link, and a hydration that
// read them inside the fetch's transaction held this store's write, and so
// every other writer of the node, for as long as they took to arrive: the rest
// of its budget, for a fetch a copy behind its leader never answers. A
// transaction's body may also run again ([store.DB.Tx]), and a fetch's rows
// read inside it are gone the second time. The rows are read whole before the
// transaction begins, so the store takes another writer's write while the
// fetch is still on its way.
//
// Mutation: read the fetch's rows inside its transaction, and this goes red.
func TestAHydrationHoldsNoWriteWhileAFetchIsOnTheWire(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	holder := openStore(t)
	seedMemory(t, holder)
	if _, err := syncerOn(t, holder, conn).Publish(t.Context(), seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	}

	fresh := openStore(t)
	syncer := syncerOn(t, fresh, conn)
	gate := &fetchGate{waiting: make(chan struct{}), open: make(chan struct{})}
	syncer.js = gatedJS{JetStream: syncer.js, gate: gate}
	var opened sync.Once
	letThrough := func() { opened.Do(func() { close(gate.open) }) }
	defer letThrough()

	type hydrated struct {
		carried int
		err     error
	}
	done := make(chan hydrated, 1)
	go func() {
		carried, err := syncer.Hydrate(t.Context(), seat.Handle)
		done <- hydrated{carried, err}
	}()
	select {
	case <-gate.waiting:
	case h := <-done:
		t.Fatalf("the hydration answered (%d, %v) before it read a fetch's rows", h.carried, h.err)
	}

	wrote := fresh.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO agent_diary
			(id, agent_id, kind, content, source, created_at)
			VALUES ('w1', ?, 'diary_long', 'written beside the hydration', 'reflect', 1)`, otherSeat)
		return err
	})
	letThrough()
	h := <-done
	if wrote != nil {
		t.Fatalf("a write while the hydration's first fetch was on the wire failed: %v — the "+
			"hydration held the store's write waiting on the link", wrote)
	}
	if h.err != nil || h.carried != len(tables) {
		t.Fatalf("Hydrate = (%d, %v), want all %d rows once the fetch arrived", h.carried, h.err, len(tables))
	}
}

// remakingJS deletes the changelog and provisions it again from its own
// configuration, stamped anew as the queue stamps every create, once: as the
// hydration publishes its barrier, so the barrier lands on the new changelog,
// where atBarrier says so, and otherwise the first time a replay deletes its
// consumer, between the first table's replay and the second's. Made again
// there, the new changelog is given what one in use since would hold: rows of
// another seat's, until its sequences have run past the last the lost one gave,
// the barrier's among them, and a barrier of the seat's past them, another
// hydration's.
type remakingJS struct {
	jetstream.JetStream
	atBarrier bool
	once      sync.Once
	err       error
}

func (j *remakingJS) Publish(ctx context.Context, subject string, data []byte,
	opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {

	if j.atBarrier && subject == barrierSubject(seat.AgentID) {
		j.once.Do(func() { _, j.err = remake(ctx, j.JetStream) })
	}
	return j.JetStream.Publish(ctx, subject, data, opts...)
}

func (j *remakingJS) DeleteConsumer(ctx context.Context, stream, consumer string) error {
	err := j.JetStream.DeleteConsumer(ctx, stream, consumer)
	if !j.atBarrier {
		j.once.Do(func() { j.err = j.remakePastTheBarrier(ctx) })
	}
	return err
}

// remakePastTheBarrier makes the changelog again, publishes another seat's rows
// to it until its sequences reach the last the lost changelog gave, and then a
// barrier of the seat's, past every sequence the lost one gave.
func (j *remakingJS) remakePastTheBarrier(ctx context.Context) error {
	last, err := remake(ctx, j.JetStream)
	if err != nil {
		return err
	}
	for i := range last {
		subject := fmt.Sprintf("%s%s.agent_diary.%032x", topics.MemoryPrefix, otherSeat, i)
		if _, err := j.JetStream.Publish(ctx, subject, []byte(`{}`)); err != nil {
			return err
		}
	}
	_, err = j.JetStream.Publish(ctx, barrierSubject(seat.AgentID), nil)
	return err
}

// remake deletes the changelog and creates it again, empty, stamped by the
// create that makes it ([statelog.StampCreatedAt]): the recreation a node's
// identity check exists for. It answers the last sequence the lost changelog
// had given. Two stamps are compared at a microsecond, and this one is minted
// after the publish and the hydration that the first one's create preceded, so
// the two differ with no wait for the clock to move.
func remake(ctx context.Context, client jetstream.JetStream) (uint64, error) {
	lost, err := client.Stream(ctx, topics.MemoryStream)
	if err != nil {
		return 0, err
	}
	again := lost.CachedInfo().Config
	again.Metadata = statelog.StampCreatedAt(again.Metadata, time.Now())
	if err := client.DeleteStream(ctx, topics.MemoryStream); err != nil {
		return 0, err
	}
	_, err = client.CreateStream(ctx, again)
	return lost.CachedInfo().State.LastSeq, err
}

// A CHANGELOG MADE AGAIN UNDER A HYDRATION REFUSES THE SEAT, WHERE ITS BARRIER
// CANNOT SEE IT.
//
// A hydration reads the changelog's identity, publishes its barrier, and then
// reads the tables one after another across its whole budget. A changelog made
// again after the barrier is EMPTY and has lost it, so a table's replay on it
// waits for a barrier that never comes, and the seat is refused. Two
// recreations the barrier cannot see: one between the identity's reading and
// the barrier, which takes the barrier itself, so every table's replay
// delivers it and no row; and one between two tables whose sequences have run
// past the barrier's and which holds a barrier of the seat's past it, another
// hydration's, so every later table's replay delivers that and no row. Either
// way the hydration reported success with the rows read before the changelog
// was made again, or none — a seat silently missing its memory, which reading
// the identity first exists to refuse. It is read again after the last table,
// and the seat is refused, the rows already written reported as what it left
// in the store.
//
// Mutation: read the identity only before the barrier, and this goes red.
func TestAChangelogMadeAgainUnderAHydrationRefusesTheSeat(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		atBarrier bool
		// wrote is the rows the hydration wrote from the lost changelog:
		// none where it was made again before the first table, and the first
		// table's one where it was made again after it.
		wrote int
	}{
		{"between the identity and the barrier", true, 0},
		{"between two tables, past the barrier", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conn := broker(t)
			holder := openStore(t)
			seedMemory(t, holder)
			if _, err := syncerOn(t, holder, conn).Publish(t.Context(), seat.Handle); err != nil {
				t.Fatalf("publish: %v", err)
			}

			fresh := openStore(t)
			syncer := syncerOn(t, fresh, conn)
			remade := &remakingJS{JetStream: syncer.js, atBarrier: tc.atBarrier}
			syncer.js = remade
			carried, err := syncer.Hydrate(t.Context(), seat.Handle)
			if remade.err != nil {
				t.Fatalf("make the changelog again: %v", remade.err)
			}
			if !errors.Is(err, statelog.ErrStreamRecreated) {
				t.Fatalf("a hydration whose changelog was made again under it answered (%d, %v), "+
					"want the seat refused as hydrating from a recreated changelog", carried, err)
			}
			if held := storeHolds(t, fresh); carried != held || held != tc.wrote {
				t.Errorf("the refused hydration reported %d rows and left %d in the store, want "+
					"the %d it wrote before the changelog was made again", carried, held, tc.wrote)
			}
		})
	}
}
