package sandbox

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
)

// A REPLY ANSWERS ONLY A RUN WORKING FOR WHOM IT WAS WRITTEN FOR (ADR-0077,
// ADR-0109). Every reply here is written by somebody the question was put to,
// so the rows judge the third clause alone. A reply written for nobody else
// always matches: its writer spoke for itself. Otherwise the run's principal
// is read in the order the ceiling composes it (its WorkingFor, else its
// Requester where not its own seat, else its RequesterLogin, else its own
// work), both sides as the ceiling reads them before it asks the directory,
// and compared by kind and by the seat's handle or the credential's login,
// never a credential's apps.
//
// Mutation: drop the clause, compare kinds alone, read Requester before
// WorkingFor, let the run's own work match only nobody, read the carrier off
// any writer, compare a credential's apps, read a no-apps credential as itself,
// or read an unknown kind as nobody, and a row goes red.
func TestAReplyAnswersOnlyARunWorkingForWhomItWasWrittenFor(t *testing.T) {
	t.Parallel()
	asked := time.Unix(1_700_000_000, 0).UTC()
	seat := func(handle string) *types.WorkingFor {
		return &types.WorkingFor{Kind: types.WorkingForSeat, Seat: handle}
	}
	credential := func(login string, apps ...iam.Grant) *types.WorkingFor {
		return &types.WorkingFor{Kind: types.WorkingForCredential, Login: login, Apps: apps}
	}
	stranger := &types.WorkingFor{Kind: types.WorkingForStranger}
	unknown := &types.WorkingFor{Kind: "robot", Seat: "b"}
	a, b := iam.GrantWorklet, iam.GrantDoclet
	requester := func(handle string) func(*PendingRun) {
		return func(r *PendingRun) { r.Requester = handle }
	}
	workingFor := func(w *types.WorkingFor, requester string) func(*PendingRun) {
		return func(r *PendingRun) { r.WorkingFor, r.Requester = w, requester }
	}
	login := func(l string, apps ...iam.Grant) func(*PendingRun) {
		return func(r *PendingRun) { r.RequesterLogin, r.RequesterApps = l, apps }
	}
	ownWork := func(*PendingRun) {}

	cases := []struct {
		name    string
		run     func(*PendingRun)
		carries *types.WorkingFor
		answers bool
	}{
		{"R's run, a reply for nobody else", requester("r"), nil, true},
		{"R's run, a reply for R", requester("r"), seat("r"), true},
		{"R's run, a reply for R2", requester("r"), seat("r2"), false},
		{"a run a colleague's write for R woke, a reply for R", workingFor(seat("r"), "b"), seat("r"), true},
		{"a run a colleague's write for R woke, a reply for that colleague", workingFor(seat("r"), "b"),
			seat("b"), false},
		{"L's run, a reply for L with its apps", login("l", a, b), credential("l", a, b), true},
		{"L's run, a reply for L its writer's node narrowed", login("l", a, b), credential("l", a), true},
		{"L's run, a reply for L2 with the same apps", login("l", a, b), credential("l2", a, b), false},
		{"L's run, a reply for the seat L", login("l", a, b), seat("l"), false},
		{"L's run, a reply for a stranger (a node could not place L)", login("l", a, b), stranger, false},
		{"L's run with no apps, a reply for a stranger", login("l"), stranger, true},
		{"a run for L with no apps, a reply for a stranger", workingFor(credential("l"), ""), stranger, true},
		{"a stranger's run, a reply for L with no apps", workingFor(stranger, ""), credential("l"), true},
		{"R's run, a reply for L with no apps", requester("r"), credential("l"), false},
		{"a stranger's run, a reply for a stranger", workingFor(stranger, ""), stranger, true},
		{"a stranger's run, a reply for R", workingFor(stranger, ""), seat("r"), false},
		{"a run for its own seat, a reply for nobody else", workingFor(seat("swe"), ""), nil, true},
		{"a run for its own seat, a reply for its seat", workingFor(seat("swe"), ""), seat("swe"), true},
		{"the seat's own run, a reply for nobody else", ownWork, nil, true},
		{"the seat's own run, a reply for its seat", ownWork, seat("swe"), true},
		{"the seat's own run, a reply for R", ownWork, seat("r"), false},
		{"the seat's own run, a reply for a stranger", ownWork, stranger, false},
		{"a stranger's run, a reply of a kind this build does not know", workingFor(stranger, ""), unknown, true},
		{"R's run, a reply of a kind this build does not know", requester("r"), unknown, false},
		{"the seat's own run, a reply of a kind this build does not know", ownWork, unknown, false},
		{"a run for its own seat, a reply for R", workingFor(seat("swe"), ""), seat("r"), false},
		{"a run for its own seat, a reply for a stranger", workingFor(seat("swe"), ""), stranger, false},
		{"R's run, a reply for its seat", requester("r"), seat("swe"), false},
		{"a stranger's run, a reply for a seat with no handle", workingFor(stranger, ""), seat(""), true},
		{"R's run, a reply for a seat with no handle", requester("r"), seat(""), false},
		{"a run for a kind this build does not know, B its requester, a reply for B",
			workingFor(unknown, "b"), seat("b"), false},
		{"a run for a kind this build does not know, a reply for a stranger", workingFor(unknown, ""),
			stranger, true},
		{"a run for L with [a], B its requester, a reply for L with [a]",
			workingFor(credential("l", a), "b"), credential("l", a), true},
		{"a run for L with [a], B its requester, a reply for B", workingFor(credential("l", a), "b"),
			seat("b"), false},
	}
	for _, tc := range cases {
		run := PendingRun{TurnID: "t1", AgentHandle: "swe", Status: StatusAwaiting,
			ConversationKey: answerOnTheDM.Identity, PartitionKey: answerOnTheDM.Partition,
			AskedAt: asked, AudienceHandles: []string{"cto"}}
		tc.run(&run)
		reply := Reply{Conv: answerOnTheDM, Events: []*events.Event{replyAt("use main", asked.Add(time.Second))},
			Writer: "cto", Says: true, WorkingFor: tc.carries}
		if _, got := reply.Best([]PendingRun{run}); got != tc.answers {
			t.Errorf("%s: answers = %v, want %v", tc.name, got, tc.answers)
		}
	}
}

// A REPLY WRITTEN FOR SOMEBODY ELSE LEAVES THE RUN PARKED, through the
// coordinator. The run works for ada; the cto, whom its question was put to,
// replies in a turn that works for omar, and the reply is omar's work, owed a
// turn of its own held to omar's apps: the coordinator answers that it is not
// the run's, and the run still waits on its question. The cto's reply written
// for ada answers it.
//
// Mutation: drop the clause from putTo, and omar's reply answers ada's run.
func TestAReplyWrittenForSomebodyElseLeavesTheRunParked(t *testing.T) {
	t.Parallel()
	rig := parkedOnAQuestionFor(t, &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}, "cto")
	reply := chatReply(answerOnTheDM, "use main", nil)
	reply.Writer = "cto"
	reply.WorkingFor = &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "omar"}
	disposition, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply)
	if err != nil || disposition != AnswerNotMine {
		t.Fatalf("a reply written for omar = %q, %v, want %q: the run works for ada", disposition, err,
			AnswerNotMine)
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting || got.Answer != nil {
		t.Fatalf("run = %q with answer %+v, want it still waiting on its question", got.Status, got.Answer)
	}
	reply.WorkingFor = &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	if disposition, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply); err != nil ||
		disposition != AnswerConsumed {
		t.Fatalf("the cto's reply written for ada = %q, %v, want %q", disposition, err, AnswerConsumed)
	}
}
