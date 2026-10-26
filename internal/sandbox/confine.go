package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// HOST CONFINEMENT: a direct box whose job may not read the engine.
//
// [Direct] isolates a run's STATE and nothing else: its job runs as the engine
// user, so a shell it starts reads whatever that user can, the engine's data
// directory, its keyring and the operator's own files included. That is the
// documented contract for a seat's code work on a workstation, and it is not
// one a person's Dashboard Assistant may run under: a deep-work run is steered
// by whatever the person asked and whatever pages it read (ADR-0054).
//
// A CONFINED direct box runs every command its job starts inside the
// strongest boundary this host can enforce, chosen once by [ProbeConfinement]:
//
//   - macOS: sandbox-exec with a profile rendered per box ([sandboxProfile]),
//     which denies reads and writes under every [Confinement.Deny] path, every
//     write outside the box's home, every other process's information and the
//     login keychain's service.
//   - Linux, with unprivileged user namespaces: bubblewrap ([bwrapArgv]), the
//     host's root read-only, every denied path hidden under an empty tmpfs or
//     /dev/null, a fresh /proc in a new pid namespace, and the box's home the
//     one writable directory.
//   - Linux, otherwise: Landlock, applied by the engine's own binary re-executed
//     as `crewlet sandbox-confine` ([ConfineMain]): an allowlist of the host's
//     tree carved around every denied path ([carve]), the box's home the one
//     writable directory. Landlock also confines ptrace, so another process's
//     /proc environ is unreadable.
//
// A HOST THAT CAN ENFORCE NONE OF THESE RUNS NOTHING CONFINED. The caller is
// told why ([ConfinementUnavailableError]) and refuses the run; it never falls
// back to an unconfined job.

// Confinement is what a confined direct box's job may not touch.
//
// The box's own records, every other box and the box root are added by the
// backend itself ([Local.confinedRules]); a caller names the engine's paths.
type Confinement struct {
	// Deny are paths the job may neither read nor write: the engine's data
	// directory, its stream store, its keyring and secret files, its config
	// files and the operator's home.
	Deny []string

	// Read are paths beneath a denied one that the job may READ: the coding
	// CLI's own installation, which on a workstation sits in the operator's
	// home. Never writable.
	Read []string
}

// Mechanism is how a host confines a job.
type Mechanism string

const (
	// MechanismSandboxExec is macOS's sandbox-exec.
	MechanismSandboxExec Mechanism = "sandbox-exec"
	// MechanismBubblewrap is bubblewrap over Linux namespaces.
	MechanismBubblewrap Mechanism = "bubblewrap"
	// MechanismLandlock is Linux Landlock, applied by the engine's binary.
	MechanismLandlock Mechanism = "landlock"
)

// ConfinementUnavailableError is a host that cannot confine a job, with why:
// what an administrator reads to decide between installing bubblewrap,
// enabling Landlock and configuring a remote placement.
type ConfinementUnavailableError struct{ Reason string }

func (e *ConfinementUnavailableError) Error() string {
	return "this engine host cannot confine a job, so nothing runs on it unconfined: " +
		e.Reason + "; configure a container or e2b placement under providers.sandbox, " +
		"or run the engine where Landlock, bubblewrap or sandbox-exec is available"
}

// ErrConfinementUnavailable matches every [ConfinementUnavailableError].
var ErrConfinementUnavailable = errors.New("sandbox: host confinement unavailable")

// Is makes the error [ErrConfinementUnavailable] under errors.Is.
func (e *ConfinementUnavailableError) Is(target error) bool {
	return target == ErrConfinementUnavailable
}

// HostConfinement is a probed mechanism, ready to confine boxes.
type HostConfinement struct {
	Mechanism Mechanism
	// helper is the engine's own executable, for [MechanismLandlock], and
	// the bwrap binary for [MechanismBubblewrap].
	helper string
}

// confineRules are one box's resolved paths.
type confineRules struct {
	// deny are hidden from the job, read and write.
	deny []string
	// read are readable beneath a denied path.
	read []string
	// write is the box's home, the one writable tree.
	write string
}

// ConfineCommand is the hidden command the engine binary answers as the
// Landlock helper. The CLI dispatches it to [ConfineMain].
const ConfineCommand = "sandbox-confine"

// resolvePath is p absolute, cleaned and with its symlinks resolved as far as
// they exist: sandbox-exec and Landlock both match the REAL path (/var is
// /private/var on macOS), so a rule written through a link names nothing.
func resolvePath(p string) (string, error) {
	if strings.ContainsAny(p, "\x00\n\r\"\\") {
		return "", fmt.Errorf("confinement path %q holds a character no rule can name", p)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("confinement path %q is not usable: %w", p, err)
	}
	// Resolve the longest existing prefix, keep the rest as written: a deny
	// rule for a file the engine has not created yet still has to hold.
	rest := ""
	for cur := abs; ; {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, rest), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// resolveAll resolves every path, dropping empties and duplicates.
func resolveAll(paths []string) ([]string, error) {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		r, err := resolvePath(p)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	slices.Sort(out)
	return out, nil
}

// within reports whether p is root or beneath it.
func within(p, root string) bool {
	if p == root {
		return true
	}
	if root == "/" {
		return strings.HasPrefix(p, "/")
	}
	return strings.HasPrefix(p, root+"/")
}

// wrap is argv run under the confinement, for a box whose rules are r.
func (h *HostConfinement) wrap(r confineRules, argv []string) ([]string, error) {
	switch h.Mechanism {
	case MechanismSandboxExec:
		return append([]string{"/usr/bin/sandbox-exec", "-p", sandboxProfile(r), "--"}, argv...), nil
	case MechanismBubblewrap:
		return append(bwrapArgv(h.helper, r), argv...), nil
	case MechanismLandlock:
		spec, err := json.Marshal(landlockSpecOf(r))
		if err != nil {
			return nil, fmt.Errorf("encode the landlock rules: %w", err)
		}
		return append([]string{h.helper, ConfineCommand, string(spec), "--"}, argv...), nil
	}
	return nil, &ConfinementUnavailableError{Reason: fmt.Sprintf("%q is no mechanism this build runs", h.Mechanism)}
}

// ancestorsOf is every directory above path, from the root down, path
// itself excluded.
func ancestorsOf(path string) []string {
	var out []string
	for dir := filepath.Dir(filepath.Clean(path)); ; dir = filepath.Dir(dir) {
		out = append(out, dir)
		if dir == filepath.Dir(dir) {
			break
		}
	}
	slices.Reverse(out)
	return out
}

// sbplString quotes s as a sandbox profile string literal. Paths reaching it
// were refused any quote, backslash or line break by [resolvePath].
func sbplString(s string) string { return strconv.Quote(s) }

// keychainServices are the macOS services that hand out the login keychain's
// items. Denied, a job cannot ask for the operator's stored passwords; the
// coding CLI signs in from the credentials seeded into the box's own home.
var keychainServices = []string{
	"com.apple.SecurityServer",
	"com.apple.securityd",
	"com.apple.security.agent",
}

// sandboxProfile is the sandbox-exec profile for one box.
//
// THE LAST MATCHING RULE WINS in a sandbox profile, which is what the order
// relies on: everything is allowed, the denied trees and every write are then
// taken away, and the box's home and the CLI's install are given back last.
// Network stays open: web access is the run's (ADR-0054).
//
// THE HOME'S ANCESTORS MAY BE STAT'ED, each by its literal path and for its
// metadata alone: never listed, never read, never written. A denied tree
// usually holds the home (the boxes directory, beside the other boxes and
// the records), and macOS's `mkdir -p` makes every component of an absolute
// path in turn, reading an EPERM as failure unless a stat shows the
// directory is there. Without this a run could not `mkdir -p "$HOME/out"`,
// the first line of most scripts a deep-work run writes.
func sandboxProfile(r confineRules) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n")
	if len(r.deny) > 0 {
		b.WriteString("(deny file-read* file-write*")
		for _, d := range r.deny {
			b.WriteString(" (subpath " + sbplString(d) + ")")
		}
		b.WriteString(")\n")
	}
	b.WriteString("(deny file-write* (subpath \"/\"))\n")
	b.WriteString("(deny process-info* (target others))\n")
	b.WriteString("(deny mach-lookup")
	for _, s := range keychainServices {
		b.WriteString(" (global-name " + sbplString(s) + ")")
	}
	b.WriteString(")\n")
	if len(r.read) > 0 {
		b.WriteString("(allow file-read*")
		for _, p := range r.read {
			b.WriteString(" (subpath " + sbplString(p) + ")")
		}
		b.WriteString(")\n")
	}
	if ancestors := ancestorsOf(r.write); len(ancestors) > 0 {
		b.WriteString("(allow file-read-metadata")
		for _, a := range ancestors {
			b.WriteString(" (literal " + sbplString(a) + ")")
		}
		b.WriteString(")\n")
	}
	b.WriteString("(allow file-read* file-write* (subpath " + sbplString(r.write) + "))\n")
	b.WriteString("(allow file-write* (literal \"/dev/null\") (literal \"/dev/zero\") " +
		"(literal \"/dev/tty\") (literal \"/dev/dtracehelper\") (regex #\"^/dev/ttys[0-9]+$\"))\n")
	return b.String()
}

// bwrapArgv is the bubblewrap command line for one box, before the job's argv.
//
// The host's root is bound READ-ONLY, then every denied path is masked: a
// directory under an empty tmpfs, a file under /dev/null. The CLI's install is
// bound back read-only and the box's home read-write, last, so they win over
// a mask of a tree that holds them. A new pid namespace with its own /proc
// keeps every other process's environment out of reach.
func bwrapArgv(bwrap string, r confineRules) []string {
	argv := []string{bwrap,
		"--die-with-parent", "--new-session",
		"--unshare-pid", "--unshare-ipc", "--unshare-uts", "--unshare-cgroup-try",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/tmp",
	}
	for _, d := range r.deny {
		info, err := os.Stat(d)
		switch {
		case err != nil:
			// Nothing there to mask. A path that appears later is not
			// in this namespace's view of the host either way: the root
			// is bound at start, and the parent mask hides siblings.
			continue
		case info.IsDir():
			argv = append(argv, "--tmpfs", d)
		default:
			argv = append(argv, "--ro-bind", "/dev/null", d)
		}
	}
	for _, p := range r.read {
		argv = append(argv, "--ro-bind-try", p, p)
	}
	argv = append(argv, "--bind", r.write, r.write, "--chdir", r.write, "--")
	return argv
}

// landlockSpec is what the Landlock helper applies before it executes the job:
// the trees it may read and execute, and the trees it may also write.
type landlockSpec struct {
	Read  []string `json:"read"`
	Write []string `json:"write"`
	// Dev is written with device rights only: /dev/null, a terminal.
	Dev []string `json:"dev"`
}

// landlockSpecOf carves the readable host around the denied paths.
func landlockSpecOf(r confineRules) landlockSpec {
	read := carve("/", r.deny, os.ReadDir)
	read = append(read, r.read...)
	slices.Sort(read)
	return landlockSpec{Read: slices.Compact(read), Write: []string{r.write}, Dev: []string{"/dev"}}
}

// carve is root's tree as the fewest subtrees that hold none of deny.
//
// Landlock only ADDS access, so a tree with a hole in it is written as its
// siblings: a root holding a denied path is split into its children, each
// child that holds none is kept whole, and the one that holds the path is
// split again, down to the path itself, which is left out. readDir is the
// directory listing, injected so the walk is testable on any host.
func carve(root string, deny []string, readDir func(string) ([]os.DirEntry, error)) []string {
	holds := false
	for _, d := range deny {
		if within(root, d) {
			return nil
		}
		if within(d, root) {
			holds = true
		}
	}
	if !holds {
		return []string{root}
	}
	entries, err := readDir(root)
	if err != nil {
		// A tree that cannot be listed cannot be carved, and is left out
		// whole rather than allowed whole.
		return nil
	}
	var out []string
	for _, e := range entries {
		out = append(out, carve(filepath.Join(root, e.Name()), deny, readDir)...)
	}
	return out
}
