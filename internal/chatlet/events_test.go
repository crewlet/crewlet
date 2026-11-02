package chatlet_test

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
)

// THE ROOM'S OWN CHANGES, AS LINES IN ITS TRANSCRIPT, and the reads that tell
// them from conversation: who joined, was added, left or was taken out, what
// the topic and description were set to, adding people additively, a public
// room that takes in everybody the company takes on, the mention count beside
// the unread one, a thread's summary and the rooms a reader may browse. Every
// case runs the real write path, the real applier and the real reader over one
// node's estate.

// reader is the read side over this round's own rows.
func (r *writeRound) reader() *chatlet.Reader {
	r.t.Helper()
	authority, err := statelogtest.LocalReaderOver(chatlet.Domain{}, r.db, r.waiter)
	if err != nil {
		r.t.Fatalf("build the read authority: %v", err)
	}
	reader, err := chatlet.NewReader(chatlet.ReaderOptions{
		Log: authority, Committed: r.waiter.Committed,
	})
	if err != nil {
		r.t.Fatalf("build the reader: %v", err)
	}
	return reader
}

// roomEvent is one event line as the transcript row holds it.
type roomEvent struct {
	author  string
	kind    chatlet.AuthorKind
	event   chatlet.RoomEvent
	seq     int64
	column  string
	thread  string
	channel string
}

// events is every event line of one room, in the room's own order, read
// through the document as every reader reads a message.
func (r *writeRound) events(channelID string) []roomEvent {
	r.t.Helper()
	var out []roomEvent
	err := r.db.Read(r.t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(r.t.Context(), `
			SELECT document, channel_seq, event FROM chat_messages
			 WHERE channel_id = ? AND event <> ''
			 ORDER BY channel_seq`, channelID)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var document []byte
			var line roomEvent
			if err := rows.Scan(&document, &line.seq, &line.column); err != nil {
				return err
			}
			message, err := chatlet.DecodeMessage(document)
			if err != nil {
				return err
			}
			if message.Event == nil {
				r.t.Errorf("row %d is marked %q and its document carries no event",
					line.seq, line.column)
				continue
			}
			line.author, line.kind, line.event = message.Author, message.AuthorKind, *message.Event
			line.thread, line.channel = message.ThreadRoot, message.ChannelID
			out = append(out, line)
		}
		return rows.Err()
	})
	if err != nil {
		r.t.Fatalf("read room %s's events: %v", channelID, err)
	}
	return out
}

// eventWords is a room's event lines as one comparable string each.
func eventWords(lines []roomEvent) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		word := l.author + " " + string(l.event.Kind)
		if len(l.event.Handles) > 0 {
			word += " " + strings.Join(l.event.Handles, ",")
		}
		if l.event.Text != "" {
			word += " " + strconvQuote(l.event.Text)
		}
		out = append(out, word)
	}
	return out
}

func strconvQuote(s string) string { return `"` + s + `"` }

// patch applies one room patch as actor and drains it.
func (r *writeRound) patch(actor chatlet.Actor, channelID string, p chatlet.ChannelPatch,
	authority chatlet.ChannelAuthority) (chatlet.Written, error) {

	r.t.Helper()
	got, err := r.store.PatchChannel(r.t.Context(), actor, channelID, p, authority)
	r.drain()
	return got, err
}

// A ROOM SAYS WHAT CHANGED IN IT, each change once, in the words of who made
// it: a named room opens with who joined it, a topic and a description are a
// line each, a template is one line in their place, an archive is none, and a
// membership is who came and went, in the voice of whoever moved them.
//
// Mutation: drop the template branch of patchEvents and the template patch
// writes two lines; write membershipEvents in the system's voice for everybody
// and `added` reads `joined`.
func TestARoomSaysWhatChangedInIt(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob", "kim"))
	room := r.room(person("jane"), "launch", "bob")
	id := room.Channel.ID

	topic, purpose := "shipping friday", "Where the launch is planned."
	if _, err := r.patch(person("jane"), id, chatlet.ChannelPatch{Topic: &topic},
		chatlet.ChannelAuthority{}); err != nil {
		t.Fatalf("set the topic: %v", err)
	}
	newTopic := "shipping monday"
	if _, err := r.patch(person("bob"), id, chatlet.ChannelPatch{Topic: &newTopic,
		Purpose: &purpose}, chatlet.ChannelAuthority{}); err != nil {
		t.Fatalf("set the topic and purpose: %v", err)
	}
	template, tTopic, tPurpose := "Release room", "Builds and sign-off.", "Release coordination."
	if _, err := r.patch(person("jane"), id, chatlet.ChannelPatch{Topic: &tTopic,
		Purpose: &tPurpose, Template: &template}, chatlet.ChannelAuthority{}); err != nil {
		t.Fatalf("set the room up from a template: %v", err)
	}
	archived, open := true, false
	for _, a := range []*bool{&archived, &open} {
		if _, err := r.patch(person("jane"), id, chatlet.ChannelPatch{Archived: a},
			chatlet.ChannelAuthority{Archive: true}); err != nil {
			t.Fatalf("archive or reopen: %v", err)
		}
	}
	if _, added, err := r.store.AddMembers(t.Context(), person("jane"), id,
		[]string{"kim", "sre"}); err != nil {
		t.Fatalf("add kim and sre: %v", err)
	} else if !slices.Equal(added, []string{"kim", "sre"}) {
		t.Errorf("add answered %v added, want [kim sre]", added)
	}
	r.drain()
	if _, err := r.store.SetMembers(t.Context(), person("bob"), id,
		[]chatlet.Member{{Handle: "jane"}, {Handle: "bob"}, {Handle: "sre"}}, nil); err != nil {
		t.Fatalf("take kim out: %v", err)
	}
	r.drain()
	if _, err := r.store.Join(t.Context(), seat("qa", "run-1"), id); err != nil {
		t.Fatalf("join: %v", err)
	}
	r.drain()
	if _, err := r.store.Leave(t.Context(), seat("qa", "run-2"), id); err != nil {
		t.Fatalf("leave: %v", err)
	}
	r.drain()

	got := eventWords(r.events(id))
	want := []string{
		"jane joined bob,jane",
		`jane topic "shipping friday"`,
		`bob description "Where the launch is planned."`,
		`bob topic "shipping monday"`,
		`jane template "Release room"`,
		"jane added kim,sre",
		"bob removed kim",
		"qa joined qa",
		"qa left qa",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the room's lines are\n  %s\nwant\n  %s", strings.Join(got, "\n  "),
			strings.Join(want, "\n  "))
	}
	for _, line := range r.events(id) {
		if line.column != string(line.event.Kind) {
			t.Errorf("an event row's column says %q and its document %q", line.column,
				line.event.Kind)
		}
		if line.thread != "" {
			t.Errorf("a %s line hangs off a thread", line.event.Kind)
		}
	}
	// AND THE ROOM'S NUMBERS STAY CONTIGUOUS through them, which is what a
	// screen holding the previous one applies the next by.
	if gaps := r.count(`SELECT MAX(channel_seq) - COUNT(*) FROM chat_messages
		WHERE channel_id = ?`, id); gaps != 0 {
		t.Errorf("the room's sequence has %d holes", gaps)
	}
}

// A DIRECT CONVERSATION AND THE ORG CHART'S OWN ROOM SPEAK IN THEIR OWN WAYS:
// a direct conversation opens with no line, since its people are what it is,
// and the org chart's changes to a unit's room are the engine's, so the seats
// it puts in joined and the seats it takes out left.
func TestADirectConversationHasNoLinesAndTheChartsAreTheEngines(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	direct, _, err := r.store.OpenDirect(t.Context(), person("jane"), []string{"pm"})
	if err != nil {
		t.Fatalf("open a conversation: %v", err)
	}
	r.drain()
	if lines := r.events(direct.Channel.ID); len(lines) != 0 {
		t.Errorf("a direct conversation opened with %v", eventWords(lines))
	}

	unit := r.unitRoom("eng", "engineering", "pm", "dev")
	if _, _, err := r.store.SetUnitMembers(t.Context(), orgChart, unit.Channel.ID,
		"engineering", members("pm", "qa"), chartActivation.AddDate(0, 0, 1)); err != nil {
		t.Fatalf("restaff the unit room: %v", err)
	}
	r.drain()
	got := eventWords(r.events(unit.Channel.ID))
	want := []string{"org-chart joined dev,pm", "org-chart joined qa", "org-chart left dev"}
	if !slices.Equal(got, want) {
		t.Errorf("the unit room's lines are %v, want %v", got, want)
	}
	for _, line := range r.events(unit.Channel.ID) {
		if line.kind != chatlet.AuthorSystem {
			t.Errorf("the chart's %s line is authored as %q", line.event.Kind, line.kind)
		}
	}
}

// A ROOM EVENT IS NOT CONVERSATION: nobody replies to it, reacts to it, edits
// it or takes it back, and it is never an unread message, a mention or the
// line a rail previews a room by.
func TestARoomEventIsNotConversation(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	room := r.room(person("jane"), "launch", "bob")
	id := room.Channel.ID
	lines := r.events(id)
	if len(lines) != 1 {
		t.Fatalf("the room opened with %d lines, want 1", len(lines))
	}
	var event string
	if err := r.db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT id FROM chat_messages
			WHERE channel_id = ? AND event <> ''`, id).Scan(&event)
	}); err != nil {
		t.Fatalf("read the event's id: %v", err)
	}
	body := "yes"
	for name, gesture := range map[string]func() error{
		"a reply": func() error {
			_, err := r.store.Reply(t.Context(), person("bob"), id, event,
				chatlet.NewMessage{Body: "welcome"})
			return err
		},
		"a reaction": func() error {
			_, err := r.store.React(t.Context(), person("bob"), id, event, "tada")
			return err
		},
		"an edit": func() error {
			_, err := r.store.Edit(t.Context(), person("jane"), id, event,
				chatlet.EditMessage{Body: body})
			return err
		},
		"a delete": func() error {
			_, err := r.store.Delete(t.Context(), person("jane"), id, event,
				chatlet.MessageAuthority{Moderate: true})
			return err
		},
	} {
		if err := gesture(); !errors.Is(err, chatlet.ErrInvalid) {
			t.Errorf("%s on a room event answered %v, want it refused invalid", name, err)
		}
	}

	rail, err := r.reader().Channels(t.Context(), "bob", chatlet.ChannelsQuery{},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read bob's rail: %v", err)
	}
	if len(rail.Channels) != 1 {
		t.Fatalf("bob's rail holds %d rooms, want 1", len(rail.Channels))
	}
	if got := rail.Channels[0]; got.Unread != 0 || got.Mentions != 0 || got.Last != nil {
		t.Errorf("a room holding only its opening line reads unread %d, mentions %d, "+
			"last %+v, want nothing: a line nobody said is no badge and no preview",
			got.Unread, got.Mentions, got.Last)
	}
}

// THE MENTION COUNT IS THE UNREAD ROOM POSTS THAT ASK FOR THE READER: those
// that name them and those that name the whole room, never their own, never one
// taken back and never a reply, which is read in its thread, counted over the
// same posts as the unread count.
//
// Mutation: drop `t.collective = 1` from the count and the `@channel` message
// is not counted; drop the LEFT JOIN's handle and every mention in the room
// counts for everybody.
func TestTheMentionCountIsWhatAsksForTheReader(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	room := r.room(person("jane"), "launch", "bob")
	id := room.Channel.ID
	post := func(actor chatlet.Actor, key string, in chatlet.NewMessage) chatlet.Written {
		got, err := r.store.Post(t.Context(), keyed(actor, requested(key)), id, in)
		if err != nil {
			t.Fatalf("post %q: %v", in.Body, err)
		}
		r.drain()
		return got
	}
	post(person("jane"), "k-1", chatlet.NewMessage{Body: "morning"})
	post(person("jane"), "k-2", chatlet.NewMessage{Body: "@bob can you look",
		Mentions: []string{"bob"}})
	post(person("jane"), "k-3", chatlet.NewMessage{Body: "@channel standup",
		Collective: true})
	taken := post(person("jane"), "k-4", chatlet.NewMessage{Body: "@bob never mind",
		Mentions: []string{"bob"}})
	if _, err := r.store.Delete(t.Context(), person("jane"), id, taken.Message.ID,
		chatlet.MessageAuthority{}); err != nil {
		t.Fatalf("take the last one back: %v", err)
	}
	r.drain()
	post(person("bob"), "k-5", chatlet.NewMessage{Body: "@bob note to self",
		Mentions: []string{"bob"}})
	// A REPLY IS READ IN ITS THREAD, mention and all: it badges neither count.
	if _, err := r.store.Reply(t.Context(), keyed(person("jane"), requested("k-6")), id,
		taken.Message.ID, chatlet.NewMessage{Body: "@bob in the thread",
			Mentions: []string{"bob"}}); err != nil {
		t.Fatalf("reply in the thread: %v", err)
	}
	r.drain()

	rail, err := r.reader().Channels(t.Context(), "bob", chatlet.ChannelsQuery{},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read bob's rail: %v", err)
	}
	got := rail.Channels[0]
	if got.Unread != 3 || got.Mentions != 2 {
		t.Errorf("bob's badge reads unread %d, mentions %d, want 3 and 2: three "+
			"room posts somebody else said and nobody took back, one naming him and "+
			"one naming the room; the reply naming him is read in its thread",
			got.Unread, got.Mentions)
	}
	jane, err := r.reader().Channels(t.Context(), "jane", chatlet.ChannelsQuery{},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read jane's rail: %v", err)
	}
	if m := jane.Channels[0].Mentions; m != 0 {
		t.Errorf("jane's mention count is %d, want 0: bob's note named only bob", m)
	}
}

// A ROOM POST SHOWS ITS THREAD: how many replies, the newest, and the first
// three people to reply in the order they first did, with how many replied in
// all, counted from the thread's own rows when the page is read.
func TestARoomPostShowsItsThread(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob", "kim", "ana"))
	room := r.room(person("jane"), "launch", "bob", "kim", "ana")
	id := room.Channel.ID
	root := r.say(person("jane"), id, "the plan", "k-root")
	quiet := r.say(person("jane"), id, "nobody answers this", "k-quiet")
	var last chatlet.Written
	for i, who := range []string{"bob", "kim", "bob", "ana", "jane"} {
		got, err := r.store.Reply(t.Context(), keyed(person(who), requested("k-r"+string(rune('a'+i)))),
			id, root.Message.ID, chatlet.NewMessage{Body: "reply " + who})
		if err != nil {
			t.Fatalf("reply as %s: %v", who, err)
		}
		r.drain()
		last = got
	}
	page, err := r.reader().Messages(t.Context(), "jane",
		chatlet.TranscriptQuery{ChannelID: id}, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read the room: %v", err)
	}
	byID := map[string]chatlet.MessageView{}
	for _, view := range page.Messages {
		byID[view.Message.ID] = view
	}
	summary := byID[root.Message.ID].Thread
	if summary == nil {
		t.Fatal("the root carries no thread summary")
	}
	if summary.Replies != 5 || summary.ReplierCount != 4 ||
		!slices.Equal(summary.Repliers, []string{"bob", "kim", "ana"}) {
		t.Errorf("the summary is %d replies by %d (%v), want 5 by 4 ([bob kim ana])",
			summary.Replies, summary.ReplierCount, summary.Repliers)
	}
	if want := byID[last.Message.ID]; !summary.LastReplyAt.Equal(want.Message.CreatedAt) ||
		summary.LastReplyPosition != want.Position {
		t.Errorf("the newest reply is said to be at %v (%v), want %v (%v)",
			summary.LastReplyAt, summary.LastReplyPosition, want.Message.CreatedAt,
			want.Position)
	}
	if byID[quiet.Message.ID].Thread != nil {
		t.Error("a post nobody answered carries a thread summary")
	}
	if byID[last.Message.ID].Thread != nil {
		t.Error("a reply carries a thread summary")
	}
}

// ADDING PEOPLE ADDS AND TAKES NOBODY OUT: it answers who it put in, leaves
// the people already there alone, and is refused where a membership cannot be
// changed this way, and for somebody the company does not have.
func TestAddingPeopleTakesNobodyOut(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob", "kim").withGone("ghost"))
	room := r.room(person("jane"), "launch", "bob")
	id := room.Channel.ID
	_, added, err := r.store.AddMembers(t.Context(), person("bob"), id, []string{"kim", "jane"})
	if err != nil {
		t.Fatalf("add kim: %v", err)
	}
	r.drain()
	if !slices.Equal(added, []string{"kim"}) {
		t.Errorf("added %v, want [kim]: jane was already in the room", added)
	}
	if n := r.count(`SELECT COUNT(*) FROM chat_members WHERE channel_id = ?`, id); n != 3 {
		t.Errorf("the room holds %d members, want 3", n)
	}
	before := r.logEnd()
	if _, again, err := r.store.AddMembers(t.Context(), person("bob"), id,
		[]string{"kim"}); err != nil || len(again) != 0 {
		t.Errorf("adding kim again answered %v, %v, want nothing added", again, err)
	}
	if r.logEnd() != before {
		t.Error("adding somebody already in the room published a record")
	}
	if _, _, err := r.store.AddMembers(t.Context(), person("bob"), id,
		[]string{"ghost"}); !errors.Is(err, chatlet.ErrInvalid) {
		t.Errorf("adding a seat the company does not have answered %v, want invalid", err)
	}
	unit := r.unitRoom("eng", "engineering", "pm")
	if _, _, err := r.store.AddMembers(t.Context(), person("jane"), unit.Channel.ID,
		[]string{"kim"}); !errors.Is(err, chatlet.ErrForbidden) {
		t.Errorf("adding to a unit's room answered %v, want forbidden", err)
	}
	direct, _, err := r.store.OpenDirect(t.Context(), person("jane"), []string{"bob"})
	if err != nil {
		t.Fatalf("open a conversation: %v", err)
	}
	r.drain()
	if _, _, err := r.store.AddMembers(t.Context(), person("jane"), direct.Channel.ID,
		[]string{"kim"}); !errors.Is(err, chatlet.ErrForbidden) {
		t.Errorf("adding to a direct conversation answered %v, want forbidden", err)
	}
}

// A PUBLIC ROOM TAKES IN EVERYBODY THE COMPANY TAKES ON, once: turning it on
// adds nobody already in the company, the engine puts each newcomer in at the
// next activation, a newcomer who leaves is not put back, and only an
// administrator turns it on, on a public room only.
func TestAnAutoJoinRoomTakesInNewcomersOnce(t *testing.T) {
	t.Parallel()
	roster := agents().withPeople("jane", "bob")
	roster.seats = []string{"bob", "jane", "pm"}
	r := newWriteRound(t, roster)
	room := r.room(person("jane"), "general")
	id := room.Channel.ID
	on := true
	if _, err := r.patch(person("jane"), id, chatlet.ChannelPatch{AutoJoin: &on},
		chatlet.ChannelAuthority{}); !errors.Is(err, chatlet.ErrForbidden) {
		t.Errorf("turning auto-join on without an administrator's admission answered "+
			"%v, want forbidden", err)
	}
	if _, err := r.patch(person("jane"), id, chatlet.ChannelPatch{AutoJoin: &on},
		chatlet.ChannelAuthority{AutoJoin: true}); err != nil {
		t.Fatalf("turn auto-join on: %v", err)
	}
	engine := chatlet.Actor{Handle: "auto-join", Kind: chatlet.AuthorSystem}
	join := func(seats ...string) []string {
		_, added, err := r.store.AutoJoin(t.Context(), engine, id, seats)
		if err != nil {
			t.Fatalf("auto-join %v: %v", seats, err)
		}
		r.drain()
		return added
	}
	if added := join("bob", "jane", "pm"); len(added) != 0 {
		t.Errorf("the company as it stood was put in the room: %v", added)
	}
	if added := join("bob", "jane", "pm", "qa", "sre"); !slices.Equal(added, []string{"qa", "sre"}) {
		t.Errorf("the newcomers put in are %v, want [qa sre]", added)
	}
	if _, err := r.store.Leave(t.Context(), seat("qa", "run-1"), id); err != nil {
		t.Fatalf("qa leaves: %v", err)
	}
	r.drain()
	if added := join("bob", "jane", "pm", "qa", "sre"); len(added) != 0 {
		t.Errorf("a newcomer who left was put back: %v", added)
	}
	lines := eventWords(r.events(id))
	if !slices.Contains(lines, "auto-join joined qa,sre") {
		t.Errorf("the room's lines %v do not say the engine put the newcomers in", lines)
	}
	if _, _, err := r.store.AutoJoin(t.Context(), person("jane"), id,
		[]string{"intruder"}); !errors.Is(err, chatlet.ErrForbidden) {
		t.Errorf("a person running auto-join answered %v, want forbidden", err)
	}

	private, err := r.store.CreateChannel(t.Context(), person("jane"), chatlet.NewChannel{
		Name: "leads", Kind: chatlet.KindPrivate, Members: members("bob"),
	})
	if err != nil {
		t.Fatalf("make a private room: %v", err)
	}
	r.drain()
	if _, err := r.patch(person("jane"), private.Channel.ID, chatlet.ChannelPatch{AutoJoin: &on},
		chatlet.ChannelAuthority{AutoJoin: true}); !errors.Is(err, chatlet.ErrInvalid) {
		t.Errorf("auto-join on a private room answered %v, want invalid", err)
	}
	rooms, err := r.store.AutoJoinRooms(t.Context())
	if err != nil || !slices.Equal(rooms, []string{id}) {
		t.Errorf("the auto-join rooms are %v (%v), want [%s]", rooms, err, id)
	}
}

// THE ROOMS A READER MAY BROWSE are every named room they may read, joined or
// not, with how many are in each and whether they are, and never a private
// room they are not in or a direct conversation; and the rooms in common with
// somebody are the reader's own rooms that person is in too.
func TestTheRoomsAReaderMayBrowse(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob", "kim"))
	launch := r.room(person("jane"), "launch", "bob")
	ops := r.room(person("kim"), "ops")
	if _, err := r.store.CreateChannel(t.Context(), person("kim"), chatlet.NewChannel{
		Name: "secret", Kind: chatlet.KindPrivate, Members: members("jane"),
	}); err != nil {
		t.Fatalf("make a private room: %v", err)
	}
	r.drain()
	if _, _, err := r.store.OpenDirect(t.Context(), person("bob"), []string{"jane"}); err != nil {
		t.Fatalf("open a conversation: %v", err)
	}
	r.drain()
	r.say(person("jane"), launch.Channel.ID, "hello", "k-1")

	fresh := statelog.Freshness{Level: statelog.ReadStale}
	dir, err := r.reader().Directory(t.Context(), "bob", fresh)
	if err != nil {
		t.Fatalf("browse as bob: %v", err)
	}
	var names []string
	for _, room := range dir.Rooms {
		names = append(names, room.Channel.Name)
		switch room.Channel.Name {
		case "launch":
			if room.Members != 2 || !room.Member || room.LastAt == nil {
				t.Errorf("#launch reads %d members, member %v, last %v", room.Members,
					room.Member, room.LastAt)
			}
		case "ops":
			if room.Members != 1 || room.Member || room.LastAt != nil {
				t.Errorf("#ops reads %d members, member %v, last %v: nobody has "+
					"spoken there and bob is not in it", room.Members, room.Member, room.LastAt)
			}
		}
	}
	if !slices.Equal(names, []string{"launch", "ops"}) {
		t.Errorf("bob may browse %v, want [launch ops]", names)
	}

	common, err := r.reader().Channels(t.Context(), "jane",
		chatlet.ChannelsQuery{With: "bob"}, fresh)
	if err != nil {
		t.Fatalf("jane's rooms with bob: %v", err)
	}
	var shared []string
	for _, room := range common.Channels {
		shared = append(shared, room.Channel.Name)
	}
	slices.Sort(shared)
	if !slices.Equal(shared, []string{"", "launch"}) {
		t.Errorf("jane shares %v with bob, want #launch and their conversation", shared)
	}
	_ = ops
}
