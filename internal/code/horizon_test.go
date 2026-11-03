package code

import (
	"database/sql"
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

// THE HORIZON STOPS AT THE FIRST PUSH INSIDE IT, IN LOG ORDER. A record's
// instant is its broker's, and a stream leader's change can stamp a later
// record earlier than the one before it: here three pushes land at positions
// p1 < p2 < p3 stamped t1, t3, t2. A push past the horizon between t2 and t3
// removes p1 and stops at p2, inside it — keeping p3, older by its stamp,
// until p2 ages out — and every applier, reading the same rows in the same
// order, removes the same ones and never one inside the horizon.
//
// Mutation: filter the expiry's statement by `at < ?` rather than stop at the
// first push inside the horizon — p3 is removed past p2, which the horizon
// keeps (and the read walks every push the repository holds to find it).
func TestTheHorizonStopsAtTheFirstPushInsideIt(t *testing.T) {
	t.Parallel()
	t1 := applyClock
	t2, t3 := t1.Add(2*time.Hour), t1.Add(3*time.Hour)
	records := []built{
		create(t, "repo-a", "a"),
		pushOf(t, "repo-a", pushEntry("p1", move("refs/heads/one", ZeroOID, oid("1")))).at(t1),
		pushOf(t, "repo-a", pushEntry("p2", move("refs/heads/two", ZeroOID, oid("2")))).at(t3),
		pushOf(t, "repo-a", pushEntry("p3", move("refs/heads/three", ZeroOID, oid("3")))).at(t2),
		// THE HORIZON half an hour after t2 and half an hour before t3.
		pushOf(t, "repo-a", pushEntry("p4", move("refs/heads/four", ZeroOID, oid("4")))).
			at(t2.Add(PushHistory + 30*time.Minute)),
	}
	var kept []string
	for i := range 2 {
		e := openApplyEstate(t)
		e.apply(records...)
		var left []string
		if err := e.db.Reader().Read(t.Context(), func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(t.Context(),
				`SELECT push_id FROM code_pushes WHERE repo_id = 'repo-a' ORDER BY position, push_id`)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					return err
				}
				left = append(left, id)
			}
			return rows.Err()
		}); err != nil {
			t.Fatalf("read the pushes left: %v", err)
		}
		if fmt.Sprint(left) != "[p2 p3 p4]" {
			t.Fatalf("applier %d keeps %v, want [p2 p3 p4]: p1 past the horizon removed, and the read "+
				"stopped at p2 inside it, keeping p3 behind it", i, left)
		}
		if i > 0 && fmt.Sprint(left) != fmt.Sprint(kept) {
			t.Fatalf("two appliers of the same records keep %v and %v", kept, left)
		}
		kept = left
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

// A SWEEP KEEPS PACE WITH BULK DELETION: a repository whose pushes each round
// make a thousand refs and delete the thousand the previous round made — a
// CI's per-build tags, a branch clean-up — keeps a thousand live refs and,
// once each delete is past RefTombstoneHorizon, no more tombstones than the
// last round made. A sweep of a fixed share a record fell behind that by about
// 936 rows a record and kept every tombstone it ever made; the sweep takes
// what the record's row budget left, so the table converges. The round's two
// pushes are two records, as the committer forms them: a thousand deletions,
// each counted with its sweep's room and the object it left, share no record
// with a thousand creates.
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
		records := []built{pushOf(t, "repo-a", pushEntry(fmt.Sprint("make-", round), made...)).at(at)}
		if len(dropped) > 0 {
			records = append(records, pushOf(t, "repo-a", pushEntry(fmt.Sprint("drop-", round), dropped...)).at(at))
		}
		e.apply(records...)
		at = at.Add(RefTombstoneHorizon + 24*time.Hour)
	}
	if live := e.num(`SELECT COUNT(*) FROM code_refs WHERE target != ''`); live != batch {
		t.Fatalf("the repository holds %d live refs, want the last push's %d", live, batch)
	}
	if stones := e.num(`SELECT COUNT(*) FROM code_refs WHERE target = ''`); stones != batch {
		t.Fatalf("after %d rounds the repository holds %d tombstones, want only the last "+
			"round's %d — every earlier one was past the horizon when a push came", rounds, stones, batch)
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
