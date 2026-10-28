package codetest

import (
	"errors"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE TWO BEHAVIOURS THE RECORD PINS (ADR-0054), as checks a topology that
// lies can be run through: each returns nil when the topology behaves, and the
// suite's own meta-test holds each able to fail.

// pausedPush is a push whose record lands while the data node's applier is
// paused: the push reaches the end of its upload, the applier is paused, and
// the push goes on to propose — so its record is durable and applied nowhere.
// It returns what git printed, the commit pushed, and the applier's resume.
func pausedPush(c *Case, p *Person, work string) (Pushed, string, func(), error) {
	c.T.Helper()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	head := p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	held, reached := make(chan struct{}), make(chan struct{})
	c.Fleet.Faults().Arm(codefault.HostAfterUpload, codefault.Action{Once: true, Block: held, Reached: reached})
	done := make(chan Pushed, 1)
	go func() { done <- p.Push(c.Ctx, work, c.URL(0), "main") }()
	select {
	case <-reached:
	case res := <-done:
		return res, head, func() {}, fmt.Errorf("the push ended before its upload did: %s", res)
	}
	resume := c.Fleet.PauseApplier(c.T, c.Fleet.DataNodes()[0])
	close(held)
	return <-done, head, resume, nil
}

// okFromRows is the check itself: every ref git printed `ok` for is at the
// pushed commit in the rows as the data node holds them NOW — read stale, so
// what the applier has not written is not there to be found.
func okFromRows(c *Case, res Pushed, head string) error {
	if !res.OK("refs/heads/main") {
		return nil
	}
	rows, err := c.Fleet.Rows(c.Ctx, RepoName, Stale)
	if err != nil {
		return fmt.Errorf("the rows behind an ok: %w", err)
	}
	if at := rows.Refs["refs/heads/main"]; at != head {
		return fmt.Errorf("git printed ok for refs/heads/main at %s while the applied rows hold it at %q: "+
			"an ok answered from a decision rather than from the push's own row", head, at)
	}
	return nil
}

// OkOnlyFromARow is the check that git is told `ok` for a ref only once the
// push's own row has applied: a push whose record is durable while the
// applier is paused must not print ok for a ref the rows do not yet hold —
// and an ENGINE WRITE is answered applied only from its row, the same.
func OkOnlyFromARow(t *testing.T, newFleet NewFleet) error {
	t.Helper()
	c := newCase(t, newFleet, Shape{}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	res, head, resume, err := pausedPush(c, p, p.Init())
	if err == nil {
		err = okFromRows(c, res, head)
	}
	resume()
	if err != nil {
		return err
	}
	rows := c.Rows()
	w := commitFiles(t, rows.Repo.ID, seatAt(t, FixtureAlice), "refs/heads/seats/alice/w", "",
		rows.Refs["refs/heads/main"], "a write", put("w.md", "w\n"))
	written, resume, err := pausedWrite(c, w)
	if err == nil {
		err = writeOkFromRows(c.Ctx, c.Fleet, written, w.Ref)
	}
	resume()
	return err
}

// NothingButSyncWritesACache is the check that a cache is a function of the
// rows: after pushes of branches and tags, a fetch and an engine write built
// beside the cache, every node's cache, synced linearizable, holds exactly the
// rows' refs, HEAD and packs, and nothing a sync never writes.
func NothingButSyncWritesACache(t *testing.T, newFleet NewFleet) error {
	t.Helper()
	c := newCase(t, newFleet, Shape{}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	if res := p.Push(c.Ctx, work, c.URL(0), "main"); res.Err != nil {
		return fmt.Errorf("the first push: %s", res)
	}
	p.Git().Run(t, work, "tag", "v1")
	p.Commit(work, map[string]string{"src/a.go": "package a\n"}, "two")
	if res := p.Push(c.Ctx, work, c.URL(0), "main", "main:refs/heads/topic", "v1"); res.Err != nil {
		return fmt.Errorf("the second push: %s", res)
	}
	clone := p.Clone(c.URL(0))
	if _, stderr, err := p.Git().Try(t, clone, nil, "fetch", "--tags", c.URL(0)); err != nil {
		return fmt.Errorf("a fetch: %w\n%s", err, stderr)
	}
	rows := c.Rows()
	res, err := c.Fleet.Write(c.Ctx, commitFiles(t, rows.Repo.ID, seatAt(t, FixtureAlice),
		"refs/heads/seats/alice/w", "", rows.Refs["refs/heads/main"], "a write", put("w.md", "w\n")))
	if err != nil || res.Outcome != statelog.OutcomeApplied {
		return fmt.Errorf("an engine write: %+v %w", res, err)
	}
	return converged(c.Ctx, c.Fleet)
}

// errInjected is what an armed fault answers a step with.
var errInjected = errors.New("codetest: an injected failure")
