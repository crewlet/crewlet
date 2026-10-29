package code_test

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CHANGE'S LANDING AND THE HOME'S DUE LISTING, through the committer and the
// reader on a real embedded broker: the record the repository's home git node
// asks for a change whose head its target came to hold, and the rows it finds
// its change work in.

// landing is the home's landing of revision rev of change, its head head,
// found inside target at tip.
func landing(repo, change string, rev int64, head, target, tip string) code.ChangeProposal {
	return code.ChangeProposal{OpID: statelog.NewOpID(time.Now(), ""), Landed: &code.ChangeLanded{RepoID: repo,
		ChangeID: change, Revision: rev, HeadOID: head, TargetRef: target, TargetOID: tip, Node: "node-a"}}
}

// merged is how change's row says it merged: its state, how, and at which
// commit.
func (n *node) merged(change string) (state, via, at string) {
	n.t.Helper()
	if err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(n.t.Context(), `SELECT state, merged_via, merge_oid FROM code_changes WHERE id = ?`,
			change).Scan(&state, &via, &at)
	}); err != nil {
		n.t.Fatalf("read change %s: %v", change, err)
	}
	return state, via, at
}

// A LANDING IS DECIDED AGAINST THE TIP ITS HOME READ: the home's record of a
// change whose head its target came to hold — moved past it rather than onto
// it — merges the change by `push` at that tip, decided by the committer
// against the change's head revision and head, its target branch and the
// target's tip as the home read them: a head moved since, a retarget, or the
// target moved on are each refused stale, appending nothing; a closed change
// is landed by nobody; a merged one is answered unchanged. The home's own
// write names no asker and no version, and a committer holding no chart
// decides it all the same.
//
// Mutation: drop the tip's check, and a landing read against a tip the
// target has moved from merges the change at a commit its target no longer
// holds.
func TestALandingIsDecidedAgainstTheTipItsHomeRead(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	chartless, _ := committerOn(t, n, n.pub, func(o *code.CommitterOptions) {
		o.SeatRules = func() (code.SeatRules, bool) { return code.SeatRules{}, false }
	})
	ctx := t.Context()
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, oidOf("A"), oidOf("M"))))

	for _, tc := range []struct {
		p    code.ChangeProposal
		says string
	}{
		// A REVISION THE CHANGE HAS MOVED PAST, and a head it does not have.
		{landing(repo, change, 2, oidOf("B"), mainRef, oidOf("M")), "is at revision 1"},
		{landing(repo, change, 1, oidOf("B2"), mainRef, oidOf("M")), "is at revision 1"},
		// A BRANCH IT DOES NOT MERGE INTO, and a tip its target has moved
		// from.
		{landing(repo, change, 1, oidOf("B"), releaseRef, oidOf("R")), "merges into"},
		{landing(repo, change, 1, oidOf("B"), mainRef, oidOf("A")), "the home reads the target again"},
	} {
		before := n.end()
		res, err := chartless.ProposeChange(ctx, tc.p)
		requireRefused(t, n, before, res, err, code.ChangeRefusalStale, tc.says)
	}

	invalid := landing(repo, change, 1, oidOf("B"), mainRef, oidOf("M"))
	invalid.Asker = dev
	if _, err := chartless.ProposeChange(ctx, invalid); !errors.Is(err, code.ErrInvalidProposal) ||
		!strings.Contains(err.Error(), "asker") {
		t.Fatalf("a landing naming an asker answered %v, want ErrInvalidProposal naming the asker", err)
	}
	versioned := landing(repo, change, 1, oidOf("B"), mainRef, oidOf("M"))
	versioned.ExpectedVersion = 1
	if _, err := chartless.ProposeChange(ctx, versioned); !errors.Is(err, code.ErrInvalidProposal) ||
		!strings.Contains(err.Error(), "expected_version") {
		t.Fatalf("a landing naming a version answered %v, want ErrInvalidProposal naming it", err)
	}

	res, err := chartless.ProposeChange(ctx, landing(repo, change, 1, oidOf("B"), mainRef, oidOf("M")))
	if err != nil || res.Outcome != statelog.OutcomeApplied || res.Unchanged || res.Position.IsZero() {
		t.Fatalf("the landing answered %+v, %v; want applied at a position", res, err)
	}
	if state, via, at := n.merged(change); state != string(code.ChangeStateMerged) ||
		via != string(code.MergedViaPush) || at != oidOf("M") {
		t.Fatalf("the landed change is %s via %q at %s, want merged by push at M", state, via, at)
	}
	before := n.end()
	again, err := chartless.ProposeChange(ctx, landing(repo, change, 1, oidOf("B"), mainRef, oidOf("M")))
	if err != nil || !again.Unchanged || n.end() != before {
		t.Fatalf("a landing of a merged change answered %+v, %v; want unchanged, nothing appended", again, err)
	}

	c, _ := committerOn(t, n, n.pub)
	opened, err := c.ProposeChange(ctx, opening(repo, releaseRef, mainRef, dev))
	requireChanged(t, opened, err)
	closed, err := c.ProposeChange(ctx, closing(repo, opened.ChangeID, dev, opened.Version))
	requireChanged(t, closed, err)
	before = n.end()
	res, err = chartless.ProposeChange(ctx, landing(repo, opened.ChangeID, 1, oidOf("R"), mainRef, oidOf("M")))
	requireRefused(t, n, before, res, err, code.ChangeRefusalChangeClosed, "closed")
}

// dueChanges is every due change of repo the reader lists, every page, and the
// path owners beside them.
func (n *node) dueChanges(repo string) ([]code.ChangeDue, []code.PathOwner) {
	n.t.Helper()
	q := code.ReadQuery{Kind: code.ReadChangesDue, ChangesDue: &code.ChangesDueQuery{RepoID: repo},
		Fresh: statelog.Freshness{Level: statelog.ReadStale}}
	var out []code.ChangeDue
	var owners []code.PathOwner
	for {
		a, err := n.reader.Read(n.t.Context(), q)
		if err != nil {
			n.t.Fatalf("read %s's due changes: %v", repo, err)
		}
		out, owners = append(out, a.DueChanges...), a.PathOwners
		if a.Next == "" {
			return out, owners
		}
		q.Cursor = a.Next
	}
}

// dueRepos is every repository the reader lists change work due in.
func (n *node) dueRepos() []string {
	n.t.Helper()
	q := code.ReadQuery{Kind: code.ReadChangesDue, Fresh: statelog.Freshness{Level: statelog.ReadStale}}
	var out []string
	for {
		a, err := n.reader.Read(n.t.Context(), q)
		if err != nil {
			n.t.Fatalf("read the due repositories: %v", err)
		}
		out = append(out, a.DueRepos...)
		if a.Next == "" {
			return out
		}
		q.Cursor = a.Next
	}
}

// THE DUE LISTING IS THE ROWS: a live change whose head revision holds no facts
// against the branch it merges into is due — on the listing of repositories by
// its repository, and on its repository's listing with its target's tip and
// the position that moved it there, beside the repository's path owners — and
// its facts recorded take it off; a retarget puts it back; a change whose
// target branch is gone is due nothing, and neither is one of an archived
// repository. The engine's own read names no asker, and only it names a due
// repository.
//
// Mutation: take facts of a change's head revision against another branch for
// its facts, and the retargeted change drops off the listing.
func TestTheDueListingIsTheRows(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	owners := []code.PathOwner{pathOwner(code.OwnerUnit, "Platform", "upload/**")}
	repo, change := ownedChange(t, n, "web", owners...)
	w, _ := writerOn(t, n, n.pub)
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()

	if got := n.dueRepos(); !slices.Equal(got, []string{repo}) {
		t.Fatalf("the due repositories are %v, want %s", got, repo)
	}
	due, gotOwners := n.dueChanges(repo)
	tip, at := n.tip(repo, mainRef)
	want := code.ChangeDue{ID: change, Number: 1, Revision: 1, Head: oidOf("B"), SourceRef: featureRef,
		TargetRef: mainRef, Tip: tip, TipPosition: at}
	if len(due) != 1 || due[0] != want || len(gotOwners) != 1 || gotOwners[0].Owner != owners[0].Owner {
		t.Fatalf("the repository's due changes are %+v beside %+v, want %+v beside its path owner", due, gotOwners,
			want)
	}

	res, err := w.Converse(ctx, n.revising(repo, change, 1, oidOf("B"), mainRef, owners, "upload/retry.go"))
	requireConversed(t, res, err)
	if got, _ := n.dueChanges(repo); len(got) != 0 {
		t.Fatalf("a change whose head revision holds its facts is still due: %+v", got)
	}
	if got := n.dueRepos(); len(got) != 0 {
		t.Fatalf("the due repositories are %v once their changes hold their facts, want none", got)
	}

	moved, err := c.ProposeChange(ctx, writing(dev, n.change(change).version, func(p *code.ChangeProposal) {
		p.Retarget = &code.ChangeRetarget{RepoID: repo, ChangeID: change, TargetRef: releaseRef}
	}))
	requireChanged(t, moved, err)
	if got, _ := n.dueChanges(repo); len(got) != 1 || got[0].TargetRef != releaseRef || got[0].Tip != oidOf("R") {
		t.Fatalf("the retargeted change is due as %+v, want it due against release at R", got)
	}

	s := suiteSettings()
	s.Owner = code.Owner{Kind: code.OwnerUnit, Ref: "Engineering"}
	s.PathOwners = code.CanonicalPathOwners(owners)
	s.Archived = true
	n.ensure("web", s, t2)
	if got, _ := n.dueChanges(repo); len(got) != 0 || len(n.dueRepos()) != 0 {
		t.Fatalf("an archived repository's change is due as %+v, want nothing", got)
	}
	s.Archived = false
	n.ensure("web", s, t3)
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(releaseRef, oidOf("R"), code.ZeroOID)))
	if got, _ := n.dueChanges(repo); len(got) != 0 {
		t.Fatalf("a change whose target branch is gone is due as %+v, want nothing", got)
	}

	if _, err := n.reader.Read(ctx, code.ReadQuery{Kind: code.ReadChangesDue, Asker: asker,
		Fresh: statelog.Freshness{Level: statelog.ReadStale}}); !errors.Is(err, code.ErrInvalid) ||
		!strings.Contains(err.Error(), "asker:") {
		t.Fatalf("a due listing naming an asker read %v, want ErrInvalid naming the asker", err)
	}
	if _, err := n.reader.Read(ctx, code.ReadQuery{Kind: code.ReadChanges, Asker: asker, Repo: "web",
		ChangesDue: &code.ChangesDueQuery{RepoID: repo},
		Fresh:      statelog.Freshness{Level: statelog.ReadStale}}); !errors.Is(err, code.ErrInvalid) ||
		!strings.Contains(err.Error(), "changes_due:") {
		t.Fatalf("a listing of changes naming a due repository read %v, want ErrInvalid naming it", err)
	}
}
