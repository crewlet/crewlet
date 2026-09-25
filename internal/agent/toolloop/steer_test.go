package toolloop_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// noteBox is a person's notes to the running loop, recording every step it was
// asked to take into a shared log so a case can say what happened FIRST.
type noteBox struct {
	mu      sync.Mutex
	pending []toolloop.SteerNote
	log     *[]string
}

func (b *noteBox) offer(id, msg string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = append(b.pending, toolloop.SteerNote{ID: id, Message: msg})
}

func (b *noteBox) Drain(int) []toolloop.SteerNote {
	b.mu.Lock()
	defer b.mu.Unlock()
	*b.log = append(*b.log, "drain")
	out := b.pending
	b.pending = nil
	return out
}

// offeringSurface offers a note to the box WHILE a named call runs — the moment
// a person steers a turn is exactly while it is busy.
type offeringSurface struct {
	fakeSurface
	box    *noteBox
	during string
	note   string
}

func (s *offeringSurface) Execute(ctx context.Context, call llm.ToolCall) (toolloop.ToolResult, error) {
	if call.Name == s.during {
		s.box.offer("n-1", s.note)
	}
	return s.fakeSurface.Execute(ctx, call)
}

// indexOf finds the note in a request's conversation, or -1.
func indexOf(msgs []llm.Message, content string) int {
	for i, m := range msgs {
		if m.Role == llm.RoleUser && m.Content == content {
			return i
		}
	}
	return -1
}

// A NOTE IS READ AT THE NEXT ROUND BOUNDARY, AFTER THE FENCE.
//
// Offered while round 1's tool runs, it must reach round 2's provider call —
// not round 1's (already sent) and not round 3's (a round late) — and the
// fence must have been asked first, so a turn this node no longer holds, or
// one a person stopped, is never handed a note it will not act on. The mark
// records the round that first read it.
func TestASteerIsDeliveredAtTheNextRoundBoundaryAfterTheFence(t *testing.T) {
	t.Parallel()
	var log []string
	box := &noteBox{log: &log}
	p := &scriptedProvider{turns: []llm.Completion{
		{ToolCalls: []llm.ToolCall{toolCall("1", "search")}},
		{ToolCalls: []llm.ToolCall{toolCall("2", "search")}},
		{Content: "done"},
	}}
	s := &offeringSurface{
		fakeSurface: fakeSurface{tools: []llm.ToolDef{def("search")}},
		box:         box, during: "search", note: "use the staging channel",
	}
	// Offer only once: the second search must not re-offer.
	ran := 0
	fenced := func() error {
		log = append(log, "fence")
		return nil
	}

	res, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: &onceSurface{inner: s, ran: &ran}, MaxRounds: 5,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "go"}},
		Fence:    fenced, Steer: box,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := indexOf(p.seen[0].Messages, "use the staging channel"); got != -1 {
		t.Fatal("round 1 read a note that was offered after it was sent")
	}
	if got := indexOf(p.seen[1].Messages, "use the staging channel"); got == -1 {
		t.Fatalf("round 2 did not read the note offered during round 1's call: %+v",
			p.seen[1].Messages)
	}
	if len(res.Steers) != 1 || res.Steers[0] != (toolloop.SteerMark{Round: 2, ID: "n-1"}) {
		t.Errorf("steers = %+v, want the note marked on round 2", res.Steers)
	}
	// Every round's drain follows that round's fence. The per-call fences
	// sit between them; what matters is that no drain comes before the
	// round-top fence of its own round.
	var rounds []string
	for i, step := range log {
		if step == "drain" {
			if i == 0 || log[i-1] != "fence" {
				t.Fatalf("a drain ran before its round's fence: %v", log)
			}
			rounds = append(rounds, step)
		}
	}
	if len(rounds) != 3 {
		t.Errorf("drained %d times for three rounds: %v", len(rounds), log)
	}
}

// onceSurface lets the inner surface offer its note on the first matching
// call only.
type onceSurface struct {
	inner *offeringSurface
	ran   *int
}

func (o *onceSurface) ToolDefs() []llm.ToolDef { return o.inner.ToolDefs() }
func (o *onceSurface) Phase() string           { return o.inner.Phase() }
func (o *onceSurface) Execute(ctx context.Context, call llm.ToolCall) (toolloop.ToolResult, error) {
	*o.ran++
	if *o.ran > 1 {
		return o.inner.fakeSurface.Execute(ctx, call)
	}
	return o.inner.Execute(ctx, call)
}

// A CLOSED FENCE READS NOTHING. The round that would have read the note never
// starts, so the note stays in the box — where the turn's end finds it and
// reports it expired rather than delivered.
func TestAClosedFenceLeavesTheNoteUnread(t *testing.T) {
	t.Parallel()
	var log []string
	box := &noteBox{log: &log}
	box.offer("n-1", "stop posting")
	stopped := errors.New("stopped")
	_, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: &scriptedProvider{}, Surface: &fakeSurface{}, MaxRounds: 3,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "go"}},
		Fence:    func() error { return stopped }, Steer: box,
	})
	if !errors.Is(err, stopped) {
		t.Fatalf("err = %v, want the fence's own", err)
	}
	if len(log) != 0 || len(box.pending) != 1 {
		t.Errorf("a fenced-off round drained its notes: log %v, pending %d", log, len(box.pending))
	}
}

// A NOTE NEVER SPLITS A TOOL CALL FROM ITS RESULT.
//
// A round that asked for two calls gets both answers before anything else is
// said: a provider rejects a conversation where a user message sits between a
// tool call and its result, and a model shown one would read the note as the
// tool's output. Offered while the FIRST of the two calls runs — the moment a
// naive delivery would slip it in — the note lands after both results.
func TestASteerNeverSplitsAToolCallFromItsResult(t *testing.T) {
	t.Parallel()
	var log []string
	box := &noteBox{log: &log}
	p := &scriptedProvider{turns: []llm.Completion{
		{ToolCalls: []llm.ToolCall{toolCall("a", "first"), toolCall("b", "second")}},
		{Content: "done"},
	}}
	s := &offeringSurface{
		fakeSurface: fakeSurface{tools: []llm.ToolDef{def("first"), def("second")}},
		box:         box, during: "first", note: "the note",
	}
	if _, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: s, MaxRounds: 4,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "go"}}, Steer: box,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	msgs := p.seen[1].Messages
	at := indexOf(msgs, "the note")
	if at == -1 {
		t.Fatalf("round 2 never read the note: %+v", msgs)
	}
	// Walk the conversation: after an assistant turn with N calls, the
	// next N messages are its results, and nothing else.
	for i, m := range msgs {
		if m.Role != llm.RoleAssistant || len(m.ToolCalls) == 0 {
			continue
		}
		for j, call := range m.ToolCalls {
			k := i + 1 + j
			if k >= len(msgs) || msgs[k].Role != llm.RoleTool || msgs[k].ToolCallID != call.ID {
				t.Fatalf("call %s is not answered at position %d — the conversation "+
					"is %+v", call.ID, k, msgs)
			}
		}
		if at <= i+len(m.ToolCalls) {
			t.Fatalf("the note is at %d, inside the calls and results of the "+
				"assistant turn at %d", at, i)
		}
	}
}
