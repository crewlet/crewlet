// Package nativelib is the engine's native-library cache: the directory a
// native library is extracted into on first use, the cross-process lock every
// extraction takes, and the verified extraction itself.
//
// # One root for every native library
//
// Two native libraries are loaded: Turso's database engine, which its driver
// embeds and extracts itself ([CacheRoot]/turso-go, prepared under [Lock] by
// internal/store), and the speech recognizer, whose official release archive
// internal/speech downloads and extracts with [Extract] and [Commit] into
// [CacheRoot]/sherpa-onnx. Both live under [CacheRoot], so the one variable an
// operator points at a writable volume ([CacheEnv]) moves them together. It is
// upstream Turso's variable, read by its driver as well as here, which is why
// it carries that name.
//
// # The lock is a DIRECTORY
//
// mkdir is the one exclusive-create primitive every filesystem and OS this
// engine ships for agrees on (flock needs a build-tagged twin). It is held only
// across an extraction, so one older than [lockStale] belongs to a process that
// was killed holding it and is reclaimed. A lock that cannot be taken (a wait
// that timed out, a directory this process cannot write) is not an error: the
// extraction then runs unguarded, strictly better than an engine that will not
// start.
//
// [Extract], [Stage] and [Commit] are the one verified pass over a pinned
// download: nothing reaches its final name until its digest matched.
//
// # Files are replaced by rename, never rewritten
//
// Another process may have a cached library mapped: truncating its backing
// file SIGBUSes whoever holds it. [Extract] and [Stage] write each file beside
// its final name and [Commit] renames it into place, so a peer keeps its inode.
package nativelib

import (
	"archive/tar"
	"compress/bzip2"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("nativelib")

// CacheEnv overrides [CacheRoot]. Read rather than set: an operator who has
// pointed the cache at a shared volume must not find the engine preparing a
// different one.
const CacheEnv = "TURSO_GO_CACHE_DIR"

// lockName is the lock directory inside the directory it guards.
const lockName = ".crewlet-extract.lock"

// lockStale is how long a lock directory may stand untouched before it is
// treated as abandoned. Its holder touches it every [lockTouch] for as long as
// it works (a download can take minutes), so only a killed holder's goes
// stale; never reclaiming that would wedge every engine on the host.
const lockStale = 2 * time.Minute

// lockTouch is how often a holder renews its lock: four times inside
// [lockStale], so a slow disk or a paused process does not lose it.
const lockTouch = lockStale / 4

// lockPoll is how often a waiting process retries the lock: a warm cache adds
// nothing measurable to startup, and a wait does not spin on the filesystem.
const lockPoll = 25 * time.Millisecond

// CacheRoot is where native libraries are extracted, derived as upstream
// Turso derives it: [CacheEnv], else the user cache directory, else the
// temporary directory.
func CacheRoot() string {
	if override := os.Getenv(CacheEnv); override != "" {
		return override
	}
	if dir, err := os.UserCacheDir(); err == nil {
		return dir
	}
	return os.TempDir()
}

// Lock takes the cross-process lock on dir, waiting until deadline for a peer
// that holds it. held is false when it was not taken (see the package doc);
// release is then nil.
func Lock(dir string, deadline time.Time) (release func(), held bool) {
	lock := filepath.Join(dir, lockName)
	for {
		err := os.Mkdir(lock, 0o755)
		if err == nil {
			stop := make(chan struct{})
			touched := make(chan struct{})
			go func() {
				defer close(touched)
				tick := time.NewTicker(lockTouch)
				defer tick.Stop()
				for {
					select {
					case <-stop:
						return
					case now := <-tick.C:
						_ = os.Chtimes(lock, now, now)
					}
				}
			}()
			return func() {
				close(stop)
				<-touched
				if removeErr := os.Remove(lock); removeErr != nil {
					log.Warn("native_library_lock_not_released",
						"lock", lock, "error", removeErr.Error())
				}
			}, true
		}
		if !errors.Is(err, os.ErrExist) {
			// No peer can hold a lock here, so waiting would only burn
			// the deadline.
			log.Debug("native_library_lock_unavailable", "lock", lock, "error", err.Error())
			return nil, false
		}
		switch info, statErr := os.Stat(lock); {
		case statErr != nil:
			// Released between the mkdir and the stat.
		case !info.IsDir():
			// Only this code creates the path, always as a directory: a
			// file here is debris, and waiting on it stalls every start.
			log.Warn("native_library_lock_debris_cleared", "lock", lock)
			_ = os.Remove(lock)
		case time.Since(info.ModTime()) > lockStale:
			log.Warn("native_library_lock_reclaimed", "lock", lock,
				"age", time.Since(info.ModTime()).String())
			_ = os.Remove(lock)
		}
		// Every path sleeps and checks the deadline, so no retry spins.
		time.Sleep(lockPoll)
		if time.Now().After(deadline) {
			log.Warn("native_library_lock_wait_timed_out", "lock", lock)
			return nil, false
		}
	}
}

// File is one file a caller loads, pinned by its own digest so a cached copy
// is verified at every use, not only when it was written.
type File struct {
	// Path is the file's path inside its archive, or its name; it is kept
	// under its base name.
	Path string

	// SHA256 is the member's digest, lowercase hex.
	SHA256 string
}

// Verify checks every file in dir, under its base name, against its pin.
func Verify(dir string, files []File) error {
	for _, f := range files {
		p := filepath.Join(dir, path.Base(f.Path))
		got, err := DigestFile(p)
		if err != nil {
			return err
		}
		if got != f.SHA256 {
			return fmt.Errorf("%s has SHA-256 %s, want %s", p, got, f.SHA256)
		}
	}
	return nil
}

// DigestFile is a file's SHA-256, lowercase hex.
func DigestFile(p string) (string, error) {
	file, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", fmt.Errorf("reading %s: %w", p, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Extract reads one tar.bz2 stream, verifying its SHA-256, and stages each
// member want names (cleaned archive path to final path) beside its final
// path. It answers the staged paths, keyed by final path, for [Commit]; on any
// error it removes what it staged. A member want names that the archive lacks
// is an error.
func Extract(src io.Reader, archiveSHA256 string, want map[string]string) (map[string]string, error) {
	h := sha256.New()
	tr := tar.NewReader(bzip2.NewReader(io.TeeReader(src, h)))
	staged := map[string]string{}
	fail := func(err error) (map[string]string, error) {
		for _, tmp := range staged {
			_ = os.Remove(tmp)
		}
		return nil, err
	}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fail(fmt.Errorf("reading the archive: %w", err))
		}
		final, ok := want[path.Clean(hdr.Name)]
		if !ok || hdr.Typeflag != tar.TypeReg {
			continue
		}
		tmp := final + ".tmp-" + strconv.Itoa(os.Getpid())
		if err := writeFile(tmp, tr); err != nil {
			return fail(err)
		}
		staged[final] = tmp
	}
	// The rest of the stream (bzip2's trailer, tar padding) counts too.
	if _, err := io.Copy(io.Discard, io.TeeReader(src, h)); err != nil {
		return fail(fmt.Errorf("reading the archive: %w", err))
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != archiveSHA256 {
		return fail(fmt.Errorf("the archive has SHA-256 %s, want %s", got, archiveSHA256))
	}
	for name, final := range want {
		if _, ok := staged[final]; !ok {
			return fail(fmt.Errorf("the archive holds no %s", name))
		}
	}
	return staged, nil
}

// Stage writes src beside final while hashing it, and answers the staged path
// for [Commit] when its SHA-256 is want; otherwise it removes what it wrote.
func Stage(src io.Reader, final, want string) (string, error) {
	tmp := final + ".tmp-" + strconv.Itoa(os.Getpid())
	h := sha256.New()
	if err := writeFile(tmp, io.TeeReader(src, h)); err != nil {
		return "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("%s has SHA-256 %s, want %s", filepath.Base(final), got, want)
	}
	return tmp, nil
}

// Commit renames each staged file onto its final path (see the package doc).
func Commit(staged map[string]string) error {
	for final, tmp := range staged {
		if err := os.Rename(tmp, final); err != nil {
			for _, rest := range staged {
				_ = os.Remove(rest)
			}
			return fmt.Errorf("moving %s into place: %w", final, err)
		}
	}
	return nil
}

// writeFile writes r to p, executable and readable by all as a shared
// library cache's files are, synced so a rename never publishes a hole.
func writeFile(p string, r io.Reader) error {
	file, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("creating %s: %w", p, err)
	}
	_, copyErr := io.Copy(file, r)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
		_ = os.Remove(p)
		return fmt.Errorf("writing %s: %w", p, err)
	}
	return nil
}
