package slack_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/slack"
)

// The workspace: one agent seat with its own app, and the people around it.
const (
	botUser   = "U0BOTSWE"
	botApp    = "A0APPSWE"
	human     = "U0ANA"
	colleague = "U0BOTQA"
)

var pinned = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// follows is a durable follow store, in memory.
type follows struct {
	mu   sync.Mutex
	held map[string]string
	err  error
}

func newFollows() *follows { return &follows{held: map[string]string{}} }

func (f *follows) key(backend, handle, channel, thread string) string {
	return backend + "|" + handle + "|" + channel + "|" + thread
}

func (f *follows) Follow(_ context.Context, backend, handle, channel, thread, reason string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.held[f.key(backend, handle, channel, thread)] = reason
	return nil
}

func (f *follows) Following(_ context.Context, backend, handle, channel, thread string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", false, f.err
	}
	reason, ok := f.held[f.key(backend, handle, channel, thread)]
	return reason, ok, nil
}

func (f *follows) Unfollow(_ context.Context, backend, handle, channel, thread string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := f.key(backend, handle, channel, thread)
	_, ok := f.held[key]
	delete(f.held, key)
	return ok, nil
}

func (f *follows) reason(handle, channel, thread string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held[f.key(slack.Backend, handle, channel, thread)]
}

func seats(handle string) (slack.Seat, bool) {
	if handle != "swe" {
		return slack.Seat{}, false
	}
	return slack.Seat{Handle: "swe", BotUserID: botUser, AppID: botApp, Channel: "C0DEFAULT"}, true
}

func parser(t *testing.T, store notify.FollowStore) *slack.Parser {
	t.Helper()
	var threads *notify.ThreadTracker
	if store != nil {
		var err error
		threads, err = notify.NewThreadTracker(slack.Grammar, store)
		if err != nil {
			t.Fatal(err)
		}
	}
	p, err := slack.NewParser(seats, threads, func() time.Time { return pinned })
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// event builds one Events API delivery addressed to the swe seat.
func event(kind string, fields map[string]any) types.RawWebhook {
	ev := map[string]any{
		"type": kind, "user": human, "channel": "C0ENG",
		"channel_type": "channel", "ts": "1700000001.000100",
		"text": "how is the redirect fix going",
	}
	for k, v := range fields {
		ev[k] = v
	}
	return types.RawWebhook{
		Handle: "swe",
		Body: map[string]any{
			"type": "event_callback", "team_id": "T0ACME",
			"api_app_id": botApp, "event_id": "Ev1",
			"authorizations": []any{map[string]any{"user_id": botUser}},
			"event":          ev,
		},
	}
}

func route(t *testing.T, p *slack.Parser, w types.RawWebhook) []notify.Routed {
	t.Helper()
	out, err := p.Parse(context.Background(), w, nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return out
}

// A TOP-LEVEL MESSAGE REACHES THE SEAT: the bot is in the channel.
func TestATopLevelMessageReachesTheSeat(t *testing.T) {
	t.Parallel()
	got := route(t, parser(t, newFollows()), event("message", nil))
	if len(got) != 1 {
		t.Fatalf("want one notification, got %d", len(got))
	}
	if got[0].To.Handle != "swe" {
		t.Errorf("addressed to %+v", got[0].To)
	}
	if got[0].Metadata["transport"] != slack.Backend {
		t.Errorf("transport = %q", got[0].Metadata["transport"])
	}
}

// A THREAD REPLY IN A THREAD NOBODY FOLLOWS IS NOT DELIVERED.
//
// Without this a seat wakes for every reply in every thread of every channel
// its bot sits in — a burst of turns nobody asked for and cannot be taken
// back.
func TestAThreadReplyIsSilentUntilTheSeatFollows(t *testing.T) {
	t.Parallel()
	store := newFollows()
	p := parser(t, store)
	reply := event("message", map[string]any{
		"thread_ts": "1700000000.000000", "ts": "1700000002.000200",
	})
	if got := route(t, p, reply); len(got) != 0 {
		t.Fatalf("an unfollowed thread reply woke the seat: %+v", got)
	}

	// A mention in the same thread establishes the follow AND is
	// delivered, and the reply after it rides that follow.
	mention := event("message", map[string]any{
		"thread_ts": "1700000000.000000", "ts": "1700000003.000300",
		"text": "<@" + botUser + "> can you look",
	})
	if got := route(t, p, mention); len(got) != 1 {
		t.Fatalf("a mention in a thread did not reach the seat: %+v", got)
	}
	if store.reason("swe", "C0ENG", "1700000000.000000") != string(notify.FollowMention) {
		t.Errorf("the follow was not recorded as a mention")
	}
	if got := route(t, p, reply); len(got) != 1 {
		t.Fatalf("a reply in a followed thread was dropped: %+v", got)
	}
}

// A MENTION IS MARKUP, NOT A NAME. Slack resolves `@swe` to `<@U…>` before
// delivery, so a parser looking for the literal handle would find nothing
// and the seat would never be woken by being named.
func TestOnlyTheResolvedMarkupCountsAsAMention(t *testing.T) {
	t.Parallel()
	store := newFollows()
	p := parser(t, store)
	// The handle written out is NOT a mention on this backend.
	literal := event("message", map[string]any{
		"thread_ts": "1700000000.000000", "text": "@swe are you there",
	})
	if got := route(t, p, literal); len(got) != 0 {
		t.Fatalf("literal text was read as a mention: %+v", got)
	}
	// And a mention of somebody ELSE is not a mention of this seat.
	other := event("message", map[string]any{
		"thread_ts": "1700000000.000000", "text": "<@" + colleague + "> can you look",
	})
	if got := route(t, p, other); len(got) != 0 {
		t.Fatalf("a colleague's mention woke this seat: %+v", got)
	}
}

// A DIRECT MESSAGE IS ADDRESSED TO THIS SEAT WHATEVER THE TEXT SAYS: there
// is nobody else it could be for.
func TestADirectMessageAlwaysReachesTheSeat(t *testing.T) {
	t.Parallel()
	store := newFollows()
	dm := event("message", map[string]any{
		"channel": "D0ANA", "channel_type": "im",
		"thread_ts": "1700000000.000000", "text": "hello",
	})
	got := route(t, parser(t, store), dm)
	if len(got) != 1 {
		t.Fatalf("a direct message did not reach the seat: %+v", got)
	}
	if got[0].Metadata[notify.ChannelKindField] != string(types.ChannelDM) {
		t.Errorf("channel kind = %q", got[0].Metadata[notify.ChannelKindField])
	}
}

// A PRIVATE CHANNEL IS A ROOM, NOT A DM. "dm" is what tells a worker the
// message was addressed to this seat alone, and a five-person private
// channel is not that.
func TestAPrivateChannelIsAGroupNotADirectMessage(t *testing.T) {
	t.Parallel()
	got := route(t, parser(t, newFollows()),
		event("message", map[string]any{"channel": "G0ENG", "channel_type": "group"}))
	if len(got) != 1 {
		t.Fatalf("want one notification, got %d", len(got))
	}
	if kind := got[0].Metadata[notify.ChannelKindField]; kind != string(types.ChannelGroup) {
		t.Errorf("a private channel reported kind %q", kind)
	}
}

// AN APP_MENTION OMITS channel_type, and the channel id's first letter has
// to answer instead — otherwise the message/app_mention double delivery
// keys one DM two different ways depending on which won the dedupe race.
func TestAnAppMentionInADMStillReadsAsADirectMessage(t *testing.T) {
	t.Parallel()
	w := event("app_mention", map[string]any{
		"channel": "D0ANA", "text": "<@" + botUser + "> hi",
	})
	delete(w.Body["event"].(map[string]any), "channel_type")

	got := route(t, parser(t, newFollows()), w)
	if len(got) != 1 {
		t.Fatalf("want one notification, got %d", len(got))
	}
	if got[0].Metadata["channel_type"] != "im" {
		t.Errorf("channel_type = %q", got[0].Metadata["channel_type"])
	}
}

// THE SEAT'S OWN MESSAGE MUST NOT WAKE IT, or it answers itself, one turn
// per reply, for ever.
func TestASeatDoesNotWakeOnItsOwnMessage(t *testing.T) {
	t.Parallel()
	store := newFollows()
	own := event("message", map[string]any{
		"user": botUser, "app_id": botApp,
		"thread_ts": "1700000000.000000", "text": "on it",
	})
	if got := route(t, parser(t, store), own); len(got) != 0 {
		t.Fatalf("a seat woke on its own message: %+v", got)
	}
	// And its own reply SUBSCRIBES it to the thread, which is what every
	// chat client does when a person replies.
	if store.reason("swe", "C0ENG", "1700000000.000000") != string(notify.FollowParticipated) {
		t.Error("replying did not subscribe the seat to the thread")
	}
}

// A LEGACY BOT ECHO CARRIES NO USER ID AT ALL, only the app's. Missing that
// second test is the same infinite loop by another route.
func TestABotMessageEchoIsAlsoSuppressed(t *testing.T) {
	t.Parallel()
	w := event("message", map[string]any{
		"subtype": "bot_message", "username": "swe", "app_id": botApp,
		"bot_id": "B0SWE", "text": "posted through a webhook",
	})
	delete(w.Body["event"].(map[string]any), "user")
	if got := route(t, parser(t, newFollows()), w); len(got) != 0 {
		t.Fatalf("a bot_message echo woke the seat: %+v", got)
	}
}

// BOOKKEEPING EVENTS CARRY NO MESSAGE. Delivering one wakes the agent into a
// phantom turn with an empty body.
func TestBookkeepingEventsWakeNobody(t *testing.T) {
	t.Parallel()
	p := parser(t, newFollows())
	for name, fields := range map[string]map[string]any{
		"an edit":                {"subtype": "message_changed", "hidden": true},
		"a deletion":             {"subtype": "message_deleted", "hidden": true},
		"the reply counter":      {"hidden": true},
		"somebody joining":       {"subtype": "channel_join", "text": "<@U0ANA> has joined"},
		"a topic change":         {"subtype": "channel_topic", "text": "set the topic"},
		"a message with no text": {"text": ""},
	} {
		if got := route(t, p, event("message", fields)); len(got) != 0 {
			t.Errorf("%s produced %d notification(s)", name, len(got))
		}
	}
}

// A SHARED FILE WITH NO COMMENT IS A REAL MESSAGE. Its text is empty and its
// content is not, and a blank body reads to the agent as an empty turn.
func TestASharedFileRendersItsNames(t *testing.T) {
	t.Parallel()
	w := event("message", map[string]any{
		"subtype": "file_share", "text": "",
		"files": []any{
			map[string]any{"name": "trace.log"},
			map[string]any{"title": "screenshot"},
		},
	})
	got := route(t, parser(t, newFollows()), w)
	if len(got) != 1 {
		t.Fatalf("a shared file woke nobody: %+v", got)
	}
	if got[0].Body != "(shared file: trace.log, screenshot)" {
		t.Errorf("body = %q", got[0].Body)
	}
}

// AN ENVELOPE THAT IS NOT AN EVENT CALLBACK IS SLACK TALKING TO THE
// WORKSPACE, not to this seat.
func TestOnlyEventCallbacksRoute(t *testing.T) {
	t.Parallel()
	p := parser(t, newFollows())
	for _, kind := range []string{"url_verification", "app_rate_limited", ""} {
		w := event("message", nil)
		w.Body["type"] = kind
		if got := route(t, p, w); len(got) != 0 {
			t.Errorf("envelope %q produced %d notification(s)", kind, len(got))
		}
	}
	// And an event type this build does not route.
	w := event("reaction_added", nil)
	if got := route(t, p, w); len(got) != 0 {
		t.Errorf("reaction_added produced %d notification(s)", len(got))
	}
}

// A DELIVERY NAMING NO SEAT IS A BUG IN THE EDGE, not an ordinary skip — the
// URL path carries the handle, so an empty one means the route changed.
func TestADeliveryWithNoHandleIsAnError(t *testing.T) {
	t.Parallel()
	w := event("message", nil)
	w.Handle = ""
	if _, err := parser(t, newFollows()).Parse(context.Background(), w, nil); err == nil {
		t.Fatal("a delivery naming no seat parsed cleanly")
	}
	// A delivery for a seat this node does not run is ordinary, though:
	// an apply may have removed it while a message was in flight.
	w.Handle = "nobody"
	if got := route(t, parser(t, newFollows()), w); len(got) != 0 {
		t.Errorf("a delivery for an unknown seat produced %d notification(s)", len(got))
	}
}

// SLACK REPEATS thread_ts ON A TOP-LEVEL MESSAGE that has replies, and the
// follow model turns on that emptiness — a message reported as its own
// thread reply would make the seat deaf to every top-level message in a
// channel it is not already following.
func TestATopLevelMessageIsNotItsOwnThreadReply(t *testing.T) {
	t.Parallel()
	w := event("message", map[string]any{
		"ts": "1700000005.000500", "thread_ts": "1700000005.000500",
	})
	got := route(t, parser(t, newFollows()), w)
	if len(got) != 1 {
		t.Fatalf("a top-level message with replies was dropped: %+v", got)
	}
	if got[0].Metadata["thread_ts"] != "" {
		t.Errorf("thread_ts = %q, want empty on a top-level message",
			got[0].Metadata["thread_ts"])
	}
}

// A MENTION IN A CHANNEL FOLLOWS THE MESSAGE'S OWN ID, because that id
// becomes the thread every reply carries — so a seat named in a channel
// hears the answers without being named again.
func TestBeingNamedInAChannelFollowsTheThreadItStarts(t *testing.T) {
	t.Parallel()
	store := newFollows()
	w := event("message", map[string]any{
		"ts": "1700000009.000900", "text": "<@" + botUser + "> please look",
	})
	if got := route(t, parser(t, store), w); len(got) != 1 {
		t.Fatalf("a mention did not reach the seat: %+v", got)
	}
	if store.reason("swe", "C0ENG", "1700000009.000900") == "" {
		t.Error("the message's own id was not followed")
	}
}

// AN UNREADABLE FOLLOW STORE FAILS CLOSED on a thread reply. A missed reply
// is quiet and self-healing; delivering instead wakes the seat for every
// reply in every thread it has ever been near.
func TestAnUnreadableFollowStoreDropsThreadReplies(t *testing.T) {
	t.Parallel()
	store := newFollows()
	store.err = errors.New("the store is unreachable")
	reply := event("message", map[string]any{"thread_ts": "1700000000.000000"})
	if got := route(t, parser(t, store), reply); len(got) != 0 {
		t.Fatalf("an unreadable store delivered a thread reply: %+v", got)
	}
	// A top-level message is unaffected: the bot is in the channel and no
	// follow is consulted.
	if got := route(t, parser(t, store), event("message", nil)); len(got) != 1 {
		t.Errorf("an unreadable store dropped a top-level message")
	}
}

// WITH NO FOLLOW STORE, THREAD ROUTING IS OFF and every message reaches the
// seat — the pre-follow behaviour, and legitimate for a single-agent
// workspace.
func TestWithoutAFollowStoreEveryMessageReaches(t *testing.T) {
	t.Parallel()
	reply := event("message", map[string]any{"thread_ts": "1700000000.000000"})
	if got := route(t, parser(t, nil), reply); len(got) != 1 {
		t.Fatalf("thread routing was applied with no store: %+v", got)
	}
}

// THE METADATA IS WHAT EVERY DOWNSTREAM CONSUMER READS.
func TestTheMetadataCarriesWhatTheSpineNeeds(t *testing.T) {
	t.Parallel()
	got := route(t, parser(t, newFollows()), event("message", nil))
	meta := got[0].Metadata
	for key, want := range map[string]string{
		"transport":       slack.Backend,
		"channel":         "C0ENG",
		"channel_type":    "channel",
		"ts":              "1700000001.000100",
		"thread_ts":       "",
		"team":            "T0ACME",
		"user":            human,
		"bot_user_id":     botUser,
		"app_id":          botApp,
		notify.ActorField: human,
		"thread_anchor":   "1700000001.000100",
	} {
		if meta[key] != want {
			t.Errorf("%s = %q, want %q", key, meta[key], want)
		}
	}
}
