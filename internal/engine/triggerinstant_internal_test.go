package engine

import (
	"context"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// WHEN A TURN'S OPERATION IDS CAN FIRST HAVE BEEN MINTED, from the trigger to
// the writer.
//
// A write in a turn derives its operation id from the turn, so a redelivered
// turn re-derives the ids its first attempt minted, and a node that adopted a
// snapshot since answers a write stamped before the adoption from what the
// adoption brought. A write stamped with its own call's instant — later than
// the first attempt's — reads as newer than an adoption the first attempt
// predates, and is decided a second time. So the instant is fixed where the
// turn is described, carried on the turn's context, and handed to every writer
// a seat's tools are given.

// instantCompany is a company with one seat, for describing its turns.
func instantCompany() *Company {
	o := &org.Organization{
		Name:  "Acme",
		Roles: []*org.Role{{Name: "Engineer", DeclaredHandle: "eng"}},
	}
	o.Normalize()
	return &Company{Config: &config.Company{}, Org: o}
}

// A DISPATCHED TURN CARRIES ITS EARLIEST TRIGGER'S INSTANT, and a turn with no
// unit of work its own start.
//
// The work key is derived from the partition's events, every attempt at the
// work was woken by them, and none can have minted anything before they
// existed — so the earliest of them bounds the first attempt's ids where the
// dispatch's own instant, on a redelivery, does not. An event with no
// timestamp says nothing and is passed over. With no work key the run is the
// seed, and nothing but this run derives its ids.
//
// Mutation: describe the turn with its own start whatever its events, or hand
// the runner a context without the instant, and this fails.
func TestADispatchedTurnCarriesItsEarliestTriggersInstant(t *testing.T) {
	t.Parallel()
	company := instantCompany()
	earliest := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)
	partition := []*events.Event{
		{Timestamp: earliest.Add(time.Minute)},
		nil,
		{},
		{Timestamp: earliest},
	}

	tel := (&Engine{}).describeTurn(t.Context(), company, Request{
		Handle: "eng", RunID: "run-2", WorkKey: "wk-1", Events: partition,
	})
	got := tel.runnerTurn(company, 0, nil, "", turn.NoReply()).Context
	if got == nil {
		t.Fatal("the dispatched turn carries no context")
	}
	if !got.TriggeredAt.Equal(earliest) {
		t.Fatalf("a redelivered turn's context carries %v, want its earliest "+
			"trigger's %v", got.TriggeredAt, earliest)
	}

	before := time.Now().UTC()
	tel = (&Engine{}).describeTurn(t.Context(), company, Request{
		Handle: "eng", RunID: "run-3", Events: partition,
	})
	after := time.Now().UTC()
	got = tel.runnerTurn(company, 0, nil, "", turn.NoReply()).Context
	if got.TriggeredAt.Before(before) || got.TriggeredAt.After(after) {
		t.Errorf("a turn with no work key carries %v, want its own start, "+
			"between %v and %v", got.TriggeredAt, before, after)
	}
}

// turnProbe is a seat tool that keeps the turn each call is handed.
type turnProbe struct {
	mu     sync.Mutex
	handed []*turnctx.Turn
}

func (*turnProbe) Name() string               { return "probe_turn" }
func (*turnProbe) Description() string        { return "Reports nothing; the test reads the turn." }
func (*turnProbe) Parameters() map[string]any { return map[string]any{"type": "object"} }

func (*turnProbe) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{Output: "no turn"}, nil
}

func (p *turnProbe) CallForTurn(_ context.Context, t *turnctx.Turn, _ map[string]any) (tools.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.handed = append(p.handed, t)
	return tools.Result{Output: "seen"}, nil
}

func (p *turnProbe) turns() []*turnctx.Turn {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.handed)
}

// probesThenSubmits calls probe_turn once in the executor's resumed round,
// then submits, and fails the review: the turn the probe is handed is the
// whole of what this reads.
type probesThenSubmits struct{}

func (probesThenSubmits) Model() string { return "probes-then-submits" }

func (probesThenSubmits) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	for _, def := range req.Tools {
		if def.Name == runner.SubmitReviewTool {
			return unavailableModel{}.Complete(ctx, req)
		}
	}
	for _, m := range req.Messages {
		if m.Name == "probe_turn" {
			return submitsThenReviewFails{}.Complete(ctx, req)
		}
	}
	return &llm.Completion{ToolCalls: []llm.ToolCall{{ID: "p", Name: "probe_turn",
		Arguments: map[string]any{}}}}, nil
}

// A RESUMED TURN'S IDENTITY IS THE ONE ITS ROW RECORDS, from the resumer to
// the writer.
//
// The resume re-enters the turn that launched the run with no trigger to
// re-read, so everything that names it comes off the run's row: the run it
// continues, the unit of work its writes are idempotent against, and the
// instant its operation ids can first have been minted at — the launching
// turn's own, which the launch writes onto the row, rather than the run's
// first launch, because a write the turn made before that launch and makes
// again after the resume derives the same id. Driven through the resumer a
// completion reaches, and read where a seat's tools read it: a tool's call is
// handed the turn the runner was built with.
//
// Mutation: describe the resumed turn with the run's first launch, a run id
// of its own or no unit of work, and this fails.
func TestAResumedTurnsIdentityIsTheOneItsRowRecords(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	company, seat := resumableCompany(t, probesThenSubmits{}, 0)
	company.Config.TurnEngine = config.DefaultTurnEngine()
	probe := &turnProbe{}
	if err := company.Tools.Register(probe, tools.OriginBuiltin); err != nil {
		t.Fatalf("Register: %v", err)
	}
	q := memory.New()
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	e, _ := resumingEngine(t)
	e.backends = &Backends{Queue: q}
	e.epoch.current.Store(company)

	state, err := execstate.Encode(execstate.State{
		Version: execstate.Version,
		Messages: []llm.Message{{Role: "assistant",
			ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "run_sandbox"}}}},
		PendingCallID: "call-1", PendingCallName: "run_sandbox",
		ActiveTools: []string{probe.Name()}, Round: 1, Task: "fix the flaking test",
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	triggered := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)
	run := sandbox.PendingRun{
		TurnID: "run-1", WorkKey: "wk-1", AgentHandle: seat.Handle(), Role: seat.Name,
		TriggeredAt: triggered, CreatedAt: triggered.Add(10 * time.Minute),
		ExecuteState: state, Reply: "tool",
	}

	_ = (&resumer{engine: e}).Resume(ctx, sandbox.ResumeRequest{Run: run, Answer: "done", Success: true})
	handed := probe.turns()
	if len(handed) != 1 || handed[0] == nil {
		t.Fatalf("the probe was handed %d turns, want the resumed round's one", len(handed))
	}
	got := handed[0]
	if got.RunID != run.TurnID || got.WorkKey != run.UnitOfWork() {
		t.Errorf("the resumed turn names run %q and unit of work %q, want the row's %q and %q",
			got.RunID, got.WorkKey, run.TurnID, run.UnitOfWork())
	}
	if !got.TriggeredAt.Equal(triggered) {
		t.Errorf("the resumed turn's writes are stamped %v, want the launching turn's %v off the row",
			got.TriggeredAt, triggered)
	}
}

// EVERY WRITER A SEAT'S TOOLS ARE HANDED CARRIES THE TURN'S MINT INSTANT.
//
// The tracker's write authority takes the instant as provenance, one writer
// per actor, and a seat's tools reach it in four shapes — the item writer, the
// project writer, the dependency sequence and the merge sequence. A shape
// that dropped it would stamp every write of its kind with the call's own
// instant, which is the second decision above for exactly that kind of write.
//
// Compared whole against the writer the provenance names, and against one
// without the instant, so the comparison is seen to turn on it.
//
// Mutation: leave MintedAt out of any one of the four, and this fails.
func TestEveryWriterASeatIsHandedCarriesTheMintInstant(t *testing.T) {
	t.Parallel()
	base := &tracker.Writer{}
	e := &Engine{native: &native{trackerReader: &tracker.Reader{}, writer: base}}
	deps := e.workDeps(instantCompany())
	actor := builtin.Actor{
		Handle: "eng", Kind: tracker.AuthorAgent, TurnID: "run-1",
		Chain: []string{"pm"}, MintedAt: time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC),
	}
	want := base.As(actor.Handle, actor.Kind, tracker.Provenance{
		TurnID: actor.TurnID, Chain: actor.Chain, MintedAt: actor.MintedAt,
	})
	unstamped := base.As(actor.Handle, actor.Kind, tracker.Provenance{
		TurnID: actor.TurnID, Chain: actor.Chain,
	})
	for name, shape := range map[string]func(builtin.Actor) any{
		"the item writer":         func(a builtin.Actor) any { return deps.Writer(a) },
		"the project writer":      func(a builtin.Actor) any { return deps.ProjectWriter(a) },
		"the dependency sequence": func(a builtin.Actor) any { return deps.Dependencies(a) },
		"the merge sequence":      func(a builtin.Actor) any { return deps.Merges(a) },
	} {
		got, ok := shape(actor).(*tracker.Writer)
		if !ok {
			t.Errorf("%s is not the tracker's writer: %T", name, shape(actor))
			continue
		}
		if !reflect.DeepEqual(got, want) || reflect.DeepEqual(got, unstamped) {
			t.Errorf("%s was derived without the turn's mint instant %v",
				name, actor.MintedAt)
		}
	}
}
