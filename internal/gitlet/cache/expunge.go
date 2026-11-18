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

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitref"
)

// WHAT AN EXPUNGE ASKS OF THE CACHE (internal/gitlet/compact): a scratch for
// each of its two records. The BEGIN's ([Cache.StageExpunge]) holds the inputs
// over an alternate of every other live pack, so `pack-objects --revs --local`
// from the tips packs what a tip reaches of the inputs alone, and
// `--stdin-packs` over the inputs less the kept and the rest packs what nothing
// reaches. The COMMIT's rescue ([Cache.StageRescue]) holds the dropping packs
// over the incarnation's objects/, so `--revs --local` packs what a ref reaches
// of them alone: what a push revived between the scan and the begin. Both link
// the incarnation's packed-refs, one moment's with its packs ([Scratch.RefTips]),
// are made under the entry's write lock (scratch.go's A REWRITE'S
// REPOSITORIES), and write no incarnation: outputs enter this copy by adoption
// once their rows apply ([Cache.Adopt], [Cache.AdoptDropping]), others by install.

// ErrInputsLeaving is a rescue asked over a copy whose objects/ still holds a
// begin's inputs (Synced.DropEpoch below Synced.Repo.DropEpoch): they hold every
// object the begin left dropping, so the rescue would find every revived object
// there, rescue none, and the commit would retire what a ref reaches. The swap
// that takes the inputs out is the round's, so the caller syncs again.
var ErrInputsLeaving = errors.New("cache: the copy's objects/ still holds the inputs of a begin")

// StageExpunge makes the scratch an expunge's BUILD writes into:
// scratch/expunge-<id>.git (id the expunge's, a canonical lowercase UUID, as a
// path component), whose objects/pack links the inputs (git checksums, each
// live, named once) and whose one alternate stores EVERY OTHER live pack of
// the incarnation serving s (newRewriteScratch), with its packed-refs linked
// ([Scratch.RefTips]). It pins that incarnation until Remove. The store is the
// live rows, never the directory: a pack a layer keeps for no live row
// (layers.go) is a compaction's input, and would hide from the build an object
// the inputs hold, kept nowhere once the begin retires them.
// Refused ([ErrInvalid]), nothing made: no input, an input named twice or not
// live, or a copy holding a dropping row (an expunge open, whose commit comes
// before another begin).
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
// scratch/rescue-<id>.git (id the commit's, a canonical lowercase UUID), whose
// objects/pack links every pack in the dropping view of the incarnation serving
// s (newRewriteScratch; none where the copy holds none) and whose one alternate
// is that incarnation's objects/, its live packs only, with its packed-refs
// linked: the refs at the rescue's sync ([Scratch.RefTips]). It pins that
// incarnation until Remove.
//
// Refused, nothing made: [ErrInputsLeaving] where the copy's objects/ still
// holds a begin's inputs, and [ErrInvalid] for an id no scratch may take.
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

// DroppingPacks is the rows an expunge's begin left dropping in the incarnation
// s pins, sorted by pack id and read as [Cache.Packs] reads a live one: what the
// commit retires, every one, and charges its rescue by. Copied under the
// entry's lock. Refused [ErrSuperseded] where a swap replaced that incarnation,
// as [Cache.Packs] is: it would answer none, and a commit retiring none is
// refused for not retiring what its expunge left dropping. [ErrInvalid] for a
// Synced this cache did not hand out.
func (c *Cache) DroppingPacks(s Synced) ([]gitlet.PackRow, error) {
	if err := c.handedOut(s, "a read of the dropping packs"); err != nil {
		return nil, err
	}
	s.entry.mu.Lock()
	if s.inc.successor != nil {
		s.entry.mu.Unlock()
		return nil, fmt.Errorf("%w: %s, read for its dropping packs", ErrSuperseded, s.Dir)
	}
	var out []gitlet.PackRow
	for id, p := range s.inc.live {
		if p.dropping {
			row := p.row(id)
			row.State = gitlet.PackDropping
			out = append(out, row)
		}
	}
	s.entry.mu.Unlock()
	slices.SortFunc(out, func(a, b gitlet.PackRow) int { return strings.Compare(a.PackID, b.PackID) })
	return out, nil
}

// RefTips is every object the scratch's packed-refs name (linked from the
// incarnation by [Cache.StageExpunge] or [Cache.StageRescue]), each once,
// sorted: the tips a rewrite keeps what they reach of. A tag's peeled line is no
// tip: the tag reaches it. A scratch with no packed-refs (one a push receives
// into) names none. A line outside the grammar the cache writes (refs.go) is an
// error naming it, since a tip read wrong is an object a rewrite would drop.
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
	// A line is at most an id, a space and a ref, as the cache writes it.
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
