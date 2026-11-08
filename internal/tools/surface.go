package tools

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools/toolargs"
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

	// counter counts the refusal-loop guard's decisions and, handed to
	// every call's [Invoke], the calls answered outside their schema, or
	// nil ([Surface.WithCounter]).
	counter Counter

	// holder records a call to a tool at Needs approval as a question a
	// person answers, or nil where nothing can wait on one
	// ([Surface.WithHolder]).
	holder Holder

	mu     sync.Mutex
	active []string
	called []Call
	// streaks are the runs of policy refusals the refusal-loop guard is
	// counting, by tool and target ([Surface.settle]).
	streaks map[streakKey]int
	// revision counts the changes to active, so a renderer can tell a
	// surface that moved from one that did not without rebuilding what it
	// renders to find out. See [Surface.Revision].
	revision uint64
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
	// (this layer has no idea what any of them mean), and canonical
	// ([toolargs]), the form the tool read.
	Observe(tool string, args toolargs.Args)
}

// Holder records a call to a tool at Needs approval ([Entry.NeedsApproval]) as
// a question a person answers, and says to the caller what became of it.
//
// ONLY AN EXECUTOR'S SURFACE HAS ONE, because only an executor has a run to
// park: its question rides the run's row, its harness ends its turn, and the
// person's answer carries the run on (ADR-0068, ADR-0077). Every other phase
// runs inside a turn on the engine host that cannot wait days for a person, so
// its surface has none, and a call it makes to such a tool is refused here.
//
// Consumer-defined and one method wide, so this layer knows nothing about
// runs, rows or seals: it hands over the call and reports what comes back.
type Holder interface {
	// Hold records call as the question its run's job parks on, and
	// returns what the caller is told: held true where it is recorded (the
	// run now waits on a person and its harness is to end its turn), false
	// with the reason where it was not, and nothing was asked.
	Hold(ctx context.Context, turn *turnctx.Turn, call HeldCall) (result Result, held bool)
}

// HeldCall is one call a [Holder] is handed: the tool as the surface resolved
// it, and its arguments in their canonical form ([Surface.ArgsOf]), which are
// what a person approves and what runs once they do.
type HeldCall struct {
	Name  string
	Entry Entry
	Args  map[string]any
	// Changed is a call held because the tool's definition changed since
	// its level was decided ([Entry.Unreviewed]), not because of the level:
	// the question says so, since what releases the tool for good is
	// accepting the change.
	Changed bool
}

// WithHolder installs what records a call to a tool at Needs approval, and
// returns the surface. Set once, by the frame that builds an executor's
// surface; a surface without one refuses every such call ([Holder]).
func (s *Surface) WithHolder(h Holder) *Surface {
	s.holder = h
	return s
}

// WithGuard installs the required-skill gate and returns the surface.
func (s *Surface) WithGuard(g Guard) *Surface {
	s.guard = g
	return s
}

// SeatCallable is a tool that needs to know which seat is calling it.
//
// The tools that speak FOR a seat: asking a colleague, marking an onboarding
// step, loading a skill this agent synthesized, recalling this agent's own
// episodes. Every one of them is an authorization decision, and every one of
// them would be forgeable if the seat arrived in the model's arguments.
//
// AND THE ONE WHOSE WORK IS THE CALL'S OWN: `delegate`, whose workers count
// their calls on the log of the turn it is called as — the call's own, where
// the surface admitted it on one ([Surface.Admit]) — so the run's record can
// keep their calls with the call that made them.
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

// TurnBound is a tool that is TOLD the turn a seat's call is made in without
// being seat-scoped: the builtin gate, which decides every call a seat makes
// inside a turn somebody woke for whoever woke it too (the requester ceiling,
// ADR-0109), and answers a call made outside any turn as the plain Callable it
// wraps — so a read-only builtin still answers the operator's surface and the
// HTTP one, and is never refused there as a seat's tool would be.
//
// Its own interface, never [SeatCallable]: a seat-scoped tool is one that
// cannot run without a seat, and a wrapper claiming that for every builtin
// would make each read-only one fail outside a turn.
type TurnBound interface {
	Callable

	// CallInTurn runs the tool for a seat's call inside turn, which is nil
	// for a turn nobody can name.
	CallInTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (Result, error)
}

var _ toolloop.Surface = (*Surface)(nil)

// Call is one invocation the surface ran, recorded for the ledger and the
// delivery gate.
type Call struct {
	Name   string
	Args   map[string]any
	Output string
	Failed bool

	// Cause is WHY a failed call failed, as the returned
	// [toolloop.ToolResult] carries it: the tool's own [Result.Cause] for a
	// call a tool answered, the surface's class for a call it refused before
	// any tool ran, and nil for a success or a third-party MCP server's
	// failure.
	//
	// THE ONE VALUE, and [Call.Refusal] and [Call.Unknown] read it: a class
	// recorded beside the cause would be a second classifier of one failure,
	// and the two drift — see [mcp.Result.Cause].
	Cause error
}

// Refusal is the call's class, read out of its cause by [RefusalOf]: "" for a
// success and for a failure nothing in this engine classified.
func (c Call) Refusal() Refusal {
	return RefusalOf(Result{Output: c.Output, Failed: c.Failed, Cause: c.Cause})
}

// Unknown reports whether the call's write may have landed, by [UnknownOf].
func (c Call) Unknown() bool {
	return UnknownOf(Result{Output: c.Output, Failed: c.Failed, Cause: c.Cause})
}

// NewSurface builds a phase surface over a snapshot.
//
// active names the tools offered to the model up front. Everything else in the
// snapshot is REACHABLE but not offered — that is what discovery is for, and
// it is why an executor is not handed the whole of a large MCP server's
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
	s.revision++
	return true
}

// Revision counts the changes to the active set.
//
// For a renderer that has to keep an EXTERNAL copy of what is offered and
// cannot see when the set moved: the MCP bridge holds one MCP server per run
// and re-renders after every bridged call, since a bridged `activate_tool` is
// the only thing that can widen the surface of a run the engine is not
// driving. Almost none of those calls change anything, and [Surface.ToolDefs]
// deep-clones every active tool's JSON Schema — so asking costs one integer
// where finding out costs the whole rendering.
//
// A COUNTER RATHER THAN A DIRTY FLAG, because there is more than one reader
// and a flag the first one cleared would hide the change from the second.
func (s *Surface) Revision() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision
}

// Active returns the currently offered names, in activation order.
func (s *Surface) Active() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.active)
}

// AnnotationsOf is one active tool's behavioural hints, as the registry holds
// them.
//
// SEPARATE FROM [Surface.ToolDefs], because the two answer different clients:
// a [llm.ToolDef] is what a MODEL is given — a name, a description and a
// schema, and a model has no use for a destructive hint — while an MCP client
// is a program that may ask a person before an irreversible call, and the hint
// is the only thing it has to ask on.
//
// It exists because the bridge advertised none. A sandboxed harness was
// handed a seat's whole grant with every tool unannotated, so its own client
// could not tell `get_work_item` from `remove_work_item` — while the registry
// behind it had carried both decisions the whole time.
//
// A name that is not in the universe answers the zero set, which is the
// correct reading of "nothing was advertised" rather than four false hints.
func (s *Surface) AnnotationsOf(name string) Annotations {
	e, held := s.universe.Lookup(name)
	if !held {
		return Annotations{}
	}
	return e.Annotations
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
	return s.execute(ctx, call.Name, s.ArgsOf(call.Name, call.Arguments), s.turn, false, false)
}

// RunApproved makes a call to a tool at Needs approval that a person approved:
// exactly [Surface.Execute] for a tool this surface resolves, offered to the
// model or not, without the approval it already has. Its caller is the one
// that carries a held call's run on with the person's answer (internal/agent/
// runner), and it makes the call once, as the person approved it: the name and
// the arguments recorded when it was held.
//
// THE TOOL IS RESOLVED NOW, against this surface's universe, never against
// what it was when the call was held: a tool blocked since is filed nowhere
// and answers not found, and the caller holds the definition to the one the
// person read ([mcp.Definition.Digest]) — and the call to the surface's guard
// ([Surface.GuardRefusal]) — before it calls this.
func (s *Surface) RunApproved(ctx context.Context, name string, raw map[string]any) (toolloop.ToolResult, error) {
	return s.execute(ctx, name, s.ArgsOf(name, raw), s.turn, false, true)
}

// GuardRefusal is why the surface's guard would refuse a call to name made now
// — a required skill covering the tool that the session has not loaded — and
// "" where it would admit one, where the surface does not resolve the name, or
// where nothing guards the surface.
//
// FOR A CALLER THAT MUST KNOW BEFORE IT COMMITS TO THE CALL: one making a call
// a person approved ([Surface.RunApproved]) marks it made on its run's row
// before it makes it, so that it is made at most once, and a refusal met only
// once the call runs would be recorded as the call's result — the approval used
// up on a call that never ran. Asked here first, the call is not made and the
// mark is never written; [Surface.RunApproved] asks the same guard again as the
// call runs.
func (s *Surface) GuardRefusal(name string) string {
	if s.guard == nil {
		return ""
	}
	e, known := s.universe.Lookup(name)
	if !known {
		return ""
	}
	server, _ := e.FromMCP()
	return s.guard.Check(name, server)
}

// ArgsOf is a call's arguments in the canonical form, made ONCE, at the
// surface's entry ([Snapshot.ArgsOf]): the admission, the run's call log, the
// guard, the refusal-loop target, the check, the tool and the ledger all read
// this form and never the one the model sent.
func (s *Surface) ArgsOf(name string, raw map[string]any) toolargs.Args {
	return s.universe.ArgsOf(name, raw)
}

// DigestOf is the identity a call to name with raw arguments is admitted
// under in its run's call log: [turnctx.ArgsDigest] of the canonical arguments
// ([Surface.ArgsOf]), the value [Surface.Admit] records.
//
// ONE RULE, called beside the admission by a caller that records the call
// somewhere the log is later seeded from — a bridged run's tool log — so the
// record carries the identity the log counted, whatever it keeps of the
// arguments.
func (s *Surface) DigestOf(name string, raw map[string]any) string {
	return turnctx.ArgsDigest(s.ArgsOf(name, raw).Map())
}

// Admit records a call in its run's call log NOW, in the order calls arrive,
// for a caller that runs calls side by side — see [turnctx.CallLog.Admit] for
// why a count read against the order calls RETURN in is one a re-run does not
// reproduce — and returns the call bound to that place. The caller admits each
// call under its own lock, in arrival order, and runs it once with what this
// returns: exactly [Surface.Execute], except that the tool counts its repeats
// from the log as it stood at the admission, and the call, already in the log,
// is not recorded there again. What the call records on its own view is handed
// back to the run's log when it returns.
//
// AT ADMISSION, so a call the surface goes on to refuse — an unknown name, a
// guard — is in the log as well, and so is one its caller admitted and then
// never ran. That can only raise a later count, and a raised count is a fresh
// operation; see [turnctx.CallLog].
//
// The admitted call runs with the arguments admitted here, in their canonical
// form; the call it is handed is read for nothing but its name.
//
// AND WHAT THE CALL'S OWN WORK RECORDED rides its result
// ([toolloop.ToolResult.Absorbed]), the calls the run's log absorbs from it
// as it returns, for a caller that records the call where a later segment's
// log is seeded from.
//
// EVEN WHERE THE CALL PANICS: what its work recorded by then is in the run's log
// all the same, so the panic is raised again carrying it
// ([toolloop.CallPanic]), for the caller that recovers it to record beside the
// call that broke. A call whose work recorded nothing panics as it did.
func (s *Surface) Admit(name string, raw map[string]any) toolloop.Admitted {
	args := s.ArgsOf(name, raw)
	calls := s.turn.CallLog().Admit(name, args)
	return func(ctx context.Context, _ llm.ToolCall) (toolloop.ToolResult, error) {
		turn := s.turn
		if calls != nil {
			turn = s.turn.WithCalls(calls)
			defer s.turn.CallLog().Absorb(calls)
		}
		defer func() {
			// RECOVERED ONLY WHERE THERE IS SOMETHING TO CARRY, so every
			// other panic reaches its recovery untouched.
			if own := ownWork(calls); len(own) > 0 {
				if v := recover(); v != nil {
					panic(&toolloop.CallPanic{Value: v, Absorbed: own})
				}
			}
		}()
		res, err := s.execute(ctx, name, args, turn, true, false)
		res.Absorbed = ownWork(calls)
		return res, err
	}
}

// ownWork is what an admitted call's own work recorded on its log
// ([turnctx.CallLog.Own]), as a call's result hands it on.
func ownWork(calls *turnctx.CallLog) []toolloop.Called {
	own := calls.Own()
	if len(own) == 0 {
		return nil
	}
	out := make([]toolloop.Called, 0, len(own))
	for _, c := range own {
		out = append(out, toolloop.Called{Tool: c.Tool, Digest: c.Digest})
	}
	return out
}

// execute is [Surface.Execute] as turn — the surface's own, or a view of it
// whose call log is an admitted call's ([Surface.Admit]). admitted is false for
// a call nobody admitted, which the run's log records once it has returned.
// approved is true only for a held call a person approved
// ([Surface.RunApproved]), which runs whether or not the tool is offered and
// is not held again.
//
// It takes the call's name and its canonical arguments ([Surface.ArgsOf]),
// never the call as the model sent it, so no step here can read the raw form.
func (s *Surface) execute(ctx context.Context, name string, args toolargs.Args,
	turn *turnctx.Turn, admitted, approved bool,
) (toolloop.ToolResult, error) {

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
		attribute.String("crewlet.tool", name),
		attribute.String("crewlet.phase", s.Phase()))
	defer span.End()

	s.mu.Lock()
	offered := slices.Contains(s.active, name)
	s.mu.Unlock()
	e, known := s.universe.Lookup(name)
	if !known || (!offered && !approved) {
		// The two readings are DIFFERENT and the model can act on the
		// difference: a name that exists but was not offered is something
		// to activate, a name that does not exist is something to stop
		// trying.
		//
		// THE CLASS FOLLOWS THE SAME SPLIT: a name nothing registered is
		// not_found, and one that exists but was not offered to THIS
		// surface is forbidden — the call is refused for where it was
		// made rather than for what it named.
		//
		// WHOSE TOOL IT WAS rides a refusal of a name that exists
		// ([toolloop.ToolResult.Source]): nothing answered it, but it was a
		// call for that server's tool, refused here, which is what its
		// count of refused calls is.
		msg, class, source := fmt.Sprintf("Unknown tool: %s", name), RefusalNotFound, ""
		if known {
			msg = fmt.Sprintf("Tool %s is not active on this surface; activate it first.", name)
			class, source = RefusalForbidden, e.Origin
		}
		s.record(Call{Name: name, Args: args.Map(), Output: msg, Failed: true, Cause: class})
		span.SetAttributes(
			attribute.Bool("crewlet.tool_failed", true),
			attribute.String("crewlet.tool_outcome", outcomeFor(known)))
		return toolloop.ToolResult{Output: msg, Failed: true, Cause: class, Source: source}, nil
	}

	if s.guard != nil {
		server, _ := e.FromMCP()
		if reason := s.guard.Check(name, server); reason != "" {
			// RECORDED AS A FAILED CALL, deliberately: the model sees the
			// reason and can act on it, and the ledger shows an operator
			// that the turn spent a round here rather than that the tool
			// silently did nothing.
			// FORBIDDEN, because the guard refuses the call as made
			// here and now (a skill not yet loaded), never its
			// arguments.
			s.record(Call{Name: name, Args: args.Map(), Output: reason, Failed: true,
				Cause: RefusalForbidden})
			span.SetAttributes(
				attribute.Bool("crewlet.tool_failed", true),
				attribute.String("crewlet.tool_outcome", "refused_by_guard"))
			return toolloop.ToolResult{Output: reason, Failed: true,
				Cause: RefusalForbidden, Source: e.Origin}, nil
		}
	}

	if asks, changed := e.asksFirst(); asks && !approved {
		return s.hold(ctx, span, name, e, args, turn, changed), nil
	}

	if server, ok := e.FromMCP(); ok {
		// WHICH server, so a slow or failing MCP child is visible as
		// itself rather than as "some tool was slow". The registry records
		// the origin at registration and this is the last frame that knows.
		span.SetAttributes(attribute.String("crewlet.tool_server", server))
	}

	_, fromMCP := e.FromMCP()
	target := TargetOf(e.Tool, args.Map())
	// THROUGH [Invoke], which holds a first-party call to the schema its
	// tool advertises (ADR-0059): a value the tool would misread is refused
	// before anything runs, and every other finding rides on the answer.
	//
	// THE ACTING SEAT IS ATTACHED HERE, which is the one frame every seat
	// tool call goes through: the authority decision reads the principal
	// off the context on every call, and until this line nothing produced
	// one for a seat at all. An outer answer wins — see
	// [turnctx.WithPrincipal] — so a surface reached through somebody's
	// own credential still acts as them.
	res, refusedByContract, err := Invoke(turnctx.WithPrincipal(ctx, turn), Invocation{
		Name: name, Tool: e.Tool, Args: args, AsSeat: true, Turn: turn,
		ThirdParty: fromMCP, Where: s.Phase(), Counter: s.counter,
	})
	if err != nil {
		tracing.Fail(span, err)
		// The caller's own context ended: the turn is being torn down.
		// Nothing is reported to the model and nothing is recorded: this
		// call did not happen as far as the ledger is concerned.
		return toolloop.ToolResult{}, err
	}
	if refusedByContract {
		recorded, run := s.settle(Call{Name: name, Args: args.Map(), Output: res.Output,
			Failed: true, Cause: RefusalInvalid}, e.Annotations, target, false)
		s.reportLoop(name, target, run, turn)
		span.SetAttributes(
			attribute.Bool("crewlet.tool_failed", true),
			attribute.String("crewlet.tool_outcome", "refused_by_contract"))
		return toolloop.ToolResult{Output: recorded.Output, Failed: true, Cause: RefusalInvalid,
			Source: e.Origin}, nil
	}
	// THE MODEL READS TEXT, so the note is rendered after the output here
	// ([Rendered]), and the ledger records what the model read.
	recorded, run := s.settle(Call{Name: name, Args: args.Map(), Output: Rendered(res),
		Failed: res.Failed, Cause: res.Cause}, e.Annotations, target, e.OutputUnkept)
	res.Output = recorded.Output
	s.reportLoop(name, target, run, turn)
	// INTO THE RUN'S LOG TOO, failed or not: a call that reports a failure
	// may still have written part of what it asked for (a gesture stopped
	// at a step whose outcome is unknown), and a call the log never heard
	// of is one a later count misses — which hands a repeat its first
	// copy's id. Recording one that wrote nothing only raises a count, and
	// a raised count is a fresh operation. See [turnctx.CallLog]. An
	// ADMITTED call is in it already, from the moment it arrived.
	if !admitted {
		turn.CallLog().Record(name, args)
	}
	if s.guard != nil && !res.Failed {
		// SUCCESS ONLY. A load that named a key nobody has comes back
		// failed, and treating it as an unlock would let a typo open
		// every tool the real skill was gating.
		s.guard.Observe(name, args)
	}
	span.SetAttributes(
		attribute.Bool("crewlet.tool_failed", res.Failed),
		attribute.String("crewlet.tool_outcome", invokedOutcome(res.Failed)))
	// WHO ANSWERED, stamped here because this is the one frame that
	// resolved the name to a registered entry — the registry records the
	// origin at registration and the loop sees only a name. Only on this
	// path: the refusals above never reached a tool, so they name none.
	server, _ := e.FromMCP()
	return toolloop.ToolResult{
		Output: res.Output, Failed: res.Failed, Cause: res.Cause,
		Origin: e.Origin, Server: server, Source: e.Origin, Unkept: e.OutputUnkept,
	}, nil
}

// hold decides a call to a tool at Needs approval, which never runs here: an
// executor's surface records it as the question its run parks on ([Holder]),
// and every other phase is refused it.
//
// # Why a phase on the engine host is refused rather than parked
//
// A reviewer, a delegated worker, the onboarding pass and the side passes each
// run inside one turn on the engine host, with the seat's other work queued
// behind it, and none of them has a run whose row a question rides or whose
// answer could carry it on days later. Parking the whole turn instead would
// hold the seat for as long as a person takes. So the call is refused as
// FORBIDDEN, as a call refused for where it was made, and the model is told
// that the seat's own run, its executor, can ask (the series' question 4).
//
// # What is recorded
//
// The call, FAILED either way, since nothing it asked for happened: a held
// call counted as made would satisfy the delivery gate for a message nobody
// sent. A held one carries [toolloop.ToolResult.Held], which tells the run's
// record that it ended the job, and is recorded without its arguments, which
// ride the run's row sealed.
func (s *Surface) hold(ctx context.Context, span trace.Span, name string, e Entry,
	args toolargs.Args, turn *turnctx.Turn, changed bool) toolloop.ToolResult {

	if s.holder == nil {
		seat := "this agent"
		if handle := turn.Handle(); handle != "" {
			seat = handle
		}
		why := "it needs a person's approval before each call"
		if changed {
			why = "its definition changed since it was allowed, so it needs a person's " +
				"approval before each call until somebody accepts the change"
		}
		msg := fmt.Sprintf("%s was not called: %s, "+
			"and this %s phase cannot wait for an answer. Only %s's own run, its executor, can "+
			"ask a person to approve a call. Carry on without it, or leave the call to that run.",
			name, why, s.Phase(), seat)
		s.record(Call{Name: name, Args: args.Map(), Output: msg, Failed: true, Cause: RefusalForbidden})
		span.SetAttributes(
			attribute.Bool("crewlet.tool_failed", true),
			attribute.String("crewlet.tool_outcome", "refused_needs_approval"))
		return toolloop.ToolResult{Output: msg, Failed: true, Cause: RefusalForbidden, Source: e.Origin}
	}
	res, held := s.holder.Hold(ctx, turn, HeldCall{Name: name, Entry: e, Args: args.Map(), Changed: changed})
	cause := res.Cause
	if held || cause == nil {
		// HELD IS NOT A FAILURE OF ANYBODY'S, but the call did not happen,
		// so it is recorded failed with the class of a call refused for
		// where it was made, and marked held.
		cause = RefusalForbidden
	}
	recorded := args.Map()
	if held {
		// ITS ARGUMENTS ARE NOT IN THE TURN'S LEDGER: they ride the run's
		// row sealed, opened for who may approve the call, and the
		// ledger is read by everybody who reads the seat's trail. The
		// call a person approves is recorded whole once it is made.
		recorded = nil
	}
	s.record(Call{Name: name, Args: recorded, Output: res.Output, Failed: true, Cause: cause})
	outcome := "held_for_approval"
	if !held {
		outcome = "not_held"
	}
	span.SetAttributes(
		attribute.Bool("crewlet.tool_failed", true),
		attribute.String("crewlet.tool_outcome", outcome))
	return toolloop.ToolResult{Output: res.Output, Failed: true, Cause: cause, Held: held,
		Source: e.Origin}
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
func invokedOutcome(failed bool) string {
	if failed {
		return "failed"
	}
	return "ok"
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
	out := maps.Clone(p)
	// AN OBJECT SCHEMA OR NOTHING. A tool's parameters reach two consumers
	// that both require the top-level `type: "object"` the MCP spec
	// mandates (a vendor's tool-definition API, which rejects the whole
	// request, and the MCP server the bridge builds, whose AddTool PANICS),
	// and for an MCP tool this map came off a third-party server's wire
	// verbatim. A value off the wire must be a value rather than a panic,
	// and the panic's blast radius is the seat: it is raised on the turn
	// goroutine that opens the bridge, so the wake naks, redelivers and
	// panics again until the seat dead-letters.
	//
	// A missing type is the ordinary malformation and the properties are
	// still good, so it is filled in. A type that is present and says
	// something else describes something that cannot be a tool's argument
	// object at all, so the schema is replaced rather than believed —
	// leaving the tool callable with whatever the model sends, which is
	// what the surface's own dispatch does with it in any case.
	switch out["type"] {
	case "object":
	case nil:
		out["type"] = "object"
	default:
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return out
}
