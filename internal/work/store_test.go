package work_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/work"
)

func newStore(t *testing.T) (*work.Store, coord.Documents) {
	t.Helper()
	docs := memory.NewFleet()
	s, err := work.NewStore(work.Options{Documents: docs})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return s, docs
}

func agent(handle string) work.Actor {
	return work.Actor{Handle: handle, Kind: work.AuthorAgent, TurnID: "t-" + handle}
}

func human(handle string) work.Actor {
	return work.Actor{Handle: handle, Kind: work.AuthorHuman}
}

func operator(id string) work.Actor {
	return work.Actor{Kind: work.AuthorOperator, OperatorID: id}
}

func file(t *testing.T, s *work.Store, actor work.Actor, in work.NewItem) work.Written {
	t.Helper()
	if in.Project == "" {
		in.Project = "ENG"
	}
	if in.Type == "" {
		in.Type = work.TypeTask
	}
	if in.Title == "" {
		in.Title = "a title"
	}
	got, err := s.Create(t.Context(), actor, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return got
}

func ptr[T any](v T) *T { return &v }

// A KEY IS MINTED FROM A COMPARE-AND-SET COUNTER, so two nodes filing at once
// never produce one key twice. A duplicate key is an item a person opens from
// a link somebody pasted into chat and finds is not the one they meant.
func TestKeysAreUniqueUnderConcurrentFiling(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)

	const writers = 12
	keys := make(chan string, writers)
	errs := make(chan error, writers)
	start := make(chan struct{})
	for i := range writers {
		go func() {
			<-start
			got, err := s.Create(context.Background(), agent("eng"), work.NewItem{
				Project: "ENG", Type: work.TypeTask,
				Title: fmt.Sprintf("item %d", i),
			})
			if err != nil {
				errs <- err
				return
			}
			keys <- got.Item.Key
		}()
	}
	close(start)

	seen := map[string]bool{}
	for range writers {
		select {
		case err := <-errs:
			t.Fatalf("a concurrent create failed: %v", err)
		case key := <-keys:
			if seen[key] {
				t.Fatalf("%s was minted twice", key)
			}
			seen[key] = true
		case <-time.After(10 * time.Second):
			t.Fatal("a concurrent create never finished")
		}
	}
	if len(seen) != writers {
		t.Fatalf("minted %d distinct keys from %d writers", len(seen), writers)
	}
	// The numbers are 1..N with no gaps here, because nothing crashed. A
	// gap is acceptable by design (the counter advances before the item is
	// created); a COLLISION is not.
	for i := 1; i <= writers; i++ {
		if !seen[fmt.Sprintf("ENG-%d", i)] {
			t.Errorf("ENG-%d was never minted: %v", i, slices.Sorted(maps.Keys(seen)))
		}
	}
}

// AN UNASSIGNED ITEM LANDS IN TRIAGE, which is what makes it somebody's
// problem rather than nobody's: triage is where the unit lead is woken. An
// assigned one starts at todo, because the assignment IS the triage.
func TestTheDefaultStatusFollowsWhetherAnybodyOwnsIt(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)

	unassigned := file(t, s, human("jane"), work.NewItem{Title: "nobody owns this"})
	if unassigned.Item.Status != work.StatusTriage {
		t.Errorf("an unassigned item starts at %q, want triage", unassigned.Item.Status)
	}
	assigned := file(t, s, human("jane"), work.NewItem{Title: "eng owns this", Assignee: "eng"})
	if assigned.Item.Status != work.StatusTodo {
		t.Errorf("an assigned item starts at %q, want todo", assigned.Item.Status)
	}
	// A caller that names a status is opting out of the default.
	explicit := file(t, s, human("jane"), work.NewItem{Title: "backlog", Status: work.StatusBacklog})
	if explicit.Item.Status != work.StatusBacklog {
		t.Errorf("an explicit status was overridden: %q", explicit.Item.Status)
	}
}

// THE REPORTER AND THE ASSIGNEE WATCH, which is Jira's participants rule and
// what makes a thread a conversation rather than a broadcast.
func TestFilingAndAssigningSubscribeTheParticipants(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	got := file(t, s, human("jane"), work.NewItem{Assignee: "eng"})
	for _, want := range []string{"jane", "eng"} {
		if !slices.Contains(got.Item.Watchers, want) {
			t.Errorf("%q does not watch the item they are on: %v", want, got.Item.Watchers)
		}
	}

	handed, err := s.Update(t.Context(), human("jane"), got.Item.ID, 0,
		work.Edit{Assignee: ptr("ops")})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(handed.Item.Watchers, "ops") {
		t.Errorf("a new assignee does not watch: %v", handed.Item.Watchers)
	}
	if !slices.Contains(handed.Item.Watchers, "eng") {
		t.Errorf("the previous assignee stopped watching on a hand-off: %v — "+
			"somebody who was on the work hears how it went", handed.Item.Watchers)
	}
}

// AN UNWATCH STICKS AGAINST EVERY AUTOMATIC RE-ADD. Without the mute, being
// named assignee re-subscribes somebody who deliberately opted out, and there
// is no gesture that means "stop".
func TestAnUnwatchSurvivesBeingReassigned(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	got := file(t, s, human("jane"), work.NewItem{Assignee: "eng"})

	muted, err := s.Update(t.Context(), human("eng"), got.Item.ID, 0, work.Edit{Watch: ptr(false)})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(muted.Item.Watchers, "eng") || !slices.Contains(muted.Item.Muted, "eng") {
		t.Fatalf("the unwatch did not take: watchers=%v muted=%v",
			muted.Item.Watchers, muted.Item.Muted)
	}

	off, err := s.Update(t.Context(), human("jane"), got.Item.ID, 0, work.Edit{Assignee: ptr("ops")})
	if err != nil {
		t.Fatal(err)
	}
	back, err := s.Update(t.Context(), human("jane"), off.Item.ID, 0, work.Edit{Assignee: ptr("eng")})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(back.Item.Watchers, "eng") {
		t.Errorf("being reassigned re-subscribed somebody who unwatched: %v", back.Item.Watchers)
	}

	// A DIRECTED MENTION STILL REACHES THEM. A mute says "stop telling me
	// about this item"; somebody typing your handle is telling YOU.
	_, _, err = s.Comment(t.Context(), human("jane"), back.Item.ID,
		work.NewComment{Body: "@eng can you look?", Mentions: []string{"eng"}})
	if err != nil {
		t.Fatal(err)
	}
	after, _, err := s.Item(t.Context(), back.Item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(after.Watchers, "eng") {
		t.Errorf("a mention did not reach a muted person: %v", after.Watchers)
	}
	if slices.Contains(after.Muted, "eng") {
		t.Errorf("the mute survived a mention, so every later reply is swallowed too: %v",
			after.Muted)
	}
}

// HAND-OFFS ARE BOUNDED ON THE ITEM, not by a depth counter. An agent
// assignment is an ownership transfer down a chart of known height, so
// charging it against the delegation cap would conflate two things — and the
// failure it needs bounding for is ping-pong, which lives on the item.
func TestAgentHandOffsAreBoundedAndAHumanTouchResetsThem(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	got := file(t, s, human("jane"), work.NewItem{Assignee: "a"})

	id := got.Item.ID
	for i := range work.ReassignmentBudget {
		to := fmt.Sprintf("seat%d", i)
		if _, err := s.Update(t.Context(), agent("a"), id, 0, work.Edit{Assignee: ptr(to)}); err != nil {
			t.Fatalf("hand-off %d was refused early: %v", i+1, err)
		}
	}
	_, err := s.Update(t.Context(), agent("a"), id, 0, work.Edit{Assignee: ptr("one-too-many")})
	if !errors.Is(err, work.ErrReassignmentBudget) {
		t.Fatalf("the %dth hand-off gave %v, want the budget refusal",
			work.ReassignmentBudget+1, err)
	}
	if !strings.Contains(err.Error(), got.Item.Key) {
		t.Errorf("the refusal must name the item: %v", err)
	}

	// A HUMAN WRITE RESETS IT — any human write, not just a reassignment,
	// so unblocking an item hands the agents a fresh budget without a
	// second gesture.
	if _, err := s.Update(t.Context(), human("jane"), id, 0,
		work.Edit{Priority: ptr(work.PriorityHigh)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(t.Context(), agent("a"), id, 0, work.Edit{Assignee: ptr("fresh")}); err != nil {
		t.Errorf("a human touch did not reset the budget: %v", err)
	}

	// AN OPERATOR COUNTS AS A PERSON. Somebody acting through the
	// dashboard with a token bound to no seat is still a person looking at
	// the item, which is the event the budget waits for.
	for range work.ReassignmentBudget {
		if _, err := s.Update(t.Context(), agent("a"), id, 0,
			work.Edit{Assignee: ptr("x" + fmt.Sprint(time.Now().UnixNano()))}); err != nil {
			break
		}
	}
	if _, err := s.Update(t.Context(), operator("ops"), id, 0,
		work.Edit{Priority: ptr(work.PriorityLow)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(t.Context(), agent("a"), id, 0, work.Edit{Assignee: ptr("after-operator")}); err != nil {
		t.Errorf("an operator's write did not reset the budget: %v", err)
	}
}

// IF-MATCH MEANS TWO DIFFERENT THINGS, and both are needed. A person editing
// a title against a description somebody else rewrote should be told; an
// agent naming two fields must not lose to a comment added while the model
// was thinking.
func TestIfMatchRefusesAndItsAbsenceMerges(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	got := file(t, s, human("jane"), work.NewItem{Title: "original"})

	stale := got.Revision
	if _, err := s.Update(t.Context(), human("jane"), got.Item.ID, 0,
		work.Edit{Body: ptr("somebody else got here first")}); err != nil {
		t.Fatal(err)
	}

	_, err := s.Update(t.Context(), human("jane"), got.Item.ID, stale,
		work.Edit{Title: ptr("edited from a stale read")})
	if !errors.Is(err, work.ErrStaleVersion) {
		t.Fatalf("a conditioned write on a moved head gave %v, want ErrStaleVersion", err)
	}

	// Without a version, the same edit merges onto the freshest head.
	merged, err := s.Update(t.Context(), human("jane"), got.Item.ID, 0,
		work.Edit{Title: ptr("edited without a condition")})
	if err != nil {
		t.Fatalf("an unconditioned write was refused: %v", err)
	}
	if merged.Item.Title != "edited without a condition" {
		t.Errorf("title = %q", merged.Item.Title)
	}
	if merged.Item.Body != "somebody else got here first" {
		t.Errorf("the merge lost the other write: %q", merged.Item.Body)
	}
}

// A COMMENT FROM A TURN IS IDEMPOTENT. A re-run turn — which the engine's own
// redelivery guarantees make ordinary — must not post its remark twice.
func TestATurnsCommentIsPostedOnceHoweverOftenTheTurnRuns(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	got := file(t, s, human("jane"), work.NewItem{Assignee: "eng"})

	post := func() work.Comment {
		c, _, err := s.Comment(t.Context(), agent("eng"), got.Item.ID, work.NewComment{
			Body: "picked this up", TurnKey: "turn-1",
		})
		if err != nil {
			t.Fatalf("comment: %v", err)
		}
		return c
	}
	first, again := post(), post()
	if first.ID != again.ID {
		t.Errorf("a re-run turn posted a second comment: %s and %s", first.ID, again.ID)
	}
	thread, err := s.Thread(t.Context(), got.Item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(thread) != 1 {
		t.Errorf("the thread has %d comments after one turn ran twice", len(thread))
	}

	// A DIFFERENT REMARK FROM THE SAME TURN IS A SECOND COMMENT: the body
	// is in the derived id precisely so a re-run that says something new
	// is not swallowed.
	if _, _, err := s.Comment(t.Context(), agent("eng"), got.Item.ID, work.NewComment{
		Body: "and here is what I found", TurnKey: "turn-1",
	}); err != nil {
		t.Fatal(err)
	}
	if thread, _ = s.Thread(t.Context(), got.Item.ID); len(thread) != 2 {
		t.Errorf("a different remark from the same turn was swallowed: %d comments", len(thread))
	}

	// A PERSON TYPING THE SAME SENTENCE TWICE MEANT TO SAY IT TWICE, so a
	// comment with no turn key is never collapsed.
	for range 2 {
		if _, _, err := s.Comment(t.Context(), human("jane"), got.Item.ID,
			work.NewComment{Body: "ping"}); err != nil {
			t.Fatal(err)
		}
	}
	if thread, _ = s.Thread(t.Context(), got.Item.ID); len(thread) != 4 {
		t.Errorf("a person's repeated comment was collapsed: %d comments", len(thread))
	}
}

// ONLY THE AUTHOR MAY EDIT A COMMENT. Anyone else rewriting it would put a
// sentence under a name that never said it.
func TestOnlyTheAuthorEditsTheirOwnComment(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	got := file(t, s, human("jane"), work.NewItem{})
	comment, _, err := s.Comment(t.Context(), agent("eng"), got.Item.ID,
		work.NewComment{Body: "first draft"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EditComment(t.Context(), human("jane"), got.Item.ID, comment.ID, "rewritten"); err == nil {
		t.Fatal("somebody else rewrote a comment")
	}
	edited, err := s.EditComment(t.Context(), agent("eng"), got.Item.ID, comment.ID, "second draft")
	if err != nil {
		t.Fatalf("the author could not edit their own comment: %v", err)
	}
	if edited.Body != "second draft" {
		t.Errorf("body = %q", edited.Body)
	}
}

// CLOSING AS A DUPLICATE MUST NAME THE SURVIVOR, or the close is a dead end:
// a person following the link finds an item that says it duplicates nothing.
func TestClosingAsADuplicateNamesTheSurvivor(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	got := file(t, s, human("jane"), work.NewItem{})
	other := file(t, s, human("jane"), work.NewItem{})

	_, err := s.Update(t.Context(), human("jane"), got.Item.ID, 0, work.Edit{
		Status: ptr(work.StatusDone), CloseReason: ptr(work.CloseDuplicate),
	})
	if !errors.Is(err, work.ErrInvalid) || !strings.Contains(err.Error(), "duplicate_of") {
		t.Fatalf("closing as a duplicate with no target gave %v", err)
	}

	closed, err := s.Update(t.Context(), human("jane"), got.Item.ID, 0, work.Edit{
		Status: ptr(work.StatusDone), CloseReason: ptr(work.CloseDuplicate),
		DuplicateOf: ptr(other.Item.ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if closed.Item.DuplicateOf != other.Item.ID || closed.Item.ClosedAt == nil {
		t.Errorf("the close did not record itself: %+v", closed.Item)
	}

	// REOPENING CLEARS THE CLOSURE, or a report counts a reopened item as
	// delivered.
	reopened, err := s.Update(t.Context(), human("jane"), got.Item.ID, 0,
		work.Edit{Status: ptr(work.StatusInProgress)})
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Item.CloseReason != "" || reopened.Item.DuplicateOf != "" || reopened.Item.ClosedAt != nil {
		t.Errorf("a reopened item still claims it was closed: %+v", reopened.Item)
	}
}

// EVERY WRITE LEAVES A CHANGE RECORD, and the record carries the routing
// snapshot — which is what lets the node that wins a feed message route
// without reading anything.
func TestEveryWriteLeavesAChangeCarryingItsOwnRouting(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	got := file(t, s, human("jane"), work.NewItem{Assignee: "eng", Title: "the work"})
	if _, err := s.Update(t.Context(), human("jane"), got.Item.ID, 0,
		work.Edit{Status: ptr(work.StatusInProgress)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Comment(t.Context(), agent("eng"), got.Item.ID,
		work.NewComment{Body: "on it"}); err != nil {
		t.Fatal(err)
	}

	history, err := s.History(t.Context(), got.Item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 {
		t.Fatalf("history = %d entries, want created + status + comment", len(history))
	}
	if got, want := []work.ChangeKind{history[0].Kind, history[1].Kind, history[2].Kind},
		[]work.ChangeKind{work.ChangeCreated, work.ChangeStatus, work.ChangeComment}; !slices.Equal(got, want) {
		t.Errorf("kinds = %v, want %v", got, want)
	}
	for i, change := range history {
		if change.Snapshot.Key != got.Item.Key {
			t.Errorf("change %d carries no key: %+v", i, change.Snapshot)
		}
		if change.HeadRevision == 0 {
			t.Errorf("change %d names no head revision, so a reader cannot tell "+
				"whether the head it holds includes it", i)
		}
	}
	// The snapshot's watchers are already MINUS the muted, so the feed
	// never has to subtract and can never forget to.
	if _, err := s.Update(t.Context(), human("eng"), got.Item.ID, 0, work.Edit{Watch: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	history, _ = s.History(t.Context(), got.Item.ID)
	last := history[len(history)-1]
	if slices.Contains(last.Snapshot.Watchers, "eng") {
		t.Errorf("a muted watcher is in the routing snapshot: %v", last.Snapshot.Watchers)
	}
}

// AN ITEM IS PURGED, NEVER DELETED, because these buckets are ageless and a
// delete's tombstone would outlive the deployment — a listing returning
// tombstones is a board with ghosts on it. The CHANGE KEYS STAY, so a
// redelivered feed message about a removed item is still deduplicated.
func TestRemovingAnItemPurgesItAndKeepsItsRecord(t *testing.T) {
	t.Parallel()
	s, docs := newStore(t)
	got := file(t, s, human("jane"), work.NewItem{})
	if _, _, err := s.Comment(t.Context(), human("jane"), got.Item.ID,
		work.NewComment{Body: "a remark"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(t.Context(), human("jane"), got.Item.ID); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.Item(t.Context(), got.Item.ID); !errors.Is(err, work.ErrNotFound) {
		t.Errorf("the item survived removal: %v", err)
	}
	thread, err := s.Thread(t.Context(), got.Item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(thread) != 0 {
		t.Errorf("the thread survived: %d comments", len(thread))
	}
	history, err := s.History(t.Context(), got.Item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) == 0 || history[len(history)-1].Kind != work.ChangeRemoved {
		t.Errorf("the removal left no record: %v", history)
	}

	// PURGED, not deleted: a listing over the family returns no tombstone
	// for the item's key.
	records, err := docs.Documents(t.Context(), coord.FamilyWork, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range records {
		if rec.Key == work.ItemKey(got.Item.ID) {
			t.Errorf("the item's key is still listed after removal")
		}
	}
}

// EVERY REFUSAL NAMES THE FIELD AND WHY. A cap that cut instead would leave
// an agent acting on half a specification.
func TestOversizedContentIsRefusedNamingTheField(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	for _, tc := range []struct {
		name  string
		in    work.NewItem
		field string
	}{
		{"a title", work.NewItem{Title: strings.Repeat("x", work.MaxTitle+1)}, "title"},
		{"a body", work.NewItem{Body: strings.Repeat("x", work.MaxBody+1)}, "body"},
		{"no title", work.NewItem{Title: "   "}, "title"},
		{"a project key", work.NewItem{Project: "eng"}, "project"},
		{"a type", work.NewItem{Type: "epic-ish"}, "type"},
		{"a status", work.NewItem{Status: "wip"}, "status"},
		{"too many labels", work.NewItem{Labels: manyLabels(work.MaxLabels + 1)}, "labels"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := tc.in
			if in.Project == "" {
				in.Project = "ENG"
			}
			if in.Type == "" {
				in.Type = work.TypeTask
			}
			if in.Title == "" {
				in.Title = "a title"
			}
			_, err := s.Create(t.Context(), human("jane"), in)
			if !errors.Is(err, work.ErrInvalid) {
				t.Fatalf("got %v, want ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("the refusal does not name %q: %v", tc.field, err)
			}
		})
	}

	got := file(t, s, human("jane"), work.NewItem{})
	_, _, err := s.Comment(t.Context(), human("jane"), got.Item.ID,
		work.NewComment{Body: strings.Repeat("x", work.MaxComment+1)})
	if !errors.Is(err, work.ErrInvalid) || !strings.Contains(err.Error(), "body") {
		t.Errorf("an oversized comment gave %v", err)
	}
}

func manyLabels(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("label-%d", i)
	}
	return out
}

// IDENTITY IS NEVER A MODEL ARGUMENT, so a write with no honest actor is
// refused rather than attributed to a guess.
func TestAWriteNeedsAnActorItCanAttributeTo(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	for _, actor := range []work.Actor{
		{},
		{Kind: "robot", Handle: "eng"},
		{Kind: work.AuthorAgent},
		{Kind: work.AuthorHuman},
	} {
		if _, err := s.Create(t.Context(), actor, work.NewItem{
			Project: "ENG", Type: work.TypeTask, Title: "t",
		}); !errors.Is(err, work.ErrInvalid) {
			t.Errorf("actor %+v was accepted: %v", actor, err)
		}
	}
	// An operator token bound to no seat IS a valid actor, and is recorded
	// under the label its operator chose rather than as "the engine".
	got, err := s.Create(t.Context(), operator("ci-pipeline"), work.NewItem{
		Project: "ENG", Type: work.TypeTask, Title: "filed by a pipeline",
	})
	if err != nil {
		t.Fatalf("an unbound operator token was refused: %v", err)
	}
	if got.Item.LastChange.Actor != "operator:ci-pipeline" {
		t.Errorf("actor = %q, want the token's own label", got.Item.LastChange.Actor)
	}
}

// AN ITEM KEY IS THE CANONICAL STRING people paste and tools resolve, so a
// lenient reader that accepted near-misses on one path and not another would
// be worse than one that accepts only the exact form.
func TestItemKeysAreExact(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"ENG-1", "ENG-4211", "PROD2-9"} {
		if !work.ValidKey(key) {
			t.Errorf("%q was refused", key)
		}
	}
	for _, key := range []string{"", "ENG", "ENG-", "-1", "ENG-0", "ENG-01", "ENG-1a", "eng 1"} {
		if work.ValidKey(key) {
			t.Errorf("%q was accepted", key)
		}
	}
	project, number, ok := work.SplitKey("ENG-42")
	if !ok || project != "ENG" || number != 42 {
		t.Errorf("SplitKey = %q %d %v", project, number, ok)
	}
	if got := work.ConversationKey("ENG-42"); got != "work:ENG-42" {
		t.Errorf("ConversationKey = %q", got)
	}
}

// The type hierarchy is STRICTLY DOWNWARD, so "the epic this belongs to" is a
// walk that always ends.
func TestTheTypeHierarchyIsATree(t *testing.T) {
	t.Parallel()
	if !work.TypeEpic.CanParent(work.TypeStory) || !work.TypeTask.CanParent(work.TypeSubtask) {
		t.Error("a legal nesting was refused")
	}
	for _, tc := range [][2]work.Type{
		{work.TypeStory, work.TypeEpic},
		{work.TypeTask, work.TypeTask},
		{work.TypeSubtask, work.TypeSubtask},
		{work.TypeEpic, work.TypeEpic},
	} {
		if tc[0].CanParent(tc[1]) {
			t.Errorf("%s may parent %s, which allows a cycle", tc[0], tc[1])
		}
	}
}
