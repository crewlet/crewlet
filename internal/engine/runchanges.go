package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Changes names each branch a collected run pushed by the live change it
// heads, and opens the changes the run asked for ([sandbox.RunPushes.Changes]).
//
// One `branch_changes` read (two past [branchQueries]' limit) at session
// floored at the push record's position ([sandbox.PushRecord.At]), which is
// past every push of the run and every change they opened: it answers as a linearizable read would,
// never a lagging node's stale "none", for no second barrier. It is unscoped:
// it names only the run's own refs, on repositories the run was given.
//
// Each ask opens once a launch ([openOpID]), so a retried collection on any
// node finds its change or the ledger's record; the open is the one the
// launching turn's code_open_change would make ([openOne], [openCeiling]).
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
	// The record's position is opaque to the sandbox ([sandbox.PushRecord.At]).
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
	// The asks open concurrently, each goroutine awaited here and stopped by ctx:
	// each is its own repository's operation ([openOpID]) decided by its own
	// committer, and each may wait up to statelog.StallGrace for its record while
	// the run's seat is held, so the collection waits for the slowest, not the sum.
	// Bounded by sandbox.MaxRunRepos (8), one ask a repository.
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
		// A copy: named is written below, once every open has answered.
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
			// The ref an open came from names its change, as any ref heading one does.
			named[a.delivered].Change = o.Change
		}
	}
	return named, opened, nil
}

// branchRef is branch as a ref names it, under refs/heads/.
func branchRef(branch string) string { return "refs/heads/" + branch }

// branchQueries packs each repository's branches (branches by repository id)
// into as few `branch_changes` queries as hold them, in repos' order, each
// naming up to gitlet.MaxRefsPerQuery branches. Only a run that pushed past
// that many needs two: a record holds sandbox.MaxPushedRefs (1 024) refs plus
// one branch an ask, a repository's branches split across them. A query names
// at most len(repos) repositories, which gitlet.MaxRunRepos bounds as the read
// does.
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
// branch. delivered is that branch as the push record left it, nil where the
// run never pushed it or it is past the record's sandbox.MaxPushedRefs (a run's
// own branch, its first push, never is); live is the change it heads where heading
// is set, read at at, which the open is decided at or past.
//
// A branch heading a change is never opened again: the change this launch
// opened (matched by its derived id, after a retried collection) is named
// opened, and any other, the run's own `git push -o change` or anybody's, is
// named as the change the run delivered to.
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
		// Fitted as a change write's item is (the code tools' workItemArg): another
		// system's labels are never a reason to leave the change unopened.
		item = &types.WorkItem{Backend: w.Backend, ID: gitlet.FitProvenance(w.ID), Key: gitlet.FitProvenance(w.Key),
			Project: gitlet.FitProvenance(w.Project)}
	}
	// Made for whom the launching turn works for (sandbox.PendingRun.CodeWorkingFor),
	// as the open is that turn's write made late; nobody, stated, the seat's own
	// work, for a run working for its seat or a row from a build before the field. Passed as
	// the row holds it, even a newer build's value: a committer that cannot read it
	// answers as another build's node and the walk moves on, and where every one
	// does (mid-roll), or the committer refuses it on a chart moved since the
	// checkout, the open ran nowhere ([ranNowhere]).
	res, err := reader.Change(ctx, gitlet.ChangeProposal{OpID: op,
		Asker: gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: run.AgentHandle}, Seen: at,
		Ceiling: openCeiling(run), WrittenFor: types.ForTurn(run.CodeWorkingFor),
		Open: &gitlet.ChangeOpen{RepoID: r.RepoID, ChangeID: id, SourceRef: branchRef(r.Branch),
			TargetRef: r.Open.Target, Title: title, Draft: r.Open.Draft, WorkItem: item, TurnID: run.TurnID,
			RunID: run.LaunchID}})
	// The fact alone, never an instruction: the reviewer reads the report as well
	// as the seat, and a step it named would direct a phase holding no such tool.
	// The one act a line names is opening again, and it says whose: the seat's
	// ([ranNowhere]).
	switch line, nothingRan := ranNowhere(err); {
	case nothingRan:
		out.Unopened = line
	case err != nil:
		out.Unopened = fmt.Sprintf("asking Gitlet to open it failed (%v), so whether it opened is unknown", err)
	case res.Refusal != "":
		out.Unopened = fmt.Sprintf("Gitlet refused it (%s): %s", res.Refusal, res.Detail)
	case res.Outcome == statelog.OutcomeApplied || res.Outcome == statelog.OutcomePending:
		out.Opened = true
		out.Change = gitlet.ChangeRef(r.Repo, res.Number)
		if res.Number == 0 {
			// A pending open is numbered by its apply, which had not run when answered.
			out.Change = "a change of " + r.Repo + " not numbered yet"
		}
	default:
		out.Unopened = "whether Gitlet opened it is unknown"
	}
	if out.Unopened != "" {
		attrs := []any{"turn_id", run.TurnID, "launch_id", run.LaunchID, "repo", r.Repo, "branch", r.Branch,
			"detail", out.Unopened}
		if err != nil {
			// The failure's own words are logged whatever the line says (a ran-nowhere
			// line leaves them out): who was asked and what each said (refused keys,
			// the refusing nodes, the estate's upgrade instruction) are the operator's
			// to act on, which neither reader of the report can.
			attrs = append(attrs, "error", err.Error())
		}
		log.WarnContext(ctx, "sandbox_change_unopened", attrs...)
	}
	return out
}

// ranNowhere is the report line for an open the estate's answer says ran
// nowhere, and whether it says so. Definite for a walk that found no committer
// to run it, none gone silent ([estate.ErrUnserved]): [unopenedUnreadable]
// where one refused a key its build does not declare ([estate.ErrUnknownKeys]:
// args.written_for to an older build mid-roll, a newer row's value to this
// one), else [unopenedUnserved]; and for a refusal as no proposal a committer
// can decide ([gitlet.ErrInvalidProposal]), given before anything is appended
// ([unopenedRefused]). Any other answer may have run and keeps its own words as
// unknown: a committer gone silent or never heard from ([estate.ErrOutcomeUnknown]),
// even beside a refusal; a keys refusal wrapping no walk, which the answering
// node ran; a caller's own failure. internal/agent/builtin's failedWrite reads
// them alike, so the seat hears what its own code_open_change would tell it.
func ranNowhere(err error) (string, bool) {
	var unserved *estate.ErrUnserved
	walked := errors.As(err, &unserved)
	switch {
	case errors.Is(err, estate.ErrOutcomeUnknown) || walked && unserved.Silent():
		return "", false
	case errors.Is(err, gitlet.ErrInvalidProposal):
		return unopenedRefused(err), true
	case !walked:
		return "", false
	case errors.As(err, new(*estate.ErrUnknownKeys)):
		return unopenedUnreadable, true
	}
	return unopenedUnserved, true
}

// unopenedRefused is the line for an open refused as no proposal a committer
// can decide ([ranNowhere]), in the refusal's words past its sentinel.
// Definite: the rule holds before anything is appended, so no change is open
// and the run's commits stay on its branch. Reachable without a bug: the
// committer judges whom the turn works for against its own chart, which may
// have moved since the checkout, so the line names no act, since whether the
// seat's own open would be refused too is that chart's to say.
func unopenedRefused(err error) string {
	said, ok := builtin.RefusalSentence(err, gitlet.ErrInvalidProposal)
	if !ok {
		said = err.Error()
	}
	return "Gitlet refused it as no proposal it can decide (" + said + "), so nothing of the open ran; the " +
		"commits the run pushed are kept"
}

// unopenedUnreadable is the line for an open no node the walk asked could read,
// and [unopenedUnserved] for one no committer could take otherwise ([ranNowhere]).
// Definite, unlike an open that may have run: nothing ran, and the commits stay
// on the branch for the seat's code_open_change, which for an unreadable open a
// node of this build decides.
//
// Not retried within the collection: it holds the run's seat and box until the
// turn resumes, so retrying would hold them for the whole roll or outage (the
// sandbox's readPushes refuses an unreadable push record for the same reason).
// Resumed at once, the reviewer reads that the work was not proposed.
const unopenedUnreadable = "the node that answered runs a build that cannot read the request, so nothing " +
	"of the open ran; the commits the run pushed are kept, and the change is the seat's to open again with " +
	builtin.CodeOpenChangeTool + ", which a node of this build answers"

// unopenedUnserved is the line for an open no committer the walk asked could
// take, each out of service, unable to run it or behind: it names no build,
// since none refused it; otherwise as [unopenedUnreadable].
const unopenedUnserved = "no Gitlet node could take it, so nothing of the open ran; the commits the run " +
	"pushed are kept, and the change is the seat's to open again with " + builtin.CodeOpenChangeTool

// openCeiling is the ceiling the run's open is held to (gitlet.WriteCeiling):
// the colleague SEAT whose ask woke the launching turn
// (sandbox.PendingRun.CodeCeiling), which every push of the run is already held
// to (gitlet.Grant.Under); NOBODY for a turn a credential acting as no seat
// woke, which pushes nowhere itself, so its open is the seat's own work or
// nothing (ADR-0110); nil for a turn nobody, the seat itself or a person woke.
//
// The open is the turn's own write made late, carrying its code_open_change's
// ceiling (builtin's codeCaller), judged on the committer's chart when it
// decides (gitlet's changeRefusal), never the launch's, which an activation may
// have moved: left off, a colleague shut out since the launch would get a
// change opened that their own ask could no longer make.
func openCeiling(run sandbox.PendingRun) *gitlet.WriteCeiling {
	switch w := run.CodeWorkingFor; {
	case run.CodeCeiling != "":
		return &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingSeat, Seat: run.CodeCeiling}
	case w != nil && w.Kind == types.WorkingForCredential:
		return &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingNobody}
	}
	return nil
}

// deliveredTitle is the title of a change asked without one: the subject of
// the commit the run delivered its branch at, read by object id as the seat at
// or past the read that found the branch; failing that, the branch name, then
// the commit id, each made a title by gitlet.DeliveredTitle (the raw name may
// hold what a title may not). An ask is never left unopened for its title.
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

// openOpID is the operation id of the open run's launch asks of repoID: a step
// (statelog.StepOpID) of the launch's own operation, derived from the launch's
// id and start on the run's row, so every collection of one launch, on any node
// and after any restart, asks the same open, which the ledger answers once.
func openOpID(run sandbox.PendingRun, repoID string) string {
	launch := statelog.DeriveOpID(run.Launch.StartedAt, "", "sandbox.launch", run.LaunchID)
	return statelog.StepOpID(launch, "open-change-"+repoID)
}
