// Package mattermost is the Mattermost integration: turning a post into a
// notification, and everything a self-hosted chat backend needs that a
// hosted one does not.
package mattermost

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/notify"
)

var log = logging.Get("mattermost")

// Backend is the transport name — the namespace this integration's identities
// and thread follows are stored under, and the value it stamps on the
// `transport` metadata key every chat consumer keys on.
const Backend = "mattermost"

// Grammar is how Mattermost writes a mention: LITERALLY, as `@username` in
// the message text, unlike a backend that rewrites one into markup before
// delivery.
//
// `@all` and `@channel` are synonyms for everyone in the channel; `@here`
// narrows to whoever is online. All three are collective, and none of them
// means this seat personally.
var Grammar = notify.LiteralGrammar{
	Name:        Backend,
	Collectives: []string{"all", "channel", "here"},
}

// Seat is one agent's Mattermost bot, as this node knows it.
type Seat struct {
	Handle string

	// Username is how a human and an MCP tool address this bot: Mattermost
	// addresses a bot by NAME, not by id, so a prompt telling an agent how
	// to mention a colleague needs this.
	Username string

	// UserID is how a PAYLOAD names it. Both are needed and they are not
	// interchangeable: the id recognises this seat's own posts, the name
	// writes a mention that renders.
	//
	// An empty id is not cosmetic — it disables own-message suppression,
	// and an agent that cannot recognise its own posts answers itself, one
	// inbound message per reply, for ever, at one turn each.
	UserID string
}

// Seats resolves a handle to the bot this node has registered for it.
//
// A function rather than a map because a live config apply registers a bot
// this node has never seen, and a parser holding a snapshot would refuse
// every message for it until a restart.
type Seats func(handle string) (Seat, bool)

// Parser turns one fleet-published post into a notification for the seat it
// was delivered to.
//
// ONE recipient, always, and that is the shape of this backend rather than a
// simplification: Mattermost has no usable inbound webhook — outgoing hooks
// fire only in public channels and carry no thread id, channel type or
// mentions — so the engine holds ONE WEBSOCKET PER SEAT and each post
// arrives already addressed to the seat whose socket saw it. A payload with
// three colleagues in it arrives three times, once per socket.
type Parser struct {
	seats   Seats
	threads *notify.ThreadTracker
	now     func() time.Time
}

// NewParser builds the parser.
//
// threads may be nil, which turns thread routing off: every post reaches the
// seat, thread reply or not. That is the pre-follow behaviour and a
// legitimate configuration for a single-agent workspace, where there is no
// second bot for a thread reply to belong to.
func NewParser(seats Seats, threads *notify.ThreadTracker, now func() time.Time) (*Parser, error) {
	if seats == nil {
		return nil, fmt.Errorf("mattermost: the parser needs a seat lookup")
	}
	if threads != nil && threads.Backend() != Backend {
		return nil, fmt.Errorf("mattermost: thread tracker is bound to %q, not %q",
			threads.Backend(), Backend)
	}
	if now == nil {
		now = time.Now
	}
	return &Parser{seats: seats, threads: threads, now: now}, nil
}

// Source implements [notify.Parser].
func (p *Parser) Source() string { return Backend }

// Parse implements [notify.Parser].
//
// A skip is an empty result, never an error: a system post, a bot's own
// echo, a thread the seat does not follow — none of those is a malformed
// delivery, and reporting them as failures would fill the log with the
// ordinary operation of a busy channel.
func (p *Parser) Parse(ctx context.Context, w types.RawWebhook, _ *notify.Registry) ([]notify.Routed, error) {
	handle := w.Handle
	if handle == "" {
		return nil, fmt.Errorf("mattermost: delivery names no seat")
	}
	seat, ok := p.seats(handle)
	if !ok {
		log.WarnContext(ctx, "mattermost_post_for_unregistered_seat", "handle", handle)
		return nil, nil
	}

	post, ok := field[map[string]any](w.Body, "post")
	if !ok || str(post, "id") == "" {
		return nil, nil
	}
	if why := SkipReason(post); why != "" {
		log.DebugContext(ctx, "mattermost_post_skipped", "handle", handle, "reason", why)
		return nil, nil
	}

	var (
		channel     = str(post, "channel_id")
		postID      = str(post, "id")
		root        = str(post, "root_id")
		sender      = str(post, "user_id")
		channelKind = str(w.Body, "channel_type")
		text        = Text(post)
	)

	// OWN-MESSAGE SUPPRESSION, and it records participation on the way
	// past: replying in a thread is how a person follows it, and an
	// agent's own reply should subscribe it to what comes back. That echo
	// is the only signal a node gets that a seat it may not even be
	// running has joined a conversation.
	if seat.UserID != "" && sender == seat.UserID {
		if root != "" && p.threads != nil {
			if err := p.threads.Participated(ctx, handle, channel, root, p.now()); err != nil {
				log.WarnContext(ctx, "mattermost_participation_not_recorded",
					"handle", handle, "thread", root, "error", err.Error())
			}
		}
		return nil, nil
	}
	if text == "" {
		return nil, nil
	}

	msg := notify.ChatMessage{
		Channel: channel, Thread: root, Text: text,
		Targeting: targeting(w.Body, channelKind, seat.UserID),
	}
	reach := notify.Delivery{Deliver: true}
	if p.threads != nil {
		var err error
		reach, err = p.threads.Reaches(ctx, handle, seat.Username, msg, p.now())
		if err != nil {
			log.WarnContext(ctx, "mattermost_thread_follow_unreadable",
				"handle", handle, "thread", root, "error", err.Error())
		}
		if !reach.Deliver {
			return nil, nil
		}
	}

	// A TOP-LEVEL message follows its own id, because that id becomes the
	// thread every reply carries — so a seat named in a channel hears the
	// answers to what it was asked, without being named again.
	if p.threads != nil && root == "" && reach.Reason != "" {
		if err := p.threads.Follow(ctx, handle, channel, postID, p.now()); err != nil {
			log.WarnContext(ctx, "mattermost_follow_not_recorded",
				"handle", handle, "thread", postID, "error", err.Error())
		}
	}

	return []notify.Routed{{
		Inbound: notify.Inbound{
			Source:    Backend,
			EventType: strOr(str(w.Body, "event"), "posted"),
			Sender:    strOr(strings.TrimPrefix(str(w.Body, "sender_name"), "@"), sender),
			Subject:   "Mattermost message",
			Body:      text,
			Metadata:  metadata(w.Body, seat, post, reach, channelKind),
		},
		To: notify.Recipient{Handle: handle},
	}}, nil
}

// metadata is what every downstream consumer reads off a chat notification.
//
// The KEY NAMES are backend-neutral even where the values are not:
// `thread_ts` carries a Mattermost root-post id, because the coalescer, the
// working-status resolver and the sandbox round trip all read that one name
// across every chat backend. Only the value is backend-shaped.
// canonicalKind maps Mattermost's single-letter channel type onto the shape
// every backend describes a surface with.
//
// A PRIVATE CHANNEL IS A GROUP, not a DM: "dm" is what tells a worker the
// message was addressed to this seat alone, and a five-person private
// channel is not that. An unrecognised letter — a type this build does not
// know — reads as unknown rather than being guessed into one of the four.
func canonicalKind(raw string) types.ChannelKind {
	switch raw {
	case "D":
		return types.ChannelDM
	case "G", "P":
		return types.ChannelGroup
	case "O":
		return types.ChannelPublic
	default:
		return types.ChannelUnknown
	}
}

func metadata(body map[string]any, seat Seat, post map[string]any, reach notify.Delivery, channelKind string) map[string]string {
	root := str(post, "root_id")
	m := map[string]string{
		"transport": Backend,
		"channel":   str(post, "channel_id"),
		// Mattermost's single-letter channel type: O(pen), P(rivate),
		// D(irect), G(roup DM). Server-stamped and always present, which
		// is why this backend needs no channel-id-prefix heuristic — and
		// must not have one, since its ids are opaque alphanumerics that
		// would mark arbitrary public channels as direct messages.
		"channel_type": channelKind,
		// The canonical shape beside the raw letter. Both, because the
		// raw one is what a prompt and an operator recognise and the
		// canonical one is what the learning workers read — and the
		// mapping belongs here, in the only code that knows what "G"
		// means (see notify.ChannelKindField).
		notify.ChannelKindField: string(canonicalKind(channelKind)),
		"channel_name":          str(body, "channel_name"),
		"ts":                    str(post, "id"),
		"thread_ts":             root,
		"user":                  str(post, "user_id"),
		"bot_user_id":           seat.UserID,
		// Mattermost addresses a bot by NAME, so a prompt needs both:
		// the id to recognise its own posts, the username to write a
		// mention that renders.
		"bot_username":         seat.Username,
		"actor_external_id":    str(post, "user_id"),
		"thread_follow_reason": string(reach.Reason),
	}
	// thread_anchor is where a reply goes: the thread if there is one,
	// otherwise this post, which becomes the thread the moment anybody
	// answers under it.
	m["thread_anchor"] = strOr(root, str(post, "id"))
	if root != "" && reach.Deliver {
		m["thread_following"] = "true"
	}
	if replayed, _ := body["replayed"].(bool); replayed {
		// A message re-read over REST across a reconnect gap rather than
		// delivered live. The prompt says so, because "this arrived
		// while I was disconnected" changes how stale the seat should
		// assume the conversation is.
		m["replayed"] = strconv.FormatBool(true)
	}
	return m
}

// targeting reads the server's own answer to "was this seat a target?".
//
// A DIRECT MESSAGE short-circuits everything: there is nobody else the
// message could be for.
//
// Otherwise the `mentions` field, which Mattermost rewrites PER CONNECTION —
// it is present only when this connection's user was mentioned, and its
// value is then this seat's own id. So it is exact about WHETHER: it resolves
// `@all` / `@channel` / `@here` against real membership and catches group and
// keyword mentions no pattern could. It can never say WHY, because a
// broadcast and being named personally arrive identically. See
// [notify.Targeting].
//
// An absent field is genuinely no answer, not a denial: a post re-read over
// REST across a reconnect gap carries none, and so does one whose seat
// identity has not resolved yet.
func targeting(body map[string]any, channelKind, ownID string) notify.Targeting {
	if isDirect(channelKind) {
		return notify.TargetingPersonal
	}
	raw, ok := body["mentions"].([]any)
	if !ok || len(raw) == 0 || ownID == "" {
		return notify.TargetingUnknown
	}
	for _, m := range raw {
		if fmt.Sprint(m) == ownID {
			return notify.TargetingIncluded
		}
	}
	// Only reachable when this seat's id is stale: the server named the
	// targets and this seat is not among them.
	return notify.TargetingExcluded
}

// DirectKinds are Mattermost's channel types for a private conversation:
// D(irect) and G(roup DM), against O(pen) and P(rivate) for rooms.
var DirectKinds = []string{"D", "G"}

func isDirect(kind string) bool {
	for _, d := range DirectKinds {
		if kind == d {
			return true
		}
	}
	return false
}

// SkipReason says why a post must NOT wake an agent, or "" for a real
// user-authored message.
//
// Mattermost marks channel bookkeeping — joins, leaves, header and purpose
// changes, renames — with a `system_` post type. Those carry text, but the
// text is ABOUT the channel rather than addressed to anyone, so delivering
// them produces turns triaging "user X joined the channel".
func SkipReason(post map[string]any) string {
	if kind := str(post, "type"); strings.HasPrefix(kind, "system_") {
		return "system post type: " + kind
	}
	if deleted, ok := post["delete_at"]; ok && truthy(deleted) {
		return "deleted post"
	}
	return ""
}

// Text is a post's user-visible content.
//
// A file attached with no comment has an empty message and real content, so
// the attachment count is rendered — a genuine post must never produce a
// blank notification body, which reads to a seat as a message with nothing
// in it rather than as a file it should go and look at.
func Text(post map[string]any) string {
	if msg := str(post, "message"); msg != "" {
		return msg
	}
	files, ok := post["file_ids"].([]any)
	if !ok || len(files) == 0 {
		return ""
	}
	noun := "files"
	if len(files) == 1 {
		noun = "file"
	}
	return fmt.Sprintf("(shared %d %s)", len(files), noun)
}

// field reads a typed value out of a decoded JSON object.
func field[T any](m map[string]any, key string) (T, bool) {
	v, ok := m[key].(T)
	return v, ok
}

// str reads a JSON value as a string, tolerating whatever shape it arrived
// in. A payload field is not a contract, and a stray number must not stop a
// message reaching somebody.
func str(m map[string]any, key string) string {
	switch v := m[key].(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		return fmt.Sprint(v)
	}
}

func strOr(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

// truthy reads a JSON value the way a timestamp-or-zero field is meant.
//
// Mattermost writes `delete_at: 0` for a live post and a millisecond stamp
// for a deleted one, and JSON decodes both as a float — so a bare presence
// check would treat every live post as deleted.
func truthy(v any) bool {
	switch n := v.(type) {
	case nil:
		return false
	case bool:
		return n
	case float64:
		return n != 0
	case int:
		return n != 0
	case string:
		return n != "" && n != "0"
	}
	return false
}
