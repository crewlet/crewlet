// The chat read surface: who may ask, what travels into the read, and what
// comes back.
//
// EVERY CASE HERE IS ABOUT THIS LAYER and not about the reader underneath it.
// The rules about rooms, cursors and unread ranges are certified in
// internal/chatlet against a real estate; what is decided HERE is who the viewer
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

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
)

// chatQuestions is the whole family, so a sweep cannot silently stop covering
// one of them — the shape of the bug that let a question be registered with a
// different posture from its siblings.
var chatQuestions = []string{
	"chat_channels", "chat_channel", "chat_messages", "chat_thread",
	"chat_activity", "chat_threads", "chat_saved", "chat_sent",
	"chat_links", "chat_message", "chat_search", "chat_room_names", "chat_rooms",
}

// chatAsked is a parameter set per question, so one table can ask them all
// without a question being skipped for want of a room id.
func chatAsked(what string) map[string]any {
	switch what {
	case "chat_channel", "chat_messages", "chat_links":
		return map[string]any{"channel_id": cEng}
	case "chat_message":
		return map[string]any{"channel_id": cEng, "message_id": "m-1"}
	case "chat_thread":
		return map[string]any{"channel_id": cEng, "root_id": "m-1"}
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
var chatServed = chatlet.Served{
	Level: statelog.ReadStale, Complete: true, Position: chatAt,
}

// ---- the reader twin --------------------------------------------------- //

// THE SEAMS ARE THE SHIPPED IMPLEMENTATIONS, asserted at compile time.
//
// A consumer-defined interface is a claim about somebody else's type, and the
// only thing that checks it is the package that wires them together. Four
// lines here name the mismatch where the shape was chosen instead.
//
// THE ROUTER'S FACADES ARE WHAT A NODE HANDS THIS SURFACE ([estate.Chat],
// [estate.ChatSearch]), and the reader's own copy satisfies the reader seam
// too, because it is what the router answers from where this node's copy
// serves.
var (
	_ queries.ChatReader   = estate.Chat{}
	_ queries.ChatReader   = (*chatlet.Reader)(nil)
	_ queries.ChatSearcher = estate.ChatSearch{}
	_ queries.ChatReads    = (coord.Fleet)(nil)
)

// stubChat is chat's read side as this surface sees it.
//
// IT DECIDES VISIBILITY WITH [chatlet.Visible] — the shipped function, over rooms
// and memberships the case declares — rather than with a canned error, because
// the claim under test is that a room somebody may not read reaches a caller
// as NOT FOUND. A stub that returned whatever it was told would pass with the
// viewer dropped entirely.
type stubChat struct {
	rooms   map[string]chatlet.Channel
	members map[string][]string

	// What the surface asked for, which is the half a canned answer
	// hides: the viewer it resolved, the freshness it carried, and each
	// query it built.
	viewer   string
	fresh    statelog.Freshness
	named    string
	channels chatlet.ChannelsQuery
	page     chatlet.TranscriptQuery
	thread   chatlet.ThreadQuery
	activity chatlet.ActivityQuery
	saved    chatlet.SavedQuery

	// reads counts every call that reached this reader, so a case about a
	// refusal can prove nothing was read rather than that nothing was
	// returned.
	reads int

	listing    chatlet.ChannelListing
	transcript chatlet.Transcript
	replies    chatlet.Thread
	feed       chatlet.Activity
	detail     chatlet.ChannelDetail

	err error
}

// see is the visibility rule, applied exactly as the reader applies it: a room
// that is not there and a room this viewer may not read answer identically,
// because a private room's existence is itself information.
func (s *stubChat) see(viewer, id string) error {
	room, held := s.rooms[id]
	if !held || !chatlet.Visible(viewer, room, slices.Contains(s.members[id], viewer)) {
		return fmt.Errorf("%w: room %s", chatlet.ErrNotFound, id)
	}
	return nil
}

func (s *stubChat) Channels(_ context.Context, viewer string, q chatlet.ChannelsQuery,
	fresh statelog.Freshness) (chatlet.ChannelListing, error) {

	s.reads++
	s.viewer, s.fresh, s.channels = viewer, fresh, q
	return s.listing, s.err
}

func (s *stubChat) Channel(_ context.Context, viewer, channelID string,
	fresh statelog.Freshness) (chatlet.ChannelDetail, error) {

	s.reads++
	s.viewer, s.fresh = viewer, fresh
	if s.err != nil {
		return chatlet.ChannelDetail{}, s.err
	}
	if err := s.see(viewer, channelID); err != nil {
		return chatlet.ChannelDetail{}, err
	}
	out := s.detail
	out.Channel = s.rooms[channelID]
	out.Served = chatServed
	return out, nil
}

// ChannelNamed answers the room a name addresses, by the reader's own rule: a
// room this viewer may not read is not found, as one nobody holds is.
func (s *stubChat) ChannelNamed(ctx context.Context, viewer, name string,
	fresh statelog.Freshness) (chatlet.ChannelDetail, error) {

	s.named = name
	for id, room := range s.rooms {
		if chatlet.NormalizeName(room.Name) == name {
			return s.Channel(ctx, viewer, id, fresh)
		}
	}
	s.reads++
	return chatlet.ChannelDetail{}, fmt.Errorf("%w: room %s", chatlet.ErrNotFound, name)
}

func (s *stubChat) Messages(_ context.Context, viewer string, q chatlet.TranscriptQuery,
	fresh statelog.Freshness) (chatlet.Transcript, error) {

	s.reads++
	s.viewer, s.fresh, s.page = viewer, fresh, q
	if s.err != nil {
		return chatlet.Transcript{}, s.err
	}
	if err := s.see(viewer, q.ChannelID); err != nil {
		return chatlet.Transcript{}, err
	}
	out := s.transcript
	out.ChannelID, out.Served = q.ChannelID, chatServed
	return out, nil
}

func (s *stubChat) Thread(_ context.Context, viewer string, q chatlet.ThreadQuery,
	fresh statelog.Freshness) (chatlet.Thread, error) {

	s.reads++
	s.viewer, s.fresh, s.thread = viewer, fresh, q
	if s.err != nil {
		return chatlet.Thread{}, s.err
	}
	if err := s.see(viewer, q.ChannelID); err != nil {
		return chatlet.Thread{}, err
	}
	out := s.replies
	out.ChannelID, out.Served = q.ChannelID, chatServed
	return out, nil
}

// Directory answers every named room this viewer may browse, by the reader's
// own rule: a private room only to a member of it, with its member count and
// whether the viewer is in it.
func (s *stubChat) Directory(_ context.Context, viewer string,
	fresh statelog.Freshness) (chatlet.Directory, error) {

	s.reads++
	s.viewer, s.fresh = viewer, fresh
	if s.err != nil {
		return chatlet.Directory{}, s.err
	}
	out := chatlet.Directory{Served: chatServed, Rooms: []chatlet.DirectoryRoom{}}
	for _, id := range slices.Sorted(maps.Keys(s.rooms)) {
		if room := s.rooms[id]; room.Name != "" && s.see(viewer, id) == nil {
			out.Rooms = append(out.Rooms, chatlet.DirectoryRoom{Channel: room,
				Members: len(s.members[id]),
				Member:  slices.Contains(s.members[id], viewer)})
		}
	}
	return out, nil
}

// RoomNames answers every named room this viewer may read, by the reader's
// own rule: a private room only to a member of it.
func (s *stubChat) RoomNames(_ context.Context, viewer string,
	fresh statelog.Freshness) (chatlet.RoomNames, error) {

	s.reads++
	s.viewer, s.fresh = viewer, fresh
	if s.err != nil {
		return chatlet.RoomNames{}, s.err
	}
	out := chatlet.RoomNames{Served: chatServed, Rooms: []chatlet.RoomName{}}
	for _, id := range slices.Sorted(maps.Keys(s.rooms)) {
		if room := s.rooms[id]; room.Name != "" && s.see(viewer, id) == nil {
			out.Rooms = append(out.Rooms, chatlet.RoomName{ID: id, Name: room.Name,
				Kind: room.Kind})
		}
	}
	return out, nil
}

// stubChatSearch is the router's search as this surface sees it: asked FOR A
// VIEWER, answering what the serving node ranked and the rooms it covered.
//
// IT RECORDS THE QUERY rather than resolving rooms, because resolving them is
// the serving node's — certified in internal/estate against its own desk — and
// what this layer decides is only what it asks: the viewer it resolved, the
// caller's narrowing and level, and the page.
type stubChatSearch struct {
	query  estate.ChatQuery
	calls  int
	answer estate.ChatSearchAnswer
	err    error
}

func (s *stubChatSearch) Search(_ context.Context, q estate.ChatQuery) (
	estate.ChatSearchAnswer, error) {

	s.calls++
	s.query = q
	if s.err != nil {
		return estate.ChatSearchAnswer{}, s.err
	}
	out := s.answer
	if out.Hits == nil {
		out.Hits = []search.ChatHit{}
	}
	out.Position = chatAt
	return out, nil
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
// The stub's rooms, by the ids a room is minted under: a UUID, which is what
// tells a room's id from its name ([chatlet.RoomRef]).
const (
	cEng    = "6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e01"
	cSecret = "6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e02"
	cOurs   = "6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e03"
)

func chatRooms() (map[string]chatlet.Channel, map[string][]string) {
	rooms := map[string]chatlet.Channel{
		cEng:    {ID: cEng, Kind: chatlet.KindPublic, Name: "engineering"},
		cSecret: {ID: cSecret, Kind: chatlet.KindPrivate, Name: "acquisition"},
		cOurs:   {ID: cOurs, Kind: chatlet.KindPrivate, Name: "leads"},
	}
	members := map[string][]string{
		cSecret: {"bo"},
		cOurs:   {"ana", "bo"},
	}
	return rooms, members
}

func newStubChat() *stubChat {
	rooms, members := chatRooms()
	return &stubChat{
		rooms: rooms, members: members,
		listing: chatlet.ChannelListing{Served: chatServed},
	}
}

// chatSources wires the surface the way a node does: the company, the chart
// the authority table asks, the reader, the index and the coordination record.
func chatSources(t *testing.T, reader queries.ChatReader, index queries.ChatSearcher,
	reads queries.ChatReads) queries.Sources {

	t.Helper()
	cfg, err := config.ParseCompany([]byte(viewerCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	s := queries.Sources{
		Company: companySource(t, cfg),
		Chart:   flatChart{},
	}
	// A TYPED NIL IS NOT AN ABSENT SOURCE, so each is set only when the
	// case has one — a nil *stubChat in the interface would register the
	// family over nothing.
	if reader != nil {
		s.Chat = reader
	}
	if index != nil {
		s.ChatSearch = index
	}
	if reads != nil {
		s.ChatReads = reads
	}
	return s
}

// asAna is ana's own session: a person bound to her human seat, holding Chatlet
// and nothing else — so a case that passes is not passing on admin.
//
// HER LOGIN IS NOT HER HANDLE, as a person's never has to be: the viewer is the
// SEAT [iam.ActorFor] names her as, and a surface that read the login instead
// would be served as somebody nobody's rooms name.
func asAna(t *testing.T) context.Context {
	t.Helper()
	return iam.WithPrincipal(t.Context(), iam.Principal{
		ID: uuid.New(), Login: "ana.diaz", Kind: iam.KindPerson, Seat: "ana",
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantChatlet},
	})
}

// askChat asks one question as ana.
func askChat(t *testing.T, s queries.Sources, what string, params map[string]any) (any, error) {
	t.Helper()
	r := queries.NewRegistry()
	queries.Register(r, s)
	return r.Answer(asAna(t), what, params)
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
func chatReadsFor(t *testing.T, handle string, delta coord.ChatReadDelta) *coordmemory.Fleet {
	t.Helper()
	fleet := coordmemory.NewFleet()
	delta.At = time.Date(2031, 5, 6, 7, 8, 0, 0, time.UTC)
	if _, err := fleet.AdvanceChatRead(t.Context(), handle, delta); err != nil {
		t.Fatalf("seed %s's read state: %v", handle, err)
	}
	return fleet
}

// ---- who may ask ------------------------------------------------------- //

// A CREDENTIAL ACTING AS ITSELF READS NOTHING — reads included, public rooms
// included — and is told so by the AUTHORITY TABLE, in its own envelope.
//
// It holds every permission there is, which is the point: chat is read AS A
// SEAT, so no grant can give such a caller a room list to be shown, and the
// refusal is the seated rows' `unseated` with no grant named — the same
// refusal the chat tools give it, because both decide the same verbs. The
// remedy is a binding in the identity directory, which no permission names.
// And nothing is read first: who is asking decides every other answer.
func TestEveryChatQuestionRefusesACredentialActingAsItself(t *testing.T) {
	t.Parallel()
	for _, what := range chatQuestions {
		reader, index := newStubChat(), &stubChatSearch{}
		r := queries.NewRegistry()
		queries.Register(r, chatSources(t, reader, index, nil))
		// A PIPELINE'S TOKEN, bound to no seat and holding everything.
		_, err := r.Answer(asGrants(t, "pipeline", iam.AllGrants...), what, chatAsked(what))
		var refusal *queries.Refusal
		if !errors.As(err, &refusal) || refusal.Reason != authz.ReasonUnseated {
			t.Errorf("%s answered %v for a credential acting as itself, want "+
				"the table's %q refusal", what, err, authz.ReasonUnseated)
			continue
		}
		if !errors.Is(err, queries.ErrUnauthorized) {
			t.Errorf("%s refused %v, which no transport renders as a refusal", what, err)
		}
		if len(refusal.Grants) != 0 {
			t.Errorf("%s named %v as the remedy — no permission seats a "+
				"credential, so a refusal naming one sends its reader to ask "+
				"for something that changes nothing", what, refusal.Grants)
		}
		if reader.reads != 0 || index.calls != 0 {
			t.Errorf("%s read %d times and searched %d for a caller who is "+
				"nobody — the viewer is resolved before anything is read",
				what, reader.reads, index.calls)
		}
	}
}

// AND AN AGENT'S OWN PRINCIPAL IS NOT A PERSON READING, although the table
// seats it: the rail it would be answered is a person's — their read state, in
// a record the maintenance sweep forgets for every handle that is no person —
// and an agent reads its rooms through its own tools. No guard composes such a
// principal for this surface, which is why the refusal is the request's
// (`bad_params`, naming the binding) rather than a new code, and why it is
// checked rather than trusted: a viewer that is no person is shown nothing.
func TestAnAgentsOwnPrincipalReadsNoChatHere(t *testing.T) {
	t.Parallel()
	for _, what := range chatQuestions {
		reader, index := newStubChat(), &stubChatSearch{}
		r := queries.NewRegistry()
		queries.Register(r, chatSources(t, reader, index, nil))
		agent := iam.WithPrincipal(t.Context(), iam.Principal{
			ID: uuid.New(), Login: "seat:ana", Kind: iam.KindSeat, Seat: "ana",
			Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantChatlet},
		})
		_, err := r.Answer(agent, what, chatAsked(what))
		if !errors.Is(err, queries.ErrBadParams) ||
			!strings.Contains(queries.RefusalDetail(err), "crewlet iam bind") {
			t.Errorf("%s answered %v to an agent's own principal, want the "+
				"refusal naming the binding", what, err)
		}
		if reader.reads != 0 || index.calls != 0 {
			t.Errorf("%s reached the reader for a viewer that is no person", what)
		}
	}
}

// AND A SEAT WITHOUT CHATLET IS REFUSED NAMING IT, before any answer
// runs — the registry's posture, as every app's questions are guarded — while
// the same seat holding it is served. Without the second half the first would
// pass with every chat question refused to everybody.
func TestEveryChatQuestionNeedsTheChatApp(t *testing.T) {
	t.Parallel()
	for _, what := range chatQuestions {
		reader, index := newStubChat(), &stubChatSearch{}
		r := queries.NewRegistry()
		queries.Register(r, chatSources(t, reader, index, nil))
		_, err := r.Answer(asSeat(t, "ana", iam.GrantWorklet, iam.GrantDoclet),
			what, chatAsked(what))
		var refusal *queries.Refusal
		if !errors.As(err, &refusal) || refusal.Reason != authz.ReasonNoGrant ||
			!slices.Equal(refusal.Grants, []iam.Grant{iam.GrantChatlet}) {

			t.Errorf("%s answered %v to a seat without Chatlet, want "+
				"a refusal naming %q", what, err, iam.GrantChatlet)
		}
		if reader.reads != 0 || index.calls != 0 {
			t.Errorf("%s reached the reader without Chatlet", what)
		}
		if _, err := r.Answer(asAna(t), what, chatAsked(what)); err != nil {
			t.Errorf("%s refused ana holding Chatlet: %v", what, err)
		}
	}
}

// AND NOBODY ASKING IS NOT ASKED ABOUT AT ALL. Chat is the one read family
// with no anonymous form — the seat is resolved FROM the principal — so an
// anonymous caller never reaches a reader, whatever a deployment's anonymous
// read posture says about the rest of the surface.
func TestAnAnonymousCallerReadsNoChat(t *testing.T) {
	t.Parallel()
	reader, index := newStubChat(), &stubChatSearch{}
	r := queries.NewRegistry()
	queries.Register(r, chatSources(t, reader, index, nil))
	for _, what := range chatQuestions {
		if !slices.Contains(r.Names(), what) {
			t.Errorf("%s is not registered, so this case asserts nothing", what)
			continue
		}
		_, err := r.Answer(iam.WithAnonymous(t.Context()), what, chatAsked(what))
		if !errors.Is(err, queries.ErrUnauthenticated) {
			t.Errorf("%s answered %v with nobody asking, want the "+
				"unauthenticated refusal", what, err)
		}
	}
	if reader.reads != 0 || index.calls != 0 {
		t.Errorf("an anonymous caller reached the reader %d times and the "+
			"index %d", reader.reads, index.calls)
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
		map[string]any{"channel_id": cEng, "handle": "bo", "viewer": "bo", "as": "bo"})
	if err != nil {
		t.Fatalf("chat_messages: %v", err)
	}
	if reader.viewer != "ana" {
		t.Errorf("the read was served as %q; the session is ana's, and naming "+
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
	for _, what := range []string{"chat_channel", "chat_messages", "chat_thread"} {
		reader := newStubChat()
		sources := chatSources(t, reader, nil, nil)
		asked := chatAsked(what)

		asked["channel_id"] = cSecret
		_, err := askChat(t, sources, what, asked)
		if !errors.Is(err, queries.ErrNotFound) {
			t.Errorf("%s of a room ana is not in answered %v, want not found — "+
				"anything else confirms the room exists", what, err)
		}
		// AND THE SAME ROOM SHAPE, joined, is served: without this the
		// case above would pass with every private room refused.
		asked["channel_id"] = cOurs
		answered, err := askChat(t, sources, what, asked)
		if err != nil {
			t.Errorf("%s of a room ana is in: %v", what, err)
			continue
		}
		if got := chatJSON(t, answered, nil); got["position"] == nil {
			t.Errorf("%s answered %v without a position", what, got)
		}
	}
}

// A ROOM IS ASKED FOR BY ITS NAME AS BY ITS ID.
//
// A name is a room's address and what every sentence about it says, and the
// rail lists only the rooms somebody is in, so every question about one room
// takes its name too, written as people write it ("#Engineering"), by the one
// rule every surface reads a typed room by (chatlet.RoomRef): the room's own
// question answers the room with its id, and its transcript and a thread in
// it are read by the id the name resolves to. A private room the viewer is
// not in is not found by name exactly as by id. Mutation: send a name to the
// read by id, keep its '#', or answer a hidden room by name, and a case here
// fails.
func TestARoomIsAskedForByItsNameAsByItsID(t *testing.T) {
	t.Parallel()
	for _, typed := range []string{"engineering", "#Engineering", " #engineering "} {
		reader := newStubChat()
		got := askChatJSON(t, chatSources(t, reader, nil, nil), "chat_channel",
			map[string]any{"channel_id": typed})
		if channel, _ := got["channel"].(map[string]any); channel["id"] != cEng ||
			reader.named != "engineering" {
			t.Errorf("the room asked for as %q answered %v, read by the name %q", typed,
				got["channel"], reader.named)
		}
	}
	for _, what := range []string{"chat_messages", "chat_thread"} {
		reader := newStubChat()
		asked := chatAsked(what)
		asked["channel_id"] = "#Engineering"
		if _, err := askChat(t, chatSources(t, reader, nil, nil), what, asked); err != nil ||
			reader.page.ChannelID+reader.thread.ChannelID != cEng {
			t.Errorf("%s of the room asked for by its name read %q and %q (%v), want its id",
				what, reader.page.ChannelID, reader.thread.ChannelID, err)
		}
	}
	for _, what := range []string{"chat_channel", "chat_messages", "chat_thread"} {
		reader := newStubChat()
		sources := chatSources(t, reader, nil, nil)
		asked := chatAsked(what)
		asked["channel_id"] = "#acquisition"
		if _, err := askChat(t, sources, what, asked); !errors.Is(err, queries.ErrNotFound) {
			t.Errorf("%s of a private room ana is not in, asked for by its name, answered "+
				"%v, want not found as by its id", what, err)
		}
		asked["channel_id"] = "#leads"
		if _, err := askChat(t, sources, what, asked); err != nil {
			t.Errorf("%s of a private room ana is in, asked for by its name: %v", what, err)
		}
		asked["channel_id"] = "#"
		if _, err := askChat(t, sources, what, asked); !errors.Is(err, queries.ErrBadParams) {
			t.Errorf("%s of a room named by nothing answered %v, want the key refused", what,
				err)
		}
	}
}

// THE NAMES ARE EVERY ROOM THE VIEWER MAY READ, JOINED OR NOT, and a private
// room only to a member of it.
//
// What a reader tells a room written in prose by: ana is in neither the public
// room nor the private room she cannot read, and the answer names the public
// room and the private room she is in, each with its id and kind, read as the
// seat her session holds at the read's level. Mutation: answer the rail's rooms
// (her membership), or hand the reader no viewer, and this fails.
func TestTheRoomNamesAreTheRoomsTheViewerMayRead(t *testing.T) {
	t.Parallel()
	reader := newStubChat()
	got := askChatJSON(t, chatSources(t, reader, nil, nil), "chat_room_names",
		map[string]any{"read_level": "linearizable"})
	rooms, _ := got["rooms"].([]any)
	var named []string
	for _, room := range rooms {
		r, _ := room.(map[string]any)
		named = append(named, fmt.Sprintf("%s %s %s", r["id"], r["name"], r["kind"]))
	}
	want := []string{
		cEng + " engineering public",
		cOurs + " leads private",
	}
	if !slices.Equal(named, want) {
		t.Errorf("ana's room names are %v, want %v: every room she may read, the "+
			"public one she never joined included, and never a private room she "+
			"is outside", named, want)
	}
	if reader.viewer != "ana" || reader.fresh.Level != statelog.ReadLinearizable {
		t.Errorf("the names were read for %q at %q, want ana's seat at the level asked",
			reader.viewer, reader.fresh.Level)
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
	cursors := map[string]int64{cEng: 4096, cOurs: 12}
	reader.listing.Channels = []chatlet.ChannelSummary{{
		Channel: reader.rooms[cEng],
		Unread:  chatlet.UnreadLimit, UnreadCapped: true,
	}}
	reads := chatReadsFor(t, "ana", coord.ChatReadDelta{Cursors: cursors})
	// SOMEBODY ELSE'S STATE beside hers, so a surface reading the wrong
	// record is not rescued by there being only one.
	if _, err := reads.AdvanceChatRead(t.Context(), "bo", coord.ChatReadDelta{
		Cursors: map[string]int64{cEng: 1}, At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

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
	if n, _ := room["unread"].(float64); int(n) != chatlet.UnreadLimit {
		t.Errorf("unread = %v, want the counted range's cap of %d", room["unread"],
			chatlet.UnreadLimit)
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
// rail from coordination and is marked per row — and the whole state rides the
// listing itself, NAMED BY ITS SEAT and carrying its revision, because the rows
// mark only the rooms listed and a screen keeps the state it is handed against
// every later state of the same record by that revision.
func TestTheRailMarksTheRoomsThisViewerSilenced(t *testing.T) {
	t.Parallel()
	reader := newStubChat()
	reader.listing.Channels = []chatlet.ChannelSummary{
		{Channel: reader.rooms[cEng]},
		{Channel: reader.rooms[cOurs]},
	}
	until := time.Date(2031, 5, 6, 9, 0, 0, 0, time.UTC)
	reads := chatReadsFor(t, "ana", coord.ChatReadDelta{
		// A ROOM THE RAIL DOES NOT LIST, muted beside one it does: the
		// rows cannot carry it, and a screen that learned the mutes from
		// the rows alone would not know it.
		Cursors: map[string]int64{cEng: 7},
		Mute:    []string{cEng, "c-unlisted"}, DNDUntil: &until,
	})
	stored, err := reads.ChatRead(t.Context(), "ana")
	if err != nil {
		t.Fatal(err)
	}
	// THE RECONCILE'S STAMP, which moves the record and is its machinery.
	if _, err := reads.SetChatReadAbsence(t.Context(), "ana", until); err != nil {
		t.Fatal(err)
	}

	got := askChatJSON(t, chatSources(t, reader, nil, reads), "chat_channels", nil)

	rooms, _ := got["channels"].([]any)
	if len(rooms) != 2 {
		t.Fatalf("the rail carried %d rooms, want 2", len(rooms))
	}
	first, _ := rooms[0].(map[string]any)
	second, _ := rooms[1].(map[string]any)
	if first["muted"] != true {
		t.Errorf("#engineering is %v, want muted: this viewer silenced it", first)
	}
	if _, marked := second["muted"]; marked {
		t.Errorf("#leads carried a mute flag (%v) and nobody muted it", second)
	}
	read, _ := got["read"].(map[string]any)
	if read == nil {
		t.Fatalf("the rail carried no read state: %v", got)
	}
	if read["handle"] != "ana" {
		t.Errorf("read.handle = %v, want the seat the state was read for", read["handle"])
	}
	if muted, _ := read["muted"].([]any); len(muted) != 2 || !slices.Contains(muted, any("c-unlisted")) {
		t.Errorf("read.muted = %v, want both rooms ana muted, the unlisted one "+
			"included", read["muted"])
	}
	if read["dnd_until"] != until.Format(time.RFC3339) {
		t.Errorf("read.dnd_until = %v, want %s", read["dnd_until"],
			until.Format(time.RFC3339))
	}
	cursors, _ := read["cursors"].(map[string]any)
	if n, _ := cursors[cEng].(float64); n != 7 {
		t.Errorf("read.cursors = %v, want the cursor the badges were counted against",
			read["cursors"])
	}
	// THE REVISION IS THE ORDER a screen keeps it against the pushes by —
	// read AFTER the absence stamp moved the record, so the record's own.
	restamped, err := reads.ChatRead(t.Context(), "ana")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := read["revision"].(float64); uint64(n) != restamped.Revision ||
		restamped.Revision <= stored.Revision {

		t.Errorf("read.revision = %v, want the record's %d", read["revision"],
			restamped.Revision)
	}
	if _, stamped := read["absent_since"]; stamped {
		t.Errorf("the rail carries the reconcile's absence stamp: %v", read)
	}
}

// AN UNREADABLE CURSOR IS NOT A CURSOR AT ZERO.
//
// The rooms, their order and their previews are the log's and are still true,
// so a coordination store that cannot be reached must not take the whole
// screen away — and it must not badge every room in the company with its
// entire history either, which is what the reader's own arithmetic produces
// for somebody who has read nothing. The answer says which of the three it is.
// A node with no record to ask is the same answer, never a fabricated one.
func TestTheRailSaysWhenTheBadgesCouldNotBeMeasured(t *testing.T) {
	t.Parallel()
	for name, reads := range map[string]*unreadableReads{
		"a store that did not answer": {},
		"no store at all":             nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reader := newStubChat()
			reader.listing.Channels = []chatlet.ChannelSummary{{
				Channel: reader.rooms[cEng],
				// What the reader answers with no cursor: the room's
				// whole tail, capped.
				Unread: chatlet.UnreadLimit, UnreadCapped: true,
			}}
			var source queries.ChatReads
			if reads != nil {
				source = reads
			}

			got := askChatJSON(t, chatSources(t, reader, nil, source), "chat_channels", nil)

			if reads != nil && reads.calls == 0 {
				t.Fatal("the read state was never asked for, so this case asserts nothing")
			}
			if got["read_state"] != false {
				t.Errorf("read_state = %v, want false: nobody could look", got["read_state"])
			}
			// AND NO STATE IS OFFERED IN ITS PLACE: an empty one is a
			// person who has read nothing and muted nothing, which is not
			// what anybody found out.
			if _, said := got["read"]; said {
				t.Errorf("an unreadable read state was answered as %v", got["read"])
			}
			rooms, _ := got["channels"].([]any)
			if len(rooms) != 1 {
				t.Fatalf("the rail carried %d rooms, want the room itself: the "+
					"rooms are the log's and are still true", len(rooms))
			}
			room, _ := rooms[0].(map[string]any)
			if n, _ := room["unread"].(float64); n != 0 {
				t.Errorf("unread = %v with no cursor to count from — a client "+
					"that ignores read_state must under-badge rather than tell "+
					"everybody they are a hundred behind everywhere", room["unread"])
			}
			if _, capped := room["unread_capped"]; capped {
				t.Errorf("the row still claims a capped count: %v", room)
			}
		})
	}
}

// THE CREATE FORM OPENS ON THE COMPANY'S OWN DEFAULT, read from the running
// document per question: a visibility selector that opened on "public" while
// the company's rooms default to private would be a privacy control a person
// is misled about.
func TestTheRailCarriesTheCompanysDefaultVisibility(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"": false,
		"chat:\n  chatlet:\n    default_channel_private: true\n": true,
	} {
		cfg, err := config.ParseCompany([]byte(viewerCompany + name))
		if err != nil {
			t.Fatalf("parse %q: %v", name, err)
		}
		s := chatSources(t, newStubChat(), nil, nil)
		s.Company = companySource(t, cfg)
		got := askChatJSON(t, s, "chat_channels", nil)
		if private, _ := got["default_private"].(bool); private != want {
			t.Errorf("with %q, default_private = %v, want %v", name,
				got["default_private"], want)
		}
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
	reader.feed.NextCursor = chatAt.String()
	sources := chatSources(t, reader, nil, nil)

	page := askChatJSON(t, sources, "chat_messages", map[string]any{"channel_id": cEng})
	next, _ := page["next_cursor"].(string)
	if next != "41" {
		t.Fatalf("next_cursor = %q, want the reader's own", next)
	}
	if _, err := askChat(t, sources, "chat_messages",
		map[string]any{"channel_id": cEng, "cursor": next}); err != nil {
		t.Fatalf("the second page: %v", err)
	}
	if reader.page.Cursor != next {
		t.Errorf("the second page asked for cursor %q, want %q — a cursor this "+
			"surface rewrites is a page with a gap in it", reader.page.Cursor, next)
	}

	if _, err := askChat(t, sources, "chat_thread", map[string]any{
		"channel_id": cEng, "root_id": "m-1", "cursor": "17",
	}); err != nil {
		t.Fatalf("chat_thread: %v", err)
	}
	if reader.thread.Cursor != "17" || reader.thread.RootID != "m-1" {
		t.Errorf("the thread was asked %+v, want root m-1 from cursor 17", reader.thread)
	}

	feed := askChatJSON(t, sources, "chat_activity", nil)
	from, _ := feed["next_cursor"].(string)
	if from != chatAt.String() {
		t.Fatalf("Activity's next_cursor = %q, want a log position", from)
	}
	if _, err := askChat(t, sources, "chat_activity",
		map[string]any{"cursor": from}); err != nil {
		t.Fatalf("the second page of Activity: %v", err)
	}
	if reader.activity.Cursor != from {
		t.Errorf("Activity resumed from %q, want %q", reader.activity.Cursor, from)
	}
}

// AND A PAGE IS BOUNDED AT BOTH ENDS, BY THE DOMAIN'S OWN PAIR. An absent
// limit is a screen and not nothing; a limit past the ceiling is served the
// ceiling, because a page size is not a filter and answering less than was
// asked for costs a reader nothing they can act on.
func TestAPageTakesTheDomainsOwnDefaultAndCeiling(t *testing.T) {
	t.Parallel()
	reader := newStubChat()
	sources := chatSources(t, reader, nil, nil)

	for what, limit := range map[string]func() int{
		"chat_messages": func() int { return reader.page.Limit },
		"chat_thread":   func() int { return reader.thread.Limit },
		"chat_activity": func() int { return reader.activity.Limit },
	} {
		if _, err := askChat(t, sources, what, chatAsked(what)); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if got := limit(); got != chatlet.DefaultLimit {
			t.Errorf("%s with no limit asked for %d, want a screenful (%d)",
				what, got, chatlet.DefaultLimit)
		}
		asked := chatAsked(what)
		asked["limit"] = 5000
		if _, err := askChat(t, sources, what, asked); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if got := limit(); got != chatlet.MaxLimit {
			t.Errorf("%s with a limit of 5000 asked for %d, want the ceiling %d",
				what, got, chatlet.MaxLimit)
		}
	}
}

// A CURSOR THIS DOMAIN REFUSES IS THE CALLER'S TO FIX, and the refusal's own
// text is what says how: it names what a cursor looks like and where the last
// answer put one. Classified as a failure it would be a 500 telling somebody
// the server is broken over a value they pasted.
func TestARefusedCursorReachesTheCallerAsABadRequest(t *testing.T) {
	t.Parallel()
	reader := newStubChat()
	reader.err = &chatlet.InvalidError{Field: "cursor",
		Why: fmt.Sprintf("%q is not a message number", "yesterday")}

	_, err := askChat(t, chatSources(t, reader, nil, nil), "chat_messages",
		map[string]any{"channel_id": cEng, "cursor": "yesterday"})
	if !errors.Is(err, queries.ErrBadParams) {
		t.Fatalf("a refused cursor answered %v, want a bad request", err)
	}
	if !strings.Contains(queries.RefusalDetail(err), "is not a message number") {
		t.Errorf("the refusal reached the caller as %q, losing the sentence "+
			"that says what a cursor is", queries.RefusalDetail(err))
	}
	// IN CHAT'S SENTENCE, never its Go error: "chatlet: invalid: cursor: …"
	// reached the screen the question was asked from.
	if detail := queries.RefusalDetail(err); strings.Contains(detail, "chatlet:") ||
		strings.Contains(detail, "invalid:") {
		t.Errorf("the refusal reached the caller as %q, a package's words", detail)
	}
}

// A QUESTION ABOUT A ROOM NAMES ONE, and the refusal names the key the domain
// itself reads, so a caller told what is missing sends the key that is read.
func TestARoomQuestionWithNoRoomIsRefusedNamingTheKey(t *testing.T) {
	t.Parallel()
	for _, what := range []string{"chat_channel", "chat_messages", "chat_thread"} {
		reader := newStubChat()
		_, err := askChat(t, chatSources(t, reader, nil, nil), what,
			map[string]any{"channel_id": "  ", "root_id": "m-1"})
		if !errors.Is(err, queries.ErrBadParams) ||
			!strings.Contains(queries.RefusalDetail(err), "channel_id") {
			t.Errorf("%s with no room answered %v, want a refusal naming channel_id",
				what, err)
		}
		if reader.reads != 0 {
			t.Errorf("%s read a room that was never named", what)
		}
	}
	_, err := askChat(t, chatSources(t, newStubChat(), nil, nil), "chat_thread",
		map[string]any{"channel_id": cEng})
	if !errors.Is(err, queries.ErrBadParams) ||
		!strings.Contains(queries.RefusalDetail(err), "root_id") {
		t.Errorf("chat_thread with no root answered %v, want a refusal naming root_id", err)
	}
}

// ---- search ------------------------------------------------------------ //

// A SEARCH IS ASKED FOR THE SERVER'S VIEWER, with the caller's own narrowing
// and level and nothing else.
//
// The rooms are not this layer's to supply: which rooms somebody may read is an
// authorization fact, resolved by the data node that reads the index from the
// viewer handed to it. So what this surface decides — and what this asserts —
// is the viewer it resolved (never a handle the request names), the room a
// request narrows to (passed for the serving node to INTERSECT, never trusted
// here), the author and the text, the page — the INDEX'S own default and
// ceiling, which it clamps every query to — and the dashboard's own level for
// the visible set, with the floor beside it.
func TestASearchIsAskedForTheServersViewer(t *testing.T) {
	t.Parallel()
	index := &stubChatSearch{}
	if _, err := askChat(t, chatSources(t, newStubChat(), index, nil), "chat_search",
		map[string]any{
			"q": " acquisition ", "author": "bo", "channel_id": cSecret,
			"handle": "bo", "viewer": "bo",
			"read_level":   string(statelog.ReadSession),
			"min_position": chatAt.String(),
		}); err != nil {
		t.Fatalf("chat_search: %v", err)
	}
	q := index.query
	if q.Viewer != "ana" {
		t.Errorf("the search was asked for %q; the session is ana's, and naming "+
			"anybody else must change nothing", q.Viewer)
	}
	if q.Text != "acquisition" || q.Author != "bo" || q.Channel != cSecret {
		t.Errorf("the search was asked %+v, losing the caller's own narrowing", q)
	}
	if q.Fresh.Level != statelog.ReadSession ||
		!slices.Equal(q.Fresh.Floors, []statelog.Position{chatAt}) {
		t.Errorf("the visible set is read at %q from %v, want the level the "+
			"caller chose and the floor they named (%v)", q.Fresh.Level,
			q.Fresh.Floors, chatAt)
	}
	if q.Limit != search.ChatSearchLimit {
		t.Errorf("limit = %d, want the index's own default of %d", q.Limit,
			search.ChatSearchLimit)
	}

	if _, err := askChat(t, chatSources(t, newStubChat(), index, nil), "chat_search",
		map[string]any{"q": "acquisition", "limit": 5000}); err != nil {
		t.Fatalf("chat_search: %v", err)
	}
	if index.query.Limit != search.MaxChatSearchLimit {
		t.Errorf("a limit of 5000 asked for %d, want the index's ceiling %d — a "+
			"larger page here is one the index cuts without a word",
			index.query.Limit, search.MaxChatSearchLimit)
	}

	if _, err := askChat(t, chatSources(t, newStubChat(), index, nil), "chat_search",
		map[string]any{"q": "  "}); !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("a search for nothing answered %v, want a bad request", err)
	}
}

// A CHAT PHRASE PAST THE BOUND EVERY RANKED SEARCH SHARES IS REFUSED HERE,
// naming `q`, its size and the limit, in the class `knowledge` and
// `work_search` refuse with — and never asked of the serving node, whose own
// refusal would cross the router as a chat failure a screen cannot tell from a
// room it could not read. At the bound it is a search.
func TestALongChatSearchPhraseIsRefusedNamingTheLimit(t *testing.T) {
	t.Parallel()
	index := &stubChatSearch{}
	long := strings.Repeat("p", knowledge.MaxQueryBytes+1)
	_, err := askChat(t, chatSources(t, newStubChat(), index, nil), "chat_search",
		map[string]any{"q": long})
	if !errors.Is(err, queries.ErrBadParams) || !errors.Is(err, knowledge.ErrQueryTooLong) {
		t.Fatalf("a %d-byte phrase answered %v, want bad params of the "+
			"query-too-long class", len(long), err)
	}
	want := fmt.Sprintf("q is %d bytes, and a search takes at most %d", len(long),
		knowledge.MaxQueryBytes)
	if detail := queries.RefusalDetail(err); !strings.HasPrefix(detail, want) {
		t.Errorf("the refusal reads %q, want it to begin %q", detail, want)
	}
	if index.calls != 0 {
		t.Error("a refused phrase still reached the serving node")
	}
	if _, err := askChat(t, chatSources(t, newStubChat(), index, nil), "chat_search",
		map[string]any{"q": strings.Repeat("p", knowledge.MaxQueryBytes)}); err != nil {
		t.Errorf("a phrase at the bound was refused: %v", err)
	}
}

// THE ANSWER IS RENDERED, and it carries the serving node's denominator, its
// position and whether its index was still building.
//
// `searched` is what tells "nothing matched" from "nothing was looked at";
// `building` is what tells "nobody said that" from "the index has not
// reached it yet", which a reader acts on in opposite ways, and the note
// says it in words for a reader that shows sentences rather than flags; the
// instant is an instant a client can parse; each hit names its room as the
// rail does, by name and kind; and an empty answer is an empty list rather
// than null.
func TestASearchRendersWhatTheServingNodeRanked(t *testing.T) {
	t.Parallel()
	index := &stubChatSearch{answer: estate.ChatSearchAnswer{
		Searched: 2, Building: true,
		Hits: []search.ChatHit{{
			MessageID: "m-9", ChannelID: cEng, ThreadRoot: "m-1", Author: "bo",
			Snippet:   "…the acquisition closes Friday…",
			CreatedAt: time.Date(2031, 5, 6, 7, 9, 0, 0, time.UTC),
			Score:     3.5,
		}},
		Rooms: map[string]chatlet.ReadableRoom{
			cEng: {ID: cEng, Kind: chatlet.KindPublic, Name: "engineering"},
		},
	}}

	got := askChatJSON(t, chatSources(t, newStubChat(), index, nil), "chat_search",
		map[string]any{"q": "acquisition"})

	if n, _ := got["searched"].(float64); n != 2 {
		t.Errorf("searched = %v, want the serving node's 2 rooms", got["searched"])
	}
	if got["building"] != true {
		t.Errorf("building = %v, want the serving node's own true — without it "+
			"a half-built index reads as a company that never said the word",
			got["building"])
	}
	if note, _ := got["note"].(string); !strings.Contains(note, "still indexing chat") {
		t.Errorf("note = %v, want the building index said in words", got["note"])
	}
	hits, _ := got["hits"].([]any)
	if len(hits) != 1 {
		t.Fatalf("hits = %v, want the one the index ranked", got["hits"])
	}
	hit, _ := hits[0].(map[string]any)
	if hit["snippet"] != "…the acquisition closes Friday…" {
		t.Errorf("snippet = %v, want the index's own, bounded where it was cut", hit["snippet"])
	}
	if hit["at"] != "2031-05-06T07:09:00Z" {
		t.Errorf("at = %v, want the instant the message was posted", hit["at"])
	}
	if hit["thread_root"] != "m-1" || hit["channel_id"] != cEng ||
		hit["channel_name"] != "engineering" || hit["channel_kind"] != "public" ||
		hit["message_id"] != "m-9" || hit["author"] != "bo" {
		t.Errorf("the hit is %v; without the room and the thread a result "+
			"opens nowhere, or the room without the context the words were "+
			"said in", hit)
	}

	empty := askChatJSON(t, chatSources(t, newStubChat(), &stubChatSearch{}, nil),
		"chat_search", map[string]any{"q": "acquisition"})
	if hits, present := empty["hits"].([]any); !present || len(hits) != 0 {
		t.Errorf("hits = %v, want an empty list a client can count", empty["hits"])
	}
	if _, said := empty["building"]; said {
		t.Errorf("a search over a caught-up index said building: %v", empty)
	}
	if _, said := empty["note"]; said {
		t.Errorf("a search over a caught-up index carried a note: %v", empty)
	}
}

// A SEARCH NO DATA NODE COULD RUN IS A RETRY, NOT AN EMPTY RESULT, and a
// refused value is the caller's to fix in the domain's own words.
//
// "Nobody said that" is an answer a person acts on; a fleet with no copy to
// ask is a moment to wait, and the router says so by name rather than with no
// hits.
func TestASearchTheRouterCouldNotRunIsRefusedForWhatItIs(t *testing.T) {
	t.Parallel()
	for name, expect := range map[string]struct {
		err  error
		want error
	}{
		"no data node": {&estate.ErrUnserved{Detail: "data-a: out of service"},
			queries.ErrUnavailable},
		"a refused value": {fmt.Errorf("%w: author: not a handle", chatlet.ErrInvalid),
			queries.ErrBadParams},
	} {
		index := &stubChatSearch{err: expect.err}
		_, err := askChat(t, chatSources(t, newStubChat(), index, nil), "chat_search",
			map[string]any{"q": "acquisition"})
		if !errors.Is(err, expect.want) {
			t.Errorf("%s answered %v, want %v", name, err, expect.want)
		}
	}
}

// ---- what every answer carries ----------------------------------------- //

// A SCREEN THAT CANNOT SAY HOW FAR BEHIND IT IS RENDERS A STALE ROOM AS AN
// EMPTY ONE.
//
// So every answer here carries the position it was true as of — the five
// reader-backed ones from the read itself, and the search from the serving
// copy's own applied position — and the level it was served at.
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
		behind := &statelog.Refused{
			Code: statelog.RefuseBehind, Level: statelog.ReadStale,
			Detail: "this node is 40 000 records behind", RetryAfter: 12 * time.Second,
		}
		reader, index := newStubChat(), &stubChatSearch{err: behind}
		reader.err = behind
		_, err := askChat(t, chatSources(t, reader, index, nil), what, chatAsked(what))
		if !errors.Is(err, queries.ErrUnavailable) {
			t.Errorf("%s on a node that is behind answered %v, want a retryable "+
				"refusal rather than an empty room", what, err)
		}
		if got := statelog.RetryAfter(err, 5*time.Second); got != 12*time.Second {
			t.Errorf("%s hinted %s, want the refusal's own 12s", what, got)
		}
	}
}

// A QUESTION WITH NO SOURCE IS UNREGISTERED, not registered-and-empty: a
// company talking on Slack has no Chatlet rooms for this node to have a copy
// of, and a screen drawing "no conversations" would say the opposite of what
// is true. Search is gated on its own half, because a node holding every room
// and no index can rank no word.
func TestTheChatQuestionsAreAbsentWithoutTheirOwnSource(t *testing.T) {
	t.Parallel()
	for _, what := range chatQuestions {
		if _, err := askChat(t, chatSources(t, nil, nil, nil), what,
			chatAsked(what)); !errors.Is(err, queries.ErrUnknown) {

			t.Errorf("%s on a node with no Chatlet answered %v, want unknown",
				what, err)
		}
	}
	// AND THE INDEX ALONE REGISTERS NOTHING: a search is over rooms, and
	// a node with no rooms to show has no search to offer beside them.
	if _, err := askChat(t, chatSources(t, nil, &stubChatSearch{}, nil), "chat_search",
		map[string]any{"q": "x"}); !errors.Is(err, queries.ErrUnknown) {
		t.Errorf("chat_search answered %v with an index and no rooms, want unknown", err)
	}
	sources := chatSources(t, newStubChat(), nil, nil)
	if _, err := askChat(t, sources, "chat_search",
		map[string]any{"q": "x"}); !errors.Is(err, queries.ErrUnknown) {
		t.Errorf("chat_search answered %v on a node with rooms and no index, "+
			"want unknown — the screen offers the rooms instead", err)
	}
	if _, err := askChat(t, sources, "chat_channels", nil); err != nil {
		t.Errorf("chat_channels: %v — the rooms answer without an index", err)
	}
}

// THE CALLER'S OWN FRESHNESS REACHES EVERY CHAT READ, whole.
//
// A dashboard is the one caller allowed to choose a level, because it renders
// the level and the lag beside the rows — and the floor beside it is what
// makes read-your-own-writes work: a person who has just posted names the
// position their write answered with and is served nothing from before it.
// The tracker's and the wiki's questions are held to this by native_test.go's
// gates; this is chat's half of them, over every question in the family rather
// than the one somebody remembered — a key parsed and dropped answers
// identically, same rows and the same `read_level`, which is how the staleness
// bounds once shipped on every question but one.
func TestTheCallersFreshnessReachesEveryChatRead(t *testing.T) {
	t.Parallel()
	for _, what := range chatQuestions {
		t.Run(what, func(t *testing.T) {
			t.Parallel()
			// served asks once and answers the freshness the read was
			// handed — the reader's for a room, the router's search for
			// chat_search, which is asked for a viewer at a level of
			// its own.
			served := func(extra map[string]any) (statelog.Freshness, error) {
				t.Helper()
				reader, index := newStubChat(), &stubChatSearch{}
				asked := chatAsked(what)
				maps.Copy(asked, extra)
				_, err := askChat(t, chatSources(t, reader, index, nil), what, asked)
				if what == "chat_search" {
					return index.query.Fresh, err
				}
				return reader.fresh, err
			}

			if got, err := served(nil); err != nil || got.Level != statelog.ReadStale {
				t.Errorf("asking nothing read at %q (%v), want this surface's "+
					"default, stale", got.Level, err)
			}
			if got, err := served(map[string]any{"read_level": "linearizable"}); err != nil ||
				got.Level != statelog.ReadLinearizable {
				t.Errorf("asking linearizable read at %q (%v) — a level the answer "+
					"then reports as the one asked for is a stale answer wearing a "+
					"stronger name", got.Level, err)
			}
			// A BARE `session` IS REFUSED rather than quietly served: it
			// waits for a position, and none was named.
			if _, err := served(map[string]any{"read_level": "session"}); !errors.Is(err,
				queries.ErrBadParams) {
				t.Errorf("a bare read_level=session answered %v", err)
			}
			got, err := served(map[string]any{
				"read_level": "session", "min_position": chatAt.String(),
			})
			if err != nil || got.Level != statelog.ReadSession ||
				!slices.Equal(got.Floors, []statelog.Position{chatAt}) {
				t.Errorf("session from %v read at %q from %v (%v) — without the "+
					"floor somebody who has just posted is shown the room from "+
					"before they did", chatAt, got.Level, got.Floors, err)
			}
			got, err = served(map[string]any{"max_lag_seconds": "30", "max_lag_seq": "250"})
			if err != nil || got.MaxLag != 30*time.Second || got.MaxLagSeq != 250 {
				t.Errorf("the staleness bounds reached the read as %s and %d (%v), "+
					"want 30s and 250 records", got.MaxLag, got.MaxLagSeq, err)
			}
			// A BOUND AT A LEVEL THAT IS NOT STALE IS REFUSED, not
			// carried and ignored: it narrows nothing there.
			if _, err := served(map[string]any{
				"read_level": "linearizable", "max_lag_seconds": "30",
			}); !errors.Is(err, queries.ErrBadParams) {
				t.Errorf("a staleness bound at linearizable answered %v", err)
			}
			// EVERY REFUSAL NAMES ITS PARAMETER AND NO DOMAIN. The grammar
			// is the framework's: read through the tracker's, a person's
			// bad chat request was refused as "worklet: …".
			for param, bad := range map[string]map[string]any{
				"read_level":   {"read_level": "eventually"},
				"min_position": {"min_position": "4711"},
			} {
				_, err := served(bad)
				var refused *statelog.FreshnessError
				if !errors.As(err, &refused) || refused.Param != param {
					t.Errorf("%v was refused as %v, want a refusal of %s", bad, err, param)
					continue
				}
				for _, domain := range []string{"worklet:", "chatlet:", "doclet:"} {
					if strings.Contains(err.Error(), domain) {
						t.Errorf("%v was refused naming %q: %v", bad, domain, err)
					}
				}
			}
		})
	}
}

// ---- the viewer -------------------------------------------------------- //

// WHERE A COMPANY TALKS, WHEN IT IS A VENDOR'S CHAT, IS ANSWERED TO A HOLDER
// OF `chatlet`, and to nobody else.
//
// Whether the company runs the engine's own chat at all is every reader's, as
// the org projection's `services`; which vendors it talks on is the one
// further fact a screen naming where the company talks needs, and it is Chatlet's disclosure,
// held to who may read the app as every app's are: a
// caller without the grant is answered an empty list, never the vendors. A
// company on the engine's own chat or on none declares no vendor to name.
//
// Mutation: answer the vendors whatever the grant, or on a company whose chat
// is not a vendor's, and a row below fails.
func TestTheViewerNamesAVendorChatToAHolderOfChat(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, doc   string
		grants      []iam.Grant
		wantVendors []string
	}{
		{"chatlet, to a holder of chatlet", "", []iam.Grant{iam.GrantChatlet}, []string{}},
		{"none, to a holder of chat", "chat:\n  backend: none\n",
			[]iam.Grant{iam.GrantChatlet}, []string{}},
		{"a vendor, to a holder of chat", "integrations:\n  slack: {}\n",
			[]iam.Grant{iam.GrantChatlet}, []string{"slack"}},
		{"a vendor, to a seat without it", "integrations:\n  slack: {}\n",
			[]iam.Grant{iam.GrantWorklet}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.ParseCompany([]byte(viewerCompany + tc.doc))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			s := viewerSources(t, &stubWork{})
			s.Company = companySource(t, cfg)
			r := queries.NewRegistry()
			queries.Register(r, s)
			answered, err := r.Answer(asSeat(t, "ana", tc.grants...), "viewer", nil)
			got := chatJSON(t, answered, err)
			if _, present := got["chat_backend"]; present {
				t.Errorf("chat_backend = %v: whether the company runs the engine's chat "+
					"is the org projection's `services`, and a second answer drifts", got["chat_backend"])
			}
			vendors, present := got["chat_vendors"].([]any)
			if !present {
				t.Fatalf("chat_vendors = %v, want a list — never null", got["chat_vendors"])
			}
			names := make([]string, 0, len(vendors))
			for _, v := range vendors {
				names = append(names, fmt.Sprint(v))
			}
			if !slices.Equal(names, tc.wantVendors) {
				t.Errorf("chat_vendors = %v, want %v", names, tc.wantVendors)
			}
		})
	}
}

// THE RAIL IS ASKED FOR THE TOP OF IT, and for only the rooms with something
// unread, as the caller says: `limit` reaches the reader within the rail's own
// cap (every room when absent, which is what the screen draws), `unread`
// reaches it beside the person's read state, and `unread` over a read state
// nobody could read is refused unavailable without a read, since the counts
// zeroed for it would answer that nothing is unread. Mutation: drop either
// parameter on its way to the reader, let the limit past the cap, or answer
// `unread` from unmeasured counts, and a case here fails.
func TestTheRailIsAskedForItsTopAndForTheRoomsWithSomethingUnread(t *testing.T) {
	t.Parallel()
	reads := chatReadsFor(t, "ana", coord.ChatReadDelta{Cursors: map[string]int64{cEng: 7}})
	for _, c := range []struct {
		params map[string]any
		limit  int
		unread bool
	}{
		{nil, chatlet.MaxRailChannels, false},
		{map[string]any{"limit": "20", "unread": "true"}, 20, true},
		{map[string]any{"limit": "100000"}, chatlet.MaxRailChannels, false},
	} {
		reader := newStubChat()
		if _, err := askChat(t, chatSources(t, reader, nil, reads), "chat_channels",
			c.params); err != nil {
			t.Fatalf("%v: %v", c.params, err)
		}
		if reader.channels.Limit != c.limit || reader.channels.Unread != c.unread {
			t.Errorf("asked with %v, the reader was asked for %d rooms, unread %v; want %d, %v",
				c.params, reader.channels.Limit, reader.channels.Unread, c.limit, c.unread)
		}
	}

	reader := newStubChat()
	_, err := askChat(t, chatSources(t, reader, nil, &unreadableReads{}), "chat_channels",
		map[string]any{"unread": "true"})
	if !errors.Is(err, queries.ErrUnavailable) || reader.reads != 0 {
		t.Errorf("the rooms with something unread over an unreadable read state answered %v "+
			"after %d reads, want unavailable and none", err, reader.reads)
	}
}

// fullRail is a reader whose person is in as many rooms as the rail answers,
// every room at every field's cap, each with a last message at its cap: the
// largest rail there is, in characters the answer's encoding writes as
// themselves (the step's bytes left over are markup's, which it writes as six
// bytes to a character). It answers the top of it the query's limit asks for,
// as the reader does ([chatlet.ChannelsQuery.Limit]).
type fullRail struct{ *stubChat }

func (f fullRail) Channels(_ context.Context, viewer string, q chatlet.ChannelsQuery,
	fresh statelog.Freshness) (chatlet.ChannelListing, error) {

	f.channels = q
	word := func(n int) string { return strings.Repeat("w", n) }
	at := time.Date(2031, 5, 6, 7, 8, 0, 0, time.UTC)
	var out chatlet.ChannelListing
	out.Served = chatServed
	for i := range chatlet.MaxRailChannels {
		if q.Limit > 0 && i == q.Limit {
			out.Truncated = true
			break
		}
		days := 30
		out.Channels = append(out.Channels, chatlet.ChannelSummary{
			Channel: chatlet.Channel{V: 1, ID: uuid.NewString(), Kind: chatlet.KindPublic,
				Name: word(chatlet.MaxChannelName), Topic: word(chatlet.MaxTopic),
				Purpose: word(chatlet.MaxPurpose), RetentionDays: &days, CreatedAt: at,
				CreatedBy: word(64), CreatedByKind: chatlet.AuthorAgent},
			Revision: 1 << 40, FollowAll: true, Unread: chatlet.UnreadLimit, UnreadCapped: true,
			Last: &chatlet.MessagePreview{MessageID: uuid.NewString(), Author: word(64),
				AuthorKind: chatlet.AuthorAgent, Excerpt: word(chatlet.MaxExcerpt),
				ChannelSeq: 1 << 40, At: at, Position: statelog.Position{
					Stream: "CREWLET_CHAT_LOG", Generation: 1 << 20, Seq: 1 << 40}},
		})
	}
	return out, nil
}

// THE ASSISTANT'S PAGE OF ROOMS IS READ WHOLE IN ONE STEP (ADR-0061): the rooms
// list_chat_rooms asks for by default, from the largest rail there is (as many
// rooms as it answers, every one at every field's cap) beside the person's
// whole read state at its own caps, encode within what one step reads. The
// rail answers every room whole, so the read asking for all of them was past
// a step's bound for a person in about sixty rooms, cut, and answered nothing.
// Mutation: let list_chat_rooms ask for the whole rail, or raise its page past
// what fits, and this fails.
func TestTheAssistantsPageOfRoomsIsReadInOneStep(t *testing.T) {
	t.Parallel()
	rooms, _ := catalog.Lookup("list_chat_rooms")
	req, err := rooms.Request(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	params := map[string]any{}
	for name := range req.Query {
		params[name] = req.Query.Get(name)
	}
	cursors := map[string]int64{}
	var muted []string
	for i := range coord.MaxReadCursors {
		id := uuid.NewString()
		cursors[id] = 1<<62 + int64(i)
		muted = append(muted, id)
	}
	dnd := time.Date(2031, 5, 6, 9, 0, 0, 0, time.UTC)
	reads := chatReadsFor(t, "ana", coord.ChatReadDelta{Cursors: cursors, Mute: muted,
		DNDUntil: &dnd})
	answer, err := askChat(t, chatSources(t, fullRail{newStubChat()}, nil, reads),
		"chat_channels", params)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > exec.MaxResponseBytes {
		t.Errorf("list_chat_rooms asks %s?%s, and the largest answer to it is %d bytes, past "+
			"the %d one step reads", req.Path, req.Query.Encode(), len(raw), exec.MaxResponseBytes)
	}
	t.Logf("the largest default page of rooms is %d bytes of %d", len(raw), exec.MaxResponseBytes)
}
