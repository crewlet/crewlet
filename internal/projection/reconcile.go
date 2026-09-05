package projection

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/store"
)

// reconcile establishes that this node's projection matches the bucket, then
// sets the cursor and hydrates.
//
// # Why this is a per-key comparison and not a cursor check
//
// The obvious pass is "read the cursor, resume the watch from it". It is
// wrong in a way that produces no error anywhere: a watch resumed from a
// revision ABOVE the stream's last sequence creates a consumer that delivers
// nothing and reports caught-up immediately. The node then sits at a
// plausible cursor over a projection that will never move again, and every
// screen it serves says the company has no work.
//
// It is not a corner case. It is what a cold restore from an older backup
// produces, what an in-memory bucket recreated at sequence 1 produces, and
// what cloning a node's data directory from a peer produces — three ordinary
// operator actions. A resume below the stream's FIRST sequence is the mirror
// image and skips silently.
//
// So every boot enumerates the bucket's keys with a metadata-only watch,
// compares them against [projection_keys], deletes what the bucket no longer
// holds, fetches exact revisions for what differs, and only then opens the
// live watch. It is O(keys) headers, which is why it says so while it runs.
func (p *Projector) reconcile(ctx context.Context) error {
	started := time.Now()

	// A DROPPED CHANGE MEANS THE ROWS ARE WRONG in a way this pass cannot
	// find by revision comparison alone — the drop may have been a purge
	// whose key still carries its old revision here. Reset the tables and
	// rebuild from the bucket's own contents.
	if dropped := p.buf.Dropped(); dropped > 0 {
		log.WarnContext(ctx, "projection_rebuild_after_drop",
			"family", string(p.family), "dropped", dropped,
			"detail", "the change buffer overflowed, so this projection has "+
				"missed writes it cannot name; rebuilding from the bucket")
		if err := p.reset(ctx); err != nil {
			return err
		}
	}
	p.buf.Reset()

	held, err := p.bucketKeys(ctx)
	if err != nil {
		return err
	}
	known, err := p.knownKeys(ctx)
	if err != nil {
		return err
	}

	var fetched, removed int
	head := uint64(0)
	for _, state := range held {
		if state.revision > head {
			head = state.revision
		}
	}

	// GONE FIRST, then changed. A key that is both gone and re-created
	// between the two passes is re-fetched by the second, where the other
	// order would apply the removal on top of the creation.
	for key, prior := range known {
		state, still := held[key]
		if still && !state.purged {
			continue
		}
		if prior.purged {
			// Already applied. Kept as a row so this pass can tell "we
			// removed it" from "we never saw it" without a fetch.
			continue
		}
		rev := prior.revision
		if still {
			rev = state.revision
		}
		if err := p.applyOne(ctx, &coord.Change{
			Key: key, Op: coord.OpPurge, Revision: rev, Initial: true,
		}); err != nil {
			return err
		}
		removed++
	}

	// SORTED BY THE APPLIER'S RANK. A map range is the order this pass
	// would otherwise take, and it puts a comment before its item often
	// enough that a fresh node projected twelve of a twenty-comment thread
	// — permanently, because the key set then records the skipped child as
	// applied. See [Applier.Order].
	pending := make([]string, 0, len(held))
	for key, state := range held {
		if state.purged {
			continue
		}
		if prior, ok := known[key]; ok && !prior.purged && prior.revision == state.revision {
			continue
		}
		pending = append(pending, key)
	}
	slices.SortFunc(pending, func(a, b string) int {
		if c := cmp.Compare(p.applier.Order(a), p.applier.Order(b)); c != 0 {
			return c
		}
		// Then by revision, so a re-run of the same reconcile applies in
		// the same order — a projection that depended on map iteration
		// would differ between two rebuilds of the same bucket.
		if c := cmp.Compare(held[a].revision, held[b].revision); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})

	noisy := time.NewTicker(reconcileNoise)
	defer noisy.Stop()
	for done, key := range pending {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-noisy.C:
			log.InfoContext(ctx, "projection_reconcile_running",
				"family", string(p.family), "keys", len(pending), "done", done,
				"fetched", fetched, "elapsed", time.Since(started).Round(time.Second),
				"detail", "a boot reconcile is O(keys) metadata reads; this node "+
					"claims no seats until it finishes")
		default:
		}
		if err := p.fetchAndApply(ctx, key, held[key].revision); err != nil {
			return err
		}
		fetched++
	}

	if err := p.setCursor(ctx, head, true); err != nil {
		return err
	}
	p.markHydrated(head)
	log.InfoContext(ctx, "projection_hydrated", "family", string(p.family),
		"keys", len(held), "fetched", fetched, "removed", removed,
		"revision", head, "took", time.Since(started).Round(time.Millisecond))
	return nil
}

// keyState is what the metadata pass learned about one key.
type keyState struct {
	revision uint64
	purged   bool
}

// bucketKeys enumerates the bucket with a metadata-only pass.
//
// From revision zero, so it sees every key the bucket holds rather than a
// tail — which is the entire point: a tail cannot say what is MISSING here.
func (p *Projector) bucketKeys(ctx context.Context) (map[string]keyState, error) {
	watcher, err := p.docs.WatchDocuments(ctx, p.family, 0)
	if err != nil {
		return nil, fmt.Errorf("projection: watch %s for reconcile: %w", p.family, err)
	}
	defer func() { _ = watcher.Stop() }()

	held := map[string]keyState{}
	changes := watcher.Changes()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case change, open := <-changes:
			if !open {
				// The watch ended before the caught-up marker. The map is
				// PARTIAL, and a partial map would have the pass delete
				// every key it did not reach — so it is refused rather
				// than used.
				return nil, errors.New(
					"projection: the reconcile watch closed before it caught up")
			}
			if change == nil {
				return held, nil
			}
			state := keyState{revision: change.Revision, purged: change.Op == coord.OpPurge}
			// LAST WRITE WINS within the pass. The opening pass delivers
			// one change per key, but a live write racing it delivers a
			// second, and the newer revision is the one to converge on.
			if prior, ok := held[change.Key]; ok && prior.revision > state.revision {
				continue
			}
			held[change.Key] = state
		}
	}
}

// knownKeys reads what this node has applied.
func (p *Projector) knownKeys(ctx context.Context) (map[string]keyState, error) {
	rows, err := p.db.SQL().QueryContext(ctx,
		`SELECT key, revision, purged FROM projection_keys WHERE family = ?`,
		string(p.family))
	if err != nil {
		return nil, fmt.Errorf("projection: read %s key set: %w", p.family, err)
	}
	defer rows.Close()
	known := map[string]keyState{}
	for rows.Next() {
		var (
			key    string
			rev    int64
			purged int
		)
		if err := rows.Scan(&key, &rev, &purged); err != nil {
			return nil, fmt.Errorf("projection: scan %s key set: %w", p.family, err)
		}
		known[key] = keyState{revision: uint64(rev), purged: purged != 0}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projection: read %s key set: %w", p.family, err)
	}
	return known, nil
}

// fetchAndApply reads one key at an exact revision and applies it.
//
// A NOT-FOUND HERE IS UNKNOWN, NEVER ABSENT. A direct get is answered by
// whichever replica takes it, from its own store, so a replica that has not
// applied this sequence yet answers "no such key" for a document the metadata
// pass just saw. Treating that as absence would delete a live record from
// this node's projection. It retries behind the sequence instead, and gives
// up loudly rather than guessing.
func (p *Projector) fetchAndApply(ctx context.Context, key string, revision uint64) error {
	var lastErr error
	for attempt := range fetchAttempts {
		rec, ok, err := p.docs.DocumentAt(ctx, p.family, key, revision)
		switch {
		case err != nil:
			lastErr = err
		case ok:
			return p.applyOne(ctx, &coord.Change{
				Key: key, Value: rec.Value, Op: coord.OpPut,
				Revision: rec.Version, Initial: true,
			})
		default:
			lastErr = fmt.Errorf(
				"revision %d is not visible on the replica that answered", revision)
		}
		if attempt+1 == fetchAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(fetchBeat << attempt):
		}
	}
	return fmt.Errorf("projection: fetch %s %s at revision %d: %w",
		p.family, key, revision, lastErr)
}

const (
	// fetchAttempts bounds the retry behind a replica that has not caught
	// up. Six, with the doubling below, spans about two seconds — which is
	// far past the milliseconds a replica takes to apply a sequence it has
	// already been sent, and short enough that a genuinely missing key
	// fails the reconcile rather than holding a boot open.
	fetchAttempts = 6

	// fetchBeat is the first pause, doubling per attempt.
	fetchBeat = 30 * time.Millisecond
)

// applyOne applies a single change in its own transaction. The reconcile's
// path; the follow loop batches instead.
func (p *Projector) applyOne(ctx context.Context, change *coord.Change) error {
	return p.applyBatch(ctx, []*coord.Change{change})
}

// applyBatch applies changes in ONE transaction and records their keys.
//
// SORTED BY THE APPLIER'S OWN RANK FIRST, which is what makes an apply's
// precondition "my parent is either already here or earlier in this same
// transaction" — see [Applier.Order] for the thread this silently truncated
// before it existed. Ties keep revision order, so a family with no hierarchy
// is applied exactly as it arrived.
//
// The cursor is NOT advanced here. It is written by the caller after the
// commit, so a crash between the two replays the batch — which is free,
// because an apply is idempotent by revision, where skipping it is not.
func (p *Projector) applyBatch(ctx context.Context, changes []*coord.Change) error {
	if len(changes) == 0 {
		return nil
	}
	changes = slices.Clone(changes)
	slices.SortStableFunc(changes, func(a, b *coord.Change) int {
		if c := cmp.Compare(p.applier.Order(a.Key), p.applier.Order(b.Key)); c != 0 {
			return c
		}
		return cmp.Compare(a.Revision, b.Revision)
	})
	if err := p.db.Tx(ctx, func(tx *sql.Tx) error {
		for _, change := range changes {
			if err := p.applier.Apply(ctx, tx, *change); err != nil {
				return applyError(p.family, change.Key, change.Revision, err)
			}
			if err := p.recordKey(ctx, tx, change); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	// AFTER THE COMMIT — see [Applier.Committed]. A failed batch does not
	// reach here, which is the point: a side effect fired for rows that
	// rolled back would have the registry re-read a page that was never
	// applied.
	p.applier.Committed(ctx)
	return nil
}

// recordKey writes what the reconcile compares against.
//
// In the SAME transaction as the rows it describes, which is what makes the
// two halves impossible to disagree: a crash cannot leave a row applied with
// its key unrecorded (the next reconcile would then re-fetch and re-apply it,
// harmlessly) or a key recorded with its row missing (which the next
// reconcile would skip, permanently).
func (p *Projector) recordKey(ctx context.Context, tx *sql.Tx, change *coord.Change) error {
	purged := 0
	if change.Op == coord.OpPurge {
		purged = 1
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO projection_keys (family, key, revision, purged)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (family, key) DO UPDATE SET
			revision = excluded.revision,
			purged   = excluded.purged`,
		string(p.family), change.Key, int64(change.Revision), purged)
	if err != nil {
		return fmt.Errorf("projection: record %s key %s: %w", p.family, change.Key, err)
	}
	return nil
}

// setCursor persists the cursor and the hydration flag.
func (p *Projector) setCursor(ctx context.Context, revision uint64, hydrated bool) error {
	flag := 0
	if hydrated {
		flag = 1
	}
	return p.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO projection_cursor (family, revision, hydrated, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (family) DO UPDATE SET
				revision   = MAX(projection_cursor.revision, excluded.revision),
				hydrated   = excluded.hydrated,
				updated_at = excluded.updated_at`,
			string(p.family), int64(revision), flag, store.EncodeTime(time.Now().UTC()))
		return err
	})
}

// reset drops this family's rows and its key set, for a rebuild.
//
// The CURSOR GOES TOO, and hydration with it: a reset projection that kept
// its cursor would resume a watch from a position describing rows that no
// longer exist.
func (p *Projector) reset(ctx context.Context) error {
	return p.db.Tx(ctx, func(tx *sql.Tx) error {
		if err := p.applier.Reset(ctx, tx); err != nil {
			return fmt.Errorf("projection: reset %s: %w", p.family, err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM projection_keys WHERE family = ?`, string(p.family)); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`DELETE FROM projection_cursor WHERE family = ?`, string(p.family))
		return err
	})
}
