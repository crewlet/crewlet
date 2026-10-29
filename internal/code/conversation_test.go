package code_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
)

// A CHANGE'S CONVERSATION THROUGH ITS WRITER: every write handed to the change
// writer beside the committer, decided in one snapshot on a real embedded
// broker as a record on the change's own subject, applied by the shipped
// applier and answered from the change's rows.

// writerOn is a change writer on n over pub, at the harness's clock, judging a
// seat and a reviewer by the grant tests' chart, with a recorder of its own. A
// gate is opened before the writer stops, so a case that fails while holding
// one still ends.
func writerOn(t testing.TB, n *node, pub code.Publisher,
	tune ...func(*code.ChangeWriterOptions)) (*code.ChangeWriter, *metrics.Recorder) {

	t.Helper()
	rec, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	o := code.ChangeWriterOptions{
		Publisher: pub, DB: n.db.Reader(), NodeID: n.id,
		Retention: func() time.Duration { return code.DefaultDeletedRetention },
		SeatRules: func() (code.SeatRules, bool) { return code.SeatRules{Roster: testChart(), Zone: time.UTC}, true },
		Now:       func() time.Time { return harnessAt }, Metrics: rec,
	}
	for _, f := range tune {
		f(&o)
	}
	w, err := code.NewChangeWriter(o)
	if err != nil {
		t.Fatalf("NewChangeWriter: %v", err)
	}
	t.Cleanup(func() {
		if g, ok := pub.(*gatePublisher); ok {
			g.open()
		}
		w.Stop(context.WithoutCancel(t.Context()))
	})
	return w, rec
}

// discussed is a change opened by dev from feature into main on a fresh
// repository named name — the repository the unit Engineering owns — and its
// id.
func discussed(t *testing.T, n *node, name string) (repo, change string) {
	t.Helper()
	c, _ := committerOn(t, n, n.pub)
	repo = changeRepo(t, n, name)
	opened, err := c.ProposeChange(t.Context(), opening(repo, featureRef, mainRef, dev))
	requireChanged(t, opened, err)
	return repo, opened.ChangeID
}

// conversing is a write of a change's conversation by asker under an operation
// id of its own: set fills in the one record it carries.
func conversing(asker code.Principal, set func(w *code.ConversationWrite)) code.ConversationWrite {
	w := code.ConversationWrite{OpID: statelog.NewOpID(time.Now(), "code_comment_on_change"), Asker: asker}
	set(&w)
	return w
}

// asking is an ask of reviewers by handle on change, by asker.
func asking(repo, change string, asker code.Principal, handles ...string) code.ConversationWrite {
	return conversing(asker, func(w *code.ConversationWrite) {
		r := &code.ReviewerRequest{RepoID: repo, ChangeID: change, Note: "the retry loop"}
		for _, h := range handles {
			r.Request = append(r.Request, code.Reviewer{Handle: h})
		}
		w.Reviewers = r
	})
}

// withdrawing is a withdrawal of the asks of handles on change, by asker.
func withdrawing(repo, change string, asker code.Principal, handles ...string) code.ConversationWrite {
	return conversing(asker, func(w *code.ConversationWrite) {
		w.Reviewers = &code.ReviewerRequest{RepoID: repo, ChangeID: change, Withdraw: handles}
	})
}

// saying is a comment on change by asker — a reply to parent where one is
// named.
func saying(repo, change string, asker code.Principal, parent, body string) code.ConversationWrite {
	return conversing(asker, func(w *code.ConversationWrite) {
		w.Comment = &code.Comment{RepoID: repo, ChangeID: change, ParentID: parent, Body: body}
	})
}

// resolving is a thread's resolution on change by asker.
func resolving(repo, change string, asker code.Principal, thread string, resolved bool) code.ConversationWrite {
	return conversing(asker, func(w *code.ConversationWrite) {
		w.ThreadResolve = &code.ThreadResolve{RepoID: repo, ChangeID: change, ThreadID: thread, Resolved: resolved}
	})
}

// requireConversed fails t unless res is a write applied at a position.
func requireConversed(t *testing.T, res code.ConversationResult, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if res.Outcome != statelog.OutcomeApplied || res.Position.IsZero() || res.Refusal != "" || res.Unchanged {
		t.Fatalf("the write answered %+v, want applied at a position", res)
	}
}

// requireConversationRefused fails t unless res is refused under code with a
// sentence holding says, and nothing was appended to n's log since before.
func requireConversationRefused(t *testing.T, n *node, before uint64, res code.ConversationResult, err error,
	refusal code.ConversationRefusal, says string) {

	t.Helper()
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if res.Refusal != refusal || res.Outcome != "" || !strings.Contains(res.Detail, says) {
		t.Fatalf("the write answered %+v, want %s saying %q", res, refusal, says)
	}
	if got := n.end(); got != before {
		t.Fatalf("a refused write appended %d records, want none", got-before)
	}
}

// reviewerRow is one ask of a reviewer as n's rows hold it.
type reviewerRow struct {
	kind, reason, state, by, byKind, note string
}

// reviewer is reviewer's ask on change as n's rows hold it, and false for none.
func (n *node) reviewer(change, reviewer string) (reviewerRow, bool) {
	n.t.Helper()
	var r reviewerRow
	err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(n.t.Context(), `SELECT reviewer_kind, reason, state, requested_by,
			requested_by_kind, note FROM code_reviewers WHERE change_id = ? AND reviewer = ?`, change, reviewer).
			Scan(&r.kind, &r.reason, &r.state, &r.by, &r.byKind, &r.note)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return reviewerRow{}, false
	case err != nil:
		n.t.Fatalf("read reviewer %s of %s: %v", reviewer, change, err)
	}
	return r, true
}

// A CONVERSATION'S RECORD IS THE CHANGE'S OWN: a comment lands as one record on
// the change's subject — never the repository's — written by the writer's
// node as its own work, its scope the repository's path and the change's, and
// it is no proposal of the committer's: a push held in the repository's
// committer, mid-publish, holds no comment back.
//
// Mutation: publish the record on the repository's subject, or queue it with
// the committer's proposals, and it waits behind the held push.
func TestAConversationIsARecordOnItsChangesOwnSubject(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	gate := newGate(n.pub)
	c, _ := committerOn(t, n, gate)
	held := proposeAsync(t.Context(), c, proposal(repo, update(mainRef, oidOf("A"), oidOf("A2"))))
	eventually(t, "the push reaching the gate", func() bool { return gate.calls.Load() == 1 })

	w, _ := writerOn(t, n, n.pub)
	say := saying(repo, change, ada, "", "Why retry twice?")
	res, err := w.Converse(t.Context(), say)
	requireConversed(t, res, err)
	if res.ID != code.CommentID(say.OpID) {
		t.Fatalf("the comment answered id %s, want %s, derived from its operation id", res.ID,
			code.CommentID(say.OpID))
	}
	rec := n.recordAt(res.Position)
	if rec.Subject != code.ChangeSubject(change) || rec.Op != code.OpComment || rec.Writer != n.id ||
		rec.ActorKind != code.ActorSystem || rec.Actor != n.id ||
		!slices.Equal([]string(rec.Scope), []string{code.RepoPath(repo), code.ChangePath(change)}) {
		t.Fatalf("the comment's record is %+v, want a comment on %s written by %s, scoped to the repository "+
			"and the change", rec.RecordEnvelope, code.ChangeSubject(change), n.id)
	}
	gate.open()
	requireOK(t, await(t, held))
}

// recordAt is the record at p on n's log, decoded.
func (n *node) recordAt(p statelog.Position) code.MutationRecord {
	n.t.Helper()
	_, body, _, ok, err := n.records.At(n.t.Context(), p.Seq)
	if err != nil || !ok {
		n.t.Fatalf("read the record at %s: %v (held: %v)", p, err, ok)
	}
	rec, err := code.Decode(body)
	if err != nil {
		n.t.Fatalf("decode the record at %s: %v", p, err)
	}
	return rec
}

// A CONVERSATION WAITS FOR THE CALLER'S FLOOR: the asker read the change on a
// node ahead of the writer's, and a reply formed against a comment it was
// shown is decided past it — or, where the writer's node cannot reach it in
// its budget, decided nothing (ErrNotServing), so the router asks the
// runner-up, never refused thread_gone by a node that has not seen the thread.
//
// Mutation: publish with no session floor, and the reply is refused
// thread_gone on the node behind.
func TestAConversationWaitsForTheCallersFloor(t *testing.T) {
	t.Parallel()
	t.Run("past the asker's floor", func(t *testing.T) {
		t.Parallel()
		a, b := newTwoNodes(t)
		repo, change := discussed(t, a, "web")
		b.drain()
		wa, _ := writerOn(t, a, a.pub)
		root, err := wa.Converse(t.Context(), saying(repo, change, ada, "", "Why?"))
		requireConversed(t, root, err)
		wb, _ := writerOn(t, b, b.pub)
		reply := saying(repo, change, ada, root.ID, "Because.")
		reply.Seen = a.waiter.Committed()
		res, err := wb.Converse(t.Context(), reply)
		requireConversed(t, res, err)
	})
	t.Run("past what the writer reaches in its budget", func(t *testing.T) {
		t.Parallel()
		a, b := newTwoNodes(t)
		repo, change := discussed(t, a, "web")
		b.drain()
		wa, _ := writerOn(t, a, a.pub)
		root, err := wa.Converse(t.Context(), saying(repo, change, ada, "", "Why?"))
		requireConversed(t, root, err)
		b.hold()
		wb, _ := writerOn(t, b, b.pub)
		before := b.end()
		reply := saying(repo, change, ada, root.ID, "Because.")
		reply.Seen = a.waiter.Committed()
		if res, err := wb.Converse(t.Context(), reply); !errors.Is(err, code.ErrNotServing) {
			t.Fatalf("a writer behind the reply's Seen answered %+v, %v; want ErrNotServing", res, err)
		}
		if got := b.end() - before; got != 0 {
			t.Fatalf("%d records appended, want none", got)
		}
	})
}

// A COMMENT REPLIES ONLY TO A ROOT: a reply names the first comment of a thread
// on the same change and joins its thread; a reply to a reply, to a comment
// of another change or to none at all is refused thread_gone, appending
// nothing — a thread is one level deep — and a comment anchored to a revision
// the change has not reached is refused unknown_revision.
//
// Mutation: let a reply name any comment, and the reply to a reply lands in
// a thread of its own making.
func TestACommentRepliesOnlyToARoot(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	c, _ := committerOn(t, n, n.pub)
	other, err := c.ProposeChange(t.Context(), opening(repo, releaseRef, mainRef, dev))
	requireChanged(t, other, err)
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()

	root, err := w.Converse(ctx, saying(repo, change, ada, "", "Why?"))
	requireConversed(t, root, err)
	reply, err := w.Converse(ctx, saying(repo, change, dev, root.ID, "Because."))
	requireConversed(t, reply, err)
	if got := n.count(`SELECT COUNT(*) FROM code_comments WHERE id = ? AND thread_id = ? AND parent_id = ?`,
		reply.ID, root.ID, root.ID); got != 1 {
		t.Fatalf("the reply is not in its root's thread")
	}
	elsewhere, err := w.Converse(ctx, saying(repo, other.ChangeID, ada, "", "And here?"))
	requireConversed(t, elsewhere, err)

	for _, tc := range []struct {
		name, parent, says string
	}{
		{"a reply to a reply", reply.ID, "is a reply in thread " + root.ID},
		{"a reply to another change's comment", elsewhere.ID, "holds no comment " + elsewhere.ID},
		{"a reply to nothing", "019a0c2e-0000-7000-8000-00000000dead", "holds no comment"},
	} {
		before := n.end()
		res, err := w.Converse(ctx, saying(repo, change, ada, tc.parent, "Hm."))
		requireConversationRefused(t, n, before, res, err, code.ConversationRefusalThreadGone, tc.says)
	}

	anchored := saying(repo, change, ada, "", "This line.")
	anchored.Comment.Anchor = &code.CommentAnchor{Revision: 2, Path: "upload/retry.go", Side: code.SideNew,
		Line: 3, BlobOID: oidOf("blob")}
	before := n.end()
	res, err := w.Converse(ctx, anchored)
	requireConversationRefused(t, n, before, res, err, code.ConversationRefusalUnknownRevision,
		"web!1 is at revision 1, and the comment is anchored to revision 2")
	anchored.Comment.Anchor.Revision = 1
	anchored.OpID = statelog.NewOpID(time.Now(), "code_comment_on_change")
	res, err = w.Converse(ctx, anchored)
	requireConversed(t, res, err)
	if got := n.count(`SELECT COUNT(*) FROM code_comments WHERE id = ? AND anchor LIKE '%"revision":1%'
		AND anchor LIKE '%upload/retry.go%'`, res.ID); got != 1 {
		t.Fatalf("the anchored comment does not keep its anchor")
	}
	if got := n.count(`SELECT comments FROM code_conversations WHERE change_id = ?`, change); got != 3 {
		t.Fatalf("the change's conversation counts %d comments, want 3", got)
	}
}

// A REQUESTED REVIEWER MUST BE ABLE TO REVIEW (rule D21): a seat is asked only
// where the chart lets it review the repository and it did not write the
// change — refused not_permitted, naming what it holds and what would admit it
// — and a person may always be asked; a handle no seat holds is no_reviewer.
// Asking is the change's authors' and the repository's reviewers', and a
// person's: a seat that only reads the repository asks nobody. Each reviewer
// asked is kept with its kind, stamped from the chart, and takes part in the
// change.
//
// Mutation: drop the requested reviewer's level check, and the read-only
// seat is asked; drop the author check, and the change's owner is asked to
// review their own change.
func TestARequestedReviewerMustBeAbleToReview(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	for _, tc := range []struct {
		name      string
		asker     code.Principal
		reviewer  string
		refusal   code.ConversationRefusal
		says      string
		landsKind string
	}{
		{"a seat below review, its relation writing", cto, "reader", code.ConversationRefusalNotPermitted,
			`seat "reader" holds read on this repository (tier read_only, relation write), and a reviewer ` +
				`reviews it: a person can set its role.code.access to review or full_access`, ""},
		{"a seat related by a read alone", cto, "vp-sales", code.ConversationRefusalNotPermitted,
			`seat "vp-sales" holds read on this repository (tier full_access, relation read), and a reviewer ` +
				`reviews it: ask the lead of the repository's owning unit`, ""},
		{"the change's author", cto, "dev", code.ConversationRefusalNotPermitted,
			`seat "dev" is an author of web!1`, ""},
		{"a handle no seat holds", cto, "ghost", code.ConversationRefusalNoReviewer, `"ghost" is no seat`, ""},
		{"a seat that only reads the repository asking", reader, "plat-lead",
			code.ConversationRefusalNotPermitted, `did not write web!1 and holds read on its repository`, ""},
		{"a seat that reviews", cto, "plat-lead", "", "", "agent"},
		{"a person", dev, "founder", "", "", "person"},
	} {
		before := n.end()
		res, err := w.Converse(ctx, asking(repo, change, tc.asker, tc.reviewer))
		if tc.refusal != "" {
			requireConversationRefused(t, n, before, res, err, tc.refusal, tc.says)
			if _, asked := n.reviewer(change, tc.reviewer); asked {
				t.Errorf("%s: %s is asked to review after a refusal", tc.name, tc.reviewer)
			}
			continue
		}
		requireConversed(t, res, err)
		got, asked := n.reviewer(change, tc.reviewer)
		by, byKind := tc.asker.Seat, "agent"
		if tc.asker.Kind == code.PrincipalOperator {
			byKind = "person"
		}
		if !asked || got != (reviewerRow{kind: tc.landsKind, reason: "requested", state: "requested", by: by,
			byKind: byKind, note: "the retry loop"}) {
			t.Errorf("%s: the ask of %s is %+v (held: %v)", tc.name, tc.reviewer, got, asked)
		}
		if n.count(`SELECT COUNT(*) FROM code_participants WHERE change_id = ? AND handle = ? AND kind = ?`,
			change, tc.reviewer, tc.landsKind) != 1 {
			t.Errorf("%s: %s does not take part in the change", tc.name, tc.reviewer)
		}
	}
}

// AN ASK FOR REVIEW MOVES ONLY WHAT IT CHANGES: asking a reviewer already asked
// and withdrawing one nobody asked is answered unchanged, appending nothing; a
// withdrawal keeps the row, withdrawn, and asking again stands it up; and a
// change asks at most MaxReviewersPerChange at once — an ask past it is
// refused too_many_reviewers, and a withdrawal makes room.
//
// Mutation: count withdrawn asks against the bound, and the ask after the
// withdrawal is refused.
func TestAnAskForReviewMovesOnlyWhatItChanges(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	res, err := w.Converse(ctx, asking(repo, change, cto, "plat-lead"))
	requireConversed(t, res, err)
	before := n.end()
	for name, again := range map[string]code.ConversationWrite{
		"an ask of a reviewer already asked":   asking(repo, change, cto, "plat-lead"),
		"a withdrawal of a reviewer not asked": withdrawing(repo, change, cto, "founder"),
	} {
		got, err := w.Converse(ctx, again)
		if err != nil || !got.Unchanged || got.Outcome != statelog.OutcomeApplied || n.end() != before {
			t.Fatalf("%s answered %+v, %v and appended %d records; want unchanged, nothing appended", name, got,
				err, n.end()-before)
		}
	}
	res, err = w.Converse(ctx, withdrawing(repo, change, cto, "plat-lead"))
	requireConversed(t, res, err)
	if got, _ := n.reviewer(change, "plat-lead"); got.state != "withdrawn" {
		t.Fatalf("a withdrawn ask is %q, want withdrawn and kept", got.state)
	}
	res, err = w.Converse(ctx, asking(repo, change, cto, "plat-lead"))
	requireConversed(t, res, err)
	if got, _ := n.reviewer(change, "plat-lead"); got.state != "requested" {
		t.Fatalf("an ask again is %q, want requested", got.state)
	}

	// THE BOUND, over persons the chart is given for the case.
	chart := testChart()
	for i := range code.MaxReviewersPerChange {
		h := fmt.Sprintf("person-%02d", i)
		chart.seats[h] = code.RosterSeat{Handle: h, Human: true, Unit: "eng"}
	}
	wide, _ := writerOn(t, n, n.pub, func(o *code.ChangeWriterOptions) {
		o.SeatRules = func() (code.SeatRules, bool) { return code.SeatRules{Roster: chart}, true }
	})
	var persons []string
	for i := range code.MaxReviewersPerChange - 1 {
		persons = append(persons, fmt.Sprintf("person-%02d", i))
	}
	res, err = wide.Converse(ctx, asking(repo, change, ada, persons...))
	requireConversed(t, res, err)
	before = n.end()
	res, err = wide.Converse(ctx, asking(repo, change, ada, fmt.Sprintf("person-%02d", code.MaxReviewersPerChange-1),
		"founder"))
	requireConversationRefused(t, n, before, res, err, code.ConversationRefusalTooManyReviewers,
		fmt.Sprintf("web!1 would ask %d reviewers at once", code.MaxReviewersPerChange+2))
	res, err = wide.Converse(ctx, withdrawing(repo, change, ada, "plat-lead"))
	requireConversed(t, res, err)
	res, err = wide.Converse(ctx, asking(repo, change, ada, fmt.Sprintf("person-%02d", code.MaxReviewersPerChange-1)))
	requireConversed(t, res, err)
	if got := n.count(`SELECT COUNT(*) FROM code_reviewers WHERE change_id = ? AND state = 'requested'`,
		change); got != code.MaxReviewersPerChange {
		t.Fatalf("the change asks %d reviewers, want %d", got, code.MaxReviewersPerChange)
	}
}

// A THREAD IS RESOLVED BY ITS STARTER, AN AUTHOR, A MAINTAINER OR A PERSON: a
// seat that started the thread, wrote the change or maintains the repository
// marks it resolved or open again, as any person does, naming who resolved
// it; any other seat is refused not_permitted; a thread's state it is in is
// answered unchanged; and a reply is no thread.
//
// Mutation: let any reader resolve a thread, and the reviewer's resolution of
// another's thread lands.
func TestAThreadIsResolvedByItsStarterAnAuthorAMaintainerOrAPerson(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	root, err := w.Converse(ctx, saying(repo, change, platLead, "", "Why?"))
	requireConversed(t, root, err)
	reply, err := w.Converse(ctx, saying(repo, change, dev, root.ID, "Because."))
	requireConversed(t, reply, err)
	other, err := w.Converse(ctx, saying(repo, change, ada, "", "And?"))
	requireConversed(t, other, err)

	before := n.end()
	res, err := w.Converse(ctx, resolving(repo, change, platLead, other.ID, true))
	requireConversationRefused(t, n, before, res, err, code.ConversationRefusalNotPermitted,
		`seat "plat-lead" did not start thread `+other.ID)
	res, err = w.Converse(ctx, resolving(repo, change, ada, reply.ID, true))
	requireConversationRefused(t, n, before, res, err, code.ConversationRefusalThreadGone, "holds no thread")

	// ITS STARTER, AN AUTHOR, A MAINTAINER AND A PERSON, each moving the
	// thread to the state it is not in: resolved, open again, and so on.
	for i, tc := range []struct {
		asker code.Principal
		by    string
		kind  string
	}{
		{platLead, "plat-lead", "agent"}, {dev, "", ""}, {cto, "cto", "agent"}, {ada, "", ""},
		{ada, "founder", "person"},
	} {
		resolved := i%2 == 0
		res, err := w.Converse(ctx, resolving(repo, change, tc.asker, root.ID, resolved))
		requireConversed(t, res, err)
		if got := n.count(`SELECT COUNT(*) FROM code_comments WHERE id = ? AND resolved = ? AND resolved_by = ?
			AND resolved_by_kind = ?`, root.ID, resolved, tc.by, tc.kind); got != 1 {
			t.Fatalf("after %s's write the thread is not resolved=%v by %q", tc.asker.Seat, resolved, tc.by)
		}
	}
	before = n.end()
	res, err = w.Converse(ctx, resolving(repo, change, ada, root.ID, true))
	if err != nil || !res.Unchanged || n.end() != before {
		t.Fatalf("a resolution of a resolved thread answered %+v, %v and appended %d records; want unchanged",
			res, err, n.end()-before)
	}
}

// ONE CHANGE'S WRITES ARE TAKEN ONE AT A TIME: while one write of a change is
// publishing, a second write of the same change waits its turn on the node,
// never publishing beside it — two would read one anchor and one lose a round
// — while a write of another change goes on; and once the first is answered,
// the second lands.
//
// Mutation: drop the single flight, and the second write reaches the
// publisher beside the first.
func TestOneChangesWritesAreTakenOneAtATime(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	c, _ := committerOn(t, n, n.pub)
	other, err := c.ProposeChange(t.Context(), opening(repo, releaseRef, mainRef, dev))
	requireChanged(t, other, err)
	gate := newGate(n.pub)
	w, _ := writerOn(t, n, gate)
	type answer struct {
		res code.ConversationResult
		err error
	}
	converse := func(cw code.ConversationWrite) <-chan answer {
		out := make(chan answer, 1)
		go func() {
			res, err := w.Converse(t.Context(), cw)
			out <- answer{res, err}
		}()
		return out
	}
	first := converse(saying(repo, change, ada, "", "One."))
	eventually(t, "the first write reaching the publisher", func() bool { return gate.calls.Load() == 1 })
	second := converse(saying(repo, change, ada, "", "Two."))
	eventually(t, "the second write waiting its turn", func() bool { return code.ChangeWriterHolders(w, change) == 2 })
	elsewhere := converse(saying(repo, other.ChangeID, ada, "", "Three."))
	eventually(t, "the other change's write reaching the publisher", func() bool { return gate.calls.Load() == 2 })
	// THE SECOND IS STILL WAITING: the first holds the change's turn inside
	// the gate, so nothing but the other change's write can have reached it.
	if got, holders := gate.calls.Load(), code.ChangeWriterHolders(w, change); got != 2 || holders != 2 {
		t.Fatalf("%d publishes in flight and %d writes of the change held or waiting, want the first write's "+
			"and the other change's publishing, and the second write of the change waiting its turn", got,
			holders)
	}
	gate.open()
	for name, ch := range map[string]<-chan answer{"first": first, "second": second, "elsewhere": elsewhere} {
		select {
		case a := <-ch:
			requireConversed(t, a.res, a.err)
		case <-time.After(30 * time.Second):
			t.Fatalf("the %s write was not answered in 30 s", name)
		}
	}
}

// A WRITE WAITS ITS TURN ONLY WHILE ITS ASKER'S ATTEMPT HAS ROOM: one whose
// deadline leaves no more than a publish and its answer is refused busy,
// undecided, at once — the committer's rule for a push's wait in its queue —
// and a stopping writer refuses every write as a node that ran nothing, so the
// router asks the repository's runner-up.
func TestAWriteWaitsItsTurnOnlyWhileItsAttemptHasRoom(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	w, rec := writerOn(t, n, n.pub)
	ctx, cancel := context.WithTimeout(t.Context(), code.CommitBudget)
	defer cancel()
	before := n.end()
	res, err := w.Converse(ctx, saying(repo, change, ada, "", "Late."))
	requireConversationRefused(t, n, before, res, err, code.ConversationRefusalBusy, "stops waiting in")
	if got := readingOf(rec, metrics.CodeConversationWrites, metrics.Attrs{"op": "comment",
		"outcome": "busy"}).Total; got != 1 {
		t.Fatalf("crewlet.code.conversation.writes counts %d busy comments, want 1", got)
	}
	w.Stop(t.Context())
	if res, err := w.Converse(t.Context(), saying(repo, change, ada, "", "After the stop.")); !errors.Is(err,
		code.ErrNotServing) || n.end() != before {
		t.Fatalf("a stopped writer answered %+v, %v; want ErrNotServing, nothing appended", res, err)
	}
}

// A SEAT'S WRITE AND EVERY ASK OF REVIEWERS NEED THE DECIDING NODE'S CHART: on
// a node that holds none yet the writer decides nothing of them and says so
// about itself (ErrNotServing), so the router asks the repository's runner-up;
// a person's comment is decided there as ever. And a node confirmed behind the
// company's configuration decides none of them either.
func TestAWriteTheChartJudgesNeedsTheDecidingNodesChart(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	chartless, _ := writerOn(t, n, n.pub, func(o *code.ChangeWriterOptions) {
		o.SeatRules = func() (code.SeatRules, bool) { return code.SeatRules{}, false }
	})
	behind, _ := writerOn(t, n, n.pub, func(o *code.ChangeWriterOptions) {
		o.Current = func() bool { return false }
	})
	before := n.end()
	for name, w := range map[string]*code.ChangeWriter{"no chart": chartless, "behind": behind} {
		for what, cw := range map[string]code.ConversationWrite{
			"a seat's comment":         saying(repo, change, dev, "", "Hm."),
			"a person's ask of review": asking(repo, change, ada, "plat-lead"),
		} {
			if res, err := w.Converse(t.Context(), cw); !errors.Is(err, code.ErrNotServing) || n.end() != before {
				t.Fatalf("%s: %s answered %+v, %v; want ErrNotServing, nothing appended", name, what, res, err)
			}
		}
		res, err := w.Converse(t.Context(), saying(repo, change, ada, "", "A person's comment, "+name))
		requireConversed(t, res, err)
		before = n.end()
	}
}

// A WRITE NO WRITER COULD DECIDE IS REFUSED BEFORE ANYTHING IS READ
// (ConversationWrite.Validate): every rule names the field it holds, the
// writer answers ErrInvalidProposal before its change's turn, and nothing is
// appended — a field the writer stamps (who wrote it, a reviewer's kind, a
// comment's id and thread) sent by an asker among them, since a record says
// what its writer did and never what its asker claimed.
//
// Mutations: drop the stamped-field rule for a comment's id, and a comment
// naming an id of its own is decided; let a reply carry an anchor, and it is
// decided.
func TestAConversationWriteIsRefusedBeforeAnythingIsRead(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	anchor := &code.CommentAnchor{Revision: 1, Path: "upload/retry.go", Side: code.SideNew, Line: 3,
		BlobOID: oidOf("blob")}
	thread := "019a0c2e-0000-7000-8000-0000000000aa"
	withComment := func(set func(c *code.Comment)) code.ConversationWrite {
		cw := saying(repo, change, dev, "", "Why?")
		set(cw.Comment)
		return cw
	}
	withAsk := func(set func(r *code.ReviewerRequest)) code.ConversationWrite {
		cw := asking(repo, change, dev, "plat-lead")
		set(cw.Reviewers)
		return cw
	}
	many := make([]string, code.MaxReviewersPerChange+1)
	for i := range many {
		many[i] = fmt.Sprintf("seat-%02d", i)
	}
	for name, tc := range map[string]struct {
		w     code.ConversationWrite
		field string
	}{
		"no record": {conversing(dev, func(*code.ConversationWrite) {}), "exactly one"},
		"two records": {conversing(dev, func(w *code.ConversationWrite) {
			w.Comment = &code.Comment{RepoID: repo, ChangeID: change, Body: "Why?"}
			w.ThreadResolve = &code.ThreadResolve{RepoID: repo, ChangeID: change, ThreadID: thread}
		}), "exactly one"},
		"an operation id this engine does not accept": {func() code.ConversationWrite {
			cw := saying(repo, change, dev, "", "Why?")
			cw.OpID = "not-an-op-id"
			return cw
		}(), "op_id"},
		"an asker naming a run": {func() code.ConversationWrite {
			cw := saying(repo, change, dev, "", "Why?")
			cw.Asker.Run = code.RunProvenance{TurnID: thread}
			return cw
		}(), "asker.run"},
		"a floor on another log": {func() code.ConversationWrite {
			cw := saying(repo, change, dev, "", "Why?")
			cw.Seen = statelog.Position{Stream: "CREWLET_TRACKER_LOG", Seq: 3}
			return cw
		}(), "seen"},
		"a repository that is no uuid": {withComment(func(c *code.Comment) { c.RepoID = "web" }), "repo_id"},
		"a change that is no uuidv7": {withComment(func(c *code.Comment) {
			c.ChangeID = "00000000-0000-4000-8000-000000000001"
		}), "change_id"},
		"a comment's id sent":      {withComment(func(c *code.Comment) { c.CommentID = thread }), "comment.comment_id"},
		"a comment's thread sent":  {withComment(func(c *code.Comment) { c.ThreadID = thread }), "comment.thread_id"},
		"who said it sent":         {withComment(func(c *code.Comment) { c.By = dev }), "comment.by"},
		"a comment saying nothing": {withComment(func(c *code.Comment) { c.Body = " \n" }), "comment.body"},
		"a comment past its bound": {withComment(func(c *code.Comment) {
			c.Body = strings.Repeat("x", code.MaxCommentBytes+1)
		}), "comment.body"},
		"a reply to no comment's id": {withComment(func(c *code.Comment) { c.ParentID = "web!1" }),
			"comment.parent_id"},
		"an anchor on a reply": {withComment(func(c *code.Comment) { c.ParentID, c.Anchor = thread, anchor }),
			"comment.anchor"},
		"an anchor of no line": {withComment(func(c *code.Comment) {
			bad := *anchor
			bad.Line = 0
			c.Anchor = &bad
		}), "comment.anchor"},
		"a turn past its bound": {withComment(func(c *code.Comment) {
			c.TurnID = strings.Repeat("t", code.MaxProvenanceBytes+1)
		}), "comment.turn_id"},
		"an ask naming nobody": {withAsk(func(r *code.ReviewerRequest) { r.Request = nil }), "reviewers"},
		"an ask of no seat's handle": {withAsk(func(r *code.ReviewerRequest) {
			r.Request = []code.Reviewer{{Handle: "Plat Lead"}}
		}), "reviewers.request[0].handle"},
		"an ask naming a handle twice": {withAsk(func(r *code.ReviewerRequest) {
			r.Withdraw = []string{"plat-lead"}
		}), "reviewers.withdraw[0]"},
		"a reviewer's kind sent": {withAsk(func(r *code.ReviewerRequest) { r.Request[0].Kind = code.PartyAgent }),
			"reviewers.request[0].kind"},
		"an ask past the bound": {withAsk(func(r *code.ReviewerRequest) {
			r.Request = nil
			for _, h := range many {
				r.Request = append(r.Request, code.Reviewer{Handle: h})
			}
		}), "reviewers.request"},
		"a note past its bound": {withAsk(func(r *code.ReviewerRequest) {
			r.Note = strings.Repeat("n", code.MaxReviewNoteBytes+1)
		}), "reviewers.note"},
		"a thread named by no comment's id": {resolving(repo, change, dev, "web!1#2", true),
			"thread_resolve.thread_id"},
	} {
		before := n.end()
		res, err := w.Converse(t.Context(), tc.w)
		if !errors.Is(err, code.ErrInvalidProposal) || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: Converse = %+v, %v; want ErrInvalidProposal naming %s", name, res, err, tc.field)
		}
		if got := n.end(); got != before {
			t.Errorf("%s: an invalid write appended %d records", name, got-before)
		}
	}
}

// A PURGED REPOSITORY'S CONVERSATION IS GATED: once a purge has removed a
// repository, every record on one of its changes' subjects applies nowhere —
// the deletion gate reads the repository off the record's payload — and the
// gate reader says so from the record's body; a write asked of the writer is
// refused repo_deleted, naming the purged repository.
//
// Mutation: gate a change's records by their subject alone, and the record
// lands a comment on a change nothing holds any more.
func TestAPurgedRepositorysConversationIsGated(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	said, err := w.Converse(t.Context(), saying(repo, change, ada, "", "Before the purge."))
	requireConversed(t, said, err)
	n.deleteAt(repo, harnessAt.Add(-2*keep))
	for {
		res, err := n.store.PurgeStep(t.Context(), repo, keep)
		if err != nil {
			t.Fatalf("PurgeStep: %v", err)
		}
		if res.Done {
			break
		}
	}
	if got := n.count(`SELECT COUNT(*) FROM code_comments WHERE change_id = ?`, change) +
		n.count(`SELECT COUNT(*) FROM code_conversations WHERE change_id = ?`, change); got != 0 {
		t.Fatalf("the purge left %d rows of the change's conversation", got)
	}

	late := code.Comment{RepoID: repo, ChangeID: change, CommentID: "019a0c2e-0000-7000-8000-0000000000aa",
		ThreadID: "019a0c2e-0000-7000-8000-0000000000aa", By: ada, Body: "After the purge."}
	_, err = n.publish(code.ChangeSubject(change), code.OpComment,
		code.Scope{code.RepoPath(repo), code.ChangePath(change)}, late)
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonDeleted {
		t.Fatalf("a comment on a purged repository's change published %v, want it dropped by the deletion gate",
			err)
	}
	if got := n.count(`SELECT COUNT(*) FROM code_comments WHERE change_id = ?`, change); got != 0 {
		t.Fatalf("a comment on a purged repository's change wrote %d rows", got)
	}
	if got := n.count(`SELECT rejects FROM code_deletions WHERE repo_id = ?`, repo); got != 1 {
		t.Fatalf("the deletion marker counts %d records dropped, want the one comment", got)
	}

	body, err := code.Encode(code.MutationRecord{RecordEnvelope: code.RecordEnvelope{OpID: "op-late",
		Subject: code.ChangeSubject(change), Op: code.OpComment, Scope: code.Scope{code.ChangePath(change)}},
		Mutation: []byte(`{"repo_id":"` + repo + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	gates := code.NewGates(n.db.Reader())
	subject := statelog.Subject{Kind: string(code.KindChange), ID: change}
	at := statelog.Position{Stream: code.Domain{}.Stream().Name, Seq: n.end() + 1}
	if reason, gated, err := gates.GatedAt(t.Context(), subject, "", "op-late", at, body); err != nil || !gated ||
		reason != statelog.ReasonDeleted {
		t.Fatalf("the gate reader judged the record's body %q, %v, %v; want deleted", reason, gated, err)
	}
	if _, gated, err := gates.GatedAt(t.Context(), subject, "", "op-late", at, nil); err != nil || gated {
		t.Fatalf("the gate reader judged a change's record with no body gated (%v, %v): its subject names no "+
			"repository", gated, err)
	}

	before := n.end()
	res, err := w.Converse(t.Context(), saying(repo, change, ada, "", "Asked after the purge."))
	requireConversationRefused(t, n, before, res, err, code.ConversationRefusalRepoDeleted, "web")
}

// A CONVERSATION REPLAYS IDENTICALLY: a node that applies the change's records
// from the log holds the rows of its conversation the node that wrote them
// does — its reviews, its findings and their texts, a later review's
// supersessions and the counters among them — the apply reads no clock, no
// configuration and no chart, a reviewer's kind riding the record.
func TestAConversationReplaysIdentically(t *testing.T) {
	t.Parallel()
	a, b := newTwoNodes(t)
	repo, change := discussed(t, a, "web")
	w, _ := writerOn(t, a, a.pub)
	ctx := t.Context()
	root, err := w.Converse(ctx, saying(repo, change, ada, "", "Why?"))
	requireConversed(t, root, err)
	review, err := w.Converse(ctx, reviewing(repo, change, platLead, 1, code.VerdictRequestChanges, nil,
		raising(code.SeverityBlocker, 1, "No backoff"), raising(code.SeverityMinor, 2, "A long line")))
	findings := requireReviewed(t, review, err)
	for _, cw := range []code.ConversationWrite{
		asking(repo, change, cto, "plat-lead", "founder"),
		saying(repo, change, platLead, root.ID, "Because."),
		withdrawing(repo, change, cto, "founder"),
		resolving(repo, change, cto, root.ID, true),
		answering(repo, change, dev, code.FindingWontFix, findings[0]),
		conversing(dev, func(cw *code.ConversationWrite) {
			cw.Comment = &code.Comment{RepoID: repo, ChangeID: change, FindingID: findings[0], Body: "Not here."}
		}),
		reviewing(repo, change, cto, 1, code.VerdictApprove, nil),
	} {
		res, err := w.Converse(ctx, cw)
		requireConversed(t, res, err)
	}
	a.revise(repo, "B", "B2")
	res, err := w.Converse(ctx, reviewing(repo, change, platLead, 2, code.VerdictApprove, nil))
	requireReviewed(t, res, err)
	b.drain()
	for _, table := range []string{"code_reviewers", "code_participants", "code_comments", "code_conversations",
		"code_reviews", "code_findings", "code_finding_texts"} {
		if got, want := b.dump(table), a.dump(table); !slices.Equal(got, want) {
			t.Errorf("%s replayed as\n\t%v\nwant\n\t%v", table, got, want)
		}
	}
}

// dump is every row of table n holds, each rendered whole, in rowid order.
func (n *node) dump(table string) []string {
	n.t.Helper()
	var out []string
	if err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(n.t.Context(), `SELECT * FROM `+table+` ORDER BY rowid`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		cols, err := rows.Columns()
		if err != nil {
			return err
		}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			out = append(out, fmt.Sprint(vals...))
		}
		return rows.Err()
	}); err != nil {
		n.t.Fatalf("dump %s: %v", table, err)
	}
	return out
}

// seedConversation writes a change's conversation straight into a node's
// estate — reviewers, participants and comments of the width given each, and
// its row weighing them — the bulk a purge has to clear. A COMMENT STANDS FOR
// THE WIDTH ITS ROW STATES, as seedChanges' bodies do: its body carries at
// most a kilobyte, and its body_bytes and the conversation's text_bytes the
// width given.
func (n *node) seedConversation(repoID, changeID string, reviewers, participants, comments int, width int64) {
	n.t.Helper()
	ctx := n.t.Context()
	vars := n.db.Caps().MaxVariables
	if err := n.db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := store.InsertRows(ctx, tx, vars, `INSERT INTO code_reviewers (change_id, reviewer,
			reviewer_kind, reason, state, requested_by, requested_by_kind, position) VALUES `,
			`(?, ?, 'agent', 'requested', 'requested', 'seat-00000', 'agent', 1)`, ``, reviewers,
			func(i int) []any { return []any{changeID, fmt.Sprintf("reviewer-%05d", i)} }); err != nil {
			return err
		}
		if _, err := store.InsertRows(ctx, tx, vars, `INSERT INTO code_participants (change_id, handle, kind,
			position) VALUES `, `(?, ?, 'agent', 1)`, ``, participants,
			func(i int) []any { return []any{changeID, fmt.Sprintf("participant-%05d", i)} }); err != nil {
			return err
		}
		if _, err := store.InsertRows(ctx, tx, vars, `INSERT INTO code_comments (id, change_id, thread_id, author,
			author_kind, body_bytes, position, at, body) VALUES `, `(?, ?, ?, 'seat-00000', 'agent', ?, ?, 0, ?)`,
			``, comments, func(i int) []any {
				id := fmt.Sprintf("%s-comment-%06d", changeID, i)
				return []any{id, changeID, id, width, i + 1, strings.Repeat("c", int(min(width, 1<<10)))}
			}); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO code_conversations (change_id, repo_id, comments, text_bytes,
			version, position) VALUES (?, ?, ?, ?, 1, 1)`, changeID, repoID, comments, int64(comments)*width)
		return err
	}); err != nil {
		n.t.Fatalf("seed %s's conversation: %v", changeID, err)
	}
}

// seedReviews writes reviews of a change straight into a node's estate, each
// raising findings of its own — the review's summary and each finding's text
// of the width given — and adds what they weigh to the change's conversation
// row, which seedConversation made: the bulk of a reviewed change a purge
// has to clear. Each review and each text stands for the width its row states,
// as a seeded comment does.
func (n *node) seedReviews(changeID string, reviews, findings int, width int64) {
	n.t.Helper()
	ctx := n.t.Context()
	vars := n.db.Caps().MaxVariables
	if err := n.db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := store.InsertRows(ctx, tx, vars, `INSERT INTO code_reviews (id, change_id, revision, reviewer,
			reviewer_kind, verdict, findings, summary_bytes, position, at, summary) VALUES `,
			`(?, ?, 1, 'seat-00000', 'agent', 'comment', ?, ?, ?, 0, ?)`, ``, reviews, func(i int) []any {
				return []any{fmt.Sprintf("%s-review-%06d", changeID, i), changeID, findings, width, i + 1,
					strings.Repeat("s", int(min(width, 1<<10)))}
			}); err != nil {
			return err
		}
		for r := range reviews {
			review := fmt.Sprintf("%s-review-%06d", changeID, r)
			if _, err := store.InsertRows(ctx, tx, vars, `INSERT INTO code_findings (id, change_id, review_id,
				raiser, raiser_kind, revision, commit_oid, path, blob_oid, side, line_start, line_end, severity,
				category, state, position) VALUES `, `(?, ?, ?, 'seat-00000', 'agent', 1, '', 'a.go', '', 'new', 1,
				1, 'minor', 'style', 'open', ?)`, ``, findings, func(i int) []any {
				return []any{fmt.Sprintf("%s-finding-%06d", review, i), changeID, review, r + 1}
			}); err != nil {
				return err
			}
			if _, err := store.InsertRows(ctx, tx, vars, `INSERT INTO code_finding_texts (change_id, finding_id,
				bytes, title) VALUES `, `(?, ?, ?, ?)`, ``, findings, func(i int) []any {
				return []any{changeID, fmt.Sprintf("%s-finding-%06d", review, i), width,
					strings.Repeat("t", int(min(width, 1<<10)))}
			}); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE code_conversations SET reviews = reviews + ?,
			text_bytes = text_bytes + ? WHERE change_id = ?`, reviews, int64(reviews*(1+findings))*width, changeID)
		return err
	}); err != nil {
		n.t.Fatalf("seed %s's reviews: %v", changeID, err)
	}
}

// seedFacts writes the facts of a change's first revisions straight into a
// node's estate, each row's touched paths of the width given, and adds what
// they weigh to the change's conversation row, which seedConversation made: a
// home's facts of a long-lived change a purge has to clear.
func (n *node) seedFacts(changeID string, revisions int, width int64) {
	n.t.Helper()
	ctx := n.t.Context()
	if err := n.db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := store.InsertRows(ctx, tx, n.db.Caps().MaxVariables, `INSERT INTO code_revision_facts
			(change_id, n, target_ref, target_oid, target_position, touched_count, node, owners_digest, bytes,
			position, touched) VALUES `, `(?, ?, 'refs/heads/main', '', 1, 1, 'node-a', '', ?, 1, ?)`, ``,
			revisions, func(i int) []any {
				return []any{changeID, i + 1, width, strings.Repeat("p", int(min(width, 1<<10)))}
			}); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE code_conversations SET text_bytes = text_bytes + ? WHERE change_id = ?`,
			int64(revisions)*width, changeID)
		return err
	}); err != nil {
		n.t.Fatalf("seed %s's facts: %v", changeID, err)
	}
}

// conversationRows is every row of the conversations of the changes ids names
// this node holds, the conversations' own included, and their revisions'
// facts.
func (n *node) conversationRows(ids []string) int {
	n.t.Helper()
	list := `["` + strings.Join(ids, `","`) + `"]`
	total := 0
	for _, table := range []string{"code_reviewers", "code_participants", "code_comments", "code_conversations",
		"code_reviews", "code_findings", "code_finding_texts", "code_revision_facts"} {
		total += n.count(`SELECT COUNT(*) FROM `+table+` WHERE change_id IN (SELECT value FROM json_each(?))`,
			list)
	}
	return total
}

// A PURGE REMOVES A CHANGE'S CONVERSATION with the change, every part within
// PurgePartRows and the conversation before its change's row: a change whose
// conversation does not fit a part is cut — its narrow rows first, its
// findings among them, then its comments, its findings' texts, its reviews
// and its revisions' facts — and a later part takes it whole; nothing of a
// neighbour's goes.
//
// Mutation: leave a conversation table out of the walk's count — the facts'
// among them — and a part deletes past PurgePartRows; out of the final purge,
// and its rows outlive their repository; delete the changes before their
// conversations, and the conversations are orphaned.
func TestAPurgeRemovesAChangesConversation(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("app", described("one"), t1)
	n.ensure("other", described("two"), t1)
	id, other := n.mustID("app"), n.mustID("other")
	n.deleteAt(id, harnessAt.Add(-2*keep))
	ids := n.seedChanges(id, changesOf(4, changeShape{revisions: 1, authors: 1}))
	for i, change := range ids {
		// THE LAST CHANGE'S CONVERSATION ALONE IS WIDER THAN A PART: it is
		// cut, and taken whole by a later part.
		comments := 10
		if i == len(ids)-1 {
			comments = code.PurgePartRows
		}
		n.seedConversation(id, change, 3, 4, comments, 64)
		n.seedReviews(change, 2, 3, 64)
		n.seedFacts(change, 2, 64)
	}
	neighbours := n.seedChanges(other, changesOf(2, changeShape{revisions: 1, authors: 1}))
	for _, change := range neighbours {
		n.seedConversation(other, change, 1, 1, 1, 64)
	}
	left := func() int {
		changes, revisions, authors, bodies := n.changeRows(id)
		return changes + revisions + authors + bodies + n.conversationRows(ids)
	}
	for steps := 0; ; steps++ {
		if steps > 5 {
			t.Fatal("the purge took more than five steps")
		}
		before := left()
		res, err := n.store.PurgeStep(t.Context(), id, keep)
		if err != nil {
			t.Fatalf("PurgeStep: %v", err)
		}
		if removed := before - left(); removed > code.PurgePartRows+1 {
			t.Fatalf("a part removed %d rows, past PurgePartRows (%d)", removed, code.PurgePartRows)
		}
		if res.Done {
			break
		}
	}
	if got := left(); got != 0 {
		t.Fatalf("the purge left %d rows of the repository's changes and their conversations", got)
	}
	if got := n.conversationRows(neighbours); got != 2*(1+1+1+1) {
		t.Fatalf("the neighbour holds %d rows of its conversations after the purge, want its 8", got)
	}
}

// A PURGE IS BOUNDED BY WHAT A CONVERSATION WEIGHS, in one budget with the
// pushes and the bodies: a change whose comments weigh three parts' bytes is
// cut by them — each part takes its comments while what the part weighs is
// under PurgePartBytes, taking what it deleted off the conversation's
// text_bytes — and the change goes whole once what is left fits; the final
// purge, whose statements are unbounded, is formed only then.
//
// Mutation: take a change whole by its rows alone, and the first part deletes
// every comment; never cut a conversation by its text, and the purge never
// ends; leave text_bytes as it was after a cut, and the walk weighs comments
// that are gone.
func TestAPurgeIsBoundedByWhatAConversationWeighs(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("app", described("one"), t1)
	id := n.mustID("app")
	n.deleteAt(id, harnessAt.Add(-2*keep))
	ids := n.seedChanges(id, changesOf(1, changeShape{revisions: 1, authors: 1}))
	const width = 1 << 20
	comments := 3 * code.PurgePartBytes / width
	n.seedConversation(id, ids[0], 0, 0, comments, width)
	var steps []int
	for range 6 {
		before := n.count(`SELECT COUNT(*) FROM code_comments WHERE change_id = ?`, ids[0])
		res, err := n.store.PurgeStep(t.Context(), id, keep)
		if err != nil {
			t.Fatalf("PurgeStep: %v", err)
		}
		after := n.count(`SELECT COUNT(*) FROM code_comments WHERE change_id = ?`, ids[0])
		steps = append(steps, before-after)
		if after > 0 {
			if got, want := n.count(`SELECT text_bytes FROM code_conversations WHERE change_id = ?`, ids[0]),
				after*width; got != want {
				t.Fatalf("after a cut the conversation weighs %d bytes, want the %d its %d comments hold", got,
					want, after)
			}
		}
		if res.Done {
			break
		}
	}
	perPart := int(code.PurgePartBytes / width)
	if want := []int{perPart, perPart, perPart}; !slices.Equal(steps, want) {
		t.Fatalf("the purge removed %v comments a record, want %v: a part's bytes of them at a time, then the "+
			"purge once what is left fits", steps, want)
	}
	if changes, _, _, _ := n.changeRows(id); changes != 0 {
		t.Fatalf("the purge left %d changes", changes)
	}
}

// A PURGE CUTS A CONVERSATION BY ITS FINDINGS AND REVIEWS TOO: a change whose
// findings' texts and reviews' summaries weigh three parts' bytes is cut by
// them — its findings' texts by finding, then its reviews in the order given
// — each part taking them while what it weighs is under PurgePartBytes and
// taking what it deleted off the conversation's text_bytes, and the change
// goes whole once what is left fits.
//
// Mutation: leave the findings' texts out of a cut, and the purge never ends;
// leave text_bytes as it was after a cut, and the walk weighs texts that are
// gone.
func TestAPurgeCutsAConversationByItsFindingsAndReviewsToo(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("app", described("one"), t1)
	id := n.mustID("app")
	n.deleteAt(id, harnessAt.Add(-2*keep))
	ids := n.seedChanges(id, changesOf(1, changeShape{revisions: 1, authors: 1}))
	const width = 1 << 20
	perPart := int(code.PurgePartBytes / width)
	// ONE REVIEW RAISING ALL BUT ONE OF THREE PARTS' WIDE ROWS, its summary
	// the last of them.
	n.seedConversation(id, ids[0], 0, 0, 0, width)
	n.seedReviews(ids[0], 1, 3*perPart-1, width)
	wide := func() int {
		return n.count(`SELECT COUNT(*) FROM code_finding_texts WHERE change_id = ?`, ids[0]) +
			n.count(`SELECT COUNT(*) FROM code_reviews WHERE change_id = ?`, ids[0])
	}
	var steps []int
	for range 6 {
		before := wide()
		res, err := n.store.PurgeStep(t.Context(), id, keep)
		if err != nil {
			t.Fatalf("PurgeStep: %v", err)
		}
		after := wide()
		steps = append(steps, before-after)
		if after > 0 {
			if got, want := n.count(`SELECT text_bytes FROM code_conversations WHERE change_id = ?`, ids[0]),
				after*width; got != want {
				t.Fatalf("after a cut the conversation weighs %d bytes, want the %d its %d wide rows hold", got,
					want, after)
			}
		}
		if res.Done {
			break
		}
	}
	if want := []int{perPart, perPart, perPart}; !slices.Equal(steps, want) {
		t.Fatalf("the purge removed %v wide rows a record, want %v: a part's bytes of them at a time, then the "+
			"purge once what is left fits", steps, want)
	}
	if changes, _, _, _ := n.changeRows(id); changes != 0 || n.conversationRows(ids) != 0 {
		t.Fatalf("the purge left %d changes and %d rows of their conversation", changes, n.conversationRows(ids))
	}
}
