package code_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // git's hashfile trailer is a SHA-1; the case reseals a file as git would
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/objstore"
)

// layerFixture is a repository's published chain as git itself writes it, by
// the publisher's own commands: two packs — every object of three commits,
// then of the two commits after — a base layer over the first and a tip over
// the second, each an incremental multi-pack-index with its bitmap written
// with --stdin-packs --no-write-chain-file over a chain file naming exactly
// its base, and a commit-graph layer each with changed-path filters version 2
// (--stdin-packs --split=no-merge --changed-paths); and, in a second
// repository holding the same packs, the whole multi-pack-index a node writes
// in own mode, at version 2 (midx.version). Beside each file, what git itself
// said of it.
type layerFixture struct {
	base, tip, whole wroteLayer
	// packs are the two packs' checksums, and objects what show-index counts
	// in each.
	packs   [2]string
	objects [2]int64
	// inPack is what count-objects -v counts in the two packs together.
	inPack int64
	// chain is commit-graph-chain as git wrote it.
	chain []string
}

// wroteLayer is one multi-pack-index with its bitmap — and, for a layer, its
// commit-graph layer — and what git said of them.
type wroteLayer struct {
	midx, bitmap, graph string
	// id is the checksum git printed for the multi-pack-index (a layer) or
	// named its bitmap by (the whole index); graphID the commit-graph
	// layer's line of the chain git wrote.
	id, graphID string
	// selected is how many commits git's bitmap writer says it selected
	// (trace2's num_selected_commits).
	selected int
	// commits is what rev-list counts in the commit-graph layer's pack.
	commits int64
}

// writeLayers writes a layerFixture with the node's git (gittest.RequireNode:
// the pin under every make target).
func writeLayers(t testing.TB) layerFixture {
	t.Helper()
	return writeLayersWith(t, gittest.RequireNode(t))
}

// writeLayersWith writes a layerFixture with the git at git.
func writeLayersWith(t testing.TB, git string) layerFixture {
	t.Helper()
	c := gittest.NewClientWith(t, git)
	work := c.Init(t)
	var commits []string
	for i, files := range []map[string]string{
		{"README.md": "one\n", "src/a.go": "package a\n"},
		{"src/b.go": "package b\n"},
		{"docs/c.md": "three\n", "src/a.go": "package a // three\n"},
		{"src/d/e.go": "package e\n"},
		{"README.md": "five\n"},
	} {
		commits = append(commits, c.Commit(t, work, files, fmt.Sprintf("commit %d", i+1)))
	}
	third, fifth := commits[2], commits[4]
	bare := gittest.Empty(t)
	var g layerFixture
	g.packs[0] = gittest.Pack(t, c, bare, work, third)
	g.packs[1] = gittest.Pack(t, c, bare, work, fifth, "^"+third)
	traces := t.TempDir()

	// THE CHAIN, as the publisher writes it: each layer in a repository whose
	// chain file names exactly its base, its refs at the commits its packs
	// close over, so the bitmap's walk finds every object it needs inside the
	// layer and the layers below it.
	layers := filepath.Join(bare, "objects", "pack", "multi-pack-index.d")
	layer := func(name, pack, base, at string) wroteLayer {
		t.Helper()
		gittest.SetRefs(t, bare, map[string]string{"refs/heads/main": at})
		trace := filepath.Join(traces, name+".json")
		out, stderr, err := c.Env("GIT_TRACE2_EVENT="+trace, "GIT_TRACE2_EVENT_NESTING=100").Try(t, bare,
			strings.NewReader("pack-"+pack+".idx\n"), "-c", "pack.writeBitmapLookupTable=true",
			"multi-pack-index", "write", "--incremental", "--bitmap", "--stdin-packs", "--no-write-chain-file",
			"--base="+base, "--no-progress")
		if err != nil {
			t.Fatalf("git wrote no %s layer: %v\n%s", name, err, stderr)
		}
		id := strings.TrimSpace(out)
		return wroteLayer{
			midx: filepath.Join(layers, "multi-pack-index-"+id+".midx"), id: id,
			bitmap: filepath.Join(layers, "multi-pack-index-"+id+".bitmap"), selected: selectedIn(t, trace),
		}
	}
	writeChain := func(ids ...string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(layers, "multi-pack-index-chain"), []byte(strings.Join(ids, "\n")+"\n"),
			0o644); err != nil {
			t.Fatal(err)
		}
	}
	g.base = layer("base", g.packs[0], "none", third)
	writeChain(g.base.id)
	g.tip = layer("tip", g.packs[1], g.base.id, fifth)
	writeChain(g.base.id, g.tip.id)
	c.Run(t, bare, "multi-pack-index", "verify")
	if out, stderr, err := c.Try(t, bare, nil, "rev-list", "--test-bitmap", fifth); err != nil ||
		!strings.Contains(out+stderr, "OK!") {
		t.Fatalf("git cannot read the chain's bitmaps: %v\n%s%s", err, out, stderr)
	}
	for _, pack := range g.packs {
		if _, stderr, err := c.Try(t, bare, strings.NewReader("pack-"+pack+".idx\n"), "-c",
			"commitGraph.changedPathsVersion=2", "commit-graph", "write", "--stdin-packs", "--split=no-merge",
			"--changed-paths", "--no-progress"); err != nil {
			t.Fatalf("git wrote no commit-graph layer over the pack %s: %v\n%s", pack, err, stderr)
		}
	}
	graphs := filepath.Join(bare, "objects", "info", "commit-graphs")
	g.chain = strings.Fields(string(mustRead(t, filepath.Join(graphs, "commit-graph-chain"))))
	if len(g.chain) != 2 {
		t.Fatalf("git's commit-graph chain is %v, want the two layers written", g.chain)
	}
	c.Run(t, bare, "commit-graph", "verify")
	for i, w := range []*wroteLayer{&g.base, &g.tip} {
		w.graphID, w.graph = g.chain[i], filepath.Join(graphs, "graph-"+g.chain[i]+".graph")
	}
	g.base.commits = revCount(t, c, bare, third)
	g.tip.commits = revCount(t, c, bare, fifth, "^"+third)
	for i, pack := range g.packs {
		idx, err := os.Open(filepath.Join(bare, "objects", "pack", "pack-"+pack+".idx"))
		if err != nil {
			t.Fatal(err)
		}
		out, stderr, err := c.Try(t, bare, idx, "show-index")
		_ = idx.Close()
		if err != nil {
			t.Fatalf("git show-index: %v\n%s", err, stderr)
		}
		// A line an object: its offset, its id and its CRC.
		g.objects[i] = int64(strings.Count(out, "\n"))
	}

	// THE WHOLE INDEX, as own mode writes it, at version 2 so both versions
	// git reads are read from git's own bytes.
	whole := gittest.Empty(t)
	for _, pack := range g.packs {
		for _, ext := range []string{".pack", ".idx", ".rev"} {
			name := "pack-" + pack + ext
			if err := os.Link(filepath.Join(bare, "objects", "pack", name),
				filepath.Join(whole, "objects", "pack", name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	gittest.SetRefs(t, whole, map[string]string{"refs/heads/main": fifth})
	trace := filepath.Join(traces, "whole.json")
	if _, stderr, err := c.Env("GIT_TRACE2_EVENT="+trace, "GIT_TRACE2_EVENT_NESTING=100").Try(t, whole, nil,
		"-c", "midx.version=2", "-c", "pack.writeBitmapLookupTable=true", "multi-pack-index", "write", "--bitmap",
		"--no-progress"); err != nil {
		t.Fatalf("git wrote no whole multi-pack-index: %v\n%s", err, stderr)
	}
	bitmaps, err := filepath.Glob(filepath.Join(whole, "objects", "pack", "multi-pack-index-*.bitmap"))
	if err != nil || len(bitmaps) != 1 {
		t.Fatalf("the whole multi-pack-index's bitmaps: %v, %v", bitmaps, err)
	}
	g.whole = wroteLayer{
		midx: filepath.Join(whole, "objects", "pack", "multi-pack-index"), bitmap: bitmaps[0],
		id:       strings.TrimSuffix(strings.TrimPrefix(filepath.Base(bitmaps[0]), "multi-pack-index-"), ".bitmap"),
		selected: selectedIn(t, trace),
	}
	for _, line := range strings.Split(c.Run(t, whole, "count-objects", "-v"), "\n") {
		if n, ok := strings.CutPrefix(line, "in-pack: "); ok {
			if g.inPack, err = strconv.ParseInt(n, 10, 64); err != nil {
				t.Fatal(err)
			}
		}
	}
	return g
}

// selectedIn is how many commits the bitmap writer whose trace2 event stream
// is at path says it selected.
func selectedIn(t testing.TB, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	selected := -1
	lines := bufio.NewScanner(f)
	lines.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for lines.Scan() {
		var e struct{ Event, Category, Key, Value string }
		if json.Unmarshal(lines.Bytes(), &e) == nil && e.Event == "data" && e.Category == "pack-bitmap-write" &&
			e.Key == "num_selected_commits" {
			if selected, err = strconv.Atoi(e.Value); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := lines.Err(); err != nil || selected < 0 {
		t.Fatalf("the trace at %s names no selected commits (%v)", path, err)
	}
	return selected
}

// revCount is what rev-list counts of revs in dir.
func revCount(t testing.TB, c *gittest.Client, dir string, revs ...string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(strings.TrimSpace(c.Run(t, dir, append([]string{"rev-list", "--count"}, revs...)...)),
		10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// mustRead is the file at path.
func mustRead(t testing.TB, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// parseFile is the file at path through one of the three readers, failing t
// on a refusal.
func parseFile[I any](t *testing.T, path string, parse func(io.ReaderAt, int64) (I, error)) I {
	t.Helper()
	b := mustRead(t, path)
	info, err := parse(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("%s, as git wrote it: %v", path, err)
	}
	return info
}

// THE READERS READ WHAT GIT WROTE, AND SAY WHAT GIT SAYS OF IT. Real git, by
// the publisher's own commands, writes a base and a tip layer, each a
// bitmapped multi-pack-index layer and a commit-graph layer with changed-path
// filters, and the whole multi-pack-index own mode writes, at version 2; git
// accepts every one of them (multi-pack-index verify, rev-list --test-bitmap,
// commit-graph verify). Each reader answers what git itself says of the same
// files: the packs it was handed, the objects show-index and count-objects
// count, the id it printed, named a bitmap by or listed in its chain, the
// commits its bitmap writer selected and rev-list counts, the base the tip was
// written over, and the filters' version it was asked to write. LayLayer
// states the same of a layer's files, as a row's byte fields.
//
// ON BOTH ENDS OF WHAT A NODE RUNS: the node pin, and the node floor's own
// build (gittest.NodeFloor, in gitproc.NodeMinVersion's series), since a
// maintainer on the floor publishes what every node installs, and a format the
// floor's git writes and this reader refused would cost every node its
// bitmap.
//
// Mutation: ParseMidx reads the pack names from the OIDF chunk rather than
// PNAM — red.
func TestTheLayerReaderReadsWhatGitWrites(t *testing.T) {
	t.Parallel()
	for _, arm := range []struct {
		name string
		git  func(testing.TB) string
	}{{"node_pin", gittest.RequireNode}, {"node_floor", gittest.NodeFloor}} {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			readsWhatGitWrote(t, writeLayersWith(t, arm.git(t)))
		})
	}
}

// readsWhatGitWrote is TestTheLayerReaderReadsWhatGitWrites over what one git
// wrote.
func readsWhatGitWrote(t *testing.T, g layerFixture) {
	wantChunks := []string{"PNAM", "OIDF", "OIDL", "OOFF", "RIDX", "BTMP"}
	for _, c := range []struct {
		name    string
		w       wroteLayer
		version int
		packs   []string
		objects int64
	}{
		{"base", g.base, 1, g.packs[:1], g.objects[0]},
		{"tip", g.tip, 1, g.packs[1:], g.objects[1]},
		{"whole", g.whole, 2, sortedOf(g.packs[:]...), g.inPack},
	} {
		m := parseFile(t, c.w.midx, code.ParseMidx)
		var names []string
		for _, p := range c.packs {
			names = append(names, "pack-"+p+".idx")
		}
		if m.Version != c.version || !slices.Equal(m.Packs, names) || m.Objects != c.objects ||
			m.Checksum != c.w.id || !slices.Equal(m.Chunks, wantChunks) {
			t.Errorf("%s: ParseMidx = %+v; git wrote version %d over %v, %d objects, named %s, with the chunks %v",
				c.name, m, c.version, names, c.objects, c.w.id, wantChunks)
		}
		b := parseFile(t, c.w.bitmap, code.ParseBitmap)
		if b.Version != 1 || b.Midx != c.w.id || b.Commits != c.w.selected || b.Options&0x15 != 0x15 {
			t.Errorf("%s: ParseBitmap = %+v; git wrote version 1 of the multi-pack-index %s, %d commits selected, "+
				"its options a full DAG, a hash cache and a lookup table (0x15)", c.name, b, c.w.id, c.w.selected)
		}
	}
	if g.inPack != g.objects[0]+g.objects[1] {
		t.Fatalf("count-objects counts %d in the two packs and show-index %d and %d: the packs share objects, and "+
			"the counts compared above are not the layers'", g.inPack, g.objects[0], g.objects[1])
	}
	for _, c := range []struct {
		name  string
		w     wroteLayer
		bases []string
	}{{"base", g.base, nil}, {"tip", g.tip, g.chain[:1]}} {
		gr := parseFile(t, c.w.graph, code.ParseGraph)
		if gr.Commits != c.w.commits || !slices.Equal(gr.Bases, c.bases) || gr.BloomVersion != 2 ||
			gr.Checksum != c.w.graphID {
			t.Errorf("%s: ParseGraph = %+v; git wrote %d commits over the bases %v with version 2 filters, "+
				"listed in its chain as %s", c.name, gr, c.w.commits, c.bases, c.w.graphID)
		}
		got, err := code.LayLayer(c.w.midx, c.w.bitmap, c.w.graph)
		want := code.LayerRef{LayerID: c.w.id, GraphID: c.w.graphID, Packs: []string{g.packs[0]},
			MidxBytes: size(t, c.w.midx), BitmapBytes: size(t, c.w.bitmap), GraphBytes: size(t, c.w.graph),
			Objects: g.objects[0], Commits: c.w.commits}
		if c.name == "tip" {
			want.Packs, want.Objects = []string{g.packs[1]}, g.objects[1]
		}
		if err != nil || !layersEqual(got, want) {
			t.Errorf("%s: LayLayer = %+v, %v; want %+v", c.name, got, err, want)
		}
	}
	// A build whose packs held no commit the base's graph layers lack wrote
	// no graph layer: the row carries none.
	got, err := code.LayLayer(g.tip.midx, g.tip.bitmap, "")
	if err != nil || got.GraphID != "" || got.GraphBytes != 0 || got.Commits != 0 || got.LayerID != g.tip.id {
		t.Errorf("LayLayer with no graph layer = %+v, %v; want the tip's layer and no graph", got, err)
	}
}

// sortedOf is ss sorted.
func sortedOf(ss ...string) []string {
	out := slices.Clone(ss)
	slices.Sort(out)
	return out
}

// size is the file at path's size.
func size(t testing.TB, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// layersEqual reports two rows the same in every field.
func layersEqual(a, b code.LayerRef) bool {
	return a.LayerID == b.LayerID && a.BaseID == b.BaseID && slices.Equal(a.Replaces, b.Replaces) &&
		a.GraphID == b.GraphID && slices.Equal(a.Packs, b.Packs) && slices.Equal(a.Parts, b.Parts) &&
		a.MidxBytes == b.MidxBytes && a.BitmapBytes == b.BitmapBytes && a.GraphBytes == b.GraphBytes &&
		a.Objects == b.Objects && a.Commits == b.Commits && a.Git == b.Git && a.TookMs == b.TookMs
}

// layerBytes are a layer's three files' bytes, as a git node lays them back
// from the layer's parts; a nil graph is a layer carrying none.
type layerBytes [3][]byte

// filesOf is w's three files' bytes.
func filesOf(t testing.TB, w wroteLayer) layerBytes {
	t.Helper()
	return layerBytes{mustRead(t, w.midx), mustRead(t, w.bitmap), mustRead(t, w.graph)}
}

// check is CheckLayer over l's bytes.
func (l layerBytes) check(row code.LayerRef, bases []string) error {
	var graph io.ReaderAt
	if l[2] != nil {
		graph = bytes.NewReader(l[2])
	}
	return code.CheckLayer(row, bytes.NewReader(l[0]), bytes.NewReader(l[1]), graph, bases)
}

// rowFor is the row a publisher names the layer w by: LayLayer's fields over
// w's files, base, git's release and a build time, and parts cut as
// StoreLayer cuts them (withParts).
func rowFor(t testing.TB, w wroteLayer, base string) code.LayerRef {
	t.Helper()
	row, err := code.LayLayer(w.midx, w.bitmap, w.graph)
	if err != nil {
		t.Fatal(err)
	}
	row.BaseID, row.Git, row.TookMs = base, "2.56.0", 3
	return withParts(row)
}

// withParts is row with parts for its size, cut as StoreLayer cuts them —
// each a key minted now and a digest standing in for its bytes, since
// CheckLayer reads files and never a part.
func withParts(row code.LayerRef) code.LayerRef {
	row.Parts = nil
	for at := int64(0); at < row.Size(); at += code.LayerPartBytes {
		n := min(code.LayerPartBytes, row.Size()-at)
		row.Parts = append(row.Parts, code.LayerPart{Object: objstore.KeyAt(time.Now()),
			Hash: objstore.HashOf([]byte(strconv.FormatInt(at, 10))), Size: n})
	}
	return row
}

// reseal is b with its trailer the SHA-1 of the bytes before it, as git's
// hashfile writes one: a file a case damaged in its body, reading past the
// trailer it would otherwise fail first.
func reseal(b []byte) []byte {
	out := slices.Clone(b)
	if len(out) < sha1.Size {
		return out
	}
	sum := sha1.Sum(out[:len(out)-sha1.Size]) //nolint:gosec // git's trailer
	copy(out[len(out)-sha1.Size:], sum[:])
	return out
}

// trailerHex is the hex of b's last twenty bytes.
func trailerHex(b []byte) string { return hex.EncodeToString(b[len(b)-sha1.Size:]) }

// A CHECK REFUSES EVERY WAY A LAYER'S FILES ARE NOT ITS ROW'S, each refusal
// ErrBadLayer naming the field it differs in, and passes the layer git wrote:
// the tip of the chain, whose commit-graph layer names the base's — so the
// base a commit-graph layer is written over is checked too.
//
// Mutation: CheckLayer skips the PNAM comparison, and the row naming a pack
// the multi-pack-index does not is red.
func TestACheckRefusesEveryMismatch(t *testing.T) {
	t.Parallel()
	g := writeLayers(t)
	tip, base := filesOf(t, g.tip), filesOf(t, g.base)
	row, bases := rowFor(t, g.tip, g.base.id), []string{g.base.graphID}
	if err := tip.check(row, bases); err != nil {
		t.Fatalf("the layer git wrote, with the row its own files state: %v", err)
	}

	edited := func(edit func(*code.LayerRef)) code.LayerRef {
		r := row
		r.Packs = slices.Clone(row.Packs)
		edit(&r)
		return withParts(r)
	}
	with := func(i int, b []byte) layerBytes {
		l := tip
		l[i] = b
		return l
	}
	// The tip's commit-graph layer with filters of version 1: its BDAT
	// header's version, resealed, and named by the row as it now is.
	bdat, _ := gittest.GraphChunk(t, g.tip.graph, "BDAT")
	v1 := slices.Clone(tip[2])
	binary.BigEndian.PutUint32(v1[bdat:], 1)
	v1 = reseal(v1)
	// The tip's multi-pack-index cut inside its table of contents: the
	// header and two rows of the seven, which the row is laid out to.
	short := tip[0][:gittest.ChunkRow(gittest.MidxHeader, 2)]
	flipped := slices.Clone(tip[0])
	flipped[len(flipped)-1] ^= 0xff
	other := strings.Repeat("e", 40)
	// The tip's multi-pack-index with no RIDX chunk — its id renamed in the
	// table of contents, resealed — and named by the row as it now is.
	renamed := filepath.Join(t.TempDir(), "midx")
	if err := os.WriteFile(renamed, tip[0], 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.RenameChunk(t, renamed, gittest.MidxHeader, "RIDX", "RIDZ")
	noRev := reseal(mustRead(t, renamed))
	// The tip's bitmap counting more selected commits than its lookup table
	// has room for, resealed: only the header's count differs.
	crowded := slices.Clone(tip[1])
	binary.BigEndian.PutUint32(crowded[8:12], 1<<20)
	crowded = reseal(crowded)

	for _, c := range []struct {
		name, field string
		files       layerBytes
		row         code.LayerRef
		bases       []string
	}{
		{"a pnam one pack short", "packs", tip,
			edited(func(r *code.LayerRef) { r.Packs = sortedOf(append(r.Packs, other)...) }), bases},
		{"a midx trailer flipped", "midx", with(0, flipped), row, bases},
		{"another layer's id", "layer_id", tip, edited(func(r *code.LayerRef) { r.LayerID = other }), bases},
		{"another count of objects", "objects", tip, edited(func(r *code.LayerRef) { r.Objects++ }), bases},
		{"a midx without its reverse index", "midx", with(0, noRev),
			edited(func(r *code.LayerRef) { r.LayerID = trailerHex(noRev) }), bases},
		{"a bitmap naming another midx", "bitmap.midx", with(1, base[1]),
			edited(func(r *code.LayerRef) { r.BitmapBytes = int64(len(base[1])) }), bases},
		{"a bitmap whose lookup table cannot fit", "bitmap", with(1, crowded), row, bases},
		{"a graph base naming a wrong base", "graph.bases", tip, row, []string{other}},
		{"a graph over a base the chain lacks", "graph.bases", tip, row, nil},
		{"another graph", "graph_id", tip, edited(func(r *code.LayerRef) { r.GraphID = g.base.graphID }), bases},
		{"another count of commits", "commits", tip, edited(func(r *code.LayerRef) { r.Commits++ }), bases},
		{"v1 filters", "graph.filters", with(2, v1),
			edited(func(r *code.LayerRef) { r.GraphID = trailerHex(v1) }), bases},
		{"a section one byte longer", "midx_bytes", with(0, append(slices.Clone(tip[0]), 0)), row, bases},
		{"a section one byte shorter", "bitmap_bytes", with(1, tip[1][:len(tip[1])-1]), row, bases},
		{"a truncated chunk table", "midx", with(0, short),
			edited(func(r *code.LayerRef) { r.MidxBytes = int64(len(short)) }), bases},
		{"a graph the row does not carry", "graph_bytes", tip,
			edited(func(r *code.LayerRef) { r.GraphID, r.GraphBytes, r.Commits = "", 0, 0 }), bases},
		{"a row its own check refuses", "packs[1]", tip,
			edited(func(r *code.LayerRef) { r.Packs = []string{r.Packs[0], r.Packs[0]} }), bases},
	} {
		err := c.files.check(c.row, c.bases)
		if !errors.Is(err, code.ErrBadLayer) || !strings.Contains(err.Error(), ": "+c.field+": ") {
			t.Errorf("%s: CheckLayer = %v, want ErrBadLayer naming %s", c.name, err, c.field)
		}
	}
	// A row its own check refuses is refused as the row is.
	if err := tip.check(edited(func(r *code.LayerRef) { r.Packs = []string{r.Packs[0], r.Packs[0]} }),
		bases); !errors.Is(err, code.ErrInvalidLayer) {
		t.Errorf("a row with a pack named twice: %v, want ErrInvalidLayer beside ErrBadLayer", err)
	}
	// A layer carrying no commit-graph layer is checked with none handed.
	noGraph := edited(func(r *code.LayerRef) { r.GraphID, r.GraphBytes, r.Commits = "", 0, 0 })
	if err := (layerBytes{tip[0], tip[1], nil}).check(noGraph, nil); err != nil {
		t.Errorf("a layer of no commit-graph layer, checked with none: %v", err)
	}
	// LayLayer refuses a build whose files no node would install.
	dir := t.TempDir()
	otherBitmap, v1Graph := filepath.Join(dir, "bitmap"), filepath.Join(dir, "graph")
	for path, body := range map[string][]byte{otherBitmap: base[1], v1Graph: v1} {
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ name, field, bitmap, graph string }{
		{"another midx's bitmap", "bitmap.midx", otherBitmap, g.tip.graph},
		{"v1 filters", "graph.filters", g.tip.bitmap, v1Graph},
	} {
		if _, err := code.LayLayer(g.tip.midx, c.bitmap, c.graph); !errors.Is(err, code.ErrBadLayer) ||
			!strings.Contains(err.Error(), ": "+c.field+": ") {
			t.Errorf("LayLayer over %s = %v, want ErrBadLayer naming %s", c.name, err, c.field)
		}
	}
}

// A FILE THAT CANNOT BE READ IS NOT A BAD LAYER. A disk that fails a read says
// nothing about the bytes, and a refusal the installer would never retry
// would cost the node the layer until the maintainer published it again: the
// read's failure is answered as it came, apart from ErrBadLayer.
//
// Mutation: answer every failure of a read as a refusal of the bytes — red.
func TestAFailedReadIsNotABadLayer(t *testing.T) {
	t.Parallel()
	g := writeLayers(t)
	tip, row := filesOf(t, g.tip), rowFor(t, g.tip, g.base.id)
	failed := errors.New("the disk failed the read")
	for i := range 3 {
		readers := [3]io.ReaderAt{bytes.NewReader(tip[0]), bytes.NewReader(tip[1]), bytes.NewReader(tip[2])}
		readers[i] = failingAt{failed}
		err := code.CheckLayer(row, readers[0], readers[1], readers[2], []string{g.base.graphID})
		if !errors.Is(err, failed) || errors.Is(err, code.ErrBadLayer) {
			t.Errorf("a failed read of the layer's file %d: %v, want the read's failure and not ErrBadLayer", i, err)
		}
	}
}

// failingAt is a file whose every read fails.
type failingAt struct{ err error }

func (f failingAt) ReadAt([]byte, int64) (int, error) { return 0, f.err }

// THE CUT IS ONE RULE BOTH WAYS: a part of a layer lies where its bytes lie in
// the three files back to back, at the shipped LayerPartBytes — the multi-pack
// -index, then its bitmap, then the commit-graph layer, a part ending inside
// one file and the next beginning there — and every byte of every file is in
// exactly one part.
//
// Mutation: a run's offset counted from the layer's start rather than its
// file's — red.
func TestALayerIsCutTheSameWayBothWays(t *testing.T) {
	t.Parallel()
	const p = code.LayerPartBytes
	sections := [3]int64{p + 100, p - 50, 1000}
	want := [][][3]int64{
		{{0, 0, p}},
		{{0, p, 100}, {1, 0, p - 100}},
		{{1, p - 100, 50}, {2, 0, 1000}},
	}
	total := sections[0] + sections[1] + sections[2]
	var covered [3]int64
	for i := range want {
		start := int64(i) * p
		got := code.LayerCut(sections, start, min(p, total-start))
		if !slices.Equal(got, want[i]) {
			t.Errorf("part %d is cut %v, want %v", i, got, want[i])
		}
		for _, run := range got {
			if run[1] != covered[run[0]] {
				t.Errorf("part %d's run in file %d begins at %d, where the parts before it ended at %d", i,
					run[0], run[1], covered[run[0]])
			}
			covered[run[0]] += run[2]
		}
	}
	if covered != sections {
		t.Errorf("the parts cover %v of the files' %v", covered, sections)
	}
	if got := code.LayerCut([3]int64{10, 5, 0}, 0, 15); !slices.Equal(got, [][3]int64{{0, 0, 10}, {1, 0, 5}}) {
		t.Errorf("a layer of no commit-graph layer is cut %v", got)
	}
	if got := code.LayerCut(sections, total, p); got != nil {
		t.Errorf("past the layer's end the cut is %v, want nothing", got)
	}
}

// atOnce is a Putter that holds every Put until all of a layer's parts have
// begun, so parts uploaded one after another never get past the first and
// the case fails by the putter's own deadline rather than hanging; it reads
// one byte of each part — the case marks every part's first byte — to learn
// which part it was handed, and answers it stored whole, its bytes unread
// past that one. With fail set, the part whose first byte it is answers err
// once all have begun, and the others wait to be cancelled.
type atOnce struct {
	want int
	fail byte
	err  error

	mu    sync.Mutex
	began []begun
	all   chan struct{}
}

// begun is one Put as atOnce saw it.
type begun struct {
	first    byte
	key      objstore.Key
	limit    int64
	meta     objstore.PutMeta
	deadline time.Time
	canceled bool
	answered time.Time
}

// partsBegin bounds how long atOnce waits for every part to begin, and for a
// part to be cancelled: a HANG DETECTOR, since three goroutines' first byte
// each is microseconds of work.
const partsBegin = 30 * time.Second

func (p *atOnce) Put(ctx context.Context, r io.Reader, limit int64, m objstore.PutMeta) (objstore.Object, error) {
	var first [1]byte
	if _, err := io.ReadFull(r, first[:]); err != nil {
		return objstore.Object{}, err
	}
	deadline, _ := ctx.Deadline()
	p.mu.Lock()
	i := len(p.began)
	p.began = append(p.began, begun{first: first[0], key: objstore.KeyAt(time.Now()), limit: limit, meta: m,
		deadline: deadline})
	if len(p.began) == p.want {
		close(p.all)
	}
	p.mu.Unlock()
	done := func(canceled bool) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.began[i].canceled, p.began[i].answered = canceled, time.Now()
	}
	select {
	case <-p.all:
	case <-ctx.Done():
		done(true)
		return objstore.Object{}, ctx.Err()
	case <-time.After(partsBegin):
		done(false)
		return objstore.Object{}, fmt.Errorf("only %d of %d parts began within %v: the parts are not uploaded "+
			"at once", len(p.began), p.want, partsBegin)
	}
	switch {
	case first[0] == p.fail:
		done(false)
		return objstore.Object{}, p.err
	case p.fail != 0:
		select {
		case <-ctx.Done():
			done(true)
			return objstore.Object{}, ctx.Err()
		case <-time.After(partsBegin):
			done(false)
			return objstore.Object{}, fmt.Errorf("a failed part did not cancel this one within %v", partsBegin)
		}
	}
	done(false)
	p.mu.Lock()
	defer p.mu.Unlock()
	return objstore.Object{Key: p.began[i].key, Hash: objstore.HashOf(first[:]), Size: limit}, nil
}

// EVERY PART OF A LAYER IS UPLOADED AT ONCE, each key minted as its upload
// begins, so the last of a layer of LayerPartBytes parts at the store's
// pacing floor lands while every key is still within RecordWithin: a layer of
// three parts at the shipped LayerPartBytes — sparse files, whose bytes the
// putter never reads past each part's first — has all three Puts begun before
// any returns, each limited at its part, typed as a layer's part and bounded
// by UploadDeadline of its size, the hit asked at each part's own start in
// the layer, and the parts answered in byte order whatever order they
// finished. One failing part cancels the others, and its error is the
// answer, with no part named.
//
// Mutation: upload the parts one after another — red, by the putter's own
// deadline.
func TestStoreLayerUploadsEveryPartAtOnce(t *testing.T) {
	t.Parallel()
	const p = code.LayerPartBytes
	sections := [3]int64{p + 100, p - 50, 1000}
	dir := t.TempDir()
	var paths [3]string
	for i, n := range sections {
		paths[i] = filepath.Join(dir, fmt.Sprintf("section-%d", i))
		f, err := os.Create(paths[i])
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(n); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	// Each part's first byte, where the cut puts it: the layer's first byte,
	// the multi-pack-index's byte at p, and the bitmap's at p - 100 (the
	// layer's byte 2p).
	marks := []struct {
		section int
		at      int64
		mark    byte
	}{{0, 0, 'A'}, {0, p, 'B'}, {1, p - 100, 'C'}}
	for _, m := range marks {
		f, err := os.OpenFile(paths[m.section], os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt([]byte{m.mark}, m.at); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	var mu sync.Mutex
	var hits []int64
	hit := func(_ context.Context, at int64) error {
		mu.Lock()
		defer mu.Unlock()
		hits = append(hits, at)
		return nil
	}

	put := &atOnce{want: 3, all: make(chan struct{})}
	began := time.Now()
	parts, err := code.StoreLayer(t.Context(), put, paths, sections, hit)
	ended := time.Now()
	if err != nil {
		t.Fatalf("store a layer of three parts: %v", err)
	}
	slices.Sort(hits)
	if !slices.Equal(hits, []int64{0, p, 2 * p}) {
		t.Errorf("the hit was asked at %v, want each part's start in the layer, %v", hits, []int64{0, p, 2 * p})
	}
	sizes := []int64{p, p, 1050}
	if len(parts) != 3 {
		t.Fatalf("StoreLayer answered %d parts, want 3", len(parts))
	}
	for i, m := range marks {
		j := slices.IndexFunc(put.began, func(b begun) bool { return b.first == m.mark })
		if j < 0 {
			t.Fatalf("no part began with the byte %q", m.mark)
		}
		b := put.began[j]
		bound := code.UploadDeadline(sizes[i])
		switch {
		case parts[i] != (code.LayerPart{Object: b.key, Hash: objstore.HashOf([]byte{m.mark}), Size: sizes[i]}):
			t.Errorf("part %d is %+v, want the object its upload stored, %d bytes", i, parts[i], sizes[i])
		case b.limit != sizes[i] || b.meta.ContentType != code.LayerContentType:
			t.Errorf("part %d was put limited at %d as %q, want %d as %q", i, b.limit, b.meta.ContentType, sizes[i],
				code.LayerContentType)
		case b.deadline.Before(began.Add(bound)) || b.deadline.After(ended.Add(bound)):
			t.Errorf("part %d was put under a deadline of %v, want %v from its start", i, b.deadline, bound)
		}
	}

	injected := errors.New("the bucket refused part 1")
	put = &atOnce{want: 3, all: make(chan struct{}), fail: 'B', err: injected}
	parts, err = code.StoreLayer(t.Context(), put, paths, sections, nil)
	if !errors.Is(err, injected) || parts != nil {
		t.Fatalf("a layer whose second part fails: %v, %v parts; want the part's own error and no part", err,
			len(parts))
	}
	for _, b := range put.began {
		if b.first != 'B' && !b.canceled {
			t.Errorf("the part beginning %q was not cancelled by the failure of another", b.first)
		}
	}
}

// A LAYER IS STORED AS ITS THREE FILES BACK TO BACK AND LAID BACK WHERE IT CAME
// FROM: the tip git wrote, stored in its one part through a store over
// memory — handed to the store exactly the three files' bytes, limited at the
// layer's size, the hit asked before every read the store makes at the bytes
// handed over — and read back through the store's verified read into three
// fresh files, through a buffer smaller than any of them, is the three files
// again, byte for byte, and passes CheckLayer under its row. A part the store
// damaged is objstore.ErrCorrupt, one it lost objstore.ErrNotFound, both
// through errors.Is, and this node's disk failing a write is its own failure,
// "on this node"; a file not the size of its section, or a layer past what
// MaxLayerParts parts hold, is refused before any byte is stored.
//
// Mutation: lay a run back at its offset in the layer rather than in its file
// — red.
func TestALayerIsStoredInPartsAndLaidBackWhereItCameFrom(t *testing.T) {
	t.Parallel()
	g := writeLayers(t)
	paths := [3]string{g.tip.midx, g.tip.bitmap, g.tip.graph}
	files := filesOf(t, g.tip)
	whole := slices.Concat(files[0], files[1], files[2])
	ref, err := code.LayLayer(paths[0], paths[1], paths[2])
	if err != nil {
		t.Fatal(err)
	}

	rec := &recording{chunk: 1000}
	var hits []int64
	if _, err := code.StoreLayer(t.Context(), rec, paths, ref.Sections(), func(_ context.Context, at int64) error {
		hits = append(hits, at)
		return nil
	}); err != nil {
		t.Fatalf("store the tip through a recording store: %v", err)
	}
	if !bytes.Equal(rec.got, whole) || rec.limit != ref.Size() || rec.meta.ContentType != code.LayerContentType ||
		!slices.Equal(hits, rec.before) {
		t.Errorf("the store was handed %d bytes limited at %d as %q, the hit asked at %v and the reads made at %v; "+
			"want the %d bytes of the three files at the layer's size as %q, a hit before every read", len(rec.got),
			rec.limit, rec.meta.ContentType, hits, rec.before, len(whole), code.LayerContentType)
	}

	store, backend := stored(t)
	parts, err := code.StoreLayer(t.Context(), store, paths, ref.Sections(), nil)
	if err != nil || len(parts) != 1 || parts[0].Size != ref.Size() || parts[0].Hash != objstore.HashOf(whole) {
		t.Fatalf("store the tip: %+v, %v; want one part of its %d bytes", parts, err, ref.Size())
	}
	ref.Parts, ref.BaseID, ref.Git, ref.TookMs = parts, g.base.id, "2.56.0", 3
	if err := ref.Check(); err != nil {
		t.Fatalf("the row the stored layer names: %v", err)
	}
	dir := t.TempDir()
	lay := func(name string, w [3]io.WriterAt) ([3]string, error) {
		t.Helper()
		var out [3]string
		var into [3]io.WriterAt
		for i := range out {
			out[i] = filepath.Join(dir, fmt.Sprintf("%s-%d", name, i))
			f, err := os.Create(out[i])
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = f.Close() })
			into[i] = f
			if w[i] != nil {
				into[i] = w[i]
			}
		}
		return out, code.FetchLayerPart(t.Context(), store, ref, 0, into, make([]byte, 7))
	}
	back, err := lay("back", [3]io.WriterAt{})
	if err != nil {
		t.Fatalf("lay the stored part back: %v", err)
	}
	for i := range back {
		if got := mustRead(t, back[i]); !bytes.Equal(got, files[i]) {
			t.Errorf("file %d laid back is %d bytes that are not its %d", i, len(got), len(files[i]))
		}
	}
	if err := (layerBytes{mustRead(t, back[0]), mustRead(t, back[1]), mustRead(t, back[2])}).check(ref,
		[]string{g.base.graphID}); err != nil {
		t.Errorf("the layer laid back fails its check: %v", err)
	}

	failed := errors.New("the disk is full")
	if _, err := lay("disk", [3]io.WriterAt{nil, failingWriterAt{failed}, nil}); !errors.Is(err, failed) ||
		!strings.Contains(err.Error(), "on this node") || errors.Is(err, objstore.ErrCorrupt) {
		t.Errorf("a write this node's disk fails: %v, want the disk's failure, on this node", err)
	}
	damaged := slices.Clone(whole)
	damaged[len(damaged)/2] ^= 0xff
	backend.Corrupt(parts[0].Object.Name(), damaged)
	if _, err := lay("damaged", [3]io.WriterAt{}); !errors.Is(err, objstore.ErrCorrupt) {
		t.Errorf("a part the store damaged: %v, want objstore.ErrCorrupt", err)
	}
	if err := backend.Delete(t.Context(), parts[0].Object.Name()); err != nil {
		t.Fatal(err)
	}
	if _, err := lay("lost", [3]io.WriterAt{}); !errors.Is(err, objstore.ErrNotFound) {
		t.Errorf("a part the store lost: %v, want objstore.ErrNotFound", err)
	}

	refuse := &recording{chunk: 1000}
	for _, c := range []struct {
		name     string
		paths    [3]string
		sections [3]int64
	}{
		{"a file longer than its section", paths, [3]int64{ref.MidxBytes - 1, ref.BitmapBytes, ref.GraphBytes}},
		{"a section named by no file", [3]string{paths[0], paths[1], ""}, ref.Sections()},
		{"a layer past its parts", [3]string{"m", "b", "g"},
			[3]int64{code.MaxLayerParts * code.LayerPartBytes, 1, 1}},
	} {
		if _, err := code.StoreLayer(t.Context(), refuse, c.paths, c.sections, nil); err == nil {
			t.Errorf("%s was stored", c.name)
		}
	}
	if len(refuse.got) != 0 || refuse.limit != 0 {
		t.Errorf("a refused layer handed the store %d bytes", len(refuse.got))
	}
}

// failingWriterAt is a file whose every write fails.
type failingWriterAt struct{ err error }

func (f failingWriterAt) WriteAt([]byte, int64) (int, error) { return 0, f.err }

// allocated is how many bytes f allocated on the heap: runtime.ReadMemStats's
// TotalAlloc either side of it, which stops the world, so it counts every
// allocation made meanwhile — f's alone while nothing else runs, as when a
// fuzz target does: go test runs every seed corpus after the package's tests
// have ended, and -fuzz runs nothing else in its workers. A stray allocation
// of another goroutine's is not f's, so a bound is judged over three
// measurements and only one past it every time is f's.
func allocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// allocBound is the most a reader may allocate reading an n-byte file: LINEAR
// IN n — each byte it reads is held a few times at most (the table of
// contents, the pack names and their strings, the chunk list, a hash buffer
// never larger than the file) — plus a slack for the reader's own state and
// its refusal's sentence. A count read from a file that sized an allocation
// before it was held to the bytes would be the count's, not the file's: a
// million packs claimed by a file that names one is sixteen megabytes of
// string headers alone.
func allocBound(n int) uint64 { return 16*uint64(n) + 64<<10 }

// boundedAlloc fails t unless parse allocates within allocBound of the n bytes
// it reads, judged over three measurements.
func boundedAlloc(t *testing.T, n int, parse func()) {
	t.Helper()
	least := uint64(1<<64 - 1)
	for range 3 {
		got := allocated(parse)
		if got <= allocBound(n) {
			return
		}
		least = min(least, got)
	}
	t.Fatalf("reading a %d-byte file allocated %d bytes every time, past the %d its size bounds", n, least,
		allocBound(n))
}

// FuzzParseMidx: whatever a multi-pack-index file holds, ParseMidx never
// panics and allocates no more than the file's own size bounds (allocBound),
// and what it accepts is what its bytes say — its version, a name for every
// pack its header counts, each a cache's name for a pack, in ascending order
// in a version 1 file, its objects inside the file, its trailer. Seeded with
// git's own files (writeLayers), each also resealed after the fuzzer's damage
// so a damaged body is read past the trailer it would fail first, and with a
// file of git's whose header counts a million packs.
//
// Mutation: size the pack names' slice by the header's count before the PNAM
// chunk is held to it — red, by the allocation bound.
func FuzzParseMidx(f *testing.F) {
	g := writeLayers(f)
	for _, path := range []string{g.base.midx, g.tip.midx, g.whole.midx} {
		body := mustRead(f, path)
		f.Add(body)
		huge := slices.Clone(body)
		binary.BigEndian.PutUint32(huge[8:12], 1<<20)
		f.Add(reseal(huge))
	}
	f.Add([]byte("MIDX\x01\x01\x00\x00\x00\x00\x00\x01"))
	f.Fuzz(func(t *testing.T, body []byte) {
		for _, b := range [][]byte{body, reseal(body)} {
			var m code.MidxInfo
			var err error
			boundedAlloc(t, len(b), func() { m, err = code.ParseMidx(bytes.NewReader(b), int64(len(b))) })
			if err != nil {
				continue
			}
			packs := int(binary.BigEndian.Uint32(b[8:12]))
			switch {
			case m.Version != int(b[4]) || (m.Version != 1 && m.Version != 2):
				t.Fatalf("accepted version %d of a file whose header says %d", m.Version, b[4])
			case len(m.Packs) != packs:
				t.Fatalf("accepted %d pack names of a file whose header counts %d", len(m.Packs), packs)
			case m.Objects < 1 || m.Objects*20 > int64(len(b)):
				t.Fatalf("accepted %d objects in a %d-byte file", m.Objects, len(b))
			case m.Checksum != trailerHex(b):
				t.Fatalf("accepted the checksum %s of a file whose trailer is %s", m.Checksum, trailerHex(b))
			}
			for i, name := range m.Packs {
				if len(name) != 49 || !strings.HasPrefix(name, "pack-") || !strings.HasSuffix(name, ".idx") ||
					(m.Version == 1 && i > 0 && m.Packs[i-1] >= name) {
					t.Fatalf("accepted the pack names %q", m.Packs)
				}
			}
		}
	})
}

// FuzzParseBitmap: whatever a bitmap file holds, ParseBitmap never panics and
// allocates no more than its size bounds, and what it accepts is what its
// header and trailer say. Seeded with git's own bitmaps, resealed after a
// damage as FuzzParseMidx's are, and one whose header counts a million
// selected commits.
//
// Mutation: read the multi-pack-index's checksum from the wrong bytes of the
// header — red.
func FuzzParseBitmap(f *testing.F) {
	g := writeLayers(f)
	for _, path := range []string{g.base.bitmap, g.tip.bitmap, g.whole.bitmap} {
		body := mustRead(f, path)
		f.Add(body)
		huge := slices.Clone(body)
		binary.BigEndian.PutUint32(huge[8:12], 1<<20)
		f.Add(reseal(huge))
	}
	f.Add([]byte("BITM\x00\x01\x00\x01"))
	f.Fuzz(func(t *testing.T, body []byte) {
		for _, b := range [][]byte{body, reseal(body)} {
			var info code.BitmapInfo
			var err error
			boundedAlloc(t, len(b), func() { info, err = code.ParseBitmap(bytes.NewReader(b), int64(len(b))) })
			if err != nil {
				continue
			}
			switch {
			case info.Version != 1 || info.Options&0x1 == 0 || info.Options != binary.BigEndian.Uint16(b[6:8]):
				t.Fatalf("accepted version %d with the options %#x", info.Version, info.Options)
			case info.Midx != hex.EncodeToString(b[12:32]):
				t.Fatalf("accepted the multi-pack-index %s of a header naming %x", info.Midx, b[12:32])
			case info.Commits != int(binary.BigEndian.Uint32(b[8:12])):
				t.Fatalf("accepted %d commits of a header counting %d", info.Commits, binary.BigEndian.Uint32(b[8:12]))
			case info.Checksum != trailerHex(b):
				t.Fatalf("accepted the checksum %s of a file whose trailer is %s", info.Checksum, trailerHex(b))
			}
		}
	})
}

// FuzzParseGraph: whatever a commit-graph file holds, ParseGraph never panics
// and allocates no more than its size bounds, and what it accepts is what its
// bytes say — a base id for every base its header counts, its commits inside
// the file, its trailer. Seeded with git's own layers, resealed after a damage
// as FuzzParseMidx's are, and one whose fanout counts a million commits.
//
// Mutation: read the base graphs' ids from the commit data chunk — red.
func FuzzParseGraph(f *testing.F) {
	g := writeLayers(f)
	for _, path := range []string{g.base.graph, g.tip.graph} {
		body := mustRead(f, path)
		f.Add(body)
		start, _ := gittest.GraphChunk(f, path, "OIDF")
		huge := slices.Clone(body)
		binary.BigEndian.PutUint32(huge[start+255*4:], 1<<20)
		f.Add(reseal(huge))
	}
	f.Add([]byte("CGPH\x01\x01\x00\x00"))
	f.Fuzz(func(t *testing.T, body []byte) {
		for _, b := range [][]byte{body, reseal(body)} {
			var info code.GraphInfo
			var err error
			boundedAlloc(t, len(b), func() { info, err = code.ParseGraph(bytes.NewReader(b), int64(len(b))) })
			if err != nil {
				continue
			}
			switch {
			case len(info.Bases) != int(b[7]):
				t.Fatalf("accepted %d bases of a header counting %d", len(info.Bases), b[7])
			case info.Commits*20 > int64(len(b)):
				t.Fatalf("accepted %d commits in a %d-byte file", info.Commits, len(b))
			case info.Checksum != trailerHex(b):
				t.Fatalf("accepted the checksum %s of a file whose trailer is %s", info.Checksum, trailerHex(b))
			}
			for _, base := range info.Bases {
				if _, err := hex.DecodeString(base); err != nil || len(base) != 40 {
					t.Fatalf("accepted the base %q", base)
				}
			}
		}
	})
}
