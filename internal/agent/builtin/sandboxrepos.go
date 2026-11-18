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
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
)

// RepoRequest is one repository a code_checkout call names, as the model
// wrote it: resolved by the engine ([CodeDeps.ResolveRunRepos]) into the
// run's grant on it, never trusted.
type RepoRequest struct {
	// Repo is the repository's name, as code_list_repos answers it.
	Repo string
	// Base is a branch, a tag or a commit id to start from; "" is the run's
	// branch where it exists, else the default branch.
	Base string
	// Branch is the branch the run works on; "" is the seat's own branch for
	// the turn's work item (gitlet.SeatBranch).
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

// OpenChangeRequest is a code_checkout call's ask to open a change from the
// run's branch at its collection: the branch it proposes into ("" for the
// default branch), its title ("" for the delivered commit's subject) and
// whether it opens as a draft.
type OpenChangeRequest struct {
	Target string
	Title  string
	Draft  bool
}

// RefusedCheckout is a checkout refused with a result already composed and
// classed (a repository the seat may not be given, in Gitlet's own words),
// which the tool answers as it is rather than as a checkout the engine could
// not do.
type RefusedCheckout struct{ Result tools.Result }

func (r *RefusedCheckout) Error() string { return r.Result.Output }

// Unwrap is the refusal's class and cause, so a caller branching on either
// reads the result's own.
func (r *RefusedCheckout) Unwrap() error { return r.Result.Cause }

// An executor's repositories on Gitlet (ADR-0093) each become a
// [sandbox.RunRepo] on the run's row, read through the code tools' own seams
// ([CodeDeps.repo], [CodeDeps.refsOf], [CodeDeps.gitRead]) as the caller they
// read as ([CodeDeps.callerOn]), so a box is never given a repository its
// requester could not read, and a seat that may not read one holds nothing of
// it. What the box may push is bounded by that requester too (M3-decisions
// U40 (a)), prechecked by [runAccess]: a waking colleague's grant
// ([RunRepos.Ceiling], [gitlet.Grant.Under]) or credential's (ADR-0110); a
// person, holding maintain everywhere, bounds nothing. Whom the turn works
// for rides on the row ([RunRepos.WorkingFor], ADR-0109) as its caller
// composed it ([writtenForTurn]), never re-derived from the requester, which
// would lose this node's narrowing of a credential's apps.

// RunRepos is what the repositories a checkout names resolve to.
type RunRepos struct {
	// Repos are the run's grants, in the order the call named them.
	Repos []sandbox.RunRepo
	// Heads are where each grant's branch stood as the resolution read it,
	// in Repos' order: its head commit, "" where the branch does not exist
	// yet. A box that lost its clone continues from it where its job holds
	// the grant already ([sandbox.GrantHeld]).
	Heads []string
	// Floor is the run's first code floor: the furthest position any read
	// of the resolution answered at, and this node's own writes to each
	// repository — a code_commit_files just before the checkout must be in
	// the clone.
	Floor statelog.Position
	// Ceiling is the handle of the colleague seat whose ask woke the turn,
	// whose grant bounds the run's box ([gitlet.Grant.Under]) as it bounded
	// the resolution's reads ([gitlet.ReadQuery.CeilingSeat]); "" for none.
	Ceiling string
	// WorkingFor is whom the turn works for, as its own code writes state it
	// ([writtenForTurn]): a colleague's seat, a person's, or a credential
	// acting as no seat with the apps this node holds it to; nil for the
	// seat's own work. The box's pushes and the change the run's collection
	// opens state it too.
	WorkingFor *types.WorkingFor
}

// GrantFunc is a seat's grant on a repository as this node's chart reads it
// ([gitlet.GrantOf] over the engine's roster): the checkout's PRECHECK of the
// access it gives a run, never its authority — every box push is judged
// again at Gitlet's admission and at its decide.
type GrantFunc func(asker gitlet.Principal, repo gitlet.RepoRef) gitlet.Grant

// ResolveRunRepos resolves the repositories a code_checkout call names, for
// turn's seat, into the run's grants and the code floor they were read at, or
// refuses with a [*RefusedCheckout] naming the repository and why. The
// engine's checkout calls it before writing the grant to the run's row, since
// it takes the turn's holds. Per request: the repository ([CodeDeps.repo]);
// the branch (named, a live change's source by [CodeDeps.runChange], else
// [gitlet.SeatBranch]); ONE ref query for every ref it needs, holding each
// seat's branch and the run's own one not there yet ([HoldRef]); the access
// ([runAccess]), which open_change needs to be write; the base (an id read by
// object id, a ref as queried, else the branch's head, else the default
// branch's, none in an empty repository); and the cone, held to the box's
// grammar.
func (d CodeDeps) ResolveRunRepos(ctx context.Context, turn *turnctx.Turn, reqs []RepoRequest,
	grant GrantFunc) (RunRepos, error) {

	const tool = CodeCheckoutTool
	if len(reqs) == 0 {
		return RunRepos{}, nil
	}
	if !d.serves() || grant == nil {
		return RunRepos{}, errors.New("builtin: this node resolves no repository on Gitlet for a run")
	}
	c, refusal := d.callerOn(ctx, turn, tool, authz.ActionCodeRead)
	if refusal != nil {
		return RunRepos{}, &RefusedCheckout{Result: *refusal}
	}
	if c.asker.Kind != gitlet.PrincipalAgent {
		return RunRepos{}, &RefusedCheckout{Result: refused(tools.RefusalForbidden,
			CodeCheckoutTool+" gives repositories to a seat's executor, and this call is no seat's.")}
	}
	out := RunRepos{Ceiling: c.ceiling, WorkingFor: c.actor.WrittenFor.Carrier()}
	byID := map[string]string{}
	for _, req := range reqs {
		got, refusal := d.runRepo(ctx, turn, c, req, grant)
		if refusal != nil {
			return RunRepos{}, &RefusedCheckout{Result: *refusal}
		}
		r := got.grant
		if first, named := byID[r.RepoID]; named {
			return RunRepos{}, &RefusedCheckout{Result: failed(fmt.Sprintf(CodeCheckoutTool+": %s and %s name one "+
				"repository, %s; one run works on one branch of a repository, so name it once.", clip(first),
				clip(req.Repo), r.Repo))}
		}
		byID[r.RepoID] = req.Repo
		out.Repos = append(out.Repos, r)
		out.Heads = append(out.Heads, got.head)
		out.Floor = statelog.Later(out.Floor, got.floor)
	}
	return out, nil
}

// resolvedRepo is what one request resolves to: its grant on the run, where
// the grant's branch stands ([RunRepos.Heads]) and the furthest position its
// reads answered at.
type resolvedRepo struct {
	grant sandbox.RunRepo
	head  string
	floor statelog.Position
}

// runRepo resolves one request — or refuses it, naming the repository.
func (d CodeDeps) runRepo(ctx context.Context, turn *turnctx.Turn, c codeCaller, req RepoRequest,
	grant GrantFunc) (resolvedRepo, *tools.Result) {

	const tool = CodeCheckoutTool
	if !gitlet.ValidName(req.Repo) {
		r := failed(fmt.Sprintf("%s: %q is not a repository's name — a name is lowercase letters, digits and "+
			"hyphens, as %s lists it.", tool, clip(req.Repo), CodeListReposTool))
		return resolvedRepo{}, &r
	}
	repo, refusal := d.repo(ctx, turn, tool, c, req.Repo)
	if refusal != nil {
		return resolvedRepo{}, refusal
	}
	floor := statelog.Later(repo.Position, d.Code.FloorOf(repo.ID))

	branch := req.Branch
	change, at, refusal := d.runChange(ctx, turn, c, req, repo)
	if refusal != nil {
		return resolvedRepo{}, refusal
	}
	if change != nil {
		branch = strings.TrimPrefix(change.SourceRef, branchRoot)
		floor = statelog.Later(floor, at)
	}
	if branch == "" {
		var err error
		if branch, err = gitlet.SeatBranch(c.asker.Seat, workItemKey(turn), turn.RunID); err != nil {
			r := faulted(ctx, tool, err, fmt.Sprintf("%s could not name the branch this run works on in "+
				"repository %q: %s.", tool, repo.Name, faultSaid))
			return resolvedRepo{}, &r
		}
	}
	if !gitref.ValidBranch(branch) {
		r := failed(fmt.Sprintf("%s: %q is not a branch name git accepts; name another `branch` for "+
			"repository %q, or leave it out for your own.", tool, clip(branch), repo.Name))
		return resolvedRepo{}, &r
	}

	target, refusal := openTarget(req, repo, branch)
	if refusal != nil {
		return resolvedRepo{}, refusal
	}

	// ONE READ for every ref the checkout needs, so all are read at one
	// checkpoint.
	names := []string{"refs/heads/" + branch}
	baseAt, defaultAt, targetAt := -1, -1, -1
	switch {
	case req.Base == "" && repo.DefaultBranch != "":
		defaultAt = slices.Index(names, "refs/heads/"+repo.DefaultBranch)
		if defaultAt < 0 {
			defaultAt, names = len(names), append(names, "refs/heads/"+repo.DefaultBranch)
		}
	case req.Base != "" && !gitlet.ValidRev(req.Base):
		baseAt, names = len(names), append(names, req.Base)
	}
	if target != "" {
		if targetAt = slices.Index(names, target); targetAt < 0 {
			targetAt, names = len(names), append(names, target)
		}
	}
	answer, refusal := d.refsOf(ctx, turn, tool, c, repo, names...)
	if refusal != nil {
		return resolvedRepo{}, refusal
	}
	floor = statelog.Later(floor, answer.Position)
	current := answer.Repo
	if current.ID == "" {
		current = repo
	}
	switch current.Status {
	case gitlet.RepoStatusDeleted, gitlet.RepoStatusPurging:
		r := refusedBy(tools.RefusalNotFound, fmt.Errorf("%w: %s", gitlet.ErrRepoNotFound, current.Name),
			fmt.Sprintf("%s: %s; nothing was checked out.", tool, gitlet.RepoGone{ID: current.ID, Name: current.Name,
				Status: current.Status, DeletedAt: current.DeletedAt, ManagedBy: current.ManagedBy}.Detail()))
		return resolvedRepo{}, &r
	}
	head := answer.Refs[0]
	switch {
	case change != nil && head.Missing != "":
		// Deleting a live change's source closes the change in the same
		// record, so both went between the two reads.
		r := refusedBy(tools.RefusalNotFound, fmt.Errorf("%w: %s", gitlet.ErrRefNotFound, head.Missing),
			fmt.Sprintf("%s: %s's branch %s was deleted as it was checked out, which closes the change; "+
				"nothing was checked out — read it with %s.", tool, change.Ref, change.SourceRef, CodeGetChangeTool))
		return resolvedRepo{}, &r
	case targetAt >= 0 && answer.Refs[targetAt].Missing != "":
		r := codeFailure(ctx, tool, repo.Name, fmt.Errorf("%w: %s — name an existing branch as "+
			"open_change's `target`", gitlet.ErrRefNotFound, answer.Refs[targetAt].Missing))
		return resolvedRepo{}, &r
	}
	if head.Missing != "" {
		// THE RUN'S OWN BRANCH, NOT THERE YET: held with no read, as a branch
		// a write creates is (no deletion can have listed it). With refsOf's
		// holds, every ref a checkout resolves is on the turn's set before
		// the grant is written; the job's row carries it from launch
		// (PendingStore.AddCodeHolds), so a relaunch keeps them held.
		if err := HoldRef(ctx, turn, d.Holder, c.actor, repo, names[0], nil, statelog.Position{},
			d.Now); err != nil {
			r := refHoldFailure(ctx, tool, repo, names[0], err)
			return resolvedRepo{}, &r
		}
	}

	// THE GRANT THE BOX WILL BE JUDGED BY, from this node's chart: the
	// seat's, held to write and, in a turn a colleague woke, under that
	// colleague's own, as Gitlet's box admission holds it.
	boxGrant := grant(c.asker, current).Within(gitlet.AccessWrite)
	if w := c.actor.WrittenFor.Carrier(); c.ceiling != "" {
		boxGrant = boxGrant.Under(grant(gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: c.ceiling}, current))
	} else if w != nil && w.Kind == types.WorkingForCredential {
		// A CREDENTIAL ACTING AS NO SEAT pushes nowhere itself, so its run
		// writes the seat's own work alone (gitlet.CredentialPrincipal).
		boxGrant = boxGrant.Under(grant(gitlet.CredentialPrincipal(w.Login), current))
	}
	access, refusal := runAccess(req, branch, head, current, boxGrant)
	if refusal != nil {
		return resolvedRepo{}, refusal
	}
	if req.OpenChange != nil && access != sandbox.RepoWrite {
		// AN ACCESS LEFT OUT came to read: the box could deliver nothing to
		// open a change from.
		r := refusedBy(tools.RefusalForbidden, gitlet.ErrNotPermitted, fmt.Sprintf("%s: repository %q's run "+
			"asks for open_change on %s, which it may not push, so it could deliver nothing to open a change "+
			"from: name a `branch` it may push — leave `branch` out for your own — or leave open_change out.",
			tool, repo.Name, branch))
		return resolvedRepo{}, &r
	}

	var base string
	switch {
	case req.Base != "" && gitlet.ValidRev(req.Base):
		// AN ID, whole or abbreviated, read by object id: expanded, and one
		// the repository does not hold refused here rather than in a box.
		got, refusal := d.gitRead(ctx, turn, tool, c, repo, gitlet.GitQuery{Kind: gitlet.GitTree, RepoID: repo.ID,
			Fresh: statelog.FloorAt(statelog.ReadSession, floor), Tree: &gitlet.TreeQuery{Rev: req.Base}})
		if refusal != nil {
			if tools.RefusalOf(*refusal) == tools.RefusalNotFound {
				r := refusedBy(tools.RefusalNotFound, refusal.Cause, fmt.Sprintf("%s: repository %q holds no "+
					"commit %s; name a commit it holds, a branch or a tag as `base`.", tool, repo.Name,
					clip(req.Base)))
				return resolvedRepo{}, &r
			}
			return resolvedRepo{}, refusal
		}
		base, floor = got.Rev, statelog.Later(floor, got.Position)
	case baseAt >= 0:
		ref := answer.Refs[baseAt]
		if ref.Missing != "" {
			r := codeFailure(ctx, tool, repo.Name, fmt.Errorf("%w: %s", gitlet.ErrRefNotFound, ref.Missing))
			return resolvedRepo{}, &r
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
			return resolvedRepo{}, &r
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
		return resolvedRepo{}, &r
	}
	branchHead := ""
	if head.Missing == "" {
		branchHead = head.OID
	}
	return resolvedRepo{grant: out, head: branchHead, floor: floor}, nil
}

// runAccess is what a run may do with a repository: the request's access held
// to g, the grant its box will be judged by on the run's branch, or the
// refusal. `write` on a repository that takes no push (archived, importing, a
// pull mirror) or on a branch g's ref rule refuses is refused in Gitlet's
// words; none asked is write exactly where write would pass, so a read-only
// seat naming a repository to look at is not refused.
//
// A PRECHECK: Gitlet's admission and decide judge every push again, so this
// only refuses a run that could never deliver before a box costs anything.
// Hence g is the box's grant, not the seat's: a branch the seat may push and
// its box may not (another seat's namespace, which needs maintain) could
// never deliver.
func runAccess(req RepoRequest, branch string, head gitlet.ResolvedRef, repo gitlet.RepoRef,
	g gitlet.Grant) (sandbox.RepoAccess, *tools.Result) {

	const tool = CodeCheckoutTool
	if req.Access == string(sandbox.RepoRead) {
		return sandbox.RepoRead, nil
	}
	why := ""
	switch {
	case repo.Status == gitlet.RepoStatusArchived:
		why = gitlet.ArchivedDetail(repo.Name, repo.ManagedBy) + "; a run may read it, not push"
	case repo.Status == gitlet.RepoStatusImporting:
		why = gitlet.ImportingDetail(repo.Name) + "; a run may read it, not push"
	case repo.Mirrored:
		why = gitlet.MirroredDetail(repo.Name, repo.ManagedBy) + "; a run may read it, not push"
	default:
		old := head.OID
		if head.Missing != "" {
			old = gitlet.ZeroOID
		}
		// THE NEW SIDE IS ANY OBJECT: the rule reads only whether a command
		// creates, moves or deletes a ref.
		if refusal, sentence := g.RefRule(gitlet.Command{Ref: "refs/heads/" + branch, Old: old,
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
	r := refusedBy(tools.RefusalForbidden, gitlet.ErrNotPermitted, fmt.Sprintf("%s: a run given repository %q "+
		"to write may not push %s: %s. Ask for `access: read`, or name a `branch` this run may push — "+
		"leave `branch` out for your own.", tool, repo.Name, branch, why))
	return "", &r
}

// runChange is the change a request names to work on, with the position it
// was read at, or nil for none: its row alone ([changeRead.row]), since a
// launch needs none of its detail, read LINEARIZABLE at the seat's level. A
// closed change is refused (its work is over unless reopened), as is a merged
// one (its work is in its target, which a run names as `base`).
func (d CodeDeps) runChange(ctx context.Context, turn *turnctx.Turn, c codeCaller, req RepoRequest,
	repo gitlet.RepoRef) (*gitlet.ChangeSummary, statelog.Position, *tools.Result) {

	const tool = CodeCheckoutTool
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
	// argval.Whole, as every code tool reads `change`, so a change one tool
	// takes is one this takes.
	number, ok := argval.Whole(s)
	if !ok || number < 1 {
		r := failed(fmt.Sprintf("%s: repository %q's `change` %q is no change: name it by its number (42) or "+
			"its reference (%s), as %s lists it.", tool, clip(req.Repo), clip(req.Change),
			gitlet.ChangeRef(repo.Name, 42), CodeListChangesTool))
		return nil, statelog.Position{}, &r
	}
	change, at, refusal := (&changeRead{deps: d, turn: turn, tool: tool, c: c, name: repo.Name, repo: repo,
		number: int64(number)}).row(ctx, statelog.Position{})
	if refusal != nil {
		return nil, statelog.Position{}, refusal
	}
	switch change.State {
	case gitlet.ChangeStateClosed:
		r := refused(tools.RefusalConflict, fmt.Sprintf("%s: %s is closed, so a run delivers "+
			"to no change on its branch; reopen it with %s, or name its branch as `branch`.", tool, change.Ref,
			CodeCloseChangeTool))
		return nil, statelog.Position{}, &r
	case gitlet.ChangeStateMerged:
		r := refused(tools.RefusalConflict, fmt.Sprintf("%s: %s is merged, its work in %s; "+
			"name that branch as `base` to build on it.", tool, change.Ref, change.TargetRef))
		return nil, statelog.Position{}, &r
	}
	return change, at, nil
}

// openTarget is the whole branch a request's open_change proposes the run's
// branch into (the one it names, else the default branch), or "" for none.
// It and the title ([gitlet.TitleProblem]) are held to an open's rules before
// anything is read, so an ask the collection's open would refuse costs no box.
func openTarget(req RepoRequest, repo gitlet.RepoRef, branch string) (string, *tools.Result) {
	const tool = CodeCheckoutTool
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
	case ask.Title != "" && gitlet.TitleProblem(ask.Title) != "":
		why = "`title` is " + gitlet.TitleProblem(ask.Title) + " — a title is one line of at most " +
			strconv.Itoa(gitlet.MaxChangeTitleBytes) + " bytes"
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
