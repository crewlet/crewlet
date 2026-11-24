// Package hostbox holds the primitives for running somebody else's process on
// the engine host.
//
// Three subsystems do that, and they must do it identically: the subscription
// harness backend, which drives a harness per seat with its own HOME; the
// local sandbox provider, which runs a whole harness in a box and drives a
// container runtime's CLI to make one; and the MCP client, which starts every
// stdio tool server a company declares. The first two seed operator-supplied
// credential paths in and copy a refreshed login back out, and all three hand
// a child process an environment. A second implementation of any of those is
// the one that drifts — and each of them is a guard, so drift means a hole.
// The environment had already drifted: the harness backend carried a copy of the
// allowlist that disagreed with this one about five names, and the MCP client
// carried none at all and handed every tool server the engine's whole
// environment — its keyring and its Tier A token values among it. A container
// runtime's own CLI has a rule of its own ([ContainerRuntime]), held here for
// the same reason: two callers start one, the sandbox and a tool server
// declared as `command: docker`. So does where a local box writes a file it is
// handed ([InHome], [SafeJoin]), which config validation applies to a setup
// step before any box exists. It is also where a freshly written executable is
// waited for ([Settle]), because Gitlet's hook script and every test
// stand-in hit the same window. And so is how a shared login of several files
// is read and written: as ONE login, journaled under the directory's lock
// ([ReadCredentials], [WriteCredentials]), because the harness backend's seats
// and commands and a local box's teardown all write it, and one of them
// replacing its files a rename at a time is a login half written whenever that
// writer stops between two of them. A write names its steps ([CredentialStep])
// to an observer its caller hands it, nil in every write the engine makes, so
// the suites can kill a writer at every one.
//
// The allowlist is not isolation: a child runs as the engine's user and can
// read whatever that user can. What it removes is the engine handing its
// secrets over.
//
// Nothing here imports the rest of Crewlet: it sits below every consumer.
package hostbox

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// DirMode is the mode for every directory the harness backend and the local
// sandbox create. Credentials and prompt transcripts both land under one, on a
// host that may run other services.
const DirMode os.FileMode = 0o700

// FileMode is the mode for every file they create, for the same reason.
const FileMode os.FileMode = 0o600

// PassthroughEnv is the host environment every child process may inherit.
//
// An ALLOWLIST, not a denylist: the engine's environment holds the signing
// keyring, Tier A token values, stream-URL credentials, collector headers and
// possibly a metered API key, none of which a child may read, and a denylist
// leaks every variable nobody thought to name. It holds what a process needs
// to run (binaries, locale, TLS, network reach), never what to authenticate
// as: credentials reach a child only through the environment its config
// declares. ONE LIST FOR EVERY CHILD: a second copy drifts (a harness behind
// a SOCKS proxy lost the ALL_PROXY pair).
var PassthroughEnv = []string{
	"PATH",
	"LANG",
	"LANGUAGE",
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

// HostUserEnv is where the engine's own user keeps its files and its runtime
// state: the home, the user's name, the temporary directory and the XDG base
// directories.
//
// SEPARATE FROM [PassthroughEnv] because a box REPLACES every one of these: a
// harness on the host and a sandboxed one each get a HOME, an XDG tree and a
// TMPDIR of their own, which is the isolation, so inheriting the engine's would
// only be overwritten. A child that runs AS the host user in no box of its own —
// a stdio MCP server launched through `npx` or `uvx`, which keeps its package
// cache under HOME, and a container runtime's CLI, which finds its config and
// a rootless daemon's socket through HOME and XDG_RUNTIME_DIR — needs the real
// ones, and none of them authenticates anything.
var HostUserEnv = []string{
	"HOME",
	"USER",
	"LOGNAME",
	"TMPDIR",
	"XDG_CONFIG_HOME",
	"XDG_CACHE_HOME",
	"XDG_DATA_HOME",
	"XDG_STATE_HOME",
	"XDG_RUNTIME_DIR",
}

// Inherit returns the allowlisted slice of the host environment, ready to be
// extended with a child's own variables. Names that are unset are omitted
// rather than passed through empty: a child that distinguishes "unset" from
// "empty" (curl does, for NO_PROXY) must see the same thing the engine saw.
func Inherit(extra ...string) map[string]string {
	env := make(map[string]string, len(PassthroughEnv)+len(extra))
	for _, name := range append(append([]string{}, PassthroughEnv...), extra...) {
		if value, ok := os.LookupEnv(name); ok {
			env[name] = value
		}
	}
	return env
}

// InheritFamilies adds to env every host variable whose name begins with one
// of prefixes.
//
// FOR A CHILD WHOSE OWN CONFIGURATION IS A DOCUMENTED FAMILY of variables — a
// container runtime's `DOCKER_*` — where naming each member would break the
// child the day its vendor adds one, and silently: a runtime that no longer
// sees its `DOCKER_HOST` dials the default socket instead of failing. A family
// is still an allowlist; what it cannot do is reach a name outside itself.
func InheritFamilies(env map[string]string, prefixes ...string) {
	for _, kv := range os.Environ() {
		name, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(name, prefix) {
				env[name] = value
				break
			}
		}
	}
}

// ContainerRuntimeEnv is what a container runtime's own CLI reads by NAME,
// beyond the host user's locations: REGISTRY_AUTH_FILE, where Podman reads
// registry credentials, and DBUS_SESSION_BUS_ADDRESS, which rootless Podman's
// systemd cgroup manager dials.
var ContainerRuntimeEnv = []string{
	"REGISTRY_AUTH_FILE",
	"DBUS_SESSION_BUS_ADDRESS",
}

// ContainerRuntimeFamilies is what a container runtime's own CLI reads by
// PREFIX: the DOCKER_ family — DOCKER_HOST, DOCKER_CONTEXT, DOCKER_CONFIG and
// the TLS trio, without which a rootless Docker's CLI dials the system socket
// instead of the user's and a remote daemon is not the one configured — the
// CONTAINER_ and CONTAINERS_ families, Podman's remote connection and its
// config, storage and registry files, and the PODMAN_ family.
//
// FAMILIES rather than names, because each vendor documents the set as a
// prefix and adds to it, and a runtime that silently lost a new member would
// dial something other than what the operator configured. None of them is
// anything of the engine's.
var ContainerRuntimeFamilies = []string{"DOCKER_", "CONTAINER_", "CONTAINERS_", "PODMAN_"}

// ContainerRuntime is the host environment a container runtime's CLI runs
// with: the allowlist, [HostUserEnv] (the runtime's config and a rootless
// daemon's XDG_RUNTIME_DIR socket), [ContainerRuntimeEnv] and every member of
// [ContainerRuntimeFamilies] the engine was started with.
//
// ONE RULE FOR BOTH CALLERS (the local sandbox, and a stdio MCP server
// declared as `command: docker` or `podman`): a second copy loses DOCKER_HOST,
// and a rootless runtime then dials the system socket and fails as a
// permission error naming no variable. Never what an image or credential
// helper authenticates with: a caller's declared channel layers over this.
func ContainerRuntime() map[string]string {
	env := Inherit(append(append([]string{}, HostUserEnv...), ContainerRuntimeEnv...)...)
	InheritFamilies(env, ContainerRuntimeFamilies...)
	return env
}

// ContainerRuntimeCarries reports whether [ContainerRuntime] passes name
// through from the host, whether or not the host has it set.
//
// The question a check asks of a `-e NAME` it finds in a container's run
// arguments: the runtime copies NAME from its OWN environment, so a name this
// answers false for arrives in the box as nothing at all unless a caller
// declares it.
func ContainerRuntimeCarries(name string) bool {
	if slices.Contains(PassthroughEnv, name) || slices.Contains(HostUserEnv, name) ||
		slices.Contains(ContainerRuntimeEnv, name) {
		return true
	}
	for _, prefix := range ContainerRuntimeFamilies {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// IsContainerRuntime reports whether command starts a container runtime's
// CLI — `docker` or `podman`, by name or by path — and so is owed
// [ContainerRuntime] rather than the environment of an ordinary child.
func IsContainerRuntime(command string) bool {
	switch filepath.Base(strings.TrimSpace(command)) {
	case "docker", "podman":
		return true
	}
	return false
}

// Environ renders env as os/exec's KEY=value slice: sorted, and NEVER nil.
//
// NEVER NIL, because a nil exec.Cmd.Env does not mean "no environment": os/exec
// then hands the child the ENGINE's own, which is precisely what the allowlist
// exists to keep out of it. An empty map therefore renders as an empty,
// non-nil slice — a child with nothing rather than a child with everything —
// and a caller whose child genuinely needs something states it.
//
// Sorted, so a spawn is reproducible from a log line and a test compares an
// environment rather than a map's iteration order.
func Environ(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for name, value := range env {
		out = append(out, name+"="+value)
	}
	slices.Sort(out)
	return out
}

// ErrEscape reports a path that resolves outside the root it was joined to.
var ErrEscape = errors.New("hostbox: path escapes its root")

// ContainerHome is where a local sandbox's container box mounts its home, and
// so the only place a path handed to that box can name a file in it.
//
// A FIXED PATH, mounted from the box directory, so a setup step, an image and
// the docs name one stable home whichever box it is; the sandbox package's
// DefaultHome is this value. HERE, beside [InHome], because it is the home
// InHome judges a container box's paths against: this is the one leaf package
// that config validation, both boxes and the sandbox package already import,
// so each of them reads the value from the rule's own package rather than from
// a copy of it.
const ContainerHome = "/home/user"

// InHome is path relative to a box's home, and whether it names a file inside
// it. THE LEXICAL HALF OF THE RULE BOTH BOXES HOLD A FILE TO, judgeable before
// any box exists (config validation uses it); [SafeJoin] is the half that
// follows links. A copy in the validator would drift into a document that
// validates and boxes that fail at setup. Relative paths are under home;
// absolute ones only beneath it after cleaning. Refused: "..", the home
// itself, an empty path. A LEADING "~" IS THE HOME as a shell reads it ("~/x"
// is "x"; "~name/x" and "~\x" refused, a backslash being a filename character
// to a POSIX host, not a separator); read as plain relative, "~/.npmrc" would
// land in a directory named "~". home is the path inside the box
// ([ContainerHome], or a direct box's host directory); EMPTY is one no path
// can name, so only a relative path lands in it.
func InHome(home, path string) (string, bool) {
	if path == "" {
		return "", false
	}
	if path[0] == '~' {
		// Only a slash may follow the tilde; what follows is then relative
		// to the home, and an empty rest is the home itself, which the
		// check below refuses.
		if len(path) > 1 && path[1] != '/' {
			return "", false
		}
		path = "." + string(filepath.Separator) + path[1:]
	}
	rel := filepath.Clean(path)
	if filepath.IsAbs(path) {
		if home == "" {
			return "", false
		}
		within, err := filepath.Rel(home, rel)
		if err != nil {
			return "", false
		}
		rel = within
	}
	up := ".." + string(filepath.Separator)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, up) {
		return "", false
	}
	return rel, true
}

// SafeJoin resolves rel under root, refusing anything that lands outside it.
//
// Paths are operator config, and a local box directory is a real host
// directory, so an escape writes to the engine host. Resolution is NON-STRICT:
// the target usually does not exist yet, so it resolves the longest existing
// ancestor (where a symlink could redirect the write) and appends the rest
// lexically; strict refuses every create, lexical misses a symlinked ancestor. The result is the RESOLVED path the check was
// made against (it differs from root on macOS under /var or /tmp). The root
// itself is not a valid result: callers name a file inside the box. An
// ABSOLUTE rel is refused, not folded into root, as a misconfiguration to
// read back.
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

// ReplaceFile puts what content yields at dst, through a temp file in dst's
// own directory and a rename, at [FileMode] — [ReplaceFileIn] over that
// directory.
//
// Atomic because the file is usually a credential or record a live process
// may be reading: a partial write parses as corrupt.
//
// It takes a reader, not a path, so a caller that opened its source once (so
// what it compares is what it writes) never has it opened again. dst's
// directory must exist. ONE file: a multi-file login is [WriteCredentials]'s,
// since replacing files one at a time leaves a login half written.
func ReplaceFile(dst string, content io.Reader) error {
	dir, err := os.OpenRoot(filepath.Dir(dst))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return ReplaceFileIn(dir, filepath.Base(dst), content)
}

// ReplaceFileIn is [ReplaceFile] for name beneath root: the temp file, the
// rename and every directory between are reached through the root, so no
// component of name — a link a process that writes the tree put there
// included — leads the write outside it.
//
// The temp file is created in name's own directory so the rename never
// crosses a filesystem, exclusively and at [FileMode] so a secret is never
// briefly world-readable, and an exclusive create follows no link at all.
// name's directory must exist.
func ReplaceFileIn(root *os.Root, name string, content io.Reader) error {
	return replaceStaged(root, name, filepath.Join(filepath.Dir(name), stagedPrefix+rand.Text()), content)
}

// stagedPrefix starts the name of every file [ReplaceFileIn] stages: a file
// under it is a write that has not reached its rename, which no reader reads,
// and one a killed write left behind is what a shared login's next hold of its
// lock removes ([ReadCredentials]).
const stagedPrefix = ".crewlet-tmp-"

// replaceStaged is [ReplaceFileIn] through the staged file tmpName, beneath
// root and in name's own directory, which it creates exclusively: it fails
// rather than write through whatever stands there.
func replaceStaged(root *os.Root, name, tmpName string, content io.Reader) error {
	tmp, err := root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, FileMode)
	if err != nil {
		return err
	}
	// No-op once the rename has succeeded, and best effort if it did not:
	// the caller is already returning the failure that matters.
	defer func() { _ = root.Remove(tmpName) }()

	if err := tmp.Chmod(FileMode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := io.Copy(tmp, content); err != nil {
		_ = tmp.Close()
		return err
	}
	// Durability before visibility: a rename can be observed before the
	// data behind it reaches disk, so a crash between the two would leave
	// a credential file that exists and is empty.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return root.Rename(tmpName, name)
}

// Digest is the SHA-256 of what r yields, in lowercase hexadecimal — the form
// every digest of a login file takes, so a seed recorded by one consumer
// compares with a file read by another.
func Digest(r io.Reader) (string, error) {
	sum := sha256.New()
	if _, err := io.Copy(sum, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}
