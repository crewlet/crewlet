package chat_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/chat"
)

// THE ROUTING ARITHMETIC, exercised as values.
//
// Every case here is a claim about who a message concerns, and none of them
// needs a broker, a store or a roster to state — which is the whole reason
// [chat.Candidates] is pure over the record and [chat.Route] takes its two
// impure facts as one predicate. A rule that could only be exercised through a
// live company is a rule nobody re-measures.

// testRoster is a company as the routing sees it: every handle is a seat it
// still has, unless it is listed as gone; the handles listed as people are the
// `kind: human` seats.
//
// THE LEAD IS ASKED FOR ONCE, BY THE WRITE PATH, and never by the routing
// arithmetic: the decide resolves a room's unit lead through this seam and
// puts the ANSWER on the record, so every node routing that record later
// uses the lead the company had when somebody spoke rather than the one it
// has now. That is why the cases below state a lead on [chat.Routing]
// directly and only the write-path cases set one here.
type testRoster struct {
	people map[string]bool
	gone   map[string]bool
	lead   map[string]string
}

func (r testRoster) Seat(handle string) (exists, human bool) {
	return !r.gone[handle], r.people[handle]
}

// Lead is the [chat.Roster] half the WRITE path uses: the decide asks it
// once, for the room's own unit, and puts the answer on the record.
func (r testRoster) Lead(unit string) string { return r.lead[unit] }

// agents is a company of nothing but agent seats, which is the shape most of
// these cases need.
func agents() testRoster { return testRoster{} }

// withPeople names the seats that are people.
func (r testRoster) withPeople(handles ...string) testRoster {
	r.people = map[string]bool{}
	for _, h := range handles {
		r.people[h] = true
	}
	return r
}

// withLead names a unit's lead, which only the WRITE path asks for.
func (r testRoster) withLead(unit, handle string) testRoster {
	r.lead = map[string]string{unit: handle}
	return r
}

// withGone names the seats the company no longer has.
func (r testRoster) withGone(handles ...string) testRoster {
	r.gone = map[string]bool{}
	for _, h := range handles {
		r.gone[h] = true
	}
	return r
}

// members is a membership set of plain members.
func members(handles ...string) []chat.Member {
	out := make([]chat.Member, 0, len(handles))
	for _, h := range handles {
		out = append(out, chat.Member{Handle: h})
	}
	return out
}

// reasonOf is the reason one handle was routed under, or empty.
func reasonOf(candidates []chat.Candidate, handle string) chat.Reason {
	for _, c := range candidates {
		if c.Handle == handle {
			return c.Reason
		}
	}
	return ""
}

// handlesRouted is every handle in a candidate set, in order.
func handlesRouted(candidates []chat.Candidate) []string {
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, c.Handle)
	}
	return out
}

// A HANDLE HEARS UNDER THE FIRST REASON THAT NAMES IT, and once.
//
// Somebody who was mentioned in a thread they have spoken in, in a room they
// follow in full, is told they were MENTIONED — the strongest fact and the one
// they will act on. Under any other arrangement one message wakes one seat
// three times, and the two weaker wakes carry no obligation, so a triage
// prompt that read the last one would let the ask through unanswered.
func TestAHandleHearsUnderTheFirstReasonThatNamesIt(t *testing.T) {
	t.Parallel()
	routed, _ := chat.Resolve(chat.Routing{
		ChannelKind: chat.KindPublic,
		AuthorKind:  chat.AuthorHuman,
		Members: []chat.Member{
			{Handle: "eng-lead", FollowAll: true},
			{Handle: "jane"},
		},
		Mentions:           []string{"eng-lead"},
		ThreadRoot:         "root-1",
		RootAuthor:         "jane",
		ThreadParticipants: []string{"eng-lead", "jane"},
		Followers:          []string{"eng-lead"},
	})

	if got := slices.Collect(slices.Values(handlesRouted(routed))); len(got) !=
		len(slices.Compact(slices.Sorted(slices.Values(got)))) {
		t.Fatalf("one message routed %v — a handle appears once, under its "+
			"strongest reason, or a seat is woken as many times as it is "+
			"connected to the room", got)
	}
	if got := reasonOf(routed, "eng-lead"); got != chat.ReasonMention {
		t.Errorf("eng-lead was woken as %q, want %q — being named outranks "+
			"speaking in the thread and following the room, and only the "+
			"strongest reason obliges an answer", got, chat.ReasonMention)
	}
	// AND THE WEAKER FACT IS STILL ROUTED FOR SOMEBODY IT IS THE
	// STRONGEST FACT ABOUT.
	if got := reasonOf(routed, "jane"); got != chat.ReasonReplyToOwnRoot {
		t.Errorf("jane was woken as %q, want %q — she started the thread, "+
			"which is a question put back to her rather than news",
			got, chat.ReasonReplyToOwnRoot)
	}
}

// A COLLECTIVE WAKE IS CAPPED, AND THE RECORD SAYS SO.
//
// One `@channel` must not be able to take a node's entire turn budget
// ([chat.MaxCollectiveRecipients] is exactly that budget), and a room that
// could not see the cut would read a partial broadcast as a complete one —
// which is the failure the flag exists for, arriving at the one place nobody
// is watching.
func TestACollectiveWakeIsCappedAndSaysSo(t *testing.T) {
	t.Parallel()
	room := make([]chat.Member, 0, chat.MaxCollectiveRecipients+8)
	for i := range cap(room) {
		room = append(room, chat.Member{Handle: fmt.Sprintf("seat-%02d", i)})
	}
	routed, truncated := chat.Resolve(chat.Routing{
		ChannelKind: chat.KindPublic, AuthorKind: chat.AuthorHuman,
		Members: room, Collective: true,
	})

	if len(routed) != chat.MaxCollectiveRecipients {
		t.Fatalf("an @channel in a room of %d woke %d, want %d — past the cap "+
			"one broadcast occupies the whole node and every other trigger "+
			"queues behind it", len(room), len(routed),
			chat.MaxCollectiveRecipients)
	}
	if !truncated {
		t.Errorf("the routing reached %d of %d and did not say it was cut — "+
			"the count that would reveal it is the size of the set BEFORE the "+
			"cap, which the record deliberately does not carry",
			len(routed), len(room))
	}
	// AND A ROOM THAT FITS IS NOT REPORTED AS CUT, which is the half a
	// flag set unconditionally would break.
	_, whole := chat.Resolve(chat.Routing{
		ChannelKind: chat.KindPublic, AuthorKind: chat.AuthorHuman,
		Members: room[:chat.MaxCollectiveRecipients], Collective: true,
	})
	if whole {
		t.Errorf("a broadcast that reached everybody it named reported itself " +
			"truncated, which makes the flag unreadable in the case it is for")
	}
}

// THE TOTAL CAP BOUNDS THE ARMS THAT HAVE NO CAP OF THEIR OWN.
//
// A room's full followers have no per-arm limit — [chat.MaxRecipients] is the
// sum the arms were sized against — so a room where a thousand seats set
// `follow_all` is exactly where the total has to bite, and say so.
func TestTheTotalCapBoundsAnArmWithNoCapOfItsOwn(t *testing.T) {
	t.Parallel()
	room := make([]chat.Member, 0, chat.MaxRecipients+16)
	for i := range cap(room) {
		room = append(room, chat.Member{
			Handle: fmt.Sprintf("seat-%03d", i), FollowAll: true,
		})
	}
	routed, truncated := chat.Resolve(chat.Routing{
		ChannelKind: chat.KindUnit, AuthorKind: chat.AuthorAgent, Members: room,
	})

	if len(routed) != chat.MaxRecipients {
		t.Fatalf("a room of %d full followers woke %d, want %d", len(room),
			len(routed), chat.MaxRecipients)
	}
	if !truncated {
		t.Errorf("the routing kept %d of %d followers and did not say so",
			len(routed), len(room))
	}
}

// A SEAT IS NEVER WOKEN BY ITS OWN MESSAGE.
//
// Telling somebody what they just said is a round a model spends on nothing —
// and there is no reason here that wakes its own author, unlike the tracker's
// unblocked notice, which is about somebody ELSE's task.
func TestASeatIsNeverWokenByItsOwnMessage(t *testing.T) {
	t.Parallel()
	routed, _ := chat.Resolve(chat.Routing{
		ChannelKind: chat.KindPublic, AuthorKind: chat.AuthorAgent,
		Members:  members("eng", "ops"),
		Mentions: []string{"eng", "ops"},
	})
	woken := chat.Route(routed, agents().Seat, "eng")

	if slices.Contains(handlesRouted(woken), "eng") {
		t.Fatalf("the author was woken by their own message: %v — a wake runs "+
			"a turn, and a turn spent reading back what it just said is the "+
			"cheapest loop this engine can enter", handlesRouted(woken))
	}
	if !slices.Contains(handlesRouted(woken), "ops") {
		t.Fatalf("dropping the author dropped everybody: %v", handlesRouted(woken))
	}
}

// A PERSON IS ADDRESSABLE AND IS NEVER WOKEN.
//
// A wake runs a TURN, so routing one to a `kind: human` seat spends a model
// call answering on a person's behalf. A person hears through the in-app
// unread and mention feeds, which are reads — and the rule is enforced in the
// routing rather than left to the notification spine, because the record
// carries the routed set, so a person left in it is a wake this package asked
// for and a later layer happens to decline.
func TestAPersonIsAddressableAndNeverWoken(t *testing.T) {
	t.Parallel()
	// THE HARDEST CASE IS A DIRECT CONVERSATION, where nothing addresses
	// its participants more directly and a person is still not woken.
	routed, _ := chat.Resolve(chat.Routing{
		ChannelKind: chat.KindDM, AuthorKind: chat.AuthorAgent,
		Members: members("eng", "sarah"),
	})
	if got := reasonOf(routed, "sarah"); got != chat.ReasonDM {
		t.Fatalf("sarah is a participant of the conversation and the record "+
			"routed her as %q — the record is what a mention feed and a card "+
			"render from, so she belongs in it", got)
	}

	woken := chat.Route(routed, agents().withPeople("sarah").Seat, "eng")
	if len(woken) != 0 {
		t.Errorf("a person was woken: %v — a wake runs a turn, and a turn "+
			"woken for a person answers on their behalf", handlesRouted(woken))
	}
}

// A HANDLE THE COMPANY NO LONGER HAS IS NOT WOKEN.
func TestASeatTheCompanyNoLongerHasIsNotWoken(t *testing.T) {
	t.Parallel()
	routed, _ := chat.Resolve(chat.Routing{
		ChannelKind: chat.KindPublic, AuthorKind: chat.AuthorHuman,
		Members: members("eng", "retired"), Mentions: []string{"eng", "retired"},
	})
	woken := chat.Route(routed, agents().withGone("retired").Seat, "jane")

	if slices.Contains(handlesRouted(woken), "retired") {
		t.Fatalf("a seat the company does not have was woken: %v — the wake "+
			"has nowhere to run, and the record still names them because the "+
			"message did", handlesRouted(woken))
	}
}

// THE LEAD FALLBACK IS NARROW, and every clause of it matters.
//
// It is the answer to "somebody spoke to this unit and nobody was listening",
// so it arises only where there is a lead to fall back to, only when a PERSON
// or an operator is speaking — an agent's post that reached nobody has reached
// exactly who it should — and only when no ordinary candidate survived.
func TestTheLeadFallbackIsNarrow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		routing chat.Routing
		want    string
	}{
		{
			name: "a person in a unit room nobody was listening to",
			routing: chat.Routing{
				ChannelKind: chat.KindUnit, AuthorKind: chat.AuthorHuman,
				Members: members("ops"), Lead: "eng-lead",
			},
			want: "eng-lead",
		},
		{
			name: "an agent's own post reaches nobody, which is correct",
			routing: chat.Routing{
				ChannelKind: chat.KindUnit, AuthorKind: chat.AuthorAgent,
				Members: members("ops"), Lead: "eng-lead",
			},
		},
		{
			name: "a room no unit owns has no lead to fall back to",
			routing: chat.Routing{
				ChannelKind: chat.KindPublic, AuthorKind: chat.AuthorHuman,
				Members: members("ops"), Lead: "eng-lead",
			},
		},
		{
			name: "somebody else was reached, so the fallback stands down",
			routing: chat.Routing{
				ChannelKind: chat.KindUnit, AuthorKind: chat.AuthorHuman,
				Members: []chat.Member{{Handle: "ops", FollowAll: true}},
				Lead:    "eng-lead",
			},
			want: "ops",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			woken := chat.Route(chatResolved(tc.routing), agents().Seat, "jane")
			got := handlesRouted(woken)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("woke %v, want nobody", got)
				}
				return
			}
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("woke %v, want exactly [%s]", got, tc.want)
			}
		})
	}
}

// THE LEAD WHO ALSO SPOKE IN THE THREAD IS WOKEN AS A PARTICIPANT.
//
// The fallback sits among the ADDRESSED reasons in the vocabulary because it
// obliges an answer as much as a mention does — but it ARISES only in the
// absence of every other reason, so the arithmetic offers it last. Offered
// where its precedence sits, a lead who had spoken in the thread would be
// claimed as the fallback and then dropped the moment anybody else was
// reached: the one person the room was addressed to would be the only one not
// woken.
func TestTheLeadWhoSpokeInTheThreadIsWokenAsAParticipant(t *testing.T) {
	t.Parallel()
	woken := chat.Route(chatResolved(chat.Routing{
		ChannelKind: chat.KindUnit, AuthorKind: chat.AuthorHuman,
		Members:            members("eng-lead", "ops"),
		ThreadRoot:         "root-1",
		RootAuthor:         "jane",
		ThreadParticipants: []string{"eng-lead", "ops"},
		Lead:               "eng-lead",
	}), agents().Seat, "jane")

	if got := reasonOf(woken, "eng-lead"); got != chat.ReasonReply {
		t.Fatalf("the lead was woken as %q, want %q — claimed as the fallback "+
			"they are dropped as soon as anybody else is reached, which is "+
			"every busy thread", got, chat.ReasonReply)
	}
	if len(woken) != 2 {
		t.Errorf("the thread woke %v, want both of its speakers",
			handlesRouted(woken))
	}
}

// A MENTION IN A PRIVATE ROOM REACHES ONLY ITS MEMBERS, AND EVERYWHERE ELSE
// IT REACHES WHOM IT NAMED.
//
// A private room's transcript is reachable only through its membership, so a
// wake there would send a seat to open a conversation it cannot read. A public
// or a unit room has nothing to join in that sense, and a mention that woke
// nobody would make naming a colleague in the company's open rooms a gesture
// with no effect at all.
func TestAMentionInAPrivateRoomReachesOnlyItsMembers(t *testing.T) {
	t.Parallel()
	for kind, want := range map[chat.Kind]bool{
		chat.KindPrivate: false,
		chat.KindPublic:  true,
		chat.KindUnit:    true,
	} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			routed, _ := chat.Resolve(chat.Routing{
				ChannelKind: kind, AuthorKind: chat.AuthorHuman,
				Members: members("ops"), Mentions: []string{"outsider"},
			})
			got := slices.Contains(handlesRouted(routed), "outsider")
			if got != want {
				t.Fatalf("a mention of a non-member in a %s room routed them: "+
					"%v, want %v", kind, got, want)
			}
		})
	}
}

// THE WRITE PATH AND THE PARSER ROUTE ONE MESSAGE IDENTICALLY.
//
// The record carries the routed set precisely so the feed never has to
// subtract, and the parser routes AGAIN because a seat may have left between
// the commit and the wake. Both halves run the same arithmetic, so the only
// difference between their answers may ever be the roster — and this is the
// case where the roster has not moved.
func TestTheWritePathAndTheParserRouteOneMessageIdentically(t *testing.T) {
	t.Parallel()
	routing := chat.Routing{
		ChannelKind: chat.KindUnit, AuthorKind: chat.AuthorHuman,
		Members: []chat.Member{
			{Handle: "ops", FollowAll: true},
			{Handle: "eng"},
			{Handle: "sarah"},
		},
		Mentions:           []string{"eng"},
		ThreadRoot:         "root-1",
		RootAuthor:         "sarah",
		ThreadParticipants: []string{"sarah", "eng"},
		Lead:               "eng-lead",
	}
	company := agents().withPeople("sarah")

	atWrite := chat.Route(chatResolved(routing), company.Seat, "jane")
	record := &chat.Notify{
		MessageID: "m-1", ChannelID: "c-1", ChannelKind: routing.ChannelKind,
		Author: "jane", AuthorKind: routing.AuthorKind,
		Lead: routing.Lead, Recipients: chat.RecipientsOf(atWrite),
	}
	atWake := chat.Route(chat.Candidates(record), company.Seat, "jane")

	if !slices.Equal(chat.RecipientsOf(atWrite), chat.RecipientsOf(atWake)) {
		t.Fatalf("the writer routed %v and the parser routed %v from the "+
			"record it wrote — one arithmetic, called twice, cannot answer "+
			"two things about one message",
			chat.RecipientsOf(atWrite), chat.RecipientsOf(atWake))
	}
	if len(atWake) == 0 {
		t.Fatal("both halves routed nobody, so this case asserts nothing")
	}
}

// THE PARSER RE-OFFERS THE LEAD THE RECORD NAMED.
//
// A record whose ordinary recipients have all left the company since it was
// written would otherwise reach nobody at all — which is exactly the state the
// fallback exists for, arriving later than the write.
func TestTheParserFallsBackToTheLeadTheRecordNamed(t *testing.T) {
	t.Parallel()
	record := &chat.Notify{
		MessageID: "m-1", ChannelID: "c-1", ChannelKind: chat.KindUnit,
		Author: "jane", AuthorKind: chat.AuthorHuman, Lead: "eng-lead",
		Recipients: []chat.Recipient{{Handle: "ops", Reason: chat.ReasonFollowAll}},
	}
	woken := chat.Route(chat.Candidates(record), agents().withGone("ops").Seat, "jane")

	if len(woken) != 1 || woken[0].Handle != "eng-lead" {
		t.Fatalf("woke %v, want the lead the record named — every ordinary "+
			"recipient has left the company", handlesRouted(woken))
	}
	if !woken[0].Addressed() {
		t.Errorf("the lead was woken unaddressed — a person spoke to this " +
			"unit and nobody was listening, which is a question put to whoever " +
			"leads it")
	}
}

// A WAKE REASON THIS BUILD DOES NOT KNOW IS STILL A WAKE.
//
// A newer build routed that handle deliberately. Dropping it would make a
// rolling upgrade a period in which some people are simply not told — while
// carrying it UNADDRESSED is the conservative half of the same record: a seat
// may absorb it rather than having to answer a question this build cannot read.
func TestAWakeReasonThisBuildDoesNotKnowIsStillAWake(t *testing.T) {
	t.Parallel()
	record := &chat.Notify{
		MessageID: "m-1", ChannelID: "c-1", ChannelKind: chat.KindPublic,
		Author: "jane", AuthorKind: chat.AuthorHuman,
		Recipients: []chat.Recipient{{Handle: "ops", Reason: chat.Reason("summoned")}},
	}
	woken := chat.Route(chat.Candidates(record), agents().Seat, "jane")

	if len(woken) != 1 || woken[0].Handle != "ops" {
		t.Fatalf("woke %v, want ops — a newer build named them for a reason it "+
			"understood", handlesRouted(woken))
	}
	if woken[0].Addressed() {
		t.Errorf("a reason this build cannot read was treated as an obligation " +
			"to answer, which is a claim nothing here can support")
	}
}

// NOTHING IS ROUTED WITHOUT A ROSTER, AND NOTHING IS ROUTED FOR A QUIET
// RECORD.
//
// Both are fail-safe by construction. A predicate that cannot tell a departed
// seat from a person answers neither question, so waking everybody the record
// named is the one response that cannot be defended; and a record with no
// snapshot — an import, a system line — is one the writer meant to wake nobody
// with, so the parser must reach the same answer without reading a row.
func TestNothingIsRoutedWithoutARosterOrWithoutASnapshot(t *testing.T) {
	t.Parallel()
	routed, _ := chat.Resolve(chat.Routing{
		ChannelKind: chat.KindPublic, AuthorKind: chat.AuthorHuman,
		Members: members("ops"), Mentions: []string{"ops"},
	})
	if woken := chat.Route(routed, nil, "jane"); len(woken) != 0 {
		t.Errorf("a routing with no roster woke %v — it can tell neither a "+
			"seat the company has from one it does not, nor a person from an "+
			"agent", handlesRouted(woken))
	}
	if got := chat.Candidates(nil); got != nil {
		t.Errorf("a record that wakes nobody concerned %v — a nil snapshot is "+
			"what quiet MEANS here, and a parser that read one anyway would "+
			"page a company about a year of imported history", got)
	}
}

// chatResolved is [chat.Resolve] where the truncation is not what is under
// test.
func chatResolved(r chat.Routing) []chat.Candidate {
	candidates, _ := chat.Resolve(r)
	return candidates
}
