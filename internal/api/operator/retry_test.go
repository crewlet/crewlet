package operator_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/statelog"
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
	work := &recordingWork{}
	s := newSurface(t, operator.Options{
		Work: builtin.WorkDeps{
			Reader: stubWorkReader{}, Merges: stubWorkMerger,
			Writer: work.writer, Actor: operator.WorkActor(nil),
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

	_, ops := work.writes()
	ids := work.filed()
	if len(ops) != 2 || len(ids) != 2 {
		t.Fatalf("the two calls reached the tracker %d time(s)", len(ops))
	}
	if ops[0] != ops[1] || ids[0] != ids[1] {
		t.Errorf("the create brought back was operation %s filing task %s, the "+
			"first %s filing %s — a second item", ops[1], ids[1], ops[0], ids[0])
	}
}

// A PERSON'S ACT IS ITS REQUEST, AND NOTHING BESIDE IT NAMES THE OPERATION.
//
// The two transports reach one catalogue and carry a retry two ways: an
// assistant over MCP has no request of its own, so its write answers the
// `op_id` it was and takes it back; a person at the dashboard sends a
// `request_id`, and the transport derives the call's operation from that. An
// `op_id` in an act's arguments would be a second answer to "which operation
// is this" — one the caller picked, beside the one its request names — so it
// is refused as the caller's mistake, and nothing is written.
func TestAnActCannotNameAnOperationBesideItsRequest(t *testing.T) {
	t.Parallel()
	work := &recordingWork{}
	h := guarded(actSurface(t, work), false)
	other := requestOperation(t, "founder", requestB, builtin.CreateWorkItemTool, createArgs())
	args := createArgs()
	args["op_id"] = other
	body, err := json.Marshal(map[string]any{"request_id": requestA, "args": args})
	if err != nil {
		t.Fatal(err)
	}
	status, answer := act(t, h, "founder-secret", builtin.CreateWorkItemTool,
		"application/json", string(body))
	if status != http.StatusUnprocessableEntity || answer["error"] != "invalid" {
		t.Errorf("an act naming op_id %s beside its request answered %d %v, want "+
			"422 invalid", other, status, answer)
	}
	if _, ops := work.writes(); len(ops) != 0 {
		t.Errorf("the act reached the tracker under %v", ops)
	}
}

// answered calls one tool over MCP and decodes its JSON answer, failing on a
// refusal.
func answered(t *testing.T, sess *mcp.ClientSession, name string,
	args map[string]any) map[string]any {

	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	text := textOf(res)
	if res.IsError {
		t.Fatalf("%s refused: %s", name, text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%s answered no JSON (%v): %s", name, err, text)
	}
	return out
}
