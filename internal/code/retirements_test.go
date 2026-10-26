package code_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A SNAPSHOT'S RETIREMENTS, on the whole path: a real broker, store, applier
// and reader. The retired rows are SEEDED (seedRetiredPacks), each case's at
// the positions and broker instants it is about, because what the reader
// serves of them is a question about those two columns and the repository's
// retired mark — which a real retire_sweep record raises here — and never
// about the record that retired them.

// snapshotFrom follows a snapshot from the page q asks to its end on one
// reader, answering every page or the first error a page met.
func snapshotFrom(t *testing.T, r *code.Reader, q code.StateQuery) ([]code.RepoSnapshot, error) {
	t.Helper()
	var pages []code.RepoSnapshot
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

// retiredOf is every retirement a snapshot's pages served, in the order they
// served them, each as "<pack id>@<sequence of the record that retired it>".
func retiredOf(pages []code.RepoSnapshot) []string {
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

// A DELTA FROM BELOW THE RETIRED MARK IS ANSWERED FULL. A sweep removed the
// rows that told of every retirement at or below the mark, so a delta from
// below it could no longer state every pack retired since its floor — the
// cache it answers would keep serving packs no row names — and is answered in
// full instead, serving no retirement the caller did not ask for. A delta from
// at the mark is still a delta.
//
// Mutation: drop the retired mark from page 1's choice, and the delta from
// below it is a delta that never names the two packs the sweep forgot.
func TestADeltaFromBelowTheRetiredMarkIsAnsweredFull(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	below := n.push(id, entry("p1", set("refs/heads/main", code.ZeroOID, oidOf("c1")))).Position
	retiring := n.push(id, entry("p2", set("refs/heads/main", oidOf("c1"), oidOf("c2")))).Position
	n.seedRetiredPacks(id, retiring.Packed(), harnessAt.Add(-code.RetireGrace-time.Hour), "gone-a", "gone-b")
	n.push(id, entry("p3", set("refs/heads/main", oidOf("c2"), oidOf("c3"))))
	// ROOM FOR A DELTA: ten more packs, so no delta here outweighs the full
	// answer and only the mark can make one full.
	n.seedRefs(id, 0, 10)

	// THE PREMISE: unswept, the retirements are what a delta from below them
	// serves.
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

// A DELTA SERVES ITS RETIREMENTS, AND A FULL ANSWER THOSE ABOVE ITS CALLER.
//
// A delta from Since serves, in a retired phase after its refs, every pack a
// record retired in (Since, its position] — each once across pages of two
// rows, in (position, pack id, part) order and with RetiredFrom = Since on
// every page; never one retired AT Since, which the round that synced to Since
// already learned of, and never one retired after page 1's position while the
// snapshot is read, which the next delta serves. A full answer serves the
// retirements above the RetiredSince it is asked with, and says so in
// RetiredFrom on every page, exactly when it can serve every one of them: a
// position on this generation, at or past the repository's retired mark, with
// no more retirements above it than the full answer has rows. Otherwise it
// serves none and RetiredFrom is zero.
//
// Mutation: serve retirements past page 1's position (a tail) → the late one
// is served; read the floor inclusively → the one at Since is served; enter
// the phase on a delta alone → the full answers serve none; honour a
// RetiredSince below the mark or on another generation → the answer claims a
// floor its own later pages refuse; honour one past the count → it claims a
// floor and serves more retirements than it has rows.
func TestADeltaServesItsRetirementsAndAFullAnswerThoseAboveItsCaller(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	recent := harnessAt.Add(-time.Hour)

	// THE MARK: one retirement past the grace, swept.
	marked := n.push(id, entry("p0", set("refs/heads/main", code.ZeroOID, oidOf("c0")))).Position
	n.seedRetiredPacks(id, marked.Packed(), harnessAt.Add(-code.RetireGrace-time.Hour), "swept")
	if swept, err := n.store.RetireSweep(t.Context(), id, harnessAt); err != nil || swept.Rows != 1 || !swept.Held() {
		t.Fatalf("the sweep = %+v, %v; want its one row removed by one applied record", swept, err)
	}
	since := n.push(id, entry("p1", set("refs/heads/main", oidOf("c0"), oidOf("c1")))).Position
	n.seedRetiredPacks(id, since.Packed(), recent, "at-since")
	p2 := n.push(id, entry("p2", set("refs/heads/main", oidOf("c1"), oidOf("c2")))).Position
	n.seedRetiredPacks(id, p2.Packed(), recent, "c2", "a2", "b2")
	p3 := n.push(id, entry("p3", set("refs/heads/main", oidOf("c2"), oidOf("c3")))).Position
	n.seedRetiredPacks(id, p3.Packed(), recent, "b3", "a3")
	// ROOM FOR THEM: ten more packs, so a full answer's rows outnumber every
	// retirement until the case adds more.
	n.seedRefs(id, 0, 10)
	code.SetPageLimits(n.reader, 2, code.MaxPageBytes)

	var late statelog.Position
	q := code.StateQuery{RepoID: id, Since: since, Fresh: stale}
	var pages []code.RepoSnapshot
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
			// A RETIREMENT AFTER PAGE 1, while the snapshot is read: the
			// next delta's, as the pushes after it are.
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

	full := func(name string, retiredSince statelog.Position) []code.RepoSnapshot {
		t.Helper()
		pages, err := snapshotFrom(t, n.reader, code.StateQuery{RepoID: id, Fresh: stale, RetiredSince: retiredSince})
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
					"retirement above %v", name, i+1, retiredOf([]code.RepoSnapshot{p}), p.RetiredFrom, retiredSince)
			}
		}
	}

	// EVERY RETIREMENT ABOVE IT, the late one now among them: the answer is
	// as of a position past it.
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

	// OUTWEIGHED: twenty more retirements than the answer has room for.
	var more []string
	for i := range 20 {
		more = append(more, fmt.Sprintf("more-%02d", i))
	}
	n.seedRetiredPacks(id, late.Packed(), recent, more...)
	refused("past the full answer's rows", since)
}

// A SWEEP UNDER A SNAPSHOT MOVES IT. A retire_sweep that raises the
// repository's retired mark past a snapshot's retired floor between its pages
// removed rows the snapshot still had to serve, so its next page answers
// ErrSnapshotMoved — a delta's floor is its Since, a full answer's the
// RetiredSince it honoured — and the snapshot asked again decides afresh. A
// full answer that serves no retirements has nothing a sweep can take, and
// reads on.
//
// Mutation: drop the check, and the delta and the full answer each finish
// without the retirements the sweep removed under them; check the mark
// against a zero floor too, and the full answer that serves none is moved.
func TestASweepUnderASnapshotMovesIt(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	since := n.push(id, entry("p1", set("refs/heads/main", code.ZeroOID, oidOf("c1")))).Position
	first := n.push(id, entry("p2", set("refs/heads/main", oidOf("c1"), oidOf("c2")))).Position
	n.seedRetiredPacks(id, first.Packed(), harnessAt.Add(-code.RetireGrace-time.Hour), "first-a", "first-b")
	second := n.push(id, entry("p3", set("refs/heads/main", oidOf("c2"), oidOf("c3")))).Position
	n.seedRetiredPacks(id, second.Packed(), harnessAt.Add(-code.RetireGrace+time.Hour), "second")
	// ROOM FOR A DELTA: ten more packs, so the delta here never outweighs
	// the full answer.
	n.seedRefs(id, 0, 10)
	code.SetPageLimits(n.reader, 1, code.MaxPageBytes)

	pageOne := func(name string, q code.StateQuery) code.RepoSnapshot {
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
	page := pageOne("the delta", code.StateQuery{RepoID: id, Since: since, Fresh: stale})
	if page.Full || page.RetiredFrom != since {
		t.Fatalf("the premise: page 1 of a delta from %v serving retirements from it, got full %v from %v",
			since, page.Full, page.RetiredFrom)
	}
	sweep(harnessAt, first)
	if _, err := n.reader.Snapshot(t.Context(), code.StateQuery{RepoID: id, Cursor: page.Next}); !errors.Is(err, code.ErrSnapshotMoved) {
		t.Fatalf("a later page of the delta after a sweep raised the mark past its floor = %v, "+
			"want ErrSnapshotMoved", err)
	}
	if again := snapshot(t, n.reader, id, since); !again[0].Full {
		t.Fatal("the delta asked again from below the raised mark was a delta")
	}

	// A FULL ANSWER honouring the mark itself: the second retirement is above it.
	page = pageOne("the full answer", code.StateQuery{RepoID: id, Fresh: stale, RetiredSince: first})
	if !page.Full || page.RetiredFrom != first {
		t.Fatalf("the premise: page 1 of a full answer serving retirements from %v, got full %v from %v",
			first, page.Full, page.RetiredFrom)
	}
	sweep(harnessAt.Add(2*time.Hour), second)
	if _, err := n.reader.Snapshot(t.Context(), code.StateQuery{RepoID: id, Cursor: page.Next}); !errors.Is(err, code.ErrSnapshotMoved) {
		t.Fatalf("a later page of the full answer after a sweep raised the mark past its retired floor = %v, "+
			"want ErrSnapshotMoved", err)
	}

	// A FULL ANSWER SERVING NONE reads on past a sweep.
	page = pageOne("the full answer serving none", code.StateQuery{RepoID: id, Fresh: stale})
	if !page.Full || !page.RetiredFrom.IsZero() {
		t.Fatalf("the premise: page 1 of a full answer serving no retirements, got full %v from %v",
			page.Full, page.RetiredFrom)
	}
	third := n.push(id, entry("p4", set("refs/heads/main", oidOf("c3"), oidOf("c4")))).Position
	n.seedRetiredPacks(id, third.Packed(), harnessAt.Add(-code.RetireGrace-time.Hour), "third")
	sweep(harnessAt.Add(2*time.Hour), third)
	if _, err := snapshotFrom(t, n.reader, code.StateQuery{RepoID: id, Cursor: page.Next}); err != nil {
		t.Fatalf("a later page of a full answer serving no retirements, after a sweep = %v", err)
	}
}
