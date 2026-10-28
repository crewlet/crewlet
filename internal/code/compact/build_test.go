package compact_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // git's own object ids, which a case computes for blobs it writes.
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/compact"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// The bounds the build cases scale a compaction to: a cut of 4 MiB — what a
// case's pushes fill in a few seconds, where production's gibibyte would be
// minutes — the bundle bound five quarters of it, as code.MaxPackBytes is of
// code.CompactPackBytes, and a blob of max_blob_bytes' default scaled the
// same: the largest object a cut of production's meets, at the cut's scale.
const (
	scaledCut    = 4 << 20
	scaledBundle = scaledCut / 4 * 5
	scaledBlob   = int(scaledCut * config.DefaultCodeMaxBlobBytes / code.CompactPackBytes)
)

// scaled is a compactor over x held to the scaled bounds.
func (x *fixture) scaled(edit func(*compact.Options)) (*compact.Compactor, *metrics.Recorder, *logs) {
	x.t.Helper()
	c, rec, l := x.compactorOf(edit)
	compact.ScaleBounds(c, scaledCut, scaledBundle)
	return c, rec, l
}

// outputs are the live rows a compaction applied: every live pack the rows
// held before it is not among them.
func outputs(before, after codetest.Rows) []code.PackRow {
	was := map[string]bool{}
	for _, r := range before.Packs {
		was[r.PackID] = true
	}
	var out []code.PackRow
	for _, r := range after.Packs {
		if !was[r.PackID] {
			out = append(out, r)
		}
	}
	return out
}

// cutFixture is a repository of code.CompactMinMerge pushes — the fewest one
// roll merges — of two and a half scaled cuts in all, some 10 MiB, each one
// commit adding files of random bytes no larger than scaledBlob, whose packs
// weigh alike, so one roll merges them all whatever the floor.
func cutFixture(t *testing.T) *fixture {
	t.Helper()
	x := newFixture(t)
	perPush := 5 * scaledCut / 2 / code.CompactMinMerge
	files := make([]int, (perPush+scaledBlob-1)/scaledBlob)
	for i := range files {
		files[i] = perPush / len(files)
	}
	x.pushN(code.CompactMinMerge, files...)
	return x
}

// EVERY CUT OUTPUT BUT THE LAST IS FULL: pack-objects closes an output before
// the object that would take it past the cut, so with objects no larger than
// max_blob_bytes — an eighth of the cut and less — every output but the last
// is at seven eighths of the cut or past it: full (code.PackRow.Full), merged
// by no later roll. Pushes of two and a half cuts make three outputs.
//
// Mutation: cut at code.FullPackBytes' share of the cut rather than at the
// cut, and the first outputs close under seven eighths.
func TestEveryCutOutputButTheLastIsFull(t *testing.T) {
	t.Parallel()
	x := cutFixture(t)
	c, _, _ := x.scaled(nil)
	before := x.rows()
	if out, err := compactWhileBusy(t, c, x.candidate()); out != compact.OutcomeApplied {
		t.Fatalf("the compaction of %d pushes: %s, %v; want applied", code.CompactMinMerge, out, err)
	}
	outs := outputs(before, x.rows())
	if len(outs) != 3 {
		t.Fatalf("pushes of two and a half cuts compacted into %d outputs, want 3: %+v", len(outs), outs)
	}
	short := 0
	for _, o := range outs {
		if o.Ref().PackBytes() > scaledCut {
			t.Errorf("output %s's pack section is %d bytes, past the %d cut", o.PackID, o.Ref().PackBytes(),
				scaledCut)
		}
		if !o.FullAt(scaledCut) {
			short++
		}
	}
	if short > 1 {
		t.Errorf("%d of the %d outputs are not full; only the last a cut writes may be: %+v", short, len(outs),
			outs)
	}
}

// EVERY OUTPUT IS UPLOADED AT ONCE: a key is minted as its upload begins, and a
// record naming it is refused once it is older than objstore.RecordWithin, so
// outputs uploaded one after another would age the first by every later one —
// two gibibyte outputs at the store's pacing floor past it, for ever. A store
// that holds every put until all three of a build's outputs have begun is
// answered: the three keys are minted within a second of one another.
//
// Mutation: upload the outputs in a loop, one put at a time, and the first put
// waits for the others until its hold gives up, failing the compaction.
func TestEveryOutputIsUploadedAtOnce(t *testing.T) {
	t.Parallel()
	x := cutFixture(t)
	const want = 3
	var (
		mu    sync.Mutex
		begun int
		all   = make(chan struct{})
	)
	rs := &recordingStore{Store: x.store}
	rs.hold = func(ctx context.Context) error {
		mu.Lock()
		begun++
		if begun == want {
			close(all)
		}
		mu.Unlock()
		select {
		case <-all:
			return nil
		case <-time.After(30 * time.Second):
			return fmt.Errorf("only %d of the %d outputs' puts began in 30 s: the uploads are not made at once",
				begun, want)
		case <-ctx.Done():
			return errHeld
		}
	}
	c, _, _ := x.scaled(func(o *compact.Options) { o.Objects = rs })
	if out, err := compactWhileBusy(t, c, x.candidate()); out != compact.OutcomeApplied {
		t.Fatalf("the compaction of three outputs: %s, %v; want applied", out, err)
	}
	stored, _ := rs.stored()
	if len(stored) != want {
		t.Fatalf("the compaction stored %d objects, want its %d outputs", len(stored), want)
	}
	first, last := stored[0].Key.Minted(), stored[0].Key.Minted()
	for _, o := range stored[1:] {
		if at := o.Key.Minted(); at.Before(first) {
			first = at
		} else if at.After(last) {
			last = at
		}
	}
	if spread := last.Sub(first); spread > time.Second {
		t.Errorf("the outputs' keys were minted %s apart, want within a second", spread)
	}
}

// A COMPACTION'S OUTPUT FITS THE BUNDLE RULE: every output is a bundle every
// git node installs — its index version 2 and its reverse index written beside
// it, held to its seal (code.CheckBundle) as every stored pack is — with its
// pack section under code.MaxPackBytes.
//
// Mutation: drop pack.writeReverseIndex from CompactConfig, and the build
// writes no reverse index, so no output can be laid out.
func TestACompactionOutputFitsTheBundleRule(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	x.pushN(code.CompactMinMerge, 32<<10)
	c, _, _ := x.compactorOf(nil)
	before := x.rows()
	if out, err := compactWhileBusy(t, c, x.candidate()); out != compact.OutcomeApplied {
		t.Fatalf("the compaction: %s, %v; want applied", out, err)
	}
	outs := outputs(before, x.rows())
	if len(outs) == 0 {
		t.Fatal("the compaction applied no output")
	}
	for _, o := range outs {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		rc, err := x.store.Open(ctx, o.Ref().Content())
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		sw := code.NewSealWriter(o.Ref())
		_, err = io.Copy(sw, rc)
		_ = rc.Close()
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		seal, err := sw.Seal()
		if err == nil {
			err = code.CheckBundle(o.Ref(), seal)
		}
		if err != nil {
			t.Errorf("output %s is no bundle a git node installs: %v", o.PackID, err)
		}
		if o.Ref().PackBytes() >= code.MaxPackBytes {
			t.Errorf("output %s's pack section is %d bytes, past %d", o.PackID, o.Ref().PackBytes(),
				code.MaxPackBytes)
		}
	}
}

// blobID is git's object id of a blob holding body.
func blobID(body string) string {
	h := sha1.New() //nolint:gosec // see the import.
	fmt.Fprintf(h, "blob %d\x00%s", len(body), body)
	return hex.EncodeToString(h.Sum(nil))
}

// handmade is a source repository a case writes objects into with
// fast-import, and packs exactly the objects it names out of — each pack into
// a bare repository of its own, as a push's receive-pack stores one.
type handmade struct {
	x   *fixture
	src string
}

func (x *fixture) handmade() *handmade {
	x.t.Helper()
	return &handmade{x: x, src: gittest.Empty(x.t)}
}

// blobs writes every body as a blob and answers their ids, in order.
func (h *handmade) blobs(bodies []string) []string {
	h.x.t.Helper()
	ids := make([]string, len(bodies))
	gittest.FastImport(h.x.t, h.x.p.Git(), h.src, func(w *bufio.Writer) error {
		for i, b := range bodies {
			ids[i] = blobID(b)
			if _, err := fmt.Fprintf(w, "blob\ndata %d\n%s\n", len(b), b); err != nil {
				return err
			}
		}
		return nil
	})
	return ids
}

// commit writes a root commit on ref whose tree holds files, answering the
// commit's id and every object it is made of: the commit, its tree and its
// blobs.
func (h *handmade) commit(ref string, files map[string]string) (string, []string) {
	h.x.t.Helper()
	names := slices.Sorted(maps.Keys(files))
	gittest.FastImport(h.x.t, h.x.p.Git(), h.src, func(w *bufio.Writer) error {
		msg := "commit " + ref + "\n"
		if _, err := fmt.Fprintf(w, "commit %s\ncommitter Case <case@example.com> 1767225600 +0000\ndata %d\n%s",
			ref, len(msg), msg); err != nil {
			return err
		}
		for _, n := range names {
			if _, err := fmt.Fprintf(w, "M 100644 inline %s\ndata %d\n%s\n", n, len(files[n]), files[n]); err != nil {
				return err
			}
		}
		return nil
	})
	tip := strings.TrimSpace(h.x.p.Git().Run(h.x.t, h.src, "rev-parse", ref))
	// A ROOT COMMIT'S OBJECTS are exactly what rev-list walks from it: the
	// commit, every tree and every blob.
	var objects []string
	for line := range strings.Lines(h.x.p.Git().Run(h.x.t, h.src, "rev-list", "--objects", ref)) {
		if id, _, _ := strings.Cut(strings.TrimSpace(line), " "); id != "" {
			objects = append(objects, id)
		}
	}
	return tip, objects
}

// pack packs exactly ids out of the source repository into a bare repository
// of its own, answering it and the pack's checksum.
func (h *handmade) pack(ids []string) (bare, checksum string) {
	h.x.t.Helper()
	bare = gittest.Empty(h.x.t)
	return bare, gittest.PackObjects(h.x.t, h.x.p.Git(), bare, h.src, ids...)
}

// A COMPACTION KEEPS EVERY OBJECT OF ITS INPUTS: its outputs hold every object
// the packs it merges hold — one that a pack it does not merge holds as well
// among them — because a merge judges no reachability: two compactions decided
// from different snapshots may each retire the pack the other relied on, and
// both land (ADR-0060). A heavy pack and eight light ones share a blob; the
// eight are merged, and the blob is in the outputs.
//
// Mutation: hand the build the packs it does not merge as excluded packs (a
// `^` line each), and the shared blob is left to the heavy pack alone.
func TestACompactionKeepsEveryObjectOfItsInputs(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	x.push(4 << 10)
	h := x.handmade()
	shared := "a blob two packs hold\n"
	heavy := map[string]string{"shared.txt": shared}
	for i := range 200 {
		heavy[fmt.Sprintf("heavy/%03d.txt", i)] = fmt.Sprintf("heavy %d\n", i)
	}
	tip, objects := h.commit("refs/heads/heavy", heavy)
	bare, cs := h.pack(objects)
	keep := x.proposePack(bare, cs, "refs/heads/heavy", tip)

	want := map[string]bool{}
	for i := range code.CompactMinMerge {
		files := map[string]string{fmt.Sprintf("light/%d.txt", i): fmt.Sprintf("light %d\n", i)}
		if i == 0 {
			files["shared.txt"] = shared
		}
		ref := fmt.Sprintf("refs/heads/light-%d", i)
		tip, objects := h.commit(ref, files)
		bare, cs := h.pack(objects)
		x.proposePack(bare, cs, ref, tip)
		for _, id := range objects {
			want[id] = true
		}
	}
	if !want[blobID(shared)] {
		t.Fatal("the fixture's light packs do not hold the shared blob")
	}

	c, _, _ := x.compactorOf(nil)
	before := x.rows()
	if out, err := compactWhileBusy(t, c, x.candidate()); out != compact.OutcomeApplied {
		t.Fatalf("the compaction: %s, %v; want applied", out, err)
	}
	after := x.rows()
	if !slices.ContainsFunc(after.Packs, func(r code.PackRow) bool { return r.PackID == keep.PackID }) {
		t.Fatalf("the heavy pack was merged; the case needs it outside the roll: %+v", after.Packs)
	}
	got := map[string]bool{}
	for _, o := range outputs(before, after) {
		for _, id := range x.objectIDs(o) {
			got[id] = true
		}
	}
	for id := range want {
		if !got[id] {
			t.Errorf("the outputs do not hold %s, which a pack they replaced held", id)
		}
	}
}

// THE BUILD READS EXACTLY ITS INPUTS, THROUGH ITS ALTERNATE: the scratch a
// compaction builds in holds no pack before its pack-objects runs, and its one
// alternate is a store holding exactly the packs the build is handed — each
// one's .pack, .idx and .rev the incarnation's own files, linked — and nothing
// else: no pack the roll leaves out, no multi-pack-index, no commit-graph. So
// `--stdin-packs`, which walks every commit, tree and blob it can reach to fill
// in its name hashes, walks the roll and never the history beside it — here a
// heavy pack live beside the eight light ones merged, which the store does not
// hold. And the build succeeds on the node pin's git: a platform assertion, as
// a git that did not read an alternate's packs for `--stdin-packs` would fail
// every compaction.
//
// Mutation: lean the build's scratch on the incarnation (NewMergeScratch's
// alternate the copy's own directory, as the build's was), and its alternate
// holds the heavy pack.
func TestTheBuildReadsItsInputsThroughTheAlternate(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	h := x.handmade()
	heavy := map[string]string{}
	for i := range 200 {
		heavy[fmt.Sprintf("heavy/%03d.txt", i)] = fmt.Sprintf("heavy %d\n", i)
	}
	tip, objects := h.commit("refs/heads/heavy", heavy)
	bare, heavyPack := h.pack(objects)
	keep := x.proposePack(bare, heavyPack, "refs/heads/heavy", tip)
	x.pushN(code.CompactMinMerge, 4<<10)
	cand := x.candidate()
	s, err := x.node().Cache().Sync(x.c.Ctx, cand.ID, statelog.FloorAt(statelog.ReadSession, cand.Version))
	if err != nil {
		t.Fatalf("sync the home's copy: %v", err)
	}
	copyDir := s.Dir
	s.Release()

	c, _, _ := x.compactorOf(nil)
	var (
		mu     sync.Mutex
		builds int
	)
	compact.BuildWith(c, func(ctx context.Context, adm *cache.Compacting, cmd gitproc.Cmd) (gitproc.Result, error) {
		mu.Lock()
		builds++
		mu.Unlock()
		listed, err := io.ReadAll(cmd.Stdin)
		if err != nil {
			return gitproc.Result{}, err
		}
		cmd.Stdin = bytes.NewReader(listed)
		var want []string
		for line := range strings.Lines(string(listed)) {
			cs := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(line), "pack-"), ".pack")
			for _, ext := range []string{".idx", ".pack", ".rev"} {
				want = append(want, "pack-"+cs+ext)
			}
		}
		slices.Sort(want)
		if held := filesIn(t, filepath.Join(cmd.Dir, "objects", "pack")); len(held) > 0 {
			t.Errorf("the scratch holds %v before the build, want nothing: its inputs are read through its "+
				"alternate", held)
		}
		if strings.HasPrefix(cmd.Alternate, copyDir) || strings.HasPrefix(copyDir, cmd.Alternate) {
			t.Errorf("the build's alternate is %s, the copy it planned over: its walk for name hashes would run "+
				"the repository's history", cmd.Alternate)
		}
		if got := filesIn(t, filepath.Join(cmd.Alternate, "objects", "pack")); !slices.Equal(got, want) {
			t.Errorf("the build's alternate holds %v, want exactly the files of the packs it is handed %v", got,
				want)
		}
		if slices.Contains(want, "pack-"+heavyPack+".pack") {
			t.Errorf("the build was handed the heavy pack the roll leaves out: %s", listed)
		}
		for _, name := range want {
			linked, err := os.Stat(filepath.Join(cmd.Alternate, "objects", "pack", name))
			if err != nil {
				continue
			}
			if held, err := os.Stat(filepath.Join(copyDir, "objects", "pack", name)); err != nil ||
				!os.SameFile(linked, held) {
				t.Errorf("the alternate's %s is not the copy's own file (%v)", name, err)
			}
		}
		for _, rel := range []string{"objects/info/commit-graph", "objects/info/commit-graphs",
			"objects/info/alternates"} {
			if _, err := os.Lstat(filepath.Join(cmd.Alternate, filepath.FromSlash(rel))); err == nil {
				t.Errorf("the build's alternate holds %s: it holds the packs merged and nothing else", rel)
			}
		}
		return compact.RunInSlot(ctx, adm, cmd)
	})
	if out, err := compactWhileBusy(t, c, cand); out != compact.OutcomeApplied {
		t.Fatalf("the compaction: %s, %v; want applied", out, err)
	}
	if builds != 1 {
		t.Errorf("the compaction ran %d builds, want one", builds)
	}
	if !slices.ContainsFunc(x.rows().Packs, func(r code.PackRow) bool { return r.PackID == keep.PackID }) {
		t.Error("the heavy pack was merged: the case needs it beside the roll")
	}
}

// filesIn is the sorted names of the files in dir; a directory absent holds
// none.
func filesIn(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("listing %s: %v", dir, err)
	}
	var out []string
	for _, d := range list {
		out = append(out, d.Name())
	}
	slices.Sort(out)
	return out
}

// builder is a case's stand-in for a build's git: every pack-objects asked of
// it recorded — the cut it was asked for and the packs it was handed — and
// then run, by rewrite where that is set, in the admission's slot.
type builder struct {
	mu      sync.Mutex
	cuts    []int64
	inputs  [][]string
	rewrite func(call int, args []string) []string
}

func (b *builder) run(ctx context.Context, adm *cache.Compacting, cmd gitproc.Cmd) (gitproc.Result, error) {
	in, err := io.ReadAll(cmd.Stdin)
	if err != nil {
		return gitproc.Result{}, err
	}
	var packs []string
	for line := range strings.Lines(string(in)) {
		packs = append(packs, strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(line), "pack-"), ".pack"))
	}
	cut := int64(0)
	for _, a := range cmd.Args {
		if v, ok := strings.CutPrefix(a, "--max-pack-size="); ok {
			cut, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	b.mu.Lock()
	call := len(b.cuts)
	b.cuts = append(b.cuts, cut)
	b.inputs = append(b.inputs, packs)
	rewrite := b.rewrite
	b.mu.Unlock()
	if rewrite != nil {
		cmd.Args = rewrite(call, slices.Clone(cmd.Args))
	}
	cmd.Stdin = bytes.NewReader(in)
	return compact.RunInSlot(ctx, adm, cmd)
}

// calls is every cut asked and every input list handed, in order.
func (b *builder) calls() ([]int64, [][]string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.cuts), slices.Clone(b.inputs)
}

// withoutCut is args with no --max-pack-size: one output of everything.
func withoutCut(args []string) []string {
	return slices.DeleteFunc(args, func(a string) bool { return strings.HasPrefix(a, "--max-pack-size=") })
}

// cutAt is args with the cut at n.
func cutAt(args []string, n int64) []string {
	for i, a := range args {
		if strings.HasPrefix(a, "--max-pack-size=") {
			args[i] = "--max-pack-size=" + strconv.FormatInt(n, 10)
		}
	}
	return args
}

// A DENSE ROLL IS CUT TO FIT THE BUNDLE: an index costs 32 bytes an object, so
// an output of many small objects cut at the nominal cut is a bundle past the
// bound every stored pack is held to. The build is asked for a cut scaled down
// by its inputs' aggregate bundle-to-pack ratio, and an output past the bound
// anyway — here the first build's, made with no cut at all — is built once
// more at its own ratio BEFORE ANY BYTE IS STORED: nothing past the bound
// reaches the store, every output stored passes the bundle rule, and every
// output but the last is full.
//
// Mutation: drop the check of the outputs before their upload, and the first
// build's one output past the bound is stored; cut at the nominal cut, and the
// first build is asked for it.
func TestADenseRollIsCutToFitTheBundle(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	h := x.handmade()
	const perPack = 20_000
	tip, objects := h.commit("refs/heads/dense", map[string]string{"README.md": "dense\n"})
	for p := range code.CompactMinMerge {
		bodies := make([]string, perPack)
		for i := range bodies {
			bodies[i] = fmt.Sprintf("dense %d %d\n", p, i)
		}
		ids := h.blobs(bodies)
		if p == 0 {
			ids = append(ids, objects...)
		}
		bare, cs := h.pack(ids)
		x.proposePack(bare, cs, fmt.Sprintf("refs/heads/dense-%d", p), tip)
	}
	b := &builder{rewrite: func(call int, args []string) []string {
		if call == 0 {
			return withoutCut(args)
		}
		return args
	}}
	rs := &recordingStore{Store: x.store}
	c, _, _ := x.scaled(func(o *compact.Options) { o.Objects = rs })
	compact.BuildWith(c, b.run)
	before := x.rows()
	if out, err := compactWhileBusy(t, c, x.candidate()); out != compact.OutcomeApplied {
		t.Fatalf("the compaction of a dense roll: %s, %v; want applied", out, err)
	}
	cuts, _ := b.calls()
	if len(cuts) != 2 {
		t.Fatalf("the dense roll was built %d times, want twice: its first output past the bound rebuilt once",
			len(cuts))
	}
	if cuts[0] <= 0 || cuts[0] >= scaledCut {
		t.Errorf("the dense roll's first build was asked for a cut of %d, want one scaled below the nominal %d",
			cuts[0], scaledCut)
	}
	_, sizes := rs.stored()
	for _, n := range sizes {
		if n > scaledBundle {
			t.Errorf("a put of %d bytes reached the store, past the %d bound", n, scaledBundle)
		}
	}
	outs := outputs(before, x.rows())
	short := 0
	for _, o := range outs {
		if err := o.Ref().Check(); err != nil || o.Size > scaledBundle {
			t.Errorf("output %s (%d bytes) breaks the bundle rule: %v", o.PackID, o.Size, err)
		}
		if !o.FullAt(scaledCut) {
			short++
		}
	}
	if short > 1 {
		t.Errorf("%d of the %d outputs of a dense roll are not full, want the last at most: %+v", short,
			len(outs), outs)
	}
}

// bulkFixture is a repository of 28 pushes, each one commit adding three
// scaledBlob files of random bytes and rewriting one more file a little — a
// delta family whose bases a cut may send to an earlier output, which
// pack-objects then writes whole — some 19 MiB of packs in all, each under
// seven eighths of a 1 MiB cut: sixteen cuts of pack-objects' own floor and
// more, none of them full. Each STORED pack carries the family's base whole
// too, which the receiver's index-pack --fix-thin appends to the thin pack the
// client sent, so the family is kept to 100 KiB.
func bulkFixture(t *testing.T) *fixture {
	t.Helper()
	x := newFixture(t)
	family := make([]byte, 100<<10)
	for i := range family {
		family[i] = byte(x.rng.Uint32())
	}
	for push := range 28 {
		for i := range 1 << 10 {
			family[x.rng.IntN(len(family))] = byte(i)
		}
		files := map[string]string{"family.bin": string(family)}
		for i := range 3 {
			b := make([]byte, scaledBlob)
			for j := range b {
				b[j] = byte(x.rng.Uint32())
			}
			files[fmt.Sprintf("bulk%02d/f%d.bin", push, i)] = string(b)
		}
		x.p.Commit(x.work, files, fmt.Sprintf("bulk %d", push))
		x.c.MustPush(x.p, x.work, "main")
	}
	return x
}

// packBytesOf is the pack bytes of the files named by checksum, as the rows
// before a compaction weigh them.
func packBytesOf(rows codetest.Rows, checksums []string) int64 {
	var n int64
	for _, cs := range checksums {
		for _, r := range rows.Packs {
			if r.GitChecksum == cs {
				n += r.Ref().PackBytes()
				break
			}
		}
	}
	return n
}

// A ROLL AT ITS BYTE CAP WRITES AT MOST ITS OUTPUTS: a roll merges at most half
// code.MaxCompactOutputs cuts of pack bytes (compact.InputByteCap), so with
// the outputs a cut writes a little under it, and a delta whose base went to
// an earlier output written whole, a roll at its cap still writes no more
// outputs than one compaction records — and applies.
//
// Mutation: cap a roll at code.MaxCompactOutputs whole cuts, and the build
// writes more outputs than one compaction records, applying nothing.
func TestARollAtItsByteCapWritesAtMostItsOutputs(t *testing.T) {
	t.Parallel()
	x := bulkFixture(t)
	const cut = 1 << 20 // pack-objects' own floor, so sixteen cuts are a fixture of seconds
	c, _, _ := x.compactorOf(nil)
	compact.ScaleBounds(c, cut, cut/4*5)
	b := &builder{}
	compact.BuildWith(c, b.run)
	before := x.rows()
	if out, err := compactWhileBusy(t, c, x.candidate()); out != compact.OutcomeApplied {
		t.Fatalf("a roll at its byte cap: %s, %v; want applied", out, err)
	}
	_, inputs := b.calls()
	if len(inputs) == 0 {
		t.Fatal("no build ran")
	}
	if got, limit := packBytesOf(before, inputs[0]), compact.InputByteCap(cut); got > limit ||
		got < limit/2 {
		t.Errorf("the roll merged %d pack bytes, want at its cap of %d: the case is about a roll AT it", got,
			limit)
	}
	if outs := outputs(before, x.rows()); len(outs) > code.MaxCompactOutputs {
		t.Errorf("a roll at its byte cap wrote %d outputs, past the %d one compaction records", len(outs),
			code.MaxCompactOutputs)
	}
}

// TOO MANY OUTPUTS HALVE THE NEXT PLAN: the build's own count is the
// authority on how many outputs it wrote, so a build past what one compaction
// records fails — nothing uploaded — and halves the bytes the repository's
// next plan merges, at every failure until one fits; and so does a build whose
// output is past the bundle rule even at the smaller cut its rebuild takes.
// Neither is remembered at the repository's version: at the pack bound every
// push carrying a pack is refused and records nothing, so a memory at the
// version would leave the repository refusing pushes until something else
// wrote to it.
//
// Mutation: keep the cap where it was, and the next plan merges the same bytes
// again; remember a bundle too large at the version, and the repository at the
// bound is never planned again.
func TestTooManyOutputsHalveTheNextPlan(t *testing.T) {
	t.Parallel()
	t.Run("outputs", func(t *testing.T) {
		t.Parallel()
		x := bulkFixture(t)
		rs := &recordingStore{Store: x.store}
		c, rec, _ := x.scaled(func(o *compact.Options) { o.Objects = rs })
		// Every build cut at pack-objects' floor, a quarter of the cut the
		// plan was capped for: its outputs pass what one compaction records
		// until the cap is down to a quarter.
		b := &builder{rewrite: func(_ int, args []string) []string { return cutAt(args, 1<<20) }}
		compact.BuildWith(c, b.run)
		before := x.rows()
		cand := x.candidate()
		applied := false
		for range 6 {
			if out, _ := compactWhileBusy(t, c, cand); out == compact.OutcomeApplied {
				applied = true
				break
			}
		}
		_, inputs := b.calls()
		if !applied || len(inputs) < 2 {
			t.Fatalf("%d builds, applied %v: want a failed one, its halved successors, and one that applied",
				len(inputs), applied)
		}
		for i := 1; i < len(inputs); i++ {
			was, now := packBytesOf(before, inputs[i-1]), packBytesOf(before, inputs[i])
			if now > was/2+int64(scaledBlob) {
				t.Errorf("build %d merged %d pack bytes after build %d's %d failed for its outputs, want half", i,
					now, i-1, was)
			}
		}
		if n := counter(rec, metrics.CodeCompactRefusals, "reason", "outputs"); n != uint64(len(inputs)-1) {
			t.Errorf("%s{reason=outputs} = %d, want one a failed build (%d builds)", metrics.CodeCompactRefusals, n,
				len(inputs))
		}
		if stored, _ := rs.stored(); len(stored) > code.MaxCompactOutputs {
			t.Errorf("%d objects were stored, more than the one compaction that applied wrote: a failed build "+
				"uploads nothing", len(stored))
		}
	})
	t.Run("bundle_too_large", func(t *testing.T) {
		t.Parallel()
		x := bulkFixture(t)
		est := &listed{Estate: served{x.fleet}}
		est.showPacks(code.MaxPacksPerRepo)
		c, rec, _ := x.scaled(func(o *compact.Options) { o.Estate = est })
		// No cut at all, so every output is the whole roll: past the bound at
		// the build and at its rebuild alike, until the cap is under it.
		b := &builder{rewrite: func(_ int, args []string) []string { return withoutCut(args) }}
		compact.BuildWith(c, b.run)
		before := x.rows()
		for pass := range 2 {
			builds := len(func() [][]string { _, in := b.calls(); return in }())
			pollWhileBusy(t, c, rec)
			_, inputs := b.calls()
			if len(inputs) == builds {
				t.Fatalf("pass %d built nothing: the repository at the bound was skipped", pass+1)
			}
		}
		_, inputs := b.calls()
		// Each pass builds twice — the build and its rebuild at the
		// offender's ratio — and each pair fails.
		if len(inputs) < 4 {
			t.Fatalf("%d builds over two passes, want two a pass", len(inputs))
		}
		if first, next := packBytesOf(before, inputs[0]), packBytesOf(before, inputs[2]); next > first/2+int64(scaledBlob) {
			t.Errorf("the second pass merged %d pack bytes after the first's %d failed bundle_too_large, want half",
				next, first)
		}
		if n := counter(rec, metrics.CodeCompactRefusals, "reason", "bundle_too_large"); n < 2 {
			t.Errorf("%s{reason=bundle_too_large} = %d over two failed passes", metrics.CodeCompactRefusals, n)
		}
	})
}
