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
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE CODE READ TOOLS: the company's own repositories (`code.native`), read on
// the server rather than in a clone — code_list_repos, code_get_repo,
// code_resolve, code_read, code_diff, code_compare, code_landed_in, code_log
// and code_blame, and a repository's changes, code_list_changes and
// code_get_change (codechanges.go) — each named for its authority row, and ONE
// list ([CodeTools]) that a seat's registry and the operator catalogue both
// take, so a schema, a default and a refusal's words are written once. The
// code WRITES join the same list where the code host's writes are wired
// (codewrite.go), through the same resolution of a repository — and the same
// turn's hold on it (codehold.go).
//
// # Every read is judged where it is answered
//
// The gate admits every principal (`authz.ClassEverybody`): who may read a
// repository is a GRANT the serving node derives from its own chart for the
// repository a call names (ADR-0058), which the gate cannot ask before the
// repository is read. So each tool names its caller — [CodeAskerOf], the one
// conversion the REST reads use too — and the data node or the git node that
// answers holds the read to that caller's grant: a listing leaves out what the
// caller may not read, and a read of such a repository is refused
// `not_permitted` in the domain's own sentence. No argument names who reads.
//
// # Every read is decided again for whoever woke the turn
//
// Inside a turn somebody woke, a seat reading code is that somebody's deputy,
// and a deputy that read out whatever it may read itself would hand a
// `visibility: unit` repository to anybody who can message one of the unit's
// seats. So every call is decided twice, as the artifacts tools decide theirs
// (the REQUESTER CEILING, [ceilingOf]): by the tool's own authority row asked
// again for the requester ([authz.DecideWithin]) — a stranger the company
// does not know reads nothing, and neither does a person who may not act in
// it now — and, for a requester that is a colleague SEAT, by that seat's own
// grant on the repository, which rides on every read the call makes
// ([code.ReadQuery.CeilingSeat]) for the node that answers it to judge beside
// the asker's, from the same chart and the same row: a listing leaves out
// what the colleague may not read, and a read of such a repository is refused
// in the code host's own sentence ([code.ErrAboveCeiling]). Judged THERE,
// never here against the repository the turn resolved once: the asker's
// grant is judged afresh at every read, and a ceiling judged on the turn's
// memo would keep reading for a colleague a later activation shut out. A
// person passes — every person maintains every repository (ADR-0055) — and a
// call outside any turn, a person's own assistant, reads for nobody but its
// caller. The requester's authority is a ceiling and never a source: what is
// read is the seat's own read, judged by its own grant.
//
// # Rows at the seat's level, objects by id
//
// What a repository is and what a ref points at are ROWS, read at the seat's
// level — linearizable on both surfaces ([seatReadLevel]), so an answer
// includes every push that landed before the call. What an object id names is
// the same object on every node at every position, so a read by one needs no
// level: it is asked with the caller's floor on the code log — this node's own
// pushes to THAT repository (never its writes to any other, which no copy of
// this one holds), and where each ref it named last MOVED, the push that set
// it — as the git node's SYNC level, never a barrier. NEVER WHERE THE RESOLVE
// READ THE REF: a linearizable read's checkpoint is past its own barrier, so a
// copy current to every push of the repository still sits below it, and a
// floor there made the git node run a sync round for nothing on every read by
// a ref; floored at the ref's move, a current copy answers as it stands, and
// only one behind the commits a call names syncs first. A turn pays one
// barrier per repository it names — the repository is resolved ONCE A TURN
// ([CodeDeps.repo], kept on the turn's lookups) — and one per call that names
// refs, however many it names, all resolved in one read ([CodeDeps.resolveRef])
// at one checkpoint; a call that names commits by id pays none beyond the
// repository's.
//
// # Text somebody else wrote is fenced
//
// A file's lines (code_read, and each line code_blame names) and a patch
// (code_diff) are text a repository's committers wrote — importers and
// outside contributors among them — that a read by object id cannot
// attribute, so each goes out inside a fence of the turn's boundary at trust
// `external`, naming no writer (ADR-0040); and so does each commit's subject
// code_log and code_blame name (`repo_commit`), a message its pusher wrote,
// up to code.MaxLogSubjectBytes of it — the long text a history is read by.
// Every other string of every answer — paths, ref names, descriptions, a
// commit's author and address, short and named beside a fenced subject — is
// defused ([foreignRead]). The git node that cuts a history's or a blame's
// page counts each of those fences and every string's defusing into the
// page's bytes (internal/code/query's page.go), so a page of a hundred fenced
// subjects is still one answer.
//
// # One page is one tool answer, sent as it was measured
//
// A git node cuts every page at code.MaxGitAnswerBytes of compact JSON, the
// tool answer less what these tools wrap round it, and these tools send it
// compact ([compactAnswer]): indented, a page of short entries would arrive
// past the answer it was cut to fit. A row listing is cut by these tools, whole
// rows, at the answer as it is sent ([foreignRead.fit]). A page whose text the
// fence lengthens past the answer — text spelling fence markers, each of which
// grows by its escape — is refused naming a narrower read, never cut.

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

// CodeToolNames are the eleven, in the order a catalogue lists them: the
// repositories, a ref, then the reads by object id — the history's last — and
// a repository's changes ([ChangeToolNames]).
func CodeToolNames() []string {
	return append([]string{CodeListReposTool, CodeGetRepoTool, CodeResolveTool, CodeReadTool,
		CodeDiffTool, CodeCompareTool, CodeLandedInTool, CodeLogTool, CodeBlameTool}, ChangeToolNames()...)
}

// The listing pages. A repository row carries a description of up to a
// kibibyte and its protection rules, so a page of a hundred is cut, whole
// rows, wherever it outgrows one tool answer; twenty-five is a screen of
// them. A branch row is a name and an id, so a hundred fit beside the detail
// unless the names are long, and are cut the same way.
const (
	defaultCodeRepoList = 25
	maxCodeRepoList     = 100
	codeBranchesPage    = 100
)

// CodeReader is the routed code host as these tools ask it — estate.CodeSeam,
// declared here by its consumer: this node's own copy of the code log where it
// serves, a data node's otherwise, and a repository's home git node for its
// content.
type CodeReader interface {
	Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error)
	ResolveRef(ctx context.Context, q code.RefQuery) (code.RefAnswer, error)
	GitRead(ctx context.Context, q code.GitQuery) (code.GitAnswer, error)
	// FloorOf is this node's floor on repository repoID — its own pushes to
	// that repository, and none of its writes to any other — which every read
	// by object id of the repository is asked with, so the copy that answers
	// holds them.
	FloorOf(repoID string) statelog.Position
}

// CodeDeps are what the code tools read through. The zero value serves none
// of them: a company that hosts no code.
type CodeDeps struct {
	// Code is the code host as the estate router answers it. Nil omits every
	// code tool.
	Code CodeReader

	// Actor is who is asking: the SAME resolver the work tools take
	// ([WorkDeps.Actor]), so a seat's turn and an operator's call name their
	// caller one way. Nil takes the turn's seat — every in-engine caller —
	// and the operator surface sets the request's principal.
	Actor func(ctx context.Context, turn *turnctx.Turn) (Actor, error)

	// CloneURL is the address a person's git clones a repository from — on
	// the deployment's `api.external_url`, where git is served — or empty
	// where the deployment names none. Nil names none. Only a person's and a
	// token's answer carries it: a seat reads through these tools, and a URL
	// it could not authenticate to would only send it round.
	CloneURL func(repo string) string

	// Requesters composes who a seat's turn is acting for, and Chart is the
	// chart the authority table decides them by: every call in a turn
	// somebody woke is decided again for them ([CodeDeps.caller]). REQUIRED
	// beside Code — the tools are wired together or not at all, as the
	// artifacts tools are, so a surface that left the ceiling out serves no
	// code read rather than every one to whoever asks a seat for it.
	Requesters Requesters
	Chart      authz.Chart

	// Writer is the code host's writes — an engine-built write asked of the
	// repository's home git node, and a repository's lifecycle request asked
	// of a data node — the same routed code host Code reads. Nil omits every
	// code WRITE tool and leaves the reads as they are: a surface that reads
	// the company's code and changes none of it. See codewrite.go.
	Writer CodeWriter

	// Tasks is the engine's own tracker as one task is read, which resolves
	// the native work item a change write names by its key to the item's
	// identity ([CodeDeps.workItemArg]): set where the company runs the
	// native tracker. Nil resolves none, and a call naming a native item's
	// key is refused, naming the `<backend>:<id>` form every item takes —
	// never filed under a key nobody resolved.
	Tasks TaskReader

	// Holder records a turn's hold on a scratch repository it names
	// ([HoldRepo]), set wherever the tools are offered to seats and
	// independently of Writer, because a seat that only reads still holds
	// what it reads. Nil records none: a turn that reaches a scratch
	// repository idle long enough to need its hold recorded is refused
	// rather than sent there unordered against an expiry in flight. An
	// operator's call has no turn, and holds nothing either way.
	Holder CodeHolder

	// Now is the clock a hold's idle threshold is read on, read after the
	// hold is taken; nil is the wall clock.
	Now func() time.Time

	// ops is how a write tool's call names its operation ([operations]):
	// derived from a seat's turn or a request's own key, or held by the
	// operator's assistant over MCP, which [OperatorTools] alone says.
	ops operations
}

// serves reports whether every half the tools read through is wired: the code
// host, and the requester ceiling's composition and chart.
func (d CodeDeps) serves() bool { return d.Code != nil && d.Requesters != nil && d.Chart != nil }

// CodeTools are the code tools a seat and an operator share: ONE list, so a
// schema, a default and a refusal's words are written once, and nil where
// deps are not wholly wired ([CodeDeps.serves]) — a company that hosts no
// code. The reads first, which deliver nothing; then, where the code host's
// writes are wired ([CodeDeps.Writer]), the writes ([CodeWrites]), each a
// delivery to the code host's surface.
func CodeTools(deps CodeDeps) []tools.Callable {
	if !deps.serves() {
		return nil
	}
	out := []tools.Callable{
		&codeListRepos{deps: deps}, &codeGetRepo{deps: deps}, &codeResolve{deps: deps},
		&codeRead{deps: deps}, &codeDiff{deps: deps}, &codeCompare{deps: deps},
		&codeLandedIn{deps: deps}, &codeLog{deps: deps}, &codeBlame{deps: deps},
		&codeListChanges{deps: deps}, &codeGetChange{deps: deps},
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
// sub-agent guard; false for a name that is not one.
//
// EVERY READ IS A PROVEN READ, and idempotent: asking twice costs a round and
// changes nothing. So a turn that only read code is a turn that delivered
// nothing (the delivery check), a worker may be handed them (the guard), and
// the operator catalogue audits no call to one, as it audits no read — the
// operator's own `code_repo_readers` among them.
//
// EVERY WRITE IS A WRITE TO A SHARED SURFACE — a branch, a repository, a
// protection, a change every colleague of the company reads — so `ReadOnly:
// No` with `OpenWorld: Yes`, which [mcp.WritesToSharedSurface] reads as true:
// a worker acting under its parent's name is given none of them (a second,
// unbudgeted writer with the parent's grant), the delivery check counts a call
// to one as a delivery, and the operator catalogue audits every call.
// IDEMPOTENT where each is held to what its caller read: a commit, a revert, a
// pick and a deletion to the branch's head (`expected_head`), a branch's
// creation to its absence, a repository's to its free name, a change's open
// to the one live change its branch may head and every other write of its
// lifecycle to the version its caller read, an ask of a reviewer already
// asked and an answer a finding already holds to nothing, and a setting or a
// protection set again is the same setting — so a call made twice leaves what
// one call left, which is what makes a retry after a lost answer safe. NOT a
// review and not a comment, which are additive: the engine makes the same
// call's re-run the same write, but the annotation describes the tool a model
// is offered, and two reviews or two comments saying the same are two (the
// tracker's comment_on_work_item, for the same reason). DESTRUCTIVE only where
// a call can take away what somebody else made with nothing kept to undo it by
// the same tool: a branch deleted (its commits stay in the repository, its
// name and head only in the push history), a repository deleted (restorable
// until its purge, then gone), and a finding dismissed — a reviewer's
// blocker cleared, which nothing moves back. A commit moves a branch forward
// over its history, which keeps every earlier version; a setting, a
// protection or what a change says is set back by the same tool, the record
// of each change keeping what it replaced; and an archive, a close and a
// draft are each undone by their opposite.
func codeAnnotations(name string) (tools.Annotations, bool) {
	for _, tool := range append(CodeToolNames(), CodeRepoReadersTool) {
		if tool == name {
			return tools.Annotations{ReadOnly: mcp.Yes, Idempotent: mcp.Yes}, true
		}
	}
	if slices.Contains(CodeWrites(), name) {
		destructive, idempotent := mcp.No, mcp.Yes
		switch name {
		case CodeDeleteBranchTool, CodeDeleteRepoTool, CodeResolveFindingTool:
			destructive = mcp.Yes
		case CodeReviewChangeTool, CodeCommentOnChangeTool:
			idempotent = mcp.No
		}
		return tools.Annotations{ReadOnly: mcp.No, Destructive: destructive, Idempotent: idempotent,
			OpenWorld: mcp.Yes}, true
	}
	return tools.Annotations{}, false
}

// CodeAskerOf is the principal a code read is judged as, from who is asking:
// a seat by its handle, which the serving node's chart relates to the
// repository; a person — every person holds a human seat — as the credential
// they act through, bound to that seat; and a credential nobody is bound
// through, or the engine itself, as that credential alone.
//
// THE ONE CONVERSION, for every surface that reads the code host on somebody's
// behalf: a seat's turn and an operator's call here, and the REST reads, which
// compose their caller with [ActorOf] first. Two conversions would be two
// readings of one principal, free to judge the same person by two grants.
func CodeAskerOf(a Actor) code.Principal {
	switch a.Kind {
	case tracker.AuthorAgent:
		return code.Principal{Kind: code.PrincipalAgent, Seat: a.Handle}
	case tracker.AuthorHuman:
		return code.Principal{Kind: code.PrincipalOperator, OperatorID: a.OperatorID, Seat: a.Handle}
	}
	// AN OPERATOR CREDENTIAL, AND THE ENGINE: a credential acting as itself,
	// bound to no seat, which reads what an unbound token reads.
	return code.Principal{Kind: code.PrincipalOperator, OperatorID: a.OperatorID}
}

// ---- who asks, and what a call names ------------------------------------ //

// asker is who this call reads as, or the refusal it is answered with: the
// caller as a write would name them ([Actor], its run's call log on it, as
// [WorkDeps.actor] puts it there) and the principal a repository's grant
// judges.
func (d CodeDeps) asker(ctx context.Context, turn *turnctx.Turn, tool string) (Actor, code.Principal,
	*tools.Result) {

	resolve := actorFor
	if d.Actor != nil {
		resolve = func(turn *turnctx.Turn) (Actor, error) { return d.Actor(ctx, turn) }
	}
	actor, err := resolve(turn)
	if err != nil {
		r := refusedBy(tools.RefusalForbidden, err, tool+" can only be called during a turn, on "+
			"behalf of a seat, or by a signed-in caller.")
		return Actor{}, code.Principal{}, &r
	}
	// THE RUN'S CALL LOG IS THE TURN'S, whoever resolved the rest — a write's
	// operation counts the run's earlier calls to the tool from it ([opIDFor]).
	actor.Calls = turn.CallLog()
	p := CodeAskerOf(actor)
	if err := p.Validate(); err != nil {
		r := refusedBy(tools.RefusalForbidden, err, fmt.Sprintf("%s could not name who is asking "+
			"in the terms a repository's grant is judged by, so it read nothing.", tool))
		return Actor{}, code.Principal{}, &r
	}
	return actor, p, nil
}

// codeCaller is who one call of a code tool reads as: the caller as a write
// and a hold name them, the asker its grant is judged by, and — inside a turn
// a colleague seat woke — that seat, whose own grant bounds the asker's on
// every read the call makes ([code.ReadQuery.CeilingSeat]), "" for a call made
// for nobody else.
//
// A WRITE's call carries the same requester as its write's ceiling too
// ([CodeDeps.writeCaller]): nil for a write made for its seat alone or for a
// person, a colleague seat's, or nobody's — a requester who may change
// nothing, whose turn the write is still made in for the seat's own work.
type codeCaller struct {
	actor   Actor
	asker   code.Principal
	ceiling string
	write   *code.WriteCeiling
}

// caller is who this call reads as, or the refusal it is answered with: the
// asker ([CodeDeps.asker]) and, inside a seat's turn somebody woke, whoever
// that is ([ceilingOf]), decided AGAIN by the tool's own authority row
// ([authz.DecideWithin], the artifacts tools' one function): a stranger
// reads nothing, a requester who may not act in the company reads nothing,
// and a requester the node could not compose or decide is answered as a node
// that could not tell, never as a turn nobody woke. A colleague seat the row
// admits rides on every read as the call's ceiling, for the node that answers
// to judge its grant on the repository ([code.JudgeRead]); a person the row
// admits bounds nothing, since a person maintains every repository.
//
// FIRST, before any argument is read, as the artifacts tools compose theirs:
// whom a call reads for decides whether it reads at all, and the composition
// reads the running chart and, for a person, the identity directory — never a
// barrier on a log. A person's or a token's call is no deputy, and composes
// nothing.
func (d CodeDeps) caller(ctx context.Context, turn *turnctx.Turn, tool string) (codeCaller, *tools.Result) {
	return d.callerOn(ctx, turn, tool, authz.Action(tool))
}

// callerOn is [CodeDeps.caller] with whoever woke the turn decided on action
// rather than on the tool's own row: for a tool that is no code read itself
// but reads the code host as one — code_status, the health grant's
// (healthtools.go), whose hot list and one-repository form read repositories
// through the code tools' resolution — the requester is decided on the verb
// those reads are, the code host's ([authz.ActionCodeRead]), and NO FURTHER.
// The requester ceiling holds a repository read (ADR-0058), so every
// repository such a call reads is read for its requester as a code tool's is;
// what the tool reads beyond the repositories is its own row's, which the
// call's gate has already decided for the seat, and deciding that row again
// for the requester would make a ceiling of the tool's family that the family
// does not keep (fleet_health, its sibling, reads no repository and keeps
// none).
func (d CodeDeps) callerOn(ctx context.Context, turn *turnctx.Turn, tool string,
	action authz.Action) (codeCaller, *tools.Result) {

	actor, asker, refusal := d.asker(ctx, turn, tool)
	if refusal != nil {
		return codeCaller{}, refusal
	}
	c := codeCaller{actor: actor, asker: asker}
	if asker.Kind != code.PrincipalAgent {
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
		r.Output = ceilingRefused(tool, ceiling.Kind)
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

// ceilingRefused is what a call refused for whoever its turn is acting for is
// answered with, by who that is: a stranger, or a requester who may not act
// in the company.
func ceilingRefused(tool string, kind authz.CeilingKind) string {
	if kind == authz.CeilingStranger {
		return fmt.Sprintf("%s didn't run: this turn was woken by somebody the company does not know, and "+
			"a repository is read for somebody only where they could read it themselves, so you may "+
			"read none for them. A person or a seat of the company has to ask for it.", tool)
	}
	return fmt.Sprintf("%s didn't run: whoever this turn is working for may not act in the company now, "+
		"so they could not read its code themselves, and you may read none of it for them.", tool)
}

// ceilingCause is the cause of a read the code host refused for the seat a
// call reads for ([code.ErrAboveCeiling]): the authority table's own ceiling
// refusal over the domain's error, so a surface reading the decision finds the
// one reason every requester-ceiling refusal carries, and one reading the
// domain finds its sentinel.
func ceilingCause(tool string, err error) error {
	refused := &DecisionRefused{Action: authz.Action(tool), Decision: authz.Decision{Reason: authz.ReasonCeiling}}
	return fmt.Errorf("%w: %w", refused, err)
}

// read is q asked of the code host as c: THE ONE ReadQuery a call sends, so
// none can name its asker and forget whom it reads for.
func (d CodeDeps) read(ctx context.Context, c codeCaller, q code.ReadQuery) (code.ReadAnswer, error) {
	q.Asker, q.CeilingSeat = c.asker, c.ceiling
	return d.Code.Read(ctx, q)
}

// codeRepoKey is a repository a turn resolved, as the turn's lookups key it:
// who asked, and the name they asked by.
type codeRepoKey struct {
	asker code.Principal
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
	if !code.ValidName(name) {
		r := failed(fmt.Sprintf("%s: %q is not a repository's name — a name is lowercase letters, "+
			"digits and hyphens, as %s lists it.", tool, clip(name), CodeListReposTool))
		return "", &r
	}
	return name, nil
}

// repo is the repository named, resolved ONCE A TURN: the first call naming it
// reads it — kind `repo`, at the seat's level, which is where a repository the
// asker may not read is refused before any git node is asked — and every
// later call in the turn reuses the answer from the turn's lookups, its id
// above all, which a read by object id names the repository by. The operator
// surface has no turn, and resolves once a call. See [CodeDeps.repoRead],
// whose read a call can make its own.
func (d CodeDeps) repo(ctx context.Context, turn *turnctx.Turn, tool string, c codeCaller,
	name string) (code.RepoRef, *tools.Result) {

	ref, _, refusal := d.repoRead(ctx, turn, tool, c, name, nil)
	return ref, refusal
}

// repoRead is [CodeDeps.repo] whose read, on a turn's first call naming the
// repository, may be the call's OWN: first, where it is not nil, is a read
// whose answer carries the repository in detail — code_get_repo's first page
// of refs, held by the reader to carry it — made in place of the read of kind
// `repo` and answered back for the call to use, so that call is ONE
// linearizable read rather than the resolution's and then its own. Its asker,
// repository and level are this method's to set, never the call's. A hit
// answers no read: the call makes its own.
//
// THE ONE RESOLUTION OF A `repo` ARGUMENT, so whatever has to happen before a
// turn's first request about a repository happens in one place; every tool
// reads all its arguments first ([repoArg], [revArg]), so a malformed call is
// refused before it costs a read. The memo decides nothing about access: the
// node answering every later read judges the grant again, the asker's and the
// ceiling's. And it holds only while it is true: a later read that answers
// the name with another id ([CodeDeps.sameRepo]), or the id gone
// ([CodeDeps.failed]), forgets it.
//
// AND THE TURN'S HOLD, taken here on a MISS, before the call sends anything
// else about the repository ([HoldRepo]): a scratch repository the turn names
// is never expired while the turn is unended (M3-decisions M3-O61). Taken
// after the read that resolved the name — the hold is ON the id the name
// resolves to, and decided at that read's position — and before the memo
// keeps the resolution, so a resolution the turn reuses is always one its
// hold was taken for: a hold that failed keeps nothing, the call is refused
// with nothing more sent, and the next call naming the repository resolves
// it, and takes its hold, again. A HIT takes none: its hold was taken with
// it, and a turn's holds only grow.
func (d CodeDeps) repoRead(ctx context.Context, turn *turnctx.Turn, tool string, c codeCaller,
	name string, first *code.ReadQuery) (code.RepoRef, *code.ReadAnswer, *tools.Result) {

	if held := lookedUpRepo(turn, c.asker, name); held != nil {
		return *held, nil, nil
	}
	q := code.ReadQuery{Kind: code.ReadRepo}
	if first != nil {
		q = *first
	}
	q.Repo, q.Fresh = name, seatRead
	answer, err := d.read(ctx, c, q)
	if err != nil {
		r := codeFailure(ctx, tool, name, err)
		return code.RepoRef{}, nil, &r
	}
	if answer.Repo == nil {
		r := faulted(ctx, tool, fmt.Errorf("builtin: the code host answered repository %s's %s read with "+
			"no detail", name, q.Kind), fmt.Sprintf("%s could not read repository %q: %s.", tool, name, faultSaid))
		return code.RepoRef{}, nil, &r
	}
	ref := answer.Repo.RepoRef
	if err := HoldRepo(ctx, turn, d.Holder, c.actor, ref, d.Now); err != nil {
		r := holdFailure(ctx, tool, ref, err)
		return code.RepoRef{}, nil, &r
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
func (d CodeDeps) sameRepo(turn *turnctx.Turn, tool string, asker code.Principal, repo,
	answered code.RepoRef) *tools.Result {

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

// failed explains a read about repo — the repository the turn resolved — that
// did not answer ([codeFailure]), and where the read answered the repository
// GONE (code.ErrRepoNotFound) forgets the turn's resolution of its name first,
// as [CodeDeps.sameRepo] does for a name that answers another id: the
// resolution is no longer true, whichever read found it out — one by name, or
// one by the id the turn resolved, which no read by name ever re-checks. A name
// purged and declared again names a repository of another id, so the next call
// naming it resolves it afresh rather than asking again, for the rest of the
// turn, about an id nothing serves.
func (d CodeDeps) failed(ctx context.Context, turn *turnctx.Turn, tool string, asker code.Principal,
	repo code.RepoRef, err error) tools.Result {

	if errors.Is(err, code.ErrRepoNotFound) {
		forget(turn, asker, repo)
	}
	return codeFailure(ctx, tool, repo.Name, err)
}

// lookedUpRepo is the turn's resolution of name as asker resolved it — one
// [CodeDeps.repoRead] kept, its hold taken — or nil where the turn holds none
// (or there is no turn).
func lookedUpRepo(turn *turnctx.Turn, asker code.Principal, name string) *code.RepoRef {
	held, ok := turn.LookedUp().Load(codeRepoKey{asker: asker, name: name})
	if !ok {
		return nil
	}
	ref, ok := held.(code.RepoRef)
	if !ok {
		return nil
	}
	return &ref
}

// forget drops the turn's resolution of repo's name, as asker asked it.
func forget(turn *turnctx.Turn, asker code.Principal, repo code.RepoRef) {
	turn.LookedUp().Forget(codeRepoKey{asker: asker, name: repo.Name})
}

// resolveRef is the NAMES a call gives as refs of repo, every one of them
// resolved in ONE code.resolve_ref at the seat's level — linearizable, the one
// read about refs that pays a barrier, so a call naming two pays one barrier
// and one round trip rather than one each, and reads both at one checkpoint:
// a comparison compares them as they stood at the same moment. It answers each
// name's row in the names' order — its oid, its whole name and where it last
// moved, which a read by object id is then floored at ([resolvedRev.at]) — and
// the checkpoint they were read at; a name no live ref holds refuses the call
// in the code host's own sentence, every such name named. Its read is THE ONE
// code.RefQuery in this package ([CodeDeps.refsOf]): code_resolve's `ref`
// calls it directly, [CodeDeps.resolveRevs] for every revision of a call that
// is not an id, a write's branch ([CodeDeps.branch]) and a coding run's launch
// every ref it resolves ([CodeDeps.ResolveRunRepos]) — and so the one place a
// call's caller and ceiling are stamped on a resolve, and the turn's hold on
// every seat's branch it answers is taken ([HoldRef]).
func (d CodeDeps) resolveRef(ctx context.Context, turn *turnctx.Turn, tool string, c codeCaller,
	repo code.RepoRef, names ...string) ([]code.ResolvedRef, statelog.Position, *tools.Result) {

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
// beside the rest ([code.ResolvedRef.Missing]) rather than refusing the call:
// for the callers to which a missing ref is an answer — a coding run's launch,
// whose own branch may not exist yet ([CodeDeps.ResolveRunRepos]), and a
// write's branch, which its own head compare answers ([CodeDeps.branch]). The
// repository the answer names is the one the turn resolved, or the call is
// refused ([CodeDeps.sameRepo]).
//
// AND THE TURN'S HOLD on every seat's branch it resolved ([HoldRef]), taken
// here — after the read whose answer a hold is decided from, and before the
// answer goes back to whatever sends a request through it — so no caller can
// read a ref for a turn and forget its hold. Every key of the answer is taken
// in ONE Take first ([takeRefHolds]): a call that resolves a comparison's two
// seats' branches writes one compare-and-set on its run row, not two. A hold
// that fails refuses the call with nothing sent.
func (d CodeDeps) refsOf(ctx context.Context, turn *turnctx.Turn, tool string, c codeCaller,
	repo code.RepoRef, names ...string) (code.RefAnswer, *tools.Result) {

	answer, err := d.Code.ResolveRef(ctx, code.RefQuery{Repo: repo.Name, Refs: names, Fresh: seatRead,
		Asker: c.asker, CeilingSeat: c.ceiling})
	if err != nil {
		r := d.failed(ctx, turn, tool, c.asker, repo, err)
		return code.RefAnswer{}, &r
	}
	if r := d.sameRepo(turn, tool, c.asker, repo, answer.Repo); r != nil {
		return code.RefAnswer{}, r
	}
	if len(answer.Refs) != len(names) {
		r := faulted(ctx, tool, fmt.Errorf("builtin: the code host answered %d of the %d refs asked of %s",
			len(answer.Refs), len(names), repo.Name), fmt.Sprintf("%s could not read repository %q: %s.", tool,
			repo.Name, faultSaid))
		return code.RefAnswer{}, &r
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
		return code.RefAnswer{}, &r
	}
	for i := range answer.Refs {
		resolved := &answer.Refs[i]
		if resolved.Missing != "" {
			continue
		}
		if err := HoldRef(ctx, turn, d.Holder, c.actor, repo, resolved.Ref, resolved, answer.Position,
			d.Now); err != nil {
			r := refHoldFailure(ctx, tool, repo, resolved.Ref, err)
			return code.RefAnswer{}, &r
		}
	}
	return answer, nil
}

// branch holds refs — the branch a write tool's call names, refs/heads/ and its
// `branch`, the source branch of the change a suggestions apply commits to, or
// the source and the target of a change it opens — for turn BEFORE the write
// is sent ([HoldRef]): THE ONE PLACE a write's branch is held, which every
// engine write calls before its GitWrite. ALL OF A CALL'S BRANCHES AT ONCE:
// one ref read for every branch it resolves, and one Take for every branch it
// holds with no read ([takeRefHolds]), so a change between two seats' branches
// pays one barrier and one compare-and-set on the turn's run row, not two of
// each.
// A WRITE IS HELD AS A READ IS, because only a write that MOVES the branch is
// ordered against a deletion of it by its own head compare: one that moves
// nothing — refused, a revert or a pick that conflicts, a no-op — appends no
// record. So a write naming the branch's head (existing: its expected_head
// given) resolves it through the one ref read ([CodeDeps.refsOf]), which holds
// it with the answer, as a read is held — and a branch the resolve finds
// missing, or one the write CREATES, is held with no read: no deletion can
// have listed what does not exist, and the write sets it now.
//
// RESOLVED ONLY WHERE A HOLD WOULD BE TAKEN AND IS NOT YET CONFIRMED
// (M3-decisions M3-O65): a turn's call, by a seat, on a branch
// [code.Holdable] admits, whose key the turn has not confirmed. Every other
// write — an operator's, one on a branch outside refs/heads/seats/, one on a
// branch this turn's read already confirmed — reads nothing here; its own
// head compare decides it, unchanged.
func (d CodeDeps) branch(ctx context.Context, turn *turnctx.Turn, tool string, c codeCaller, repo code.RepoRef,
	existing bool, refs ...string) *tools.Result {

	if turn == nil || c.asker.Kind != code.PrincipalAgent {
		return nil
	}
	var held []string
	for _, ref := range refs {
		if code.Holdable(ref) && !slices.Contains(held, ref) &&
			!turn.Holding().Confirmed(code.RefHoldKey(repo.ID, ref)) {
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

// refsRead is the furthest position on the code log at which a ref this turn
// resolved on one repository last MOVED — code_resolve's ref, and every ref a
// call named — which a seat's engine write of the repository is floored at
// ([CodeDeps.writeFresh]): a turn writes onto a head it read and names commits
// it read, and a copy synced no further than the turn's first read of the
// repository may hold neither, so its write is judged on a copy that lacks
// them and judged again at the log's head — a barrier and a second sync, and
// for a head the copy has not seen move, a data node's read of the write's
// own outcome first. Kept by the turn ([turnctx.Lookups], taken with
// LoadOrStore) and only ever raised, under its own lock, since the turn's
// workers resolve refs while it runs; a FACT about what the turn read, never a
// resolution: it decides nothing, and outlives a name the turn forgets
// ([forget]), since what was read stays read.
type refsRead struct {
	mu sync.Mutex
	at statelog.Position
}

// noteRefsRead raises turn's [refsRead] on repoID to where each of refs last
// moved; a ref no live ref holds, which moved nowhere, raises nothing.
func noteRefsRead(turn *turnctx.Turn, repoID string, refs []code.ResolvedRef) {
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
// object id — whole, or an abbreviation the git node expands — and, where the
// argument was a ref, the ref it resolved through and the checkpoint that
// read was at. A read of it is floored at the record that last moved its ref
// (code.RefsFloor), and an id's at nothing of its own.
type resolvedRev struct {
	oid  string
	ref  *code.ResolvedRef
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

// resolveRevs is a call's revision arguments as oids, in their order: an
// object id, whole or abbreviated, passes with no read (the git node expands
// an abbreviation from the objects alone, never as a ref); `HEAD` is the
// repository's default branch; every other is a branch, a tag or a ref, and
// ALL OF THEM are resolved in the one read of [CodeDeps.resolveRef] — a name
// given twice, once — so a call pays one barrier however many refs it names.
// A branch whose name is all hex digits is named by its whole ref.
func (d CodeDeps) resolveRevs(ctx context.Context, turn *turnctx.Turn, tool string, c codeCaller,
	repo code.RepoRef, revs ...string) ([]resolvedRev, *tools.Result) {

	out := make([]resolvedRev, len(revs))
	refOf := make([]string, len(revs))
	var names []string
	for i, rev := range revs {
		switch {
		case code.ValidRev(rev):
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

// gitFresh is what a read by object id of repo is asked with: the furthest of
// this node's own floor on the repository and the positions the call's refs
// last moved at — a SYNC level for the copy that answers, so it holds the
// commits those refs named and this node's own pushes to it — or none, which
// reads the copy as it stands: the code domain's one rule (code.RefsFloor),
// which a question's read by object id is floored by too.
func (d CodeDeps) gitFresh(repo code.RepoRef, revs ...resolvedRev) statelog.Freshness {
	var refs []code.ResolvedRef
	for _, r := range revs {
		if r.ref != nil {
			refs = append(refs, *r.ref)
		}
	}
	return code.RefsFloor(d.Code.FloorOf(repo.ID), refs...)
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

// gitRead asks q of repo's home git node as c — THE ONE GitQuery a call sends,
// stamped here with its caller, its ceiling and a seat's turn, so no tool can name its asker
// and forget whom it reads for — refused here first where the query is one no
// git node would take, so a malformed argument costs no round trip and is
// told in the domain's own words. A git node that answers the repository gone
// ends the turn's resolution of its name ([CodeDeps.failed]).
func (d CodeDeps) gitRead(ctx context.Context, turn *turnctx.Turn, tool string, c codeCaller, repo code.RepoRef,
	q code.GitQuery) (code.GitAnswer, *tools.Result) {

	q.Asker, q.CeilingSeat = c.asker, c.ceiling
	// A SEAT'S READ NAMES ITS TURN, whose bucket on the answering git node
	// pays the read's one second look at the log's head (code.NewResyncs):
	// so one turn naming what its floor lacks in a loop spends its own
	// re-syncs and, past its seat's, the seat's — never the code log's.
	if turn != nil && c.asker.Kind == code.PrincipalAgent {
		q.Turn = turn.RunID
	}
	if err := q.Validate(); err != nil {
		r := codeFailure(ctx, tool, repo.Name, err)
		return code.GitAnswer{}, &r
	}
	answer, err := d.Code.GitRead(ctx, q)
	if err != nil {
		r := d.failed(ctx, turn, tool, c.asker, repo, err)
		return code.GitAnswer{}, &r
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

// codeGitAnswer is a read by object id as a code tool answers it: the git
// node's page whole — the object it read, the copy's position, the cursor of
// the next page, the push behind each commit it names — beside the repository
// by name and every ref the call named, by the argument that named it.
type codeGitAnswer struct {
	Repo     string                  `json:"repo"`
	Resolved map[string]codeResolved `json:"resolved,omitempty"`
	code.GitAnswer
}

// gitAnswer is the answer to a read by object id over repo, the refs resolved
// by argument name.
func gitAnswer(repo code.RepoRef, page code.GitAnswer, revs map[string]resolvedRev) codeGitAnswer {
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

// codeFailure explains a code read that did not answer, each sentinel the
// domain answered in its own class and its own sentence — composed for the
// caller, naming what to change — and, past them, a condition of the node or
// the fleet as unavailable (waiting clears it) and anything else as this
// node's fault.
//
// A NODE THAT CANNOT JUDGE IS NEVER A REFUSAL OF THE CALLER: a data node with
// no chart answers a seat's read about itself (code.ErrNotServing), a git node
// unready (code.ErrGitUnready), and the router asks another — so what reaches
// here from them is the fleet having no node that could answer now, which a
// retry clears. Only code.ErrNotPermitted is a decision about who asked, and
// code.ErrAboveCeiling about whom the asker reads for.
func codeFailure(ctx context.Context, tool, repo string, err error) tools.Result {
	var (
		unready  *code.ErrGitUnready
		unserved *estate.ErrUnserved
	)
	switch {
	case errors.Is(err, code.ErrAboveCeiling):
		// THE CEILING'S OWN SENTENCE: the code host names the seat this
		// turn is working for, why it reads nothing of the repository and who
		// does. That the asker could read it is no reason to: read for that
		// seat, it would be the visibility set aside for whoever can message
		// a seat that reads it — the refusal is the requester ceiling's, in
		// the artifacts tools' terms.
		said, ok := RefusalSentence(err, code.ErrAboveCeiling)
		if !ok {
			said = fmt.Sprintf("the seat this turn is working for may not read repository %q", repo)
		}
		return refusedBy(tools.RefusalForbidden, ceilingCause(tool, err), fmt.Sprintf("%s didn't run: the "+
			"seat this turn is working for could not read this itself — %s — so you may not read it for them. "+
			"That is a decision about whom you are reading for, not about the repository or the node, so the "+
			"same call is refused again in this turn: tell them who does read it.", tool, said))
	case errors.Is(err, code.ErrNotPermitted):
		// THE GRANT'S OWN SENTENCE names the visibility that shut the asker
		// out and who does read the repository; nothing here suggests a way
		// round it, since another seat reading it for this one would be the
		// visibility set aside rather than answered.
		said, ok := RefusalSentence(err, code.ErrNotPermitted)
		if !ok {
			said = fmt.Sprintf("your grant does not let you read repository %q", repo)
		}
		return refusedBy(tools.RefusalForbidden, err, fmt.Sprintf("%s: %s. That is a decision about "+
			"who is asking, not about the repository or the node, so the same call is refused again.",
			tool, said))
	case errors.Is(err, code.ErrRepoNotFound):
		said, ok := RefusalSentence(err, code.ErrRepoNotFound)
		if !ok || said == repo {
			said = fmt.Sprintf("there is no repository %q", repo)
		}
		return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("%s: %s. List the repositories you "+
			"can read with %s.", tool, said, CodeListReposTool))
	case errors.Is(err, code.ErrRefNotFound):
		said, _ := RefusalSentence(err, code.ErrRefNotFound)
		return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("%s: %s. Its branches are listed by "+
			"%s; a commit is named by its id.", tool, said, CodeGetRepoTool))
	case errors.Is(err, code.ErrChangeNotFound):
		// THE CHANGE IT NAMES, of a repository the asker reads: a number of
		// one it may not read is never this answer (code.ErrNotPermitted).
		said, ok := RefusalSentence(err, code.ErrChangeNotFound)
		if !ok {
			said = "of that number"
		}
		return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("%s: repository %q has no change %s. Its "+
			"changes are listed by %s.", tool, repo, said, CodeListChangesTool))
	case errors.Is(err, code.ErrObjectNotFound), errors.Is(err, code.ErrNotATree),
		errors.Is(err, code.ErrNotABlob):
		return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("%s: %s.", tool, domainWords(err)))
	case errors.Is(err, code.ErrSliceTooLarge), errors.Is(err, code.ErrInvalid),
		errors.Is(err, code.ErrBadCursor), errors.Is(err, code.ErrBadLevel):
		return refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s refused what it was asked: %s. "+
			"Change the arguments — the same ones are refused again.", tool, domainWords(err)))
	case errors.As(err, &unready), errors.Is(err, code.ErrNotServing):
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s could not read repository %q "+
			"right now (no node holding it could answer in time). This is NOT an answer about the "+
			"repository: try again shortly.", tool, repo))
	case errors.As(err, &unserved) && unserved.Git:
		// THE GIT NODES' WALK, which Condition's words — about the data
		// nodes, which were serving — would misname.
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s could not read repository %q "+
			"right now (none of its git nodes could answer in time — its home may still be filling its "+
			"copy). This is NOT an answer about the repository: try again shortly.", tool, repo))
	}
	if why, ok := Condition(err); ok {
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s could not read repository %q "+
			"right now (%s). This is NOT an answer about the repository: try again shortly.", tool,
			repo, why))
	}
	return faulted(ctx, tool, err, fmt.Sprintf("%s could not read repository %q: %s. This is NOT an "+
		"answer about the repository: say you could not read it.", tool, repo, faultSaid))
}

// domainWords is a code refusal's own sentence: the words its domain wrote for
// the caller past the sentinel, whichever sentinel it carries.
func domainWords(err error) string {
	for _, sentinel := range []error{code.ErrObjectNotFound, code.ErrNotATree, code.ErrNotABlob,
		code.ErrSliceTooLarge, code.ErrInvalid, code.ErrBadCursor, code.ErrBadLevel} {
		if said, ok := RefusalSentence(err, sentinel); ok {
			return said
		}
	}
	return err.Error()
}

// RefusalSentence is the sentence err carries past its sentinel's own words —
// `code: the caller's grant does not cover this: <sentence>` read as the
// sentence — wherever in the chain the sentinel was wrapped, since a refusal
// another node answered arrives wrapped by the estate router as well; false
// where the sentinel carries none.
//
// EXPORTED for the code host's REST reads, which answer the same refusals of
// the same reader: one reading of where a domain's sentence starts, so a
// person's surface and a seat's tool quote the same words.
func RefusalSentence(err, sentinel error) (string, bool) {
	_, said, ok := strings.Cut(err.Error(), sentinel.Error()+": ")
	said = strings.TrimSpace(said)
	return said, ok && said != ""
}

// codeRepoArg and codeCursorArg are the arguments every code tool's schema
// shares, and codeRevArg the one each revision argument is described by —
// each a fresh map for every schema it is put in, so a consumer that writes
// into one tool's schema writes into no other's.
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

// codeRevArg describes a revision argument by its role alone, and points to
// [revisionGrammar] in code_resolve's description for what a revision is.
//
// THE GRAMMAR IS STATED ONCE: every round of every seat of a company hosting
// code carries the catalogue uncached, and the grammar, written into each of
// the six arguments that take a revision, was a sixth of its eleven
// kilobytes — the same sentence, near three hundred bytes rendered, six times.
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
				"enum": []string{string(code.RepoStatusImporting), string(code.RepoStatusActive),
					string(code.RepoStatusArchived)},
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
// by and decides about — never the counts and ids that are the code host's own
// bookkeeping.
type codeRepoRow struct {
	Name          string          `json:"name"`
	Description   string          `json:"description,omitempty"`
	Status        code.RepoStatus `json:"status"`
	Owner         code.Owner      `json:"owner"`
	Visibility    code.Visibility `json:"visibility"`
	Search        code.SearchMode `json:"search"`
	Access        string          `json:"access"`
	DefaultBranch string          `json:"default_branch"`
	Head          string          `json:"head,omitempty"`
	Pushes        int64           `json:"pushes"`
	LastPushAt    time.Time       `json:"last_push_at,omitzero"`
	// Protections are its rules as a push is held to them, as its row
	// answers them: never a rule's merge gate, which code_get_repo answers.
	Protections []code.PushRule `json:"protections,omitempty"`
	// ProtectionsLeftOut is set, in place of Protections, on the one row of
	// a listing whose rules alone are past an answer: how many there are and
	// which tool pages them.
	ProtectionsLeftOut string `json:"protections_left_out,omitempty"`
}

func repoRowOf(s code.RepoSummary) codeRepoRow {
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
	q := code.ReadQuery{Kind: code.ReadRepos, Cursor: strings.TrimSpace(argString(args, "cursor")),
		Fresh: seatRead}
	switch status := code.RepoStatus(strings.TrimSpace(argString(args, "status"))); status {
	case "":
	case code.RepoStatusImporting, code.RepoStatusActive, code.RepoStatusArchived:
		q.Status = []code.RepoStatus{status}
	default:
		return failed(fmt.Sprintf("%s: `status` is %s, %s or %s, not %q.", t.Name(), code.RepoStatusImporting,
			code.RepoStatusActive, code.RepoStatusArchived, clip(string(status)))), nil
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
	// A PAGE CUT WHOLE-ROW AT THE ANSWER: the reader pages by its own bound,
	// and a repository's rules can make a page of them outgrow one tool
	// answer. The listing is keyset by name, so the next page begins after
	// the last row kept.
	page := func(k int) any {
		next := answer.Next
		if k > 0 && k < len(rows) {
			next = rows[k-1].Name
		}
		return codeRepoList{Repos: rows[:k], Next: next, Position: answer.Position}
	}
	read := newForeignRead(turn, c.asker.Seat)
	k, rendered, err := read.fit(len(rows), page)
	if err == nil && k == 0 && len(rows) > 0 {
		// A REPOSITORY WHOSE RULES ALONE ARE PAST ONE ANSWER is listed without
		// them, saying so, and the page ends at it. Answered as a refusal with
		// no `next`, as it once was, it ended the listing there for every
		// seat, and every repository after it by name was listed by none.
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
		"holds a merge to, its size, how many branches and tags it has, your access, its last push and, "+
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

// codeRepoDetail is one repository in detail, as code_get_repo answers it.
type codeRepoDetail struct {
	codeRepoRow
	ManagedBy code.ManagedBy `json:"managed_by"`
	Refs      int64          `json:"refs"`
	Branches  int64          `json:"branches"`
	Tags      int64          `json:"tags"`
	LivePacks int64          `json:"live_packs"`
	LiveBytes int64          `json:"live_bytes"`
	CreatedAt time.Time      `json:"created_at,omitzero"`
	DeletedAt time.Time      `json:"deleted_at,omitzero"`
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
	// Protections are its rules WHOLE, each with the merge gate it holds a
	// merge to: what its detail's row answers of each rule beside the gates
	// the read asked for (code.ReadQuery.Gates), on the detail's page alone,
	// in pattern order — those that fit, and ProtectionsNext the pattern of
	// the last answered where more follow it ([codeRepoRules]).
	Protections     []code.Protection `json:"protections,omitempty"`
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
	Protections     []code.Protection `json:"protections"`
	ProtectionsNext string            `json:"protections_next,omitempty"`
	Position        statelog.Position `json:"position"`
}

// wholeRules is a repository's rules with their merge gates: the rules its
// row answers, each given the gate its detail answered for its pattern —
// gates read beside the rules, in one transaction, so every gate names a rule
// of the row ([code.RepoDetail.Gates]). Only for a detail read that asked for
// the gates: on one that did not, every rule would read as ungated.
func wholeRules(rules []code.PushRule, gates []code.RuleGate) []code.Protection {
	gateOf := make(map[string]code.MergeGate, len(gates))
	for _, g := range gates {
		gateOf[g.Pattern] = g.Gate
	}
	out := make([]code.Protection, len(rules))
	for i, r := range rules {
		out[i].PushRule = r
		if g, ok := gateOf[r.Pattern]; ok {
			out[i].Gate = &g
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
	q := code.ReadQuery{Kind: code.ReadRefs, Limit: codeBranchesPage,
		Cursor: strings.TrimSpace(argString(args, "branches_cursor"))}
	if after := strings.TrimSpace(argString(args, "protections_cursor")); after != "" {
		if q.Cursor != "" {
			return failed(fmt.Sprintf("%s: `protections_cursor` and `branches_cursor` continue two lists — "+
				"pass one of them.", t.Name())), nil
		}
		return t.rulesAfter(ctx, turn, c, name, after)
	}
	// A NAMESPACE UNDER refs/heads/, however the model spells it — with or
	// without the root, with or without the closing slash the listing's
	// prefix ends in.
	ns := strings.TrimPrefix(strings.TrimSpace(argString(args, "branches_prefix")), branchRoot)
	if ns = strings.Trim(ns, "/"); ns != "" {
		q.Prefix = branchRoot + ns + "/"
	}
	// THE FIRST PAGE CARRIES THE REPOSITORY'S DETAIL, so on a turn's first
	// call naming it that page IS the resolution: one linearizable read, not
	// a read of kind repo and then the page. A later page carries none, and a
	// turn whose first word of a repository is one resolves it by its own.
	// The detail's page asks for its rules' merge gates, which this tool
	// answers and no other resolution reads.
	var first *code.ReadQuery
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
	var rules []code.Protection
	if d := answer.Repo; d != nil {
		if r := t.deps.sameRepo(turn, t.Name(), c.asker, repo, d.RepoRef); r != nil {
			return *r, nil
		}
		out.Detail = &codeRepoDetail{codeRepoRow: repoRowOf(d.RepoSummary), ManagedBy: d.ManagedBy,
			Refs: d.Refs, Branches: d.Branches, Tags: d.Tags, LivePacks: d.LivePacks,
			LiveBytes: d.LiveBytes, CreatedAt: d.CreatedAt, DeletedAt: d.DeletedAt}
		// THE RULES ONCE, WHOLE: beside the detail rather than in its row,
		// which would answer each rule's push half a second time.
		out.Detail.Protections = nil
		rules = wholeRules(d.Protections, d.Gates)
		if c.asker.Kind == code.PrincipalOperator && t.deps.CloneURL != nil {
			out.CloneURL = t.deps.CloneURL(repo.Name)
		}
	}
	branches := make([]codeBranch, len(answer.Refs))
	for i, ref := range answer.Refs {
		branches[i] = codeBranch{Name: strings.TrimPrefix(ref.Ref, branchRoot), OID: ref.OID}
	}
	// RULES AND BRANCHES CUT WHOLE AT THE ANSWER, each where its own list
	// continues: the rules are in pattern order, so the rest of them begin
	// after the last rule kept, and the refs listing is keyset by the ref, so
	// the next page of branches begins after the last branch kept.
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
	// THE RULES FIRST, leaving room for one branch, and then the branches in
	// what the rules leave: at the domain's bounds a repository's rules are
	// half a mebibyte with their gates, far past one answer, and cut at the
	// answer with no room kept, a repository that wide would answer no branch
	// on its first page. A rule at its widest — its pattern and its gate, ten
	// kilobytes — and a branch at its widest beside the detail at its widest
	// fit one answer with room to spare, so each list makes progress on every
	// page (TestEveryRepositoryAtTheDomainsBoundsIsAnswered).
	read := newForeignRead(turn, c.asker.Seat)
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

// rulesAfter is code_get_repo's answer to a `protections_cursor`: the
// repository's rules whose patterns sort after after, whole and in pattern
// order, cut whole-rule at the answer — keyset by pattern, so the rest of the
// rules follow a rule removed meanwhile, and a rule added before the cursor is
// answered by a first page.
//
// READ AS THE FIRST PAGE IS: a refs listing's first page carries the
// repository's detail and, asked, its rules' gates — of one ref here, which
// this answer does not present — so a turn's first call naming the
// repository is its resolution too ([CodeDeps.repoRead]), and every later one
// reads by the kind this tool reads, never a second resolution of its own.
func (t *codeGetRepo) rulesAfter(ctx context.Context, turn *turnctx.Turn, c codeCaller, name,
	after string) (tools.Result, error) {

	q := code.ReadQuery{Kind: code.ReadRefs, Limit: 1, Gates: true}
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
		return faulted(ctx, t.Name(), fmt.Errorf("builtin: the code host answered repository %s's read with "+
			"no detail", repo.Name), fmt.Sprintf("%s could not read repository %q: %s.", t.Name(), repo.Name,
			faultSaid)), nil
	}
	if r := t.deps.sameRepo(turn, t.Name(), c.asker, repo, d.RepoRef); r != nil {
		return *r, nil
	}
	rules := wholeRules(d.Protections, d.Gates)
	if i := slices.IndexFunc(rules, func(p code.Protection) bool { return p.Pattern > after }); i >= 0 {
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

// Description says what the tool is FOR: turning a ref into its commit id once,
// so the calls after it name the commit. Never that it is the one read that
// waits for the newest push — every code read of a row is linearizable, the
// repositories' listing and detail and every ref a call names alike — which
// taught the model that naming a branch anywhere else was free. And it states
// the revision grammar every other code tool's revision argument points to.
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
	q := code.GitQuery{RepoID: repo.ID, Fresh: t.deps.gitFresh(repo, rev),
		Cursor: strings.TrimSpace(argString(args, "cursor"))}
	if recursive {
		q.Kind, q.Tree = code.GitTree, &code.TreeQuery{Rev: rev.oid, Path: path, PathB64: pathB64,
			Recursive: true}
	} else {
		q.Kind, q.Blob = code.GitBlob, &code.BlobQuery{Rev: rev.oid, Path: path, PathB64: pathB64,
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
		read.fence(fieldPath{}.key("blob").key("text"), provenance.SourceRepoFile, nil, false, b.Text)
		narrow = blobNarrowing(b)
	}
	return read.page(ctx, t.Name(), gitAnswer(repo, page, map[string]resolvedRev{"rev": rev}), narrow)
}

// blobNarrowing is what a page of a file's lines is refused naming when its
// fenced text outgrows one answer — text spelling fence markers, each longer
// once escaped: the first half of the page's lines, or, for a page that is
// one line, that the line cannot be read whole here.
func blobNarrowing(b *code.BlobAnswer) string {
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
		"status and its added and deleted lines, filtered by `paths` — or one file's patch (`file`), a " +
		"page at a time. A patch arrives fenced as content nobody vouches for: the two fence lines " +
		"are a label, not part of the patch."
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
					"directory, at most %d.", code.MaxDiffPaths),
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
	if v, set := args["renames"].(bool); set {
		renames = v
	}
	diff := &code.DiffQuery{Base: base.oid, Head: head.oid, Paths: paths, File: argString(args, "file"),
		FileB64: argString(args, "file_b64"), Renames: renames}
	page, refusal := t.deps.gitRead(ctx, turn, t.Name(), c, repo, code.GitQuery{Kind: code.GitDiff,
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
		read.fence(fieldPath{}.key("diff").key("patch"), provenance.SourceRepoPatch, nil, false, d.Patch)
		narrow = fmt.Sprintf("The patch spells fence markers, which grow when they are escaped: read "+
			"the file's lines at either commit with %s and a line range instead.", CodeReadTool)
	}
	return read.page(ctx, t.Name(), gitAnswer(repo, page, map[string]resolvedRev{"base": base, "head": head}),
		narrow)
}

// argPaths reads a list of paths EXACTLY as written — never trimmed and never
// split on a comma, which a path may hold — tolerating the single string a
// model sends where a list belongs; false for a value that is neither.
func argPaths(args map[string]any, key string) ([]string, bool) {
	switch v := args[key].(type) {
	case nil:
		return nil, true
	case string:
		return []string{v}, true
	case []string:
		return v, true
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
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
	page, refusal := t.deps.gitRead(ctx, turn, t.Name(), c, repo, code.GitQuery{Kind: code.GitCompare,
		RepoID: repo.ID, Fresh: t.deps.gitFresh(repo, base, head),
		Compare: &code.CompareQuery{Base: base.oid, Head: head.oid}})
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
	case code.ValidRev(target):
		return failed(fmt.Sprintf("%s: `target` is the branch asked about, and %q reads as a commit "+
			"id; a branch whose name is all hex digits is named by its whole ref (refs/heads/<name>).",
			t.Name(), clip(target))), nil
	}
	repo, refusal := t.deps.repo(ctx, turn, t.Name(), c, name)
	if refusal != nil {
		return *refusal, nil
	}
	// THE COMMIT AND THE TARGET IN ONE READ: the target is never an id (it
	// was refused above), so it resolves as a ref — HEAD the default branch —
	// beside the commit where that is a ref too.
	revs, refusal := t.deps.resolveRevs(ctx, turn, t.Name(), c, repo, commitName, target)
	if refusal != nil {
		return *refusal, nil
	}
	commit, tip := revs[0], revs[1]
	// THE TIP'S OWN MOVE, never where the resolve read it: the target's moves
	// up to the one that put the tip there are every move it had when it was
	// resolved, which is all the index must hold.
	page, refusal := t.deps.gitRead(ctx, turn, t.Name(), c, repo, code.GitQuery{Kind: code.GitLandedIn,
		RepoID: repo.ID, Fresh: t.deps.gitFresh(repo, commit, tip),
		LandedIn: &code.LandedInQuery{Commit: commit.oid, Target: tip.ref.Ref, TargetOID: tip.oid,
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

// Description says what the history is for — "when and why did this change",
// answered on the server — so a model reaches for it before a clone, and how
// its two narrowings read. How a page goes on is `cursor`'s to say, as every
// paged code tool leaves it: the catalogue is paid again every round.
func (t *codeLog) Description() string {
	return "The commits that changed a file or directory, or every commit with no `path`, newest first from a " +
		"commit: each with its parents, author, dates, subject and the push that introduced it, read on the " +
		"server. Use it for when and why something changed, never a clone. `grep` keeps commits whose message " +
		"holds its words (a literal, any case); `since` those committed from then on. Subjects arrive fenced " +
		"as content nobody vouches for."
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
					"bytes.", code.MaxLogGrepBytes),
			},
			"since": map[string]any{
				"type":        "string",
				"description": "Only commits committed at or after this time, in RFC 3339 (`2026-10-01T00:00:00Z`).",
			},
			"limit": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf("How many commits a page lists, 1 to %d; %d by default.",
					code.MaxLogLimit, code.DefaultLogLimit),
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
	q := &code.LogQuery{Path: argString(args, "path"), PathB64: argString(args, "path_b64"),
		Grep: argString(args, "grep"), Limit: argInt(args, "limit", 0)}
	if v, set := args["limit"]; set && v != nil && (q.Limit < 1 || q.Limit > code.MaxLogLimit) {
		return failed(fmt.Sprintf("%s: `limit` is 1 to %d.", t.Name(), code.MaxLogLimit)), nil
	}
	if s := strings.TrimSpace(argString(args, "since")); s != "" {
		since, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return failed(fmt.Sprintf("%s: `since` is a time in RFC 3339, such as 2026-10-01T00:00:00Z — "+
				"not %q.", t.Name(), clip(s))), nil
		}
		q.Since = since
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
	q.Rev = rev.oid
	page, refusal := t.deps.gitRead(ctx, turn, t.Name(), c, repo, code.GitQuery{Kind: code.GitLog,
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
			read.fence(fieldPath{}.key("log").key("commits").index(i).key("subject"), provenance.SourceRepoCommit,
				nil, false, commit.Subject)
		}
	}
	return read.page(ctx, t.Name(), gitAnswer(repo, page, map[string]resolvedRev{"rev": rev}),
		"Ask for fewer commits with `limit`, or narrow the history with `path`, `grep` or `since`.")
}

// ---- code_blame --------------------------------------------------------- //

type codeBlame struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeBlame)(nil)

func (t *codeBlame) Name() string { return CodeBlameTool }

func (t *codeBlame) Description() string {
	return fmt.Sprintf("Which commit last changed each line of a file, read on the server: each line with its "+
		"commit and its line there, and each commit once with its author, date, subject and the push behind "+
		"it; at most %d lines a call. Lines and subjects arrive fenced as content nobody vouches for.",
		code.MaxBlameLines)
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
					"when left out.", code.MaxBlameLines-1),
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
	page, refusal := t.deps.gitRead(ctx, turn, t.Name(), c, repo, code.GitQuery{Kind: code.GitBlame,
		RepoID: repo.ID, Fresh: t.deps.gitFresh(repo, rev), Cursor: strings.TrimSpace(argString(args, "cursor")),
		Blame: &code.BlameQuery{Rev: rev.oid, Path: path, PathB64: pathB64,
			StartLine: argInt(args, "start_line", 0), EndLine: argInt(args, "end_line", 0)}})
	if refusal != nil {
		return *refusal, nil
	}
	read := newForeignRead(turn, c.asker.Seat)
	if b := page.Blame; b != nil {
		// A LINE IS THE FILE'S TEXT and a subject its pusher's words, which
		// nobody this read can name vouches for: each fenced, at the least
		// trust.
		blame := fieldPath{}.key("blame")
		for i, line := range b.Lines {
			read.fence(blame.key("lines").index(i).key("text"), provenance.SourceRepoFile, nil, false, line.Text)
		}
		for id, commit := range b.Commits {
			read.fence(blame.key("commits").key(id).key("subject"), provenance.SourceRepoCommit, nil, false,
				commit.Subject)
		}
	}
	return read.page(ctx, t.Name(), gitAnswer(repo, page, map[string]resolvedRev{"rev": rev}),
		"Blame fewer lines with `start_line` and `end_line`.")
}

// fit is the longest prefix of n rows whose answer fits one tool answer,
// with that answer rendered as it is sent — n where the whole page does.
// answer builds the answer holding the first k rows.
//
// MEASURED AS SENT, by rendering each probe the way the answer goes out — the
// same defusing, the same compact encoding — so the bound is the bytes the
// caller receives rather than an estimate of them. A search over the prefixes,
// since an answer only grows with its rows: a page that fits costs one render,
// and one that does not about log₂ n more.
//
// One read labels every probe, so a page it cuts fences nothing ([fitLabelled]
// builds a page whose rows carry fenced text, a read a probe).
func (r *foreignRead) fit(n int, answer func(k int) any) (int, json.RawMessage, error) {
	return fitLabelled(n, func(k int) (*foreignRead, any) { return r, answer(k) })
}
