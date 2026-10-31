package chatlet

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Reader answers questions about the company's conversation from THIS NODE's
// own applied rows.
//
// # Every answer says where it came from
//
// EVERY READ GOES THROUGH [statelog.Reader], which is what turns the level
// from a word into a property of the answer: the refusal ladder first (an
// evicted node, one below the trim floor, one whose applier has stopped serves
// nothing), then the coverage probe over what the read is ABOUT, then the
// freshness target — a quorum-committed barrier for `linearizable`, the
// caller's own high-water mark for `session`, a declared bound for `stale` —
// and only then the rows. [Served] travels with every answer for the same
// reason the wiki's listing carries it: a caller cannot reconstruct the
// position afterwards, because [Reader.At] answers about NOW rather than about
// the transaction the rows were read in.
//
// AND ONE TRANSACTION PER ANSWER. A channel's detail is its row plus its
// membership, and a transcript page is its messages plus their reactions —
// each read in the transaction [statelog.Reader.Read] opens, so a page cannot
// answer with reactions from after the messages it reports.
//
// # Read-your-own-writes is the framework's floor, not a level of ours
//
// A write returns the position its record landed at and a wake carries one.
// A caller that holds either names it as [statelog.Freshness.MinPosition] and
// is served nothing from before it — at every level, with a `behind` refusal
// rather than rows from before the write when this node has not got there.
// There is no chat read level, no `wait_for` parameter and no second waiter:
// the one the framework already honours is the one this package threads.
//
// # ONE visibility function
//
// [Visible] decides who may read a room, and every read here calls it. The
// set reads narrow in SQL as well — `private = 0 OR <this reader is a member>`
// over the `private` column the applier computed from the same predicate — and
// that is a NARROWING rather than a second answer: without it a LIMIT would
// count rows the caller may not see and a page of fifty would render three.
// The decision is still [Visible]'s, on every row either filter admits.
//
// A ROOM THE VIEWER MAY NOT READ IS NOT FOUND. Not forbidden: a private room's
// EXISTENCE is information — who is talking to whom, and about what, is most
// of what a transcript discloses — and a refusal that distinguished "no such
// room" from "not yours" would answer that question for anybody who guessed an
// id.
//
// # Two cursors, and why they are not one
//
// A TRANSCRIPT PAGES ON `channel_seq`, the contiguous per-room number the
// applier mints from log order. A FEED PAGES ON THE COMPOSED POSITION, which
// carries the generation in the ordering and therefore spans a reanchor with
// no gap and no repeat.
//
// The composed position is the WRONG cursor for a transcript, and that is a
// correctness property rather than a preference: `chat_messages.version` is
// the last record that CHANGED the row, so an edit moves a message up the
// ordering. Paging backwards on it, a message edited while somebody scrolls
// jumps above the cursor and is never returned — the exact gap a keyset cursor
// exists to prevent. `channel_seq` is minted once and never moves, it is what
// `chat_messages_channel_idx` is ordered by, and it survives a reanchor
// because a replay in the same log order mints the same number. The mention
// feed has no such column and needs none: an edit keeps the row of every
// handle it still names and `chat_mentions` rows are written `ON CONFLICT DO
// NOTHING`, so a row's version is the position of the record that NAMED that
// handle and never moves either.
//
// # What is NOT here
//
//   - READ STATE. Where somebody's eye has reached lives in coordination
//     ([github.com/crewlet/crewlet/internal/coord.ChatReads]), and the cursors
//     reach this package as a VALUE on the query. A reader that dialled
//     coordination itself would put a fleet round trip inside every rail poll
//     and give this package a second store to be wrong about.
//   - SEARCH. The keyword index over this corpus is
//     [github.com/crewlet/crewlet/internal/search]'s, and it crosses the
//     estate boundary — the messages are replicated and the index is this
//     node's own — which is precisely why it is not a statement in here.
//   - A WAKE. That is derived from the committed record by something that
//     outlives the writer; see [github.com/crewlet/crewlet/internal/changefeed].
type Reader struct {
	log *statelog.Reader

	// committed is this node's own applied position on the chat log, or
	// nil on a build that runs no applier. It is what [Reader.At] answers
	// and what a caller holding no position of its own starts from.
	committed func() statelog.Position
}

// ReaderOptions configure a reader.
//
// THERE IS NO STORE HANDLE HERE. Every statement below runs inside the
// transaction [statelog.Reader.Read] opens, and a second handle beside it is
// the door a statement escapes through — outside the snapshot, outside the
// level, outside the refusal ladder and outside the coverage probe. The wiki's
// reader carried one for exactly that reason and read it nowhere; it no longer
// takes one either, so the two domains now ask their callers for the same
// thing.
type ReaderOptions struct {
	// Log is this domain's read authority. REQUIRED: without it every read
	// level is a label rather than a guarantee, which is silent at every
	// surface that renders one.
	Log *statelog.Reader

	// Committed is this node's applied position. Nil answers the zero
	// position, which is what a build with no applier has and what a read
	// then honestly reports.
	Committed func() statelog.Position
}

// NewReader builds chat's read side.
func NewReader(opts ReaderOptions) (*Reader, error) {
	if opts.Log == nil {
		return nil, errors.New("chatlet: a reader needs its domain's read " +
			"authority — without it every read level is a label rather than " +
			"a guarantee, and a degradation invisible in the answer is worse " +
			"than a refusal")
	}
	r := &Reader{log: opts.Log, committed: opts.Committed}
	if r.committed == nil {
		r.committed = func() statelog.Position { return statelog.Position{} }
	}
	return r, nil
}

// At is the position this node's rows were derived through, which every answer
// here was true as of when it was read.
func (r *Reader) At() statelog.Position { return r.committed() }

// The page bounds every listing here takes, on the wiki's and the tracker's
// reasoning: fifty is a screen, five hundred is what an export or a sync walk
// may ask for in one round trip, and a caller that names neither gets the
// screen rather than the export.
const (
	DefaultLimit = 50
	MaxLimit     = 500
)

// pageBytes is the second bound on a page of messages: the stored documents it
// carries, summed. A page ends at its limit or here, whichever comes first,
// and its cursor carries on from the last message it holds.
//
// HALF OF WHAT ONE MESSAGE ON THE BROKER CARRIES ([queue.MaxPayloadBytes]),
// because a page is ONE REPLY when a node that holds no data reads it through
// the estate router, and a reply past that ceiling is refused whole — "too
// large, narrow the request" — so a room of long messages could not be read a
// page at a time from such a node at all. The count alone never bounded it: a
// message's document at its caps runs to most of [MaxRecordBytes] — a body of
// markup encodes six bytes for one — so a page at [MaxLimit] is well over a
// hundred mebibytes of them, and a screen asking for one gets an error rather
// than fewer rows. The other half is for what rides
// with the page — the reactions, a thread's root and participants, the
// envelope — and for the difference between a stored document and its wire
// encoding. On an ordinary room the count binds long before this does.
const pageBytes = queue.MaxPayloadBytes / 2

// UnreadLimit is how far an unread count counts before it answers "at least
// this many".
//
// A HUNDRED. An unread badge is a number somebody glances at: the difference
// between 100 and 4,000 changes nothing anybody does, while counting to 4,000
// is a scan of a room's whole transcript on every poll of a rail that has one
// row per room. So the count is a COUNTED RANGE over the room's newest hundred
// messages — a person back from a fortnight's leave is told "99+" instead of
// costing the node a corpus scan to be told a number they will not read.
//
// THE WINDOW CAN ONLY LOSE WHAT THE CAP ALREADY HIDES. The messages above a
// cursor are the newest ones, so if more than a hundred of them are unread the
// answer is the cap whichever way it was counted; the window and the cap are
// one decision rather than two.
const UnreadLimit = 100

// MaxRailChannels bounds how many rooms one person's channel list answers for.
//
// TWO HUNDRED AND FIFTY-SIX, which is the cap the read-state bucket puts on
// one person's cursors
// ([github.com/crewlet/crewlet/internal/coord.MaxReadCursors]) — past it a
// room could not carry an unread count at all, because the record that would
// remember where its reader had got to has already dropped the stalest cursor
// to stay inside its own bound. Tying the two is what stops a rail that lists
// rooms whose badge can never be right.
//
// THE RAIL IS NOT PAGED, and that is what this cap buys. The list is ordered
// by ACTIVITY, which is a fact about each room's newest message rather than
// about any column the membership index is ordered by — so a window applied in
// SQL would page by channel id and re-sort each page against itself, which is
// an order that changes as you scroll. A bounded list sorted once is the
// honest shape; [ChannelListing.Truncated] says when a person is in more rooms
// than that. A caller that reads fewer ([ChannelsQuery.Limit]) is given the
// most recently active of that one sorted list, its top, never a page of it.
const MaxRailChannels = 256

// Visible is the ONE answer to whether somebody may read a room.
//
// # The rule
//
// A PUBLIC OR A UNIT ROOM IS READABLE BY ANY SEAT OF THE COMPANY, because the
// ENGINE is the boundary: everything that reaches this package has already
// been resolved to a seat in this company's own org chart, and a company whose
// seats could not read its own open rooms would be one where the org chart hid
// the work rather than organised it. A PRIVATE ROOM, A DIRECT CONVERSATION AND
// A GROUP ARE THEIR MEMBERSHIP, which is the same predicate the applier wrote
// the `private` column from ([privateRoom]) — one rule, one column, no second
// opinion.
//
// AN UNKNOWN KIND IS NOT VISIBLE. A kind a newer peer wrote is one this build
// cannot classify, and a room whose visibility rule is unknown must not be
// served on the guess that it resembles a public one: the failure of refusing
// is a room a person cannot open until this node is upgraded, and the failure
// of serving is a transcript disclosed to everybody.
//
// AN EMPTY VIEWER IS NOBODY. Identity is resolved above this package — a
// person reads as the human seat they hold, and the node the caller asked
// refuses a principal with no seat to read as before anything is routed — so
// an empty handle here is such a caller that slipped through, and the only
// honest answer for one is no. The reads refuse it by name rather than by an
// empty answer; see [viewerOf].
//
// AND NOBODY ELSE HAS A WAY IN. There is deliberately no arm here for an
// admin: no principal reads a private room, a group or a direct conversation
// it is not in. What compliance needs reaches past membership WITHOUT reading
// — a moderator's delete, an erase, a prune — and an arm that let whoever runs
// the deployment read every conversation between two people would be a
// product decision of its own, not a property of being trusted with the
// company's configuration.
//
// THE MEMBER FLAG IS THE CALLER'S because the two ways of establishing it are
// a row lookup and a row the caller already read: a function that went and
// looked would issue a second statement for every room in a listing that had
// just read the membership it needed.
func Visible(viewer string, ch Channel, member bool) bool {
	if strings.TrimSpace(viewer) == "" {
		return false
	}
	if !ch.Kind.Valid() {
		return false
	}
	if !privateRoom(ch.Kind) {
		return true
	}
	return member
}

// visibleWhere is [Visible]'s private arm as a SQL predicate, for the set
// reads whose LIMIT must count only rows the viewer may see.
//
// IT NARROWS AND NEVER DECIDES. Every row either filter admits is still put
// through [Visible] in Go, so there is one answer to who may read a room and
// this is the index's half of executing it. It takes ONE bind, the viewer's
// handle, and it drives `chat_members_handle_idx`.
const visibleWhere = `(c.private = 0 OR EXISTS (
		SELECT 1 FROM chat_members vm
		 WHERE vm.channel_id = c.id AND vm.handle = ?))`

// viewerOf is the identity every read starts from.
//
// THE REFUSAL NAMES THE REMEDY, which is the whole of its value: a principal
// with no seat to read as gets nothing in chat — reads included — and whoever
// has just built a pipeline credential needs to be told that what to change is
// the seat it acts as, not that the company has no conversations. It is the
// BACKSTOP: the node the caller asked refuses such a principal first, in a
// sentence of its own, and this is what a read that reached a data node with
// nobody in it is told.
func viewerOf(viewer string) (string, error) {
	who := strings.TrimSpace(viewer)
	if who == "" {
		return "", fmt.Errorf("%w: viewer: this chat read resolved to no seat "+
			"— a room is read as a seat, so sign in as the person whose seat it "+
			"is or bind the credential to a `kind: human` seat; a credential "+
			"acting as itself reads nothing here, and a transcript is the most "+
			"sensitive thing this deployment holds", ErrForbidden)
	}
	return who, nil
}

// errNoLevel refuses a read that named no freshness.
//
// AN ABSENT LEVEL IS THE SURFACE'S TO RESOLVE, through
// [statelog.LevelFor] — a seat tool reads linearizable, a dashboard poll
// stale — because a grammar four surfaces share cannot know which one is
// asking. Defaulting it here would pick one of them for all four, silently.
func errNoLevel(what string) error {
	return fmt.Errorf("chatlet: %s names no read level — a surface resolves an "+
		"absent read_level to its own default before it reads", what)
}

// Served is what every answer here carries beside its rows.
//
// THE POSITION AND THE COVERAGE TRAVEL WITH THE ROWS, because a caller cannot
// reconstruct either afterwards: [Reader.At] answers about now rather than
// about the transaction these rows came from, and a set read that could not
// account for everything is indistinguishable from a short one.
type Served struct {
	// Level is what the read was SERVED at, which is the level asked for
	// or a refusal — never the level requested, which is how a level
	// becomes a label.
	Level statelog.ReadLevel `json:"read_level"`

	// Complete is false when a deferred record's scope meets a SET read:
	// rooms may be missing, messages may be missing. A point read refuses
	// instead, because the object it is about may be the stale one.
	Complete bool `json:"complete"`

	// Position is the point on the log these rows were derived through.
	Position statelog.Position `json:"position"`

	// LogLag is ABSENT rather than zero when the broker could not be
	// reached: a read asks how far behind an answer may be, and an
	// unreachable broker answers "not at all".
	LogLag *uint64 `json:"log_lag,omitempty"`
}

// servedFrom is the framework's answer as this package reports it.
func servedFrom(a statelog.Answer) Served {
	return Served{
		Level: a.Level, Complete: a.Complete, Position: a.Position,
		LogLag: a.Lag,
	}
}

// ---- the rail: one person's rooms -------------------------------------- //

// ChannelsQuery asks for one person's channel list.
type ChannelsQuery struct {
	// Cursors is the viewer's own read state: a channel id against the
	// PACKED position they have read through, exactly as
	// [github.com/crewlet/crewlet/internal/coord.ChatReadState] holds it.
	//
	// IT IS A VALUE RATHER THAN A LOOKUP. Read state is the one piece of
	// chat state that is not on this log — it is a fact about one person's
	// attention that nobody replays — so it reaches this read from the
	// caller that already had to fetch it for the mute list and the
	// do-not-disturb window. An absent entry is a room this person has
	// never opened, whose whole tail is unread.
	Cursors map[string]int64

	// IncludeArchived keeps rooms that were closed to new messages. Off by
	// default: an archived room is readable for ever and belongs in a
	// search result or an explicit visit rather than in the list somebody
	// works out of.
	IncludeArchived bool

	// Unread keeps only the rooms with messages above the viewer's cursor.
	// It reads [ChannelsQuery.Cursors] as a MEASUREMENT: with none, every
	// room's whole tail counts as unread, so a caller that could not read
	// the person's read state must not ask for it.
	Unread bool

	// With keeps only the rooms this seat is ALSO a member of: the rooms
	// the viewer has in common with somebody, which a profile lists. Empty
	// is every room of the viewer's; the viewer's own handle is the same.
	With string

	// Limit is the most rooms the listing answers, the most recently
	// active first, cut from the one list sorted by activity and never
	// paged; zero is [MaxRailChannels]. A rail is answered whole, each room
	// with its last message and the viewer's read state with it, so a
	// caller bound to a size (the assistant's read, which one step holds to
	// 64 KiB) asks for the top of it.
	Limit int
}

// MessagePreview is the last thing said in a room, as a rail renders it.
type MessagePreview struct {
	MessageID  string     `json:"message_id"`
	Author     string     `json:"author,omitempty"`
	AuthorKind AuthorKind `json:"author_kind,omitempty"`

	// Excerpt is at most [MaxExcerpt] bytes, cut rune-safely by [Excerpt]
	// — a rail must not carry thirty-two kibibytes per room to draw one
	// line each.
	Excerpt string `json:"excerpt,omitempty" untrusted:"fence"`

	// ChannelSeq is the number this message landed at, so a live frame
	// carrying the next one can be applied rather than refetched.
	ChannelSeq int64 `json:"channel_seq"`

	// At is the BROKER'S own instant, which is what every node renders.
	At time.Time `json:"at"`

	// Deleted marks a tombstone: the row survives with its body blanked,
	// so the preview is empty rather than absent and a room does not look
	// as though nothing was ever said in it.
	Deleted bool `json:"deleted,omitempty"`

	// Position is where the record that last changed this message sits.
	// [statelog.Position.Packed] is what a read cursor stores, so this is
	// the value a client flushes once the room has been read.
	Position statelog.Position `json:"position"`
}

// ChannelSummary is one room as a rail renders it.
type ChannelSummary struct {
	Channel Channel `json:"channel"`

	// Revision is the room's own log revision, which is what a later edit
	// states as its expectation.
	Revision uint64 `json:"revision"`

	// FollowAll is THIS VIEWER'S membership flag: every message in the
	// room reaches them. It is per member rather than per room, which is
	// why it is here rather than on the channel.
	FollowAll bool `json:"follow_all,omitempty"`

	// Unread is how many messages sit above this viewer's cursor, counted
	// to [UnreadLimit]. Their OWN messages and the tombstones do not
	// count: a badge for something you said yourself, or for a body that
	// is no longer there, is a badge nobody can clear by reading.
	Unread int `json:"unread"`

	// UnreadCapped says the count stopped at [UnreadLimit] — the "99+"
	// case, stated rather than inferred from the number, so a caller
	// rendering "100" cannot mean two different things by it.
	UnreadCapped bool `json:"unread_capped,omitempty"`

	// Mentions is how many of the Unread messages name this viewer or the
	// whole room (`@channel`): the badge that says somebody is asking for
	// them. Counted over the same messages as Unread, so it is never more
	// than Unread and stops where UnreadCapped says that did. A room event
	// names nobody, and a muted room is counted all the same: the mute
	// silences the badge on the screen, and the screen is where it is
	// applied.
	Mentions int `json:"mentions"`

	// Last is the newest message, or nil in a room where nothing has been
	// said yet.
	Last *MessagePreview `json:"last,omitempty"`

	// Participants are the handles in a DIRECT conversation, which has no
	// name to render. Empty for every named room, whose address is its
	// name.
	Participants []string `json:"participants,omitempty"`
}

// ChannelListing is one person's rooms, newest activity first.
type ChannelListing struct {
	Served

	Channels []ChannelSummary `json:"channels"`

	// Truncated says the listing left rooms out: this person is in more
	// rooms than [MaxRailChannels], or more matched than the query's
	// [ChannelsQuery.Limit]. STATED rather than left to a caller counting,
	// because the list is sorted by activity after it is read: a caller
	// could not tell a truncated list from a complete one by its length
	// alone.
	Truncated bool `json:"truncated,omitempty"`

	// Unreadable is how many of this person's rooms this build could not
	// present: a document a newer build wrote, or a kind it cannot
	// classify. REPORTED rather than logged, because the caller is the one
	// who can see a room missing from a list, and refusing the whole rail
	// over one row would cost a person every other room they are in.
	Unreadable int `json:"unreadable,omitempty"`
}

// Channels answers one person's channel list.
//
// A SET READ over the whole domain, because the closure is every room this
// person is in and a scope cannot enumerate them — [MaxScopeTerms] is sixteen
// and a membership is bounded by [MaxRailChannels]. A deferred record
// therefore makes the list INCOMPLETE rather than refusing it: a rail that
// answered nothing because one room's record could not be decoded would take
// the company's whole conversation off the screen.
func (r *Reader) Channels(ctx context.Context, viewer string, q ChannelsQuery,
	fresh statelog.Freshness) (ChannelListing, error) {

	who, err := viewerOf(viewer)
	if err != nil {
		return ChannelListing{}, err
	}
	if fresh.Level == "" {
		return ChannelListing{}, errNoLevel("a channel list")
	}
	var out ChannelListing
	answer, err := r.log.Read(ctx, fresh.Query(ReadScope(""), true),
		func(tx *sql.Tx) error {
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			var err error
			out, err = r.rail(ctx, tx, who, q)
			return err
		})
	if err != nil {
		return ChannelListing{}, err
	}
	out.Served = servedFrom(answer)
	return out, nil
}

// Readable is every room this viewer may READ, each as a search needs it
// ([ReadableRoom]): its id to scope the index by, and the name and kind a hit
// in it is shown under.
//
// # The rail is not this set
//
// A rail is what somebody is IN, and [Visible]'s rule is wider than that: a
// public room and a unit's room are readable by any seat of the company,
// joined or not. A search driven off the rail would therefore answer nothing
// from exactly the rooms a company does most of its talking in, and would do
// it silently — an empty result reads as "nobody said that" rather than as
// "this search could not see the room".
//
// # Archived rooms are in
//
// An archive closes a room to new messages, not to reading it, and a search is
// the read where a closed room is most of the point: what a team concluded
// last quarter is in the room they stopped using. The rail leaves them out
// because it is the list somebody works out of; this is not that list.
//
// # Whole, never cut
//
// The WHOLE set materialises here, with no limit and so no truncation to
// report. That is what makes it safe to hand a search as its visible set: a
// partial one would narrow the answer without saying so, which is the failure
// the search's own [github.com/crewlet/crewlet/internal/search.ErrNoViewer]
// exists to refuse. It is NOT bounded by [MaxChannels], which counts live
// rooms only — archived rooms accumulate, since nothing deletes a room — and
// a limit at that cap cut the set, alphabetically and silently, for a company
// that had archived its way past it. What the set's size costs a search is
// the search's own concern: the index binds it in chunks sized from the
// store's measured parameter limit, so no count of rooms decides whether chat
// search works.
//
// An empty slice with no error is a viewer who may read nothing at all — a
// real state, and the caller's to tell apart from a viewer it failed to
// resolve, which is refused here by name.
func (r *Reader) Readable(ctx context.Context, viewer string,
	fresh statelog.Freshness) ([]ReadableRoom, error) {

	who, err := viewerOf(viewer)
	if err != nil {
		return nil, err
	}
	if fresh.Level == "" {
		return nil, errNoLevel("a readable channel set")
	}
	var out []ReadableRoom
	if _, err := r.log.Read(ctx, fresh.Query(ReadScope(""), true),
		func(tx *sql.Tx) error {
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			var err error
			out, err = readableChannels(ctx, tx, who)
			return err
		}); err != nil {
		return nil, err
	}
	return out, nil
}

// ReadableRoom is one room a viewer may read, as a search over everything
// they may read needs it: its id, which scopes the index, and the name and
// kind a hit in it is shown under, as a mention carries its room's
// ([Mention.ChannelName]).
//
// READ WITH THE SET, in its transaction, rather than looked up for the hits
// afterwards: the set is the authorization the index is handed, and a second
// read beside it could name a hit's room from another snapshot than the one
// that admitted it.
type ReadableRoom struct {
	ID   string `json:"id"`
	Kind Kind   `json:"kind"`
	// Name is the room's displayed name, empty for a direct conversation,
	// which has none.
	Name string `json:"name,omitempty"`
}

// readableChannels is [Reader.Readable] inside one transaction.
//
// THE SQL NARROWS AND GO DECIDES, which is [visibleWhere]'s own contract: the
// predicate drives `chat_members_handle_idx` and admits a superset, and every
// row it admits is still put through [Visible] — so a kind a newer peer wrote
// is left out here exactly as it is left out of a rail, rather than being
// served on the guess that it resembles a public room.
func readableChannels(ctx context.Context, tx *sql.Tx, who string) ([]ReadableRoom, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT c.id, c.kind, c.name FROM chat_channels c
		  WHERE `+visibleWhere+`
		  ORDER BY c.id`, who)
	if err != nil {
		return nil, fmt.Errorf("chatlet: read the rooms %q may search: %w", who, err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]ReadableRoom, 0, 16)
	for rows.Next() {
		var room ReadableRoom
		var kind string
		if err := rows.Scan(&room.ID, &kind, &room.Name); err != nil {
			return nil, fmt.Errorf("chatlet: scan a readable room: %w", err)
		}
		room.Kind = Kind(kind)
		// MEMBER IS TRUE because the predicate above already
		// established it for every private row it admitted, and the
		// flag is ignored for every row it did not have to.
		if !Visible(who, Channel{Kind: room.Kind}, true) {
			continue
		}
		out = append(out, room)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chatlet: read the rooms %q may search: %w", who, err)
	}
	return out, nil
}

// ---- the names: every room a viewer may read, as prose names it -------- //

// MaxRoomNames bounds how many rooms one names read answers.
//
// [MaxChannels], the most LIVE rooms a company can hold, so every live room a
// viewer may read always fits. Archived rooms are the only ones a company can
// hold more of than that, since nothing deletes a room, and they are the ones
// prose names least: the read answers the live rooms first, fills what is left
// with archived ones, the most recently archived first, and says when it left
// any out ([RoomNames.Truncated]). At the cap the answer is about a hundred
// kibibytes, read once by a reader's frame rather than on every screen.
const MaxRoomNames = MaxChannels

// RoomName is one room a viewer may read, as prose names it: its id, its name
// (the address a sentence writes as `#launch`), its kind and whether it is
// archived.
type RoomName struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     Kind   `json:"kind"`
	Archived bool   `json:"archived,omitempty"`
}

// RoomNames is every named room a viewer may read, the live ones first.
type RoomNames struct {
	Served

	// Rooms are the live rooms in name order, then the archived ones, the
	// most recently archived first. Never nil.
	Rooms []RoomName `json:"rooms"`

	// Truncated says rooms were left out past [MaxRoomNames]: a caller
	// that reads a name as a room only where this answer holds it reads
	// the rest by their shape, and has to know that it is doing so.
	Truncated bool `json:"truncated,omitempty"`
}

// RoomNames answers every NAMED room the viewer may read: [Reader.Readable]'s
// set, less the direct and group conversations, which have no name, and
// bounded at [MaxRoomNames].
//
// # Why a reader needs the names
//
// A room's name is any `[a-z0-9][a-z0-9_-]{0,63}` ([namePattern]), so `#e2e`
// and `#b2b` are spelled exactly as a colour is and `#2026` as an issue's
// number. Prose names rooms that way, a seat's and a model's alike, and a
// reader can only tell the room from the colour by knowing which rooms there
// are. The rail is not that set, for [Reader.Readable]'s reason: a public room
// and a unit's room are readable by every seat, joined or not, and they are
// the rooms a company names most.
//
// # Names, never a room's contents
//
// Each room is its id, its name, its kind and whether it is archived: nothing
// a [Visible] room does not already show the viewer, and nothing the rail adds
// (a room's last message, its unread count), which costs a scan per room. It
// is one statement over the rooms, read for a frame rather than per screen.
func (r *Reader) RoomNames(ctx context.Context, viewer string,
	fresh statelog.Freshness) (RoomNames, error) {

	who, err := viewerOf(viewer)
	if err != nil {
		return RoomNames{}, err
	}
	if fresh.Level == "" {
		return RoomNames{}, errNoLevel("the names of the rooms a viewer may read")
	}
	var out RoomNames
	answer, err := r.log.Read(ctx, fresh.Query(ReadScope(""), true),
		func(tx *sql.Tx) error {
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			var err error
			out, err = roomNames(ctx, tx, who)
			return err
		})
	if err != nil {
		return RoomNames{}, err
	}
	out.Served = servedFrom(answer)
	return out, nil
}

// roomNames is [Reader.RoomNames] inside one transaction.
//
// NO LIMIT IN THE SQL: [visibleWhere] admits a superset and [Visible] decides,
// so a LIMIT there could count a row this build then leaves out, and say
// [RoomNames.Truncated] of a set that fits. The walk stops instead at the first
// room past the cap that [Visible] keeps.
func roomNames(ctx context.Context, tx *sql.Tx, who string) (RoomNames, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT c.id, c.kind, c.name, c.archived_at IS NOT NULL FROM chat_channels c
		  WHERE c.name <> '' AND `+visibleWhere+`
		  ORDER BY c.archived_at IS NOT NULL, c.archived_at DESC, c.name_norm`, who)
	if err != nil {
		return RoomNames{}, fmt.Errorf("chatlet: read the names of the rooms %q may read: %w",
			who, err)
	}
	defer func() { _ = rows.Close() }()
	out := RoomNames{Rooms: make([]RoomName, 0, 16)}
	for rows.Next() {
		var room RoomName
		var kind string
		if err := rows.Scan(&room.ID, &kind, &room.Name, &room.Archived); err != nil {
			return RoomNames{}, fmt.Errorf("chatlet: scan a room's name: %w", err)
		}
		room.Kind = Kind(kind)
		// MEMBER IS TRUE for [readableChannels]' reason: the predicate
		// established it for every private row it admitted.
		if !Visible(who, Channel{Kind: room.Kind}, true) {
			continue
		}
		if len(out.Rooms) == MaxRoomNames {
			out.Truncated = true
			break
		}
		out.Rooms = append(out.Rooms, room)
	}
	if err := rows.Err(); err != nil {
		return RoomNames{}, fmt.Errorf("chatlet: read the names of the rooms %q may read: %w",
			who, err)
	}
	return out, nil
}

// rail is the whole listing inside one transaction.
//
// FOUR STATEMENT SHAPES, and the order is what keeps the cost proportional to
// the person rather than to the corpus: the membership rows (one index seek),
// the rooms themselves (a primary-key lookup per id, in one statement), then
// per room the newest message (one index seek) and — ONLY where that message
// is above this viewer's cursor — the counted range. A person who is caught up
// in a room pays nothing for its badge, which is the common case on every poll
// after the first.
func (r *Reader) rail(ctx context.Context, tx *sql.Tx, who string,
	q ChannelsQuery) (ChannelListing, error) {

	var out ChannelListing
	follow, order, err := railMemberships(ctx, tx, who, q.With)
	if err != nil {
		return ChannelListing{}, err
	}
	if len(order) > MaxRailChannels {
		out.Truncated = true
		order = order[:MaxRailChannels]
	}
	if len(order) == 0 {
		return out, nil
	}
	rooms, undecodable, err := railRooms(ctx, tx, order, q.IncludeArchived)
	if err != nil {
		return ChannelListing{}, err
	}
	out.Unreadable = undecodable
	direct := make([]string, 0, len(rooms))
	for _, entry := range rooms {
		if !Visible(who, entry.room, true) {
			// A ROOM THIS BUILD CANNOT CLASSIFY IS NOT SHOWN, and it
			// is counted with the documents it cannot decode: both
			// are a newer peer's row that this build must not
			// present on a guess about what it means.
			out.Unreadable++
			continue
		}
		id := entry.room.ID
		summary := ChannelSummary{
			Channel: entry.room, Revision: entry.revision,
			FollowAll: follow[id],
		}
		summary.Last, err = lastMessage(ctx, tx, id)
		if err != nil {
			return ChannelListing{}, err
		}
		summary.Unread, summary.UnreadCapped, summary.Mentions, err = unreadCount(
			ctx, tx, id, who, q.Cursors[id], summary.Last)
		if err != nil {
			return ChannelListing{}, err
		}
		if entry.room.Kind.Direct() {
			direct = append(direct, id)
		}
		out.Channels = append(out.Channels, summary)
	}
	if err := attachParticipants(ctx, tx, out.Channels, direct); err != nil {
		return ChannelListing{}, err
	}
	sortRail(out.Channels)
	if q.Unread {
		out.Channels = slices.DeleteFunc(out.Channels, func(c ChannelSummary) bool {
			return c.Unread == 0
		})
	}
	// THE TOP OF THE ONE SORTED LIST, never a page: the most recently active
	// rooms, the rest said to be left out.
	if q.Limit > 0 && len(out.Channels) > q.Limit {
		out.Channels = out.Channels[:q.Limit]
		out.Truncated = true
	}
	return out, nil
}

// railMemberships is the viewer's own member rows: their follow-all flag per
// room, and the room ids in a stable order.
//
// IT READS ONE MORE THAN THE CAP, which is what tells a membership that fits
// from one that was cut: a read limited to exactly [MaxRailChannels] comes
// back full in both cases, and the listing would then report a complete rail
// for a person whose rooms it had truncated.
//
// WITH ANOTHER SEAT NAMED, the same rows narrowed to the rooms that seat is in
// too, in the same statement: a seek on that seat's own member row per room
// rather than a second listing intersected afterwards.
func railMemberships(ctx context.Context, tx *sql.Tx, who, with string) (
	map[string]bool, []string, error) {

	query, args := `
		SELECT channel_id, follow_all
		  FROM chat_members
		 WHERE handle = ?
		 ORDER BY channel_id
		 LIMIT ?`, []any{who, MaxRailChannels + 1}
	if with = strings.TrimSpace(with); with != "" && with != who {
		query, args = `
		SELECT a.channel_id, a.follow_all
		  FROM chat_members a
		  JOIN chat_members b ON b.channel_id = a.channel_id AND b.handle = ?
		 WHERE a.handle = ?
		 ORDER BY a.channel_id
		 LIMIT ?`, []any{with, who, MaxRailChannels + 1}
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("chatlet: read %s's rooms: %w", who, err)
	}
	defer func() { _ = rows.Close() }()
	follow := map[string]bool{}
	var order []string
	for rows.Next() {
		var id string
		var followAll int
		if err := rows.Scan(&id, &followAll); err != nil {
			return nil, nil, fmt.Errorf("chatlet: scan one of %s's rooms: %w",
				who, err)
		}
		follow[id] = followAll != 0
		order = append(order, id)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("chatlet: read %s's rooms: %w", who, err)
	}
	return follow, order, nil
}

// railRoom is one room's row as the rail read it.
type railRoom struct {
	room     Channel
	revision uint64
}

// railRooms reads the rooms themselves, in ONE statement.
//
// A primary-key lookup per id rather than a join back to the membership,
// because `chat_members` carries a `version` of its own and the shared
// [ChannelRevision] expression names an unqualified column: a join would have
// to spell the room's revision a second way, which is how two answers to one
// revision start.
// IT REPORTS WHAT IT COULD NOT DECODE beside the rooms it could, rather than
// leaving the caller to subtract: a room filtered out because it is archived
// and one left out because a newer build wrote its document are the same
// absence from a length, and only one of them is worth telling somebody about.
func railRooms(ctx context.Context, tx *sql.Tx, ids []string,
	archived bool) ([]railRoom, int, error) {

	where := `id IN (` + placeholders(len(ids)) + `)`
	if !archived {
		where += ` AND archived_at IS NULL`
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT document, `+ChannelRevision+` FROM chat_channels
		  WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("chatlet: read a rail's rooms: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]railRoom, 0, len(ids))
	undecodable := 0
	for rows.Next() {
		var document []byte
		var revision int64
		if err := rows.Scan(&document, &revision); err != nil {
			return nil, 0, fmt.Errorf("chatlet: scan a rail's room: %w", err)
		}
		room, err := DecodeChannel(document)
		if err != nil {
			// LEFT OUT AND COUNTED rather than failing the rail: one
			// room a newer build wrote must not cost this person
			// every other room they are in.
			undecodable++
			continue
		}
		out = append(out, railRoom{room: room, revision: uint64(revision)})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("chatlet: read a rail's rooms: %w", err)
	}
	return out, undecodable, nil
}

// lastMessage is the newest message in a room, or nil where nothing has been
// said.
//
// ONE INDEX SEEK on `chat_messages_channel_idx`, which is ordered
// `(channel_id, channel_seq DESC)` precisely so this is the first row the
// index yields rather than an aggregate over the room.
//
// A ROOM EVENT IS NEVER THE PREVIEW. "Ana added Ravi" is not what anybody
// said in the room, so the rail shows the last thing somebody did say, and
// steps past the events above it on the same index.
func lastMessage(ctx context.Context, tx *sql.Tx, channelID string) (
	*MessagePreview, error) {

	var (
		preview    MessagePreview
		kind       string
		body       string
		at         int64
		deletedAt  sql.NullInt64
		stream     string
		generation int64
		seq        int64
	)
	err := tx.QueryRowContext(ctx, `
		SELECT id, author_handle, author_kind, body, channel_seq, created_at,
		       deleted_at, log_stream, log_generation, log_seq
		  FROM chat_messages
		 WHERE channel_id = ? AND event = ''
		 ORDER BY channel_seq DESC
		 LIMIT 1`, channelID).
		Scan(&preview.MessageID, &preview.Author, &kind, &body,
			&preview.ChannelSeq, &at, &deletedAt, &stream, &generation, &seq)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("chatlet: read the last message in room %s: %w",
			channelID, err)
	}
	preview.AuthorKind = AuthorKind(kind)
	preview.Excerpt = Excerpt(body)
	preview.At = store.DecodeTime(at)
	preview.Deleted = deletedAt.Valid
	preview.Position = statelog.Position{
		Stream: stream, Generation: uint32(generation), Seq: uint64(seq),
	}
	return &preview, nil
}

// unreadCount is the counted range above one person's cursor in one room.
//
// # Why the newest message decides whether anything is counted at all
//
// The cursor is a POSITION and the transcript is ordered by SEQUENCE, so the
// two meet only through a row's own `version`. The newest message carries the
// highest version any post in the room has, so a room whose newest message is
// at or below the cursor has nothing above it and the count is zero WITH NO
// SECOND STATEMENT — which is every room a person has already read, on every
// poll after the first.
//
// # Why the window is the room's tail
//
// Because a predicate that is not the index's own order cannot terminate. The
// rows above a cursor are the newest ones, so counting the room's newest
// [UnreadLimit] messages and keeping those above the cursor reads exactly that
// many rows, always — where `version > cursor` scanned in sequence order would
// walk the whole room to be sure it had found every straggler. The window can
// only lose rows in a room that is already past the cap, where the answer is
// the cap either way.
//
// A MESSAGE YOU WROTE IS NOT ONE YOU HAVE NOT READ, and a tombstone is not a
// message: both are excluded, because a badge nobody can clear by reading the
// room is a badge that trains people to ignore the rail. Nor is a room event,
// which is the room's change rather than anything said in it, so the window
// is the room's newest [UnreadLimit] MESSAGES, stepping past events on the
// same index.
//
// # And the mentions among them, in the same statement
//
// The third value is how many of the counted messages name the viewer
// (a `chat_mentions` row, one primary-key probe per message in the window) or
// the whole room (the row's own `collective`). A LEFT JOIN rather than a
// correlated EXISTS, because the pinned driver may rewrite a correlated
// subquery into a join over its whole child table ([storetest.Decorrelated]),
// and that child is every mention in the company.
func unreadCount(ctx context.Context, tx *sql.Tx, channelID, who string,
	cursor int64, last *MessagePreview) (unread int, capped bool, mentions int, err error) {

	if last == nil || last.Position.Packed() <= cursor {
		return 0, false, 0, nil
	}
	err = tx.QueryRowContext(ctx, unreadStatement,
		channelID, UnreadLimit, who, cursor, who).Scan(&unread, &mentions)
	if err != nil {
		return 0, false, 0, fmt.Errorf("chatlet: count what %s has not read in "+
			"room %s: %w", who, channelID, err)
	}
	return unread, unread >= UnreadLimit, mentions, nil
}

// unreadStatement is [unreadCount]'s one statement: the room's newest
// messages, those above the cursor that somebody else wrote and nobody took
// back, and of those the ones naming the reader or the room.
const unreadStatement = `
	SELECT COUNT(*),
	       COALESCE(SUM(CASE WHEN t.collective = 1 OR n.handle IS NOT NULL
	                         THEN 1 ELSE 0 END), 0)
	  FROM (SELECT id, version, author_handle, deleted_at, collective
	          FROM chat_messages
	         WHERE channel_id = ? AND event = ''
	         ORDER BY channel_seq DESC
	         LIMIT ?) t
	  LEFT JOIN chat_mentions n ON n.message_id = t.id AND n.handle = ?
	 WHERE t.version > ? AND t.author_handle <> ? AND t.deleted_at IS NULL`

// attachParticipants fills in who is in each DIRECT conversation, in one
// statement for the whole rail.
//
// A direct conversation has no name — its identity is derived from the sorted
// handles of the people in it — so the handles ARE what a client renders it
// by. Bounded by [MaxDMParticipants] per room, and read in one statement
// rather than one per room for the reason the wiki's label read gives: a
// person in forty conversations would otherwise take forty round trips to draw
// one rail.
func attachParticipants(ctx context.Context, tx *sql.Tx, summaries []ChannelSummary,
	direct []string) error {

	if len(direct) == 0 {
		return nil
	}
	args := make([]any, 0, len(direct))
	for _, id := range direct {
		args = append(args, id)
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT channel_id, handle FROM chat_members
		  WHERE channel_id IN (`+placeholders(len(direct))+`)
		  ORDER BY channel_id, handle`, args...)
	if err != nil {
		return fmt.Errorf("chatlet: read the participants of a rail's "+
			"conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	at := make(map[string]int, len(summaries))
	for i, summary := range summaries {
		at[summary.Channel.ID] = i
	}
	for rows.Next() {
		var id, handle string
		if err := rows.Scan(&id, &handle); err != nil {
			return fmt.Errorf("chatlet: scan a conversation's participant: %w", err)
		}
		if i, ok := at[id]; ok {
			summaries[i].Participants = append(summaries[i].Participants, handle)
		}
	}
	return rows.Err()
}

// sortRail orders a person's rooms the way they read them: newest activity
// first.
//
// IN GO RATHER THAN IN SQL, and that is what [MaxRailChannels] pays for. The
// ordering value is the newest message's instant, which lives in a different
// table from the membership the listing is driven off — so an ORDER BY would
// need every room's last message before the first row could be returned, which
// is the whole scan a bounded list plus one seek per room replaces.
//
// A ROOM WITH NOTHING IN IT SORTS BY ITS CREATE, so a room somebody has just
// made appears where they expect it rather than at the bottom for ever. The
// tie-break is the id, so two nodes answering one person's rail answer it in
// one order.
func sortRail(summaries []ChannelSummary) {
	slices.SortFunc(summaries, func(a, b ChannelSummary) int {
		if at := railActivity(b).Compare(railActivity(a)); at != 0 {
			return at
		}
		return cmp.Compare(a.Channel.ID, b.Channel.ID)
	})
}

func railActivity(s ChannelSummary) time.Time {
	if s.Last != nil {
		return s.Last.At
	}
	return s.Channel.CreatedAt
}

// ---- one room ---------------------------------------------------------- //

// ChannelMember is one row of a room's membership.
//
// RICHER THAN THE [Member] a record carries, deliberately: a record states
// what a writer meant to set — a handle and the follow-all flag — where these
// are the columns the applier derived beside it, and the one that matters is
// SOURCE. A unit room's membership is the org chart's, restated whole on every
// apply, and any other room's is somebody's decision; a screen that could not
// tell them apart would offer to remove a seat that the next apply puts
// straight back.
type ChannelMember struct {
	Handle string `json:"handle"`
	Role   string `json:"role,omitempty"`

	// Source is `org` for a member the unit reconcile added and
	// `explicit` for one somebody invited. See [MemberSourceOrg].
	Source string `json:"source,omitempty"`

	FollowAll bool      `json:"follow_all,omitempty"`
	JoinedAt  time.Time `json:"joined_at"`
}

// ChannelDetail is one room, its membership and what it is true as of.
type ChannelDetail struct {
	Served

	Channel Channel `json:"channel"`

	// Revision is the room's own log revision, which a later edit states
	// as its expectation.
	Revision uint64 `json:"revision"`

	// MessageSeq is the room's high-water mark: the number the next
	// message will take, minus one. It is NOT a count of what is in the
	// room — a retention prune removes rows and deliberately does not move
	// this, because the sequence has to stay unique for ever.
	MessageSeq int64 `json:"message_seq"`

	Members []ChannelMember `json:"members,omitempty"`

	// Member says whether the VIEWER is in the room, which a public room
	// answers no to without that meaning they cannot read it.
	Member bool `json:"member"`
}

// Channel reads one room's metadata and its membership.
//
// A POINT READ, so a deferred scope REFUSES rather than answering incomplete:
// this answer is about one room, and if that room's own scope is stale there
// is nothing honest to return — its topic may have changed, and so may the
// membership that decides who may read it at all.
func (r *Reader) Channel(ctx context.Context, viewer, channelID string,
	fresh statelog.Freshness) (ChannelDetail, error) {

	who, err := viewerOf(viewer)
	if err != nil {
		return ChannelDetail{}, err
	}
	if fresh.Level == "" {
		return ChannelDetail{}, errNoLevel("a channel read")
	}
	var out ChannelDetail
	answer, err := r.log.Read(ctx, fresh.Query(ReadScope(channelID), false),
		func(tx *sql.Tx) error {
			//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			room, revision, seq, err := readRoom(ctx, tx, channelID)
			if err != nil {
				return err
			}
			members, err := readMembers(ctx, tx, channelID)
			if err != nil {
				return err
			}
			out = ChannelDetail{
				Channel: room, Revision: revision, MessageSeq: seq,
				Members: members, Member: memberIn(members, who),
			}
			if !Visible(who, room, out.Member) {
				return notFound()
			}
			return nil
		})
	if err != nil {
		return ChannelDetail{}, err
	}
	out.Served = servedFrom(answer)
	return out, nil
}

// ChannelNamed is one room by its NAME — the address it was created under —
// answered as [Reader.Channel] answers one by id: the same detail, the same
// [Visible], and NOT FOUND for a room this viewer may not read, so a private
// room's name discloses exactly as much as its id does.
//
// # Why a name is an address here and not only on somebody's rail
//
// Because a name IS the room's address — unique across the company, claimed
// once and never renamed — and every sentence anybody writes about a room uses
// it. Resolved only against the rooms a caller is IN, a public room or a unit's
// room nobody here had joined was readable and writable by id while its name
// meant nothing — and nothing a seat can call hands it that id. A decision may
// promise its outcome to any unit's room, so an asker outside that unit owed a
// post to a room it could name and could not reach, and its turn was sent back
// until its cap.
//
// # What the read is about
//
// The ADDRESS, and the room it resolves to: a deferred record claiming this
// name — a newer peer's create — refuses the read rather than answering "no
// such room" about a room that is on its way. Resolved in the answer's own
// transaction, because the room's id is what the name is being read to find.
func (r *Reader) ChannelNamed(ctx context.Context, viewer, name string,
	fresh statelog.Freshness) (ChannelDetail, error) {

	who, err := viewerOf(viewer)
	if err != nil {
		return ChannelDetail{}, err
	}
	norm := NormalizeName(name)
	if norm == "" {
		return ChannelDetail{}, invalid("name", "a read by name names no room")
	}
	if fresh.Level == "" {
		return ChannelDetail{}, errNoLevel("a channel read by name")
	}
	address := ScopeTerm{Kind: TermName, ID: ChannelToken(norm)}.Path()
	var out ChannelDetail
	answer, err := r.log.Read(ctx, fresh.Resolved(func(ctx context.Context,
		tx *sql.Tx) (statelog.ScopeSet, error) {

		id, found, missed := channelAt(ctx, tx, norm)
		if missed != nil || !found {
			return statelog.ScopeSet{Paths: []string{address}}, missed
		}
		return statelog.ScopeSet{Paths: []string{address,
			ScopeTerm{Kind: TermChannel, ID: id}.Path()}}, nil
	}, false), func(tx *sql.Tx) error {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		id, found, err := channelAt(ctx, tx, norm)
		switch {
		case err != nil:
			return err
		case !found:
			return fmt.Errorf("%w: no room is called %q", ErrNotFound, norm)
		}
		room, revision, seq, err := readRoom(ctx, tx, id)
		if err != nil {
			return err
		}
		members, err := readMembers(ctx, tx, id)
		if err != nil {
			return err
		}
		out = ChannelDetail{
			Channel: room, Revision: revision, MessageSeq: seq,
			Members: members, Member: memberIn(members, who),
		}
		if !Visible(who, room, out.Member) {
			// THE SAME SENTENCE AS A NAME NOTHING HOLDS, for [notFound]'s
			// reason: "not yours" would confirm the room exists.
			return fmt.Errorf("%w: no room is called %q", ErrNotFound, norm)
		}
		return nil
	})
	if err != nil {
		return ChannelDetail{}, err
	}
	out.Served = servedFrom(answer)
	return out, nil
}

// channelAt is the room holding an address, read from the claim the create
// took — the row that says a name is taken, which nothing ever deletes.
func channelAt(ctx context.Context, tx *sql.Tx, norm string) (string, bool, error) {
	var id string
	err := tx.QueryRowContext(ctx,
		`SELECT channel_id FROM chat_channel_names WHERE name_token = ?`,
		ChannelToken(norm)).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("chatlet: read the room called %q: %w", norm, err)
	}
	return id, true, nil
}

// readRoom is one room's row, through its DOCUMENT.
//
// Through the document although every field it holds is also a column, for the
// reason the write path reads it that way: the document is what carries a
// NEWER BUILD'S fields through this node, and a room rebuilt from columns
// would answer without them.
func readRoom(ctx context.Context, tx *sql.Tx, channelID string) (
	Channel, uint64, int64, error) {

	if strings.TrimSpace(channelID) == "" {
		return Channel{}, 0, 0, invalid("channel_id", "a read names no room")
	}
	var document []byte
	var revision, seq int64
	err := tx.QueryRowContext(ctx,
		`SELECT document, `+ChannelRevision+`, message_seq
		   FROM chat_channels WHERE id = ?`, channelID).
		Scan(&document, &revision, &seq)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Channel{}, 0, 0, notFound()
	case err != nil:
		return Channel{}, 0, 0, fmt.Errorf("chatlet: read room %s: %w",
			channelID, err)
	}
	room, err := DecodeChannel(document)
	if err != nil {
		return Channel{}, 0, 0, err
	}
	return room, uint64(revision), seq, nil
}

// notFound is what a room somebody may not read answers, and what a room that
// is not there answers.
//
// ONE ERROR FOR BOTH, which is the point: a private room's existence is itself
// information, so a refusal that said "not yours" would answer "who is talking
// to whom" for anybody willing to guess an id.
//
// AND NAMING NO ROOM: the caller named it, and the one thing this could add
// is the room's id, which nobody chose.
func notFound() error {
	return fmt.Errorf("%w: there's no such room", ErrNotFound)
}

// readMembers is a room's whole membership, in handle order.
//
// THE ORDER IS THE APPLIER'S OWN, so a caller comparing this against a set it
// is about to write compares two slices in one order rather than two sets
// through a map. Bounded by [MaxMembers], which is what a membership record
// may carry in the first place.
func readMembers(ctx context.Context, tx *sql.Tx, channelID string) (
	[]ChannelMember, error) {

	rows, err := tx.QueryContext(ctx, `
		SELECT handle, role, source, follow_all, joined_at
		  FROM chat_members
		 WHERE channel_id = ?
		 ORDER BY handle
		 LIMIT ?`, channelID, MaxMembers)
	if err != nil {
		return nil, fmt.Errorf("chatlet: read room %s's members: %w", channelID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []ChannelMember
	for rows.Next() {
		var m ChannelMember
		var followAll int
		var joined int64
		if err := rows.Scan(&m.Handle, &m.Role, &m.Source, &followAll,
			&joined); err != nil {

			return nil, fmt.Errorf("chatlet: scan a member of room %s: %w",
				channelID, err)
		}
		m.FollowAll = followAll != 0
		m.JoinedAt = store.DecodeTime(joined)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chatlet: read room %s's members: %w", channelID, err)
	}
	return out, nil
}

// memberIn reports whether a handle is in a membership already read.
//
// OVER THE SLICE RATHER THAN A SECOND STATEMENT, because every caller here has
// just read the membership for its own sake: a point query beside it would be
// a second answer to the same question, read at the same instant, for the cost
// of another round trip.
func memberIn(members []ChannelMember, who string) bool {
	return slices.ContainsFunc(members, func(m ChannelMember) bool {
		return m.Handle == who
	})
}

// memberOf answers the same question where the membership has NOT been read —
// a transcript page, which needs the flag and not the thousand rows.
func memberOf(ctx context.Context, tx *sql.Tx, channelID, who string) (bool, error) {
	var present int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chat_members WHERE channel_id = ? AND handle = ?`,
		channelID, who).Scan(&present)
	if err != nil {
		return false, fmt.Errorf("chatlet: read whether %s is in room %s: %w",
			who, channelID, err)
	}
	return present > 0, nil
}

// ---- the transcript ---------------------------------------------------- //

// ReactionCount is one emoji on one message, as a transcript renders it.
//
// A COUNT AND A FLAG RATHER THAN THE HANDLES. A message may carry
// [MaxReactionEmoji] distinct emoji and a room may hold [MaxMembers] people,
// so the handles are a product this read must not put on a page of fifty
// messages — and what a screen draws is the number and whether the reader is
// in it.
type ReactionCount struct {
	Emoji string `json:"emoji"`
	Count int    `json:"count"`

	// Mine says the viewer is one of them, which is what a client toggles
	// on.
	Mine bool `json:"mine,omitempty"`
}

// MessageView is one message as a reader renders it.
//
// THE DOCUMENT PLUS THE TWO VALUES THE APPLIER MINTED, which the document
// deliberately does not carry: the per-channel sequence, and the position of
// the record that last changed the row. Both are read rather than derived,
// because they are the applier's arithmetic and a second copy in the blob
// would be a second answer every edit would have to carry forward.
type MessageView struct {
	Message Message `json:"message"`

	// ChannelSeq is contiguous within the room, which is what lets a
	// client tell a live frame it can apply from a hole it must refetch:
	// holding 41 and handed 43, it asks for 42 rather than for the room.
	ChannelSeq int64 `json:"channel_seq"`

	// Position is where the record that last changed this row sits.
	// [statelog.Position.Packed] is what a read cursor stores.
	Position statelog.Position `json:"position"`

	Reactions []ReactionCount `json:"reactions,omitempty"`

	// Thread is the replies under a room post, nil on a reply, on a room
	// event and on a post nobody answered.
	Thread *ThreadSummary `json:"thread,omitempty"`
}

// ThreadSummary is a root's replies, as a transcript shows them under it.
//
// READ, NEVER KEPT: counted from the thread's own rows when the page is read
// (`chat_messages_thread_idx`), so a delete, an erase and a prune that change
// what a thread holds change its summary with no counter for each of them to
// keep in step.
type ThreadSummary struct {
	// Replies is every reply, a tombstone included: it still sits in the
	// thread, under the words it replaced.
	Replies int `json:"replies"`

	// LastReplyAt is the newest reply's broker instant, and
	// LastReplyPosition its record, whose [statelog.Position.Packed] a
	// screen compares with the reader's cursor to tell a thread that moved
	// past what they have read.
	LastReplyAt       time.Time         `json:"last_reply_at"`
	LastReplyPosition statelog.Position `json:"last_reply_position"`

	// Repliers are the first [MaxThreadRepliers] seats to reply, in the
	// order of their first reply, and ReplierCount how many replied in all.
	Repliers     []string `json:"repliers"`
	ReplierCount int      `json:"replier_count"`
}

// TranscriptQuery asks for a page of one room.
type TranscriptQuery struct {
	ChannelID string

	// Cursor is the `channel_seq` of the last row of the previous page,
	// rendered as the decimal [Transcript.NextCursor] handed back. A page
	// is NEWEST FIRST, so the next page is what sits BELOW it.
	Cursor string

	// Limit is how many messages one page carries: [DefaultLimit] when
	// unset, [MaxLimit] at the top.
	Limit int
}

// Transcript is a page of one room.
type Transcript struct {
	Served

	ChannelID string        `json:"channel_id"`
	Messages  []MessageView `json:"messages"`

	// NextCursor is where the next page starts, empty at the end of the
	// room. It is the EXTRA ROW'S evidence rather than the last row of
	// this page being assumed to have a successor, so a caller is never
	// sent back for a page that is empty.
	NextCursor string `json:"next_cursor,omitempty"`

	// Unreadable is how many rows on this page this build could not
	// decode — a message a newer peer wrote. Reported rather than logged,
	// for [ChannelListing.Unreadable]'s reason.
	Unreadable int `json:"unreadable,omitempty"`
}

// Messages answers a page of one room's transcript, newest first.
//
// A POINT READ on the room ([Reader.Channel]'s reasoning): a deferred record
// in this room is a message that would have been on this page, and the room's
// own scope is what the probe compares against, so refusing is both honest and
// available — another node can answer.
func (r *Reader) Messages(ctx context.Context, viewer string, q TranscriptQuery,
	fresh statelog.Freshness) (Transcript, error) {

	who, err := viewerOf(viewer)
	if err != nil {
		return Transcript{}, err
	}
	if fresh.Level == "" {
		return Transcript{}, errNoLevel("a transcript read")
	}
	before, err := parseSeqCursor(q.Cursor)
	if err != nil {
		return Transcript{}, err
	}
	out := Transcript{ChannelID: q.ChannelID}
	answer, err := r.log.Read(ctx, fresh.Query(ReadScope(q.ChannelID), false),
		func(tx *sql.Tx) error {
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			if err := r.reachable(ctx, tx, who, q.ChannelID); err != nil {
				return err
			}
			where := []string{"channel_id = ?"}
			args := []any{q.ChannelID}
			if before > 0 {
				// STRICTLY BELOW: the page is newest-first, so the
				// cursor names the last row of the previous page
				// and a `<=` would repeat it on every page.
				where = append(where, "channel_seq < ?")
				args = append(args, before)
			}
			//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			page, err := readMessagePage(ctx, tx, who, where, args,
				"channel_seq DESC", limitOf(q.Limit))
			if err != nil {
				return err
			}
			out.Messages, out.NextCursor, out.Unreadable =
				page.views, page.next, page.undecodable
			return nil
		})
	if err != nil {
		return Transcript{}, err
	}
	out.Served = servedFrom(answer)
	return out, nil
}

// ---- one thread -------------------------------------------------------- //

// ThreadQuery asks for one thread from its root.
type ThreadQuery struct {
	ChannelID string

	// RootID is the message the thread hangs off. A thread here is ONE
	// LEVEL DEEP by construction — a reply to a reply carries the same
	// root — which is what makes this a range scan rather than a walk.
	RootID string

	// Cursor is the `channel_seq` of the last reply of the previous page.
	// A thread is read OLDEST FIRST, because a conversation is read
	// forwards, so the next page is what sits ABOVE it.
	Cursor string

	Limit int

	// Newest, where set, answers the NEWEST that many replies still
	// standing instead of a page from the start — still oldest first among
	// themselves, behind the root — and counts the rest in [Thread.Earlier].
	// Held to [MaxLimit]; refused beside a Cursor, which pages the other way.
	//
	// A DELETED REPLY IS STEPPED OVER, because the newest end is read to be
	// read: a tombstone is a blank line a reader drops, and one answered in
	// the window is a reply that could have been there in its place. It is
	// counted in Earlier with the rest — the thread is no shorter for it.
	//
	// It is what a seat woken in a thread reads ([ThreadReader]): the
	// message that woke it is the thread's newest, and paging forwards from
	// the root to reach it is a walk the length of the thread — where the
	// index this read drives on ([chat_messages_thread_idx]) is a seek
	// from either end.
	Newest int
}

// Thread is one thread: its root, its replies and who has spoken in it.
type Thread struct {
	Served

	ChannelID string      `json:"channel_id"`
	Root      MessageView `json:"root"`

	// Replies are in the order they were said, which is the order they
	// are read in.
	Replies []MessageView `json:"replies,omitempty"`

	// Participants is who has spoken here, DERIVED BY THE APPLIER from the
	// messages themselves rather than claimed by any record. It is not a
	// follow: taking part in a thread means a reply reaches you as
	// [ReasonReply], which obliges nothing.
	Participants []string `json:"participants,omitempty"`

	NextCursor string `json:"next_cursor,omitempty"`
	Unreadable int    `json:"unreadable,omitempty"`

	// Earlier is how many replies a read of the [ThreadQuery.Newest] end did
	// not answer: those before the window, and the deleted ones it stepped
	// over — the count a reader states rather than leaving the thread to
	// look shorter than it is. Never the replies inside the window this
	// build could not decode, which [Thread.Unreadable] counts, so a reader
	// adding the two counts each reply once. Zero on a read from the start,
	// whose remainder is [Thread.NextCursor]'s to page.
	Earlier int `json:"earlier,omitempty"`
}

// Thread answers one thread from its root.
//
// THE ROOT IS READ EVEN WHEN IT IS A TOMBSTONE, which is what the tombstone is
// for: a delete blanks the body and keeps the row precisely so a thread does
// not silently lose the message it hangs off. A root that is not there at all
// — erased, or pruned below the horizon — is [ErrNotFound], because the thread
// then has no first line for a reader to place the replies against.
func (r *Reader) Thread(ctx context.Context, viewer string, q ThreadQuery,
	fresh statelog.Freshness) (Thread, error) {

	who, err := viewerOf(viewer)
	if err != nil {
		return Thread{}, err
	}
	if fresh.Level == "" {
		return Thread{}, errNoLevel("a thread read")
	}
	if strings.TrimSpace(q.RootID) == "" {
		return Thread{}, invalid("root_id", "a thread read names no root — a "+
			"thread is addressed by the message it hangs off, and the room's "+
			"own transcript is `Messages`")
	}
	if q.Newest < 0 || (q.Newest > 0 && q.Cursor != "") {
		return Thread{}, invalid("newest", "a thread read asks for the newest "+
			"%d replies beside a cursor — the cursor pages forward from the "+
			"root and the newest end is read in one, so a read names one or "+
			"the other", q.Newest)
	}
	after, err := parseSeqCursor(q.Cursor)
	if err != nil {
		return Thread{}, err
	}
	out := Thread{ChannelID: q.ChannelID}
	answer, err := r.log.Read(ctx, fresh.Query(ReadScope(q.ChannelID), false),
		func(tx *sql.Tx) error {
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			if err := r.reachable(ctx, tx, who, q.ChannelID); err != nil {
				return err
			}
			//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			root, err := readMessagePage(ctx, tx, who,
				[]string{"channel_id = ?", "id = ?"},
				[]any{q.ChannelID, q.RootID}, "channel_seq", 1)
			if err != nil {
				return err
			}
			if len(root.views) == 0 {
				if root.undecodable > 0 {
					// THE ROOT IS THERE AND THIS BUILD CANNOT READ
					// IT, which is not the same fact as a thread
					// that is not there: one clears when this node
					// is upgraded and the other never does. A
					// `not found` here would send somebody looking
					// for a message that is sitting in the room.
					return fmt.Errorf("chatlet: this thread's first message " +
						"was written by a newer build than this node runs, so " +
						"this node can't show the thread. Another node can, " +
						"and this one can once it's upgraded")
				}
				return fmt.Errorf("%w: there's no such thread in this room",
					ErrNotFound)
			}
			out.Root = root.views[0]

			where := []string{"channel_id = ?", "thread_root = ?"}
			args := []any{q.ChannelID, q.RootID}
			if q.Newest > 0 {
				//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
				tail, earlier, err := readThreadTail(ctx, tx, who,
					q.ChannelID, q.RootID, q.Newest)
				if err != nil {
					return err
				}
				out.Replies, out.Earlier = tail.views, earlier
				out.Unreadable = root.undecodable + tail.undecodable
				out.Participants, err = threadParticipants(ctx, tx,
					q.ChannelID, q.RootID)
				return err
			}
			if after > 0 {
				where = append(where, "channel_seq > ?")
				args = append(args, after)
			}
			page, err := readMessagePage(ctx, tx, who, where, args,
				"channel_seq", limitOf(q.Limit))
			if err != nil {
				return err
			}
			out.Replies, out.NextCursor = page.views, page.next
			out.Unreadable = root.undecodable + page.undecodable
			out.Participants, err = threadParticipants(ctx, tx, q.ChannelID,
				q.RootID)
			return err
		})
	if err != nil {
		return Thread{}, err
	}
	out.Served = servedFrom(answer)
	return out, nil
}

// readThreadTail reads a thread's newest n replies still standing, oldest
// first among themselves, and counts the rest — see [ThreadQuery.Newest].
//
// THE NEWEST END, read off the thread's index backwards and handed back in the
// order a conversation is read, stepping over a tombstone. The rest is COUNTED
// rather than paged, in the same snapshot, so the number is the thread's and
// not a guess — every reply, deleted or not, less the ones answered and the
// ones in the window this build cannot decode, which are the page's own
// undecodable count.
func readThreadTail(ctx context.Context, tx *sql.Tx, who, channelID, root string,
	n int) (messagePage, int, error) {

	where := []string{"channel_id = ?", "thread_root = ?", "deleted_at IS NULL"}
	page, err := readMessagePage(ctx, tx, who, where, []any{channelID, root},
		"channel_seq DESC", min(n, MaxLimit))
	if err != nil {
		return messagePage{}, 0, err
	}
	slices.Reverse(page.views)
	// A CURSOR MINTED FOR THE OTHER DIRECTION, which a forward page would
	// read as "replies after this one" — nothing a tail read hands back.
	page.next = ""
	var total int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM chat_messages
		 WHERE channel_id = ? AND thread_root = ?`, channelID, root).
		Scan(&total); err != nil {
		return messagePage{}, 0, fmt.Errorf("chatlet: count thread %s's replies: %w",
			root, err)
	}
	return page, max(total-len(page.views)-page.undecodable, 0), nil
}

// threadParticipants is who has spoken in one thread.
//
// Bounded by [MaxThreadParticipants], which is the bound the routing reads
// them under: past a dozen or so voices a thread is a meeting, and what
// matters there is the mention.
func threadParticipants(ctx context.Context, tx *sql.Tx, channelID, root string) (
	[]string, error) {

	rows, err := tx.QueryContext(ctx, `
		SELECT handle FROM chat_thread_participants
		 WHERE channel_id = ? AND thread_root = ?
		 ORDER BY handle LIMIT ?`,
		channelID, root, MaxThreadParticipants)
	if err != nil {
		return nil, fmt.Errorf("chatlet: read who has spoken in thread %s: %w",
			root, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var handle string
		if err := rows.Scan(&handle); err != nil {
			return nil, fmt.Errorf("chatlet: scan a participant of thread %s: %w",
				root, err)
		}
		out = append(out, handle)
	}
	return out, rows.Err()
}

// reachable refuses a read of a room this viewer may not open, as NOT FOUND.
//
// IT READS THE ROOM AND ONE MEMBERSHIP ROW rather than the whole membership:
// [Visible] needs the kind and one flag, and a transcript page must not pay
// for a thousand handles to establish that the reader is one of them.
func (r *Reader) reachable(ctx context.Context, tx *sql.Tx, who,
	channelID string) error {

	room, _, _, err := readRoom(ctx, tx, channelID)
	if err != nil {
		return err
	}
	member := false
	if privateRoom(room.Kind) {
		// ONLY WHERE IT DECIDES SOMETHING. A public or a unit room
		// refuses nobody, so the membership probe there is a statement
		// per read whose answer changes nothing.
		if member, err = memberOf(ctx, tx, channelID, who); err != nil {
			return err
		}
	}
	if !Visible(who, room, member) {
		return notFound()
	}
	return nil
}

// ---- the message page, shared by the transcript and the thread --------- //

// messagePage is one page of messages plus what could not be decoded and where
// the next page starts.
type messagePage struct {
	views       []MessageView
	next        string
	undecodable int
}

// readMessagePage reads one window of `chat_messages` and everything hanging
// off it.
//
// ONE FUNCTION FOR THE TRANSCRIPT AND THE THREAD, because they differ in
// exactly two values — the predicate and the direction — and everything after
// the scan is identical: decode the document, carry the minted sequence and
// the position, and attach the reactions in one statement. Written twice, the
// second copy is where a page stops carrying reactions or starts overrunning
// its limit.
//
// THE WINDOW IS BOUND HERE, not by the caller. The placeholders are
// positional, so a caller that appended the limit to its own `args` would be
// one reordering away from paging a room by a channel id — which reads exactly
// like a room nobody has written in.
func readMessagePage(ctx context.Context, tx *sql.Tx, who string,
	where []string, args []any, order string, limit int) (messagePage, error) {

	// ONE MORE THAN THE LIMIT, and it is the cursor's evidence rather than
	// an answer: a page that returned it would overrun what the caller
	// asked for, and a NextCursor minted without it would send every
	// caller back for one empty page at the end of every room.
	args = append(slices.Clip(args), limit+1)
	rows, err := tx.QueryContext(ctx, `
		SELECT id, document, channel_seq, log_stream, log_generation, log_seq
		  FROM chat_messages
		 WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY `+order+`
		 LIMIT ?`, args...)
	if err != nil {
		return messagePage{}, fmt.Errorf("chatlet: read a page of messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out messagePage
	ids := make([]string, 0, limit)
	var lastSeq int64
	var held int
	more := false
	for rows.Next() {
		if len(ids) == limit {
			more = true
			break
		}
		var (
			id         string
			document   []byte
			seq        int64
			stream     string
			generation int64
			position   int64
		)
		if err := rows.Scan(&id, &document, &seq, &stream, &generation,
			&position); err != nil {

			return messagePage{}, fmt.Errorf("chatlet: scan a message: %w", err)
		}
		// THE BYTE BOUND ([pageBytes]), past the first message — a page
		// that held nothing would be a cursor that never moves — and
		// before this row is taken, so the cursor names the last one
		// the page holds and the next page starts at this one.
		if len(ids) > 0 && held+len(document) > pageBytes {
			more = true
			break
		}
		held += len(document)
		// THE CURSOR IS THE LAST ROW SCANNED, not the last one decoded.
		// A message this build cannot read still occupies a place in the
		// room's sequence, and a cursor that skipped it would hand the
		// next page a starting point above rows it never returned.
		lastSeq = seq
		ids = append(ids, id)
		message, err := DecodeMessage(document)
		if err != nil {
			out.undecodable++
			continue
		}
		out.views = append(out.views, MessageView{
			Message: message, ChannelSeq: seq,
			Position: statelog.Position{
				Stream: stream, Generation: uint32(generation),
				Seq: uint64(position),
			},
		})
	}
	if err := rows.Err(); err != nil {
		return messagePage{}, fmt.Errorf("chatlet: read a page of messages: %w", err)
	}
	if more && lastSeq > 0 {
		out.next = strconv.FormatInt(lastSeq, 10)
	}
	if err := attachReactions(ctx, tx, who, out.views, ids); err != nil {
		return messagePage{}, err
	}
	return out, attachThreads(ctx, tx, out.views)
}

// attachThreads puts each room post's thread summary on it, in two statements
// per room on the page: every replier of every root with their count and
// their first and last reply, then the newest reply of each root.
//
// PER ROOM, because the thread index leads with the room
// (`chat_messages_thread_idx`, `(channel_id, thread_root, channel_seq)`), and
// a page a reader assembled from several rooms (the saved messages) seeks
// each room's roots in its own range. A transcript page is one room.
func attachThreads(ctx context.Context, tx *sql.Tx, views []MessageView) error {
	roots := map[string][]string{}
	var rooms []string
	at := map[string]int{}
	for i, view := range views {
		m := view.Message
		if m.ThreadRoot != "" || m.Event != nil {
			continue
		}
		if _, seen := roots[m.ChannelID]; !seen {
			rooms = append(rooms, m.ChannelID)
		}
		roots[m.ChannelID] = append(roots[m.ChannelID], m.ID)
		at[m.ID] = i
	}
	for _, room := range rooms {
		if err := attachRoomThreads(ctx, tx, room, roots[room], views, at); err != nil {
			return err
		}
	}
	return nil
}

// attachRoomThreads is [attachThreads] for one room's roots.
func attachRoomThreads(ctx context.Context, tx *sql.Tx, room string, ids []string,
	views []MessageView, at map[string]int) error {

	args := make([]any, 0, len(ids)+1)
	args = append(args, room)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT thread_root, author_handle, COUNT(*), MIN(channel_seq), MAX(channel_seq)
		  FROM chat_messages
		 WHERE channel_id = ? AND thread_root IN (`+placeholders(len(ids))+`)
		 GROUP BY thread_root, author_handle
		 ORDER BY thread_root, MIN(channel_seq)`, args...)
	if err != nil {
		return fmt.Errorf("chatlet: read room %s's threads: %w", room, err)
	}
	newestOf := map[string]int64{}
	for rows.Next() {
		var (
			root, author string
			count        int
			first, last  int64
		)
		if err = rows.Scan(&root, &author, &count, &first, &last); err != nil {
			_ = rows.Close()
			return fmt.Errorf("chatlet: scan a thread's replier: %w", err)
		}
		i, ok := at[root]
		if !ok {
			continue
		}
		summary := views[i].Thread
		if summary == nil {
			summary = &ThreadSummary{Repliers: []string{}}
			views[i].Thread = summary
		}
		summary.Replies += count
		summary.ReplierCount++
		if len(summary.Repliers) < MaxThreadRepliers {
			summary.Repliers = append(summary.Repliers, author)
		}
		if last > newestOf[root] {
			newestOf[root] = last
		}
	}
	if err = closeRows(rows, "room "+room+"'s threads"); err != nil {
		return err
	}
	if len(newestOf) == 0 {
		return nil
	}
	// A ROOM'S SEQUENCE IS ITS OWN AND UNIQUE in it, so each newest reply
	// names its root back.
	newest := make(map[int64]string, len(newestOf))
	for root, seq := range newestOf {
		newest[seq] = root
	}
	seqArgs := make([]any, 0, len(newest)+1)
	seqArgs = append(seqArgs, room)
	for seq := range newest {
		seqArgs = append(seqArgs, seq)
	}
	last, err := tx.QueryContext(ctx, `
		SELECT channel_seq, created_at, log_stream, log_generation, log_seq
		  FROM chat_messages
		 WHERE channel_id = ? AND channel_seq IN (`+placeholders(len(newest))+`)`,
		seqArgs...)
	if err != nil {
		return fmt.Errorf("chatlet: read room %s's newest replies: %w", room, err)
	}
	for last.Next() {
		var (
			seq, created, generation, position int64
			stream                             string
		)
		if err = last.Scan(&seq, &created, &stream, &generation, &position); err != nil {
			_ = last.Close()
			return fmt.Errorf("chatlet: scan a thread's newest reply: %w", err)
		}
		summary := views[at[newest[seq]]].Thread
		summary.LastReplyAt = store.DecodeTime(created)
		summary.LastReplyPosition = statelog.Position{
			Stream: stream, Generation: uint32(generation), Seq: uint64(position),
		}
	}
	return closeRows(last, "room "+room+"'s newest replies")
}

// attachReactions puts each message's reactions on it, in ONE statement for
// the whole page.
//
// AGGREGATED IN SQL rather than read as rows: the handles behind a count are
// [MaxReactionEmoji] times a room's membership, which is a product no page of
// messages may carry to draw a row of emoji. The viewer's own membership of
// each set comes back as part of the same aggregate, so a client can render
// the toggle without a second question.
//
// THE VIEWER IS THE FIRST BIND, before the page's ids, because the
// placeholders are positional and the `CASE` sits in the select list ahead of
// the `IN`. One page is at most [MaxLimit] ids plus this one, which is inside
// the 999 parameters the store guarantees.
func attachReactions(ctx context.Context, tx *sql.Tx, who string,
	views []MessageView, ids []string) error {

	if len(views) == 0 || len(ids) == 0 {
		return nil
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, who)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT message_id, emoji, COUNT(*),
		       MAX(CASE WHEN handle = ? THEN 1 ELSE 0 END)
		  FROM chat_reactions
		 WHERE message_id IN (`+placeholders(len(ids))+`)
		 GROUP BY message_id, emoji
		 ORDER BY message_id, emoji`, args...)
	if err != nil {
		return fmt.Errorf("chatlet: read a page's reactions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	at := make(map[string]int, len(views))
	for i, view := range views {
		at[view.Message.ID] = i
	}
	for rows.Next() {
		var id string
		var reaction ReactionCount
		var mine int
		if err := rows.Scan(&id, &reaction.Emoji, &reaction.Count,
			&mine); err != nil {

			return fmt.Errorf("chatlet: scan a reaction: %w", err)
		}
		reaction.Mine = mine != 0
		if i, ok := at[id]; ok {
			views[i].Reactions = append(views[i].Reactions, reaction)
		}
	}
	return rows.Err()
}

// ---- the mention feed -------------------------------------------------- //

// MentionQuery asks for a page of one person's @-mentions.
type MentionQuery struct {
	// Cursor is the POSITION of the last row of the previous page, as
	// [statelog.Position.String] renders it. The feed is newest first, so
	// the next page is what sits below it.
	Cursor string

	Limit int
}

// Mention is one message that named this person.
//
// IT CARRIES THE ROOM AND AN EXCERPT rather than a message id alone, because
// the feed is READ rather than navigated: somebody scanning what named them
// wants to know where and roughly what, and a feed that answered with ids
// would be one fetch per row to render.
//
// # It is the moment they were named, and it stays there
//
// The feed is the whole of a person's away story — the engine sends no email,
// push or SMS — so what it owes its reader is "where have I been named, newest
// first", each entry once. That decides both of the things that can happen to
// a message after it named somebody:
//
//   - AN EDIT that still names them leaves the mention exactly where it was,
//     with the new words. Fixing a typo is not naming anybody again, and a feed
//     that lifted the mention to the top re-presented, as news, something the
//     reader had already seen — every time its author touched it — while a
//     reader paging back never saw it at all, because it jumped above the
//     cursor they held. An edit that names somebody the words before it did
//     not IS a naming, and it is placed and dated at the edit: put back at the
//     message's own place, it would land on a page the reader already read and
//     be found by nobody — and [Store.Edit] wakes that person, and only them,
//     for the same reason.
//   - A DELETE takes the mention away, as an edit that no longer names them
//     does, and as an erase and a prune do. The room keeps a tombstone because
//     a thread hangs off it; the feed is a list of things to answer, and a
//     message taken back asks nothing, has no words left to read, and should
//     not go on telling somebody they were named in words that were withdrawn.
type Mention struct {
	MessageID string `json:"message_id"`

	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name,omitempty"`
	ChannelKind Kind   `json:"channel_kind"`

	// ThreadRoot is the thread this was said in, empty on a room post.
	ThreadRoot string `json:"thread_root,omitempty"`

	Author     string     `json:"author,omitempty"`
	AuthorKind AuthorKind `json:"author_kind,omitempty"`

	// Excerpt is at most [MaxExcerpt] bytes of what the message says NOW —
	// after any edit, which [Mention.EditedAt] says happened. There is no
	// tombstone in this feed: a delete takes a message's mention rows with
	// its body, so a message somebody took back is no longer something that
	// named you.
	Excerpt string `json:"excerpt,omitempty"`

	// At is WHEN THIS PERSON WAS NAMED: the broker's instant for the record
	// at [Mention.Position] — the message's own for a handle its post named,
	// the edit's for one an edit named. It is the instant of the place the
	// feed orders by, so a row's time never contradicts where it sits.
	At time.Time `json:"at"`

	// EditedAt is the message's last edit, absent where it was never edited.
	// The excerpt is the words as they now stand, which may not be the words
	// that named you, and a mention an edit made sits above the message's own
	// place in its room: the mark is what tells a reader both.
	EditedAt *time.Time `json:"edited_at,omitempty"`

	// Position is where the record that named this handle sits — the post,
	// or the edit that named it where the words before it did not — and it
	// is what a cursor is built from. It never moves: an edit keeps the row
	// of every handle it still names, and a row is written `ON CONFLICT DO
	// NOTHING`.
	Position statelog.Position `json:"position"`
}

// MentionFeed is a page of one person's mentions.
type MentionFeed struct {
	Served

	Handle   string    `json:"handle"`
	Mentions []Mention `json:"mentions"`

	NextCursor string `json:"next_cursor,omitempty"`
}

// Mentions answers a page of the viewer's own @-mention feed.
//
// THERE IS NO MAILBOX TABLE BEHIND THIS. A mention is a fact about the
// message, `chat_mentions_handle_idx` is ordered `(handle, version DESC)`, and
// the feed is a range over it — which is why there is no per-viewer
// notification row anywhere in this domain and nothing to keep in step with
// the messages.
//
// A SET READ over the domain, because a person is named in rooms a scope
// cannot enumerate. It is also the one read here whose VISIBILITY is not
// implied: being named in a private room you are not in is an ordinary thing
// to happen, it wakes nobody, and the feed must not show it.
func (r *Reader) Mentions(ctx context.Context, viewer string, q MentionQuery,
	fresh statelog.Freshness) (MentionFeed, error) {

	who, err := viewerOf(viewer)
	if err != nil {
		return MentionFeed{}, err
	}
	if fresh.Level == "" {
		return MentionFeed{}, errNoLevel("a mention feed read")
	}
	before, err := parsePositionCursor(q.Cursor)
	if err != nil {
		return MentionFeed{}, err
	}
	out := MentionFeed{Handle: who}
	answer, err := r.log.Read(ctx, fresh.Query(ReadScope(""), true),
		func(tx *sql.Tx) error {
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			var err error
			out.Mentions, out.NextCursor, err = readMentions(ctx, tx, who,
				before, limitOf(q.Limit))
			return err
		})
	if err != nil {
		return MentionFeed{}, err
	}
	out.Served = servedFrom(answer)
	return out, nil
}

// readMentions is one page of the feed.
//
// THE KEYSET IS THE COMPOSED POSITION, which is what lets a cursor span a
// reanchor with no gap and no repeat: `version` already carries
// `(generation << 40) | seq`, so the generation is IN the ordering rather than
// beside it, and a page taken before an operator rebuilt the estate resumes
// against one taken after it.
//
// THE JOIN TO THE MESSAGE IS INNER, unlike the tracker's inbox: a mention row
// and the message it is about are written in one transaction and removed in
// one — an erase and a prune both take them together — so a mention with no
// message is not a lifetime difference this domain has, it is a row nothing
// could render.
func readMentions(ctx context.Context, tx *sql.Tx, who string,
	before statelog.Position, limit int) ([]Mention, string, error) {

	where := []string{"n.handle = ?"}
	args := []any{who}
	if !before.IsZero() {
		where = append(where, "n.version < ?")
		args = append(args, before.Packed())
	}
	// THE VISIBILITY NARROWING GOES LAST in the predicate and therefore
	// last in the binds, because the placeholders are positional: the
	// order of these two lines and the order of the two appends are one
	// fact stated twice, and a page filtered by a cursor value would be a
	// feed that renders nothing.
	where = append(where, visibleWhere)
	args = append(args, who)
	args = append(args, limit+1)

	rows, err := tx.QueryContext(ctx, `
		SELECT n.message_id, n.version, n.named_at, n.channel_id, c.name,
		       c.kind, m.thread_root, m.author_handle, m.author_kind, m.body,
		       m.edited_at, m.log_stream
		  FROM chat_mentions n
		  JOIN chat_messages m ON m.id = n.message_id
		  JOIN chat_channels c ON c.id = n.channel_id
		 WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY n.version DESC
		 LIMIT ?`, args...)
	if err != nil {
		return nil, "", fmt.Errorf("chatlet: read %s's mentions: %w", who, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Mention, 0, limit)
	more := false
	for rows.Next() {
		if len(out) == limit {
			more = true
			break
		}
		var (
			mention    Mention
			packed     int64
			namedAt    int64
			kind       string
			authorKind string
			body       string
			editedAt   sql.NullInt64
			stream     string
		)
		if err := rows.Scan(&mention.MessageID, &packed, &namedAt,
			&mention.ChannelID, &mention.ChannelName, &kind,
			&mention.ThreadRoot, &mention.Author, &authorKind, &body,
			&editedAt, &stream); err != nil {

			return nil, "", fmt.Errorf("chatlet: scan one of %s's mentions: %w",
				who, err)
		}
		mention.ChannelKind = Kind(kind)
		mention.AuthorKind = AuthorKind(authorKind)
		mention.Excerpt = Excerpt(body)
		// THE ROW'S OWN INSTANT, not the message's: see [Mention.At].
		mention.At = store.DecodeTime(namedAt)
		if editedAt.Valid {
			edited := store.DecodeTime(editedAt.Int64)
			mention.EditedAt = &edited
		}
		// THE WHOLE POSITION FROM THE ROW'S OWN VERSION. The message's
		// position columns are the LAST record that changed the message,
		// and a mention keeps the position of the record that named its
		// handle — after an edit in a later generation, the message's
		// generation beside this row's sequence names a place on the log
		// that neither record holds. The stream is the domain's one log.
		mention.Position = statelog.Position{
			Stream:     stream,
			Generation: uint32(packed / statelog.GenerationStride),
			Seq:        uint64(packed % statelog.GenerationStride),
		}
		// THE SQL NARROWED AND THIS DECIDES. The predicate above is what
		// makes the LIMIT count rows this person may see; the rule about
		// who may read a room is [Visible]'s, here as everywhere else —
		// and a kind this build cannot classify is refused here rather
		// than admitted by a column the applier wrote from a predicate
		// this build no longer knows the whole of.
		if !Visible(who, Channel{Kind: mention.ChannelKind}, true) {
			continue
		}
		out = append(out, mention)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("chatlet: read %s's mentions: %w", who, err)
	}
	var next string
	if more && len(out) > 0 {
		next = out[len(out)-1].Position.String()
	}
	return out, next, nil
}

// ---- the small shared rules -------------------------------------------- //

// limitOf bounds a page.
//
// ZERO IS "the caller said nothing", which takes [DefaultLimit] rather than
// returning nothing: an absent limit is the common case at every surface, and
// a read that answered with no rows for it would be indistinguishable from a
// room nobody has written in.
func limitOf(limit int) int {
	if limit <= 0 {
		return DefaultLimit
	}
	return min(limit, MaxLimit)
}

// placeholders renders an IN list of n binds.
func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// parseSeqCursor reads a transcript cursor: one room's own message number.
//
// A BARE INTEGER, because that is what it is — a per-room ordinal minted by
// the applier from log order, which is neither a position nor comparable with
// one. Spelling it as a position would invite a caller to paste it into
// `min_position`, where it would name a sequence on a log rather than a place
// in a room.
func parseSeqCursor(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	seq, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seq < 0 {
		return 0, invalid("cursor", "%q is not a message number — a page of a "+
			"room is cursored by `next_cursor` from the answer before it, "+
			"which is the last message's own number in that room", raw)
	}
	return seq, nil
}

// parsePositionCursor reads a feed cursor: a position on this log.
//
// `<stream>@<generation>:<sequence>`, which is [statelog.Position.String] and
// therefore exactly what the answer handed back. The STREAM is part of it
// because a sequence alone is a number in a space it may not belong to: an
// operator who rebuilt the broker estate starts a new stream at 1, and a
// cursor that could not say which stream it came from would page a feed from
// the middle of a log that no longer exists.
func parsePositionCursor(raw string) (statelog.Position, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return statelog.Position{}, nil
	}
	refuse := func() (statelog.Position, error) {
		return statelog.Position{}, invalid("cursor", "%q is not a log "+
			"position — one reads `<stream>@<generation>:<sequence>`, and the "+
			"answer that produced it carries the value to send back", raw)
	}
	stream, rest, found := strings.Cut(raw, "@")
	if !found || stream == "" {
		return refuse()
	}
	generation, sequence, found := strings.Cut(rest, ":")
	if !found {
		return refuse()
	}
	gen, err := strconv.ParseUint(generation, 10, 32)
	if err != nil {
		return refuse()
	}
	seq, err := strconv.ParseUint(sequence, 10, 64)
	if err != nil {
		return refuse()
	}
	at := statelog.Position{Stream: stream, Generation: uint32(gen), Seq: seq}
	if err := at.Valid(); err != nil {
		return statelog.Position{}, invalid("cursor", "%q is not a usable log "+
			"position: %s", raw, err.Error())
	}
	return at, nil
}
