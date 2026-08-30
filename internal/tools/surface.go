package tools

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"

	"go.opentelemetry.io/otel/attribute"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tracing"
)

// Surface is the set of tools ONE phase may call, and the thing the tool loop
// drives.
//
// It is a live view, not a fixed list: activating a tool mid-phase adds to it,
// and the loop re-reads ToolDefs at the top of every round so the addition is
// visible on the NEXT provider call rather than the one after.
//
// The snapshot it resolves against is fixed, though. A server restarting
// mid-turn must not change what a phase is judged against between the call and
// the delivery gate.
type Surface struct {
	phase    string
	universe Snapshot

	// turn is who this surface acts for. Nil outside a turn — a validate
	// command, a test driving a runner directly — and the seat-scoped
	// tools refuse rather than guessing, which is the honest answer.
	turn *turnctx.Turn

	// guard is the required-skill gate, or nil where nothing is enforced.
	// Set once by the runner after the surface is built — the guard needs
	// the finished surface to know which skills it is gating, and the
	// surface needs the guard to gate anything, so one of them has to be
	// installed second.
	guard Guard

	mu     sync.Mutex
	active []string
	called []Call
}

// Guard decides whether a tool may be called yet.
//
// Consumer-defined and one method wide, so the tool layer knows nothing
// about skills: it asks whether this call is allowed and reports whatever
// reason comes back. Nil allows everything, which is the ordinary case.
type Guard interface {
	// Check reports why a call must be refused, or "" to allow it. The
	// server is passed because a rule may be about everything one MCP
	// server publishes rather than about one tool name.
	Check(tool, server string) string

	// Observe is told about a call that SUCCEEDED, so a guard whose own
	// unlock is a tool can watch for it.
	//
	// The after-half of the same seam, and it has to be here rather than
	// in the tool: the tool is registered once per company while the
	// guard is per phase session, so a tool holding a guard would hold
	// whichever session registered last. The arguments are passed opaque
	// — this layer has no idea what any of them mean.
	Observe(tool string, args map[string]any)
}

// WithGuard installs the required-skill gate and returns the surface.
func (s *Surface) WithGuard(g Guard) *Surface {
	s.guard = g
	return s
}

// Detached is a tool whose work outlives the call that started it.
//
// The one tool in the engine that has any use for this is run_sandbox: it
// starts a coding run and asks the loop to SUSPEND with the call unanswered,
// so the conversation can be persisted and re-entered — minutes later,
// possibly after a restart, possibly on another node — when the run finishes.
//
// Optional, like [SeatCallable], and for the same reason: a plain Callable is
// unaffected and knows nothing about turns.
type Detached interface {
	Callable

	// CallDetached runs the tool and may ask the loop to SUSPEND with this
	// call unanswered.
	CallDetached(ctx context.Context, turn *turnctx.Turn, args map[string]any) (DetachedResult, error)
}

// DetachedResult is an ordinary result that may also stop the loop.
//
// A SECOND optional interface rather than a Suspend field on [Result], for the
// same reason SeatCallable is one: Result is the MCP tool contract, an MCP
// server has no notion of a turn to suspend, and widening it would put a
// turn-engine concept on every bridged tool to be ignored. This way the one
// tool whose work outlives its turn asks for the ability, and the rest of the
// registry is unaffected.
type DetachedResult struct {
	Result

	// Suspend stops the loop with this call UNANSWERED, so the conversation
	// can be persisted and re-entered when the detached work finishes.
	// Honoured only where the phase allows suspending — Execute — and
	// logged and ignored elsewhere, because a phase that never persists a
	// partial conversation cannot resume one. See d-402.
	Suspend bool

	// Payload is handed to the caller to persist alongside the state.
	Payload map[string]any
}

// SeatCallable is a tool that needs to know which seat is calling it.
//
// The tools that speak FOR a seat: asking a colleague, marking an onboarding
// step, loading a skill this agent synthesized, recalling this agent's own
// episodes. Every one of them is an authorization decision, and every one of
// them would be forgeable if the seat arrived in the model's arguments.
//
// Optional, and checked at the one frame that holds both halves. A plain
// Callable — every MCP tool, every discovery meta-tool — is unaffected and
// knows nothing about turns.
type SeatCallable interface {
	Callable

	// CallForTurn runs the tool on behalf of a turn. A nil turn means
	// there is no acting seat, which a seat-scoped tool reports as a
	// failed Result rather than an error: the model asked for something
	// reasonable in a context that cannot serve it.
	CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (Result, error)
}

var _ toolloop.Surface = (*Surface)(nil)

// Call is one invocation the surface ran, recorded for the ledger and the
// delivery gate.
type Call struct {
	Name   string
	Args   map[string]any
	Output string
	Failed bool
}

// NewSurface builds a phase surface over a snapshot.
//
// active names the tools offered to the model up front. Everything else in the
// snapshot is REACHABLE but not offered — that is what discovery is for, and
// it is why a planner is not handed the whole of a large MCP server's
// catalogue in its prompt.
func NewSurface(phase string, universe Snapshot, active []string) *Surface {
	s := &Surface{phase: phase, universe: universe}
	for _, name := range active {
		s.Activate(name)
	}
	return s
}

// ForTurn binds the acting seat and returns the surface, for chaining.
//
// Set once, at construction, by the frame that built the surface. Not a
// constructor parameter only because every existing call site would have to
// pass nil, which reads as "no seat here" at sites where there simply is not
// one to pass yet.
func (s *Surface) ForTurn(t *turnctx.Turn) *Surface {
	s.turn = t
	return s
}

// Turn reports who this surface acts for, or nil.
func (s *Surface) Turn() *turnctx.Turn { return s.turn }

// Phase names the phase, for telemetry.
func (s *Surface) Phase() string { return s.phase }

// Activate adds a tool to what the model is offered, reporting whether it
// resolved.
//
// It is the only writer of the active list, and it admits nothing the universe
// does not resolve. That invariant is what lets ToolDefs render without a miss
// check — see there.
//
// Idempotent: a model that activates the same tool twice has not made an
// error worth reporting, and a duplicate in the offered list is a duplicate in
// the request the vendor rejects.
func (s *Surface) Activate(name string) bool {
	if _, ok := s.universe.Lookup(name); !ok {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.Contains(s.active, name) {
		return true
	}
	s.active = append(s.active, name)
	return true
}

// Active returns the currently offered names, in activation order.
func (s *Surface) Active() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.active)
}

// ToolDefs renders what the model is offered this round.
func (s *Surface) ToolDefs() []llm.ToolDef {
	s.mu.Lock()
	active := slices.Clone(s.active)
	s.mu.Unlock()

	defs := make([]llm.ToolDef, 0, len(active))
	for _, name := range active {
		// No miss check. Activate is the ONLY writer of active and it
		// admits nothing the universe does not resolve, and the universe
		// is a frozen snapshot — so a name here always resolves.
		//
		// The check was written, mutated away, and nothing noticed. It was
		// guarding against a server restarting mid-phase, which this
		// surface is deliberately immune to: it resolves against the
		// snapshot, not the live registry. Keeping it would have implied a
		// hazard that the snapshot already removes.
		e, _ := s.universe.Lookup(name)
		defs = append(defs, llm.ToolDef{
			Name:        e.Name(),
			Description: e.Tool.Description(),
			Parameters:  paramsOrEmpty(e.Tool.Parameters()),
		})
	}
	return defs
}

// Execute runs one call.
//
// A tool the model asked for but that is not on this surface comes back as an
// ordinary FAILED result, not a Go error: the model asked for something it
// cannot have, which is a thing to tell it about, and an error here would tear
// down a turn over a hallucinated name.
func (s *Surface) Execute(ctx context.Context, call llm.ToolCall) (toolloop.ToolResult, error) {
	// Arguments arrive already decoded — the provider layer owns that,
	// including keeping large integer ids exact.
	args := call.Arguments
	if args == nil {
		args = map[string]any{}
	}

	// THE TOOL SPAN LIVES HERE, not in toolloop, because this is the frame
	// that knows what the call actually was: whether the tool exists, which
	// MCP server it came from, whether a skill guard refused it, and which
	// seat is acting. toolloop sees a name and a phase.
	//
	// It covers all THREE outcomes, and two of them never reach invoke: an
	// unknown or inactive name, and a guard refusal. Those are the calls a
	// reader is most often hunting for — a turn that spent a round on a tool
	// it could not have — and a span opened inside invoke would miss both.
	ctx, span := tracing.Start(ctx, "tools", "tool.call",
		attribute.String("crewlet.tool", call.Name),
		attribute.String("crewlet.phase", s.Phase()))
	defer span.End()

	s.mu.Lock()
	offered := slices.Contains(s.active, call.Name)
	s.mu.Unlock()
	e, known := s.universe.Lookup(call.Name)
	if !known || !offered {
		// The two readings are DIFFERENT and the model can act on the
		// difference: a name that exists but was not offered is something
		// to activate, a name that does not exist is something to stop
		// trying.
		msg := fmt.Sprintf("Unknown tool: %s", call.Name)
		if known {
			msg = fmt.Sprintf("Tool %s is not active on this surface — activate it first.", call.Name)
		}
		s.record(Call{Name: call.Name, Args: args, Output: msg, Failed: true})
		span.SetAttributes(
			attribute.Bool("crewlet.tool_failed", true),
			attribute.String("crewlet.tool_outcome", outcomeFor(known)))
		return toolloop.ToolResult{Output: msg, Failed: true}, nil
	}

	if s.guard != nil {
		server, _ := e.FromMCP()
		if reason := s.guard.Check(call.Name, server); reason != "" {
			// RECORDED AS A FAILED CALL, deliberately: the model sees the
			// reason and can act on it, and the ledger shows an operator
			// that the turn spent a round here rather than that the tool
			// silently did nothing.
			s.record(Call{Name: call.Name, Args: args, Output: reason, Failed: true})
			span.SetAttributes(
				attribute.Bool("crewlet.tool_failed", true),
				attribute.String("crewlet.tool_outcome", "refused_by_guard"))
			return toolloop.ToolResult{Output: reason, Failed: true}, nil
		}
	}

	if server, ok := e.FromMCP(); ok {
		// WHICH server, so a slow or failing MCP child is visible as
		// itself rather than as "some tool was slow". The registry records
		// the origin at registration and this is the last frame that knows.
		span.SetAttributes(attribute.String("crewlet.tool_server", server))
	}

	res, err := s.invoke(ctx, e.Tool, args)
	if err != nil {
		tracing.Fail(span, err)
		// The caller's own context ended — the turn is being torn down.
		// Nothing is reported to the model and nothing is recorded: this
		// call did not happen as far as the ledger is concerned.
		return toolloop.ToolResult{}, err
	}
	s.record(Call{Name: call.Name, Args: args, Output: res.Output, Failed: res.Failed})
	if s.guard != nil && !res.Failed {
		// SUCCESS ONLY. A load that named a key nobody has comes back
		// failed, and treating it as an unlock would let a typo open
		// every tool the real skill was gating.
		s.guard.Observe(call.Name, args)
	}
	span.SetAttributes(
		attribute.Bool("crewlet.tool_failed", res.Failed),
		attribute.String("crewlet.tool_outcome", invokedOutcome(res.Failed, res.Suspend)))
	return toolloop.ToolResult{
		Output: res.Output, Failed: res.Failed,
		Suspend: res.Suspend, SuspendPayload: res.Payload,
	}, nil
}

// outcomeFor names the two readings of a call that never reached a tool. The
// difference matters to a reader for the same reason it matters to the model:
// one is a tool to activate, the other a name that does not exist.
func outcomeFor(known bool) string {
	if known {
		return "not_active"
	}
	return "unknown_tool"
}

// invokedOutcome names what a real invocation did.
//
// `suspended` is its own outcome rather than a flavour of success: a
// suspending call is the one that returns NO ledger row — toolloop leaves it
// unanswered and appends no Execution — so this span is the only in-band
// record that `run_sandbox` was ever called.
func invokedOutcome(failed, suspended bool) string {
	switch {
	case suspended:
		return "suspended"
	case failed:
		return "failed"
	default:
		return "ok"
	}
}

// invoke runs one tool, telling it who is calling when it asks.
//
// THE SEAT COMES FROM THE SURFACE, never from the arguments. A tool that acts
// for a seat — asking a colleague, marking an onboarding step, writing to a
// diary — must not be able to be pointed at another one by a model that
// spelled a different handle in its arguments. The surface is built per turn
// by the runner, which is the frame that knows.
//
// An OPTIONAL interface rather than a parameter on Callable, because Callable
// is the MCP tool contract as well: an MCP server has no notion of a turn, and
// widening the signature would put a turn-shaped argument on every bridged
// tool to be ignored. This way the two kinds of tool coexist under one
// registry, and the one that needs the fact asks for it.
func (s *Surface) invoke(ctx context.Context, tool Callable, args map[string]any) (DetachedResult, error) {
	switch t := tool.(type) {
	case Detached:
		return t.CallDetached(ctx, s.turn, args)
	case SeatCallable:
		res, err := t.CallForTurn(ctx, s.turn, args)
		return DetachedResult{Result: res}, err
	default:
		res, err := tool.Call(ctx, args)
		return DetachedResult{Result: res}, err
	}
}

func (s *Surface) record(c Call) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.called = append(s.called, c)
}

// Calls returns what this surface ran, in order.
func (s *Surface) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.called)
}

// CalledNames returns the names this surface ran, in order, with repeats.
func (s *Surface) CalledNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.called))
	for _, c := range s.called {
		out = append(out, c.Name)
	}
	return out
}

// Universe is the snapshot this surface resolves against — the full
// catalogue, not just what is offered. The delivery gate reads it, because a
// tool discovered and called mid-phase is still a real delivery.
func (s *Surface) Universe() Snapshot { return s.universe }

// paramsOrEmpty guarantees a non-nil schema object.
//
// A nil parameters map marshals to `null`, which vendors reject as an invalid
// tool schema — so one zero-argument tool fails the whole request rather than
// just itself.
func paramsOrEmpty(p map[string]any) map[string]any {
	if len(p) == 0 {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return maps.Clone(p)
}
