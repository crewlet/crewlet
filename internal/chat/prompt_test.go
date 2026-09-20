package chat_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/notify"
)

// WHAT A MESSAGE ASKS OF THE SEAT IT REACHED.
//
// Every case below is built from a trigger the PARSER produced, never from a
// hand-written metadata map. The prompt is the last reader of a vocabulary
// four other places write, and a case that stamped its own keys would keep
// passing after the producer stopped writing one — which is the exact defect
// the shared vocabulary exists to close.

// chatParties is the roster as a prompt resolves it.
//
// ByExternalID ANSWERS NOTHING, deliberately. A native seat holds no external
// id in any namespace — there is nothing to scope by transport — and a stub
// that answered here would let a lookup by the wrong identity pass unnoticed.
type chatParties map[string]notify.Party

func (chatParties) ByExternalID(string, string) (notify.Party, bool) {
	return notify.Party{}, false
}

func (p chatParties) ByHandle(handle string) (notify.Party, bool) {
	party, ok := p[handle]
	return party, ok
}

// trigger is one seat's wake, as the spine hands it to a prompt: the parser's
// own delivery plus the recipient handle, which is resolved after the parser
// and is the one fact it genuinely cannot know.
func trigger(t *testing.T, recipient string, shape func(*chat.Notify)) notify.Inbound {
	t.Helper()
	rec := aPost()
	rec.Notify.Recipients = []chat.Recipient{
		{Handle: recipient, Reason: chat.ReasonMention},
	}
	shape(rec.Notify)

	routed := wakes(t, rec, company(t, []string{"jane", recipient}))
	if len(routed) != 1 {
		t.Fatalf("the record woke %+v, want %s alone", routed, recipient)
	}
	in := routed[0].Inbound
	in.Metadata[notify.RecipientField] = recipient
	return in
}

// A NATIVE WAKE KEEPS THE SEAT'S OWN CONTEXT, THREAD REPLY OR NOT.
//
// The recon flag says the trigger is a POINTER — that the body only says where
// to look — and it is TRUE on a vendor's thread reply for a good reason: the
// webhook carries a fragment and the conversation is somewhere else.
//
// A native wake is not in that position. It carries what was said, who said
// it, which room, which thread and why it reached this seat, all copied onto
// the record inside the decide that routed it. And the flag is not free:
// setting it also suppresses the turn-start personal-memory filtering and
// episode recall, so a chat turn raised on it loses exactly the context that
// decides how to answer this colleague — what they asked last week.
func TestANativeWakeKeepsTheSeatsOwnContext(t *testing.T) {
	t.Parallel()
	prompt := chat.NewPrompt()
	reply := trigger(t, "eng", func(n *chat.Notify) { n.ThreadRoot = "msg-0" })
	if prompt.RequiresRecon(reply) {
		t.Error("a native thread reply is treated as a pointer, so the seat " +
			"is told to go and read and loses its memory and episode recall " +
			"for a trigger that already carries the message")
	}
	top := trigger(t, "eng", func(*chat.Notify) {})
	if prompt.RequiresRecon(top) {
		t.Error("a native top-level message is treated as a pointer")
	}
	// AND THE SEAT IS STILL SENT TO THE CONVERSATION. Not being a pointer
	// is not the same as being the whole story: the thread around the
	// message is read with the seat's own tools, and the prompt says so.
	if !strings.Contains(prompt.Build(reply, nil), "thread") {
		t.Error("a thread reply's prompt never mentions the thread it is in")
	}
}

// A TYPING BURST IN A DIRECT CONVERSATION IS ONE TURN, AND TWO ASKS IN A ROOM
// ARE TWO.
//
// The conversation key is what the inbox partitions on, so it decides what
// merges. In a direct conversation a person's consecutive top-level messages
// are one conversation — that is the headline case for coalescing at all, and
// keying them per message would spend a turn on each line somebody typed.
// In a shared room two unrelated top-level asks must NOT merge, so the key
// stays thread-grained: a top-level message keys on its OWN id, which is what
// the replies under it will carry.
//
// A DIRECT THREAD REPLY KEEPS ITS THREAD KEY: merging it with unrelated
// top-level pings would hand the turn one merged metadata whose thread pointer
// names only one of two reply targets, steering the answer to the wrong place.
func TestTheConversationKeyMergesADirectBurstAndNothingElse(t *testing.T) {
	t.Parallel()
	prompts := notify.NewPrompts(chat.NewPrompt())
	for name, tc := range map[string]struct {
		kind   chat.Kind
		thread string
		want   string
	}{
		"a direct message":         {chat.KindDM, "", "chat:room-1"},
		"a group conversation":     {chat.KindGroup, "", "chat:room-1"},
		"a reply in a direct":      {chat.KindDM, "msg-0", "chat:room-1:msg-0"},
		"a room post":              {chat.KindPublic, "", "chat:room-1:msg-1"},
		"a reply in a room":        {chat.KindPublic, "msg-0", "chat:room-1:msg-0"},
		"a unit room's own thread": {chat.KindUnit, "msg-0", "chat:room-1:msg-0"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			in := trigger(t, "eng", func(n *chat.Notify) {
				n.ChannelKind, n.ThreadRoot = tc.kind, tc.thread
			})
			if got := prompts.Key(in); got != tc.want {
				t.Errorf("%s keys on %q, want %q", name, got, tc.want)
			}
		})
	}
}

// THE PROMPT NAMES THE TOOLS THIS BUILD SHIPS.
//
// The rule that a prompt must not name a tool is about a deployed MCP server,
// whose tool names the engine cannot know. These three are registered by this
// build, under names this build chose, on every seat that can read the
// notification at all — and naming them is what makes the difference between
// "reply where you were asked" and an agent posting its answer as a new
// top-level message in the room, which reads as a second conversation.
func TestThePromptNamesTheToolsThisBuildShips(t *testing.T) {
	t.Parallel()
	built := chat.NewPrompt().Build(trigger(t, "eng", func(*chat.Notify) {}), nil)
	for _, tool := range []string{"reply_in_thread", "post_message", "send_dm"} {
		if !strings.Contains(built, tool) {
			t.Errorf("an addressed chat trigger never names %s, so the seat "+
				"has to guess which of its tools answers a message", tool)
		}
	}
}

// AN UNADDRESSED WAKE IS TOLD IT IS NEWS.
//
// The other half of the same decision, and the one that keeps a company
// usable: a seat that answers every message it observes makes a busy room
// unreadable within an hour. It is branched on the stamped verdict rather than
// on the reason again, because being asked and being told are what a recipient
// does differently.
func TestAnUnaddressedWakeIsToldItIsNewsRatherThanAnAsk(t *testing.T) {
	t.Parallel()
	prompt := chat.NewPrompt()
	in := trigger(t, "eng", func(n *chat.Notify) {
		n.ThreadRoot = "msg-0"
		n.Recipients = []chat.Recipient{{Handle: "eng", Reason: chat.ReasonFollow}}
	})
	if prompt.Addressed(in) {
		t.Fatal("control: following a thread was read as being asked something")
	}
	built := prompt.Build(in, nil)
	if !strings.Contains(built, "news, not a request") {
		t.Errorf("an unaddressed wake is not told it may stay silent:\n%s", built)
	}
	if strings.Contains(built, "Somebody is waiting on this message") {
		t.Error("an unaddressed wake is told somebody is waiting for an " +
			"answer, which obliges a seat to reply to every remark in every " +
			"thread it follows")
	}
}

// THE SEAT IS TOLD WHY IT WAS WOKEN, IN ITS OWN TERMS.
//
// The routing reached a conclusion the TEXT cannot always show: nothing in "we
// should ship on Friday" says it arrived because nobody else in the unit was
// listening. The triage block teaches how to find an addressee in the words;
// this is what the router already decided.
//
// THE UNKNOWN REASON IS A ROW because it is the one a rolling upgrade
// guarantees: a newer build routed this seat deliberately, and a prompt that
// said nothing would leave it guessing from the text alone.
func TestThePromptSaysWhyThisSeatWasWoken(t *testing.T) {
	t.Parallel()
	for reason, want := range map[chat.Reason]string{
		chat.ReasonDM:              "direct conversation",
		chat.ReasonMention:         "named in this message",
		chat.ReasonReplyToOwnRoot:  "thread YOU started",
		chat.ReasonLeadFallback:    "unit's lead",
		chat.ReasonReply:           "spoken in this thread",
		chat.ReasonCollective:      "whole room",
		chat.ReasonFollow:          "follow this thread",
		chat.ReasonFollowAll:       "everything said in this room",
		chat.Reason("newer-build"): "newer build routed this to you",
	} {
		t.Run(string(reason), func(t *testing.T) {
			t.Parallel()
			in := trigger(t, "eng", func(n *chat.Notify) {
				n.ChannelKind, n.ThreadRoot, n.Lead = chat.KindUnit, "msg-0", "eng"
				n.AuthorKind = chat.AuthorHuman
				n.Recipients = []chat.Recipient{{Handle: "eng", Reason: reason}}
			})
			built := chat.NewPrompt().Build(in, nil)
			if !strings.Contains(built, want) {
				t.Errorf("a wake routed as %q never says %q:\n%s",
					reason, want, built)
			}
		})
	}
}

// THE SENDER IS RESOLVED BY HANDLE, and a person is named as one.
//
// A native sender IS a seat's own identity, so there is no transport-scoped id
// to look up — [notify.Parties.ByHandle] exists for exactly the first-party
// sources. What the resolution buys is the difference between a bare handle
// and a colleague the agent can act on: a HUMAN replies on their own time and
// cannot be reached by an agent-to-agent ask, so an agent told to treat them
// as a seat goes looking for a tool that will never answer.
func TestTheSenderIsResolvedByHandleAndAPersonIsNamedAsOne(t *testing.T) {
	t.Parallel()
	in := trigger(t, "eng", func(*chat.Notify) {})
	parties := chatParties{"jane": {Handle: "jane", Name: "Jane", Human: true}}

	built := chat.NewPrompt().Build(in, parties)
	if !strings.Contains(built, "human colleague") {
		t.Errorf("a person's message renders them as an ordinary seat:\n%s",
			built)
	}
	if !strings.Contains(built, "Jane") {
		t.Errorf("the sender is not named:\n%s", built)
	}
}

// AN OPERATOR AND THE ENGINE'S OWN NARRATION ARE NOT SEATS.
//
// Neither resolves through the roster at all, and rendering either as a bare
// handle invites the agent to answer a colleague who does not exist. The
// author kind is on the record precisely so the prompt can say what it is.
func TestAnAuthorWhoIsNotASeatIsDescribedRatherThanNamed(t *testing.T) {
	t.Parallel()
	in := trigger(t, "eng", func(n *chat.Notify) {
		n.Author, n.AuthorKind = "tok-1", chat.AuthorOperator
	})
	built := chat.NewPrompt().Build(in, chatParties{})
	if !strings.Contains(built, "an operator") {
		t.Errorf("an operator's message renders them as a colleague:\n%s", built)
	}
}

// A PARTIAL BROADCAST SAYS IT IS ONE.
//
// An `@channel` that exceeded the cap reached fewer seats than the room has,
// and it reads exactly like a complete one unless it is said: the seats that
// were woken assume the rest heard it too, and nobody repeats it.
func TestATruncatedBroadcastTellsTheSeatsItReachedFewerThanItNamed(t *testing.T) {
	t.Parallel()
	plain := trigger(t, "eng", func(*chat.Notify) {})
	if strings.Contains(chat.NewPrompt().Build(plain, nil), "fewer seats") {
		t.Fatal("control: an ordinary message claims its broadcast was cut short")
	}
	cut := trigger(t, "eng", func(n *chat.Notify) { n.WakesTruncated = true })
	if !strings.Contains(chat.NewPrompt().Build(cut, nil), "fewer seats") {
		t.Error("a truncated @channel is rendered as though it reached the " +
			"whole room")
	}
}

// A ZERO PROMPT STILL NAMES ITS SOURCE.
//
// [notify.NewPrompts] SKIPS a prompt whose source is empty. An embedded value
// answers with its Backend field, which is empty in a zero struct — so a
// wiring mistake would register nothing, every chat wake in the company would
// render through the generic fallback, and there would be no symptom anywhere.
// The source is the package constant, which cannot be empty.
func TestAZeroPromptStillNamesItsSource(t *testing.T) {
	t.Parallel()
	if got := (chat.Prompt{}).Source(); got != chat.Source {
		t.Errorf("a zero prompt names source %q, so the registry silently "+
			"drops it and every chat wake renders as a generic notification",
			got)
	}
	if got := notify.NewPrompts(chat.NewPrompt()).For(chat.Source); got.Source() != chat.Source {
		t.Errorf("the registry resolves %q for native chat", got.Source())
	}
}
