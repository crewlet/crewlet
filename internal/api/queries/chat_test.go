// The chat read surface: who may ask, what travels into the read, and what
// comes back.
//
// EVERY CASE HERE IS ABOUT THIS LAYER and not about the reader underneath it.
// The rules about rooms, cursors and unread ranges are certified in
// internal/chat against a real estate; what is decided HERE is who the viewer
// is, what reaches the reader, how a refusal is classified, and what the wire
// carries — and each of those has failed silently somewhere in this tree
// before: a filter honoured on one transport, a level that was a label, a
// refusal rendered as a broken server.
//
// So the assertions are mostly about the REQUEST the surface built and the
// JSON it produced, which is the half a canned answer would otherwise hide.

package queries_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
)

// chatQuestions is the whole family, so a sweep cannot silently stop covering
// one of them — the shape of the bug that let a question be registered with a
// different posture from its siblings.
var chatQuestions = []string{
	"chat_channels", "chat_channel", "chat_messages", "chat_thread",
	"chat_mentions", "chat_search",
}

// chatAsked is a parameter set per question, so one table can ask all six
// without a question being skipped for want of a room id.
func chatAsked(what string) map[string]any {
	switch what {
	case "chat_channel", "chat_messages":
		return map[string]any{"channel_id": "c-eng"}
	case "chat_thread":
		return map[string]any{"channel_id": "c-eng", "root_id": "m-1"}
	case "chat_search":
		return map[string]any{"q": "acquisition"}
	}
	return map[string]any{}
}

// chatAt is the position every canned answer here was true as of. A real
// stream name rather than a word, because the position travels to a client as
// `<stream>@<generation>:<sequence>` and a cursor that cannot say which stream
// it came from pages a feed from a log that no longer exists.
var chatAt = statelog.Position{Stream: topics.ChatLogStream, Generation: 2, Seq: 4117}

// chatServed is what every reader answer carries beside its rows.
var chatServed = chat.Served{
	Level: statelog.ReadStale, Complete: true, Position: chatAt,
}

// ---- the reader twin --------------------------------------------------- //

// THE SEAMS ARE THE SHIPPED IMPLEMENTATIONS, asserted at compile time.
//
// A consumer-defined interface is a claim about somebody else's type, and the
// only thing that checks it is the package that wires them together — which is
// a different package, built by a different agent, discovering the mismatch as
// a wall of method-set errors. Three lines here name the mismatch where the
// shape was chosen instead.
var (
	_ queries.ChatReader   = (*chat.Reader)(nil)
	_ queries.ChatSearcher = (*search.ChatIndexer)(nil)
	_ queries.ChatReads    = (coord.Fleet)(nil)
)

// stubChat is chat's read side as this surface sees it.
//
// IT DECIDES VISIBILITY WITH [chat.Visible] — the shipped function, over rooms
// and memberships the case declares — rather than with a canned error, because
// the claim under test is that a room somebody may not read reaches a caller
// as NOT FOUND. A stub that returned whatever it was told would pass with the
// viewer dropped entirely.
type stubChat struct {
	rooms   map[string]chat.Channel
	members map[string][]string

	// What the surface asked for, which is the half a canned answer
	// hides: the viewer it resolved, the freshness it carried, and each
	// query it built.
	viewer   string
	fresh    statelog.Freshness
	channels chat.ChannelsQuery
	page     chat.TranscriptQuery
	thread   chat.ThreadQuery
	feed     chat.MentionQuery

	// reads counts every call that reached this reader, so a case about a
	// refusal can prove nothing was read rather than that nothing was
	// returned.
	reads int

	listing    chat.ChannelListing
	transcript chat.Transcript
	replies    chat.Thread
	mentions   chat.MentionFeed
	detail     chat.ChannelDetail
	readable   []string

	err error
}

func (s *stubChat) At() statelog.Position { return chatAt }

// see is the visibility rule, applied exactly as the reader applies it: a room
// that is not there and a room this viewer may not read answer identically,
// because a private room's existence is itself information.
func (s *stubChat) see(viewer, id string) error {
	room, held := s.rooms[id]
	if !held || !chat.Visible(viewer, room, slices.Contains(s.members[id], viewer)) {
		return fmt.Errorf("%w: room %s", chat.ErrNotFound, id)
	}
	return nil
}

func (s *stubChat) Channels(_ context.Context, viewer string, q chat.ChannelsQuery,
	fresh statelog.Freshness) (chat.ChannelListing, error) {

	s.reads++
	s.viewer, s.fresh, s.channels = viewer, fresh, q
	return s.listing, s.err
}

func (s *stubChat) Channel(_ context.Context, viewer, channelID string,
	fresh statelog.Freshness) (chat.ChannelDetail, error) {

	s.reads++
	s.viewer, s.fresh = viewer, fresh
	if s.err != nil {
		return chat.ChannelDetail{}, s.err
	}
	if err := s.see(viewer, channelID); err != nil {
		return chat.ChannelDetail{}, err
	}
	out := s.detail
	out.Channel = s.rooms[channelID]
	out.Served = chatServed
	return out, nil
}

func (s *stubChat) Messages(_ context.Context, viewer string, q chat.TranscriptQuery,
	fresh statelog.Freshness) (chat.Transcript, error) {

	s.reads++
	s.viewer, s.fresh, s.page = viewer, fresh, q
	if s.err != nil {
		return chat.Transcript{}, s.err
	}
	if err := s.see(viewer, q.ChannelID); err != nil {
		return chat.Transcript{}, err
	}
	out := s.transcript
	out.ChannelID, out.Served = q.ChannelID, chatServed
	return out, nil
}

func (s *stubChat) Thread(_ context.Context, viewer string, q chat.ThreadQuery,
	fresh statelog.Freshness) (chat.Thread, error) {

	s.reads++
	s.viewer, s.fresh, s.thread = viewer, fresh, q
	if s.err != nil {
		return chat.Thread{}, s.err
	}
	if err := s.see(viewer, q.ChannelID); err != nil {
		return chat.Thread{}, err
	}
	out := s.replies
	out.ChannelID, out.Served = q.ChannelID, chatServed
	return out, nil
}

func (s *stubChat) Mentions(_ context.Context, viewer string, q chat.MentionQuery,
	fresh statelog.Freshness) (chat.MentionFeed, error) {

	s.reads++
	s.viewer, s.fresh, s.feed = viewer, fresh, q
	if s.err != nil {
		return chat.MentionFeed{}, s.err
	}
	out := s.mentions
	out.Handle, out.Served = viewer, chatServed
	return out, nil
}

func (s *stubChat) Readable(_ context.Context, viewer string,
	fresh statelog.Freshness) ([]string, error) {

	s.reads++
	s.viewer, s.fresh = viewer, fresh
	return s.readable, s.err
}

// stubChatSearch is the index twin, and it REFUSES an empty channel set
// exactly as [search.ChatIndexer.SearchMessages] does.
//
// That refusal is the point of the twin. A surface that handed the index a
// viewer it had lost would be answered [search.ErrNoViewer] in production and
// an empty result by a permissive stub — so the stub that answered nothing
// would certify the bug.
type stubChatSearch struct {
	query search.ChatQuery
	calls int
	hits  []search.ChatHit
	err   error
}

func (s *stubChatSearch) SearchMessages(_ context.Context, q search.ChatQuery) (
	[]search.ChatHit, error) {

	s.calls++
	s.query = q
	if len(q.Channels) == 0 {
		return nil, search.ErrNoViewer
	}
	return s.hits, s.err
}

// unreadableReads is a coordination store that cannot be reached, which is a
// state the rail has to be able to report: the rooms are still true and the
// badges are not.
type unreadableReads struct{ calls int }

func (u *unreadableReads) ChatRead(context.Context, string) (coord.ChatReadState, error) {
	u.calls++
	return coord.ChatReadState{}, errors.New("the fleet store did not answer")
}

// ---- the fixture ------------------------------------------------------- //

// chatRooms are the three shapes visibility has to tell apart: a public room
// anybody in the company may read, a private one that is its membership, and
// a private one this viewer is in.
func chatRooms() (map[string]chat.Channel, map[string][]string) {
	rooms := map[string]chat.Channel{
		"c-eng":    {ID: "c-eng", Kind: chat.KindPublic, Name: "engineering"},
		"c-secret": {ID: "c-secret", Kind: chat.KindPrivate, Name: "acquisition"},
		"c-ours":   {ID: "c-ours", Kind: chat.KindPrivate, Name: "leads"},
	}
	members := map[string][]string{
		"c-secret": {"bo"},
		"c-ours":   {"ana", "bo"},
	}
	return rooms, members
}

func newStubChat() *stubChat {
	rooms, members := chatRooms()
	return &stubChat{
		rooms: rooms, members: members,
		listing: chat.ChannelListing{Served: chatServed},
		// EVERY ROOM THIS VIEWER MAY READ, which is wider than the rail
		// — a public room is readable by any seat, joined or not.
		readable: []string{"c-eng", "c-ours"},
	}
}

// chatSources wires the surface the way a node does: the company (which is how
// a token becomes a seat), the reader, the index and the coordination record.
func chatSources(t *testing.T, reader queries.ChatReader, index queries.ChatSearcher,
	reads queries.ChatReads) queries.Sources {

	t.Helper()
	cfg, err := config.ParseCompany([]byte(viewerCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	return queries.Sources{
		Company:    func() *config.Company { return cfg },
		Chat:       reader,
		ChatSearch: index,
		ChatReads:  reads,
	}
}

// askChat asks one question as ana, whose seat binds the ops-1 token.
func askChat(t *testing.T, s queries.Sources, what string, params map[string]any) (any, error) {
	t.Helper()
	r := queries.NewRegistry()
	queries.Register(r, s)
	return r.Answer(t.Context(), what, params, "ops-1")
}

// chatJSON is the answer AS A CLIENT RECEIVES IT.
//
// Through the encoder rather than by type assertion, because half of what this
// surface decides is the wire shape: an embedded struct that stopped being
// embedded, a flag lost to `omitempty`, an instant left as the store's own
// integer. A case reading Go fields would pass through every one of those.
func chatJSON(t *testing.T, got any, err error) map[string]any {
	t.Helper()
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal the answer: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the answer is not a JSON object: %v", err)
	}
	return out
}

// askChatJSON is one question asked as ana and read as a client reads it.
func askChatJSON(t *testing.T, s queries.Sources, what string,
	params map[string]any) map[string]any {

	t.Helper()
	answered, err := askChat(t, s, what, params)
	return chatJSON(t, answered, err)
}

// chatReadsFor seeds one person's real read state in the coordination twin —
// the same implementation the engine runs, held to the same contract suite,
// rather than a stand-in for it.
func chatReadsFor(t *testing.T, handle string, delta coord.ChatReadDelta) queries.ChatReads {
	t.Helper()
	fleet := coordmemory.NewFleet()
	delta.At = time.Date(2031, 5, 6, 7, 8, 0, 0, time.UTC)
	if _, err := fleet.AdvanceChatRead(t.Context(), handle, delta); err != nil {
		t.Fatalf("seed %s's read state: %v", handle, err)
	}
	return fleet
}

// ---- who may ask ------------------------------------------------------- //

// A CREDENTIAL THAT IS NOT A PERSON READS NOTHING — reads included, public
// rooms included.
//
// This is stricter than every other personal question here, where a caller
// with no seat can still name a handle with an operator credential. A
// transcript has no such arm: the refusal names the field to change, and it
// must reach the caller rather than the log, because the remedy is a line of
// company configuration that nobody can guess from `unauthorized`.
func TestEveryChatQuestionRefusesACredentialBoundToNoSeat(t *testing.T) {
	t.Parallel()
	for _, what := range chatQuestions {
		reader, index := newStubChat(), &stubChatSearch{}
		r := queries.NewRegistry()
		queries.Register(r, chatSources(t, reader, index, nil))
		// A REAL TOKEN naming an operator id no seat claims — the
		// pipeline credential, which is exactly the case the product
		// page names.
		_, err := r.Answer(t.Context(), what, chatAsked(what), "ops-nobody")
		if !errors.Is(err, queries.ErrBadParams) {
			t.Errorf("%s answered %v for an unbound token, want a refusal a "+
				"client keeps the text of", what, err)
		}
		if !strings.Contains(err.Error(), "contact.crewlet_operator_id") {
			t.Errorf("%s refused with %q, which does not name the field an "+
				"operator has to change", what, err)
		}
		if reader.reads != 0 || index.calls != 0 {
			t.Errorf("%s read %d times and searched %d for a caller who is "+
				"nobody — the viewer is resolved before anything is read",
				what, reader.reads, index.calls)
		}
	}
}

// AND AN ANONYMOUS CALLER NEVER REACHES THE QUESTION AT ALL.
//
// Chat is the one read family with no anonymous form: the seat is resolved
// FROM the credential, so a caller with none is not a person this surface can
// answer about. Declared in the registry rather than discovered six times,
// which is also what keeps `api.allow_anonymous_read` away from a transcript.
func TestTheChatFamilyIsOperatorOnlyRatherThanAnonymouslyReadable(t *testing.T) {
	t.Parallel()
	reader, index := newStubChat(), &stubChatSearch{}
	r := queries.NewRegistry()
	queries.Register(r, chatSources(t, reader, index, nil))
	for _, what := range chatQuestions {
		if !slices.Contains(r.Names(), what) {
			t.Errorf("%s is not registered, so this case asserts nothing", what)
			continue
		}
		if !r.RequiresOperator(what) {
			t.Errorf("%s is served to any caller; a transcript is the most "+
				"sensitive thing this deployment holds", what)
		}
		if _, err := r.Answer(t.Context(), what, chatAsked(what), ""); !errors.Is(
			err, queries.ErrUnauthorized) {

			t.Errorf("%s answered %v with no credential at all, want an "+
				"authorization refusal", what, err)
		}
	}
	if reader.reads != 0 {
		t.Errorf("an anonymous caller reached the reader %d times", reader.reads)
	}
}

// THE VIEWER IS THE SERVER'S ANSWER, and a caller may never name a seat.
//
// The other personal questions take a `handle` and check it. Here it is not a
// parameter at all: a credential that could name a seat could read every
// direct conversation in the company, so the keys a caller might reach for are
// simply not read.
func TestACallerCannotNameTheSeatChatIsReadAs(t *testing.T) {
	t.Parallel()
	reader := newStubChat()
	_, err := askChat(t, chatSources(t, reader, nil, nil), "chat_messages",
		map[string]any{"channel_id": "c-eng", "handle": "bo", "viewer": "bo", "as": "bo"})
	if err != nil {
		t.Fatalf("chat_messages: %v", err)
	}
	if reader.viewer != "ana" {
		t.Errorf("the read was served as %q; ops-1 is ana's token, and naming "+
			"anybody else must change nothing", reader.viewer)
	}
}

// A PRIVATE ROOM'S EXISTENCE IS ITSELF INFORMATION.
//
// So a room somebody may not read answers exactly as a room that is not there
// — and the refusal has to survive this layer as NOT FOUND rather than as a
// failure, which a client renders as a broken server for what is a dead link.
func TestAPrivateRoomIsNotFoundToANonMemberAndFoundToAMember(t *testing.T) {
	t.Parallel()
	for _, what := range []string{"chat_channel", "chat_messages"} {
		reader := newStubChat()
		sources := chatSources(t, reader, nil, nil)

		_, err := askChat(t, sources, what, map[string]any{"channel_id": "c-secret"})
		if !errors.Is(err, queries.ErrNotFound) {
			t.Errorf("%s of a room ana is not in answered %v, want not found — "+
				"anything else confirms the room exists", what, err)
		}
		// AND THE SAME ROOM SHAPE, joined, is served: without this the
		// case above would pass with every private room refused.
		answered, err := askChat(t, sources, what, map[string]any{"channel_id": "c-ours"})
		if err != nil {
			t.Errorf("%s of a room ana is in: %v", what, err)
			continue
		}
		if got := chatJSON(t, answered, nil); got["position"] == nil {
			t.Errorf("%s answered %v without a position", what, got)
		}
	}
}

// ---- the rail ---------------------------------------------------------- //

// THE BADGE IS THIS VIEWER'S OWN CURSORS, COUNTED TO A CAP.
//
// Both halves are this surface's to get right. The cursors live in
// coordination rather than on the log, so a rail that did not carry them would
// count every room's whole tail as unread — and the cap, which is what stops a
// person back from a fortnight away costing this node a scan of every
// transcript, is only usable by a screen if the FLAG travels: "100" has to be
// able to mean "at least a hundred" without meaning exactly a hundred.
func TestTheUnreadBadgeIsTheViewersOwnCursorsCountedToTheCap(t *testing.T) {
	t.Parallel()
	reader := newStubChat()
	cursors := map[string]int64{"c-eng": 4096, "c-ours": 12}
	reader.listing.Channels = []chat.ChannelSummary{{
		Channel: reader.rooms["c-eng"],
		Unread:  chat.UnreadLimit, UnreadCapped: true,
	}}
	reads := chatReadsFor(t, "ana", coord.ChatReadDelta{Cursors: cursors})

	got := askChatJSON(t, chatSources(t, reader, nil, reads), "chat_channels", nil)

	if !maps.Equal(reader.channels.Cursors, cursors) {
		t.Errorf("the read was given cursors %v, want ana's own %v — without "+
			"them every room reports its whole tail unread",
			reader.channels.Cursors, cursors)
	}
	rooms, _ := got["channels"].([]any)
	if len(rooms) != 1 {
		t.Fatalf("the rail carried %d rooms, want the one the reader answered", len(rooms))
	}
	room, _ := rooms[0].(map[string]any)
	if n, _ := room["unread"].(float64); int(n) != chat.UnreadLimit {
		t.Errorf("unread = %v, want the counted range's cap of %d", room["unread"],
			chat.UnreadLimit)
	}
	if room["unread_capped"] != true {
		t.Errorf("the row is %v; without unread_capped a screen cannot draw "+
			"99+ and a hundred means two different things", room)
	}
	if got["read_state"] != true {
		t.Errorf("read_state = %v, want true: the cursors were read", got["read_state"])
	}
}

// A MUTE IS A FACT ABOUT THE READER, not about the room, so it reaches the
// rail from coordination and is marked per row — and do-not-disturb, the one
// piece of this state that is not per room, rides the listing itself.
func TestTheRailMarksTheRoomsThisViewerSilenced(t *testing.T) {
	t.Parallel()
	reader := newStubChat()
	reader.listing.Channels = []chat.ChannelSummary{
		{Channel: reader.rooms["c-eng"]},
		{Channel: reader.rooms["c-ours"]},
	}
	until := time.Date(2031, 5, 6, 9, 0, 0, 0, time.UTC)
	reads := chatReadsFor(t, "ana", coord.ChatReadDelta{
		Muted: []string{"c-eng"}, DNDUntil: &until,
	})

	got := askChatJSON(t, chatSources(t, reader, nil, reads), "chat_channels", nil)

	rooms, _ := got["channels"].([]any)
	if len(rooms) != 2 {
		t.Fatalf("the rail carried %d rooms, want 2", len(rooms))
	}
	first, _ := rooms[0].(map[string]any)
	second, _ := rooms[1].(map[string]any)
	if first["muted"] != true {
		t.Errorf("c-eng is %v, want muted: this viewer silenced it", first)
	}
	if _, marked := second["muted"]; marked {
		t.Errorf("c-ours carried a mute flag (%v) and nobody muted it", second)
	}
	if got["dnd_until"] != until.Format(time.RFC3339) {
		t.Errorf("dnd_until = %v, want %s", got["dnd_until"],
			until.Format(time.RFC3339))
	}
}

// AN UNREADABLE CURSOR IS NOT A CURSOR AT ZERO.
//
// The rooms, their order and their previews are the log's and are still true,
// so a coordination store that cannot be reached must not take the whole
// screen away — and it must not badge every room in the company with its
// entire history either, which is what the reader's own arithmetic produces
// for somebody who has read nothing. The answer says which of the three it is.
func TestTheRailSaysWhenTheBadgesCouldNotBeMeasured(t *testing.T) {
	t.Parallel()
	reader := newStubChat()
	reader.listing.Channels = []chat.ChannelSummary{{
		Channel: reader.rooms["c-eng"],
		// What the reader answers with no cursor: the room's whole
		// tail, capped.
		Unread: chat.UnreadLimit, UnreadCapped: true,
	}}
	reads := &unreadableReads{}

	got := askChatJSON(t, chatSources(t, reader, nil, reads), "chat_channels", nil)

	if reads.calls == 0 {
		t.Fatal("the read state was never asked for, so this case asserts nothing")
	}
	if got["read_state"] != false {
		t.Errorf("read_state = %v, want false: nobody could look", got["read_state"])
	}
	rooms, _ := got["channels"].([]any)
	if len(rooms) != 1 {
		t.Fatalf("the rail carried %d rooms, want the room itself: the rooms "+
			"are the log's and are still true", len(rooms))
	}
	room, _ := rooms[0].(map[string]any)
	if n, _ := room["unread"].(float64); n != 0 {
		t.Errorf("unread = %v with no cursor to count from — a client that "+
			"ignores read_state must under-badge rather than tell everybody "+
			"they are a hundred behind everywhere", room["unread"])
	}
	if _, capped := room["unread_capped"]; capped {
		t.Errorf("the row still claims a capped count: %v", room)
	}
}

// ---- paging ------------------------------------------------------------ //

// A CURSOR ROUND-TRIPS UNCHANGED, and the two grammars are not interchangeable.
//
// A transcript pages on the room's own message number, minted once and never
// moved; a feed pages on the composed log position, which carries the
// generation in its ordering and spans a reanchor. This surface neither builds
// nor decodes either one — it hands back what the answer gave and sends back
// what the caller returned — which is the only arrangement where a page cannot
// gain a gap or a repeat.
func TestAPageCursorRoundTripsUnchanged(t *testing.T) {
	t.Parallel()
	reader := newStubChat()
	reader.transcript.NextCursor = "41"
	reader.mentions.NextCursor = chatAt.String()
	sources := chatSources(t, reader, nil, nil)

	page := askChatJSON(t, sources, "chat_messages", map[string]any{"channel_id": "c-eng"})
	next, _ := page["next_cursor"].(string)
	if next != "41" {
		t.Fatalf("next_cursor = %q, want the reader's own", next)
	}
	if _, err := askChat(t, sources, "chat_messages",
		map[string]any{"channel_id": "c-eng", "cursor": next}); err != nil {
		t.Fatalf("the second page: %v", err)
	}
	if reader.page.Cursor != next {
		t.Errorf("the second page asked for cursor %q, want %q — a cursor this "+
			"surface rewrites is a page with a gap in it", reader.page.Cursor, next)
	}

	feed := askChatJSON(t, sources, "chat_mentions", nil)
	from, _ := feed["next_cursor"].(string)
	if from != chatAt.String() {
		t.Fatalf("the feed's next_cursor = %q, want a log position", from)
	}
	if _, err := askChat(t, sources, "chat_mentions",
		map[string]any{"cursor": from}); err != nil {
		t.Fatalf("the second page of the feed: %v", err)
	}
	if reader.feed.Cursor != from {
		t.Errorf("the feed resumed from %q, want %q", reader.feed.Cursor, from)
	}
}

// AND A PAGE IS BOUNDED AT BOTH ENDS. An absent limit is a screen and not
// nothing; a limit past the ceiling is served the ceiling, because a page size
// is not a filter and answering less than was asked for costs a reader nothing
// they can act on.
func TestAPageTakesTheDomainsOwnDefaultAndCeiling(t *testing.T) {
	t.Parallel()
	reader := newStubChat()
	sources := chatSources(t, reader, nil, nil)

	if _, err := askChat(t, sources, "chat_messages",
		map[string]any{"channel_id": "c-eng"}); err != nil {
		t.Fatalf("chat_messages: %v", err)
	}
	if reader.page.Limit != chat.DefaultLimit {
		t.Errorf("an unstated limit asked for %d, want a screenful (%d)",
			reader.page.Limit, chat.DefaultLimit)
	}
	if _, err := askChat(t, sources, "chat_messages",
		map[string]any{"channel_id": "c-eng", "limit": 5000}); err != nil {
		t.Fatalf("chat_messages: %v", err)
	}
	if reader.page.Limit != chat.MaxLimit {
		t.Errorf("a limit of 5000 asked for %d, want the ceiling %d",
			reader.page.Limit, chat.MaxLimit)
	}
}

// A CURSOR THIS DOMAIN REFUSES IS THE CALLER'S TO FIX, and the refusal's own
// text is what says how: it names what a cursor looks like and where the last
// answer put one. Classified as a failure it would be a 500 telling somebody
// the server is broken over a value they pasted.
func TestARefusedCursorReachesTheCallerAsABadRequest(t *testing.T) {
	t.Parallel()
	reader := newStubChat()
	reader.err = fmt.Errorf("%w: cursor: %q is not a message number",
		chat.ErrInvalid, "yesterday")

	_, err := askChat(t, chatSources(t, reader, nil, nil), "chat_messages",
		map[string]any{"channel_id": "c-eng", "cursor": "yesterday"})
	if !errors.Is(err, queries.ErrBadParams) {
		t.Fatalf("a refused cursor answered %v, want a bad request", err)
	}
	if !strings.Contains(err.Error(), "is not a message number") {
		t.Errorf("the refusal reached the caller as %q, losing the sentence "+
			"that says what a cursor is", err)
	}
}

// ---- search ------------------------------------------------------------ //

// A SEARCH WITHOUT A VIEWER IS REFUSED RATHER THAN ANSWERED EMPTY.
//
// [search.ErrNoViewer] exists because an empty channel set has two readings
// that differ by the whole transcript: a caller that genuinely sees no rooms,
// and one that LOST its viewer. So the set is never handed to the index empty
// — a viewer who may read nothing is answered here, before the index is asked,
// and an unbound credential never gets this far at all.
func TestASearchNeverHandsTheIndexAViewerItLost(t *testing.T) {
	t.Parallel()
	reader, index := newStubChat(), &stubChatSearch{}
	// A REAL, ORDINARY STATE: a company whose rooms this person may not
	// read, or one with no rooms yet.
	reader.readable = nil

	got := askChatJSON(t, chatSources(t, reader, index, nil), "chat_search",
		map[string]any{"q": "acquisition"})

	if index.calls != 0 {
		t.Errorf("the index was asked %d times with %v — an empty set is the "+
			"shape a caller that lost its viewer produces, and the index "+
			"refuses it", index.calls, index.query)
	}
	if hits, _ := got["hits"].([]any); len(hits) != 0 {
		t.Errorf("hits = %v, want none", got["hits"])
	}
	if n, _ := got["searched"].(float64); n != 0 {
		t.Errorf("searched = %v, want 0 rooms — it is the denominator that "+
			"tells 'nothing matched' from 'nothing was looked at'", got["searched"])
	}
}

// THE VISIBLE SET IS COMPUTED HERE AND IS WIDER THAN THE RAIL: a public room
// is readable by any seat, joined or not, so a search driven off somebody's
// membership answers nothing from the rooms a company does most of its talking
// in — silently, because an empty result reads as "nobody said that".
func TestASearchRunsOverEveryRoomTheViewerMayRead(t *testing.T) {
	t.Parallel()
	reader, index := newStubChat(), &stubChatSearch{}
	index.hits = []search.ChatHit{{
		MessageID: "m-9", ChannelID: "c-eng", ThreadRoot: "m-1", Author: "bo",
		Excerpt:   strings.Repeat("a paragraph somebody pasted. ", 4000),
		CreatedAt: time.Date(2031, 5, 6, 7, 9, 0, 0, time.UTC).UnixMicro(),
		Score:     3.5,
	}}

	got := askChatJSON(t, chatSources(t, reader, index, nil), "chat_search",
		map[string]any{"q": "acquisition", "author": "bo"})

	if !slices.Equal(index.query.Channels, reader.readable) {
		t.Errorf("the search ran over %v, want every room ana may read (%v)",
			index.query.Channels, reader.readable)
	}
	if index.query.Author != "bo" || index.query.Text != "acquisition" {
		t.Errorf("the index was asked %+v, losing the caller's own narrowing",
			index.query)
	}
	if index.query.Limit != queries.DefaultChatSearchPage {
		t.Errorf("limit = %d, want this surface's own default of %d",
			index.query.Limit, queries.DefaultChatSearchPage)
	}
	hits, _ := got["hits"].([]any)
	if len(hits) != 1 {
		t.Fatalf("hits = %v, want the one the index ranked", got["hits"])
	}
	hit, _ := hits[0].(map[string]any)
	// THE ROW IS RENDERED, not passed through: the index's `excerpt` is
	// the message's WHOLE body and its instant is the store's own
	// integer, so a page of twenty-five would be hundreds of kilobytes of
	// JSON to draw twenty-five lines.
	excerpt, _ := hit["excerpt"].(string)
	if len(excerpt) > chat.MaxExcerpt {
		t.Errorf("the hit carries %d bytes, past the %d-byte excerpt every "+
			"other surface cuts to", len(excerpt), chat.MaxExcerpt)
	}
	if hit["at"] != "2031-05-06T07:09:00Z" {
		t.Errorf("at = %v, want the instant rather than the store's integer", hit["at"])
	}
	if hit["thread_root"] != "m-1" {
		t.Errorf("the hit is %v; without the thread a result opens the room "+
			"and loses the context the words were said in", hit)
	}
}

// NARROWING TO A ROOM IS INTERSECTED, NEVER TRUSTED. Naming a room the viewer
// may not read answers nothing rather than refusing — a refusal confirms the
// room exists, and "there is no such room" and "you may not see it" must read
// identically from outside.
func TestASearchNarrowedToAnUnreadableRoomAnswersNothing(t *testing.T) {
	t.Parallel()
	reader, index := newStubChat(), &stubChatSearch{}

	answered, err := askChat(t, chatSources(t, reader, index, nil), "chat_search",
		map[string]any{"q": "acquisition", "channel_id": "c-secret"})
	got := chatJSON(t, answered, err)

	if index.calls != 0 {
		t.Errorf("the index was asked over %v for a room ana cannot read",
			index.query.Channels)
	}
	if hits, _ := got["hits"].([]any); len(hits) != 0 {
		t.Errorf("hits = %v, want none", got["hits"])
	}
	// AND A ROOM THEY CAN READ NARROWS TO EXACTLY THAT ROOM, or the case
	// above would pass with the filter refusing everything.
	if _, err := askChat(t, chatSources(t, reader, index, nil), "chat_search",
		map[string]any{"q": "acquisition", "channel_id": "c-ours"}); err != nil {
		t.Fatalf("a search inside a room ana is in: %v", err)
	}
	if !slices.Equal(index.query.Channels, []string{"c-ours"}) {
		t.Errorf("the search ran over %v, want only the room that was named",
			index.query.Channels)
	}
}

// ---- what every answer carries ----------------------------------------- //

// A SCREEN THAT CANNOT SAY HOW FAR BEHIND IT IS RENDERS A STALE ROOM AS AN
// EMPTY ONE.
//
// So every answer here carries the position it was true as of — the five
// reader-backed ones from the read itself, and the search from this node's own
// applied position, which is the only one it has: the visible set comes back
// as rooms rather than as the transaction they were read in.
func TestEveryChatAnswerCarriesThePositionItWasTrueAsOf(t *testing.T) {
	t.Parallel()
	want := chatAt.String()
	for _, what := range chatQuestions {
		reader, index := newStubChat(), &stubChatSearch{}
		got := askChatJSON(t, chatSources(t, reader, index, nil), what, chatAsked(what))
		position, _ := got["position"].(map[string]any)
		if position == nil {
			t.Errorf("%s answered %v with no position", what, got)
			continue
		}
		generation, _ := position["generation"].(float64)
		seq, _ := position["seq"].(float64)
		at := statelog.Position{
			Stream:     fmt.Sprint(position["stream"]),
			Generation: uint32(generation), Seq: uint64(seq),
		}
		if at.String() != want {
			t.Errorf("%s was true as of %s, want %s", what, at, want)
		}
		if got["read_level"] == nil {
			t.Errorf("%s answered %v with no read level; a level nobody can "+
				"see is a label rather than a guarantee", what, got)
		}
	}
}

// A NODE BEHIND THE LOG SAYS SO RATHER THAN ANSWERING EMPTY.
//
// The classification is the state log's own — a node that is behind will catch
// up — and without it every refusal reaches a client as a plain failure and is
// drawn as a broken server on a screen that would have worked in a few
// seconds. The hint is the refusal's own, derived from how fast this node is
// actually draining.
func TestANodeBehindTheLogRefusesEveryChatQuestionRetryably(t *testing.T) {
	t.Parallel()
	for _, what := range chatQuestions {
		reader, index := newStubChat(), &stubChatSearch{}
		reader.err = &statelog.Refused{
			Code: statelog.RefuseBehind, Level: statelog.ReadStale,
			Detail: "this node is 40 000 records behind", RetryAfter: 12 * time.Second,
		}
		_, err := askChat(t, chatSources(t, reader, index, nil), what, chatAsked(what))
		if !errors.Is(err, queries.ErrUnavailable) {
			t.Errorf("%s on a node that is behind answered %v, want a retryable "+
				"refusal rather than an empty room", what, err)
		}
		if got := queries.RetryAfter(err); got != 12*time.Second {
			t.Errorf("%s hinted %s, want the refusal's own 12s", what, got)
		}
	}
}

// A QUESTION WITH NO SOURCE IS UNREGISTERED, not registered-and-empty: a
// company talking on Slack has no native rooms for this node to have a copy
// of, and a screen drawing "no conversations" would say the opposite of what
// is true. Search is gated on its own half, because a node still building an
// index holds every room and can rank no word.
func TestTheChatQuestionsAreAbsentWithoutTheirOwnSource(t *testing.T) {
	t.Parallel()
	for _, what := range chatQuestions {
		if _, err := askChat(t, chatSources(t, nil, nil, nil), what,
			chatAsked(what)); !errors.Is(err, queries.ErrUnknown) {

			t.Errorf("%s on a node with no native chat answered %v, want unknown",
				what, err)
		}
	}
	r := queries.NewRegistry()
	queries.Register(r, chatSources(t, newStubChat(), nil, nil))
	if _, err := r.Answer(t.Context(), "chat_search",
		map[string]any{"q": "x"}, "ops-1"); !errors.Is(err, queries.ErrUnknown) {

		t.Errorf("chat_search answered %v on a node with rooms and no index, "+
			"want unknown — the screen offers the rooms instead", err)
	}
	if _, err := r.Answer(t.Context(), "chat_channels", nil, "ops-1"); err != nil {
		t.Errorf("chat_channels: %v — the rooms answer without an index", err)
	}
}

// THE CALLER'S OWN FRESHNESS REACHES THE READ, whole.
//
// A dashboard is the one caller allowed to choose a level, because it renders
// the level and the lag beside the rows — and the floor beside it is what
// makes read-your-own-writes work: a person who has just posted names the
// position their write answered with and is served nothing from before it.
func TestTheCallersFreshnessReachesTheChatRead(t *testing.T) {
	t.Parallel()
	reader := newStubChat()
	if _, err := askChat(t, chatSources(t, reader, nil, nil), "chat_messages",
		map[string]any{
			"channel_id":   "c-eng",
			"read_level":   string(statelog.ReadSession),
			"min_position": chatAt.String(),
		}); err != nil {
		t.Fatalf("chat_messages: %v", err)
	}
	if reader.fresh.Level != statelog.ReadSession {
		t.Errorf("the read was served at %q, want the level the caller asked for",
			reader.fresh.Level)
	}
	if reader.fresh.MinPosition != chatAt {
		t.Errorf("the read's floor is %v, want the caller's own write at %v — "+
			"without it somebody who has just posted is shown the room from "+
			"before they did", reader.fresh.MinPosition, chatAt)
	}
}
