package homecache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
// (its work tree, its temporary files) and is never kept.
func Open(root string, exclude ...string) (*Cache, error) {
	c := &Cache{root: root, exclude: slices.Clone(exclude)}
	for _, dir := range []string{c.trees(), c.cuts(), c.objects(), c.locks(), c.owners()} {
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
func (c *Cache) owners() string  { return filepath.Join(c.root, "owners") }

func (c *Cache) tree(key Key) string { return filepath.Join(c.trees(), string(key)) }
func (c *Cache) cut(key Key) string  { return filepath.Join(c.cuts(), string(key)+".json") }

// owner is the record of owner's keys, under a digest of its name, which a
// seat's handle need not be safe to name a file with.
func (c *Cache) owner(owner string) string {
	sum := sha256.Sum256([]byte(owner))
	return filepath.Join(c.owners(), hex.EncodeToString(sum[:16])+".json")
}

// Builder runs steps in a box made for the build and hands back its home. It
// is handed no credential (the package doc), and its home must be on the
// cache's filesystem, since the cache takes it over by renaming it.
type Builder func(ctx context.Context, steps []Step) (Built, error)

// Built is what a [Builder] left.
type Built struct {
	// Home is the build box's home, the cache's from now on (also when
	// Escaped is set, when the cache removes it).
	Home string
	// Escaped is the first step whose effects left the home, if one did:
	// its Step, Name and Paths.
	Escaped *Cut
}

// Cut is where a key's cached prefix ends short of the steps it was built
// from, and why: kept with the key, so no later box tries it again.
type Cut struct {
	// Step is the index, in the steps the key was built from, of the first
	// step that runs in each box instead.
	Step int `json:"step"`
	// Name is that step's name, when a step's effects left the home.
	Name string `json:"name,omitempty"`
	// Paths is what that step changed outside the home.
	Paths []string `json:"paths,omitempty"`
	// Secret is where, relative to the home, the build left a value the
	// caller named as a credential, never the value: the tree was discarded
	// and nothing of the key is cached (Step is 0).
	Secret string `json:"secret,omitempty"`
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
	// Cuts is each cut this call made, for the caller to say so; a later
	// call finds them and says nothing.
	Cuts []Cut
}

// Provision fills dst, a new box's home, from the cache: the tree of in's
// cached prefix, built by build first if this node has none, recorded as
// in.Owner's, whose trees of other keys it removes. A zero [Result.Prefix]
// copies nothing, and the caller runs every step.
func (c *Cache) Provision(ctx context.Context, in Input, build Builder, dst string) (Result, error) {
	var res Result
	var chain []Key
	for prefix := CachedPrefix(in.Steps, in.Withheld, in.Secrets); prefix > 0; {
		key := KeyOf(in, prefix)
		chain = append(chain, key)
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
		cut, built, err := c.ensure(ctx, key, in.Steps[:prefix], in.Secrets, build)
		if err == nil && cut == nil {
			// Recorded under the lock, so no collection takes the tree
			// between its build and its owner's record.
			err = c.record(in.Owner, chain)
			if err == nil && c.copying != nil {
				c.copying(key)
			}
			if err == nil {
				err = copyTree(c.tree(key), dst)
			}
		}
		release()
		switch {
		case err != nil:
			return res, err
		case cut != nil:
			if built {
				res.Cuts = append(res.Cuts, *cut)
			}
			prefix = cut.Step
			continue
		}
		res.Prefix, res.Key, res.Built = prefix, key, built
		return res, nil
	}
	return res, c.record(in.Owner, chain)
}

// ensure makes key's tree exist, holding key's lock: there already, built now,
// or cut (where, and whether this call cut it).
func (c *Cache) ensure(ctx context.Context, key Key, steps []Step, secrets []string, build Builder) (*Cut, bool, error) {
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
	cut := b.Escaped
	switch {
	case cut != nil:
		_ = removeTree(b.Home)
		if cut.Step < 0 || cut.Step >= len(steps) {
			return nil, false, fmt.Errorf("homecache: the build named step %d as leaving the home, "+
				"of %d steps", cut.Step, len(steps))
		}
		cut = &Cut{Step: cut.Step, Name: cut.Name, Paths: cut.Paths}
	default:
		var secret *secretAt
		if err = c.publish(b.Home, key, secrets); errors.As(err, &secret) {
			// An engine bug, or a credential that is also ordinary text:
			// either way not kept, and every step runs in each box.
			cut = &Cut{Step: 0, Secret: secret.path}
		}
		if err != nil {
			_ = removeTree(b.Home)
		}
		if cut == nil {
			return nil, true, err
		}
	}
	if err = c.writeCut(key, *cut); err != nil {
		return nil, false, err
	}
	return cut, true, nil
}

// readCut is key's cut, if it has one. of is how many steps key holds: a cut
// at or past it cannot have come from building key, and is refused.
func (c *Cache) readCut(key Key, of int) (Cut, bool, error) {
	raw, err := os.ReadFile(c.cut(key))
	if errors.Is(err, fs.ErrNotExist) {
		return Cut{}, false, nil
	} else if err != nil {
		return Cut{}, false, fmt.Errorf("homecache: reading %s: %w", c.cut(key), err)
	}
	var cut Cut
	if err = json.Unmarshal(raw, &cut); err != nil || cut.Step < 0 || cut.Step >= of {
		return Cut{}, false, fmt.Errorf("homecache: %s is no cut of a key of %d steps; remove it "+
			"and the next box builds the key again", c.cut(key), of)
	}
	return cut, true, nil
}

func (c *Cache) writeCut(key Key, cut Cut) error {
	return c.writeJSON(c.cuts(), c.cut(key), cut)
}

// ownerRecord is the keys one owner's last box was provisioned through: each
// cut key on the way, then the tree copied.
type ownerRecord struct {
	Owner string `json:"owner"`
	Keys  []Key  `json:"keys"`
}

// record makes keys owner's, and removes the keys owner held before and holds
// no more: its trees of an older setup or image. A key whose lock is held is
// left for [Cache.Collect].
func (c *Cache) record(owner string, keys []Key) error {
	before, err := c.readOwner(c.owner(owner))
	if err != nil {
		return err
	}
	if err = c.writeJSON(c.owners(), c.owner(owner), ownerRecord{Owner: owner, Keys: keys}); err != nil {
		return err
	}
	for _, key := range before.Keys {
		if !slices.Contains(keys, key) {
			if _, err = c.remove(key); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Cache) readOwner(path string) (ownerRecord, error) {
	var rec ownerRecord
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return rec, nil
	} else if err != nil {
		return rec, fmt.Errorf("homecache: reading %s: %w", path, err)
	}
	if err = json.Unmarshal(raw, &rec); err != nil {
		return rec, fmt.Errorf("homecache: %s is no owner's record; remove it and the next "+
			"collection removes its keys: %w", path, err)
	}
	return rec, nil
}

// writeJSON writes v to path, whole or not at all (a rename from dir).
func (c *Cache) writeJSON(dir, path string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("homecache: encoding %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(dir, ".write-*")
	if err != nil {
		return fmt.Errorf("homecache: writing %s: %w", path, err)
	}
	_, werr := tmp.Write(raw)
	cerr := tmp.Close()
	if err = errors.Join(werr, cerr); err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("homecache: writing %s: %w", path, err)
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

// remove removes key's tree and cut unless its lock is held (built or copied
// right now), and says whether it did.
func (c *Cache) remove(key Key) (bool, error) {
	lock, err := flock.Try(filepath.Join(c.locks(), string(key)))
	if errors.Is(err, flock.ErrHeld) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("homecache: %w", err)
	}
	defer lock.Release()
	if err = errors.Join(removeTree(c.tree(key)), removeMissingOK(c.cut(key))); err != nil {
		return false, fmt.Errorf("homecache: removing %s: %w", key, err)
	}
	return true, nil
}

// Collect removes every tree and cut no owner of keep holds (an owner gone,
// or a build no record names), and every shared file no tree still holds. A
// key whose lock is held is left for the next collection. It hands back the
// keys it removed.
func (c *Cache) Collect(keep []string) ([]Key, error) {
	held := map[Key]bool{}
	records, err := os.ReadDir(c.owners())
	if err != nil {
		return nil, fmt.Errorf("homecache: listing %s: %w", c.owners(), err)
	}
	for _, e := range records {
		if filepath.Ext(e.Name()) != ".json" {
			continue // a record being written
		}
		path := filepath.Join(c.owners(), e.Name())
		rec, rerr := c.readOwner(path)
		switch {
		case rerr != nil:
			return nil, rerr
		case slices.Contains(keep, rec.Owner):
			for _, key := range rec.Keys {
				held[key] = true
			}
		default:
			if rerr = removeMissingOK(path); rerr != nil {
				return nil, fmt.Errorf("homecache: removing %s: %w", path, rerr)
			}
		}
	}
	unheld := map[Key]bool{}
	for _, dir := range []string{c.trees(), c.cuts()} {
		entries, lerr := os.ReadDir(dir)
		if lerr != nil {
			return nil, fmt.Errorf("homecache: listing %s: %w", dir, lerr)
		}
		for _, e := range entries {
			name, isCut := strings.CutSuffix(e.Name(), ".json")
			if dir == c.cuts() && !isCut {
				continue // a cut being written
			}
			if key := Key(name); !held[key] {
				unheld[key] = true
			}
		}
	}
	var removed []Key
	for _, key := range slices.Sorted(maps.Keys(unheld)) {
		done, rerr := c.remove(key)
		if rerr != nil {
			return removed, rerr
		}
		if done {
			removed = append(removed, key)
		}
	}
	return removed, c.sweepObjects()
}

func removeMissingOK(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
