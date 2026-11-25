package sandboxtest

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/sandbox"
)

func testALaunchingRunIsActive(t *testing.T, s sandbox.PendingStore) {
	// A launching run is never polled, and is listed anyway: a node that
	// died mid-launch left a box behind, and a row nobody lists is a box
	// nobody reclaims.
	mustBeginLaunch(t, s, run("t1"))

	got, err := s.ListActive(t.Context())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Status != sandbox.StatusLaunching {
		t.Errorf("active = %+v, want the launching run", got)
	}
	seat, err := s.ListActiveForSeat(t.Context(), "swe")
	if err != nil {
		t.Fatalf("list for seat: %v", err)
	}
	if len(seat) != 1 {
		t.Errorf("the seat's own listing missed its launching run: %+v", seat)
	}
}

// A SEAT'S RUNS ARE FOUND BY ITS ID, and by the id alone. The reader that asks
// this way — a mailbox retirement, about to delete the subscriptions every one
// of these runs is reached on — may hold no handle at all, and a store that
// matched on the handle beside the id handed it nothing for such a seat, while
// one that matched on the handle instead handed it a namesake's runs: the same
// handle under a renamed organisation is another seat. Every unfinished status
// is listed, a launching run included, a finished run is gone from this
// listing like every other, and no seat at all is refused rather than answered
// with every row that names none.
func testASeatsRunsAreFoundByItsID(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	seat, namesake := uuid.New(), uuid.New()
	named := func(turnID string, id string) sandbox.PendingRun {
		r := run(turnID)
		r.AgentID = id
		return r
	}
	mustLaunched(t, s, named("running", seat.String()))
	mustLaunched(t, s, named("parked", seat.String()))
	park(t, s, "parked")
	mustBeginLaunch(t, s, named("launching", seat.String()))
	// The same handle on both of these, "swe", as on every run above.
	mustLaunched(t, s, named("namesake", namesake.String()))
	mustLaunched(t, s, named("unnamed", ""))

	turns := func(id uuid.UUID) []string {
		t.Helper()
		got, err := s.ListActiveForSeatID(ctx, id)
		if err != nil {
			t.Fatalf("ListActiveForSeatID(%s): %v", id, err)
		}
		var ids []string
		for _, r := range got {
			ids = append(ids, r.TurnID)
		}
		slices.Sort(ids)
		return ids
	}
	if got, want := turns(seat), []string{"launching", "parked", "running"}; !slices.Equal(got, want) {
		t.Fatalf("the seat's runs by id = %v, want %v and not its namesake's", got, want)
	}
	if got := turns(namesake); !slices.Equal(got, []string{"namesake"}) {
		t.Fatalf("the namesake's runs by id = %v, want only its own", got)
	}
	if _, err := s.ListActiveForSeatID(ctx, uuid.Nil); err == nil {
		t.Fatal("a listing for no seat was answered; it would hand a retirement every run " +
			"whose row names no seat")
	}

	if _, finished, err := end(ctx, s, "running", everyJob(sandbox.Fence{}, sandbox.Active)); err != nil || !finished {
		t.Fatalf("end = %v, %v", finished, err)
	}
	if got, want := turns(seat), []string{"launching", "parked"}; !slices.Equal(got, want) {
		t.Fatalf("after one run finished the seat's runs by id = %v, want %v", got, want)
	}
}

func testExecuteStateRoundTrips(t *testing.T, s sandbox.PendingStore) {
	// THE SUSPENDED CONVERSATION. Everything the tool loop needs to re-enter
	// where it stopped; a lossy round trip here resumes into a conversation
	// that is not the one that was suspended.
	//
	// BYTE FOR BYTE: the store carries the conversation and never reads it,
	// so anything it gives back other than what it was given — a number
	// re-read as a float64, a key re-sorted — is the store editing a turn.
	mustLaunched(t, s, run("t1"))
	got := mustGet(t, s, "t1")
	if string(got.ExecuteState) != suspendedState {
		t.Errorf("execute state = %s\nwant exactly %s", got.ExecuteState, suspendedState)
	}
}

func testActiveIncludesResumed(t *testing.T, s sandbox.PendingStore) {
	// Boot recovery has to SEE a tail that died mid-flight with the previous
	// engine. Nothing else would ever look at that row again, and its paused
	// box would leak for ever.
	mustLaunched(t, s, run("t1"))
	mustClaim(t, s, "t1")
	got, err := s.ListActive(t.Context())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Status != sandbox.StatusResumed {
		t.Errorf("active = %+v; a resumed tail must stay visible to recovery", got)
	}

	// A finished run does not.
	if _, _, err := end(t.Context(), s, "t1", everyJob(sandbox.Fence{}, sandbox.Active)); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if got, _ := s.ListActive(t.Context()); len(got) != 0 {
		t.Errorf("a finished run is still active: %+v", got)
	}
}

func testAnAnswerFindsTheRunThatAsked(t *testing.T, s sandbox.PendingStore) {
	// A seat can have parked more than one question on one thread, and the
	// answer belongs to the most recent — the person is replying to what
	// they were just asked.
	first, second := run("t1"), run("t2")
	second.CreatedAt = base.Add(time.Minute)
	mustLaunched(t, s, first)
	mustLaunched(t, s, second)
	for _, id := range []string{"t1", "t2"} {
		Ask(t, s, id, Question("?"+id, sandbox.AskToRequester))
		if err := s.MarkAwaiting(t.Context(), id,
			sandbox.Clarification{AskedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("park %s: %v", id, err)
		}
	}
	got, ok, err := findAwaiting(t.Context(), s, "swe", answerOnTheDM)
	if err != nil || !ok {
		t.Fatalf("find: ok=%v err=%v", ok, err)
	}
	if got.TurnID != "t2" {
		t.Errorf("matched %s, want the most recently parked question", got.TurnID)
	}
	// And a different seat's conversation is not this seat's.
	if _, ok, _ := findAwaiting(t.Context(), s, "other", answerOnTheDM); ok {
		t.Error("another seat's answer matched this seat's run")
	}
	// THE MATCH IS ON THE CONVERSATION, NOT ON THE BATCH BESIDE IT: a direct
	// message is one conversation however it is threaded, so a TOP-LEVEL
	// reply on the same DM line answers a question asked in a thread on it.
	// Compared on the partition this would miss, which is the same miss that
	// strands a question asked the other way round — see
	// testARunParkedOnATopLevelDMIsAnsweredInItsThread.
	if _, ok, _ := findAwaiting(t.Context(), s, "swe", sandbox.ConversationRef{
		Identity: "chat:D1", Partition: "chat:D1",
	}); !ok {
		t.Error("a top-level reply on the DM line did not answer the question asked on it")
	}
	// And the identity IS carried, so the resume knows where to report.
	if got.ConversationKey != "chat:D1" {
		t.Errorf("the run reports back to %q, want the DM line it was launched from",
			got.ConversationKey)
	}
}

// THE ENGINE'S OWN PROMPT SENDS THE ANSWER WHERE THE PARTITION CANNOT REACH.
//
// A run launched from a TOP-LEVEL direct message parks under the bare DM
// channel, because that is the partition a top-level burst coalesces on — and
// the chat prompt then tells the seat to reply AS A THREAD, so the person's
// answer arrives keyed on that thread. Compared on the partition the two
// strings never meet: the clarification the box is parked waiting for is
// silently never delivered, and the run sits until its pause TTL reaps it.
// Compared on the conversation it arrives, because a direct message is ONE
// conversation however it is threaded.
func testARunParkedOnATopLevelDMIsAnsweredInItsThread(t *testing.T, s sandbox.PendingStore) {
	top := run("t1")
	// What a top-level DM turn writes: the partition IS the bare channel,
	// and so is the conversation.
	top.PartitionKey, top.ConversationKey = "chat:D1", "chat:D1"
	mustLaunched(t, s, top)
	park(t, s, "t1")

	// The person's reply, in the thread the seat was told to open.
	got, ok, err := findAwaiting(t.Context(), s, "swe", sandbox.ConversationRef{
		Identity: "chat:D1", Partition: "chat:D1:root-1",
	})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if !ok || got.TurnID != "t1" {
		t.Fatalf("the threaded reply matched %q (ok=%v); the answer to a question "+
			"asked from a top-level DM never reaches the run that asked it",
			got.TurnID, ok)
	}
}

// TWO QUESTIONS PARKED IN TWO THREADS OF ONE DIRECT MESSAGE ARE TOLD APART BY
// THE THREAD, not by which was asked last.
//
// A direct message is one conversation however it is threaded, which is what
// makes an answer reach the run that asked at all — and it means every run
// parked on that channel shares one identity. So a reply in thread root-1 is
// admitted by both runs, and picking the NEWEST resumes the one waiting in
// root-2: its question spliced with the answer to somebody else's, and the run
// that was actually answered still waiting. Both the arriving reference and
// each row carry the thread; preferring the row whose partition matches is the
// whole fix, and recency is what decides only between runs that agree on it.
func testTwoQuestionsOnOneDMAreToldApartByTheirThreads(t *testing.T, s sandbox.PendingStore) {
	first, second := run("t1"), run("t2")
	// The same DM line, two threads on it — and the one asked EARLIER is
	// the one the reply belongs to, so recency alone gets this wrong.
	first.PartitionKey = "chat:D1:root-1"
	second.PartitionKey = "chat:D1:root-2"
	second.CreatedAt = base.Add(time.Minute)
	mustLaunched(t, s, first)
	mustLaunched(t, s, second)
	park(t, s, "t1")
	park(t, s, "t2")

	got, ok, err := findAwaiting(t.Context(), s, "swe", sandbox.ConversationRef{
		Identity: "chat:D1", Partition: "chat:D1:root-1",
	})
	if err != nil || !ok {
		t.Fatalf("find: ok=%v err=%v", ok, err)
	}
	if got.TurnID != "t1" {
		t.Errorf("a reply in thread root-1 answered %s, the question parked in "+
			"root-2: the answer to one question resumed another run", got.TurnID)
	}
	// AND THE OTHER THREAD'S REPLY REACHES THE OTHER RUN, so what is under
	// test is the pairing rather than a preference for the older row.
	got, ok, err = findAwaiting(t.Context(), s, "swe", sandbox.ConversationRef{
		Identity: "chat:D1", Partition: "chat:D1:root-2",
	})
	if err != nil || !ok || got.TurnID != "t2" {
		t.Errorf("a reply in thread root-2 answered %q (ok=%v err=%v)", got.TurnID, ok, err)
	}
	// AND RECENCY IS STILL THE LAST WORD where the thread cannot decide: a
	// TOP-LEVEL reply on the DM line matches neither thread, and the person
	// is answering what they were just asked.
	got, ok, err = findAwaiting(t.Context(), s, "swe", sandbox.ConversationRef{
		Identity: "chat:D1", Partition: "chat:D1",
	})
	if err != nil || !ok || got.TurnID != "t2" {
		t.Errorf("a top-level reply answered %q (ok=%v err=%v), want the most "+
			"recently parked question", got.TurnID, ok, err)
	}
}

// A run is answered by ITS conversation and no other. Matching on the seat
// alone would hand an unrelated message to whichever run happened to be
// waiting — and that run would treat it as the answer to its question.
func testAnAnswerOnAnotherConversationMatchesNothing(t *testing.T, s sandbox.PendingStore) {
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	if _, ok, _ := findAwaiting(t.Context(), s, "swe", sandbox.ConversationRef{
		Identity: "chat:D2", Partition: "chat:D2:root-9",
	}); ok {
		t.Error("a message on another conversation answered this run's question")
	}
}

func testAnAnswerWithNoConversationMatchesNothing(t *testing.T, s sandbox.PendingStore) {
	// Matching by seat alone would hand an unrelated message to whichever
	// run happened to be waiting — and that run would treat it as the
	// answer to its question.
	mustLaunched(t, s, run("t1"))
	if err := s.MarkAwaiting(t.Context(), "t1",
		sandbox.Clarification{AskedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("park: %v", err)
	}
	if _, ok, _ := findAwaiting(t.Context(), s, "swe",
		sandbox.ConversationRef{}); ok {
		t.Error("a message with no conversation matched a parked run")
	}
	// AND NEITHER DOES A RUN WITH NONE, which is what an A2A wake or a task
	// assignment launches: it stored no conversation any message could ever
	// reproduce, so every reply on every surface would otherwise be its
	// answer.
	keyless := run("t2")
	keyless.PartitionKey, keyless.ConversationKey = "", ""
	keyless.CreatedAt = base.Add(time.Minute)
	mustLaunched(t, s, keyless)
	park(t, s, "t2")
	got, ok, err := findAwaiting(t.Context(), s, "swe", answerOnTheDM)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if ok && got.TurnID == "t2" {
		t.Error("a run launched with no conversation was answered by a chat message")
	}
}

func testListingsAreStable(t *testing.T, s sandbox.PendingStore) {
	// A recovery pass that reordered its work every boot would make two runs
	// of the same failure look like different failures.
	for i, id := range []string{"c", "a", "b"} {
		r := run(id)
		r.CreatedAt = base.Add(time.Duration(i) * time.Second)
		mustLaunched(t, s, r)
	}
	var first []string
	for range 5 {
		got, err := s.ListActive(t.Context())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		var ids []string
		for _, r := range got {
			ids = append(ids, r.TurnID)
		}
		if first == nil {
			first = ids
			continue
		}
		if len(ids) != len(first) {
			t.Fatalf("listing length changed: %v then %v", first, ids)
		}
		for i := range ids {
			if ids[i] != first[i] {
				t.Fatalf("listing order changed: %v then %v", first, ids)
			}
		}
	}
}
