package cache

import "errors"

var (
	// ErrOptions is an Options value Open refuses — a Layout already opened
	// or released among them — or a directory Prepare cannot take: wrapped
	// with the field or the setting to change.
	ErrOptions = errors.New("cache: refused options")

	// ErrNotACache is a directory that is not a crewlet git cache: Prepare's
	// refusal of a non-empty directory without the cache's CACHEDIR.TAG, and
	// Open's of a Layout Prepare did not return.
	ErrNotACache = errors.New("cache: not a git cache directory")

	// ErrCacheInUse is a cache directory another process holds — or a
	// Layout or a Cache of this one, until it is released or stopped: two
	// nodes pointed at one git.cache_dir.
	ErrCacheInUse = errors.New("cache: another process holds this cache directory")
)
