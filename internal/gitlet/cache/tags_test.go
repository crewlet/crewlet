package cache_test

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// mktag makes a tag object of body in repo's work tree (git mktag, with
// --no-strict where strict is false), stores it as its own pack and writes
// the tag name to it: what an import of a tag no `git tag` writes pushes.
func (w *syncWorld) mktag(t testing.TB, repo, name, body string, strict bool) string {
	t.Helper()
	f := w.repos[repo]
	args := []string{"mktag"}
	if !strict {
		args = append(args, "--no-strict")
	}
	out, stderr, err := w.client.Try(t, f.work, strings.NewReader(body), args...)
	if err != nil {
		t.Fatalf("git mktag of %s: %v\n%s", name, err, stderr)
	}
	oid := strings.TrimSpace(out)
	row := w.pack(t, f, oid, "")
	w.log.write(repo, []gitlet.PackRow{row}, map[string]string{"refs/tags/" + name: oid})
	return oid
}

// tagBody is a tag object's text: of obj, a typ, by a tagger dated secs,
// none where secs is "".
func tagBody(obj, typ, name, secs string) string {
	body := fmt.Sprintf("object %s\ntype %s\ntag %s\n", obj, typ, name)
	if secs != "" {
		body += "tagger T <t@example.com> " + secs + " +0000\n"
	}
	return body + "\n" + name + "\n"
}

// tagsOracle is git's own listing of the copy at dir, in the index's terms:
// a line a tag in git's creatordate order, each its ref, object, type, what
// it peels to and that object's type, and its date or "-".
func tagsOracle(t testing.TB, w *syncWorld, dir string) []string {
	t.Helper()
	out, err := runGit(t, w.pool, dir, "", "for-each-ref", "--sort=-creatordate",
		"--format=%(refname)%00%(objectname)%00%(objecttype)%00%(creatordate:unix)", "refs/tags/")
	if err != nil {
		t.Fatalf("for-each-ref: %v", err)
	}
	var rows []string
	for line := range strings.Lines(out) {
		f := strings.Split(strings.TrimSuffix(line, "\n"), "\x00")
		peeled, err := runGit(t, w.pool, dir, "", "rev-parse", f[0]+"^{}")
		if err != nil {
			t.Fatalf("rev-parse %s^{}: %v", f[0], err)
		}
		peeled = strings.TrimSpace(peeled)
		typ, err := runGit(t, w.pool, dir, "", "cat-file", "-t", peeled)
		if err != nil {
			t.Fatalf("cat-file -t %s: %v", peeled, err)
		}
		date := f[3]
		if date == "" {
			date = "-"
		}
		rows = append(rows, strings.Join([]string{f[0], f[1], f[2], peeled, strings.TrimSpace(typ), date}, " "))
	}
	return rows
}

// indexRows is every row of the copy s's tag index, as TagRows spells them,
// read through Cache.Tags.
func indexRows(t testing.TB, c *cache.Cache, s cache.Synced) ([]string, map[string][]string) {
	t.Helper()
	x, ok := c.Tags(s)
	if !ok {
		t.Fatalf("the copy at %s keeps no tag index a read can trust", s.Dir)
	}
	defer func() { _ = x.Close() }()
	var rows []string
	byCommit := map[string][]string{}
	err := x.Rows(nil, func(e cache.TagEntry) bool {
		date := "-"
		if e.Dated {
			date = fmt.Sprint(e.Date)
		}
		rows = append(rows, strings.Join([]string{e.Ref, e.OID, string(e.Type), e.Peeled, string(e.PeeledType), date},
			" "))
		if e.PeeledType == gitlet.ObjectCommit {
			byCommit[e.Peeled] = append(byCommit[e.Peeled], e.Ref)
		}
		return true
	})
	if err != nil {
		t.Fatalf("the tag index's rows: %v", err)
	}
	for commit, refs := range byCommit {
		got, more, err := x.Commit(commit, gitlet.MaxCommitTags)
		if take := min(len(refs), gitlet.MaxCommitTags); err != nil || !slices.Equal(got, refs[:take]) ||
			more != len(refs)-take {
			t.Errorf("the tags of %s are %v and %d more (%v), want %v", commit, got, more, err, refs)
		}
	}
	return rows, byCommit
}

// THE TAG INDEX FOLLOWS THE TAGS: a fresh cache's bytes and git's listing.
//
// Mutation: drop a deleted tag's record only on a full round → red.
func TestTheTagIndexFollowsTheTags(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	f := w.repos[repo]
	a, _ := w.push(t, repo, "refs/heads/main", nil)
	tree := strings.TrimSpace(w.client.Run(t, f.work, "rev-parse", a+"^{tree}"))
	at := strings.TrimSpace(w.client.Run(t, f.work, "log", "-1", "--format=%ct", a))
	annotated := w.mktag(t, repo, "annotated", tagBody(a, "commit", "annotated", at), true)
	w.log.write(repo, nil, map[string]string{"refs/tags/light": a, "refs/tags/tree-light": tree})
	w.mktag(t, repo, "tree-annotated", tagBody(tree, "tree", "tree-annotated", "1000"), true)
	w.mktag(t, repo, "tag-of-tag", tagBody(annotated, "tag", "tag-of-tag", "2000"), true)
	w.mktag(t, repo, "no-tagger", tagBody(a, "commit", "no-tagger", ""), false)
	w.mktag(t, repo, "far", tagBody(a, "commit", "far", "253402300800"), true)
	c := w.open(t)

	check := func(step string) {
		t.Helper()
		s := w.sync(t, c, repo)
		if got, want := w.image(t, c, repo), w.rebuilt(t, repo); got != want {
			t.Errorf("%s: the copy is not a fresh cache's:\n%s\nwant:\n%s", step, got, want)
		}
		rows, _ := indexRows(t, c, s)
		if want := tagsOracle(t, w, s.Dir); !slices.Equal(rows, want) {
			t.Errorf("%s: the index lists\n%s\nand git\n%s", step, strings.Join(rows, "\n"), strings.Join(want, "\n"))
		}
	}
	check("create")
	b, _ := w.push(t, repo, "refs/heads/main", nil)
	w.log.write(repo, nil, map[string]string{"refs/tags/light": b})
	check("a tag moved to another commit")
	w.log.write(repo, nil, map[string]string{"refs/tags/annotated": b})
	check("an annotated tag made lightweight")
	w.log.write(repo, nil, map[string]string{"refs/tags/tree-light": ""})
	check("delete")
	w.log.write(repo, nil, map[string]string{"refs/tags/tree-light": a})
	check("a deleted name made again at another object")
}

// readings records each reading a tag step is about to make, failing the
// steps fail says to.
type readings struct {
	mu   sync.Mutex
	got  []int
	fail func(n int) error
}

func (r *readings) knob(o *cache.Options) {
	cache.BeforeTags(o, func(ctx context.Context, _ string, reading int) error {
		r.mu.Lock()
		r.got = append(r.got, reading)
		n, fail := len(r.got), r.fail
		r.mu.Unlock()
		if fail != nil {
			return fail(n)
		}
		return nil
	})
}

func (r *readings) since(n int) []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.got[n:])
}

func (r *readings) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

// tagsAt is the identity of the tag index of the copy at dir.
func tagsAt(t testing.TB, dir string) os.FileInfo {
	t.Helper()
	info, err := os.Lstat(filepath.Join(dir, cache.TagsRel))
	if err != nil {
		t.Fatalf("the tag index of %s: %v", dir, err)
	}
	return info
}

// A ROUND THAT MOVES NO TAG READS NO TAG, and one moving one reads it alone.
//
// Mutations: run the step on a push moving no tag, or write an index a full
// round finds level → red.
func TestARoundThatMovesNoTagReadsNoTag(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	w.tag(t, repo, "v1", true)
	w.tag(t, repo, "l1", false)
	var r readings
	c := w.open(t, r.knob)
	s := w.sync(t, c, repo)

	n := r.count()
	w.push(t, repo, "refs/heads/main", nil)
	w.sync(t, c, repo)
	if got := r.since(n); len(got) != 0 {
		t.Errorf("a push moving a branch ran the tag step reading %v, want none", got)
	}
	n = r.count()
	w.tag(t, repo, "v2", true)
	w.sync(t, c, repo)
	if got := r.since(n); !slices.Equal(got, []int{1}) {
		t.Errorf("a push moving an annotated tag read %v, want its date alone", got)
	}

	before := tagsAt(t, s.Dir)
	n = r.count()
	cache.MarkWhole(c, repo, cache.RebuildShape, 0)
	s = w.sync(t, c, repo)
	if got := r.since(n); !slices.Equal(got, []int{0}) {
		t.Errorf("a full round over a level index read %v, want nothing", got)
	}
	if !os.SameFile(before, tagsAt(t, s.Dir)) || !before.ModTime().Equal(tagsAt(t, s.Dir).ModTime()) {
		t.Error("a full round over a level index wrote it")
	}

	before = tagsAt(t, s.Dir)
	c = w.reopen(t, c, r.knob)
	n = r.count()
	s = w.sync(t, c, repo)
	if got := r.since(n); !slices.Equal(got, []int{0}) {
		t.Errorf("a new process's first round read %v, want nothing", got)
	}
	if !os.SameFile(before, tagsAt(t, s.Dir)) {
		t.Error("a new process's first round wrote the index it found level")
	}
	if rows, _ := indexRows(t, c, s); len(rows) != 3 {
		t.Errorf("the index a new process found level lists %d tags, want 3", len(rows))
	}
}

// A TAG MOVE MERGES THE INDEX, the round's own step allocating for what
// moved and never for the tags it keeps.
//
// Mutation: build whole on a delta → allocations grow with the tags, red.
func TestATagMoveMergesTheIndex(t *testing.T) { //nolint:paralleltest // AllocsPerRun refuses a parallel test
	w := newSyncWorld(t, onMemory)
	c := w.open(t)
	allocs := func(n int) float64 {
		dir := gittest.Empty(t)
		gittest.FastImport(t, w.client, dir, namedTagStream(n, max(1, n/1_000), false))
		var refs []gitlet.RefRow
		for line := range strings.Lines(w.client.Run(t, dir, "for-each-ref", "--format=%(refname) %(objectname)",
			"refs/tags/")) {
			ref, oid, _ := strings.Cut(strings.TrimSpace(line), " ")
			refs = append(refs, gitlet.RefRow{Ref: ref, OID: oid})
		}
		b := cache.NewTagBench(c, dir)
		if err := b.Step(t.Context(), refs, true); err != nil {
			t.Fatalf("the first build of %d tags: %v", n, err)
		}
		// The delta deletes the first tag, from packed-refs too, so a whole
		// build would read what the merge does.
		if err := b.SetPackedRefs(refs[1:]); err != nil {
			t.Fatal(err)
		}
		deleted, err := b.Peel(t.Context(), []gitlet.RefRow{{Ref: refs[0].Ref}}, false)
		if err != nil {
			t.Fatal(err)
		}
		var stepErr error
		step := func() {
			if serr := b.StepPeeled(t.Context(), deleted); serr != nil && stepErr == nil {
				stepErr = serr
			}
		}
		// The least of several counts: another goroutine's allocation only
		// ever adds to one.
		least := math.Inf(1)
		for range 5 {
			least = min(least, testing.AllocsPerRun(1, step))
		}
		if stepErr != nil {
			t.Fatalf("deleting a tag of %d: %v", n, stepErr)
		}
		x, err := b.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = x.Close() }()
		if x.Len() != n-1 {
			t.Fatalf("after deleting a tag of %d the index holds %d", n, x.Len())
		}
		return least
	}
	small, large := allocs(1_000), allocs(10_000)
	if large > small+8 {
		t.Errorf("a delta deleting a tag of 10 000 allocates %v times and of 1 000 %v: the step builds whole",
			large, small)
	}
}

// A TAG INDEX WHOSE NEXT FAILS TO RENAME IN IS SET ASIDE, though no ref moved.
//
// Mutation: set aside only where the round moves refs → red.
func TestATagIndexWhoseNextFailsToRenameInIsSetAside(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	w.tag(t, repo, "v1", true)
	c := w.open(t)
	s := w.sync(t, c, repo)
	cache.PlaceUnrenamed(c, repo)
	if x, ok := c.Tags(s); ok {
		_ = x.Close()
		t.Error("a read was handed the index a failed rename left in place")
	}
	if servable, failed := cache.TagsServable(c, repo); servable || failed.IsZero() {
		t.Errorf("after a failed rename the index is servable %t, its step failed at %v", servable, failed)
	}
}

// A TAG INDEX A ROUND FINDS DAMAGED IS NEVER SERVED AGAIN, while the round
// goes on, and the round builds it.
//
// Mutation: leave the vouch to the step's placement → red.
func TestATagIndexARoundFindsDamagedIsNeverServedAgain(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	w.tag(t, repo, "v1", true)
	var hold atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	c := w.open(t, func(o *cache.Options) {
		cache.BeforeTags(o, func(ctx context.Context, _ string, _ int) error {
			if !hold.CompareAndSwap(true, false) {
				return nil
			}
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	})
	s := w.sync(t, c, repo)
	flipTags(t, s.Dir, firstDateByte, 0x7f)
	if !cache.TagsVouched(c, repo) {
		t.Fatal("the flip moved the file's identity: the case needs the vouch to stand")
	}
	hold.Store(true)
	cache.MarkWhole(c, repo, cache.RebuildShape, 0)
	done := make(chan error, 1)
	go func() {
		got, err := c.Sync(t.Context(), repo, linearizable)
		if err == nil {
			got.Release()
		}
		done <- err
	}()
	<-entered
	if x, ok := c.Tags(s); ok {
		_ = x.Close()
		t.Error("a read was handed the index the round found damaged")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got, want := w.image(t, c, repo), w.rebuilt(t, repo); got != want {
		t.Errorf("the round kept the damaged index:\n%s\nwant:\n%s", got, want)
	}
}

// A TAG INDEX NO VOUCHED FILE STANDS BEHIND IS BUILT AGAIN, at a read's asking.
//
// Mutation: answer the read false and ask no round → red.
func TestATagIndexNoVouchedFileStandsBehindIsBuiltAgain(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	w.tag(t, repo, "v1", true)
	c := w.open(t)
	s := w.sync(t, c, repo)
	cache.UnvouchTags(c, repo)
	if x, ok := c.Tags(s); ok {
		_ = x.Close()
		t.Fatal("a read was handed an index no vouch stands behind")
	}
	eventually(t, "a round builds the index the read asked for", func() bool {
		return cache.TagsVouched(c, repo)
	})
	s = w.sync(t, c, repo)
	if rows, _ := indexRows(t, c, s); len(rows) != 1 {
		t.Errorf("the index built again lists %d tags, want 1", len(rows))
	}
}

// A TAG INDEX KEEPS THROUGH A COMPACTION AND AN EXPUNGE.
//
// Mutations: set OnPackRetired: Drop → Cache.Tags false after the swap;
// OnPackExpunged: Drop → no index after the expunge's round, red.
func TestATagIndexKeepsThroughACompactionAndAnExpunge(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	_, first := w.push(t, repo, "refs/heads/main", nil)
	gone, goneRow := w.tagged(t, repo, "gone")
	_, second := w.push(t, repo, "refs/heads/main", nil)
	_, keptRow := w.tagged(t, repo, "kept")
	c := w.open(t)
	held := w.sync(t, c, repo)

	merged := w.compacted(t, repo, first, goneRow, second, keptRow)
	s := w.sync(t, c, repo)
	if s.Dir == held.Dir {
		t.Fatalf("the compaction's round answered the copy a reader holds: no swap")
	}
	if !sameInode(t, filepath.Join(held.Dir, cache.TagsRel), filepath.Join(s.Dir, cache.TagsRel)) {
		t.Error("the swap wrote the tag index again rather than linking it")
	}
	if rows, _ := indexRows(t, c, s); len(rows) != 2 {
		t.Errorf("the index after the swap lists %d tags, want 2", len(rows))
	}

	w.log.write(repo, nil, map[string]string{"refs/tags/gone": ""})
	s = w.sync(t, c, repo)
	before := tagsAt(t, s.Dir)
	w.expungeBegin(t, repo, []string{gone}, merged)
	w.sync(t, c, repo)
	w.log.expungeCommit(repo)
	s = w.sync(t, c, repo)
	if after := tagsAt(t, s.Dir); !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Error("the expunge wrote the tag index again rather than keeping it")
	}
	if got, want := w.image(t, c, repo), w.rebuilt(t, repo); got != want {
		t.Errorf("after the expunge the copy is not a fresh cache's:\n%s\nwant:\n%s", got, want)
	}
	if rows, _ := indexRows(t, c, s); len(rows) != 1 {
		t.Errorf("the index after the expunge lists %d tags, want the one kept", len(rows))
	}
}

// A REBUILD READS THE TAGS AGAIN in its full round, to the same bytes.
//
// Mutation: leave the absent index to a later delta → red.
func TestARebuildReadsTheTagsAgain(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	w.tag(t, repo, "v1", true)
	w.push(t, repo, "refs/heads/main", nil)
	w.tag(t, repo, "v2", true)
	w.tag(t, repo, "l2", false)
	var r readings
	c := w.open(t, r.knob)
	old := w.sync(t, c, repo)
	was := tagsImage(old.Dir)
	n := r.count()
	rebuilt, err := cache.RebuildWhole(t.Context(), c, old, cache.Failure{Reason: cache.RebuildCorrupt})
	if err != nil {
		t.Fatalf("RebuildWhole: %v", err)
	}
	defer rebuilt.Release()
	if got := r.since(n); !slices.Equal(got, []int{3}) {
		t.Errorf("the rebuild's full round read %v, want every tag's facts", got)
	}
	if got := tagsImage(rebuilt.Dir); got != was {
		t.Errorf("the rebuilt index is\n%s\nwant\n%s", got, was)
	}
}

// A FAILED TAG STEP SETS THE INDEX ASIDE, never the round; a read brings it back.
//
// Mutations: serve the old index, trash the file, or ask no round → red.
func TestAFailedTagStepSetsTheIndexAsideAndAReadBringsItBack(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	w.tag(t, repo, "v1", true)
	w.tag(t, repo, "l1", false)
	r := readings{}
	c := w.open(t, clk.on, r.knob)
	s := w.sync(t, c, repo)
	before := tagsAt(t, s.Dir)

	failing := r.count() + 1
	r.mu.Lock()
	r.fail = func(n int) error {
		if n == failing {
			return fmt.Errorf("the case fails the step")
		}
		return nil
	}
	r.mu.Unlock()
	w.tag(t, repo, "v2", true)
	s = w.sync(t, c, repo)
	if !strings.Contains(read(t, filepath.Join(s.Dir, "packed-refs")), "refs/tags/v2") {
		t.Error("the round whose tag step failed did not move the refs")
	}
	if x, ok := c.Tags(s); ok {
		_ = x.Close()
		t.Error("a read was handed the index the failed step left behind its refs")
	}
	if !os.SameFile(before, tagsAt(t, s.Dir)) {
		t.Error("the failed step replaced the index rather than setting it aside")
	}
	if got := len(w.logs.lines(t, "code_cache_tags_failed")); got != 1 {
		t.Errorf("code_cache_tags_failed logged %d times, want once", got)
	}

	n := r.count()
	w.push(t, repo, "refs/heads/main", nil)
	s = w.sync(t, c, repo)
	if x, ok := c.Tags(s); ok {
		_ = x.Close()
		t.Error("a read within TagsRetry was handed an index")
	}
	if got := r.since(n); len(got) != 0 {
		t.Errorf("within TagsRetry a round ran the tag step reading %v, want none", got)
	}

	// Past it, with nothing pushed: the read asks the round.
	clk.Add(cache.TagsRetry)
	n = r.count()
	if x, ok := c.Tags(s); ok {
		_ = x.Close()
		t.Fatal("a read past TagsRetry was handed the index set aside")
	}
	eventually(t, "a round builds the index the read asked for", func() bool {
		servable, _ := cache.TagsServable(c, repo)
		return servable
	})
	if got := r.since(n); !slices.Equal(got, []int{1}) {
		t.Errorf("the round the read asked for read %v, want the moved tag alone", got)
	}
	s = w.sync(t, c, repo)
	if got, want := w.image(t, c, repo), w.rebuilt(t, repo); got != want {
		t.Errorf("the index built again is not a fresh cache's:\n%s\nwant:\n%s", got, want)
	}
	if rows, _ := indexRows(t, c, s); len(rows) != 3 {
		t.Errorf("the index built again lists %d tags, want 3", len(rows))
	}
}

// flipTags flips one byte of the tag index at dir in place, at off from its
// start, keeping its inode and its modification time: what the disk does to a
// file this process vouched for.
func flipTags(t testing.TB, dir string, off int64, to byte) {
	t.Helper()
	path := filepath.Join(dir, cache.TagsRel)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteAt([]byte{to}, off); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
}

// The first record's date's last byte, and its types byte.
const (
	firstDateByte  = 8 + 49
	firstTypesByte = 8 + 40
)

// A DAMAGED TAG INDEX IS BUILT AGAIN: by the scrub, a boot or a tag's merge.
//
// Mutations: vouch at boot unread, or skip a vouched file's trailer → red.
func TestADamagedTagIndexIsBuiltAgain(t *testing.T) {
	t.Parallel()
	t.Run("the scrub", func(t *testing.T) {
		t.Parallel()
		sc := newScrubbedCopy(t)
		flipTags(t, sc.dir, 0, 'X')
		sc.used(t)
		cache.ScrubPass(sc.c)
		if got := counterOf(sc.w.rec, metrics.CodeCacheChecks, map[string]string{"file": "tags",
			"outcome": "damaged"}); got != 1 {
			t.Errorf("the scrub found the tag index damaged %d times, want once", got)
		}
		s := sc.w.sync(t, sc.c, sc.h.repo)
		if _, err := cache.TagRows(filepath.Join(s.Dir, cache.TagsRel)); err != nil {
			t.Errorf("after the repair round the tag index is %v", err)
		}
		if _, ok := sc.c.Tags(s); !ok {
			t.Error("after the repair round no read is handed the tag index")
		}
	})
	t.Run("a restart", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		w.push(t, repo, "refs/heads/main", nil)
		w.tag(t, repo, "v1", true)
		c := w.open(t)
		s := w.sync(t, c, repo)
		flipTags(t, s.Dir, firstDateByte, 0x7f)
		c = w.reopen(t, c)
		s = w.sync(t, c, repo)
		if got, want := w.image(t, c, repo), w.rebuilt(t, repo); got != want {
			t.Errorf("a new process kept the damaged index:\n%s\nwant:\n%s", got, want)
		}
		if got := len(w.logs.lines(t, "code_cache_tags_unreadable")); got != 1 {
			t.Errorf("code_cache_tags_unreadable logged %d times, want once", got)
		}
	})
	t.Run("a tag push", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		w.push(t, repo, "refs/heads/main", nil)
		w.tag(t, repo, "v1", true)
		c := w.open(t)
		s := w.sync(t, c, repo)
		flipTags(t, s.Dir, firstDateByte, 0x7f)
		if !cache.TagsVouched(c, repo) {
			t.Fatal("the flip moved the file's identity: the case needs the vouch to stand")
		}
		w.tag(t, repo, "v2", false)
		w.sync(t, c, repo)
		if got, want := w.image(t, c, repo), w.rebuilt(t, repo); got != want {
			t.Errorf("the merge carried the damage forward:\n%s\nwant:\n%s", got, want)
		}
	})
}

// A TAG INDEX READ THAT FAILS SETS IT ASIDE, logged once, and a round rebuilds it.
//
// Mutations: log per read, or leave the record standing → red.
func TestATagIndexReadThatFailsSetsItAside(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	w.tag(t, repo, "v1", true)
	c := w.open(t)
	s := w.sync(t, c, repo)
	flipTags(t, s.Dir, firstTypesByte, 7<<4|1)
	for range 2 {
		if x, ok := c.Tags(s); ok {
			err := x.Rows(nil, func(cache.TagEntry) bool { return true })
			_ = x.Close()
			if err == nil {
				t.Fatal("a read of a record of type 7 listed it")
			}
		}
	}
	if got := len(w.logs.lines(t, "code_cache_tags_unreadable")); got != 1 {
		t.Errorf("code_cache_tags_unreadable logged %d times across two reads, want once", got)
	}
	if why, _ := cache.ScrubDue(c, repo); why == "" {
		t.Error("the copy's scrub is not due after a read found its tag index damaged")
	}
	eventually(t, "a round builds the index the read set aside", func() bool {
		servable, _ := cache.TagsServable(c, repo)
		return servable
	})
	s = w.sync(t, c, repo)
	if got, want := w.image(t, c, repo), w.rebuilt(t, repo); got != want {
		t.Errorf("the index built again is not a fresh cache's:\n%s\nwant:\n%s", got, want)
	}
	if rows, _ := indexRows(t, c, s); len(rows) != 1 {
		t.Errorf("the index built again lists %d tags, want 1", len(rows))
	}
}

// A STEP THE CACHE STOPS KEEPS THE INDEX, which the next boot levels.
//
// Mutation: set the index aside on the round's end → the boot reads all, red.
func TestAStepTheCacheStopsKeepsTheIndex(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	w.tag(t, repo, "v1", true)
	w.tag(t, repo, "l1", false)
	c := w.open(t)
	s := w.sync(t, c, repo)
	refs, tags := filepath.Join(s.Dir, "packed-refs"), tagsAt(t, s.Dir)
	refsInfo, err := os.Lstat(refs)
	if err != nil {
		t.Fatal(err)
	}
	c.Stop(t.Context())

	entered := make(chan struct{})
	c = w.open(t, func(o *cache.Options) {
		var once sync.Once
		cache.BeforeTags(o, func(ctx context.Context, _ string, reading int) error {
			if reading == 0 {
				return nil // the boot's level check
			}
			once.Do(func() { close(entered) })
			<-ctx.Done()
			return ctx.Err()
		})
	})
	w.sync(t, c, repo)
	trash := names(t, filepath.Join(w.layout.Dir, "trash"))
	w.tag(t, repo, "v2", true)
	done := make(chan error, 1)
	go func() {
		_, serr := c.Sync(t.Context(), repo, linearizable)
		done <- serr
	}()
	<-entered
	c.Stop(t.Context())
	if err := <-done; err == nil {
		t.Fatal("the round the cache stopped answered")
	}
	after, err := os.Lstat(refs)
	if err != nil || !os.SameFile(refsInfo, after) {
		t.Errorf("packed-refs moved under a round the cache stopped (%v)", err)
	}
	if !os.SameFile(tags, tagsAt(t, s.Dir)) {
		t.Error("the tag index moved under a round the cache stopped")
	}
	if trashed := names(t, filepath.Join(w.layout.Dir, "trash")); !slices.Equal(trashed, trash) {
		t.Errorf("trash/ holds %v after a round the cache stopped, and %v before it", trashed, trash)
	}

	var r readings
	c = w.open(t, r.knob)
	w.sync(t, c, repo)
	if got := r.since(0); !slices.Equal(got, []int{1}) {
		t.Errorf("the next process's first round read %v, want the new tag alone", got)
	}
}
