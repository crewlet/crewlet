// Package hostbox holds the primitives for running somebody else's process in
// a directory on the engine host.
//
// Two subsystems do that, and they must do it identically: the subscription
// CLI LLM backend, which drives a coding CLI per seat with its own HOME, and
// the local sandbox provider, which runs a whole coding agent in a box. Both
// seed operator-supplied credential paths in, both copy a refreshed login back
// out, and both hand a child process an environment. A second implementation
// of any of those is the one that drifts — and each of them is a guard, so
// drift means a hole.
//
// Nothing here imports the rest of Crewlet: it sits below both consumers.
package hostbox

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// DirMode is the mode for every directory these two subsystems create.
// Credentials and prompt transcripts both land under one, on a host that may
// run other services.
const DirMode os.FileMode = 0o700

// FileMode is the mode for every file they create, for the same reason.
const FileMode os.FileMode = 0o600

// PassthroughEnv is the host environment a child process may inherit.
//
// An ALLOWLIST, not a denylist, and that polarity is the whole point: the
// engine's own environment holds the org's chat token, its database DSN and
// possibly a metered API key, none of which a coding agent has any business
// reading. A denylist would leak every variable nobody thought to name.
//
// What is on it is what a process needs to run at all — where to find
// binaries, how to talk TLS, how to reach the network — never what to
// authenticate as. Credentials reach a child only through the run environment
// config deliberately put there.
var PassthroughEnv = []string{
	"PATH",
	"LANG",
	"LC_ALL",
	"LC_CTYPE",
	"LC_NUMERIC",
	"TERM",
	"TZ",
	"SSL_CERT_FILE",
	"SSL_CERT_DIR",
	"REQUESTS_CA_BUNDLE",
	"CURL_CA_BUNDLE",
	"NODE_EXTRA_CA_CERTS",
	"HTTP_PROXY",
	"HTTPS_PROXY",
	"ALL_PROXY",
	"NO_PROXY",
	"http_proxy",
	"https_proxy",
	"all_proxy",
	"no_proxy",
}

// Inherit returns the allowlisted slice of the host environment, ready to be
// extended with a box's own variables. Names that are unset are omitted rather
// than passed through empty: a child that distinguishes "unset" from "empty"
// (curl does, for NO_PROXY) must see the same thing the engine saw.
func Inherit(extra ...string) map[string]string {
	env := make(map[string]string, len(PassthroughEnv)+len(extra))
	for _, name := range append(append([]string{}, PassthroughEnv...), extra...) {
		if value, ok := os.LookupEnv(name); ok {
			env[name] = value
		}
	}
	return env
}

// ErrEscape reports a path that resolves outside the root it was joined to.
var ErrEscape = errors.New("hostbox: path escapes its root")

// SafeJoin resolves rel under root, refusing anything that lands outside it.
//
// The paths it guards are operator config — a CLI profile's credential_paths,
// a setup step's file entries — so "../../.." is a value someone can write. In
// a local sandbox that is not a sandboxed write: the box directory is a real
// host directory, so an escape writes to the engine host itself.
//
// Resolution is NON-STRICT, matching Python's Path.resolve(): the target
// usually does not exist yet (it is about to be created), so the check
// resolves the longest existing ancestor — which is where a symlink could
// redirect the write — and appends the rest lexically. A strict resolve would
// refuse every create, and a purely lexical Clean would miss a symlinked
// ancestor entirely.
//
// The root itself is not a valid result: these callers always name a file
// inside the box, so a rel that resolves back to the root is an escape that
// happened to stop at the boundary.
//
// An ABSOLUTE rel is refused rather than contained. filepath.Join would
// happily fold "/etc/passwd" into "<root>/etc/passwd", which is safe but
// silent — and every caller here has a relative-path contract, so an absolute
// value is a misconfiguration that should be read back to the operator rather
// than quietly rewritten into a path they did not name.
func SafeJoin(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: %q is absolute, and this path is relative to %q", ErrEscape, rel, root)
	}
	realRoot, err := resolve(root)
	if err != nil {
		return "", fmt.Errorf("hostbox: resolving root %q: %w", root, err)
	}
	target, err := resolve(filepath.Join(root, rel))
	if err != nil {
		return "", fmt.Errorf("hostbox: resolving %q: %w", rel, err)
	}
	if !Within(realRoot, target) {
		return "", fmt.Errorf("%w: %q is not under %q", ErrEscape, rel, realRoot)
	}
	return target, nil
}

// Within reports whether path is strictly inside root. Both must already be
// resolved; a caller holding raw input wants SafeJoin instead.
//
// Strictly: root is not within itself. A separator is appended before the
// prefix test so /home/user-2 does not read as being under /home/user.
func Within(root, path string) bool {
	if root == path {
		return false
	}
	prefix := root
	if !strings.HasSuffix(prefix, string(os.PathSeparator)) {
		prefix += string(os.PathSeparator)
	}
	return strings.HasPrefix(path, prefix)
}

// resolve is a non-strict filepath.EvalSymlinks: it walks up to the longest
// existing ancestor, resolves that, and re-appends what did not exist.
func resolve(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	remainder := ""
	current := filepath.Clean(abs)
	for {
		real, err := filepath.EvalSymlinks(current)
		if err == nil {
			return filepath.Join(real, remainder), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			// Walked to the volume root without finding anything that
			// exists. Nothing can be resolved, so the lexical form is
			// the honest answer.
			return filepath.Join(current, remainder), nil
		}
		remainder = filepath.Join(filepath.Base(current), remainder)
		current = parent
	}
}

// CopyFileAtomic copies src onto dst through a temp file and a rename, and
// reports whether it ran.
//
// Atomic because the file is usually a credential a live process may be
// reading: a partial write is a login that parses as corrupt, and both
// consumers write these while something else could be starting up. The temp
// file is created in dst's own directory so the rename never crosses a
// filesystem, and at [FileMode] so the secret is never briefly world-readable.
//
// A missing src is not an error — it is the ordinary case of a profile naming
// a credential file this vendor does not use — and reports false.
func CopyFileAtomic(src, dst string) (bool, error) {
	info, err := os.Stat(src)
	if err != nil || !info.Mode().IsRegular() {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), DirMode); err != nil {
		return false, err
	}
	in, err := os.Open(src)
	if err != nil {
		return false, err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".crewlet-tmp-*")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has succeeded

	if err := tmp.Chmod(FileMode); err != nil {
		tmp.Close()
		return false, err
	}
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return false, err
	}
	// Durability before visibility: a rename can be observed before the
	// data behind it reaches disk, so a crash between the two would leave
	// a credential file that exists and is empty.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return false, err
	}
	return true, nil
}

// FileDigest is the SHA-256 of a file's contents, or "" if it cannot be read.
//
// Used to decide whether a credential actually CHANGED before writing it back
// over the shared store. Comparing mtimes would not do: a coding agent
// rewrites its credential file on every run whether or not the token rotated,
// so an mtime test would write back constantly and race a concurrent seat.
func FileDigest(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return ""
	}
	return hex.EncodeToString(sum.Sum(nil))
}
