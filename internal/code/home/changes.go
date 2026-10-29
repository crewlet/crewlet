package home

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Estate is the code log and the git reads as the home's change work reaches
// them — the router's code seam satisfies it (estate.CodeSeam): the due
// listing (Read), a revision's facts (GitRead, asked of the repository's git
// nodes, its home first), a record of them on the change (Converse, asked of
// the repository's committing data node's change writer) and a landing (Change,
// asked of its committer). Each raises this node's floors past what it landed,
// so the next listing reads it wherever it is answered.
type Estate interface {
	Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error)
	GitRead(ctx context.Context, q code.GitQuery) (code.GitAnswer, error)
	Converse(ctx context.Context, w code.ConversationWrite) (code.ConversationResult, error)
	Change(ctx context.Context, p code.ChangeProposal) (code.ChangeResult, error)
}

// WorkerChanges is the change work's worker's name, as
// crewlet.code.home.passes counts its turns.
const WorkerChanges = "changes"

// changes is the home's CHANGE WORK, the worker every loop is built with: for
// each live change of a repository whose head revision holds no facts against
// the branch it merges into, the revision's facts read and recorded on the
// change, or the change landed where its target holds its head.
type changes struct {
	estate  Estate
	node    string
	metrics *metrics.Recorder
	log     *slog.Logger
	now     func() time.Time
}

// Name is [WorkerChanges].
func (c *changes) Name() string { return WorkerChanges }

// Due is a page of the repositories holding a due change: the `changes_due`
// listing (internal/code's ReadChangesDue).
func (c *changes) Due(ctx context.Context, floor statelog.Position, cursor string) ([]string, string, error) {
	a, err := c.estate.Read(ctx, code.ReadQuery{Kind: code.ReadChangesDue, Cursor: cursor, Fresh: freshAt(floor)})
	if err != nil {
		return nil, "", err
	}
	return a.DueRepos, a.Next, nil
}

// Work is one repository's due changes, every page, each worked in the order
// the listing names them ([changes.change]) — a failure of one left for the
// next pass while the others are worked — until they are done or the turn's
// context ends.
func (c *changes) Work(ctx context.Context, repoID string, floor statelog.Position) error {
	q := code.ReadQuery{Kind: code.ReadChangesDue, ChangesDue: &code.ChangesDueQuery{RepoID: repoID},
		Fresh: freshAt(floor)}
	var failed []error
	for {
		a, err := c.estate.Read(ctx, q)
		if err != nil {
			return fmt.Errorf("home: read repository %s's due changes: %w", repoID, err)
		}
		for _, due := range a.DueChanges {
			if err := ctx.Err(); err != nil {
				return errors.Join(append(failed, err)...)
			}
			if err := c.change(ctx, repoID, due, a.PathOwners, a.Position); err != nil {
				failed = append(failed, err)
			}
		}
		if a.Next == "" {
			return errors.Join(failed...)
		}
		q.Cursor = a.Next
	}
}

// change is one due change: its head revision's facts against its target's
// tip as the listing read it — asked at the listing's position, so the git node
// that answers holds both commits — then the change LANDED where its target
// holds its head, and the facts RECORDED otherwise, the path owners matched
// against the touched paths here, on the home (code.MatchOwners), beside the
// digest of the owners the listing read.
func (c *changes) change(ctx context.Context, repoID string, due code.ChangeDue, owners []code.PathOwner,
	at statelog.Position) error {

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
		return fmt.Errorf("home: read the facts of revision %d of change %d of repository %s: %w", due.Revision,
			due.Number, repoID, err)
	case read.RevisionFacts == nil:
		return fmt.Errorf("home: the facts of revision %d of change %d of repository %s were answered with none",
			due.Revision, due.Number, repoID)
	}
	f := read.RevisionFacts
	if f.HeadInTarget {
		return c.land(ctx, repoID, due, at)
	}
	return c.revise(ctx, repoID, due, f, owners, at)
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
// f, its touched paths matched against owners: an answer that the rows moved
// since — superseded, the change closed or merged — is the next pass's to
// read; one that decided nothing for a reason of the log's is this turn's
// failure.
func (c *changes) revise(ctx context.Context, repoID string, due code.ChangeDue, f *code.RevisionFactsAnswer,
	owners []code.PathOwner, at statelog.Position) error {

	matched, digest := code.MatchOwners(owners, f.Touched, f.TouchesAll)
	res, err := c.estate.Converse(ctx, code.ConversationWrite{OpID: statelog.NewOpID(c.now(), ""), Seen: at,
		Revise: &code.Revise{RepoID: repoID, ChangeID: due.ID, N: due.Revision, HeadOID: due.Head,
			TargetRef: due.TargetRef, TargetOID: due.Tip, TargetPosition: due.TipPosition, BaseOID: f.MergeBase,
			PatchID: f.PatchID, PatchIDOmitted: f.PatchIDOmitted, Touched: f.Touched, TouchesAll: f.TouchesAll,
			Node: c.node, RequestedOwners: matched, OwnersDigest: digest}})
	switch {
	case err != nil:
		return fmt.Errorf("home: record the facts of revision %d of change %d of repository %s: %w", due.Revision,
			due.Number, repoID, err)
	case slices.Contains(undecidedFacts, res.Refusal):
		return fmt.Errorf("home: record the facts of revision %d of change %d of repository %s: %s: %s",
			due.Revision, due.Number, repoID, res.Refusal, res.Detail)
	case res.Refusal != "":
		c.log.Debug("code_change_facts_refused", "repo", repoID, "change", due.Number, "revision", due.Revision,
			"refusal", res.Refusal, "detail", res.Detail)
		return nil
	}
	c.log.Debug("code_change_facts_recorded", "repo", repoID, "change", due.Number, "revision", due.Revision,
		"target", due.TargetRef, "tip", due.Tip, "touched", len(f.Touched), "touches_all", f.TouchesAll,
		"owners", len(matched), "position", res.Position.String())
	return nil
}

// undecidedLandings and undecidedFacts are every refusal of a landing and of a
// record of facts that decided NOTHING for a reason of the writer's or of the
// log's — busy, the log refusing the record or dropping it everywhere — which
// a turn counts as failed, its change listed again by the next pass; every
// other refusal is the rows answering, which the next pass reads again.
var (
	undecidedLandings = []code.ChangeRefusal{code.ChangeRefusalBusy, code.ChangeRefusalNotApplied,
		code.ChangeRefusalUnavailable}
	undecidedFacts = []code.ConversationRefusal{code.ConversationRefusalBusy, code.ConversationRefusalNotApplied,
		code.ConversationRefusalUnavailable}
)

// freshAt is the level a read at floor is asked at: `session` beside it, so
// the rows read hold the record it names, and stale where it is zero.
func freshAt(floor statelog.Position) statelog.Freshness {
	if floor.IsZero() {
		return statelog.Freshness{Level: statelog.ReadStale}
	}
	return statelog.FloorAt(statelog.ReadSession, floor)
}
