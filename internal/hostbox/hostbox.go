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
// environment — its keyring and its Tier A token values among it. A container runtime's own CLI has a rule
// of its own ([ContainerRuntime]), held here for the same reason: two callers
// start one, the sandbox and a tool server declared as `command: docker`.
// So does where a local box writes a file it is handed ([InHome], [SafeJoin]),
// which config validation applies to a setup step before any box exists. And
// so does how a shared login of several files is read and written: as ONE
// login, journaled under the directory's lock ([ReadCredentials],
// [WriteCredentials]), because the harness backend's seats and commands and a
// local box's teardown all write it, and one of them replacing its files a
// rename at a time is a login half written whenever that writer stops between
// two of them.
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
// An ALLOWLIST, not a denylist, and that polarity is the whole point: the
// engine's own environment holds the keyring every session cookie and every
// sealed revision is signed under, the Tier A token values, any credential
// in an external stream URL, the
// collector credential in OTEL_EXPORTER_OTLP_HEADERS and possibly a metered
// API key,
// none of which a child has any business reading. A denylist would leak every
// variable nobody thought to name — and the engine's Tier A `${VAR}`
// references mean an operator names those variables, not this package.
//
// What is on it is what a process needs to run at all — where to find
// binaries, how to render text, how to talk TLS, how to reach the network —
// never what to authenticate as. Credentials reach a child only through the
// environment its config deliberately declares.
//
// ONE LIST FOR EVERY CHILD. The harness backend kept its own and had already
// drifted from this one: it lacked LC_NUMERIC, TERM and the ALL_PROXY pair (so
// a harness behind a SOCKS proxy reached nothing), and it carried LANGUAGE,
// which this one did not.
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
// with: the allowlist, the host user's locations ([HostUserEnv] — where the
// runtime keeps its config, and XDG_RUNTIME_DIR, where a rootless daemon's
// socket lives), [ContainerRuntimeEnv] and every member of
// [ContainerRuntimeFamilies] the engine was started with.
//
// ONE RULE FOR EVERY CALLER THAT STARTS ONE, and there are two: the local
// sandbox drives the CLI to make and run a box, and a stdio MCP server
// declared as `command: docker` or `podman` IS the CLI. Written twice, the
// copy on the second path is the one that loses DOCKER_HOST — and a rootless
// runtime without it dials the system socket, which fails as a permission
// error at the daemon rather than as anything naming the variable.
//
// It is what the runtime needs to reach the daemon the operator configured,
// never what an image or a credential helper authenticates with: a caller
// with a declared channel layers that over this.
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
// It is the home of the one account every remote (E2B) template runs as, and
// the mount was put there so a path reads the same in either cell; the sandbox
// package's DefaultHome is this value. HERE, beside [InHome], because it is
// the home InHome judges a container box's paths against: this is the one
// leaf package that config validation, both local boxes and the sandbox
// package already import, so each of them reads the value from the rule's own
// package rather than from a copy of it.
const ContainerHome = "/home/user"

// InHome is where path lands under a local sandbox box's home: the path
// relative to that home, and whether it names a file inside it at all.
//
// THE LEXICAL HALF OF THE ONE RULE BOTH LOCAL CELLS HOLD A FILE TO — a direct
// box, which has no filesystem of its own, and a container box, which writes
// a file on the host side of its one mount — and the half that can be judged
// before any box exists, which is what config validation does with a setup
// step's files. Both boxes call this before they touch a path and
// [SafeJoin] after it, which is the other half: following whatever links the
// host's existing directories hold, which only a box on that host can do. A
// copy of the rule in the validator would be the one that drifts, and the
// symptom of the drift is a document that validates and a seat whose every box
// fails at setup.
//
// A relative path is under the home as it stands. An absolute one is under
// it only when it names somewhere beneath it, compared after both are
// cleaned, so "/home/user//x" is inside and "/home/user/../../etc/x" is not.
// Either is refused when it climbs out with "..", and so is the home itself,
// which is no file in it — and so is an empty path, which names nothing.
//
// A LEADING "~" IS THE HOME, as a remote (E2B) box's own agent reads one when
// it writes a file there: "~/x" is the relative "x", and "~" alone is the
// home itself, refused as above. "~name/x" — another account's home — is
// refused, as that agent refuses it. Read as an ordinary relative path
// instead, "~/.npmrc" was accepted here and written to a directory named "~"
// in a local box's home, while the same document put the file in the home in
// a remote one: one setup step, two places, and a rule that approved both.
//
// home is the home as a path inside the box names it: [ContainerHome] for a
// container box, the host directory a direct box was minted for. EMPTY is a
// home NO path can name, which is what a direct box's home is to everything
// written before the box exists — it is minted per box, under a name no
// document holds — so only a relative path lands in one.
func InHome(home, path string) (string, bool) {
	if path == "" {
		return "", false
	}
	if path[0] == '~' {
		// Both separators, as the remote box's agent takes either after the
		// tilde; what follows is then relative to the home, and an empty
		// rest is the home itself, which the check below refuses.
		if len(path) > 1 && path[1] != '/' && path[1] != '\\' {
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
// The paths it guards are operator config — a harness profile's credential_paths,
// a setup step's file entries — so "../../.." is a value someone can write. In
// a local sandbox that is not a sandboxed write: the box directory is a real
// host directory, so an escape writes to the engine host itself.
//
// Resolution is NON-STRICT, because the target usually does not exist yet
// (it is about to be created), so the check
// resolves the longest existing ancestor — which is where a symlink could
// redirect the write — and appends the rest lexically. A strict resolve would
// refuse every create, and a purely lexical Clean would miss a symlinked
// ancestor entirely.
//
// The result is the RESOLVED path, with the symlinks in the root and in every
// existing ancestor already followed, because that is the string the
// containment check was made against: what the caller writes to is visibly
// under the resolved root, rather than a join whose meaning depends on links
// being read again. A caller comparing the result with the root it passed
// compares against that root's resolved form, which on macOS differs for any
// directory under /var or /tmp.
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

// ReplaceFile puts what content yields at dst, through a temp file in dst's
// own directory and a rename, at [FileMode] — [ReplaceFileIn] over that
// directory.
//
// Atomic because the file is usually a credential or a record a live process
// may be reading: a partial write is one that parses as corrupt, and every
// consumer writes these while something else could be starting up.
//
// It takes a reader rather than a source path: a caller that opened its
// source in a particular way — once, so that what it compares is what it
// writes — must not hand this a path to open again. dst's directory must
// exist. ONE file: a login of several is [WriteCredentials]'s, because
// replacing its files one at a time is a login half written whenever the
// process stops between two of them.
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
	tmpName := filepath.Join(filepath.Dir(name), ".crewlet-tmp-"+rand.Text())
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
