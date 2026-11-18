package gitlet_test

import (
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY CODE READ IS STAMPED THROUGH THE BARRIERS ITS NODE WAS RELEASED PAST.
// A node reaches a barrier when it decodes it and commits it later, so its
// checkpoint row can lag a barrier a read waited for and was released at. A
// barrier writes no row, so the read's rows are whole through it and its stamp
// must say so: stamped at the checkpoint row alone, a linearizable answer comes
// back below one another node gave on the same rows, and a cache takes it for
// its log gone back. Every read here is floored at a barrier reached with its
// commit held, so a stamp below the floor is the checkpoint row alone.
func TestEveryCodeReadIsStampedThroughTheBarriersItsNodeReached(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	pushed := n.push(id, entry("p1", set("refs/heads/main", gitlet.ZeroOID, oidOf("c1"))))
	n.holdBarrierCommits()
	barrier := n.barrier()
	n.drain()
	if got := n.waiter.Committed(); got != pushed.Position {
		t.Fatalf("the checkpoint is at %s, want it held at the push at %s: the case held no barrier's commit",
			got, pushed.Position)
	}
	if got := n.waiter.Reached(); got != barrier {
		t.Fatalf("the node reached %s, want the barrier at %s", got, barrier)
	}
	ctx := t.Context()
	ask := statelog.FloorAt(statelog.ReadStale, barrier)
	for _, tc := range []struct {
		what string
		read func() ([]statelog.Position, error)
	}{
		{"Resolve", func() ([]statelog.Position, error) {
			ref, err := n.reader.Resolve(ctx, gitlet.ResolveQuery{Name: "app", Fresh: ask})
			return []statelog.Position{ref.Position}, err
		}},
		{"Snapshot", func() ([]statelog.Position, error) {
			page, err := n.reader.Snapshot(ctx, gitlet.StateQuery{RepoID: id, Fresh: ask})
			return []statelog.Position{page.Position, page.Repo.Position}, err
		}},
		{"Read(repo)", func() ([]statelog.Position, error) {
			answer, err := n.reader.Read(ctx, gitlet.ReadQuery{Asker: asker, Kind: gitlet.ReadRepo, Repo: "app",
				Fresh: ask})
			return []statelog.Position{answer.Position}, err
		}},
		{"ResolveRef", func() ([]statelog.Position, error) {
			answer, err := n.reader.ResolveRef(ctx, gitlet.RefQuery{Asker: asker, Repo: "app",
				Refs: []string{"main"}, Fresh: ask})
			return []statelog.Position{answer.Position, answer.Repo.Position}, err
		}},
		{"PushOutcome", func() ([]statelog.Position, error) {
			out, err := n.reader.PushOutcome(ctx, gitlet.OutcomeQuery{RepoID: id, PushID: "p1", At: barrier})
			return []statelog.Position{out.Position}, err
		}},
	} {
		t.Run(tc.what, func(t *testing.T) {
			stamps, err := tc.read()
			if err != nil {
				t.Fatalf("floored at the barrier at %s: %v", barrier, err)
			}
			for _, at := range stamps {
				if at.Packed() < barrier.Packed() {
					t.Errorf("floored at the barrier at %s, which this node reached and has not committed, "+
						"it was stamped %s: the checkpoint row, below the floor it was served at", barrier, at)
				}
			}
		})
	}
}

// A SNAPSHOT'S PAGES READ ON TWO NODES ARE ONE HISTORY whichever has committed
// the barriers it was read past: a later page reads at session floored at the
// highest stamp a page was read at, and its node stamps the page at or past
// that floor. Stamped at its checkpoint row alone, a node that reached the
// floor's barrier without committing it would stamp below the floor, and the
// next page would refuse its cursor as one no reader issues.
func TestASnapshotsPagesStayOneHistoryOnANodeThatHasNotCommittedItsBarriers(t *testing.T) {
	t.Parallel()
	a, b := newTwoNodes(t)
	id := a.ensure("app", described("app"), t1).RepoID
	const branches = 4
	for i := range branches {
		a.push(id, entry(fmt.Sprintf("s-%d", i),
			set(fmt.Sprintf("refs/heads/s%d", i), gitlet.ZeroOID, oidOf(fmt.Sprint(i)))))
	}
	b.drain()
	b.holdBarrierCommits()
	barrier := a.barrier()
	a.drain()
	b.drain()
	if got := b.waiter.Committed(); got.Packed() >= barrier.Packed() {
		t.Fatalf("node B committed %s, at or past the barrier at %s: the case held no barrier's commit",
			got, barrier)
	}
	gitlet.SetPageLimits(a.reader, 2, gitlet.MaxPageBytes)
	gitlet.SetPageLimits(b.reader, 2, gitlet.MaxPageBytes)

	page, err := a.reader.Snapshot(t.Context(), gitlet.StateQuery{RepoID: id, Fresh: stale})
	switch {
	case err != nil || page.Next == nil:
		t.Fatalf("page 1 on A = %v; want a page with more to come", err)
	case page.Position != barrier:
		t.Fatalf("page 1 on A, which committed the barrier at %s, was stamped %s", barrier, page.Position)
	}
	// ALTERNATING, B first: each page B serves is floored at a stamp A's
	// committed barrier set.
	nodes := []struct {
		name   string
		reader *gitlet.Reader
	}{{"B", b.reader}, {"A", a.reader}}
	pages := []gitlet.RepoSnapshot{page}
	for i := 0; page.Next != nil; i++ {
		on := nodes[i%len(nodes)]
		page, err = on.reader.Snapshot(t.Context(), gitlet.StateQuery{RepoID: id, Cursor: page.Next})
		if err != nil {
			t.Fatalf("page %d, on %s: %v", len(pages)+1, on.name, err)
		}
		if page.Next != nil && page.Next.Seen.Packed() < barrier.Packed() {
			t.Errorf("page %d, on %s, was read at session floored at the barrier at %s and set the next "+
				"page's floor to %s", len(pages)+1, on.name, barrier, page.Next.Seen)
		}
		pages = append(pages, page)
		if len(pages) > 50 {
			t.Fatal("the snapshot did not end in fifty pages")
		}
	}
	if len(pages) < 3 {
		t.Fatalf("the snapshot ended in %d pages: no page was read on A after one on B", len(pages))
	}
	u := unionOf(pages)
	for i := range branches {
		if ref := fmt.Sprintf("refs/heads/s%d", i); u.refs[ref] != oidOf(fmt.Sprint(i)) {
			t.Errorf("the snapshot serves %s at %q, want %s", ref, u.refs[ref], oidOf(fmt.Sprint(i)))
		}
	}
}
