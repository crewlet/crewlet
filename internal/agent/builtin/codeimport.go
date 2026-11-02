package builtin

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE IMPORT: code_import_repo, a person's request that the code host create a
// project repository filled from a repository at another host — once, or for
// good as a PULL MIRROR (`mirror`), which the repository's home then keeps in
// step with its source (internal/code/mirror, M6-D16 and M6-D19). It is a
// lifecycle CREATE carrying the source ([code.LifecycleRequest.Import], or the
// settings' [code.RepoSettings.MirrorFrom]), decided by a data node like every
// create: the repository exists, `importing`, from the record on, and its home
// fetches the source and lands it in chunks; nothing waits here for the fetch.
//
// # A person's alone
//
// The authority table holds it to a person (authz.ActionCodeImportRepo:
// human-only), so no seat is offered it: an import dials another host for the
// company, and a model's say-so would point the code host at any URL a page it
// read named (U49 a). The deciding data node refuses a seat's create carrying
// an import or a mirror again, whatever its grant ([code.LifecycleRequest]'s
// rule), so the two cannot drift. A credential bound to nobody imports nothing
// ([code.UnboundDetail]): a create names the person it is made for.
//
// # The credential is a NAME, never a token
//
// `credential` is the name of the variable that holds the token for the
// source host — `GITHUB_TOKEN` — which the tool makes the one whole `${VAR}`
// reference a record carries, resolved only when the home dials, from the
// secret store and then the environment (M6-D17). A token typed into the call
// is refused for its shape before anything is sent, so it is never recorded on
// a repository's settings, which every data node keeps and every backup
// carries.
//
// # One create, however often it is asked
//
// The repository's id is derived from the call's operation (m3-w1's rule,
// [opIDFor]), as code_create_repo's is, so the same call made again — the act
// transport's key, an assistant's `op_id` brought back — is the same create,
// which the domain's ledger answers from its own record.

// CodeImportRepoTool is the import's name: its authority row's action.
const CodeImportRepoTool = string(authz.ActionCodeImportRepo)

// ImportWrites are the import, after the purge: M6's writes, in the spec's
// order ([CodeWrites]).
func ImportWrites() []string { return []string{CodeImportRepoTool} }

// codeImportRepo is the import.
type codeImportRepo struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeImportRepo)(nil)

func (t *codeImportRepo) Name() string { return CodeImportRepoTool }

func (t *codeImportRepo) Description() string {
	return "Create a project repository filled from one at another host (an https URL), or kept in step " +
		"with it (`mirror`). A person's alone; `credential` names the variable holding the source's token. " +
		"The repository is `importing` until its home has fetched it."
}

func (t *codeImportRepo) Parameters() map[string]any {
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":           map[string]any{"type": "string"},
			"url":            map[string]any{"type": "string"},
			"credential":     map[string]any{"type": "string", "description": "A variable's name, never a token."},
			"owner":          map[string]any{"type": "object", "description": ownerShape},
			"visibility":     map[string]any{"type": "string", "enum": spelled(code.Visibilities())},
			"default_branch": map[string]any{"type": "string", "description": "`main` when left out."},
			"mirror":         map[string]any{"type": "boolean"},
		},
		"required": []string{"name", "url", "owner"},
	})
}

func (t *codeImportRepo) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *codeImportRepo) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	tool := t.Name()
	c, refusal := t.deps.writeCaller(ctx, turn, tool)
	if refusal != nil {
		return *refusal, nil
	}
	if c.asker.Kind != code.PrincipalOperator || c.asker.Seat == "" {
		// THE TABLE'S RULE AGAIN, here where the request is formed: a seat
		// is refused before the call reaches a tool, and a create carrying an
		// import a seat could send would be refused by every data node.
		return refusedBy(tools.RefusalForbidden, code.ErrNotPermitted, tool+": an import is a person's, "+
			"through a credential bound to their human seat."), nil
	}
	// EVERY ARGUMENT FIRST, so a malformed call mints no operation.
	req, refusal := importRequest(tool, args)
	if refusal != nil {
		return *refusal, nil
	}
	actor, bound := t.deps.bindOperation(c.actor, tool, args)
	if bound != "" {
		return failed(bound), nil
	}
	c.actor = actor
	req.Actor = c.asker
	req.OpID = opIDFor(actor, tool, lifecycleVerb(req.Kind), newRepoObject, args)
	id, ok := code.RepoIDOf(req.OpID)
	if !ok {
		return faulted(ctx, tool, fmt.Errorf("builtin: operation %s carries no instant", req.OpID),
			fmt.Sprintf("%s could not name the repository it would create: %s.", tool, faultSaid)), nil
	}
	repo := code.RepoRef{ID: id, Name: req.Name, Kind: code.RepoProject}
	if err := req.Validate(); err != nil {
		return codeWriteFailure(ctx, tool, actor, req.Name, err), nil
	}
	res, err := t.deps.Writer.Lifecycle(ctx, req)
	if err != nil {
		return t.deps.failedWrite(ctx, turn, tool, c, repo, req.OpID, err), nil
	}
	// THE CREATE'S ANSWER, as code_create_repo gives it: the repository as
	// its record made it — `importing` — or the refusal in its class.
	return (&codeRepoWrite{deps: t.deps, tool: tool}).answer(ctx, turn, c, repo, req, res)
}

// importRequest is an import's create from its arguments — every field but
// the operation and the actor — or the refusal of an argument no create could
// take. A tool's repository is searched with grep until somebody who
// maintains it asks for an index, as code_create_repo's is.
func importRequest(tool string, args map[string]any) (code.LifecycleRequest, *tools.Result) {
	name := strings.TrimSpace(argString(args, "name"))
	if !code.ValidName(name) {
		r := failed(fmt.Sprintf("%s needs `name`: the project's name — lowercase letters, digits and "+
			"hyphens; %q is not one.", tool, clip(name)))
		return code.LifecycleRequest{}, &r
	}
	if handle, scratch := gitref.ScratchName(name); scratch {
		r := failed(fmt.Sprintf("%s: %s names seat %s's scratch repository, which takes no import — name a "+
			"project.", tool, name, handle))
		return code.LifecycleRequest{}, &r
	}
	url := strings.TrimSpace(argString(args, "url"))
	if url == "" {
		r := failed(fmt.Sprintf("%s needs `url`: the repository at the other host, an https URL.", tool))
		return code.LifecycleRequest{}, &r
	}
	credential, refusal := credentialArg(tool, args)
	if refusal != nil {
		return code.LifecycleRequest{}, refusal
	}
	owner, given, refusal := ownerArg(tool, "owner", args)
	if refusal != nil {
		return code.LifecycleRequest{}, refusal
	}
	if !given {
		r := failed(fmt.Sprintf("%s: the project %s needs an `owner` — %s.", tool, name, ownerShape))
		return code.LifecycleRequest{}, &r
	}
	settings := code.RepoSettings{ManagedBy: code.ManagedTools, Kind: code.RepoProject, Owner: owner,
		DefaultBranch: cmp.Or(strings.TrimSpace(argString(args, "default_branch")), "main"),
		Visibility: code.Visibility(cmp.Or(strings.TrimSpace(argString(args, "visibility")),
			string(code.VisibilityCompany))),
		Search: code.SearchGrep}
	req := code.LifecycleRequest{Kind: code.LifecycleCreate, Name: name, Create: &settings}
	// A PULL MIRROR IS IMPORTED FROM ITS OWN SOURCE as it is created, and an
	// import beside it would be a second source (code.CreateRepo): so
	// `mirror` puts the URL on the settings and no import on the request.
	if argBool(args, "mirror") {
		settings.MirrorFrom = &code.MirrorSource{URL: url, Credential: credential}
	} else {
		req.Import = &code.ImportSource{URL: url, Credential: credential}
	}
	return req, nil
}

// credentialArg is the reference an import's `credential` names — `${NAME}`
// for the variable NAME, or "" for an anonymous fetch — or the refusal of an
// argument that is no variable's name. The refusal never quotes it: what is
// not a name is most likely the token itself.
//
// A NAME IN UPPER CASE, which is narrower than every name a reference can
// reach ([envref.ValidName]) on purpose: a forge's token is letters of both
// cases, digits and underscores — a GitHub token is `ghp_` and thirty-six of
// them — which the reference grammar alone would take as a variable's name and
// record, the token itself, on the repository's settings. A variable the
// secret store or an environment holds for a credential is named in upper case
// (GITHUB_TOKEN), and so is every one the documentation names.
func credentialArg(tool string, args map[string]any) (string, *tools.Result) {
	name := strings.TrimSpace(argString(args, "credential"))
	switch {
	case name == "":
		return "", nil
	case envref.ValidName(name) && name == strings.ToUpper(name):
		return "${" + name + "}", nil
	}
	r := failed(fmt.Sprintf("%s: `credential` is the NAME of the variable that holds the source's token, "+
		"in upper case (such as GITHUB_TOKEN), resolved from the secret store or the environment when the "+
		"code host dials — never the token itself, which every record of the repository's settings would "+
		"carry. Store the token with `crewlet secrets set <NAME>` and name it here.", tool))
	return "", &r
}
