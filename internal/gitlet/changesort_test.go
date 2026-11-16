package gitlet_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// The branches the live listing's cases open changes from, beside a change repository's own.
const (
	liveTopicRef  = "refs/heads/topic"
	liveMergedRef = "refs/heads/merged"
)

// liveWorld is two repositories whose changes moved in a known order, every record a minute
// after the one before: web!1, web!2, app!1, app!2 and web!3 opened, app!3 opened as a draft,
// web!4 opened and merged; then web!1 commented on, web!2's head pushed, and web!4 commented on.
func liveWorld(t *testing.T) *node {
	t.Helper()
	n := newNode(t)
	n.chart = testChart()
	n.storedAt = func(seq uint64, _ time.Time) time.Time { return harnessAt.Add(time.Duration(seq) * time.Minute) }
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	web, app := changeRepo(t, n, "web"), changeRepo(t, n, "app")
	pushedBy(t, c, web, ada, update(liveTopicRef, gitlet.ZeroOID, oidOf("T")),
		update(liveMergedRef, gitlet.ZeroOID, oidOf("M")))
	pushedBy(t, c, app, ada, update(liveTopicRef, gitlet.ZeroOID, oidOf("T")))
	open := func(repo, source string, who gitlet.Principal, draft bool) gitlet.ChangeResult {
		t.Helper()
		p := opening(repo, source, mainRef, who)
		p.Open.Draft = draft
		res, err := c.ProposeChange(ctx, p)
		requireChanged(t, res, err)
		return res
	}
	web1 := open(web, featureRef, dev, false)
	open(web, releaseRef, cto, false)
	open(app, featureRef, dev, false)
	open(app, releaseRef, cto, false)
	open(web, liveTopicRef, dev, false)
	open(app, liveTopicRef, ada, true)
	web4 := open(web, liveMergedRef, dev, false)
	pushedBy(t, c, web, ada, update(mainRef, oidOf("A"), oidOf("M")))
	said, err := w.Converse(ctx, saying(web, web1.ChangeID, ada, "", "Is this still wanted?"))
	requireConversed(t, said, err)
	pushedBy(t, c, web, ada, update(releaseRef, oidOf("R"), oidOf("R2")))
	said, err = w.Converse(ctx, saying(web, web4.ChangeID, ada, "", "Merged, thanks."))
	requireConversed(t, said, err)
	return n
}

// A LIVE STATE IS LISTED BY WHEN ITS CHANGES LAST MOVED: newest move first, each live change once,
// a change spoken of after its lifecycle last moved at the word, the rest at their lifecycle's
// last move, whatever the page's size; a merged change somebody spoke of later is never listed.
//
// Mutation: date a change by its lifecycle alone, keep a change from both sides, or seek the live
// index without its state.
func TestALiveStateIsListedByWhenItsChangesLastMoved(t *testing.T) {
	t.Parallel()
	n := liveWorld(t)
	for _, tc := range []struct {
		repo, owner string
		state       gitlet.ChangeState
		want        []string
	}{
		{"", "", gitlet.ChangeStateOpen, []string{"web!2", "web!1", "web!3", "app!2", "app!1"}},
		{"web", "", gitlet.ChangeStateOpen, []string{"web!2", "web!1", "web!3"}},
		{"app", "", gitlet.ChangeStateOpen, []string{"app!2", "app!1"}},
		{"", "dev", gitlet.ChangeStateOpen, []string{"web!1", "web!3", "app!1"}},
		{"web", "dev", gitlet.ChangeStateOpen, []string{"web!1", "web!3"}},
		{"", "", gitlet.ChangeStateDraft, []string{"app!3"}},
		{"web", "", gitlet.ChangeStateDraft, nil},
	} {
		for _, limit := range []int{1, 2, 50} {
			q := gitlet.ReadQuery{Asker: ada, Repo: tc.repo, Owner: tc.owner, State: tc.state,
				ChangeSort: gitlet.ChangeSortUpdated}
			if got, _ := listChanges(t, n, q, limit); !slices.Equal(got, tc.want) {
				t.Errorf("repo %q, owner %q, state %s by last move, %d a page: listed %v, want %v", tc.repo,
					tc.owner, tc.state, limit, got, tc.want)
			}
		}
	}
	// THE ORDER IS THE INSTANT EACH ROW SAYS, newest first.
	answer, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadChanges, Asker: ada,
		State: gitlet.ChangeStateOpen, ChangeSort: gitlet.ChangeSortUpdated, Fresh: stale})
	if err != nil {
		t.Fatalf("Read(open changes by last move): %v", err)
	}
	for i := 1; i < len(answer.Changes); i++ {
		//fixedwait:stated record instants the case's storedAt hook states, a minute apart
		if prev, s := answer.Changes[i-1], answer.Changes[i]; !s.UpdatedAt.Before(prev.UpdatedAt) {
			t.Errorf("%s says it moved at %s, not before %s, listed above it at %s", s.Ref, s.UpdatedAt,
				prev.Ref, prev.UpdatedAt)
		}
	}
}

// A KEPT PREFIX OF A SORTED PAGE RESUMES WHERE IT STOPPED: a caller that keeps the first change of
// a page by last move and hands back its cursor ([gitlet.ReadQuery.ChangesCursor]) is answered the
// rest of that page first.
//
// Mutation: spell the sorted walk's cursor by opening.
func TestAKeptPrefixOfASortedPageResumesWhereItStopped(t *testing.T) {
	t.Parallel()
	n := liveWorld(t)
	q := gitlet.ReadQuery{Kind: gitlet.ReadChanges, Asker: ada, State: gitlet.ChangeStateOpen,
		ChangeSort: gitlet.ChangeSortUpdated, Limit: 3, Fresh: stale}
	page, err := n.reader.Read(t.Context(), q)
	if err != nil || len(page.Changes) != 3 {
		t.Fatalf("the first page of open changes by last move: %+v, %v", page, err)
	}
	q.Cursor = q.ChangesCursor(page.Changes[0])
	rest, err := n.reader.Read(t.Context(), q)
	if err != nil {
		t.Fatalf("resume after %s: %v", page.Changes[0].Ref, err)
	}
	refs := func(cs []gitlet.ChangeSummary) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.Ref)
		}
		return out
	}
	if got, want := refs(rest.Changes), refs(page.Changes[1:]); len(got) < len(want) ||
		!slices.Equal(got[:len(want)], want) {
		t.Errorf("resumed after %s: listed %v, want it to begin %v", page.Changes[0].Ref, got, want)
	}
}

// A LIVE LISTING STEPS OVER ENDED CHANGES' CONVERSATIONS WITHIN ITS BOUND: twelve merged changes
// spoken of after every open change last moved, web!1 among them, hold the conversations' index
// above the open ones, so a first page of two bounded to four of them answers short with a cursor,
// where the shipped bound fills it, and paging on lists the three open changes, each once, newest
// move first. A page that reads every live change reads no conversation, so one page holds them.
//
// Mutation: drop the conversation side's bound (web!1 is never listed), or answer a short page
// with no cursor.
func TestALiveListingStepsOverEndedChangesConversationsWithinItsBound(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	n.storedAt = func(seq uint64, _ time.Time) time.Time { return harnessAt.Add(time.Duration(seq) * time.Minute) }
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	web := changeRepo(t, n, "web")
	const ended = 12
	branches := []gitlet.Command{update(liveTopicRef, gitlet.ZeroOID, oidOf("T"))}
	for i := range ended {
		branches = append(branches, update(fmt.Sprintf("refs/heads/merged-%02d", i), gitlet.ZeroOID,
			oidOf(fmt.Sprint("M", i))))
	}
	pushedBy(t, c, web, ada, branches...)
	var oldest gitlet.ChangeResult
	for _, source := range []string{featureRef, releaseRef, liveTopicRef} {
		res, err := c.ProposeChange(ctx, opening(web, source, mainRef, dev))
		requireChanged(t, res, err)
		if oldest.ChangeID == "" {
			oldest = res
		}
	}
	main := oidOf("A")
	for i := range ended {
		res, err := c.ProposeChange(ctx, opening(web, fmt.Sprintf("refs/heads/merged-%02d", i), mainRef, dev))
		requireChanged(t, res, err)
		pushedBy(t, c, web, ada, update(mainRef, main, oidOf(fmt.Sprint("M", i))))
		main = oidOf(fmt.Sprint("M", i))
		said, err := w.Converse(ctx, saying(web, res.ChangeID, ada, "", "Merged."))
		requireConversed(t, said, err)
		if i == ended/2 {
			said, err = w.Converse(ctx, saying(web, oldest.ChangeID, ada, "", "Still wanted?"))
			requireConversed(t, said, err)
		}
	}
	want := []string{"web!1", "web!3", "web!2"}
	firstPage := func(repo string) gitlet.ReadAnswer {
		t.Helper()
		answer, err := n.reader.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadChanges, Asker: ada, Repo: repo,
			State: gitlet.ChangeStateOpen, ChangeSort: gitlet.ChangeSortUpdated, Limit: 2, Fresh: stale})
		if err != nil {
			t.Fatalf("repo %q, the first page of two: %v", repo, err)
		}
		return answer
	}
	listed := func(repo string, limit, minPages, maxPages int) {
		t.Helper()
		q := gitlet.ReadQuery{Asker: ada, Repo: repo, State: gitlet.ChangeStateOpen,
			ChangeSort: gitlet.ChangeSortUpdated}
		if got, pages := listChanges(t, n, q, limit); !slices.Equal(got, want) || pages < minPages ||
			pages > maxPages {
			t.Errorf("repo %q, %d a page: listed %v in %d pages, want %v in %d to %d", repo, limit, got, pages,
				want, minPages, maxPages)
		}
	}
	for _, repo := range []string{"", "web"} {
		if page := firstPage(repo); len(page.Changes) != 2 || page.Next == "" {
			t.Errorf("repo %q at the shipped bound: the first page of two answered %d changes, next %q; want "+
				"it full", repo, len(page.Changes), page.Next)
		}
		listed(repo, 2, 2, 100)
		listed(repo, 50, 1, 1)
	}
	gitlet.SetSpokenScanBound(n.reader, 4)
	for _, repo := range []string{"", "web"} {
		if page := firstPage(repo); len(page.Changes) >= 2 || page.Next == "" {
			t.Errorf("repo %q bounded to 4 conversations: the first page of two answered %d changes, next %q; "+
				"want a short page with a cursor", repo, len(page.Changes), page.Next)
		}
		listed(repo, 1, 4, 100)
		listed(repo, 2, 3, 100)
		listed(repo, 50, 1, 1)
	}
}

// A LIVE LISTING BY UPDATE KEEPS TO ITS WINDOW: the open changes that moved within it, newest move
// first, a change spoken of today whose lifecycle last moved a week ago among them, and none that
// moved only before it.
//
// Mutation: test the window on rows of a walk that never reads below it, or seek the lifecycle side
// alone.
func TestALiveListingByUpdateKeepsToItsWindow(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	var days atomic.Int64
	n.storedAt = func(seq uint64, _ time.Time) time.Time {
		return harnessAt.Add(time.Duration(days.Load())*24*time.Hour + time.Duration(seq)*time.Minute)
	}
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	web := changeRepo(t, n, "web")
	pushedBy(t, c, web, ada, update(liveTopicRef, gitlet.ZeroOID, oidOf("T")))
	var opened []gitlet.ChangeResult
	for _, source := range []string{featureRef, releaseRef, liveTopicRef} {
		res, err := c.ProposeChange(ctx, opening(web, source, mainRef, dev))
		requireChanged(t, res, err)
		opened = append(opened, res)
	}
	days.Store(8)
	said, err := w.Converse(ctx, saying(web, opened[0].ChangeID, ada, "", "Is this still wanted?"))
	requireConversed(t, said, err)
	pushedBy(t, c, web, ada, update(releaseRef, oidOf("R"), oidOf("R2")))
	readAt := harnessAt.Add(8*24*time.Hour + time.Hour)
	n.readAt.Store(&readAt)
	for _, tc := range []struct {
		window gitlet.ChangeWindow
		want   []string
	}{
		{gitlet.WindowDay, []string{"web!2", "web!1"}},
		{gitlet.WindowWeek, []string{"web!2", "web!1"}},
		{gitlet.WindowMonth, []string{"web!2", "web!1", "web!3"}},
		{"", []string{"web!2", "web!1", "web!3"}},
	} {
		for _, repo := range []string{"", "web"} {
			for _, limit := range []int{1, 50} {
				q := gitlet.ReadQuery{Asker: ada, Repo: repo, State: gitlet.ChangeStateOpen, Window: tc.window,
					ChangeSort: gitlet.ChangeSortUpdated}
				if got, _ := listChanges(t, n, q, limit); !slices.Equal(got, tc.want) {
					t.Errorf("repo %q, window %q by last move, %d a page: listed %v, want %v", repo, tc.window,
						limit, got, tc.want)
				}
			}
		}
	}
}

// A SORT IS REFUSED WHERE ITS WALK CANNOT ORDER IT, naming change_sort: an order this build does
// not know; updated beside a reviewer or what the changes wait on, whose walks keep the asks'
// order, and with no live state and no window nothing else leads; opened on a window nothing else
// leads, which is listed by last move. Updated on such a window, and opened on a live state, each
// answer what the listing unsorted answers.
//
// Mutation: relax a refusal into today's order.
func TestASortIsRefusedWhereItsWalkCannotOrderIt(t *testing.T) {
	t.Parallel()
	n := liveWorld(t)
	open, updated, opened := gitlet.ChangeStateOpen, gitlet.ChangeSortUpdated, gitlet.ChangeSortOpened
	for _, q := range []gitlet.ReadQuery{
		{State: open, ChangeSort: "sideways"},
		{State: open, ChangeSort: updated, Reviewer: "cto"},
		{State: open, ChangeSort: updated, Needs: gitlet.NeedReview},
		{State: open, ChangeSort: updated, Needs: gitlet.NeedFix},
		{State: open, ChangeSort: updated, Needs: gitlet.NeedMe},
		{State: open, ChangeSort: updated, Needs: gitlet.NeedPerson},
		{Repo: "web", ChangeSort: updated},
		{State: gitlet.ChangeStateMerged, ChangeSort: updated},
		{Work: "native:task-1", ChangeSort: updated},
		{Window: gitlet.WindowWeek, ChangeSort: opened},
	} {
		q.Kind, q.Asker, q.Fresh = gitlet.ReadChanges, ada, stale
		if _, err := n.reader.Read(t.Context(), q); !errors.Is(err, gitlet.ErrInvalid) ||
			!strings.Contains(err.Error(), "change_sort:") {
			t.Errorf("%+v = %v, want ErrInvalid naming change_sort", q, err)
		}
	}
	for _, tc := range []struct {
		sorted, unsorted gitlet.ReadQuery
	}{
		{gitlet.ReadQuery{Asker: ada, Window: gitlet.WindowWeek, ChangeSort: updated},
			gitlet.ReadQuery{Asker: ada, Window: gitlet.WindowWeek}},
		{gitlet.ReadQuery{Asker: ada, State: open, ChangeSort: opened}, gitlet.ReadQuery{Asker: ada, State: open}},
		{gitlet.ReadQuery{Asker: ada, Repo: "web", ChangeSort: opened}, gitlet.ReadQuery{Asker: ada, Repo: "web"}},
	} {
		got, _ := listChanges(t, n, tc.sorted, 2)
		want, _ := listChanges(t, n, tc.unsorted, 2)
		if len(want) == 0 || !slices.Equal(got, want) {
			t.Errorf("%+v listed %v, want what it lists unsorted: %v", tc.sorted, got, want)
		}
	}
}
