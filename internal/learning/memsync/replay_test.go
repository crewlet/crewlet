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

// A SEAT'S MEMORY IS REPLAYED A TABLE AT A TIME, IN THE REGISTRY'S ORDER.
//
// The cases publish a seat through the syncer, as its holder does, and hydrate
// a store that never held it, as the node placement moves the seat to does.

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
// Every publish resends a seat's skills whole and a version once, so from the
// second on the changelog (one message per subject) holds the skill's only copy
// after its version, and a replay in the changelog's order has its foreign key
// refuse the seat on any store lacking the skill. A table at a time in the
// registry's order writes every skill before its versions: here a few rows
// apart in one pull, and with more notes between than a pull asks for, in two.
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
		{"in one pull", 0},
		{"in two pulls", fetchBatch + 44},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conn := broker(t)
			holder := openStore(t)
			seedMemory(t, holder)
			syncer := syncerOn(t, holder, conn)
			if _, err := syncer.PublishOwn(t.Context(), seat.Handle); err != nil {
				t.Fatalf("publish: %v", err)
			}
			var notes []string
			if tc.between > 0 {
				notes = writeNotes(t, holder, tc.between, 0)
			}
			if _, err := syncer.PublishOwn(t.Context(), seat.Handle); err != nil {
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
						"this case needs more than a pull's %d rows between the two",
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

// A FAILED PULL REPORTS NONE OF THE ROWS ITS TRANSACTION ROLLED BACK.
//
// A row the store refuses rolls back its whole pull's transaction, so a pull's
// rows are counted once it commits and a failed hydration reports exactly the
// rows it left in the store. The refused row is an episode carrying only its id
// (breaking the table's NOT NULL columns), in one pull behind the seat's own
// episode, and behind the diary.
//
// Mutation: count each row as it is written, inside the transaction, and this
// goes red.
func TestAFailedPullReportsNoneOfTheRowsItsTransactionRolledBack(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	holder := openStore(t)
	seedMemory(t, holder)
	if _, err := syncerOn(t, holder, conn).PublishOwn(t.Context(), seat.Handle); err != nil {
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
// Versions replay after every skill the changelog holds, so a version still
// missing its skill is one whose skill the changelog never got (a skill written
// and refined between a publish's read of the skills and of their versions, by
// a node that lost the seat before publishing again) or one the store skipped
// for another of the seat's skills under its name. Nothing reads it without its
// skill, so rather than break its foreign key and refuse the seat it is left
// out, counted on memory_rows_not_carried, the only word a row was dropped.
//
// Mutation: write a version without looking for its skill, or leave it out
// without counting it, and this goes red.
func TestAVersionWhoseSkillTheChangelogDoesNotHoldIsLeftOutNotRefused(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	holder := openStore(t)
	seedMemory(t, holder)
	if _, err := syncerOn(t, holder, conn).PublishOwn(t.Context(), seat.Handle); err != nil {
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
// A child's rows follow its parents' only where the registry lists the parent
// first, and an orphan is left out rather than refusing the seat only where its
// entry names the parent. The schema declares references, so the registry is
// held to it: every foreign key of a carried table is to a carried table, named
// on the child's entry, and listed before it.
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
// A replay writes what it reads off one table's subjects in that table's turn,
// and the turns' order writes a skill before its versions. A row whose body
// names another table is one no build publishes (a subject is built from its
// row's table) and would be written out of its turn: it is refused, as an
// undecodable row is. The row here, on the diary's subjects, is a whole diary
// note and a whole episode at once (they share `kind`), so the store would
// take it as either and only the refusal, by name, fails the hydration.
//
// Mutation: write a row without holding its body's table to the one its
// subjects name, and this goes red.
func TestARowOnAnotherTablesSubjectsIsRefused(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	holder := openStore(t)
	seedMemory(t, holder)
	if _, err := syncerOn(t, holder, conn).PublishOwn(t.Context(), seat.Handle); err != nil {
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

// pullGate holds back the first word a hydration's replay reads off its
// inbox until the case opens it, and says when the hydration first waits on
// it.
type pullGate struct {
	waiting chan struct{} // closed when the hydration first reads its inbox
	open    chan struct{} // closed by the case to let the words through
	once    sync.Once
}

// wait is the gate, as a replay's inbox meets it: until it is open the pull
// is on the wire, as a slow link or a copy behind its leader keeps it.
func (g *pullGate) wait() {
	g.once.Do(func() { close(g.waiting) })
	<-g.open
}

// A HYDRATION HOLDS NO WRITE WHILE A PULL IS ON THE WIRE.
//
// Read inside its transaction, a pull's rows would hold the store's write, and
// every other writer of the node, until they arrive: up to a stall, for a copy
// that stopped answering. A transaction's body may also run again
// ([store.DB.Tx]), with the pull's rows gone the second time. So the rows are
// staged whole before the transaction begins.
//
// Mutation: open the pull's transaction before reading its rows, and this
// goes red.
func TestAHydrationHoldsNoWriteWhileAFetchIsOnTheWire(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	holder := openStore(t)
	seedMemory(t, holder)
	if _, err := syncerOn(t, holder, conn).PublishOwn(t.Context(), seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	}

	fresh := openStore(t)
	syncer := syncerOn(t, fresh, conn)
	gate := &pullGate{waiting: make(chan struct{}), open: make(chan struct{})}
	newSpyCopies(syncer).beforeNext = gate.wait
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
		t.Fatalf("the hydration answered (%d, %v) before it read a pull's rows", h.carried, h.err)
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
		t.Fatalf("a write while the hydration's first pull was on the wire failed: %v — the "+
			"hydration held the store's write waiting on the link", wrote)
	}
	if h.err != nil || h.carried != len(tables) {
		t.Fatalf("Hydrate = (%d, %v), want all %d rows once the pull arrived", h.carried, h.err, len(tables))
	}
}

// remakingJS deletes the changelog and provisions it again from its own
// configuration, freshly stamped, once: with atBarrier, as the hydration
// publishes its barrier, so the barrier lands on the new changelog; otherwise
// the first time a replay deletes its consumer, between the first table's
// replay and the second's. Remade there, it is given what a changelog in use
// since would hold: another seat's rows until its sequences pass the lost
// one's last (the barrier's among them), then another hydration's barrier of
// the seat's.
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

// remake deletes the changelog and creates it again, empty and stamped by its
// create ([statelog.StampCreatedAt]): the recreation a node's identity check
// exists for. It answers the lost changelog's last sequence. Stamps compare at
// a microsecond, and this one is minted after the publish and hydration the
// first create preceded, so the two differ with no wait for the clock.
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

// A CHANGELOG MADE AGAIN UNDER A HYDRATION REFUSES THE SEAT.
//
// The barrier cannot see two recreations, after which a table's replay
// delivers a barrier and no row: one between the identity's read and the
// barrier, which takes the barrier itself, and one between two tables, whose
// sequences run past the barrier's to another hydration's barrier of the
// seat's. Either would report success with only the rows read before, a seat
// silently missing its memory, so the identity is read again after the last
// table and the seat refused, the rows already written reported as what it
// left in the store.
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
			if _, err := syncerOn(t, holder, conn).PublishOwn(t.Context(), seat.Handle); err != nil {
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
