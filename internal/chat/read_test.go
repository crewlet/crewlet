package chat_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
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
	db           *store.DB
	applier      *chat.Applier
	waiter       *readWaiter
	gen          uint32
	seq          uint64
	maxVariables int
}

func newReadHarness(t *testing.T) *readHarness {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"),
		store.Options{PinnedWriters: 1})
	if err != nil {
		t.Fatalf("open a store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	return &readHarness{
		t: t, db: db, applier: chat.NewApplier("node-a", nil),
		waiter: &readWaiter{}, gen: 1,
		maxVariables: db.Replicated().Caps().MaxVariables,
	}
}

// storedAt is the broker's instant for the record at this position.
func (h *readHarness) storedAt() time.Time {
	return readBrokerAt.Add(time.Duration(h.seq) * time.Second)
}

// apply runs one record at the next position and commits it, exactly as the
// framework's replication loop does — and advances the waiter afterwards, so a
// read asking for this position finds it.
func (h *readHarness) apply(rec chat.MutationRecord) statelog.Position {
	h.t.Helper()
	h.seq++
	rec.Gen = h.gen
	rec.Writer = "node-a"
	body, err := chat.Encode(rec)
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
	if err := h.db.Replicated().Tx(h.t.Context(), func(tx *sql.Tx) error {
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
func (h *readHarness) reader() *chat.Reader {
	h.t.Helper()
	authority, err := statelogtest.LocalReaderOver(chat.Domain{},
		h.db.Replicated(), h.waiter)
	if err != nil {
		h.t.Fatalf("build the read authority: %v", err)
	}
	reader, err := chat.NewReader(chat.ReaderOptions{
		Log: authority, Committed: h.waiter.Committed,
	})
	if err != nil {
		h.t.Fatalf("build the reader: %v", err)
	}
	return reader
}

// readerBehind is the same node reporting itself `lag` records behind the
// log's end, which is the only way a staleness bound is observable at all.
func (h *readHarness) readerBehind(lag uint64) *chat.Reader {
	h.t.Helper()
	authority, err := statelogtest.LocalReaderBehind(chat.Domain{},
		h.db.Replicated(), h.waiter.Committed(), lag)
	if err != nil {
		h.t.Fatalf("build the read authority: %v", err)
	}
	reader, err := chat.NewReader(chat.ReaderOptions{
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
func readRecord(subject chat.Subject, op chat.OpKind, opID string, payload any,
	scope chat.ScopeSet, actor string) chat.MutationRecord {

	rec := chat.MutationRecord{
		RecordEnvelope: chat.RecordEnvelope{
			V: chat.RecordVersion, OpID: opID, Subject: subject, Op: op,
			CreatedAt: readBrokerAt, Scope: scope,
		},
		Actor: actor, ActorKind: chat.AuthorHuman,
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
func readCreateRoom(channelID, name string, kind chat.Kind,
	members ...string) chat.MutationRecord {

	founding := make([]chat.Member, 0, len(members))
	for _, handle := range members {
		founding = append(founding, chat.Member{Handle: handle})
	}
	return readRecord(chat.ChannelNameSubject(name), chat.OpCreate,
		"op-create-"+channelID, chat.ChannelCreate{
			V: chat.DocumentVersion, ChannelID: channelID, Kind: kind,
			Name: name, Members: founding,
			CreatedBy: "ada", CreatedByKind: chat.AuthorHuman,
		}, chat.ScopeSet{Terms: []chat.ScopeTerm{
			{Kind: chat.TermName, ID: chat.ChannelToken(name)},
			{Kind: chat.TermChannel, ID: channelID},
		}}, "ada")
}

// readPost builds one message into a room.
func readPost(channelID, messageID, author, body string,
	mentions ...string) chat.MutationRecord {

	return readRecord(chat.MessageSubject(channelID), chat.OpPost,
		"op-post-"+messageID, chat.MessagePost{
			V: chat.DocumentVersion, MessageID: messageID, Body: body,
			Author: author, AuthorKind: chat.AuthorHuman, Mentions: mentions,
		}, chat.ScopeSet{Subject: true}, author)
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
// The case holds THREE readers to one rule at once: [chat.Visible] over every
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
		kind   chat.Kind
		member bool
		want   bool
	}{
		{chat.KindPublic, false, true},
		{chat.KindPublic, true, true},
		{chat.KindUnit, false, true},
		{chat.KindPrivate, false, false},
		{chat.KindPrivate, true, true},
		{chat.KindDM, false, false},
		{chat.KindDM, true, true},
		{chat.KindGroup, false, false},
		{chat.KindGroup, true, true},
		{chat.Kind("broadcast"), true, false},
	} {
		if got := chat.Visible("ada", chat.Channel{Kind: c.kind}, c.member); got != c.want {
			t.Errorf("Visible(ada, %s room, member=%t) = %t, want %t",
				c.kind, c.member, got, c.want)
		}
	}
	// AND NOBODY IS NOT A READER. Identity is resolved above this package,
	// so an empty handle is an unbound token that slipped through — and the
	// one answer that is never right for one is yes.
	if chat.Visible("", chat.Channel{Kind: chat.KindPublic}, true) {
		t.Error("an empty viewer read a public room — an unbound token gets " +
			"nothing in chat, reads included")
	}

	h := newReadHarness(t)
	h.apply(readCreateRoom("room-open", "open", chat.KindPublic, "ada", "bob"))
	h.apply(readCreateRoom("room-shut", "shut", chat.KindPrivate, "bob"))
	// ONE MESSAGE IN EACH, BOTH NAMING ADA. The mention in the private room
	// is the row the feed must not show her: being named in a room you are
	// not in is ordinary, it wakes nobody, and the message is still a row.
	h.apply(readPost("room-open", "msg-open", "bob", "morning @ada", "ada"))
	h.apply(readPost("room-shut", "msg-shut", "bob", "about @ada", "ada"))
	reader := h.reader()

	_, err := reader.Channel(t.Context(), "ada", "room-shut", h.session())
	if !errors.Is(err, chat.ErrNotFound) {
		t.Errorf("reading a private room ada is not in answered %v, want %v — "+
			"a room she may not read must be indistinguishable from one that "+
			"is not there", err, chat.ErrNotFound)
	}
	if errors.Is(err, chat.ErrForbidden) {
		t.Error("reading a private room answered `forbidden`, which tells " +
			"anybody who guesses an id that the room exists")
	}
	if _, err := reader.Messages(t.Context(), "ada",
		chat.TranscriptQuery{ChannelID: "room-shut"}, h.session()); !errors.Is(
		err, chat.ErrNotFound) {

		t.Errorf("reading a private room's transcript answered %v, want %v",
			err, chat.ErrNotFound)
	}

	// THE CONTROL, and without it every assertion above is satisfied by a
	// reader that finds nothing at all: bob is in the room and reads it.
	detail, err := reader.Channel(t.Context(), "bob", "room-shut", h.session())
	if err != nil {
		t.Fatalf("bob reading the room he is in: %v", err)
	}
	if !detail.Member || detail.Channel.Kind != chat.KindPrivate {
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

	feed, err := reader.Mentions(t.Context(), "ada", chat.MentionQuery{}, h.session())
	if err != nil {
		t.Fatalf("ada's mention feed: %v", err)
	}
	if len(feed.Mentions) != 1 || feed.Mentions[0].MessageID != "msg-open" {
		t.Fatalf("ada's mention feed is %v, want the public room's mention "+
			"alone — a mention in a room she cannot open discloses the room",
			mentionIDs(feed))
	}
}

// A CURSOR SPANS A REANCHOR, with no gap and no repeat.
//
// An operator who loses the broker estate rebuilds it, and the new stream
// starts at one. Every cursor this package hands out has to survive that, and
// the two of them survive it for DIFFERENT reasons — which is why both halves
// are here:
//
//   - THE FEED pages on the COMPOSED position, where the generation is IN the
//     ordering. A bare sequence would page the second half of this case
//     against `seq < 1` and return nothing, losing every message written
//     before the rebuild.
//   - THE TRANSCRIPT pages on `channel_seq`, which the applier mints from log
//     order and never moves. It is contiguous across the rebuild because the
//     room's high-water mark is a row rather than a position.
func TestACursorSpansAReanchor(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	h.apply(readCreateRoom("room-1", "eng", chat.KindPublic, "ada", "bob"))
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
		got, err := reader.Messages(t.Context(), "ada", chat.TranscriptQuery{
			ChannelID: "room-1", Cursor: cursor, Limit: 2,
		}, h.session())
		if err != nil {
			t.Fatalf("transcript page %d: %v", page, err)
		}
		for _, view := range got.Messages {
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
		got, err := reader.Mentions(t.Context(), "ada", chat.MentionQuery{
			Cursor: cursor, Limit: 2,
		}, h.session())
		if err != nil {
			t.Fatalf("mention page %d: %v", page, err)
		}
		for _, mention := range got.Mentions {
			seen = append(seen, mention.MessageID)
		}
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
	h.apply(readCreateRoom("room-1", "eng", chat.KindPublic, "ada", "bob"))
	var last statelog.Position
	for i := 0; i < chat.UnreadLimit+20; i++ {
		last = h.apply(readPost("room-1", fmt.Sprintf("msg-%03d", i), "bob",
			fmt.Sprintf("message %d", i)))
	}
	reader := h.reader()

	// A ROOM ADA HAS NEVER OPENED: no cursor at all, a hundred and twenty
	// messages in it, and an answer of a hundred that says it is a floor.
	fresh := h.readRail(reader, nil)
	if fresh.Unread != chat.UnreadLimit || !fresh.UnreadCapped {
		t.Errorf("a room with %d unread messages reports %d (capped=%t), want "+
			"%d and a capped flag — the number is what a client renders as "+
			"\"99+\"", chat.UnreadLimit+20, fresh.Unread, fresh.UnreadCapped,
			chat.UnreadLimit)
	}
	if fresh.Last == nil || fresh.Last.MessageID != fmt.Sprintf("msg-%03d",
		chat.UnreadLimit+19) {

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
	h.apply(readCreateRoom("room-1", "eng", chat.KindPublic, "ada"))
	at := h.apply(readPost("room-1", "msg-1", "ada", "just said this"))
	reader := h.reader()

	got, err := reader.Messages(t.Context(), "ada",
		chat.TranscriptQuery{ChannelID: "room-1"},
		statelog.Freshness{Level: statelog.ReadSession, MinPosition: at})
	if err != nil {
		t.Fatalf("a read floored at its own write: %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Message.ID != "msg-1" {
		t.Fatalf("a read floored at its own write answered %d messages, want "+
			"the one it just wrote", len(got.Messages))
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
		chat.TranscriptQuery{ChannelID: "room-1"},
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
	h.apply(readCreateRoom("room-1", "eng", chat.KindPublic, "ada"))
	h.apply(readPost("room-1", "msg-1", "ada", "said before the node fell back"))
	reader := h.readerBehind(500)

	_, err := reader.Messages(t.Context(), "ada",
		chat.TranscriptQuery{ChannelID: "room-1"},
		statelog.Freshness{Level: statelog.ReadStale, MaxLagSeq: 10})
	var refused *statelog.Refused
	if !errors.As(err, &refused) || refused.Code != statelog.RefuseTooStale {
		t.Fatalf("a node 500 records behind served a read that accepts 10: "+
			"%v — a bound nothing enforces is a label on an answer of any age",
			err)
	}

	got, err := reader.Messages(t.Context(), "ada",
		chat.TranscriptQuery{ChannelID: "room-1"},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("an unbounded stale read on a behind node: %v — a node that "+
			"is behind is still a node that can answer", err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("an unbounded stale read answered %d messages, want the one "+
			"this node has", len(got.Messages))
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
func (h *readHarness) readRail(reader *chat.Reader,
	cursors map[string]int64) chat.ChannelSummary {

	h.t.Helper()
	listing, err := reader.Channels(h.t.Context(), "ada",
		chat.ChannelsQuery{Cursors: cursors}, h.session())
	if err != nil {
		h.t.Fatalf("ada's channel list: %v", err)
	}
	if len(listing.Channels) != 1 {
		h.t.Fatalf("ada's channel list holds %d rooms, want the one she is in",
			len(listing.Channels))
	}
	return listing.Channels[0]
}

// mentionIDs is a feed as the messages it names.
func mentionIDs(feed chat.MentionFeed) []string {
	out := make([]string, 0, len(feed.Mentions))
	for _, mention := range feed.Mentions {
		out = append(out, mention.MessageID)
	}
	return out
}
