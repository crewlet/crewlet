package home

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Estate is the code log and the git reads as the home's work reaches them —
// the router's code seam satisfies it (estate.CodeSeam): the due listings
// (Read), a revision's facts and a commit's checks file (GitRead, asked of the
// repository's Gitlet nodes, its home first), a record of a revision's facts, of
// a change's mergeability or of its readiness on the change (Converse, asked
// of the repository's committing data node's change writer), a landing
// (Change, asked of its committer), a record of its checks (Checks, asked of
// the same node's checks writer) and a protected target's green or broken
// record (Queue, asked of its committer). Each raises this node's floors past what
// it landed, so the next listing reads it wherever it is answered.
type Estate interface {
	Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error)
	GitRead(ctx context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error)
	Converse(ctx context.Context, w gitlet.ConversationWrite) (gitlet.ConversationResult, error)
	Change(ctx context.Context, p gitlet.ChangeProposal) (gitlet.ChangeResult, error)
	Checks(ctx context.Context, r gitlet.ChecksRequest) (gitlet.ChecksResult, error)
	Queue(ctx context.Context, p gitlet.QueueProposal) (gitlet.QueueResult, error)
}

// Cache is this node's git cache as the home's merges reach it —
// internal/gitlet/cache's *Cache satisfies it: a copy of a repository synced
// to a floor and pinned while a merge runs over it (Sync), a scratch beside
// it the merge writes in (NewScratch), and — for a merge preview, which only
// the walk's home fills a copy for — whether it holds or is filling one
// (Holds).
type Cache interface {
	Holds(repoID string) bool
	Sync(ctx context.Context, repoID string, fresh statelog.Freshness) (cache.Synced, error)
	NewScratch(ctx context.Context, s cache.Synced, id string) (*cache.Scratch, error)
}

// Pool runs git — internal/gitlet/gitproc's *Pool satisfies it.
type Pool interface {
	Run(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error)
}

// WorkerChanges is the change work's worker's name, as
// crewlet.code.home.passes counts its turns.
const WorkerChanges = "changes"

// changes is the home's CHANGE WORK, the worker every loop is built with: for
// each live change of a repository, its head revision's facts read and
// recorded on the change and its merge into its target's tip recorded beside
// them, or the change landed where its target holds its head; its readiness
// recorded again, merging nothing, where what it reads moved since its merge
// judged it — a review, an ask, an answer to findings, facts, its own row, its
// head's checks, or the chart this node holds, a later one than judged it —
// and, a moved target's changes merged again at the refresh, within a pass's
// budget; and, for a change live or ended, its asks for review settled with
// its lifecycle where they no longer match it ([changes.settle]).
type changes struct {
	estate Estate
	cache  Cache
	git    Pool
	node   string
	// chart is the activation of the chart this node holds now
	// ([Options.Chart]), which every listing names.
	chart   func() int64
	metrics *metrics.Recorder
	log     *slog.Logger
	now     func() time.Time
	// rechecks is each repository's share of a pass's merges of moved
	// targets ([MergeabilityPerPass]).
	rechecks *recheckBudget
	// factsRecorded is told each revision's facts this worker recorded: the
	// repository and the position their record landed at. The record is on
	// the CHANGE's subject, which the loop's watch does not see, and a head's
	// checks are dispatched only once its facts are recorded — so the loop
	// schedules the repository's check work there ([Loop.follow]) rather
	// than leave the dispatch to its backstop, a minute later.
	factsRecorded func(repoID string, at statelog.Position)
}

// Name is [WorkerChanges].
func (c *changes) Name() string { return WorkerChanges }

// Due is a page of the repositories holding a due change: the `changes_due`
// listing (internal/gitlet's ReadChangesDue), a merge due again once it is
// [MergeabilityRefresh] old.
func (c *changes) Due(ctx context.Context, floor statelog.Position, cursor string) ([]string, string, error) {
	a, err := c.estate.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadChangesDue, Cursor: cursor, Fresh: freshAt(floor),
		ChangesDue: &gitlet.ChangesDueQuery{RecheckBefore: c.now().Add(-MergeabilityRefresh), ChartEpoch: c.chart()}})
	if err != nil {
		return nil, "", err
	}
	return a.DueRepos, a.Next, nil
}

// Work is one repository's due changes: first the asks its first page names
// due a settling ([changes.settle]), then every page's changes, each worked in
// the order the listing names them ([changes.change]) — a failure of one left
// for the next pass while the others are worked — and then the merges its moved
// targets ask again, the oldest first and no more than what is left of the
// repository's share of this pass ([recheckBudget]), until they are done or
// the turn's context ends. A change a step of this turn landed is merged
// again by none.
func (c *changes) Work(ctx context.Context, repoID string, floor statelog.Position) error {
	q := gitlet.ReadQuery{Kind: gitlet.ReadChangesDue, Fresh: freshAt(floor),
		ChangesDue: &gitlet.ChangesDueQuery{RepoID: repoID, RecheckBefore: c.now().Add(-MergeabilityRefresh),
			Rechecks: c.rechecks.left(repoID), ChartEpoch: c.chart()}}
	var (
		failed  []error
		again   []gitlet.ChangeDue
		againAt statelog.Position
	)
	landed := map[string]bool{}
	for {
		a, err := c.estate.Read(ctx, q)
		if err != nil {
			return errors.Join(append(failed, fmt.Errorf("home: read repository %s's due changes: %w", repoID,
				err))...)
		}
		if q.Cursor == "" {
			again, againAt = a.Rechecks, a.Position
			for _, due := range a.AskSettles {
				if err := ctx.Err(); err != nil {
					return errors.Join(append(failed, err)...)
				}
				if err := c.settle(ctx, repoID, due, a.Position); err != nil {
					failed = append(failed, err)
				}
			}
		}
		for _, due := range a.DueChanges {
			if err := ctx.Err(); err != nil {
				return errors.Join(append(failed, err)...)
			}
			done, err := c.change(ctx, repoID, due, against{owners: a.PathOwners, gates: a.Gates}, a.Position)
			if done {
				landed[due.ID] = true
			}
			if err != nil {
				failed = append(failed, err)
			}
		}
		if a.Next == "" {
			break
		}
		q.Cursor = a.Next
	}
	for _, due := range again {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failed, err)...)
		}
		if landed[due.ID] {
			continue
		}
		c.rechecks.spend(repoID)
		if _, err := c.merge(ctx, repoID, due, againAt); err != nil {
			failed = append(failed, err)
		}
	}
	return errors.Join(failed...)
}

// change is one due change: its head revision's FACTS where it owes them —
// read against its target's tip as the listing read it, asked at the
// listing's position, so the Gitlet node that answers holds both commits — the
// change LANDED where its target holds its head, through its merge step
// ([changes.merge]), whose ancestry over this node's own copy finds it so and
// whose walk of the target's moves names the push that merged it; and the
// facts RECORDED otherwise, the path owners and the human paths its target's gate names
// matched against the touched paths here, on the home (gitlet.MatchOwners,
// gitlet.MatchTargetHumanPaths), beside the digests of the owners and the rules
// the listing read;
// then its MERGE where it owes one ([changes.merge]), whatever became of its
// facts, since the merge reads none of them; and where it owes no merge, a
// REFRESH of its readiness ([changes.refresh]) where it owes one, or where
// this turn recorded facts its stored readiness was judged before. A merge
// records the readiness itself, judged with its own snapshot, so a change
// merged this turn owes no refresh. It answers whether the change was found
// landed — its head inside its target's tip, which this turn does no more
// work on whatever its landing answered — and every step's failure, A
// LANDING THAT DECIDED NOTHING among them ([undecidedLandings]): a committer
// that keeps refusing a landing busy, or a Change call that keeps failing,
// is a turn that failed, never one that worked.
func (c *changes) change(ctx context.Context, repoID string, due gitlet.ChangeDue, rules against,
	at statelog.Position) (bool, error) {

	var failed []error
	revised := false
	if due.FactsDue {
		in, recorded, err := c.facts(ctx, repoID, due, rules, at)
		if in {
			// THE MERGE STEP LANDS IT, its ancestry asked again over this
			// node's own copy: the landing names the push that merged the
			// change, which only a walk of the target's moves through git can
			// tell, and the merge step holds the copy and the scratch that
			// walk runs in. Its ancestry agrees with the Gitlet node's, which
			// read the same two commits; where it did not land the change it
			// recorded its merge, and this turn does no more of it.
			return c.merge(ctx, repoID, due, at)
		}
		revised = recorded
		if err != nil {
			failed = append(failed, err)
		}
	}
	switch {
	case due.MergeDue:
		landed, err := c.merge(ctx, repoID, due, at)
		if err != nil {
			failed = append(failed, err)
		}
		if landed {
			return true, errors.Join(failed...)
		}
	case due.RefreshDue || revised:
		if err := c.refresh(ctx, repoID, due, at); err != nil {
			failed = append(failed, err)
		}
	}
	return false, errors.Join(failed...)
}

// facts is a due change's facts read, then its facts recorded where its head
// is not in its target ([changes.change]): whether its head is in its target,
// which its merge step lands, and whether its facts were recorded. Facts the
// Gitlet node left short for time are no facts to keep
// (gitlet.RevisionFactsAnswer.Final): the turn's failure, the change still due.
func (c *changes) facts(ctx context.Context, repoID string, due gitlet.ChangeDue, rules against,
	at statelog.Position) (bool, bool, error) {

	began := c.now()
	read, err := c.estate.GitRead(ctx, gitlet.GitQuery{Kind: gitlet.GitRevisionFacts, RepoID: repoID,
		Fresh: freshAt(at), RevisionFacts: &gitlet.RevisionFactsQuery{Head: due.Head, Target: due.Tip}})
	if c.metrics != nil {
		outcome := "answered"
		if err != nil {
			outcome = "failed"
		}
		c.metrics.Observe(metrics.CodeHomeFactsDuration, c.now().Sub(began), metrics.Attrs{"outcome": outcome})
	}
	switch {
	case err != nil:
		return false, false, fmt.Errorf("home: read the facts of revision %d of change %d of repository %s: %w",
			due.Revision, due.Number, repoID, err)
	case read.RevisionFacts == nil:
		return false, false, fmt.Errorf("home: the facts of revision %d of change %d of repository %s were "+
			"answered with none", due.Revision, due.Number, repoID)
	}
	f := read.RevisionFacts
	if f.HeadInTarget {
		return true, false, nil
	}
	if !f.Final() {
		// FACTS LEFT SHORT FOR TIME are the Gitlet node's moment, never the
		// commits': recorded, they would hold for good — a revise takes
		// facts of a revision and a tip once — and an id omitted for a slow
		// node would carry no approval across the revision. The change stays
		// due its facts, so the next pass asks again, of whichever Gitlet node
		// the walk reaches then.
		return false, false, fmt.Errorf("home: the facts of revision %d of change %d of repository %s ran out "+
			"the Gitlet node's budget before their patch id (%q) or their classes (%q) were whole; the next pass "+
			"asks again", due.Revision, due.Number, repoID, f.PatchIDOmission, f.ClassesOmission)
	}
	recorded, err := c.revise(ctx, repoID, due, f, rules, at)
	return false, recorded, err
}

// The outcomes of crewlet.code.home.merge.duration: a merge's state, a
// landing the ancestry found with no merge run, and a merge that could not
// run.
const (
	mergeLanded = "landed"
	mergeFailed = "failed"
)

// merge is a due change's MERGE on this node — the repository's home — and
// whether it landed the change: this node's copy synced to the listing's
// position at, so it holds both commits, and pinned; a scratch beside it
// ([MergeScratchID]), removed after; the change's ANCESTRY first — its head
// inside its target's tip is a change merged, whatever push moved the target
// there, which is LANDED rather than merged again (D7b), naming the push the
// walk of the target's moves over the same scratch finds merged it
// ([changes.land]) — and otherwise the
// merge ([mergeIn]), in the maintenance lane within [MergeabilityDeadline],
// recorded on the change as its MERGEABILITY. A refusal that the rows moved
// since is the next pass's to read; one that decided nothing for a reason of
// the log's is this turn's failure.
//
// crewlet.code.home.merge.duration TIMES THE MERGE ALONE — the sync, the
// scratch, the ancestry and the merge-tree — observed the moment the ancestry
// or the merge answers, BEFORE the landing or the record is asked: those are
// a committer's proposal and a change writer's record through the router,
// another node's work counted by its own instruments
// (crewlet.code.change.proposals, crewlet.code.conversation.writes), so a
// slow change writer never reads as slow merges on the home, and a merge
// whose record failed still counts as the merge it was.
// `failed` is a merge that could not run: a copy that would not sync, a
// scratch that could not be made, git's ancestry or merge failing.
func (c *changes) merge(ctx context.Context, repoID string, due gitlet.ChangeDue, at statelog.Position) (bool,
	error) {

	began := c.now()
	observe := func(outcome string) {
		if c.metrics != nil {
			c.metrics.Observe(metrics.CodeHomeMergeDuration, c.now().Sub(began),
				metrics.Attrs{"work": workMergeability, "outcome": outcome})
		}
	}
	synced, err := c.cache.Sync(ctx, repoID, freshAt(at))
	if err != nil {
		observe(mergeFailed)
		return false, fmt.Errorf("home: sync repository %s to merge change %d: %w", repoID, due.Number, err)
	}
	defer synced.Release()
	sc, err := c.cache.NewScratch(ctx, synced, MergeScratchID(due.ID, due.Revision, due.TipPosition))
	if err != nil {
		observe(mergeFailed)
		return false, fmt.Errorf("home: a scratch to merge change %d of repository %s in: %w", due.Number, repoID,
			err)
	}
	defer sc.Remove()
	// ANCESTRY FIRST, as a read of the scratch: the one question a moved
	// target asks before its merge — a target that came to hold the head is
	// the change merged.
	res, err := c.git.Run(ctx, gitproc.Cmd{Dir: sc.Dir, Alternate: sc.Alternate,
		Args:  []string{"merge-base", "--is-ancestor", due.Head, due.Tip},
		Class: gitproc.ClassMaintenance, Deadline: gitproc.ReadDeadline, Answers: []int{1}})
	if err != nil {
		observe(mergeFailed)
		return false, fmt.Errorf("home: ask whether change %d of repository %s is in its target: %w", due.Number,
			repoID, err)
	}
	if res.Exit == 0 {
		observe(mergeLanded)
		return true, c.land(ctx, repoID, due, sc, at)
	}
	m, err := mergeIn(ctx, c.git, sc, due.Tip, due.Head, gitproc.ClassMaintenance, MergeabilityDeadline)
	if err != nil {
		observe(mergeFailed)
		return false, fmt.Errorf("home: merge change %d of repository %s: %w", due.Number, repoID, err)
	}
	observe(string(m.state))
	return false, c.mergeability(ctx, repoID, due, m, at)
}

// workMergeability is the `work` crewlet.code.home.merge.duration counts the
// home's own merges under, and workPreview a merge preview's.
const (
	workMergeability = "mergeability"
	workPreview      = "preview"
)

// mergeability asks the repository's change writer to record m, the merge of
// due's head revision into its target's tip: an answer that the rows moved
// since — superseded, the change closed or merged — is the next pass's to
// read; one that decided nothing for a reason of the log's is this turn's
// failure.
func (c *changes) mergeability(ctx context.Context, repoID string, due gitlet.ChangeDue, m merged,
	at statelog.Position) error {

	res, err := c.estate.Converse(ctx, gitlet.ConversationWrite{OpID: statelog.NewOpID(c.now(), ""), Seen: at,
		Mergeability: &gitlet.Mergeability{RepoID: repoID, ChangeID: due.ID, Revision: due.Revision,
			HeadOID: due.Head, TargetRef: due.TargetRef, TargetOID: due.Tip, TargetPosition: due.TipPosition,
			State: m.state, Conflicts: m.conflicts.Paths, ConflictsOmitted: m.conflicts.Omitted, Node: c.node}})
	switch {
	case err != nil:
		return fmt.Errorf("home: record whether change %d of repository %s merges: %w", due.Number, repoID, err)
	case slices.Contains(undecidedFacts, res.Refusal):
		return fmt.Errorf("home: record whether change %d of repository %s merges: %s: %s", due.Number, repoID,
			res.Refusal, res.Detail)
	case res.Refusal != "":
		c.log.Debug("code_change_mergeability_refused", "repo", repoID, "change", due.Number,
			"revision", due.Revision, "refusal", res.Refusal, "detail", res.Detail)
		return nil
	}
	c.log.Debug("code_change_mergeability_recorded", "repo", repoID, "change", due.Number,
		"revision", due.Revision, "target", due.TargetRef, "tip", due.Tip, "state", m.state,
		"conflicts", len(m.conflicts.Paths)+int(m.conflicts.Omitted), "position", res.Position.String())
	return nil
}

// land asks the repository's committer to record due's change merged at the
// tip the home found its head in, decided against the change's head and that
// tip, naming the push that merged it as the walk of the target's moves over
// sc finds it ([changes.merger]): an answer that the rows moved since — stale,
// the change closed — or that it is merged already is the next pass's to
// read; one that decided nothing for a reason of the log's is this turn's
// failure, and so is a walk that could not be read.
func (c *changes) land(ctx context.Context, repoID string, due gitlet.ChangeDue, sc *cache.Scratch,
	at statelog.Position) error {

	landed := &gitlet.ChangeLanded{RepoID: repoID, ChangeID: due.ID, Revision: due.Revision, HeadOID: due.Head,
		TargetRef: due.TargetRef, TargetOID: due.Tip, Node: c.node}
	var err error
	if landed.PushID, landed.PushPosition, err = c.merger(ctx, repoID, due, sc, at); err != nil {
		return fmt.Errorf("home: find the push that merged change %d of repository %s: %w", due.Number, repoID,
			err)
	}
	res, err := c.estate.Change(ctx, gitlet.ChangeProposal{OpID: statelog.NewOpID(c.now(), ""), Seen: at,
		Landed: landed})
	switch {
	case err != nil:
		return fmt.Errorf("home: land change %d of repository %s: %w", due.Number, repoID, err)
	case slices.Contains(undecidedLandings, res.Refusal):
		return fmt.Errorf("home: land change %d of repository %s: %s: %s", due.Number, repoID, res.Refusal,
			res.Detail)
	case res.Refusal != "":
		c.log.Debug("code_change_landing_refused", "repo", repoID, "change", due.Number, "refusal", res.Refusal,
			"detail", res.Detail)
		return nil
	}
	c.log.Info("code_change_landed", "repo", repoID, "change", due.Number, "revision", due.Revision,
		"target", due.TargetRef, "tip", due.Tip, "push", landed.PushID, "unchanged", res.Unchanged,
		"position", res.Position.String())
	return nil
}

// The landing walk's bounds ([changes.merger]).
//
// landingWalkPage is the most pushes one read of the walk asks for: one
// group-commit record's worth (gitlet.MaxPushesPerRecord), so the common walk —
// the merge in the tip's own record or the one before it — is one read.
//
// landingWalkPushes is the most pushes the walk reads, newest first from the
// tip, before it names nobody: FOUR FULL RECORDS. The walk spans the pushes
// landed between the merge and the tip the home's pass read, and a pass
// follows a push on the repository's subject within the loop's nudge — a
// sync, a listing and a git process, seconds — so even a target every seat
// pushes to has moved a handful of times since the merge, against the 256
// pushes of four records written back to back at group commit's widest. Each
// push on the target costs one `merge-base --is-ancestor` in the maintenance
// lane, so the bound is also the walk's git: at most 256 reads of a few
// milliseconds over a warm copy, once a change. A walk cut there names nobody,
// which tells every author the change merged, its merger among them: one
// wake too many, never a wrong name.
const (
	landingWalkPage   = gitlet.MaxPushesPerRecord
	landingWalkPushes = 4 * gitlet.MaxPushesPerRecord
)

// merger is the push that MERGED due's change — brought its head into its
// target: the latest move of the target, at or below the tip the home found
// the head in, whose OLD commit did not hold the head, so the target held it
// at every move from that one through the tip — its id and the position of
// its record; none where no move the walk read is that one.
//
// WHY A WALK, AND WHY BACKWARDS: the push that moved the target LAST is not
// the merger whenever the target moved again before this pass read it —
// somebody's push on top, in the merge's own group-commit record or a later
// one — and naming it would tell the merger `merged` of their own merge and
// tell every other author that somebody else merged it. Whether a commit
// holds another is git's to say, so the home asks: from the tip down, each
// move's new commit is known to hold the head (the tip does; each older
// move's new commit is the old one of the move after it), and the first move
// whose old commit does not hold it is the merger. A target forced back
// behind the head and pushed past it again names the move that brought it
// back, which is the merge that stands. The walk costs the moves since the
// merge, never the target's history, and stops at [landingWalkPushes].
//
// It names nobody where the moves do not chain (a push row between them gone
// past the history's horizon or purged), where the rows run out with every
// old commit holding the head (the head was in the target before every move
// they keep), and past the bound. The committer holds what it names to the
// rows (internal/gitlet's landingPusher).
func (c *changes) merger(ctx context.Context, repoID string, due gitlet.ChangeDue, sc *cache.Scratch,
	at statelog.Position) (string, statelog.Position, error) {

	if due.TipPosition.IsZero() {
		return "", statelog.Position{}, nil
	}
	// BELOW THE NEXT SEQUENCE: the introduced read's descending page is the
	// pushes strictly below its bound, and the tip's own record is the first
	// the walk needs.
	before := due.TipPosition
	before.Seq++
	q := gitlet.ReadQuery{Kind: gitlet.ReadIntroduced, Fresh: freshAt(at),
		Introduced: &gitlet.IntroducedQuery{RepoID: repoID, Before: before, Descending: true}}
	holds := due.Tip
	for read := 0; read < landingWalkPushes; {
		q.Limit = min(landingWalkPage, landingWalkPushes-read)
		a, err := c.estate.Read(ctx, q)
		if err != nil {
			return "", statelog.Position{}, fmt.Errorf("read the pushes of repository %s below %s: %w", repoID,
				before, err)
		}
		for _, row := range a.Introduced {
			read++
			i := slices.IndexFunc(row.Moves, func(m gitlet.RefMove) bool { return m.Ref == due.TargetRef })
			if i < 0 {
				continue
			}
			move := row.Moves[i]
			if move.New != holds {
				c.log.Debug("code_change_merger_unchained", "repo", repoID, "change", due.Number,
					"push", row.PushID, "new", move.New, "want", holds)
				return "", statelog.Position{}, nil
			}
			in, err := c.holdsHead(ctx, sc, move.Old, due.Head)
			if err != nil {
				return "", statelog.Position{}, err
			}
			if !in {
				return row.PushID, row.Position, nil
			}
			holds = move.Old
		}
		if a.Next == "" || len(a.Introduced) == 0 {
			return "", statelog.Position{}, nil
		}
		q.Cursor = a.Next
	}
	c.log.Debug("code_change_merger_past_bound", "repo", repoID, "change", due.Number,
		"pushes", landingWalkPushes)
	return "", statelog.Position{}, nil
}

// holdsHead reports whether commit holds head, read over sc: a ref's creation
// (the zero id, or none) holds nothing.
func (c *changes) holdsHead(ctx context.Context, sc *cache.Scratch, commit, head string) (bool, error) {
	if commit == "" || commit == gitlet.ZeroOID {
		return false, nil
	}
	res, err := c.git.Run(ctx, gitproc.Cmd{Dir: sc.Dir, Alternate: sc.Alternate,
		Args:  []string{"merge-base", "--is-ancestor", head, commit},
		Class: gitproc.ClassMaintenance, Deadline: gitproc.ReadDeadline, Answers: []int{1}})
	if err != nil {
		return false, fmt.Errorf("ask whether %s holds %s: %w", commit, head, err)
	}
	return res.Exit == 0, nil
}

// against is what a due page says a revision's touched paths are matched
// against: the repository's path owners and its rules' merge gates, read in
// the page's snapshot beside the changes they are matched for.
type against struct {
	owners []gitlet.PathOwner
	gates  []gitlet.RuleGate
}

// revise asks the repository's change writer to record due's revision's facts
// f — its trees and its classes with the rest, as the Gitlet node read them —
// its touched paths matched against rules' path owners and against the human
// paths rules' gates hold its target to, and answers whether they were
// recorded: an answer that the rows moved since — superseded, the change
// closed or merged — is the next pass's to read; one that decided nothing for
// a reason of the log's is this turn's failure.
func (c *changes) revise(ctx context.Context, repoID string, due gitlet.ChangeDue, f *gitlet.RevisionFactsAnswer,
	rules against, at statelog.Position) (bool, error) {

	matched, digest := gitlet.MatchOwners(rules.owners, f.Touched, f.TouchesAll)
	held, humanDigest, err := gitlet.MatchTargetHumanPaths(rules.gates, due.TargetRef, f.Touched, f.TouchesAll,
		f.Classes)
	if err != nil {
		return false, fmt.Errorf("home: match the human paths of revision %d of change %d of repository %s: %w",
			due.Revision, due.Number, repoID, err)
	}
	res, err := c.estate.Converse(ctx, gitlet.ConversationWrite{OpID: statelog.NewOpID(c.now(), ""), Seen: at,
		Revise: &gitlet.Revise{RepoID: repoID, ChangeID: due.ID, N: due.Revision, HeadOID: due.Head,
			TargetRef: due.TargetRef, TargetOID: due.Tip, TargetPosition: due.TipPosition, BaseOID: f.MergeBase,
			BaseTree: f.BaseTree, HeadTree: f.HeadTree, PatchID: f.PatchID,
			PatchIDOmitted: f.PatchIDOmission == gitlet.OmittedPastBound, Touched: f.Touched,
			TouchesAll: f.TouchesAll, Classes: f.Classes, Node: c.node,
			RequestedOwners: matched, OwnersDigest: digest, HumanPaths: held, HumanPathsDigest: humanDigest}})
	switch {
	case err != nil:
		return false, fmt.Errorf("home: record the facts of revision %d of change %d of repository %s: %w",
			due.Revision, due.Number, repoID, err)
	case slices.Contains(undecidedFacts, res.Refusal):
		return false, fmt.Errorf("home: record the facts of revision %d of change %d of repository %s: %s: %s",
			due.Revision, due.Number, repoID, res.Refusal, res.Detail)
	case res.Refusal != "":
		c.log.Debug("code_change_facts_refused", "repo", repoID, "change", due.Number, "revision", due.Revision,
			"refusal", res.Refusal, "detail", res.Detail)
		return false, nil
	}
	c.log.Debug("code_change_facts_recorded", "repo", repoID, "change", due.Number, "revision", due.Revision,
		"target", due.TargetRef, "tip", due.Tip, "touched", len(f.Touched), "touches_all", f.TouchesAll,
		"classed", len(f.Classes.WhitespaceOnly)+len(f.Classes.CommentOnly), "classes_complete", f.Classes.Complete,
		"owners", len(matched), "human_paths_held", len(held.Held), "position", res.Position.String())
	if c.factsRecorded != nil {
		c.factsRecorded(repoID, res.Position)
	}
	return true, nil
}

// refresh asks the repository's change writer to record due's readiness again
// over the merge its row holds — a mergeability that MERGES NOTHING
// (gitlet.Mergeability.Refresh): no copy synced, no scratch, no git — naming the
// head revision, its head and its target, the writer stamping the merge from
// the row and the readiness from its own snapshot. An answer that the rows
// moved since — superseded: the row's answer judged at the inputs already, a
// merge owed first, the change closed or merged — is the next pass's to read;
// one that decided nothing for a reason of the log's is this turn's failure.
func (c *changes) refresh(ctx context.Context, repoID string, due gitlet.ChangeDue, at statelog.Position) error {
	res, err := c.estate.Converse(ctx, gitlet.ConversationWrite{OpID: statelog.NewOpID(c.now(), ""), Seen: at,
		Mergeability: &gitlet.Mergeability{RepoID: repoID, ChangeID: due.ID, Revision: due.Revision,
			HeadOID: due.Head, TargetRef: due.TargetRef, Refresh: true, Node: c.node}})
	switch {
	case err != nil:
		return fmt.Errorf("home: refresh the readiness of change %d of repository %s: %w", due.Number, repoID, err)
	case slices.Contains(undecidedFacts, res.Refusal):
		return fmt.Errorf("home: refresh the readiness of change %d of repository %s: %s: %s", due.Number, repoID,
			res.Refusal, res.Detail)
	case res.Refusal != "":
		c.log.Debug("code_change_readiness_refused", "repo", repoID, "change", due.Number,
			"revision", due.Revision, "refusal", res.Refusal, "detail", res.Detail)
		return nil
	}
	c.log.Debug("code_change_readiness_refreshed", "repo", repoID, "change", due.Number,
		"revision", due.Revision, "position", res.Position.String())
	return nil
}

// settle asks the repository's change writer to settle due's asks for review
// with its change's lifecycle (gitlet.AskSettle): those still waiting on a change
// that closed or merged ended, those its end left on a change reopened since
// asked again — the writer stamping the asks it moves from its own snapshot.
// An answer that the rows moved since — superseded: the change reopened or
// ended again, or holding no such ask — is the next pass's to read; one that
// decided nothing for a reason of the log's is this turn's failure.
func (c *changes) settle(ctx context.Context, repoID string, due gitlet.AskDue, at statelog.Position) error {
	res, err := c.estate.Converse(ctx, gitlet.ConversationWrite{OpID: statelog.NewOpID(c.now(), ""), Seen: at,
		Asks: &gitlet.AskSettle{RepoID: repoID, ChangeID: due.ChangeID, State: due.State}})
	switch {
	case err != nil:
		return fmt.Errorf("home: settle the asks of change %s of repository %s: %w", due.ChangeID, repoID, err)
	case slices.Contains(undecidedFacts, res.Refusal):
		return fmt.Errorf("home: settle the asks of change %s of repository %s: %s: %s", due.ChangeID, repoID,
			res.Refusal, res.Detail)
	case res.Refusal != "":
		c.log.Debug("code_change_asks_refused", "repo", repoID, "change", due.ChangeID, "state", due.State,
			"refusal", res.Refusal, "detail", res.Detail)
		return nil
	}
	c.log.Debug("code_change_asks_settled", "repo", repoID, "change", due.ChangeID, "state", due.State,
		"position", res.Position.String())
	return nil
}

// undecidedLandings and undecidedFacts are every refusal of a landing and of a
// record of facts or a mergeability that decided NOTHING for a reason of the
// writer's or of the log's — busy, the log refusing the record or dropping it
// everywhere — which a turn counts as failed, its change listed again by the
// next pass; every other refusal is the rows answering, which the next pass
// reads again.
var (
	undecidedLandings = []gitlet.ChangeRefusal{gitlet.ChangeRefusalBusy, gitlet.ChangeRefusalNotApplied,
		gitlet.ChangeRefusalUnavailable}
	undecidedFacts = []gitlet.ConversationRefusal{gitlet.ConversationRefusalBusy, gitlet.ConversationRefusalNotApplied,
		gitlet.ConversationRefusalUnavailable}
)

// recheckBudget is each repository's share of a PASS's merges of its moved
// targets on this node: at most [MergeabilityPerPass] between two of the
// loop's paced passes — the backstop's ticks, which renew every repository's
// share at once ([recheckBudget.newPass]) — whichever turns, the pass's own
// or a nudge's, run them.
//
// A PASS, NEVER A CLOCK WINDOW: a window opened at a repository's first
// recheck and ended one backstop later is ended by a moment no pass keeps,
// since a pass's turn starts after its listing, its placement read and a
// free turn — a delay that varies pass to pass — so a pass whose turn came
// sooner than the last one's found the window still open and its share
// spent, and a quiet target's backlog drained at half the share. Renewed by
// the ticker that paces the passes, every pass's turns find the whole share.
// A pass a watch's gap asks early renews nothing: a gap is no tick, and a
// node whose watch keeps losing its consumer would otherwise merge without
// bound.
//
// A count and never a list: the rows say which changes are due, and the share
// only how many of them a pass may merge, so a node that restarts or a home
// that moves begins with the whole share, a pass's merges early at the most.
type recheckBudget struct {
	mu sync.Mutex
	// perPass is the merges one repository's share holds,
	// [MergeabilityPerPass] — a suite's own (export_test.go) — and used each
	// repository's merges since the last paced pass, forgotten whole at the
	// next, so a node home to many repositories holds those that rechecked
	// this pass alone.
	perPass int
	used    map[string]int
}

// newRecheckBudget is a budget of MergeabilityPerPass merges a repository a
// pass.
func newRecheckBudget() *recheckBudget {
	return &recheckBudget{perPass: MergeabilityPerPass, used: map[string]int{}}
}

// newPass renews every repository's share: a paced pass begins.
func (b *recheckBudget) newPass() {
	b.mu.Lock()
	defer b.mu.Unlock()
	clear(b.used)
}

// left is how many merges repoID may still run this pass.
func (b *recheckBudget) left(repoID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return max(0, b.perPass-b.used[repoID])
}

// spend counts one merge of repoID's this pass.
func (b *recheckBudget) spend(repoID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used[repoID]++
}

// freshAt is the level a read at floor is asked at: `session` beside it, so
// the rows read hold the record it names, and stale where it is zero.
func freshAt(floor statelog.Position) statelog.Freshness {
	if floor.IsZero() {
		return statelog.Freshness{Level: statelog.ReadStale}
	}
	return statelog.FloorAt(statelog.ReadSession, floor)
}
