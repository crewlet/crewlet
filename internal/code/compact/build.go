package compact

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
)

// CompactConfig is the configuration a compaction's pack-objects runs under,
// beside the pool's base: every output a bundle in the stored format — its
// index version 2 and its reverse index written beside it, which the bundle's
// seal holds it to (code.CheckBundle) — and the delta window held to what the
// cache's serve configuration bounds a clone's pack-objects to, twice the
// epoch's max_blob_bytes. A PER-THREAD bound, which the pool's pack.threads=1
// makes the build's whole window — and a bound on the window alone: the build
// held 5.4 of the largest blobs around it where its search met them, which is
// what its memory charge counts (cache.CompactWindowBytes, six of them).
func CompactConfig(maxBlob int64) []gitproc.KV {
	return []gitproc.KV{
		{Key: "pack.indexVersion", Value: "2"},
		{Key: "pack.writeReverseIndex", Value: "true"},
		{Key: "pack.windowMemory", Value: strconv.FormatInt(2*maxBlob, 10)},
	}
}

// cutFor is the --max-pack-size a build of p cuts its outputs at: the nominal
// cut, unless the inputs are dense enough that an output cut there could be a
// BUNDLE past the bound every stored pack is held to — an index costs 32 bytes
// an object (a version-2 .idx's 28 and a .rev's 4), so an output of some 8.39
// million objects under its 1 GiB cut, a stretch of small deltas, is a bundle
// past code.MaxPackBytes, which its committer would refuse pack_too_large.
// Where the indexes of every object the inputs hold could pass the room the
// bound leaves beside a full cut, the cut is scaled down by the inputs'
// AGGREGATE bundle-to-pack ratio — their bundles' bytes over their pack
// sections' — so an output's bundle lands near the nominal cut. In aggregate,
// so one tiny pack's fixed index header never sets it (M3-decisions M3-O28).
func (b bounds) cutFor(p planned) int64 {
	objects := p.objects()
	if code.IndexBytesFor(objects)+code.ReverseBytesFor(objects) <= b.maxBundle-b.cut {
		return b.cut
	}
	return scaledCut(b.cut, p.size(), p.packBytes())
}

// scaledCut is cut scaled down by a ratio of bundle bytes to pack bytes:
// the cut at which an output of that density is a bundle of about cut.
func scaledCut(cut, bundle, pack int64) int64 {
	if pack <= 0 || bundle <= pack {
		return cut
	}
	return int64(float64(cut) * float64(pack) / float64(bundle))
}

// output is one pack a build wrote: its .pack's path, and the bundle its
// three files make, under the compaction's derived id for it — stored by
// code.StoreBundle, which fills its Object and Hash.
type output struct {
	path string
	ref  code.PackRef
}

// pack runs ONE pack-objects over p's inputs in the admission's reserved slot,
// in the scratch sc — every input read through the scratch's one alternate, a
// store holding EXACTLY the inputs, hard links of the packs of the incarnation
// the plan was made over (cache.Cache.NewMergeScratch), so the scratch holds
// no input and the build writes into nothing but the scratch — cut at cut, and
// answers the checksums it printed, one an output, in the order it wrote them:
// the order their ids are derived in.
//
// NO EXCLUDED PACK: the inputs are listed and nothing is excluded with a `^`
// line, so every object an input holds is in the outputs — an object it shares
// with a pack that is no input is written again rather than left to that pack,
// because two compactions decided from different snapshots may each retire the
// pack the other relied on (ADR-0060).
//
// AND NOTHING BUT THE INPUTS IN REACH: to fill in the name hash its delta
// search sorts by, `--stdin-packs` walks from every commit its inputs hold
// through every tree and blob it can reach, and only an excluded pack stops it
// — so over the whole incarnation a roll of a few small packs walked the
// repository's history, its cost and its memory following the history while its
// charge and its deadline follow the roll (measured at review, about 3.5 µs an
// object of a 400 032-object history on the node pin, and for a roll of eight
// 451 ms and 100.8 MiB beside the 100 MB source history in BenchmarkCompact's
// roll arm: past the deadline of a roll of small pushes from a few million
// objects, a gigabyte or two of a source repository, so every such roll was
// killed and the repository went on to refuse pushes compaction_behind). Over a
// store of the inputs alone the walk ends at their edge, as git walks with
// ignore_missing_links, and writes the same packs.
//
// The build names no Repairer: the compactor repairs nothing itself, so what
// the build reports of an input — the store's links are the incarnation's own
// files — reaches the cache's observer as it arrives, and the compaction
// fails; the next poll plans again over the copy the cache repairs.
func (c *Compactor) pack(ctx context.Context, adm *cache.Compacting, sc *cache.Scratch, p planned, cut int64,
	limits code.Limits) ([]string, error) {

	var list strings.Builder
	for _, in := range p.inputs {
		list.WriteString("pack-" + in.checksum + ".pack\n")
	}
	res, err := c.runBuild(ctx, adm, gitproc.Cmd{
		Dir: sc.Dir, Alternate: sc.Alternate, Class: gitproc.ClassMaintenance,
		Args: []string{"pack-objects", "--stdin-packs", "--max-pack-size=" + strconv.FormatInt(cut, 10),
			"--delta-base-offset", "--quiet", "objects/pack/pack"},
		Stdin:    strings.NewReader(list.String()),
		Config:   CompactConfig(limits.MaxBlobBytes),
		Deadline: gitproc.PackWorkDeadline(p.packBytes()),
	})
	if err != nil {
		return nil, fmt.Errorf("compact: pack-objects over %d packs: %w", len(p.inputs), err)
	}
	var printed []string
	lines := bufio.NewScanner(bytes.NewReader(res.Stdout))
	for lines.Scan() {
		cs := strings.TrimSpace(lines.Text())
		if !gitref.ValidOID(cs) {
			return nil, fmt.Errorf("compact: pack-objects printed %q where it names a pack it wrote", cs)
		}
		printed = append(printed, cs)
	}
	if err := lines.Err(); err != nil {
		return nil, fmt.Errorf("compact: read what pack-objects printed: %w", err)
	}
	if len(printed) == 0 {
		return nil, fmt.Errorf("compact: pack-objects over %d packs printed no pack", len(p.inputs))
	}
	return printed, nil
}

// lay lays out every output the build printed, in the order it printed them,
// under the compaction cid's derived ids: each output's sections' sizes from
// its files and its object count from its index (code.LayBundle). The scratch
// must hold exactly the packs the build printed — a pack it printed and did not
// write, or one it wrote and did not print, is a build no row could describe.
func lay(sc *cache.Scratch, cid string, printed []string) ([]output, error) {
	held, err := sc.Packs()
	if err != nil {
		return nil, err
	}
	wrote := make([]string, len(held))
	for i, p := range held {
		wrote[i] = p.Checksum
	}
	if want := slices.Sorted(slices.Values(printed)); !slices.Equal(want, wrote) {
		return nil, fmt.Errorf("compact: pack-objects printed %v and wrote %v", printed, wrote)
	}
	outs := make([]output, len(printed))
	for n, cs := range printed {
		path := filepath.Join(sc.Dir, "objects", "pack", "pack-"+cs+".pack")
		ref, err := code.LayBundle(code.DeriveOutputPackID(cid, n), path, cs)
		if err != nil {
			return nil, fmt.Errorf("compact: lay out output %d: %w", n, err)
		}
		outs[n] = output{path: path, ref: *ref}
	}
	return outs, nil
}

// pastBound is the bundle-to-pack ratio of the densest output whose bundle is
// past bound — the ratio a rebuild cuts by — or 0 when every output is within
// it.
func pastBound(outs []output, bound int64) float64 {
	var worst float64
	for _, o := range outs {
		if o.ref.Size > bound {
			worst = max(worst, float64(o.ref.Size)/float64(o.ref.PackBytes()))
		}
	}
	return worst
}

// refs is every output's bundle, in order: the compaction's Add.
func refs(outs []output) []code.PackRef {
	out := make([]code.PackRef, len(outs))
	for i, o := range outs {
		out[i] = o.ref
	}
	return out
}

// outputBytes is what the outputs weigh, every bundle whole.
func outputBytes(outs []output) int64 {
	var n int64
	for _, o := range outs {
		n += o.ref.Size
	}
	return n
}
