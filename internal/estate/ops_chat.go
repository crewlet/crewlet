package estate

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
)

// ---- the engine's own chat ---------------------------------------------- //
//
// THE KNOWLEDGE BASE'S CLASSES: the store derives a chat write's operation id
// (and a message's or a new room's) from the caller's key on its actor
// ([chatlet.Actor.OpKey]) and the gesture; unkeyed, it mints one afresh, and a
// repeat would be a second message. So every gesture is a once-write
// repeatable under the key ([op.repeatableWhen]); the erase and the prune take
// a whole operation id and are idempotent writes, as a purge is.
//
// EVERY READ IS ANSWERED FOR ITS VIEWER, in the SIGNED body ([Envelopes]), or a
// private room could be read as anybody; and carries the chat log's FLOOR, the
// search included: a seat that just joined a room must find it.

type chatChannelsArgs struct {
	Viewer string
	Query  chatlet.ChannelsQuery
	Fresh  statelog.Freshness
}

var opChatChannels = define("chat.channels", opRead, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatChannelsArgs) (chatlet.ChannelListing, error) {
		if b.Chat == nil {
			return chatlet.ChannelListing{}, errNoHalf
		}
		return b.Chat.Channels(ctx, a.Viewer, a.Query, a.Fresh)
	}).rowsTo(statelog.AnswerBytes)

type chatChannelArgs struct {
	Viewer    string
	ChannelID string
	Fresh     statelog.Freshness
}

var opChatChannel = define("chat.channel", opRead, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatChannelArgs) (chatlet.ChannelDetail, error) {
		if b.Chat == nil {
			return chatlet.ChannelDetail{}, errNoHalf
		}
		return b.Chat.Channel(ctx, a.Viewer, a.ChannelID, a.Fresh)
	}).oneAnswer()

type chatChannelNamedArgs struct {
	Viewer string
	Name   string
	Fresh  statelog.Freshness
}

// A ROOM BY ITS ADDRESS, answered for a room the viewer may not read exactly
// as for a name nobody holds — the reader's rule, kept on whichever node
// answers.
var opChatChannelNamed = define("chat.channel_named", opRead, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatChannelNamedArgs) (chatlet.ChannelDetail, error) {
		if b.Chat == nil {
			return chatlet.ChannelDetail{}, errNoHalf
		}
		return b.Chat.ChannelNamed(ctx, a.Viewer, a.Name, a.Fresh)
	}).oneAnswer()

type chatMessagesArgs struct {
	Viewer string
	Query  chatlet.TranscriptQuery
	Fresh  statelog.Freshness
}

var opChatMessages = define("chat.messages", opRead, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatMessagesArgs) (chatlet.Transcript, error) {
		if b.Chat == nil {
			return chatlet.Transcript{}, errNoHalf
		}
		return b.Chat.Messages(ctx, a.Viewer, a.Query, a.Fresh)
	}).rowsTo(statelog.AnswerBytes)

type chatThreadArgs struct {
	Viewer string
	Query  chatlet.ThreadQuery
	Fresh  statelog.Freshness
}

var opChatThread = define("chat.thread", opRead, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatThreadArgs) (chatlet.Thread, error) {
		if b.Chat == nil {
			return chatlet.Thread{}, errNoHalf
		}
		return b.Chat.Thread(ctx, a.Viewer, a.Query, a.Fresh)
	}).rowsTo(statelog.AnswerBytes)

// THE ENGINE'S TURN-START THREAD READ: chat.thread with whom each message was
// written for ([chatlet.TurnThread]), which no reader is handed. Its own
// operation, not a member on chat.thread's answer, which every reader's
// surface holds and an older data node would refuse; asked only through
// [TurnThreads], which the engine alone builds and which reads the plain
// thread where an older node answers it unserved.
var opChatTurnThread = define("chat.turn_thread", opRead, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatThreadArgs) (chatlet.TurnThread, error) {
		if b.Chat == nil {
			return chatlet.TurnThread{}, errNoHalf
		}
		return b.Chat.TurnThread(ctx, a.Viewer, a.Query, a.Fresh)
	}).rowsTo(statelog.AnswerBytes)

type chatRoomNamesArgs struct {
	Viewer string
	Fresh  statelog.Freshness
}

// THE NAMES OF THE ROOMS A VIEWER MAY READ, resolved by the serving reader from
// the viewer that crossed, as every chat read's set is: never a set of rooms a
// caller could hand over.
var opChatRoomNames = define("chat.room_names", opRead, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatRoomNamesArgs) (chatlet.RoomNames, error) {
		if b.Chat == nil {
			return chatlet.RoomNames{}, errNoHalf
		}
		return b.Chat.RoomNames(ctx, a.Viewer, a.Fresh)
	}).rowsTo(statelog.AnswerBytes)

// EVERY ROOM A VIEWER MAY BROWSE, resolved by the serving reader from the
// viewer that crossed, for [opChatRoomNames]' reason. Its arguments are that
// read's own.
var opChatDirectory = define("chat.directory", opRead, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatRoomNamesArgs) (chatlet.Directory, error) {
		if b.Chat == nil {
			return chatlet.Directory{}, errNoHalf
		}
		return b.Chat.Directory(ctx, a.Viewer, a.Fresh)
	}).rowsTo(statelog.AnswerBytes)

// ChatQuery is one ranked search of the company's chat, as it crosses: the
// VIEWER whose rooms it searches, never the rooms. Unlike [search.ChatQuery],
// the visible set is not a field: it is an authorization fact a caller must
// not supply, resolved by the serving node from the viewer, on the copy whose
// index it reads.
type ChatQuery struct {
	// Viewer is the seat the search is FOR. Required — the reader refuses a
	// read for nobody.
	Viewer string

	// Text is what somebody typed.
	Text string

	// Channel narrows to one room, empty for every room the viewer may
	// read. INTERSECTED with the visible set, never trusted: a room the
	// viewer may not read answers nothing, since a refusal would confirm it
	// exists.
	Channel string

	// Author narrows to one speaker's handle, empty for anybody.
	Author string

	// Limit bounds the hits; zero takes the index's own default.
	Limit int

	// Fresh is the level the VISIBLE SET is read at; the index has none,
	// each node maintaining it behind its applier.
	Fresh statelog.Freshness
}

// ChatSearchAnswer is one search's hits, and what they were drawn from.
type ChatSearchAnswer struct {
	// Hits is what matched, best first. Never nil.
	Hits []search.ChatHit

	// Rooms are the rooms the hits were said in, by id, each with the name
	// and kind a hit is shown under ([chatlet.ReadableRoom]), as a mention
	// carries its room's ([chatlet.Mention]). FROM THE SEARCH'S OWN VISIBLE
	// SET ([ChatReader.Readable]), never a read beside it, so a hit is shown
	// under the name of the room the search was allowed to read.
	Rooms map[string]chatlet.ReadableRoom

	// Searched is how many rooms the search covered: zero for a viewer who
	// may read none, an answer rather than a refusal.
	Searched int

	// Building says the answering node's index was not caught up with its
	// transcript for the whole search ([ChatSearcher.Ready]): the hits may be
	// partial or from an index a rebuild will drop, which a seat must not
	// read as "nobody said that". On the answer, not an operation of its own
	// as [opKnowledgeBuilding] is: the readiness is the ANSWERING node's walk's,
	// and a second call could reach another node.
	Building bool

	// Position is the answering copy's applied position on the chat log,
	// read BEFORE the visible set was resolved, so it is at or below the
	// read's own and never overstates what the answer saw.
	Position statelog.Position
}

// THE VISIBLE SET AND THE HITS ARE ONE NODE'S, hence one operation: two hops
// could reach two copies, and a set handed to another node's index would be an
// authorization fact crossing the wire, the field [ChatQuery] lacks.
//
// A viewer who may read nothing is ANSWERED, not refused ([search.ErrNoViewer]
// is for a caller that lost its viewer); a viewer who is nobody is refused by
// the reader. Readiness is asked on BOTH SIDES of the search and must hold at
// both: a rebuild started meanwhile shows only after, a walk that caught up
// meanwhile only before. A readiness this node could not read is BUILDING: it
// cannot promise full coverage, and failing the search would break a tool
// whose hits exist.
var opChatSearch = define("chat.search", opRead, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q ChatQuery) (ChatSearchAnswer, error) {
		if b.Chat == nil || b.ChatSearch == nil {
			return ChatSearchAnswer{}, errNoHalf
		}
		at := b.Chat.At()
		ready := chatIndexReady(ctx, b.ChatSearch)
		readable, err := b.Chat.Readable(ctx, q.Viewer, q.Fresh)
		if err != nil {
			return ChatSearchAnswer{}, err
		}
		rooms := make(map[string]chatlet.ReadableRoom, len(readable))
		visible := make([]string, 0, len(readable))
		for _, room := range readable {
			if q.Channel == "" || room.ID == q.Channel {
				rooms[room.ID] = room
				visible = append(visible, room.ID)
			}
		}
		out := ChatSearchAnswer{Hits: []search.ChatHit{}, Searched: len(visible),
			Building: !ready, Position: at}
		if len(visible) == 0 {
			return out, nil
		}
		hits, err := b.ChatSearch.SearchMessages(ctx, search.ChatQuery{
			Text: q.Text, Channels: visible, Author: q.Author, Limit: q.Limit,
		})
		if err != nil {
			return ChatSearchAnswer{}, err
		}
		out.Building = !ready || !chatIndexReady(ctx, b.ChatSearch)
		if hits != nil {
			out.Hits = hits
		}
		for _, hit := range hits {
			if out.Rooms == nil {
				out.Rooms = make(map[string]chatlet.ReadableRoom)
			}
			out.Rooms[hit.ChannelID] = rooms[hit.ChannelID]
		}
		return out, nil
	}).oneAnswer()

// chatIndexReady is whether this node's chat index holds its whole transcript,
// with a read that failed taken as not — see [opChatSearch] — and said in the
// log, since the answer then carries no reason of its own.
func chatIndexReady(ctx context.Context, index ChatSearcher) bool {
	ready, err := index.Ready(ctx)
	if err != nil {
		log.WarnContext(ctx, "chat_index_readiness_unread", "error", err.Error(),
			"detail", "a chat search was answered as building because this node "+
				"could not tell whether its index covers the transcript")
		return false
	}
	return ready
}

type chatCreateChannelArgs struct {
	Actor   chatlet.Actor
	Channel chatlet.NewChannel
}

var opChatCreateChannel = define("chat.create_channel", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatCreateChannelArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.CreateChannel(ctx, a.Actor, a.Channel)
	}).repeatableWhen(func(a chatCreateChannelArgs) bool { return chatKeyed(a.Actor) }).
	refusedBeforeAsking(func(a chatCreateChannelArgs) error { return chatActorWrites(a.Actor) })

type chatOpenDirectArgs struct {
	Actor        chatlet.Actor
	Participants []string
}

// openedDirect is [ChatWriter.OpenDirect]'s two answers, named.
type openedDirect struct {
	Written chatlet.Written
	Created bool
}

var opChatOpenDirect = define("chat.open_direct", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatOpenDirectArgs) (openedDirect, error) {
		if b.ChatWriter == nil {
			return openedDirect{}, errNoHalf
		}
		w, created, err := b.ChatWriter.OpenDirect(ctx, a.Actor, a.Participants)
		return openedDirect{Written: w, Created: created}, err
	}).repeatableWhen(func(a chatOpenDirectArgs) bool { return chatKeyed(a.Actor) }).
	refusedBeforeAsking(func(a chatOpenDirectArgs) error { return chatActorWrites(a.Actor) })

type chatPatchChannelArgs struct {
	Actor     chatlet.Actor
	ChannelID string
	Patch     chatlet.ChannelPatch

	// Authority is the caller's answer from the authority table: whether
	// it may move this room's archive, and the room's unit when decided.
	// Decided on the asking node, judged against the serving node's
	// snapshot: a room that has since left that unit is a conflict.
	Authority chatlet.ChannelAuthority
}

var opChatPatchChannel = define("chat.patch_channel", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatPatchChannelArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.PatchChannel(ctx, a.Actor, a.ChannelID, a.Patch, a.Authority)
	}).repeatableWhen(func(a chatPatchChannelArgs) bool { return chatKeyed(a.Actor) }).
	refusedBeforeAsking(func(a chatPatchChannelArgs) error { return chatActorWrites(a.Actor) })

type chatSetMembersArgs struct {
	Actor     chatlet.Actor
	ChannelID string
	Members   []chatlet.Member
	// Expected is the membership the caller read, nil for none
	// ([chatlet.Store.SetMembers]). A pointer, so the empty room a caller
	// expects crosses the wire as the empty list rather than as no
	// expectation at all.
	Expected *[]chatlet.Member `json:",omitempty"`
}

var opChatSetMembers = define("chat.set_members", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatSetMembersArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.SetMembers(ctx, a.Actor, a.ChannelID, a.Members, a.Expected)
	}).repeatableWhen(func(a chatSetMembersArgs) bool { return chatKeyed(a.Actor) }).
	refusedBeforeAsking(func(a chatSetMembersArgs) error { return chatActorWrites(a.Actor) })

type chatAddMembersArgs struct {
	Actor     chatlet.Actor
	ChannelID string
	Handles   []string
}

// addedMembers is [ChatWriter.AddMembers]' two answers, named.
type addedMembers struct {
	Written chatlet.Written
	Added   []string
}

var opChatAddMembers = define("chat.add_members", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatAddMembersArgs) (addedMembers, error) {
		if b.ChatWriter == nil {
			return addedMembers{}, errNoHalf
		}
		w, added, err := b.ChatWriter.AddMembers(ctx, a.Actor, a.ChannelID, a.Handles)
		return addedMembers{Written: w, Added: added}, err
	}).repeatableWhen(func(a chatAddMembersArgs) bool { return chatKeyed(a.Actor) }).
	refusedBeforeAsking(func(a chatAddMembersArgs) error { return chatActorWrites(a.Actor) })

type chatMembershipArgs struct {
	Actor     chatlet.Actor
	ChannelID string
}

var opChatJoin = define("chat.join", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatMembershipArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.Join(ctx, a.Actor, a.ChannelID)
	}).repeatableWhen(func(a chatMembershipArgs) bool { return chatKeyed(a.Actor) }).
	refusedBeforeAsking(func(a chatMembershipArgs) error { return chatActorWrites(a.Actor) })

var opChatLeave = define("chat.leave", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatMembershipArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.Leave(ctx, a.Actor, a.ChannelID)
	}).repeatableWhen(func(a chatMembershipArgs) bool { return chatKeyed(a.Actor) }).
	refusedBeforeAsking(func(a chatMembershipArgs) error { return chatActorWrites(a.Actor) })

type chatPostArgs struct {
	Actor     chatlet.Actor
	ChannelID string
	Message   chatlet.NewMessage
}

var opChatPost = define("chat.post", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatPostArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.Post(ctx, a.Actor, a.ChannelID, a.Message)
	}).repeatableWhen(func(a chatPostArgs) bool { return chatKeyed(a.Actor) }).
	refusedBeforeAsking(func(a chatPostArgs) error { return chatActorWrites(a.Actor) })

type chatReplyArgs struct {
	Actor     chatlet.Actor
	ChannelID string
	ReplyTo   string
	Message   chatlet.NewMessage
}

// A REPLY IS ITS OWN OPERATION rather than a post with an optional parent: the
// store refuses a reply that names no message, and one operation for both would
// turn that refusal into a post to the room.
var opChatReply = define("chat.reply", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatReplyArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.Reply(ctx, a.Actor, a.ChannelID, a.ReplyTo, a.Message)
	}).repeatableWhen(func(a chatReplyArgs) bool { return chatKeyed(a.Actor) }).
	refusedBeforeAsking(func(a chatReplyArgs) error { return chatActorWrites(a.Actor) })

// chatReachArgs is a post's dry run as it crosses: the post's own arguments,
// with the thread it would answer (empty for a room post).
type chatReachArgs struct {
	Actor     chatlet.Actor
	ChannelID string
	ReplyTo   string
	Message   chatlet.NewMessage
}

// A POST'S DRY RUN IS A READ: it decides on the serving node's snapshot, with
// the roster that node's write path routes by, and publishes nothing, so
// asking it again is asking again ([chatlet.Store.Reach]).
var opChatReach = define("chat.reach", opRead, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatReachArgs) (chatlet.Draft, error) {
		if b.ChatWriter == nil {
			return chatlet.Draft{}, errNoHalf
		}
		return b.ChatWriter.Reach(ctx, a.Actor, a.ChannelID, a.ReplyTo, a.Message)
	}).oneAnswer()

type chatEditArgs struct {
	Actor     chatlet.Actor
	ChannelID string
	MessageID string
	Edit      chatlet.EditMessage
}

var opChatEdit = define("chat.edit", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatEditArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.Edit(ctx, a.Actor, a.ChannelID, a.MessageID, a.Edit)
	}).repeatableWhen(func(a chatEditArgs) bool { return chatKeyed(a.Actor) }).
	refusedBeforeAsking(func(a chatEditArgs) error { return chatActorWrites(a.Actor) })

type chatDeleteArgs struct {
	Actor     chatlet.Actor
	ChannelID string
	MessageID string

	// Authority is the caller's answer from the authority table — whether
	// it may take down a message it did not write — decided on the asking
	// node, which holds the grants, and judged against the message's author
	// in the serving node's own snapshot.
	Authority chatlet.MessageAuthority
}

var opChatDelete = define("chat.delete", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatDeleteArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.Delete(ctx, a.Actor, a.ChannelID, a.MessageID, a.Authority)
	}).repeatableWhen(func(a chatDeleteArgs) bool { return chatKeyed(a.Actor) }).
	refusedBeforeAsking(func(a chatDeleteArgs) error { return chatActorWrites(a.Actor) })

type chatReactArgs struct {
	Actor     chatlet.Actor
	ChannelID string
	MessageID string
	Emoji     string

	// Remove takes the reaction back rather than adding it — one operation
	// for both, because the store's two gestures are one decision with
	// opposite intents.
	Remove bool
}

var opChatReact = define("chat.react", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatReactArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		if a.Remove {
			return b.ChatWriter.Unreact(ctx, a.Actor, a.ChannelID, a.MessageID, a.Emoji)
		}
		return b.ChatWriter.React(ctx, a.Actor, a.ChannelID, a.MessageID, a.Emoji)
	}).repeatableWhen(func(a chatReactArgs) bool { return chatKeyed(a.Actor) }).
	refusedBeforeAsking(func(a chatReactArgs) error { return chatActorWrites(a.Actor) })

type chatTakeArgs struct {
	Actor     chatlet.Actor
	ChannelID string
	MessageID string
	Handle    string
	As        chatlet.Taken
}

var opChatTake = define("chat.take_receipt", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatTakeArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.TakeReceipt(ctx, a.Actor, a.ChannelID, a.MessageID, a.Handle, a.As)
	}).repeatableWhen(func(a chatTakeArgs) bool { return chatKeyed(a.Actor) }).
	refusedBeforeAsking(func(a chatTakeArgs) error { return chatActorWrites(a.Actor) })

// ---- the admin's chat writes --------------------------------------------- //

type chatSetRetentionArgs struct {
	Actor     chatlet.Actor
	ChannelID string

	// Days is the room's override, and ABSENT CLEARS IT. A pointer, because
	// [chatlet.RetentionForever] is zero and real: a present 0 keeps the room
	// for ever, and collapsed into absent it would take the company's horizon.
	Days *int
}

// A ROOM'S RETENTION IS ITS OWN WRITE, never a patch field: a shorter horizon
// is a delayed prune, an admin's to set on a surface that asks a fresh proof of
// who they are, never one any member's patch could schedule. Every surface
// taking one has a key to make it repeatable.
var opChatSetRetention = define("chat.set_retention", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatSetRetentionArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.SetRetention(ctx, a.Actor, a.ChannelID, a.Days)
	}).repeatableWhen(func(a chatSetRetentionArgs) bool { return chatKeyed(a.Actor) }).
	refusedBeforeAsking(func(a chatSetRetentionArgs) error { return chatActorWrites(a.Actor) })

type chatEraseArgs struct {
	Actor      chatlet.Actor
	OpID       string
	ChannelID  string
	MessageIDs []string
	Reason     string
}

// AN ERASE crosses as a purge does: its operation id is the caller's, so a
// repeat after an unanswered one collapses on it, and the ledger answers that
// repeat with the first outcome.
var opChatErase = define("chat.erase", opIdempotentWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatEraseArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.Erase(ctx, a.Actor, a.OpID, a.ChannelID, a.MessageIDs, a.Reason)
	}).refusedBeforeAsking(func(a chatEraseArgs) error { return chatActorWrites(a.Actor) })

type chatPruneArgs struct {
	Actor     chatlet.Actor
	OpID      string
	ChannelID string
	Cutoff    time.Time
}

// A PRUNE crosses as an erase does, under the operation id its caller derived.
var opChatPrune = define("chat.prune", opIdempotentWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatPruneArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.Prune(ctx, a.Actor, a.OpID, a.ChannelID, a.Cutoff)
	}).refusedBeforeAsking(func(a chatPruneArgs) error { return chatActorWrites(a.Actor) })
