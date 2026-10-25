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
// holds under root — its incarnations as their states count them, what has no
// state as a walk finds it, and trash/, scratch/ and tmp/.
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
	// walk adds what an entry Open moves aside holds: all of it goes back
	// to the volume once the janitor has removed it.
	walk := func(name string) error {
		n, werr := sizeUnder(ctx, filepath.Join(repos, name), allNames, skipped)
		held += n
		return werr
	}
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
			// An older incarnation's own state counts it, when it has one
			// this build reads: an interrupted rebuild's predecessor can
			// hold a large repository's every pack.
			if st, _ := readState(filepath.Join(repos, older.name), id); st != nil {
				held += st.DiskBytes + st.derivedBytes()
			} else if err = walk(older.name); err != nil {
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

	// scratch/ counts only what has no other name, as the janitor's measure
	// does: a file there with a second one is an incarnation's, counted by
	// its state above, or one in trash/, counted by the walk of trash/.
	for name, rule := range map[string]nameRule{trashDir: allNames, scratchDir: soleNames, tmpDir: allNames} {
		n, werr := sizeUnder(ctx, filepath.Join(root, name), rule, skipped)
		if werr != nil {
			return nil, 0, werr
		}
		held += n
	}
	return b, held, nil
}

// measureIncarnation is what an incarnation with no state holds, counted as a
// state counts it: the files under objects/ — the packs, and every derived
// file, since validateAll refuses a declaration that owns or holds a file
// anywhere else — and packed-refs. Reading objects/ alone is right only
// while that rule stands: a root validateAll admits is one this walk must
// read too, and the two change together. Its first round reads the whole
// answer and measures the two halves exactly.
func measureIncarnation(ctx context.Context, dir string, skipped func(string, error)) (int64, error) {
	total, err := sizeUnder(ctx, filepath.Join(dir, "objects"), allNames, skipped)
	if err != nil {
		return 0, err
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
