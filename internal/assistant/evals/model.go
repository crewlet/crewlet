package evals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
)

// Model is a scripted model: it answers each model request with the next round
// of its script and records every request it was sent, so a suite drives a
// whole turn without a real model (the engine rule: tests never call one).
//
// A REQUEST THAT OFFERS TOOLS IS A CLI RUN, as every backend's is (ADR-0049):
// the model connects to the request's tool host the way a CLI's MCP client
// does (internal/providers/llm/llmtest) and plays rounds in turn, each one's
// prose and then its calls, until a round calls nothing (the answer) or the
// host ends the run, a change parked on the person's approval among them. The
// rounds a run did not reach are the next run's.
type Model struct {
	// Name is what Model() reports.
	Name string

	mu       sync.Mutex
	script   []Round
	requests []llm.Request
	// Gate, when set, is waited on before each round is answered, so a
	// suite can stop a turn while its model is thinking.
	Gate chan struct{}
	// unready is what the model says its next call would meet, without a
	// call ([Model.Unready]).
	unready error
	// offered is each request's tools as its CLI listed them, and answers
	// every call's answer by its id.
	offered [][]llm.ToolDef
	answers map[string]llmtest.Result
	// seen is every message the model read, in order: each request's
	// conversation, then the answers its calls were given over the session.
	seen []llm.Message
}

// Round is one scripted model response: prose, tool calls, or an error.
type Round struct {
	Text  string
	Calls []llm.ToolCall
	Err   error
	// Input and Output are the tokens the round reports spending.
	Input, Output int
	// Deltas are sent, in order, before the round's response is played:
	// the fragments of its prose a CLI streams while it writes.
	Deltas []llm.Delta
	// Streamed, when set, is closed once the deltas are sent, and Hold,
	// when set, is waited on after that, so a suite can read the stream
	// while the round is still being written.
	Streamed chan struct{}
	Hold     chan struct{}
}

// Say is a round that answers in prose.
func Say(text string) Round { return Round{Text: text, Input: 100, Output: 20} }

// Call is a round that asks for one tool.
func Call(id, name string, args map[string]any) Round {
	return Round{Calls: []llm.ToolCall{{ID: id, Name: name, Arguments: args}},
		Input: 100, Output: 10}
}

// NewModel scripts rounds, in order.
func NewModel(rounds ...Round) *Model {
	return &Model{Name: "scripted", script: rounds, answers: map[string]llmtest.Result{}}
}

// ErrScriptEnded is a round asked for past the script.
var ErrScriptEnded = errors.New("evals: the model's script has ended")

// Model reports the scripted model's name.
func (m *Model) Model() string { return m.Name }

// Complete answers one model request: a run over the request's tools, or the
// next round's prose for a request that offers none.
func (m *Model) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	m.mu.Lock()
	m.requests = append(m.requests, req)
	at := len(m.requests) - 1
	m.offered = append(m.offered, nil)
	m.seen = append(m.seen, req.Messages...)
	m.mu.Unlock()
	if req.Tools == nil {
		next, err := m.next(ctx, req)
		if err != nil {
			return nil, err
		}
		return &llm.Completion{Content: next.Text, Model: m.Name, StopReason: llm.StopEnd,
			InputTokens: next.Input, OutputTokens: next.Output}, nil
	}
	c, err := llmtest.Connect(ctx, req, llmtest.ConnOptions{ModelName: m.Name})
	if err != nil {
		return nil, err
	}
	defer c.Close()
	m.mu.Lock()
	m.offered[at] = c.Offered()
	m.mu.Unlock()
	// THE RUN, as a CLI plays it: a round's prose and calls, then the next,
	// until a round calls nothing or the host ends the run.
	for !c.Stopped() {
		next, err := m.next(ctx, req)
		if err != nil {
			c.Close()
			return nil, err
		}
		r := response(next)
		results := c.Respond(ctx, r)
		m.mu.Lock()
		for i, res := range results {
			id := r.Calls[i].ID
			m.answers[id] = res
			m.seen = append(m.seen, llm.Message{Role: llm.RoleTool, ToolCallID: id,
				Name: res.Name, Content: res.Output, Failed: res.Failed})
		}
		m.mu.Unlock()
		if len(r.Calls) == 0 {
			break
		}
	}
	return c.Finish(""), nil
}

// OfferedAt is the tools the model's request i offered, as its CLI listed
// them: none for a request that offered no tools.
func (m *Model) OfferedAt(i int) []llm.ToolDef {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i < 0 || i >= len(m.offered) {
		return nil
	}
	return slices.Clone(m.offered[i])
}

// Seen is every message the model read, in order: each request's
// conversation as it was sent, then every answer its CLI was given over the
// tool session as a tool message. What a suite scans for what reached the
// model, since a run's answers travel over the session and never in a
// request.
func (m *Model) Seen() []llm.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.seen)
}

// Answer is what the call with id was answered over the tool session, and
// whether the model made it and heard back.
func (m *Model) Answer(id string) (llmtest.Result, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	res, ok := m.answers[id]
	return res, ok
}

// next takes the script's next round, once the gate and the round's own
// stream and hold allow it, or the error it fails with.
func (m *Model) next(ctx context.Context, req llm.Request) (Round, error) {
	if m.Gate != nil {
		select {
		case <-m.Gate:
		case <-ctx.Done():
			return Round{}, ctx.Err()
		}
	}
	m.mu.Lock()
	if len(m.script) == 0 {
		m.mu.Unlock()
		return Round{}, &llm.Error{Kind: llm.KindFatal, Err: ErrScriptEnded}
	}
	next := m.script[0]
	m.script = m.script[1:]
	m.mu.Unlock()
	for _, d := range next.Deltas {
		req.Send(d)
	}
	if next.Streamed != nil {
		close(next.Streamed)
	}
	if next.Hold != nil {
		select {
		case <-next.Hold:
		case <-ctx.Done():
			return Round{}, ctx.Err()
		}
	}
	if next.Err != nil {
		return Round{}, next.Err
	}
	return next, nil
}

// response is a round as the scripted CLI plays it. Arguments a round marks
// unreadable ([llm.ToolCall.ArgumentsError]) are sent as a model that wrote
// them would: a JSON value that is not one object.
func response(r Round) llmtest.Response {
	out := llmtest.Response{Text: r.Text, Usage: llm.Usage{Input: r.Input, Output: r.Output}}
	for _, d := range r.Deltas {
		out.Streamed += d.Content
	}
	for _, call := range r.Calls {
		args := "{}"
		switch {
		case call.ArgumentsError != "":
			args = `["not", "an object"]`
		case call.Arguments != nil:
			raw, err := json.Marshal(call.Arguments)
			if err == nil {
				args = string(raw)
			}
		}
		out.Calls = append(out.Calls, llmtest.Call{ID: call.ID, Name: call.Name, Args: args})
	}
	return out
}

// Unready is what the model says, without a call, its next call would meet:
// nil until a suite sets it ([Model.SetUnready]), as a CLI entry that is
// signed out or spent would.
func (m *Model) Unready(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.unready
}

// SetUnready makes [Model.Unready] answer err.
func (m *Model) SetUnready(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unready = err
}

// Requests is every request the model was sent, in order.
func (m *Model) Requests() []llm.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]llm.Request(nil), m.requests...)
}

// Prompts is every request rendered as the text a model would read, for a
// suite asserting what never reaches one.
func (m *Model) Prompts() string {
	var out string
	for _, req := range m.Requests() {
		for _, msg := range req.Messages {
			out += fmt.Sprintf("[%s] %s\n", msg.Role, msg.Content)
		}
	}
	return out
}
