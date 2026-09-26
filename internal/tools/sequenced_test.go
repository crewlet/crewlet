package tools

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// INSIDE THE PACKAGE, because one of these asserts on the surface's own
// locks: whether the sequence is still held while a call's record is waiting
// is not visible from outside, and a lock released before the record passes
// every outside test that does not happen to lose that race.

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
func (s *seatTool) Call(ctx context.Context, args map[string]any) (Result, error) {
	return s.CallForTurn(ctx, nil, args)
}

func (s *seatTool) CallForTurn(_ context.Context, turn *turnctx.Turn,
	_ map[string]any) (Result, error) {

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
	return Result{Output: s.name}, nil
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

var _ Sequenced = (*sequencedTool)(nil)

// readTool is an ordinary tool, answering at once.
type readTool struct{ name string }

func (r readTool) Name() string               { return r.name }
func (r readTool) Description() string        { return r.name }
func (r readTool) Parameters() map[string]any { return nil }
func (r readTool) Call(context.Context, map[string]any) (Result, error) {
	return Result{Output: "ok"}, nil
}

// registered is a registry holding tools, each registered as a builtin.
func registered(t *testing.T, tools ...Callable) *Registry {
	t.Helper()
	r := NewRegistry()
	for _, tool := range tools {
		if err := r.Register(tool, OriginBuiltin); err != nil {
			t.Fatalf("Register(%s): %v", tool.Name(), err)
		}
	}
	return r
}

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
	write := &seatTool{name: "write"}
	r := registered(t, readTool{name: "read"}, write)
	bound := (&turnctx.Turn{RunID: "run-1"}).
		WithEarlier([]ledger.Call{{Name: "before-the-phase"}})
	s := NewSurface("execute", r.Snapshot(), []string{"read", "write"}).
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
// collapses the second into the first. A read is not held behind a write:
// serialised behind one it would only be slower.
//
// Mutation: take no sequence lock, or take it for every tool, and this fails.
// How far the lock reaches is [TestTheSequenceIsHeldUntilTheCallIsRecorded]'s.
func TestTwoSequencedCallsNeverRunAtOnce(t *testing.T) {
	t.Parallel()
	write := &sequencedTool{seatTool{
		name: "write", entered: make(chan int, 2), gate: make(chan struct{}),
	}}
	r := registered(t, write, readTool{name: "read"})
	s := NewSurface("execute", r.Snapshot(), []string{"write", "read"}).
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

// sequenceWatch is how long [TestTheSequenceIsHeldUntilTheCallIsRecorded]
// watches for a release that must not come.
//
// A surface that releases before the record does so the moment the call
// returns, so a release shows within microseconds of the gate opening, and a
// quarter of a second is thousands of times that. A surface that holds the
// sequence never releases while the record is blocked, however long this is.
const sequenceWatch = 250 * time.Millisecond

// A SEQUENCED CALL HOLDS THE SEQUENCE UNTIL IT IS RECORDED, not only while it
// runs.
//
// The next sequenced call is handed the calls recorded before it, and this one
// is not among them until it is recorded: a sequence released when the call
// returns lets the next one start in between and be handed a list without it,
// which names two writes as one. The window is a few instructions wide, so a
// test that only runs two calls passes whether it is closed or not. This one
// holds the surface's own state lock, which the record needs, while the call
// returns — so the record cannot land, and a sequence that is free then was
// released before it.
//
// Mutation: release the sequence after the invocation rather than after the
// record, or take none, and the sequence is free while the record waits.
func TestTheSequenceIsHeldUntilTheCallIsRecorded(t *testing.T) {
	t.Parallel()
	write := &sequencedTool{seatTool{
		name: "write", entered: make(chan int, 1), gate: make(chan struct{}),
	}}
	s := NewSurface("execute", registered(t, write).Snapshot(), []string{"write"}).
		ForTurn(&turnctx.Turn{RunID: "run-1"})

	done := make(chan error, 1)
	go func() {
		_, err := s.Execute(context.Background(), llm.ToolCall{Name: "write"})
		done <- err
	}()
	<-write.entered

	s.mu.Lock()
	close(write.gate)
	released := false
	for deadline := time.Now().Add(sequenceWatch); time.Now().Before(deadline); {
		if s.sequence.TryLock() {
			s.sequence.Unlock()
			released = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	recorded := len(s.called)
	s.mu.Unlock()

	if err := <-done; err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if released {
		t.Errorf("the sequence was free while the call's record was waiting (%d "+
			"recorded), so the next sequenced call could be handed a list "+
			"without it", recorded)
	}
	if got := s.CalledNames(); !slices.Equal(got, []string{"write"}) {
		t.Errorf("the surface recorded %v, want the one write", got)
	}
}
