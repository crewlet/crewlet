package builtin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
	"github.com/crewlet/crewlet/internal/words"
)

// CodeCheckoutTool is the tool a seat's coding run checks a repository of the
// company's own code host out through, declared in internal/toolname: like
// `ask_and_wait` it is a tool the executor run's surface offers of its own,
// which no row of the authority table names, because what it may grant is
// decided by the code host's read verb for the seat and for whoever woke its
// turn ([CodeDeps.ResolveRunRepos], on [authz.ActionCodeRead]) and by every
// repository's grant — never by a permission of its own a role could hold
// without the grant.
const CodeCheckoutTool = toolname.CodeCheckout

// RunCheckouts is where `code_checkout` gives a running job a repository: the
// engine resolves the request for the turn's seat ([CodeDeps.ResolveRunRepos]),
// writes the grant to the job's row ([sandbox.PendingStore.GrantRepo]) and
// answers it with the steps that clone it in the job's box. A refusal the
// resolution composed is a [*RefusedCheckout]; any other error is the node's.
type RunCheckouts interface {
	Checkout(ctx context.Context, turn *turnctx.Turn, launch string, req RepoRequest) (Checkout, error)
}

// Checkout is a repository given to a running job: its grant as the row
// holds it, whether this call added it or the job held it already, and the
// steps that clone it in the job's box ([sandbox.CodingSteps]).
type Checkout struct {
	Repo    sandbox.RunRepo
	Outcome sandbox.GrantOutcome
	Steps   []sandbox.SetupStep
}

// NewCodeCheckout is the tool a seat's coding run checks a repository out
// through, or nil without the engine's checkouts.
//
// # Why one repository per call, made from inside the run
//
// A coding run is the seat's own CLI in a box (ADR-0066), launched with no
// repository: what it needs is decided while it works, by the agent that does
// the work. So the box holds the code host's address and the run's credential
// from its launch, and nothing else, and each checkout is a call over the
// run's bridge that the bridge holds to the seat's gates like any other: the
// engine resolves the repository for the seat and for whoever woke its turn,
// writes the grant onto the job's row, and the code host's box admission,
// which reads that row on every request, lets the box clone it from the next
// request on (ADR-0086). Nothing the run did not check out is ever reachable
// from its box, and what it may push is decided per checkout — the branch,
// the access and a change to open at its collection — rather than for every
// repository the seat can reach.
//
// It is offered on the executor run's surface alone, beside `ask_and_wait`:
// no other phase has a job whose row a grant goes on.
func NewCodeCheckout(desk RunCheckouts) *CodeCheckout {
	if desk == nil {
		return nil
	}
	return &CodeCheckout{desk: desk}
}

// CodeCheckout is the `code_checkout` tool ([NewCodeCheckout]).
type CodeCheckout struct{ desk RunCheckouts }

var _ tools.SeatCallable = (*CodeCheckout)(nil)

// Name is the tool's wire name, [CodeCheckoutTool].
func (t *CodeCheckout) Name() string { return CodeCheckoutTool }

// Description is what the run is told the tool is for and when to use it.
func (t *CodeCheckout) Description() string {
	return "Check out a repository of this company's own code host into this run, one per call: it gives " +
		"this run access to the repository and answers the commands that clone it into your working " +
		"directory (a blobless clone: file contents arrive as git needs them), and the branch to work on. " +
		"Run them, then work and push as usual; your pushes are delivered when the run ends. Call it before " +
		"you touch a repository: nothing you did not check out is reachable. Repositories on another host " +
		"are cloned with that host's own credentials."
}

// Parameters is a checkout's schema: one repository, and how the run works on
// it.
func (t *CodeCheckout) Parameters() map[string]any {
	return map[string]any{
		"type":                 "object",
		"required":             []any{"repo"},
		"additionalProperties": false,
		"properties": map[string]any{
			"repo": map[string]any{"type": "string",
				"description": "The repository's name, as " + CodeListReposTool + " lists it."},
			"base": map[string]any{"type": "string",
				"description": "The branch, tag or commit to start from. Default: your branch if it exists, " +
					"else the default branch."},
			"branch": map[string]any{"type": "string",
				"description": "The branch to work on. Default: seats/<your handle>/<the work item's key>, or " +
					"seats/<your handle>/run-<id> when the turn is on no work item."},
			"access": map[string]any{"type": "string", "enum": []any{"read", "write"},
				"description": "write to push. Default: write where your grant lets you push the branch, " +
					"else read."},
			"paths": map[string]any{"type": "array", "maxItems": sandbox.MaxConePaths,
				"items": map[string]any{"type": "string"},
				"description": "Directories to check out (a sparse cone), for a large repository. Default: " +
					"the whole tree."},
			"change": map[string]any{"type": "string",
				"description": "A change to work on, by its number or reference (web!42): its branch, from " +
					"its head. Instead of `branch` and `base`."},
			"open_change": map[string]any{"type": "object", "additionalProperties": false,
				"description": "Open a change from the branch once the run succeeds: once, from what it " +
					"pushed, carrying your work item. Needs write.",
				"properties": map[string]any{
					"target": map[string]any{"type": "string",
						"description": "The branch it proposes into. Default: the default branch."},
					"title": map[string]any{"type": "string",
						"description": "Default: the subject of the commit the run delivers."},
					"draft": map[string]any{"type": "boolean"},
				}},
		},
	}
}

// Call is [CodeCheckout.CallForTurn] with no turn, which no run makes.
func (t *CodeCheckout) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

// CallForTurn checks one repository out to the job whose bridge made the
// call, or refuses it naming why.
func (t *CodeCheckout) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	const tool = CodeCheckoutTool
	launch := turnctx.JobOf(ctx)
	if turn == nil || turn.RunID == "" || launch == "" {
		// NOT A RUN'S CALL: nothing names the job whose row the grant goes
		// on. The tool is offered on an executor run's surface alone, so
		// this is a wiring fault, said as one rather than as the model's.
		return refusedBy(tools.RefusalUnavailable, waitless(), tool+" is unavailable here: it gives a "+
			"repository to a coding run, and this call was not made by one. Read the repository with "+
			CodeReadTool+" instead."), nil
	}
	req, refusal := checkoutArgs(args)
	if refusal != nil {
		return *refusal, nil
	}
	got, err := t.desk.Checkout(ctx, turn, launch, req)
	if err != nil {
		var composed *RefusedCheckout
		if errors.As(err, &composed) {
			return composed.Result, nil
		}
		return nodeFailure(ctx, tool, err, tool+" could not give this run repository "+
			fmt.Sprintf("%q", clip(req.Repo)), "Nothing was checked out: call it again."), nil
	}
	return checkoutAnswer(req, got), nil
}

// checkoutAnswer is what the run is told of a checkout: the commands that
// clone the repository, and the branch, base and access it was given — or why
// the job's row took no grant.
func checkoutAnswer(req RepoRequest, got Checkout) tools.Result {
	const tool = CodeCheckoutTool
	r := got.Repo
	switch got.Outcome {
	case sandbox.GrantAdded, sandbox.GrantHeld:
	case sandbox.GrantConflict:
		if r.RepoID == "" {
			return refused(tools.RefusalConflict, fmt.Sprintf("%s: this run works for one requester, and "+
				"its repositories were checked out for another: nothing was checked out.", tool))
		}
		return refused(tools.RefusalConflict, fmt.Sprintf("%s: this run has %s checked out already, on "+
			"branch %s to %s: one run works on one branch of a repository. Name that branch and access, or "+
			"leave them out, to check it out again.", tool, r.Repo, r.Branch, r.Access))
	case sandbox.GrantFull:
		return refused(tools.RefusalConflict, fmt.Sprintf("%s: this run has %d repositories checked out, "+
			"the most one run is given: nothing was checked out. Work on them, or end the run and start "+
			"the rest in another.", tool, sandbox.MaxRunRepos))
	default:
		return refused(tools.RefusalNotRunning, tool+": this run is no longer running, so nothing can be "+
			"checked out to it. End your turn.")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Repository %s is checked out to this run", r.Repo)
	if got.Outcome == sandbox.GrantHeld {
		b.WriteString(" (it was already)")
	}
	fmt.Fprintf(&b, ", on branch %s to %s", r.Branch, r.Access)
	if r.Base != "" {
		fmt.Fprintf(&b, ", starting from commit %s", r.Base)
	}
	if len(r.Paths) > 0 {
		fmt.Fprintf(&b, ", its checkout limited to %s", words.And(r.Paths))
	}
	b.WriteString(".")
	switch {
	case r.Change != nil:
		fmt.Fprintf(&b, " You are working on change %s: push to its branch to deliver to it.",
			strings.TrimSpace(req.Change))
	case r.Open != nil:
		fmt.Fprintf(&b, " A change into %s opens from what you push once the run succeeds.",
			strings.TrimPrefix(r.Open.Target, branchRoot))
	}
	b.WriteString(" Clone it by running these commands in your working directory, in order:\n\n```sh\n")
	for _, step := range got.Steps {
		for _, command := range step.Commands {
			b.WriteString(command)
			b.WriteString("\n")
		}
	}
	b.WriteString("```")
	// WHAT THE COMMANDS MAKE TRUE, each step's own brief: where the checkout
	// is, that its clone holds the default branch alone and how another
	// branch is fetched, and how it pushes — or that a push of a repository
	// given to read is refused. A run is given no repository at launch, so
	// no environment block names its checkouts; the answer that gives it one
	// is the one place it can be told, and without it the run spends rounds
	// rediscovering how its git reaches the code host.
	for _, step := range got.Steps {
		if brief := strings.TrimSpace(step.Brief); brief != "" {
			b.WriteString("\n\n")
			b.WriteString(brief)
		}
	}
	return tools.Result{Output: b.String()}
}

// checkoutKeys are the keys a checkout may hold, and openChangeKeys those its
// `open_change` may.
var (
	checkoutKeys   = []string{"repo", "base", "branch", "access", "paths", "change", "open_change"}
	openChangeKeys = []string{"target", "title", "draft"}
)

// checkoutArgs is the call's request, read STRICTLY — a key the schema does
// not name, a field the check at the surface would misread (a list or an
// object where a string belongs, anything but true or false where a bool
// does), an empty `repo`, an `access` outside its two values, a `change` beside the `branch`
// or `base` it decides, an `open_change` beside a `change` or asked with
// `read`, or a cone past its bound is refused naming the field, before the
// engine is asked.
//
// STRICT, unlike the tolerant readers beside it, because what a repository is
// given is a GRANT: an `acess: write` misspelt and dropped would hand the run
// the default access, which may be more than the model meant to ask for — and
// so would an `"access": ["read"]` read as no access asked, which argString,
// tolerant of a value of the wrong type by design, answers as "" (repoField).
// Strict is not a rule of its own, though: every value is read through
// argval, the reader the check at the surface admits it by (ADR-0059), so
// what the check lets through as lossless — a number or a bool where a string
// belongs, read as the model spelt it; one string where the list of `paths`
// belongs, read as argval reads a list of strings — is what the checkout
// asks for, and only what the check would misread is refused here.
func checkoutArgs(args map[string]any) (RepoRequest, *tools.Result) {
	refuse := func(format string, a ...any) (RepoRequest, *tools.Result) {
		r := failed(CodeCheckoutTool + ": " + fmt.Sprintf(format, a...) + " Nothing was checked out.")
		return RepoRequest{}, &r
	}
	for key := range args {
		if !slices.Contains(checkoutKeys, key) {
			return refuse("it was given %q, which is not one of %s.", clip(key), words.Or(checkoutKeys))
		}
	}
	var req RepoRequest
	for _, f := range []struct {
		key  string
		into *string
	}{{"repo", &req.Repo}, {"base", &req.Base}, {"branch", &req.Branch}, {"access", &req.Access},
		{"change", &req.Change}} {
		value, ok := repoField(args, f.key)
		if !ok {
			return refuse("`%s` is not a string: write it as one, or leave it out.", f.key)
		}
		*f.into = value
	}
	open, refusal := openChangeField(args)
	if refusal != nil {
		return RepoRequest{}, refusal
	}
	req.OpenChange = open
	switch {
	case req.Repo == "":
		return refuse("it needs `repo`: a repository's name, as %s lists it.", CodeListReposTool)
	case req.Access != "" && req.Access != string(sandbox.RepoRead) && req.Access != string(sandbox.RepoWrite):
		return refuse("`access` is %q: it is %q, %q, or left out for the most your grant allows.",
			clip(req.Access), sandbox.RepoRead, sandbox.RepoWrite)
	case req.Change != "" && (req.Branch != "" || req.Base != ""):
		// THE CHANGE DECIDES BOTH: its source branch, from its head. A branch
		// or a base beside it is a run told two places to start.
		return refuse("it names `change` beside `branch` or `base`: a change is worked on from its own " +
			"branch's head, so name one or the other.")
	case req.Change != "" && req.OpenChange != nil:
		return refuse("it names `change` and `open_change`: the change's branch heads it already, so the " +
			"run delivers to it. Leave `open_change` out.")
	case req.OpenChange != nil && req.Access == string(sandbox.RepoRead):
		return refuse("it asks for `open_change` with `access: read`: a run that pushes nothing delivers " +
			"nothing to open a change from.")
	}
	if paths, ok := args["paths"]; ok && paths != nil {
		list, ok := argval.StringItems(paths)
		if !ok {
			return refuse("`paths` must be a list of directories.")
		}
		if len(list) > sandbox.MaxConePaths {
			return refuse("`paths` names %d directories; a cone names at most %d.", len(list), sandbox.MaxConePaths)
		}
		for k, p := range list {
			// A null element is no path, though argval reads it as "": a cone
			// of "" would be the whole tree the cone was asked to narrow.
			s, ok := argval.String(p)
			if !ok || p == nil {
				return refuse("paths[%d] is not a directory's path.", k)
			}
			req.Paths = append(req.Paths, s)
		}
	}
	return req, nil
}

// repoField is one string field of a checkout, read by [argval.String] and
// trimmed — "" where the call leaves it out or sets it null, which the
// resolution reads as the field's default; a number or a bool as the model
// spelt it (`"change": 42` is change 42), as the check at the surface admits
// it — and false where it holds a list or an object. That is REFUSED rather
// than read as argString reads it (""): the default of every field is a
// choice made on the model's behalf (the most its grant allows, the seat's
// own branch, the branch's own head), so a value it did send in the wrong
// shape is a request it made, and is answered as one it must make again,
// never as one it did not make.
func repoField(fields map[string]any, key string) (string, bool) {
	v, ok := argval.String(fields[key])
	if !ok {
		return "", false
	}
	return strings.TrimSpace(v), true
}

// openChangeField is the checkout's `open_change`, read as strictly as the
// checkout — nil where it leaves the field out or sets it null; an object of
// only `target` and `title`, each a string, and `draft`, a bool; anything else
// refused naming it — for the checkout's reason: an ask the engine acts on as
// the seat once the run is done, which a misspelt key would have made another
// one.
func openChangeField(fields map[string]any) (*OpenChangeRequest, *tools.Result) {
	refuse := func(format string, a ...any) (*OpenChangeRequest, *tools.Result) {
		r := failed(CodeCheckoutTool + ": " + fmt.Sprintf(format, a...) + " Nothing was checked out.")
		return nil, &r
	}
	raw, named := fields["open_change"]
	if !named || raw == nil {
		return nil, nil
	}
	ask, ok := raw.(map[string]any)
	if !ok {
		return refuse("`open_change` is not an object: write {} for a change into the default branch, or " +
			"name its `target`, `title` and `draft`.")
	}
	for key := range ask {
		if !slices.Contains(openChangeKeys, key) {
			return refuse("open_change holds %q, which is not one of %s.", clip(key), words.Or(openChangeKeys))
		}
	}
	var out OpenChangeRequest
	for _, f := range []struct {
		key  string
		into *string
	}{{"target", &out.Target}, {"title", &out.Title}} {
		value, ok := repoField(ask, f.key)
		if !ok {
			return refuse("open_change.%s is not a string: write it as one, or leave it out.", f.key)
		}
		*f.into = value
	}
	if raw := ask["draft"]; raw != nil {
		draft, ok := argval.Bool(raw)
		if !ok {
			return refuse("open_change.draft is not true or false.")
		}
		out.Draft = draft
	}
	return &out, nil
}

// CheckoutUnavailable is a checkout this deployment cannot make at all — a
// company that hosts no repositories, a node with no address a box clones
// from — refused in words the run reads, naming what an operator changes. No
// wait clears a configuration, so it carries no wait ([waitless]): a run told
// to come back in two seconds would ask again for nothing.
func CheckoutUnavailable(why string) *RefusedCheckout {
	return &RefusedCheckout{Result: refusedBy(tools.RefusalUnavailable, waitless(), CodeCheckoutTool+": "+why+".")}
}

// CheckoutNotServedNow is a checkout this node cannot make while the code host
// it met is not served here (starting, stopping, or in a maintenance window):
// met is what it found, so [RetryAfter] answers it as that condition says, and
// an unknown one under this node's wait, because the next call may land once
// the code host is up.
func CheckoutNotServedNow(met error, why string) *RefusedCheckout {
	return &RefusedCheckout{Result: refusedBy(tools.RefusalUnavailable, met, CodeCheckoutTool+": "+why+".")}
}
