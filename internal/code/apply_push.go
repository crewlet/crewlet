package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/store"
)

// applyPush applies a batch of pushes to one repository: each entry IN ORDER,
// a later entry seeing an earlier one's refs, inside the repository's
// horizons — its expired push history removed before the refs are written,
// its due tombstones after.
//
// # Two reads for the whole record, never one per entry or per command
//
// The apply transaction holds the replicated estate's write lock — each log
// has its own pinned writer, but they are writers of ONE file, so every other
// domain's applier on the node waits behind this transaction — and a record
// the committer fills carries nearly four thousand commands. So the push ids
// the record's entries carry are read in ONE statement over their primary key,
// the refs their ok commands name in ONE statement over the repository's
// primary-key range, and the ref state is carried across the entries IN
// MEMORY: the last value of a ref is the whole truth, since every row a record
// writes carries the record's one position and instant. (The repository's
// tombstone count is one more read, of one row by its key.)
//
// # The tombstones are bounded exactly, and refuse nobody
//
// A repository holds at most MaxTombstonesPerRepo tombstones after EVERY
// record, by two moves, each a pure function of the record and the rows. The
// sweep ([Applier.sweepTombstones]) takes the oldest past the bound as well
// as those past their horizon, within what the record's row budget leaves it:
// 3 995 rows behind a one-command push, and behind any record the committer
// forms at least one for every deletion the record carries, since the
// committer counts each deletion twice in a record's rows ([rowBound]) — and
// none at all behind a record it filled to the row bound with creations,
// which makes no tombstone. A record that would leave the count further past
// the bound than its sweep can take — a record no committer of this build
// forms, a newer build's or one formed by hand — writes its deletions as
// REMOVALS instead: each deleted ref's row is deleted rather than emptied,
// which is the one row its tombstone would have cost, so the budget and the
// counters stay exact, and the count after it is at most the count before.
// Its mark rises to its own position, since a cache synced below it would
// otherwise be handed a delta missing those deletions; such a cache — every
// one on every git node, since none has synced the record yet — reads the
// repository whole, which is why the committer leaves no record of its own
// needing it. Decided BEFORE a ref is written, from the counter and the
// record — never from a count a write already moved.
//
// # The counters are the rows, kept as the rows move
//
// code_repos counts what the repository's refs hold — live refs, the branches
// and tags among them, and tombstones — so no read counts them by walking a
// namespace: a hundred thousand refs is tens of milliseconds a COUNT, on every
// detail read, on whichever node answers. Each counter moves by the difference
// between every named ref's state before the record and after it, which is
// exact however many entries moved the ref in between.
//
// # An applied push never applies twice
//
// A push id already in code_pushes — a redelivery, a failover's second record,
// or a repeat inside this record, which no decide forms — is skipped rather
// than allowed to abort the insert on its primary key, which would stall the
// log on every node.
func (a *Applier) applyPush(ctx context.Context, tx *sql.Tx, at applyContext, p PushBatch) (int, error) {
	ids := make([]string, 0, len(p.Pushes))
	for _, e := range p.Pushes {
		ids = append(ids, e.PushID)
	}
	applied, err := keySet(ctx, tx, pushesAppliedSQL, ids)
	if err != nil {
		return 0, fmt.Errorf("code: read the applied pushes at %s: %w", at.position, err)
	}
	var named []string
	seen := map[string]bool{}
	for _, e := range p.Pushes {
		if applied[e.PushID] {
			continue
		}
		for _, c := range e.Commands {
			if c.Result == ResultOK && !seen[c.Ref] {
				seen[c.Ref] = true
				named = append(named, c.Ref)
			}
		}
	}
	prior, err := refsNamed(ctx, tx, p.RepoID, named)
	if err != nil {
		return 0, fmt.Errorf("code: read the refs the push at %s moves: %w", at.position, err)
	}

	var (
		final       = map[string]string{}
		pushRows    []PushEntry
		packRows    []PackRef
		packBytes   int64
		packObjects int64
	)
	for _, e := range p.Pushes {
		if applied[e.PushID] {
			continue
		}
		applied[e.PushID] = true
		for _, c := range e.Commands {
			if c.Result != ResultOK {
				continue
			}
			if c.New == ZeroOID {
				final[c.Ref] = ""
				continue
			}
			final[c.Ref] = c.New
		}
		if e.Pack != nil {
			packRows = append(packRows, *e.Pack)
			packBytes += e.Pack.Size
			packObjects += e.Pack.Objects
		}
		pushRows = append(pushRows, e)
	}
	if len(pushRows) == 0 {
		// A WHOLLY REDELIVERED BATCH writes nothing at all.
		return 0, nil
	}

	refs := make([]string, 0, len(final))
	for ref := range final {
		refs = append(refs, ref)
	}
	slices.Sort(refs)
	rows := len(refs) + len(packRows) + len(pushRows) + 1

	// THE PUSH HISTORY FIRST, before any ref is written: what it removes is
	// rows of the record's budget the tombstones cannot have, and the
	// tombstones' removal backstop is decided from that budget before the
	// refs it changes are written.
	expired, err := a.expirePushes(ctx, tx, at, p.RepoID, MaxRecordRows-rows)
	if err != nil {
		return 0, err
	}
	// THE MARK AND THE COUNTER ARE ONE ROW, written once the tombstones are
	// swept: the budget's last.
	sweep := MaxRecordRows - rows - expired - 1

	held, err := tombstonesHeld(ctx, tx, p.RepoID)
	if err != nil {
		return 0, fmt.Errorf("code: read the tombstones the push at %s finds: %w", at.position, err)
	}
	// HOW FAR PAST ITS BOUND the record would leave the tombstones were its
	// deletions tombstones: more than the sweep can take, and they are
	// removals.
	over := held + moves(prior, final, false).tombstones - MaxTombstonesPerRepo
	removal := over > max(sweep, 0)
	moved := moves(prior, final, removal)
	var kept, removed []string
	for _, ref := range refs {
		if removal && final[ref] == "" {
			removed = append(removed, ref)
			continue
		}
		kept = append(kept, ref)
	}
	if _, err = store.InsertRows(ctx, tx, at.maxVariables,
		`INSERT INTO code_refs (repo_id, ref, target, position, at) VALUES `, `(?, ?, ?, ?, ?)`,
		`ON CONFLICT (repo_id, ref) DO UPDATE SET target = excluded.target, position = excluded.position, at = excluded.at`,
		len(kept), func(i int) []any {
			return []any{p.RepoID, kept[i], final[kept[i]], at.packed, at.at()}
		}); err != nil {
		return 0, fmt.Errorf("code: write the refs at %s: %w", at.position, err)
	}
	if len(removed) > 0 {
		list, listErr := jsonList(removed)
		if listErr != nil {
			return 0, listErr
		}
		if _, err = tx.ExecContext(ctx, deleteRefsSQL, p.RepoID, list); err != nil {
			return 0, fmt.Errorf("code: remove the refs deleted at %s: %w", at.position, err)
		}
	}
	if _, err = store.InsertRows(ctx, tx, at.maxVariables,
		`INSERT INTO code_packs (repo_id, pack_id, part, object, hash, size, index_bytes,
			reverse_bytes, git_checksum, objects, kind, state, added_position) VALUES `,
		`(?, ?, 'pack', ?, ?, ?, ?, ?, ?, ?, 'push', 'live', ?)`,
		`ON CONFLICT (repo_id, pack_id, part) DO NOTHING`,
		len(packRows), func(i int) []any {
			k := packRows[i]
			return []any{p.RepoID, k.PackID, k.Object.String(), string(k.Hash), k.Size,
				k.IndexBytes, k.ReverseBytes, k.GitChecksum, k.Objects, at.packed}
		}); err != nil {
		return 0, fmt.Errorf("code: write the packs at %s: %w", at.position, err)
	}
	// THE ROW'S WIDTH IS WRITTEN BESIDE IT: the bytes of its two wide
	// columns, which is what deleting it costs and what the history's
	// horizon and a purge part are bounded by.
	encoded := make([]string, len(pushRows))
	introduced := make([]string, len(pushRows))
	for i, e := range pushRows {
		if encoded[i], err = recordedCommands(e.Commands); err != nil {
			return 0, err
		}
		introduced[i] = strings.Join(e.Introduced, " ")
	}
	if _, err = store.InsertRows(ctx, tx, at.maxVariables,
		`INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, actor_seat,
			node, atomic, commands, pack_id, pack_bytes, introduced, introduced_count,
			received_at, row_bytes) VALUES `,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, ``,
		len(pushRows), func(i int) []any {
			e := pushRows[i]
			packID, size := "", int64(0)
			if e.Pack != nil {
				packID, size = e.Pack.PackID, e.Pack.Size
			}
			return []any{e.PushID, p.RepoID, at.packed, at.at(), e.Principal.OperatorID,
				string(e.Principal.Kind), e.Principal.Seat, e.Node, e.Atomic, encoded[i],
				packID, size, introduced[i], e.IntroducedCount,
				store.EncodeTime(e.ReceivedAt), len(encoded[i]) + len(introduced[i])}
		}); err != nil {
		return 0, fmt.Errorf("code: write the pushes at %s: %w", at.position, err)
	}
	// A REMOVAL RAISES THE MARK TO THE RECORD'S OWN POSITION, in the counters'
	// own row: a delta from below it would miss the deletions it removed.
	var mark int64
	if removal {
		mark = at.packed
	}
	if _, err = tx.ExecContext(ctx, pushCountersSQL, moved.live, moved.branches, moved.tags,
		moved.tombstones, len(packRows), packBytes, packObjects, len(pushRows), at.at(), at.packed,
		mark, p.RepoID); err != nil {
		return 0, fmt.Errorf("code: count the pushes at %s: %w", at.position, err)
	}

	swept, err := a.sweepTombstones(ctx, tx, at, p.RepoID, sweep,
		held+moved.tombstones-MaxTombstonesPerRepo)
	if err != nil {
		return 0, err
	}
	return rows + expired + swept, nil
}

// moves is what the record does to the repository's counts: every ref it
// names from its state before the record (prior, absent for a ref with no
// row) to its state after (final, "" for a deletion). A deletion is a
// TOMBSTONE, or with removal a ref with NO ROW, which counts nothing.
func moves(prior, final map[string]string, removal bool) refCounts {
	var moved refCounts
	for ref, target := range final {
		if before, had := prior[ref]; had {
			moved.count(ref, before, -1)
		}
		if target != "" || !removal {
			moved.count(ref, target, +1)
		}
	}
	return moved
}

// tombstonesHeld is the tombstones the repository's row counts, and none for a
// repository with no row — a record about one is applied as the rows it
// writes, never refused, since an error here would stall the log on every node.
func tombstonesHeld(ctx context.Context, tx *sql.Tx, repoID string) (int, error) {
	var held int
	err := tx.QueryRowContext(ctx, tombstonesHeldSQL, repoID).Scan(&held)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return held, err
}

// refCounts is what a repository's refs hold, as code_repos counts it.
type refCounts struct {
	live, branches, tags, tombstones int
}

// count adds sign times one ref holding target — a tombstone when target is
// empty — to the counts.
func (c *refCounts) count(ref, target string, sign int) {
	if target == "" {
		c.tombstones += sign
		return
	}
	c.live += sign
	switch {
	case strings.HasPrefix(ref, branchPrefix):
		c.branches += sign
	case strings.HasPrefix(ref, tagPrefix):
		c.tags += sign
	}
}

// recordedCommands is the commands a push row stores: every one with its
// result, and no Detail — a sentence is an answer, not state.
func recordedCommands(cmds []Command) (string, error) {
	stored := make([]Command, len(cmds))
	for i, c := range cmds {
		c.Detail = ""
		stored[i] = c
	}
	b, err := json.Marshal(stored)
	if err != nil {
		return "", fmt.Errorf("code: encode a push's commands: %w", err)
	}
	return string(b), nil
}

// expirePushes removes, inside the record's own repository and within the
// rows the record has left of MaxRecordRows (budget), its push rows older than
// PushHistory by the record's broker instant — at most HorizonDeletesPerRecord
// of them, and no more once their bytes reach HorizonBytesPerRecord — and
// answers how many it removed.
//
// OLDEST BY POSITION, STOPPING AT THE FIRST INSIDE THE HORIZON. The rows are
// read in log order (code_pushes_repo_idx, replicated 0056) and taken while
// their instant is before the horizon, and the first row that is not ends the
// read. NEVER A FILTER ON THE INSTANT: the index orders by position, so `at <
// ?` seeks nothing there, and over position order it would walk the
// repository's whole history — 181 days of pushes — on every push record
// that found fewer than its LIMIT expired, to find the few that were.
// Stopped, the read costs the rows it removes and one more. A record's
// instant is the broker's, and a stream leader's change can stamp a later
// record earlier; such a row waits for the one before it, at most the
// members' clock skew past its own horizon — and every node, holding the
// same rows in the same order, removes the same ones, never one inside the
// horizon.
func (a *Applier) expirePushes(ctx context.Context, tx *sql.Tx, at applyContext, repoID string,
	budget int) (int, error) {

	limit := min(HorizonDeletesPerRecord, budget)
	if limit <= 0 {
		return 0, nil
	}
	horizon := store.EncodeTime(at.brokerAt.Add(-PushHistory))
	expired, _, err := pushesWithin(ctx, tx, HorizonBytesPerRecord,
		func(instant int64) bool { return instant >= horizon }, expiredPushesSQL, repoID, limit)
	if err != nil {
		return 0, fmt.Errorf("code: read %s's expired pushes: %w", repoID, err)
	}
	if len(expired) == 0 {
		return 0, nil
	}
	list, err := jsonList(expired)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, deletePushesSQL, list)
	if err != nil {
		return 0, fmt.Errorf("code: delete %s's expired pushes: %w", repoID, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// sweepTombstones removes, inside the record's own repository and within limit
// rows, its tombstones oldest first — by deletion instant, then ref — each
// that is among the first excess or older than RefTombstoneHorizon by the
// record's broker instant, stopping at the first that is neither; and raises
// tombstones_through to the highest position a swept tombstone held, so a
// delta from below it is answered in full. It answers the rows it wrote: the
// tombstones and the mark.
//
// # Two reasons a tombstone goes, one pass
//
// AGE: a tombstone is kept so a cache syncing a delta learns of the deletion,
// and a cache synced longer ago than RefTombstoneHorizon is answered whole
// instead. COUNT: excess is how far the record left the repository past
// MaxTombstonesPerRepo, and the oldest go first, because the cache a forgotten
// deletion sends to a full answer is one synced before it, and the longer ago
// that was, the likelier it is idle — and a full answer re-reads the rows and
// reuses every pack it holds, never a download. Both read the one order
// code_refs_tombstones_idx keeps, so the pass is one seek and a scan that ends
// at the first tombstone it keeps: one row read when nothing is due.
//
// # The tombstones take everything the record left
//
// A record adds at most MaxPushesPerRecord push rows, which a fixed share of
// HorizonDeletesPerRecord keeps pace with. It can make nearly two thousand
// TOMBSTONES behind a record this build's committer forms, every deletion
// counted twice in its rows — and behind one formed otherwise, a newer
// build's or one written by hand, as many as one append carries, since
// nothing at the apply holds a record to this build's committer's bounds:
// some sixty thousand deletions of short refs in the log's largest record
// (CodeLogMaxRecordBytes), which is why the removal backstop takes whatever
// the record carries. A fixed share of those falls behind any repository
// whose pushes delete refs in bulk — a CI deleting its build tags, a branch
// clean-up: measured, records that each made a thousand refs and deleted the
// previous thousand grew the table by about 936 tombstones a record with a
// share of 64, for ever. The rest of the row budget is room the framework's transaction bound
// already pays for, and every node computes the same budget from the same
// record and the same rows, so the sweep stays a pure function of the record:
// 3 995 tombstones behind a one-command push, at least as many as its own
// deletions behind any record a committer forms ([rowBound]), and none behind
// one it filled to the row bound with creations. That is what makes the COUNT
// exact with one backstop no record of this build's committer reaches: a
// record whose excess is more than its limit writes its deletions as removals
// instead ([Applier.applyPush]), so excess here never passes limit.
func (a *Applier) sweepTombstones(ctx context.Context, tx *sql.Tx, at applyContext, repoID string,
	limit, excess int) (int, error) {

	if limit <= 0 {
		return 0, nil
	}
	aged := store.EncodeTime(at.brokerAt.Add(-RefTombstoneHorizon))
	rows, err := tx.QueryContext(ctx, tombstonesOldestSQL, repoID, limit)
	if err != nil {
		return 0, fmt.Errorf("code: read %s's oldest tombstones: %w", repoID, err)
	}
	var stones []string
	var through int64
	for rows.Next() {
		var ref string
		var position, deleted int64
		if scanErr := rows.Scan(&ref, &position, &deleted); scanErr != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("code: read %s's oldest tombstones: %w", repoID, scanErr)
		}
		if len(stones) >= excess && deleted >= aged {
			break
		}
		stones = append(stones, ref)
		through = max(through, position)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("code: read %s's oldest tombstones: %w", repoID, err)
	}
	_ = rows.Close()
	if len(stones) == 0 {
		return 0, nil
	}
	list, err := jsonList(stones)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, deleteRefsSQL, repoID, list)
	if err != nil {
		return 0, fmt.Errorf("code: delete %s's swept tombstones: %w", repoID, err)
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, sweptTombstonesSQL, through, n, repoID); err != nil {
		return 0, fmt.Errorf("code: raise %s's tombstone mark: %w", repoID, err)
	}
	return int(n) + 1, nil
}

// applyPurgePart deletes one bounded part of a deleted repository's rows, in a
// fixed order — its pushes, then its refs, then its packs, then the packs a
// record retired — so every node deletes the same rows. THE PART IS THE
// GUARD: only the row's purge_parts + 1 deletes anything, so a redelivered or
// stale part deletes nothing more.
//
// BOUNDED BY ROWS AND BY BYTES: at most PurgePartRows rows, and push rows
// only until their row_bytes reach PurgePartBytes — a push row is the
// estate's one wide row, and deleting one costs its overflow pages, which a
// count does not see. A part whose pushes were cut by bytes deletes nothing
// else: the refs and packs wait for a part whose pushes are done.
//
// A RETIRED PACK'S ROW GOES WITH ITS REPOSITORY, its grace or not: the grace
// is for a reader that began on the pack before it was retired, and no read
// begins on a deleted repository — it is refused to every new one from its
// deletion, and code.deleted_retention_days, a day at least, outlasts the
// reads that had begun. Its object is then named by nothing, and the
// collector takes it as it takes the repository's live packs'.
func (a *Applier) applyPurgePart(ctx context.Context, tx *sql.Tx, at applyContext, p PurgePart) (int, error) {
	var parts int
	if err := tx.QueryRowContext(ctx, purgePartsSQL, p.RepoID).Scan(&parts); err != nil {
		return 0, fmt.Errorf("code: read %s's purge at %s: %w", p.RepoID, at.position, err)
	}
	if p.Part != parts+1 {
		return 0, nil
	}
	budget := PurgePartRows
	deleted := 0

	pushIDs, cut, err := pushesWithin(ctx, tx, PurgePartBytes, nil, purgePushesSQL, p.RepoID, budget)
	if err != nil {
		return 0, fmt.Errorf("code: read %s's pushes to purge: %w", p.RepoID, err)
	}
	if len(pushIDs) > 0 {
		list, err := jsonList(pushIDs)
		if err != nil {
			return 0, err
		}
		res, err := tx.ExecContext(ctx, deletePushesSQL, list)
		if err != nil {
			return 0, fmt.Errorf("code: purge %s's pushes: %w", p.RepoID, err)
		}
		n, _ := res.RowsAffected()
		deleted += int(n)
		budget -= len(pushIDs)
	}
	if cut {
		budget = 0
	}

	var refs []string
	var gone refCounts
	if budget > 0 {
		rows, err := tx.QueryContext(ctx, purgeRefsSQL, p.RepoID, budget)
		if err != nil {
			return 0, fmt.Errorf("code: read %s's refs to purge: %w", p.RepoID, err)
		}
		for rows.Next() {
			var ref, target string
			if err := rows.Scan(&ref, &target); err != nil {
				_ = rows.Close()
				return 0, fmt.Errorf("code: read %s's refs to purge: %w", p.RepoID, err)
			}
			refs = append(refs, ref)
			gone.count(ref, target, +1)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("code: read %s's refs to purge: %w", p.RepoID, err)
		}
		_ = rows.Close()
	}
	if len(refs) > 0 {
		list, err := jsonList(refs)
		if err != nil {
			return 0, err
		}
		res, err := tx.ExecContext(ctx, deleteRefsSQL, p.RepoID, list)
		if err != nil {
			return 0, fmt.Errorf("code: purge %s's refs: %w", p.RepoID, err)
		}
		n, _ := res.RowsAffected()
		deleted += int(n)
		budget -= len(refs)
	}

	var packIDs []string
	livePacksGone, liveBytesGone, liveObjectsGone := 0, int64(0), int64(0)
	if budget > 0 {
		rows, err := tx.QueryContext(ctx, purgePacksSQL, p.RepoID, budget)
		if err != nil {
			return 0, fmt.Errorf("code: read %s's packs to purge: %w", p.RepoID, err)
		}
		for rows.Next() {
			var id, state string
			var size, objects int64
			if err := rows.Scan(&id, &size, &objects, &state); err != nil {
				_ = rows.Close()
				return 0, fmt.Errorf("code: read %s's packs to purge: %w", p.RepoID, err)
			}
			packIDs = append(packIDs, id)
			if state == packLive {
				livePacksGone++
				liveBytesGone += size
				liveObjectsGone += objects
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("code: read %s's packs to purge: %w", p.RepoID, err)
		}
		_ = rows.Close()
	}
	if len(packIDs) > 0 {
		list, err := jsonList(packIDs)
		if err != nil {
			return 0, err
		}
		res, err := tx.ExecContext(ctx, deletePacksSQL, p.RepoID, list)
		if err != nil {
			return 0, fmt.Errorf("code: purge %s's packs: %w", p.RepoID, err)
		}
		n, _ := res.RowsAffected()
		// ONE PART A PACK is what makes deleting by pack id exact: every
		// pack this build stores is one bundle, one row. A pack split
		// across rows would be deleted whole here while the budget counted
		// one of them — a writer's bug, refused rather than miscounted.
		if int(n) != len(packIDs) {
			return 0, fmt.Errorf("code: purging %s deleted %d pack rows for %d packs — "+
				"a pack this build stores is one row", p.RepoID, n, len(packIDs))
		}
		deleted += int(n)
		budget -= len(packIDs)
	}
	if budget > 0 {
		res, err := tx.ExecContext(ctx, purgeRetiredSQL, p.RepoID, budget)
		if err != nil {
			return 0, fmt.Errorf("code: purge %s's retired packs: %w", p.RepoID, err)
		}
		n, _ := res.RowsAffected()
		deleted += int(n)
	}
	if _, err := tx.ExecContext(ctx, purgeCountersSQL, p.Part, gone.live, gone.branches, gone.tags,
		gone.tombstones, livePacksGone, liveBytesGone, liveObjectsGone, at.packed, p.RepoID); err != nil {
		return 0, fmt.Errorf("code: count %s's purge at %s: %w", p.RepoID, at.position, err)
	}
	return deleted + 1, nil
}

// applyPurge removes what remains of a repository and writes its deletion
// marker — THE GATE. Applied whatever the row's status (the decide is the
// guard, and the applier never refuses ordinary traffic) and idempotent: a
// redelivery of the marker's own record, which its gate lets through by op id,
// deletes nothing and inserts nothing. Unbounded statements are safe because
// the decide forms a purge only once at most PurgePartRows of the
// repository's pushes, refs, packs and retired packs remain AND the pushes
// among them weigh at most PurgePartBytes — a part's own bounds — and it holds
// at most MaxProtectionsPerRepo rules: with the name claim, the row and the
// marker that is PurgePartRows + MaxProtectionsPerRepo + 3 rows, which the
// limits test holds within MaxRecordRows.
func (a *Applier) applyPurge(ctx context.Context, tx *sql.Tx, at applyContext, p Purge) (int, error) {
	deleted := 0
	for _, stmt := range []string{
		purgeAllPushesSQL, purgeAllRefsSQL, purgeAllPacksSQL, purgeAllRetiredSQL, clearProtectionsSQL,
	} {
		res, err := tx.ExecContext(ctx, stmt, p.RepoID)
		if err != nil {
			return 0, fmt.Errorf("code: purge %s at %s: %w", p.RepoID, at.position, err)
		}
		n, _ := res.RowsAffected()
		deleted += int(n)
	}
	// THE RECORD STATES THE NAME IT RELEASES, and only a claim naming THIS
	// repository is released: a claim another repository holds on the same
	// token is not this purge's to remove.
	res, err := tx.ExecContext(ctx, releaseNameSQL, NameToken(p.Name), p.RepoID)
	if err != nil {
		return 0, fmt.Errorf("code: release %q at %s: %w", p.Name, at.position, err)
	}
	n, _ := res.RowsAffected()
	deleted += int(n)
	if res, err = tx.ExecContext(ctx, removeRepoSQL, p.RepoID); err != nil {
		return 0, fmt.Errorf("code: remove %s at %s: %w", p.RepoID, at.position, err)
	}
	n, _ = res.RowsAffected()
	deleted += int(n)
	if res, err = tx.ExecContext(ctx, markSQL, p.RepoID, p.Name, at.at(), at.record.Actor,
		p.Reason, at.record.OpID, at.packed); err != nil {
		return 0, fmt.Errorf("code: mark %s purged at %s: %w", p.RepoID, at.position, err)
	}
	n, _ = res.RowsAffected()
	return deleted + int(n), nil
}

// packLive is a pack row's state while a repository names it.
const packLive = "live"

// keySet reads the keys among keys a statement over json_each finds.
func keySet(ctx context.Context, tx *sql.Tx, query string, keys []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(keys) == 0 {
		return out, nil
	}
	list, err := jsonList(keys)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, query, list)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
}

// pushesWithin reads the push ids query answers with their row_bytes, in its
// order, and keeps them until their bytes reach bytes — always the first,
// whatever it weighs, so a step always advances, and so at most one row past
// the bound. cut reports that it left a row the query answered: the pushes
// were bounded by what they weigh rather than by the query's LIMIT. Every
// node holds the same rows with the same widths, so every node keeps the
// same ids.
//
// stop, where it is given, ends the read at the first row it holds for: the
// query then selects a THIRD column, which stop is asked of, and the row it
// holds for is not kept — how the push-history horizon ends at the first push
// inside it, in the one order every node reads (expirePushes), where a filter
// in the statement would walk every push the repository keeps. A read ended by
// stop is not cut: what bounded it is the rows' age, not their weight. The
// purge passes none and keeps its two-column statement.
func pushesWithin(ctx context.Context, tx *sql.Tx, bytes int64, stop func(int64) bool, query string,
	args ...any) (ids []string, cut bool, err error) {

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	var weighed int64
	for rows.Next() {
		if weighed >= bytes {
			return ids, true, nil
		}
		var id string
		var width, third int64
		dest := []any{&id, &width}
		if stop != nil {
			dest = append(dest, &third)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, false, err
		}
		if stop != nil && stop(third) {
			return ids, false, nil
		}
		ids = append(ids, id)
		weighed += width
	}
	return ids, false, rows.Err()
}

// refsNamed is what the repository holds of each of refs — its target, empty
// for a tombstone, and absent from the map where the repository has no row —
// in ONE statement over the primary key.
func refsNamed(ctx context.Context, tx *sql.Tx, repoID string, refs []string) (map[string]string, error) {
	out := map[string]string{}
	if len(refs) == 0 {
		return out, nil
	}
	list, err := jsonList(refs)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, refsNamedSQL, repoID, list)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var ref, target string
		if err := rows.Scan(&ref, &target); err != nil {
			return nil, err
		}
		out[ref] = target
	}
	return out, rows.Err()
}

// The push apply's and the purge's statements: every one with a WHERE is
// registered in plan_test.go, so the pinned driver's planner is held to the
// seek each is written for — and planner.go says why a batch is driven from
// its list rather than tested with IN.
const (
	pushesAppliedSQL = `SELECT push_id FROM code_pushes
		WHERE push_id IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
	refsNamedSQL = `SELECT r.ref, r.target FROM json_each(?2) AS j CROSS JOIN code_refs AS r
		WHERE r.repo_id = ?1 AND r.ref = CAST(j.value AS TEXT)`
	pushCountersSQL = `UPDATE code_repos SET refs = refs + ?, branches = branches + ?,
		tags = tags + ?, tombstones = tombstones + ?, live_packs = live_packs + ?,
		live_bytes = live_bytes + ?, live_objects = live_objects + ?, pushes = pushes + ?,
		last_push_at = ?, version = ?, tombstones_through = MAX(tombstones_through, ?) WHERE id = ?`
	tombstonesHeldSQL = `SELECT tombstones FROM code_repos WHERE id = ?`
	// expiredPushesSQL reads a repository's oldest pushes in log order with
	// the instant expirePushes stops at — no filter on it: see there.
	expiredPushesSQL = `SELECT push_id, row_bytes, at FROM code_pushes WHERE repo_id = ?
		ORDER BY position, push_id LIMIT ?`
	deletePushesSQL = `DELETE FROM code_pushes
		WHERE push_id IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
	tombstonesOldestSQL = `SELECT ref, position, at FROM code_refs WHERE repo_id = ? AND target = ''
		ORDER BY at, ref LIMIT ?`
	deleteRefsSQL = `DELETE FROM code_refs WHERE rowid IN (SELECT r.rowid
		FROM json_each(?2) AS j CROSS JOIN code_refs AS r
		WHERE r.repo_id = ?1 AND r.ref = CAST(j.value AS TEXT))`
	sweptTombstonesSQL = `UPDATE code_repos SET tombstones_through = MAX(tombstones_through, ?),
		tombstones = tombstones - ? WHERE id = ?`
	purgePartsSQL  = `SELECT purge_parts FROM code_repos WHERE id = ?`
	purgePushesSQL = `SELECT push_id, row_bytes FROM code_pushes WHERE repo_id = ?
		ORDER BY position, push_id LIMIT ?`
	purgeRefsSQL  = `SELECT ref, target FROM code_refs WHERE repo_id = ? ORDER BY ref LIMIT ?`
	purgePacksSQL = `SELECT pack_id, size, objects, state FROM code_packs WHERE repo_id = ?
		ORDER BY pack_id, part LIMIT ?`
	deletePacksSQL = `DELETE FROM code_packs WHERE rowid IN (SELECT p.rowid
		FROM json_each(?2) AS j CROSS JOIN code_packs AS p
		WHERE p.repo_id = ?1 AND p.pack_id = CAST(j.value AS TEXT))`
	// purgeRetiredSQL is a part's retired packs, oldest retirement first and
	// no more than the part has left — the sweep's own order.
	purgeRetiredSQL = `DELETE FROM code_packs_retired WHERE rowid IN (SELECT rowid
		FROM code_packs_retired INDEXED BY code_packs_retired_repo_idx WHERE repo_id = ?
		ORDER BY retired_position, pack_id, part LIMIT ?)`
	purgeCountersSQL = `UPDATE code_repos SET status = 'purging', purge_parts = ?, refs = refs - ?,
		branches = branches - ?, tags = tags - ?, tombstones = tombstones - ?,
		live_packs = live_packs - ?, live_bytes = live_bytes - ?, live_objects = live_objects - ?,
		version = ? WHERE id = ?`
	releaseNameSQL     = `DELETE FROM code_names WHERE token = ? AND repo_id = ?`
	purgeAllPushesSQL  = `DELETE FROM code_pushes WHERE repo_id = ?`
	purgeAllRefsSQL    = `DELETE FROM code_refs WHERE repo_id = ?`
	purgeAllPacksSQL   = `DELETE FROM code_packs WHERE repo_id = ?`
	purgeAllRetiredSQL = `DELETE FROM code_packs_retired INDEXED BY code_packs_retired_repo_idx
		WHERE repo_id = ?`
	removeRepoSQL = `DELETE FROM code_repos WHERE id = ?`
	markSQL       = `INSERT INTO code_deletions
			(repo_id, name, at, by, reason, purge_record_id, rejects, last_reject_at, version)
		VALUES (?, ?, ?, ?, ?, ?, 0, 0, ?)
		ON CONFLICT (repo_id) DO NOTHING`
)
