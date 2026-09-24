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
// (MCP at [MCPPath], for an assistant) adapts a request into a call through
// the catalogue's own dispatch and nothing else: it does not decide which
// tools exist, what a schema says, which hints a tool carries or how a
// refusal is worded. A
// transport that built its own catalogue would be a second call to the same
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
// # A retry is one write
//
// A person has no turn and nothing redelivers their call, but they RETRY: a
// write whose answer never arrived is sent again. A transport that knows the
// caller's own request identity puts it on the context with
// [WithRequestKey], and the actor carries it into every derived operation id
// ([builtin.Actor.RequestKey]) — so the operation ledger collapses the retry
// into the first attempt exactly as it collapses a redelivered turn. A call
// that names no request (an MCP client's) writes fresh every time, which is
// what two calls from an assistant mean.
//
// # It is ALWAYS authenticated
//
// Unlike the sandbox bridge at [mcpbridge.PathPrefix], which authenticates
// with a signed per-run token in its own path because the box inside holds no
// API credential, this surface is reached by a person's own client and is
// guarded by the ordinary operator bearer token — the same one /config and
// /secrets take. It WRITES to the company, so `allow_anonymous_read` does not
// reach it: a write is a write whatever reads are open.
package operator

import (
	"context"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
)

var log = logging.Get("api.operator")

// Options configure the surface.
type Options struct {
	// Work and Pages are the native backends. Both nil serves nothing and
	// the route is ABSENT — which is the honest shape for a company on
	// Jira and Confluence: there is nothing here it could manage.
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

	// Company names the company in the MCP server's own title, so an
	// operator with two of these connected can tell which is which.
	Company string
}

// Server is the operator surface: the catalogue, and the transports over it.
type Server struct {
	catalogue catalogue
	mcp       mcpTransport
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

// New builds the surface, or nil when there is nothing to serve.
//
// NIL RATHER THAN AN EMPTY SERVER, so no route is mounted at all: an endpoint
// that exists and lists no tools reads to an operator as broken, while one
// that is not there matches what their config says.
func New(opts Options) *Server {
	// THE PAGE TOOLS READ THE REQUEST KEY THROUGH THEIR OWN SEAM, and it is
	// this package's to wire because this package is the one that puts it
	// on the context: a key whose writer and reader were wired in two
	// places is one somebody wires on only one side. The work tools carry
	// it on the actor instead — see [WorkActor].
	opts.Pages.RequestKey = requestKeyFrom
	callables := builtin.OperatorTools(builtin.OperatorDeps{
		Work: opts.Work, Pages: opts.Pages, Knowledge: opts.Knowledge,
		Org: opts.Org, Leads: opts.Leads, LeadsProject: opts.LeadsProject,
	})
	if len(callables) == 0 {
		return nil
	}
	s := &Server{catalogue: newCatalogue(callables)}
	s.mcp = newMCPTransport(s.catalogue, opts.Company)
	return s
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

// call runs one catalogue tool by name — the one path every transport takes
// into a tool, so a transport cannot reach one the catalogue does not hold.
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
