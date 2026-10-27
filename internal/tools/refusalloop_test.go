package tools_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/tools"
)

// policyTool refuses on its policy while refusing is set, per target.
type policyTool struct {
	name    string
	refuse  atomic.Bool
	calls   atomic.Int64
	targets bool
}

func (p *policyTool) Name() string               { return p.name }
func (p *policyTool) Description() string        { return "writes a thing" }
func (p *policyTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (p *policyTool) RefusalTarget(args map[string]any) string {
	if !p.targets {
		return ""
	}
	id, _ := args["artifact"].(string)
	return id
}
func (p *policyTool) Call(context.Context, map[string]any) (tools.Result, error) {
	p.calls.Add(1)
	if p.refuse.Load() {
		return tools.Result{Output: "refused: fix the index", Failed: true, Cause: tools.RefusalInvalid}, nil
	}
	return tools.Result{Output: "published"}, nil
}

// countingRecorder counts what a surface records.
type countingRecorder struct {
	mu sync.Mutex
	n  map[string]uint64
}

func (c *countingRecorder) Add(name string, n uint64, attrs metrics.Attrs) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[string]uint64{}
	}
	c.n[name+"|"+attrs["tool"]] += n
}

func (c *countingRecorder) count(name, tool string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[name+"|"+tool]
}

func surfaceOf(t *testing.T, tool *policyTool, ann tools.Annotations) (*tools.Surface, *countingRecorder) {
	t.Helper()
	r := tools.NewRegistry()
	if err := r.RegisterWith(tool, tools.OriginBuiltin, ann); err != nil {
		t.Fatal(err)
	}
	rec := &countingRecorder{}
	return tools.NewSurface("execute", r.Snapshot(), []string{tool.name}).WithCounter(rec), rec
}

func call(t *testing.T, s *tools.Surface, name, args string) string {
	t.Helper()
	res, err := s.Execute(context.Background(), llm.ToolCall{Name: name, Arguments: decodeArgs(t, args)})
	if err != nil {
		t.Fatal(err)
	}
	return res.Output
}

func decodeArgs(t *testing.T, raw string) map[string]any {
	t.Helper()
	if raw == "" {
		return nil
	}
	out := tools.ReadArgs(raw)
	if out == nil {
		t.Fatalf("fixture arguments are not JSON: %s", raw)
	}
	return out
}

var write = tools.Annotations{ReadOnly: mcp.No, OpenWorld: mcp.Yes}

// A RUN OF POLICY REFUSALS IS STOPPED IN PLAIN WORDS, from the limit on: the
// refusal is answered whole, the call still runs (never a silent cap), the
// instruction is conditional on a work item, and the loop is logged and
// counted ONCE however long it runs (ADR-0059).
//
// Mutation: count every answer past the limit and the counter reads 2.
func TestARunOfPolicyRefusalsIsStoppedInWordsAndCountedOnce(t *testing.T) {
	t.Parallel()
	tool := &policyTool{name: "create_thing"}
	tool.refuse.Store(true)
	s, rec := surfaceOf(t, tool, write)
	for i := 1; i <= tools.PolicyRefusalLimit+1; i++ {
		out := call(t, s, "create_thing", "")
		stopped := strings.Contains(out, "STOP RETRYING create_thing")
		if want := i >= tools.PolicyRefusalLimit; stopped != want {
			t.Errorf("attempt %d: told to stop %v, want %v: %q", i, stopped, want, out)
		}
		if !strings.HasPrefix(out, "refused: fix the index") {
			t.Errorf("attempt %d: the refusal was not answered whole: %q", i, out)
		}
		if stopped && (!strings.Contains(out, "If you are working on a work item, raise a blocker on it; otherwise tell whoever asked") ||
			strings.Contains(out, "in this turn")) {
			t.Errorf("the instruction is not conditional, or names a turn it does not count: %q", out)
		}
	}
	if got := tool.calls.Load(); got != tools.PolicyRefusalLimit+1 {
		t.Errorf("the tool ran %d times, want every call run", got)
	}
	if got := rec.count(metrics.ToolPolicyRefusalLoops, "create_thing"); got != 1 {
		t.Errorf("the loop was counted %d times, want once", got)
	}
	// THE LEDGER RECORDS WHAT THE MODEL WAS TOLD.
	calls := s.Calls()
	if !strings.Contains(calls[len(calls)-1].Output, "STOP RETRYING") {
		t.Errorf("the ledger's last call is %q, want the answer the model read", calls[len(calls)-1].Output)
	}
}

// THE RUN IS CONSECUTIVE: three refusals, a success, then one more refusal is
// no loop; a run that starts again and reaches the limit is a second loop.
//
// Mutation: count every refusal in the phase and the fourth is told to stop.
func TestASuccessEndsTheRun(t *testing.T) {
	t.Parallel()
	tool := &policyTool{name: "create_thing"}
	s, rec := surfaceOf(t, tool, write)
	tool.refuse.Store(true)
	for range tools.PolicyRefusalLimit - 1 {
		call(t, s, "create_thing", "")
	}
	tool.refuse.Store(false)
	call(t, s, "create_thing", "")
	tool.refuse.Store(true)
	if out := call(t, s, "create_thing", ""); strings.Contains(out, "STOP RETRYING") {
		t.Errorf("a refusal after a success was told to stop: %q", out)
	}
	for range tools.PolicyRefusalLimit - 1 {
		call(t, s, "create_thing", "")
	}
	if got := rec.count(metrics.ToolPolicyRefusalLoops, "create_thing"); got != 1 {
		t.Errorf("the second run reaching the limit was counted %d times, want once", got)
	}
}

// THE RUN IS PER TARGET: refusals across four artifacts are four first
// attempts, and a success on one artifact does not end the run on another.
//
// Mutation: key the run by tool alone and the fourth artifact is told to stop.
func TestARunIsCountedPerTarget(t *testing.T) {
	t.Parallel()
	tool := &policyTool{name: "publish_thing", targets: true}
	tool.refuse.Store(true)
	s, _ := surfaceOf(t, tool, write)
	for _, id := range []string{"a", "b", "c", "d"} {
		if out := call(t, s, "publish_thing", `{"artifact":"`+id+`"}`); strings.Contains(out, "STOP") {
			t.Errorf("a first refusal on %s was told to stop: %q", id, out)
		}
	}
	for range tools.PolicyRefusalLimit - 2 {
		call(t, s, "publish_thing", `{"artifact":"a"}`)
	}
	tool.refuse.Store(false)
	call(t, s, "publish_thing", `{"artifact":"b"}`)
	tool.refuse.Store(true)
	out := call(t, s, "publish_thing", `{"artifact":"a"}`)
	if !strings.Contains(out, "STOP RETRYING publish_thing on a.") {
		t.Errorf("the fourth refusal in a row on a was not told to stop: %q", out)
	}
}

// A READ IS NEVER A LOOP, and another refusal class neither extends nor ends a
// run.
func TestAReadIsNeverALoop(t *testing.T) {
	t.Parallel()
	tool := &policyTool{name: "read_thing"}
	tool.refuse.Store(true)
	s, _ := surfaceOf(t, tool, tools.Annotations{ReadOnly: mcp.Yes})
	for range tools.PolicyRefusalLimit + 2 {
		if out := call(t, s, "read_thing", ""); strings.Contains(out, "STOP") {
			t.Fatalf("a read was told to stop: %q", out)
		}
	}
}

// THE COUNT AND THE RECORD ARE ONE STEP: calls answered side by side (a
// response's calls run at once) cannot read the same run length, so exactly
// the calls from the limit on are told to stop, and the loop is counted once.
//
// Mutation: read the run outside the lock that records the call, and two
// parallel refusals tell the same number, under -race.
func TestParallelRefusalsCountEachCallOnce(t *testing.T) {
	t.Parallel()
	tool := &policyTool{name: "create_thing"}
	tool.refuse.Store(true)
	s, rec := surfaceOf(t, tool, write)
	const n = 32
	var wg sync.WaitGroup
	var stops atomic.Int64
	for range n {
		wg.Go(func() {
			if strings.Contains(call(t, s, "create_thing", ""), "STOP RETRYING") {
				stops.Add(1)
			}
		})
	}
	wg.Wait()
	if want := int64(n - tools.PolicyRefusalLimit + 1); stops.Load() != want {
		t.Errorf("%d answers said stop, want %d", stops.Load(), want)
	}
	if got := rec.count(metrics.ToolPolicyRefusalLoops, "create_thing"); got != 1 {
		t.Errorf("the loop was counted %d times, want once", got)
	}
}
