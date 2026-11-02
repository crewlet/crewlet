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
	plan, err := planRewrite(ctx, tx, c.RepoID, c.Retire, nil, len(c.Add), func(i int) (PackRef, PackKind,
		PackState) {
		return c.Add[i], PackCompact, PackLive
	})
	if err != nil {
		return 0, fmt.Errorf("code: the compaction at %s: %w", at.position, err)
	}
	if c.Layer == nil && plan.idle() {
		// APPLIED ALREADY: nothing of it is left to move.
		return 0, nil
	}
	m, err := plan.apply(ctx, tx, at, c.RepoID)
	if err != nil {
		return 0, fmt.Errorf("code: the compaction at %s: %w", at.position, err)
	}
	moved := m.rows + 1
	if c.Layer == nil {
		if _, err := tx.ExecContext(ctx, compactCountersSQL, m.goneLive.packs, m.addedLive.packs,
			m.goneLive.bytes, m.addedLive.bytes, m.goneLive.objects, m.addedLive.objects, at.packed,
			c.RepoID); err != nil {
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
	if _, err := tx.ExecContext(ctx, publishCountersSQL, m.goneLive.packs, m.addedLive.packs, m.goneLive.bytes,
		m.addedLive.bytes, m.goneLive.objects, m.addedLive.objects, packs, objects, at.at(), c.Layer.TookMs,
		at.packed, at.packed, c.RepoID); err != nil {
		return 0, fmt.Errorf("code: count the publication at %s: %w", at.position, err)
	}
	return moved + placed, nil
}

// A REWRITE'S PACKS, MOVED IN ONE PLACE: a compaction's ([Applier.applyCompact])
// and an expunge's begin's and commit's (apply_expunge.go) — every record that
// replaces some of a repository's packs by others — so the rows it retires,
// the outputs it writes, the date their content is read at by a purge and the
// counters it moves them by are one reading for every kind of rewrite
// (ADR-0008).

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
	// content is the position its outputs' content dates from as a purge
	// reads it ([PackRow.Content]): the least of the rows it retires, since
	// moving content judges none of it — a merge's output holds whatever the
	// oldest of its inputs held — and 0, below every position, where it
	// retires none: a record whose inputs a later one moved first, whose
	// outputs then claim nothing about what they hold. A purge's begin dates
	// its outputs at its scan instead ([Applier.applyExpungeBegin]).
	content int64
	// arrived is when its outputs' NEWEST content arrived ([PackRow.Arrived]),
	// encoded: the greatest of the rows it retires, since moving content
	// brings none — and 0 where it retires none, which [rewritePlan.apply]
	// writes as the record's own broker instant: outputs that claim nothing
	// about what they hold are as young as the record, the side an expunge's
	// grace keeps whole.
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

// planRewrite reads what a rewrite of repoID's packs will move: every row of
// code_packs whose pack id retire names — those retiring keeps, where it is
// given — leaves; and of its n outputs, output(i) is the i-th and the kind and
// state it is written in, each written where code_packs will not hold it once
// those rows have left, its content dated from the oldest of the leaving rows'
// ([rewritePlan.content]). An output code_packs holds already is not written
// again, unless it is also an input — which no writer forms — and is leaving;
// and a record naming one output twice writes it once.
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
