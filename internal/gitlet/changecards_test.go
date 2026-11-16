package gitlet_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// A LISTED CHANGE'S CARD, read beside its readiness where a listing asks for it, from the
// rows the shipped committer's and change writer's records wrote on a real embedded broker.

// listedCards is every change q lists, read limit rows a page with Cards as asked, in the
// order its pages served them, and how many pages that took.
func listedCards(t *testing.T, n *node, q gitlet.ReadQuery, cards bool, limit int) ([]gitlet.ChangeSummary, int) {
	t.Helper()
	q.Kind, q.Limit, q.Fresh, q.Cards = gitlet.ReadChanges, limit, stale, cards
	var out []gitlet.ChangeSummary
	for pages := 1; pages <= 100; pages++ {
		answer, err := n.reader.Read(t.Context(), q)
		if err != nil {
			t.Fatalf("Read(changes) as %+v: %v", q, err)
		}
		out = append(out, answer.Changes...)
		if answer.Next == "" {
			return out, pages
		}
		q.Cursor = answer.Next
	}
	t.Fatalf("the listing %+v did not end in a hundred pages", q)
	return nil, 0
}

// cardOf is the card of web's change number in a listing of web's changes with cards,
// failing t where the listing carries none.
func cardOf(t *testing.T, n *node, number int64) gitlet.ChangeCard {
	t.Helper()
	listed, _ := listedCards(t, n, gitlet.ReadQuery{Asker: ada, Repo: "web"}, true, 0)
	for _, s := range listed {
		if s.Number == number {
			if s.Card == nil {
				t.Fatalf("web!%d is listed with no card", number)
			}
			return *s.Card
		}
	}
	t.Fatalf("web!%d is not listed", number)
	return gitlet.ChangeCard{}
}

// widerChart is the grant tests' chart and persons person-00 onward, as many as asked.
func widerChart(persons int) chart {
	c := testChart()
	for i := range persons {
		h := fmt.Sprintf("person-%02d", i)
		c.seats[h] = gitlet.RosterSeat{Handle: h, Human: true, Unit: "eng"}
	}
	return c
}

// A CARD IS ANSWERED ONLY WHERE IT IS ASKED, on every walk a listing takes: by repository,
// by a live state, in a window, both halves of needs=me and needs=person. Unasked, no row
// carries one; asked, every listed row does.
//
// Mutation: answer the card unasked, or read it on one walk alone.
func TestACardIsAnsweredOnlyWhereItIsAsked(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	n.storedAt = func(seq uint64, _ time.Time) time.Time { return harnessAt.Add(time.Duration(seq) * time.Second) }
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	web := changeRepo(t, n, "web")
	const topicRef = "refs/heads/topic"
	pushedBy(t, c, web, ada, update(topicRef, gitlet.ZeroOID, oidOf("T")))
	var opened []string
	for _, o := range []gitlet.ChangeProposal{opening(web, featureRef, mainRef, dev),
		opening(web, releaseRef, mainRef, dev), opening(web, topicRef, mainRef, cto)} {
		res, err := c.ProposeChange(ctx, o)
		requireChanged(t, res, err)
		opened = append(opened, res.ChangeID)
	}
	// web!1 waits on a person, web!2 on dev's fix, and web!3 on dev's review.
	converse(t, w, asking(web, opened[0], dev, "founder"))
	converse(t, w, n.patched(web, opened[0], 1, "B", oidOf("the retry diff")))
	converse(t, w, n.merging(web, opened[0], 1, oidOf("B"), mainRef, gitlet.MergeClean))
	converse(t, w, reviewing(web, opened[1], platLead, 1, gitlet.VerdictRequestChanges, nil))
	converse(t, w, asking(web, opened[2], ada, "dev"))

	for _, tc := range []struct {
		what string
		q    gitlet.ReadQuery
		want []string
	}{
		{"a repository's", gitlet.ReadQuery{Asker: ada, Repo: "web"}, []string{"web!3", "web!2", "web!1"}},
		{"the open ones", gitlet.ReadQuery{Asker: ada, State: gitlet.ChangeStateOpen},
			[]string{"web!3", "web!2", "web!1"}},
		{"the day's", gitlet.ReadQuery{Asker: ada, Window: gitlet.WindowDay}, []string{"web!3", "web!2", "web!1"}},
		{"what waits on dev", gitlet.ReadQuery{Asker: dev, Needs: gitlet.NeedMe}, []string{"web!3", "web!2"}},
		{"what waits on a person", gitlet.ReadQuery{Asker: ada, Needs: gitlet.NeedPerson}, []string{"web!1"}},
	} {
		for _, cards := range []bool{false, true} {
			listed, _ := listedCards(t, n, tc.q, cards, 1)
			var refs []string
			for _, s := range listed {
				refs = append(refs, s.Ref)
				if (s.Card != nil) != cards {
					t.Errorf("%s, cards %t: %s carries the card %+v", tc.what, cards, s.Ref, s.Card)
				}
			}
			if !slices.Equal(refs, tc.want) {
				t.Errorf("%s, cards %t: listed %v, want %v", tc.what, cards, refs, tc.want)
			}
		}
	}
}

// A CARD COUNTS WHAT ITS CONVERSATION COUNTS, and shows its reviewers: its comments, reviews
// and changes requested equal the change's own conversation counters, and an undiscussed
// change's are zero. Its faces are each reviewer once, changes requested first, then a
// waiting ask, then an approval: a reviewer who asked for changes or approved and was
// asked again is one face carrying both, ranked by the stronger, and a withdrawn ask or a
// comment alone is no face.
//
// Mutation: count reviews as comments, keep a withdrawn ask, list a reviewer twice, or
// rank an approval above a waiting ask.
func TestACardCountsWhatItsConversationCounts(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = widerChart(2)
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub, func(o *gitlet.ChangeWriterOptions) {
		o.SeatRules = func() (gitlet.SeatRules, bool) { return gitlet.SeatRules{Roster: n.chart}, true }
	})
	ctx := t.Context()
	web := changeRepo(t, n, "web")
	var opened []string
	for _, source := range []string{featureRef, releaseRef} {
		res, err := c.ProposeChange(ctx, opening(web, source, mainRef, dev))
		requireChanged(t, res, err)
		opened = append(opened, res.ChangeID)
	}
	change := opened[0]
	converse(t, w, saying(web, change, ada, "", "Is this still wanted?"))
	converse(t, w, saying(web, change, ada, "", "And the retry loop?"))
	converse(t, w, reviewing(web, change, platLead, 1, gitlet.VerdictRequestChanges, nil))
	converse(t, w, reviewing(web, change, cto, 1, gitlet.VerdictApprove, nil))
	converse(t, w, asking(web, change, cto, "person-00", "founder"))
	converse(t, w, withdrawing(web, change, cto, "founder"))
	converse(t, w, asking(web, change, ada, "plat-lead", "cto"))
	pia := gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "pat:pia", Seat: "person-01"}
	converse(t, w, reviewing(web, change, pia, 1, gitlet.VerdictApprove, nil))

	card := cardOf(t, n, 1)
	detail, err := readChange(t, n, ada, "", "web", 1, 0, "")
	if err != nil || detail.Change == nil || detail.Change.Conversation == nil {
		t.Fatalf("read web!1: %+v, %v", detail.Change, err)
	}
	said := detail.Change.Conversation
	if card.Comments != said.Comments || card.Reviews != said.Reviews ||
		card.ChangesRequested != said.ChangesRequested || card.Comments != 2 || card.Reviews != 3 ||
		card.ChangesRequested != 1 {
		t.Errorf("web!1's card counts %d comments, %d reviews, %d asking for changes; its conversation %+v, "+
			"want 2, 3 and 1", card.Comments, card.Reviews, card.ChangesRequested, *said)
	}
	want := []gitlet.ReviewerFace{
		{Handle: "plat-lead", Kind: gitlet.PartyAgent, Verdict: gitlet.VerdictRequestChanges, VerdictRevision: 1,
			Asked: true, Reason: gitlet.ReviewerAsked},
		{Handle: "cto", Kind: gitlet.PartyAgent, Verdict: gitlet.VerdictApprove, VerdictRevision: 1,
			Asked: true, Reason: gitlet.ReviewerAsked},
		{Handle: "person-00", Kind: gitlet.PartyPerson, Asked: true, Reason: gitlet.ReviewerAsked},
		{Handle: "person-01", Kind: gitlet.PartyPerson, Verdict: gitlet.VerdictApprove, VerdictRevision: 1},
	}
	if !slices.Equal(card.Reviewers, want) || card.ReviewersOmitted != 0 {
		t.Errorf("web!1's faces are %+v and %d more, want %+v", card.Reviewers, card.ReviewersOmitted, want)
	}
	if untouched := cardOf(t, n, 2); untouched.Comments != 0 || untouched.Reviews != 0 ||
		untouched.ChangesRequested != 0 || len(untouched.Reviewers) != 0 || untouched.Checks != nil ||
		untouched.Ejected != nil {
		t.Errorf("web!2, which nobody discussed, carries %+v", untouched)
	}
	// EVERY LISTED ROW CARRIES AN ENCODABLE CARD, the zero counters at least.
	listed, _ := listedCards(t, n, gitlet.ReadQuery{Asker: ada, Repo: "web"}, true, 0)
	for _, s := range listed {
		b, err := json.Marshal(s.Card)
		if err != nil || len(b) < len(`{"comments":0,"reviews":0,"changes_requested":0}`) {
			t.Errorf("%s's card encodes as %s, %v", s.Ref, b, err)
		}
	}
}

// A CARD'S FACES ARE BOUNDED WITH A COUNT OF THE REST: eleven reviewers asked and an
// approval are eight faces, the asks first by handle, and four more counted.
//
// Mutation: drop the bound or the count, or order the faces by handle alone.
func TestACardsFacesAreBoundedWithACountOfTheRest(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = widerChart(11)
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub, func(o *gitlet.ChangeWriterOptions) {
		o.SeatRules = func() (gitlet.SeatRules, bool) { return gitlet.SeatRules{Roster: n.chart}, true }
	})
	var persons []string
	for i := range 11 {
		persons = append(persons, fmt.Sprintf("person-%02d", i))
	}
	converse(t, w, asking(repo, change, ada, persons...))
	converse(t, w, reviewing(repo, change, cto, 1, gitlet.VerdictApprove, nil))

	card := cardOf(t, n, 1)
	var got []string
	for _, f := range card.Reviewers {
		got = append(got, f.Handle)
	}
	if !slices.Equal(got, persons[:gitlet.MaxCardReviewers]) || card.ReviewersOmitted != 4 {
		t.Errorf("the card's faces are %v and %d more, want %v and 4 more", got, card.ReviewersOmitted,
			persons[:gitlet.MaxCardReviewers])
	}
}

// A CARD'S CHECKS ARE WHERE THE CHANGE'S LATEST CHECKS STAND: each check's latest attempt of
// its head for its target, as `checks` with latest reads them, a rerun counted at its rerun
// and another target's attempts not at all; a change whose head holds no attempt has none.
//
// Mutation: count every attempt, or read every target of the head.
func TestACardsChecksAreWhereTheChangesLatestChecksStand(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	web := changeRepo(t, n, "web")
	for _, source := range []string{featureRef, releaseRef} {
		res, err := c.ProposeChange(t.Context(), opening(web, source, mainRef, dev))
		requireChanged(t, res, err)
	}
	n.checked(web, oidOf("B"), mainRef, "unit", 1, gitlet.CheckStatePassed)
	n.checked(web, oidOf("B"), mainRef, "e2e", 1, gitlet.CheckStateFailed)
	n.checked(web, oidOf("B"), mainRef, "e2e", 2, gitlet.CheckStatePassed)
	n.checked(web, oidOf("B"), mainRef, "lint", 1, gitlet.CheckStateFailed)
	n.exec(`UPDATE code_checks SET required = 0 WHERE repo_id = ? AND name = 'lint'`, web)
	n.checked(web, oidOf("B"), releaseRef, "deploy", 1, gitlet.CheckStateFailed)

	card := cardOf(t, n, 1)
	if card.Checks == nil {
		t.Fatal("web!1's card carries no checks")
	}
	got := *card.Checks
	// THE ROLLUP EQUALS what `checks` with latest answers of the change.
	latest := servedChecks(t, n, gitlet.ReadQuery{Kind: gitlet.ReadChecks, Asker: ada, Repo: "web", Fresh: stale,
		Checks: &gitlet.CheckQuery{Change: 1, Latest: true}})
	var counts gitlet.CheckCounts
	var required, requiredPassed int
	var failing []gitlet.FailingCheck
	for _, row := range latest {
		switch row.State {
		case gitlet.CheckStatePassed:
			counts.Passed++
		case gitlet.CheckStateFailed:
			counts.Failed++
			failing = append(failing, gitlet.FailingCheck{Name: row.Name, State: row.State, Required: row.Required})
		default:
			t.Fatalf("the latest attempts hold %s, which this case never wrote", row.State)
		}
		if row.Required {
			required++
			if row.State == gitlet.CheckStatePassed {
				requiredPassed++
			}
		}
	}
	if got.Counts != counts || got.Required != required || got.RequiredPassed != requiredPassed ||
		!slices.Equal(got.Failing, failing) {
		t.Errorf("the card's checks are %+v, and `checks` with latest answers %d attempts: %+v passed of %d "+
			"required, failing %+v", got, len(latest), counts, required, failing)
	}
	want := gitlet.CheckCounts{Passed: 2, Failed: 1}
	if got.State != gitlet.RollupFailing || got.Counts != want || got.Required != 2 || got.RequiredPassed != 2 ||
		!slices.Equal(got.Failing, []gitlet.FailingCheck{{Name: "lint", State: gitlet.CheckStateFailed}}) ||
		got.More {
		t.Errorf("the card's checks are %+v, want failing on lint alone, not required, 2 of 2 required passed",
			got)
	}
	if other := cardOf(t, n, 2); other.Checks != nil {
		t.Errorf("web!2, whose head holds no attempt, carries checks %+v", *other.Checks)
	}
}

// A CARD'S CHECKS ARE BOUNDED AT A CHECKS FILE: a head failing every check one checks file
// declares names MaxRollupFailing of them and counts the rest, and one holding more checks
// than a rollup reads says so.
//
// Mutation: read the rollup unbounded, or drop its count of the failing it does not name.
func TestACardsChecksAreBoundedAtAChecksFile(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, _ := discussed(t, n, "web")
	for i := range gitlet.MaxChecksPerFile {
		n.checked(repo, oidOf("B"), mainRef, fmt.Sprintf("check-%03d", i), 1, gitlet.CheckStateFailed)
	}
	got := cardOf(t, n, 1).Checks
	if got == nil || len(got.Failing) != gitlet.MaxRollupFailing ||
		got.FailingMore != gitlet.MaxChecksPerFile-gitlet.MaxRollupFailing || got.More ||
		got.Counts.Failed != gitlet.MaxChecksPerFile {
		t.Fatalf("a head failing %d checks carries %+v, want %d named, the rest counted", gitlet.MaxChecksPerFile,
			got, gitlet.MaxRollupFailing)
	}
	for i := gitlet.MaxChecksPerFile; i <= 2*gitlet.MaxChecksPerFile; i++ {
		n.checked(repo, oidOf("B"), mainRef, fmt.Sprintf("check-%03d", i), 1, gitlet.CheckStatePassed)
	}
	if got = cardOf(t, n, 1).Checks; got == nil || !got.More ||
		got.Counts.Failed+got.Counts.Passed != 2*gitlet.MaxChecksPerFile {
		t.Fatalf("a head holding %d checks carries %+v, want the rollup's bound read and more said",
			2*gitlet.MaxChecksPerFile+1, got)
	}
}

// A CARD SAYS A LIVE CHANGE WAS TAKEN OUT OF ITS QUEUE WITHIN A DAY: why, when and the
// checks that failed, while its newest entry is the ejection, it is live and the ejection
// is under QueueEjectionsShown old; never once the day has passed, once it is closed, or
// once it is queued again.
//
// Mutation: take the newest ENDED entry, skip the horizon, or answer a closed change.
func TestACardSaysALiveChangeWasTakenOutOfItsQueueWithinADay(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	n.storedAt = func(seq uint64, _ time.Time) time.Time { return harnessAt.Add(time.Duration(seq) * time.Second) }
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	repo, change := queueRepo(t, n, c, "web")
	ready(t, n, w, repo, change)
	enqueue := enqueueing(repo, change, ada, oidOf("B"))
	res, err := c.ProposeQueue(ctx, enqueue)
	requireQueued(t, res, err, gitlet.QueueWaiting, 1)
	if got := cardOf(t, n, 1).Ejected; got != nil {
		t.Fatalf("a queued change carries the ejection %+v", *got)
	}
	res, err = c.ProposeQueue(ctx, training(repo, oidOf("A"), enqueue.Enqueue.EntryID, oidOf("M")))
	requireRecorded(t, res, err)
	failed := []string{"unit"}
	for i := range gitlet.MaxCardEjectionChecks + 1 {
		failed = append(failed, fmt.Sprintf("e2e-%02d", i))
	}
	res, err = c.ProposeQueue(ctx, queueRecord(func(p *gitlet.QueueProposal) {
		p.Eject = &gitlet.Eject{RepoID: repo, Entries: []gitlet.Ejection{{EntryID: enqueue.Enqueue.EntryID,
			Reason: gitlet.EjectCheckFailed, Checks: failed}}}
	}))
	requireRecorded(t, res, err)

	got := cardOf(t, n, 1).Ejected
	if got == nil || got.Reason != gitlet.EjectCheckFailed || got.At.Before(harnessAt) ||
		!got.At.Before(harnessAt.Add(time.Hour)) || !slices.Equal(got.Checks, failed[:gitlet.MaxCardEjectionChecks]) ||
		got.ChecksMore != 2 {
		t.Fatalf("the ejected change carries %+v, want check_failed at its record's instant, naming %v and 2 more",
			got, failed[:gitlet.MaxCardEjectionChecks])
	}
	// PAST THE HORIZON the ejection is the change's history, not its row's.
	past := got.At.Add(gitlet.QueueEjectionsShown + time.Second)
	n.readAt.Store(&past)
	if got := cardOf(t, n, 1).Ejected; got != nil {
		t.Errorf("a day and a second on, the change carries the ejection %+v", *got)
	}
	n.readAt.Store(nil)

	// A CLOSED CHANGE merges nothing, so its ejection is no tag; reopened, it is again.
	version := func() int64 {
		listed, _ := listedCards(t, n, gitlet.ReadQuery{Asker: ada, Repo: "web"}, false, 0)
		return listed[0].Version
	}
	closed, err := c.ProposeChange(ctx, closing(repo, change, dev, version()))
	requireChanged(t, closed, err)
	if got := cardOf(t, n, 1).Ejected; got != nil {
		t.Errorf("a closed change carries the ejection %+v", *got)
	}
	reopened, err := c.ProposeChange(ctx, reopening(repo, change, dev, version()))
	requireChanged(t, reopened, err)
	if got := cardOf(t, n, 1).Ejected; got == nil {
		t.Error("the reopened change carries no ejection, and its newest entry was ejected an instant ago")
	}

	// QUEUED AGAIN, its newest entry is live, whatever its newest ended one says.
	again := enqueueing(repo, change, ada, oidOf("B"))
	res, err = c.ProposeQueue(ctx, again)
	requireQueued(t, res, err, gitlet.QueueWaiting, 1)
	if got := cardOf(t, n, 1).Ejected; got != nil {
		t.Errorf("a change queued again carries the ejection %+v", *got)
	}
}

// A PAGE OF CARDS IS CUT BY WHAT ITS CARDS WEIGH: with a page's byte budget between a row's
// size without its card and with it, a listing with cards takes a row a page where one
// without takes two, and every page resumes after the last row it kept.
//
// Mutation: weigh a row without its card.
func TestAPageOfCardsIsCutByWhatItsCardsWeigh(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	web := changeRepo(t, n, "web")
	var opened []string
	for _, source := range []string{featureRef, releaseRef} {
		res, err := c.ProposeChange(t.Context(), opening(web, source, mainRef, dev))
		requireChanged(t, res, err)
		opened = append(opened, res.ChangeID)
	}
	for _, change := range opened {
		converse(t, w, asking(web, change, cto, "plat-lead", "founder"))
	}
	size := func(cards bool) (least, most int) {
		listed, _ := listedCards(t, n, gitlet.ReadQuery{Asker: ada, Repo: "web"}, cards, 0)
		least = 1 << 30
		for _, s := range listed {
			b, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			least, most = min(least, len(b)), max(most, len(b))
		}
		return least, most
	}
	_, bare := size(false)
	carded, _ := size(true)
	if carded <= bare+1 {
		t.Fatalf("a row with its card is %d bytes and one without %d: no budget lies between", carded, bare)
	}
	gitlet.SetPageLimits(n.reader, 50, bare+1)
	for _, tc := range []struct {
		cards bool
		first []string
	}{{false, []string{"web!2", "web!1"}}, {true, []string{"web!2"}}} {
		q := gitlet.ReadQuery{Kind: gitlet.ReadChanges, Asker: ada, Repo: "web", Fresh: stale, Cards: tc.cards}
		answer, err := n.reader.Read(t.Context(), q)
		if err != nil {
			t.Fatalf("Read(changes), cards %t: %v", tc.cards, err)
		}
		var first []string
		for _, s := range answer.Changes {
			first = append(first, s.Ref)
		}
		if !slices.Equal(first, tc.first) {
			t.Errorf("cards %t at %d bytes a page: the first page lists %v, want %v", tc.cards, bare+1, first,
				tc.first)
		}
		listed, _ := listedCards(t, n, gitlet.ReadQuery{Asker: ada, Repo: "web"}, tc.cards, 0)
		var refs []string
		for _, s := range listed {
			refs = append(refs, s.Ref)
		}
		if !slices.Equal(refs, []string{"web!2", "web!1"}) {
			t.Errorf("cards %t at %d bytes a page: listed %v, want web!2 then web!1", tc.cards, bare+1, refs)
		}
	}
}
