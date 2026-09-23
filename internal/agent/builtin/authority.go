package builtin

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

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
//
// # The arguments are decided here too, and first
//
// For the same reason the authority is: this wrapper is the one place every
// call of every builtin passes, whichever of the three callers made it. A
// call carrying an argument its tool does not declare is refused by name
// before anybody's authority is asked ([ErrUndeclaredArgument]) — the HTTP
// surface used to check that at its routes alone, so the other callers
// dropped what it refused.

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

// ErrUndecidable reports a call this node could not decide: the caller could
// not be established, or the chart the rule reads could not be.
//
// A SENTINEL rather than "any error that is not [ErrRefused]", because the
// surfaces that answer in status codes must tell three apart — refused,
// undecidable, and a wiring that decided nothing ([ErrNoAuthorizer]) — and an
// arm reading "everything else" would put the third in the second's 503,
// telling a caller to retry a build that will never answer.
var ErrUndecidable = errors.New("builtin: this node could not decide")

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
			return fmt.Errorf("%w who is calling %s: %w", ErrUndecidable,
				action, iam.Reason(ctx))
		case iam.Anonymous:
			return fmt.Errorf("%w: %s needs one and none was presented",
				ErrUnauthenticated, action)
		}
		return DecisionError(action,
			authz.Decide(ctx, principal, action, object, chart, time.Now()))
	}
}

// ErrUnauthenticated reports a call nobody presented a credential for.
//
// A REFUSAL — it wraps [ErrRefused] — and its own sentinel beside it, because
// the HTTP surface answers it 401 where every other refusal is 403: "present
// a credential" and "you may not" send a person to opposite places.
var ErrUnauthenticated = fmt.Errorf("%w: nobody presented a credential", ErrRefused)

// DecisionError is one authority decision as the error every surface reports
// it with: nil for an allow, [ErrUndecidable] for a decision the node could
// not take, [ErrRefused] for a refusal.
//
// EXPORTED so the one surface that decides before a tool runs — the HTTP
// write surface, refusing at the route — words its refusal from the SAME
// error the tool's own gate would have returned, through [Refusal]. Composed
// twice, the two sentences would agree only for as long as nobody edited
// either.
func DecisionError(action authz.Action, d authz.Decision) error {
	switch {
	case d.Unknown():
		return fmt.Errorf("%w %s: %w", ErrUndecidable, action, d.Err)
	case !d.Allowed:
		return fmt.Errorf("%w: %s refused (%s)", ErrRefused, action, d.Reason)
	}
	return nil
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

	// THE PERSONAL ONES THAT TAKE NO NAME are about the caller: `my_work`,
	// `mark_inbox` and `set_pins` take no handle, because a model that
	// could name whose day to read could read anybody's. An unnamed owner
	// is the CALLER — see [subject.objectFor].
	"my_work":    {kind: authz.KindPerson},
	"mark_inbox": {kind: authz.KindPerson},
	"set_pins":   {kind: authz.KindPerson},

	// AND THE ONES THAT DO ARE DECIDED ON WHOSE RECORD THE NAME IS, NOT ON
	// WHAT WAS TYPED. Each tool resolves its `handle` first
	// ([WorkDeps.personRecord]): the caller's own names are their own
	// record, somebody else's login is their holder's — a bound person's
	// SEAT — and `set_priorities` resolves anything else against the chart,
	// because a model types a name, a role or an email. Decided here on
	// the raw argument, a lead who named their report by role or by login
	// was refused as leading nobody called that, before the tool ever
	// resolved it — and an administrator's decision on a login was a
	// decision about a record nothing of that person's is under.
	"get_person":     {kind: authz.KindPerson, inTool: true},
	"work_inbox":     {kind: authz.KindPerson, inTool: true},
	"set_priorities": {kind: authz.KindPerson, inTool: true},

	// A SAVED VIEW IS EITHER, and its own class reads whichever it
	// names. Its container is a kind plus a key in ONE argument
	// (`project:ENG`, `unit:eng`, `person:ana`, `workspace`), which is
	// why this row states a parser rather than a kind. This decides the
	// view being WRITTEN; the one a save REPLACES is a stored row, and
	// the tool asks the same action on it once it has read it.
	//
	// PERSONAL IS A FLAG AND NOT A NAME: the view's owner is the caller's
	// own record — [iam.RecordOwner] — or nobody's. It was a free `owner`
	// handle, so a bound person typing their login saved a view under a
	// name no strip of theirs reads, and one that marked it protected was
	// then refused their own next save.
	"save_work_view": {kind: authz.KindView, personal: "personal", container: "container"},

	// THE TRASH IS DECIDED BY THE TASK'S OWN PROJECT, which is a stored row
	// and never an argument: the tools take a key or an id, and an id names
	// no project. So the gate cannot form the object, and these tools ask
	// the same action themselves once they have read it.
	//
	// Decided here with the empty object they had, every caller but the
	// holder of the admin grant was refused as naming no project — a
	// project's own lead could not take an item out of their own board,
	// which is the one thing [authz.ClassDestructive] exists to let them do.
	//
	// The read the tool decides on is OUTSIDE the write's snapshot, so the
	// project is passed to the writer as a PRECONDITION and the tracker
	// refuses a task filed elsewhere by then with a conflict — the same
	// holds for `update_work_item`'s re-route.
	"remove_work_item":  {kind: authz.KindTask, inTool: true},
	"restore_work_item": {kind: authz.KindTask, inTool: true},
}

// subject is what one tool is about, as the names of its own arguments.
type subject struct {
	// kind is what sort of thing the call acts on.
	kind authz.ObjectKind

	// owner is the argument naming WHOSE record it is, empty on a verb
	// that takes no handle.
	owner string

	// personal is the BOOLEAN argument that makes the object the caller's
	// own record, on a verb that never takes a name for it — see
	// `save_work_view`'s row.
	personal string

	// container is the argument naming the project, unit or container
	// the call is inside.
	container string

	// inTool marks a verb whose object its arguments do not STATE — a
	// stored row (which project a task is filed under), or a name the tool
	// resolves to a record (whose inbox a login means, whose priorities a
	// typed `handle` means). The gate does not decide it, and the tool asks
	// the same action through [WorkDeps.mayWrite] once it knows the object.
	// A tool marked so and asking nothing would be ungated — which is why
	// [TestARowDecidedToolAsksAfterItReads],
	// [TestALeadNamesAReportTheWayAModelTypesThem] and
	// [TestSomebodyElsesLoginNamesTheirHoldersRecord] call each one as a
	// caller the table refuses and require the refusal.
	inTool bool
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
		o.Owner = iam.RecordOwner(p)
	}
	if s.personal != "" && argBool(args, s.personal) {
		o.Owner = iam.RecordOwner(p)
		if o.Owner == "" {
			// A PERSONAL OBJECT NOBODY CAN OWN NAMES NOTHING, which every
			// class refuses. Left with its container it would be decided
			// as SHARED — the one reading an owner-less view has — and
			// the caller who has no record would be admitted to write a
			// tab everybody sees.
			return authz.Object{Kind: s.kind}
		}
	}
	if s.container != "" {
		o.ContainerKind, o.Container = containerArg(args, s.container)
		if s.kind != authz.KindView {
			o.Kind = cmp.Or(o.ContainerKind, s.kind)
		}
	}
	return o
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
	if kind == tracker.ContainerWorkspace {
		// THE WORKSPACE CARRIES NO ID, so `workspace:x` names nothing —
		// the tool refuses the spelling with the grammar in its message.
		return "", ""
	}
	return containerObject(tracker.Container{Kind: kind, ID: id})
}

// containerObject is a tracker container as the authority table's kind and
// key.
//
// ONE MAPPING for both of the places a view's container reaches a decision —
// the argument the gate reads and the STORED view `save_work_view` asks about
// once it has read it — so the two cannot decide one container two ways.
func containerObject(c tracker.Container) (authz.ObjectKind, string) {
	switch c.Kind {
	case tracker.ContainerWorkspace:
		return authz.KindCompany, tracker.ContainerWorkspace
	case tracker.ContainerProject:
		return authz.KindProject, tracker.ProjectKey(c.ID)
	case tracker.ContainerUnit:
		return authz.KindUnit, c.ID
	case tracker.ContainerPerson:
		return authz.KindPerson, c.ID
	}
	// A KIND THIS BUILD DOES NOT KNOW NAMES NOTHING, which every class
	// refuses. The tool parses the same string and refuses it too, with a
	// message that says what the grammar is — and the refusal a caller
	// reads should be that one, not an authority answer.
	return "", ""
}

// viewObject is a saved view as [authz.ClassSavedView] decides it: its owner,
// which makes it that person's record, and its container, which makes a
// shared one that container's lead's.
func viewObject(owner string, c tracker.Container) authz.Object {
	o := authz.Object{Kind: authz.KindView, Owner: owner}
	o.ContainerKind, o.Container = containerObject(c)
	return o
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
	if refusal := g.admit(ctx, args); refusal != nil {
		// NO SUSPENSION ON A REFUSAL, which is the whole reason this arm
		// exists: a suspended loop is re-entered when detached work
		// finishes, and work that never started never finishes.
		return tools.DetachedResult{Result: *refusal}, nil
	}
	return g.detached.CallDetached(ctx, turn, args)
}

func (g *gated) Call(ctx context.Context, args map[string]any) (mcp.Result, error) {
	if refusal := g.admit(ctx, args); refusal != nil {
		return *refusal, nil
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
	if refusal := g.admit(ctx, args); refusal != nil {
		return *refusal, nil
	}
	return g.seat.CallForTurn(ctx, turn, args)
}

// admit is what every call of a gated tool passes, on all three of its
// paths — the ARGUMENTS, then the AUTHORITY — answering nil to run the tool
// or the result it is answered with instead.
//
// THE ARGUMENTS HERE, at the one place every call of every builtin passes:
// a seat's turn, a worker's slice of it, a sandbox's bridge, the operator's
// assistant and the HTTP write surface all reach a builtin through this
// wrapper. The HTTP surface used to check them at its routes alone, so the
// other four dropped what the route refused — see [ErrUndeclaredArgument].
func (g *gated) admit(ctx context.Context, args map[string]any) *mcp.Result {
	if unread := undeclared(g.Callable, args); len(unread) > 0 {
		refusal := undeclaredRefusal(g.Callable, unread)
		return &refusal
	}
	if refusal := g.check(ctx, args); refusal != nil {
		result := refused(g.Name(), refusal)
		return &result
	}
	return nil
}

// ErrUndeclaredArgument reports a call carrying an argument its tool does not
// declare.
//
// REFUSED, NEVER DROPPED. A builtin reads the arguments its schema declares
// and nothing else, so an undeclared one was DROPPED — and a dropped argument
// is not a no-op, it is a call answered as though somebody had asked for
// less. `save_work_view` lost its free `owner` when a personal view became
// the caller's own (`personal: true`), and an assistant still sending the old
// shape had its PERSONAL view saved as a SHARED tab on the container,
// visible to everybody on it, under an answer saying it had worked. The
// model, the assistant and the client each fix a misspelt or retired argument
// the moment they are told its name; told nothing, none of them can.
//
// ITS OWN SENTINEL, not [ErrRefused]: nobody's authority was asked, and a
// surface answering in status codes answers it 400 — the request is the
// caller's to change — where a refusal is 403.
var ErrUndeclaredArgument = errors.New("builtin: this call carries an " +
	"argument its tool does not read")

// undeclared is the arguments a call carries that its tool's schema does not
// declare, quoted and sorted — nil when there are none.
//
// THE SCHEMA IS THE LIST, read off the tool rather than typed beside it, so an
// argument a tool gains is accepted the moment it can be read and one it loses
// is refused the moment it cannot. A second list anywhere would be the copy
// that drifts — which is what the HTTP surface's own copy of this check was,
// until it became this one.
func undeclared(tool tools.Callable, args map[string]any) []string {
	properties, _ := tool.Parameters()["properties"].(map[string]any)
	var unread []string
	for field := range args {
		if _, held := properties[field]; !held {
			unread = append(unread, fmt.Sprintf("%q", field))
		}
	}
	slices.Sort(unread)
	return unread
}

// undeclaredRefusal is what a call carrying unread arguments is answered:
// every one named, and what the tool does read, so one round fixes all of
// them.
func undeclaredRefusal(tool tools.Callable, unread []string) mcp.Result {
	properties, _ := tool.Parameters()["properties"].(map[string]any)
	takes := slices.Sorted(maps.Keys(properties))
	reads := "nothing"
	if len(takes) > 0 {
		reads = strings.Join(takes, ", ")
	}
	names := strings.Join(unread, ", ")
	return mcp.Result{
		Failed: true,
		Cause:  fmt.Errorf("%w: %s takes no %s", ErrUndeclaredArgument, tool.Name(), names),
		Output: fmt.Sprintf("%s takes no argument %s — it reads %s, and one it "+
			"does not read is refused rather than ignored, because ignoring it "+
			"answers a different request from the one you sent", tool.Name(),
			names, reads),
	}
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
	subject := subjectOf[g.Name()]
	if subject.inTool {
		// THE TOOL ASKS, with the same action and the object it resolved
		// — see [subject.inTool]. Deciding here as well, on an object the
		// arguments do not state, would refuse everybody the tool is
		// about to admit.
		return nil
	}
	principal, _ := iam.From(ctx)
	object := subject.objectFor(principal, args)
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
//
// THE REFUSAL RIDES AS THE CAUSE, so a surface answering in status codes reads
// which of the three it was from the value rather than from the sentence.
func refused(name string, why error) mcp.Result {
	return mcp.Result{Failed: true, Output: Refusal(name, why), Cause: why}
}

// Refusal is the ONE sentence an authority refusal reads as, on every surface
// that serves these verbs: a seat's own turn, the operator's assistant over
// MCP, and the HTTP write surface.
//
// EXPORTED FOR THE THIRD. The first two reach it through the gate, because
// they call the tools; the HTTP surface refuses at its ROUTE — before the
// tool runs, where the object is knowable from the path — and a route that
// composed its own sentence would be where "you may not" first read two ways.
// That is not cosmetic: a person told one thing by the dashboard and another
// by their assistant about the same verb asks which of them is wrong, and the
// answer is neither — so the only way to keep the question from arising is to
// have one sentence and nowhere else to write one.
//
// IT NAMES THE VERB AND THE REASON AND NEVER THE CALLER. A wording that said
// who was refused would differ for every caller asking the same question, and
// the caller already knows who they are.
func Refusal(verb string, why error) string {
	return fmt.Sprintf("%s was refused: %s", verb, why.Error())
}
