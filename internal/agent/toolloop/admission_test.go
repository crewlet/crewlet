package toolloop_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/tools"
)

// countingWrite is a seat tool that reports the repeat count its call reads
// off the run's call log — the count a derived operation id carries — by the
// call's "value". A call whose value is "hold" blocks until another call has
// read its own count, so the two run side by side and return in the opposite
// order to the one they arrived in.
type countingWrite struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once

	mu   sync.Mutex
	seen map[string]int
}

func newCountingWrite() *countingWrite {
	return &countingWrite{entered: make(chan struct{}), release: make(chan struct{}),
		seen: map[string]int{}}
}

func (w *countingWrite) Name() string        { return "write" }
func (w *countingWrite) Description() string { return "writes a value" }
func (w *countingWrite) Parameters() map[string]any {
	return map[string]any{"type": "object",
		"properties": map[string]any{"value": map[string]any{"type": "string"}}}
}

func (w *countingWrite) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{Output: "unreachable: a seat tool is called for its turn", Failed: true}, nil
}

func (w *countingWrite) CallForTurn(_ context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {
	value, _ := args["value"].(string)
	if value == "hold" {
		close(w.entered)
		<-w.release
	}
	n := turn.CallLog().Ordinal(w.Name(), args)
	w.mu.Lock()
	w.seen[value] = n
	w.mu.Unlock()
	if value != "hold" {
		w.once.Do(func() { close(w.release) })
	}
	return tools.Result{Output: fmt.Sprint(n)}, nil
}

func (w *countingWrite) counts() map[string]int {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]int, len(w.seen))
	for k, v := range w.seen {
		out[k] = v
	}
	return out
}

// CALLS MADE AT ONCE COUNT IN THE ORDER THEY ARRIVED. A CLI makes a
// response's calls side by side and they return in whatever order their tools
// finish, so each call's place in the run's call log — the repeat count an
// operation id carries — is fixed when the loop ADMITS it, in arrival order,
// not when it returns: the call that arrived first counts nothing after it,
// even once that later call has returned, and the later one counts the first
// though it is still running. A re-run making the same calls then derives the
// same ids, whichever tool is faster this time.
//
// Mutation: run each call through the surface's own Execute (the call logged
// when it returns), and the second call counts nothing before it while the
// first counts the second.
func TestCallsMadeAtOnceCountInTheOrderTheyArrived(t *testing.T) {
	t.Parallel()
	write := newCountingWrite()
	seat := &org.Role{Name: "Engineer", DeclaredHandle: "dev"}
	turn := &turnctx.Turn{RunID: "run-1", Seat: seat,
		Org:   &org.Organization{Name: "Nimbus", Roles: []*org.Role{seat}},
		Calls: turnctx.NewCallLog()}
	reg := tools.NewRegistry()
	if err := reg.Register(write, tools.OriginBuiltin); err != nil {
		t.Fatalf("Register: %v", err)
	}
	surf := tools.NewSurface("execute", reg.Snapshot(), []string{"write"}).ForTurn(turn)

	prov := &llmtest.Script{Concurrency: llm.CallsAtOnce, Runs: []llmtest.Run{{Responses: []llmtest.Response{
		{Text: "writing both", Parallel: true, Calls: []llmtest.Call{
			{Name: "write", Args: `{"value":"hold"}`},
			// Reaches the host only once the first call is running.
			{Name: "write", Args: `{"value":"other"}`, Wait: write.entered},
		}},
		{Text: "done"},
	}}}}
	if _, err := run(t, toolloop.Config{Provider: prov, Surface: surf}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := write.counts()
	if got["other"] != 1 {
		t.Errorf("the second call counts %d different calls before it, want 1: the first, "+
			"admitted before it and still running", got["other"])
	}
	if got["hold"] != 0 {
		t.Errorf("the first call counts %d, want 0: it counted a call that arrived after it",
			got["hold"])
	}
	// AND BOTH ARE IN THE RUN'S LOG once they have returned, so a call after
	// them counts both.
	if n := turn.CallLog().Ordinal("write", map[string]any{"value": "third"}); n != 2 {
		t.Errorf("a later call counts %d calls before it, want both of the response's", n)
	}
}
