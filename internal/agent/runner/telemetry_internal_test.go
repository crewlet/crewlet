package runner

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// The prompt measurement is exercised HERE, over values, as well as through a
// runner in fallback_test.go. Two levels because they hold different things:
// the runner cases hold the figure against what a provider was actually
// handed, and these hold the arithmetic itself — which of a phase's three
// possible openings is counted, and what a tool array comes to — without a
// provider, a surface or a turn to stand up first.

// A PHASE IS MEASURED AS THE LOOP SENDS IT, which is a seeded conversation OR
// an opening pair, never both.
//
// phaseRun documents system and user as ignored once seed is set, and the tool
// loop honours that by using the saved messages verbatim. A meter that added
// the strings anyway would report bytes no provider receives — on exactly the
// phase whose real prompt is largest, a resumed detached coding run.
func TestOnlyTheOpeningAPhaseActuallySendsIsCounted(t *testing.T) {
	t.Parallel()
	seed := []llm.Message{
		{Role: llm.RoleSystem, Content: "you are the CTO"},
		{Role: llm.RoleUser, Content: "fix the failing build"},
		{Role: "tool", Content: "the box says it compiles now"},
	}
	const seedChars = 15 + 21 + 28

	fresh, err := measurePrompt("system text", "user text", nil, nil)
	if err != nil {
		t.Fatalf("measurePrompt: %v", err)
	}
	if fresh.system != 11 || fresh.user != 9 || fresh.messages != 0 {
		t.Errorf("a fresh phase measured %d/%d/%d, want 11/9/0",
			fresh.system, fresh.user, fresh.messages)
	}

	resumed, err := measurePrompt("system text", "user text", seed, nil)
	if err != nil {
		t.Fatalf("measurePrompt: %v", err)
	}
	if resumed.messages != seedChars {
		t.Errorf("a resumed phase measured %d characters of conversation, want %d",
			resumed.messages, seedChars)
	}
	if resumed.system != 0 || resumed.user != 0 {
		t.Errorf("a resumed phase measured %d/%d system/user chars, but the loop sends "+
			"neither once a seed is set", resumed.system, resumed.user)
	}
}

// THE APPROXIMATION IS OVER EVERY TERM, so the columns beside it reproduce it.
//
// A total that omitted one of its own components would be a number nobody
// could re-derive from the row it sits on — and the term it omitted for as
// long as this event existed was the tool array, which is usually the largest.
func TestTheApproximationCoversEveryTermMeasured(t *testing.T) {
	t.Parallel()
	m := promptMeasure{system: 400, user: 200, messages: 80, tools: 1_200, toolCount: 9}
	if want := (400 + 200 + 80 + 1200) / bytesPerToken; m.approximateTokens() != want {
		t.Errorf("approximateTokens() = %d, want %d — every character term over the ratio",
			m.approximateTokens(), want)
	}
	// The count is not a character term. Adding it would make the figure
	// move when a tool is renamed into nothing.
	counted := promptMeasure{toolCount: 9}
	if counted.approximateTokens() != 0 {
		t.Error("a surface of nine definitions with no bytes approximated above zero")
	}
}

// A TOOL ARRAY IS MEASURED AS THE WIRE CARRIES IT: compact JSON, one
// {name, description, parameters} object per tool.
//
// The compact form is the number both HTTP vendors put on the wire, which is
// what makes the figure comparable across providers. The cli-agent text
// backend renders the same definitions indented inside a fence and follows
// them with a response contract, so its prompt carries strictly more — which
// is why the choice is written down rather than left to be inferred from a
// number.
func TestAToolArrayIsMeasuredAsCompactWireJSON(t *testing.T) {
	t.Parallel()
	defs := []llm.ToolDef{{
		Name:        "post_message",
		Description: "post to a channel",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}},
		},
	}}
	got, err := toolDefBytes(defs)
	if err != nil {
		t.Fatalf("toolDefBytes: %v", err)
	}
	want := len(`[{"name":"post_message","description":"post to a channel",` +
		`"parameters":{"properties":{"text":{"type":"string"}},"type":"object"}}]`)
	if got != want {
		t.Errorf("measured %d bytes, want %d — the compact array, not an indented one", got, want)
	}

	// No tools is no bytes, and it has to be reachable: a phase whose
	// surface is empty must be distinguishable from one that was never
	// measured, which is why the field carries no omitempty.
	if got, err := toolDefBytes(nil); got != 0 || err != nil {
		t.Errorf("toolDefBytes(nil) = %d, %v, want 0 and no error", got, err)
	}
}

// A TOOL WITH NO SCHEMA AND NO DESCRIPTION IS STILL A TOOL, not a panic.
//
// The surface fills an absent schema in before it ever renders a definition,
// so a nil map cannot arrive from there — but this arithmetic is the kind that
// gets called from somewhere else later, and a measurement that panics takes
// the turn goroutine with it. The fields are dropped rather than sent empty,
// which is what both vendors do.
func TestAToolWithNothingToDeclareStillMeasures(t *testing.T) {
	t.Parallel()
	got, err := toolDefBytes([]llm.ToolDef{{Name: "ping"}})
	if err != nil {
		t.Fatalf("toolDefBytes: %v", err)
	}
	if want := len(`[{"name":"ping"}]`); got != want {
		t.Errorf("measured %d bytes, want %d", got, want)
	}
}

// A SCHEMA THAT CANNOT BE ENCODED NAMES ITS OWN TOOL.
//
// An MCP server's schema reached this map through json.Unmarshal and a
// builtin's is a literal in this tree, so this is unreachable from a real
// surface — and it is exactly the case where the error a person reads decides
// whether they can act. encoding/json reports the offending Go type, of which
// a surface has one per tool; the tool's name is the thing they can go and
// look at.
func TestAnUnencodableSchemaNamesTheToolItBelongsTo(t *testing.T) {
	t.Parallel()
	_, err := toolDefBytes([]llm.ToolDef{
		{Name: "healthy", Parameters: map[string]any{"type": "object"}},
		{Name: "broken", Parameters: map[string]any{"type": make(chan int)}},
	})
	if err == nil {
		t.Fatal("a schema json cannot encode measured cleanly, so nothing would report it")
	}
	if !strings.Contains(err.Error(), `"broken"`) {
		t.Errorf("error does not name the tool to go and fix: %v", err)
	}
	// And the error is a wrapper over what json said, so a caller can still
	// reach the cause.
	var unsupported *json.UnsupportedTypeError
	if !errors.As(err, &unsupported) {
		t.Errorf("the json error was restated rather than wrapped: %v", err)
	}
}
