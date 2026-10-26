package code

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// THE OBJECTS A PUSH LEFT, case by case on the applier: a forced update or a
// deletion records the object it moved its ref off (code_ref_drops), and a
// push record forgets its repository's drops past PruneGrace with what its
// row budget leaves once the tombstones have had theirs.

// refDropRow is one code_ref_drops row as a case reads it back.
type refDropRow struct {
	ref, old     string
	position, at int64
}

func (d refDropRow) String() string {
	return fmt.Sprintf("%s@%s(%d,%d)", d.ref, d.old[:7], d.position, d.at)
}

// dropsOf is every ref drop of a repository, in the order the expiry and a
// purge read them: by instant, then by the key.
func (e *estate) dropsOf(repoID string) []refDropRow {
	e.t.Helper()
	var out []refDropRow
	if err := e.db.Reader().Read(context.Background(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(context.Background(), `SELECT ref, old, position, at FROM code_ref_drops
			WHERE repo_id = ? ORDER BY at, position, ref, old`, repoID)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var d refDropRow
			if err := rows.Scan(&d.ref, &d.old, &d.position, &d.at); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	}); err != nil {
		e.t.Fatalf("read %s's ref drops: %v", repoID, err)
	}
	return out
}

// seedDrops writes count ref drops of id straight into the estate, all left by
// the record at seq and stamped at — a case's rows, never the applier's.
func (e *estate) seedDrops(id string, seq uint64, at time.Time, count int) {
	e.t.Helper()
	ctx := context.Background()
	if err := e.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := store.InsertRows(ctx, tx, e.db.Caps().MaxVariables,
			`INSERT INTO code_ref_drops (repo_id, position, ref, old, at) VALUES `, `(?, ?, ?, ?, ?)`, ``,
			count, func(i int) []any {
				return []any{id, codeAt(seq).Packed(), fmt.Sprintf("refs/heads/dropped-%d-%06d", seq, i),
					oid(fmt.Sprint(id, seq, i)), store.EncodeTime(at)}
			})
		return err
	}); err != nil {
		e.t.Fatalf("seed %s's ref drops: %v", id, err)
	}
}

// forced is an ok forced update of ref from old to new.
func forced(ref, old, new string) Command {
	c := move(ref, old, new)
	c.Forced = true
	return c
}

// A FORCED OR DELETING COMMAND RECORDS THE OBJECT IT LEFT, and nothing else
// does: of one record's commands, a forced update, a branch's deletion and a
// tag's each write a drop row naming the ref and the object it held, at the
// record's own position and broker instant — and a fast-forward, a create, a
// refused command and a redelivered push's write none. A ref the record's
// entries move in turn leaves each object it is moved off, so three forced
// turns of one ref through X, Y and back are two rows, X once; and the rows
// count in what the record reports it touched.
//
// Mutation: record every update with an old object, and the fast-forward is a
// drop; record only forced updates, and the deletions leave none; key the rows
// by (position, ref) alone, and one of the turns' two objects is lost.
func TestAForcedOrDeletingCommandRecordsTheObjectItLeft(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"),
		pushOf(t, "repo-a", pushEntry("p0",
			move("refs/heads/ff", ZeroOID, oid("ff-0")),
			move("refs/heads/forced", ZeroOID, oid("forced-0")),
			move("refs/heads/gone", ZeroOID, oid("gone-0")),
			move("refs/tags/v1", ZeroOID, oid("v1")),
			move("refs/heads/turns", ZeroOID, oid("X")),
			move("refs/heads/kept", ZeroOID, oid("kept-0")))))
	if drops := e.dropsOf("repo-a"); len(drops) != 0 {
		t.Fatalf("the premise: the creates left %v — a create moves its ref off nothing", drops)
	}

	refused := forced("refs/heads/kept", oid("kept-0"), oid("kept-1"))
	refused.Result, refused.Reason = ResultNG, RefusalProtectedForce
	record := pushOf(t, "repo-a",
		// A REDELIVERED PUSH applies as nothing, its deletion included.
		pushEntry("p0", move("refs/heads/kept", oid("kept-0"), ZeroOID)),
		pushEntry("p1",
			move("refs/heads/ff", oid("ff-0"), oid("ff-1")),
			forced("refs/heads/forced", oid("forced-0"), oid("forced-1")),
			move("refs/heads/gone", oid("gone-0"), ZeroOID),
			move("refs/tags/v1", oid("v1"), ZeroOID),
			move("refs/heads/new", ZeroOID, oid("new")),
			refused,
			forced("refs/heads/turns", oid("X"), oid("Y"))),
		pushEntry("p2", forced("refs/heads/turns", oid("Y"), oid("X"))),
		pushEntry("p3", forced("refs/heads/turns", oid("X"), oid("W"))))
	touched := e.applyCounting(record)

	at := codeAt(3).Packed()
	stamp := store.EncodeTime(applyClock)
	want := []refDropRow{
		{ref: "refs/heads/forced", old: oid("forced-0"), position: at, at: stamp},
		{ref: "refs/heads/gone", old: oid("gone-0"), position: at, at: stamp},
		{ref: "refs/heads/turns", old: oid("X"), position: at, at: stamp},
		{ref: "refs/heads/turns", old: oid("Y"), position: at, at: stamp},
		{ref: "refs/tags/v1", old: oid("v1"), position: at, at: stamp},
	}
	slices.SortFunc(want, func(a, b refDropRow) int {
		return strings.Compare(a.ref+" "+a.old, b.ref+" "+b.old)
	})
	if got := e.dropsOf("repo-a"); !slices.Equal(got, want) {
		t.Fatalf("the record left drops %v, want %v: one for the forced update, each deletion and each "+
			"object the turns moved the ref off, none for the fast-forward, the create, the refused "+
			"command or the redelivered push", got, want)
	}
	// SIX REFS (ff, forced, gone, v1, new, turns), the three new entries'
	// packs and push rows, the five drops and the counters — and no horizon
	// due, the tombstones being fresh.
	if wantTouched := 6 + 3 + 3 + len(want) + 1; touched != wantTouched {
		t.Fatalf("the record reports %d rows touched, want %d: its drops count in what it wrote",
			touched, wantTouched)
	}
	if got := e.str(`SELECT target FROM code_refs WHERE ref = 'refs/heads/kept'`); got != oid("kept-0") {
		t.Fatalf("kept holds %q: the redelivered push's deletion applied", got)
	}
}

// A REF DROP OUTLIVES ITS GRACE AND NO MORE: a push record of the repository
// stored exactly PruneGrace after the drop keeps it, one stored a microsecond
// later — the resolution the instant is stored at — removes it, and a drop
// inside the grace stays beside it; another repository's drop as old as the
// first is its own repository's to forget, at its next push.
//
// Mutation: remove a drop at its grace (`at <= ?`), and the push at the grace
// takes it; judge no grace, and the push past it keeps it; drop the
// repository from the expiry, and repo-b's goes with repo-a's.
func TestARefDropOutlivesItsGraceAndNoMore(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	t0 := applyClock
	e.apply(create(t, "repo-a", "a"), create(t, "repo-b", "b"),
		pushOf(t, "repo-a", pushEntry("a0", move("refs/heads/old", ZeroOID, oid("old")),
			move("refs/heads/young", ZeroOID, oid("young")))).at(t0),
		pushOf(t, "repo-b", pushEntry("b0", move("refs/heads/other", ZeroOID, oid("other")))).at(t0),
		pushOf(t, "repo-a", pushEntry("a1", move("refs/heads/old", oid("old"), ZeroOID))).at(t0),
		pushOf(t, "repo-b", pushEntry("b1", move("refs/heads/other", oid("other"), ZeroOID))).at(t0),
		pushOf(t, "repo-a", pushEntry("a2", forced("refs/heads/young", oid("young"), oid("younger")))).
			at(t0.Add(24*time.Hour)))
	refsOf := func(repo string) []string {
		var refs []string
		for _, d := range e.dropsOf(repo) {
			refs = append(refs, d.ref)
		}
		return refs
	}
	if got := refsOf("repo-a"); !slices.Equal(got, []string{"refs/heads/old", "refs/heads/young"}) {
		t.Fatalf("the premise: repo-a holds drops %v", got)
	}

	e.apply(pushOf(t, "repo-a", pushEntry("a3", move("refs/heads/at", ZeroOID, oid("at")))).at(t0.Add(PruneGrace)))
	if got := refsOf("repo-a"); !slices.Equal(got, []string{"refs/heads/old", "refs/heads/young"}) {
		t.Fatalf("a push PruneGrace after the drop left repo-a's drops %v — at its grace a drop is kept", got)
	}

	e.apply(pushOf(t, "repo-a", pushEntry("a4", move("refs/heads/past", ZeroOID, oid("past")))).
		at(t0.Add(PruneGrace + time.Microsecond)))
	if got := refsOf("repo-a"); !slices.Equal(got, []string{"refs/heads/young"}) {
		t.Fatalf("a push a microsecond past the old drop's grace left repo-a's drops %v, want the "+
			"young one alone", got)
	}
	if got := refsOf("repo-b"); !slices.Equal(got, []string{"refs/heads/other"}) {
		t.Fatalf("repo-a's pushes left repo-b's drops %v — a record forgets only its own repository's", got)
	}
}

// THE REF DROPS TAKE WHAT THE TOMBSTONES LEAVE: with more expired tombstones
// waiting than one record may sweep and expired drops beside them, a push
// record sweeps a whole budget of tombstones and forgets no drop; the next,
// with fewer tombstones due than its budget, sweeps them and forgets drops
// with the rest — every row of the budget touched by the first, and never one
// past it by either. A drop forgotten late costs a row; a tombstone's room
// taken costs every git node a full read.
//
// Mutation: forget the drops before the sweep, and the first record forgets
// them all and leaves the tombstones a thousand rows short.
func TestTheRefDropsTakeWhatTheTombstonesLeave(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	const stones, drops = MaxRecordRows + 500, 1_000
	e.exec(`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i + 1 < ?)
		INSERT INTO code_refs (repo_id, ref, target, position, at)
		SELECT 'repo-a', printf('refs/heads/gone-%06d', i), '', 1, 0 FROM n`, stones)
	e.exec(`UPDATE code_repos SET tombstones = ? WHERE id = 'repo-a'`, stones)
	e.seedDrops("repo-a", 1, time.Unix(0, 0), drops)

	// A REF, A PACK, A PUSH ROW AND THE COUNTERS, then the sweep's whole
	// budget and its mark.
	if written := e.applyCounting(pushOf(t, "repo-a", pushEntry("p1",
		move("refs/heads/one", ZeroOID, oid("one"))))); written != MaxRecordRows {
		t.Fatalf("the first record touched %d rows, want its whole budget of %d", written, MaxRecordRows)
	}
	swept := MaxRecordRows - 4 - 1
	if left := e.num(`SELECT COUNT(*) FROM code_refs WHERE target = ''`); left != int64(stones-swept) {
		t.Fatalf("%d tombstones left after the first record, want %d: the sweep takes the budget first",
			left, stones-swept)
	}
	if left := e.num(`SELECT COUNT(*) FROM code_ref_drops`); left != drops {
		t.Fatalf("the first record forgot %d drops with no row of its budget left for them", drops-left)
	}

	written := e.applyCounting(pushOf(t, "repo-a", pushEntry("p2", move("refs/heads/two", ZeroOID, oid("two")))))
	if want := 4 + (stones - swept + 1) + drops; written != want {
		t.Fatalf("the second record touched %d rows, want %d: its four, the %d tombstones left and their "+
			"mark, and every drop past its grace", written, want, stones-swept)
	}
	if left := e.num(`SELECT COUNT(*) FROM code_ref_drops`); left != 0 {
		t.Fatalf("%d drops left past their grace by a record with room for them all", left)
	}
	e.countersMatchRows("repo-a")
}

// THE DROPS' EXPIRY KEEPS PACE WITH BULK FORCING: a repository whose every
// push force-moves a thousand branches — a mirror pushed with --force, a
// tool rewriting its branches — holds, once each push is past the grace of
// the one two before it, only the drops of the last two: each record forgets
// the thousand that came due with what its rows leave, nearly two thousand. A
// fixed share of MaxRecordRows/8, five hundred a record, fell behind it by
// five hundred a record for as long as such pushes come.
//
// Mutation: cap the expiry at MaxRecordRows/8, and the drops grow by five
// hundred a round.
func TestTheRefDropExpiryKeepsPaceWithBulkForcing(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	const branches, rounds = MaxCommandsPerPush, 8
	tip := func(round, i int) string { return oid(fmt.Sprint(round, "-", i)) }
	var made []Command
	for i := range branches {
		made = append(made, move(fmt.Sprintf("refs/heads/b%04d", i), ZeroOID, tip(0, i)))
	}
	at := applyClock
	e.apply(pushOf(t, "repo-a", pushEntry("make", made...)).at(at))
	// HALF THE GRACE AND AN HOUR A ROUND: a round's drops are past their
	// grace two rounds on, and not one.
	step := PruneGrace/2 + time.Hour
	for round := 1; round <= rounds; round++ {
		var moved []Command
		for i := range branches {
			moved = append(moved, forced(fmt.Sprintf("refs/heads/b%04d", i), tip(round-1, i), tip(round, i)))
		}
		at = at.Add(step)
		e.apply(pushOf(t, "repo-a", pushEntry(fmt.Sprint("force-", round), moved...)).at(at))
		want := min(round, 2) * branches
		if got := e.num(`SELECT COUNT(*) FROM code_ref_drops WHERE repo_id = 'repo-a'`); got != int64(want) {
			t.Fatalf("after round %d the repository holds %d drops, want %d: the last two rounds', every "+
				"older one forgotten by the record it came due under", round, got, want)
		}
	}
}
