package chatlet_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
)

// The reader's own views, over a real replicated estate fed by the shipped
// applier: each is a claim about what one seat is shown across rooms, which no
// per-room read makes.

// readReact builds one reaction to a message.
func readReact(channelID, messageID, emoji, by string) chatlet.MutationRecord {
	return readRecord(chatlet.MessageSubject(channelID), chatlet.OpReact,
		"op-react-"+messageID+emoji+by, chatlet.Reaction{V: chatlet.DocumentVersion,
			MessageID: messageID, Emoji: emoji, By: by, Cap: chatlet.MaxReactionEmoji},
		chatlet.ScopeSet{Subject: true}, by)
}

// readLinked builds one message that carries links.
func readLinked(channelID, messageID, author string, links ...string) chatlet.MutationRecord {
	return readRecord(chatlet.MessageSubject(channelID), chatlet.OpPost,
		"op-post-"+messageID, chatlet.MessagePost{
			V: chatlet.DocumentVersion, MessageID: messageID, Body: "see this",
			Author: author, AuthorKind: chatlet.AuthorHuman, Links: links,
		}, chatlet.ScopeSet{Subject: true}, author)
}

// activityWorld is one company's conversation from jane's side: a public room
// she is in, a private room she is not in, a thread she replied in, a thread
// she unfollowed, and a reaction to her message.
func activityWorld(t *testing.T) *readHarness {
	t.Helper()
	h := newReadHarness(t)
	h.apply(readCreateRoom("room-pub", "general", chatlet.KindPublic, "ada", "jane", "sam"))
	h.apply(readCreateRoom("room-priv", "secret", chatlet.KindPrivate, "ada", "sam"))
	h.apply(readPost("room-pub", "root-a", "ada", "who takes the release?"))
	h.apply(readReply("room-pub", "root-a", "reply-jane", "jane", "I can"))
	h.apply(readReply("room-pub", "root-a", "reply-sam", "sam", "thanks jane"))
	h.apply(readPost("room-pub", "root-b", "sam", "lunch?"))
	h.apply(readReply("room-pub", "root-b", "reply-jane-b", "jane", "yes"))
	h.apply(readReply("room-pub", "root-b", "reply-ada-b", "ada", "me too"))
	h.apply(readPost("room-priv", "priv-1", "ada", "@jane between us", "jane"))
	h.apply(readPost("room-pub", "named", "ada", "@jane review please", "jane"))
	h.apply(readReact("room-pub", "reply-jane", ":+1:", "sam"))
	h.apply(readReact("room-pub", "reply-jane", ":eyes:", "jane"))
	return h
}

// ACTIVITY IS WHAT WAITS FOR THE READER, NEWEST FIRST, AND NOTHING ELSE.
//
// A mention of them, a reply in a thread they spoke in and a reaction to their
// message, merged by when each happened. Their own reply and their own
// reaction are not news; a thread they unfollowed is quiet; a mention in a
// private room they are not in is not shown, exactly as the room is not.
//
// Mutation: drop the unfollow filter and root-b's reply is listed; drop the
// author filter and jane's own reply is.
func TestActivityIsWhatWaitsForTheReader(t *testing.T) {
	t.Parallel()
	h := activityWorld(t)
	marks := chatlet.ReaderMarks{Known: true,
		Unfollowed: []chatlet.ThreadRef{{ChannelID: "room-pub", RootID: "root-b"}}}
	got, err := h.reader().Activity(t.Context(), "jane",
		chatlet.ActivityQuery{Marks: marks}, h.session())
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	var keys []string
	for _, item := range got.Items {
		keys = append(keys, string(item.Kind)+" "+item.MessageID)
	}
	want := []string{"reaction reply-jane", "mention named", "reply reply-sam"}
	if !slices.Equal(keys, want) {
		t.Fatalf("Activity is %v, want %v", keys, want)
	}
	if got.Items[0].Actor != "sam" || got.Items[0].Emoji != ":+1:" || got.Items[0].Unread {
		t.Errorf("the reaction row is %+v, want sam's :+1: and never unread", got.Items[0])
	}
	if !got.Items[1].Unread || got.Items[1].ReadThrough == 0 {
		t.Errorf("a mention above no cursor is %+v, want unread with a place to read through",
			got.Items[1])
	}
	if got.Unread.Mention != 1 || got.Unread.Reply != 1 {
		t.Errorf("the counts are %+v, want one unread mention and one unread reply", got.Unread)
	}
}

// A MARK READ BY THE ROOM'S CURSOR IS READ IN ACTIVITY, and a row's own
// read_through is what marks it: the badge and the feed agree.
func TestActivityRowsAreReadByTheReadersPlaces(t *testing.T) {
	t.Parallel()
	h := activityWorld(t)
	first, err := h.reader().Activity(t.Context(), "jane",
		chatlet.ActivityQuery{Marks: chatlet.ReaderMarks{Known: true}}, h.session())
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	var through int64
	for _, item := range first.Items {
		through = max(through, item.ReadThrough)
	}
	marks := chatlet.ReaderMarks{Known: true, Cursors: map[string]int64{"room-pub": through}}
	read, err := h.reader().Activity(t.Context(), "jane",
		chatlet.ActivityQuery{Marks: marks}, h.session())
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	for _, item := range read.Items {
		if item.Unread {
			t.Errorf("%s %s is still unread past the room's cursor", item.Kind, item.MessageID)
		}
	}
	if read.Unread.Mention != 0 || read.Unread.Reply != 0 {
		t.Errorf("the counts past the cursor are %+v, want none", read.Unread)
	}
	only, err := h.reader().Activity(t.Context(), "jane",
		chatlet.ActivityQuery{Marks: marks, Unread: true}, h.session())
	if err != nil || len(only.Items) != 0 {
		t.Errorf("unread alone past the cursor answered %+v, %v, want nothing", only.Items, err)
	}
	_, err = h.reader().Activity(t.Context(), "jane",
		chatlet.ActivityQuery{Unread: true}, h.session())
	if !errors.Is(err, chatlet.ErrInvalid) {
		t.Errorf("unread over a record nobody could read answered %v, want invalid", err)
	}
}

// ACTIVITY PAGES WITH NO GAP AND NO REPEAT across its three sources: a page of
// one, followed by its cursor, walks the same rows a single page lists.
//
// Mutation: drop the floor of a full window in the merge and the walk skips
// the reply, which is older than the mention but was not read past.
func TestActivityPagesWithNoGapAndNoRepeat(t *testing.T) {
	t.Parallel()
	h := activityWorld(t)
	marks := chatlet.ReaderMarks{Known: true}
	whole, err := h.reader().Activity(t.Context(), "jane",
		chatlet.ActivityQuery{Marks: marks}, h.session())
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	var walked []string
	cursor := ""
	for range 10 {
		page, err := h.reader().Activity(t.Context(), "jane",
			chatlet.ActivityQuery{Marks: marks, Cursor: cursor, Limit: 1}, h.session())
		if err != nil {
			t.Fatalf("Activity page: %v", err)
		}
		for _, item := range page.Items {
			walked = append(walked, item.Key)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	var want []string
	for _, item := range whole.Items {
		want = append(want, item.Key)
	}
	if !slices.Equal(walked, want) {
		t.Errorf("paging one at a time walked %v, want %v", walked, want)
	}
}

// THE THREADS A READER FOLLOWS ARE THE ONES THEY SPOKE IN, LESS THE ONES THEY
// LEFT, PLUS THE ONES THEY MARKED, newest reply first, each with its root and
// its newest replies.
func TestThreadsAreTheOnesTheReaderFollows(t *testing.T) {
	t.Parallel()
	h := activityWorld(t)
	h.apply(readPost("room-pub", "root-c", "sam", "a thread jane never spoke in"))
	h.apply(readReply("room-pub", "root-c", "reply-c", "ada", "noted"))
	marks := chatlet.ReaderMarks{Known: true,
		Following:  []chatlet.ThreadRef{{ChannelID: "room-pub", RootID: "root-c"}},
		Unfollowed: []chatlet.ThreadRef{{ChannelID: "room-pub", RootID: "root-b"}}}
	got, err := h.reader().Threads(t.Context(), "jane",
		chatlet.ThreadsQuery{Marks: marks}, h.session())
	if err != nil {
		t.Fatalf("Threads: %v", err)
	}
	var roots []string
	for _, thread := range got.Threads {
		roots = append(roots, thread.Root.Message.ID+" "+string(thread.Reason))
	}
	if want := []string{"root-c marked", "root-a replied"}; !slices.Equal(roots, want) {
		t.Fatalf("the threads are %v, want %v", roots, want)
	}
	latest := got.Threads[1].Latest
	if len(latest) != 2 || latest[0].Message.ID != "reply-jane" || latest[1].Message.ID != "reply-sam" {
		t.Errorf("root-a carries %+v, want its two newest replies oldest first", latest)
	}
}

// A SAVED MESSAGE IS SHOWN ONLY IN THE ROOM ITS MARK NAMES, and only when the
// reader may read that room: a mark is the reader's own value, and a mark that
// named a private room's message under a public room's id must show nothing.
//
// Mutation: drop the room check on the message and the forged mark shows the
// private message.
func TestASavedMessageIsShownOnlyWhereTheReaderMayReadIt(t *testing.T) {
	t.Parallel()
	h := activityWorld(t)
	got, err := h.reader().Saved(t.Context(), "jane", chatlet.SavedQuery{Saved: []chatlet.SavedMark{
		{ChannelID: "room-pub", MessageID: "root-a"},
		{ChannelID: "room-pub", MessageID: "priv-1"},
		{ChannelID: "room-priv", MessageID: "priv-1"},
	}}, h.session())
	if err != nil {
		t.Fatalf("Saved: %v", err)
	}
	if len(got.Saved) != 3 || got.Saved[0].Gone || got.Saved[0].View == nil {
		t.Fatalf("the readable save is %+v, want its message", got.Saved)
	}
	for _, entry := range got.Saved[1:] {
		if !entry.Gone || entry.View != nil {
			t.Errorf("the save of %s under %s shows %+v, want it gone",
				entry.Mark.MessageID, entry.Mark.ChannelID, entry.View)
		}
	}
}

// WHAT A READER SENT IS THEIRS, NEWEST FIRST, in every room they may read.
func TestSentIsWhatTheReaderSaidNewestFirst(t *testing.T) {
	t.Parallel()
	h := activityWorld(t)
	got, err := h.reader().Sent(t.Context(), "jane", chatlet.SentQuery{}, h.session())
	if err != nil {
		t.Fatalf("Sent: %v", err)
	}
	var ids []string
	for _, m := range got.Messages {
		ids = append(ids, m.View.Message.ID)
	}
	if want := []string{"reply-jane-b", "reply-jane"}; !slices.Equal(ids, want) {
		t.Errorf("jane sent %v, want %v", ids, want)
	}
}

// A ROOM'S SHARED LINKS ARE EVERY LINK IT CARRIED, NEWEST FIRST, and a room
// the reader may not read has none to show: it is not found.
func TestARoomsSharedLinksAreEveryLinkNewestFirst(t *testing.T) {
	t.Parallel()
	h := activityWorld(t)
	h.apply(readLinked("room-pub", "l-1", "ada", "https://example.com/a"))
	h.apply(readLinked("room-pub", "l-2", "sam", "https://example.com/b", "https://example.com/c"))
	h.apply(readLinked("room-priv", "l-3", "ada", "https://example.com/d"))
	got, err := h.reader().Links(t.Context(), "jane",
		chatlet.LinksQuery{ChannelID: "room-pub"}, h.session())
	if err != nil {
		t.Fatalf("Links: %v", err)
	}
	var urls []string
	for _, l := range got.Links {
		urls = append(urls, l.URL)
	}
	want := []string{"https://example.com/b", "https://example.com/c", "https://example.com/a"}
	if !slices.Equal(urls, want) {
		t.Errorf("the room shared %v, want %v", urls, want)
	}
	if _, err := h.reader().Links(t.Context(), "jane",
		chatlet.LinksQuery{ChannelID: "room-priv"}, h.session()); !errors.Is(err, chatlet.ErrNotFound) {
		t.Errorf("a private room's links answered %v to an outsider, want not found", err)
	}
}

// ONE MESSAGE READ IN PLACE CARRIES ITS THREAD: the root and the replies just
// before it, oldest first.
func TestOneMessageIsReadWithItsThread(t *testing.T) {
	t.Parallel()
	h := activityWorld(t)
	got, err := h.reader().Message(t.Context(), "jane",
		chatlet.MessageQuery{ChannelID: "room-pub", MessageID: "reply-sam"}, h.session())
	if err != nil {
		t.Fatalf("Message: %v", err)
	}
	if got.Root == nil || got.Root.Message.ID != "root-a" {
		t.Fatalf("the reply's root is %+v, want root-a", got.Root)
	}
	if len(got.Before) != 1 || got.Before[0].Message.ID != "reply-jane" {
		t.Errorf("the replies before it are %+v, want reply-jane", got.Before)
	}
	if _, err := h.reader().Message(t.Context(), "jane",
		chatlet.MessageQuery{ChannelID: "room-pub", MessageID: "priv-1"}, h.session()); !errors.Is(err, chatlet.ErrNotFound) {
		t.Errorf("a message named under a room it is not in answered %v, want not found", err)
	}
}

// A MESSAGE TO THE WHOLE ROOM IS A MENTION OF EVERY MEMBER, and of nobody
// outside it: `@channel` names nobody in the mention rows, so Activity reads
// it from the reader's own memberships.
//
// Mutation: drop the collective source and the row is missing.
func TestAMessageToTheWholeRoomIsAMentionOfItsMembers(t *testing.T) {
	t.Parallel()
	h := activityWorld(t)
	h.apply(readRecord(chatlet.MessageSubject("room-pub"), chatlet.OpPost,
		"op-post-all", chatlet.MessagePost{V: chatlet.DocumentVersion, MessageID: "all",
			Body: "@channel standup moved", Author: "ada",
			AuthorKind: chatlet.AuthorHuman, Collective: true},
		chatlet.ScopeSet{Subject: true}, "ada"))
	for viewer, want := range map[string]bool{"jane": true, "outsider": false} {
		got, err := h.reader().Activity(t.Context(), viewer,
			chatlet.ActivityQuery{Kind: chatlet.ActivityMention,
				Marks: chatlet.ReaderMarks{Known: true}}, h.session())
		if err != nil {
			t.Fatalf("Activity for %s: %v", viewer, err)
		}
		found := slices.ContainsFunc(got.Items, func(i chatlet.ActivityItem) bool {
			return i.MessageID == "all" && i.Collective
		})
		if found != want {
			t.Errorf("%s's Activity lists the whole-room message: %v, want %v", viewer, found, want)
		}
	}
}
