// Package catalog is the Dashboard Assistant's tool catalogue: the curated
// table of what the assistant may reach, and the descriptor each tool the
// model is offered is built from.
//
// # One exposure decision per surface, held complete by walks
//
// Every registry question, every operator catalogue tool and every guarded
// route has exactly one [Exposure] row saying what the assistant does with it:
// offer it as a read, offer it as a write that becomes a proposal, offer it as
// a page action, link to it only, or never, with the reason a reviewer reads.
// The walks that hold the table live beside the engine metadata they read (the
// registry, the operator catalogue, the API's route table, the authority
// table), so a new question, tool or route is a red build that says "classify
// this for the assistant" rather than a surface the assistant silently gains
// or silently lacks. A verb the authority table keeps from the assistant
// (ADR-0051) can only be Never, derived from the table rather than a list.
//
// # Only what the person can take now is offered
//
// A tool the person could not take is not offered at all, so the model cannot
// be talked into trying it: reads by their question's posture against the
// person's grants, writes by the viewer's `acts`. Offering is a filter and
// never authority; every call is still decided by the route it sends, as the
// person (internal/assistant/exec).
//
// # No tool names who acts
//
// No schema here has an argument naming the company, a principal, an actor,
// a login, an operation id or an operator: identity is the forwarded grant and
// nothing else. An argument naming a person or a seat names the SUBJECT of a
// read the authority table then decides, never "act as".
//
// # One place shapes what the model reads
//
// [Shape] is the whole path from a read's answer to the two things made of it:
// every string through redact.Secrets, the card by the tool's declared spec
// (internal/assistant/cards), and a summary of whole rows within
// cards.ModelBytes with the fence included, fenced under the turn's boundary
// with the tool's Source and Trust where the answer carries text somebody
// wrote (ADR-0040) and with every marker defused where it does not. A second
// copy of any step elsewhere is a step that can be skipped.
//
// # A write is previewed from the object as it is now
//
// Every write and page action carries its Preview (internal/assistant/proposal):
// the object read through the turn's grant, the change rows, the revision it
// binds and the exact request the approval sends, which is the one the tool
// builds. [Revision] re-reads a recorded revision, so an approval refuses a
// change prepared against a version the object has left.
//
// # The registry is gated by the same table
//
// [ReadsQuestion] and [ReadsRoute] are what an assisted principal may ask the
// question registry and the overview routes (ADR-0051): an Everybody or a
// Requires question never reaches the authority table, so its assistant
// column cannot keep the assistant from one, and internal/api wires these as
// the gate instead.
package catalog

import (
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
)

// As is what the assistant does with one surface.
type As string

const (
	// AsReadTool offers the surface as a read tool (risk tier R0).
	AsReadTool As = "read_tool"
	// AsWriteTool offers it as a write tool whose call becomes a proposal
	// the person approves (R2 or R3).
	AsWriteTool As = "write_tool"
	// AsPageAction offers it as a change to the open screen, applied by
	// the page (R1).
	AsPageAction As = "page_action"
	// AsNavigateOnly offers no tool, only an open in app link to it.
	AsNavigateOnly As = "navigate_only"
	// AsNever keeps it from the assistant, for the reason the row states.
	AsNever As = "never"
	// AsPreviewRead offers no tool: the question is read only by the named
	// write tool's preview, or an approval's re-read of the revision that
	// preview recorded (a person's tracker record, which a priority change
	// is bound to).
	AsPreviewRead As = "preview_read"
)

// Ases are the six, in declaration order.
var Ases = []As{AsReadTool, AsWriteTool, AsPageAction, AsNavigateOnly, AsNever, AsPreviewRead}

// Valid reports whether a is a decision this build knows.
func (a As) Valid() bool { return slices.Contains(Ases, a) }

// Exposure is one decision about one surface the assistant could reach. It
// names the surface by the identifier the engine already uses for it, exactly
// one of Question, Tool and Route.
type Exposure struct {
	// Question is a registry question's name.
	Question string
	// Tool is an operator catalogue tool's name.
	Tool string
	// Route is a guarded route's pattern, "METHOD /path".
	Route string
	As    As
	// Name is the model-facing tool the surface is offered as, for a read
	// or a write tool.
	Name string
	// Why is required for Never and NavigateOnly: the reason a reviewer
	// reads.
	Why string
}

// Tier is a tool's risk, DERIVED rather than chosen: R3 when its verb row is
// human-only, asks a step-up, or its tool is destructive beyond the caller's
// own record; R2 for any other write; R1 for a page action; R0 for a read. A
// table row may not state a tier lower than the derivation.
type Tier string

const (
	// TierRead is a read (R0): sent at once, through the turn's read grant.
	TierRead Tier = "R0"
	// TierPageLocal is a page action (R1): a change to the screen the
	// person has open, applied by the page itself.
	TierPageLocal Tier = "R1"
	// TierReversible is any write that is not consequential (R2).
	TierReversible Tier = "R2"
	// TierConsequential is a write whose verb row is human-only or asks a
	// step-up, or whose tool is destructive beyond the caller's own record
	// (R3).
	TierConsequential Tier = "R3"
)

// Tiers are the four, lowest first.
var Tiers = []Tier{TierRead, TierPageLocal, TierReversible, TierConsequential}

// Valid reports whether t is a tier this build knows.
func (t Tier) Valid() bool { return slices.Contains(Tiers, t) }

// Kind is how a tool's call is carried out.
type Kind string

const (
	// KindRead is sent at once, through the grant, and its answer fenced
	// and carded.
	KindRead Kind = "read"
	// KindWrite is never sent by the turn: it becomes a proposal, and the
	// approval sends it under an exec grant.
	KindWrite Kind = "write"
	// KindPageAction becomes a proposal the page applies.
	KindPageAction Kind = "page_action"
	// KindNavigate builds an open in app card and sends nothing.
	KindNavigate Kind = "navigate"
)

// Kinds are the four, in declaration order.
var Kinds = []Kind{KindRead, KindWrite, KindPageAction, KindNavigate}

// Valid reports whether k is a kind this build knows.
func (k Kind) Valid() bool { return slices.Contains(Kinds, k) }

// Tool is one tool the model may be offered.
type Tool struct {
	// Name is the model-facing name, snake_case.
	Name string
	// Description is what the model reads about it.
	Description string
	// Phrase completes "You do not have permission to ..." in a refusal
	// card ("file tasks"), so the sentence is never model text.
	Phrase string
	// Parameters is the call's JSON Schema; an argument it does not declare
	// is refused before anything is sent.
	Parameters map[string]any
	Kind       Kind
	Tier       Tier
	// Routes are every guarded route the call may be sent to, each
	// "METHOD /pattern" as the API mounts it; Build picks one by the
	// call's arguments and sends nothing else. A write's is only
	// `POST /operator/act/{tool}`, with Act as the tool.
	Routes []string
	// Act is the operator catalogue tool a write executes as.
	Act string
	// Apps are the apps a card built from this tool's answer needs, so a
	// stored card is withheld from a person who no longer holds one.
	Apps []iam.Grant
	// Fence marks an answer that carries text somebody else wrote, fenced
	// under the turn's boundary before the model reads it (ADR-0040), with
	// Source and Trust as its label.
	Fence bool
	// Source is the surface a read's answer is labelled as in its fence:
	// provenance.SourceAPIAnswer for every route's answer.
	Source provenance.Source
	// Trust is how far the model may rely on a read's answer: the least
	// its writers earn (a route's answer names no writer per field, so
	// colleague for the company's own records).
	Trust provenance.Trust
	// Label renders the step's sentence from the call's safe arguments.
	Label func(args map[string]any) string
	// Build turns the call's arguments into the one request it sends, on
	// one of Routes. Call it through [Tool.Request], which checks the
	// arguments against the schema first and the request against Routes
	// after.
	Build func(args map[string]any) (exec.Request, error)

	// Cards are how a read's answer becomes a card, by variant: the one
	// spec under "" for a tool with one shape of answer, else one per value
	// [Tool.Variant] answers (a section, a grouping).
	Cards map[string]cards.Spec
	// Variant names which of Cards a call's answer is carded by; nil is
	// always "".
	Variant func(args Args) string
	// Destructive is the act tool's own MCP hint (internal/agent/builtin's
	// annotations), stated here because this package may not import the
	// builtins; the catalogue walk holds it to the hint, and it is one of
	// the three inputs a write's tier is derived from.
	Destructive bool
	// ChangesVisibility marks a write that changes who can see something (a
	// share, an unshare): its preview must name the audience (D09), which
	// proposal.Preview.Check holds when told so.
	ChangesVisibility bool

	// Preview is a write's or a page action's preview: what the call would
	// change, read from the object as it is now, and the exact request
	// the approval sends (internal/assistant/proposal).
	Preview proposal.Previewer

	// StepUp reports a call whose effect asks the approver to confirm who
	// they are on top of whatever the verb's own row asks: a share to the
	// whole company (D09). Nil asks nothing more. The approval asks the
	// verb's row (authz.RecencyOf) as well, always.
	StepUp func(args Args) bool

	// schema is Parameters as this package checks it.
	schema *schema
}

// StepLabel is the sentence the drawer shows for one call of the tool: its
// own [Tool.Label] where it has one, and otherwise a sentence from what the
// catalogue already says of it, never its model-facing name. A write is a
// proposal being prepared, named by the task it touches where the call names
// one; a page action prepares a change to the screen; a link is looked up.
func (t Tool) StepLabel(args map[string]any) string {
	if t.Label != nil {
		if l := strings.TrimSpace(t.Label(args)); l != "" {
			return l
		}
	}
	switch t.Kind {
	case KindWrite:
		if item := Args(args).String("item"); item != "" && len(item) <= nameBytes {
			return "Preparing a change to " + item
		}
		return "Preparing a change for your approval"
	case KindPageAction:
		return "Preparing a change to this screen"
	case KindNavigate:
		return "Finding where to open it"
	}
	if t.Phrase == "" {
		return t.Name
	}
	return strings.ToUpper(t.Phrase[:1]) + t.Phrase[1:]
}

// Check reports what is wrong with a call's arguments, wrapping
// [ErrInvalidArguments]: an argument the schema does not declare, an
// identity argument ([IdentityArguments]), a missing required one, or a value
// of the wrong kind or out of its range.
func (t Tool) Check(args map[string]any) error {
	if t.schema == nil {
		return fmt.Errorf("%w: %s declares no schema", ErrInvalidArguments, t.Name)
	}
	if args == nil {
		args = map[string]any{}
	}
	if err := t.schema.check(t.Name, args); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidArguments, err)
	}
	return nil
}

// Request is the one request a call sends: its arguments checked, the request
// built, and the request held to one of the tool's Routes, so a Build that
// strayed is refused here rather than sent.
func (t Tool) Request(args map[string]any) (exec.Request, error) {
	if err := t.Check(args); err != nil {
		return exec.Request{}, err
	}
	if t.Build == nil {
		return exec.Request{}, fmt.Errorf("catalog: %s sends no request", t.Name)
	}
	req, err := t.Build(args)
	if err != nil {
		return exec.Request{}, fmt.Errorf("%w: %w", ErrInvalidArguments, err)
	}
	if t.Kind == KindPageAction {
		if req.Method != PageMethod || !strings.HasPrefix(req.Path, PageActionPrefix) {
			return exec.Request{}, fmt.Errorf("catalog: %s built %s %s, which is not a page "+
				"action", t.Name, req.Method, req.Path)
		}
		return req, nil
	}
	if _, ok := t.RouteOf(req); !ok {
		return exec.Request{}, fmt.Errorf("catalog: %s built %s %s, which is none of its "+
			"routes %v", t.Name, req.Method, req.Path, t.Routes)
	}
	return req, nil
}

// RouteOf is the Routes entry req was built on.
func (t Tool) RouteOf(req exec.Request) (string, bool) {
	for _, route := range t.Routes {
		if matches(route, req.Method, req.Path) {
			return route, true
		}
	}
	return "", false
}

// StepRequest is how a step shows the request req makes: its method, and the
// path pattern of the tool's route it matches, without the method the route
// pattern starts with, since the step carries the method apart. THE ONE RULE
// for a step's request, so a step never reads "GET GET /work/{id}". False
// where req matches none of the tool's routes.
func (t Tool) StepRequest(req exec.Request) (*wire.StepRequest, bool) {
	route, ok := t.RouteOf(req)
	if !ok {
		return nil, false
	}
	_, path, _ := strings.Cut(route, " ")
	return &wire.StepRequest{Method: req.Method, Route: path}, true
}

// CardFor is the spec a call's answer is carded by.
func (t Tool) CardFor(args Args) (cards.Spec, bool) {
	variant := ""
	if t.Variant != nil {
		variant = t.Variant(args)
	}
	spec, ok := t.Cards[variant]
	return spec, ok
}

// matches reports whether method and path are a request route, "METHOD
// /pattern", serves: each `{name}` a single non-empty segment, a trailing
// `{name...}` the rest.
func matches(route, method, path string) bool {
	m, pattern, ok := strings.Cut(route, " ")
	if !ok || m != method {
		return false
	}
	want, got := strings.Split(pattern, "/"), strings.Split(path, "/")
	for i, segment := range want {
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "...}") {
			return i < len(got) && got[i] != ""
		}
		if i >= len(got) {
			return false
		}
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			if got[i] == "" {
				return false
			}
			continue
		}
		if segment != got[i] {
			return false
		}
	}
	return len(got) == len(want)
}
