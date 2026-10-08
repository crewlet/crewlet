package engine

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/config"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// The turn's opening record, agent_turn_started, from the two frames that
// publish it.
//
// Driven through the REAL frames rather than through [Engine.publishTurnStarted]
// alone, because every property below is about WHERE the frame calls it — before
// the prefetch, after a resume's retries, and paired with a completion on every
// path — and a test of the builder cannot see any of that.

// starting builds an engine whose only wired backend is a capturing queue,
// over a one-seat company that refuses a turn at its DEPTH GUARD: the frame
// runs its prefetch, builds its runner and publishes its completion, and the
// loop ends before any model is asked anything. Nil models is a company whose
// runner cannot be built at all.
func starting(t *testing.T, models *phase.Registry) (*Engine, *pub) {
	t.Helper()
	seat := &org.Role{Name: "SWE", DeclaredHandle: "swe", LLM: org.ProviderKeys{"only"}}
	p := &pub{}
	// A COORDINATION STORE, as every engine `crewlet run` builds has one
	// (local coordination is an in-memory fleet): a turn whose identity
	// carries no start — these cases' literal run ids carry none — rebases
	// there (rebase.go), and an engine with nowhere to record that refuses
	// the turn rather than guess.
	e := &Engine{backends: &Backends{Queue: p, Fleet: coordmem.NewFleet()}}
	e.epoch.current.Store(&Company{
		Org:    &org.Organization{Name: "Acme", Roles: []*org.Role{seat}},
		Models: models,
		Tools:  tools.NewRegistry(),
		Config: &config.Company{Name: "Acme", TurnEngine: config.TurnEngine{
			MaxIterations: 1, DelegationDepthLimit: 1, MaxToolRounds: 3,
		}},
	})
	return e, p
}

// refusingModels is the seat's one model, which no case reaches.
func refusingModels(t *testing.T) *phase.Registry {
	t.Helper()
	models, err := phase.NewRegistry([]phase.Entry{{Key: "only", Provider: refusingProvider{}}})
	if err != nil {
		t.Fatalf("phase.NewRegistry: %v", err)
	}
	return models
}

// published is the capture's events, in the order they were published.
func (p *pub) published() []*events.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.events)
}

// position is where the first event of a type sits in what was published, or
// -1 when none was.
func position(evs []*events.Event, typ string) int {
	return slices.IndexFunc(evs, func(ev *events.Event) bool { return ev.Type == typ })
}

// startsOf is every agent_turn_started in what was published, with its
// envelope, since the depth and the chain are the envelope's.
func startsOf(evs []*events.Event) []*events.Event {
	var out []*events.Event
	for _, ev := range evs {
		if _, ok := events.DataAs[*types.AgentTurnStarted](ev); ok {
			out = append(out, ev)
		}
	}
	return out
}

// A DISPATCHED TURN IS ON THE RECORD BEFORE IT GATHERS ITS CONTEXT.
//
// The prefetch reads a chat thread and searches the knowledge base over the
// network, and it is the first thing a turn does that takes time — so a start
// published after it, or from inside the first phase, leaves exactly the
// window this event exists for with nothing saying a turn is running, and a
// turn that dies in it with no row naming its run. The prefetch's own summary
// is the witness: it is published the moment the context is assembled.
func TestATurnAnnouncesItselfBeforeItsPrefetch(t *testing.T) {
	t.Parallel()
	e, p := starting(t, refusingModels(t))
	tick := events.New(types.TaskAssigned{
		TaskID: "t-1", RoleName: "SWE", Description: "write the standup note",
		Schedule: "standup",
	}, events.TraceContext{})

	if _, err := e.runTurn(t.Context(), Request{
		RunID: "run-1", Handle: "swe", WorkKey: "wk-1", ConversationKey: "conv-1",
		Depth: 3, DelegationChain: []string{"ceo", "cto"},
		Events: []*events.Event{tick},
	}); err != nil {
		t.Fatalf("runTurn: %v", err)
	}

	evs := p.published()
	starts := startsOf(evs)
	if len(starts) != 1 {
		t.Fatalf("published %d starts (%s), want exactly one", len(starts), typesOf(evs))
	}
	start, prefetched, ended := position(evs, "agent_turn_started"),
		position(evs, "prefetch_summary"), position(evs, "agent_turn_completed")
	if prefetched < 0 || ended < 0 {
		t.Fatalf("the turn did not run its prefetch and its completion (%s); this "+
			"case asserts nothing", typesOf(evs))
	}
	if start > prefetched {
		t.Errorf("the start was published after the prefetch (%s): the turn was "+
			"silent for the whole of its context assembly", typesOf(evs))
	}
	if start > ended {
		t.Errorf("the start was published after the completion (%s)", typesOf(evs))
	}

	ev := starts[0]
	got, _ := events.DataAs[*types.AgentTurnStarted](ev)
	if got.TurnID != "run-1" || got.WorkKey != "wk-1" {
		t.Errorf("turn_id = %q, work_key = %q, want the run and its unit of work",
			got.TurnID, got.WorkKey)
	}
	if got.AgentHandle != "swe" || got.RoleName != "SWE" || got.Agent == "" {
		t.Errorf("start = %+v, want it addressed to the seat, by handle, role and id", got)
	}
	if got.Trigger.Type != tick.Type || got.ConversationKey != "conv-1" {
		t.Errorf("trigger = %+v, conversation = %q, want what woke it and where",
			got.Trigger, got.ConversationKey)
	}
	if got.Resumed {
		t.Error("a dispatch announced itself as a resumed segment")
	}
	// THE SAME INSTANT THE LEARNING RECORD REPORTS AS THE TURN'S START: both
	// are read off the one telemetry, so two records of one run cannot
	// disagree about when it began.
	done := only[*types.TurnCompleted](t, p, "turn_completed")
	if got.StartedAt.IsZero() || !got.StartedAt.Equal(done.StartedAt) {
		t.Errorf("started_at = %v, turn_completed.started_at = %v, want one instant",
			got.StartedAt, done.StartedAt)
	}
	// The depth and the chain are the ENVELOPE's keys: a payload field
	// under either would be dropped on the way out.
	if ev.DelegationDepth != 3 || !slices.Equal(ev.DelegationChain, []string{"ceo", "cto"}) {
		t.Errorf("envelope depth = %d, chain = %v, want the dispatch's 3 and [ceo cto]",
			ev.DelegationDepth, ev.DelegationChain)
	}
	if ev.Source != "SWE" {
		t.Errorf("source = %q, want the seat, as every turn-level event is sourced", ev.Source)
	}
	// ABSENT, never null, while nothing names an item — the documented shape
	// of an unattributed turn.
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"work_item":`) {
		t.Errorf("an unattributed start carries a work_item key: %s", raw)
	}
}

// A TURN WHOSE RUNNER COULD NOT BE BUILT IS CLOSED, because it was opened.
//
// This path published nothing while a turn's first record was its first
// phase; once the turn announces itself before the build, a failed build that
// stays silent leaves a start nothing ends — which a reader takes for a turn
// still running, or one whose process died under it.
func TestATurnWhoseRunnerCouldNotBeBuiltIsClosed(t *testing.T) {
	t.Parallel()
	e, p := starting(t, nil)

	_, err := e.runTurn(t.Context(), Request{
		RunID: "run-2", Handle: "swe", WorkKey: "wk-2",
		Events: []*events.Event{events.New(types.TaskAssigned{
			TaskID: "t-2", RoleName: "SWE", Description: "d", Schedule: "s",
		}, events.TraceContext{})},
	})
	if !errors.Is(err, phase.ErrNoProviders) {
		t.Fatalf("err = %v, want the build's own refusal", err)
	}
	evs := p.published()
	if n := len(startsOf(evs)); n != 1 {
		t.Fatalf("published %d starts (%s), want the one this run announced", n, typesOf(evs))
	}
	summary := only[*types.AgentTurnCompleted](t, p, "agent_turn_completed")
	if summary.TurnID != "run-2" || !summary.Failed ||
		!strings.Contains(summary.Error, "configures no model provider") {
		t.Errorf("completion = %+v, want run-2 closed as failed, naming why", summary)
	}
	if done := only[*types.TurnCompleted](t, p, "turn_completed"); done.TurnID != "run-2" {
		t.Errorf("turn_completed names %q, want run-2", done.TurnID)
	}
}

// A RESUMED SEGMENT ANNOUNCES ITSELF UNDER THE RUN IT RE-ENTERS.
//
// One run has several starts when it parks, and `resumed` is what tells the
// first — the turn beginning — from the rest. The turn id is the PARKED ROW's,
// never a fresh one: a new id would split one turn across two on every screen.
func TestAResumeAnnouncesItsSegment(t *testing.T) {
	t.Parallel()
	e, p := starting(t, refusingModels(t))
	company := e.Company()
	seat := company.Org.AgentSeatByHandle("swe")
	completion := events.New(types.SandboxRunCompleted{
		AgentHandle: "swe", RoleName: "SWE", TurnID: "run-1", SandboxID: "sbx-1",
	}, events.TraceContext{})

	if err := e.resumeTurn(t.Context(), resumeInput{
		Company: company,
		Run: sandbox.PendingRun{
			TurnID: "run-1", WorkKey: "wk-1", AgentHandle: "swe", Reply: "tool",
			TaskDescription: "fix the failing test", ConversationKey: "conv-1",
			DelegationDepth: 3, DelegationChain: []string{"ceo"},
		},
		Turn:    &turnctx.Turn{RunID: "run-1", WorkKey: "wk-1", Seat: seat, Org: company.Org},
		Answer:  "the tests pass now",
		Trigger: completion,
	}); err != nil {
		t.Fatalf("resumeTurn: %v", err)
	}

	evs := p.published()
	starts := startsOf(evs)
	if len(starts) != 1 {
		t.Fatalf("published %d starts (%s), want exactly one for the segment",
			len(starts), typesOf(evs))
	}
	if position(evs, "agent_turn_started") > position(evs, "agent_turn_completed") {
		t.Errorf("the segment's start was published after its completion (%s)", typesOf(evs))
	}
	got, _ := events.DataAs[*types.AgentTurnStarted](starts[0])
	if !got.Resumed {
		t.Error("a resumed segment announced itself as a fresh turn")
	}
	if got.TurnID != "run-1" || got.WorkKey != "wk-1" {
		t.Errorf("turn_id = %q, work_key = %q, want the parked run's own", got.TurnID, got.WorkKey)
	}
	if got.Trigger.Type != completion.Type || got.ConversationKey != "conv-1" {
		t.Errorf("trigger = %+v, conversation = %q, want the event that resumed it "+
			"and the conversation the run reports back to", got.Trigger, got.ConversationKey)
	}
	if done := only[*types.TurnCompleted](t, p, "turn_completed"); !got.StartedAt.Equal(done.StartedAt) {
		t.Errorf("started_at = %v, want the segment's own start, %v", got.StartedAt, done.StartedAt)
	}
	if starts[0].DelegationDepth != 3 || !slices.Equal(starts[0].DelegationChain, []string{"ceo"}) {
		t.Errorf("envelope depth = %d, chain = %v, want the parked row's",
			starts[0].DelegationDepth, starts[0].DelegationChain)
	}
}

// A RESUME THAT CANNOT RUN ANNOUNCES NOTHING.
//
// Every early return of the resume is a retry of the SAME run: the claim
// reverts and the resume comes round again under one turn id. A start
// published before them leaves a segment nothing closes — and closing it would
// record as ended a run that is still parked — so the segment is announced
// only once it is certain to run.
func TestAResumeThatCannotRunAnnouncesNothing(t *testing.T) {
	t.Parallel()
	e, p := starting(t, nil)
	company := e.Company()
	seat := company.Org.AgentSeatByHandle("swe")

	err := e.resumeTurn(t.Context(), resumeInput{
		Company: company,
		Run: sandbox.PendingRun{
			TurnID: "run-3", AgentHandle: "swe", Reply: "tool",
			TaskDescription: "fix the failing test",
		},
		Turn: &turnctx.Turn{RunID: "run-3", Seat: seat, Org: company.Org},
	})
	if !errors.Is(err, phase.ErrNoProviders) {
		t.Fatalf("err = %v, want the build's own refusal", err)
	}
	if evs := p.published(); len(startsOf(evs)) != 0 {
		t.Errorf("a resume that will be retried announced a segment (%s)", typesOf(evs))
	}
}

// begins is a [sandbox.ResumeRequest.Begin] that records each call, how many
// segment starts had been published when it came, and answers err.
type begins struct {
	mu     sync.Mutex
	calls  int
	starts []int
	p      *pub
	err    error
}

func (b *begins) begin(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	b.starts = append(b.starts, len(startsOf(b.p.published())))
	return b.err
}

// A RESUME BEGINS ITS TURN BEFORE ANY OF IT RUNS, and once: the coordinator's
// commit — for a recorded answer, the record that this turn took it — is made
// after every check that can still send the resume back, and before the
// segment is so much as announced. Through the engine's own resumer, so the
// commit the coordinator hands over is the one the frame calls.
func TestAResumeBeginsItsTurnBeforeAnyOfItRuns(t *testing.T) {
	t.Parallel()
	e, p := starting(t, refusingModels(t))
	state, err := execstate.Encode(execstate.State{
		Messages: []llm.Message{{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
			ID: "call-1", Name: builtin.RunSandboxTool,
		}}}},
		PendingCallID: "call-1", PendingCallName: builtin.RunSandboxTool,
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	b := &begins{p: p}
	err = (&resumer{engine: e}).Resume(t.Context(), sandbox.ResumeRequest{
		Run: sandbox.PendingRun{
			TurnID: "run-4", AgentHandle: "swe", Reply: "tool",
			TaskDescription: "fix the failing test", DelegationDepth: 3,
			ExecuteState: state,
		},
		Answer: "use main",
		Begin:  b.begin,
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if b.calls != 1 {
		t.Fatalf("Begin called %d times, want once for the one segment", b.calls)
	}
	if b.starts[0] != 0 {
		t.Errorf("Begin came after %d segment starts, want before the first: a turn on the "+
			"record before its commit is one a crash can leave untaken", b.starts[0])
	}
	if starts := startsOf(p.published()); len(starts) != 1 {
		t.Errorf("published %d starts, want the segment's one", len(starts))
	}
}

// A COMMIT THAT CANNOT BE MADE STOPS THE TURN: the resume returns Begin's own
// failure — a retry, as every early return of the resume is — and nothing of
// the segment runs or is announced. A turn that ran without it would be read,
// by whoever reaps its row after a crash, as one that never got the answer.
func TestAResumeWhoseBeginIsRefusedDoesNotRun(t *testing.T) {
	t.Parallel()
	e, p := starting(t, refusingModels(t))
	company := e.Company()
	seat := company.Org.AgentSeatByHandle("swe")
	refused := errors.New("the claim no longer holds the answer")
	b := &begins{p: p, err: refused}
	err := e.resumeTurn(t.Context(), resumeInput{
		Company: company,
		Run: sandbox.PendingRun{
			TurnID: "run-5", AgentHandle: "swe", Reply: "tool",
			TaskDescription: "fix the failing test", DelegationDepth: 3,
		},
		Turn:  &turnctx.Turn{RunID: "run-5", Seat: seat, Org: company.Org},
		Begin: b.begin,
	})
	if !errors.Is(err, refused) || errors.Is(err, sandbox.ErrResumeAbandoned) {
		t.Fatalf("err = %v, want Begin's refusal handed back as a retry", err)
	}
	if evs := p.published(); len(evs) != 0 {
		t.Errorf("a turn whose commit was refused published %s", typesOf(evs))
	}
}

// A RESUME THAT CANNOT RUN NEVER BEGINS: the runner could not be built, so the
// resume is a retry and nothing took the answer.
func TestAResumeThatCannotRunNeverBegins(t *testing.T) {
	t.Parallel()
	e, p := starting(t, nil)
	company := e.Company()
	seat := company.Org.AgentSeatByHandle("swe")
	b := &begins{p: p}
	if err := e.resumeTurn(t.Context(), resumeInput{
		Company: company,
		Run: sandbox.PendingRun{
			TurnID: "run-6", AgentHandle: "swe", Reply: "tool", TaskDescription: "fix it",
		},
		Turn:  &turnctx.Turn{RunID: "run-6", Seat: seat, Org: company.Org},
		Begin: b.begin,
	}); !errors.Is(err, phase.ErrNoProviders) {
		t.Fatalf("err = %v, want the build's own refusal", err)
	}
	if b.calls != 0 {
		t.Fatalf("Begin called %d times by a resume that never reached its turn", b.calls)
	}
}
