package homecache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/flock"
)

// lockPoll is how often a taker waiting on a key's lock asks again. The lock
// is held for a copy (0.18-0.6 s measured, see the package doc) or a build
// (seconds to minutes): a waiter is late by at most a poll, under a third of
// the shortest copy, and costs nothing while it sleeps.
const lockPoll = 50 * time.Millisecond

// Cache is a node's cache of provisioned homes under one root ([Open]).
type Cache struct {
	root    string
	exclude []string

	// waiting, when set, is called once by a taker of key that finds its lock
	// held, before it waits: the signal a test that two takers overlap rests on.
	waiting func(key Key)
	// copying, when set, is called with key's lock held, before its tree is
	// copied out: the signal a test of a copy in flight rests on.
	copying func(key Key)
}

// Open is the cache rooted at root, made if missing. exclude is every path,
// relative to a built home, that is the box's own rather than its setup's
// (its work tree, the engine's records in it) and is never kept.
func Open(root string, exclude ...string) (*Cache, error) {
	c := &Cache{root: root, exclude: slices.Clone(exclude)}
	for _, dir := range []string{c.trees(), c.cuts(), c.objects(), c.locks()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("homecache: making %s: %w", dir, err)
		}
	}
	return c, nil
}

func (c *Cache) trees() string   { return filepath.Join(c.root, "trees") }
func (c *Cache) cuts() string    { return filepath.Join(c.root, "cuts") }
func (c *Cache) objects() string { return filepath.Join(c.root, "objects") }
func (c *Cache) locks() string   { return filepath.Join(c.root, "locks") }

func (c *Cache) tree(key Key) string { return filepath.Join(c.trees(), string(key)) }
func (c *Cache) cut(key Key) string  { return filepath.Join(c.cuts(), string(key)+".json") }

// Builder runs steps in a box made for the build and hands back its home. It
// is handed no credential (the package doc), and its home must be on the
// cache's filesystem, since the cache takes it over by renaming it.
type Builder func(ctx context.Context, steps []Step) (Built, error)

// Built is what a [Builder] left.
type Built struct {
	// Home is the build box's home, the cache's from now on (also when
	// Escaped is set, when the cache removes it).
	Home string
	// Escaped is the first step whose effects left the home, if one did.
	Escaped *Escape
}

// Escape is a step whose effects reached outside the home, so it and every
// later step run in each box instead.
type Escape struct {
	// Step is the step's index in the steps the builder was handed.
	Step int `json:"step"`
	// Name is the step's name.
	Name string `json:"name"`
	// Paths is what the step changed outside the home.
	Paths []string `json:"paths"`
}

// Result is what [Cache.Provision] did.
type Result struct {
	// Prefix is how many of the input's steps the copied home holds: the
	// caller runs the rest in the box. Zero copies nothing.
	Prefix int
	// Key is the tree copied, empty when Prefix is zero.
	Key Key
	// Built is whether this call built it.
	Built bool
	// Cut is the step this call found leaving the home ([Built.Escaped]),
	// for the caller to say so; a later call finds the cut and says nothing.
	Cut *Escape
}

// SecretError is a build whose tree held a credential value: discarded.
type SecretError struct {
	// Path is where, relative to the home: never the value.
	Path string
}

func (e *SecretError) Error() string {
	return fmt.Sprintf("homecache: a build left a credential at %s in the home, which was handed none; "+
		"the tree is discarded", e.Path)
}

// Provision fills dst, a new box's home, from the cache: the tree of in's
// cached prefix, built by build first if this node has none. secrets is every
// credential value a launch would hand the box: none may be in a tree. A
// zero [Result.Prefix] copies nothing, and the caller runs every step.
func (c *Cache) Provision(ctx context.Context, in Input, secrets []string, build Builder, dst string) (Result, error) {
	var res Result
	prefix := CachedPrefix(in.Steps, in.Withheld)
	for prefix > 0 {
		key := KeyOf(in, prefix)
		if cut, ok, err := c.readCut(key, prefix); err != nil {
			return res, err
		} else if ok {
			prefix = cut.Step
			continue
		}
		release, err := c.lock(ctx, key)
		if err != nil {
			return res, err
		}
		cut, built, err := c.ensure(ctx, key, in.Steps[:prefix], secrets, build)
		if err == nil && cut == nil {
			if c.copying != nil {
				c.copying(key)
			}
			err = copyTree(c.tree(key), dst)
		}
		release()
		switch {
		case err != nil:
			return res, err
		case cut != nil:
			if built {
				res.Cut = cut
			}
			prefix = cut.Step
			continue
		}
		res.Prefix, res.Key, res.Built = prefix, key, built
		return res, nil
	}
	return res, nil
}

// ensure makes key's tree exist, holding key's lock: there already, built now,
// or cut (the step to stop before, and whether this call found it).
func (c *Cache) ensure(ctx context.Context, key Key, steps []Step, secrets []string, build Builder) (*Escape, bool, error) {
	if cut, ok, err := c.readCut(key, len(steps)); err != nil || ok {
		return &cut, false, err
	}
	if _, err := os.Stat(c.tree(key)); err == nil {
		return nil, false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, fmt.Errorf("homecache: reading %s: %w", c.tree(key), err)
	}
	b, err := build(ctx, slices.Clone(steps))
	if err != nil {
		return nil, false, err
	}
	if b.Escaped != nil {
		_ = removeTree(b.Home)
		if b.Escaped.Step < 0 || b.Escaped.Step >= len(steps) {
			return nil, false, fmt.Errorf("homecache: the build named step %d as leaving the home, "+
				"of %d steps", b.Escaped.Step, len(steps))
		}
		if err = c.writeCut(key, *b.Escaped); err != nil {
			return nil, false, err
		}
		return b.Escaped, true, nil
	}
	if err = c.publish(b.Home, key, secrets); err != nil {
		_ = removeTree(b.Home)
		return nil, false, err
	}
	return nil, true, nil
}

// readCut is key's cut, if it has one. of is how many steps key holds: a cut
// at or past it cannot have come from building key, and is refused.
func (c *Cache) readCut(key Key, of int) (Escape, bool, error) {
	raw, err := os.ReadFile(c.cut(key))
	if errors.Is(err, fs.ErrNotExist) {
		return Escape{}, false, nil
	} else if err != nil {
		return Escape{}, false, fmt.Errorf("homecache: reading %s: %w", c.cut(key), err)
	}
	var cut Escape
	if err = json.Unmarshal(raw, &cut); err != nil || cut.Step < 0 || cut.Step >= of {
		return Escape{}, false, fmt.Errorf("homecache: %s is no cut of a key of %d steps; remove it "+
			"and the next box builds the key again", c.cut(key), of)
	}
	return cut, true, nil
}

// writeCut records cut for key, whole or not at all (a rename).
func (c *Cache) writeCut(key Key, cut Escape) error {
	raw, err := json.Marshal(cut)
	if err != nil {
		return fmt.Errorf("homecache: encoding the cut of %s: %w", key, err)
	}
	tmp, err := os.CreateTemp(c.cuts(), ".cut-*")
	if err != nil {
		return fmt.Errorf("homecache: recording the cut of %s: %w", key, err)
	}
	_, werr := tmp.Write(raw)
	cerr := tmp.Close()
	if err = errors.Join(werr, cerr); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("homecache: recording the cut of %s: %w", key, err)
	}
	if err = os.Rename(tmp.Name(), c.cut(key)); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("homecache: recording the cut of %s: %w", key, err)
	}
	return nil
}

// lock takes key's lock, waiting while another taker holds it, and hands
// back its release.
func (c *Cache) lock(ctx context.Context, key Key) (func(), error) {
	path := filepath.Join(c.locks(), string(key))
	signalled := false
	for {
		lock, err := flock.Try(path)
		if err == nil {
			return lock.Release, nil
		}
		if !errors.Is(err, flock.ErrHeld) {
			return nil, fmt.Errorf("homecache: %w", err)
		}
		if !signalled && c.waiting != nil {
			signalled = true
			c.waiting(key)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("homecache: waiting for the lock of %s: %w", key, ctx.Err())
		case <-time.After(lockPoll):
		}
	}
}

// Collect removes every tree and cut no key of keep names, and every shared
// file no tree still holds. A key whose lock is held (built or copied right
// now) is left for the next collection. It hands back the keys it removed.
func (c *Cache) Collect(keep []Input) ([]Key, error) {
	wanted := map[Key]bool{}
	for _, in := range keep {
		for prefix := CachedPrefix(in.Steps, in.Withheld); prefix > 0; {
			key := KeyOf(in, prefix)
			wanted[key] = true
			cut, ok, err := c.readCut(key, prefix)
			if err != nil || !ok {
				break // an unreadable cut is the next Provision's to report
			}
			prefix = cut.Step
		}
	}
	held := map[Key]bool{}
	for _, dir := range []string{c.trees(), c.cuts()} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("homecache: listing %s: %w", dir, err)
		}
		for _, e := range entries {
			key := Key(e.Name())
			if dir == c.cuts() {
				if filepath.Ext(e.Name()) != ".json" {
					continue // a cut being written
				}
				key = Key(e.Name()[:len(e.Name())-len(".json")])
			}
			if !wanted[key] {
				held[key] = true
			}
		}
	}
	var removed []Key
	for _, key := range slices.Sorted(maps.Keys(held)) {
		lock, err := flock.Try(filepath.Join(c.locks(), string(key)))
		if err != nil {
			continue // held: built or copied right now
		}
		rerr := errors.Join(removeTree(c.tree(key)), removeMissingOK(c.cut(key)))
		lock.Release()
		if rerr != nil {
			return removed, fmt.Errorf("homecache: removing %s: %w", key, rerr)
		}
		removed = append(removed, key)
	}
	return removed, c.sweepObjects()
}

func removeMissingOK(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
