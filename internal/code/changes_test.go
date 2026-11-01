package code_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
)

// A CHANGE'S LIFECYCLE THROUGH THE COMMITTER: every write proposed to the
// repository's committer, decided in one snapshot on a real embedded broker,
// applied by the shipped applier and answered from the change's row.

// The principals a change case asks as, on the grant tests' chart (testChart)
// and a repository the unit Engineering owns ([changeRepo]): dev writes it,
// cto — Engineering's lead — maintains it, plat-lead reviews it, vp-sales and
// reader read it; ada is a person, and the token bound to nobody reads only.
var (
	dev      = agent("dev")
	cto      = agent("cto")
	platLead = agent("plat-lead")
	vpSales  = agent("vp-sales")
	reader   = agent("reader")
	ada      = code.Principal{Kind: code.PrincipalOperator, OperatorID: "pat:ada", Seat: "founder"}
	unbound  = code.Principal{Kind: code.PrincipalOperator, OperatorID: "token:ci"}
)

// The branches a change case's repository starts with.
const (
	mainRef    = "refs/heads/main"
	featureRef = "refs/heads/feature"
	releaseRef = "refs/heads/release"
)

// changeRepo is a fresh repository named name, owned by the unit Engineering,
// with main at A, feature at B and release at R.
func changeRepo(t testing.TB, n *node, name string) string {
	t.Helper()
	s := suiteSettings()
	s.Owner = code.Owner{Kind: code.OwnerUnit, Ref: "Engineering"}
	// MAIN ASKS ONE COUNTED APPROVAL, and nothing else of a merge: the gate a
	// change's readiness in these cases is judged under. A branch whose gate
	// asks none merges on none (code.JudgeGate), and the readiness cases are
	// about whose approval counts.
	s.Protections[0].Gate = &code.MergeGate{MergeBy: code.MergeByWriters, Approvals: code.ApprovalsOne,
		Landing: code.Landing{Policy: code.LandingPreLand, Depth: 4, Batch: 4}, DeleteSource: code.DeleteSeats}
	repo := n.ensure(name, s, t1).RepoID
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, code.ZeroOID, oidOf("A")),
		set(featureRef, code.ZeroOID, oidOf("B")), set(releaseRef, code.ZeroOID, oidOf("R"))))
	return repo
}

// opening is an open of source into target on repo, asked by asker under an
// operation id of its own.
func opening(repo, source, target string, asker code.Principal) code.ChangeProposal {
	opID := statelog.NewOpID(time.Now(), "code_open_change")
	return code.ChangeProposal{OpID: opID, Asker: asker, Open: &code.ChangeOpen{RepoID: repo,
		ChangeID: code.OpenChangeID(opID), SourceRef: source, TargetRef: target, Title: "Retry the upload"}}
}

// writing is a write of a change's lifecycle by asker at version, under an
// operation id of its own: set fills in the one record it carries.
func writing(asker code.Principal, version int64, set func(p *code.ChangeProposal)) code.ChangeProposal {
	p := code.ChangeProposal{OpID: statelog.NewOpID(time.Now(), "code_update_change"), Asker: asker,
		ExpectedVersion: version}
	set(&p)
	return p
}

// retitled is an edit of change on repo setting its title.
func retitled(repo, change string, asker code.Principal, version int64, title string) code.ChangeProposal {
	return writing(asker, version, func(p *code.ChangeProposal) {
		p.Edit = &code.ChangeEdit{RepoID: repo, ChangeID: change, Title: &title}
	})
}

// closing is a close of change on repo.
func closing(repo, change string, asker code.Principal, version int64) code.ChangeProposal {
	return writing(asker, version, func(p *code.ChangeProposal) {
		p.Close = &code.ChangeClose{RepoID: repo, ChangeID: change, Reason: "not needed"}
	})
}

// reopening is a reopen of change on repo.
func reopening(repo, change string, asker code.Principal, version int64) code.ChangeProposal {
	return writing(asker, version, func(p *code.ChangeProposal) {
		p.Reopen = &code.ChangeReopen{RepoID: repo, ChangeID: change}
	})
}

// requireChanged fails t unless res is a write applied at a position, with a
// version.
func requireChanged(t *testing.T, res code.ChangeResult, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("ProposeChange: %v", err)
	}
	if res.Outcome != statelog.OutcomeApplied || res.Position.IsZero() || res.Refusal != "" || res.Unchanged ||
		res.Version < 1 || res.Number < 1 {
		t.Fatalf("the change answered %+v, want applied at a position with its number and version", res)
	}
}

// requireRefused fails t unless res is refused under code with a sentence
// holding says, and nothing was appended to n's log since before.
func requireRefused(t *testing.T, n *node, before uint64, res code.ChangeResult, err error,
	refusal code.ChangeRefusal, says string) {

	t.Helper()
	if err != nil {
		t.Fatalf("ProposeChange: %v", err)
	}
	if res.Refusal != refusal || res.Outcome != "" || !strings.Contains(res.Detail, says) {
		t.Fatalf("the change answered %+v, want %s saying %q", res, refusal, says)
	}
	if got := n.end(); got != before {
		t.Fatalf("a refused write appended %d records, want none", got-before)
	}
}

// storedChange is a change as its rows hold it: what a case reads back.
type storedChange struct {
	number, revision, version, bodyBytes, position int64
	state, source, target, head, title, owner      string
	ownerKind, operator, closeReason               string
	work                                           types.WorkItem
	turn, run                                      string
	closed                                         bool
}

// change is change id as n's rows hold it.
func (n *node) change(id string) storedChange {
	n.t.Helper()
	var c storedChange
	var backend string
	var closed sql.NullInt64
	if err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(n.t.Context(), `SELECT number, revision, version, body_bytes, position, state,
			source_ref, target_ref, head_oid, title, owner, owner_kind, owner_operator_id, close_reason,
			work_backend, work_id, work_key, work_project, created_turn, created_run, closed_at
			FROM code_changes WHERE id = ?`, id).Scan(&c.number, &c.revision, &c.version, &c.bodyBytes,
			&c.position, &c.state, &c.source, &c.target, &c.head, &c.title, &c.owner, &c.ownerKind, &c.operator,
			&c.closeReason, &backend, &c.work.ID, &c.work.Key, &c.work.Project, &c.turn, &c.run, &closed)
	}); err != nil {
		n.t.Fatalf("read change %s: %v", id, err)
	}
	c.work.Backend, c.closed = types.WorkBackend(backend), closed.Valid
	return c
}

// ONE OPEN CHANGE A HEAD: a branch heads one live change at a time, so a second
// open from it is refused change_exists naming the first, appending nothing;
// once the first is closed the branch opens another; and a closed change is
// reopened only while no other live change heads its branch — the invariant a
// push's revisions rest on, that a source names one change.
//
// Mutation: skip the decide's probe of the live change on the source, and the
// second open lands beside the first.
func TestOneOpenChangePerHead(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()
	repo := changeRepo(t, n, "web")

	first, err := c.ProposeChange(ctx, opening(repo, featureRef, mainRef, dev))
	requireChanged(t, first, err)
	if first.Number != 1 {
		t.Fatalf("the first change is web!%d, want web!1", first.Number)
	}
	before := n.end()
	second, err := c.ProposeChange(ctx, opening(repo, featureRef, releaseRef, ada))
	requireRefused(t, n, before, second, err, code.ChangeRefusalChangeExists, "refs/heads/feature already heads web!1")

	closed, err := c.ProposeChange(ctx, closing(repo, first.ChangeID, dev, first.Version))
	requireChanged(t, closed, err)
	third, err := c.ProposeChange(ctx, opening(repo, featureRef, mainRef, ada))
	requireChanged(t, third, err)
	if third.Number != 2 {
		t.Fatalf("the change opened once web!1 was closed is web!%d, want web!2", third.Number)
	}
	before = n.end()
	reopened, err := c.ProposeChange(ctx, reopening(repo, first.ChangeID, dev, closed.Version))
	requireRefused(t, n, before, reopened, err, code.ChangeRefusalChangeExists, "refs/heads/feature heads web!2")
	if got := n.count(`SELECT COUNT(*) FROM code_changes WHERE repo_id = ? AND source_ref = ?
		AND state IN ('draft', 'open')`, repo, featureRef); got != 1 {
		t.Fatalf("%d live changes head refs/heads/feature, want one", got)
	}
}

// A CHANGE'S NUMBER IS THE LOG'S ORDER: changes opened at once, each from a
// branch of its own, take the repository's numbers from 1 in the order their
// records land — the apply gives a number, from next_change, which it
// advances, and no decide does — so none repeats and none is skipped; and an
// open asked again under its own operation id is the same change, its record
// collapsed by the ledger, appending nothing and taking no number.
//
// Mutation: number a change in the decide, from the row its snapshot read,
// and two opens decided from one snapshot share a number.
func TestAChangeNumberIsTheLogsOrder(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()
	repo := changeRepo(t, n, "web")
	const opens = 8
	var cmds []code.Command
	for i := range opens {
		cmds = append(cmds, set(branch(i), code.ZeroOID, oidOf("topic"+string(rune('a'+i)))))
	}
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), cmds...))

	proposals := make([]code.ChangeProposal, opens)
	answers := make([]code.ChangeResult, opens)
	errs := make([]error, opens)
	var wg sync.WaitGroup
	for i := range opens {
		proposals[i] = opening(repo, branch(i), mainRef, dev)
		wg.Add(1)
		go func() {
			defer wg.Done()
			answers[i], errs[i] = c.ProposeChange(ctx, proposals[i])
		}()
	}
	wg.Wait()
	for i := range opens {
		requireChanged(t, answers[i], errs[i])
	}
	byPosition := slices.Clone(answers)
	slices.SortFunc(byPosition, func(a, b code.ChangeResult) int {
		return int(a.Position.Packed() - b.Position.Packed())
	})
	for i, a := range byPosition {
		if a.Number != int64(i+1) {
			t.Fatalf("the change whose record landed %d of %d is web!%d, want web!%d: %+v", i+1, opens,
				a.Number, i+1, byPosition)
		}
	}
	if got := n.count(`SELECT next_change FROM code_repos WHERE id = ?`, repo); got != opens+1 {
		t.Fatalf("the repository numbers its next change %d, want %d", got, opens+1)
	}

	before := n.end()
	again, err := c.ProposeChange(ctx, proposals[0])
	requireChanged(t, again, err)
	if again.ChangeID != answers[0].ChangeID || again.Number != answers[0].Number ||
		again.Position != answers[0].Position || n.end() != before {
		t.Fatalf("the open asked again answered %+v and appended %d records; want web!%d at %s, nothing appended",
			again, n.end()-before, answers[0].Number, answers[0].Position)
	}
	if got := n.count(`SELECT next_change FROM code_repos WHERE id = ?`, repo); got != opens+1 {
		t.Fatalf("an open asked again moved the next number to %d", got)
	}
}

// branch is the i-th topic branch a case opens a change from.
func branch(i int) string { return "refs/heads/topic-" + string(rune('a'+i)) }

// A STALE VERSION LOSES TO THE EDIT THAT MOVED IT: two edits formed against one
// version of a change — one lands, and moves the change's version; the other
// is refused stale naming both versions, appending nothing, and the change
// keeps what the first wrote. An edit that asks for what the change already
// says is answered unchanged from its row, at its version, with no record.
//
// Mutation: drop the decide's comparison of the versions, and the second edit
// lands over the first.
func TestAStaleVersionLosesToTheEditThatMovedIt(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()
	repo := changeRepo(t, n, "web")
	opened, err := c.ProposeChange(ctx, opening(repo, featureRef, mainRef, dev))
	requireChanged(t, opened, err)
	if opened.Version != 1 {
		t.Fatalf("an opened change is at version %d, want 1", opened.Version)
	}

	edited, err := c.ProposeChange(ctx, retitled(repo, opened.ChangeID, dev, 1, "Retry the upload twice"))
	requireChanged(t, edited, err)
	if edited.Version != 2 {
		t.Fatalf("an edit left the change at version %d, want 2", edited.Version)
	}
	before := n.end()
	lost, err := c.ProposeChange(ctx, retitled(repo, opened.ChangeID, cto, 1, "Retry the upload thrice"))
	requireRefused(t, n, before, lost, err, code.ChangeRefusalStale,
		"web!1 is at version 2, and this write was formed against version 1")
	if got := n.change(opened.ChangeID); got.title != "Retry the upload twice" || got.version != 2 {
		t.Fatalf("the change is %q at version %d, want the first edit's title at version 2", got.title, got.version)
	}

	same, err := c.ProposeChange(ctx, retitled(repo, opened.ChangeID, cto, 2, "Retry the upload twice"))
	if err != nil || !same.Unchanged || same.Outcome != statelog.OutcomeApplied || same.Version != 2 ||
		same.Number != 1 || same.Position.Packed() != n.change(opened.ChangeID).position || n.end() != before {
		t.Fatalf("an edit to the title the change has answered %+v, %v and appended %d records; want unchanged "+
			"at version 2 and the change's own position, nothing appended", same, err, n.end()-before)
	}
}

// ONLY THE OWNER, A MAINTAINER OR A PERSON EDITS A CHANGE: once a change is
// open, its owner, a principal that maintains its repository and any person
// may write its lifecycle, and nobody else — a seat that reviews or reads the
// repository, and a token bound to nobody, are refused not_permitted, told
// what they hold and who may. And an open is anyone's who may push its source
// branch: a seat that only reads the repository opens none.
//
// Mutation: let a seat at review write any change, and plat-lead's edit
// lands; let the owner check compare handles alone, and a person holding a
// seat's handle would pass for it.
func TestOnlyTheOwnerAMaintainerOrAPersonEditsAChange(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()
	repo := changeRepo(t, n, "web")
	before := n.end()
	res, err := c.ProposeChange(ctx, opening(repo, featureRef, mainRef, reader))
	requireRefused(t, n, before, res, err, code.ChangeRefusalNotPermitted,
		"whoever may push its source branch, and this seat holds read on this repository")

	opened, err := c.ProposeChange(ctx, opening(repo, featureRef, mainRef, dev))
	requireChanged(t, opened, err)
	version := opened.Version
	for _, tc := range []struct {
		name  string
		asker code.Principal
		// refused is the sentence the write is refused with, "" for one
		// that lands.
		refused string
	}{
		{"its owner", dev, ""},
		{"a seat that reviews the repository", platLead, `seat "plat-lead" holds review on this repository`},
		{"a seat that reads the repository", vpSales, `seat "vp-sales" holds read on this repository`},
		{"a token bound to nobody", unbound, "a credential bound to nobody reads only"},
		{"the repository's maintainer", cto, ""},
		{"a person", ada, ""},
	} {
		before := n.end()
		res, err := c.ProposeChange(ctx, retitled(repo, opened.ChangeID, tc.asker, version, "by "+tc.name))
		if tc.refused != "" {
			requireRefused(t, n, before, res, err, code.ChangeRefusalNotPermitted, tc.refused)
			continue
		}
		requireChanged(t, res, err)
		version = res.Version
	}
	if got := n.change(opened.ChangeID); got.title != "by a person" || got.owner != "dev" || got.version != 4 {
		t.Fatalf("the change is %+v, want dev's, retitled by the person at version 4", got)
	}
}

// A SEAT'S WRITE OF A CHANGE IS DECIDED ONLY WITH A CHART: on a node that holds
// none yet the committer decides nothing of it and says so about itself
// (ErrNotServing), so the router asks the repository's runner-up; a person's
// is decided there as ever.
func TestASeatsChangeNeedsTheDecidingNodesChart(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	c, _ := committerOn(t, n, n.pub, func(o *code.CommitterOptions) {
		o.SeatRules = func() (code.SeatRules, bool) { return code.SeatRules{}, false }
	})
	ctx := t.Context()
	repo := changeRepo(t, n, "web")
	before := n.end()
	if res, err := c.ProposeChange(ctx, opening(repo, featureRef, mainRef, dev)); !errors.Is(err,
		code.ErrNotServing) || n.end() != before {
		t.Fatalf("a seat's open on a node with no chart answered %+v, %v; want ErrNotServing, nothing appended",
			res, err)
	}
	res, err := c.ProposeChange(ctx, opening(repo, featureRef, mainRef, ada))
	requireChanged(t, res, err)
}

// A CHANGE PROPOSAL IS DECIDED ALONE: queued between two pushes behind a batch
// in flight, an open is decided as a record of its own, after the push before
// it and before the push after it — a record carries one op, and the
// repository's proposals land in the order they arrived.
//
// Mutation: let a batch gather a change behind its pushes, and the three are
// decided as one push record.
func TestAChangeProposalIsDecidedAlone(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := changeRepo(t, n, "web")
	gate := newGate(n.pub)
	c, _ := committerOn(t, n, gate)
	lead := proposeAsync(t.Context(), c, proposal(repo, update("refs/heads/lead", code.ZeroOID, oidOf("L"))))
	eventually(t, "the first publish reaching the gate", func() bool { return gate.calls.Load() == 1 })
	before := n.end()

	ahead := proposeAsync(t.Context(), c, proposal(repo, update(mainRef, oidOf("A"), oidOf("A2"))))
	eventually(t, "the push ahead queued", func() bool { return code.CommitterQueued(c, repo) == 1 })
	opened := make(chan struct {
		res code.ChangeResult
		err error
	}, 1)
	go func() {
		res, err := c.ProposeChange(t.Context(), opening(repo, featureRef, releaseRef, ada))
		opened <- struct {
			res code.ChangeResult
			err error
		}{res, err}
	}()
	eventually(t, "the change queued", func() bool { return code.CommitterQueued(c, repo) == 2 })
	behind := proposeAsync(t.Context(), c, proposal(repo, update(mainRef, oidOf("A2"), oidOf("A3"))))
	eventually(t, "the push behind queued", func() bool { return code.CommitterQueued(c, repo) == 3 })
	gate.open()

	requireOK(t, await(t, lead))
	first, second := await(t, ahead), await(t, behind)
	requireOK(t, first)
	requireOK(t, second)
	var got struct {
		res code.ChangeResult
		err error
	}
	select {
	case got = <-opened:
	case <-time.After(30 * time.Second):
		t.Fatal("the change was not answered in 30 s")
	}
	requireChanged(t, got.res, got.err)
	positions := []uint64{first.res.Position.Seq, got.res.Position.Seq, second.res.Position.Seq}
	if !slices.IsSorted(positions) || positions[0] == positions[1] || positions[1] == positions[2] {
		t.Fatalf("the push ahead, the change and the push behind landed at %v, want three records in that "+
			"order", positions)
	}
	if appended := n.end() - before; appended != 4 {
		t.Fatalf("%d records appended, want 4: the lead's, then the push, the change and the push each alone",
			appended)
	}
}

// AN OPEN WRITES THE CHANGE'S ROWS FROM ITS RECORD: its row numbered and at
// version 1, its source's head as its first revision — made by its owner, by
// no push — its owner its first author, its description a row of its own, the
// work item, the turn and the run it was opened in, and a person's credential
// beside the human seat it names; and the repository's next number moved.
func TestAnOpenWritesTheChangesRows(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	c, _ := committerOn(t, n, n.pub)
	repo := changeRepo(t, n, "web")
	p := opening(repo, featureRef, mainRef, ada)
	p.Open.Body, p.Open.Draft = "It fails one run in ten.", true
	p.Open.WorkItem = &types.WorkItem{Backend: types.WorkWorklet, ID: "task-9", Key: "ENG-412", Project: "ENG"}
	p.Open.TurnID, p.Open.RunID = "turn-1", "run-1"
	res, err := c.ProposeChange(t.Context(), p)
	requireChanged(t, res, err)
	if res.ChangeID != p.Open.ChangeID || res.Version != 1 {
		t.Fatalf("the open answered %+v, want change %s at version 1", res, p.Open.ChangeID)
	}
	got := n.change(res.ChangeID)
	want := storedChange{number: 1, revision: 1, version: 1, bodyBytes: int64(len(p.Open.Body)),
		position: res.Position.Packed(), state: "draft", source: featureRef, target: mainRef, head: oidOf("B"),
		title: "Retry the upload", owner: "founder", ownerKind: "person", operator: "pat:ada",
		work: *p.Open.WorkItem, turn: "turn-1", run: "run-1"}
	if got != want {
		t.Fatalf("the change's row is\n\t%+v\nwant\n\t%+v", got, want)
	}
	if n.count(`SELECT COUNT(*) FROM code_change_bodies WHERE change_id = ? AND body = ?`, res.ChangeID,
		p.Open.Body) != 1 {
		t.Error("the change's description is not its body row")
	}
	if n.count(`SELECT COUNT(*) FROM code_change_authors WHERE change_id = ? AND author = 'founder'
		AND author_kind = 'person' AND first_revision = 1`, res.ChangeID) != 1 {
		t.Error("the change's owner is not its first author")
	}
	if n.count(`SELECT COUNT(*) FROM code_revisions WHERE change_id = ? AND n = 1 AND head_oid = ?
		AND pusher = 'founder' AND pusher_kind = 'person' AND push_id = '' AND position = ?`,
		res.ChangeID, oidOf("B"), res.Position.Packed()) != 1 {
		t.Error("the change's first revision is not its source's head, made by its owner and by no push")
	}
	if got := n.count(`SELECT next_change FROM code_repos WHERE id = ?`, repo); got != 2 {
		t.Errorf("the repository numbers its next change %d, want 2", got)
	}
}

// EVERY WRITE MOVES THE CHANGE'S ROW AS ITS RECORD SAYS, one version a record:
// an edit its title, its description — removed by an empty one — and its work
// item, cleared again; a draft's state; a retarget its target; a close its
// state, instant and reason; and a reopen opens it again — at its source's
// head, a revision of its own made by whoever reopened it where the branch
// moved while the change was closed.
func TestEveryLifecycleWriteMovesItsRow(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()
	repo := changeRepo(t, n, "web")
	p := opening(repo, featureRef, mainRef, dev)
	p.Open.Body = "first words"
	opened, err := c.ProposeChange(ctx, p)
	requireChanged(t, opened, err)
	id, version := opened.ChangeID, opened.Version
	item := types.WorkItem{Backend: types.WorkJira, ID: "10042", Key: "ENG-7", Project: "ENG"}
	empty := ""
	for _, tc := range []struct {
		name  string
		write func(p *code.ChangeProposal)
		check func(got storedChange) bool
	}{
		{"an edit of the work item", func(p *code.ChangeProposal) {
			p.Edit = &code.ChangeEdit{RepoID: repo, ChangeID: id, WorkItem: &item}
		}, func(got storedChange) bool { return got.work == item }},
		{"an edit removing the description", func(p *code.ChangeProposal) {
			p.Edit = &code.ChangeEdit{RepoID: repo, ChangeID: id, Body: &empty, ClearWorkItem: true}
		}, func(got storedChange) bool { return got.bodyBytes == 0 && got.work == types.WorkItem{} }},
		{"a draft", func(p *code.ChangeProposal) {
			p.Draft = &code.ChangeDraft{RepoID: repo, ChangeID: id, Draft: true}
		}, func(got storedChange) bool { return got.state == "draft" }},
		{"a retarget", func(p *code.ChangeProposal) {
			p.Retarget = &code.ChangeRetarget{RepoID: repo, ChangeID: id, TargetRef: releaseRef}
		}, func(got storedChange) bool { return got.target == releaseRef }},
		{"a close", func(p *code.ChangeProposal) {
			p.Close = &code.ChangeClose{RepoID: repo, ChangeID: id, Reason: "superseded"}
		}, func(got storedChange) bool {
			return got.state == "closed" && got.closed && got.closeReason == "superseded"
		}},
	} {
		res, err := c.ProposeChange(ctx, writing(dev, version, tc.write))
		requireChanged(t, res, err)
		if res.Version != version+1 {
			t.Fatalf("%s left the change at version %d, want %d", tc.name, res.Version, version+1)
		}
		version = res.Version
		if got := n.change(id); !tc.check(got) || got.position != res.Position.Packed() {
			t.Fatalf("after %s the change is %+v", tc.name, got)
		}
	}
	if n.count(`SELECT COUNT(*) FROM code_change_bodies WHERE change_id = ?`, id) != 0 {
		t.Error("an emptied description left its body row")
	}

	// THE SOURCE MOVES WHILE THE CHANGE IS CLOSED, and its reopen is the
	// branch's head now: revision 2, made by whoever reopened it.
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(featureRef, oidOf("B"), oidOf("C"))))
	reopened, err := c.ProposeChange(ctx, reopening(repo, id, cto, version))
	requireChanged(t, reopened, err)
	got := n.change(id)
	if got.state != "open" || got.closed || got.closeReason != "" || got.head != oidOf("C") || got.revision != 2 {
		t.Fatalf("the reopened change is %+v, want it open at refs/heads/feature's head, revision 2", got)
	}
	if n.count(`SELECT COUNT(*) FROM code_revisions WHERE change_id = ? AND n = 2 AND head_oid = ? AND pusher = 'cto'
		AND pusher_kind = 'agent' AND push_id = ''`, id, oidOf("C")) != 1 {
		t.Error("the reopen's revision is not the source's head, made by whoever reopened it")
	}
}

// A WRITE IS REFUSED FOR WHAT ITS REPOSITORY, ITS BRANCHES AND ITS CHANGE ARE,
// each in a sentence of its own, appending nothing: an open from a branch to
// itself, from a branch or to one that is not live, or from one at its
// target's commit; a write naming a change the repository does not hold; an
// edit of a closed change; a retarget onto its source or onto no branch; a
// reopen whose branch is gone; and any write to an archived repository or a
// deleted one.
func TestAChangeIsRefusedForWhatItsRepositoryAndBranchesAre(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()
	repo := changeRepo(t, n, "web")
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set("refs/heads/twin", code.ZeroOID, oidOf("A"))))
	opened, err := c.ProposeChange(ctx, opening(repo, featureRef, mainRef, dev))
	requireChanged(t, opened, err)
	id := opened.ChangeID
	closed, err := c.ProposeChange(ctx, closing(repo, id, dev, opened.Version))
	requireChanged(t, closed, err)
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(featureRef, oidOf("B"), code.ZeroOID)))

	for _, tc := range []struct {
		name    string
		p       code.ChangeProposal
		refusal code.ChangeRefusal
		says    string
	}{
		{"an open from a branch to itself", opening(repo, mainRef, mainRef, dev), code.ChangeRefusalSameRef,
			"refs/heads/main is both the change's source and its target"},
		{"an open from no branch", opening(repo, "refs/heads/nowhere", mainRef, dev), code.ChangeRefusalNoSource,
			"web holds no live refs/heads/nowhere: push the branch first"},
		{"an open into no branch", opening(repo, releaseRef, "refs/heads/nowhere", dev), code.ChangeRefusalNoTarget,
			"web holds no live refs/heads/nowhere to merge into"},
		{"an open from its target's commit", opening(repo, "refs/heads/twin", mainRef, dev),
			code.ChangeRefusalNoDifference, "the source holds nothing its target does not"},
		{"a write of no change", closing(repo, uuid.Must(uuid.NewV7()).String(), ada, 1),
			code.ChangeRefusalChangeNotFound, "repository web holds no change"},
		{"an edit of a closed change", retitled(repo, id, dev, closed.Version, "again"),
			code.ChangeRefusalChangeClosed, "web!1 is closed: reopen it first"},
		{"a reopen whose branch is gone", reopening(repo, id, dev, closed.Version), code.ChangeRefusalNoSource,
			"web holds no live refs/heads/feature, web!1's source"},
	} {
		before := n.end()
		res, err := c.ProposeChange(ctx, tc.p)
		requireRefused(t, n, before, res, err, tc.refusal, tc.says)
	}

	live, err := c.ProposeChange(ctx, opening(repo, releaseRef, mainRef, dev))
	requireChanged(t, live, err)
	for _, tc := range []struct {
		name, target string
		refusal      code.ChangeRefusal
		says         string
	}{
		{"onto its source", releaseRef, code.ChangeRefusalSameRef, "refs/heads/release is web!2's source"},
		{"onto no branch", "refs/heads/nowhere", code.ChangeRefusalNoTarget, "web holds no live refs/heads/nowhere"},
	} {
		before := n.end()
		res, err := c.ProposeChange(ctx, writing(dev, live.Version, func(p *code.ChangeProposal) {
			p.Retarget = &code.ChangeRetarget{RepoID: repo, ChangeID: live.ChangeID, TargetRef: tc.target}
		}))
		requireRefused(t, n, before, res, err, tc.refusal, tc.says)
	}

	s := suiteSettings()
	s.Owner, s.Archived = code.Owner{Kind: code.OwnerUnit, Ref: "Engineering"}, true
	n.ensure("web", s, t2)
	before := n.end()
	res, err := c.ProposeChange(ctx, retitled(repo, live.ChangeID, ada, live.Version, "archived"))
	requireRefused(t, n, before, res, err, code.ChangeRefusalRepoArchived, "repository web is archived and read-only")
	if _, err := n.store.DeleteRepo(ctx, n.activate(t3), repo); err != nil {
		t.Fatal(err)
	}
	n.drain()
	before = n.end()
	res, err = c.ProposeChange(ctx, closing(repo, live.ChangeID, ada, live.Version))
	requireRefused(t, n, before, res, err, code.ChangeRefusalRepoDeleted, "repository web is deleted")
}

// A CHANGE PROPOSAL THAT IS NONE IS REFUSED BEFORE IT IS QUEUED, naming the
// field: its asker's own bug, never a refusal its committer records.
func TestAChangeProposalThatIsNoneIsRefusedBeforeItIsQueued(t *testing.T) {
	t.Parallel()
	const repo = "0190f3c4-0000-7000-8000-00000000c0de"
	long := strings.Repeat("x", code.MaxChangeTitleBytes+1)
	for name, tc := range map[string]struct {
		p     func() code.ChangeProposal
		field string
	}{
		"none": {func() code.ChangeProposal { return code.ChangeProposal{OpID: "x"} }, "exactly one"},
		"two": {func() code.ChangeProposal {
			p := opening(repo, featureRef, mainRef, dev)
			p.Close = &code.ChangeClose{RepoID: repo, ChangeID: p.Open.ChangeID}
			return p
		}, "exactly one"},
		"an op id no engine minted": {func() code.ChangeProposal {
			p := opening(repo, featureRef, mainRef, dev)
			p.OpID = "mine"
			return p
		}, "op_id"},
		"an asker naming a run": {func() code.ChangeProposal {
			p := opening(repo, featureRef, mainRef, dev)
			p.Asker.Run = code.RunProvenance{TurnID: "turn-1"}
			return p
		}, "asker.run"},
		"an open's change id not derived from its op": {func() code.ChangeProposal {
			p := opening(repo, featureRef, mainRef, dev)
			p.Open.ChangeID = uuid.Must(uuid.NewV7()).String()
			return p
		}, "open.change_id"},
		"an open naming a version": {func() code.ChangeProposal {
			p := opening(repo, featureRef, mainRef, dev)
			p.ExpectedVersion = 1
			return p
		}, "expected_version"},
		"a write naming none": {func() code.ChangeProposal {
			return closing(repo, uuid.Must(uuid.NewV7()).String(), dev, 0)
		}, "expected_version"},
		"a tag as the source": {func() code.ChangeProposal {
			return opening(repo, "refs/tags/v1", mainRef, dev)
		}, "open.source_ref"},
		"a title past its bound": {func() code.ChangeProposal {
			p := opening(repo, featureRef, mainRef, dev)
			p.Open.Title = long
			return p
		}, "open.title"},
		"a title of two lines": {func() code.ChangeProposal {
			p := opening(repo, featureRef, mainRef, dev)
			p.Open.Title = "one\ntwo"
			return p
		}, "open.title"},
		"a work item of no tracker": {func() code.ChangeProposal {
			p := opening(repo, featureRef, mainRef, dev)
			p.Open.WorkItem = &types.WorkItem{Backend: "trello", ID: "1"}
			return p
		}, "open.work_item"},
		"an open's head sent by its asker": {func() code.ChangeProposal {
			p := opening(repo, featureRef, mainRef, dev)
			p.Open.Head = oidOf("B")
			return p
		}, "open.head"},
		"an open's owner sent by its asker": {func() code.ChangeProposal {
			p := opening(repo, featureRef, mainRef, dev)
			p.Open.Owner = cto
			return p
		}, "open.owner"},
		"an edit that sets nothing": {func() code.ChangeProposal {
			return writing(dev, 1, func(p *code.ChangeProposal) {
				p.Edit = &code.ChangeEdit{RepoID: repo, ChangeID: uuid.Must(uuid.NewV7()).String()}
			})
		}, "edit"},
		"an edit that sets a work item and clears it": {func() code.ChangeProposal {
			return writing(dev, 1, func(p *code.ChangeProposal) {
				p.Edit = &code.ChangeEdit{RepoID: repo, ChangeID: uuid.Must(uuid.NewV7()).String(),
					WorkItem: &types.WorkItem{Backend: types.WorkWorklet, ID: "t"}, ClearWorkItem: true}
			})
		}, "edit.clear_work_item"},
		"a close's writer sent by its asker": {func() code.ChangeProposal {
			p := closing(repo, uuid.Must(uuid.NewV7()).String(), dev, 1)
			p.Close.By = dev
			return p
		}, "close.by"},
		"a close's reason past its bound": {func() code.ChangeProposal {
			p := closing(repo, uuid.Must(uuid.NewV7()).String(), dev, 1)
			p.Close.Reason = strings.Repeat("x", code.MaxCloseReasonBytes+1)
			return p
		}, "close.reason"},
		"a seen on another log": {func() code.ChangeProposal {
			p := opening(repo, featureRef, mainRef, dev)
			p.Seen = statelog.Position{Stream: "CREWLET_TRACKER_LOG", Generation: 1, Seq: 1}
			return p
		}, "seen"},
	} {
		err := tc.p().Validate()
		if !errors.Is(err, code.ErrInvalidProposal) || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: Validate = %v, want ErrInvalidProposal naming %s", name, err, tc.field)
		}
	}
	if err := opening(repo, featureRef, mainRef, dev).Validate(); err != nil {
		t.Errorf("an open as a tool forms one: %v", err)
	}
}

// EVERY ANSWER IS COUNTED UNDER ITS OP AND WHAT BECAME OF IT
// (crewlet.code.change.proposals), and a write the committer met busy under
// the busy count's `change` kind.
func TestEveryChangeAnswerIsCounted(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	c, rec := committerOn(t, n, n.pub)
	ctx := t.Context()
	repo := changeRepo(t, n, "web")
	opened, err := c.ProposeChange(ctx, opening(repo, featureRef, mainRef, dev))
	requireChanged(t, opened, err)
	if _, err := c.ProposeChange(ctx, opening(repo, featureRef, mainRef, dev)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ProposeChange(ctx, retitled(repo, opened.ChangeID, dev, 1, "Retry the upload")); err != nil {
		t.Fatal(err)
	}
	counted := map[[2]string]uint64{}
	for _, s := range rec.Read() {
		if s.Name == metrics.CodeChangeProposals {
			counted[[2]string{s.Attrs["op"], s.Attrs["outcome"]}] += s.Total
		}
	}
	want := map[[2]string]uint64{
		{"change_open", "applied"}: 1, {"change_open", "change_exists"}: 1, {"change_edit", "unchanged"}: 1,
	}
	for key, times := range want {
		if counted[key] != times {
			t.Errorf("crewlet.code.change.proposals counted %v %d times, want %d (all: %v)", key, counted[key],
				times, counted)
		}
	}
}

// The applier's purity is the framework's claim; held here for the change's
// rows: the same records, replayed by a node that applies them from zero,
// leave every change table byte-identical.
func TestAChangesRowsReplayIdentically(t *testing.T) {
	t.Parallel()
	a, b := newTwoNodes(t)
	c, _ := committerOn(t, a, a.pub)
	ctx := t.Context()
	repo := changeRepo(t, a, "web")
	opened, err := c.ProposeChange(ctx, opening(repo, featureRef, mainRef, dev))
	requireChanged(t, opened, err)
	edited, err := c.ProposeChange(ctx, retitled(repo, opened.ChangeID, ada, opened.Version, "Retitled"))
	requireChanged(t, edited, err)
	closed, err := c.ProposeChange(ctx, closing(repo, opened.ChangeID, cto, edited.Version))
	requireChanged(t, closed, err)
	b.drain()
	for _, table := range []string{"code_changes", "code_change_bodies", "code_change_authors", "code_revisions"} {
		if got, want := tableRows(t, b.db, table), tableRows(t, a.db, table); !slices.Equal(got, want) {
			t.Errorf("%s replayed on another node holds\n\t%v\nwant\n\t%v", table, got, want)
		}
	}
}

// tableRows is every row of table on db, each as its columns' text, in key
// order.
func tableRows(t *testing.T, db store.ReplicatedHandle, table string) []string {
	t.Helper()
	var out []string
	if err := db.Reader().Read(context.Background(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(context.Background(), `SELECT * FROM `+table+` ORDER BY 1, 2`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		cols, err := rows.Columns()
		if err != nil {
			return err
		}
		for rows.Next() {
			values := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			var b strings.Builder
			for _, v := range values {
				b.WriteString("|")
				b.WriteString(strings.TrimSpace(strings.ReplaceAll(stringOf(v), "\n", " ")))
			}
			out = append(out, b.String())
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	return out
}

// stringOf is a column's value as text.
func stringOf(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return string(x)
	}
	return fmt.Sprint(v)
}
