package codetest

import (
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/rendezvous"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ENGINE WRITE'S FLEET CASES (ADR-0059, ADR-0062): what only more than one
// git node can make of a write asked through the estate's routing
// (code.git_write) — the member it is asked through, the home that builds it,
// the home that dies under it, a member behind the company's configuration.

// writesOf is every push row of rc's repository that names push id.
func (rc *repoCase) writesOf(id string) []code.PushRow {
	rc.t.Helper()
	var rows []code.PushRow
	for _, row := range rc.pushRows() {
		if row.PushID == id {
			rows = append(rows, row)
		}
	}
	return rows
}

// F39: EVERY MEMBER ROUTES AN ENGINE WRITE TO THE REPOSITORY'S HOME (ADR-0059).
// A seat's write asked through each member's own router — the home's among
// them, which asks its own git half in process at its place — is built by the
// repository's home and by no other member: the git node that built a write is
// the one its push row names (code.PushRow.Node), and every row names the
// home. On a cluster every member is a data node and a git node, so the home
// and the member a repository's pushes are committed by are one member, one
// rendezvous order over one set. A router that asked its own member first
// would warm a copy of the repository on every member a seat writes through,
// and build each write beside a copy cold until it synced.
func everyMemberRoutesAnEngineWriteToTheHome(t *testing.T, c Cluster) {
	rc := newRepoCase(t, c, "fleet-f39")
	home := rc.committer()
	p := NewPerson(t, c, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	rc.mustPush(p, work, rc.other(home), "main")

	ref := "refs/heads/seats/" + FixtureAlice + "/f39"
	// at is the branch's head once a write made it, and base its start until
	// then; asked is the member each write was asked through, by its push.
	at, base := "", head
	asked := map[string]string{}
	for i, via := range c.DataNodes() {
		w := commitFiles(t, rc.id, seatAt(t, FixtureAlice), ref, at, base, "asked through "+via,
			put(fmt.Sprintf("f39/%d.txt", i), via+"\n"))
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

// F40: AN ENGINE WRITE WHOSE HOME DIES AFTER ITS APPEND IS ONE PUSH (ADR-0062).
// The repository's home builds a seat's write and lands it — its proposal
// answered, its record durable — and dies before it answers: a CRASH at
// code.HostAfterPropose, never a Block (F21's rule), and the member KILLED,
// its broker member with it. The member the write was asked through hears
// nothing for its router's whole git write attempt, then asks the runner-up the
// same write — the same operation, so the same push — and the runner-up, its
// copy synced to the write's floor and the branch already at the write's own
// commit, finds that push's row and answers from it, nothing built: applied,
// under the write's own push id, ONE push row, built by the home. Every
// member's copy is then the rows, the restarted home's too.
//
// THE RUNNER-UP HOLDS A COPY BEFORE THE HOME DIES — the repository's first
// push went through it — because the walk that meets a silent home asks the
// runner-up unstamped (a silent node may be filling the copy it was asked to),
// and a node that holds none fills nothing for a write not stamped its walk's
// home (F27's rule): the case is the push's, not the fill's.
func anEngineWriteWhoseHomeDiesAfterAppendIsOnePush(t *testing.T, c Cluster) {
	rc := newRepoCase(t, c, "fleet-f40")
	order := rendezvous.Order(rc.id, c.DataNodes())
	home, runnerUp, via := order[0], order[1], order[2]
	p := NewPerson(t, c, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	rc.mustPush(p, work, runnerUp, "main")
	if !memberNode(t, c, runnerUp).Cache().Holds(rc.id) {
		t.Fatalf("the premise: the runner-up %s holds no copy of a repository pushed through it", runnerUp)
	}

	w := commitFiles(t, rc.id, seatAt(t, FixtureAlice), "refs/heads/seats/"+FixtureAlice+"/f40", "", head,
		"built by the home", put("f40.txt", "f40\n"))
	pushID := code.WritePushID(w.OpID)
	reached := make(chan struct{})
	c.Faults().Arm(codefault.HostAfterPropose, codefault.Action{Once: true, Crash: true, Reached: reached})
	type answer struct {
		res code.EngineWriteResult
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

// F41: A REVOKED SEAT IS REFUSED ONCE THE LAGGING COMMITTER SHEDS
// (M3-decisions M3-O8). A revision takes alice out of the chart. The
// repository's committer — its home git node too, one rendezvous order over
// one set — fails to apply it: it keeps the chart that still holds her and is
// confirmed behind the company's configuration, while every other member
// applies it. alice's write, asked through another member, is refused
// not_permitted and nothing lands: the member behind judges no seat's grant
// from the chart the fleet replaced — its git half answers her write
// `behind_config`, which hands the walk to the runner-up — and the runner-up's
// chart has no alice. A person's push to the same repository, whose grant
// reads no chart, lands, its proposal decided by a current committer: a fleet
// with a member behind refuses what the revision revoked and nothing else.
//
// THE ARMS HOLD TOGETHER. Were the member behind to build alice's write — its
// git half's refusal dropped (host.Options.Current) — its proposal would still
// be asked of a current committer first (the soft arm, estate.Backend.Current),
// and its own committer would hand a seat's proposal on unjudged
// (code.CommitterOptions.Current); with all three dropped, the member behind
// builds the write by the chart it kept and lands it — red.
func aRevokedSeatIsRefusedOnceTheLaggingCommitterSheds(t *testing.T, c Cluster) {
	rc := newRepoCase(t, c, "fleet-f41")
	behind := rc.committer()
	via := rc.other(behind)
	p := NewPerson(t, c, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	rc.mustPush(p, work, via, "main")
	ref := "refs/heads/seats/" + FixtureAlice + "/f41"

	// THE PREMISE: alice may write the branch before the revision.
	granted, err := c.GitWriteThrough(rc.ctx, via, commitFiles(t, rc.id, seatAt(t, FixtureAlice), ref, "", head,
		"before the revision", put("f41.txt", "before\n")))
	if err != nil || granted.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the premise: alice's write before the revision = (%+v, %v), want it applied", granted, err)
	}

	restore := c.Revoke(t, FixtureAlice, behind)
	defer restore()
	w := commitFiles(t, rc.id, seatAt(t, FixtureAlice), ref, granted.Commit, "", "after the revision",
		put("f41.txt", "after\n"))
	res, err := c.GitWriteThrough(rc.ctx, via, w)
	if err != nil || res.Outcome != "" || res.Command.Reason != code.RefusalNotPermitted {
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
