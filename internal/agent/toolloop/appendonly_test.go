package toolloop_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// THE CONVERSATION ONLY GROWS. Claude Opus 5.5, Sonnet 5.5 and Fable 5.1 bind
// every thinking block to the conversation before it, so a request that sent
// any earlier message differently — a corrective spliced in, a note rewritten,
// a turn rebuilt — invalidates every block after the change, which on an
// account the vendor enforces is a 400 no fallback retries. So every request a
// phase makes must be the previous one with messages ADDED at the end, and the
// conversation a phase hands back — which an extension, a withheld corrective
// and a suspension all continue from — must extend the last request too.
//
// One run exercises every way the loop adds a message: a tool round, a note a
// person sent while it ran, the empty-answer corrective and the finishing one.
// Each assistant turn carries the vendor's own blocks, and those must reach
// every later request unchanged as well, since they are what is replayed.
func TestTheConversationOnlyGrows(t *testing.T) {
	t.Parallel()
	blocks := func(b ...string) []json.RawMessage {
		out := make([]json.RawMessage, len(b))
		for i := range b {
			out[i] = json.RawMessage(b[i])
		}
		return out
	}
	var log []string
	box := &noteBox{log: &log}
	p := &scriptedProvider{turns: []llm.Completion{
		{
			Provider: "anthropic", Model: "claude-test",
			ToolCalls: []llm.ToolCall{toolCall("1", "search")},
			Raw: blocks(`{"type":"thinking","thinking":"a","signature":"s1"}`,
				`{"type":"tool_use","id":"1","name":"search","input":{}}`),
		},
		// Thought and said nothing: the empty-answer corrective.
		{Provider: "anthropic", Model: "claude-test",
			Raw: blocks(`{"type":"thinking","thinking":"b","signature":"s2"}`)},
		// Answered in prose where a submission was owed: the finishing one.
		{Provider: "anthropic", Model: "claude-test", Content: "All done.",
			Raw: blocks(`{"type":"text","text":"All done."}`)},
		{
			Provider: "anthropic", Model: "claude-test",
			ToolCalls: []llm.ToolCall{toolCall("2", "submit")},
			Raw:       blocks(`{"type":"tool_use","id":"2","name":"submit","input":{}}`),
		},
	}}
	s := &offeringSurface{
		fakeSurface: fakeSurface{tools: []llm.ToolDef{def("search"), def("submit")}},
		box:         box, during: "search", note: "use the staging channel",
	}
	res, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: s, MaxRounds: 8, Steer: box,
		TerminateAfter: []string{"submit"},
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "be useful"},
			{Role: llm.RoleUser, Content: "go"},
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.seen) != 4 {
		t.Fatalf("made %d requests, want 4 — the scenario did not run as written", len(p.seen))
	}
	if indexOf(p.seen[1].Messages, "use the staging channel") < 0 {
		t.Fatal("the note never reached the conversation — the scenario did not run as written")
	}

	grows := func(what string, before, after []llm.Message) {
		t.Helper()
		if len(after) <= len(before) {
			t.Errorf("%s: %d messages after %d — nothing was added", what, len(after), len(before))
			return
		}
		for i := range before {
			if !reflect.DeepEqual(before[i], after[i]) {
				t.Errorf("%s: message %d changed\n  was %+v\n  now %+v", what, i, before[i], after[i])
			}
		}
	}
	for k := 1; k < len(p.seen); k++ {
		grows("request "+string(rune('0'+k+1)), p.seen[k-1].Messages, p.seen[k].Messages)
	}
	grows("the conversation handed back", p.seen[len(p.seen)-1].Messages, res.Messages)

	// Every assistant turn is its completion's, untouched — the vendor's
	// blocks and the origin they are replayed by included. An edit to the
	// newest turn before the next request sends it passes the check above,
	// and is still a turn that is not what the model wrote.
	var turns int
	for _, m := range res.Messages {
		if m.Role != llm.RoleAssistant {
			continue
		}
		if want := p.turns[turns].Message(); !reflect.DeepEqual(m, want) {
			t.Errorf("assistant turn %d =\n  %+v\nwant its completion's own\n  %+v", turns+1, m, want)
		}
		turns++
	}
	if turns != len(p.turns) {
		t.Errorf("%d assistant turns in the conversation, want %d", turns, len(p.turns))
	}
}
