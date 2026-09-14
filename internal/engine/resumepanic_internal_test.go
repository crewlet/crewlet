package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A RESUME THAT PANICS IS ABANDONED, NOT STRANDED.
//
// The resume path had no recovery at all. A panic outside the turn loop
// unwound through the coordinator, skipping both its revert and its settle, so
// the run row stayed in resumed while the queue redelivered a completion the
// claim then refused. Abandoned, the coordinator leaves the claim taken and
// settles the delivery, and the seat is put AFK with the cause.
func TestAResumeThatPanicsIsAbandonedAndPutsTheSeatAFK(t *testing.T) {
	t.Parallel()
	p := &pub{}
	e := &Engine{backends: &Backends{Queue: p}}
	run := sandbox.PendingRun{
		TurnID: "t-9", AgentHandle: "ceo", Role: "CEO", AgentID: "a-1",
		TraceID: "0af7651916cd43dd8448eb211c80319c",
	}

	err := e.guardResume(context.Background(), run, func() error {
		panic("the runner's registry was nil")
	})

	if !errors.Is(err, sandbox.ErrResumeAbandoned) {
		t.Fatalf("err = %v, want ErrResumeAbandoned: anything else reverts the "+
			"claim and the retry re-enters the same panic", err)
	}
	var panicked *turn.PanicError
	if !errors.As(err, &panicked) || !strings.Contains(panicked.Stack, "goroutine") {
		t.Errorf("err = %v, want the recovered panic with its stack", err)
	}
	breach := only[*types.TurnGuardBreach](t, p, "turn.guard_breach")
	if breach.Kind != types.GuardUnhandledException {
		t.Errorf("kind = %q, want %q", breach.Kind, types.GuardUnhandledException)
	}
	// Off the run's own row, since this engine has no company to ask.
	if breach.RoleName != "CEO" || breach.Agent != "a-1" || breach.TurnID != "t-9" {
		t.Errorf("breach = %+v, want it addressed to the run's seat and turn", breach)
	}
	if strings.Contains(breach.Detail, "goroutine") {
		t.Errorf("the published detail carries a stack: %q", breach.Detail)
	}
}

// A resume that did not panic is passed through untouched, so the recovery
// cannot turn an ordinary failure into an abandonment.
func TestAResumeThatFailsWithoutPanickingKeepsItsError(t *testing.T) {
	t.Parallel()
	p := &pub{}
	e := &Engine{backends: &Backends{Queue: p}}
	cause := errors.New("provider unreachable")

	err := e.guardResume(context.Background(), sandbox.PendingRun{TurnID: "t-1"},
		func() error { return cause })

	if err != cause { //nolint:errorlint // the identical value is the assertion
		t.Errorf("err = %v, want the resume's own error unchanged", err)
	}
	if errors.Is(err, sandbox.ErrResumeAbandoned) {
		t.Error("an ordinary failure was abandoned, so it will never be retried")
	}
	none[*types.TurnGuardBreach](t, p, "turn.guard_breach")
}

// A NODE WITH NO COMPANY HANDS THE RESUME BACK rather than dereferencing nil.
// It is a routing failure, not a broken turn: a peer with the revision can
// resume it.
func TestAResumeOnANodeWithNoCompanyIsUnavailableHere(t *testing.T) {
	t.Parallel()
	state, err := execstate.Encode(execstate.State{AgentRun: true})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	r := &resumer{engine: &Engine{backends: &Backends{Queue: &pub{}}}}
	err = r.Resume(context.Background(), sandbox.ResumeRequest{Run: sandbox.PendingRun{
		TurnID: "t-2", AgentHandle: "ceo", ExecuteState: state,
	}})
	if !errors.Is(err, sandbox.ErrResumeUnavailable) {
		t.Fatalf("err = %v, want ErrResumeUnavailable so the completion goes back", err)
	}
	// The decode refusal wraps the same sentinel, so the reason is what
	// proves this is the company check and not a state that failed to read.
	if !strings.Contains(err.Error(), "no applied company") {
		t.Errorf("err = %v, want it to name the missing company", err)
	}
}

// A RESUME RUNS UNDER THE EPOCH ITS SEAT WAS FOUND IN.
//
// The resumer resolves the seat and its org from one read of the epoch, and
// resumeTurn used to read the epoch again to build the runner, its registry,
// its meter and its judge. An apply landing between the two resumed a seat of
// one revision with the tools and caps of the next. The epoch now travels on
// the resume: this engine holds a live company, and a resume handed none is
// refused rather than quietly served from a second read.
func TestAResumeIsNotRebuiltFromASecondReadOfTheEpoch(t *testing.T) {
	t.Parallel()
	company, seat := modeCompany(t, "claude-code", true, "")
	e := &Engine{backends: &Backends{Queue: &pub{}}}
	e.epoch.current.Store(company)

	err := e.resumeTurn(context.Background(), resumeInput{
		Run:  sandbox.PendingRun{TurnID: "t-3", AgentHandle: "swe"},
		Turn: &turnctx.Turn{ID: "t-3", Seat: seat, Org: company.Org},
	})
	if !errors.Is(err, sandbox.ErrResumeUnavailable) {
		t.Fatalf("err = %v, want ErrResumeUnavailable: a resume with no pinned "+
			"company must not be rebuilt from a second read of the epoch", err)
	}
	if !strings.Contains(err.Error(), "t-3") {
		t.Errorf("err = %v, want it to name the run", err)
	}
}
