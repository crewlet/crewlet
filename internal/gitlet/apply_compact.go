package gitlet

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/store"
)

// applyLog is the applier's own voice: an operator grepping for a record
// applied here need not know it was filed under the committer.
var applyLog = logging.Get("gitlet.apply")

// applyCompact applies one compaction ([Compaction]): its retired packs move
// from code_packs to code_packs_retired, the outputs code_packs lacks are
// written, and a publication (one carrying a layer) replaces the chain's top
// ([Applier.applyLayer]). It reads no clock or config: the decide proved inputs
// live and the layer's fit on the rows the subject's arbitration hands this apply,
// so every guard is a SKIP, never a refusal stopping the log on every node: a
// retired id no longer held, an output already held, a layer the chain does not
// fit (NOTHING applies, code_layer_not_applied unless a replay: its repack alone
// would retire packs of layers that stay). Counters move by the rows found and
// written, never the record's lists; published_* are recounted. Beyond
// [CompactionProposal.Validate]'s compactDecidedRows no size guard: the budget is
// checked only at a record's boundary. Only a sweep marks retired.
func (a *Applier) applyCompact(ctx context.Context, tx *sql.Tx, at applyContext, c Compaction) (int, error) {
	var chain []LayerRow
	if c.Layer != nil {
		var err error
		if chain, err = layersOf(ctx, tx, c.RepoID); err != nil {
			return 0, fmt.Errorf("gitlet: the publication at %s: %w", at.position, err)
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
	plan, err := planRewrite(ctx, tx, c.RepoID, c.Retire, nil, len(c.Add), func(i int) (PackRef, PackKind,
		PackState) {
		return c.Add[i], PackCompact, PackLive
	})
	if err != nil {
		return 0, fmt.Errorf("gitlet: the compaction at %s: %w", at.position, err)
	}
	if c.Layer == nil && plan.idle() {
		// Applied already.
		return 0, nil
	}
	m, err := plan.apply(ctx, tx, at, c.RepoID)
	if err != nil {
		return 0, fmt.Errorf("gitlet: the compaction at %s: %w", at.position, err)
	}
	moved := m.rows + 1
	if c.Layer == nil {
		if _, err = tx.ExecContext(ctx, compactCountersSQL, m.goneLive.packs, m.addedLive.packs,
			m.goneLive.bytes, m.addedLive.bytes, m.goneLive.objects, m.addedLive.objects, at.packed,
			c.RepoID); err != nil {
			return 0, fmt.Errorf("gitlet: count the compaction at %s: %w", at.position, err)
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
		return 0, fmt.Errorf("gitlet: recount the packs the publication at %s leaves published: %w", at.position, err)
	}
	if _, err := tx.ExecContext(ctx, publishCountersSQL, m.goneLive.packs, m.addedLive.packs, m.goneLive.bytes,
		m.addedLive.bytes, m.goneLive.objects, m.addedLive.objects, packs, objects, at.at(), c.Layer.TookMs,
		at.packed, at.packed, c.RepoID); err != nil {
		return 0, fmt.Errorf("gitlet: count the publication at %s: %w", at.position, err)
	}
	return moved + placed, nil
}

// A REWRITE'S PACKS, MOVED IN ONE PLACE: a compaction's and an expunge's begin
// and commit (apply_expunge.go) share one reading of the rows retired, the
// outputs written, their content date and the counters (ADR-0008).

// packTally is what a set of pack rows holds.
type packTally struct {
	packs          int
	bytes, objects int64
}

// add counts in one pack of size bytes holding objects objects.
func (t *packTally) add(size, objects int64) {
	t.packs++
	t.bytes += size
	t.objects += objects
}

// rewritePlan is what one rewrite's apply will move, read before it moves
// anything: the rows it retires, the outputs it writes, and the position every
// output's content dates from.
type rewritePlan struct {
	leaving []namedPack
	adds    []rewriteOutput
	// content is the position its outputs' content dates from ([PackRow.Content]):
	// the least of the retired rows', since a merge's output holds whatever its
	// oldest input held; 0 where it retires none (a later record moved its
	// inputs first, so its outputs claim nothing). A purge's begin dates its
	// outputs at its scan instead ([Applier.applyExpungeBegin]).
	content int64
	// arrived is when its outputs' newest content arrived ([PackRow.Arrived]):
	// the greatest of the retired rows'; 0 where it retires none, which
	// [rewritePlan.apply] writes as the record's broker instant, so an
	// expunge's grace keeps such outputs whole.
	arrived int64
}

// rewriteOutput is one output a rewrite writes, and the kind and state it is
// written in.
type rewriteOutput struct {
	out   PackRef
	kind  PackKind
	state PackState
}

// rewritten is what [rewritePlan.apply] moved: the rows it wrote or deleted,
// what it retired — all of it, and the live part — and the live packs it
// added, which are what the repository's live counters move by.
type rewritten struct {
	rows      int
	goneAll   packTally
	goneLive  packTally
	addedLive packTally
}

// planRewrite reads what a rewrite of repoID's packs will move: every
// code_packs row whose id retire names (and retiring keeps, where given)
// leaves; output(i) is the i-th of n outputs with its kind and state, written
// unless code_packs still holds it once those rows leave. A record naming one
// output twice writes it once.
func planRewrite(ctx context.Context, tx *sql.Tx, repoID string, retire []string, retiring func(namedPack) bool,
	n int, output func(i int) (PackRef, PackKind, PackState)) (rewritePlan, error) {

	found, err := namedPackRows(ctx, tx, repoID, retire)
	if err != nil {
		return rewritePlan{}, fmt.Errorf("read the packs it retires: %w", err)
	}
	var plan rewritePlan
	gone := make(map[string]bool, len(found))
	for _, p := range found {
		if retiring == nil || retiring(p) {
			if len(plan.leaving) == 0 || p.content < plan.content {
				plan.content = p.content
			}
			plan.arrived = max(plan.arrived, p.arrived)
			plan.leaving = append(plan.leaving, p)
			gone[p.id] = true
		}
	}
	ids := make([]string, n)
	for i := range n {
		out, _, _ := output(i)
		ids[i] = out.PackID
	}
	held, err := namedPackRows(ctx, tx, repoID, ids)
	if err != nil {
		return rewritePlan{}, fmt.Errorf("read its outputs: %w", err)
	}
	stays := make(map[string]bool, len(held))
	for _, p := range held {
		if !gone[p.id] {
			stays[p.id] = true
		}
	}
	for i := range n {
		out, kind, state := output(i)
		if !stays[out.PackID] {
			stays[out.PackID] = true
			plan.adds = append(plan.adds, rewriteOutput{out: out, kind: kind, state: state})
		}
	}
	return plan, nil
}

// idle reports a plan that moves nothing: a record whose rows another record,
// or its own earlier apply, moved first.
func (p rewritePlan) idle() bool { return len(p.leaving) == 0 && len(p.adds) == 0 }

// apply moves what p planned, in repoID's rows: the leaving rows into
// code_packs_retired, whole, stamped with at's position and broker instant,
// and out of code_packs; and the outputs into code_packs, added at at's
// position, their content dated from p's.
func (p rewritePlan) apply(ctx context.Context, tx *sql.Tx, at applyContext, repoID string) (rewritten, error) {
	var moved rewritten
	for _, l := range p.leaving {
		moved.goneAll.add(l.size, l.objects)
		if l.state == PackLive {
			moved.goneLive.add(l.size, l.objects)
		}
	}
	retired, err := store.InsertRows(ctx, tx, at.maxVariables,
		`INSERT INTO code_packs_retired (repo_id, pack_id, part, object, hash, size, index_bytes,
			reverse_bytes, git_checksum, objects, kind, added_position, content_position, arrived_at,
			retired_position, retired_at) VALUES `,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		`ON CONFLICT (repo_id, pack_id, part) DO NOTHING`,
		len(p.leaving), func(i int) []any {
			l := p.leaving[i]
			return []any{repoID, l.id, l.part, l.object, l.hash, l.size, l.indexBytes, l.reverseBytes,
				l.checksum, l.objects, string(l.kind), l.added, l.content, l.arrived, at.packed, at.at()}
		})
	if err != nil {
		return rewritten{}, fmt.Errorf("retire the packs it replaces: %w", err)
	}
	var deleted int64
	if len(p.leaving) > 0 {
		ids := make([]string, len(p.leaving))
		for i, l := range p.leaving {
			ids[i] = l.id
		}
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		list, err := jsonList(distinct(ids))
		if err != nil {
			return rewritten{}, err
		}
		res, err := tx.ExecContext(ctx, deletePacksSQL, repoID, list)
		if err != nil {
			return rewritten{}, fmt.Errorf("remove the packs it replaces: %w", err)
		}
		deleted, _ = res.RowsAffected()
	}
	arrived := p.arrived
	if arrived == 0 {
		arrived = at.at()
	}
	added, err := store.InsertRows(ctx, tx, at.maxVariables,
		`INSERT INTO code_packs (repo_id, pack_id, part, object, hash, size, index_bytes,
			reverse_bytes, git_checksum, objects, kind, state, added_position, content_position,
			arrived_at) VALUES `,
		`(?, ?, 'pack', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		`ON CONFLICT (repo_id, pack_id, part) DO NOTHING`,
		len(p.adds), func(i int) []any {
			a := p.adds[i]
			return []any{repoID, a.out.PackID, a.out.Object.String(), string(a.out.Hash), a.out.Size,
				a.out.IndexBytes, a.out.ReverseBytes, a.out.GitChecksum, a.out.Objects, string(a.kind),
				string(a.state), at.packed, p.content, arrived}
		})
	if err != nil {
		return rewritten{}, fmt.Errorf("write its outputs: %w", err)
	}
	for _, a := range p.adds {
		if a.state == PackLive {
			moved.addedLive.add(a.out.Size, a.out.Objects)
		}
	}
	moved.rows = retired + int(deleted) + added
	return moved, nil
}

// layerFits reports whether a publication's layer fits chain, by the decide's
// own reading ([replacedTop]): the layers it replaces are the top, its base the
// layer below, and its id none of the base's. Otherwise applied reports a
// replay (its id already in the chain, there exactly when the rest of its one
// transaction is); false is a record no correct log carries.
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

// applyLayer replaces the chain's top with the publication's layer and its
// parts, answering the rows it wrote or deleted. Nothing is renumbered: the
// layer takes its base's seq plus one, the replaced layers being exactly the
// rows above the base ([layerFits]).
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
			return 0, fmt.Errorf("gitlet: remove the parts of the layers the publication at %s replaces: %w",
				at.position, err)
		}
		parts, _ := res.RowsAffected()
		if res, err = tx.ExecContext(ctx, deleteLayersAboveSQL, c.RepoID, baseSeq); err != nil {
			return 0, fmt.Errorf("gitlet: remove the layers the publication at %s replaces: %w", at.position, err)
		}
		layers, _ := res.RowsAffected()
		rows = parts + layers
	}
	packs, err := json.Marshal(l.Packs)
	if err != nil {
		return 0, fmt.Errorf("gitlet: encode the packs of the layer at %s: %w", at.position, err)
	}
	if _, err = tx.ExecContext(ctx, insertLayerSQL, c.RepoID, baseSeq+1, l.LayerID, l.BaseID, l.GraphID,
		string(packs), l.MidxBytes, l.BitmapBytes, l.GraphBytes, l.Objects, l.Commits, l.Git, l.TookMs,
		at.packed); err != nil {
		return 0, fmt.Errorf("gitlet: write the layer the publication at %s adds: %w", at.position, err)
	}
	parts, err := store.InsertRows(ctx, tx, at.maxVariables,
		`INSERT INTO code_layer_parts (repo_id, layer_id, part, object, hash, size) VALUES `,
		`(?, ?, ?, ?, ?, ?)`, `ON CONFLICT (repo_id, layer_id, part) DO NOTHING`,
		len(l.Parts), func(i int) []any {
			p := l.Parts[i]
			return []any{c.RepoID, l.LayerID, i, p.Object.String(), string(p.Hash), p.Size}
		})
	if err != nil {
		return 0, fmt.Errorf("gitlet: write the parts of the layer the publication at %s adds: %w", at.position, err)
	}
	return int(rows) + 1 + parts, nil
}

// publishedIn counts the repository's live packs the layers of chain cover,
// and their objects, from the rows by checksum: a pack row byte-identical to a
// published one is published, to this count as to git.
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

// The compaction apply's statements, registered in plan_test.go: a
// publication's replaced layers go by the key's range above their base, their
// parts by the parts' key, driven from the list of their ids (planner.go).
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
