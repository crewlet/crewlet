package builtin

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// The native chat's tool names, re-exported from the package that owns the
// vocabulary.
//
// DECLARED IN internal/chat rather than here, for the reason the tracker's
// are: that package's own notification prompt NAMES them — it tells a woken
// seat which tool to reach for — and a constant defined here and read there
// is an import cycle. The domain owns its vocabulary; this package implements
// it.
const (
	PostMessageTool    = chat.PostMessageTool
	ReplyInThreadTool  = chat.ReplyInThreadTool
	SendDMTool         = chat.SendDMTool
	ReadChannelTool    = chat.ReadChannelTool
	SearchMessagesTool = chat.SearchMessagesTool
	ListChannelsTool   = chat.ListChannelsTool
	ReactToMessageTool = chat.ReactToMessageTool
	JoinChannelTool    = chat.JoinChannelTool
	LeaveChannelTool   = chat.LeaveChannelTool
)

// ChatWrites are the three that count as a DELIVERY.
//
// SAYING SOMETHING IS THE ANSWER on this surface — in the thread, in the room
// or privately — and without this the delivery gate sees only builtins,
// concludes the turn reached nobody, and corrects it into another round for
// having done exactly what it was woken to do. See [chat.WriteTools] for why
// a reaction and a membership change are not here.
func ChatWrites() []string { return chat.WriteTools() }

// ChatReader is what these tools need from chat's read side.
//
// THE LEVEL IS PART OF THE CALL, on [PageReader]'s reasoning: a seat reads at
// [seatReadLevel] — `linearizable` — because it must see its own writes, and
// that is what stops a seat that has just posted from reading the room, not
// finding its message and posting again.
type ChatReader interface {
	Channels(ctx context.Context, viewer string, q chat.ChannelsQuery,
		fresh statelog.Freshness) (chat.ChannelListing, error)
	Messages(ctx context.Context, viewer string, q chat.TranscriptQuery,
		fresh statelog.Freshness) (chat.Transcript, error)
	Thread(ctx context.Context, viewer string, q chat.ThreadQuery,
		fresh statelog.Freshness) (chat.Thread, error)
	// ChannelNamed resolves a room's NAME — its address — for a viewer,
	// answering not found for a room that viewer may not read. See
	// [ChatDeps.channelID].
	ChannelNamed(ctx context.Context, viewer, name string,
		fresh statelog.Freshness) (chat.ChannelDetail, error)
}

// ChatWriter is what these tools need from chat's write side.
//
// THE ACTOR IS A PARAMETER rather than the seam being resolved per actor, and
// that is the domain's own shape rather than a choice made here: one node's
// chat serves every seat and every operator through one write path, so the
// party a write acts as travels per call. It is the wiki's arrangement, not
// the tracker's — see [PageWriter].
//
// AND SO IS THE KEY ([statelog.CallKey]) every gesture's operation is derived
// from — on the message's own input where the gesture has one — for the same
// reason: it is the call's, not the writer's. See [ChatDeps.callKey].
type ChatWriter interface {
	Post(ctx context.Context, actor chat.Actor, channelID string,
		in chat.NewMessage) (chat.Written, error)
	Reply(ctx context.Context, actor chat.Actor, channelID, replyTo string,
		in chat.NewMessage) (chat.Written, error)
	// OpenDirect reports whether it CREATED the conversation beside the
	// room itself, because "it was already open" is not a failure here —
	// the id is derived from the participants, so the loser of a race is
	// handed the room the winner made.
	OpenDirect(ctx context.Context, actor chat.Actor, participants []string,
		key statelog.CallKey) (chat.Written, bool, error)
	React(ctx context.Context, actor chat.Actor, channelID, messageID,
		emoji string, key statelog.CallKey) (chat.Written, error)
	Unreact(ctx context.Context, actor chat.Actor, channelID, messageID,
		emoji string, key statelog.CallKey) (chat.Written, error)
	Join(ctx context.Context, actor chat.Actor, channelID string,
		key statelog.CallKey) (chat.Written, error)
	Leave(ctx context.Context, actor chat.Actor, channelID string,
		key statelog.CallKey) (chat.Written, error)

	// The gestures only a person makes — see chatoperator.go. They are on
	// the one writer rather than a second seam, because they are the same
	// store's and a seat's registry simply never offers them.
	CreateChannel(ctx context.Context, actor chat.Actor,
		in chat.NewChannel) (chat.Written, error)
	PatchChannel(ctx context.Context, actor chat.Actor, channelID string,
		patch chat.ChannelPatch, key statelog.CallKey) (chat.Written, error)
	SetMembers(ctx context.Context, actor chat.Actor, channelID string,
		members []chat.Member, key statelog.CallKey) (chat.Written, error)
	Edit(ctx context.Context, actor chat.Actor, channelID, messageID string,
		in chat.EditMessage) (chat.Written, error)
	Delete(ctx context.Context, actor chat.Actor, channelID, messageID string,
		key statelog.CallKey) (chat.Written, error)
}

// ChatCursors is where a person's read state is written — the one chat write
// that is not a record on the log. See [ChatDeps.Cursors].
type ChatCursors interface {
	AdvanceChatRead(ctx context.Context, handle string,
		delta coord.ChatReadDelta) (coord.ChatReadState, error)
}

// ChatSearch is one ranked search over the company's chat, as this tool asks
// it.
//
// THE CALLER'S OWN SHAPE rather than [search.ChatQuery], for one field:
// that type carries the VISIBLE CHANNEL SET, which is an authorization fact
// the implementation resolves from the viewer and which a tool must never be
// able to supply. Passing the domain's query with that field left empty would
// be a field with two meanings — "everything" and "the caller forgot" —
// which differ by the whole transcript.
type ChatSearch struct {
	// Text is what somebody typed.
	Text string

	// Channel narrows to one room, empty for every room the viewer may
	// read. It is INTERSECTED with the visible set rather than trusted, so
	// naming a private room somebody is not in answers nothing rather than
	// refusing — a refusal would confirm the room exists.
	Channel string

	// Author narrows to one speaker, empty for anybody.
	Author string

	// Limit bounds the answer; zero takes the searcher's own default.
	Limit int
}

// ChatSearcher ranks a company's chat FOR ONE VIEWER.
//
// Declared by the consumer and kept to the one method these tools call. The
// VIEWER is an argument and the visible channel set is not: resolving which
// rooms somebody may read is the seam's job, because it is the half that
// holds both the membership rows and the index — and a tool that passed the
// set would be a tool that could pass the wrong one.
//
// Nil on a build with no chat index, and the tool is then OMITTED rather than
// refusing at the call, on [Register]'s own rule.
type ChatSearcher interface {
	SearchMessages(ctx context.Context, viewer string, q ChatSearch) ([]search.ChatHit, error)
}

// ChatDeps are chat's halves plus what a write needs.
type ChatDeps struct {
	Reader ChatReader
	Writer ChatWriter

	// Search ranks messages by what they say. A VALUE rather than a
	// function of the actor, unlike the writer: a search reads, and what
	// may be read is decided from the viewer it is handed.
	Search ChatSearcher

	// Mentions resolves the handles a message names, so an @-mention wakes
	// the person the author meant. Nil resolves nothing, which degrades to
	// a message that reaches the room's own subscribers and nobody
	// specifically.
	Mentions MentionResolver

	// ReadState is this viewer's read state: how far they have read in
	// each room ([coord.ChatReadState.Cursors], keyed on channel id against
	// a packed position — what [chat.ChannelsQuery.Cursors] counts above),
	// which rooms they have muted, and when their do-not-disturb ends.
	//
	// THE WHOLE STATE, not the cursors alone, because `list_channels` is
	// what a person's own assistant reads the dashboard's rail through: a
	// room the person muted raises no badge there, and handed over as
	// ordinary unread here it is a backlog the assistant offers to work
	// through that the person deliberately silenced.
	//
	// NIL IS THE ORDINARY CASE AND IT IS NOT AN ABSENCE OF DATA. A read
	// cursor is a PERSON's attention, written when somebody looks at a
	// room; a seat is woken rather than browsing and has none. With this
	// nil the listing carries no unread counts at all, which is the honest
	// answer — the alternative is reporting every room's whole tail as
	// unread, and a model told it has ninety-nine unread messages in a
	// room it was never reading will try to catch up on them. The
	// operator's surface sets it, because its caller is a person.
	//
	// THREE ANSWERS, NOT TWO: cursors (none at all is somebody who has
	// opened nothing yet, and every room's tail IS unread to them), or an
	// error. An error is not a cursor at zero — counted from zero, every
	// room in the listing would read as unread to a person who has read
	// all of it — so a listing whose read state could not be read carries
	// no counts and says why, as the dashboard's own rail does.
	ReadState func(ctx context.Context, viewer string) (coord.ChatReadState, error)

	// Cursors WRITES a person's read state — how far they read in each
	// room, what they muted, whether they are in do-not-disturb — and is
	// what `mark_chat_read` is served over. Nil omits that tool, which is
	// every seat's surface: a cursor is a person's attention, and a seat
	// has none.
	//
	// THE IMPLEMENTATION ALSO TELLS THE PERSON'S OTHER SCREENS. A cursor is
	// not on the log, so no applier will ever announce it, and the only
	// reader that wants it is the same person's other tabs — a badge cleared
	// in one window has to clear in the other. The tool knows no socket, so
	// the push is the seam's, beside the write it follows.
	//
	// ITS ERROR IS THE COORDINATION STORE'S, RETURNED AS IT IS, and it has
	// three answers the tool tells apart by sentinel alone: a flush the
	// merge refused ([coord.ErrInvalidReadState], the caller's to change,
	// said to the person in the merge's own words through
	// [coord.ReadStateSentence]); a flush sent whose answer was lost
	// ([coord.ErrWriteUnknown], which may have landed, answered `unknown`
	// and safe to repeat, because the merge is idempotent); and any other
	// error, a store that could not take it and changed nothing (this
	// node's, worth asking again). Re-worded, a sentinel is lost — a
	// refusal that can never land reads "try again", and a flush that may
	// have landed reads "nothing was recorded"; wrapped, even with %w, the
	// wrap's words reach the person ahead of the merge's.
	Cursors ChatCursors

	// Now stamps a read-state write ([coord.ChatReadDelta.At]). Nil is the
	// wall clock, in UTC.
	Now func() time.Time

	// Actor decides who a write is attributed to, and — because a private
	// room's contents are decided by its membership — who a READ is served
	// as. Nil takes the turn's seat; the operator surface sets it. See
	// [WorkDeps.Actor] for why this is a seam rather than a second copy of
	// these tools.
	Actor func(ctx context.Context, turn *turnctx.Turn) (chat.Actor, error)

	// Operation reads the operation a transport named for this call off a
	// request made outside a turn, or "" — [PageDeps.Operation]'s seam, on
	// the chat tools for its reasons: [chat.Actor] is who spoke, and an
	// operation is not attribution. Nil, which is every seat's surface,
	// reads "".
	//
	// What it buys is that a person's retried message is ONE message: every
	// chat write's operation — a post's, which is also the message's id, a
	// direct conversation's open, a reaction's, a join's — is derived from
	// it exactly as from a turn's key ([ChatDeps.callKey]). The operator's
	// act transport sets it from the request ([RequestOperation]); these
	// tools take no `op_id`, so over MCP each chat write is a new one.
	Operation func(ctx context.Context) string

	// Await is handed the position every chat write landed at, so the next
	// read sees it. See [WorkDeps.Await]: same seam, same reason, and the
	// same floor the wait happens behind — every surface, the operator's
	// included, hands the position to the node's session floor, which every
	// later read and write carries to whichever copy answers it. On this
	// surface it is the difference between a seat seeing its own message
	// and posting it twice.
	Await func(ctx context.Context, at statelog.Position) error
}

// settle hands a chat write's position to [ChatDeps.Await], so the next read
// sees it.
//
// APPLIED AFTER EVERY WRITE AND BEFORE NO READ. A write lands on the fleet's
// log while a read is answered from some copy's rows, so a seat that posts and
// then reads the room in the same turn must be made to see its own message — a
// model that cannot find what it just said says it again. Before a read it
// would buy nothing: there is no position to wait for that the caller did not
// already produce.
//
// Best effort, and a failure — which only a surface that waits here can have
// — is LOGGED AND IGNORED: the message landed, and telling a model its post
// failed when it is in the room is the one answer that produces the
// duplicate.
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
// arguments: a model that could name its own author could speak as anybody,
// and a room where that is possible is not a conversation.
//
// THE TURN ID IS THE RUN, which is provenance (ADR-0017): an audit walks from
// a message to the execution that said it. It derives nothing. What makes a
// re-run turn post once is the call's key ([ChatDeps.callKey]), whose seed is
// the unit of work a redelivery reproduces — and that is not attribution,
// which is why it travels beside the actor rather than on it, as the wiki's
// does ([pageActor]).
func chatActor(turn *turnctx.Turn) (chat.Actor, error) {
	seat, err := turn.RequireSeat()
	if err != nil {
		return chat.Actor{}, err
	}
	return chat.Actor{
		Handle: seat.Handle(), Kind: chat.AuthorAgent,
		TurnID: turn.RunID, Chain: turn.Chain,
	}, nil
}

// actor resolves who this call acts as — see [ChatDeps.Actor].
func (d ChatDeps) actor(ctx context.Context, turn *turnctx.Turn) (chat.Actor, error) {
	if d.Actor != nil {
		return d.Actor(ctx, turn)
	}
	return chatActor(turn)
}

// callKey is the key every chat write this call makes is derived from —
// [callKeyOf], with chat's policy:
//
//   - THE TOOL IS PART OF THE KEY in every case, because the repeat count is
//     kept per tool and two of these tools make one gesture — see
//     [statelog.CallKey] and the chat package's ids.go;
//   - and the count is the FAMILY's rather than the tool's
//     ([chat.CallFamily]), because two of these tools UNDO each other:
//     join_channel and leave_channel write one membership, and a count blind
//     to the leave between two joins handed the second join the first one's
//     operation and its success.
//
// So a turn that says two things writes two messages, a remark made again
// after a different one is a third, a join after a leave is a join whatever
// the room was called in each, and a re-run that makes the same calls writes
// each of them once.
func (d ChatDeps) callKey(ctx context.Context, turn *turnctx.Turn, tool string,
	args map[string]any) statelog.CallKey {

	return callKeyOf(ctx, turn, d.Operation, tool, chat.CallFamily(tool), args, true)
}

// chatActorFailure is what a call with no author reports.
//
// THE SEAM'S OWN REFUSAL IS PASSED THROUGH, unlike the wiki's and the
// tracker's, because on this surface it carries the remedy: an operator token
// bound to no seat is refused naming `contact.crewlet_operator_id`, and
// flattening that into "only during a turn" would send a person looking for a
// turn they do not have. A seat with no turn still gets the ordinary
// sentence, which is the only thing that failure means there.
//
// [tools.RefusalForbidden] either way: nothing about the arguments is wrong,
// and this caller — no seat, or a credential bound to none or to an agent —
// may not speak or read as anybody here. The forbidden class is one a
// person's surface prints as the tool wrote it, which is what carries the
// binding's name to the person who has to add it.
func chatActorFailure(name string, err error) tools.Result {
	if errors.Is(err, turnctx.ErrNoSeat) {
		return notInATurn(name)
	}
	return refused(tools.RefusalForbidden, name+" has nobody to act as: "+err.Error())
}

// unconfiguredChat is the refusal when the company runs no native chat.
//
// [tools.RefusalUnavailable], on [unconfigured]'s reasoning: the call is well
// formed, and the room may well exist in the chat the company DOES run.
func unconfiguredChat(name string) tools.Result {
	return refused(tools.RefusalUnavailable, name+" is unavailable: this "+
		"company does not run native chat. Use the chat tools your company has "+
		"configured.")
}

// chatWriteFailure explains a write that did not land, in terms the model can
// act on, classed for a reader that is not a model.
//
// EACH SENTINEL HAS ITS OWN CLASS, for [writeFailure]'s reason: the class is
// what a person's surface acts on, and a lost race, a name somebody else
// holds, a permission, a room this node cannot serve until it is upgraded and
// a log that refused the append each ask a different thing of the caller.
//
// THE UNMARKED REMAINDER IS [tools.RefusalUnavailable], as the wiki's is, and
// for the same reason: the chat store wraps every refusal it decides on what
// was asked in one of its own sentinels, so what reaches the end of this
// switch is a read, an encode or a ledger that failed, or a log that refused
// the append — this node's condition, never the caller's input.
//
// THE TWO CLASSES A PERSON IS SHOWN AS WRITTEN — invalid and forbidden —
// carry the store's own sentence ([chat.Sentence]) without the package's Go
// prefix, and nothing written for a model that a person could not act on.
//
// `next` is what this caller does about an UNKNOWN outcome — the sentence the
// tool's own unknown answer gives, for the same call: [unknownNext] for a
// tool that adds, [restateNext] for one that states. See the first arm.
func chatWriteFailure(name string, err error, next string) tools.Result {
	switch {
	case errors.Is(err, estate.ErrOutcomeUnknown):
		// SENT AND NEVER ANSWERED, which is not the same as not made — so
		// never the remainder's "nothing was written". TWO CLASSES REACH IT
		// ([estate.ErrOutcomeUnknown]), and they differ in what makes a
		// repeat safe: a write carrying no key of its caller's (an
		// operator's assistant over MCP) is reported at the first data node
		// that did not answer, and a repeat is a new write; a KEYED one — a
		// turn's call, a person's request — is promoted by the router and
		// asked of every other data node first, and reported only when none
		// answered, where the same call or the same request IS the safe
		// retry. So what to do next is the caller's, and it is exactly what
		// the tool's own unknown answer says for this call: one call that
		// can come back unknown two ways must not give two answers.
		return unknownOutcome(fmt.Sprintf("%s: whether this landed is unknown "+
			"(%v). It may have landed and it may not — do not report it as "+
			"done, and do not report it as failed. %s", name, err, next))
	case errors.Is(err, chat.ErrInvalid):
		return refused(tools.RefusalInvalid, fmt.Sprintf("%s refused that: %s",
			name, chat.Sentence(err)))
	case errors.Is(err, chat.ErrArchived):
		// AN ARGUMENT TO CHANGE, not a permission: the room takes nothing
		// more from anybody, and what fixes the call is saying it in a room
		// that is still open.
		return refused(tools.RefusalInvalid, fmt.Sprintf("%s refused that: %s. "+
			"Nothing more can be said there — say it somewhere that is still "+
			"open, or say nothing.", name, chat.Sentence(err)))
	case errors.Is(err, chat.ErrForbidden):
		return refused(tools.RefusalForbidden, fmt.Sprintf("%s was refused: %s. "+
			"This is a permission, not a retry: asking again will be refused "+
			"the same way.", name, chat.Sentence(err)))
	case errors.Is(err, chat.ErrUnknownKind):
		// THIS NODE'S CONDITION, NOT A PERMISSION: a newer peer made the
		// room and this build cannot say who may reach it. The same call
		// lands once this node is upgraded, so the forbidden class — "asking
		// again will be refused the same way" — would be false the moment
		// that happens. The sentence names the upgrade, which is the remedy.
		return refused(tools.RefusalUnavailable, fmt.Sprintf("%s could not be "+
			"served on this node: %s. Nothing was written.", name, chat.Sentence(err)))
	case errors.Is(err, chat.ErrNameTaken):
		return refused(tools.RefusalExists, fmt.Sprintf("%s: %v. A room's name "+
			"is its address and that one is held — open the room that has it "+
			"(list_channels names the ones you are in), or choose another name.",
			name, err))
	case errors.Is(err, chat.ErrRoomExists):
		return refused(tools.RefusalExists, fmt.Sprintf("%s: %v.", name, err))
	case errors.Is(err, chat.ErrConflict):
		return refused(tools.RefusalConflict, fmt.Sprintf("%s could not land: "+
			"%v. The room kept changing under this write — read it again "+
			"before retrying.", name, err))
	case errors.Is(err, chat.ErrNotFound):
		// A ROOM OR A MESSAGE THAT IS NOT THERE, and the advice names where
		// each is looked up. Either address reaches any room the caller may
		// read, joined or not ([ChatDeps.channelID]), so neither is offered
		// as the cure for the other: a seat told a name will not do goes
		// looking for an id it never needed.
		return refused(tools.RefusalNotFound, fmt.Sprintf("%s: %v\n\nCheck what "+
			"you named: list_channels names the rooms you are in, any room you "+
			"may read answers to its id or to its name, and read_channel shows "+
			"each message's id.", name, err))
	}
	return refused(tools.RefusalUnavailable, fmt.Sprintf("%s did not land (%v): "+
		"nothing was written — do not report it as done.", name, err))
}

// chatUnknown explains a chat write whose outcome is unknown: it may have
// landed and it may not, so it is reported as neither — [pageUnknown]'s
// sentence, for this surface.
//
// AN UNVOUCHED ONE SAYS SO, because it sends the caller somewhere else: this
// node's operation ledger may have lost the record of the operation, so the
// same operation asked here answers the same way every time. What a repeat IS
// is `next`'s to say.
func chatUnknown(name string, result statelog.Result, next string) string {
	why := "the write's acknowledgement was lost"
	if result.Unvouched {
		why = "this node's operation ledger may have lost the record of it, so " +
			"this node cannot tell"
	}
	return fmt.Sprintf("%s: whether this landed is unknown (%s; operation %s). "+
		"It may have landed and it may not — do not report it as done, and do "+
		"not report it as failed. %s", name, why, result.OpID, next)
}

// stated is the receipt a write that STATES something answers with — a
// reaction on or off, this seat in a room or out of it — or, where its
// outcome is unknown, the unknown answer.
//
// ITS REPEAT IS OFFERED RATHER THAN WARNED AGAINST ([restateNext]): stating
// the same thing again changes nothing if the first one landed, whichever
// operation the repeat is.
func stated(tool string, written chat.Written, look string,
	out map[string]any) (tools.Result, error) {
	if written.Outcome.Outcome == statelog.OutcomeUnknown {
		return unknownOutcome(chatUnknown(tool, written.Outcome, restateNext(look))), nil
	}
	out["revision"] = written.Revision
	out["outcome"] = string(written.Outcome.Outcome)
	out["position"] = positionOf(written.Outcome.Position)
	return jsonResult(out)
}

// directUnknown is send_dm's answer when the conversation's OPEN came back
// `unknown` — a lost acknowledgement, or an operation this node's ledger
// cannot vouch for, whose answer names no room at all.
//
// # Why it stops there
//
// Because the post would go into a room this call cannot name. An unvouched
// open answers no id, so the message would be addressed to nothing and
// refused as "not found" — a different and misleading answer about a
// conversation that may well exist; a lost acknowledgement names the derived
// room, which this node may not have applied, and is refused the same way.
// Neither is what happened, and both send a seat looking for a problem it
// does not have.
//
// # Why its advice is not [unknownNext]'s
//
// NOTHING WAS SAID: the message comes after the open and was never sent. So
// the hazard every other `unknown` here warns about — a repeat that is a new
// operation saying it twice — is not there: a direct conversation is its
// participants' whatever opens it, and the message has not been posted once.
// The same call is still what to make, because for a seat it is the same
// operation and reaches the same room; where this node cannot vouch for that
// operation it answers the same way until the open reaches this node.
//
// `why` is what made it unknown, already said ([unknownWhy] for an answer
// that said so, the router's error for a data node that never answered), and
// `unvouched` whether this node's ledger is what cannot tell.
func directUnknown(to []string, why string, unvouched bool, again string) string {
	next := "Call send_dm again"
	if again != "" {
		next = capitalize(again)
	}
	next += ": the conversation is opened once however often it is asked for, " +
		"and the message is then said once."
	if unvouched {
		next += " Until the open reaches this node the same call answers the " +
			"same way, so if it keeps doing so, say you could not send it."
	}
	return fmt.Sprintf("send_dm: whether the conversation with %s could be "+
		"opened is unknown (%s), so the message was NOT sent — nothing was "+
		"said. %s", clip(strings.Join(to, ", ")), why, next)
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

// chatReadFailure explains a read that could not be served.
//
// IT NEVER SAYS "NOTHING FOUND", on [readFailure]'s reasoning: a node whose
// rows are behind must not be able to tell a seat the room is empty, because
// a seat that believes nobody answered asks again — and its class is
// [tools.RefusalUnavailable] for the same reason, since a person's surface
// told "not found" about a read this node could not serve renders the room as
// gone. A room the read itself reports absent is [tools.RefusalNotFound], a
// reader with no seat behind it is refused by the store naming the binding it
// lacks, which is [tools.RefusalForbidden], and an argument the reader refused
// — a cursor that is no position, a thread named by nothing — is
// [tools.RefusalInvalid]: "try again" over a refused argument is a retry that
// is refused the same way every time.
func chatReadFailure(name string, err error) tools.Result {
	switch {
	case errors.Is(err, chat.ErrInvalid):
		return refused(tools.RefusalInvalid, fmt.Sprintf("%s refused that: %s",
			name, chat.Sentence(err)))
	case errors.Is(err, chat.ErrNotFound):
		return refused(tools.RefusalNotFound, fmt.Sprintf("%s: %v\n\nCheck the "+
			"room with list_channels. A private room you are not in answers the "+
			"same way as one that does not exist.", name, err))
	case errors.Is(err, chat.ErrForbidden):
		return refused(tools.RefusalForbidden, fmt.Sprintf("%s was refused: %s",
			name, chat.Sentence(err)))
	}
	return refused(tools.RefusalUnavailable, fmt.Sprintf("%s could not read the "+
		"company's chat right now (%v). This is NOT an empty room — do not "+
		"conclude nothing was said. Try again, or say you could not check.",
		name, err))
}

// ---- addressing a room ------------------------------------------------- //

// channelID resolves what a model typed to a room id.
//
// # Why a name resolves at all
//
// Because a model will type one. A room's id is a uuid nothing in a
// conversation ever says out loud, while its NAME is what the room is called
// in every sentence anybody writes about it — so a tool that took only the id
// would refuse the first call of every turn that had not just listed the
// rooms. This only runs when the argument is not already an id, so the common
// case — a room id carried in from the wake that woke this seat — costs
// nothing.
//
// # And why ANY room the viewer may read, not only the ones it is in
//
// Because a name is the room's ADDRESS — unique across the company and claimed
// once — and the store resolves it as one ([chat.Reader.ChannelNamed]): a
// public room, or a unit's room, resolves for whoever may read it, joined or
// not, and a private room only for its members. Resolved against the caller's
// own rooms instead, a unit's room an asker had promised a decision's outcome
// to was writable by id while its name meant nothing, and no tool handed a
// seat that id — so the post it owed could never be made and its turn was
// sent back until its cap.
//
// # What a failure is
//
// A name nothing the viewer may read holds is the ARGUMENT's —
// [tools.RefusalInvalid], never not-found, because the fix is the argument and
// a private room answers exactly as a name nobody holds. A lookup this node
// could not serve is [tools.RefusalUnavailable]: the name may be perfectly
// good, and nothing was written.
func (d ChatDeps) channelID(ctx context.Context, name, viewer, ref string) (
	string, *tools.Result) {

	// A LEADING '#' IS HOW PEOPLE WRITE A ROOM and never part of a name —
	// [chat.ValidName] admits neither it nor any capital — so stripping it
	// here cannot collide with a room somebody really called that.
	want := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ref), "#"))
	if want == "" {
		out := failed(name + " needs a `channel` — the room's id, or its name.")
		return "", &out
	}
	if _, err := uuid.Parse(want); err == nil {
		return want, nil
	}
	if d.Reader == nil {
		// THIS SURFACE HOLDS NOTHING TO RESOLVE A NAME AGAINST, so the
		// argument that works here is the id — which is the fix, and why
		// it is the argument's class rather than the node's.
		out := failed(fmt.Sprintf("%s cannot resolve the room name %q here — "+
			"name the room by its id.", name, clip(want)))
		return "", &out
	}
	// ARCHIVED ROOMS INCLUDED, as the address always is: a read of one is
	// legitimate and a write to one is refused by the store naming the room,
	// which is a better answer than "no such channel".
	room, err := d.Reader.ChannelNamed(ctx, viewer, want, seatRead)
	switch {
	case errors.Is(err, chat.ErrNotFound):
		out := failed(fmt.Sprintf("%s: there is no room called %q that you can "+
			"read — list_channels names the rooms you are in, and a private "+
			"room you are not in cannot be addressed at all.", name, clip(want)))
		return "", &out
	case err != nil:
		out := refused(tools.RefusalUnavailable, fmt.Sprintf("%s could not "+
			"resolve the room name %q right now (%v). Nothing was read or "+
			"written — name the room by its id, or try again.", name,
			clip(want), err))
		return "", &out
	}
	return room.Channel.ID, nil
}

// resolved is the shared preamble of every tool here: who is acting, and
// which room they meant.
func (d ChatDeps) resolved(ctx context.Context, turn *turnctx.Turn, name, ref string) (
	chat.Actor, string, *tools.Result) {

	actor, err := d.actor(ctx, turn)
	if err != nil {
		out := chatActorFailure(name, err)
		return chat.Actor{}, "", &out
	}
	id, refusal := d.channelID(ctx, name, actor.Name(), ref)
	if refusal != nil {
		return chat.Actor{}, "", refusal
	}
	return actor, id, nil
}

// collectiveAddress matches the way this surface addresses a whole room.
//
// `@channel`, AS A WHOLE WORD. The bound on the right is what stops
// `@channels` from waking thirty-two seats, and the left-hand class is what
// keeps an email address or a path ending in the word from doing it.
var collectiveAddress = regexp.MustCompile(`(^|[^0-9A-Za-z_@])@channel($|[^0-9A-Za-z_-])`)

// addressesRoom reports whether this message addressed the whole room.
//
// DERIVED FROM THE BODY, and deliberately NOT an argument. The prompt a woken
// seat reads tells it that a room is addressed by writing `@channel`, so that
// is the gesture a model reproduces when it wants to reach everybody — and a
// flag beside the text would let the two disagree in both directions: a
// message saying `@channel` that woke nobody reads to the room as though
// everybody had seen it, and a flag set on a message that says nothing of the
// sort wakes thirty-two seats with no visible reason why.
func addressesRoom(body string) bool {
	return collectiveAddress.MatchString(body)
}

// message builds what a post or a reply carries, from the body the model
// wrote and the call's key ([ChatDeps.callKey]).
//
// THE KEY DERIVES THE MESSAGE'S ID, which is also its operation's (see the
// head of chat/ids.go): in a turn, from the unit of work and this call's
// repeat count in the run, so a re-run posts each of the turn's messages once
// and a turn's second remark is never its first one's retry; under a request,
// from the operation the transport derived from it, so a person's retry is
// the message they already sent. An operator's assistant over MCP names
// neither, and each of its posts is a new message — exactly as a person
// pressing send twice says it twice, since nothing redelivers that call.
func (d ChatDeps) message(body string, links []string, key statelog.CallKey) chat.NewMessage {
	in := chat.NewMessage{
		Body: body, Links: links, Collective: addressesRoom(body), CallKey: key,
	}
	if d.Mentions != nil {
		in.Mentions = d.Mentions.Mentions(body)
	}
	return in
}

// posted is the receipt every posting tool answers with — or, for a post whose
// outcome is unknown, the unknown answer and never a receipt.
//
// # Why an unknown post is not a receipt
//
// Because the rest of a receipt is a claim. A `message_id` beside it is read
// as the message said, and the seat reports it said — when nothing may have
// been published at all: an operation this node's ledger cannot vouch for is
// answered `unknown` WITHOUT publishing, which is exactly what a seat
// re-running a turn whose trigger predates the ledger's sweep is told.
// [unknownWrite]'s reasoning, for the same reader.
//
// `again` is how this caller repeats the post as the same message
// ([keyedCaller.sameCall]): a seat's same call, a person's same request, or
// nothing — an operator's assistant over MCP, whose repeat is a second
// message if the first one landed.
func posted(tool string, written chat.Written, channelID, again string) (tools.Result, error) {
	if written.Outcome.Outcome == statelog.OutcomeUnknown {
		return unknownOutcome(chatUnknown(tool, written.Outcome,
			postNext(written.Outcome.Unvouched, again, "Read the room with read_channel"))), nil
	}
	return jsonResult(postReceipt(written, channelID))
}

// postNext is what a caller does about a post whose outcome is unknown —
// [unknownNext] for a message, in ONE place, because a post can come back
// unknown two ways (an answer that said so, a data node that never answered)
// and both must say the same thing. `look` names the read that shows the room.
func postNext(unvouched bool, again, look string) string {
	return unknownNext(unvouched, again, look, "that is a second message")
}

// postReceipt is what a post that landed answers: the message, where it was
// said, and the outcome and position a caller reads after it — see [posted].
func postReceipt(written chat.Written, channelID string) map[string]any {
	out := map[string]any{
		"message_id": written.ChangeID, "channel": channelID,
		"revision": written.Revision,
		"outcome":  string(written.Outcome.Outcome),
		"position": positionOf(written.Outcome.Position),
	}
	if root := written.Message.ThreadRoot; root != "" {
		out["thread_root"] = root
	}
	if len(written.Message.Mentions) > 0 {
		out["mentioned"] = written.Message.Mentions
	}
	if written.Message.Collective {
		// SAID BACK, because the model did not pass a flag: it wrote
		// `@channel` in prose and the engine read it as an address, so
		// the receipt is where it learns that thirty-two colleagues are
		// about to be woken.
		out["addressed_whole_room"] = true
	}
	return out
}

// ---- post_message ------------------------------------------------------ //

type postMessage struct{ deps ChatDeps }

var _ tools.SeatCallable = (*postMessage)(nil)

func (t *postMessage) Name() string { return PostMessageTool }

func (t *postMessage) Description() string {
	return "Say something new in a room. This starts a TOP-LEVEL message: to " +
		"answer something somebody said, use reply_in_thread instead, which " +
		"puts your answer beside the question where the next reader will " +
		"find it. @-mention a colleague by handle to reach them specifically; " +
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
					"at. There are no file attachments.", chat.MaxLinks),
			},
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
	if denied := foreignOperation(ctx, turn, t.deps.Operation, PostMessageTool, args); denied != "" {
		return failed(denied), nil
	}
	body := strings.TrimSpace(argString(args, "body"))
	if body == "" {
		return failed("post_message needs a `body` — an empty message is a " +
			"wake for nothing."), nil
	}
	actor, id, refusal := t.deps.resolved(ctx, turn, PostMessageTool, argString(args, "channel"))
	if refusal != nil {
		return *refusal, nil
	}
	key := t.deps.callKey(ctx, turn, PostMessageTool, args)
	again := callerOf(turn, key).sameCall(PostMessageTool)
	written, err := t.deps.Writer.Post(ctx, actor, id,
		t.deps.message(body, argStrings(args, "links"), key))
	if err != nil {
		return chatWriteFailure(PostMessageTool, err,
			postNext(false, again, "Read the room with read_channel")), nil
	}
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
					"one you were woken about, or one read_channel returned.",
			},
			"body": map[string]any{
				"type":        "string",
				"description": "Your answer, in markdown.",
			},
			"links": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": fmt.Sprintf("Up to %d URLs. No file attachments.",
					chat.MaxLinks),
			},
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
	if denied := foreignOperation(ctx, turn, t.deps.Operation, ReplyInThreadTool, args); denied != "" {
		return failed(denied), nil
	}
	body := strings.TrimSpace(argString(args, "body"))
	replyTo := strings.TrimSpace(argString(args, "reply_to"))
	switch {
	case body == "":
		return failed("reply_in_thread needs a `body`."), nil
	case replyTo == "":
		return failed("reply_in_thread needs a `reply_to` — the id of the " +
			"message you are answering. To start a new topic, use post_message."), nil
	}
	actor, id, refusal := t.deps.resolved(ctx, turn, ReplyInThreadTool, argString(args, "channel"))
	if refusal != nil {
		return *refusal, nil
	}
	key := t.deps.callKey(ctx, turn, ReplyInThreadTool, args)
	again := callerOf(turn, key).sameCall(ReplyInThreadTool)
	written, err := t.deps.Writer.Reply(ctx, actor, id, replyTo,
		t.deps.message(body, argStrings(args, "links"), key))
	if err != nil {
		return chatWriteFailure(ReplyInThreadTool, err,
			postNext(false, again, "Read the room with read_channel")), nil
	}
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
					"people can join by.", chat.MaxDMParticipants),
			},
			"body": map[string]any{"type": "string", "description": "What to say."},
			"links": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
			},
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
	if denied := foreignOperation(ctx, turn, t.deps.Operation, SendDMTool, args); denied != "" {
		return failed(denied), nil
	}
	body := strings.TrimSpace(argString(args, "body"))
	// A BARE STRING IS ACCEPTED BESIDE THE ARRAY, because "send a DM to
	// alice" is one recipient and a model writing `"to": "alice"` means
	// exactly what the one-element array means. [argStrings] already reads
	// both.
	to := argStrings(args, "to")
	switch {
	case body == "":
		return failed("send_dm needs a `body`."), nil
	case len(to) == 0:
		return failed("send_dm needs a `to` — the handle of the colleague you " +
			"are writing to. Use lookup_colleague if you are unsure of it."), nil
	}
	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		return chatActorFailure(SendDMTool, err), nil
	}

	// ONE KEY FOR BOTH APPENDS, read once: the open and the post are two
	// gestures with two verbs, so the store derives two operations from it,
	// and reading the count once is what keeps them this one call's.
	key := t.deps.callKey(ctx, turn, SendDMTool, args)
	again := callerOf(turn, key).sameCall(SendDMTool)
	said := t.deps.message(body, argStrings(args, "links"), key)
	// WHAT THE MESSAGE SAYS IS REFUSED BEFORE ANYTHING IS WRITTEN. A body
	// over the cap is refused by the post however the room is found, so
	// opening the conversation first only left a room in every
	// participant's list, opened by a refusal. What the post decides from
	// the room itself is still the post's — see [directPostFailure].
	if rejected := said.Validate(); rejected != nil {
		// A REFUSAL OF CONTENT, never unknown: nothing was sent anywhere.
		return chatWriteFailure(SendDMTool, rejected, ""), nil
	}
	room, _, err := t.deps.Writer.OpenDirect(ctx, actor, to, key)
	if errors.Is(err, estate.ErrOutcomeUnknown) {
		// AN OPEN NOBODY ANSWERED is [directUnknown]'s case — no room this
		// call can name, so nothing is posted into one — whichever way it
		// came back unknown.
		return unknownOutcome(directUnknown(to, err.Error(), false, again)), nil
	}
	if err != nil {
		return chatWriteFailure(SendDMTool, err, ""), nil
	}
	if room.Outcome.Outcome == statelog.OutcomeUnknown {
		// NO ROOM THIS CALL CAN NAME, so nothing is posted into one — see
		// [directUnknown].
		return unknownOutcome(directUnknown(to, unknownWhy("the open's",
			room.Outcome), room.Outcome.Unvouched, again)), nil
	}
	// AND SETTLE IT BEFORE SAYING ANYTHING IN IT. The post decides from
	// the rows of whichever copy takes it — it reads the room to route the
	// message — so a post decided before that copy applied the create finds
	// no room at all and is refused for a conversation that exists. Settled,
	// the open's position is the floor the post's own decision waits for.
	t.deps.settle(ctx, room.Outcome.Position)

	written, err := t.deps.Writer.Post(ctx, actor, room.Channel.ID, said)
	if err != nil {
		return directPostFailure(to, room, err, again), nil
	}
	t.deps.settle(ctx, written.Outcome.Position)
	if written.Outcome.Outcome == statelog.OutcomeUnknown {
		// THE OPEN LANDED AND THE ANSWER SAYS SO, as a refused post's does:
		// two records, and only the second one is in doubt.
		return unknownOutcome(directOpen(to, room) + chatUnknown(
			SendDMTool+"'s message", written.Outcome,
			postNext(written.Outcome.Unvouched, again,
				"Read the conversation with read_channel"))), nil
	}
	// TWO RECORDS, ONE ANSWER: the call is as certain as the less certain
	// of the open and the post, and a caller that reads after it waits
	// for the later of the two — [statelog.LessCertain] and
	// [statelog.Later], the rule every write that appends twice answers by.
	// An open that found the conversation already there appended nothing,
	// and answers no position.
	out := postReceipt(written, room.Channel.ID)
	out["outcome"] = string(statelog.LessCertain(room.Outcome.Outcome,
		written.Outcome.Outcome))
	out["position"] = positionOf(statelog.Later(room.Outcome.Position,
		written.Outcome.Position))
	return jsonResult(out)
}

// directOpen is how every send_dm answer about its post begins once the open
// has landed: the conversation is there, whatever became of the message.
func directOpen(to []string, room chat.Written) string {
	return fmt.Sprintf("The conversation with %s is open (%s). ",
		clip(strings.Join(to, ", ")), room.Channel.ID)
}

// directPostFailure is send_dm's answer when the conversation is open and the
// message was refused — [dependencyFailure]'s shape for this call: two
// writes, and only the second failed.
//
// THE CLASS IS THE POST'S, because the post is what did not happen and its
// remedy is the caller's to take. THE SENTENCE SAYS THE ROOM IS THERE, because
// "nothing was written" is false about the open, and a caller that believed it
// would look for a conversation-opening problem it does not have.
//
// WHICH IS WHY THE NODE'S CLASS IS SAID HERE rather than borrowed: every
// unavailable sentence [chatWriteFailure] writes ends "nothing was written",
// which is exactly the claim this answer exists to correct — prefixed to it,
// the answer said the conversation was open and that nothing was written, in
// one breath. The caller's classes carry no such claim and are said as written.
//
// AN UNKNOWN POST STAYS UNKNOWN, and it is asked of the mapper's verdict before
// anything is said in the node's class, because an unknown outcome IS that
// class: a post sent to a data node that never answered ([estate.ErrOutcomeUnknown],
// the router's answer for a write carrying no key of its caller's, since a
// repeat would be a second message) may have been said. Rewritten as "did not
// land … was not said", it told the caller the one thing nobody can know and
// invited the second message the router refused to risk — and dropped the
// [tools.Result.Unknown] mark a person's surface tells "nothing happened" from
// "may have happened" by.
func directPostFailure(to []string, room chat.Written, err error, again string) tools.Result {
	failure := chatWriteFailure(SendDMTool, err, "")
	if failure.Unknown {
		return unknownOutcome(directOpen(to, room) + fmt.Sprintf("Whether the "+
			"message landed is unknown (%v). It may have been said and it may "+
			"not — do not report it as sent, and do not report it as failed. %s",
			err, postNext(false, again, "Read the conversation with read_channel")))
	}
	if failure.Refusal == tools.RefusalUnavailable {
		failure.Output = fmt.Sprintf("the message did not land (%v) and was not "+
			"said — do not report it as sent.", err)
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
		"oldest first, by naming the message it hangs off. A thread you were " +
		"woken in is handed to you at turn start under `## The thread so far`; " +
		"read it here only where that block says it is incomplete. The rest " +
		"of the room is not handed to you, so read it here when your answer " +
		"depends on what was said before."
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
					chat.MaxLimit, chat.DefaultLimit),
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
	actor, id, refusal := t.deps.resolved(ctx, turn, ReadChannelTool, argString(args, "channel"))
	if refusal != nil {
		return *refusal, nil
	}
	limit := argInt(args, "limit", 0)
	cursor := strings.TrimSpace(argString(args, "cursor"))

	if root := strings.TrimSpace(argString(args, "thread")); root != "" {
		got, err := t.deps.Reader.Thread(ctx, actor.Name(), chat.ThreadQuery{
			ChannelID: id, RootID: root, Cursor: cursor, Limit: limit,
		}, seatRead)
		if err != nil {
			return chatReadFailure(ReadChannelTool, err), nil
		}
		return jsonAnswer(got, "Ask for fewer messages with `limit`.")
	}
	got, err := t.deps.Reader.Messages(ctx, actor.Name(), chat.TranscriptQuery{
		ChannelID: id, Cursor: cursor, Limit: limit,
	}, seatRead)
	if err != nil {
		return chatReadFailure(ReadChannelTool, err), nil
	}
	return jsonAnswer(got, "Ask for fewer messages with `limit`.")
}

// ---- search_messages --------------------------------------------------- //

type searchMessages struct{ deps ChatDeps }

var _ tools.SeatCallable = (*searchMessages)(nil)

func (t *searchMessages) Name() string { return SearchMessagesTool }

func (t *searchMessages) Description() string {
	return "Find a message by what it SAYS, ranked across every room you can " +
		"read. Use it when you remember that something was discussed but not " +
		"where: \"the decision about the retry backoff\". read_channel is the " +
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
			"limit": map[string]any{"type": "integer"},
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
		return failed("search_messages needs a `text` — what was said, in " +
			"plain words."), nil
	}
	if knowledge.CheckQuery(text) != nil {
		// REFUSED, NOT CUT, at the bound every ranked search shares
		// ([knowledge.MaxQueryBytes]). The index behind this refuses it
		// too, but as a failure the model would read as chat being
		// unreadable; this says what to send instead.
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return failed(fmt.Sprintf("search_messages' `text` is %d bytes and takes at "+
			"most %d — search on a few words somebody would have said, not a "+
			"pasted passage.", len(text), knowledge.MaxQueryBytes)), nil
	}
	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		return chatActorFailure(SearchMessagesTool, err), nil
	}
	// THE ROOM IS RESOLVED ONLY WHEN ONE WAS NAMED, and a name that resolves
	// to nothing is REFUSED rather than dropped: searching every room in its
	// place would hand back hits from rooms the caller did not mean, read as
	// the one it did. The refusal says to name the room by its id or leave
	// the filter out. An id is never refused here — the searcher intersects
	// it with what the viewer may read ([ChatSearch.Channel]).
	room := ""
	if named := strings.TrimSpace(argString(args, "channel")); named != "" {
		var refusal *tools.Result
		room, refusal = t.deps.channelID(ctx, SearchMessagesTool, actor.Name(), named)
		if refusal != nil {
			return *refusal, nil
		}
	}
	hits, err := t.deps.Search.SearchMessages(ctx, actor.Name(), ChatSearch{
		Text:    text,
		Channel: room,
		Author:  strings.TrimSpace(argString(args, "author")),
		Limit:   argInt(args, "limit", 0),
	})
	if err != nil {
		return chatReadFailure(SearchMessagesTool, err), nil
	}
	if len(hits) == 0 {
		return tools.Result{Output: "Nothing in the rooms you can read matches that."}, nil
	}
	return jsonAnswer(map[string]any{"count": len(hits), "messages": hits},
		"Narrow it with `channel`, `author` or a shorter `limit`.")
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

// channelRow is one room as this tool renders it.
//
// A PROJECTION rather than the reader's own summary, for one field: the rail
// is built for a screen and carries an UNREAD COUNT, which is a fact about a
// person's attention. A seat has none — see [ChatDeps.ReadState] — and
// handing a model "99+ unread" for a room it was never reading sends it off
// to catch up on a backlog that is not its work.
type channelRow struct {
	ID   string    `json:"id"`
	Name string    `json:"name,omitempty"`
	Kind chat.Kind `json:"kind"`

	Topic   string `json:"topic,omitempty"`
	Purpose string `json:"purpose,omitempty"`
	Unit    string `json:"unit,omitempty"`

	Archived bool `json:"archived,omitempty"`

	// Participants are the handles in a direct conversation, which has no
	// name to render.
	Participants []string `json:"participants,omitempty"`

	// FollowAll says every message here reaches this seat, whether or not
	// it is named — which is what a unit's own room does for the seats in
	// that unit.
	FollowAll bool `json:"follow_all,omitempty"`

	// Muted says the person reading this silenced the room, as the rail
	// marks it: it raises no badge and no push for them. It suppresses
	// NOTICE and never delivery — the messages are there and the count is
	// still true — so a reader may still read it, and should not present
	// its backlog as something the person is waiting on.
	Muted bool `json:"muted,omitempty"`

	// Unread is nil when the caller has no read state, which is not the
	// same fact as zero: a POINTER, because "nothing new" and "nobody is
	// tracking where you got to" are answers a reader acts on differently.
	Unread       *int `json:"unread,omitempty"`
	UnreadCapped bool `json:"unread_capped,omitempty"`

	Last *chat.MessagePreview `json:"last,omitempty"`
}

func (t *listChannels) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *listChannels) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	if t.deps.Reader == nil {
		return unconfiguredChat(ListChannelsTool), nil
	}
	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		return chatActorFailure(ListChannelsTool, err), nil
	}
	q := chat.ChannelsQuery{IncludeArchived: argBool(args, "include_archived")}
	// COUNTED ONLY AGAINST A READ STATE THAT WAS READ — see
	// [ChatDeps.ReadState]. One that could not be is not a cursor at zero,
	// and the rooms, their order and what was last said in them are still
	// true without it, so it costs the counts and never the listing.
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
			q.Cursors = state.Cursors
		}
	}
	muted := make(map[string]bool, len(state.Muted))
	for _, id := range state.Muted {
		muted[id] = true
	}
	listing, err := t.deps.Reader.Channels(ctx, actor.Name(), q, seatRead)
	if err != nil {
		return chatReadFailure(ListChannelsTool, err), nil
	}
	if len(listing.Channels) == 0 {
		return tools.Result{Output: "You are in no chat rooms."}, nil
	}
	rows := make([]channelRow, 0, len(listing.Channels))
	for _, room := range listing.Channels {
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
		// SAID, rather than left to a reader noticing every count is
		// missing: no counts and nothing unread are different facts. Under
		// the rail's own name for the fact — whether the read state was
		// measured — and never under `unread`, which every row uses for a
		// number: one key with two types and two meanings in one answer.
		out["read_state"] = fmt.Sprintf("unknown: how far you have read could "+
			"not be fetched (%v), so no room carries a count of what is new, "+
			"and none is marked muted", unreadable)
	}
	if counted && state.DNDUntil.After(t.deps.now()) {
		// THE PERSON IS IN DO-NOT-DISTURB, as the rail says: an instant
		// rather than a flag, so a reader can tell when it ends. One
		// already past is not in it, and is not reported as if it were.
		out["dnd_until"] = state.DNDUntil
	}
	// AND WHAT THE ANSWER COULD NOT ACCOUNT FOR. A model that reads a
	// short list as the whole truth posts in the wrong room.
	if !listing.Complete {
		out["complete"] = false
	}
	if listing.Truncated {
		out["truncated"] = true
	}
	return jsonAnswer(out, "There is no narrower listing — read one room with read_channel.")
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
			"message": map[string]any{
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
		"required": []any{"channel", "message", "emoji"},
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
	if denied := foreignOperation(ctx, turn, t.deps.Operation, ReactToMessageTool, args); denied != "" {
		return failed(denied), nil
	}
	messageID := strings.TrimSpace(argString(args, "message"))
	emoji := strings.TrimSpace(argString(args, "emoji"))
	switch {
	case messageID == "":
		return failed("react_to_message needs a `message` — the id of the " +
			"message you are reacting to."), nil
	case emoji == "":
		return failed("react_to_message needs an `emoji`."), nil
	}
	actor, id, refusal := t.deps.resolved(ctx, turn, ReactToMessageTool, argString(args, "channel"))
	if refusal != nil {
		return *refusal, nil
	}
	remove := argBool(args, "remove")
	react := t.deps.Writer.React
	if remove {
		react = t.deps.Writer.Unreact
	}
	const look = "Read the message's reactions with read_channel"
	written, err := react(ctx, actor, id, messageID, emoji,
		t.deps.callKey(ctx, turn, ReactToMessageTool, args))
	if err != nil {
		return chatWriteFailure(ReactToMessageTool, err, restateNext(look)), nil
	}
	t.deps.settle(ctx, written.Outcome.Position)
	return stated(ReactToMessageTool, written, look, map[string]any{
		"channel": id, "message_id": messageID, "emoji": emoji,
		"removed": remove,
	})
}

// ---- join_channel / leave_channel -------------------------------------- //

// membershipLook is the read that shows whether a join or a leave landed.
const membershipLook = "Check list_channels, which names every room you are in"

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
	if denied := foreignOperation(ctx, turn, t.deps.Operation, JoinChannelTool, args); denied != "" {
		return failed(denied), nil
	}
	actor, id, refusal := t.deps.resolved(ctx, turn, JoinChannelTool, argString(args, "channel"))
	if refusal != nil {
		return *refusal, nil
	}
	written, err := t.deps.Writer.Join(ctx, actor, id,
		t.deps.callKey(ctx, turn, JoinChannelTool, args))
	if err != nil {
		return chatWriteFailure(JoinChannelTool, err, restateNext(membershipLook)), nil
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
	if denied := foreignOperation(ctx, turn, t.deps.Operation, LeaveChannelTool, args); denied != "" {
		return failed(denied), nil
	}
	actor, id, refusal := t.deps.resolved(ctx, turn, LeaveChannelTool, argString(args, "channel"))
	if refusal != nil {
		return *refusal, nil
	}
	written, err := t.deps.Writer.Leave(ctx, actor, id,
		t.deps.callKey(ctx, turn, LeaveChannelTool, args))
	if err != nil {
		return chatWriteFailure(LeaveChannelTool, err, restateNext(membershipLook)), nil
	}
	t.deps.settle(ctx, written.Outcome.Position)
	return stated(LeaveChannelTool, written, membershipLook, map[string]any{
		"channel": id, "name": written.Channel.Name, "left": true,
	})
}
