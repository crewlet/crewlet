package codetest

import (
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
)

// F54: A RENAMED REPOSITORY'S OLD NAME REDIRECTS UNTIL THE RETENTION PASS
// RELEASES IT, through the topology: a person makes a project repository with
// a tool's create and pushes to it, then renames it, both asked through the
// fleet's routing (code.lifecycle) as a person's tool asks them. For
// code.RedirectWindow the old name answers for the repository — a person's
// clone of the old URL checks it out, one presenting its credential up front
// follows the redirect to the new name, and a push there is
// refused at the old name, naming the new one and pushing nothing — and a
// retention pass while the window lasts releases nothing. Once the fleet's
// clock is past the window — the deciding node's and the broker's together,
// as a month passing moves them — the pass releases the old name, which then
// names nothing: the rows resolve no repository by it and a clone of it
// fails, while the new name serves the repository as before.
//
// A config-managed repository takes no tool's rename (code.repos names it),
// so the repository the case renames is one a person's tool made.
//
// Mutations: free no old name in the release's apply — the old name still
// redirects after the pass, red; publish no release record from the store —
// the pass releases none, red; admit a push at the old name — it lands, red;
// serve a fetch's advertisement at the old name in place of its 302 — the
// up-front clone is not rebased onto the new name, red.
func aRenamedRepositoryRedirectsAndIsReleased(t *testing.T, newFleet NewFleet) {
	clock := &Clock{}
	c := newCase(t, newFleet, Shape{Clock: clock}, Settings())
	const from, to = "renamed-from", "renamed-to"
	ask := func(what string, r code.LifecycleRequest) code.LifecycleResult {
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
	settings.ManagedBy, settings.Kind = code.ManagedTools, code.RepoProject
	id := ask("create", code.LifecycleRequest{Kind: code.LifecycleCreate, Name: from, Create: &settings}).Repo.ID

	p := NewPerson(t, c.Fleet, FixturePat)
	url := func(name string) string { return c.Fleet.Nodes()[0].URL() + "/git/" + name }
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	if res := p.Push(c.Ctx, work, url(from), "main"); res.Err != nil {
		t.Fatalf("the person's push to %s: %s", from, res)
	}
	renamed := ask("rename", code.LifecycleRequest{Kind: code.LifecycleRename, RepoID: id, Name: to})
	if renamed.Repo.ID != id || renamed.Repo.Name != to {
		t.Fatalf("the rename answered %+v, want %s under %s", renamed.Repo, id, to)
	}

	// THE OLD NAME, WHILE IT REDIRECTS: the rows answer the repository under
	// its new name by it, a clone of it checks the repository out, and a push
	// to it is told the new name and pushes nothing.
	old, err := c.Fleet.Rows(c.Ctx, from, Linearizable)
	if err != nil || old.Repo.ID != id || old.Repo.Name != to {
		t.Fatalf("the old name resolves to %+v (%v), want %s under %s", old.Repo, err, id, to)
	}
	if got := p.Head(p.Clone(url(from))); got != head {
		t.Fatalf("the person's clone of the old name checked out %s, want %s", got, head)
	}
	// FOLLOWING THE REDIRECT, which the clone above cannot show: a person's
	// git meets the 302 only on the retry its credential helper answered,
	// which git follows without rebasing, so that clone works whether the old
	// name redirects or serves the repository in place. A clone presenting
	// its credential up front (http.extraHeader, a CI checkout's shape) meets
	// the 302 on its first request, rebases onto the new name and says so.
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

	// A PASS WHILE THE REDIRECT LASTS releases nothing.
	if report := c.Fleet.Retain(t, clock.Now()); report.Released != 0 {
		t.Fatalf("a retention pass inside the redirect window released %d names", report.Released)
	}
	if kept, err := c.Fleet.Rows(c.Ctx, from, Linearizable); err != nil || kept.Repo.ID != id {
		t.Fatalf("inside the redirect window the old name resolves to %+v (%v), want %s under %s",
			kept.Repo, err, id, to)
	}

	// A MONTH ON: every record applied before it read as it was, the pass
	// releases the old name.
	if err := c.Fleet.Level(c.Ctx, c.Fleet.DataNodes()[0]); err != nil {
		t.Fatalf("level the data node before the clock moves: %v", err)
	}
	clock.Elapse(code.RedirectWindow + time.Hour)
	if report := c.Fleet.Retain(t, clock.Now()); report.Released != 1 || len(report.Failed) != 0 {
		t.Fatalf("a retention pass past the redirect window released %d names (failed %v), want the old one",
			report.Released, report.Failed)
	}
	if gone, err := c.Fleet.Rows(c.Ctx, from, Linearizable); !errors.Is(err, code.ErrRepoNotFound) {
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
