package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Changes names each branch a collected run pushed by the live change it
// heads, and opens the changes the run asked to open — see
// [sandbox.RunPushes.Changes].
//
// ONE `branch_changes` READ of every branch the run pushed on the
// repositories it was given to push to and the branch of each ask to open one,
// every repository in it — a second only for a run that pushed more branches
// than one read answers ([branchQueries]) — at SESSION FLOORED AT THE PUSH
// RECORD'S POSITION ([sandbox.PushRecord.At]): a delivered ref named by no
// change because a node behind answered is the stale "none" a collection must
// never say, and the record's linearizable first page already put the floor
// past every push the run made and every change those pushes opened — so the
// read answers what a linearizable one would, and the collection pays the
// record's one barrier on the code log and no second. Its rows are what the
// repositories hold, not who may read them: the engine names its own run's
// refs, on repositories the run was given.
//
// AN ASK OPENS ITS CHANGE ONCE A LAUNCH ([openOpID]): from the branch the
// run's push record says it delivered, as the seat it ran for, carrying the
// work item the run was charged to and naming the turn and the run that
// opened it, held to the colleague whose ask woke the launching turn as the
// box's pushes are ([openCeiling]) — the open a code_open_change of the same
// turn would make, decided by the repository's committer at or past the read,
// from its own chart. Asked again by a
// retried collection, on this node or another, the open is the same
// operation: the read finds the change it opened and names it opened, or the
// operation ledger answers the record that landed.
func (p runPushes) Changes(ctx context.Context, run sandbox.PendingRun, record sandbox.PushRecord,
	open bool) ([]sandbox.PushedRef, []sandbox.OpenedChange, error) {

	pushed := record.Refs
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
			ref.New != gitlet.ZeroOID {
			add(r.RepoID, ref.Ref)
		}
	}
	if len(branches) == 0 {
		return pushed, nil, nil
	}
	// THE RECORD'S POSITION IS ITS TOKEN, which the sandbox carries from
	// Pushed to here without reading it ([sandbox.PushRecord.At]).
	recordAt, err := statelog.ParsePosition(record.At)
	switch {
	case err != nil:
		return pushed, nil, fmt.Errorf("engine: read the changes run %s's branches head: its push record's "+
			"position %q cannot be read: %w", run.TurnID, record.At, err)
	case recordAt.IsZero() || recordAt.Stream != topics.CodeLogStream:
		return pushed, nil, fmt.Errorf("engine: read the changes run %s's branches head: its push record was "+
			"read at %s, no position of the code log to read them at", run.TurnID, recordAt)
	}
	reader, err := p.code()
	if err != nil {
		return pushed, nil, err
	}
	heads := map[pushKey]gitlet.BranchChange{}
	at := recordAt
	for _, q := range branchQueries(repos, branches) {
		answer, err := reader.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadBranchChanges, BranchChanges: &q,
			Fresh: statelog.FloorAt(statelog.ReadSession, recordAt)})
		switch {
		case err != nil:
			return pushed, nil, fmt.Errorf("engine: read the changes run %s's branches head: %w", run.TurnID, err)
		case answer.Position.IsZero() || answer.Position.Stream != topics.CodeLogStream:
			return pushed, nil, fmt.Errorf("engine: read the changes run %s's branches head: answered at %s, no "+
				"position of the code log", run.TurnID, answer.Position)
		}
		at = statelog.Later(at, answer.Position)
		for _, bc := range answer.BranchChanges {
			heads[pushKey{repo: bc.RepoID, ref: bc.Ref}] = bc
		}
	}
	named := slices.Clone(pushed)
	for i, ref := range named {
		if r, given := byName[ref.Repo]; given {
			if bc, heading := heads[pushKey{repo: r.RepoID, ref: ref.Ref}]; heading {
				named[i].Change = gitlet.ChangeRef(r.Repo, bc.Number)
			}
		}
	}
	if !open {
		return named, nil, nil
	}
	// THE ASKS ARE OPENED AT ONCE, one goroutine an ask, each owned and
	// awaited here and stopped with ctx: each is its own repository's, under
	// its own operation (openOpID) and decided by that repository's
	// committer, so none waits on another, and in turn a collection waits for
	// its slowest open rather than for the sum of them — a title read by
	// object id and a committer write that may wait up to statelog.StallGrace
	// for its record to apply, each, while the run's seat is held for its
	// collection. Bounded by the run's repositories, sandbox.MaxRunRepos (8)
	// at most, which a run's grant is held to; one ask a repository.
	type ask struct {
		repo      sandbox.RunRepo
		delivered int // the index in named of the branch it is opened from, -1 for none
	}
	var asks []ask
	for _, r := range repos {
		if r.Open == nil {
			continue
		}
		source := branchRef(r.Branch)
		asks = append(asks, ask{repo: r, delivered: slices.IndexFunc(named, func(ref sandbox.PushedRef) bool {
			return ref.Repo == r.Repo && ref.Ref == source
		})})
	}
	if len(asks) == 0 {
		return named, nil, nil
	}
	opened := make([]sandbox.OpenedChange, len(asks))
	var wg sync.WaitGroup
	for i, a := range asks {
		// A COPY OF THE DELIVERED REF, which the open only reads: named is
		// written below, once every open has answered.
		var delivered *sandbox.PushedRef
		if a.delivered >= 0 {
			ref := named[a.delivered]
			delivered = &ref
		}
		bc, heading := heads[pushKey{repo: a.repo.RepoID, ref: branchRef(a.repo.Branch)}]
		wg.Go(func() { opened[i] = p.openOne(ctx, reader, run, a.repo, delivered, bc, heading, at) })
	}
	wg.Wait()
	for i, a := range asks {
		if o := opened[i]; a.delivered >= 0 && named[a.delivered].Change == "" && o.Opened &&
			strings.Contains(o.Change, "!") {
			// THE CHANGE IT OPENED NAMES THE REF IT WAS OPENED FROM, as
			// every change a delivered ref heads does.
			named[a.delivered].Change = o.Change
		}
	}
	return named, opened, nil
}

// branchRef is branch as a ref names it, under refs/heads/.
func branchRef(branch string) string { return "refs/heads/" + branch }

// branchQueries packs the branches of each of repos — branches by repository
// id — into `branch_changes` queries, in repos' order: as few as hold them,
// each naming up to gitlet.MaxRefsPerQuery branches across every repository it
// names. ONE for every run but one that pushed more branches than that: a
// record holds sandbox.MaxPushedRefs (1 024) refs, beside one branch an ask,
// so a run that pushed past a thousand branches is read in two, its
// repository's branches split across them. A query names at most len(repos)
// repositories — a run's, which gitlet.MaxRunRepos bounds as the read does.
func branchQueries(repos []sandbox.RunRepo, branches map[string][]string) []gitlet.BranchChangesQuery {
	var out []gitlet.BranchChangesQuery
	var q gitlet.BranchChangesQuery
	held := 0
	for _, r := range repos {
		for refs := branches[r.RepoID]; len(refs) > 0; {
			n := min(len(refs), gitlet.MaxRefsPerQuery-held)
			q.Repos = append(q.Repos, gitlet.RepoBranches{RepoID: r.RepoID, Refs: refs[:n]})
			held, refs = held+n, refs[n:]
			if held == gitlet.MaxRefsPerQuery {
				out, q, held = append(out, q), gitlet.BranchChangesQuery{}, 0
			}
		}
	}
	if held > 0 {
		out = append(out, q)
	}
	return out
}

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
	delivered *sandbox.PushedRef, live gitlet.BranchChange, heading bool, at statelog.Position) sandbox.OpenedChange {

	out := sandbox.OpenedChange{Repo: r.Repo, Branch: r.Branch}
	op := openOpID(run, r.RepoID)
	id := gitlet.OpenChangeID(op)
	switch {
	case heading:
		out.Change, out.Opened = gitlet.ChangeRef(r.Repo, live.Number), live.ChangeID == id
		return out
	case delivered == nil:
		out.Unopened = "the run pushed nothing to its branch, so there was no work to propose"
		return out
	case delivered.New == gitlet.ZeroOID:
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
		item = &types.WorkItem{Backend: w.Backend, ID: gitlet.FitProvenance(w.ID), Key: gitlet.FitProvenance(w.Key),
			Project: gitlet.FitProvenance(w.Project)}
	}
	// WHOM THE OPEN IS MADE FOR (gitlet.ChangeProposal.WrittenFor): the run's
	// row holds no carrier for its code writes, its code ceiling naming a
	// colleague seat alone, so the change its collection opens is stated made
	// for nobody, the seat's own work — a carrier its committer holds to no
	// ceiling, whichever the row names.
	res, err := reader.Change(ctx, gitlet.ChangeProposal{OpID: op,
		Asker: gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: run.AgentHandle}, Seen: at,
		Ceiling: openCeiling(run), WrittenFor: types.ForTurn(nil),
		Open: &gitlet.ChangeOpen{RepoID: r.RepoID, ChangeID: id, SourceRef: branchRef(r.Branch),
			TargetRef: r.Open.Target, Title: title, Draft: r.Open.Draft, WorkItem: item, TurnID: run.TurnID,
			RunID: run.LaunchID}})
	// THE FACT ALONE, never an instruction: the run's report is read by the
	// reviewer as well as the seat, and a step it names ("read it with ...")
	// would be the engine telling a phase that holds no such tool what to do.
	switch {
	case err != nil:
		out.Unopened = fmt.Sprintf("asking Gitlet to open it failed (%v), so whether it opened is unknown", err)
	case res.Refusal != "":
		out.Unopened = fmt.Sprintf("Gitlet refused it (%s): %s", res.Refusal, res.Detail)
	case res.Outcome == statelog.OutcomeApplied || res.Outcome == statelog.OutcomePending:
		out.Opened = true
		out.Change = gitlet.ChangeRef(r.Repo, res.Number)
		if res.Number == 0 {
			// A PENDING OPEN IS NUMBERED BY ITS APPLY, which none has run
			// where the answer was formed.
			out.Change = "a change of " + r.Repo + " not numbered yet"
		}
	default:
		out.Unopened = "whether Gitlet opened it is unknown"
	}
	if out.Unopened != "" {
		log.WarnContext(ctx, "sandbox_change_unopened", "turn_id", run.TurnID, "launch_id", run.LaunchID,
			"repo", r.Repo, "branch", r.Branch, "detail", out.Unopened)
	}
	return out
}

// openCeiling is whom the open a run asked for is made for (gitlet.WriteCeiling):
// the colleague SEAT whose ask woke the launching turn, as the run's row names
// it (sandbox.PendingRun.CodeCeiling) — the seat the box's admission already
// holds every push of the run to (gitlet.Grant.Under) — and nil for a run no
// colleague seat bounds: a turn nobody woke, one the seat itself woke, and one
// a person woke, who bounds nothing.
//
// THE OPEN IS THE TURN'S OWN WRITE, made late: a code_open_change of the same
// turn carries the same ceiling (internal/agent/builtin's codeCaller), and the
// committer judges it against its own chart when it decides the open
// (gitlet.WriteCeiling's changeRefusal) — never the launching node's one
// resolution of the repositories, which a later activation may have moved —
// sparing the seat's own work, an open of a branch under its own namespace or
// in a repository it owns, as it spares the tool's. Left off, a colleague an
// activation shut out of the repository between the launch and the collection
// would have a change opened for them that their own ask could no longer make.
func openCeiling(run sandbox.PendingRun) *gitlet.WriteCeiling {
	if run.CodeCeiling == "" {
		return nil
	}
	return &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingSeat, Seat: run.CodeCeiling}
}

// deliveredTitle is the title of the change a run's ask opens without naming
// one: the subject of the commit the run delivered its branch at — what that
// commit says it does, as a push's options title theirs — read by object id
// as the seat, at or past the read that found the branch; and, where it cannot
// be read or is no title, the branch's name and then the commit's id, each
// made a title (gitlet.DeliveredTitle) — never the name raw, which may hold what
// a title may not: an ask is never left unopened for its title.
func (p runPushes) deliveredTitle(ctx context.Context, reader pushReader, run sandbox.PendingRun,
	r sandbox.RunRepo, head string, at statelog.Position) string {

	answer, err := reader.GitRead(ctx, gitlet.GitQuery{Kind: gitlet.GitLog, RepoID: r.RepoID,
		Asker: gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: run.AgentHandle}, Turn: run.TurnID,
		Fresh: statelog.FloorAt(statelog.ReadSession, at), Log: &gitlet.LogQuery{Rev: head, Limit: 1}})
	if err != nil {
		log.WarnContext(ctx, "sandbox_change_title_unread", "turn_id", run.TurnID, "repo", r.Repo,
			"commit", head, "error", err.Error())
		return gitlet.DeliveredTitle("", r.Branch, head)
	}
	subject := ""
	if answer.Log != nil && len(answer.Log.Commits) == 1 {
		subject = answer.Log.Commits[0].Subject
	}
	return gitlet.DeliveredTitle(subject, r.Branch, head)
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
