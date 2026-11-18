package runner_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/extension"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
)

// A PHASE CARRIES ITS OWN WALL CLOCK, on the record that reports it.
//
// Pairing `agent_phase_completed` with its `agent_phase_started` needs both
// events in one reader's hands, and three never have both: a dashboard opened
// on a running turn queried before the phase started and then buffers only
// completed envelopes, a NESTED phase (a
// delegate's worker, the round-cap judge) publishes no start, and a phase
// started on one node and completed on another subtracts two unreconciled
// clocks. The engine measures where the clock is, and these cases read the
// PUBLISHED events, since the wire is what consumers see.

// slowProvider is a model that takes a fixed delay over every response, so the
// elapsed time a phase reports is bounded below by something the test chose.
//
// A SLEEP rather than an injected clock: `time.Since` is monotonic, so a
// sleep can only overshoot and the assertion is a floor; a fake clock would
// need a seam through runPhase, the judge and the subagent package.
type slowProvider struct {
	delay time.Duration
	next  []llmtest.Response

	mu sync.Mutex
	n  int
}

func (p *slowProvider) Model() string { return "slow" }

func (p *slowProvider) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	return llmtest.Drive(ctx, req, llmtest.ConnOptions{}, func(*llmtest.Conn) (llmtest.Response, error) {
		select {
		case <-ctx.Done():
			return llmtest.Response{}, ctx.Err()
		case <-time.After(p.delay):
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if len(p.next) == 0 {
			return llmtest.Response{Text: "(no script)"}, nil
		}
		r := p.next[min(p.n, len(p.next)-1)]
		p.n++
		return r, nil
	})
}

// roundDelay is each scripted round's wait: well above the millisecond the
// wire field is quantised to, so rounding down to zero cannot pass.
const roundDelay = 8 * time.Millisecond

func TestACompletedPhaseReportsHowLongItTook(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	prov := &slowProvider{delay: roundDelay, next: []llmtest.Response{
		submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`),
	}}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{pub: pub})

	if _, err := r.Review(context.Background(), 1, workFor("posted"), nil); err != nil {
		t.Fatalf("Review: %v", err)
	}

	done := completedPhase(t, pub, "review")
	if done.DurationMS < int(roundDelay/time.Millisecond) {
		t.Errorf("duration_ms = %d for a phase whose only round slept %s; "+
			"the record carries no measurement of its own",
			done.DurationMS, roundDelay)
	}
}

// AND SO DOES ONE THAT DIED: otherwise a provider that hung four minutes and
// one that refused in fifty milliseconds leave the same record.
func TestAFailedPhaseStillReportsHowLongItTook(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	prov := &refusingProvider{delay: roundDelay}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{pub: pub})

	if _, err := r.Review(context.Background(), 1, workFor("posted"), nil); err == nil {
		t.Fatal("a provider that refuses every call returned no error")
	}

	done := completedPhase(t, pub, "review")
	if !done.Failed {
		t.Fatal("the record of a phase that died is not marked failed")
	}
	if done.DurationMS < int(roundDelay/time.Millisecond) {
		t.Errorf("duration_ms = %d on a failed phase that spent %s in its "+
			"provider before refusing", done.DurationMS, roundDelay)
	}
}

// AN EXTENDED PHASE IS ONE PHASE THAT RAN LONGER, so its duration covers every
// invocation of the loop; measured per invocation, exactly the long phases an
// operator investigates would under-report.
func TestAnExtendedPhaseReportsTheWholeStretch(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	// Three rounds against a two-round cap: the phase exhausts its budget,
	// the judge grants more, and it re-enters the loop.
	prov := &slowProvider{delay: roundDelay, next: []llmtest.Response{
		persist(t, "a", "one"),
		persist(t, "b", "two"),
		markDone(t, "read both"),
	}}
	r := extendableRunner(t, prov, pub, alwaysExtend{})

	onboard(t, r)

	done := completedPhase(t, pub, "onboarding")
	if done.RoundsUsed != 3 {
		t.Fatalf("the record claims %d rounds for a phase that ran 3", done.RoundsUsed)
	}
	// Three rounds' delay: two would pass a measurement of the last
	// invocation alone.
	if want := 3 * int(roundDelay/time.Millisecond); done.DurationMS < want {
		t.Errorf("duration_ms = %d over three rounds of %s; the measurement "+
			"covers only the invocation after the extension", done.DurationMS, roundDelay)
	}
}

// THE JUDGE'S OWN CALL IS MEASURED TOO: the one number that separates a judge
// stalling a phase from one that simply said no, and as a nested phase it
// publishes no start to pair with.
func TestTheRoundCapJudgeReportsWhatItsJudgementCost(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	prov := &slowProvider{next: []llmtest.Response{
		persist(t, "a", "one"),
		persist(t, "b", "two"),
		markDone(t, "read both"),
	}}
	r := extendableRunner(t, prov, pub, slowJudge{delay: roundDelay})

	onboard(t, r)

	judged := completedPhase(t, pub, string(types.PhaseJudge))
	if judged.DurationMS < int(roundDelay/time.Millisecond) {
		t.Errorf("the judge's record says it took %dms after a %s call — a "+
			"slow judge stalling the middle of a phase is invisible",
			judged.DurationMS, roundDelay)
	}
}

// slowJudge grants every request, after taking its time about it.
type slowJudge struct{ delay time.Duration }

func (j slowJudge) Decide(ctx context.Context, _ extension.Request) (extension.Decision, error) {
	select {
	case <-ctx.Done():
		return extension.Decision{}, ctx.Err()
	case <-time.After(j.delay):
	}
	// Asked and Model tell the caller a model was CALLED, which gates the
	// record this test reads.
	return extension.Decision{
		Extend: true, Reason: "still making progress", Asked: true, Model: "slow-judge",
	}, nil
}

// refusingProvider fails every call after taking its time, before it connects,
// like a harness that never reached its tools.
type refusingProvider struct{ delay time.Duration }

func (p *refusingProvider) Model() string { return "refusing" }

func (p *refusingProvider) Complete(ctx context.Context, _ llm.Request) (*llm.Completion, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(p.delay):
	}
	return nil, errors.New("the provider refused")
}
