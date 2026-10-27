package code

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A PUBLICATION'S DECIDE AND APPLY, case by case, over one node's estate with
// no broker between: records built here, the decide run in a snapshot of the
// rows a case seeds.

// sumOf is the git checksum of the n-th pack [estate.seed] wrote for id.
func sumOf(id string, n int) string {
	return packOf(fmt.Sprintf("%s-pack-%06d", id, n)).GitChecksum
}

// idOf is the pack id of the n-th pack [estate.seed] wrote for id.
func idOf(id string, n int) string { return fmt.Sprintf("%s-pack-%06d", id, n) }

// layerOver is a published layer named for name, valid by [LayerRef.Check],
// over packs (git checksums, sorted here) on base, replacing replaces, its
// bytes one part whose key was minted at minted.
func layerOver(name, base string, replaces []string, minted time.Time, packs ...string) LayerRef {
	sorted := slices.Clone(packs)
	slices.Sort(sorted)
	return LayerRef{
		LayerID: oid("layer " + name), BaseID: base, Replaces: replaces, Packs: sorted,
		Parts:     []LayerPart{{Object: objstore.KeyAt(minted), Hash: objstore.HashOf([]byte(name)), Size: 120}},
		MidxBytes: 100, BitmapBytes: 20, Objects: int64(len(packs)), Git: "2.56.0", TookMs: 7,
	}
}

// publication is a compaction of repository id carrying l, planned at seen,
// repacking retire into add.
func publication(id string, seen uint64, l LayerRef, retire []string, add ...PackRef) Compaction {
	cid := statelog.NewOpID(applyClock, "")
	for i := range add {
		add[i].PackID = DeriveOutputPackID(cid, i)
	}
	return Compaction{RepoID: id, CompactionID: cid, Node: "node-a", Seen: codeAt(seen), Retire: retire,
		Add: add, Layer: &l}
}

// recordOf is c as the record that carries it.
func recordOf(t testing.TB, c Compaction) built {
	return rec(t, RepoSubject(c.RepoID), OpCompact, Scope{RepoPath(c.RepoID)}, c)
}

// chainOf is the ids of id's published chain on e, root first, read with the
// apply's own reader.
func (e *estate) chainOf(id string) []string {
	e.t.Helper()
	var out []string
	if err := e.db.Reader().Read(context.Background(), func(tx *sql.Tx) error {
		chain, err := layersOf(context.Background(), tx, id)
		for _, row := range chain {
			out = append(out, row.Layer.LayerID)
		}
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// dump is every row a query answers on e, one line a row — what two copies of
// one log are compared by.
func (e *estate) dump(query string, args ...any) string {
	e.t.Helper()
	var out strings.Builder
	if err := e.db.Reader().Read(context.Background(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(context.Background(), query, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		columns, err := rows.Columns()
		if err != nil {
			return err
		}
		values := make([]any, len(columns))
		for i := range values {
			values[i] = new(any)
		}
		for rows.Next() {
			if err := rows.Scan(values...); err != nil {
				return err
			}
			for _, v := range values {
				value := *(v.(*any))
				if b, ok := value.([]byte); ok {
					value = string(b)
				}
				fmt.Fprintf(&out, "%v|", value)
			}
			out.WriteByte('\n')
		}
		return rows.Err()
	}); err != nil {
		e.t.Fatalf("%q: %v", query, err)
	}
	return out.String()
}

// layersDump is every row of a repository's chain, its parts and the five
// columns of its row a publication writes.
func (e *estate) layersDump(id string) string {
	e.t.Helper()
	return e.dump(`SELECT * FROM code_layers WHERE repo_id = ? ORDER BY seq`, id) +
		e.dump(`SELECT * FROM code_layer_parts WHERE repo_id = ? ORDER BY layer_id, part`, id) +
		e.dump(`SELECT published_packs, published_objects, published_at, publish_took_ms, layers_position
			FROM code_repos WHERE id = ?`, id)
}

// A LAYER ROW IS CHECKED, field by field, before any byte moves: every
// refusal [ErrInvalidLayer] naming the field that is wrong, and a layer every
// rule admits passes.
//
// Mutation: let Check accept an unsorted pack list, and the case naming
// packs[1] passes.
func TestALayerRowIsChecked(t *testing.T) {
	t.Parallel()
	good := layerOver("l1", "", nil, applyClock, oid("p1"), oid("p2"))
	if err := good.Check(); err != nil {
		t.Fatalf("a layer every rule admits: %v", err)
	}
	many := make([]string, MaxPacksPerRepo+1)
	for i := range many {
		many[i] = fmt.Sprintf("%040x", i+1)
	}
	for name, tc := range map[string]struct {
		edit  func(*LayerRef)
		field string
	}{
		"a layer id no checksum is": {func(l *LayerRef) { l.LayerID = "abc" }, "layer_id"},
		"git's zero id":             {func(l *LayerRef) { l.LayerID = ZeroOID }, "layer_id"},
		"a base no checksum is":     {func(l *LayerRef) { l.BaseID = "below" }, "base_id"},
		"a base that is the layer":  {func(l *LayerRef) { l.BaseID = l.LayerID }, "base_id"},
		"a replaced layer no checksum is": {func(l *LayerRef) { l.Replaces = []string{"x"} },
			"replaces[0]"},
		"a replaced layer named twice": {func(l *LayerRef) { l.Replaces = []string{oid("r"), oid("r")} },
			"replaces[1]"},
		"a graph id over no graph": {func(l *LayerRef) { l.GraphID = oid("g") }, "graph_id"},
		"a graph with no id": {func(l *LayerRef) {
			l.GraphBytes, l.Commits, l.Parts[0].Size = 10, 3, 130
		}, "graph_id"},
		"commits with no graph": {func(l *LayerRef) { l.Commits = 2 }, "commits"},
		"a graph with no commit": {func(l *LayerRef) {
			l.GraphID, l.GraphBytes, l.Parts[0].Size = oid("g"), 10, 130
		}, "commits"},
		"no multi-pack-index":                 {func(l *LayerRef) { l.MidxBytes, l.Parts[0].Size = 0, 20 }, "midx_bytes"},
		"no bitmap":                           {func(l *LayerRef) { l.BitmapBytes, l.Parts[0].Size = 0, 100 }, "bitmap_bytes"},
		"no object":                           {func(l *LayerRef) { l.Objects = 0 }, "objects"},
		"git's whole version line":            {func(l *LayerRef) { l.Git = "git version 2.56.0" }, "git"},
		"a release with a leading zero":       {func(l *LayerRef) { l.Git = "2.056.0" }, "git"},
		"a release of two numbers":            {func(l *LayerRef) { l.Git = "2.56" }, "git"},
		"a build that took less than nothing": {func(l *LayerRef) { l.TookMs = -1 }, "took_ms"},
		"no pack":                             {func(l *LayerRef) { l.Packs = nil }, "packs"},
		"more packs than a repository holds":  {func(l *LayerRef) { l.Packs = many }, "packs"},
		"a pack no checksum is":               {func(l *LayerRef) { l.Packs = []string{"p"} }, "packs[0]"},
		"packs out of order":                  {func(l *LayerRef) { l.Packs = []string{l.Packs[1], l.Packs[0]} }, "packs[1]"},
		"a pack named twice":                  {func(l *LayerRef) { l.Packs = []string{oid("p1"), oid("p1")} }, "packs[1]"},
		"no part":                             {func(l *LayerRef) { l.Parts = nil }, "parts"},
		"more parts than a layer is cut into": {func(l *LayerRef) {
			l.Parts = slices.Repeat(l.Parts, MaxLayerParts+1)
		}, "parts"},
		"a part naming no object": {func(l *LayerRef) { l.Parts[0].Object = objstore.Key{} }, "parts[0]"},
		"a part short of the cut before the last": {func(l *LayerRef) {
			l.Parts = []LayerPart{l.Parts[0], l.Parts[0]}
			l.Parts[0].Size, l.Parts[1].Size = 60, 60
		}, "parts[0].size"},
		"an empty last part": {func(l *LayerRef) {
			l.Parts = []LayerPart{l.Parts[0], l.Parts[0]}
			l.Parts[0].Size, l.Parts[1].Size = LayerPartBytes, 0
			l.MidxBytes = LayerPartBytes - l.BitmapBytes
		}, "parts[1].size"},
		"parts that do not cut the sections": {func(l *LayerRef) { l.Parts[0].Size = 119 }, "parts"},
	} {
		l := good
		l.Packs, l.Parts, l.Replaces = slices.Clone(good.Packs), slices.Clone(good.Parts), slices.Clone(good.Replaces)
		tc.edit(&l)
		err := l.Check()
		if !errors.Is(err, ErrInvalidLayer) || !strings.Contains(err.Error(), "malformed: "+tc.field+":") {
			t.Errorf("%s: Check = %v, want ErrInvalidLayer naming %s", name, err, tc.field)
		}
	}
}

// A PUBLICATION IS DECIDED OVER THE CHAIN IT BUILDS ON, by every rule of the
// layer's own: the layers it replaces must be the chain's top and its base the
// one below (6a, else stale); the chain it leaves no longer than
// MaxPublishedLayers (6b, else an error); every pack it names live (6c, else
// stale); every pack live as of its plan that its base does not name named by
// it (6d, else stale) — but a pack added after its plan, which no builder saw,
// is no reason to refuse it; no pack of its base, and not its base's id (6e,
// else an error); and each part's key minted in time (6f, else
// upload_expired).
//
// Mutation: drop 6d's exemption of a pack added after the plan, and the
// publication planned before the late push is refused stale.
func TestAPublicationIsDecidedOverTheChainItBuildsOn(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	e.seed("repo-a", 0, 0, 6)
	root := layerOver("root", "", nil, applyClock, sumOf("repo-a", 0), sumOf("repo-a", 1), sumOf("repo-a", 2),
		sumOf("repo-a", 3))
	e.apply(recordOf(t, publication("repo-a", e.next-1, root, nil)))
	seen := e.next - 1
	e.apply(pushOf(t, "repo-a", pushEntry("late", move("refs/heads/late", ZeroOID, oid("L")))))
	late := packOf("late").GitChecksum
	top := func(minted time.Time, packs ...string) LayerRef {
		return layerOver("top", root.LayerID, nil, minted, packs...)
	}
	for name, tc := range map[string]struct {
		c       Compaction
		refusal Refusal
		detail  string
		invalid string // an ErrInvalidProposal naming this, rather than a refusal
	}{
		// PLANNED BEFORE THE LATE PUSH, which no builder saw: the layer
		// leaves its pack outside the chain and is decided.
		"a layer planned before a push, its pack left outside": {c: publication("repo-a", seen,
			top(applyClock, sumOf("repo-a", 4), sumOf("repo-a", 5)), nil)},
		"a layer planned after a push, with its pack": {c: publication("repo-a", seen+1,
			top(applyClock, sumOf("repo-a", 4), sumOf("repo-a", 5), late), nil)},
		"a layer planned after a push, its pack left out": {c: publication("repo-a", seen+1,
			top(applyClock, sumOf("repo-a", 4), sumOf("repo-a", 5)), nil),
			refusal: RefusalStale, detail: "pack " + late + ", live at the plan's position"},
		"a chain that moved under it": {c: publication("repo-a", seen,
			layerOver("again", "", nil, applyClock, sumOf("repo-a", 4), sumOf("repo-a", 5)), nil),
			refusal: RefusalStale, detail: "moved under the plan"},
		"a pack no longer live": {c: publication("repo-a", seen,
			top(applyClock, sumOf("repo-a", 4), sumOf("repo-a", 5), oid("gone")), nil),
			refusal: RefusalStale, detail: "is no longer live"},
		"a pack live at its plan that it leaves out": {c: publication("repo-a", seen,
			top(applyClock, sumOf("repo-a", 4)), nil),
			refusal: RefusalStale, detail: "pack " + sumOf("repo-a", 5) + ", live at the plan's position"},
		"a pack its base names": {c: publication("repo-a", seen,
			top(applyClock, sumOf("repo-a", 0), sumOf("repo-a", 4), sumOf("repo-a", 5)), nil),
			invalid: "is its base layer"},
		"a part uploaded too long ago": {c: publication("repo-a", seen,
			top(applyClock.Add(-objstore.RecordWithin), sumOf("repo-a", 4), sumOf("repo-a", 5)), nil),
			refusal: RefusalUploadExpired, detail: "the part was uploaded at"},
	} {
		d, err := compactionDecidedIn(t, e, tc.c, applyClock)
		switch {
		case tc.invalid != "":
			if !errors.Is(err, ErrInvalidProposal) || !strings.Contains(err.Error(), tc.invalid) {
				t.Errorf("%s: the decide answered %v (%q), want ErrInvalidProposal saying %q", name, err,
					d.refusal, tc.invalid)
			}
		case err != nil:
			t.Errorf("%s: %v", name, err)
		case d.refusal != tc.refusal || !strings.Contains(d.detail, tc.detail):
			t.Errorf("%s: the decide answered %q (%q), want %q saying %q", name, d.refusal, d.detail,
				tc.refusal, tc.detail)
		}
	}
	// 6b, A CHAIN AT ITS BOUND: one more layer on MaxPublishedLayers is a
	// planner's miscount, refused before any record.
	full := openApplyEstate(t)
	full.apply(create(t, "repo-b", "b"))
	full.seed("repo-b", 0, 0, MaxPublishedLayers+1)
	base := ""
	for i := range MaxPublishedLayers {
		l := layerOver(fmt.Sprint("b", i), base, nil, applyClock, sumOf("repo-b", i))
		full.apply(recordOf(t, publication("repo-b", full.next-1, l, nil)))
		base = l.LayerID
	}
	if got := len(full.chainOf("repo-b")); got != MaxPublishedLayers {
		t.Fatalf("the premise: the chain holds %d layers, want %d", got, MaxPublishedLayers)
	}
	past := publication("repo-b", full.next-1,
		layerOver("past", base, nil, applyClock, sumOf("repo-b", MaxPublishedLayers)), nil)
	if _, err := compactionDecidedIn(t, full, past, applyClock); !errors.Is(err, ErrInvalidProposal) ||
		!strings.Contains(err.Error(), "MaxPublishedLayers") {
		t.Errorf("a layer past the chain's bound: %v, want ErrInvalidProposal naming MaxPublishedLayers", err)
	}

	// 6e, ITS BASE'S ID: a layer can be no layer below it.
	again := publication("repo-b", full.next-1, layerOver("b0", base, nil, applyClock,
		sumOf("repo-b", MaxPublishedLayers)), nil)
	again.Layer.Replaces = []string{base}
	again.Layer.BaseID = full.chainOf("repo-b")[MaxPublishedLayers-2]
	if _, err := compactionDecidedIn(t, full, again, applyClock); !errors.Is(err, ErrInvalidProposal) ||
		!strings.Contains(err.Error(), "is a layer of the chain below it") {
		t.Errorf("a layer named as a layer of its base: %v, want ErrInvalidProposal", err)
	}
}

// A COMPACTION NEVER RETIRES A PUBLISHED PACK: a tail compaction of packs a
// layer names is refused stale, naming the layer — every git node's chain is
// loadable only over every pack it names — while a publication that replaces
// that layer may repack them.
//
// Mutation: skip the chain's read in the inputs' rule, and the tail
// compaction is decided.
func TestACompactionNeverRetiresAPublishedPack(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	e.seed("repo-a", 0, 0, 6)
	root := layerOver("root", "", nil, applyClock, sumOf("repo-a", 0), sumOf("repo-a", 1), sumOf("repo-a", 2),
		sumOf("repo-a", 3))
	e.apply(recordOf(t, publication("repo-a", e.next-1, root, nil)))

	cid := statelog.NewOpID(applyClock, "")
	tail := Compaction{RepoID: "repo-a", CompactionID: cid, Node: "node-a", Seen: codeAt(e.next - 1),
		Retire: []string{idOf("repo-a", 0), idOf("repo-a", 1)}, Add: []PackRef{outputOf(cid, 0, 2, applyClock)}}
	d, err := compactionDecidedIn(t, e, tail, applyClock)
	if err != nil || d.refusal != RefusalStale || !strings.Contains(d.detail, "is in published layer "+root.LayerID) {
		t.Fatalf("a tail compaction of published packs answered %q (%q), %v; want stale naming the layer",
			d.refusal, d.detail, err)
	}

	out := outputOf("", 0, 2, applyClock)
	merged := publication("repo-a", e.next-1, layerOver("merged", "", []string{root.LayerID}, applyClock,
		out.GitChecksum, sumOf("repo-a", 2), sumOf("repo-a", 3), sumOf("repo-a", 4), sumOf("repo-a", 5)),
		[]string{idOf("repo-a", 0), idOf("repo-a", 1)}, out)
	if d, err := compactionDecidedIn(t, e, merged, applyClock); err != nil || d.refusal != "" {
		t.Fatalf("a publication replacing the layer and repacking its packs answered %q (%q), %v; want decided",
			d.refusal, d.detail, err)
	}
}

// A PUBLICATION REPLACES THE TOP OF THE CHAIN: a layer-only record adds a
// layer above its base, and a repacking one that replaces the two top layers
// removes them and their parts, takes their place — its seq its base's plus
// one, the chain contiguous — and the repository's published packs and
// objects are recounted from the rows, with the publication's instant, its
// build and its position.
//
// Mutation: place the layer above the chain's top rather than its base, and
// the replacement lands at seq 4.
func TestAPublicationReplacesTheTopOfTheChain(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	e.seed("repo-a", 0, 0, 8)
	l1 := layerOver("l1", "", nil, applyClock, sumOf("repo-a", 0), sumOf("repo-a", 1), sumOf("repo-a", 2),
		sumOf("repo-a", 3))
	l2 := layerOver("l2", l1.LayerID, nil, applyClock, sumOf("repo-a", 4), sumOf("repo-a", 5))
	l3 := layerOver("l3", l2.LayerID, nil, applyClock, sumOf("repo-a", 6))
	for _, l := range []LayerRef{l1, l2, l3} {
		e.apply(recordOf(t, publication("repo-a", e.next-1, l, nil)))
	}
	if got, want := e.chainOf("repo-a"), []string{l1.LayerID, l2.LayerID, l3.LayerID}; !slices.Equal(got, want) {
		t.Fatalf("the chain after three layer-only publications is %v, want %v", got, want)
	}
	// THE PACK NO LAYER COVERS: 7 is the tail.
	if got := e.num(`SELECT published_packs FROM code_repos WHERE id = 'repo-a'`); got != 7 {
		t.Fatalf("published_packs is %d after three layers over seven packs, want 7", got)
	}

	out := outputOf("", 0, 3, applyClock)
	merged := layerOver("merged", l1.LayerID, []string{l2.LayerID, l3.LayerID}, applyClock, out.GitChecksum,
		sumOf("repo-a", 7))
	merged.TookMs = 4321
	p := publication("repo-a", e.next-1, merged, []string{idOf("repo-a", 4), idOf("repo-a", 5),
		idOf("repo-a", 6)}, out)
	at := codeAt(e.next)
	rows := e.applyCounting(recordOf(t, p))
	// THREE INPUTS RETIRED AND REMOVED, ONE OUTPUT, THE COUNTERS; TWO LAYERS
	// AND THEIR PARTS REMOVED, THE LAYER AND ITS PART WRITTEN.
	if want := 3 + 3 + 1 + 1 + 2 + 2 + 1 + 1; rows != want || rows > compactDecidedRows(3, 1, &merged) {
		t.Fatalf("the publication reported %d rows, want %d — at most compactDecidedRows %d", rows, want,
			compactDecidedRows(3, 1, &merged))
	}
	if got, want := e.chainOf("repo-a"), []string{l1.LayerID, merged.LayerID}; !slices.Equal(got, want) {
		t.Fatalf("the chain after the merge is %v, want %v", got, want)
	}
	if got := e.dump(`SELECT seq, base_id FROM code_layers WHERE repo_id = 'repo-a' ORDER BY seq`); got !=
		"1||\n2|"+l1.LayerID+"|\n" {
		t.Fatalf("the chain's places are\n%s, want seq 1 and seq 2 above it", got)
	}
	if got := e.dump(`SELECT layer_id, part FROM code_layer_parts WHERE repo_id = 'repo-a' ORDER BY layer_id`); got !=
		strings.Join(sorted(l1.LayerID+"|0|", merged.LayerID+"|0|"), "\n")+"\n" {
		t.Fatalf("the parts after the merge are\n%s, want the root's and the merged layer's alone", got)
	}
	// RECOUNTED: the root's four, the output and the last seeded pack — and
	// their objects, a seeded pack's one each and the output's three.
	want := fmt.Sprintf("6|8|%d|4321|%d|\n", store.EncodeTime(applyClock), at.Packed())
	if got := e.dump(`SELECT published_packs, published_objects, published_at, publish_took_ms, layers_position
		FROM code_repos WHERE id = 'repo-a'`); got != want {
		t.Fatalf("the repository's publication columns are %q, want %q", got, want)
	}
	if live, published := e.num(`SELECT live_packs FROM code_repos WHERE id = 'repo-a'`),
		e.num(`SELECT published_packs FROM code_repos WHERE id = 'repo-a'`); live != published {
		t.Fatalf("live_packs %d and published_packs %d: every live pack is published after the merge", live,
			published)
	}
}

// sorted is ss in order.
func sorted(ss ...string) []string {
	out := slices.Clone(ss)
	slices.Sort(out)
	return out
}

// decided decides c over e's rows at the decide's clock and applies its
// record when the decide formed one, answering the refusal.
func (e *estate) decided(t *testing.T, c Compaction) Refusal {
	t.Helper()
	d, err := compactionDecidedIn(t, e, c, applyClock)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if d.refusal == "" {
		e.apply(recordOf(t, c))
	}
	return d.refusal
}

// livePublished fails t unless every pack a layer of id's chain names is a
// live pack's — the invariant decide rules 2 and 6c keep.
func (e *estate) livePublished(t *testing.T, id, after string) {
	t.Helper()
	if n := e.num(`SELECT COUNT(*) FROM code_layers AS l, json_each(l.packs) AS j
		WHERE l.repo_id = ?1 AND CAST(j.value AS TEXT) NOT IN
			(SELECT git_checksum FROM code_packs WHERE repo_id = ?1)`, id); n != 0 {
		t.Fatalf("after %s, %d packs a layer names are no live pack's", after, n)
	}
}

// EVERY LAYER NAMES ONLY LIVE PACKS: through a sequence of compactions and
// publications, each decided over the rows and applied where the decide
// formed it, every pack a layer of the chain names stays a live pack's — the
// tail compaction of published packs refused, the repacking publication that
// replaces their layer applied.
//
// Mutation: drop the chain's read from the inputs' rule, and the tail
// compaction retires two of the root's packs.
func TestEveryLayerNamesOnlyLivePacks(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	e.seed("repo-a", 0, 0, 4)
	root := layerOver("root", "", nil, applyClock, sumOf("repo-a", 0), sumOf("repo-a", 1), sumOf("repo-a", 2),
		sumOf("repo-a", 3))
	if r := e.decided(t, publication("repo-a", e.next-1, root, nil)); r != "" {
		t.Fatalf("the root publication was refused %q", r)
	}
	e.livePublished(t, "repo-a", "the root")
	// THE TAIL: four pushes after the root's plan, outside every layer.
	var pushed []string
	for i := range 4 {
		id := fmt.Sprintf("tail-%d", i)
		e.apply(pushOf(t, "repo-a", pushEntry(id, move("refs/heads/"+id, ZeroOID, oid(id)))))
		pushed = append(pushed, id)
	}

	cid := statelog.NewOpID(applyClock, "")
	if r := e.decided(t, Compaction{RepoID: "repo-a", CompactionID: cid, Node: "node-a", Seen: codeAt(e.next - 1),
		Retire: []string{idOf("repo-a", 0), idOf("repo-a", 1)},
		Add:    []PackRef{outputOf(cid, 0, 2, applyClock)}}); r != RefusalStale {
		t.Fatalf("a tail compaction of published packs answered %q, want stale", r)
	}
	e.livePublished(t, "repo-a", "a tail compaction of published packs")

	cid = statelog.NewOpID(applyClock, "")
	if r := e.decided(t, Compaction{RepoID: "repo-a", CompactionID: cid, Node: "node-a", Seen: codeAt(e.next - 1),
		Retire: sorted(pushed[0], pushed[1]), Add: []PackRef{outputOf(cid, 0, 2, applyClock)}}); r != "" {
		t.Fatalf("a tail compaction of unpublished packs answered %q, want decided", r)
	}
	e.livePublished(t, "repo-a", "a tail compaction of unpublished packs")

	var tail []string
	for _, p := range e.livePackChecksums("repo-a") {
		if !slices.Contains(root.Packs, p) {
			tail = append(tail, p)
		}
	}
	upper := layerOver("upper", root.LayerID, nil, applyClock, tail...)
	if r := e.decided(t, publication("repo-a", e.next-1, upper, nil)); r != "" {
		t.Fatalf("a layer over the tail answered %q", r)
	}
	e.livePublished(t, "repo-a", "a layer over the tail")

	out := outputOf("", 0, 2, applyClock)
	merged := layerOver("merged", "", []string{root.LayerID, upper.LayerID}, applyClock,
		append([]string{out.GitChecksum, sumOf("repo-a", 2), sumOf("repo-a", 3)}, tail...)...)
	if r := e.decided(t, publication("repo-a", e.next-1, merged, []string{idOf("repo-a", 0), idOf("repo-a", 1)},
		out)); r != "" {
		t.Fatalf("a publication replacing the whole chain and repacking two of its packs answered %q", r)
	}
	e.livePublished(t, "repo-a", "a repacking publication")
	if got := e.chainOf("repo-a"); !slices.Equal(got, []string{merged.LayerID}) {
		t.Fatalf("the chain is %v, want the merged layer alone", got)
	}
}

// livePackChecksums is every live pack's checksum of id on e, sorted.
func (e *estate) livePackChecksums(id string) []string {
	e.t.Helper()
	got := strings.Fields(strings.ReplaceAll(e.dump(`SELECT git_checksum FROM code_packs WHERE repo_id = ?
		ORDER BY git_checksum`, id), "|", ""))
	return got
}

// THE LAYER GUARD APPLIES NOTHING OVER A WRONG BASE: a publication whose
// replaced layers are not the chain's top, or whose base is not the layer
// below them, changes no row — not its layer, and not its repack either,
// which would retire the packs of layers that stay — identically on every
// copy; and a publication applied again, at a later position the version
// guard does not stop, finds its layer in the chain and changes nothing.
//
// Mutation: guard only the layer's half, and the repack over the wrong base
// retires its inputs.
func TestTheLayerGuardAppliesNothingOverAWrongBase(t *testing.T) {
	t.Parallel()
	var copies []*estate
	for range 2 {
		e := openApplyEstate(t)
		e.apply(create(t, "repo-a", "a"))
		e.seed("repo-a", 0, 0, 6)
		copies = append(copies, e)
	}
	root := layerOver("root", "", nil, applyClock, sumOf("repo-a", 0), sumOf("repo-a", 1))
	applied := recordOf(t, publication("repo-a", 1, root, nil))
	out := outputOf("", 0, 2, applyClock)
	astray := recordOf(t, publication("repo-a", 2, layerOver("astray", oid("layer nobody"), []string{oid("x")},
		applyClock, out.GitChecksum, sumOf("repo-a", 4), sumOf("repo-a", 5)),
		[]string{idOf("repo-a", 2), idOf("repo-a", 3)}, out))
	whole := func(e *estate) string {
		return e.layersDump("repo-a") + e.dump(`SELECT * FROM code_packs WHERE repo_id = 'repo-a' ORDER BY pack_id`) +
			e.dump(`SELECT * FROM code_packs_retired WHERE repo_id = 'repo-a'`) +
			e.dump(`SELECT live_packs, live_bytes, live_objects FROM code_repos WHERE id = 'repo-a'`)
	}
	var after []string
	for _, e := range copies {
		e.apply(applied)
		before := whole(e)
		if rows := e.applyCounting(astray); rows != 0 {
			t.Errorf("a publication over a base the chain does not hold reported %d rows, want none", rows)
		}
		if got := whole(e); got != before {
			t.Errorf("a publication over a wrong base changed the rows:\nbefore\n%s\nafter\n%s", before, got)
		}
		if rows := e.applyCounting(applied); rows != 0 {
			t.Errorf("the root publication applied again reported %d rows, want none", rows)
		}
		if got := whole(e); got != before {
			t.Errorf("the root publication applied again changed the rows:\nbefore\n%s\nafter\n%s", before, got)
		}
		after = append(after, whole(e))
	}
	if after[0] != after[1] {
		t.Fatalf("two copies of one log hold different rows:\n%s\n%s", after[0], after[1])
	}
}

// THE LAYERS OF A REPOSITORY ARE REPRODUCIBLE: three copies applying the same
// records — a root, a layer on it, a publication that replaces it and repacks
// — hold byte-identical chains, parts and publication columns, since every
// value the apply writes is the record's or its broker instant.
//
// Mutation: stamp published_at with the applier's clock, and the copies
// differ.
func TestTheLayersOfARepositoryAreReproducible(t *testing.T) {
	t.Parallel()
	root := layerOver("root", "", nil, applyClock, sumOf("repo-a", 0), sumOf("repo-a", 1), sumOf("repo-a", 2))
	upper := layerOver("upper", root.LayerID, nil, applyClock, sumOf("repo-a", 3))
	out := outputOf("", 0, 2, applyClock)
	merged := layerOver("merged", root.LayerID, []string{upper.LayerID}, applyClock, out.GitChecksum,
		sumOf("repo-a", 5))
	records := []built{
		recordOf(t, publication("repo-a", 1, root, nil)),
		recordOf(t, publication("repo-a", 2, upper, nil)).at(applyClock.Add(time.Minute)),
		recordOf(t, publication("repo-a", 3, merged, []string{idOf("repo-a", 3), idOf("repo-a", 4)},
			out)).at(applyClock.Add(time.Hour)),
	}
	var dumps []string
	for range 3 {
		e := openApplyEstate(t)
		e.apply(create(t, "repo-a", "a"))
		e.seed("repo-a", 0, 0, 6)
		e.apply(records...)
		dumps = append(dumps, e.layersDump("repo-a"))
	}
	if dumps[0] != dumps[1] || dumps[1] != dumps[2] {
		t.Fatalf("three copies of one log hold different chains:\n%s\n%s\n%s", dumps[0], dumps[1], dumps[2])
	}
	if !strings.Contains(dumps[0], fmt.Sprint(store.EncodeTime(applyClock.Add(time.Hour)))) {
		t.Fatalf("the chain's publication instant is not the last record's broker instant:\n%s", dumps[0])
	}
}
