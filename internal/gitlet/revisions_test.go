package gitlet_test

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// A CHANGE'S REVISIONS ARE ITS PUSHES': moving a live change's source writes
// its next revision and makes the pusher an author, setting its target to its
// head merges it, deleting its source closes it — through the shipped
// committer and applier on a real embedded broker.

// pushedBy proposes one push of cmds to repo by who through c, fails t unless
// every command landed ok, and answers its result. A seat's push names its
// run (turn, launch, harness), as the engine stamps a coding box's.
func pushedBy(t *testing.T, c *gitlet.Committer, repo string, who gitlet.Principal,
	cmds ...gitlet.Command) gitlet.ProposeResult {

	t.Helper()
	p := proposal(repo, cmds...)
	p.Principal = who
	if who.Kind == gitlet.PrincipalAgent {
		p.Principal.Run = gitlet.RunProvenance{TurnID: uuid.Must(uuid.NewV7()).String(),
			RunID: uuid.NewString(), Harness: "claude-code"}
	}
	res, err := c.Propose(t.Context(), p)
	requireOK(t, answer{res, err})
	return res
}

// forced is an undecided forced update of ref from old to new.
func forced(ref, old, new string) gitlet.Command {
	c := update(ref, old, new)
	c.Forced = true
	return c
}

// storedRevision is one of a change's revisions as its row holds it.
type storedRevision struct {
	n                          int64
	head, pusher, kind, pushID string
	position                   int64
}

// revisions is every revision of change id n holds, in order.
func (n *node) revisions(id string) []storedRevision {
	n.t.Helper()
	var out []storedRevision
	if err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(n.t.Context(), `SELECT n, head_oid, pusher, pusher_kind, push_id, position
			FROM code_revisions WHERE change_id = ? ORDER BY n`, id)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var r storedRevision
			if err := rows.Scan(&r.n, &r.head, &r.pusher, &r.kind, &r.pushID, &r.position); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	}); err != nil {
		n.t.Fatalf("read the revisions of %s: %v", id, err)
	}
	return out
}

// storedAuthor is one of a change's authors as its row holds it.
type storedAuthor struct {
	author, kind    string
	first, position int64
}

// authors is every author of change id n holds, in key order.
func (n *node) authors(id string) []storedAuthor {
	n.t.Helper()
	var out []storedAuthor
	if err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(n.t.Context(), `SELECT author, author_kind, first_revision, position
			FROM code_change_authors WHERE change_id = ? ORDER BY author`, id)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var a storedAuthor
			if err := rows.Scan(&a.author, &a.kind, &a.first, &a.position); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	}); err != nil {
		n.t.Fatalf("read the authors of %s: %v", id, err)
	}
	return out
}

// storedMerge is how a change's row says it merged: by what, at which
// commit, and whether it holds the instant.
type storedMerge struct {
	via, oid string
	at       bool
}

// merge is how change id's row on n says it merged.
func (n *node) merge(id string) storedMerge {
	n.t.Helper()
	var m storedMerge
	var at sql.NullInt64
	if err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(n.t.Context(), `SELECT merged_via, merge_oid, merged_at FROM code_changes
			WHERE id = ?`, id).Scan(&m.via, &m.oid, &at)
	}); err != nil {
		n.t.Fatalf("read how %s merged: %v", id, err)
	}
	m.at = at.Valid
	return m
}

// EVERY HEAD MOVE IS THE NEXT REVISION: moving a live change's source, forced
// or not, writes revision n + 1 (head, pusher, push, record position) and
// moves the change's head, revision and version, so a write formed before the
// push is refused stale; two pushes of one record are two revisions in order
// and one version; a push to a branch heading no live change (another branch,
// a closed change's source) moves no change.
//
// Mutation: skip the revision's row, or the head's move, and this fails.
func TestEveryHeadMoveIsTheNextRevision(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()
	repo := changeRepo(t, n, "web")
	opened, err := c.ProposeChange(ctx, opening(repo, featureRef, mainRef, dev))
	requireChanged(t, opened, err)
	id := opened.ChangeID

	byPerson := pushedBy(t, c, repo, ada, update(featureRef, oidOf("B"), oidOf("C")))
	bySeat := pushedBy(t, c, repo, cto, forced(featureRef, oidOf("C"), oidOf("D")))
	if got := n.change(id); got.head != oidOf("D") || got.revision != 3 || got.version != 3 ||
		got.position != bySeat.Position.Packed() || got.state != "open" {
		t.Fatalf("after two pushes to its source the change is %+v, want it open at D, revision 3, version 3, "+
			"at the second push's record", got)
	}
	want := []storedRevision{
		{1, oidOf("B"), "dev", "agent", "", opened.Position.Packed()},
		{2, oidOf("C"), "founder", "person", byPerson.Answer.PushID, byPerson.Position.Packed()},
		{3, oidOf("D"), "cto", "agent", bySeat.Answer.PushID, bySeat.Position.Packed()},
	}
	if got := n.revisions(id); !slices.Equal(got, want) {
		t.Fatalf("the change's revisions are\n\t%+v\nwant\n\t%+v", got, want)
	}
	before := n.end()
	stale, err := c.ProposeChange(ctx, retitled(repo, id, dev, opened.Version, "Retitled"))
	requireRefused(t, n, before, stale, err, gitlet.ChangeRefusalStale,
		"web!1 is at version 3, and this write was formed against version 1")

	// TWO PUSHES OF ONE RECORD, in its order: each a revision, the record
	// one version.
	first, second := uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String()
	both := n.push(repo, entry(first, set(featureRef, oidOf("D"), oidOf("E"))),
		entry(second, set(featureRef, oidOf("E"), oidOf("F"))))
	moved := n.change(id)
	if moved.head != oidOf("F") || moved.revision != 5 || moved.version != 4 ||
		moved.position != both.Position.Packed() {
		t.Fatalf("after one record of two pushes to its source the change is %+v, want it at F, revision 5, "+
			"version 4", moved)
	}
	want = append(want,
		storedRevision{4, oidOf("E"), "harness", "person", first, both.Position.Packed()},
		storedRevision{5, oidOf("F"), "harness", "person", second, both.Position.Packed()})
	if got := n.revisions(id); !slices.Equal(got, want) {
		t.Fatalf("the change's revisions are\n\t%+v\nwant\n\t%+v", got, want)
	}

	// A BRANCH THAT HEADS NO LIVE CHANGE moves none: another branch, and the
	// source of a change once it is closed.
	pushedBy(t, c, repo, ada, update(releaseRef, oidOf("R"), oidOf("R2")))
	if got := n.change(id); got != moved {
		t.Fatalf("a push to a branch the change does not name moved it to %+v", got)
	}
	closed, err := c.ProposeChange(ctx, closing(repo, id, dev, moved.version))
	requireChanged(t, closed, err)
	pushedBy(t, c, repo, ada, update(featureRef, oidOf("F"), oidOf("G")))
	if got := n.change(id); got.head != oidOf("F") || got.revision != 5 || got.version != closed.Version {
		t.Fatalf("a push to a closed change's source moved it to %+v", got)
	}
	if got := n.revisions(id); !slices.Equal(got, want) {
		t.Fatalf("a push to a closed change's source wrote revisions:\n\t%+v", got)
	}
}

// A PUSHER BECOMES AN AUTHOR from the revision they first wrote — a person by
// their human seat, a seat by its handle, in the record that added them — and
// an existing author, the owner among them, stays as they were: the authors
// are the owner and every head pusher, each once.
//
// Mutation: skip adding the pusher, and the person whose push moved the
// change is no author of it.
func TestAPusherBecomesAnAuthor(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()
	repo := changeRepo(t, n, "web")
	opened, err := c.ProposeChange(ctx, opening(repo, featureRef, mainRef, dev))
	requireChanged(t, opened, err)
	id := opened.ChangeID

	byPerson := pushedBy(t, c, repo, ada, update(featureRef, oidOf("B"), oidOf("C")))
	pushedBy(t, c, repo, dev, update(featureRef, oidOf("C"), oidOf("D")))
	byLead := pushedBy(t, c, repo, cto, update(featureRef, oidOf("D"), oidOf("E")))
	pushedBy(t, c, repo, ada, update(featureRef, oidOf("E"), oidOf("F")))
	// ONE RECORD, TWO PUSHES BY ONE NEW PUSHER: one author, from the first.
	grace := gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "pat:grace", Seat: "grace"}
	first, second := entry(uuid.Must(uuid.NewV7()).String(), set(featureRef, oidOf("F"), oidOf("G"))),
		entry(uuid.Must(uuid.NewV7()).String(), set(featureRef, oidOf("G"), oidOf("H")))
	first.Principal, second.Principal = grace, grace
	both := n.push(repo, first, second)

	want := []storedAuthor{
		{"cto", "agent", 4, byLead.Position.Packed()},
		{"dev", "agent", 1, opened.Position.Packed()},
		{"founder", "person", 2, byPerson.Position.Packed()},
		{"grace", "person", 6, both.Position.Packed()},
	}
	if got := n.authors(id); !slices.Equal(got, want) {
		t.Fatalf("the change's authors are\n\t%+v\nwant\n\t%+v", got, want)
	}
	if got := n.change(id); got.revision != 7 || got.owner != "dev" {
		t.Fatalf("the change is %+v, want dev's at revision 7", got)
	}
}

// DELETING THE SOURCE CLOSES THE CHANGE in the same record — reason
// source_deleted, at the record's instant, one version more, head and
// revisions kept — and restoring the branch reopens it at its head. A push
// that sets the target to the head and deletes the source leaves it MERGED: a
// record's merges come before its closes.
//
// Mutation: skip the close, and the change stays open on a branch that is
// gone.
func TestDeletingTheSourceClosesTheChange(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()
	repo := changeRepo(t, n, "web")
	opened, err := c.ProposeChange(ctx, opening(repo, featureRef, mainRef, dev))
	requireChanged(t, opened, err)
	id := opened.ChangeID

	deleted := pushedBy(t, c, repo, ada, update(featureRef, oidOf("B"), gitlet.ZeroOID))
	got := n.change(id)
	if got.state != "closed" || got.closeReason != gitlet.SourceDeleted || !got.closed || got.version != 2 ||
		got.position != deleted.Position.Packed() || got.head != oidOf("B") || got.revision != 1 {
		t.Fatalf("after its source was deleted the change is %+v, want it closed, source_deleted, at version 2 "+
			"and the deletion's record, its head and revision as they were", got)
	}
	if got.closedBy != "founder" || got.closedByKind != string(gitlet.PartyPerson) {
		t.Fatalf("the change a person's push closed names %q (%s) as its closer, want founder, a person",
			got.closedBy, got.closedByKind)
	}
	if revs := n.revisions(id); len(revs) != 1 {
		t.Fatalf("deleting the source wrote a revision: %+v", revs)
	}
	before := n.end()
	refused, err := c.ProposeChange(ctx, reopening(repo, id, dev, got.version))
	requireRefused(t, n, before, refused, err, gitlet.ChangeRefusalNoSource, "holds no live refs/heads/feature")

	pushedBy(t, c, repo, ada, update(featureRef, gitlet.ZeroOID, oidOf("B")))
	reopened, err := c.ProposeChange(ctx, reopening(repo, id, dev, got.version))
	requireChanged(t, reopened, err)
	if got := n.change(id); got.state != "open" || got.closed || got.closeReason != "" || got.closedBy != "" ||
		got.revision != 1 {
		t.Fatalf("reopened on its branch pushed back to its head, the change is %+v, want it open at revision 1",
			got)
	}

	// MERGED, NOT CLOSED: the target set to the head, the source deleted.
	pushedBy(t, c, repo, ada, update("refs/heads/topic", gitlet.ZeroOID, oidOf("T")))
	topic, err := c.ProposeChange(ctx, opening(repo, "refs/heads/topic", releaseRef, dev))
	requireChanged(t, topic, err)
	pushedBy(t, c, repo, ada, update("refs/heads/topic", oidOf("T"), gitlet.ZeroOID),
		update(releaseRef, oidOf("R"), oidOf("T")))
	if got := n.change(topic.ChangeID); got.state != "merged" || got.closed || got.closeReason != "" ||
		got.version != 2 {
		t.Fatalf("a push setting its target to its head and deleting its source left the change %+v, want it "+
			"merged at version 2", got)
	}
}

// PUSHING THE TARGET TO THE HEAD MERGES IT — by push, at that commit and the
// record's instant, one version more; a target moved anywhere else leaves it
// open. The head is the one the same push's moves leave, so one commit pushed
// to source and target merges whatever the command order. No later push moves
// a merged change.
//
// Mutation: skip the merge, and the change stays open with its head in its
// target.
func TestPushingTheTargetToTheHeadMergesIt(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()
	repo := changeRepo(t, n, "web")
	opened, err := c.ProposeChange(ctx, opening(repo, featureRef, mainRef, dev))
	requireChanged(t, opened, err)
	id := opened.ChangeID

	pushedBy(t, c, repo, ada, update(mainRef, oidOf("A"), oidOf("A2")))
	if got := n.change(id); got.state != "open" || got.version != 1 {
		t.Fatalf("a push moving the target elsewhere than the change's head left it %+v, want it open, unmoved",
			got)
	}
	merging := pushedBy(t, c, repo, ada, update(mainRef, oidOf("A2"), oidOf("B")))
	got := n.change(id)
	if got.state != "merged" || got.version != 2 || got.position != merging.Position.Packed() ||
		got.head != oidOf("B") || got.revision != 1 {
		t.Fatalf("a push setting the target to the change's head left it %+v, want it merged at version 2 and "+
			"the push's record", got)
	}
	if m := n.merge(id); m != (storedMerge{via: string(gitlet.MergedViaPush), oid: oidOf("B"), at: true}) {
		t.Fatalf("the merged change says it merged %+v, want by push at B, with its instant", m)
	}
	pushedBy(t, c, repo, ada, update(featureRef, oidOf("B"), oidOf("C")))
	if after := n.change(id); after != got || len(n.revisions(id)) != 1 {
		t.Fatalf("a push to a merged change's source moved it to %+v", after)
	}

	// ONE PUSH, THE TARGET LISTED BEFORE THE SOURCE: the head its own move
	// leaves is the one the target is set to.
	pushedBy(t, c, repo, ada, update("refs/heads/topic", gitlet.ZeroOID, oidOf("T")))
	topic, err := c.ProposeChange(ctx, opening(repo, "refs/heads/topic", releaseRef, dev))
	requireChanged(t, topic, err)
	both := pushedBy(t, c, repo, ada, update(releaseRef, oidOf("R"), oidOf("U")),
		update("refs/heads/topic", oidOf("T"), oidOf("U")))
	if got := n.change(topic.ChangeID); got.state != "merged" || got.head != oidOf("U") || got.revision != 2 ||
		got.version != 2 {
		t.Fatalf("one push setting a change's source and target to one commit left it %+v, want it merged at "+
			"that commit as revision 2, one version more", got)
	}
	if m := n.merge(topic.ChangeID); m.via != string(gitlet.MergedViaPush) || m.oid != oidOf("U") {
		t.Fatalf("the change merged by one push to both its branches says %+v", m)
	}
	if revs := n.revisions(topic.ChangeID); len(revs) != 2 || revs[1].pushID != both.Answer.PushID {
		t.Fatalf("the change's revisions are %+v, want its second the push that merged it", revs)
	}
}

// ONE RECORD MERGES AT MOST ITS BOUND OF CHANGES (gitlet.MaxPushMerges): a
// target set to a head more live changes share merges the lowest-numbered and
// leaves the rest open, so a record's rows stay bounded.
func TestOneRecordMergesAtMostItsBound(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()
	repo := changeRepo(t, n, "web")
	changes := gitlet.MaxPushMerges + 1
	var cmds []gitlet.Command
	for i := range changes {
		cmds = append(cmds, set(fmt.Sprintf("refs/heads/twin-%02d", i), gitlet.ZeroOID, oidOf("H")))
	}
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), cmds...))
	ids := make([]string, changes)
	for i := range changes {
		res, err := c.ProposeChange(ctx, opening(repo, fmt.Sprintf("refs/heads/twin-%02d", i), releaseRef, dev))
		requireChanged(t, res, err)
		ids[i] = res.ChangeID
	}
	pushedBy(t, c, repo, ada, update(releaseRef, oidOf("R"), oidOf("H")))
	for i, id := range ids {
		got := n.change(id)
		want := "merged"
		if i == changes-1 {
			want = "open"
		}
		if got.state != want {
			t.Errorf("web!%d is %s, want %s: a record merges the %d lowest-numbered changes it reaches",
				got.number, got.state, want, gitlet.MaxPushMerges)
		}
	}
}

// THE CHANGES A PUSH MOVES ARE COUNTED IN ITS RECORD, AND ONE THAT DOES NOT FIT
// LEADS THE NEXT: a second push whose moved changes' rows would take the
// record past its bound lands, with the push behind it, in the next record, in
// arrival order; no record's apply writes past gitlet.MaxRecordRows.
//
// Mutation: admit pushes without counting their changes' rows → the record is
// past the bound, the publish refuses the batch, and every push errs.
func TestAPushWhoseChangesDoNotFitLeadsTheNextRecord(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	var widest int
	n.applied = func(_ uint64, rows int) { widest = max(widest, rows) }
	repo := changeRepo(t, n, "web")
	// SIX HUNDRED BRANCHES TO DELETE and six hundred live changes to move,
	// three rows each: the pushes' own rows fit one record, with the
	// changes' they do not.
	const junk, changes = 600, 600
	heads := seededHeads(repo, changes)
	var cmds []gitlet.Command
	for i := range junk {
		cmds = append(cmds, set(fmt.Sprintf("refs/heads/junk-%04d", i), gitlet.ZeroOID, oidOf(fmt.Sprint("junk", i))))
	}
	for i, head := range heads {
		cmds = append(cmds, set(seededSource(i), gitlet.ZeroOID, head))
	}
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), cmds...))
	ids := n.seedChanges(repo, changesOf(changes, changeShape{revisions: 1, authors: 1}))

	gate := newGate(n.pub)
	c, _ := committerOn(t, n, gate)
	asPerson := func(cmds ...gitlet.Command) gitlet.Proposal {
		p := proposal(repo, cmds...)
		p.Principal = ada
		return p
	}
	lead := proposeAsync(t.Context(), c, asPerson(update(releaseRef, oidOf("R"), oidOf("R2"))))
	eventually(t, "the lead publish reaching the gate", func() bool { return gate.calls.Load() == 1 })
	before := n.end()
	var deletions, moves []gitlet.Command
	for i := range junk {
		deletions = append(deletions, update(fmt.Sprintf("refs/heads/junk-%04d", i), oidOf(fmt.Sprint("junk", i)),
			gitlet.ZeroOID))
	}
	for i, head := range heads {
		moves = append(moves, update(seededSource(i), head, oidOf(fmt.Sprint("next", i))))
	}
	first := proposeAsync(t.Context(), c, asPerson(deletions...))
	eventually(t, "the deletions queued", func() bool { return c.Queued(repo) == 1 })
	second := proposeAsync(t.Context(), c, asPerson(moves...))
	eventually(t, "the moves queued", func() bool { return c.Queued(repo) == 2 })
	third := proposeAsync(t.Context(), c, asPerson(update("refs/heads/after", gitlet.ZeroOID, oidOf("after"))))
	eventually(t, "the push behind queued", func() bool { return c.Queued(repo) == 3 })
	gate.open()

	requireOK(t, await(t, lead))
	a, b, after := await(t, first), await(t, second), await(t, third)
	requireOK(t, a)
	requireOK(t, b)
	requireOK(t, after)
	if a.res.Position.Seq >= b.res.Position.Seq || after.res.Position.Seq < b.res.Position.Seq {
		t.Fatalf("the deletions landed at %s, the moves at %s and the push behind at %s: want the moves in a "+
			"later record than the deletions, and the push behind them no earlier", a.res.Position,
			b.res.Position, after.res.Position)
	}
	if appended := n.end() - before; appended != 3 {
		t.Fatalf("%d records appended, want 3: the lead's, the deletions', and the moves' with the push behind",
			appended)
	}
	for i, id := range ids {
		if got := n.change(id); got.head != oidOf(fmt.Sprint("next", i)) || got.revision != 2 {
			t.Fatalf("the moved change %s is %+v, want its next head as revision 2", id, got)
		}
	}
	if widest > gitlet.MaxRecordRows {
		t.Fatalf("a record's apply wrote %d rows, past the %d one record may", widest, gitlet.MaxRecordRows)
	}
}

// A PUSH MOVING MORE CHANGES THAN A RECORD HOLDS IS REFUSED WHOLE: one whose
// changes' rows overflow even a record of its own is refused too_many_changes
// — every command ng, in one sentence naming how many changes it moves —
// appending nothing; fewer at a time land, and no record's apply writes past
// gitlet.MaxRecordRows.
//
// Mutation: admit a push without counting its changes' rows → the publish
// refuses the batch, and the push errs rather than too_many_changes.
func TestAPushMovingMoreChangesThanARecordHoldsIsRefusedWhole(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	var widest int
	n.applied = func(_ uint64, rows int) { widest = max(widest, rows) }
	repo := changeRepo(t, n, "web")
	changes := gitlet.MaxCommandsPerPush
	heads := seededHeads(repo, changes)
	var cmds []gitlet.Command
	for i, head := range heads {
		cmds = append(cmds, set(seededSource(i), gitlet.ZeroOID, head))
	}
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), cmds...))
	ids := n.seedChanges(repo, changesOf(changes, changeShape{revisions: 1, authors: 1}))
	c, _ := committerOn(t, n, n.pub)

	moves := make([]gitlet.Command, changes)
	for i, head := range heads {
		moves[i] = update(seededSource(i), head, oidOf(fmt.Sprint("next", i)))
	}
	p := proposal(repo, moves...)
	p.Principal = ada
	before := n.end()
	res, err := c.Propose(t.Context(), p)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if res.Answer.Refused != gitlet.RefusalTooManyChanges || res.Outcome != "" || n.end() != before ||
		!strings.Contains(res.Answer.Detail, fmt.Sprintf("this push moves %d live changes", changes)) {
		t.Fatalf("a push moving %d live changes answered %+v (%q) and appended %d records; want too_many_changes "+
			"naming them, nothing appended", changes, res, res.Answer.Detail, n.end()-before)
	}
	for _, cmd := range res.Answer.Commands {
		if cmd.Result != gitlet.ResultNG || cmd.Reason != gitlet.RefusalTooManyChanges || cmd.Detail != res.Answer.Detail {
			t.Fatalf("%s answered %s %s (%q), want ng under too_many_changes with the push's sentence", cmd.Ref,
				cmd.Result, cmd.Reason, cmd.Detail)
		}
	}

	half := changes / 2
	for _, part := range [][]gitlet.Command{moves[:half], moves[half:]} {
		p := proposal(repo, part...)
		p.Principal = ada
		res, err := c.Propose(t.Context(), p)
		requireOK(t, answer{res, err})
	}
	for i, id := range ids {
		if got := n.change(id); got.head != oidOf(fmt.Sprint("next", i)) || got.revision != 2 {
			t.Fatalf("the change %s pushed in half the push is %+v, want its next head as revision 2", id, got)
		}
	}
	if widest > gitlet.MaxRecordRows {
		t.Fatalf("a record's apply wrote %d rows, past the %d one record may", widest, gitlet.MaxRecordRows)
	}
}

// seededSource is the source branch seedChanges names the i-th change it seeds
// by.
func seededSource(i int) string { return fmt.Sprintf("refs/heads/change-%06d", i+1) }

// seededHeads is the head of each of the first count changes seedChanges
// seeds on repoID at one revision: its source's, pushed before the seeding.
func seededHeads(repoID string, count int) []string {
	heads := make([]string, count)
	for i := range heads {
		heads[i] = oidOf(fmt.Sprint(fmt.Sprintf("%s-change-%06d", repoID, i+1), 1))
	}
	return heads
}

// The applier's purity, held for the change rows a push writes: revisions by
// a person and a seat, a forced update, a merge by push and a close by a
// deleted source, replayed from zero, leave every change table byte-identical.
func TestRevisionsReplayIdentically(t *testing.T) {
	t.Parallel()
	a, b := newTwoNodes(t)
	c, _ := committerOn(t, a, a.pub)
	ctx := t.Context()
	repo := changeRepo(t, a, "web")
	opened, err := c.ProposeChange(ctx, opening(repo, featureRef, mainRef, dev))
	requireChanged(t, opened, err)
	pushedBy(t, c, repo, ada, update(featureRef, oidOf("B"), oidOf("C")))
	pushedBy(t, c, repo, cto, forced(featureRef, oidOf("C"), oidOf("D")))
	pushedBy(t, c, repo, ada, update(mainRef, oidOf("A"), oidOf("D")))
	pushedBy(t, c, repo, ada, update("refs/heads/topic", gitlet.ZeroOID, oidOf("T")))
	topic, err := c.ProposeChange(ctx, opening(repo, "refs/heads/topic", releaseRef, ada))
	requireChanged(t, topic, err)
	pushedBy(t, c, repo, dev, update("refs/heads/topic", oidOf("T"), oidOf("T2")))
	pushedBy(t, c, repo, ada, update("refs/heads/topic", oidOf("T2"), gitlet.ZeroOID))
	if got := a.change(opened.ChangeID); got.state != "merged" {
		t.Fatalf("the first change is %s, want merged", got.state)
	}
	if got := a.change(topic.ChangeID); got.state != "closed" {
		t.Fatalf("the second change is %s, want closed", got.state)
	}
	b.drain()
	for _, table := range []string{"code_changes", "code_change_authors", "code_revisions"} {
		if got, want := tableRows(t, b.db, table), tableRows(t, a.db, table); !slices.Equal(got, want) {
			t.Errorf("%s replayed on another node holds\n\t%v\nwant\n\t%v", table, got, want)
		}
	}
}
