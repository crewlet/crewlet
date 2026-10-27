package tools_test

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools"
)

// statusSchema closes `status` to a set, so " Draft" is the member "draft".
var statusSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"item":   map[string]any{"type": "string"},
		"status": map[string]any{"type": "string", "enum": []any{"draft", "published"}},
	},
}

// opTool writes the way the builtins do: its operation id is a digest of the
// arguments it was handed and its repeat count in the run
// ([turnctx.CallLog.Ordinal]), and a write under an id already written is the
// same write.
type opTool struct {
	mu       sync.Mutex
	ordinals []int
	writes   map[string]int
}

func (*opTool) Name() string               { return "set_status" }
func (*opTool) Description() string        { return "sets a status" }
func (*opTool) Parameters() map[string]any { return statusSchema }
func (o *opTool) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return o.CallForTurn(ctx, nil, args)
}

func (o *opTool) CallForTurn(_ context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	n := turn.CallLog().Ordinal("set_status", args)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ordinals = append(o.ordinals, n)
	if o.writes == nil {
		o.writes = map[string]int{}
	}
	o.writes[turnctx.ArgsDigest(args)+"#"+strconv.Itoa(n)]++
	return tools.Result{Output: "applied"}, nil
}

func opSurface(t *testing.T, tool tools.Callable) *tools.Surface {
	t.Helper()
	r := tools.NewRegistry()
	if err := r.RegisterWith(tool, tools.OriginBuiltin, tools.Annotations{ReadOnly: mcp.No}); err != nil {
		t.Fatal(err)
	}
	turn := &turnctx.Turn{RunID: "run-1", Calls: turnctx.NewCallLog()}
	return tools.NewSurface("execute", r.Snapshot(), []string{tool.Name()}).ForTurn(turn)
}

// spellings are one call written three ways a model writes it again.
var spellings = []string{`"draft"`, `"Draft"`, `" draft "`}

// A RETRY SPELT ANOTHER WAY IS THE SAME WRITE (#423 re-verify): the canonical
// arguments are made once, at the surface's entry, and the run's call log,
// the repeat count and the tool all read them. When the form was made in the
// middle, the log recorded "Draft" as sent while the tool counted "draft",
// so the retry took a fresh repeat count, a fresh operation id, and wrote
// twice. Both entries: a call made on its own ([tools.Surface.Execute]) and
// one admitted beside others ([tools.Surface.Admit]).
//
// Mutation: record the call in the log by the arguments as sent and the
// ordinals read [0 1 1], three writes.
func TestARetrySpeltAnotherWayIsTheSameWrite(t *testing.T) {
	t.Parallel()
	for _, admitted := range []bool{false, true} {
		tool := &opTool{}
		s := opSurface(t, tool)
		for _, status := range spellings {
			call := llm.ToolCall{Name: "set_status",
				Arguments: decodeArgs(t, `{"item": "GP-12", "status": `+status+`}`)}
			var err error
			if admitted {
				_, err = s.Admit(call.Name, call.Arguments)(context.Background(), call)
			} else {
				_, err = s.Execute(context.Background(), call)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		for i, n := range tool.ordinals {
			if n != 0 {
				t.Errorf("admitted %v: %s took repeat count %d, want 0: ordinals %v",
					admitted, spellings[i], n, tool.ordinals)
			}
		}
		if len(tool.writes) != 1 {
			t.Errorf("admitted %v: three spellings of one call wrote %d operations, want 1: %v",
				admitted, len(tool.writes), tool.writes)
		}
	}
}

// targetTool refuses on its policy, and names its refusals' target by a value
// a closed set holds: the kind of thing it writes.
type targetTool struct{ policyTool }

func (*targetTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"kind": map[string]any{"type": "string", "enum": []any{"brand", "deck"}},
	}}
}

func (*targetTool) RefusalTarget(args map[string]any) string {
	kind, _ := args["kind"].(string)
	return kind
}

// A LOOP SPELT SEVERAL WAYS IS STILL ONE LOOP: a model that alternates the
// spelling of a target is repeating itself, and the refusal-loop guard reads
// the target from the canonical arguments, so the run is counted and the
// model told to stop at the limit as it would be for one spelling.
//
// Mutation: read the target from the arguments as sent and every attempt
// starts a run of its own.
func TestALoopSpeltSeveralWaysIsStillOneLoop(t *testing.T) {
	t.Parallel()
	tool := &targetTool{policyTool{name: "publish_kind", targets: true}}
	tool.refuse.Store(true)
	r := tools.NewRegistry()
	if err := r.RegisterWith(tool, tools.OriginBuiltin, write); err != nil {
		t.Fatal(err)
	}
	s := tools.NewSurface("execute", r.Snapshot(), []string{tool.name})
	kinds := []string{`"brand"`, `"Brand"`, `" BRAND "`, `"brand "`}
	for i := 1; i <= tools.PolicyRefusalLimit; i++ {
		out := call(t, s, "publish_kind", `{"kind": `+kinds[(i-1)%len(kinds)]+`}`)
		stopped := strings.Contains(out, "STOP RETRYING publish_kind")
		if want := i >= tools.PolicyRefusalLimit; stopped != want {
			t.Errorf("attempt %d (%s): told to stop %v, want %v", i, kinds[(i-1)%len(kinds)], stopped, want)
		}
	}
}
