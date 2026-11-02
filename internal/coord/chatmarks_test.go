package coord_test

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/coord"
)

var markAt = time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)

// merge applies one flush and fails the test on a refusal.
func merge(t *testing.T, state coord.ChatReadState, delta coord.ChatReadDelta) coord.ChatReadState {
	t.Helper()
	out, err := coord.MergeChatRead(state, delta)
	if err != nil {
		t.Fatalf("MergeChatRead(%+v): %v", delta, err)
	}
	return out
}

// A ROOM REACHES ITS READER AT ONE LEVEL. The muted set and the every-message
// set are two lists of one fact, so a level moves a room between them and a
// mute takes it out of every message: a room in both would be muted and loud
// at once, and which the screens drew would depend on which they read first.
//
// Mutation: drop a mute's lift of every message and the first case keeps the
// room in both sets.
func TestARoomReachesItsReaderAtOneLevel(t *testing.T) {
	t.Parallel()
	state := merge(t, coord.ChatReadState{}, coord.ChatReadDelta{
		Notify: map[string]coord.NotifyLevel{"eng": coord.NotifyAll, "ops": coord.NotifyAll},
	})
	if !slices.Equal(state.Every, []string{"eng", "ops"}) || len(state.Muted) != 0 {
		t.Fatalf("two rooms set to every message left every %v, muted %v", state.Every, state.Muted)
	}
	state = merge(t, state, coord.ChatReadDelta{Mute: []string{"eng"}})
	if !slices.Equal(state.Every, []string{"ops"}) || !slices.Equal(state.Muted, []string{"eng"}) {
		t.Errorf("muting a room at every message left every %v, muted %v, want it "+
			"muted alone", state.Every, state.Muted)
	}
	state = merge(t, state, coord.ChatReadDelta{
		Notify: map[string]coord.NotifyLevel{"eng": coord.NotifyAll, "ops": coord.NotifyMentions},
	})
	if !slices.Equal(state.Every, []string{"eng"}) || len(state.Muted) != 0 {
		t.Errorf("eng back to every message and ops to mentions left every %v, muted %v",
			state.Every, state.Muted)
	}
	state = merge(t, state, coord.ChatReadDelta{
		Notify: map[string]coord.NotifyLevel{"eng": coord.NotifyMuted},
	})
	if len(state.Every) != 0 || !slices.Equal(state.Muted, []string{"eng"}) {
		t.Errorf("a room set to muted left every %v, muted %v", state.Every, state.Muted)
	}

	for name, delta := range map[string]coord.ChatReadDelta{
		"a level and a mute of one room": {Mute: []string{"eng"},
			Notify: map[string]coord.NotifyLevel{"eng": coord.NotifyAll}},
		"a level this build does not know": {
			Notify: map[string]coord.NotifyLevel{"eng": "loud"}},
		"a level for no room": {Notify: map[string]coord.NotifyLevel{"": coord.NotifyAll}},
	} {
		if _, err := coord.MergeChatRead(state, delta); !errors.Is(err, coord.ErrInvalidReadState) {
			t.Errorf("%s answered %v, want the sentinel", name, err)
		}
	}
}

// A SAVE IS KEPT AS IT WAS FIRST MADE, so a flush sent again after its answer
// was lost leaves the record exactly as the first did. A save that lifted the
// message to the top again would make the store's own retry visible as a
// reordered list.
//
// Mutation: overwrite an existing save's instant and the retry moves it.
func TestASaveSentTwiceIsOneSave(t *testing.T) {
	t.Parallel()
	first := coord.MessageRef{ChannelID: "eng", MessageID: "m-1"}
	second := coord.MessageRef{ChannelID: "ops", MessageID: "m-2"}
	state := merge(t, coord.ChatReadState{}, coord.ChatReadDelta{Save: []coord.MessageRef{first}, At: markAt})
	state = merge(t, state, coord.ChatReadDelta{Save: []coord.MessageRef{second}, At: markAt.Add(time.Minute)})
	again := merge(t, state, coord.ChatReadDelta{Save: []coord.MessageRef{first}, At: markAt.Add(time.Hour)})
	if len(again.Saved) != 2 || again.Saved[0].MessageID != "m-2" || !again.Saved[1].At.Equal(markAt) {
		t.Fatalf("saving m-1 again reordered or restamped the list: %+v", again.Saved)
	}
	gone := merge(t, again, coord.ChatReadDelta{Unsave: []coord.MessageRef{{MessageID: "m-2"}}})
	if len(gone.Saved) != 1 || gone.Saved[0].MessageID != "m-1" {
		t.Errorf("unsaving m-2 left %+v", gone.Saved)
	}
	if _, err := coord.MergeChatRead(gone, coord.ChatReadDelta{
		Save: []coord.MessageRef{second}, Unsave: []coord.MessageRef{second},
	}); !errors.Is(err, coord.ErrInvalidReadState) {
		t.Errorf("a message both saved and unsaved in one flush answered %v", err)
	}
}

// A STAR AND A SAVE PAST THE CAP ARE REFUSED, NEVER TRADED FOR AN OLDER ONE,
// while a flush that does not grow the set still lands on a full record: a
// silent drop is a saved message gone with no way to find out, and a refusal
// of everything else would freeze the record.
//
// Mutation: drop either cap check and the set grows past the bound.
func TestAStarOrASavePastTheCapIsRefused(t *testing.T) {
	t.Parallel()
	full := coord.ChatReadState{}
	for i := range coord.MaxReaderMarks {
		id := fmt.Sprintf("x-%03d", i)
		full.Starred = append(full.Starred, id)
		full.Saved = append(full.Saved, coord.SavedMessage{ChannelID: "eng", MessageID: id, At: markAt})
	}
	for name, delta := range map[string]coord.ChatReadDelta{
		"a star":  {Star: []string{"one-more"}},
		"a save":  {Save: []coord.MessageRef{{ChannelID: "eng", MessageID: "one-more"}}, At: markAt},
		"a level": {Notify: map[string]coord.NotifyLevel{"one-more": coord.NotifyAll}},
	} {
		state := full
		if name == "a level" {
			state = coord.ChatReadState{Every: full.Starred}
		}
		_, err := coord.MergeChatRead(state, delta)
		if !errors.Is(err, coord.ErrInvalidReadState) ||
			!strings.Contains(err.Error(), strconv.Itoa(coord.MaxReaderMarks)) {
			t.Errorf("%s past the cap answered %v, want the sentinel naming the cap", name, err)
		}
	}
	for name, delta := range map[string]coord.ChatReadDelta{
		"an unstar":         {Unstar: []string{"x-000"}},
		"an unsave":         {Unsave: []coord.MessageRef{{MessageID: "x-000"}}},
		"a star held":       {Star: []string{"x-001"}},
		"a cursor":          {Cursors: map[string]int64{"eng": 9}},
		"a thread followed": {Follow: []coord.ThreadRef{{ChannelID: "eng", RootID: "r"}}, At: markAt},
	} {
		if _, err := coord.MergeChatRead(full, delta); err != nil {
			t.Errorf("%s on a full record was refused: %v", name, err)
		}
	}
}

// A THREAD MARK PAST THE CAP DROPS THE OLDEST, because every unfollow leaves
// one and a cap that refused would keep somebody in a thread they asked to
// leave. A mark that already says what the flush says keeps its instant, so a
// retried flush moves nothing.
//
// Mutation: refuse past the cap and the last unfollow below is refused.
func TestAThreadMarkPastTheCapDropsTheOldest(t *testing.T) {
	t.Parallel()
	state := coord.ChatReadState{}
	for i := range coord.MaxReaderMarks {
		state = merge(t, state, coord.ChatReadDelta{
			Unfollow: []coord.ThreadRef{{ChannelID: "eng", RootID: fmt.Sprintf("r-%03d", i)}},
			At:       markAt.Add(time.Duration(i) * time.Second),
		})
	}
	state = merge(t, state, coord.ChatReadDelta{
		Unfollow: []coord.ThreadRef{{ChannelID: "eng", RootID: "newest"}},
		At:       markAt.Add(time.Hour),
	})
	if len(state.Threads) != coord.MaxReaderMarks {
		t.Fatalf("the marks hold %d, want the cap %d", len(state.Threads), coord.MaxReaderMarks)
	}
	if state.Threads[0].RootID != "newest" || slices.ContainsFunc(state.Threads,
		func(m coord.ThreadMark) bool { return m.RootID == "r-000" }) {
		t.Errorf("past the cap the marks kept the oldest or lost the newest: first %+v", state.Threads[0])
	}
	again := merge(t, state, coord.ChatReadDelta{
		Unfollow: []coord.ThreadRef{{ChannelID: "eng", RootID: "newest"}},
		At:       markAt.Add(2 * time.Hour),
	})
	if !again.Threads[0].At.Equal(markAt.Add(time.Hour)) {
		t.Errorf("an unfollow sent again restamped the mark to %v", again.Threads[0].At)
	}
	flipped := merge(t, again, coord.ChatReadDelta{
		Follow: []coord.ThreadRef{{ChannelID: "eng", RootID: "newest"}},
		At:     markAt.Add(3 * time.Hour),
	})
	if !flipped.Threads[0].Following {
		t.Errorf("a follow of an unfollowed thread left %+v", flipped.Threads[0])
	}
	if _, err := coord.MergeChatRead(flipped, coord.ChatReadDelta{
		Follow:   []coord.ThreadRef{{ChannelID: "eng", RootID: "x"}},
		Unfollow: []coord.ThreadRef{{ChannelID: "eng", RootID: "x"}},
	}); !errors.Is(err, coord.ErrInvalidReadState) {
		t.Errorf("a thread both followed and unfollowed in one flush answered %v", err)
	}
}

// A REWIND IS THE ONE WAY A CURSOR MOVES BACK, and it moves exactly where it
// is told: "mark unread" sets the room's place before a message, which the
// forward merge would ignore. Zero forgets the place. Naming a room both to
// advance and to rewind is two facts about one place and is refused. Thread
// cursors take the forward rule of room cursors.
//
// Mutation: apply a rewind through the forward merge and the first case keeps
// the cursor at 500.
func TestARewindSetsTheCursorExactly(t *testing.T) {
	t.Parallel()
	state := merge(t, coord.ChatReadState{}, coord.ChatReadDelta{
		Cursors:       map[string]int64{"eng": 500, "ops": 40},
		ThreadCursors: map[string]int64{"root-1": 300},
	})
	state = merge(t, state, coord.ChatReadDelta{
		Rewind:        map[string]int64{"eng": 120, "ops": 0},
		ThreadCursors: map[string]int64{"root-1": 100},
	})
	if got := state.Cursors["eng"]; got != 120 {
		t.Errorf("a rewind to 120 left the cursor at %d", got)
	}
	if _, kept := state.Cursors["ops"]; kept {
		t.Errorf("a rewind to zero kept the cursor: %v", state.Cursors)
	}
	if got := state.ThreadCursors["root-1"]; got != 300 {
		t.Errorf("a thread cursor moved back to %d", got)
	}
	if _, err := coord.MergeChatRead(state, coord.ChatReadDelta{
		Cursors: map[string]int64{"eng": 700}, Rewind: map[string]int64{"eng": 10},
	}); !errors.Is(err, coord.ErrInvalidReadState) {
		t.Errorf("a room advanced and rewound in one flush answered %v", err)
	}
	if _, err := coord.MergeChatRead(state, coord.ChatReadDelta{
		Rewind: map[string]int64{"eng": -1},
	}); !errors.Is(err, coord.ErrInvalidReadState) {
		t.Errorf("a rewind below zero answered %v", err)
	}
}

// THE DASHBOARD'S CAP ON THE READER'S MARKS IS THE ENGINE'S. The Rooms panel
// and the message toolbar say a star or a save is past the cap before the
// press; a copy too low greys out a gesture the engine would take, and one too
// high moves the refusal from the control to the write.
func TestTheDashboardsReaderMarkCapIsTheEngines(t *testing.T) {
	t.Parallel()
	raw, err := clientsource.Scalar(clientsource.Tree(t), "MAX_READER_MARKS")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.ReplaceAll(raw, "_", ""), strconv.Itoa(coord.MaxReaderMarks); got != want {
		t.Errorf("contract/chat.ts MAX_READER_MARKS is %s, want the engine's %s", raw, want)
	}
}
