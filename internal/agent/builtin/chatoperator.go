package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// The chat gestures a PERSON makes, which no seat's registry holds.
//
// # Why they exist as tools at all
//
// Because the dashboard writes nothing of its own (ADR-0024): every change a
// person makes from it is `POST /operator/act/{tool}`, one tool of the operator
// catalogue per request, made as the person the token is bound to — so a room
// a person creates, a message they edit or take back, a conversation they open
// and how far they have read are each a tool here, served to the act transport
// and to that person's own assistant alike. The same seam carries every one of
// them ([ChatDeps]).
//
// # Who may
//
// THE AUTHORITY TABLE DECIDES WHO, and the store decides WHICH ROOM. Every one
// is a person's verb, so the table refuses a seat and a credential acting as
// itself before anything is read. What the arguments cannot say is decided
// after a read: whose words a message is ([ChatDeps.remarkAuthority]), and
// who made a room and which unit holds it, which is what its archive turns on
// ([updateChannel]). The rest is the store's, the same rules a seat meets: only
// an author edits, a private room refuses whoever is outside it, a unit's room
// is the org chart's.
//
// # Why no seat is given them
//
// Each for its own reason, written beside its name in internal/chat's
// vocabulary ([chat.OperatorOnlyTools]). In short: a seat corrects itself by
// saying so rather than rewriting the record, a room and its membership are a
// company's furniture and a private room's readership, and a read cursor is a
// person's attention.
//
// # How a repeat is the same gesture
//
// Through the call's key ([callOpKey]), exactly as for the seat tools: under
// the act transport the key is the one the request carried, so a person's
// retry after an `unknown` is the first attempt's operation and is answered
// with what landed; over MCP each call is a new operation. The one exception
// is [markChatRead], which is not a record on the log and needs no operation —
// a cursor only advances and a mute or an unmute states one fact about one
// room, so the same flush twice leaves the same state.

// ---- create_channel ---------------------------------------------------- //

type createChannel struct{ deps ChatDeps }

var _ tools.SeatCallable = (*createChannel)(nil)

func (t *createChannel) Name() string { return chat.CreateChannelTool }

func (t *createChannel) Description() string {
	return "Make a named room. Its name is its address, so two people cannot " +
		"make the same one and there is no renaming it afterwards. You are in " +
		"the room you make. A unit's own room is the org chart's and cannot be " +
		"made here."
}

func (t *createChannel) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			// `channel`, AS EVERY CHAT TOOL NAMES ITS ROOM: a room's name
			// is its address, and the room this makes answers to it from
			// the moment it exists — so a failed create names the room a
			// caller looks up to see what became of it, as every other
			// failed chat write does.
			"channel": map[string]any{
				"type": "string",
				"description": fmt.Sprintf("The new room's name, which is its "+
					"address: lower-case letters, digits and hyphens, at most "+
					"%d bytes (\"#launch\" and \"launch\" are the same name).",
					chat.MaxChannelName),
			},
			"kind": map[string]any{
				"type": "string",
				"enum": []any{string(chat.KindPublic), string(chat.KindPrivate)},
				"description": "Who may read it: public is anybody in the " +
					"company, private only its members. Omitted takes the " +
					"company's default.",
			},
			"topic":   map[string]any{"type": "string", "description": "What is being discussed now."},
			"purpose": map[string]any{"type": "string", "description": "What the room is for."},
			"members": membersSchema("The founding members beside you — for a " +
				"private room the only way anybody else gets in."),
		},
		"required": []any{"channel"},
	}
}

func (t *createChannel) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *createChannel) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	const name = chat.CreateChannelTool
	if t.deps.Writer == nil {
		return unconfiguredChat(name), nil
	}
	room := strings.TrimPrefix(strings.TrimSpace(argString(args, "channel")), "#")
	if room == "" {
		return failed(chat.CreateChannelTool + " needs a `channel` — the new room's name, " +
			"which is its address."), nil
	}
	members, refusal := argMembers(name, args, "members")
	if refusal != nil {
		return *refusal, nil
	}
	actor, refusal := t.deps.actor(ctx, turn, name, args)
	if refusal != nil {
		return *refusal, nil
	}
	in := chat.NewChannel{
		Name: room, Kind: chat.Kind(strings.TrimSpace(argString(args, "kind"))),
		Topic:   strings.TrimSpace(argString(args, "topic")),
		Purpose: strings.TrimSpace(argString(args, "purpose")),
		Members: members,
	}
	again := callRepeat(turn, actor.OpKey, name)
	// A ROOM NOBODY CAN SAY WAS MADE IS NOT ONE TO REPORT: the id below
	// would be a create's that may never have landed. Looking first is safe
	// either way — a second create of the same name is refused because the
	// name is taken.
	next := func(unvouched bool) string {
		return unknownNext(unvouched, again, "List the rooms with "+ListChannelsTool,
			"the second is refused because the name is taken")
	}
	written, err := t.deps.Writer.CreateChannel(ctx, actor, in)
	if err != nil {
		return chatWriteFailure(ctx, name, err, next(false)), nil
	}
	if written.Outcome.Outcome == statelog.OutcomeUnknown {
		return chatUnknown(name, written.Outcome, next(written.Outcome.Unvouched)), nil
	}
	t.deps.settle(ctx, written.Outcome.Position)
	return jsonResult(map[string]any{
		"channel": written.Channel.ID, "name": written.Channel.Name,
		"kind": written.Channel.Kind, "revision": written.Revision,
		"outcome":  outcomeOf(written.Outcome),
		"position": positionOf(written.Outcome.Position),
	})
}

// ---- open_dm ----------------------------------------------------------- //

type openDM struct{ deps ChatDeps }

var _ tools.SeatCallable = (*openDM)(nil)

func (t *openDM) Name() string { return chat.OpenDMTool }

func (t *openDM) Description() string {
	return "Open the private conversation between you and the people named, " +
		"or reach the one already open — without saying anything in it yet. " +
		"A conversation is its participants: opening it twice is one room."
}

func (t *openDM) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"to": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": fmt.Sprintf("The handles of the others, up to %d "+
					"including you. You are added yourself.", chat.MaxDMParticipants),
			},
		},
		"required": []any{"to"},
	}
}

func (t *openDM) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *openDM) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	const name = chat.OpenDMTool
	if t.deps.Writer == nil {
		return unconfiguredChat(name), nil
	}
	to := argStrings(args, "to")
	if len(to) == 0 {
		return failed(chat.OpenDMTool + " needs a `to` — the handles of the people the " +
			"conversation is with."), nil
	}
	actor, refusal := t.deps.actor(ctx, turn, name, args)
	if refusal != nil {
		return *refusal, nil
	}
	// STATED, NOT POSTED: the conversation is its participants' whatever
	// opens it, so opening it again is the same room and changes nothing.
	const look = "List your rooms with " + ListChannelsTool
	room, created, err := t.deps.Writer.OpenDirect(ctx, actor, to)
	if err != nil {
		return chatWriteFailure(ctx, name, err, restateNext(look)), nil
	}
	t.deps.settle(ctx, room.Outcome.Position)
	return stated(name, room, look, map[string]any{
		"channel": room.Channel.ID, "kind": room.Channel.Kind, "created": created,
	})
}

// ---- update_channel ---------------------------------------------------- //

type updateChannel struct{ deps ChatDeps }

var _ tools.SeatCallable = (*updateChannel)(nil)

func (t *updateChannel) Name() string { return chat.UpdateChannelTool }

func (t *updateChannel) Description() string {
	return "Change a room's settings: its topic, its purpose, whether it is " +
		"archived. Only what you name changes. An archived room is still read " +
		"and searched and takes no new messages; archiving it, or opening it " +
		"again, is its creator's, its unit's lead's or an admin's."
}

func (t *updateChannel) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"channel": map[string]any{
				"type":        "string",
				"description": "The room: its id, or its name.",
			},
			"topic": map[string]any{
				"type":        "string",
				"description": "What is being discussed now. An empty string clears it.",
			},
			"purpose": map[string]any{
				"type":        "string",
				"description": "What the room is for. An empty string clears it.",
			},
			"archived": map[string]any{
				"type":        "boolean",
				"description": "True closes the room to new messages; false reopens it.",
			},
		},
		"required": []any{"channel"},
	}
}

func (t *updateChannel) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *updateChannel) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	const name = chat.UpdateChannelTool
	if t.deps.Writer == nil {
		return unconfiguredChat(name), nil
	}
	// A NAMED FIELD IS SET AND AN ABSENT ONE IS LEFT ALONE, which is the
	// record's own rule: an empty topic is a topic cleared, never "no change".
	var patch chat.ChannelPatch
	if _, given := args["topic"]; given {
		topic := strings.TrimSpace(argString(args, "topic"))
		patch.Topic = &topic
	}
	if _, given := args["purpose"]; given {
		purpose := strings.TrimSpace(argString(args, "purpose"))
		patch.Purpose = &purpose
	}
	if raw, given := args["archived"]; given {
		archived, ok := raw.(bool)
		if !ok {
			return failed(chat.UpdateChannelTool + "'s `archived` is true or false."), nil
		}
		patch.Archived = &archived
	}
	if patch.Topic == nil && patch.Purpose == nil && patch.Archived == nil {
		return failed(chat.UpdateChannelTool + " names no change — set at least one of " +
			"`topic`, `purpose` and `archived`."), nil
	}
	actor, id, refusal := t.deps.resolved(ctx, turn, name, args)
	if refusal != nil {
		return *refusal, nil
	}
	var authority chat.ChannelAuthority
	if patch.Archived != nil {
		decided, refusal := t.deps.archiveAuthority(ctx, actor.Name(), id)
		if refusal != nil {
			return *refusal, nil
		}
		authority = decided
	}
	const look = "Read the room's settings with " + ListChannelsTool
	written, err := t.deps.Writer.PatchChannel(ctx, actor, id, patch, authority)
	if err != nil {
		return chatWriteFailure(ctx, name, err, restateNext(look)), nil
	}
	t.deps.settle(ctx, written.Outcome.Position)
	return stated(name, written, look, map[string]any{
		"channel": id, "name": written.Channel.Name,
		"topic": written.Channel.Topic, "purpose": written.Channel.Purpose,
		"archived": written.Channel.ArchivedAt != nil,
	})
}

// archiveAuthority decides whether the caller may archive the room, or open it
// again, against the room as it is stored ([authz.ActionChatArchive]).
//
// # Why after a read
//
// Because both halves of who may are ON THE ROW and never in the arguments: a
// room's creator archives it, and so does the lead of the unit a unit's room
// is recorded under — and which unit that is, the org chart moves. Decided on
// the arguments, every caller but an admin was refused as naming nobody, and
// a unit's own lead could not close their unit's room.
//
// # Why the unit is carried
//
// The room was read outside the write's snapshot, so the unit the decision
// was taken on travels as a PRECONDITION ([chat.ChannelAuthority.ArchiveUnit]):
// a room re-homed to another unit between the read and the write is refused
// as a conflict, rather than archived by a lead of the unit it left.
func (d ChatDeps) archiveAuthority(ctx context.Context, viewer, channelID string) (
	chat.ChannelAuthority, *tools.Result) {

	if d.Reader == nil {
		out := failed(chat.UpdateChannelTool + " cannot read the room here to say " +
			"who made it and which unit holds it, so its archive cannot be decided.")
		return chat.ChannelAuthority{}, &out
	}
	room, err := d.Reader.Channel(ctx, viewer, channelID, seatRead)
	if err != nil {
		out := chatReadFailure(ctx, chat.UpdateChannelTool, err)
		return chat.ChannelAuthority{}, &out
	}
	if refused := d.mayWrite(ctx, authz.ActionChatArchive, authz.Object{
		Kind: authz.KindChannel, ID: channelID,
		Container: room.Channel.Unit, Author: room.Channel.CreatedBy,
	}); refused != nil {
		return chat.ChannelAuthority{}, refused
	}
	return chat.ChannelAuthority{Archive: true, ArchiveUnit: room.Channel.Unit}, nil
}

// ---- set_channel_members ----------------------------------------------- //

type setChannelMembers struct{ deps ChatDeps }

var _ tools.SeatCallable = (*setChannelMembers)(nil)

func (t *setChannelMembers) Name() string { return chat.SetChannelMembersTool }

func (t *setChannelMembers) Description() string {
	return "State who is in a room — the WHOLE membership, replacing what it " +
		"was. Whoever you leave out is taken out, and for a private room that " +
		"is who may read it. A unit's own room and a direct conversation " +
		"cannot be changed this way."
}

func (t *setChannelMembers) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"channel": map[string]any{
				"type":        "string",
				"description": "The room: its id, or its name.",
			},
			"members": membersSchema("Everybody who is in the room after this " +
				"call. An empty list empties it."),
		},
		"required": []any{"channel", "members"},
	}
}

func (t *setChannelMembers) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *setChannelMembers) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	const name = chat.SetChannelMembersTool
	if t.deps.Writer == nil {
		return unconfiguredChat(name), nil
	}
	// AN ABSENT LIST IS REFUSED, AN EMPTY ONE IS A GESTURE. Emptying a room
	// is real, so "you sent nothing" cannot be read as "nobody".
	if _, given := args["members"]; !given {
		return failed(chat.SetChannelMembersTool + " needs `members` — the whole " +
			"membership after this call; an empty list empties the room."), nil
	}
	members, refusal := argMembers(name, args, "members")
	if refusal != nil {
		return *refusal, nil
	}
	if members == nil {
		members = []chat.Member{}
	}
	actor, id, refusal := t.deps.resolved(ctx, turn, name, args)
	if refusal != nil {
		return *refusal, nil
	}
	const look = "Look at the room's members"
	written, err := t.deps.Writer.SetMembers(ctx, actor, id, members)
	if err != nil {
		return chatWriteFailure(ctx, name, err, restateNext(look)), nil
	}
	t.deps.settle(ctx, written.Outcome.Position)
	handles := make([]string, 0, len(members))
	for _, m := range members {
		handles = append(handles, m.Handle)
	}
	return stated(name, written, look, map[string]any{
		"channel": id, "name": written.Channel.Name, "members": handles,
	})
}

// ---- edit_message ------------------------------------------------------ //

type editMessage struct{ deps ChatDeps }

var _ tools.SeatCallable = (*editMessage)(nil)

func (t *editMessage) Name() string { return chat.EditMessageTool }

func (t *editMessage) Description() string {
	return "Rewrite a message you wrote. Only its author may. Anybody the new " +
		"words @-mention who was not named before is told; nobody else is " +
		"woken again."
}

func (t *editMessage) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"channel": map[string]any{
				"type":        "string",
				"description": "The room: its id, or its name.",
			},
			"message_id": map[string]any{
				"type":        "string",
				"description": "The id of the message to rewrite.",
			},
			"body": map[string]any{
				"type":        "string",
				"description": "The new words, in markdown, replacing the old.",
			},
			"links": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": fmt.Sprintf("Up to %d URLs, replacing the "+
					"message's links; an empty list removes them. Omit it to "+
					"keep the links the message has.", chat.MaxLinks),
			},
		},
		"required": []any{"channel", "message_id", "body"},
	}
}

func (t *editMessage) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *editMessage) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	const name = chat.EditMessageTool
	if t.deps.Writer == nil {
		return unconfiguredChat(name), nil
	}
	messageID := strings.TrimSpace(argString(args, "message_id"))
	body := strings.TrimSpace(argString(args, "body"))
	switch {
	case messageID == "":
		return failed(chat.EditMessageTool + " needs a `message_id` — the id of the " +
			"message to rewrite."), nil
	case body == "":
		return failed(chat.EditMessageTool + " needs a `body`. To take a message back, " +
			"use " + chat.DeleteMessageTool + " — its row stays, so a thread hung off it keeps " +
			"its root."), nil
	}
	actor, id, refusal := t.deps.resolved(ctx, turn, name, args)
	if refusal != nil {
		return *refusal, nil
	}
	// WHOSE WORDS THEY ARE decides it, and the store refuses anybody but
	// their author an edit whoever the table admitted — an edit puts words
	// in somebody's mouth, so there is no moderator's edit to carry.
	if _, refusal := t.deps.remarkAuthority(ctx, authz.ActionChatEdit, actor.Name(),
		id, messageID); refusal != nil {
		return *refusal, nil
	}
	// THE MENTIONS ARE RE-READ FROM THE NEW WORDS, because the body is what
	// they are derived from: an edit that carried the first draft's set
	// would leave the row naming whoever the first draft named.
	in := chat.EditMessage{Body: body}
	if t.deps.Mentions != nil {
		in.Mentions = t.deps.Mentions.Mentions(body)
	}
	// THE LINKS ARE NOT: they change only when the call names them, so a typo
	// fixed without re-sending them keeps every one ([chat.EditMessage]). A
	// named list replaces them — an empty one included, which is how they are
	// removed, so it must arrive as empty rather than as absent.
	if _, given := args["links"]; given {
		links := argStrings(args, "links")
		if links == nil {
			links = []string{}
		}
		in.Links = &links
	}
	// STATED: the same words again replace the message with what it already
	// says, so a repeat after an `unknown` changes nothing if the first
	// landed.
	const look = "Read the message with " + ReadChannelTool
	written, err := t.deps.Writer.Edit(ctx, actor, id, messageID, in)
	if err != nil {
		return chatWriteFailure(ctx, name, err, restateNext(look)), nil
	}
	t.deps.settle(ctx, written.Outcome.Position)
	return stated(name, written, look,
		map[string]any{"channel": id, "message_id": messageID, "edited": true})
}

// ---- delete_message ---------------------------------------------------- //

type deleteMessage struct{ deps ChatDeps }

var _ tools.SeatCallable = (*deleteMessage)(nil)

func (t *deleteMessage) Name() string { return chat.DeleteMessageTool }

func (t *deleteMessage) Description() string {
	return "Take a message back. Its words go; its place stays, so a thread " +
		"hung off it keeps its root. Its author may take it back, and an " +
		"admin may take anybody's down, in any room. Nobody is woken by it."
}

func (t *deleteMessage) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"channel": map[string]any{
				"type":        "string",
				"description": "The room: its id, or its name.",
			},
			"message_id": map[string]any{
				"type":        "string",
				"description": "The id of the message to take back.",
			},
		},
		"required": []any{"channel", "message_id"},
	}
}

func (t *deleteMessage) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *deleteMessage) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	const name = chat.DeleteMessageTool
	if t.deps.Writer == nil {
		return unconfiguredChat(name), nil
	}
	messageID := strings.TrimSpace(argString(args, "message_id"))
	if messageID == "" {
		return failed(chat.DeleteMessageTool + " needs a `message_id` — the id of the " +
			"message to take back."), nil
	}
	actor, id, refusal := t.deps.resolved(ctx, turn, name, args)
	if refusal != nil {
		return *refusal, nil
	}
	moderate, refusal := t.deps.remarkAuthority(ctx, authz.ActionChatDelete,
		actor.Name(), id, messageID)
	if refusal != nil {
		return *refusal, nil
	}
	// STATED: a message already taken back is taken back again to the same
	// tombstone, so the repeat changes nothing.
	const look = "Read the room with " + ReadChannelTool
	written, err := t.deps.Writer.Delete(ctx, actor, id, messageID,
		chat.MessageAuthority{Moderate: moderate})
	if err != nil {
		return chatWriteFailure(ctx, name, err, restateNext(look)), nil
	}
	t.deps.settle(ctx, written.Outcome.Position)
	return stated(name, written, look,
		map[string]any{"channel": id, "message_id": messageID, "deleted": true})
}

// ---- mark_chat_read ---------------------------------------------------- //

type markChatRead struct{ deps ChatDeps }

var _ tools.SeatCallable = (*markChatRead)(nil)

func (t *markChatRead) Name() string { return chat.MarkChatReadTool }

func (t *markChatRead) Description() string {
	return "Record how far you have read in your rooms, which rooms you mute " +
		"or unmute, and whether you are in do-not-disturb. Only what you name " +
		"changes: a room's place only moves forward, a room you mute or unmute " +
		"changes alone, and an empty do-not-disturb ends it."
}

func (t *markChatRead) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"cursors": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "integer", "minimum": 0},
				"description": "Room id → the packed log position read through " +
					"(a message's `version`). A position at or below the one " +
					"held is ignored.",
			},
			"mute": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": "Room ids to mute. Every room you do not name " +
					"keeps the state it has.",
			},
			"unmute": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": "Room ids to unmute. Every room you do not name " +
					"keeps the state it has.",
			},
			"dnd_until": map[string]any{
				"type": "string",
				"description": "When do-not-disturb ends, RFC 3339. An empty " +
					"string ends it now.",
			},
		},
	}
}

func (t *markChatRead) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *markChatRead) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	const name = chat.MarkChatReadTool
	if t.deps.Cursors == nil {
		return unconfiguredChat(name), nil
	}
	delta := coord.ChatReadDelta{At: t.deps.now()}
	if raw, given := args["cursors"]; given {
		cursors, refusal := argCursors(raw)
		if refusal != nil {
			return *refusal, nil
		}
		delta.Cursors = cursors
	}
	// A ROOM AT A TIME, never the list: the person names the rooms whose
	// state changes and every other keeps its own, so muting one room needs
	// nothing read first — [coord.ChatReadDelta]'s own contract.
	delta.Mute = argStrings(args, "mute")
	delta.Unmute = argStrings(args, "unmute")
	if _, given := args["dnd_until"]; given {
		var until time.Time
		if raw := strings.TrimSpace(argString(args, "dnd_until")); raw != "" {
			parsed, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				//nolint:nilerr // A tool failure is a RESULT the model reads.
				return failed(fmt.Sprintf(chat.MarkChatReadTool+"'s `dnd_until` is an RFC "+
					"3339 instant (2026-05-01T09:00:00Z), or an empty string to "+
					"end do-not-disturb now — %q is neither.", clip(raw))), nil
			}
			until = parsed.UTC()
		}
		delta.DNDUntil = &until
	}
	if delta.Cursors == nil && delta.Mute == nil && delta.Unmute == nil &&
		delta.DNDUntil == nil {
		return failed(chat.MarkChatReadTool + " names nothing to record — send `cursors`, " +
			"`mute`, `unmute` or `dnd_until`."), nil
	}
	actor, refusal := t.deps.actor(ctx, turn, name, args)
	if refusal != nil {
		return *refusal, nil
	}
	// A CURSOR IS A PERSON'S, and the table refuses a seat this verb before
	// anything is read. This is the same rule where the record is written,
	// for a surface wired without the table: the coordination store keys a
	// read state by whatever handle it is given and knows no kinds.
	if !actor.IsHuman() {
		return refused(tools.RefusalForbidden, fmt.Sprintf("%s is a person's: "+
			"%s is a %s, and a seat is woken rather than reading, so it has no "+
			"place in a room to record.", name, actor.Name(), actor.Kind)), nil
	}
	// THE RECORD IS THE CALLER'S OWN, kept under the seat they hold — the
	// actor's name, which is [iam.RecordOwner]'s answer for a bound person —
	// and the tool takes no handle, so nobody marks another's.
	state, err := t.deps.Cursors.AdvanceChatRead(ctx, actor.Name(), delta)
	switch {
	case errors.Is(err, coord.ErrInvalidReadState):
		// THE MERGE REFUSED WHAT WAS SENT, and it refuses it the same way
		// every time, so this is the caller's to change rather than to
		// retry. Two refusals reach it from here — a mute that would take
		// the set past its cap, and one room both muted and unmuted in one
		// call — because [argCursors] refuses a cursor naming no room and
		// [argStrings] drops an empty room id before any flush is sent; and
		// the person who named those rooms reads it, so it is said in the
		// merge's own words rather than with the Go package's prefix.
		return refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s refused that: "+
			"%s. Nothing was recorded.", name, coord.ReadStateSentence(err))), nil
	case errors.Is(err, coord.ErrWriteUnknown):
		// SENT, AND THE ANSWER LOST: it may have landed, so "nothing was
		// recorded" is exactly what nobody can say — a compare-and-set whose
		// reply never came. Unlike a log write, the repeat is not a second
		// operation to warn against: the merge is idempotent, so a flush
		// that did land is merged again to the same record. No operation
		// names it, because none exists.
		return failedUnknown("", false, fmt.Sprintf("%s: whether this landed is "+
			"unknown — the coordination store took the write and its answer "+
			"was lost. It may have been recorded and it may not — do not report "+
			"it as done, and do not report it as failed. Sending the same call "+
			"again is safe: a place in a room only moves forward, and a mute, "+
			"an unmute or a do-not-disturb sent twice is stated twice.", name)), nil
	case err != nil:
		// THE COORDINATION STORE'S CONDITION, before any write: it could not
		// be read, or every compare-and-set lost to another writer — each a
		// write the store never took.
		why := "the coordination store could not take it"
		if words, ok := Condition(err); ok {
			why = words
		}
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s did not "+
			"land (%s): nothing was recorded — try again.", name, why)), nil
	}
	// THE PERSON'S STATE AND NOTHING OF THE RECONCILE'S: the absence stamp
	// on the record is the sweep's machinery, not something a person set.
	state.AbsentSince = time.Time{}
	return jsonResult(markChatReadAnswer{
		Read:    coord.PersonChatRead{Handle: actor.Name(), ChatReadState: state},
		Outcome: statelog.OutcomeApplied,
	})
}

// markChatReadAnswer is what a `mark_chat_read` that landed answers.
type markChatReadAnswer struct {
	// Read is the caller's whole read state as the write left it, NAMED BY
	// ITS PERSON ([coord.PersonChatRead]) — the one form a client is handed a
	// read state in, so the live channel's push and the rail's own `read`
	// carry exactly this.
	//
	// THE SCREEN THAT FLUSHED APPLIES IT, which is why it is the same form.
	// The push reaches every socket of the person, the pressing tab's
	// included, and the record's revision orders the two: whichever arrives
	// first is the state, and the other is the same revision and moves
	// nothing. Answered as anything else — a bare state, or no state at all —
	// the pressing tab re-read its rooms on the answer AND on the push, and a
	// tab could not tell this person's state from the last person's.
	Read coord.PersonChatRead `json:"read"`

	// Outcome is always applied: a read state is a compare-and-set in the
	// coordination store, which has landed when it answers.
	Outcome statelog.Outcome `json:"outcome"`

	// Position is always null — APPLIED AT NO POSITION, since a read state is
	// on no log and there is nothing for a read to wait for. Stated rather
	// than omitted, because the act transport reads an absent position and a
	// null one alike and a reader of the tool's answer should not have to know
	// that.
	Position *string `json:"position"`
}

// ---- the arguments these share ----------------------------------------- //

// membersSchema is the shape a room's members are written in, everywhere one
// is taken.
func membersSchema(description string) map[string]any {
	return map[string]any{
		"type": "array",
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"handle": map[string]any{"type": "string"},
				"follow_all": map[string]any{
					"type": "boolean",
					"description": "Every message in the room reaches them, " +
						"named or not.",
				},
			},
			"required": []any{"handle"},
		},
		"description": description + " A bare handle is a member who " +
			"follows only what names them.",
	}
}

// argMembers reads a list of members: each `{handle, follow_all}`, or a bare
// handle, which is what a model writes when nobody follows everything.
//
// ANYTHING ELSE IS REFUSED NAMING THE ENTRY, rather than dropped: a member a
// caller listed and this did not read is somebody left out of a private room
// with nothing to say so. Nil for an absent or empty list — the caller decides
// which of those it can tell apart.
func argMembers(tool string, args map[string]any, key string) ([]chat.Member, *tools.Result) {
	var raw []any
	switch v := args[key].(type) {
	case nil:
		return nil, nil
	case []any:
		raw = v
	case []string:
		for _, h := range v {
			raw = append(raw, h)
		}
	default:
		out := failed(fmt.Sprintf("%s's `%s` is a list of members — each a "+
			"handle, or {\"handle\": …, \"follow_all\": true}.", tool, key))
		return nil, &out
	}
	members := make([]chat.Member, 0, len(raw))
	for i, entry := range raw {
		var member chat.Member
		switch v := entry.(type) {
		case string:
			member.Handle = strings.TrimSpace(v)
		case map[string]any:
			member.Handle = strings.TrimSpace(argString(v, "handle"))
			if follow, given := v["follow_all"]; given {
				all, ok := follow.(bool)
				if !ok {
					out := failed(fmt.Sprintf("%s's `%s` entry %d says follow_all "+
						"is %v — it is true or false.", tool, key, i, follow))
					return nil, &out
				}
				member.FollowAll = all
			}
		}
		if member.Handle == "" {
			out := failed(fmt.Sprintf("%s's `%s` entry %d names no handle.",
				tool, key, i))
			return nil, &out
		}
		members = append(members, member)
	}
	if len(members) == 0 {
		return nil, nil
	}
	return members, nil
}

// argCursors reads read cursors: room id to a packed log position.
//
// A POSITION THAT IS NOT A WHOLE, NON-NEGATIVE NUMBER IS REFUSED, never read
// as zero: a zero cursor is ignored by the merge, so a cursor that failed to
// parse would answer `applied` and leave the badge where it was.
func argCursors(raw any) (map[string]int64, *tools.Result) {
	given, ok := raw.(map[string]any)
	if !ok {
		out := failed(chat.MarkChatReadTool + "'s `cursors` is an object of room id to " +
			"the position read through.")
		return nil, &out
	}
	cursors := make(map[string]int64, len(given))
	for room, value := range given {
		room = strings.TrimSpace(room)
		at, whole := argPositionValue(value)
		if room == "" || !whole || at < 0 {
			out := failed(fmt.Sprintf(chat.MarkChatReadTool+"'s cursor for %q is %v — a "+
				"room id and a whole, non-negative log position.", clip(room), value))
			return nil, &out
		}
		cursors[room] = at
	}
	return cursors, nil
}

// argPositionValue reads a packed log position, reporting whether it could.
//
// [argIntValue]'s discipline — a whole, finite number, the whole string — with
// one exception taken first: a [json.Number] is read as an INTEGER, for
// [argInt]'s reason. A packed position puts the generation above bit 40, and
// through a float it is exact only below 2^53.
func argPositionValue(raw any) (int64, bool) {
	if n, ok := raw.(json.Number); ok {
		if at, err := n.Int64(); err == nil {
			return at, true
		}
	}
	v, ok := argIntValue(raw)
	return int64(v), ok
}
