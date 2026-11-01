package code_test

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
)

// WHOM A CHANGE RECORD CONCERNS (concerns.go): every record about a change
// carries the snapshot of whom it concerns, stamped by the writer that decides
// it, and the notice and the wake it causes are read from that alone.

// routingAt is the routings the record at p on n's log carries.
func routingAt(t *testing.T, n *node, p statelog.Position) []*code.ChangeRouting {
	t.Helper()
	payload, err := code.DecodeMutation(n.recordAt(p))
	if err != nil {
		t.Fatalf("decode the payload at %s: %v", p, err)
	}
	return code.RoutingsOf(payload)
}

// heard is who concerns names and hears, each as "handle:reason".
func heard(concerns []code.Concern) []string {
	var out []string
	for _, c := range concerns {
		if c.Hears() {
			out = append(out, c.Handle+":"+string(c.Reason))
		}
	}
	return out
}

// THE CODE REASONS ARE IN PRECEDENCE ORDER — the order D11 decided, written
// once as the list [code.Concerns] offers them in — and the candidates come
// back in it: a routing with a handle of its own in every role names each in
// the list's order, one handle in every role hears under the first reason
// alone, and across the routings of one record (a push's) a handle hears
// under its strongest reason, whichever change offered it. Addressed is the
// nine asks and re_review; the default primary reasons are another list. The
// gate's own reasons sit where M5-D26 puts them — a failed check after a
// request for changes, a broken target right after it, a removal from the
// queue after a conflict — each naming the handles the gate adds: an
// enqueuer, the owning unit's lead.
//
// Mutation: swap two reasons in code.Reasons, or offer a role under another
// reason, and the order or the reason a handle hears under is wrong.
func TestTheCodeReasonsAreInPrecedenceOrder(t *testing.T) {
	t.Parallel()
	want := []code.Reason{"mention", "review_requested", "changes_requested", "checks_failed", "target_broken",
		"conflicted", "dequeued", "finding_replied", "ready_to_merge", "merged", "review_submitted", "re_review",
		"thread", "watcher", "lead_fallback"}
	if got := code.Reasons(); !slices.Equal(got, want) {
		t.Fatalf("the reasons are %v, want D11's precedence %v", got, want)
	}
	asks := []code.Reason{"mention", "review_requested", "changes_requested", "checks_failed", "target_broken",
		"conflicted", "dequeued", "finding_replied", "ready_to_merge", "re_review"}
	for _, r := range code.Reasons() {
		if !r.Valid() {
			t.Errorf("%q is listed and not valid", r)
		}
		if r.Addressed() != slices.Contains(asks, r) {
			t.Errorf("%q addressed = %v, want %v", r, r.Addressed(), slices.Contains(asks, r))
		}
	}
	for _, r := range code.Reasons() {
		if r.WakesActor() != (r == code.ReasonChecksFailed) {
			t.Errorf("%q wakes its actor = %v: only a failed check is news to who made it", r, r.WakesActor())
		}
	}
	unaddressed := 0
	for _, r := range code.DefaultPrimaryReasons {
		if !r.Valid() {
			t.Errorf("%q leads an inbox and is not a reason", r)
		}
		if !r.Addressed() {
			unaddressed++
		}
	}
	if unaddressed == 0 {
		t.Error("every reason that leads an inbox also asks something: the two lists were folded into one")
	}

	// A HANDLE OF ITS OWN IN EVERY ROLE, the authors under each verdict and
	// turn that tells them.
	every := func(set func(r *code.ChangeRouting)) *code.ChangeRouting {
		r := &code.ChangeRouting{ChangeID: "c-1", Mentions: []string{"m"}, Requested: []string{"rq"},
			PathOwners: []string{"po"}, Raisers: []string{"rs"}, ReadyFor: []string{"rf"},
			ReReview: []string{"rr"}, ThreadParticipants: []string{"tp"}, Watchers: []string{"w"},
			LeadFallback: "lead", Authors: []string{"a"}, Enqueuer: "e", Lead: "l"}
		set(r)
		return r
	}
	for name, tc := range map[string]struct {
		set     func(r *code.ChangeRouting)
		authors code.Reason
		// gate is each handle the gate adds beside the ten, as
		// "handle:reason".
		gate []string
	}{
		"changes requested": {func(r *code.ChangeRouting) { r.Verdict = code.VerdictRequestChanges },
			code.ReasonChangesRequested, nil},
		"conflicted": {func(r *code.ChangeRouting) { r.Conflicted = true }, code.ReasonConflicted, nil},
		"merged":     {func(r *code.ChangeRouting) { r.Merged = true }, code.ReasonMerged, nil},
		"review submitted": {func(r *code.ChangeRouting) { r.Verdict = code.VerdictApprove },
			code.ReasonReviewSubmitted, nil},
		"checks failed": {func(r *code.ChangeRouting) { r.ChecksFailed = true }, code.ReasonChecksFailed, nil},
		"target broken": {func(r *code.ChangeRouting) { r.Broken = true }, code.ReasonTargetBroken,
			[]string{"l:target_broken"}},
		"dequeued": {func(r *code.ChangeRouting) { r.Dequeued = true }, code.ReasonDequeued,
			[]string{"e:dequeued"}},
	} {
		got := code.Concerns([]*code.ChangeRouting{every(tc.set)})
		last := -1
		for _, c := range got {
			at := slices.Index(want, c.Reason)
			if at < last {
				t.Errorf("%s: %s hears %s after a later reason: %v", name, c.Handle, c.Reason, heard(got))
			}
			last = at
			if c.Handle == "a" && c.Reason != tc.authors {
				t.Errorf("%s: the authors hear %s, want %s", name, c.Reason, tc.authors)
			}
			if (c.Handle == "e" || c.Handle == "l") && !slices.Contains(tc.gate, c.Handle+":"+string(c.Reason)) {
				t.Errorf("%s: %s hears %s, want %v", name, c.Handle, c.Reason, tc.gate)
			}
		}
		if len(got) != 10+len(tc.gate) {
			t.Errorf("%s: %d concerns (%v), want one for each of the ten handles and %v", name, len(got),
				heard(got), tc.gate)
		}
	}

	// ONE HANDLE IN EVERY ROLE hears once, under the first reason.
	one := every(func(r *code.ChangeRouting) {
		for _, list := range []*[]string{&r.Mentions, &r.Requested, &r.PathOwners, &r.Authors, &r.Raisers,
			&r.ReadyFor, &r.ReReview, &r.ThreadParticipants, &r.Watchers} {
			*list = []string{"x"}
		}
		r.LeadFallback, r.Merged, r.Conflicted, r.Verdict = "x", true, true, code.VerdictRequestChanges
		r.Enqueuer, r.Lead, r.ChecksFailed, r.Broken, r.Dequeued = "x", "x", true, true, true
	})
	if got := heard(code.Concerns([]*code.ChangeRouting{one})); !slices.Equal(got, []string{"x:mention"}) {
		t.Fatalf("one handle in every role hears %v, want once, as mentioned", got)
	}

	// ACROSS A RECORD'S ROUTINGS: merged by the first, mentioned by the
	// second, told of the second.
	merged := &code.ChangeRouting{ChangeID: "c-1", Authors: []string{"x"}, Merged: true}
	opened := &code.ChangeRouting{ChangeID: "c-2", Mentions: []string{"x"}}
	got := code.Concerns([]*code.ChangeRouting{merged, opened})
	if len(got) != 1 || got[0].Reason != code.ReasonMention || got[0].Change != opened {
		t.Fatalf("one handle across two routings hears %v, want once, mentioned on c-2", heard(got))
	}
}

// EVERY CHANGE RECORD CARRIES ITS ROUTING: an open, an ask for review, a
// comment, a revision's facts, a review, an answer to findings, a close and a
// reopen each land with the snapshot of whom they concern, naming their
// change — the open by the number its apply gave it — and the audience its op
// routes by: the open's mentions held to the chart, which says who is a
// person; the reviewers newly asked; a comment's mentions, who join the
// change's participants; the authors a review tells, by its verdict; the
// raiser an answer replies to; and the participants a close tells. Nobody hears
// of their own write.
//
// Mutation: stamp no routing on one of the decides, or read the watchers off
// another role, and the walk fails at that record.
func TestEveryChangeRecordCarriesItsRouting(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	repo := changeRepo(t, n, "web")
	open := opening(repo, featureRef, mainRef, dev)
	open.Open.Mentions = []string{"founder", "nobody-here"}
	opened, err := c.ProposeChange(t.Context(), open)
	requireChanged(t, opened, err)
	change := opened.ChangeID
	w, _ := writerOn(t, n, n.pub)

	one := func(what string, p statelog.Position) *code.ChangeRouting {
		t.Helper()
		routings := routingAt(t, n, p)
		if len(routings) != 1 || routings[0].ChangeID != change || routings[0].ChangeRef != "web!1" ||
			routings[0].Owner != "dev" {
			t.Fatalf("the %s's record carries %+v, want one routing of web!1, owned by dev", what, routings)
		}
		return routings[0]
	}
	r := one("open", opened.Position)
	if !slices.Equal(r.Mentions, []string{"founder"}) || !slices.Equal(r.Persons, []string{"founder"}) ||
		r.Actor != "dev" {
		t.Fatalf("the open's routing is %+v, want founder mentioned — a person — and nobody the chart lacks", r)
	}

	asked := converse(t, w, asking(repo, change, dev, "plat-lead"))
	if r := one("ask", asked.Position); !slices.Equal(r.Requested, []string{"plat-lead"}) {
		t.Fatalf("the ask's routing requests %v, want plat-lead", r.Requested)
	}
	again := converse(t, w, asking(repo, change, dev, "plat-lead", "cto"))
	if r := one("second ask", again.Position); !slices.Equal(r.Requested, []string{"cto"}) {
		t.Fatalf("the second ask's routing requests %v, want cto alone: plat-lead was asked already", r.Requested)
	}

	say := saying(repo, change, ada, "", "Why retry twice, @reader?")
	say.Comment.Mentions = []string{"reader"}
	said := converse(t, w, say)
	if r := one("comment", said.Position); !slices.Equal(r.Mentions, []string{"reader"}) ||
		len(r.ThreadParticipants) != 0 {
		t.Fatalf("the comment's routing is %+v, want reader mentioned in a thread of its own", r)
	}
	if got := n.count(`SELECT COUNT(*) FROM code_participants WHERE change_id = ? AND handle = 'reader'`,
		change); got != 1 {
		t.Fatalf("reader, mentioned, is %d participants of the change, want one", got)
	}
	reply := converse(t, w, saying(repo, change, dev, said.ID, "Because the first one races."))
	if r := one("reply", reply.Position); !slices.Equal(r.ThreadParticipants, []string{"founder"}) ||
		!slices.Equal(heard(code.Concerns([]*code.ChangeRouting{r})), []string{"founder:thread"}) {
		t.Fatalf("the reply's routing names %v in its thread, hearing %v; want ada's seat told, the replier not",
			r.ThreadParticipants, heard(code.Concerns([]*code.ChangeRouting{r})))
	}

	facts := converse(t, w, n.patched(repo, change, 1, "B", oidOf("the retry diff")))
	if r := one("revise", facts.Position); r.Actor != "" {
		t.Fatalf("the home's revise names %q as its actor, want nobody", r.Actor)
	}

	reviewed := converse(t, w, reviewing(repo, change, platLead, 1, code.VerdictRequestChanges, nil,
		raising(code.SeverityBlocker, 7, "the retry races")))
	r = one("review", reviewed.Position)
	if got := heard(code.Concerns([]*code.ChangeRouting{r})); !slices.Equal(got, []string{"dev:changes_requested"}) {
		t.Fatalf("the review tells %v, want its author asked for changes", got)
	}
	answered := converse(t, w, answering(repo, change, dev, code.FindingDisputed, reviewed.FindingIDs...))
	if r := one("answer", answered.Position); !slices.Equal(r.Raisers, []string{"plat-lead"}) {
		t.Fatalf("the answer replies to %v, want the raiser plat-lead", r.Raisers)
	}

	closed, err := c.ProposeChange(t.Context(), closing(repo, change, dev, 1))
	requireChanged(t, closed, err)
	r = one("close", closed.Position)
	if want := []string{"cto", "dev", "founder", "plat-lead", "reader"}; !slices.Equal(r.Watchers, want) {
		t.Fatalf("the close tells %v, want the change's participants %v", r.Watchers, want)
	}
	reopened, err := c.ProposeChange(t.Context(), reopening(repo, change, dev, closed.Version))
	requireChanged(t, reopened, err)
	if r := one("reopen", reopened.Position); !slices.Contains(heard(code.Concerns([]*code.ChangeRouting{r})),
		"founder:watcher") {
		t.Fatalf("the reopen tells %v, want the participants", heard(code.Concerns([]*code.ChangeRouting{r})))
	}
}

// A PUSH THAT MOVES NO CHANGE CONCERNS NOBODY: a push that moves a change's head
// — its next revision — and one to a branch no change uses land with no
// routing at all, and nobody hears of either; the push that sets the target
// to the change's head MERGES it, and its record tells the change's authors —
// but the pusher, who made the merge, whatever they wrote of it.
//
// Mutation: route a head move as the change's authors, or drop the pusher as
// the merge's actor, and the case fails.
func TestAPushThatMovesNoChangeConcernsNobody(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	c, _ := committerOn(t, n, n.pub)
	for name, p := range map[string]code.Proposal{
		"a head move":             proposal(repo, update(featureRef, oidOf("B"), oidOf("B2"))),
		"a branch no change uses": proposal(repo, update(releaseRef, oidOf("R"), oidOf("R2"))),
	} {
		res, err := c.Propose(t.Context(), p)
		if err != nil || res.Position.IsZero() {
			t.Fatalf("%s: Propose answered %+v, %v", name, res, err)
		}
		concerns, err := code.Candidates(n.recordAt(res.Position))
		if err != nil || len(concerns) != 0 {
			t.Fatalf("%s concerns %v (%v), want nobody", name, heard(concerns), err)
		}
	}
	res, err := c.Propose(t.Context(), proposal(repo, update(mainRef, oidOf("A"), oidOf("B2"))))
	if err != nil || res.Position.IsZero() {
		t.Fatalf("the merging push answered %+v, %v", res, err)
	}
	routings := routingAt(t, n, res.Position)
	if len(routings) != 1 || routings[0].ChangeID != change || !routings[0].Merged || routings[0].Actor != "ada" {
		t.Fatalf("the merging push carries %+v, want web!1 merged by ada", routings)
	}
	if got := heard(code.Concerns(routings)); !slices.Equal(got, []string{"dev:merged"}) {
		t.Fatalf("the merge tells %v, want its owner, and not ada, who pushed the head and merged it", got)
	}
}

// READY TO MERGE IS ANNOUNCED ONLY BY A MERGEABILITY RECORD: a draft change
// whose facts and clean merge are recorded becomes ready the moment its draft
// is undone and an eligible reviewer approves it — two records on two
// subjects, the repository's and the change's, either of which may complete
// it. Neither announces it, whichever lands last: decided from two snapshots,
// a pair that land at once each see the other unapplied, so a transition both
// complete would be seen by neither. Nor does a check of its head that
// passes, on a third subject, the repository's checks (M5-D26). The home's
// refresh that follows stores the answer going from not ready to ready, and
// its record alone tells whoever may merge it — dev, its author, under a gate
// any writer merges by; a merge of a later tip that finds it ready still
// tells nobody.
//
// Mutation: announce from a review that completes the readiness (judge it in
// decideReview) → the first case's review carries ready_for; from a lifecycle
// record → the second case's undraft does; from a passing check's ending →
// the check's record does.
func TestReadyToMergeIsAnnouncedOnlyByAMergeabilityRecord(t *testing.T) {
	t.Parallel()
	for _, approvalLast := range []bool{true, false} {
		n := newNode(t)
		n.chart = testChart()
		c, _ := committerOn(t, n, n.pub)
		repo := changeRepo(t, n, "web")
		open := opening(repo, featureRef, mainRef, dev)
		open.Open.Draft = true
		opened, err := c.ProposeChange(t.Context(), open)
		requireChanged(t, opened, err)
		change := opened.ChangeID
		w, _ := writerOn(t, n, n.pub)
		cw, _, _, _ := checksWriterOn(t, n)
		converse(t, w, n.patched(repo, change, 1, "B", oidOf("the retry diff")))
		merged := converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, code.MergeClean))
		if stored := n.storedReadiness(change); stored.ready {
			t.Fatalf("a draft nobody approved is stored ready")
		}

		approve := func() statelog.Position {
			return converse(t, w, reviewing(repo, change, platLead, 1, code.VerdictApprove, nil)).Position
		}
		undraft := func() statelog.Position {
			res, err := c.ProposeChange(t.Context(), writing(dev, 1, func(p *code.ChangeProposal) {
				p.Draft = &code.ChangeDraft{RepoID: repo, ChangeID: change}
			}))
			requireChanged(t, res, err)
			return res.Position
		}
		var completing []statelog.Position
		if approvalLast {
			completing = []statelog.Position{undraft(), approve()}
		} else {
			completing = []statelog.Position{approve(), undraft()}
		}
		checked := runCheck(t, cw, code.CheckKey{RepoID: repo, OID: oidOf("B"), Target: mainRef, Name: "unit",
			Attempt: 1}, change, 1, code.CheckStatePassed, code.CheckReasonExit)
		for _, p := range append(completing, merged.Position, checked) {
			for _, r := range routingAt(t, n, p) {
				if len(r.ReadyFor) != 0 {
					t.Fatalf("approval last %v: the record at %s announces web!1 ready to %v", approvalLast, p,
						r.ReadyFor)
				}
			}
		}
		refresh := converse(t, w, code.ConversationWrite{OpID: statelog.NewOpID(harnessAt, ""),
			Mergeability: &code.Mergeability{RepoID: repo, ChangeID: change, Revision: 1, HeadOID: oidOf("B"),
				TargetRef: mainRef, Refresh: true, Node: n.id}})
		got := heard(code.Concerns(routingAt(t, n, refresh.Position)))
		if !slices.Equal(got, []string{"dev:ready_to_merge"}) {
			t.Fatalf("approval last %v: the refresh tells %v, want the owner that web!1 is ready", approvalLast, got)
		}
		n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, oidOf("A"), oidOf("A2"))))
		later := converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, code.MergeClean))
		if got := heard(code.Concerns(routingAt(t, n, later.Position))); len(got) != 0 {
			t.Fatalf("a merge of a later tip, ready still, tells %v, want nobody", got)
		}
	}
}
