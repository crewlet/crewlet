package gitlet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A COMMIT'S CHANGES, the `commit` read: the repository in detail, held to the
// asker's grant as a read of one repository is, and in the same snapshot every
// change the commit belongs to: each whose merge it is (code_changes_merge_idx)
// and each revision it heads (code_revisions_head_idx), each with how it landed
// and its gate ([ChangeLanding], [LandingGate]). The commit and the push that
// introduced it are the repository's content and its Gitlet node's index, never
// these rows: a question asks them by object id beside this read.
//
// A commit belongs to a handful of changes in practice but to any number in the
// worst case, so each list is read to [MaxCommitChanges] and says when it holds
// more (More): a commit's page names what it belongs to; `changes` lists them.

// MaxCommitChanges is the most changes a `commit` read names a commit merged
// and the most revisions it names it heading, each list read one row further to
// say it holds more: a change conversation's default page (defaultReviewLimit),
// past which a commit's page would be listing rather than naming.
const MaxCommitChanges = 50

// CommitChanges is a `commit` read's answer beside the repository's detail
// (ReadAnswer.Commit): the changes one commit of the repository belongs to.
//
// A read's answer, so it takes no versionedFields row; a build predating the
// `commit` kind refuses it whole ([ReadKinds]).
type CommitChanges struct {
	// Commit is the commit, its whole id.
	Commit string `json:"commit"`
	// Merged is every change whose merge is the commit, by its number, each with
	// how it landed and its gate; at most MaxCommitChanges.
	Merged []ChangeLanding `json:"merged,omitempty"`
	// Revisions is every revision the commit is the head of, at most
	// MaxCommitChanges, in the order code_revisions_head_idx keeps them, each
	// change with how it landed once merged and its gate.
	Revisions []CommitRevision `json:"revisions,omitempty"`
	// More is set where either list holds more than it names.
	More bool `json:"more,omitempty"`
}

// CommitRevision is one revision a commit heads: the change and its number.
type CommitRevision struct {
	Change ChangeLanding `json:"change"`
	N      int64         `json:"n"`
}

// ChangeLanding is a change as a commit's page names it and, once merged, how it
// landed (ChangeRow's columns); a `commit` read alone adds a person's override
// reason and the gate, a `commit_marks` read neither.
type ChangeLanding struct {
	ChangeName
	MergeOID     string    `json:"merge_oid,omitempty"`
	MergedAt     time.Time `json:"merged_at,omitzero"`
	MergedVia    MergedVia `json:"merged_via,omitempty"`
	MergedBy     string    `json:"merged_by,omitempty"`
	MergedByKind PartyKind `json:"merged_by_kind,omitempty"`
	// OverrideReason is a person's reason for merging past the gate: their
	// words, declared so.
	OverrideReason string       `json:"override_reason,omitempty" untrusted:"name"`
	Gate           *LandingGate `json:"gate,omitempty"`
}

// LandingGate is a merge's gate as the rows keep it, since the merge record
// keeps no snapshot of it: the approvals of the revision the change merged at,
// and where the checks on the commit the gate judged stand.
type LandingGate struct {
	// Approvals is each standing approval of the merged revision, by handle,
	// at most MaxGateApprovals; ApprovalsMore that there are more. One of an
	// earlier revision is not named, though the gate counted it unchanged.
	Approvals     []GateApproval `json:"approvals,omitempty"`
	ApprovalsMore bool           `json:"approvals_more,omitempty"`
	// ChecksOn is the commit the gate's checks ran on, and Checks where they
	// stand for the change's target; both absent where no attempt is kept.
	ChecksOn string       `json:"checks_on,omitempty"`
	Checks   *CheckRollup `json:"checks,omitempty"`
	// Gone is set where no attempt is kept and the merged revision was pushed
	// more than CheckHistory ago, so its checks may have been swept.
	Gone bool `json:"gone,omitempty"`
}

// GateApproval is one standing approval: who gave it and the revision it
// judged, the merged one.
type GateApproval struct {
	Handle   string    `json:"handle"`
	Kind     PartyKind `json:"kind"`
	Revision int64     `json:"revision"`
}

// MaxGateApprovals is the most approvals a gate names: a caption names a few
// approvers, and eight is past any gate's required approvals.
const MaxGateApprovals = 8

// The indexes a `commit` read and a `commit_marks` read seek, GUARDS as
// planner.go says, each held by plan_test.go to its seek.
const (
	commitMergedKey = "code_changes_merge_idx"
	commitHeadsKey  = "code_revisions_head_idx"
	// participantsKey is code_participants' PRIMARY KEY, (change_id,
	// handle): a change's standing verdicts in handle order.
	participantsKey = "sqlite_autoindex_code_participants_1"
	// changeLandingsKey is code_landings_change_idx, (change_id) where one
	// is named: the landing a queue (or a merge record) made of a change.
	changeLandingsKey = "code_landings_change_idx"
	// changeRevisionsKey is code_revisions' PRIMARY KEY, (change_id, n).
	changeRevisionsKey = "sqlite_autoindex_code_revisions_1"
)

// landingColumns is a change as [scanLanding] reads it.
const landingColumns = `c.id, c.number, c.title, c.state, c.target_ref, c.head_oid, c.merge_oid, c.merged_at,
		c.merged_via, c.merged_by, c.merged_by_kind, c.override_reason, c.revision`

// The `commit` read's statements, registered in plan_test.go: the changes a
// commit merged and the revisions it heads, each one row past its bound, and a
// merged change's approvals, queue landing and merged revision's push instant.
const (
	commitMergedSQL = `SELECT ` + landingColumns + ` FROM code_changes AS c INDEXED BY ` + commitMergedKey + `
		WHERE c.repo_id = ? AND c.merge_oid = ? AND c.merge_oid != '' LIMIT ?`
	commitHeadsSQL = `SELECT r.n, ` + landingColumns + `
		FROM code_revisions AS r INDEXED BY ` + commitHeadsKey + ` CROSS JOIN code_changes AS c
		WHERE r.head_oid = ? AND c.id = r.change_id AND c.repo_id = ? LIMIT ?`
	changeApprovalsSQL = `SELECT handle, kind, verdict_revision FROM code_participants INDEXED BY ` +
		participantsKey + `
		WHERE change_id = ? AND verdict = 'approve' AND verdict_revision = ? ORDER BY handle LIMIT ?`
	changeLandingSQL = `SELECT target_ref, position FROM code_landings INDEXED BY ` + changeLandingsKey + `
		WHERE change_id = ? AND change_id != '' LIMIT 1`
	revisionPushedSQL = `SELECT at FROM code_revisions INDEXED BY ` + changeRevisionsKey + `
		WHERE change_id = ? AND n = ?`
)

// belonging is a change a commit belongs to as its reads scan it: how it landed,
// and what its gate is read by.
type belonging struct {
	ChangeLanding
	id, target, head string
	revision         int64
}

// marked is the landing as a history page's mark carries it: no reason and
// no gate, which a commit's page answers.
func (l belonging) marked() ChangeLanding {
	out := l.ChangeLanding
	out.OverrideReason, out.Gate = "", nil
	return out
}

// headed is a revision a commit heads, its change as it landed.
type headed struct {
	change belonging
	n      int64
}

// scanLanding reads one row of [landingColumns] of a change of repo, after
// lead where given.
func scanLanding(s scanner, repo RepoRef, lead ...any) (belonging, error) {
	var (
		l                         belonging
		number                    int64
		title, state, via, byKind string
		mergedAt                  sql.NullInt64
	)
	if err := s.Scan(append(lead, &l.id, &number, &title, &state, &l.target, &l.head, &l.MergeOID, &mergedAt,
		&via, &l.MergedBy, &byKind, &l.OverrideReason, &l.revision)...); err != nil {
		return belonging{}, err
	}
	l.ChangeName = ChangeName{Repo: repo.Name, Ref: ChangeRef(repo.Name, number), Title: title,
		State: ChangeState(state)}
	l.MergedVia, l.MergedByKind = MergedVia(via), PartyKind(byKind)
	if mergedAt.Valid {
		l.MergedAt = store.DecodeTime(mergedAt.Int64)
	}
	return l, nil
}

// commitMerged is the changes of repo whose merge is commit, at most limit.
func commitMerged(ctx context.Context, tx *sql.Tx, repo RepoRef, commit string, limit int) ([]belonging, error) {
	rows, err := tx.QueryContext(ctx, commitMergedSQL, repo.ID, commit, limit)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read the changes %s merged in %s: %w", commit, repo.Name, err)
	}
	var out []belonging
	err = eachRow(rows, func(s scanner) (err error) {
		var l belonging
		if l, err = scanLanding(s, repo); err == nil {
			out = append(out, l)
		}
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("gitlet: read the changes %s merged in %s: %w", commit, repo.Name, err)
	}
	return out, nil
}

// commitHeads is the revisions of repo's changes commit heads, at most limit.
func commitHeads(ctx context.Context, tx *sql.Tx, repo RepoRef, commit string, limit int) ([]headed, error) {
	rows, err := tx.QueryContext(ctx, commitHeadsSQL, commit, repo.ID, limit)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read the revisions %s heads in %s: %w", commit, repo.Name, err)
	}
	var out []headed
	err = eachRow(rows, func(s scanner) (err error) {
		var (
			h headed
			l belonging
		)
		if l, err = scanLanding(s, repo, &h.n); err == nil {
			h.change = l
			out = append(out, h)
		}
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("gitlet: read the revisions %s heads in %s: %w", commit, repo.Name, err)
	}
	return out, nil
}

// commit serves a `commit` read: the repository in detail, judged as a read
// of one repository is ([Reader.repo]), and the changes q.Commit belongs to,
// each merged through its gate with that gate, in the same transaction.
func (r *Reader) commit(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, cp statelog.Position,
	out *ReadAnswer) error {

	if err := r.repo(ctx, tx, q, chart, cp, out); err != nil {
		return err
	}
	repo := out.Repo.RepoRef
	merged, err := commitMerged(ctx, tx, repo, q.Commit, MaxCommitChanges+1)
	if err != nil {
		return err
	}
	heads, err := commitHeads(ctx, tx, repo, q.Commit, MaxCommitChanges+1)
	if err != nil {
		return err
	}
	c := &CommitChanges{Commit: q.Commit, More: len(merged) > MaxCommitChanges || len(heads) > MaxCommitChanges}
	// ONE GATE A CHANGE, read once: the change a commit merged is often the
	// one whose last revision it heads.
	gates := map[string]*LandingGate{}
	gated := func(l belonging) (ChangeLanding, error) {
		if l.MergedVia != MergedViaDirect && l.MergedVia != MergedViaQueue {
			// A PUSH'S MERGE HAS NO GATE: its pusher is its push row's.
			return l.ChangeLanding, nil
		}
		g, read := gates[l.id]
		if !read {
			var err error
			if g, err = r.gateOf(ctx, tx, repo.ID, l); err != nil {
				return ChangeLanding{}, err
			}
			gates[l.id] = g
		}
		out := l.ChangeLanding
		out.Gate = g
		return out, nil
	}
	for _, l := range merged[:min(len(merged), MaxCommitChanges)] {
		named, err := gated(l)
		if err != nil {
			return err
		}
		c.Merged = append(c.Merged, named)
	}
	for _, h := range heads[:min(len(heads), MaxCommitChanges)] {
		named, err := gated(h.change)
		if err != nil {
			return err
		}
		c.Revisions = append(c.Revisions, CommitRevision{Change: named, N: h.n})
	}
	out.Commit = c
	return nil
}

// gateOf is the gate change l merged through, as the rows keep it: its merged
// revision's approvals, and the checks on its head (a direct merge) or on the
// tip its land record left the branch at (a queue landing, from code_landings).
func (r *Reader) gateOf(ctx context.Context, tx *sql.Tx, repoID string, l belonging) (*LandingGate, error) {
	g := &LandingGate{}
	rows, err := tx.QueryContext(ctx, changeApprovalsSQL, l.id, l.revision, MaxGateApprovals+1)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read the approvals of %s: %w", l.Ref, err)
	}
	err = eachRow(rows, func(s scanner) (err error) {
		var (
			a    GateApproval
			kind string
		)
		if err = s.Scan(&a.Handle, &kind, &a.Revision); err != nil {
			return err
		}
		a.Kind = PartyKind(kind)
		g.Approvals = append(g.Approvals, a)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("gitlet: read the approvals of %s: %w", l.Ref, err)
	}
	if len(g.Approvals) > MaxGateApprovals {
		g.Approvals, g.ApprovalsMore = g.Approvals[:MaxGateApprovals], true
	}
	on := l.head
	if l.MergedVia == MergedViaQueue {
		if on, err = landedTip(ctx, tx, repoID, l.id); err != nil {
			return nil, fmt.Errorf("gitlet: read the landing of %s: %w", l.Ref, err)
		}
	}
	if on != "" {
		if g.Checks, err = rollupOfTarget(ctx, tx, repoID, on, l.target); err != nil {
			return nil, err
		}
	}
	if g.Checks != nil {
		g.ChecksOn = on
	}
	if g.Checks == nil {
		// The gate's attempts ended after its revision was pushed, so none
		// can have been swept before that is CheckHistory old.
		pushed, err := revisionPushed(ctx, tx, l)
		if err != nil {
			return nil, fmt.Errorf("gitlet: read when %s's merged revision was pushed: %w", l.Ref, err)
		}
		g.Gone = pushed.Before(r.now().Add(-CheckHistory))
	}
	return g, nil
}

// revisionPushed is when l's merged revision was pushed, or its merge instant
// where no row of the revision is kept.
func revisionPushed(ctx context.Context, tx *sql.Tx, l belonging) (time.Time, error) {
	var at int64
	err := tx.QueryRowContext(ctx, revisionPushedSQL, l.id, l.revision).Scan(&at)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return l.MergedAt, nil
	case err != nil:
		return time.Time{}, err
	}
	return store.DecodeTime(at), nil
}

// landedTip is the commit the land record that landed change left its target
// at, or "" where no landing row of it is kept: the change's landing, then
// its record's last.
func landedTip(ctx context.Context, tx *sql.Tx, repoID, change string) (string, error) {
	var (
		target   string
		position int64
		tip      string
	)
	err := tx.QueryRowContext(ctx, changeLandingSQL, change).Scan(&target, &position)
	if err == nil {
		err = tx.QueryRowContext(ctx, landingTipSQL, repoID, target, position).Scan(&tip)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return tip, err
}

// validateCommit is [ReadQuery.validate] for a `commit` read: the commit it
// names, a WHOLE id — the rows record whole ids, and an abbreviation is the
// repository's content to expand, which no row read can — and no page: a
// commit's changes are named whole, to MaxCommitChanges.
func (q ReadQuery) validateCommit() error {
	switch {
	case !gitref.ValidOID(q.Commit) || q.Commit == ZeroOID:
		return fmt.Errorf("%w: commit: %q is not a whole commit id", ErrInvalid, q.Commit)
	case q.Limit != 0 || q.Cursor != "":
		return fmt.Errorf("%w: limit: a commit's changes are named whole, never paged", ErrInvalid)
	}
	return nil
}

// eachRow calls each for every row of rows, and closes them.
func eachRow(rows *sql.Rows, each func(scanner) error) error {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := each(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
