package gitlet

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
// point [NearLimit] names, and the one with the most live and recent refs
// together. The zero value is a reading that found none.
type RefsNear struct {
	// Repos is how many repositories have reached the point.
	Repos int

	// Worst is the name of the one with the most live + recent refs, Live its
	// live refs and Recent those set within [RefExpiry]; empty and zero where
	// Repos is.
	Worst  string
	Live   int
	Recent int
}

// NearLimit reports whether a repository holding live refs, recent of them
// set within [RefExpiry], has reached the point code_refs_near_limit stands
// for: at its rate it reaches [MaxRefsPerRepo] within RefExpiry (or holds it
// already), and the ref lifecycle cannot make the room, deleting only a seat's
// own-namespace branch nobody set for RefExpiry, never a recent one.
//
// It is >= because the push decide refuses a create (repo_refs) once the
// repository HOLDS the bound: one at it with nothing recent (persons' refs never
// expire) refuses every create. The threshold is two other decisions', never a
// number of its own (ADR-0015, internal/README.md's statelog row): repo_refs's
// bound and the lifecycle's window, over which recent refs are the rate.
func NearLimit(live, recent int) bool { return live+recent >= MaxRefsPerRepo }

// refsCandidateFloor is the fewest live refs at which [NearLimit] can hold,
// and so the cut of [Reader.RefsNearLimit]'s candidates: a repository's recent
// refs are some of its live ones, so live+recent >= MaxRefsPerRepo needs
// 2*live >= MaxRefsPerRepo, that is live at least half the bound rounded up.
// Arithmetic of the bound, never a tuning of its own.
const refsCandidateFloor = (MaxRefsPerRepo + 1) / 2

// RefsNearLimit reads the ACTIVE repositories [NearLimit] holds for, as this
// node's rows hold them at now (an archived one takes no push; a deleted one's
// refs are its purge's): candidates of [refsCandidateFloor] live refs from a
// keyset walk of code_repos_name_idx, then each one's refs set within [RefExpiry].
// Every page and count is its own STALE read, holding no estate reader connection
// across another's; a repository renamed or archived between them reads as the
// later one finds it. A page's LIMIT ([lifePage]) counts candidates, so one page
// usually visits every row: bounded by the table (declared, tool-made, unpurged
// repos), never by lifePage. A count ranges up to MaxRefsPerRepo +
// [MaxTombstonesPerRepo] keys (BenchmarkRefsNearLimit). Cached per quarter-hour
// trim tick (internal/engine's retention.evaluate): the figure moves over days,
// and every alarm beat, operator request and dashboard poll reads it.
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
			return RefsNear{}, fmt.Errorf("gitlet: list the repositories near the ref bound: %w", err)
		}
		for _, c := range page {
			var recent int
			if _, err := r.log.Read(ctx, query, func(tx *sql.Tx) error {
				return tx.QueryRowContext(ctx, refsSetSinceSQL, c.id, since).Scan(&recent)
			}); err != nil {
				return RefsNear{}, fmt.Errorf("gitlet: count the refs %s set within %s: %w", c.name, RefExpiry, err)
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
// code_repos_name_idx, as livesSQL's is, never a scan
// (TestEveryCodeIndexServesARegisteredQuery), status and refs tested on each
// row: no index leads with refs. A candidate's recent refs are counted over its
// key's repo_id prefix, the tombstones (an empty target) never counted: a
// deletion is not a ref set.
const (
	refsCandidatesSQL = `SELECT id, name, refs FROM code_repos
		WHERE name > ? AND status = 'active' AND refs >= ?
		ORDER BY name LIMIT ?`
	refsSetSinceSQL = `SELECT COUNT(*) FROM code_refs WHERE repo_id = ? AND target != '' AND at > ?`
)
