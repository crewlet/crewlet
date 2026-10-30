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

// The bind mount, and the two things container mode is built on.
//
// A container box's home is a directory on the engine host mounted at [DefaultHome]
// inside a container. That is what makes in-box paths identical to a remote
// backend's and file I/O a plain host read or write with no copy round trip —
// and it means TWO PROCESSES SHARE ONE FILESYSTEM. Both properties that has to
// hold fail silently, and both were live:
//
//  1. The mount must be THIS host's filesystem. A runtime driving a remote
//     daemon (DOCKER_HOST, a rootless socket forwarded from elsewhere) mounts
//     the DAEMON's filesystem instead, so the box directory the engine wrote
//     is not the one the container sees. Nothing errors; the agent simply
//     finds no brief, no credentials and no tools.
//
//  2. What the container writes there, the engine must be able to manage.
//     Under a ROOTFUL runtime a container's root is the host's root, so every
//     directory the box creates in the mount is root-owned: the engine, which
//     is not root, can then neither write into it (`Install` creating
//     .crewlet/bin from inside the box and then writing a file into it from
//     outside failed with EACCES, which is exactly how this was found) nor
//     unlink from it, so `removeBox`'s RemoveAll leaves the checkout behind
//     and every box directory leaks on the engine host for good.

// containerProbeTimeout bounds the runtime queries around a box's creation:
// the rootless probe and the in-box half of the mount proof.
//
// Short on purpose, and shorter than [ControlTimeout]. Both run against a
// daemon this process is about to ask for a container anyway, so a daemon slow
// enough to blow this is a create that is about to fail regardless — the only
// thing a longer bound would buy is a later error. Ten seconds is well past a
// healthy `docker info` (sub-second) and well under the minute a wedged daemon
// would otherwise add to a doomed create.
const containerProbeTimeout = 10 * time.Second

// containerUserArgs is the `--user` the container needs, or nothing.
//
// THE CONTAINER MUST WRITE THE MOUNT AS THE ENGINE'S OWN USER, and which flag
// achieves that depends on the runtime rather than on anything Crewlet chooses:
//
//   - A ROOTLESS runtime (podman's default on Fedora and RHEL, rootless
//     Docker) already does it. Its user namespace maps container uid 0 onto
//     the invoking user, so the container's root IS the engine user on disk —
//     and passing --user there would map the id into the subuid range instead,
//     turning a working configuration into one that cannot even enter the
//     box's own 0700 directory.
//
//   - A ROOTFUL runtime does not. Container uid 0 is host uid 0, so --user is
//     what stops the box writing root-owned files into a directory the engine
//     has to be able to reclaim.
//
// An unreadable probe answers ROOTFUL, which is the loud direction: --user
// against a rootless runtime fails the mount proof at creation with a message
// that names the cause, while the opposite mistake is silent until an
// operator wonders why the engine host is full of undeletable box directories.
func containerUserArgs(ctx context.Context, cli containerCLI) []string {
	if runtimeIsRootless(ctx, cli) {
		return nil
	}
	return []string{"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())}
}

// runtimeIsRootless asks the runtime whether it maps container root onto this
// user.
//
// ONE QUESTION, ASKED THE WAY EACH RUNTIME ANSWERS IT. Docker reports it among
// its top-level SecurityOptions, Podman as a boolean at host.security.rootless,
// and there is no format string that reads both — so the query is chosen by
// which binary [ResolveContainerRuntime] picked, using a plain single-value
// template rather than a whole-document dump, which is what every version of
// both CLIs supports.
//
// Anything else — a daemon that is down, an answer this build does not
// recognise — is not an answer, and the caller's default covers it.
func runtimeIsRootless(ctx context.Context, cli containerCLI) bool {
	format := "{{.SecurityOptions}}"
	if cli.podman() {
		format = "{{.Host.Security.Rootless}}"
	}
	result, err := runHost(ctx, cli.command(containerProbeTimeout,
		"info", "--format", format))
	if err != nil || result.ExitCode != 0 {
		// THE ERROR TRAVELS, and it is not decoration: "could not start
		// the binary" and "the daemon answered nothing" are different
		// faults with different fixes, and dropping the first left the
		// only evidence of one being a container that came up with
		// --user against a rootless runtime. Stderr too, because a
		// runtime that refuses says why there and nowhere else.
		localLog.Debug("local_sandbox_rootless_probe_unanswered",
			"runtime", cli.path, "exit", result.ExitCode,
			"error", err, "stderr", strings.TrimSpace(result.Stderr))
		return false
	}
	answer := strings.TrimSpace(result.Stdout)
	// Podman answers the boolean itself; Docker answers a list in which the
	// option is present or absent. "false" is Podman saying no, and must not
	// be read as a Docker list that happens to mention the word.
	if answer == "true" {
		return true
	}
	return answer != "false" && strings.Contains(answer, "rootless")
}

// execer is the one thing the mount proof needs from a box.
type execer interface {
	Exec(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error)
}

// The mount probe's own exits, one per half of its command, which no exit of
// a container runtime (125 to 127) or of a box (124 for its timeout, above
// 128 for a signal) is.
const (
	// probeUnread is the probe the engine wrote not being there to read.
	probeUnread = 3
	// probeUnmade is the box unable to make a directory in its own home.
	probeUnmade = 4
)

// probeFailed is what the mount probe's answer says, or nil for a probe that
// ran and read the engine's own token back.
//
// FOUR FAILURES, EACH WITH ITS OWN CLASS AND ITS OWN WORDS, because they send
// a reader to four different places. A probe that RAN and read something else
// — the file not there, or other bytes in it — is a mount on another host's
// filesystem, a configuration. A box that could not make a directory in its
// own home is a mount it may not write, a configuration too. A probe that ran
// out of its time is the runtime not answering, which a wait can clear. A
// probe the runtime refused to run at all — the container gone, an exec the
// daemon would not take — is the runtime's refusal, a configuration as every
// one of its refusals is. And a runtime this host could not even start is no
// class at all, but the engine host's own fault. Every one of them was told
// as the first, so an exec that never happened sent an operator to look for a
// remote daemon that was not there.
func probeFailed(ctx context.Context, layout boxLayout, token string, result ExecResult, err error) error {
	said := strings.TrimSpace(result.Stderr)
	switch {
	case err == nil && result.ExitCode == 0 && strings.TrimSpace(result.Stdout) == token:
		return nil
	case ctx.Err() != nil:
		// THE CALLER STOPPED WAITING, which a timed-out probe would
		// otherwise be read as: the box is torn down either way, and the
		// launch says why.
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
			"a local daemon, or use the %q sandbox provider for a remote runtime",
			layout.id, layout.home(), DefaultHome, E2BKind)
	case result.ExitCode == probeUnmade:
		return localRefusedf(WhyConfiguration, "container sandbox %s cannot make a directory in "+
			"its own home: %s is mounted at %s, and the container may not write there (%s). "+
			"Run the container as a user that can, with providers.sandbox.local.run_args, "+
			"or mount the box directory writable", layout.id, layout.home(), DefaultHome, said)
	}
	return localRefusedf(WhyConfiguration, "the container runtime refused to run container "+
		"sandbox %s's mount probe (exit %d): %s", layout.id, result.ExitCode, said)
}

// verifyMount proves the two properties above, using the operations that
// actually break when they do not hold.
//
// The engine writes a token and the box reads it back: a mount that is not
// this filesystem cannot. Then the BOX creates a directory and the ENGINE
// writes a file inside it and removes the lot — which is `Install` and
// `removeBox` in miniature, and is why the check is a directory rather than a
// file. Unlinking a file needs write permission on its PARENT, not on the
// file, so an engine that removes a root-owned file proves nothing at all; it
// is the box-made DIRECTORY that the engine cannot write into or empty.
//
// EVERY FILE OF IT THROUGH THE BOX'S HOME ([boxLayout.openHome]): the container
// is running by now, and the directory the proof writes into is one the
// container made.
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

	// Unquoted, because every path here is a compile-time constant under
	// DefaultHome — there is no operator input in this command line. Each
	// half exits with a status of its own, so a probe that ran and read
	// something else is told apart from one the runtime never ran.
	inBox := DefaultHome + "/.crewlet"
	result, err := box.Exec(ctx, "cat "+inBox+"/mount-probe || exit "+
		strconv.Itoa(probeUnread)+"; mkdir -p "+inBox+"/mount-probe.d || exit "+
		strconv.Itoa(probeUnmade), ExecOptions{
		// Not the workspace: the proof must not depend on a working
		// directory a setup step could still be creating.
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
