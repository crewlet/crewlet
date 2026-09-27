package tracker_test

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE PURGE'S OWN RECORD IS NOT GATED BY THE MARKER IT WROTE.
//
// # The failure this exists to catch
//
// The deletion gate drops every commit about a purged task WHATEVER ITS
// POSITION — which is what makes the destruction permanent rather than a race
// a redelivery can undo. Its one exception is the record that wrote the
// marker, keyed on that record's own id.
//
// Without the exception, a purge whose acknowledgement was lost resolves as
// "the record was durable and applied nowhere": the caller is told the
// destruction it asked for did not happen, when it did. Keyed on the op KIND
// instead, a second purge of the same task would pass the gate too — and a
// purge is the one operation that destroys rows.
func TestAPurgeIsNotGatedByItsOwnMarker(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	purged, err := r.writer.PurgeTask(t.Context(), "op-purge", "t-1", "ENG", "spam")
	if err != nil {
		t.Fatalf("PurgeTask: %v", err)
	}
	r.drain()
	if answer := r.ask(map[string]any{"container": "project:ENG"}); len(answer.Rows) != 0 {
		t.Fatalf("the task survived its purge: %+v", answer.Rows)
	}

	gates := tracker.NewGates(r.db)
	reason, gated, err := gates.GatedAt(t.Context(),
		statelog.Subject{Kind: "task", ID: "t-1"}, "node-a", "op-purge",
		purged.Position)
	if err != nil {
		t.Fatalf("GatedAt: %v", err)
	}
	if gated {
		t.Fatalf("the purge's own record is gated as %q by the marker it "+
			"wrote — its caller would be told the destruction did not happen, "+
			"when it did", reason)
	}

	// AND EVERY OTHER RECORD ON THAT TASK IS GATED, including a second
	// purge: "any purge" as the exception would let one through, and a
	// purge is the one operation that destroys rows.
	for _, opID := range []string{"op-comment", "op-purge-again"} {
		reason, gated, err := gates.GatedAt(t.Context(),
			statelog.Subject{Kind: "task", ID: "t-1"}, "node-a", opID,
			purged.Position)
		if err != nil {
			t.Fatalf("GatedAt %s: %v", opID, err)
		}
		if !gated || reason != statelog.ReasonDeleted {
			t.Errorf("a record %q about a purged task is gated=%v as %q — the "+
				"gate is what stops a redelivery months later resurrecting "+
				"rows an operator deliberately removed", opID, gated, reason)
		}
	}
}

// A WRITE ON A PURGED TASK IS REFUSED WITH THE GATE THAT DROPPED IT.
//
// It is a refusal rather than an outcome and never a re-decide: republishing
// produces another durable record nothing applies, and the caller burns its
// whole round budget to a conflict a model reads as a colleague editing.
func TestAWriteOnAPurgedTaskIsRefusedAsDeleted(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()
	if _, err := r.writer.PurgeTask(t.Context(), "op-purge", "t-1", "ENG", ""); err != nil {
		t.Fatalf("PurgeTask: %v", err)
	}
	r.drain()

	_, err := r.writer.UpdateTask(t.Context(), "op-late", "t-1", "ENG", tracker.NoIfMatch,
		tracker.TaskPatch{Title: ptr("back from the dead")}, tracker.ChangeFields, nil)
	if err == nil {
		t.Fatal("a write on a purged task was accepted")
	}
	if !strings.Contains(err.Error(), "not on this node") &&
		!errors.Is(err, statelog.ErrUnavailable) {
		t.Fatalf("the refusal is %v, which does not tell the caller its task "+
			"is gone rather than contended", err)
	}
	if answer := r.ask(map[string]any{"container": "project:ENG"}); len(answer.Rows) != 0 {
		t.Fatalf("a purged task came back: %+v", answer.Rows)
	}
}

// A RECORD THE DELETION GATE DROPS WRITES NOTHING, ITS MARKER INCLUDED.
//
// A gate is a rule under which an accepted record produces rows on no node,
// and the framework asks it inside the apply transaction so the answer comes
// from committed state. A gate that wrote — a tally on the marker — would make
// a dropped record produce a row after all, one nothing reads: what a person
// sees of the drop is the framework's own log line, counter and alarm, the
// same for every gate.
//
// The shape is a redelivery of a patch after the purge, which is what a
// reprocess after an upgrade or a snapshot adopter replaying forward looks
// like.
//
// Mutation: make the deletion gate UPDATE the marker when it drops a record,
// and the marker row changes.
func TestARecordTheDeletionGateDropsWritesNothing(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()
	patch, err := r.writer.UpdateTask(t.Context(), "op-late", "t-1", "ENG", tracker.NoIfMatch,
		tracker.TaskPatch{Title: ptr("late")}, tracker.ChangeFields, nil)
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	r.drain()
	if _, err := r.writer.PurgeTask(t.Context(), "op-purge", "t-1", "ENG", ""); err != nil {
		t.Fatalf("PurgeTask: %v", err)
	}
	r.drain()
	before := markerRow(t, r, "t-1")
	if strings.Contains(before, "reject") {
		t.Fatalf("the deletion marker carries a tally of what its gate drops: %s",
			before)
	}

	r.redeliver(patch.Position.Seq)

	if after := markerRow(t, r, "t-1"); after != before {
		t.Fatalf("the gate dropped a record and wrote the marker:\n before %s\n after  %s",
			before, after)
	}
	if rows := r.strings(`SELECT id FROM tracker_tasks WHERE id = 't-1'`); len(rows) != 0 {
		t.Fatalf("the dropped patch wrote the purged task back: %v", rows)
	}
}

// markerRow is a task's deletion marker, every column of it, as one string —
// by `*` rather than by name, so the comparison covers whatever the table
// carries.
func markerRow(t *testing.T, r *roundTrip, id string) string {
	t.Helper()
	var row string
	if err := r.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(),
			`SELECT * FROM tracker_deletions WHERE task_id = ?`, id)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		columns, err := rows.Columns()
		if err != nil {
			return err
		}
		if !rows.Next() {
			return fmt.Errorf("task %s has no deletion marker", id)
		}
		values := make([]any, len(columns))
		for i := range values {
			values[i] = new(any)
		}
		if err := rows.Scan(values...); err != nil {
			return err
		}
		for i, v := range values {
			row += fmt.Sprintf("%s=%v ", columns[i], *(v.(*any)))
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the marker: %v", err)
	}
	return row
}

// EVERY GATE-INSTALLING RECORD IS DECLARED, AND PINNED.
//
// A node that DEFERRED a gate record would leave its own gate table empty and
// go on applying every record the evicted node appends, with no inverse that
// repairs it — so an un-decodable gate record stops that build's applier
// instead. That only works if the predicate names every one of them, and if
// every one is decodable by every build there will ever be.
func TestEveryGateRecordIsDeclared(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		envelope statelog.Envelope
		gate     bool
	}{
		"an eviction": {
			envelope: statelog.Envelope{Kind: string(tracker.KindEviction),
				Op: string(tracker.OpEviction)},
			gate: true,
		},
		"a readmission": {
			// THE INVERSE IS A GATE TOO. A node that deferred it would
			// go on dropping a readmitted peer's records for ever.
			envelope: statelog.Envelope{Kind: string(tracker.KindEviction),
				Op: string(tracker.OpEviction)},
			gate: true,
		},
		"a purge": {
			envelope: statelog.Envelope{Kind: string(tracker.KindTask),
				Op: string(tracker.OpPurge)},
			gate: true,
		},
		"an ordinary patch": {
			envelope: statelog.Envelope{Kind: string(tracker.KindTask),
				Op: string(tracker.OpPatch)},
		},
		"a barrier": {
			// NOT A GATE. It writes no row anywhere by design, so a
			// rule that dropped it would be a rule about a record with
			// nothing to drop.
			envelope: statelog.Envelope{Kind: string(tracker.KindBarrier),
				Op: string(tracker.OpBarrier)},
		},
		"a generation": {
			envelope: statelog.Envelope{Kind: string(tracker.KindGeneration),
				Op: string(tracker.OpGeneration)},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := (tracker.Domain{}).InstallsGate(tc.envelope); got != tc.gate {
				t.Fatalf("InstallsGate(%s/%s) = %v, want %v — a gate this "+
					"predicate does not name is one a node will DEFER, and a "+
					"deferred gate licenses every later record on that node",
					tc.envelope.Kind, tc.envelope.Op, got, tc.gate)
			}
		})
	}
}

// EVERY TABLE A GATE IS READ FROM IS COVERED BY THE PREDICATE.
//
// The walk is over the schema rather than over a list: a third gate table
// added without extending the predicate is a gate every node silently defers.
func TestEveryGateTableIsCoveredByThePredicate(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	var tables []string
	if err := r.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(),
			`SELECT name FROM sqlite_master WHERE type = 'table'
			 AND (name LIKE 'tracker_%evict%' OR name LIKE 'tracker_%deletion%')
			 ORDER BY name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			tables = append(tables, name)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("walk the schema: %v", err)
	}

	// EXACTLY TWO, and the number is the assertion: a third gate table is
	// a third gate, and the categories in gates.go say there are two.
	want := []string{"tracker_deletions", "tracker_evictions"}
	if strings.Join(tables, ",") != strings.Join(want, ",") {
		t.Fatalf("the schema holds gate tables %v and the predicate covers %v "+
			"— a gate table with no clause in InstallsGate is a gate every "+
			"node defers, which licenses every later record on it", tables, want)
	}
}

// A PURGE'S REASON TRAVELS WHOLE, OR NOTHING IS PURGED.
//
// The reason is the operator's account of an act with no inverse, and the line
// a purge leaves — the lead's notification, whose excerpt the activity feed
// row carries — is where it is read. No read surface returns the copy on the
// deletion marker, so a reason cut to fit that line would have no way back:
// one that does not fit is refused naming how much does, before anything is
// destroyed. With or without a lead, so what a purge accepts does not change
// when somebody is appointed.
func TestAPurgeReasonTravelsWholeOrNothingIsPurged(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"t-a", "t-b", "t-c", "t-d"} {
		filedTask(t, r, id)
	}
	r.writer.Leads = fixedLeads{project: "eng-lead"}
	operator := asOperator(r, "ops-1")

	// THE LINE WITH NO REASON ON IT is the room the reason has left,
	// less the separator it is joined by.
	if _, err := operator.PurgeTask(t.Context(), "op-a", "t-a", "ENG", ""); err != nil {
		t.Fatalf("purge t-a: %v", err)
	}
	r.drain()
	room := tracker.MaxExcerpt - len(r.lastWake().Excerpt) - len(": ")

	// EXACTLY THE ROOM, ending on a two-byte rune, lands whole.
	fits := strings.Repeat("r", room-2) + "é"
	if _, err := operator.PurgeTask(t.Context(), "op-b", "t-b", "ENG", fits); err != nil {
		t.Fatalf("a reason of exactly the %d bytes that fit was refused: %v",
			room, err)
	}
	r.drain()
	if excerpt := r.lastWake().Excerpt; !strings.HasSuffix(excerpt, ": "+fits) {
		t.Fatalf("the lead's line does not carry the whole reason: it ends %q",
			excerpt[max(0, len(excerpt)-16):])
	}

	// ONE BYTE PAST IT is refused naming the room, and the task survives.
	tooLong := "r" + fits
	for _, c := range []struct {
		id    string
		leads tracker.Leads
	}{{"t-c", fixedLeads{project: "eng-lead"}}, {"t-d", nil}} {
		r.writer.Leads = c.leads
		operator := asOperator(r, "ops-1")
		_, err := operator.PurgeTask(t.Context(), "op-"+c.id, c.id, "ENG", tooLong)
		// TYPED, because the fix is the caller's: a surface that cannot
		// tell this from a purge that failed answers an operator's long
		// sentence as a server fault.
		var refused *tracker.ErrPurgeReasonTooLong
		switch {
		case err == nil:
			t.Errorf("%s (lead %v): a %d-byte reason against %d bytes of room "+
				"was accepted — the line that carries it would cut it, and no "+
				"read surface returns the rest", c.id, c.leads != nil,
				len(tooLong), room)
		case !errors.As(err, &refused):
			t.Errorf("%s: the refusal %v is not an ErrPurgeReasonTooLong, so no "+
				"caller can tell it from a failed purge", c.id, err)
		case refused.Room != room || refused.Bytes != len(tooLong):
			t.Errorf("%s: the refusal carries %d bytes of reason against %d of "+
				"room, want %d against %d", c.id, refused.Bytes, refused.Room,
				len(tooLong), room)
		case !strings.Contains(err.Error(), fmt.Sprintf("at most %d", room)):
			t.Errorf("%s: the refusal %q does not say how much fits", c.id, err)
		}
	}
	r.drain()
	survivors := ids(r.ask(map[string]any{"container": "project:ENG"}))
	for _, id := range []string{"t-c", "t-d"} {
		if !slices.Contains(survivors, id) {
			t.Errorf("%s was purged although its reason was refused — the "+
				"refusal has to come before the destruction", id)
		}
	}
}
