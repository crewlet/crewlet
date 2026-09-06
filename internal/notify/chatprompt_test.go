package notify_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/notify"
)

var chatPrompt = notify.ChatPrompt{
	Backend: "chat", Label: "Chat",
	DirectKinds: []string{"D", "G"},
	Collectives: "`@all` / `@channel` / `@here`",
	SelfReference: func(m map[string]string) string {
		if n := m["bot_username"]; n != "" {
			return "`@" + n + "`"
		}
		return ""
	},
	MentionHint: func(map[string]string) string { return "**Mentions:** write `@username`." },
	IdentityNote: func(m map[string]string) string {
		if id := m["bot_user_id"]; id != "" {
			return "Your id is `" + id + "`."
		}
		return ""
	},
}

func chatNote(mutate func(map[string]string)) notify.Inbound {
	m := map[string]string{
		"transport": "chat", "channel": "C1", "ts": "p1",
		"channel_type": "O", "user": "u-ana",
		"bot_username": "agent-swe", "bot_user_id": "bot-1",
		notify.RecipientField: "swe",
	}
	if mutate != nil {
		mutate(m)
	}
	return notify.Inbound{
		Source: "chat", EventType: "posted", Sender: "ana",
		Body: "can you look at this", Metadata: m,
	}
}

func TestTheChatPromptCarriesTheTriageRules(t *testing.T) {
	got := chatPrompt.Build(chatNote(nil), nil)

	for _, want := range []string{
		"A Chat message was posted by **ana**",
		"Your handle is `swe`",
		"Your id is `bot-1`",
		"## Triage",
		"`@agent-swe` or `swe`",
		"`@all` / `@channel` / `@here`",
		"silence beats noise",
		"**Message:** can you look at this",
		"**From:** ana",
		"**Channel:** C1",
		"**Thread:** p1 (top-level message — reply as a thread)",
		"**Message id:** p1",
		"**Mentions:** write `@username`.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the prompt does not carry %q:\n%s", want, got)
		}
	}
	// NOTHING NAMES A TOOL: the deployed MCP server's tool names are not
	// knowable by the engine, so the prompt describes the capability.
	for _, tool := range []string{"mattermost_post", "slack_post", "create_post("} {
		if strings.Contains(got, tool) {
			t.Errorf("the prompt names a tool: %q", tool)
		}
	}
}

// A thread reply gets the self-check block, because the triggering message
// is usually thin and the thread is the context.
func TestAThreadReplyGetsTheThreadBlock(t *testing.T) {
	reply := chatNote(func(m map[string]string) { m["thread_ts"] = "root-1" })

	got := chatPrompt.Build(reply, nil)
	if !strings.Contains(got, "## Thread context") {
		t.Fatalf("a thread reply got no thread block:\n%s", got)
	}
	if !strings.Contains(got, "Messages from `@agent-swe` in this thread are YOUR previous replies") {
		t.Fatalf("the self-check does not name the agent:\n%s", got)
	}
	if !strings.Contains(got, "**Thread:** root-1 (existing thread)") {
		t.Fatalf("the thread pointer is wrong:\n%s", got)
	}
	if !chatPrompt.RequiresRecon(reply) {
		t.Fatal("a thread reply does not ask for recon")
	}

	// A top-level message carries its own body and needs no such trip.
	top := chatNote(nil)
	if chatPrompt.RequiresRecon(top) {
		t.Fatal("a top-level message asked for recon")
	}
	if strings.Contains(chatPrompt.Build(top, nil), "## Thread context") {
		t.Fatal("a top-level message got the thread block")
	}
}

// An unresolved identity degrades rather than rendering an empty marker: the
// prompt still names the handle, and the self-check falls back to prose.
func TestAnUnresolvedIdentityStillReads(t *testing.T) {
	bare := chatNote(func(m map[string]string) {
		delete(m, "bot_username")
		delete(m, "bot_user_id")
		m["thread_ts"] = "root-1"
	})

	got := chatPrompt.Build(bare, nil)
	// The marker must be the handle ALONE, not an empty reference joined
	// to it — "names you —  or `swe`" reads as a rendering fault and
	// teaches the agent that something about its identity is missing.
	if !strings.Contains(got, "names you — `swe`,") {
		t.Fatalf("an empty identity left a dangling marker:\n%s", got)
	}
	if strings.Contains(got, "Your id is ``") {
		t.Fatalf("an empty id rendered as empty markup:\n%s", got)
	}
	if !strings.Contains(got, "Messages from your own account in this thread") {
		t.Fatalf("the self-check has no fallback:\n%s", got)
	}
	if !strings.Contains(got, "Your handle is `swe`") {
		t.Fatalf("the handle went missing:\n%s", got)
	}
}

// A colleague is annotated so the agent treats a person as a person — who
// replies on their own time and cannot be reached by an agent-to-agent ask —
// rather than as an opaque platform id.
func TestAKnownColleagueIsAnnotated(t *testing.T) {
	r := registry(t)
	if err := r.Register("chat", "u-ana", "dana-founder"); err != nil {
		t.Fatalf("register: %v", err)
	}

	got := chatPrompt.Build(chatNote(nil), r)
	if !strings.Contains(got, "human colleague") {
		t.Fatalf("a human sender was not annotated as one:\n%s", got)
	}
	if !strings.Contains(got, "`u-ana`") {
		t.Fatalf("the annotation dropped the platform id:\n%s", got)
	}
	// A stranger stays a stranger, with the display name the vendor gave.
	plain := chatPrompt.Build(chatNote(nil), notify.NewRegistry(nil))
	if !strings.Contains(plain, "posted by **ana**") {
		t.Fatalf("an unknown sender rendered as %q", plain)
	}
}

// The transport always writes the sender key, so an absent sender arrives as
// an empty string — a lookup default would never fire and the prompt would
// say "posted by ****".
func TestAnUnnamedSenderNeverRendersBlank(t *testing.T) {
	n := chatNote(nil)
	n.Sender = ""
	if got := chatPrompt.Build(n, nil); !strings.Contains(got, "posted by **u-ana**") {
		t.Fatalf("an empty sender fell back to %q", got)
	}
	n.Metadata["user"] = ""
	if got := chatPrompt.Build(n, nil); !strings.Contains(got, "posted by **unknown**") {
		t.Fatalf("a wholly unnamed sender rendered as %q", got)
	}
	// An empty body says so rather than rendering a dangling label.
	n.Body = ""
	if got := chatPrompt.Build(n, nil); !strings.Contains(got, "**Message:** (empty)") {
		t.Fatalf("an empty body rendered as %q", got)
	}
}

// In a direct conversation consecutive TOP-LEVEL messages are one
// conversation, so a typing burst coalesces into one turn — the headline
// case for coalescing at all.
func TestConversationKeysGroupATypingBurstButNotTwoAsks(t *testing.T) {
	direct := func(mutate func(map[string]string)) map[string]string {
		m := map[string]string{"channel": "D1", "channel_type": "D", "ts": "p1"}
		if mutate != nil {
			mutate(m)
		}
		return m
	}

	first := chatPrompt.ConversationKey(direct(nil), "")
	second := chatPrompt.ConversationKey(direct(func(m map[string]string) { m["ts"] = "p2" }), "")
	if first != "D1" || second != "D1" {
		t.Fatalf("a DM burst keyed %q and %q, want the channel both times", first, second)
	}

	// A DM THREAD REPLY keeps its thread key: merging it with unrelated
	// top-level pings would hand the turn one metadata whose thread
	// pointer names only one of two reply targets.
	inThread := chatPrompt.ConversationKey(direct(func(m map[string]string) {
		m["thread_ts"] = "root-1"
	}), "")
	if inThread != "D1:root-1" {
		t.Fatalf("a DM thread reply keyed %q", inThread)
	}

	// In a SHARED channel two unrelated top-level asks must not merge, so
	// the key stays thread-grained throughout.
	shared := func(ts, root string) string {
		return chatPrompt.ConversationKey(map[string]string{
			"channel": "C1", "channel_type": "O", "ts": ts, "thread_ts": root,
		}, "")
	}
	if a, b := shared("p1", ""), shared("p2", ""); a == b {
		t.Fatalf("two unrelated channel asks share the key %q", a)
	}
	// A top-level message keys on its OWN id, so its later replies land
	// in the same partition.
	if top, reply := shared("p1", ""), shared("p9", "p1"); top != reply {
		t.Fatalf("a reply keyed %q, want its root's %q", reply, top)
	}
	// Without a channel there is no conversation identity — and a key
	// that was just the thread would collide across channels.
	if got := chatPrompt.ConversationKey(map[string]string{"ts": "p1"}, ""); got != "" {
		t.Fatalf("a channel-less message keyed %q", got)
	}
	if got := chatPrompt.ConversationKey(map[string]string{"channel": "C1"}, ""); got != "" {
		t.Fatalf("an anchor-less message keyed %q", got)
	}
}

// A chat backend has no supersede rule — every event it emits IS a message,
// and a person typing four times has said four things.
func TestEveryChatConstituentIsKept(t *testing.T) {
	for _, kind := range []string{"posted", "post_edited", "anything"} {
		if got := chatPrompt.DigestBody(kind, "the message"); got != "the message" {
			t.Fatalf("%s collapsed to %q", kind, got)
		}
	}
	// And nothing a chat backend emits wakes its own actor: the parser
	// suppresses the echo, and no chat event is an OUTCOME the actor does
	// not already know about.
	for _, kind := range []string{"posted", "post_edited", "reaction_added"} {
		if chatPrompt.WakesActor(kind) {
			t.Fatalf("%s wakes its own actor", kind)
		}
	}
}

func TestADirectConversationIsRecognised(t *testing.T) {
	for _, kind := range []string{"D", "G"} {
		if !chatPrompt.IsDirect(map[string]string{"channel_type": kind}) {
			t.Errorf("%q is not read as direct", kind)
		}
	}
	for _, kind := range []string{"O", "P", ""} {
		if chatPrompt.IsDirect(map[string]string{"channel_type": kind}) {
			t.Errorf("%q is read as direct", kind)
		}
	}
	// A backend WITH a meaningful prefix uses it as the fallback.
	prefixed := chatPrompt
	prefixed.DMPrefix = "D"
	if !prefixed.IsDirect(map[string]string{"channel": "D0123"}) {
		t.Fatal("the prefix fallback did not fire")
	}
	// And a backend with opaque ids must not: it would mark arbitrary
	// public channels as direct messages.
	if chatPrompt.IsDirect(map[string]string{"channel": "D0123"}) {
		t.Fatal("a prefix-less backend used a prefix anyway")
	}
}

func TestTheChatPromptSatisfiesTheInterface(t *testing.T) {
	var _ notify.Prompt = chatPrompt
	if chatPrompt.Source() != "chat" {
		t.Fatalf("Source = %q", chatPrompt.Source())
	}
	// A bare prompt still renders: every per-backend hook is optional,
	// which is what lets a new backend start with only a name.
	bare := notify.ChatPrompt{Backend: "new"}
	got := bare.Build(chatNote(nil), nil)
	if !strings.Contains(got, "A chat message was posted") {
		t.Fatalf("a bare prompt rendered %q", got)
	}
	if !strings.Contains(got, "`@channel` / `@here`") {
		t.Fatalf("a bare prompt has no collectives:\n%s", got)
	}
}

// ONE IMPLEMENTATION with the working-status indicator, deliberately: the
// indicator says "this agent is working on your message" and the delivery
// check says "this agent owes your message an answer". The two disagreeing
// would raise a spinner on a turn allowed to end in silence, or end one in
// silence after raising a spinner.
func TestTheChatPromptAddressesTheSameMessagesTheIndicatorDoes(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(map[string]string){
		"a direct message":     func(m map[string]string) { m["channel_type"] = "D" },
		"a group DM":           func(m map[string]string) { m["channel_type"] = "G" },
		"a mention it follows": func(m map[string]string) { m["thread_follow_reason"] = "mention" },
		"a thread it follows":  func(m map[string]string) { m["thread_following"] = "yes" },
	} {
		n := chatNote(mutate)
		if !chatPrompt.Addressed(n) {
			t.Errorf("%s does not address the seat", name)
		}
		// The two answers are the SAME rule, read through both doors.
		if got := notify.Addressed(n.Metadata, chatPrompt.DMPrefix); !got {
			t.Errorf("%s: the indicator and the prompt disagree", name)
		}
	}
	// A passive channel message is the counterfactual: every bot in the
	// room wakes on one, and a seat obliged to answer each would post N
	// replies to traffic nobody addressed to any of them.
	if chatPrompt.Addressed(chatNote(nil)) {
		t.Error("a passive channel message addresses the seat")
	}
}
