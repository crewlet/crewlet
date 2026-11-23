package memory

import (
	"bytes"
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/coord"
)

// The atlas buckets in the twin, each value held to the bound of the class
// the KV backend declares for its key (internal/coord/kv's atlas holdings),
// so the twin never certifies a write the store refuses.

// ---- the atlas sources' standings --------------------------------------- //

// AtlasSourceStatuses returns every recorded standing, keyed by source, each
// cloned on the way out as the integration statuses are.
func (f *Fleet) AtlasSourceStatuses(context.Context) (map[string][]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string][]byte, len(f.atlasSources))
	for kind, value := range f.atlasSources {
		out[kind] = bytes.Clone(value)
	}
	return out, nil
}

// PutAtlasSourceStatus records one source's standing.
func (f *Fleet) PutAtlasSourceStatus(_ context.Context, kind string, value []byte) error {
	if kind == "" {
		return errors.New("coord/memory: an atlas source standing needs a source name")
	}
	if err := coord.HoldValue("the atlas source standing of "+kind, len(value),
		coord.MaxAtlasSourceStandingBytes); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.atlasSources[kind] = bytes.Clone(value)
	return nil
}

// AtlasSourceMisses reads one source's misses.
func (f *Fleet) AtlasSourceMisses(_ context.Context, kind string) ([]byte, bool, error) {
	if kind == "" {
		return nil, false, errors.New("coord/memory: an atlas source's misses are read per source")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	value, ok := f.atlasMisses[kind]
	return bytes.Clone(value), ok, nil
}

// PutAtlasSourceMisses records one source's misses.
func (f *Fleet) PutAtlasSourceMisses(_ context.Context, kind string, value []byte) error {
	if kind == "" {
		return errors.New("coord/memory: an atlas source's misses need a source name")
	}
	if err := coord.HoldValue("the atlas source misses of "+kind, len(value),
		coord.MaxAtlasSourceMissesBytes); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.atlasMisses[kind] = bytes.Clone(value)
	return nil
}

// DeleteAtlasSourceStatus drops a source's standing and its misses; what is
// not there is the outcome asked for.
func (f *Fleet) DeleteAtlasSourceStatus(_ context.Context, kind string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.atlasMisses, kind)
	delete(f.atlasSources, kind)
	return nil
}

// ---- the atlas sources' refresh asks ------------------------------------ //

// AtlasSourceRefreshes returns every waiting ask, keyed by source, each
// cloned on the way out.
func (f *Fleet) AtlasSourceRefreshes(context.Context) (map[string][]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string][]byte, len(f.atlasRefresh))
	for kind, value := range f.atlasRefresh {
		out[kind] = bytes.Clone(value)
	}
	return out, nil
}

// PutAtlasSourceRefresh records an ask for one source.
func (f *Fleet) PutAtlasSourceRefresh(_ context.Context, kind string, value []byte) error {
	if kind == "" {
		return errors.New("coord/memory: an atlas source refresh ask needs a source name")
	}
	if err := coord.HoldValue("the atlas source refresh ask for "+kind, len(value), coord.SmallValueBytes); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.atlasRefresh[kind] = bytes.Clone(value)
	return nil
}

// DeleteAtlasSourceRefresh drops a source's ask; one that is not there is
// the outcome asked for.
func (f *Fleet) DeleteAtlasSourceRefresh(_ context.Context, kind string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.atlasRefresh, kind)
	return nil
}

// ---- the atlas code scans' standings ------------------------------------ //

// AtlasCodeScans returns the standing of every repository of host, keyed by
// the repository, each cloned on the way out.
func (f *Fleet) AtlasCodeScans(_ context.Context, host string) (map[string][]byte, error) {
	if host == "" {
		return nil, errors.New("coord/memory: the atlas code scan standings are listed per host")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string][]byte{}
	for key, value := range f.atlasCode {
		if repo, ok := coord.AtlasCodeRepoOf(key, host); ok {
			out[repo] = bytes.Clone(value)
		}
	}
	return out, nil
}

// AtlasCodeScan reads one standing.
func (f *Fleet) AtlasCodeScan(_ context.Context, host, repo string) ([]byte, bool, error) {
	if host == "" {
		return nil, false, errors.New("coord/memory: an atlas code scan standing names its host")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	value, ok := f.atlasCode[coord.AtlasCodeScanKey(host, repo)]
	return bytes.Clone(value), ok, nil
}

// PutAtlasCodeScan records one standing, held to coord.SmallValueBytes as
// the KV backend holds it.
func (f *Fleet) PutAtlasCodeScan(_ context.Context, host, repo string, value []byte) error {
	if host == "" {
		return errors.New("coord/memory: an atlas code scan standing names its host")
	}
	key := coord.AtlasCodeScanKey(host, repo)
	if err := coord.HoldValue("the atlas code scan standing "+key, len(value), coord.SmallValueBytes); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.atlasCode[key] = bytes.Clone(value)
	return nil
}

// DeleteAtlasCodeScan drops one standing; one that is not there is the
// outcome asked for.
func (f *Fleet) DeleteAtlasCodeScan(_ context.Context, host, repo string) error {
	if host == "" {
		return errors.New("coord/memory: an atlas code scan standing names its host")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.atlasCode, coord.AtlasCodeScanKey(host, repo))
	return nil
}
