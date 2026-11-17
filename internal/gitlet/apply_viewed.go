package gitlet

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// A PERSON'S VIEWED FILES AND A REPOSITORY'S VIEWED KEY, APPLIED: the applier's writes of
// code_viewed_files and code_viewed_keys, each a function of its record and its own subject's rows.

// applyViewed applies one viewed record to its person's rows: the changes it forgets, the marks it
// takes back, the files it marks at its position and instant, then the oldest past MaxViewedFiles.
func (a *Applier) applyViewed(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	p, ok := payload.(ViewedMarks)
	if !ok || at.record.Op != OpViewed {
		return 0, fmt.Errorf("gitlet: %s is not an op this build applies as a person's viewed marks (the record "+
			"at %s)", at.record.Op, at.position)
	}
	if p.Handle != at.record.Subject.ID {
		return 0, fmt.Errorf("gitlet: the record at %s is on person.%s and its payload names %s; a record on one "+
			"person's subject may write only that person's marks", at.position, at.record.Subject.ID, p.Handle)
	}
	written := 0
	count := func(res sql.Result, what string, execErr error) error {
		if execErr != nil {
			return fmt.Errorf("gitlet: %s of %s at %s: %w", what, p.Handle, at.position, execErr)
		}
		n, _ := res.RowsAffected()
		written += int(n)
		return nil
	}
	if len(p.Forget) > 0 {
		list, encodeErr := json.Marshal(p.Forget)
		if encodeErr != nil {
			return 0, encodeErr
		}
		res, execErr := tx.ExecContext(ctx, forgetViewedSQL, p.Handle, string(list))
		if err = count(res, "forget the viewed marks of ended changes", execErr); err != nil {
			return 0, err
		}
	}
	if len(p.Unviewed) > 0 {
		list, encodeErr := json.Marshal(p.Unviewed)
		if encodeErr != nil {
			return 0, encodeErr
		}
		res, execErr := tx.ExecContext(ctx, unviewSQL, p.Handle, p.ChangeID, string(list))
		if err = count(res, "take back viewed marks", execErr); err != nil {
			return 0, err
		}
	}
	for _, mark := range p.Viewed {
		res, execErr := tx.ExecContext(ctx, viewSQL, p.Handle, p.ChangeID, mark.PathDigest, mark.FileDigest,
			at.at(), at.packed)
		if err = count(res, "mark a file viewed", execErr); err != nil {
			return 0, err
		}
	}
	var held int
	if err = tx.QueryRowContext(ctx, viewedCountSQL, p.Handle).Scan(&held); err != nil {
		return 0, fmt.Errorf("gitlet: count the viewed marks of %s at %s: %w", p.Handle, at.position, err)
	}
	if held > MaxViewedFiles {
		res, execErr := tx.ExecContext(ctx, dropOldestViewedSQL, p.Handle, held-MaxViewedFiles)
		if err = count(res, "drop the oldest viewed marks", execErr); err != nil {
			return 0, err
		}
	}
	return written, nil
}

// applyViewedKey writes the key a repository's viewed marks are digested under: the first key stands.
func (a *Applier) applyViewedKey(ctx context.Context, tx *sql.Tx, at applyContext, p ViewedKeyRecord) (int, error) {
	if !p.Key.Valid() {
		return 0, fmt.Errorf("gitlet: the viewed key record at %s carries a key for repository %s that is not %d "+
			"bytes in lowercase hex", at.position, p.RepoID, ViewedKeyBytes)
	}
	res, err := tx.ExecContext(ctx, viewedKeyInsertSQL, p.RepoID, string(p.Key))
	if err != nil {
		return 0, fmt.Errorf("gitlet: record repository %s's viewed key at %s: %w", p.RepoID, at.position, err)
	}
	n, _ := res.RowsAffected()
	if _, err = tx.ExecContext(ctx, touchedSQL, at.packed, p.RepoID); err != nil {
		return 0, fmt.Errorf("gitlet: move %s's version at %s: %w", p.RepoID, at.position, err)
	}
	return int(n) + 1, nil
}

// The viewed applies' statements, each registered in plan_test.go but the two inserts, which read
// nothing. A key already recorded stands: replacing it would leave every mark made under it matching
// nothing.
const (
	forgetViewedSQL = `DELETE FROM code_viewed_files WHERE rowid IN (SELECT v.rowid
		FROM json_each(?2) AS j CROSS JOIN code_viewed_files AS v
		WHERE v.handle = ?1 AND v.change_id = CAST(j.value AS TEXT))`
	unviewSQL = `DELETE FROM code_viewed_files WHERE rowid IN (SELECT v.rowid
		FROM json_each(?3) AS j CROSS JOIN code_viewed_files AS v
		WHERE v.handle = ?1 AND v.change_id = ?2 AND v.path_digest = CAST(j.value AS TEXT))`
	viewSQL = `INSERT INTO code_viewed_files (handle, change_id, path_digest, file_digest, at, position)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (handle, change_id, path_digest) DO UPDATE SET
		file_digest = excluded.file_digest, at = excluded.at, position = excluded.position`
	viewedCountSQL      = `SELECT COUNT(*) FROM code_viewed_files WHERE handle = ?`
	dropOldestViewedSQL = `DELETE FROM code_viewed_files WHERE rowid IN (SELECT rowid
		FROM code_viewed_files WHERE handle = ?1 ORDER BY position, change_id, path_digest LIMIT ?2)`
	viewedKeyInsertSQL = `INSERT INTO code_viewed_keys (repo_id, secret) VALUES (?, ?)
		ON CONFLICT (repo_id) DO NOTHING`
)
