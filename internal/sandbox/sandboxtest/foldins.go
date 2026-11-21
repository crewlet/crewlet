package sandboxtest

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// The cases of a FOLD-IN ([sandbox.FoldIn]): a question the engine asks on the
// row of a running job, whether another writer's request joins its work,
// answered once by somebody other than its asker, and closed with its job.
var foldInCases = []struct {
	name string
	fn   func(*testing.T, sandbox.PendingStore)
}{
	{"AFoldInIsAskedOnlyForItsJobWhileItWorks", testAFoldInIsAskedOnlyForItsJobWhileItWorks},
	{"AJobAsksAFoldInOncePerWakeAndAtMostItsBound", testAJobAsksAFoldInOncePerWakeAndAtMostItsBound},
	{"AFoldInIsAnsweredOnceAndNeverByItsAsker", testAFoldInIsAnsweredOnceAndNeverByItsAsker},
	{"ANewJobClosesTheFoldInsOfTheOldOne", testANewJobClosesTheFoldInsOfTheOldOne},
}

// foldIn is a fold-in for job asking whether wake joins, by ceo, put to the
// run's requester.
func foldIn(job, wake string) sandbox.FoldIn {
	return sandbox.FoldIn{ID: uuid.NewSHA1(uuid.NameSpaceURL, []byte(job+"/"+wake)).String(), Launch: job,
		Wake: wake, Asker: "ceo", AskerKind: iam.ActorHuman, Conversation: "chat:C9",
		Words: "can you also redo the logo?", PutTo: []string{requester}}
}

// A FOLD-IN IS ASKED ONLY FOR ITS JOB WHILE IT WORKS: not of a run with no
// record, nor for a job the record does not name, nor once its completion is
// claimed; never past what the row holds of one, nor put to nobody or to its
// asker; and the store stamps when it took it.
func testAFoldInIsAskedOnlyForItsJobWhileItWorks(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-foldin-window"
	ctx := t.Context()
	if _, err := s.AskFoldIn(ctx, turn, foldIn("job-0", "w-0")); !errors.Is(err, sandbox.ErrFoldInClosed) {
		t.Errorf("a fold-in for a run with no record answered %v, want ErrFoldInClosed", err)
	}
	job := mustLaunched(t, s, run(turn))
	if _, err := s.AskFoldIn(ctx, turn, foldIn("job-9", "w-1")); !errors.Is(err, sandbox.ErrFoldInClosed) {
		t.Errorf("a fold-in for a job the record does not name answered %v, want ErrFoldInClosed", err)
	}
	large := foldIn(job, "w-2")
	large.Words = strings.Repeat("x", sandbox.MaxFoldInBytes)
	if _, err := s.AskFoldIn(ctx, turn, large); !errors.Is(err, sandbox.ErrFoldInTooLarge) {
		t.Errorf("a fold-in past what the row holds of one answered %v, want ErrFoldInTooLarge", err)
	}
	nobody, self, answered := foldIn(job, "w-3"), foldIn(job, "w-4"), foldIn(job, "w-5")
	nobody.PutTo, self.PutTo, answered.Answer = nil, []string{requester, "ceo"}, sandbox.FoldInApprove
	for name, f := range map[string]sandbox.FoldIn{"put to nobody": nobody, "put to its asker": self,
		"answered already": answered} {
		if _, err := s.AskFoldIn(ctx, turn, f); err == nil {
			t.Errorf("a fold-in %s was asked", name)
		}
	}
	got, err := s.AskFoldIn(ctx, turn, foldIn(job, "w-6"))
	if err != nil || got.AskedAt.IsZero() || !got.Open() {
		t.Fatalf("AskFoldIn = %+v, %v; want an open fold-in the store stamped", got, err)
	}
	if held := mustGet(t, s, turn).OpenFoldIns(); len(held) != 1 || held[0].ID != got.ID {
		t.Errorf("the job waits on fold-ins %+v, want the one asked", held)
	}
	if _, won, err := s.ClaimForResume(ctx, turn, sandbox.CompletionTail(job), sandbox.Fence{}); err != nil || !won {
		t.Fatalf("ClaimForResume = %v, %v", won, err)
	}
	if _, err := s.AskFoldIn(ctx, turn, foldIn(job, "w-7")); !errors.Is(err, sandbox.ErrFoldInClosed) {
		t.Errorf("a fold-in for a job whose completion was claimed answered %v, want ErrFoldInClosed", err)
	}
	if open := mustGet(t, s, turn).OpenFoldIns(); len(open) != 0 {
		t.Errorf("a job no longer working still waits on fold-ins %+v", open)
	}
}

// A JOB ASKS A FOLD-IN ONCE PER WAKE, AND AT MOST ITS BOUND: asking again for
// the same wake answers the one recorded, and past [sandbox.MaxJobFoldIns] the
// job is spent.
func testAJobAsksAFoldInOncePerWakeAndAtMostItsBound(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-foldin-bound"
	ctx := t.Context()
	job := mustLaunched(t, s, run(turn))
	first, err := s.AskFoldIn(ctx, turn, foldIn(job, "w-0"))
	if err != nil {
		t.Fatal(err)
	}
	again := foldIn(job, "w-0")
	again.ID = uuid.NewString()
	if got, againErr := s.AskFoldIn(ctx, turn, again); againErr != nil || got.ID != first.ID {
		t.Errorf("asking again for the same wake answered %+v, %v; want the first, %s", got, againErr, first.ID)
	}
	for i := 1; i < sandbox.MaxJobFoldIns; i++ {
		if _, err := s.AskFoldIn(ctx, turn, foldIn(job, "w-"+string(rune('a'+i)))); err != nil {
			t.Fatalf("fold-in %d of %d: %v", i+1, sandbox.MaxJobFoldIns, err)
		}
	}
	if _, err := s.AskFoldIn(ctx, turn, foldIn(job, "w-past")); !errors.Is(err, sandbox.ErrFoldInsSpent) {
		t.Errorf("a fold-in past the job's bound answered %v, want ErrFoldInsSpent", err)
	}
	if n := len(mustGet(t, s, turn).FoldIns); n != sandbox.MaxJobFoldIns {
		t.Errorf("the row holds %d fold-ins, want %d", n, sandbox.MaxJobFoldIns)
	}
}

// A FOLD-IN IS ANSWERED ONCE, AND NEVER BY ITS ASKER: the asker's answer is
// refused, the first other answer is recorded with who gave it and when, and a
// later one is refused naming it.
func testAFoldInIsAnsweredOnceAndNeverByItsAsker(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-foldin-answer"
	ctx := t.Context()
	job := mustLaunched(t, s, run(turn))
	f, err := s.AskFoldIn(ctx, turn, foldIn(job, "w-0"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AnswerFoldIn(ctx, turn, f.ID, sandbox.FoldInAnswered{Answer: sandbox.FoldInApprove,
		By: "ceo", ByKind: iam.ActorHuman}); !errors.Is(err, sandbox.ErrFoldInAsker) {
		t.Errorf("the asker's own answer answered %v, want ErrFoldInAsker", err)
	}
	if _, err = s.AnswerFoldIn(ctx, turn, f.ID, sandbox.FoldInAnswered{Answer: "maybe",
		By: requester}); err == nil {
		t.Error("an answer that is neither approve nor refuse was taken")
	}
	got, err := s.AnswerFoldIn(ctx, turn, f.ID, sandbox.FoldInAnswered{Answer: sandbox.FoldInApprove,
		By: requester, ByKind: iam.ActorHuman})
	if err != nil || got.Answer != sandbox.FoldInApprove || got.AnsweredBy != requester || got.AnsweredAt.IsZero() {
		t.Fatalf("AnswerFoldIn = %+v, %v; want approved by %s, stamped", got, err, requester)
	}
	late, err := s.AnswerFoldIn(ctx, turn, f.ID, sandbox.FoldInAnswered{Answer: sandbox.FoldInRefuse,
		By: "boss", ByKind: iam.ActorHuman})
	if !errors.Is(err, sandbox.ErrFoldInAnswered) || late.AnsweredBy != requester {
		t.Errorf("a second answer answered %+v, %v; want ErrFoldInAnswered naming %s", late, err, requester)
	}
	row := mustGet(t, s, turn)
	if held, ok := row.FoldInFor("w-0"); !ok || held.Answer != sandbox.FoldInApprove || len(row.OpenFoldIns()) != 0 {
		t.Errorf("the row holds %+v (found %v), want the first answer and nothing open", held, ok)
	}
}

// A NEW JOB CLOSES THE FOLD-INS OF THE OLD ONE: its opening clears them, so a
// late answer to one is refused and joins nothing.
func testANewJobClosesTheFoldInsOfTheOldOne(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-foldin-relaunch"
	ctx := t.Context()
	job := mustLaunched(t, s, run(turn))
	f, err := s.AskFoldIn(ctx, turn, foldIn(job, "w-0"))
	if err != nil {
		t.Fatal(err)
	}
	next := run(turn)
	next.Continues = true
	if _, err = s.BeginLaunch(ctx, next, sandbox.Fence{}); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if held := mustGet(t, s, turn).FoldIns; len(held) != 0 {
		t.Errorf("the new job's row holds the old job's fold-ins %+v", held)
	}
	if _, err = s.AnswerFoldIn(ctx, turn, f.ID, sandbox.FoldInAnswered{Answer: sandbox.FoldInApprove,
		By: requester, ByKind: iam.ActorHuman}); !errors.Is(err, sandbox.ErrFoldInClosed) {
		t.Errorf("a late answer to the old job's fold-in answered %v, want ErrFoldInClosed", err)
	}
}
