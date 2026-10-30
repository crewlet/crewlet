package builtin

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// A CODING RUN'S REPOSITORIES ON THE COMPANY'S OWN CODE HOST, resolved at its
// launch into the run's grants (ADR-0063): each repository a run_sandbox call
// names becomes a [sandbox.RunRepo] on the run's row — its id beside its
// current name, the commit the run starts from, the branch it works on, what
// it may do and its cone — and the position the launch read them at becomes
// the run's first code floor.
//
// THROUGH THE CODE TOOLS' OWN SEAMS, never a second copy of them: the
// repository by the one resolution of a `repo` argument ([CodeDeps.repo],
// which takes the turn's hold on a scratch repository AFTER the read that
// judged the seat's grant, so a seat that may not read a repository holds
// nothing of it), every ref by the one ref query ([CodeDeps.refsOf]), a base
// named by its id by the one read by object id ([CodeDeps.gitRead]), and all
// of them as the caller the code tools read as — decided again for whoever
// woke the turn ([CodeDeps.callerOn] on the code host's read verb), so a box
// is never given a repository its requester could not read.

// RunRepos is what the repositories one run_sandbox call names resolve to.
type RunRepos struct {
	// Repos are the run's grants, in the order the call named them.
	Repos []sandbox.RunRepo
	// Floor is the run's first code floor: the furthest position any read
	// of the resolution answered at, and this node's own writes to each
	// repository — a code_commit_files just before the launch must be in
	// the clone.
	Floor statelog.Position
}

// GrantFunc is a seat's grant on a repository as this node's chart reads it
// ([code.GrantOf] over the engine's roster): the launch's PRECHECK of the
// access it gives a run, never its authority — every box push is judged
// again at the code host's admission and at its decide.
type GrantFunc func(asker code.Principal, repo code.RepoRef) code.Grant

// ResolveRunRepos resolves the repositories a run_sandbox call names, for
// turn's seat, into the run's grants and its first code floor — refusing, as
// a [*RefusedLaunch] whose result the tool answers as it is, any repository
// the seat may not be given, naming it and why. Called by the launcher AFTER
// its own refusals and before anything durable of the launch exists, since a
// resolution takes the turn's holds and reads at the seat's level.
//
// For each request, in order:
//
//  1. the repository, by [CodeDeps.repo] — judged by the node that answers
//     for the seat and for whoever its turn acts for, and held; a deleted or
//     purging one is refused in the code host's words;
//  2. the branch: the request's, or [code.SeatBranch] of the seat, the
//     turn's work item's key and the turn — held to [gitref.ValidBranch];
//  3. ONE ref query for the branch, a base named as a ref, and — where no
//     base is named — the default branch;
//  4. the access: `write` asked must pass the seat's grant's ref rule for
//     the branch (and the repository must not be archived), `read` is read,
//     and none asked is write where `write` would pass, else read;
//  5. the base: an id — whole or abbreviated — expanded and checked by a
//     read by object id; a ref, as the query answered it; none, the branch's
//     head where it exists, else the default branch's, and an EMPTY
//     repository (no default branch) none;
//  6. the cone, each path cleaned and held to the box's grammar.
func (d CodeDeps) ResolveRunRepos(ctx context.Context, turn *turnctx.Turn, reqs []RepoRequest,
	grant GrantFunc) (RunRepos, error) {

	const tool = RunSandboxTool
	if len(reqs) == 0 {
		return RunRepos{}, nil
	}
	if !d.serves() || grant == nil {
		return RunRepos{}, errors.New("builtin: this node resolves no repository of the code host for a run")
	}
	c, refusal := d.callerOn(ctx, turn, tool, authz.ActionCodeRead)
	if refusal != nil {
		return RunRepos{}, &RefusedLaunch{Result: *refusal}
	}
	if c.asker.Kind != code.PrincipalAgent {
		return RunRepos{}, &RefusedLaunch{Result: refused(tools.RefusalForbidden,
			"run_sandbox gives repositories to a seat's coding run, and this call is no seat's.")}
	}
	var out RunRepos
	byID := map[string]string{}
	for _, req := range reqs {
		r, floor, refusal := d.runRepo(ctx, turn, c, req, grant)
		if refusal != nil {
			return RunRepos{}, &RefusedLaunch{Result: *refusal}
		}
		if first, named := byID[r.RepoID]; named {
			return RunRepos{}, &RefusedLaunch{Result: failed(fmt.Sprintf("run_sandbox: %s and %s name one "+
				"repository, %s; one run works on one branch of a repository, so name it once.", clip(first),
				clip(req.Repo), r.Repo))}
		}
		byID[r.RepoID] = req.Repo
		out.Repos = append(out.Repos, r)
		out.Floor = statelog.Later(out.Floor, floor)
	}
	return out, nil
}

// runRepo is one request's grant on the run and the furthest position its
// reads answered at — or the refusal, naming the repository.
func (d CodeDeps) runRepo(ctx context.Context, turn *turnctx.Turn, c codeCaller, req RepoRequest,
	grant GrantFunc) (sandbox.RunRepo, statelog.Position, *tools.Result) {

	const tool = RunSandboxTool
	if !code.ValidName(req.Repo) {
		r := failed(fmt.Sprintf("%s: %q is not a repository's name — a name is lowercase letters, digits and "+
			"hyphens, as %s lists it.", tool, clip(req.Repo), CodeListReposTool))
		return sandbox.RunRepo{}, statelog.Position{}, &r
	}
	repo, refusal := d.repo(ctx, turn, tool, c, req.Repo)
	if refusal != nil {
		return sandbox.RunRepo{}, statelog.Position{}, refusal
	}
	floor := statelog.Later(repo.Position, d.Code.FloorOf(repo.ID))

	branch := req.Branch
	if branch == "" {
		var err error
		if branch, err = code.SeatBranch(c.asker.Seat, workItemKey(turn), turn.RunID); err != nil {
			r := faulted(ctx, tool, err, fmt.Sprintf("%s could not name the branch this run works on in "+
				"repository %q: %s.", tool, repo.Name, faultSaid))
			return sandbox.RunRepo{}, statelog.Position{}, &r
		}
	}
	if !gitref.ValidBranch(branch) {
		r := failed(fmt.Sprintf("%s: %q is not a branch name git accepts; name another `branch` for "+
			"repository %q, or leave it out for your own.", tool, clip(branch), repo.Name))
		return sandbox.RunRepo{}, statelog.Position{}, &r
	}

	// ONE READ for every ref the launch needs of the repository: the run's
	// branch, a base named as a ref, and the default branch where no base is
	// named — one barrier, all of them at one checkpoint.
	names := []string{"refs/heads/" + branch}
	baseAt, defaultAt := -1, -1
	switch {
	case req.Base == "" && repo.DefaultBranch != "":
		defaultAt = slices.Index(names, "refs/heads/"+repo.DefaultBranch)
		if defaultAt < 0 {
			defaultAt, names = len(names), append(names, "refs/heads/"+repo.DefaultBranch)
		}
	case req.Base != "" && !code.ValidRev(req.Base):
		baseAt, names = len(names), append(names, req.Base)
	}
	answer, refusal := d.refsOf(ctx, turn, tool, c, repo, names...)
	if refusal != nil {
		return sandbox.RunRepo{}, statelog.Position{}, refusal
	}
	floor = statelog.Later(floor, answer.Position)
	current := answer.Repo
	if current.ID == "" {
		current = repo
	}
	switch current.Status {
	case code.RepoStatusDeleted, code.RepoStatusPurging:
		r := refusedBy(tools.RefusalNotFound, fmt.Errorf("%w: %s", code.ErrRepoNotFound, current.Name),
			fmt.Sprintf("%s: %s; nothing was launched.", tool, code.RepoGone{ID: current.ID, Name: current.Name,
				Status: current.Status, DeletedAt: current.DeletedAt, ManagedBy: current.ManagedBy}.Detail()))
		return sandbox.RunRepo{}, statelog.Position{}, &r
	}
	head := answer.Refs[0]

	access, refusal := runAccess(req, branch, head, current, grant(c.asker, current))
	if refusal != nil {
		return sandbox.RunRepo{}, statelog.Position{}, refusal
	}

	var base string
	switch {
	case req.Base != "" && code.ValidRev(req.Base):
		// AN ID, whole or abbreviated, read by object id: expanded to the
		// commit it names, and a commit the repository does not hold is
		// refused here rather than at the clone, inside a box.
		got, refusal := d.gitRead(ctx, turn, tool, c, repo, code.GitQuery{Kind: code.GitTree, RepoID: repo.ID,
			Fresh: statelog.FloorAt(statelog.ReadSession, floor), Tree: &code.TreeQuery{Rev: req.Base}})
		if refusal != nil {
			if tools.RefusalOf(*refusal) == tools.RefusalNotFound {
				r := refusedBy(tools.RefusalNotFound, refusal.Cause, fmt.Sprintf("%s: repository %q holds no "+
					"commit %s; name a commit it holds, a branch or a tag as `base`.", tool, repo.Name,
					clip(req.Base)))
				return sandbox.RunRepo{}, statelog.Position{}, &r
			}
			return sandbox.RunRepo{}, statelog.Position{}, refusal
		}
		base, floor = got.Rev, statelog.Later(floor, got.Position)
	case baseAt >= 0:
		ref := answer.Refs[baseAt]
		if ref.Missing != "" {
			r := codeFailure(ctx, tool, repo.Name, fmt.Errorf("%w: %s", code.ErrRefNotFound, ref.Missing))
			return sandbox.RunRepo{}, statelog.Position{}, &r
		}
		base = ref.OID
	case head.Missing == "":
		// THE RUN'S BRANCH EXISTS: a second run on one work item continues
		// what the first pushed.
		base = head.OID
	case defaultAt >= 0 && answer.Refs[defaultAt].Missing == "":
		base = answer.Refs[defaultAt].OID
	}
	// ELSE AN EMPTY REPOSITORY, whose default branch holds no commit: the
	// run's first commit starts its branch.

	paths := make([]string, len(req.Paths))
	for i, p := range req.Paths {
		paths[i] = path.Clean(strings.TrimSpace(p))
		if why := sandbox.ConePathProblem(paths[i]); why != "" {
			r := failed(fmt.Sprintf("%s: repository %q's paths[%d] %q %s.", tool, repo.Name, i, clip(p), why))
			return sandbox.RunRepo{}, statelog.Position{}, &r
		}
	}
	out := sandbox.RunRepo{Repo: current.Name, RepoID: current.ID, Base: base, Branch: branch, Access: access,
		Paths: paths}
	if err := out.ValidateFor(sandbox.CloneCoding); err != nil {
		r := failed(fmt.Sprintf("%s: repository %q cannot be given to a run as named: %v.", tool, repo.Name,
			err))
		return sandbox.RunRepo{}, statelog.Position{}, &r
	}
	return out, floor, nil
}

// runAccess is what a run may do with a repository — the request's access,
// held to the seat's grant on the run's branch — or the refusal: `write` asked
// on an archived repository, or on a branch the grant's ref rule refuses, is
// refused with the code host's sentence; none asked is write exactly where
// write would pass, so a read-only seat naming a repository to look at is not
// refused, and a run that forgot the field can deliver where its seat may.
//
// A PRECHECK, from this node's chart: a box's every push is decided again at
// the code host's admission and at its decide, so this decides only whether a
// run that could never deliver is refused before a box costs anything.
func runAccess(req RepoRequest, branch string, head code.ResolvedRef, repo code.RepoRef,
	g code.Grant) (sandbox.RepoAccess, *tools.Result) {

	const tool = RunSandboxTool
	if req.Access == string(sandbox.RepoRead) {
		return sandbox.RepoRead, nil
	}
	why := ""
	if repo.Status == code.RepoStatusArchived {
		why = code.ArchivedDetail(repo.Name, repo.ManagedBy) + "; a run may read it, not push"
	} else {
		old := head.OID
		if head.Missing != "" {
			old = code.ZeroOID
		}
		// THE NEW SIDE IS ANY OBJECT: the rule reads only whether a command
		// creates, moves or deletes a ref.
		if refusal, sentence := g.RefRule(code.Command{Ref: "refs/heads/" + branch, Old: old,
			New: strings.Repeat("f", gitref.OIDHexLen)}); refusal != "" {
			why = sentence
		}
	}
	switch {
	case why == "":
		return sandbox.RepoWrite, nil
	case req.Access == "":
		return sandbox.RepoRead, nil
	}
	r := refusedBy(tools.RefusalForbidden, code.ErrNotPermitted, fmt.Sprintf("%s: a run given repository %q "+
		"to write may not push %s: %s. Ask for `access: read`, or name a `branch` your grant lets you push.",
		tool, repo.Name, branch, why))
	return "", &r
}

// workItemKey is the key of the work item turn is charged to, as its tracker
// shows it — never the turn's dedupe key over its trigger events — or "" for a
// turn charged to none.
func workItemKey(turn *turnctx.Turn) string {
	if turn == nil || turn.WorkItem == nil {
		return ""
	}
	return turn.WorkItem.Key
}
