package cliagent_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
)

// genericTools are the Dashboard Assistant's three generic API tools as their
// schemas type the parameters a markup call writes as text.
var genericTools = []llm.ToolDef{{
	Name: "list_agents",
}, {
	Name: "describe_api",
	Parameters: map[string]any{"type": "object", "properties": map[string]any{
		"search": map[string]any{"type": "string"},
		"route":  map[string]any{"type": "string"},
	}},
}, {
	Name: "api_request",
	Parameters: map[string]any{"type": "object", "properties": map[string]any{
		"route": map[string]any{"type": "string"},
		"path":  map[string]any{"type": "object"},
	}},
}, {
	Name: "propose_api_change",
	Parameters: map[string]any{"type": "object", "properties": map[string]any{
		"route": map[string]any{"type": "string"},
		"path":  map[string]any{"type": "object"},
		"body":  map[string]any{"type": "object"},
	}},
}}

// reply is one fixture under testdata/replies: a reply as a CLI printed it.
func reply(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "replies", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// names are the tools calls name, in order.
func names(calls []llm.ToolCall) string {
	var out []string
	for _, c := range calls {
		out = append(out, c.Name)
	}
	return strings.Join(out, ",")
}

// A CALL THE REPLY MAKES IS RUN OR REFUSED, NEVER LOST.
//
// The incident: a Dashboard Assistant turn asked to add a seat ended
// `completed` after six read steps, its answer saying a proposal had been
// prepared and nothing proposed. The model had made the call, but not in the
// envelope the reply was read by: in an envelope before it, as markup beside
// it, or as a fenced block. Each shape here is one a model writes (the
// captured ones verbatim from Claude Code 2.1.310 on Sonnet, replaying the
// incident's question), and each must either run the call it makes or refuse
// the whole reply with the call on the record, never complete on its prose.
//
// Mutation: read only the envelope the reply was picked by (drop the
// namedCalls check in ParseEnvelope), and every "lost" case completes with
// no call and the claim as its answer.
func TestACallTheReplyMakesIsRunOrRefusedNeverLost(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		fixture string
		// runs are the calls the round runs; refused the calls a
		// protocol fault names, when the reply is one.
		runs, refused string
	}{
		{fixture: "captured-markup-then-envelope.txt", runs: "describe_api"},
		{fixture: "captured-markup-stray-fence-correction.txt", runs: "api_request"},
		{fixture: "captured-empty-invoke-then-envelope.txt", runs: "propose_api_change"},
		// The incident's loss, captured live: the proposal and a read
		// written as markup, then an envelope asking for nothing. Read by
		// the envelope alone it was a round with no call, and the model,
		// believing its proposal made, said so next round.
		{fixture: "captured-lost-markup-then-empty-envelope.txt", refused: "propose_api_change,list_agents"},
		{fixture: "lost-two-envelopes.txt", refused: "propose_api_change"},
		{fixture: "lost-markup-then-claim.txt", refused: "propose_api_change"},
		{fixture: "lost-fenced-markup.txt", refused: "propose_api_change"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			c := cliagent.ReadReply(reply(t, tc.fixture), genericTools)
			if got := names(c.ToolCalls); got != tc.runs {
				t.Errorf("runs %q, want %q", got, tc.runs)
			}
			if got := names(c.RefusedCalls); got != tc.refused {
				t.Errorf("refuses %q, want %q", got, tc.refused)
			}
			if faulted := c.ProtocolError != ""; faulted != (tc.refused != "") {
				t.Errorf("ProtocolError = %q, want one exactly when calls are refused", c.ProtocolError)
			}
			if c.ProtocolError == "" && len(c.ToolCalls) == 0 {
				t.Errorf("the reply completes with no call: %q", c.Content)
			}
		})
	}
}

// The same call written twice (as markup and then as the envelope, the shape
// Claude Code writes most often in text mode) is asked for once, and runs
// once, with the arguments both forms agree on.
func TestACallWrittenTwiceRunsOnce(t *testing.T) {
	t.Parallel()
	c := cliagent.ReadReply(reply(t, "captured-markup-stray-fence-correction.txt"), genericTools)
	if len(c.ToolCalls) != 1 {
		t.Fatalf("calls = %+v, want the one api_request", c.ToolCalls)
	}
	path, ok := c.ToolCalls[0].Arguments["path"].(map[string]any)
	if !ok || path["id"] != "crewlet-swe" {
		t.Errorf("path = %#v, want {id: crewlet-swe}", c.ToolCalls[0].Arguments["path"])
	}
}

// A CALL WRITTEN LESS COMPLETELY BESIDE THE WHOLE OF IT IS THE SAME REQUEST,
// and one with a value or an argument the picked call does not hold is
// another, refused.
func TestACallIsAccountedForByTheWholeOfIt(t *testing.T) {
	t.Parallel()
	envelope := "\n```json\n" + `{"message": "", "tool_calls": [{"name": "api_request", ` +
		`"arguments": {"route": "GET /config/roles/{id}", "path": {"id": "engineer"}}}]}` + "\n```"
	for _, tc := range []struct {
		markup string
		runs   bool
	}{
		{`<invoke name="api_request"></invoke>`, true},
		{`<invoke name="api_request"><parameter name="route">GET /config/roles/{id}</parameter></invoke>`, true},
		{`<invoke name="api_request"><parameter name="path">{"id": "crewlet-swe"}</parameter></invoke>`, false},
		{`<invoke name="api_request"><parameter name="limit">5</parameter></invoke>`, false},
		{`<invoke name="describe_api"></invoke>`, false},
	} {
		c := cliagent.ReadReply(tc.markup+envelope, genericTools)
		if ran := c.ProtocolError == "" && names(c.ToolCalls) == "api_request"; ran != tc.runs {
			t.Errorf("%s beside the envelope: ran = %v, want %v (%q)", tc.markup, ran, tc.runs, c.ProtocolError)
		}
	}
}

// AN ENVELOPE WITH BOTH IS CALLS AND NARRATION. A message beside tool calls is
// what the model says while they run: the calls run, and the round is not a
// final answer, which the tool loop reads from the calls being there.
func TestAnEnvelopeWithAMessageAndCallsRunsTheCalls(t *testing.T) {
	t.Parallel()
	c := cliagent.ReadReply("```json\n"+`{"message": "Reading the engineer role first.", `+
		`"tool_calls": [{"name": "api_request", "arguments": {"route": "GET /config/roles/{id}", `+
		`"path": {"id": "engineer"}}}]}`+"\n```", genericTools)
	if c.ProtocolError != "" || names(c.ToolCalls) != "api_request" || c.StopReason != llm.StopToolUse {
		t.Fatalf("read as %+v, want the call run", c)
	}
	if c.Content != "Reading the engineer role first." {
		t.Errorf("content = %q, want the message as the round's narration", c.Content)
	}
}

// Two envelopes asking for different calls are one reply asking for two sets:
// neither is run, since nothing says which the model meant, and both are on
// the record. The worked example a model shows before its answer is refused
// with it; asking again costs one round, running the example costs a change
// nobody asked for.
func TestTwoEnvelopesAskingForDifferentCallsRunNeither(t *testing.T) {
	t.Parallel()
	c := cliagent.ReadReply("Here is the format:\n```json\n"+
		`{"message":"example","tool_calls":[{"name":"describe_api"}]}`+"\n```\nAnd my answer:\n```json\n"+
		`{"message":"real","tool_calls":[{"name":"api_request","arguments":{"route":"GET /agents"}}]}`+
		"\n```", genericTools)
	if c.ProtocolError == "" || len(c.ToolCalls) != 0 {
		t.Fatalf("read as %+v, want a protocol fault that runs nothing", c)
	}
	if got := names(c.RefusedCalls); got != "api_request,describe_api" {
		t.Errorf("refused %q, want both calls on the record", got)
	}
}
