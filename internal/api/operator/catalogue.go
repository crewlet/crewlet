// Package operator serves the company's own tracker and knowledge base to the
// people who run it: ONE catalogue of tools, built once, and every transport
// that reaches it drawing from that one value.
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
// # One catalogue, every transport
//
// The catalogue (catalogue.go) is the tool set, resolved against this
// company's backends by [builtin.OperatorTools] ONCE, in [New]. A transport
// — MCP at [MCPPath] for an assistant, and the act transport at [ActPattern]
// for a person at the dashboard (act.go) — adapts a request into a call
// through the catalogue's own dispatch and nothing else: it does not decide
// which tools exist, what a schema says, which hints a tool carries or how a
// refusal is worded. A transport that built its own catalogue would be a second call to the same
// constructor with a second chance to pass it different deps — and the
// difference would surface as a verb that works from one surface and is
// missing from the other, which nobody tests for because each surface looks
// complete from inside.
//
// # It is the SAME TOOLS a seat holds, with a different writer
//
// Not a parallel implementation. Every tool here is the one from
// [internal/agent/builtin], constructed with [builtin.WorkDeps.Actor] set —
// so a schema, a default, a trimmed field and the wording of a refusal are
// each written once. Two copies of "file an item" drift on exactly the parts
// nobody looks at, and only one of the two is ever tested.
//
// What differs is WHO the write is attributed to. A seat's writes carry its
// handle and its turn; these carry the operator's token label and
// [tracker.AuthorOperator], so a person and the credential they used are two
// separate facts on the record and an audit can tell an operator's edit from
// an agent's. See [WorkActor] in actor.go.
//
// # A retry is the same operations
//
// A person has no turn and nothing redelivers their call, but they RETRY: a
// write whose answer never arrived is sent again. A transport that knows the
// caller's own request identity puts it on the context with
// [WithRequestKey], and the actor carries it into every id the write derives
// ([builtin.Actor.RequestKey], [builtin.PageDeps.RequestKey]) — each record's
// operation, a created item's, view's or page's own id, a comment's — so the
// retry is the first attempt's operations rather than new ones, exactly as a
// redelivered turn's are. A call that names no request (an MCP client's)
// writes fresh every time, which is what two calls from an assistant mean.
//
// # It is ALWAYS authenticated
//
// Unlike the sandbox bridge at [mcpbridge.PathPrefix], which authenticates
// with a signed per-run token in its own path because the box inside holds no
// API credential, this surface is reached by a person's own client and is
// guarded by the ordinary operator bearer token — the same one /config and
// /secrets take. It WRITES to the company, so `allow_anonymous_read` does not
// reach it: a write is a write whatever reads are open.
//
// # The act transport admits a PERSON, and nobody else (ADR-0024)
//
// The two transports differ in exactly one rule. MCP admits any token, bound
// or not, because an assistant connected with a CI token is a credential
// acting as itself. The act transport — the dashboard's buttons — admits only
// a token `contact.crewlet_operator_id` binds to a human seat, and refuses
// every other caller `unbound`: a disabled guard's anonymous caller, and a
// credential nobody bound. A button is pressed by somebody, and the only
// somebody a browser session can honestly claim to be is the person the token
// names; a write attributed to "the dashboard" would be the one actor an audit
// cannot ask why. The attribution itself does not change — the author is still
// the token, the kind still `operator`, and the person rides beside it as the
// actor's seat — so an audit reads a dashboard write exactly as it reads the
// same person's assistant. See [ActPattern].
//
// # Every call that may write is audited, on both transports
//
// The history of a work item or a page records what CHANGED; it has nothing
// to say about a call that was refused, one whose answer never came back, or
// a verb that is not a tracker or wiki write. So every call that is not a
// proven read publishes one `operator_acted` event (audit.go) — the
// credential, the person it is bound to, the transport, the tool, the request
// id and what became of it, never the arguments — and the event store writes
// it on this node. It is done in the ONE dispatch both transports call, so
// what is recorded about a person cannot depend on whether they pressed a
// button or asked their assistant. [New] refuses a surface with tools and no
// [Options.Audit].
package operator

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
)

var log = logging.Get("api.operator")

// Options configure the surface.
type Options struct {
	// Work and Pages are the native backends. Both nil serves none of
	// their tools — the honest shape for a company on Jira and Confluence.
	// A surface left with no tool at all (no Runs or Pauses either) is ABSENT, with
	// nothing on it to manage.
	Work  builtin.WorkDeps
	Pages builtin.PageDeps

	// Knowledge is the company's ranked search, or nil.
	Knowledge builtin.KnowledgeSearcher

	// Org is the company chart, resolved per call because a config apply
	// replaces it. An operator has no turn to carry one.
	//
	// TWO THINGS NEED IT, which is why it is wired unconditionally.
	// `search_knowledge` is scoped against it and is registered only when
	// both it and a knowledge backend are present — that is the gate
	// [builtin.OperatorTools] makes. WHO THE CALLER IS needs it too, on
	// every call: the same chart says which seat a token is bound to, and
	// a surface that wired this only where the company had a wiki wrote a
	// founder's own inbox marks under their credential's name. See
	// [WorkActor] and [builtin.Parties].
	Org func() *org.Organization

	// Leads answers whether one handle leads another — the one authority
	// over a person's record that reaches across people. Nil degrades to
	// "your own only" rather than to a hole.
	Leads builtin.Leads

	// LeadsProject answers whether a handle leads the unit that owns a
	// project — the authority over that project's SETTINGS, which is a
	// different question from the line above: one is about a person, the
	// other about a container. Nil REFUSES every policy edit naming the
	// project, which is the safe direction.
	LeadsProject builtin.LeadsProject

	// Fleet reads what each node's build can carry out, off the presence
	// heartbeat, so a verb carried out by the node holding a seat is
	// refused `peer_upgrading` while that node cannot. Nil refuses those
	// verbs as unavailable.
	Fleet builtin.Fleet

	// Runs is the parked coding runs a person may answer by turn
	// (`answer_run`), and who is answering. A zero value serves no such tool.
	Runs builtin.RunDeps

	// Pauses is the fleet's record of which seats a person paused
	// (`pause_seat`, `resume_seat`), and who is pausing. A zero value serves
	// neither tool.
	Pauses builtin.SeatPauseDeps

	// Steer reaches the node running a turn, so a person can send it a note
	// (`steer_turn`), and says who is sending it. A zero value serves no
	// such tool.
	Steer builtin.SteerDeps

	// Answer answers a person's question from the company's knowledge
	// (`answer_knowledge`): the model, the company's budget, the corpus
	// position the answers are cached at, and who is asking. It searches
	// through Knowledge and Work.Search, so it is served only with
	// Knowledge and Org. A zero value serves no such tool.
	Answer builtin.AnswerDeps

	// Company names the company in the MCP server's own title, so an
	// operator with two of these connected can tell which is which.
	Company string

	// Audit is where every call that is not a proven read publishes its
	// runtime audit record (see [Audit]). REQUIRED whenever the catalogue
	// serves anything: a surface that writes to the company and records
	// nothing about who called it is the one [New] refuses to build.
	Audit AuditPublisher
}

// Server is the operator surface: the catalogue, and the transports over it.
type Server struct {
	catalogue catalogue
	mcp       mcpTransport

	// chart is [Options.Org], kept for the decisions a transport makes
	// about the caller rather than the tool: whether the token is bound to
	// a person, which is the whole of the act transport's admission rule,
	// and which person an audit record names.
	chart func() *org.Organization

	// audit is [Options.Audit].
	audit AuditPublisher
}

// catalogue is the tool set every transport serves, in the order
// [builtin.OperatorTools] returned it.
//
// A VALUE BUILT ONCE rather than a constructor each transport calls, which is
// the whole point of the type: see the package doc.
type catalogue struct {
	tools  []tools.Callable
	byName map[string]tools.Callable
}

func newCatalogue(callables []tools.Callable) catalogue {
	c := catalogue{byName: make(map[string]tools.Callable, len(callables))}
	for _, tool := range callables {
		c.tools = append(c.tools, tool)
		c.byName[tool.Name()] = tool
	}
	return c
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

// ErrNoAudit refuses a surface that would serve tools and publish no audit
// record of them.
var ErrNoAudit = errors.New("operator: Options.Audit is required: every call " +
	"that is not a proven read publishes a runtime audit record, and a surface " +
	"that writes to the company without one is a wiring mistake")

// New builds the surface, or nil when there is nothing to serve.
//
// NIL RATHER THAN AN EMPTY SERVER, so no route is mounted at all: an endpoint
// that exists and lists no tools reads to an operator as broken, while one
// that is not there matches what their config says. A surface with tools to
// serve and no [Options.Audit] is refused with [ErrNoAudit].
func New(opts Options) (*Server, error) {
	// THE PAGE TOOLS READ THE REQUEST KEY THROUGH THEIR OWN SEAM, and it is
	// this package's to wire because this package is the one that puts it
	// on the context: a key whose writer and reader were wired in two
	// places is one somebody wires on only one side. The work tools carry
	// it on the actor instead — see [WorkActor].
	opts.Pages.RequestKey = requestKeyFrom
	callables := builtin.OperatorTools(builtin.OperatorDeps{
		Work: opts.Work, Pages: opts.Pages, Knowledge: opts.Knowledge,
		Org: opts.Org, Leads: opts.Leads, LeadsProject: opts.LeadsProject,
		Fleet: opts.Fleet, Runs: opts.Runs, Pauses: opts.Pauses, Steer: opts.Steer,
		Answer: opts.Answer,
	})
	if len(callables) == 0 {
		return nil, nil
	}
	if opts.Audit == nil {
		return nil, ErrNoAudit
	}
	s := &Server{catalogue: newCatalogue(callables), chart: opts.Org, audit: opts.Audit}
	s.mcp = newMCPTransport(s, opts.Company)
	return s, nil
}

// Tools names what this surface serves, for the operator log line.
func (s *Server) Tools() []string {
	if s == nil {
		return nil
	}
	return s.catalogue.names()
}

// Annotations is the behavioural hints this surface advertises for one tool,
// or the zero set for a name it does not serve.
//
// It exists because the hints were invisible from outside: the surface
// published a name, a description and a schema and nothing else, so an
// operator's own assistant saw `search_work_items` and `remove_work_item` as
// identically unannotated and had nothing to ask a person on before an
// irreversible call. Exposing what is advertised is what lets that be
// asserted rather than assumed.
//
// A NAME THIS SURFACE DOES NOT SERVE ANSWERS THE ZERO SET, which is what this
// comment always promised and what the code did not do: it returned the
// engine-wide hints for any name at all, so asking about a verb the company
// does not serve reported it as annotated — a check for "is this advertised
// with hints" passed on a tool that was not advertised.
func (s *Server) Annotations(name string) tools.Annotations {
	if s == nil {
		return tools.Annotations{}
	}
	if _, ok := s.catalogue.lookup(name); !ok {
		return tools.Annotations{}
	}
	return builtin.AnnotationsFor(name)
}

// call runs one catalogue tool by name, so a transport cannot reach one the
// catalogue does not hold. Transports go through [Server.dispatch], which
// audits the call; this is the tool half of it.
// served is false for a name the catalogue does not hold, which the transport
// refuses in its own vocabulary.
func (c catalogue) call(ctx context.Context, name string,
	args map[string]any) (result tools.Result, served bool, err error) {

	tool, ok := c.lookup(name)
	if !ok {
		return tools.Result{}, false, nil
	}
	result, err = tool.Call(ctx, args)
	return result, true, err
}
