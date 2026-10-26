package chain_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// The chain as a seat at work meets it: walked by the tool loop, every member a
// CLI run over the phase's real session (llmtest), so what one member did is on
// the host's record when the next is tried.

// surface answers every call with "<name> ok" and remembers what ran.
type surface struct {
	mu    sync.Mutex
	tools []llm.ToolDef
	ran   []string
}

func newSurface(names ...string) *surface {
	s := &surface{}
	for _, n := range names {
		s.tools = append(s.tools, llm.ToolDef{Name: n, Description: n,
			Parameters: map[string]any{"type": "object"}})
	}
	return s
}

func (s *surface) ToolDefs() []llm.ToolDef              { return slices.Clone(s.tools) }
func (s *surface) Revision() uint64                     { return 0 }
func (s *surface) AnnotationsOf(string) mcp.Annotations { return mcp.Annotations{} }
func (s *surface) Detached(string) bool                 { return false }
func (s *surface) Phase() string                        { return "execute" }

// Admit admits a call to a surface that keeps no call log: it runs as it is.
func (s *surface) Admit(string, map[string]any) toolloop.Admitted { return s.execute }

func (s *surface) execute(_ context.Context, call llm.ToolCall) (toolloop.ToolResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ran = append(s.ran, call.Name)
	return toolloop.ToolResult{Output: call.Name + " ok"}, nil
}

func (s *surface) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ran)
}

func host(t *testing.T) *toolsession.Host {
	t.Helper()
	h, err := toolsession.NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func opening() []llm.Message {
	return []llm.Message{
		{Role: llm.RoleSystem, Content: "you are a seat"},
		{Role: llm.RoleUser, Content: "do the task"},
	}
}

// diesMidRun is a member whose CLI makes a call, starts writing its next
// response, and dies: the call is on the host's record, the prose is not.
type diesMidRun struct {
	mu   sync.Mutex
	seen []llm.Request
}

func (*diesMidRun) Model() string { return "dies" }

func (d *diesMidRun) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	d.mu.Lock()
	d.seen = append(d.seen, req)
	d.mu.Unlock()
	c, err := llmtest.Connect(ctx, req, llmtest.ConnOptions{})
	if err != nil {
		return nil, err
	}
	c.Respond(ctx, llmtest.Response{Calls: []llmtest.Call{{Name: "look"}}})
	req.Send(llm.Delta{Content: "half an ans"})
	c.Close()
	return nil, &llm.Error{Kind: llm.KindServer, Provider: "p", Model: "dies",
		Err: errors.New("the CLI exited mid-response")}
}

// continues is a member that reads what it was handed, then submits.
type continues struct {
	mu      sync.Mutex
	seen    []llm.Request
	pending [][]llm.Message
}

func (*continues) Model() string { return "continues" }

func (c *continues) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	c.mu.Lock()
	c.seen = append(c.seen, req)
	if req.Tools != nil {
		c.pending = append(c.pending, req.Tools.Pending())
	}
	c.mu.Unlock()
	req.Send(llm.Delta{Content: "submitting"})
	return llmtest.Drive(ctx, req, llmtest.ConnOptions{}, func(*llmtest.Conn) (llmtest.Response, error) {
		return llmtest.Response{Text: "submitting", Calls: []llmtest.Call{{Name: "submit_work"}}}, nil
	})
}

// A FALLBACK CONTINUES FROM THE SESSION'S RECORD. The member that died had
// already had a call run; the next one is handed the same conversation and the
// same host, which holds that call and its result, so the work is shown to the
// model rather than done again. The abandoned prose is all that is dropped: a
// watcher is told the attempt restarted and sets the text aside, and the
// record keeps the call the dead member made.
//
// Mutation: hand a later member the request without its host, as a walk that
// restarted the call from scratch would, and it is shown nothing the first
// did.
func TestAFallbackContinuesFromTheSessionsRecord(t *testing.T) {
	t.Parallel()
	first, second := &diesMidRun{}, &continues{}
	var mu sync.Mutex
	var hops []chain.Fallback
	var frames []toolloop.Result
	ch, err := chain.New([]chain.Member{
		{Key: "first", Provider: first}, {Key: "second", Provider: second},
	}, chain.Options{OnFallback: func(f chain.Fallback) {
		mu.Lock()
		defer mu.Unlock()
		hops = append(hops, f)
	}})
	if err != nil {
		t.Fatalf("chain.New: %v", err)
	}
	surf := newSurface("look", "submit_work")
	res, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: ch, Surface: surf, Host: host(t), Messages: opening(), MaxRounds: 8,
		TerminateAfter: []string{"submit_work"},
		StreamPartials: true,
		OnProgress: func(r toolloop.Result) {
			mu.Lock()
			defer mu.Unlock()
			frames = append(frames, r)
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// THE WORK WAS NOT DONE TWICE.
	if got := surf.calls(); !slices.Equal(got, []string{"look", "submit_work"}) {
		t.Errorf("the surface ran %v, want the first member's call once and the second's submission", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hops) != 1 || hops[0].From != "first" || hops[0].To != "second" {
		t.Errorf("hand-offs %+v, want the one from first to second", hops)
	}

	// THE SAME REQUEST: the same conversation, the same host.
	if len(first.seen) != 1 || len(second.seen) != 1 {
		t.Fatalf("members were asked %d and %d times, want once each", len(first.seen), len(second.seen))
	}
	a, b := first.seen[0], second.seen[0]
	if !slices.EqualFunc(a.Messages, b.Messages, func(x, y llm.Message) bool {
		return x.Role == y.Role && x.Content == y.Content
	}) {
		t.Errorf("the second member was handed %+v, want the first's conversation %+v", b.Messages, a.Messages)
	}
	if a.Tools == nil || a.Tools != b.Tools {
		t.Error("the second member was handed another host, so it cannot see what the first did")
	}
	// AND THE HOST'S RECORD SHOWS WHAT THE FIRST DID: the call, and its
	// result, ready to be rendered as work already done.
	if len(second.pending) != 1 {
		t.Fatalf("the second member read the record %d times", len(second.pending))
	}
	var asked, answered bool
	for _, m := range second.pending[0] {
		if m.Role == llm.RoleAssistant && slices.ContainsFunc(m.ToolCalls, func(c llm.ToolCall) bool {
			return c.Name == "look"
		}) {
			asked = true
		}
		if m.Role == llm.RoleTool && m.Content == "look ok" {
			answered = true
		}
	}
	if !asked || !answered {
		t.Errorf("the record the second member continued from is %+v, want the first's call and its result",
			second.pending[0])
	}

	// ONLY THE ABANDONED PROSE IS SET ASIDE.
	if strings.Contains(res.Text, "half an ans") {
		t.Errorf("the abandoned prose reached the phase's text: %q", res.Text)
	}
	abandoned := false
	for _, f := range frames {
		if f.Partial == nil {
			continue
		}
		for _, n := range f.Partial.Abandoned {
			if strings.Contains(n.Content, "half an ans") {
				abandoned = true
			}
		}
	}
	if !abandoned {
		t.Error("no live frame set the dead member's prose aside as abandoned")
	}
	names := make([]string, 0, len(res.Executions))
	for _, ex := range res.Executions {
		names = append(names, ex.Name)
	}
	if !slices.Equal(names, []string{"look", "submit_work"}) {
		t.Errorf("executions %v, want the dead member's call kept beside the submission", names)
	}
	if res.ProviderKey != "second" {
		t.Errorf("provider key %q, want the member that answered", res.ProviderKey)
	}
}

// THE WALK STOPS WHERE THE PHASE IS DECIDED. A member submitted, the host
// ended its run, and its CLI — stopped mid-run — reports a failure the chain
// would otherwise walk on from: a second model asked to redo a phase that is
// over. The loop reads the submission off its own record.
//
// Mutation: drop the Halted check from Complete, and the second member is
// asked.
func TestTheChainStopsWhereThePhaseIsDecided(t *testing.T) {
	t.Parallel()
	first := &llmtest.Script{Runs: []llmtest.Run{{
		Responses: []llmtest.Response{{Calls: []llmtest.Call{{Name: "submit_work"}}}},
		Err: &llm.Error{Kind: llm.KindServer, Provider: "p", Model: "first",
			Err: errors.New("the CLI was stopped")},
	}}}
	second := &llmtest.Script{}
	fired := 0
	ch, err := chain.New([]chain.Member{
		{Key: "first", Provider: first}, {Key: "second", Provider: second},
	}, chain.Options{OnFallback: func(chain.Fallback) { fired++ }})
	if err != nil {
		t.Fatalf("chain.New: %v", err)
	}
	surf := newSurface("submit_work")
	res, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: ch, Surface: surf, Host: host(t), Messages: opening(), MaxRounds: 8,
		TerminateAfter: []string{"submit_work"},
	})
	if err != nil {
		t.Fatalf("Run: %v — a phase its submission ended was failed over the CLI stopped after it", err)
	}
	if n := len(second.Seen()); n != 0 {
		t.Errorf("the second member was asked %d times after the phase submitted", n)
	}
	if fired != 0 {
		t.Errorf("%d hand-offs were announced where none happened", fired)
	}
	if got := surf.calls(); !slices.Equal(got, []string{"submit_work"}) || res.ExhaustedRounds {
		t.Errorf("the surface ran %v (exhausted %v), want the one submission", got, res.ExhaustedRounds)
	}
}
