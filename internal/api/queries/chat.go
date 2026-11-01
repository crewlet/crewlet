// The company's own conversation, read for a person.
//
// # The viewer is the PRINCIPAL's seat, and a caller may never name one
//
// Every other personal question here takes a handle and falls back to the
// caller's own record — see [Sources.recordHandle], where naming somebody
// else's is what a lead's or an administrator's authority buys. Chat has no
// such arm, and the difference is deliberate: a transcript is the most
// sensitive thing a deployment holds, so who is asking is the request's
// principal and there is nowhere to write a handle at all. A `handle=` on one
// of these requests is an ignored parameter, not a filter.
//
// The viewer is the seat [iam.ActorFor] names the principal as — the SAME
// function a chat write's author comes from, so whoever may post in a room is
// exactly whoever is shown it — and every chat verb is SEATED: a credential
// acting as itself (a Tier A token, a service account bound to no seat) is
// refused by the authority table's own row, `unauthorized` with reason
// `unseated` and no grant named, as the chat tools refuse it. The remedy is a
// binding in the identity directory (`crewlet iam bind`), not a credential. It
// is never derived from the principal's record owner: for an unbound machine
// that is its LOGIN, and [chatlet.Visible] reads any non-empty viewer as somebody
// who may read every public room.
//
// # The rooms come from the log and the badges do not
//
// A room, its membership and its messages are the state log's, read through
// the estate router — this node's own copy where it serves, a data node's
// otherwise — so every answer here carries the position it was true as of,
// and a copy that has not caught up says so rather than drawing an empty room.
// Where each person's eye has reached, what they have muted and whether they
// are in do-not-disturb is NOT on that log: it is one reader's attention, it
// lives in coordination, and it reaches the rail as a value
// ([chatlet.ChannelsQuery.Cursors]).
//
// That is why the rail's answer carries `read_state`. A coordination store
// that could not be read leaves the badges UNKNOWN, which is a third state
// beside "read" and "unread" — and reporting an unknown cursor as a cursor at
// zero would badge every room in the company with its whole history the moment
// a bucket read failed.

package queries

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
)

// ChatReader is chat's read side as this surface calls it.
//
// Consumer-defined and kept to what these questions ask, like every other seam
// here. THE FRESHNESS IS PART OF EVERY CALL rather than a property of the
// reader, for [PageReader]'s reason: a dashboard poll is the one caller
// allowed to choose its own level.
//
// A node hands it the ESTATE ROUTER'S facade ([estate.Chat]), resolved per
// question, never its own copy: a data node whose copy is out of service would
// otherwise go on drawing its operator's rooms from the rows it stopped
// serving its seats from.
type ChatReader interface {
	Channels(ctx context.Context, viewer string, q chatlet.ChannelsQuery,
		fresh statelog.Freshness) (chatlet.ChannelListing, error)
	Channel(ctx context.Context, viewer, channelID string,
		fresh statelog.Freshness) (chatlet.ChannelDetail, error)
	ChannelNamed(ctx context.Context, viewer, name string,
		fresh statelog.Freshness) (chatlet.ChannelDetail, error)
	Messages(ctx context.Context, viewer string, q chatlet.TranscriptQuery,
		fresh statelog.Freshness) (chatlet.Transcript, error)
	Thread(ctx context.Context, viewer string, q chatlet.ThreadQuery,
		fresh statelog.Freshness) (chatlet.Thread, error)
	RoomNames(ctx context.Context, viewer string,
		fresh statelog.Freshness) (chatlet.RoomNames, error)
	Directory(ctx context.Context, viewer string,
		fresh statelog.Freshness) (chatlet.Directory, error)

	// The reader's own views (chatreader.go).
	ChatReaderViews
}

// ChatSearcher is the keyword search over the company's chat, as this surface
// asks it: FOR A VIEWER, never over a set of rooms.
//
// WHICH ROOMS SOMEBODY MAY READ IS AN AUTHORIZATION FACT, and it is resolved
// by the data node that answers ([estate.ChatSearch]) from the viewer this
// surface resolved — on the copy whose index it then reads, in one operation.
// Resolved here and handed over, it would be a set read from one copy and
// searched on another's index, and a seam that took the rooms would be a seam
// a caller could hand the wrong ones. What this surface keeps is the request's
// own parts: the level the visible set is read at, which is the DASHBOARD's
// rather than a seat's (a seat reads at `linearizable`, which would put a
// quorum barrier on every keystroke of a search box), and a narrowing to one
// room, which the serving node intersects with what the viewer may read.
type ChatSearcher interface {
	Search(ctx context.Context, q estate.ChatQuery) (estate.ChatSearchAnswer, error)
}

// ChatReads is where one person's eye has reached, as this surface needs it.
//
// The READ half of [coord.ChatReads] and nothing else: a flush is a write, and
// a person's writes go through the operator catalogue's `mark_chat_read`.
// Nil is "this process cannot say", which the rail reports rather than
// fabricating a cursor at zero for every room.
type ChatReads interface {
	ChatRead(ctx context.Context, handle string) (coord.ChatReadState, error)
}

// errNoChatSeat is a chat question asked by a principal the table admitted and
// [iam.ActorFor] names as no person's seat — which the seated rows make
// unreachable for every principal the guard composes, and which is refused
// rather than trusted if it is reached: a viewer that is no seat is shown
// nothing, never everything.
var errNoChatSeat = fmt.Errorf("%w: chat is read as a seat, and this "+
	"credential isn't bound to one. Bind it to a human seat with `crewlet iam "+
	"bind`", ErrBadParams)

// chatViewer is the seat a chat question is answered for: the authority
// table's decision of action for the request's principal — the app and the
// SEATED bar, refused in the table's own envelope — and then the seat
// [iam.ActorFor] names it as.
//
// AT THE TOP OF EVERY CHAT QUESTION, before a parameter is read and before any
// store is touched, because the answer to "who is asking" decides every other
// answer on this surface — which rooms exist, whose unread counts these are,
// and which transcripts may be searched at all.
func (s Sources) chatViewer(ctx context.Context, action authz.Action) (string, error) {
	principal, how := iam.From(ctx)
	if how == iam.Unknown {
		return "", unresolved(ctx, string(action))
	}
	d := authz.Decide(ctx, principal, action, authz.Object{Kind: authz.KindChannel},
		s.Chart, s.clock())
	switch {
	case d.Unknown():
		return "", fmt.Errorf("%w: this node cannot decide %s yet: %w",
			ErrUnavailable, action, d.Err)
	case !d.Allowed:
		return "", NewRefusal(string(action), d)
	}
	actor := iam.ActorFor(principal)
	if actor.Kind != iam.ActorHuman || actor.Name == "" {
		return "", errNoChatSeat
	}
	return actor.Name, nil
}

// chatFailure classifies one chat read's failure for the transports.
//
// TWO OUTCOMES THAT ARE NOT FAILURES OF THIS NODE, each acted on differently:
// a room that is not there — or that this viewer may not see, which
// [chatlet.Reader] deliberately answers identically — is a dead link; and a cursor
// or a room id this surface refused is a request to change. Everything else is
// left to the registry's own classification, which makes a node behind its log
// or a fleet with no copy to answer from `unavailable`.
//
// [chatlet.ErrForbidden] is NOT mapped here, and its absence is the point: the
// only read that raises it is one with an empty viewer, which [chatViewer]
// refuses before any reader is called.
func chatFailure(err error) error {
	switch {
	case errors.Is(err, chatlet.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, chatlet.ErrInvalid):
		// THE TEXT SURVIVES, because it is written for the caller: it
		// says what a cursor looks like and where the last answer put
		// one.
		return refusedBy(err, chatlet.Sentence(err))
	}
	return err
}

// chatChannelID reads the room a question is about.
//
// `channel_id` RATHER THAN `channel`, because the domain's own refusals name
// the field: [chatlet.Reader] answers a read with no room with `channel_id: a read
// names no room`, and those sentences reach the caller unchanged. It is also
// the name every answer uses for the same value, so what a client reads off a
// transcript is what it sends back.
func chatChannelID(p Params) (string, error) {
	id := strings.TrimSpace(p.String("channel_id"))
	if id == "" {
		return "", badParams("channel_id", "", nil)
	}
	return id, nil
}

// chatRoomID is the room a question names as its id: the id itself, or the
// room a name addresses ([chatlet.RoomRef]), read through the reader's by-name
// read, which answers a room this viewer may not read exactly as one nobody
// holds. A transcript and a thread are each read by the id, so a room named as
// people write it is resolved first, as the room's own question resolves it.
func (s Sources) chatRoomID(ctx context.Context, who, ref string,
	fresh statelog.Freshness) (string, error) {

	switch id, name := chatlet.RoomRef(ref); {
	case id != "":
		return id, nil
	case name != "":
		detail, err := s.Chat.ChannelNamed(ctx, who, name, fresh)
		if err != nil {
			return "", chatFailure(err)
		}
		return detail.Channel.ID, nil
	}
	return "", badParams("channel_id", ref, nil)
}

// chatPage bounds a transcript, a thread or a feed page — by
// [chatlet.DefaultLimit] and [chatlet.MaxLimit] rather than numbers of this
// surface's own, because the reader clamps to exactly these, and a second pair
// here would be two ceilings for one page.
func chatPage(p Params) int {
	return Clamp(p.Int("limit", 0), chatlet.DefaultLimit, chatlet.MaxLimit)
}

// ---- the rail ---------------------------------------------------------- //

// chatRoom is one room as the rail renders it: the reader's own summary, plus
// the one fact about it that is not on the log.
//
// EMBEDDED rather than restated field by field: a row rebuilt here would be a
// second spelling of the reader's own shape, and the copy that drifts is always
// the one a screen is actually drawing.
type chatRoom struct {
	chatlet.ChannelSummary

	// Muted says this viewer silenced the room. It suppresses NOTICE and
	// never delivery: the messages are there, they are searchable, and a
	// seat's wake is not affected by anybody's mute.
	Muted bool `json:"muted,omitempty"`
}

// chatRail is one person's rooms, and what this node could establish about
// them.
type chatRail struct {
	chatlet.Served

	Channels []chatRoom `json:"channels"`

	// Truncated says the rail left rooms out: this person is in more rooms
	// than [chatlet.MaxRailChannels], or more matched than the `limit` asked
	// for — the reader's own flag, carried rather than inferred from the
	// length.
	Truncated bool `json:"truncated,omitempty"`

	// Unreadable is how many of this person's rooms this build could not
	// present, a room a newer peer wrote included.
	Unreadable int `json:"unreadable,omitempty"`

	// DefaultPrivate is what a room created WITHOUT a stated visibility
	// will be, from `chat.chatlet.default_channel_private` — on the rail
	// because the create form opens out of it, and THE SCREEN NEEDS IT TO
	// BE HONEST: a visibility selector that opened on "public" while the
	// company default was private would be a privacy control a person is
	// misled about. The SERVER still decides, so a stale value changes what
	// the form shows and never what it gets.
	DefaultPrivate bool `json:"default_private,omitempty"`

	// ReadState says the badges on these rows are a MEASUREMENT. False
	// means the coordination record could not be read — the counts are
	// then zero, the mutes absent and Read with them, which is not "you
	// have read everything" but "nobody could look".
	ReadState bool `json:"read_state"`

	// Read is this person's WHOLE read state, as the badges were counted
	// against it, NAMED BY ITS SEAT ([coord.PersonChatRead]) and carried
	// with its revision — so a screen keeps it, or a pushed state, by the
	// order it keeps any two states of one record, and replaces it rather
	// than compares when the seat it reads as is another. Absent when
	// ReadState is false. The reconcile's absence stamp is its machinery,
	// and is not answered.
	Read *coord.PersonChatRead `json:"read,omitempty"`
}

// chatChannels answers the viewer's own rooms, newest activity first: every
// one, or with `limit` the most recently active that many, and with `unread`
// only those with something unread.
//
// THE CURSORS TRAVEL INTO THE READ rather than the badge being counted
// afterwards, which is what makes an unread count cost a bounded range scan
// per room: the reader counts above this person's own cursor and stops at
// [chatlet.UnreadLimit].
//
// `unread` OVER A READ STATE NOBODY COULD READ IS REFUSED, unavailable: the
// counts are unknown, and answering the rooms with something unread out of
// counts zeroed for it would say nothing is unread, which is a different
// answer, and a wrong one.
func (s Sources) chatChannels(ctx context.Context, p Params) (any, error) {
	who, err := s.chatViewer(ctx, authz.ActionChatList)
	if err != nil {
		return nil, err
	}
	fresh, err := Freshness(p)
	if err != nil {
		return nil, err
	}
	state, measured := s.chatReadState(ctx, who)
	unread := p.Bool("unread", false)
	if unread && !measured {
		return nil, fmt.Errorf("%w: which rooms have unread messages cannot be said now, "+
			"because the read state could not be read. Ask again shortly, or for the rooms "+
			"without `unread`", ErrUnavailable)
	}
	listing, err := s.Chat.Channels(ctx, who, chatlet.ChannelsQuery{
		Cursors: state.Cursors,
		// THE ROOMS IN COMMON with another seat, which a profile lists:
		// the viewer's own rooms that seat is in too.
		With: p.String("with"),
		// AN ARCHIVED ROOM IS READABLE FOR EVER and belongs in a search
		// result or a deliberate visit rather than in the list somebody
		// works out of.
		IncludeArchived: p.Bool("include_archived", false),
		Unread:          unread,
		// THE WHOLE RAIL BY DEFAULT, which is what the dashboard draws.
		Limit: Clamp(p.Int("limit", 0), chatlet.MaxRailChannels, chatlet.MaxRailChannels),
	}, fresh)
	if err != nil {
		return nil, chatFailure(err)
	}
	muted := make(map[string]bool, len(state.Muted))
	for _, id := range state.Muted {
		muted[id] = true
	}
	rooms := make([]chatRoom, 0, len(listing.Channels))
	for _, summary := range listing.Channels {
		if !measured {
			// AN UNKNOWN CURSOR IS NOT A CURSOR AT ZERO. With no read
			// state the reader counted each room's whole tail, the
			// arithmetic for somebody who never opened it — so it is
			// zeroed rather than carried, and a client ignoring
			// `read_state` under-badges instead of telling every person
			// they are a hundred behind everywhere.
			summary.Unread, summary.UnreadCapped, summary.Mentions = 0, false, 0
		}
		rooms = append(rooms, chatRoom{ChannelSummary: summary,
			Muted: muted[summary.Channel.ID]})
	}
	out := chatRail{
		Served: listing.Served, Channels: rooms,
		Truncated: listing.Truncated, Unreadable: listing.Unreadable,
		ReadState: measured, DefaultPrivate: s.chatDefaultPrivate(),
	}
	if measured {
		state.AbsentSince = time.Time{}
		out.Read = &coord.PersonChatRead{Handle: who, ChatReadState: state}
	}
	return out, nil
}

// chatReadState is the viewer's cursors and mutes, and whether they could be
// read at all.
//
// A FAILURE HERE DOES NOT REFUSE THE RAIL. The rooms, the previews and the
// order are the log's and are still true; only the badges are unknown, and
// taking the whole screen away to say so would cost a person their whole
// conversation over a bucket read.
func (s Sources) chatReadState(ctx context.Context, who string) (coord.ChatReadState, bool) {
	if s.ChatReads == nil {
		return coord.ChatReadState{}, false
	}
	state, err := s.ChatReads.ChatRead(ctx, who)
	if err != nil {
		log.WarnContext(ctx, "chat_read_state_unreadable", "handle", who, "error", err)
		return coord.ChatReadState{}, false
	}
	return state, true
}

// chatRooms answers every named room the viewer may browse, joined or not,
// the archived ones included, each with its member count, whether the viewer
// is in it and when somebody last spoke there ([chatlet.Reader.Directory]).
//
// THE RAIL'S VERB, seated as every chat question is, and resolved by the
// serving reader from this viewer, so a private room is listed to its members
// alone.
func (s Sources) chatRooms(ctx context.Context, p Params) (any, error) {
	who, err := s.chatViewer(ctx, authz.ActionChatList)
	if err != nil {
		return nil, err
	}
	fresh, err := Freshness(p)
	if err != nil {
		return nil, err
	}
	rooms, err := s.Chat.Directory(ctx, who, fresh)
	if err != nil {
		return nil, chatFailure(err)
	}
	return rooms, nil
}

// chatRoomNames answers the name of every room the viewer may READ, joined or
// not, the archived ones included: each room's id, name and kind, and no more.
//
// FOR PROSE, which names a room as `#launch`. A room's name may be spelled
// exactly as a colour (`#e2e`) or an issue's number (`#2026`), and the only
// way a reader tells which a sentence means is to know which rooms there are.
// The rail cannot say: it is the rooms somebody is IN, and the room an agent
// names in an ask is as often a public room the reader never joined. The room
// finder offers the same rooms, so a room nobody has spoken in is still found.
//
// THE RAIL'S VERB, and seated as every chat question is: the set is
// [chatlet.Reader.RoomNames]', resolved by the serving reader from this
// viewer, so a private room is named only to a member of it.
func (s Sources) chatRoomNames(ctx context.Context, p Params) (any, error) {
	who, err := s.chatViewer(ctx, authz.ActionChatList)
	if err != nil {
		return nil, err
	}
	fresh, err := Freshness(p)
	if err != nil {
		return nil, err
	}
	names, err := s.Chat.RoomNames(ctx, who, fresh)
	if err != nil {
		return nil, chatFailure(err)
	}
	return names, nil
}

// ---- one room ---------------------------------------------------------- //

// chatChannel answers one room's metadata and its membership, the room named
// by its id or by its name ("launch", or "#launch" as people write it,
// [chatlet.RoomRef]).
//
// A ROOM THIS VIEWER MAY NOT READ IS NOT FOUND, which is [chatlet.Reader]'s own
// answer: a private room's EXISTENCE is information, so "no such room" and
// "not yours" must read identically from outside, by name as by id.
//
// BY NAME, because a name is the room's address and what every sentence about
// it says, while the rail lists only the rooms somebody is in: a caller that
// knows "#launch" and is not in it has no other way to reach the room. A name
// is read once, by the reader's by-name read, which answers the room itself;
// the room's transcript and its threads resolve one the same way
// ([chatRoomID]).
func (s Sources) chatChannel(ctx context.Context, p Params) (any, error) {
	who, err := s.chatViewer(ctx, authz.ActionChatRead)
	if err != nil {
		return nil, err
	}
	ref, err := chatChannelID(p)
	if err != nil {
		return nil, err
	}
	fresh, err := Freshness(p)
	if err != nil {
		return nil, err
	}
	var detail chatlet.ChannelDetail
	switch id, name := chatlet.RoomRef(ref); {
	case id != "":
		detail, err = s.Chat.Channel(ctx, who, id, fresh)
	case name != "":
		detail, err = s.Chat.ChannelNamed(ctx, who, name, fresh)
	default:
		return nil, badParams("channel_id", ref, nil)
	}
	if err != nil {
		return nil, chatFailure(err)
	}
	return detail, nil
}

// chatMessages answers a page of one room, newest first.
//
// THE CURSOR IS THE ANSWER'S OWN `next_cursor`, sent back unchanged: the
// room's per-message number rather than an offset or an instant, because an
// offset re-reads a row whenever anything is written above it. This surface
// neither builds nor decodes one; it carries it.
func (s Sources) chatMessages(ctx context.Context, p Params) (any, error) {
	who, err := s.chatViewer(ctx, authz.ActionChatRead)
	if err != nil {
		return nil, err
	}
	ref, err := chatChannelID(p)
	if err != nil {
		return nil, err
	}
	fresh, err := Freshness(p)
	if err != nil {
		return nil, err
	}
	id, err := s.chatRoomID(ctx, who, ref, fresh)
	if err != nil {
		return nil, err
	}
	page, err := s.Chat.Messages(ctx, who, chatlet.TranscriptQuery{
		ChannelID: id, Cursor: strings.TrimSpace(p.String("cursor")),
		Limit: chatPage(p),
	}, fresh)
	if err != nil {
		return nil, chatFailure(err)
	}
	return page, nil
}

// chatThread answers one thread from its root, oldest first — a question of its
// own rather than a filter on the transcript, because a thread is opened
// beside the room and paged in the opposite direction.
func (s Sources) chatThread(ctx context.Context, p Params) (any, error) {
	who, err := s.chatViewer(ctx, authz.ActionChatRead)
	if err != nil {
		return nil, err
	}
	ref, err := chatChannelID(p)
	if err != nil {
		return nil, err
	}
	root := strings.TrimSpace(p.String("root_id"))
	if root == "" {
		return nil, badParams("root_id", "", nil)
	}
	fresh, err := Freshness(p)
	if err != nil {
		return nil, err
	}
	id, err := s.chatRoomID(ctx, who, ref, fresh)
	if err != nil {
		return nil, err
	}
	thread, err := s.Chat.Thread(ctx, who, chatlet.ThreadQuery{
		ChannelID: id, RootID: root,
		Cursor: strings.TrimSpace(p.String("cursor")), Limit: chatPage(p),
	}, fresh)
	if err != nil {
		return nil, chatFailure(err)
	}
	return thread, nil
}

// ---- search ------------------------------------------------------------ //

// chatHit is one matched message as this surface answers it.
type chatHit struct {
	MessageID string `json:"message_id"`
	ChannelID string `json:"channel_id"`

	// ChannelName and ChannelKind are the room the match was said in, as a
	// mention carries its room's ([chatlet.Mention]): what a result is shown
	// under, "#launch", rather than an id nobody reads. A direct
	// conversation has no name.
	ChannelName string       `json:"channel_name,omitempty"`
	ChannelKind chatlet.Kind `json:"channel_kind"`

	// ThreadRoot is the thread the match was said in, empty on a room post
	// — which is what lets a result open the thread rather than the room.
	ThreadRoot string `json:"thread_root,omitempty"`

	Author string `json:"author,omitempty"`

	// Snippet is what the message says around the words that matched,
	// bounded by the index itself ([search.ChatSnippetBytes]) and marked
	// where it was cut, so one screen's rows are not fifty times another's.
	Snippet string `json:"snippet,omitempty"`

	At    time.Time `json:"at"`
	Score float64   `json:"score"`
}

// chatSearchAnswer is one ranked search over the rooms this viewer may read.
type chatSearchAnswer struct {
	Hits []chatHit `json:"hits"`

	// Searched is how many rooms this query actually ran over, which is
	// the DENOMINATOR a reader needs: "nothing matched" over three rooms
	// and over three hundred are different answers.
	Searched int `json:"searched"`

	// Building says the answering node's index had not caught up with its
	// transcript while it searched, so the hits are drawn from part of it:
	// "the index is still building", which a screen must say rather than
	// "nobody said that".
	Building bool `json:"building,omitempty"`

	// Note says it in words, as the knowledge and the work searches' notes
	// do, so a reader that shows the answer's own sentence rather than its
	// flags (the assistant's card) tells "nobody said that" from "not
	// indexed yet". Empty over a caught-up index.
	Note string `json:"note,omitempty"`

	// ReadLevel is what the VISIBLE SET was resolved at — the only half of
	// a search that can be behind, and the half that decides what may be
	// seen.
	ReadLevel statelog.ReadLevel `json:"read_level"`

	// Position is the answering copy's applied position on the chat log,
	// read BEFORE it resolved the visible set, so it is at or below the
	// read's own and never overstates what the answer saw.
	Position statelog.Position `json:"position"`
}

// chatIndexBuilding is what a search over a chat index still catching up says
// of itself.
const chatIndexBuilding = "the node that answered is still indexing chat, so a message " +
	"that says this may not be findable yet"

// chatSearch ranks the company's chat for the viewer, over the rooms they may
// read.
//
// THE VISIBLE SET IS WIDER THAN THE RAIL: a public room and a unit's room are
// readable by any seat, joined or not, so a search driven off somebody's
// membership would answer nothing from exactly the rooms a company does most
// of its talking in. It is resolved by the serving node from the viewer, and
// never handed in. A NARROWING IS INTERSECTED, NEVER TRUSTED: naming a room
// the viewer may not read answers nothing rather than refusing, because a
// refusal confirms the room exists.
//
// THE PAGE IS THE INDEX'S OWN — [search.ChatSearchLimit] by default and
// [search.MaxChatSearchLimit] at most, the bounds the index clamps every query
// to — rather than a pair of this surface's, which the index would clamp
// silently below whatever this one promised.
func (s Sources) chatSearch(ctx context.Context, p Params) (any, error) {
	who, err := s.chatViewer(ctx, authz.ActionChatSearch)
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(p.String("q"))
	if text == "" {
		return nil, badParams("q", "", nil)
	}
	// THE BOUND EVERY RANKED SEARCH SHARES, refused here in the API's own
	// words rather than left to the index, whose refusal would cross the
	// router as words alone, a screen unable to tell it from a fault.
	if refused := searchTextRefusal(text); refused != nil {
		return nil, refused
	}
	fresh, err := Freshness(p)
	if err != nil {
		return nil, err
	}
	found, err := s.ChatSearch.Search(ctx, estate.ChatQuery{
		Viewer: who, Text: text,
		Channel: strings.TrimSpace(p.String("channel_id")),
		Author:  strings.TrimSpace(p.String("author")),
		Limit: Clamp(p.Int("limit", 0), search.ChatSearchLimit,
			search.MaxChatSearchLimit),
		Fresh: fresh,
	})
	if err != nil {
		return nil, chatFailure(err)
	}
	out := chatSearchAnswer{
		// AN EMPTY SLICE, never null: a client rendering `hits.length`
		// should not have to guard the field as well.
		Hits: make([]chatHit, 0, len(found.Hits)), Searched: found.Searched,
		Building: found.Building, ReadLevel: fresh.Level, Position: found.Position,
	}
	if found.Building {
		out.Note = chatIndexBuilding
	}
	for _, hit := range found.Hits {
		room := found.Rooms[hit.ChannelID]
		out.Hits = append(out.Hits, chatHit{
			MessageID: hit.MessageID, ChannelID: hit.ChannelID,
			ChannelName: room.Name, ChannelKind: room.Kind,
			ThreadRoot: hit.ThreadRoot, Author: hit.Author, Snippet: hit.Snippet,
			At: hit.CreatedAt, Score: hit.Score,
		})
	}
	return out, nil
}

// chatDefaultPrivate is the company's default room visibility, or false when
// there is no company to ask — read per call, because an apply replaces the
// epoch and a value captured at boot would go on telling the screen the old
// policy on a privacy control.
func (s Sources) chatDefaultPrivate() bool {
	c := s.company()
	if c == nil {
		return false
	}
	return c.Chat.Chatlet.DefaultPrivate()
}

// chatVendors is the vendor chat surfaces a company talking on a vendor's
// chat declares: empty, never nil, on a company that talks on the engine's
// own chat or on none, and with no company.
func (s Sources) chatVendors() []string {
	c := s.company()
	if c == nil || c.ChatBackendFor() != config.ChatVendor {
		return []string{}
	}
	return c.ChatVendors()
}

// company is the running company's document, or nil before one is running.
func (s Sources) company() *config.Company {
	if s.Company == nil {
		return nil
	}
	c, _ := s.Company()
	return c
}
