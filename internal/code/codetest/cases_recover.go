package codetest

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
)

// F21: a crash at every step of a push leaves a state the client was told
// the truth about — never an ok for a ref no row moved — that every cache
// converges to, that leaves no object no row names once the collector has run,
// and that the same push re-run completes.
func aCrashAtEveryPushStepLeavesATruthfulState(t *testing.T, newFleet NewFleet) {
	for _, point := range codefault.PushPath() {
		t.Run(string(point), func(t *testing.T) { crashAt(t, newFleet, point) })
	}
}

// crashAt is F21 at one point. A CRASH, never a Block: a node killed in this
// process cancels the hit's context, and a Block would let the step's own
// error path tidy up before the restart — which a process that died never
// does (internal/code/codefault).
func crashAt(t *testing.T, newFleet NewFleet, point codefault.Point) {
	c := newCase(t, newFleet, Shape{}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	head := p.Commit(work, map[string]string{"big.bin": randomFile(64 << 10)}, "two")

	reached := make(chan struct{})
	c.Fleet.Faults().Arm(point, codefault.Action{Once: true, Crash: true, Reached: reached})
	done := make(chan Pushed, 1)
	go func() { done <- p.Push(c.Ctx, work, c.URL(0), "main") }()
	select {
	case <-reached:
	case res := <-done:
		t.Fatalf("the push ended before it reached %s: %s", point, res)
	}
	killed := c.Fleet.Nodes()[0].ID()
	if point == codefault.CommitterAfterAppend {
		killed = c.Fleet.DataNodes()[0]
	}
	c.Fleet.Kill(t, killed)
	res := <-done
	c.Fleet.Restart(t, killed)

	if res.OK("refs/heads/main") {
		if at := c.Rows().Refs["refs/heads/main"]; at != head {
			t.Errorf("git printed ok for main at %s and the rows hold %s: %s", head, at, res)
		}
	}
	for _, n := range c.Fleet.Nodes() {
		for _, sub := range []string{"scratch", "tmp", "trash"} {
			dir := filepath.Join(n.CacheDir(), sub)
			if !within(10*time.Second, func() bool { return emptyDir(dir) }) {
				t.Errorf("%s's %s/ still holds %v after the restart", n.ID(), sub, entries(dir))
			}
		}
	}
	assertConverged(t, c.Fleet)
	assertNoUnnamedObject(t, c)

	again := p.Push(c.Ctx, work, c.URL(0), "main")
	if again.Err != nil {
		t.Errorf("the push re-run after a crash at %s: %s", point, again)
	}
	if got := c.Rows().Refs["refs/heads/main"]; got != head {
		t.Errorf("after the re-run main is at %s, want %s", got, head)
	}
	assertConverged(t, c.Fleet)
}

// assertNoUnnamedObject fails t unless, once the collector has run, every
// object the store holds is named by a pack row: an upload a crash orphaned
// is the collector's to remove — it is the one deleter (ADR-0027) — and it
// must find and remove it.
func assertNoUnnamedObject(t *testing.T, c *Case) {
	t.Helper()
	report := c.Fleet.Collect(t)
	named := map[string]bool{}
	for _, p := range c.Rows().Packs {
		named[p.Object.Name()] = true
	}
	for _, name := range objectNames(t, c.Fleet.Objects()) {
		if !named[name] {
			t.Errorf("%s is named by no pack row and the collector left it (%+v)", name, report)
		}
	}
}

// R4: a restart sweeps the scratch repository a push was received in when
// the process died.
func aRestartSweepsScratch(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	reached := make(chan struct{})
	c.Fleet.Faults().Arm(codefault.HostAfterScratch, codefault.Action{Once: true, Crash: true, Reached: reached})
	done := make(chan Pushed, 1)
	go func() { done <- p.Push(c.Ctx, work, c.URL(0), "main") }()
	<-reached
	node := c.Fleet.Nodes()[0]
	if emptyDir(filepath.Join(node.CacheDir(), "scratch")) {
		t.Fatal("the push held after its scratch was made left no scratch to sweep")
	}
	c.Fleet.Kill(t, node.ID())
	<-done
	c.Fleet.Restart(t, node.ID())
	if got := entries(filepath.Join(node.CacheDir(), "scratch")); len(got) > 0 {
		t.Errorf("after the restart scratch/ holds %v", got)
	}
	trash := filepath.Join(node.CacheDir(), "trash")
	if !within(10*time.Second, func() bool { return emptyDir(trash) }) {
		t.Errorf("the janitor left %v in trash/", entries(trash))
	}
	assertConverged(t, c.Fleet)
}

// R6: a purged repository leaves nothing — no row, no object once the
// collector has run, no cache directory — and its name, declared again, is a
// new, empty repository.
func aPurgedRepositoryLeavesNothing(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	old := c.Rows().Repo.ID
	c.Fleet.Remove(t, RepoName)
	c.Fleet.Purge(t)
	if _, err := c.Fleet.Rows(c.Ctx, RepoName, Linearizable); !errors.Is(err, code.ErrRepoNotFound) {
		t.Errorf("a purged repository's rows: %v, want code.ErrRepoNotFound", err)
	}
	c.Fleet.Collect(t)
	if names := objectNames(t, c.Fleet.Objects()); len(names) > 0 {
		t.Errorf("the collector left %v of a purged repository", names)
	}
	for _, n := range c.Fleet.Nodes() {
		if s, err := n.Cache().Sync(c.Ctx, old, Linearizable); err == nil {
			s.Release()
			t.Errorf("%s's cache still serves the purged repository", n.ID())
		}
		repos := filepath.Join(n.CacheDir(), "repos")
		if !within(10*time.Second, func() bool {
			return !slices.ContainsFunc(entries(repos), func(e string) bool { return strings.HasPrefix(e, old+".") })
		}) {
			t.Errorf("%s's cache still holds a directory of the purged repository: %v", n.ID(), entries(repos))
		}
	}
	c.Fleet.Declare(t, code.Declared{Name: RepoName, Settings: Settings()})
	rows := c.Rows()
	if rows.Repo.ID == old || len(rows.Refs) > 0 || len(rows.Packs) > 0 {
		t.Errorf("the name declared again is %s with %d refs and %d packs, want a new, empty repository",
			rows.Repo.ID, len(rows.Refs), len(rows.Packs))
	}
	assertConverged(t, c.Fleet)
}

// R7: while the data node is evicted nothing takes a push, and git is told
// so — nobody ran it, so a re-run lands it — and once readmitted the push
// lands.
func anEvictedNodesPushesAreRefused(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	head := p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	data := c.Fleet.DataNodes()[0]
	c.Fleet.Evict(t, data)
	res := p.Push(c.Ctx, work, c.URL(0), "main")
	wantRefused(t, res, "refs/heads/main", "unavailable")
	if reason, _ := res.Rejected("refs/heads/main"); !strings.Contains(reason, "no data node took push ") {
		t.Errorf("an evicted node's push printed %q, want (unavailable: no data node took push …)", reason)
	}
	c.Fleet.Readmit(t, data)
	if again := p.Push(c.Ctx, work, c.URL(0), "main"); !again.OK("refs/heads/main") {
		t.Errorf("the push once readmitted: %s", again)
	}
	if got := c.Rows().Refs["refs/heads/main"]; got != head {
		t.Errorf("main is at %s, want %s", got, head)
	}
	assertConverged(t, c.Fleet)
}
