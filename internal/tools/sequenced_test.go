package tools_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools"
)

// seatTool is a seat-callable tool that keeps the Turn each call was handed,
// and, when gate is set, holds its FIRST call until gate is closed.
type seatTool struct {
	name string

	mu      sync.Mutex
	handed  []*turnctx.Turn
	entered chan int // receives the call's number as it starts, when set
	gate    chan struct{}
}

func (s *seatTool) Name() string               { return s.name }
func (s *seatTool) Description() string        { return s.name }
func (s *seatTool) Parameters() map[string]any { return nil }
func (s *seatTool) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return s.CallForTurn(ctx, nil, args)
}

func (s *seatTool) CallForTurn(_ context.Context, turn *turnctx.Turn,
	_ map[string]any) (tools.Result, error) {

	s.mu.Lock()
	s.handed = append(s.handed, turn)
	n := len(s.handed)
	s.mu.Unlock()
	if s.entered != nil {
		s.entered <- n
	}
	if n == 1 && s.gate != nil {
		<-s.gate
	}
	return tools.Result{Output: s.name}, nil
}

// turns is every Turn a call was handed, in the order the calls started.
func (s *seatTool) turns() []*turnctx.Turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.handed)
}

// sequencedTool is a seatTool that names its writes after the calls before it.
type sequencedTool struct{ seatTool }

func (*sequencedTool) Sequenced() {}

var _ tools.Sequenced = (*sequencedTool)(nil)

// callNames are the names of calls, in order.
func callNames(calls []ledger.Call) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Name)
	}
	return out
}

// A CALL IS HANDED EVERY CALL BEFORE IT: what the bound Turn already carried,
// then what this surface has recorded since.
//
// The native tracker's writes are named after the writes before them in the
// turn, and a surface hands a tool the Turn it was bound to — whose calls stop
// where the phase began. A write handed only that would be named as though the
// phase had made no write before it, and the operation ledger would answer a
// second write to one object `applied` without writing it.
//
// Mutation: hand the bound Turn itself, or drop what it already carried, and
// this fails.
func TestACallIsHandedEveryCallBeforeIt(t *testing.T) {
	t.Parallel()
	r := tools.NewRegistry()
	mustRegister(t, r, tool("read"), tools.OriginBuiltin)
	write := &seatTool{name: "write"}
	mustRegister(t, r, write, tools.OriginBuiltin)
	bound := (&turnctx.Turn{RunID: "run-1"}).
		WithEarlier([]ledger.Call{{Name: "before-the-phase"}})
	s := tools.NewSurface("execute", r.Snapshot(), []string{"read", "write"}).
		ForTurn(bound)

	ctx := context.Background()
	for _, name := range []string{"read", "write", "write"} {
		if _, err := s.Execute(ctx, llm.ToolCall{Name: name}); err != nil {
			t.Fatalf("Execute %s: %v", name, err)
		}
	}
	handed := write.turns()
	if len(handed) != 2 {
		t.Fatalf("the tool was handed %d turns, want 2", len(handed))
	}
	for i, want := range [][]string{
		{"before-the-phase", "read"},
		{"before-the-phase", "read", "write"},
	} {
		if got := callNames(handed[i].Calls()); !slices.Equal(got, want) {
			t.Errorf("call %d was handed %v, want %v", i+1, got, want)
		}
	}
	// AND THE CALL WAS HANDED ITS OWN SNAPSHOT: the Turn the surface is
	// bound to is not what grows.
	if got := callNames(bound.Calls()); !slices.Equal(got, []string{"before-the-phase"}) {
		t.Errorf("the bound turn now carries %v — the surface wrote into it", got)
	}
	if handed[0].RunID != "run-1" {
		t.Errorf("the handed turn lost its identity: run %q", handed[0].RunID)
	}
}

// TWO SEQUENCED CALLS NEVER RUN AT ONCE, and the second is handed the first.
//
// A surface CAN run two calls at once — the MCP bridge executes each call a
// coding agent sends as it arrives. Two writes in flight together would each
// be handed the same earlier calls and name themselves alike, and the ledger
// collapses the second into the first.
//
// Mutation: take the sequence lock only around the invocation rather than
// across the record, or not at all, and the second call is handed a list
// without the first.
func TestTwoSequencedCallsNeverRunAtOnce(t *testing.T) {
	t.Parallel()
	r := tools.NewRegistry()
	write := &sequencedTool{seatTool{
		name: "write", entered: make(chan int, 2), gate: make(chan struct{}),
	}}
	mustRegister(t, r, write, tools.OriginBuiltin)
	mustRegister(t, r, tool("read"), tools.OriginBuiltin)
	s := tools.NewSurface("execute", r.Snapshot(), []string{"write", "read"}).
		ForTurn(&turnctx.Turn{RunID: "run-1"})

	ctx := context.Background()
	var wg sync.WaitGroup
	execute := func() {
		defer wg.Done()
		if _, err := s.Execute(ctx, llm.ToolCall{Name: "write"}); err != nil {
			t.Errorf("Execute: %v", err)
		}
	}
	wg.Add(1)
	go execute()
	if n := <-write.entered; n != 1 {
		t.Fatalf("the first call to start was call %d", n)
	}
	wg.Add(1)
	go execute()
	select {
	case <-write.entered:
		t.Error("the second write started while the first was still running")
	case <-time.After(100 * time.Millisecond):
	}

	// AN ORDINARY TOOL IS NOT HELD BEHIND A WRITE: a read serialised
	// behind one would only be slower.
	done := make(chan error, 1)
	go func() {
		_, err := s.Execute(ctx, llm.ToolCall{Name: "read"})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Execute read: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("a read waited behind a write that was still running")
	}

	close(write.gate)
	wg.Wait()
	handed := write.turns()
	if len(handed) != 2 {
		t.Fatalf("the tool was handed %d turns, want 2", len(handed))
	}
	if !slices.Contains(callNames(handed[1].Calls()), "write") {
		t.Errorf("the second write was handed %v, without the first — it names "+
			"itself as the first did", callNames(handed[1].Calls()))
	}
}
