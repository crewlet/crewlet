package chatlet_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// The read side's own guards, over a real replicated estate fed by the shipped
// applier.
//
// Each case here is a claim no other suite can make: the applier's cases are
// about the rows a record writes, the write path's are about what reaches the
// log, and these are about what somebody is ALLOWED and ABLE to read back —
// who may open a room, what a cursor does across a reanchor, what an unread
// count costs, and what a node answers when it is behind rather than wrong.
//
// THE HELPERS ARE PREFIXED `read…` for the reason the applier's are prefixed
// `apply…`: this package's suites are several files written against one test
// package, and a bare `harness` here would be a collision rather than a shared
// fixture.

// readBrokerAt is the broker's own instant for the first record; every record
// after it is stamped a second later, so a rail ordered by activity has an
// order to be right or wrong about.
var readBrokerAt = time.Date(2031, 5, 6, 7, 8, 0, 0, time.UTC)

// readHarness is one node's estate, its applier and the reader over them.
type readHarness struct {
	t            *testing.T
	db           store.ReplicatedHandle
	applier      *chatlet.Applier
	waiter       *readWaiter
	gen          uint32
	seq          uint64
	maxVariables int

	// stamped counts every record the broker stamped, across reanchors: a
	// rebuilt estate starts its sequence again, and the broker's clock does
	// not.
	stamped int64
}

func newReadHarness(t *testing.T) *readHarness {
	t.Helper()
	node, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"),
		store.Options{}, 1)
	t.Cleanup(func() {
		if err := node.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	return &readHarness{
		t: t, db: db, applier: chatlet.NewApplier("node-a", nil),
		waiter: &readWaiter{}, gen: 1,
		maxVariables: db.Caps().MaxVariables,
	}
}

// storedAt is the broker's instant for the record last applied, a second
// after the one before it whatever generation either is in.
func (h *readHarness) storedAt() time.Time {
	return readBrokerAt.Add(time.Duration(h.stamped) * time.Second)
}

// apply runs one record at the next position and commits it, exactly as the
// framework's replication loop does — and advances the waiter afterwards, so a
// read asking for this position finds it.
func (h *readHarness) apply(rec chatlet.MutationRecord) statelog.Position {
	h.t.Helper()
	h.seq++
	h.stamped++
	rec.Gen = h.gen
	rec.Writer = "node-a"
	body, err := chatlet.Encode(rec)
	if err != nil {
		h.t.Fatalf("encode the record on %s: %v", rec.Subject, err)
	}
	at := statelog.Position{
		Stream: topics.ChatLogStream, Generation: h.gen, Seq: h.seq,
	}
	storedAt := h.storedAt()
	record := statelog.Record{
		Envelope: statelog.Envelope{
			V: rec.V, Kind: string(rec.Subject.Kind),
			Subject: statelog.Subject{
				Kind: string(rec.Subject.Kind), ID: rec.Subject.ID,
			},
			Op: string(rec.Op), OpID: rec.OpID, Gen: rec.Gen,
			Writer: rec.Writer, Scope: rec.Scope.Resolve(rec.Subject),
		},
		Position: at, Payload: body, StoredAt: storedAt,
	}
	if err := h.db.Tx(h.t.Context(), func(tx *sql.Tx) error {
		reason, gated, err := h.applier.Gated(h.t.Context(), tx, record)
		if err != nil {
			return err
		}
		if gated {
			return fmt.Errorf("the record on %s was gated: %s", rec.Subject, reason)
		}
		_, err = h.applier.Apply(h.t.Context(), tx, record, statelog.ApplyOptions{
			Now: storedAt, StoredAt: storedAt, MaxVariables: h.maxVariables,
		})
		return err
	}); err != nil {
		h.t.Fatalf("apply the record on %s at %s: %v", rec.Subject, at, err)
	}
	h.applier.Committed(h.t.Context())
	h.waiter.reach(at)
	return at
}

// reanchor is an operator rebuilding the broker estate: the generation moves
// and the sequence starts again at one, which is the only thing that makes a
// bare sequence and a composed position tell different stories.
func (h *readHarness) reanchor() {
	h.gen++
	h.seq = 0
}

// reader is the read authority over this node, with a waiter that MOVES.
//
// [statelogtest.LocalReader]'s waiter is already at its position and never
// blocks, which cannot tell "served from before the caller's floor" from
// "waited and reached it" — and that difference is the whole of what a floor
// is. This one blocks until [readHarness.apply] has got there.
func (h *readHarness) reader() *chatlet.Reader {
	h.t.Helper()
	authority, err := statelogtest.LocalReaderOver(chatlet.Domain{},
		h.db, h.waiter)
	if err != nil {
		h.t.Fatalf("build the read authority: %v", err)
	}
	reader, err := chatlet.NewReader(chatlet.ReaderOptions{
		Log: authority, Committed: h.waiter.Committed,
	})
	if err != nil {
		h.t.Fatalf("build the reader: %v", err)
	}
	return reader
}

// readerBehind is the same node reporting itself `lag` records behind the
// log's end, which is the only way a staleness bound is observable at all.
func (h *readHarness) readerBehind(lag uint64) *chatlet.Reader {
	h.t.Helper()
	authority, err := statelogtest.LocalReaderBehind(chatlet.Domain{},
		h.db, h.waiter.Committed(), lag)
	if err != nil {
		h.t.Fatalf("build the read authority: %v", err)
	}
	reader, err := chatlet.NewReader(chatlet.ReaderOptions{
		Log: authority, Committed: h.waiter.Committed,
	})
	if err != nil {
		h.t.Fatalf("build the reader: %v", err)
	}
	return reader
}

// readWaiter is this node's applier as a read sees it.
type readWaiter struct {
	mu sync.Mutex
	at statelog.Position
}

func (w *readWaiter) reach(p statelog.Position) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if p.Packed() > w.at.Packed() {
		w.at = p
	}
}

func (w *readWaiter) Committed() statelog.Position {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.at
}

func (w *readWaiter) WaitCommitted(ctx context.Context, p statelog.Position) error {
	for {
		if w.Committed().Packed() >= p.Packed() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func (w *readWaiter) WaitApplied(ctx context.Context, _ statelog.ScopeSet,
	p statelog.Position) error {

	return w.WaitCommitted(ctx, p)
}

// readRecord builds one record with its payload encoded.
func readRecord(subject chatlet.Subject, op chatlet.OpKind, opID string, payload any,
	scope chatlet.ScopeSet, actor string) chatlet.MutationRecord {

	rec := chatlet.MutationRecord{
		RecordEnvelope: chatlet.RecordEnvelope{
			V: chatlet.RecordVersion, OpID: opID, Subject: subject, Op: op,
			CreatedAt: readBrokerAt, Scope: scope,
		},
		Actor: actor, ActorKind: chatlet.AuthorHuman,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	rec.Mutation = body
	return rec
}

// readCreateRoom builds the create of one NAMED room — public, private or a
// unit's own — with its founding membership.
func readCreateRoom(channelID, name string, kind chatlet.Kind,
	members ...string) chatlet.MutationRecord {

	founding := make([]chatlet.Member, 0, len(members))
	for _, handle := range members {
		founding = append(founding, chatlet.Member{Handle: handle})
	}
	return readRecord(chatlet.ChannelNameSubject(name), chatlet.OpCreate,
		"op-create-"+channelID, chatlet.ChannelCreate{
			V: chatlet.DocumentVersion, ChannelID: channelID, Kind: kind,
			Name: name, Members: founding,
			CreatedBy: "ada", CreatedByKind: chatlet.AuthorHuman,
		}, chatlet.ScopeSet{Terms: []chatlet.ScopeTerm{
			{Kind: chatlet.TermName, ID: chatlet.ChannelToken(name)},
			{Kind: chatlet.TermChannel, ID: channelID},
		}}, "ada")
}

// readPost builds one message into a room.
func readPost(channelID, messageID, author, body string,
	mentions ...string) chatlet.MutationRecord {

	return readRecord(chatlet.MessageSubject(channelID), chatlet.OpPost,
		"op-post-"+messageID, chatlet.MessagePost{
			V: chatlet.DocumentVersion, MessageID: messageID, Body: body,
			Author: author, AuthorKind: chatlet.AuthorHuman, Mentions: mentions,
		}, chatlet.ScopeSet{Subject: true}, author)
}

// said is a page's messages without its room events: what people said, which
// is what a case about messages counts. A named room's create writes a
// `joined` line, and every room here is made by one.
func said(views []chatlet.MessageView) []chatlet.MessageView {
	out := make([]chatlet.MessageView, 0, len(views))
	for _, view := range views {
		if view.Message.Event == nil {
			out = append(out, view)
		}
	}
	return out
}

// session is a read that names this node's own applied position as its floor,
// which is what every case here wants: the rows it has just applied.
func (h *readHarness) session() statelog.Freshness {
	return statelog.Freshness{
		Level: statelog.ReadSession, MinPosition: h.waiter.Committed(),
	}
}

// A PRIVATE ROOM IS NOT FOUND TO SOMEBODY WHO IS NOT IN IT.
//
// Not forbidden, and the difference is the whole point: a private room's
// EXISTENCE is information — who is talking to whom is most of what a
// transcript discloses — so a refusal that distinguished "no such room" from
// "not yours" would answer that question for anybody willing to guess an id.
//
// The case holds THREE readers to one rule at once: [chatlet.Visible] over every
// kind, the point reads that call it, and the mention feed, whose SQL narrows
// by the same predicate so that its LIMIT counts only rows the reader may see.
// The last is the one that fails silently if the two ever disagree — a feed
// that filtered in Go alone would return a page of fifty and render three.
func TestAPrivateRoomIsNotFoundToANonMember(t *testing.T) {
	t.Parallel()

	// THE RULE ITSELF, over every kind this build serves plus one it does
	// not. Without the unknown kind the closed set is a claim: a newer
	// peer's room would otherwise read as public, because "not private" is
	// what an unrecognised kind looks like from the other direction.
	for _, c := range []struct {
		kind   chatlet.Kind
		member bool
		want   bool
	}{
		{chatlet.KindPublic, false, true},
		{chatlet.KindPublic, true, true},
		{chatlet.KindUnit, false, true},
		{chatlet.KindPrivate, false, false},
		{chatlet.KindPrivate, true, true},
		{chatlet.KindDM, false, false},
		{chatlet.KindDM, true, true},
		{chatlet.KindGroup, false, false},
		{chatlet.KindGroup, true, true},
		{chatlet.Kind("broadcast"), true, false},
	} {
		if got := chatlet.Visible("ada", chatlet.Channel{Kind: c.kind}, c.member); got != c.want {
			t.Errorf("Visible(ada, %s room, member=%t) = %t, want %t",
				c.kind, c.member, got, c.want)
		}
	}
	// AND NOBODY IS NOT A READER. Identity is resolved above this package,
	// so an empty handle is an unbound token that slipped through — and the
	// one answer that is never right for one is yes.
	if chatlet.Visible("", chatlet.Channel{Kind: chatlet.KindPublic}, true) {
		t.Error("an empty viewer read a public room — an unbound token gets " +
			"nothing in chat, reads included")
	}

	h := newReadHarness(t)
	h.apply(readCreateRoom("room-open", "open", chatlet.KindPublic, "ada", "bob"))
	h.apply(readCreateRoom("room-shut", "shut", chatlet.KindPrivate, "bob"))
	// ONE MESSAGE IN EACH, BOTH NAMING ADA. The mention in the private room
	// is the row the feed must not show her: being named in a room you are
	// not in is ordinary, it wakes nobody, and the message is still a row.
	h.apply(readPost("room-open", "msg-open", "bob", "morning @ada", "ada"))
	h.apply(readPost("room-shut", "msg-shut", "bob", "about @ada", "ada"))
	reader := h.reader()

	_, err := reader.Channel(t.Context(), "ada", "room-shut", h.session())
	if !errors.Is(err, chatlet.ErrNotFound) {
		t.Errorf("reading a private room ada is not in answered %v, want %v — "+
			"a room she may not read must be indistinguishable from one that "+
			"is not there", err, chatlet.ErrNotFound)
	}
	if errors.Is(err, chatlet.ErrForbidden) {
		t.Error("reading a private room answered `forbidden`, which tells " +
			"anybody who guesses an id that the room exists")
	}
	if _, err := reader.Messages(t.Context(), "ada",
		chatlet.TranscriptQuery{ChannelID: "room-shut"}, h.session()); !errors.Is(
		err, chatlet.ErrNotFound) {

		t.Errorf("reading a private room's transcript answered %v, want %v",
			err, chatlet.ErrNotFound)
	}

	// THE CONTROL, and without it every assertion above is satisfied by a
	// reader that finds nothing at all: bob is in the room and reads it.
	detail, err := reader.Channel(t.Context(), "bob", "room-shut", h.session())
	if err != nil {
		t.Fatalf("bob reading the room he is in: %v", err)
	}
	if !detail.Member || detail.Channel.Kind != chatlet.KindPrivate {
		t.Errorf("bob's read of room-shut reports member=%t kind=%s, want a "+
			"member of a private room", detail.Member, detail.Channel.Kind)
	}
	// AND A PUBLIC ROOM REFUSES NOBODY: the engine is the boundary, so a
	// seat of this company reads its open rooms without being in them.
	if _, err := reader.Channel(t.Context(), "carol", "room-open",
		h.session()); err != nil {

		t.Errorf("carol reading a public room she is not in: %v — a public "+
			"room is readable by any seat of the company", err)
	}

	feed := mentionsOf(t, reader, "ada", "", 0, h.session())
	if len(feed.Items) != 1 || feed.Items[0].MessageID != "msg-open" {
		t.Fatalf("ada's mentions are %v, want the public room's mention "+
			"alone: a mention in a room she cannot open discloses the room",
			mentionIDs(feed))
	}
}

// A ROOM'S NAME IS ITS ADDRESS TO ANYBODY WHO MAY READ IT, member or not —
// and to nobody else.
//
// A name was once resolved only against the rooms a caller was IN, so a unit's
// room or a public room nobody here had joined was readable and writable by id
// while its name meant nothing, and nothing a seat could call handed it the id.
// The name is the room's address — unique, claimed once — so it resolves for
// whoever [chatlet.Visible] admits, in any spelling a person writes it in; a
// private room's name answers exactly as a name nobody holds, so the address
// discloses no more than the id does.
func TestARoomIsAddressedByItsNameByAnybodyWhoMayReadIt(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	h.apply(readCreateRoom("room-open", "launch", chatlet.KindPublic, "ada"))
	h.apply(readCreateRoom("room-unit", "eng", chatlet.KindUnit, "ada"))
	h.apply(readCreateRoom("room-shut", "board", chatlet.KindPrivate, "bob"))
	reader := h.reader()

	for name, want := range map[string]string{
		"launch": "room-open", "  Launch ": "room-open", "eng": "room-unit",
	} {
		got, err := reader.ChannelNamed(t.Context(), "carol", name, h.session())
		if err != nil {
			t.Errorf("carol resolving %q, a room she may read and is not in: %v",
				name, err)
			continue
		}
		if got.Channel.ID != want || got.Member {
			t.Errorf("carol resolved %q to %s (member=%t), want %s as an outsider",
				name, got.Channel.ID, got.Member, want)
		}
	}

	_, hidden := reader.ChannelNamed(t.Context(), "carol", "board", h.session())
	_, absent := reader.ChannelNamed(t.Context(), "carol", "nobody-holds-this", h.session())
	if !errors.Is(hidden, chatlet.ErrNotFound) || !errors.Is(absent, chatlet.ErrNotFound) {
		t.Fatalf("a private room's name answered %v and a name nobody holds %v, "+
			"want both not found", hidden, absent)
	}
	if hidden.Error() != strings.Replace(absent.Error(), "nobody-holds-this", "board", 1) {
		t.Errorf("a private room's name answers %q where a name nobody holds "+
			"answers %q — the difference says the room exists", hidden, absent)
	}

	// THE CONTROL: the member reads the private room by its name.
	got, err := reader.ChannelNamed(t.Context(), "bob", "board", h.session())
	if err != nil || got.Channel.ID != "room-shut" || !got.Member {
		t.Fatalf("bob resolving the private room he is in answered %+v, %v", got, err)
	}
	if _, err := reader.ChannelNamed(t.Context(), "bob", " ", h.session()); !errors.Is(
		err, chatlet.ErrInvalid) {

		t.Errorf("a read by an empty name answered %v, want invalid", err)
	}
}

// readEdit builds one rewrite of a message, naming `mentions` afresh.
func readEdit(channelID, messageID, opID, body string,
	mentions ...string) chatlet.MutationRecord {

	return readRecord(chatlet.MessageSubject(channelID), chatlet.OpEdit, opID,
		chatlet.MessageEdit{
			V: chatlet.DocumentVersion, MessageID: messageID, Body: body,
			Mentions: mentions, EditedBy: "bob", EditedByKind: chatlet.AuthorHuman,
		}, chatlet.ScopeSet{Subject: true}, "bob")
}

// AN EDIT LEAVES A MENTION WHERE IT WAS.
//
// The feed pages on a mention row's version, so that version is the
// mention's place in it — and it is the position of the record that NAMED
// the handle. An edit that cleared a message's rows and wrote them again
// stamped every one at the edit, so each mention it kept jumped to the top of
// the feed under the message's original time, above every cursor a reader
// paging back already held: a pager of the feed never saw it again, and a
// reader at the top saw it again as news. What an edit owes the feed is only
// its own difference — a handle it no longer names leaves it, a handle it
// names that the words before it did not arrives at the edit, dated by it —
// and the position reported for a kept mention has to stay whole across a
// reanchor, because the message's own position columns move to the edit's
// generation while the mention does not.
func TestAnEditLeavesAMentionWhereItWas(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	h.apply(readCreateRoom("room-1", "eng", chatlet.KindPublic, "ada", "bob"))
	h.apply(readPost("room-1", "msg-1", "bob", "@ada @carol look",
		"ada", "carol"))
	postedAt := h.storedAt()
	h.apply(readPost("room-1", "msg-2", "bob", "and this @ada", "ada"))
	h.apply(readEdit("room-1", "msg-1", "op-edit-1",
		"@ada @dan look again", "ada", "dan"))
	editedAt := h.storedAt()
	reader := h.reader()

	feedOf := func(who string) chatlet.Activity {
		t.Helper()
		return mentionsOf(t, reader, who, "", 0, h.session())
	}
	ada := feedOf("ada")
	if got, want := mentionIDs(ada), []string{"msg-2", "msg-1"}; !slices.Equal(got, want) {
		t.Fatalf("ada's feed after an edit that still names her reads %v, "+
			"want %v — the edit moved her mention to the top", got, want)
	}
	if got := ada.Items[1].Excerpt; got != "@ada @dan look again" {
		t.Errorf("ada's kept mention reads %q, want the edited words", got)
	}
	// DATED WHERE IT SITS: when she was named, which the edit did not
	// change.
	if got := ada.Items[1].At; !got.Equal(postedAt) {
		t.Errorf("ada's kept mention is dated %s, want %s when the post named "+
			"her", got, postedAt)
	}
	if got := mentionIDs(feedOf("carol")); len(got) != 0 {
		t.Errorf("carol's feed reads %v after an edit that no longer names "+
			"her, want nothing", got)
	}
	dan := feedOf("dan")
	if got := mentionIDs(dan); !slices.Equal(got, []string{"msg-1"}) {
		t.Fatalf("dan's feed reads %v, want msg-1: the edit is the record "+
			"that named him", got)
	}
	if got := dan.Items[0].At; !got.Equal(editedAt) {
		t.Errorf("dan's mention is dated %s, want %s: he was named by the "+
			"edit, and a row at the top of the feed dated when the words were "+
			"first said reads as a feed out of order", got, editedAt)
	}

	// A REANCHOR, AND THE MESSAGE EDITED AGAIN IN THE NEW GENERATION: its
	// own position columns now name generation two, and ada's mention still
	// sits where the post named her.
	h.reanchor()
	h.apply(readEdit("room-1", "msg-1", "op-edit-2", "@ada once more", "ada"))
	ada = feedOf("ada")
	if got, want := mentionIDs(ada), []string{"msg-2", "msg-1"}; !slices.Equal(got, want) {
		t.Fatalf("ada's feed after a second edit reads %v, want %v", got, want)
	}
	if got := ada.Items[1].At; !got.Equal(postedAt) {
		t.Errorf("ada's kept mention is dated %s after an edit in the next "+
			"generation, want %s when the post named her", got, postedAt)
	}

	// AN EDIT THAT NAMES NOBODY CLEARS EVERY ROW, rather than reading an
	// empty set as a list nothing is outside of.
	h.apply(readEdit("room-1", "msg-1", "op-edit-3", "nobody now"))
	if got := mentionIDs(feedOf("ada")); !slices.Equal(got, []string{"msg-2"}) {
		t.Errorf("ada's feed after an edit naming nobody reads %v, want msg-2 "+
			"alone", got)
	}

	// AND NAMING HER AGAIN IS A NAMING: the words before this edit did not
	// name her, so it is the record that did, and she finds it at the top.
	h.apply(readEdit("room-1", "msg-1", "op-edit-4", "@ada back", "ada"))
	renamedAt := h.storedAt()
	ada = feedOf("ada")
	if got, want := mentionIDs(ada), []string{"msg-1", "msg-2"}; !slices.Equal(got, want) {
		t.Fatalf("ada's feed after an edit named her again reads %v, want %v",
			got, want)
	}
	if got := ada.Items[0].At; !got.Equal(renamedAt) {
		t.Errorf("ada's renewed mention is dated %s, want %s", got, renamedAt)
	}
}

// A DELETE TAKES THE MENTION AWAY rather than leaving a tombstone in the feed.
//
// The room keeps a tombstone, because a thread hangs off the message; the
// feed is a list of the places a person was named, and a message taken back
// names nobody, asks nothing and has no words left to show. Every other
// mention stays where it was.
func TestADeleteTakesTheMentionOutOfTheFeed(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	h.apply(readCreateRoom("room-1", "eng", chatlet.KindPublic, "ada", "bob"))
	h.apply(readPost("room-1", "msg-1", "bob", "first @ada", "ada"))
	h.apply(readPost("room-1", "msg-2", "bob", "second @ada", "ada"))
	h.apply(readPost("room-1", "msg-3", "bob", "third @ada", "ada"))
	h.apply(readRecord(chatlet.MessageSubject("room-1"), chatlet.OpDelete,
		"op-delete-3", chatlet.MessageDelete{
			V: chatlet.DocumentVersion, MessageID: "msg-3", DeletedBy: "bob",
			DeletedByKind: chatlet.AuthorHuman,
		}, chatlet.ScopeSet{Subject: true}, "bob"))

	feed := mentionsOf(t, h.reader(), "ada", "", 0, h.session())
	if got, want := mentionIDs(feed), []string{"msg-2", "msg-1"}; !slices.Equal(got, want) {
		t.Fatalf("ada's feed after a delete reads %v, want %v: a message "+
			"taken back still names her", got, want)
	}
}

// A CURSOR SPANS A REANCHOR, with no gap and no repeat.
//
// An operator who loses the broker estate rebuilds it, and the new stream
// starts at one. Every cursor this package hands out has to survive that, and
// the two of them survive it for DIFFERENT reasons — which is why both halves
// are here:
//
//   - THE MENTIONS in Activity page on the COMPOSED position, where the
//     generation is IN the ordering. A bare sequence would page the second half of this case
//     against `seq < 1` and return nothing, losing every message written
//     before the rebuild.
//   - THE TRANSCRIPT pages on `channel_seq`, which the applier mints from log
//     order and never moves. It is contiguous across the rebuild because the
//     room's high-water mark is a row rather than a position.
func TestACursorSpansAReanchor(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	h.apply(readCreateRoom("room-1", "eng", chatlet.KindPublic, "ada", "bob"))
	h.apply(readPost("room-1", "msg-1", "bob", "one @ada", "ada"))
	h.apply(readPost("room-1", "msg-2", "bob", "two @ada", "ada"))
	// THE ESTATE IS REBUILT HERE. Everything below is generation two,
	// sequence one upwards — numbers that are BELOW the ones above it in
	// every space but the composed one.
	h.reanchor()
	h.apply(readPost("room-1", "msg-3", "bob", "three @ada", "ada"))
	h.apply(readPost("room-1", "msg-4", "bob", "four @ada", "ada"))
	reader := h.reader()

	var seen []string
	cursor := ""
	for page := 0; page < 4; page++ {
		got, err := reader.Messages(t.Context(), "ada", chatlet.TranscriptQuery{
			ChannelID: "room-1", Cursor: cursor, Limit: 2,
		}, h.session())
		if err != nil {
			t.Fatalf("transcript page %d: %v", page, err)
		}
		for _, view := range said(got.Messages) {
			seen = append(seen, view.Message.ID)
		}
		if cursor = got.NextCursor; cursor == "" {
			break
		}
	}
	if want := []string{"msg-4", "msg-3", "msg-2", "msg-1"}; !slices.Equal(seen, want) {
		t.Errorf("paging the transcript two at a time across a reanchor read "+
			"%v, want %v — a cursor that cannot span the rebuild either "+
			"repeats a message or loses one", seen, want)
	}

	seen = seen[:0]
	cursor = ""
	for page := 0; page < 4; page++ {
		got := mentionsOf(t, reader, "ada", cursor, 2, h.session())
		seen = append(seen, mentionIDs(got)...)
		if cursor = got.NextCursor; cursor == "" {
			break
		}
	}
	if want := []string{"msg-4", "msg-3", "msg-2", "msg-1"}; !slices.Equal(seen, want) {
		t.Errorf("paging the mention feed two at a time across a reanchor "+
			"read %v, want %v — the generation has to be IN the ordering, "+
			"which is what the composed position is for", seen, want)
	}
}

// AN UNREAD COUNT STOPS AT A HUNDRED.
//
// A badge is a number somebody glances at: the difference between 100 and
// 4,000 changes nothing anybody does, and counting to 4,000 is a scan of a
// room's whole transcript on every poll of a rail with one row per room. So
// the count is a bounded range that says it stopped — and the three other
// assertions here are what stop the bound from being the only thing that is
// true: a reader who is caught up sees zero, their OWN message is not
// something they have not read, and the next message from somebody else is.
func TestAnUnreadCountStopsAtAHundred(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	h.apply(readCreateRoom("room-1", "eng", chatlet.KindPublic, "ada", "bob"))
	var last statelog.Position
	for i := 0; i < chatlet.UnreadLimit+20; i++ {
		last = h.apply(readPost("room-1", fmt.Sprintf("msg-%03d", i), "bob",
			fmt.Sprintf("message %d", i)))
	}
	reader := h.reader()

	// A ROOM ADA HAS NEVER OPENED: no cursor at all, a hundred and twenty
	// messages in it, and an answer of a hundred that says it is a floor.
	fresh := h.readRail(reader, nil)
	if fresh.Unread != chatlet.UnreadLimit || !fresh.UnreadCapped {
		t.Errorf("a room with %d unread messages reports %d (capped=%t), want "+
			"%d and a capped flag — the number is what a client renders as "+
			"\"99+\"", chatlet.UnreadLimit+20, fresh.Unread, fresh.UnreadCapped,
			chatlet.UnreadLimit)
	}
	if fresh.Last == nil || fresh.Last.MessageID != fmt.Sprintf("msg-%03d",
		chatlet.UnreadLimit+19) {

		t.Fatalf("the rail's preview is %v, want the newest message",
			fresh.Last)
	}

	// CAUGHT UP. Without this the cap alone would be satisfied by a count
	// that always answers a hundred.
	caught := h.readRail(reader, map[string]int64{"room-1": last.Packed()})
	if caught.Unread != 0 || caught.UnreadCapped {
		t.Errorf("a room ada has read to the end of reports %d unread "+
			"(capped=%t), want 0", caught.Unread, caught.UnreadCapped)
	}

	// HER OWN MESSAGE IS NOT ONE SHE HAS NOT READ. It is above her cursor,
	// so a count that merely ranged on the position would raise a badge for
	// something she typed — which is a badge reading the room cannot clear.
	h.apply(readPost("room-1", "msg-mine", "ada", "and one from me"))
	mine := h.readRail(reader, map[string]int64{"room-1": last.Packed()})
	if mine.Unread != 0 {
		t.Errorf("ada's own message counted %d unread, want 0", mine.Unread)
	}
	if mine.Last == nil || mine.Last.MessageID != "msg-mine" {
		t.Errorf("the preview is %v, want her own message — excluded from the "+
			"COUNT and still the last thing said", mine.Last)
	}

	// AND SOMEBODY ELSE'S IS. The control for the exclusion above: without
	// it, "her own does not count" is satisfied by a count stuck at zero.
	h.apply(readPost("room-1", "msg-theirs", "bob", "and one more"))
	theirs := h.readRail(reader, map[string]int64{"room-1": last.Packed()})
	if theirs.Unread != 1 || theirs.UnreadCapped {
		t.Errorf("one new message from bob counted %d unread (capped=%t), "+
			"want 1", theirs.Unread, theirs.UnreadCapped)
	}
}

// A READ NAMES ITS OWN WRITE POSITION AS A FLOOR, and is served nothing from
// before it.
//
// This is read-your-own-writes, and it is the FRAMEWORK'S floor rather than a
// level of chat's: a write returns the position its record landed at, a wake
// carries one, and a caller holding either names it as `min_position`. A node
// that has not reached it REFUSES `behind` — which is the half that matters,
// because the alternative is a screen redrawing after its own post and
// rendering the room without it.
func TestAReadNamesItsOwnWritePositionAsAFloor(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	h.apply(readCreateRoom("room-1", "eng", chatlet.KindPublic, "ada"))
	at := h.apply(readPost("room-1", "msg-1", "ada", "just said this"))
	reader := h.reader()

	got, err := reader.Messages(t.Context(), "ada",
		chatlet.TranscriptQuery{ChannelID: "room-1"},
		statelog.Freshness{Level: statelog.ReadSession, MinPosition: at})
	if err != nil {
		t.Fatalf("a read floored at its own write: %v", err)
	}
	if msgs := said(got.Messages); len(msgs) != 1 || msgs[0].Message.ID != "msg-1" {
		t.Fatalf("a read floored at its own write answered %d messages, want "+
			"the one it just wrote", len(msgs))
	}
	if got.Position.Packed() < at.Packed() {
		t.Errorf("the answer reports position %s, which is before the floor "+
			"%s it was served under", got.Position, at)
	}

	// AND A FLOOR THIS NODE HAS NOT REACHED REFUSES. Without this the case
	// is satisfied by a reader that ignores the floor entirely: at a
	// position it already holds, honouring one and dropping one are the
	// same answer.
	ahead := at.At(at.Seq + 1)
	short, err := reader.Messages(t.Context(), "ada",
		chatlet.TranscriptQuery{ChannelID: "room-1"},
		statelog.Freshness{Level: statelog.ReadSession, MinPosition: ahead})
	var refused *statelog.Refused
	if !errors.As(err, &refused) || refused.Code != statelog.RefuseBehind {
		t.Fatalf("a read floored past this node answered (%d messages, %v), "+
			"want a `behind` refusal — rows from before a caller's own write "+
			"are the one answer it cannot use", len(short.Messages), err)
	}
	if len(short.Messages) != 0 {
		t.Errorf("a refused read still carried %d messages", len(short.Messages))
	}
}

// A NODE THAT IS BEHIND SAYS SO, rather than answering with what it happens to
// hold.
//
// Two halves, and neither is sufficient alone. A read that declares a
// staleness bound is REFUSED past it — `too_stale`, which names a different
// thing to do from every other refusal, because waiting on this node is
// exactly what will not help. A read that declares none is SERVED and carries
// the lag on the answer, so a surface renders the distance rather than
// implying there is none.
func TestABehindNodeSaysSoRatherThanAnsweringEmpty(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	h.apply(readCreateRoom("room-1", "eng", chatlet.KindPublic, "ada"))
	h.apply(readPost("room-1", "msg-1", "ada", "said before the node fell back"))
	reader := h.readerBehind(500)

	_, err := reader.Messages(t.Context(), "ada",
		chatlet.TranscriptQuery{ChannelID: "room-1"},
		statelog.Freshness{Level: statelog.ReadStale, MaxLagSeq: 10})
	var refused *statelog.Refused
	if !errors.As(err, &refused) || refused.Code != statelog.RefuseTooStale {
		t.Fatalf("a node 500 records behind served a read that accepts 10: "+
			"%v — a bound nothing enforces is a label on an answer of any age",
			err)
	}

	got, err := reader.Messages(t.Context(), "ada",
		chatlet.TranscriptQuery{ChannelID: "room-1"},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("an unbounded stale read on a behind node: %v — a node that "+
			"is behind is still a node that can answer", err)
	}
	if msgs := said(got.Messages); len(msgs) != 1 {
		t.Fatalf("an unbounded stale read answered %d messages, want the one "+
			"this node has", len(msgs))
	}
	if got.LogLag == nil || *got.LogLag != 500 {
		t.Errorf("the answer reports lag %v, want 500 — an answer that does "+
			"not say how far behind it is reads as one that is not",
			got.LogLag)
	}
	if got.Level != statelog.ReadStale {
		t.Errorf("the answer was served at %q, want %q — a level is what the "+
			"read got, never what it asked for", got.Level, statelog.ReadStale)
	}
}

// readRail is one room's summary off this person's channel list.
func (h *readHarness) readRail(reader *chatlet.Reader,
	cursors map[string]int64) chatlet.ChannelSummary {

	h.t.Helper()
	listing, err := reader.Channels(h.t.Context(), "ada",
		chatlet.ChannelsQuery{Cursors: cursors}, h.session())
	if err != nil {
		h.t.Fatalf("ada's channel list: %v", err)
	}
	if len(listing.Channels) != 1 {
		h.t.Fatalf("ada's channel list holds %d rooms, want the one she is in",
			len(listing.Channels))
	}
	return listing.Channels[0]
}

// mentionsOf is a page of who's mentions, as Activity's Mentions reads them.
func mentionsOf(t *testing.T, reader *chatlet.Reader, who, cursor string, limit int,
	fresh statelog.Freshness) chatlet.Activity {
	t.Helper()
	feed, err := reader.Activity(t.Context(), who, chatlet.ActivityQuery{
		Kind: chatlet.ActivityMention, Cursor: cursor, Limit: limit,
	}, fresh)
	if err != nil {
		t.Fatalf("%s's mentions: %v", who, err)
	}
	return feed
}

// mentionIDs is a page of mentions as the messages it names.
func mentionIDs(feed chatlet.Activity) []string {
	out := make([]string, 0, len(feed.Items))
	for _, item := range feed.Items {
		out = append(out, item.MessageID)
	}
	return out
}

// A PAGE OF LONG MESSAGES ENDS WHERE ONE REPLY CAN CARRY IT, and the cursor
// carries on from there.
//
// A page is ONE REPLY when a node that holds no data reads it through the
// estate router, and a reply past [queue.MaxPayloadBytes] is refused whole.
// The count was the only bound, and a message's document runs to most of a
// record, so a page at [chatlet.MaxLimit] of long messages was an error rather
// than a page — a room nobody could scroll from such a node. Now a page stops
// at half the payload of documents, never below one message, and paging on
// reads every message once, in order.
func TestAPageOfLongMessagesFitsOneReplyAndPagesOn(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	h.apply(readCreateRoom("room-1", "essays", chatlet.KindPublic, "ada"))
	const posted = 300
	long := strings.Repeat("x", chatlet.MaxBody)
	for i := 1; i <= posted; i++ {
		h.apply(readPost("room-1", fmt.Sprintf("msg-%03d", i), "ada", long))
	}
	reader := h.reader()

	var seen []string
	cursor := ""
	pages := 0
	for {
		got, err := reader.Messages(t.Context(), "ada", chatlet.TranscriptQuery{
			ChannelID: "room-1", Cursor: cursor, Limit: chatlet.MaxLimit,
		}, h.session())
		if err != nil {
			t.Fatalf("transcript page %d: %v", pages, err)
		}
		pages++
		if len(got.Messages) == 0 {
			t.Fatalf("page %d held nothing with cursor %q — a page must hold at "+
				"least one message or its cursor never moves", pages, cursor)
		}
		wire, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("encode page %d: %v", pages, err)
		}
		if len(wire) > queue.MaxPayloadBytes/2+chatlet.MaxRecordBytes {
			t.Errorf("page %d encodes to %d bytes with %d messages — past what "+
				"half a reply carries, so the router's answer is refused once "+
				"the rest of the reply rides with it", pages, len(wire),
				len(got.Messages))
		}
		for _, view := range said(got.Messages) {
			seen = append(seen, view.Message.ID)
		}
		if cursor = got.NextCursor; cursor == "" {
			break
		}
		if pages > posted {
			t.Fatal("the cursor never ran out")
		}
	}
	if pages < 2 {
		t.Errorf("%d long messages came back in %d page — the byte bound never "+
			"bit, so this case measured the count alone", posted, pages)
	}
	if len(seen) != posted {
		t.Fatalf("paging read %d messages, want all %d once each", len(seen), posted)
	}
	for i, id := range seen {
		if want := fmt.Sprintf("msg-%03d", posted-i); id != want {
			t.Fatalf("message %d of the walk is %s, want %s — the byte bound "+
				"repeated or skipped one where it cut", i, id, want)
		}
	}
}

// A RAIL ASKED FOR FEWER ROOMS IS THE TOP OF THE ONE SORTED LIST, never a page:
// the most recently active rooms, in the order the whole rail draws them, and
// said to leave the rest out; and one asked for the rooms with something unread
// keeps only those, counted against the reader's own cursors. Mutation: cut the
// list before it is sorted (by id, as the membership index orders it), cut it
// without saying so, or keep a room the reader has read, and a case here fails.
func TestARailAskedForFewerRoomsIsTheTopOfIt(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	// ROOM IDS IN THE OPPOSITE ORDER TO THEIR ACTIVITY, so a cut taken in the
	// membership index's order (by id) takes the least active.
	for _, room := range []string{"room-c", "room-b", "room-a"} {
		h.apply(readCreateRoom(room, strings.TrimPrefix(room, "room-"),
			chatlet.KindPublic, "ada", "bob"))
	}
	var read statelog.Position
	for _, room := range []string{"room-c", "room-b", "room-a"} {
		read = h.apply(readPost(room, "msg-"+room, "bob", "news in "+room))
	}
	h.apply(readPost("room-a", "msg-room-a-2", "bob", "more news"))
	reader := h.reader()
	ask := func(q chatlet.ChannelsQuery) chatlet.ChannelListing {
		t.Helper()
		listing, err := reader.Channels(t.Context(), "ada", q, h.session())
		if err != nil {
			t.Fatalf("ada's channel list: %v", err)
		}
		return listing
	}
	ids := func(l chatlet.ChannelListing) []string {
		out := make([]string, 0, len(l.Channels))
		for _, c := range l.Channels {
			out = append(out, c.Channel.ID)
		}
		return out
	}
	whole := ask(chatlet.ChannelsQuery{})
	if !slices.Equal(ids(whole), []string{"room-a", "room-b", "room-c"}) || whole.Truncated {
		t.Fatalf("ada's whole rail is %v (truncated=%t), want the three, most recently "+
			"active first", ids(whole), whole.Truncated)
	}
	top := ask(chatlet.ChannelsQuery{Limit: 2})
	if !slices.Equal(ids(top), []string{"room-a", "room-b"}) || !top.Truncated {
		t.Errorf("ada's top two rooms are %v (truncated=%t), want the two most recently "+
			"active and the rest said to be left out", ids(top), top.Truncated)
	}
	// ADA HAS READ room-b AND room-c TO THE END, and not room-a's last message.
	unread := ask(chatlet.ChannelsQuery{Unread: true, Cursors: map[string]int64{
		"room-a": read.Packed(), "room-b": read.Packed(), "room-c": read.Packed()}})
	if !slices.Equal(ids(unread), []string{"room-a"}) || unread.Truncated {
		t.Errorf("ada's rooms with something unread are %v (truncated=%t), want room-a "+
			"alone", ids(unread), unread.Truncated)
	}
}
