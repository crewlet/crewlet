package codetest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// F9: a git request that finds every transfer slot busy for the slot wait is
// answered 503, and git prints the sentence that names the wait.
func busySlotsAnswer503(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{MaxTransfers: 1}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	held, reached := make(chan struct{}), make(chan struct{})
	c.Fleet.Faults().Arm(codefault.HostHookAdmitted, codefault.Action{Once: true, Block: held, Reached: reached})
	pushed := make(chan Pushed, 1)
	go func() { pushed <- p.Push(c.Ctx, work, c.URL(0), "main") }()
	<-reached
	// THE PUSH HOLDS THE ONE SLOT, so a clone waits the slot wait for it.
	_, stderr, err := NewPerson(t, c.Fleet, "dev").TryClone(c.Ctx, c.URL(0))
	close(held)
	if res := <-pushed; !res.OK("refs/heads/main") {
		t.Errorf("the push that held the slot: %s", res)
	}
	want := fmt.Sprintf("every git slot on this node stayed busy for %d s", int(gitproc.SlotWait/time.Second))
	if err == nil || !strings.Contains(stderr, "503") || !strings.Contains(stderr, want) {
		t.Errorf("a clone with every slot busy: %v\n%s\nwant a 503 saying %q", err, stderr, want)
	}
	assertConverged(t, c.Fleet)
}

// F10: a cache whose pack index is damaged is rebuilt under the fetch that
// found it, and the fetch is served.
func aCorruptCacheIsRebuiltUnderAFetch(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n", "src/a.go": "package a\n"}, "one")
	c.MustPush(p, work, "main")
	damageNewestIndex(t, c, 0)
	clone := p.Clone(c.URL(0))
	if got := p.Head(clone); got != head {
		t.Errorf("a clone of the damaged cache is at %s, want %s", got, head)
	}
	assertConverged(t, c.Fleet)
}

// damageNewestIndex truncates the newest .idx of RepoName's cache on node i
// to 100 bytes: `index file … is too small`, which git classifies corrupt.
func damageNewestIndex(t *testing.T, c *Case, i int) {
	t.Helper()
	rows := c.Rows()
	s, err := c.Fleet.Nodes()[i].Cache().Sync(c.Ctx, rows.Repo.ID, Linearizable)
	if err != nil {
		t.Fatalf("sync the cache: %v", err)
	}
	dir := filepath.Join(s.Dir, "objects", "pack")
	s.Release()
	list, err := filepath.Glob(filepath.Join(dir, "*.idx"))
	if err != nil || len(list) == 0 {
		t.Fatalf("no pack index in %s (%v)", dir, err)
	}
	newest, at := "", time.Time{}
	for _, path := range list {
		if info, err := os.Stat(path); err == nil && info.ModTime().After(at) {
			newest, at = path, info.ModTime()
		}
	}
	if err := os.Truncate(newest, 100); err != nil {
		t.Fatalf("truncate %s: %v", newest, err)
	}
}

// F11: a pack the object store lost is answered 503, naming the pack and the
// alarm that reports it.
func aMissingPackIs503NamingIt(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	rows := c.Rows()
	if len(rows.Packs) != 1 {
		t.Fatalf("one push stored %d packs", len(rows.Packs))
	}
	pack := rows.Packs[0]
	if err := c.Fleet.Objects().Delete(c.Ctx, pack.Object.Name()); err != nil {
		t.Fatalf("delete the pack's object: %v", err)
	}
	node := c.Fleet.Nodes()[0]
	dir := node.CacheDir()
	c.Fleet.Kill(t, node.ID())
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove %s: %v", dir, err)
	}
	c.Fleet.Restart(t, node.ID())
	_, stderr, err := p.TryClone(c.Ctx, c.URL(0))
	if err == nil {
		t.Fatal("a clone of a repository whose pack is gone succeeded")
	}
	for _, want := range []string{"503", pack.PackID, "is missing from the object store", "objects_missing"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("a clone of a repository whose pack is gone printed %q, want it to say %q", stderr, want)
		}
	}
}

// F12: a push is visible through every node at once, under both protocols.
func aPushIsVisibleThroughEveryNodeAtOnce(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{GitNodes: 2}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	_ = p.LsRemote(c.URL(1), 0)
	head := p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	c.MustPush(p, work, "main")
	for _, v := range []int{0, 2} {
		if got := p.LsRemote(c.URL(1), v)["refs/heads/main"]; got != head {
			t.Errorf("at once after the push, the other node advertises main at %q under protocol %d, want %s",
				got, v, head)
		}
	}
	assertConverged(t, c.Fleet)
}

// F13: a fetch from a node that has not seen the commit the client learned
// of elsewhere finds it: the node behind reads the log's head before it
// answers.
func aFetchFromABehindNodeFindsItsWants(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{GitNodes: 2}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	clone := p.Clone(c.URL(1))
	head := p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	c.MustPush(p, work, "main")
	if got := p.LsRemote(c.URL(0), 2)["refs/heads/main"]; got != head {
		t.Fatalf("the pushing node advertises main at %q, want %s", got, head)
	}
	p.Git().Run(t, clone, "fetch", "--quiet", "origin")
	if got := strings.TrimSpace(p.Git().Run(t, clone, "rev-parse", "origin/main")); got != head {
		t.Errorf("a fetch through the node that last synced before the push has main at %s, want %s", got, head)
	}
	assertConverged(t, c.Fleet)
}

// F18: a deleted repository answers that it is deleted and how to bring it
// back, and declaring it again restores it as it was.
func aDeletedRepositoryIsRestoredByDeclaringItAgain(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	c.Fleet.Remove(t, RepoName)
	_, stderr, err := p.TryClone(c.Ctx, c.URL(0))
	if err == nil || !strings.Contains(stderr, "remote error: repository "+RepoName+" is deleted") {
		t.Errorf("a clone of a deleted repository: %v\n%s\nwant remote error: repository %s is deleted …",
			err, stderr, RepoName)
	}
	c.Fleet.Declare(t, code.Declared{Name: RepoName, Settings: Settings()})
	clone := p.Clone(c.URL(0))
	if got := p.Head(clone); got != head {
		t.Errorf("the restored repository's clone is at %s, want %s", got, head)
	}
	assertConverged(t, c.Fleet)
}

// F22: a blobless clone is served under protocol v2, and so is the ONE lazy
// fetch its checkout makes for every blob it lacks — through the other git
// node, as a balancer may send it, whose cache has never read the repository —
// and the files checked out are the files pushed.
func aBloblessCloneChecksOut(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{GitNodes: 2}, Settings())
	bloblessCloneChecksOut(t, c.Fleet, c.URL(0), c.URL(1))
	assertConverged(t, c.Fleet)
}

// bloblessCloneChecksOut is F22 over the repository at url, its checkout's
// lazy fetch sent to lazyURL.
func bloblessCloneChecksOut(t *testing.T, f Fleet, url, lazyURL string) {
	t.Helper()
	p := NewPerson(t, f, "ceo")
	work := p.Init()
	// A TREE OF A FEW HUNDRED FILES, each distinct, so a clone missing every
	// blob lacks exactly one object a file.
	files := map[string]string{}
	for d := range 15 {
		for i := range 20 {
			files[fmt.Sprintf("dir%02d/file%02d.txt", d, i)] = fmt.Sprintf("directory %d, file %d\n", d, i)
		}
	}
	head := p.Commit(work, files, "a tree of a few hundred files")
	if res := p.Push(t.Context(), work, url, "main"); res.Err != nil {
		t.Fatalf("git push main: %s", res)
	}
	clone := filepath.Join(t.TempDir(), "clone")
	if _, stderr, err := p.Git().Try(t, "", nil, "-c", "protocol.version=2", "clone", "--quiet",
		"--filter=blob:none", "--no-checkout", url, clone); err != nil {
		t.Fatalf("git clone --filter=blob:none: %v\n%s", err, stderr)
	}
	if missing := missingObjects(t, p, clone); len(missing) != len(files) {
		t.Fatalf("the blobless clone lacks %d objects, want the %d blobs its tree names: the filter was "+
			"not applied", len(missing), len(files))
	}
	p.Git().Run(t, clone, "remote", "set-url", "origin", lazyURL)
	if _, stderr, err := p.Git().Try(t, clone, nil, "-c", "protocol.version=2", "checkout", "--quiet",
		"main"); err != nil {
		t.Fatalf("the checkout of a blobless clone, whose lazy fetch goes to %s: %v\n%s", lazyURL, err, stderr)
	}
	if got := p.Head(clone); got != head {
		t.Errorf("the blobless clone is at %s, want %s", got, head)
	}
	if missing := missingObjects(t, p, clone); len(missing) != 0 {
		t.Errorf("after its checkout the blobless clone still lacks %d objects", len(missing))
	}
	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(clone, filepath.FromSlash(rel)))
		if err != nil || string(got) != want {
			t.Errorf("the checkout's %s is %q (%v), want %q", rel, got, err, want)
		}
	}
}

// missingObjects is every object dir's history names that dir does not hold —
// read without fetching any, which a promisor repository does by itself for
// any other read of an object it lacks.
func missingObjects(t *testing.T, p *Person, dir string) []string {
	t.Helper()
	var missing []string
	for line := range strings.Lines(p.Git().Run(t, dir, "rev-list", "--objects", "--missing=print", "--all")) {
		if id, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "?"); ok {
			missing = append(missing, id)
		}
	}
	return missing
}

// F23: a partial clone is served ONLY the filters pack-objects answers from
// the bitmap — blob:none, blob:limit, tree at depth 0, object:type and a
// combine of them — and every other is refused in git's own words, which git
// prints: sparse:oid and tree at any depth past 0 walk every object the clone
// reaches instead.
func aFilterABitmapCannotAnswerIsRefused(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{}, Settings())
	filtersABitmapCannotAnswerAreRefused(t, c.Fleet, c.URL(0))
	assertConverged(t, c.Fleet)
}

// filtersABitmapCannotAnswerAreRefused is F23 over the repository at url.
//
// WHAT GIT PRINTS FOR A REFUSAL is read from git itself: the same serve
// configuration over git's own transport, from a copy of the repository — so
// the case holds the host to passing git's line through verbatim on whichever
// git runs it, never to one release's wording. The table, not that oracle,
// says which filters are refused, and a refused filter git's own upload-pack
// serves under the configuration fails the case.
func filtersABitmapCannotAnswerAreRefused(t *testing.T, f Fleet, url string) {
	t.Helper()
	p := NewPerson(t, f, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{
		"README.md": "one\n", "src/a.go": "package a\n",
		// Past blob:limit=1k, so that filter, served, leaves one blob out.
		"big.bin": randomFile(2 << 10),
	}, "one")
	if res := p.Push(t.Context(), work, url, "main"); res.Err != nil {
		t.Fatalf("git push main: %s", res)
	}
	oracle := filepath.Join(t.TempDir(), "oracle.git")
	p.Git().Run(t, "", "clone", "--quiet", "--bare", work, oracle)
	for _, kv := range f.Nodes()[0].Cache().ServeConfig() {
		p.Git().Run(t, oracle, "config", kv.Key, kv.Value)
	}
	for _, tc := range []struct {
		filter  string
		refused bool
	}{
		{"sparse:oid=main:README.md", true},
		{"tree:1", true},
		{"combine:blob:none+tree:1", true},
		{"blob:none", false},
		{"blob:limit=1k", false},
		{"tree:0", false},
		{"object:type=commit", false},
		{"combine:blob:none+tree:0", false},
	} {
		t.Run(tc.filter, func(t *testing.T) {
			dir, stderr, err := p.TryClone(t.Context(), url, "--no-checkout", "--filter="+tc.filter)
			if !tc.refused {
				if err != nil {
					t.Fatalf("git clone --filter=%s: %v\n%s", tc.filter, err, stderr)
				}
				if len(missingObjects(t, p, dir)) == 0 {
					t.Errorf("git clone --filter=%s holds every object: the filter was not applied", tc.filter)
				}
				return
			}
			if err == nil {
				t.Fatalf("git clone --filter=%s was served; want it refused, as a filter the bitmap cannot "+
					"answer", tc.filter)
			}
			_, said, oerr := p.TryClone(t.Context(), "file://"+oracle, "--no-checkout", "--filter="+tc.filter)
			words := remoteError(said)
			if oerr == nil || words == "" {
				t.Fatalf("git's own upload-pack, configured as the cache's, serves --filter=%s (%v):\n%s",
					tc.filter, oerr, said)
			}
			if got := remoteError(stderr); got != words {
				t.Errorf("git clone --filter=%s printed:\n%s\nwant `remote error: %s`, git's own line",
					tc.filter, stderr, words)
			}
		})
	}
}

// remoteError is the text after `remote error: ` on the line git printed it
// on, or "".
func remoteError(stderr string) string {
	for line := range strings.Lines(stderr) {
		if _, words, ok := strings.Cut(strings.TrimSuffix(line, "\n"), "remote error: "); ok {
			return words
		}
	}
	return ""
}
