package cache

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/flock"
)

// THE BOOT'S READING of repos/: done once, by Prepare, under the claim Open
// takes over — so what Open acts on is what is on the disk, with no second
// reading to pay for and no window in which another process could have
// changed it. A boot reads nothing but the directory: every repository's
// highest incarnation is kept with its state, and everything else under
// repos/ is moved aside by Open.

// boot is what Prepare hands Open inside a Layout.
type boot struct {
	// dir is the root Prepare resolved and claimed.
	dir string
	// claim is this process's hold on dir, from Prepare to Stop.
	claim *flock.Lock
	// taken is set by whichever of Open and Layout.Release takes the claim
	// first: a Layout opens once.
	taken atomic.Bool

	// keep is each repository's highest incarnation, by id order.
	keep []kept
	// aside is every entry of repos/ Open moves into trash/, with why.
	aside []aside
}

// kept is one repository's incarnation as Prepare found it.
type kept struct {
	id   string
	n    int
	name string
	// state is its state; nil when the file is absent (stateErr nil) or
	// says nothing this build can use (stateErr names why).
	state    *state
	stateErr error
	// measured is what it holds when it has no state: its files under
	// objects/ and its packed-refs.
	measured int64
	// lastUsed is its state file's modification, else the directory's;
	// zero when neither could be read.
	lastUsed time.Time
}

// aside is an entry of repos/ Open moves into trash/.
type aside struct {
	name string
	why  string
}

// readBoot is Prepare's step 6: the reading of repos/ and the bytes the cache
// holds under root — each repository's kept incarnation as its state counts
// it, one with no state as a walk finds it, and everything the boot gives
// back, each file once (givenBack): every other entry of repos/, and trash/,
// scratch/ and tmp/.
func readBoot(ctx context.Context, root string) (*boot, int64, error) {
	skipped := func(path string, err error) {
		log.Warn("code_cache_held_unmeasured", "path", path, "error", err)
	}
	b := &boot{dir: root}
	repos := filepath.Join(root, reposDir)
	list, err := os.ReadDir(repos)
	if err != nil {
		return nil, 0, fmt.Errorf("cache: listing %s: %w", repos, err)
	}

	var held int64
	// gone is what the boot gives back, every tree of it walked before any
	// is counted: a file's names can lie in any two of them.
	gone := newGivenBack(ctx, skipped)
	// walk adds an entry Open moves aside to what the boot gives back.
	walk := func(name string) error { return gone.walk(filepath.Join(repos, name)) }
	type found struct {
		name string
		n    int
	}
	byID := map[string][]found{}
	for _, d := range list {
		if err = ctx.Err(); err != nil {
			return nil, 0, fmt.Errorf("cache: reading %s: %w", repos, err)
		}
		id, n, ok := parseIncarnation(d.Name())
		switch {
		case !ok:
			b.aside = append(b.aside, aside{d.Name(), "not a repository's directory (repos/<id>.<n>.git)"})
		case !d.IsDir():
			b.aside = append(b.aside, aside{d.Name(), "not a directory"})
		default:
			byID[id] = append(byID[id], found{name: d.Name(), n: n})
			continue
		}
		if err = walk(d.Name()); err != nil {
			return nil, 0, err
		}
	}

	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if err = ctx.Err(); err != nil {
			return nil, 0, fmt.Errorf("cache: reading %s: %w", repos, err)
		}
		incs := byID[id]
		slices.SortFunc(incs, func(a, b found) int { return cmp.Compare(a.n, b.n) })
		for _, older := range incs[:len(incs)-1] {
			b.aside = append(b.aside, aside{older.name, "an older incarnation of " + id})
			// AN OLDER INCARNATION IS WHAT THE BOOT GIVES BACK, never what its
			// state says it holds. It is one a crash left beside the
			// incarnation kept — a rebuild's predecessor, or a retired one a
			// reader still held — and the kept one holds HARD LINKS of its
			// files: a rebuild links its predecessor's every pack a check has
			// just found whole (seedRebuild), and a swap its predecessor's
			// every live pack, HEAD, config, packed-refs and the derived files
			// a retirement keeps but the memo, whose tree it moves whole
			// (swap.go). The kept one's state counts those, so counted by its
			// own state as well, every linked pack was counted twice, and the
			// engine, which adds Held to the volume's free space to derive the
			// cache's budget, claimed room the volume did not have, by the
			// linked bytes. Walked rather than read from its state: an older
			// incarnation exists only where a crash left one, its walk one
			// Lstat a file of a directory the janitor is about to remove; and
			// two older ones can share a file the kept one does not — a pack
			// the second swap took out, linked by the first — which the walk
			// counts once.
			if err = walk(older.name); err != nil {
				return nil, 0, err
			}
		}

		top := incs[len(incs)-1]
		dir := filepath.Join(repos, top.name)
		st, stateErr := readState(dir, id)
		var foreign *formatError
		if errors.As(stateErr, &foreign) {
			b.aside = append(b.aside, aside{top.name, foreign.Error()})
			if err = walk(top.name); err != nil {
				return nil, 0, err
			}
			continue
		}
		k := kept{id: id, n: top.n, name: top.name, state: st, stateErr: stateErr, lastUsed: lastUsedOn(dir)}
		if st != nil {
			held += st.DiskBytes + st.derivedBytes()
		} else {
			if k.measured, err = measureIncarnation(ctx, dir, skipped); err != nil {
				return nil, 0, err
			}
			held += k.measured
		}
		b.keep = append(b.keep, k)
	}

	// The leftovers, which the janitor empties: a file in them with a name
	// in a kept incarnation is that incarnation's, counted by its state
	// above, and one whose names all lie in what the boot gives back is
	// counted once, wherever they lie.
	for _, name := range []string{trashDir, scratchDir, tmpDir} {
		if err = gone.walk(filepath.Join(root, name)); err != nil {
			return nil, 0, err
		}
	}
	return b, held + gone.bytes(), nil
}

// measureIncarnation is what an incarnation with no state holds, counted as a
// state counts it: the files under objects/ — the packs, and every derived
// file git writes — under the dropping view (gitproc.DroppingDir: the packs an
// expunge's begin left dropping, which a state counts in DiskBytes too), and
// under every other root a declaration owns files in
// (derivedRoots: crewlet/, the cache's own derived files), since validateAll
// refuses a declaration that owns or holds a file anywhere else; and
// packed-refs. The roots are READ FROM THE DECLARATIONS, so a root
// validateAll admits is one this walk reads (M3-decisions M3-O77). Its first
// round reads the whole answer and measures the two halves exactly.
func measureIncarnation(ctx context.Context, dir string, skipped func(string, error)) (int64, error) {
	var total int64
	for _, root := range derivedRoots(DerivedFiles()) {
		n, err := sizeUnder(ctx, filepath.Join(dir, root), allNames, skipped, nil)
		if err != nil {
			return 0, err
		}
		total += n
	}
	refs := filepath.Join(dir, "packed-refs")
	if info, lerr := os.Lstat(refs); lerr == nil {
		total += info.Size()
	} else if !errors.Is(lerr, fs.ErrNotExist) {
		skipped(refs, lerr)
	}
	return total, nil
}

// lastUsedOn is when the incarnation at dir was last used, as near as the
// disk says: its state file's modification, else its directory's — the
// paths, in that order, the janitor stamps with the moment of last use
// (Cache.recordUse) — else zero.
func lastUsedOn(dir string) time.Time {
	for _, path := range []string{filepath.Join(dir, stateName), dir} {
		if info, err := os.Lstat(path); err == nil {
			return info.ModTime().UTC()
		}
	}
	return time.Time{}
}
