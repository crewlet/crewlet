package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/hostbox"
)

// A container box's home is an engine-host directory bind-mounted at
// [DefaultHome]: file I/O needs no copy, but the engine and the box share one
// filesystem. Two properties must hold, and both fail silently:
//
//  1. The mount is THIS host's filesystem. A runtime driving a remote daemon
//     (DOCKER_HOST, a forwarded socket) mounts the daemon's instead, and the
//     agent finds no brief, credentials or tools.
//  2. The engine can manage what the box writes. Under a rootful runtime the
//     box's directories are root-owned, so `Install` cannot write into them
//     (EACCES) and `removeBox` cannot empty them: box directories leak.

// containerProbeTimeout bounds the rootless probe and the in-box half of the
// mount proof. Shorter than [ControlTimeout] on purpose: a daemon too slow for
// it would fail the create anyway, so a longer bound only delays the error.
// Ten seconds is far past a healthy `docker info` (sub-second).
const containerProbeTimeout = 10 * time.Second

// containerUserArgs is the `--user` the container needs so it writes the
// mount as the engine's own user, or nothing.
//
// A rootless runtime already maps container root onto the invoking user, and
// --user there would map into the subuid range, unable to enter the box's 0700
// directory. A rootful runtime needs --user, or the box writes root-owned
// files the engine cannot reclaim. An unreadable probe answers rootful, the
// loud direction: the mount proof then fails at creation naming the cause,
// where the opposite mistake silently leaks box directories.
func containerUserArgs(ctx context.Context, cli containerCLI) []string {
	if runtimeIsRootless(ctx, cli) {
		return nil
	}
	return []string{"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())}
}

// runtimeIsRootless asks the runtime whether it maps container root onto this
// user. Docker lists it in SecurityOptions, Podman in host.security.rootless,
// and no one format string reads both, so the template follows the binary
// [ResolveContainerRuntime] picked: a single-value template, which every
// version of both supports, not a whole-document dump. A down daemon or an
// unrecognised answer is no answer: false, the caller's default.
func runtimeIsRootless(ctx context.Context, cli containerCLI) bool {
	format := "{{.SecurityOptions}}"
	if cli.podman() {
		format = "{{.Host.Security.Rootless}}"
	}
	result, err := runHost(ctx, cli.command(containerProbeTimeout,
		"info", "--format", format))
	if err != nil || result.ExitCode != 0 {
		// The error and stderr are logged: "could not start the binary"
		// and "the daemon answered nothing" have different fixes, and a
		// refusing runtime says why only on stderr.
		localLog.Debug("local_sandbox_rootless_probe_unanswered",
			"runtime", cli.path, "exit", result.ExitCode,
			"error", err, "stderr", strings.TrimSpace(result.Stderr))
		return false
	}
	answer := strings.TrimSpace(result.Stdout)
	// Podman answers a boolean, Docker a list holding "rootless" or not;
	// "false" is Podman's no, never a Docker list.
	if answer == "true" {
		return true
	}
	return answer != "false" && strings.Contains(answer, "rootless")
}

// execer is the one thing the mount proof needs from a box.
type execer interface {
	Exec(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error)
}

// The mount probe's exits, one per half of its command, distinct from a
// runtime's (125 to 127) and a box's (124 timeout, above 128 signal).
const (
	// probeUnread is the probe the engine wrote not being there to read.
	probeUnread = 3
	// probeUnmade is the box unable to make a directory in its own home.
	probeUnmade = 4
)

// probeFailed is what the mount probe's answer says, or nil for a probe that
// read the engine's own token back.
//
// Each failure gets its own class and words, since each sends the reader
// somewhere else: a token not read back is a mount on another host's
// filesystem; an unmade directory is a mount the box may not write; a timeout
// is a runtime not answering, which a wait can clear; any other exit is the
// runtime refusing the exec; and a runtime this host cannot start is the
// engine host's own fault, no class at all.
func probeFailed(ctx context.Context, layout boxLayout, token string, result ExecResult, err error) error {
	said := strings.TrimSpace(result.Stderr)
	switch {
	case err == nil && result.ExitCode == 0 && strings.TrimSpace(result.Stdout) == token:
		return nil
	case ctx.Err() != nil:
		// The caller stopped waiting; not to be read as a timed-out probe.
		return fmt.Errorf("container sandbox %s: its mount probe was abandoned: %w",
			layout.id, ctx.Err())
	case err != nil:
		return localErrorf("container sandbox %s could not run its mount probe, because "+
			"this host could not run the container runtime: %v", layout.id, err)
	case result.ExitCode == 124:
		return localRefusedf(WhyProviderUnavailable, "container sandbox %s did not answer "+
			"its mount probe in %s: %s", layout.id, containerProbeTimeout, said)
	case result.ExitCode == probeUnread, result.ExitCode == 0:
		return localRefusedf(WhyConfiguration, "container sandbox %s cannot see its own box directory: %s is "+
			"mounted at %s but the container reads something else there. The runtime is "+
			"driving a daemon on another host (DOCKER_HOST, a forwarded socket), which "+
			"cannot bind-mount this filesystem — point providers.sandbox.local.runtime at "+
			"a local daemon", layout.id, layout.home(), DefaultHome)
	case result.ExitCode == probeUnmade:
		return localRefusedf(WhyConfiguration, "container sandbox %s cannot make a directory in "+
			"its own home: %s is mounted at %s, and the container may not write there (%s). "+
			"Run the container as a user that can, with providers.sandbox.local.run_args, "+
			"or mount the box directory writable", layout.id, layout.home(), DefaultHome, said)
	}
	return localRefusedf(WhyConfiguration, "the container runtime refused to run container "+
		"sandbox %s's mount probe (exit %d): %s", layout.id, result.ExitCode, said)
}

// verifyMount proves the two properties above with the operations that break
// when they do not hold. The engine writes a token the box must read back;
// then the box makes a directory the engine must write into and remove,
// `Install` and `removeBox` in miniature. A directory, not a file: unlinking
// needs write permission on the parent, so removing a root-owned file proves
// nothing. Every file goes through [boxLayout.openHome], since the container
// is running and made the directory written into.
func verifyMount(ctx context.Context, box execer, layout boxLayout) error {
	home, err := layout.openHome()
	if err != nil {
		return localErrorf("container sandbox %s could not prepare %s: %v",
			layout.id, layout.meta(), err)
	}
	defer func() { _ = home.Close() }()
	if err = home.MkdirAll(metaName, hostbox.DirMode); err != nil {
		return localErrorf("container sandbox %s could not prepare %s: %v",
			layout.id, layout.meta(), err)
	}
	token := uuid.NewString()
	fromEngine := filepath.Join(metaName, "mount-probe")
	if err = replaceIn(home, fromEngine, []byte(token)); err != nil {
		return localErrorf("container sandbox %s could not write its mount probe: %v",
			layout.id, err)
	}
	defer func() { _ = home.Remove(fromEngine) }()

	// Unquoted: every path is a constant under DefaultHome, with no operator
	// input. Each half has its own exit, telling a probe that read something
	// else from one the runtime never ran.
	inBox := DefaultHome + "/.crewlet"
	result, err := box.Exec(ctx, "cat "+inBox+"/mount-probe || exit "+
		strconv.Itoa(probeUnread)+"; mkdir -p "+inBox+"/mount-probe.d || exit "+
		strconv.Itoa(probeUnmade), ExecOptions{
		// Not the workspace, which a setup step may still be creating.
		Cwd:        DefaultHome,
		TimeoutSec: containerProbeTimeout.Seconds(),
	})
	if failed := probeFailed(ctx, layout, token, result, err); failed != nil {
		return failed
	}

	// The box made this directory; the engine now has to live in it.
	made := filepath.Join(metaName, "mount-probe.d")
	writeErr := replaceIn(home, filepath.Join(made, "from-engine"), []byte(token))
	removeErr := home.RemoveAll(made)
	if writeErr != nil || removeErr != nil {
		detail := writeErr
		if detail == nil {
			detail = removeErr
		}
		return localRefusedf(WhyConfiguration, "container sandbox %s writes its box directory as a user this "+
			"engine cannot manage: the container created %s and this process can neither "+
			"write into it nor remove it (%v). The engine would be left unable to install "+
			"a run's files or reclaim the box's disk. Run the container as this user by "+
			"passing `--user %d:%d` in providers.sandbox.local.run_args, or use a rootless "+
			"container runtime",
			layout.id, filepath.Join(layout.home(), made), detail, os.Getuid(), os.Getgid())
	}
	return nil
}
