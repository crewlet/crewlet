package boxhelpertest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/prereqtest"
)

// psTimeout bounds one ps: it answers at once, so past this it hangs.
const psTimeout = 10 * time.Second

// commandLine is the arguments the kernel holds for pid, executable first:
// exactly, from /proc, on Linux, unless the process rewrote its title into
// one string, as Chromium does in its new headless mode; elsewhere, and
// then, that one line split on spaces, which is exact for arguments holding
// no space.
func commandLine(pid int) ([]string, error) {
	if runtime.GOOS == "linux" {
		raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			return nil, fmt.Errorf("pid %d holds no command line: its main thread has exited", pid)
		}
		args := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
		if len(args) == 1 {
			args = strings.Split(args[0], " ")
		}
		return args, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), psTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-ww", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return nil, fmt.Errorf("ps of pid %d: %w", pid, err)
	}
	return strings.Split(strings.TrimSpace(string(out)), " "), nil
}

// processesCarrying is every process whose arguments hold arg, as
// commandLine reads them: from /proc on Linux, one process at a time, a
// process gone between the listing and the reading passed over; elsewhere
// from one ps of every process.
func processesCarrying(arg string) ([]int, error) {
	var pids []int
	if runtime.GOOS == "linux" {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			pid, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			if args, err := commandLine(pid); err == nil && slices.Contains(args[1:], arg) {
				pids = append(pids, pid)
			}
		}
		return pids, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), psTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-A", "-ww", "-o", "pid=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps of every process: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		pid, command, _ := strings.Cut(strings.TrimSpace(line), " ")
		args := strings.Fields(command)
		if n, err := strconv.Atoi(pid); err == nil && len(args) > 1 && slices.Contains(args[1:], arg) {
			pids = append(pids, n)
		}
	}
	return pids, nil
}

// Argv is the arguments the kernel holds for the process pid, a browser the
// case laid out as executable, after its executable: exact on Linux, and on
// macOS ps's line with executable stripped as a prefix it knows, the rest
// split on single spaces, exact for switches and paths holding no space.
func Argv(t testing.TB, pid int, executable string) []string {
	t.Helper()
	if runtime.GOOS == "linux" {
		args, err := commandLine(pid)
		if err != nil {
			t.Fatal(err)
		}
		return args[1:]
	}
	ctx, cancel := context.WithTimeout(t.Context(), psTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-ww", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("ps of pid %d: %v", pid, err)
	}
	rest, ok := strings.CutPrefix(strings.TrimSpace(string(out)), executable+" ")
	if !ok {
		t.Fatalf("pid %d runs %q, not %s", pid, out, executable)
	}
	return strings.Split(rest, " ")
}

// EnvironNames is the name of every variable in pid's environment as the
// kernel holds it, read from /proc; on another platform, which keeps it from
// another process, it is the error ErrNoEnviron.
func EnvironNames(pid int) ([]string, error) {
	if runtime.GOOS != "linux" {
		return nil, ErrNoEnviron
	}
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, kv := range bytes.Split(bytes.TrimSuffix(raw, []byte{0}), []byte{0}) {
		name, _, _ := strings.Cut(string(kv), "=")
		names = append(names, name)
	}
	return names, nil
}

// ErrNoEnviron is a platform that does not show a process's environment.
var ErrNoEnviron = errors.New("boxhelpertest: only Linux shows another process's environment")

// SandboxRuns is whether Chromium's own sandbox can start on this host, told
// independently of the helper: on Linux whether an unprivileged process can
// make a user namespace and map itself root in it, as Chromium's namespace
// sandbox does (`unshare --user --map-root-user true`), which Ubuntu's
// AppArmor restriction, user namespaces turned off or a container's seccomp
// profile each refuse; on macOS always, since its sandbox needs none.
func SandboxRuns(t testing.TB) bool {
	t.Helper()
	if runtime.GOOS != "linux" {
		return true
	}
	unshare := prereqtest.OnPath(t, "unshare", "it tells whether Chromium's own sandbox can start on this host",
		"install util-linux, which every Linux distribution ships")
	ctx, cancel := context.WithTimeout(t.Context(), psTimeout)
	defer cancel()
	return exec.CommandContext(ctx, unshare, "--user", "--map-root-user", "true").Run() == nil
}

// SandboxCause is the first cause this host shows for a sandbox that
// [SandboxRuns] says cannot start, read here from what the kernel says, so a
// case holds the cause the helper names to it: root; Ubuntu's AppArmor
// restriction on unprivileged user namespaces; user namespaces turned off; a
// seccomp filter on this process, which a box's processes inherit; else
// unknown.
func SandboxCause() browser.EnsureCause {
	read := func(file string) string {
		raw, err := os.ReadFile(file)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(raw))
	}
	switch {
	case os.Getuid() == 0:
		return browser.CauseRoot
	case read("/proc/sys/kernel/apparmor_restrict_unprivileged_userns") == "1":
		return browser.CauseAppArmor
	case read("/proc/sys/user/max_user_namespaces") == "0" || read("/proc/sys/kernel/unprivileged_userns_clone") == "0":
		return browser.CauseUserns
	case slices.Contains(strings.Split(read("/proc/self/status"), "\n"), "Seccomp:\t2"):
		return browser.CauseSeccomp
	}
	return browser.CauseUnknown
}
