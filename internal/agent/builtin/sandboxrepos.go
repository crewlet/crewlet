package builtin

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
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
// nothing of it), every ref by the one ref query ([CodeDeps.refsOf], which
// takes the turn's hold on every seat's branch it answers), a base
// named by its id by the one read by object id ([CodeDeps.gitRead]), and all
// of them as the caller the code tools read as — decided again for whoever
// woke the turn ([CodeDeps.callerOn] on the code host's read verb), so a box
// is never given a repository its requester could not read.
//
// AND WHAT THE BOX MAY PUSH IS DECIDED FOR THAT REQUESTER TOO (M3-decisions
// U40 (a), its write half for a coding run): in a turn a colleague seat woke,
// the run's row names that seat ([RunRepos.Ceiling], the row's CodeCeiling),
// and the code host's box admission holds the box to the colleague's own
// grant on every request it makes ([code.Grant.Under]) — a read the colleague
// could not make, and a push outside the seat's own work the colleague could
// not make, are refused, while its own work — the branches under its own
// namespace, and every branch of a repository it owns — stays its own,
// which anybody who can message it steers. The launch prechecks the
// same rule ([runAccess]), so `access: write` on a branch the colleague could
// not push is refused before a box costs anything, and an access left out is
// write only where the box could push. A person bounds nothing, holding
// maintain on every repository, and a stranger's turn reads nothing at all.

// RunRepos is what the repositories one run_sandbox call names resolve to.
type RunRepos struct {
	// Repos are the run's grants, in the order the call named them.
	Repos []sandbox.RunRepo
	// Floor is the run's first code floor: the furthest position any read
	// of the resolution answered at, and this node's own writes to each
	// repository — a code_commit_files just before the launch must be in
	// the clone.
	Floor statelog.Position
	// Ceiling is the handle of the colleague seat whose ask woke the turn,
	// whose own grant bounds the run's box on every repository it was given
	// ([code.Grant.Under]) — the seat the resolution's reads carried as their
	// ceiling ([code.ReadQuery.CeilingSeat]) — and "" where no colleague seat
	// bounds the run.
	Ceiling string
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
//  2. the branch: the request's; a change's it names to work on — read
//     LINEARIZABLE, as every seat's read is, and refused closed or merged
//     ([CodeDeps.runChange]) — its source branch; or [code.SeatBranch] of the
//     seat, the turn's work item's key and the turn — held to
//     [gitref.ValidBranch]; and the branch a change it asks to open proposes
//     into ([openTarget]): the one it names, or the default branch;
//  3. ONE ref query for the branch, a base named as a ref, — where no base
//     is named — the default branch, and that branch the change proposes
//     into, which must exist; the turn's hold taken on every seat's branch
//     among them ([HoldRef]) and on the run's own branch where it does not
//     exist yet, before anything durable of the launch exists — and a
//     change's branch, a live change's source, must;
//  4. the access: `write` asked must pass, for the branch, the ref rule the
//     box will be judged by — the seat's grant held to a run given write and,
//     in a turn a colleague woke, to that colleague's grant outside the
//     seat's own work (code.Grant.Under: its own namespace, and a repository
//     it owns) — and the repository must not be archived; `read`
//     is read, and none asked is write where `write` would pass, else read;
//     a run that asks to open a change must come to write, or it could
//     deliver nothing to open one from;
//  5. the base: an id — whole or abbreviated — expanded and checked by a
//     read by object id; a ref, as the query answered it; none, the branch's
//     head where it exists — a change's head, for a run on one — else the
//     default branch's, and an EMPTY repository (no default branch) none;
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
	out := RunRepos{Ceiling: c.ceiling}
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
	change, at, refusal := d.runChange(ctx, turn, c, req, repo)
	if refusal != nil {
		return sandbox.RunRepo{}, statelog.Position{}, refusal
	}
	if change != nil {
		branch = strings.TrimPrefix(change.SourceRef, branchRoot)
		floor = statelog.Later(floor, at)
	}
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

	target, refusal := openTarget(req, repo, branch)
	if refusal != nil {
		return sandbox.RunRepo{}, statelog.Position{}, refusal
	}

	// ONE READ for every ref the launch needs of the repository: the run's
	// branch, a base named as a ref, the default branch where no base is
	// named, and the branch a change it asks to open proposes into — one
	// barrier, all of them at one checkpoint.
	names := []string{"refs/heads/" + branch}
	baseAt, defaultAt, targetAt := -1, -1, -1
	switch {
	case req.Base == "" && repo.DefaultBranch != "":
		defaultAt = slices.Index(names, "refs/heads/"+repo.DefaultBranch)
		if defaultAt < 0 {
			defaultAt, names = len(names), append(names, "refs/heads/"+repo.DefaultBranch)
		}
	case req.Base != "" && !code.ValidRev(req.Base):
		baseAt, names = len(names), append(names, req.Base)
	}
	if target != "" {
		if targetAt = slices.Index(names, target); targetAt < 0 {
			targetAt, names = len(names), append(names, target)
		}
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
	switch {
	case change != nil && head.Missing != "":
		// A LIVE CHANGE'S SOURCE IS LIVE — deleting it closes the change in
		// the same record — so the branch was deleted between the two reads,
		// and the change with it.
		r := refusedBy(tools.RefusalNotFound, fmt.Errorf("%w: %s", code.ErrRefNotFound, head.Missing),
			fmt.Sprintf("%s: %s's branch %s was deleted as the run was launched, which closes the change; "+
				"nothing was launched — read it with %s.", tool, change.Ref, change.SourceRef, CodeGetChangeTool))
		return sandbox.RunRepo{}, statelog.Position{}, &r
	case targetAt >= 0 && answer.Refs[targetAt].Missing != "":
		r := codeFailure(ctx, tool, repo.Name, fmt.Errorf("%w: %s — name an existing branch as "+
			"open_change's `target`", code.ErrRefNotFound, answer.Refs[targetAt].Missing))
		return sandbox.RunRepo{}, statelog.Position{}, &r
	}
	if head.Missing != "" {
		// THE RUN'S OWN BRANCH, NOT THERE YET: held with no read, as a branch
		// a write creates is — no deletion can have listed it, and the box's
		// first push sets it. Every ref the read answered is held already
		// (CodeDeps.refsOf), so every ref a launch resolves is on the turn's
		// set before BeginLaunch writes that set on the run's row, and a
		// relaunch, which replaces the row's grants, keeps them held.
		if err := HoldRef(ctx, turn, d.Holder, c.actor, repo, names[0], nil, statelog.Position{},
			d.Now); err != nil {
			r := refHoldFailure(ctx, tool, repo, names[0], err)
			return sandbox.RunRepo{}, statelog.Position{}, &r
		}
	}

	// THE GRANT THE BOX WILL BE JUDGED BY, from this node's chart: the
	// seat's, held to a run given write — what `write` puts on the row — and,
	// in a turn a colleague woke, to that colleague's own grant outside the
	// seat's namespace, as the code host's box admission holds it.
	boxGrant := grant(c.asker, current).Within(code.AccessWrite)
	if c.ceiling != "" {
		boxGrant = boxGrant.Under(grant(code.Principal{Kind: code.PrincipalAgent, Seat: c.ceiling}, current))
	}
	access, refusal := runAccess(req, branch, head, current, boxGrant)
	if refusal != nil {
		return sandbox.RunRepo{}, statelog.Position{}, refusal
	}
	if req.OpenChange != nil && access != sandbox.RepoWrite {
		// AN ACCESS LEFT OUT came to read: the box could push nothing, so
		// it could deliver nothing to open a change from.
		r := refusedBy(tools.RefusalForbidden, code.ErrNotPermitted, fmt.Sprintf("%s: repository %q's run "+
			"asks for open_change on %s, which it may not push, so it could deliver nothing to open a change "+
			"from: name a `branch` it may push — leave `branch` out for your own — or leave open_change out.",
			tool, repo.Name, branch))
		return sandbox.RunRepo{}, statelog.Position{}, &r
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
	if change != nil {
		out.Change = &sandbox.RunChange{ID: change.ID, Number: change.Number}
	}
	if req.OpenChange != nil {
		out.Open = &sandbox.OpenChange{Target: target, Title: req.OpenChange.Title, Draft: req.OpenChange.Draft}
	}
	if err := out.ValidateFor(sandbox.CloneCoding); err != nil {
		r := failed(fmt.Sprintf("%s: repository %q cannot be given to a run as named: %v.", tool, repo.Name,
			err))
		return sandbox.RunRepo{}, statelog.Position{}, &r
	}
	return out, floor, nil
}

// runAccess is what a run may do with a repository — the request's access,
// held to g, the grant its box will be judged by on the run's branch — or the
// refusal: `write` asked on an archived repository, or on a branch g's ref
// rule refuses (the seat's, or the colleague's its turn works for), is refused
// with the code host's sentence; none asked is write exactly where write
// would pass, so a read-only seat naming a repository to look at is not
// refused, and a run that forgot the field can deliver where its box may.
//
// A PRECHECK, from this node's chart: a box's every push is decided again at
// the code host's admission and at its decide, so this decides only whether a
// run that could never deliver is refused before a box costs anything — which
// is why g is the box's grant, held to a run given write, and not the seat's:
// a branch the seat may push and its box may not (another seat's namespace,
// which needs maintain) is a run that could never deliver.
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
		"to write may not push %s: %s. Ask for `access: read`, or name a `branch` this run may push — "+
		"leave `branch` out for your own.", tool, repo.Name, branch, why))
	return "", &r
}

// runChange is the change a request names to work on, read once at the seat's
// level — LINEARIZABLE ([CodeDeps.changeOf]), held to the caller's grant on
// the repository — with the position it was read at; nil for a request that
// names none. A change that is no longer live is refused naming what became
// of it: a closed one's work is over unless it is reopened, and a merged
// one's is in its target, which a run starts from by naming it as `base`.
func (d CodeDeps) runChange(ctx context.Context, turn *turnctx.Turn, c codeCaller, req RepoRequest,
	repo code.RepoRef) (*code.ChangeDetail, statelog.Position, *tools.Result) {

	const tool = RunSandboxTool
	if req.Change == "" {
		return nil, statelog.Position{}, nil
	}
	s := req.Change
	if name, n, ok := strings.Cut(s, "!"); ok {
		if name != req.Repo {
			r := failed(fmt.Sprintf("%s: repository %q's `change` %q names a change of repository %q — name "+
				"the change by its number on %q.", tool, clip(req.Repo), clip(req.Change), clip(name),
				clip(req.Repo)))
			return nil, statelog.Position{}, &r
		}
		s = n
	}
	number, err := strconv.ParseInt(s, 10, 64)
	if err != nil || number < 1 {
		r := failed(fmt.Sprintf("%s: repository %q's `change` %q is no change: name it by its number (42) or "+
			"its reference (%s), as %s lists it.", tool, clip(req.Repo), clip(req.Change),
			code.ChangeRef(repo.Name, 42), CodeListChangesTool))
		return nil, statelog.Position{}, &r
	}
	written, refusal := d.changeOf(ctx, turn, tool, c, repo, number)
	if refusal != nil {
		return nil, statelog.Position{}, refusal
	}
	change := written.detail
	switch change.State {
	case code.ChangeStateClosed:
		r := refused(tools.RefusalConflict, fmt.Sprintf("%s: %s is closed, so a run delivers "+
			"to no change on its branch; reopen it with %s, or name its branch as `branch`.", tool, change.Ref,
			CodeCloseChangeTool))
		return nil, statelog.Position{}, &r
	case code.ChangeStateMerged:
		r := refused(tools.RefusalConflict, fmt.Sprintf("%s: %s is merged, its work in %s; "+
			"name that branch as `base` to build on it.", tool, change.Ref, change.TargetRef))
		return nil, statelog.Position{}, &r
	}
	return change, written.at, nil
}

// openTarget is the branch, whole, a request's ask to open a change proposes
// the run's branch into — the one it names, else the repository's default
// branch — held to the rules an open is, before anything is read; "" for a
// request that asks for none. A title it names is held to a change's title's
// rule here too ([code.TitleProblem]), so an ask the collection's open would
// refuse costs no box.
func openTarget(req RepoRequest, repo code.RepoRef, branch string) (string, *tools.Result) {
	const tool = RunSandboxTool
	ask := req.OpenChange
	if ask == nil {
		return "", nil
	}
	target := strings.TrimPrefix(ask.Target, branchRoot)
	if target == "" {
		target = repo.DefaultBranch
	}
	why := ""
	switch {
	case target == "":
		why = "the repository has no default branch yet, so name the branch it proposes into as `target`"
	case !gitref.ValidBranch(target):
		why = fmt.Sprintf("`target` %q is not a branch name git accepts", clip(target))
	case target == branch:
		why = fmt.Sprintf("it would propose %s into itself: name another `target`", branch)
	case ask.Title != "" && code.TitleProblem(ask.Title) != "":
		why = "`title` is " + code.TitleProblem(ask.Title) + " — a title is one line of at most " +
			strconv.Itoa(code.MaxChangeTitleBytes) + " bytes"
	}
	if why != "" {
		r := failed(fmt.Sprintf("%s: repository %q's open_change: %s.", tool, repo.Name, why))
		return "", &r
	}
	return branchRoot + target, nil
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
