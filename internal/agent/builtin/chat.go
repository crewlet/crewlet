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
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// Chatlet's tool names, re-exported from the package that owns the
// vocabulary.
//
// DECLARED IN internal/chatlet rather than here, for the reason the tracker's
// are: that package's own notification prompt NAMES them — it tells a woken
// seat which tool to reach for — and a constant defined here and read there
// is an import cycle. The domain owns its vocabulary; this package implements
// it.
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

// ChatWrites are the three that count as a DELIVERY.
//
// SAYING SOMETHING IS THE ANSWER on this surface — in the thread, in the room
// or privately — and without this the delivery gate sees only builtins,
// concludes the turn reached nobody, and corrects it into another round for
// having done exactly what it was woken to do. See [chatlet.WriteTools] for why
// a reaction and a membership change are not here.
func ChatWrites() []string { return chatlet.WriteTools() }

// ChatReader is what these tools need from chat's read side.
//
// THE LEVEL IS PART OF THE CALL, on [PageReader]'s reasoning: a seat reads at
// [seatReadLevel] — `linearizable` — because it must see its own writes, and
// that is what stops a seat that has just posted from reading the room, not
// finding its message and posting again.
//
// EVERY READ IS FOR A VIEWER, the seat it is answered as: a private room's
// contents are its membership's, and the reader answers a room the viewer may
// not read as NOT FOUND rather than confirming it exists.
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
	// Thread is one thread from its root — and, asked of any message as its
	// root, that message: a reply's own thread is empty, and its root read
	// is the message itself, which is how an edit and a delete learn whose
	// words they would touch ([ChatDeps.remarkAuthority]).
	Thread(ctx context.Context, viewer string, q chatlet.ThreadQuery,
		fresh statelog.Freshness) (chatlet.Thread, error)
}

// ChatWriter is what these tools need from chat's write side.
//
// THE ACTOR IS A PARAMETER rather than the seam being resolved per actor, and
// that is the domain's own shape rather than a choice made here: one node's
// chat serves every seat and every person through one write path, so the
// party a write acts as travels per call. It is the knowledge base's
// arrangement, not the tracker's — see [PageWriter] — and so is the KEY every
// gesture's operation is derived from: it rides on the actor
// ([chatlet.Actor.OpKey]), set from the call ([callOpKey]).
type ChatWriter interface {
	Post(ctx context.Context, actor chatlet.Actor, channelID string,
		in chatlet.NewMessage) (chatlet.Written, error)
	Reply(ctx context.Context, actor chatlet.Actor, channelID, replyTo string,
		in chatlet.NewMessage) (chatlet.Written, error)
	// OpenDirect reports whether it CREATED the conversation beside the
	// room itself, because "it was already open" is not a failure here —
	// the id is derived from the participants, so the loser of a race is
	// handed the room the winner made.
	OpenDirect(ctx context.Context, actor chatlet.Actor, participants []string) (
		chatlet.Written, bool, error)
	React(ctx context.Context, actor chatlet.Actor, channelID, messageID,
		emoji string) (chatlet.Written, error)
	Unreact(ctx context.Context, actor chatlet.Actor, channelID, messageID,
		emoji string) (chatlet.Written, error)
	Join(ctx context.Context, actor chatlet.Actor, channelID string) (chatlet.Written, error)
	Leave(ctx context.Context, actor chatlet.Actor, channelID string) (chatlet.Written, error)

	// The gestures only a person makes — see chatoperator.go. They are on
	// the one writer rather than a second seam, because they are the same
	// store's and a seat's registry simply never offers them.
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

// ChatSearcher ranks a company's chat FOR ONE VIEWER.
//
// Declared by the consumer and kept to the one method these tools call: the
// estate router's chat search, which resolves the rooms the viewer may read
// beside the index on the one data node that answers. THE VISIBLE SET IS NOT
// AN ARGUMENT, and that is the point: resolving which rooms somebody may read
// is the seam's job, because it is the half that holds both the membership
// rows and the index — and a tool that passed the set would be a tool that
// could pass the wrong one.
//
// Nil on a build with no chat index, and the tool is then OMITTED rather than
// refusing at the call, on [Register]'s own rule.
type ChatSearcher interface {
	Search(ctx context.Context, q estate.ChatQuery) (estate.ChatSearchAnswer, error)
}

// THE ESTATE ROUTER'S CHAT IS EACH SEAM, held at compile time: it is what
// every surface hands these tools, on every node with or without data, and a
// seam whose method drifted from it would fail only where a node is wired.
var (
	_ ChatReader   = estate.Chat{}
	_ ChatWriter   = estate.Chat{}
	_ ChatSearcher = estate.ChatSearch{}
)

// ChatDeps are chat's halves plus what a write needs.
type ChatDeps struct {
	Reader ChatReader
	Writer ChatWriter

	// Search ranks messages by what they say. A VALUE rather than a
	// function of the actor, unlike the writer: a search reads, and what
	// may be read is decided from the viewer it is handed.
	Search ChatSearcher

	// Authorize decides the checks a tool can only take after a read — a
	// room's archive, whose remark an edit or a delete touches — in the
	// shape and for the reason [WorkDeps.Authorize] states. Pushed down by
	// [Register] and [OperatorTools].
	Authorize Authorizer

	// Mentions resolves the handles a message names, so an @-mention wakes
	// the person the author meant. Nil resolves nothing, which degrades to
	// a message that reaches the room's own subscribers and nobody
	// specifically.
	Mentions MentionResolver

	// ReadState is this viewer's read state: how far they have read in
	// each room ([coord.ChatReadState.Cursors], keyed on channel id against
	// a packed position — what [chatlet.ChannelsQuery.Cursors] counts above),
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
	// ITS ERROR IS THE COORDINATION STORE'S, RETURNED AS IT IS, and it has
	// three answers the tool tells apart by sentinel alone: a flush the
	// merge refused ([coord.ErrInvalidReadState], the caller's to change,
	// said to the caller in the merge's own words through
	// [coord.ReadStateSentence]); a flush sent whose answer was lost
	// ([coord.ErrWriteUnknown], which may have landed, answered `unknown`
	// and safe to repeat, because the merge is idempotent); and any other
	// error, a store that could not take it and changed nothing (this
	// node's condition or fault). Re-worded, a sentinel is lost — a
	// refusal that can never land reads "try again", and a flush that may
	// have landed reads "nothing was recorded".
	Cursors ChatCursors

	// Now stamps a read-state write ([coord.ChatReadDelta.At]). Nil is the
	// wall clock, in UTC.
	Now func() time.Time

	// Actor decides who a write is attributed to, and — because a private
	// room's contents are decided by its membership — who a READ is served
	// as. Nil takes the turn's seat; the operator surface sets it. See
	// [WorkDeps.Actor] for why this is a seam rather than a second copy of
	// these tools.
	//
	// AND IT CARRIES THE CALLER'S OPERATION KEY where the caller's
	// transport names one, exactly as [PageDeps.Actor] does: a surface that
	// takes an `Idempotency-Key` sets it as [chatlet.Actor.OpKey] on the actor
	// it returns, and the store derives every gesture's id from it, bound
	// to what that gesture says. In a turn the key is the turn's
	// ([callOpKey]), whatever the actor carries.
	Actor func(ctx context.Context, turn *turnctx.Turn) (chatlet.Actor, error)

	// Await is handed the position every chat write landed at, so the next
	// read sees it. See [WorkDeps.Await]: same seam, same reason, and the
	// same floor the wait happens behind — every surface, the operator's
	// included, hands the position to the node's session floor, which every
	// later read and write carries to whichever copy answers it. On this
	// surface it is the difference between a seat seeing its own message
	// and posting it twice.
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
// re-run turn post once is the call's key ([callOpKey]), whose seed is the
// unit of work a redelivery reproduces.
func chatActor(turn *turnctx.Turn) (chatlet.Actor, error) {
	seat, err := turn.RequireSeat()
	if err != nil {
		return chatlet.Actor{}, err
	}
	return chatlet.Actor{
		Handle: seat.Handle(), Kind: chatlet.AuthorAgent,
		TurnID: turn.RunID, Chain: turn.Chain,
	}, nil
}

// actor resolves who this call acts as — see [ChatDeps.Actor] — keyed for a
// write this call makes ([callOpKey]).
//
// A ROOM IS SPOKEN IN, AND READ, AS A SEAT, so an actor that names none is
// refused here, whichever surface built it: the authority table refuses a
// credential acting as itself every chat verb first (`unseated`), and this is
// the arm a surface wired without the table would otherwise reach — a login
// passed to the reader as a viewer reads every public room, and one passed to
// the writer is refused by the store in its own words.
func (d ChatDeps) actor(ctx context.Context, turn *turnctx.Turn, tool string,
	args map[string]any) (chatlet.Actor, *tools.Result) {

	var (
		actor chatlet.Actor
		err   error
	)
	if d.Actor != nil {
		actor, err = d.Actor(ctx, turn)
	} else {
		actor, err = chatActor(turn)
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

// chatActorFailure is what a call with nobody to act as reports.
//
// [tools.RefusalForbidden]: nothing about the arguments is wrong, and this
// caller — no seat behind it, no turn, no request a surface resolved — may
// not speak or read as anybody here. A seat with no turn is told the ordinary
// sentence; a request that carried nobody is told so.
func chatActorFailure(name string, err error) tools.Result {
	if errors.Is(err, turnctx.ErrNoSeat) {
		return notInATurn(name)
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
// one it did not — a condition of the node, a fault, an outcome that is not a
// refusal at all.
//
// THE ONE TABLE for both readers that need it: a chat tool's own failure, and
// the person's write surface, which reads every native domain's classes
// through [WriterClass]. Two lists would drift on exactly the sentinel a new
// gesture adds.
func chatClass(err error) (tools.Refusal, bool) {
	var invalid *chatlet.InvalidError
	switch {
	case errors.Is(err, chatlet.ErrNotFound):
		return tools.RefusalNotFound, true
	case errors.Is(err, chatlet.ErrForbidden):
		return tools.RefusalForbidden, true
	case errors.Is(err, chatlet.ErrNameTaken), errors.Is(err, chatlet.ErrRoomExists):
		return tools.RefusalExists, true
	case errors.Is(err, chatlet.ErrConflict), errors.Is(err, chatlet.ErrArchived),
		errors.Is(err, chatlet.ErrReactionsFull):
		// A LOST RACE, AND THE STATE OF THE ROOM OR THE MESSAGE THE CALL IS
		// ABOUT, on the one rule every domain's refusals take (internal/mcp's
		// [tools.Refusal]): an archived room and a message carrying all the
		// emoji it may refuse the same request until somebody reopens the
		// room or takes an emoji off, and only that makes it land — as an
		// artifact in the trash and a project archived before a task is
		// filed into it do. Classed `invalid`, the archive told a caller to
		// change an argument the call has nothing wrong with.
		return tools.RefusalConflict, true
	case errors.Is(err, chatlet.ErrUnknownKind):
		// THIS NODE'S CONDITION, NOT A PERMISSION: a newer peer made the
		// room and this build cannot say who may reach it. The same call
		// lands once this node is upgraded.
		return tools.RefusalUnavailable, true
	case errors.Is(err, chatlet.ErrInvalid), errors.As(err, &invalid):
		return tools.RefusalInvalid, true
	}
	return "", false
}

// chatWriteFailure explains a write that did not land, in terms the model can
// act on, classed for a reader that is not a model — the class stated beneath
// the store's own error ([refusedBy]), so a surface answering in status codes
// reads either.
//
// EACH SENTINEL HAS ITS OWN SENTENCE and its class from [chatClass]: a lost
// race, a name somebody else holds, a permission, a room this node cannot
// serve until it is upgraded each ask a different thing of the caller.
//
// THE UNMARKED REMAINDER IS THE NODE'S, as the knowledge base's is
// ([pageWriteFailure]): the chat store wraps every refusal it decides on what
// was asked in one of its own sentinels, so what reaches the end is a log that
// refused the append — a CONDITION, [tools.RefusalUnavailable] — or a read or
// an encode that broke, a FAULT ([faulted]), whose error is the log's.
//
// AND ONE ERROR IS NEITHER: a write the estate's router sent and never heard
// back about ([estate.ErrOutcomeUnknown]), which may have landed — an UNKNOWN
// outcome ([UnknownCause]), never "nothing was written".
//
// INVALID AND FORBIDDEN carry the store's own sentence ([chatlet.Sentence])
// without the package's Go prefix: the field or the rule it names is what the
// caller changes its call by.
//
// `next` is what this caller does about an UNKNOWN outcome — the sentence the
// tool's own unknown answer gives for the same call: [unknownNext] for a tool
// that adds, [restateNext] for one that states.
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
		// NOT "NOTHING WAS WRITTEN", for [writeFailure]'s reason: a fault
		// while the append resolved leaves a record nobody here can
		// account for.
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
		// THE DOMAIN'S SENTENCE NAMES THE ROOM AND THE TWO WAYS ON; what
		// this adds is where a seat finds the room that holds it.
		return refusedBy(class, err, fmt.Sprintf("%s: %s. A room's name is its "+
			"address, and "+ListChannelsTool+" names the rooms you are in.",
			name, chatlet.Sentence(err)))
	case class == tools.RefusalExists:
		return refusedBy(class, err, fmt.Sprintf("%s: %s.", name, chatlet.Sentence(err)))
	case class == tools.RefusalConflict:
		// A LOST RACE, in words that say which read comes before a
		// retry. A tool of another domain tells it in [statelog.LostRace]
		// ([WriterSentence], [chatlet.Sentence]), and a person's surface in
		// its code's own sentence (ADR-0060).
		return refusedBy(class, err, fmt.Sprintf("%s could not land: the room "+
			"kept changing under this write, so nothing was written. Read it again "+
			"before retrying.", name))
	}
	// A ROOM OR A MESSAGE THAT IS NOT THERE, and the advice names where each
	// is looked up. Either address reaches any room the caller may read,
	// joined or not ([ChatDeps.channelID]), so neither is offered as the
	// cure for the other: a seat told a name will not do goes looking for an
	// id it never needed.
	return refusedBy(class, err, fmt.Sprintf("%s: %s\n\nCheck what you named: "+
		ListChannelsTool+" names the rooms you are in, any room you may read answers "+
		"to its id or to its name, and "+ReadChannelTool+" shows each message's id.",
		name, chatlet.Sentence(err)))
}

// chatUnknown is the answer to a chat write whose outcome is unknown: it may
// have landed and it may not, so it is reported as neither — [pageUnknown]'s
// answer, for this surface, carrying [UnknownOutcome] as its cause.
//
// AN UNVOUCHED ONE SAYS SO, because it sends the caller somewhere else: this
// node's operation ledger may have lost the record of the operation, so the
// same operation asked here answers the same way every time. What a repeat IS
// is `next`'s to say.
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

// stated is the receipt a write that STATES something answers with — a
// reaction on or off, this seat in a room or out of it, a room's settings —
// or, where its outcome is unknown, the unknown answer.
//
// ITS REPEAT IS OFFERED RATHER THAN WARNED AGAINST ([restateNext]): stating
// the same thing again changes nothing if the first one landed, whichever
// operation the repeat is.
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
// `unknown` — a lost acknowledgement, an operation this node's ledger cannot
// vouch for (whose answer names no room at all), or a data node that never
// answered.
//
// # Why it stops there
//
// Because the post would go into a room this call cannot name. An unvouched
// open answers no id, so the message would be addressed to nothing and
// refused as "not found" — a different and misleading answer about a
// conversation that may well exist; a lost acknowledgement names the derived
// room, which this node may not have applied, and is refused the same way.
//
// # Why its advice is not [unknownNext]'s
//
// NOTHING WAS SAID: the message comes after the open and was never sent. So
// the hazard every other `unknown` here warns about — a repeat that is a new
// operation saying it twice — is not there: a direct conversation is its
// participants' whatever opens it, and the message has not been posted once.
//
// `why` is what made it unknown, already said, `opID` the open's operation and
// `unvouched` whether this node's ledger is what cannot tell.
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

// chatReadFailure explains a read that could not be served.
//
// IT NEVER SAYS "NOTHING FOUND", on [readFailure]'s reasoning: a node whose
// rows are behind must not be able to tell a seat the room is empty, because
// a seat that believes nobody answered asks again. A room the read itself
// reports absent is [tools.RefusalNotFound], a reader with no seat behind it is
// refused by the store, which is [tools.RefusalForbidden], and an argument the
// reader refused — a cursor that is no position, a thread named by nothing —
// is [tools.RefusalInvalid]: "try again" over a refused argument is a retry
// that is refused the same way every time. Anything else is the node's — a
// CONDITION waiting clears, or a FAULT whose error is the log's ([faulted]).
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
// once — and the store resolves it as one ([chatlet.Reader.ChannelNamed]): a
// public room, or a unit's room, resolves for whoever may read it, joined or
// not, and a private room only for its members. Resolved against the caller's
// own rooms instead, a unit's room an asker had promised a decision's outcome
// to was writable by id while its name meant nothing.
//
// # What a failure is
//
// A name nothing the viewer may read holds is the ARGUMENT's —
// [tools.RefusalInvalid], never not-found, because the fix is the argument and
// a private room answers exactly as a name nobody holds. A lookup this node
// could not serve is the read's failure ([chatReadFailure]): the name may be
// perfectly good, and nothing was written.
func (d ChatDeps) channelID(ctx context.Context, name, viewer, ref string) (
	string, *tools.Result) {

	// THE ROOM AS PEOPLE WRITE IT, by the one rule every surface reads a
	// typed room by ([chatlet.RoomRef]): its id, or its name with or without
	// the '#' before it.
	id, want := chatlet.RoomRef(ref)
	if id != "" {
		return id, nil
	}
	if want == "" {
		out := failed(name + " needs a `channel` — the room's id, or its name.")
		return "", &out
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
// it was admitted as MODERATION — on another's words — or as their author.
//
// # Two decisions, because the second needs the message
//
// The first is taken on the message nobody has read yet
// ([authz.Object.Unresolved]): admin is admitted there and then, which is
// moderation — and a moderator never has to read a message to take it down, so
// one in a private room or a direct conversation they are not in is reached
// without being shown. Anybody else is told [authz.ErrUnresolved]: read the
// message as this viewer — one they may not read is NOT FOUND — and decide
// again on whose words they are, which admits its author and nobody else.
//
// MODERATE CROSSES TO THE STORE AS THE DECISION, never re-derived there
// ([chatlet.MessageAuthority]): the serving node judges it against the author its
// own snapshot reads, because the read here was another transaction's.
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

// message builds what a post or a reply carries from the body the model wrote.
//
// THE ACTOR'S KEY DERIVES THE MESSAGE'S ID, which is also its operation's (see
// the head of chatlet/ids.go): in a turn, from the unit of work and this call's
// repeat count in the run, so a re-run posts each of the turn's messages once
// and a turn's second remark is never its first one's retry; under a request,
// from the key the transport carried, so a person's retry is the message they
// already sent. An operator's assistant over MCP names neither, and each of its
// posts is a new message — exactly as a person pressing send twice says it
// twice, since nothing redelivers that call.
func (d ChatDeps) message(body string, links []string) chatlet.NewMessage {
	in := chatlet.NewMessage{Body: body, Links: links, Collective: addressesRoom(body)}
	if d.Mentions != nil {
		in.Mentions = d.Mentions.Mentions(body)
	}
	return in
}

// linkShape is what each link a chat tool takes is, as its `links` says it:
// the store refuses any other ([chatlet.LinkSchemes]), and the description is
// where a model learns the rule before it writes rather than from a refusal.
const linkShape = "each an absolute http://, https:// or mailto: address written " +
	"whole, as https://example.com/page or mailto:name@example.com"

// posted is the receipt every posting tool answers with — or, for a post whose
// outcome is unknown, the unknown answer and never a receipt.
//
// # Why an unknown post is not a receipt
//
// Because the rest of a receipt is a claim. A `message_id` beside it is read
// as the message said, and the seat reports it said — when nothing may have
// been published at all: an operation this node's ledger cannot vouch for is
// answered `unknown` WITHOUT publishing. [unknownWrite]'s reasoning, for the
// same reader.
//
// `again` is how this caller repeats the post as the same message
// ([callRepeat]): a seat's same call, a person's same request, or nothing — an
// operator's assistant over MCP, whose repeat is a second message if the first
// one landed.
func posted(tool string, written chatlet.Written, channelID, again string) (tools.Result, error) {
	if written.Outcome.Outcome == statelog.OutcomeUnknown {
		return chatUnknown(tool, written.Outcome,
			postNext(written.Outcome.Unvouched, again, "Read the room with "+ReadChannelTool)), nil
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
		"answer something somebody said, use " + ReplyInThreadTool + " instead, which " +
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
					"at, %s. There are no file attachments.", chatlet.MaxLinks, linkShape),
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
	body := strings.TrimSpace(unfence(turn, argString(args, "body")))
	if body == "" {
		return failed(PostMessageTool + " needs a `body` — an empty message is a " +
			"wake for nothing."), nil
	}
	actor, id, refusal := t.deps.resolved(ctx, turn, PostMessageTool, args)
	if refusal != nil {
		return *refusal, nil
	}
	again := callRepeat(turn, actor.OpKey, PostMessageTool)
	written, err := t.deps.Writer.Post(ctx, actor, id,
		t.deps.message(body, argStrings(args, "links")))
	if err != nil {
		return chatWriteFailure(ctx, PostMessageTool, err,
			postNext(false, again, "Read the room with "+ReadChannelTool)), nil
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
	again := callRepeat(turn, actor.OpKey, ReplyInThreadTool)
	written, err := t.deps.Writer.Reply(ctx, actor, id, replyTo,
		t.deps.message(body, argStrings(args, "links")))
	if err != nil {
		return chatWriteFailure(ctx, ReplyInThreadTool, err,
			postNext(false, again, "Read the room with "+ReadChannelTool)), nil
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
					"people can join by.", chatlet.MaxDMParticipants),
			},
			"body": map[string]any{"type": "string", "description": "What to say."},
			"links": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": fmt.Sprintf("Up to %d URLs this message points "+
					"at, %s. There are no file attachments.", chatlet.MaxLinks, linkShape),
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
	body := strings.TrimSpace(unfence(turn, argString(args, "body")))
	// A BARE STRING IS ACCEPTED BESIDE THE ARRAY, because "send a DM to
	// alice" is one recipient and a model writing `"to": "alice"` means
	// exactly what the one-element array means. [argStrings] already reads
	// both.
	to := argStrings(args, "to")
	switch {
	case body == "":
		return failed(SendDMTool + " needs a `body`."), nil
	case len(to) == 0:
		return failed(SendDMTool + " needs a `to` — the handle of the colleague you " +
			"are writing to. Use " + LookupColleagueTool + " if you are unsure of it."), nil
	}
	// ONE KEY FOR BOTH APPENDS, read once: the open and the post are two
	// gestures with two verbs, so the store derives two operations from it,
	// and reading the count once is what keeps them this one call's.
	actor, refusal := t.deps.actor(ctx, turn, SendDMTool, args)
	if refusal != nil {
		return *refusal, nil
	}
	again := callRepeat(turn, actor.OpKey, SendDMTool)
	said := t.deps.message(body, argStrings(args, "links"))
	// WHAT THE MESSAGE SAYS IS REFUSED BEFORE ANYTHING IS WRITTEN. A body
	// over the cap is refused by the post however the room is found, so
	// opening the conversation first only left a room in every
	// participant's list, opened by a refusal. What the post decides from
	// the room itself is still the post's — see [directPostFailure].
	if rejected := said.Validate(); rejected != nil {
		// A REFUSAL OF CONTENT, never unknown: nothing was sent anywhere.
		return chatWriteFailure(ctx, SendDMTool, rejected, ""), nil
	}
	room, _, err := t.deps.Writer.OpenDirect(ctx, actor, to)
	if errors.Is(err, estate.ErrOutcomeUnknown) {
		// AN OPEN NOBODY ANSWERED is [directUnknown]'s case — no room this
		// call can name, so nothing is posted into one — whichever way it
		// came back unknown.
		return directUnknown(to, Unanswered, "", false, again), nil
	}
	if err != nil {
		return chatWriteFailure(ctx, SendDMTool, err, ""), nil
	}
	if room.Outcome.Outcome == statelog.OutcomeUnknown {
		return directUnknown(to, unknownWhy("the open's", room.Outcome),
			room.Outcome.OpID, room.Outcome.Unvouched, again), nil
	}
	// AND SETTLE IT BEFORE SAYING ANYTHING IN IT. The post decides from
	// the rows of whichever copy takes it — it reads the room to route the
	// message — so a post decided before that copy applied the create finds
	// no room at all and is refused for a conversation that exists. Settled,
	// the open's position is the floor the post's own decision waits for.
	t.deps.settle(ctx, room.Outcome.Position)

	written, err := t.deps.Writer.Post(ctx, actor, room.Channel.ID, said)
	if err != nil {
		return directPostFailure(ctx, to, room, err, again), nil
	}
	t.deps.settle(ctx, written.Outcome.Position)
	if written.Outcome.Outcome == statelog.OutcomeUnknown {
		// THE OPEN LANDED AND THE ANSWER SAYS SO, as a refused post's does:
		// two records, and only the second one is in doubt.
		return failedUnknown(written.Outcome.OpID, written.Outcome.Unvouched,
			directOpen(to, room)+chatUnknownText(SendDMTool+"'s message",
				written.Outcome, postNext(written.Outcome.Unvouched, again,
					"Read the conversation with "+ReadChannelTool))), nil
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
func directOpen(to []string, room chatlet.Written) string {
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
// AN UNKNOWN POST STAYS UNKNOWN, and it is asked before anything is said in the
// node's class, because an unknown outcome IS that class: a post sent to a data
// node that never answered may have been said. Rewritten as "was not said", it
// told the caller the one thing nobody can know and invited the second message
// the router refused to risk — and dropped the cause a person's surface tells
// "nothing happened" from "may have happened" by.
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
		// THE CONDITION'S NEXT STEP TOO, which this sentence replaces
		// [chatWriteFailure]'s along with the rest of it: a direct message
		// refused on a full log was told what happened and nothing to do.
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
		"oldest first, by naming the message it hangs off. A thread you were " +
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

// fenceMessage fences what one message says, under its author, in the message
// view at path: the transcript records the author's kind beside the handle,
// which the fence takes at its word. A tombstone says nothing and is not
// fenced; its row carries no body.
//
// A ROOM EVENT says nothing either, and carries words all the same: the topic
// or description it records, or a template's name, which its author wrote as
// the room's label. They are fenced under that author, read from the room as
// a topic is.
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
		// REFUSED, NOT CUT, at the bound every ranked search shares
		// ([knowledge.MaxQueryBytes]) — and BEFORE THE ROUTER, because the
		// index's own refusal crosses to this node as words alone and would
		// read as chat being unreadable; this says what to send instead.
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return failed(fmt.Sprintf(SearchMessagesTool+"' `text` is %d bytes and takes at "+
			"most %d — search on a few words somebody would have said, not a "+
			"pasted passage.", len(text), knowledge.MaxQueryBytes)), nil
	}
	actor, refusal := t.deps.actor(ctx, turn, SearchMessagesTool, args)
	if refusal != nil {
		return *refusal, nil
	}
	// THE ROOM IS RESOLVED ONLY WHEN ONE WAS NAMED, and a name that resolves
	// to nothing is REFUSED rather than dropped: searching every room in its
	// place would hand back hits from rooms the caller did not mean, read as
	// the one it did. An id is never refused here — the search intersects it
	// with what the viewer may read ([estate.ChatQuery.Channel]).
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
			// NOT "NOTHING MATCHED": the index this node answered from was
			// still catching up, so the message may be there and not yet
			// indexed — a seat told nobody said it acts on that.
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
		// WHAT THE MESSAGE SAYS, under the writer its row records — a
		// snippet is the message itself, cut only where it is long.
		read.fence(fieldPath{}.key("messages").index(i).key("snippet"),
			provenance.SourceChatThread,
			[]provenance.Author{read.writer(hit.Author, iam.ActorKind(hit.AuthorKind))},
			false, hit.Snippet)
	}
	out := map[string]any{"count": len(rows), "messages": rows}
	if answer.Building {
		// SAID BESIDE THE HITS for the same reason: what came back is what
		// the index had reached, not necessarily all there is.
		out["building"] = true
	}
	return read.answer(ctx, SearchMessagesTool, out,
		"Narrow it with `channel`, `author` or a shorter `limit`.")
}

// chatHitRow is one search hit as this tool renders it.
//
// A PROJECTION rather than the index's own value, which carries no JSON
// names: handed over whole it keyed a hit `MessageID` and `ThreadRoot` beside
// every other chat answer's `message_id` and `thread_root`, so a seat reading
// one to reply under it met the id under a key no other tool spells. The keys
// are the transcript's own — `read_channel` answers a message's room as
// `channel_id` — and the instant is `at`, as the API's search answers it.
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

// channelRow is one room as this tool renders it.
//
// A PROJECTION rather than the reader's own summary, for one field: the rail
// is built for a screen and carries an UNREAD COUNT, which is a fact about a
// person's attention. A seat has none — see [ChatDeps.ReadState] — and
// handing a model "99+ unread" for a room it was never reading sends it off
// to catch up on a backlog that is not its work.
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
		return chatReadFailure(ctx, ListChannelsTool, err), nil
	}
	if len(listing.Channels) == 0 {
		return tools.Result{Output: "You are in no chat rooms."}, nil
	}
	read := newForeignRead(turn, actor.Name())
	rows := make([]channelRow, 0, len(listing.Channels))
	for i, room := range listing.Channels {
		at := fieldPath{}.key("channels").index(i)
		// WHAT THE ROOM SAYS ABOUT ITSELF is somebody's words too — a
		// person sets a topic and a purpose, and the room keeps no record
		// of which person — so each is fenced as a room's label written by
		// writers the row does not name: a colleague's, since only the
		// company's own principals set one, and never this seat's own.
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
		// SAID, rather than left to a reader noticing every count is
		// missing: no counts and nothing unread are different facts. In
		// fixed words, because the coordination store's error names its
		// transport, and under the rail's own name for the fact — whether
		// the read state was measured.
		why := "the coordination store could not be read"
		if words, ok := Condition(unreadable); ok {
			why = words
		}
		out["read_state"] = fmt.Sprintf("unknown: how far you have read could "+
			"not be fetched (%s), so no room carries a count of what is new, "+
			"and none is marked muted", why)
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
