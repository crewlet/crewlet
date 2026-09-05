package work

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// The tracker's retention, and what is deliberately NOT swept.
//
// # Items and comments are kept for ever
//
// A closed item is the company's record of what it decided and why, and the
// question it answers — "have we seen this before, and what did we do?" — is
// the one a tracker exists for. Nothing here ages an item out, and a company
// that wants one gone removes it explicitly.
//
// # Changes are not
//
// A change key is written on EVERY edit: a status flip, a comment, a
// reassignment. An item worked for a month carries hundreds, and each is a
// key in the broker's per-subject index on every member for the life of the
// deployment. They are what a feed redelivery is deduplicated against and
// what an audit walks, and both of those have a horizon; the item's own head
// carries the current state, so nothing a board or a tool reads goes with
// them.
//
// # Why this is a duty and not a bucket age
//
// The documents bucket holds items, comments, changes and counters TOGETHER,
// under one grammar, because they are one family with one watch and one
// projection. A bucket MaxAge cannot distinguish them — it would expire the
// items too — so the horizon that applies to one class is applied by a
// fleet singleton that reads the class off the key.

// ChangeRetention is how long a change record survives.
//
// A YEAR. Two things need it and neither needs longer: a feed redelivery is
// deduplicated against the change id, which no broker will replay past its
// own retention (days, not months), and an audit walks it to answer "who
// changed this and when" — a question asked about the last quarter, at the
// outside about the last year, and answerable from the head and the comment
// thread after that.
//
// The cost of a longer horizon is not storage, which is small, but the
// per-subject index every cluster member holds: a company writing a thousand
// changes a day carries 365,000 live keys at this horizon and would carry a
// million at three years, for records nothing reads.
const ChangeRetention = 365 * 24 * time.Hour

// OrphanGrace is how long a comment whose item is gone is left alone.
//
// AN HOUR, and generously so: [Store.Remove] writes the change key first and
// purges the comments before the head, so a crash mid-remove leaves comments
// under an item that still exists — not an orphan at all. The orphan case is
// the reverse, and rarer: a comment written concurrently with the removal
// that landed after the thread was swept. An hour is far longer than any
// write that could still be in flight, which is what keeps this from racing
// a slow but healthy writer.
const OrphanGrace = time.Hour

// Sweeper is the retention pass over the tracker's own records.
//
// SEPARATE FROM [Store], and deliberately: a store is what a seat writes
// through and every node holds one, while this runs as a FLEET SINGLETON —
// N nodes purging the same keys is waste rather than corruption, but it is
// still waste, and a purge that lost its race logs a conflict nobody should
// be reading.
type Sweeper struct {
	docs Documents
	now  func() time.Time
}

// NewSweeper builds the tracker's retention pass.
func NewSweeper(docs Documents, now func() time.Time) *Sweeper {
	if now == nil {
		now = nowUTC
	}
	return &Sweeper{docs: docs, now: now}
}

// SweepChanges purges change records older than cutoff, reporting how many
// went.
//
// The cutoff is compared against the record's OWN CreatedAt rather than
// against the time-ordered id it is keyed by. The id orders them; the record
// says when. A build that changed how ids are minted would silently change
// what this deleted if it read the key.
func (s *Sweeper) SweepChanges(ctx context.Context, cutoff time.Time) (int, error) {
	records, err := s.docs.Documents(ctx, coord.FamilyWork, "")
	if err != nil {
		return 0, fmt.Errorf("work: list the record for the retention sweep: %w", err)
	}
	var swept int
	for _, rec := range records {
		if class, ok := ClassOf(rec.Key); !ok || class != ClassChange {
			continue
		}
		change, err := DecodeChange(rec.Value)
		if err != nil {
			// A change this build cannot decode is LEFT, not deleted. It
			// was written by a peer whose format this one does not know,
			// and deleting what you cannot read is how a rolling upgrade
			// loses a newer node's records.
			log.WarnContext(ctx, "work_change_undecodable", "key", rec.Key,
				"error", err.Error(),
				"detail", "left in place; a record this build cannot read is "+
					"not a record it may delete")
			continue
		}
		if !change.CreatedAt.Before(cutoff) {
			continue
		}
		if _, err := s.docs.PurgeDocument(ctx, coord.FamilyWork, rec.Key, rec.Version); err != nil {
			return swept, fmt.Errorf("work: purge the change %s: %w", rec.Key, err)
		}
		swept++
	}
	return swept, nil
}

// SweepOrphans purges comments whose item is gone, reporting how many went.
//
// THE ITEM SET IS BUILT FROM THE SAME LISTING, not from a per-comment read.
// One listing is one pass over the family; a read per comment would be one
// round trip per comment on a company with a hundred thousand of them, on a
// duty that finds nothing on almost every run.
func (s *Sweeper) SweepOrphans(ctx context.Context, at time.Time) (int, error) {
	records, err := s.docs.Documents(ctx, coord.FamilyWork, "")
	if err != nil {
		return 0, fmt.Errorf("work: list the record for the orphan sweep: %w", err)
	}
	items := map[string]bool{}
	for _, rec := range records {
		if class, ok := ClassOf(rec.Key); ok && class == ClassItem {
			if id, ok := ItemIDOf(rec.Key); ok {
				items[id] = true
			}
		}
	}
	var swept int
	for _, rec := range records {
		class, ok := ClassOf(rec.Key)
		if !ok || class != ClassComment {
			continue
		}
		itemID, ok := ItemIDOf(rec.Key)
		if !ok || items[itemID] {
			continue
		}
		comment, err := DecodeComment(rec.Value)
		if err != nil {
			log.WarnContext(ctx, "work_comment_undecodable", "key", rec.Key,
				"error", err.Error(), "detail", "left in place")
			continue
		}
		if at.Sub(comment.CreatedAt) < OrphanGrace {
			// Inside the grace, a comment written concurrently with a
			// removal and one left by a crash look identical — and
			// deleting the first is deleting somebody's live write.
			continue
		}
		if _, err := s.docs.PurgeDocument(ctx, coord.FamilyWork, rec.Key, rec.Version); err != nil {
			return swept, fmt.Errorf("work: purge the orphan comment %s: %w", rec.Key, err)
		}
		log.InfoContext(ctx, "work_orphan_comment_swept", "key", rec.Key, "item", itemID)
		swept++
	}
	return swept, nil
}
