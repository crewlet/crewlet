package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A TARGET'S QUEUE, ITS LANDINGS AND ITS GREEN, READ — four kinds of read over
// the rows a repository's queue records write (queue.go):
//
//   - `queue` ([ReadQueue]) lists the LIVE entries of every queue — of one
//     repository, of one of its targets, or of every repository the asker
//     reads — in queue order, each with its place, beside the trains testing a
//     named repository's (code_queue_live_idx, code_candidates_train_idx),
//     held to the asker's grant as a listing of changes is.
//   - `queue_due` ([ReadQueueDue]) is the home's: the ACTIVE repositories
//     holding a live entry, by id — an archived or deleted one's queue is
//     nobody's work until it is active again — or one repository's live
//     entries in queue order: the engine's own read, which no principal makes.
//   - `landings` ([ReadLandings]) is one target's landings ABOVE ITS GREEN, in
//     the order they landed, beside the green itself: what its tip checks have
//     not yet judged.
//   - `green` ([ReadGreen]) is one repository's green rows: each protected
//     target's newest commit whose required checks passed.
//
// EVERY LISTING IS A KEYSET over its index's own order, its cursor the last
// row's place in it.

// QueueRow is one live entry of a target's queue as a read answers it.
type QueueRow struct {
	RepoID    string `json:"repo_id"`
	Repo      string `json:"repo"`
	EntryID   string `json:"entry_id"`
	TargetRef string `json:"target_ref"`
	ChangeID  string `json:"change_id"`
	// Number is its change's number on its repository (web!42's 42).
	Number int64 `json:"number"`
	// Place is its place in its target's queue, from 1; Seq the position of
	// the record that enqueued it.
	Place        int               `json:"place"`
	Seq          statelog.Position `json:"seq"`
	HeadOID      string            `json:"head_oid"`
	Method       MergeMethod       `json:"method"`
	DeleteSource bool              `json:"delete_source,omitempty"`
	CloseTask    bool              `json:"close_task,omitempty"`
	AutoRevert   bool              `json:"auto_revert,omitempty"`
	// EnqueuedBy is who enqueued it, and EnqueuedOperator the credential a
	// person acted through.
	EnqueuedBy       Party      `json:"enqueued_by"`
	EnqueuedOperator string     `json:"enqueued_operator,omitempty"`
	State            QueueState `json:"state"`
	// Train, CandidateID and CommitOID are the train and the candidate
	// testing it and the commit the candidate built of it — each "" until a
	// candidate takes it.
	Train       string    `json:"train,omitempty"`
	CandidateID string    `json:"candidate_id,omitempty"`
	CommitOID   string    `json:"commit_oid,omitempty"`
	EnqueuedAt  time.Time `json:"enqueued_at"`
}

// CandidateEntry is one entry a candidate merges, and the commit it became.
type CandidateEntry struct {
	EntryID string `json:"entry_id"`
	Commit  string `json:"commit"`
}

// CandidateRow is one candidate a pre_land queue built: its train's ordinal'th
// merge of its entries onto Base, Tip its result, its entries and their
// commits, the pack its objects are stored in, where it stands, and the
// record that wrote it.
type CandidateRow struct {
	RepoID      string            `json:"repo_id"`
	CandidateID string            `json:"candidate_id"`
	TargetRef   string            `json:"target_ref"`
	Train       string            `json:"train"`
	Ordinal     int               `json:"ordinal"`
	BaseOID     string            `json:"base_oid"`
	TipOID      string            `json:"tip_oid"`
	Entries     []CandidateEntry  `json:"entries"`
	PackID      string            `json:"pack_id,omitempty"`
	State       string            `json:"state"`
	Position    statelog.Position `json:"position"`
}

// LandingRow is one landing on a protected target: the record that landed it
// and its place in that record, what kind of landing it was, what landed —
// the change and its entry, "" for a push that landed none — the target before
// and after, who, and what the target's tip checks found of it.
type LandingRow struct {
	RepoID     string            `json:"repo_id"`
	TargetRef  string            `json:"target_ref"`
	Position   statelog.Position `json:"position"`
	Ordinal    int               `json:"ordinal"`
	Kind       LandingKind       `json:"kind"`
	ChangeID   string            `json:"change_id,omitempty"`
	EntryID    string            `json:"entry_id,omitempty"`
	BaseOID    string            `json:"base_oid"`
	CommitOID  string            `json:"commit_oid"`
	EnqueuedBy string            `json:"enqueued_by,omitempty"`
	MergedBy   string            `json:"merged_by,omitempty"`
	Green      bool              `json:"green,omitempty"`
	Broken     bool              `json:"broken,omitempty"`
	At         time.Time         `json:"at"`
}

// GreenRow is one protected target's newest commit whose required checks
// passed, and the record that said so.
type GreenRow struct {
	RepoID    string            `json:"repo_id"`
	TargetRef string            `json:"target_ref"`
	CommitOID string            `json:"commit_oid"`
	Position  statelog.Position `json:"position"`
	At        time.Time         `json:"at"`
}

// QueueDueQuery is a `queue_due` read's repository, by id: "" lists the
// repositories holding a live entry, and one names the repository whose live
// entries its home reads.
type QueueDueQuery struct {
	RepoID string `json:"repo_id,omitempty"`
}

// The pages of the queue's reads: a queue's entries a listing of changes'
// bounds (MaxChangesPerPage), a landing's row narrow — a target's landings
// since its green a page of them.
const (
	defaultQueueLimit   = 50
	MaxQueueRowsPerPage = 200
	defaultLandingLimit = 200
	MaxLandingsPerPage  = 1_000
)

// validateQueueRead is [ReadQuery.validate] for a `queue`, a `landings` and a
// `green` read, after the rules every principal's read meets: a target only
// beside the repository it is of, a branch a change may name — required for
// landings — and nothing a queue read does not read.
func (q ReadQuery) validateQueueRead() error {
	refused := func(field, why string) error { return fmt.Errorf("%w: %s: %s", ErrInvalid, field, why) }
	switch {
	case q.Kind != ReadQueue && q.Repo == "":
		return refused("repo", fmt.Sprintf("a %s read names the repository it is of", q.Kind))
	case q.Kind == ReadLandings && q.Target == "":
		return refused("target", "a landings read names the protected branch whose landings it lists")
	case q.Target != "" && q.Repo == "":
		return refused("target", "a target is a branch of the repository a read names")
	case q.Target != "" && branchProblem(q.Target) != "":
		return refused("target", branchProblem(q.Target))
	case q.Kind == ReadGreen && (q.Cursor != "" || q.Limit != 0):
		return refused("cursor", "a repository's green rows are one answer, never paged")
	}
	return nil
}

// validateQueueDue is [ReadQuery.validate] for a `queue_due` read: the
// engine's own, so it names NO asker and no seat it reads for, for the
// `compaction_due` read's reason; its own query, REQUIRED, its repository,
// where it names one, by id in its canonical spelling; and nothing else a
// listing names.
func (q ReadQuery) validateQueueDue() error {
	refused := func(field, why string) error { return fmt.Errorf("%w: %s: %s", ErrInvalid, field, why) }
	switch d := q.QueueDue; {
	case q.Asker != (Principal{}):
		return refused("asker", "a queue_due read is the engine's own, which no principal makes")
	case q.CeilingSeat != "":
		return refused("ceiling_seat", "a queue_due read is the engine's own, which no seat makes for another")
	case d == nil:
		return refused("queue_due", "a queue_due read names its query: the repositories holding a live entry, "+
			"or one repository's by its id")
	case d.RepoID != "" && !canonicalUUID(d.RepoID):
		return refused("queue_due.repo_id", fmt.Sprintf("%q is not a repository's id in its canonical "+
			"lowercase spelling", d.RepoID))
	}
	for _, f := range []struct {
		field string
		set   bool
	}{
		{"repo", q.Repo != ""}, {"status", len(q.Status) > 0}, {"prefix", q.Prefix != ""},
		{"introduced", q.Introduced != nil}, {"run_pushes", q.RunPushes != nil}, {"min_packs", q.MinPacks != 0},
		{"limit", q.Limit != 0}, {"brief", q.Brief}, {"push", q.Push != ""}, {"change", q.Change != 0},
		{"state", q.State != ""}, {"owner", q.Owner != ""}, {"work", q.Work != ""},
		{"reviewer", q.Reviewer != ""}, {"needs", q.Needs != ""}, {"part", q.Part != ""},
		{"changes_due", q.ChangesDue != nil}, {"checks", q.Checks != nil}, {"checks_due", q.ChecksDue != nil},
		{"check_box", q.CheckBox != nil}, {"path_owners", q.PathOwners}, {"gates", q.Gates},
		{"expunge_tips", q.ExpungeTips != nil}, {"target", q.Target != ""}, {"checks_running", q.ChecksRunning != nil},
	} {
		if f.set {
			return refused(f.field, "a queue_due read lists the repositories holding a live entry, or one "+
				"repository's live entries by its id, a page of MaxSnapshotItems at a time, and names nothing else")
		}
	}
	return nil
}

// queueQuery is the framework read a queue read resolves to: a SET READ of the
// repository it names, resolved by its name as a push history is, or of the
// domain — the home's due listing of repositories, or a listing across
// repositories — and of the one repository a `queue_due` read names by id.
func (q ReadQuery) queueQuery(fresh statelog.Freshness) statelog.Query {
	switch {
	case q.Kind == ReadQueueDue && q.QueueDue.RepoID != "":
		return fresh.Query(statelog.ScopeSet{Paths: []string{RepoPath(q.QueueDue.RepoID)}}, true)
	case q.Kind == ReadQueueDue, q.Repo == "":
		return fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
	}
	return fresh.Resolved(resolver(q.Repo), true)
}

// queueCursor is the place of an entry in the listing's order — its
// repository's id, its target and its seq — spelled with spaces, which
// neither an id nor a branch holds.
func queueCursor(e queueEntry) string {
	return e.RepoID + " " + e.TargetRef + " " + strconv.FormatInt(e.Seq, 10)
}

// cutQueueCursor reads a [queueCursor].
func cutQueueCursor(s string) (repoID, target string, seq int64, ok bool) {
	parts := strings.Split(s, " ")
	if len(parts) != 3 {
		return "", "", 0, false
	}
	seq, err := strconv.ParseInt(parts[2], 10, 64)
	return parts[0], parts[1], seq, err == nil && seq > 0 && parts[0] != "" && parts[1] != ""
}

// queue serves one page of a `queue` read: the live entries of the repository
// q names — of its target, where it names one — or, naming none, of every
// repository the asker reads, in queue order; each with its place and its
// change's number; and, for a named repository, the candidates of every train
// of the targets the page reaches.
func (r *Reader) queueListing(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, out *ReadAnswer) error {
	limit := min(limitOr(q.Limit, defaultQueueLimit), r.pageRows)
	repos := map[string]listedRepo{}
	var (
		stmt string
		args []any
	)
	cursorRepo, cursorTarget, cursorSeq, resumed := "", "", int64(0), q.Cursor != ""
	if resumed {
		var ok bool
		if cursorRepo, cursorTarget, cursorSeq, ok = cutQueueCursor(q.Cursor); !ok {
			return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
		}
	}
	switch {
	case q.Repo != "" && q.Target != "":
		named, err := readableIn(ctx, tx, q.Repo, q.Asker, q.CeilingSeat, chart)
		if err != nil {
			return err
		}
		repos[named.ref.ID] = named
		stmt, args = queueOfTargetSQL, []any{named.ref.ID, q.Target, cursorSeq, limit}
	case q.Repo != "":
		named, err := readableIn(ctx, tx, q.Repo, q.Asker, q.CeilingSeat, chart)
		if err != nil {
			return err
		}
		repos[named.ref.ID] = named
		stmt, args = queueOfRepoSQL, []any{named.ref.ID, limit}
		if resumed {
			stmt, args = queueOfRepoAfterSQL, []any{named.ref.ID, cursorTarget, cursorTarget, cursorTarget,
				cursorSeq, limit}
		}
	default:
		// EVERY REPOSITORY'S, from the first repository id up: a seek of
		// the index's range, never a walk of the index whole.
		stmt, args = queueAllSQL, []any{"", limit}
		if resumed {
			stmt, args = queueAllAfterSQL, []any{cursorRepo, cursorRepo, cursorTarget, cursorTarget, cursorSeq,
				limit}
		}
	}
	scanned, err := entriesBy(ctx, tx, stmt, args...)
	if err != nil {
		return err
	}
	if q.Repo == "" {
		ids := make([]string, len(scanned))
		for i, e := range scanned {
			ids[i] = e.RepoID
		}
		if repos, err = listedReposAmong(ctx, tx, ids, q.Asker, q.CeilingSeat, chart, "a page of queues"); err != nil {
			return err
		}
	}
	numbers, err := numbersOf(ctx, tx, scanned)
	if err != nil {
		return err
	}
	var (
		group [2]string
		place int
	)
	for _, e := range scanned {
		repo, listed := repos[e.RepoID]
		if !listed {
			continue
		}
		if key := [2]string{e.RepoID, e.TargetRef}; key != group {
			group = key
			if place, err = placeIn(ctx, tx, e); err != nil {
				return err
			}
		} else {
			place++
		}
		out.Queue = append(out.Queue, e.row(repo.ref.Name, numbers[e.ChangeID], place))
	}
	if len(scanned) == limit {
		out.Next = queueCursor(scanned[len(scanned)-1])
	}
	if q.Repo == "" {
		return nil
	}
	// THE TRAINS OF THE TARGETS THE PAGE REACHED, a named repository's: a
	// target's candidates are its live trains' alone, since a train that ends
	// removes them.
	var targets []string
	for _, row := range out.Queue {
		targets = appendOnce(targets, row.TargetRef)
	}
	for _, target := range targets {
		trains, err := trainsOf(ctx, tx, out.Queue[0].RepoID, target)
		if err != nil {
			return err
		}
		out.Trains = append(out.Trains, trains...)
	}
	return nil
}

// queueDue serves one page of a `queue_due` read: with no repository, the
// repositories holding a live entry, by id, after the cursor; with one, its
// live entries in queue order, after the cursor — each page MaxSnapshotItems
// rows at most.
func (r *Reader) queueDue(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	if q.QueueDue.RepoID == "" {
		rows, err := tx.QueryContext(ctx, queueDueReposSQL, q.Cursor, r.pageRows)
		if err != nil {
			return fmt.Errorf("code: list the repositories holding a queue: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return fmt.Errorf("code: list the repositories holding a queue: %w", err)
			}
			out.DueRepos = append(out.DueRepos, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(out.DueRepos) == r.pageRows {
			out.Next = out.DueRepos[len(out.DueRepos)-1]
		}
		return nil
	}
	stmt, args := queueOfRepoSQL, []any{q.QueueDue.RepoID, r.pageRows}
	if q.Cursor != "" {
		_, target, seq, ok := cutQueueCursor(q.Cursor)
		if !ok {
			return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
		}
		stmt, args = queueOfRepoAfterSQL, []any{q.QueueDue.RepoID, target, target, target, seq, r.pageRows}
	}
	scanned, err := entriesBy(ctx, tx, stmt, args...)
	if err != nil {
		return err
	}
	numbers, err := numbersOf(ctx, tx, scanned)
	if err != nil {
		return err
	}
	var (
		group string
		place int
	)
	for _, e := range scanned {
		if e.TargetRef != group {
			group = e.TargetRef
			if place, err = placeIn(ctx, tx, e); err != nil {
				return err
			}
		} else {
			place++
		}
		out.Queue = append(out.Queue, e.row("", numbers[e.ChangeID], place))
	}
	if len(scanned) == r.pageRows {
		out.Next = queueCursor(scanned[len(scanned)-1])
	}
	return nil
}

// landings serves one page of a `landings` read: the target's green, where it
// has one, and its landings above it, in the order they landed, after the
// cursor — a landing's position and its ordinal in its record.
func (r *Reader) landings(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, out *ReadAnswer) error {
	named, err := readableIn(ctx, tx, q.Repo, q.Asker, q.CeilingSeat, chart)
	if err != nil {
		return err
	}
	green, found, err := greenIn(ctx, tx, named.ref.ID, q.Target)
	if err != nil {
		return err
	}
	// ABOVE ITS GREEN: every landing of a record after the one its green row
	// names — a record's every landing is green with its tip — so the first
	// page starts past that record's every ordinal.
	from, ordinal := int64(0), -1
	if found {
		out.Green = []GreenRow{green}
		from, ordinal = green.Position.Packed(), math.MaxInt32
	}
	if q.Cursor != "" {
		at, n, ok := strings.Cut(q.Cursor, "/")
		position, perr := strconv.ParseInt(at, 10, 64)
		o, oerr := strconv.Atoi(n)
		if !ok || perr != nil || oerr != nil || position < from {
			return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
		}
		from, ordinal = position, o
	}
	limit := min(limitOr(q.Limit, defaultLandingLimit), r.pageRows)
	rows, err := tx.QueryContext(ctx, landingsSQL, named.ref.ID, q.Target, from, from, ordinal, limit)
	if err != nil {
		return fmt.Errorf("code: read %s's landings on %s: %w", named.ref.Name, q.Target, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			l         LandingRow
			packed, t int64
		)
		if err := rows.Scan(&packed, &l.Ordinal, &l.Kind, &l.ChangeID, &l.EntryID, &l.BaseOID, &l.CommitOID,
			&l.EnqueuedBy, &l.MergedBy, &l.Green, &l.Broken, &t); err != nil {
			return fmt.Errorf("code: read %s's landings on %s: %w", named.ref.Name, q.Target, err)
		}
		l.RepoID, l.TargetRef, l.Position, l.At = named.ref.ID, q.Target, unpacked(packed), store.DecodeTime(t)
		out.Landings = append(out.Landings, l)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if n := len(out.Landings); n == limit {
		last := out.Landings[n-1]
		out.Next = strconv.FormatInt(last.Position.Packed(), 10) + "/" + strconv.Itoa(last.Ordinal)
	}
	return nil
}

// greens serves a `green` read: the repository's green rows, one each
// protected target that has one — of the one target q names, where it names
// one.
func (r *Reader) greens(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, out *ReadAnswer) error {
	named, err := readableIn(ctx, tx, q.Repo, q.Asker, q.CeilingSeat, chart)
	if err != nil {
		return err
	}
	if q.Target != "" {
		green, found, err := greenIn(ctx, tx, named.ref.ID, q.Target)
		if err != nil || !found {
			return err
		}
		out.Green = []GreenRow{green}
		return nil
	}
	rows, err := tx.QueryContext(ctx, greensSQL, named.ref.ID)
	if err != nil {
		return fmt.Errorf("code: read %s's green rows: %w", named.ref.Name, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			g         GreenRow
			packed, t int64
		)
		if err := rows.Scan(&g.TargetRef, &g.CommitOID, &packed, &t); err != nil {
			return fmt.Errorf("code: read %s's green rows: %w", named.ref.Name, err)
		}
		g.RepoID, g.Position, g.At = named.ref.ID, unpacked(packed), store.DecodeTime(t)
		out.Green = append(out.Green, g)
	}
	return rows.Err()
}

// greenIn is target's green row on repoID, and false where it has none.
func greenIn(ctx context.Context, tx *sql.Tx, repoID, target string) (GreenRow, bool, error) {
	var (
		g         = GreenRow{RepoID: repoID, TargetRef: target}
		packed, t int64
	)
	err := tx.QueryRowContext(ctx, greenSQL, repoID, target).Scan(&g.CommitOID, &packed, &t)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return GreenRow{}, false, nil
	case err != nil:
		return GreenRow{}, false, fmt.Errorf("code: read %s's green row: %w", target, err)
	}
	g.Position, g.At = unpacked(packed), store.DecodeTime(t)
	return g, true, nil
}

// entriesBy reads the entries query answers for args.
func entriesBy(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]queueEntry, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("code: read a queue's entries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []queueEntry
	for rows.Next() {
		e, err := scanEntry(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("code: read a queue's entries: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// numbersOf is the number of each change entries name, by id — one statement
// for each repository the entries reach, over the list of its changes' ids
// ([changeNumbersIn]).
func numbersOf(ctx context.Context, tx *sql.Tx, entries []queueEntry) (map[string]int64, error) {
	var repos []string
	byRepo := map[string][]string{}
	for _, e := range entries {
		if _, seen := byRepo[e.RepoID]; !seen {
			repos = append(repos, e.RepoID)
		}
		byRepo[e.RepoID] = appendOnce(byRepo[e.RepoID], e.ChangeID)
	}
	out := map[string]int64{}
	for _, repo := range repos {
		numbers, err := changeNumbersIn(ctx, tx, repo, byRepo[repo])
		if err != nil {
			return nil, err
		}
		maps.Copy(out, numbers)
	}
	return out, nil
}

// trainsOf is every candidate of target's live trains on repoID, in train and
// ordinal order — one seek of code_candidates_train_idx.
func trainsOf(ctx context.Context, tx *sql.Tx, repoID, target string) ([]CandidateRow, error) {
	rows, err := tx.QueryContext(ctx, trainsSQL, repoID, target)
	if err != nil {
		return nil, fmt.Errorf("code: read %s's trains: %w", target, err)
	}
	defer func() { _ = rows.Close() }()
	var out []CandidateRow
	for rows.Next() {
		var (
			c       = CandidateRow{RepoID: repoID, TargetRef: target}
			packed  int64
			entries string
		)
		if err := rows.Scan(&c.CandidateID, &c.Train, &c.Ordinal, &c.BaseOID, &c.TipOID, &c.PackID, &c.State,
			&packed, &entries); err != nil {
			return nil, fmt.Errorf("code: read %s's trains: %w", target, err)
		}
		if err := json.Unmarshal([]byte(entries), &c.Entries); err != nil {
			return nil, fmt.Errorf("code: read candidate %s's entries: %w", c.CandidateID, err)
		}
		c.Position = unpacked(packed)
		out = append(out, c)
	}
	return out, rows.Err()
}

// row is e as a queue read answers it, of repository repo, its change's
// number and its place.
func (e queueEntry) row(repo string, number int64, place int) QueueRow {
	return QueueRow{RepoID: e.RepoID, Repo: repo, EntryID: e.EntryID, TargetRef: e.TargetRef, ChangeID: e.ChangeID,
		Number: number, Place: place, Seq: unpacked(e.Seq), HeadOID: e.HeadOID, Method: e.Method,
		DeleteSource: e.DeleteSource, CloseTask: e.CloseTask, AutoRevert: e.AutoRevert, EnqueuedBy: e.EnqueuedBy,
		EnqueuedOperator: e.EnqueuedOperator, State: e.State, Train: e.Train, CandidateID: e.CandidateID,
		CommitOID: e.CommitOID, EnqueuedAt: store.DecodeTime(e.EnqueuedAt)}
}

// The queue's reads, each registered in plan_test.go so the pinned driver's
// planner is held to the index each is written for, and to reading in its
// order: a target's live entries after a seq; a repository's in (target,
// seq) order, from the top and after a place, with the >= beside the OR that
// keeps the range seek (planner.go); every repository's the same way; the
// repositories holding one, grouped on the index's leading column; a target's
// trains by code_candidates_train_idx; its landings above a place by their
// key; and its green row, and a repository's, by theirs. Every read of
// code_queue's live entries repeats the index's partial predicate verbatim.
// The numbers of a page's changes are [changeNumbersIn]'s, registered as the
// committer's.
const (
	queueOfTargetSQL = `SELECT ` + entryColumns + ` FROM code_queue INDEXED BY code_queue_live_idx
		WHERE repo_id = ? AND target_ref = ? AND state IN ('waiting', 'testing') AND seq > ?
		ORDER BY seq LIMIT ?`
	queueOfRepoSQL = `SELECT ` + entryColumns + ` FROM code_queue INDEXED BY code_queue_live_idx
		WHERE repo_id = ? AND state IN ('waiting', 'testing') ORDER BY target_ref, seq LIMIT ?`
	queueOfRepoAfterSQL = `SELECT ` + entryColumns + ` FROM code_queue INDEXED BY code_queue_live_idx
		WHERE repo_id = ? AND state IN ('waiting', 'testing') AND target_ref >= ?
			AND (target_ref > ? OR (target_ref = ? AND seq > ?))
		ORDER BY target_ref, seq LIMIT ?`
	queueAllSQL = `SELECT ` + entryColumns + ` FROM code_queue INDEXED BY code_queue_live_idx
		WHERE state IN ('waiting', 'testing') AND repo_id >= ? ORDER BY repo_id, target_ref, seq LIMIT ?`
	queueAllAfterSQL = `SELECT ` + entryColumns + ` FROM code_queue INDEXED BY code_queue_live_idx
		WHERE state IN ('waiting', 'testing') AND repo_id >= ?
			AND (repo_id > ? OR target_ref > ? OR (target_ref = ? AND seq > ?))
		ORDER BY repo_id, target_ref, seq LIMIT ?`
	queueDueReposSQL = `SELECT q.repo_id FROM code_queue AS q INDEXED BY code_queue_live_idx
		WHERE q.state IN ('waiting', 'testing') AND q.repo_id > ?1
			AND EXISTS (SELECT 1 FROM code_repos AS r WHERE r.id = q.repo_id AND r.status = 'active')
		GROUP BY q.repo_id ORDER BY q.repo_id LIMIT ?2`
	trainsSQL = `SELECT candidate_id, train, ordinal, base_oid, tip_oid, pack_id, state, position, entries
		FROM code_candidates INDEXED BY code_candidates_train_idx WHERE repo_id = ? AND target_ref = ?
		ORDER BY train, ordinal`
	landingsSQL = `SELECT position, ordinal, kind, change_id, entry_id, base_oid, commit_oid, enqueued_by,
			merged_by, green, broken, at
		FROM code_landings WHERE repo_id = ? AND target_ref = ? AND position >= ?
			AND (position > ? OR ordinal > ?)
		ORDER BY position, ordinal LIMIT ?`
	greenSQL  = `SELECT commit_oid, position, at FROM code_green WHERE repo_id = ? AND target_ref = ?`
	greensSQL = `SELECT target_ref, commit_oid, position, at FROM code_green WHERE repo_id = ? ORDER BY target_ref`
)
