package chatlet_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/provenance"
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
// ByExternalID ANSWERS NOTHING, deliberately. A Chatlet seat holds no external
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
func trigger(t *testing.T, recipient string, shape func(*chatlet.Notify)) notify.Inbound {
	t.Helper()
	rec := aPost()
	rec.Notify.Recipients = []chatlet.Recipient{
		{Handle: recipient, Reason: chatlet.ReasonMention},
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

// A CHATLET WAKE KEEPS THE SEAT'S OWN CONTEXT, THREAD REPLY OR NOT.
//
// The recon flag says the trigger is a POINTER — that the body only says where
// to look — and it is TRUE on a vendor's thread reply for a good reason: the
// webhook carries a fragment and the conversation is somewhere else.
//
// A Chatlet wake is not in that position. It carries what was said, who said
// it, which room, which thread and why it reached this seat, all copied onto
// the record inside the decide that routed it. And the flag is not free:
// setting it also suppresses the turn-start personal-memory filtering and
// episode recall, so a chat turn raised on it loses exactly the context that
// decides how to answer this colleague — what they asked last week.
func TestAChatletWakeKeepsTheSeatsOwnContext(t *testing.T) {
	t.Parallel()
	prompt := chatlet.NewPrompt()
	reply := trigger(t, "eng", func(n *chatlet.Notify) { n.ThreadRoot = "msg-0" })
	if prompt.RequiresRecon(reply) {
		t.Error("a Chatlet thread reply is treated as a pointer, so the seat " +
			"is told to go and read and loses its memory and episode recall " +
			"for a trigger that already carries the message")
	}
	top := trigger(t, "eng", func(*chatlet.Notify) {})
	if prompt.RequiresRecon(top) {
		t.Error("a Chatlet top-level message is treated as a pointer")
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
// The partition key is what the inbox partitions on, so it decides what
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
func TestThePartitionKeyMergesADirectBurstAndNothingElse(t *testing.T) {
	t.Parallel()
	prompts := notify.NewPrompts(chatlet.NewPrompt())
	for name, tc := range map[string]struct {
		kind   chatlet.Kind
		thread string
		want   string
	}{
		"a direct message":         {chatlet.KindDM, "", "chat:room-1"},
		"a group conversation":     {chatlet.KindGroup, "", "chat:room-1"},
		"a reply in a direct":      {chatlet.KindDM, "msg-0", "chat:room-1:msg-0"},
		"a room post":              {chatlet.KindPublic, "", "chat:room-1:msg-1"},
		"a reply in a room":        {chatlet.KindPublic, "msg-0", "chat:room-1:msg-0"},
		"a unit room's own thread": {chatlet.KindUnit, "msg-0", "chat:room-1:msg-0"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			in := trigger(t, "eng", func(n *chatlet.Notify) {
				n.ChannelKind, n.ThreadRoot = tc.kind, tc.thread
			})
			if got := prompts.Partition(in); got != tc.want {
				t.Errorf("%s keys on %q, want %q", name, got, tc.want)
			}
		})
	}
}

// A DIRECT CONVERSATION IS ONE LINE, THREAD OR NO THREAD.
//
// The partition and the identity are two answers, and chat is where they
// part: a reply in a DM keeps its thread's PARTITION (above), while its
// IDENTITY — what the seat's own history is filed under — is the bare room,
// because the identity must be one value across every event in a partition
// and a DM's top-level burst partitions on the room alone. Keyed on the thread
// instead, the answer to "what did we say yesterday" is filed under an address
// the next turn never looks up. In a shared room a thread IS the conversation,
// and the two answers coincide.
func TestADirectConversationIsOneLineThreadOrNoThread(t *testing.T) {
	t.Parallel()
	prompts := notify.NewPrompts(chatlet.NewPrompt())
	for name, tc := range map[string]struct {
		kind   chatlet.Kind
		thread string
		want   string
	}{
		"a direct message":    {chatlet.KindDM, "", "chat:room-1"},
		"a reply in a direct": {chatlet.KindDM, "msg-0", "chat:room-1"},
		"a reply in a group":  {chatlet.KindGroup, "msg-0", "chat:room-1"},
		"a room post":         {chatlet.KindPublic, "", "chat:room-1:msg-1"},
		"a reply in a room":   {chatlet.KindPublic, "msg-0", "chat:room-1:msg-0"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			in := trigger(t, "eng", func(n *chatlet.Notify) {
				n.ChannelKind, n.ThreadRoot = tc.kind, tc.thread
			})
			if got := prompts.Conversation(in); got != tc.want {
				t.Errorf("%s files its history under %q, want %q", name, got, tc.want)
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
	built := chatlet.NewPrompt().Build(trigger(t, "eng", func(*chatlet.Notify) {}), nil)
	for _, tool := range []string{"reply_in_thread", "post_message", "send_dm"} {
		if !strings.Contains(built, tool) {
			t.Errorf("an addressed chat trigger never names %s, so the seat "+
				"has to guess which of its tools answers a message", tool)
		}
	}
}

// A THREAD REPLY IS POINTED AT THE THREAD IT WAS HANDED, NOT SENT TO READ IT.
//
// The turn-start prefetch reads a Chatlet thread as the woken seat and puts it
// in the system prompt under `## The thread so far` — the one copy of the
// thread a turn gets. A trigger that still told the seat the conversation
// "is not" here sent it to `read_channel` for a second copy of what it already
// held: a wasted round on every thread wake, and two copies of one thread in
// one turn, which is what retiring the record's own excerpt removed. A
// TOP-LEVEL message is the other case: it opens no thread, nothing before it
// was handed over, and reading the room is still the seat's only way to it.
func TestAThreadReplyIsPointedAtTheThreadItWasHanded(t *testing.T) {
	t.Parallel()
	prompt := chatlet.NewPrompt()

	reply := trigger(t, "eng", func(n *chatlet.Notify) { n.ThreadRoot = "msg-0" })
	if !prompt.Addressed(reply) {
		t.Fatal("control: a mention in a thread was not read as an ask")
	}
	built := prompt.Build(reply, nil)
	if !strings.Contains(built, "`## The thread so far`") {
		t.Errorf("an addressed thread reply is not pointed at the thread the "+
			"prefetch handed it:\n%s", built)
	}
	for _, sent := range []string{
		"the conversation around it is not",
		"read the room or the thread",
	} {
		if strings.Contains(built, sent) {
			t.Errorf("an addressed thread reply is still told %q, which sends "+
				"it to read a thread it already holds:\n%s", sent, built)
		}
	}

	top := prompt.Build(trigger(t, "eng", func(*chatlet.Notify) {}), nil)
	if strings.Contains(top, "## The thread so far") {
		t.Errorf("a top-level message is pointed at a thread block no "+
			"prefetch renders for it:\n%s", top)
	}
	if !strings.Contains(top, "read the room with `"+chatlet.ReadChannelTool+"`") {
		t.Errorf("a top-level message is not told to read the room it was "+
			"handed nothing of:\n%s", top)
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
	prompt := chatlet.NewPrompt()
	in := trigger(t, "eng", func(n *chatlet.Notify) {
		n.ThreadRoot = "msg-0"
		n.Recipients = []chatlet.Recipient{{Handle: "eng", Reason: chatlet.ReasonFollow}}
	})
	if prompt.Addressed(in) {
		t.Fatal("control: following a thread was read as being asked something")
	}
	built := prompt.Build(in, nil)
	if !strings.Contains(built, "news, not a request") {
		t.Errorf("an unaddressed wake is not told it may stay silent:\n%s", built)
	}
	if strings.Contains(built, "Answer where you were asked") {
		t.Error("an unaddressed wake is told to answer where it was asked, " +
			"which obliges a seat to reply to every remark in every thread it " +
			"follows")
	}
}

// THE SEAT IS TOLD WHY IT WAS WOKEN, IN ITS OWN TERMS, AND ONCE.
//
// The routing reached a conclusion the TEXT cannot always show: nothing in "we
// should ship on Friday" says it arrived because nobody else in the unit was
// listening. The triage teaches how to find an addressee in the words; this is
// what the router already decided.
//
// A DIRECT CONVERSATION AND A MENTION ARE THE SHARED VERDICT'S to state, and
// their rows hold this prompt to adding no line of its own: a "**Why you:**"
// beside "**Addressed to you:**" told the seat one reason twice. Every other
// reason is a line only this domain can write, and it is written once.
//
// THE UNKNOWN REASON IS A ROW because it is the one a rolling upgrade
// guarantees: a newer build routed this seat deliberately, and a prompt that
// said nothing would leave it guessing from the text alone.
func TestThePromptSaysWhyThisSeatWasWoken(t *testing.T) {
	t.Parallel()
	for reason, tc := range map[chatlet.Reason]struct {
		kind chatlet.Kind
		want string
		// own is whether the reason is this prompt's line rather than
		// the shared verdict's.
		own bool
	}{
		chatlet.ReasonDM:              {chatlet.KindDM, "**Addressed to you:** this is a direct message", false},
		chatlet.ReasonMention:         {chatlet.KindUnit, "**Addressed to you:** the message names you", false},
		chatlet.ReasonReplyToOwnRoot:  {chatlet.KindUnit, "thread YOU started", true},
		chatlet.ReasonLeadFallback:    {chatlet.KindUnit, "unit's lead", true},
		chatlet.ReasonReply:           {chatlet.KindUnit, "spoken in this thread", true},
		chatlet.ReasonCollective:      {chatlet.KindUnit, "whole room", true},
		chatlet.ReasonFollow:          {chatlet.KindUnit, "follow this thread", true},
		chatlet.ReasonFollowAll:       {chatlet.KindUnit, "everything said in this room", true},
		chatlet.Reason("newer-build"): {chatlet.KindUnit, "newer build routed this to you", true},
	} {
		t.Run(string(reason), func(t *testing.T) {
			t.Parallel()
			in := trigger(t, "eng", func(n *chatlet.Notify) {
				n.ChannelKind, n.ThreadRoot, n.Lead = tc.kind, "msg-0", "eng"
				n.AuthorKind = chatlet.AuthorHuman
				n.Recipients = []chatlet.Recipient{{Handle: "eng", Reason: reason}}
			})
			built := chatlet.NewPrompt().Build(in, nil)
			if !strings.Contains(built, tc.want) {
				t.Errorf("a wake routed as %q never says %q:\n%s",
					reason, tc.want, built)
			}
			lines := strings.Count(built, "**Why you:**")
			if tc.own && lines != 1 {
				t.Errorf("a wake routed as %q states its reason in %d lines "+
					"of its own, want one:\n%s", reason, lines, built)
			}
			if !tc.own && lines != 0 {
				t.Errorf("a wake routed as %q states the reason its verdict "+
					"already gave a second time:\n%s", reason, built)
			}
		})
	}
}

// THE SENDER IS RESOLVED BY HANDLE, and a person is named as one.
//
// A Chatlet sender IS a seat's own identity, so there is no transport-scoped id
// to look up — [notify.Parties.ByHandle] exists for exactly the first-party
// sources. What the resolution buys is the difference between a bare handle
// and a colleague the agent can act on: a HUMAN replies on their own time and
// cannot be reached by an agent-to-agent ask, so an agent told to treat them
// as a seat goes looking for a tool that will never answer.
func TestTheSenderIsResolvedByHandleAndAPersonIsNamedAsOne(t *testing.T) {
	t.Parallel()
	in := trigger(t, "eng", func(*chatlet.Notify) {})
	parties := chatParties{"jane": {Handle: "jane", Name: "Jane", Human: true}}

	built := chatlet.NewPrompt().Build(in, parties)
	if !strings.Contains(built, "human colleague") {
		t.Errorf("a person's message renders them as an ordinary seat:\n%s",
			built)
	}
	if !strings.Contains(built, "Jane") {
		t.Errorf("the sender is not named:\n%s", built)
	}
}

// AN AUTHOR THE ROSTER DOES NOT NAME IS DESCRIBED BY KIND, never as a bare
// handle an agent would answer as a colleague.
//
// A human author is a person, under the handle of the seat they hold — and
// when the roster no longer names that seat, the kind on the record is what
// still says an agent-to-agent ask reaches nobody there. The roster's own
// answer wins whenever it has one: the same person is named as the person they
// are.
func TestAnAuthorTheRosterDoesNotNameIsDescribedByKind(t *testing.T) {
	t.Parallel()
	in := trigger(t, "eng", func(n *chatlet.Notify) {
		n.Author, n.AuthorKind = "jane", chatlet.AuthorHuman
	})
	built := chatlet.NewPrompt().Build(in, chatParties{})
	if !strings.Contains(built, "a person") {
		t.Errorf("a person's message renders them as a colleague:\n%s", built)
	}
	named := chatlet.NewPrompt().Build(in,
		chatParties{"jane": {Handle: "jane", Name: "Jane", Human: true}})
	if !strings.Contains(named, "human colleague") || !strings.Contains(named, "Jane") {
		t.Errorf("a person is not named as the person the roster says they "+
			"are:\n%s", named)
	}
	// A CREDENTIAL'S LINE, which no write of this build makes, still says
	// nobody there answers an ask.
	in = trigger(t, "eng", func(n *chatlet.Notify) {
		n.Author, n.AuthorKind = "token:ci", chatlet.AuthorOperator
	})
	if built := chatlet.NewPrompt().Build(in, chatParties{}); !strings.Contains(built,
		"a credential") {
		t.Errorf("a credential's line renders it as a colleague:\n%s", built)
	}
}

// A CHATLET MESSAGE IS FENCED UNDER WHO WROTE IT, the roster's seat first and the
// kind the record stated where the roster no longer names them: a person
// whose seat has gone is still a person who wrote this, and the engine's own
// narration is the engine — never the outside party a vendor's unknown id is.
//
// Mutation: drop NewPrompt's Author and a person the roster lost reads as
// external; resolve by kind before the roster and a colleague the record
// called an agent outranks what the roster says she is.
func TestAChatletMessageIsFencedUnderWhoWroteIt(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		author  string
		kind    chatlet.AuthorKind
		parties notify.Parties
		label   string
	}{
		"a colleague the roster names": {"jane", chatlet.AuthorAgent,
			chatParties{"jane": {Handle: "jane", Name: "Jane", Human: true}},
			"trust=colleague authors=human:jane]"},
		"a person the roster lost": {"jane", chatlet.AuthorHuman, chatParties{},
			"trust=colleague authors=human:jane]"},
		"the engine narrating": {"", chatlet.AuthorSystem, chatParties{},
			"trust=colleague authors=system]"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			in := trigger(t, "eng", func(n *chatlet.Notify) {
				n.Author, n.AuthorKind = c.author, c.kind
			})
			in.Body, in.Fence = "the release slipped", provenance.NewBoundary()
			built := notify.Render(chatlet.NewPrompt(), in, c.parties)
			fences := provenance.Fences(built, in.Fence)
			if len(fences) != 1 {
				t.Fatalf("the wake carries %d fences, want the message's:\n%s",
					len(fences), built)
			}
			if opening := strings.SplitN(fences[0], "\n", 2)[0]; !strings.HasSuffix(opening,
				" source=chat_thread "+c.label) {
				t.Errorf("the message is fenced as %q, want %q", opening, c.label)
			}
		})
	}
}

// A PARTIAL BROADCAST SAYS IT IS ONE.
//
// An `@channel` that exceeded the cap reached fewer seats than the room has,
// and it reads exactly like a complete one unless it is said: the seats that
// were woken assume the rest heard it too, and nobody repeats it.
func TestATruncatedBroadcastTellsTheSeatsItReachedFewerThanItNamed(t *testing.T) {
	t.Parallel()
	plain := trigger(t, "eng", func(*chatlet.Notify) {})
	if strings.Contains(chatlet.NewPrompt().Build(plain, nil), "fewer seats") {
		t.Fatal("control: an ordinary message claims its broadcast was cut short")
	}
	cut := trigger(t, "eng", func(n *chatlet.Notify) { n.WakesTruncated = true })
	if !strings.Contains(chatlet.NewPrompt().Build(cut, nil), "fewer seats") {
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
	if got := (chatlet.Prompt{}).Source(); got != chatlet.Source {
		t.Errorf("a zero prompt names source %q, so the registry silently "+
			"drops it and every chat wake renders as a generic notification",
			got)
	}
	if got := notify.NewPrompts(chatlet.NewPrompt()).For(chatlet.Source); got.Source() != chatlet.Source {
		t.Errorf("the registry resolves %q for Chatlet", got.Source())
	}
}
