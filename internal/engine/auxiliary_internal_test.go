package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
)

// THE AUXILIARY SEAM, held to both of its jobs.
//
// `token_budget` used to be charged in exactly two places — the turn loop and
// the coding sandbox — while every auxiliary completion resolved a model and
// called it directly, so that spend reached no counter; and once it did, it
// still reached no EVENT, so every figure built from events understated the
// counters by exactly it. The cases below are about the seam: a caller that
// resolves its model the ordinary way is charged AND recorded without knowing
// either is happening, which is what makes a caller added later do both.

// countingMeter records what it was asked to record, and when.
type countingMeter struct {
	spent int
	calls int
	at    []time.Time
	err   error
}

func (m *countingMeter) Record(_ context.Context, tokens int, at time.Time) error {
	m.calls++
	m.at = append(m.at, at)
	if m.err != nil {
		return m.err
	}
	m.spent += tokens
	return nil
}

// answeringProvider returns a completion with a known token cost.
type answeringProvider struct {
	in, out    int
	cacheRead  int
	err        error
	completion bool // a completion beside the error, as a billed failure has
	calls      int
}

func (p *answeringProvider) Model() string { return "test-model" }

func (p *answeringProvider) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	p.calls++
	if p.err != nil && !p.completion {
		return nil, p.err
	}
	return &llm.Completion{Model: "test-model", InputTokens: p.in, OutputTokens: p.out,
		CacheRead: p.cacheRead}, p.err
}

// staticHeads is a registry whose every head is one provider, recording the
// phases it was asked for.
type staticHeads struct {
	provider llm.Provider
	asked    *[]phase.Phase
}

func (m staticHeads) Head(_ *org.Role, ph phase.Phase) (chain.Member, error) {
	if m.asked != nil {
		*m.asked = append(*m.asked, ph)
	}
	return chain.Member{Key: "cheap", Provider: m.provider}, nil
}

// capturedEvents is a publisher keeping what the ledger published.
type capturedEvents struct {
	mu  sync.Mutex
	got []*events.Event
}

func (c *capturedEvents) Publish(_ context.Context, _ string, ev *events.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, ev)
	return nil
}

func (c *capturedEvents) records(t *testing.T) []types.AuxiliarySpend {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]types.AuxiliarySpend, 0, len(c.got))
	for _, ev := range c.got {
		rec, ok := events.DataAs[*types.AuxiliarySpend](ev)
		if !ok {
			t.Fatalf("the ledger published a %s", ev.Type)
		}
		out = append(out, *rec)
	}
	return out
}

// dev is the agent seat every seam case runs on.
var dev = &org.Role{Name: "Dev"}

// seamRig is a seam over one provider, with its ledger's publisher and a
// fixed clock.
type seamRig struct {
	seam   auxiliarySeam
	pub    *capturedEvents
	ledger *auxspend.Ledger
	org    *org.Organization
}

func newSeamRig(provider llm.Provider, meter spendRecorder) *seamRig {
	o := &org.Organization{Name: "Acme", Roles: []*org.Role{dev}}
	pub := &capturedEvents{}
	ledger := auxspend.NewLedger(pub)
	r := &seamRig{pub: pub, ledger: ledger, org: o}
	r.seam = auxiliarySeam{
		heads: staticHeads{provider: provider}, org: o, zone: time.UTC, ledger: ledger,
		now: func() time.Time { return time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC) },
	}
	if meter != nil {
		r.seam.charge = func(*org.Role, types.AuxStage) spendRecorder { return meter }
	}
	return r
}

var turnUse = auxspend.Use{Stage: types.AuxStageTurn, Purpose: types.AuxMemoryFilter,
	TurnID: "run-1", WorkKey: "wk-1"}

func (r *seamRig) complete(t *testing.T, role *org.Role, use auxspend.Use) (*llm.Completion, error) {
	t.Helper()
	member, err := r.seam.Auxiliary(role, use)
	if err != nil {
		t.Fatalf("Auxiliary: %v", err)
	}
	return member.Provider.Complete(t.Context(), llm.Request{})
}

func (r *seamRig) flushed(t *testing.T) []types.AuxiliarySpend {
	t.Helper()
	r.ledger.Flush(t.Context())
	return r.pub.records(t)
}

// AN AUXILIARY COMPLETION IS CHARGED AND RECORDED. The whole finding, in one
// case: the counter hears the tokens, and the spend history holds one record
// of the call under exactly the attribution its caller stated — the seat by its
// derived id, the stage, the purpose, the run and its unit of work, the model
// the reply named and the entry that served it, on the company day it was
// charged in.
func TestAnAuxiliaryCompletionIsChargedAndRecorded(t *testing.T) {
	t.Parallel()
	meter := &countingMeter{}
	r := newSeamRig(&answeringProvider{in: 700, out: 300, cacheRead: 200}, meter)
	if _, err := r.complete(t, dev, turnUse); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if meter.spent != 1000 {
		t.Errorf("charged %d tokens, want the completion's 1000", meter.spent)
	}
	recs := r.flushed(t)
	if len(recs) != 1 {
		t.Fatalf("recorded %d records, want one", len(recs))
	}
	id, _ := r.org.AgentIDFor(dev)
	got := recs[0]
	if got.Agent != id.String() || got.AgentHandle != dev.Handle() || got.RoleName != "Dev" ||
		got.Stage != types.AuxStageTurn || got.Purpose != types.AuxMemoryFilter ||
		got.TurnID != "run-1" || got.WorkKey != "wk-1" || got.Model != "test-model" ||
		got.ProviderKey != "cheap" || got.Day != "2026-09-23" || got.Calls != 1 ||
		got.FailedCalls != 0 || got.InputTokens != 700 || got.OutputTokens != 300 ||
		got.TotalTokens != 1000 || got.CacheReadTokens != 200 || got.ActorSeat != "" {
		t.Errorf("recorded %+v", got)
	}
}

// AN ATTRIBUTION THE RECORD CANNOT BE FILED UNDER IS REFUSED BEFORE A MODEL IS
// RESOLVED, so a caller that forgot its purpose spends nothing rather than
// spending under a line no breakdown names.
func TestAnUnattributedCallIsRefusedBeforeAModelIsResolved(t *testing.T) {
	t.Parallel()
	var asked []phase.Phase
	provider := &answeringProvider{in: 1, out: 1}
	r := newSeamRig(provider, nil)
	r.seam.heads = staticHeads{provider: provider, asked: &asked}
	for _, use := range []auxspend.Use{
		{},
		{Stage: types.AuxStageTurn},
		{Purpose: types.AuxMemoryFilter},
		{Stage: types.AuxStageReflection, Purpose: types.AuxPersistDecider, Tally: auxspend.NewTally()},
	} {
		if _, err := r.seam.Auxiliary(dev, use); !errors.Is(err, auxspend.ErrUnattributed) {
			t.Errorf("Auxiliary(%+v) = %v, want ErrUnattributed", use, err)
		}
	}
	if len(asked) != 0 || provider.calls != 0 {
		t.Fatalf("an unattributed call resolved %v and called %d times", asked, provider.calls)
	}
	// And a valid one resolves the AUXILIARY chain, nothing else.
	if _, err := r.seam.Auxiliary(dev, turnUse); err != nil || len(asked) != 1 || asked[0] != phase.Auxiliary {
		t.Fatalf("a valid call resolved %v (%v), want the auxiliary chain", asked, err)
	}
}

// WITH NO COUNTER A CALL IS STILL RECORDED. The counter exists only where a
// coordination store does; a figure a person reads must not depend on one.
func TestWithNoCounterAnAuxiliaryCallIsStillRecorded(t *testing.T) {
	t.Parallel()
	r := newSeamRig(&answeringProvider{in: 5, out: 5}, nil)
	if _, err := r.complete(t, dev, turnUse); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if recs := r.flushed(t); len(recs) != 1 || recs[0].TotalTokens != 10 {
		t.Fatalf("recorded %+v, want the call's 10 tokens", recs)
	}
}

// A FAILED CHARGE DOES NOT FAIL THE COMPLETION, AND DOES NOT LOSE THE RECORD.
// The call already succeeded at the vendor and the caller's work is valid;
// turning a coordination blip into a reflection outage would be the wrong
// trade, and the history still says what was spent.
func TestAnUncountedSpendStillReturnsTheCompletionAndIsRecorded(t *testing.T) {
	t.Parallel()
	meter := &countingMeter{err: errors.New("counter unreachable")}
	r := newSeamRig(&answeringProvider{in: 10, out: 10}, meter)
	got, err := r.complete(t, dev, turnUse)
	if err != nil {
		t.Fatalf("a charge failure was propagated as a completion failure: %v", err)
	}
	if got == nil || got.TotalTokens() != 20 {
		t.Fatalf("completion = %+v, want the provider's answer intact", got)
	}
	if recs := r.flushed(t); len(recs) != 1 || recs[0].TotalTokens != 20 {
		t.Fatalf("recorded %+v, want the 20 tokens the counter did not hear", recs)
	}
}

// A FAILED CALL IS A CALL. One that returned nothing charges nothing — there
// are no tokens to bill — and is recorded as a failed call, so a model that
// times out is visible; one that failed AFTER reporting usage is charged what
// it reported, since a provider bills what it computed.
func TestAFailedCallIsRecordedAndChargedWhatItReported(t *testing.T) {
	t.Parallel()
	meter := &countingMeter{}
	r := newSeamRig(&answeringProvider{err: errors.New("upstream 500")}, meter)
	if _, err := r.complete(t, dev, turnUse); err == nil {
		t.Fatal("the provider error was swallowed")
	}
	if meter.calls != 0 {
		t.Errorf("charged %d times for a call that returned nothing", meter.calls)
	}
	recs := r.flushed(t)
	if len(recs) != 1 || recs[0].Calls != 1 || recs[0].FailedCalls != 1 || recs[0].TotalTokens != 0 {
		t.Fatalf("recorded %+v, want one failed call of no tokens", recs)
	}

	billed := newSeamRig(&answeringProvider{in: 40, out: 2, err: errors.New("cut off"),
		completion: true}, meter)
	if _, err := billed.complete(t, dev, turnUse); err == nil {
		t.Fatal("the provider error was swallowed")
	}
	if meter.spent != 42 {
		t.Errorf("charged %d, want the 42 the failed call reported", meter.spent)
	}
	if recs := billed.flushed(t); len(recs) != 1 || recs[0].FailedCalls != 1 || recs[0].TotalTokens != 42 {
		t.Fatalf("recorded %+v, want one failed call of 42 tokens", recs)
	}
}

// THE TURN'S TALLY HEARS ITS OWN CALLS. A call made with the turn's tally adds
// what it cost there — which is what the turn's work item is charged from
// (ADR-0022) — and a call made without one adds to nothing.
func TestATurnsTallyHearsItsOwnCalls(t *testing.T) {
	t.Parallel()
	r := newSeamRig(&answeringProvider{in: 30, out: 12, cacheRead: 10}, nil)
	tally := auxspend.NewTally()
	use := turnUse
	use.Tally = tally
	for range 2 {
		if _, err := r.complete(t, dev, use); err != nil {
			t.Fatalf("Complete: %v", err)
		}
	}
	if _, err := r.complete(t, dev, turnUse); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := tally.Total(); got != (auxspend.Spent{Calls: 2, Input: 60, Output: 24, CacheRead: 20}) {
		t.Fatalf("tally = %+v, want the two calls made with it", got)
	}
}

// turnBudget is a turn's meter as the seam reaches it: what it was asked to
// record, and the refusal it holds, if any.
type turnBudget struct {
	countingMeter
	held  error
	asked int
}

func (b *turnBudget) Held() error {
	b.asked++
	return b.held
}

// A TURN'S CALL IS CHARGED THROUGH THE TURN'S OWN METER, never beside it.
//
// The counters are the seat's and the company's either way; what differs is
// which frame hears their answer. Charged through the seat's bare recorder, a
// window the call filled was one the turn's meter never learned of, so the
// turn's next round was sent, billed and refused. A call that carries no meter
// — a turn-stage call made between two segments, a reflection — is charged as
// it always was.
func TestATurnsCallIsChargedThroughItsOwnMeter(t *testing.T) {
	t.Parallel()
	seat := &countingMeter{}
	r := newSeamRig(&answeringProvider{in: 700, out: 300}, seat)
	budget := &turnBudget{}
	use := turnUse
	use.Budget = budget
	if _, err := r.complete(t, dev, use); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if budget.spent != 1000 || budget.calls != 1 {
		t.Errorf("the turn's meter recorded %d tokens over %d calls, want the call's 1000 once",
			budget.spent, budget.calls)
	}
	if seat.calls != 0 {
		t.Errorf("the seat's bare recorder was charged %d times beside the turn's meter, "+
			"which counts the call twice", seat.calls)
	}
	if budget.asked != 1 {
		t.Errorf("the turn's meter was asked %d times before the call, want once", budget.asked)
	}

	if _, err := r.complete(t, dev, turnUse); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if seat.spent != 1000 {
		t.Errorf("a call with no turn meter charged the seat %d, want its 1000", seat.spent)
	}
}

// A CALL THE TURN'S METER ALREADY REFUSES IS NOT MADE.
//
// The turn has seen a window full, so its next round is refused before it is
// sent; a rewrite made now would feed a prompt nobody sends. Nothing was
// spent, so nothing is charged, recorded or tallied — and the caller is told
// the budget refused it, which it takes as it takes any rewrite it cannot have.
func TestACallTheTurnsMeterRefusesIsNotMade(t *testing.T) {
	t.Parallel()
	provider := &answeringProvider{in: 700, out: 300}
	seat := &countingMeter{}
	r := newSeamRig(provider, seat)
	budget := &turnBudget{held: &toolloop.BudgetError{
		Scope: "agent", Used: 1600, Limit: 1000, Period: period.Day, Window: "2026-09-23",
	}}
	use := turnUse
	use.Budget, use.Tally = budget, auxspend.NewTally()

	_, err := r.complete(t, dev, use)
	if !errors.Is(err, toolloop.ErrBudgetExhausted) {
		t.Fatalf("Complete = %v, want the turn's refusal", err)
	}
	if provider.calls != 0 {
		t.Errorf("the provider was called %d times past a refusal the turn already held", provider.calls)
	}
	if budget.calls != 0 || seat.calls != 0 {
		t.Errorf("a call never made was charged (turn meter %d, seat %d)", budget.calls, seat.calls)
	}
	if got := use.Tally.Total(); got != (auxspend.Spent{}) {
		t.Errorf("a call never made was tallied: %+v", got)
	}
	if recs := r.flushed(t); len(recs) != 0 {
		t.Errorf("a call never made was recorded: %+v", recs)
	}
}

// THE DAY IS THE CHARGE'S. The instant a call returned is read once, and both
// the counter's windows and the record's company day are cut from it — so a
// call that returned a moment before midnight on the company's clock is on that
// day in both, never charged to one day and recorded on the next.
func TestTheRecordsDayIsTheDayTheCounterWasCharged(t *testing.T) {
	t.Parallel()
	santiago, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Fatalf("no zone database: %v", err)
	}
	meter := &countingMeter{}
	r := newSeamRig(&answeringProvider{in: 1, out: 1}, meter)
	r.seam.zone = santiago
	// 23:59:59.9 in Santiago on 22 September, which is 02:59 UTC on the 23rd.
	ended := time.Date(2026, 9, 22, 23, 59, 59, 900_000_000, santiago)
	ticks := []time.Time{ended.Add(-time.Second), ended}
	r.seam.now = func() time.Time {
		next := ticks[0]
		ticks = ticks[1:]
		return next
	}
	if _, err := r.complete(t, dev, turnUse); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(meter.at) != 1 || !meter.at[0].Equal(ended) {
		t.Fatalf("the counter was charged at %v, want the instant the call returned, %v", meter.at, ended)
	}
	recs := r.flushed(t)
	if len(recs) != 1 || recs[0].Day != "2026-09-22" {
		t.Fatalf("recorded on %+v, want the company day the charge fell in, 2026-09-22", recs)
	}
	if day := coord.WindowsAt(meter.at[0], santiago)[0].Label; day != recs[0].Day {
		t.Fatalf("the counter's day %s and the record's %s disagree", day, recs[0].Day)
	}
}

// AN AGENT SEAT'S SPEND IS THE SEAT'S AND THE COMPANY'S; EVERYTHING ELSE IS THE
// COMPANY'S ALONE.
//
// A person's question (the operator stage) is the company's even when the seat
// the credential names runs as an agent, because a person has no seat budget;
// and a background pass resolved on a HUMAN seat's chain — a unit a person
// leads — is the company's too, where it used to reach no counter at all. Both
// are recorded for the person, never as a seat the live view would key a state
// on.
func TestWhoseCounterAnAuxiliaryCallIsChargedTo(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	founder := &org.Role{Name: "Founder", Kind: org.KindHuman}
	c := meteredCompany(config.TokenBudget{}, dev, founder)
	provider := &answeringProvider{in: 30, out: 12}
	registry, err := phase.NewRegistry([]phase.Entry{{Key: "cheap", Provider: provider}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	c.Models = registry
	pub := &capturedEvents{}
	e := &Engine{backends: &Backends{Fleet: fleet}, auxSpend: auxspend.NewLedger(pub)}
	seam := e.auxiliaryFor(c)

	call := func(role *org.Role, use auxspend.Use) {
		t.Helper()
		member, err := seam.Auxiliary(role, use)
		if err != nil {
			t.Fatalf("Auxiliary: %v", err)
		}
		if _, err := member.Provider.Complete(ctx, llm.Request{}); err != nil {
			t.Fatalf("Complete: %v", err)
		}
	}
	windows := coord.WindowsAt(time.Now(), time.UTC)
	day := func(scope string) int {
		u, err := fleet.Used(ctx, scope, windows)
		if err != nil {
			t.Fatalf("Used(%s): %v", scope, err)
		}
		return u.In(period.Day).Used
	}

	call(dev, turnUse)
	if day(coord.OrgScope) != 42 || day(scopeOf(t, c, dev)) != 42 {
		t.Fatalf("a seat's call: company %d, seat %d — want 42 on both",
			day(coord.OrgScope), day(scopeOf(t, c, dev)))
	}
	call(dev, auxspend.Use{Stage: types.AuxStageOperator, Purpose: types.AuxAnswerKnowledge})
	if day(coord.OrgScope) != 84 || day(scopeOf(t, c, dev)) != 42 {
		t.Fatalf("a person's question: company %d, seat %d — want it on the company alone",
			day(coord.OrgScope), day(scopeOf(t, c, dev)))
	}
	call(founder, auxspend.Use{Stage: types.AuxStageBackground, Purpose: types.AuxSkillPromotion})
	if day(coord.OrgScope) != 126 {
		t.Fatalf("a pass on a person's chain: company %d — want it counted there, 126",
			day(coord.OrgScope))
	}

	e.auxSpend.Flush(ctx)
	byPurpose := map[types.AuxPurpose]types.AuxiliarySpend{}
	for _, rec := range pub.records(t) {
		byPurpose[rec.Purpose] = rec
	}
	if seat := byPurpose[types.AuxMemoryFilter]; seat.Agent == "" || seat.ActorSeat != "" {
		t.Errorf("a seat's call was recorded as %+v, want the seat by its agent id", seat)
	}
	for _, purpose := range []types.AuxPurpose{types.AuxAnswerKnowledge, types.AuxSkillPromotion} {
		rec := byPurpose[purpose]
		if rec.Agent != "" || rec.RoleName != "" || rec.ActorSeat == "" {
			t.Errorf("%s was recorded as %+v, want a person's: actor_seat, no agent, no role", purpose, rec)
		}
	}
}

// AN UNCAPPED SEAT'S AUXILIARY SPEND IS COUNTED TOO. The counters are what a
// ceiling set later judges, so spend that reached none of them would be a
// window handed back its whole allowance the moment somebody capped it.
func TestAnUncappedSeatsAuxiliarySpendIsCounted(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	free := &org.Role{Name: "Free"}
	c := meteredCompany(config.TokenBudget{}, free)
	e := &Engine{backends: &Backends{Fleet: fleet}}
	seam := auxiliarySeam{heads: staticHeads{provider: &answeringProvider{in: 30, out: 12}},
		org: c.Org, zone: time.UTC, charge: e.auxiliaryCharge(c), now: time.Now}
	member, err := seam.Auxiliary(free, turnUse)
	if err != nil {
		t.Fatalf("Auxiliary: %v", err)
	}
	if _, err := member.Provider.Complete(ctx, llm.Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	windows := coord.WindowsAt(time.Now(), time.UTC)
	for _, scope := range []string{coord.OrgScope, scopeOf(t, c, free)} {
		u, err := fleet.Used(ctx, scope, windows)
		if err != nil || u.In(period.Day).Used != 42 {
			t.Errorf("%s's day = (%+v, %v), want the 42 tokens the completion spent",
				scope, u.In(period.Day), err)
		}
	}
}

// searchableKnowledge is a knowledge backend that will always search, so the
// prefetch's knowledge block reaches its auxiliary query call.
type searchableKnowledge struct{}

func (searchableKnowledge) Backend() string                             { return "test" }
func (searchableKnowledge) CanSearch(*org.Role, *org.Organization) bool { return true }
func (searchableKnowledge) Search(context.Context, knowledge.Query) knowledge.Result {
	return knowledge.Result{}
}

// queryingProvider answers a usable search query at a known token cost.
type queryingProvider struct{ in, out, calls int }

func (p *queryingProvider) Model() string { return "test-model" }

func (p *queryingProvider) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	p.calls++
	return &llm.Completion{Model: "test-model", Content: "deploy runbook",
		InputTokens: p.in, OutputTokens: p.out}, nil
}

// THE TURN-START PREFETCH'S AUXILIARY CALLS ARE CHARGED, RECORDED AND TALLIED.
//
// The memory filter, the knowledge query and the episode summary each send a
// full prompt on EVERY turn, and the prefetch resolved them off the bare
// registry — so that spend reached no counter and no record, a seat at its
// ceiling went on paying for its turn-start context, and every figure an
// operator reads understated it. This drives a real Fetch through the engine's
// own sources and reads the counters, the record and the turn's tally back.
func TestThePrefetchsAuxiliaryCompletionsAreChargedAndRecorded(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	seat := &org.Role{Name: "Dev"}
	c := meteredCompany(config.TokenBudget{}, seat)
	provider := &queryingProvider{in: 300, out: 25}
	models, err := phase.NewRegistry([]phase.Entry{{Key: "aux", Provider: provider}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	c.Models = models
	pub := &capturedEvents{}
	e := &Engine{backends: &Backends{Fleet: fleet}, auxSpend: auxspend.NewLedger(pub)}

	src := e.prefetchSources(c)
	src.Knowledge = searchableKnowledge{}
	tally := auxspend.NewTally()
	prefetch.New(src).Fetch(ctx, prefetch.Request{
		Seat: seat, Org: c.Org, Task: "ship the release", Ask: "ship the release",
		Aux: auxspend.Use{Stage: types.AuxStageTurn, TurnID: "run-1", Tally: tally},
	})
	if provider.calls != 1 {
		t.Fatalf("the auxiliary model was called %d times, want the one knowledge "+
			"query — the case exercises nothing otherwise", provider.calls)
	}
	windows := coord.WindowsAt(time.Now(), time.UTC)
	for _, scope := range []string{coord.OrgScope, scopeOf(t, c, seat)} {
		u, err := fleet.Used(ctx, scope, windows)
		if err != nil || u.In(period.Day).Used != 325 {
			t.Errorf("%s's day = (%+v, %v), want the 325 tokens the prefetch's "+
				"knowledge query spent", scope, u.In(period.Day), err)
		}
	}
	e.auxSpend.FlushTurn(ctx, "run-1")
	recs := pub.records(t)
	if len(recs) != 1 || recs[0].Purpose != types.AuxKnowledgeQuery || recs[0].TurnID != "run-1" ||
		recs[0].TotalTokens != 325 {
		t.Fatalf("recorded %+v, want the knowledge query's 325 tokens under the turn", recs)
	}
	if got := tally.Total().Tokens(); got != 325 {
		t.Fatalf("the turn's tally holds %d, want the 325 its context cost", got)
	}
}
