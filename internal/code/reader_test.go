package code_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// stale is a read of whatever this node holds.
var stale = statelog.Freshness{Level: statelog.ReadStale}

// snapshot follows a snapshot from page 1 to its end on one reader, answering
// every page.
func snapshot(t *testing.T, r *code.Reader, repo string, since statelog.Position) []code.RepoSnapshot {
	t.Helper()
	pages, err := snapshotErr(t, r, repo, since)
	if err != nil {
		t.Fatalf("snapshot %s: %v", repo, err)
	}
	return pages
}

// snapshotErr is [snapshot], answering the first error a page met.
func snapshotErr(t *testing.T, r *code.Reader, repo string, since statelog.Position) ([]code.RepoSnapshot, error) {
	t.Helper()
	var pages []code.RepoSnapshot
	q := code.StateQuery{RepoID: repo, Since: since, Fresh: stale}
	for range 10_000 {
		page, err := r.Snapshot(t.Context(), q)
		if err != nil {
			return pages, err
		}
		pages = append(pages, page)
		if page.Next == nil {
			return pages, nil
		}
		q.Cursor = page.Next
	}
	t.Fatal("a snapshot did not end in ten thousand pages")
	return nil, nil
}

// union is every pack and ref a snapshot's pages served: packs by id and
// part, refs by name at the LAST value served (a delta serves a ref once, a
// full snapshot once; a repeat is a bug the callers check for separately).
type union struct {
	packs map[string]code.PackRow
	refs  map[string]string
}

func unionOf(pages []code.RepoSnapshot) union {
	u := union{packs: map[string]code.PackRow{}, refs: map[string]string{}}
	for _, p := range pages {
		for _, k := range p.Packs {
			u.packs[k.PackID+"/"+k.Part] = k
		}
		for _, r := range p.Refs {
			u.refs[r.Ref] = r.OID
		}
	}
	return u
}

// seededRepo is a repository ensured on n and its id.
func seededRepo(t *testing.T, n *node, name string) string {
	t.Helper()
	return n.ensure(name, described(name), t1).RepoID
}

// A SNAPSHOT WITH NO POSITION IS FULL, and so is one from a position on
// another generation of the log: a delta is only ever from a position this
// history contains.
func TestASnapshotIsFullWithoutAPosition(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	n.push(id, entry("p1", set("refs/heads/main", code.ZeroOID, oidOf("c1"))))
	pages := snapshot(t, n.reader, id, statelog.Position{})
	if len(pages) != 1 || !pages[0].Full || len(pages[0].Packs) != 1 || len(pages[0].Refs) != 1 {
		t.Fatalf("a snapshot with no position = %+v, want one full page of the pack and the ref", pages)
	}
	if pages[0].Position != n.waiter.Committed() || pages[0].StreamCreated.IsZero() {
		t.Fatalf("the snapshot is as of %v on a stream created %v, want this node's %v and the "+
			"stream's identity", pages[0].Position, pages[0].StreamCreated, n.waiter.Committed())
	}
	other := pages[0].Position
	other.Generation += 7
	if again := snapshot(t, n.reader, id, other); !again[0].Full {
		t.Fatal("a snapshot from a position on another generation was a delta")
	}
}

// A DELTA CARRIES EXACTLY WHAT MOVED SINCE ITS POSITION: the packs added above
// it and the refs moved above it, a deletion as a ref with no object — and
// nothing older.
func TestADeltaCarriesExactlyWhatMovedSince(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	n.push(id, entry("p1", set("refs/heads/main", code.ZeroOID, oidOf("c1")),
		set("refs/heads/gone", code.ZeroOID, oidOf("g1"))))
	since := n.waiter.Committed()
	n.push(id, entry("p2", set("refs/heads/main", oidOf("c1"), oidOf("c2")),
		set("refs/heads/gone", oidOf("g1"), code.ZeroOID)))
	pages := snapshot(t, n.reader, id, since)
	if pages[0].Full {
		t.Fatal("a snapshot from a position this history holds was full")
	}
	u := unionOf(pages)
	if len(u.packs) != 1 || u.packs["push-p2/pack"].PackID != "push-p2" {
		t.Fatalf("the delta's packs are %v, want p2's alone", u.packs)
	}
	if len(u.refs) != 2 || u.refs["refs/heads/main"] != oidOf("c2") || u.refs["refs/heads/gone"] != "" {
		t.Fatalf("the delta's refs are %v, want main at c2 and gone as a deletion", u.refs)
	}
}

// A SNAPSHOT IS FULL ONCE A DELETION ITS DELTA NEEDED WAS SWEPT: below the
// repository's tombstone mark a delta can no longer state every deletion, so
// it is answered in full — and the deleted ref is simply absent.
func TestASnapshotIsFullAfterTombstonesAreSwept(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	n.push(id, entry("p1", set("refs/heads/main", code.ZeroOID, oidOf("c1")),
		set("refs/heads/gone", code.ZeroOID, oidOf("g1"))))
	n.push(id, entry("p2", set("refs/heads/gone", oidOf("g1"), code.ZeroOID)))
	deleted := n.waiter.Committed()
	before := deleted
	before.Seq--

	sweep := time.Now().Add(code.RefTombstoneHorizon + time.Hour)
	n.storedAt = func(uint64, time.Time) time.Time { return sweep }
	n.push(id, entry("p3", set("refs/heads/main", oidOf("c1"), oidOf("c3"))))
	n.storedAt = nil

	pages := snapshot(t, n.reader, id, before)
	if !pages[0].Full {
		t.Fatal("a delta from below the swept tombstone was answered as a delta")
	}
	if _, present := unionOf(pages).refs["refs/heads/gone"]; present {
		t.Fatal("the full snapshot serves the deleted ref")
	}
	if delta := snapshot(t, n.reader, id, deleted); delta[0].Full {
		t.Fatal("a delta from at the tombstone mark was answered in full")
	}
}

// A DELTA IS NEVER LARGER THAN THE FULL ANSWER IT WOULD REPLACE: a delta from
// below a clean-up that deleted more refs than the repository keeps carries a
// row for every deletion, where the full answer carries only what is left, so
// that delta is answered in full — while a delta from after it, carrying one
// push's change, is still a delta. And the comparison is of every row each
// answer carries, PACKS INCLUDED: a delta's packs are only those added since
// it, a full answer's every pack, so a repository with many packs keeps its
// delta past a clean-up a repository with few would not. RETIREMENTS are
// weighed LIKE WITH LIKE: a delta carries every pack retired above its floor,
// and the full answer it stands in for those above its caller's RetiredSince
// when it can serve every one, so each side is weighed with the retirements
// it would carry — those both would carry cancel, and those only one would
// carry weigh on its side alone.
//
// Mutation: drop the count, and the delta from below the clean-up is a delta
// of sixty tombstones beside a full answer of forty refs; compare the refs
// alone, and the repository of many packs is answered in full — 343 rows — for
// a delta of 63, under a fifth of its size; leave the retirements out of the
// count, and a delta of a ref, a pack and two retirements stands in for a full
// answer of three rows that serves none of them; count them on the delta's
// side alone, and the delta asked from its own floor is replaced by a full
// answer carrying them again beside its rows; leave out those the full answer
// would serve from below the delta's floor, and a delta of 63 rows is replaced
// by a full answer of 63.
func TestADeltaLargerThanTheFullAnswerIsAnsweredInFull(t *testing.T) {
	t.Parallel()
	const made, deleted = 100, 60
	name := func(i int) string { return fmt.Sprintf("refs/heads/b%03d", i) }
	cleanedUp := func(t *testing.T, packs int) (n *node, id string, before, after statelog.Position) {
		n = newNode(t)
		id = seededRepo(t, n, "app")
		n.seedRefs(id, 0, packs)
		var create []code.Command
		for i := range made {
			create = append(create, set(name(i), code.ZeroOID, oidOf(name(i))))
		}
		n.push(id, entry("made", create...))
		before = n.waiter.Committed()
		var drop []code.Command
		for i := range deleted {
			drop = append(drop, set(name(i), oidOf(name(i)), code.ZeroOID))
		}
		n.push(id, entry("cleaned", drop...))
		after = n.waiter.Committed()
		n.push(id, entry("moved", set(name(made-1), oidOf(name(made-1)), oidOf("moved"))))
		return n, id, before, after
	}

	t.Run("few packs", func(t *testing.T) {
		t.Parallel()
		n, id, before, after := cleanedUp(t, 0)
		pages := snapshot(t, n.reader, id, before)
		if !pages[0].Full {
			t.Fatalf("a delta from below a clean-up of %d refs, of a repository left with %d, was "+
				"answered as a delta of %d refs where the full answer is %d", deleted, made-deleted,
				len(unionOf(pages).refs), made-deleted)
		}
		u := unionOf(pages)
		if len(u.refs) != made-deleted || len(u.packs) != 3 {
			t.Fatalf("the full answer serves %d refs and %d packs, want the %d live and all 3",
				len(u.refs), len(u.packs), made-deleted)
		}
		for ref, oid := range u.refs {
			if oid == "" {
				t.Fatalf("the full answer serves %s as a deletion", ref)
			}
		}
		delta := snapshot(t, n.reader, id, after)
		if delta[0].Full {
			t.Fatal("a delta of one push's change was answered in full")
		}
		if u := unionOf(delta); len(u.refs) != 1 || len(u.packs) != 1 {
			t.Fatalf("the delta from after the clean-up serves %d refs and %d packs, want the "+
				"one ref and the one pack the push after it moved", len(u.refs), len(u.packs))
		}
	})

	// THE FULL ANSWER IS WEIGHED WITH EVERY RETIREMENT IT WOULD SERVE, those
	// the delta does not carry too: asked for them from below the delta's
	// floor, the full answer standing in for the delta of the clean-up's 63
	// rows carries its own 43 and every retirement between the two floors, so
	// the delta is answered in full only while those are fewer than the 20
	// rows it carries past the full answer's own — and a tie is a delta.
	//
	// Mutation: leave the full answer's retirements out of the weighing when
	// its caller asked them from another floor than the delta's, and the delta
	// of 63 rows is replaced by a full answer of 63.
	t.Run("retirements below the delta's floor", func(t *testing.T) {
		t.Parallel()
		n, id, before, _ := cleanedUp(t, 0)
		below := before
		below.Seq--
		ask := func() []code.RepoSnapshot {
			t.Helper()
			pages, err := snapshotFrom(t, n.reader,
				code.StateQuery{RepoID: id, Since: before, Fresh: stale, RetiredSince: below})
			if err != nil {
				t.Fatalf("the snapshot from %v asking for the retirements above %v: %v", before, below, err)
			}
			return pages
		}
		// Retired AT the delta's floor: above the caller's RetiredSince, so
		// the full answer's, and not above Since, so never the delta's.
		var between []string
		for i := range 20 {
			between = append(between, fmt.Sprintf("between-%02d", i))
		}
		n.seedRetiredPacks(id, before.Packed(), harnessAt, between[:19]...)
		pages := ask()
		if !pages[0].Full || pages[0].RetiredFrom != below || len(retiredOf(pages)) != 19 {
			t.Fatalf("a delta of 63 rows beside a full answer of 43 and the 19 retirements between the "+
				"floors = full %v serving %d from %v, want the full answer serving all 19 from %v",
				pages[0].Full, len(retiredOf(pages)), pages[0].RetiredFrom, below)
		}
		n.seedRetiredPacks(id, before.Packed(), harnessAt, between[19])
		pages = ask()
		if pages[0].Full || pages[0].RetiredFrom != before || len(retiredOf(pages)) != 0 {
			t.Fatalf("a delta of 63 rows beside a full answer of 43 and the 20 retirements between the "+
				"floors = full %v serving %d from %v, want the delta, which carries none of them, from %v",
				pages[0].Full, len(retiredOf(pages)), pages[0].RetiredFrom, before)
		}
	})

	t.Run("many packs", func(t *testing.T) {
		t.Parallel()
		// Three hundred seeded packs, and one for each of the three pushes.
		const packs = 300 + 3
		n, id, before, _ := cleanedUp(t, 300)
		pages := snapshot(t, n.reader, id, before)
		if live := pages[0].Repo.LivePacks; live != packs {
			t.Fatalf("the repository counts %d live packs, want the %d it holds", live, packs)
		}
		if pages[0].Full {
			t.Fatalf("a delta of %d refs and 2 packs beside a full answer of %d refs and %d "+
				"packs was answered in full", deleted+1, made-deleted, packs)
		}
		if u := unionOf(pages); len(u.refs) != deleted+1 || len(u.packs) != 2 {
			t.Fatalf("the delta serves %d refs and %d packs, want the %d the clean-up deleted with "+
				"the one moved after it, and the 2 packs added since", len(u.refs), len(u.packs),
				deleted+1)
		}
	})

	// THE RETIREMENTS ARE WEIGHED LIKE WITH LIKE: where the full answer
	// would serve the same retirements — its caller asked from the delta's
	// own floor, the position a git node's cache holds — they sit on both
	// sides and cancel; only those the full answer would not serve count
	// against the delta alone.
	//
	// WHAT A FULL ANSWER SERVES IS BOUNDED BY MaxFullRetirements, never by
	// its own rows: a repository of few refs, whose compaction retires more
	// packs than it has rows, still has its retirements served, and only
	// past the bound does a full answer serve none.
	//
	// Mutations: count the retirements on the delta's side alone, and the
	// delta asked from its own floor is answered in full — the full answer
	// then carrying those retirements AND its own rows, more than the delta
	// it replaced; bound them by the full answer's rows, and the twenty a
	// compaction retired beside three rows are served by no full answer and
	// make the delta full.
	t.Run("many retirements", func(t *testing.T) {
		t.Parallel()
		// A full answer of one ref and two packs; a delta from after the
		// first push of the ref, the second push's pack and its retirements.
		n := newNode(t)
		id := seededRepo(t, n, "app")
		since := n.push(id, entry("p1", set("refs/heads/main", code.ZeroOID, oidOf("c1")))).Position
		retiring := n.push(id, entry("p2", set("refs/heads/main", oidOf("c1"), oidOf("c2")))).Position
		ask := func(retiredSince statelog.Position) []code.RepoSnapshot {
			t.Helper()
			pages, err := snapshotFrom(t, n.reader,
				code.StateQuery{RepoID: id, Since: since, Fresh: stale, RetiredSince: retiredSince})
			if err != nil {
				t.Fatalf("the snapshot from %v asking for the retirements above %v: %v", since, retiredSince, err)
			}
			return pages
		}

		// ASKED FOR NONE, the full answer serves no retirement, so the
		// delta's retirements count on its side alone.
		n.seedRetiredPacks(id, retiring.Packed(), harnessAt, "retired-1")
		if pages := ask(statelog.Position{}); pages[0].Full {
			t.Fatal("a delta of a ref, a pack and a retirement — three rows, the full answer's three — " +
				"was answered in full")
		}
		n.seedRetiredPacks(id, retiring.Packed(), harnessAt, "retired-2")
		if pages := ask(statelog.Position{}); !pages[0].Full {
			t.Fatalf("a delta of a ref, a pack and two retirements beside a full answer of three rows "+
				"that serves none was answered as a delta serving %v", retiredOf(pages))
		}

		// ASKED FROM ITS OWN FLOOR, the full answer would serve the same
		// two: the delta of a ref and a pack beside three rows is kept.
		pages := ask(since)
		if pages[0].Full || pages[0].RetiredFrom != since {
			t.Fatalf("a delta of a ref, a pack and two retirements, asked from its own floor, beside a full "+
				"answer of three rows and the same two retirements = full %v from %v, want a delta from %v",
				pages[0].Full, pages[0].RetiredFrom, since)
		}
		if got, want := retiredOf(pages), retiredBy(retiring, "retired-1", "retired-2"); !slices.Equal(got, want) {
			t.Fatalf("the delta serves the retirements %v, want %v", got, want)
		}

		// MORE THAN THE FULL ANSWER'S ROWS, as one compaction of a
		// repository of few refs retires — twenty beside its three rows —
		// and still within the bound: the delta asked from its own floor is
		// kept, and a full answer asked from that floor serves every one.
		seedNumbered := func(from, to int) {
			t.Helper()
			var packs []string
			for i := from; i <= to; i++ {
				packs = append(packs, fmt.Sprintf("retired-%05d", i))
			}
			n.seedRetiredPacks(id, retiring.Packed(), harnessAt, packs...)
		}
		seedNumbered(3, 20)
		whole := func() []code.RepoSnapshot {
			t.Helper()
			pages, err := snapshotFrom(t, n.reader, code.StateQuery{RepoID: id, Fresh: stale, RetiredSince: since})
			if err != nil || !pages[0].Full {
				t.Fatalf("a full answer asking for the retirements above %v = %v (full %v)", since, err,
					len(pages) > 0 && pages[0].Full)
			}
			return pages
		}
		pages = ask(since)
		if pages[0].Full || pages[0].RetiredFrom != since || len(retiredOf(pages)) != 20 {
			t.Fatalf("a delta of a ref, a pack and twenty retirements, asked from its own floor, beside a full "+
				"answer of three rows that would serve the same twenty = full %v serving %d from %v, want a "+
				"delta serving all twenty from %v", pages[0].Full, len(retiredOf(pages)), pages[0].RetiredFrom, since)
		}
		if pages := whole(); pages[0].RetiredFrom != since || len(retiredOf(pages)) != 20 {
			t.Fatalf("a full answer of three rows asked for the twenty retirements above %v serves %d from %v, "+
				"want all twenty: a bound of its own rows would serve none", since, len(retiredOf(pages)),
				pages[0].RetiredFrom)
		}

		// PAST THE BOUND a full answer serves none, and they count on the
		// delta's side alone again: answered in full, serving none.
		seedNumbered(21, code.MaxFullRetirements+1)
		pages = ask(since)
		if !pages[0].Full || !pages[0].RetiredFrom.IsZero() || len(retiredOf(pages)) != 0 {
			t.Fatalf("a delta of a ref, a pack and %d retirements beside a full answer of three rows that "+
				"cannot serve them = full %v serving %d from %v, want a full answer serving none",
				code.MaxFullRetirements+1, pages[0].Full, len(retiredOf(pages)), pages[0].RetiredFrom)
		}
		if pages := whole(); !pages[0].RetiredFrom.IsZero() || len(retiredOf(pages)) != 0 {
			t.Fatalf("a full answer asked for %d retirements, past the bound, serves %d from %v, want none",
				code.MaxFullRetirements+1, len(retiredOf(pages)), pages[0].RetiredFrom)
		}
	})
}

// seedRefs writes count live refs and packs packs straight into a node's
// estate at position 1 — the bulk a paging case needs — and counts them, as
// the applier would: a snapshot decides full or delta by the counts.
func (n *node) seedRefs(repoID string, refs, packs int) {
	n.t.Helper()
	ctx := n.t.Context()
	vars := n.db.Caps().MaxVariables
	if err := n.db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := store.InsertRows(ctx, tx, vars,
			`INSERT INTO code_refs (repo_id, ref, target, position, at) VALUES `, `(?, ?, ?, 1, 0)`, ``,
			refs, func(i int) []any {
				return []any{repoID, fmt.Sprintf("refs/heads/r%06d", i), oidOf(fmt.Sprint(i))}
			}); err != nil {
			return err
		}
		_, err := store.InsertRows(ctx, tx, vars,
			`INSERT INTO code_packs (repo_id, pack_id, part, object, hash, size, index_bytes,
				reverse_bytes, git_checksum, objects, kind, state, added_position) VALUES `,
			`(?, ?, 'pack', ?, ?, ?, ?, ?, ?, 1, 'push', 'live', 1)`, ``, packs,
			func(i int) []any {
				p := suitePack(fmt.Sprintf("seed-%06d", i))
				return []any{repoID, p.PackID, p.Object.String(), string(p.Hash), p.Size,
					p.IndexBytes, p.ReverseBytes, p.GitChecksum}
			})
		return err
	}); err != nil {
		n.t.Fatalf("seed %s: %v", repoID, err)
	}
	n.exec(`UPDATE code_repos SET refs = refs + ?, branches = branches + ?, live_packs = live_packs + ?
		WHERE id = ?`, refs, refs, packs, repoID)
}

// MANY PAGES EQUAL ONE PAGE: a repository past a page's row limit is served
// across pages whose union is exactly the one unbounded read — every row once,
// and every page answering as of the same position.
func TestManyPagesEqualOnePage(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	n.seedRefs(id, 25_000, 300)
	pages := snapshot(t, n.reader, id, statelog.Position{})
	if len(pages) < 3 {
		t.Fatalf("25 300 rows were served in %d pages at %d rows a page", len(pages), code.MaxSnapshotItems)
	}
	seen := map[string]int{}
	for _, p := range pages {
		if p.Position != pages[0].Position {
			t.Fatalf("a page answered as of %v, and page 1 as of %v", p.Position, pages[0].Position)
		}
		for _, k := range p.Packs {
			seen["pack "+k.PackID]++
		}
		for _, r := range p.Refs {
			seen["ref "+r.Ref]++
		}
	}
	unbounded := n.reader
	code.SetPageLimits(unbounded, 1<<30, 1<<40)
	whole := snapshot(t, unbounded, id, statelog.Position{})
	if len(whole) != 1 {
		t.Fatalf("the unbounded read took %d pages", len(whole))
	}
	if len(seen) != len(whole[0].Packs)+len(whole[0].Refs) {
		t.Fatalf("the pages served %d distinct rows, and the one read %d", len(seen),
			len(whole[0].Packs)+len(whole[0].Refs))
	}
	for key, times := range seen {
		if times != 1 {
			t.Fatalf("%s was served %d times", key, times)
		}
	}
}

// A SNAPSHOT SPANNING A WRITE SERVES THE PACKS ITS REFS NEED: a push received
// earlier can commit later, so a pack whose id sorts below every pack the
// packs phase already passed can land beside a ref a later page serves — the
// tail is what serves it.
func TestASnapshotSpanningAWriteServesThePacksItsRefsNeed(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	for i := range 4 {
		n.push(id, entry(fmt.Sprintf("z-%d", i), set(fmt.Sprintf("refs/heads/b%d", i), code.ZeroOID, oidOf(fmt.Sprint(i)))))
	}
	code.SetPageLimits(n.reader, 3, code.MaxPageBytes)
	first, err := n.reader.Snapshot(t.Context(), code.StateQuery{RepoID: id, Fresh: stale})
	if err != nil || first.Next == nil {
		t.Fatalf("page 1 = %+v, %v; want a page with more to come", first, err)
	}
	// THE COMMITTED-LATE PUSH: a pack id below every one served.
	n.push(id, entry("a-late", set("refs/heads/main", code.ZeroOID, oidOf("late"))))
	pages := []code.RepoSnapshot{first}
	q := code.StateQuery{RepoID: id, Cursor: first.Next}
	for q.Cursor != nil {
		page, err := n.reader.Snapshot(t.Context(), q)
		if err != nil {
			t.Fatalf("a later page: %v", err)
		}
		pages = append(pages, page)
		q.Cursor = page.Next
	}
	u := unionOf(pages)
	if u.refs["refs/heads/main"] != oidOf("late") {
		t.Fatalf("the premise: a later page serves main at the late push's commit, and the "+
			"snapshot serves it at %q", u.refs["refs/heads/main"])
	}
	if _, ok := u.packs["push-a-late/pack"]; !ok {
		t.Fatal("the snapshot serves main at the late push's commit and not the late push's pack")
	}
}

// A LATER PAGE IS NEVER READ BEHIND AN EARLIER ONE: every page after the first
// reads at session floored at the highest checkpoint a page was read at, so a
// node that has not applied that far refuses `behind` rather than ending the
// tail before a pack the refs it served name.
func TestALaterPageIsNeverReadBehindAnEarlierOne(t *testing.T) {
	t.Parallel()
	a, b := newTwoNodes(t)
	id := a.ensure("app", described("app"), t1).RepoID
	for i := range 3 {
		a.push(id, entry(fmt.Sprintf("s-%d", i), set(fmt.Sprintf("refs/heads/s%d", i), code.ZeroOID, oidOf(fmt.Sprint(i)))))
	}
	a.push(id, entry("s-3", set("refs/heads/s3", code.ZeroOID, oidOf("3"))))
	b.drain()
	code.SetPageLimits(a.reader, 2, code.MaxPageBytes)
	code.SetPageLimits(b.reader, 2, code.MaxPageBytes)

	page, err := a.reader.Snapshot(t.Context(), code.StateQuery{RepoID: id, Fresh: stale})
	if err != nil || page.Next == nil {
		t.Fatalf("page 1 = %v; want a page with more to come", err)
	}
	b.hold()
	for i := range 3 {
		ref := fmt.Sprintf("refs/heads/p%d", i)
		if i == 2 {
			ref = "refs/heads/zzz"
		}
		a.push(id, entry(fmt.Sprintf("p-%d", i), set(ref, code.ZeroOID, oidOf("p"+fmt.Sprint(i)))))
	}
	pages := []code.RepoSnapshot{page}
	cursor := page.Next
	for cursor != nil && cursor.Phase != "tail" {
		page, err = a.reader.Snapshot(t.Context(), code.StateQuery{RepoID: id, Cursor: cursor})
		if err != nil {
			t.Fatalf("a page on A: %v", err)
		}
		pages = append(pages, page)
		cursor = page.Next
	}
	if cursor == nil {
		t.Fatal("the snapshot ended before its tail")
	}
	if cursor.Seen != a.waiter.Committed() {
		t.Fatalf("the tail's floor is %v, want the refs read's %v", cursor.Seen, a.waiter.Committed())
	}
	_, err = b.reader.Snapshot(t.Context(), code.StateQuery{RepoID: id, Cursor: cursor})
	var refused *statelog.Refused
	if !errors.As(err, &refused) || refused.Code != statelog.RefuseBehind {
		t.Fatalf("the tail on a node behind the refs read = %v, want refused behind", err)
	}
	b.release()
	for cursor != nil {
		page, err = b.reader.Snapshot(t.Context(), code.StateQuery{RepoID: id, Cursor: cursor})
		if err != nil {
			t.Fatalf("the tail on B once it applied: %v", err)
		}
		pages = append(pages, page)
		cursor = page.Next
	}
	u := unionOf(pages)
	if u.refs["refs/heads/zzz"] != oidOf("p2") {
		t.Fatalf("the premise: the refs read on A serves zzz at p-2's commit, and the snapshot "+
			"serves it at %q", u.refs["refs/heads/zzz"])
	}
	if _, ok := u.packs["push-p-2/pack"]; !ok {
		t.Fatal("the snapshot names zzz at p-2's commit and never serves p-2's pack")
	}
}

// A SWEEP UNDER A DELTA SNAPSHOT RESTARTS IT: a tombstone the delta needed,
// swept between its pages, is a deletion the snapshot can no longer state — so
// a later page answers ErrSnapshotMoved, and the snapshot asked again is full.
func TestASweepUnderADeltaSnapshotRestartsIt(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	n.push(id, entry("p1", set("refs/heads/gone", code.ZeroOID, oidOf("g"))))
	since := n.waiter.Committed()
	n.push(id, entry("p2", set("refs/heads/gone", oidOf("g"), code.ZeroOID)))
	var moved []code.Command
	for i := range 6 {
		moved = append(moved, set(fmt.Sprintf("refs/heads/m%d", i), code.ZeroOID, oidOf(fmt.Sprint(i))))
	}
	n.push(id, entry("p3", moved...))
	code.SetPageLimits(n.reader, 3, code.MaxPageBytes)
	page, err := n.reader.Snapshot(t.Context(), code.StateQuery{RepoID: id, Since: since, Fresh: stale})
	if err != nil || page.Full || page.Next == nil {
		t.Fatalf("page 1 = full %v, next %v, %v; want a delta with more to come", page.Full, page.Next, err)
	}
	sweep := time.Now().Add(code.RefTombstoneHorizon + time.Hour)
	n.storedAt = func(uint64, time.Time) time.Time { return sweep }
	n.push(id, entry("p4", set("refs/heads/m0", oidOf("0"), oidOf("x"))))
	n.storedAt = nil
	if _, err := n.reader.Snapshot(t.Context(), code.StateQuery{RepoID: id, Cursor: page.Next}); !errors.Is(err, code.ErrSnapshotMoved) {
		t.Fatalf("a later page after its deletion was swept = %v, want ErrSnapshotMoved", err)
	}
	again := snapshot(t, n.reader, id, since)
	if !again[0].Full {
		t.Fatal("the snapshot asked again from the same position was a delta")
	}
	if _, present := unionOf(again).refs["refs/heads/gone"]; present {
		t.Fatal("the full snapshot serves the deleted ref")
	}
}

// A REANCHOR OR A RECREATION UNDER A SNAPSHOT RESTARTS IT, and a cursor this
// reader never issued is refused.
func TestAReanchorUnderASnapshotRestartsIt(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	var since statelog.Position
	for i := range 4 {
		at := n.push(id, entry(fmt.Sprintf("p%d", i), set(fmt.Sprintf("refs/heads/r%d", i), code.ZeroOID, oidOf(fmt.Sprint(i))))).Position
		if i == 0 {
			since = at
		}
	}
	code.SetPageLimits(n.reader, 2, code.MaxPageBytes)
	page, err := n.reader.Snapshot(t.Context(), code.StateQuery{RepoID: id, Fresh: stale})
	if err != nil || page.Next == nil {
		t.Fatalf("page 1: %v", err)
	}
	delta, err := n.reader.Snapshot(t.Context(), code.StateQuery{RepoID: id, Since: since, Fresh: stale})
	if err != nil || delta.Full || delta.Next == nil {
		t.Fatalf("page 1 of a delta = full %v, next %v, %v; want a delta with more to come", delta.Full,
			delta.Next, err)
	}

	recreated := *page.Next
	recreated.Stream = recreated.Stream.Add(time.Hour)
	if _, err := n.reader.Snapshot(t.Context(), code.StateQuery{RepoID: id, Cursor: &recreated}); !errors.Is(err, code.ErrSnapshotMoved) {
		t.Fatalf("a cursor keyed to another instance of the log = %v, want ErrSnapshotMoved", err)
	}
	for name, mangle := range map[string]func(*code.StateCursor){
		"read below where it began": func(c *code.StateCursor) { c.Seen = statelog.Position{Stream: c.From.Stream} },
		"seen on another log":       func(c *code.StateCursor) { c.Seen.Stream = "CREWLET_TRACKER_LOG" },
		"a phase nobody serves":     func(c *code.StateCursor) { c.Phase = "objects" },
		"a delta with no floor":     func(c *code.StateCursor) { c.Full = false },
		"a key of another shape":    func(c *code.StateCursor) { c.Phase, c.After = "tail", "not-a-key" },
		// A FULL ANSWER SERVES A RETIRED PHASE only from the floor its first
		// page honoured, a position on the code log.
		"retiring with no floor": func(c *code.StateCursor) { c.Phase, c.After = "retired", "" },
		"retiring from another log": func(c *code.StateCursor) {
			c.RetiredFrom = statelog.Position{Stream: "CREWLET_TRACKER_LOG", Generation: c.From.Generation, Seq: 1}
		},
		"retiring past a key of another shape": func(c *code.StateCursor) {
			c.Phase, c.RetiredFrom, c.After = "retired", c.From, "not-a-key"
		},
		"at a drop epoch below 0": func(c *code.StateCursor) { c.DropEpoch = -1 },
	} {
		bad := *page.Next
		mangle(&bad)
		if _, err := n.reader.Snapshot(t.Context(), code.StateQuery{RepoID: id, Cursor: &bad}); !errors.Is(err, code.ErrBadCursor) {
			t.Errorf("a cursor %s = %v, want ErrBadCursor", name, err)
		}
	}
	// A DELTA'S RETIREMENTS ARE FROM ITS OWN FLOOR, always.
	for name, mangle := range map[string]func(*code.StateCursor){
		"a delta retiring from elsewhere": func(c *code.StateCursor) { c.RetiredFrom = c.From },
		"a delta retiring nothing":        func(c *code.StateCursor) { c.RetiredFrom = statelog.Position{} },
	} {
		bad := *delta.Next
		mangle(&bad)
		if _, err := n.reader.Snapshot(t.Context(), code.StateQuery{RepoID: id, Cursor: &bad}); !errors.Is(err, code.ErrBadCursor) {
			t.Errorf("a cursor %s = %v, want ErrBadCursor", name, err)
		}
	}

	// THE REANCHOR: this node's checkpoint moves to a later generation.
	at := n.waiter.Committed()
	moved := statelog.Position{Stream: at.Stream, Generation: at.Generation + 1, Seq: 1}
	n.exec(`UPDATE statelog_cursor SET generation = ?, seq = 1 WHERE stream = ?`,
		int64(moved.Generation), moved.Stream)
	n.waiter.reach(moved)
	if _, err := n.reader.Snapshot(t.Context(), code.StateQuery{RepoID: id, Cursor: page.Next}); !errors.Is(err, code.ErrSnapshotMoved) {
		t.Fatalf("a later page after a reanchor = %v, want ErrSnapshotMoved", err)
	}
}

// AN EXPUNGE'S BEGIN UNDER A SNAPSHOT RESTARTS IT: every page of one snapshot
// is read at one drop epoch, because pages spanning a begin would leave a
// copy holding, live, the packs the begin rewrote — every object of them,
// those it left dropping too — beside a row at the begin's epoch, and a push
// checked against that copy would carry an epoch its objects were not read at.
// So a later page read after a begin answers ErrSnapshotMoved, and the
// snapshot asked again reads every page at the begin's epoch.
//
// Mutation: drop the later page's comparison of the epochs, and that page is
// served.
func TestABeginUnderASnapshotRestartsIt(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id, ids := packedRepo(t, n, "app", 4)
	code.SetPageLimits(n.reader, 2, code.MaxPageBytes)
	page, err := n.reader.Snapshot(t.Context(), code.StateQuery{RepoID: id, Fresh: stale})
	if err != nil || page.Next == nil || page.Repo.DropEpoch != 0 || page.Next.DropEpoch != 0 {
		t.Fatalf("page 1 = drop epoch %d, next %+v, %v; want epoch 0 with more to come", page.Repo.DropEpoch,
			page.Next, err)
	}
	c, _ := committerOn(t, n, n.pub)
	b := beginFor(n, id, 1, ids, 1, 1)
	res, err := c.ProposeExpunge(t.Context(), code.ExpungeProposal{Begin: &b})
	requireExpunged(t, res, err)
	if _, err := n.reader.Snapshot(t.Context(), code.StateQuery{RepoID: id, Cursor: page.Next}); !errors.Is(err, code.ErrSnapshotMoved) {
		t.Fatalf("a later page after a begin = %v, want ErrSnapshotMoved", err)
	}
	again := snapshot(t, n.reader, id, statelog.Position{})
	if len(again) < 2 {
		t.Fatalf("the snapshot asked again took %d pages, want more than one", len(again))
	}
	for i, p := range again {
		if p.Repo.DropEpoch != 1 {
			t.Fatalf("page %d of the snapshot asked again is at drop epoch %d, want the begin's 1", i+1,
				p.Repo.DropEpoch)
		}
	}
}

// A PAGE ENDS AT ITS BYTE BUDGET, whatever its row limit: maximal pushes with
// the longest refs fill a page with far fewer rows than it may carry, and
// paging still serves every row once.
func TestAPageEndsAtItsByteBudget(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	long := strings.Repeat("x", code.MaxRefNameBytes-len("refs/heads/")-8)
	for p := range 20 {
		var cmds []code.Command
		for c := range code.MaxCommandsPerPush {
			cmds = append(cmds, set(fmt.Sprintf("refs/heads/%s%03d%04d", long, p, c),
				code.ZeroOID, oidOf(fmt.Sprint(p, c))))
		}
		n.push(id, entry(fmt.Sprintf("push-%02d", p), cmds...))
	}

	pages := snapshot(t, n.reader, id, statelog.Position{})
	refs := map[string]bool{}
	for _, p := range pages {
		if len(p.Packs)+len(p.Refs) == 0 && p.Next != nil {
			t.Fatal("a page served no row and was not the last")
		}
		size, largest := 0, 0
		for _, r := range p.Refs {
			b, _ := json.Marshal(r)
			size += len(b)
			largest = max(largest, len(b))
			if refs[r.Ref] {
				t.Fatalf("%s was served twice", r.Ref)
			}
			refs[r.Ref] = true
		}
		for _, k := range p.Packs {
			b, _ := json.Marshal(k)
			size += len(b)
			largest = max(largest, len(b))
		}
		if size > code.MaxPageBytes+largest {
			t.Fatalf("a snapshot page carried %d bytes, past its %d-byte budget and one row", size, code.MaxPageBytes)
		}
	}
	if len(refs) != 20*code.MaxCommandsPerPush || len(pages) < 2 {
		t.Fatalf("the snapshot served %d refs of %d in %d pages", len(refs), 20*code.MaxCommandsPerPush, len(pages))
	}

	seen := map[string]bool{}
	q := code.ReadQuery{Asker: asker, Kind: code.ReadPushes, Repo: "app", Limit: 200}
	for range 100 {
		answer, err := n.reader.Read(t.Context(), q)
		if err != nil {
			t.Fatalf("Read(pushes): %v", err)
		}
		size, largest := 0, 0
		for _, p := range answer.Pushes {
			b, _ := json.Marshal(p)
			size += len(b)
			largest = max(largest, len(b))
			seen[p.PushID] = true
		}
		if size > code.MaxPageBytes+largest {
			t.Fatalf("a pushes page carried %d bytes, past its budget and one row", size)
		}
		if answer.Next == "" {
			break
		}
		q.Cursor = answer.Next
	}
	if len(seen) != 20 {
		t.Fatalf("the pushes listing served %d pushes of 20", len(seen))
	}
}

// A LISTING PAGE COUNTS ITS RULES: a repository row carries its protection
// rules, up to MaxProtectionsPerRepo of them at up to MaxRefNameBytes, so a
// page measured before its rules are filled in — a page of 500 repositories
// at 64 long rules each is ten megabytes — passes the reply ceiling while
// believing itself small. The page ends at its byte budget measured with them.
func TestAListingPageCountsItsRules(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	settings := suiteSettings()
	settings.Protections = nil
	for i := range 10 {
		head := fmt.Sprintf("refs/heads/%02d-", i)
		settings.Protections = append(settings.Protections, code.Protection{PushRule: code.PushRule{
			Pattern:    head + strings.Repeat("x", code.MaxRefNameBytes-len(head)),
			DirectPush: code.DirectPersons,
		}})
	}
	const repos = 12
	for i := range repos {
		n.ensure(fmt.Sprintf("repo-%02d", i), settings, t1)
	}
	// A BUDGET OF ABOUT THREE REPOSITORIES WITH THEIR RULES, and twenty
	// without them.
	budget := 9_000
	code.SetPageLimits(n.reader, code.MaxSnapshotItems, budget)

	listed := map[string]bool{}
	q := code.ReadQuery{Asker: asker, Kind: code.ReadRepos}
	for range repos + 1 {
		answer, err := n.reader.Read(t.Context(), q)
		if err != nil {
			t.Fatalf("Read(repos): %v", err)
		}
		size, largest := 0, 0
		for _, r := range answer.Repos {
			if len(r.Protections) != len(settings.Protections) {
				t.Fatalf("%s is listed with %d rules, want its %d", r.Name, len(r.Protections),
					len(settings.Protections))
			}
			b, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			size += len(b)
			largest = max(largest, len(b))
			listed[r.Name] = true
		}
		if size > budget+largest {
			t.Fatalf("a listing page carried %d bytes of %d repositories, past its %d-byte "+
				"budget and one row — its rules were not counted", size, len(answer.Repos), budget)
		}
		if answer.Next == "" {
			break
		}
		q.Cursor = answer.Next
	}
	if len(listed) != repos {
		t.Fatalf("the listing served %d repositories of %d", len(listed), repos)
	}
}

// PUSHES PAGE ACROSS ONE RECORD WITHOUT LOSING ANY: sixty-four pushes share one
// record's position and instant, so the listing's cursor carries the push id —
// without it, a page boundary inside the record skips the rest of it.
func TestPushesPageAcrossOneRecordWithoutLosingAny(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	var entries []code.PushEntry
	for i := range code.MaxPushesPerRecord {
		entries = append(entries, entry(fmt.Sprintf("push-%02d", i),
			set(fmt.Sprintf("refs/heads/b%02d", i), code.ZeroOID, oidOf(fmt.Sprint(i)))))
	}
	n.push(id, entries...)
	var got []string
	q := code.ReadQuery{Asker: asker, Kind: code.ReadPushes, Repo: "app", Limit: 10}
	pages := 0
	// BOUNDED: a cursor that loses its place pages for ever, and that is
	// this case's failure rather than its timeout's.
	for pages < 50 {
		answer, err := n.reader.Read(t.Context(), q)
		if err != nil {
			t.Fatalf("Read(pushes): %v", err)
		}
		pages++
		for _, p := range answer.Pushes {
			got = append(got, p.PushID)
		}
		if answer.Next == "" {
			break
		}
		q.Cursor = answer.Next
	}
	want := make([]string, 0, code.MaxPushesPerRecord)
	for _, e := range entries {
		want = append(want, e.PushID)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(want)))
	if pages != 7 || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%d pages served %v, want seven pages newest first: %v", pages, got, want)
	}
}

// A PUSH LISTING IS NEWEST IN LOG ORDER: the history pages by the position a
// push landed at, newest first, its cursor `<position>/<push id>` — never by
// the record's instant, which is the broker's and steps back across a stream
// leader's change. Here every record is stamped a minute EARLIER than the one
// before it, so an order by instant reads the history backwards.
//
// Mutation: order the listing by `at` again — the pushes come oldest first
// (and the plan gate finds it sorting every push of the repository).
func TestAPushListingIsNewestInLogOrder(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	n.storedAt = func(seq uint64, _ time.Time) time.Time {
		return harnessAt.Add(-time.Duration(seq) * time.Minute)
	}
	var landed []string
	var positions []statelog.Position
	for i := range 4 {
		push := fmt.Sprintf("push-%d", i)
		positions = append(positions, n.push(id, entry(push,
			set(fmt.Sprintf("refs/heads/b%d", i), code.ZeroOID, oidOf(push)))).Position)
		landed = append([]string{push}, landed...)
	}
	var got, cursors []string
	q := code.ReadQuery{Asker: asker, Kind: code.ReadPushes, Repo: "app", Limit: 1}
	for range 10 {
		answer, err := n.reader.Read(t.Context(), q)
		if err != nil {
			t.Fatalf("Read(pushes): %v", err)
		}
		for _, p := range answer.Pushes {
			got = append(got, p.PushID)
		}
		if answer.Next == "" {
			break
		}
		cursors = append(cursors, answer.Next)
		q.Cursor = answer.Next
	}
	if fmt.Sprint(got) != fmt.Sprint(landed) {
		t.Fatalf("the history reads %v, want %v: newest first by the position each landed at", got, landed)
	}
	if want := fmt.Sprintf("%d/push-3", positions[3].Packed()); len(cursors) == 0 || cursors[0] != want {
		t.Fatalf("the first page's cursor is %v, want %q: <packed position>/<push id>", cursors, want)
	}
}

// A PUSH ROW NAMES WHO MADE THE PUSH AND HOW, as its record's entry says, and
// the history and the index of pushes read it back as the entry said it: a
// person's push names the credential it came through as its actor, beside
// the seat they hold, built by git and with no run; a seat's push names the
// seat's handle as its actor and its seat — never a blank, since a seat holds
// no credential — built by the engine, with the turn, the work item and the
// model its run names.
//
// Mutations: write the principal's credential as every row's actor
// (Principal.Actor answering OperatorID) — the seat's row names nobody;
// read a seat's row back without its run (PushRow.principal dropping Run) —
// the index of pushes names no turn.
func TestAPushRowNamesWhoMadeItAndHow(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	person := entry("p-person", set("refs/heads/main", code.ZeroOID, oidOf("person")))
	person.Principal = code.Principal{Kind: code.PrincipalOperator, OperatorID: "pat:019a2f4e", Seat: "founder"}
	seat := entry("p-seat", set("refs/heads/seats/docs-writer/eng-12", code.ZeroOID, oidOf("seat")))
	seat.Principal = code.Principal{Kind: code.PrincipalAgent, Seat: "docs-writer", Run: code.RunProvenance{
		TurnID: "0192f00d-0000-7000-8000-000000000001", WorkBackend: "native", WorkKey: "ENG-12",
		Model: "claude-sonnet-4-5"}}
	seat.Via = code.ViaEngine
	n.push(id, person, seat)

	type named struct {
		actor, kind, seat string
		via               code.PushVia
		run               code.RunProvenance
	}
	want := map[string]named{
		"p-person": {"pat:019a2f4e", string(code.PrincipalOperator), "founder", code.ViaGit, code.RunProvenance{}},
		"p-seat":   {"docs-writer", string(code.PrincipalAgent), "docs-writer", code.ViaEngine, seat.Principal.Run},
	}
	history, err := n.reader.Read(t.Context(), code.ReadQuery{Asker: asker, Kind: code.ReadPushes, Repo: "app",
		Fresh: stale})
	if err != nil {
		t.Fatalf("Read(pushes): %v", err)
	}
	got := map[string]named{}
	for _, r := range history.Pushes {
		got[r.PushID] = named{r.Actor, r.ActorKind, r.ActorSeat, r.Via, r.Run}
	}
	for push, w := range want {
		if g, ok := got[push]; !ok || g != w {
			t.Errorf("push %s's row names %+v (%t), want %+v", push, g, ok, w)
		}
	}

	// THE INDEX OF PUSHES IS BUILT FROM THE SAME ROWS, and its principal is
	// the entry's, run and all — what a git node says of who pushed a commit.
	index, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadIntroduced,
		Introduced: &code.IntroducedQuery{RepoID: id}, Fresh: stale})
	if err != nil {
		t.Fatalf("Read(introduced): %v", err)
	}
	entries := map[string]code.PushEntry{person.PushID: person, seat.PushID: seat}
	for _, r := range index.Introduced {
		if e, ok := entries[r.PushID]; !ok || r.Principal != e.Principal || r.Via != e.Via {
			t.Errorf("the index of pushes reads push %s as by %+v, %s; want its entry's %+v, %s", r.PushID,
				r.Principal, r.Via, e.Principal, e.Via)
		}
		delete(entries, r.PushID)
	}
	if len(entries) > 0 {
		t.Errorf("the index of pushes read none of %v", entries)
	}
}

// A PUSH ROW NAMING NO WAY IS REFUSED BY EVERY READER of it, never read as
// one: the via column's default is no way at all (replicated 0086), so a row
// an apply wrote without it fails the read, naming the push — the history's
// and the index of pushes' alike — where a default naming a way would have
// answered every push the engine built as one a client's git sent.
//
// Mutation: scanPush taking any via — the row is read as a push built no way.
func TestAPushRowNamingNoWayIsRefused(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	n.push(id, entry("p-forgot", set("refs/heads/main", code.ZeroOID, oidOf("forgot"))))
	n.exec(`UPDATE code_pushes SET via = '' WHERE push_id = 'p-forgot'`)
	for name, q := range map[string]code.ReadQuery{
		"the history": {Asker: asker, Kind: code.ReadPushes, Repo: "app", Fresh: stale},
		"the index of pushes": {Kind: code.ReadIntroduced, Introduced: &code.IntroducedQuery{RepoID: id},
			Fresh: stale},
	} {
		if _, err := n.reader.Read(t.Context(), q); !errors.Is(err, code.ErrInvalid) ||
			!strings.Contains(err.Error(), "p-forgot") {
			t.Errorf("%s of a push row naming no way: %v, want ErrInvalid naming the push", name, err)
		}
	}
}

// A HISTORY READ IN BRIEF SHOWS A PUSH'S REFUSALS FIRST AND COUNTS WHAT IT
// LEAVES OUT. A push of more commands than a screen gives it lines is cut to
// BriefCommands — every refused one ahead of the rest, each part in the order
// the client sent it — and every command left out is counted by what became
// of it, so the listed commands and the counts are the whole push; its
// introduced ids are counted and not listed. A push that fits is listed whole
// and omits nothing, and the same history read whole lists every command and
// every id, as one push read by its id does.
//
// Mutations: cut in the order the client sent (the second refusal is left
// out), count every command rather than those left out, or keep the
// introduced ids — each fails below.
func TestAHistoryInBriefShowsRefusalsFirstAndCountsTheRest(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	// EXACTLY BriefCommands, which fit.
	seed := entry(uuid.Must(uuid.NewV7()).String(),
		set("refs/heads/main", code.ZeroOID, oidOf("M1")), set("refs/heads/old", code.ZeroOID, oidOf("O")),
		set("refs/heads/topic", code.ZeroOID, oidOf("T1")), set("refs/heads/dev", code.ZeroOID, oidOf("D1")),
		set("refs/heads/same", code.ZeroOID, oidOf("S")))
	n.push(id, seed)

	refused := func(ref, old, new string, why code.Refusal) code.Command {
		return code.Command{Ref: ref, Old: old, New: new, Result: code.ResultNG, Reason: why}
	}
	tag := func(name string) code.Command { return set("refs/tags/"+name, code.ZeroOID, oidOf(name)) }
	forced := set("refs/heads/topic", oidOf("T1"), oidOf("T2"))
	forced.Forced = true
	sent := []code.Command{
		tag("t00"), tag("t01"),
		refused("refs/heads/main", oidOf("M1"), oidOf("X"), code.RefusalProtectedForce),
		tag("t03"), tag("t04"),
		set("refs/heads/old", oidOf("O"), code.ZeroOID),
		tag("t06"),
		{Ref: "refs/heads/same", Old: oidOf("S"), New: oidOf("S"), Result: code.ResultNoop},
		refused("refs/tags/v0", oidOf("V0"), oidOf("V1"), code.RefusalTagMoved),
		forced,
		tag("t10"),
		set("refs/heads/dev", oidOf("D1"), oidOf("D2")),
	}
	big := entry(uuid.Must(uuid.NewV7()).String(), sent...)
	big.Introduced, big.IntroducedCount = []string{oidOf("c1"), oidOf("c2")}, 2
	n.push(id, big)

	read := func(q code.ReadQuery) map[string]code.PushRow {
		t.Helper()
		q.Asker, q.Kind, q.Repo, q.Fresh = asker, code.ReadPushes, "app", stale
		answer, err := n.reader.Read(t.Context(), q)
		if err != nil {
			t.Fatalf("Read(pushes %+v): %v", q, err)
		}
		rows := map[string]code.PushRow{}
		for _, r := range answer.Pushes {
			rows[r.PushID] = r
		}
		return rows
	}

	brief := read(code.ReadQuery{Brief: true})
	cut := brief[big.PushID]
	want := []code.Command{sent[2], sent[8], sent[0], sent[1], sent[3]}
	if !slices.Equal(cut.Commands, want) {
		t.Errorf("the push in brief lists %+v,\nwant its refusals first, then the first it sent: %+v",
			cut.Commands, want)
	}
	wantOmitted := code.CommandCounts{Created: 3, Moved: 1, Forced: 1, Deleted: 1, Unchanged: 1}
	if cut.Omitted != wantOmitted {
		t.Errorf("the push in brief omits %+v, want what it leaves out: %+v", cut.Omitted, wantOmitted)
	}
	if cut.Introduced != nil || cut.IntroducedCount != 2 {
		t.Errorf("the push in brief introduces %v (%d), want none listed and 2 counted", cut.Introduced,
			cut.IntroducedCount)
	}
	if fits := brief[seed.PushID]; !slices.Equal(fits.Commands, seed.Commands) ||
		fits.Omitted != (code.CommandCounts{}) {
		t.Errorf("a push of %d commands in brief lists %+v omitting %+v, want it whole",
			len(seed.Commands), fits.Commands, fits.Omitted)
	}

	whole := read(code.ReadQuery{})[big.PushID]
	if !slices.Equal(whole.Commands, sent) || whole.Omitted != (code.CommandCounts{}) ||
		!slices.Equal(whole.Introduced, big.Introduced) {
		t.Errorf("the push read whole lists %d commands omitting %+v and introduces %v, want all %d and %v",
			len(whole.Commands), whole.Omitted, whole.Introduced, len(sent), big.Introduced)
	}
	one := read(code.ReadQuery{Push: big.PushID})
	if got, ok := one[big.PushID]; len(one) != 1 || !ok || !slices.Equal(got.Commands, sent) ||
		!slices.Equal(got.Introduced, big.Introduced) {
		t.Errorf("the push read by its id answered %+v, want it alone and whole", one)
	}
}

// ONE PUSH IS READ FROM ITS OWN REPOSITORY'S HISTORY ONLY. A read of one
// repository's history narrowed to a push of another answers no row: the
// asker was judged on the repository the read names, and a push of another
// is that one's contents — which the asker may not read at all. A push nobody
// made answers none either, as a listing narrowed to it holds none.
//
// Mutation: answer the push whatever repository its row names, and the read
// of app answers lib's push.
func TestOnePushIsReadFromItsOwnRepositorysHistoryOnly(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	app, lib := seededRepo(t, n, "app"), seededRepo(t, n, "lib")
	ours := entry(uuid.Must(uuid.NewV7()).String(), set("refs/heads/main", code.ZeroOID, oidOf("ours")))
	theirs := entry(uuid.Must(uuid.NewV7()).String(), set("refs/heads/main", code.ZeroOID, oidOf("theirs")))
	n.push(app, ours)
	n.push(lib, theirs)
	for push, want := range map[string]int{
		ours.PushID: 1, theirs.PushID: 0, uuid.Must(uuid.NewV7()).String(): 0,
	} {
		answer, err := n.reader.Read(t.Context(), code.ReadQuery{Asker: asker, Kind: code.ReadPushes,
			Repo: "app", Push: push, Fresh: stale})
		if err != nil {
			t.Fatalf("Read(app's push %s): %v", push, err)
		}
		if len(answer.Pushes) != want || answer.Next != "" ||
			(want == 1 && answer.Pushes[0].PushID != push) {
			t.Errorf("app's history narrowed to push %s answered %+v, want %d row", push, answer.Pushes, want)
		}
	}
}

// ONLY A PUSH HISTORY IS READ IN BRIEF OR NARROWED TO ONE PUSH, and a push is
// named by the id its receiving node minted: either field on another kind is
// refused naming it, as one push beside a cursor, a limit or a brief — which
// page or cut a listing it is not — and an id that no push can carry.
func TestOnlyAPushHistoryIsReadInBriefOrNarrowedToOnePush(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	push := uuid.Must(uuid.NewV7()).String()
	history := func(edit func(*code.ReadQuery)) code.ReadQuery {
		q := code.ReadQuery{Asker: asker, Kind: code.ReadPushes, Repo: "app", Fresh: stale}
		edit(&q)
		return q
	}
	for _, c := range []struct {
		name, field string
		q           code.ReadQuery
	}{
		{"a listing in brief", "brief", code.ReadQuery{Asker: asker, Kind: code.ReadRepos, Brief: true,
			Fresh: stale}},
		{"a repository narrowed to a push", "push", code.ReadQuery{Asker: asker, Kind: code.ReadRepo,
			Repo: "app", Push: push, Fresh: stale}},
		{"refs in brief", "brief", code.ReadQuery{Asker: asker, Kind: code.ReadRefs, Repo: "app", Brief: true,
			Fresh: stale}},
		{"an introduced read in brief", "brief", code.ReadQuery{Kind: code.ReadIntroduced, Brief: true,
			Introduced: &code.IntroducedQuery{RepoID: id}}},
		{"an introduced read narrowed to a push", "push", code.ReadQuery{Kind: code.ReadIntroduced,
			Push: push, Introduced: &code.IntroducedQuery{RepoID: id}}},
		{"a compaction listing narrowed to a push", "push", code.ReadQuery{Kind: code.ReadCompactionDue,
			MinPacks: 1, Push: push, Fresh: stale}},
		{"a publication listing in brief", "brief", code.ReadQuery{Kind: code.ReadPublicationDue,
			Brief: true, Fresh: stale}},
		{"an expunge listing narrowed to a push", "push", code.ReadQuery{Kind: code.ReadExpungeDue,
			Push: push, Fresh: stale}},
		{"a push named by no push's id", "push", history(func(q *code.ReadQuery) { q.Push = "p-1" })},
		{"a push's id in capitals", "push", history(func(q *code.ReadQuery) {
			q.Push = strings.ToUpper(push)
		})},
		{"one push after a cursor", "push", history(func(q *code.ReadQuery) { q.Push, q.Cursor = push, "1/x" })},
		{"one push with a limit", "push", history(func(q *code.ReadQuery) { q.Push, q.Limit = push, 1 })},
		{"one push in brief", "push", history(func(q *code.ReadQuery) { q.Push, q.Brief = push, true })},
	} {
		// REFUSED NAMING THE FIELD, so a case refused for anything else
		// certifies nothing.
		if _, err := n.reader.Read(t.Context(), c.q); !errors.Is(err, code.ErrInvalid) ||
			!strings.Contains(err.Error(), ": "+c.field+":") {
			t.Errorf("%s: %v, want ErrInvalid naming %s", c.name, err, c.field)
		}
	}
	// AND THE SAME FIELDS ON A PUSH HISTORY ARE READ.
	for _, q := range []code.ReadQuery{
		history(func(q *code.ReadQuery) { q.Brief = true }),
		history(func(q *code.ReadQuery) { q.Push = push }),
	} {
		if _, err := n.reader.Read(t.Context(), q); err != nil {
			t.Errorf("Read(pushes, brief %t, push %q): %v", q.Brief, q.Push, err)
		}
	}
}

// A PAGE IN BRIEF READS WHAT A WHOLE PAGE READS. Its rows are read and
// decoded whole to be cut and counted, so each is charged the row it read, or
// the brief one where that is the larger: charged only what it sends, a page
// of imports would read a few hundred bytes' worth each, and so without bound
// — here every push the history holds, against a budget a whole page fills
// at its third.
//
// Mutation: charge a row in brief what it sends, and the brief page holds
// every push.
func TestAPageInBriefReadsWhatAWholePageReads(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	const pushes, refs = 12, 60
	for p := range pushes {
		var cmds []code.Command
		for c := range refs {
			cmds = append(cmds, set(fmt.Sprintf("refs/tags/r%02d-%03d", p, c), code.ZeroOID,
				oidOf(fmt.Sprint(p, "/", c))))
		}
		n.push(id, entry(uuid.Must(uuid.NewV7()).String(), cmds...))
	}
	whole, err := n.reader.Read(t.Context(), code.ReadQuery{Asker: asker, Kind: code.ReadPushes, Repo: "app",
		Fresh: stale})
	if err != nil {
		t.Fatalf("Read(pushes): %v", err)
	}
	row, err := json.Marshal(whole.Pushes[0])
	if err != nil {
		t.Fatal(err)
	}
	// TWO AND A HALF WHOLE ROWS' WORTH: full at the third, whatever a row's
	// position adds to its length.
	code.SetPageLimits(n.reader, code.MaxSnapshotItems, 5*len(row)/2)
	for _, brief := range []bool{false, true} {
		answer, err := n.reader.Read(t.Context(), code.ReadQuery{Asker: asker, Kind: code.ReadPushes,
			Repo: "app", Brief: brief, Fresh: stale})
		if err != nil {
			t.Fatalf("Read(pushes, brief %t): %v", brief, err)
		}
		if len(answer.Pushes) != 3 || answer.Next == "" {
			t.Errorf("a page (brief %t) under a budget of two and a half whole rows holds %d of %d pushes, "+
				"next %q; want three and a cursor", brief, len(answer.Pushes), pushes, answer.Next)
		}
	}
}

// A CODE READ STATES ITS FRESHNESS: a point read and a snapshot's first page
// name their level, and a session read — a listing's too — names the position
// it waits for.
func TestACodeReadStatesItsFreshness(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	for name, read := range map[string]func() error{
		"Resolve with no level": func() error {
			_, err := n.reader.Resolve(t.Context(), code.ResolveQuery{Name: "app"})
			return err
		},
		"Snapshot with no level": func() error {
			_, err := n.reader.Snapshot(t.Context(), code.StateQuery{RepoID: id})
			return err
		},
		"Resolve at session with no position": func() error {
			_, err := n.reader.Resolve(t.Context(), code.ResolveQuery{Name: "app",
				Fresh: statelog.Freshness{Level: statelog.ReadSession}})
			return err
		},
		"Read at session with no position": func() error {
			_, err := n.reader.Read(t.Context(), code.ReadQuery{Asker: asker, Kind: code.ReadRepos,
				Fresh: statelog.Freshness{Level: statelog.ReadSession}})
			return err
		},
	} {
		if err := read(); !errors.Is(err, code.ErrBadLevel) {
			t.Errorf("%s = %v, want ErrBadLevel", name, err)
		}
	}
}

// A CODE ROW READ SERVES `session` AT ITS FLOOR, AND ONLY THERE.
//
// A screen reads its own write back by asking every question of the log it
// wrote at `session`, the position the write answered with as its floor — so
// the list it just changed never comes back from a node that has not applied
// the change. Every kind a principal reads serves the level beside a position
// on the code log: a node that has not applied that position refuses `behind`
// rather than answer from before it, and once it has, answers WITH the write,
// labelled `session`. A bare `session` names nothing to wait for and is refused
// as a level; a floor on another log is the request's own mistake, refused
// before any row is read. And every other level is served as well: none of them
// is refused as a level (this node has no read index, so a linearizable read
// is refused by the LOG, never as a level).
//
// Mutation: serve a session read without its floor (ReadQuery.freshness
// dropping the floors once they are checked) → the held node answers every
// kind from before the push it was asked to include, and the first half goes
// red.
func TestACodeRowReadServesSessionAtItsFloor(t *testing.T) {
	t.Parallel()
	a, b := newTwoNodes(t)
	// THE CHART a read of changes judges their readiness by, whoever asks.
	b.chart = testChart()
	id := a.ensure("app", described("app"), t1).RepoID
	a.push(id, entry("p1", set("refs/heads/main", code.ZeroOID, oidOf("c1")),
		set("refs/heads/release", code.ZeroOID, oidOf("r1"))))
	// A CHANGE OF MAIN INTO RELEASE, whose head every push to main moves.
	c, _ := committerOn(t, a, a.pub)
	opened, err := c.ProposeChange(t.Context(), opening(id, "refs/heads/main", "refs/heads/release", asker))
	requireChanged(t, opened, err)
	b.drain()
	b.hold()
	at := a.push(id, entry("p2", set("refs/heads/main", oidOf("c1"), oidOf("c2")))).Position
	if at.IsZero() {
		t.Fatal("the push answered no position")
	}
	asked := func(kind code.ReadKind, fresh statelog.Freshness) code.ReadQuery {
		q := code.ReadQuery{Asker: asker, Kind: kind, Fresh: fresh}
		if kind != code.ReadRepos {
			q.Repo = "app"
		}
		if kind == code.ReadChange {
			q.Change = opened.Number
		}
		return q
	}
	// headOf is main's commit as each kind answers it.
	headOf := func(answer code.ReadAnswer) string {
		switch {
		case len(answer.Repos) == 1:
			return answer.Repos[0].Head
		case len(answer.Refs) > 0:
			return answer.Refs[0].OID
		case answer.Repo != nil:
			return answer.Repo.Head
		case len(answer.Pushes) > 0:
			return answer.Pushes[0].Commands[0].New
		case len(answer.Changes) == 1:
			return answer.Changes[0].Head
		case answer.Change != nil:
			return answer.Change.Head
		}
		return ""
	}
	kinds := []code.ReadKind{code.ReadRepos, code.ReadRepo, code.ReadPushes, code.ReadRefs, code.ReadChanges,
		code.ReadChange}
	session := statelog.FloorAt(statelog.ReadSession, at)
	for _, kind := range kinds {
		_, err := b.reader.Read(t.Context(), asked(kind, session))
		var refused *statelog.Refused
		if !errors.As(err, &refused) || refused.Code != statelog.RefuseBehind {
			t.Errorf("%s at session on a node that has not applied its floor = %v, want refused behind",
				kind, err)
		}
	}
	b.release()
	for _, kind := range kinds {
		answer, err := b.reader.Read(t.Context(), asked(kind, session))
		switch {
		case err != nil:
			t.Errorf("%s at session once its floor is applied: %v", kind, err)
		case answer.Level != statelog.ReadSession:
			t.Errorf("%s at session was answered at %q", kind, answer.Level)
		case headOf(answer) != oidOf("c2"):
			t.Errorf("%s at session floored at the push answered main at %q, want the push's %q",
				kind, headOf(answer), oidOf("c2"))
		}
	}

	elsewhere := statelog.Position{Stream: "CREWLET_TRACKER_LOG", Generation: 1, Seq: 4711}
	for _, kind := range kinds {
		if _, err := b.reader.Read(t.Context(), asked(kind,
			statelog.Freshness{Level: statelog.ReadSession})); !errors.Is(err, code.ErrBadLevel) {
			t.Errorf("%s at a bare session = %v, want ErrBadLevel", kind, err)
		}
		if _, err := b.reader.Read(t.Context(), asked(kind,
			statelog.FloorAt(statelog.ReadSession, elsewhere))); !errors.Is(err, statelog.ErrForeignPosition) {
			t.Errorf("%s at session floored on another log = %v, want the request's mistake", kind, err)
		}
		for _, level := range statelog.ReadLevels {
			fresh := statelog.FloorAt(level, at)
			if _, err := b.reader.Read(t.Context(), asked(kind, fresh)); errors.Is(err, code.ErrBadLevel) {
				t.Errorf("%s at %s = %v, want the level served", kind, level, err)
			}
		}
	}
}

// RESOLVE ANSWERS A DELETED REPOSITORY, so the caller can say when it can be
// restored until; an unknown name is not found; and a record this node
// deferred on the repository refuses the read rather than answer around it.
func TestResolveAnswersADeletedRepository(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	if _, err := n.store.DeleteRepo(t.Context(), n.activate(t2), id); err != nil {
		t.Fatalf("DeleteRepo: %v", err)
	}
	ref, err := n.reader.Resolve(t.Context(), code.ResolveQuery{Name: "app", Fresh: stale})
	if err != nil || ref.Status != code.RepoStatusDeleted || ref.DeletedAt.IsZero() || ref.ID != id {
		t.Fatalf("Resolve(app) = %+v, %v; want the deleted repository with its deletion's instant", ref, err)
	}
	if _, err := n.reader.Resolve(t.Context(), code.ResolveQuery{Name: "nobody", Fresh: stale}); !errors.Is(err, code.ErrRepoNotFound) {
		t.Fatalf("Resolve(nobody) = %v, want ErrRepoNotFound", err)
	}
	n.exec(`INSERT INTO code_log_deferred (position, subject, subject_kind, subject_id, version,
		payload, stored_at) VALUES (999999, ?, 'repo', ?, 9, x'7b7d', 0)`, "crewlet.code.log.repo."+id, id)
	n.exec(`INSERT INTO code_log_deferred_scope (position, path) VALUES (999999, ?)`, code.RepoPath(id))
	_, err = n.reader.Resolve(t.Context(), code.ResolveQuery{Name: "app", Fresh: stale})
	var refused *statelog.Refused
	if !errors.As(err, &refused) || refused.Code != statelog.RefuseDeferred {
		t.Fatalf("Resolve over a deferred record on the repository = %v, want refused deferred", err)
	}
}

// THE LISTING FOLLOWS THE NAME: repositories in name order, paged by name,
// each with its default branch's head — empty for an empty repository — and
// filtered by status; a limit past a page is refused, never clamped.
func TestTheListingFollowsTheName(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	names := []string{"web", "api", "app", "docs"}
	ids := map[string]string{}
	for _, name := range names {
		ids[name] = seededRepo(t, n, name)
	}
	n.push(ids["app"], entry("p1", set("refs/heads/main", code.ZeroOID, oidOf("head"))))
	if _, err := n.store.DeleteRepo(t.Context(), n.activate(t2), ids["docs"]); err != nil {
		t.Fatalf("DeleteRepo: %v", err)
	}
	var listed []string
	heads := map[string]string{}
	q := code.ReadQuery{Asker: asker, Kind: code.ReadRepos, Limit: 2}
	for {
		answer, err := n.reader.Read(t.Context(), q)
		if err != nil {
			t.Fatalf("Read(repos): %v", err)
		}
		for _, r := range answer.Repos {
			listed = append(listed, r.Name)
			heads[r.Name] = r.Head
		}
		if answer.Next == "" {
			break
		}
		q.Cursor = answer.Next
	}
	if fmt.Sprint(listed) != "[api app web]" {
		t.Fatalf("the listing is %v, want the live repositories by name", listed)
	}
	if heads["app"] != oidOf("head") || heads["api"] != "" {
		t.Fatalf("the heads are %v, want app's main and none for an empty repository", heads)
	}
	deleted, err := n.reader.Read(t.Context(), code.ReadQuery{Asker: asker, Kind: code.ReadRepos,
		Status: []code.RepoStatus{code.RepoStatusDeleted}})
	if err != nil || len(deleted.Repos) != 1 || deleted.Repos[0].Name != "docs" {
		t.Fatalf("the deleted listing = %+v, %v", deleted.Repos, err)
	}
	if _, err := n.reader.Read(t.Context(), code.ReadQuery{Asker: asker, Kind: code.ReadRepos, Limit: 501}); !errors.Is(err, code.ErrInvalid) {
		t.Fatalf("a limit of 501 = %v, want ErrInvalid", err)
	}
	// ONE BRANCH, TWO TAGS AND THREE TOMBSTONES, so no count can stand in
	// for another.
	made := []code.Command{set("refs/tags/v1", code.ZeroOID, oidOf("v1")),
		set("refs/tags/v2", code.ZeroOID, oidOf("v2"))}
	var gone []code.Command
	for _, ref := range []string{"refs/heads/a", "refs/heads/b", "refs/heads/c"} {
		made = append(made, set(ref, code.ZeroOID, oidOf(ref)))
		gone = append(gone, set(ref, oidOf(ref), code.ZeroOID))
	}
	n.push(ids["app"], entry("p2", made...))
	n.push(ids["app"], entry("p3", gone...))
	detail, err := n.reader.Read(t.Context(), code.ReadQuery{Asker: asker, Kind: code.ReadRepo, Repo: "app"})
	if err != nil || detail.Repo == nil || detail.Repo.Branches != 1 || detail.Repo.Tags != 2 ||
		detail.Repo.Tombstones != 3 || detail.Repo.Refs != 3 || detail.Repo.Pushes != 3 ||
		detail.Repo.Limits != code.DefaultLimits() {
		t.Fatalf("Read(repo app) = %+v, %v — want main, two tags, three tombstones, three pushes",
			detail.Repo, err)
	}
}

// A PUSH'S OUTCOME IS FINAL ONLY AT ITS POSITION: a node that has not applied
// the record refuses `behind` rather than answering "not found" early.
func TestPushOutcomeIsFinalOnlyAtItsPosition(t *testing.T) {
	t.Parallel()
	a, b := newTwoNodes(t)
	id := a.ensure("app", described("app"), t1).RepoID
	b.drain()
	b.hold()
	a.push(id, entry("p1", set("refs/heads/main", code.ZeroOID, oidOf("c1"))))
	at := a.waiter.Committed()
	_, err := b.reader.PushOutcome(t.Context(), code.OutcomeQuery{RepoID: id, PushID: "p1", At: at})
	var refused *statelog.Refused
	if !errors.As(err, &refused) || refused.Code != statelog.RefuseBehind {
		t.Fatalf("the outcome on a node behind the record = %v, want refused behind", err)
	}
	b.release()
	got, err := b.reader.PushOutcome(t.Context(), code.OutcomeQuery{RepoID: id, PushID: "p1", At: at})
	if err != nil || !got.Found || got.Push.PushID != "p1" || len(got.Push.Commands) != 1 {
		t.Fatalf("the outcome at its position = %+v, %v", got, err)
	}
	none, err := b.reader.PushOutcome(t.Context(), code.OutcomeQuery{RepoID: id, PushID: "never", At: at})
	if err != nil || none.Found {
		t.Fatalf("the outcome of a push no record carries = %+v, %v; want not found", none, err)
	}
	if _, err := b.reader.PushOutcome(context.Background(), code.OutcomeQuery{RepoID: id, PushID: "p1"}); !errors.Is(err, code.ErrInvalid) {
		t.Fatalf("an outcome with no position = %v, want ErrInvalid", err)
	}
}

// unitRepos is a node judging seats by [testChart], holding three
// repositories — plat-private (`visibility: unit`, Platform's, its main at
// "secret"), platform (`visibility: company`, Platform's) and sales-private
// (`visibility: unit`, Sales') — and plat-private's id.
func unitRepos(t *testing.T) (*node, string) {
	t.Helper()
	n := newNode(t)
	n.chart = testChart()
	declare := func(name, unit string, v code.Visibility) string {
		t.Helper()
		s := suiteSettings()
		s.Owner, s.Visibility = code.Owner{Kind: code.OwnerUnit, Ref: unit}, v
		return n.ensure(name, s, t1).RepoID
	}
	private := declare("plat-private", "Platform", code.VisibilityUnit)
	declare("platform", "Platform", code.VisibilityCompany)
	declare("sales-private", "Sales", code.VisibilityUnit)
	n.push(private, entry("p1", set("refs/heads/main", code.ZeroOID, oidOf("secret"))))
	return n, private
}

// A `VISIBILITY: UNIT` REPOSITORY IS READ ONLY BY ITS READERS: a seat lists,
// details and pages the history of exactly the repositories its grant reads,
// each listed with its access; a person, bound or not, reads every repository
// whatever its visibility; and a listing page of repositories the asker may
// not read is an empty page with a cursor, never the listing's end. (An
// outsider's read of one is TestAReadOfAUnitRepositoryByAnOutsiderIsNotPermitted.)
//
// Mutations: the listing not filtering → vp-sales lists plat-private; the
// cursor taken from the kept rows → vp-sales's one-row listing ends at its
// first, empty, page; the visibility read for a person → the bound person
// and the unbound token list two.
func TestAUnitRepositoryIsReadOnlyByItsReaders(t *testing.T) {
	t.Parallel()
	n, _ := unitRepos(t)

	// listing is every repository who lists, by name, at its access, paged
	// limit rows at a time; and how many pages that took.
	listing := func(who code.Principal, limit int) (map[string]string, int) {
		t.Helper()
		got := map[string]string{}
		q := code.ReadQuery{Asker: who, Kind: code.ReadRepos, Limit: limit}
		for pages := 1; pages <= 10; pages++ {
			answer, err := n.reader.Read(t.Context(), q)
			if err != nil {
				t.Fatalf("Read(repos) as %+v: %v", who, err)
			}
			for _, r := range answer.Repos {
				got[r.Name] = r.Access
			}
			if answer.Next == "" {
				return got, pages
			}
			q.Cursor = answer.Next
		}
		t.Fatalf("the listing as %+v did not end in ten pages", who)
		return nil, 0
	}
	unbound := code.Principal{Kind: code.PrincipalOperator, OperatorID: "ci"}
	everything := "map[plat-private:%[1]s platform:%[1]s sales-private:%[1]s]"
	for _, c := range []struct {
		who  code.Principal
		want string
	}{
		{agent("dev"), "map[plat-private:write platform:write]"},
		{agent("cto"), "map[plat-private:read platform:read]"},
		{agent("vp-sales"), "map[platform:read sales-private:maintain]"},
		{agent("root-bot"), "map[platform:read]"},
		{asker, fmt.Sprintf(everything, "maintain")},
		{unbound, fmt.Sprintf(everything, "read")},
	} {
		if got, _ := listing(c.who, 0); fmt.Sprint(got) != c.want {
			t.Errorf("%+v lists %v, want %s", c.who, got, c.want)
		}
	}

	// ONE ROW A PAGE: vp-sales's first page scans plat-private and keeps
	// nothing, and the listing goes on past it.
	first, err := n.reader.Read(t.Context(), code.ReadQuery{Asker: agent("vp-sales"), Kind: code.ReadRepos, Limit: 1})
	if err != nil || len(first.Repos) != 0 || first.Next != "plat-private" {
		t.Fatalf("vp-sales's first one-row page = %+v, %v; want no repository and the cursor past "+
			"plat-private", first, err)
	}
	if got, pages := listing(agent("vp-sales"), 1); fmt.Sprint(got) != "map[platform:read sales-private:maintain]" ||
		pages != 4 {
		t.Fatalf("vp-sales's one-row listing is %v in %d pages, want platform and sales-private "+
			"in four", got, pages)
	}

	for who, want := range map[string]string{"dev": "write", "cto": "read"} {
		detail, err := n.reader.Read(t.Context(), code.ReadQuery{Asker: agent(who), Kind: code.ReadRepo, Repo: "plat-private"})
		if err != nil || detail.Repo == nil || detail.Repo.Access != want || detail.Repo.Head != oidOf("secret") {
			t.Errorf("%s's detail of plat-private = %+v, %v; want its head at %s", who, detail.Repo, err, want)
		}
		pushes, err := n.reader.Read(t.Context(), code.ReadQuery{Asker: agent(who), Kind: code.ReadPushes, Repo: "plat-private"})
		if err != nil || len(pushes.Pushes) != 1 {
			t.Errorf("%s's history of plat-private = %+v, %v; want its one push", who, pushes.Pushes, err)
		}
		refs, err := n.reader.Read(t.Context(), code.ReadQuery{Asker: agent(who), Kind: code.ReadRefs, Repo: "plat-private"})
		if err != nil || len(refs.Refs) != 1 || refs.Repo == nil || refs.Repo.Access != want {
			t.Errorf("%s's refs of plat-private = %+v, %v; want main, with the detail at %s", who, refs, err, want)
		}
	}

	for name, who := range map[string]code.Principal{
		"no asker":                {},
		"a seat naming no handle": {Kind: code.PrincipalAgent},
		"an unknown kind":         {Kind: "robot", Seat: "dev"},
	} {
		_, err := n.reader.Read(t.Context(), code.ReadQuery{Asker: who, Kind: code.ReadRepos})
		if !errors.Is(err, code.ErrInvalid) || !strings.Contains(err.Error(), "asker") {
			t.Errorf("%s: Read = %v, want ErrInvalid naming the asker", name, err)
		}
	}
}

// A READ OF A UNIT REPOSITORY BY AN OUTSIDER IS NOT PERMITTED: its detail, its
// history, its refs and any one ref resolved are refused a seat the
// visibility shuts out — after the row is read, by a sentence naming why and
// who does read it — while a repository nobody declared is still not found,
// and a person resolves the same ref.
//
// Mutation: any of the four not judging the grant → vp-sales reads it.
func TestAReadOfAUnitRepositoryByAnOutsiderIsNotPermitted(t *testing.T) {
	t.Parallel()
	n, _ := unitRepos(t)
	reads := map[string]func(repo string) error{}
	for _, kind := range []code.ReadKind{code.ReadRepo, code.ReadPushes, code.ReadRefs} {
		reads[string(kind)] = func(repo string) error {
			_, err := n.reader.Read(t.Context(), code.ReadQuery{Asker: agent("vp-sales"), Kind: kind, Repo: repo})
			return err
		}
	}
	reads["ResolveRef"] = func(repo string) error {
		_, err := n.reader.ResolveRef(t.Context(), code.RefQuery{Asker: agent("vp-sales"), Repo: repo,
			Refs: []string{"main"}, Fresh: stale})
		return err
	}
	for name, read := range reads {
		err := read("plat-private")
		if !errors.Is(err, code.ErrNotPermitted) || !strings.Contains(err.Error(), "visibility: unit") ||
			!strings.Contains(err.Error(), `unit "Platform"`) {
			t.Errorf("vp-sales's %s of plat-private = %v, want ErrNotPermitted naming the visibility "+
				"and who reads it", name, err)
		}
		if err := read("nope"); !errors.Is(err, code.ErrRepoNotFound) {
			t.Errorf("vp-sales's %s of a repository nobody declared = %v, want ErrRepoNotFound", name, err)
		}
	}
	got, err := n.reader.ResolveRef(t.Context(), code.RefQuery{Asker: asker, Repo: "plat-private",
		Refs: []string{"main"}, Fresh: stale})
	if err != nil || len(got.Refs) != 1 || got.Refs[0].OID != oidOf("secret") {
		t.Fatalf("the person's resolve of plat-private's main = %+v, %v", got, err)
	}
}

// A SEAT'S READ MADE FOR A COLLEAGUE IS HELD TO THE COLLEAGUE'S GRANT TOO, on
// the node that answers it, from the one chart and the one row it judges the
// asker by: inside a turn vp-sales woke, dev — who writes plat-private — lists
// only what vp-sales reads as well, each repository at DEV's access, and its
// detail, its history, its refs (a first page and a later one) and any ref
// resolved are refused ErrAboveCeiling, naming the visibility that shuts
// vp-sales out — never ErrNotPermitted, which is false of dev. Read for cto,
// who reads it, dev is served as it is for nobody; and an asker that reads
// nothing is told so about itself, whoever it reads for.
//
// Mutations: any of the six reads judging the asker alone (the listing, the
// detail, the history, the first refs page, a later refs page, the resolve)
// → dev reads plat-private for vp-sales; the ceiling judged before the
// asker → vp-sales's read for root-bot is refused about root-bot.
func TestAReadMadeForAColleagueIsHeldToTheColleaguesGrant(t *testing.T) {
	t.Parallel()
	n, private := unitRepos(t)
	n.push(private, entry("p2", set("refs/heads/topic", code.ZeroOID, oidOf("topic"))))

	listing := func(who code.Principal, ceiling string) map[string]string {
		t.Helper()
		got := map[string]string{}
		q := code.ReadQuery{Asker: who, CeilingSeat: ceiling, Kind: code.ReadRepos, Limit: 1}
		for range 10 {
			answer, err := n.reader.Read(t.Context(), q)
			if err != nil {
				t.Fatalf("Read(repos) as %+v for %q: %v", who, ceiling, err)
			}
			for _, r := range answer.Repos {
				got[r.Name] = r.Access
			}
			if answer.Next == "" {
				return got
			}
			q.Cursor = answer.Next
		}
		t.Fatalf("the listing as %+v for %q did not end in ten pages", who, ceiling)
		return nil
	}
	for _, c := range []struct {
		who, ceiling, want string
	}{
		{"dev", "", "map[plat-private:write platform:write]"},
		{"dev", "vp-sales", "map[platform:write]"},
		{"dev", "cto", "map[plat-private:write platform:write]"},
		{"vp-sales", "dev", "map[platform:read]"},
	} {
		if got := listing(agent(c.who), c.ceiling); fmt.Sprint(got) != c.want {
			t.Errorf("%s, reading for %q, lists %v; want %s", c.who, c.ceiling, got, c.want)
		}
	}

	// A LATER PAGE OF REFS is judged on its own read of the row, never on the
	// first page's: its cursor, from dev's own listing, is no admission.
	first, firstErr := n.reader.Read(t.Context(), code.ReadQuery{Asker: agent("dev"), Kind: code.ReadRefs,
		Repo: "plat-private", Limit: 1})
	if firstErr != nil || first.Next == "" {
		t.Fatalf("dev's first one-ref page of plat-private = %+v, %v; want a cursor past it", first, firstErr)
	}
	reads := func(who code.Principal, ceiling string) map[string]func() error {
		read := func(q code.ReadQuery) func() error {
			return func() error {
				q.Asker, q.CeilingSeat, q.Repo = who, ceiling, "plat-private"
				_, err := n.reader.Read(t.Context(), q)
				return err
			}
		}
		return map[string]func() error{
			"detail":            read(code.ReadQuery{Kind: code.ReadRepo}),
			"history":           read(code.ReadQuery{Kind: code.ReadPushes}),
			"refs":              read(code.ReadQuery{Kind: code.ReadRefs}),
			"a later refs page": read(code.ReadQuery{Kind: code.ReadRefs, Limit: 1, Cursor: first.Next}),
			"resolve": func() error {
				_, err := n.reader.ResolveRef(t.Context(), code.RefQuery{Asker: who, CeilingSeat: ceiling,
					Repo: "plat-private", Refs: []string{"main"}, Fresh: stale})
				return err
			},
		}
	}
	for name, read := range reads(agent("dev"), "vp-sales") {
		err := read()
		if !errors.Is(err, code.ErrAboveCeiling) || errors.Is(err, code.ErrNotPermitted) ||
			!strings.Contains(err.Error(), `seat "vp-sales"`) || !strings.Contains(err.Error(), "visibility: unit") {
			t.Errorf("dev's %s of plat-private for vp-sales = %v, want ErrAboveCeiling naming vp-sales and "+
				"the visibility", name, err)
		}
	}
	for name, read := range reads(agent("dev"), "cto") {
		if err := read(); err != nil {
			t.Errorf("dev's %s of plat-private for cto, who reads it = %v, want it served", name, err)
		}
	}
	for name, read := range reads(agent("vp-sales"), "root-bot") {
		err := read()
		if !errors.Is(err, code.ErrNotPermitted) || !strings.Contains(err.Error(), `seat "vp-sales"`) {
			t.Errorf("vp-sales's %s of plat-private for root-bot = %v, want ErrNotPermitted about vp-sales "+
				"itself", name, err)
		}
	}
	detail, detailErr := n.reader.Read(t.Context(), code.ReadQuery{Asker: agent("dev"), CeilingSeat: "cto",
		Kind: code.ReadRepo, Repo: "plat-private"})
	if detailErr != nil || detail.Repo == nil || detail.Repo.Access != "write" {
		t.Errorf("dev's detail of plat-private for cto = %+v, %v; want it at dev's own access, write",
			detail.Repo, detailErr)
	}
}

// ONLY A SEAT'S READ IS MADE FOR ANOTHER SEAT, and only for a seat named by
// its handle: a person's read and a token's are their own, which no ceiling
// bounds, so a query naming one beside them is refused naming the field rather
// than served as the person alone — a caller that sent it meant something no
// reader does — and the engine's own reads, which name no principal, name no
// seat they read for either.
//
// Mutations: ceilingInvalid accepting a ceiling beside an operator → the
// person's listing is served; an engine's own read accepting one (the
// `expunge_due` listing's validation naming no ceiling) → that listing is
// served as though a seat had asked it for another.
func TestOnlyASeatsReadIsMadeForAnotherSeat(t *testing.T) {
	t.Parallel()
	n, _ := unitRepos(t)
	unbound := code.Principal{Kind: code.PrincipalOperator, OperatorID: "ci"}
	for name, q := range map[string]code.ReadQuery{
		"a person's listing":               {Asker: asker, CeilingSeat: "dev", Kind: code.ReadRepos},
		"a token's detail":                 {Asker: unbound, CeilingSeat: "dev", Kind: code.ReadRepo, Repo: "platform"},
		"a seat for no handle":             {Asker: agent("dev"), CeilingSeat: "Not A Handle", Kind: code.ReadRepos},
		"a duty listing for a seat":        {CeilingSeat: "dev", Kind: code.ReadCompactionDue, MinPacks: 1},
		"a publication listing for a seat": {CeilingSeat: "dev", Kind: code.ReadPublicationDue},
		"an expunge listing for a seat":    {CeilingSeat: "dev", Kind: code.ReadExpungeDue},
		"an introduced page for a seat": {CeilingSeat: "dev", Kind: code.ReadIntroduced,
			Introduced: &code.IntroducedQuery{RepoID: "id"}},
		"a run's pushes for a seat": {CeilingSeat: "dev", Kind: code.ReadRunPushes,
			RunPushes: &code.RunPushesQuery{Run: runA}},
	} {
		q.Fresh = stale
		if _, err := n.reader.Read(t.Context(), q); !errors.Is(err, code.ErrInvalid) ||
			!strings.Contains(err.Error(), "ceiling_seat:") {
			t.Errorf("%s: Read = %v, want ErrInvalid naming ceiling_seat", name, err)
		}
	}
	for name, q := range map[string]code.RefQuery{
		"a person's resolve":   {Asker: asker, CeilingSeat: "dev"},
		"a seat for no handle": {Asker: agent("dev"), CeilingSeat: "Not A Handle"},
	} {
		q.Repo, q.Refs, q.Fresh = "platform", []string{"main"}, stale
		if _, err := n.reader.ResolveRef(t.Context(), q); !errors.Is(err, code.ErrInvalid) ||
			!strings.Contains(err.Error(), "ceiling_seat:") {
			t.Errorf("%s: ResolveRef = %v, want ErrInvalid naming ceiling_seat", name, err)
		}
	}
}

// A READER WITH NO CHART REFUSES ABOUT ITSELF (M3-O82). Between the start of a
// node's code reader and the install of its first epoch — every boot, and a
// first apply that reconciles code.repos before it installs the epoch — the
// node holds no chart, and a seat's read there is refused ErrNotServing: a
// refusal about the READER, which the estate reads as a node that ran
// nothing and asks a peer, never a definite "not permitted" or a listing with
// every repository left out, which every peer holding a chart contradicts. A
// roster that answers nil and no roster at all are the same; a person's read
// asks no chart and is answered — but for a read of changes, whose readiness
// the chart judges for every asker, refused about the reader as a seat's is;
// and once the chart is there, the seat is judged by it — a chart that denies
// is the answer again.
//
// Mutation: answer AccessNone again with no chart → the seat's listing is
// served empty and every other read refused not_permitted.
func TestAReaderWithNoChartRefusesAboutItself(t *testing.T) {
	t.Parallel()
	n, private := unitRepos(t)
	unrostered, err := code.NewReader(code.ReaderOptions{Log: n.authority})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	reads := func(r *code.Reader, who code.Principal) map[string]func() error {
		read := func(q code.ReadQuery) func() error {
			return func() error {
				q.Asker = who
				_, err := r.Read(t.Context(), q)
				return err
			}
		}
		return map[string]func() error{
			"repos":  read(code.ReadQuery{Kind: code.ReadRepos}),
			"repo":   read(code.ReadQuery{Kind: code.ReadRepo, Repo: "platform"}),
			"pushes": read(code.ReadQuery{Kind: code.ReadPushes, Repo: "plat-private"}),
			"refs":   read(code.ReadQuery{Kind: code.ReadRefs, Repo: "plat-private"}),
			"resolve ref": func() error {
				_, err := r.ResolveRef(t.Context(), code.RefQuery{Asker: who, Repo: "plat-private",
					Refs: []string{"main"}, Fresh: stale})
				return err
			},
		}
	}
	n.chart = nil
	for reader, r := range map[string]*code.Reader{"a roster answering none": n.reader, "no roster": unrostered} {
		for name, read := range reads(r, agent("dev")) {
			err := read()
			if !errors.Is(err, code.ErrNotServing) || errors.Is(err, code.ErrNotPermitted) ||
				!strings.Contains(err.Error(), "holds no chart") {
				t.Errorf("%s: a seat's %s = %v, want ErrNotServing saying this node holds no chart", reader,
					name, err)
			}
		}
		for name, read := range reads(r, asker) {
			if err := read(); err != nil {
				t.Errorf("%s: a person's %s = %v, want it answered: no chart decides a person", reader, name, err)
			}
		}
		// A READ OF CHANGES judges their readiness by the chart whoever asks
		// — an approver's eligibility is the chart's — so a person's is
		// refused about the reader too; a page of a change's part, which
		// carries no readiness, asks none.
		for name, q := range map[string]code.ReadQuery{
			"a listing of changes": {Kind: code.ReadChanges, Repo: "platform"},
			"a change":             {Kind: code.ReadChange, Repo: "platform", Change: 1},
		} {
			q.Asker, q.Fresh = asker, stale
			if _, err := r.Read(t.Context(), q); !errors.Is(err, code.ErrNotServing) ||
				!strings.Contains(err.Error(), "a change's readiness") {
				t.Errorf("%s: a person's %s = %v, want ErrNotServing saying no chart judges a readiness", reader,
					name, err)
			}
		}
		if _, err := r.Read(t.Context(), code.ReadQuery{Asker: asker, Kind: code.ReadChange, Repo: "platform",
			Change: 1, Part: code.PartComments, Fresh: stale}); errors.Is(err, code.ErrNotServing) {
			t.Errorf("%s: a person's page of a change's comments = %v, want it judged by no chart", reader, err)
		}
	}

	n.chart = testChart()
	for name, read := range reads(n.reader, agent("dev")) {
		if err := read(); err != nil {
			t.Errorf("with the chart, dev's %s = %v, want it answered", name, err)
		}
	}
	if err := reads(n.reader, agent("vp-sales"))["refs"](); !errors.Is(err, code.ErrNotPermitted) {
		t.Errorf("with the chart, vp-sales's refs of %s = %v, want the chart's own refusal", private, err)
	}
}
