package runner_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/extension"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// A PHASE CARRIES ITS OWN WALL CLOCK, on the record that reports it.
//
// It used to be derivable only by pairing `agent_phase_completed` with the
// `agent_phase_started` that shares its (turn, phase, iteration) key, and
// every consumer did exactly that. The pairing needs BOTH events in one
// reader's hands, and three readers never have both:
//
//   - a dashboard deep-linked into a turn WHILE IT RUNS asked its query before
//     the phase started, and afterwards buffers only completed envelopes — so
//     on the one screen the reconstruction exists for, it contributed nothing
//     and every phase reported no duration at all;
//   - a NESTED phase — a delegate's worker, the round-cap judge — publishes no
//     start by design, so no worker of a fan-out of eight had a duration
//     anywhere and "which one was slow" had no answer on any screen;
//   - a phase started on one node and completed on another subtracts two
//     clocks nothing reconciles.
//
// The engine measures where the clock is and publishes the answer. These cases
// are over the PUBLISHED events rather than any return value, because the wire
// is the only place the defect lived.

// slowProvider answers after a fixed delay, so the elapsed time a phase
// reports is bounded below by something the test chose.
//
// A SLEEP rather than an injected clock: `time.Since` is monotonic, so a
// sleep can only overshoot and the assertion is a floor. A fake clock would
// need a seam through runPhase, the judge and the subagent package to test one
// arithmetic step each of them does not own.
type slowProvider struct {
	delay time.Duration
	next  []llm.Completion
	n     int
}

func (p *slowProvider) Model() string { return "slow" }

func (p *slowProvider) Complete(ctx context.Context, _ llm.Request) (*llm.Completion, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(p.delay):
	}
	if len(p.next) == 0 {
		return &llm.Completion{Content: "(no script)"}, nil
	}
	c := p.next[min(p.n, len(p.next)-1)]
	p.n++
	return &c, nil
}

// The delay each scripted round waits. Comfortably above the millisecond the
// wire field is quantised to, so a rounding-down to zero cannot pass.
const roundDelay = 8 * time.Millisecond

func TestACompletedPhaseReportsHowLongItTook(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	prov := &slowProvider{delay: roundDelay, next: []llm.Completion{
		submitCall(t, runner.SubmitWorkTool,
			`{"outcome":"blocked","summary":"nothing to do","evidence":"no write tool"}`),
	}}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{pub: pub})

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	done := completedPhase(t, pub, "execute")
	if done.DurationMS < int(roundDelay/time.Millisecond) {
		t.Errorf("duration_ms = %d for a phase whose only round slept %s; "+
			"the record carries no measurement of its own",
			done.DurationMS, roundDelay)
	}
}

// AND SO DOES ONE THAT DIED. On a timeout it is the number that says so: a
// phase whose provider hung for four minutes and one refused in fifty
// milliseconds are otherwise the same record.
func TestAFailedPhaseStillReportsHowLongItTook(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	prov := &refusingProvider{delay: roundDelay}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{pub: pub})

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err == nil {
		t.Fatal("a provider that refuses every call returned no error")
	}

	done := completedPhase(t, pub, "execute")
	if !done.Failed {
		t.Fatal("the record of a phase that died is not marked failed")
	}
	if done.DurationMS < int(roundDelay/time.Millisecond) {
		t.Errorf("duration_ms = %d on a failed phase that spent %s in its "+
			"provider before refusing", done.DurationMS, roundDelay)
	}
}

// AN EXTENDED PHASE IS ONE PHASE THAT RAN LONGER, so its duration covers every
// invocation of the loop rather than the last one. Measured per invocation, the
// long phases an operator opens the page to investigate are exactly the ones
// that would under-report.
func TestAnExtendedPhaseReportsTheWholeStretch(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	// Three rounds against a two-round cap: the phase exhausts its budget,
	// the judge grants more, and it re-enters the loop.
	prov := &slowProvider{delay: roundDelay, next: []llm.Completion{
		{ToolCalls: []llm.ToolCall{{ID: "a", Name: "read_file"}}},
		{ToolCalls: []llm.ToolCall{{ID: "b", Name: "read_file"}}},
		submitCall(t, runner.SubmitWorkTool, `{"outcome":"delivered","summary":"read both"}`),
	}}
	r := extendableRunner(t, prov, pub, alwaysExtend{})

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	done := completedPhase(t, pub, "execute")
	if done.RoundsUsed != 3 {
		t.Fatalf("the record claims %d rounds for a phase that ran 3", done.RoundsUsed)
	}
	// Three rounds at the scripted delay. Two would pass an assertion written
	// against the last invocation alone, which is the shape being refused.
	if want := 3 * int(roundDelay/time.Millisecond); done.DurationMS < want {
		t.Errorf("duration_ms = %d over three rounds of %s; the measurement "+
			"covers only the invocation after the extension", done.DurationMS, roundDelay)
	}
}

// THE JUDGE'S OWN CALL IS MEASURED TOO, and it is the one number that
// separates a judge stalling a phase from a judge that simply said no. A
// nested phase publishes no start, so the pairing this replaces could never
// have produced one.
func TestTheRoundCapJudgeReportsWhatItsJudgementCost(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	prov := &slowProvider{next: []llm.Completion{
		{ToolCalls: []llm.ToolCall{{ID: "a", Name: "read_file"}}},
		{ToolCalls: []llm.ToolCall{{ID: "b", Name: "read_file"}}},
		submitCall(t, runner.SubmitWorkTool, `{"outcome":"delivered","summary":"read both"}`),
	}}
	r := extendableRunner(t, prov, pub, slowJudge{delay: roundDelay})

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

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
	// Asked and Model are what tell the caller a model was CALLED, which is
	// what gates the record this test reads.
	return extension.Decision{
		Extend: true, Reason: "still making progress", Asked: true, Model: "slow-judge",
	}, nil
}

// refusingProvider fails every call, after taking its time — the failure path
// that used to publish a record with no measurement on it.
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
