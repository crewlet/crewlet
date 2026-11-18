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

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/words"
)

// PackConfig is the configuration a compaction's pack-objects runs under,
// beside the pool's base: index version 2 and a reverse index, the stored
// format the bundle's seal holds an output to (gitlet.CheckBundle), and the
// delta window held to twice the epoch's max_blob_bytes, as the serve
// configuration bounds a clone's. The bound is per thread (the pool's
// pack.threads=1 makes it the whole build's) and on the window alone; the
// memory charge counts six largest blobs (cache.CompactWindowBytes).
func PackConfig(maxBlob int64) []gitproc.KV {
	return []gitproc.KV{
		{Key: "pack.indexVersion", Value: "2"},
		{Key: "pack.writeReverseIndex", Value: "true"},
		{Key: "pack.windowMemory", Value: strconv.FormatInt(2*maxBlob, 10)},
	}
}

// cutFor is the --max-pack-size a build of p cuts its outputs at: the nominal
// cut, unless the indexes of every object the inputs hold (32 bytes an object:
// a version-2 .idx's 28 and a .rev's 4) could pass the room
// gitlet.MaxPackBytes leaves beside a full cut, where an output's bundle could
// be refused pack_too_large. Then the cut is scaled down by the inputs'
// AGGREGATE bundle-to-pack ratio, so an output's bundle lands near the nominal
// cut; aggregate, so one tiny pack's fixed index header never sets it
// (M3-decisions M3-O28).
func (b bounds) cutFor(p planned) int64 {
	objects := p.objects()
	if gitlet.IndexBytesFor(objects)+gitlet.ReverseBytesFor(objects) <= b.maxBundle-b.cut {
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
// gitlet.StoreBundle, which fills its Object and Hash.
type output struct {
	path string
	ref  gitlet.PackRef
}

// pack runs ONE pack-objects over p's inputs in the admission's reserved slot,
// in a merge's scratch sc, cut at cut, and answers the checksums it printed in
// the order it wrote them: the order their ids are derived in. The scratch's
// one alternate is a store of EXACTLY the inputs (cache.Cache.NewMergeScratch)
// and no pack is excluded with a `^` line, so every input object is rewritten
// (ADR-0090) and the walk ends at the inputs' edge, not the whole history
// (package doc, "A merge makes no reachability judgement").
//
// It names no Repairer: the compactor repairs nothing, so a fault in an input
// (the store links the incarnation's own files) reaches the cache's observer
// and fails the build; the next poll plans over the copy the cache repairs.
func (c *Compactor) pack(ctx context.Context, adm *cache.Admission, sc *cache.Scratch, p planned, cut int64,
	limits gitlet.Limits) ([]string, error) {

	var list strings.Builder
	for _, in := range p.inputs {
		list.WriteString("pack-" + in.checksum + ".pack\n")
	}
	res, err := c.runBuild(ctx, adm, gitproc.Cmd{
		Dir: sc.Dir, Alternate: sc.Alternate, Class: gitproc.ClassMaintenance,
		Args: []string{"pack-objects", "--stdin-packs", "--max-pack-size=" + strconv.FormatInt(cut, 10),
			"--delta-base-offset", "--quiet", "objects/pack/pack"},
		Stdin:    strings.NewReader(list.String()),
		Config:   PackConfig(limits.MaxBlobBytes),
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

// lay lays out every output the build printed, in printed order, under the
// derived ids of cid (a compaction's, or the publication's whose repack it is)
// with gitlet.LayBundle. The scratch, a merge's that held no pack before its
// build, must hold exactly the packs printed: one printed and not written, or
// written and not printed, is a build no row could describe.
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
		return nil, fmt.Errorf("compact: pack-objects printed %s and wrote %s", words.And(printed), words.And(wrote))
	}
	outs := make([]output, len(printed))
	for n, cs := range printed {
		path := filepath.Join(sc.Dir, "objects", "pack", "pack-"+cs+".pack")
		ref, err := gitlet.LayBundle(gitlet.DeriveOutputPackID(cid, n), path, cs)
		if err != nil {
			return nil, fmt.Errorf("compact: lay out output %d: %w", n, err)
		}
		outs[n] = output{path: path, ref: *ref}
	}
	return outs, nil
}

// pastBound is the bundle-to-pack ratio a rebuild cuts by: the densest
// output's whose bundle is past bound, or 0 when every output is within it.
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
func refs(outs []output) []gitlet.PackRef {
	out := make([]gitlet.PackRef, len(outs))
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

// outputObjects is how many objects outs hold, each counted by its own pack.
func outputObjects(outs []output) int64 {
	var n int64
	for _, o := range outs {
		n += o.ref.Objects
	}
	return n
}
