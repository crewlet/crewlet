package builtin

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// WHO MAY CALL A TOOL, AND WHY THE DECISION IS TAKEN HERE.
//
// # One decision, three surfaces
//
// The same tool implementations serve three callers: a seat's own registry
// inside a turn, the operator's assistant over MCP, and the HTTP surface a
// person writes through. If each decided for itself, the three would drift —
// and they would drift silently, because the only way to notice is to ask the
// same question three ways and compare the wording.
//
// So the decision is a SEAM on the deps every one of them constructs, and
// every one populates it from [authz.Decide]. One table, one function, three
// callers.
//
// # And why it is not tools.Surface's Guard
//
// That seam is `Check(tool, server) string`: it carries no principal, no
// object and no arguments, it is per phase SESSION rather than per call, and
// it is already occupied by the skill gate. The operator's assistant never
// constructs a Surface at all, so a decision living there would cover one
// caller of three. internal/tools' own doc states that; this is the other end
// of it.
//
// # A nil Authorizer REFUSES
//
// The opposite of [tools.Surface]'s Guard, whose nil is documented to allow
// everything — and the difference is what each one is FOR. A missing skill
// gate means no skills are locked, which is a company that declared none. A
// missing authority decision means nobody decided, and a surface that shipped
// without wiring one would serve every verb to every caller while looking
// exactly like a surface that had.

// Authorizer decides whether the party behind a call may make it.
//
// THE PRINCIPAL COMES FROM THE CONTEXT, on every call, and is never captured:
// a seat's tools are cloned into its lease and an MCP session outlives any one
// request, so a principal bound at construction would answer for whoever
// happened to be there when the surface was built.
//
// AN ERROR IS THE REFUSAL, and it carries which kind: [ErrRefused] is an
// authority answer a retry will never change, and anything else is a node that
// could not decide — which a surface answers 403 and 503 to. A nil error is
// the allow.
type Authorizer func(ctx context.Context, action authz.Action,
	object authz.Object) error

// ErrRefused reports a call the acting party may not make.
//
// DISTINCT FROM AN UNDECIDABLE ONE for internal/coord's reason, applied to
// authority: "you may not", and "this node could not tell" are two answers,
// and collapsing the second into the first tells somebody to go and ask for a
// capability they already hold.
var ErrRefused = errors.New("builtin: this party may not do that")

// ErrNoAuthorizer reports deps nobody wired a decision into.
//
// ITS OWN SENTINEL so the refusal a caller sees names the WIRING rather than
// the caller: "you may not create work items" sends somebody to ask for a
// grant, and this is a build that never asked anybody anything.
var ErrNoAuthorizer = errors.New("builtin: no authority decision is wired, " +
	"so nothing here may be called")

// Decide is the Authorizer every surface builds, over one chart.
//
// ONE CONSTRUCTOR rather than three call sites composing [authz.Decide]
// themselves, because the composition has two parts a caller can get wrong
// independently: reading the principal three-valued, and turning an UNKNOWN
// decision into an error rather than a refusal.
func Decide(chart authz.Chart) Authorizer {
	return func(ctx context.Context, action authz.Action,
		object authz.Object) error {

		principal, how := iam.From(ctx)
		switch how {
		case iam.Unknown:
			return fmt.Errorf("this node could not establish who is calling "+
				"%s: %w", action, iam.Reason(ctx))
		case iam.Anonymous:
			return fmt.Errorf("%w: %s needs a credential and none was "+
				"presented", ErrRefused, action)
		}
		d := authz.Decide(ctx, principal, action, object, chart)
		switch {
		case d.Unknown():
			return fmt.Errorf("this node could not decide %s: %w", action, d.Err)
		case !d.Allowed:
			return fmt.Errorf("%w: %s refused (%s)", ErrRefused, action, d.Reason)
		}
		return nil
	}
}

// subjectOf is what each tool is ABOUT, per tool, from its own arguments.
//
// A TABLE RATHER THAN A CHECK INSIDE EACH TOOL, for the reason internal/authz
// keeps one: there are thirty verbs and half a dozen shapes of object, so a
// decision written at each call site is the same sentence thirty times and
// drifts on whichever copy somebody edited. What is here is only the
// EXTRACTION — which argument names the project, the container, the person —
// and never the rule, which is internal/authz's.
//
// FIELD NAMES AS DATA AND NOT CLOSURES, which is what the first shape got
// wrong: `ownerFrom("handle")` on a tool that declares no `handle` argument
// reads empty on every call, and every personal class refuses an empty
// owner — so `my_work`, `mark_inbox`, `set_pins` and `save_work_view` were
// gated shut for every caller but an admin, with the only symptom a refusal
// naming a grant the caller already held. A closure can only be checked by
// calling it with arguments somebody thought of; a field name is compared
// against the tool's own JSON Schema, which is what
// [TestEveryObjectFieldIsOneItsToolDeclares] does.
//
// A TOOL WITH NO ROW IS ABOUT NOTHING, which is correct rather than a gap:
// every class that reads a field refuses an empty one, so a verb whose object
// somebody forgot is refused rather than admitted. The classes that read no
// field at all — the ordinary reads, the colleague writes decided by
// capability — are the rows that are deliberately absent.
var subjectOf = map[string]subject{
	// THE COLLEAGUE WRITES name their KIND and nothing else: the rule
	// picks work:write or knowledge:write from it, and asks the chart
	// about no relation at all.
	"create_work_item":     {kind: authz.KindTask},
	"update_work_item":     {kind: authz.KindTask},
	"comment_on_work_item": {kind: authz.KindTask},
	"merge_work_item":      {kind: authz.KindTask},
	"a2a_ask":              {kind: authz.KindTask},
	"write_page":           {kind: authz.KindPage},
	"save_page":            {kind: authz.KindPage},
	"comment_on_page":      {kind: authz.KindPage},

	// THE CONTAINER CLASS asks who leads the project or the container.
	// `write_work_catalogue` is NOT here: the catalogue is the whole
	// workspace's and the tool names no container, so its rule is the
	// company grant rather than a relation.
	"write_project": {kind: authz.KindProject, container: "project"},

	// THE PERSONAL ONES name whose record it is, and three of them name
	// nobody at all: `my_work`, `mark_inbox` and `set_pins` take no
	// handle, because a model that could name whose day to read could
	// read anybody's. An unnamed owner is the CALLER — see
	// [subject.objectFor].
	"get_person":     {kind: authz.KindPerson, owner: "handle"},
	"work_inbox":     {kind: authz.KindPerson, owner: "handle"},
	"set_priorities": {kind: authz.KindPerson, owner: "handle"},
	"my_work":        {kind: authz.KindPerson},
	"mark_inbox":     {kind: authz.KindPerson},
	"set_pins":       {kind: authz.KindPerson},

	// A SAVED VIEW IS EITHER, and its own class reads whichever it
	// names. Its container is a kind plus a key in ONE argument
	// (`project:ENG`, `unit:eng`, `person:ana`, `workspace`), which is
	// why this row states a parser rather than a kind.
	"save_work_view": {kind: authz.KindView, owner: "owner", container: "container"},
}

// subject is what one tool is about, as the names of its own arguments.
type subject struct {
	// kind is what sort of thing the call acts on.
	kind authz.ObjectKind

	// owner is the argument naming WHOSE record it is, empty on a verb
	// that takes no handle.
	owner string

	// container is the argument naming the project, unit or container
	// the call is inside.
	container string
}

// objectFor is one call's object, from the caller and the arguments.
//
// AN ABSENT OWNER IS THE CALLER, on the PERSONAL kind and on no other. Three
// of those verbs take no handle at all and the rest default to yours, so an
// empty one is the shape of the verb rather than a caller who forgot — and
// reading it as [authz.ReasonUnnamed] refuses every one of them. A view is
// deliberately not that kind for exactly this reason: a view naming no owner
// is SHARED, not mine.
func (s subject) objectFor(p iam.Principal, args map[string]any) authz.Object {
	o := authz.Object{Kind: s.kind}
	if s.owner != "" {
		o.Owner = strings.TrimSpace(stringArg(args, s.owner))
	}
	if s.kind == authz.KindPerson && o.Owner == "" {
		o.Owner = selfOf(p)
	}
	if s.container != "" {
		o.ContainerKind, o.Container = containerArg(args, s.container)
		if s.kind != authz.KindView {
			o.Kind = cmp.Or(o.ContainerKind, s.kind)
		}
	}
	return o
}

// selfOf is the handle a personal class compares the caller against.
//
// THE SEAT, FALLING BACK TO THE LOGIN, which is [authz]'s own actorOf and for
// the same reason: the classes compare both fields, and a seat writing its own
// inbox is addressed by its handle.
func selfOf(p iam.Principal) string {
	if p.Seat != "" {
		return p.Seat
	}
	return p.Login
}

// containerArg reads a container argument as its kind and its key.
//
// TWO SPELLINGS IN ONE FUNCTION, because the tools have two: `write_project`
// takes a bare project key, and `save_work_view` takes the tracker's own
// `kind:id` container grammar. A bare value is a PROJECT, which is what every
// caller of the first spelling means.
func containerArg(args map[string]any, field string) (authz.ObjectKind, string) {
	raw := strings.TrimSpace(stringArg(args, field))
	if raw == "" {
		return "", ""
	}
	kind, id, found := strings.Cut(raw, ":")
	if !found {
		if strings.EqualFold(raw, tracker.ContainerWorkspace) {
			return authz.KindCompany, tracker.ContainerWorkspace
		}
		return authz.KindProject, tracker.ProjectKey(raw)
	}
	switch kind {
	case tracker.ContainerProject:
		return authz.KindProject, tracker.ProjectKey(id)
	case tracker.ContainerUnit:
		return authz.KindUnit, id
	case tracker.ContainerPerson:
		return authz.KindPerson, id
	}
	// A KIND THIS BUILD DOES NOT KNOW NAMES NOTHING, which every class
	// refuses. The tool parses the same string and refuses it too, with a
	// message that says what the grammar is — and the refusal a caller
	// reads should be that one, not an authority answer.
	return "", ""
}

// stringArg is one argument as a string, or empty.
//
// EMPTY RATHER THAN AN ERROR, because an absent argument is the tool's own
// business: every class that reads a field refuses an empty one, and a
// personal class reads an empty owner as the caller themselves. Deciding here
// what an absent project means would be this table holding an opinion the
// rules already hold.
func stringArg(args map[string]any, field string) string {
	held, _ := args[field].(string)
	return held
}

// gated wraps one tool with the authority decision.
//
// IT WRAPS RATHER THAN BEING CALLED FROM EACH TOOL, because the action IS the
// tool's name — internal/authz's table is keyed on it, and the walk in this
// package's tests holds the two against each other in both directions. So
// there is exactly one place the check can be forgotten, and it is a place
// nothing else goes through.
//
// A POINTER, AND THAT IS LOAD-BEARING: a registered tool is compared with `==`
// — internal/engine asserts a seat's builtins are the applied epoch's OBJECTS
// and not merely its names — and a struct holding a func field is
// UNCOMPARABLE, so the first shape turned that assertion into a runtime panic
// at the comparison. A pointer is comparable whatever it points at, and
// pointer identity is exactly what "the same object" means there.
type gated struct {
	tools.Callable
	authorize Authorizer
}

// gatedSeat and gatedDetached are [gated] for the two OPTIONAL interfaces a
// tool may also satisfy.
//
// THREE TYPES rather than one with nil checks, because the surface dispatches
// on which interface a tool satisfies — and a single wrapper implementing all
// of them would make every read-only builtin look seat-scoped and
// suspendable. The phase surface would then refuse each one outside a turn,
// and the tool loop would wait for a suspension nothing was going to send.
//
// Measured by deleting the detached arm: `run_sandbox` stopped suspending the
// loop, so a turn that launched a coding run ended believing the work was
// done.
type gatedSeat struct {
	gated
	seat tools.SeatCallable
}

type gatedDetached struct {
	gated
	detached tools.Detached
}

func gate(tool tools.Callable, authorize Authorizer) tools.Callable {
	wrapped := gated{Callable: tool, authorize: authorize}
	switch held := tool.(type) {
	case tools.Detached:
		return &gatedDetached{gated: wrapped, detached: held}
	case tools.SeatCallable:
		return &gatedSeat{gated: wrapped, seat: held}
	}
	return &wrapped
}

func (g *gatedDetached) CallDetached(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.DetachedResult, error) {

	ctx = turnctx.WithPrincipal(ctx, turn)
	if refusal := g.check(ctx, args); refusal != nil {
		// NO SUSPENSION ON A REFUSAL, which is the whole reason this arm
		// exists: a suspended loop is re-entered when detached work
		// finishes, and work that never started never finishes.
		return tools.DetachedResult{Result: refused(g.Name(), refusal)}, nil
	}
	return g.detached.CallDetached(ctx, turn, args)
}

func (g *gated) Call(ctx context.Context, args map[string]any) (mcp.Result, error) {
	if refusal := g.check(ctx, args); refusal != nil {
		return refused(g.Name(), refusal), nil
	}
	return g.Callable.Call(ctx, args)
}

// CallForTurn derives the acting principal FROM THE TURN when the context
// carries none.
//
// THE TURN IS THE AUTHORITY on these calls, and deriving it here rather than
// only at [tools.Surface] is what makes the gate correct for every caller: a
// seat tool invoked with a turn and no principal — which is what a direct
// call, a resumed loop and any surface added later look like — would otherwise
// reach the decision as NOBODY and be refused, naming an identity problem
// where there is none. [turnctx.WithPrincipal] leaves an answer already in the
// context alone, so an operator calling through their own credential is never
// overwritten by a turn they happen to be carrying.
func (g *gatedSeat) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (mcp.Result, error) {

	ctx = turnctx.WithPrincipal(ctx, turn)
	if refusal := g.check(ctx, args); refusal != nil {
		return refused(g.Name(), refusal), nil
	}
	return g.seat.CallForTurn(ctx, turn, args)
}

// check runs the decision for one call.
func (g *gated) check(ctx context.Context, args map[string]any) error {
	if g.authorize == nil {
		return ErrNoAuthorizer
	}
	// THE PRINCIPAL IS READ TWICE AND DECIDED ONCE. What it is needed for
	// here is only the personal default — whose record an unnamed verb is
	// about — and a read that answers nothing leaves the owner empty,
	// which is exactly what [Authorizer] is about to refuse on.
	principal, _ := iam.From(ctx)
	object := subjectOf[g.Name()].objectFor(principal, args)
	return g.authorize(ctx, authz.Action(g.Name()), object)
}

// mayWrite is the ask a WORK tool makes once it has read what it needs.
//
// A NIL ANSWER IS THE ALLOW and a non-nil one is the result to return. It is
// [gated.check]'s body without the table lookup, because these calls name
// their own action and their own object — the table cannot form either from
// the arguments alone.
//
// A NIL AUTHORIZER REFUSES, on [Deps.Authorize]'s rule: the field is pushed
// down by [Register] and [OperatorTools], so a nil here is a surface that
// wired no decision rather than one that decided to allow.
func (d WorkDeps) mayWrite(ctx context.Context, action authz.Action,
	object authz.Object) *tools.Result {

	return askAuthority(ctx, d.Authorize, action, object)
}

// mayWrite is [WorkDeps.mayWrite] for the knowledge base.
func (d PageDeps) mayWrite(ctx context.Context, action authz.Action,
	object authz.Object) *tools.Result {

	return askAuthority(ctx, d.Authorize, action, object)
}

// askAuthority is the one body both deps ask through.
func askAuthority(ctx context.Context, authorize Authorizer,
	action authz.Action, object authz.Object) *tools.Result {

	if authorize == nil {
		refusal := refused(string(action), ErrNoAuthorizer)
		return &refusal
	}
	if err := authorize(ctx, action, object); err != nil {
		refusal := refused(string(action), err)
		return &refusal
	}
	return nil
}

// refused is what a refused call answers the MODEL with.
//
// A FAILED RESULT AND NOT AN ERROR, which is internal/mcp's own contract: an
// error means the turn is being torn down and nothing is reported, while a
// refusal is something the model should see and reason about — "I may not do
// that, so I will ask somebody who can" is a legitimate next move and an
// invisible refusal is a round spent finding out nothing.
func refused(name string, why error) mcp.Result {
	return mcp.Result{
		Failed: true,
		Output: fmt.Sprintf("%s was refused: %s", name, why.Error()),
	}
}
