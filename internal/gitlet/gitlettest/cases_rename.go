package gitlettest

import (
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// F54: a renamed repository's old name redirects until the retention pass
// releases it. A person creates a project repository with a tool (a
// config-managed one takes no tool's rename), pushes, and renames it through
// the fleet's routing (code.lifecycle). For gitlet.RedirectWindow the old name
// serves a clone, redirects one presenting its credential up front, refuses a
// push (naming the new name, pushing nothing), and survives a retention pass.
// Past the window (the node's and the broker's clocks moved together) the
// pass releases it: it names nothing, and the new name serves as before.
//
// Red when the release's apply frees no old name, the store publishes no
// release record, a push lands at the old name, or the old name serves a
// fetch's advertisement in place of its 302.
func aRenamedRepositoryRedirectsAndIsReleased(t *testing.T, newFleet NewFleet) {
	clock := &Clock{}
	c := newCase(t, newFleet, Shape{Clock: clock}, Settings())
	const from, to = "renamed-from", "renamed-to"
	ask := func(what string, r gitlet.LifecycleRequest) gitlet.LifecycleResult {
		t.Helper()
		r.OpID, r.Actor = newOp(t), personAt(FixturePat)
		res, err := c.Fleet.Lifecycle(c.Ctx, r)
		if err != nil {
			t.Fatalf("the %s through the fleet: %v", what, err)
		}
		if res.Refusal != "" || res.Outcome != statelog.OutcomeApplied {
			t.Fatalf("the %s through the fleet was answered %+v, want applied", what, res)
		}
		return res
	}
	settings := Settings()
	settings.ManagedBy, settings.Kind = gitlet.ManagedTools, gitlet.RepoProject
	id := ask("create", gitlet.LifecycleRequest{Kind: gitlet.LifecycleCreate, Name: from, Create: &settings}).Repo.ID

	p := NewPerson(t, c.Fleet, FixturePat)
	url := func(name string) string { return c.Fleet.Nodes()[0].URL() + "/git/" + name }
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	if res := p.Push(c.Ctx, work, url(from), "main"); res.Err != nil {
		t.Fatalf("the person's push to %s: %s", from, res)
	}
	renamed := ask("rename", gitlet.LifecycleRequest{Kind: gitlet.LifecycleRename, RepoID: id, Name: to})
	if renamed.Repo.ID != id || renamed.Repo.Name != to {
		t.Fatalf("the rename answered %+v, want %s under %s", renamed.Repo, id, to)
	}

	// While the old name redirects.
	old, err := c.Fleet.Rows(c.Ctx, from, Linearizable)
	if err != nil || old.Repo.ID != id || old.Repo.Name != to {
		t.Fatalf("the old name resolves to %+v (%v), want %s under %s", old.Repo, err, id, to)
	}
	if got := p.Head(p.Clone(url(from))); got != head {
		t.Fatalf("the person's clone of the old name checked out %s, want %s", got, head)
	}
	// The clone above passes whether the old name redirects or serves in
	// place: git meets the 302 only on the credential helper's retry, which it
	// follows without rebasing. A clone presenting its credential up front
	// (http.extraHeader, a CI checkout's shape) meets it first and rebases.
	basic := base64.StdEncoding.EncodeToString([]byte("crewlet:" + p.Token()))
	upFront := filepath.Join(t.TempDir(), "up-front")
	if _, stderr, err := p.Git().Try(t, "", nil, "-c", "http.extraHeader=Authorization: Basic "+basic,
		"clone", url(from), upFront); err != nil {
		t.Fatalf("a clone of the old name with its credential up front: %v\n%s", err, stderr)
	} else if !strings.Contains(stderr, "redirecting to "+url(to)+".git/") {
		t.Fatalf("a clone of the old name with its credential up front followed no redirect to %s:\n%s",
			to, stderr)
	}
	if got := p.Head(upFront); got != head {
		t.Fatalf("the up-front clone of the old name checked out %s, want %s", got, head)
	}
	p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	pushed := p.Push(c.Ctx, work, url(from), "main")
	if pushed.Err == nil {
		t.Fatalf("a push to the old name went through: %s", pushed)
	}
	if !strings.Contains(pushed.Stderr, `"`+from+`" was renamed to "`+to+`"`) {
		t.Fatalf("a push to the old name was not told the new name: %s", pushed)
	}
	if got := p.LsRemote(url(to), 0)["refs/heads/main"]; got != head {
		t.Fatalf("after a push to the old name, main is %s, want %s unmoved", got, head)
	}

	// A pass inside the window releases nothing.
	if report := c.Fleet.Retain(t, clock.Now()); report.Released != 0 {
		t.Fatalf("a retention pass inside the redirect window released %d names", report.Released)
	}
	if kept, err := c.Fleet.Rows(c.Ctx, from, Linearizable); err != nil || kept.Repo.ID != id {
		t.Fatalf("inside the redirect window the old name resolves to %+v (%v), want %s under %s",
			kept.Repo, err, id, to)
	}

	// A month on, with every record applied before it read at the old time,
	// the pass releases the old name.
	if err := c.Fleet.Level(c.Ctx, c.Fleet.DataNodes()[0]); err != nil {
		t.Fatalf("level the data node before the clock moves: %v", err)
	}
	clock.Elapse(gitlet.RedirectWindow + time.Hour)
	if report := c.Fleet.Retain(t, clock.Now()); report.Released != 1 || len(report.Failed) != 0 {
		t.Fatalf("a retention pass past the redirect window released %d names (failed %v), want the old one",
			report.Released, report.Failed)
	}
	if gone, err := c.Fleet.Rows(c.Ctx, from, Linearizable); !errors.Is(err, gitlet.ErrRepoNotFound) {
		t.Fatalf("released, the old name resolves to %+v (%v), want no repository", gone.Repo, err)
	}
	if _, stderr, err := p.Git().Try(t, "", nil, "clone", url(from), t.TempDir()+"/released"); err == nil {
		t.Fatalf("a clone of the released old name succeeded:\n%s", stderr)
	}
	if cur, err := c.Fleet.Rows(c.Ctx, to, Linearizable); err != nil || cur.Repo.ID != id ||
		cur.Refs["refs/heads/main"] != head {
		t.Fatalf("the new name resolves to %+v (%v), want %s at %s", cur.Repo, err, id, head)
	}
	assertConverged(t, c.Fleet)
}
