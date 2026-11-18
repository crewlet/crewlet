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

// The boot reads repos/ once, in Prepare, under the claim Open takes over,
// so no other process can change what Open acts on (package doc, "A boot
// starts from the disk, once").

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

// readBoot is Prepare's step 6: it reads repos/ and sums the bytes held under
// root — a kept incarnation by its state, else by a walk, and everything the
// boot gives back (every other entry of repos/, trash/, scratch/, tmp/) each
// file once (givenBack).
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
	// Every tree given back is walked before any is counted: a file's names
	// can lie in any two of them.
	gone := newGivenBack(ctx, skipped)
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
			// An older incarnation is given back, walked, never counted by
			// its state: the kept one holds hard links of its files
			// (seedRebuild, swap.go) and counts them, so its state too would
			// count them twice and Held, which the engine adds to free space
			// for the budget, would claim room the volume lacks. The walk
			// also counts once a file two older ones share (package doc, "A
			// boot starts from the disk, once").
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

	// The leftovers the janitor empties: a file also named in a kept
	// incarnation is counted by its state, any other once (givenBack).
	for _, name := range []string{trashDir, scratchDir, tmpDir} {
		if err = gone.walk(filepath.Join(root, name)); err != nil {
			return nil, 0, err
		}
	}
	return b, held + gone.bytes(), nil
}

// measureIncarnation is what an incarnation with no state holds, counted as a
// state counts it: every file under objects/, the dropping view
// (gitproc.DroppingDir, which a state counts in DiskBytes) and each root a
// declaration owns files in, plus packed-refs. The roots come from the
// declarations (derivedRoots, M3-O77), so every root validateAll admits is
// walked.
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

// lastUsedOn is when the incarnation at dir was last used: its state file's
// modification, else its directory's (the paths Cache.recordUse stamps),
// else zero.
func lastUsedOn(dir string) time.Time {
	for _, path := range []string{filepath.Join(dir, stateName), dir} {
		if info, err := os.Lstat(path); err == nil {
			return info.ModTime().UTC()
		}
	}
	return time.Time{}
}
