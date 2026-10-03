// Package operator serves the company's own tracker and knowledge base to the
// people who run it: ONE catalogue of tools, built for every call from what
// this node serves at that moment, and every transport that reaches it
// drawing from that one value through one dispatch.
//
// # Why this exists
//
// The premise of running Crewlet is that an AI manages your company. The
// person doing that management is very often working through an AI of their
// own — a coding agent, an assistant, whatever they already have open — and
// that assistant needs to be able to ask what is on the board, file the thing
// they just decided, and read what the company has written down. Without this
// they are reduced to describing the dashboard to it.
//
// # One catalogue, one dispatch, every transport
//
// The catalogue (catalogue.go) is the tool set, resolved against this
// company's backends by [builtin.OperatorTools]. THREE transports adapt a
// request into a call through [Server.Dispatch] and nothing else: MCP at
// [MCPPath] for an assistant (mcp.go), the act route at [ActPattern] for a
// person at the dashboard (act.go), and the human write surface's tool-backed
// routes (internal/api/workapi) for a script or the CLI. None of them decides
// which tools exist, what a schema says, which hints a tool carries, how a
// refusal is worded, what status it is answered with ([Fail], refusal.go) or
// what is recorded about the call (audit.go). A transport that built its own
// catalogue would be a second call to the same constructor with a second
// chance to pass it different deps — and the difference would surface as a
// verb that works from one surface and is missing from the other, which
// nobody tests for because each surface looks complete from inside.
//
// # Built for every call, never captured
//
// The catalogue is the native halves' — a node's tracker and knowledge base
// come up with its FIRST COMPANY, which a node that booted with none meets at
// an apply, long after its API started serving — and the knowledge search
// beside them is whatever backend the company runs NOW, which an apply can
// add. So [Options.Halves] is read at the start of every call, and a node that
// has not met its company answers `503 no_active_revision` while one whose
// catalogue is empty answers as the route's absence would. A catalogue built
// once, when the API was wired, served the halves the node had at that moment
// for the life of the process: none, on the node a company was bootstrapped
// on.
//
// # It is the SAME TOOLS a seat holds, with a different writer
//
// Not a parallel implementation. Every tool here is the one from
// [internal/agent/builtin], constructed with [builtin.WorkDeps.Actor] set —
// so a schema, a default, a trimmed field and the wording of a refusal are
// each written once. Two copies of "file an item" drift on exactly the parts
// nobody looks at, and only one of the two is ever tested.
//
// What differs is WHO the write is attributed to, and that is the request's
// PRINCIPAL, read off its context ([builtin.PrincipalActor]): a person bound to
// a seat writes AS that seat, with kind `human`; a credential nobody is bound
// through writes under its own whole login, with kind `operator`; and the
// credential the call came through rides beside either as the operator id
// ([iam.ActorFor]). There is deliberately no way for a caller to name a seat
// to act as: a tracker whose author field is chosen by the writer is not an
// audit trail. The principal is resolved ONCE, by the request guard, and is a
// value on the context — so the dispatch, the tool's actor and the audit
// record read one answer, and a directory rebind or a chart rename landing
// while a call runs changes the next request, never this one.
//
// # A retry is the same operations
//
// A person has no turn and nothing redelivers their call, but they RETRY: a
// write whose answer never arrived is sent again. The transports differ in
// who names the operation:
//
//   - the act route and the human write surface take it from the
//     `Idempotency-Key` header (internal/api/opkey), SCOPED by the principal
//     that sent it, and every write the call makes derives its id from that
//     key — the tracker's through [builtin.Actor.WorkKey], the knowledge
//     base's through [pages.Actor.OpKey] — so the request sent again is the
//     first attempt's writes rather than new ones;
//   - an MCP client names none, so each of its calls is a new operation, and a
//     tracker write answers with the `op_id` that operation was, which the
//     assistant sends back with the same arguments to finish a write that
//     answered `unknown` or stopped part of the way through.
//
// A call that names its operation in the header and ALSO carries an `op_id`
// argument is refused `400`: two answers to "which operation is this", one of
// them the caller's pick ([NoOperationArg]).
//
// # It is ALWAYS authenticated, and every principal may call it
//
// Unlike the sandbox bridge at [mcpbridge.PathPrefix], which authenticates
// with a signed per-run token in its own path because the box inside holds no
// API credential, this surface is reached by a person's own client and sits
// behind the ordinary request guard — it is not on the exemption list, and it
// is under its own prefix rather than /mcp/. WHO MAY CALL WHAT is the
// authority table's, through [Options.Authorize], for every principal the
// guard resolved: nobody is refused for being unbound to a seat (ADR-0024),
// because a credential acting as itself is a legitimate author and the table
// already says what each one may do.
//
// # Every call that may write is audited, on every transport
//
// The history of a work item or a page records what CHANGED; it has nothing
// to say about a call that was refused, one whose answer never came back, or
// a verb that is not a tracker or wiki write. So every call that is not a
// proven read publishes one `operator_acted` event (audit.go) — who, through
// which credential, on which transport, the tool, the operation and what
// became of it, never the arguments — and the event store writes it on this
// node. It is done in the ONE dispatch every transport calls, so what is
// recorded about a person cannot depend on which surface they used. [New]
// refuses a surface with no [Options.Audit].
package operator

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/logging"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
)

var log = logging.Get("api.operator")

// Halves are the company's own tracker, knowledge base and knowledge search as
// ONE call finds them. Their Actor and Authorize fields are this surface's to
// set and are overwritten: the actor is the request's principal carrying the
// call's operation, and the decision is [Options.Authorize].
type Halves struct {
	// Work and Pages are the native backends. A zero half serves none of
	// its tools — the honest shape for a company on Jira or Confluence.
	Work  builtin.WorkDeps
	Pages builtin.PageDeps

	// Knowledge is the company's ranked search, or nil. It is offered
	// whatever backend the company runs, native or not: ranked search over
	// the company's own wiki is exactly as useful on Confluence.
	Knowledge builtin.KnowledgeSearcher

	// Company names the company in the MCP server's own title, so an
	// operator with two of these connected can tell which is which.
	Company string
}

// Options configure the surface.
type Options struct {
	// Halves is what the catalogue is built from, read at the start of
	// every call — never once, when the surface is built. False is a node
	// that has not been handed a company yet, which every transport answers
	// `503 no_active_revision`. REQUIRED.
	Halves func() (Halves, bool)

	// Org is the company chart, resolved per call because a config apply
	// replaces it. An operator has no turn to carry one: `search_knowledge`
	// is scoped against it, and a pause resolves the seat it names through
	// it. Nil serves neither.
	Org func() *org.Organization

	// Authorize decides whether the party behind a call may make it, and
	// it is the SAME decision a seat's own registry is built with — one
	// table, one function, every surface. REQUIRED: a surface that wired no
	// decision would serve every verb to every caller while looking exactly
	// like one that had, so [New] refuses it rather than serving the
	// refusal [builtin.OperatorTools] makes of a nil one on every call.
	Authorize builtin.Authorizer

	// Fleet reads what each node's build can carry out, off the presence
	// heartbeat, so a verb carried out by the node holding a seat is
	// refused `peer_upgrading` while that node cannot. Nil refuses those
	// verbs as unavailable.
	Fleet builtin.Fleet

	// Runs answers a parked coding run by its turn (`answer_run`). A zero
	// value serves no such tool. Its Actor is set per call.
	Runs builtin.RunDeps

	// Pauses is the fleet's record of which seats a person paused
	// (`pause_seat`, `resume_seat`). A zero value serves neither tool. Its
	// Actor is set per call.
	Pauses builtin.SeatPauseDeps

	// Steer reaches the node running a turn, so a person can send it a note
	// (`steer_turn`). A zero value serves no such tool. Its Actor is set per
	// call.
	Steer builtin.SteerDeps

	// Answer answers a person's question from the company's knowledge
	// (`answer_knowledge`): the model, the company's budget, the corpus
	// position the answers are cached at and the cache itself — built once
	// per node, since a catalogue built per call that made its own would
	// cache nothing. It searches through the halves' knowledge and work
	// search, so it is served only where those are. Its Actor is set per
	// call.
	Answer builtin.AnswerDeps

	// Audit is where every call that is not a proven read publishes its
	// runtime audit record (see [Audit]). REQUIRED: a surface that writes
	// to the company and records nothing about who called it is the one
	// [New] refuses to build.
	Audit AuditPublisher
}

// Server is the operator surface: the dispatch, and the transports over it.
type Server struct {
	halves    func() (Halves, bool)
	org       func() *org.Organization
	authorize builtin.Authorizer
	fleet     builtin.Fleet
	runs      builtin.RunDeps
	pauses    builtin.SeatPauseDeps
	steer     builtin.SteerDeps
	answer    builtin.AnswerDeps
	audit     AuditPublisher
}

// ErrNoAudit refuses a surface that would publish no audit record of the
// calls it serves.
var ErrNoAudit = errors.New("operator: Options.Audit is required: every call " +
	"that is not a proven read publishes a runtime audit record, and a surface " +
	"that writes to the company without one is a wiring mistake")

// ErrNoAuthorize refuses a surface built with no authority decision.
var ErrNoAuthorize = errors.New("operator: Options.Authorize is required: " +
	"every tool is decided by the authority table, and a surface wired with " +
	"no decision is a wiring mistake rather than a surface that allows")

// ErrNoHalves refuses a surface with nothing to build a catalogue from.
var ErrNoHalves = errors.New("operator: Options.Halves is required: every " +
	"call reads the tracker and knowledge base this node serves as the call " +
	"finds them")

// New builds the surface, or refuses a missing required dependency by name.
//
// ALWAYS A SURFACE, never the nil an empty one used to be: which tools exist is
// a question a call asks ([Options.Halves]), and a transport over a catalogue
// that turns out empty answers in the very bytes its absence would have.
func New(opts Options) (*Server, error) {
	switch {
	case opts.Halves == nil:
		return nil, ErrNoHalves
	case opts.Authorize == nil:
		return nil, ErrNoAuthorize
	case opts.Audit == nil:
		return nil, ErrNoAudit
	}
	return &Server{
		halves: opts.Halves, org: opts.Org, authorize: opts.Authorize,
		fleet: opts.Fleet, runs: opts.Runs, pauses: opts.Pauses,
		steer: opts.Steer, answer: opts.Answer, audit: opts.Audit,
	}, nil
}

// catalogue is the tool set ONE call is served from, in the order
// [builtin.OperatorTools] returned it.
type catalogue struct {
	tools   []tools.Callable
	byName  map[string]tools.Callable
	company string
}

// lookup resolves one tool by name. A name the catalogue does not hold is
// absent, never a nearest match: a transport asked for a verb this company
// does not serve refuses it by name.
func (c catalogue) lookup(name string) (tools.Callable, bool) {
	tool, ok := c.byName[name]
	return tool, ok
}

// names is the catalogue's tool names, in catalogue order.
func (c catalogue) names() []string {
	out := make([]string, 0, len(c.tools))
	for _, tool := range c.tools {
		out = append(out, tool.Name())
	}
	return out
}

// catalogueFor is the catalogue this node serves NOW, and false where it has
// not been handed a company yet.
//
// EVERY ACTOR IS THE REQUEST'S PRINCIPAL, carrying the operation the call's
// transport named — read off the call's context, where [Server.run] puts it,
// so one catalogue serves a call whatever key it carries and no tool argument
// can set it ([withKey]).
func (s *Server) catalogueFor() (catalogue, bool) {
	halves, up := s.halves()
	if !up {
		return catalogue{}, false
	}
	work, kb := halves.Work, halves.Pages
	work.Actor, kb.Actor = workActor, pageActor
	runs, pauses, steer, answer := s.runs, s.pauses, s.steer, s.answer
	runs.Actor, pauses.Actor, steer.Actor, answer.Actor =
		workActor, workActor, workActor, workActor
	callables := builtin.OperatorTools(builtin.OperatorDeps{
		Work: work, Pages: kb, Knowledge: halves.Knowledge, Org: s.org,
		Authorize: s.authorize, Fleet: s.fleet, Runs: runs, Pauses: pauses,
		Steer: steer, Answer: answer,
	})
	c := catalogue{
		byName:  make(map[string]tools.Callable, len(callables)),
		company: halves.Company,
	}
	for _, tool := range callables {
		c.tools = append(c.tools, tool)
		c.byName[tool.Name()] = tool
	}
	return c, true
}

// Tools names what this surface serves NOW, and nil on a node that has not
// been handed a company yet.
func (s *Server) Tools() []string {
	if s == nil {
		return nil
	}
	c, up := s.catalogueFor()
	if !up {
		return nil
	}
	return c.names()
}

// Annotations is the behavioural hints this surface advertises for one tool,
// or the zero set for a name it does not serve now.
//
// It exists because the hints were invisible from outside: the surface
// published a name, a description and a schema and nothing else, so an
// operator's own assistant saw `search_work_items` and `remove_work_item` as
// identically unannotated and had nothing to ask a person on before an
// irreversible call. Exposing what is advertised is what lets that be
// asserted rather than assumed.
//
// A NAME THIS SURFACE DOES NOT SERVE ANSWERS THE ZERO SET: answering the
// engine-wide hints for any name at all reported a verb the company does not
// serve as annotated, so a check for "is this advertised with hints" passed on
// a tool that was not advertised.
func (s *Server) Annotations(name string) tools.Annotations {
	if s == nil {
		return tools.Annotations{}
	}
	c, up := s.catalogueFor()
	if !up {
		return tools.Annotations{}
	}
	if _, ok := c.lookup(name); !ok {
		return tools.Annotations{}
	}
	return builtin.AnnotationsFor(name)
}

// Call is one tool call through the dispatch, as a transport hands it over.
type Call struct {
	// Transport is which surface the call arrived on — one of the
	// transports the runtime audit names (types.TransportAct, …).
	Transport string

	// Key is the operation the call's writes derive their ids from: the
	// request's own, scoped by internal/api/opkey, or empty on a transport
	// that names none — MCP, whose tracker writes mint one per call and
	// answer it as `op_id`. It is also what the audit record names as the
	// call's request.
	Key string

	// Tool is the catalogue tool to call, by name.
	Tool string

	// Args are the tool's own arguments.
	Args map[string]any
}

// ErrNotUp is a call to a node that has not been handed a company yet, which
// every transport answers `503 no_active_revision`.
var ErrNotUp = errors.New("operator: this node has not been handed a company " +
	"yet, so it serves no tracker or knowledge base")

// ErrNotServed is a call naming a tool this company's catalogue does not
// serve. Not a refusal: the call is not one at all, so it is neither made nor
// audited.
var ErrNotServed = errors.New("operator: this company's catalogue serves no such tool")

// Dispatch is the ONE path every transport takes into a tool, and the one
// place a call is audited — so the dashboard, a person's assistant and a
// script cannot differ in what is made or recorded for them.
//
// It reads the halves, builds the catalogue the call is served from, calls the
// tool as the request's principal carrying the call's operation, and publishes
// the call's runtime audit record. [ErrNotUp] and [ErrNotServed] are answered
// before any tool runs; any other error is the tool's — the caller's context
// ended — and is a call whose write nobody can vouch for ([Interrupted]).
func (s *Server) Dispatch(ctx context.Context, c Call) (tools.Result, error) {
	cat, up := s.catalogueFor()
	if !up {
		return tools.Result{}, ErrNotUp
	}
	return s.run(ctx, cat, c)
}

// RecordWrite is a write to one person's record BY NAME, which no catalogue
// tool takes: [builtin.MarkInboxFor] and [builtin.SetPinsFor], the human write
// surface's door onto `mark_inbox` and `set_pins` for somebody other than the
// caller. The tools write only the caller's own record, deliberately — a model
// that could name whose inbox to mark could mark anybody's — so these are the
// tools' parsing and writer behind a resolution of the name, and not tools.
type RecordWrite func(ctx context.Context, deps builtin.WorkDeps, name string,
	args map[string]any) tools.Result

// DispatchRecord makes one [RecordWrite] through the dispatch: over the same
// work half the catalogue is built from, as the request's principal carrying
// the call's operation, decided by the same authority — and audited under the
// tool's own name, as the same call through the catalogue would be.
// [ErrNotUp] and [ErrNotServed] are answered as [Server.Dispatch] answers them.
func (s *Server) DispatchRecord(ctx context.Context, c Call, name string,
	write RecordWrite) (tools.Result, error) {

	halves, up := s.halves()
	if !up {
		return tools.Result{}, ErrNotUp
	}
	work := halves.Work
	if work.PersonWriter == nil {
		return tools.Result{}, fmt.Errorf("%w: %q", ErrNotServed, c.Tool)
	}
	work.Actor, work.Authorize = workActor, s.authorize
	result := write(withKey(ctx, c.Key), work, name, c.Args)
	s.auditCall(ctx, c, result, nil)
	return result, nil
}

// run makes one call on a catalogue already built for it, and audits it.
//
// THE PRINCIPAL IS READ ONCE, here, for the record — and the tool's actor reads
// the same context, so the two cannot name different people: the guard
// resolved it before the request reached any handler, and nothing in a call
// resolves it again.
func (s *Server) run(ctx context.Context, cat catalogue, c Call) (tools.Result, error) {
	tool, served := cat.lookup(c.Tool)
	if !served {
		return tools.Result{}, fmt.Errorf("%w: %q", ErrNotServed, c.Tool)
	}
	result, err := tool.Call(withKey(ctx, c.Key), c.Args)
	if !readOnly(c.Tool) {
		s.auditCall(ctx, c, result, err)
	}
	return result, err
}

// readOnly reports whether a tool is a PROVEN read — the catalogue's own hint,
// never a guess from its name. A tool whose read-only hint is unknown is
// served as the write it may be.
func readOnly(name string) bool {
	return crewletmcp.ReadOnlyProven(builtin.AnnotationsFor(name))
}

// OperationArg is the argument the operator's tools take an operation id in on
// the transport that names none — MCP, whose writes answer the `op_id` they
// were.
const OperationArg = "op_id"

// NoOperationArg reports whether args may be called under a transport-named
// operation: false where they also carry an `op_id`.
//
// THE TRANSPORT'S OPERATION IS THE REQUEST'S KEY, and every write the call
// makes derives from it — so an `op_id` beside it is a second answer to which
// operation this is, one the caller picked. A transport refuses it `400`
// naming the header ([RefuseOperationArg]), because it is the caller's to
// change: let through, the tool refused it as an argument it would not take in
// a sentence written for an assistant, which a person's surface answered as
// the domain's refusal.
func NoOperationArg(args map[string]any) bool {
	_, sent := args[OperationArg]
	return !sent
}
