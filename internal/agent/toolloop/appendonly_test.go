package toolloop_test

import (
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// THE CONVERSATION ONLY GROWS. A model answers the conversation it was sent,
// so a request that sent any earlier message differently — a corrective
// spliced in, a note rewritten, a turn rebuilt — shows it a history in which
// its own earlier answers replied to something they never saw. So every
// request a phase makes must be the previous one with messages ADDED at the
// end, and the conversation a phase hands back — which an extension, a
// withheld corrective and a suspension all continue from — must extend the
// last request too.
//
// One run exercises every way the loop adds a message: a tool round, a note a
// person sent while it ran, the empty-answer corrective and the finishing one.
func TestTheConversationOnlyGrows(t *testing.T) {
	t.Parallel()
	var log []string
	box := &noteBox{log: &log}
	p := &scriptedProvider{turns: []llm.Completion{
		{Model: "claude-test", ToolCalls: []llm.ToolCall{toolCall("1", "search")}},
		// Said nothing and called nothing: the empty-answer corrective.
		{Model: "claude-test"},
		// Answered in prose where a submission was owed: the finishing one.
		{Model: "claude-test", Content: "All done."},
		{Model: "claude-test", ToolCalls: []llm.ToolCall{toolCall("2", "submit")}},
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

	// Every assistant turn is its completion's, untouched. An edit to the
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
