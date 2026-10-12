package code

import (
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A PUSH'S HORIZONS ARE ITS OWN REPOSITORY'S: the push history and the ref
// tombstones age out inside the record that touches a repository, bounded per
// record, so a sweep is a pure function of a committed record and never a
// second writer — and one repository's push never removes another's rows.
func TestPushRowsExpireOnlyWithinTheirRepository(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"), create(t, "repo-b", "b"))
	// SEVENTY PUSHES EACH, every one stamped at the epoch: older than
	// PushHistory by decades.
	e.seed("repo-a", 70, 0, 0)
	e.seed("repo-b", 70, 0, 0)
	e.apply(pushOf(t, "repo-a", pushEntry("fresh", move("refs/heads/main", ZeroOID, oid("A")))))

	if left := e.num(`SELECT COUNT(*) FROM code_pushes WHERE repo_id = 'repo-a'`); left != 70-HorizonDeletesPerRecord+1 {
		t.Fatalf("repo-a holds %d pushes, want %d: the seventy less the %d oldest one record "+
			"may remove, plus its own", left, 70-HorizonDeletesPerRecord+1, HorizonDeletesPerRecord)
	}
	if oldest := e.num(`SELECT MIN(at) FROM code_pushes WHERE repo_id = 'repo-a'`); oldest != HorizonDeletesPerRecord {
		t.Fatalf("the oldest push repo-a keeps is at %d, want %d — the horizon removes the "+
			"oldest first", oldest, HorizonDeletesPerRecord)
	}
	if left := e.num(`SELECT COUNT(*) FROM code_pushes WHERE repo_id = 'repo-b'`); left != 70 {
		t.Fatalf("a push to repo-a removed %d of repo-b's pushes", 70-left)
	}
}

// A SWEPT TOMBSTONE RAISES THE MARK: a delta from below it can no longer learn
// every deletion, so a snapshot asked for one answers in full — and a
// tombstone younger than the horizon stays.
func TestATombstoneIsSweptAndRaisesTheMark(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	t0 := applyClock
	e.apply(create(t, "repo-a", "a"),
		pushOf(t, "repo-a",
			pushEntry("p1", move("refs/heads/old", ZeroOID, oid("1")), move("refs/heads/young", ZeroOID, oid("2")))).at(t0),
		pushOf(t, "repo-a", pushEntry("p2", move("refs/heads/old", oid("1"), ZeroOID))).at(t0))
	deleteAt := statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 3}.Packed()
	young := t0.Add(RefTombstoneHorizon)
	e.apply(pushOf(t, "repo-a", pushEntry("p3", move("refs/heads/young", oid("2"), ZeroOID))).at(young))
	if stones := e.num(`SELECT COUNT(*) FROM code_refs WHERE target = ''`); stones != 2 {
		t.Fatalf("the premise: two tombstones, and the repository holds %d", stones)
	}
	if mark := e.num(`SELECT tombstones_through FROM code_repos WHERE id = 'repo-a'`); mark != 0 {
		t.Fatalf("the mark is %d before any tombstone aged out", mark)
	}

	e.apply(pushOf(t, "repo-a", pushEntry("p4", move("refs/heads/new", ZeroOID, oid("3")))).
		at(t0.Add(RefTombstoneHorizon + time.Second)))
	if n := e.num(`SELECT COUNT(*) FROM code_refs WHERE ref = 'refs/heads/old'`); n != 0 {
		t.Fatal("the tombstone past the horizon was not swept")
	}
	if n := e.num(`SELECT COUNT(*) FROM code_refs WHERE ref = 'refs/heads/young' AND target = ''`); n != 1 {
		t.Fatal("the tombstone inside the horizon was swept")
	}
	if mark := e.num(`SELECT tombstones_through FROM code_repos WHERE id = 'repo-a'`); mark != deleteAt {
		t.Fatalf("the mark is %d after the sweep, want the swept delete's position %d", mark, deleteAt)
	}
	e.countersMatchRows("repo-a")
}

// A SWEEP KEEPS PACE WITH BULK DELETION: a repository whose every push makes a
// thousand refs and deletes the thousand the previous push made — a CI's
// per-build tags, a branch clean-up — keeps a thousand live refs and, once
// each delete is past RefTombstoneHorizon, no more tombstones than the last
// push made. A sweep of a fixed share a record fell behind that by about 936
// rows a record and kept every tombstone it ever made; the sweep takes what
// the record's row budget left, so the table converges.
func TestTheTombstoneSweepKeepsPaceWithBulkDeletion(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	const batch, rounds = 1_000, 12
	at := applyClock
	for round := range rounds {
		var made, dropped []Command
		for i := range batch {
			made = append(made, move(fmt.Sprintf("refs/tags/build-%02d-%04d", round, i), ZeroOID,
				oid(fmt.Sprint(round, i))))
			if round > 0 {
				dropped = append(dropped, move(fmt.Sprintf("refs/tags/build-%02d-%04d", round-1, i),
					oid(fmt.Sprint(round-1, i)), ZeroOID))
			}
		}
		entries := []PushEntry{pushEntry(fmt.Sprint("make-", round), made...)}
		if len(dropped) > 0 {
			entries = append(entries, pushEntry(fmt.Sprint("drop-", round), dropped...))
		}
		e.apply(pushOf(t, "repo-a", entries...).at(at))
		at = at.Add(RefTombstoneHorizon + 24*time.Hour)
	}
	if live := e.num(`SELECT COUNT(*) FROM code_refs WHERE target != ''`); live != batch {
		t.Fatalf("the repository holds %d live refs, want the last push's %d", live, batch)
	}
	if stones := e.num(`SELECT COUNT(*) FROM code_refs WHERE target = ''`); stones != batch {
		t.Fatalf("after %d rounds the repository holds %d tombstones, want only the last "+
			"push's %d — every earlier one was past the horizon when a push came", rounds, stones, batch)
	}
	e.countersMatchRows("repo-a")
}

// A SWEEP NEVER TAKES A RECORD PAST ITS ROW BUDGET: what a record writes and
// what it removes together fit MaxRecordRows, however many expired tombstones
// are waiting — the rest wait for the next push.
func TestTheTombstoneSweepStaysInsideTheRecordsRowBudget(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	// MORE EXPIRED TOMBSTONES THAN ONE RECORD MAY REMOVE, stamped at the
	// epoch.
	waiting := MaxRecordRows + 500
	e.exec(`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i + 1 < ?)
		INSERT INTO code_refs (repo_id, ref, target, position, at)
		SELECT 'repo-a', printf('refs/heads/gone-%06d', i), '', 1, 0 FROM n`, waiting)
	e.exec(`UPDATE code_repos SET tombstones = ? WHERE id = 'repo-a'`, waiting)

	var cmds []Command
	for i := range MaxCommandsPerPush {
		cmds = append(cmds, move(fmt.Sprintf("refs/heads/new-%04d", i), ZeroOID, oid(fmt.Sprint(i))))
	}
	if written := e.applyCounting(pushOf(t, "repo-a", pushEntry("big", cmds...))); written != MaxRecordRows {
		t.Fatalf("the record wrote and removed %d rows, want exactly its budget of %d — "+
			"the sweep takes what the push left", written, MaxRecordRows)
	}
	// The push wrote its refs, its pack, its push row and the counters; the
	// sweep's mark is one more row.
	swept := MaxRecordRows - (MaxCommandsPerPush + 1 + 1 + 1) - 1
	if left := e.num(`SELECT COUNT(*) FROM code_refs WHERE target = ''`); left != int64(waiting-swept) {
		t.Fatalf("%d tombstones are left, want %d of %d after a sweep of %d", left,
			waiting-swept, waiting, swept)
	}
	e.countersMatchRows("repo-a")
}
