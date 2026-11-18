package gitlet

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

// PurgeStep takes ONE step of purging a deleted repository past its retention:
// a part while more remains than one record may delete ([PurgePartRows], and
// [PurgePartBytes] for the wide rows), then the purge, which writes the
// deletion marker and releases the name. A caller steps until [PurgeResult.Done].
//
// Inside retention it is [ErrNotYet]; on a repository not deleted,
// [ErrNotDeleted]; on one already purged, done with nothing written; on an
// unknown id, [ErrRepoNotFound]. A PURGING repository is stepped whatever its
// age: its rows are partly gone and nothing restores it. retention is the
// caller's epoch's value, never defaulted here.
func (s *Store) PurgeStep(ctx context.Context, repoID string, retention time.Duration) (PurgeResult, error) {
	if retention <= 0 {
		return PurgeResult{}, refusedf("%w: retention: %v is not a retention", ErrInvalid, retention)
	}
	// THE NAME IS READ BEFORE THE DECIDE: the scope, which holds the claim
	// the purge releases, is stated before it runs, and the decide holds the
	// row to it. No rename reaches a deleted or purging repository, so the
	// reads agree. An old name a rename left redirecting stays reserved until
	// its window ends, and the retention pass frees it (Store.ReleaseName).
	name, found, err := s.repoName(ctx, repoID)
	if err != nil {
		return PurgeResult{}, err
	}
	subject := RepoSubject(repoID)
	// THE CHECKS ARE IN EVERY STEP'S SCOPE: a step deletes check rows its
	// checks subject's records write, so a check record this node cannot
	// decode holds the step back, and the decide reads them (remainingSQL).
	parts := []string{RepoPath(repoID), ChecksPath(repoID)}
	scope := parts
	if found {
		scope = append([]string{NamePath(NameToken(name))}, parts...)
	}
	opID := statelog.NewOpID(time.Now(), "code_purge")
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
					return statelog.Decision{}, fmt.Errorf("gitlet: read %s's deletion marker: %w", repoID, markErr)
				}
				if marked == 0 {
					return statelog.Decision{}, fmt.Errorf("%w: %s", ErrRepoNotFound, repoID)
				}
				done = true
				return statelog.Decision{}, nil
			}
			switch {
			case row.Name != name:
				return statelog.Decision{}, fmt.Errorf("gitlet: repository %s is named %q "+
					"and this purge was scoped to %q", repoID, row.Name, name)
			case row.Status.live():
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
				return statelog.Decision{}, fmt.Errorf("gitlet: count %s's rows: %w", repoID, countErr)
			}
			wrote = true
			actor := Actor{ID: s.nodeID, Kind: ActorSystem}
			if remaining > PurgePartRows || weighed > PurgePartBytes {
				return recordFor(stamp, subject, OpPurgePart, parts, opID,
					actor, PurgePart{RepoID: repoID, Part: row.PurgeParts + 1}, at)
			}
			done = true
			return recordFor(stamp, subject, OpPurge, scope, opID, actor,
				Purge{RepoID: repoID, Name: row.Name, Reason: "deleted past code.deleted_retention_days"}, at)
		},
	})
	switch {
	case err != nil:
		return PurgeResult{}, fmt.Errorf("gitlet: purge repository %s: %w", repoID, err)
	case !wrote:
		return PurgeResult{Done: done}, nil
	case result.Outcome == statelog.OutcomeUnknown:
		return PurgeResult{}, unknownOutcome(ctx, "gitlet: whether the purge step on %s landed is "+
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
		return "", false, fmt.Errorf("gitlet: read repository %s's name: %w", id, err)
	}
	return name, true, nil
}

// NameOf is the name an operator reads repository id by: this node's rows'
// name for it, or the id where they hold none or could not be read. It serves
// a report keyed by name with only an id in hand (the retention pass's
// failures, the busy alarm's repository).
func (s *Store) NameOf(ctx context.Context, id string) string {
	if name, found, err := s.repoName(ctx, id); err == nil && found {
		return name
	}
	return id
}

// The purge's decide statements. remainingSQL is a BOUNDED COUNT, since the
// decide asks only "more than PurgePartRows": every table a part deletes from
// is counted at most ?2 (PurgePartRows+1) deep for repository ?1, and the
// changes' notices by code_changes.notices — code_notifications is divergent,
// so the decide counts what no node holds fewer rows than (apply_notices.go).
// Its second column weighs the wide rows over the same depth, off narrow rows
// or indexes that carry the width (code_pushes_repo_idx,
// code_checks_finished_idx), never off the wide rows, whose overflow pages a
// read would walk. Past PurgePartRows pushes or changes the count alone forms
// a part, and short of it each sum is the whole. A table or wide row left out is one the final purge's
// unbounded statements delete whatever it holds.
const (
	repoNameSQL  = `SELECT name FROM code_repos WHERE id = ?`
	remainingSQL = `SELECT
		(SELECT COUNT(*) FROM (SELECT 1 FROM code_pushes WHERE repo_id = ?1 LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_refs WHERE repo_id = ?1 LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_packs WHERE repo_id = ?1 LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_packs_retired WHERE repo_id = ?1 LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_ref_drops WHERE repo_id = ?1 LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM ` + firstChanges + `
			CROSS JOIN code_revisions AS r WHERE r.change_id = c.id LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM ` + firstChanges + `
			CROSS JOIN code_change_authors AS a WHERE a.change_id = c.id LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM ` + firstChanges + `
			CROSS JOIN code_change_bodies AS b WHERE b.change_id = c.id LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM ` + firstChanges + `
			CROSS JOIN code_reviewers AS r WHERE r.change_id = c.id LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM ` + firstChanges + `
			CROSS JOIN code_participants AS p WHERE p.change_id = c.id LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM ` + firstChanges + `
			CROSS JOIN code_findings AS f WHERE f.change_id = c.id LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM ` + firstChanges + `
			CROSS JOIN code_comments AS m WHERE m.change_id = c.id LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM ` + firstChanges + `
			CROSS JOIN code_finding_texts AS x WHERE x.change_id = c.id LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM ` + firstChanges + `
			CROSS JOIN code_reviews AS w WHERE w.change_id = c.id LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM ` + firstChanges + `
			CROSS JOIN code_revision_facts AS t WHERE t.change_id = c.id LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM ` + firstChanges + `
			CROSS JOIN code_mergeability AS g WHERE g.change_id = c.id LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM ` + firstChanges + `
			CROSS JOIN code_conversations AS v WHERE v.change_id = c.id LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_changes INDEXED BY ` + changesNumberKey + `
			WHERE repo_id = ?1 LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_checks INDEXED BY ` + checksEndedKey + `
			WHERE repo_id = ?1 LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_check_logs WHERE repo_id = ?1 LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_check_dispatches INDEXED BY ` + checkDispatchesKey + `
			WHERE repo_id = ?1 LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_check_targets INDEXED BY ` + checkTargetsKey + `
			WHERE repo_id = ?1 LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_queue WHERE repo_id = ?1 LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_candidates WHERE repo_id = ?1 LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_landings WHERE repo_id = ?1 LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_green WHERE repo_id = ?1 LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_tips_due WHERE repo_id = ?1 LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_push_days WHERE repo_id = ?1 LIMIT ?2))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_pusher_days WHERE repo_id = ?1 LIMIT ?2))
		+ (SELECT COALESCE(SUM(notices), 0) FROM
			(SELECT notices FROM code_changes INDEXED BY ` + changesNumberKey + `
				WHERE repo_id = ?1 LIMIT ?2)),
		(SELECT COALESCE(SUM(row_bytes), 0) FROM
			(SELECT row_bytes FROM code_pushes WHERE repo_id = ?1 LIMIT ?2))
		+ (SELECT COALESCE(SUM(body_bytes), 0) FROM
			(SELECT body_bytes FROM code_changes INDEXED BY ` + changesNumberKey + `
				WHERE repo_id = ?1 LIMIT ?2))
		+ (SELECT COALESCE(SUM(text_bytes), 0) FROM
			(SELECT v.text_bytes FROM ` + firstChanges + `
				CROSS JOIN code_conversations AS v WHERE v.change_id = c.id LIMIT ?2))
		+ (SELECT COALESCE(SUM(row_bytes), 0) FROM
			(SELECT row_bytes FROM code_checks INDEXED BY ` + checksEndedKey + `
				WHERE repo_id = ?1 LIMIT ?2))`
	// firstChanges is repository ?1's first ?2 changes in number order, as c.
	// A change's dependents carry no repo_id, so they are counted through it,
	// as the final purge reaches them ([changesNumberKey]). A LIMIT bounds the
	// rows a term finds, not the changes it walks: driven from every change, a
	// table most changes hold no row of would walk them all, O(N²/PurgePartRows)
	// a purge. Past PurgePartRows changes the changes' own term forms a part
	// anyway, and at or below it these are every change.
	firstChanges = `(SELECT id FROM code_changes INDEXED BY ` + changesNumberKey + `
			WHERE repo_id = ?1 ORDER BY number LIMIT ?2) AS c`
)
