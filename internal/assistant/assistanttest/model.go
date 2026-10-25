package assistanttest

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// Model is a scripted model: it answers each round with the next completion of
// its script and records every request it was sent, so a suite drives a whole
// turn without a real model (the engine rule: tests never call one).
type Model struct {
	// Name is what Model() reports.
	Name string

	mu       sync.Mutex
	script   []Round
	requests []llm.Request
	// Gate, when set, is waited on before each round is answered, so a
	// suite can stop a turn while its model is thinking.
	Gate chan struct{}
}

// Round is one scripted answer: prose, tool calls, or an error.
type Round struct {
	Text  string
	Calls []llm.ToolCall
	Err   error
	// Input and Output are the tokens the round reports spending.
	Input, Output int
	// Deltas are sent, in order, to a request that asked to be streamed,
	// before the round answers: the fragments a streaming CLI would send.
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
func NewModel(rounds ...Round) *Model { return &Model{Name: "scripted", script: rounds} }

// ErrScriptEnded is a round asked for past the script.
var ErrScriptEnded = errors.New("assistanttest: the model's script has ended")

// Model reports the scripted model's name.
func (m *Model) Model() string { return m.Name }

// Complete answers the next round.
func (m *Model) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	if m.Gate != nil {
		select {
		case <-m.Gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	m.mu.Lock()
	m.requests = append(m.requests, req)
	if len(m.script) == 0 {
		m.mu.Unlock()
		return nil, &llm.Error{Kind: llm.KindFatal, Err: ErrScriptEnded}
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
			return nil, ctx.Err()
		}
	}
	if next.Err != nil {
		return nil, next.Err
	}
	c := &llm.Completion{Content: next.Text, ToolCalls: next.Calls, Model: m.Name,
		InputTokens: next.Input, OutputTokens: next.Output}
	if len(next.Calls) > 0 {
		c.StopReason = llm.StopToolUse
	}
	return c, nil
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
