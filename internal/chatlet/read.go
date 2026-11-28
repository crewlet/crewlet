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

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Reader answers questions about the company's conversation from THIS NODE's
// own applied rows. Every read goes through [statelog.Reader] (refusal ladder,
// coverage probe, freshness target, rows) in ONE transaction per answer, so a
// page never pairs reactions from after its messages. [Served] travels with each
// answer because [Reader.At] answers about NOW. A caller's write position goes in
// [statelog.Freshness.Floors]; there is no chat level or `wait_for`. Read state
// lives in coord and arrives as a value on the query: no second store here.
// [Visible] decides every room read; the SQL `private` narrowing only stops a
// LIMIT counting rows the caller cannot see. A room the viewer may not read is
// NOT FOUND, never forbidden: its existence discloses who talks to whom.
// A transcript pages on `channel_seq`, a feed on the composed position:
// `chat_messages.version` moves on edit, so paging on it skips an edited message.
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
// screen rather than the export. A page also ends at the bytes
// [statelog.RowsOf] holds, its cursor carrying on: a message at its caps runs
// to most of [MaxRecordBytes], so a page at [MaxLimit] is over a hundred MiB.
const (
	DefaultLimit = 50
	MaxLimit     = 500
)

// UnreadLimit is how far an unread count counts before it answers "at least
// this many".
//
// A HUNDRED: counting to 4,000 changes nothing anybody does but scans a room's
// whole transcript on every poll of a rail with one row per room. The count is
// a range over the room's newest hundred messages. The window loses only what
// the cap already hides: the messages above a cursor are the newest, so more
// than a hundred unread is the cap however it was counted.
const UnreadLimit = 100

// MaxRailChannels bounds how many rooms one person's channel list answers for.
//
// 256 is [github.com/crewlet/crewlet/internal/coord.MaxReadCursors], the cap on
// one person's read cursors: past it a room could not carry an unread count,
// its cursor already dropped.
//
// THE RAIL IS NOT PAGED. It is ordered by ACTIVITY (each room's newest
// message), not by any indexed column, so a SQL window would page by channel id
// and re-sort each page, an order that changes as you scroll. One bounded list
// is sorted once; [ChannelListing.Truncated] says when there are more rooms. A
// smaller [ChannelsQuery.Limit] gets the top of that list, never a page of it.
const MaxRailChannels = 256

// Visible is the ONE answer to whether somebody may read a room.
//
// A public or unit room is readable by any seat of the company (the engine is
// the boundary: callers are already resolved to a seat). A private room, a
// direct conversation and a group are their membership, the predicate the
// applier wrote the `private` column from ([privateRoom]).
//
// An unknown kind is NOT visible: a newer peer's room must not be served on the
// guess that it resembles a public one. An empty viewer is nobody ([viewerOf]).
// There is deliberately no admin arm: compliance (a moderator's delete, an
// erase, a prune) reaches past membership WITHOUT reading. The member flag is
// the caller's so a listing does not issue a second statement per room.
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

// ---- the directory: every room a viewer may read ----------------------- //

// DirectoryRoom is one room as a browse of every room shows it: the room, how
// many are in it, whether the viewer is, and when somebody last spoke there.
type DirectoryRoom struct {
	Channel Channel `json:"channel"`

	// Members is how many seats are in the room.
	Members int `json:"members"`

	// Member says the viewer is one of them. A public room and a unit's
	// room are readable without it; joining is what it offers.
	Member bool `json:"member,omitempty"`

	// LastAt is the broker's instant of the newest message somebody said
	// there, nil for a room nobody has spoken in. A room event is not
	// activity: a room somebody only joined is a quiet one.
	LastAt *time.Time `json:"last_at,omitempty"`
}

// Directory is every named room a viewer may read.
type Directory struct {
	Served

	// Rooms are the live rooms in name order, then the archived ones, the
	// most recently archived first: [RoomNames]' order and bound. Never
	// nil.
	Rooms []DirectoryRoom `json:"rooms"`

	// Truncated says rooms were left out past [MaxRoomNames] or the bytes
	// [statelog.RowsOf] holds, the archived ones first.
	Truncated bool `json:"truncated,omitempty"`

	// Unreadable is how many rooms this build could not present, a
	// document a newer peer wrote or a kind it cannot classify.
	Unreadable int `json:"unreadable,omitempty"`
}

// Directory answers every NAMED room the viewer may read, joined or not, the
// archived ones included, each with its member count, whether the viewer is
// in it and its last activity: what a person browses to find a room to join.
//
// [Reader.RoomNames]' SET AND ORDER, read by the same visibility predicate,
// so a private room is listed to its members alone and a direct conversation
// is never listed: it has no name to browse by. Then per room, as the rail
// does, one seek for its member count and one for its newest message, which
// [MaxRoomNames] bounds; and the viewer's own memberships in one statement.
//
// A SET READ, incomplete rather than refused under a deferred record, for
// [Reader.Channels]' reason.
func (r *Reader) Directory(ctx context.Context, viewer string,
	fresh statelog.Freshness) (Directory, error) {

	who, err := viewerOf(viewer)
	if err != nil {
		return Directory{}, err
	}
	if fresh.Level == "" {
		return Directory{}, errNoLevel("the rooms a viewer may browse")
	}
	var out Directory
	answer, err := r.log.Read(ctx, fresh.Query(ReadScope(""), true),
		func(tx *sql.Tx) error {
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			var err error
			out, err = directory(ctx, tx, who)
			return err
		})
	if err != nil {
		return Directory{}, err
	}
	out.Served = servedFrom(answer)
	return out, nil
}

// directory is [Reader.Directory] inside its read.
func directory(ctx context.Context, tx *sql.Tx, who string) (Directory, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT c.document FROM chat_channels c
		  WHERE c.name <> '' AND `+visibleWhere+`
		  ORDER BY c.archived_at IS NOT NULL, c.archived_at DESC, c.name_norm`, who)
	if err != nil {
		return Directory{}, fmt.Errorf("chatlet: read the rooms %q may browse: %w", who, err)
	}
	out := Directory{Rooms: make([]DirectoryRoom, 0, 16)}
	held := statelog.RowsOf(ctx)
	for rows.Next() {
		var document []byte
		if err = rows.Scan(&document); err != nil {
			_ = rows.Close()
			return Directory{}, fmt.Errorf("chatlet: scan a room to browse: %w", err)
		}
		room, decodeErr := DecodeChannel(document)
		// MEMBER IS TRUE for [readableChannels]' reason: the predicate
		// established it for every private row it admitted.
		if decodeErr != nil || !Visible(who, room, true) {
			out.Unreadable++
			continue
		}
		if len(out.Rooms) == MaxRoomNames || !held.Take(len(document)) {
			out.Truncated = true
			break
		}
		out.Rooms = append(out.Rooms, DirectoryRoom{Channel: room})
	}
	if err = closeRows(rows, "the rooms "+who+" may browse"); err != nil {
		return Directory{}, err
	}
	mine, err := handlesTx(ctx, tx,
		`SELECT channel_id FROM chat_members WHERE handle = ?`, who)
	if err != nil {
		return Directory{}, err
	}
	member := make(map[string]bool, len(mine))
	for _, id := range mine {
		member[id] = true
	}
	for i := range out.Rooms {
		entry := &out.Rooms[i]
		entry.Member = member[entry.Channel.ID]
		if err = tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM chat_members WHERE channel_id = ?`,
			entry.Channel.ID).Scan(&entry.Members); err != nil {
			return Directory{}, fmt.Errorf("chatlet: count room %s's members: %w",
				entry.Channel.ID, err)
		}
		last, lastErr := lastMessage(ctx, tx, entry.Channel.ID)
		if lastErr != nil {
			return Directory{}, lastErr
		}
		if last != nil {
			at := last.At
			entry.LastAt = &at
		}
	}
	return out, nil
}

// ---- the rail: one person's rooms -------------------------------------- //

// ChannelsQuery asks for one person's channel list.
type ChannelsQuery struct {
	// Marks is the viewer's own read state, as [ActivityQuery.Marks]: the
	// room cursors the unread counts are measured from, and what
	// [ChannelsQuery.Waiting] is counted against.
	//
	// IT IS A VALUE RATHER THAN A LOOKUP. Read state is the one piece of
	// chat state that is not on this log — it is a fact about one person's
	// attention that nobody replays — so it reaches this read from the
	// caller that already had to fetch it for the mute list and the
	// do-not-disturb window. An absent cursor is a room this person has
	// never opened, whose whole tail is unread.
	Marks ReaderMarks

	// Waiting asks for [ChannelListing.Waiting], counted only over a record
	// that was read ([ReaderMarks.Known]). Ignored with [ChannelsQuery.With],
	// whose rooms are two seats' shared ones, not the reader's rail.
	Waiting bool

	// IncludeArchived keeps rooms that were closed to new messages. Off by
	// default: an archived room is readable for ever and belongs in a
	// search result or an explicit visit rather than in the list somebody
	// works out of.
	IncludeArchived bool

	// Unread keeps only the rooms with messages above the viewer's cursor.
	// It reads the cursors in [ChannelsQuery.Marks] as a MEASUREMENT: with
	// none, every room's whole tail counts as unread, so a caller that could
	// not read the person's read state must not ask for it.
	Unread bool

	// With keeps only the rooms this seat is ALSO a member of: the rooms
	// the viewer has in common with somebody, which a profile lists. Empty
	// is every room of the viewer's; the viewer's own handle is the same.
	With string

	// Starred is the rooms the viewer starred, from the same read state as
	// [ChannelsQuery.Marks]. A star is a person keeping a room in the
	// list they work out of, so a starred room they may READ and are not in
	// is listed beside the rooms they are in ([ChannelSummary.Member] says
	// which): a team's room is readable by every seat and joined only by
	// being in the team, and before this a star on it kept it nowhere.
	// A starred room the viewer may not read (a private room they left) is
	// not listed and not counted, exactly as a room that does not exist.
	// Ignored with [ChannelsQuery.With], whose rooms are memberships two
	// seats share.
	Starred []string

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

	// Member says the viewer is in the room. False only for a room listed
	// because they starred it ([ChannelsQuery.Starred]), which a screen
	// must not take for a membership: no message reaches them from it
	// unless it names them.
	Member bool `json:"member"`

	// Unread is how many room posts sit above this viewer's cursor, counted
	// to [UnreadLimit]. A reply in a thread is not one: it is read in its
	// thread, which the reader follows there. Their OWN messages and the
	// tombstones do not count: a badge for something you said yourself, or
	// for a body that is no longer there, is a badge nobody can clear by
	// reading.
	Unread int `json:"unread"`

	// UnreadCapped says the count stopped at [UnreadLimit] — the "99+"
	// case, stated rather than inferred from the number, so a caller
	// rendering "100" cannot mean two different things by it.
	UnreadCapped bool `json:"unread_capped,omitempty"`

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
	// rooms than [MaxRailChannels], more matched than the query's
	// [ChannelsQuery.Limit], or more than the bytes [statelog.RowsOf]
	// holds. STATED rather than left to a caller counting,
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

	// Waiting is what waits on the reader, place by place, when the query
	// asked ([ChannelsQuery.Waiting]): over every room they may read, listed
	// here or not, before the listing's `Unread` and `Limit` cuts.
	Waiting []Waiting `json:"waiting"`

	// WaitingCapped says a count in Waiting stopped at [UnreadLimit], so
	// their sum is a floor.
	WaitingCapped bool `json:"waiting_capped"`
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
			if out, err = r.rail(ctx, tx, who, q); err != nil {
				return err
			}
			// OVER EVERY ROOM, listed or not: a reader in no room may still be
			// named in one.
			if q.Waiting && q.Marks.Known && strings.TrimSpace(q.With) == "" {
				out.Waiting, out.WaitingCapped, err = railWaiting(ctx, tx, who, q.Marks,
					out.Channels)
			}
			return err
		})
	if err != nil {
		return ChannelListing{}, err
	}
	out.Served = servedFrom(answer)
	return out, nil
}

// Readable is every room this viewer may READ, each as a search needs it
// ([ReadableRoom]). The rail is not this set: [Visible] also admits public and
// unit rooms the viewer has not joined, which a search must not silently miss.
// Archived rooms are in: an archive closes a room to writing, not reading.
//
// The WHOLE set materialises, with no limit: a partial one would narrow a search
// without saying so, the failure
// [github.com/crewlet/crewlet/internal/search.ErrNoViewer] exists to refuse. It
// is NOT bounded by [MaxChannels], which counts live rooms only. An empty slice
// with no error is a viewer who may read nothing, not an unresolved one.
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

	// Truncated says rooms were left out past [MaxRoomNames] or the bytes
	// [statelog.RowsOf] holds: a caller that reads a name as a room only
	// where this answer holds it reads the rest by their shape, and has to
	// know that it is doing so.
	Truncated bool `json:"truncated,omitempty"`
}

// RoomNames answers every NAMED room the viewer may read: [Reader.Readable]'s
// set, less the direct and group conversations, which have no name, and
// bounded at [MaxRoomNames].
//
// A room's name is any `[a-z0-9][a-z0-9_-]{0,63}` ([namePattern]), so `#e2e`
// and `#2026` read as a colour or an issue number; a reader can only tell them
// apart by knowing which rooms exist. The rail is not that set, for
// [Reader.Readable]'s reason.
//
// Names, never contents: each room is its id, name, kind and archived flag,
// nothing a [Visible] room does not already show, and none of the rail's
// per-room scans (last message, unread count). One statement, read per frame.
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
	held := statelog.RowsOf(ctx)
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
		if len(out.Rooms) == MaxRoomNames || !held.TakeEncoded(room) {
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
	member := make(map[string]bool, len(order))
	for _, id := range order {
		member[id] = true
	}
	if strings.TrimSpace(q.With) == "" {
		order = append(order, starredBeside(q.Starred, member)...)
	}
	if len(order) > MaxRailChannels {
		// THE MEMBERSHIPS FIRST: they lead the list, so a cut takes the
		// rooms only starred before any room the person is in.
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
		id := entry.room.ID
		if !entry.room.Kind.Valid() {
			// A ROOM THIS BUILD CANNOT CLASSIFY IS NOT SHOWN, and it
			// is counted with the documents it cannot decode: both
			// are a newer peer's row that this build must not
			// present on a guess about what it means.
			out.Unreadable++
			continue
		}
		if !Visible(who, entry.room, member[id]) {
			// A STARRED ROOM THIS VIEWER MAY NOT READ is not there, and
			// is not counted: a private room they were taken out of
			// answers as a room that does not exist, here as in every
			// read. A membership is always visible, so only a star
			// reaches this.
			continue
		}
		summary := ChannelSummary{
			Channel: entry.room, Revision: entry.revision,
			FollowAll: follow[id], Member: member[id],
		}
		summary.Last, err = lastMessage(ctx, tx, id)
		if err != nil {
			return ChannelListing{}, err
		}
		summary.Unread, summary.UnreadCapped, err = unreadCount(
			ctx, tx, id, who, q.Marks.Cursors[id], summary.Last)
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
	// AND THE TOP THE BYTES HOLD ([statelog.RowsOf]), cut from the sorted
	// list for the same reason: a room's newer-peer fields and its preview
	// ride whole on its summary.
	held := statelog.RowsOf(ctx)
	for i, summary := range out.Channels {
		if !held.TakeEncoded(summary) {
			out.Channels = out.Channels[:i]
			out.Truncated = true
			break
		}
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

// starredBeside is the starred rooms a rail lists beside the viewer's own: each
// once, in id order, leaving out the rooms they are already in. At most
// [MaxRailChannels], which is the most one rail lists at all; the read state
// that holds the stars bounds them far below the size of a statement.
func starredBeside(starred []string, member map[string]bool) []string {
	var out []string
	seen := map[string]bool{}
	for _, id := range starred {
		id = strings.TrimSpace(id)
		if id == "" || member[id] || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	slices.Sort(out)
	if len(out) > MaxRailChannels {
		out = out[:MaxRailChannels]
	}
	return out
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

// railWaiting is [ChannelListing.Waiting] ([waiting]), over the rooms' newest
// messages the rail has already read.
func railWaiting(ctx context.Context, tx *sql.Tx, who string, marks ReaderMarks,
	rooms []ChannelSummary) ([]Waiting, bool, error) {

	known := make(map[string]*MessagePreview, len(rooms))
	for _, room := range rooms {
		known[room.Channel.ID] = room.Last
	}
	t, err := waiting(ctx, tx, who, marks, known)
	if err != nil {
		return nil, false, err
	}
	return t.list(), t.capped, nil
}

// unreadCount is the counted range above one person's cursor in one room.
//
// The cursor is a POSITION, the transcript ordered by SEQUENCE; they meet only
// through `version`. A newest message at or below the cursor means zero WITH NO
// SECOND STATEMENT (every already-read room, every poll). The window is the
// newest [UnreadLimit] because `version > cursor` in sequence order would walk
// the whole room. Your own messages, tombstones, events and thread replies are
// excluded: a badge reading cannot clear trains people to ignore the rail.
func unreadCount(ctx context.Context, tx *sql.Tx, channelID, who string,
	cursor int64, last *MessagePreview) (unread int, capped bool, err error) {

	if last == nil || last.Position.Packed() <= cursor {
		return 0, false, nil
	}
	err = tx.QueryRowContext(ctx, unreadStatement,
		channelID, UnreadLimit, cursor, who).Scan(&unread)
	if err != nil {
		return 0, false, fmt.Errorf("chatlet: count what %s has not read in "+
			"room %s: %w", who, channelID, err)
	}
	return unread, unread >= UnreadLimit, nil
}

// unreadStatement is [unreadCount]'s one statement: the room's newest
// messages, those above the cursor that somebody else wrote and nobody took
// back.
const unreadStatement = `
	SELECT COUNT(*)
	  FROM (SELECT version, author_handle, author_kind, deleted_at
	          FROM chat_messages
	         WHERE channel_id = ? AND event = '' AND thread_root = ''
	         ORDER BY channel_seq DESC
	         LIMIT ?) t
	 WHERE t.version > ? AND (t.author_handle <> ? OR t.author_kind = '` + string(AuthorSystem) + `')
	   AND t.deleted_at IS NULL`

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
// IN GO, not SQL, which [MaxRailChannels] pays for: the newest message's instant
// lives in another table than the membership, so an ORDER BY would need every
// room's last message before the first row, the scan a bounded list plus one
// seek per room replaces. An empty room sorts by its create, not at the bottom
// for ever. The id tie-break makes two nodes answer one rail in one order.
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
// A name IS the room's address (unique, never renamed), so it resolves against
// every room the viewer may read, not only the ones they are IN: a decision may
// promise its outcome to any unit's room, and no seat call hands over that id.
//
// The read is about the ADDRESS and the room it resolves to: a deferred record
// claiming this name (a newer peer's create) refuses rather than answering "no
// such room". The name resolves in the answer's own transaction.
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
// THE DOCUMENT AS A READER IS HANDED IT ([Message.forReaders], without whom
// it was written for) PLUS THE TWO VALUES THE APPLIER MINTED, which the document
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

	// Answered is which of the seats a person's post reached
	// ([Message.Reach]) have answered it, in the order they answered:
	// spoken in its thread, or anywhere after it in a direct
	// conversation. Empty while nobody has, and on every message with no
	// reach.
	//
	// READ BESIDE THE PAGE from `chat_receipts`, which the applier marks as
	// the answers land, because whether a seat answered is a fact about
	// LATER messages and the document is the post's own.
	Answered []Answer `json:"answered,omitempty"`

	// Taken is which of the seats it reached took it into their running turn
	// instead of a turn of their own, and how, while they have not answered
	// it ([OpTake]): read beside the page from `chat_receipts`, as Answered.
	Taken []Take `json:"taken,omitempty"`

	// Delivery is where a PERSON'S post stands with the agents it reached,
	// read from its reach, its answers and its takes: [DeliveryUnknown] for one whose
	// reach was never recorded, and empty on everything that is not a
	// person's post (an agent's message, a room event). One word a reader
	// can filter or badge on, beside the two lists it is read from.
	Delivery Delivery `json:"delivery,omitempty"`
}

// Delivery is where a person's post stands with the agents it reached.
type Delivery string

const (
	// DeliveryUnknown is a person's post whose reach nobody recorded: one
	// stored before posts recorded it. NOT [DeliveryNoAgent], which is a
	// recorded fact that nobody was asked; a reader shows no receipt for
	// this one rather than claim the post reached nobody.
	DeliveryUnknown Delivery = "unknown"

	// DeliveryNoAgent is a post that reached no agent: nobody was asked.
	DeliveryNoAgent Delivery = "no_agent"

	// DeliveryWaiting is a post that reached agents, none of whom has
	// answered yet.
	DeliveryWaiting Delivery = "waiting"

	// DeliveryPartlyAnswered is a post some of the agents it reached have
	// answered, and some have not.
	DeliveryPartlyAnswered Delivery = "partly_answered"

	// DeliveryAnswered is a post every agent it reached has answered, or
	// read in its running turn finding nothing asked ([TakenAbsorbed]).
	DeliveryAnswered Delivery = "answered"
)

// deliveryOf is the state of a message with these answers and takes: a
// person's post by its reach, and nothing for any other message. A seat whose
// running turn absorbed it owes it nothing, so counts as answered; one that
// joined it still owes the answer.
func deliveryOf(m Message, answered []Answer, taken []Take) Delivery {
	r := m.Reach
	settled := len(answered)
	for _, t := range taken {
		if t.As == TakenAbsorbed {
			settled++
		}
	}
	switch {
	case m.AuthorKind != AuthorHuman || m.Event != nil:
		return ""
	case r == nil:
		return DeliveryUnknown
	case len(r.Recipients) == 0:
		return DeliveryNoAgent
	case settled == 0:
		return DeliveryWaiting
	case settled < len(r.Recipients):
		return DeliveryPartlyAnswered
	}
	return DeliveryAnswered
}

// Answer is a seat's answer to a message it was reached by.
type Answer struct {
	Handle string `json:"handle"`

	// MessageID is the message that answered, and At its broker instant.
	MessageID string    `json:"message_id"`
	At        time.Time `json:"at"`
}

// Take is a seat's running turn having taken a message it was reached by
// ([OpTake]): how, and the broker's instant of the record that said so.
type Take struct {
	Handle string    `json:"handle"`
	As     Taken     `json:"as"`
	At     time.Time `json:"at"`
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
	// The engine's line is a reply and no replier ([Message.speaker]): no
	// seat said it.
	Repliers     []string `json:"repliers"`
	ReplierCount int      `json:"replier_count"`
}

// TranscriptQuery asks for a page of one room: its room posts and room events,
// each root carrying its thread's summary. A reply is read in its thread
// ([Reader.Thread]), never here.
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
			// ROOM POSTS ONLY: a reply belongs to its thread, which its
			// root's summary counts, and drawn here it read as said in the
			// room. The thread index seeks `thread_root = ''` directly.
			where := []string{"channel_id = ?", "thread_root = ''"}
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
	// A DELETED REPLY IS STEPPED OVER: a tombstone is a blank line a reader
	// drops and would displace a real reply from the window. It still counts
	// in Earlier. This is what a seat woken in a thread reads ([ThreadReader]):
	// its waking message is the newest, and paging from the root is a walk the
	// thread's length where [chat_messages_thread_idx] seeks from either end.
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

// TurnThread is one thread as the engine's turn-start read takes it: the
// thread, as any reader is handed it, and whom each of its messages was
// written for, which no reader is ever handed ([Message.forReaders]).
//
// THE ENGINE'S ALONE. A turn works through only the waiting messages written
// for whom it works for (ADR-0109), and nothing else needs whom an agent's post
// was written for: a room's reader needs no requester's seat, login or apps.
// So it is answered by its own read ([Reader.TurnThread]), which only the
// engine's own facade asks, and every view keeps stripping it.
type TurnThread struct {
	Thread Thread `json:"thread"`

	// WorkingFor maps a message's id to whom its post was written for, for
	// every message of the thread whose document holds a carrier
	// ([Message.WorkingFor]). A message missing from it was written for
	// nobody else: a person's, a room event, an agent's own work, or a post
	// below the carrier's version.
	WorkingFor map[string]*types.WorkingFor `json:"working_for,omitempty"`

	// Carried says the serving reader read WorkingFor. Only it sets it, so
	// an answer that lacks it, a plain thread a fallback read instead or a
	// build that never set it, says nothing was read, and a turn then marks
	// no waiting message as worked through: the safe direction.
	Carried bool `json:"carried"`
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

	out, _, err := r.readThread(ctx, viewer, q, fresh)
	return out, err
}

// TurnThread answers one thread as [Reader.Thread] does, in the same one read,
// with whom each of its messages was written for ([TurnThread]).
//
// FOR THE ENGINE'S TURN-START READ ALONE, through the engine's own facade
// (estate.TurnThreads): a seat's turn works through only the waiting messages
// written for whom it works for, so it must know whom each was written for,
// and no reader of a room may. ONE READ, never [Reader.Thread] and then a
// second for the carriers: a turn's thread is one linearizable read
// ([LinearizableReadsPerSeatDay]), and two would be two answers that need not
// agree.
func (r *Reader) TurnThread(ctx context.Context, viewer string, q ThreadQuery,
	fresh statelog.Freshness) (TurnThread, error) {

	out, carriers, err := r.readThread(ctx, viewer, q, fresh)
	if err != nil {
		return TurnThread{}, err
	}
	return TurnThread{Thread: out, WorkingFor: carriers, Carried: true}, nil
}

// readThread is [Reader.Thread]'s one read, with the carriers of the messages it
// answered, which [Reader.TurnThread] alone hands on.
func (r *Reader) readThread(ctx context.Context, viewer string, q ThreadQuery,
	fresh statelog.Freshness) (Thread, map[string]*types.WorkingFor, error) {

	who, err := viewerOf(viewer)
	if err != nil {
		return Thread{}, nil, err
	}
	if fresh.Level == "" {
		return Thread{}, nil, errNoLevel("a thread read")
	}
	if strings.TrimSpace(q.RootID) == "" {
		return Thread{}, nil, invalid("root_id", "a thread read names no root — a "+
			"thread is addressed by the message it hangs off, and the room's "+
			"own transcript is `Messages`")
	}
	if q.Newest < 0 || (q.Newest > 0 && q.Cursor != "") {
		return Thread{}, nil, invalid("newest", "a thread read asks for the newest "+
			"%d replies beside a cursor — the cursor pages forward from the "+
			"root and the newest end is read in one, so a read names one or "+
			"the other", q.Newest)
	}
	after, err := parseSeqCursor(q.Cursor)
	if err != nil {
		return Thread{}, nil, err
	}
	out := Thread{ChannelID: q.ChannelID}
	var carriers map[string]*types.WorkingFor
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
			carriers = withCarriers(carriers, root.workingFor)

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
				carriers = withCarriers(carriers, tail.workingFor)
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
			carriers = withCarriers(carriers, page.workingFor)
			out.Unreadable = root.undecodable + page.undecodable
			out.Participants, err = threadParticipants(ctx, tx, q.ChannelID,
				q.RootID)
			return err
		})
	if err != nil {
		return Thread{}, nil, err
	}
	out.Served = servedFrom(answer)
	return out, carriers, nil
}

// withCarriers adds a page's carriers to those a read has gathered.
func withCarriers(into, page map[string]*types.WorkingFor) map[string]*types.WorkingFor {
	if len(page) == 0 {
		return into
	}
	if into == nil {
		into = make(map[string]*types.WorkingFor, len(page))
	}
	for id, carrier := range page {
		into[id] = carrier
	}
	return into
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

	// lastAt and lastID are the last row scanned, which a cursor keyed on
	// `(created_at, id)` resumes after ([Reader.Sent]): the last view may
	// sit above rows this build could not decode.
	lastAt int64
	lastID string

	// workingFor is whom each message of the page that carries one was
	// written for, by its id: taken before [Message.forReaders] strips it
	// from the view, and handed on by [Reader.TurnThread] alone.
	workingFor map[string]*types.WorkingFor
}

// readMessagePage reads one window of `chat_messages` and everything hanging
// off it.
//
// ONE FUNCTION FOR THE TRANSCRIPT AND THE THREAD: they differ only in the
// predicate and direction, and a second copy is where a page stops carrying
// reactions or overruns its limit. THE WINDOW IS BOUND HERE: placeholders are
// positional, so a caller appending the limit to its own `args` is one
// reordering from paging by a channel id, which reads like an empty room.
func readMessagePage(ctx context.Context, tx *sql.Tx, who string,
	where []string, args []any, order string, limit int) (messagePage, error) {

	// ONE MORE THAN THE LIMIT, and it is the cursor's evidence rather than
	// an answer: a page that returned it would overrun what the caller
	// asked for, and a NextCursor minted without it would send every
	// caller back for one empty page at the end of every room.
	args = append(slices.Clip(args), limit+1)
	rows, err := tx.QueryContext(ctx, `
		SELECT id, document, channel_seq, log_stream, log_generation, log_seq,
		       created_at
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
	held := statelog.RowsOf(ctx)
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
			createdAt  int64
		)
		if err := rows.Scan(&id, &document, &seq, &stream, &generation,
			&position, &createdAt); err != nil {

			return messagePage{}, fmt.Errorf("chatlet: scan a message: %w", err)
		}
		// THE BYTE BOUND ([statelog.RowsOf]), before this row is taken, so
		// the cursor names the last one the page holds and the next page
		// starts at this one.
		if !held.Take(len(document)) {
			more = true
			break
		}
		// THE CURSOR IS THE LAST ROW SCANNED, not the last one decoded.
		// A message this build cannot read still occupies a place in the
		// room's sequence, and a cursor that skipped it would hand the
		// next page a starting point above rows it never returned.
		lastSeq, out.lastAt, out.lastID = seq, createdAt, id
		ids = append(ids, id)
		message, err := DecodeMessage(document)
		if err != nil {
			out.undecodable++
			continue
		}
		if message.WorkingFor != nil {
			if out.workingFor == nil {
				out.workingFor = map[string]*types.WorkingFor{}
			}
			out.workingFor[message.ID] = message.WorkingFor
		}
		out.views = append(out.views, MessageView{
			Message: message.forReaders(), ChannelSeq: seq,
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
	if err := attachAnswers(ctx, tx, out.views); err != nil {
		return messagePage{}, err
	}
	for i := range out.views {
		out.views[i].Delivery = deliveryOf(out.views[i].Message, out.views[i].Answered, out.views[i].Taken)
	}
	return out, attachThreads(ctx, tx, out.views)
}

// attachAnswers puts on each post with a reach the seats that have answered
// it, and those whose running turn took it unanswered, in one statement for
// the page.
//
// ONLY THE POSTS THAT REACHED SOMEBODY ARE ASKED ABOUT, which is a person's
// posts and nothing else, so a page of agents talking costs no statement at
// all. The receipts are keyed (message, seat), and a page is at most
// [MaxLimit] ids, inside the 999 parameters the store guarantees.
func attachAnswers(ctx context.Context, tx *sql.Tx, views []MessageView) error {
	at := make(map[string]int, len(views))
	var ids []any
	for i, view := range views {
		if r := view.Message.Reach; r != nil && len(r.Recipients) > 0 {
			at[view.Message.ID] = i
			ids = append(ids, view.Message.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT message_id, handle, answer_id, COALESCE(answered_at, 0), taken, taken_at
		  FROM chat_receipts
		 WHERE message_id IN (`+placeholders(len(ids))+`) AND (answer_id <> '' OR taken <> '')
		 ORDER BY message_id, COALESCE(answered_at, taken_at), handle`, ids...)
	if err != nil {
		return fmt.Errorf("chatlet: read a page's answers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			id, handle, answerID, taken string
			answeredAt, takenAt         int64
		)
		if err := rows.Scan(&id, &handle, &answerID, &answeredAt, &taken, &takenAt); err != nil {
			return fmt.Errorf("chatlet: scan an answer: %w", err)
		}
		i, ok := at[id]
		switch {
		case !ok:
		case answerID != "":
			views[i].Answered = append(views[i].Answered,
				Answer{Handle: handle, MessageID: answerID, At: store.DecodeTime(answeredAt)})
		default:
			views[i].Taken = append(views[i].Taken, Take{Handle: handle, As: Taken(taken),
				At: store.DecodeTime(takenAt)})
		}
	}
	return rows.Err()
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
		SELECT thread_root, author_handle, author_kind, COUNT(*), MIN(channel_seq),
		       MAX(channel_seq)
		  FROM chat_messages
		 WHERE channel_id = ? AND thread_root IN (`+placeholders(len(ids))+`)
		 GROUP BY thread_root, author_handle, author_kind
		 ORDER BY thread_root, MIN(channel_seq)`, args...)
	if err != nil {
		return fmt.Errorf("chatlet: read room %s's threads: %w", room, err)
	}
	newestOf := map[string]int64{}
	for rows.Next() {
		var (
			root        string
			said        Message
			count       int
			first, last int64
		)
		if err = rows.Scan(&root, &said.Author, &said.AuthorKind, &count, &first,
			&last); err != nil {
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
		if author := said.speaker(); author != "" {
			summary.ReplierCount++
			if len(summary.Repliers) < MaxThreadRepliers {
				summary.Repliers = append(summary.Repliers, author)
			}
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
	// THE NEWEST REPLY'S INSTANT IS ITS created_at COLUMN, which holds the
	// instant its own document carries: the apply writes both from the
	// record's broker instant, which the state log hands it in the store's
	// encoding on every path ([statelog.Record.StoredAt]), so the column
	// names the reply at the instant the reply says without decoding a
	// document per thread — and of a reply this build cannot read as well.
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
		if err = last.Scan(&seq, &created, &stream, &generation,
			&position); err != nil {

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
// AGGREGATED IN SQL, not read as rows: the handles behind a count are
// [MaxReactionEmoji] times a room's membership. The viewer's own membership
// comes back in the same aggregate. THE VIEWER IS THE FIRST BIND because the
// placeholders are positional and the `CASE` precedes the `IN`; a page is at
// most [MaxLimit] ids plus this one, inside the store's 999 parameters.
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
