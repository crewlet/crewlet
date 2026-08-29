package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	turso_libs "github.com/tursodatabase/turso-go-platform-libs"
)

// Turso's native library, and why this file exists.
//
// The Turso driver is pure Go in the sense that matters — no cgo, no C
// toolchain — but it is not self-contained: the engine it drives ships as a
// native library EMBEDDED in the driver's own module, extracted on first use
// into a per-user cache and loaded from there with purego. That extraction is
// process-shared state populated lazily, and it is populated UNSAFELY:
// upstream writes straight into the final path rather than to a temporary file
// it renames. Two processes starting at once is all it takes — one creates the
// file and begins copying twenty megabytes into it, the other sees a non-empty
// file, hashes what is there so far, and gets a mismatch.
//
// That mismatch is not a degraded mode. `tursogo.InitLibrary` PANICS on it,
// inside a sync.Once — which marks itself done even when the function it ran
// panicked, so the failure is not retryable within the process either. The
// engine dies on its first query, and the corrupt file it read stays on disk
// making every later start fail the same way, until somebody deletes a cache
// entry they have never heard of.
//
// So the preparation happens HERE, before anything opens a connection, where
// it can be serialised across processes and where the underlying loader —
// which is exported, and returns an error rather than panicking — can be
// called directly.

// tursoCacheDirName is upstream's subdirectory under the user cache root. It
// is not exported by the library, so it is mirrored here; getting it wrong
// costs a heal that clears nothing, never a wrong file.
const tursoCacheDirName = "turso-go"

// tursoCacheEnv overrides the cache root, for upstream and for this file
// alike. Read rather than set: an operator who has pointed the cache at a
// shared volume must not find the engine preparing a different one.
const tursoCacheEnv = "TURSO_GO_CACHE_DIR"

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

// tursoLockStale is how long a lock directory may stand before it is treated
// as abandoned.
//
// A lock is held only across the extraction above, so anything older than this
// belongs to a process that was killed mid-copy. Reclaiming it too eagerly
// costs a concurrent extraction — which is the behaviour without any lock at
// all — while never reclaiming it would wedge every engine on the host for
// good on the first SIGKILL.
const tursoLockStale = 2 * time.Minute

// tursoLockPoll is how often a waiting process retries the lock. Short enough
// that a warm cache adds nothing measurable to startup, long enough not to spin
// on the filesystem for a wait that is normally sub-second.
const tursoLockPoll = 25 * time.Millisecond

// prepareTursoLibrary readies the native library exactly once per process.
//
// The result is memoised INCLUDING a failure: a second attempt would take the
// same corrupt cache to the same place, and the caller needs the reason, not a
// retry.
var prepareTursoLibrary = sync.OnceValue(prepareTursoLibraryNow)

// tursoCacheRoot is where upstream extracts, derived the same way it does.
func tursoCacheRoot() string {
	if override := os.Getenv(tursoCacheEnv); override != "" {
		return override
	}
	if dir, err := os.UserCacheDir(); err == nil {
		return dir
	}
	return os.TempDir()
}

// prepareTursoLibraryNow extracts and verifies the library under a
// cross-process lock, healing a cache a previous run left half-written.
//
// It takes no root: the loader reads the location from the ENVIRONMENT and
// offers no other way to direct it, so a parameter here could only ever name a
// directory this function locked and healed while upstream extracted somewhere
// else. Point [tursoCacheEnv] at a different cache and both halves move
// together — which is also how this is tested.
func prepareTursoLibraryNow() error {
	dir := filepath.Join(tursoCacheRoot(), tursoCacheDirName)
	// 0755, matching what upstream creates: a cache both processes write is
	// one whose mode they must agree on.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("store: preparing the Turso library cache at %s: %w", dir, err)
	}
	// A lock that cannot be taken — a wait that timed out, a cache directory
	// this process cannot write — is not a reason to refuse: the preparation
	// then runs unguarded, which is exactly the behaviour before this file
	// existed and strictly better than an engine that will not start.
	if release, held := acquireTursoLock(dir, time.Now().Add(tursoPrepareTimeout)); held {
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
			dir, tursoCacheEnv, libcAdvice(), err)
	}
	return nil
}

// libcAdvice names the C library mismatch, when there is evidence of one.
//
// # Why this sentence exists
//
// The engine is a static pure-Go binary, so an operator reasonably assumes it
// runs anywhere linux runs — and it does, right up until the store opens. The
// database engine is a native shared object, and the driver embeds a GLIBC
// build and a MUSL build behind a build tag. Extract the glibc one on Alpine
// and every step before the last one SUCCEEDS: the file is written, its
// sha256 matches, and then dlopen fails because the loader is not there.
//
// The message that failure lands in is about a cache that will not verify —
// which is exactly the wrong thing to tell someone whose cache is perfect. It
// sends them to delete a directory, watch it be rebuilt identically, and
// delete it again. The archive they need is `_musl` (or `_linux` if they built
// with the tag and deployed on glibc), and nothing anywhere would have said so.
//
// Appended rather than substituted, and phrased as "looks like": the evidence
// is one glob (see runningOnMusl), a host can carry both C libraries, and the
// underlying error is still the thing to read. A hint that is occasionally
// unnecessary is much cheaper than the absence of the only hint that helps.
func libcAdvice() string {
	if runningOnMusl() == builtForMusl {
		return ""
	}
	if builtForMusl {
		return "\n\nThis binary was built with -tags musl, for a musl C library, and this " +
			"host does not look like a musl system. Use the plain linux archive " +
			"(crewlet_<version>_linux_<arch>.tar.gz) instead."
	}
	return "\n\nThis host looks like a musl system (Alpine and friends) and this binary " +
		"carries the glibc build of the database engine, which will not load there. " +
		"Use the musl archive (crewlet_<version>_linux_<arch>_musl.tar.gz), or run " +
		"the published container image, which is glibc."
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

// acquireTursoLock takes a cross-process lock on the cache directory.
//
// A DIRECTORY, because mkdir is the one exclusive-create primitive every
// filesystem and every operating system this engine ships for agrees on —
// unlike flock, which needs a build-tagged Windows twin, and unlike an
// O_EXCL file, which is the same idea with an extra step. It is held only
// across an extraction.
//
// The second return says whether the lock is HELD, and a false there is not an
// error: a wait that timed out, or a cache directory this process cannot write,
// both end with the preparation running unguarded — which is exactly the
// behaviour before this file existed, and strictly better than refusing to
// start.
func acquireTursoLock(dir string, deadline time.Time) (release func(), held bool) {
	lock := filepath.Join(dir, ".crewlet-extract.lock")
	for {
		err := os.Mkdir(lock, 0o755)
		if err == nil {
			return func() {
				if removeErr := os.Remove(lock); removeErr != nil {
					log.Warn("turso_library_lock_not_released",
						"lock", lock, "error", removeErr.Error())
				}
			}, true
		}
		if !errors.Is(err, os.ErrExist) {
			// The path cannot hold a lock at all — there is no peer here
			// to wait for, so waiting would only burn the timeout.
			log.Debug("turso_library_lock_unavailable", "lock", lock, "error", err.Error())
			return nil, false
		}
		switch info, statErr := os.Stat(lock); {
		case statErr != nil:
			// It existed a moment ago and does not now: the holder
			// released it between the mkdir and the stat.
		case !info.IsDir():
			// NOT A LOCK AT ALL. Only this code creates this path and it
			// only ever creates a directory, so a file here is debris —
			// and waiting for it would stall every engine on the host for
			// the whole timeout, once, and then again on the next start.
			log.Warn("turso_library_lock_debris_cleared", "lock", lock)
			_ = os.Remove(lock)
		case time.Since(info.ModTime()) > tursoLockStale:
			// A lock older than the work it guards belongs to a process
			// that was killed holding it.
			log.Warn("turso_library_lock_reclaimed", "lock", lock,
				"age", time.Since(info.ModTime()).String())
			_ = os.Remove(lock)
		}
		// EVERY path through the loop passes here, deliberately: a retry
		// that skipped the deadline could spin against a lock that keeps
		// changing under it, and one that skipped the sleep would spin hot
		// for the whole minute rather than waiting.
		time.Sleep(tursoLockPoll)
		if time.Now().After(deadline) {
			log.Warn("turso_library_lock_wait_timed_out", "lock", lock)
			return nil, false
		}
	}
}
