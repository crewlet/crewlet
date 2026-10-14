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
