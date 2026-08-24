package mattermost_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/notify"
)

var t0 = time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

type follows struct {
	rows     map[string]string
	failRead error
}

func newFollows() *follows { return &follows{rows: map[string]string{}} }

func key(b, h, c, th string) string { return b + "|" + h + "|" + c + "|" + th }

func (f *follows) Follow(_ context.Context, b, h, c, th, reason string, _ time.Time) error {
	f.rows[key(b, h, c, th)] = reason
	return nil
}

func (f *follows) Following(_ context.Context, b, h, c, th string) (string, bool, error) {
	if f.failRead != nil {
		return "", false, f.failRead
	}
	r, ok := f.rows[key(b, h, c, th)]
	return r, ok, nil
}

func (f *follows) Unfollow(_ context.Context, b, h, c, th string) (bool, error) {
	k := key(b, h, c, th)
	_, ok := f.rows[k]
	delete(f.rows, k)
	return ok, nil
}

var seat = mattermost.Seat{Handle: "swe", Username: "agent-swe", UserID: "bot-1"}

func seats(s ...mattermost.Seat) mattermost.Seats {
	by := map[string]mattermost.Seat{}
	for _, one := range s {
		by[one.Handle] = one
	}
	return func(handle string) (mattermost.Seat, bool) {
		got, ok := by[handle]
		return got, ok
	}
}

func parser(t *testing.T, store notify.FollowStore) (*mattermost.Parser, *follows) {
	t.Helper()
	f, _ := store.(*follows)
	var tracker *notify.ThreadTracker
	if store != nil {
		var err error
		tracker, err = notify.NewThreadTracker(mattermost.Grammar, store)
		if err != nil {
			t.Fatalf("NewThreadTracker: %v", err)
		}
	}
	p, err := mattermost.NewParser(seats(seat), tracker, func() time.Time { return t0 })
	if err != nil {
		t.Fatalf("NewParser: %v", err)
	}
	return p, f
}

// post builds a delivery the websocket fleet would publish.
func post(mutate func(body, post map[string]any)) types.RawWebhook {
	p := map[string]any{
		"id": "p1", "channel_id": "C1", "user_id": "u-ana",
		"message": "hello there", "delete_at": float64(0),
	}
	body := map[string]any{
		"event": "posted", "post": p, "channel_type": "O",
		"channel_name": "eng", "sender_name": "@ana",
	}
	if mutate != nil {
		mutate(body, p)
	}
	return types.RawWebhook{Body: body, Handle: "swe"}
}

func parseOne(t *testing.T, p *mattermost.Parser, w types.RawWebhook) (notify.Routed, bool) {
	t.Helper()
	got, err := p.Parse(t.Context(), w, nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) == 0 {
		return notify.Routed{}, false
	}
	if len(got) != 1 {
		t.Fatalf("one socket produced %d notifications", len(got))
	}
	return got[0], true
}

func TestAChannelMessageReachesTheSeatWhoseSocketSawIt(t *testing.T) {
	p, _ := parser(t, newFollows())

	got, ok := parseOne(t, p, post(nil))
	if !ok {
		t.Fatal("a plain channel message did not reach the seat")
	}
	if got.To.Handle != "swe" {
		t.Fatalf("routed to %q", got.To.Handle)
	}
	if got.Source != mattermost.Backend || got.Body != "hello there" {
		t.Fatalf("parsed %+v", got.Inbound)
	}
	if got.Sender != "ana" {
		t.Fatalf("sender = %q, want the display name with no leading @", got.Sender)
	}
	m := got.Metadata
	for k, want := range map[string]string{
		"transport": "mattermost", "channel": "C1", "channel_type": "O",
		"channel_name": "eng", "ts": "p1", "thread_ts": "",
		"user": "u-ana", "bot_user_id": "bot-1", "bot_username": "agent-swe",
		"thread_anchor": "p1", "actor_external_id": "u-ana",
	} {
		if m[k] != want {
			t.Errorf("metadata[%q] = %q, want %q", k, m[k], want)
		}
	}
	// The self-action guard reads ONE key across every integration, and
	// this backend must stamp it or the guard silently does not exist here.
	if m[notify.ActorField] == "" {
		t.Fatal("the post carries no actor for the self-action guard")
	}
}

// Mattermost has no usable inbound webhook, so the engine holds one socket
// per seat and each post arrives already addressed. A payload concerning
// three colleagues arrives three times.
func TestOneSocketProducesOneRecipient(t *testing.T) {
	p, _ := parser(t, newFollows())
	got, err := p.Parse(t.Context(), post(func(b, pp map[string]any) {
		pp["message"] = "@agent-swe and @agent-qa please look"
	}), nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 1 || got[0].To.Handle != "swe" {
		t.Fatalf("a message naming two seats produced %d notifications", len(got))
	}
}

// Channel bookkeeping carries text, but the text is ABOUT the channel rather
// than addressed to anyone — delivering it produces turns triaging "user X
// joined the channel".
func TestSystemAndDeletedPostsAreSkipped(t *testing.T) {
	p, _ := parser(t, newFollows())
	for _, mutate := range []func(b, pp map[string]any){
		func(_, pp map[string]any) { pp["type"] = "system_join_channel" },
		func(_, pp map[string]any) { pp["type"] = "system_header_change" },
		func(_, pp map[string]any) { pp["delete_at"] = float64(1718003000) },
	} {
		if _, ok := parseOne(t, p, post(mutate)); ok {
			t.Error("a bookkeeping post woke the seat")
		}
	}
	// A LIVE post writes delete_at: 0, so a bare presence check would
	// treat every real message as deleted.
	if _, ok := parseOne(t, p, post(nil)); !ok {
		t.Fatal("delete_at: 0 was read as deleted")
	}
	// A regular typed post has no `type` at all, and one that does must
	// not be confused with a system post by prefix alone.
	if _, ok := parseOne(t, p, post(func(_, pp map[string]any) { pp["type"] = "custom_poll" })); !ok {
		t.Fatal("a custom post type was skipped as bookkeeping")
	}
}

// An agent that cannot recognise its own posts answers itself: one inbound
// message per reply, for ever, at one turn each.
func TestAnAgentDoesNotHearItsOwnPost(t *testing.T) {
	p, store := parser(t, newFollows())

	if _, ok := parseOne(t, p, post(func(_, pp map[string]any) {
		pp["user_id"] = "bot-1"
	})); ok {
		t.Fatal("the agent heard its own post")
	}

	// And its own THREAD reply subscribes it to what comes back —
	// replying is how a person follows a thread, and that echo is the
	// only signal a node gets that a seat joined a conversation.
	if _, ok := parseOne(t, p, post(func(_, pp map[string]any) {
		pp["user_id"], pp["root_id"] = "bot-1", "root-1"
	})); ok {
		t.Fatal("the agent heard its own thread reply")
	}
	if got := store.rows[key("mattermost", "swe", "C1", "root-1")]; got != string(notify.FollowParticipated) {
		t.Fatalf("its own reply recorded %q", got)
	}
}

// A file attached with no comment has real content: a blank body reads to a
// seat as a message with nothing in it rather than a file to go and look at.
func TestAFileShareWithNoCommentStillSaysSomething(t *testing.T) {
	p, _ := parser(t, newFollows())

	got, ok := parseOne(t, p, post(func(_, pp map[string]any) {
		pp["message"] = ""
		pp["file_ids"] = []any{"f1"}
	}))
	if !ok || got.Body != "(shared 1 file)" {
		t.Fatalf("a single file share rendered %q", got.Body)
	}
	got, _ = parseOne(t, p, post(func(_, pp map[string]any) {
		pp["message"] = ""
		pp["file_ids"] = []any{"f1", "f2"}
	}))
	if got.Body != "(shared 2 files)" {
		t.Fatalf("a two-file share rendered %q", got.Body)
	}
	// A post with neither text nor files is nothing at all.
	if _, ok := parseOne(t, p, post(func(_, pp map[string]any) { pp["message"] = "" })); ok {
		t.Fatal("an empty post woke the seat")
	}
}

// A thread reply reaches the seat only if it follows the thread.
func TestAThreadReplyNeedsAFollow(t *testing.T) {
	p, store := parser(t, newFollows())
	reply := func(text string) types.RawWebhook {
		return post(func(_, pp map[string]any) {
			pp["root_id"], pp["message"], pp["id"] = "root-1", text, "p2"
		})
	}

	if _, ok := parseOne(t, p, reply("any thoughts?")); ok {
		t.Fatal("an unfollowed thread reply woke the seat")
	}
	got, ok := parseOne(t, p, reply("@agent-swe any thoughts?"))
	if !ok {
		t.Fatal("a mention in a thread did not wake the seat")
	}
	if got.Metadata["thread_follow_reason"] != string(notify.FollowMention) {
		t.Fatalf("the follow reason is %q", got.Metadata["thread_follow_reason"])
	}
	if got.Metadata["thread_following"] != "true" {
		t.Fatal("a followed thread reply does not say so")
	}
	if got.Metadata["thread_ts"] != "root-1" || got.Metadata["thread_anchor"] != "root-1" {
		t.Fatalf("a reply anchors on %q/%q", got.Metadata["thread_ts"], got.Metadata["thread_anchor"])
	}
	if store.rows[key("mattermost", "swe", "C1", "root-1")] == "" {
		t.Fatal("the mention did not record a follow")
	}
	// Every later reply now reaches it, named or not.
	if _, ok := parseOne(t, p, reply("still there?")); !ok {
		t.Fatal("a followed thread's later reply was dropped")
	}
}

// A seat named in a CHANNEL follows the post it was named in, because that
// post's id becomes the thread every answer carries — so it hears the
// answers to what it was asked without being named again.
func TestBeingNamedAtTopLevelFollowsTheAnswers(t *testing.T) {
	p, store := parser(t, newFollows())

	if _, ok := parseOne(t, p, post(func(_, pp map[string]any) {
		pp["message"] = "@agent-swe can you take this"
	})); !ok {
		t.Fatal("a named seat did not hear a top-level message")
	}
	if store.rows[key("mattermost", "swe", "C1", "p1")] == "" {
		t.Fatal("being named at top level did not follow the answers")
	}

	// The answer arrives as a reply under that post and reaches the seat.
	if _, ok := parseOne(t, p, post(func(_, pp map[string]any) {
		pp["root_id"], pp["id"], pp["message"] = "p1", "p2", "here you go"
	})); !ok {
		t.Fatal("the answer to a question the seat was asked was dropped")
	}

	// A message that names NOBODY follows nothing: a channel this bot
	// sits in would otherwise accrue a follow per message.
	p2, store2 := parser(t, newFollows())
	if _, ok := parseOne(t, p2, post(nil)); !ok {
		t.Fatal("a plain channel message was dropped")
	}
	if len(store2.rows) != 0 {
		t.Fatalf("a passive message recorded %v", store2.rows)
	}
}

// A direct message short-circuits everything: there is nobody else it could
// be for, so even a collective address typed into one is personal.
func TestADirectMessageIsAlwaysPersonal(t *testing.T) {
	p, _ := parser(t, newFollows())
	for _, kind := range mattermost.DirectKinds {
		got, ok := parseOne(t, p, post(func(b, pp map[string]any) {
			b["channel_type"] = kind
			pp["root_id"], pp["message"] = "root-1", "@channel heads up"
		}))
		if !ok {
			t.Fatalf("a %q message was dropped", kind)
		}
		if got.Metadata["thread_follow_reason"] != string(notify.FollowMention) {
			t.Fatalf("a %q message read as %q", kind, got.Metadata["thread_follow_reason"])
		}
	}
}

// The server's mentions list says WHETHER exactly; the text says WHY. Using
// either for the other's job gets it wrong.
func TestTheServerListAndTheTextAnswerDifferentQuestions(t *testing.T) {
	cases := []struct {
		name     string
		mentions []any
		text     string
		deliver  bool
		reason   notify.FollowReason
	}{
		// The list resolves a collective against real membership, so a
		// broadcast arrives with this seat's own id in it — and must
		// stay a broadcast rather than being promoted to a personal
		// address, which is what the status modes turn on.
		{"a broadcast", []any{"bot-1"}, "@channel standup", true, notify.FollowCollective},
		// A group or keyword mention no pattern could see.
		{"an invisible mention", []any{"bot-1"}, "the deploy broke", true, notify.FollowMention},
		{"named personally", []any{"bot-1"}, "@agent-swe look", true, notify.FollowMention},
		// The server named the targets and this seat is not among them,
		// which VETOES a pattern matching on a stale identity.
		{"somebody else", []any{"bot-9"}, "@agent-swe look", false, ""},
		// No list at all — a post re-read over REST across a reconnect
		// gap carries none — so the text decides alone.
		{"no list, named", nil, "@agent-swe look", true, notify.FollowMention},
		{"no list, silent", nil, "just chatting", false, ""},
	}
	for _, c := range cases {
		p, _ := parser(t, newFollows())
		got, ok := parseOne(t, p, post(func(b, pp map[string]any) {
			pp["root_id"], pp["id"] = "root-1", "p2"
			pp["message"] = c.text
			if c.mentions != nil {
				b["mentions"] = c.mentions
			}
		}))
		if ok != c.deliver {
			t.Errorf("%s: delivered = %v, want %v", c.name, ok, c.deliver)
			continue
		}
		if ok && got.Metadata["thread_follow_reason"] != string(c.reason) {
			t.Errorf("%s: reason = %q, want %q",
				c.name, got.Metadata["thread_follow_reason"], c.reason)
		}
	}
}

// An unresolved seat identity makes the list unusable: without an id to look
// for, "this seat was mentioned" and "somebody was mentioned" are the same
// observation, so the text has to decide alone.
func TestAnUnresolvedIdentityFallsBackToTheText(t *testing.T) {
	tracker, err := notify.NewThreadTracker(mattermost.Grammar, newFollows())
	if err != nil {
		t.Fatalf("NewThreadTracker: %v", err)
	}
	p, err := mattermost.NewParser(
		seats(mattermost.Seat{Handle: "swe", Username: "agent-swe"}),
		tracker, func() time.Time { return t0 })
	if err != nil {
		t.Fatalf("NewParser: %v", err)
	}

	got, ok := parseOne(t, p, post(func(b, pp map[string]any) {
		b["mentions"] = []any{"bot-9"}
		pp["root_id"], pp["id"], pp["message"] = "root-1", "p2", "@agent-swe look"
	}))
	if !ok || got.Metadata["thread_follow_reason"] != string(notify.FollowMention) {
		t.Fatalf("an unresolved identity produced %v/%+v", ok, got.Metadata)
	}
	// And with no id, own-message suppression is off — which is why an
	// empty id is a warning-worthy state and not merely cosmetic.
	if _, ok := parseOne(t, p, post(func(_, pp map[string]any) {
		pp["user_id"], pp["message"] = "bot-1", "@agent-swe look"
	})); !ok {
		t.Fatal("suppression fired on an unresolved identity")
	}
}

// A message re-read over REST across a reconnect gap says so: "this arrived
// while I was disconnected" changes how stale the seat should assume the
// conversation is.
func TestAReplayedMessageSaysSo(t *testing.T) {
	p, _ := parser(t, newFollows())

	got, _ := parseOne(t, p, post(func(b, _ map[string]any) { b["replayed"] = true }))
	if got.Metadata["replayed"] != "true" {
		t.Fatalf("a replayed message reads as %q", got.Metadata["replayed"])
	}
	got, _ = parseOne(t, p, post(nil))
	if _, present := got.Metadata["replayed"]; present {
		t.Fatal("a live message claims to be replayed")
	}
}

// Thread routing OFF is the pre-follow behaviour and a legitimate
// configuration: in a single-agent workspace there is no second bot for a
// thread reply to belong to.
func TestWithoutThreadRoutingEveryReplyReaches(t *testing.T) {
	p, _ := parser(t, nil)

	got, ok := parseOne(t, p, post(func(_, pp map[string]any) {
		pp["root_id"], pp["id"], pp["message"] = "root-1", "p2", "any thoughts?"
	}))
	if !ok {
		t.Fatal("a reply was dropped with thread routing off")
	}
	if got.Metadata["thread_follow_reason"] != "" {
		t.Fatalf("a follow reason appeared with no tracker: %q",
			got.Metadata["thread_follow_reason"])
	}
}

// An unreadable follow store must not deliver every reply in every thread of
// every channel this bot sits in.
func TestAnUnreadableFollowStoreDropsTheReply(t *testing.T) {
	store := newFollows()
	store.failRead = errors.New("store unreachable")
	p, _ := parser(t, store)

	if _, ok := parseOne(t, p, post(func(_, pp map[string]any) {
		pp["root_id"], pp["id"], pp["message"] = "root-1", "p2", "any thoughts?"
	})); ok {
		t.Fatal("an unreadable store delivered an unfollowed reply")
	}
	// A message that NAMES the seat still gets through: dropping it too
	// would let a store blip eat a message addressed by name.
	if _, ok := parseOne(t, p, post(func(_, pp map[string]any) {
		pp["root_id"], pp["id"], pp["message"] = "root-1", "p2", "@agent-swe look"
	})); !ok {
		t.Fatal("a store blip ate a message addressed by name")
	}
}

func TestADeliveryForAnUnknownSeatIsQuiet(t *testing.T) {
	p, _ := parser(t, newFollows())
	w := post(nil)
	w.Handle = "nobody"
	if _, ok := parseOne(t, p, w); ok {
		t.Fatal("a post for an unregistered seat woke somebody")
	}
	// A delivery naming NO seat is malformed, though: this backend
	// addresses every post at the socket that saw it, so an unaddressed
	// one means the fleet published something it should not have.
	w.Handle = ""
	if _, err := p.Parse(t.Context(), w, nil); err == nil {
		t.Fatal("an unaddressed delivery was accepted")
	}
}

func TestAMalformedPayloadIsQuietRatherThanFatal(t *testing.T) {
	p, _ := parser(t, newFollows())
	for _, body := range []map[string]any{
		{},
		{"post": "not an object"},
		{"post": map[string]any{}},
		{"post": map[string]any{"id": ""}},
	} {
		got, err := p.Parse(t.Context(), types.RawWebhook{Body: body, Handle: "swe"}, nil)
		if err != nil {
			t.Errorf("Parse(%v) reported %v, want a quiet skip", body, err)
		}
		if len(got) != 0 {
			t.Errorf("Parse(%v) produced %d notifications", body, len(got))
		}
	}
}

// A payload field is not a contract: a stray number where a string was
// expected must not stop a message reaching somebody.
func TestOddlyTypedFieldsAreCoerced(t *testing.T) {
	p, _ := parser(t, newFollows())
	got, ok := parseOne(t, p, post(func(b, pp map[string]any) {
		pp["id"] = float64(12345)
		pp["user_id"] = float64(999)
		b["channel_name"] = 7
	}))
	if !ok {
		t.Fatal("an oddly typed payload was dropped")
	}
	if got.Metadata["ts"] != "12345" || got.Metadata["user"] != "999" {
		t.Fatalf("coerced to %q/%q", got.Metadata["ts"], got.Metadata["user"])
	}
}

func TestTheParserRefusesAMismatchedTracker(t *testing.T) {
	other, err := notify.NewThreadTracker(
		notify.LiteralGrammar{Name: "elsewhere", Collectives: []string{"all"}}, newFollows())
	if err != nil {
		t.Fatalf("NewThreadTracker: %v", err)
	}
	if _, err := mattermost.NewParser(seats(seat), other, nil); err == nil {
		t.Fatal("a tracker bound to another backend was accepted")
	} else if !strings.Contains(err.Error(), "elsewhere") {
		t.Fatalf("the error does not name the mismatch: %v", err)
	}
	if _, err := mattermost.NewParser(nil, nil, nil); err == nil {
		t.Fatal("a parser was built with no seat lookup")
	}
}

func TestTheParserDeclaresItsSource(t *testing.T) {
	p, _ := parser(t, newFollows())
	if p.Source() != mattermost.Backend {
		t.Fatalf("Source = %q", p.Source())
	}
	var _ notify.Parser = p
}
