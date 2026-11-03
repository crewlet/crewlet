package code

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// codeAt is the position at seq on the code log's first generation, the one
// every record here is applied on.
func codeAt(seq uint64) statelog.Position {
	return statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: seq}
}

// seedRetired writes count retired pack rows of id straight into the estate,
// all retired by the record at seq — a case's rows, never the applier's — in
// one transaction, since a benchmark seeds a whole record's worth.
func (e *estate) seedRetired(id string, seq uint64, at time.Time, count int) {
	e.t.Helper()
	ctx := context.Background()
	if err := e.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := store.InsertRows(ctx, tx, e.db.Caps().MaxVariables,
			`INSERT INTO code_packs_retired (repo_id, pack_id, part, object, hash, size, index_bytes,
				reverse_bytes, git_checksum, objects, kind, added_position, retired_position,
				retired_at) VALUES `,
			`(?, ?, 'pack', ?, ?, 10, 0, 0, ?, 1, 'push', 1, ?, ?)`, ``, count,
			func(i int) []any {
				p := packOf(fmt.Sprintf("%s-retired-%d-%d", id, seq, i))
				return []any{id, p.PackID, p.Object.String(), string(p.Hash), p.GitChecksum,
					codeAt(seq).Packed(), store.EncodeTime(at)}
			})
		return err
	}); err != nil {
		e.t.Fatalf("seed %s's retired packs: %v", id, err)
	}
}

// sweepOf is a retire_sweep of id through the record at seq.
func sweepOf(t testing.TB, id string, seq uint64) built {
	return rec(t, RepoSubject(id), OpRetireSweep, Scope{RepoPath(id)},
		RetireSweep{RepoID: id, Through: codeAt(seq)})
}

// A SWEEP REMOVES ONLY WHAT ITS RECORD NAMES: the retired rows at or below its
// Through, and the repository's mark raised to it — never a row above it,
// whatever that row's instant says, since the apply reads no clock and the
// record is the whole decision — and applied again it deletes nothing and
// leaves the mark where it was. A Through no writer forms is refused rather
// than applied.
//
// Mutation: delete the rows strictly below Through, and its own position's
// stay; judge the rows by the record's broker instant instead, and the row
// above Through, as old as the rest, goes with them; lower the mark to a
// later record's Through, and a delta below the rows the first one removed
// reads as complete.
func TestASweepRemovesOnlyWhatItsRecordNames(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	// THREE RETIRING RECORDS, at positions 2, 3 and 4, all a day and an hour
	// before the sweep's own instant: the rows' age is not what decides.
	old := applyClock.Add(-RetireGrace - time.Hour)
	e.seedRetired("repo-a", 2, old, 2)
	e.seedRetired("repo-a", 3, old, 3)
	e.seedRetired("repo-a", 4, old, 1)
	at := func(seq uint64) int64 {
		return e.num(`SELECT COUNT(*) FROM code_packs_retired WHERE retired_position = ?`, codeAt(seq).Packed())
	}
	mark := func() int64 { return e.num(`SELECT retired_through FROM code_repos WHERE id = 'repo-a'`) }

	sweep := sweepOf(t, "repo-a", 3)
	e.next = 10
	if rows := e.applyCounting(sweep); rows != 5+1 {
		t.Fatalf("the sweep reported %d rows, want the five it removed and the repository's own", rows)
	}
	if at(2) != 0 || at(3) != 0 || at(4) != 1 {
		t.Fatalf("after the sweep through position 3, positions 2, 3 and 4 hold %d, %d and %d rows; "+
			"want 0, 0 and 1", at(2), at(3), at(4))
	}
	if got := mark(); got != codeAt(3).Packed() {
		t.Fatalf("the retired mark is %d, want position 3's %d", got, codeAt(3).Packed())
	}
	if version := e.num(`SELECT version FROM code_repos WHERE id = 'repo-a'`); version != codeAt(10).Packed() {
		t.Fatalf("the repository's version is %d, want the sweep's position %d", version, codeAt(10).Packed())
	}

	// REDELIVERED AT ITS OWN POSITION, the version guard holds it to nothing.
	e.next = 10
	if rows := e.applyCounting(sweep); rows != 0 {
		t.Fatalf("the sweep applied again reported %d rows, want none", rows)
	}
	// A LATER SWEEP THROUGH AN EARLIER POSITION removes nothing more, and
	// never lowers the mark.
	if rows := e.applyCounting(sweepOf(t, "repo-a", 2)); rows != 1 || at(4) != 1 ||
		mark() != codeAt(3).Packed() {
		t.Fatalf("a later sweep through position 2 reported %d rows, left %d at position 4 and "+
			"marked %d; want only the repository's own row, 1 and %d", rows, at(4), mark(),
			codeAt(3).Packed())
	}

	// A THROUGH NO WRITER FORMS: at or past the sweep's own position, or on
	// another log.
	for name, b := range map[string]built{
		"through its own position": sweepOf(t, "repo-a", 50),
		"through another log's position": rec(t, RepoSubject("repo-a"), OpRetireSweep,
			Scope{RepoPath("repo-a")}, RetireSweep{RepoID: "repo-a",
				Through: statelog.Position{Stream: topics.PagesLogStream, Generation: 1, Seq: 4}}),
	} {
		if err := e.applyErr(b); err == nil {
			t.Errorf("a sweep %s applied; want it refused as no writer's record", name)
		}
	}
	if at(4) != 1 {
		t.Fatalf("a refused sweep removed position 4's row")
	}
}
