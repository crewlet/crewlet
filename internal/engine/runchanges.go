package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Changes names each branch a collected run pushed by the live change it
// heads, and opens the changes the run asked to open — see
// [sandbox.RunPushes.Changes].
//
// ONE `branch_changes` READ A REPOSITORY the run was given to push to, of
// every branch it pushed there and the branch of its ask to open one: the
// first LINEARIZABLE, as the push record's first page is — a delivered ref
// named by no change because a node behind answered is the stale "none" a
// collection must never say — and every later one at session at the first's
// position, paying no barrier of its own. Its rows are what the repository
// holds, not who may read them: the engine names its own run's refs, on
// repositories the run was given.
//
// AN ASK OPENS ITS CHANGE ONCE A LAUNCH ([openOpID]): from the branch the
// run's push record says it delivered, as the seat it ran for, carrying the
// work item the run was charged to and naming the turn and the run that
// opened it — the open a code_open_change of the same turn would make,
// decided by the repository's committer at or past the read. Asked again by a
// retried collection, on this node or another, the open is the same
// operation: the read finds the change it opened and names it opened, or the
// operation ledger answers the record that landed.
func (p runPushes) Changes(ctx context.Context, run sandbox.PendingRun, pushed []sandbox.PushedRef,
	open bool) ([]sandbox.PushedRef, []sandbox.OpenedChange, error) {

	repos := sandbox.Pushable(run.Repos)
	byName := make(map[string]sandbox.RunRepo, len(repos))
	branches := make(map[string][]string, len(repos))
	add := func(repoID, ref string) {
		if !slices.Contains(branches[repoID], ref) {
			branches[repoID] = append(branches[repoID], ref)
		}
	}
	for _, r := range repos {
		byName[r.Repo] = r
		if open && r.Open != nil {
			add(r.RepoID, branchRef(r.Branch))
		}
	}
	for _, ref := range pushed {
		if r, given := byName[ref.Repo]; given && strings.HasPrefix(ref.Ref, branchRef("")) &&
			ref.New != code.ZeroOID {
			add(r.RepoID, ref.Ref)
		}
	}
	if len(branches) == 0 {
		return pushed, nil, nil
	}
	reader, err := p.code()
	if err != nil {
		return pushed, nil, err
	}
	heads := map[pushKey]code.BranchChange{}
	var at statelog.Position
	for _, r := range repos {
		for refs := branches[r.RepoID]; len(refs) > 0; {
			n := min(len(refs), code.MaxRefsPerQuery)
			fresh := statelog.Freshness{Level: statelog.ReadLinearizable}
			if !at.IsZero() {
				fresh = statelog.FloorAt(statelog.ReadSession, at)
			}
			answer, err := reader.Read(ctx, code.ReadQuery{Kind: code.ReadBranchChanges,
				BranchChanges: &code.BranchChangesQuery{RepoID: r.RepoID, Refs: refs[:n]}, Fresh: fresh})
			switch {
			case err != nil:
				return pushed, nil, fmt.Errorf("engine: read the changes run %s's branches of %s head: %w",
					run.TurnID, r.Repo, err)
			case answer.Position.IsZero() || answer.Position.Stream != topics.CodeLogStream:
				return pushed, nil, fmt.Errorf("engine: read the changes run %s's branches of %s head: answered "+
					"at %s, no position of the code log", run.TurnID, r.Repo, answer.Position)
			}
			at = statelog.Later(at, answer.Position)
			for _, bc := range answer.BranchChanges {
				heads[pushKey{repo: r.RepoID, ref: bc.Ref}] = bc
			}
			refs = refs[n:]
		}
	}
	named := slices.Clone(pushed)
	for i, ref := range named {
		if r, given := byName[ref.Repo]; given {
			if bc, heading := heads[pushKey{repo: r.RepoID, ref: ref.Ref}]; heading {
				named[i].Change = code.ChangeRef(r.Repo, bc.Number)
			}
		}
	}
	if !open {
		return named, nil, nil
	}
	var opened []sandbox.OpenedChange
	for _, r := range repos {
		if r.Open == nil {
			continue
		}
		source := branchRef(r.Branch)
		var delivered *sandbox.PushedRef
		if i := slices.IndexFunc(named, func(ref sandbox.PushedRef) bool {
			return ref.Repo == r.Repo && ref.Ref == source
		}); i >= 0 {
			delivered = &named[i]
		}
		bc, heading := heads[pushKey{repo: r.RepoID, ref: source}]
		o := p.openOne(ctx, reader, run, r, delivered, bc, heading, at)
		if delivered != nil && delivered.Change == "" && o.Opened && strings.Contains(o.Change, "!") {
			// THE CHANGE IT OPENED NAMES THE REF IT WAS OPENED FROM, as
			// every change a delivered ref heads does.
			delivered.Change = o.Change
		}
		opened = append(opened, o)
	}
	return named, opened, nil
}

// branchRef is branch as a ref names it, under refs/heads/.
func branchRef(branch string) string { return "refs/heads/" + branch }

// openOne is what becomes of run's ask to open a change from repository r's
// branch: delivered is that branch as the push record says the run left it,
// nil for a branch it never pushed (or one past the refs the record holds,
// sandbox.MaxPushedRefs, which a run's own branch — its first push — is
// not); live the change the branch heads where heading is set, read at at,
// which the open is decided at or past.
//
// A branch heading a change is never opened again: the one the run's own
// launch opened, found by the id the open derives (a collection retried
// after its open landed), is named opened, and any other — a `git push -o
// change` of the run's own, or anybody's — is named as the change the run
// delivered to.
func (p runPushes) openOne(ctx context.Context, reader pushReader, run sandbox.PendingRun, r sandbox.RunRepo,
	delivered *sandbox.PushedRef, live code.BranchChange, heading bool, at statelog.Position) sandbox.OpenedChange {

	out := sandbox.OpenedChange{Repo: r.Repo, Branch: r.Branch}
	op := openOpID(run, r.RepoID)
	id := code.OpenChangeID(op)
	switch {
	case heading:
		out.Change, out.Opened = code.ChangeRef(r.Repo, live.Number), live.ChangeID == id
		return out
	case delivered == nil:
		out.Unopened = "the run pushed nothing to its branch, so there was no work to propose"
		return out
	case delivered.New == code.ZeroOID:
		out.Unopened = "the run deleted its branch, so there was no work to propose"
		return out
	}
	title := r.Open.Title
	if title == "" {
		title = p.deliveredTitle(ctx, reader, run, r, delivered.New, at)
	}
	var item *types.WorkItem
	if w := run.WorkItem; w != nil && w.ID != "" {
		// FITTED, as a change write's item is (the code tools'
		// workItemArg): another system's labels, copied as they arrived, and
		// never a reason to leave the change unopened.
		item = &types.WorkItem{Backend: w.Backend, ID: code.FitProvenance(w.ID), Key: code.FitProvenance(w.Key),
			Project: code.FitProvenance(w.Project)}
	}
	res, err := reader.Change(ctx, code.ChangeProposal{OpID: op,
		Asker: code.Principal{Kind: code.PrincipalAgent, Seat: run.AgentHandle}, Seen: at,
		Open: &code.ChangeOpen{RepoID: r.RepoID, ChangeID: id, SourceRef: branchRef(r.Branch),
			TargetRef: r.Open.Target, Title: title, Draft: r.Open.Draft, WorkItem: item, TurnID: run.TurnID,
			RunID: run.LaunchID}})
	switch {
	case err != nil:
		out.Unopened = fmt.Sprintf("asking the code host to open it failed (%v), so whether it opened is unknown: "+
			"read its branch's change with code_list_changes before you open it with code_open_change", err)
	case res.Refusal != "":
		out.Unopened = fmt.Sprintf("the code host refused it (%s): %s", res.Refusal, res.Detail)
	case res.Outcome == statelog.OutcomeApplied || res.Outcome == statelog.OutcomePending:
		out.Opened = true
		out.Change = code.ChangeRef(r.Repo, res.Number)
		if res.Number == 0 {
			// A PENDING OPEN IS NUMBERED BY ITS APPLY, which none has run
			// where the answer was formed.
			out.Change = "a change of " + r.Repo + " not numbered yet (read it with code_list_changes)"
		}
	default:
		out.Unopened = "whether the code host opened it is unknown: read its branch's change with " +
			"code_list_changes before you open it with code_open_change"
	}
	if out.Unopened != "" {
		log.WarnContext(ctx, "sandbox_change_unopened", "turn_id", run.TurnID, "launch_id", run.LaunchID,
			"repo", r.Repo, "branch", r.Branch, "detail", out.Unopened)
	}
	return out
}

// deliveredTitle is the title of the change a run's ask opens without naming
// one: the subject of the commit the run delivered its branch at — what that
// commit says it does, as a push's options title theirs (code.AsTitle) — read
// by object id as the seat, at or past the read that found the branch; and,
// where it cannot be read or is no title, the branch's name: an ask is never
// left unopened for its title.
func (p runPushes) deliveredTitle(ctx context.Context, reader pushReader, run sandbox.PendingRun,
	r sandbox.RunRepo, head string, at statelog.Position) string {

	answer, err := reader.GitRead(ctx, code.GitQuery{Kind: code.GitLog, RepoID: r.RepoID,
		Asker: code.Principal{Kind: code.PrincipalAgent, Seat: run.AgentHandle}, Turn: run.TurnID,
		Fresh: statelog.FloorAt(statelog.ReadSession, at), Log: &code.LogQuery{Rev: head, Limit: 1}})
	if err != nil {
		log.WarnContext(ctx, "sandbox_change_title_unread", "turn_id", run.TurnID, "repo", r.Repo,
			"commit", head, "error", err.Error())
		return r.Branch
	}
	if answer.Log != nil && len(answer.Log.Commits) == 1 {
		if title := code.AsTitle(answer.Log.Commits[0].Subject); title != "" {
			return title
		}
	}
	return r.Branch
}

// openOpID is the operation id of the open run's launch asks of repository
// repoID at its collection: a STEP of the launch's own operation
// (statelog.StepOpID), DERIVED from the launch — its id and the instant it
// began, both on the run's row — so every collection of one launch, on any
// node and after any restart, asks the same open, which the operation ledger
// answers once, while every launch asks its own.
func openOpID(run sandbox.PendingRun, repoID string) string {
	launch := statelog.DeriveOpID(run.Launch.StartedAt, "", "sandbox.launch", run.LaunchID)
	return statelog.StepOpID(launch, "open-change-"+repoID)
}
