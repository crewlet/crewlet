package builtin

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// WHO MAY CALL A TOOL. The same tools serve three callers (a seat's registry in
// a turn, the operator's assistant over MCP, the HTTP write surface), so the
// decision is a seam on the deps each constructs, populated from [authz.Decide]:
// decided three ways, the answers would drift silently. Not [tools.Surface]'s
// Guard: that carries no principal, object or arguments, is per phase session,
// and the operator's assistant never builds a Surface (internal/tools' doc).
//
// A nil Authorizer REFUSES, unlike the Guard's nil: a missing skill gate is a
// company that locked no skills, a missing authority decision a surface that
// would serve every verb to every caller. Undeclared arguments are refused here
// too, first ([ErrUndeclaredArgument]): this wrapper is the one place every
// call of every builtin passes.

// Authorizer decides whether the party behind a call may make it.
//
// The principal comes from the context on every call, never captured: a
// seat's tools are cloned into its lease and an MCP session outlives a request,
// so one bound at construction would answer for whoever was there then.
//
// A nil error allows. [ErrRefused] is an answer a retry never changes; any
// other error is a node that could not decide (403 versus 503 on a surface).
type Authorizer func(ctx context.Context, action authz.Action,
	object authz.Object) error

// ErrRefused reports a call the acting party may not make. Kept distinct from
// [ErrUndecidable]: collapsing "this node could not tell" into "you may not"
// sends somebody to ask for a capability they already hold.
var ErrRefused = errors.New("not allowed")

// ErrUndecidable reports a call this node could not decide: the caller could
// not be established, or the chart the rule reads could not be.
//
// A sentinel rather than "any error but [ErrRefused]": status-code surfaces
// tell three apart, and an "everything else" arm would answer
// [ErrNoAuthorizer] 503, telling a caller to retry a build that never decides.
var ErrUndecidable = errors.New("can't check access right now")

// ErrNoAuthorizer reports deps nobody wired a decision into. Its own sentinel
// so the refusal names the wiring, not the caller: "you may not" would send
// somebody to ask for access from a build that never asked anybody.
var ErrNoAuthorizer = errors.New("builtin: no authority decision is wired, " +
	"so nothing here may be called")

// Decide is the Authorizer every surface builds, over one chart: one
// constructor, because composing [authz.Decide] has two parts a caller can get
// wrong — reading the principal three-valued, and turning an UNKNOWN decision
// into an error rather than a refusal.
//
// A seat's call in a turn somebody woke is decided for whoever woke it too, as
// far as the apps go ([authz.DecideWithinApps], ADR-0109): every action it
// decides, the gate's and the tool's own asks, reads the ceiling its gate bound
// ([bindCeiling]), composed only when the action's row names an app. A seat's
// decision with nothing bound is refused as undecidable (the closed end, as
// [authz.CeilingKind]'s zero); a caller deciding outside any turn on purpose
// says so ([OutsideAnyTurn]).
func Decide(chart authz.Chart) Authorizer {
	return func(ctx context.Context, action authz.Action,
		object authz.Object) error {

		principal, how := iam.From(ctx)
		switch how {
		case iam.Unknown:
			return fmt.Errorf("%w (can't tell who is calling): %w",
				ErrUndecidable, iam.Reason(ctx))
		case iam.Anonymous:
			return ErrUnauthenticated
		}
		// Judged at the instant the request was routed ([authz.DecidedAt]):
		// a tool-backed route's call is one more check of that request, and
		// judged at its own clock a proof the route admitted could lapse
		// mid-body and the call be refused as stale. A turn, and a call no
		// route decided (MCP, the act route), carry no instant: judged now.
		now := authz.DecidedAt(ctx)
		if principal.Kind != iam.KindSeat {
			return DecisionError(action, authz.Decide(ctx, principal, action, object, chart, now))
		}
		bound, ok := ctx.Value(ceilingKey{}).(boundCeiling)
		if !ok {
			// THE CLOSED END: no gate bound a ceiling, so a path skipped
			// the gate. Decided for nobody, it would grant every app.
			return fmt.Errorf("%w: %w", ErrUndecidable, errNoCeiling)
		}
		if !authz.NamesApp(action) {
			return DecisionError(action, authz.Decide(ctx, principal, action, object, chart, now))
		}
		ceiling, err := bound()
		if err != nil {
			return fmt.Errorf("%w (can't tell who this turn's work is for): %w", ErrUndecidable, err)
		}
		return DecisionError(action,
			authz.DecideWithinApps(ctx, principal, ceiling, action, object, chart, now))
	}
}

// errNoCeiling is a seat's decision that reached the authority with no
// requester ceiling bound to it: no gate decided the call, and no caller said
// it was made outside any turn ([OutsideAnyTurn]).
var errNoCeiling = errors.New("builtin: a seat's call reached the authority with nobody bound as whom it " +
	"works for — no gate decided it, and nothing said it was made outside any turn")

// ErrUnauthenticated reports a call nobody presented a credential for. It
// wraps [ErrRefused] but is its own sentinel because the HTTP surface answers
// it 401, not 403: "present a credential" and "you may not" send a person to
// opposite places.
var ErrUnauthenticated = fmt.Errorf("%w: sign in first", ErrRefused)

// DecisionError is one authority decision as the error every surface reports
// it with: nil for an allow, [ErrUndecidable] for a decision the node could
// not take, [ErrRefused] for a refusal.
//
// Exported so the HTTP write surface, refusing at the route, words its refusal
// through [Refusal] from the same error the tool's gate returns: composed
// twice, the two sentences would drift.
func DecisionError(action authz.Action, d authz.Decision) error {
	switch {
	case d.Unknown():
		return fmt.Errorf("%w: %w", ErrUndecidable, d.Err)
	case !d.Allowed:
		return &DecisionRefused{Action: action, Decision: d}
	}
	return nil
}

// DecisionRefused is an authority refusal carrying the decision that made it.
//
// The decision, not only its sentence, so a status-code surface renders the
// table's own refusal (`403 unauthorized` with the reason and the grants that
// would admit, or `step_up_required` with the proof window); a tool result's
// cause is the one carrier from the gate to the surface. It is [ErrRefused]
// for [errors.Is], and its text is the sentence [Refusal] embeds.
type DecisionRefused struct {
	Action   authz.Action
	Decision authz.Decision
}

func (e *DecisionRefused) Error() string {
	refusal := fmt.Sprintf("%s (%s)", ErrRefused, e.Decision.Reason)
	if e.Decision.Reason == authz.ReasonUnseated {
		// The one refusal whose remedy (a binding) no grant names, and the
		// envelope's `grants` is empty by design, so the sentence must.
		refusal += ": it's made as a seat and this credential acts as none. " +
			"Bind it to a human seat with `crewlet iam bind`"
	}
	if e.Decision.Reason == authz.ReasonRole && e.Decision.Role.Valid() {
		// The role is the remedy and the artifact's owner gives it; the
		// envelope carries it as `role`, and the sentence says the same.
		refusal += ": it needs the " + string(e.Decision.Role) + " role on the " +
			"artifact, which its owner can share with you"
	}
	if e.Decision.Reason == authz.ReasonCeiling {
		refusal += ceilingWords(e.Decision)
	}
	if e.Decision.Reason == authz.ReasonTokenRefused {
		refusal += ": it needs the person here, signed in as themselves, which an access token is not"
	}
	return refusal
}

// ceilingWords is what a refusal of the requester ceiling says (ADR-0109), by
// whom it was decided for: a stranger to the company, a requester who may not
// act in it at all (no app would admit them), one lacking the verb's app (named,
// since an administrator granting it changes the answer; a person is to be told
// so, a credential is not), or one who could not take the verb themselves.
// Never the requester's name: the agent knows who woke its turn, and [Refusal]
// keeps one sentence per kind of requester.
func ceilingWords(d authz.Decision) string {
	switch {
	case d.Ceiling == authz.CeilingStranger:
		return ": this turn was woken by somebody the company does not know, and you use no app " +
			"and reach nothing of anybody's for them. Answer from what you already know, or a " +
			"person of the company has to ask"
	case d.RequesterReason == authz.ReasonStage:
		return ": whoever this turn is working for may not act in the company now, so you may " +
			"not act for them either"
	case len(d.RequesterGrants) > 0:
		said := make([]words.Said, 0, len(d.RequesterGrants))
		for _, app := range d.RequesterGrants {
			name := string(app)
			if name != "" {
				name = strings.ToUpper(name[:1]) + name[1:]
			}
			said = append(said, words.Said(name))
		}
		apps := words.Or(said)
		if d.RequesterKind == iam.KindMachine {
			return ": the credential this turn is working for does not use " + apps + ", so you " +
				"may not use it for it. Answer without it; an administrator can give the " +
				"credential " + apps
		}
		return ": the person this turn is working for does not use " + apps + ", so you may " +
			"not use it for them. Answer without it, or tell them an administrator can give " +
			"them " + apps
	}
	return ": whoever this turn is working for could not do this themselves, so you may not do " +
		"it for them"
}

// Unwrap makes every [DecisionRefused] an [ErrRefused].
func (e *DecisionRefused) Unwrap() error { return ErrRefused }

// subjectOf is what each tool is ABOUT, as the names of its own arguments: one
// table, not thirty drifting checks, and only the extraction; the rule is
// internal/authz's. Field names rather than closures, so
// [TestEveryObjectFieldIsOneItsToolDeclares] holds each against the tool's JSON
// Schema: an undeclared field reads empty and gates the verb shut for all but
// an admin. A tool with no row is about nothing, which every field-reading
// class refuses. Rows deliberately absent read no field: an app's own verbs
// ([authz.ClassApp], decided on the app alone), the overview's and the code
// tools' ([authz.ClassEverybody]; see codetools.go), the seat-only `a2a_ask`
// (the table bars a person before any class reads), and `code_checkout`,
// offered only by an executor run's bridge, decided by [CodeDeps.ResolveRunRepos].
var subjectOf = map[string]subject{
	// An item and its thread are decided on the item: somebody without
	// Worklet may still read and answer an item an OPEN ASK names them on,
	// a stored row ([authz.ClassAppOrAsked]). A tracker holder is admitted
	// at once; anybody else gets [authz.ErrUnresolved], and the tool probes
	// the asks ([worklet.AskedOn]) and decides again. See
	// [getWorkItem.CallForTurn] and [commentOnWorkItem.CallForTurn].
	GetWorkItemTool:           {kind: authz.KindTask, inTool: true},
	worklet.CommentOnWorkTool: {kind: authz.KindTask, inTool: true},

	// The personal ones that take no name are about the caller (a model
	// that could name whose day to read could read anybody's); see
	// [subject.objectFor].
	worklet.MyWorkTool:    {kind: authz.KindPerson},
	worklet.MarkInboxTool: {kind: authz.KindPerson},
	worklet.SetPinsTool:   {kind: authz.KindPerson},
	// And the code log's and the artifacts log's halves of an inbox's marks.
	CodeMarkInboxTool:     {kind: authz.KindPerson},
	ArtifactMarkInboxTool: {kind: authz.KindPerson},
	// And a person's viewed files of a change: the caller's own record, naming no handle.
	CodeMarkViewedTool: {kind: authz.KindPerson},

	// The ones that do are decided on whose record the name resolves to,
	// not on what was typed: each tool resolves its `handle` first
	// ([WorkDeps.personRecord]) — somebody else's login is their holder's
	// seat, and `set_priorities` resolves a name, role or email against the
	// chart. Decided on the raw argument, a lead naming their report by
	// role or login would be refused.
	worklet.GetPersonTool:     {kind: authz.KindPerson, inTool: true},
	worklet.WorkInboxTool:     {kind: authz.KindPerson, inTool: true},
	worklet.SetPrioritiesTool: {kind: authz.KindPerson, inTool: true},

	// A saved view is either; its container is a kind plus a key in ONE
	// argument (`project:ENG`, `unit:eng`, `person:ana`, `workspace`),
	// hence a parser rather than a kind. This decides the view being
	// written; the tool asks the same action of the stored view a save
	// replaces once it has read it.
	//
	// Personal is a flag, not a name: the owner is the caller's own record
	// ([iam.RecordOwner]) or nobody's, so a typed login can never file a
	// view under a name no strip of the caller's reads.
	worklet.SaveWorkViewTool: {kind: authz.KindView, personal: "personal", container: "container"},
	// A delete names only the view's id, so the tool reads the view and
	// asks [authz.ActionViewDelete] of its owner and container.
	worklet.DeleteWorkViewTool: {kind: authz.KindView, inTool: true},

	// The trash is decided on the task's stored row (its project and
	// reporter), which no argument states, so the tool reads it and asks
	// itself ([authz.ClassTidy]: the project's lead, the item's filer, or
	// an admin); decided here on an empty object, a lead could not tidy
	// their own board. That read is outside the write's snapshot, so the
	// project goes to the writer as a PRECONDITION and a task re-filed
	// meanwhile is a conflict (as with `update_work_item`'s re-route).
	worklet.RemoveWorkItemTool:  {kind: authz.KindTask, inTool: true},
	worklet.RestoreWorkItemTool: {kind: authz.KindTask, inTool: true},

	// So is a file's removal, decided on the project it is kept in, which
	// the tool settles first: a call naming no project means the caller's
	// own team's, which the gate cannot know.
	worklet.RemoveProjectFileTool: {kind: authz.KindProject, inTool: true},

	// So is a move, against the item's own project read off the row: a
	// move is a re-route with a re-key, so it is the lead's of the project
	// the item is leaving (workmove.go).
	worklet.MoveWorkItemTool: {kind: authz.KindTask, inTool: true},

	// A seat's work is decided on the seat, never quite what the arguments
	// state: a pause or resume resolves a typed handle (`@sre`) through the
	// org; a note names a turn, whose seat only its running node can say,
	// so steer_turn asks it first; a run's answer is decided on the run's
	// row (its requester, else its seat's lead); a schedule run on every
	// seat the schedule resolves to. See seatpause.go, steer.go,
	// answerexecutor.go and runschedule.go.
	PauseSeatTool:      {kind: authz.KindPerson, inTool: true},
	ResumeSeatTool:     {kind: authz.KindPerson, inTool: true},
	SteerTurnTool:      {kind: authz.KindPerson, inTool: true},
	AnswerExecutorTool: {kind: authz.KindPerson, inTool: true},
	RunScheduleTool:    {kind: authz.KindPerson, inTool: true},

	// An artifact is decided on its stored row (owner, shares, whether an
	// agent owns it) and, in a turn somebody woke, again for them, so each
	// tool reads the row and asks [authz.DecideWithin] itself
	// (artifacts.go). Absent: the listing (each row decided as a read), the
	// creates (app verbs, decided again by the tool with the caller as
	// owner beside the parent folder's role), and the brand (everybody's).
	GetArtifactTool:         {kind: authz.KindArtifact, inTool: true},
	RenderArtifactTool:      {kind: authz.KindArtifact, inTool: true},
	PublishArtifactTool:     {kind: authz.KindArtifact, inTool: true},
	UpdateArtifactTool:      {kind: authz.KindArtifact, inTool: true},
	ShareArtifactTool:       {kind: authz.KindArtifact, inTool: true},
	UnshareArtifactTool:     {kind: authz.KindArtifact, inTool: true},
	UploadArtifactAssetTool: {kind: authz.KindArtifact, inTool: true},
	// A move is decided per artifact on each stored row, and its
	// destination on the folder's top-level owner and shares
	// (artifacts_folders.go).
	MoveArtifactTool: {kind: authz.KindArtifact, inTool: true},
	// A comment is decided on the same row; the writer keeps an edit to
	// the remark's author (artifacts_comments.go).
	ListArtifactCommentsTool:   {kind: authz.KindArtifact, inTool: true},
	CommentOnArtifactTool:      {kind: authz.KindArtifact, inTool: true},
	ResolveArtifactCommentTool: {kind: authz.KindArtifact, inTool: true},
	EditArtifactCommentTool:    {kind: authz.KindArtifact, inTool: true},
	RemoveArtifactCommentTool:  {kind: authz.KindArtifact, inTool: true},

	// And its datastore: the same row, then each collection's own rules
	// (artifactdata.go).
	ReadArtifactDataTool:  {kind: authz.KindArtifact, inTool: true},
	QueryArtifactDataTool: {kind: authz.KindArtifact, inTool: true},
	WriteArtifactDataTool: {kind: authz.KindArtifact, inTool: true},
	SetArtifactSchemaTool: {kind: authz.KindArtifact, inTool: true},

	// A chat message is decided on its author, a stored row (the tools
	// name it by id). An admin is admitted on the unread message
	// (moderation, without showing it to them); anybody else gets
	// [authz.ErrUnresolved], reads it as themselves and is decided again on
	// its author. See [ChatDeps.remarkAuthority].
	chatlet.EditMessageTool:   {kind: authz.KindMessage, inTool: true},
	chatlet.DeleteMessageTool: {kind: authz.KindMessage, inTool: true},

	// And a read cursor is the caller's own record: `mark_chat_read` takes
	// no handle. So is a Home board.
	chatlet.MarkChatReadTool: {kind: authz.KindPerson},
	SaveHomeBoardTool:        {kind: authz.KindPerson},
	SaveInstructionsTool:     {kind: authz.KindPerson},
}

// ownBookkeeping are the tools whose every write is the CALLER'S OWN
// BOOKKEEPING (read cursors, inbox marks on either log, pins, a Home board),
// each a personal tool taking no name ([subjectOf]), so it acts only for its
// caller. Declared beside [subjectOf] because the runtime audit files these
// under a type of their own that the company's activity feed leaves out
// (internal/api/operator).
var ownBookkeeping = map[string]bool{
	worklet.MarkInboxTool:    true,
	worklet.SetPinsTool:      true,
	CodeMarkInboxTool:        true,
	CodeMarkViewedTool:       true,
	ArtifactMarkInboxTool:    true,
	chatlet.MarkChatReadTool: true,
	SaveHomeBoardTool:        true,
	SaveInstructionsTool:     true,
}

// OwnBookkeeping reports whether every write a tool makes is the caller's own
// bookkeeping (see [ownBookkeeping]).
func OwnBookkeeping(name string) bool { return ownBookkeeping[name] }

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

	// inTool marks a verb whose object its arguments do not state (a
	// stored row, or a name the tool resolves to a record). The gate skips
	// it; the tool asks the same action through [WorkDeps.mayWrite] once it
	// knows the object. One asking nothing would be ungated, which
	// [TestARowDecidedToolAsksAfterItReads],
	// [TestALeadNamesAReportTheWayAModelTypesThem] and
	// [TestSomebodyElsesLoginNamesTheirHoldersRecord] catch.
	inTool bool
}

// objectFor is one call's object, from the caller and the arguments.
//
// An absent owner is the caller on the PERSONAL kind only: those verbs take no
// handle or default to yours, so [authz.ReasonUnnamed] would refuse them all.
// A view is deliberately not that kind: an owner-less view is SHARED.
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
			// A personal object nobody can own names nothing, which every
			// class refuses; left with its container it would be decided
			// SHARED, admitting a recordless caller to a tab all see.
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

// containerArg reads a container argument as its kind and its key, in both
// spellings the tools use: `write_project`'s bare project key and
// `save_work_view`'s `kind:id` grammar. A bare value is a PROJECT.
func containerArg(args map[string]any, field string) (authz.ObjectKind, string) {
	raw := strings.TrimSpace(stringArg(args, field))
	if raw == "" {
		return "", ""
	}
	kind, id, found := strings.Cut(raw, ":")
	if !found {
		if strings.EqualFold(raw, worklet.ContainerWorkspace) {
			return authz.KindCompany, worklet.ContainerWorkspace
		}
		return authz.KindProject, worklet.ProjectKey(raw)
	}
	if kind == worklet.ContainerWorkspace {
		// The workspace carries no id, so `workspace:x` names nothing;
		// the tool refuses it with the grammar in its message.
		return "", ""
	}
	return containerObject(worklet.Container{Kind: kind, ID: id})
}

// containerObject is a tracker container as the authority table's kind and
// key: one mapping for the gate's argument and the stored view `save_work_view`
// reads, so one container is never decided two ways.
func containerObject(c worklet.Container) (authz.ObjectKind, string) {
	switch c.Kind {
	case worklet.ContainerWorkspace:
		return authz.KindCompany, worklet.ContainerWorkspace
	case worklet.ContainerProject:
		return authz.KindProject, worklet.ProjectKey(c.ID)
	case worklet.ContainerUnit:
		return authz.KindUnit, c.ID
	case worklet.ContainerPerson:
		return authz.KindPerson, c.ID
	}
	// A kind this build does not know names nothing; the tool refuses the
	// same string with the grammar, the message a caller should read.
	return "", ""
}

// viewObject is a saved view as [authz.ClassSavedView] decides it: its owner,
// which makes it that person's record, and its container, which makes a
// shared one that container's lead's.
func viewObject(owner string, c worklet.Container) authz.Object {
	o := authz.Object{Kind: authz.KindView, Owner: owner}
	o.ContainerKind, o.Container = containerObject(c)
	return o
}

// stringArg is one argument as a string, or empty, never an error: what an
// absent argument means is the rules' to say (a field-reading class refuses
// it, a personal class reads the caller), not this table's.
func stringArg(args map[string]any, field string) string {
	held, _ := argval.String(args[field])
	return held
}

// gated wraps one tool with the authority decision. A wrapper rather than a
// call in each tool: the action IS the tool's name (this package's walks hold
// the table and the tools together), so there is one place to miss the check.
//
// A POINTER, load-bearing: internal/engine compares a seat's builtins with `==`
// against the applied epoch's objects, and a struct holding a func field is
// uncomparable, a runtime panic at that comparison.
type gated struct {
	tools.Callable
	authorize Authorizer

	// requesters composes who a seat's turn is acting for, bound to each of
	// the turn's calls ([bindCeiling]); nil binds nobody, which a surface no
	// seat calls through (the operator's) is.
	requesters Requesters
}

// gatedSeat is [gated] for a tool that is also [tools.SeatCallable]. Two types,
// not a nil check: [tools.Invoke] dispatches on the interface, and one wrapper
// implementing both would make every read-only builtin look seat-scoped,
// refused outside a turn.
type gatedSeat struct {
	gated
	seat tools.SeatCallable
}

// Unwrap implements [tools.Wrapper], so the surface reads what the gated tool
// declares (its refusal target) through the gate. [gatedSeat] inherits it.
func (g *gated) Unwrap() tools.Callable { return g.Callable }

func gate(tool tools.Callable, authorize Authorizer, requesters Requesters) tools.Callable {
	wrapped := gated{Callable: tool, authorize: authorize, requesters: requesters}
	if held, ok := tool.(tools.SeatCallable); ok {
		return &gatedSeat{gated: wrapped, seat: held}
	}
	return &wrapped
}

func (g *gated) Call(ctx context.Context, args map[string]any) (mcp.Result, error) {
	if refusal := g.admit(ctx, args); refusal != nil {
		return *refusal, nil
	}
	return g.Callable.Call(ctx, args)
}

// CallInTurn is a seat's call of a gated tool that is not seat-scoped, made
// inside a turn ([tools.TurnBound]): decided as every call is, with the turn's
// requester ceiling bound to it ([bindCeiling]), so a read-only builtin is
// held to whoever woke the turn as the seat-scoped ones are — and still
// answers as itself outside any turn ([gated.Call]).
func (g *gated) CallInTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (mcp.Result,
	error) {

	ctx = bindCeiling(turnctx.WithPrincipal(ctx, turn), turn, g.requesters)
	if refusal := g.admit(ctx, args); refusal != nil {
		return *refusal, nil
	}
	return g.Callable.Call(ctx, args)
}

// CallForTurn derives the acting principal FROM THE TURN when the context
// carries none, here rather than only at [tools.Surface]: a direct call, a
// resumed loop or a later surface would otherwise reach the decision as nobody
// and be refused. [turnctx.WithPrincipal] leaves a principal already in the
// context alone, so an operator's own credential is never overwritten by a
// turn they carry.
func (g *gatedSeat) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (mcp.Result, error) {

	ctx = bindCeiling(turnctx.WithPrincipal(ctx, turn), turn, g.requesters)
	if refusal := g.admit(ctx, args); refusal != nil {
		return *refusal, nil
	}
	return g.seat.CallForTurn(ctx, turn, args)
}

// admit is what every call of a gated tool passes on all three paths — the
// ARGUMENTS, then the AUTHORITY — answering nil to run the tool or the result
// to answer instead. Arguments are checked here because every caller (a turn,
// a worker, an executor's bridge, the operator's assistant, the HTTP surface)
// reaches a builtin through this wrapper ([ErrUndeclaredArgument]).
func (g *gated) admit(ctx context.Context, args map[string]any) *mcp.Result {
	if unread := undeclared(g.Callable, args); len(unread) > 0 {
		refusal := undeclaredRefusal(g.Callable, unread)
		return &refusal
	}
	if refusal := g.check(ctx, args); refusal != nil {
		result := authorityRefusal(g.Name(), refusal)
		return &result
	}
	return nil
}

// ErrUndeclaredArgument reports a call carrying an argument its tool does not
// declare.
//
// Refused, never dropped: a builtin reads only its schema's arguments, and a
// dropped one answers the call as though less were asked (a retired argument
// can turn a personal view into a shared tab, reported as done). Told its
// name, the model, assistant or client fixes it at once.
//
// Its own sentinel, not [ErrRefused]: nobody's authority was asked, and a
// status-code surface answers it 400 (the caller's to change), not 403.
var ErrUndeclaredArgument = errors.New("builtin: this call carries an " +
	"argument its tool does not read")

// UndeclaredArguments is an [ErrUndeclaredArgument] refusal as a value: the
// tool and the arguments it does not read, so a surface answering in status
// codes names them as a field rather than in a sentence written for a model
// (ADR-0060).
type UndeclaredArguments struct {
	Tool  string
	Names []string
}

func (e *UndeclaredArguments) Error() string {
	return fmt.Sprintf("%s: %s takes no %s", ErrUndeclaredArgument, e.Tool,
		strings.Join(e.Names, ", "))
}

// Is reports the sentinel the refusal is an instance of.
func (e *UndeclaredArguments) Is(target error) bool { return target == ErrUndeclaredArgument }

// undeclared is the arguments a call carries that its tool's schema does not
// declare, sorted, or nil when there are none. The schema is the list, read off
// the tool, so no second copy can drift from it.
func undeclared(tool tools.Callable, args map[string]any) []string {
	properties, _ := tool.Parameters()["properties"].(map[string]any)
	var unread []string
	for field := range args {
		if _, held := properties[field]; !held {
			unread = append(unread, field)
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
	quoted := make([]string, len(unread))
	for i, name := range unread {
		quoted[i] = fmt.Sprintf("%q", name)
	}
	names := strings.Join(quoted, ", ")
	return mcp.Result{
		Failed: true,
		// INVALID, beneath the sentinel: the request is the caller's to
		// change, and a surface answering in status codes reads both.
		Cause: mcp.Classify(mcp.RefusalInvalid,
			&UndeclaredArguments{Tool: tool.Name(), Names: unread}),
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
	// The principal is read here only for the personal default; a read
	// answering nothing leaves the owner empty, which [Authorizer] refuses.
	about := subjectOf[g.Name()]
	if about.inTool {
		// The tool asks with the object it resolves ([subject.inTool]);
		// deciding here on an object the arguments do not state would
		// refuse everybody the tool admits.
		return nil
	}
	principal, _ := iam.From(ctx)
	object := about.objectFor(principal, args)
	return g.authorize(ctx, authz.Action(g.Name()), object)
}

// Admits reports whether the authority table could admit the party on ctx to
// tool at all, before any object is named: which verbs a surface offers. The
// object is the gate's own for an argument-less call ([subject.objectFor]), or
// for a tool-decided verb the UNRESOLVED object of its kind
// ([authz.Object.Unresolved], [unresolvedName]).
//
// [authz.ErrUnresolved] admits: whose record it is decides, so a lead can be
// admitted on a report (as [ErrUndecidable] it would answer leads with a
// retry). [authz.ReasonStepUp] admits too: the caller clears it, and a press
// is answered `step_up_required` for the step-up ceremony. Otherwise the error
// is the [Authorizer]'s own. Decided [OutsideAnyTurn]: an offer is the seat's
// own authority; whoever later wakes a turn bounds its calls, not the offer.
func Admits(ctx context.Context, authorize Authorizer, tool string) error {
	if authorize == nil {
		return ErrNoAuthorizer
	}
	ctx = OutsideAnyTurn(ctx)
	about := subjectOf[tool]
	object := authz.Object{Kind: about.kind, Owner: unresolvedName, Unresolved: true}
	if !about.inTool {
		principal, _ := iam.From(ctx)
		object = about.objectFor(principal, nil)
	}
	err := authorize(ctx, authz.Action(tool), object)
	var refused *DecisionRefused
	if errors.Is(err, authz.ErrUnresolved) ||
		errors.As(err, &refused) && refused.Decision.Reason == authz.ReasonStepUp {
		return nil
	}
	return err
}

// unresolvedName is the owner an unresolved object is decided under when the
// call names no record (a turn a note is for, a verb offered before anything is
// named). No login (it carries a dot or a colon) or seat handle (lowercase,
// digits, hyphens) can equal it, so the self arm never matches and, with
// [authz.Object.Unresolved], the lead arm asks only whether the caller leads
// anybody. Not empty: personal classes refuse an empty owner first.
const unresolvedName = "(unresolved)"

// mayWrite is the ask a WORK tool makes once it has read what it needs: nil
// allows, otherwise the result to return. These calls name their own action
// and object, which the table cannot form from the arguments. A nil Authorizer
// refuses ([Deps.Authorize]'s rule): [Register] and [OperatorTools] push the
// field down, so nil is a surface that wired no decision.
func (d WorkDeps) mayWrite(ctx context.Context, action authz.Action,
	object authz.Object) *tools.Result {

	return askAuthority(ctx, d.Authorize, action, object)
}

// decide is [WorkDeps.mayWrite]'s question answered as the [Authorizer]'s own
// error rather than as a result, for a tool that has to tell a refusal from
// an undecidable answer — or keep the refusal to report later, against an
// outcome only the writer can see.
func (d WorkDeps) decide(ctx context.Context, action authz.Action,
	object authz.Object) error {

	if d.Authorize == nil {
		return ErrNoAuthorizer
	}
	return d.Authorize(ctx, action, object)
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
		refusal := authorityRefusal(string(action), ErrNoAuthorizer)
		return &refusal
	}
	if err := authorize(ctx, action, object); err != nil {
		refusal := authorityRefusal(string(action), err)
		return &refusal
	}
	return nil
}

// authorityRefusal is what a call the authority refused answers the MODEL
// with: a failed result, not an error (internal/mcp's contract: an error tears
// the turn down unreported, a refusal is something the model can reason about
// and route around). The refusal rides as the cause beneath its class
// ([authorityClass]), so a status-code surface reads it with [errors.Is] or
// [mcp.RefusalOf] rather than from the sentence.
func authorityRefusal(name string, why error) mcp.Result {
	return mcp.Result{Failed: true, Output: Refusal(name, why),
		Cause: mcp.Classify(authorityClass(why), why)}
}

// authorityClass is the class an authority answer reads as on a surface that
// is not a model: [ErrUndecidable] is unavailable, since waiting can clear it;
// everything else is forbidden — a refusal, an unidentified caller (told apart
// by [ErrUnauthenticated]), or a surface wired with no decision.
func authorityClass(why error) mcp.Refusal {
	if errors.Is(why, ErrUndecidable) {
		return mcp.RefusalUnavailable
	}
	return mcp.RefusalForbidden
}

// Refusal is the ONE sentence an authority refusal reads as, on every surface
// that serves these verbs: a seat's turn, the operator's assistant over MCP,
// and the HTTP write surface. Exported for the last, which refuses at its route
// before the tool runs; a sentence of its own would let the dashboard and the
// assistant word one refusal two ways. It names the verb and the reason, never
// the caller, so every caller asking the same question reads the same answer.
func Refusal(verb string, why error) string {
	return fmt.Sprintf("%s didn't run: %s", verb, why.Error())
}
