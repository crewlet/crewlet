package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	turso_libs "github.com/tursodatabase/turso-go-platform-libs"

	"github.com/crewlet/crewlet/internal/nativelib"
)

// Turso's native library, and why this file exists.
//
// The engine ships as a native library EMBEDDED in the driver's module,
// extracted on first use into a per-user cache and loaded with purego.
// Upstream extracts UNSAFELY, straight into the final path: a second process
// sees a partial file and hashes a mismatch, and `tursogo.InitLibrary` PANICS
// inside a sync.Once (done even after a panic, so not retryable) while the
// corrupt file fails every later start. So preparation happens HERE, before
// any connection, serialised across processes, through the exported loader,
// which returns an error instead of panicking.

// tursoCacheDirName is upstream's subdirectory under the user cache root. It
// is not exported by the library, so it is mirrored here; getting it wrong
// costs a heal that clears nothing, never a wrong file.
const tursoCacheDirName = "turso-go"

// tursoPrepareTimeout bounds the whole preparation: waiting for a peer that is
// already extracting, plus this process's own extraction and verification.
//
// Sized against the work, not guessed. The library is ~20 MB; extracting and
// hashing it is well under a second on any disk the engine would run on, and
// the wait only ever queues behind one peer doing exactly that. A minute is
// far past the slowest plausible cold start and still bounded, because the
// alternative to giving up is an engine that never starts at all — and giving
// up here degrades to the old behaviour rather than refusing to run.
const tursoPrepareTimeout = time.Minute

// prepareTursoLibrary readies the native library exactly once per process.
//
// The result is memoised INCLUDING a failure: a second attempt would take the
// same corrupt cache to the same place, and the caller needs the reason, not a
// retry.
var prepareTursoLibrary = sync.OnceValue(prepareTursoLibraryNow)

// prepareTursoLibraryNow extracts and verifies the library under a
// cross-process lock, healing a cache a previous run left half-written.
//
// It takes no root: the loader reads the location from the ENVIRONMENT and
// offers no other way to direct it, so a parameter here could only ever name a
// directory this function locked and healed while upstream extracted somewhere
// else. Point [nativelib.CacheEnv] at a different cache and both halves move
// together — which is also how this is tested.
func prepareTursoLibraryNow() error {
	dir := filepath.Join(nativelib.CacheRoot(), tursoCacheDirName)
	// 0755, matching what upstream creates: a cache both processes write is
	// one whose mode they must agree on.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("store: preparing the Turso library cache at %s: %w", dir, err)
	}
	// A lock that cannot be taken — a wait that timed out, a cache directory
	// this process cannot write — is not a reason to refuse: the preparation
	// then runs unguarded, which is exactly the behaviour before this file
	// existed and strictly better than an engine that will not start.
	if release, held := nativelib.Lock(dir, time.Now().Add(tursoPrepareTimeout)); held {
		defer release()
	}
	return loadOrHealTursoLibrary(dir)
}

// loadOrHealTursoLibrary loads the library, clearing the cache and trying once
// more if it will not verify. The caller holds the lock, if there is one.
func loadOrHealTursoLibrary(dir string) error {
	err := loadTursoLibrary()
	if err == nil {
		return nil
	}
	// ONE HEAL, THEN THE ERROR. A cache written by a process that died
	// mid-copy fails identically forever, and the file is a cache entry
	// with an embedded original — clearing it costs one extraction, while
	// leaving it costs every future start on this host.
	log.Warn("turso_library_cache_healing", "cache", dir, "error", err.Error())
	clearTursoCache(dir)
	if err := loadTursoLibrary(); err != nil {
		return fmt.Errorf("store: the Turso native library could not be prepared. Its cache "+
			"at %s was cleared and re-extracted and still does not verify or will not "+
			"load — delete that directory by hand, or point %s at a writable directory "+
			"of its own and start again. There is no second driver to fall back to.%s: %w",
			dir, nativelib.CacheEnv, libcAdvice(), err)
	}
	return nil
}

// libcAdvice names the C library requirement, when there is evidence it is
// the thing in the way.
//
// On linux the binary is NOT static despite CGO_ENABLED=0: purego's
// //go:cgo_import_dynamic imports make a DYNAMIC executable needing glibc.
// Plain musl fails at execve and never reaches here; this covers a musl host
// with a glibc shim (gcompat), where the shared object will not load and the
// error otherwise reads as "the cache will not verify". Appended, not
// substituted, and phrased "looks like": the evidence is one glob
// (runningOnMusl) and the underlying error is still the thing to read.
func libcAdvice() string {
	if !runningOnMusl() {
		return ""
	}
	return "\n\nThis host looks like a musl system (Alpine and friends). The linux " +
		"binaries are dynamically linked against GLIBC — the database engine is " +
		"loaded with dlopen, which is not something a pure-Go build avoids — so " +
		"the shared object will not load here even under a glibc shim. Run the " +
		"published container image, or use a glibc base image."
}

// loadTursoLibrary extracts-if-absent, verifies and loads.
//
// Called through the loader rather than the driver on purpose: this is the
// same work `tursogo.InitLibrary` does on first connect, minus the panic. Once
// it has succeeded, the driver's own call finds a complete cache and cannot
// take the process down.
//
// It does cost the work twice — a stat, a sha256 over ~20 MB and a dlopen,
// tens of milliseconds, once per process at the first Open. There is no way to
// hand the driver a result it has already been given, and buying a legible
// error instead of a panic that cannot be recovered from is worth it.
func loadTursoLibrary() error {
	_, err := turso_libs.LoadTursoLibrary(turso_libs.LoadTursoLibraryConfig{
		LoadStrategy: turso_libs.EmbeddedLibraryLoadStrategy,
	})
	return err
}

// clearTursoCache removes the extracted libraries, leaving the directory.
//
// Everything under here is upstream's own extraction of a file it still
// embeds, so nothing is lost that cannot be rebuilt.
//
// UNLINKED RATHER THAN TRUNCATED, and that is the whole reason this is a
// removal and not an overwrite: another process may have this very file
// dlopen'd, and shortening a mapping's backing file SIGBUSes whoever holds it.
// An unlinked file keeps serving its existing mappings and the re-extraction
// lands on a fresh inode, so a peer mid-run is never touched. On Windows a
// mapped file refuses to be unlinked at all — logged, and not worth failing a
// heal for.
func clearTursoCache(dir string) {
	matches, err := filepath.Glob(filepath.Join(dir, "*", "*"))
	if err != nil {
		return
	}
	for _, path := range matches {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Warn("turso_library_cache_entry_not_cleared", "path", path, "error", err.Error())
		}
	}
}
