package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// A harness for a case that drives a WHOLE TURN through the engine. A phase
// that offers tools reaches its harness through a session in the engine's
// directory, so a case's model is a scripted harness on the real session
// ([llmtest.Script]), never a provider handing calls back in its answer. A turn
// interleaves its phases with text calls in an order a case should not have to
// predict, so the run a phase plays is picked from the tools it offers.

// phasedHarness plays, for each call that offers tools, the run pick chooses from
// the names the phase offers; a call that offers none is answered by text.
type phasedHarness struct {
	model string

	// pick is the run a phase plays, from the tools it offers.
	pick func(offered []string) llmtest.Run

	// text answers a call that offers no tools. Nil answers "done".
	text func(ctx context.Context, req llm.Request) (*llm.Completion, error)
}

var _ llm.Provider = phasedHarness{}

func (p phasedHarness) Model() string { return p.model }

func (p phasedHarness) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	if req.Tools == nil {
		if p.text == nil {
			return &llm.Completion{Model: p.model, Content: "done", StopReason: llm.StopEnd}, nil
		}
		return p.text(ctx, req)
	}
	offered, err := offeredTo(req)
	if err != nil {
		return nil, err
	}
	script := &llmtest.Script{ModelName: p.model, Runs: []llmtest.Run{p.pick(offered)}}
	return script.Complete(ctx, req)
}

// offeredTo is the names a request's tool host offers — what the harness lists
// over its session before its model's first request.
func offeredTo(req llm.Request) ([]string, error) {
	host, ok := req.Tools.(interface{ Offered() []toolsession.Tool })
	if !ok {
		return nil, errors.New("phasedHarness: the request's tool host lists no tools")
	}
	var names []string
	for _, tool := range host.Offered() {
		names = append(names, tool.Def.Name)
	}
	return names, nil
}

// offers reports whether a phase offers name.
func offers(offered []string, name string) bool {
	for _, n := range offered {
		if n == name {
			return true
		}
	}
	return false
}

// calling is one response that calls name with args and costs usage.
func calling(name string, args map[string]any, usage llm.Usage) llmtest.Run {
	raw, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	return llmtest.Run{Responses: []llmtest.Response{{
		Calls: []llmtest.Call{{Name: name, Args: string(raw)}}, Usage: usage,
	}}}
}

// withSessions gives an engine a case built by hand the session directory
// [New] makes: every turn's phases serve their tools to the harness through it.
func withSessions(t *testing.T, e *Engine) *Engine {
	t.Helper()
	e.sessions = testSessions(t)
	return e
}

// testSessions is a session directory for one case, removed when it ends.
func testSessions(t *testing.T) *toolsession.Host {
	t.Helper()
	host, err := toolsession.NewHost()
	if err != nil {
		t.Fatalf("toolsession.NewHost: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	return host
}
