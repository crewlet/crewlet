package toolloop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// What a loop records about its time and its cache.
//
// Every figure here is measured where the call is made, because that is the
// only frame with both ends of it: a reader that reconstructs a round's latency
// from when its events arrived subtracts two publishers' clocks and a queue's
// delay, and the cache counts every backend reports on its completion reached
// nothing past the round's span attribute until the loop kept them.

// slowProvider answers after a fixed delay, so a round's measured duration has
// a floor a test can assert.
type slowProvider struct {
	scriptedProvider
	delay time.Duration
}

func (p *slowProvider) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	time.Sleep(p.delay)
	return p.scriptedProvider.Complete(ctx, req)
}

// timedSurface answers after a fixed delay, as a named origin, and records what
// the loop had published and which round the call was told it was in at the
// moment each call reached it.
type timedSurface struct {
	delay  time.Duration
	origin string
	server string

	// latest is the last live Result the loop published, read at the
	// instant a call arrives — which is what "announced before the call"
	// has to mean.
	latest *toolloop.Result
	atCall []toolloop.Result
	rounds []int
}

func (s *timedSurface) ToolDefs() []llm.ToolDef { return []llm.ToolDef{def("read"), def("write")} }
func (s *timedSurface) Phase() string           { return "execute" }

func (s *timedSurface) Execute(ctx context.Context, _ llm.ToolCall) (toolloop.ToolResult, error) {
	if s.latest != nil {
		s.atCall = append(s.atCall, *s.latest)
	}
	round, ok := toolloop.CallRound(ctx)
	if !ok {
		round = -1
	}
	s.rounds = append(s.rounds, round)
	time.Sleep(s.delay)
	return toolloop.ToolResult{Output: "ok", Origin: s.origin, Server: s.server}, nil
}

func TestEveryRoundRecordsItsOwnSpanAndTokens(t *testing.T) {
	t.Parallel()
	const delay = 15 * time.Millisecond
	p := &slowProvider{delay: delay, scriptedProvider: scriptedProvider{turns: []llm.Completion{
		{
			Model: "first-member", Content: "reading",
			ToolCalls:   []llm.ToolCall{toolCall("1", "read"), toolCall("2", "read")},
			InputTokens: 1000, OutputTokens: 40, CacheRead: 800, CacheWrite: 150,
		},
		// A fallback chain can move a phase between members: each round
		// names the model that served IT.
		{
			Model: "second-member", Content: "done",
			InputTokens: 1300, OutputTokens: 20, CacheRead: 1100,
		},
	}}}
	s := &timedSurface{}

	before := time.Now().UTC()
	res, err := toolloop.Run(t.Context(), toolloop.Config{Provider: p, Surface: s, MaxRounds: 6})
	after := time.Now().UTC()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(res.Rounds) != 2 {
		t.Fatalf("recorded %d rounds, want one per provider call (2): %+v", len(res.Rounds), res.Rounds)
	}
	first, second := res.Rounds[0], res.Rounds[1]
	for i, r := range res.Rounds {
		if r.Round != i+1 {
			t.Errorf("round %d is numbered %d — the lists join on a one-based round", i, r.Round)
		}
		if r.StartedAt.Location() != time.UTC {
			t.Errorf("round %d started at %v, want UTC", r.Round, r.StartedAt)
		}
		if r.StartedAt.Before(before) || r.StartedAt.After(after) {
			t.Errorf("round %d started at %v, outside the run [%v, %v]", r.Round, r.StartedAt, before, after)
		}
		// THE MODEL'S HALF ONLY: at least the provider's own delay, and
		// never the round's tool calls on top.
		if r.Duration < delay {
			t.Errorf("round %d took %v, want at least the provider's %v", r.Round, r.Duration, delay)
		}
	}
	if second.StartedAt.Before(first.StartedAt.Add(first.Duration)) {
		t.Errorf("round 2 began at %v, before round 1's model call ended (%v + %v)",
			second.StartedAt, first.StartedAt, first.Duration)
	}
	if first.Model != "first-member" || second.Model != "second-member" {
		t.Errorf("rounds served by %q, %q — want the model each completion named", first.Model, second.Model)
	}
	if first.InputTokens != 1000 || first.OutputTokens != 40 || first.CacheRead != 800 || first.CacheWrite != 150 {
		t.Errorf("round 1 cost %+v, want its own completion's counts", first)
	}
	if first.ToolCalls != 2 || second.ToolCalls != 0 {
		t.Errorf("tool calls per round = %d, %d, want 2, 0", first.ToolCalls, second.ToolCalls)
	}

	// THE ONE PRODUCER OF CACHE TOKENS: the totals are the rounds' own, and
	// a breakdown of InputTokens rather than an addition to it.
	if res.CacheRead != 1900 || res.CacheWrite != 150 {
		t.Errorf("cache = %d read / %d write, want 1900 / 150", res.CacheRead, res.CacheWrite)
	}
	if res.InputTokens != 2300 {
		t.Errorf("input = %d, want 2300 — the cache must not be added to it", res.InputTokens)
	}
	if res.MaxRounds != 6 {
		t.Errorf("MaxRounds = %d, want the cap the loop ran under echoed (6)", res.MaxRounds)
	}
}

func TestAToolCallIsTimedAndNamesItsOrigin(t *testing.T) {
	t.Parallel()
	const delay = 15 * time.Millisecond
	p := &scriptedProvider{turns: []llm.Completion{
		{Content: "reading", ToolCalls: []llm.ToolCall{toolCall("1", "read"), toolCall("2", "write")}},
		{Content: "done"},
	}}
	s := &timedSurface{delay: delay, origin: "mcp:github", server: "github"}

	res, err := toolloop.Run(t.Context(), toolloop.Config{Provider: p, Surface: s, MaxRounds: 4})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Executions) != 2 {
		t.Fatalf("executions = %+v, want two", res.Executions)
	}
	round := res.Rounds[0]
	modelEnded := round.StartedAt.Add(round.Duration)
	for _, ex := range res.Executions {
		if ex.Duration < delay {
			t.Errorf("%s took %v, want at least the surface's %v", ex.Name, ex.Duration, delay)
		}
		if ex.StartedAt.Location() != time.UTC {
			t.Errorf("%s started at %v, want UTC", ex.Name, ex.StartedAt)
		}
		// SERIAL, after the model's half of the round.
		if ex.StartedAt.Before(modelEnded) {
			t.Errorf("%s began at %v, before its round's model call ended at %v", ex.Name, ex.StartedAt, modelEnded)
		}
		if ex.Origin != "mcp:github" || ex.Server != "github" {
			t.Errorf("%s names origin %q server %q, want the surface's mcp:github / github", ex.Name, ex.Origin, ex.Server)
		}
	}
	a, b := res.Executions[0], res.Executions[1]
	if b.StartedAt.Before(a.StartedAt.Add(a.Duration)) {
		t.Errorf("the second call began at %v, before the first ended (%v + %v)", b.StartedAt, a.StartedAt, a.Duration)
	}
}

func TestTheRunningCallIsPublishedBeforeTheCallAndClearedAfter(t *testing.T) {
	t.Parallel()
	p := &scriptedProvider{turns: []llm.Completion{
		{Content: "reading", ToolCalls: []llm.ToolCall{
			{ID: "1", Name: "read", Arguments: map[string]any{"path": "a.go"}},
			{ID: "2", Name: "write", Arguments: map[string]any{"path": "b.go"}},
		}},
		{Content: "done"},
	}}
	s := &timedSurface{}
	var frames []toolloop.Result
	res, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: s, MaxRounds: 4,
		OnProgress: func(r toolloop.Result) {
			frames = append(frames, r)
			s.latest = &frames[len(frames)-1]
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(s.atCall) != 2 {
		t.Fatalf("saw %d frames at call time, want one per call", len(s.atCall))
	}
	for i, want := range []struct {
		name, path string
		done       int
	}{{"read", "a.go", 0}, {"write", "b.go", 1}} {
		frame := s.atCall[i]
		running := frame.Running
		if running == nil {
			t.Fatalf("call %q reached the tool before any frame named it running", want.name)
		}
		if running.Name != want.name || running.Args["path"] != want.path || running.Round != 1 {
			t.Errorf("the frame before %q named %+v", want.name, running)
		}
		if running.StartedAt.IsZero() || running.StartedAt.Location() != time.UTC {
			t.Errorf("the running call's start is %v, want a UTC instant", running.StartedAt)
		}
		// The frame is the loop's whole view AT that instant: the calls
		// before this one have answered, this one has not.
		if len(frame.Executions) != want.done {
			t.Errorf("the frame before %q carried %d executions, want %d", want.name, len(frame.Executions), want.done)
		}
		// And the round's own clock, so "this round has taken 40s" has
		// something to count from.
		if !frame.RoundStartedAt.Equal(res.Rounds[0].StartedAt) {
			t.Errorf("the frame's round started at %v, want round 1's %v", frame.RoundStartedAt, res.Rounds[0].StartedAt)
		}
	}
	// CLEARED once the call returns: the next frame after the round's
	// tools names nothing, and neither does the finished result.
	afterTools := frames[3]
	if afterTools.Running != nil || len(afterTools.Executions) != 2 {
		t.Errorf("the frame after the round's tools = running %+v, %d executions; want none running, 2 done",
			afterTools.Running, len(afterTools.Executions))
	}
	if res.Running != nil || !res.RoundStartedAt.IsZero() {
		t.Errorf("a finished result carries live-only state: running %+v, round start %v", res.Running, res.RoundStartedAt)
	}
}

func TestACallIsToldItsRoundOnThePhasesScale(t *testing.T) {
	t.Parallel()
	p := &scriptedProvider{turns: []llm.Completion{
		{Content: "one", ToolCalls: []llm.ToolCall{toolCall("1", "read")}},
		{Content: "two", ToolCalls: []llm.ToolCall{toolCall("2", "read")}},
		{Content: "done"},
	}}
	s := &timedSurface{}
	// A phase already twenty rounds in — an extension continuing it.
	ctx := toolloop.WithRoundOffset(t.Context(), 20)
	if _, err := toolloop.Run(ctx, toolloop.Config{Provider: p, Surface: s, MaxRounds: 4}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(s.rounds) != 2 || s.rounds[0] != 21 || s.rounds[1] != 22 {
		t.Errorf("calls were told rounds %v, want [21 22] — the phase's numbers, not the invocation's", s.rounds)
	}
	if _, ok := toolloop.CallRound(t.Context()); ok {
		t.Error("a context no loop stamped answers a round")
	}
}

func TestARefusedChargeLeavesItsRoundOnTheFailureRecord(t *testing.T) {
	t.Parallel()
	// The provider billed the round before the company's meter refused it,
	// and the refusal is what the failure record exists to explain — so the
	// round it refused has to be on it, not only the ones before.
	p := &scriptedProvider{turns: []llm.Completion{
		{Content: "one", ToolCalls: []llm.ToolCall{toolCall("1", "read")}, InputTokens: 100, OutputTokens: 10},
		{Content: "two", InputTokens: 500, OutputTokens: 50, CacheRead: 400},
	}}
	progress := &toolloop.Progress{}
	_, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: &fakeSurface{tools: []llm.ToolDef{def("read")}}, MaxRounds: 4,
		Budget: &meter{refuseAt: 200}, Progress: progress,
	})
	if !errors.Is(err, toolloop.ErrBudgetExhausted) {
		t.Fatalf("err = %v, want the budget refusal", err)
	}
	snap := progress.Snapshot()
	if snap.RoundsUsed != 2 || len(snap.Rounds) != 2 {
		t.Errorf("the failure record states %d rounds (%d timed), want the refused round 2 on it",
			snap.RoundsUsed, len(snap.Rounds))
	}
	if snap.InputTokens != 600 || snap.CacheRead != 400 {
		t.Errorf("the failure record billed %d input / %d cached, want 600 / 400", snap.InputTokens, snap.CacheRead)
	}
}

// THE PHASE NAMES THE ENTRY THAT SERVED IT, by the model's own precedence.
//
// Config.ProviderKey is the configured head, standing in the way
// Provider.Model() stands in for the model; the first completion that names an
// entry replaces it, and a later round served by another entry does not —
// exactly as the phase's model latches. A completion from a bare backend names
// no entry, so the head stays the answer rather than going blank.
func TestThePhaseNamesTheEntryThatServedIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		turns []llm.Completion
		want  string
	}{
		{name: "the completion names the entry", want: "backup", turns: []llm.Completion{
			{Model: "m1", ProviderKey: "backup", ToolCalls: []llm.ToolCall{toolCall("1", "read")}},
			{Model: "m2", ProviderKey: "third", Content: "done"},
		}},
		{name: "a bare backend names none", want: "head", turns: []llm.Completion{
			{Model: "m1", Content: "done"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &scriptedProvider{turns: tc.turns}
			progress := &toolloop.Progress{}
			res, err := toolloop.Run(t.Context(), toolloop.Config{
				Provider: p, ProviderKey: "head", Surface: &timedSurface{},
				MaxRounds: 6, Progress: progress,
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.ProviderKey != tc.want {
				t.Errorf("ProviderKey = %q, want %q", res.ProviderKey, tc.want)
			}
			// The live snapshot a failure record is built from agrees.
			if got := progress.Snapshot().ProviderKey; got != tc.want {
				t.Errorf("the snapshot names %q, want %q", got, tc.want)
			}
		})
	}
}
