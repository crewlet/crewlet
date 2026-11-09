package chatlet_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
)

// THE QUIET RULE (ADR-0113), exercised as values: what an agent's message that
// asks nothing wakes, what a message naming a person wakes, and what an answer
// wakes, each through the same Resolve and Route the write path and the parser
// call.

// thread is a reply in a thread `amir` started, in a unit room whose agents
// follow it in full, with `dev` having spoken and `ops` following the thread.
func thread(author chatlet.AuthorKind) chatlet.Routing {
	return chatlet.Routing{
		ChannelKind: chatlet.KindUnit, AuthorKind: author,
		Members: []chatlet.Member{
			{Handle: "amir", FollowAll: true}, {Handle: "dev", FollowAll: true},
			{Handle: "ops", FollowAll: true}, {Handle: "qa", FollowAll: true},
		},
		ThreadRoot: "root-1", RootAuthor: "amir",
		ThreadParticipants: []string{"amir", "dev"},
		Followers:          []string{"ops"},
	}
}

// AN AGENT'S ACKNOWLEDGEMENT WAKES NO AGENT: a reply that asks nothing, names
// nobody and answers nobody's question reaches the thread's author, its
// speakers, its followers and the room's full followers in the room alone. The
// measured loop, two agents thanking each other six times in five minutes,
// was exactly this message waking the other side under `reply_to_own_root`.
func TestAnAgentsAcknowledgementWakesNoAgent(t *testing.T) {
	t.Parallel()
	ack := thread(chatlet.AuthorAgent)
	woken := chatlet.Route(chatResolved(ack), agents().Seat, "dev")
	if len(woken) != 0 {
		t.Fatalf("an agent's acknowledgement woke %v, want nobody", handlesRouted(woken))
	}
	held := chatlet.Route(chatlet.Held(ack, chatResolved(ack)), agents().Seat, "dev")
	if got := handlesRouted(held); !slices.Equal(got, []string{"amir", "ops", "qa"}) {
		t.Errorf("held %v, want the author, the follower and the room's other "+
			"follower, which the record reports", got)
	}

	// IN A DIRECT CONVERSATION TOO: an acknowledgement there obliged the
	// other side to answer it, under `dm`.
	dm := chatlet.Routing{ChannelKind: chatlet.KindDM, AuthorKind: chatlet.AuthorAgent,
		Members: members("amir", "dev")}
	if woken := chatlet.Route(chatResolved(dm), agents().Seat, "dev"); len(woken) != 0 {
		t.Errorf("an agent's direct message that asks nothing woke %v", handlesRouted(woken))
	}

	// AND AN AGENT'S @channel THAT ASKS NOTHING is news nobody stops for.
	room := chatlet.Routing{ChannelKind: chatlet.KindPublic, AuthorKind: chatlet.AuthorAgent,
		Members: members("amir", "dev", "ops"), Collective: true}
	if woken := chatlet.Route(chatResolved(room), agents().Seat, "dev"); len(woken) != 0 {
		t.Errorf("an agent's @channel that asks nothing woke %v", handlesRouted(woken))
	}

	// A NAME STILL WAKES, OBLIGING NOTHING: an agent's acknowledgement
	// naming a colleague reaches it as `named`, so it can end in silence.
	named := thread(chatlet.AuthorAgent)
	named.Mentions = []string{"amir"}
	woken = chatlet.Route(chatResolved(named), agents().Seat, "dev")
	if len(woken) != 1 || woken[0].Handle != "amir" || woken[0].Reason != chatlet.ReasonNamed {
		t.Fatalf("an agent naming amir woke %v, want amir alone, as named", woken)
	}
	if woken[0].Reason.Addressed() {
		t.Error("an agent's name that asks nothing obliges an answer, so the named " +
			"seat must answer an acknowledgement and the loop runs on")
	}
}

// AN AGENT'S ASKING MESSAGE ROUTES AS BEFORE: with `asks`, the standing
// arrangements wake as they always did, under the reasons they always had.
func TestAnAgentsAskingMessageRoutesAsBefore(t *testing.T) {
	t.Parallel()
	asking := thread(chatlet.AuthorAgent)
	asking.Asks = true
	woken := chatlet.Route(chatResolved(asking), agents().Seat, "dev")
	want := map[string]chatlet.Reason{
		"amir": chatlet.ReasonReplyToOwnRoot, "ops": chatlet.ReasonFollow,
		"qa": chatlet.ReasonFollowAll,
	}
	if len(woken) != len(want) {
		t.Fatalf("an asking reply woke %v, want %v", woken, want)
	}
	for _, c := range woken {
		if want[c.Handle] != c.Reason {
			t.Errorf("%s woken as %q, want %q", c.Handle, c.Reason, want[c.Handle])
		}
	}
	if held := chatlet.Held(asking, chatResolved(asking)); len(held) != 0 {
		t.Errorf("an asking message held %v back", held)
	}
	dm := chatlet.Routing{ChannelKind: chatlet.KindDM, AuthorKind: chatlet.AuthorAgent,
		Members: members("amir", "dev"), Asks: true}
	woken = chatlet.Route(chatResolved(dm), agents().Seat, "dev")
	if len(woken) != 1 || woken[0].Reason != chatlet.ReasonDM || !woken[0].Reason.Addressed() {
		t.Errorf("an agent's asking direct message woke %v, want the other side, obliged", woken)
	}
}

// A MESSAGE ADDRESSED TO A PERSON WAKES NO AGENT IT DOES NOT NAME, whoever
// wrote it: a status note for a person in a unit room woke every agent that
// followed the room, each to decide to say nothing.
func TestAMessageAddressedToAPersonWakesNoUnnamedAgent(t *testing.T) {
	t.Parallel()
	company := agents().withPeople("sara")
	for _, author := range []chatlet.AuthorKind{chatlet.AuthorHuman, chatlet.AuthorAgent} {
		note := thread(author)
		note.Asks = author == chatlet.AuthorAgent
		note.Mentions = []string{"sara"}
		if woken := chatlet.Route(chatResolved(note), company.Seat, "jane"); len(woken) != 0 {
			t.Errorf("a %s's note to sara woke %v, want no agent", author, handlesRouted(woken))
		}
		// NAMING AN AGENT BESIDE HER WAKES THAT AGENT ALONE.
		note.Mentions = []string{"sara", "ops"}
		woken := chatlet.Route(chatResolved(note), company.Seat, "jane")
		if got := handlesRouted(woken); !slices.Equal(got, []string{"ops"}) {
			t.Errorf("a %s's note to sara and ops woke %v, want ops alone", author, got)
		}
	}
	// A PERSON WHO NAMES NOBODY STILL WAKES THE ROOM, as before.
	open := thread(chatlet.AuthorHuman)
	if got := handlesRouted(chatlet.Route(chatResolved(open), company.Seat, "jane")); len(got) != 4 {
		t.Errorf("a person's reply naming nobody woke %v, want the thread and the room", got)
	}
}

// AN ANSWER WAKES THE AGENT THAT ASKED, whatever else the message is: a
// reply that asks nothing still reaches the seat whose open question it
// answers, and a seat still owing an answer in the thread hears what is said
// there, while nobody else is woken.
func TestAnAnswerWakesTheAgentThatAsked(t *testing.T) {
	t.Parallel()
	answer := thread(chatlet.AuthorAgent)
	answer.Answers = []string{"amir"}
	answer.Owing = []string{"qa"}
	woken := chatlet.Route(chatResolved(answer), agents().Seat, "dev")
	want := map[string]chatlet.Reason{"amir": chatlet.ReasonAnswer, "qa": chatlet.ReasonOwing}
	if len(woken) != len(want) {
		t.Fatalf("an answer woke %v, want %v", woken, want)
	}
	for _, c := range woken {
		if want[c.Handle] != c.Reason || c.Reason.Addressed() {
			t.Errorf("%s woken as %q (addressed %v), want %q, obliging nothing",
				c.Handle, c.Reason, c.Reason.Addressed(), want[c.Handle])
		}
	}
	// THE ANSWER REACHES ITS ASKER EVEN WHEN IT NAMES SOMEBODY ELSE.
	answer.Mentions = []string{"ops"}
	woken = chatlet.Route(chatResolved(answer), agents().Seat, "dev")
	if reasonOf(woken, "amir") != chatlet.ReasonAnswer || reasonOf(woken, "qa") != "" {
		t.Errorf("an answer naming ops woke %v, want amir answered and qa, not named, quiet", woken)
	}
}

// AN AGENT'S EDIT THAT NAMES SOMEBODY NEW wakes them the way its post would:
// obliged where the message asks, told where it does not.
func TestAnEditNamesUnderTheMessagesOwnAsk(t *testing.T) {
	t.Parallel()
	for asks, want := range map[bool]chatlet.Reason{true: chatlet.ReasonMention, false: chatlet.ReasonNamed} {
		woken, _ := chatlet.ResolveMentions(chatlet.Routing{ChannelKind: chatlet.KindPublic,
			AuthorKind: chatlet.AuthorAgent, Members: members("amir"),
			Mentions: []string{"amir"}, Asks: asks})
		if len(woken) != 1 || woken[0].Reason != want {
			t.Errorf("an edit of a message with asks=%v named amir as %v, want %q", asks, woken, want)
		}
	}
}
