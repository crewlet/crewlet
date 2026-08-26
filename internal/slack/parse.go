// Package slack is the Slack integration: turning an Events API delivery
// into a notification, and everything a hosted chat backend needs that a
// self-hosted one does not.
//
// # One app per agent, and that is the whole shape
//
// A Slack app has ONE bot user and ONE request URL. An engine running seven
// agents therefore runs seven apps, each with its own credentials, its own
// signing secret and its own `/webhooks/slack/{handle}` path — which is why
// the inbound edge is per-seat where every other vendor's is per-company,
// and why provisioning here is an app-manifest problem rather than an
// account-creation one.
package slack

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/notify"
)

var log = logging.Get("slack")

// Backend is the transport name — the namespace this integration's
// identities and thread follows are stored under, and the value it stamps on
// the `transport` metadata key every chat consumer keys on.
const Backend = "slack"

// Grammar is how Slack writes a mention: as MARKUP, resolved before
// delivery. `@ana` becomes `<@U024BE7LH>` and `@channel` becomes
// `<!channel>`, so matching is exact — Slack has already decided who was
// meant, and there is no ambiguity left for a pattern to guess at.
var Grammar = notify.MarkupGrammar{
	Name: Backend,
	// A user id is U… for a person, W… for an Enterprise Grid user, and
	// B… for a bot user. The submatch is what makes a mention of somebody
	// else impossible to mistake for one of this seat.
	User: regexp.MustCompile(`<@([UWB][A-Z0-9]+)(?:\|[^>]*)?>`),
	// `<!channel>`, `<!here>`, `<!everyone>`, each optionally carrying a
	// display fallback after a pipe.
	Collective: regexp.MustCompile(`<!(?:channel|here|everyone)(?:\|[^>]*)?>`),
}

// DirectKinds are Slack's own words for a conversation with no room around
// it: a one-to-one DM, and a multi-person DM.
//
// A PRIVATE CHANNEL IS NOT ONE. Slack calls it "group", and a five-person
// private channel is a room — treating it as a direct message would tell
// every seat in it that a message was addressed to it alone.
var DirectKinds = []string{"im", "mpim"}

// DMPrefix marks a direct message unambiguously by channel id.
//
// Slack's ids carry their kind in the first letter — C for a channel, G for
// a private one, D for a DM — which is what makes this safe here and unsafe
// on a backend whose ids are opaque. It is the fallback for the one event
// that omits `channel_type`: an app_mention. See [notify.Addressed].
const DMPrefix = "D"

// contentSubtypes are the `message` subtypes that carry NEW user-visible
// content and may wake an agent.
//
// Slack reuses `type: "message"` for channel bookkeeping — edits, deletions,
// thread-reply counters, join and topic lines — and those envelopes carry no
// top-level `user` or `text`. Parsing one as a message wakes an agent into a
// phantom turn with an empty body.
var contentSubtypes = map[string]bool{
	"": true, "thread_broadcast": true, "file_share": true, "bot_message": true,
}

// SkipReason is why a `message`-family event must NOT wake an agent, or "".
//
// Two discriminators, both required:
//
//   - `hidden: true` marks a bookkeeping delivery — message_changed (a human
//     edit AND Slack's own link-unfurl edit of a bot's message),
//     message_deleted, and the message_replied thread counter, which Slack
//     delivers WITHOUT its subtype. For that last one the flag is the only
//     reliable tell.
//   - The subtype allowlist drops the system lines that DO carry text
//     (channel_join, channel_topic) but are about the channel rather than
//     addressed to anybody.
//
// An app_mention carries neither field and always passes.
func SkipReason(event map[string]any) string {
	if hidden, _ := event["hidden"].(bool); hidden {
		return "hidden bookkeeping event (subtype " + orNone(str(event, "subtype")) + ")"
	}
	if subtype := str(event, "subtype"); !contentSubtypes[subtype] {
		return "non-content message subtype: " + subtype
	}
	return ""
}

// Text is the user-visible text of a content-bearing event.
//
// A file shared without a comment has empty `text` and real content, so the
// file names are rendered — a genuine message must never produce a blank
// notification body, which reads to the agent as an empty turn.
func Text(event map[string]any) string {
	if text := str(event, "text"); text != "" {
		return text
	}
	files, _ := event["files"].([]any)
	var names []string
	for _, raw := range files {
		file, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		names = append(names, firstOf(str(file, "name"), str(file, "title"), "unnamed file"))
	}
	if len(names) > 0 {
		return "(shared file: " + strings.Join(names, ", ") + ")"
	}
	return ""
}

// Sender is who sent a content-bearing event.
//
// A human or a bot USER carries `user`; a legacy `bot_message` — an incoming
// webhook, a workflow bot — carries `username` and `bot_id` instead. The
// fallback chain is what keeps the sender from ever being blank.
func Sender(event map[string]any) string {
	return firstOf(str(event, "user"), str(event, "username"), str(event, "bot_id"))
}

// Seat is one agent's Slack app, as this node knows it.
type Seat struct {
	Handle string

	// BotUserID is how a PAYLOAD names this seat: the `<@U…>` a mention
	// resolves to, and the `user` on its own messages.
	//
	// An empty id is not cosmetic — it disables own-message suppression
	// and mention detection at once, so the seat cannot tell its own posts
	// from a colleague's and answers itself for ever, one turn per reply.
	BotUserID string

	// AppID is the app the delivery must have come through, and the second
	// half of own-message suppression: a `bot_message` echo of this seat's
	// own post carries the app id and no user id at all.
	AppID string

	// Channel is where this seat posts when nothing else names a target.
	Channel string
}

// Seats resolves a handle to the app this node has registered for it.
//
// A function rather than a map because a live config apply registers an app
// this node has never seen, and a parser holding a snapshot would refuse
// every delivery for it until a restart.
type Seats func(handle string) (Seat, bool)

// Parser turns one Events API delivery into a notification for the seat it
// was addressed to.
//
// ONE recipient, always: the delivery arrived on that seat's own app's
// request URL, so the URL path already decided who it is for. A message
// naming three colleagues arrives three times, once per app.
type Parser struct {
	seats   Seats
	threads *notify.ThreadTracker
	now     func() time.Time
}

// NewParser builds the parser.
//
// threads may be nil, which turns thread routing off: every message reaches
// the seat, thread reply or not. That is the pre-follow behaviour and a
// legitimate configuration for a single-agent workspace, where there is no
// second bot for a thread reply to belong to.
func NewParser(seats Seats, threads *notify.ThreadTracker, now func() time.Time) (*Parser, error) {
	if seats == nil {
		return nil, fmt.Errorf("slack: the parser needs a seat lookup")
	}
	if threads != nil && threads.Backend() != Backend {
		return nil, fmt.Errorf("slack: thread tracker is bound to %q, not %q",
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
// A skip is an empty result, never an error: a bookkeeping event, a bot's
// own echo, a thread the seat does not follow — none of those is a malformed
// delivery, and reporting them as failures would fill the log with the
// ordinary operation of a busy workspace.
func (p *Parser) Parse(ctx context.Context, w types.RawWebhook, _ *notify.Registry) ([]notify.Routed, error) {
	handle := w.Handle
	if handle == "" {
		return nil, fmt.Errorf("slack: delivery names no seat")
	}
	seat, ok := p.seats(handle)
	if !ok {
		log.Warn("slack_event_for_unregistered_seat", "handle", handle)
		return nil, nil
	}
	// The url_verification handshake is answered at the edge, without a
	// signature, before anything is published — see the route. Anything
	// else that is not an event callback is Slack telling the workspace
	// something rather than telling this seat something.
	if kind := str(w.Body, "type"); kind != "event_callback" {
		log.Debug("slack_envelope_ignored", "handle", handle, "type", kind)
		return nil, nil
	}
	event, _ := w.Body["event"].(map[string]any)
	eventType := str(event, "type")
	if eventType != "message" && eventType != "app_mention" {
		log.Debug("slack_event_type_ignored", "handle", handle, "event_type", eventType)
		return nil, nil
	}
	if why := SkipReason(event); why != "" {
		log.Debug("slack_event_skipped", "handle", handle, "reason", why)
		return nil, nil
	}

	var (
		channel = str(event, "channel")
		ts      = str(event, "ts")
		thread  = str(event, "thread_ts")
		text    = Text(event)
	)
	// SLACK REPEATS thread_ts ON A TOP-LEVEL MESSAGE that has replies, and
	// the follow model turns on that emptiness: a message whose own id is
	// its thread id is not a reply, and reporting it as one would deliver
	// every reply in every thread the seat has ever been in.
	if thread == ts {
		thread = ""
	}

	// OWN-MESSAGE SUPPRESSION, and it records participation on the way
	// past: replying in a thread is how a seat follows it, and its own
	// reply should subscribe it to what comes back.
	if p.isOwn(seat, w.Body, event) {
		if thread != "" && p.threads != nil {
			if err := p.threads.Participated(ctx, handle, channel, thread, p.now()); err != nil {
				log.Warn("slack_participation_not_recorded",
					"handle", handle, "thread", thread, "error", err.Error())
			}
		}
		log.Debug("slack_own_message_skipped", "handle", handle)
		return nil, nil
	}
	if text == "" {
		return nil, nil
	}

	kind := channelKind(event, channel)
	msg := notify.ChatMessage{
		Channel: channel, Thread: thread, Text: text,
		Targeting: targeting(kind, eventType),
	}
	reach := notify.Delivery{Deliver: true}
	if p.threads != nil {
		var err error
		reach, err = p.threads.Reaches(ctx, handle, seat.BotUserID, msg, p.now())
		if err != nil {
			log.Warn("slack_thread_follow_unreadable",
				"handle", handle, "thread", thread, "error", err.Error())
		}
		if !reach.Deliver {
			return nil, nil
		}
	}

	// A TOP-LEVEL message follows its OWN id, because that id becomes the
	// thread every reply carries — so a seat named in a channel hears the
	// answers to what it was asked, without being named again.
	if p.threads != nil && thread == "" && reach.Reason != "" {
		if err := p.threads.Follow(ctx, handle, channel, ts, p.now()); err != nil {
			log.Warn("slack_follow_not_recorded",
				"handle", handle, "thread", ts, "error", err.Error())
		}
	}

	return []notify.Routed{{
		Inbound: notify.Inbound{
			Source:    Backend,
			EventType: eventType,
			Sender:    Sender(event),
			Subject:   "Slack message",
			Body:      text,
			Metadata:  metadata(w.Body, event, seat, msg, kind, ts, thread, reach),
		},
		To: notify.Recipient{Handle: handle},
	}}, nil
}

// isOwn reports a delivery that is this seat's own message coming back.
//
// TWO TESTS, because Slack echoes a bot's post in two shapes. An ordinary
// post carries `user` equal to the bot user id; a post made through an
// incoming webhook or with a custom username arrives as a `bot_message` with
// no `user` at all, and only the app id identifies it. Missing either one
// makes the seat answer itself, one turn per reply, for ever.
func (p *Parser) isOwn(seat Seat, body, event map[string]any) bool {
	if seat.BotUserID != "" && str(event, "user") == seat.BotUserID {
		return true
	}
	appID := str(event, "app_id")
	if appID == "" {
		return false
	}
	// The seat's own app id where the engine knows it, and the delivery's
	// own `api_app_id` otherwise — the envelope names the app the request
	// URL belongs to, which IS this seat's.
	return appID == seat.AppID || appID == str(body, "api_app_id")
}

// botUserID is the account this delivery's app authenticates as, as Slack
// states it on the envelope.
//
// Read from the delivery rather than from config, because it is the one
// place the id appears without a round trip — and it is what a mention of
// this seat resolves to, so a seat whose config predates its install still
// recognises being named.
func botUserID(body map[string]any) string {
	auths, _ := body["authorizations"].([]any)
	if len(auths) == 0 {
		return ""
	}
	first, _ := auths[0].(map[string]any)
	return str(first, "user_id")
}

// channelKind is Slack's own word for the conversation: "channel", "group",
// "im" or "mpim".
//
// An app_mention omits the field, so the channel id's first letter answers
// instead — Slack's ids carry their kind, and D is always a DM. Without that
// fallback the message/app_mention double delivery would key one DM two
// different ways depending on which won the dedupe race, splitting a burst
// into two coalescing partitions.
func channelKind(event map[string]any, channel string) string {
	if kind := str(event, "channel_type"); kind != "" {
		return kind
	}
	if strings.HasPrefix(channel, DMPrefix) {
		return "im"
	}
	return ""
}

// targeting is what SLACK knows about this message that its text cannot
// show.
//
// A direct message is personal whatever the text says: there is nobody else
// it could be for. An app_mention is Slack's own resolution of a mention of
// this bot — including through a user group, which no pattern over the text
// could see — so it is INCLUDED rather than personal, and the grammar
// decides why. Everything else is unknown and the text decides alone.
func targeting(kind, eventType string) notify.Targeting {
	for _, direct := range DirectKinds {
		if kind == direct {
			return notify.TargetingPersonal
		}
	}
	if eventType == "app_mention" {
		return notify.TargetingIncluded
	}
	return notify.TargetingUnknown
}

// metadata is what every downstream consumer reads off a chat notification.
//
// The KEY NAMES are backend-neutral even where the values are not: the
// coalescer, the working-status resolver and the sandbox round trip all read
// `thread_ts` across every chat backend. Only the value is backend-shaped —
// here a Slack message timestamp, which is what Slack uses as a thread id.
func metadata(body, event map[string]any, seat Seat, msg notify.ChatMessage,
	kind, ts, thread string, reach notify.Delivery,
) map[string]string {
	m := map[string]string{
		"transport":    Backend,
		"channel":      msg.Channel,
		"channel_type": kind,
		// The canonical shape beside the vendor's own word. Both,
		// because the raw one is what a prompt and an operator
		// recognise and the canonical one is what the learning workers
		// read — and the mapping belongs here, in the only code that
		// knows what "mpim" means.
		notify.ChannelKindField: string(canonicalKind(kind)),
		"ts":                    ts,
		"thread_ts":             thread,
		"team":                  str(body, "team_id"),
		// The RAW user id, never a display name: the learning subsystem
		// resolves counterparty identity from this key.
		"user":                 str(event, "user"),
		"bot_user_id":          firstOf(botUserID(body), seat.BotUserID),
		"app_id":               firstOf(str(body, "api_app_id"), seat.AppID),
		notify.ActorField:      str(event, "user"),
		"thread_follow_reason": string(reach.Reason),
	}
	// thread_anchor is where a reply goes: the thread if there is one,
	// otherwise this message, which becomes the thread the moment anybody
	// answers under it.
	m["thread_anchor"] = firstOf(thread, ts)
	if thread != "" && reach.Deliver {
		m["thread_following"] = "true"
	}
	return m
}

// canonicalKind maps Slack's conversation word onto the shape every backend
// describes a surface with.
//
// A GROUP — Slack's word for a private channel — is a group, not a DM. "dm"
// is what tells a worker the message was addressed to this seat alone, and a
// private channel is a room. An unrecognised word reads as unknown rather
// than being guessed into one of the four.
func canonicalKind(kind string) types.ChannelKind {
	switch kind {
	case "im":
		return types.ChannelDM
	case "mpim", "group":
		return types.ChannelGroup
	case "channel":
		return types.ChannelPublic
	default:
		return types.ChannelUnknown
	}
}

func str(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
