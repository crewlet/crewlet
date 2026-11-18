package gitlet_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A SNAPSHOT'S RETIREMENTS, on a real broker, store, applier and reader. The
// retired rows are SEEDED (seedRetiredPacks) at the positions and instants each
// case is about: what the reader serves turns on those columns and the retired
// mark (raised here by a real retire_sweep), never on the retiring record.

// snapshotFrom follows a snapshot from q's page to its end, answering every
// page or the first error.
func snapshotFrom(t *testing.T, r *gitlet.Reader, q gitlet.StateQuery) ([]gitlet.RepoSnapshot, error) {
	t.Helper()
	var pages []gitlet.RepoSnapshot
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

// retiredOf is every retirement the pages served, in order, each as
// "<pack id>@<retiring record's sequence>".
func retiredOf(pages []gitlet.RepoSnapshot) []string {
	var out []string
	for _, p := range pages {
		for _, r := range p.Retired {
			out = append(out, fmt.Sprintf("%s@%d", r.PackID, r.Position.Seq))
		}
	}
	return out
}

// retiredBy names packs as retiredOf spells them, all retired at position.
func retiredBy(position statelog.Position, packs ...string) []string {
	out := make([]string, len(packs))
	for i, p := range packs {
		out[i] = fmt.Sprintf("%s@%d", p, position.Seq)
	}
	return out
}

// A DELTA FROM BELOW THE RETIRED MARK IS ANSWERED FULL. The sweep removed the
// rows of every retirement at or below the mark, so such a delta could not name
// every pack retired since its floor, and its cache would keep serving packs no
// row names. The full answer serves no retirement unasked; a delta from at the
// mark stays a delta.
//
// Mutation: drop the mark from page 1's choice → the delta never names the
// two packs the sweep forgot.
func TestADeltaFromBelowTheRetiredMarkIsAnsweredFull(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	below := n.push(id, entry("p1", set("refs/heads/main", gitlet.ZeroOID, oidOf("c1")))).Position
	retiring := n.push(id, entry("p2", set("refs/heads/main", oidOf("c1"), oidOf("c2")))).Position
	n.seedRetiredPacks(id, retiring.Packed(), harnessAt.Add(-gitlet.RetireGrace-time.Hour), "gone-a", "gone-b")
	n.push(id, entry("p3", set("refs/heads/main", oidOf("c2"), oidOf("c3"))))
	// ROOM FOR A DELTA: ten more packs, so only the mark can make one full.
	n.seedRefs(id, 0, 10)

	// THE PREMISE: unswept, a delta from below serves the retirements.
	pages := snapshot(t, n.reader, id, below)
	if pages[0].Full || !slices.Equal(retiredOf(pages), retiredBy(retiring, "gone-a", "gone-b")) {
		t.Fatalf("the premise: a delta from below two unswept retirements = full %v serving %v",
			pages[0].Full, retiredOf(pages))
	}

	swept, err := n.store.RetireSweep(t.Context(), id, harnessAt)
	if err != nil || swept.Rows != 2 || !swept.Held() {
		t.Fatalf("the sweep = %+v, %v; want both rows removed by one applied record", swept, err)
	}
	if mark := int64(n.retiredThrough(id)); mark != retiring.Packed() {
		t.Fatalf("the repository's retired mark is %d, want the swept retirement's %d", mark, retiring.Packed())
	}

	pages = snapshot(t, n.reader, id, below)
	if !pages[0].Full {
		t.Fatalf("a delta from below the retired mark was answered as a delta serving %v — "+
			"the two retirements the sweep forgot are in no answer", retiredOf(pages))
	}
	for i, p := range pages {
		if len(p.Retired) != 0 || !p.RetiredFrom.IsZero() {
			t.Fatalf("page %d of the full answer serves %v from %v, and its caller asked for no retirements",
				i+1, p.Retired, p.RetiredFrom)
		}
	}
	if delta := snapshot(t, n.reader, id, retiring); delta[0].Full {
		t.Fatal("a delta from at the retired mark was answered in full")
	}
}

// A DELTA SERVES ITS RETIREMENTS, AND A FULL ANSWER THOSE ABOVE ITS CALLER. A
// delta from Since serves, after its refs, each pack retired in (Since, page 1's
// position] once, in (position, pack id, part) order, RetiredFrom = Since on
// every page: not one AT Since (already synced), not one after page 1 (the next
// delta's). A full answer serves those above RetiredSince, with RetiredFrom =
// RetiredSince, only when it can serve all (same generation, at or past the
// mark, at most MaxFullRetirements above, which may exceed its rows); else none
// and RetiredFrom zero. Mutation: serve a tail → the late one; inclusive floor →
// the one at Since; the phase on deltas only → full answers serve none; honour
// a RetiredSince below the mark or on another generation → a floor later pages
// refuse; past the bound → a floor and over a page served; bound by the
// answer's rows → the twenty-six beside sixteen refused.
func TestADeltaServesItsRetirementsAndAFullAnswerThoseAboveItsCaller(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	recent := harnessAt.Add(-time.Hour)

	// THE MARK: one retirement past the grace, swept.
	marked := n.push(id, entry("p0", set("refs/heads/main", gitlet.ZeroOID, oidOf("c0")))).Position
	n.seedRetiredPacks(id, marked.Packed(), harnessAt.Add(-gitlet.RetireGrace-time.Hour), "swept")
	if swept, err := n.store.RetireSweep(t.Context(), id, harnessAt); err != nil || swept.Rows != 1 || !swept.Held() {
		t.Fatalf("the sweep = %+v, %v; want its one row removed by one applied record", swept, err)
	}
	since := n.push(id, entry("p1", set("refs/heads/main", oidOf("c0"), oidOf("c1")))).Position
	n.seedRetiredPacks(id, since.Packed(), recent, "at-since")
	p2 := n.push(id, entry("p2", set("refs/heads/main", oidOf("c1"), oidOf("c2")))).Position
	n.seedRetiredPacks(id, p2.Packed(), recent, "c2", "a2", "b2")
	p3 := n.push(id, entry("p3", set("refs/heads/main", oidOf("c2"), oidOf("c3")))).Position
	n.seedRetiredPacks(id, p3.Packed(), recent, "b3", "a3")
	// ROOM FOR A DELTA: ten more packs, so the delta from Since never
	// outweighs the full answer's sixteen rows.
	n.seedRefs(id, 0, 10)
	gitlet.SetPageLimits(n.reader, 2, gitlet.MaxPageBytes)

	var late statelog.Position
	q := gitlet.StateQuery{RepoID: id, Since: since, Fresh: stale}
	var pages []gitlet.RepoSnapshot
	for page := 0; ; page++ {
		if page > 40 {
			t.Fatal("the delta did not end in forty pages")
		}
		got, err := n.reader.Snapshot(t.Context(), q)
		if err != nil {
			t.Fatalf("Snapshot page %d: %v", page+1, err)
		}
		if got.Full {
			t.Fatalf("the premise: the snapshot from %v is a delta", since)
		}
		if got.RetiredFrom != since {
			t.Fatalf("page %d of the delta serves retirements from %v, want its own floor %v",
				page+1, got.RetiredFrom, since)
		}
		pages = append(pages, got)
		if page == 0 {
			// A RETIREMENT AFTER PAGE 1: the next delta's.
			late = n.push(id, entry("p4", set("refs/heads/main", oidOf("c3"), oidOf("c4")))).Position
			n.seedRetiredPacks(id, late.Packed(), recent, "late")
		}
		if got.Next == nil {
			break
		}
		q.Cursor = got.Next
	}
	want := slices.Concat(retiredBy(p2, "a2", "b2", "c2"), retiredBy(p3, "a3", "b3"))
	if got := retiredOf(pages); !slices.Equal(got, want) {
		t.Fatalf("the delta's retired phase served %v, want %v: each once, in (position, pack, part) "+
			"order, above Since and at or below page 1's position", got, want)
	}
	if served := pages[0].Retired; len(pages) < 3 || len(served) != 0 {
		t.Fatalf("the premise: the retirements span pages after the refs, and page 1 of %d serves %v",
			len(pages), served)
	}
	for _, p := range pages {
		for _, r := range p.Retired {
			if r.Part != "pack" || r.GitChecksum != oidOf(r.PackID) || !r.RetiredAt.Equal(recent) {
				t.Fatalf("the retirement of %s is served as %+v, want part pack, its checksum and "+
					"its record's instant", r.PackID, r)
			}
		}
	}

	full := func(name string, retiredSince statelog.Position) []gitlet.RepoSnapshot {
		t.Helper()
		pages, err := snapshotFrom(t, n.reader, gitlet.StateQuery{RepoID: id, Fresh: stale, RetiredSince: retiredSince})
		if err != nil || !pages[0].Full {
			t.Fatalf("%s: a full answer = %v (full %v)", name, err, len(pages) > 0 && pages[0].Full)
		}
		return pages
	}
	honoured := func(name string, retiredSince statelog.Position, want []string) {
		t.Helper()
		pages := full(name, retiredSince)
		for i, p := range pages {
			if p.RetiredFrom != retiredSince {
				t.Fatalf("%s: page %d says its retirements are from %v, want %v", name, i+1,
					p.RetiredFrom, retiredSince)
			}
		}
		if got := retiredOf(pages); !slices.Equal(got, want) {
			t.Fatalf("%s: the full answer served %v, want %v", name, got, want)
		}
	}
	refused := func(name string, retiredSince statelog.Position) {
		t.Helper()
		for i, p := range full(name, retiredSince) {
			if len(p.Retired) != 0 || !p.RetiredFrom.IsZero() {
				t.Fatalf("%s: page %d serves %v from %v, where the reader cannot serve every "+
					"retirement above %v", name, i+1, retiredOf([]gitlet.RepoSnapshot{p}), p.RetiredFrom, retiredSince)
			}
		}
	}

	// EVERY RETIREMENT ABOVE IT, the late one included: the answer is past it.
	honoured("from Since", since, slices.Concat(want, retiredBy(late, "late")))
	// AT THE MARK it is still honoured, and the retirement at Since is above it.
	honoured("from the mark", marked, slices.Concat(retiredBy(since, "at-since"), want, retiredBy(late, "late")))

	beneath := marked
	beneath.Seq--
	refused("from below the mark", beneath)
	elsewhere := since
	elsewhere.Generation += 7
	refused("from another generation", elsewhere)
	refused("from nothing", statelog.Position{})

	// BEYOND THE ANSWER'S ROWS, within the bound: twenty-six retirements
	// beside sixteen rows, all served.
	numbered := func(from, to int) []string {
		var out []string
		for i := from; i < to; i++ {
			out = append(out, fmt.Sprintf("more-%05d", i))
		}
		return out
	}
	n.seedRetiredPacks(id, late.Packed(), recent, numbered(0, 20)...)
	honoured("past the full answer's rows", since,
		slices.Concat(want, retiredBy(late, append([]string{"late"}, numbered(0, 20)...)...)))

	// PAST THE BOUND: one more retirement above Since than MaxFullRetirements.
	above := len(want) + 1 + 20
	n.seedRetiredPacks(id, late.Packed(), recent, numbered(20, 20+gitlet.MaxFullRetirements+1-above)...)
	refused("past MaxFullRetirements", since)
}

// A SWEEP UNDER A SNAPSHOT MOVES IT. A retire_sweep raising the mark past the
// snapshot's retired floor (a delta's Since, a full answer's honoured
// RetiredSince) between pages removed rows it had to serve, so the next page
// answers ErrSnapshotMoved. A full answer serving no retirements reads on.
//
// Mutation: drop the check → both finish short of the swept retirements; check
// a zero floor too → the full answer serving none is moved.
func TestASweepUnderASnapshotMovesIt(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	since := n.push(id, entry("p1", set("refs/heads/main", gitlet.ZeroOID, oidOf("c1")))).Position
	first := n.push(id, entry("p2", set("refs/heads/main", oidOf("c1"), oidOf("c2")))).Position
	n.seedRetiredPacks(id, first.Packed(), harnessAt.Add(-gitlet.RetireGrace-time.Hour), "first-a", "first-b")
	second := n.push(id, entry("p3", set("refs/heads/main", oidOf("c2"), oidOf("c3")))).Position
	n.seedRetiredPacks(id, second.Packed(), harnessAt.Add(-gitlet.RetireGrace+time.Hour), "second")
	// ROOM FOR A DELTA: ten more packs, so it never outweighs the full answer.
	n.seedRefs(id, 0, 10)
	gitlet.SetPageLimits(n.reader, 1, gitlet.MaxPageBytes)

	pageOne := func(name string, q gitlet.StateQuery) gitlet.RepoSnapshot {
		t.Helper()
		page, err := n.reader.Snapshot(t.Context(), q)
		if err != nil || page.Next == nil {
			t.Fatalf("%s: page 1 = next %+v, %v; want a page with more to come", name, page.Next, err)
		}
		return page
	}
	sweep := func(now time.Time, through statelog.Position) {
		t.Helper()
		swept, err := n.store.RetireSweep(t.Context(), id, now)
		if err != nil || swept.Rows == 0 || !swept.Held() {
			t.Fatalf("the sweep at %v = %+v, %v; want rows removed by an applied record", now, swept, err)
		}
		if mark := int64(n.retiredThrough(id)); mark != through.Packed() {
			t.Fatalf("the retired mark is %d after the sweep, want %d", mark, through.Packed())
		}
	}

	// A DELTA: its floor is Since, below the first retirement's position.
	page := pageOne("the delta", gitlet.StateQuery{RepoID: id, Since: since, Fresh: stale})
	if page.Full || page.RetiredFrom != since {
		t.Fatalf("the premise: page 1 of a delta from %v serving retirements from it, got full %v from %v",
			since, page.Full, page.RetiredFrom)
	}
	sweep(harnessAt, first)
	if _, err := n.reader.Snapshot(t.Context(), gitlet.StateQuery{RepoID: id, Cursor: page.Next}); !errors.Is(err, gitlet.ErrSnapshotMoved) {
		t.Fatalf("a later page of the delta after a sweep raised the mark past its floor = %v, "+
			"want ErrSnapshotMoved", err)
	}
	if again := snapshot(t, n.reader, id, since); !again[0].Full {
		t.Fatal("the delta asked again from below the raised mark was a delta")
	}

	// A FULL ANSWER honouring the mark itself: the second retirement is above it.
	page = pageOne("the full answer", gitlet.StateQuery{RepoID: id, Fresh: stale, RetiredSince: first})
	if !page.Full || page.RetiredFrom != first {
		t.Fatalf("the premise: page 1 of a full answer serving retirements from %v, got full %v from %v",
			first, page.Full, page.RetiredFrom)
	}
	sweep(harnessAt.Add(2*time.Hour), second)
	if _, err := n.reader.Snapshot(t.Context(), gitlet.StateQuery{RepoID: id, Cursor: page.Next}); !errors.Is(err, gitlet.ErrSnapshotMoved) {
		t.Fatalf("a later page of the full answer after a sweep raised the mark past its retired floor = %v, "+
			"want ErrSnapshotMoved", err)
	}

	// A FULL ANSWER SERVING NONE reads on past a sweep.
	page = pageOne("the full answer serving none", gitlet.StateQuery{RepoID: id, Fresh: stale})
	if !page.Full || !page.RetiredFrom.IsZero() {
		t.Fatalf("the premise: page 1 of a full answer serving no retirements, got full %v from %v",
			page.Full, page.RetiredFrom)
	}
	third := n.push(id, entry("p4", set("refs/heads/main", oidOf("c3"), oidOf("c4")))).Position
	n.seedRetiredPacks(id, third.Packed(), harnessAt.Add(-gitlet.RetireGrace-time.Hour), "third")
	sweep(harnessAt.Add(2*time.Hour), third)
	if _, err := snapshotFrom(t, n.reader, gitlet.StateQuery{RepoID: id, Cursor: page.Next}); err != nil {
		t.Fatalf("a later page of a full answer serving no retirements, after a sweep = %v", err)
	}
}
