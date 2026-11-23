package kv

import (
	"bytes"
	"context"
	"errors"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// The atlas buckets: each source's standing and misses, the asks that a
// source be read now, and the code scans' standings, each key held to its
// class where its bucket opens (settle.go's atlas holdings).

// ---- the atlas sources' standings --------------------------------------- //

// AtlasSourceStatuses returns every recorded standing, keyed by source: a
// pass over the standings' class alone, so it never waits on a source's
// misses crossing.
func (f *FleetStore) AtlasSourceStatuses(ctx context.Context) (map[string][]byte, error) {
	out := map[string][]byte{}
	err := f.eachUnder(ctx, f.atlasSources, coord.DocumentFilter(coord.AtlasSourceStandingClass),
		"the atlas source standings", func(kve jetstream.KeyValueEntry) error {
			kind, ok := coord.AtlasSourceOf(kve.Key(), coord.AtlasSourceStandingClass)
			if !ok {
				// A key this backend did not write: a guessed name would
				// put a source nothing reads on an operator's screen.
				return nil
			}
			// COPIED: the entry's buffer belongs to the client.
			out[kind] = bytes.Clone(kve.Value())
			return nil
		})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PutAtlasSourceStatus records one source's standing.
func (f *FleetStore) PutAtlasSourceStatus(ctx context.Context, kind string, value []byte) error {
	if kind == "" {
		return errors.New("coord/kv: an atlas source standing needs a source name")
	}
	if _, err := f.atlasSources.Put(ctx, coord.AtlasSourceStandingKey(kind), value); err != nil {
		return unavailable("record the atlas source standing", err)
	}
	return nil
}

// AtlasSourceMisses reads one source's misses by its key.
func (f *FleetStore) AtlasSourceMisses(ctx context.Context, kind string) ([]byte, bool, error) {
	if kind == "" {
		return nil, false, errors.New("coord/kv: an atlas source's misses are read per source")
	}
	entry, err := f.get(ctx, f.atlasSources, coord.AtlasSourceMissesKey(kind))
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return nil, false, nil
	case err != nil:
		return nil, false, unavailable("read the "+kind+" source's misses", err)
	}
	return bytes.Clone(entry.Value()), true, nil
}

// PutAtlasSourceMisses records one source's misses.
func (f *FleetStore) PutAtlasSourceMisses(ctx context.Context, kind string, value []byte) error {
	if kind == "" {
		return errors.New("coord/kv: an atlas source's misses need a source name")
	}
	if _, err := f.atlasSources.Put(ctx, coord.AtlasSourceMissesKey(kind), value); err != nil {
		return unavailable("record the "+kind+" source's misses", err)
	}
	return nil
}

// DeleteAtlasSourceStatus drops a source's misses, then its standing: PURGED,
// as every removal from an ageless bucket is (markers.go).
func (f *FleetStore) DeleteAtlasSourceStatus(ctx context.Context, kind string) error {
	for _, key := range []string{coord.AtlasSourceMissesKey(kind), coord.AtlasSourceStandingKey(kind)} {
		if err := f.atlasSources.Purge(ctx, key); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
			return unavailable("delete the atlas source standing", err)
		}
	}
	return nil
}

// ---- the atlas sources' refresh asks ------------------------------------ //

// AtlasSourceRefreshes returns every waiting ask, keyed by source.
func (f *FleetStore) AtlasSourceRefreshes(ctx context.Context) (map[string][]byte, error) {
	out := map[string][]byte{}
	err := f.each(ctx, f.atlasRefresh, func(kve jetstream.KeyValueEntry) error {
		// A key this backend did not write is skipped, and the value is
		// copied, as the atlas sources' are.
		if kind, ok := decodeKey(kve.Key()); ok {
			out[kind] = bytes.Clone(kve.Value())
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PutAtlasSourceRefresh records an ask for one source.
func (f *FleetStore) PutAtlasSourceRefresh(ctx context.Context, kind string, value []byte) error {
	if kind == "" {
		return errors.New("coord/kv: an atlas source refresh ask needs a source name")
	}
	if _, err := f.atlasRefresh.Put(ctx, encodeKey(kind), value); err != nil {
		return unavailable("record the atlas source refresh ask", err)
	}
	return nil
}

// DeleteAtlasSourceRefresh drops a source's ask: PURGED, as every removal
// from an ageless bucket is (markers.go).
func (f *FleetStore) DeleteAtlasSourceRefresh(ctx context.Context, kind string) error {
	err := f.atlasRefresh.Purge(ctx, encodeKey(kind))
	if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return unavailable("delete the atlas source refresh ask", err)
	}
	return nil
}

// ---- the atlas code scans' standings ------------------------------------ //

// AtlasCodeScans returns every recorded standing, keyed by repository.
func (f *FleetStore) AtlasCodeScans(ctx context.Context) (map[string][]byte, error) {
	out := map[string][]byte{}
	err := f.each(ctx, f.atlasCode, func(kve jetstream.KeyValueEntry) error {
		// A key this backend did not write is skipped, and the value is
		// copied, as the atlas sources' are.
		if repo, ok := decodeKey(kve.Key()); ok {
			out[repo] = bytes.Clone(kve.Value())
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PutAtlasCodeScan records one repository's standing.
func (f *FleetStore) PutAtlasCodeScan(ctx context.Context, repo string, value []byte) error {
	if repo == "" {
		return errors.New("coord/kv: an atlas code scan standing needs a repository")
	}
	if _, err := f.atlasCode.Put(ctx, encodeKey(repo), value); err != nil {
		return unavailable("record the atlas code scan standing", err)
	}
	return nil
}

// DeleteAtlasCodeScan drops a repository's standing: PURGED, as every removal
// from an ageless bucket is (markers.go).
func (f *FleetStore) DeleteAtlasCodeScan(ctx context.Context, repo string) error {
	err := f.atlasCode.Purge(ctx, encodeKey(repo))
	if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return unavailable("delete the atlas code scan standing", err)
	}
	return nil
}
