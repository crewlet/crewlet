package gitlettest

import (
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/rendezvous"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The engine write's fleet cases (ADR-0089, ADR-0092): what only more than one
// Gitlet node makes of a write asked through the estate's routing
// (code.git_write).

// writesOf is every push row of rc's repository that names push id.
func (rc *repoCase) writesOf(id string) []gitlet.PushRow {
	rc.t.Helper()
	var rows []gitlet.PushRow
	for _, row := range rc.pushRows() {
		if row.PushID == id {
			rows = append(rows, row)
		}
	}
	return rows
}

// F40: every member routes an engine write to the repository's home
// (ADR-0089). A seat's write asked through each member's router, the home's
// included (it asks its own git half in process), is built by the home alone:
// every push row names it (gitlet.PushRow.Node). On a cluster the home and the
// repository's committer are one member, one rendezvous order over one set. A
// router asking its own member first would warm a copy on every member a seat
// writes through, and build each write beside a copy cold until it synced.
func everyMemberRoutesAnEngineWriteToTheHome(t *testing.T, c Cluster) {
	rc := newRepoCase(t, c, "fleet-f40")
	home := rc.committer()
	p := NewPerson(t, c, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	rc.mustPush(p, work, rc.other(home), "main")

	ref := "refs/heads/seats/" + FixtureAlice + "/f40"
	// at is the branch's head once a write made it, and base its start until
	// then; asked is the member each write was asked through, by its push.
	at, base := "", head
	asked := map[string]string{}
	for i, via := range c.DataNodes() {
		w := commitFiles(t, rc.id, seatAt(t, FixtureAlice), ref, at, base, "asked through "+via,
			put(fmt.Sprintf("f40/%d.txt", i), via+"\n"))
		res, err := c.GitWriteThrough(rc.ctx, via, w)
		if err != nil || res.Outcome != statelog.OutcomeApplied || res.Commit == "" {
			t.Fatalf("the write asked through %s = (%+v, %v), want it applied", via, res, err)
		}
		asked[res.PushID], at, base = via, res.Commit, ""
	}
	for id, via := range asked {
		rows := rc.writesOf(id)
		if len(rows) != 1 || rows[0].Node != home {
			t.Errorf("the write asked through %s left the push rows %+v, want one, built by the repository's home %s",
				via, rows, home)
		}
	}
	if got := rc.rows().Refs[ref]; got != at {
		t.Errorf("%s is at %s, want the last write's commit %s", ref, got, at)
	}
	rc.converged()
}

// F41: an engine write whose home dies after its append is one push
// (ADR-0092). The home lands a seat's write and is killed, broker member and
// all, at gitletfault.HostAfterPropose (a Crash, as F21's). The member it was
// asked through hears nothing for its router's git write attempt, asks the
// runner-up the same operation, and is answered from that push's row, nothing
// built: applied, one row, built by the home; every copy then converges.
//
// The runner-up holds a copy before the home dies (the first push went
// through it): the walk past a silent home asks it unstamped (a silent node may
// be filling the copy it was asked to), and a node holding none fills nothing
// for a write not stamped its walk's home (F27's rule). The case is the
// push's, not the fill's.
func anEngineWriteWhoseHomeDiesAfterAppendIsOnePush(t *testing.T, c Cluster) {
	rc := newRepoCase(t, c, "fleet-f41")
	order := rendezvous.Order(rc.id, c.DataNodes())
	home, runnerUp, via := order[0], order[1], order[2]
	p := NewPerson(t, c, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	rc.mustPush(p, work, runnerUp, "main")
	if !memberNode(t, c, runnerUp).Cache().Holds(rc.id) {
		t.Fatalf("the premise: the runner-up %s holds no copy of a repository pushed through it", runnerUp)
	}

	w := commitFiles(t, rc.id, seatAt(t, FixtureAlice), "refs/heads/seats/"+FixtureAlice+"/f41", "", head,
		"built by the home", put("f41.txt", "f41\n"))
	pushID := gitlet.WritePushID(w.OpID)
	reached := make(chan struct{})
	c.Faults().Arm(gitletfault.HostAfterPropose, gitletfault.Action{Once: true, Crash: true, Reached: reached})
	type answer struct {
		res gitlet.EngineWriteResult
		err error
	}
	done := make(chan answer, 1)
	go func() {
		res, err := c.GitWriteThrough(rc.ctx, via, w)
		done <- answer{res: res, err: err}
	}()
	select {
	case <-reached:
	case a := <-done:
		t.Fatalf("the write ended before its home's proposal was answered: (%+v, %v)", a.res, a.err)
	}
	c.Kill(t, home)
	restarted := false
	defer func() {
		if !restarted {
			c.Restart(t, home)
		}
	}()

	a := <-done
	if a.err != nil || a.res.Outcome != statelog.OutcomeApplied || a.res.PushID != pushID || a.res.Commit == "" {
		t.Fatalf("a write whose home died after its append = (%+v, %v), want it applied under its own push %s, "+
			"answered from the row its home landed", a.res, a.err, pushID)
	}
	if rows := rc.writesOf(pushID); len(rows) != 1 || rows[0].Node != home {
		t.Errorf("the write left the push rows %+v, want one, built by its home %s", rows, home)
	}
	if got := rc.rows().Refs[w.Ref]; got != a.res.Commit {
		t.Errorf("%s is at %s, want the write's commit %s", w.Ref, got, a.res.Commit)
	}
	rc.converged()
	c.Restart(t, home)
	restarted = true
	rc.converged()
}

// F42: a revoked seat is refused once the lagging committer sheds
// (M3-decisions M3-O8). The repository's committer (its home too) keeps the
// chart a revision took alice out of, confirmed behind the configuration. Her
// write asked through another member is refused not_permitted with nothing
// landed: the member behind answers it behind_config, handing the walk to the
// runner-up, whose chart has no alice. A person's push, whose grant reads no
// chart, lands: only what the revision revoked is refused.
//
// Red only with all three arms dropped: the git half's behind_config
// (host.Options.Current), asking a current committer first
// (estate.Backend.Current), and the committer behind handing a seat's proposal
// on unjudged (gitlet.CommitterOptions.Current).
func aRevokedSeatIsRefusedOnceTheLaggingCommitterSheds(t *testing.T, c Cluster) {
	rc := newRepoCase(t, c, "fleet-f42")
	behind := rc.committer()
	via := rc.other(behind)
	p := NewPerson(t, c, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	rc.mustPush(p, work, via, "main")
	ref := "refs/heads/seats/" + FixtureAlice + "/f42"

	// The premise: alice may write the branch before the revision.
	granted, err := c.GitWriteThrough(rc.ctx, via, commitFiles(t, rc.id, seatAt(t, FixtureAlice), ref, "", head,
		"before the revision", put("f42.txt", "before\n")))
	if err != nil || granted.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the premise: alice's write before the revision = (%+v, %v), want it applied", granted, err)
	}

	restore := c.Revoke(t, FixtureAlice, behind)
	defer restore()
	w := commitFiles(t, rc.id, seatAt(t, FixtureAlice), ref, granted.Commit, "", "after the revision",
		put("f42.txt", "after\n"))
	res, err := c.GitWriteThrough(rc.ctx, via, w)
	if err != nil || res.Outcome != "" || res.Command.Reason != gitlet.RefusalNotPermitted {
		t.Errorf("alice's write once a revision revoked her, its committer behind it = (%+v, %v), want it refused "+
			"not_permitted with nothing landed", res, err)
	}
	if got := rc.rows().Refs[ref]; got != granted.Commit {
		t.Errorf("%s is at %s after the refused write, want it unmoved at %s", ref, got, granted.Commit)
	}

	next := p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	rc.mustPush(p, work, via, "main")
	if got := rc.rows().Refs["refs/heads/main"]; got != next {
		t.Errorf("main is at %s after a person's push beside the member behind, want %s", got, next)
	}
	restore()
	rc.converged()
}
