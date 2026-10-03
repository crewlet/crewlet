package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// FileQuery is one listing of a project's files.
type FileQuery struct {
	Project string

	// Folder narrows the listing to the paths under it — `reports` answers
	// `reports/q3.md` and `reports/2026/q1.md` and not `reportsheet.md`.
	// Empty is the whole project.
	Folder string

	// Removed includes removed files, stamped as such.
	Removed bool

	// After is the path the previous page ended on, and Limit the page's
	// size, 0 for [DefaultFilePage].
	After string
	Limit int

	Freshness statelog.Freshness
}

// DefaultFilePage and MaxFilePage are a listing's page.
//
// TWO HUNDRED BY DEFAULT AND A THOUSAND AT MOST: a row is a few hundred bytes,
// so a page is well inside what a tool answer carries, and a project with more
// files than that is paged through rather than sent whole.
const (
	DefaultFilePage = 200
	MaxFilePage     = 1000
)

// FileRow is one file as a listing shows it — everything but the chunks.
type FileRow struct {
	Project     string     `json:"project"`
	Path        string     `json:"path"`
	ContentType string     `json:"content_type,omitempty"`
	Hash        string     `json:"hash,omitempty"`
	Size        int64      `json:"size"`
	Version     uint64     `json:"version"`
	CreatedBy   string     `json:"created_by,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedBy   string     `json:"updated_by,omitempty"`
	UpdatedAt   time.Time  `json:"updated_at"`
	RemovedBy   string     `json:"removed_by,omitempty"`
	RemovedAt   *time.Time `json:"removed_at,omitempty"`
}

// FileListing is a page of a project's files, with the framework's own
// verdict on the read.
type FileListing struct {
	Files []FileRow `json:"files"`

	// Next is the path to pass as After for the next page, empty on the
	// last.
	Next string `json:"next,omitempty"`

	Level          statelog.ReadLevel `json:"read_level"`
	LogSeq         uint64             `json:"log_seq"`
	AppliedThrough uint64             `json:"applied_through"`
	LogLag         *uint64            `json:"log_lag,omitempty"`
	Complete       bool               `json:"complete"`
	Incomplete     *Incomplete        `json:"incomplete,omitempty"`
}

// Files answers a page of one project's files, in path order.
//
// AN UNKNOWN PROJECT IS [ErrNoProject], never an empty page: a listing that
// answered nothing for a mistyped key would tell a caller the project has no
// files.
func (r *Reader) Files(ctx context.Context, q FileQuery) (FileListing, error) {
	if q.Freshness.Level == "" {
		return FileListing{}, fmt.Errorf("tracker: this file read names no level — " +
			"a surface resolves an absent read_level to its own default before it reads")
	}
	q.Project = ProjectKey(q.Project)
	folder := strings.Trim(strings.TrimSpace(q.Folder), "/")
	limit := q.Limit
	switch {
	case limit <= 0:
		limit = DefaultFilePage
	case limit > MaxFilePage:
		return FileListing{}, fmt.Errorf("tracker: a file page is at most %d, "+
			"asked for %d", MaxFilePage, limit)
	}
	var listing FileListing
	served, err := r.log.Read(ctx, q.Freshness.Query(fileListScope(q.Project), true),
		func(tx *sql.Tx) error {
			if _, held, err := readProject(ctx, tx, q.Project); err != nil {
				return err
			} else if !held {
				return fmt.Errorf("%w: %s", ErrNoProject, q.Project)
			}
			// KEYSET PAGING ON THE INDEX'S OWN ORDER, and one row past
			// the page so the answer knows whether there is another.
			query := `SELECT path, content_type, hash, size, version, created_by,
					created_at, updated_by, updated_at, removed_by, removed_at
				FROM tracker_files
				WHERE project_key = ? AND path > ?`
			args := []any{q.Project, q.After}
			if folder != "" {
				// A RANGE RATHER THAN A LIKE, so the index serves it and
				// no path character is read as a pattern: every path
				// under `folder/` sorts between `folder/` and `folder0`,
				// `0` being the byte after `/`.
				query += ` AND path > ? AND path < ?`
				args = append(args, folder+"/", folder+"0")
			}
			if !q.Removed {
				query += ` AND removed_at IS NULL`
			}
			query += ` ORDER BY path LIMIT ?`
			args = append(args, limit+1)
			rows, err := tx.QueryContext(ctx, query, args...)
			if err != nil {
				return fmt.Errorf("tracker: list the files of %s: %w", q.Project, err)
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				row := FileRow{Project: q.Project}
				var created, updated int64
				var removed sql.NullInt64
				if err = rows.Scan(&row.Path, &row.ContentType, &row.Hash, &row.Size,
					&row.Version, &row.CreatedBy, &created, &row.UpdatedBy, &updated,
					&row.RemovedBy, &removed); err != nil {
					return err
				}
				row.CreatedAt, row.UpdatedAt = store.DecodeTime(created), store.DecodeTime(updated)
				if removed.Valid {
					at := store.DecodeTime(removed.Int64)
					row.RemovedAt = &at
				}
				listing.Files = append(listing.Files, row)
			}
			if err = rows.Err(); err != nil {
				return err
			}
			if len(listing.Files) > limit {
				listing.Files = listing.Files[:limit]
				listing.Next = listing.Files[limit-1].Path
			}
			position, applied, err := readCheckpoint(ctx, tx)
			if err != nil {
				return err
			}
			listing.LogSeq, listing.AppliedThrough = position, applied
			return nil
		})
	if err != nil {
		return FileListing{}, err
	}
	if listing.Files == nil {
		listing.Files = []FileRow{}
	}
	listing.Level, listing.Complete, listing.LogLag = served.Level, served.Complete, served.Lag
	if served.Incomplete != nil {
		listing.Incomplete = incompleteFrom(served.Incomplete)
	}
	return listing, nil
}

// fileListScope is a listing's closure: the project, under which every one of
// its files' own paths nests.
func fileListScope(project string) statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{
		ScopeTerm{Kind: TermContainer, ID: project}.Path(),
	}}.Normalised()
}

// FileDetail is one file with its manifest — what a reader opens it by.
type FileDetail struct {
	File File `json:"file"`

	Level          statelog.ReadLevel `json:"read_level"`
	LogSeq         uint64             `json:"log_seq"`
	AppliedThrough uint64             `json:"applied_through"`
	LogLag         *uint64            `json:"log_lag,omitempty"`
}

// File answers one file by its address. A file that is not there, or was
// removed, is [ErrNoFile].
//
// A POINT READ, so a record this node could not apply on this file's own
// address refuses the read rather than answering around it — a manifest from
// before a put this node has not applied is the wrong bytes, not fewer of them.
func (r *Reader) File(ctx context.Context, project, path string,
	fresh statelog.Freshness) (FileDetail, error) {

	if fresh.Level == "" {
		return FileDetail{}, fmt.Errorf("tracker: this file read names no level — " +
			"a surface resolves an absent read_level to its own default before it reads")
	}
	project = ProjectKey(project)
	path, err := NormalizeFilePath(path)
	if err != nil {
		return FileDetail{}, err
	}
	subject := FileSubject(project, path)
	scope := statelog.ScopeSet{Paths: []string{subjectPath(subject, project)}}
	var detail FileDetail
	served, err := r.log.Read(ctx, fresh.Query(scope, false), func(tx *sql.Tx) error {
		file, held, readErr := readFile(ctx, tx, subject)
		switch {
		case readErr != nil:
			return readErr
		case !held || file.Removed():
			if _, known, projectErr := readProject(ctx, tx, project); projectErr != nil {
				return projectErr
			} else if !known {
				return fmt.Errorf("%w: %s", ErrNoProject, project)
			}
			return fmt.Errorf("%w: %s in %s", ErrNoFile, path, project)
		}
		detail.File = file
		position, applied, checkpointErr := readCheckpoint(ctx, tx)
		if checkpointErr != nil {
			return checkpointErr
		}
		detail.LogSeq, detail.AppliedThrough = position, applied
		return nil
	})
	if err != nil {
		return FileDetail{}, err
	}
	detail.Level, detail.LogLag = served.Level, served.Lag
	return detail, nil
}

// ObjectEstate is this domain as the object store's passes read it: a barrier
// on the tracker's log, and a read of this node's rows that says whether it
// covers every record. WHICH rows is not here — the passes build that from the
// declared tables ([FileChunkReferences]), so a statement written beside the
// declaration can never disagree with it. See internal/objstore/upkeep's
// Estate.
type ObjectEstate struct {
	Reader *Reader
}

// Name is the domain, as a declaration spells it.
func (ObjectEstate) Name() string { return Domain{}.Name() }

// chunkScope is the references' closure: the whole domain, because a file can
// be in any project and a record this node could not apply may be a put of
// any of them — which is exactly the case the collector must see as
// incomplete.
var chunkScope = statelog.ScopeSet{Paths: []string{ScopeTerm{Kind: TermDomain}.Path()}}

// Barrier waits until this node has applied everything the log had committed
// when it was called, and answers where that is.
func (s ObjectEstate) Barrier(ctx context.Context) (statelog.Position, error) {
	served, err := s.Reader.log.Read(ctx, statelog.Query{
		Level: statelog.ReadLinearizable, Scope: chunkScope, Set: true,
	}, func(*sql.Tx) error { return nil })
	if err != nil {
		return statelog.Position{}, err
	}
	return served.Position, nil
}

// Read runs fn over this node's rows, no earlier than at, and reports whether
// they cover every record of the domain.
func (s ObjectEstate) Read(ctx context.Context, at statelog.Position,
	fn func(*sql.Tx) error) (bool, error) {

	served, err := s.Reader.log.Read(ctx, statelog.Query{
		Level: statelog.ReadStale, Scope: chunkScope, Set: true, MinPosition: at,
	}, fn)
	if err != nil {
		return false, err
	}
	return served.Complete, nil
}
