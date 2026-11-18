package gitlet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A TARGET'S QUEUE, ITS LANDINGS AND ITS GREEN, read over the rows queue.go writes:
//   - `queue` ([ReadQueue]): the live entries of a repository, a target or every
//     repository the asker reads, in queue order, held to the asker's grant as a
//     listing of changes is, beside a named repository's (or, where
//     [ReadQuery.Trains] asks, every one's) trains and recent ejections.
//   - `landings` ([ReadLandings]): a target's landings above its green; `green`
//     ([ReadGreen]): each protected target's newest commit whose checks passed.
//   - the due queue ([Reader.QueueDue]): the engine's own read, which no principal
//     makes and never a [ReadQuery] kind, because its home carries what it reads
//     into its next records, so its answer is read whole or refused ([QueueDueAnswer]).
//
// Every listing is a keyset over its index's order, its cursor the last row's place.

// QueueRow is one live entry of a target's queue as a read answers it.
type QueueRow struct {
	RepoID    string `json:"repo_id"`
	Repo      string `json:"repo"`
	EntryID   string `json:"entry_id"`
	TargetRef string `json:"target_ref"`
	ChangeID  string `json:"change_id"`
	// Number is its change's number on its repository (web!42's 42).
	Number int64 `json:"number"`
	// Title and SourceRef are its change's title and source branch, read with its number.
	Title     string `json:"title,omitempty" untrusted:"name"`
	SourceRef string `json:"source_ref,omitempty"`
	// Place is its place in its target's queue, from 1; Seq the position of
	// the record that enqueued it.
	Place        int               `json:"place"`
	Seq          statelog.Position `json:"seq"`
	HeadOID      string            `json:"head_oid"`
	Method       MergeMethod       `json:"method"`
	DeleteSource bool              `json:"delete_source,omitempty"`
	CloseTask    bool              `json:"close_task,omitempty"`
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

// CandidateRow is one candidate a queue built: its train's ordinal'th
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

// EjectedRow is one entry a queue put out, answered by a `queue` read of its
// repository beside the live ones: why ([QueueEndReason]), what a person reads
// first (the required checks that failed, or the paths its merge conflicted on),
// when, and the train its checks failed on.
//
// An answer's row, not a record's, so it takes no versionedFields row: every
// build that answers a `queue` read answers its ejections ([RecordVersion]).
type EjectedRow struct {
	RepoID    string `json:"repo_id"`
	Repo      string `json:"repo"`
	EntryID   string `json:"entry_id"`
	TargetRef string `json:"target_ref"`
	ChangeID  string `json:"change_id"`
	// Number is its change's number on its repository (web!42's 42).
	Number int64 `json:"number"`
	// Title and SourceRef are its change's title and source branch, read with its number.
	Title      string `json:"title,omitempty" untrusted:"name"`
	SourceRef  string `json:"source_ref,omitempty"`
	HeadOID    string `json:"head_oid"`
	EnqueuedBy Party  `json:"enqueued_by"`
	// Method, DeleteSource and CloseTask are how the entry was to land, what a re-queue
	// sends again; a row with no Method is an older build's, which answers none of the three.
	Method       MergeMethod    `json:"method,omitempty"`
	DeleteSource bool           `json:"delete_source,omitempty"`
	CloseTask    bool           `json:"close_task,omitempty"`
	Reason       QueueEndReason `json:"reason"`
	Checks       []string       `json:"checks,omitempty"`
	Paths        []string       `json:"paths,omitempty"`
	EnqueuedAt   time.Time      `json:"enqueued_at"`
	EjectedAt    time.Time      `json:"ejected_at"`
	// TestedIn is the train its checks failed on, where its ejection names one ([Ejection.TestedIn]):
	// its run is `check` at CommitOID for TargetRef. Absent before [TestedInVersion].
	TestedIn *EjectionTrain `json:"tested_in,omitempty"`
}

// QueueEjectionsShown is how far back a `queue` read answers a repository's
// ejections: a day, covering a reader who looks once a working day. The
// ejection's own wake (`dequeued`, `checks_failed`) reaches whoever must act;
// older ones are the change's history, on its page.
const QueueEjectionsShown = 24 * time.Hour

// MaxEjectionsShown is the most ejections one `queue` read answers, newest
// first: the queue listing's own page (defaultQueueLimit). A queue that ejects
// more in a day is broken, not busy, and its newest fifty say why.
const MaxEjectionsShown = defaultQueueLimit

// EjectedTotal is how many entries the asker's queues put out in the last
// [QueueEjectionsShown]; More where the count stopped at [MaxQueueCounted], so Total is a floor
// and a read of every repository answers rows of the counted ejections alone.
type EjectedTotal struct {
	Total int64 `json:"total"`
	More  bool  `json:"more,omitempty"`
}

// MaxQueueCounted is the most index entries one count of a queue read or a counts read's
// queue part walks: MaxSnapshotItems, met only by a fleet in trouble. A queue page stays
// under MaxPageBytes: 200 rows, 200 targets' trains of MaxQueueDepth and 50 ejections, about 2.6 MiB.
const MaxQueueCounted = MaxSnapshotItems

// ejections files into out what repo's queues (its target's, where target names
// one) put out in the last [QueueEjectionsShown], newest first, at most
// [MaxEjectionsShown], and where total asks their count within [MaxQueueCounted].
// It seeks an index partial on the ejections alone (replicated 0132), never one
// of every ended entry: a healthy queue lands thousands a day.
func (r *Reader) ejections(ctx context.Context, tx *sql.Tx, repo RepoRef, target string, total bool,
	out *ReadAnswer) error {

	since := store.EncodeTime(r.now().Add(-QueueEjectionsShown))
	stmt, args := ejectedOfRepoSQL, []any{repo.ID, since, MaxEjectionsShown}
	if target != "" {
		stmt, args = ejectedOfTargetSQL, []any{repo.ID, target, since, MaxEjectionsShown}
	}
	if err := ejectedRows(ctx, tx, map[string]string{repo.ID: repo.Name}, stmt, args, out); err != nil {
		return fmt.Errorf("gitlet: read what %s's queues put out: %w", repo.Name, err)
	}
	if !total {
		return nil
	}
	stmt, args = ejectedCountOfRepoSQL, []any{repo.ID, since, r.queueCounted}
	if target != "" {
		stmt, args = ejectedCountOfTargetSQL, []any{repo.ID, target, since, r.queueCounted}
	}
	var n int64
	if err := tx.QueryRowContext(ctx, stmt, args...).Scan(&n); err != nil {
		return fmt.Errorf("gitlet: count what %s's queues put out: %w", repo.Name, err)
	}
	out.EjectedTotal = &EjectedTotal{Total: n, More: n == int64(r.queueCounted)}
	return nil
}

// ejectionsEverywhere files into out what the queues of every repository the asker reads put
// out in the last [QueueEjectionsShown], newest first, at most [MaxEjectionsShown], and their
// total: the newest [MaxQueueCounted] ejections counted by repository, the repositories judged
// once, then the newest rows of those the asker reads. Past the bound the rows keep to the
// counted window, so they and the total cover the same ejections.
func (r *Reader) ejectionsEverywhere(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster,
	out *ReadAnswer) error {

	since := store.EncodeTime(r.now().Add(-QueueEjectionsShown))
	counted, read, oldest, err := ejectedByRepo(ctx, tx, since, r.queueCounted)
	if err != nil {
		return fmt.Errorf("gitlet: count what every queue put out: %w", err)
	}
	listed, err := listedReposAmong(ctx, tx, slices.Collect(maps.Keys(counted)), q.Asker, q.CeilingSeat, chart,
		"a page of ejections")
	if err != nil {
		return err
	}
	total := &EjectedTotal{More: read == int64(r.queueCounted)}
	if total.More {
		since = max(since, oldest)
	}
	names := make(map[string]string, len(listed))
	for id, repo := range listed {
		total.Total += counted[id]
		names[id] = repo.ref.Name
	}
	out.EjectedTotal = total
	if len(names) == 0 {
		return nil
	}
	list, err := jsonList(slices.Sorted(maps.Keys(names)))
	if err != nil {
		return err
	}
	if err = ejectedRows(ctx, tx, names, ejectedEverywhereSQL, []any{since, list, MaxEjectionsShown},
		out); err != nil {
		return fmt.Errorf("gitlet: read what every queue put out: %w", err)
	}
	return nil
}

// countedByRepo is what stmt, bound to args, counts by repository, and how many index
// entries it read.
func countedByRepo(ctx context.Context, tx *sql.Tx, stmt string, args ...any) (map[string]int64, int64, error) {
	rows, err := tx.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	counted := map[string]int64{}
	var read int64
	for rows.Next() {
		var (
			id string
			n  int64
		)
		if err = rows.Scan(&id, &n); err != nil {
			return nil, 0, err
		}
		counted[id], read = n, read+n
	}
	return counted, read, rows.Err()
}

// ejectedByRepo is the newest ejections ending at or after since, at most bound of them,
// counted by repository, how many it read, and when the oldest of them ended.
func ejectedByRepo(ctx context.Context, tx *sql.Tx, since int64, bound int) (counted map[string]int64,
	read, oldest int64, err error) {

	rows, err := tx.QueryContext(ctx, ejectedReposSQL, since, bound)
	if err != nil {
		return nil, 0, 0, err
	}
	defer func() { _ = rows.Close() }()
	counted = map[string]int64{}
	for rows.Next() {
		var (
			id     string
			n, end int64
		)
		if err = rows.Scan(&id, &n, &end); err != nil {
			return nil, 0, 0, err
		}
		counted[id], read = n, read+n
		if oldest == 0 || end < oldest {
			oldest = end
		}
	}
	return counted, read, oldest, rows.Err()
}

// ejectedRows files into out the ejections stmt reads for args, each of a repository names
// holds by id, with its change's label.
func ejectedRows(ctx context.Context, tx *sql.Tx, names map[string]string, stmt string, args []any,
	out *ReadAnswer) error {

	rows, err := tx.QueryContext(ctx, stmt, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var (
		entries []queueEntry
		details []string
	)
	for rows.Next() {
		var detail string
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		e, err := scanEntry(func(dest ...any) error { return rows.Scan(append(dest, &detail)...) })
		if err != nil {
			return err
		}
		entries, details = append(entries, e), append(details, detail)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	labels, err := labelsOf(ctx, tx, entries)
	if err != nil {
		return err
	}
	for i, e := range entries {
		row, rowErr := ejectedRow(e, names[e.RepoID], labels[e.ChangeID], details[i])
		if rowErr != nil {
			return rowErr
		}
		out.Ejected = append(out.Ejected, row)
	}
	return nil
}

// ejectedRow is e as a queue read answers it: of repository repo, its change's label, and
// the ejection's detail as the `ejected` column keeps it.
func ejectedRow(e queueEntry, repo string, label changeLabel, detail string) (EjectedRow, error) {
	row := EjectedRow{RepoID: e.RepoID, Repo: repo, EntryID: e.EntryID, TargetRef: e.TargetRef,
		ChangeID: e.ChangeID, Number: label.number, Title: label.title, SourceRef: label.sourceRef,
		HeadOID: e.HeadOID, EnqueuedBy: e.EnqueuedBy, Method: e.Method, DeleteSource: e.DeleteSource,
		CloseTask: e.CloseTask, Reason: e.EndReason, EnqueuedAt: store.DecodeTime(e.EnqueuedAt),
		EjectedAt: store.DecodeTime(e.EndedAt)}
	if detail == "" {
		return row, nil
	}
	var x struct {
		Checks   []string       `json:"checks"`
		Paths    []string       `json:"paths"`
		TestedIn *EjectionTrain `json:"tested_in"`
	}
	if err := json.Unmarshal([]byte(detail), &x); err != nil {
		return EjectedRow{}, fmt.Errorf("gitlet: read entry %s's ejection: %w", e.EntryID, err)
	}
	row.Checks, row.Paths, row.TestedIn = x.Checks, x.Paths, x.TestedIn
	return row, nil
}

// LandingRow is one landing on a protected target: the record and its ordinal
// in it, its kind, the change and entry it landed ("" for a push), the target
// before and after, who, and whether a bisect of the tip checks found it broken.
// A landing is green by position, at or below the green row, so none listed is.
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

// QueueDueQuery is the home's read of the queue work it owes
// ([Reader.QueueDue]): a page of the repositories owing any, where it names no
// repository, or one repository's, by id, a target a page.
//
// An estate request, not a record, so it takes no versionedFields row: an older
// data node answers it unserved, and the home's queue waits for one that serves it.
type QueueDueQuery struct {
	RepoID string `json:"repo_id,omitempty"`
	// EnqueuedSince, beside a RepoID, has each page's target's enqueues since
	// it counted ([QueueDue.Enqueued]): the home states now less
	// [MergeRateWindow]. Zero counts none.
	EnqueuedSince time.Time `json:"enqueued_since,omitzero"`
	// Cursor is "" for a first page, else the Next the page before answered: a
	// listing's last repository, marked by the walk it ended in, or one
	// repository's last target.
	Cursor string `json:"cursor,omitempty"`
	// Fresh is the level the page is read at ([checkFreshness]): the home states
	// the position its nudge or record landed at as a session floor; none is stale.
	Fresh statelog.Freshness `json:"fresh"`
}

// QueueDueAnswer is a [QueueDueQuery]'s page: a listing's repositories
// (DueRepos), or one target's queue work (DueQueue) with, on a repository's
// first page, the attempts its home cancels and the closes it owes; the next
// page's cursor, "" past the last; and the position the page was read at.
//
// Read whole or refused (internal/estate's code.queue_due): the home carries
// what it reads into the records it asks next, so a member this build does not
// declare fails the turn, and the queue waits for a home that declares it,
// rather than being dropped while the rest is carried on.
type QueueDueAnswer struct {
	// DueRepos is a listing's page of repositories, by id, at most
	// MaxSnapshotItems, in [Reader.dueQueueRepos]'s walk order.
	DueRepos []string `json:"due_repos,omitempty"`
	// DueQueue is one repository's page: one target's queue work
	// ([QueueDue]), the target the next page's cursor; nil past the last.
	DueQueue *QueueDue `json:"due_queue,omitempty"`
	// QueueAbandoned is, on a repository's first page, its queues' unended
	// attempts on commits no train of their target tests or landed
	// ([AbandonedCheck]): what its home cancels once its targets have stepped.
	QueueAbandoned []AbandonedCheck `json:"queue_abandoned,omitempty"`
	// QueueOwedCloses is, on the same first page, the repository's landed
	// entries still owing their change's work item its close (M5-D27,
	// code_queue.close_op), at most [MaxOwedCloses]: what its home closes
	// as each entry's enqueuer and settles (close_settled), whichever home
	// asked the landing.
	QueueOwedCloses []QueueWorkEntry `json:"queue_owed_closes,omitempty"`
	Next            string           `json:"next,omitempty"`
	// Position is the checkpoint the page was read at: what each record its
	// home asks from the page states it saw ([QueueProposal.Seen]).
	Position statelog.Position `json:"position"`
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

// validate refuses a due-queue read its home cannot have meant: a repository id
// not in its canonical spelling, or enqueues counted of the listing. It names
// no asker, since no principal makes it.
func (q QueueDueQuery) validate() error {
	refused := func(field, why string) error { return fmt.Errorf("%w: %s: %s", ErrInvalid, field, why) }
	switch {
	case q.RepoID != "" && !canonicalUUID(q.RepoID):
		return refused("repo_id", fmt.Sprintf("%q is not a repository's id in its canonical lowercase spelling",
			q.RepoID))
	case q.RepoID == "" && !q.EnqueuedSince.IsZero():
		return refused("enqueued_since", "the enqueues counted are a repository's targets', so they are asked "+
			"beside its repo_id, never of the listing")
	}
	return nil
}

// queueQuery is the framework read a queue read resolves to: a SET READ of the
// repository it names, resolved by its name as a push history is, or of the
// domain, for a listing across repositories.
func (q ReadQuery) queueQuery(fresh statelog.Freshness) statelog.Query {
	if q.Repo == "" {
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

// queueListing serves one page of a `queue` read: the live entries q names, in
// queue order, and, for a named repository or where q asks [ReadQuery.Trains],
// the trains of the targets the page reaches and, on a first page, the recent
// ejections ([Reader.ejections], [Reader.ejectionsEverywhere]).
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
	// THE CHANGES OF THE LISTED REPOSITORIES ALONE are read, so no title of
	// one the asker may not read is ever fetched.
	var listed []queueEntry
	for _, e := range scanned {
		if _, ok := repos[e.RepoID]; ok {
			listed = append(listed, e)
		}
	}
	labels, err := labelsOf(ctx, tx, listed)
	if err != nil {
		return err
	}
	var (
		group [2]string
		place int
	)
	for _, e := range listed {
		if key := [2]string{e.RepoID, e.TargetRef}; key != group {
			group = key
			if place, err = placeIn(ctx, tx, e); err != nil {
				return err
			}
		} else {
			place++
		}
		out.Queue = append(out.Queue, e.row(repos[e.RepoID].ref.Name, labels[e.ChangeID], place))
	}
	if len(scanned) == limit {
		out.Next = queueCursor(scanned[len(scanned)-1])
	}
	switch {
	case q.Repo == "" && !q.Trains:
		return nil
	case q.Repo == "" && !resumed:
		if err = r.ejectionsEverywhere(ctx, tx, q, chart, out); err != nil {
			return err
		}
	case !resumed:
		for _, named := range repos {
			if err = r.ejections(ctx, tx, named.ref, q.Target, q.Trains, out); err != nil {
				return err
			}
		}
	}
	// THE TRAINS OF THE TARGETS THE PAGE REACHED: a target's candidates are
	// its live trains' alone, since a train that ends removes them.
	var targets [][2]string
	for _, row := range out.Queue {
		targets = appendOnce(targets, [2]string{row.RepoID, row.TargetRef})
	}
	for _, target := range targets {
		trains, err := trainsOf(ctx, tx, target[0], target[1])
		if err != nil {
			return err
		}
		out.Trains = append(out.Trains, trains...)
	}
	return nil
}

// QueueDue answers one page of the queue work a repository's home Gitlet node
// owes ([QueueDueQuery]): with no repository, the active repositories owing any
// ([Reader.dueQueueRepos]); with one, its next target's work ([dueQueue]).
//
// A set read, as `changes_due`'s: the committer decides every record the home
// asks after it again from its own snapshot. Read at the query's level
// ([checkFreshness]), stale where it names none, and at no grant, since no
// principal asks it.
func (r *Reader) QueueDue(ctx context.Context, q QueueDueQuery) (QueueDueAnswer, error) {
	if err := q.validate(); err != nil {
		return QueueDueAnswer{}, err
	}
	fresh := q.Fresh
	if fresh.Level == "" {
		fresh.Level = statelog.ReadStale
	}
	if err := checkFreshness(fresh); err != nil {
		return QueueDueAnswer{}, err
	}
	scope := statelog.ScopeSet{Paths: []string{DomainPath()}}
	if q.RepoID != "" {
		scope = statelog.ScopeSet{Paths: []string{RepoPath(q.RepoID)}}
	}
	var out QueueDueAnswer
	if _, err := r.log.ReadThrough(ctx, fresh.Query(scope, true), func(tx *sql.Tx, cp statelog.Checkpoint) error {
		out = QueueDueAnswer{Position: cp.At}
		if q.RepoID != "" {
			return dueQueue(ctx, tx, q.RepoID, q.Cursor, q.EnqueuedSince, &out)
		}
		return r.dueQueueRepos(ctx, tx, q.Cursor, &out)
	}); err != nil {
		return QueueDueAnswer{}, err
	}
	return out, nil
}

// dueQueueRepos files one page of the due-queue listing after cursor into out:
// the active repositories owing queue work, by id, MaxSnapshotItems a page,
// across three walks of their own indexes — a live entry, a given-up train's
// attempt (queuework.go, cursor marked [abandonedCursor]), a landed change's
// owed close (code_queue.close_op, [closeOwedCursor]). An archived or deleted
// repository's queue is nobody's work until it is active again. A repository in several walks is listed
// in each, and its home works it once.
func (r *Reader) dueQueueRepos(ctx context.Context, tx *sql.Tx, cursor string, out *QueueDueAnswer) error {
	// The walks run in order, each resumed from its own cursor mark, filling
	// one page. Each names its statement, a constant, at its call, so the
	// read-index gate reads which statement it runs.
	after, owed := strings.CutPrefix(cursor, closeOwedCursor)
	after, abandoned := strings.CutPrefix(after, abandonedCursor)
	walk := func(mark, what string, list func(after string, room int) ([]string, error)) (bool, error) {
		room := r.pageRows - len(out.DueRepos)
		ids, err := list(after, room)
		if err != nil {
			return false, fmt.Errorf("gitlet: list the repositories %s: %w", what, err)
		}
		out.DueRepos = append(out.DueRepos, ids...)
		after = ""
		if len(ids) == room {
			out.Next = mark + ids[len(ids)-1]
			return true, nil
		}
		return false, nil
	}
	if !owed && !abandoned {
		if full, err := walk("", "holding a queue", func(after string, room int) ([]string, error) {
			return dueRepoIDs(ctx, tx, queueDueReposSQL, after, room)
		}); full || err != nil {
			return err
		}
	}
	if !owed {
		if full, err := walk(abandonedCursor, "holding a given-up train's checks",
			func(after string, room int) ([]string, error) {
				return dueRepoIDs(ctx, tx, abandonedReposSQL, after, room)
			}); full || err != nil {
			return err
		}
	}
	_, err := walk(closeOwedCursor, "owing a landed change's work item its close",
		func(after string, room int) ([]string, error) {
			return dueRepoIDs(ctx, tx, closeOwedReposSQL, after, room)
		})
	return err
}

// abandonedCursor and closeOwedCursor mark a due-queue cursor as one of the
// second or third walk ([Reader.dueQueueRepos]), the id after it that walk's
// last. No repository id, a canonical UUID, begins with either.
const (
	abandonedCursor = "abandoned:"
	closeOwedCursor = "close:"
)

// dueRepoIDs is the repository ids stmt lists after cursor, at most limit.
func dueRepoIDs(ctx context.Context, tx *sql.Tx, stmt, cursor string, limit int) ([]string, error) {
	rows, err := tx.QueryContext(ctx, stmt, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// landings serves one page of a `landings` read: the target's green, where it
// has one, and its landings above it in landing order, after the cursor (a
// landing's position and its ordinal in its record).
func (r *Reader) landings(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, out *ReadAnswer) error {
	named, err := readableIn(ctx, tx, q.Repo, q.Asker, q.CeilingSeat, chart)
	if err != nil {
		return err
	}
	green, found, err := greenIn(ctx, tx, named.ref.ID, q.Target)
	if err != nil {
		return err
	}
	// Every landing of the green row's record is green with its tip, so the
	// first page starts past that record's every ordinal.
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
		return fmt.Errorf("gitlet: read %s's landings on %s: %w", named.ref.Name, q.Target, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			l         LandingRow
			packed, t int64
		)
		if err := rows.Scan(&packed, &l.Ordinal, &l.Kind, &l.ChangeID, &l.EntryID, &l.BaseOID, &l.CommitOID,
			&l.EnqueuedBy, &l.MergedBy, &l.Broken, &t); err != nil {
			return fmt.Errorf("gitlet: read %s's landings on %s: %w", named.ref.Name, q.Target, err)
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
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		green, found, err := greenIn(ctx, tx, named.ref.ID, q.Target)
		if err != nil || !found {
			return err
		}
		out.Green = []GreenRow{green}
		return nil
	}
	rows, err := tx.QueryContext(ctx, greensSQL, named.ref.ID)
	if err != nil {
		return fmt.Errorf("gitlet: read %s's green rows: %w", named.ref.Name, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			g         GreenRow
			packed, t int64
		)
		if err := rows.Scan(&g.TargetRef, &g.CommitOID, &packed, &t); err != nil {
			return fmt.Errorf("gitlet: read %s's green rows: %w", named.ref.Name, err)
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
		return GreenRow{}, false, fmt.Errorf("gitlet: read %s's green row: %w", target, err)
	}
	g.Position, g.At = unpacked(packed), store.DecodeTime(t)
	return g, true, nil
}

// entriesBy reads the entries query answers for args.
func entriesBy(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]queueEntry, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read a queue's entries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []queueEntry
	for rows.Next() {
		e, err := scanEntry(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gitlet: read a queue's entries: %w", err)
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

// changeLabel is what a queue row names of its change.
type changeLabel struct {
	number           int64
	title, sourceRef string
}

// labelsOf is the label of each change entries name, by id: one statement for each
// repository the entries reach, over the list of its changes' ids.
func labelsOf(ctx context.Context, tx *sql.Tx, entries []queueEntry) (map[string]changeLabel, error) {
	var repos []string
	byRepo := map[string][]string{}
	for _, e := range entries {
		if _, seen := byRepo[e.RepoID]; !seen {
			repos = append(repos, e.RepoID)
		}
		byRepo[e.RepoID] = appendOnce(byRepo[e.RepoID], e.ChangeID)
	}
	out := map[string]changeLabel{}
	for _, repo := range repos {
		if err := labelsIn(ctx, tx, repo, byRepo[repo], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// labelsIn files into out the label of each change ids names that is a change of repoID.
func labelsIn(ctx context.Context, tx *sql.Tx, repoID string, ids []string, out map[string]changeLabel) error {
	list, err := jsonList(ids)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, changeLabelsSQL, repoID, list)
	if err != nil {
		return fmt.Errorf("gitlet: read the titles of repository %s's changes: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			id string
			l  changeLabel
		)
		if err = rows.Scan(&id, &l.number, &l.title, &l.sourceRef); err != nil {
			return fmt.Errorf("gitlet: read the titles of repository %s's changes: %w", repoID, err)
		}
		out[id] = l
	}
	return rows.Err()
}

// trainsOf is every candidate of target's live trains on repoID, in train and
// ordinal order — one seek of code_candidates_train_idx.
func trainsOf(ctx context.Context, tx *sql.Tx, repoID, target string) ([]CandidateRow, error) {
	rows, err := tx.QueryContext(ctx, trainsSQL, repoID, target)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read %s's trains: %w", target, err)
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
			return nil, fmt.Errorf("gitlet: read %s's trains: %w", target, err)
		}
		if err := json.Unmarshal([]byte(entries), &c.Entries); err != nil {
			return nil, fmt.Errorf("gitlet: read candidate %s's entries: %w", c.CandidateID, err)
		}
		c.Position = unpacked(packed)
		out = append(out, c)
	}
	return out, rows.Err()
}

// row is e as a queue read answers it, of repository repo, its change's
// label and its place.
func (e queueEntry) row(repo string, label changeLabel, place int) QueueRow {
	return QueueRow{RepoID: e.RepoID, Repo: repo, EntryID: e.EntryID, TargetRef: e.TargetRef, ChangeID: e.ChangeID,
		Number: label.number, Title: label.title, SourceRef: label.sourceRef, Place: place, Seq: unpacked(e.Seq), HeadOID: e.HeadOID, Method: e.Method,
		DeleteSource: e.DeleteSource, CloseTask: e.CloseTask, EnqueuedBy: e.EnqueuedBy,
		EnqueuedOperator: e.EnqueuedOperator, State: e.State, Train: e.Train, CandidateID: e.CandidateID,
		CommitOID: e.CommitOID, EnqueuedAt: store.DecodeTime(e.EnqueuedAt)}
}

// The queue's reads, each registered in plan_test.go so the pinned driver's
// planner is held to the index each names and to reading in its order. A
// resumed keyset keeps the >= beside the OR so the range seek holds
// (planner.go); the ejection reads seek the indexes partial on ejections
// (replicated 0132), every repository's the one that leads with the ending.
// Every read of code_queue by a partial index repeats its predicate verbatim.
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
	ejectedOfRepoSQL = `SELECT ` + entryColumns + `, ejected FROM code_queue INDEXED BY code_queue_ejected_idx
		WHERE repo_id = ? AND state = 'ejected' AND ended_at >= ?
		ORDER BY ended_at DESC LIMIT ?`
	ejectedOfTargetSQL = `SELECT ` + entryColumns + `, ejected FROM code_queue
			INDEXED BY code_queue_target_ejected_idx
		WHERE repo_id = ? AND target_ref = ? AND state = 'ejected' AND ended_at >= ?
		ORDER BY ended_at DESC LIMIT ?`
	ejectedCountOfRepoSQL = `SELECT COUNT(*) FROM (SELECT 1 FROM code_queue INDEXED BY code_queue_ejected_idx
		WHERE repo_id = ? AND state = 'ejected' AND ended_at >= ? LIMIT ?)`
	ejectedCountOfTargetSQL = `SELECT COUNT(*) FROM (SELECT 1 FROM code_queue INDEXED BY code_queue_target_ejected_idx
		WHERE repo_id = ? AND target_ref = ? AND state = 'ejected' AND ended_at >= ? LIMIT ?)`
	ejectedReposSQL = `SELECT repo_id, COUNT(*), MIN(ended_at) FROM (SELECT repo_id, ended_at FROM code_queue
			INDEXED BY code_queue_ejected_recent_idx WHERE state = 'ejected' AND ended_at >= ?1
			ORDER BY ended_at DESC LIMIT ?2)
		GROUP BY repo_id`
	ejectedEverywhereSQL = `SELECT ` + entryColumns + `, ejected FROM code_queue
			INDEXED BY code_queue_ejected_recent_idx
		WHERE state = 'ejected' AND ended_at >= ?1 AND repo_id IN (SELECT CAST(value AS TEXT) FROM json_each(?2))
		ORDER BY ended_at DESC, repo_id DESC LIMIT ?3`
	queueCountOfRepoSQL = `SELECT COUNT(*) FROM (SELECT 1 FROM code_queue INDEXED BY code_queue_live_idx
		WHERE repo_id = ? AND state IN ('waiting', 'testing') LIMIT ?)`
	queueCountsSQL = `SELECT repo_id, COUNT(*) FROM (SELECT repo_id FROM code_queue INDEXED BY code_queue_live_idx
			WHERE state IN ('waiting', 'testing') AND repo_id >= '' LIMIT ?1)
		GROUP BY repo_id`
	changeLabelsSQL = `SELECT c.id, c.number, c.title, c.source_ref FROM json_each(?2) AS j
		CROSS JOIN code_changes AS c WHERE c.id = CAST(j.value AS TEXT) AND c.repo_id = ?1`
	trainsSQL = `SELECT candidate_id, train, ordinal, base_oid, tip_oid, pack_id, state, position, entries
		FROM code_candidates INDEXED BY code_candidates_train_idx WHERE repo_id = ? AND target_ref = ?
		ORDER BY train, ordinal`
	landingsSQL = `SELECT position, ordinal, kind, change_id, entry_id, base_oid, commit_oid, enqueued_by,
			merged_by, broken, at
		FROM code_landings WHERE repo_id = ? AND target_ref = ? AND position >= ?
			AND (position > ? OR ordinal > ?)
		ORDER BY position, ordinal LIMIT ?`
	greenSQL  = `SELECT commit_oid, position, at FROM code_green WHERE repo_id = ? AND target_ref = ?`
	greensSQL = `SELECT target_ref, commit_oid, position, at FROM code_green WHERE repo_id = ? ORDER BY target_ref`
)
