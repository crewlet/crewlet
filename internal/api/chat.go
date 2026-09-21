package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opsmcp"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE COMPANY'S OWN CHAT, served to a person.
//
// # The one rule the whole surface is built around
//
// THE VIEWER IS RESOLVED BY THE SERVER, ON EVERY CALL, AND A CALLER MAY NEVER
// NAME A SEAT. A person in chat IS a `kind: human` seat whose
// `contact.crewlet_operator_id` names one of Tier A's `api.auth.tokens`, and
// the chain from the presented credential to that seat is walked here, per
// request, by [opsmcp.ChatActor] — the same function the operator MCP surface
// resolves its author with, because two implementations of "who is this" is
// two answers to the only question that decides what a transcript may show.
//
// A TOKEN BOUND TO NO SEAT GETS NOTHING, READS INCLUDED, and the refusal names
// `contact.crewlet_operator_id` because the remedy is a line of company
// configuration rather than a different credential. That is stricter than
// every other personal surface in this tree — the tracker's `work_person` is
// scoped rather than operator-only, and the wiki records a credential as an
// author of its own kind — and it is deliberate: a private room's contents are
// decided by its membership, a caller the engine cannot resolve to a seat has
// no membership anywhere, and a pipeline's credential is not a person.
//
// # Reads are not here
//
// Every chat READ is a registered question in internal/api/queries, reached by
// a named route in rest.go exactly as the board's and the wiki's are. That is
// not tidiness: the socket's query channel is a thin adapter over the same
// function each REST route calls, and a read implemented here would be the
// second implementation that makes the two surfaces disagree. What lives in
// this file is what a question cannot be — a write.
//
// # A write does not push
//
// None of these routes touches the live channel, with one stated exception
// below. A committed record reaches an open socket through the applier's own
// observer ([stream.ChatHub]), which every node runs — so a message posted
// through this node appears on a tab connected to another one. A push from the
// route would reach only the sockets of whichever node happened to serve the
// request, and would arrive BEFORE the rows it describes.
//
// The exception is the read cursor, and it is an exception because it is not
// on the log at all: where somebody's eye has reached is a fact about one
// reader's attention that nobody replays, it lives in coordination, and the
// only thing in the world that wants it is that same person's other tabs.

// MaxChatBody bounds a chat write's request body.
//
// 128 KiB, from the largest gesture this surface accepts: a membership set is
// [chat.MaxMembers] entries and an erase is [chat.MaxEraseMessages] ids, which
// are tens of kibibytes once each is a JSON object with a handle or a uuid in
// it. It is deliberately about twice [chat.MaxRecordBytes] rather than equal
// to it — the record's encoding is smaller than the request's — so this bounds
// the READ and the domain still refuses an oversized value naming its own
// field, which is the refusal a person can act on.
const MaxChatBody = 128 << 10

// ChatWriter is the slice of the company's chat these routes write through.
//
// Declared here, by the consumer, and it is every gesture the value layer
// exports rather than a subset: each route below is one of them, and a seam
// narrower than the surface would be a seam that decides which gestures a
// person may make — which is the guard's job and the write path's, not this
// interface's.
//
// THE ACTOR TRAVELS PER CALL rather than the seam being built per person: one
// node's chat serves every seat and every operator through one write path.
// That is the domain's own shape; see [chat.Actor].
//
// THERE IS NO RENAME, and no follow gesture. A channel's name is the address
// its create arbitrated on and the value layer has no op that moves one; a
// follow is derived by the applier from a message's own mentions. Both are
// stated in [chat.Store]'s own doc, and a route for either would be a call
// that fails every time.
type ChatWriter interface {
	CreateChannel(ctx context.Context, actor chat.Actor, in chat.NewChannel) (
		chat.Written, error)
	// OpenDirect reports whether it CREATED the conversation, because "it
	// was already open" is not a failure: the id is derived from the
	// participants, so the loser of a race is handed the room the winner
	// made.
	OpenDirect(ctx context.Context, actor chat.Actor, participants []string) (
		chat.Written, bool, error)
	PatchChannel(ctx context.Context, actor chat.Actor, channelID string,
		patch chat.ChannelPatch) (chat.Written, error)
	SetMembers(ctx context.Context, actor chat.Actor, channelID string,
		members []chat.Member) (chat.Written, error)
	Join(ctx context.Context, actor chat.Actor, channelID string) (chat.Written, error)
	Leave(ctx context.Context, actor chat.Actor, channelID string) (chat.Written, error)
	Erase(ctx context.Context, actor chat.Actor, channelID string,
		messageIDs []string, reason string) (chat.Written, error)
	Prune(ctx context.Context, actor chat.Actor, channelID string,
		cutoff time.Time) (chat.Written, error)
	Post(ctx context.Context, actor chat.Actor, channelID string,
		in chat.NewMessage) (chat.Written, error)
	Reply(ctx context.Context, actor chat.Actor, channelID, replyTo string,
		in chat.NewMessage) (chat.Written, error)
	Edit(ctx context.Context, actor chat.Actor, channelID, messageID string,
		in chat.EditMessage) (chat.Written, error)
	Delete(ctx context.Context, actor chat.Actor, channelID, messageID string) (
		chat.Written, error)
	React(ctx context.Context, actor chat.Actor, channelID, messageID,
		emoji string) (chat.Written, error)
	Unreact(ctx context.Context, actor chat.Actor, channelID, messageID,
		emoji string) (chat.Written, error)
}

// ChatCursors is the read-cursor half, which is coordination rather than the
// log.
//
// Declared here and kept to the ONE method that writes: a route that could
// also reach the activation pointer or the secret store would eventually be
// given a reason to. The read half is a registered question, like every other
// read on this surface.
type ChatCursors interface {
	AdvanceChatRead(ctx context.Context, handle string, delta coord.ChatReadDelta) (
		coord.ChatReadState, error)
}

// chatViewer resolves an operator id to the seat it is bound to, or "" for a
// credential bound to none.
//
// It is what the live channel identifies a socket with, and it is built from
// the SAME resolution the write routes attribute with, so a person who may
// post in a room is exactly the person whose socket may be shown it.
func chatViewer(actor func(context.Context) (chat.Actor, error)) func(string) string {
	return func(operatorID string) string {
		if operatorID == "" {
			return ""
		}
		who, err := actor(auth.WithOperator(context.Background(), operatorID))
		if err != nil {
			return ""
		}
		return who.Handle
	}
}

// ChatLiveOptions configure the chat arm of the live channel.
//
// See [NewChatLive] for why this is built outside an App.
type ChatLiveOptions struct {
	// Company reads the CURRENT epoch. It is the same accessor
	// [Options.Sources.Company] takes, and handing both the SAME function
	// is what makes "who may write in a room" and "whose socket may be
	// shown it" one answer.
	Company func() *config.Company

	// Rooms is chat's read side, for the visibility filter. A function
	// because the reader does not exist yet when this is built.
	Rooms func() stream.ChatRooms

	// Peers carries presence across the fleet. Nil keeps it local to this
	// node's own sockets.
	Peers func() stream.ChatPeers

	// NodeID names this node in its own presence answer.
	NodeID string
}

// NewChatLive builds the chat arm of the live channel.
//
// # Why this is not built inside [New]
//
// The chat applier takes its observer when the state log comes up, and the
// state log comes up before the API exists — so a hub built by [New] could
// never be the thing the applier announces to. The process therefore builds
// this first, hands it to the engine (as the chat applier's [chat.Observer]
// and as the working indicator's poster) and to [Options.ChatLive], and every
// seam it reads through is a function so that none of them has to exist yet.
//
// What it adds over [stream.NewChatHub] is the one thing this package owns:
// the resolution from a credential to a seat. That is deliberately not the
// caller's to supply — a second resolution would be a second answer to who a
// socket is, and the whole surface rests on there being one.
func NewChatLive(opts ChatLiveOptions) *stream.ChatHub {
	return stream.NewChatHub(stream.ChatHubOptions{
		Rooms:  opts.Rooms,
		Viewer: chatViewer(chatActorFor(opts.Company)),
		Peers:  opts.Peers,
		NodeID: opts.NodeID,
	})
}

// chatActorFor builds the per-request author resolution over the live epoch.
//
// A FUNCTION OF THE COMPANY, read on every call, for the reason every other
// source here is one: an apply replaces the org chart, and a resolution
// captured at boot would keep attributing a message to the handle a renamed
// seat used to have — or keep admitting a token whose binding a revision
// removed.
func chatActorFor(company func() *config.Company) func(context.Context) (chat.Actor, error) {
	resolve := opsmcp.ChatActor(func() *org.Organization {
		if company == nil {
			return nil
		}
		c := company()
		if c == nil {
			return nil
		}
		organization, err := c.Organization()
		if err != nil {
			// A company that will not resolve into an org chart is one
			// no node is running, so there is no seat to be. The
			// refusal below names the binding, which is the same
			// remedy either way.
			return nil
		}
		return organization
	})
	return func(ctx context.Context) (chat.Actor, error) {
		// THE TURN IS NIL AND THERE IS NOWHERE TO PUT ONE. That
		// argument exists for the seat surface, where a write is
		// attributed to the turn's own seat; here the credential is the
		// whole of the identity, which is exactly what makes a caller
		// unable to name one.
		return resolve(ctx, nil)
	}
}

// mountChat registers the chat write surface.
//
// Every route is a POST, including the ones that only set a flag. The
// anonymous-read posture a laptop deployment allows must never reach any of
// them: saying something in a company's rooms, changing who may read one, and
// destroying a year of a conversation are not reads, whatever the posture
// says.
//
// The paths are the read routes' with a gesture on the end. A gesture rather
// than a verb on the resource, because this surface has gestures the HTTP
// method vocabulary cannot name — joining, reacting, erasing, pruning — and a
// surface where half the writes are PATCH and half are POST-with-a-suffix is
// one where a reader has to remember which.
func (a *App) mountChat(mux *http.ServeMux) {
	if a.chat != nil {
		mux.Handle("POST /chat/channels", http.HandlerFunc(a.serveChatCreate))
		mux.Handle("POST /chat/dms", http.HandlerFunc(a.serveChatOpenDirect))
		mux.Handle("POST /chat/channels/{channel_id}/patch", http.HandlerFunc(a.serveChatPatch))
		mux.Handle("POST /chat/channels/{channel_id}/members", http.HandlerFunc(a.serveChatMembers))
		mux.Handle("POST /chat/channels/{channel_id}/join", http.HandlerFunc(a.serveChatJoin))
		mux.Handle("POST /chat/channels/{channel_id}/leave", http.HandlerFunc(a.serveChatLeave))
		mux.Handle("POST /chat/channels/{channel_id}/messages", http.HandlerFunc(a.serveChatPost))
		mux.Handle("POST /chat/channels/{channel_id}/messages/{message_id}/replies",
			http.HandlerFunc(a.serveChatReply))
		mux.Handle("POST /chat/channels/{channel_id}/messages/{message_id}/edit",
			http.HandlerFunc(a.serveChatEdit))
		mux.Handle("POST /chat/channels/{channel_id}/messages/{message_id}/delete",
			http.HandlerFunc(a.serveChatDelete))
		mux.Handle("POST /chat/channels/{channel_id}/messages/{message_id}/react",
			a.chatReaction(false))
		mux.Handle("POST /chat/channels/{channel_id}/messages/{message_id}/unreact",
			a.chatReaction(true))
		// THE TWO THAT DESTROY, beside each other as the tracker's purge
		// sits beside the eviction: an erase removes message rows for
		// ever and a prune removes everything a room said before an
		// instant, neither has an inverse, and both therefore carry a
		// confirmation and are recorded as an OPERATOR's act.
		mux.Handle("POST /chat/channels/{channel_id}/erase", http.HandlerFunc(a.serveChatErase))
		mux.Handle("POST /chat/channels/{channel_id}/prune", http.HandlerFunc(a.serveChatPrune))
	}
	if a.chatCursors != nil {
		mux.Handle("POST /chat/read", http.HandlerFunc(a.serveChatRead))
	}
}

// ---- the author, resolved per request ---------------------------------- //

// chatAuthor resolves the request's credential to the seat it writes as, or
// refuses it.
//
// AUTHOR KIND HUMAN, ALWAYS. The credential is recorded beside the seat rather
// than instead of it, so an audit can still ask what one token did, but the
// name a room renders is the person's. A room where `ci` or `ops-bot` can
// appear as a speaker is one where "who said this" has two vocabularies.
func (a *App) chatAuthor(w http.ResponseWriter, r *http.Request) (chat.Actor, bool) {
	if a.chatActor == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]string{"error": "no_chat"})
		return chat.Actor{}, false
	}
	actor, err := a.chatActor(r.Context())
	if err != nil {
		// 403 AND NOT 401. The credential was accepted — the guard let
		// this request through — and what is missing is an identity in
		// the company. Answering "unauthorized" would send a person
		// looking for a token they already hold, when the remedy is a
		// line of the company document.
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":  "no_seat",
			"detail": err.Error(),
		})
		return chat.Actor{}, false
	}
	return actor, true
}

// chatModerator is the same person, recorded as an OPERATOR's act.
//
// The two destructive gestures demand it in the decide rather than at the
// route ([chat.Store.Erase], [chat.Store.Prune]), and the reason is that an
// author who could erase its own messages could erase the evidence of what it
// did. What makes this an honest escalation rather than a hole is that the
// handle is still the person's own — every chat actor carries one, an operator
// included — so the record says WHO destroyed the messages and under which
// credential, and a request that carries no operator identity is refused here
// by name.
func (a *App) chatModerator(w http.ResponseWriter, r *http.Request) (chat.Actor, bool) {
	actor, ok := a.chatAuthor(w, r)
	if !ok {
		return chat.Actor{}, false
	}
	operator, present := auth.OperatorFrom(r.Context())
	if !present || operator == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "operator_required",
			"detail": "destroying messages is an operator gesture and this " +
				"request carries no operator identity",
		})
		return chat.Actor{}, false
	}
	actor.Kind, actor.OperatorID = chat.AuthorOperator, operator
	return actor, true
}

// ---- the room gestures ------------------------------------------------- //

// chatNewChannel is a room to create, as a caller states it.
//
// The words are the READ side's — `handle`, `follow_all` — rather than the
// record's own `h` and `fa`, which are a byte budget on the busiest log in the
// engine and not a vocabulary anybody types. A create whose fields did not
// match the answer a read gives back would be two spellings of one room.
type chatNewChannel struct {
	Name          string           `json:"name"`
	Kind          chat.Kind        `json:"kind"`
	Topic         string           `json:"topic,omitempty"`
	Purpose       string           `json:"purpose,omitempty"`
	Unit          string           `json:"unit,omitempty"`
	Members       []chatMemberBody `json:"members,omitempty"`
	RetentionDays *int             `json:"retention_days,omitempty"`
}

// chatMemberBody is one member as a caller states them. A POINTER-FREE shape:
// the flag's zero value is a real setting (not following the room in full),
// which is what an absent flag means.
type chatMemberBody struct {
	Handle    string `json:"handle"`
	FollowAll bool   `json:"follow_all,omitempty"`
}

func chatMembersOf(in []chatMemberBody) []chat.Member {
	out := make([]chat.Member, 0, len(in))
	for _, m := range in {
		out = append(out, chat.Member{Handle: m.Handle, FollowAll: m.FollowAll})
	}
	return out
}

func (a *App) serveChatCreate(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.chatAuthor(w, r)
	if !ok {
		return
	}
	var body chatNewChannel
	if !readChatBody(w, r, &body) {
		return
	}
	written, err := a.chat.CreateChannel(r.Context(), actor, chat.NewChannel{
		Name: body.Name, Kind: body.Kind, Topic: body.Topic,
		Purpose: body.Purpose, Unit: body.Unit,
		Members: chatMembersOf(body.Members), RetentionDays: body.RetentionDays,
	})
	a.answerChatWrite(w, r, "create_channel", written, err, nil)
}

// chatDirect is a conversation to open.
type chatDirect struct {
	Participants []string `json:"participants"`
}

// serveChatOpenDirect answers POST /chat/dms.
//
// THE AUTHOR IS ALWAYS ONE OF THE PARTICIPANTS and is added by the write path,
// so a body that lists only the other person opens the conversation a person
// means. A direct conversation's id IS its participant set, which is why this
// route can be pressed twice with no second room: the loser of the race is
// handed the room the winner made, and `created` says which happened.
func (a *App) serveChatOpenDirect(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.chatAuthor(w, r)
	if !ok {
		return
	}
	var body chatDirect
	if !readChatBody(w, r, &body) {
		return
	}
	written, created, err := a.chat.OpenDirect(r.Context(), actor, body.Participants)
	a.answerChatWrite(w, r, "open_direct", written, err,
		map[string]any{"created": created})
}

// serveChatPatch answers POST /chat/channels/{channel_id}/patch.
//
// IT DECODES INTO THE RECORD'S OWN PAYLOAD, because every field a caller may
// change is a field of [chat.ChannelPatch] and two shapes for one set of
// settings is a place for them to drift. The version is the build's and is set
// by the write path, so a caller that sends one is ignored rather than obeyed.
//
// There is no name here, and that is the value layer's own rule rather than an
// omission: a channel's name is the address its create arbitrated on, and this
// build has no record that moves one.
func (a *App) serveChatPatch(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.chatAuthor(w, r)
	if !ok {
		return
	}
	var patch chat.ChannelPatch
	if !readChatBody(w, r, &patch) {
		return
	}
	written, err := a.chat.PatchChannel(r.Context(), actor,
		r.PathValue("channel_id"), patch)
	a.answerChatWrite(w, r, "patch_channel", written, err, nil)
}

// chatMembership is a room's whole membership.
type chatMembership struct {
	Members []chatMemberBody `json:"members"`
}

// serveChatMembers answers POST /chat/channels/{channel_id}/members.
//
// THE SET TRAVELS WHOLE, because a delta cannot rebuild a row on a replay from
// zero and membership is what a private room's readability IS: a node that
// replayed a room's adds and missed one of its removals would serve a
// conversation to somebody who was taken out of it. An empty list is a real
// gesture and is not the zero value's meaning, which is why the field is read
// rather than defaulted.
func (a *App) serveChatMembers(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.chatAuthor(w, r)
	if !ok {
		return
	}
	var body chatMembership
	if !readChatBody(w, r, &body) {
		return
	}
	written, err := a.chat.SetMembers(r.Context(), actor,
		r.PathValue("channel_id"), chatMembersOf(body.Members))
	a.answerChatWrite(w, r, "set_members", written, err, nil)
}

func (a *App) serveChatJoin(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.chatAuthor(w, r)
	if !ok {
		return
	}
	written, err := a.chat.Join(r.Context(), actor, r.PathValue("channel_id"))
	a.answerChatWrite(w, r, "join_channel", written, err, nil)
}

func (a *App) serveChatLeave(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.chatAuthor(w, r)
	if !ok {
		return
	}
	written, err := a.chat.Leave(r.Context(), actor, r.PathValue("channel_id"))
	a.answerChatWrite(w, r, "leave_channel", written, err, nil)
}

// ---- what anybody says ------------------------------------------------- //

// chatMessage is something to say.
type chatMessage struct {
	Body  string   `json:"body"`
	Links []string `json:"links,omitempty"`

	// Mentions are the handles the body named, ALREADY RESOLVED by
	// whichever surface read what somebody typed. The engine does not
	// re-read the prose here: resolving `@ali` to a seat is the composer's
	// job on this surface exactly as it is a seat's own tool's, and the
	// write path drops any handle the company no longer has.
	Mentions []string `json:"mentions,omitempty"`

	// Collective says the message addressed the whole room (`@channel`).
	Collective bool `json:"collective,omitempty"`

	// OperationID is the caller's own idempotency key, and it is REQUIRED
	// — the write path refuses an empty one naming this field. A post
	// arbitrates nothing at the broker, so there is nothing else that
	// could tell a resubmission from a second remark, and the failure of
	// minting one here would be silent: the person sees what they said
	// twice.
	OperationID string `json:"operation_id"`
}

func (m chatMessage) message() chat.NewMessage {
	return chat.NewMessage{
		Body: m.Body, Links: m.Links, Mentions: m.Mentions,
		Collective: m.Collective, OperationID: m.OperationID,
	}
}

func (a *App) serveChatPost(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.chatAuthor(w, r)
	if !ok {
		return
	}
	var body chatMessage
	if !readChatBody(w, r, &body) {
		return
	}
	written, err := a.chat.Post(r.Context(), actor, r.PathValue("channel_id"),
		body.message())
	a.answerChatWrite(w, r, "post_message", written, err, nil)
}

// serveChatReply answers POST /chat/channels/{channel_id}/messages/{message_id}/replies.
//
// THE THREAD IS RESOLVED BY THE WRITE PATH from the message being answered,
// not taken from the caller: a thread here is one level deep, so a reply to a
// reply carries the same root, and a route that accepted a root would let a
// caller file an answer under a thread it does not belong to.
func (a *App) serveChatReply(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.chatAuthor(w, r)
	if !ok {
		return
	}
	var body chatMessage
	if !readChatBody(w, r, &body) {
		return
	}
	written, err := a.chat.Reply(r.Context(), actor, r.PathValue("channel_id"),
		r.PathValue("message_id"), body.message())
	a.answerChatWrite(w, r, "reply_in_thread", written, err, nil)
}

// chatEdit is a message's new content.
type chatEdit struct {
	Body     string   `json:"body"`
	Links    []string `json:"links,omitempty"`
	Mentions []string `json:"mentions,omitempty"`
}

// serveChatEdit answers POST /chat/channels/{channel_id}/messages/{message_id}/edit.
//
// THE DERIVED COLLECTIONS ARE RE-CARRIED, both of them, because the body is
// what they were derived from: an edit that added a mention and did not carry
// the new set would leave the row naming whoever the first draft named. The
// author gate is the write path's and is checked against the row inside its
// own snapshot — a remark somebody else can rewrite is a remark attributed to
// a person who did not make it.
func (a *App) serveChatEdit(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.chatAuthor(w, r)
	if !ok {
		return
	}
	var body chatEdit
	if !readChatBody(w, r, &body) {
		return
	}
	written, err := a.chat.Edit(r.Context(), actor, r.PathValue("channel_id"),
		r.PathValue("message_id"), chat.EditMessage{
			Body: body.Body, Links: body.Links, Mentions: body.Mentions,
		})
	a.answerChatWrite(w, r, "edit_message", written, err, nil)
}

// serveChatDelete tombstones a message: the body goes, the row stays, so a
// thread hung off it still has its root.
func (a *App) serveChatDelete(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.chatAuthor(w, r)
	if !ok {
		return
	}
	written, err := a.chat.Delete(r.Context(), actor, r.PathValue("channel_id"),
		r.PathValue("message_id"))
	a.answerChatWrite(w, r, "delete_message", written, err, nil)
}

// chatReaction answers both the react and the unreact route.
//
// ONE HANDLER FOR BOTH, because they are one gesture with a sign — the record
// is a TOGGLE rather than a set, so two people reacting to one message never
// overwrite each other — and two handlers would be two copies of one refusal
// vocabulary.
func (a *App) chatReaction(remove bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := a.chatAuthor(w, r)
		if !ok {
			return
		}
		var body struct {
			Emoji string `json:"emoji"`
		}
		if !readChatBody(w, r, &body) {
			return
		}
		channel, message := r.PathValue("channel_id"), r.PathValue("message_id")
		var written chat.Written
		var err error
		what := "react_to_message"
		if remove {
			what = "unreact_to_message"
			written, err = a.chat.Unreact(r.Context(), actor, channel, message, body.Emoji)
		} else {
			written, err = a.chat.React(r.Context(), actor, channel, message, body.Emoji)
		}
		a.answerChatWrite(w, r, what, written, err, nil)
	}
}

// ---- the two that destroy ---------------------------------------------- //

// chatErase is what an erase removes and why.
type chatErase struct {
	MessageIDs []string `json:"message_ids"`

	// Reason is REQUIRED here although the record makes it optional. The
	// rows are destroyed and there is no inverse, so the reason is the
	// only account of them that survives — the same rule the tracker's
	// purge route applies for the same reason.
	Reason string `json:"reason"`
}

// serveChatErase answers POST /chat/channels/{channel_id}/erase.
//
// THE CONFIRMATION IS THE COUNT, echoed in `?confirm=`. Repeating the room id
// would confirm nothing — it is already in the URL that carries the request —
// while the number of messages is a value the caller had to establish, which
// is the whole point of asking for one.
func (a *App) serveChatErase(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.chatModerator(w, r)
	if !ok {
		return
	}
	var body chatErase
	if !readChatBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Reason) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "reason_required",
			"detail": "state why in `reason` — the rows are destroyed on every " +
				"node with no inverse, and the history row's reason is the " +
				"only account of them that survives",
		})
		return
	}
	if confirm := r.URL.Query().Get("confirm"); confirm != strconv.Itoa(len(body.MessageIDs)) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "confirm_required",
			"detail": "repeat the number of messages in ?confirm= — an erase " +
				"removes those rows permanently, on every node, and nothing " +
				"undoes it",
		})
		return
	}
	written, err := a.chat.Erase(r.Context(), actor, r.PathValue("channel_id"),
		body.MessageIDs, body.Reason)
	// LOGGED AT INFO WITH THE REASON, because this is one of the two
	// gestures here whose subject no longer exists to be inspected.
	if err == nil {
		log.Info("chat_erased", "channel", r.PathValue("channel_id"),
			"messages", len(body.MessageIDs), "operator", actor.OperatorID,
			"seat", actor.Handle, "reason", body.Reason,
			"outcome", written.Outcome.Outcome)
	}
	a.answerChatWrite(w, r, "erase_messages", written, err,
		map[string]any{"messages": len(body.MessageIDs)})
}

// serveChatPrune answers POST /chat/channels/{channel_id}/prune.
//
// THE CUTOFF IS A QUERY PARAMETER AND IS ECHOED IN `?confirm=`, so the
// instant that decides how much of a room disappears appears twice in one
// request — the same guard the capacity window's byte ceiling takes, and for
// the same reason: this is not a value to inherit from a shell history.
//
// The ordinary producer of a prune is the retention duty, which publishes a
// cutoff as the engine's own [chat.AuthorSystem]. This route is the other
// caller: a person answering an erasure request for one room, now.
func (a *App) serveChatPrune(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.chatModerator(w, r)
	if !ok {
		return
	}
	raw := strings.TrimSpace(r.URL.Query().Get("cutoff"))
	cutoff, err := time.Parse(time.RFC3339, raw)
	if raw == "" || err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "cutoff_required",
			"detail": "name the instant in ?cutoff= as RFC 3339 — a prune " +
				"deletes everything the room said before it, on every node, " +
				"so there is no value to guess",
		})
		return
	}
	if r.URL.Query().Get("confirm") != raw {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "confirm_required",
			"detail": "repeat the cutoff in ?confirm= — everything said in " +
				"this room before it is destroyed on every node, with no inverse",
		})
		return
	}
	written, pruneErr := a.chat.Prune(r.Context(), actor,
		r.PathValue("channel_id"), cutoff)
	if pruneErr == nil {
		log.Info("chat_pruned", "channel", r.PathValue("channel_id"),
			"cutoff", cutoff.UTC().Format(time.RFC3339),
			"operator", actor.OperatorID, "seat", actor.Handle,
			"outcome", written.Outcome.Outcome)
	}
	a.answerChatWrite(w, r, "prune_channel", written, pruneErr,
		map[string]any{"cutoff": cutoff.UTC()})
}

// ---- the read cursor --------------------------------------------------- //

// chatReadFlush is one person's read state moving.
//
// EVERY FIELD IS OPTIONAL AND ABSENT MEANS UNCHANGED, which is what lets two
// tabs flush different facts about one person without either erasing the
// other's. It is [coord.ChatReadDelta]'s own contract, carried through rather
// than reinterpreted here.
type chatReadFlush struct {
	// Cursors are ADVANCED, never set: a value at or below what is stored
	// is ignored, so two tabs racing produce the same record whichever
	// lands first.
	Cursors map[string]int64 `json:"cursors,omitempty"`

	// Muted replaces the whole list when present. A caller clearing it
	// sends an empty array; omitting the key leaves it alone, which is
	// why this is a pointer rather than a slice — a nil slice and an
	// empty one are the same value to encoding/json on the way in, and
	// they mean opposite things here.
	Muted *[]string `json:"muted,omitempty"`

	// DNDUntil replaces when present, INCLUDING with the zero instant,
	// which is how do-not-disturb is cleared.
	DNDUntil *time.Time `json:"dnd_until,omitempty"`
}

// serveChatRead answers POST /chat/read.
//
// # Why a cursor is written through a node at all
//
// Read state lives in the coordination store, and on the default topology that
// store is the engine's own embedded broker, which binds no socket: nothing
// outside this process can reach it. So the flush goes where the state is
// reachable, exactly as the retention acknowledgement and the budget reset do.
//
// # And why this is the one write here that pushes
//
// A cursor is not on the log, so no applier will ever announce it. The only
// reader that wants it is the same person's other tabs — a badge cleared in
// one window has to clear in the other — so this route pushes it to THAT
// PERSON'S sockets and to nobody else's. It is a fact about one reader's
// attention, and there is no other recipient it could have.
func (a *App) serveChatRead(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.chatAuthor(w, r)
	if !ok {
		return
	}
	var body chatReadFlush
	if !readChatBody(w, r, &body) {
		return
	}
	delta := coord.ChatReadDelta{Cursors: body.Cursors, At: a.now()}
	if body.Muted != nil {
		// A NON-NIL EMPTY SLICE IS HOW A LIST IS CLEARED, and nil
		// leaves it alone. The pointer above is what carries that
		// difference across the decode.
		muted := *body.Muted
		if muted == nil {
			muted = []string{}
		}
		delta.Muted = muted
	}
	delta.DNDUntil = body.DNDUntil
	state, err := a.chatCursors.AdvanceChatRead(r.Context(), actor.Handle, delta)
	if err != nil {
		log.Warn("api_chat_read_flush_failed", "seat", actor.Handle, "error", err)
		writeJSON(w, http.StatusInternalServerError,
			map[string]string{"error": "read_state_failed"})
		return
	}
	a.chatLive.PushTo(actor.Handle, stream.KindChatCursorMoved, state)
	writeJSON(w, http.StatusOK, state)
}

// ---- the shared shape of an answer ------------------------------------- //

// chatAuthorFields are the spellings a caller might reach for to say who a
// message is from.
//
// THEY ARE REFUSED, NOT IGNORED, and that is the difference between a caller
// who learns the surface does not work that way and one who believes they have
// just posted as somebody else. Ignoring is the worse failure of the two: the
// message lands under the REAL author with nothing anywhere to say the
// attempt was made, so a script that meant to post on a colleague's behalf
// posts under its own operator's seat and nobody finds out until somebody
// reads the room.
//
// The set is the top level only, which is why `handle` is safe to list: a
// membership body names handles INSIDE its `members` objects, and a top-level
// one there would be a caller trying to say who the write is from.
var chatAuthorFields = []string{
	"author", "author_kind", "as", "as_seat", "handle", "seat",
	"on_behalf_of", "operator", "operator_id", "user",
}

// readChatBody decodes one request body, or answers the refusal itself.
//
// # A missing body is an empty one
//
// Four of these routes carry nothing at all — a join, a leave, a delete — and
// a decoder that insisted on an object would make a caller send `{}` to press
// a button.
//
// # Unknown fields are REFUSED, unlike on the socket's query channel
//
// A query frame is lenient about content on purpose: an unknown field costs
// nothing to ignore, which is what makes a newer client safe against an older
// server. A WRITE is the opposite. Every field here changes what lands in the
// company's own record, so a key this build does not know is either a caller
// saying something it will not get (`colective`, `operationId`) or a caller
// claiming something it may not have (an author) — and both are silent when
// ignored. The first is a broadcast that never happened; the second is
// impersonation that quietly did not occur.
func readChatBody[T any](w http.ResponseWriter, r *http.Request, into *T) bool {
	raw, err := httpjson.ReadBody(w, r, MaxChatBody)
	if err != nil {
		httpjson.Refuse(w, err)
		return false
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return true
	}
	// THE AUTHOR CHECK FIRST, so its refusal is the one a caller trying to
	// name a seat reads. Through a raw map rather than by matching on the
	// decoder's own error text, which is a sentence Go owns and may
	// reword.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]any{"detail": err.Error()})
		return false
	}
	for _, field := range chatAuthorFields {
		if _, named := keys[field]; !named {
			continue
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "author_not_yours",
			"detail": "the body names `" + field + "` — a caller may never name " +
				"the seat a write acts as. The server resolves the presented " +
				"credential to the `kind: human` seat bound to it with " +
				"`contact.crewlet_operator_id`, and that seat is the author; a " +
				"room where the writer chose the name beside the words is not a " +
				"conversation",
		})
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]any{"detail": err.Error()})
		return false
	}
	return true
}

// answerChatWrite renders one write, whole — including the two outcomes a
// 200/500 pair collapses.
//
// # THREE OUTCOMES, THREE STATUSES
//
// A write here is a record on the fleet's log, and the framework answers three
// different facts about it. Rendering them as success-or-failure loses the one
// a caller most needs to act on:
//
//   - APPLIED is 200. The record is durable AND this node has applied it, so
//     the rows a caller is about to read are the rows it produced.
//   - PENDING is 202. The record is DURABLE at its position and what it
//     produced is unresolved HERE; every other node will apply it. That is
//     precisely what 202 means, and a caller told 200 would read the room on
//     this node, not find the message and say it again.
//   - UNKNOWN is 504. Nothing can be established about this record from this
//     node — it may have landed and it may not. 504 is the one status whose
//     meaning is "no timely answer from upstream; the request may have been
//     carried out", and it is deliberately not the 503 this surface already
//     uses for "ask me again in a moment", which promises the opposite.
//
// The op id travels with all three, because RETRYING UNDER THE SAME ONE is the
// only safe retry: a fresh id would defeat the ledger that exists for exactly
// this case, and on a message it would derive a second message id and say the
// same thing twice.
func (a *App) answerChatWrite(w http.ResponseWriter, r *http.Request, what string,
	written chat.Written, err error, extra map[string]any) {

	if err != nil {
		a.refuseChatWrite(w, what, err)
		return
	}
	body := make(map[string]any, len(extra)+6)
	for key, value := range extra {
		body[key] = value
	}
	body["outcome"] = written.Outcome.Outcome
	body["op_id"] = written.ChangeID
	body["position"] = written.Outcome.Position
	body["revision"] = written.Revision
	if written.Channel.ID != "" {
		body["channel"] = written.Channel
	}
	if written.Message.ID != "" {
		// THE MESSAGE'S OWN INSTANT IS ZERO HERE, deliberately: a
		// message is stamped with the BROKER'S time, which is what
		// makes one node's copy of a conversation identical to
		// another's, and it does not exist yet when this answer is
		// formed. A caller that needs it reads the message back.
		body["message"] = written.Message
	}
	status := http.StatusOK
	switch written.Outcome.Outcome {
	case statelog.OutcomePending:
		status = http.StatusAccepted
	case statelog.OutcomeUnknown:
		status = http.StatusGatewayTimeout
		log.WarnContext(r.Context(), "chat_write_outcome_unknown",
			"what", what, "op_id", written.ChangeID,
			"hint", "retry under the same op_id; a fresh one would say it twice")
	}
	writeJSON(w, status, body)
}

// refuseChatWrite maps the write path's own vocabulary onto a status.
//
// EACH SENTINEL IS A DIFFERENT THING FOR THE CALLER TO DO, which is why they
// are not folded into one code: a name somebody else holds is a name to
// negotiate, an archived room is a room to reopen, a refusal is a permission
// to ask for, and a conflict is a read to redo.
//
// THE DETAIL TRAVELS ON THE ONES WRITTEN FOR A PERSON and on no others. The
// value layer's refusals name the field to change and the rule that refused it
// — that is the whole of their value — while an unclassified failure can carry
// a database path or a driver's own message, so its reason reaches the log
// instead.
func (a *App) refuseChatWrite(w http.ResponseWriter, what string, err error) {
	status, code := http.StatusInternalServerError, "write_failed"
	switch {
	case errors.Is(err, chat.ErrInvalid):
		status, code = http.StatusBadRequest, "invalid"
	case errors.Is(err, chat.ErrNotFound):
		// A ROOM THE VIEWER MAY NOT READ ANSWERS THE SAME WAY, and that
		// is the read side's own rule carried here: a private room's
		// EXISTENCE is information, so "not yours" and "no such room"
		// must be one answer.
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, chat.ErrForbidden):
		status, code = http.StatusForbidden, "forbidden"
	case errors.Is(err, chat.ErrArchived):
		status, code = http.StatusConflict, "archived"
	case errors.Is(err, chat.ErrNameTaken):
		status, code = http.StatusConflict, "name_taken"
	case errors.Is(err, chat.ErrRoomExists):
		status, code = http.StatusConflict, "room_exists"
	case errors.Is(err, chat.ErrConflict):
		status, code = http.StatusConflict, "conflict"
	}
	if status == http.StatusInternalServerError {
		log.Warn("api_chat_write_failed", "what", what, "error", err)
		writeJSON(w, status, map[string]string{"error": code})
		return
	}
	writeJSON(w, status, map[string]string{"error": code, "detail": err.Error()})
}

// ChatLive exposes the chat arm of the live channel.
//
// For the engine, which needs it in two places this package cannot reach: as
// the chat applier's [chat.Observer], so a committed record becomes a frame,
// and as the [notify.StatusPoster] behind the working indicator, so a seat
// running a turn against a chat trigger appears in the room it is answering.
//
// Nil on a node that serves no native chat, and every method on a nil one is a
// no-op — so neither caller needs a branch.
func (a *App) ChatLive() *stream.ChatHub { return a.chatLive }
