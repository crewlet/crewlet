package code_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

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

// seedRefs writes count live refs and packs packs straight into a node's
// estate at position 1 — the bulk a paging case needs.
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
	for i := range 4 {
		n.push(id, entry(fmt.Sprintf("p%d", i), set(fmt.Sprintf("refs/heads/r%d", i), code.ZeroOID, oidOf(fmt.Sprint(i)))))
	}
	code.SetPageLimits(n.reader, 2, code.MaxPageBytes)
	page, err := n.reader.Snapshot(t.Context(), code.StateQuery{RepoID: id, Fresh: stale})
	if err != nil || page.Next == nil {
		t.Fatalf("page 1: %v", err)
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
	} {
		bad := *page.Next
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
	q := code.ReadQuery{Kind: code.ReadPushes, Repo: "app", Limit: 200}
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
		settings.Protections = append(settings.Protections, code.Protection{
			Pattern:    head + strings.Repeat("x", code.MaxRefNameBytes-len(head)),
			DirectPush: code.DirectPersons,
		})
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
	q := code.ReadQuery{Kind: code.ReadRepos}
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
	q := code.ReadQuery{Kind: code.ReadPushes, Repo: "app", Limit: 10}
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

// A CODE READ STATES ITS FRESHNESS: a point read and a snapshot's first page
// name their level, a session read names the position it waits for, and a
// person's listing is never a session read.
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
		"Read at session": func() error {
			_, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadRepos,
				Fresh: statelog.Freshness{Level: statelog.ReadSession, MinPosition: n.waiter.Committed()}})
			return err
		},
	} {
		if err := read(); !errors.Is(err, code.ErrBadLevel) {
			t.Errorf("%s = %v, want ErrBadLevel", name, err)
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
	q := code.ReadQuery{Kind: code.ReadRepos, Limit: 2}
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
	deleted, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadRepos,
		Status: []code.RepoStatus{code.RepoStatusDeleted}})
	if err != nil || len(deleted.Repos) != 1 || deleted.Repos[0].Name != "docs" {
		t.Fatalf("the deleted listing = %+v, %v", deleted.Repos, err)
	}
	if _, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadRepos, Limit: 501}); !errors.Is(err, code.ErrInvalid) {
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
	detail, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadRepo, Repo: "app"})
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
