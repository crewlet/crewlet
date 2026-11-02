package code_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
)

// THE READS OF A CHANGE'S CONVERSATION: the changes waiting on a reviewer and
// the parts of one change's conversation, read through the shipped reader from
// the rows the shipped committer's and change writer's records wrote, on a
// real embedded broker.

// newestFirst is the references of the changes ids name, newest first by
// id — the order a reviewer's listing reads them in — from refs, a change's
// reference by its id.
func newestFirst(refs map[string]string, ids ...string) []string {
	ids = slices.Clone(ids)
	slices.Sort(ids)
	slices.Reverse(ids)
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = refs[id]
	}
	return out
}

// THE CHANGES WAITING ON A REVIEWER ARE THE ONES WHOSE ASK OF THEM STANDS: a
// listing naming a reviewer reads their standing asks, newest change first and
// a page at a time, across repositories — a withdrawn ask is none, and a
// closed change's ask stands until its home settles it (the case below);
// needs=review keeps the live changes alone,
// the asker's own where it names no reviewer, a person's by the human seat
// they hold; and every other filter is tested on the rows a page read.
//
// Mutations: read the withdrawn asks too → app!1 is listed for plat-lead;
// drop the live test → web!1, closed, waits on plat-lead's review; walk the
// asks oldest first → the pages come in the other order.
func TestTheChangesWaitingOnAReviewerAreTheOnesWhoseAskStands(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	web := changeRepo(t, n, "web")
	app := changeRepo(t, n, "app")

	refs := map[string]string{}
	open := func(repo, name, source string) code.ChangeResult {
		res, err := c.ProposeChange(ctx, opening(repo, source, mainRef, dev))
		requireChanged(t, res, err)
		refs[res.ChangeID] = code.ChangeRef(name, res.Number)
		return res
	}
	web1, web2 := open(web, "web", featureRef), open(web, "web", releaseRef)
	app1, app2 := open(app, "app", featureRef), open(app, "app", releaseRef)
	for _, ask := range []code.ConversationWrite{
		asking(web, web1.ChangeID, cto, "plat-lead"),
		asking(web, web2.ChangeID, cto, "plat-lead", "founder"),
		asking(app, app1.ChangeID, cto, "plat-lead"),
		asking(app, app2.ChangeID, cto, "founder"),
		withdrawing(app, app1.ChangeID, cto, "plat-lead"),
	} {
		res, err := w.Converse(ctx, ask)
		requireConversed(t, res, err)
	}
	closed, err := c.ProposeChange(ctx, closing(web, web1.ChangeID, dev, web1.Version))
	requireChanged(t, closed, err)

	for name, tc := range map[string]struct {
		q    code.ReadQuery
		want []string
	}{
		"plat-lead's standing asks, a closed change's among them": {
			code.ReadQuery{Asker: cto, Reviewer: "plat-lead"}, newestFirst(refs, web1.ChangeID, web2.ChangeID)},
		"what waits on plat-lead's review, asked by plat-lead": {
			code.ReadQuery{Asker: platLead, Needs: code.NeedReview}, newestFirst(refs, web2.ChangeID)},
		"what waits on a person's review, by the seat they hold": {
			code.ReadQuery{Asker: ada, Needs: code.NeedReview}, newestFirst(refs, web2.ChangeID, app2.ChangeID)},
		"what waits on the founder's review, asked by another": {
			code.ReadQuery{Asker: cto, Reviewer: "founder", Needs: code.NeedReview},
			newestFirst(refs, web2.ChangeID, app2.ChangeID)},
		"the founder's asks on one repository": {
			code.ReadQuery{Asker: cto, Reviewer: "founder", Repo: "app"}, newestFirst(refs, app2.ChangeID)},
		"plat-lead's asks of closed changes": {
			code.ReadQuery{Asker: cto, Reviewer: "plat-lead", State: code.ChangeStateClosed},
			newestFirst(refs, web1.ChangeID)},
		"plat-lead's asks of another owner's changes": {
			code.ReadQuery{Asker: cto, Reviewer: "plat-lead", Owner: "cto"}, nil},
		"the asks of a reviewer nobody asked": {code.ReadQuery{Asker: cto, Reviewer: "vp-sales"}, nil},
	} {
		for _, limit := range []int{1, 50} {
			got, pages := listChanges(t, n, tc.q, limit)
			if !slices.Equal(got, tc.want) {
				t.Errorf("%s, %d a page: listed %v, want %v", name, limit, got, tc.want)
			}
			if limit == 1 && len(tc.want) > 1 && pages < len(tc.want) {
				t.Errorf("%s: %d changes came in %d pages of one", name, len(tc.want), pages)
			}
		}
	}
}

// settling is the home's settling of change's asks with its lifecycle, moving
// them to state.
func settling(repo, change string, state code.ReviewerState) code.ConversationWrite {
	return conversing(code.Principal{}, func(w *code.ConversationWrite) {
		w.Asks = &code.AskSettle{RepoID: repo, ChangeID: change, State: state}
	})
}

// askSettles is the changes of repo whose asks the reader names due a
// settling, read as a home reads its first page.
func (n *node) askSettles(repo string) []code.AskDue {
	n.t.Helper()
	q := code.ReadQuery{Kind: code.ReadChangesDue, Fresh: stale,
		ChangesDue: &code.ChangesDueQuery{RepoID: repo, RecheckBefore: harnessAt}}
	a, err := n.reader.Read(n.t.Context(), q)
	if err != nil {
		n.t.Fatalf("read %s's due changes: %v", repo, err)
	}
	return a.AskSettles
}

// THE CHANGES WAITING ON A REVIEWER ARE READ FROM THE ASKS STILL WAITING: a
// review answers its reviewer's own ask, and a change's end ends the asks
// still waiting on it once its home settles them — so a reviewer whose every
// other ask is answered or ended finds their one live, unanswered ask, the
// OLDEST of them all, on the first page of pages of one, where the walk used
// to read every newer settled ask first and come back empty; reviewer= names
// none of the settled. The ended change is due its settling — its repository
// listed, the change named — until the settling lands, and not after; the
// settling's decide holds the direction to the change's state, a live
// change's end and an ended change's ask again superseded; and once the
// change is reopened its ended ask is due the other way, settled, and waits
// again.
//
// Mutations: drop the review's settling of its own ask → the reviewed change
// is read first and the first page comes back empty; drop applyAsks's move →
// the closed change stays listed under reviewer= and stays due; drop the
// ended listing → the closed change is never named due.
func TestAReviewersListingWalksOnlyTheAsksStillWaiting(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	web := changeRepo(t, n, "web")
	refs := map[string]string{}
	open := func(source string) code.ChangeResult {
		t.Helper()
		if source != featureRef && source != releaseRef {
			n.push(web, entry(uuid.Must(uuid.NewV7()).String(), set(source, code.ZeroOID, oidOf(source))))
		}
		res, err := c.ProposeChange(ctx, opening(web, source, mainRef, dev))
		requireChanged(t, res, err)
		refs[res.ChangeID] = code.ChangeRef("web", res.Number)
		return res
	}
	converse := func(cw code.ConversationWrite) code.ConversationResult {
		t.Helper()
		res, err := w.Converse(ctx, cw)
		requireConversed(t, res, err)
		return res
	}
	// THE OLDEST CHANGE WAITS, which a reviewer's listing reads last.
	waiting := open(featureRef)
	converse(asking(web, waiting.ChangeID, cto, "plat-lead"))
	reviewed := open(releaseRef)
	converse(asking(web, reviewed.ChangeID, cto, "plat-lead"))
	converse(reviewing(web, reviewed.ChangeID, platLead, 1, code.VerdictComment, nil))
	ended := open("refs/heads/ended")
	converse(asking(web, ended.ChangeID, cto, "plat-lead"))
	closed, err := c.ProposeChange(ctx, closing(web, ended.ChangeID, dev, ended.Version))
	requireChanged(t, closed, err)

	// DUE ITS SETTLING, and the decide holds the direction to its state.
	if got := n.askSettles(web); len(got) != 1 || got[0] != (code.AskDue{ChangeID: ended.ChangeID,
		State: code.ReviewerEnded}) {
		t.Fatalf("the due listing names %+v, want the closed change's asks due their end", got)
	}
	if !slices.Contains(n.dueRepos(), web) {
		t.Fatalf("the due repositories %v leave out web, whose closed change's asks still wait", n.dueRepos())
	}
	// A LIVE CHANGE'S ASKS DO NOT END, AND AN ENDED CHANGE'S ARE NOT ASKED
	// AGAIN: each refused superseded, appending nothing.
	for _, cw := range []code.ConversationWrite{
		settling(web, waiting.ChangeID, code.ReviewerEnded),
		settling(web, ended.ChangeID, code.ReviewerRequested),
	} {
		before := n.end()
		res, err := w.Converse(ctx, cw)
		requireConversationRefused(t, n, before, res, err, code.ConversationRefusalSuperseded, "")
	}
	converse(settling(web, ended.ChangeID, code.ReviewerEnded))
	if got := n.askSettles(web); len(got) != 0 {
		t.Fatalf("the due listing still names %+v once the closed change's asks ended", got)
	}

	// ONE PAGE OF ONE HOLDS THE ONE ASK STILL WAITING.
	q := code.ReadQuery{Kind: code.ReadChanges, Asker: platLead, Needs: code.NeedReview, Limit: 1, Fresh: stale}
	a, err := n.reader.Read(ctx, q)
	switch {
	case err != nil:
		t.Fatalf("read what waits on plat-lead's review: %v", err)
	case len(a.Changes) != 1 || a.Changes[0].Ref != refs[waiting.ChangeID]:
		t.Fatalf("the first page of one of what waits on plat-lead's review lists %+v, want %s alone",
			a.Changes, refs[waiting.ChangeID])
	}
	if got, _ := listChanges(t, n, code.ReadQuery{Asker: cto, Reviewer: "plat-lead"}, 50); !slices.Equal(got,
		[]string{refs[waiting.ChangeID]}) {
		t.Fatalf("plat-lead's waiting asks are %v, want %s alone", got, refs[waiting.ChangeID])
	}

	// REOPENED, ITS ENDED ASK IS DUE AGAIN THE OTHER WAY, and waits once
	// settled.
	reopened, err := c.ProposeChange(ctx, reopening(web, ended.ChangeID, dev, closed.Version))
	requireChanged(t, reopened, err)
	if got := n.askSettles(web); len(got) != 1 || got[0] != (code.AskDue{ChangeID: ended.ChangeID,
		State: code.ReviewerRequested}) {
		t.Fatalf("the due listing names %+v, want the reopened change's asks due again", got)
	}
	converse(settling(web, ended.ChangeID, code.ReviewerRequested))
	if got := n.askSettles(web); len(got) != 0 {
		t.Fatalf("the due listing still names %+v once the reopened change's asks wait again", got)
	}
	if got, _ := listChanges(t, n, code.ReadQuery{Asker: cto, Reviewer: "plat-lead"}, 50); !slices.Equal(got,
		newestFirst(refs, waiting.ChangeID, ended.ChangeID)) {
		t.Fatalf("plat-lead's waiting asks are %v, want the reopened change's and the oldest", got)
	}
}

// A CHANGE'S CONVERSATION IS READ A PART AT A TIME: its counters ride its
// detail; its comments come a thread at a time — each thread's first comment
// and then its replies in the order they were said, threads in the order of
// their ids — a page at a time, a cursor resuming after the comment it names,
// with the line a thread points at and who resolved it; its reviewers by
// handle, a withdrawn ask among them; and a part's page carries no detail. A
// cursor of another part is none of this one's.
//
// Mutations: order the comments by position alone → a later thread's first
// comment comes between an earlier thread's replies; carry the detail on
// every part's first page → the comments' page holds it.
func TestAChangesConversationIsReadAPartAtATime(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	say := func(asker code.Principal, parent, body string) string {
		res, err := w.Converse(ctx, saying(repo, change, asker, parent, body))
		requireConversed(t, res, err)
		return res.ID
	}
	first := say(platLead, "", "Why a minute?")
	second := say(cto, "", "Ship it.")
	firstReply := say(dev, first, "The upstream limit.")
	secondReply := say(dev, second, "Thanks.")
	firstAgain := say(platLead, first, "Fine.")
	res, err := w.Converse(ctx, resolving(repo, change, dev, first, true))
	requireConversed(t, res, err)
	for _, ask := range []code.ConversationWrite{
		asking(repo, change, cto, "plat-lead", "founder"), withdrawing(repo, change, cto, "founder"),
	} {
		res, err := w.Converse(ctx, ask)
		requireConversed(t, res, err)
	}

	detail, err := readChange(t, n, cto, "", "web", 1, 0, "")
	if err != nil {
		t.Fatalf("read web!1: %v", err)
	}
	if got := detail.Change.Conversation; got == nil || got.Comments != 5 || got.Version != 8 {
		t.Fatalf("web!1's conversation counts %+v, want 5 comments over 8 records", got)
	}

	// THE COMMENTS, a page of two at a time.
	threads := [][]string{{first, firstReply, firstAgain}, {second, secondReply}}
	slices.SortFunc(threads, func(a, b []string) int { return strings.Compare(a[0], b[0]) })
	want := slices.Concat(threads...)
	var got []string
	q := code.ReadQuery{Asker: cto, Kind: code.ReadChange, Repo: "web", Change: 1, Part: code.PartComments,
		Limit: 2, Fresh: stale}
	for pages := 1; ; pages++ {
		answer, err := n.reader.Read(ctx, q)
		if err != nil {
			t.Fatalf("read web!1's comments: %v", err)
		}
		if answer.Change != nil || len(answer.Revisions) > 0 {
			t.Fatalf("a page of comments carries the change's detail or revisions: %+v", answer)
		}
		for _, cm := range answer.Comments {
			got = append(got, cm.ID)
			switch cm.ID {
			case first:
				if !cm.Resolved || cm.ResolvedBy != "dev" || cm.ResolvedByKind != "agent" || cm.ParentID != "" ||
					cm.ThreadID != first || cm.Author != "plat-lead" || cm.Body != "Why a minute?" {
					t.Errorf("the first thread's root reads %+v", cm)
				}
			case firstReply:
				if cm.ThreadID != first || cm.ParentID != first || cm.Author != "dev" || cm.Resolved {
					t.Errorf("a reply reads %+v", cm)
				}
			}
		}
		if answer.Next == "" {
			break
		}
		if pages > 5 {
			t.Fatal("five comments took more than five pages")
		}
		q.Cursor = answer.Next
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the comments came %v, want %v", got, want)
	}

	// THE REVIEWERS, by handle, the withdrawn ask kept.
	answer, err := n.reader.Read(ctx, code.ReadQuery{Asker: cto, Kind: code.ReadChange, Repo: "web", Change: 1,
		Part: code.PartReviewers, Fresh: stale})
	if err != nil {
		t.Fatalf("read web!1's reviewers: %v", err)
	}
	if len(answer.Reviewers) != 2 || answer.Reviewers[0].Reviewer != "founder" ||
		answer.Reviewers[0].State != code.ReviewerWithdrawn || answer.Reviewers[0].Kind != "person" ||
		answer.Reviewers[1].Reviewer != "plat-lead" || answer.Reviewers[1].State != code.ReviewerRequested ||
		answer.Reviewers[1].RequestedBy != "cto" || answer.Next != "" || answer.Change != nil {
		t.Fatalf("web!1's reviewers read %+v", answer)
	}

	// A CURSOR OF ANOTHER PART.
	for name, q := range map[string]code.ReadQuery{
		"comments after a reviewer": {Part: code.PartComments, Cursor: "plat-lead"},
		"reviewers after a comment": {Part: code.PartReviewers, Cursor: "12/" + first},
	} {
		q.Asker, q.Kind, q.Repo, q.Change, q.Fresh = cto, code.ReadChange, "web", 1, stale
		if _, err := n.reader.Read(ctx, q); !errors.Is(err, code.ErrBadCursor) {
			t.Errorf("%s: Read = %v, want ErrBadCursor", name, err)
		}
	}
}

// A CHANGE'S REVIEWS AND FINDINGS ARE READ WITH IT: its detail counts its
// reviews, its open findings, its standing blockers and its requests for
// changes; its reviews come in the order they were given and its findings by
// id, each with its text — the lines a suggestion would put in place among
// it — and its last answer, a page at a time; a reply to a finding names it;
// and a cursor of another part is none of these.
//
// Mutations: order the reviews by reviewer, and cto's comes before
// plat-lead's earlier one; read a finding's suggestion as an empty string
// where it has none, and every finding suggests deleting its lines.
func TestAChangesReviewsAndFindingsAreReadWithIt(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	suggestion := "time.Sleep(backoff)"
	suggesting := raising(code.SeverityMajor, 7, "Sleep between tries")
	suggesting.Suggestion = &suggestion
	res, err := w.Converse(ctx, reviewing(repo, change, platLead, 1, code.VerdictRequestChanges, nil,
		raising(code.SeverityBlocker, 3, "No backoff"), suggesting))
	first := requireReviewed(t, res, err)
	firstID := res.ID
	res, err = w.Converse(ctx, reviewing(repo, change, cto, 1, code.VerdictComment, nil,
		raising(code.SeverityNit, 9, "A name")))
	second := requireReviewed(t, res, err)
	secondID := res.ID
	res, err = w.Converse(ctx, conversing(dev, func(cw *code.ConversationWrite) {
		cw.FindingResolve = &code.FindingResolve{RepoID: repo, ChangeID: change, Findings: []code.FindingResolution{
			{FindingID: first[0], Resolution: code.FindingDisputed, Note: "the client backs off"}}}
	}))
	requireConversed(t, res, err)
	answeredAt := res.Position
	res, err = w.Converse(ctx, conversing(platLead, func(cw *code.ConversationWrite) {
		cw.Comment = &code.Comment{RepoID: repo, ChangeID: change, FindingID: first[0], Body: "It does not."}
	}))
	requireConversed(t, res, err)
	reply := res.ID

	detail, err := readChange(t, n, cto, "", "web", 1, 0, "")
	if err != nil {
		t.Fatalf("read web!1: %v", err)
	}
	if got := detail.Change.Conversation; got == nil || got.Reviews != 2 || got.OpenFindings != 2 ||
		got.OpenBlockers != 1 || got.ChangesRequested != 1 || got.Comments != 1 {
		t.Fatalf("web!1's conversation counts %+v, want 2 reviews, 2 open findings, 1 standing blocker, 1 "+
			"request for changes and 1 comment", got)
	}

	// THE REVIEWS, a page of one at a time, in the order given.
	var reviews []string
	q := code.ReadQuery{Asker: cto, Kind: code.ReadChange, Repo: "web", Change: 1, Part: code.PartReviews,
		Limit: 1, Fresh: stale}
	for pages := 1; ; pages++ {
		answer, err := n.reader.Read(ctx, q)
		if err != nil {
			t.Fatalf("read web!1's reviews: %v", err)
		}
		if answer.Change != nil || len(answer.Findings) > 0 {
			t.Fatalf("a page of reviews carries more than reviews: %+v", answer)
		}
		for _, rv := range answer.Reviews {
			reviews = append(reviews, rv.ID)
			if rv.ID == firstID && (rv.Reviewer != "plat-lead" || rv.ReviewerKind != "agent" ||
				rv.Verdict != code.VerdictRequestChanges || rv.Findings != 2 || rv.Revision != 1 ||
				rv.Summary != "The retry loop.") {
				t.Errorf("plat-lead's review reads %+v", rv)
			}
		}
		if answer.Next == "" {
			break
		}
		if pages > 3 {
			t.Fatal("two reviews took more than three pages")
		}
		q.Cursor = answer.Next
	}
	if !slices.Equal(reviews, []string{firstID, secondID}) {
		t.Fatalf("the reviews came %v, want %v in the order given", reviews, []string{firstID, secondID})
	}

	// THE FINDINGS, a page of two at a time, by id.
	want := slices.Concat(first, second)
	slices.Sort(want)
	var findings []string
	q = code.ReadQuery{Asker: cto, Kind: code.ReadChange, Repo: "web", Change: 1, Part: code.PartFindings,
		Limit: 2, Fresh: stale}
	for pages := 1; ; pages++ {
		answer, err := n.reader.Read(ctx, q)
		if err != nil {
			t.Fatalf("read web!1's findings: %v", err)
		}
		for _, f := range answer.Findings {
			findings = append(findings, f.ID)
			switch f.ID {
			case first[0]:
				if f.State != code.FindingDisputed || f.ResolutionNote != "the client backs off" ||
					f.ResolvedBy != "dev" || f.ResolvedByKind != "agent" || f.ResolvedPosition != answeredAt ||
					f.Severity != code.SeverityBlocker || f.Title != "No backoff" || f.Suggestion != nil ||
					f.ReviewID != firstID || f.Raiser != "plat-lead" || f.LineStart != 3 {
					t.Errorf("the disputed blocker reads %+v", f)
				}
			case first[1]:
				if f.Suggestion == nil || *f.Suggestion != suggestion || f.State != code.FindingOpen ||
					!f.ResolvedPosition.IsZero() {
					t.Errorf("the suggesting finding reads %+v", f)
				}
			}
		}
		if answer.Next == "" {
			break
		}
		if pages > 3 {
			t.Fatal("three findings took more than three pages")
		}
		q.Cursor = answer.Next
	}
	if !slices.Equal(findings, want) {
		t.Fatalf("the findings came %v, want %v by id", findings, want)
	}

	// A REPLY TO A FINDING NAMES IT.
	answer, err := n.reader.Read(ctx, code.ReadQuery{Asker: cto, Kind: code.ReadChange, Repo: "web", Change: 1,
		Part: code.PartComments, Fresh: stale})
	if err != nil || len(answer.Comments) != 1 || answer.Comments[0].ID != reply ||
		answer.Comments[0].FindingID != first[0] || answer.Comments[0].ThreadID != first[0] {
		t.Fatalf("web!1's comments read %+v, %v; want the reply in the finding's thread", answer.Comments, err)
	}

	// A CURSOR OF ANOTHER PART.
	for name, q := range map[string]code.ReadQuery{
		"reviews after a finding":         {Part: code.PartReviews, Cursor: first[0]},
		"findings after a review":         {Part: code.PartFindings, Cursor: "3000"},
		"reviews after a comment's place": {Part: code.PartReviews, Cursor: "12/" + reply},
	} {
		q.Asker, q.Kind, q.Repo, q.Change, q.Fresh = cto, code.ReadChange, "web", 1, stale
		if _, err := n.reader.Read(ctx, q); !errors.Is(err, code.ErrBadCursor) {
			t.Errorf("%s: Read = %v, want ErrBadCursor", name, err)
		}
	}
}

// WHAT WAITS ON AN OWNER'S FIX IS WHAT THEIR CHANGES' COUNTERS ASK: needs=fix
// lists an owner's live changes — the asker's own where it names none — whose
// reviewer's standing verdict asks for changes or whose finding nobody has
// answered, newest first, in the state it names, open where it names none;
// an author's answer to the last open finding takes a change off it; and
// needs=me is what waits on the asker, the changes asking its review and then
// its own awaiting a fix, a change of both listed once. A need is refused in a
// state that is not live, beside a reviewer or an owner it does not take, and
// for a credential bound to no seat.
//
// Mutations: drop the fix test on the rows, and dev's approved change is
// listed as awaiting a fix; drop the second half's leaving out of the asks,
// and the founder's own change asked of them is listed twice.
func TestWhatWaitsOnAnOwnersFixIsWhatTheirCountersAsk(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	web := changeRepo(t, n, "web")
	app := changeRepo(t, n, "app")
	n.push(app, entry(uuid.Must(uuid.NewV7()).String(), set("refs/heads/fix", code.ZeroOID, oidOf("F"))))
	open := func(repo, source string, owner code.Principal) code.ChangeResult {
		t.Helper()
		res, err := c.ProposeChange(ctx, opening(repo, source, mainRef, owner))
		requireChanged(t, res, err)
		return res
	}
	web1, web2 := open(web, featureRef, dev), open(web, releaseRef, dev)
	app1, app2 := open(app, featureRef, dev), open(app, releaseRef, cto)
	app3 := open(app, "refs/heads/fix", ada)
	converse := func(cw code.ConversationWrite) code.ConversationResult {
		t.Helper()
		res, err := w.Converse(ctx, cw)
		requireConversed(t, res, err)
		return res
	}
	converse(reviewing(web, web1.ChangeID, platLead, 1, code.VerdictRequestChanges, nil))
	web2Finding := converse(reviewing(web, web2.ChangeID, platLead, 1, code.VerdictComment, nil,
		raising(code.SeverityMinor, 1, "A long line"))).FindingIDs[0]
	converse(reviewing(app, app1.ChangeID, platLead, 1, code.VerdictApprove, nil))
	converse(reviewing(app, app2.ChangeID, platLead, 1, code.VerdictRequestChanges, nil))
	converse(asking(app, app2.ChangeID, cto, "dev"))
	converse(reviewing(app, app3.ChangeID, platLead, 1, code.VerdictRequestChanges, nil))
	converse(asking(app, app3.ChangeID, cto, "founder"))

	for name, tc := range map[string]struct {
		q    code.ReadQuery
		want []string
	}{
		"what waits on dev's fix": {code.ReadQuery{Asker: dev, Needs: code.NeedFix}, []string{"web!2", "web!1"}},
		"what waits on cto's fix, asked by a person": {code.ReadQuery{Asker: ada, Needs: code.NeedFix,
			Owner: "cto"}, []string{"app!2"}},
		"what waits on dev's fix on one repository": {code.ReadQuery{Asker: dev, Needs: code.NeedFix,
			Repo: "web"}, []string{"web!2", "web!1"}},
		"dev's drafts awaiting a fix": {code.ReadQuery{Asker: dev, Needs: code.NeedFix,
			State: code.ChangeStateDraft}, nil},
		"what waits on dev": {code.ReadQuery{Asker: dev, Needs: code.NeedMe},
			[]string{"app!2", "web!2", "web!1"}},
		"what waits on the founder, a change of both once": {code.ReadQuery{Asker: ada, Needs: code.NeedMe},
			[]string{"app!3"}},
	} {
		for _, limit := range []int{1, 50} {
			if got, _ := listChanges(t, n, tc.q, limit); !slices.Equal(got, tc.want) {
				t.Errorf("%s, %d a page: listed %v, want %v", name, limit, got, tc.want)
			}
		}
	}

	converse(answering(web, web2.ChangeID, dev, code.FindingResolved, web2Finding))
	if got, _ := listChanges(t, n, code.ReadQuery{Asker: dev, Needs: code.NeedFix}, 50); !slices.Equal(got,
		[]string{"web!1"}) {
		t.Fatalf("once its finding is answered dev's fixes are %v, want web!1 alone", got)
	}

	unbound := code.Principal{Kind: code.PrincipalOperator, OperatorID: "token:ci"}
	for name, q := range map[string]code.ReadQuery{
		"a need in a state that is not live": {Asker: dev, Needs: code.NeedReview,
			State: code.ChangeStateClosed},
		"a fix beside a reviewer":                {Asker: dev, Needs: code.NeedFix, Reviewer: "plat-lead"},
		"what waits on me beside an owner":       {Asker: dev, Needs: code.NeedMe, Owner: "cto"},
		"a fix of a credential bound to no seat": {Asker: unbound, Needs: code.NeedFix},
		"a need this build does not know":        {Asker: dev, Needs: "merge"},
	} {
		q.Kind, q.Fresh = code.ReadChanges, stale
		if _, err := n.reader.Read(ctx, q); !errors.Is(err, code.ErrInvalid) {
			t.Errorf("%s: Read = %v, want ErrInvalid", name, err)
		}
	}
}
