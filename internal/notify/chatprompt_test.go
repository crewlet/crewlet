package notify_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/notify"
)

var chatPrompt = notify.ChatPrompt{
	Backend: "chat", Label: "Chat",
	Address: notify.AddressRule{
		DirectKinds: []string{"D", "G"},
		Follows:     notify.AddressingFollows(),
	},
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

// direct builds one message's metadata in a direct conversation, and shared
// one in an open channel: the two shapes the two keys answer differently for.
func direct(mutate func(map[string]string)) map[string]string {
	m := map[string]string{"channel": "D1", "channel_type": "D", "ts": "p1"}
	if mutate != nil {
		mutate(m)
	}
	return m
}

func shared(ts, root string) string {
	return chatPrompt.PartitionKey(map[string]string{
		"channel": "C1", "channel_type": "O", "ts": ts, "thread_ts": root,
	}, "")
}

func identity(ts, root string) string {
	return chatPrompt.ConversationIdentity(map[string]string{
		"channel": "C1", "channel_type": "O", "ts": ts, "thread_ts": root,
	}, "")
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
	// IT NO LONGER SENDS THE AGENT TO GO AND READ THE THREAD. On a company
	// whose chat tools come from a per-role MCP server that instruction cost
	// three rounds before a word was read, and an agent that skipped it
	// answered eleven words of trigger text with no idea what the thread was
	// about. The thread is now in the SYSTEM prompt, under its own heading.
	if strings.Contains(got, "Read the thread with your chat tools") {
		t.Fatalf("the prompt still tells the agent to go and fetch the thread:\n%s", got)
	}
	if !strings.Contains(got, "**Thread:** root-1 (existing thread)") {
		t.Fatalf("the thread pointer is wrong:\n%s", got)
	}
	// THE RECON FLAG STAYS TRUE, and the engine handing the thread over does
	// not change it. It describes the trigger BODY, and "+1" is exactly as
	// useless a search query with the thread in the system prompt as it was
	// without — so flipping it would turn two auxiliary LLM calls back on for
	// every chat thread reply in the company. It is also stored on every past
	// event and read by the dashboard, so its meaning cannot be changed
	// retroactively.
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
	// A stranger stays a stranger, with the display name the third-party app gave.
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

// In a direct conversation consecutive TOP-LEVEL messages are one partition,
// so a typing burst coalesces into one turn — the headline case for
// coalescing at all.
func TestPartitionKeysGroupATypingBurstButNotTwoAsks(t *testing.T) {
	first := chatPrompt.PartitionKey(direct(nil), "")
	second := chatPrompt.PartitionKey(direct(func(m map[string]string) { m["ts"] = "p2" }), "")
	if first != "D1" || second != "D1" {
		t.Fatalf("a DM burst keyed %q and %q, want the channel both times", first, second)
	}

	// A DM THREAD REPLY keeps its thread key: merging it with unrelated
	// top-level pings would hand the turn one metadata whose thread
	// pointer names only one of two reply targets.
	inThread := chatPrompt.PartitionKey(direct(func(m map[string]string) {
		m["thread_ts"] = "root-1"
	}), "")
	if inThread != "D1:root-1" {
		t.Fatalf("a DM thread reply keyed %q", inThread)
	}

	// In a SHARED channel two unrelated top-level asks must not merge, so
	// the key stays thread-grained throughout.
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
	if got := chatPrompt.PartitionKey(map[string]string{"ts": "p1"}, ""); got != "" {
		t.Fatalf("a channel-less message keyed %q", got)
	}
	if got := chatPrompt.PartitionKey(map[string]string{"channel": "C1"}, ""); got != "" {
		t.Fatalf("an anchor-less message keyed %q", got)
	}
}

// A DIRECT CONVERSATION IS ONE CONVERSATION, thread or not — which is the
// defect this pair was split to fix. A person's first DM and their reply in
// the thread the agent opened are the same 1:1 line, and while both answers
// came from one function the reply was filed under "D1:root-1" and the next
// turn looked the history up under "D1" and found a first turn.
func TestADirectConversationIsOneIdentityHoweverItIsThreaded(t *testing.T) {
	top := chatPrompt.ConversationIdentity(direct(nil), "")
	later := chatPrompt.ConversationIdentity(direct(func(m map[string]string) { m["ts"] = "p2" }), "")
	reply := chatPrompt.ConversationIdentity(direct(func(m map[string]string) {
		m["thread_ts"] = "root-1"
		m["ts"] = "p3"
	}), "")
	if top != "D1" || later != "D1" || reply != "D1" {
		t.Fatalf("one DM resolved to %q, %q and %q — a thread reply is not a new conversation",
			top, later, reply)
	}

	// A SHARED CHANNEL IS THE OPPOSITE: a thread IS the conversation
	// there, so two unrelated asks in one channel stay two identities and
	// a reply joins the one it answers.
	if a, b := identity("p1", ""), identity("p2", ""); a == b {
		t.Fatalf("two unrelated channel asks share the identity %q", a)
	}
	if got, want := identity("p9", "p1"), identity("p1", ""); got != want {
		t.Fatalf("a channel reply resolved to %q, want its root's %q", got, want)
	}
	// The same two guards the partition key has: no channel and no anchor
	// each mean this source cannot name a conversation at all, and both
	// keys must agree about that or [notify.Derived] gates recording under
	// one reading and not the other.
	if got := chatPrompt.ConversationIdentity(map[string]string{"ts": "p1"}, ""); got != "" {
		t.Fatalf("a channel-less message resolved to %q", got)
	}
	if got := chatPrompt.ConversationIdentity(map[string]string{"channel": "C1"}, ""); got != "" {
		t.Fatalf("an anchor-less shared-channel message resolved to %q", got)
	}
}

// THE PARTITION REFINES THE IDENTITY, on every chat shape: a partition key is
// the identity itself or the identity plus a finer anchor. That is what makes
// one ledger entry per turn well-defined — a turn is a partition, its entry is
// written once, and constituents disagreeing about the identity would make the
// ledger key depend on which event sorted first.
//
// The DM burst is the case that decides it: its three constituents partition
// on the bare channel while carrying three different ts values, so an identity
// of channel+anchor would give that one partition three identities.
func TestAPartitionKeyAlwaysRefinesTheConversationIdentity(t *testing.T) {
	cases := map[string]map[string]string{
		"a DM burst":                  direct(nil),
		"a second message in that DM": direct(func(m map[string]string) { m["ts"] = "p2" }),
		"a DM thread reply":           direct(func(m map[string]string) { m["thread_ts"] = "root-1" }),
		"a shared-channel top-level":  {"channel": "C1", "channel_type": "O", "ts": "p1"},
		"a shared-channel thread reply": {
			"channel": "C1", "channel_type": "O", "ts": "p9", "thread_ts": "p1",
		},
	}
	for name, meta := range cases {
		part := chatPrompt.PartitionKey(meta, "")
		id := chatPrompt.ConversationIdentity(meta, "")
		if part == "" || id == "" {
			t.Errorf("%s: partition %q, identity %q — neither may be empty here", name, part, id)
			continue
		}
		if part != id && !strings.HasPrefix(part, id+":") {
			t.Errorf("%s: partition %q does not refine the identity %q", name, part, id)
		}
	}

	// AND THE DM CASE ACTUALLY DIVERGES, or everything above is satisfied
	// by handing back one value twice — which is precisely the state this
	// change removed.
	threaded := direct(func(m map[string]string) { m["thread_ts"] = "root-1" })
	if chatPrompt.PartitionKey(threaded, "") == chatPrompt.ConversationIdentity(threaded, "") {
		t.Error("a DM thread reply's two keys are equal, so the split bought nothing")
	}
	// While a shared channel's do NOT diverge: there the thread is the
	// conversation and a second value would be a second answer to one
	// question.
	top := map[string]string{"channel": "C1", "channel_type": "O", "ts": "p1"}
	if chatPrompt.PartitionKey(top, "") != chatPrompt.ConversationIdentity(top, "") {
		t.Error("a shared-channel message's two keys differ, so a thread is two conversations")
	}
}

// ONE ANCHOR, THREE READERS. The working indicator raises its spinner on
// [notify.ConversationOf]'s channel+anchor, Build tells the agent to reply
// under that same ts, and the ledger keys on the identity — and nothing
// compares the three derivations. Where they disagree, the spinner a person
// watches, the thread the reply lands in and the history the seat reads name
// different threads.
func TestTheChatIdentityIsTheAnchorTheIndicatorAndTheReplyUse(t *testing.T) {
	for _, meta := range []map[string]string{
		{"channel": "C1", "channel_type": "O", "ts": "p1", "transport": "chat"},
		{"channel": "C1", "channel_type": "O", "ts": "p9", "thread_ts": "p1", "transport": "chat"},
	} {
		conv, ok := notify.ConversationOf(meta, "chat")
		if !ok {
			t.Fatalf("%v: the indicator resolved no conversation", meta)
		}
		if got, want := chatPrompt.ConversationIdentity(meta, ""),
			conv.Channel+":"+conv.Thread; got != want {
			t.Errorf("%v: the ledger keys on %q and the indicator raises on %q", meta, got, want)
		}
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
		if !chatPrompt.Address.IsDirect(map[string]string{notify.ChannelTypeField: kind}) {
			t.Errorf("%q is not read as direct", kind)
		}
	}
	for _, kind := range []string{"O", "P", ""} {
		if chatPrompt.Address.IsDirect(map[string]string{notify.ChannelTypeField: kind}) {
			t.Errorf("%q is read as direct", kind)
		}
	}
	// A backend WITH a meaningful prefix uses it as the fallback.
	prefixed := chatPrompt
	prefixed.Address.DMPrefix = "D"
	if !prefixed.Address.IsDirect(map[string]string{notify.ChannelField: "D0123"}) {
		t.Fatal("the prefix fallback did not fire")
	}
	// And a backend with opaque ids must not: it would mark arbitrary
	// public channels as direct messages.
	if chatPrompt.Address.IsDirect(map[string]string{notify.ChannelField: "D0123"}) {
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
		"a direct message": func(m map[string]string) { m[notify.ChannelTypeField] = "D" },
		"a group DM":       func(m map[string]string) { m[notify.ChannelTypeField] = "G" },
		"a message naming it": func(m map[string]string) {
			m[notify.FollowReasonField] = string(notify.FollowMention)
		},
		"a thread it is in because it was named": func(m map[string]string) {
			m[notify.FollowingField] = string(notify.FollowMention)
		},
	} {
		n := chatNote(mutate)
		if !chatPrompt.Addressed(n) {
			t.Errorf("%s does not address the seat", name)
		}
		// The two answers are the SAME rule, read through both doors.
		if !chatPrompt.Address.Addressed(n.Metadata) {
			t.Errorf("%s: the indicator and the prompt disagree", name)
		}
	}
	// A passive channel message is the counterfactual: every bot in the
	// room wakes on one, and a seat obliged to answer each would post N
	// replies to traffic nobody addressed to any of them.
	if chatPrompt.Addressed(chatNote(nil)) {
		t.Error("a passive channel message addresses the seat")
	}
	// And a STANDING FOLLOW is not an ask: a seat that spoke in a thread
	// once does not owe every later message in it an answer.
	spoke := chatNote(func(m map[string]string) {
		m[notify.ThreadField] = "p0"
		m[notify.FollowingField] = string(notify.FollowParticipated)
		m[notify.FollowReasonField] = string(notify.FollowParticipated)
	})
	if chatPrompt.Addressed(spoke) {
		t.Error("a reply in a thread the seat merely spoke in addresses it")
	}
}
