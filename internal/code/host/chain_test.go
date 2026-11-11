package host_test

import (
	"encoding/base64"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/host"
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

// newChainCaseOn is newChainCase with its Gitlet node's host reading now for
// its clock (host.SetClock) — what a chain's merges spend their share by,
// beside the git charged for them — handed to the host before the case's
// first request, since every request's goroutine reads it.
func newChainCaseOn(t *testing.T, now func() time.Time) *chainCase {
	t.Helper()
	c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
	host.SetClock(c.Fleet.Nodes()[0].Host(), now)
	return &chainCase{writeCase: seedWriteCase(c, map[string]string{"README.md": "one\n"})}
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

// build asks the Gitlet node to build entries onto main's seed, as one chain read
// at the log's position now, and fails the case on an error.
func (c *chainCase) build(entries ...code.ChainEntry) (code.ChainResult, string) {
	c.T.Helper()
	b := code.ChainBuild{RepoID: c.repo, ChainID: opNow(), TargetRef: "refs/heads/main", Base: c.head,
		Seen: c.Rows().Position, Entries: entries}
	res, err := c.server().BuildChain(c.Ctx, b)
	if err != nil || res.Settle == nil {
		c.T.Fatalf("build the chain: %+v %v", res, err)
	}
	c.T.Cleanup(func() { _ = res.Settle(c.Ctx, false) })
	return res, b.ChainID
}

// scratch is the directory of chain id's scratch on the case's Gitlet node.
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
// twice, under two ids, is the same commits and the same pack — each commit
// authored AND committed at its ENTRY's instant (U18 c: the later of it and
// its parents', whose commits gittest dates at the start of 2026), never at
// the build's; the merge commit authored by its enqueuer and the squash by its
// change's owner (U43 a), committed by the engine.
//
// The entries' instants are the case's own, two days before it runs, so a
// date the build's clock gave a commit is two days off whenever it is built:
// read off the commits themselves, never off two builds a clock step apart.
//
// Mutation: date an entry's commit, author or committer, by the build's clock
// → its dates are the build's, red; author the squash by its merger, pat,
// rather than its owner, alice → red.
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
		got := c.inScratch(id, "%an|%at|%cn <%ce>|%ct", res.Commits[i].Commit)
		when := strconv.FormatInt(want.when.Unix(), 10)
		if w := strings.Join([]string{want.author, when,
			codetest.FixtureCommitterName + " <" + codetest.FixtureCommitterEmail + ">", when}, "|"); got != w {
			t.Errorf("entry %d's commit is %q, want %q: its entry's author at its entry's instant, committed "+
				"by the engine at it", i, got, w)
		}
	}
}

// bigLines is lines of random base64, seeded, and line changed (none where it
// is -1) reading "changed": about 77 bytes a line, which deflate keeps some
// three quarters of.
func bigLines(lines, changed int) string {
	rnd := rand.New(rand.NewPCG(1, 2))
	raw := make([]byte, 57)
	var b strings.Builder
	for i := range lines {
		for j := range raw {
			raw[j] = byte(rnd.Uint32())
		}
		if i == changed {
			b.WriteString("changed\n")
			continue
		}
		b.WriteString(base64.StdEncoding.EncodeToString(raw))
		b.WriteByte('\n')
	}
	return b.String()
}

// AN ENTRY IS HELD TO A MERGE'S PACK BOUND: main's seed holds a file of 6 MiB
// of random text; one entry changes its first line and another its last, so
// the second's merge onto the first's commit writes a new 6 MiB blob, which
// packs past code.MaxWritePackBytes as a merge_change of it would. That entry
// alone is skipped too_large, in a merge_change's own words; the entry after
// it is built onto the first's commit, and the chain's pack holds none of the
// skipped merge's blob.
//
// Mutation: drop the entry's bound (accept every entry under any loose size)
// → the second entry is built, red; prune nothing of it → the pack carries
// its blob, red.
func TestAChainEntryIsHeldToAMergesPackBound(t *testing.T) {
	t.Parallel()
	const lines = 6 << 20 / 77
	c := &chainCase{writeCase: newWriteCase(t, codetest.SoloOptions{},
		map[string]string{"README.md": "one\n", "big.txt": bigLines(lines, -1)})}
	top := c.branch("top", map[string]string{"big.txt": bigLines(lines, 0)})
	bottom := c.branch("bottom", map[string]string{"big.txt": bigLines(lines, lines-1)})
	small := c.branch("small", map[string]string{"small.md": "small\n"})
	at := time.Now().Add(-time.Hour)
	res, _ := c.build(c.entry("top", top, code.MergeMethodMerge, at),
		c.entry("bottom", bottom, code.MergeMethodMerge, at.Add(time.Minute)),
		c.entry("small", small, code.MergeMethodMerge, at.Add(2*time.Minute)))
	if len(res.Commits) != 3 {
		t.Fatalf("the chain answered %d entries, want 3: %+v", len(res.Commits), res.Commits)
	}
	one, two, three := res.Commits[0], res.Commits[1], res.Commits[2]
	if one.Commit == "" || three.Commit == "" || res.Tip != three.Commit {
		t.Fatalf("the first and third entries were not built onto each other: %+v %+v, tip %s", one, three,
			res.Tip)
	}
	if two.Skipped != code.WriteTooLarge || two.Commit != "" ||
		!strings.Contains(two.Detail, "code.MaxWritePackBytes") {
		t.Fatalf("the entry whose merge writes 6 MiB is %+v, want skipped too_large in a merge_change's words",
			two)
	}
	if res.Pack == nil || res.Pack.Size > 1<<20 {
		t.Errorf("the chain's pack is %+v, want its two merge commits and their trees, none of the skipped "+
			"merge's blob", res.Pack)
	}
}

// A CHAIN HOLDS ITS OBJECTS' BYTES, AN ENTRY PAST THEM ALONE IS SKIPPED:
// main's seed holds a file of 256 KiB of random text; three entries change
// its first, its last and its middle line, so the second's merge and the
// third's each write a new blob of the file's size, and the first's none.
// With the chain's objects held to one and a half files, the third would
// take the chain past them: it is NOT REACHED, and the chain answers the two
// before it with the very pack those two alone make. Held to half a file,
// the second and the third each write more than the chain may hold alone:
// each is skipped too_large, naming the bound, and the first is built.
//
// Mutation: drop the chain's running total → the third is built in the
// first case, red; drop the bound an entry is held to alone → the second is
// left unreached rather than skipped in the second case, red.
func TestAChainHoldsItsObjectsBytes(t *testing.T) {
	t.Parallel()
	const lines = 256 << 10 / 77
	const file = lines * 77
	for _, tc := range []struct {
		name  string
		bound int64
	}{
		{"the third would take it past them", file * 3 / 2},
		{"the second and third are past them alone", file / 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &chainCase{writeCase: newWriteCase(t, codetest.SoloOptions{},
				map[string]string{"README.md": "one\n", "big.txt": bigLines(lines, -1)})}
			at := time.Now().Add(-time.Hour)
			entries := []code.ChainEntry{
				c.entry("first", c.branch("first", map[string]string{"big.txt": bigLines(lines, 0)}),
					code.MergeMethodMerge, at),
				c.entry("last", c.branch("last", map[string]string{"big.txt": bigLines(lines, lines-1)}),
					code.MergeMethodMerge, at.Add(time.Minute)),
				c.entry("middle", c.branch("middle", map[string]string{"big.txt": bigLines(lines, lines/2)}),
					code.MergeMethodMerge, at.Add(2*time.Minute)),
			}
			host.SetChainObjects(c.server(), tc.bound)
			res, _ := c.build(entries...)
			if tc.bound > file {
				two, _ := c.build(entries[:2]...)
				if len(res.Commits) != 2 || res.Commits[0].Commit == "" || res.Commits[1].Commit == "" ||
					res.Tip != two.Tip || res.Pack == nil || two.Pack == nil ||
					res.Pack.GitChecksum != two.Pack.GitChecksum {
					t.Fatalf("the chain answered %+v tip %s pack %+v, want the two entries before the one past "+
						"its bytes built, as the two alone are: tip %s pack %+v", res.Commits, res.Tip, res.Pack,
						two.Tip, two.Pack)
				}
				return
			}
			if len(res.Commits) != 3 || res.Commits[0].Commit == "" || res.Tip != res.Commits[0].Commit {
				t.Fatalf("the chain answered %+v tip %s, want its first entry built and the others answered",
					res.Commits, res.Tip)
			}
			for _, skipped := range res.Commits[1:] {
				if skipped.Skipped != code.WriteTooLarge || skipped.Commit != "" ||
					!strings.Contains(skipped.Detail, "code.MaxChainObjectBytes") {
					t.Errorf("an entry whose merge writes a whole file is %+v, want skipped too_large naming "+
						"code.MaxChainObjectBytes", skipped)
				}
			}
		})
	}
}

// A CHAIN OUT OF TIME ANSWERS ITS BUILT PREFIX: the chain's merges share one
// merge's budget after its first entry, which has a merge's whole budget of
// its own. Its git charged 20 s a merge-tree, the first entry is built, the
// second is cut by what the first left — short of a merge's whole budget, so
// NOT REACHED rather than too_large — and the third is never started: the
// chain answers the first alone, with the very pack the first alone makes, so
// the cut merge left nothing. And the share is spent by the time since the
// merges began as much as by the git charged for them: a chain whose first
// merge-tree waited three seconds, charged nothing for it, stops there too.
//
// THE HOST'S CLOCK IS THE CASE'S (manualClock), and moves only when the case
// moves it: the chain reads the time since its merges began off that clock,
// so the waited merge-tree's three seconds are a step the case takes while it
// is held, and no wall clock has to pass for them.
//
// Mutation: answer an error where the chain's time runs out → no prefix, red;
// skip the cut entry too_large → a second entry answered, red; charge the
// share only its git's run time → the waited chain builds all three, red;
// prune nothing of the cut entry → its tree is packed, the packs differ, red.
func TestAChainOutOfTimeAnswersItsBuiltPrefix(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		slow func(t *testing.T, s *host.Server, clock *manualClock)
	}{
		{"its git ran", func(_ *testing.T, s *host.Server, _ *manualClock) {
			host.SlowGit(s, 20*time.Second, "merge-tree")
		}},
		{"its git waited", func(t *testing.T, s *host.Server, clock *manualClock) {
			host.SetChainMerges(s, 2*time.Second)
			reached, release := host.HoldGit(s, "merge-tree")
			t.Cleanup(release)
			go func() {
				<-reached
				clock.advance(3 * time.Second)
				release()
			}()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := &manualClock{t: time.Now()}
			c := newChainCaseOn(t, clock.now)
			heads := []string{c.branch("a", map[string]string{"a.md": "a\n"}),
				c.branch("b", map[string]string{"b.md": "b\n"}), c.branch("c", map[string]string{"c.md": "c\n"})}
			at := time.Now().Add(-time.Hour)
			var entries []code.ChainEntry
			for i, name := range []string{"a", "b", "c"} {
				entries = append(entries, c.entry(name, heads[i], code.MergeMethodMerge,
					at.Add(time.Duration(i)*time.Minute)))
			}
			alone, _ := c.build(entries[0])
			tc.slow(t, c.server(), clock)
			res, _ := c.build(entries...)
			if len(res.Commits) != 1 || res.Commits[0].Commit == "" || res.Commits[0].EntryID != entries[0].EntryID {
				t.Fatalf("the chain answered %+v, want its first entry built and the rest not reached",
					res.Commits)
			}
			if res.Tip != alone.Tip || res.Pack == nil || alone.Pack == nil ||
				res.Pack.GitChecksum != alone.Pack.GitChecksum {
				t.Errorf("the chain's prefix is tip %s pack %+v, want the first entry's alone: tip %s pack %+v",
					res.Tip, res.Pack, alone.Tip, alone.Pack)
			}
		})
	}
}

// AN OBJECT GIT'S CHECK REFUSES SKIPS ITS ENTRY ALONE: a chain of three whose
// pack `index-pack --strict` refuses for an object the second entry's merge
// wrote skips that entry rejected_object, in git's own words, leaves the
// third — merged onto the second's commit — not reached, and packs again: the
// chain answers the first entry built and the second skipped, with the very
// pack the first alone makes.
//
// Mutation: refuse the chain whole on the check's refusal → an error, red;
// keep the entries after the refused one → three answered, red; prune nothing
// → the second's and third's objects are packed, the packs differ, red.
func TestAChainSkipsTheEntryWhoseObjectGitsCheckRefuses(t *testing.T) {
	t.Parallel()
	c := newChainCase(t)
	heads := []string{c.branch("a", map[string]string{"a.md": "a\n"}),
		c.branch("b", map[string]string{"b.md": "b\n"}), c.branch("c", map[string]string{"c.md": "c\n"})}
	at := time.Now().Add(-time.Hour)
	var entries []code.ChainEntry
	for i, name := range []string{"a", "b", "c"} {
		entries = append(entries, c.entry(name, heads[i], code.MergeMethodMerge, at.Add(time.Duration(i)*time.Minute)))
	}
	whole, _ := c.build(entries...)
	alone, _ := c.build(entries[0])
	if len(whole.Commits) != 3 || whole.Commits[1].Commit == "" {
		t.Fatalf("the chain built %+v, want three commits", whole.Commits)
	}
	host.RefuseObjectOnce(c.server(), whole.Commits[1].Commit)
	res, _ := c.build(entries...)
	if len(res.Commits) != 2 || res.Commits[0].Commit != alone.Tip ||
		res.Commits[1].Skipped != code.WriteRejectedObject || res.Commits[1].Commit != "" ||
		!strings.Contains(res.Commits[1].Detail, whole.Commits[1].Commit) {
		t.Fatalf("the chain answered %+v, want the first entry built and the second skipped rejected_object "+
			"naming %s, the third not reached", res.Commits, whole.Commits[1].Commit)
	}
	if res.Tip != alone.Tip || res.Pack == nil || alone.Pack == nil || res.Pack.GitChecksum != alone.Pack.GitChecksum {
		t.Errorf("the chain is tip %s pack %+v, want the first entry's alone: tip %s pack %+v", res.Tip, res.Pack,
			alone.Tip, alone.Pack)
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

// NO CHAIN IS BUILT FOR A PULL MIRROR, nor for a repository being imported: a
// queue's chain on a repository made a pull mirror is refused by its row
// before anything is built — repo_importing while the mirror's import runs and
// repo_mirrored once it is done, each in the domain's one sentence for the
// state (code.ChainResult.Status, Detail) — with no commit, no scratch left
// and no pack stored, where the candidate naming it would be refused from the
// same row after a scratch, a share unit and a stored pack.
//
// Mutation: drop BuildChain's status check — the chain is built and its pack
// stored, red; drop either arm of the status mapping — that chain likewise,
// red.
func TestNoChainIsBuiltForAPullMirror(t *testing.T) {
	t.Parallel()
	c := newChainCase(t)
	head := c.branch("work", map[string]string{"work.md": "work\n"})
	work := c.entry("work", head, code.MergeMethodMerge, time.Now())
	mirrored := codetest.Settings()
	mirrored.MirrorFrom = &code.MirrorSource{URL: "https://forge.example.com/acme/web.git"}
	c.Fleet.Declare(t, code.Declared{Name: codetest.RepoName, Settings: mirrored})
	stored := len(c.objects())
	refused := func(state string, want code.MergeRefusal, words string) {
		t.Helper()
		b := code.ChainBuild{RepoID: c.repo, ChainID: opNow(), TargetRef: "refs/heads/main", Base: c.head,
			Seen: c.Rows().Position, Entries: []code.ChainEntry{work}}
		res, err := c.server().BuildChain(c.Ctx, b)
		if err != nil || res.Status != want || res.Detail != words || res.Pack != nil ||
			len(res.Commits) != 0 || res.Settle == nil {
			t.Fatalf("a chain on a repository %s answered %+v %v, want refused %s saying %q, nothing built",
				state, res, err, want, words)
		}
		if _, err := os.Stat(c.scratch(b.ChainID)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a chain on a repository %s left a scratch: %v", state, err)
		}
		if got := len(c.objects()); got != stored {
			t.Errorf("a chain on a repository %s stored %d objects: its row refuses it before anything is "+
				"built", state, got-stored)
		}
	}
	refused("being imported", code.MergeRefusalRepoImporting, code.ImportingDetail(codetest.RepoName))
	done := code.ImportDone{RepoID: c.repo, Node: c.Fleet.Nodes()[0].ID()}
	if res, err := c.Fleet.(*codetest.SoloFleet).Code(t).Import(c.Ctx, code.ImportProposal{Done: &done}); err != nil ||
		res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("end the pull mirror's import: %+v, %v", res, err)
	}
	refused("that is a pull mirror", code.MergeRefusalRepoMirrored,
		code.MirroredDetail(codetest.RepoName, code.ManagedConfig))
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
