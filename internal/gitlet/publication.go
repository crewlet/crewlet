package gitlet

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A PUBLICATION is a compaction carrying a [LayerRef] ([Compaction.Layer]); the
// package doc's "A publication puts a published layer on the chain" says why.
// Beyond [decideCompaction]'s rules it is held, against one snapshot's rows, to
// rule 2 as amended for every compaction, to 6g, and to 6a–6f ([decideLayer]),
// each refusing it stale unless noted:
//   - 2: no input is a pack named by a layer the record does not replace,
//     since a node loads a chain only over every pack it names;
//   - 6g, first, off the repository's row: no expunge open ([ExpungeBegin]).
//     Until its commit a live pack may need an object only a dropping pack
//     holds, so no layer is sound; [ReadPublicationDue] lists none mid-expunge.

// The apply (apply_compact.go) replaces the chain's top and recounts published
// packs from the rows, never by arithmetic, which a pack row byte-identical to
// a published one would make drift from what git reads. Every pack a layer row names then stays live: rule 2
// retires none, an expunge's begin drops every layer over a pack it retires and
// all above it (decide_expunge.go), and 6c names none that is not.

// Check is what every layer row must be before any byte moves, at the
// committer's decide and a Gitlet node's install alike; each refusal is
// [ErrInvalidLayer] naming the field. Ids and pack checksums are git's 40
// lowercase hex digits and never its zero id, the packs sorted and distinct, and
// the parts whole objects of exactly [LayerPartBytes] but the last, summing to
// [LayerRef.Size].
func (l LayerRef) Check() error {
	bad := func(field, format string, args ...any) error {
		return fmt.Errorf("%w: %s: %s", ErrInvalidLayer, field, fmt.Sprintf(format, args...))
	}
	switch {
	case !layerName(l.LayerID):
		return bad("layer_id", "%q is not a multi-pack-index layer's checksum, 40 lowercase hex digits", l.LayerID)
	case l.BaseID != "" && !layerName(l.BaseID):
		return bad("base_id", "%q is not a layer's checksum; the chain's root names none", l.BaseID)
	case l.BaseID == l.LayerID:
		return bad("base_id", "%s is the layer itself", l.BaseID)
	}
	for i, id := range l.Replaces {
		switch {
		case !layerName(id):
			return bad(fmt.Sprintf("replaces[%d]", i), "%q is not a layer's checksum", id)
		case slices.Contains(l.Replaces[:i], id):
			return bad(fmt.Sprintf("replaces[%d]", i), "%s is named twice", id)
		}
	}
	switch {
	case l.GraphBytes < 0:
		return bad("graph_bytes", "%d", l.GraphBytes)
	case l.GraphBytes == 0 && l.GraphID != "":
		return bad("graph_id", "%s names a commit-graph layer of no bytes", l.GraphID)
	case l.GraphBytes > 0 && !layerName(l.GraphID):
		return bad("graph_id", "%q is not a commit-graph layer's checksum, and the layer carries %d bytes of one",
			l.GraphID, l.GraphBytes)
	case l.GraphBytes == 0 && l.Commits != 0:
		return bad("commits", "%d commits in no commit-graph layer", l.Commits)
	case l.GraphBytes > 0 && l.Commits < 1:
		return bad("commits", "%d commits in a commit-graph layer of %d bytes", l.Commits, l.GraphBytes)
	case l.MidxBytes < 1:
		return bad("midx_bytes", "%d; a layer is a multi-pack-index first", l.MidxBytes)
	case l.BitmapBytes < 1:
		return bad("bitmap_bytes", "%d; a published layer carries its bitmap, or no layer beneath it serves one",
			l.BitmapBytes)
	case l.Objects < 1:
		return bad("objects", "%d; a multi-pack-index layer holds an object at the least", l.Objects)
	case !validRelease(l.Git):
		return bad("git", "%q is not a git release as its version line spells it (2.56.0)", l.Git)
	case l.TookMs < 0:
		return bad("took_ms", "%d", l.TookMs)
	case len(l.Packs) == 0:
		return bad("packs", "a layer covers a pack at the least")
	case len(l.Packs) > MaxPacksPerRepo:
		return bad("packs", "%d packs, past the %d a repository holds (MaxPacksPerRepo)", len(l.Packs),
			MaxPacksPerRepo)
	}
	for i, p := range l.Packs {
		switch {
		case !layerName(p):
			return bad(fmt.Sprintf("packs[%d]", i), "%q is not a pack's git checksum, 40 lowercase hex digits", p)
		case i > 0 && l.Packs[i-1] >= p:
			// SORTED AND DISTINCT, so one plan forms one record on every node
			// and the decide's set of them is the list.
			return bad(fmt.Sprintf("packs[%d]", i), "%s is not after %s — the packs are sorted and distinct",
				p, l.Packs[i-1])
		}
	}
	switch {
	case len(l.Parts) == 0:
		return bad("parts", "a layer's bytes are stored in a part at the least")
	case len(l.Parts) > MaxLayerParts:
		return bad("parts", "%d parts, past the %d a layer is cut into (MaxLayerParts)", len(l.Parts), MaxLayerParts)
	}
	var stored int64
	for i, p := range l.Parts {
		field := fmt.Sprintf("parts[%d]", i)
		if err := p.Content().Validate(); err != nil {
			return bad(field, "%v", err)
		}
		switch last := i == len(l.Parts)-1; {
		case !last && p.Size != LayerPartBytes:
			return bad(field+".size", "%d bytes; every part but the last is exactly %d (LayerPartBytes)", p.Size,
				LayerPartBytes)
		case last && (p.Size < 1 || p.Size > LayerPartBytes):
			return bad(field+".size", "%d bytes; the last part holds 1 to %d (LayerPartBytes)", p.Size,
				LayerPartBytes)
		}
		stored += p.Size
	}
	if stored != l.Size() {
		return bad("parts", "%d bytes stored, and the sections are %d (midx %d, bitmap %d, graph %d)", stored,
			l.Size(), l.MidxBytes, l.BitmapBytes, l.GraphBytes)
	}
	return nil
}

// layerName reports a checksum as git names a layer's, a commit-graph layer's
// or a pack's files: 40 lowercase hex digits, never the zero id.
func layerName(s string) bool { return gitref.ValidOID(s) && s != ZeroOID }

// validRelease reports s spelled as git's version line spells its first three
// numbers and gitproc.Version.String writes them back ("2.56.0"): no sign, no
// leading zero. The domain reads no git, so it checks the spelling; a Gitlet
// node parses the field back with gitproc.
func validRelease(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return false
	}
	for _, n := range parts {
		// NINE DIGITS AT MOST, so every number a release spells fits an int
		// on every platform the engine builds for.
		if n == "" || len(n) > 9 || (len(n) > 1 && n[0] == '0') {
			return false
		}
		if _, err := strconv.ParseUint(n, 10, 32); err != nil {
			return false
		}
	}
	return true
}

// validLayerFields is [CompactionProposal.Validate]'s half for a publication:
// the layer passes [LayerRef.Check], every repack output is a pack the layer
// covers, and it replaces fewer layers than a chain holds, so the chain it
// leaves is never empty.
func (c Compaction) validLayerFields() error {
	l := c.Layer
	if err := l.Check(); err != nil {
		return fmt.Errorf("%w: layer: %w", ErrInvalidProposal, err)
	}
	if len(l.Replaces) >= MaxPublishedLayers {
		return fmt.Errorf("%w: layer.replaces: %d layers, and a chain holds at most %d (MaxPublishedLayers)",
			ErrInvalidProposal, len(l.Replaces), MaxPublishedLayers)
	}
	for i, out := range c.Add {
		if _, found := slices.BinarySearch(l.Packs, out.GitChecksum); !found {
			return fmt.Errorf("%w: add[%d].git_checksum: %s is an output of the layer's repack and not among the "+
				"packs the layer covers", ErrInvalidProposal, i, out.GitChecksum)
		}
	}
	return nil
}

// layersOf is a repository's published chain, root first, without its parts
// ([Reader.Snapshot] alone reads them): at most [MaxPublishedLayers] rows, by
// the table's key.
func layersOf(ctx context.Context, tx *sql.Tx, repoID string) ([]LayerRow, error) {
	rows, err := tx.QueryContext(ctx, layersOfSQL, repoID)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read %s's published layers: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []LayerRow
	for rows.Next() {
		var (
			row   LayerRow
			packs string
			added int64
		)
		l := &row.Layer
		if err := rows.Scan(&row.Seq, &l.LayerID, &l.BaseID, &l.GraphID, &packs, &l.MidxBytes, &l.BitmapBytes,
			&l.GraphBytes, &l.Objects, &l.Commits, &l.Git, &l.TookMs, &added); err != nil {
			return nil, fmt.Errorf("gitlet: read %s's published layers: %w", repoID, err)
		}
		if err := json.Unmarshal([]byte(packs), &l.Packs); err != nil {
			return nil, fmt.Errorf("gitlet: %s's published layer %s holds packs %q: %w", repoID, l.LayerID, packs, err)
		}
		row.Added = unpacked(added)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gitlet: read %s's published layers: %w", repoID, err)
	}
	return out, nil
}

// replacedTop reports whether replaces is chain's top, in order, with base the
// layer directly below it ("" when it replaces the whole chain): rule 6a and
// the apply's guard share it, so the committer and every applier agree.
func replacedTop(chain []LayerRow, base string, replaces []string) bool {
	k, j := len(chain), len(replaces)
	if j > k {
		return false
	}
	for i, id := range replaces {
		if chain[k-j+i].Layer.LayerID != id {
			return false
		}
	}
	if k == j {
		return base == ""
	}
	return chain[k-j-1].Layer.LayerID == base
}

// livePack is one live pack row as a publication's rules read it.
type livePack struct {
	id, checksum   string
	objects, added int64
}

// livePacksOf is a repository's live pack rows, one read by the key's prefix,
// for 6c, 6d and the apply's recount (a publication's and an expunge begin's).
// A dropping pack an expunge left is not live: no layer may name it and no
// recount counts it.
func livePacksOf(ctx context.Context, tx *sql.Tx, repoID string) ([]livePack, error) {
	rows, err := tx.QueryContext(ctx, livePacksSQL, repoID)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read %s's live packs: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []livePack
	for rows.Next() {
		var p livePack
		if err := rows.Scan(&p.id, &p.checksum, &p.objects, &p.added); err != nil {
			return nil, fmt.Errorf("gitlet: read %s's live packs: %w", repoID, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gitlet: read %s's live packs: %w", repoID, err)
	}
	return out, nil
}

// publishedBy maps every pack checksum chain's layers name, but those of the
// layers replaces names, to its layer: what an input may not be (rule 2) and,
// of the base alone, what a layer may not name again (6e).
func publishedBy(chain []LayerRow, replaces []string) map[string]string {
	out := map[string]string{}
	for _, row := range chain {
		if slices.Contains(replaces, row.Layer.LayerID) {
			continue
		}
		for _, p := range row.Layer.Packs {
			out[p] = row.Layer.LayerID
		}
	}
	return out
}

// decideLayer is rules 6a–6f over the chain the decide read, the inputs rule
// 2 retires and the rows tx holds, at now: a refusal and its sentence, nothing
// for a layer that fits, or [ErrInvalidProposal] for one no correct planner
// forms (6b, 6e). 6a: the replaced layers are the chain's top, the base the one
// below. 6b: the chain left holds at most [MaxPublishedLayers]. 6c: every pack
// named is live after the record. 6d COMPLETE: it names every pack live at
// [Compaction.Seen] that its base does not and the record does not retire —
// those packs are closed together, and git writes no bitmap over fewer; a later
// push depends only on packs before it, so it may stay out. 6e: no pack or id
// of its base's. 6f: each part's key minted within objstore.RecordWithin.
func decideLayer(ctx context.Context, tx *sql.Tx, c Compaction, chain []LayerRow, retired map[string]bool,
	now time.Time) (Refusal, string, error) {

	l := c.Layer
	k, j := len(chain), len(l.Replaces)
	if !replacedTop(chain, l.BaseID, l.Replaces) {
		return RefusalStale, fmt.Sprintf("the published layers moved under the plan — it builds on %s and "+
			"replaces %d of them, and the chain is %s; another publication landed first, and the next pass "+
			"plans again", baseName(l.BaseID), j, chainNames(chain)), nil
	}
	if k-j+1 > MaxPublishedLayers {
		return "", "", fmt.Errorf("%w: layer.replaces: the chain holds %d layers and the publication replaces %d, "+
			"leaving %d, past the %d one holds (MaxPublishedLayers)", ErrInvalidProposal, k, j, k-j+1,
			MaxPublishedLayers)
	}
	base := publishedBy(chain[:k-j], nil)
	if slices.ContainsFunc(chain[:k-j], func(row LayerRow) bool { return row.Layer.LayerID == l.LayerID }) {
		return "", "", fmt.Errorf("%w: layer.layer_id: %s is a layer of the chain below it", ErrInvalidProposal,
			l.LayerID)
	}
	for _, p := range l.Packs {
		if under, ok := base[p]; ok {
			return "", "", fmt.Errorf("%w: layer.packs: pack %s is its base layer %s's already — a layer covers "+
				"the packs its base does not", ErrInvalidProposal, p, under)
		}
	}

	live, err := livePacksOf(ctx, tx, c.RepoID)
	if err != nil {
		return "", "", err
	}
	// LIVE AFTER THE RECORD: the repository's live packs less the inputs it
	// retires, and its outputs.
	after := map[string]bool{}
	for _, p := range live {
		if !retired[p.id] {
			after[p.checksum] = true
		}
	}
	for _, out := range c.Add {
		after[out.GitChecksum] = true
	}
	for _, p := range l.Packs {
		if !after[p] {
			return RefusalStale, fmt.Sprintf("pack %s the layer covers is no longer live: a compaction moved it "+
				"first; the next pass plans again", p), nil
		}
	}
	for _, p := range live {
		if retired[p.id] || p.added > c.Seen.Packed() {
			continue
		}
		if _, ok := base[p.checksum]; ok {
			continue
		}
		if _, found := slices.BinarySearch(l.Packs, p.checksum); !found {
			return RefusalStale, fmt.Sprintf("pack %s, live at the plan's position %s, is in neither the layer "+
				"nor its base, so the layer would not be closed; the next pass plans again", p.checksum, c.Seen), nil
		}
	}
	for i, part := range l.Parts {
		if detail, expired := uploadExpired("the part", part.Object, now, "the next pass builds the layer "+
			"again"); expired {
			return RefusalUploadExpired, fmt.Sprintf("layer %s, part %d: %s", l.LayerID, i, detail), nil
		}
	}
	return "", "", nil
}

// baseName words a layer's base for a refusal: its id, or the chain's root.
func baseName(id string) string {
	if id == "" {
		return "the chain's root"
	}
	return "layer " + id
}

// chainNames words a chain for a refusal: its layers' ids, root first.
func chainNames(chain []LayerRow) string {
	if len(chain) == 0 {
		return "empty"
	}
	ids := make([]string, len(chain))
	for i, row := range chain {
		ids[i] = row.Layer.LayerID
	}
	return "[" + strings.Join(ids, " ") + "]"
}

// layerApplied reports whether the layer comp publishes is in chain as the
// record at at added it — what a publication is answered applied by beside its
// outputs ([Committer.appliedCompaction]).
func layerApplied(chain []LayerRow, comp Compaction, at statelog.Position) bool {
	return slices.ContainsFunc(chain, func(row LayerRow) bool {
		return row.Layer.LayerID == comp.Layer.LayerID && row.Added.Packed() == at.Packed()
	})
}

// The publication's reads, each registered in plan_test.go: a repository's
// chain whole and its live packs, each by its table's key.
const (
	layersOfSQL = `SELECT seq, layer_id, base_id, graph_id, packs, midx_bytes, bitmap_bytes, graph_bytes,
		objects, commits, git, took_ms, added_position FROM code_layers WHERE repo_id = ? ORDER BY seq`
	livePacksSQL = `SELECT pack_id, git_checksum, objects, added_position FROM code_packs
		WHERE repo_id = ? AND part = 'pack' AND state = 'live'`
)
