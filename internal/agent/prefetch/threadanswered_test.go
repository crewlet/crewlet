package prefetch_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/notify"
)

// msg is a message with the backend's own id.
func msg(id, who, what string) notify.Message {
	return notify.Message{ID: id, SenderID: who, Text: what}
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
	if got := blocks.ThreadContextAnswered; !slices.Equal(got, []string{"102"}) {
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
	if got := fetch(t, prefetch.Sources{Threads: source}, r).ThreadContextAnswered; !slices.Equal(got, []string{"102"}) {
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
	if slices.Contains(blocks.ThreadContextAnswered, "101") {
		t.Fatalf("answered = %v names a message the bound dropped", blocks.ThreadContextAnswered)
	}
	if len(blocks.ThreadContextAnswered) == 0 {
		t.Fatal("no shown waiting message was reported")
	}
}
