package code_test

import (
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
)

// A WINDOW LISTS EACH CHANGE ONCE, AT ITS LAST MOVE: the later of the last
// record on its repository's subject that changed its row and the last one
// somebody wrote about it on its own — newest move first, a page at a time,
// whatever the page's size. A change opened three days ago and commented on
// today is in today's window, at the comment; one opened three days ago and
// left alone is in the week's and not the day's. A live state beside the
// window leads its own walk instead, newest opening first, with the window
// tested on each row.
//
// Mutation: keep a change from the conversation's index where its lifecycle
// moved it as late — or from the lifecycle's where somebody spoke of it later
// — and the commented change is listed twice; date a change by its
// lifecycle alone, and the comment moves nothing; let the window lead a live
// state, and the open changes in the week come newest move first.
func TestAWindowListsEachChangeOnceAtItsLastMove(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	// EVERY RECORD A MINUTE AFTER THE ONE BEFORE, from an instant the test
	// moves: what the code log's broker would have stamped.
	var days atomic.Int64
	n.storedAt = func(seq uint64, _ time.Time) time.Time {
		return harnessAt.Add(time.Duration(days.Load())*24*time.Hour + time.Duration(seq)*time.Minute)
	}
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	web := changeRepo(t, n, "web")
	const topicRef = "refs/heads/topic"
	pushedBy(t, c, web, ada, update(topicRef, code.ZeroOID, oidOf("T")))
	var opened []code.ChangeResult
	for _, source := range []string{featureRef, releaseRef, topicRef} {
		res, err := c.ProposeChange(ctx, opening(web, source, mainRef, dev))
		requireChanged(t, res, err)
		opened = append(opened, res)
	}
	days.Store(3)
	said, err := w.Converse(ctx, saying(web, opened[0].ChangeID, ada, "", "Is this still wanted?"))
	requireConversed(t, said, err)
	readAt := harnessAt.Add(3*24*time.Hour + time.Hour)
	n.readAt.Store(&readAt)

	for _, tc := range []struct {
		window code.ChangeWindow
		state  code.ChangeState
		want   []string
	}{
		// THE DAY: the change spoken of today alone.
		{code.WindowDay, "", []string{"web!1"}},
		// THE WEEK: every one, the one spoken of today first, then the
		// rest by when they were opened, newest first.
		{code.WindowWeek, "", []string{"web!1", "web!3", "web!2"}},
		// A LIVE STATE BESIDE THE WINDOW leads its own walk, newest opening
		// first, and the window is tested on each row it read: web!2 and
		// web!3 are open and outside the day.
		{code.WindowWeek, code.ChangeStateOpen, []string{"web!3", "web!2", "web!1"}},
		{code.WindowDay, code.ChangeStateOpen, []string{"web!1"}},
		// A TERMINAL STATE BESIDE THE WINDOW is tested on the rows the
		// window's walk read, and keeps its order.
		{code.WindowWeek, code.ChangeStateClosed, nil},
	} {
		for _, limit := range []int{1, 2, 50} {
			q := code.ReadQuery{Asker: ada, Window: tc.window, State: tc.state}
			if got, _ := listChanges(t, n, q, limit); !slices.Equal(got, tc.want) {
				t.Errorf("window %s, state %q, %d a page: listed %v, want %v", tc.window, tc.state, limit, got,
					tc.want)
			}
		}
	}

	// THE INSTANT A ROW SAYS is the last move the walk listed it at: the
	// comment's for the change spoken of, its open's for the others.
	answer, err := n.reader.Read(ctx, code.ReadQuery{Kind: code.ReadChanges, Asker: ada, Window: code.WindowWeek,
		Fresh: stale})
	if err != nil {
		t.Fatalf("Read(changes in the week): %v", err)
	}
	if len(answer.Changes) != 3 {
		t.Fatalf("the week listed %d changes, want 3", len(answer.Changes))
	}
	spoken, last := answer.Changes[0], answer.Changes[2]
	if !spoken.UpdatedAt.After(last.UpdatedAt.Add(48 * time.Hour)) { //fixedwait:stated both are record instants the case's storedAt hook states, three days apart
		t.Errorf("web!1 was last moved at %s and web!2 at %s: the comment three days on moved web!1",
			spoken.UpdatedAt, last.UpdatedAt)
	}
	if !last.UpdatedAt.Equal(last.OpenedAt) {
		t.Errorf("web!2 was last moved at %s, want its open at %s: nothing moved it since", last.UpdatedAt,
			last.OpenedAt)
	}
	// A CHANGE READ SAYS THE SAME INSTANT as the listing, from the one
	// conversation row its detail reads, and that row says when somebody
	// last wrote on the change.
	//
	// Mutation: date the detail by its lifecycle alone, or leave the
	// conversation's instant unread, and web!1's detail says its open.
	for _, s := range answer.Changes {
		got, err := readChange(t, n, ada, "", "web", s.Number, 0, "")
		if err != nil || got.Change == nil {
			t.Fatalf("read %s: %+v, %v", s.Ref, got, err)
		}
		if !got.Change.UpdatedAt.Equal(s.UpdatedAt) {
			t.Errorf("%s's detail says it last moved at %s, and the week's listing %s", s.Ref,
				got.Change.UpdatedAt, s.UpdatedAt)
		}
		if s.Ref == spoken.Ref && (got.Change.Conversation == nil ||
			!got.Change.Conversation.SpokenAt.Equal(spoken.UpdatedAt)) {
			t.Errorf("%s's conversation says %+v, want it last written on at the comment, %s", s.Ref,
				got.Change.Conversation, spoken.UpdatedAt)
		}
	}
	// A REPOSITORY BESIDE THE WINDOW leads its own walk, and the window is
	// tested on the rows it read.
	if got, _ := listChanges(t, n, code.ReadQuery{Asker: ada, Repo: "web", Window: code.WindowDay}, 1); !slices.Equal(
		got, []string{"web!1"}) {
		t.Errorf("web's changes in the day: listed %v, want [web!1]", got)
	}

	// A PAGE THE STATE EMPTIED STILL GOES ON where the walk of an index
	// stopped: with web!2 closed and web!3 spoken of after it, a page of one
	// of the closed changes in the week reads web!2 from the lifecycle's
	// index and web!3 from the conversation's, keeps web!3 alone — past
	// web!2, which the next page reads — and the state drops it. Its cursor
	// is where the walk stopped, never the end of the listing.
	//
	// Mutation: end a page that filled none of its rows where the rows it
	// kept end, and the closed changes in the week are listed as none.
	closed, err := c.ProposeChange(ctx, closing(web, opened[1].ChangeID, dev, opened[1].Version))
	requireChanged(t, closed, err)
	said, err = w.Converse(ctx, saying(web, opened[2].ChangeID, ada, "", "Still on it?"))
	requireConversed(t, said, err)
	for _, tc := range []struct {
		window code.ChangeWindow
		state  code.ChangeState
		want   []string
	}{
		{code.WindowWeek, code.ChangeStateOpen, []string{"web!3", "web!1"}},
		{code.WindowWeek, code.ChangeStateClosed, []string{"web!2"}},
		{code.WindowDay, "", []string{"web!3", "web!2", "web!1"}},
	} {
		for _, limit := range []int{1, 2, 50} {
			q := code.ReadQuery{Asker: ada, Window: tc.window, State: tc.state}
			if got, _ := listChanges(t, n, q, limit); !slices.Equal(got, tc.want) {
				t.Errorf("after web!2's close: window %s, state %q, %d a page: listed %v, want %v", tc.window,
					tc.state, limit, got, tc.want)
			}
		}
	}
}

// A LISTING SAYS HOW MANY OF EACH CHANGE'S FINDINGS STAND OPEN — none of its
// authors has answered them — read from its conversation in the listing's own
// transaction: a review raising two is two, an author's answer to one leaves
// one, and a change nobody has reviewed is none.
//
// Mutation: answer the listing's rows without their conversation's count, and
// every change lists none; answer a change read's without it, and web!1's
// detail says none.
func TestAListingSaysHowManyOfEachChangesFindingsStandOpen(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	c, _ := committerOn(t, n, n.pub)
	quiet, err := c.ProposeChange(t.Context(), opening(repo, releaseRef, mainRef, dev))
	requireChanged(t, quiet, err)
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	res, err := w.Converse(ctx, reviewing(repo, change, platLead, 1, code.VerdictRequestChanges, nil,
		raising(code.SeverityBlocker, 3, "No backoff"), raising(code.SeverityNit, 9, "A name")))
	raised := requireReviewed(t, res, err)
	open := func() map[string]int64 {
		t.Helper()
		answer, err := n.reader.Read(ctx, code.ReadQuery{Kind: code.ReadChanges, Asker: ada, Repo: "web",
			Fresh: stale})
		if err != nil {
			t.Fatalf("Read(web's changes): %v", err)
		}
		out := map[string]int64{}
		for _, s := range answer.Changes {
			out[s.Ref] = s.OpenFindings
		}
		return out
	}
	if got := open(); got["web!1"] != 2 || got["web!2"] != 0 {
		t.Errorf("open findings after a review raising two on web!1: %v, want web!1 2 and web!2 0", got)
	}
	// A CHANGE READ SAYS THE SAME COUNT, from the conversation row its
	// detail reads.
	if got, err := readChange(t, n, ada, "", "web", 1, 0, ""); err != nil || got.Change == nil ||
		got.Change.OpenFindings != 2 {
		t.Errorf("web!1's detail after a review raising two = %+v, %v; want 2 open findings", got.Change, err)
	}
	res, err = w.Converse(ctx, conversing(dev, func(cw *code.ConversationWrite) {
		cw.FindingResolve = &code.FindingResolve{RepoID: repo, ChangeID: change, Findings: []code.FindingResolution{
			{FindingID: raised[1], Resolution: code.FindingResolved, Note: "renamed"}}}
	}))
	requireConversed(t, res, err)
	if got := open(); got["web!1"] != 1 {
		t.Errorf("open findings after an author answered one of web!1's two: %d, want 1", got["web!1"])
	}
}
