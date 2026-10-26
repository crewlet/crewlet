package code

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/store"
)

// applyCompact applies one compaction ([Compaction]): the rows of the packs it
// retires leave code_packs for code_packs_retired, stamped with the record's
// position and broker instant, the outputs code_packs does not hold are written
// there, and the repository's counters move by exactly what moved — in the one
// transaction the framework holds a record's apply to.
//
// PURE IN THE RECORD, ITS POSITION AND ITS INSTANT, like every apply here: it
// reads no clock and no configuration and decides nothing again. The decide
// already held every input live, every output a bundle and the objects to what
// the inputs held, and the broker's arbitration on the repository's subject
// guarantees the record applies against the rows that decide read. So every
// guard here is a SKIP, never a refusal, which would stop the log on every
// node:
//
//   - a pack id the record retires that code_packs no longer holds is passed
//     over — a replay of a record whose packs a later one has since moved, or
//     a record a newer build formed — and every row of one it holds moves,
//     whatever its part;
//   - an output code_packs already holds is not written again;
//   - and a record that finds nothing to retire and nothing to add has
//     applied already, and writes nothing at all.
//
// THE COUNTERS ARE THE ROWS: live_packs, live_bytes and live_objects move by
// the rows this apply found and wrote — the live ones among those it retires,
// as a purge part counts them — never by the record's lists, so a replay over
// rows it already moved moves nothing, and an input named twice counts once.
// Nothing else is touched: no ref, no push row, no tombstone, and not the
// retired mark, which only a sweep raises.
//
// NO GUARD ON ITS SIZE: a compaction this build's committer forms writes at
// most compactDecidedRows rows ([CompactionProposal.Validate], and the decide's
// withinCompaction), and one formed otherwise — a newer build that raised
// MaxCompactInputs — is applied whole, as a push record is: the framework
// checks its transaction budget only at a record's boundary, and a record this
// build reads and refused at its apply would stop the applier on every node.
func (a *Applier) applyCompact(ctx context.Context, tx *sql.Tx, at applyContext, c Compaction) (int, error) {
	retiring, err := namedPackRows(ctx, tx, c.RepoID, c.Retire)
	if err != nil {
		return 0, fmt.Errorf("code: read the packs the compaction at %s retires: %w", at.position, err)
	}
	ids := make([]string, len(c.Add))
	for i, out := range c.Add {
		ids[i] = out.PackID
	}
	held, err := namedPackRows(ctx, tx, c.RepoID, ids)
	if err != nil {
		return 0, fmt.Errorf("code: read the outputs of the compaction at %s: %w", at.position, err)
	}
	// AN OUTPUT IS WRITTEN WHERE code_packs WILL NOT HOLD IT once the inputs
	// have left: one it holds is not written again, unless it is also an
	// input — which no writer forms — and is leaving.
	leaving := make(map[string]bool, len(retiring))
	for _, p := range retiring {
		leaving[p.id] = true
	}
	stays := make(map[string]bool, len(held))
	for _, p := range held {
		if !leaving[p.id] {
			stays[p.id] = true
		}
	}
	var adding []PackRef
	for _, out := range c.Add {
		if !stays[out.PackID] {
			// NAMED ONCE: a record naming one output twice writes it once.
			stays[out.PackID] = true
			adding = append(adding, out)
		}
	}
	if len(retiring) == 0 && len(adding) == 0 {
		// APPLIED ALREADY: nothing of it is left to move.
		return 0, nil
	}

	var gonePacks int
	var goneBytes, goneObjects int64
	for _, p := range retiring {
		if p.state == packLive {
			gonePacks++
			goneBytes += p.size
			goneObjects += p.objects
		}
	}
	retired, err := store.InsertRows(ctx, tx, at.maxVariables,
		`INSERT INTO code_packs_retired (repo_id, pack_id, part, object, hash, size, index_bytes,
			reverse_bytes, git_checksum, objects, kind, added_position, retired_position, retired_at) VALUES `,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		`ON CONFLICT (repo_id, pack_id, part) DO NOTHING`,
		len(retiring), func(i int) []any {
			p := retiring[i]
			return []any{c.RepoID, p.id, p.part, p.object, p.hash, p.size, p.indexBytes, p.reverseBytes,
				p.checksum, p.objects, p.kind, p.added, at.packed, at.at()}
		})
	if err != nil {
		return 0, fmt.Errorf("code: retire the packs the compaction at %s replaces: %w", at.position, err)
	}
	var deleted int64
	if len(retiring) > 0 {
		list, err := jsonList(distinct(c.Retire))
		if err != nil {
			return 0, err
		}
		res, err := tx.ExecContext(ctx, deletePacksSQL, c.RepoID, list)
		if err != nil {
			return 0, fmt.Errorf("code: remove the packs the compaction at %s replaces: %w", at.position, err)
		}
		deleted, _ = res.RowsAffected()
	}
	var addedBytes, addedObjects int64
	for _, out := range adding {
		addedBytes += out.Size
		addedObjects += out.Objects
	}
	added, err := store.InsertRows(ctx, tx, at.maxVariables,
		`INSERT INTO code_packs (repo_id, pack_id, part, object, hash, size, index_bytes,
			reverse_bytes, git_checksum, objects, kind, state, added_position) VALUES `,
		`(?, ?, 'pack', ?, ?, ?, ?, ?, ?, ?, 'compact', 'live', ?)`,
		`ON CONFLICT (repo_id, pack_id, part) DO NOTHING`,
		len(adding), func(i int) []any {
			out := adding[i]
			return []any{c.RepoID, out.PackID, out.Object.String(), string(out.Hash), out.Size,
				out.IndexBytes, out.ReverseBytes, out.GitChecksum, out.Objects, at.packed}
		})
	if err != nil {
		return 0, fmt.Errorf("code: write the outputs of the compaction at %s: %w", at.position, err)
	}
	if _, err := tx.ExecContext(ctx, compactCountersSQL, gonePacks, len(adding), goneBytes, addedBytes,
		goneObjects, addedObjects, at.packed, c.RepoID); err != nil {
		return 0, fmt.Errorf("code: count the compaction at %s: %w", at.position, err)
	}
	return retired + int(deleted) + added + 1, nil
}

// compactCountersSQL moves a repository's live-pack counters by what one
// compaction's apply found and wrote, and its version to the record's —
// registered in plan_test.go.
const compactCountersSQL = `UPDATE code_repos SET live_packs = live_packs - ? + ?,
	live_bytes = live_bytes - ? + ?, live_objects = live_objects - ? + ?, version = ? WHERE id = ?`
