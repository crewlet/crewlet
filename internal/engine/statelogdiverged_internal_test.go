package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// divergedBroker is a broker restored from a copy older than node A's rows and
// then written past them by node B, whose own rows were the copy's age — the
// one restore a checkpoint past the log's end cannot show, because the log no
// longer ends below A's checkpoint by the time A looks.
type divergedBroker struct {
	// a and b are the two nodes' bootstraps: one broker directory, a store
	// each.
	a, b config.Bootstrap
	cfg  *config.Company

	// before is A's task as its rows held it before the restore, and
	// checkpoint A's tracker checkpoint then.
	before     taskFields
	checkpoint cursorRow

	// end is where the log ends once B has written past A's checkpoint, and
	// written how many records B appended after the restore.
	end     uint64
	written int
}

// stageDivergedBroker stages it on one embedded broker, one node at a time:
// the restore ([stageRestoredBroker]), and then B booting on it and writing
// until the log ends past A's checkpoint.
func stageDivergedBroker(t *testing.T) divergedBroker {
	t.Helper()
	d := stageRestoredBroker(t)
	d.writePast(t)
	return d
}

// stageRestoredBroker stages the restore alone: A writes a project, a task and
// an edit, the broker's store is copied — and so is A's, which becomes B — A
// writes the tail the copy never has, and the broker is restored from the
// copy. Nothing has written the restored log yet, so A's checkpoint is past
// its end.
func stageRestoredBroker(t *testing.T) divergedBroker {
	t.Helper()
	base := t.TempDir()
	d := divergedBroker{a: config.DefaultBootstrap()}
	d.a.Node.ID = "node-a"
	d.a.Store.Path = filepath.Join(base, "a", "crewlet.db")
	d.a.Stream.StoreDir = filepath.Join(base, "stream")
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	d.cfg = cfg
	copyDir := filepath.Join(base, "copy")

	// A, FIRST BOOT: what the copy will hold.
	e, back := bootNode(t, &d.a, cfg)
	waitUntil(t, 20*time.Second, "node A to admit seats", hydrated(t, e))
	at := time.Now().UTC()
	mustApply(t, "the project", func() (tracker.WriteResult, error) {
		return e.native.Load().writer.WriteDocument(t.Context(), "op-project",
			tracker.ProjectSubject("ENG"), "", tracker.Project{
				V: tracker.DocumentVersion, Key: "ENG", Name: "Engineering",
				CreatedAt: at, UpdatedAt: at,
			}, tracker.ChangeProjectCreated, nil)
	})
	mustApply(t, "the task", func() (tracker.WriteResult, error) {
		return e.native.Load().writer.CreateTask(t.Context(), "op-create", tracker.Task{
			V: tracker.DocumentVersion, ID: "t-1", Project: "ENG", Type: "task",
			Title: "copied", Status: tracker.StatusTodo,
			StatusGroup: tracker.GroupNotStarted, Priority: tracker.PriorityNormal,
			CreatedAt: at, UpdatedAt: at,
		}, nil)
	})
	mustApply(t, "the first edit", func() (tracker.WriteResult, error) {
		title := "in the copy"
		return e.native.Load().writer.UpdateTask(t.Context(), "op-edit-1", "t-1", "ENG",
			tracker.NoIfMatch, tracker.TaskPatch{Title: &title}, tracker.ChangeFields, nil)
	})
	e.Stop(context.Background())
	back.Close(context.Background())

	// THE COPY, and B: A's own store at the same moment.
	if err := os.CopyFS(copyDir, os.DirFS(d.a.Stream.StoreDir)); err != nil {
		t.Fatalf("copy the broker's store: %v", err)
	}
	d.b = d.a
	d.b.Node.ID = "node-b"
	d.b.Store.Path = filepath.Join(base, "b", "crewlet.db")
	if err := os.CopyFS(filepath.Dir(d.b.Store.Path), os.DirFS(filepath.Dir(d.a.Store.Path))); err != nil {
		t.Fatalf("copy node A's store as node B's: %v", err)
	}

	// A, SECOND BOOT: the tail the copy never has — several records, so the
	// log's end stays below A's checkpoint across a record or two the
	// restored log is written with.
	e2, back2 := bootNode(t, &d.a, cfg)
	waitUntil(t, 20*time.Second, "node A to admit seats again", hydrated(t, e2))
	for i := range 3 {
		mustApply(t, "an edit after the copy", func() (tracker.WriteResult, error) {
			title, done := fmt.Sprintf("after the copy %d", i), tracker.StatusDone
			return e2.native.Load().writer.UpdateTask(t.Context(), fmt.Sprintf("op-edit-2-%d", i),
				"t-1", "ENG", tracker.NoIfMatch,
				tracker.TaskPatch{Title: &title, Status: &done}, tracker.ChangeStatus, nil)
		})
	}
	d.before = taskState(t, e2, "t-1")
	d.checkpoint = readCursorRow(t, e2, tracker.Domain{}.Name())
	e2.Stop(context.Background())
	back2.Close(context.Background())

	// THE RESTORE.
	if err := os.RemoveAll(d.a.Stream.StoreDir); err != nil {
		t.Fatalf("remove the broker's store: %v", err)
	}
	if err := os.CopyFS(d.a.Stream.StoreDir, os.DirFS(copyDir)); err != nil {
		t.Fatalf("restore the broker's store from the copy: %v", err)
	}
	return d
}

// writePast boots B on the restored broker — its rows are the copy's age, so
// nothing it can see is wrong — writes the log past A's checkpoint, and
// PUBLISHES where that left it, as B's heartbeat would before it stopped: a
// fleet whose copy-age node never said how far it got hides every rule a peer's
// position feeds.
func (d *divergedBroker) writePast(t *testing.T) {
	t.Helper()
	eb, backB := bootNode(t, &d.b, d.cfg)
	waitUntil(t, 20*time.Second, "node B to admit seats", hydrated(t, eb))
	running := eb.native.Load().log.Domain(tracker.Domain{}.Name())
	for {
		stats, err := running.log.Stats(t.Context())
		if err != nil {
			t.Fatalf("read the restored log: %v", err)
		}
		if stats.LastSeq > d.checkpoint.at.Seq {
			d.end = stats.LastSeq
			break
		}
		d.written++
		mustApply(t, "B's write after the restore", func() (tracker.WriteResult, error) {
			title := fmt.Sprintf("written after the restore %d", d.written)
			return eb.native.Load().writer.UpdateTask(t.Context(),
				fmt.Sprintf("op-b-%d", d.written), "t-1", "ENG", tracker.NoIfMatch,
				tracker.TaskPatch{Title: &title}, tracker.ChangeFields, nil)
		})
	}
	waitUntil(t, 10*time.Second, "node B to apply its own writes", func() bool {
		return running.runner.Committed().Seq >= d.end
	})
	eb.native.Load().log.publishPositions(t.Context())
	rows, err := eb.backends.Fleet.Positions(t.Context())
	if err != nil {
		t.Fatalf("read the register: %v", err)
	}
	for _, row := range rows {
		if row.NodeID != d.b.Node.ID {
			continue
		}
		if pos := row.Domains[tracker.Domain{}.Name()]; pos.Seq < d.end || pos.CheckpointStoredAt.IsZero() {
			t.Fatalf("node B published %+v, want its position at the log's end %d "+
				"naming the record it stands on", pos, d.end)
		}
	}
	eb.Stop(context.Background())
	backB.Close(context.Background())
}

// A NODE WHOSE RESTORED LOG WAS WRITTEN PAST ITS ROWS BEFORE IT BOOTED REFUSES
// THE DOMAIN AND APPLIES NOTHING PAST ITS CHECKPOINT.
//
// Its checkpoint is not past the log's end — the other node wrote past it — so
// the only refusal a restored broker used to get never fired, and the node's
// applier resumed from its checkpoint into the other history: B's edits landed
// on top of rows holding A's tail, a mix no node could ever reconcile. The boot
// now verifies the record at the checkpoint, finds B's there, and the node
// refuses reads and writes, applies nothing, donates nothing, tells the fleet,
// and names the restored case.
func TestANodeWhoseRestoredLogWasWrittenPastItRefusesAndAppliesNothing(t *testing.T) {
	t.Parallel()
	d := stageDivergedBroker(t)

	e, _ := bootNode(t, &d.a, d.cfg)
	running := e.native.Load().log.Domain(tracker.Domain{}.Name())
	if err := running.runner.StreamIdentity(); !errors.Is(err, statelog.ErrLogDiverged) {
		t.Fatalf("at boot the tracker's identity is %v, want the divergence — the "+
			"log's record at the checkpoint is B's", err)
	}
	waitUntil(t, 10*time.Second, "the tracker's applier to stop on the divergence", func() bool {
		return errors.Is(running.runner.Stopped(), statelog.ErrLogDiverged)
	})
	if got := running.runner.Committed(); got != d.checkpoint.at {
		t.Fatalf("the tracker's checkpoint moved to %s, want it held at %s", got,
			d.checkpoint.at)
	}
	if got := taskState(t, e, "t-1"); got != d.before {
		t.Fatalf("the task is %+v and was %+v before the restore — the other "+
			"history's edits were applied on top of these rows", got, d.before)
	}

	// A WRITE REFUSES, naming the finding.
	title := "refused"
	_, err := e.native.Load().writer.UpdateTask(t.Context(), "op-refused", "t-1", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Title: &title}, tracker.ChangeFields, nil)
	var refused *statelog.Unavailable
	if !errors.As(err, &refused) || refused.Reason != statelog.ReasonWrongStream ||
		!errors.Is(err, statelog.ErrLogDiverged) {
		t.Fatalf("a write on the diverged node returned %v, want wrong_stream over "+
			"the divergence", err)
	}

	// THE HEALTH AND THE STATUS SAY SO.
	health, err := e.native.Load().log.health(t.Context(), running)
	if err != nil {
		t.Fatalf("read the tracker's health: %v", err)
	}
	if !health.LogDiverged || health.Refusal(time.Now()) != statelog.RefuseWrongStream {
		t.Fatalf("the health is diverged=%v refusing %q, want the divergence "+
			"refusing wrong_stream", health.LogDiverged, health.Refusal(time.Now()))
	}
	for _, row := range e.native.Load().log.Status(t.Context()) {
		if row.Name == (tracker.Domain{}).Name() && (row.Ready ||
			!errors.Is(running.runner.StreamIdentity(), statelog.ErrLogDiverged)) {
			t.Fatalf("the replication status reads %+v for the diverged tracker", row)
		}
	}

	// THE FLEET IS TOLD, on this node's own row.
	e.native.Load().log.publishPositions(t.Context())
	rows, err := e.backends.Fleet.Positions(t.Context())
	if err != nil {
		t.Fatalf("read the register: %v", err)
	}
	published := false
	for _, row := range rows {
		if row.NodeID == d.a.Node.ID {
			published = row.Domains[tracker.Domain{}.Name()].LogDiverged
		}
	}
	if !published {
		t.Fatal("node A's own row does not say the log diverged from its rows")
	}

	// AND THE REANCHOR IT NEEDS IS THE RESTORED ONE, from where the log ends.
	view, err := e.ReanchorStatus(t.Context(), tracker.Domain{}.Stream().Name)
	if err != nil {
		t.Fatalf("ReanchorStatus: %v", err)
	}
	if view.Case != statelog.ReanchorRestored || view.Cursor != d.end {
		t.Fatalf("ReanchorStatus names the %q case at %d (%s), want the restored "+
			"case at the log's end, %d", view.Case, view.Cursor, view.Refusal, d.end)
	}
}

// A DIVERGED NODE KEEPS ITS VERDICT AFTER THE RECORD IT WAS FOUND BY IS GONE.
//
// The verdict is found by comparing the record the checkpoint names with the
// log's record at the same sequence, and the log can lose that record — the
// trim removes it once the node is evicted and stops being counted, and a
// stream can be purged by hand. A node restarted after that had nothing to
// compare: it found its log replayable from its checkpoint, applied the other
// history on top of its rows and published that the log no longer diverged,
// which lifted every peer's truncation fence. The verdict is recorded in the
// node estate when it is reached, so the restarted node still refuses, applies
// nothing and tells the fleet.
func TestADivergedNodeKeepsItsVerdictAfterTheRecordItWasFoundByIsGone(t *testing.T) {
	t.Parallel()
	d := stageDivergedBroker(t)

	// A MEETS THE DIVERGENCE AT BOOT, and records it.
	e, back := bootNode(t, &d.a, d.cfg)
	running := e.native.Load().log.Domain(tracker.Domain{}.Name())
	if err := running.runner.StreamIdentity(); !errors.Is(err, statelog.ErrLogDiverged) {
		t.Fatalf("at boot the tracker's identity is %v, want the divergence", err)
	}
	e.Stop(context.Background())
	back.Close(context.Background())

	// THE RECORD AT A'S CHECKPOINT GOES, purged through B's node on the one
	// broker both share.
	eb, backB := bootNode(t, &d.b, d.cfg)
	waitUntil(t, 20*time.Second, "node B to admit seats", hydrated(t, eb))
	logB := eb.native.Load().log.Domain(tracker.Domain{}.Name()).log
	if err := logB.Purge(t.Context(), d.checkpoint.at.Seq+1); err != nil {
		t.Fatalf("purge the log past A's checkpoint: %v", err)
	}
	stats, err := logB.Stats(t.Context())
	if err != nil || stats.FirstSeq <= d.checkpoint.at.Seq {
		t.Fatalf("the log starts at %d (%v), want past A's checkpoint %d", stats.FirstSeq,
			err, d.checkpoint.at.Seq)
	}
	eb.Stop(context.Background())
	backB.Close(context.Background())

	// A AGAIN: the log holds nothing at its checkpoint to compare, and it
	// still refuses.
	e2, _ := bootNode(t, &d.a, d.cfg)
	running2 := e2.native.Load().log.Domain(tracker.Domain{}.Name())
	if err := running2.runner.StreamIdentity(); !errors.Is(err, statelog.ErrLogDiverged) {
		t.Fatalf("after the restart the tracker's identity is %v, want the recorded "+
			"divergence — the rows are still the ones the log does not continue", err)
	}
	waitUntil(t, 10*time.Second, "the tracker's applier to stop on the divergence", func() bool {
		return errors.Is(running2.runner.Stopped(), statelog.ErrLogDiverged)
	})
	if got := taskState(t, e2, "t-1"); got != d.before {
		t.Fatalf("the task is %+v and was %+v before the restore — the restarted "+
			"node applied the other history on top of its rows", got, d.before)
	}
	title := "refused"
	_, err = e2.native.Load().writer.UpdateTask(t.Context(), "op-refused-2", "t-1", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Title: &title}, tracker.ChangeFields, nil)
	if !errors.Is(err, statelog.ErrLogDiverged) {
		t.Fatalf("a write on the restarted node returned %v, want the divergence", err)
	}
	e2.native.Load().log.publishPositions(t.Context())
	rows, err := e2.backends.Fleet.Positions(t.Context())
	if err != nil {
		t.Fatalf("read the register: %v", err)
	}
	for _, row := range rows {
		if row.NodeID == d.a.Node.ID && !row.Domains[tracker.Domain{}.Name()].LogDiverged {
			t.Fatal("node A's own row no longer says the log diverged from its rows — " +
				"every peer's truncation fence lifts on it")
		}
	}
}
