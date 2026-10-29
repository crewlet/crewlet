package code

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// PurgeResult is what one [Store.PurgeStep] did.
type PurgeResult struct {
	// Done reports the purge complete ON THE LOG: this step appended its
	// final record — the purge, which writes the deletion marker and
	// releases the name — or found the repository already purged in this
	// node's rows.
	Done bool

	// Outcome is the outcome of the record this step appended: applied, or
	// pending — durable at Position and not yet applied on THIS node, whose
	// rows then still hold the repository. Never unknown, which is the
	// step's error. Empty when the step appended nothing.
	Outcome statelog.Outcome

	// Position is where that record landed; zero when the step appended
	// nothing.
	Position statelog.Position
}

// Held reports that this node's rows already hold what the step did: its
// record applied here, or it appended none. A purge that is Done but not
// Held has released its name on the log and not yet in this node's rows, so
// anything this node decides about that name still finds it purging.
func (r PurgeResult) Held() bool { return r.Outcome != statelog.OutcomePending }

// PurgeStep takes ONE step of purging a deleted repository past its retention,
// and reports what it did. Each step is one record bounded by [PurgePartRows]
// and, for its wide rows — the push rows and the changes' bodies — by
// [PurgePartBytes]: a part while more remains than one record may delete,
// then the purge itself — the gate that writes the deletion marker and
// releases the name — once what remains fits both. A caller steps until
// [PurgeResult.Done].
//
// Inside retention it is [ErrNotYet]; on a repository that is not deleted,
// [ErrNotDeleted]; on one already purged, done with nothing written; on an id
// nobody has, [ErrRepoNotFound]. A PURGING repository is stepped whatever its
// age: a purge once begun is always finished, because its rows are already
// partly gone and nothing restores it. retention is the caller's epoch's
// value, never defaulted here.
func (s *Store) PurgeStep(ctx context.Context, repoID string, retention time.Duration) (PurgeResult, error) {
	if retention <= 0 {
		return PurgeResult{}, fmt.Errorf("%w: retention: %v is not a retention", ErrInvalid, retention)
	}
	// THE NAME IS READ BEFORE THE DECIDE, because the request's scope is
	// stated before the decide runs and the purge releases the name's
	// claim; the decide holds the row to it. A purge steps only a deleted or
	// purging repository, which no rename reaches (a tool's rename refuses
	// one), so the two reads agree. An OLD name a rename left redirecting is
	// not the purge's: its redirect keeps the name reserved until its window
	// ends, as it would for a live repository, and the retention pass's
	// release frees it then (Store.ReleaseName).
	name, found, err := s.repoName(ctx, repoID)
	if err != nil {
		return PurgeResult{}, err
	}
	subject := RepoSubject(repoID)
	scope := []string{RepoPath(repoID)}
	if found {
		scope = append([]string{NamePath(NameToken(name))}, scope...)
	}
	opID := statelog.NewOpID(time.Now(), "code.purge")
	at := s.now()
	var done, wrote bool
	result, err := s.publisher.Publish(ctx, statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: scope},
		OpID: opID, Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			done, wrote = false, false
			row, present, readErr := repoIn(ctx, tx, repoID)
			if readErr != nil {
				return statelog.Decision{}, readErr
			}
			if !present {
				var marked int
				if markErr := tx.QueryRowContext(ctx, purgedSQL, repoID).Scan(&marked); markErr != nil {
					return statelog.Decision{}, fmt.Errorf("code: read %s's deletion marker: %w", repoID, markErr)
				}
				if marked == 0 {
					return statelog.Decision{}, fmt.Errorf("%w: %s", ErrRepoNotFound, repoID)
				}
				done = true
				return statelog.Decision{}, nil
			}
			switch {
			case row.Name != name:
				return statelog.Decision{}, fmt.Errorf("code: repository %s is named %q "+
					"and this purge was scoped to %q", repoID, row.Name, name)
			case row.Status == RepoStatusActive || row.Status == RepoStatusArchived:
				return statelog.Decision{}, fmt.Errorf("%w: %s is %s", ErrNotDeleted, row.Name, row.Status)
			case row.Status == RepoStatusDeleted && row.DeletedAt.Valid &&
				store.DecodeTime(row.DeletedAt.Int64).Add(retention).After(s.now()):
				return statelog.Decision{}, fmt.Errorf("%w: %s was deleted at %s", ErrNotYet,
					row.Name, store.DecodeTime(row.DeletedAt.Int64).Format(time.RFC3339))
			}
			var remaining int
			var weighed int64
			if countErr := tx.QueryRowContext(ctx, remainingSQL, repoID,
				PurgePartRows+1).Scan(&remaining, &weighed); countErr != nil {
				return statelog.Decision{}, fmt.Errorf("code: count %s's rows: %w", repoID, countErr)
			}
			wrote = true
			actor := Actor{ID: s.nodeID, Kind: ActorSystem}
			if remaining > PurgePartRows || weighed > PurgePartBytes {
				return recordFor(stamp, subject, OpPurgePart, []string{RepoPath(repoID)}, opID,
					actor, PurgePart{RepoID: repoID, Part: row.PurgeParts + 1}, at)
			}
			done = true
			return recordFor(stamp, subject, OpPurge, scope, opID, actor,
				Purge{RepoID: repoID, Name: row.Name, Reason: "deleted past code.deleted_retention_days"}, at)
		},
	})
	switch {
	case err != nil:
		return PurgeResult{}, fmt.Errorf("code: purge repository %s: %w", repoID, err)
	case !wrote:
		return PurgeResult{Done: done}, nil
	case result.Outcome == statelog.OutcomeUnknown:
		return PurgeResult{}, unknownOutcome(ctx, "code: whether the purge step on %s landed is "+
			"unknown (operation %s); the next pass decides it again", repoID, opID)
	}
	return PurgeResult{Done: done, Outcome: result.Outcome, Position: result.Position}, nil
}

// repoName is a repository's name, read outside any decision; false when this
// node holds no row of it.
func (s *Store) repoName(ctx context.Context, id string) (string, bool, error) {
	var name string
	err := s.db.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, repoNameSQL, id).Scan(&name)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("code: read repository %s's name: %w", id, err)
	}
	return name, true, nil
}

// NameOf is the name repository id is reported by, as an operator reads it:
// the name this node's rows hold for it, or the id where they hold none or
// could not be read — for a report keyed by name that has only an id in hand,
// the retention pass's failures and the busy alarm's repository among them.
func (s *Store) NameOf(ctx context.Context, id string) string {
	if name, found, err := s.repoName(ctx, id); err == nil && found {
		return name
	}
	return id
}

// The purge's decide statements. remainingSQL is the tracker's BOUNDED COUNT:
// the decide needs "more than PurgePartRows", never the total, so each table
// a part deletes from — the pushes, the refs, the packs, the retired packs, the
// ref drops, the changes' revisions, authors, bodies and rows, and their
// conversations' reviewers, participants, findings, comments, finding texts,
// reviews, revisions' facts and rows — is counted at most PurgePartRows + 1
// deep (?2, against the repository ?1), and beside them what the wide rows
// among them weigh, summed over the same depth: the push rows' row_bytes from
// code_pushes_repo_idx, which carries every row's width, the changes'
// body_bytes from their own narrow rows, never from the bodies, whose overflow
// pages a read would walk, and their conversations' text_bytes from the
// conversations' narrow rows, never from the comments, the findings' texts,
// the reviews' summaries or the facts' touched paths it counts. Past
// PurgePartRows pushes or changes the count alone already forms a part, and
// short of it each sum is the whole. A table left out of the count — or a wide
// row left out of the sum — is one the final purge's unbounded statements
// would delete whatever it held.
//
// A CHANGE'S REVISIONS, AUTHORS, BODY AND CONVERSATION ARE COUNTED THROUGH
// THEIR CHANGE, the changes in number order ([changesNumberKey]) and each
// one's rows by its key — the dependents carry no repo_id — which is also how
// the final purge reaches them, so the count is of exactly the rows it would
// delete.
const (
	repoNameSQL  = `SELECT name FROM code_repos WHERE id = ?`
	remainingSQL = `SELECT
		(SELECT COUNT(*) FROM (SELECT 1 FROM code_pushes WHERE repo_id = ?1 LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_refs WHERE repo_id = ?1 LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_packs WHERE repo_id = ?1 LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_packs_retired WHERE repo_id = ?1 LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_ref_drops WHERE repo_id = ?1 LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_changes AS c INDEXED BY ` + changesNumberKey + `
			CROSS JOIN code_revisions AS r WHERE c.repo_id = ?1 AND r.change_id = c.id LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_changes AS c INDEXED BY ` + changesNumberKey + `
			CROSS JOIN code_change_authors AS a WHERE c.repo_id = ?1 AND a.change_id = c.id LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_changes AS c INDEXED BY ` + changesNumberKey + `
			CROSS JOIN code_change_bodies AS b WHERE c.repo_id = ?1 AND b.change_id = c.id LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_changes AS c INDEXED BY ` + changesNumberKey + `
			CROSS JOIN code_reviewers AS r WHERE c.repo_id = ?1 AND r.change_id = c.id LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_changes AS c INDEXED BY ` + changesNumberKey + `
			CROSS JOIN code_participants AS p WHERE c.repo_id = ?1 AND p.change_id = c.id LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_changes AS c INDEXED BY ` + changesNumberKey + `
			CROSS JOIN code_findings AS f WHERE c.repo_id = ?1 AND f.change_id = c.id LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_changes AS c INDEXED BY ` + changesNumberKey + `
			CROSS JOIN code_comments AS m WHERE c.repo_id = ?1 AND m.change_id = c.id LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_changes AS c INDEXED BY ` + changesNumberKey + `
			CROSS JOIN code_finding_texts AS x WHERE c.repo_id = ?1 AND x.change_id = c.id LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_changes AS c INDEXED BY ` + changesNumberKey + `
			CROSS JOIN code_reviews AS w WHERE c.repo_id = ?1 AND w.change_id = c.id LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_changes AS c INDEXED BY ` + changesNumberKey + `
			CROSS JOIN code_revision_facts AS t WHERE c.repo_id = ?1 AND t.change_id = c.id LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_changes AS c INDEXED BY ` + changesNumberKey + `
			CROSS JOIN code_conversations AS v WHERE c.repo_id = ?1 AND v.change_id = c.id LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_changes INDEXED BY ` + changesNumberKey + `
			WHERE repo_id = ?1 LIMIT ?2)),
		(SELECT COALESCE(SUM(row_bytes), 0) FROM
			(SELECT row_bytes FROM code_pushes WHERE repo_id = ?1 LIMIT ?2))
		+ (SELECT COALESCE(SUM(body_bytes), 0) FROM
			(SELECT body_bytes FROM code_changes INDEXED BY ` + changesNumberKey + `
				WHERE repo_id = ?1 LIMIT ?2))
		+ (SELECT COALESCE(SUM(text_bytes), 0) FROM
			(SELECT v.text_bytes FROM code_changes AS c INDEXED BY ` + changesNumberKey + `
				CROSS JOIN code_conversations AS v WHERE c.repo_id = ?1 AND v.change_id = c.id LIMIT ?2))`
)
