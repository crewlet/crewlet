package codetest

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
)

// THE REF LIFECYCLE through a whole topology: a seat's branch nobody has set
// for code.RefExpiry is deleted by the code-retention pass's push on the
// engine's own account, every cache follows, and its push row is what restores
// it; and a branch set again after its listing is kept.

// F52: an expired branch is restored from its push row. A seat writes a
// branch in its namespace; a pass a fortnight and an hour on (its clock alone)
// deletes it through the committer; every cache at the head lacks it; the
// expiry's push row — the engine's own, via expiry — keeps the commit it was
// at; a person's code_create_branch at that commit restores it, and their git
// fetches it at the old commit.
func anExpiredBranchIsRestoredFromItsPushRow(t *testing.T, newFleet NewFleet) {
	c, p, _, head := seeded(t, newFleet, Shape{}, map[string]string{"README.md": "one\n"})
	repo := c.Rows().Repo.ID
	branch := code.SeatNamespace(FixtureAlice) + "x"
	made := c.mustLand(commitFiles(t, repo, seatAt(t, FixtureAlice), branch, "", head,
		"work in alice's namespace", put("notes.md", "a note\n")))

	report := c.Fleet.Retain(t, time.Now().Add(code.RefExpiry+time.Hour))
	if report.RefsExpired != 1 || len(report.ExpiryFailed) != 0 || report.ExpiryUnread != "" {
		t.Fatalf("the pass = expired %d failed %v unread %q, want %s expired", report.RefsExpired,
			report.ExpiryFailed, report.ExpiryUnread, branch)
	}
	if got, live := c.Rows().Refs[branch]; live {
		t.Fatalf("%s is still at %s after its expiry", branch, got)
	}
	assertConverged(t, c.Fleet)

	var old string
	for _, row := range pushRows(t, c) {
		if row.Via != code.ViaExpiry {
			continue
		}
		if row.ActorKind != string(code.PrincipalSystem) || row.Actor != "" || row.ActorSeat != "" {
			t.Errorf("the expiry's push row names %s %q seat %q, want the engine's own account", row.ActorKind,
				row.Actor, row.ActorSeat)
		}
		for _, cmd := range row.Commands {
			if cmd.Ref == branch && cmd.Result == code.ResultOK {
				old = cmd.Old
			}
		}
	}
	if old != made.Commit {
		t.Fatalf("the expiry's push row keeps %q for %s, want the commit it was at, %s", old, branch, made.Commit)
	}

	c.mustLand(code.EngineWrite{Kind: code.WriteCreateBranch, RepoID: repo, Principal: personAt("ceo"),
		OpID: newOp(t), Fresh: Linearizable, Ref: branch, Commit: old})
	clone := p.Clone(c.URL(0))
	p.Git().Run(t, clone, "checkout", "-q", strings.TrimPrefix(branch, "refs/heads/"))
	if got := p.Head(clone); got != made.Commit {
		t.Errorf("the restored %s checks out at %s, want %s", branch, got, made.Commit)
	}
	assertConverged(t, c.Fleet)
}

// F53: an expiry keeps what was pushed meanwhile. The listing is read with
// the branch at one commit; the seat writes it on; the expiry formed from the
// listing is then proposed: its command is stale, nothing is recorded, and the
// branch is at the seat's new commit on every cache.
func anExpiryKeepsWhatWasPushedMeanwhile(t *testing.T, newFleet NewFleet) {
	c, _, _, head := seeded(t, newFleet, Shape{}, map[string]string{"README.md": "one\n"})
	repo := c.Rows().Repo.ID
	branch := code.SeatNamespace(FixtureAlice) + "x"
	first := c.mustLand(commitFiles(t, repo, seatAt(t, FixtureAlice), branch, "", head, "first",
		put("notes.md", "a note\n")))
	// THE LISTING: the branch at first, at this position.
	seen := c.Rows().Position
	second := c.mustLand(commitFiles(t, repo, seatAt(t, FixtureAlice), branch, first.Commit, "", "second",
		put("more.md", "more\n")))
	res, err := c.Fleet.Propose(c.Ctx, code.Proposal{PushID: newOp(t), RepoID: repo,
		Principal: code.Principal{Kind: code.PrincipalSystem}, Via: code.ViaExpiry, Node: "codetest-retention",
		Commands:   []code.Command{{Ref: branch, Old: first.Commit, New: code.ZeroOID}},
		ReceivedAt: time.Now().Add(code.RefExpiry + time.Hour), Seen: seen})
	if err != nil || res.Outcome != "" || len(res.Answer.Commands) != 1 ||
		res.Answer.Commands[0].Reason != code.RefusalStale {
		t.Fatalf("the expiry formed before the seat's write = %+v, %v; want its command stale and no record", res,
			err)
	}
	if got := c.Rows().Refs[branch]; got != second.Commit {
		t.Errorf("%s is at %s, want the seat's later commit %s", branch, got, second.Commit)
	}
	assertConverged(t, c.Fleet)
}
