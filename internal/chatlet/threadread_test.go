package chatlet_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/statelog"
)

// readReply builds one reply into a thread.
func readReply(channelID, root, messageID, author, body string) chatlet.MutationRecord {
	return readRecord(chatlet.MessageSubject(channelID), chatlet.OpPost,
		"op-post-"+messageID, chatlet.MessagePost{
			V: chatlet.DocumentVersion, MessageID: messageID, Body: body,
			ThreadRoot: root, Author: author, AuthorKind: chatlet.AuthorHuman,
		}, chatlet.ScopeSet{Subject: true}, author)
}

// A THREAD IS READ FROM ITS NEWEST END, AND WHAT CAME BEFORE IS COUNTED.
//
// What a seat woken in a thread needs is its newest end — the message that
// woke it is the thread's last — and the forward page could reach it only by
// walking the thread from the root. The newest end comes back in the order a
// conversation is read, and the replies before it are a count in the same
// snapshot rather than a guess.
func TestAThreadIsReadFromItsNewestEndWithTheRestCounted(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	h.apply(readCreateRoom("room-1", "launch", chatlet.KindPublic, "ada"))
	h.apply(readPost("room-1", "root", "ada", "the root question"))
	for i := 1; i <= 5; i++ {
		h.apply(readReply("room-1", "root", fmt.Sprintf("reply-%d", i), "ada",
			fmt.Sprintf("reply %d", i)))
	}

	got, err := h.reader().Thread(t.Context(), "ada", chatlet.ThreadQuery{
		ChannelID: "room-1", RootID: "root", Newest: 2,
	}, h.session())
	if err != nil {
		t.Fatalf("read the newest end: %v", err)
	}
	if got.Root.Message.ID != "root" {
		t.Errorf("the newest end came back without its root: %q", got.Root.Message.ID)
	}
	var bodies []string
	for _, r := range got.Replies {
		bodies = append(bodies, r.Message.Body)
	}
	if want := []string{"reply 4", "reply 5"}; !slices.Equal(bodies, want) {
		t.Errorf("the newest two replies read as %v, want %v — the newest, "+
			"oldest first among themselves", bodies, want)
	}
	if got.Earlier != 3 {
		t.Errorf("the replies before the newest two counted %d, want 3", got.Earlier)
	}

	whole, err := h.reader().Thread(t.Context(), "ada", chatlet.ThreadQuery{
		ChannelID: "room-1", RootID: "root", Newest: 50,
	}, h.session())
	if err != nil {
		t.Fatalf("read a window wider than the thread: %v", err)
	}
	if len(whole.Replies) != 5 || whole.Earlier != 0 {
		t.Errorf("a window wider than the thread answered %d replies and %d "+
			"earlier, want 5 and 0", len(whole.Replies), whole.Earlier)
	}

	if _, err := h.reader().Thread(t.Context(), "ada", chatlet.ThreadQuery{
		ChannelID: "room-1", RootID: "root", Newest: 2, Cursor: "3",
	}, h.session()); !errors.Is(err, chatlet.ErrInvalid) {
		t.Errorf("a newest-end read beside a forward cursor answered %v, want %v",
			err, chatlet.ErrInvalid)
	}
}

// THE NEWEST END STEPS OVER A DELETED REPLY, AND COUNTS IT.
//
// A tombstone is a blank line the block drops, so answering one in the window
// is a reply that could have stood in its place — the reader asked for as many
// as the block renders, and every one it answers must be one the block can
// render. The deleted reply is still a reply: it is counted with the rest.
func TestTheNewestEndStepsOverADeletedReplyAndCountsIt(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	h.apply(readCreateRoom("room-1", "launch", chatlet.KindPublic, "ada"))
	h.apply(readPost("room-1", "root", "ada", "the root question"))
	for i := 1; i <= 6; i++ {
		h.apply(readReply("room-1", "root", fmt.Sprintf("reply-%d", i), "ada",
			fmt.Sprintf("reply %d", i)))
	}
	h.apply(readRecord(chatlet.MessageSubject("room-1"), chatlet.OpDelete,
		"op-delete-reply-5", chatlet.MessageDelete{
			V: chatlet.DocumentVersion, MessageID: "reply-5", DeletedBy: "ada",
			DeletedByKind: chatlet.AuthorHuman,
		}, chatlet.ScopeSet{Subject: true}, "ada"))

	got, err := h.reader().Thread(t.Context(), "ada", chatlet.ThreadQuery{
		ChannelID: "room-1", RootID: "root", Newest: 2,
	}, h.session())
	if err != nil {
		t.Fatalf("read the newest end: %v", err)
	}
	var ids []string
	for _, r := range got.Replies {
		ids = append(ids, r.Message.ID)
	}
	if want := []string{"reply-4", "reply-6"}; !slices.Equal(ids, want) {
		t.Errorf("the newest two standing replies are %v, want %v — a tombstone "+
			"answered in the window is a reply the block drops", ids, want)
	}
	if got.Earlier != 4 || got.Unreadable != 0 {
		t.Errorf("earlier %d, unreadable %d — want the three before the window "+
			"and the one deleted inside it, 4, and nothing unreadable",
			got.Earlier, got.Unreadable)
	}
}

// A REPLY THIS BUILD CANNOT DECODE IS UNREADABLE, NOT EARLIER.
//
// The thread reader hands the renderer Earlier + Unreadable as the count of
// what is not in front of the seat, so a reply in the window a newer build
// wrote must be in exactly one of them: counted in both, the seat is told one
// more message is missing than is.
func TestAnUndecodableReplyInTheWindowIsCountedOnce(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	h.apply(readCreateRoom("room-1", "launch", chatlet.KindPublic, "ada"))
	h.apply(readPost("room-1", "root", "ada", "the root question"))
	for i := 1; i <= 5; i++ {
		h.apply(readReply("room-1", "root", fmt.Sprintf("reply-%d", i), "ada",
			fmt.Sprintf("reply %d", i)))
	}
	// A NEWER BUILD'S DOCUMENT, which this one refuses to decode.
	if err := h.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`UPDATE chat_messages SET document = ? WHERE id = 'reply-5'`,
			[]byte(`{"v":999,"id":"reply-5"}`))
		return err
	}); err != nil {
		t.Fatalf("plant a newer build's reply: %v", err)
	}

	got, err := h.reader().Thread(t.Context(), "ada", chatlet.ThreadQuery{
		ChannelID: "room-1", RootID: "root", Newest: 2,
	}, h.session())
	if err != nil {
		t.Fatalf("read the newest end: %v", err)
	}
	if len(got.Replies) != 1 || got.Replies[0].Message.ID != "reply-4" {
		t.Fatalf("the window answered %+v, want reply-4 beside the one it "+
			"could not decode", got.Replies)
	}
	if got.Unreadable != 1 || got.Earlier != 3 {
		t.Errorf("unreadable %d, earlier %d — want the undecodable reply counted "+
			"once, as unreadable, and the three before the window as earlier",
			got.Unreadable, got.Earlier)
	}
	if total := len(got.Replies) + got.Earlier + got.Unreadable; total != 5 {
		t.Errorf("the read accounts for %d replies, want the thread's 5", total)
	}
}

// threadSource is a [chatlet.ThreadSource] that answers one thread and records
// what it was asked.
type threadSource struct {
	thread chatlet.Thread
	err    error

	viewer string
	query  chatlet.ThreadQuery
	fresh  statelog.Freshness
}

func (s *threadSource) Thread(_ context.Context, viewer string, q chatlet.ThreadQuery,
	fresh statelog.Freshness) (chatlet.Thread, error) {

	s.viewer, s.query, s.fresh = viewer, q, fresh
	return s.thread, s.err
}

// roster is the company's party registry as these cases need it.
type roster map[string]notify.Party

func (r roster) ByExternalID(string, string) (notify.Party, bool) { return notify.Party{}, false }
func (r roster) ByHandle(handle string) (notify.Party, bool) {
	p, ok := r[handle]
	return p, ok
}

func view(id, author string, kind chatlet.AuthorKind, body string, links ...string) chatlet.MessageView {
	return chatlet.MessageView{Message: chatlet.Message{
		ID: id, Author: author, AuthorKind: kind, Body: body, Links: links,
	}}
}

// A CHATLET THREAD IS HANDED TO THE TURN IT WOKE, AS THE SEAT IT WOKE.
//
// Every Chatlet thread reply used to be told its thread "could not be read from
// this node": there was no reader for the transport the chat parser stamps.
// The transcript holds the root first, the seat's own replies marked, every
// author named the way the woken message's own sender is, and what the window
// or this build could not show counted rather than hidden.
func TestAChatletThreadIsReadAsTheSeatItWoke(t *testing.T) {
	t.Parallel()
	source := &threadSource{thread: chatlet.Thread{
		ChannelID: "room-1",
		Root:      view("root", "ana", chatlet.AuthorHuman, "what is the plan"),
		Replies: []chatlet.MessageView{
			view("m-1", "swe", chatlet.AuthorAgent, "on it"),
			view("m-2", "org-chart", chatlet.AuthorSystem, "dana joined"),
			view("m-3", "ana", chatlet.AuthorHuman, "", "https://example.com/spec"),
			view("m-4", "ana", chatlet.AuthorHuman, ""),
		},
		Earlier: 40, Unreadable: 2,
	}}
	parties := roster{"ana": {Handle: "ana", Name: "Ana Ruiz", Human: true}}
	reader := chatlet.NewThreadReader(source, func() notify.Parties { return parties })

	if got := reader.ThreadBackend(); got != chatlet.Source {
		t.Fatalf("the reader answers for %q, want the transport the parser stamps, %q",
			got, chatlet.Source)
	}
	got, ok := reader.ReadThread(t.Context(), "swe", "room-1", "root")
	if !ok {
		t.Fatal("a thread the source answered was reported unreadable")
	}
	if source.viewer != "swe" || source.query.ChannelID != "room-1" ||
		source.query.RootID != "root" || source.query.Newest == 0 {
		t.Errorf("the source was asked %q / %+v, want the woken seat's own read of "+
			"the thread's newest end", source.viewer, source.query)
	}
	// THE WINDOW IS THE MOST ONE READ ANSWERS, root aside. The block is
	// bounded by bytes and CONDENSES the middle of a long thread rather
	// than dropping it, so a window sized to what it renders verbatim — it
	// was thirty, the count bound the block no longer has — cut exactly
	// the middle the condensation is for, and a Chatlet thread past thirty
	// lost it as "not shown" where a Mattermost thread, read whole, was
	// condensed.
	if source.query.Newest != chatlet.MaxLimit {
		t.Errorf("the newest-end read asks for %d replies, want %d — as many "+
			"as one read answers, leaving the bound to the block",
			source.query.Newest, chatlet.MaxLimit)
	}
	if want := statelog.DefaultReadLevel(statelog.SurfaceSeat); source.fresh.Level != want {
		t.Errorf("the thread was read at %q, want a seat's own level %q",
			source.fresh.Level, want)
	}
	if len(got.Messages) != 5 {
		t.Fatalf("the transcript holds %d messages, want the root and four replies",
			len(got.Messages))
	}
	// THE ENGINE HOSTS THIS SURFACE, and the transcript says so: only company
	// principals send here, so a line whose sender the roster no longer
	// names is a colleague's to the block that fences it (ADR-0040).
	if !got.Hosted {
		t.Error("a Chatlet transcript does not say the engine hosts it, so the turn's " +
			"block would read a sender nobody can name as an outside party")
	}
	// EVERY MESSAGE CARRIES ITS OWN ID, the value a Chatlet wake stamps as
	// the trigger's `ts`: it is how the block says which waiting messages a
	// turn was shown, and a transcript without it matched nothing.
	for i, want := range []string{"root", "m-1", "m-2", "m-3", "m-4"} {
		if got.Messages[i].ID != want {
			t.Errorf("message %d carries id %q, want %q", i, got.Messages[i].ID, want)
		}
	}
	// A PERSON IS NAMED AS THE PERSON the roster says they are, never by
	// their kind's fallback: labelled "(a person …)" by the seat they hold,
	// every line they said would read to the seat as somebody the roster
	// does not know.
	root := got.Messages[0]
	if root.Text != "what is the plan" || !strings.Contains(root.SenderName, "Ana Ruiz") ||
		strings.Contains(root.SenderName, "(a person") {
		t.Errorf("the root reads %+v, want ana's question named as she is", root)
	}
	if own := got.Messages[1]; !own.Own || got.Messages[0].Own {
		t.Errorf("the seat's own reply is marked %v and the root %v — an agent "+
			"must not read its own words as a colleague's", own.Own, got.Messages[0].Own)
	}
	if engine := got.Messages[2].SenderName; !strings.Contains(engine, "the engine") {
		t.Errorf("a system line is attributed to %q, want the engine named as itself",
			engine)
	}
	if links := got.Messages[3].Text; links != "https://example.com/spec" {
		t.Errorf("a message carrying only a link reads %q, want the link", links)
	}
	if blank := got.Messages[4].Text; blank != "" {
		t.Errorf("a tombstone reads %q, want nothing", blank)
	}
	if got.Older != 42 || got.StoppedShort {
		t.Errorf("the transcript counts %d not shown (stopped short: %v), want the "+
			"40 the window left plus the 2 this build cannot read, and a thread "+
			"read from its newest end never stops short", got.Older, got.StoppedShort)
	}

	source.err = fmt.Errorf("%w: the room", chatlet.ErrNotFound)
	if _, ok := reader.ReadThread(t.Context(), "swe", "room-1", "root"); ok {
		t.Error("a refused read was reported as a thread")
	}
}
