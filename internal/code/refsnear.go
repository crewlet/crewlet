package code

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// RefsNear is what the code_refs_near_limit alarm reads of this node's rows
// ([Reader.RefsNearLimit]): how many ACTIVE repositories have reached the
// point [NearLimit] names, and the one of them with the most live and recently
// set refs together — by name, with its live refs and how many of those were
// set within [RefExpiry]. The zero value is a reading that found none.
type RefsNear struct {
	// Repos is how many repositories have reached the point.
	Repos int

	// Worst is the name of the one with the most live + recent refs, its
	// live refs Live and those of them set within RefExpiry Recent; empty,
	// and both zero, where Repos is.
	Worst  string
	Live   int
	Recent int
}

// NearLimit reports whether a repository holding live refs, recent of them
// set within [RefExpiry], has reached the point code_refs_near_limit stands
// for: at the rate it sets refs it reaches [MaxRefsPerRepo] within RefExpiry
// unless refs are deleted — or holds it already — and the ref lifecycle cannot
// be what makes the room: it deletes only a seat's branch nobody set for
// RefExpiry, so none of the recent ones, and of the rest only those seats set
// in their own namespaces.
//
// REACHING THE BOUND IS WHAT IS REFUSED, so the sum is compared with >=, as
// the push decide compares what a repository holds with the bound: a create
// is refused repo_refs once the repository holds MaxRefsPerRepo live refs, not
// once it holds more. A repository at the bound exactly with nothing set
// within RefExpiry — persons' tags and branches, which never expire — refuses
// every create, and is the very one the alarm is for.
//
// THE THRESHOLD IS TWO OTHER DECISIONS', never a number of its own (ADR-0015,
// and internal/README.md's statelog row): the bound the push decide refuses a
// create at (repo_refs) and the lifecycle's window. Recent refs measure the
// rate over that window: a repository that set that many in the last
// RefExpiry, holding them all still, sets as many again in the next one.
func NearLimit(live, recent int) bool { return live+recent >= MaxRefsPerRepo }

// refsCandidateFloor is the fewest live refs at which [NearLimit] can hold,
// and so the cut of [Reader.RefsNearLimit]'s candidates: a repository's recent
// refs are some of its live ones, so live+recent >= MaxRefsPerRepo needs
// 2*live >= MaxRefsPerRepo, that is live at least half the bound rounded up.
// Arithmetic of the bound, never a tuning of its own.
const refsCandidateFloor = (MaxRefsPerRepo + 1) / 2

// RefsNearLimit reads the ACTIVE repositories that have reached the point
// [NearLimit] names as this node's rows hold them at now: those holding at
// least [refsCandidateFloor] live refs, half the bound — the only ones
// NearLimit can hold for, since a repository's recent refs are some of its
// live ones — found by a keyset walk of code_repos in name order on
// code_repos_name_idx (up to [lifePage] candidates a statement), and each
// one's refs set within [RefExpiry] by one count over its own live refs.
//
// AN ARCHIVED REPOSITORY IS NEVER COUNTED: it takes no push, so nothing it
// holds moves towards the bound, and a deleted one's refs are its purge's.
//
// EVERY STATEMENT IS ITS OWN STALE READ of this node's rows, as every alarm's
// reading is (the alarm says what this node holds): a page of the listing,
// or one candidate's count, so no read holds one of the estate's reader
// connections across another's work. What each one reads:
//
//   - a page of the listing visits code_repos_name_idx from the last
//     candidate's name on, every row with a row lookup — active, archived,
//     deleted and purging alike, since no index leads with status or refs —
//     until it has found lifePage candidates or reached the table's end. Its
//     LIMIT counts candidates, not rows visited, so with fewer than lifePage
//     of them (the usual case) ONE statement visits every row: it is bounded
//     by the table — the repositories a document declares
//     (config.CodeMaxRepos), those tools made and those not yet purged —
//     never by lifePage;
//   - a candidate's count is one range of code_refs' primary key over its
//     repo_id prefix, which holds its live refs AND its tombstones: up to
//     MaxRefsPerRepo + [MaxTombstonesPerRepo] entries, 200 000, each with a
//     row lookup for its target and its time (BenchmarkRefsNearLimit's
//     widest range).
//
// A repository renamed or archived between two of them is read as the later
// statement finds it, which the next reading corrects.
//
// Measured on the trim's quarter-hour tick and cached until the next one
// (internal/engine's retention.evaluate): the figure moves over days, and a
// reading is assembled on every alarm beat, operator request and dashboard
// poll.
func (r *Reader) RefsNearLimit(ctx context.Context, now time.Time) (RefsNear, error) {
	query := statelog.Freshness{Level: statelog.ReadStale}.Query(
		statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
	since := store.EncodeTime(now.Add(-RefExpiry))
	type candidate struct {
		id, name string
		live     int
	}
	var (
		out   RefsNear
		worst int
		page  = make([]candidate, 0, lifePage)
		after = ""
	)
	for {
		_, err := r.log.Read(ctx, query, func(tx *sql.Tx) error {
			page = page[:0]
			rows, err := tx.QueryContext(ctx, refsCandidatesSQL, after, refsCandidateFloor, lifePage)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var c candidate
				if err := rows.Scan(&c.id, &c.name, &c.live); err != nil {
					return err
				}
				page = append(page, c)
			}
			return rows.Err()
		})
		if err != nil {
			return RefsNear{}, fmt.Errorf("code: list the repositories near the ref bound: %w", err)
		}
		for _, c := range page {
			var recent int
			if _, err := r.log.Read(ctx, query, func(tx *sql.Tx) error {
				return tx.QueryRowContext(ctx, refsSetSinceSQL, c.id, since).Scan(&recent)
			}); err != nil {
				return RefsNear{}, fmt.Errorf("code: count the refs %s set within %s: %w", c.name, RefExpiry, err)
			}
			if !NearLimit(c.live, recent) {
				continue
			}
			out.Repos++
			// THE MOST LIVE AND RECENT TOGETHER is the repository nearest the
			// bound at its own rate, so the one an operator acts on first; the
			// first in name order of a tie, as the walk meets them.
			if c.live+recent > worst {
				worst = c.live + recent
				out.Worst, out.Live, out.Recent = c.name, c.live, recent
			}
		}
		if len(page) < lifePage {
			return out, nil
		}
		after = page[len(page)-1].name
	}
}

// THE ALARM'S TWO STATEMENTS. The candidates are a keyset walk of
// code_repos_name_idx, as livesSQL's is — never a scan, which
// TestEveryCodeIndexServesARegisteredQuery refuses — the status and the
// counter tested on each row the seek reaches: no index leads with refs, and
// the table is the repositories a document declares (code.repos,
// config.CodeMaxRepos), those tools made and those not yet purged. A
// candidate's recent refs are counted over its own rows by its key's repo_id
// prefix, which reaches at most MaxRefsPerRepo live rows and
// MaxTombstonesPerRepo tombstones, the tombstones (an empty target) never
// counted — a deletion is not a ref set.
const (
	refsCandidatesSQL = `SELECT id, name, refs FROM code_repos
		WHERE name > ? AND status = 'active' AND refs >= ?
		ORDER BY name LIMIT ?`
	refsSetSinceSQL = `SELECT COUNT(*) FROM code_refs WHERE repo_id = ? AND target != '' AND at > ?`
)
