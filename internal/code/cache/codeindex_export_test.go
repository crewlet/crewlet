package cache

import (
	"context"
	"path/filepath"
	"time"
)

// CodeIndexFiles is the code index repoID's current copy keeps: the tip its
// record was read at and the path of every file it names, the tip map first —
// false where the copy keeps none.
func CodeIndexFiles(c *Cache, repoID string) (tip string, paths []string, ok bool) {
	c.mu.Lock()
	e := c.entries[repoID]
	c.mu.Unlock()
	if e == nil {
		return "", nil, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current == nil {
		return "", nil, false
	}
	rec := e.current.codeIndexRecord()
	if rec == nil {
		return "", nil, false
	}
	for _, rel := range rec.rels() {
		paths = append(paths, filepath.Join(e.current.dir, rel))
	}
	return rec.Tip, paths, true
}

// CodeIndexBytes is the node's running total of what its code indexes hold.
func CodeIndexBytes(c *Cache) int64 { return c.codeIndexBytes.Load() }

// IndexPass runs one of the indexer's full looks, synchronously, as work of
// the cache's own.
func IndexPass(ctx context.Context, c *Cache) {
	if !c.enter() {
		return
	}
	defer c.leave()
	c.indexPass(ctx)
}

// TrimCodeIndexes runs the janitor's trim of the code indexes alone, and
// answers whether it dropped any.
func TrimCodeIndexes(ctx context.Context, c *Cache) bool {
	if !c.enter() {
		return false
	}
	defer c.leave()
	return c.trimCodeIndexes(ctx)
}

// IndexReadingWith makes the cache Open builds tell fn how many blobs each
// build of a code index reads through its cat-file (knobs.indexReading).
func IndexReadingWith(fn func(blobs int)) func(*Options) {
	return func(o *Options) {
		if o.knobs == nil {
			o.knobs = &knobs{}
		}
		o.knobs.indexReading = fn
	}
}

// IndexSegmentBytesWith makes each build of a code index by the cache Open
// builds write a segment once it has read n bytes of source, in place of
// codeindex.SegmentSourceBytes (knobs.indexSegmentBytes).
func IndexSegmentBytesWith(n int64) func(*Options) {
	return func(o *Options) {
		if o.knobs == nil {
			o.knobs = &knobs{}
		}
		o.knobs.indexSegmentBytes = n
	}
}

// IndexTicksWith makes the indexer of the cache Open builds look at every
// repository on ticks rather than every IndexPoll (knobs.indexTicks).
func IndexTicksWith(ticks <-chan time.Time) func(*Options) {
	return func(o *Options) {
		if o.knobs == nil {
			o.knobs = &knobs{}
		}
		o.knobs.indexTicks = ticks
	}
}
