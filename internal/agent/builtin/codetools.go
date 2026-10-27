package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE CODE READ TOOLS: the company's own repositories (`code.native`), read on
// the server rather than in a clone — code_list_repos, code_get_repo,
// code_resolve, code_read, code_diff, code_compare and code_landed_in — each
// named for its authority row, and ONE list ([CodeTools]) that a seat's
// registry and the operator catalogue both take, so a schema, a default and a
// refusal's words are written once.
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
// # Rows at the seat's level, objects by id
//
// What a repository is and what a ref points at are ROWS, read at the seat's
// level — linearizable on both surfaces ([seatReadLevel]), so an answer
// includes every push that landed before the call. What an object id names is
// the same object on every node at every position, so a read by one needs no
// level: it is asked with the caller's floor on the code log (its own pushes,
// and the position a ref it named was resolved at) as the git node's SYNC
// level, never a barrier. A turn pays one barrier per repository it names —
// the repository is resolved ONCE A TURN ([CodeDeps.repo], kept on the turn's
// lookups) — and one per ref a call names; a call that names commits by id
// pays none beyond the repository's.
//
// # Text somebody else wrote is fenced
//
// A file's lines (code_read) and a patch (code_diff) are text a repository's
// committers wrote — importers and outside contributors among them — that a
// read by object id cannot attribute, so each goes out inside a fence of the
// turn's boundary at trust `external`, naming no writer (ADR-0040), and every
// other string of every answer — paths, ref names, descriptions — is defused
// ([foreignRead]).
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
)

// CodeToolNames are the seven, in the order a catalogue lists them: the
// repositories, a ref, then the reads by object id.
func CodeToolNames() []string {
	return []string{CodeListReposTool, CodeGetRepoTool, CodeResolveTool, CodeReadTool,
		CodeDiffTool, CodeCompareTool, CodeLandedInTool}
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
	// Floor is this node's session floor on the code log — its own pushes —
	// which every read by object id is asked with, so the copy that answers
	// holds them.
	Floor() statelog.Position
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
}

// CodeTools are the code tools a seat and an operator share: ONE list, so a
// schema, a default and a refusal's words are written once, and nil where
// deps reach no code host. Every one reads; none delivers anywhere.
func CodeTools(deps CodeDeps) []tools.Callable {
	if deps.Code == nil {
		return nil
	}
	return []tools.Callable{
		&codeListRepos{deps: deps}, &codeGetRepo{deps: deps}, &codeResolve{deps: deps},
		&codeRead{deps: deps}, &codeDiff{deps: deps}, &codeCompare{deps: deps},
		&codeLandedIn{deps: deps},
	}
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
// EVERY ONE IS A PROVEN READ, and idempotent: asking twice costs a round and
// changes nothing. So a turn that only read code is a turn that delivered
// nothing (the delivery check), a worker may be handed them (the guard), and
// the operator catalogue audits no call to one, as it audits no read.
func codeAnnotations(name string) (tools.Annotations, bool) {
	for _, tool := range CodeToolNames() {
		if tool == name {
			return tools.Annotations{ReadOnly: mcp.Yes, Idempotent: mcp.Yes}, true
		}
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

// asker is who this call reads as, or the refusal it is answered with.
func (d CodeDeps) asker(ctx context.Context, turn *turnctx.Turn, tool string) (code.Principal, *tools.Result) {
	resolve := actorFor
	if d.Actor != nil {
		resolve = func(turn *turnctx.Turn) (Actor, error) { return d.Actor(ctx, turn) }
	}
	actor, err := resolve(turn)
	if err != nil {
		r := refusedBy(tools.RefusalForbidden, err, tool+" can only be called during a turn, on "+
			"behalf of a seat, or by a signed-in caller.")
		return code.Principal{}, &r
	}
	p := CodeAskerOf(actor)
	if err := p.Validate(); err != nil {
		r := refusedBy(tools.RefusalForbidden, err, fmt.Sprintf("%s could not name who is asking "+
			"in the terms a repository's grant is judged by, so it read nothing.", tool))
		return code.Principal{}, &r
	}
	return p, nil
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
// surface has no turn, and resolves once a call.
//
// THE ONE RESOLUTION OF A `repo` ARGUMENT, so whatever has to happen before a
// turn's first request about a repository happens in one place; every tool
// reads all its arguments first ([repoArg], [revArg]), so a malformed call is
// refused before it costs a read. The memo decides nothing about access: the
// node answering every later read judges the grant again.
func (d CodeDeps) repo(ctx context.Context, turn *turnctx.Turn, tool string, asker code.Principal,
	name string) (code.RepoRef, *tools.Result) {

	key := codeRepoKey{asker: asker, name: name}
	if held, ok := turn.LookedUp().Load(key); ok {
		if ref, ok := held.(code.RepoRef); ok {
			return ref, nil
		}
	}
	answer, err := d.Code.Read(ctx, code.ReadQuery{Kind: code.ReadRepo, Asker: asker, Repo: name, Fresh: seatRead})
	if err != nil {
		r := codeFailure(ctx, tool, name, err)
		return code.RepoRef{}, &r
	}
	if answer.Repo == nil {
		r := faulted(ctx, tool, fmt.Errorf("builtin: the code host answered repository %s with no "+
			"detail", name), fmt.Sprintf("%s could not read repository %q: %s.", tool, name, faultSaid))
		return code.RepoRef{}, &r
	}
	ref := answer.Repo.RepoRef
	turn.LookedUp().Store(key, ref)
	return ref, nil
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
	turn.LookedUp().Forget(codeRepoKey{asker: asker, name: repo.Name})
	r := refused(tools.RefusalStaleVersion, fmt.Sprintf("%s: the name %q names another repository "+
		"than it did when this turn first read it — it was deleted and declared again — so nothing "+
		"was read. Make the same call again: it reads the repository the name names now.", tool,
		repo.Name))
	return &r
}

// resolveRef is a NAME as a ref of repo: ONE code.resolve_ref at the seat's
// level — linearizable, the one read about a ref that pays a barrier —
// answering the ref's row: its oid, its whole name and the position the read
// saw it at, which a read by object id is then floored at. THE ONE
// code.RefQuery in this package: code_resolve's `ref` and code_landed_in's
// `target` call it directly (a ref, never an id), and [CodeDeps.resolveRev] for
// a revision that is not an id.
func (d CodeDeps) resolveRef(ctx context.Context, turn *turnctx.Turn, tool string, asker code.Principal,
	repo code.RepoRef, name string) (code.RefAnswer, *tools.Result) {

	answer, err := d.Code.ResolveRef(ctx, code.RefQuery{Repo: repo.Name, Ref: name, Fresh: seatRead,
		Asker: asker})
	if err != nil {
		r := codeFailure(ctx, tool, repo.Name, err)
		return code.RefAnswer{}, &r
	}
	if r := d.sameRepo(turn, tool, asker, repo, answer.Repo); r != nil {
		return code.RefAnswer{}, r
	}
	return answer, nil
}

// headRev is the revision a call names its repository's default branch by.
const headRev = "HEAD"

// resolvedRev is a revision argument as a read by object id names it: the
// object id — whole, or an abbreviation the git node expands — and, where the
// argument was a ref, the ref it resolved through.
type resolvedRev struct {
	oid string
	ref *code.RefAnswer
}

// at is the position the revision's ref was read at, zero for an id.
func (r resolvedRev) at() statelog.Position {
	if r.ref == nil {
		return statelog.Position{}
	}
	return r.ref.Position
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

// resolveRev is a revision as an oid: an object id, whole or abbreviated,
// passes with no read (the git node expands an abbreviation from the objects
// alone, never as a ref); `HEAD` is the repository's default branch; anything
// else is a branch, a tag or a ref, resolved by [CodeDeps.resolveRef]. So a
// branch whose name is all hex digits is named by its whole ref.
func (d CodeDeps) resolveRev(ctx context.Context, turn *turnctx.Turn, tool string, asker code.Principal,
	repo code.RepoRef, rev string) (resolvedRev, *tools.Result) {

	switch {
	case code.ValidRev(rev):
		return resolvedRev{oid: rev}, nil
	case rev == headRev:
		rev = repo.DefaultBranch
	}
	answer, refusal := d.resolveRef(ctx, turn, tool, asker, repo, rev)
	if refusal != nil {
		return resolvedRev{}, refusal
	}
	return resolvedRev{oid: answer.OID, ref: &answer}, nil
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

// resolvePair is a comparison's two revisions as oids, each by
// [CodeDeps.resolveRev].
func (d CodeDeps) resolvePair(ctx context.Context, turn *turnctx.Turn, tool string, asker code.Principal,
	repo code.RepoRef, base, head string) (resolvedRev, resolvedRev, *tools.Result) {

	b, refusal := d.resolveRev(ctx, turn, tool, asker, repo, base)
	if refusal != nil {
		return resolvedRev{}, resolvedRev{}, refusal
	}
	h, refusal := d.resolveRev(ctx, turn, tool, asker, repo, head)
	if refusal != nil {
		return resolvedRev{}, resolvedRev{}, refusal
	}
	return b, h, nil
}

// gitFresh is what a read by object id is asked with: the furthest of this
// node's own floor on the code log and the positions the call's refs were read
// at — a SYNC level for the copy that answers, so it holds the commits those
// refs named and this node's own pushes — or none, which reads the copy as it
// stands.
func (d CodeDeps) gitFresh(revs ...resolvedRev) statelog.Freshness {
	floor := d.Code.Floor()
	for _, r := range revs {
		floor = furthestOnLog(floor, r.at())
	}
	if floor.IsZero() {
		return statelog.Freshness{}
	}
	return statelog.FloorAt(statelog.ReadSession, floor)
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

// gitRead asks q of the repository's home git node, refused here first where
// the query is one no git node would take — so a malformed argument costs no
// round trip and is told in the domain's own words.
func (d CodeDeps) gitRead(ctx context.Context, tool, repo string, q code.GitQuery) (code.GitAnswer, *tools.Result) {
	if err := q.Validate(); err != nil {
		r := codeFailure(ctx, tool, repo, err)
		return code.GitAnswer{}, &r
	}
	answer, err := d.Code.GitRead(ctx, q)
	if err != nil {
		r := codeFailure(ctx, tool, repo, err)
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
		out.Resolved[arg] = codeResolved{Ref: r.ref.Ref, OID: r.ref.OID, Position: r.ref.Position}
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
// retry clears. Only code.ErrNotPermitted is a decision about who asked.
func codeFailure(ctx context.Context, tool, repo string, err error) tools.Result {
	var (
		unready  *code.ErrGitUnready
		unserved *estate.ErrUnserved
	)
	switch {
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

// codeRevArg describes a revision argument.
func codeRevArg(what string) map[string]any {
	return map[string]any{
		"type": "string",
		"description": what + ": a commit id — whole, or its first 7 or more hex digits, which costs " +
			"nothing — or a branch, a tag, a whole ref, or " + headRev + " for the default branch, " +
			"each resolved to the commit it points at now. A branch whose name is all hex digits is " +
			"named by its whole ref (refs/heads/<name>).",
	}
}

// ---- code_list_repos ---------------------------------------------------- //

type codeListRepos struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeListRepos)(nil)

func (t *codeListRepos) Name() string { return CodeListReposTool }

func (t *codeListRepos) Description() string {
	return "List the company's repositories you can read, by name: each one's owner, visibility, " +
		"default branch and the commit it points at, your access, its last push and its protection " +
		"rules, a page at a time — pass `next` back as `cursor` until no `next` comes back. A " +
		"repository you may not read is not listed, so a page can hold fewer than `limit`, or none, " +
		"and still name a `next`."
}

func (t *codeListRepos) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status": map[string]any{
				"type": "string",
				"enum": []string{string(code.RepoStatusActive), string(code.RepoStatusArchived)},
				"description": "Only the active repositories, or only the archived ones (read-only); " +
					"both when left out.",
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
	Name          string            `json:"name"`
	Description   string            `json:"description,omitempty"`
	Status        code.RepoStatus   `json:"status"`
	Owner         code.Owner        `json:"owner"`
	Visibility    code.Visibility   `json:"visibility"`
	Access        string            `json:"access"`
	DefaultBranch string            `json:"default_branch"`
	Head          string            `json:"head,omitempty"`
	Pushes        int64             `json:"pushes"`
	LastPushAt    time.Time         `json:"last_push_at,omitzero"`
	Protections   []code.Protection `json:"protections,omitempty"`
}

func repoRowOf(s code.RepoSummary) codeRepoRow {
	return codeRepoRow{
		Name: s.Name, Description: s.Description, Status: s.Status, Owner: s.Owner,
		Visibility: s.Visibility, Access: s.Access, DefaultBranch: s.DefaultBranch, Head: s.Head,
		Pushes: s.Pushes, LastPushAt: s.LastPushAt, Protections: s.Protections,
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

	asker, refusal := t.deps.asker(ctx, turn, t.Name())
	if refusal != nil {
		return *refusal, nil
	}
	q := code.ReadQuery{Kind: code.ReadRepos, Asker: asker, Cursor: strings.TrimSpace(argString(args, "cursor")),
		Fresh: seatRead}
	switch status := code.RepoStatus(strings.TrimSpace(argString(args, "status"))); status {
	case "":
	case code.RepoStatusActive, code.RepoStatusArchived:
		q.Status = []code.RepoStatus{status}
	default:
		return failed(fmt.Sprintf("%s: `status` is %s or %s, not %q.", t.Name(), code.RepoStatusActive,
			code.RepoStatusArchived, clip(string(status)))), nil
	}
	q.Limit = argInt(args, "limit", defaultCodeRepoList)
	if q.Limit < 1 || q.Limit > maxCodeRepoList {
		return failed(fmt.Sprintf("%s: `limit` is 1 to %d.", t.Name(), maxCodeRepoList)), nil
	}
	answer, err := t.deps.Code.Read(ctx, q)
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
	read := newForeignRead(turn, asker.Seat)
	k, rendered, err := read.fit(len(rows), page)
	switch {
	case err != nil:
		return labelFault(ctx, t.Name(), err), nil
	case k == 0 && len(rows) > 0:
		return failed(fmt.Sprintf("%s: repository %q alone is past one answer — its protection rules "+
			"are that long — so it is not listed whole on any page. Read its rules in the company's "+
			"configuration (code.repos).", t.Name(), rows[0].Name)), nil
	}
	return compactAnswer(rendered, "Ask for fewer with `limit`.")
}

// ---- code_get_repo ------------------------------------------------------ //

type codeGetRepo struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeGetRepo)(nil)

func (t *codeGetRepo) Name() string { return CodeGetRepoTool }

func (t *codeGetRepo) Description() string {
	return fmt.Sprintf("One repository in detail — its protection rules, its size, how many branches "+
		"and tags it has, your access, its last push and, asked by a person or a token, the URL to "+
		"clone it from (a seat reads a repository through these tools) — and a page of its branches "+
		"with the commit each points at, %d a page: `branches_prefix` narrows them to a namespace "+
		"(`seats/<handle>/`, `release/`), and `branches_next` comes back as `branches_cursor` for "+
		"the next page, which answers the branches alone.", codeBranchesPage)
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
	Repo         string            `json:"repo"`
	Detail       *codeRepoDetail   `json:"detail,omitempty"`
	CloneURL     string            `json:"clone_url,omitempty"`
	Branches     []codeBranch      `json:"branches"`
	BranchesNext string            `json:"branches_next,omitempty"`
	Position     statelog.Position `json:"position"`
}

// branchRoot is where a branch's ref begins.
const branchRoot = "refs/heads/"

func (t *codeGetRepo) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	asker, refusal := t.deps.asker(ctx, turn, t.Name())
	if refusal != nil {
		return *refusal, nil
	}
	name, refusal := repoArg(t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	repo, refusal := t.deps.repo(ctx, turn, t.Name(), asker, name)
	if refusal != nil {
		return *refusal, nil
	}
	q := code.ReadQuery{Kind: code.ReadRefs, Asker: asker, Repo: repo.Name, Limit: codeBranchesPage,
		Cursor: strings.TrimSpace(argString(args, "branches_cursor")), Fresh: seatRead}
	// A NAMESPACE UNDER refs/heads/, however the model spells it — with or
	// without the root, with or without the closing slash the listing's
	// prefix ends in.
	ns := strings.TrimPrefix(strings.TrimSpace(argString(args, "branches_prefix")), branchRoot)
	if ns = strings.Trim(ns, "/"); ns != "" {
		q.Prefix = branchRoot + ns + "/"
	}
	answer, err := t.deps.Code.Read(ctx, q)
	if err != nil {
		return codeFailure(ctx, t.Name(), repo.Name, err), nil
	}
	out := codeRepoAnswer{Repo: repo.Name, Position: answer.Position}
	if d := answer.Repo; d != nil {
		if r := t.deps.sameRepo(turn, t.Name(), asker, repo, d.RepoRef); r != nil {
			return *r, nil
		}
		out.Detail = &codeRepoDetail{codeRepoRow: repoRowOf(d.RepoSummary), ManagedBy: d.ManagedBy,
			Refs: d.Refs, Branches: d.Branches, Tags: d.Tags, LivePacks: d.LivePacks,
			LiveBytes: d.LiveBytes, CreatedAt: d.CreatedAt, DeletedAt: d.DeletedAt}
		if asker.Kind == code.PrincipalOperator && t.deps.CloneURL != nil {
			out.CloneURL = t.deps.CloneURL(repo.Name)
		}
	}
	branches := make([]codeBranch, len(answer.Refs))
	for i, ref := range answer.Refs {
		branches[i] = codeBranch{Name: strings.TrimPrefix(ref.Ref, branchRoot), OID: ref.OID}
	}
	// BRANCHES CUT WHOLE-ROW AT THE ANSWER, as a repository listing is: the
	// refs listing is keyset by the ref, so the next page begins after the
	// last branch kept.
	page := func(k int) any {
		paged := out
		paged.Branches, paged.BranchesNext = branches[:k], answer.Next
		if k > 0 && k < len(branches) {
			paged.BranchesNext = answer.Refs[k-1].Ref
		}
		return paged
	}
	read := newForeignRead(turn, asker.Seat)
	k, rendered, err := read.fit(len(branches), page)
	switch {
	case err != nil:
		return labelFault(ctx, t.Name(), err), nil
	case k == 0 && len(branches) > 0:
		return failed(fmt.Sprintf("%s: repository %q's detail leaves no room in one answer for even "+
			"one branch — its protection rules are that long. Its branches are read a page at a "+
			"time once you pass the `branches_next` of a page; read its rules in the company's "+
			"configuration (code.repos).", t.Name(), repo.Name)), nil
	}
	return compactAnswer(rendered, "Narrow the branches with `branches_prefix`.")
}

// ---- code_resolve ------------------------------------------------------- //

type codeResolve struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeResolve)(nil)

func (t *codeResolve) Name() string { return CodeResolveTool }

func (t *codeResolve) Description() string {
	return "Resolve a branch, a tag or a whole ref to the commit it points at NOW — the one code read " +
		"that waits for the newest push. Call it once per ref you need, then pass the commit id it " +
		"answers to the other code tools: every call that takes an id costs nothing extra, and " +
		"reads the same commit however the branch moves meanwhile."
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

	asker, refusal := t.deps.asker(ctx, turn, t.Name())
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
	repo, refusal := t.deps.repo(ctx, turn, t.Name(), asker, name)
	if refusal != nil {
		return *refusal, nil
	}
	if ref == headRev {
		ref = repo.DefaultBranch
	}
	answer, refusal := t.deps.resolveRef(ctx, turn, t.Name(), asker, repo, ref)
	if refusal != nil {
		return *refusal, nil
	}
	read := newForeignRead(turn, asker.Seat)
	return read.page(ctx, t.Name(), codeRefAnswer{Repo: repo.Name, Ref: answer.Ref, OID: answer.OID,
		Position: answer.Position}, "")
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

	asker, refusal := t.deps.asker(ctx, turn, t.Name())
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
	repo, refusal := t.deps.repo(ctx, turn, t.Name(), asker, name)
	if refusal != nil {
		return *refusal, nil
	}
	rev, refusal := t.deps.resolveRev(ctx, turn, t.Name(), asker, repo, revName)
	if refusal != nil {
		return *refusal, nil
	}
	path, pathB64 := argString(args, "path"), argString(args, "path_b64")
	q := code.GitQuery{RepoID: repo.ID, Asker: asker, Fresh: t.deps.gitFresh(rev),
		Cursor: strings.TrimSpace(argString(args, "cursor"))}
	if recursive {
		q.Kind, q.Tree = code.GitTree, &code.TreeQuery{Rev: rev.oid, Path: path, PathB64: pathB64,
			Recursive: true}
	} else {
		q.Kind, q.Blob = code.GitBlob, &code.BlobQuery{Rev: rev.oid, Path: path, PathB64: pathB64,
			StartLine: start, EndLine: end}
	}
	page, refusal := t.deps.gitRead(ctx, t.Name(), repo.Name, q)
	if refusal != nil {
		return *refusal, nil
	}
	read := newForeignRead(turn, asker.Seat)
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

	asker, refusal := t.deps.asker(ctx, turn, t.Name())
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
	repo, refusal := t.deps.repo(ctx, turn, t.Name(), asker, name)
	if refusal != nil {
		return *refusal, nil
	}
	base, head, refusal := t.deps.resolvePair(ctx, turn, t.Name(), asker, repo, baseName, headName)
	if refusal != nil {
		return *refusal, nil
	}
	renames := true
	if v, set := args["renames"].(bool); set {
		renames = v
	}
	diff := &code.DiffQuery{Base: base.oid, Head: head.oid, Paths: paths, File: argString(args, "file"),
		FileB64: argString(args, "file_b64"), Renames: renames}
	page, refusal := t.deps.gitRead(ctx, t.Name(), repo.Name, code.GitQuery{Kind: code.GitDiff,
		RepoID: repo.ID, Asker: asker, Fresh: t.deps.gitFresh(base, head),
		Cursor: strings.TrimSpace(argString(args, "cursor")), Diff: diff})
	if refusal != nil {
		return *refusal, nil
	}
	read := newForeignRead(turn, asker.Seat)
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

	asker, refusal := t.deps.asker(ctx, turn, t.Name())
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
	repo, refusal := t.deps.repo(ctx, turn, t.Name(), asker, name)
	if refusal != nil {
		return *refusal, nil
	}
	base, head, refusal := t.deps.resolvePair(ctx, turn, t.Name(), asker, repo, baseName, headName)
	if refusal != nil {
		return *refusal, nil
	}
	page, refusal := t.deps.gitRead(ctx, t.Name(), repo.Name, code.GitQuery{Kind: code.GitCompare,
		RepoID: repo.ID, Asker: asker, Fresh: t.deps.gitFresh(base, head),
		Compare: &code.CompareQuery{Base: base.oid, Head: head.oid}})
	if refusal != nil {
		return *refusal, nil
	}
	read := newForeignRead(turn, asker.Seat)
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

	asker, refusal := t.deps.asker(ctx, turn, t.Name())
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
	repo, refusal := t.deps.repo(ctx, turn, t.Name(), asker, name)
	if refusal != nil {
		return *refusal, nil
	}
	commit, refusal := t.deps.resolveRev(ctx, turn, t.Name(), asker, repo, commitName)
	if refusal != nil {
		return *refusal, nil
	}
	if target == headRev {
		target = repo.DefaultBranch
	}
	tip, refusal := t.deps.resolveRef(ctx, turn, t.Name(), asker, repo, target)
	if refusal != nil {
		return *refusal, nil
	}
	resolved := resolvedRev{oid: tip.OID, ref: &tip}
	page, refusal := t.deps.gitRead(ctx, t.Name(), repo.Name, code.GitQuery{Kind: code.GitLandedIn,
		RepoID: repo.ID, Asker: asker, Fresh: t.deps.gitFresh(commit, resolved),
		LandedIn: &code.LandedInQuery{Commit: commit.oid, Target: tip.Ref, TargetOID: tip.OID,
			At: tip.Position}})
	if refusal != nil {
		return *refusal, nil
	}
	read := newForeignRead(turn, asker.Seat)
	return read.page(ctx, t.Name(), gitAnswer(repo, page,
		map[string]resolvedRev{"commit": commit, "target": resolved}), "")
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
func (r *foreignRead) fit(n int, answer func(k int) any) (int, json.RawMessage, error) {
	whole, err := r.render(answer(n))
	if err != nil || len(whole) <= ToolAnswerBytes {
		return n, whole, err
	}
	fits, kept := 0, json.RawMessage(nil)
	if kept, err = r.render(answer(0)); err != nil {
		return 0, nil, err
	}
	for over := n; over-fits > 1; {
		mid := fits + (over-fits)/2
		rendered, err := r.render(answer(mid))
		if err != nil {
			return 0, nil, err
		}
		if len(rendered) <= ToolAnswerBytes {
			fits, kept = mid, rendered
		} else {
			over = mid
		}
	}
	return fits, kept, nil
}
