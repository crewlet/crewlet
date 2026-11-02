package host_test

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CHAIN OF MERGES, BUILT IN ONE SCRATCH (chain.go): each entry merged onto
// the commit the one before it made by a merge_change's own build, a
// conflicting entry skipped and leaving nothing behind, one pack stored — and
// nothing of it in the cache until its caller's record applies.

// chainCase is a write case whose ceo has pushed branches off main's seed for
// a chain to merge.
type chainCase struct {
	*writeCase
	number int64
}

// newChainCase is a chain case, main seeded with README.md.
func newChainCase(t *testing.T) *chainCase {
	t.Helper()
	return &chainCase{writeCase: newWriteCase(t, codetest.SoloOptions{}, map[string]string{"README.md": "one\n"})}
}

// branch commits files onto main's seed as branch name, pushes it, and
// answers its head.
func (c *chainCase) branch(name string, files map[string]string) string {
	c.T.Helper()
	c.p.Git().Run(c.T, c.work, "checkout", "-q", "-B", name, c.head)
	head := c.p.Commit(c.work, files, "the work of "+name)
	c.MustPush(c.p, c.work, name)
	return head
}

// entry is the next change's queue entry of head by method, enqueued by pat at
// at.
func (c *chainCase) entry(name, head string, method code.MergeMethod, at time.Time) code.ChainEntry {
	c.number++
	merger := pat()
	return code.ChainEntry{EntryID: code.QueueEntryID(opAt(at)), Head: head, Method: method, Merger: merger,
		Change: code.ChainChange{ID: uuid.Must(uuid.NewV7()).String(), Number: c.number, Title: "Land " + name,
			SourceRef: "refs/heads/" + name, Owner: codetest.FixturePat, Authors: []string{codetest.FixturePat}}}
}

// build asks the git node to build entries onto main's seed, as one chain read
// at the log's position now, and fails the case on an error or a refusal.
func (c *chainCase) build(entries ...code.ChainEntry) (code.ChainResult, string) {
	c.T.Helper()
	b := code.ChainBuild{RepoID: c.repo, ChainID: opNow(), TargetRef: "refs/heads/main", Base: c.head,
		Seen: c.Rows().Position, Entries: entries}
	res, err := c.server().BuildChain(c.Ctx, b)
	if err != nil || res.Refusal != "" || res.Settle == nil {
		c.T.Fatalf("build the chain: %+v %v", res, err)
	}
	c.T.Cleanup(func() { _ = res.Settle(c.Ctx, false) })
	return res, b.ChainID
}

// scratch is the directory of chain id's scratch on the case's git node.
func (c *chainCase) scratch(id string) string {
	return filepath.Join(c.Fleet.Nodes()[0].CacheDir(), "scratch", id+".git")
}

// inScratch is `git log -1 --format=<format> <rev>` in chain id's scratch,
// which alone holds the chain's commits until its caller's record applies.
func (c *chainCase) inScratch(id, format, rev string) string {
	c.T.Helper()
	return strings.TrimSpace(c.p.Git().Run(c.T, c.scratch(id), "log", "-1", "--format="+format, rev))
}

// A CHAIN SKIPS ITS CONFLICTING ENTRY: of three entries — one rewriting the
// README, one rewriting it otherwise, one adding a file — the second
// conflicts with what the first made and is skipped, naming the path; the
// third is merged onto the FIRST's commit, so the chain lands on without it.
// And the skipped merge leaves nothing behind: the chain's pack is the very
// pack the same chain without that entry makes, byte for byte.
//
// Mutation: build the next entry onto the skipped one's tip (or stop the
// chain at it) → the third's parent is wrong, red; drop the prune of what the
// skipped merge wrote → its tree and marked README are packed, the two packs
// differ, red.
func TestAChainSkipsItsConflictingEntry(t *testing.T) {
	t.Parallel()
	c := newChainCase(t)
	ours := c.branch("ours", map[string]string{"README.md": "ours\n"})
	theirs := c.branch("theirs", map[string]string{"README.md": "theirs\n"})
	more := c.branch("more", map[string]string{"more.md": "more\n"})
	at := time.Now().Add(-time.Hour)
	first := c.entry("ours", ours, code.MergeMethodMerge, at)
	conflicting := c.entry("theirs", theirs, code.MergeMethodMerge, at.Add(time.Minute))
	third := c.entry("more", more, code.MergeMethodMerge, at.Add(2*time.Minute))
	res, id := c.build(first, conflicting, third)
	if len(res.Commits) != 3 {
		t.Fatalf("the chain answered %d entries, want 3: %+v", len(res.Commits), res.Commits)
	}
	one, two, three := res.Commits[0], res.Commits[1], res.Commits[2]
	if one.Skipped != "" || one.Commit == "" || three.Skipped != "" || three.Commit == "" {
		t.Fatalf("the first and third entries were not merged: %+v %+v", one, three)
	}
	if two.Skipped != code.WriteConflict || two.Commit != "" || len(two.Conflicts) != 1 ||
		two.Conflicts[0].Path != "README.md" {
		t.Fatalf("the conflicting entry is %+v, want skipped as a conflict on README.md", two)
	}
	if res.Tip != three.Commit {
		t.Errorf("the chain's tip is %s, want the third entry's commit %s", res.Tip, three.Commit)
	}
	if parents := c.inScratch(id, "%P", three.Commit); parents != one.Commit+" "+more {
		t.Errorf("the third entry's commit has parents %q, want the first's commit and its head: %s %s",
			parents, one.Commit, more)
	}
	if res.Pack == nil {
		t.Fatal("the chain stored no pack, where two merge commits are new")
	}
	without, _ := c.build(first, third)
	if without.Tip != res.Tip || without.Pack == nil || without.Pack.GitChecksum != res.Pack.GitChecksum {
		t.Errorf("the chain without its conflicting entry is tip %s pack %+v, want the chain's own tip %s and "+
			"pack %s: the skipped merge left objects behind", without.Tip, without.Pack, res.Tip,
			res.Pack.GitChecksum)
	}
}

// A REBUILT CHAIN IS BYTE-IDENTICAL: a chain of a merge and a squash built
// twice, a while apart, under two ids, is the same commits and the same pack
// — each commit's author date its ENTRY's instant, the enqueue's own, never
// the build's; the merge commit authored by its enqueuer and the squash by its
// change's owner (U43 a), committed by the engine.
//
// Mutation: date an entry's commit by the build's clock → the author dates are
// the build's, and the two builds' commits differ, red; author the squash by
// its merger, pat, rather than its owner, alice → red.
func TestARebuiltChainIsByteIdentical(t *testing.T) {
	t.Parallel()
	c := newChainCase(t)
	merged := c.branch("merged", map[string]string{"merged.md": "merged\n"})
	squashed := c.branch("squashed", map[string]string{"squashed.md": "squashed\n"})
	at := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	first := c.entry("merged", merged, code.MergeMethodMerge, at)
	second := c.entry("squashed", squashed, code.MergeMethodSquash, at.Add(time.Hour))
	second.Change.Owner, second.Change.Authors = codetest.FixtureAlice, []string{codetest.FixtureAlice,
		codetest.FixturePat}
	res, id := c.build(first, second)
	time.Sleep(1100 * time.Millisecond)
	again, _ := c.build(first, second)
	for i := range res.Commits {
		if res.Commits[i].Commit == "" || again.Commits[i].Commit != res.Commits[i].Commit {
			t.Errorf("entry %d built as %s and again as %s, want one commit", i, res.Commits[i].Commit,
				again.Commits[i].Commit)
		}
	}
	if res.Pack == nil || again.Pack == nil || again.Pack.GitChecksum != res.Pack.GitChecksum ||
		again.Pack.Size != res.Pack.Size {
		t.Errorf("the chain packed %+v and again %+v, want one pack", res.Pack, again.Pack)
	}
	for i, want := range []struct {
		author string
		when   time.Time
	}{{codetest.FixturePat, at}, {codetest.FixtureAlice, at.Add(time.Hour)}} {
		got := c.inScratch(id, "%an|%at|%cn <%ce>", res.Commits[i].Commit)
		if w := strings.Join([]string{want.author, strconv.FormatInt(want.when.Unix(), 10),
			codetest.FixtureCommitterName + " <" + codetest.FixtureCommitterEmail + ">"}, "|"); got != w {
			t.Errorf("entry %d's commit is %q, want %q: its entry's author at its entry's instant, committed "+
				"by the engine", i, got, w)
		}
	}
}

// A CHAIN WRITES NO CACHE: what a chain builds is in its scratch alone — its
// tip in no incarnation of the repository the cache holds — and its pack is
// stored as one bundle; settled unapplied, its scratch is gone and the cache
// still holds none of it. Only a record that applied takes its pack in: the
// same chain built again and settled applied puts its tip in the cache.
//
// Mutation: adopt the pack whatever the record's outcome → the tip is in the
// cache after an unapplied settle, red; keep the scratch after it → red;
// adopt nothing however the record ended → the applied chain's tip is in no
// copy, red.
func TestAChainWritesNoCache(t *testing.T) {
	t.Parallel()
	c := newChainCase(t)
	head := c.branch("work", map[string]string{"work.md": "work\n"})
	stored := len(c.objects())
	work := c.entry("work", head, code.MergeMethodMerge, time.Now())
	res, id := c.build(work)
	cached := func() []string {
		t.Helper()
		var holding []string
		dirs, err := filepath.Glob(filepath.Join(c.Fleet.Nodes()[0].CacheDir(), "repos", c.repo+".*.git"))
		if err != nil || len(dirs) == 0 {
			t.Fatalf("the cache holds no copy of the repository: %v %v", dirs, err)
		}
		for _, dir := range dirs {
			if _, _, err := c.p.Git().Try(t, dir, nil, "cat-file", "-e", res.Tip); err == nil {
				holding = append(holding, dir)
			}
		}
		return holding
	}
	if got := cached(); len(got) > 0 {
		t.Errorf("the chain's tip %s is in the cache's %v before any record named it", res.Tip, got)
	}
	if got := c.inScratch(id, "%H", res.Tip); got != res.Tip {
		t.Errorf("the chain's scratch reads its tip as %q", got)
	}
	if got := len(c.objects()); res.Pack == nil || got != stored+1 {
		t.Errorf("the object store holds %d objects after the chain, %d before, its pack %+v: want its one "+
			"bundle stored", got, stored, res.Pack)
	}
	if err := res.Settle(c.Ctx, false); err != nil {
		t.Fatalf("settle the chain unapplied: %v", err)
	}
	if _, err := os.Stat(c.scratch(id)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the chain's scratch is still there after it settled: %v", err)
	}
	if got := cached(); len(got) > 0 {
		t.Errorf("the chain's tip %s is in the cache's %v after it settled unapplied", res.Tip, got)
	}
	again, againID := c.build(work)
	if again.Tip != res.Tip {
		t.Fatalf("the same chain built again has tip %s, want %s", again.Tip, res.Tip)
	}
	if err := again.Settle(c.Ctx, true); err != nil {
		t.Fatalf("settle the chain applied: %v", err)
	}
	if got := cached(); len(got) == 0 {
		t.Errorf("the chain's tip %s is in no copy of the cache after it settled applied", again.Tip)
	}
	if _, err := os.Stat(c.scratch(againID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the chain's scratch is still there after it settled applied: %v", err)
	}
}

// A CHAIN IS HELD TO ITS SHAPE before anything is built: an id that cannot
// name its scratch, no entries or more than one record lands, an entry twice,
// an entry with no instant, and a squash with no owner are refused invalid,
// naming the field.
//
// Mutation: drop the duplicate check → the doubled entry builds, red.
func TestAChainIsHeldToItsShape(t *testing.T) {
	t.Parallel()
	c := newChainCase(t)
	head := c.branch("shape", map[string]string{"shape.md": "shape\n"})
	good := c.entry("shape", head, code.MergeMethodMerge, time.Now())
	ownerless := good
	ownerless.EntryID, ownerless.Method = code.QueueEntryID(opNow()), code.MergeMethodSquash
	ownerless.Change.Owner = ""
	timeless := good
	timeless.EntryID = uuid.NewString()
	many := make([]code.ChainEntry, code.MaxChainEntries+1)
	for i := range many {
		many[i] = good
		many[i].EntryID = code.QueueEntryID(opNow())
	}
	for _, tc := range []struct {
		name, field string
		mutate      func(*code.ChainBuild)
	}{
		{"an id that is no UUIDv7", "chain_id", func(b *code.ChainBuild) { b.ChainID = uuid.NewString() }},
		{"no entries", "entries", func(b *code.ChainBuild) { b.Entries = nil }},
		{"more than one record lands", "entries", func(b *code.ChainBuild) { b.Entries = many }},
		{"an entry twice", "entries[1].entry_id", func(b *code.ChainBuild) {
			b.Entries = []code.ChainEntry{good, good}
		}},
		{"an entry with no instant", "entries[0].entry_id", func(b *code.ChainBuild) {
			b.Entries = []code.ChainEntry{timeless}
		}},
		{"a squash with no owner", "entries[0].change.owner", func(b *code.ChainBuild) {
			b.Entries = []code.ChainEntry{ownerless}
		}},
		{"no position read", "seen", func(b *code.ChainBuild) { b.Seen = statelog.Position{} }},
	} {
		b := code.ChainBuild{RepoID: c.repo, ChainID: opNow(), TargetRef: "refs/heads/main", Base: c.head,
			Seen: c.Rows().Position, Entries: []code.ChainEntry{good}}
		tc.mutate(&b)
		_, err := c.server().BuildChain(c.Ctx, b)
		if !errors.Is(err, code.ErrInvalid) || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: BuildChain answered %v, want invalid naming %s", tc.name, err, tc.field)
		}
	}
}
