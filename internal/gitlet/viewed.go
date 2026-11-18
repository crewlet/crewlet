package gitlet

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A PERSON'S VIEWED FILES: the marks a person keeps over the files of a change they read, each as
// two HMAC-SHA256 digests under a key of the change's repository, decided on their own subject;
// the key, recorded on the repository's subject before its first mark; and the read of a change's.

// ViewedGesture is one gesture over the files of one change: the files marked viewed, each as the
// revision read gives it, and the paths whose marks are taken back.
type ViewedGesture struct {
	// RepoID and ChangeID are the change's repository and the change; it takes marks while it is a
	// draft or open.
	RepoID   string `json:"repo_id"`
	ChangeID string `json:"change_id"`
	// Viewed and Unviewed together name 1 to MaxViewedPerMark files, each once.
	Viewed   []ViewedFile `json:"viewed,omitempty"`
	Unviewed []string     `json:"unviewed,omitempty"`
}

// ViewedFile is one file marked viewed: its path, and the object id and mode the revision read gives
// it ([DiffFile.NewOID], [DiffFile.NewMode]), neither for a file the revision deletes. The mark reads
// unviewed once a later revision changes either.
type ViewedFile struct {
	Path    string `json:"path"`
	BlobOID string `json:"blob_oid,omitempty"`
	Mode    string `json:"mode,omitempty"`
}

// ViewedMarks is a viewed record's payload: whose marks, the gesture on one change by its keyed
// digests, and the ended changes whose marks it forgets, as its decide found them.
type ViewedMarks struct {
	Handle   string       `json:"handle"`
	ChangeID string       `json:"change_id"`
	Viewed   []ViewedMark `json:"viewed,omitempty"`
	Unviewed []string     `json:"unviewed,omitempty"`
	Forget   []string     `json:"forget,omitempty"`
}

// ViewedMark is one file's mark: its path's digest and the digest of the state it was marked at, each
// under its repository's viewed key ([ViewedKey.PathDigest], [ViewedKey.FileDigest]).
type ViewedMark struct {
	PathDigest string `json:"path_digest"`
	FileDigest string `json:"file_digest"`
}

// ViewedKeyRecord is a viewed_key record's payload: the repository, and the key the writer of its
// first viewed mark chose, which every data node writes as it is.
type ViewedKeyRecord struct {
	RepoID string    `json:"repo_id"`
	Key    ViewedKey `json:"key"`
}

// ViewedRow is one file the asker marked viewed, and when: current while the revision shown gives
// the path its PathDigest names a state of the same FileDigest.
type ViewedRow struct {
	ViewedMark
	At time.Time `json:"at"`
}

// Cursor is the cursor that resumes a page of viewed marks after r: its path's digest.
func (r ViewedRow) Cursor() string { return r.PathDigest }

const (
	// MaxViewedPerMark is the files one gesture names: a press marks one, a directory a few dozen.
	MaxViewedPerMark = 32
	// MaxViewedFiles is the marks one person holds, past which their oldest go. The apply reads it,
	// so a new cap is a record format (a versioned field carrying it), never an edit here.
	MaxViewedFiles = 2_000
	// MaxViewedForget is the ended changes one record forgets; the next record forgets the rest.
	MaxViewedForget    = 64
	defaultViewedLimit = 500
	// MaxViewedPerPage is the most marks one page of a change's viewed files asks for.
	MaxViewedPerPage = 1_000
)

// ViewedKey is a repository's viewed key, 32 random bytes in lowercase hex: its persons' viewed marks
// keep each path and file state digested under it. No format verb or log line prints its value.
type ViewedKey string

// ViewedKeyBytes is a viewed key's length in bytes.
const ViewedKeyBytes = 32

// Valid reports a key of ViewedKeyBytes bytes in lowercase hex, the one form a key is minted in.
func (k ViewedKey) Valid() bool { return isLowerHex(string(k), 2*ViewedKeyBytes) }

// PathDigest is path as a viewed mark keeps it: HMAC-SHA256 of its bytes under k, in lowercase hex.
func (k ViewedKey) PathDigest(path string) string { return k.digest(path) }

// FileDigest is a file's state as a viewed mark keeps it: HMAC-SHA256 under k of the path, a NUL, the
// object id, a NUL and the mode, in lowercase hex; the id and the mode empty for a file the revision deletes.
func (k ViewedKey) FileDigest(path, oid, mode string) string {
	return k.digest(path + "\x00" + oid + "\x00" + mode)
}

// digest is HMAC-SHA256 of data under the bytes k names, in lowercase hex.
func (k ViewedKey) digest(data string) string {
	secret, _ := hex.DecodeString(string(k))
	mac := hmac.New(sha256.New, secret)
	_, _ = io.WriteString(mac, data)
	return hex.EncodeToString(mac.Sum(nil))
}

// String names the key without its value, so no format verb prints it.
func (ViewedKey) String() string { return "[viewed key]" }

// GoString is String, for %#v.
func (k ViewedKey) GoString() string { return k.String() }

// Format is String under every verb, %d and %x among them.
func (k ViewedKey) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, k.String()) }

// LogValue is String, so no log line prints the key either.
func (k ViewedKey) LogValue() slog.Value { return slog.StringValue(k.String()) }

// newViewedKey is a fresh key: ViewedKeyBytes from crypto/rand, which never fails.
func newViewedKey() ViewedKey {
	var raw [ViewedKeyBytes]byte
	_, _ = rand.Read(raw[:])
	return ViewedKey(hex.EncodeToString(raw[:]))
}

// isLowerHex reports s of n lowercase hex digits.
func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// viewedKeyIn is repoID's viewed key, false where none is recorded.
func viewedKeyIn(ctx context.Context, tx *sql.Tx, repoID string) (ViewedKey, bool, error) {
	var secret string
	err := tx.QueryRowContext(ctx, viewedKeySQL, repoID).Scan(&secret)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("gitlet: read repository %s's viewed key: %w", repoID, err)
	}
	if key := ViewedKey(secret); key.Valid() {
		return key, true, nil
	}
	return "", false, fmt.Errorf("gitlet: repository %s holds a viewed key that is not %d bytes in lowercase hex",
		repoID, ViewedKeyBytes)
}

// validate refuses a gesture no node could decide, naming the field.
func (g ViewedGesture) validate() error {
	if validRepoID("files.repo_id", g.RepoID) != nil {
		return fmt.Errorf("%w: files.repo_id: %q is not a repository's id", ErrInvalid, g.RepoID)
	}
	if bareUUIDv7("files.change_id", g.ChangeID) != nil {
		return fmt.Errorf("%w: files.change_id: %q is not a change's id", ErrInvalid, g.ChangeID)
	}
	if n := len(g.Viewed) + len(g.Unviewed); n < 1 || n > MaxViewedPerMark {
		return fmt.Errorf("%w: files: a gesture names 1 to %d files", ErrInvalid, MaxViewedPerMark)
	}
	named := make(map[string]bool, len(g.Viewed)+len(g.Unviewed))
	check := func(field, p string) error {
		if !utf8.ValidString(p) {
			return fmt.Errorf("%w: %s: not valid UTF-8", ErrInvalid, field)
		}
		if why := validGitPath(p, false); why != "" {
			return fmt.Errorf("%w: %s: %s", ErrInvalid, field, why)
		}
		if named[p] {
			return fmt.Errorf("%w: %s: %q is named twice; a gesture names each file once", ErrInvalid, field, p)
		}
		named[p] = true
		return nil
	}
	for i, f := range g.Viewed {
		field := fmt.Sprintf("files.viewed[%d]", i)
		if err := check(field+".path", f.Path); err != nil {
			return err
		}
		switch {
		case f.BlobOID != "" && (!gitref.ValidOID(f.BlobOID) || f.BlobOID == ZeroOID):
			return fmt.Errorf("%w: %s.blob_oid: %q is not a whole object id", ErrInvalid, field, f.BlobOID)
		case f.Mode != "" && !validMode(f.Mode):
			return fmt.Errorf("%w: %s.mode: %q is not a mode git gives a file, six octal digits", ErrInvalid,
				field, f.Mode)
		case (f.BlobOID == "") != (f.Mode == ""):
			return fmt.Errorf("%w: %s: a file is marked with its new side's object id and mode, or with "+
				"neither where the revision deletes it", ErrInvalid, field)
		}
	}
	for i, p := range g.Unviewed {
		if err := check(fmt.Sprintf("files.unviewed[%d]", i), p); err != nil {
			return err
		}
	}
	return nil
}

// validMode reports six octal digits other than 000000, as git's raw diff spells a file's mode.
func validMode(m string) bool {
	if len(m) != 6 || m == "000000" {
		return false
	}
	for i := range len(m) {
		if m[i] < '0' || m[i] > '7' {
			return false
		}
	}
	return true
}

// viewedDecision is what one round of a viewed mark's decide came to.
type viewedDecision struct {
	markDecision
	// keyless is a decide that found the repository's key unrecorded and appended nothing.
	keyless bool
}

// markViewed decides one gesture over a change's files: once, and where the repository holds no
// viewed key yet, again after the key step records one.
func (m *Marks) markViewed(ctx context.Context, w MarkWrite) (MarkResult, error) {
	res, keyless, err := m.publishViewed(ctx, w, w.Seen)
	if err != nil || !keyless {
		return res, err
	}
	floor, ended, done, err := m.recordViewedKey(ctx, w)
	if err != nil || done {
		return ended, err
	}
	seen := w.Seen
	if floor.Packed() > seen.Packed() {
		seen = floor
	}
	res, keyless, err = m.publishViewed(ctx, w, seen)
	if err == nil && keyless {
		return MarkResult{}, fmt.Errorf("gitlet: repository %s holds no viewed key at %s, past the key step of "+
			"mark %s", w.Files.RepoID, seen, w.OpID)
	}
	return res, err
}

// publishViewed publishes w's viewed record at the floor seen, answering what became of it, and
// true where its last decide found no key and appended nothing.
func (m *Marks) publishViewed(ctx context.Context, w MarkWrite, seen statelog.Position) (MarkResult, bool,
	error) {

	g := w.Files
	subject := PersonSubject(w.Handle)
	// Probe the repository too, so a node holding back a record about it declines; declare the marks
	// alone, so a build holding this record back holds back nothing of the repository.
	probe := []string{ViewedPath(w.Handle), RepoPath(g.RepoID)}
	scope := []string{ViewedPath(w.Handle)}
	actor := Actor{ID: w.Handle, Kind: ActorOperator, OperatorID: w.Asker.OperatorID}
	now := m.now()
	var last viewedDecision
	res, err := m.pub.Publish(ctx, statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: probe},
		OpID: w.OpID, Pattern: statelog.PatternArbitrated, Session: seen,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			last = viewedDecision{}
			p, decided, err := decideViewed(ctx, tx, w, &last)
			if err != nil || !decided {
				return statelog.Decision{}, err
			}
			return recordFor(stamp, subject, OpViewed, scope, w.OpID, actor, p, now)
		},
	})
	if err != nil {
		r, failed := viewedFailure(err, fmt.Sprintf("%s's viewed files kept changing for every round the mark "+
			"may take; the same mark again is the same write", w.Handle))
		if failed != nil {
			return MarkResult{}, false, fmt.Errorf("gitlet: mark operation %s: %w", w.OpID, failed)
		}
		return r, false, nil
	}
	switch res.Outcome {
	case statelog.OutcomeApplied:
		if !res.Position.IsZero() {
			return MarkResult{Outcome: statelog.OutcomeApplied, Position: res.Position}, false, nil
		}
		switch {
		case last.keyless:
			return MarkResult{}, true, nil
		case last.refusal != "":
			return MarkResult{Refusal: last.refusal, Detail: last.detail}, false, nil
		case last.unchanged:
			return MarkResult{Outcome: statelog.OutcomeApplied, Unchanged: true, Position: unpacked(last.position)},
				false, nil
		}
		return MarkResult{}, false, fmt.Errorf("gitlet: the publish of mark %s answered applied with no record, "+
			"and its decide neither refused it nor found it unchanged", w.OpID)
	case statelog.OutcomePending:
		return MarkResult{Outcome: statelog.OutcomePending, Position: res.Position}, false, nil
	}
	return MarkResult{Outcome: statelog.OutcomeUnknown}, false, nil
}

// decideViewed is one round of a viewed mark's decide in tx: the payload of the record to append
// and true, or false with last saying why nothing is.
func decideViewed(ctx context.Context, tx *sql.Tx, w MarkWrite, last *viewedDecision) (ViewedMarks, bool,
	error) {

	g := w.Files
	repo, found, err := pushRepoIn(ctx, tx, g.RepoID)
	if err != nil {
		return ViewedMarks{}, false, err
	}
	if !found || repo.status == RepoStatusDeleted || repo.status == RepoStatusPurging {
		last.refusal, last.detail = MarkEnded, "the change's repository is deleted, and its changes keep no "+
			"viewed files"
		return ViewedMarks{}, false, nil
	}
	row, found, err := changeRowIn(ctx, tx, g.ChangeID)
	if err != nil {
		return ViewedMarks{}, false, err
	}
	switch {
	case !found || row.repoID != g.RepoID:
		last.refusal, last.detail = MarkInvalid, fmt.Sprintf("repository %s holds no change %s", repo.name,
			g.ChangeID)
		return ViewedMarks{}, false, nil
	case !row.state.Live():
		last.refusal, last.detail = MarkEnded, fmt.Sprintf("%s is %s, and a change keeps viewed files only "+
			"while it is a draft or open", ChangeRef(repo.name, row.number), row.state)
		return ViewedMarks{}, false, nil
	}
	key, found, err := viewedKeyIn(ctx, tx, g.RepoID)
	if err != nil {
		return ViewedMarks{}, false, err
	}
	if !found {
		last.keyless = true
		return ViewedMarks{}, false, nil
	}
	out := ViewedMarks{Handle: w.Handle, ChangeID: g.ChangeID}
	digests := make([]string, 0, len(g.Viewed)+len(g.Unviewed))
	for _, f := range g.Viewed {
		mark := ViewedMark{PathDigest: key.PathDigest(f.Path), FileDigest: key.FileDigest(f.Path, f.BlobOID, f.Mode)}
		out.Viewed = append(out.Viewed, mark)
		digests = append(digests, mark.PathDigest)
	}
	for _, p := range g.Unviewed {
		out.Unviewed = append(out.Unviewed, key.PathDigest(p))
	}
	digests = append(digests, out.Unviewed...)
	held, newest, err := viewedNamedIn(ctx, tx, w.Handle, g.ChangeID, digests)
	if err != nil {
		return ViewedMarks{}, false, err
	}
	if out.Forget, err = viewedEndedIn(ctx, tx, w.Handle); err != nil {
		return ViewedMarks{}, false, err
	}
	unchanged := len(out.Forget) == 0
	for _, mark := range out.Viewed {
		unchanged = unchanged && held[mark.PathDigest] == mark.FileDigest
	}
	for _, d := range out.Unviewed {
		_, marked := held[d]
		unchanged = unchanged && !marked
	}
	if unchanged {
		last.unchanged, last.position = true, newest
		return ViewedMarks{}, false, nil
	}
	return out, true, nil
}

// viewedNamedIn is the file digest of each of digests handle's marks on change hold, and the newest
// position among them, zero where none holds one: one statement for the gesture.
func viewedNamedIn(ctx context.Context, tx *sql.Tx, handle, change string, digests []string) (map[string]string,
	int64, error) {

	list, err := json.Marshal(digests)
	if err != nil {
		return nil, 0, err
	}
	rows, err := tx.QueryContext(ctx, viewedNamedSQL, handle, change, string(list))
	if err != nil {
		return nil, 0, fmt.Errorf("gitlet: read the viewed marks a mark of %s names: %w", handle, err)
	}
	defer func() { _ = rows.Close() }()
	held := make(map[string]string, len(digests))
	var newest int64
	for rows.Next() {
		var path, file string
		var position int64
		if scanErr := rows.Scan(&path, &file, &position); scanErr != nil {
			return nil, 0, fmt.Errorf("gitlet: read the viewed marks a mark of %s names: %w", handle, scanErr)
		}
		held[path] = file
		newest = max(newest, position)
	}
	if err = rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("gitlet: read the viewed marks a mark of %s names: %w", handle, err)
	}
	return held, newest, nil
}

// viewedEndedIn is the changes handle's marks name whose row stands closed or merged, at most
// MaxViewedForget in id order; a change with no row is never among them.
func viewedEndedIn(ctx context.Context, tx *sql.Tx, handle string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, viewedEndedSQL, handle, MaxViewedForget)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read the ended changes %s's viewed marks name: %w", handle, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if scanErr := rows.Scan(&id); scanErr != nil {
			return nil, fmt.Errorf("gitlet: read the ended changes %s's viewed marks name: %w", handle, scanErr)
		}
		out = append(out, id)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("gitlet: read the ended changes %s's viewed marks name: %w", handle, err)
	}
	return out, nil
}

// recordViewedKey records the key w's repository's viewed marks are digested under, where none is:
// the position the mark waits for (zero where nothing was appended), and the mark's answer and
// true where the step ends it.
func (m *Marks) recordViewedKey(ctx context.Context, w MarkWrite) (statelog.Position, MarkResult, bool, error) {
	repoID := w.Files.RepoID
	key := newViewedKey()
	subject := RepoSubject(repoID)
	scope := []string{RepoPath(repoID)}
	opID := statelog.StepOpID(w.OpID, "viewed-key")
	actor := Actor{ID: m.nodeID, Kind: ActorSystem}
	now := m.now()
	res, err := m.pub.Publish(ctx, statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: scope},
		OpID: opID, Pattern: statelog.PatternArbitrated, Session: w.Seen,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			repo, found, err := pushRepoIn(ctx, tx, repoID)
			if err != nil || !found || !repo.status.live() {
				return statelog.Decision{}, err
			}
			_, keyed, err := viewedKeyIn(ctx, tx, repoID)
			if err != nil || keyed {
				return statelog.Decision{}, err
			}
			return recordFor(stamp, subject, OpViewedKey, scope, opID, actor,
				ViewedKeyRecord{RepoID: repoID, Key: key}, now)
		},
	})
	var refusal *statelog.Unavailable
	switch {
	case errors.As(err, &refusal) && refusal.Reason == statelog.ReasonDeleted:
		return statelog.Position{}, MarkResult{Refusal: MarkEnded, Detail: "the change's repository is deleted, " +
			"and its changes keep no viewed files"}, true, nil
	case err != nil:
		r, failed := viewedFailure(err, fmt.Sprintf("repository %s kept changing for every round its first viewed "+
			"mark may take; the same mark again is the same write", repoID))
		if failed != nil {
			return statelog.Position{}, MarkResult{}, true, fmt.Errorf("gitlet: the viewed key of mark %s: %w",
				w.OpID, failed)
		}
		return statelog.Position{}, r, true, nil
	case res.Outcome == statelog.OutcomeApplied, res.Outcome == statelog.OutcomePending:
		return res.Position, MarkResult{}, false, nil
	}
	return statelog.Position{}, MarkResult{Outcome: statelog.OutcomeUnknown}, true, nil
}

// viewedFailure is a viewed publish's error as its answer, busy worded as given; an error it answers
// none for is returned to be wrapped.
func viewedFailure(err error, busy string) (MarkResult, error) {
	var refusal *statelog.Unavailable
	switch {
	case errors.Is(err, ErrInvalidProposal):
		return MarkResult{}, err
	case errors.Is(err, statelog.ErrConflict):
		return MarkResult{Refusal: MarkBusy, Detail: busy}, nil
	case errors.As(err, &refusal) && nodeRefusals[refusal.Reason]:
		return MarkResult{}, fmt.Errorf("%w: %w", ErrNotServing, refusal)
	case errors.As(err, &refusal):
		return MarkResult{Refusal: MarkUnavailable, Detail: "Gitlet could not record the mark just now, and " +
			"nothing was written"}, nil
	}
	return MarkResult{}, err
}

// changeViewed serves a page of the asker's viewed marks on change row, in path digest order after
// the digest a cursor names, with the repository's key beside a page that holds one.
func (r *Reader) changeViewed(ctx context.Context, tx *sql.Tx, q ReadQuery, repoID, named string, row changeRow,
	out *ReadAnswer) error {

	out.Viewed = []ViewedRow{}
	if q.Cursor != "" && !isLowerHex(q.Cursor, 2*sha256.Size) {
		return fmt.Errorf("%w: %q is not a viewed file's digest", ErrBadCursor, q.Cursor)
	}
	handle, kind := q.Asker.party()
	if kind != PartyPerson || !row.state.Live() {
		return nil
	}
	limit := min(limitOr(q.Limit, defaultViewedLimit), r.pageRows)
	query, args := viewedPageSQL, []any{handle, row.id, limit}
	if q.Cursor != "" {
		query, args = viewedPageAfterSQL, []any{handle, row.id, q.Cursor, limit}
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("gitlet: read %s's viewed files of %s: %w", handle, named, err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: limit, maxB: r.pageBytes}
	for rows.Next() {
		var (
			v  ViewedRow
			at int64
		)
		if scanErr := rows.Scan(&v.PathDigest, &v.FileDigest, &at); scanErr != nil {
			return fmt.Errorf("gitlet: read %s's viewed files of %s: %w", handle, named, scanErr)
		}
		v.At = store.DecodeTime(at)
		out.Viewed = append(out.Viewed, v)
		if pg.add(v.encodedBytes()) {
			out.Next = v.Cursor()
			break
		}
	}
	if err = rows.Err(); err != nil || len(out.Viewed) == 0 {
		return err
	}
	key, found, err := viewedKeyIn(ctx, tx, repoID)
	switch {
	case err != nil:
		return err
	case !found:
		return fmt.Errorf("gitlet: %s holds viewed marks on %s and repository %s holds no viewed key", handle,
			named, repoID)
	}
	out.ViewedKey = key
	return nil
}

// The viewed marks' decides' and read's statements, each registered in plan_test.go.
const (
	viewedKeySQL   = `SELECT secret FROM code_viewed_keys WHERE repo_id = ?`
	viewedNamedSQL = `SELECT v.path_digest, v.file_digest, v.position FROM json_each(?3) AS j
		CROSS JOIN code_viewed_files AS v INDEXED BY sqlite_autoindex_code_viewed_files_1
		WHERE v.handle = ?1 AND v.change_id = ?2 AND v.path_digest = CAST(j.value AS TEXT)`
	viewedEndedSQL = `SELECT g.change_id FROM (SELECT change_id FROM code_viewed_files
		WHERE handle = ?1 GROUP BY change_id) AS g CROSS JOIN code_changes AS c ON c.id = g.change_id
		WHERE c.state NOT IN ('draft', 'open') ORDER BY g.change_id LIMIT ?2`
	viewedPageSQL = `SELECT path_digest, file_digest, at FROM code_viewed_files
		WHERE handle = ?1 AND change_id = ?2 ORDER BY path_digest LIMIT ?3`
	viewedPageAfterSQL = `SELECT path_digest, file_digest, at FROM code_viewed_files
		WHERE handle = ?1 AND change_id = ?2 AND path_digest > ?3 ORDER BY path_digest LIMIT ?4`
)
