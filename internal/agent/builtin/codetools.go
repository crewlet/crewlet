package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE CODE READ TOOLS: the company's Gitlet repositories (`code.backend:
// gitlet`), read on the server rather than in a clone — code_list_repos,
// code_get_repo, code_resolve, code_read, code_diff, code_compare,
// code_landed_in, code_log and code_blame here, a repository's changes in
// codechanges.go and code_search in codesearch.go — each named for its
// authority row, in ONE list ([CodeTools]) a seat's registry and the operator
// catalogue share. The writes join it (codewrite.go) through the same
// resolution of a repository and the turn's hold on it (codehold.go). How a
// read is judged, decided for the requester, levelled, fenced and paged:
// package doc, "The code tools".

// The code read tools' names: each is its authority row's action.
const (
	CodeListReposTool = string(authz.ActionCodeListRepos)
	CodeGetRepoTool   = string(authz.ActionCodeGetRepo)
	CodeResolveTool   = string(authz.ActionCodeResolve)
	CodeReadTool      = string(authz.ActionCodeReadFile)
	CodeDiffTool      = string(authz.ActionCodeDiff)
	CodeCompareTool   = string(authz.ActionCodeCompare)
	CodeLandedInTool  = string(authz.ActionCodeLandedIn)
	CodeLogTool       = string(authz.ActionCodeLog)
	CodeBlameTool     = string(authz.ActionCodeBlame)
)

// CodeToolNames are the twelve code reads in catalogue order: the
// repositories, a ref, the reads by object id (the history's last), a
// repository's changes ([ChangeToolNames]) and a commit's check failures
// (codechecks.go).
func CodeToolNames() []string {
	return append([]string{CodeListReposTool, CodeGetRepoTool, CodeResolveTool, CodeReadTool,
		CodeDiffTool, CodeCompareTool, CodeLandedInTool, CodeLogTool, CodeBlameTool},
		append(ChangeToolNames(), CodeGetCheckFailuresTool)...)
}

// The listing pages. A repository row carries up to a kibibyte of
// description and its protection rules, so a page is cut, whole rows, where it
// outgrows one tool answer; twenty-five is a screen of them. A branch row is a
// name and an id, so a hundred fit beside the detail unless the names are long.
const (
	defaultCodeRepoList = 25
	maxCodeRepoList     = 100
	codeBranchesPage    = 100
)

// CodeReader is Gitlet as these tools ask it (estate.CodeSeam, declared by its
// consumer), routed: this node's copy of the code log where it serves, a data
// node's otherwise, and a repository's home Gitlet node for its content.
type CodeReader interface {
	Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error)
	ResolveRef(ctx context.Context, q gitlet.RefQuery) (gitlet.RefAnswer, error)
	GitRead(ctx context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error)
	// FloorOf is this node's floor on repository repoID — its own pushes to
	// it, never its writes to any other — which every read by object id of it
	// is asked with, so the answering copy holds them.
	FloorOf(repoID string) statelog.Position
}

// CodeDeps are what the code tools read through. The zero value serves none
// of them: a company that hosts no code.
type CodeDeps struct {
	// Code is Gitlet as the estate router answers it. Nil omits every
	// code tool.
	Code CodeReader

	// Actor resolves who is asking, as [WorkDeps.Actor] does, so a seat's
	// turn and an operator's call name their caller one way. Nil takes the
	// turn's seat; the operator surface sets the request's principal.
	Actor func(ctx context.Context, turn *turnctx.Turn) (Actor, error)

	// CloneURL is the address a person's git clones a repository from (on
	// `api.external_url`); nil or empty names none. Only a person's or a
	// token's answer carries it: a seat reads through these tools, and could
	// not authenticate to it.
	CloneURL func(repo string) string

	// Requesters composes whom a seat's turn acts for, and Chart decides them
	// ([CodeDeps.caller]). REQUIRED beside Code, as for the artifacts tools: a
	// surface that left the ceiling out serves no code read rather than every
	// one to whoever asks a seat for it.
	Requesters Requesters
	Chart      authz.Chart

	// Writer is Gitlet's writes (engine-built writes to a repository's home
	// Gitlet node, lifecycle requests to a data node), routed as Code's reads
	// are. Nil omits every code WRITE tool and leaves the reads (codewrite.go).
	Writer CodeWriter

	// Tasks resolves the Worklet item a change write's `work_item` names, by
	// key or id ([CodeDeps.workItemOf]), and code_list_changes' `work` by key
	// ([CodeDeps.listedWorkItem]). Nil resolves none, and such a call is
	// refused naming the `<backend>:<id>` form — never filed or listed under a
	// key nobody resolved.
	Tasks TaskReader

	// Closer is Worklet's writer FOR ONE ACTOR (the tracker's rule), through
	// which code_merge_change's `close_task` closes a merged change's item as
	// the caller who merged it. Set beside Tasks; nil closes none, and the
	// merge says its item was not closed and why.
	Closer func(actor Actor) WorkWriter

	// Mentions resolves the handles a change's text names (an open's or an
	// edit's title and body, a review's summary and findings, a comment's
	// body) to seats at the write ([CodeDeps.mentionsOf]), as THE ASKER'S,
	// which the deciding node holds to its own chart. Nil resolves nothing:
	// the write lands and wakes only those it concerns by role.
	Mentions MentionResolver

	// Holder records a turn's hold on a scratch repository it names
	// ([HoldRepo]), set wherever seats are offered the tools, independently
	// of Writer: a seat that only reads still holds what it reads. Nil records
	// none, so a turn reaching a scratch repository idle long enough to need
	// its hold recorded is refused rather than sent there unordered against
	// an expiry in flight. An operator's call has no turn and holds nothing.
	Holder CodeHolder

	// Logs reads a check's kept log from the object store ([CheckLogReader])
	// for code_get_check_failures. Nil reads no log page: the row's failures,
	// summary and tail are still answered, and a call naming a page is told
	// this node reads none.
	Logs CheckLogReader

	// Searcher is the code search over the fleet's Gitlet nodes, for
	// code_search (codesearch.go). Nil omits code_search; the engine wires it
	// wherever it wires Code.
	Searcher CodeSearcher

	// Placement is where a repository's work is placed — its maintainer and
	// the nodes that may hold a copy — which a purge's answer names
	// ([CodePlacement]). Nil names neither, and a purge's answer says so:
	// its request is recorded all the same.
	Placement CodePlacement

	// Now is the clock a hold's idle threshold is read on, read after the
	// hold is taken; nil is the wall clock.
	Now func() time.Time

	// ops names a write call's operation ([operations]): derived from a seat's
	// turn or a request's key, or held by the operator's assistant over MCP,
	// which only [OperatorTools] sets.
	ops operations
}

// serves reports whether every half the tools read through is wired:
// Gitlet, and the requester ceiling's composition and chart.
func (d CodeDeps) serves() bool { return d.Code != nil && d.Requesters != nil && d.Chart != nil }

// CodeTools are the code tools a seat and an operator share — ONE list, so a
// schema, a default and a refusal's words are written once — or nil where
// deps are not wholly wired ([CodeDeps.serves]). The twelve reads
// ([CodeToolNames]), then code_search where [CodeDeps.Searcher] is wired,
// then the writes ([CodeWrites]) where [CodeDeps.Writer] is.
func CodeTools(deps CodeDeps) []tools.Callable {
	if !deps.serves() {
		return nil
	}
	out := []tools.Callable{
		&codeListRepos{deps: deps}, &codeGetRepo{deps: deps}, &codeResolve{deps: deps},
		&codeRead{deps: deps}, &codeDiff{deps: deps}, &codeCompare{deps: deps},
		&codeLandedIn{deps: deps}, &codeLog{deps: deps}, &codeBlame{deps: deps},
		&codeListChanges{deps: deps}, &codeGetChange{deps: deps}, &codeGetCheckFailures{deps: deps},
	}
	if deps.Searcher != nil {
		out = append(out, &codeSearch{deps: deps})
	}
	if deps.Writer != nil {
		out = append(out, codeWriteTools(deps)...)
	}
	return out
}

// codeCandidates are [CodeTools] as a registry's candidates: all of them, or
// none where the company hosts no code.
func codeCandidates(deps CodeDeps) []candidate {
	var out []candidate
	for _, tool := range CodeTools(deps) {
		out = append(out, candidate{tool, true})
	}
	return out
}

// codeAnnotations classifies one code tool for the delivery gate and the
// sub-agent guard; false for a name that is not one. A READ is read-only and
// idempotent: a turn of reads delivered nothing, a worker may be handed one,
// the catalogue audits none. A WRITE changes a surface every colleague reads,
// so OpenWorld ([mcp.WritesToSharedSurface]): no worker gets one (an
// unbudgeted second writer with its parent's grant); a call delivers, audited.
// IDEMPOTENT, each held to what its caller read (`expected_head`, a branch's
// absence, a free name, the version read), so a retry after a lost answer is
// safe; not a review or a comment, which are additive as a model sees them.
// DESTRUCTIVE only where it takes away somebody's work with nothing to undo it
// by the same tool: a branch or repository deleted, a finding dismissed,
// unreachable objects purged.
func codeAnnotations(name string) (tools.Annotations, bool) {
	for _, tool := range append(CodeToolNames(), CodeSearchTool, CodeRepoReadersTool) {
		if tool == name {
			return tools.Annotations{ReadOnly: mcp.Yes, Idempotent: mcp.Yes}, true
		}
	}
	if slices.Contains(CodeWrites(), name) {
		destructive, idempotent := mcp.No, mcp.Yes
		switch name {
		case CodeDeleteBranchTool, CodeDeleteRepoTool, CodeDeleteCommentTool, CodeResolveFindingTool,
			CodePurgeUnreachableTool:
			destructive = mcp.Yes
		case CodeReviewChangeTool, CodeCommentOnChangeTool:
			idempotent = mcp.No
		}
		return tools.Annotations{ReadOnly: mcp.No, Destructive: destructive, Idempotent: idempotent,
			OpenWorld: mcp.Yes}, true
	}
	return tools.Annotations{}, false
}

// CodeAskerOf is the principal a code read is judged as: a seat by its handle,
// which the serving node's chart relates to the repository; a person as the
// credential they act through, bound to their human seat; and an unbound
// credential, or the engine, as that credential alone.
//
// THE ONE CONVERSION for every surface reading Gitlet on somebody's behalf (a
// seat's turn, an operator's call, the REST reads via [ActorOf]): two would be
// free to judge one person by two grants.
func CodeAskerOf(a Actor) gitlet.Principal {
	switch a.Kind {
	case worklet.AuthorAgent:
		return gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: a.Handle}
	case worklet.AuthorHuman:
		return gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: a.OperatorID, Seat: a.Handle}
	}
	// AN OPERATOR CREDENTIAL, AND THE ENGINE: a credential acting as itself,
	// bound to no seat, which reads what an unbound token reads.
	return gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: a.OperatorID}
}

// ---- who asks, and what a call names ------------------------------------ //

// asker is who this call reads as, or the refusal it is answered with: the
// caller as a write names them ([Actor], carrying its run's call log as
// [WorkDeps.actor] does) and the principal a repository's grant judges.
func (d CodeDeps) asker(ctx context.Context, turn *turnctx.Turn, tool string) (Actor, gitlet.Principal,
	*tools.Result) {

	resolve := func(turn *turnctx.Turn) (Actor, error) { return actorFor(ctx, turn, d.Requesters) }
	if d.Actor != nil {
		resolve = func(turn *turnctx.Turn) (Actor, error) { return d.Actor(ctx, turn) }
	}
	actor, err := resolve(turn)
	if errors.Is(err, ErrUndecidable) {
		// A SEAT'S TURN WHOSE REQUESTER COULD NOT BE COMPOSED, which a write
		// this call makes would carry ([writtenForTurn]): asked again
		// shortly, never answered as a caller with no seat.
		r := unattributed(tool, err)
		return Actor{}, gitlet.Principal{}, &r
	}
	if err != nil {
		r := refusedBy(tools.RefusalForbidden, err, tool+" can only be called during a turn, on "+
			"behalf of a seat, or by a signed-in caller.")
		return Actor{}, gitlet.Principal{}, &r
	}
	// THE RUN'S CALL LOG IS THE TURN'S, whoever resolved the rest — a write's
	// operation counts the run's earlier calls to the tool from it ([opIDFor]).
	actor.Calls = turn.CallLog()
	p := CodeAskerOf(actor)
	if err := p.Validate(); err != nil {
		r := refusedBy(tools.RefusalForbidden, err, fmt.Sprintf("%s could not name who is asking "+
			"in the terms a repository's grant is judged by, so it read nothing.", tool))
		return Actor{}, gitlet.Principal{}, &r
	}
	return actor, p, nil
}

// codeCaller is who one call of a code tool reads as: the caller as a write
// and a hold name them, the asker its grant is judged by, and — in a turn a
// colleague seat woke — that seat, whose own grant bounds the asker's on every
// read ([gitlet.ReadQuery.CeilingSeat]); "" for a call made for nobody else.
// write is a write's ceiling ([CodeDeps.writeCaller]): nil for a write made
// for its seat alone or for a person, else a colleague seat's or nobody's — a
// requester who may change nothing, whose turn the write is still made in for
// the seat's own work. Actor.WrittenFor, composed from the same ceiling, is
// stamped beside it on every request the write forms ([written]).
type codeCaller struct {
	actor   Actor
	asker   gitlet.Principal
	ceiling string
	write   *gitlet.WriteCeiling
}

// caller is who this call reads as, or the refusal it is answered with: the
// asker ([CodeDeps.asker]) and, in a seat's turn somebody woke, that somebody
// ([ceilingOf]), decided AGAIN on the tool's own authority row
// ([authz.DecideWithin]): a stranger or a requester who may not act in the
// company reads nothing, and one the node could not compose or decide is
// answered as a node that could not tell, never as a turn nobody woke. An
// admitted colleague seat rides on every read as the ceiling, judged by the
// answering node ([gitlet.JudgeRead]); an admitted person bounds nothing.
//
// Composed FIRST, before any argument is read: whom a call reads for decides
// whether it reads at all, and composing reads the chart and the identity
// directory, never a log's barrier. A person's or token's call is no deputy.
func (d CodeDeps) caller(ctx context.Context, turn *turnctx.Turn, tool string) (codeCaller, *tools.Result) {
	return d.callerOn(ctx, turn, tool, authz.Action(tool))
}

// callerOn is [CodeDeps.caller] with the requester decided on action rather
// than the tool's own row: for a tool that is no code read but reads
// repositories through the code tools' resolution — code_status
// (healthtools.go) — on Gitlet's verb ([authz.ActionCodeRead]) and NO FURTHER.
// The requester ceiling holds a repository read (ADR-0088); what the tool
// reads beyond repositories is its own row's, already decided for the seat by
// the gate, and deciding it again for the requester would refuse every woken
// turn, since a requester holds no admin. fleet_health likewise decides on a
// row everybody who may act holds ([fleetHealth.forRequester]).
func (d CodeDeps) callerOn(ctx context.Context, turn *turnctx.Turn, tool string,
	action authz.Action) (codeCaller, *tools.Result) {

	actor, asker, refusal := d.asker(ctx, turn, tool)
	if refusal != nil {
		return codeCaller{}, refusal
	}
	c := codeCaller{actor: actor, asker: asker}
	if asker.Kind != gitlet.PrincipalAgent {
		return c, nil
	}
	ceiling, err := ceilingOf(ctx, turn, asker.Seat, d.Requesters)
	if err != nil {
		r := refusedBy(tools.RefusalUnavailable, fmt.Errorf("%w: %w", ErrUndecidable, err),
			fmt.Sprintf("%s could not establish who this turn's work is for, which every code read is "+
				"decided for as well as for you. Nothing was read. Try again shortly.", tool))
		return codeCaller{}, &r
	}
	if ceiling.Kind == authz.CeilingNone {
		return c, nil
	}
	dec := authz.DecideWithin(ctx, turnctx.Principal(turn), ceiling, action, authz.Object{}, d.Chart,
		time.Now().UTC())
	switch {
	case dec.Unknown():
		r := refusedBy(tools.RefusalUnavailable, fmt.Errorf("%w: %w", ErrUndecidable, dec.Err),
			fmt.Sprintf("%s could not decide right now whether whoever this turn is working for may "+
				"read the company's code. Nothing was read. Try again shortly.", tool))
		return codeCaller{}, &r
	case !dec.Allowed && dec.Reason == authz.ReasonCeiling:
		r := authorityRefusal(tool, DecisionError(action, dec))
		r.Output = ceilingRefused(tool, ceiling)
		return codeCaller{}, &r
	case !dec.Allowed:
		r := authorityRefusal(tool, DecisionError(action, dec))
		return codeCaller{}, &r
	}
	if ceiling.Kind == authz.CeilingRequester && ceiling.Principal.Kind == iam.KindSeat {
		c.ceiling = ceiling.Principal.Seat
	}
	return c, nil
}

// ceilingRefused answers a call refused for whoever its turn acts for, by who
// that is: a stranger, a requester who may not act now, or one without Gitlet
// (ADR-0110) — the three a code read is refused for, since it asks the app and
// none of the requester's relations. A credential acting as no seat is named
// as one, as [DecisionRefused] names it: a seat told "the person" would look
// for somebody who never asked.
func ceilingRefused(tool string, c authz.Ceiling) string {
	switch {
	case c.Kind == authz.CeilingStranger:
		return fmt.Sprintf("%s didn't run: this turn was woken by somebody the company does not know, and "+
			"a repository is read for somebody only where they could read it themselves, so you may "+
			"read none for them. A person or a seat of the company has to ask for it.", tool)
	case !c.Principal.Stage.MayAct():
		return fmt.Sprintf("%s didn't run: whoever this turn is working for may not act in the company now, "+
			"so they could not read its code themselves, and you may read none of it for them.", tool)
	case c.Principal.Kind == iam.KindMachine:
		return fmt.Sprintf("%s didn't run: the credential this turn is working for does not use Gitlet, so it "+
			"could not read the company's code itself, and you may read none of it for it. Answer without it; "+
			"an administrator can give the credential Gitlet.", tool)
	}
	return fmt.Sprintf("%s didn't run: the person this turn is working for does not use Gitlet, so they "+
		"could not read the company's code themselves, and you may read none of it for them. Answer without "+
		"it, or tell them an administrator can give them Gitlet.", tool)
}

// ceilingCause is the cause of a read Gitlet refused for the seat a
// call reads for ([gitlet.ErrAboveCeiling]): the authority table's own ceiling
// refusal over the domain's error, so a surface reading the decision finds the
// one reason every requester-ceiling refusal carries, and one reading the
// domain finds its sentinel.
func ceilingCause(tool string, err error) error {
	refused := &DecisionRefused{Action: authz.Action(tool), Decision: authz.Decision{Reason: authz.ReasonCeiling}}
	return fmt.Errorf("%w: %w", refused, err)
}

// read is q asked of Gitlet as c: THE ONE ReadQuery a call sends, so
// none can name its asker and forget whom it reads for.
func (d CodeDeps) read(ctx context.Context, c codeCaller, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	q.Asker, q.CeilingSeat = c.asker, c.ceiling
	return d.Code.Read(ctx, q)
}

// codeRepoKey is a repository a turn resolved, as the turn's lookups key it:
// who asked, and the name they asked by.
type codeRepoKey struct {
	asker gitlet.Principal
	name  string
}

// repoArg is the repository a call names, by its name, or the refusal of an
// argument that names none — refused before anything is read.
func repoArg(tool string, args map[string]any) (string, *tools.Result) {
	name := strings.TrimSpace(argString(args, "repo"))
	if name == "" {
		r := failed(fmt.Sprintf("%s needs a `repo`: a repository's name, as %s lists it.", tool,
			CodeListReposTool))
		return "", &r
	}
	if !gitlet.ValidName(name) {
		r := failed(fmt.Sprintf("%s: %q is not a repository's name — a name is lowercase letters, "+
			"digits and hyphens, as %s lists it.", tool, clip(name), CodeListReposTool))
		return "", &r
	}
	return name, nil
}

// repo is the repository named, resolved ONCE A TURN: the first call reads it
// (kind `repo`, at the seat's level, where a repository the asker may not
// read is refused before any Gitlet node is asked), and later calls reuse it
// from the turn's lookups — its id above all, which reads by object id name.
// The operator surface has no turn and resolves once a call. See
// [CodeDeps.repoRead].
func (d CodeDeps) repo(ctx context.Context, turn *turnctx.Turn, tool string, c codeCaller,
	name string) (gitlet.RepoRef, *tools.Result) {

	ref, _, refusal := d.repoRead(ctx, turn, tool, c, name, nil)
	return ref, refusal
}

// repoRead is [CodeDeps.repo] whose read, on a turn's first call naming the
// repository, may be the call's OWN: first, when not nil, is a read whose
// answer carries the repository (code_get_repo's first refs page, or one with
// [gitlet.ReadQuery.Resolve]), sent in place of the `repo` read with asker,
// repository and level set here: one barrier, not two. A hit answers none.
//
// THE ONE RESOLUTION OF A `repo` ARGUMENT. Its memo decides no access (every
// read is judged again) and is forgotten once untrue ([CodeDeps.sameRepo],
// [CodeDeps.failed]). A MISS takes the turn's hold ([HoldRepo]: no scratch
// repository expires under an unended turn, M3-decisions M3-O61) after the
// read, on the id it resolved, and before the memo keeps it, so a reused
// resolution has its hold; a failed hold refuses the call and keeps nothing.
func (d CodeDeps) repoRead(ctx context.Context, turn *turnctx.Turn, tool string, c codeCaller,
	name string, first *gitlet.ReadQuery) (gitlet.RepoRef, *gitlet.ReadAnswer, *tools.Result) {

	if held := lookedUpRepo(turn, c.asker, name); held != nil {
		return *held, nil, nil
	}
	q := gitlet.ReadQuery{Kind: gitlet.ReadRepo}
	if first != nil {
		q = *first
	}
	q.Repo, q.Fresh = name, seatRead
	answer, err := d.read(ctx, c, q)
	if err != nil {
		r := codeFailure(ctx, tool, name, err)
		return gitlet.RepoRef{}, nil, &r
	}
	var ref gitlet.RepoRef
	switch {
	case answer.Repo != nil:
		ref = answer.Repo.RepoRef
	case answer.Resolved != nil:
		ref = *answer.Resolved
	default:
		r := faulted(ctx, tool, fmt.Errorf("builtin: Gitlet answered repository %s's %s read with "+
			"neither its detail nor its resolution", name, q.Kind), fmt.Sprintf("%s could not read repository "+
			"%q: %s.", tool, name, faultSaid))
		return gitlet.RepoRef{}, nil, &r
	}
	if err := HoldRepo(ctx, turn, d.Holder, c.actor, ref, d.Now); err != nil {
		r := holdFailure(ctx, tool, ref, err)
		return gitlet.RepoRef{}, nil, &r
	}
	turn.LookedUp().Store(codeRepoKey{asker: c.asker, name: name}, ref)
	if first == nil {
		return ref, nil, nil
	}
	return ref, &answer, nil
}

// sameRepo refuses an answer read by NAME that names another repository than
// the one the turn resolved the name to — a name released and taken again
// between the two reads — and forgets the turn's resolution, so the call made
// again resolves the name afresh. nil where they are one repository.
func (d CodeDeps) sameRepo(turn *turnctx.Turn, tool string, asker gitlet.Principal, repo,
	answered gitlet.RepoRef) *tools.Result {

	if answered.ID == "" || answered.ID == repo.ID {
		return nil
	}
	forget(turn, asker, repo)
	r := refused(tools.RefusalStaleVersion, fmt.Sprintf("%s: the name %q names another repository "+
		"than it did when this turn first read it — it was deleted and declared again — so nothing "+
		"was read. Make the same call again: it reads the repository the name names now.", tool,
		repo.Name))
	return &r
}

// failed explains a read about repo, the repository the turn resolved, that
// did not answer ([codeFailure]), first forgetting the turn's resolution of
// its name where the read found the repository GONE (gitlet.ErrRepoNotFound):
// no read by name re-checks a read by id, and a name purged and declared again
// names another id, which the next call resolves afresh.
func (d CodeDeps) failed(ctx context.Context, turn *turnctx.Turn, tool string, asker gitlet.Principal,
	repo gitlet.RepoRef, err error) tools.Result {

	if errors.Is(err, gitlet.ErrRepoNotFound) {
		forget(turn, asker, repo)
	}
	return codeFailure(ctx, tool, repo.Name, err)
}

// lookedUpRepo is the turn's resolution of name as asker resolved it — one
// [CodeDeps.repoRead] kept, its hold taken — or nil where the turn holds none
// (or there is no turn).
func lookedUpRepo(turn *turnctx.Turn, asker gitlet.Principal, name string) *gitlet.RepoRef {
	held, ok := turn.LookedUp().Load(codeRepoKey{asker: asker, name: name})
	if !ok {
		return nil
	}
	ref, ok := held.(gitlet.RepoRef)
	if !ok {
		return nil
	}
	return &ref
}

// forget drops the turn's resolution of repo's name, as asker asked it.
func forget(turn *turnctx.Turn, asker gitlet.Principal, repo gitlet.RepoRef) {
	turn.LookedUp().Forget(codeRepoKey{asker: asker, name: repo.Name})
}

// resolveRef resolves the names a call gives as refs of repo in ONE
// code.resolve_ref at the seat's level: one barrier and one round trip however
// many it names, all read at one checkpoint, so a comparison compares them as
// they stood together. It answers each name's row in order (its oid, whole
// name and where it last moved, which a read by object id is floored at) and
// the checkpoint; a name no live ref holds refuses the call in Gitlet's
// sentence, every such name named. Its read, [CodeDeps.refsOf], is THE ONE
// gitlet.RefQuery in this package, so the one place a resolve is stamped with
// its caller and ceiling and the turn's holds are taken ([HoldRef]).
func (d CodeDeps) resolveRef(ctx context.Context, turn *turnctx.Turn, tool string, c codeCaller,
	repo gitlet.RepoRef, names ...string) ([]gitlet.ResolvedRef, statelog.Position, *tools.Result) {

	answer, refusal := d.refsOf(ctx, turn, tool, c, repo, names...)
	if refusal != nil {
		return nil, statelog.Position{}, refusal
	}
	if err := answer.Err(); err != nil {
		r := codeFailure(ctx, tool, repo.Name, err)
		return nil, statelog.Position{}, &r
	}
	return answer.Refs, answer.Position, nil
}

// refsOf is [CodeDeps.resolveRef]'s read, ANSWERING a name no live ref holds
// ([gitlet.ResolvedRef.Missing]) rather than refusing: for an executor's
// launch, whose branch may not exist yet ([CodeDeps.ResolveRunRepos]), and a
// write's branch, which its own head compare answers ([CodeDeps.branch]). An
// answer naming another repository is refused ([CodeDeps.sameRepo]).
//
// It takes the turn's hold on every seat's branch it resolved ([HoldRef])
// after the read and before the answer goes anywhere, so no caller can read a
// ref and forget its hold: every key in ONE Take first ([takeRefHolds]), one
// compare-and-set on the run row. A failed hold refuses with nothing sent.
func (d CodeDeps) refsOf(ctx context.Context, turn *turnctx.Turn, tool string, c codeCaller,
	repo gitlet.RepoRef, names ...string) (gitlet.RefAnswer, *tools.Result) {

	answer, err := d.Code.ResolveRef(ctx, gitlet.RefQuery{Repo: repo.Name, Refs: names, Fresh: seatRead,
		Asker: c.asker, CeilingSeat: c.ceiling})
	if err != nil {
		r := d.failed(ctx, turn, tool, c.asker, repo, err)
		return gitlet.RefAnswer{}, &r
	}
	if r := d.sameRepo(turn, tool, c.asker, repo, answer.Repo); r != nil {
		return gitlet.RefAnswer{}, r
	}
	if len(answer.Refs) != len(names) {
		r := faulted(ctx, tool, fmt.Errorf("builtin: Gitlet answered %d of the %d refs asked of %s",
			len(answer.Refs), len(names), repo.Name), fmt.Sprintf("%s could not read repository %q: %s.", tool,
			repo.Name, faultSaid))
		return gitlet.RefAnswer{}, &r
	}
	noteRefsRead(turn, repo.ID, answer.Refs)
	var held []string
	for _, resolved := range answer.Refs {
		if resolved.Missing == "" {
			held = append(held, resolved.Ref)
		}
	}
	if taking, err := takeRefHolds(ctx, turn, c.actor, repo, held); err != nil {
		r := refHoldFailure(ctx, tool, repo, taking, err)
		return gitlet.RefAnswer{}, &r
	}
	for i := range answer.Refs {
		resolved := &answer.Refs[i]
		if resolved.Missing != "" {
			continue
		}
		if err := HoldRef(ctx, turn, d.Holder, c.actor, repo, resolved.Ref, resolved, answer.Position,
			d.Now); err != nil {
			r := refHoldFailure(ctx, tool, repo, resolved.Ref, err)
			return gitlet.RefAnswer{}, &r
		}
	}
	return answer, nil
}

// branch holds the refs a write names — its `branch`, a suggestions apply's
// source branch, a change open's source and target — for turn BEFORE the
// write is sent ([HoldRef]): THE ONE PLACE a write's branch is held, all at
// once (one ref read, one Take, [takeRefHolds]). Held as a read is, because
// only a write that MOVES the branch is ordered against its deletion by its
// head compare; a refused, conflicting or no-op write appends nothing. So an
// existing branch (expected_head given) resolves through [CodeDeps.refsOf],
// held with the answer; one missing or being created is held with no read,
// since no deletion listed it. Only where a hold is due and unconfirmed
// (M3-decisions M3-O65): a seat's call in a turn on a [gitlet.Holdable] branch
// the turn has not confirmed; any other write reads nothing here.
func (d CodeDeps) branch(ctx context.Context, turn *turnctx.Turn, tool string, c codeCaller, repo gitlet.RepoRef,
	existing bool, refs ...string) *tools.Result {

	if turn == nil || c.asker.Kind != gitlet.PrincipalAgent {
		return nil
	}
	var held []string
	for _, ref := range refs {
		if gitlet.Holdable(ref) && !slices.Contains(held, ref) &&
			!turn.Holding().Confirmed(gitlet.RefHoldKey(repo.ID, ref)) {
			held = append(held, ref)
		}
	}
	if len(held) == 0 {
		return nil
	}
	unread := held
	if existing {
		answer, refusal := d.refsOf(ctx, turn, tool, c, repo, held...)
		if refusal != nil {
			return refusal
		}
		unread = nil
		for i, resolved := range answer.Refs {
			if resolved.Missing != "" {
				unread = append(unread, held[i])
			}
		}
	}
	if taking, err := takeRefHolds(ctx, turn, c.actor, repo, unread); err != nil {
		r := refHoldFailure(ctx, tool, repo, taking, err)
		return &r
	}
	for _, ref := range unread {
		if err := HoldRef(ctx, turn, d.Holder, c.actor, repo, ref, nil, statelog.Position{}, d.Now); err != nil {
			r := refHoldFailure(ctx, tool, repo, ref, err)
			return &r
		}
	}
	return nil
}

// refsReadKey keys, in a turn's lookups, how far the refs the turn resolved on
// one repository had moved ([refsRead]).
type refsReadKey struct{ repoID string }

// refsRead is the furthest code-log position at which a ref this turn resolved
// on one repository last MOVED, which a seat's engine write of the repository
// is floored at ([CodeDeps.writeFresh]): the write names a head and commits
// the turn read, and judged on a copy lacking them it is judged again at the
// log's head — a barrier and a second sync. Kept in the turn's lookups and
// only ever raised, under its own lock, since the turn's workers resolve refs
// concurrently. A FACT about what was read, never a resolution: it decides
// nothing, and outlives a name the turn forgets ([forget]).
type refsRead struct {
	mu sync.Mutex
	at statelog.Position
}

// noteRefsRead raises turn's [refsRead] on repoID to where each of refs last
// moved; a ref no live ref holds, which moved nowhere, raises nothing.
func noteRefsRead(turn *turnctx.Turn, repoID string, refs []gitlet.ResolvedRef) {
	if turn == nil || turn.LookedUp() == nil {
		return
	}
	held, _ := turn.LookedUp().LoadOrStore(refsReadKey{repoID: repoID}, &refsRead{})
	read, ok := held.(*refsRead)
	if !ok {
		return
	}
	read.mu.Lock()
	defer read.mu.Unlock()
	for _, ref := range refs {
		read.at = furthestOnLog(read.at, ref.Moved)
	}
}

// refsReadOn is turn's [refsRead] on repoID: the zero position where the turn
// has resolved no ref of it, or there is no turn.
func refsReadOn(turn *turnctx.Turn, repoID string) statelog.Position {
	held, ok := turn.LookedUp().Load(refsReadKey{repoID: repoID})
	if !ok {
		return statelog.Position{}
	}
	read, ok := held.(*refsRead)
	if !ok {
		return statelog.Position{}
	}
	read.mu.Lock()
	defer read.mu.Unlock()
	return read.at
}

// headRev is the revision a call names its repository's default branch by.
const headRev = "HEAD"

// resolvedRev is a revision argument as a read by object id names it: the
// object id — whole, or an abbreviation the Gitlet node expands — and, where the
// argument was a ref, the ref it resolved through and the checkpoint that
// read was at. A read of it is floored at the record that last moved its ref
// (gitlet.RefsFloor), and an id's at nothing of its own.
type resolvedRev struct {
	oid  string
	ref  *gitlet.ResolvedRef
	read statelog.Position
}

// revArg is the revision a call names in arg, or the refusal of an argument
// that names none — refused before anything is read.
func revArg(tool, arg string, args map[string]any) (string, *tools.Result) {
	rev := strings.TrimSpace(argString(args, arg))
	if rev == "" {
		r := failed(fmt.Sprintf("%s needs `%s`: a commit id — whole, or its first 7 or more hex digits — "+
			"or a branch, a tag, a whole ref, or %s for the default branch.", tool, arg, headRev))
		return "", &r
	}
	return rev, nil
}

// resolveRevs is a call's revision arguments as oids, in order: an object id,
// whole or abbreviated, passes unread (the Gitlet node expands it from the
// objects, never as a ref); `HEAD` is the default branch; every other is
// resolved, all in [CodeDeps.resolveRef]'s one read, each name once. A branch
// whose name is all hex digits is named by its whole ref.
func (d CodeDeps) resolveRevs(ctx context.Context, turn *turnctx.Turn, tool string, c codeCaller,
	repo gitlet.RepoRef, revs ...string) ([]resolvedRev, *tools.Result) {

	out := make([]resolvedRev, len(revs))
	refOf := make([]string, len(revs))
	var names []string
	for i, rev := range revs {
		switch {
		case gitlet.ValidRev(rev):
			out[i] = resolvedRev{oid: rev}
			continue
		case rev == headRev:
			rev = repo.DefaultBranch
		}
		refOf[i] = rev
		if !slices.Contains(names, rev) {
			names = append(names, rev)
		}
	}
	if len(names) == 0 {
		return out, nil
	}
	refs, read, refusal := d.resolveRef(ctx, turn, tool, c, repo, names...)
	if refusal != nil {
		return nil, refusal
	}
	for i, name := range refOf {
		if name == "" {
			continue
		}
		ref := &refs[slices.Index(names, name)]
		out[i] = resolvedRev{oid: ref.OID, ref: ref, read: read}
	}
	return out, nil
}

// revPairArg is a comparison's two revisions, `base` and `head`, read with
// the rest of a call's arguments, before anything is.
func revPairArg(tool string, args map[string]any) (base, head string, refusal *tools.Result) {
	if base, refusal = revArg(tool, "base", args); refusal != nil {
		return "", "", refusal
	}
	if head, refusal = revArg(tool, "head", args); refusal != nil {
		return "", "", refusal
	}
	return base, head, nil
}

// gitFresh is the level a read by object id of repo is asked at: a SYNC to the
// furthest of this node's floor on it and where the call's refs last moved, so
// the answering copy holds those commits and this node's pushes — or none,
// reading the copy as it stands (gitlet.RefsFloor, the code domain's one rule).
func (d CodeDeps) gitFresh(repo gitlet.RepoRef, revs ...resolvedRev) statelog.Freshness {
	var refs []gitlet.ResolvedRef
	for _, r := range revs {
		if r.ref != nil {
			refs = append(refs, *r.ref)
		}
	}
	return gitlet.RefsFloor(d.Code.FloorOf(repo.ID), refs...)
}

// furthestOnLog is the later of two positions on the code log, a zero one
// standing for none.
func furthestOnLog(a, b statelog.Position) statelog.Position {
	switch {
	case a.IsZero():
		return b
	case b.IsZero():
		return a
	}
	if before, err := a.Before(b); err == nil && before {
		return b
	}
	return a
}

// gitRead asks q of repo's home Gitlet node as c — THE ONE GitQuery a call
// sends, stamped here with its caller, ceiling and a seat's turn so no tool
// forgets whom it reads for — refused first where no Gitlet node would take
// it, so a malformed argument costs no round trip. A repository answered gone
// ends the turn's resolution of its name ([CodeDeps.failed]).
func (d CodeDeps) gitRead(ctx context.Context, turn *turnctx.Turn, tool string, c codeCaller, repo gitlet.RepoRef,
	q gitlet.GitQuery) (gitlet.GitAnswer, *tools.Result) {

	q.Asker, q.CeilingSeat = c.asker, c.ceiling
	// A SEAT'S READ NAMES ITS TURN, whose bucket on the answering Gitlet node
	// pays the read's second look at the log's head (gitlet.NewResyncs): a
	// turn looping on what its floor lacks spends its own re-syncs, then its
	// seat's, never the code log's.
	if turn != nil && c.asker.Kind == gitlet.PrincipalAgent {
		q.Turn = turn.RunID
	}
	if err := q.Validate(); err != nil {
		r := codeFailure(ctx, tool, repo.Name, err)
		return gitlet.GitAnswer{}, &r
	}
	answer, err := d.Code.GitRead(ctx, q)
	if err != nil {
		r := d.failed(ctx, turn, tool, c.asker, repo, err)
		return gitlet.GitAnswer{}, &r
	}
	return answer, nil
}

// codeResolved is a ref a call's revision named, as an answer says it was
// resolved: its whole name, the commit it pointed at, and where the read saw
// it.
type codeResolved struct {
	Ref      string            `json:"ref"`
	OID      string            `json:"oid"`
	Position statelog.Position `json:"position"`
}

// codeGitAnswer is a read by object id as a code tool answers it: the Gitlet
// node's page whole — the object it read, the copy's position, the cursor of
// the next page, the push behind each commit it names — beside the repository
// by name and every ref the call named, by the argument that named it.
type codeGitAnswer struct {
	Repo     string                  `json:"repo"`
	Resolved map[string]codeResolved `json:"resolved,omitempty"`
	gitlet.GitAnswer
}

// gitAnswer is the answer to a read by object id over repo, the refs resolved
// by argument name.
func gitAnswer(repo gitlet.RepoRef, page gitlet.GitAnswer, revs map[string]resolvedRev) codeGitAnswer {
	out := codeGitAnswer{Repo: repo.Name, GitAnswer: page}
	for arg, r := range revs {
		if r.ref == nil {
			continue
		}
		if out.Resolved == nil {
			out.Resolved = map[string]codeResolved{}
		}
		out.Resolved[arg] = codeResolved{Ref: r.ref.Ref, OID: r.ref.OID, Position: r.read}
	}
	return out
}

// codeFailure explains a code read that did not answer: each domain sentinel
// in its own class and sentence, naming what to change; past them a node or
// fleet condition as unavailable, and anything else as this node's fault.
//
// A NODE THAT CANNOT JUDGE IS NEVER A REFUSAL OF THE CALLER: on
// gitlet.ErrNotServing or gitlet.ErrGitUnready the router asks another, so
// what reaches here is no node able to answer now, which a retry clears. Only
// gitlet.ErrNotPermitted judges who asked, and gitlet.ErrAboveCeiling whom the
// asker reads for.
func codeFailure(ctx context.Context, tool, repo string, err error) tools.Result {
	var (
		unready  *gitlet.ErrGitUnready
		unserved *estate.ErrUnserved
	)
	switch {
	case errors.Is(err, gitlet.ErrAboveCeiling):
		// THE CEILING'S OWN SENTENCE names the seat this turn works for and
		// who does read the repository. The asker's own access is no reason to
		// read it: for that seat it would set the visibility aside.
		said, ok := RefusalSentence(err, gitlet.ErrAboveCeiling)
		if !ok {
			said = fmt.Sprintf("the seat this turn is working for may not read repository %q", repo)
		}
		return refusedBy(tools.RefusalForbidden, ceilingCause(tool, err), fmt.Sprintf("%s didn't run: the "+
			"seat this turn is working for could not read this itself — %s — so you may not read it for them. "+
			"That is a decision about whom you are reading for, not about the repository or the node, so the "+
			"same call is refused again in this turn: tell them who does read it.", tool, said))
	case errors.Is(err, gitlet.ErrNotPermitted):
		// THE GRANT'S OWN SENTENCE names the visibility and who does read the
		// repository; nothing suggests a way round, since another seat reading
		// it for this one would set the visibility aside.
		said, ok := RefusalSentence(err, gitlet.ErrNotPermitted)
		if !ok {
			said = fmt.Sprintf("your grant does not let you read repository %q", repo)
		}
		return refusedBy(tools.RefusalForbidden, err, fmt.Sprintf("%s: %s. That is a decision about "+
			"who is asking, not about the repository or the node, so the same call is refused again.",
			tool, said))
	case errors.Is(err, gitlet.ErrRepoNotFound):
		said, ok := RefusalSentence(err, gitlet.ErrRepoNotFound)
		if !ok || said == repo {
			said = fmt.Sprintf("there is no repository %q", repo)
		}
		return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("%s: %s. List the repositories you "+
			"can read with %s.", tool, said, CodeListReposTool))
	case errors.Is(err, gitlet.ErrRefNotFound):
		said, _ := RefusalSentence(err, gitlet.ErrRefNotFound)
		return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("%s: %s. Its branches are listed by "+
			"%s; a commit is named by its id.", tool, said, CodeGetRepoTool))
	case errors.Is(err, gitlet.ErrChangeNotFound):
		// THE CHANGE IT NAMES, of a repository the asker reads: a number of
		// one it may not read is never this answer (gitlet.ErrNotPermitted).
		said, ok := RefusalSentence(err, gitlet.ErrChangeNotFound)
		if !ok {
			said = "of that number"
		}
		return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("%s: repository %q has no change %s. Its "+
			"changes are listed by %s.", tool, repo, said, CodeListChangesTool))
	case errors.Is(err, gitlet.ErrObjectNotFound), errors.Is(err, gitlet.ErrNotATree),
		errors.Is(err, gitlet.ErrNotABlob):
		return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("%s: %s.", tool, domainWords(err)))
	case errors.Is(err, gitlet.ErrSliceTooLarge), errors.Is(err, gitlet.ErrInvalid),
		errors.Is(err, gitlet.ErrBadCursor), errors.Is(err, gitlet.ErrBadLevel):
		return refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s refused what it was asked: %s. "+
			"Change the arguments — the same ones are refused again.", tool, domainWords(err)))
	case errors.As(err, new(*estate.ErrUnknownKeys)):
		// THE FLEET RUNS TWO BUILDS, which no retry clears: matched AHEAD of
		// the unserved walk it wraps, which would tell a passing outage.
		why, _ := Condition(err)
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s could not read repository %q "+
			"(%s). This is NOT an answer about the repository. %s", tool, repo, why, stuckRead))
	case errors.As(err, &unready), errors.Is(err, gitlet.ErrNotServing):
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s could not read repository %q "+
			"right now (no node holding it could answer in time). This is NOT an answer about the "+
			"repository: try again shortly.", tool, repo))
	case errors.As(err, &unserved) && unserved.Git:
		// THE GITLET NODES' WALK, which Condition's words — about the data
		// nodes, which were serving — would misname.
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s could not read repository %q "+
			"right now (none of its Gitlet nodes could answer in time — its home may still be filling its "+
			"copy). This is NOT an answer about the repository: try again shortly.", tool, repo))
	}
	if why, ok := Condition(err); ok {
		// THE CONDITION'S OWN ADVICE: a model told to retry one no wait
		// clears retries it all turn ([conditionNext]).
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s could not read repository %q%s "+
			"(%s). This is NOT an answer about the repository%s", tool, repo, rightNow(err), why,
			conditionNext(err, ": try again shortly.", ". "+stuckRead)))
	}
	return faulted(ctx, tool, err, fmt.Sprintf("%s could not read repository %q: %s. This is NOT an "+
		"answer about the repository: say you could not read it.", tool, repo, faultSaid))
}

// codeClass is the class of a refusal Gitlet decided, by the rule every native
// domain's refusals are classed by ([tools.Refusal]); [WriterClass] and
// [codeFailure] follow it. NOT_FOUND: what the call is about is not there.
// CONFLICT: the repository refuses by its state (purging, in deleted
// retention, not deleted, config-managed); it lands once that changes.
// STALE_VERSION: the history moved under a snapshot; read again. FORBIDDEN: a
// grant, or the seat the call is made for, refuses. INVALID: a field, cursor
// or level refused, a read too large, a decision past a record's budget — the
// same request never lands. UNAVAILABLE: no node can answer now; waiting
// clears it. The rest (a configuration apply's wiring, a misbuilt proposal or
// layer, ErrMisplaced, a stored object unlike its row, a newer build's record)
// are no caller's refusal: the asking node tells them as its own fault.
func codeClass(err error) (tools.Refusal, bool) {
	var unready *gitlet.ErrGitUnready
	switch {
	case errors.Is(err, gitlet.ErrRepoNotFound), errors.Is(err, gitlet.ErrRefNotFound),
		errors.Is(err, gitlet.ErrChangeNotFound), errors.Is(err, gitlet.ErrCheckNotFound),
		errors.Is(err, gitlet.ErrObjectNotFound), errors.Is(err, gitlet.ErrNotATree),
		errors.Is(err, gitlet.ErrNotABlob):
		return tools.RefusalNotFound, true
	case errors.Is(err, gitlet.ErrPurging), errors.Is(err, gitlet.ErrNotYet), errors.Is(err, gitlet.ErrNotDeleted),
		errors.Is(err, gitlet.ErrConfigManaged):
		return tools.RefusalConflict, true
	case errors.Is(err, gitlet.ErrSnapshotMoved):
		return tools.RefusalStaleVersion, true
	case errors.Is(err, gitlet.ErrNotPermitted), errors.Is(err, gitlet.ErrAboveCeiling):
		return tools.RefusalForbidden, true
	case errors.Is(err, gitlet.ErrInvalid), errors.Is(err, gitlet.ErrBadCursor), errors.Is(err, gitlet.ErrBadLevel),
		errors.Is(err, gitlet.ErrSliceTooLarge), errors.Is(err, gitlet.ErrRecordCap):
		return tools.RefusalInvalid, true
	case errors.Is(err, gitlet.ErrNotServing), errors.Is(err, gitlet.ErrAnswerDue), errors.As(err, &unready):
		return tools.RefusalUnavailable, true
	}
	return "", false
}

// domainWords is a code refusal's own sentence: the words its domain wrote for
// the caller past the sentinel, whichever sentinel it carries.
func domainWords(err error) string {
	for _, sentinel := range []error{gitlet.ErrObjectNotFound, gitlet.ErrNotATree, gitlet.ErrNotABlob,
		gitlet.ErrSliceTooLarge, gitlet.ErrInvalid, gitlet.ErrBadCursor, gitlet.ErrBadLevel} {
		if said, ok := RefusalSentence(err, sentinel); ok {
			return said
		}
	}
	return err.Error()
}

// RefusalSentence is the sentence err carries past its sentinel's own words
// (`gitlet: the caller's grant does not cover this: <sentence>`), wherever in
// the chain the sentinel was wrapped, since the estate router wraps another
// node's refusal too; false where it carries none. Exported for Gitlet's REST
// reads, so a person's surface and a seat's tool quote the same words.
func RefusalSentence(err, sentinel error) (string, bool) {
	_, said, ok := strings.Cut(err.Error(), sentinel.Error()+": ")
	said = strings.TrimSpace(said)
	return said, ok && said != ""
}

// codeRepoArg and codeCursorArg are the arguments every code tool's schema
// shares, each a fresh map per schema, so a consumer writing into one tool's
// schema writes into no other's.
func codeRepoArg() map[string]any {
	return map[string]any{
		"type":        "string",
		"description": "The repository's name, as " + CodeListReposTool + " lists it.",
	}
}

func codeCursorArg() map[string]any {
	return map[string]any{
		"type":        "string",
		"description": "The `next` of the previous page, handed back unchanged; leave it out for the first page.",
	}
}

// codeRevArg describes a revision argument by its role alone, pointing to
// [revisionGrammar] in code_resolve's description: the catalogue rides every
// round of every seat uncached, and the grammar repeated on the six revision
// arguments was a sixth of it.
func codeRevArg(what string) map[string]any {
	return map[string]any{
		"type":        "string",
		"description": what + ": a revision, as " + CodeResolveTool + " describes one.",
	}
}

// revisionGrammar is what a revision argument takes, in the description of
// the one tool every revision argument points to ([codeRevArg]).
const revisionGrammar = "A revision — what the other code tools' `rev`, `base`, `head` and `commit` take — " +
	"is a commit id, whole or its first 7 or more hex digits, which resolves no ref; or a branch, a tag, " +
	"a whole ref, or " + headRev + " for the default branch, resolved to the commit it points at when the " +
	"call reads it. A branch whose name is all hex digits is named by its whole ref (refs/heads/<name>)."

// ---- code_list_repos ---------------------------------------------------- //

type codeListRepos struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeListRepos)(nil)

func (t *codeListRepos) Name() string { return CodeListReposTool }

func (t *codeListRepos) Description() string {
	return "List the company's repositories you can read, by name: each one's owner, visibility, " +
		"default branch and the commit it points at, your access, its last push and its protection " +
		"rules, without their merge gates (" + CodeGetRepoTool + " reads those), a page at a time — " +
		"pass `next` back as `cursor` until no `next` comes back. A " +
		"repository you may not read — or, in a turn a colleague woke, one they may not — is not " +
		"listed, so a page can hold fewer than `limit`, or none, and still name a `next`."
}

func (t *codeListRepos) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status": map[string]any{
				"type": "string",
				"enum": []string{string(gitlet.RepoStatusImporting), string(gitlet.RepoStatusActive),
					string(gitlet.RepoStatusArchived)},
				"description": "Only those being imported, the active, or the archived (read-only); all three " +
					"when left out.",
			},
			"cursor": codeCursorArg(),
			"limit": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf("How many repositories a page lists, 1 to %d; %d by default. "+
					"A page that would outgrow one answer holds fewer, and says where the next begins.",
					maxCodeRepoList, defaultCodeRepoList),
			},
		},
	}
}

func (t *codeListRepos) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

// codeRepoRow is a repository as a code tool names it: what a seat reads it
// by and decides about — never the counts and ids that are Gitlet's own
// bookkeeping.
type codeRepoRow struct {
	Name          string            `json:"name"`
	Description   string            `json:"description,omitempty" untrusted:"name"`
	Status        gitlet.RepoStatus `json:"status"`
	Owner         gitlet.Owner      `json:"owner"`
	Visibility    gitlet.Visibility `json:"visibility"`
	Search        gitlet.SearchMode `json:"search"`
	Access        string            `json:"access"`
	DefaultBranch string            `json:"default_branch"`
	Head          string            `json:"head,omitempty"`
	Pushes        int64             `json:"pushes"`
	LastPushAt    time.Time         `json:"last_push_at,omitzero"`
	// Protections are its rules as a push is held to them, as its row
	// answers them: never a rule's merge gate, which code_get_repo answers.
	Protections []gitlet.PushRule `json:"protections,omitempty"`
	// ProtectionsLeftOut is set, in place of Protections, on the one row of
	// a listing whose rules alone are past an answer: how many there are and
	// which tool pages them.
	ProtectionsLeftOut string `json:"protections_left_out,omitempty"`
}

func repoRowOf(s gitlet.RepoSummary) codeRepoRow {
	return codeRepoRow{
		Name: s.Name, Description: s.Description, Status: s.Status, Owner: s.Owner,
		Visibility: s.Visibility, Search: s.Search, Access: s.Access, DefaultBranch: s.DefaultBranch,
		Head: s.Head, Pushes: s.Pushes, LastPushAt: s.LastPushAt, Protections: s.Protections,
	}
}

// codeRepoList is code_list_repos' answer.
type codeRepoList struct {
	Repos    []codeRepoRow     `json:"repos"`
	Next     string            `json:"next,omitempty"`
	Position statelog.Position `json:"position"`
}

func (t *codeListRepos) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	c, refusal := t.deps.caller(ctx, turn, t.Name())
	if refusal != nil {
		return *refusal, nil
	}
	q := gitlet.ReadQuery{Kind: gitlet.ReadRepos, Cursor: strings.TrimSpace(argString(args, "cursor")),
		Fresh: seatRead}
	switch status := gitlet.RepoStatus(strings.TrimSpace(argString(args, "status"))); status {
	case "":
	case gitlet.RepoStatusImporting, gitlet.RepoStatusActive, gitlet.RepoStatusArchived:
		q.Status = []gitlet.RepoStatus{status}
	default:
		return failed(fmt.Sprintf("%s: `status` is %s, %s or %s, not %q.", t.Name(), gitlet.RepoStatusImporting,
			gitlet.RepoStatusActive, gitlet.RepoStatusArchived, clip(string(status)))), nil
	}
	q.Limit = argInt(args, "limit", defaultCodeRepoList)
	if q.Limit < 1 || q.Limit > maxCodeRepoList {
		return failed(fmt.Sprintf("%s: `limit` is 1 to %d.", t.Name(), maxCodeRepoList)), nil
	}
	answer, err := t.deps.read(ctx, c, q)
	if err != nil {
		return codeFailure(ctx, t.Name(), "", err), nil
	}
	rows := make([]codeRepoRow, len(answer.Repos))
	for i, s := range answer.Repos {
		rows[i] = repoRowOf(s)
	}
	// A PAGE CUT WHOLE-ROW AT THE ANSWER: repositories' rules can make the
	// reader's page outgrow one tool answer. Keyset by name, so the next page
	// begins after the last row kept.
	page := func(k int) any {
		next := answer.Next
		if k > 0 && k < len(rows) {
			next = rows[k-1].Name
		}
		return codeRepoList{Repos: rows[:k], Next: next, Position: answer.Position}
	}
	read := newForeignRead(turn, c.asker.Seat)
	// A REPOSITORY'S DESCRIPTION IS A NAME somebody chose, whom its row does
	// not name: neutralised in place, and the envelope says so (ADR-0040).
	if slices.ContainsFunc(rows, func(r codeRepoRow) bool { return r.Description != "" }) {
		namesBy(read)
	}
	k, rendered, err := read.fit(len(rows), page)
	if err == nil && k == 0 && len(rows) > 0 {
		// A REPOSITORY WHOSE RULES ALONE ARE PAST ONE ANSWER is listed without
		// them, saying so, and the page ends at it: a refusal would end the
		// listing there, and no seat would list the repositories after it.
		rows[0].ProtectionsLeftOut = fmt.Sprintf("its %d protection rules are past one answer beside it: "+
			"%s pages them", len(rows[0].Protections), CodeGetRepoTool)
		rows[0].Protections = nil
		k, rendered, err = read.fit(1, page)
	}
	switch {
	case err != nil:
		return labelFault(ctx, t.Name(), err), nil
	case k == 0 && len(rows) > 0:
		// A ROW WITHOUT ITS RULES is a name, an owner, a description and a
		// default branch at their bounds, about ten kilobytes, so this is a
		// bound broken, never a repository.
		return faulted(ctx, t.Name(), fmt.Errorf("builtin: repository %s's row without its rules is past one "+
			"tool answer", rows[0].Name), fmt.Sprintf("%s could not fit repository %q in one answer: %s.",
			t.Name(), rows[0].Name, faultSaid)), nil
	}
	return compactAnswer(rendered, "Ask for fewer with `limit`.")
}

// ---- code_get_repo ------------------------------------------------------ //

type codeGetRepo struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeGetRepo)(nil)

func (t *codeGetRepo) Name() string { return CodeGetRepoTool }

func (t *codeGetRepo) Description() string {
	return fmt.Sprintf("One repository in detail — its protection rules, each with the merge gate it "+
		"holds a merge to and who last set it and when, its size, how many branches and tags it has, "+
		"your access, its last push and, "+
		"asked by a person or a token, the URL to clone it from (a seat reads a repository through these "+
		"tools) — and a page of its branches with the commit each points at, %d a page: "+
		"`branches_prefix` narrows them to a namespace (`seats/<handle>/`, `release/`), and "+
		"`branches_next` comes back as `branches_cursor` for the next page, which answers the branches "+
		"alone. Rules too many for one answer end at `protections_next`, which comes back as "+
		"`protections_cursor` for the rest of them, alone.", codeBranchesPage)
}

func (t *codeGetRepo) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo": codeRepoArg(),
			"branches_prefix": map[string]any{
				"type": "string",
				"description": "Only the branches under this namespace, a directory of names ending in " +
					"`/` — `seats/<handle>/` for a seat's own, `release/`. Every branch when left out.",
			},
			"branches_cursor": map[string]any{
				"type":        "string",
				"description": "The `branches_next` of the previous page, with the same `branches_prefix`.",
			},
			"protections_cursor": map[string]any{
				"type":        "string",
				"description": "The `protections_next` of the previous answer, never beside `branches_cursor`.",
			},
		},
		"required": []string{"repo"},
	}
}

func (t *codeGetRepo) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

// codeRepoDetail is one repository in detail, as code_get_repo answers it:
// every field of gitlet.RepoDetail a reader acts on, copied by name, so a field
// the detail grows is carried here or named left out with its reason
// (TestCodeGetRepoCarriesEveryDetailFieldOrSaysWhy).
type codeRepoDetail struct {
	codeRepoRow
	ManagedBy gitlet.ManagedBy `json:"managed_by"`
	Refs      int64            `json:"refs"`
	Branches  int64            `json:"branches"`
	Tags      int64            `json:"tags"`
	LivePacks int64            `json:"live_packs"`
	LiveBytes int64            `json:"live_bytes"`
	CreatedAt time.Time        `json:"created_at,omitzero"`
	DeletedAt time.Time        `json:"deleted_at,omitzero"`
	// Expunge is where its expunges stand: the one open, a purge a person
	// asked for and not yet served, and the last one committed — what the
	// purge's own answer sends its asker here to read.
	Expunge gitlet.ExpungeState `json:"expunge,omitzero"`
	// Remotes is where it stands with other hosts: its mirrors as its
	// settings name them, the import its create filled it from and how that
	// ended — its error, where it ended short, the engine's own sentence —
	// and a pull mirror's last sync. Every credential in it is a ${VAR}
	// reference, never a value.
	Remotes gitlet.RemoteState `json:"remotes,omitzero"`
}

// codeBranch is one branch: its name under refs/heads/, and its commit.
type codeBranch struct {
	Name string `json:"name"`
	OID  string `json:"oid"`
}

// codeRepoAnswer is code_get_repo's answer.
type codeRepoAnswer struct {
	Repo     string          `json:"repo"`
	Detail   *codeRepoDetail `json:"detail,omitempty"`
	CloneURL string          `json:"clone_url,omitempty"`
	// Protections are its rules WHOLE ([codeRule], read with
	// gitlet.ReadQuery.Gates), on the detail's page alone, in pattern order:
	// those that fit, ProtectionsNext the last answered where more follow
	// ([codeRepoRules]).
	Protections     []codeRule        `json:"protections,omitempty"`
	ProtectionsNext string            `json:"protections_next,omitempty"`
	Branches        []codeBranch      `json:"branches"`
	BranchesNext    string            `json:"branches_next,omitempty"`
	Position        statelog.Position `json:"position"`
}

// codeRepoRules is code_get_repo's answer to a `protections_cursor`: the
// repository's rules after it, whole and in pattern order, those that fit one
// answer, and ProtectionsNext where more follow.
type codeRepoRules struct {
	Repo            string            `json:"repo"`
	Protections     []codeRule        `json:"protections"`
	ProtectionsNext string            `json:"protections_next,omitempty"`
	Position        statelog.Position `json:"position"`
}

// codeRule is one of a repository's rules WHOLE: what a push is held to, the
// merge gate it holds a merge to, and who last set it and when
// ([gitlet.RuleSource]). The source tells a seat where a rule is changed: one
// the company document declares (SetByKind config, naming nobody) is the
// document's to change, and one a tool set names the seat, the person's seat
// or the credential that set it, which the change's conversation can reach.
type codeRule struct {
	gitlet.Protection
	SetBy     string           `json:"set_by,omitempty"`
	SetByKind gitlet.ActorKind `json:"set_by_kind,omitempty"`
	SetAt     time.Time        `json:"set_at,omitzero"`
}

// wholeRules is a repository's rules, each given the merge gate and source its
// detail answered for its pattern, read beside the rules in one transaction
// ([gitlet.RepoDetail.Gates], [gitlet.RepoDetail.RuleSources]). Only for a
// detail read that asked for the gates: otherwise every rule would read as
// ungated and set by nobody.
func wholeRules(rules []gitlet.PushRule, gates []gitlet.RuleGate, sources []gitlet.RuleSource) []codeRule {
	gateOf := make(map[string]gitlet.MergeGate, len(gates))
	for _, g := range gates {
		gateOf[g.Pattern] = g.Gate
	}
	sourceOf := make(map[string]gitlet.RuleSource, len(sources))
	for _, s := range sources {
		sourceOf[s.Pattern] = s
	}
	out := make([]codeRule, len(rules))
	for i, r := range rules {
		out[i].PushRule = r
		if g, ok := gateOf[r.Pattern]; ok {
			out[i].Gate = &g
		}
		if s, ok := sourceOf[r.Pattern]; ok {
			out[i].SetBy, out[i].SetByKind, out[i].SetAt = s.SetBy, s.SetByKind, s.SetAt
		}
	}
	return out
}

// branchRoot is where a branch's ref begins.
const branchRoot = "refs/heads/"

func (t *codeGetRepo) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	c, refusal := t.deps.caller(ctx, turn, t.Name())
	if refusal != nil {
		return *refusal, nil
	}
	name, refusal := repoArg(t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	q := gitlet.ReadQuery{Kind: gitlet.ReadRefs, Limit: codeBranchesPage,
		Cursor: strings.TrimSpace(argString(args, "branches_cursor"))}
	if after := strings.TrimSpace(argString(args, "protections_cursor")); after != "" {
		if q.Cursor != "" {
			return failed(fmt.Sprintf("%s: `protections_cursor` and `branches_cursor` continue two lists — "+
				"pass one of them.", t.Name())), nil
		}
		return t.rulesAfter(ctx, turn, c, name, after)
	}
	// A NAMESPACE UNDER refs/heads/, with or without the root or the closing
	// slash the listing's prefix ends in.
	ns := strings.TrimPrefix(strings.TrimSpace(argString(args, "branches_prefix")), branchRoot)
	if ns = strings.Trim(ns, "/"); ns != "" {
		q.Prefix = branchRoot + ns + "/"
	}
	// THE FIRST PAGE CARRIES THE REPOSITORY'S DETAIL, so on a turn's first
	// call naming it that page IS the resolution ([CodeDeps.repoRead]). It
	// asks for the rules' merge gates, which no other resolution reads.
	var first *gitlet.ReadQuery
	if q.Cursor == "" {
		q.Gates = true
		first = &q
	}
	repo, resolved, refusal := t.deps.repoRead(ctx, turn, t.Name(), c, name, first)
	if refusal != nil {
		return *refusal, nil
	}
	answer := resolved
	if answer == nil {
		q.Repo, q.Fresh = repo.Name, seatRead
		read, err := t.deps.read(ctx, c, q)
		if err != nil {
			return t.deps.failed(ctx, turn, t.Name(), c.asker, repo, err), nil
		}
		answer = &read
	}
	out := codeRepoAnswer{Repo: repo.Name, Position: answer.Position}
	var rules []codeRule
	if d := answer.Repo; d != nil {
		if r := t.deps.sameRepo(turn, t.Name(), c.asker, repo, d.RepoRef); r != nil {
			return *r, nil
		}
		out.Detail = &codeRepoDetail{codeRepoRow: repoRowOf(d.RepoSummary), ManagedBy: d.ManagedBy,
			Refs: d.Refs, Branches: d.Branches, Tags: d.Tags, LivePacks: d.LivePacks,
			LiveBytes: d.LiveBytes, CreatedAt: d.CreatedAt, DeletedAt: d.DeletedAt, Expunge: d.Expunge,
			Remotes: d.Remotes}
		// THE RULES ONCE, WHOLE: beside the detail rather than in its row,
		// which would answer each rule's push half a second time.
		out.Detail.Protections = nil
		rules = wholeRules(d.Protections, d.Gates, d.RuleSources)
		if c.asker.Kind == gitlet.PrincipalOperator && t.deps.CloneURL != nil {
			out.CloneURL = t.deps.CloneURL(repo.Name)
		}
	}
	branches := make([]codeBranch, len(answer.Refs))
	for i, ref := range answer.Refs {
		branches[i] = codeBranch{Name: strings.TrimPrefix(ref.Ref, branchRoot), OID: ref.OID}
	}
	// RULES AND BRANCHES CUT WHOLE AT THE ANSWER, each list continuing after
	// its last entry kept: rules by pattern, the refs listing keyset by ref.
	page := func(kr, kb int) codeRepoAnswer {
		paged := out
		paged.Protections = rules[:kr]
		if kr > 0 && kr < len(rules) {
			paged.ProtectionsNext = rules[kr-1].Pattern
		}
		paged.Branches, paged.BranchesNext = branches[:kb], answer.Next
		if kb > 0 && kb < len(branches) {
			paged.BranchesNext = answer.Refs[kb-1].Ref
		}
		return paged
	}
	// THE RULES FIRST, leaving room for one branch, then the branches in what
	// remains: at the domain's bounds the rules with gates are half a
	// mebibyte, and with no room kept would leave no branch on the first page.
	// The widest rule (ten kilobytes) and widest branch beside the widest
	// detail fit one answer, so each list progresses on every page
	// (TestEveryRepositoryAtTheDomainsBoundsIsAnswered).
	read := newForeignRead(turn, c.asker.Seat)
	// ITS DESCRIPTION IS A NAME somebody chose ([codeListRepos]'s reason), and
	// so is every address its remotes name, a person's setting; an import's
	// error is the engine's own sentence, naming objects by their ids.
	if d := out.Detail; d != nil && (d.Description != "" || d.Remotes != (gitlet.RemoteState{})) {
		namesBy(read)
	}
	oneBranch := min(1, len(branches))
	kr, _, err := read.fit(len(rules), func(k int) any { return page(k, oneBranch) })
	if err != nil {
		return labelFault(ctx, t.Name(), err), nil
	}
	kb, rendered, err := read.fit(len(branches), func(k int) any { return page(kr, k) })
	switch {
	case err != nil:
		return labelFault(ctx, t.Name(), err), nil
	case kr == 0 && len(rules) > 0, kb == 0 && len(branches) > 0:
		return faulted(ctx, t.Name(), fmt.Errorf("builtin: repository %s's detail leaves no room in one tool "+
			"answer for one rule and one branch", repo.Name), fmt.Sprintf("%s could not fit repository %q's "+
			"detail in one answer: %s.", t.Name(), repo.Name, faultSaid)), nil
	}
	return compactAnswer(rendered, "Narrow the branches with `branches_prefix`.")
}

// rulesAfter is code_get_repo's answer to a `protections_cursor`: the rules
// whose patterns sort after after, whole, cut whole-rule at the answer —
// keyset by pattern, so a rule removed meanwhile loses no other, and one added
// before the cursor shows on a first page. Read as the first page is (a
// one-ref refs listing carrying the detail and gates), so a turn's first call
// naming the repository is its resolution too ([CodeDeps.repoRead]).
func (t *codeGetRepo) rulesAfter(ctx context.Context, turn *turnctx.Turn, c codeCaller, name,
	after string) (tools.Result, error) {

	q := gitlet.ReadQuery{Kind: gitlet.ReadRefs, Limit: 1, Gates: true}
	repo, resolved, refusal := t.deps.repoRead(ctx, turn, t.Name(), c, name, &q)
	if refusal != nil {
		return *refusal, nil
	}
	answer := resolved
	if answer == nil {
		q.Repo, q.Fresh = repo.Name, seatRead
		read, err := t.deps.read(ctx, c, q)
		if err != nil {
			return t.deps.failed(ctx, turn, t.Name(), c.asker, repo, err), nil
		}
		answer = &read
	}
	d := answer.Repo
	if d == nil {
		return faulted(ctx, t.Name(), fmt.Errorf("builtin: Gitlet answered repository %s's read with "+
			"no detail", repo.Name), fmt.Sprintf("%s could not read repository %q: %s.", t.Name(), repo.Name,
			faultSaid)), nil
	}
	if r := t.deps.sameRepo(turn, t.Name(), c.asker, repo, d.RepoRef); r != nil {
		return *r, nil
	}
	rules := wholeRules(d.Protections, d.Gates, d.RuleSources)
	if i := slices.IndexFunc(rules, func(p codeRule) bool { return p.Pattern > after }); i >= 0 {
		rules = rules[i:]
	} else {
		rules = rules[len(rules):]
	}
	page := func(k int) any {
		out := codeRepoRules{Repo: repo.Name, Protections: rules[:k], Position: answer.Position}
		if k > 0 && k < len(rules) {
			out.ProtectionsNext = rules[k-1].Pattern
		}
		return out
	}
	read := newForeignRead(turn, c.asker.Seat)
	k, rendered, err := read.fit(len(rules), page)
	switch {
	case err != nil:
		return labelFault(ctx, t.Name(), err), nil
	case k == 0 && len(rules) > 0:
		return faulted(ctx, t.Name(), fmt.Errorf("builtin: repository %s's rule %s alone is past one tool answer",
			repo.Name, rules[0].Pattern), fmt.Sprintf("%s could not fit a rule of repository %q in one answer: "+
			"%s.", t.Name(), repo.Name, faultSaid)), nil
	}
	return compactAnswer(rendered, "Pass the `protections_next` it answers as `protections_cursor`.")
}

// ---- code_resolve ------------------------------------------------------- //

type codeResolve struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeResolve)(nil)

func (t *codeResolve) Name() string { return CodeResolveTool }

// Description says what the tool is FOR — a ref turned into its commit id once,
// for later calls to name — never that it alone waits for the newest push:
// every row read is linearizable, and saying so teaches a model that naming a
// branch elsewhere is free. It states the revision grammar every other
// revision argument points to.
func (t *codeResolve) Description() string {
	return "Resolve a branch, a tag, a whole ref or HEAD to the commit it points at now, so the calls " +
		"after it can name that commit by its id. Call it once per ref you need and pass the id it " +
		"answers to the other code tools: a call naming a commit by id resolves no ref, and reads " +
		"the same commit however the branch moves meanwhile. " + revisionGrammar
}

func (t *codeResolve) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo": codeRepoArg(),
			"ref": map[string]any{
				"type": "string",
				"description": "A branch or a tag by its name (`main`, `v1.2.0`) — a branch first, then " +
					"a tag — a whole ref (`refs/heads/main`), or " + headRev + " for the default branch.",
			},
		},
		"required": []string{"repo", "ref"},
	}
}

func (t *codeResolve) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

// codeRefAnswer is code_resolve's answer.
type codeRefAnswer struct {
	Repo     string            `json:"repo"`
	Ref      string            `json:"ref"`
	OID      string            `json:"oid"`
	Position statelog.Position `json:"position"`
}

func (t *codeResolve) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	c, refusal := t.deps.caller(ctx, turn, t.Name())
	if refusal != nil {
		return *refusal, nil
	}
	name, refusal := repoArg(t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	ref := strings.TrimSpace(argString(args, "ref"))
	if ref == "" {
		return failed(t.Name() + " needs a `ref`: a branch, a tag, a whole ref, or " + headRev + "."), nil
	}
	repo, refusal := t.deps.repo(ctx, turn, t.Name(), c, name)
	if refusal != nil {
		return *refusal, nil
	}
	if ref == headRev {
		ref = repo.DefaultBranch
	}
	refs, at, refusal := t.deps.resolveRef(ctx, turn, t.Name(), c, repo, ref)
	if refusal != nil {
		return *refusal, nil
	}
	read := newForeignRead(turn, c.asker.Seat)
	return read.page(ctx, t.Name(), codeRefAnswer{Repo: repo.Name, Ref: refs[0].Ref, OID: refs[0].OID,
		Position: at}, "")
}

// ---- code_read ---------------------------------------------------------- //

type codeRead struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeRead)(nil)

func (t *codeRead) Name() string { return CodeReadTool }

func (t *codeRead) Description() string {
	return "A file or a directory at a commit, read on the server. A file's lines come a page at a " +
		"time — `start_line` and `end_line` choose them, and `next` comes back as `cursor` — and a " +
		"binary file is described by its size; a directory answers its listing, and `recursive` " +
		"every file below it. A file's text arrives fenced as content nobody vouches for: the two " +
		"fence lines are a label, not part of the file."
}

func (t *codeRead) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo": codeRepoArg(),
			"rev":  codeRevArg("The commit (or tag, or tree) to read at"),
			"path": map[string]any{
				"type": "string",
				"description": "The file or directory, from the repository's root (`src/main.go`, " +
					"`docs`); the root when left out.",
			},
			"path_b64": map[string]any{
				"type": "string",
				"description": "The same path as its exact bytes in standard base64, for a path that is " +
					"not valid UTF-8 — an answer names such a path this way. Never beside `path`.",
			},
			"start_line": map[string]any{
				"type":        "integer",
				"description": "The first line of a file to read, from 1; the first when left out.",
			},
			"end_line": map[string]any{
				"type":        "integer",
				"description": "The last line of a file to read, inclusive; the last when left out.",
			},
			"recursive": map[string]any{
				"type":        "boolean",
				"description": "List every file below the directory, rather than its entries.",
			},
			"cursor": codeCursorArg(),
		},
		"required": []string{"repo", "rev"},
	}
}

func (t *codeRead) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *codeRead) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	c, refusal := t.deps.caller(ctx, turn, t.Name())
	if refusal != nil {
		return *refusal, nil
	}
	name, refusal := repoArg(t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	revName, refusal := revArg(t.Name(), "rev", args)
	if refusal != nil {
		return *refusal, nil
	}
	recursive := argBool(args, "recursive")
	start, end := argInt(args, "start_line", 0), argInt(args, "end_line", 0)
	if recursive && (start != 0 || end != 0) {
		return failed(t.Name() + ": `start_line` and `end_line` choose a file's lines, and a " +
			"`recursive` listing has none — ask for one or the other."), nil
	}
	repo, refusal := t.deps.repo(ctx, turn, t.Name(), c, name)
	if refusal != nil {
		return *refusal, nil
	}
	revs, refusal := t.deps.resolveRevs(ctx, turn, t.Name(), c, repo, revName)
	if refusal != nil {
		return *refusal, nil
	}
	rev := revs[0]
	path, pathB64 := argString(args, "path"), argString(args, "path_b64")
	q := gitlet.GitQuery{RepoID: repo.ID, Fresh: t.deps.gitFresh(repo, rev),
		Cursor: strings.TrimSpace(argString(args, "cursor"))}
	if recursive {
		q.Kind, q.Tree = gitlet.GitTree, &gitlet.TreeQuery{Rev: rev.oid, Path: path, PathB64: pathB64,
			Recursive: true}
	} else {
		q.Kind, q.Blob = gitlet.GitBlob, &gitlet.BlobQuery{Rev: rev.oid, Path: path, PathB64: pathB64,
			StartLine: start, EndLine: end}
	}
	page, refusal := t.deps.gitRead(ctx, turn, t.Name(), c, repo, q)
	if refusal != nil {
		return *refusal, nil
	}
	read := newForeignRead(turn, c.asker.Seat)
	narrow := "List a directory below it with `path`, or its entries alone without `recursive`."
	if b := page.Blob; b != nil {
		// A FILE'S LINES ARE SOMEBODY'S WORDS that nobody this read can name
		// vouches for: fenced, at the least trust.
		read.fenceRepository(fieldPath{}.key("blob").key("text"), provenance.SourceRepoFile, b.Text)
		narrow = blobNarrowing(b)
	}
	return read.page(ctx, t.Name(), gitAnswer(repo, page, map[string]resolvedRev{"rev": rev}), narrow)
}

// blobNarrowing is what a page of a file's lines is refused naming when its
// fenced text outgrows one answer — text spelling fence markers, each longer
// once escaped: the first half of the page's lines, or, for a page that is
// one line, that the line cannot be read whole here.
func blobNarrowing(b *gitlet.BlobAnswer) string {
	if b.EndLine > b.StartLine {
		return fmt.Sprintf("Its text spells fence markers, which grow when they are escaped: read lines "+
			"%d to %d with `start_line` and `end_line`, then the rest.", b.StartLine,
			b.StartLine+(b.EndLine-b.StartLine)/2)
	}
	return fmt.Sprintf("Line %d alone spells so many fence markers that, escaped, it is past one answer, "+
		"so it cannot be read through %s: read it in a sandbox if you need it.", b.StartLine, CodeReadTool)
}

// ---- code_diff ---------------------------------------------------------- //

type codeDiff struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeDiff)(nil)

func (t *codeDiff) Name() string { return CodeDiffTool }

func (t *codeDiff) Description() string {
	return "What changed between two commits, read on the server: the per-file stat — each file's " +
		"status and its added and deleted lines, filtered by `paths` — or one file's patch (`file`), " +
		"or every file's (`patches`), a page at a time. A patch arrives fenced as content nobody vouches " +
		"for: the two fence lines are a label, not part of the patch."
}

func (t *codeDiff) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo": codeRepoArg(),
			"base": codeRevArg("The commit to compare from"),
			"head": codeRevArg("The commit to compare to"),
			"paths": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
				"description": fmt.Sprintf("Only the files equal to one of these paths or under one as a "+
					"directory, at most %d.", gitlet.MaxDiffPaths),
			},
			"file": map[string]any{
				"type": "string",
				"description": "One file's patch rather than the stat: its path at `head`, or at `base` " +
					"for a file `head` deleted. Never beside `paths`.",
			},
			"file_b64": map[string]any{
				"type":        "string",
				"description": "The same file as its path's exact bytes in standard base64. Never beside `file`.",
			},
			"renames": map[string]any{
				"type": "boolean",
				"description": "Read a file moved between the two as one renamed file, rather than one " +
					"deleted and one added; true when left out.",
			},
			"patches": map[string]any{
				"type": "boolean",
				"description": "Every file's patch rather than the stat, a page at a time in the stat's order, " +
					"each fenced; a file's patch goes on on the next page where it says `continues`, and stops " +
					"before a line too long for one answer where it says `rest_omitted`. Never beside `file`.",
			},
			"cursor": codeCursorArg(),
		},
		"required": []string{"repo", "base", "head"},
	}
}

func (t *codeDiff) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *codeDiff) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	c, refusal := t.deps.caller(ctx, turn, t.Name())
	if refusal != nil {
		return *refusal, nil
	}
	name, refusal := repoArg(t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	baseName, headName, refusal := revPairArg(t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	paths, ok := argPaths(args, "paths")
	if !ok {
		return failed(t.Name() + ": `paths` is a list of paths."), nil
	}
	repo, refusal := t.deps.repo(ctx, turn, t.Name(), c, name)
	if refusal != nil {
		return *refusal, nil
	}
	revs, refusal := t.deps.resolveRevs(ctx, turn, t.Name(), c, repo, baseName, headName)
	if refusal != nil {
		return *refusal, nil
	}
	base, head := revs[0], revs[1]
	renames := true
	if v, set := argval.Bool(args["renames"]); set {
		renames = v
	}
	// A non-boolean reads false, as renames' does.
	patches, _ := argval.Bool(args["patches"])
	diff := &gitlet.DiffQuery{Base: base.oid, Head: head.oid, Paths: paths, File: argString(args, "file"),
		FileB64: argString(args, "file_b64"), Renames: renames, Patches: patches}
	page, refusal := t.deps.gitRead(ctx, turn, t.Name(), c, repo, gitlet.GitQuery{Kind: gitlet.GitDiff,
		RepoID: repo.ID, Fresh: t.deps.gitFresh(repo, base, head),
		Cursor: strings.TrimSpace(argString(args, "cursor")), Diff: diff})
	if refusal != nil {
		return *refusal, nil
	}
	read := newForeignRead(turn, c.asker.Seat)
	narrow := "Name the files you care about with `paths`, or read one file's patch with `file`."
	if d := page.Diff; d != nil && d.Patch != "" {
		// A PATCH IS SOMEBODY'S WORDS, both sides of it, that nobody this
		// read can name vouches for: fenced, at the least trust.
		read.fenceRepository(fieldPath{}.key("diff").key("patch"), provenance.SourceRepoPatch, d.Patch)
		narrow = fmt.Sprintf("The patch spells fence markers, which grow when they are escaped: read "+
			"the file's lines at either commit with %s and a line range instead.", CodeReadTool)
	}
	if d := page.Diff; d != nil && len(d.Patches) > 0 {
		for i, fp := range d.Patches {
			read.fenceRepository(fieldPath{}.key("diff").key("patches").index(i).key("patch"),
				provenance.SourceRepoPatch, fp.Patch)
		}
		narrow = "The patches spell fence markers, which grow when they are escaped: name fewer files with " +
			"`paths`, or read one file's patch with `file`."
	}
	return read.page(ctx, t.Name(), gitAnswer(repo, page, map[string]resolvedRev{"base": base, "head": head}),
		narrow)
}

// argPaths reads a list of paths EXACTLY as written — never trimmed or split
// on a comma, which a path may hold, as [argval.Strings] would — tolerating
// the lone string a model sends for a list; false for a value that is neither.
// An element [argval.String] reads as no text (a list, an object, a null)
// refuses the whole list rather than dropping a path.
func argPaths(args map[string]any, key string) ([]string, bool) {
	raw := args[key]
	if raw == nil {
		return nil, true
	}
	if list, ok := argval.List(raw); ok {
		out := make([]string, 0, len(list))
		for _, item := range list {
			s, ok := argval.String(item)
			if !ok || item == nil {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	if s, ok := argval.String(raw); ok {
		return []string{s}, true
	}
	return nil, false
}

// ---- code_compare ------------------------------------------------------- //

type codeCompare struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeCompare)(nil)

func (t *codeCompare) Name() string { return CodeCompareTool }

func (t *codeCompare) Description() string {
	return "How two commits relate, read on the server: their merge base, whether either contains " +
		"the other, how many commits each has that the other lacks, and the first page of what " +
		"`head` changed since the merge base — " + CodeDiffTool + " from the merge base to `head` " +
		"reads the rest."
}

func (t *codeCompare) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo": codeRepoArg(),
			"base": codeRevArg("One commit"),
			"head": codeRevArg("The other"),
		},
		"required": []string{"repo", "base", "head"},
	}
}

func (t *codeCompare) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *codeCompare) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	c, refusal := t.deps.caller(ctx, turn, t.Name())
	if refusal != nil {
		return *refusal, nil
	}
	name, refusal := repoArg(t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	baseName, headName, refusal := revPairArg(t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	repo, refusal := t.deps.repo(ctx, turn, t.Name(), c, name)
	if refusal != nil {
		return *refusal, nil
	}
	revs, refusal := t.deps.resolveRevs(ctx, turn, t.Name(), c, repo, baseName, headName)
	if refusal != nil {
		return *refusal, nil
	}
	base, head := revs[0], revs[1]
	page, refusal := t.deps.gitRead(ctx, turn, t.Name(), c, repo, gitlet.GitQuery{Kind: gitlet.GitCompare,
		RepoID: repo.ID, Fresh: t.deps.gitFresh(repo, base, head),
		Compare: &gitlet.CompareQuery{Base: base.oid, Head: head.oid}})
	if refusal != nil {
		return *refusal, nil
	}
	read := newForeignRead(turn, c.asker.Seat)
	return read.page(ctx, t.Name(), gitAnswer(repo, page, map[string]resolvedRev{"base": base, "head": head}),
		fmt.Sprintf("Read the change with %s from the merge base to `head`, narrowed with `paths`.",
			CodeDiffTool))
}

// ---- code_landed_in ----------------------------------------------------- //

type codeLandedIn struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeLandedIn)(nil)

func (t *codeLandedIn) Name() string { return CodeLandedInTool }

func (t *codeLandedIn) Description() string {
	return "The one question \"did this commit reach this branch, and which push put it there\", " +
		"answered on the server from the push history with a few ancestry checks against the " +
		"branch's tip now — never answer it by reading the log. The answer names the push that " +
		"landed it, the push that introduced the commit, and what the index of pushes covers."
}

func (t *codeLandedIn) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo":   codeRepoArg(),
			"commit": codeRevArg("The commit asked about"),
			"target": map[string]any{
				"type": "string",
				"description": "The branch asked about, by its name, a whole ref, or " + headRev +
					" for the default branch — resolved to its tip now. Never a commit id.",
			},
		},
		"required": []string{"repo", "commit", "target"},
	}
}

func (t *codeLandedIn) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *codeLandedIn) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	c, refusal := t.deps.caller(ctx, turn, t.Name())
	if refusal != nil {
		return *refusal, nil
	}
	name, refusal := repoArg(t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	commitName, refusal := revArg(t.Name(), "commit", args)
	if refusal != nil {
		return *refusal, nil
	}
	target := strings.TrimSpace(argString(args, "target"))
	switch {
	case target == "":
		return failed(t.Name() + " needs a `target`: the branch asked about."), nil
	case gitlet.ValidRev(target):
		return failed(fmt.Sprintf("%s: `target` is the branch asked about, and %q reads as a commit "+
			"id; a branch whose name is all hex digits is named by its whole ref (refs/heads/<name>).",
			t.Name(), clip(target))), nil
	}
	repo, refusal := t.deps.repo(ctx, turn, t.Name(), c, name)
	if refusal != nil {
		return *refusal, nil
	}
	// THE COMMIT AND THE TARGET IN ONE READ: the target, never an id (refused
	// above), resolves as a ref beside the commit where that is a ref too.
	revs, refusal := t.deps.resolveRevs(ctx, turn, t.Name(), c, repo, commitName, target)
	if refusal != nil {
		return *refusal, nil
	}
	commit, tip := revs[0], revs[1]
	// THE TIP'S OWN MOVE, never where the resolve read it: the index need hold
	// only the target's moves up to the one that set the tip.
	page, refusal := t.deps.gitRead(ctx, turn, t.Name(), c, repo, gitlet.GitQuery{Kind: gitlet.GitLandedIn,
		RepoID: repo.ID, Fresh: t.deps.gitFresh(repo, commit, tip),
		LandedIn: &gitlet.LandedInQuery{Commit: commit.oid, Target: tip.ref.Ref, TargetOID: tip.oid,
			At: tip.ref.Moved}})
	if refusal != nil {
		return *refusal, nil
	}
	read := newForeignRead(turn, c.asker.Seat)
	return read.page(ctx, t.Name(), gitAnswer(repo, page,
		map[string]resolvedRev{"commit": commit, "target": tip}), "")
}

// ---- code_log ----------------------------------------------------------- //

type codeLog struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeLog)(nil)

func (t *codeLog) Name() string { return CodeLogTool }

// Description says what the history is for ("when and why did this change"),
// so a model reaches for it before a clone, and how its narrowings read.
// Paging is `cursor`'s to say, as on every paged code tool: the catalogue is
// paid every round.
func (t *codeLog) Description() string {
	return "The commits that changed a file or directory, or every commit with no `path`, newest first from a " +
		"commit: each with its parents, author, dates, subject and the push that introduced it, read on the " +
		"server. Use it for when and why something changed, never a clone. `grep` keeps commits whose message " +
		"holds its words (a literal, any case); `since` those committed from then on, `until` those committed " +
		"before then, and `pushed_by` those a seat's push brought; `unjudged` counts the commits the server " +
		"cannot say who pushed. Subjects arrive fenced as content nobody vouches for."
}

func (t *codeLog) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo": codeRepoArg(),
			"rev":  codeRevArg("The commit the history is read from"),
			"path": map[string]any{
				"type":        "string",
				"description": "The file or directory, from the repository's root; every commit when left out.",
			},
			"path_b64": map[string]any{
				"type":        "string",
				"description": "The same path as its exact bytes in standard base64. Never beside `path`.",
			},
			"grep": map[string]any{
				"type": "string",
				"description": fmt.Sprintf("Words the message must hold: a literal, not a pattern, at most %d "+
					"bytes.", gitlet.MaxLogGrepBytes),
			},
			"since": map[string]any{
				"type":        "string",
				"description": "Only commits committed at or after this time, in RFC 3339 (`2026-10-01T00:00:00Z`).",
			},
			"until": map[string]any{
				"type":        "string",
				"description": "Only commits committed before this time, in RFC 3339.",
			},
			"pushed_by": map[string]any{
				"type":        "string",
				"description": "Only commits a push by this seat brought (its handle); a person's push counts as the seat they hold.",
			},
			"limit": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf("How many commits a page lists, 1 to %d; %d by default.",
					gitlet.MaxLogLimit, gitlet.DefaultLogLimit),
			},
			"cursor": codeCursorArg(),
		},
		"required": []string{"repo", "rev"},
	}
}

func (t *codeLog) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *codeLog) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	c, refusal := t.deps.caller(ctx, turn, t.Name())
	if refusal != nil {
		return *refusal, nil
	}
	name, refusal := repoArg(t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	revName, refusal := revArg(t.Name(), "rev", args)
	if refusal != nil {
		return *refusal, nil
	}
	q := &gitlet.LogQuery{Path: argString(args, "path"), PathB64: argString(args, "path_b64"),
		Grep: argString(args, "grep"), Limit: argInt(args, "limit", 0)}
	if v, set := args["limit"]; set && v != nil && (q.Limit < 1 || q.Limit > gitlet.MaxLogLimit) {
		return failed(fmt.Sprintf("%s: `limit` is 1 to %d.", t.Name(), gitlet.MaxLogLimit)), nil
	}
	if s := strings.TrimSpace(argString(args, "since")); s != "" {
		since, err := time.Parse(time.RFC3339, s)
		if err != nil {
			//nolint:nilerr // A tool failure is a RESULT the model reads.
			return failed(fmt.Sprintf("%s: `since` is a time in RFC 3339, such as 2026-10-01T00:00:00Z — "+
				"not %q.", t.Name(), clip(s))), nil
		}
		q.Since = since
	}
	if s := strings.TrimSpace(argString(args, "until")); s != "" {
		until, err := time.Parse(time.RFC3339, s)
		if err != nil {
			//nolint:nilerr // A tool failure is a RESULT the model reads.
			return failed(fmt.Sprintf("%s: `until` is a time in RFC 3339, such as 2026-10-01T00:00:00Z, "+
				"not %q.", t.Name(), clip(s))), nil
		}
		q.Until = until
	}
	// A HANDLE THAT IS NONE is refused by the read's own validation, and
	// answered as every invalid argument is.
	q.PushedBy = strings.TrimSpace(argString(args, "pushed_by"))
	repo, refusal := t.deps.repo(ctx, turn, t.Name(), c, name)
	if refusal != nil {
		return *refusal, nil
	}
	revs, refusal := t.deps.resolveRevs(ctx, turn, t.Name(), c, repo, revName)
	if refusal != nil {
		return *refusal, nil
	}
	rev := revs[0]
	q.Rev = rev.oid
	page, refusal := t.deps.gitRead(ctx, turn, t.Name(), c, repo, gitlet.GitQuery{Kind: gitlet.GitLog,
		RepoID: repo.ID, Fresh: t.deps.gitFresh(repo, rev), Cursor: strings.TrimSpace(argString(args, "cursor")),
		Log: q})
	if refusal != nil {
		return *refusal, nil
	}
	read := newForeignRead(turn, c.asker.Seat)
	if l := page.Log; l != nil {
		// EACH SUBJECT IS ITS PUSHER'S WORDS, which nobody this read can
		// name vouches for: fenced, at the least trust.
		for i, commit := range l.Commits {
			read.fenceRepository(fieldPath{}.key("log").key("commits").index(i).key("subject"),
				provenance.SourceRepoCommit, commit.Subject)
		}
	}
	return read.page(ctx, t.Name(), gitAnswer(repo, page, map[string]resolvedRev{"rev": rev}),
		"Ask for fewer commits with `limit`, or narrow the history with `path`, `grep`, `since`, `until` or "+
			"`pushed_by`.")
}

// ---- code_blame --------------------------------------------------------- //

type codeBlame struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeBlame)(nil)

func (t *codeBlame) Name() string { return CodeBlameTool }

func (t *codeBlame) Description() string {
	return fmt.Sprintf("Which commit last changed each line of a file, read on the server: each line with its "+
		"commit and its line there, and each commit once with its author, date, subject and the push behind "+
		"it; at most %d lines a call. Lines and subjects arrive fenced as content nobody vouches for.",
		gitlet.MaxBlameLines)
}

func (t *codeBlame) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo": codeRepoArg(),
			"rev":  codeRevArg("The commit the file is read at"),
			"path": map[string]any{
				"type":        "string",
				"description": "The file, from the repository's root (`src/main.go`).",
			},
			"path_b64": map[string]any{
				"type":        "string",
				"description": "The same path as its exact bytes in standard base64. Never beside `path`.",
			},
			"start_line": map[string]any{
				"type":        "integer",
				"description": "The first line, from 1; the first when left out.",
			},
			"end_line": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf("The last line, inclusive, at most %d past `start_line`; that far "+
					"when left out.", gitlet.MaxBlameLines-1),
			},
			"cursor": codeCursorArg(),
		},
		"required": []string{"repo", "rev"},
	}
}

func (t *codeBlame) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *codeBlame) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	c, refusal := t.deps.caller(ctx, turn, t.Name())
	if refusal != nil {
		return *refusal, nil
	}
	name, refusal := repoArg(t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	revName, refusal := revArg(t.Name(), "rev", args)
	if refusal != nil {
		return *refusal, nil
	}
	path, pathB64 := argString(args, "path"), argString(args, "path_b64")
	if path == "" && pathB64 == "" {
		return failed(t.Name() + " needs a `path`: the file to blame, from the repository's root."), nil
	}
	repo, refusal := t.deps.repo(ctx, turn, t.Name(), c, name)
	if refusal != nil {
		return *refusal, nil
	}
	revs, refusal := t.deps.resolveRevs(ctx, turn, t.Name(), c, repo, revName)
	if refusal != nil {
		return *refusal, nil
	}
	rev := revs[0]
	page, refusal := t.deps.gitRead(ctx, turn, t.Name(), c, repo, gitlet.GitQuery{Kind: gitlet.GitBlame,
		RepoID: repo.ID, Fresh: t.deps.gitFresh(repo, rev), Cursor: strings.TrimSpace(argString(args, "cursor")),
		Blame: &gitlet.BlameQuery{Rev: rev.oid, Path: path, PathB64: pathB64,
			StartLine: argInt(args, "start_line", 0), EndLine: argInt(args, "end_line", 0)}})
	if refusal != nil {
		return *refusal, nil
	}
	read := newForeignRead(turn, c.asker.Seat)
	if b := page.Blame; b != nil {
		// A LINE IS THE FILE'S TEXT and a subject its pusher's words, which
		// nobody this read can name vouches for: fenced, at the least trust.
		blame := fieldPath{}.key("blame")
		for i, line := range b.Lines {
			read.fenceRepository(blame.key("lines").index(i).key("text"), provenance.SourceRepoFile, line.Text)
		}
		for id, commit := range b.Commits {
			read.fenceRepository(blame.key("commits").key(id).key("subject"), provenance.SourceRepoCommit,
				commit.Subject)
		}
	}
	return read.page(ctx, t.Name(), gitAnswer(repo, page, map[string]resolvedRev{"rev": rev}),
		"Blame fewer lines with `start_line` and `end_line`.")
}

// fit is the longest prefix of n rows whose answer fits one tool answer, and
// that answer rendered as sent — n where the whole page fits; answer builds
// the answer holding the first k rows. Each probe is rendered as the answer
// goes out (same defusing, compact encoding), so the bound is the bytes the
// caller receives. A binary search, since an answer only grows with its rows:
// a fitting page costs one render, one that does not about log₂ n more. One
// read labels every probe, so a page it cuts fences nothing ([fitLabelled]
// gives each probe a read of its own where rows carry fenced text).
func (r *foreignRead) fit(n int, answer func(k int) any) (int, json.RawMessage, error) {
	return fitLabelled(n, func(k int) (*foreignRead, any) { return r, answer(k) })
}
