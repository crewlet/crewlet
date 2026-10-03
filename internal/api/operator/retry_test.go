package operator_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/iam"
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
	s := newSurface(t, operator.Options{Halves: fixed(operator.Halves{
		Work: builtin.WorkDeps{
			Reader: stubWorkReader{}, Merges: stubWorkMerger, Writer: work.writer,
		},
	})})
	sess := dialOperator(t, s,
		machine("token:founder", iam.GrantStateRead, iam.GrantWorkWrite))

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
		_, offered = schema.Properties[operator.OperationArg]
	}
	if !offered {
		t.Fatal("create_work_item is served with no op_id in its schema, so an " +
			"assistant has no way to send one back")
	}

	args := map[string]any{"title": "the follow-up", "project": "ENG"}
	first := answered(t, sess, builtin.CreateWorkItemTool, args)
	op, _ := first[operator.OperationArg].(string)
	if err := statelog.CheckCallerOpID(op); err != nil {
		t.Fatalf("the create answered op_id %q, which this surface refuses back: %v", op, err)
	}
	args[operator.OperationArg] = op
	second := answered(t, sess, builtin.CreateWorkItemTool, args)
	if second[operator.OperationArg] != op {
		t.Errorf("the create brought back answered op_id %v, want %s",
			second[operator.OperationArg], op)
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

// A PERSON'S ACT NAMES ITS OPERATION IN ITS HEADER, AND NOTHING BESIDE IT DOES.
//
// The transports reach one catalogue and carry a retry two ways: an assistant
// over MCP has no request key, so its write answers the `op_id` it was and
// takes it back; a person at the dashboard sends an `Idempotency-Key`, and
// every write the call makes derives from that. An `op_id` in an act's
// arguments would be a second answer to "which operation is this" — one the
// caller picked, beside the one its request names — so it is refused `400`
// as the caller's to change, naming where the operation goes, and nothing is
// written.
func TestAnActCannotNameAnOperationBesideItsKey(t *testing.T) {
	t.Parallel()
	work := &recordingWork{}
	h := guarded(actSurface(t, work))
	args := createArgs()
	args[operator.OperationArg] = newKey()
	body, err := json.Marshal(map[string]any{"args": args})
	if err != nil {
		t.Fatal(err)
	}
	status, answer := act(t, h, "founder", builtin.CreateWorkItemTool,
		"application/json", string(body), newKey())
	if status != http.StatusBadRequest || answer["error"] != string(httpjson.CodeInvalidBody) {
		t.Errorf("an act naming op_id beside its key answered %d %v, want 400 "+
			"invalid_body", status, answer)
	}
	if detail, _ := answer["detail"].(string); !containsAll(detail, opkey.Header,
		operator.OperationArg) {

		t.Errorf("the refusal %q does not say where the operation goes", detail)
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

// containsAll reports whether s holds every one of parts.
func containsAll(s string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(s, part) {
			return false
		}
	}
	return true
}
