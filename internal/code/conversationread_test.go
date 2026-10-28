package code_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

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
// closed change's ask still stands; needs=review keeps the live changes alone,
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
