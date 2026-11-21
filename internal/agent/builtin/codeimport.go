package builtin

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/tools"
)

// code_import_repo: a person's request that Gitlet create a project repository
// filled from one at another host, once or, with `mirror`, kept in step as a
// pull mirror (internal/gitlet/mirror). It is a lifecycle create carrying the
// source ([gitlet.LifecycleRequest.Import] or [gitlet.RepoSettings.MirrorFrom]):
// the repository is `importing` from its record on while its home fetches the
// source in chunks; nothing here waits for the fetch.
//
// A person's alone (authz.ActionCodeImportRepo: human-only): an import dials
// another host for the company, and a model's say-so would point Gitlet at any
// URL a page it read named. The deciding data node refuses a seat's import or
// mirror again ([gitlet.LifecycleRequest]); a credential bound to nobody imports
// nothing ([gitlet.UnboundDetail]).

// `credential` is a variable's NAME, never a token ([credentialArg]): the record
// carries the `${VAR}` reference, resolved from the secret store, then the
// environment, only when the home dials, so no token lands on settings every
// data node and backup keeps. Only a variable code.remote_credentials declares
// for the source's host is taken, by the data node and the dial alike
// (config.CodeRemoteCredentials.Refusal), so a person cannot have Gitlet send a
// secret the administrator did not name for that host to one they chose.
//
// The repository's id derives from the call's operation ([opIDFor]), as
// code_create_repo's does, so the same call made again is the same create,
// answered from the domain's ledger.

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
			"name": map[string]any{"type": "string"},
			"url":  map[string]any{"type": "string"},
			"credential": map[string]any{"type": "string",
				"description": "A variable code.remote_credentials declares for the url's host, by name; never a token."},
			"owner":          map[string]any{"type": "object", "description": ownerShape},
			"visibility":     map[string]any{"type": "string", "enum": spelled(gitlet.Visibilities())},
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
	if c.asker.Kind != gitlet.PrincipalOperator || c.asker.Seat == "" {
		// The table's rule again, where the request is formed: every data node
		// refuses a seat's import too.
		return refusedBy(tools.RefusalForbidden, gitlet.ErrNotPermitted, tool+": an import is a person's, "+
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
	id, ok := gitlet.RepoIDOf(req.OpID)
	if !ok {
		return faulted(ctx, tool, fmt.Errorf("builtin: operation %s carries no instant", req.OpID),
			fmt.Sprintf("%s could not name the repository it would create: %s.", tool, faultSaid)), nil
	}
	repo := gitlet.RepoRef{ID: id, Name: req.Name, Kind: gitlet.RepoProject}
	if err := req.Validate(); err != nil {
		return codeWriteFailure(ctx, tool, actor, req.Name, err), nil
	}
	res, err := t.deps.Writer.Lifecycle(ctx, req)
	if err != nil {
		return t.deps.failedWrite(ctx, turn, tool, c, repo, req.OpID, err), nil
	}
	// Answered as code_create_repo answers: the repository `importing`, or the
	// refusal in its class.
	return (&codeRepoWrite{deps: t.deps, tool: tool}).answer(ctx, turn, c, repo, req, res)
}

// importRequest is an import's create from its arguments, all but the
// operation and the actor, or the refusal of one no create could take. Its
// search is grep until a maintainer asks for an index, as code_create_repo's.
func importRequest(tool string, args map[string]any) (gitlet.LifecycleRequest, *tools.Result) {
	name := strings.TrimSpace(argString(args, "name"))
	if !gitlet.ValidName(name) {
		r := failed(fmt.Sprintf("%s needs `name`: the project's name — lowercase letters, digits and "+
			"hyphens; %q is not one.", tool, clip(name)))
		return gitlet.LifecycleRequest{}, &r
	}
	if handle, scratch := gitref.ScratchName(name); scratch {
		r := failed(fmt.Sprintf("%s: %s names seat %s's scratch repository, which takes no import — name a "+
			"project.", tool, name, handle))
		return gitlet.LifecycleRequest{}, &r
	}
	url := strings.TrimSpace(argString(args, "url"))
	if url == "" {
		r := failed(fmt.Sprintf("%s needs `url`: the repository at the other host, an https URL.", tool))
		return gitlet.LifecycleRequest{}, &r
	}
	credential, refusal := credentialArg(tool, args)
	if refusal != nil {
		return gitlet.LifecycleRequest{}, refusal
	}
	owner, given, refusal := ownerArg(tool, "owner", args)
	if refusal != nil {
		return gitlet.LifecycleRequest{}, refusal
	}
	if !given {
		r := failed(fmt.Sprintf("%s: the project %s needs an `owner` — %s.", tool, name, ownerShape))
		return gitlet.LifecycleRequest{}, &r
	}
	settings := gitlet.RepoSettings{ManagedBy: gitlet.ManagedTools, Kind: gitlet.RepoProject, Owner: owner,
		DefaultBranch: cmp.Or(strings.TrimSpace(argString(args, "default_branch")), "main"),
		Visibility: gitlet.Visibility(cmp.Or(strings.TrimSpace(argString(args, "visibility")),
			string(gitlet.VisibilityCompany))),
		Search: gitlet.SearchGrep}
	req := gitlet.LifecycleRequest{Kind: gitlet.LifecycleCreate, Name: name, Create: &settings}
	// A PULL MIRROR IS IMPORTED FROM ITS OWN SOURCE as it is created, and an
	// import beside it would be a second source (gitlet.CreateRepo): so
	// `mirror` puts the URL on the settings and no import on the request.
	if argBool(args, "mirror") {
		settings.MirrorFrom = &gitlet.MirrorSource{URL: url, Credential: credential}
	} else {
		req.Import = &gitlet.ImportSource{URL: url, Credential: credential}
	}
	return req, nil
}

// credentialArg is the reference an import's `credential` names — `${NAME}`,
// or "" for an anonymous fetch — or the refusal of what is no variable's name,
// which never quotes it: it is most likely the token itself.
//
// Upper case only, narrower than [envref.ValidName] on purpose: a forge token
// (GitHub's is `ghp_` and 36 letters, digits and underscores) passes the
// reference grammar and would be recorded, the token itself, on the settings.
// Every credential variable the secret store, an environment or the docs name
// is upper case (GITHUB_TOKEN).
func credentialArg(tool string, args map[string]any) (string, *tools.Result) {
	name := strings.TrimSpace(argString(args, "credential"))
	switch {
	case name == "":
		return "", nil
	case envref.ValidName(name) && name == strings.ToUpper(name):
		return "${" + name + "}", nil
	}
	r := failedArg("credential",
		fmt.Sprintf("%s: `credential` is the NAME of the variable that holds the source's token, "+
			"in upper case (such as GITHUB_TOKEN) — one code.remote_credentials declares for the source's host — "+
			"resolved from the secret store or the environment when Gitlet dials, never the token itself, "+
			"which every record of the repository's settings would carry. An administrator stores the token "+
			"with `crewlet secrets set <NAME>` and declares the NAME for its host; name it here.", tool))
	return "", &r
}
