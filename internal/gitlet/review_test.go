package gitlet_test

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CHANGE'S REVIEWS AND THE FINDINGS THEY RAISE, through the change writer on
// a real embedded broker: who may review, a review with all its findings as
// one record, a later revision's review superseding what its reviewer does
// not carry, and S40 — who clears a blocker — each read back from the rows
// the shipped applier wrote.

// reviewing is a review of revision of change by asker with verdict, carrying
// carries and raising findings, under an operation id of its own.
func reviewing(repo, change string, asker gitlet.Principal, revision int64, verdict gitlet.Verdict, carries []string,
	findings ...gitlet.Finding) gitlet.ConversationWrite {

	return conversing(asker, func(w *gitlet.ConversationWrite) {
		w.Review = &gitlet.Review{RepoID: repo, ChangeID: change, Revision: revision, Verdict: verdict,
			Summary: "The retry loop.", Findings: findings, Carries: carries}
	})
}

// raising is a finding of severity on line of upload/retry.go, titled title.
func raising(severity gitlet.Severity, line int, title string) gitlet.Finding {
	return gitlet.Finding{Path: "upload/retry.go", Side: gitlet.SideNew, LineStart: line, LineEnd: line,
		CommitOID: oidOf("B"), BlobOID: oidOf("blob"), Severity: severity, Category: gitlet.CategoryCorrectness,
		Title: title}
}

// answering is an answer of resolution to each finding ids names on change,
// by asker.
func answering(repo, change string, asker gitlet.Principal, resolution gitlet.FindingState,
	ids ...string) gitlet.ConversationWrite {

	return conversing(asker, func(w *gitlet.ConversationWrite) {
		fr := &gitlet.FindingResolve{RepoID: repo, ChangeID: change}
		for _, id := range ids {
			fr.Findings = append(fr.Findings, gitlet.FindingResolution{FindingID: id, Resolution: resolution})
		}
		w.FindingResolve = fr
	})
}

// requireReviewed fails t unless res is a review applied at a position, and
// answers its findings' ids in the order it raised them.
func requireReviewed(t *testing.T, res gitlet.ConversationResult, err error) []string {
	t.Helper()
	requireConversed(t, res, err)
	if res.ID == "" {
		t.Fatalf("the review answered %+v, naming no review", res)
	}
	return res.FindingIDs
}

// revise moves change's source branch on repo — feature — from old to new
// through a push record, its next revision.
func (n *node) revise(repo, old, new string) {
	n.t.Helper()
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(featureRef, oidOf(old), oidOf(new))))
}

// findingStates is the state of every finding of change n's rows hold, by
// id.
func (n *node) findingStates(change string) map[string]gitlet.FindingState {
	n.t.Helper()
	out := map[string]gitlet.FindingState{}
	if err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(n.t.Context(), `SELECT id, state FROM code_findings WHERE change_id = ?`,
			change)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id, state string
			if err := rows.Scan(&id, &state); err != nil {
				return err
			}
			out[id] = gitlet.FindingState(state)
		}
		return rows.Err()
	}); err != nil {
		n.t.Fatalf("read %s's findings: %v", change, err)
	}
	return out
}

// counters is a change's conversation's review counters as its row holds
// them.
type counters struct {
	reviews, openFindings, openBlockers, changesRequested int
}

// counters is change's conversation's counters as n's rows hold them.
func (n *node) counters(change string) counters {
	n.t.Helper()
	var c counters
	if err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(n.t.Context(), `SELECT reviews, open_findings, open_blockers, changes_requested
			FROM code_conversations WHERE change_id = ?`, change).Scan(&c.reviews, &c.openFindings,
			&c.openBlockers, &c.changesRequested)
	}); err != nil {
		n.t.Fatalf("read %s's conversation: %v", change, err)
	}
	return c
}

// requireStates fails t unless every finding want names is in the state it
// gives.
func requireStates(t *testing.T, n *node, change string, want map[string]gitlet.FindingState) {
	t.Helper()
	got := n.findingStates(change)
	for id, state := range want {
		if got[id] != state {
			t.Errorf("finding %s is %q, want %q", id, got[id], state)
		}
	}
}

// A BLOCKER CLEARS ONLY BY ITS DISMISSAL OR ITS RAISER'S LATER REVIEW (S40):
// an author's answer — resolved, won't fix, disputed — leaves a blocker
// STANDING, still counted among the change's standing blockers, for its raiser
// to judge; its raiser, a principal that maintains the repository or a person
// dismisses one, and nobody else — an author who writes the repository is
// refused not_raiser; an answer is an author's to give (not_author); a finding
// no longer standing is answered nothing again (finding_gone); and the
// raiser's review of a later revision clears what it does not carry.
//
// Mutation: let wont_fix clear a blocker — FindingState.Stands false for it,
// or the apply counting a won't-fix blocker off the standing ones — and the
// change's standing blockers drop with an author's answer.
func TestABlockerClearsOnlyByDismissalOrTheRaisersLaterReview(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	res, err := w.Converse(ctx, reviewing(repo, change, platLead, 1, gitlet.VerdictRequestChanges, nil,
		raising(gitlet.SeverityBlocker, 1, "No backoff"), raising(gitlet.SeverityBlocker, 2, "No jitter"),
		raising(gitlet.SeverityBlocker, 3, "No cap"), raising(gitlet.SeverityBlocker, 4, "No deadline")))
	ids := requireReviewed(t, res, err)
	backoff, jitter, capped, deadline := ids[0], ids[1], ids[2], ids[3]
	if got := n.counters(change); got != (counters{reviews: 1, openFindings: 4, openBlockers: 4,
		changesRequested: 1}) {
		t.Fatalf("after the review the conversation counts %+v", got)
	}

	// AN AUTHOR'S ANSWERS LEAVE EVERY BLOCKER STANDING.
	for id, answer := range map[string]gitlet.FindingState{backoff: gitlet.FindingResolved,
		jitter: gitlet.FindingWontFix, capped: gitlet.FindingDisputed} {
		res, err := w.Converse(ctx, answering(repo, change, dev, answer, id))
		requireConversed(t, res, err)
	}
	requireStates(t, n, change, map[string]gitlet.FindingState{backoff: gitlet.FindingResolved,
		jitter: gitlet.FindingWontFix, capped: gitlet.FindingDisputed, deadline: gitlet.FindingOpen})
	if got := n.counters(change); got.openFindings != 1 || got.openBlockers != 4 {
		t.Fatalf("after the author's answers the conversation counts %+v, want 1 open finding and the 4 "+
			"blockers still standing", got)
	}

	// NOBODY ELSE ANSWERS, AND NOBODY ELSE DISMISSES.
	for _, tc := range []struct {
		name    string
		write   gitlet.ConversationWrite
		refusal gitlet.ConversationRefusal
		says    string
	}{
		{"its raiser, answering it resolved", answering(repo, change, platLead, gitlet.FindingResolved, deadline),
			gitlet.ConversationRefusalNotAuthor, `seat "plat-lead" did not write web!1`},
		{"an author who writes the repository, dismissing", answering(repo, change, dev, gitlet.FindingDismissed,
			jitter), gitlet.ConversationRefusalNotRaiser, `seat "dev" did not raise finding ` + jitter},
		{"a reader, dismissing", answering(repo, change, reader, gitlet.FindingDismissed, jitter),
			gitlet.ConversationRefusalNotRaiser, `seat "reader" did not raise finding ` + jitter},
		{"a finding nobody raised", answering(repo, change, dev, gitlet.FindingResolved,
			"019a0c2e-0000-7000-8000-00000000dead"), gitlet.ConversationRefusalFindingGone, "holds no finding"},
	} {
		before := n.end()
		res, err := w.Converse(ctx, tc.write)
		requireConversationRefused(t, n, before, res, err, tc.refusal, tc.says)
	}

	// ITS RAISER AND A MAINTAINER DISMISS, and a dismissed finding is
	// answered nothing again.
	res, err = w.Converse(ctx, answering(repo, change, platLead, gitlet.FindingDismissed, backoff))
	requireConversed(t, res, err)
	res, err = w.Converse(ctx, answering(repo, change, cto, gitlet.FindingDismissed, jitter))
	requireConversed(t, res, err)
	if got := n.counters(change); got.openBlockers != 2 {
		t.Fatalf("after two dismissals the change has %d standing blockers, want 2", got.openBlockers)
	}
	before := n.end()
	res, err = w.Converse(ctx, answering(repo, change, dev, gitlet.FindingResolved, backoff))
	requireConversationRefused(t, n, before, res, err, gitlet.ConversationRefusalFindingGone, "is dismissed")

	// THE RAISER'S LATER REVIEW clears what it does not carry, and a person
	// dismisses the one it carried.
	n.revise(repo, "B", "B2")
	res, err = w.Converse(ctx, reviewing(repo, change, platLead, 2, gitlet.VerdictApprove, []string{deadline}))
	requireReviewed(t, res, err)
	requireStates(t, n, change, map[string]gitlet.FindingState{backoff: gitlet.FindingDismissed,
		jitter: gitlet.FindingDismissed, capped: gitlet.FindingSuperseded, deadline: gitlet.FindingOpen})
	if got := n.counters(change); got != (counters{reviews: 2, openFindings: 1, openBlockers: 1}) {
		t.Fatalf("after the later review the conversation counts %+v, want the carried blocker alone", got)
	}
	res, err = w.Converse(ctx, answering(repo, change, ada, gitlet.FindingDismissed, deadline))
	requireConversed(t, res, err)
	if got := n.counters(change); got.openBlockers != 0 || got.openFindings != 0 {
		t.Fatalf("after a person's dismissal the conversation counts %+v, want nothing standing", got)
	}
}

// A LATER REVIEW SUPERSEDES EVERY STANDING FINDING IT DOES NOT CARRY (rule
// D22): its reviewer's review of a later revision carries the findings of
// theirs that still stand and supersedes every other one they raised on an
// earlier revision — open or answered alike — naming the raiser and the
// review's record as its last answer; another raiser's findings stay theirs;
// and what a review carries is its reviewer's own (not_raiser) and still
// standing (finding_gone).
//
// Mutation: supersede only the open findings, and the resolved one the later
// review left out still stands.
func TestALaterReviewSupersedesEveryStandingFindingItDoesNotCarry(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	res, err := w.Converse(ctx, reviewing(repo, change, platLead, 1, gitlet.VerdictRequestChanges, nil,
		raising(gitlet.SeverityBlocker, 1, "No backoff"), raising(gitlet.SeverityMajor, 2, "No jitter"),
		raising(gitlet.SeverityMinor, 3, "A long line")))
	first := requireReviewed(t, res, err)
	carried, answered, left := first[0], first[1], first[2]
	res, err = w.Converse(ctx, reviewing(repo, change, cto, 1, gitlet.VerdictComment, nil,
		raising(gitlet.SeverityBlocker, 5, "No tests")))
	others := requireReviewed(t, res, err)
	res, err = w.Converse(ctx, conversing(dev, func(cw *gitlet.ConversationWrite) {
		cw.FindingResolve = &gitlet.FindingResolve{RepoID: repo, ChangeID: change, Findings: []gitlet.FindingResolution{
			{FindingID: answered, Resolution: gitlet.FindingResolved, Note: "jitter added", Commit: oidOf("B2")}}}
	}))
	requireConversed(t, res, err)
	n.revise(repo, "B", "B2")

	for _, tc := range []struct {
		name    string
		carries []string
		refusal gitlet.ConversationRefusal
		says    string
	}{
		{"another raiser's finding", []string{others[0]}, gitlet.ConversationRefusalNotRaiser,
			"a review carries its own reviewer's findings alone"},
		{"a finding the change does not hold", []string{"019a0c2e-0000-7000-8000-00000000dead"},
			gitlet.ConversationRefusalFindingGone, "holds no finding"},
	} {
		before := n.end()
		res, err := w.Converse(ctx, reviewing(repo, change, platLead, 2, gitlet.VerdictComment, tc.carries))
		requireConversationRefused(t, n, before, res, err, tc.refusal, tc.says)
	}

	res, err = w.Converse(ctx, reviewing(repo, change, platLead, 2, gitlet.VerdictRequestChanges,
		[]string{carried}, raising(gitlet.SeverityNit, 7, "A name")))
	second := requireReviewed(t, res, err)
	requireStates(t, n, change, map[string]gitlet.FindingState{carried: gitlet.FindingOpen,
		answered: gitlet.FindingSuperseded, left: gitlet.FindingSuperseded, second[0]: gitlet.FindingOpen,
		others[0]: gitlet.FindingOpen})
	if got := n.count(`SELECT COUNT(*) FROM code_findings WHERE id IN (?, ?) AND resolved_by = 'plat-lead'
		AND resolved_by_kind = 'agent' AND resolved_position = ? AND resolution_note = ''
		AND resolved_commit = ''`, answered, left, res.Position.Packed()); got != 2 {
		t.Fatalf("%d superseded findings name their raiser's review as their last answer, want 2", got)
	}
	if got := n.counters(change); got != (counters{reviews: 3, openFindings: 3, openBlockers: 2,
		changesRequested: 1}) {
		t.Fatalf("after the later review the conversation counts %+v", got)
	}

	before := n.end()
	res, err = w.Converse(ctx, reviewing(repo, change, platLead, 2, gitlet.VerdictComment, []string{left}))
	requireConversationRefused(t, n, before, res, err, gitlet.ConversationRefusalFindingGone, "is superseded")
}

// A SAME-REVISION REVIEW SUPERSEDES NOTHING: a reviewer's second review of the
// revision their findings were raised on — one reviewer's two passes over one
// revision — leaves every one of them standing, carried or not, and so does a
// review of an earlier revision than theirs; only a later revision's review
// supersedes.
//
// Mutation: supersede the findings of the revision a review judges as well
// (`<=` for `<` in supersededBy), and the second pass supersedes the first's.
func TestASameRevisionReviewSupersedesNothing(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	res, err := w.Converse(ctx, reviewing(repo, change, platLead, 1, gitlet.VerdictRequestChanges, nil,
		raising(gitlet.SeverityBlocker, 1, "No backoff")))
	first := requireReviewed(t, res, err)
	res, err = w.Converse(ctx, reviewing(repo, change, platLead, 1, gitlet.VerdictRequestChanges, nil,
		raising(gitlet.SeverityMajor, 2, "No jitter")))
	second := requireReviewed(t, res, err)
	requireStates(t, n, change, map[string]gitlet.FindingState{first[0]: gitlet.FindingOpen,
		second[0]: gitlet.FindingOpen})

	n.revise(repo, "B", "B2")
	res, err = w.Converse(ctx, reviewing(repo, change, platLead, 2, gitlet.VerdictComment,
		[]string{first[0], second[0]}, raising(gitlet.SeverityNit, 3, "A name")))
	third := requireReviewed(t, res, err)
	res, err = w.Converse(ctx, reviewing(repo, change, platLead, 1, gitlet.VerdictComment, nil))
	requireReviewed(t, res, err)
	requireStates(t, n, change, map[string]gitlet.FindingState{first[0]: gitlet.FindingOpen,
		second[0]: gitlet.FindingOpen, third[0]: gitlet.FindingOpen})
	if got := n.counters(change); got != (counters{reviews: 4, openFindings: 3, openBlockers: 1,
		changesRequested: 1}) {
		t.Fatalf("the conversation counts %+v, want every finding standing", got)
	}
}

// AN AUTHOR CANNOT REVIEW (rule D21): the change's owner, a seat whose push
// made it an author and a person whose push did are each refused
// not_permitted — an author's review of their own change, its approval above
// all, counts for nothing, so none is taken — and so is a seat below review on
// the repository, naming what it holds and what would admit it; a seat that
// reviews the repository, and a person who did not write the change, review
// it.
//
// Mutation: drop the author check, and the owner's approval of their own
// change lands.
func TestAnAuthorCannotReview(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	c, _ := committerOn(t, n, n.pub)
	pushedBy(t, c, repo, cto, update(featureRef, oidOf("B"), oidOf("B2")))
	pushedBy(t, c, repo, ada, update(featureRef, oidOf("B2"), oidOf("B3")))
	other, err := c.ProposeChange(t.Context(), opening(repo, releaseRef, mainRef, dev))
	requireChanged(t, other, err)
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	for _, tc := range []struct {
		name  string
		asker gitlet.Principal
		says  string
	}{
		{"its owner", dev, `seat "dev" is an author of web!1`},
		{"a seat whose push made it an author", cto, `seat "cto" is an author of web!1`},
		{"a person whose push made them an author", ada, `credential "pat:ada" is an author of web!1`},
		{"a seat below review", reader, `seat "reader" holds read on web!1's repository, and a review needs ` +
			`review: a person can set its role.code.access to review or full_access`},
	} {
		before := n.end()
		res, err := w.Converse(ctx, reviewing(repo, change, tc.asker, 3, gitlet.VerdictApprove, nil))
		requireConversationRefused(t, n, before, res, err, gitlet.ConversationRefusalNotPermitted, tc.says)
	}
	if got := n.count(`SELECT COUNT(*) FROM code_reviews WHERE change_id = ?`, change); got != 0 {
		t.Fatalf("refused reviews wrote %d rows", got)
	}
	res, err := w.Converse(ctx, reviewing(repo, change, platLead, 3, gitlet.VerdictApprove, nil))
	requireReviewed(t, res, err)
	res, err = w.Converse(ctx, reviewing(repo, other.ChangeID, ada, 1, gitlet.VerdictApprove, nil))
	requireReviewed(t, res, err)
	if got := n.count(`SELECT COUNT(*) FROM code_reviews WHERE (change_id = ? AND reviewer = 'plat-lead'
		AND reviewer_kind = 'agent' AND revision = 3) OR (change_id = ? AND reviewer = 'founder'
		AND reviewer_kind = 'person' AND operator_id = 'pat:ada')`, change, other.ChangeID); got != 2 {
		t.Fatalf("%d of the two reviews are kept with their reviewer, want 2", got)
	}
}

// A SEAT THAT LOST THE REPOSITORY WRITES NOTHING OF ITS CONVERSATION: an
// author's answer to a finding, a raiser's dismissal of its own, a thread's
// starter's resolution and an author's ask for review — or its withdrawal —
// admit a writer by what it did on the change, and a seat the deciding chart
// no longer lets read the repository holds none of them: one that left the
// chart, and one whose unit a `visibility: unit` repository no longer reaches
// once the chart moved it. Each such write is refused not_permitted in the
// grant's own sentence and appends nothing, while a person — who maintains
// every repository — and a seat the chart still lets maintain it write on.
//
// Mutation: drop the grant check that precedes the relation rules, and the
// author's answer, the raiser's dismissal, the starter's resolution and the
// author's withdrawal each land.
func TestASeatThatLostTheRepositoryWritesNothingOfItsConversation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		visibility gitlet.Visibility
		// revoke is the chart the writer judges by once the seats lost the
		// repository; why is the end of the read refusal that says so.
		revoke func(chart) chart
		why    string
	}{
		{"the seats left the chart", gitlet.VisibilityCompany, func(c chart) chart {
			delete(c.seats, "dev")
			delete(c.seats, "plat-lead")
			return c
		}, "this seat is no seat of the chart"},
		{"a unit repository no longer reaches the seats", gitlet.VisibilityUnit, func(c chart) chart {
			c.seats["dev"] = gitlet.RosterSeat{Handle: "dev", Tier: gitlet.TierFullAccess, Unit: "sales"}
			c.seats["plat-lead"] = gitlet.RosterSeat{Handle: "plat-lead", Tier: gitlet.TierReview, Unit: "sales"}
			return c
		}, `it is visibility: unit, read by the seats of unit "Engineering" and the leads above it`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := newNode(t)
			s := suiteSettings()
			s.Owner, s.Visibility = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}, tc.visibility
			repo := n.ensure("web", s, t1).RepoID
			n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, gitlet.ZeroOID, oidOf("A")),
				set(featureRef, gitlet.ZeroOID, oidOf("B"))))
			c, _ := committerOn(t, n, n.pub)
			opened, err := c.ProposeChange(t.Context(), opening(repo, featureRef, mainRef, dev))
			requireChanged(t, opened, err)
			change := opened.ChangeID

			var judged atomic.Pointer[chart]
			whole := testChart()
			judged.Store(&whole)
			w, _ := writerOn(t, n, n.pub, func(o *gitlet.ChangeWriterOptions) {
				o.SeatRules = func() (gitlet.SeatRules, bool) {
					return gitlet.SeatRules{Roster: *judged.Load(), Zone: time.UTC}, true
				}
			})
			ctx := t.Context()

			// WHILE THEY READ IT: plat-lead raises two findings, dev starts a
			// thread and asks cto to review.
			res, err := w.Converse(ctx, reviewing(repo, change, platLead, 1, gitlet.VerdictRequestChanges, nil,
				raising(gitlet.SeverityBlocker, 1, "No backoff"), raising(gitlet.SeverityBlocker, 2, "No jitter")))
			ids := requireReviewed(t, res, err)
			thread, err := w.Converse(ctx, saying(repo, change, dev, "", "Why the retry loop?"))
			requireConversed(t, thread, err)
			res, err = w.Converse(ctx, asking(repo, change, dev, "cto"))
			requireConversed(t, res, err)

			// THE CHART TAKES THE REPOSITORY FROM THEM.
			revoked := tc.revoke(testChart())
			judged.Store(&revoked)
			for _, write := range []struct {
				name string
				as   string
				w    gitlet.ConversationWrite
			}{
				{"the author's answer to a finding", "dev",
					answering(repo, change, dev, gitlet.FindingResolved, ids[0])},
				{"the raiser's dismissal of its own finding", "plat-lead",
					answering(repo, change, platLead, gitlet.FindingDismissed, ids[1])},
				{"the starter's resolution of its thread", "dev",
					resolving(repo, change, dev, thread.ID, true)},
				{"the author's withdrawal of an ask", "dev", withdrawing(repo, change, dev, "cto")},
				{"the author's ask for review", "dev", asking(repo, change, dev, "vp-sales")},
			} {
				before := n.end()
				res, err := w.Converse(ctx, write.w)
				requireConversationRefused(t, n, before, res, err, gitlet.ConversationRefusalNotPermitted,
					`seat "`+write.as+`" holds no access to repository "web": `+tc.why)
			}

			// A PERSON, AND A SEAT THE CHART STILL LETS MAINTAIN IT, WRITE ON.
			res, err = w.Converse(ctx, answering(repo, change, ada, gitlet.FindingDismissed, ids[1]))
			requireConversed(t, res, err)
			res, err = w.Converse(ctx, resolving(repo, change, cto, thread.ID, true))
			requireConversed(t, res, err)
			requireStates(t, n, change, map[string]gitlet.FindingState{ids[0]: gitlet.FindingOpen,
				ids[1]: gitlet.FindingDismissed})
			if got, _ := n.reviewer(change, "cto"); got.state != string(gitlet.ReviewerRequested) {
				t.Fatalf("cto's ask is %q after the refused withdrawal, want it still requested", got.state)
			}
		})
	}
}

// A REVIEW WITH ALL ITS FINDINGS IS ONE RECORD: a review raising findings —
// one suggesting the lines to put in their place — lands as ONE record on the
// change's subject, the review's id and every finding's derived from its
// operation id and answered with it; its rows are written whole — the review,
// each finding and its text — and the conversation's counters and what its
// text weighs move by all of it; asked again under its own operation id it is
// the review that landed, appending nothing; and a review past what one write
// of a conversation may carry is refused before anything is read, saying to
// split it into two reviews of the same revision.
//
// Mutation: publish a review's findings as records of their own, and the
// review takes four records.
func TestAReviewWithAllItsFindingsIsOneRecord(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	suggestion := "time.Sleep(backoff)"
	suggesting := raising(gitlet.SeverityMajor, 7, "Sleep between tries")
	suggesting.Body, suggesting.Suggestion = "Two tries back to back hammer it.", &suggestion
	review := reviewing(repo, change, platLead, 1, gitlet.VerdictRequestChanges, nil,
		raising(gitlet.SeverityBlocker, 3, "No backoff"), suggesting, raising(gitlet.SeverityNit, 9, "A name"))
	review.Review.TurnID, review.Review.Model = "019a0c2e-0000-7000-8000-0000000000d3", "model-a"

	before := n.end()
	res, err := w.Converse(ctx, review)
	ids := requireReviewed(t, res, err)
	if got := n.end() - before; got != 1 {
		t.Fatalf("the review appended %d records, want one", got)
	}
	want := []string{gitlet.FindingID(review.OpID, 0), gitlet.FindingID(review.OpID, 1), gitlet.FindingID(review.OpID, 2)}
	if res.ID != gitlet.ReviewID(review.OpID) || !slices.Equal(ids, want) {
		t.Fatalf("the review answered %s with findings %v, want %s with %v, derived from its operation id",
			res.ID, ids, gitlet.ReviewID(review.OpID), want)
	}
	rec := n.recordAt(res.Position)
	decoded, err := gitlet.DecodeMutation(rec)
	if err != nil {
		t.Fatalf("decode the review's record: %v", err)
	}
	got, ok := decoded.(gitlet.Review)
	if !ok || rec.Subject != gitlet.ChangeSubject(change) || rec.Op != gitlet.OpReview || got.ReviewID != res.ID ||
		got.By != platLead || len(got.Findings) != 3 || got.Findings[1].ID != want[1] ||
		got.Findings[1].Suggestion == nil || *got.Findings[1].Suggestion != suggestion {
		t.Fatalf("the review's record is %+v, %+v; want the review and its three findings on %s", rec.RecordEnvelope,
			decoded, gitlet.ChangeSubject(change))
	}
	if got := n.count(`SELECT COUNT(*) FROM code_reviews WHERE id = ? AND change_id = ? AND revision = 1
		AND reviewer = 'plat-lead' AND verdict = 'request_changes' AND findings = 3 AND turn_id = ?
		AND model = 'model-a' AND summary = 'The retry loop.' AND position = ?`, res.ID, change,
		review.Review.TurnID, res.Position.Packed()); got != 1 {
		t.Fatalf("the review's row does not say what it judged")
	}
	if got := n.count(`SELECT COUNT(*) FROM code_findings AS f JOIN code_finding_texts AS x
		ON x.change_id = f.change_id AND x.finding_id = f.id
		WHERE f.review_id = ? AND f.raiser = 'plat-lead' AND f.state = 'open' AND f.revision = 1`,
		res.ID); got != 3 {
		t.Fatalf("%d of the review's findings are kept open with their text, want 3", got)
	}
	if got := n.count(`SELECT COUNT(*) FROM code_finding_texts WHERE finding_id = ? AND suggestion = ?
		AND body = ?`, want[1], suggestion, suggesting.Body); got != 1 {
		t.Fatalf("the suggesting finding's text is not kept whole")
	}
	if got := n.count(`SELECT COUNT(*) FROM code_finding_texts WHERE finding_id IN (?, ?)
		AND suggestion IS NULL`, want[0], want[2]); got != 2 {
		t.Fatalf("a finding suggesting nothing keeps a suggestion")
	}
	if got := n.counters(change); got != (counters{reviews: 1, openFindings: 3, openBlockers: 1,
		changesRequested: 1}) {
		t.Fatalf("the conversation counts %+v after the review", got)
	}
	weighs := len("The retry loop.")
	for _, f := range review.Review.Findings {
		weighs += len(f.Title) + len(f.Body)
		if f.Suggestion != nil {
			weighs += len(*f.Suggestion)
		}
	}
	if got := n.count(`SELECT text_bytes FROM code_conversations WHERE change_id = ?`, change); got != weighs {
		t.Fatalf("the conversation weighs %d bytes, want the %d the review's text holds", got, weighs)
	}

	// ASKED AGAIN, the review that landed.
	before = n.end()
	again, err := w.Converse(ctx, review)
	if err != nil || again.Outcome != statelog.OutcomeApplied || again.ID != res.ID ||
		again.Position != res.Position || n.end() != before {
		t.Fatalf("the review asked again answered %+v, %v and appended %d records; want review %s at %s",
			again, err, n.end()-before, res.ID, res.Position)
	}

	// PAST A RECORD: refused before anything is read.
	wide := strings.Repeat("x", gitlet.MaxSuggestionBytes)
	var many []gitlet.Finding
	for i := range gitlet.MaxFindingsPerReview {
		f := raising(gitlet.SeverityMinor, i+1, "A line")
		f.Suggestion = &wide
		many = append(many, f)
	}
	before = n.end()
	res, err = w.Converse(ctx, reviewing(repo, change, platLead, 1, gitlet.VerdictComment, nil, many...))
	if !errors.Is(err, gitlet.ErrInvalidProposal) || !strings.Contains(err.Error(),
		"split it into two reviews of the same revision") || n.end() != before {
		t.Fatalf("a review past MaxConversationBytes answered %+v, %v; want ErrInvalidProposal saying to split it",
			res, err)
	}
}

// THE WIDEST REVIEW ONE WRITE TAKES IS ONE RECORD: a review of
// MaxFindingsPerReview findings whose suggestions bring it to the last bytes
// ConversationWrite.Validate admits — MaxConversationBytes, the record less
// the room it keeps for whom the change concerns — lands as one record and is
// applied whole, and its record still leaves MaxRoutingBytes of
// MaxRecordBytes free: the text a write carries never takes the room a
// change's record keeps for the wakes it causes, and no review its asker was
// told to send is lost to the record's own cap.
//
// Mutation: bound a write by MaxRecordBytes, and the widest review admitted
// leaves its record no room for whom the change concerns.
func TestTheWidestReviewOneWriteTakesIsOneRecord(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	widest := func(width int) gitlet.ConversationWrite {
		text := strings.Repeat("s", width)
		findings := make([]gitlet.Finding, gitlet.MaxFindingsPerReview)
		for i := range findings {
			findings[i] = raising(gitlet.SeverityMinor, i+1, "A line")
			findings[i].Suggestion = &text
		}
		return reviewing(repo, change, platLead, 1, gitlet.VerdictComment, nil, findings...)
	}
	// THE WIDEST SUGGESTION VALIDATE ADMITS, searched for by halves: a
	// review is admitted up to some width and refused past it.
	if widest(gitlet.MaxSuggestionBytes).Validate() == nil {
		t.Fatalf("a review of %d suggestions at MaxSuggestionBytes fits MaxConversationBytes: the bound this "+
			"case measures is no bound", gitlet.MaxFindingsPerReview)
	}
	width, refused := 0, gitlet.MaxSuggestionBytes
	for refused-width > 1 {
		if mid := (width + refused) / 2; widest(mid).Validate() == nil {
			width = mid
		} else {
			refused = mid
		}
	}
	before := n.end()
	res, err := w.Converse(t.Context(), widest(width))
	ids := requireReviewed(t, res, err)
	if got := n.end() - before; got != 1 || len(ids) != gitlet.MaxFindingsPerReview {
		t.Fatalf("the widest review appended %d records raising %d findings, want one raising %d", got, len(ids),
			gitlet.MaxFindingsPerReview)
	}
	if got := n.count(`SELECT COUNT(*) FROM code_finding_texts WHERE change_id = ? AND length(suggestion) = ?`,
		change, width); got != gitlet.MaxFindingsPerReview {
		t.Fatalf("%d of the widest review's suggestions are kept whole, want %d", got, gitlet.MaxFindingsPerReview)
	}
	_, body, _, held, err := n.records.At(t.Context(), res.Position.Seq)
	if err != nil || !held {
		t.Fatalf("read the widest review's record: %v (held: %v)", err, held)
	}
	if room := gitlet.MaxRecordBytes - len(body); room < gitlet.MaxRoutingBytes {
		t.Fatalf("the widest review's record is %d bytes, leaving %d of MaxRecordBytes (%d) — under the "+
			"MaxRoutingBytes (%d) a change's record keeps for whom the change concerns", len(body), room,
			gitlet.MaxRecordBytes, gitlet.MaxRoutingBytes)
	}
}

// A REQUEST FOR CHANGES STANDS UNTIL ITS REVIEWER JUDGES AGAIN: a reviewer's
// standing verdict is the latest of their reviews that judges — approve or
// request_changes — kept on their participant row; a review that comments
// judges nothing and leaves it as it was; and the change counts the reviewers
// whose standing verdict asks for changes, one a reviewer however many times
// they asked.
//
// Mutation: move the standing verdict on a review that comments, and the
// comment withdraws the request for changes it followed.
func TestARequestForChangesStandsUntilItsReviewerJudgesAgain(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	review := func(asker gitlet.Principal, verdict gitlet.Verdict) {
		t.Helper()
		res, err := w.Converse(ctx, reviewing(repo, change, asker, 1, verdict, nil))
		requireReviewed(t, res, err)
	}
	standing := func(handle string) string {
		t.Helper()
		var verdict string
		if err := n.db.Reader().Read(ctx, func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, `SELECT verdict FROM code_participants WHERE change_id = ?
				AND handle = ?`, change, handle).Scan(&verdict)
		}); err != nil {
			t.Fatalf("read %s's standing verdict: %v", handle, err)
		}
		return verdict
	}
	for i, step := range []struct {
		asker     gitlet.Principal
		verdict   gitlet.Verdict
		requested int
		platLead  string
	}{
		{platLead, gitlet.VerdictRequestChanges, 1, "request_changes"},
		{platLead, gitlet.VerdictRequestChanges, 1, "request_changes"},
		{platLead, gitlet.VerdictComment, 1, "request_changes"},
		{cto, gitlet.VerdictRequestChanges, 2, "request_changes"},
		{platLead, gitlet.VerdictApprove, 1, "approve"},
		{platLead, gitlet.VerdictComment, 1, "approve"},
		{cto, gitlet.VerdictApprove, 0, "approve"},
	} {
		review(step.asker, step.verdict)
		if got := n.counters(change).changesRequested; got != step.requested {
			t.Fatalf("step %d: the change counts %d requests for changes, want %d", i, got, step.requested)
		}
		if got := standing("plat-lead"); got != step.platLead {
			t.Fatalf("step %d: plat-lead's standing verdict is %q, want %q", i, got, step.platLead)
		}
	}
}

// A REPLY TO A FINDING IS IN THE FINDING'S THREAD: a comment naming a finding
// of the change joins the thread the finding names — whatever state the
// finding is in — and is answered there, while a finding of another change,
// or none, is no thread to reply to (thread_gone); and a finding's thread is
// answered by the finding's own answer, never resolved as a comment's is.
//
// Mutation: let a reply to a reply in a finding's thread through, and a
// thread forms two levels deep.
func TestAReplyToAFindingIsInTheFindingsThread(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	c, _ := committerOn(t, n, n.pub)
	other, err := c.ProposeChange(t.Context(), opening(repo, releaseRef, mainRef, dev))
	requireChanged(t, other, err)
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	res, err := w.Converse(ctx, reviewing(repo, change, platLead, 1, gitlet.VerdictRequestChanges, nil,
		raising(gitlet.SeverityBlocker, 1, "No backoff")))
	finding := requireReviewed(t, res, err)[0]
	res, err = w.Converse(ctx, reviewing(repo, other.ChangeID, platLead, 1, gitlet.VerdictComment, nil,
		raising(gitlet.SeverityNit, 1, "A name")))
	elsewhere := requireReviewed(t, res, err)[0]

	replying := func(asker gitlet.Principal, finding, body string) gitlet.ConversationWrite {
		return conversing(asker, func(cw *gitlet.ConversationWrite) {
			cw.Comment = &gitlet.Comment{RepoID: repo, ChangeID: change, FindingID: finding, Body: body}
		})
	}
	reply, err := w.Converse(ctx, replying(dev, finding, "It backs off now."))
	requireConversed(t, reply, err)
	if got := n.count(`SELECT COUNT(*) FROM code_comments WHERE id = ? AND thread_id = ? AND finding_id = ?
		AND parent_id = ''`, reply.ID, finding, finding); got != 1 {
		t.Fatalf("the reply is not in the finding's thread")
	}
	for _, tc := range []struct {
		name  string
		write gitlet.ConversationWrite
		says  string
	}{
		{"a reply to another change's finding", replying(dev, elsewhere, "Hm."), "holds no finding " + elsewhere},
		{"a reply to a reply in a finding's thread", saying(repo, change, dev, reply.ID, "Hm."),
			"is a reply in thread " + finding},
		{"a resolution of a finding's thread", resolving(repo, change, platLead, finding, true),
			"holds no thread " + finding},
	} {
		before := n.end()
		res, err := w.Converse(ctx, tc.write)
		requireConversationRefused(t, n, before, res, err, gitlet.ConversationRefusalThreadGone, tc.says)
	}
	res, err = w.Converse(ctx, answering(repo, change, platLead, gitlet.FindingDismissed, finding))
	requireConversed(t, res, err)
	reply, err = w.Converse(ctx, replying(cto, finding, "Agreed."))
	requireConversed(t, reply, err)
}

// A WRITE OF A REVIEW OR AN ANSWER NO WRITER COULD DECIDE IS REFUSED BEFORE
// ANYTHING IS READ, as every write of a conversation is
// (TestAConversationWriteIsRefusedBeforeAnythingIsRead), naming its field:
// the ids the writer stamps sent by an asker, a review of no revision or that
// judges and says nothing, a finding its bounds refuse, carried or answered
// ids that are no finding's or are named twice, an answer no write gives, a
// reply to a root and a finding both.
//
// Mutations: let a finding's line run backwards, and it is decided; let one
// run past gitlet.MaxAnchorLine, and it is decided.
func TestAReviewAndAnAnswerAreRefusedBeforeAnythingIsRead(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	finding := "019a0c2e-0000-7000-8000-0000000000aa"
	withReview := func(set func(r *gitlet.Review)) gitlet.ConversationWrite {
		cw := reviewing(repo, change, platLead, 1, gitlet.VerdictRequestChanges, nil,
			raising(gitlet.SeverityBlocker, 1, "No backoff"))
		set(cw.Review)
		return cw
	}
	withFinding := func(set func(f *gitlet.Finding)) gitlet.ConversationWrite {
		return withReview(func(r *gitlet.Review) { set(&r.Findings[0]) })
	}
	withAnswer := func(set func(a *gitlet.FindingResolve)) gitlet.ConversationWrite {
		cw := answering(repo, change, dev, gitlet.FindingResolved, finding)
		set(cw.FindingResolve)
		return cw
	}
	many := make([]gitlet.Finding, gitlet.MaxFindingsPerReview+1)
	for i := range many {
		many[i] = raising(gitlet.SeverityNit, i+1, "A line")
	}
	for name, tc := range map[string]struct {
		w     gitlet.ConversationWrite
		field string
	}{
		"a review's id sent":      {withReview(func(r *gitlet.Review) { r.ReviewID = finding }), "review.review_id"},
		"its reviewer sent":       {withReview(func(r *gitlet.Review) { r.By = platLead }), "review.by"},
		"a review of no revision": {withReview(func(r *gitlet.Review) { r.Revision = 0 }), "review.revision"},
		"a verdict this build does not know": {withReview(func(r *gitlet.Review) { r.Verdict = "lgtm" }),
			"review.verdict"},
		"a comment saying nothing": {withReview(func(r *gitlet.Review) {
			r.Verdict, r.Summary, r.Findings = gitlet.VerdictComment, " ", nil
		}), "review"},
		"a summary past its bound": {withReview(func(r *gitlet.Review) {
			r.Summary = strings.Repeat("s", gitlet.MaxReviewSummaryBytes+1)
		}), "review.summary"},
		"findings past the bound": {withReview(func(r *gitlet.Review) { r.Findings = many }), "review.findings"},
		"a finding's id sent":     {withFinding(func(f *gitlet.Finding) { f.ID = finding }), "review.findings[0]"},
		"a finding's line past any file's": {withFinding(func(f *gitlet.Finding) { f.LineEnd = gitlet.MaxAnchorLine + 1 }),
			"review.findings[0]"},
		"a finding's lines backwards": {withFinding(func(f *gitlet.Finding) { f.LineStart, f.LineEnd = 5, 4 }),
			"review.findings[0]"},
		"a finding of no severity this build knows": {withFinding(func(f *gitlet.Finding) { f.Severity = "fatal" }),
			"review.findings[0]"},
		"a finding of no category this build knows": {withFinding(func(f *gitlet.Finding) { f.Category = "vibes" }),
			"review.findings[0]"},
		"a finding's title of two lines": {withFinding(func(f *gitlet.Finding) { f.Title = "No\nbackoff" }),
			"review.findings[0]"},
		"a finding's title past its bound": {withFinding(func(f *gitlet.Finding) {
			f.Title = strings.Repeat("t", gitlet.MaxFindingTitleBytes+1)
		}), "review.findings[0]"},
		"a suggestion past its bound": {withFinding(func(f *gitlet.Finding) {
			long := strings.Repeat("s", gitlet.MaxSuggestionBytes+1)
			f.Suggestion = &long
		}), "review.findings[0]"},
		"a finding's commit that is no object id": {withFinding(func(f *gitlet.Finding) { f.CommitOID = "main" }),
			"review.findings[0]"},
		"a carried id that is no finding's": {withReview(func(r *gitlet.Review) { r.Carries = []string{"web!1"} }),
			"review.carries[0]"},
		"a finding carried twice": {withReview(func(r *gitlet.Review) { r.Carries = []string{finding, finding} }),
			"review.carries[1]"},
		"a model past its bound": {withReview(func(r *gitlet.Review) {
			r.Model = strings.Repeat("m", gitlet.MaxProvenanceBytes+1)
		}), "review.model"},
		"an answer naming nothing": {withAnswer(func(a *gitlet.FindingResolve) { a.Findings = nil }),
			"finding_resolve.findings"},
		"who answers sent": {withAnswer(func(a *gitlet.FindingResolve) { a.By = dev }), "finding_resolve.by"},
		"an answer no write gives": {withAnswer(func(a *gitlet.FindingResolve) {
			a.Findings[0].Resolution = gitlet.FindingSuperseded
		}), "finding_resolve.findings[0].resolution"},
		"a finding answered twice": {withAnswer(func(a *gitlet.FindingResolve) {
			a.Findings = append(a.Findings, a.Findings[0])
		}), "finding_resolve.findings[1].finding_id"},
		"an answer's commit that is no object id": {withAnswer(func(a *gitlet.FindingResolve) {
			a.Findings[0].Commit = "HEAD"
		}), "finding_resolve.findings[0].commit"},
		"an answer's note past its bound": {withAnswer(func(a *gitlet.FindingResolve) {
			a.Findings[0].Note = strings.Repeat("n", gitlet.MaxReviewNoteBytes+1)
		}), "finding_resolve.findings[0].note"},
		"a reply to a root and a finding both": {conversing(dev, func(cw *gitlet.ConversationWrite) {
			cw.Comment = &gitlet.Comment{RepoID: repo, ChangeID: change, ParentID: finding, FindingID: finding,
				Body: "Hm."}
		}), "comment.finding_id"},
	} {
		before := n.end()
		res, err := w.Converse(t.Context(), tc.w)
		if !errors.Is(err, gitlet.ErrInvalidProposal) || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: Converse = %+v, %v; want ErrInvalidProposal naming %s", name, res, err, tc.field)
		}
		if got := n.end(); got != before {
			t.Errorf("%s: an invalid write appended %d records", name, got-before)
		}
	}
}

// THE STANDING STATES THE DECIDE SEEKS ARE THE ONES THAT STAND: the list a
// raiser's standing findings are read by, one seek a state, is exactly the
// states FindingState.Stands names, so a review never supersedes past a
// finding it did not read, nor counts one that no longer stands.
func TestTheStandingStatesAreTheOnesThatStand(t *testing.T) {
	t.Parallel()
	var want []string
	for _, s := range gitlet.FindingStates() {
		if s.Stands() {
			want = append(want, string(s))
		}
	}
	if got := gitlet.StandingStates(); !slices.Equal(got, want) {
		t.Fatalf("the decide seeks a raiser's standing findings in %v, and the standing states are %v", got, want)
	}
}
