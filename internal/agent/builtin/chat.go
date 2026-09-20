package builtin

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/chat"
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

// ChatTools is the whole native catalogue, so a caller registering them names
// one thing.
func ChatTools() []string { return chat.Tools() }

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
}

// ChatWriter is what these tools need from chat's write side.
//
// THE ACTOR IS A PARAMETER rather than the seam being resolved per actor, and
// that is the domain's own shape rather than a choice made here: one node's
// chat serves every seat and every operator through one write path, so the
// party a write acts as travels per call. It is the wiki's arrangement, not
// the tracker's — see [PageWriter].
type ChatWriter interface {
	Post(ctx context.Context, actor chat.Actor, channelID string,
		in chat.NewMessage) (chat.Written, error)
	Reply(ctx context.Context, actor chat.Actor, channelID, replyTo string,
		in chat.NewMessage) (chat.Written, error)
	// OpenDirect reports whether it CREATED the conversation beside the
	// room itself, because "it was already open" is not a failure here —
	// the id is derived from the participants, so the loser of a race is
	// handed the room the winner made.
	OpenDirect(ctx context.Context, actor chat.Actor, participants []string) (
		chat.Written, bool, error)
	React(ctx context.Context, actor chat.Actor, channelID, messageID,
		emoji string) (chat.Written, error)
	Unreact(ctx context.Context, actor chat.Actor, channelID, messageID,
		emoji string) (chat.Written, error)
	Join(ctx context.Context, actor chat.Actor, channelID string) (chat.Written, error)
	Leave(ctx context.Context, actor chat.Actor, channelID string) (chat.Written, error)
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

	// ReadState is how far this viewer has read in each room, keyed on
	// channel id against a packed position — [chat.ChannelsQuery.Cursors].
	//
	// NIL IS THE ORDINARY CASE AND IT IS NOT AN ABSENCE OF DATA. A read
	// cursor is a PERSON's attention, written when somebody looks at a
	// room; a seat is woken rather than browsing and has none. With this
	// nil the listing carries no unread counts at all, which is the honest
	// answer — the alternative is reporting every room's whole tail as
	// unread, and a model told it has ninety-nine unread messages in a
	// room it was never reading will try to catch up on them.
	ReadState func(ctx context.Context, viewer string) map[string]int64

	// Actor decides who a write is attributed to, and — because a private
	// room's contents are decided by its membership — who a READ is served
	// as. Nil takes the turn's seat; the operator surface sets it. See
	// [WorkDeps.Actor] for why this is a seam rather than a second copy of
	// these tools.
	Actor func(ctx context.Context, turn *turnctx.Turn) (chat.Actor, error)

	// Await blocks until this node's applier has consumed a write. See
	// [WorkDeps.Await]: same seam, same reason, and on this surface it is
	// the difference between a seat seeing its own message and posting it
	// twice.
	Await func(ctx context.Context, at statelog.Position) error
}

// settle waits for a write to reach this node's own applied rows.
//
// APPLIED AFTER EVERY WRITE AND BEFORE NO READ. A write lands on the fleet's
// log and every read here is served from this node's own rows, so a seat that
// posts and then reads the room in the same turn must be made to see its own
// message — a model that cannot find what it just said says it again. Before
// a read it would buy nothing: there is no position to wait for that the
// caller did not already produce.
//
// Best effort, and a failure is LOGGED AND IGNORED: the message landed, and
// telling a model its post failed when it is in the room is the one answer
// that produces the duplicate.
func (d ChatDeps) settle(ctx context.Context, at statelog.Position) {
	if d.Await == nil || at.Seq == 0 {
		return
	}
	if err := d.Await(ctx, at); err != nil {
		log.WarnContext(ctx, "chat_write_not_applied_yet",
			"at", at.String(), "error", err.Error(),
			"detail", "the message landed on the fleet's log; this node's own "+
				"copy has not caught up, so reading the room in this same turn "+
				"may not show it yet")
	}
}

// chatActor builds the write's attribution from the TURN, never from
// arguments: a model that could name its own author could speak as anybody,
// and a room where that is possible is not a conversation.
//
// THE TURN ID IS THE OPERATION SEED rather than the run id, and that is what
// makes a re-run turn post once. A message's id is derived from (turn,
// channel, ordinal) and a redelivered trigger runs again under a NEW run id —
// so seeding from the run would make every re-run a second copy of everything
// the turn said. Which of a turn's two identities an idempotent id is built
// from is one rule with several callers, which is why it goes through
// [turnKey] rather than being spelled again here.
func chatActor(turn *turnctx.Turn) (chat.Actor, error) {
	seat, err := turn.RequireSeat()
	if err != nil {
		return chat.Actor{}, err
	}
	return chat.Actor{
		Handle: seat.Handle(), Kind: chat.AuthorAgent,
		TurnID: turnKey(turn), Chain: turn.Chain,
	}, nil
}

// actor resolves who this call acts as — see [ChatDeps.Actor].
func (d ChatDeps) actor(ctx context.Context, turn *turnctx.Turn) (chat.Actor, error) {
	if d.Actor != nil {
		return d.Actor(ctx, turn)
	}
	return chatActor(turn)
}

// chatActorFailure is what a call with no author reports.
//
// THE SEAM'S OWN REFUSAL IS PASSED THROUGH, unlike the wiki's and the
// tracker's, because on this surface it carries the remedy: an operator token
// bound to no seat is refused naming `contact.crewlet_operator_id`, and
// flattening that into "only during a turn" would send a person looking for a
// turn they do not have. A seat with no turn still gets the ordinary
// sentence, which is the only thing that failure means there.
func chatActorFailure(name string, err error) tools.Result {
	if errors.Is(err, turnctx.ErrNoSeat) {
		return notInATurn(name)
	}
	return failed(name + " has nobody to act as: " + err.Error())
}

func unconfiguredChat(name string) tools.Result {
	return failed(name + " is unavailable: this company does not run native " +
		"chat. Use the chat tools your company has configured.")
}

// chatWriteFailure explains a write that did not land, in terms the model can
// act on.
func chatWriteFailure(name string, err error) string {
	switch {
	case errors.Is(err, chat.ErrInvalid):
		return fmt.Sprintf("%s refused that: %v", name, err)
	case errors.Is(err, chat.ErrNotFound):
		return fmt.Sprintf("%v\n\nCheck the room with list_channels — a room "+
			"you are not in is addressed by its id rather than by its name.", err)
	case errors.Is(err, chat.ErrArchived):
		return fmt.Sprintf("%v\n\nNothing more can be said there. Say it "+
			"somewhere that is still open, or say nothing.", err)
	case errors.Is(err, chat.ErrForbidden):
		return fmt.Sprintf("%v\n\nThis is a permission, not a retry: asking "+
			"again will be refused the same way.", err)
	case errors.Is(err, chat.ErrConflict):
		return fmt.Sprintf("%s could not land: %v. The room kept changing "+
			"under this write — read it again before retrying.", name, err)
	}
	return fmt.Sprintf("%s did not land (%v). Nothing was said — do not report "+
		"it as done.", name, err)
}

// chatReadFailure explains a read that could not be served.
//
// IT NEVER SAYS "NOTHING FOUND", on [readFailure]'s reasoning: a node whose
// rows are behind must not be able to tell a seat the room is empty, because
// a seat that believes nobody answered asks again.
func chatReadFailure(name string, err error) string {
	if errors.Is(err, chat.ErrNotFound) {
		return fmt.Sprintf("%v\n\nCheck the room with list_channels. A private "+
			"room you are not in answers the same way as one that does not "+
			"exist.", err)
	}
	return fmt.Sprintf("%s could not read the company's chat right now (%v). "+
		"This is NOT an empty room — do not conclude nothing was said. Try "+
		"again, or say you could not check.", name, err)
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
// rooms. The listing is bounded ([chat.MaxRailChannels]) and this only runs
// when the argument is not already an id, so the common case — a room id
// carried in from the wake that woke this seat — costs nothing.
//
// # And why only the rooms this viewer is in
//
// That is the whole of what the rail can answer: a public room nobody here
// has joined is visible but not listed, so it has no name to match against.
// Addressing one by id is the honest remaining path, and the refusal says so
// rather than reporting that the room does not exist.
func (d ChatDeps) channelID(ctx context.Context, viewer, ref string) (string, error) {
	// A LEADING '#' IS HOW PEOPLE WRITE A ROOM and never part of a name —
	// [chat.ValidName] admits neither it nor any capital — so stripping it
	// here cannot collide with a room somebody really called that.
	want := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ref), "#"))
	if want == "" {
		return "", errors.New("names no channel")
	}
	if _, err := uuid.Parse(want); err == nil {
		return want, nil
	}
	if d.Reader == nil {
		return "", fmt.Errorf("cannot resolve the name %q without a reader", clip(want))
	}
	// ARCHIVED ROOMS INCLUDED, deliberately: a read of one is legitimate
	// and a write to one is refused by the store naming the room, which is
	// a better answer than "no such channel".
	listing, err := d.Reader.Channels(ctx, viewer,
		chat.ChannelsQuery{IncludeArchived: true}, seatRead)
	if err != nil {
		return "", err
	}
	for _, room := range listing.Channels {
		if strings.EqualFold(room.Channel.Name, want) {
			return room.Channel.ID, nil
		}
	}
	return "", fmt.Errorf("there is no room called %q among the ones you are "+
		"in — list_channels names them, and a room you are not in is "+
		"addressed by its id", clip(want))
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
	id, err := d.channelID(ctx, actor.Name(), ref)
	if err != nil {
		out := failed(fmt.Sprintf("%s %v", name, err))
		return chat.Actor{}, "", &out
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
// wrote.
//
// # Which idempotency rule this message takes
//
// The store picks it from what the actor can prove, and this is the caller's
// half of that choice — see the head of chat/ids.go.
//
// A TURN supplies an ORDINAL. Its id is derived from (turn, channel, ordinal)
// so a re-run of one turn posts each of its messages once, and the ordinal is
// what keeps a turn's second remark from overwriting its first.
//
// AN OPERATOR SUPPLIES A FRESH KEY, because there is nothing else honest to
// supply. Their assistant made one MCP call, nothing redelivers it, and a key
// derived from the words would collapse a person who said "ping" twice on
// purpose into one message. The cost is stated rather than hidden: a client
// that retries after a lost answer posts twice, exactly as a person pressing
// send twice does, because the caller brought no submission key of its own.
func (d ChatDeps) message(turn *turnctx.Turn, actor chat.Actor, body string,
	links []string) chat.NewMessage {

	in := chat.NewMessage{Body: body, Links: links, Collective: addressesRoom(body)}
	if actor.TurnID != "" {
		// THE ORDINAL NUMBERS THE GESTURE, not the tool: all three
		// posting tools derive ids in one space, so a room post and a
		// thread reply in one turn must not both be call zero. See
		// [turnctx.Turn.CallOrdinal].
		in.Ordinal = turn.CallOrdinal(chat.WriteTools()...)
	} else {
		in.OperationID = uuid.NewString()
	}
	if d.Mentions != nil {
		in.Mentions = d.Mentions.Mentions(body)
	}
	return in
}

// posted is the receipt every posting tool answers with.
func posted(written chat.Written, channelID string) (tools.Result, error) {
	out := map[string]any{
		"message_id": written.ChangeID, "channel": channelID,
		"revision": written.Revision,
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
	return jsonResult(out)
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
				"description": "The room: its id, or its name if you are in " +
					"it (\"#eng\" and \"eng\" are the same room).",
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
	body := strings.TrimSpace(argString(args, "body"))
	if body == "" {
		return failed("post_message needs a `body` — an empty message is a " +
			"wake for nothing."), nil
	}
	actor, id, refused := t.deps.resolved(ctx, turn, PostMessageTool, argString(args, "channel"))
	if refused != nil {
		return *refused, nil
	}
	written, err := t.deps.Writer.Post(ctx, actor, id,
		t.deps.message(turn, actor, body, argStrings(args, "links")))
	if err != nil {
		return failed(chatWriteFailure(PostMessageTool, err)), nil
	}
	t.deps.settle(ctx, written.Outcome.Position)
	return posted(written, id)
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
				"description": "The room: its id, or its name if you are in it.",
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
	body := strings.TrimSpace(argString(args, "body"))
	replyTo := strings.TrimSpace(argString(args, "reply_to"))
	switch {
	case body == "":
		return failed("reply_in_thread needs a `body`."), nil
	case replyTo == "":
		return failed("reply_in_thread needs a `reply_to` — the id of the " +
			"message you are answering. To start a new topic, use post_message."), nil
	}
	actor, id, refused := t.deps.resolved(ctx, turn, ReplyInThreadTool, argString(args, "channel"))
	if refused != nil {
		return *refused, nil
	}
	written, err := t.deps.Writer.Reply(ctx, actor, id, replyTo,
		t.deps.message(turn, actor, body, argStrings(args, "links")))
	if err != nil {
		return failed(chatWriteFailure(ReplyInThreadTool, err)), nil
	}
	t.deps.settle(ctx, written.Outcome.Position)
	return posted(written, id)
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

	room, _, err := t.deps.Writer.OpenDirect(ctx, actor, to)
	if err != nil {
		return failed(chatWriteFailure(SendDMTool, err)), nil
	}
	// AND WAIT FOR IT BEFORE SAYING ANYTHING IN IT. The post decides from
	// this node's own applied rows — it reads the room to route the
	// message — so a post issued before the create has been applied here
	// finds no room at all and is refused for a conversation that exists.
	t.deps.settle(ctx, room.Outcome.Position)

	written, err := t.deps.Writer.Post(ctx, actor, room.Channel.ID,
		t.deps.message(turn, actor, body, argStrings(args, "links")))
	if err != nil {
		return failed(chatWriteFailure(SendDMTool, err)), nil
	}
	t.deps.settle(ctx, written.Outcome.Position)
	return posted(written, room.Channel.ID)
}

// ---- read_channel ------------------------------------------------------ //

type readChannel struct{ deps ChatDeps }

var _ tools.SeatCallable = (*readChannel)(nil)

func (t *readChannel) Name() string { return ReadChannelTool }

func (t *readChannel) Description() string {
	return "Read what was said in a room, newest first — or one thread in it, " +
		"oldest first, by naming the message it hangs off. Read before you " +
		"answer: the message you were woken about is here in full, but the " +
		"conversation around it is not."
}

func (t *readChannel) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"channel": map[string]any{
				"type":        "string",
				"description": "The room: its id, or its name if you are in it.",
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
	actor, id, refused := t.deps.resolved(ctx, turn, ReadChannelTool, argString(args, "channel"))
	if refused != nil {
		return *refused, nil
	}
	limit := argInt(args, "limit", 0)
	cursor := strings.TrimSpace(argString(args, "cursor"))

	if root := strings.TrimSpace(argString(args, "thread")); root != "" {
		got, err := t.deps.Reader.Thread(ctx, actor.Name(), chat.ThreadQuery{
			ChannelID: id, RootID: root, Cursor: cursor, Limit: limit,
		}, seatRead)
		if err != nil {
			return failed(chatReadFailure(ReadChannelTool, err)), nil
		}
		return jsonAnswer(got, "Ask for fewer messages with `limit`.")
	}
	got, err := t.deps.Reader.Messages(ctx, actor.Name(), chat.TranscriptQuery{
		ChannelID: id, Cursor: cursor, Limit: limit,
	}, seatRead)
	if err != nil {
		return failed(chatReadFailure(ReadChannelTool, err)), nil
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
				"description": "What was said, in plain words. Not a query " +
					"language.",
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
	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		return chatActorFailure(SearchMessagesTool, err), nil
	}
	// THE ROOM IS RESOLVED ONLY WHEN ONE WAS NAMED. An unresolvable name
	// here is not a refusal — the search is over every room the viewer can
	// read, and narrowing to a room they cannot name is a filter they can
	// simply drop.
	room := ""
	if named := strings.TrimSpace(argString(args, "channel")); named != "" {
		room, err = t.deps.channelID(ctx, actor.Name(), named)
		if err != nil {
			return failed(fmt.Sprintf("search_messages %v", err)), nil
		}
	}
	hits, err := t.deps.Search.SearchMessages(ctx, actor.Name(), ChatSearch{
		Text:    text,
		Channel: room,
		Author:  strings.TrimSpace(argString(args, "author")),
		Limit:   argInt(args, "limit", 0),
	})
	if err != nil {
		return failed(chatReadFailure(SearchMessagesTool, err)), nil
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
		"each is for and the last thing said in it. This is where a room's " +
		"id comes from when you only know its name."
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
	if t.deps.ReadState != nil {
		q.Cursors = t.deps.ReadState(ctx, actor.Name())
	}
	listing, err := t.deps.Reader.Channels(ctx, actor.Name(), q, seatRead)
	if err != nil {
		return failed(chatReadFailure(ListChannelsTool, err)), nil
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
			Last: room.Last,
		}
		if t.deps.ReadState != nil {
			unread := room.Unread
			row.Unread, row.UnreadCapped = &unread, room.UnreadCapped
		}
		rows = append(rows, row)
	}
	out := map[string]any{"count": len(rows), "channels": rows}
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
				"description": "The room: its id, or its name if you are in it.",
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
	messageID := strings.TrimSpace(argString(args, "message"))
	emoji := strings.TrimSpace(argString(args, "emoji"))
	switch {
	case messageID == "":
		return failed("react_to_message needs a `message` — the id of the " +
			"message you are reacting to."), nil
	case emoji == "":
		return failed("react_to_message needs an `emoji`."), nil
	}
	actor, id, refused := t.deps.resolved(ctx, turn, ReactToMessageTool, argString(args, "channel"))
	if refused != nil {
		return *refused, nil
	}
	remove := argBool(args, "remove")
	react := t.deps.Writer.React
	if remove {
		react = t.deps.Writer.Unreact
	}
	written, err := react(ctx, actor, id, messageID, emoji)
	if err != nil {
		return failed(chatWriteFailure(ReactToMessageTool, err)), nil
	}
	t.deps.settle(ctx, written.Outcome.Position)
	return jsonResult(map[string]any{
		"channel": id, "message_id": messageID, "emoji": emoji,
		"removed": remove, "revision": written.Revision,
	})
}

// ---- join_channel / leave_channel -------------------------------------- //

type joinChannel struct{ deps ChatDeps }

var _ tools.SeatCallable = (*joinChannel)(nil)

func (t *joinChannel) Name() string { return JoinChannelTool }

func (t *joinChannel) Description() string {
	return "Join a room, so that what is said there reaches you. Only yours " +
		"to change: this adds you and nobody else. A private room is not " +
		"joinable — somebody already in it adds you."
}

func (t *joinChannel) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"channel": map[string]any{
				"type": "string",
				"description": "The room's id — a room you are not in has no " +
					"name you can address it by.",
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
	actor, id, refused := t.deps.resolved(ctx, turn, JoinChannelTool, argString(args, "channel"))
	if refused != nil {
		return *refused, nil
	}
	written, err := t.deps.Writer.Join(ctx, actor, id)
	if err != nil {
		return failed(chatWriteFailure(JoinChannelTool, err)), nil
	}
	t.deps.settle(ctx, written.Outcome.Position)
	return jsonResult(map[string]any{
		"channel": id, "name": written.Channel.Name, "joined": true,
		"revision": written.Revision,
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
	actor, id, refused := t.deps.resolved(ctx, turn, LeaveChannelTool, argString(args, "channel"))
	if refused != nil {
		return *refused, nil
	}
	written, err := t.deps.Writer.Leave(ctx, actor, id)
	if err != nil {
		return failed(chatWriteFailure(LeaveChannelTool, err)), nil
	}
	t.deps.settle(ctx, written.Outcome.Position)
	return jsonResult(map[string]any{
		"channel": id, "name": written.Channel.Name, "left": true,
		"revision": written.Revision,
	})
}
