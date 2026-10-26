package tools_test

import (
	"context"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools"
)

// ordinalTool reads the run's call count the way a write deriving its
// operation id does, and holds the calls a case marks until it releases them.
type ordinalTool struct {
	mu     sync.Mutex
	counts map[string]int
	// entered is told each held call's value once its count is read, and
	// release lets it return.
	entered chan string
	release chan struct{}
}

func (o *ordinalTool) Name() string               { return "update" }
func (o *ordinalTool) Description() string        { return "updates an item" }
func (o *ordinalTool) Parameters() map[string]any { return nil }
func (o *ordinalTool) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{Output: "no turn"}, nil
}

func (o *ordinalTool) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	v, _ := args["status"].(string)
	n := turn.CallLog().Ordinal(ctx, o.Name(), args)
	o.mu.Lock()
	o.counts[v] = n
	o.mu.Unlock()
	if hold, _ := args["hold"].(bool); hold {
		o.entered <- v
		<-o.release
	}
	return tools.Result{Output: "updated"}, nil
}

// A CALL MADE WHILE ANOTHER IS STILL IN FLIGHT COUNTS IT. An executor's calls
// arrive over the bridge, each on its own goroutine, so a run's X, Y, X to one
// tool can have the second X made while Y has not returned. A count read
// before an in-flight call was recorded gave the second X its first copy's
// operation id — answered as that call's retry, its write skipped with a
// success. Each call takes its place in the run's log as it is made, so the
// second X counts the Y that came before it, finished or not — and Y, made
// before the second X, does not count it.
func TestACallCountsTheCallsStillInFlightBeforeIt(t *testing.T) {
	t.Parallel()
	tool := &ordinalTool{counts: map[string]int{},
		entered: make(chan string, 1), release: make(chan struct{})}
	r := tools.NewRegistry()
	mustRegister(t, r, tool, tools.OriginBuiltin)
	turn := &turnctx.Turn{Calls: turnctx.NewCallLog()}
	s := tools.NewSurface("execute", r.Snapshot(), []string{"update"}).ForTurn(turn)
	call := func(args map[string]any) llm.ToolCall {
		return llm.ToolCall{ID: "c", Name: "update", Arguments: args}
	}

	if _, err := s.Execute(t.Context(), call(map[string]any{"status": "in_progress"})); err != nil {
		t.Fatalf("first call: %v", err)
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		if _, err := s.Execute(t.Context(), call(map[string]any{"status": "done", "hold": true})); err != nil {
			t.Errorf("the held call: %v", err)
		}
	})
	<-tool.entered
	// The held call has read its count and not returned: the second X is made
	// beside it.
	if _, err := s.Execute(t.Context(), call(map[string]any{"status": "in_progress"})); err != nil {
		t.Fatalf("the call made again: %v", err)
	}
	close(tool.release)
	wg.Wait()

	tool.mu.Lock()
	defer tool.mu.Unlock()
	if got := tool.counts["in_progress"]; got != 1 {
		t.Errorf("the call made again beside an in-flight different one counted %d, want 1: "+
			"it would take its first copy's operation id and be skipped as a retry", got)
	}
	if got := tool.counts["done"]; got != 1 {
		t.Errorf("the in-flight call counted %d, want the 1 call before it and not the one after", got)
	}
}
