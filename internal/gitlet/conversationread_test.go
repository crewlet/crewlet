package gitlet_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
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
	open := func(repo, name, source string) gitlet.ChangeResult {
		res, err := c.ProposeChange(ctx, opening(repo, source, mainRef, dev))
		requireChanged(t, res, err)
		refs[res.ChangeID] = gitlet.ChangeRef(name, res.Number)
		return res
	}
	web1, web2 := open(web, "web", featureRef), open(web, "web", releaseRef)
	app1, app2 := open(app, "app", featureRef), open(app, "app", releaseRef)
	for _, ask := range []gitlet.ConversationWrite{
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
		q    gitlet.ReadQuery
		want []string
	}{
		"plat-lead's standing asks, a closed change's among them": {
			gitlet.ReadQuery{Asker: cto, Reviewer: "plat-lead"}, newestFirst(refs, web1.ChangeID, web2.ChangeID)},
		"what waits on plat-lead's review, asked by plat-lead": {
			gitlet.ReadQuery{Asker: platLead, Needs: gitlet.NeedReview}, newestFirst(refs, web2.ChangeID)},
		"what waits on a person's review, by the seat they hold": {
			gitlet.ReadQuery{Asker: ada, Needs: gitlet.NeedReview}, newestFirst(refs, web2.ChangeID, app2.ChangeID)},
		"what waits on the founder's review, asked by another": {
			gitlet.ReadQuery{Asker: cto, Reviewer: "founder", Needs: gitlet.NeedReview},
			newestFirst(refs, web2.ChangeID, app2.ChangeID)},
		"the founder's asks on one repository": {
			gitlet.ReadQuery{Asker: cto, Reviewer: "founder", Repo: "app"}, newestFirst(refs, app2.ChangeID)},
		"plat-lead's asks of closed changes": {
			gitlet.ReadQuery{Asker: cto, Reviewer: "plat-lead", State: gitlet.ChangeStateClosed},
			newestFirst(refs, web1.ChangeID)},
		"plat-lead's asks of another owner's changes": {
			gitlet.ReadQuery{Asker: cto, Reviewer: "plat-lead", Owner: "cto"}, nil},
		"the asks of a reviewer nobody asked": {gitlet.ReadQuery{Asker: cto, Reviewer: "vp-sales"}, nil},
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
func settling(repo, change string, state gitlet.ReviewerState) gitlet.ConversationWrite {
	return conversing(gitlet.Principal{}, func(w *gitlet.ConversationWrite) {
		w.Asks = &gitlet.AskSettle{RepoID: repo, ChangeID: change, State: state}
	})
}

// askSettles is the changes of repo whose asks the reader names due a
// settling, read as a home reads its first page.
func (n *node) askSettles(repo string) []gitlet.AskDue {
	n.t.Helper()
	q := gitlet.ReadQuery{Kind: gitlet.ReadChangesDue, Fresh: stale,
		ChangesDue: &gitlet.ChangesDueQuery{RepoID: repo, RecheckBefore: harnessAt}}
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
	open := func(source string) gitlet.ChangeResult {
		t.Helper()
		if source != featureRef && source != releaseRef {
			n.push(web, entry(uuid.Must(uuid.NewV7()).String(), set(source, gitlet.ZeroOID, oidOf(source)))) //nolint:contextcheck // the harness publishes under the test's context, which ctx is
		}
		res, err := c.ProposeChange(ctx, opening(web, source, mainRef, dev))
		requireChanged(t, res, err)
		refs[res.ChangeID] = gitlet.ChangeRef("web", res.Number)
		return res
	}
	converse := func(cw gitlet.ConversationWrite) gitlet.ConversationResult {
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
	converse(reviewing(web, reviewed.ChangeID, platLead, 1, gitlet.VerdictComment, nil))
	ended := open("refs/heads/ended")
	converse(asking(web, ended.ChangeID, cto, "plat-lead"))
	closed, err := c.ProposeChange(ctx, closing(web, ended.ChangeID, dev, ended.Version))
	requireChanged(t, closed, err)

	// DUE ITS SETTLING, and the decide holds the direction to its state.
	if got := n.askSettles(web); len(got) != 1 || got[0] != (gitlet.AskDue{ChangeID: ended.ChangeID,
		State: gitlet.ReviewerEnded}) {
		t.Fatalf("the due listing names %+v, want the closed change's asks due their end", got)
	}
	if !slices.Contains(n.dueRepos(), web) {
		t.Fatalf("the due repositories %v leave out web, whose closed change's asks still wait", n.dueRepos())
	}
	// A LIVE CHANGE'S ASKS DO NOT END, AND AN ENDED CHANGE'S ARE NOT ASKED
	// AGAIN: each refused superseded, appending nothing.
	for _, cw := range []gitlet.ConversationWrite{
		settling(web, waiting.ChangeID, gitlet.ReviewerEnded),
		settling(web, ended.ChangeID, gitlet.ReviewerRequested),
	} {
		before := n.end()
		res, err := w.Converse(ctx, cw)
		requireConversationRefused(t, n, before, res, err, gitlet.ConversationRefusalSuperseded, "")
	}
	converse(settling(web, ended.ChangeID, gitlet.ReviewerEnded))
	if got := n.askSettles(web); len(got) != 0 {
		t.Fatalf("the due listing still names %+v once the closed change's asks ended", got)
	}

	// ONE PAGE OF ONE HOLDS THE ONE ASK STILL WAITING.
	q := gitlet.ReadQuery{Kind: gitlet.ReadChanges, Asker: platLead, Needs: gitlet.NeedReview, Limit: 1, Fresh: stale}
	a, err := n.reader.Read(ctx, q)
	switch {
	case err != nil:
		t.Fatalf("read what waits on plat-lead's review: %v", err)
	case len(a.Changes) != 1 || a.Changes[0].Ref != refs[waiting.ChangeID]:
		t.Fatalf("the first page of one of what waits on plat-lead's review lists %+v, want %s alone",
			a.Changes, refs[waiting.ChangeID])
	}
	if got, _ := listChanges(t, n, gitlet.ReadQuery{Asker: cto, Reviewer: "plat-lead"}, 50); !slices.Equal(got,
		[]string{refs[waiting.ChangeID]}) {
		t.Fatalf("plat-lead's waiting asks are %v, want %s alone", got, refs[waiting.ChangeID])
	}

	// REOPENED, ITS ENDED ASK IS DUE AGAIN THE OTHER WAY, and waits once
	// settled.
	reopened, err := c.ProposeChange(ctx, reopening(web, ended.ChangeID, dev, closed.Version))
	requireChanged(t, reopened, err)
	if got := n.askSettles(web); len(got) != 1 || got[0] != (gitlet.AskDue{ChangeID: ended.ChangeID,
		State: gitlet.ReviewerRequested}) {
		t.Fatalf("the due listing names %+v, want the reopened change's asks due again", got)
	}
	converse(settling(web, ended.ChangeID, gitlet.ReviewerRequested))
	if got := n.askSettles(web); len(got) != 0 {
		t.Fatalf("the due listing still names %+v once the reopened change's asks wait again", got)
	}
	if got, _ := listChanges(t, n, gitlet.ReadQuery{Asker: cto, Reviewer: "plat-lead"}, 50); !slices.Equal(got,
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
	say := func(asker gitlet.Principal, parent, body string) string {
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
	for _, ask := range []gitlet.ConversationWrite{
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
	q := gitlet.ReadQuery{Asker: cto, Kind: gitlet.ReadChange, Repo: "web", Change: 1, Part: gitlet.PartComments,
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
	answer, err := n.reader.Read(ctx, gitlet.ReadQuery{Asker: cto, Kind: gitlet.ReadChange, Repo: "web", Change: 1,
		Part: gitlet.PartReviewers, Fresh: stale})
	if err != nil {
		t.Fatalf("read web!1's reviewers: %v", err)
	}
	if len(answer.Reviewers) != 2 || answer.Reviewers[0].Reviewer != "founder" ||
		answer.Reviewers[0].State != gitlet.ReviewerWithdrawn || answer.Reviewers[0].Kind != "person" ||
		answer.Reviewers[1].Reviewer != "plat-lead" || answer.Reviewers[1].State != gitlet.ReviewerRequested ||
		answer.Reviewers[1].RequestedBy != "cto" || answer.Next != "" || answer.Change != nil {
		t.Fatalf("web!1's reviewers read %+v", answer)
	}

	// A CURSOR OF ANOTHER PART.
	for name, q := range map[string]gitlet.ReadQuery{
		"comments after a reviewer": {Part: gitlet.PartComments, Cursor: "plat-lead"},
		"reviewers after a comment": {Part: gitlet.PartReviewers, Cursor: "12/" + first},
	} {
		q.Asker, q.Kind, q.Repo, q.Change, q.Fresh = cto, gitlet.ReadChange, "web", 1, stale
		if _, err := n.reader.Read(ctx, q); !errors.Is(err, gitlet.ErrBadCursor) {
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
	suggesting := raising(gitlet.SeverityMajor, 7, "Sleep between tries")
	suggesting.Suggestion = &suggestion
	res, err := w.Converse(ctx, reviewing(repo, change, platLead, 1, gitlet.VerdictRequestChanges, nil,
		raising(gitlet.SeverityBlocker, 3, "No backoff"), suggesting))
	first := requireReviewed(t, res, err)
	firstID := res.ID
	res, err = w.Converse(ctx, reviewing(repo, change, cto, 1, gitlet.VerdictComment, nil,
		raising(gitlet.SeverityNit, 9, "A name")))
	second := requireReviewed(t, res, err)
	secondID := res.ID
	res, err = w.Converse(ctx, conversing(dev, func(cw *gitlet.ConversationWrite) {
		cw.FindingResolve = &gitlet.FindingResolve{RepoID: repo, ChangeID: change, Findings: []gitlet.FindingResolution{
			{FindingID: first[0], Resolution: gitlet.FindingDisputed, Note: "the client backs off"}}}
	}))
	requireConversed(t, res, err)
	answeredAt := res.Position
	res, err = w.Converse(ctx, conversing(platLead, func(cw *gitlet.ConversationWrite) {
		cw.Comment = &gitlet.Comment{RepoID: repo, ChangeID: change, FindingID: first[0], Body: "It does not."}
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
	q := gitlet.ReadQuery{Asker: cto, Kind: gitlet.ReadChange, Repo: "web", Change: 1, Part: gitlet.PartReviews,
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
				rv.Verdict != gitlet.VerdictRequestChanges || rv.Findings != 2 || rv.Revision != 1 ||
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
	q = gitlet.ReadQuery{Asker: cto, Kind: gitlet.ReadChange, Repo: "web", Change: 1, Part: gitlet.PartFindings,
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
				if f.State != gitlet.FindingDisputed || f.ResolutionNote != "the client backs off" ||
					f.ResolvedBy != "dev" || f.ResolvedByKind != "agent" || f.ResolvedPosition != answeredAt ||
					f.Severity != gitlet.SeverityBlocker || f.Title != "No backoff" || f.Suggestion != nil ||
					f.ReviewID != firstID || f.Raiser != "plat-lead" || f.LineStart != 3 {
					t.Errorf("the disputed blocker reads %+v", f)
				}
			case first[1]:
				if f.Suggestion == nil || *f.Suggestion != suggestion || f.State != gitlet.FindingOpen ||
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
	answer, err := n.reader.Read(ctx, gitlet.ReadQuery{Asker: cto, Kind: gitlet.ReadChange, Repo: "web", Change: 1,
		Part: gitlet.PartComments, Fresh: stale})
	if err != nil || len(answer.Comments) != 1 || answer.Comments[0].ID != reply ||
		answer.Comments[0].FindingID != first[0] || answer.Comments[0].ThreadID != first[0] {
		t.Fatalf("web!1's comments read %+v, %v; want the reply in the finding's thread", answer.Comments, err)
	}

	// A CURSOR OF ANOTHER PART.
	for name, q := range map[string]gitlet.ReadQuery{
		"reviews after a finding":         {Part: gitlet.PartReviews, Cursor: first[0]},
		"findings after a review":         {Part: gitlet.PartFindings, Cursor: "3000"},
		"reviews after a comment's place": {Part: gitlet.PartReviews, Cursor: "12/" + reply},
	} {
		q.Asker, q.Kind, q.Repo, q.Change, q.Fresh = cto, gitlet.ReadChange, "web", 1, stale
		if _, err := n.reader.Read(ctx, q); !errors.Is(err, gitlet.ErrBadCursor) {
			t.Errorf("%s: Read = %v, want ErrBadCursor", name, err)
		}
	}
}

// A READ NAMING FINDINGS SERVES THOSE ALONE: a findings part narrowed to the
// findings a write names (ReadQuery.FindingIDs) answers each the change holds
// — whichever review raised it, however the part would page them — in id
// order, with its text, and none it does not hold and none it did not name;
// after a cursor, only those past it; and a narrowing of another part, of
// another kind, beside a limit, past MaxFindingsPerReview, naming one twice
// or naming no finding's id is refused.
//
// Mutations: serve the part's first page in place of the ids named → the
// unnamed finding is read, red; keep the read's order in place of the ids'
// → red; serve the ids before the cursor → red; let a limit beside them pass
// → red.
func TestAReadNamingFindingsServesThoseAlone(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	suggestion := "time.Sleep(backoff)"
	suggesting := raising(gitlet.SeverityMajor, 7, "Sleep between tries")
	suggesting.Suggestion = &suggestion
	res, err := w.Converse(ctx, reviewing(repo, change, platLead, 1, gitlet.VerdictRequestChanges, nil,
		raising(gitlet.SeverityBlocker, 3, "No backoff"), suggesting))
	first := requireReviewed(t, res, err)
	res, err = w.Converse(ctx, reviewing(repo, change, cto, 1, gitlet.VerdictComment, nil,
		raising(gitlet.SeverityNit, 9, "A name")))
	second := requireReviewed(t, res, err)
	unheld := uuid.Must(uuid.NewV7()).String()

	named := func(q gitlet.ReadQuery) []string {
		t.Helper()
		q.Asker, q.Kind, q.Repo, q.Change, q.Part, q.Fresh = cto, gitlet.ReadChange, "web", 1, gitlet.PartFindings, stale
		answer, err := n.reader.Read(ctx, q)
		if err != nil {
			t.Fatalf("read web!1's findings %v after %q: %v", q.FindingIDs, q.Cursor, err)
		}
		if answer.Next != "" {
			t.Errorf("three short findings named carry a cursor %q", answer.Next)
		}
		var ids []string
		for _, f := range answer.Findings {
			ids = append(ids, f.ID)
			if f.ID == first[1] && (f.Suggestion == nil || *f.Suggestion != suggestion || f.Raiser != "plat-lead") {
				t.Errorf("the suggesting finding reads %+v", f)
			}
		}
		return ids
	}
	want := []string{second[0], first[1]}
	slices.Sort(want)
	if got := named(gitlet.ReadQuery{FindingIDs: []string{second[0], unheld, first[1]}}); !slices.Equal(got, want) {
		t.Errorf("the findings named came %v, want %v: the two web!1 holds, by id, and not %s", got, want, first[0])
	}
	if got := named(gitlet.ReadQuery{FindingIDs: []string{second[0], first[1]}, Cursor: want[0]}); !slices.Equal(got,
		want[1:]) {
		t.Errorf("the findings named after %s came %v, want %v", want[0], got, want[1:])
	}

	many := make([]string, gitlet.MaxFindingsPerReview+1)
	for i := range many {
		many[i] = uuid.Must(uuid.NewV7()).String()
	}
	for name, q := range map[string]gitlet.ReadQuery{
		"another part":     {Kind: gitlet.ReadChange, Change: 1, Part: gitlet.PartReviews, FindingIDs: first},
		"the revisions":    {Kind: gitlet.ReadChange, Change: 1, FindingIDs: first},
		"a listing":        {Kind: gitlet.ReadChanges, FindingIDs: first},
		"beside a limit":   {Kind: gitlet.ReadChange, Change: 1, Part: gitlet.PartFindings, FindingIDs: first, Limit: 1},
		"past the bound":   {Kind: gitlet.ReadChange, Change: 1, Part: gitlet.PartFindings, FindingIDs: many},
		"one named twice":  {Kind: gitlet.ReadChange, Change: 1, Part: gitlet.PartFindings, FindingIDs: []string{first[0], first[0]}},
		"no finding's id":  {Kind: gitlet.ReadChange, Change: 1, Part: gitlet.PartFindings, FindingIDs: []string{"f-1"}},
		"an uppercased id": {Kind: gitlet.ReadChange, Change: 1, Part: gitlet.PartFindings, FindingIDs: []string{strings.ToUpper(first[0])}},
	} {
		q.Asker, q.Repo, q.Fresh = cto, "web", stale
		if _, err := n.reader.Read(ctx, q); !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(),
			"finding_ids") {
			t.Errorf("%s: Read = %v, want ErrInvalid naming finding_ids", name, err)
		}
	}
}

// THE STANDING PART IS THE ASKER'S OWN STANDING FINDINGS, WHOLE: a change's
// `standing` part answers the findings its asker keeps standing — open, or
// answered by an author — by id, a page at a time to the last, and none they
// dismissed and none another reviewer raised: what a review of theirs
// carries, never a page of the change's whole history. A cursor that is no
// finding's id is refused.
//
// Mutations: drop the cursor's filter → the second page repeats the first,
// red; drop the cursor a page past the limit carries → the read stops after
// its first page, red.
func TestTheStandingPartIsTheAskersOwnStandingFindingsWhole(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	res, err := w.Converse(ctx, reviewing(repo, change, platLead, 1, gitlet.VerdictRequestChanges, nil,
		raising(gitlet.SeverityBlocker, 3, "No backoff"), raising(gitlet.SeverityMajor, 7, "Sleep between tries"),
		raising(gitlet.SeverityNit, 11, "A comment")))
	own := requireReviewed(t, res, err)
	res, err = w.Converse(ctx, reviewing(repo, change, cto, 1, gitlet.VerdictComment, nil,
		raising(gitlet.SeverityNit, 9, "A name")))
	others := requireReviewed(t, res, err)
	res, err = w.Converse(ctx, conversing(dev, func(cw *gitlet.ConversationWrite) {
		cw.FindingResolve = &gitlet.FindingResolve{RepoID: repo, ChangeID: change, Findings: []gitlet.FindingResolution{
			{FindingID: own[0], Resolution: gitlet.FindingDisputed, Note: "the client backs off"}}}
	}))
	requireConversed(t, res, err)
	res, err = w.Converse(ctx, conversing(platLead, func(cw *gitlet.ConversationWrite) {
		cw.FindingResolve = &gitlet.FindingResolve{RepoID: repo, ChangeID: change, Findings: []gitlet.FindingResolution{
			{FindingID: own[1], Resolution: gitlet.FindingDismissed, Note: "it sleeps already"}}}
	}))
	requireConversed(t, res, err)

	standing := func(asker gitlet.Principal, limit int) []string {
		t.Helper()
		q := gitlet.ReadQuery{Asker: asker, Kind: gitlet.ReadChange, Repo: "web", Change: 1, Part: gitlet.PartStanding,
			Limit: limit, Fresh: stale}
		var ids []string
		for pages := 1; ; pages++ {
			answer, err := n.reader.Read(ctx, q)
			if err != nil {
				t.Fatalf("read web!1's standing findings as %s: %v", asker.Seat, err)
			}
			if answer.Change != nil || len(answer.Reviews) > 0 || len(answer.Comments) > 0 {
				t.Fatalf("a page of standing findings carries more than findings: %+v", answer)
			}
			for _, f := range answer.Findings {
				ids = append(ids, f.ID)
				if f.ID == own[0] && (f.State != gitlet.FindingDisputed || f.Title != "No backoff") {
					t.Errorf("the disputed blocker reads %+v", f)
				}
			}
			if answer.Next == "" {
				return ids
			}
			if pages > 4 {
				t.Fatalf("a few standing findings took more than four pages: %v", ids)
			}
			q.Cursor = answer.Next
		}
	}
	want := []string{own[0], own[2]}
	slices.Sort(want)
	if got := standing(platLead, 1); !slices.Equal(got, want) {
		t.Errorf("plat-lead's standing findings a page of one at a time came %v, want %v: the disputed and the "+
			"open one, by id, and neither the one dismissed nor cto's", got, want)
	}
	if got := standing(platLead, 0); !slices.Equal(got, want) {
		t.Errorf("plat-lead's standing findings in one page came %v, want %v", got, want)
	}
	if got := standing(cto, 0); !slices.Equal(got, others) {
		t.Errorf("cto's standing findings came %v, want %v", got, others)
	}
	if got := standing(dev, 0); len(got) != 0 {
		t.Errorf("dev, who raised none, keeps %v standing", got)
	}
	if _, err := n.reader.Read(ctx, gitlet.ReadQuery{Asker: platLead, Kind: gitlet.ReadChange, Repo: "web", Change: 1,
		Part: gitlet.PartStanding, Cursor: "3000", Fresh: stale}); !errors.Is(err, gitlet.ErrBadCursor) {
		t.Errorf("a standing page after a review's cursor: Read = %v, want ErrBadCursor", err)
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
	n.push(app, entry(uuid.Must(uuid.NewV7()).String(), set("refs/heads/fix", gitlet.ZeroOID, oidOf("F"))))
	open := func(repo, source string, owner gitlet.Principal) gitlet.ChangeResult {
		t.Helper()
		res, err := c.ProposeChange(ctx, opening(repo, source, mainRef, owner))
		requireChanged(t, res, err)
		return res
	}
	web1, web2 := open(web, featureRef, dev), open(web, releaseRef, dev)
	app1, app2 := open(app, featureRef, dev), open(app, releaseRef, cto)
	app3 := open(app, "refs/heads/fix", ada)
	converse := func(cw gitlet.ConversationWrite) gitlet.ConversationResult {
		t.Helper()
		res, err := w.Converse(ctx, cw)
		requireConversed(t, res, err)
		return res
	}
	converse(reviewing(web, web1.ChangeID, platLead, 1, gitlet.VerdictRequestChanges, nil))
	web2Finding := converse(reviewing(web, web2.ChangeID, platLead, 1, gitlet.VerdictComment, nil,
		raising(gitlet.SeverityMinor, 1, "A long line"))).FindingIDs[0]
	converse(reviewing(app, app1.ChangeID, platLead, 1, gitlet.VerdictApprove, nil))
	converse(reviewing(app, app2.ChangeID, platLead, 1, gitlet.VerdictRequestChanges, nil))
	converse(asking(app, app2.ChangeID, cto, "dev"))
	converse(reviewing(app, app3.ChangeID, platLead, 1, gitlet.VerdictRequestChanges, nil))
	converse(asking(app, app3.ChangeID, cto, "founder"))

	for name, tc := range map[string]struct {
		q    gitlet.ReadQuery
		want []string
	}{
		"what waits on dev's fix": {gitlet.ReadQuery{Asker: dev, Needs: gitlet.NeedFix}, []string{"web!2", "web!1"}},
		"what waits on cto's fix, asked by a person": {gitlet.ReadQuery{Asker: ada, Needs: gitlet.NeedFix,
			Owner: "cto"}, []string{"app!2"}},
		"what waits on dev's fix on one repository": {gitlet.ReadQuery{Asker: dev, Needs: gitlet.NeedFix,
			Repo: "web"}, []string{"web!2", "web!1"}},
		"dev's drafts awaiting a fix": {gitlet.ReadQuery{Asker: dev, Needs: gitlet.NeedFix,
			State: gitlet.ChangeStateDraft}, nil},
		"what waits on dev": {gitlet.ReadQuery{Asker: dev, Needs: gitlet.NeedMe},
			[]string{"app!2", "web!2", "web!1"}},
		"what waits on the founder, a change of both once": {gitlet.ReadQuery{Asker: ada, Needs: gitlet.NeedMe},
			[]string{"app!3"}},
	} {
		for _, limit := range []int{1, 50} {
			if got, _ := listChanges(t, n, tc.q, limit); !slices.Equal(got, tc.want) {
				t.Errorf("%s, %d a page: listed %v, want %v", name, limit, got, tc.want)
			}
		}
	}

	converse(answering(web, web2.ChangeID, dev, gitlet.FindingResolved, web2Finding))
	if got, _ := listChanges(t, n, gitlet.ReadQuery{Asker: dev, Needs: gitlet.NeedFix}, 50); !slices.Equal(got,
		[]string{"web!1"}) {
		t.Fatalf("once its finding is answered dev's fixes are %v, want web!1 alone", got)
	}

	unbound := gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "token:ci"}
	for name, q := range map[string]gitlet.ReadQuery{
		"a need in a state that is not live": {Asker: dev, Needs: gitlet.NeedReview,
			State: gitlet.ChangeStateClosed},
		"a fix beside a reviewer":                {Asker: dev, Needs: gitlet.NeedFix, Reviewer: "plat-lead"},
		"what waits on me beside an owner":       {Asker: dev, Needs: gitlet.NeedMe, Owner: "cto"},
		"a fix of a credential bound to no seat": {Asker: unbound, Needs: gitlet.NeedFix},
		"a need this build does not know":        {Asker: dev, Needs: "merge"},
	} {
		q.Kind, q.Fresh = gitlet.ReadChanges, stale
		if _, err := n.reader.Read(ctx, q); !errors.Is(err, gitlet.ErrInvalid) {
			t.Errorf("%s: Read = %v, want ErrInvalid", name, err)
		}
	}
}
