package pages

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// The knowledge base's retention.
//
// Pages, containers and comments are kept for ever, for the reason [work]
// keeps items: a page is what the company knows, and a wiki that forgot
// would be answering "what do we already know about this" from a window. The
// three classes swept here are the ones that are MACHINERY rather than
// knowledge — a superseded body, an audit record, and a lock somebody's
// crash left behind — and each has a horizon that a bucket age could not
// express, because all six classes share one family.

// ChangeRetention is how long a page change record survives.
//
// A YEAR, the same horizon and for the same reasons [work.ChangeRetention]
// carries: the feed's dedupe window is days, an audit's question is months,
// and the cost of a longer horizon is the per-subject index every cluster
// member holds rather than the bytes.
const ChangeRetention = 365 * 24 * time.Hour

// ClaimGrace is how long an orphaned title claim survives the sweep.
//
// AN HOUR, where [OrphanGrace] — the value a live writer steps over a claim
// at — is thirty seconds. The two numbers answer different questions and
// must not be one constant. A writer stepping over a claim has a page in
// hand and a person waiting, so it takes the shortest grace that cannot race
// a healthy write. This DELETES, with nobody waiting and nothing to gain
// from being quick, so it takes the horizon past which a crash is the only
// remaining explanation.
//
// A claim that outlives its grace is not merely tidy to remove: it makes a
// title unusable, and the title is how a person addresses a page.
const ClaimGrace = time.Hour

// Sweeper is the retention pass over the knowledge base's own records.
//
// A FLEET SINGLETON, on [work.Sweeper]'s terms.
type Sweeper struct {
	docs Documents
	now  func() time.Time
}

// NewSweeper builds the knowledge base's retention pass.
func NewSweeper(docs Documents, now func() time.Time) *Sweeper {
	if now == nil {
		now = nowUTC
	}
	return &Sweeper{docs: docs, now: now}
}

// SweepChanges purges change records older than cutoff.
func (s *Sweeper) SweepChanges(ctx context.Context, cutoff time.Time) (int, error) {
	records, err := s.docs.Documents(ctx, coord.FamilyPages, "")
	if err != nil {
		return 0, fmt.Errorf("pages: list the record for the retention sweep: %w", err)
	}
	var swept int
	for _, rec := range records {
		if class, ok := ClassOf(rec.Key); !ok || class != ClassChange {
			continue
		}
		change, err := DecodeChange(rec.Value)
		if err != nil {
			// LEFT, not deleted, on [work.Sweeper.SweepChanges]'s rule: a
			// record this build cannot read is one a peer wrote, and
			// deleting it is how a rolling upgrade loses the newer half's
			// history.
			log.WarnContext(ctx, "pages_change_undecodable", "key", rec.Key,
				"error", err.Error(), "detail", "left in place")
			continue
		}
		if !change.CreatedAt.Before(cutoff) {
			continue
		}
		if _, err := s.docs.PurgeDocument(ctx, coord.FamilyPages, rec.Key, rec.Version); err != nil {
			return swept, fmt.Errorf("pages: purge the change %s: %w", rec.Key, err)
		}
		swept++
	}
	return swept, nil
}

// SweepRevisions trims each page's history to [RevisionsKept], newest first.
//
// PER PAGE, never a global count: a hundred revisions of the runbook nobody
// edits and a hundred of the one an auto-refiner rewrites every turn are
// both what the cap promises, and a global horizon would take the whole
// history of the quiet page to make room for the noisy one.
//
// Ordered by the revision's own VERSION rather than by its timestamp. The
// version is what the head counts and what a reader asks for; two revisions
// written in the same millisecond by two nodes have distinct versions and
// indistinguishable timestamps.
func (s *Sweeper) SweepRevisions(ctx context.Context) (int, error) {
	records, err := s.docs.Documents(ctx, coord.FamilyPages, "")
	if err != nil {
		return 0, fmt.Errorf("pages: list the record for the revision sweep: %w", err)
	}

	type versioned struct {
		key     string
		version uint64
		number  int
	}
	byPage := map[string][]versioned{}
	for _, rec := range records {
		if class, ok := ClassOf(rec.Key); !ok || class != ClassRevision {
			continue
		}
		pageID, ok := PageIDOf(rec.Key)
		if !ok {
			continue
		}
		rev, err := DecodeRevision(rec.Value)
		if err != nil {
			log.WarnContext(ctx, "pages_revision_undecodable", "key", rec.Key,
				"error", err.Error(), "detail", "left in place")
			continue
		}
		byPage[pageID] = append(byPage[pageID],
			versioned{key: rec.Key, version: rec.Version, number: rev.Version})
	}

	var swept int
	for pageID, revs := range byPage {
		if len(revs) <= RevisionsKept {
			continue
		}
		// NEWEST FIRST, so the tail of the slice is what goes.
		slices.SortFunc(revs, func(a, b versioned) int { return b.number - a.number })
		for _, rev := range revs[RevisionsKept:] {
			if _, err := s.docs.PurgeDocument(ctx, coord.FamilyPages, rev.key, rev.version); err != nil {
				return swept, fmt.Errorf("pages: purge revision %d of %s: %w",
					rev.number, pageID, err)
			}
			swept++
		}
		log.InfoContext(ctx, "pages_history_trimmed", "page", pageID,
			"kept", RevisionsKept, "purged", len(revs)-RevisionsKept)
	}
	return swept, nil
}

// SweepOrphans purges the records whose page is gone — title claims first,
// then comments and revisions left behind by an interrupted removal.
//
// THE TITLE CLAIM IS THE ONE THAT MATTERS. A stray comment costs a key; a
// stray claim makes a title unusable, and on this backend a title is how a
// person addresses a page.
func (s *Sweeper) SweepOrphans(ctx context.Context, at time.Time) (int, error) {
	records, err := s.docs.Documents(ctx, coord.FamilyPages, "")
	if err != nil {
		return 0, fmt.Errorf("pages: list the record for the orphan sweep: %w", err)
	}
	live := map[string]bool{}
	for _, rec := range records {
		if class, ok := ClassOf(rec.Key); ok && class == ClassPage {
			if id, ok := PageIDOf(rec.Key); ok {
				live[id] = true
			}
		}
	}

	var swept int
	for _, rec := range records {
		class, ok := ClassOf(rec.Key)
		if !ok {
			continue
		}
		var (
			pageID  string
			written time.Time
		)
		switch class {
		case ClassTitle:
			claim, err := DecodeClaim(rec.Value)
			if err != nil {
				log.WarnContext(ctx, "pages_claim_undecodable", "key", rec.Key,
					"error", err.Error(), "detail", "left in place")
				continue
			}
			pageID, written = claim.PageID, claim.CreatedAt
		case ClassComment:
			comment, err := DecodeComment(rec.Value)
			if err != nil {
				log.WarnContext(ctx, "pages_comment_undecodable", "key", rec.Key,
					"error", err.Error(), "detail", "left in place")
				continue
			}
			pageID, written = comment.PageID, comment.CreatedAt
		case ClassRevision:
			rev, err := DecodeRevision(rec.Value)
			if err != nil {
				continue
			}
			pageID, written = rev.PageID, rev.CreatedAt
		default:
			continue
		}
		if pageID == "" || live[pageID] || at.Sub(written) < ClaimGrace {
			continue
		}
		if _, err := s.docs.PurgeDocument(ctx, coord.FamilyPages, rec.Key, rec.Version); err != nil {
			return swept, fmt.Errorf("pages: purge the orphan %s: %w", rec.Key, err)
		}
		log.InfoContext(ctx, "pages_orphan_swept", "key", rec.Key,
			"class", class, "page", pageID)
		swept++
	}
	return swept, nil
}
