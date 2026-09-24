package operator_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// AN OPERATOR'S ASSISTANT CAN FINISH WHAT IT STARTED, OVER THE WIRE IT USES.
//
// The builtin suite certifies what the tools make of an `op_id`; this is the
// half only the served surface can: that the argument is in the schema an
// MCP client is handed — a client builds its call from that schema and sends
// nothing it does not name — and that the id an answer carries, sent back
// through the protocol, reaches the tracker as the SAME operation and the
// same task. Before it, every call here minted a fresh operation, so the one
// remedy an `unknown` or a gesture stopped part of the way through asks for
// could not be expressed at all, and the text that asked for a repeat filed a
// second item.
func TestAnOperatorsAssistantFinishesACreateWithTheOpIDItWasAnswered(t *testing.T) {
	t.Parallel()
	writer := &recordingWriter{}
	s := operator.New(operator.Options{
		Work: builtin.WorkDeps{
			Reader: stubWorkReader{}, Merges: stubWorkMerger,
			Writer: func(builtin.Actor) builtin.WorkWriter { return writer },
			Actor:  operator.WorkActor(nil),
		},
	})
	if s == nil {
		t.Fatal("a company on the native tracker got no surface")
	}
	sess := dialOperator(t, s, "founder")

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	listed, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	offered := false
	for _, tool := range listed.Tools {
		if tool.Name != builtin.CreateWorkItemTool {
			continue
		}
		raw, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		_ = json.Unmarshal(raw, &schema)
		_, offered = schema.Properties["op_id"]
	}
	if !offered {
		t.Fatal("create_work_item is served with no op_id in its schema, so an " +
			"assistant has no way to send one back")
	}

	args := map[string]any{"title": "the follow-up", "project": "ENG"}
	first := answered(t, sess, builtin.CreateWorkItemTool, args)
	op, _ := first["op_id"].(string)
	if err := statelog.CheckCallerOpID(op); err != nil {
		t.Fatalf("the create answered op_id %q, which this surface refuses back: %v", op, err)
	}
	args["op_id"] = op
	second := answered(t, sess, builtin.CreateWorkItemTool, args)
	if second["op_id"] != op {
		t.Errorf("the create brought back answered op_id %v, want %s", second["op_id"], op)
	}

	ops, ids := writer.seen()
	if len(ops) != 2 {
		t.Fatalf("the two calls reached the tracker %d time(s)", len(ops))
	}
	if ops[0] != ops[1] || ids[0] != ids[1] {
		t.Errorf("the create brought back was operation %s filing task %s, the "+
			"first %s filing %s — a second item", ops[1], ids[1], ops[0], ids[0])
	}
}

// answered calls one tool and decodes its JSON answer, failing on a refusal.
func answered(t *testing.T, sess *mcp.ClientSession, name string,
	args map[string]any) map[string]any {

	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	if res.IsError {
		t.Fatalf("%s refused: %s", name, text.String())
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text.String()), &out); err != nil {
		t.Fatalf("%s answered no JSON (%v): %s", name, err, text.String())
	}
	return out
}

// recordingWriter is the tracker's write side as this case needs it: what
// each create was asked for, and nothing decided.
type recordingWriter struct {
	mu  sync.Mutex
	ops []string
	ids []string
}

func (w *recordingWriter) seen() ([]string, []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.ops...), append([]string(nil), w.ids...)
}

func (w *recordingWriter) CreateTask(_ context.Context, opID string, task tracker.Task,
	_ *tracker.Notify) (tracker.WriteResult, error) {

	w.mu.Lock()
	defer w.mu.Unlock()
	w.ops = append(w.ops, opID)
	w.ids = append(w.ids, task.ID)
	return tracker.WriteResult{Key: "ENG-9", Result: statelog.Result{
		Outcome: statelog.OutcomeApplied, Version: 1,
	}}, nil
}

func (w *recordingWriter) UpdateTask(context.Context, string, string, string, uint64,
	tracker.TaskPatch, tracker.ChangeKind, *tracker.Notify) (tracker.WriteResult, error) {

	return tracker.WriteResult{}, nil
}
