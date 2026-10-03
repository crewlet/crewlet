package tracker_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/tracker"
)

// EVERY READER THAT NAMES A TASK BY ITS KEY SAYS WHEN THE KEY OPENS ANOTHER.
//
// A key two tasks hold resolves to the one that claimed it, so a duplicate is
// reached only by its id — and the readers a task page, the company feed and a
// person's open questions are drawn from each hand a task onward by its key.
// [TestEveryRowAnItemIsOpenedFromSaysWhenItsKeyOpensAnotherTask] holds the
// board, the item, the inbox and a person's own work to it; this holds the
// rest: a task page's neighbours (`around=`), the task a child is filed under,
// the company feed's rows, and the asks a decision screen lists with the call
// that answers each.
//
// Mutation: read a neighbour's bare key in [segmentFirst] or [aroundListed],
// drop `key_collision` from the parent, the feed or the decisions page, or
// hand an ask's answer call the key — and the duplicate is reached as the
// claimant.
func TestEveryNewReaderSaysWhenAKeyOpensAnotherTask(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	at := time.Unix(1_700_000_100, 0).UTC()
	step := 0
	apply := func(rec tracker.MutationRecord) {
		t.Helper()
		step++
		rec.OpID = rec.OpID + "-" + itoa(step)
		if _, err := h.apply(rec, at.Add(time.Duration(step)*time.Second)); err != nil {
			t.Fatalf("apply %s: %v", rec.OpID, err)
		}
	}
	// t-1 CLAIMS ENG-7 and t-2 holds it beside it; t-4 is filed under the
	// duplicate. The titles are the order a `sort=title` board draws.
	for _, spec := range []struct{ id, key, title, parent string }{
		{"t-1", "ENG-7", "1 the claimant", ""},
		{"t-2", "ENG-7", "2 the duplicate", ""},
		{"t-4", "ENG-4", "3 the child", "t-2"},
	} {
		task := newTask(spec.id)
		task.Key, task.Title = spec.key, spec.title
		if spec.parent != "" {
			task.Parent = ptr(spec.parent)
		}
		apply(taskRecord(spec.id, tracker.OpCreate, task, &tracker.Notify{
			Kind: tracker.ChangeCreated, Snapshot: tracker.Snapshot{Key: spec.key},
		}))
	}
	apply(taskRecord("t-2", tracker.OpPatch, tracker.TaskPatch{
		Comment: &tracker.Comment{ID: "c-2", Task: "t-2", Author: "bo",
			AuthorKind: tracker.AuthorHuman, Body: "which one is this?",
			Ask: "ana", CreatedAt: at},
	}, &tracker.Notify{Kind: tracker.ChangeComment,
		Snapshot: tracker.Snapshot{Key: "ENG-7"}}))

	log, err := statelogtest.LocalReader(tracker.Domain{}, h.db.Replicated(),
		statelog.Position{Stream: tracker.Domain{}.Stream().Name})
	if err != nil {
		t.Fatalf("local read authority: %v", err)
	}
	reader, err := tracker.NewReader(h.db, log)
	if err != nil {
		t.Fatalf("tracker reader: %v", err)
	}
	ctx, now := t.Context(), at.Add(time.Hour)

	// A TASK PAGE'S NEIGHBOURS are addresses: the duplicate is stepped to by
	// its id, the claimant and the child by their keys.
	for around, want := range map[string][2]string{
		"t-1":   {"", "t-2"},
		"t-2":   {"ENG-7", "ENG-4"},
		"ENG-4": {"t-2", ""},
	} {
		answer, err := reader.Tasks(ctx, tracker.Query{
			Scope: tracker.Scope{Workspace: true}, Level: statelog.ReadStale,
			Sort: []tracker.Sort{{Key: "title"}}, Around: around,
		}, now)
		if err != nil {
			t.Fatalf("around %s: %v", around, err)
		}
		if answer.Around == nil {
			t.Fatalf("around %s placed nothing", around)
		}
		if got := [2]string{deref(answer.Around.Prev), deref(answer.Around.Next)}; got != want {
			t.Errorf("around %s steps to %q, want %q — a neighbour whose key "+
				"another task claimed is reached only by its id", around, got, want)
		}
	}

	// THE TASK A CHILD IS FILED UNDER says its key is not its address.
	child, err := reader.Task(ctx, "ENG-4", tracker.DetailWants{},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("open the child: %v", err)
	}
	if child.Parent == nil || child.Parent.ID != "t-2" || !child.Parent.KeyCollision {
		t.Errorf("the child names its parent as %+v, want t-2 flagged — its "+
			"key ENG-7 opens t-1", child.Parent)
	}

	// THE COMPANY FEED'S ROWS carry the flag beside the key.
	feed, err := reader.CompanyFeed(ctx, tracker.FeedQuery{
		Kinds: []tracker.FeedKind{tracker.FeedCreated}, Limit: tracker.MaxFeedPage,
		Level: statelog.ReadStale,
	})
	if err != nil {
		t.Fatalf("the company feed: %v", err)
	}
	fed := map[string]bool{}
	for _, row := range feed.Rows {
		fed[row.Task] = true
		if want := row.Task == "t-2"; row.KeyCollision != want {
			t.Errorf("the feed's row about %s carries key_collision=%v, want %v",
				row.Task, row.KeyCollision, want)
		}
	}
	if len(fed) != 3 {
		t.Fatalf("the feed carries creates of %v, want all three", fed)
	}

	// AND THE QUESTION WAITING ON ANA is on the duplicate, so its row is
	// flagged and the call it hands over answers on the duplicate's id.
	decisions, err := reader.Decisions(ctx, tracker.DecisionsQuery{
		Handle: "ana", Level: statelog.ReadStale,
	}, now, time.UTC)
	if err != nil {
		t.Fatalf("ana's decisions: %v", err)
	}
	if len(decisions.Asks) != 1 {
		t.Fatalf("ana has %d asks, want the one on the duplicate", len(decisions.Asks))
	}
	ask := decisions.Asks[0]
	if !ask.KeyCollision || !strings.Contains(ask.Answer, `item: "t-2"`) {
		t.Errorf("the ask on the duplicate is flagged %v and hands over %s — "+
			"answered through ENG-7 it would post on the claimant",
			ask.KeyCollision, ask.Answer)
	}
}

// deref is a pointer's string, or "" for none.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
