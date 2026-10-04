package tracker_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
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

	gates := tracker.NewGates(r.db.Reader())
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

// EVERY WRITE ON A PURGED TASK IS REFUSED AS PURGED, which is final — never as
// "not on this node".
//
// It is a refusal rather than an outcome and never a re-decide: republishing
// produces another durable record nothing applies, and the caller burns its
// whole round budget to a conflict a model reads as a colleague editing. And
// it is the FINAL refusal, [tracker.ErrNoTask] naming the purge, because the
// other one — [statelog.ErrUnavailable], "not on this node" — is what the
// CALLER retries (the estate's router hands it back as the holder gave it),
// and every node holding the task's deletion marker would say it again: a seat
// editing a task somebody purged was told to try again for ever. The purge and
// a turn's spend had the final answer; every other gesture on a task answered
// the retryable one until each was held here.
func TestEveryWriteOnAPurgedTaskIsRefusedAsPurged(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")
	filedTask(t, r, "t-2")
	if _, err := r.writer.PurgeTask(t.Context(), "op-purge", "t-1", "ENG", ""); err != nil {
		t.Fatalf("PurgeTask: %v", err)
	}
	r.drain()

	for _, c := range []struct {
		gesture string
		write   func() error
	}{
		{"an edit", func() error {
			_, err := r.writer.UpdateTask(t.Context(), "op-edit", "t-1", "ENG",
				tracker.NoIfMatch, tracker.TaskPatch{Title: ptr("back from the dead")},
				tracker.ChangeFields, nil)
			return err
		}},
		{"a removal", func() error {
			_, err := r.writer.RemoveTask(t.Context(), "op-remove", "t-1", "ENG", false, nil)
			return err
		}},
		{"a restore", func() error {
			_, err := r.writer.RestoreTask(t.Context(), "op-restore", "t-1", "ENG", nil)
			return err
		}},
		{"a move to another project", func() error {
			_, err := r.writer.MoveTaskToProject(t.Context(), "op-move", "t-1", "OPS", nil)
			return err
		}},
		{"a merge of it", func() error {
			_, err := r.writer.MergeDuplicates(t.Context(), "op-merge-it", "t-1", "t-2", false, nil)
			return err
		}},
		{"a merge into it", func() error {
			_, err := r.writer.MergeDuplicates(t.Context(), "op-merge-into", "t-2", "t-1", true, nil)
			return err
		}},
		{"a dependency on it", func() error {
			_, err := r.writer.Depend(t.Context(), "op-depend", tracker.DependencyChange{
				Task: "t-1", Project: "ENG", WaitingOnAdd: []string{"t-2"},
			}, nil)
			return err
		}},
		{"a promotion out of it", func() error {
			_, err := r.writer.PromoteItem(t.Context(), "op-promote", "t-1", "item-1",
				newTask("t-3"), nil)
			return err
		}},
	} {
		err := c.write()
		switch {
		case err == nil:
			t.Errorf("%s of a purged task was accepted", c.gesture)
		case !errors.Is(err, tracker.ErrNoTask) || errors.Is(err, statelog.ErrUnavailable):
			t.Errorf("%s of a purged task answered %v, want the final ErrNoTask "+
				"and never a refusal a caller would retry elsewhere", c.gesture, err)
		case !strings.Contains(err.Error(), "was purged"):
			t.Errorf("%s: the refusal %q does not say the task was purged", c.gesture, err)
		}
	}
	if answer := r.ask(map[string]any{"container": "project:ENG"}); len(answer.Rows) != 1 {
		t.Fatalf("the board after the refused writes is %+v, want t-2 alone", answer.Rows)
	}
}

// A SECOND PURGE OF A PURGED TASK IS REFUSED AS PURGED, which is final — never
// as "not on this node".
//
// That refusal is the one the CALLER retries — the estate's router hands it
// back as the holder gave it — and every node holding the task's deletion
// marker would say it again: an operator re-running a purge to be sure was
// told the node was behind, of a task every node had destroyed. The purge's
// own retry, under its own id, is answered by the first purge's outcome
// instead, and nothing is appended.
func TestASecondPurgeOfAPurgedTaskIsRefusedAsPurged(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")
	first, err := r.writer.PurgeTask(t.Context(), "op-purge", "t-1", "ENG", "spam")
	if err != nil {
		t.Fatalf("PurgeTask: %v", err)
	}
	r.drain()
	end := r.logEnd(t)

	_, err = r.writer.PurgeTask(t.Context(), "op-purge-again", "t-1", "ENG", "to be sure")
	if !errors.Is(err, tracker.ErrNoTask) || errors.Is(err, statelog.ErrUnavailable) {
		t.Fatalf("a second purge of a purged task answered %v, want the final "+
			"ErrNoTask and never a refusal a caller would retry elsewhere", err)
	}
	if !strings.Contains(err.Error(), "was purged") {
		t.Errorf("the refusal %q does not say the task was purged", err)
	}
	retry, err := r.writer.PurgeTask(t.Context(), "op-purge", "t-1", "ENG", "spam")
	if err != nil || retry.Position != first.Position {
		t.Fatalf("the first purge's own retry = (%+v, %v), want its outcome at %s",
			retry.Result, err, first.Position)
	}
	if got := r.logEnd(t); got != end {
		t.Fatalf("purging a purged task put %d record(s) on the log", got-end)
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
	if err := r.db.Read(t.Context(), func(tx *sql.Tx) error {
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
		rows, err := tracker.Domain{}.Evictions(t.Context(), r.db.Reader())
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
		evicted, found, err := statelog.EvictedOnLog(t.Context(), tracker.Domain{}, statelog.EstateStream(tracker.Domain{}), r.log, node)
		if err != nil || !found || evicted != want {
			t.Fatalf("%s's standing read off the log = evicted %v, found %v (%v), "+
				"want evicted %v", node, evicted, found, err, want)
		}
	}
	if _, found, err := statelog.EvictedOnLog(t.Context(), tracker.Domain{}, statelog.EstateStream(tracker.Domain{}), r.log, "node-b"); err != nil || found {
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
		"a node's release of the log": {
			// THE SAME GATE AS AN EVICTION, the node's own. A node that
			// deferred it would apply every write the leaver had in
			// flight — and halting is the answer an older build gives
			// it, by its kind, at the version that added it.
			envelope: statelog.Envelope{Kind: string(tracker.KindEviction),
				Op: string(tracker.OpRelease)},
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
	if err := r.db.Read(t.Context(), func(tx *sql.Tx) error {
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

// A RELEASE RACING A WRITER: THE WRITE THE LEAVING NODE HAD IN FLIGHT IS
// DROPPED ON EVERY HOLDER, AND ITS CALLER IS TOLD SO.
//
// A node leaving a partition stops deciding there, then releases each
// identity-claiming log of it: its own statement that nothing it publishes on
// the log afterwards applies anywhere. A write already past the node's last
// question about serving when the leave began can still reach the broker after
// the release — here the leave runs between that question and the landing —
// and it is dropped by the gate an eviction installs, its writer told
// `released`: never `applied`, and never a lost race. After the release the
// node decides nothing on the log (`not_holder`); the row records the release
// as the node's own gate, which the node's write fence does not read as an
// eviction; and a release naming another node is refused before the log.
func TestAReleaseRacingAWriterDropsTheWriteItHadInFlight(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	if _, err := r.writer.CreateTask(t.Context(), "op-before", newTask("t-before"), nil); err != nil {
		t.Fatalf("a write before the release: %v", err)
	}
	race := statelogtest.NewRace(r.log)
	inFlight := r.writerOver(t, race)
	var released tracker.WriteResult
	race.Before(func(subject string) bool { return strings.HasSuffix(subject, ".t-racing") },
		func() {
			// THE LEAVE, between the in-flight write's last question and
			// its landing: the node stops serving, then releases the log.
			r.holding.Stop(statelog.EstatePartition)
			var err error
			if released, err = r.writer.ReleaseLog(t.Context(), "op-release", r.nodeID); err != nil {
				t.Errorf("ReleaseLog: %v", err)
			}
		})
	_, err := inFlight.CreateTask(t.Context(), "op-racing", newTask("t-racing"), nil)
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonReleased {
		t.Fatalf("the write that landed after its node's release was answered %v, "+
			"want a refusal %q — it is on the log and applies nowhere", err,
			statelog.ReasonReleased)
	}
	r.drain()
	if answer := r.ask(map[string]any{"container": "project:ENG"}); len(answer.Rows) != 1 {
		t.Fatalf("the project holds %d task(s), want only the one written before the "+
			"release", len(answer.Rows))
	}

	rows, err := tracker.Domain{}.Evictions(t.Context(), r.db.Reader())
	if err != nil {
		t.Fatalf("read the gates: %v", err)
	}
	var row *statelog.EvictionRow
	for i := range rows {
		if rows[i].NodeID == r.nodeID {
			row = &rows[i]
		}
	}
	switch {
	case row == nil:
		t.Fatalf("the release landed and the log's rows hold no gate for %s", r.nodeID)
	case row.Kind != statelog.EvictionKindRelease:
		t.Fatalf("the release is recorded as a %q", row.Kind)
	case row.From != uint64(released.Position.Packed()) || row.Back:
		t.Fatalf("the release is recorded as %+v, want the node out above its own "+
			"position %d", *row, released.Position.Packed())
	case row.By != r.nodeID:
		t.Fatalf("the release is recorded as %s's, want the node's own (%s)", row.By, r.nodeID)
	}
	evicted, err := tracker.NewFence(r.db.Reader(), r.nodeID).Evicted(t.Context())
	if err != nil || evicted {
		t.Fatalf("the node's fence reads its own release as an eviction (%v, %v) — "+
			"a node that left a partition is not one the fleet removed", evicted, err)
	}

	end := r.logEnd(t)
	_, err = r.writer.CreateTask(t.Context(), "op-after", newTask("t-after"), nil)
	if !errors.Is(err, statelog.ErrNotHolder) {
		t.Fatalf("a write asked of the node after it left was answered %v, want %v",
			err, statelog.ErrNotHolder)
	}
	if _, err := r.writer.ReleaseLog(t.Context(), "op-release-other", "node-b"); err == nil {
		t.Fatal("a node published a release naming another node — an eviction " +
			"nobody judged")
	}
	if after := r.logEnd(t); after != end {
		t.Fatalf("a refused write reached the log: its end moved from %d to %d", end, after)
	}
}
