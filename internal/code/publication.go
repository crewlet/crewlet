package code

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

// A PUBLICATION, DECIDED: a compaction that carries a published layer
// ([Compaction.Layer]). A repository's maintainer builds git's own incremental
// multi-pack-index layer — its bitmap, and the commit-graph layer over the same
// packs — once for the fleet, stores its bytes as [LayerPart] objects, and
// proposes it to the repository's committer as a compaction, so the one writer
// of a repository's packs is also the one writer of its published chain. Its
// Retire and Add, when present, are the repack of the non-full packs the layer
// gathers; a publication may carry neither.
//
// # What the decide holds a publication to
//
// Everything [decideCompaction] holds a compaction to — the repository, the
// inputs, the outputs and the arithmetic — with the inputs' rule amended and
// seven rules of the layer's own, each against the rows of the same snapshot:
//
//   - 6g, NO EXPUNGE OPEN, read with the repository's row before anything
//     else: a publication decided while the repository's expunge has begun
//     and not committed ([ExpungeBegin]) is stale, and the next pass plans
//     again once it commits. Its live packs need not be closed then — a ref a
//     push moved between the expunge's scan and its begin may reach an object
//     only a dropping pack holds, until the commit's rescue makes it live —
//     so a layer covering them may cover no history git can bitmap, and one
//     covering a dropping pack would be over a pack the commit retires. Held
//     off until the commit, every layer the chain holds names live packs
//     alone, and the commit keeps every one. The publication due listing
//     ([ReadPublicationDue]) names no repository with an expunge open, so no
//     publisher builds a layer this rule would refuse;
//   - THE INPUTS (rule 2, amended for every compaction): an input's pack is
//     named by NO layer of the chain unless the record replaces that layer —
//     else stale. A layer names its packs by checksum, and a node's chain is
//     loadable only over every pack it names, so retiring one under a layer
//     that stays would break the chain on every git node;
//   - 6a, THE CHAIN IT BUILDS ON: the layers it replaces are the chain's top,
//     in order, and the one below them is its base — else stale: another
//     publication landed first, and the next pass plans again;
//   - 6b, ITS LENGTH: the chain it leaves holds at most [MaxPublishedLayers]
//     — else an error ([ErrInvalidProposal]), a planner that miscounted;
//   - 6c, LIVE: every pack it names is a live pack once the record's own
//     retirements and outputs are counted — else stale: a compaction moved it
//     first;
//   - 6d, COMPLETE: every pack live as of its plan ([Compaction.Seen]) that
//     its base does not name, and that the record does not retire, it names —
//     else stale. This is the closure git's bitmap needs: every push's pack is
//     closed over the packs before it, and a compaction drops no object, so
//     the packs live at one position are closed together — and a layer over
//     fewer of them, the full packs of a cut alone among them, was refused by
//     git and wrote no bitmap. A pack added after the plan was in no
//     builder's snapshot; every pack it depends on was live before it, so it
//     stays outside the chain, which git reads beside it;
//   - 6e, DISJOINT: no pack it names is its base's, and its id is none of its
//     base's layers' — else an error, a builder that named its packs twice;
//   - 6f, ITS PARTS NAMED IN TIME: each part's key was minted within
//     objstore.RecordWithin of the decision — else upload_expired, the push's
//     own rule ([uploadExpired]).
//
// Nothing else is read: no ref, no protection, no push. The chain is at most
// [MaxPublishedLayers] rows read whole by its key, and the live packs one read
// of the repository's own.
//
// # Applied, the chain's top is replaced
//
// The apply (apply_compact.go) removes the layers the record replaces, with
// their parts, adds the new one above its base, and RECOUNTS the repository's
// published packs from the rows — never arithmetic, which a pack row
// byte-identical to a published one (two rows, one checksum) would make drift
// from what git reads; a push's arithmetic carries exactly that drift, in the
// tail, until the next recount (replicated 0085 says why no push recounts).
// Every pack a layer row of an active or archived repository names is then a
// live pack's: rule 2 retires none a layer that stays names, an expunge's
// begin removes every layer over a pack it retires and every layer above that
// one (decide_expunge.go), and 6c names none that is not live.

// Check is what every layer row must be before any byte moves — the
// committer's decide and a git node's install alike — each refusal
// [ErrInvalidLayer] naming the field: LayerID 40 lowercase hex digits and not
// git's zero id; BaseID "" or such an id, and not the layer's own; Replaces
// distinct such ids; GraphID such an id, or "" exactly when GraphBytes is 0;
// Packs 1 to [MaxPacksPerRepo] such checksums, sorted and distinct; 1 to
// [MaxLayerParts] parts, each a whole object, every one but the last exactly
// [LayerPartBytes] and the last 1 to that, summing to [LayerRef.Size];
// MidxBytes and BitmapBytes positive and GraphBytes not negative; Objects at
// least 1, Commits not negative and 0 exactly when GraphBytes is; Git a release
// as git's version line spells its first three numbers ("2.56.0"); TookMs not
// negative.
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

// layerName reports a layer's, a commit-graph layer's or a pack's checksum as
// git names its files: 40 lowercase hex digits, and never git's zero id, which
// names no file.
func layerName(s string) bool { return gitref.ValidOID(s) && s != ZeroOID }

// validRelease reports a git release spelled as git's version line spells its
// first three numbers and gitproc.Version.String writes them back: three
// decimal numbers, each with no sign and no leading zero, joined by dots
// ("2.56.0"). The domain reads no git, so it holds the spelling rather than
// parsing a version line, and a git node reads the field back with gitproc.
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
// the layer a row of it must be ([LayerRef.Check]), every output one of the
// layer's packs — a repack's outputs are packs the layer covers, or the layer
// would name a pack it does not cover — and fewer replaced layers than a chain
// may hold, so the chain the record leaves is never empty above its root.
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

// layersOf is a repository's published chain as its rows hold it, root first
// — every column but its parts, which [Reader.Snapshot] alone reads: at most
// [MaxPublishedLayers] rows, read whole by the table's key.
func layersOf(ctx context.Context, tx *sql.Tx, repoID string) ([]LayerRow, error) {
	rows, err := tx.QueryContext(ctx, layersOfSQL, repoID)
	if err != nil {
		return nil, fmt.Errorf("code: read %s's published layers: %w", repoID, err)
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
			return nil, fmt.Errorf("code: read %s's published layers: %w", repoID, err)
		}
		if err := json.Unmarshal([]byte(packs), &l.Packs); err != nil {
			return nil, fmt.Errorf("code: %s's published layer %s holds packs %q: %w", repoID, l.LayerID, packs, err)
		}
		row.Added = unpacked(added)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("code: read %s's published layers: %w", repoID, err)
	}
	return out, nil
}

// replacedTop reports whether replaces is chain's top, in order, and base the
// layer directly below it ("" where it replaces the whole chain) — the one
// reading of decide rule 6a and the apply's guard, so the committer and every
// applier agree on whether a publication fits the chain it meets.
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

// livePacksOf is every live pack row of a repository, of the one part every
// pack this build stores is — one read of the repository's own rows, by the
// key's prefix: the decide's 6c and 6d read them, and the apply's recount,
// a publication's and an expunge's begin's. LIVE: a dropping pack an expunge
// left is no pack a layer may name or a recount count.
func livePacksOf(ctx context.Context, tx *sql.Tx, repoID string) ([]livePack, error) {
	rows, err := tx.QueryContext(ctx, livePacksSQL, repoID)
	if err != nil {
		return nil, fmt.Errorf("code: read %s's live packs: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []livePack
	for rows.Next() {
		var p livePack
		if err := rows.Scan(&p.id, &p.checksum, &p.objects, &p.added); err != nil {
			return nil, fmt.Errorf("code: read %s's live packs: %w", repoID, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("code: read %s's live packs: %w", repoID, err)
	}
	return out, nil
}

// publishedBy is every pack checksum the layers of chain name but the ones a
// record replaces, each with the layer that names it: what an input may not be
// (rule 2) and, of the base alone, what a layer may not name again (6e).
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

// decideLayer is rules 6a–6f of a publication, over the chain the decide read,
// its inputs' rows as rule 2 found them and the rows tx holds, at now: the
// refusal and its sentence, nothing for a layer that fits, or an error for a
// layer no correct planner forms ([ErrInvalidProposal]).
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
