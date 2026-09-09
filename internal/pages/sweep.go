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
//
// LISTED BY CLASS. All six classes share one family, so a listing with no
// prefix transfers every page body and every revision in the company to find
// the change records — which is the whole of the knowledge base, on a pass
// that runs hourly and deletes a handful of keys.
func (s *Sweeper) SweepChanges(ctx context.Context, cutoff time.Time) (int, error) {
	records, err := s.docs.Documents(ctx, coord.FamilyPages, ClassChange)
	if err != nil {
		return 0, fmt.Errorf("pages: list the record for the retention sweep: %w", err)
	}
	var swept int
	for _, rec := range records {
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
	// KEYS ONLY, and the version comes OUT OF THE KEY.
	//
	// A revision key is `r.<page>.<n>`, and n is the version — the same
	// number the head counts and a reader asks for. Decoding every
	// revision body to read it back transferred every past body of every
	// page in the company, hourly, to sort a list of integers the keys
	// already spell: at a 512 KiB cap and a hundred revisions a page, that
	// is the largest single transfer this engine makes, for a pass that
	// usually purges nothing.
	keys, err := s.docs.DocumentKeys(ctx, coord.FamilyPages, ClassRevision)
	if err != nil {
		return 0, fmt.Errorf("pages: list the revisions for the sweep: %w", err)
	}

	type versioned struct {
		key    string
		number int
	}
	byPage := map[string][]versioned{}
	for _, key := range keys {
		pageID, number, ok := RevisionOf(key)
		if !ok {
			// A key this grammar did not write, or one whose version
			// segment is not a number. Left in place: the sweep does not
			// delete what it cannot read.
			log.WarnContext(ctx, "pages_revision_key_unreadable", "key", key,
				"detail", "left in place")
			continue
		}
		byPage[pageID] = append(byPage[pageID], versioned{key: key, number: number})
	}

	var swept int
	for pageID, revs := range byPage {
		if len(revs) <= RevisionsKept {
			continue
		}
		// NEWEST FIRST, so the tail of the slice is what goes.
		slices.SortFunc(revs, func(a, b versioned) int { return b.number - a.number })
		for _, rev := range revs[RevisionsKept:] {
			// THE RECORD IS READ ONLY FOR THE ONES BEING PURGED, and only
			// for its coordination version: a purge is a compare-and-set,
			// so it needs the version the key is at, and nothing needs the
			// body.
			record, found, err := s.docs.Document(ctx, coord.FamilyPages, rev.key)
			if err != nil {
				return swept, fmt.Errorf("pages: read revision %d of %s: %w",
					rev.number, pageID, err)
			}
			if !found {
				// A peer swept it between the listing and this read, which
				// is the ordinary outcome of two nodes running the same
				// pass rather than a fault.
				continue
			}
			if _, err := s.docs.PurgeDocument(ctx, coord.FamilyPages, rev.key, record.Version); err != nil {
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
//
// # What it transfers
//
// The live page set and the two child classes are KEY LISTINGS: a comment key
// and a revision key both name their page, so whether they are orphaned is a
// question the key answers. Only the title claims are read, because a claim
// names its page in its VALUE and nowhere else — and there is one per page.
//
// A record's own timestamp is read only for the keys whose page is already
// gone, which on a healthy company is none. What this pass used to do was
// transfer every page body, every past revision and every comment in the
// company, hourly, to find the handful a crash left behind.
func (s *Sweeper) SweepOrphans(ctx context.Context, at time.Time) (int, error) {
	pageKeys, err := s.docs.DocumentKeys(ctx, coord.FamilyPages, ClassPage)
	if err != nil {
		return 0, fmt.Errorf("pages: list the pages for the orphan sweep: %w", err)
	}
	live := map[string]bool{}
	for _, key := range pageKeys {
		if id, ok := PageIDOf(key); ok {
			live[id] = true
		}
	}

	var swept int

	// THE CLAIMS, read: a claim names its page in its value.
	claims, err := s.docs.Documents(ctx, coord.FamilyPages, ClassTitle)
	if err != nil {
		return 0, fmt.Errorf("pages: list the title claims for the orphan sweep: %w", err)
	}
	for _, rec := range claims {
		claim, err := DecodeClaim(rec.Value)
		if err != nil {
			log.WarnContext(ctx, "pages_claim_undecodable", "key", rec.Key,
				"error", err.Error(), "detail", "left in place")
			continue
		}
		if claim.PageID == "" || live[claim.PageID] || at.Sub(claim.CreatedAt) < ClaimGrace {
			continue
		}
		purged, err := s.purgeOrphan(ctx, rec.Key, rec.Version, ClassTitle, claim.PageID)
		if err != nil {
			return swept, err
		}
		if purged {
			swept++
		}
	}

	// THE CHILDREN, by key: both classes name their page in the key, so an
	// orphan is identifiable without reading anything.
	for _, class := range []string{ClassComment, ClassRevision} {
		keys, err := s.docs.DocumentKeys(ctx, coord.FamilyPages, class)
		if err != nil {
			return swept, fmt.Errorf("pages: list %q for the orphan sweep: %w", class, err)
		}
		for _, key := range keys {
			pageID, ok := PageIDOf(key)
			if !ok || pageID == "" || live[pageID] {
				continue
			}
			// READ ONLY NOW, and only for its timestamp: the grace is
			// what tells a crash's debris from a removal still in
			// flight, and it is a fact on the record rather than on the
			// key.
			rec, found, err := s.docs.Document(ctx, coord.FamilyPages, key)
			if err != nil {
				return swept, fmt.Errorf("pages: read the orphan %s: %w", key, err)
			}
			if !found {
				// Swept by a peer between the listing and this read.
				continue
			}
			written, ok := s.writtenAt(ctx, class, rec)
			if !ok || at.Sub(written) < ClaimGrace {
				continue
			}
			purged, err := s.purgeOrphan(ctx, key, rec.Version, class, pageID)
			if err != nil {
				return swept, err
			}
			if purged {
				swept++
			}
		}
	}
	return swept, nil
}

// writtenAt is when an orphaned child record was written, and false for one
// this build cannot decode — which is LEFT IN PLACE, on the same rule the
// change sweep follows: a record this build cannot read is one a peer wrote.
func (s *Sweeper) writtenAt(ctx context.Context, class string, rec coord.Record) (time.Time, bool) {
	switch class {
	case ClassComment:
		comment, err := DecodeComment(rec.Value)
		if err != nil {
			log.WarnContext(ctx, "pages_comment_undecodable", "key", rec.Key,
				"error", err.Error(), "detail", "left in place")
			return time.Time{}, false
		}
		return comment.CreatedAt, true
	case ClassRevision:
		rev, err := DecodeRevision(rec.Value)
		if err != nil {
			log.WarnContext(ctx, "pages_revision_undecodable", "key", rec.Key,
				"error", err.Error(), "detail", "left in place")
			return time.Time{}, false
		}
		return rev.CreatedAt, true
	}
	return time.Time{}, false
}

// purgeOrphan removes one record whose page is gone.
func (s *Sweeper) purgeOrphan(ctx context.Context, key string, version uint64,
	class, pageID string,
) (bool, error) {
	purged, err := s.docs.PurgeDocument(ctx, coord.FamilyPages, key, version)
	if err != nil {
		return false, fmt.Errorf("pages: purge the orphan %s: %w", key, err)
	}
	if !purged {
		return false, nil
	}
	log.InfoContext(ctx, "pages_orphan_swept", "key", key, "class", class, "page", pageID)
	return true, nil
}
