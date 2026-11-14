package gitlet

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

// A TARGET'S QUEUE, ITS LANDINGS AND ITS GREEN, READ — four reads over the
// rows a repository's queue records write (queue.go), three of them kinds of
// [ReadQuery] and the fourth a read of its own:
//
//   - `queue` ([ReadQueue]) lists the LIVE entries of every queue — of one
//     repository, of one of its targets, or of every repository the asker
//     reads — in queue order, each with its place, beside the trains testing a
//     named repository's (code_queue_live_idx, code_candidates_train_idx) and
//     the entries its queues put out in the last [QueueEjectionsShown]
//     (code_queue_ended_idx), held to the asker's grant as a listing of
//     changes is.
//   - `landings` ([ReadLandings]) is one target's landings ABOVE ITS GREEN, in
//     the order they landed, beside the green itself: what its tip checks have
//     not yet judged.
//   - `green` ([ReadGreen]) is one repository's green rows: each protected
//     target's newest commit whose required checks passed.
//   - the DUE QUEUE ([Reader.QueueDue], internal/estate's code.queue_due) is
//     the home's: the ACTIVE repositories holding a live entry, then those
//     holding a given-up train's attempt, then those owing a landed change's
//     work item its close, by id — an archived or deleted one's queue is
//     nobody's work until it is active again — or one repository's queue
//     work, a target a page ([QueueDue], queuework.go), its first page with
//     the given-up trains' attempts it cancels ([AbandonedCheck]) and the
//     closes it owes: the engine's own read, which no principal makes. A
//     READ OF ITS OWN, never a kind of [ReadQuery], because its home carries
//     every entry it reads into the record it asks next — a landing, an
//     ejection, a close and its settle — so its answer crosses read whole or
//     refused ([QueueDueAnswer]), where a [ReadAnswer], which a caller only
//     shows, is read for what its build declares.
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

// EjectedRow is one entry a queue put out, as a `queue` read of its
// repository answers it beside the live ones: the change it was and the head
// it was enqueued at, who enqueued it, why the queue put it out
// ([QueueEndReason]: check_failed, conflict, gate_unmet, head_moved,
// not_fast_forward) and what of it a person reads first — the required checks
// that failed on what it was tested in, or the paths its merge conflicted on —
// and when.
//
// AN ANSWER'S ROW, NOT A RECORD'S, so it takes no versionedFields row. The
// domain landed on main with it ([RecordVersion]), so every build that answers
// a `queue` read answers its ejections.
type EjectedRow struct {
	RepoID    string `json:"repo_id"`
	Repo      string `json:"repo"`
	EntryID   string `json:"entry_id"`
	TargetRef string `json:"target_ref"`
	ChangeID  string `json:"change_id"`
	// Number is its change's number on its repository (web!42's 42).
	Number     int64          `json:"number"`
	HeadOID    string         `json:"head_oid"`
	EnqueuedBy Party          `json:"enqueued_by"`
	Reason     QueueEndReason `json:"reason"`
	Checks     []string       `json:"checks,omitempty"`
	Paths      []string       `json:"paths,omitempty"`
	EnqueuedAt time.Time      `json:"enqueued_at"`
	EjectedAt  time.Time      `json:"ejected_at"`
}

// QueueEjectionsShown is how far back a `queue` read of a repository answers
// the entries its queues put out: a DAY. An ejection is a person's or a
// seat's to act on — fix the change and enqueue it again — and its own wake
// (`dequeued`, `checks_failed`) reaches whoever must; the Queue section shows
// it beside the queue it left so a reader watching that queue sees why it
// shrank. A day covers a reader who looks once a working day; older ones are
// the change's own history, on its page.
const QueueEjectionsShown = 24 * time.Hour

// MaxEjectionsShown is the most ejections one `queue` read answers, newest
// first: 50, the queue listing's own page (defaultQueueLimit) — a band of a
// page's height under a queue's live entries. A queue that ejected more in a
// day is broken, not busy, and its fifty newest say why as well as all of
// them.
const MaxEjectionsShown = defaultQueueLimit

// ejections files into out what repository repo's queues put out in the last
// [QueueEjectionsShown] — its target's alone, where target names one — newest
// first, at most [MaxEjectionsShown]: one seek of an index partial on the
// ejections alone (replicated 0132), the repository's or the target's, from
// the newest down to the horizon, so the read touches the rows it answers and
// never the entries the queue LANDED meanwhile — which a healthy queue ends
// thousands of a day, and an index of every ended entry walked to find a
// handful of ejections on every poll of the Queue section.
func (r *Reader) ejections(ctx context.Context, tx *sql.Tx, repo RepoRef, target string, out *ReadAnswer) error {
	since := store.EncodeTime(r.now().Add(-QueueEjectionsShown))
	var (
		rows *sql.Rows
		err  error
	)
	if target == "" {
		rows, err = tx.QueryContext(ctx, ejectedOfRepoSQL, repo.ID, since, MaxEjectionsShown)
	} else {
		rows, err = tx.QueryContext(ctx, ejectedOfTargetSQL, repo.ID, target, since, MaxEjectionsShown)
	}
	if err != nil {
		return fmt.Errorf("gitlet: read what %s's queues put out: %w", repo.Name, err)
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
			return fmt.Errorf("gitlet: read what %s's queues put out: %w", repo.Name, err)
		}
		entries, details = append(entries, e), append(details, detail)
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("gitlet: read what %s's queues put out: %w", repo.Name, err)
	}
	numbers, err := numbersOf(ctx, tx, entries)
	if err != nil {
		return err
	}
	for i, e := range entries {
		row := EjectedRow{RepoID: e.RepoID, Repo: repo.Name, EntryID: e.EntryID, TargetRef: e.TargetRef,
			ChangeID: e.ChangeID, Number: numbers[e.ChangeID], HeadOID: e.HeadOID, EnqueuedBy: e.EnqueuedBy,
			Reason: e.EndReason, EnqueuedAt: store.DecodeTime(e.EnqueuedAt), EjectedAt: store.DecodeTime(e.EndedAt)}
		if details[i] != "" {
			var x struct {
				Checks []string `json:"checks"`
				Paths  []string `json:"paths"`
			}
			if err := json.Unmarshal([]byte(details[i]), &x); err != nil {
				return fmt.Errorf("gitlet: read entry %s's ejection: %w", e.EntryID, err)
			}
			row.Checks, row.Paths = x.Checks, x.Paths
		}
		out.Ejected = append(out.Ejected, row)
	}
	return nil
}

// LandingRow is one landing on a protected target: the record that landed it
// and its place in that record, what kind of landing it was, what landed —
// the change and its entry, "" for a push that landed none — the target before
// and after, who, and whether a bisect of the target's tip checks found it
// broken. Every landing it lists lies above the green row and so is not green:
// a landing is green by its position, at or below that row.
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
// AN ESTATE REQUEST, NOT A RECORD, so it takes no versionedFields row: a data
// node of a build that predates the read answers its operation unserved, as
// one it does not know, and the home's queue waits for a node that serves it,
// stepping on nothing meanwhile.
type QueueDueQuery struct {
	RepoID string `json:"repo_id,omitempty"`
	// EnqueuedSince, beside a RepoID, asks each page's target's enqueues at
	// or after it counted ([QueueDue.Enqueued]): the home's queue worker
	// states now less [MergeRateWindow], the window its merge rate is
	// measured over. Zero counts none.
	EnqueuedSince time.Time `json:"enqueued_since,omitzero"`
	// Cursor is "" for a first page, and the Next a page answered for the
	// page after it: a listing's last repository, marked by the walk it ended
	// in, or one repository's last target.
	Cursor string `json:"cursor,omitempty"`
	// Fresh is the level the page is read at, by every code read's one rule
	// ([checkFreshness]): the home states the position its nudge or its own
	// record landed at as a session floor, and a read naming no level is read
	// stale, as a duty listing is.
	Fresh statelog.Freshness `json:"fresh"`
}

// QueueDueAnswer is a [QueueDueQuery]'s page: a listing's repositories
// (DueRepos), or one repository's — one target's queue work (DueQueue), and
// on its first page the given-up trains' attempts its home cancels
// (QueueAbandoned) and the closes it owes (QueueOwedCloses) — beside the
// cursor of the page after it, "" past the last, and the position the page
// was read at.
//
// READ WHOLE OR REFUSED (internal/estate's code.queue_due): the home carries
// what it reads into the records it asks next, landing, ejecting and settling
// each entry as the entry it read, closing its change's work item as its
// enqueuer, and stating the page's position as what it saw. A member a newer
// data node puts in the answer that this build does not declare refuses the
// whole page — the turn fails, and the queue waits until its home runs a
// build that declares it — and is never dropped while the rest is carried on.
type QueueDueAnswer struct {
	// DueRepos is a listing's page of repositories, by id, at most
	// MaxSnapshotItems: those holding a live entry, then those holding a
	// given-up train's attempt, then those owing a landed change's work item
	// its close — a repository in several walks listed in each, and worked
	// once by its home.
	DueRepos []string `json:"due_repos,omitempty"`
	// DueQueue is one repository's page: one target's queue work
	// ([QueueDue]), the target the next page's cursor; nil past the last.
	DueQueue *QueueDue `json:"due_queue,omitempty"`
	// QueueAbandoned is, on one repository's first page, the attempts its
	// queues queued that have not ended, on commits no train of their target
	// tests or landed ([AbandonedCheck]): what its home cancels once its
	// targets have taken their steps.
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

// validate refuses a due-queue read its home cannot have meant: a repository,
// where it names one, by id in its canonical spelling, and enqueues counted
// beside that repository alone, never of the listing. Nothing else is asked
// of it: it names no asker, since no principal makes it, and every member its
// request may carry is one of these.
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

// queue serves one page of a `queue` read: the live entries of the repository
// q names — of its target, where it names one — or, naming none, of every
// repository the asker reads, in queue order; each with its place and its
// change's number; and, for a named repository, the candidates of every train
// of the targets the page reaches and, on its first page, what its queues —
// its target's, where it names one — put out lately ([Reader.ejections]).
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
	if !resumed {
		for _, named := range repos {
			if err := r.ejections(ctx, tx, named.ref, q.Target, out); err != nil {
				return err
			}
		}
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

// QueueDue answers one page of the queue work a repository's HOME Gitlet node
// owes ([QueueDueQuery]): with no repository, the active repositories its home
// owes queue work, by id ([Reader.dueQueueRepos]); with one, its next target's
// queue work after the cursor, one target a page ([dueQueue]).
//
// A SET READ, of the domain or of the one repository, as `changes_due`'s:
// every record the home asks after it is decided again by the repository's
// committer from its own snapshot. Read at the level the query names, by every
// code read's one rule ([checkFreshness]), and stale where it names none, as a
// duty listing is; and at no grant, since no principal asks it.
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

// dueQueueRepos files one page of the due-queue listing after cursor into
// out: the active repositories its home owes queue work, by id,
// MaxSnapshotItems a page — those holding a live entry, then those holding a
// given-up train's attempt (queuework.go), then those owing a landed change's
// work item its close (code_queue.close_op), three walks of their own
// indexes, the second's cursor marked [abandonedCursor] and the third's
// [closeOwedCursor]. A repository in several walks is listed in each, and its
// home works it once.
func (r *Reader) dueQueueRepos(ctx context.Context, tx *sql.Tx, cursor string, out *QueueDueAnswer) error {
	// THE WALKS IN ORDER, each resumed from its own cursor mark, the page
	// filled across them. Each walk names its statement at its call, a
	// constant, so the read-index gate reads which statement it runs.
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

// abandonedCursor and closeOwedCursor mark a due-queue listing's cursor as
// one of its second walk, the repositories holding a given-up train's
// attempt, or of its third, those owing a landed change's work item its
// close: the id after it is that walk's last. No repository's id — a UUID in
// its canonical spelling — begins with either.
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
// number and its place.
func (e queueEntry) row(repo string, number int64, place int) QueueRow {
	return QueueRow{RepoID: e.RepoID, Repo: repo, EntryID: e.EntryID, TargetRef: e.TargetRef, ChangeID: e.ChangeID,
		Number: number, Place: place, Seq: unpacked(e.Seq), HeadOID: e.HeadOID, Method: e.Method,
		DeleteSource: e.DeleteSource, CloseTask: e.CloseTask, EnqueuedBy: e.EnqueuedBy,
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
// key; its green row, and a repository's, by theirs; and a repository's
// ejections, or one target's, newest first by the index partial on them
// (replicated 0132). Every read of code_queue by a partial index repeats its
// predicate verbatim.
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
	ejectedOfRepoSQL = `SELECT ` + entryColumns + `, ejected FROM code_queue INDEXED BY code_queue_ejected_idx
		WHERE repo_id = ? AND state = 'ejected' AND ended_at >= ?
		ORDER BY ended_at DESC LIMIT ?`
	ejectedOfTargetSQL = `SELECT ` + entryColumns + `, ejected FROM code_queue
			INDEXED BY code_queue_target_ejected_idx
		WHERE repo_id = ? AND target_ref = ? AND state = 'ejected' AND ended_at >= ?
		ORDER BY ended_at DESC LIMIT ?`
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
