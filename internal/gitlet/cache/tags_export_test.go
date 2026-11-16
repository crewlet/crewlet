package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// TagsRel is the tag index, relative to its incarnation.
const TagsRel = tagsRel

// BeforeTags makes every round's tag step call f before it reads the facts of
// reading objects through git, its error failing the step.
func BeforeTags(o *Options, f func(ctx context.Context, dir string, reading int) error) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.tagging = f
}

// TagRows decodes the tag file at path, held to its format and its trailer,
// one line a tag in its order: ref, object, type, peeled, peeled type, and its
// date or "-" where it is undated.
func TagRows(path string) ([]string, error) {
	var out []string
	_, err := scanTagFile(path, tagScan{record: func(_ int, r *tagRecord, _ []byte) error {
		if err := validTagName(r.name); err != nil {
			return err
		}
		e := r.entry()
		date := "-"
		if e.Dated {
			date = fmt.Sprint(e.Date)
		}
		out = append(out, strings.Join([]string{e.Ref, e.OID, string(e.Type), e.Peeled, string(e.PeeledType), date},
			" "))
		return nil
	}})
	return out, err
}

// TagsServable reports whether repoID's current incarnation keeps a tag index
// a read is handed by its record, and when its last tag step failed.
func TagsServable(c *Cache, repoID string) (bool, time.Time) {
	e := lookup(c, repoID)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.current.derived[DerivedTags].Step == StepTags, e.current.tagsFailed
}

// TagsVouched reports whether repoID's current incarnation's tag index is
// vouched at the identity it has now.
func TagsVouched(c *Cache, repoID string) bool {
	e := lookup(c, repoID)
	e.mu.Lock()
	defer e.mu.Unlock()
	return vouchedAt(e.current, tagsRel, lstatID(filepath.Join(e.current.dir, tagsRel)))
}

// PlaceUnrenamed places, in repoID's current incarnation, a staged tag index
// that fails to rename in.
func PlaceUnrenamed(c *Cache, repoID string) {
	e := lookup(c, repoID)
	e.mu.Lock()
	defer e.mu.Unlock()
	st := tagStep{staged: filepath.Join(c.dir, tmpDir, "tags-never-staged")}
	c.placeTags(e, e.current, &st)
}

// UnvouchTags drops the vouch of repoID's current tag index and keeps its
// record: what a swap leaves where a read set the index aside between carrying
// the record and carrying the vouch.
func UnvouchTags(c *Cache, repoID string) {
	e := lookup(c, repoID)
	e.mu.Lock()
	defer e.mu.Unlock()
	unvouch(e.current, tagsRel)
}

// TagBench is the tag step over a bare repository outside any copy, run and
// read by the step's own code as a round and a read would: what
// BenchmarkTagIndex measures.
type TagBench struct {
	c      *Cache
	e      *entry
	inc    *incarnation
	digest string
}

// SetPackedRefs writes the repository's packed-refs as this cache writes one,
// of refs, so a step that builds whole on a delta reads its tags.
func (b *TagBench) SetPackedRefs(refs []gitlet.RefRow) error {
	rows := slices.Clone(refs)
	slices.SortFunc(rows, func(x, y gitlet.RefRow) int { return strings.Compare(x.Ref, y.Ref) })
	var out strings.Builder
	out.WriteString(packedRefsHeader)
	for _, r := range rows {
		out.WriteString(r.OID + " " + r.Ref + "\n")
	}
	sum := sha256.Sum256([]byte(out.String()))
	b.digest = hex.EncodeToString(sum[:])
	return os.WriteFile(filepath.Join(b.inc.dir, "packed-refs"), []byte(out.String()), 0o600)
}

// NewTagBench is the tag step of c's pool over the repository at dir.
func NewTagBench(c *Cache, dir string) *TagBench {
	inc := &incarnation{dir: dir}
	e := &entry{id: "tag-bench", current: inc}
	return &TagBench{c: c, e: e, inc: inc}
}

// BenchPeel is what a round's peel read of the tags a snapshot sets.
type BenchPeel struct {
	snap snapshot
	pf   peelFacts
}

// Peel peels refs as a round does, a full round's snapshot or a delta's moves:
// the round's own cost, which the step is handed.
func (b *TagBench) Peel(ctx context.Context, refs []gitlet.RefRow, full bool) (BenchPeel, error) {
	snap := snapshot{full: full, refs: slices.Clone(refs)}
	if full {
		slices.SortFunc(snap.refs, func(x, y gitlet.RefRow) int { return strings.Compare(x.Ref, y.Ref) })
	}
	pf, err := b.c.peel(ctx, b.e.id, b.inc.dir, snap, 0)
	return BenchPeel{snap: snap, pf: pf}, err
}

// Step levels the repository's tag index with refs, peeled as a round peels
// them, and places what it left.
func (b *TagBench) Step(ctx context.Context, refs []gitlet.RefRow, full bool) error {
	p, err := b.Peel(ctx, refs, full)
	if err != nil {
		return err
	}
	return b.StepPeeled(ctx, p)
}

// StepPeeled is Step over what a peel read already.
func (b *TagBench) StepPeeled(ctx context.Context, p BenchPeel) error {
	left := refsLeft{path: filepath.Join(b.inc.dir, "packed-refs"), digest: b.digest}
	st, err := b.c.levelTags(ctx, b.e, b.inc, p.snap, left, p.pf, 0, !p.snap.full)
	defer st.discard(b.c)
	switch {
	case err != nil:
		return err
	case st.failed != nil:
		return st.failed
	}
	b.e.mu.Lock()
	defer b.e.mu.Unlock()
	b.c.placeTags(b.e, b.inc, &st)
	return nil
}

// Open opens the repository's tag index for one read.
func (b *TagBench) Open() (*TagIndex, error) {
	f, err := os.Open(filepath.Join(b.inc.dir, tagsRel))
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	x, err := openTagIndex(f, info.Size())
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	x.f = f
	return x, nil
}

// TagKeyOf is the key of a tag of name dated date, as a cursor names it.
func TagKeyOf(date int64, name string) *TagKey { return &TagKey{Date: date, Ref: tagRefPrefix + name} }
