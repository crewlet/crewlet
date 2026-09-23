package tracker_test

import (
	"database/sql"
	"errors"
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

// THE GATE COUNTS WHAT IT DROPS.
//
// The residual producers are replay-shaped — a deferred record reprocessed
// after an upgrade, a snapshot adopter replaying forward — and those are hits
// worth counting rather than writes worth attributing. A counter is the
// strictest form of the envelope-only rule: it stores neither envelope nor
// payload.
func TestTheDeletionGateCountsItsHits(t *testing.T) {
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

	// THE PATCH AGAIN, after the marker exists. That is the replay shape
	// the counter is for: a redelivery, a reprocess after an upgrade, or
	// a snapshot adopter replaying forward past a purge it never saw.
	r.redeliver(patch.Position.Seq)

	var rejects int
	if err := r.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT rejects FROM tracker_deletions WHERE task_id = ?`,
			"t-1").Scan(&rejects)
	}); err != nil {
		t.Fatalf("read the marker: %v", err)
	}
	if rejects == 0 {
		t.Fatal("the gate dropped a record and counted nothing — the count is " +
			"the only trace a dropped record leaves, and an operator reading " +
			"the purge report sees zero for a task the fleet is still " +
			"rejecting writes on")
	}
}

// A READMISSION IS THE INVERSE COMMIT, AND THIS LOG'S OWN ROWS SAY SO.
//
// The eviction's whole history survives a replay because a readmission is a
// second record rather than a delete — so a node that was evicted, readmitted
// and evicted again reads as three facts rather than as one long absence. And
// [tracker.Domain.Evictions] is what the trim reads to stop counting a node on
// THIS log, so what it answers after each step is the contract: evicted and
// not back, then back, then evicted again with the readmission cleared.
//
// Whether the readmission is PERMITTED is not this writer's to judge any more:
// it is one fleet gesture over every identity-claiming log, judged once by the
// engine's node gate before either log is written, and certified there.
func TestAReadmissionIsTheInverseCommitOnThisLog(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	standing := func(node string) (held, back bool) {
		t.Helper()
		rows, err := tracker.Domain{}.Evictions(t.Context(), r.db)
		if err != nil {
			t.Fatalf("read the evictions: %v", err)
		}
		for _, row := range rows {
			if row.NodeID == node {
				if row.From == 0 || row.At.IsZero() {
					t.Fatalf("%s's eviction row carries no position (%d) or no "+
						"instant (%s) — the gate and the fence window read both",
						node, row.From, row.At)
				}
				if row.Back && row.Readmitted <= row.From {
					t.Fatalf("%s is back at %d, which is not above its eviction "+
						"at %d", node, row.Readmitted, row.From)
				}
				return true, row.Back
			}
		}
		return false, false
	}
	// AND THE SAME STANDING READ OFF THE LOG ITSELF ([statelog.EvictedOnLog]),
	// which is how a node a peer re-anchored past sees an eviction its
	// stopped applier never reaches.
	onLog := func(node string, want bool) {
		t.Helper()
		evicted, found, err := statelog.EvictedOnLog(t.Context(), tracker.Domain{}, r.log, node)
		if err != nil || !found || evicted != want {
			t.Fatalf("%s's standing read off the log = evicted %v, found %v (%v), "+
				"want evicted %v", node, evicted, found, err, want)
		}
	}
	if _, found, err := statelog.EvictedOnLog(t.Context(), tracker.Domain{}, r.log, "node-b"); err != nil || found {
		t.Fatalf("a node never gated has a standing on the log: found %v, %v", found, err)
	}

	for _, node := range []string{"node-b", "node-c"} {
		if _, err := r.writer.EvictNode(t.Context(), "op-evict-"+node, node); err != nil {
			t.Fatalf("EvictNode %s: %v", node, err)
		}
	}
	r.drain()
	if held, back := standing("node-b"); !held || back {
		t.Fatalf("node-b after its eviction: held %v, back %v — want evicted", held, back)
	}
	onLog("node-b", true)

	if _, err := r.writer.ReadmitNode(t.Context(), "op-back", "node-b"); err != nil {
		t.Fatalf("ReadmitNode: %v", err)
	}
	r.drain()
	if held, back := standing("node-b"); !held || !back {
		t.Fatalf("node-b after its readmission: held %v, back %v — want its row "+
			"kept and marked back, which is what an inverse commit is", held, back)
	}
	if held, back := standing("node-c"); !held || back {
		t.Fatalf("node-c, never readmitted, reads held %v, back %v", held, back)
	}
	onLog("node-b", false)
	onLog("node-c", true)

	if _, err := r.writer.EvictNode(t.Context(), "op-evict-again", "node-b"); err != nil {
		t.Fatalf("EvictNode again: %v", err)
	}
	r.drain()
	if held, back := standing("node-b"); !held || back {
		t.Fatalf("node-b evicted a second time reads held %v, back %v — a "+
			"re-eviction must clear the readmission, or the row would still "+
			"say the node is back", held, back)
	}
	onLog("node-b", true)
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
