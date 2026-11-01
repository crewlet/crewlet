package toolloop_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
)

// The budget: every request's tokens charged as the harness's record shows them, a
// refusal deciding what FOLLOWS — no further call runs and no further request
// is made — and a refusal already certain making no run at all.

// weekTurnsOver is when the refusing week of the budget cases ends.
var weekTurnsOver = time.Date(2026, time.September, 28, 0, 0, 0, 0, time.UTC)

// weekMeter records every charge and can refuse or fail. A refused charge is
// recorded too, as the fleet's counter records one: the meter is handed tokens
// already spent. And it holds what it has seen as the engine's meter does:
// once nothing more fits under refuseAt, every later charge is certain to be
// refused, and Refused says so before one is made.
type weekMeter struct {
	mu       sync.Mutex
	spent    int
	charges  []int
	refuseAt int // refuse once cumulative spend would exceed this; 0 never
	err      error
}

func (m *weekMeter) refusal() toolloop.SpendOutcome {
	return toolloop.SpendOutcome{
		Scope: "role", Used: m.spent, Limit: m.refuseAt,
		Period: period.Week, Window: "2026-W39", ResetsAt: weekTurnsOver,
	}
}

func (m *weekMeter) Refused(context.Context) (toolloop.SpendOutcome, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.refuseAt == 0 || m.spent < m.refuseAt {
		return toolloop.SpendOutcome{}, false
	}
	return m.refusal(), true
}

func (m *weekMeter) Spend(_ context.Context, tokens int) (toolloop.SpendOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return toolloop.SpendOutcome{}, m.err
	}
	fits := m.refuseAt == 0 || m.spent+tokens <= m.refuseAt
	m.spent += tokens
	m.charges = append(m.charges, tokens)
	if !fits {
		return m.refusal(), nil
	}
	return toolloop.SpendOutcome{OK: true}, nil
}

func (m *weekMeter) total() (int, []int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.spent, slices.Clone(m.charges)
}

// A REFUSED SPEND NAMES ITS SCOPE AND ITS WINDOW AND STOPS THE LOOP — and the
// refused request's tools never run, since tools are where the irreversible
// things happen and charging after them makes the cap advisory. The refused
// request was handed to the meter WHOLE, input and output, both billed before
// anything judged them.
func TestARefusedSpendNamesItsScopeAndStopsTheLoop(t *testing.T) {
	t.Parallel()
	surf := newSurface("send_email")
	m := &weekMeter{refuseAt: 100}
	prov := script(runOf(
		llmtest.Response{Calls: []llmtest.Call{{Name: "send_email"}}, Usage: llm.Usage{Input: 60, Output: 60}},
		says("a request made after the refusal"),
	))
	_, err := run(t, toolloop.Config{Provider: prov, Surface: surf, MaxRounds: 5, Budget: m})
	var be *toolloop.BudgetError
	if !errors.As(err, &be) || !errors.Is(err, toolloop.ErrBudgetExhausted) {
		t.Fatalf("err = %v, want a *BudgetError carrying the refusing scope", err)
	}
	if be.Scope != "role" {
		t.Errorf("scope = %q, want role — the refusal did not name itself", be.Scope)
	}
	if spent, _ := m.total(); spent != 120 || be.Used != 120 || be.Limit != 100 {
		t.Errorf("meter holds %d and the refusal says %d/%d, want the whole 120-token request "+
			"counted against the ceiling of 100", spent, be.Used, be.Limit)
	}
	if be.Period != period.Week || be.Window != "2026-W39" || !be.ResetsAt.Equal(weekTurnsOver) {
		t.Errorf("window = %s %q resets %v, want the week 2026-W39 resetting %v",
			be.Period, be.Window, be.ResetsAt, weekTurnsOver)
	}
	if !strings.Contains(err.Error(), "2026-W39") || !strings.Contains(err.Error(), "2026-09-28T00:00:00Z") {
		t.Errorf("error %q does not name the window and when it resets", err)
	}
	if got := surf.calls(); len(got) != 0 {
		t.Errorf("a refused request ran its tools: %v", got)
	}
	// The call the refused request made is answered only once the harness is
	// stopped: answered at once, it is the harness's cue to ask its model again.
	if late := prov.Late(); late != 0 {
		t.Errorf("the harness asked its model %d time(s) after the refusal", late)
	}
}

// A REFUSAL THE METER ALREADY HOLDS MAKES NO RUN — the first included. Started,
// the run would be billed by the vendor and then refused; stopped before it,
// it is refused with the same error, the meter is charged nothing more, and a
// note waiting for it is left in the box for whoever reads it next.
func TestARefusalTheMeterHoldsMakesNoRun(t *testing.T) {
	t.Parallel()
	log := &stepLog{}
	box := &stepBox{log: log}
	box.offer("n-1", "use staging")
	m := &weekMeter{refuseAt: 100, spent: 130}
	prov := script()
	_, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface(), MaxRounds: 5,
		Budget: m, Steer: box})
	var be *toolloop.BudgetError
	if !errors.As(err, &be) || be.Scope != "role" || be.Used != 130 || be.Limit != 100 ||
		be.Window != "2026-W39" || !be.ResetsAt.Equal(weekTurnsOver) {
		t.Fatalf("err = %v, want the refusal the meter holds, window and all", err)
	}
	if n := len(prov.Seen()); n != 0 {
		t.Errorf("the harness was started %d times on a refusal the meter already held", n)
	}
	if spent, _ := m.total(); spent != 130 {
		t.Errorf("the meter was charged to %d by a run that should never have started", spent)
	}
	if len(log.all()) != 0 {
		t.Errorf("the box was drained (%v) by a run that never started", log.all())
	}
}

// A REFUSAL LANDING DURING A CALL STOPS THE NEXT REQUEST. The request was
// admitted and its tool ran, and the tool's own charge — a worker refused by
// the seat — took the window past its ceiling: the call's answer is withheld,
// so the harness asks its model nothing more.
func TestARefusalLandingDuringACallStopsTheNextRequest(t *testing.T) {
	t.Parallel()
	m := &weekMeter{refuseAt: 100}
	surf := newSurface("delegate")
	surf.hook = func(ctx context.Context, _ llm.ToolCall) { _, _ = m.Spend(ctx, 80) }
	prov := script(runOf(
		llmtest.Response{Calls: []llmtest.Call{{Name: "delegate"}}, Usage: llm.Usage{Input: 40}},
		says("never asked"),
	))
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, MaxRounds: 5, Budget: m})
	if !errors.Is(err, toolloop.ErrBudgetExhausted) {
		t.Fatalf("err = %v (result %+v), want the refusal the tool's charge left standing", err, res)
	}
	if late := prov.Late(); late != 0 {
		t.Errorf("the harness asked its model %d time(s) after the refusal", late)
	}
	if got := surf.calls(); len(got) != 1 {
		t.Errorf("tools ran %v, want the admitted request's one call", got)
	}
}

// A REFUSAL MID-RUN LETS A CALL IN FLIGHT FINISH, AND RECORDS IT. A harness running
// a response's calls at once has them both out when one of them takes the
// window past its ceiling: the other is a tool already writing, which the
// refusal does not cut short — it runs out under the phase, its result is on
// the failure record, and the harness is answered neither.
func TestARefusalMidRunRecordsTheCallStillInFlight(t *testing.T) {
	t.Parallel()
	// Room for the response's own 120 tokens, and not for the tool's 200.
	m := &weekMeter{refuseAt: 200}
	surf := newSurface("charge", "slow")
	surf.holdTool, surf.hold, surf.entered = "slow", make(chan struct{}), make(chan struct{})
	charged := make(chan struct{})
	surf.hook = func(ctx context.Context, call llm.ToolCall) {
		if call.Name == "charge" {
			<-surf.entered
			_, _ = m.Spend(ctx, 200)
			close(charged)
		}
	}
	go func() {
		<-charged
		// Let the refusal be read before the slow call returns.
		time.Sleep(20 * time.Millisecond)
		close(surf.hold)
	}()
	prog := &toolloop.Progress{}
	prov := script(runOf(
		llmtest.Response{Parallel: true, Calls: []llmtest.Call{{Name: "slow"}, {Name: "charge"}}},
		says("never asked"),
	))
	_, err := run(t, toolloop.Config{Provider: prov, Surface: surf, MaxRounds: 5, Budget: m, Progress: prog})
	if !errors.Is(err, toolloop.ErrBudgetExhausted) {
		t.Fatalf("err = %v, want the refusal", err)
	}
	if late := prov.Late(); late != 0 {
		t.Errorf("the harness asked its model %d time(s) after the refusal", late)
	}
	snap := prog.Snapshot()
	byName := map[string]toolloop.Execution{}
	for _, e := range snap.Executions {
		byName[e.Name] = e
	}
	if byName["slow"].Output != "slow ok" || byName["charge"].Output != "charge ok" {
		t.Errorf("the failure record holds %+v, want both calls' real results", snap.Executions)
	}
	for _, r := range prov.Results() {
		if r.Output == "slow ok" || r.Output == "charge ok" {
			t.Errorf("the harness read %+v after the refusal", r)
		}
	}
}

// AN UNREACHABLE COUNTER IS NOT A REFUSAL. Different answers with opposite
// consequences: treating unreachable as refused stops every turn in the company
// on a store blip; treating refused as unreachable spends past the cap.
func TestAnUnreachableCounterIsNotARefusal(t *testing.T) {
	t.Parallel()
	prov := script(runOf(llmtest.Response{Text: "hi", Usage: llm.Usage{Input: 10}}))
	_, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface(), MaxRounds: 2,
		Budget: &weekMeter{err: errors.New("store unreachable")}})
	if err == nil {
		t.Fatal("an unreachable counter was ignored")
	}
	if errors.Is(err, toolloop.ErrBudgetExhausted) {
		t.Error("an unreachable counter was reported as a budget refusal")
	}
}

// A REFUSED CHARGE LEAVES ITS ROUND ON THE FAILURE RECORD. The vendor billed the
// request before the meter refused it, and the refusal is what the failure
// record exists to explain — so the round it refused is on it, tokens and all.
func TestARefusedChargeLeavesItsRoundOnTheFailureRecord(t *testing.T) {
	t.Parallel()
	prov := script(runOf(
		llmtest.Response{Text: "one", Calls: []llmtest.Call{{Name: "read"}}, Usage: llm.Usage{Input: 100, Output: 10}},
		llmtest.Response{Text: "two", Usage: llm.Usage{Input: 500, Output: 50, CacheRead: 400}},
	))
	prog := &toolloop.Progress{}
	_, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("read"), MaxRounds: 4,
		Budget: &weekMeter{refuseAt: 200}, Progress: prog})
	if !errors.Is(err, toolloop.ErrBudgetExhausted) {
		t.Fatalf("err = %v, want the budget refusal", err)
	}
	snap := prog.Snapshot()
	if snap.RoundsUsed != 2 || len(snap.Rounds) != 2 {
		t.Errorf("the failure record states %d rounds (%d timed), want the refused round 2 on it",
			snap.RoundsUsed, len(snap.Rounds))
	}
	if snap.InputTokens != 600 || snap.CacheRead != 400 {
		t.Errorf("the failure record billed %d input / %d cached, want 600 / 400", snap.InputTokens, snap.CacheRead)
	}
}

// AN UNOBSERVED HARNESS IS CHARGED ONCE, AT THE END, FOR THE WHOLE RUN: its record
// shows no request, so the host learns its cost only when it is over — one
// charge of the run's total, on the round the run is, on the run's own record
// and on the phase's.
func TestAnUnobservedHarnessSpendIsChargedOnceAtTheEnd(t *testing.T) {
	t.Parallel()
	m := &weekMeter{}
	prov := &llmtest.Script{Unobserved: true, Runs: []llmtest.Run{runOf(
		calling("", "look"), calling("", "look"), says("all looked"),
	)}}
	res, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("look"), Budget: m})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if spent, charges := m.total(); spent != 360 || !slices.Equal(charges, []int{360}) {
		t.Errorf("the meter was charged %v, want the run's 360 once", charges)
	}
	if res.InputTokens != 300 || res.OutputTokens != 60 {
		t.Errorf("the phase spent %d/%d, want the run's 300/60", res.InputTokens, res.OutputTokens)
	}
	if inv := res.Invocations; len(inv) != 1 || inv[0].InputTokens != 300 || inv[0].OutputTokens != 60 {
		t.Errorf("invocations %+v, want the run's cost on it", inv)
	}
	if r := res.Rounds; len(r) != 1 || r[0].InputTokens != 300 || r[0].OutputTokens != 60 {
		t.Errorf("rounds %+v, want one carrying the run's cost", r)
	}
}

// AN UNOBSERVED RUN'S CHARGE, REFUSED AT THE END, STOPS THE PHASE: its tools
// already ran — nothing told the host what they cost until now — and the loop
// starts no further run on a window the run took past its ceiling.
func TestAnUnobservedRunRefusedAtTheEndStopsThePhase(t *testing.T) {
	t.Parallel()
	m := &weekMeter{refuseAt: 200}
	prov := &llmtest.Script{Unobserved: true, Runs: []llmtest.Run{runOf(
		calling("", "look"), calling("", "look"), says(writtenSubmission),
	)}}
	_, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("look", "submit_work"),
		Budget: m, TerminateAfter: []string{"submit_work"}})
	if !errors.Is(err, toolloop.ErrBudgetExhausted) {
		t.Fatalf("err = %v, want the end-of-run refusal", err)
	}
	if n := len(prov.Seen()); n != 1 {
		t.Errorf("%d runs, want none after the refused one", n)
	}
}

// silent is a backend that reports no spend through the host — none per
// request and none at the end — and states its cost only on its answer.
type silent struct{ *llmtest.Script }

type silentHost struct{ llm.ToolHost }

func (silentHost) Spent(llm.Usage) {}

func (s silent) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	req.Tools = silentHost{req.Tools}
	return s.Script.Complete(ctx, req)
}

// A RUN WHOSE BACKEND REPORTED NO SPEND IS CHARGED FROM ITS ANSWER, once — and
// one that reported its spend is not charged again from its answer, which
// states the same tokens a second time.
func TestARunIsChargedFromItsAnswerOnlyWhenNothingWasReported(t *testing.T) {
	t.Parallel()
	t.Run("nothing reported", func(t *testing.T) {
		t.Parallel()
		m := &weekMeter{}
		prov := silent{script(runOf(calling("", "look"), says("done")))}
		res, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("look"), Budget: m})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if _, charges := m.total(); !slices.Equal(charges, []int{240}) {
			t.Errorf("the meter was charged %v, want the answer's 240 once", charges)
		}
		if res.InputTokens != 200 || res.OutputTokens != 40 {
			t.Errorf("the phase spent %d/%d, want the answer's 200/40", res.InputTokens, res.OutputTokens)
		}
	})
	t.Run("reported per request", func(t *testing.T) {
		t.Parallel()
		m := &weekMeter{}
		prov := script(runOf(calling("", "look"), says("done")))
		res, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("look"), Budget: m})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if _, charges := m.total(); !slices.Equal(charges, []int{120, 120}) {
			t.Errorf("the meter was charged %v, want each request's 120 and nothing again", charges)
		}
		if res.InputTokens != 200 || res.OutputTokens != 40 {
			t.Errorf("the phase spent %d/%d, want 200/40", res.InputTokens, res.OutputTokens)
		}
	})
}

// A REQUEST THE METER PAID FOR RUNS ALL ITS CALLS, even when its own charge
// filled the window exactly: the refusal that is now certain refuses the
// request the harness would make NEXT, as their answers go back — not the calls of
// one already paid for, which is where the irreversible things it decided
// happen. And a submission among them still finishes the phase, since nothing
// follows it for the refusal to refuse.
func TestARequestThatFillsTheWindowRunsItsCallsAndASubmissionStands(t *testing.T) {
	t.Parallel()
	t.Run("ordinary calls run, the next request is refused", func(t *testing.T) {
		t.Parallel()
		m := &weekMeter{refuseAt: 100}
		surf := newSurface("post", "note")
		prov := script(runOf(
			llmtest.Response{Parallel: true, Calls: []llmtest.Call{{Name: "post"}, {Name: "note"}},
				Usage: llm.Usage{Input: 100}},
			says("never asked"),
		))
		_, err := run(t, toolloop.Config{Provider: prov, Surface: surf, MaxRounds: 5, Budget: m})
		if !errors.Is(err, toolloop.ErrBudgetExhausted) {
			t.Fatalf("err = %v, want the refusal of the request after the window filled", err)
		}
		if got := surf.calls(); len(got) != 2 {
			t.Errorf("tools ran %v, want both calls of the request the meter paid for", got)
		}
		if late := prov.Late(); late != 0 {
			t.Errorf("the harness asked its model %d time(s) after the refusal", late)
		}
	})
	t.Run("a submission finishes the phase", func(t *testing.T) {
		t.Parallel()
		m := &weekMeter{refuseAt: 100}
		surf := newSurface("submit_work")
		prov := script(runOf(
			llmtest.Response{Calls: []llmtest.Call{{Name: "submit_work"}}, Usage: llm.Usage{Input: 100}},
			says("never asked"),
		))
		res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, MaxRounds: 5, Budget: m,
			TerminateAfter: []string{"submit_work"}})
		if err != nil {
			t.Fatalf("Run: %v — the submission the meter paid for was refused", err)
		}
		if res.ExhaustedRounds || len(res.Executions) != 1 || res.Executions[0].Failed {
			t.Errorf("result %+v, want the submission run and the phase finished", res)
		}
	})
}

// A WINDOW THE LAST ROUND FILLED IS A PHASE OUT OF ROUNDS, NOT ONE THE BUDGET
// REFUSED: no request is about to be made, so there is nothing for the budget
// to refuse — and the caller's extension judge, not a budget error, is what
// decides whether the phase goes on.
func TestAWindowTheLastRoundFilledIsExhaustion(t *testing.T) {
	t.Parallel()
	m := &weekMeter{refuseAt: 100}
	surf := newSurface("post")
	prov := script(runOf(
		llmtest.Response{Calls: []llmtest.Call{{Name: "post"}}, Usage: llm.Usage{Input: 100}},
		says("never asked"),
	))
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, MaxRounds: 1, Budget: m})
	if err != nil {
		t.Fatalf("Run: %v, want the phase out of rounds", err)
	}
	if !res.ExhaustedRounds {
		t.Errorf("result %+v, want ExhaustedRounds", res)
	}
}
