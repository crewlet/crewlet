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
// (ADR-0048) can only be Never, derived from the table rather than a list.
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
package catalog

import (
	"slices"

	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/iam"
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
)

// Ases are the five, in declaration order.
var Ases = []As{AsReadTool, AsWriteTool, AsPageAction, AsNavigateOnly, AsNever}

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
	TierRead          Tier = "R0"
	TierPageLocal     Tier = "R1"
	TierReversible    Tier = "R2"
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
	// Route is the guarded route the call is sent to, "METHOD /pattern";
	// a write's is `POST /operator/act/{tool}` with Act as the tool.
	Route string
	// Act is the operator catalogue tool a write executes as.
	Act string
	// Apps are the apps a card built from this tool's answer needs, so a
	// stored card is withheld from a person who no longer holds one.
	Apps []iam.Grant
	// Fence marks an answer that carries text somebody else wrote, fenced
	// under the turn's boundary before the model reads it (ADR-0040).
	Fence bool
	// Label renders the step's sentence from the call's safe arguments.
	Label func(args map[string]any) string
	// Build turns the call's arguments into the request it sends.
	Build func(args map[string]any) (exec.Request, error)
}
