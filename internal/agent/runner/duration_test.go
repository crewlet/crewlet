package runner_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/extension"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools"
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

// A SUSPENDED PHASE'S CLOCK FOLDS ACROSS THE SUSPEND, exactly as its rounds
// and its tokens do.
//
// A detached coding run parks the executor mid-loop and the phase is re-entered
// later — after a restart, possibly on another node. The re-entry is a fresh
// call into the phase, so its own wall clock starts at zero: without carrying
// the pre-suspend half, `duration_ms` on the ONE record this phase ever
// publishes reports how long it took to COLLECT the answer rather than how
// long the run took. That is the most expensive thing a seat does, reading as
// near-instant on the screen this field exists to fill — while `rounds_used`,
// the tokens and the tool executions on that same record all fold correctly,
// so the duration would be the single number that means something different
// for a sandboxed phase than for every other.
//
// Over the PUBLISHED event and through a real resume, because the arithmetic
// restated in a test is arithmetic the test cannot catch changing.
func TestAResumedPhaseReportsTheWholePhasesDuration(t *testing.T) {
	t.Parallel()
	// Ninety seconds of coding run, as the parked row carries it.
	const priorMS = 90_000
	state := suspendedAfterTwoRounds()
	state.ElapsedMS = priorMS

	prov := &scriptedProvider{execute: []llm.Completion{
		submitCall(t, runner.SubmitWorkTool,
			`{"outcome":"delivered","summary":"shipped it","evidence":"the box did the work"}`),
	}}
	pub := newCapture()
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{
		pub:    pub,
		resume: &runner.Resume{State: state, Answer: "the run succeeded"},
	})
	if _, _, err := r.Resume(context.Background(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	done := completedPhase(t, pub, "execute")
	if done.DurationMS < priorMS {
		t.Errorf("duration_ms = %d, want at least the %d the phase spent before it suspended",
			done.DurationMS, priorMS)
	}
	// And not a wild overshoot — the re-entry is what is added to it.
	if done.DurationMS > priorMS+60_000 {
		t.Errorf("duration_ms = %d, want about %d plus the re-entry", done.DurationMS, priorMS)
	}
}

// suspendingTool is a `run_sandbox` stand-in: it parks the loop with its call
// unanswered, which is what a detached coding run does.
type suspendingTool struct{}

func (suspendingTool) Name() string               { return "run_sandbox" }
func (suspendingTool) Description() string        { return "start a detached coding run" }
func (suspendingTool) Parameters() map[string]any { return nil }
func (suspendingTool) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{Output: "started"}, nil
}
func (suspendingTool) CallDetached(context.Context, *turnctx.Turn, map[string]any) (tools.DetachedResult, error) {
	return tools.DetachedResult{Result: tools.Result{Output: "started"}, Suspend: true}, nil
}

// THE SUSPEND IS WHAT WRITES THE CLOCK, and the write is half the fold.
//
// The case above injects a state that already carries the offset, so it proves
// the READ. Nothing there would notice if the suspension stopped recording it:
// every parked row would go out at zero, every resumed phase would measure only
// its re-entry, and the read-side test would stay green because it supplies its
// own number. This drives the suspension itself and reads the row the engine
// would persist.
func TestASuspensionRecordsTheClockItParkedOn(t *testing.T) {
	t.Parallel()
	// Each scripted round waits, so the phase has a measurable span before it
	// parks — a floor the assertion can name.
	prov := &slowProvider{delay: roundDelay, next: []llm.Completion{
		// The executor's starting surface carries no sandbox tool, so the
		// round that unlocks it is part of the phase's clock too.
		activate("run_sandbox"),
		submitCall(t, "run_sandbox", `{"task":"fix the failing test"}`),
	}}
	r, reg := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{})
	if err := reg.RegisterWith(suspendingTool{}, tools.Origin("sandbox"), tools.Annotations{}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	w, _, err := r.Execute(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !w.Suspended {
		t.Fatal("the phase did not suspend, so there is no parked row to read")
	}
	parked, ok := r.Suspended()
	if !ok {
		t.Fatal("no suspension was recorded, so the engine has nothing to persist")
	}
	if parked.State.ElapsedMS <= 0 {
		t.Fatalf("the parked row recorded elapsed_ms = %d, so every resume of it "+
			"would report only its re-entry", parked.State.ElapsedMS)
	}
	// It is THIS phase's clock, not the process's: bounded below by the round
	// the provider made it wait for and above by anything sane.
	if got := time.Duration(parked.State.ElapsedMS) * time.Millisecond; got < roundDelay {
		t.Errorf("parked clock = %v, want at least the %v the round took", got, roundDelay)
	}
}

// A PARKED ROW FROM BEFORE THE FIELD EXISTED RESUMES AS IT ALWAYS DID.
//
// The state is persisted to a pending-run row and read back by a build that
// may not be the one that wrote it, so the fold must be additive: an absent
// `elapsed_ms` decodes to zero, seeds no offset, and measures the re-entry
// alone — which is exactly right for a phase whose first half was never
// measured.
func TestAResumeWithNoRecordedClockMeasuresTheReentry(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{execute: []llm.Completion{
		submitCall(t, runner.SubmitWorkTool,
			`{"outcome":"delivered","summary":"shipped it","evidence":"the box did the work"}`),
	}}
	pub := newCapture()
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{
		pub: pub,
		// suspendedAfterTwoRounds carries no ElapsedMS, like every row
		// written before the field existed.
		resume: &runner.Resume{State: suspendedAfterTwoRounds(), Answer: "the run succeeded"},
	})
	if _, _, err := r.Resume(context.Background(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if done := completedPhase(t, pub, "execute"); done.DurationMS > 60_000 {
		t.Errorf("duration_ms = %d on a row carrying no prior clock, want only the re-entry",
			done.DurationMS)
	}
}

// THE FOLD SURVIVES THE WIRE, and is omitted when there is nothing to fold.
//
// The offset travels through JSON in a pending-run row, so a field serialized
// under a name the reader does not know would silently reinstate the bug above
// with every test here still green.
func TestTheSuspendedClockSurvivesTheWire(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(execstate.State{Version: execstate.Version, ElapsedMS: 1234})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"elapsed_ms":1234`) {
		t.Fatalf("state serialized as %s, with no elapsed_ms", raw)
	}
	var back execstate.State
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.ElapsedMS != 1234 {
		t.Errorf("elapsed_ms came back %d, want 1234", back.ElapsedMS)
	}

	bare, err := json.Marshal(execstate.State{Version: execstate.Version})
	if err != nil {
		t.Fatalf("marshal bare: %v", err)
	}
	if strings.Contains(string(bare), "elapsed_ms") {
		t.Errorf("a zero elapsed_ms was written: %s", bare)
	}
}
