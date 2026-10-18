package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/textcut"
)

// THE COMPANY'S OWN CHAT: the writes a person makes in a room, each executing
// as the chat gesture the dashboard's own controls send on the act route, so a
// message the assistant prepared and one the person typed are one code path.
//
// A CHAT GESTURE IS A CALL, NOT A RESOURCE. The generic change can show what a
// call sends and nothing more: never which room an id names, who reads it,
// whom a membership list leaves out or which words an edit replaces. So every
// gesture whose effect turns on what the room or the message holds now is a
// write of its own here, and its preview reads the room
// (`GET /chatlet/channels/{channel_id}`) or the message through the turn's grant,
// as the person (ADR-0051):
//
//   - it names the room as people do, and who will read it once the write
//     lands: the whole company for a room anybody may read, and its members
//     by name for one only they may (D09);
//   - it binds the room as the card shows it, or the message's position, so
//     an approval after somebody changed either is refused as stale and
//     prepared again against what is there now. NOT THE ROOM'S OWN
//     `revision`: every message said in the room moves that, so a change
//     bound to it would go stale whenever anybody spoke;
//   - a membership is stated whole, which is where a member is dropped by
//     being forgotten, and in a private room that is who may read it: the
//     card shows who is added and who is removed, and a removal the call does
//     not name in `removes` refuses the step (ADR-0055);
//   - a change that takes something away (a message taken down, a member
//     taken out of a room) is destructive ([wire.RiskDestructive]), and its
//     summary says what goes, as the generic change's does.
//
// The people a write names are seats, by handle or by seat name ([Tool.Names]):
// the turn resolves a name to its handle before the preview reads anything,
// and a member the chart no longer lists is kept as the room holds them
// ([DirMember]).
//
// The other gestures (a reaction, joining a room, making one, opening a
// conversation before anything is said in it, marking rooms read) touch only
// what their own call names, so the generic change's card, the call as sent
// and titled by its tool, shows the whole of what they do.
//
// EVERY TEXT A CHAT CARD SHOWS PASSES redact.Secrets, as the generic change's
// rows do: a room is where a credential gets pasted, and taking such a message
// down is one of these writes, so its card must not copy the credential into
// the conversation's record.

// The two reads a chat write's preview makes (internal/api's named reads).
const (
	chatRoomRoute = "GET /chatlet/channels/{channel_id}"
	// chatThreadRoute is also how ONE MESSAGE is read: a thread is addressed
	// by the message it hangs off, and a read of any message's own id answers
	// that message as its root, which is how the chat tools themselves read
	// whose words a message is before an edit or a delete.
	chatThreadRoute = "GET /chatlet/channels/{channel_id}/threads/{root_id}"
)

// The bounds a chat write's arguments are held to. They are internal/chatlet's
// own, which this package may not import (a domain's writer), so they are
// copied here and held equal to the domain's by internal/agent/builtin's
// catalogue walk: a call the schema admits is within every bound the room
// holds it to. What a link is, the room decides, and the schema says it
// ([chatLinkShape]).
const (
	chatBodyBytes    = 32 << 10 // chatlet.MaxBody
	chatTopicBytes   = 256      // chatlet.MaxTopic
	chatPurposeBytes = 600      // chatlet.MaxPurpose
	chatLinks        = 8        // chatlet.MaxLinks
	chatLinkBytes    = 2 << 10  // chatlet.MaxLinkBytes
	chatMembers      = 1000     // chatlet.MaxMembers
	// chatDirectOthers is everybody a direct message reaches beside the
	// person, whom chatlet.MaxDMParticipants counts as well.
	chatDirectOthers = 7
)

// chatWrites are the chat writes, in the order a model meets them: what the
// person says, then what they change about a room.
func chatWrites() []Tool {
	room := text("The room, by its name as people write it (#launch) or by its id.", nameBytes)
	links := linkList(fmt.Sprintf("Up to %d links the message points at.", chatLinks))
	members := chatWrite("set_chat_room_members", authz.ActionChatMembers, true,
		"change who is in a chat room",
		"Propose a chat room's whole membership. Read the room first with "+APIRequestTool+
			" (GET /chatlet/channels/{channel_id}) and send everybody who stays as well as "+
			"anybody new: a member left out is removed, so name each one the person asked "+
			"to remove in removes. A unit's own room and a direct conversation cannot be "+
			"changed this way.",
		object([]string{"channel", "members"}, map[string]*schema{
			"channel": room,
			"members": {kind: "array", description: "Everybody in the room after the " +
				"change, the members who stay included, each {\"handle\": ...} naming a " +
				"seat by its handle or its name. A member given without follow_all keeps " +
				"how they follow the room.", maxItems: chatMembers,
				items: object([]string{"handle"}, map[string]*schema{
					"handle": text("A seat's handle or name.", nameBytes),
					"follow_all": flag("Every message in the room reaches them, " +
						"named or not."),
				})},
			RemovesArgument: names("The members the change takes out, each by handle or "+
				"name. Only these leave: anybody else left out of members refuses the "+
				"change. The person sees every removal at the top of the card.", chatMembers),
		}))
	// A MEMBERSHIP NAMES WHO THE ROOM HOLDS, which may be a seat the chart no
	// longer lists: kept as written, for the preview to decide against the
	// room ([DirMember]).
	members.Names = map[string]Directory{"members": DirMember, RemovesArgument: DirMember}
	// `removes` ADMITS A REMOVAL AND IS NEVER SENT: the membership the
	// request states is the whole of it, and a member it leaves out is the
	// removal.
	membersTool := string(authz.ActionChatMembers)
	members.Build = func(a map[string]any) (exec.Request, error) {
		sent := maps.Clone(a)
		delete(sent, RemovesArgument)
		return act(membersTool, Args(sent))
	}
	direct := chatWrite("send_direct_message", authz.ActionChatDirect, false,
		"send direct messages",
		"Propose a direct message, opening the conversation if it is not open yet. The "+
			"person sees who it goes to before anything is sent.",
		object([]string{"to", "body"}, map[string]*schema{
			"to": names(fmt.Sprintf("The people it goes to, each a seat's handle or name, "+
				"up to %d. The person is in the conversation too.", chatDirectOthers),
				chatDirectOthers),
			"body":  text("The message, in Markdown.", chatBodyBytes),
			"links": links,
		}))
	direct.Names = map[string]Directory{"to": DirSeat}
	return []Tool{
		chatWrite("post_chat_message", authz.ActionChatPost, false, "post in chat rooms",
			"Propose a message in a chat room. The person sees the room and who can read it "+
				"before anything is posted. To answer a message, reply in its thread with "+
				"reply_in_chat_thread.",
			object([]string{"channel", "body"}, map[string]*schema{
				"channel": room,
				"body": text("The message, in Markdown. Name somebody with @handle to "+
					"tell them.", chatBodyBytes),
				"links": links,
			})),
		chatWrite("reply_in_chat_thread", authz.ActionChatReply, false, "reply in chat threads",
			"Propose a reply to a chat message, in its thread. The person sees the message "+
				"it answers and who can read it before anything is posted.",
			object([]string{"channel", "reply_to", "body"}, map[string]*schema{
				"channel": room,
				"reply_to": text("The id of the message this answers. A reply to a reply "+
					"joins the same thread.", nameBytes),
				"body": text("The reply, in Markdown. Name somebody with @handle to tell "+
					"them.", chatBodyBytes),
				"links": links,
			})),
		direct,
		chatWrite("edit_chat_message", authz.ActionChatEdit, true, "edit chat messages",
			"Propose new words for a chat message the person wrote. The person sees the old "+
				"words beside the new ones before anything changes.",
			object([]string{"channel", "message_id", "body"}, map[string]*schema{
				"channel":    room,
				"message_id": text("The id of the message to rewrite.", nameBytes),
				"body":       text("The whole new message, replacing the old words.", chatBodyBytes),
				"links": linkList("The message's links, replacing them. An empty list " +
					"removes them; leave it out to keep them."),
			})),
		chatWrite("delete_chat_message", authz.ActionChatDelete, true, "delete chat messages",
			"Propose taking a chat message back. Its words go and its place stays, so a "+
				"thread under it keeps its first message. The person sees the words it takes "+
				"down, so it must be a message in a room they can read.",
			object([]string{"channel", "message_id"}, map[string]*schema{
				"channel":    room,
				"message_id": text("The id of the message to take back.", nameBytes),
			})),
		chatWrite("update_chat_room", authz.ActionChatUpdate, true, "change chat rooms",
			"Propose a chat room's new topic or purpose, or archiving it or opening it again. "+
				"Only what you name changes. Archiving is for the room's creator, its team's "+
				"lead or an administrator.",
			object([]string{"channel"}, map[string]*schema{
				"channel": room,
				"topic": text("What is being discussed now. An empty string clears it.",
					chatTopicBytes),
				"purpose": text("What the room is for. An empty string clears it.",
					chatPurposeBytes),
				"archived": flag("True closes the room to new messages; false opens it again."),
			})),
		members,
		chatWrite("leave_chat_room", authz.ActionChatLeave, false, "leave chat rooms",
			"Propose taking the person out of a chat room. Leaving a private room ends their "+
				"access to it until a member adds them back.",
			object([]string{"channel"}, map[string]*schema{"channel": room})),
	}
}

// linkList is a message's links as a chat write takes them: at most
// [chatLinks], each as long as a room takes one, which is far longer than a
// name ([names]): an address with its query is often past a hundred bytes.
// Its description ends with what a link is ([chatLinkShape]).
func linkList(description string) *schema {
	return &schema{kind: "array", description: description + " " + chatLinkShape,
		maxItems: chatLinks, items: &schema{kind: "string", maxLen: chatLinkBytes}}
}

// chatLinkShape is what a room takes as a link, as a model is told it: an
// absolute address in one of internal/chatlet's LinkSchemes. The room refuses
// any other only when the approved write reaches it, so this is where the
// model learns the rule before a person sees the card. internal/agent/builtin's
// catalogue walk holds it to that set.
const chatLinkShape = "Each is an absolute http://, https:// or mailto: address " +
	"written whole, as https://example.com/page or mailto:name@example.com."

// chatWrite is a chat write: a write on the act route whose preview names who
// reads the result, since every chat write lands in front of the room's
// readers (D09).
func chatWrite(name string, verb authz.Action, destructive bool, phrase, description string,
	s *schema) Tool {
	t := write(name, string(verb), destructive, phrase, description, s)
	t.ChangesVisibility = true
	return t
}

// room is a chat room as its read answers it.
type room struct {
	id, kind, name, topic, purpose string
	archived                       bool
	members                        []roomMember
}

// roomMember is one member of a room: as its read answers them, or as a
// membership change leaves them.
type roomMember struct {
	handle    string
	followAll bool
}

// readRoom reads one room through the grant, named as the call names it: by
// its name as people write it ("#launch") or by its id, which the room's read
// resolves by the one rule every surface reads a typed room by. A room the
// person may not read is the route's own not found, which is what it answers
// for a room nobody holds too.
//
// THE ID IT ANSWERS IS WHAT THE WRITE IS SENT WITH ([roomRequest]) and what
// the change is bound to, so a name is resolved once, here, and the card and
// the write name one room.
func readRoom(ctx context.Context, read proposal.Read, ref string) (room, error) {
	if strings.TrimSpace(ref) == "" {
		return room{}, fmt.Errorf("%w: channel names no room", ErrInvalidArguments)
	}
	doc, err := readGet(ctx, read, chatRoomRoute, map[string]string{"channel_id": ref}, nil)
	if err != nil {
		return room{}, err
	}
	out := room{id: value(doc, "channel.id"), kind: value(doc, "channel.kind"),
		name: value(doc, "channel.name"), topic: value(doc, "channel.topic"),
		purpose:  value(doc, "channel.purpose"),
		archived: value(doc, "channel.archived_at") != ""}
	if out.id == "" {
		return room{}, fmt.Errorf("%w: the room was answered without its id to bind the "+
			"change to", ErrPreviewRead)
	}
	list, _ := cards.Lookup(doc, "members").([]any)
	for _, m := range list {
		out.members = append(out.members, roomMember{handle: value(m, "handle"),
			followAll: value(m, "follow_all") == "true"})
	}
	return out, nil
}

// readByAll reports a room anybody in the company may read (internal/chatlet's
// Visible): a public room and a unit's. Every other kind is read by its
// members alone.
func (r room) readByAll() bool { return r.kind == "public" || r.kind == "unit" }

// direct reports a direct conversation, which is the people in it.
func (r room) direct() bool { return r.kind == "dm" || r.kind == "group" }

// called is how a card names the room: #launch for a named room, and what it
// is for one with no name, whose readers the card names beside it.
func (r room) called() string {
	switch {
	case r.name != "":
		return "#" + r.name
	case r.direct():
		return "Direct conversation"
	}
	return "Unnamed room"
}

// handles are the room's members' handles.
func (r room) handles() []string { return handlesOf(r.members) }

// member reports whether handle is in the room.
func (r room) member(handle string) bool {
	return slices.ContainsFunc(r.members, func(m roomMember) bool { return m.handle == handle })
}

// readers are who reads the room once a write leaves members in it, as a card
// names them (D09): the whole company for a room anybody may read, and
// otherwise the members, each as name says them.
func (r room) readers(name func(handle string) string, members []string) []string {
	if r.readByAll() {
		return []string{audienceLabel("company")}
	}
	return namedOnCard(name, members)
}

// bound is the revision a change of the room binds: the room as a card shows
// it, its settings and its whole membership, as a digest
// ([proposal.RevisionRoom]).
//
// WHAT THE CARD SHOWS, AND NOT THE ROOM'S `revision`. That number is the
// room's own log revision taken with its high-water mark, and every message
// said in the room moves the mark, so a change bound to it would go stale
// whenever anybody spoke. What a chat write is shown against is the room's
// settings (a topic it replaces, an archive it moves) and who reads it (the
// audience it names, the members it replaces), and those move only when
// somebody changes them.
func (r room) bound() string {
	type member struct {
		Handle    string `json:"handle"`
		FollowAll bool   `json:"follow_all"`
	}
	shown := struct {
		Topic    string   `json:"topic"`
		Purpose  string   `json:"purpose"`
		Archived bool     `json:"archived"`
		Members  []member `json:"members"`
	}{Topic: r.topic, Purpose: r.purpose, Archived: r.archived, Members: []member{}}
	for _, m := range r.members {
		shown.Members = append(shown.Members, member{Handle: m.handle, FollowAll: m.followAll})
	}
	// IN ONE ORDER whatever order the read answered them in, so a room that
	// did not change re-reads as the revision it was shown at.
	slices.SortFunc(shown.Members, func(a, b member) int { return strings.Compare(a.Handle, b.Handle) })
	// Strings and truths, which always encode.
	encoded, _ := json.Marshal(shown)
	sum := sha256.Sum256(encoded)
	return proposal.Revision{Kind: proposal.RevisionRoom, ID: r.id,
		Version: hex.EncodeToString(sum[:16])}.String()
}

// roomRequest is a chat write's request with its room as the room's read
// answered it: by its id, whatever the call named it by, so the write the
// person approves goes to the room the card shows. The call itself was held
// to the tool's schema before anything was read, so a malformed one is
// refused without a read.
func roomRequest(t Tool, args map[string]any, r room) (exec.Request, error) {
	pinned := maps.Clone(args)
	pinned["channel"] = r.id
	return built(t, pinned)
}

// openRoom reads the room a message is posted in, refusing one that takes no
// new messages: a card the person approves only to watch the post refused is
// a change the engine already knew it could not make.
func openRoom(ctx context.Context, read proposal.Read, id string) (room, error) {
	r, err := readRoom(ctx, read, id)
	if err != nil {
		return room{}, err
	}
	if r.archived {
		return room{}, fmt.Errorf("%w: %s is archived, so it takes no new messages. It has "+
			"to be opened again first", ErrInvalidArguments, r.called())
	}
	return r, nil
}

// handlesOf are members' handles, in their order.
func handlesOf(members []roomMember) []string {
	out := make([]string, 0, len(members))
	for _, m := range members {
		out = append(out, m.handle)
	}
	return out
}

// namedOnCard is handles as a card names them: each as name says it, the
// first [listNamed] one by one and how many more after, and nobody for none.
func namedOnCard(name func(handle string) string, handles []string) []string {
	if len(handles) == 0 {
		return []string{"Nobody"}
	}
	out := make([]string, 0, min(len(handles), listNamed)+1)
	for i, h := range handles {
		if i == listNamed {
			out = append(out, fmt.Sprintf("and %d more", len(handles)-listNamed))
			break
		}
		out = append(out, name(h))
	}
	return out
}

// chatMessage is one chat message as the thread read answers it.
type chatMessage struct {
	room, id, author, body string
	links                  []string
	deleted                bool
	// version is the position of the record that last changed it: what an
	// edit and a delete move, and a reaction does not.
	version string
}

// readMessage reads one message of a room through the grant.
func readMessage(ctx context.Context, read proposal.Read, roomID, id string) (chatMessage, error) {
	doc, err := readGet(ctx, read, chatThreadRoute,
		map[string]string{"channel_id": roomID, "root_id": id},
		// THE MESSAGE ALONE: a thread read answers a page of the replies
		// under it too, which no preview reads.
		url.Values{"limit": {"1"}})
	if err != nil {
		return chatMessage{}, err
	}
	root := cards.Lookup(doc, "root")
	m := chatMessage{room: roomID, id: value(root, "message.id"),
		author: value(root, "message.author"), body: value(root, "message.body"),
		deleted: value(root, "message.deleted_at") != ""}
	generation, seq := value(root, "position.generation"), value(root, "position.seq")
	if m.id == "" || seq == "" {
		return chatMessage{}, fmt.Errorf("%w: the message was answered without its id and "+
			"position to bind the change to", ErrPreviewRead)
	}
	m.version = orDefault(generation, "0") + ":" + seq
	list, _ := cards.Lookup(root, "message.links").([]any)
	for _, l := range list {
		m.links = append(m.links, cards.Text(l))
	}
	return m, nil
}

// bound is the revision a change of the message binds.
func (m chatMessage) bound() string {
	return proposal.Revision{Kind: proposal.RevisionMessage,
		ID: url.PathEscape(m.room) + "/" + url.PathEscape(m.id), Version: m.version}.String()
}

// quote is the message a reply answers, as its card names it: whose words, as
// name says the author, and the words on one line.
func (m chatMessage) quote(name func(handle string) string) string {
	if m.deleted {
		return name(m.author) + ": a message that was taken back"
	}
	said := strings.Join(strings.Fields(redact.Secrets(m.body)), " ")
	return name(m.author) + ": " + textcut.Within(said, cardLineBytes)
}

// chatRevision re-reads a room's or a message's revision now, for [Revision].
// One gone since its preview, or out of the person's reach, has moved as far
// as anything can ([proposal.ErrGone]).
func chatRevision(ctx context.Context, read proposal.Read, r proposal.Revision) (string, error) {
	now, err := chatRevisionNow(ctx, read, r)
	var refused *PreviewRefusal
	if errors.As(err, &refused) && refused.Status == http.StatusNotFound {
		return "", proposal.ErrGone
	}
	return now, err
}

// chatRevisionNow reads the room or the message r names, and the revision it
// is at now.
func chatRevisionNow(ctx context.Context, read proposal.Read, r proposal.Revision) (string, error) {
	if r.Kind == proposal.RevisionRoom {
		at, err := readRoom(ctx, read, r.ID)
		if err != nil {
			return "", err
		}
		return at.bound(), nil
	}
	roomPart, messagePart, _ := strings.Cut(r.ID, "/")
	roomID, roomErr := url.PathUnescape(roomPart)
	messageID, messageErr := url.PathUnescape(messagePart)
	if roomErr != nil || messageErr != nil || roomID == "" || messageID == "" {
		return "", fmt.Errorf("%w: revision %s names no message", proposal.ErrInvalid,
			r.String())
	}
	m, err := readMessage(ctx, read, roomID, messageID)
	if err != nil {
		return "", err
	}
	return m.bound(), nil
}

// said adds what a message says: its words, and the links it points at.
func (c *changes) said(a Args) {
	c.move("body", "Message", "", redact.Secrets(a.String("body")))
	if row, ok := proposal.List("links", "Links", nil, redactAll(a.Strings("links"))); ok {
		*c = append(*c, row)
	}
}

// redactAll is values, each through redact.Secrets.
func redactAll(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, redact.Secrets(v))
	}
	return out
}

func chatPostPreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
		if _, err := built(t, args); err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		seats, err := companyOf(ctx, read)
		if err != nil {
			return proposal.Preview{}, err
		}
		r, err := openRoom(ctx, read, a.String("channel"))
		if err != nil {
			return proposal.Preview{}, err
		}
		req, err := roomRequest(t, args, r)
		if err != nil {
			return proposal.Preview{}, err
		}
		var c changes
		c.fact("room", "Room", r.called())
		c.said(a)
		return proposal.Preview{Title: "Post in room", Target: r.called(),
			Note: "Everyone it names with @ is told, and so is everyone who follows the " +
				"whole room.",
			ApproveLabel: "Approve and post", Changes: c.rows(), Request: req,
			Audience: r.readers(seats.seatName, r.handles()), SourceRevision: r.bound()}, nil
	}
}

func chatReplyPreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
		if _, err := built(t, args); err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		seats, err := companyOf(ctx, read)
		if err != nil {
			return proposal.Preview{}, err
		}
		r, err := openRoom(ctx, read, a.String("channel"))
		if err != nil {
			return proposal.Preview{}, err
		}
		req, err := roomRequest(t, args, r)
		if err != nil {
			return proposal.Preview{}, err
		}
		answered, err := readMessage(ctx, read, r.id, a.String("reply_to"))
		if err != nil {
			return proposal.Preview{}, err
		}
		var c changes
		c.fact("room", "Room", r.called())
		c.fact("reply_to", "Replying to", answered.quote(seats.seatName))
		c.said(a)
		return proposal.Preview{Title: "Reply in thread", Target: r.called(),
			Note:         "Everyone in the thread is told, and so is everyone it names with @.",
			ApproveLabel: "Approve and reply", Changes: c.rows(), Request: req,
			Audience: r.readers(seats.seatName, r.handles()), SourceRevision: r.bound()}, nil
	}
}

// chatDirectPreview names who a direct message reaches. It reads no room: a
// conversation is the people in it, which the call itself names, so nothing
// it reaches can move before the approval.
func chatDirectPreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
		req, err := built(t, args)
		if err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		seats, err := companyOf(ctx, read)
		if err != nil {
			return proposal.Preview{}, err
		}
		viewer, err := readGet(ctx, read, "GET /viewer", nil, nil)
		if err != nil {
			return proposal.Preview{}, err
		}
		self := value(viewer, "handle")
		var others, named []string
		for _, h := range a.Strings("to") {
			if h != self && !slices.Contains(others, h) {
				others = append(others, h)
				named = append(named, seats.seatName(h))
			}
		}
		to := strings.Join(named, ", ")
		if len(others) == 0 {
			to = "Only you"
		}
		row := proposal.Fact("to", "To", to)
		row.Items = named
		c := changes{row}
		c.said(a)
		return proposal.Preview{Title: "Send direct message", Target: to,
			Note:         "Everyone it goes to is told.",
			ApproveLabel: "Approve and send", Changes: c.rows(), Request: req,
			Audience: namedOnCard(seats.seatName, append([]string{self}, others...))}, nil
	}
}

func chatEditPreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
		if _, err := built(t, args); err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		seats, err := companyOf(ctx, read)
		if err != nil {
			return proposal.Preview{}, err
		}
		viewer, err := readGet(ctx, read, "GET /viewer", nil, nil)
		if err != nil {
			return proposal.Preview{}, err
		}
		r, err := readRoom(ctx, read, a.String("channel"))
		if err != nil {
			return proposal.Preview{}, err
		}
		req, err := roomRequest(t, args, r)
		if err != nil {
			return proposal.Preview{}, err
		}
		m, err := readMessage(ctx, read, r.id, a.String("message_id"))
		if err != nil {
			return proposal.Preview{}, err
		}
		switch {
		case m.deleted:
			return proposal.Preview{}, fmt.Errorf("%w: the message was taken back, so it has "+
				"no words left to edit", ErrInvalidArguments)
		case m.author != value(viewer, "handle"):
			// ONLY ITS AUTHOR REWRITES A MESSAGE, whatever else they may do
			// in the room: an edit puts words in somebody's mouth.
			return proposal.Preview{}, fmt.Errorf("%w: only the person who wrote a message "+
				"can edit it, and %s wrote this one", ErrInvalidArguments,
				seats.seatName(m.author))
		}
		var c changes
		c.fact("room", "Room", r.called())
		c.fact("author", "Written by", seats.seatName(m.author))
		c.move("body", "Message", redact.Secrets(m.body), redact.Secrets(a.String("body")))
		if _, given := args["links"]; given {
			row, moved := proposal.List("links", "Links", redactAll(m.links),
				redactAll(a.Strings("links")))
			if moved {
				c = append(c, row)
			}
		}
		if len(c) < 3 {
			return proposal.Preview{}, fmt.Errorf("%w: the edit leaves the message exactly as "+
				"it is", ErrInvalidArguments)
		}
		return proposal.Preview{Title: "Edit message", Target: r.called(),
			Note: "Anybody the new words name with @ who was not named before is told. " +
				"Nobody else is woken again.",
			ApproveLabel: "Approve and edit", Changes: c.rows(), Request: req,
			Audience: r.readers(seats.seatName, r.handles()), SourceRevision: m.bound()}, nil
	}
}

func chatDeletePreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
		if _, err := built(t, args); err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		seats, err := companyOf(ctx, read)
		if err != nil {
			return proposal.Preview{}, err
		}
		r, err := readRoom(ctx, read, a.String("channel"))
		if err != nil {
			return proposal.Preview{}, err
		}
		req, err := roomRequest(t, args, r)
		if err != nil {
			return proposal.Preview{}, err
		}
		m, err := readMessage(ctx, read, r.id, a.String("message_id"))
		if err != nil {
			return proposal.Preview{}, err
		}
		if m.deleted {
			return proposal.Preview{}, fmt.Errorf("%w: the message is already taken back",
				ErrInvalidArguments)
		}
		var c changes
		c.fact("room", "Room", r.called())
		c.fact("author", "Written by", seats.seatName(m.author))
		c.move("body", "Message", redact.Secrets(m.body), "")
		if row, removed := proposal.List("links", "Links", redactAll(m.links), nil); removed {
			c = append(c, row)
		}
		// WHAT GOES LEADS THE CARD, as the generic change's deletion does:
		// the words are blanked for good, so it is destructive.
		return proposal.Preview{Title: "Delete message", Target: r.called(),
			Summary: "This deletes the message " + seats.seatName(m.author) + " wrote in " +
				r.called() + ", for everyone who can read it.",
			Note: "Its place stays, so a thread under it keeps its first message. Nobody is " +
				"told, and its words cannot be brought back.",
			Risk:         wire.RiskDestructive,
			ApproveLabel: "Approve and delete", Changes: c.rows(), Request: req,
			Audience: r.readers(seats.seatName, r.handles()), SourceRevision: m.bound()}, nil
	}
}

func chatRoomPreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
		if _, err := built(t, args); err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		seats, err := companyOf(ctx, read)
		if err != nil {
			return proposal.Preview{}, err
		}
		r, err := readRoom(ctx, read, a.String("channel"))
		if err != nil {
			return proposal.Preview{}, err
		}
		req, err := roomRequest(t, args, r)
		if err != nil {
			return proposal.Preview{}, err
		}
		var c changes
		c.fact("room", "Room", r.called())
		if _, given := args["topic"]; given {
			c.move("topic", "Topic", redact.Secrets(r.topic), redact.Secrets(a.String("topic")))
		}
		if _, given := args["purpose"]; given {
			c.move("purpose", "Purpose", redact.Secrets(r.purpose),
				redact.Secrets(a.String("purpose")))
		}
		note := "Everyone who can read " + r.called() + " sees the change."
		if archive, given := args["archived"].(bool); given && archive != r.archived {
			c = append(c, proposal.Changed("archived", "State", roomState(r.archived),
				roomState(archive)))
			note = "People can post in " + r.called() + " again."
			if archive {
				note = "Nobody can post in " + r.called() + " until it is opened again. " +
					"Everything said in it stays readable and searchable."
			}
		}
		if len(c) < 2 {
			return proposal.Preview{}, fmt.Errorf("%w: the change leaves %s exactly as it is",
				ErrInvalidArguments, r.called())
		}
		return proposal.Preview{Title: "Update room", Target: r.called(), Note: note,
			ApproveLabel: "Approve and update", Changes: c.rows(), Request: req,
			Audience: r.readers(seats.seatName, r.handles()), SourceRevision: r.bound()}, nil
	}
}

// roomState is a room's archive as a card shows it.
func roomState(archived bool) string {
	if archived {
		return "Archived"
	}
	return "Open"
}

// chatMembersPreview shows a membership stated whole as the change it makes to
// the room as it is now: who is added, who is removed and whose following
// moves, each one row. A member the call leaves out is a removal only where
// the call names them in `removes` (ADR-0055), and a member given without
// follow_all keeps how they follow the room, which the preview writes into the
// request, so the approved membership is the one the card shows.
func chatMembersPreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
		a := Args(args)
		seats, err := companyOf(ctx, read)
		if err != nil {
			return proposal.Preview{}, err
		}
		r, err := readRoom(ctx, read, a.String("channel"))
		if err != nil {
			return proposal.Preview{}, err
		}
		switch {
		case r.kind == "unit":
			return proposal.Preview{}, fmt.Errorf("%w: %s is a unit's own room, and the org "+
				"chart decides who is in it. Change the unit in the company document "+
				"instead", ErrInvalidArguments, r.called())
		case r.direct():
			return proposal.Preview{}, fmt.Errorf("%w: a direct conversation is the people in "+
				"it, so who is in it cannot change. Start a new conversation instead",
				ErrInvalidArguments)
		}
		next, written, faults := membersAfter(r, args["members"])
		// A MEMBER WHO JOINS IS A SEAT ON THE CHART. The turn keeps a handle
		// it cannot place ([DirMember]), because one who stays may be a seat
		// the chart no longer lists; one who joins is not in the room yet, so
		// a handle no seat holds is a guess, refused with the seats as the
		// resolver refuses a name ([match]). EVERY ONE IS NAMED, beside every
		// other fault of the membership, in one refusal (ADR-0059).
		var notes []string
		for i, m := range next {
			if _, seat := seats.seats[m.handle]; !seat && !r.member(m.handle) {
				faults = append(faults, fmt.Errorf("%s.handle: %q is not one of the seats you "+
					"can see", written[i], m.handle))
				notes = []string{choices("seats", seats.entries())}
			}
		}
		if len(faults) > 0 {
			return proposal.Preview{}, refusedFor(faults, notes...)
		}
		var removed []string
		for _, h := range r.handles() {
			if !slices.ContainsFunc(next, func(m roomMember) bool { return m.handle == h }) {
				removed = append(removed, h)
			}
		}
		slices.Sort(removed)
		if refusal := removalsNamed(r, removed, a.Strings(RemovesArgument)); refusal != nil {
			return proposal.Preview{}, refusal
		}
		completed := maps.Clone(args)
		completed["members"] = membersArgument(next)
		req, err := roomRequest(t, completed, r)
		if err != nil {
			return proposal.Preview{}, err
		}
		var c changes
		c.fact("room", "Room", r.called())
		for _, m := range next {
			was := slices.IndexFunc(r.members, func(held roomMember) bool {
				return held.handle == m.handle
			})
			name := seats.seatName(m.handle)
			switch {
			case was < 0:
				shown := name
				if m.followAll {
					shown += " (follows the whole room)"
				}
				c = append(c, proposal.Added("members."+m.handle, "Member", shown, false))
			case r.members[was].followAll != m.followAll:
				c = append(c, proposal.Changed("members."+m.handle, name,
					following(r.members[was].followAll), following(m.followAll)))
			}
		}
		var removes []string
		for _, h := range removed {
			name := seats.seatName(h)
			removes = append(removes, name)
			c = append(c, proposal.Removed("members."+h, "Member", name))
		}
		if len(c) < 2 {
			return proposal.Preview{}, fmt.Errorf("%w: the change leaves who is in %s exactly "+
				"as it is", ErrInvalidArguments, r.called())
		}
		note := "Anybody in the company can read " + r.called() + " either way. Its members " +
			"are told about what is said in it."
		if !r.readByAll() {
			note = "Only its members can read " + r.called() + ", so whoever is taken out can " +
				"no longer read it and whoever is added can read all of it."
		}
		p := proposal.Preview{Title: "Change room members", Target: r.called(), Note: note,
			ApproveLabel: "Approve and change", Changes: c.rows(), Request: req,
			Audience: r.readers(seats.seatName, handlesOf(next)), SourceRevision: r.bound(),
			Removes: removes}
		if len(removes) > 0 {
			// WHAT GOES LEADS THE CARD (ADR-0055), as a member the generic
			// change removes does: everyone it takes out, by name, above
			// every row.
			p.Summary = "This removes " + inWords(removes) + " from " + r.called() + "."
			p.Risk = wire.RiskDestructive
		}
		return p, nil
	}
}

// membersAfter is the membership a call states, in its order, and where the
// call wrote each member (`members[2]`): a member given without follow_all
// keeps how they follow the room now, and a new one follows only what names
// them, as the chat tool reads a bare handle. A member named twice, or by no
// handle, is a fault of the call, which would state two memberships at once;
// EVERY ONE IS A FAULT OF ITS OWN, a blank handle by where it was written and a
// repeated one by every place it was (ADR-0059), and the membership is the
// members the call states once each.
func membersAfter(r room, raw any) ([]roomMember, []string, []error) {
	list, _ := raw.([]any)
	out := make([]roomMember, 0, len(list))
	written := make([]string, 0, len(list))
	var faults []error
	places := map[string][]string{}
	var repeated []string
	for i, e := range list {
		at := fmt.Sprintf("members[%d]", i)
		entry, _ := e.(map[string]any)
		handle := strings.TrimSpace(cards.Text(entry["handle"]))
		if handle == "" {
			faults = append(faults, fmt.Errorf("%s.handle is empty", at))
			continue
		}
		places[handle] = append(places[handle], at)
		switch len(places[handle]) {
		case 1:
		case 2:
			repeated = append(repeated, handle)
			continue
		default:
			continue
		}
		follow, given := entry["follow_all"].(bool)
		if held := slices.IndexFunc(r.members, func(m roomMember) bool {
			return m.handle == handle
		}); !given && held >= 0 {
			follow = r.members[held].followAll
		}
		out = append(out, roomMember{handle: handle, followAll: follow})
		written = append(written, at)
	}
	for _, handle := range repeated {
		faults = append(faults, fmt.Errorf("%s each name %s; name a member once",
			inWords(places[handle]), handle))
	}
	return out, written, faults
}

// membersArgument is a membership as the chat tool takes it, every member's
// following stated, so the request carries exactly the membership the card
// shows.
func membersArgument(members []roomMember) []any {
	out := make([]any, 0, len(members))
	for _, m := range members {
		out = append(out, map[string]any{"handle": m.handle, "follow_all": m.followAll})
	}
	return out
}

// removalsNamed holds a membership's removals to the call's `removes`, as
// [removalCheck] holds a replace's: every member it removes is one the call
// names, and every one it names is one it removes, so the card never warns of
// a removal that will not happen. BOTH FAULTS, EVERY ONE OF EACH, ARE ONE
// REFUSAL (ADR-0059). Its refusals name handles, which is how the model writes
// `removes` back.
func removalsNamed(r room, removed, declared []string) error {
	var undeclared, stays, absent []string
	for _, h := range removed {
		if !slices.Contains(declared, h) {
			undeclared = append(undeclared, h)
		}
	}
	for _, h := range declared {
		switch {
		case slices.Contains(removed, h):
		case r.member(h):
			stays = append(stays, h)
		default:
			absent = append(absent, h)
		}
	}
	var said, hints []string
	if len(undeclared) > 0 {
		said = append(said, "This would remove "+inWords(undeclared)+" from "+r.called()+".")
		hints = append(hints, "Include them in members to keep them, or list them in "+
			"removes if the person asked for that.")
	}
	if len(stays) > 0 || len(absent) > 0 {
		var named []string
		if len(stays) > 0 {
			named = append(named, inWords(stays)+", who "+staysIn(len(stays))+" "+r.called())
		}
		if len(absent) > 0 {
			named = append(named, inWords(absent)+", who "+isNotIn(len(absent))+" "+r.called())
		}
		said = append(said, "`removes` names "+strings.Join(named, ", and ")+".")
		hints = append(hints, "List in removes only the members the change takes out.")
	}
	if len(said) == 0 {
		return nil
	}
	return &PreviewRefusal{Status: http.StatusUnprocessableEntity, Refusal: wire.Refusal{
		Code: removalCode, Message: strings.Join(said, " "), Hint: strings.Join(hints, " "),
	}}
}

// staysIn and isNotIn are the verbs a removal refusal says of n members.
func staysIn(n int) string {
	if n == 1 {
		return "stays in"
	}
	return "stay in"
}

func isNotIn(n int) string {
	if n == 1 {
		return "is not in"
	}
	return "are not in"
}

// following is how a member follows a room, as a card shows it.
func following(all bool) string {
	if all {
		return "Follows the whole room"
	}
	return "Follows only what names them"
}

func chatLeavePreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
		if _, err := built(t, args); err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		seats, err := companyOf(ctx, read)
		if err != nil {
			return proposal.Preview{}, err
		}
		viewer, err := readGet(ctx, read, "GET /viewer", nil, nil)
		if err != nil {
			return proposal.Preview{}, err
		}
		self := value(viewer, "handle")
		r, err := readRoom(ctx, read, a.String("channel"))
		if err != nil {
			return proposal.Preview{}, err
		}
		req, err := roomRequest(t, args, r)
		if err != nil {
			return proposal.Preview{}, err
		}
		switch {
		case r.kind == "unit":
			return proposal.Preview{}, fmt.Errorf("%w: %s is a unit's own room, and the org "+
				"chart decides who is in it. Leaving it would be undone the next time the "+
				"company's configuration is applied", ErrInvalidArguments, r.called())
		case r.direct():
			return proposal.Preview{}, fmt.Errorf("%w: a direct conversation is the people in "+
				"it, so nobody can leave it", ErrInvalidArguments)
		case !r.member(self):
			return proposal.Preview{}, fmt.Errorf("%w: the person is not in %s",
				ErrInvalidArguments, r.called())
		}
		var stay []string
		for _, h := range r.handles() {
			if h != self {
				stay = append(stay, h)
			}
		}
		var c changes
		c.fact("room", "Room", r.called())
		c = append(c, proposal.Removed("member", "Member", seats.seatName(self)))
		note := "You can join " + r.called() + " again at any time."
		if !r.readByAll() {
			note = "You can no longer read " + r.called() + ", and only a member can add " +
				"you back."
		}
		// A MEMBER TAKEN OUT, the person themself: destructive as any
		// member's removal is, what goes leading the card.
		return proposal.Preview{Title: "Leave room", Target: r.called(),
			Summary: "This removes you from " + r.called() + ".", Note: note,
			Risk:         wire.RiskDestructive,
			ApproveLabel: "Approve and leave", Changes: c.rows(), Request: req,
			Audience: r.readers(seats.seatName, stay), SourceRevision: r.bound()}, nil
	}
}
