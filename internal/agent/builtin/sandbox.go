package builtin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// RunSandboxTool is the tool's wire name.
const RunSandboxTool = "run_sandbox"

// SandboxRequest is one run_sandbox call: the brief, and the repositories on
// this company's own code host it names.
type SandboxRequest struct {
	Brief string
	// Repos are the repositories the call names, as the model wrote them —
	// already held to their grammar ([RepoRequest]), and resolved by the
	// launcher, never trusted. Empty for a run given none.
	Repos []RepoRequest
}

// RepoRequest is one repository a run_sandbox call names, as the model wrote
// it: resolved by the launcher ([CodeDeps.ResolveRunRepos]) into the run's
// grant on it, never trusted.
type RepoRequest struct {
	// Repo is the repository's name, as code_list_repos answers it.
	Repo string
	// Base is a branch, a tag or a commit id to start from; "" is the run's
	// branch where it exists, else the default branch.
	Base string
	// Branch is the branch the run works on; "" is the seat's own branch for
	// the turn's work item (code.SeatBranch).
	Branch string
	// Access is "read", "write" or "" — the most the seat's grant allows on
	// the branch.
	Access string
	// Paths are the directories checked out (a sparse cone); empty is the
	// whole tree.
	Paths []string
	// Change is a change of the repository the run works on — its number
	// (42) or its reference (web!42) — resolved to its source branch, from
	// its head; "" for none. Never beside Branch or Base, which it decides.
	Change string
	// OpenChange asks the engine to open a change from the run's branch once
	// the run finishes ([OpenChangeRequest]); nil for none. Never beside
	// Change, whose branch heads one already.
	OpenChange *OpenChangeRequest
}

// OpenChangeRequest is a run_sandbox call's ask to open a change from the
// branch the run works on, at its collection: the branch it proposes into —
// "" for the repository's default branch — its title — "" for the subject of
// the commit the run delivers — and whether it opens as a draft.
type OpenChangeRequest struct {
	Target string
	Title  string
	Draft  bool
}

// SandboxLauncher is what this tool needs to start a detached coding run.
//
// An interface so the tool can be built and tested without a provider, and so
// the launch's ordering rules stay in one place rather than being re-derived
// here.
type SandboxLauncher interface {
	// Launch starts the run and returns once the job is going. It does NOT
	// wait for the job. A launch refused with a result the code tools
	// composed answers a [*RefusedLaunch], which the tool answers as it is.
	Launch(ctx context.Context, turn *turnctx.Turn, req SandboxRequest) (sandbox.LaunchResult, error)
}

// RefusedLaunch is a launch refused with a result already composed and
// classed — a repository the call named that the seat may not be given, in
// the code host's own words — which the tool answers as it is rather than as
// a launch the engine could not do.
type RefusedLaunch struct{ Result tools.Result }

func (r *RefusedLaunch) Error() string { return r.Result.Output }

// Unwrap is the refusal's class and cause, so a caller branching on either
// reads the result's own.
func (r *RefusedLaunch) Unwrap() error { return r.Result.Cause }

// runSandbox hands a concrete code task to a coding agent in an isolated box.
//
// THE SANDBOX IS A TOOL, not a phase backend that replaces Execute. The
// executor calls it with a code task, the engine starts the coding agent
// detached and SUSPENDS this Execute loop, and when the job finishes the loop
// is resumed with the agent's findings spliced in as this call's result. The
// same turn then continues — reporting, fixing, or calling the sandbox again —
// with its ordinary tools and full context.
//
// That is why it suspends rather than blocking: a coding job runs for minutes
// to hours, far past any broker ack window, and a turn that blocked on one
// would hold its seat and its node for the duration and lose everything to a
// restart.
//
// `repos` IS OFFERED WHERE THE COMPANY'S CODE HOST IS: only where the code
// tools are registered beside it ([Deps.Code], whose read seam is set exactly
// where this node's running runtime serves the code host), so a seat in a
// company that hosts no code is never shown an argument every call of which
// would be refused.
type runSandbox struct {
	launcher SandboxLauncher
	repos    bool
}

var _ tools.Detached = (*runSandbox)(nil)

func (t *runSandbox) Name() string { return RunSandboxTool }

func (t *runSandbox) Description() string {
	description := "Hand a concrete code task to a coding agent running in an " +
		"isolated sandbox with its own shell, git checkout and developer " +
		"toolchain. Name the repository and the exact change or " +
		"investigation. The agent works on its own and ends by reporting " +
		"what it found or delivered, which comes back to you as this " +
		"call's result. You are NOT done when you call this — when it " +
		"returns you still report to, or act for, the requester."
	if t.repos {
		description += " Name this company's repositories in `repos` rather than in the brief: they are " +
			"cloned for you with your access."
	}
	return description
}

func (t *runSandbox) Parameters() map[string]any {
	properties := map[string]any{
		"brief": map[string]any{
			"type": "string",
			"description": "The concrete code task: which repository, and the " +
				"exact change or investigation. For example \"Clone " +
				"example.com/acme/api, run the test suite, and report which " +
				"tests fail and why\" or \"Fix the failing CI in acme/api and " +
				"open a pull request\".",
		},
	}
	if t.repos {
		properties["repos"] = reposParameter()
	}
	return map[string]any{
		"type":       "object",
		"properties": properties,
		"required":   []any{"brief"},
	}
}

// reposParameter is the schema of `repos`: at most sandbox.MaxRunRepos
// repositories, each cone at most sandbox.MaxConePaths directories.
func reposParameter() map[string]any {
	return map[string]any{
		"type":     "array",
		"maxItems": sandbox.MaxRunRepos,
		"description": "Repositories on this company's own code host to give the coding agent, each cloned " +
			"into the box before it starts (a blobless clone: file contents arrive as git needs them). Name " +
			"repositories on another host in the brief instead.",
		"items": map[string]any{
			"type":                 "object",
			"required":             []any{"repo"},
			"additionalProperties": false,
			"properties": map[string]any{
				"repo": map[string]any{"type": "string",
					"description": "The repository's name, as " + CodeListReposTool + " lists it."},
				"base": map[string]any{"type": "string",
					"description": "The branch, tag or commit to start from. Default: your branch if it " +
						"exists, else the default branch."},
				"branch": map[string]any{"type": "string",
					"description": "The branch to work on. Default: seats/<your handle>/<the work item's " +
						"key>, or seats/<your handle>/run-<id> when the turn is on no work item."},
				"access": map[string]any{"type": "string", "enum": []any{"read", "write"},
					"description": "write to push. Default: write where your grant lets you push the " +
						"branch, else read."},
				"paths": map[string]any{"type": "array", "maxItems": sandbox.MaxConePaths,
					"items": map[string]any{"type": "string"},
					"description": "Directories to check out (a sparse cone), for a large repository. " +
						"Default: the whole tree."},
				"change": map[string]any{"type": "string",
					"description": "A change to work on, by its number or reference (web!42): its branch, " +
						"from its head. Instead of `branch` and `base`."},
				"open_change": map[string]any{"type": "object", "additionalProperties": false,
					"description": "Open a change from the branch once the run succeeds — once, from what it " +
						"pushed, carrying your work item. Needs write.",
					"properties": map[string]any{
						"target": map[string]any{"type": "string",
							"description": "The branch it proposes into. Default: the default branch."},
						"title": map[string]any{"type": "string",
							"description": "Default: the subject of the commit the run delivers."},
						"draft": map[string]any{"type": "boolean"},
					}},
			},
		},
	}
}

// Call is the non-detached path, and it REFUSES.
//
// A surface that invoked this tool without the detached seam would run the
// coding job and then answer the call normally — the turn would end believing
// the work was done while the job was still running, and nothing would ever
// collect its result. Refusing is the only safe answer, and it says why.
func (t *runSandbox) Call(context.Context, map[string]any) (tools.Result, error) {
	return refused(tools.RefusalForbidden, "run_sandbox can only be called "+
		"from an Execute phase that can suspend, because the coding job "+
		"outlives the turn that starts it."), nil
}

func (t *runSandbox) CallDetached(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.DetachedResult, error) {
	brief := strings.TrimSpace(argString(args, "brief"))
	if brief == "" {
		return tools.DetachedResult{Result: failed("run_sandbox needs a " +
			"non-empty `brief` describing the code task.")}, nil
	}
	repos, refusal := t.reposArg(args)
	if refusal != nil {
		return tools.DetachedResult{Result: *refusal}, nil
	}
	if turn == nil || turn.RunID == "" {
		return tools.DetachedResult{Result: refused(tools.RefusalForbidden,
			"run_sandbox needs an active turn; it can only be called from "+
				"within an Execute phase.")}, nil
	}

	res, err := t.launcher.Launch(ctx, turn, SandboxRequest{Brief: brief, Repos: repos})
	if err != nil {
		var composed *RefusedLaunch
		if errors.As(err, &composed) {
			return tools.DetachedResult{Result: composed.Result}, nil
		}
		// A launch that failed is a TOOL FAILURE, not an engine one: the
		// model asked for something the engine could not do, and telling it
		// so lets it fall back to its own tools rather than losing the turn.
		return tools.DetachedResult{Result: refused(tools.RefusalUnavailable,
			fmt.Sprintf("the sandbox could not be started: %v", err))}, nil
	}

	log.InfoContext(ctx, "run_sandbox_suspended",
		"turn_id", turn.RunID, "sandbox_id", res.SandboxID,
		"coding_agent", res.CodingAgent, "reused", res.Reused, "repos", len(repos))

	return tools.DetachedResult{
		Result: tools.Result{
			Output: "(the sandbox coding job is running; its findings will come " +
				"back as this call's result)",
		},
		Suspend: true,
		Payload: map[string]any{
			"turn_id":    turn.RunID,
			"sandbox_id": res.SandboxID,
		},
	}, nil
}

// repoKeys are the keys a `repos` item may hold, and openChangeKeys those its
// `open_change` may.
var (
	repoKeys       = []string{"repo", "base", "branch", "access", "paths", "change", "open_change"}
	openChangeKeys = []string{"target", "title", "draft"}
)

// reposArg is the call's `repos`, read STRICTLY — an item that is not an
// object, a key the schema does not name, a field that is not the type the
// schema gives it, an empty `repo`, an `access` outside its two values, a
// `change` beside the `branch` or `base` it decides, an `open_change` beside
// a `change` or asked of a run given `read`, a list past its bound, or one
// repository named twice is refused naming the
// item and the field, before the launcher is asked — or nil where the call
// names none. A tool that offers no `repos` (a company whose code host this
// node does not serve) never reads one: the argument is not in its schema, so
// every call path refuses it as one the tool does not read before the tool is
// asked.
//
// STRICT, unlike the tolerant readers beside it, because what a repository is
// given is a GRANT: an `acess: write` misspelt and dropped would hand the run
// the default access, which may be more than the model meant to ask for — and
// so would an `"access": ["read"]` read as no access asked, which argString,
// tolerant of a value of the wrong type by design, answers as "" (repoField).
func (t *runSandbox) reposArg(args map[string]any) ([]RepoRequest, *tools.Result) {
	raw, named := args["repos"]
	if !named || raw == nil {
		return nil, nil
	}
	refuse := func(format string, a ...any) ([]RepoRequest, *tools.Result) {
		r := failed("run_sandbox: " + fmt.Sprintf(format, a...))
		return nil, &r
	}
	items, ok := raw.([]any)
	if !ok {
		return refuse("`repos` must be a list of objects, each naming one repository as {\"repo\": <name>}.")
	}
	if len(items) > sandbox.MaxRunRepos {
		return refuse("`repos` names %d repositories; one run is given at most %d — split the work across "+
			"runs.", len(items), sandbox.MaxRunRepos)
	}
	out := make([]RepoRequest, 0, len(items))
	for i, item := range items {
		fields, ok := item.(map[string]any)
		if !ok {
			return refuse("repos[%d] is not an object: write {\"repo\": <name>}.", i)
		}
		for key := range fields {
			if !slices.Contains(repoKeys, key) {
				return refuse("repos[%d] holds %q, which is not one of %s.", i, clip(key),
					strings.Join(repoKeys, ", "))
			}
		}
		var req RepoRequest
		for _, f := range []struct {
			key  string
			into *string
		}{{"repo", &req.Repo}, {"base", &req.Base}, {"branch", &req.Branch}, {"access", &req.Access},
			{"change", &req.Change}} {
			value, ok := repoField(fields, f.key)
			if !ok {
				return refuse("repos[%d].%s is not a string: write it as one, or leave it out.", i, f.key)
			}
			*f.into = value
		}
		open, refusal := openChangeField(i, fields)
		if refusal != nil {
			return nil, refusal
		}
		req.OpenChange = open
		switch {
		case req.Repo == "":
			return refuse("repos[%d] needs `repo`: a repository's name, as %s lists it.", i, CodeListReposTool)
		case req.Access != "" && req.Access != string(sandbox.RepoRead) && req.Access != string(sandbox.RepoWrite):
			return refuse("repos[%d].access is %q: it is %q, %q, or left out for the most your grant allows.",
				i, clip(req.Access), sandbox.RepoRead, sandbox.RepoWrite)
		case req.Change != "" && (req.Branch != "" || req.Base != ""):
			// THE CHANGE DECIDES BOTH: its source branch, from its head. A
			// branch or a base beside it is a run told two places to start.
			return refuse("repos[%d] names `change` beside `branch` or `base`: a change is worked on from its "+
				"own branch's head — name one or the other.", i)
		case req.Change != "" && req.OpenChange != nil:
			return refuse("repos[%d] names `change` and `open_change`: the change's branch heads it already, "+
				"so the run delivers to it — leave `open_change` out.", i)
		case req.OpenChange != nil && req.Access == string(sandbox.RepoRead):
			return refuse("repos[%d] asks for `open_change` with `access: read`: a run that pushes nothing "+
				"delivers nothing to open a change from.", i)
		}
		if paths, ok := fields["paths"]; ok && paths != nil {
			list, ok := paths.([]any)
			if !ok {
				return refuse("repos[%d].paths must be a list of directories.", i)
			}
			if len(list) > sandbox.MaxConePaths {
				return refuse("repos[%d].paths names %d directories; a cone names at most %d.", i, len(list),
					sandbox.MaxConePaths)
			}
			for k, p := range list {
				s, ok := p.(string)
				if !ok {
					return refuse("repos[%d].paths[%d] is not a directory's path.", i, k)
				}
				req.Paths = append(req.Paths, s)
			}
		}
		if slices.ContainsFunc(out, func(r RepoRequest) bool { return r.Repo == req.Repo }) {
			return refuse("%s is named twice; one run works on one branch of a repository.", clip(req.Repo))
		}
		out = append(out, req)
	}
	return out, nil
}

// repoField is one string field of a `repos` item, trimmed — "" where the item
// leaves it out or sets it null, which the launch reads as the field's default
// — and false where the item holds it as anything but a string. A list, an
// object, a number or a bool is REFUSED rather than read as argString reads
// it: the default of every field is a choice the launch makes on the model's
// behalf (the most its grant allows, the seat's own branch, the branch's own
// head), so a value it did send in the wrong shape is a request it made, and
// is answered as one it must make again, never as one it did not make.
func repoField(fields map[string]any, key string) (string, bool) {
	switch v := fields[key].(type) {
	case nil:
		return "", true
	case string:
		return strings.TrimSpace(v), true
	}
	return "", false
}

// openChangeField is the i-th `repos` item's `open_change`, read as strictly
// as the item — nil where it leaves the field out or sets it null; an object
// of only `target` and `title`, each a string, and `draft`, a bool; anything
// else refused naming it — for the item's reason: an ask the engine acts on
// as the seat once the run is done, which a misspelt key would have made
// another one.
func openChangeField(i int, fields map[string]any) (*OpenChangeRequest, *tools.Result) {
	refuse := func(format string, a ...any) (*OpenChangeRequest, *tools.Result) {
		r := failed("run_sandbox: " + fmt.Sprintf(format, a...))
		return nil, &r
	}
	raw, named := fields["open_change"]
	if !named || raw == nil {
		return nil, nil
	}
	ask, ok := raw.(map[string]any)
	if !ok {
		return refuse("repos[%d].open_change is not an object: write {} for a change into the default branch, "+
			"or name its `target`, `title` and `draft`.", i)
	}
	for key := range ask {
		if !slices.Contains(openChangeKeys, key) {
			return refuse("repos[%d].open_change holds %q, which is not one of %s.", i, clip(key),
				strings.Join(openChangeKeys, ", "))
		}
	}
	var out OpenChangeRequest
	for _, f := range []struct {
		key  string
		into *string
	}{{"target", &out.Target}, {"title", &out.Title}} {
		value, ok := repoField(ask, f.key)
		if !ok {
			return refuse("repos[%d].open_change.%s is not a string: write it as one, or leave it out.", i, f.key)
		}
		*f.into = value
	}
	switch v := ask["draft"].(type) {
	case nil:
	case bool:
		out.Draft = v
	default:
		return refuse("repos[%d].open_change.draft is not true or false.", i)
	}
	return &out, nil
}
