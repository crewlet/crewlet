// The company's own conversation, read for a person.
//
// # The viewer is the SERVER's answer, and a caller may never name a seat
//
// Every other personal question here takes a handle and falls back to the
// caller's own seat — see [Sources.viewerHandle], where naming somebody else's
// is what an operator credential buys. Chat has no such arm, and the
// difference is deliberate rather than an oversight: a transcript is the most
// sensitive thing a deployment holds, so who is asking is resolved from the
// credential on every call and there is nowhere to write a handle at all. A
// `handle=` on one of these requests is an ignored parameter, not a filter.
//
// A TOKEN BOUND TO NO SEAT READS NOTHING — see [Sources.chatViewer]. That is
// stricter than the rule the tracker's inbox follows, and it is the posture
// the product page states in as many words: a pipeline's credential is not a
// person. The refusal names `contact.crewlet_operator_id`, because the remedy
// is a line of company configuration and an operator who has just built a
// deploy token needs to be told what to change rather than that the company
// has no conversations.
//
// # The rooms come from the log and the badges do not
//
// A room, its membership and its messages are the state log's, read from this
// node's own applied rows through [chat.Reader] — so every answer here carries
// the position it was true as of, and a node that has not caught up says so
// rather than drawing an empty room. Where each person's eye has reached, what
// they have muted and whether they are in do-not-disturb is NOT on that log:
// it is one reader's attention, it lives in coordination, and it reaches the
// rail as a value ([chat.ChannelsQuery.Cursors]).
//
// That is why the rail's answer carries `read_state`. A coordination store
// that could not be read leaves the badges UNKNOWN, which is a third state
// beside "read" and "unread" — and reporting an unknown cursor as a cursor at
// zero would badge every room in the company with its whole history the moment
// a bucket read failed. [Sources.budgets] reports `durable: false` for exactly
// this reason, over exactly this store.

package queries

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// ChatReader is chat's read side as this surface calls it.
//
// Consumer-defined and kept to what these six questions ask, like every other
// seam here. THE FRESHNESS IS PART OF EVERY CALL rather than a property of the
// reader, for [PageReader]'s reason: a dashboard poll is the one caller
// allowed to choose its own level, and a reader built around one would make
// the level a label on the answer.
//
// [ChatReader.At] is on it for the one question that has no other position to
// report — see [Sources.chatSearch].
type ChatReader interface {
	Channels(ctx context.Context, viewer string, q chat.ChannelsQuery,
		fresh statelog.Freshness) (chat.ChannelListing, error)
	Channel(ctx context.Context, viewer, channelID string,
		fresh statelog.Freshness) (chat.ChannelDetail, error)
	Messages(ctx context.Context, viewer string, q chat.TranscriptQuery,
		fresh statelog.Freshness) (chat.Transcript, error)
	Thread(ctx context.Context, viewer string, q chat.ThreadQuery,
		fresh statelog.Freshness) (chat.Thread, error)
	Mentions(ctx context.Context, viewer string, q chat.MentionQuery,
		fresh statelog.Freshness) (chat.MentionFeed, error)

	// Readable is every room this viewer may READ, which is WIDER than
	// the rail: a public room and a unit's room are readable by any seat
	// of the company, joined or not. It is the authorization half of
	// search, and the search's own refusal exists because an empty set
	// and a lost viewer differ by the whole transcript.
	Readable(ctx context.Context, viewer string, fresh statelog.Freshness) ([]string, error)

	// At is this node's applied position on the chat log.
	At() statelog.Position
}

// ChatSearcher is the keyword index over the company's chat, as this surface
// asks it.
//
// THE RAW INDEX SEAM, taking the visible channel set as a parameter — which is
// the opposite of what a seat's tools take ([builtin.ChatSearcher] resolves
// the set itself, from the viewer). Two reasons, and both are about this
// surface rather than about safety: the set has to be resolved at the
// DASHBOARD's freshness (a seat resolves its own at `linearizable`, which
// would put a quorum barrier on every keystroke of a search box), and the
// answer has to carry the POSITION it was resolved at, which a seam that
// resolved it internally cannot report.
//
// The set is still never the caller's: it is computed here, from the viewer
// the server resolved, and a request may only NARROW it to one room — see
// [Sources.chatSearch].
type ChatSearcher interface {
	SearchMessages(ctx context.Context, q search.ChatQuery) ([]search.ChatHit, error)
}

// ChatReads is where one person's eye has reached, as this surface needs it.
//
// The READ half of [coord.ChatReads] and nothing else: a flush is a write, and
// nothing on this surface writes. Nil is "this process cannot say", which the
// rail reports rather than fabricating a cursor at zero for every room.
type ChatReads interface {
	ChatRead(ctx context.Context, handle string) (coord.ChatReadState, error)
}

// Page sizes for the one chat question whose rows are neither a room's
// transcript nor a feed, and therefore cannot take [chat.DefaultLimit].
//
// TWENTY-FIVE is the default, which is [DefaultSearchLimit]'s figure for the
// same surface and the same reason — a ranked list is SCANNED rather than
// worked, and a reader flicks past a row in a fraction of a second — and it is
// a quarter above the seat tool's own twenty, because a tool's rows are read
// into a PROMPT where every one of them costs context the turn could spend on
// the work.
//
// A HUNDRED is the ceiling, and it is the HYDRATE that sets it rather than the
// screen: [search.ChatIndexer.SearchMessages] reads one row of chat_messages
// per hit, so the ceiling bounds the point reads one search costs the
// replicated estate on a request path. [chat.MaxLimit]'s five hundred is the
// right ceiling for a transcript page, where the rows are one range scan over
// `chat_messages_channel_idx`; it would be five hundred separate lookups here.
const (
	DefaultChatSearchPage = 25
	MaxChatSearchPage     = 100
)

// errNoChatSeat is the ONE refusal every chat question makes, and the reason
// it is a variable rather than six sentences is that six spellings of one
// refusal is how they drift.
//
// [ErrBadParams] rather than [ErrUnauthorized], for [errNoSeat]'s reason and
// one of chat's own. Nobody was refused a room: there is no person to answer
// about, and the remedy is a line of company configuration rather than a
// different credential. And it is the only class whose MESSAGE survives to the
// caller — the socket and the REST route both reduce every other refusal to
// the query's name, deliberately, because a failure's own text can carry a
// database path. A refusal whose whole value is naming the field to change
// must be in the one class that carries its text.
//
// Its own sentence rather than [errNoSeat]'s, because that one ends by
// offering to name a handle instead, which is precisely what chat forbids.
var errNoChatSeat = fmt.Errorf("%w: this credential is not bound to a seat and "+
	"chat is read as a person — give a `kind: human` seat a "+
	"contact.crewlet_operator_id matching the api.auth token id; an unbound "+
	"credential reads nothing here, not even a public room, because a "+
	"transcript is the most sensitive thing this deployment holds",
	ErrBadParams)

// chatViewer resolves the caller's credential to the seat it acts as.
//
// AT THE TOP OF EVERY CHAT QUESTION, before a parameter is read and before any
// store is touched, because the answer to "who is asking" decides every other
// answer on this surface — which rooms exist, whose unread counts these are,
// and which transcripts may be searched at all.
//
// There is no `handle` parameter anywhere here and no operator arm that reads
// somebody else's rooms. An operator who needs to read a private room reads it
// as a member of it; a credential that could name a seat would be a credential
// that could read every direct conversation in the company.
func (s Sources) chatViewer(ctx context.Context) (string, error) {
	seat := s.seatForOperator(operatorFrom(ctx))
	if seat == nil {
		return "", errNoChatSeat
	}
	handle := seat.Handle()
	if handle == "" {
		// A SEAT WITH NO HANDLE IS NOT A VIEWER. The org model derives a
		// handle for every role, so this is unreachable on a config that
		// parsed — and an empty one passed on would reach [chat.Visible]
		// as "nobody", which is refused there by name rather than
		// serving anything. Refusing here keeps the sentence the
		// operator gets pointing at the same field.
		return "", errNoChatSeat
	}
	return handle, nil
}

// chatFailure classifies one chat read's failure for the transports.
//
// THREE OUTCOMES THAT ARE NOT FAILURES OF THIS NODE, and each is acted on
// differently: a room that is not there — or that this viewer may not see,
// which [chat.Reader] deliberately answers identically — is a dead link; a
// cursor or a room id this surface refused is a request to change; and a node
// behind the log is a retry in a moment, carrying the refusal's own hint.
//
// [chat.ErrForbidden] is NOT mapped here, and its absence is the point: the
// only read that raises it is one with an empty viewer, which [chatViewer]
// refuses before any reader is called. A branch for it would be a branch with
// no producer, indistinguishable to the next reader from one whose producer
// nobody found.
func chatFailure(err error) error {
	switch {
	case errors.Is(err, chat.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, chat.ErrInvalid):
		// THE TEXT SURVIVES, because it is written for the caller: it
		// says what a cursor looks like and where the last answer put
		// one. See [freshness] for the same wrapping.
		return fmt.Errorf("%w: %w", ErrBadParams, err)
	}
	return unavailableIfTransient(err)
}

// chatChannelID reads the room a question is about.
//
// `channel_id` RATHER THAN `channel`, and the reason is that the domain's own
// refusals name the field: [chat.Reader] answers a read with no room with
// `channel_id: a read names no room`, and a thread with no root with
// `root_id`. Those sentences reach the caller unchanged — they are the one
// class of refusal both transports keep the text of — so a shorter parameter
// here would send somebody looking for a key this surface does not accept. It
// is also the name every answer uses for the same value, so what a client
// reads off a transcript is what it sends back.
func chatChannelID(p Params) (string, error) {
	id := strings.TrimSpace(p.String("channel_id"))
	if id == "" {
		return "", badParams("channel_id", "", nil)
	}
	return id, nil
}

// chatPage bounds a transcript, a thread or a feed page.
//
// [chat.DefaultLimit] and [chat.MaxLimit] rather than numbers of this
// surface's own: fifty is a screen and five hundred is what an export may ask
// for in one round trip, and the reader clamps to exactly these — so a second
// pair here would be two ceilings for one page, disagreeing the day either
// moved.
func chatPage(p Params) int {
	return Clamp(p.Int("limit", 0), chat.DefaultLimit, chat.MaxLimit)
}

// ---- the rail ---------------------------------------------------------- //

// chatRoom is one room as the rail renders it: the reader's own summary, plus
// the one fact about it that is not on the log.
//
// EMBEDDED rather than restated field by field. A row rebuilt here would be a
// second spelling of the reader's own shape — the preview, the unread count,
// the participants of a direct conversation — and the copy that drifts is
// always the one a screen is actually drawing.
type chatRoom struct {
	chat.ChannelSummary

	// Muted says this viewer silenced the room. It suppresses NOTICE and
	// never delivery: the messages are there, they are searchable, and a
	// seat's wake is not affected by anybody's mute.
	Muted bool `json:"muted,omitempty"`
}

// chatRail is one person's rooms, and what this node could establish about
// them.
type chatRail struct {
	chat.Served

	Channels []chatRoom `json:"channels"`

	// Truncated says this person is in more rooms than
	// [chat.MaxRailChannels] — the reader's own flag, carried rather than
	// inferred from the length, because the list is sorted by activity
	// after it is read.
	Truncated bool `json:"truncated,omitempty"`

	// Unreadable is how many of this person's rooms this build could not
	// present, a room a newer peer wrote included.
	Unreadable int `json:"unreadable,omitempty"`

	// DefaultPrivate is what a room created WITHOUT a stated visibility
	// will be, from `chat.native.default_channel_private`.
	//
	// ON THE RAIL because the rail is the first thing the screen loads and
	// the create form opens out of it, so this costs no round trip. It is
	// policy rather than a property of the rooms listed, which is why it
	// sits on the wrapper here and not on [chat.ChannelListing] — the
	// domain type answers what exists, not what the company prefers.
	//
	// THE SCREEN NEEDS IT TO BE HONEST, which is the whole reason it is
	// exposed. The composer's visibility selector has to open on what will
	// actually happen; a form that showed "public" while the company
	// default was private would be a privacy control the person is
	// actively misled about. The SERVER still decides — an omitted kind is
	// resolved in chat.Store.CreateChannel — so a stale or absent value
	// here changes what the form SHOWS and never what it gets.
	DefaultPrivate bool `json:"default_private,omitempty"`

	// ReadState says the badges on these rows are a MEASUREMENT. False
	// means the coordination record could not be read — the counts are
	// then zero and the mutes absent, which is not "you have read
	// everything" but "nobody could look".
	//
	// STATED rather than left to a client noticing every count is zero:
	// an empty company and an unreadable bucket produce identical rows,
	// and only one of them is a fact about the conversation.
	ReadState bool `json:"read_state"`

	// DNDUntil is when this person's do-not-disturb ends, absent when
	// they are not in it. A POINTER because the zero instant is what "not
	// in do-not-disturb" is stored as, and a zero time renders as year
	// one rather than disappearing.
	DNDUntil *time.Time `json:"dnd_until,omitempty"`
}

// chatChannels answers the viewer's own rooms, newest activity first.
//
// THE CURSORS TRAVEL INTO THE READ rather than the badge being counted
// afterwards, which is what makes an unread count cost a bounded range scan
// per room: the reader counts above this person's own cursor and stops at
// [chat.UnreadLimit], so somebody back from a fortnight away is told "99+"
// instead of costing this node a scan of every room's whole transcript.
func (s Sources) chatChannels(ctx context.Context, p Params) (any, error) {
	who, err := s.chatViewer(ctx)
	if err != nil {
		return nil, err
	}
	fresh, err := freshness(p)
	if err != nil {
		return nil, err
	}
	state, measured := s.chatReadState(ctx, who)
	listing, err := s.Chat.Channels(ctx, who, chat.ChannelsQuery{
		Cursors: state.Cursors,
		// AN ARCHIVED ROOM IS READABLE FOR EVER and belongs in a search
		// result or a deliberate visit rather than in the list somebody
		// works out of — so it is off unless this caller asked.
		IncludeArchived: p.Bool("include_archived", false),
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
			// state the reader counted each room's whole tail, which
			// is the arithmetic for somebody who has never opened it
			// — true of a new reader and false of everybody else. It
			// is zeroed rather than carried, so a client that ignores
			// `read_state` under-badges instead of telling every
			// person in the company that they are a hundred behind
			// everywhere.
			summary.Unread, summary.UnreadCapped = 0, false
		}
		rooms = append(rooms, chatRoom{
			ChannelSummary: summary,
			Muted:          muted[summary.Channel.ID],
		})
	}

	out := chatRail{
		Served: listing.Served, Channels: rooms,
		Truncated: listing.Truncated, Unreadable: listing.Unreadable,
		ReadState:      measured,
		DefaultPrivate: s.chatDefaultPrivate(),
	}
	if measured && !state.DNDUntil.IsZero() {
		until := state.DNDUntil
		out.DNDUntil = &until
	}
	return out, nil
}

// chatReadState is the viewer's cursors and mutes, and whether they could be
// read at all.
//
// A FAILURE HERE DOES NOT REFUSE THE RAIL. The rooms, the previews and the
// order are the log's and are still true; only the badges are unknown, and
// taking the whole screen away to say so would cost a person their whole
// conversation over a bucket read. The flag is what keeps the two apart — see
// [Sources.budgets], which answers `durable: false` over this same store for
// this same reason.
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

// ---- one room ---------------------------------------------------------- //

// chatChannel answers one room's metadata and its membership.
//
// A ROOM THIS VIEWER MAY NOT READ IS NOT FOUND, which is [chat.Reader]'s own
// answer and not a decision restated here: a private room's EXISTENCE is
// information — who is talking to whom is most of what a transcript discloses
// — so "no such room" and "not yours" must read identically from outside.
func (s Sources) chatChannel(ctx context.Context, p Params) (any, error) {
	who, err := s.chatViewer(ctx)
	if err != nil {
		return nil, err
	}
	id, err := chatChannelID(p)
	if err != nil {
		return nil, err
	}
	fresh, err := freshness(p)
	if err != nil {
		return nil, err
	}
	detail, err := s.Chat.Channel(ctx, who, id, fresh)
	if err != nil {
		return nil, chatFailure(err)
	}
	return detail, nil
}

// chatMessages answers a page of one room, newest first.
//
// THE CURSOR IS THE ANSWER'S OWN `next_cursor`, sent back unchanged. It is the
// room's per-message number rather than an offset or an instant, and that is a
// correctness property rather than a style: an offset re-reads a row whenever
// anything is written above it, and a message EDITED while somebody scrolls
// moves up an ordering keyed on the log position — the exact gap a keyset
// cursor exists to prevent. This surface neither builds nor decodes one; it
// carries it.
func (s Sources) chatMessages(ctx context.Context, p Params) (any, error) {
	who, err := s.chatViewer(ctx)
	if err != nil {
		return nil, err
	}
	id, err := chatChannelID(p)
	if err != nil {
		return nil, err
	}
	fresh, err := freshness(p)
	if err != nil {
		return nil, err
	}
	page, err := s.Chat.Messages(ctx, who, chat.TranscriptQuery{
		ChannelID: id,
		Cursor:    strings.TrimSpace(p.String("cursor")),
		Limit:     chatPage(p),
	}, fresh)
	if err != nil {
		return nil, chatFailure(err)
	}
	return page, nil
}

// chatThread answers one thread from its root, oldest first.
//
// A SECOND QUESTION rather than a filter on the transcript, for the reason
// `containers` is separate from `pages`: a thread is opened beside the room
// and paged in the opposite direction — a conversation is read forwards — so
// one question answering either would make every caller branch on what came
// back.
func (s Sources) chatThread(ctx context.Context, p Params) (any, error) {
	who, err := s.chatViewer(ctx)
	if err != nil {
		return nil, err
	}
	id, err := chatChannelID(p)
	if err != nil {
		return nil, err
	}
	root := strings.TrimSpace(p.String("root_id"))
	if root == "" {
		return nil, badParams("root_id", "", nil)
	}
	fresh, err := freshness(p)
	if err != nil {
		return nil, err
	}
	thread, err := s.Chat.Thread(ctx, who, chat.ThreadQuery{
		ChannelID: id, RootID: root,
		Cursor: strings.TrimSpace(p.String("cursor")),
		Limit:  chatPage(p),
	}, fresh)
	if err != nil {
		return nil, chatFailure(err)
	}
	return thread, nil
}

// chatMentions answers the viewer's own @-mention feed.
//
// THE FEED'S CURSOR IS A LOG POSITION where a transcript's is a room's own
// message number, and the two are not interchangeable: a mention row's version
// is the position that FIRST named this handle, which carries the generation
// in its ordering and therefore spans a reanchor with no gap and no repeat.
// Both arrive here as opaque strings from the answer that produced them, which
// is what stops this surface having an opinion about either.
func (s Sources) chatMentions(ctx context.Context, p Params) (any, error) {
	who, err := s.chatViewer(ctx)
	if err != nil {
		return nil, err
	}
	fresh, err := freshness(p)
	if err != nil {
		return nil, err
	}
	feed, err := s.Chat.Mentions(ctx, who, chat.MentionQuery{
		Cursor: strings.TrimSpace(p.String("cursor")),
		Limit:  chatPage(p),
	}, fresh)
	if err != nil {
		return nil, chatFailure(err)
	}
	return feed, nil
}

// ---- search ------------------------------------------------------------ //

// chatHit is one matched message as this surface answers it.
//
// RENDERED RATHER THAN PASSED THROUGH, for two facts the index's own row
// cannot carry to a screen. Its instant is the store's encoded integer, and
// every other instant on this surface is an RFC 3339 time. And its `Excerpt`
// is the message's WHOLE body: the index selects the `body` column into it, so
// a page of twenty-five hits on a corpus with pasted blocks in it is hundreds
// of kilobytes of JSON to draw twenty-five lines. [chat.Excerpt] is the cut
// the rail's preview and the mention feed already take — rune-safe and bounded
// by [chat.MaxExcerpt] — so one screen's rows are not fifty times another's.
type chatHit struct {
	MessageID string `json:"message_id"`
	ChannelID string `json:"channel_id"`

	// ThreadRoot is the thread the match was said in, empty on a room
	// post — which is what lets a result open the thread rather than the
	// room and lose the context the words were in.
	ThreadRoot string `json:"thread_root,omitempty"`

	Author  string `json:"author,omitempty"`
	Excerpt string `json:"excerpt,omitempty"`

	At    time.Time `json:"at"`
	Score float64   `json:"score"`
}

// chatSearchAnswer is one ranked search over the rooms this viewer may read.
type chatSearchAnswer struct {
	Hits []chatHit `json:"hits"`

	// Searched is how many rooms this query actually ran over, which is
	// the DENOMINATOR a reader needs: "nothing matched" over three rooms
	// and over three hundred are different answers, and a search narrowed
	// to a room the viewer cannot read answers over none of them.
	Searched int `json:"searched"`

	// ReadLevel is what the VISIBLE SET was resolved at. The ranking
	// itself has no level — the index is this node's own and is not on
	// the log at all — so this is the level of the only half that can be
	// behind, which is also the half that decides what may be seen.
	ReadLevel statelog.ReadLevel `json:"read_level"`

	// Position is this node's applied position, read BEFORE the visible
	// set was resolved.
	//
	// THE DIRECTION OF THE ERROR IS DELIBERATE. [chat.Reader.Readable]
	// answers with the rooms and not the transaction it read them in, so
	// the honest position available here is this node's own — and taken
	// first it is at or BELOW the read's, never above. A caller told it
	// is further behind than it is refetches; one told the opposite
	// renders a stale answer as a fresh one.
	Position statelog.Position `json:"position"`
}

// chatSearch ranks the company's chat for the viewer, over the rooms they may
// read.
//
// THE VISIBLE SET IS COMPUTED HERE AND PASSED IN, and it is wider than the
// rail: a public room and a unit's room are readable by any seat, joined or
// not, so a search driven off somebody's membership would answer nothing from
// exactly the rooms a company does most of its talking in — silently, because
// an empty result reads as "nobody said that".
//
// AN EMPTY SET IS NEVER HANDED TO THE INDEX. [search.ErrNoViewer] is what the
// index answers a caller that LOST its viewer, and the two states differ by
// the whole transcript: a viewer who may genuinely read nothing is answered
// with no hits here, before the index is asked, and an unbound credential
// never reaches this function at all.
func (s Sources) chatSearch(ctx context.Context, p Params) (any, error) {
	who, err := s.chatViewer(ctx)
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(p.String("q"))
	if text == "" {
		return nil, badParams("q", "", nil)
	}
	fresh, err := freshness(p)
	if err != nil {
		return nil, err
	}
	// BEFORE THE READ, so the position this answer reports cannot
	// overstate what it saw — see [chatSearchAnswer.Position].
	at := s.Chat.At()
	visible, err := s.Chat.Readable(ctx, who, fresh)
	if err != nil {
		return nil, chatFailure(err)
	}
	if room := strings.TrimSpace(p.String("channel_id")); room != "" {
		// INTERSECTED, NEVER TRUSTED. Naming a room the viewer may not
		// read answers NOTHING rather than refusing: a refusal confirms
		// the room exists, and "there is no such room" and "you may not
		// see it" must read identically from outside — the same rule
		// [chat.Reader] applies to a direct read of one.
		if slices.Contains(visible, room) {
			visible = []string{room}
		} else {
			visible = nil
		}
	}
	out := chatSearchAnswer{
		// AN EMPTY SLICE, never null: a client rendering `hits.length`
		// should not have to guard the field as well.
		Hits: []chatHit{}, Searched: len(visible),
		ReadLevel: fresh.Level, Position: at,
	}
	if len(visible) == 0 {
		return out, nil
	}
	hits, err := s.ChatSearch.SearchMessages(ctx, search.ChatQuery{
		Text:     text,
		Channels: visible,
		Author:   strings.TrimSpace(p.String("author")),
		Limit:    Clamp(p.Int("limit", 0), DefaultChatSearchPage, MaxChatSearchPage),
	})
	if err != nil {
		return nil, err
	}
	for _, hit := range hits {
		out.Hits = append(out.Hits, chatHit{
			MessageID: hit.MessageID, ChannelID: hit.ChannelID,
			ThreadRoot: hit.ThreadRoot, Author: hit.Author,
			Excerpt: chat.Excerpt(hit.Excerpt),
			At:      store.DecodeTime(hit.CreatedAt), Score: hit.Score,
		})
	}
	return out, nil
}

// chatDefaultPrivate is the company's default room visibility, or false when
// there is no company to ask.
//
// READ PER CALL through [Sources.Company], for the reason every other source
// here is: an apply replaces the epoch, and a value captured at boot would go
// on telling the screen the old policy for the life of the process — on a
// privacy control, for as long as nobody restarts the node.
func (s Sources) chatDefaultPrivate() bool {
	if s.Company == nil {
		return false
	}
	c := s.Company()
	if c == nil {
		return false
	}
	return c.Chat.Native.DefaultPrivate()
}
