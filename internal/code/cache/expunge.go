package cache

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/gitref"
)

// WHAT AN EXPUNGE ASKS OF THE CACHE. A repository's maintainer expunges what
// no ref reaches (internal/code/compact) in two records, and the cache is
// where it sorts the objects: for the BEGIN, a scratch whose own packs are the
// inputs and whose alternate is a store of every other live pack
// ([Cache.StageExpunge]), so `pack-objects --revs --local` from the tips packs
// what a tip reaches of the inputs alone, and `--stdin-packs` over the inputs
// less what was kept and less the rest packs what nothing reaches; for the
// COMMIT's rescue, a scratch whose own packs are the dropping packs and whose
// alternate is the incarnation's objects/ ([Cache.StageRescue]), so the same
// `--revs --local` packs what a ref reaches of the dropping packs alone —
// what a push landed between the scan and the begin revived. Both scratches
// carry the incarnation's packed-refs, linked, so the refs the rewrite keeps
// what they reach of are one moment's with the packs it sorts
// ([Scratch.RefTips]); both are made under the entry's write lock (scratch.go's
// A REWRITE'S REPOSITORIES); and neither writes an incarnation: what the
// maintainer builds enters the cache by adoption (the kept and rescue packs,
// live) or by a round's install (the dropping packs, into the dropping view),
// once its rows have applied.

// ErrInputsLeaving is a rescue asked over a copy whose objects/ still holds the
// inputs of the begin it rescues for, or of any begin past the one its
// objects/ reflects (Synced.DropEpoch below Synced.Repo.DropEpoch): those
// inputs hold every object the begin left dropping, so a rescue leaning on
// that objects/ would find every revived object there, rescue none of them,
// and the commit would retire what a ref reaches. A moment's state — the swap
// that takes the inputs out is the round's — so the caller syncs again rather
// than give up.
var ErrInputsLeaving = errors.New("cache: the copy's objects/ still holds the inputs of a begin")

// StageExpunge makes the scratch an expunge's BUILD writes into: scratch/
// expunge-<id>.git, whose own objects/pack holds the inputs — every pack of
// inputs, git checksums each live in objects/pack, named once — and whose one
// alternate is a merge's store of EVERY OTHER pack the incarnation s pins
// holds live, both as hard links of the incarnation's files, with the
// incarnation's packed-refs linked beside them: the refs the scan keeps what
// they reach of ([Scratch.RefTips]). It pins s's incarnation until Remove.
// id names it — the expunge's own id, a canonical lowercase UUID, since it is
// a path component — once.
//
// THE STORE IS THE LIVE ROWS, NEVER THE DIRECTORY: a pack an installed layer
// holds in objects/pack for no live row (layers.go) is a compaction's input,
// every object of which is in the compaction's outputs — so in the store it
// would hide from the build an object the inputs hold, kept nowhere once the
// begin retires them.
//
// Refused ([ErrInvalid]), with nothing made: no input, an input named twice or
// not live in objects/pack, and a copy holding a row an expunge's begin left
// dropping — an expunge open, whose commit comes before another begin.
func (c *Cache) StageExpunge(ctx context.Context, s Synced, id string, inputs []string) (*Scratch, error) {
	if err := c.handedOut(s, "an expunge's scratch"); err != nil {
		return nil, err
	}
	switch {
	case !canonicalID(id):
		return nil, fmt.Errorf("%w: expunge id %q is not a canonical lowercase UUID (it becomes a path component)",
			ErrInvalid, id)
	case len(inputs) == 0:
		return nil, fmt.Errorf("%w: an expunge's scratch names the packs it sorts, and this one names none",
			ErrInvalid)
	}
	return c.newRewriteScratch(ctx, s, "expunge-"+id, func(inc *incarnation) (rewritePick, error) {
		named := make(map[string]bool, len(inputs))
		for _, cs := range inputs {
			switch {
			case named[cs]:
				return rewritePick{}, fmt.Errorf("pack-%s is named twice", cs)
			case placeOf(inc, cs) != placeLive:
				return rewritePick{}, fmt.Errorf("pack-%s is no pack live in %s", cs, inc.dir)
			}
			named[cs] = true
		}
		var rest []string
		for cs, r := range inc.byChecksum {
			if r.dropping > 0 {
				return rewritePick{}, fmt.Errorf("pack-%s is dropping in %s: an expunge is open, and its commit "+
					"comes before another begins", cs, inc.dir)
			}
			if !named[cs] {
				rest = append(rest, cs)
			}
		}
		slices.Sort(rest)
		return rewritePick{local: slices.Clone(inputs), place: placeLive, store: rest, leanStore: true}, nil
	})
}

// StageRescue makes the scratch an expunge's RESCUE writes into:
// scratch/rescue-<id>.git, whose own objects/pack holds every pack the
// incarnation s pins keeps in its dropping view — linked from there — and
// whose one alternate is the incarnation's objects/, its live packs and never
// its dropping view, with the incarnation's packed-refs linked beside them:
// the refs at the rescue's sync ([Scratch.RefTips]). A copy holding no
// dropping pack makes a scratch holding none. It pins s's incarnation until
// Remove. id names it — the commit's own id, a canonical lowercase UUID — once.
//
// Refused [ErrInputsLeaving] where the copy's objects/ still holds a begin's
// inputs (Synced.DropEpoch below the row's), and [ErrInvalid] for an id no
// scratch may take; nothing is made either way.
func (c *Cache) StageRescue(ctx context.Context, s Synced, id string) (*Scratch, error) {
	if err := c.handedOut(s, "a rescue's scratch"); err != nil {
		return nil, err
	}
	if !canonicalID(id) {
		return nil, fmt.Errorf("%w: rescue id %q is not a canonical lowercase UUID (it becomes a path component)",
			ErrInvalid, id)
	}
	if s.DropEpoch != s.Repo.DropEpoch {
		return nil, fmt.Errorf("%w: %s holds drop epoch %d in its objects/ and its row %d", ErrInputsLeaving,
			s.Dir, s.DropEpoch, s.Repo.DropEpoch)
	}
	return c.newRewriteScratch(ctx, s, "rescue-"+id, func(inc *incarnation) (rewritePick, error) {
		var local []string
		for cs, r := range inc.byChecksum {
			if placeOfRows(r) == placeDropping {
				local = append(local, cs)
			}
		}
		slices.Sort(local)
		return rewritePick{local: local, place: placeDropping}, nil
	})
}

// DroppingPacks is the rows of the incarnation s pins that an expunge's begin
// left dropping, sorted by pack id — each row's id, its git checksum, its
// bundle's bytes, its objects and its state, as [Cache.Packs] reads a live
// one — what the expunge's commit retires, every one, and what its rescue is
// charged by. Copied under the entry's lock; nil for a Synced this cache did
// not hand out.
func (c *Cache) DroppingPacks(s Synced) []code.PackRow {
	if c.handedOut(s, "a read of the dropping packs") != nil {
		return nil
	}
	s.entry.mu.Lock()
	var out []code.PackRow
	for id, p := range s.inc.live {
		if p.dropping {
			row := p.row(id)
			row.State = code.PackDropping
			out = append(out, row)
		}
	}
	s.entry.mu.Unlock()
	slices.SortFunc(out, func(a, b code.PackRow) int { return strings.Compare(a.PackID, b.PackID) })
	return out
}

// RefTips is every object the scratch's refs name, each once, sorted — the
// object ids of its packed-refs, linked from the incarnation when the scratch
// was made ([Cache.StageExpunge], [Cache.StageRescue]): the tips a rewrite
// keeps what they reach of. A tag's peeled line is no tip of its own: the tag
// object reaches what it peels to. A scratch with no packed-refs — one a push
// receives into — names none.
//
// READ AS THE CACHE WROTE IT (refs.go): a header, then a line a ref, its
// object id and its name, and a peeled line under an annotated tag; a line
// outside that grammar is an error naming it, since a tip read wrong is an
// object a rewrite would drop.
func (s *Scratch) RefTips() ([]string, error) {
	f, err := os.Open(filepath.Join(s.Dir, "packed-refs"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cache: reading %s's refs: %w", s.Dir, err)
	}
	defer func() { _ = f.Close() }()
	seen := map[string]bool{}
	lines := bufio.NewScanner(f)
	// A LINE IS AT MOST AN ID, A SPACE AND A REF: what the cache writes, and
	// the scanner's default buffer holds it.
	lines.Buffer(nil, gitref.OIDHexLen+1+gitref.MaxRefNameBytes+1)
	for n := 1; lines.Scan(); n++ {
		line := lines.Text()
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		oid, ref, ok := strings.Cut(line, " ")
		if !ok || !gitref.ValidOID(oid) || ref == "" {
			return nil, fmt.Errorf("cache: %s's packed-refs line %d is no ref: %q", s.Dir, n, line)
		}
		seen[oid] = true
	}
	if err := lines.Err(); err != nil {
		return nil, fmt.Errorf("cache: reading %s's refs: %w", s.Dir, err)
	}
	return slices.Sorted(func(yield func(string) bool) {
		for oid := range seen {
			if !yield(oid) {
				return
			}
		}
	}), nil
}
