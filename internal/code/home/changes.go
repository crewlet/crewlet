package home

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Estate is the code log and the git reads as the home's work reaches them —
// the router's code seam satisfies it (estate.CodeSeam): the due listings
// (Read), a revision's facts and a commit's checks file (GitRead, asked of the
// repository's git nodes, its home first), a record of a revision's facts, of
// a change's mergeability or of its readiness on the change (Converse, asked
// of the repository's committing data node's change writer), a landing
// (Change, asked of its committer), a record of its checks (Checks, asked of
// the same node's checks writer) and a protected target's green or broken
// record (Queue, asked of its committer). Each raises this node's floors past what
// it landed, so the next listing reads it wherever it is answered.
type Estate interface {
	Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error)
	GitRead(ctx context.Context, q code.GitQuery) (code.GitAnswer, error)
	Converse(ctx context.Context, w code.ConversationWrite) (code.ConversationResult, error)
	Change(ctx context.Context, p code.ChangeProposal) (code.ChangeResult, error)
	Checks(ctx context.Context, r code.ChecksRequest) (code.ChecksResult, error)
	Queue(ctx context.Context, p code.QueueProposal) (code.QueueResult, error)
}

// Cache is this node's git cache as the home's merges reach it —
// internal/code/cache's *Cache satisfies it: a copy of a repository synced
// to a floor and pinned while a merge runs over it (Sync), a scratch beside
// it the merge writes in (NewScratch), and — for a merge preview, which only
// the walk's home fills a copy for — whether it holds or is filling one
// (Holds).
type Cache interface {
	Holds(repoID string) bool
	Sync(ctx context.Context, repoID string, fresh statelog.Freshness) (cache.Synced, error)
	NewScratch(ctx context.Context, s cache.Synced, id string) (*cache.Scratch, error)
}

// Pool runs git — internal/code/gitproc's *Pool satisfies it.
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
// judged it — a review, an ask, an answer to findings, facts, its own row —
// and, a moved target's changes merged again at the refresh, within a pass's
// budget; and, for a change live or ended, its asks for review settled with
// its lifecycle where they no longer match it ([changes.settle]).
type changes struct {
	estate  Estate
	cache   Cache
	git     Pool
	node    string
	metrics *metrics.Recorder
	log     *slog.Logger
	now     func() time.Time
	// rechecks is each repository's share of a pass's merges of moved
	// targets ([MergeabilityPerPass]).
	rechecks *recheckBudget
}

// Name is [WorkerChanges].
func (c *changes) Name() string { return WorkerChanges }

// Due is a page of the repositories holding a due change: the `changes_due`
// listing (internal/code's ReadChangesDue), a merge due again once it is
// [MergeabilityRefresh] old.
func (c *changes) Due(ctx context.Context, floor statelog.Position, cursor string) ([]string, string, error) {
	a, err := c.estate.Read(ctx, code.ReadQuery{Kind: code.ReadChangesDue, Cursor: cursor, Fresh: freshAt(floor),
		ChangesDue: &code.ChangesDueQuery{RecheckBefore: c.now().Add(-MergeabilityRefresh)}})
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
	now := c.now()
	q := code.ReadQuery{Kind: code.ReadChangesDue, Fresh: freshAt(floor),
		ChangesDue: &code.ChangesDueQuery{RepoID: repoID, RecheckBefore: now.Add(-MergeabilityRefresh),
			Rechecks: c.rechecks.left(repoID, now)}}
	var (
		failed  []error
		again   []code.ChangeDue
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
			done, err := c.change(ctx, repoID, due, a.PathOwners, a.Position)
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
		c.rechecks.spend(repoID, now)
		if _, err := c.merge(ctx, repoID, due, againAt); err != nil {
			failed = append(failed, err)
		}
	}
	return errors.Join(failed...)
}

// change is one due change: its head revision's FACTS where it owes them —
// read against its target's tip as the listing read it, asked at the
// listing's position, so the git node that answers holds both commits — the
// change LANDED where its target holds its head, and the facts RECORDED
// otherwise, the path owners matched against the touched paths here, on the
// home (code.MatchOwners), beside the digest of the owners the listing read;
// then its MERGE where it owes one ([changes.merge]), whatever became of its
// facts, since the merge reads none of them; and where it owes no merge, a
// REFRESH of its readiness ([changes.refresh]) where it owes one, or where
// this turn recorded facts its stored readiness was judged before. A merge
// records the readiness itself, judged with its own snapshot, so a change
// merged this turn owes no refresh. It answers whether the change was landed.
func (c *changes) change(ctx context.Context, repoID string, due code.ChangeDue, owners []code.PathOwner,
	at statelog.Position) (bool, error) {

	var failed []error
	revised := false
	if due.FactsDue {
		landed, recorded, err := c.facts(ctx, repoID, due, owners, at)
		if landed {
			return true, nil
		}
		revised = recorded
		if err != nil {
			failed = append(failed, err)
		}
	}
	switch {
	case due.MergeDue:
		landed, err := c.merge(ctx, repoID, due, at)
		if landed {
			return true, errors.Join(failed...)
		}
		if err != nil {
			failed = append(failed, err)
		}
	case due.RefreshDue || revised:
		if err := c.refresh(ctx, repoID, due, at); err != nil {
			failed = append(failed, err)
		}
	}
	return false, errors.Join(failed...)
}

// facts is a due change's facts read, then the change landed or its facts
// recorded ([changes.change]): whether it was landed, and whether its facts
// were recorded. Facts the git node left short for time are no facts to keep
// (code.RevisionFactsAnswer.Final): the turn's failure, the change still due.
func (c *changes) facts(ctx context.Context, repoID string, due code.ChangeDue, owners []code.PathOwner,
	at statelog.Position) (bool, bool, error) {

	began := c.now()
	read, err := c.estate.GitRead(ctx, code.GitQuery{Kind: code.GitRevisionFacts, RepoID: repoID,
		Fresh: freshAt(at), RevisionFacts: &code.RevisionFactsQuery{Head: due.Head, Target: due.Tip}})
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
		return true, false, c.land(ctx, repoID, due, at)
	}
	if !f.Final() {
		// FACTS LEFT SHORT FOR TIME are the git node's moment, never the
		// commits': recorded, they would hold for good — a revise takes
		// facts of a revision and a tip once — and an id omitted for a slow
		// node would carry no approval across the revision. The change stays
		// due its facts, so the next pass asks again, of whichever git node
		// the walk reaches then.
		return false, false, fmt.Errorf("home: the facts of revision %d of change %d of repository %s ran out "+
			"the git node's budget before their patch id (%q) or their classes (%q) were whole; the next pass "+
			"asks again", due.Revision, due.Number, repoID, f.PatchIDOmission, f.ClassesOmission)
	}
	recorded, err := c.revise(ctx, repoID, due, f, owners, at)
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
// there, which is LANDED rather than merged again (D7b) — and otherwise the
// merge ([mergeIn]), in the maintenance lane within [MergeabilityDeadline],
// recorded on the change as its MERGEABILITY. A refusal that the rows moved
// since is the next pass's to read; one that decided nothing for a reason of
// the log's is this turn's failure.
func (c *changes) merge(ctx context.Context, repoID string, due code.ChangeDue, at statelog.Position) (bool,
	error) {

	began := c.now()
	outcome := mergeFailed
	defer func() {
		if c.metrics != nil {
			c.metrics.Observe(metrics.CodeHomeMergeDuration, c.now().Sub(began),
				metrics.Attrs{"work": workMergeability, "outcome": outcome})
		}
	}()
	synced, err := c.cache.Sync(ctx, repoID, freshAt(at))
	if err != nil {
		return false, fmt.Errorf("home: sync repository %s to merge change %d: %w", repoID, due.Number, err)
	}
	defer synced.Release()
	sc, err := c.cache.NewScratch(ctx, synced, MergeScratchID(due.ID, due.Revision, due.TipPosition))
	if err != nil {
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
		return false, fmt.Errorf("home: ask whether change %d of repository %s is in its target: %w", due.Number,
			repoID, err)
	}
	if res.Exit == 0 {
		outcome = mergeLanded
		return true, c.land(ctx, repoID, due, at)
	}
	m, err := mergeIn(ctx, c.git, sc, due.Tip, due.Head, gitproc.ClassMaintenance, MergeabilityDeadline)
	if err != nil {
		return false, fmt.Errorf("home: merge change %d of repository %s: %w", due.Number, repoID, err)
	}
	outcome = string(m.state)
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
func (c *changes) mergeability(ctx context.Context, repoID string, due code.ChangeDue, m merged,
	at statelog.Position) error {

	res, err := c.estate.Converse(ctx, code.ConversationWrite{OpID: statelog.NewOpID(c.now(), ""), Seen: at,
		Mergeability: &code.Mergeability{RepoID: repoID, ChangeID: due.ID, Revision: due.Revision,
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
// tip: an answer that the rows moved since — stale, the change closed — or
// that it is merged already is the next pass's to read; one that decided
// nothing for a reason of the log's is this turn's failure.
func (c *changes) land(ctx context.Context, repoID string, due code.ChangeDue, at statelog.Position) error {
	res, err := c.estate.Change(ctx, code.ChangeProposal{OpID: statelog.NewOpID(c.now(), ""), Seen: at,
		Landed: &code.ChangeLanded{RepoID: repoID, ChangeID: due.ID, Revision: due.Revision, HeadOID: due.Head,
			TargetRef: due.TargetRef, TargetOID: due.Tip, Node: c.node}})
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
		"target", due.TargetRef, "tip", due.Tip, "unchanged", res.Unchanged, "position", res.Position.String())
	return nil
}

// revise asks the repository's change writer to record due's revision's facts
// f — its trees and its classes with the rest, as the git node read them —
// its touched paths matched against owners, and answers whether they were
// recorded: an answer that the rows moved since — superseded, the change
// closed or merged — is the next pass's to read; one that decided nothing for
// a reason of the log's is this turn's failure.
func (c *changes) revise(ctx context.Context, repoID string, due code.ChangeDue, f *code.RevisionFactsAnswer,
	owners []code.PathOwner, at statelog.Position) (bool, error) {

	matched, digest := code.MatchOwners(owners, f.Touched, f.TouchesAll)
	res, err := c.estate.Converse(ctx, code.ConversationWrite{OpID: statelog.NewOpID(c.now(), ""), Seen: at,
		Revise: &code.Revise{RepoID: repoID, ChangeID: due.ID, N: due.Revision, HeadOID: due.Head,
			TargetRef: due.TargetRef, TargetOID: due.Tip, TargetPosition: due.TipPosition, BaseOID: f.MergeBase,
			BaseTree: f.BaseTree, HeadTree: f.HeadTree, PatchID: f.PatchID,
			PatchIDOmitted: f.PatchIDOmission == code.OmittedPastBound, Touched: f.Touched,
			TouchesAll: f.TouchesAll, Classes: f.Classes, Node: c.node,
			RequestedOwners: matched, OwnersDigest: digest}})
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
		"owners", len(matched), "position", res.Position.String())
	return true, nil
}

// refresh asks the repository's change writer to record due's readiness again
// over the merge its row holds — a mergeability that MERGES NOTHING
// (code.Mergeability.Refresh): no copy synced, no scratch, no git — naming the
// head revision, its head and its target, the writer stamping the merge from
// the row and the readiness from its own snapshot. An answer that the rows
// moved since — superseded: the row's answer judged at the inputs already, a
// merge owed first, the change closed or merged — is the next pass's to read;
// one that decided nothing for a reason of the log's is this turn's failure.
func (c *changes) refresh(ctx context.Context, repoID string, due code.ChangeDue, at statelog.Position) error {
	res, err := c.estate.Converse(ctx, code.ConversationWrite{OpID: statelog.NewOpID(c.now(), ""), Seen: at,
		Mergeability: &code.Mergeability{RepoID: repoID, ChangeID: due.ID, Revision: due.Revision,
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
// with its change's lifecycle (code.AskSettle): those still waiting on a change
// that closed or merged ended, those its end left on a change reopened since
// asked again — the writer stamping the asks it moves from its own snapshot.
// An answer that the rows moved since — superseded: the change reopened or
// ended again, or holding no such ask — is the next pass's to read; one that
// decided nothing for a reason of the log's is this turn's failure.
func (c *changes) settle(ctx context.Context, repoID string, due code.AskDue, at statelog.Position) error {
	res, err := c.estate.Converse(ctx, code.ConversationWrite{OpID: statelog.NewOpID(c.now(), ""), Seen: at,
		Asks: &code.AskSettle{RepoID: repoID, ChangeID: due.ChangeID, State: due.State}})
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
	undecidedLandings = []code.ChangeRefusal{code.ChangeRefusalBusy, code.ChangeRefusalNotApplied,
		code.ChangeRefusalUnavailable}
	undecidedFacts = []code.ConversationRefusal{code.ConversationRefusalBusy, code.ConversationRefusalNotApplied,
		code.ConversationRefusalUnavailable}
)

// recheckBudget is each repository's share of a PASS's merges of its moved
// targets on this node: at most [MergeabilityPerPass] in one window of the
// loop's backstop, whichever turns — a pass's or a nudge's — run them. A rate
// and never a list: the rows say which changes are due, and the window only
// says how many of them a minute may merge, so a node that restarts or a home
// that moves begins a window anew, at most one window's merges early.
type recheckBudget struct {
	mu sync.Mutex
	// window is a pass's length — the loop's backstop — and perPass the
	// merges one repository's window holds, [MergeabilityPerPass]: each a
	// suite's own (export_test.go).
	window  time.Duration
	perPass int
	used    map[string]recheckWindow
	// swept is when the windows that ended were last forgotten: at most once
	// a window, so a node home to many repositories forgets theirs at a cost
	// that follows them once a minute, never once a turn.
	swept time.Time
}

// recheckWindow is one repository's current window: when it began and how
// many merges it has run.
type recheckWindow struct {
	began time.Time
	used  int
}

// newRecheckBudget is a budget of windows of window, MergeabilityPerPass
// merges each.
func newRecheckBudget(window time.Duration) *recheckBudget {
	return &recheckBudget{window: window, perPass: MergeabilityPerPass, used: map[string]recheckWindow{}}
}

// left is how many merges repoID may still run in the window now is in — the
// whole share where its window has ended — forgetting, once a window, every
// window that has.
func (b *recheckBudget) left(repoID string, now time.Time) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if now.Sub(b.swept) >= b.window {
		for repo, w := range b.used {
			if now.Sub(w.began) >= b.window {
				delete(b.used, repo)
			}
		}
		b.swept = now
	}
	w := b.used[repoID]
	if now.Sub(w.began) >= b.window {
		return b.perPass
	}
	return max(0, b.perPass-w.used)
}

// spend counts one merge of repoID's in the window now is in, beginning one
// where none runs.
func (b *recheckBudget) spend(repoID string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	w, ok := b.used[repoID]
	if !ok || now.Sub(w.began) >= b.window {
		w = recheckWindow{began: now}
	}
	w.used++
	b.used[repoID] = w
}

// freshAt is the level a read at floor is asked at: `session` beside it, so
// the rows read hold the record it names, and stale where it is zero.
func freshAt(floor statelog.Position) statelog.Freshness {
	if floor.IsZero() {
		return statelog.Freshness{Level: statelog.ReadStale}
	}
	return statelog.FloorAt(statelog.ReadSession, floor)
}
