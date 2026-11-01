package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/store"
)

// WHO READ A UNIT REPOSITORY, over a range of company days: every node's
// repo_read records summed, one row per reader.
//
// EVERY NODE'S ROWS, which is why this is the domain's and not the event log's:
// a repository's reads are recorded where they were served — the git node a
// fetch reached, the home a code tool's read was asked of — and its readers are
// the union, a node that has since left the fleet included.

// RepoReadsQuery is one repository's readers over a range of company days.
type RepoReadsQuery struct {
	// RepoID is the repository, by the id its records name.
	RepoID string

	// From and To are company day labels (`2026-09-23`), both inclusive, at
	// most [History] apart: what the rows keep.
	From, To string
}

// RepoReadsMaxDays is the longest range a repository's readers are read over:
// [History] in days, the days the rows keep — a range longer than that would
// promise days the applier has already let go.
var RepoReadsMaxDays = HorizonDays

// ErrBadRange is a readers query this domain cannot answer: no repository, a
// day that is not one, a range that ends before it starts or is longer than
// [RepoReadsMaxDays]. The caller's mistake, which no other node answers
// otherwise.
var ErrBadRange = errors.New("usage: the readers query is not one this read answers")

// Validate refuses a query this read cannot answer, naming why
// ([ErrBadRange]) — what a surface asks before it routes the read, so its
// caller is told by name rather than by a peer.
func (q RepoReadsQuery) Validate() error {
	if strings.TrimSpace(q.RepoID) == "" {
		return fmt.Errorf("%w: it names no repository", ErrBadRange)
	}
	return ValidRange(q.From, q.To)
}

// ValidRange refuses a range of company days a readers read cannot answer
// ([ErrBadRange]): a label that is not a day, a range that ends before it
// starts, one longer than [RepoReadsMaxDays] — so a surface can refuse its
// caller's range before it reads anything.
//
// EVERY REFUSAL A SENTENCE FOR THE CALLER past the sentinel, naming the end
// or the range to change: a surface quotes those words and nothing else.
func ValidRange(fromLabel, toLabel string) error {
	from, err := ParseDay("from", fromLabel)
	if err != nil {
		return err
	}
	to, err := ParseDay("to", toLabel)
	if err != nil {
		return err
	}
	if fromLabel > toLabel {
		return fmt.Errorf("%w: the range %s..%s ends before it starts", ErrBadRange, fromLabel, toLabel)
	}
	if from.Shift(RepoReadsMaxDays-1).Label < to.Label {
		return fmt.Errorf("%w: the range %s..%s is longer than the %d days a repository's readers are kept",
			ErrBadRange, fromLabel, toLabel, RepoReadsMaxDays)
	}
	return nil
}

// ParseDay is label as the company day a readers range names at its end which
// (`from` or `to`), or [ErrBadRange] naming that end and how a day is written.
// A label is canonical or nothing (period.Parse), so two labels compare as the
// days they name.
func ParseDay(which, label string) (period.Window, error) {
	day, err := period.Parse(period.Day, label, nil)
	if err != nil {
		return period.Window{}, fmt.Errorf("%w: %s %q is not a day, which is written 2026-09-23",
			ErrBadRange, which, label)
	}
	return day, nil
}

// check is [RepoReadsQuery.Validate], as the reads below hold every query
// they are handed to it.
func (q RepoReadsQuery) check() error { return q.Validate() }

// RepoReader is one reader's reads of a repository over a range, summed across
// its days and every node that served them.
//
// TAGGED, unlike the spend rows: its encoding is the one a page is measured by
// ([Fill]) and the one the readers route and tool answer with, so the page a
// node cuts is the page a surface sends.
type RepoReader struct {
	PrincipalKind string    `json:"principal_kind"`
	Principal     string    `json:"principal"`
	Seat          string    `json:"seat,omitempty"`
	Via           string    `json:"via"`
	Reads         int64     `json:"reads"`
	Bytes         int64     `json:"bytes"`
	LastAt        time.Time `json:"last_at"`
}

// Group is the reader r is: every row the read sums into it, so a page is cut
// between two readers and never inside one.
func (r RepoReader) Group() Group { return Group{r.PrincipalKind, r.Principal, r.Via, r.Seat} }

// The statements a page of readers issues, NAMED so the plan gate
// (TestEveryPagedStatementSeeksItsKeyAndStreams) explains the very text a page
// runs.
const (
	// repoReadersStatement is one principal kind's readers of a repository
	// from a principal on, in reader order — usage_repo_reads_repo_idx's own
	// (replicated 0088) — each summed over the range's days, with the keyset
	// a resumed page puts on the principal it resumes in as the %s.
	repoReadersStatement = `
		SELECT principal_kind, principal, via, seat, SUM(reads), SUM(bytes), MAX(last_at)
		  FROM usage_repo_reads
		 WHERE repo_id = ? AND principal_kind = ? AND principal >= ? AND day >= ? AND day <= ? AND %s
		 GROUP BY principal_kind, principal, via, seat
		 ORDER BY principal_kind, principal, via, seat`

	// repoReaderKindStatement is the walk's seek to a repository's next
	// principal kind, read off the reader index's head.
	repoReaderKindStatement = `
		SELECT MIN(principal_kind) FROM usage_repo_reads WHERE repo_id = ? AND principal_kind %s ?`

	// repoReadsElidedStatement is how many readers the per-day cap dropped
	// over a range, on every node, off the head rows' repository index.
	repoReadsElidedStatement = `
		SELECT COALESCE(SUM(readers_elided), 0)
		  FROM usage_repo_read_days
		 WHERE repo_id = ? AND day >= ? AND day <= ? AND readers_elided > 0`
)

// RepoReadsPage is one page of a repository's readers over q's days, in
// reader order, resuming after the reader after (empty for the first) and cut
// by [Fill] at budget bytes — a budget of zero or less is every reader.
//
// ONE PRINCIPAL KIND PER STATEMENT, each found by a seek of the reader index's
// head ([repoReaderKindStatement]), so a page that resumes SEEKS the principal
// it resumes at — `principal >= ?` under the index — and reads, past the keyset
// that holds the rest of that principal's readers, only its own rows: a
// statement over the whole repository with the keyset as a filter read every
// reader before the page's first again on every page. A reader's rows are its
// days on every node, summed in the statement, which streams in the index's
// order with no sort, so a statement stopped at a page's end has read that
// page's readers and nothing past them.
func RepoReadsPage(ctx context.Context, estate Estate, q RepoReadsQuery, after Group,
	budget int) (Page[RepoReader], error) {

	if err := q.check(); err != nil {
		return Page[RepoReader]{}, err
	}
	resume, err := resumeAfter("repo-readers", []string{"principal", "via", "seat"}, after)
	if err != nil {
		return Page[RepoReader]{}, err
	}
	var page Page[RepoReader]
	err = estate.Read(ctx, func(tx *sql.Tx) error {
		walk := func(yield func(RepoReader, error) bool) {
			kind, inclusive := resume.key(), true
			for {
				next, found, err := nextReaderKind(ctx, tx, q.RepoID, kind, inclusive)
				if err != nil {
					yield(RepoReader{}, err)
					return
				}
				if !found {
					return
				}
				from := ""
				if next == resume.key() && len(resume.after) > 1 {
					from = resume.after[1]
				}
				past, pastArgs := resume.on(next)
				for r, err := range repoReaderRows(ctx, tx, q, next, from, past, pastArgs) {
					if !yield(r, err) || err != nil {
						return
					}
				}
				kind, inclusive = next, false
			}
		}
		var fillErr error
		page, fillErr = Fill(budget, RepoReader.Group, walk)
		return fillErr
	})
	if err != nil {
		return Page[RepoReader]{}, fmt.Errorf("usage: read the readers of repository %s for %s..%s: %w",
			q.RepoID, q.From, q.To, err)
	}
	return page, nil
}

// repoReaderRows is one principal kind's readers of q's repository over q's
// days from principal from on, in reader order, where past — a keyset over the
// group's columns after the kind, or `1 = 1` — holds.
func repoReaderRows(ctx context.Context, tx *sql.Tx, q RepoReadsQuery, kind, from, past string,
	pastArgs []any) iter.Seq2[RepoReader, error] {

	args := append([]any{q.RepoID, kind, from, q.From, q.To}, pastArgs...)
	return scanned(ctx, tx, fmt.Sprintf(repoReadersStatement, past), args,
		func(rows *sql.Rows) (RepoReader, error) {
			var (
				r    RepoReader
				last int64
			)
			if err := rows.Scan(&r.PrincipalKind, &r.Principal, &r.Via, &r.Seat, &r.Reads, &r.Bytes,
				&last); err != nil {
				return r, err
			}
			if last != 0 {
				r.LastAt = store.DecodeTime(last)
			}
			return r, nil
		})
}

// nextReaderKind is the first principal kind after kind — or at it, when
// inclusive — that repository's readers hold, and whether there is one.
func nextReaderKind(ctx context.Context, tx *sql.Tx, repo, kind string, inclusive bool) (string, bool, error) {
	var next sql.NullString
	if err := tx.QueryRowContext(ctx, readerKindStatement(inclusive), repo, kind).Scan(&next); err != nil {
		return "", false, fmt.Errorf("find the next principal kind of repository %s's readers after %q: %w",
			repo, kind, err)
	}
	return next.String, next.Valid, nil
}

// readerKindStatement is [nextReaderKind]'s seek.
func readerKindStatement(inclusive bool) string {
	op := ">"
	if inclusive {
		op = ">="
	}
	return fmt.Sprintf(repoReaderKindStatement, op)
}

// RepoReadsElided is how many readers the per-day cap ([ReadersPerRepoDay])
// dropped from a repository's records over a range of company days, on every
// node — readers an answer over the range cannot name. Zero is the reading that
// says the answer is complete.
func RepoReadsElided(ctx context.Context, estate Estate, q RepoReadsQuery) (int64, error) {
	if err := q.check(); err != nil {
		return 0, err
	}
	var total int64
	err := estate.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, repoReadsElidedStatement, q.RepoID, q.From, q.To).Scan(&total)
	})
	if err != nil {
		return 0, fmt.Errorf("usage: count the elided readers of repository %s for %s..%s: %w",
			q.RepoID, q.From, q.To, err)
	}
	return total, nil
}
