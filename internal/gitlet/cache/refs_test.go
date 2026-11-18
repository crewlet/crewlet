package cache_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
)

// PACKED-REFS CARRY PEELED TAGS: under the `peeled` trait git trusts a tag's
// peel line rather than reading the tag object, so an advertisement of
// thousands of tags reads no tag at all — and a delta that moves a branch
// carries every other line, a tag's peel line included, byte for byte.
func TestPackedRefsCarryPeeledTags(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/dev", nil)
	commit, _ := w.push(t, repo, "refs/heads/main", nil)
	annotated := w.tag(t, repo, "v1", true)
	w.tag(t, repo, "light", false)
	c := w.open(t)
	s := w.sync(t, c, repo)
	path := filepath.Join(s.Dir, "packed-refs")
	refs := read(t, path)
	lines := strings.Split(strings.TrimSuffix(refs, "\n"), "\n")
	if lines[0] != "# pack-refs with: peeled sorted " {
		t.Errorf("the header is %q", lines[0])
	}
	var names []string
	for i, line := range lines[1:] {
		if strings.HasPrefix(line, "^") {
			if prev := lines[i]; !strings.HasSuffix(prev, " refs/tags/v1") || line != "^"+commit {
				t.Errorf("a peel line %q after %q, want ^%s after the annotated tag alone", line, prev, commit)
			}
			continue
		}
		names = append(names, strings.SplitN(line, " ", 2)[1])
	}
	if !slices.IsSorted(names) || len(names) != 4 {
		t.Errorf("refs %v, want four in byte order", names)
	}
	if !strings.Contains(refs, annotated+" refs/tags/v1\n^"+commit+"\n") {
		t.Errorf("the annotated tag is not followed by its peel:\n%s", refs)
	}

	res, err := w.pool.Run(t.Context(), gitproc.Cmd{
		Dir: s.Dir, Args: []string{"upload-pack", "--stateless-rpc", "--advertise-refs", s.Dir},
		Config: c.ServeConfig(), Class: gitproc.ClassTransfer, Share: suiteShare, Deadline: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Stdout), commit+" refs/tags/v1^{}") {
		t.Errorf("upload-pack does not advertise the tag's peel:\n%s", res.Stdout)
	}

	w.push(t, repo, "refs/heads/dev", nil)
	w.sync(t, c, repo)
	moved := read(t, path)
	keep := func(text string) []string {
		var out []string
		for _, line := range strings.Split(text, "\n") {
			if !strings.HasSuffix(line, " refs/heads/dev") {
				out = append(out, line)
			}
		}
		return out
	}
	if !slices.Equal(keep(moved), keep(refs)) {
		t.Errorf("a delta moving one branch changed other lines:\n%s\nwas:\n%s", moved, refs)
	}

	w.log.write(repo, nil, map[string]string{"refs/tags/v1": ""})
	w.sync(t, c, repo)
	if got := read(t, path); strings.Contains(got, "refs/tags/v1") || strings.Contains(got, "^") {
		t.Errorf("deleting the annotated tag left it or its peel:\n%s", got)
	}
}

// A DAMAGED REFS FILE IS REWRITTEN WHOLE: a delta merges only onto the file
// this cache wrote. A flipped byte would carry the damage forward; a file an
// operator's rm or a disk fault took, or a directory in its place, leaves
// nothing to carry. Either way the round reads the repository whole and writes
// the file afresh, rather than failing every delta until a restart.
func TestADamagedRefsFileIsRewrittenWhole(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		damage func(t *testing.T, path string)
	}{
		{"a flipped byte", func(t *testing.T, path string) {
			raw := []byte(read(t, path))
			at := strings.Index(string(raw), "\n") + 1
			raw[at] ^= 0x01 // the first object id's first digit: still the grammar
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"a removed file", func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
		{"a directory in its place", func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			repo := w.repo(t, 1)
			w.push(t, repo, "refs/heads/main", nil)
			w.push(t, repo, "refs/heads/dev", nil)
			cc := w.open(t)
			s := w.sync(t, cc, repo)
			c.damage(t, filepath.Join(s.Dir, "packed-refs"))

			for range 2 {
				w.push(t, repo, "refs/heads/main", nil)
				pages := len(w.log.firstPages(repo))
				if s = w.sync(t, cc, repo); s.Kind != cache.SyncFull {
					t.Errorf("Kind = %s, want full", s.Kind)
				}
				firsts := w.log.firstPages(repo)[pages:]
				if len(firsts) != 2 || firsts[0].Since.IsZero() || !firsts[1].Since.IsZero() {
					t.Errorf("page-1 reads %+v, want the delta then the whole answer", firsts)
				}
				if got := w.image(t, cc, repo); got != w.rebuilt(t, repo) {
					t.Errorf("image:\n%s\nwant:\n%s", got, w.rebuilt(t, repo))
				}
				// Rewritten whole, the file is this cache's again: the next
				// push is a delta merged onto it.
				w.push(t, repo, "refs/heads/dev", nil)
				if s = w.sync(t, cc, repo); s.Kind != cache.SyncDelta {
					t.Errorf("the push after the repair cost %s, want a delta", s.Kind)
				}
				c.damage(t, filepath.Join(s.Dir, "packed-refs"))
			}
		})
	}
}

// A DELTA'S MERGE ALLOCATES FOR WHAT MOVED, NEVER FOR THE FILE: moving one ref
// in a packed-refs twenty times larger allocates no more (mergeDelta).
//
// The least of ten one-merge counts: AllocsPerRun counts the whole process, and
// nats-server, which this binary links, allocates on a timer (server/pse's
// once-a-second CPU sample). Samples come at a rate, so more runs do not average
// them away; a sample only adds, and all ten windows hold one only if a merge
// nears half a second, so the least count is the merge's own.
//
// Mutation: carry a line as a string (io.WriteString(w, string(line.raw))) and
// 20 000 refs allocate ~19 000 times more than 1 000; allocate once every
// 10 000 carried lines, and two more.
func TestADeltaMergeAllocatesForWhatMovedNotForTheFile(t *testing.T) {
	// Not parallel: testing.AllocsPerRun counts every goroutine's
	// allocations, and refuses to run beside a parallel case.
	allocs := func(refs int) float64 {
		dir, digest := packedRefsOf(t, refs)
		moved := []gitlet.RefRow{{Ref: fmt.Sprintf("refs/heads/b%06d", refs/2+1), OID: strings.Repeat("e", 40)}}
		var err error
		merge := func() {
			got, merr := cache.MergeRefs(dir, moved, digest, io.Discard)
			if merr == nil && got != refs {
				merr = fmt.Errorf("the merge wrote %d refs, want %d", got, refs)
			}
			if err == nil {
				err = merr
			}
		}
		least := math.Inf(1)
		for range 10 {
			least = min(least, testing.AllocsPerRun(1, merge))
			if err != nil {
				t.Fatalf("merging one ref onto %d: %v", refs, err)
			}
		}
		return least
	}
	small, large := allocs(1000), allocs(20000)
	if large > small {
		t.Errorf("moving one ref allocated %v times in a file of 1 000 refs and %v in one of 20 000: "+
			"a carried line allocates", small, large)
	}
}

// packedRefsOf writes a packed-refs of n refs as this cache writes one — every
// tenth an annotated tag with its peel line — into a directory of its own,
// and answers the directory and the file's digest.
func packedRefsOf(t testing.TB, n int) (string, string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("# pack-refs with: peeled sorted \n")
	oid, peel := strings.Repeat("a", 40), strings.Repeat("b", 40)
	var tags []string
	for i := range n {
		if i%10 == 0 {
			tags = append(tags, fmt.Sprintf("%s refs/tags/t%06d\n^%s\n", oid, i, peel))
			continue
		}
		fmt.Fprintf(&b, "%s refs/heads/b%06d\n", oid, i)
	}
	for _, tag := range tags {
		b.WriteString(tag)
	}
	dir := t.TempDir()
	write(t, filepath.Join(dir, "packed-refs"), b.String())
	sum := sha256.Sum256([]byte(b.String()))
	return dir, hex.EncodeToString(sum[:])
}

// A DEFAULT BRANCH CHANGE MOVES HEAD, on a delta that moves no ref.
func TestADefaultBranchChangeMovesHEAD(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/trunk", nil)
	c := w.open(t)
	w.sync(t, c, repo).Release()
	w.log.settings(repo, "trunk")
	s := w.sync(t, c, repo)
	if s.Kind != cache.SyncDelta {
		t.Errorf("Kind = %s, want delta", s.Kind)
	}
	if got := read(t, filepath.Join(s.Dir, "HEAD")); got != "ref: refs/heads/trunk\n" {
		t.Errorf("HEAD = %q", got)
	}
	if s.Repo.DefaultBranch != "trunk" {
		t.Errorf("Synced.Repo names the default branch %q", s.Repo.DefaultBranch)
	}
}
