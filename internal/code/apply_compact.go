package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/store"
)

// applyLog is the applier's own voice: what it says is about a record applied
// here — today only a publication that applies as nothing — and an operator
// grepping for that should not have to know it was filed under the committer.
var applyLog = logging.Get("code.apply")

// applyCompact applies one compaction ([Compaction]): the rows of the packs it
// retires leave code_packs for code_packs_retired, stamped with the record's
// position and broker instant, the outputs code_packs does not hold are written
// there, and the repository's counters move by exactly what moved — in the one
// transaction the framework holds a record's apply to. A PUBLICATION — a
// compaction carrying a layer — also replaces the top of the repository's
// published chain with its layer ([applyLayer]).
//
// PURE IN THE RECORD, ITS POSITION AND ITS INSTANT, like every apply here: it
// reads no clock and no configuration and decides nothing again. The decide
// already held every input live, every output a bundle and the objects to what
// the inputs held, and a publication's layer to the chain it builds on, and
// the broker's arbitration on the repository's subject guarantees the record
// applies against the rows that decide read. So every guard here is a SKIP,
// never a refusal, which would stop the log on every node:
//
//   - a pack id the record retires that code_packs no longer holds is passed
//     over — a replay of a record whose packs a later one has since moved, or
//     a record a newer build formed — and every row of one it holds moves,
//     whatever its part;
//   - an output code_packs already holds is not written again;
//   - a record that finds nothing to retire and nothing to add, and carries
//     no layer, has applied already, and writes nothing at all;
//   - and a publication whose layer the chain does not fit applies as
//     NOTHING, whole — the replay of one applied already silently, and one no
//     correct log carries logged (code_layer_not_applied) — because its
//     repack without its layer would retire the packs of layers that stay.
//
// THE COUNTERS ARE THE ROWS: live_packs, live_bytes and live_objects move by
// the rows this apply found and wrote — the live ones among those it retires,
// as a purge part counts them — never by the record's lists, so a replay over
// rows it already moved moves nothing, and an input named twice counts once;
// and a publication's published_packs and published_objects are RECOUNTED from
// the rows it leaves. Nothing else is touched: no ref, no push row, no
// tombstone, and not the retired mark, which only a sweep raises.
//
// NO GUARD ON ITS SIZE: a compaction this build's committer forms writes at
// most compactDecidedRows rows ([CompactionProposal.Validate], and the decide's
// withinCompaction), and one formed otherwise — a newer build that raised
// MaxCompactInputs — is applied whole, as a push record is: the framework
// checks its transaction budget only at a record's boundary, and a record this
// build reads and refused at its apply would stop the applier on every node.
func (a *Applier) applyCompact(ctx context.Context, tx *sql.Tx, at applyContext, c Compaction) (int, error) {
	var chain []LayerRow
	if c.Layer != nil {
		var err error
		if chain, err = layersOf(ctx, tx, c.RepoID); err != nil {
			return 0, fmt.Errorf("code: the publication at %s: %w", at.position, err)
		}
		if fits, applied := layerFits(chain, *c.Layer); !fits {
			if !applied {
				applyLog.WarnContext(ctx, "code_layer_not_applied", "repo", c.RepoID,
					"layer", c.Layer.LayerID, "base", c.Layer.BaseID, "replaces", c.Layer.Replaces,
					"chain", chainNames(chain), "position", at.position.String())
			}
			return 0, nil
		}
	}
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
	if c.Layer == nil && len(retiring) == 0 && len(adding) == 0 {
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
		list, listErr := jsonList(distinct(c.Retire))
		if listErr != nil {
			return 0, listErr
		}
		res, deleteErr := tx.ExecContext(ctx, deletePacksSQL, c.RepoID, list)
		if deleteErr != nil {
			return 0, fmt.Errorf("code: remove the packs the compaction at %s replaces: %w", at.position,
				deleteErr)
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
	moved := retired + int(deleted) + added + 1
	if c.Layer == nil {
		if _, err := tx.ExecContext(ctx, compactCountersSQL, gonePacks, len(adding), goneBytes, addedBytes,
			goneObjects, addedObjects, at.packed, c.RepoID); err != nil {
			return 0, fmt.Errorf("code: count the compaction at %s: %w", at.position, err)
		}
		return moved, nil
	}
	placed, err := a.applyLayer(ctx, tx, at, c, chain)
	if err != nil {
		return 0, err
	}
	packs, objects, err := publishedIn(ctx, tx, c.RepoID, append(slices.Clone(chain[:len(chain)-len(c.Layer.Replaces)]),
		LayerRow{Layer: *c.Layer}))
	if err != nil {
		return 0, fmt.Errorf("code: recount the packs the publication at %s leaves published: %w", at.position, err)
	}
	if _, err := tx.ExecContext(ctx, publishCountersSQL, gonePacks, len(adding), goneBytes, addedBytes,
		goneObjects, addedObjects, packs, objects, at.at(), c.Layer.TookMs, at.packed, at.packed,
		c.RepoID); err != nil {
		return 0, fmt.Errorf("code: count the publication at %s: %w", at.position, err)
	}
	return moved + placed, nil
}

// layerFits is the apply's guard over a publication's layer, read against the
// chain its rows hold: fits where the layers it replaces are the chain's top,
// its base the layer below them, and its id none of that base's — the decide's
// own reading ([replacedTop]); otherwise applied reports the layer's id already
// in the chain, the replay of a record that applied — every write of one is
// in its one transaction, so its layer's row is there exactly when the rest
// is — and false a record no correct log carries.
func layerFits(chain []LayerRow, l LayerRef) (fits, applied bool) {
	inChain := slices.ContainsFunc(chain, func(row LayerRow) bool { return row.Layer.LayerID == l.LayerID })
	if !replacedTop(chain, l.BaseID, l.Replaces) {
		return false, inChain
	}
	base := chain[:len(chain)-len(l.Replaces)]
	if slices.ContainsFunc(base, func(row LayerRow) bool { return row.Layer.LayerID == l.LayerID }) {
		return false, false
	}
	return true, false
}

// applyLayer replaces the top of a repository's published chain with a
// publication's layer — the layers it replaces and their parts removed, the
// layer written above its base with its parts — and answers the rows it wrote
// or deleted. The chain is renumbered by nothing: the layer takes its base's
// place plus one, the replaced layers being exactly the rows above the base,
// which [layerFits] read as the chain's top.
func (a *Applier) applyLayer(ctx context.Context, tx *sql.Tx, at applyContext, c Compaction,
	chain []LayerRow) (int, error) {

	l := c.Layer
	baseSeq := 0
	if under := len(chain) - len(l.Replaces); under > 0 {
		baseSeq = chain[under-1].Seq
	}
	var rows int64
	if len(l.Replaces) > 0 {
		list, err := jsonList(l.Replaces)
		if err != nil {
			return 0, err
		}
		res, err := tx.ExecContext(ctx, deleteLayerPartsSQL, c.RepoID, list)
		if err != nil {
			return 0, fmt.Errorf("code: remove the parts of the layers the publication at %s replaces: %w",
				at.position, err)
		}
		parts, _ := res.RowsAffected()
		if res, err = tx.ExecContext(ctx, deleteLayersAboveSQL, c.RepoID, baseSeq); err != nil {
			return 0, fmt.Errorf("code: remove the layers the publication at %s replaces: %w", at.position, err)
		}
		layers, _ := res.RowsAffected()
		rows = parts + layers
	}
	packs, err := json.Marshal(l.Packs)
	if err != nil {
		return 0, fmt.Errorf("code: encode the packs of the layer at %s: %w", at.position, err)
	}
	if _, err := tx.ExecContext(ctx, insertLayerSQL, c.RepoID, baseSeq+1, l.LayerID, l.BaseID, l.GraphID,
		string(packs), l.MidxBytes, l.BitmapBytes, l.GraphBytes, l.Objects, l.Commits, l.Git, l.TookMs,
		at.packed); err != nil {
		return 0, fmt.Errorf("code: write the layer the publication at %s adds: %w", at.position, err)
	}
	parts, err := store.InsertRows(ctx, tx, at.maxVariables,
		`INSERT INTO code_layer_parts (repo_id, layer_id, part, object, hash, size) VALUES `,
		`(?, ?, ?, ?, ?, ?)`, `ON CONFLICT (repo_id, layer_id, part) DO NOTHING`,
		len(l.Parts), func(i int) []any {
			p := l.Parts[i]
			return []any{c.RepoID, l.LayerID, i, p.Object.String(), string(p.Hash), p.Size}
		})
	if err != nil {
		return 0, fmt.Errorf("code: write the parts of the layer the publication at %s adds: %w", at.position, err)
	}
	return int(rows) + 1 + parts, nil
}

// publishedIn counts the repository's live packs the layers of chain cover,
// and the objects they hold — FROM THE ROWS, by checksum, so a pack row
// byte-identical to a published one (two rows, one checksum) is published to
// this count as it is to git.
func publishedIn(ctx context.Context, tx *sql.Tx, repoID string, chain []LayerRow) (int, int64, error) {
	covered := map[string]bool{}
	for _, row := range chain {
		for _, p := range row.Layer.Packs {
			covered[p] = true
		}
	}
	live, err := livePacksOf(ctx, tx, repoID)
	if err != nil {
		return 0, 0, err
	}
	var packs int
	var objects int64
	for _, p := range live {
		if covered[p.checksum] {
			packs++
			objects += p.objects
		}
	}
	return packs, objects, nil
}

// The compaction apply's statements — registered in plan_test.go where they
// read: compactCountersSQL moves a repository's live-pack counters by what one
// compaction's apply found and wrote, and its version to the record's;
// publishCountersSQL does that for a publication with its recount and its
// layers' mark; a publication's replaced layers go by the key's range above
// their base, their parts by the parts' key, driven from the list of their
// ids (planner.go).
const (
	compactCountersSQL = `UPDATE code_repos SET live_packs = live_packs - ? + ?,
		live_bytes = live_bytes - ? + ?, live_objects = live_objects - ? + ?, version = ? WHERE id = ?`
	publishCountersSQL = `UPDATE code_repos SET live_packs = live_packs - ? + ?,
		live_bytes = live_bytes - ? + ?, live_objects = live_objects - ? + ?,
		published_packs = ?, published_objects = ?, published_at = ?, publish_took_ms = ?,
		layers_position = ?, version = ? WHERE id = ?`
	deleteLayersAboveSQL = `DELETE FROM code_layers WHERE repo_id = ? AND seq > ?`
	deleteLayerPartsSQL  = `DELETE FROM code_layer_parts WHERE rowid IN (SELECT p.rowid
		FROM json_each(?2) AS j CROSS JOIN code_layer_parts AS p
		WHERE p.repo_id = ?1 AND p.layer_id = CAST(j.value AS TEXT))`
	insertLayerSQL = `INSERT INTO code_layers (repo_id, seq, layer_id, base_id, graph_id, packs,
		midx_bytes, bitmap_bytes, graph_bytes, objects, commits, git, took_ms, added_position)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
)
