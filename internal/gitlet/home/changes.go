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

// Estate is the code log and the git reads the home's work uses; the router's
// code seam (estate.CodeSeam) satisfies it. Read answers the change and check
// work's listings; QueueDue the merge queue's, read whole or refused, since
// the queue carries every entry it reads into its next record. GitRead asks
// the repository's Gitlet nodes, its home first; Converse its committing data
// node's change writer; Checks that node's checks writer; Change and Queue its
// committer. Each write raises this node's floors past what it landed, so the
// next listing reads it wherever it is answered.
type Estate interface {
	Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error)
	QueueDue(ctx context.Context, q gitlet.QueueDueQuery) (gitlet.QueueDueAnswer, error)
	GitRead(ctx context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error)
	Converse(ctx context.Context, w gitlet.ConversationWrite) (gitlet.ConversationResult, error)
	Change(ctx context.Context, p gitlet.ChangeProposal) (gitlet.ChangeResult, error)
	Checks(ctx context.Context, r gitlet.ChecksRequest) (gitlet.ChecksResult, error)
	Queue(ctx context.Context, p gitlet.QueueProposal) (gitlet.QueueResult, error)
}

// Cache is this node's git cache (internal/gitlet/cache's *Cache): Sync pins
// a copy at a floor while a merge runs over it, NewScratch makes the scratch
// the merge writes in, and Holds (whether it holds or is filling a copy)
// serves merge previews, which only the walk's home fills a copy for.
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

// changes is the home's CHANGE WORK, the worker every loop is built with
// (package doc, "What a turn of the changes worker does"). A change's
// readiness is recorded again, merging nothing, where what it reads moved since
// its merge judged it, a later chart among them; its asks for review are
// settled with its lifecycle, live or ended ([changes.settle]).
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
	// factsRecorded is told the repository and position of each facts record
	// this worker lands. The record is on the change's subject, which the
	// loop's watch does not see, and a head's checks dispatch only once its
	// facts are recorded, so [Loop.follow] schedules the check work there
	// rather than leave it to the backstop, a minute later.
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

// Work is one repository's due changes: the first page's ask settlings
// ([changes.settle]), every page's changes in listing order ([changes.change],
// one's failure left for the next pass), then the merges its moved targets
// ask again, oldest first, within the repository's share of this pass
// ([recheckBudget]). A change this turn landed is not merged again.
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

// change is one due change: its FACTS where owed ([changes.facts]), a head
// already in its target LANDED through [changes.merge]; then its MERGE where
// owed, whatever became of the facts, since the merge reads none; else a
// REFRESH of its readiness ([changes.refresh]) where owed or where this turn
// recorded facts newer than the stored readiness. A merge records readiness
// itself, so a change merged this turn owes no refresh. It answers whether the
// change was found landed (no more work this turn, whatever the landing
// answered) and every step's failure, an undecided landing
// ([undecidedLandings]) among them: a committer that keeps refusing busy is a
// turn that failed, never one that worked.
func (c *changes) change(ctx context.Context, repoID string, due gitlet.ChangeDue, rules against,
	at statelog.Position) (bool, error) {

	var failed []error
	revised := false
	if due.FactsDue {
		in, recorded, err := c.facts(ctx, repoID, due, rules, at)
		if in {
			// The merge step lands it: naming the merging push takes a git
			// walk of the target's moves over the copy and scratch the merge
			// step holds. Its ancestry agrees with the Gitlet node's (same
			// two commits); either way this turn does no more of it.
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

// facts reads a due change's facts, asked at the listing's position so the
// answering Gitlet node holds both commits, and records them where its head is
// not in its target: whether the head is in its target (the merge step lands
// it) and whether the facts were recorded. Facts left short for time
// (gitlet.RevisionFactsAnswer.Final) are the turn's failure, the change still due.
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
		// Facts short for time are the Gitlet node's moment, not the
		// commits': recorded, they would hold for good (a revise takes a
		// revision's facts against a tip once), and a patch id omitted for
		// a slow node would carry no approval across the revision.
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

// merge is a due change's MERGE on its home, and whether it landed the change,
// over this node's copy synced and pinned at the listing's position at (so it
// holds both commits) in a scratch ([MergeScratchID]). ANCESTRY first: a head
// inside its target's tip is LANDED, not merged again (D7b; [changes.land]).
// Otherwise [mergeIn] runs in the maintenance lane within
// [MergeabilityDeadline], recorded by [changes.mergeability].
// crewlet.code.home.merge.duration times the sync, scratch, ancestry and
// merge-tree alone, observed before the landing or record is asked: those are
// other nodes' work (crewlet.code.change.proposals,
// crewlet.code.conversation.writes), so a slow writer never reads as slow
// merges here and a merge whose record failed still counts. `failed` is a
// merge that could not run.
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
	// Ancestry first: a target that came to hold the head is the change merged.
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
// due's head revision into its target's tip. A refusal that decided nothing
// ([undecidedFacts]) is this turn's failure; any other is the next pass's to read.
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
// tip the home found its head in, decided against its head and that tip,
// naming the push [changes.merger] finds over sc. A refusal that decided nothing ([undecidedLandings]) or a walk that
// could not be read is this turn's failure; any other refusal is the next
// pass's to read.
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

// The landing walk's bounds ([changes.merger]). landingWalkPage is one
// group-commit record's worth (gitlet.MaxPushesPerRecord), so the common walk
// (the merge in the tip's record or the one before) is one read.
// landingWalkPushes, the most it reads before naming nobody, is FOUR FULL
// RECORDS: the walk spans the pushes between
// the merge and the tip the pass read, which a pass follows within the loop's
// nudge (seconds), so even a busy target has moved a handful of times, against
// 256. Each push costs one `merge-base --is-ancestor`, so the bound also caps
// the walk's git at 256 reads of milliseconds over a warm copy, once a change.
// A walk cut there names nobody, which tells every author the change merged:
// one wake too many, never a wrong name.
const (
	landingWalkPage   = gitlet.MaxPushesPerRecord
	landingWalkPushes = 4 * gitlet.MaxPushesPerRecord
)

// merger is the push that MERGED due's change, its id and record position:
// the latest move of the target at or below the tip the head was found in
// whose OLD commit did not hold the head. The target's last push is not it
// whenever the target moved again before this pass, and naming that would
// misattribute the merge. So it walks back from the tip: each move's new
// commit holds the head (each older move's new is the next one's old), git is
// asked of each old commit, and the first not holding it is the merger; a
// target forced back and pushed past the head again names the move that
// stands. It names nobody where the moves do not chain (a push row past the
// horizon or purged), where every old commit holds the head, or past
// [landingWalkPushes]. The committer holds what it names to the rows
// (internal/gitlet's landingPusher).
func (c *changes) merger(ctx context.Context, repoID string, due gitlet.ChangeDue, sc *cache.Scratch,
	at statelog.Position) (string, statelog.Position, error) {

	if due.TipPosition.IsZero() {
		return "", statelog.Position{}, nil
	}
	// The descending read is strictly below its bound, and the walk starts
	// at the tip's own record.
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

// revise asks the repository's change writer to record f, due's revision's
// facts as the Gitlet node read them, its touched paths matched here against
// rules' path owners and the human paths rules' gates hold its target to,
// measured now to the microsecond, and answers whether they were recorded. A
// refusal that decided nothing ([undecidedFacts]) is this turn's failure; any
// other is the next pass's to read.
func (c *changes) revise(ctx context.Context, repoID string, due gitlet.ChangeDue, f *gitlet.RevisionFactsAnswer,
	rules against, at statelog.Position) (bool, error) {

	measured := c.now().UTC().Truncate(time.Microsecond)
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
			TouchesAll: f.TouchesAll, Classes: f.Classes, Node: c.node, MeasuredAt: measured,
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
// over the merge its row holds: a mergeability that MERGES NOTHING
// (gitlet.Mergeability.Refresh), no copy, scratch or git, the writer stamping
// the merge from the row and the readiness from its own snapshot. A refusal
// that decided nothing ([undecidedFacts]) is this turn's failure; any other
// is the next pass's to read.
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
// with its change's lifecycle (gitlet.AskSettle): asks waiting on a closed or
// merged change end, and those its end left on a change reopened since are
// asked again. A refusal that decided nothing ([undecidedFacts]) is this
// turn's failure; any other is the next pass's to read.
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

// undecidedLandings and undecidedFacts are the refusals of a landing and of a
// facts or mergeability record that decided NOTHING for the writer's or the
// log's reason (busy, the record refused or dropped everywhere): the turn
// fails and the next pass lists the change again. Every other refusal is the
// rows answering, which the next pass reads.
var (
	undecidedLandings = []gitlet.ChangeRefusal{gitlet.ChangeRefusalBusy, gitlet.ChangeRefusalNotApplied,
		gitlet.ChangeRefusalUnavailable}
	undecidedFacts = []gitlet.ConversationRefusal{gitlet.ConversationRefusalBusy, gitlet.ConversationRefusalNotApplied,
		gitlet.ConversationRefusalUnavailable}
)

// recheckBudget is each repository's share of a PASS's merges of its moved
// targets on this node: at most [MergeabilityPerPass] between two paced passes,
// renewed for every repository at once by the backstop's tick
// ([recheckBudget.newPass]), whichever turns run them. A pass, never a clock
// window: a pass's turn starts after a delay that varies, so a window could
// still be open, its share spent, at a sooner next turn. A pass a watch's gap
// asks early renews nothing, or a node whose watch keeps losing its consumer
// would merge without bound. A count, never a list: the rows say which changes
// are due, so a restarted node or moved home begins with the whole share, a
// pass's merges early at most.
type recheckBudget struct {
	mu sync.Mutex
	// perPass is [MergeabilityPerPass] (a suite sets its own, export_test.go);
	// used counts each repository's merges since the last paced pass, cleared
	// whole at the next, so it holds only this pass's repositories.
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
