package prefetch_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
)

// msg is a message with the backend's own id.
func msg(id, who, what string) notify.Message {
	return notify.Message{ID: id, SenderID: who, Text: what}
}

// idsOf is the ids of the messages a block reported as answered.
func idsOf(answered []prefetch.Answered) []string {
	ids := make([]string, 0, len(answered))
	for _, a := range answered {
		ids = append(ids, a.ID)
	}
	return ids
}

// THE BLOCK SAYS WHICH WAITING MESSAGES IT SHOWED THE TURN — and only those.
//
// A message that failed and comes round behind its conversation's newer mail
// is dropped as worked when the newer message's turn was shown it waiting: so
// what counts is exactly what that turn SAW and what was still waiting for an
// answer. The trigger's writer's, after the seat's own last reply, before the
// trigger. Not the seat's own replies, not what it had already answered, not
// the trigger itself, and not anything after it.
func TestTheThreadBlockNamesTheWaitingMessagesItShowed(t *testing.T) {
	t.Parallel()
	source := &threads{messages: []notify.Message{
		msg("100", "U1", "can you look at staging"),
		{ID: "101", Text: "on it", Own: true},
		msg("102", "U1", "it loops on /login"), // M1: failed, comes round later
		msg("103", "U1", "and on /logout"),     // M2: what woke this turn
		msg("104", "U1", "a message written after the turn was woken"),
	}}
	r := threadRequest(t)
	r.Message = "103"
	blocks := fetch(t, prefetch.Sources{Threads: source}, r)
	if got := idsOf(blocks.ThreadContextAnswered); !slices.Equal(got, []string{"102"}) {
		t.Fatalf("answered = %v, want [102]: the one message after the seat's own reply "+
			"and before the trigger", got)
	}

	// NOTHING WITHOUT THE TRIGGER: a read that does not contain it says
	// nothing about which messages came before it.
	r.Message = "999"
	if got := fetch(t, prefetch.Sources{Threads: source}, r).ThreadContextAnswered; len(got) != 0 {
		t.Fatalf("answered = %v with the trigger missing from the read, want none", got)
	}
	r.Message = ""
	if got := fetch(t, prefetch.Sources{Threads: source}, r).ThreadContextAnswered; len(got) != 0 {
		t.Fatalf("answered = %v with no trigger named, want none", got)
	}
	// AND NOTHING FROM A READ THAT STOPPED SHORT.
	r.Message = "103"
	short := &threads{messages: source.messages, stoppedShort: true}
	if got := fetch(t, prefetch.Sources{Threads: short}, r).ThreadContextAnswered; len(got) != 0 {
		t.Fatalf("answered = %v from a read that stopped short, want none", got)
	}
}

// ANOTHER WRITER'S WAITING MESSAGE IS OWED A TURN OF ITS OWN: one turn works
// for one writer (ADR-0109), held to their apps, so a turn A woke never
// records S's message as worked. Dropped when it came back behind A's turn,
// S's message would have been answered only inside a turn that worked for A.
// The interleaved drain M1(A) M2(S) M3(A), with M3 waking A's turn: A's own M1
// is answered by it, and S's M2 still gets S's turn. A message whose sender
// the backend did not name counts for nobody, and nor does anything when the
// trigger's own sender is unnamed.
//
// Mutation: drop the writer check from waitingMessages, and M2 is recorded as
// worked by A's turn.
func TestAnotherWritersWaitingMessageIsOwedItsOwnTurn(t *testing.T) {
	t.Parallel()
	source := &threads{messages: []notify.Message{
		msg("100", "UA", "can you look at staging"),
		{ID: "101", Text: "on it", Own: true},
		msg("102", "UA", "it loops on /login"), // M1(A): failed, comes round later
		msg("103", "US", "and on /logout"),     // M2(S): owed S's own turn
		msg("104", "", "an unnamed sender's"),  // nobody's
		msg("105", "UA", "and on /health"),     // M3(A): what woke this turn
	}}
	r := threadRequest(t)
	r.Message = "105"
	if got := idsOf(fetch(t, prefetch.Sources{Threads: source}, r).ThreadContextAnswered); !slices.Equal(got, []string{"102"}) {
		t.Fatalf("answered = %v, want [102]: A's own waiting message, never S's or an unnamed sender's", got)
	}
	unnamed := &threads{messages: []notify.Message{
		msg("100", "UA", "root"),
		msg("101", "", "waiting"),
		msg("102", "", "the trigger, its sender unnamed"),
	}}
	r.Message = "102"
	if got := fetch(t, prefetch.Sources{Threads: unnamed}, r).ThreadContextAnswered; len(got) != 0 {
		t.Fatalf("answered = %v for a trigger whose sender the backend did not name, want none", got)
	}
}

// A MESSAGE A BOUND DROPPED WAS NEVER SHOWN, so it is never reported as
// answered: a message dropped as worked that the turn did not see is a
// message nobody answers.
func TestAMessageTheBlockDroppedIsNotReportedAsShown(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 3000)
	source := &threads{messages: []notify.Message{
		msg("100", "U1", "root"),
		msg("101", "U1", long), // waiting, but the byte ceiling eats it
		msg("102", "U1", long),
		msg("103", "U1", long),
		msg("104", "U1", "the trigger"),
	}}
	r := threadRequest(t)
	r.Message = "104"
	blocks := fetch(t, prefetch.Sources{Threads: source}, r)
	if !strings.Contains(blocks.ThreadContext, "not shown") {
		t.Fatalf("the ceiling dropped nothing, so this case proves nothing:\n%s", blocks.ThreadContext)
	}
	if slices.Contains(idsOf(blocks.ThreadContextAnswered), "101") {
		t.Fatalf("answered = %v names a message the bound dropped", blocks.ThreadContextAnswered)
	}
	if len(blocks.ThreadContextAnswered) == 0 {
		t.Fatal("no shown waiting message was reported")
	}
}

// A WAITING MESSAGE IS REPORTED AT THE VERSION THE BLOCK SHOWED IT: an edited
// message by the version its reader named, an unedited one by none. The mark
// the dispatch writes from it drops the wake of that very edit and no later
// one, whose words the turn never saw.
//
// Mutation: report the id alone, and the edited message's version is lost.
func TestAWaitingMessageIsReportedAtTheEditTheBlockShowed(t *testing.T) {
	t.Parallel()
	edited := msg("102", "U1", "it loops on /login, and on /health")
	edited.Version = "chat@1:41"
	source := &threads{messages: []notify.Message{
		msg("100", "U1", "can you look at staging"),
		{ID: "101", Text: "on it", Own: true},
		edited,
		msg("103", "U1", "and on /logout"),
		msg("104", "U1", "and now?"), // what woke this turn
	}}
	r := threadRequest(t)
	r.Message = "104"
	got := fetch(t, prefetch.Sources{Threads: source}, r).ThreadContextAnswered
	want := []prefetch.Answered{{ID: "102", Version: "chat@1:41"}, {ID: "103"}}
	if !slices.Equal(got, want) {
		t.Fatalf("answered = %+v, want %+v: each waiting message at the version it was shown at, "+
			"and an unedited one at none", got, want)
	}
}

// carried is msg written for whom.
func carried(id, who, what string, whom *types.WorkingFor) notify.Message {
	m := msg(id, who, what)
	m.WorkingFor = whom
	return m
}

func seatOf(handle string) *types.WorkingFor {
	return &types.WorkingFor{Kind: types.WorkingForSeat, Seat: handle}
}

// A WAITING MESSAGE WRITTEN FOR ANOTHER REQUESTER IS OWED ITS OWN TURN (ADR-0109).
// One agent's messages written for two requesters are two partitions, and a
// turn works for one of them: a waiting message counts as worked through only
// where it was written for exactly whom the turn works for, as the partition
// keys a carrier. Otherwise it would be dropped when it came back behind this
// turn, answered inside a turn held to another requester's apps — R2's request
// worked inside B's own turn, or B's own inside R1's.
//
// Mutation: compare the sender alone, compare carriers only when the turn has
// one, or match a message stored with no carrier to any turn, and a row goes
// red.
func TestAWaitingMessageWrittenForAnotherRequesterIsOwedItsOwnTurn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		turn, message *types.WorkingFor
		marked        bool
	}{
		{"a turn for R1, a message for R1", seatOf("r1"), seatOf("r1"), true},
		{"a turn for R1, a message for R2", seatOf("r1"), seatOf("r2"), false},
		{"a turn for R1, a message for nobody else", seatOf("r1"), nil, false},
		{"a turn for nobody else, a message for R2", nil, seatOf("r2"), false},
		{"a turn for nobody else, a message for nobody else", nil, nil, true},
		{"a turn for nobody else, a message carrying an empty kind", nil, &types.WorkingFor{}, false},
	} {
		source := &threads{messages: []notify.Message{
			msg("100", "U1", "can you look at staging"),
			{ID: "101", Text: "on it", Own: true},
			carried("102", "U1", "it loops on /login", tc.message),
			carried("103", "U1", "and on /logout", tc.turn),
		}}
		r := threadRequest(t)
		r.Message, r.WorkingFor = "103", tc.turn
		got := idsOf(fetch(t, prefetch.Sources{Threads: source}, r).ThreadContextAnswered)
		if want := tc.marked; want != slices.Equal(got, []string{"102"}) || (!want && len(got) != 0) {
			t.Errorf("%s: answered = %v, marked want %v", tc.name, got, want)
		}
	}
}

// A WAITING MESSAGE IS MATCHED TO WHOM THE TURN WORKS FOR, NOT TO ITS TRIGGER'S
// POST. An edit's wake carries the editing turn's carrier while the edited
// message's document keeps its post's: B's trigger here was posted for R1 and
// the turn works for R2, as for such an edit. B's message written for R2 is
// worked through, and the one written for R1 is not.
//
// Mutation: compare against the trigger message's own carrier, and the rows
// swap.
func TestAWaitingMessageIsMatchedToWhomTheTurnWorksForNotToItsTriggersPost(t *testing.T) {
	t.Parallel()
	source := &threads{messages: []notify.Message{
		msg("100", "U1", "can you look at staging"),
		{ID: "101", Text: "on it", Own: true},
		carried("102", "U1", "for r2", seatOf("r2")),
		carried("103", "U1", "for r1", seatOf("r1")),
		carried("104", "U1", "the trigger, posted for r1", seatOf("r1")),
	}}
	r := threadRequest(t)
	r.Message, r.WorkingFor = "104", seatOf("r2")
	if got := idsOf(fetch(t, prefetch.Sources{Threads: source}, r).ThreadContextAnswered); !slices.Equal(got,
		[]string{"102"}) {
		t.Fatalf("answered = %v, want only the message written for r2, whom the turn works for", got)
	}
}

// A THREAD READ THAT CANNOT SAY WHOM MESSAGES WERE WRITTEN FOR MARKS NOTHING:
// it cannot tell one requester's message from another's, so even the trigger
// writer's messages that seem to carry nobody are left to turns of their own.
//
// Mutation: ignore WorkingForUnread, and the waiting message is marked.
func TestAThreadReadThatCannotSayWhomMessagesWereWrittenForMarksNothing(t *testing.T) {
	t.Parallel()
	source := &threads{unread: true, messages: []notify.Message{
		msg("100", "U1", "can you look at staging"),
		{ID: "101", Text: "on it", Own: true},
		msg("102", "U1", "it loops on /login"),
		msg("103", "U1", "and on /logout"),
	}}
	r := threadRequest(t)
	r.Message = "103"
	if got := fetch(t, prefetch.Sources{Threads: source}, r).ThreadContextAnswered; len(got) != 0 {
		t.Fatalf("answered = %v from a read that could not say whom its messages were for, want none", got)
	}
}

// A CREDENTIAL'S WAITING MESSAGE MATCHES ONLY ITS OWN APPS, as the partition
// splits one login's work by the apps it carried: the same login with other
// apps is another partition. And the apps are held to the order a writer
// carries them in, so a turn whose carrier was read off a history row, in the
// order its apps were granted, matches the wake of the same apps, and an app
// this build does not know is dropped on both sides.
//
// Mutation: compare a credential's login alone, or key its apps as they
// arrive, and a row goes red.
func TestACredentialsWaitingMessageMatchesOnlyItsOwnApps(t *testing.T) {
	t.Parallel()
	token := func(apps ...iam.Grant) *types.WorkingFor {
		return &types.WorkingFor{Kind: types.WorkingForCredential, Login: "token:ci", Apps: apps}
	}
	for _, tc := range []struct {
		name          string
		turn, message *types.WorkingFor
		marked        bool
	}{
		{"the same login with other apps", token(iam.GrantWorklet), token(iam.GrantWorklet, iam.GrantDoclet), false},
		{"the same login and apps, granted in another order", token(iam.GrantChatlet, iam.GrantWorklet),
			token(iam.GrantWorklet, iam.GrantChatlet), true},
		{"the same login and apps, one of them unknown to this build", token(iam.GrantWorklet),
			token(iam.GrantWorklet, iam.Grant("telepathy")), true},
	} {
		source := &threads{messages: []notify.Message{
			msg("100", "U1", "root"),
			{ID: "101", Text: "on it", Own: true},
			carried("102", "U1", "waiting", tc.message),
			carried("103", "U1", "the trigger", tc.turn),
		}}
		r := threadRequest(t)
		r.Message, r.WorkingFor = "103", tc.turn
		got := idsOf(fetch(t, prefetch.Sources{Threads: source}, r).ThreadContextAnswered)
		if marked := slices.Equal(got, []string{"102"}); marked != tc.marked {
			t.Errorf("%s: answered = %v, marked want %v", tc.name, got, tc.marked)
		}
	}
}
