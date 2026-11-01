package code_test

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/store"
)

// RETIRED PACKS AND THEIR SWEEP, on the whole path: a real broker, store and
// applier. The retired rows are SEEDED, each case's at the positions and
// broker instants it is about, because what a sweep decides is a question
// about those two columns and never about the record that wrote them.

// seedRetired writes count retired pack rows of repoID straight into a node's
// estate, all retired by one record — at the packed position on the code log
// and its broker instant at — and answers the objects they name.
func (n *node) seedRetired(repoID string, position int64, at time.Time, count int) []objstore.Key {
	n.t.Helper()
	packs := make([]string, count)
	for i := range packs {
		packs[i] = uuid.NewString()
	}
	return n.seedRetiredPacks(repoID, position, at, packs...)
}

// seedRetiredPacks is [node.seedRetired] for packs a case names: one retired
// row a pack, part `pack`, its object keyOf and its git checksum oidOf the
// pack's id.
func (n *node) seedRetiredPacks(repoID string, position int64, at time.Time, packs ...string) []objstore.Key {
	n.t.Helper()
	ctx := n.t.Context()
	keys := make([]objstore.Key, len(packs))
	if err := n.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := store.InsertRows(ctx, tx, n.db.Caps().MaxVariables,
			`INSERT INTO code_packs_retired (repo_id, pack_id, part, object, hash, size, index_bytes,
				reverse_bytes, git_checksum, objects, kind, added_position, retired_position,
				retired_at) VALUES `,
			`(?, ?, 'pack', ?, ?, 100, 0, 0, ?, 1, 'push', 1, ?, ?)`, ``, len(packs),
			func(i int) []any {
				pack := packs[i]
				keys[i] = keyOf(pack)
				return []any{repoID, pack, keys[i].String(), string(objstore.HashOf([]byte(pack))),
					oidOf(pack), position, store.EncodeTime(at)}
			})
		return err
	}); err != nil {
		n.t.Fatalf("seed %s's retired packs: %v", repoID, err)
	}
	return keys
}

// retiredAt is how many retired rows of repoID this node holds at the packed
// position.
func (n *node) retiredAt(repoID string, position int64) int {
	n.t.Helper()
	return n.count(`SELECT COUNT(*) FROM code_packs_retired WHERE repo_id = ? AND retired_position = ?`,
		repoID, position)
}

// retiredThrough is repoID's retired mark in this node's rows.
func (n *node) retiredThrough(repoID string) int {
	n.t.Helper()
	return n.count(`SELECT retired_through FROM code_repos WHERE id = ?`, repoID)
}

// A RETIRED PACK KEEPS ITS OBJECT FOR THE GRACE (ADR-0074): while its row is in
// code_packs_retired the collector reads its object as named — through the
// declaration and the statement it builds, as every collection asks — and a
// sweep removes the row only once the clock it is given puts the row past
// RetireGrace, after which nothing names the object and the collector takes
// it as it takes anything no row names.
//
// The rows are seeded first, at the instant the case is about, and then
// retired by a real compaction's record, whose instant is the broker's own.
//
// Mutation: declare RetiredPackReferences over code_packs, and the retired
// objects read as nobody's while their rows stand; let the sweep's apply
// delete nothing, and they stay named past it; drop the decide's cutoff, and
// the sweep a second inside the grace removes them; let a compaction's apply
// skip the retired rows, and the packs it replaced read as nobody's at once.
func TestARetiredPackKeepsItsObjectForTheGrace(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	retiredAt := harnessAt.Add(-time.Hour)
	keys := n.seedRetired(id, 1, retiredAt, 2)
	sources, err := collect.Sources([]objstore.ReferenceTable{code.RetiredPackReferences},
		code.ObjectEstate{Reader: n.reader})
	if err != nil {
		t.Fatalf("the collector's sources: %v", err)
	}
	src := sources[0]
	named := func() int {
		t.Helper()
		got, complete, err := src.Referenced(t.Context(), keys, n.waiter.Committed())
		if err != nil || !complete {
			t.Fatalf("Referenced = %v (complete %v)", err, complete)
		}
		return len(got)
	}
	if got := named(); got != len(keys) {
		t.Fatalf("the collector reads %d of the %d retired objects as named — it would delete "+
			"the rest under a read that began before their retirement", got, len(keys))
	}

	// A SECOND INSIDE THE GRACE the sweep decides nothing, and writes nothing.
	before := n.end()
	res, err := n.store.RetireSweep(t.Context(), id, retiredAt.Add(code.RetireGrace-time.Second))
	if err != nil || res.Rows != 0 || n.end() != before {
		t.Fatalf("a sweep a second inside the grace = %+v, %v (log %d → %d), want nothing written",
			res, err, before, n.end())
	}
	if got := named(); got != len(keys) {
		t.Fatalf("inside the grace %d of the %d retired objects are named, want all", got, len(keys))
	}

	// AT THE GRACE the rows go in one record, and nothing names the objects.
	res, err = n.store.RetireSweep(t.Context(), id, retiredAt.Add(code.RetireGrace))
	if err != nil || res.Rows != len(keys) || res.More || !res.Held() || res.Position.IsZero() {
		t.Fatalf("the sweep at the grace = %+v, %v; want both rows removed by one applied record",
			res, err)
	}
	if got := named(); got != 0 {
		t.Fatalf("after the sweep %d retired objects are still named — the collector would keep "+
			"them for ever", got)
	}
	if through := n.retiredThrough(id); through != 1 {
		t.Fatalf("the repository's retired mark is %d after the sweep through position 1", through)
	}

	// AND RETIRED BY A COMPACTION'S OWN RECORD: the packs its output replaces
	// keep their objects named until a sweep past the grace — judged against
	// the instant the broker stamped the compaction's record with — and a
	// sweep a second short of it leaves them named.
	repo, ids := packedRepo(t, n, "web", 2)
	inputs := n.packObjects(repo)
	c, _ := committerOn(t, n, n.pub)
	compacted, err := c.ProposeCompaction(t.Context(), compactionOf(n, repo, ids, 2, harnessAt))
	requireApplied(t, compacted, err)
	retiredBy := func() int {
		t.Helper()
		got, complete, err := src.Referenced(t.Context(), inputs, n.waiter.Committed())
		if err != nil || !complete {
			t.Fatalf("Referenced = %v (complete %v)", err, complete)
		}
		return len(got)
	}
	if got := retiredBy(); got != len(inputs) {
		t.Fatalf("the collector reads %d of the %d packs a compaction retired as named, want all", got,
			len(inputs))
	}
	var stamped int64
	if err := n.db.Reader().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT MAX(retired_at) FROM code_packs_retired WHERE repo_id = ?`,
			repo).Scan(&stamped)
	}); err != nil {
		t.Fatal(err)
	}
	if res, err := n.store.RetireSweep(t.Context(), repo,
		store.DecodeTime(stamped).Add(code.RetireGrace-time.Second)); err != nil || res.Rows != 0 {
		t.Fatalf("a sweep a second inside the compaction's grace = %+v, %v; want nothing", res, err)
	}
	if res, err := n.store.RetireSweep(t.Context(), repo,
		store.DecodeTime(stamped).Add(code.RetireGrace)); err != nil || res.Rows != len(inputs) {
		t.Fatalf("the sweep at the compaction's grace = %+v, %v; want its %d retirements", res, err, len(inputs))
	}
	if got := retiredBy(); got != 0 {
		t.Fatalf("after the sweep %d packs the compaction retired are still named", got)
	}
}

// packObjects is the object every live pack of repo names on n.
func (n *node) packObjects(repo string) []objstore.Key {
	n.t.Helper()
	var out []objstore.Key
	if err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(n.t.Context(), `SELECT object FROM code_packs WHERE repo_id = ?`, repo)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var object string
			if err := rows.Scan(&object); err != nil {
				return err
			}
			k, err := objstore.ParseKey(object)
			if err != nil {
				return err
			}
			out = append(out, k)
		}
		return rows.Err()
	}); err != nil {
		n.t.Fatalf("read %s's pack objects: %v", repo, err)
	}
	return out
}

// A SWEEP DECIDES NO ROW STILL INSIDE THE GRACE. Its walk takes a repository's
// retired rows oldest position first and stops at the first its clock does not
// put past RetireGrace — so a retirement recorded after one still inside the
// grace but stamped before it (a leader change steps the broker's clock back)
// waits for the next pass rather than going with it — and the record's bound
// never cuts a retirement: a position it would cut is left whole for the next
// record, and one alone past the bound is no writer's.
//
// Mutation: walk on past a row inside the grace, and that row is swept with
// the one behind it; drop the bound's take-back, and a record removes part of
// a retirement and marks it whole; drop the bound's refusal, and a decide
// claims more rows than one record may remove.
func TestASweepDecidesNoRowStillInsideTheGrace(t *testing.T) {
	t.Parallel()
	old := harnessAt.Add(-code.RetireGrace - time.Hour)
	t.Run("the walk stops at the first row inside the grace", func(t *testing.T) {
		t.Parallel()
		n := newNode(t)
		id := seededRepo(t, n, "app")
		// FOUR RECORDS on the log, each the retirement of the rows seeded at
		// its position: the third stamped inside the grace, the fourth — a
		// leader change behind it — stamped as old as the first two.
		var at [5]int
		for i := 1; i <= 4; i++ {
			res := n.push(id, entry(fmt.Sprintf("p%d", i),
				set(fmt.Sprintf("refs/heads/b%d", i), code.ZeroOID, oidOf(fmt.Sprint("c", i)))))
			at[i] = int(res.Position.Packed())
		}
		n.seedRetired(id, int64(at[1]), old, 2)
		n.seedRetired(id, int64(at[2]), old, 3)
		n.seedRetired(id, int64(at[3]), harnessAt.Add(-time.Hour), 1)
		n.seedRetired(id, int64(at[4]), old, 2)
		res, err := n.store.RetireSweep(t.Context(), id, harnessAt)
		if err != nil || res.Rows != 5 || res.More {
			t.Fatalf("the sweep = %+v, %v; want the five rows of the first two retirements, and no more",
				res, err)
		}
		left := []int{n.retiredAt(id, int64(at[1])), n.retiredAt(id, int64(at[2])),
			n.retiredAt(id, int64(at[3])), n.retiredAt(id, int64(at[4]))}
		if !slices.Equal(left, []int{0, 0, 1, 2}) {
			t.Fatalf("after the sweep the four retirements hold %v rows; want [0 0 1 2]: nothing at or "+
				"past the row still inside the grace", left)
		}
		if through := n.retiredThrough(id); through != at[2] {
			t.Fatalf("the retired mark is %d, want the second retirement's position %d", through, at[2])
		}
	})
	t.Run("a retirement the bound would cut waits whole", func(t *testing.T) {
		t.Parallel()
		n := newNode(t)
		id := seededRepo(t, n, "app")
		n.seedRetired(id, 1, old, code.RetireSweepRows-1)
		n.seedRetired(id, 2, old, 3)
		res, err := n.store.RetireSweep(t.Context(), id, harnessAt)
		if err != nil || res.Rows != code.RetireSweepRows-1 || !res.More {
			t.Fatalf("the first sweep = %+v, %v; want position 1's %d rows and More, position 2's "+
				"three past the bound", res, err, code.RetireSweepRows-1)
		}
		if n.retiredAt(id, 1) != 0 || n.retiredAt(id, 2) != 3 || n.retiredThrough(id) != 1 {
			t.Fatalf("after the first sweep positions 1 and 2 hold %d and %d rows, marked through %d; "+
				"want 0 and 3, through 1", n.retiredAt(id, 1), n.retiredAt(id, 2), n.retiredThrough(id))
		}
		res, err = n.store.RetireSweep(t.Context(), id, harnessAt)
		if err != nil || res.Rows != 3 || res.More || n.retiredAt(id, 2) != 0 || n.retiredThrough(id) != 2 {
			t.Fatalf("the second sweep = %+v, %v (position 2 holds %d, marked through %d); want its "+
				"three rows, through 2", res, err, n.retiredAt(id, 2), n.retiredThrough(id))
		}
	})
	t.Run("a retirement past the bound is no writer's", func(t *testing.T) {
		t.Parallel()
		n := newNode(t)
		id := seededRepo(t, n, "app")
		n.seedRetired(id, 1, old, code.RetireSweepRows+1)
		before := n.end()
		res, err := n.store.RetireSweep(t.Context(), id, harnessAt)
		if err == nil || !strings.Contains(err.Error(), "more than one sweep record removes") {
			t.Fatalf("the sweep of one retirement past the bound = %+v, %v; want a refusal naming it", res, err)
		}
		if n.end() != before || n.retiredAt(id, 1) != code.RetireSweepRows+1 {
			t.Fatalf("the refused sweep moved the log %d → %d and left %d rows", before, n.end(),
				n.retiredAt(id, 1))
		}
	})
}

// opsAfter is every record on the node's log after the sequence after, each as
// its op and its subject — less the read index's barriers and the trim's
// records.
func (n *node) opsAfter(after uint64) []string {
	n.t.Helper()
	var out []string
	for seq := after + 1; seq <= n.end(); seq++ {
		_, payload, _, ok, err := n.records.At(n.t.Context(), seq)
		if err != nil || !ok {
			n.t.Fatalf("read record %d: held %v, %v", seq, ok, err)
		}
		rec, err := code.Decode(payload)
		if err != nil {
			n.t.Fatalf("decode record %d: %v", seq, err)
		}
		if rec.Op == code.OpBarrier || rec.Op == code.OpTrimmed {
			continue
		}
		out = append(out, fmt.Sprintf("%s %s", rec.Op, rec.Subject))
	}
	return out
}

// THE RETENTION PASS SWEEPS BEFORE IT PURGES: a pass that finds one repository
// holding retired packs past their grace and another deleted past its
// retention publishes the sweep first — one cheap record, and every hour a
// retired row outlives its grace is an object the collector cannot take — and
// reports both: the sweep's record and the rows it removed beside the purge.
//
// Mutation: step the purges first, and the purge's record comes before the
// sweep's; leave the sweep out of the report, and the pass says it swept
// nothing.
func TestTheRetentionPassSweepsBeforeItPurges(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("kept", described("kept"), t1)
	n.ensure("gone", described("gone"), t1)
	kept, gone := n.mustID("kept"), n.mustID("gone")
	n.deleteAt(gone, harnessAt.Add(-2*keep))
	n.seedRetired(kept, 1, harnessAt.Add(-code.RetireGrace-time.Hour), 3)
	before := n.end()

	var p purged
	report, err := passOver(n.store, n.reader, &p).Pass(t.Context())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if report.Sweeps != 1 || report.SweptRows != 3 || !slices.Equal(report.Purged, []string{"gone"}) ||
		report.Steps != 1 || len(report.Failed) != 0 || report.Remain {
		t.Fatalf("the pass reported %+v; want one sweep of three rows, and gone purged in one step", report)
	}
	want := []string{
		fmt.Sprintf("%s %s", code.OpRetireSweep, code.RepoSubject(kept)),
		fmt.Sprintf("%s %s", code.OpPurge, code.RepoSubject(gone)),
	}
	if got := n.opsAfter(before); !slices.Equal(got, want) {
		t.Fatalf("the pass appended %v, want %v: the sweep first", got, want)
	}
	if n.retiredAt(kept, 1) != 0 {
		t.Fatalf("kept still holds %d retired rows past their grace", n.retiredAt(kept, 1))
	}
}

// A SWEEP STEP THAT FAILS IS REPORTED BY THE REPOSITORY'S NAME, as a purge
// step's is, and the pass goes on: the listing names the repository by its
// id, and the report an operator reads is keyed by name.
//
// Mutation: report the id, and the failure is keyed by something no person
// declared.
func TestAFailedSweepIsReportedByTheRepositorysName(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("stuck", described("stuck"), t1)
	n.ensure("freed", described("freed"), t1)
	stuck, freed := n.mustID("stuck"), n.mustID("freed")
	old := harnessAt.Add(-code.RetireGrace - time.Hour)
	n.seedRetired(stuck, 1, old, 1)
	n.seedRetired(freed, 2, old, 1)
	s, err := code.NewStore(code.StoreOptions{
		Publisher: refusingOne{Publisher: n.pub, subject: code.RepoSubject(stuck)},
		DB:        n.db.Reader(), NodeID: n.id, Now: func() time.Time { return harnessAt },
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	var p purged
	report, err := passOver(s, n.reader, &p).Pass(t.Context())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if report.Sweeps != 1 || report.SweptRows != 1 || len(report.Failed) != 1 ||
		!strings.Contains(report.Failed["stuck"], "the broker refused this record") {
		t.Fatalf("the pass reported %+v; want freed swept, and stuck failed by its name", report)
	}
	if n.retiredAt(stuck, 1) != 1 || n.retiredAt(freed, 2) != 0 {
		t.Fatalf("stuck holds %d and freed %d retired rows; want 1 and 0", n.retiredAt(stuck, 1),
			n.retiredAt(freed, 2))
	}
}

// THE RETIRED LISTING NAMES EACH RETIRING RECORD ONCE, however many packs it
// retired: a record's rows share its broker instant and its repository, and a
// sweep step removes a record whole, so an entry a record is at most one step
// of the pass's and a page of the listing is a page of the pass's work rather
// than of one record's rows. Read two entries a page, a record that retired
// five packs is one entry, the records after it fill the page in their
// instants' order, and a record still inside the grace is no entry at all.
//
// Mutation: list the rows rather than the records, and the first page names
// the five-pack record twice; drop the cutoff, and the record inside the
// grace is listed.
func TestTheRetiredListingNamesEachRetiringRecordOnce(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	a, b := seededRepo(t, n, "a"), seededRepo(t, n, "b")
	old := harnessAt.Add(-code.RetireGrace - time.Hour)
	n.seedRetired(a, 1, old, 5)
	n.seedRetired(b, 2, old.Add(time.Minute), 1)
	n.seedRetired(a, 3, old.Add(2*time.Minute), 2)
	n.seedRetired(b, 4, harnessAt.Add(-time.Hour), 1)
	pages, err := code.RetiredDuePages(t.Context(), n.reader, harnessAt.Add(-code.RetireGrace), 2)
	if err != nil {
		t.Fatalf("RetiredDuePages: %v", err)
	}
	if want := [][]string{{a, b}, {a}}; !slices.EqualFunc(pages, want, slices.Equal[[]string]) {
		t.Fatalf("the listing read %v two entries a page, want %v: one entry for each retiring "+
			"record past the grace, oldest first", pages, want)
	}
}

// A PURGE REMOVES A REPOSITORY'S RETIRED ROWS, whatever their grace — no read
// begins on a deleted repository — and counts them toward its parts: a deleted
// repository holding nothing but retired packs, one past a part's rows, is
// purged in a part and then the purge, never by one unbounded statement, and
// no retired row of it is left.
//
// Mutation: leave the retired rows out of the bounded count, and the purge is
// one record deleting all of them at once; drop the part's delete, and the
// parts remove nothing.
func TestAPurgeRemovesARepositorysRetiredRows(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("app", described("one"), t1)
	id := n.mustID("app")
	n.deleteAt(id, harnessAt.Add(-2*keep))
	n.seedRetired(id, 1, harnessAt.Add(-time.Hour), code.PurgePartRows+1)
	retired := func() int { return n.count(`SELECT COUNT(*) FROM code_packs_retired WHERE repo_id = ?`, id) }
	var steps []int
	for range 3 {
		left := retired()
		res, err := n.store.PurgeStep(t.Context(), id, keep)
		if err != nil {
			t.Fatalf("PurgeStep: %v", err)
		}
		steps = append(steps, left-retired())
		if res.Done {
			break
		}
	}
	if want := []int{code.PurgePartRows, 1}; !slices.Equal(steps, want) {
		t.Fatalf("the purge removed %v retired rows a record, want %v: a part of PurgePartRows (%d), "+
			"then the purge", steps, want, code.PurgePartRows)
	}
	if left := retired(); left != 0 {
		t.Fatalf("the purge left %d retired rows of the repository", left)
	}
	if _, found := n.repo("app"); found {
		t.Fatal("the repository still has a row after its purge")
	}
}
