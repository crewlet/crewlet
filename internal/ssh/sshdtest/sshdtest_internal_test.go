package sshdtest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/imagetest"
)

// THE CACHE IS MARKED COMPLETE ONLY ONCE IT KEEPS EVERY IMAGE, which is what
// ci.yml saves the directory on: a shard that kept some of them never saves a
// cache that would build the rest on every later run under the same key. Each
// image is taken as Start takes it, from a stand-in for imagetest.Provide that
// keeps its tar in the cache, as a build and its save would.
// Mutation: mark the cache after one image, never, or take an image without
// marking it, and this fails.
func TestTheCacheIsMarkedCompleteOnlyOnceItKeepsEveryImage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(CacheEnv, dir)
	keep := func(t testing.TB, img imagetest.Image) string {
		t.Helper()
		if err := os.WriteFile(img.Tar(dir), []byte("tar"), 0o600); err != nil {
			t.Fatal(err)
		}
		return img.Tag()
	}
	for i, base := range Images {
		image(t, base, keep)
		_, err := os.Stat(filepath.Join(dir, completeFile))
		if every := i+1 == len(Images); (err == nil) != every {
			t.Fatalf("keeping %d of %d images, the cache is marked complete: %v, want %v",
				i+1, len(Images), err == nil, every)
		}
	}
}
