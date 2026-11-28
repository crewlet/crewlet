package builtin

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// Chatlet's tool names, re-exported from internal/chatlet, which declares them
// because its notification prompt names them: defined here and read there
// would be an import cycle.
const (
	PostMessageTool    = chatlet.PostMessageTool
	ReplyInThreadTool  = chatlet.ReplyInThreadTool
	SendDMTool         = chatlet.SendDMTool
	ReadChannelTool    = chatlet.ReadChannelTool
	SearchMessagesTool = chatlet.SearchMessagesTool
	ListChannelsTool   = chatlet.ListChannelsTool
	ReactToMessageTool = chatlet.ReactToMessageTool
	JoinChannelTool    = chatlet.JoinChannelTool
	LeaveChannelTool   = chatlet.LeaveChannelTool
)

// ChatWrites are the three that count as a DELIVERY: saying something is the
// answer on this surface, and without them the delivery gate would correct a
// turn into another round for doing what it was woken to do. See
// [chatlet.WriteTools] for why a reaction and a membership change are not here.
func ChatWrites() []string { return chatlet.WriteTools() }

// ChatReader is what these tools need from chat's read side.
//
// The level is part of the call, on [PageReader]'s reasoning: a seat reads at
// [seatReadLevel] (`linearizable`), since one that cannot find its own post
// posts again. Every read is for a viewer, and a room the viewer may not read
// answers NOT FOUND rather than confirming it exists.
type ChatReader interface {
	Channels(ctx context.Context, viewer string, q chatlet.ChannelsQuery,
		fresh statelog.Freshness) (chatlet.ChannelListing, error)
	// Channel is one room's own row, which the archive is decided on: who
	// made it, and the unit a unit's room belongs to.
	Channel(ctx context.Context, viewer, channelID string,
		fresh statelog.Freshness) (chatlet.ChannelDetail, error)
	// ChannelNamed resolves a room's NAME — its address — for a viewer,
	// answering not found for a room that viewer may not read. See
	// [ChatDeps.channelID].
	ChannelNamed(ctx context.Context, viewer, name string,
		fresh statelog.Freshness) (chatlet.ChannelDetail, error)
	Messages(ctx context.Context, viewer string, q chatlet.TranscriptQuery,
		fresh statelog.Freshness) (chatlet.Transcript, error)
	// Thread is one thread from its root. Asked of a reply as root, it
	// answers that message alone: how an edit and a delete learn whose words
	// they touch ([ChatDeps.remarkAuthority]).
	Thread(ctx context.Context, viewer string, q chatlet.ThreadQuery,
		fresh statelog.Freshness) (chatlet.Thread, error)
}

// ChatWriter is what these tools need from chat's write side.
//
// The actor is a parameter because one node's chat serves every seat and
// person through one write path ([PageWriter]'s arrangement). Every gesture's
// operation key rides on it ([chatlet.Actor.OpKey], set by [callOpKey]).
type ChatWriter interface {
	Post(ctx context.Context, actor chatlet.Actor, channelID string,
		in chatlet.NewMessage) (chatlet.Written, error)
	Reply(ctx context.Context, actor chatlet.Actor, channelID, replyTo string,
		in chatlet.NewMessage) (chatlet.Written, error)
	// OpenDirect also reports whether it CREATED the conversation: already
	// open is no failure, since the id derives from the participants and a
	// race's loser is handed the winner's room.
	OpenDirect(ctx context.Context, actor chatlet.Actor, participants []string) (
		chatlet.Written, bool, error)
	React(ctx context.Context, actor chatlet.Actor, channelID, messageID,
		emoji string) (chatlet.Written, error)
	Unreact(ctx context.Context, actor chatlet.Actor, channelID, messageID,
		emoji string) (chatlet.Written, error)
	Join(ctx context.Context, actor chatlet.Actor, channelID string) (chatlet.Written, error)
	Leave(ctx context.Context, actor chatlet.Actor, channelID string) (chatlet.Written, error)

	// The gestures only a person makes (chatoperator.go): on this writer
	// because they are the same store's, and a seat's registry never offers
	// them.
	CreateChannel(ctx context.Context, actor chatlet.Actor,
		in chatlet.NewChannel) (chatlet.Written, error)
	PatchChannel(ctx context.Context, actor chatlet.Actor, channelID string,
		patch chatlet.ChannelPatch, authority chatlet.ChannelAuthority) (chatlet.Written, error)
	SetMembers(ctx context.Context, actor chatlet.Actor, channelID string,
		members []chatlet.Member, expected *[]chatlet.Member) (chatlet.Written, error)
	AddMembers(ctx context.Context, actor chatlet.Actor, channelID string,
		handles []string) (chatlet.Written, []string, error)
	Edit(ctx context.Context, actor chatlet.Actor, channelID, messageID string,
		in chatlet.EditMessage) (chatlet.Written, error)
	Delete(ctx context.Context, actor chatlet.Actor, channelID, messageID string,
		authority chatlet.MessageAuthority) (chatlet.Written, error)
}

// ChatCursors is where a person's read state is written — the one chat write
// that is not a record on the log. See [ChatDeps.Cursors].
type ChatCursors interface {
	AdvanceChatRead(ctx context.Context, handle string,
		delta coord.ChatReadDelta) (coord.ChatReadState, error)
}

// ChatSearcher ranks a company's chat FOR ONE VIEWER: the estate router's chat
// search. The visible set is not an argument: the seam resolves which rooms the
// viewer may read, since it holds both the membership rows and the index, and a
// tool that passed the set could pass the wrong one.
//
// Nil on a build with no chat index omits the tool ([Register]'s rule).
type ChatSearcher interface {
	Search(ctx context.Context, q estate.ChatQuery) (estate.ChatSearchAnswer, error)
}

// The estate router's chat is each seam, held at compile time: every surface
// hands it to these tools, and a drifted method would fail only where a node
// is wired.
var (
	_ ChatReader   = estate.Chat{}
	_ ChatWriter   = estate.Chat{}
	_ ChatSearcher = estate.ChatSearch{}
)

// ChatDeps are chat's halves plus what a write needs.
type ChatDeps struct {
	Reader ChatReader
	Writer ChatWriter

	// Search ranks messages by what they say. A value, not a function of the
	// actor: what may be read is decided from the viewer it is handed.
	Search ChatSearcher

	// Authorize decides the checks a tool can only take after a read (a
	// room's archive, a message's author), as [WorkDeps.Authorize] does.
	// Pushed down by [Register] and [OperatorTools].
	Authorize Authorizer

	// Mentions resolves the handles a message names, so an @-mention wakes
	// the person meant. Nil resolves none: the message reaches only the
	// room's own subscribers.
	Mentions MentionResolver

	// ReadState is this viewer's whole read state: cursors per room
	// ([coord.ChatReadState.Cursors], what [chatlet.ChannelsQuery.Marks]
	// counts above), muted rooms and do-not-disturb. Whole, because a
	// person's assistant reads the dashboard's rail through `list_channels`,
	// and a muted room handed over as unread is a backlog they silenced.
	//
	// Nil, a seat's case (a cursor is a person's attention), counts nothing,
	// never every tail as unread for a model to catch up on; no cursors at
	// all is a person who opened nothing, to whom every tail IS unread.
	//
	// An error is not a cursor at zero (that reads every room as unread): the
	// listing carries no counts and says why, as the rail does.
	ReadState func(ctx context.Context, viewer string) (coord.ChatReadState, error)

	// Cursors writes a person's read state (cursors, mutes, do-not-disturb)
	// for `mark_chat_read`. Nil omits that tool, as on every seat's surface.
	//
	// Its error is the coordination store's, returned as it is: the tool
	// tells three answers apart by sentinel alone. [coord.ErrInvalidReadState]
	// is the caller's to change ([coord.ReadStateSentence]);
	// [coord.ErrWriteUnknown] may have landed, answered `unknown` and safe to
	// repeat (the merge is idempotent); anything else changed nothing (this
	// node's condition or fault). Re-worded, a sentinel is lost.
	Cursors ChatCursors

	// Now stamps a read-state write ([coord.ChatReadDelta.At]). Nil is the
	// wall clock, in UTC.
	Now func() time.Time

	// Actor decides who a write is attributed to and who a read is served as
	// (a private room's contents are its membership's). Nil takes the turn's
	// seat; the operator surface sets it. See [WorkDeps.Actor] for why this
	// is a seam. A surface whose transport takes an `Idempotency-Key` sets it
	// as [chatlet.Actor.OpKey], as [PageDeps.Actor] does; in a turn the key
	// is the turn's ([callOpKey]) whatever the actor carries.
	Actor func(ctx context.Context, turn *turnctx.Turn) (chatlet.Actor, error)

	// Await is handed the position every chat write landed at, so the next
	// read sees it: [WorkDeps.Await]'s seam, and here what keeps a seat
	// seeing its own message rather than posting it twice.
	Await func(ctx context.Context, at statelog.Position) error
}

// now is [ChatDeps.Now], or the wall clock — in UTC either way.
func (d ChatDeps) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

// settle hands a chat write's position to [ChatDeps.Await], so the next read
// sees it: after every write and before no read, since a model that cannot
// find what it just said says it again. A failure is LOGGED AND IGNORED: the
// message landed, and telling a model its post failed is the one answer that
// produces the duplicate.
func (d ChatDeps) settle(ctx context.Context, at statelog.Position) {
	if d.Await == nil || at.Seq == 0 {
		return
	}
	if err := d.Await(ctx, at); err != nil {
		log.WarnContext(ctx, "chat_write_not_applied_yet",
			"at", at.String(), "error", err.Error(),
			"detail", "the message landed on the fleet's log; the copy this "+
				"surface reads has not caught up, so reading the room in this "+
				"same turn may not show it yet")
	}
}

// chatActor builds the write's attribution from the TURN, never from
// arguments: a model that could name its own author could speak as anybody.
// TurnID is provenance (ADR-0017) and derives nothing; a re-run turn posts
// once by the call's key ([callOpKey]). WrittenFor, whom the turn works for,
// comes from the call's bound ceiling ([writtenForTurn]), so the turn a
// message wakes works for them too (ADR-0109).
func chatActor(ctx context.Context, turn *turnctx.Turn) (chatlet.Actor, error) {
	seat, err := turn.RequireSeat()
	if err != nil {
		return chatlet.Actor{}, err
	}
	writtenFor, err := writtenForTurn(ctx, turn, seat.Handle(), nil)
	if err != nil {
		return chatlet.Actor{}, err
	}
	return chatlet.Actor{
		Handle: seat.Handle(), Kind: chatlet.AuthorAgent, WrittenFor: writtenFor,
		TurnID: turn.RunID, Chain: turn.Chain,
	}, nil
}

// actor resolves who this call acts as ([ChatDeps.Actor]), keyed for a write
// this call makes ([callOpKey]). A room is spoken in and read as a seat, so an
// actor naming none is refused whichever surface built it: the authority
// table refuses it first (`unseated`), and this guards a surface wired without
// the table, where a login would read every public room.
func (d ChatDeps) actor(ctx context.Context, turn *turnctx.Turn, tool string,
	args map[string]any) (chatlet.Actor, *tools.Result) {

	var (
		actor chatlet.Actor
		err   error
	)
	if d.Actor != nil {
		actor, err = d.Actor(ctx, turn)
	} else {
		actor, err = chatActor(ctx, turn)
	}
	if err != nil {
		out := chatActorFailure(tool, err)
		return chatlet.Actor{}, &out
	}
	if actor.Kind != chatlet.AuthorAgent && actor.Kind != chatlet.AuthorHuman ||
		actor.Name() == "" {
		out := refused(tools.RefusalForbidden, fmt.Sprintf("%s is said and read "+
			"as a seat, and %q acts as none — bind the credential to a human "+
			"seat in the identity directory (`crewlet iam bind`).", tool,
			clip(actor.Name())))
		return chatlet.Actor{}, &out
	}
	actor.OpKey = callOpKey(turn, tool, chatlet.CallFamily(tool), args, actor.OpKey)
	return actor, nil
}

// chatActorFailure is what a call with nobody to act as reports:
// [tools.RefusalForbidden], since nothing in the arguments is wrong. No seat,
// or a requester that could not be composed, answers [unattributed]'s
// sentence; a request that carried nobody is told so.
func chatActorFailure(name string, err error) tools.Result {
	if errors.Is(err, turnctx.ErrNoSeat) || errors.Is(err, ErrUndecidable) {
		return unattributed(name, err)
	}
	return refusedBy(tools.RefusalForbidden, err, name+" has nobody to act as: "+
		"this request carries no person bound to a seat. Sign in as the person "+
		"whose seat this is, or bind the credential to a human seat in the "+
		"identity directory (`crewlet iam bind`).")
}

// unconfiguredChat is the refusal when the company runs no Chatlet.
//
// [tools.RefusalUnavailable], on [unconfigured]'s reasoning: the call is well
// formed, and the room may well exist in the chat the company DOES run.
func unconfiguredChat(name string) tools.Result {
	return refusedBy(tools.RefusalUnavailable, waitless(), name+" is unavailable: this "+
		"company does not run Chatlet. Use the chat tools your company has "+
		"configured.")
}

// chatClass is the class of a refusal the chat store decided, and false for
// anything else. The one table for a chat tool's failure and the person's write
// surface ([WriterClass]): two lists would drift on the sentinel a new gesture
// adds.
func chatClass(err error) (tools.Refusal, bool) {
	var invalid *chatlet.InvalidError
	switch {
	case errors.Is(err, chatlet.ErrNotFound):
		return tools.RefusalNotFound, true
	case errors.Is(err, chatlet.ErrForbidden):
		return tools.RefusalForbidden, true
	case errors.Is(err, chatlet.ErrNameTaken), errors.Is(err, chatlet.ErrRoomExists):
		return tools.RefusalExists, true
	case errors.Is(err, chatlet.ErrMembersChanged):
		// A read outgrown, not a race lost: reading the members again cures it.
		return tools.RefusalStaleVersion, true
	case errors.Is(err, chatlet.ErrConflict), errors.Is(err, chatlet.ErrArchived),
		errors.Is(err, chatlet.ErrReactionsFull):
		// A lost race, or the room's or message's state, on the rule every
		// domain's refusals take ([tools.Refusal]): only reopening the room or
		// taking an emoji off makes it land. Not `invalid`, which would tell
		// the caller to change an argument that is fine.
		return tools.RefusalConflict, true
	case errors.Is(err, chatlet.ErrUnknownKind):
		// This node's condition, not a permission: a newer peer made the
		// room, and the call lands once this node is upgraded.
		return tools.RefusalUnavailable, true
	case errors.Is(err, chatlet.ErrInvalid), errors.As(err, &invalid):
		return tools.RefusalInvalid, true
	}
	return "", false
}

// chatWriteFailure explains a write that did not land, in words the model can
// act on, with the class beneath the store's error ([refusedBy]) for a surface
// answering in status codes. Each sentinel has its own sentence and its class
// from [chatClass]. The store wraps every refusal it decides in a sentinel, so
// the remainder is the node's, as in [pageWriteFailure]: a condition
// ([tools.RefusalUnavailable]) or a fault ([faulted]). A write the router never
// heard back about ([estate.ErrOutcomeUnknown]) may have landed: UNKNOWN
// ([UnknownCause]). Invalid and forbidden carry [chatlet.Sentence] without the
// Go prefix: the field or rule it names is what the caller changes.
//
// `next` is what the caller does about an UNKNOWN outcome: [unknownNext] for a
// tool that adds, [restateNext] for one that states.
func chatWriteFailure(ctx context.Context, name string, err error, next string) tools.Result {
	if errors.Is(err, estate.ErrOutcomeUnknown) {
		return tools.Result{Output: fmt.Sprintf("%s: %s. It may have landed and "+
			"it may not — do not report it as done, and do not report it as "+
			"failed. %s", name, Unanswered, next),
			Failed: true, Cause: UnknownCause("", err)}
	}
	class, ok := chatClass(err)
	if !ok {
		if why, cond := Condition(err); cond {
			return refusedBy(tools.RefusalUnavailable, err, sentence(fmt.Sprintf("%s did not "+
				"land (%s): nothing was written — do not report it as done.",
				name, why), conditionNext(err, "", stuckWrite)))
		}
		// Not "nothing was written", for [writeFailure]'s reason: a fault
		// while the append resolved leaves a record nobody can account for.
		return faulted(ctx, name, err, fmt.Sprintf("%s failed: %s. Do not report "+
			"it as done, and do not make it again by other means — say it could "+
			"not be made.", name, faultSaid))
	}
	switch {
	case errors.Is(err, chatlet.ErrArchived):
		return refusedBy(class, err, fmt.Sprintf("%s refused that: %s. Nothing "+
			"more can be said there — say it somewhere that is still open, or "+
			"say nothing.", name, chatlet.Sentence(err)))
	case errors.Is(err, chatlet.ErrReactionsFull):
		return refusedBy(class, err, fmt.Sprintf("%s refused that: %s.", name,
			chatlet.Sentence(err)))
	case class == tools.RefusalInvalid:
		return refusedBy(class, err, fmt.Sprintf("%s refused that: %s", name,
			chatlet.Sentence(err)))
	case class == tools.RefusalForbidden:
		return refusedBy(class, err, fmt.Sprintf("%s was refused: %s. This is a "+
			"permission, not a retry: asking again will be refused the same way.",
			name, chatlet.Sentence(err)))
	case errors.Is(err, chatlet.ErrUnknownKind):
		return refusedBy(class, err, fmt.Sprintf("%s could not be served on this "+
			"node: %s. Nothing was written.", name, chatlet.Sentence(err)))
	case errors.Is(err, chatlet.ErrNameTaken):
		// The domain's sentence names the room and the ways on; this adds
		// where a seat finds it.
		return refusedBy(class, err, fmt.Sprintf("%s: %s. A room's name is its "+
			"address, and "+ListChannelsTool+" names the rooms you are in.",
			name, chatlet.Sentence(err)))
	case class == tools.RefusalExists:
		return refusedBy(class, err, fmt.Sprintf("%s: %s.", name, chatlet.Sentence(err)))
	case errors.Is(err, chatlet.ErrMembersChanged):
		// A read outgrown: the change was stated against members the room no
		// longer holds, and a person is told so in their words.
		return toPerson(refusedBy(class, err, fmt.Sprintf("%s was refused: %s, so "+
			"nothing was written. Read the room's members again and state the "+
			"change against what it holds now.", name, chatlet.Sentence(err))),
			caveat.MembersChanged())
	case class == tools.RefusalConflict:
		// A lost race, naming the read before a retry. Another domain's tool
		// says it via [statelog.LostRace] ([WriterSentence],
		// [chatlet.Sentence]); a person's surface in its code's (ADR-0060).
		return refusedBy(class, err, fmt.Sprintf("%s could not land: the room "+
			"kept changing under this write, so nothing was written. Read it again "+
			"before retrying.", name))
	}
	// A room or message that is not there. Either address reaches any room
	// the caller may read ([ChatDeps.channelID]), so neither is offered as
	// the cure for the other.
	return refusedBy(class, err, fmt.Sprintf("%s: %s\n\nCheck what you named: "+
		ListChannelsTool+" names the rooms you are in, any room you may read answers "+
		"to its id or to its name, and "+ReadChannelTool+" shows each message's id.",
		name, chatlet.Sentence(err)))
}

// chatUnknown answers a chat write whose outcome is unknown as neither, as
// [pageUnknown] does, with [UnknownOutcome] as its cause. An unvouched one says
// so: this node's ledger may have lost the record, so the same operation
// answers the same way here every time. `next` says what a repeat is.
func chatUnknown(name string, result statelog.Result, next string) tools.Result {
	return failedUnknown(result.OpID, result.Unvouched, chatUnknownText(name, result, next))
}

// chatUnknownText is [chatUnknown]'s sentence.
func chatUnknownText(name string, result statelog.Result, next string) string {
	why := "the write's acknowledgement was lost"
	if result.Unvouched {
		why = "this node's operation ledger may have lost the record of it, so " +
			"this node cannot tell"
	}
	return fmt.Sprintf("%s: whether this landed is unknown (%s; operation %s). "+
		"It may have landed and it may not — do not report it as done, and do "+
		"not report it as failed. %s", name, why, result.OpID, next)
}

// stated is the receipt of a write that STATES something (a reaction, a
// membership, a room's settings), or the unknown answer. Its repeat is offered
// ([restateNext]): stating the same thing again changes nothing if the first
// landed.
func stated(tool string, written chatlet.Written, look string,
	out map[string]any) (tools.Result, error) {

	if written.Outcome.Outcome == statelog.OutcomeUnknown {
		return chatUnknown(tool, written.Outcome, restateNext(look)), nil
	}
	return jsonResult(statedReceipt(written, out))
}

// statedReceipt is out with what the write [stated] landed as: its revision,
// its outcome and its position.
func statedReceipt(written chatlet.Written, out map[string]any) map[string]any {
	out["revision"] = written.Revision
	out["outcome"] = outcomeOf(written.Outcome)
	out["position"] = positionOf(written.Outcome.Position)
	return out
}

// directUnknown is send_dm's answer when the conversation's OPEN came back
// `unknown`. It stops there: the post would go to a room this call cannot name
// (an unvouched open answers no id; a lost acknowledgement names a room this
// node may not have applied) and be refused as a misleading "not found".
//
// Its advice is not [unknownNext]'s: nothing was said, and a direct
// conversation is its participants' whatever opens it, so a repeat cannot say
// anything twice.
//
// `why` is what made it unknown, `opID` the open's operation and `unvouched`
// whether this node's ledger is what cannot tell.
func directUnknown(to []string, why, opID string, unvouched bool, again string) tools.Result {
	next := "Call " + SendDMTool + " again"
	if again != "" {
		next = capitalize(again)
	}
	next += ": the conversation is opened once however often it is asked for, " +
		"and the message is then said once."
	if unvouched {
		next += " Until the open reaches this node the same call answers the " +
			"same way, so if it keeps doing so, say you could not send it."
	}
	return failedUnknown(opID, unvouched, fmt.Sprintf(SendDMTool+": whether the "+
		"conversation with %s could be opened is unknown (%s), so the message "+
		"was NOT sent — nothing was said. %s", clip(strings.Join(to, ", ")), why, next))
}

// unknownWhy says why a write that answered `unknown` is unknown, naming its
// operation: `whose` is the write's own possessive ("the open's").
func unknownWhy(whose string, result statelog.Result) string {
	if result.Unvouched {
		return fmt.Sprintf("this node's operation ledger may have lost the "+
			"record of it, so this node cannot tell; operation %s", result.OpID)
	}
	return fmt.Sprintf("%s acknowledgement was lost; operation %s", whose, result.OpID)
}

// chatReadFailure explains a read that could not be served. It never says
// "nothing found" ([readFailure]): a seat that believes nobody answered asks
// again. A room reported absent is [tools.RefusalNotFound], a reader with no
// seat [tools.RefusalForbidden], and a refused argument (a cursor that is no
// position, a thread named by nothing) [tools.RefusalInvalid], since "try
// again" would be refused the same way. Anything else is the node's: a
// condition, or a fault ([faulted]).
func chatReadFailure(ctx context.Context, name string, err error) tools.Result {
	class, ok := chatClass(err)
	switch {
	case ok && class == tools.RefusalNotFound:
		return refusedBy(class, err, fmt.Sprintf("%s: %s\n\nCheck the room with "+
			ListChannelsTool+". A private room you are not in answers the same way as "+
			"one that does not exist.", name, chatlet.Sentence(err)))
	case ok && (class == tools.RefusalInvalid || class == tools.RefusalForbidden):
		return refusedBy(class, err, fmt.Sprintf("%s was refused: %s", name,
			chatlet.Sentence(err)))
	case ok:
		return refusedBy(class, err, fmt.Sprintf("%s could not be served on this "+
			"node: %s", name, chatlet.Sentence(err)))
	}
	if why, cond := Condition(err); cond {
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s could not "+
			"read the company's chat%s (%s). This is NOT an empty room — "+
			"do not conclude nothing was said. %s", name, rightNow(err), why,
			conditionNext(err, "Try again, or say you could not check.", stuckRead)))
	}
	return faulted(ctx, name, err, fmt.Sprintf("%s could not read the company's "+
		"chat: %s. This is NOT an empty room — do not conclude nothing was said. "+
		"Say you could not check.", name, faultSaid))
}

// ---- addressing a room ------------------------------------------------- //

// channelID resolves what a model typed to a room id. A name resolves because
// a model will type one: an id is a uuid no conversation says aloud. An id
// argument costs no lookup.
//
// Any room the viewer may read resolves, not only those it is in: a name is
// the room's company-wide address ([chatlet.Reader.ChannelNamed]), so a public
// or unit's room resolves joined or not, and a private room only for members.
//
// A name nothing readable holds is [tools.RefusalInvalid], never not-found: the
// fix is the argument, and a private room answers as a name nobody holds. A
// lookup this node could not serve is [chatReadFailure]'s.
func (d ChatDeps) channelID(ctx context.Context, name, viewer, ref string) (
	string, *tools.Result) {

	// The room as people write it ([chatlet.RoomRef]): its id, or its name
	// with or without '#'.
	id, want := chatlet.RoomRef(ref)
	if id != "" {
		return id, nil
	}
	if want == "" {
		out := failed(name + " needs a `channel` — the room's id, or its name.")
		return "", &out
	}
	if d.Reader == nil {
		// Nothing here resolves a name, so the fix is the id: the
		// argument's class, not the node's.
		out := failed(fmt.Sprintf("%s cannot resolve the room name %q here — "+
			"name the room by its id.", name, clip(want)))
		return "", &out
	}
	// Archived rooms included: a read of one is legitimate, and the store's
	// refusal of a write names the room, better than "no such channel".
	room, err := d.Reader.ChannelNamed(ctx, viewer, want, seatRead)
	switch {
	case errors.Is(err, chatlet.ErrNotFound):
		out := refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s: there is no "+
			"room called %q that you can read — "+ListChannelsTool+" names the rooms you "+
			"are in, and a private room you are not in cannot be addressed at all.",
			name, clip(want)))
		return "", &out
	case err != nil:
		out := chatReadFailure(ctx, name, err)
		return "", &out
	}
	return room.Channel.ID, nil
}

// resolved is the shared preamble of every tool here: who is acting, keyed
// for this call, and which room they meant.
func (d ChatDeps) resolved(ctx context.Context, turn *turnctx.Turn, name string,
	args map[string]any) (chatlet.Actor, string, *tools.Result) {

	actor, refusal := d.actor(ctx, turn, name, args)
	if refusal != nil {
		return chatlet.Actor{}, "", refusal
	}
	id, refusal := d.channelID(ctx, name, actor.Name(), argString(args, "channel"))
	if refusal != nil {
		return chatlet.Actor{}, "", refusal
	}
	return actor, id, nil
}

// mayWrite is [WorkDeps.mayWrite] for chat.
func (d ChatDeps) mayWrite(ctx context.Context, action authz.Action,
	object authz.Object) *tools.Result {

	return askAuthority(ctx, d.Authorize, action, object)
}

// remarkAuthority decides an edit or a delete of one message, and says whether
// it was admitted as MODERATION (on another's words) or as their author.
//
// First on the unread message ([authz.Object.Unresolved]): admin is admitted
// there, so a moderator reaches a message in a room they are not in without
// being shown it. Anybody else gets [authz.ErrUnresolved]: read the message as
// this viewer (unreadable is NOT FOUND) and decide again on its author, the
// only one then admitted.
//
// moderate crosses to the store as the decision ([chatlet.MessageAuthority]),
// never re-derived: the serving node judges it against the author its own
// snapshot reads, since this read was another transaction's.
func (d ChatDeps) remarkAuthority(ctx context.Context, action authz.Action,
	viewer, channelID, messageID string) (moderate bool, refusal *tools.Result) {

	if d.Authorize == nil {
		out := authorityRefusal(string(action), ErrNoAuthorizer)
		return false, &out
	}
	err := d.Authorize(ctx, action, authz.Object{Kind: authz.KindMessage, Unresolved: true})
	switch {
	case err == nil:
		return true, nil
	case !errors.Is(err, authz.ErrUnresolved):
		out := authorityRefusal(string(action), err)
		return false, &out
	}
	if d.Reader == nil {
		out := failed(fmt.Sprintf("%s cannot read the message here to say whose "+
			"words it is, so it cannot be decided.", action))
		return false, &out
	}
	held, readErr := d.Reader.Thread(ctx, viewer, chatlet.ThreadQuery{
		ChannelID: channelID, RootID: messageID, Limit: 1,
	}, seatRead)
	if readErr != nil {
		out := chatReadFailure(ctx, string(action), readErr)
		return false, &out
	}
	if refused := d.mayWrite(ctx, action, authz.Object{
		Kind: authz.KindMessage, ID: messageID, Container: channelID,
		Author: held.Root.Message.Author,
	}); refused != nil {
		return false, refused
	}
	return false, nil
}

// collectiveAddress matches `@channel` as a whole word: the right bound stops
// `@channels` waking the room, the left class an email address or a path
// ending in the word.
var collectiveAddress = regexp.MustCompile(`(^|[^0-9A-Za-z_@])@channel($|[^0-9A-Za-z_-])`)

// addressesRoom reports whether this message addressed the whole room. Derived
// from the body, not an argument: a woken seat's prompt says a room is
// addressed by writing `@channel`, and a flag beside the text could disagree
// with it either way.
func addressesRoom(body string) bool {
	return collectiveAddress.MatchString(body)
}

// message builds what a post or a reply carries from the body the model wrote.
// The actor's key derives the message's id (chatlet/ids.go): in a turn from the
// unit of work and this call's repeat count, so a re-run posts each message
// once and a second remark is never the first's retry; under a request from the transport's key, so a person's retry is the
// same message. An operator's assistant over MCP names neither, so each post is
// new, as a person pressing send twice.
func (d ChatDeps) message(body string, links []string, asks bool) chatlet.NewMessage {
	in := ChatMessage(d.Mentions, body, links)
	in.Asks = asks
	return in
}

// ChatMessage is what a post carries, read from the words a person or a model
// wrote: the seats it names (through mentions, which may be nil) and whether
// it addresses the whole room. Exported for the post's dry run (`chat_reach`):
// one function reads the words on both sides, so whom a draft is told it
// reaches is whom the posted words reach.
func ChatMessage(mentions MentionResolver, body string, links []string) chatlet.NewMessage {
	in := chatlet.NewMessage{Body: body, Links: links, Collective: addressesRoom(body)}
	if mentions != nil {
		in.Mentions = mentions.Mentions(body)
	}
	return in
}

// linkShape is what each link a chat tool takes is, as its `links` says it:
// the store refuses any other ([chatlet.LinkSchemes]), and the description is
// where a model learns the rule before it writes rather than from a refusal.
const linkShape = "each an absolute http://, https:// or mailto: address written " +
	"whole, as https://example.com/page or mailto:name@example.com"

// posted is the receipt every posting tool answers with, or for an unknown
// outcome the unknown answer, never a receipt: a `message_id` reads as the
// message said, when an unvouched operation is answered `unknown` without
// publishing ([unknownWrite]).
//
// `again` is how this caller repeats the post as the same message
// ([callRepeat]): a seat's same call, a person's same request, or nothing (an
// operator's assistant over MCP, whose repeat is a second message).
func posted(tool string, written chatlet.Written, channelID, again string) (tools.Result, error) {
	if written.Outcome.Outcome == statelog.OutcomeUnknown {
		return chatUnknown(tool, written.Outcome,
			postNext(written.Outcome.Unvouched, again, "Read the room with "+ReadChannelTool)), nil
	}
	return jsonResult(postReceipt(written, channelID))
}

// chatWritten is the record a message write wrote: the conversation the message
// is in, as a wake about a reply in its thread is stamped
// ([chatlet.MessageConversation]). None for an answer the node could not vouch
// for, which names no room ([chatlet.Written]).
func chatWritten(w chatlet.Written) types.WrittenRecord {
	return types.ConversationWritten(chatlet.MessageConversation(w.Channel, w.Message))
}

// postNext is [unknownNext] for a message, in one place: a post comes back
// unknown two ways (an answer that said so, a data node that never answered)
// that must say the same. `look` names the read that shows the room.
func postNext(unvouched bool, again, look string) string {
	return unknownNext(unvouched, again, look, "that is a second message")
}

// postReceipt is what a post that landed answers: the message, where it was
// said, and the outcome and position a caller reads after it — see [posted].
func postReceipt(written chatlet.Written, channelID string) map[string]any {
	out := map[string]any{
		"message_id": written.ChangeID, "channel": channelID,
		"revision": written.Revision,
		"outcome":  outcomeOf(written.Outcome),
		"position": positionOf(written.Outcome.Position),
	}
	if root := written.Message.ThreadRoot; root != "" {
		out["thread_root"] = root
	}
	if len(written.Message.Mentions) > 0 {
		out["mentioned"] = written.Message.Mentions
	}
	if written.Message.AuthorKind == chatlet.AuthorAgent && !written.Message.Asks {
		// SAID BACK, so a seat that meant to ask learns at once that its
		// words reached only whom they name (ADR-0113).
		out["asks"] = false
		out["wakes"] = "only the colleagues it names, whoever's question it " +
			"answers and anyone in the thread who still owes an answer; post " +
			"again with asks: true if you need an answer from the room"
	}
	if written.Message.Collective {
		// Said back: the model wrote `@channel` in prose, and the receipt
		// is where it learns the whole room is about to be woken.
		out["addressed_whole_room"] = true
	}
	if reach := written.Message.Reach; reach != nil {
		// Whom a person's post reached, so they learn at once whether any
		// agent was asked rather than waiting on an answer nobody owes.
		out["reached"] = reach.Recipients
		if len(reach.Recipients) == 0 {
			out["reached_no_agent"] = true
		}
	}
	return out
}

// ---- post_message ------------------------------------------------------ //

type postMessage struct{ deps ChatDeps }

var _ tools.SeatCallable = (*postMessage)(nil)

func (t *postMessage) Name() string { return PostMessageTool }

func (t *postMessage) Description() string {
	return "Say something new in a room. This starts a TOP-LEVEL message: to " +
		"answer something somebody said, use " + ReplyInThreadTool + " instead, which " +
		"puts your answer beside the question where the next reader will " +
		"find it. @-mention a colleague by handle, with `asks: true`, to wake them; " +
		"write @channel only when everybody in the room genuinely needs to " +
		"stop what they are doing."
}

func (t *postMessage) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"channel": map[string]any{
				"type": "string",
				"description": "The room: its id, or its name (\"#eng\" and " +
					"\"eng\" are the same room).",
			},
			"body": map[string]any{
				"type": "string",
				"description": "What to say, in markdown. @handle reaches one " +
					"colleague; @channel wakes the room.",
			},
			"links": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": fmt.Sprintf("Up to %d URLs this message points "+
					"at, %s. There are no file attachments.", chatlet.MaxLinks, linkShape),
			},
			"asks": asksParam(chatAsks),
		},
		"required": []any{"channel", "body"},
	}
}

func (t *postMessage) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *postMessage) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	if t.deps.Writer == nil {
		return unconfiguredChat(PostMessageTool), nil
	}
	body := strings.TrimSpace(unfence(turn, argString(args, "body")))
	if body == "" {
		return failed(PostMessageTool + " needs a `body` — an empty message is a " +
			"wake for nothing."), nil
	}
	actor, id, refusal := t.deps.resolved(ctx, turn, PostMessageTool, args)
	if refusal != nil {
		return *refusal, nil
	}
	asks, refused := asksOf(PostMessageTool, args, actor.Kind == chatlet.AuthorAgent)
	if refused != nil {
		return *refused, nil
	}
	again := callRepeat(turn, actor.OpKey, PostMessageTool)
	written, err := t.deps.Writer.Post(ctx, actor, id,
		t.deps.message(body, argStrings(args, "links"), asks))
	if err != nil {
		return chatWriteFailure(ctx, PostMessageTool, err,
			postNext(false, again, "Read the room with "+ReadChannelTool)), nil
	}
	commit(turn, written.Outcome.Outcome, types.CommittedChat, chatWritten(written))
	t.deps.settle(ctx, written.Outcome.Position)
	return posted(PostMessageTool, written, id, again)
}

// ---- reply_in_thread --------------------------------------------------- //

type replyInThread struct{ deps ChatDeps }

var _ tools.SeatCallable = (*replyInThread)(nil)

func (t *replyInThread) Name() string { return ReplyInThreadTool }

func (t *replyInThread) Description() string {
	return "Answer a message, under its own thread. This is where an answer " +
		"belongs: it sits beside the question, and everybody already in that " +
		"thread hears it. Replying to a reply puts you in the same thread — " +
		"threads here are one level deep."
}

func (t *replyInThread) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"channel": map[string]any{
				"type":        "string",
				"description": "The room: its id, or its name.",
			},
			"reply_to": map[string]any{
				"type": "string",
				"description": "The id of the message you are answering — the " +
					"one you were woken about, or one " + ReadChannelTool + " returned.",
			},
			"body": map[string]any{
				"type":        "string",
				"description": "Your answer, in markdown.",
			},
			"links": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": fmt.Sprintf("Up to %d URLs, %s. No file attachments.",
					chatlet.MaxLinks, linkShape),
			},
			"asks": asksParam(chatAsks),
		},
		"required": []any{"channel", "reply_to", "body"},
	}
}

func (t *replyInThread) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *replyInThread) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	if t.deps.Writer == nil {
		return unconfiguredChat(ReplyInThreadTool), nil
	}
	body := strings.TrimSpace(unfence(turn, argString(args, "body")))
	replyTo := strings.TrimSpace(argString(args, "reply_to"))
	switch {
	case body == "":
		return failed(ReplyInThreadTool + " needs a `body`."), nil
	case replyTo == "":
		return failed(ReplyInThreadTool + " needs a `reply_to` — the id of the " +
			"message you are answering. To start a new topic, use " + PostMessageTool + "."), nil
	}
	actor, id, refusal := t.deps.resolved(ctx, turn, ReplyInThreadTool, args)
	if refusal != nil {
		return *refusal, nil
	}
	asks, refused := asksOf(ReplyInThreadTool, args, actor.Kind == chatlet.AuthorAgent)
	if refused != nil {
		return *refused, nil
	}
	again := callRepeat(turn, actor.OpKey, ReplyInThreadTool)
	written, err := t.deps.Writer.Reply(ctx, actor, id, replyTo,
		t.deps.message(body, argStrings(args, "links"), asks))
	if err != nil {
		return chatWriteFailure(ctx, ReplyInThreadTool, err,
			postNext(false, again, "Read the room with "+ReadChannelTool)), nil
	}
	commit(turn, written.Outcome.Outcome, types.CommittedChat, chatWritten(written))
	t.deps.settle(ctx, written.Outcome.Position)
	return posted(ReplyInThreadTool, written, id, again)
}

// ---- send_dm ----------------------------------------------------------- //

type sendDM struct{ deps ChatDeps }

var _ tools.SeatCallable = (*sendDM)(nil)

func (t *sendDM) Name() string { return SendDMTool }

func (t *sendDM) Description() string {
	return "Say something privately to one colleague, or to a few. Use it for " +
		"what is genuinely between you — NEVER to answer a question that was " +
		"asked in the open, where the answer is the thing everybody else in " +
		"the room is waiting for too. The conversation is opened if it is not " +
		"open already."
}

func (t *sendDM) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"to": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": fmt.Sprintf("Handles, up to %d including you. "+
					"Past that a conversation is a room, which has a name "+
					"people can join by.", chatlet.MaxDMParticipants),
			},
			"body": map[string]any{"type": "string", "description": "What to say."},
			"links": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": fmt.Sprintf("Up to %d URLs this message points "+
					"at, %s. There are no file attachments.", chatlet.MaxLinks, linkShape),
			},
			"asks": asksParam(chatAsks),
		},
		"required": []any{"to", "body"},
	}
}

func (t *sendDM) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *sendDM) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	if t.deps.Writer == nil {
		return unconfiguredChat(SendDMTool), nil
	}
	body := strings.TrimSpace(unfence(turn, argString(args, "body")))
	// A bare string means the one-element array; [argStrings] reads both.
	to := argStrings(args, "to")
	switch {
	case body == "":
		return failed(SendDMTool + " needs a `body`."), nil
	case len(to) == 0:
		return failed(SendDMTool + " needs a `to` — the handle of the colleague you " +
			"are writing to. Use " + LookupColleagueTool + " if you are unsure of it."), nil
	}
	// One key for both appends, read once: the store derives the open's and
	// the post's operations from it, and one read keeps both this call's.
	actor, refusal := t.deps.actor(ctx, turn, SendDMTool, args)
	if refusal != nil {
		return *refusal, nil
	}
	asks, refused := asksOf(SendDMTool, args, actor.Kind == chatlet.AuthorAgent)
	if refused != nil {
		return *refused, nil
	}
	again := callRepeat(turn, actor.OpKey, SendDMTool)
	said := t.deps.message(body, argStrings(args, "links"), asks)
	// The message is validated before anything is written, or a refused body
	// would leave an opened room in every participant's list. What the post
	// decides from the room stays the post's ([directPostFailure]).
	if rejected := said.Validate(); rejected != nil {
		// A refusal of content, never unknown: nothing was sent.
		return chatWriteFailure(ctx, SendDMTool, rejected, ""), nil
	}
	room, _, err := t.deps.Writer.OpenDirect(ctx, actor, to)
	if errors.Is(err, estate.ErrOutcomeUnknown) {
		// An open nobody answered: no room to post into ([directUnknown]).
		return directUnknown(to, Unanswered, "", false, again), nil
	}
	if err != nil {
		return chatWriteFailure(ctx, SendDMTool, err, ""), nil
	}
	if room.Outcome.Outcome == statelog.OutcomeUnknown {
		return directUnknown(to, unknownWhy("the open's", room.Outcome),
			room.Outcome.OpID, room.Outcome.Unvouched, again), nil
	}
	// Settle the open before posting: the post reads the room on whichever
	// copy takes it, and before that copy applied the create it would be
	// refused for a conversation that exists.
	t.deps.settle(ctx, room.Outcome.Position)

	written, err := t.deps.Writer.Post(ctx, actor, room.Channel.ID, said)
	if err != nil {
		return directPostFailure(ctx, to, room, err, again), nil
	}
	commit(turn, written.Outcome.Outcome, types.CommittedChat, chatWritten(written))
	t.deps.settle(ctx, written.Outcome.Position)
	if written.Outcome.Outcome == statelog.OutcomeUnknown {
		// The open landed and the answer says so: only the post is in doubt.
		return failedUnknown(written.Outcome.OpID, written.Outcome.Unvouched,
			directOpen(to, room)+chatUnknownText(SendDMTool+"'s message",
				written.Outcome, postNext(written.Outcome.Unvouched, again,
					"Read the conversation with "+ReadChannelTool))), nil
	}
	// Two records, one answer: as certain as the less certain, positioned at
	// the later ([statelog.LessCertain], [statelog.Later]). An open that found
	// the conversation already there answers no position.
	out := postReceipt(written, room.Channel.ID)
	out["outcome"] = string(statelog.LessCertain(room.Outcome.Outcome,
		written.Outcome.Outcome))
	out["position"] = positionOf(statelog.Later(room.Outcome.Position,
		written.Outcome.Position))
	return jsonResult(out)
}

// directOpen is how every send_dm answer about its post begins once the open
// has landed: the conversation is there, whatever became of the message.
func directOpen(to []string, room chatlet.Written) string {
	return fmt.Sprintf("The conversation with %s is open (%s). ",
		clip(strings.Join(to, ", ")), room.Channel.ID)
}

// directPostFailure is send_dm's answer when the conversation is open and the
// message was refused ([dependencyFailure]'s shape). The class is the post's,
// whose remedy is the caller's; the sentence says the room is there, since
// "nothing was written" is false about the open.
//
// An unknown post stays unknown, checked before the node's class it belongs
// to: a post to a data node that never answered may have been said, and "was
// not said" would invite a second message and drop the cause a person's
// surface reads.
func directPostFailure(ctx context.Context, to []string, room chatlet.Written, err error,
	again string) tools.Result {

	failure := chatWriteFailure(ctx, SendDMTool, err, "")
	if tools.UnknownOf(failure) {
		return tools.Result{Output: directOpen(to, room) + fmt.Sprintf("Whether the "+
			"message landed is unknown (%s). It may have been said and it may not "+
			"— do not report it as sent, and do not report it as failed. %s",
			Unanswered, postNext(false, again, "Read the conversation with "+ReadChannelTool)),
			Failed: true, Cause: failure.Cause}
	}
	switch tools.RefusalOf(failure) {
	case tools.RefusalUnavailable:
		// Replacing [chatWriteFailure]'s sentence, keep the condition's
		// next step.
		failure.Output = sentence(fmt.Sprintf("the message did not land (%s) "+
			"and was not said — do not report it as sent.", toldOf(failure, err)),
			conditionNext(err, "", stuckWrite))
	case tools.RefusalInternalError:
		failure.Output = fmt.Sprintf("the message failed: %s — do not report it as "+
			"sent, and do not send it again by other means.", faultSaid)
	}
	failure.Output = directOpen(to, room) + "But the message was NOT sent: " +
		failure.Output
	return failure
}

// ---- read_channel ------------------------------------------------------ //

type readChannel struct{ deps ChatDeps }

var _ tools.SeatCallable = (*readChannel)(nil)

func (t *readChannel) Name() string { return ReadChannelTool }

func (t *readChannel) Description() string {
	return "Read what was said in a room, newest first — or one thread in it, " +
		"oldest first, by naming the message it hangs off. A room's page holds " +
		"no replies: a message somebody replied to carries `thread` with how " +
		"many, and its replies are read by naming it as `thread`. A thread you were " +
		"woken in is handed to you at turn start under `## The thread so far`; " +
		"read it here only where that block says it is incomplete. The rest " +
		"of the room is not handed to you, so read it here when your answer " +
		"depends on what was said before. A message carrying an `event` is the " +
		"room's own change (who joined or was added, a new topic or " +
		"description), not something anybody said, and takes no reply."
}

func (t *readChannel) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"channel": map[string]any{
				"type":        "string",
				"description": "The room: its id, or its name.",
			},
			"thread": map[string]any{
				"type": "string",
				"description": "The id of the message a thread hangs off. " +
					"Omit for the room itself.",
			},
			"limit": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf("How many messages, 1..%d (default %d).",
					chatlet.MaxLimit, chatlet.DefaultLimit),
			},
			"cursor": map[string]any{
				"type": "string",
				"description": "The `next_cursor` from the previous page, to " +
					"keep reading.",
			},
		},
		"required": []any{"channel"},
	}
}

func (t *readChannel) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *readChannel) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	if t.deps.Reader == nil {
		return unconfiguredChat(ReadChannelTool), nil
	}
	actor, id, refusal := t.deps.resolved(ctx, turn, ReadChannelTool, args)
	if refusal != nil {
		return *refusal, nil
	}
	limit := argInt(args, "limit", 0)
	cursor := strings.TrimSpace(argString(args, "cursor"))

	read := newForeignRead(turn, actor.Name())
	if root := strings.TrimSpace(argString(args, "thread")); root != "" {
		got, err := t.deps.Reader.Thread(ctx, actor.Name(), chatlet.ThreadQuery{
			ChannelID: id, RootID: root, Cursor: cursor, Limit: limit,
		}, seatRead)
		if err != nil {
			return chatReadFailure(ctx, ReadChannelTool, err), nil
		}
		fenceMessage(read, fieldPath{}.key("root"), got.Root.Message)
		for i, reply := range got.Replies {
			fenceMessage(read, fieldPath{}.key("replies").index(i), reply.Message)
		}
		return read.answer(ctx, ReadChannelTool, got, "Ask for fewer messages with `limit`.")
	}
	got, err := t.deps.Reader.Messages(ctx, actor.Name(), chatlet.TranscriptQuery{
		ChannelID: id, Cursor: cursor, Limit: limit,
	}, seatRead)
	if err != nil {
		return chatReadFailure(ctx, ReadChannelTool, err), nil
	}
	for i, view := range got.Messages {
		fenceMessage(read, fieldPath{}.key("messages").index(i), view.Message)
	}
	return read.answer(ctx, ReadChannelTool, got, "Ask for fewer messages with `limit`.")
}

// fenceMessage fences what one message says at path, under its author and the
// kind the transcript records, taken at its word. A tombstone carries no body
// and is not fenced. A room event's
// words (a topic, a description, a template's name) are fenced under its
// author as a room's label.
func fenceMessage(read *foreignRead, path fieldPath, m chatlet.Message) {
	writers := []provenance.Author{read.writer(m.Author, iam.ActorKind(m.AuthorKind))}
	read.fence(path.key("message").key("body"), provenance.SourceChatThread,
		writers, false, m.Body)
	if m.Event != nil {
		read.fence(path.key("message").key("event").key("text"),
			provenance.SourceChatRoom, writers, false, m.Event.Text)
	}
}

// ---- search_messages --------------------------------------------------- //

type searchMessages struct{ deps ChatDeps }

var _ tools.SeatCallable = (*searchMessages)(nil)

func (t *searchMessages) Name() string { return SearchMessagesTool }

func (t *searchMessages) Description() string {
	return "Find a message by what it SAYS, ranked across every room you can " +
		"read. Use it when you remember that something was discussed but not " +
		"where: \"the decision about the retry backoff\". " + ReadChannelTool + " is the " +
		"other half — it reads one room in order, from a point you name."
}

func (t *searchMessages) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{
				"type": "string",
				"description": fmt.Sprintf("What was said, in plain words — at "+
					"most %d bytes. Not a query language.", knowledge.MaxQueryBytes),
			},
			"channel": map[string]any{
				"type":        "string",
				"description": "Narrow to one room. Omit for every room you can read.",
			},
			"author": map[string]any{
				"type":        "string",
				"description": "Narrow to one speaker's handle.",
			},
			"limit": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf("How many messages, 1..%d (default %d).",
					search.MaxChatSearchLimit, search.ChatSearchLimit),
			},
		},
		"required": []any{"text"},
	}
}

func (t *searchMessages) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *searchMessages) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	if t.deps.Search == nil {
		return unconfiguredChat(SearchMessagesTool), nil
	}
	text := strings.TrimSpace(argString(args, "text"))
	if text == "" {
		return failed(SearchMessagesTool + " needs a `text` — what was said, in " +
			"plain words."), nil
	}
	if knowledge.CheckQuery(text) != nil {
		// Refused, not cut ([knowledge.MaxQueryBytes]), before the router:
		// the index's refusal arrives as words alone and reads as chat
		// being unreadable.
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return failed(fmt.Sprintf(SearchMessagesTool+"' `text` is %d bytes and takes at "+
			"most %d — search on a few words somebody would have said, not a "+
			"pasted passage.", len(text), knowledge.MaxQueryBytes)), nil
	}
	actor, refusal := t.deps.actor(ctx, turn, SearchMessagesTool, args)
	if refusal != nil {
		return *refusal, nil
	}
	// A named room that resolves to nothing is refused, not dropped: every
	// room's hits would read as the one meant. An id is never refused here;
	// the search intersects it with what the viewer may read
	// ([estate.ChatQuery.Channel]).
	room := ""
	if named := strings.TrimSpace(argString(args, "channel")); named != "" {
		room, refusal = t.deps.channelID(ctx, SearchMessagesTool, actor.Name(), named)
		if refusal != nil {
			return *refusal, nil
		}
	}
	answer, err := t.deps.Search.Search(ctx, estate.ChatQuery{
		Viewer:  actor.Name(),
		Text:    text,
		Channel: room,
		Author:  strings.TrimSpace(argString(args, "author")),
		Limit:   argInt(args, "limit", 0),
		Fresh:   seatRead,
	})
	if err != nil {
		return chatReadFailure(ctx, SearchMessagesTool, err), nil
	}
	if len(answer.Hits) == 0 {
		if answer.Building {
			// Not "nothing matched": the index is still catching up, and a
			// seat told nobody said it acts on that.
			return tools.Result{Output: "Nothing matched yet — but the chat " +
				"index is still being built, so a message that says this may " +
				"not be indexed yet. Read the room with " + ReadChannelTool + " if you " +
				"know where it was said, or search again shortly."}, nil
		}
		return tools.Result{Output: "Nothing in the rooms you can read matches that."}, nil
	}
	read := newForeignRead(turn, actor.Name())
	rows := make([]chatHitRow, 0, len(answer.Hits))
	for i, hit := range answer.Hits {
		rows = append(rows, chatHitRow{
			MessageID: hit.MessageID, ChannelID: hit.ChannelID, ThreadRoot: hit.ThreadRoot,
			Author: hit.Author, Snippet: hit.Snippet, At: hit.CreatedAt, Score: hit.Score,
		})
		// A snippet is the message's words, fenced under its recorded writer.
		read.fence(fieldPath{}.key("messages").index(i).key("snippet"),
			provenance.SourceChatThread,
			[]provenance.Author{read.writer(hit.Author, iam.ActorKind(hit.AuthorKind))},
			false, hit.Snippet)
	}
	out := map[string]any{"count": len(rows), "messages": rows}
	if answer.Building {
		// Said beside the hits too: they are what the index had reached.
		out["building"] = true
	}
	return read.answer(ctx, SearchMessagesTool, out,
		"Narrow it with `channel`, `author` or a shorter `limit`.")
}

// chatHitRow is one search hit as this tool renders it: a projection with the
// transcript's keys (`message_id`, `channel_id`, `thread_root`) and the API
// search's `at`, since the index's value carries no JSON names and a seat
// would meet the id under a key no other tool spells.
type chatHitRow struct {
	MessageID  string    `json:"message_id"`
	ChannelID  string    `json:"channel_id"`
	ThreadRoot string    `json:"thread_root,omitempty"`
	Author     string    `json:"author,omitempty"`
	Snippet    string    `json:"snippet" untrusted:"fence"`
	At         time.Time `json:"at"`
	Score      float64   `json:"score"`
}

// ---- list_channels ----------------------------------------------------- //

type listChannels struct{ deps ChatDeps }

var _ tools.SeatCallable = (*listChannels)(nil)

func (t *listChannels) Name() string { return ListChannelsTool }

func (t *listChannels) Description() string {
	return "List the rooms you are in, most recently active first, with what " +
		"each is for and the last thing said in it. Any room you may read is " +
		"also reached by its name, joined or not."
}

func (t *listChannels) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"include_archived": map[string]any{
				"type": "boolean",
				"description": "Include rooms that are closed to new messages. " +
					"They are readable for ever; nothing more can be said in them.",
			},
		},
	}
}

// channelRow is one room as this tool renders it: a projection so the unread
// count, a person's attention, appears only with a read state
// ([ChatDeps.ReadState]); a seat told "99+ unread" chases a backlog that is
// not its work.
type channelRow struct {
	ID   string       `json:"id"`
	Name string       `json:"name,omitempty"`
	Kind chatlet.Kind `json:"kind"`

	Topic   string `json:"topic,omitempty" untrusted:"fence"`
	Purpose string `json:"purpose,omitempty" untrusted:"fence"`
	Unit    string `json:"unit,omitempty"`

	Archived bool `json:"archived,omitempty"`

	// Participants are the handles in a direct conversation, which has no
	// name to render.
	Participants []string `json:"participants,omitempty"`

	// FollowAll says every message here reaches this seat, whether or not
	// it is named — which is what a unit's own room does for the seats in
	// that unit.
	FollowAll bool `json:"follow_all,omitempty"`

	// Muted says this reader silenced the room, as the rail marks it: no
	// badge, no push. It suppresses notice, never delivery: the backlog is
	// readable but not something the person is waiting on.
	Muted bool `json:"muted,omitempty"`

	// Unread is nil without a read state: "nothing new" and "nobody tracks
	// where you got to" are answers a reader acts on differently.
	Unread       *int `json:"unread,omitempty"`
	UnreadCapped bool `json:"unread_capped,omitempty"`

	Last *chatlet.MessagePreview `json:"last,omitempty"`
}

func (t *listChannels) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *listChannels) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	if t.deps.Reader == nil {
		return unconfiguredChat(ListChannelsTool), nil
	}
	actor, refusal := t.deps.actor(ctx, turn, ListChannelsTool, args)
	if refusal != nil {
		return *refusal, nil
	}
	q := chatlet.ChannelsQuery{IncludeArchived: argBool(args, "include_archived")}
	// Counted only against a read state that was read ([ChatDeps.ReadState]):
	// a failed read costs the counts, never the listing.
	var (
		counted    bool
		unreadable error
		state      coord.ChatReadState
	)
	if t.deps.ReadState != nil {
		read, readErr := t.deps.ReadState(ctx, actor.Name())
		if readErr != nil {
			unreadable = readErr
			log.WarnContext(ctx, "chat_read_state_unreadable",
				"handle", actor.Name(), "error", readErr.Error())
		} else {
			state, counted = read, true
			// The cursors alone: this list counts unread room posts and
			// asks for nothing waiting ([chatlet.ChannelsQuery.Waiting]).
			q.Marks = chatlet.ReaderMarks{Known: true, Cursors: state.Cursors}
		}
	}
	muted := make(map[string]bool, len(state.Muted))
	for _, id := range state.Muted {
		muted[id] = true
	}
	listing, err := t.deps.Reader.Channels(ctx, actor.Name(), q, seatRead)
	if err != nil {
		return chatReadFailure(ctx, ListChannelsTool, err), nil
	}
	if len(listing.Channels) == 0 {
		return tools.Result{Output: "You are in no chat rooms."}, nil
	}
	read := newForeignRead(turn, actor.Name())
	rows := make([]channelRow, 0, len(listing.Channels))
	for i, room := range listing.Channels {
		at := fieldPath{}.key("channels").index(i)
		// A topic and a purpose are somebody's words with no recorded
		// author: fenced as a room's label by unnamed colleagues (only the
		// company's principals set one), never this seat's own.
		read.fence(at.key("topic"), provenance.SourceChatRoom, nil, true, room.Channel.Topic)
		read.fence(at.key("purpose"), provenance.SourceChatRoom, nil, true, room.Channel.Purpose)
		if last := room.Last; last != nil {
			read.fence(at.key("last").key("excerpt"), provenance.SourceChatThread,
				[]provenance.Author{read.writer(last.Author, iam.ActorKind(last.AuthorKind))},
				false, last.Excerpt)
		}
		row := channelRow{
			ID: room.Channel.ID, Name: room.Channel.Name, Kind: room.Channel.Kind,
			Topic: room.Channel.Topic, Purpose: room.Channel.Purpose,
			Unit: room.Channel.Unit, Archived: room.Channel.ArchivedAt != nil,
			Participants: room.Participants, FollowAll: room.FollowAll,
			Muted: muted[room.Channel.ID], Last: room.Last,
		}
		if counted {
			unread := room.Unread
			row.Unread, row.UnreadCapped = &unread, room.UnreadCapped
		}
		rows = append(rows, row)
	}
	out := map[string]any{"count": len(rows), "channels": rows}
	if unreadable != nil {
		// Said, since no counts and nothing unread are different facts; in
		// fixed words, because the store's error names its transport.
		why := "the coordination store could not be read"
		if words, ok := Condition(unreadable); ok {
			why = words
		}
		out["read_state"] = fmt.Sprintf("unknown: how far you have read could "+
			"not be fetched (%s), so no room carries a count of what is new, "+
			"and none is marked muted", why)
	}
	if counted && state.DNDUntil.After(t.deps.now()) {
		// Do-not-disturb as an instant, so a reader can tell when it ends;
		// one already past is not reported.
		out["dnd_until"] = state.DNDUntil
	}
	// What the answer could not account for: a model reading a short list
	// as the whole truth posts in the wrong room.
	if !listing.Complete {
		out["complete"] = false
	}
	if listing.Truncated {
		out["truncated"] = true
	}
	return read.answer(ctx, ListChannelsTool, out,
		"There is no narrower listing — read one room with "+ReadChannelTool+".")
}

// ---- react_to_message -------------------------------------------------- //

type reactToMessage struct{ deps ChatDeps }

var _ tools.SeatCallable = (*reactToMessage)(nil)

func (t *reactToMessage) Name() string { return ReactToMessageTool }

func (t *reactToMessage) Description() string {
	return "Put one emoji on a message, or take yours back with `remove`. It " +
		"is an acknowledgement and NOT an answer: nobody is woken by it, and " +
		"a question asked of you is still unanswered after it. If you have " +
		"something to say, reply."
}

func (t *reactToMessage) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"channel": map[string]any{
				"type":        "string",
				"description": "The room: its id, or its name.",
			},
			"message_id": map[string]any{
				"type":        "string",
				"description": "The id of the message to react to.",
			},
			"emoji": map[string]any{
				"type":        "string",
				"description": "A shortcode, e.g. \":eyes:\" or \":+1:\".",
			},
			"remove": map[string]any{
				"type":        "boolean",
				"description": "True to take your own reaction back.",
			},
		},
		"required": []any{"channel", "message_id", "emoji"},
	}
}

func (t *reactToMessage) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *reactToMessage) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	if t.deps.Writer == nil {
		return unconfiguredChat(ReactToMessageTool), nil
	}
	messageID := strings.TrimSpace(argString(args, "message_id"))
	emoji := strings.TrimSpace(argString(args, "emoji"))
	switch {
	case messageID == "":
		return failed(ReactToMessageTool + " needs a `message_id` — the id of the " +
			"message you are reacting to."), nil
	case emoji == "":
		return failed(ReactToMessageTool + " needs an `emoji`."), nil
	}
	actor, id, refusal := t.deps.resolved(ctx, turn, ReactToMessageTool, args)
	if refusal != nil {
		return *refusal, nil
	}
	remove := argBool(args, "remove")
	react := t.deps.Writer.React
	if remove {
		react = t.deps.Writer.Unreact
	}
	const look = "Read the message's reactions with " + ReadChannelTool
	written, err := react(ctx, actor, id, messageID, emoji)
	if err != nil {
		return chatWriteFailure(ctx, ReactToMessageTool, err, restateNext(look)), nil
	}
	t.deps.settle(ctx, written.Outcome.Position)
	return stated(ReactToMessageTool, written, look, map[string]any{
		"channel": id, "message_id": messageID, "emoji": emoji,
		"removed": remove,
	})
}

// ---- join_channel / leave_channel -------------------------------------- //

// membershipLook is the read that shows whether a join or a leave landed.
const membershipLook = "Check " + ListChannelsTool + ", which names every room you are in"

type joinChannel struct{ deps ChatDeps }

var _ tools.SeatCallable = (*joinChannel)(nil)

func (t *joinChannel) Name() string { return JoinChannelTool }

func (t *joinChannel) Description() string {
	return "Join a room, so that what is said there reaches you. Only yours " +
		"to change: this adds you and nobody else. A private room is not " +
		"joinable — somebody already in it adds you — and neither is a " +
		"unit's own room: its membership is the org chart's, and the next " +
		"apply would take you back out."
}

func (t *joinChannel) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"channel": map[string]any{
				"type":        "string",
				"description": "The room: its id, or its name.",
			},
		},
		"required": []any{"channel"},
	}
}

func (t *joinChannel) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *joinChannel) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	if t.deps.Writer == nil {
		return unconfiguredChat(JoinChannelTool), nil
	}
	actor, id, refusal := t.deps.resolved(ctx, turn, JoinChannelTool, args)
	if refusal != nil {
		return *refusal, nil
	}
	written, err := t.deps.Writer.Join(ctx, actor, id)
	if err != nil {
		return chatWriteFailure(ctx, JoinChannelTool, err, restateNext(membershipLook)), nil
	}
	t.deps.settle(ctx, written.Outcome.Position)
	return stated(JoinChannelTool, written, membershipLook, map[string]any{
		"channel": id, "name": written.Channel.Name, "joined": true,
	})
}

type leaveChannel struct{ deps ChatDeps }

var _ tools.SeatCallable = (*leaveChannel)(nil)

func (t *leaveChannel) Name() string { return LeaveChannelTool }

func (t *leaveChannel) Description() string {
	return "Leave a room, so that what is said there stops reaching you. Only " +
		"yours to change. A unit's own room cannot be left — its membership " +
		"is the org chart's, and the next apply would put you back."
}

func (t *leaveChannel) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"channel": map[string]any{
				"type":        "string",
				"description": "The room: its id, or its name.",
			},
		},
		"required": []any{"channel"},
	}
}

func (t *leaveChannel) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *leaveChannel) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	if t.deps.Writer == nil {
		return unconfiguredChat(LeaveChannelTool), nil
	}
	actor, id, refusal := t.deps.resolved(ctx, turn, LeaveChannelTool, args)
	if refusal != nil {
		return *refusal, nil
	}
	written, err := t.deps.Writer.Leave(ctx, actor, id)
	if err != nil {
		return chatWriteFailure(ctx, LeaveChannelTool, err, restateNext(membershipLook)), nil
	}
	t.deps.settle(ctx, written.Outcome.Position)
	return stated(LeaveChannelTool, written, membershipLook, map[string]any{
		"channel": id, "name": written.Channel.Name, "left": true,
	})
}
