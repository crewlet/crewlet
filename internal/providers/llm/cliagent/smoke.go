package cliagent

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// smokeTool is the one tool the doctor's smoke test offers.
const smokeTool = "crewlet_smoke"

// smokeHost is the doctor's tool host: ONE tool, served on a real toolsession
// server exactly as a seat's are — the relay, the socket, the MCP server — and
// a record of what the CLI did with it.
//
// The doctor's own, rather than the tool loop's: the loop is a phase, with a
// seat, a fence and a budget the doctor has none of, and what the smoke test
// certifies is the route — that this CLI, configured by this profile, starts
// the relay, lists the server's tools and calls one.
type smokeHost struct {
	sessions *toolsession.Host

	mu     sync.Mutex
	calls  []hostCallRecord
	turns  []llm.Turn
	opts   llm.ServeOptions
	listed bool

	done chan struct{}
}

// hostCallRecord is one call the smoke host was handed.
type hostCallRecord struct {
	name string
	args map[string]any
	meta map[string]any
}

var (
	_ llm.ToolHost           = (*smokeHost)(nil)
	_ toolsession.Dispatcher = (*smokeHost)(nil)
)

func newSmokeHost(sessions *toolsession.Host) *smokeHost {
	return &smokeHost{sessions: sessions, done: make(chan struct{})}
}

func (h *smokeHost) Serve(ctx context.Context, opts llm.ServeOptions) (llm.Served, error) {
	session, err := h.sessions.Open(ctx, config.ToolServerName, "crewlet llm doctor", h)
	if err != nil {
		return llm.Served{}, err
	}
	h.mu.Lock()
	h.opts = opts
	h.mu.Unlock()
	command, args := h.sessions.Command(session)
	var once sync.Once
	return llm.Served{Command: command, Args: args, Release: func() bool {
		once.Do(func() {
			session.Retire()
			h.mu.Lock()
			h.listed = session.Connected()
			h.mu.Unlock()
		})
		return session.Connected()
	}}, nil
}

func (h *smokeHost) Observe(t llm.Turn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, seen := range h.turns {
		if seen.ID == t.ID {
			h.turns[i] = t
			return
		}
	}
	h.turns = append(h.turns, t)
}

func (h *smokeHost) Spent(llm.Usage)        {}
func (h *smokeHost) Done() <-chan struct{}  { return h.done }
func (h *smokeHost) Halted() bool           { return false }
func (h *smokeHost) Pending() []llm.Message { return nil }
func (h *smokeHost) Revision() uint64       { return 0 }

func (h *smokeHost) Offered() []toolsession.Tool {
	return []toolsession.Tool{{
		Def: llm.ToolDef{
			Name:        smokeTool,
			Description: "Confirm the tool channel works.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"ok": map[string]any{"type": "boolean"}},
				"required":   []any{"ok"},
			},
		},
		// Read-only, as it is: a CLI that asks before a write would ask
		// nobody here and refuse the call.
		Annotations: mcp.Annotations{ReadOnly: mcp.Yes},
	}}
}

func (h *smokeHost) Call(call toolsession.Call) toolsession.Result {
	h.mu.Lock()
	h.calls = append(h.calls, hostCallRecord{name: call.Name, args: call.Arguments, meta: call.Meta})
	opts := h.opts
	h.mu.Unlock()
	// The backend is told a call started and ended, as a phase's host
	// tells it — the clock it pauses is what keeps a slow doctor run alive.
	if opts.OnCall != nil {
		opts.OnCall(true)
		defer opts.OnCall(false)
	}
	return toolsession.Result{Output: "ok — the tool channel works. Now reply with only: done"}
}

// verdict reads what the CLI did into the smoke line: whether it called the
// tool, how its call was joined to its record, and what its model was offered.
func (h *smokeHost) verdict(offered []string, comp *llm.Completion, observes bool) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	called := slices.ContainsFunc(h.calls, func(c hostCallRecord) bool { return c.name == smokeTool })
	if !called {
		return "", false
	}
	// THE RECORD HAS TO SHOW THE CALL TOO, where the profile reads one: a
	// profile whose requests rules no longer match what its CLI writes
	// still reaches the tools, and nothing else would say that a phase's
	// rounds and spend are not being read as the run goes. Asked on a real
	// sign-in, this is the one check of a record no stand-in can produce
	// (cursor-agent's).
	if observes && !slices.ContainsFunc(h.turns, func(t llm.Turn) bool {
		return slices.ContainsFunc(t.Calls, func(c llm.TurnCall) bool { return c.Host && c.Name == smokeTool })
	}) {
		return fmt.Sprintf("failed — the CLI called %s through the crewlet server, but its record "+
			"never showed the request that asked for it (%d requests read), so the profile's "+
			"requests rules no longer match what the CLI writes: a phase's rounds and spend are "+
			"not read as it runs — set overrides.requests", smokeTool, len(h.turns)), true
	}
	var parts []string
	parts = append(parts, "called "+smokeTool+" through the crewlet server")
	parts = append(parts, h.joinLocked(observes))
	if len(offered) > 0 {
		sorted := slices.Clone(offered)
		sort.Strings(sorted)
		parts = append(parts, "its model was offered "+strings.Join(sorted, ", "))
	}
	parts = append(parts, fmt.Sprintf("%d in / %d out", comp.InputTokens, comp.OutputTokens))
	return "ok — " + strings.Join(parts, "; "), true
}

// joinLocked says how the CLI's call was joined to the request that asked for
// it: by the call id it sent with the call, matched against its record, or by
// name and arguments.
//
// Read off what the CLI sent rather than off a list of keys: any `_meta` value
// equal to a call id its record shows is the join, whatever the CLI calls it.
func (h *smokeHost) joinLocked(observes bool) string {
	if !observes {
		return "its record shows no requests, so a run counts as one round"
	}
	recorded := map[string]bool{}
	for _, t := range h.turns {
		for _, c := range t.Calls {
			if c.ID != "" {
				recorded[c.ID] = true
			}
		}
	}
	var keys []string
	for _, c := range h.calls {
		for _, key := range slices.Sorted(maps.Keys(c.meta)) {
			if id, ok := c.meta[key].(string); ok && recorded[id] && !slices.Contains(keys, key) {
				keys = append(keys, key)
			}
		}
	}
	if len(keys) == 0 {
		return "it sends no call id its record shows, so its calls are joined to its record by " +
			"name and arguments"
	}
	return "its calls are joined to its record by the id it sends in _meta " + strings.Join(keys, ", ")
}

// offeredKey carries where a run with tools reports what its CLI offered its
// model, for the doctor.
type offeredKey struct{}

// withOffered asks a run with tools to report the tools its CLI's record says
// it offered its model.
func withOffered(ctx context.Context, report func([]string)) context.Context {
	return context.WithValue(ctx, offeredKey{}, report)
}

// reportOffered hands the doctor what the record showed, where it asked.
func reportOffered(ctx context.Context, tools []string) {
	if report, ok := ctx.Value(offeredKey{}).(func([]string)); ok {
		report(tools)
	}
}
