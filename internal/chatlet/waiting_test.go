package chatlet_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
)

// What waits on a reader is ONE count that every badge takes: the rail's
// places, which the sidebar, the room rows and the thread lines draw, and
// Activity's unread rows and counts.

// waitPost is one message, a reply when root is set.
func waitPost(channelID, root, messageID, author, body string, collective bool,
	mentions ...string) chatlet.MutationRecord {

	return readRecord(chatlet.MessageSubject(channelID), chatlet.OpPost,
		"op-post-"+messageID, chatlet.MessagePost{
			V: chatlet.DocumentVersion, MessageID: messageID, Body: body,
			ThreadRoot: root, Author: author, AuthorKind: chatlet.AuthorHuman,
			Mentions: mentions, Collective: collective,
		}, chatlet.ScopeSet{Subject: true}, author)
}

// waitDirect opens the direct conversation between the handles.
func waitDirect(handles ...string) (string, chatlet.MutationRecord) {
	id := chatlet.DirectChannelID(handles).String()
	members := make([]chatlet.Member, 0, len(handles))
	for _, h := range handles {
		members = append(members, chatlet.Member{Handle: h})
	}
	return id, readRecord(chatlet.ChannelSubject(id), chatlet.OpCreate, "op-open-"+id,
		chatlet.ChannelCreate{V: chatlet.DocumentVersion, ChannelID: id,
			Kind: chatlet.KindDM, Members: members, CreatedBy: handles[0],
			CreatedByKind: chatlet.AuthorHuman},
		chatlet.ScopeSet{Subject: true}, handles[0])
}

// waitingWorld is every case from bob's side, jane writing: a direct
// message; mentions of him and of the room; replies in threads he started,
// replied in and followed by hand; a reply naming him and one naming the room
// in his thread; and what never waits: a muted room, an unfollowed thread, a
// thread he never joined, his own words.
func waitingWorld(t *testing.T) (*readHarness, string, chatlet.ReaderMarks) {
	t.Helper()
	h := newReadHarness(t)
	dm, open := waitDirect("bob", "jane")
	h.apply(open)
	h.apply(readCreateRoom("ops", "ops", chatlet.KindPublic, "jane", "bob"))
	h.apply(readCreateRoom("quiet", "quiet", chatlet.KindPublic, "jane", "bob"))

	h.apply(waitPost(dm, "", "dm-1", "jane", "can you check the deploy?", false))
	h.apply(waitPost("ops", "", "named", "jane", "@bob please look", false, "bob"))
	h.apply(waitPost("ops", "", "both", "jane", "@channel @bob standup", true, "bob"))
	h.apply(waitPost("ops", "", "plain", "jane", "release is on track", false))
	h.apply(waitPost("ops", "", "self", "bob", "@bob note to self", false, "bob"))

	h.apply(waitPost("ops", "", "started", "bob", "who owns the cache?", false))
	h.apply(waitPost("ops", "started", "s-1", "jane", "I do", false))
	h.apply(waitPost("ops", "started", "s-2", "jane", "@bob can you review?", false, "bob"))
	h.apply(waitPost("ops", "started", "s-3", "jane", "@channel heads up", true))

	h.apply(waitPost("ops", "", "notes", "jane", "notes are up", false))
	h.apply(waitPost("ops", "notes", "n-1", "bob", "looks good", false))
	h.apply(waitPost("ops", "notes", "n-2", "jane", "added the migration", false))

	h.apply(waitPost("ops", "", "window", "jane", "maintenance on Friday", false))
	h.apply(waitPost("ops", "window", "w-1", "jane", "starts at 22:00", false))

	h.apply(waitPost("ops", "", "lunch", "jane", "lunch?", false))
	h.apply(waitPost("ops", "lunch", "l-1", "bob", "yes", false))
	h.apply(waitPost("ops", "lunch", "l-2", "jane", "me too", false))

	h.apply(waitPost("ops", "", "random", "jane", "a thought", false))
	h.apply(waitPost("ops", "random", "r-1", "jane", "and another", false))

	h.apply(waitPost("quiet", "", "q-root", "bob", "cost report", false))
	h.apply(waitPost("quiet", "q-root", "q-1", "jane", "filed", false))
	h.apply(waitPost("quiet", "", "q-named", "jane", "@bob one more", false, "bob"))

	marks := chatlet.ReaderMarks{Known: true,
		Following:  []chatlet.ThreadRef{{ChannelID: "ops", RootID: "window"}},
		Unfollowed: []chatlet.ThreadRef{{ChannelID: "ops", RootID: "lunch"}},
		Muted:      map[string]bool{"quiet": true},
	}
	return h, dm, marks
}

// rail is bob's rail, asked for what waits on him.
func (h *readHarness) waitingRail(marks chatlet.ReaderMarks) chatlet.ChannelListing {
	h.t.Helper()
	listing, err := h.reader().Channels(h.t.Context(), "bob",
		chatlet.ChannelsQuery{Marks: marks, Waiting: true}, h.session())
	if err != nil {
		h.t.Fatalf("bob's rail: %v", err)
	}
	return listing
}

// activity is bob's Activity.
func (h *readHarness) waitingActivity(marks chatlet.ReaderMarks) chatlet.Activity {
	h.t.Helper()
	feed, err := h.reader().Activity(h.t.Context(), "bob",
		chatlet.ActivityQuery{Marks: marks, Limit: chatlet.MaxLimit}, h.session())
	if err != nil {
		h.t.Fatalf("bob's Activity: %v", err)
	}
	return feed
}

// places writes a rail's waiting list as "room/thread=count".
func places(waiting []chatlet.Waiting) []string {
	out := make([]string, 0, len(waiting))
	for _, w := range waiting {
		out = append(out, fmt.Sprintf("%s/%s=%d", w.ChannelID, w.RootID, w.Count))
	}
	return out
}

// EVERY CASE WAITS WHERE IT WAITS, EACH MESSAGE ONCE, AND THE RAIL AGREES WITH
// ACTIVITY: the rail's places sum to Activity's unread mentions and replies
// plus the direct conversation's unread posts, which Activity does not list.
//
// Mutation: drop the collective exclusion from the reply sources and his
// thread counts four; drop the author filter from the mention source and the
// room's posts count three; drop the mute from the marks and the quiet room
// waits.
func TestEveryBadgeCountsWhatWaitsOnTheReaderOnce(t *testing.T) {
	t.Parallel()
	h, dm, marks := waitingWorld(t)
	rail := h.waitingRail(marks)
	want := []string{dm + "/=1", "ops/=2", "ops/notes=1", "ops/started=3", "ops/window=1"}
	slices.Sort(want)
	if got := places(rail.Waiting); !slices.Equal(got, want) || rail.WaitingCapped {
		t.Fatalf("bob's waiting places are %v (capped %t), want %v", got,
			rail.WaitingCapped, want)
	}
	feed := h.waitingActivity(marks)
	if feed.Unread.Mention != 4 || feed.Unread.Reply != 3 {
		t.Errorf("Activity counts %+v, want 4 mentions (named, both, the reply naming "+
			"him, the reply naming the room) and 3 replies", feed.Unread)
	}
	unread := 0
	for _, item := range feed.Items {
		if item.Unread {
			unread++
		}
		if item.ChannelID == "quiet" && item.Unread {
			t.Errorf("%s in the muted room is unread in Activity", item.MessageID)
		}
	}
	sum := 0
	for _, w := range rail.Waiting {
		sum += w.Count
	}
	if sum != unread+1 || unread != feed.Unread.Mention+feed.Unread.Reply {
		t.Errorf("the rail waits %d, Activity marks %d rows unread and counts %d: "+
			"want the rail to be Activity's rows plus the one direct message",
			sum, unread, feed.Unread.Mention+feed.Unread.Reply)
	}
}

// READING A THREAD CLEARS IT EVERYWHERE AND NOTHING ELSE: its own place, moved
// past its newest reply from any tab, takes it off the rail and out of
// Activity's counts, and every other room and thread still waits.
func TestReadingAThreadClearsItsCountEverywhere(t *testing.T) {
	t.Parallel()
	h, dm, marks := waitingWorld(t)
	var through int64
	for _, item := range h.waitingActivity(marks).Items {
		if item.ThreadRoot == "started" {
			through = max(through, item.ReadThrough)
		}
	}
	marks.ThreadCursors = map[string]int64{"started": through}
	want := []string{dm + "/=1", "ops/=2", "ops/notes=1", "ops/window=1"}
	slices.Sort(want)
	if got := places(h.waitingRail(marks).Waiting); !slices.Equal(got, want) {
		t.Errorf("after reading his thread bob waits on %v, want %v", got, want)
	}
	if got := h.waitingActivity(marks).Unread; got.Mention != 2 || got.Reply != 2 {
		t.Errorf("after reading his thread Activity counts %+v, want 2 and 2", got)
	}
}

// A RECORD NOBODY COULD READ COUNTS NOTHING, rather than everything: no place
// waits and Activity marks nothing unread.
func TestAnUnreadRecordWaitsOnNothing(t *testing.T) {
	t.Parallel()
	h, _, _ := waitingWorld(t)
	if got := h.waitingRail(chatlet.ReaderMarks{}); len(got.Waiting) != 0 || got.WaitingCapped {
		t.Errorf("over an unread record the rail waits %v, want nothing", places(got.Waiting))
	}
}

// A PROFILE'S RAIL COUNTS NOTHING: the rooms two seats share are not the
// reader's rail, and a count over them would be half of one.
func TestARailOfSharedRoomsCountsNothingWaiting(t *testing.T) {
	t.Parallel()
	h, _, marks := waitingWorld(t)
	listing, err := h.reader().Channels(t.Context(), "bob",
		chatlet.ChannelsQuery{Marks: marks, Waiting: true, With: "jane"}, h.session())
	if err != nil {
		t.Fatalf("bob's rooms shared with jane: %v", err)
	}
	if len(listing.Channels) == 0 || len(listing.Waiting) != 0 {
		t.Errorf("the shared rooms are %d with %v waiting, want rooms and nothing waiting",
			len(listing.Channels), places(listing.Waiting))
	}
}

// THE COUNT IS BOUNDED BY [chatlet.UnreadLimit] AND SAYS SO: a room whose newest
// hundred messages are all unread is a floor, for the room and the whole count,
// never a scan of the whole thread.
func TestWhatWaitsIsCountedToTheBoundAndSaysSo(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	h.apply(readCreateRoom("ops", "ops", chatlet.KindPublic, "jane", "bob"))
	h.apply(waitPost("ops", "", "root", "bob", "a busy thread", false))
	for i := range chatlet.UnreadLimit + 5 {
		h.apply(waitPost("ops", "root", fmt.Sprintf("r-%03d", i), "jane", "more", false))
	}
	rail := h.waitingRail(chatlet.ReaderMarks{Known: true})
	if got := places(rail.Waiting); !slices.Equal(got,
		[]string{fmt.Sprintf("ops/root=%d", chatlet.UnreadLimit)}) || !rail.WaitingCapped ||
		!rail.Waiting[0].Capped {
		t.Errorf("the busy thread waits %v (capped %t), want %d and a floor", got,
			rail.WaitingCapped, chatlet.UnreadLimit)
	}
}

// READ ROWS ELSEWHERE NEVER PUSH AN UNREAD ONE OUT: each room is counted over
// its own newest messages. Here bob read a busy thread in #busy to its end and
// a muted #ann filled with `@channel` posts, both newer than the one reply
// waiting in #ops; the reply still waits, and nothing is a floor.
//
// Mutation: count over the newest rows of each source across rooms, as Activity
// pages them, and the reply drops out with no floor.
func TestReadRowsElsewhereNeverHideWhatWaits(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	h.apply(readCreateRoom("ops", "ops", chatlet.KindPublic, "jane", "bob"))
	h.apply(readCreateRoom("busy", "busy", chatlet.KindPublic, "jane", "bob"))
	h.apply(readCreateRoom("ann", "ann", chatlet.KindPublic, "jane", "bob"))
	h.apply(waitPost("ops", "", "root", "bob", "who owns it?", false))
	h.apply(waitPost("ops", "root", "waits", "jane", "I do", false))
	h.apply(waitPost("busy", "", "busy-root", "bob", "a busy thread", false))
	var read int64
	for i := range chatlet.UnreadLimit + 5 {
		read = h.apply(waitPost("busy", "busy-root", fmt.Sprintf("b-%03d", i), "jane",
			"more", false)).Packed()
	}
	for i := range chatlet.UnreadLimit + 5 {
		h.apply(waitPost("ann", "", fmt.Sprintf("a-%03d", i), "jane", "@channel", true))
	}
	marks := chatlet.ReaderMarks{Known: true, Cursors: map[string]int64{"busy": read},
		Muted: map[string]bool{"ann": true}}
	rail := h.waitingRail(marks)
	if got := places(rail.Waiting); !slices.Equal(got, []string{"ops/root=1"}) ||
		rail.WaitingCapped {
		t.Errorf("bob waits on %v (capped %t), want the one reply in #ops, exact",
			got, rail.WaitingCapped)
	}
	if got := h.waitingActivity(marks).Unread; got.Reply != 1 || got.Mention != 0 || got.Capped {
		t.Errorf("Activity counts %+v, want the one reply", got)
	}
}

// A ROOM THE READER IS NOT IN STILL HOLDS WHAT WAITS ON THEM: a mention there,
// and a reply in a thread they follow, an `@channel` reply included, which
// names the room's members and not them. Activity lists both the same way.
func TestARoomTheReaderIsNotInStillHoldsWhatWaits(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	h.apply(readCreateRoom("pub", "pub", chatlet.KindPublic, "jane"))
	h.apply(waitPost("pub", "", "named", "jane", "@bob have a look", false, "bob"))
	h.apply(waitPost("pub", "", "root", "jane", "the plan", false))
	h.apply(waitPost("pub", "root", "r-1", "jane", "@channel it moved", true))
	marks := chatlet.ReaderMarks{Known: true,
		Following: []chatlet.ThreadRef{{ChannelID: "pub", RootID: "root"}}}
	if got := places(h.waitingRail(marks).Waiting); !slices.Equal(got,
		[]string{"pub/=1", "pub/root=1"}) {
		t.Errorf("bob waits on %v in a room he is not in, want the mention and the reply", got)
	}
	feed := h.waitingActivity(marks)
	if feed.Unread.Mention != 1 || feed.Unread.Reply != 1 {
		t.Errorf("Activity counts %+v, want one mention and one reply", feed.Unread)
	}
	kinds := map[string]chatlet.ActivityKind{}
	for _, item := range feed.Items {
		kinds[item.MessageID] = item.Kind
	}
	if kinds["named"] != chatlet.ActivityMention || kinds["r-1"] != chatlet.ActivityReply {
		t.Errorf("Activity lists %v, want the mention and the @channel reply as a reply", kinds)
	}
}
