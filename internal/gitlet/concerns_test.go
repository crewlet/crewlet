package gitlet_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// WHOM A CHANGE RECORD CONCERNS (concerns.go): every record about a change
// carries the snapshot of whom it concerns, stamped by the writer that decides
// it, and the notice and the wake it causes are read from that alone.

// routingAt is the routings the record at p on n's log carries.
func routingAt(t *testing.T, n *node, p statelog.Position) []*gitlet.ChangeRouting {
	t.Helper()
	payload, err := gitlet.DecodeMutation(n.recordAt(p))
	if err != nil {
		t.Fatalf("decode the payload at %s: %v", p, err)
	}
	return gitlet.RoutingsOf(payload)
}

// heard is who concerns names and hears, each as "handle:reason".
func heard(concerns []gitlet.Concern) []string {
	var out []string
	for _, c := range concerns {
		if c.Hears() {
			out = append(out, c.Handle+":"+string(c.Reason))
		}
	}
	return out
}

// THE CODE REASONS ARE IN PRECEDENCE ORDER — D11's, written once as the list
// [gitlet.Concerns] offers them in — and the candidates come back in it: a
// handle per role is named in that order; one handle in every role hears once,
// under the first reason; across one record's routings (a push's) a handle
// hears of EACH change under its strongest reason, and of one change two
// routings name, once, under the stronger. Addressed is the nine asks and
// re_review; default primary reasons are another list; the gate's sit where
// M5-D26 puts them, adding an enqueuer and the owning unit's lead.
//
// Mutation: swap two reasons in gitlet.Reasons, or offer a role under
// another, and an order or a reason is wrong; one builder for a record's
// routings tells of one change alone; a builder per routing tells it twice.
func TestTheCodeReasonsAreInPrecedenceOrder(t *testing.T) {
	t.Parallel()
	want := []gitlet.Reason{"mention", "review_requested", "changes_requested", "checks_failed", "target_broken",
		"conflicted", "dequeued", "finding_replied", "ready_to_merge", "merged", "review_submitted", "re_review",
		"thread", "watcher", "lead_fallback"}
	if got := gitlet.Reasons(); !slices.Equal(got, want) {
		t.Fatalf("the reasons are %v, want D11's precedence %v", got, want)
	}
	asks := []gitlet.Reason{"mention", "review_requested", "changes_requested", "checks_failed", "target_broken",
		"conflicted", "dequeued", "finding_replied", "ready_to_merge", "re_review"}
	for _, r := range gitlet.Reasons() {
		if !r.Valid() {
			t.Errorf("%q is listed and not valid", r)
		}
		if r.Addressed() != slices.Contains(asks, r) {
			t.Errorf("%q addressed = %v, want %v", r, r.Addressed(), slices.Contains(asks, r))
		}
	}
	for _, r := range gitlet.Reasons() {
		if r.WakesActor() != (r == gitlet.ReasonChecksFailed) {
			t.Errorf("%q wakes its actor = %v: only a failed check is news to who made it", r, r.WakesActor())
		}
	}
	unaddressed := 0
	for _, r := range gitlet.DefaultPrimaryReasons {
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
	every := func(set func(r *gitlet.ChangeRouting)) *gitlet.ChangeRouting {
		r := &gitlet.ChangeRouting{ChangeID: "c-1", Mentions: []string{"m"}, Requested: []string{"rq"},
			PathOwners: []string{"po"}, Raisers: []string{"rs"}, ReadyFor: []string{"rf"},
			ReReview: []string{"rr"}, ThreadParticipants: []string{"tp"}, Watchers: []string{"w"},
			LeadFallback: "lead", Authors: []string{"a"}, Enqueuer: "e", Lead: "l"}
		set(r)
		return r
	}
	for name, tc := range map[string]struct {
		set     func(r *gitlet.ChangeRouting)
		authors gitlet.Reason
		// gate is each handle the gate adds beside the ten, as
		// "handle:reason".
		gate []string
	}{
		"changes requested": {func(r *gitlet.ChangeRouting) { r.Verdict = gitlet.VerdictRequestChanges },
			gitlet.ReasonChangesRequested, nil},
		"conflicted": {func(r *gitlet.ChangeRouting) { r.Conflicted = true }, gitlet.ReasonConflicted, nil},
		"merged":     {func(r *gitlet.ChangeRouting) { r.Merged = true }, gitlet.ReasonMerged, nil},
		"merged, its work item not closed": {func(r *gitlet.ChangeRouting) {
			r.Merged, r.NotClosed = true, "ENG-7 is a jira work item"
		}, gitlet.ReasonMerged, []string{"e:merged"}},
		"review submitted": {func(r *gitlet.ChangeRouting) { r.Verdict = gitlet.VerdictApprove },
			gitlet.ReasonReviewSubmitted, nil},
		"checks failed": {func(r *gitlet.ChangeRouting) { r.ChecksFailed = true }, gitlet.ReasonChecksFailed, nil},
		"target broken": {func(r *gitlet.ChangeRouting) { r.Broken = true }, gitlet.ReasonTargetBroken,
			[]string{"l:target_broken"}},
		"dequeued": {func(r *gitlet.ChangeRouting) { r.Dequeued = true }, gitlet.ReasonDequeued,
			[]string{"e:dequeued"}},
	} {
		got := gitlet.Concerns([]*gitlet.ChangeRouting{every(tc.set)})
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
	one := every(func(r *gitlet.ChangeRouting) {
		for _, list := range []*[]string{&r.Mentions, &r.Requested, &r.PathOwners, &r.Authors, &r.Raisers,
			&r.ReadyFor, &r.ReReview, &r.ThreadParticipants, &r.Watchers} {
			*list = []string{"x"}
		}
		r.LeadFallback, r.Merged, r.Conflicted, r.Verdict = "x", true, true, gitlet.VerdictRequestChanges
		r.Enqueuer, r.Lead, r.ChecksFailed, r.Broken, r.Dequeued = "x", "x", true, true, true
	})
	if got := heard(gitlet.Concerns([]*gitlet.ChangeRouting{one})); !slices.Equal(got, []string{"x:mention"}) {
		t.Fatalf("one handle in every role hears %v, want once, as mentioned", got)
	}

	// ACROSS A RECORD'S ROUTINGS: merged by the first, mentioned by the
	// second, told of each change.
	merged := &gitlet.ChangeRouting{ChangeID: "c-1", Authors: []string{"x"}, Merged: true}
	opened := &gitlet.ChangeRouting{ChangeID: "c-2", Mentions: []string{"x"}}
	got := gitlet.Concerns([]*gitlet.ChangeRouting{merged, opened})
	if len(got) != 2 || got[0].Reason != gitlet.ReasonMerged || got[0].Change != merged ||
		got[1].Reason != gitlet.ReasonMention || got[1].Change != opened {
		t.Fatalf("one handle across two changes' routings hears %v, want merged on c-1 and mentioned on c-2",
			heard(got))
	}
	// AND ONE CHANGE TWO ROUTINGS NAME: opened mentioning x, then merged by a
	// later push of the same record — x hears of it once, as mentioned.
	again := &gitlet.ChangeRouting{ChangeID: "c-2", Authors: []string{"x"}, Merged: true}
	got = gitlet.Concerns([]*gitlet.ChangeRouting{opened, again})
	if len(got) != 1 || got[0].Reason != gitlet.ReasonMention || got[0].Change != opened {
		t.Fatalf("one handle two routings of one change name hears %v, want once, mentioned", heard(got))
	}
}

// EVERY CHANGE RECORD CARRIES ITS ROUTING: an open, ask, comment, revision's
// facts, review, answer, close and reopen each name their change (the open by
// its applied number) and their op's audience — the open's mentions held to the
// chart, the newly asked, a comment's mentions (who join the participants), the
// authors by verdict, the raiser answered, a close's participants; nobody hears
// of their own write. ONLY A RECORD WITH AN AUDIENCE carries one: facts asking
// nobody, and an unthreaded comment naming only handles the chart lacks, carry
// no routing or mention and write no notice; the routing is mentions' ONE copy.
//
// Mutation: no routing on a decide, or watchers off another role → that record
// fails; a routing whatever the audience → the facts carry one; the asker's
// mentions kept on the record → the comment carries them.
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

	one := func(what string, p statelog.Position) *gitlet.ChangeRouting {
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
		!slices.Equal(heard(gitlet.Concerns([]*gitlet.ChangeRouting{r})), []string{"founder:thread"}) {
		t.Fatalf("the reply's routing names %v in its thread, hearing %v; want ada's seat told, the replier not",
			r.ThreadParticipants, heard(gitlet.Concerns([]*gitlet.ChangeRouting{r})))
	}

	// NO AUDIENCE, NO ROUTING: facts that ask nobody — plat-lead is asked
	// already, so no lead is fallen back to — and a comment whose one
	// mention the chart lacks, outside any thread.
	unheard := func(what string, p statelog.Position) {
		t.Helper()
		if routings := routingAt(t, n, p); len(routings) != 0 {
			t.Fatalf("the %s, which concerns nobody, carries the routing %+v", what, routings[0])
		}
		if body := string(n.recordAt(p).Mutation); strings.Contains(body, `"mentions"`) ||
			strings.Contains(body, `"routing"`) {
			t.Fatalf("the %s, which concerns nobody, recorded %s; want no routing and no mention", what, body)
		}
		if got := noticesOf(t, n, p); len(got) != 0 {
			t.Fatalf("the %s, which concerns nobody, wrote the notices %v", what, got)
		}
	}
	facts := converse(t, w, n.patched(repo, change, 1, "B", oidOf("the retry diff")))
	unheard("revise", facts.Position)
	aside := saying(repo, change, ada, "", "Retrying twice, @nobody-here?")
	aside.Comment.Mentions = []string{"nobody-here"}
	unheard("comment mentioning nobody the chart holds", converse(t, w, aside).Position)
	if body := string(n.recordAt(said.Position).Mutation); strings.Count(body, `"mentions"`) != 1 {
		t.Fatalf("the comment mentioning reader recorded %s; want its mentions in its routing alone", body)
	}

	reviewed := converse(t, w, reviewing(repo, change, platLead, 1, gitlet.VerdictRequestChanges, nil,
		raising(gitlet.SeverityBlocker, 7, "the retry races")))
	r = one("review", reviewed.Position)
	if got := heard(gitlet.Concerns([]*gitlet.ChangeRouting{r})); !slices.Equal(got, []string{"dev:changes_requested"}) {
		t.Fatalf("the review tells %v, want its author asked for changes", got)
	}
	answered := converse(t, w, answering(repo, change, dev, gitlet.FindingDisputed, reviewed.FindingIDs...))
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
	if r := one("reopen", reopened.Position); !slices.Contains(heard(gitlet.Concerns([]*gitlet.ChangeRouting{r})),
		"founder:watcher") {
		t.Fatalf("the reopen tells %v, want the participants", heard(gitlet.Concerns([]*gitlet.ChangeRouting{r})))
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
	for name, p := range map[string]gitlet.Proposal{
		"a head move":             proposal(repo, update(featureRef, oidOf("B"), oidOf("B2"))),
		"a branch no change uses": proposal(repo, update(releaseRef, oidOf("R"), oidOf("R2"))),
	} {
		res, err := c.Propose(t.Context(), p)
		if err != nil || res.Position.IsZero() {
			t.Fatalf("%s: Propose answered %+v, %v", name, res, err)
		}
		concerns, err := gitlet.Candidates(n.recordAt(res.Position))
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
	if got := heard(gitlet.Concerns(routings)); !slices.Equal(got, []string{"dev:merged"}) {
		t.Fatalf("the merge tells %v, want its owner, and not ada, who pushed the head and merged it", got)
	}
}

// A LANDING IS MADE BY THE PUSH THAT BROUGHT ITS HEAD IN: a target moved PAST a
// change's head (an author's merge commit, the common agent flow) merges it in
// no push's own apply, so its home walks the target's moves for the push after
// which it held the head. That pusher is the actor — a seat merging its own
// change hears nothing while its co-author is told `merged`; a person is among
// the persons — even if somebody pushed on top before the home read the target,
// in that record or later. The committer holds the named push to the rows; one
// moving no target applied, or before the head revision, or none: all are told.
//
// Mutation: name the push that last set the target to the tip (`c.New == tip`)
// → the pushed-on cases name cto; drop the applied-move check → the other-branch
// and refused cases do; drop the revision's bound → the earlier merge names dev.
func TestALandingIsMadeByThePushThatBroughtItsHeadIn(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	c, _ := committerOn(t, n, n.pub)
	pushedBy := func(who gitlet.Principal, id string, cmds ...gitlet.Command) gitlet.PushEntry {
		e := entry(id, cmds...)
		e.Principal = who
		return e
	}
	refused := gitlet.Command{Ref: mainRef, Old: oidOf("A"), New: oidOf("M1"), Result: gitlet.ResultNG,
		Reason: gitlet.RefusalNotPermitted}
	// records is how a case writes its history: push appends one push
	// record of its entries, and revise the change's revision 2 (B → B2).
	type records struct {
		push   func(entries ...gitlet.PushEntry) statelog.Position
		revise func()
	}
	for _, tc := range []struct {
		name, repo string
		// merged records the case's pushes onto main past the head B2 and
		// answers the push the home names as the merger, where it names
		// one, its record, and the tip the home found.
		merged func(r records) (named string, at statelog.Position, tip string)
		actor  string
		person bool
		heard  []string
	}{
		{
			// The seat dev, which owns the change, pushes the merge commit;
			// cto pushes on top of it in the same record, and the person on
			// harness, an author since it pushed revision 2, set release to
			// M1 earlier in it.
			name: "a seat's merge pushed on within its record", repo: "web", actor: "dev",
			heard: []string{"harness:merged"},
			merged: func(r records) (string, statelog.Position, string) {
				r.revise()
				merge := uuid.Must(uuid.NewV7()).String()
				at := r.push(entry(uuid.Must(uuid.NewV7()).String(), set(releaseRef, oidOf("R"), oidOf("M1"))),
					pushedBy(dev, merge, set(mainRef, oidOf("A"), oidOf("M1"))),
					pushedBy(cto, uuid.Must(uuid.NewV7()).String(), set(mainRef, oidOf("M1"), oidOf("M2"))))
				return merge, at, oidOf("M2")
			},
		},
		{
			name: "a seat's merge pushed on in a later record", repo: "app", actor: "dev",
			heard: []string{"harness:merged"},
			merged: func(r records) (string, statelog.Position, string) {
				r.revise()
				merge := uuid.Must(uuid.NewV7()).String()
				at := r.push(pushedBy(dev, merge, set(mainRef, oidOf("A"), oidOf("M1"))))
				r.push(pushedBy(cto, uuid.Must(uuid.NewV7()).String(), set(mainRef, oidOf("M1"), oidOf("M2"))))
				return merge, at, oidOf("M2")
			},
		},
		{
			// ada, the person on founder and no author, pushes the merge
			// commit; the seat cto, later in the record, set release to M1
			// and was refused main.
			name: "a person's merge", repo: "api", actor: "founder", person: true,
			heard: []string{"dev:merged", "harness:merged"},
			merged: func(r records) (string, statelog.Position, string) {
				r.revise()
				merge := uuid.Must(uuid.NewV7()).String()
				at := r.push(pushedBy(ada, merge, set(mainRef, oidOf("A"), oidOf("M1"))),
					pushedBy(cto, uuid.Must(uuid.NewV7()).String(), set(releaseRef, oidOf("R"), oidOf("M1")),
						refused))
				return merge, at, oidOf("M1")
			},
		},
		{
			name: "a push of another branch named", repo: "cli", heard: []string{"dev:merged", "harness:merged"},
			merged: func(r records) (string, statelog.Position, string) {
				r.revise()
				other := uuid.Must(uuid.NewV7()).String()
				at := r.push(pushedBy(cto, other, set(releaseRef, oidOf("R"), oidOf("M1"))),
					pushedBy(dev, uuid.Must(uuid.NewV7()).String(), set(mainRef, oidOf("A"), oidOf("M1"))))
				return other, at, oidOf("M1")
			},
		},
		{
			name: "a push refused the target named", repo: "docs", heard: []string{"dev:merged", "harness:merged"},
			merged: func(r records) (string, statelog.Position, string) {
				r.revise()
				refusedPush := uuid.Must(uuid.NewV7()).String()
				at := r.push(pushedBy(cto, refusedPush, set(releaseRef, oidOf("R"), oidOf("M1")), refused),
					pushedBy(dev, uuid.Must(uuid.NewV7()).String(), set(mainRef, oidOf("A"), oidOf("M1"))))
				return refusedPush, at, oidOf("M1")
			},
		},
		{
			name: "a merge before the head's revision named", repo: "ops",
			heard: []string{"dev:merged", "harness:merged"},
			merged: func(r records) (string, statelog.Position, string) {
				merge := uuid.Must(uuid.NewV7()).String()
				at := r.push(pushedBy(dev, merge, set(mainRef, oidOf("A"), oidOf("M1"))))
				r.revise()
				return merge, at, oidOf("M1")
			},
		},
		{
			name: "no push named", repo: "lib", heard: []string{"dev:merged", "harness:merged"},
			merged: func(r records) (string, statelog.Position, string) {
				r.revise()
				r.push(pushedBy(dev, uuid.Must(uuid.NewV7()).String(), set(mainRef, oidOf("A"), oidOf("M1"))))
				return "", statelog.Position{}, oidOf("M1")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, change := discussed(t, n, tc.repo)
			named, at, tip := tc.merged(records{
				push: func(entries ...gitlet.PushEntry) statelog.Position {
					pushed := n.push(repo, entries...)
					if routings := routingAt(t, n, pushed.Position); len(routings) != 0 {
						t.Fatalf("a push past the head routes %+v, want nothing: no push's own apply sees the "+
							"change merged", routings)
					}
					return pushed.Position
				},
				revise: func() { n.revise(repo, "B", "B2") },
			})
			p := landing(repo, change, 2, oidOf("B2"), mainRef, tip)
			p.Landed.PushID, p.Landed.PushPosition = named, at
			res, err := c.ProposeChange(t.Context(), p)
			if err != nil || res.Outcome != statelog.OutcomeApplied || res.Unchanged {
				t.Fatalf("the landing answered %+v, %v; want applied", res, err)
			}
			routings := routingAt(t, n, res.Position)
			if len(routings) != 1 || routings[0].ChangeID != change || !routings[0].Merged {
				t.Fatalf("the landing carries %+v, want one routing of the change, merged", routings)
			}
			r := routings[0]
			if r.Actor != tc.actor || (tc.actor != "" && r.Person(tc.actor) != tc.person) {
				t.Errorf("the landing names %q as its actor (a person: %v), want %q (a person: %v), who made the "+
					"push that brought the head in", r.Actor, r.Person(r.Actor), tc.actor, tc.person)
			}
			if got := heard(gitlet.Concerns(routings)); !slices.Equal(got, tc.heard) {
				t.Errorf("the landing tells %v, want %v: the change's authors but whoever merged it", got,
					tc.heard)
			}
		})
	}
}

// A LANDING NAMING A PUSH IS HELD TO ITS SHAPE: a push id with no position, a
// position with no push id, one on another log, or an id that is no push id
// is refused before anything is decided, each naming what it lacks — a home
// sends the push it found whole, or none.
//
// Mutation: drop any arm of the landing's push check in validFields → its
// case is accepted, or refused by a later arm for what it does not lack.
func TestALandingNamingAPushIsHeldToItsShape(t *testing.T) {
	t.Parallel()
	id := uuid.Must(uuid.NewV7()).String()
	at := statelog.Position{Stream: topics.CodeLogStream, Seq: 7}
	for _, tc := range []struct {
		name, why string
		set       func(l *gitlet.ChangeLanded)
	}{
		{"an id with no position", "named at no position", func(l *gitlet.ChangeLanded) { l.PushID = id }},
		{"a position with no id", "landed.push_id", func(l *gitlet.ChangeLanded) { l.PushPosition = at }},
		{"a position on another log", "another log", func(l *gitlet.ChangeLanded) {
			l.PushID, l.PushPosition = id, statelog.Position{Stream: topics.ChatLogStream, Seq: 7}
		}},
		{"an id that is no push id", "is not a push id", func(l *gitlet.ChangeLanded) {
			l.PushID, l.PushPosition = "push-1", at
		}},
	} {
		p := landing("01920000-0000-7000-8000-00000000000a", "01920000-0000-7000-8000-00000000000b", 1,
			oidOf("B"), mainRef, oidOf("M"))
		tc.set(p.Landed)
		if err := p.Validate(); !errors.Is(err, gitlet.ErrInvalidProposal) || !strings.Contains(err.Error(), tc.why) {
			t.Errorf("%s: a landing naming its push so validates %v, want refused invalid for %q", tc.name, err,
				tc.why)
		}
	}
	p := landing("01920000-0000-7000-8000-00000000000a", "01920000-0000-7000-8000-00000000000b", 1,
		oidOf("B"), mainRef, oidOf("M"))
	p.Landed.PushID, p.Landed.PushPosition = id, at
	if err := p.Validate(); err != nil {
		t.Errorf("a landing naming its push whole validates %v, want accepted", err)
	}
}

// READY TO MERGE IS ANNOUNCED ONLY BY A MERGEABILITY RECORD: a draft with its
// facts and clean merge recorded is ready once undrafted and approved — two
// records on two subjects, either last. Neither announces it: decided from two
// snapshots, a pair landing at once each see the other unapplied, so neither
// would see the transition. Nor does a passing check (a third subject, M5-D26).
// The home's next refresh stores not ready → ready and alone tells whoever may
// merge (dev, its author, under a gate any writer merges by); a merge of a
// later tip that finds it ready still tells nobody.
//
// Mutation: announce from a review completing readiness (judge it in
// decideReview) → the review carries ready_for; from a lifecycle record → the
// undraft does; from a passing check's ending → the check's record does.
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
		merged := converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, gitlet.MergeClean))
		if stored := n.storedReadiness(change); stored.ready {
			t.Fatalf("a draft nobody approved is stored ready")
		}

		approve := func() statelog.Position {
			return converse(t, w, reviewing(repo, change, platLead, 1, gitlet.VerdictApprove, nil)).Position
		}
		undraft := func() statelog.Position {
			res, err := c.ProposeChange(t.Context(), writing(dev, 1, func(p *gitlet.ChangeProposal) {
				p.Draft = &gitlet.ChangeDraft{RepoID: repo, ChangeID: change}
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
		checked := runCheck(t, cw, gitlet.CheckKey{RepoID: repo, OID: oidOf("B"), Target: mainRef, Name: "unit",
			Attempt: 1}, change, 1, gitlet.CheckStatePassed, gitlet.CheckReasonExit)
		for _, p := range append(completing, merged.Position, checked) {
			for _, r := range routingAt(t, n, p) {
				if len(r.ReadyFor) != 0 {
					t.Fatalf("approval last %v: the record at %s announces web!1 ready to %v", approvalLast, p,
						r.ReadyFor)
				}
			}
		}
		refresh := converse(t, w, gitlet.ConversationWrite{OpID: statelog.NewOpID(harnessAt, ""),
			Mergeability: &gitlet.Mergeability{RepoID: repo, ChangeID: change, Revision: 1, HeadOID: oidOf("B"),
				TargetRef: mainRef, Refresh: true, Node: n.id}})
		got := heard(gitlet.Concerns(routingAt(t, n, refresh.Position)))
		if !slices.Equal(got, []string{"dev:ready_to_merge"}) {
			t.Fatalf("approval last %v: the refresh tells %v, want the owner that web!1 is ready", approvalLast, got)
		}
		n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, oidOf("A"), oidOf("A2"))))
		later := converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, gitlet.MergeClean))
		if got := heard(gitlet.Concerns(routingAt(t, n, later.Position))); len(got) != 0 {
			t.Fatalf("a merge of a later tip, ready still, tells %v, want nobody", got)
		}
	}
}
