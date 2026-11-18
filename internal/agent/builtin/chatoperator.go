package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
)

// The chat gestures a PERSON makes, which no seat's registry holds (why each is
// withheld from a seat: [chatlet.OperatorOnlyTools]). They are tools because
// the dashboard writes nothing of its own (ADR-0024): each is one
// `POST /operator/act/{tool}` as the token's person, served to the act
// transport and to that person's own assistant alike through [ChatDeps].
//
// THE AUTHORITY TABLE DECIDES WHO (refusing a seat, or a credential acting as
// itself, before any read) and the store decides WHICH ROOM. What the arguments
// cannot say is decided after a read: whose words a message is
// ([ChatDeps.remarkAuthority]), a room's creator and unit ([updateChannel]).
// A repeat is the same gesture through [callOpKey], as for the seat tools; only
// [markChatRead], on no log and idempotent, needs no operation.

// ---- create_channel ---------------------------------------------------- //

type createChannel struct{ deps ChatDeps }

var _ tools.SeatCallable = (*createChannel)(nil)

func (t *createChannel) Name() string { return chatlet.CreateChannelTool }

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
			// `channel`, AS EVERY CHAT TOOL NAMES ITS ROOM: the name is the
			// address, so a failed create names the room a caller looks up
			// to see what became of it.
			"channel": map[string]any{
				"type": "string",
				"description": fmt.Sprintf("The new room's name, which is its "+
					"address: lower-case letters, digits and hyphens, at most "+
					"%d bytes (\"#launch\" and \"launch\" are the same name).",
					chatlet.MaxChannelName),
			},
			"kind": map[string]any{
				"type": "string",
				"enum": []any{string(chatlet.KindPublic), string(chatlet.KindPrivate)},
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

	const name = chatlet.CreateChannelTool
	if t.deps.Writer == nil {
		return unconfiguredChat(name), nil
	}
	room := strings.TrimPrefix(strings.TrimSpace(argString(args, "channel")), "#")
	if room == "" {
		return failed(chatlet.CreateChannelTool + " needs a `channel` — the new room's name, " +
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
	in := chatlet.NewChannel{
		Name: room, Kind: chatlet.Kind(strings.TrimSpace(argString(args, "kind"))),
		Topic:   strings.TrimSpace(argString(args, "topic")),
		Purpose: strings.TrimSpace(argString(args, "purpose")),
		Members: members,
	}
	again := callRepeat(turn, actor.OpKey, name)
	// AN UNKNOWN CREATE REPORTS NO ROOM: its id may never have landed. Looking
	// first is safe, since a second create of the name is refused as taken.
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

func (t *openDM) Name() string { return chatlet.OpenDMTool }

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
					"including you. You are added yourself.", chatlet.MaxDMParticipants),
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

	const name = chatlet.OpenDMTool
	if t.deps.Writer == nil {
		return unconfiguredChat(name), nil
	}
	to := argStrings(args, "to")
	if len(to) == 0 {
		return failed(chatlet.OpenDMTool + " needs a `to` — the handles of the people the " +
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

func (t *updateChannel) Name() string { return chatlet.UpdateChannelTool }

func (t *updateChannel) Description() string {
	return "Change a room's settings: its topic, its purpose, whether it is " +
		"archived, and whether it takes in everybody who joins the company. Only " +
		"what you name changes, and the room's transcript records a new topic " +
		"or purpose as a line. An archived room is still read and searched and " +
		"takes no new messages; archiving it, or opening it again, is its " +
		"creator's, its unit's lead's or an admin's. Auto-join is an admin's, " +
		"and a public room's only."
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
			"template": map[string]any{
				"type": "string",
				"description": fmt.Sprintf("The name of the template the topic "+
					"and purpose are set from, at most %d characters: the "+
					"transcript records one line naming it in place of a topic "+
					"line and a purpose line. Send it with the template's topic "+
					"or purpose.", chatlet.MaxTemplateName),
			},
			"auto_join": map[string]any{
				"type": "boolean",
				"description": "True puts everybody who joins the company from " +
					"now on in this public room; false stops it. Nobody already " +
					"in the company is added. An administrator's setting.",
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

	const name = chatlet.UpdateChannelTool
	if t.deps.Writer == nil {
		return unconfiguredChat(name), nil
	}
	// A NAMED FIELD IS SET AND AN ABSENT ONE LEFT ALONE, the record's own rule:
	// an empty topic clears it. A NULL IS ABSENT, as the surface check reads it
	// (ADR-0059), or it would clear a field or refuse an archive it admitted.
	var patch chatlet.ChannelPatch
	if args["topic"] != nil {
		topic := strings.TrimSpace(argString(args, "topic"))
		patch.Topic = &topic
	}
	if args["purpose"] != nil {
		purpose := strings.TrimSpace(argString(args, "purpose"))
		patch.Purpose = &purpose
	}
	// BY THE CHECK'S OWN READER ([argval.Bool]): true or false and nothing
	// else, since false reopens the room rather than saying nothing.
	if raw := args["archived"]; raw != nil {
		archived, ok := argval.Bool(raw)
		if !ok {
			return failed(chatlet.UpdateChannelTool + "'s `archived` is true or false."), nil
		}
		patch.Archived = &archived
	}
	if args["template"] != nil {
		template := strings.TrimSpace(argString(args, "template"))
		patch.Template = &template
	}
	if raw := args["auto_join"]; raw != nil {
		on, ok := argval.Bool(raw)
		if !ok {
			return failed(chatlet.UpdateChannelTool + "'s `auto_join` is true or false."), nil
		}
		patch.AutoJoin = &on
	}
	if patch.Topic == nil && patch.Purpose == nil && patch.Archived == nil &&
		patch.AutoJoin == nil {
		return failed(chatlet.UpdateChannelTool + " names no change. Set at least one of " +
			"`topic`, `purpose`, `archived` and `auto_join`."), nil
	}
	actor, id, refusal := t.deps.resolved(ctx, turn, name, args)
	if refusal != nil {
		return *refusal, nil
	}
	var authority chatlet.ChannelAuthority
	if patch.Archived != nil {
		decided, refusal := t.deps.archiveAuthority(ctx, actor.Name(), id)
		if refusal != nil {
			return *refusal, nil
		}
		authority = decided
	}
	if patch.AutoJoin != nil {
		// AN ADMINISTRATOR'S, decided by the table here and carried to the
		// store like the archive's; the room's kind (public only) is the
		// store's to judge against its own snapshot.
		if refused := t.deps.mayWrite(ctx, authz.ActionChatAutoJoin, authz.Object{
			Kind: authz.KindChannel, ID: id,
		}); refused != nil {
			return *refused, nil
		}
		authority.AutoJoin = true
	}
	const look = "Read the room's settings with " + ListChannelsTool
	written, err := t.deps.Writer.PatchChannel(ctx, actor, id, patch, authority)
	if err != nil {
		return chatWriteFailure(ctx, name, err, restateNext(look)), nil
	}
	t.deps.settle(ctx, written.Outcome.Position)
	if written.Outcome.Outcome == statelog.OutcomeUnknown {
		return stated(name, written, look, nil)
	}
	// THE ROOM'S SETTINGS AS THEY NOW STAND: a topic or purpose this call
	// left alone is whoever set it last, so each is fenced as list_channels
	// fences it, by writers the room does not name (ADR-0040).
	read := newForeignRead(turn, actor.Name())
	read.fence(fieldPath{}.key("topic"), provenance.SourceChatRoom, nil, true, written.Channel.Topic)
	read.fence(fieldPath{}.key("purpose"), provenance.SourceChatRoom, nil, true, written.Channel.Purpose)
	return read.answer(ctx, name, statedReceipt(written, map[string]any{
		"channel": id, "name": written.Channel.Name,
		"topic": written.Channel.Topic, "purpose": written.Channel.Purpose,
		"archived":  written.Channel.ArchivedAt != nil,
		"auto_join": written.Channel.AutoJoin,
	}), "")
}

// archiveAuthority decides whether the caller may archive the room, or open it
// again, against the room as stored ([authz.ActionChatArchive]): who may is ON
// THE ROW, never in the arguments — its creator, and the lead of the unit the
// org chart now records a unit's room under.
//
// The room is read outside the write's snapshot, so the unit decided on
// travels as a PRECONDITION ([chatlet.ChannelAuthority.ArchiveUnit]): a room
// re-homed between the read and the write is refused as a conflict rather
// than archived by a lead of the unit it left.
func (d ChatDeps) archiveAuthority(ctx context.Context, viewer, channelID string) (
	chatlet.ChannelAuthority, *tools.Result) {

	if d.Reader == nil {
		out := failed(chatlet.UpdateChannelTool + " cannot read the room here to say " +
			"who made it and which unit holds it, so its archive cannot be decided.")
		return chatlet.ChannelAuthority{}, &out
	}
	room, err := d.Reader.Channel(ctx, viewer, channelID, seatRead)
	if err != nil {
		out := chatReadFailure(ctx, chatlet.UpdateChannelTool, err)
		return chatlet.ChannelAuthority{}, &out
	}
	if refused := d.mayWrite(ctx, authz.ActionChatArchive, authz.Object{
		Kind: authz.KindChannel, ID: channelID,
		Container: room.Channel.Unit, Author: room.Channel.CreatedBy,
	}); refused != nil {
		return chatlet.ChannelAuthority{}, refused
	}
	return chatlet.ChannelAuthority{Archive: true, ArchiveUnit: room.Channel.Unit}, nil
}

// ---- set_channel_members ----------------------------------------------- //

type setChannelMembers struct{ deps ChatDeps }

var _ tools.SeatCallable = (*setChannelMembers)(nil)

func (t *setChannelMembers) Name() string { return chatlet.SetChannelMembersTool }

func (t *setChannelMembers) Description() string {
	return "State who is in a room — the WHOLE membership, replacing what it " +
		"was. Whoever you leave out is taken out, and for a private room that " +
		"is who may read it. Send the membership you read as " +
		"`expected_members` so a change somebody else made meanwhile is " +
		"refused rather than written back over. A unit's own room and a " +
		"direct conversation cannot be changed this way."
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
			// THE READ THE SET WAS BUILT FROM, compared inside the write's
			// snapshot ([chatlet.Store.SetMembers]), so restating everybody
			// cannot write back over a member somebody just took out.
			"expected_members": membersSchema("The room's membership as you " +
				"read it, each member with their follow_all, which the change " +
				"was built from. If the room holds anybody else now, or " +
				"anybody follows it differently, the change is refused and you " +
				"read the room again. An empty list expects an empty room."),
		},
		"required": []any{"channel", "members"},
	}
}

func (t *setChannelMembers) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *setChannelMembers) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	const name = chatlet.SetChannelMembersTool
	if t.deps.Writer == nil {
		return unconfiguredChat(name), nil
	}
	// AN ABSENT LIST IS REFUSED, AN EMPTY ONE EMPTIES THE ROOM: "sent nothing"
	// is never "nobody". Null is nothing sent, as the surface check reads it
	// (ADR-0059).
	if args["members"] == nil {
		return failed(chatlet.SetChannelMembersTool + " needs `members` — the whole " +
			"membership after this call; an empty list empties the room."), nil
	}
	members, refusal := argMemberSet(name, args, "members", "everybody in the "+
		"room after this call, or an empty list, [], to empty it")
	if refusal != nil {
		return *refusal, nil
	}
	// ABSENT IS NO EXPECTATION; otherwise the same reader as `members`, since
	// one naming nobody by mistake would refuse every change.
	var expected *[]chatlet.Member
	if args["expected_members"] != nil {
		read, readRefusal := argMemberSet(name, args, "expected_members", "everybody "+
			"in the room as you read it, or an empty list, [], for an empty room")
		if readRefusal != nil {
			return *readRefusal, nil
		}
		expected = &read
	}
	actor, id, refusal := t.deps.resolved(ctx, turn, name, args)
	if refusal != nil {
		return *refusal, nil
	}
	const look = "Look at the room's members"
	written, err := t.deps.Writer.SetMembers(ctx, actor, id, members, expected)
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

// ---- add_channel_members ----------------------------------------------- //

type addChannelMembers struct{ deps ChatDeps }

var _ tools.SeatCallable = (*addChannelMembers)(nil)

func (t *addChannelMembers) Name() string { return chatlet.AddChannelMembersTool }

func (t *addChannelMembers) Description() string {
	return "Put people and agents in a room, taking nobody out. The room's " +
		"transcript records who you added. A unit's own room and a direct " +
		"conversation cannot be changed this way, and a private room only by " +
		"somebody in it."
}

func (t *addChannelMembers) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"channel": map[string]any{
				"type":        "string",
				"description": "The room: its id, or its name.",
			},
			"members": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": "The handles of the people and agents to add. " +
					"Anybody already in the room stays as they are.",
			},
		},
		"required": []any{"channel", "members"},
	}
}

func (t *addChannelMembers) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *addChannelMembers) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	const name = chatlet.AddChannelMembersTool
	if t.deps.Writer == nil {
		return unconfiguredChat(name), nil
	}
	handles := argStrings(args, "members")
	if len(handles) == 0 {
		return failed(name + " needs `members`: the handles of the people and " +
			"agents to add."), nil
	}
	actor, id, refusal := t.deps.resolved(ctx, turn, name, args)
	if refusal != nil {
		return *refusal, nil
	}
	const look = "Look at the room's members"
	written, added, err := t.deps.Writer.AddMembers(ctx, actor, id, handles)
	if err != nil {
		return chatWriteFailure(ctx, name, err, restateNext(look)), nil
	}
	t.deps.settle(ctx, written.Outcome.Position)
	return stated(name, written, look, map[string]any{
		"channel": id, "name": written.Channel.Name, "added": added,
	})
}

// ---- edit_message ------------------------------------------------------ //

type editMessage struct{ deps ChatDeps }

var _ tools.SeatCallable = (*editMessage)(nil)

func (t *editMessage) Name() string { return chatlet.EditMessageTool }

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
				"description": fmt.Sprintf("Up to %d URLs, %s, replacing the "+
					"message's links; an empty list removes them. Omit it to "+
					"keep the links the message has.", chatlet.MaxLinks, linkShape),
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

	const name = chatlet.EditMessageTool
	if t.deps.Writer == nil {
		return unconfiguredChat(name), nil
	}
	messageID := strings.TrimSpace(argString(args, "message_id"))
	body := strings.TrimSpace(argString(args, "body"))
	switch {
	case messageID == "":
		return failed(chatlet.EditMessageTool + " needs a `message_id` — the id of the " +
			"message to rewrite."), nil
	case body == "":
		return failed(chatlet.EditMessageTool + " needs a `body`. To take a message back, " +
			"use " + chatlet.DeleteMessageTool + " — its row stays, so a thread hung off it keeps " +
			"its root."), nil
	}
	actor, id, refusal := t.deps.resolved(ctx, turn, name, args)
	if refusal != nil {
		return *refusal, nil
	}
	// WHOSE WORDS THEY ARE decides it: the store refuses anybody but the
	// author an edit, since an edit puts words in somebody's mouth — there is
	// no moderator's edit.
	if _, refusal := t.deps.remarkAuthority(ctx, authz.ActionChatEdit, actor.Name(),
		id, messageID); refusal != nil {
		return *refusal, nil
	}
	// THE MENTIONS ARE RE-READ FROM THE NEW WORDS: the first draft's set would
	// leave the row naming whoever that draft named.
	in := chatlet.EditMessage{Body: body}
	if t.deps.Mentions != nil {
		in.Mentions = t.deps.Mentions.Mentions(body)
	}
	// THE LINKS ARE NOT: they change only when named ([chatlet.EditMessage]).
	// A named list replaces them, and an empty one removes them, so it must
	// arrive empty rather than absent; a null names none (ADR-0059).
	if args["links"] != nil {
		links := argStrings(args, "links")
		if links == nil {
			links = []string{}
		}
		in.Links = &links
	}
	// STATED: the same words again change nothing, so a repeat after an
	// `unknown` is safe.
	const look = "Read the message with " + ReadChannelTool
	written, err := t.deps.Writer.Edit(ctx, actor, id, messageID, in)
	if err != nil {
		return chatWriteFailure(ctx, name, err, restateNext(look)), nil
	}
	commit(turn, written.Outcome.Outcome, types.CommittedChat)
	t.deps.settle(ctx, written.Outcome.Position)
	return stated(name, written, look,
		map[string]any{"channel": id, "message_id": messageID, "edited": true})
}

// ---- delete_message ---------------------------------------------------- //

type deleteMessage struct{ deps ChatDeps }

var _ tools.SeatCallable = (*deleteMessage)(nil)

func (t *deleteMessage) Name() string { return chatlet.DeleteMessageTool }

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

	const name = chatlet.DeleteMessageTool
	if t.deps.Writer == nil {
		return unconfiguredChat(name), nil
	}
	messageID := strings.TrimSpace(argString(args, "message_id"))
	if messageID == "" {
		return failed(chatlet.DeleteMessageTool + " needs a `message_id` — the id of the " +
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
		chatlet.MessageAuthority{Moderate: moderate})
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

func (t *markChatRead) Name() string { return chatlet.MarkChatReadTool }

func (t *markChatRead) Description() string {
	return "Record your own place and marks in chat: how far you have read in " +
		"your rooms and threads, a room set back to unread, how each room reaches " +
		"you (every message, mentions or muted), rooms you star, messages you " +
		"save for later, threads you follow or unfollow, and whether you are in " +
		"do-not-disturb. Only what you name changes: a place only moves forward " +
		"unless you rewind it, every room, message or thread you do not name " +
		"keeps its state, and an empty do-not-disturb ends it."
}

// markRefSchema is one entry of a list that names a message or a thread by its
// room and its id, as save, unsave, follow and unfollow take one.
func markRefSchema(idKey, what string) map[string]any {
	return map[string]any{
		"type": "array",
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"channel_id": map[string]any{"type": "string"},
				idKey:        map[string]any{"type": "string"},
			},
			"required": []any{"channel_id", idKey},
		},
		"description": what,
	}
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
			"thread_cursors": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "integer", "minimum": 0},
				"description": "Thread root message id → the packed log position " +
					"read through in that thread. Only moves forward.",
			},
			"rewind": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "integer", "minimum": 0},
				"description": "Room id → the packed log position to set its place " +
					"back to exactly (mark unread from there); 0 marks the whole " +
					"room unread. A room named here is not named in `cursors`.",
			},
			"notify": map[string]any{
				"type": "object",
				"additionalProperties": map[string]any{
					"type": "string", "enum": notifyLevelNames(),
				},
				"description": "Room id → how the room reaches you: `all` (every " +
					"message), `mentions` (only what names you, the default) or " +
					"`muted` (nothing). A room named here is not named in `mute` " +
					"or `unmute`.",
			},
			"star": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": "Room ids to star.",
			},
			"unstar": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": "Room ids to unstar.",
			},
			"save": markRefSchema("message_id", "Messages to save for later, "+
				"each by its room and its id."),
			"unsave": markRefSchema("message_id", "Saved messages to let go, "+
				"each by its room and its id."),
			"follow": markRefSchema("root_id", "Threads to follow, each by its "+
				"room and its root message's id."),
			"unfollow": markRefSchema("root_id", "Threads to stop following, "+
				"each by its room and its root message's id. A thread you "+
				"started, replied in or were named in is followed until you do."),
		},
	}
}

// notifyLevelNames are the notify levels as the schema's enum lists them.
func notifyLevelNames() []any {
	out := make([]any, 0, len(coord.NotifyLevels))
	for _, l := range coord.NotifyLevels {
		out = append(out, string(l))
	}
	return out
}

func (t *markChatRead) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *markChatRead) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	const name = chatlet.MarkChatReadTool
	if t.deps.Cursors == nil {
		return unconfiguredChat(name), nil
	}
	// A NULL IS NOTHING SENT, as the check at the surface reads it (ADR-0059):
	// a null do-not-disturb read as named was the empty one, which ends it.
	delta := coord.ChatReadDelta{At: t.deps.now()}
	// IN A FIXED ORDER, so a call wrong in two places is told the same one
	// every time it is sent.
	for _, places := range []struct {
		key  string
		into *map[string]int64
	}{
		{"cursors", &delta.Cursors}, {"thread_cursors", &delta.ThreadCursors},
		{"rewind", &delta.Rewind},
	} {
		if raw := args[places.key]; raw != nil {
			positions, refusal := argCursors(places.key, raw)
			if refusal != nil {
				return *refusal, nil
			}
			*places.into = positions
		}
	}
	if raw := args["notify"]; raw != nil {
		levels, refusal := argNotifyLevels(raw)
		if refusal != nil {
			return *refusal, nil
		}
		delta.Notify = levels
	}
	delta.Star = argStrings(args, "star")
	delta.Unstar = argStrings(args, "unstar")
	for _, messages := range []struct {
		key  string
		into *[]coord.MessageRef
	}{{"save", &delta.Save}, {"unsave", &delta.Unsave}} {
		refs, refusal := argMarkRefs(args, messages.key, "message_id")
		if refusal != nil {
			return *refusal, nil
		}
		for _, ref := range refs {
			*messages.into = append(*messages.into,
				coord.MessageRef{ChannelID: ref[0], MessageID: ref[1]})
		}
	}
	for _, threads := range []struct {
		key  string
		into *[]coord.ThreadRef
	}{{"follow", &delta.Follow}, {"unfollow", &delta.Unfollow}} {
		refs, refusal := argMarkRefs(args, threads.key, "root_id")
		if refusal != nil {
			return *refusal, nil
		}
		for _, ref := range refs {
			*threads.into = append(*threads.into,
				coord.ThreadRef{ChannelID: ref[0], RootID: ref[1]})
		}
	}
	// A ROOM AT A TIME, never the list: every room not named keeps its state,
	// so muting one needs nothing read first ([coord.ChatReadDelta]).
	delta.Mute = argStrings(args, "mute")
	delta.Unmute = argStrings(args, "unmute")
	if args["dnd_until"] != nil {
		var until time.Time
		if raw := strings.TrimSpace(argString(args, "dnd_until")); raw != "" {
			parsed, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				//nolint:nilerr // A tool failure is a RESULT the model reads.
				return failed(fmt.Sprintf(chatlet.MarkChatReadTool+"'s `dnd_until` is an RFC "+
					"3339 instant (2026-05-01T09:00:00Z), or an empty string to "+
					"end do-not-disturb now — %q is neither.", clip(raw))), nil
			}
			until = parsed.UTC()
		}
		delta.DNDUntil = &until
	}
	if delta.Cursors == nil && delta.ThreadCursors == nil && delta.Rewind == nil &&
		delta.Mute == nil && delta.Unmute == nil && delta.Notify == nil &&
		delta.Star == nil && delta.Unstar == nil && delta.Save == nil &&
		delta.Unsave == nil && delta.Follow == nil && delta.Unfollow == nil &&
		delta.DNDUntil == nil {
		return failed(chatlet.MarkChatReadTool + " names nothing to record — send `cursors`, " +
			"`thread_cursors`, `rewind`, `mute`, `unmute`, `notify`, `star`, `unstar`, " +
			"`save`, `unsave`, `follow`, `unfollow` or `dnd_until`."), nil
	}
	actor, refusal := t.deps.actor(ctx, turn, name, args)
	if refusal != nil {
		return *refusal, nil
	}
	// A CURSOR IS A PERSON'S. The table already refuses a seat; this holds a
	// surface wired without it, since the coordination store keys a read
	// state by any handle and knows no kinds.
	if !actor.IsHuman() {
		return refused(tools.RefusalForbidden, fmt.Sprintf("%s is a person's: "+
			"%s is a %s, and a seat is woken rather than reading, so it has no "+
			"place in a room to record.", name, actor.Name(), actor.Kind)), nil
	}
	// THE RECORD IS THE CALLER'S OWN, under the actor's name ([iam.RecordOwner]'s
	// answer for a bound person); the tool takes no handle, so nobody marks
	// another's.
	state, err := t.deps.Cursors.AdvanceChatRead(ctx, actor.Name(), delta)
	switch {
	case errors.Is(err, coord.ErrInvalidReadState):
		// THE MERGE REFUSED WHAT WAS SENT, the same way every time: the
		// caller's to change, not retry. Only a mute past the set's cap or a
		// room both muted and unmuted reach here ([argCursors] and
		// [argStrings] stop a room-less entry first), so it is said in the
		// merge's own words, without the Go package's prefix.
		return refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s refused that: "+
			"%s. Nothing was recorded.", name, coord.ReadStateSentence(err))), nil
	case errors.Is(err, coord.ErrWriteUnknown):
		// SENT, AND THE ANSWER LOST: it may have landed. Unlike a log write,
		// a repeat is no second operation to warn against — the merge is
		// idempotent — and no operation exists to name.
		return failedUnknown("", false, fmt.Sprintf("%s: whether this landed is "+
			"unknown — the coordination store took the write and its answer "+
			"was lost. It may have been recorded and it may not — do not report "+
			"it as done, and do not report it as failed. Sending the same call "+
			"again is safe: a place in a room only moves forward, and a mute, "+
			"an unmute or a do-not-disturb sent twice is stated twice.", name)), nil
	case err != nil:
		// THE COORDINATION STORE'S CONDITION: unreadable, or every
		// compare-and-set lost to another writer — a write it never took.
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
	// ITS PERSON ([coord.PersonChatRead]): the one form a client is handed a
	// read state in, the live channel's push and the rail's `read` included.
	//
	// THE SCREEN THAT FLUSHED APPLIES IT, as it applies the push that reaches
	// every socket of the person; the record's revision orders the two, so
	// the second moves nothing. Any other form made the tab re-read its rooms
	// twice and left it unable to tell this person's state from the last's.
	Read coord.PersonChatRead `json:"read"`

	// Outcome is always applied: a read state is a compare-and-set in the
	// coordination store, which has landed when it answers.
	Outcome statelog.Outcome `json:"outcome"`

	// Position is always null: a read state is on no log, so a read has
	// nothing to wait for. Stated rather than omitted, so a reader of the
	// answer need not know the act transport reads absent and null alike.
	Position *string `json:"position"`
}

// ---- the arguments these share ----------------------------------------- //

// membersSchema is the shape a room's members are written in, everywhere one
// is taken.
//
// AN ENTRY IS AN OBJECT OR A STRING, because a bare handle is a member and the
// surface check holds every call to this schema before the tool runs
// (ADR-0059): declared an object alone, a bare handle would be refused.
func membersSchema(description string) map[string]any {
	return map[string]any{
		"type": "array",
		"items": map[string]any{
			"type": []any{"object", "string"},
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
// BY THE CHECK'S OWN READERS (ADR-0059), so it reads exactly what
// [membersSchema] admits: the list by [argval.StringItems] (a comma-split
// string included), an entry by [argval.Object] or else [argval.String], and
// `follow_all` by [argval.Bool]. Null is absent to each.
//
// ANYTHING ELSE IS REFUSED NAMING THE ENTRY, never dropped: a dropped member
// is somebody silently left out of a private room. Nil for any value naming
// nobody; [argMemberSet] tells the empty list apart.
func argMembers(tool string, args map[string]any, key string) ([]chatlet.Member, *tools.Result) {
	if args[key] == nil {
		return nil, nil
	}
	raw, ok := argval.StringItems(args[key])
	if !ok {
		out := failed(fmt.Sprintf("%s's `%s` is a list of members — each a "+
			"handle, or {\"handle\": …, \"follow_all\": true}.", tool, key))
		return nil, &out
	}
	members := make([]chatlet.Member, 0, len(raw))
	for i, entry := range raw {
		if entry == nil {
			continue
		}
		var member chatlet.Member
		if object, isObject := argval.Object(entry); isObject {
			member.Handle = strings.TrimSpace(argString(object, "handle"))
			if follow, given := object["follow_all"]; given {
				all, ok := argval.Bool(follow)
				if !ok {
					out := failed(fmt.Sprintf("%s's `%s` entry %d says follow_all "+
						"is %v: it is true or false.", tool, key, i, follow))
					return nil, &out
				}
				member.FollowAll = all
			}
		} else if handle, isText := argval.String(entry); isText {
			member.Handle = strings.TrimSpace(handle)
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

// argMemberSet reads a WHOLE membership under key, where only the empty list
// means nobody; want is what the refusal asks for in its place, and the caller
// has already decided what an absent key means.
//
// Any other value naming nobody (a handle-less string such as " , ", a list of
// nulls) is refused: likelier a caller meaning to send nothing, and for
// `members` nobody empties a private room beyond anyone's reach to read it or
// set its members again. Hence the one unambiguous spelling, by [argval.List].
func argMemberSet(tool string, args map[string]any, key, want string) (
	[]chatlet.Member, *tools.Result) {

	members, refusal := argMembers(tool, args, key)
	if refusal != nil {
		return nil, refusal
	}
	if members != nil {
		return members, nil
	}
	if list, isList := argval.List(args[key]); !isList || len(list) > 0 {
		out := failed(fmt.Sprintf("%s's `%s` names nobody. Send the handles of "+
			"%s.", tool, key, want))
		return nil, &out
	}
	return []chatlet.Member{}, nil
}

// argCursors reads read cursors, id to a packed log position, for each of
// `cursors`, `rewind` and `thread_cursors`, naming key in a refusal.
//
// BY THE CHECK'S OWN READER ([argval.Object], ADR-0059): a null position is an
// id not named. Nil when no id is named, so a call naming nothing else is
// refused rather than answered `applied` having recorded nothing.
//
// A POSITION THAT IS NOT A WHOLE, NON-NEGATIVE NUMBER IS REFUSED, never read
// as zero: the merge ignores a zero cursor, so a parse failure would answer
// `applied` and leave the badge where it was.
func argCursors(key string, raw any) (map[string]int64, *tools.Result) {
	given, ok := argval.Object(raw)
	if !ok {
		out := failed(fmt.Sprintf("%s's `%s` is an object of id to a packed log "+
			"position.", chatlet.MarkChatReadTool, key))
		return nil, &out
	}
	cursors := make(map[string]int64, len(given))
	for id, value := range given {
		if value == nil {
			continue
		}
		id = strings.TrimSpace(id)
		at, whole := argPositionValue(value)
		if id == "" || !whole || at < 0 {
			out := failed(fmt.Sprintf("%s's `%s` entry for %q is %v: an id and a "+
				"whole, non-negative log position.", chatlet.MarkChatReadTool, key,
				clip(id), value))
			return nil, &out
		}
		cursors[id] = at
	}
	if len(cursors) == 0 {
		return nil, nil
	}
	return cursors, nil
}

// argNotifyLevels reads `notify`: room id to how the room reaches the caller.
// A null is a room not named, as for the cursors; a level this build does not
// know is refused naming the room, never read as the default.
func argNotifyLevels(raw any) (map[string]coord.NotifyLevel, *tools.Result) {
	given, ok := argval.Object(raw)
	if !ok {
		out := failed(chatlet.MarkChatReadTool + "'s `notify` is an object of room id " +
			"to `all`, `mentions` or `muted`.")
		return nil, &out
	}
	levels := make(map[string]coord.NotifyLevel, len(given))
	for room, value := range given {
		if value == nil {
			continue
		}
		room = strings.TrimSpace(room)
		word, _ := argval.String(value)
		level := coord.NotifyLevel(strings.TrimSpace(word))
		if room == "" || !level.Valid() {
			out := failed(fmt.Sprintf("%s's `notify` entry for %q is %v: a room id "+
				"and one of `all`, `mentions` or `muted`.", chatlet.MarkChatReadTool,
				clip(room), value))
			return nil, &out
		}
		levels[room] = level
	}
	if len(levels) == 0 {
		return nil, nil
	}
	return levels, nil
}

// argMarkRefs reads a list that names messages or threads, each as
// `{channel_id, <idKey>}`, as the pair [room, id]. A null entry is one not
// named; an entry without both is refused naming its place in the list, since
// a mark nobody can find again is one the reader can never take back.
func argMarkRefs(args map[string]any, key, idKey string) ([][2]string, *tools.Result) {
	if args[key] == nil {
		return nil, nil
	}
	raw, ok := argval.StringItems(args[key])
	if !ok {
		out := failed(fmt.Sprintf("%s's `%s` is a list of {\"channel_id\": …, "+
			"\"%s\": …}.", chatlet.MarkChatReadTool, key, idKey))
		return nil, &out
	}
	refs := make([][2]string, 0, len(raw))
	for i, entry := range raw {
		if entry == nil {
			continue
		}
		object, isObject := argval.Object(entry)
		channel := strings.TrimSpace(argString(object, "channel_id"))
		id := strings.TrimSpace(argString(object, idKey))
		if !isObject || channel == "" || id == "" {
			out := failed(fmt.Sprintf("%s's `%s` entry %d needs a `channel_id` and "+
				"a `%s`.", chatlet.MarkChatReadTool, key, i, idKey))
			return nil, &out
		}
		refs = append(refs, [2]string{channel, id})
	}
	return refs, nil
}

// argPositionValue reads a packed log position, reporting whether it could.
//
// By [argIntValue], the check's own integer reader ([argval.Whole], ADR-0059),
// which reads a json.Number exactly as an INTEGER: a packed position puts the
// generation above bit 40, and a float is exact only below 2^53.
func argPositionValue(raw any) (int64, bool) {
	v, ok := argIntValue(raw)
	return int64(v), ok
}
