//go:build linux

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// confineProbeTimeout bounds the trial run a probe makes: one process start.
const confineProbeTimeout = 10 * time.Second

// ProbeConfinement reports the strongest confinement this host enforces.
//
// BUBBLEWRAP FIRST, because its namespaces give the job a /proc of its own
// besides a filesystem view, and it is proven by a trial run rather than
// assumed from its presence: a distribution that restricts unprivileged user
// namespaces leaves bwrap installed and unable to start anything. LANDLOCK
// SECOND, applied by this binary re-executed, when the kernel has it.
func ProbeConfinement() (*HostConfinement, error) {
	var reasons []string
	if bwrap, err := exec.LookPath("bwrap"); err != nil {
		reasons = append(reasons, "bubblewrap (bwrap) is not installed")
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), confineProbeTimeout)
		out, runErr := exec.CommandContext(ctx, bwrap, "--die-with-parent", "--unshare-pid",
			"--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--", "true").CombinedOutput()
		cancel()
		if runErr == nil {
			return &HostConfinement{Mechanism: MechanismBubblewrap, helper: bwrap}, nil
		}
		reasons = append(reasons, fmt.Sprintf("bubblewrap cannot start a namespace here (%s)",
			strings.TrimSpace(string(out)+" "+runErr.Error())))
	}
	abi, err := landlockABI()
	if err != nil {
		reasons = append(reasons, "the kernel has no Landlock ("+err.Error()+")")
		return nil, &ConfinementUnavailableError{Reason: strings.Join(reasons, "; ")}
	}
	self, err := os.Executable()
	if err != nil {
		reasons = append(reasons, fmt.Sprintf("Landlock %d is available but the engine's own "+
			"binary cannot be found to apply it (%v)", abi, err))
		return nil, &ConfinementUnavailableError{Reason: strings.Join(reasons, "; ")}
	}
	return &HostConfinement{Mechanism: MechanismLandlock, helper: self}, nil
}

// landlockABI is the kernel's Landlock version, or why there is none.
func landlockABI() (int, error) {
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0,
		unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return 0, errno
	}
	if int(abi) < 1 {
		return 0, errors.New("version 0")
	}
	return int(abi), nil
}

// landlockFileRights are the rights a rule on a FILE may carry: the kernel
// refuses a directory right on a non-directory.
const landlockFileRights = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
	unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_TRUNCATE |
	unix.LANDLOCK_ACCESS_FS_IOCTL_DEV

// landlockReadRights read and execute a tree.
const landlockReadRights = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_READ_FILE |
	unix.LANDLOCK_ACCESS_FS_READ_DIR

// handledRights are the filesystem rights abi can restrict: every one it knows,
// so nothing it knows is left unrestricted.
func handledRights(abi int) uint64 {
	rights := uint64(unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG | unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM)
	if abi >= 2 {
		rights |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		rights |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if abi >= 5 {
		rights |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	return rights
}

// rulesetAttr is the kernel's landlock_ruleset_attr as version 1 defines it:
// a larger struct with zero tail is accepted by every later kernel, a smaller
// one by every kernel, so the filesystem field alone is passed.
type rulesetAttr struct{ handledFS uint64 }

// ConfineMain is the Landlock helper: `crewlet sandbox-confine <spec> -- argv`.
// It restricts this thread to the spec and executes argv in its place, so the
// job starts inside the domain and can never leave it. It returns only on a
// failure, whose exit status the caller passes on.
func ConfineMain(args []string) int {
	if len(args) < 3 || args[1] != "--" {
		fmt.Fprintln(os.Stderr, "usage: crewlet "+ConfineCommand+" <rules> -- <command> [args...]")
		return 2
	}
	var spec landlockSpec
	if err := json.Unmarshal([]byte(args[0]), &spec); err != nil {
		fmt.Fprintln(os.Stderr, "crewlet "+ConfineCommand+": the rules are not readable:", err)
		return 2
	}
	argv := args[2:]
	path, err := exec.LookPath(argv[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "crewlet "+ConfineCommand+":", err)
		return 127
	}
	// ONE THREAD, start to finish: Landlock restricts the calling thread,
	// and execve keeps exactly that thread's domain.
	runtime.LockOSThread()
	if err = applyLandlock(spec); err != nil {
		fmt.Fprintln(os.Stderr, "crewlet "+ConfineCommand+": the confinement could not be applied, "+
			"so nothing ran:", err)
		return 126
	}
	err = unix.Exec(path, argv, os.Environ())
	fmt.Fprintln(os.Stderr, "crewlet "+ConfineCommand+":", err)
	return 126
}

// applyLandlock restricts the calling thread to spec.
//
// The ruleset's descriptor is closed on every path, and a failed close is
// reported with whatever else failed: the job is not started on a thread whose
// confinement left a descriptor in an unknown state.
func applyLandlock(spec landlockSpec) (err error) {
	abi, err := landlockABI()
	if err != nil {
		return fmt.Errorf("landlock: %w", err)
	}
	handled := handledRights(abi)
	attr := rulesetAttr{handledFS: handled}
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return fmt.Errorf("landlock_create_ruleset: %w", errno)
	}
	ruleset := int(fd)
	defer func() { err = errors.Join(err, closeFD(ruleset, "the landlock ruleset")) }()
	add := func(paths []string, rights uint64) error {
		for _, p := range paths {
			if err := addLandlockRule(ruleset, p, rights&handled); err != nil {
				return err
			}
		}
		return nil
	}
	devRights := uint64(landlockReadRights | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_IOCTL_DEV | unix.LANDLOCK_ACCESS_FS_TRUNCATE)
	if err := add(spec.Read, landlockReadRights); err != nil {
		return err
	}
	if err := add(spec.Dev, devRights); err != nil {
		return err
	}
	if err := add(spec.Write, handled); err != nil {
		return err
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(ruleset), 0, 0); errno != 0 {
		return fmt.Errorf("landlock_restrict_self: %w", errno)
	}
	return nil
}

// addLandlockRule grants rights beneath one path. A path that does not exist
// grants nothing and is skipped: an allowlist entry for a directory this host
// lacks is simply nothing to reach.
func addLandlockRule(ruleset int, path string, rights uint64) (err error) {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EACCES) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { err = errors.Join(err, closeFD(fd, path)) }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		rights &= landlockFileRights
	}
	if rights == 0 {
		return nil
	}
	attr := unix.LandlockPathBeneathAttr{Allowed_access: rights, Parent_fd: int32(fd)}
	if _, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(ruleset),
		unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&attr)), 0, 0, 0); errno != 0 {
		return fmt.Errorf("landlock_add_rule %s: %w", path, errno)
	}
	return nil
}

// closeFD closes a descriptor this file opened, naming what it held when the
// close fails.
func closeFD(fd int, what string) error {
	if err := unix.Close(fd); err != nil {
		return fmt.Errorf("close %s: %w", what, err)
	}
	return nil
}
