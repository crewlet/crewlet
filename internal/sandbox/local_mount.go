package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/hostbox"
)

// The bind mount, and the two things container mode is built on.
//
// A container box is a directory on the engine host mounted at [DefaultHome]
// inside a container. That is what makes in-box paths identical to a remote
// backend's and file I/O a plain host read or write with no copy round trip —
// and it means TWO PROCESSES SHARE ONE FILESYSTEM. Both properties that has to
// hold fail silently, and both were live:
//
//  1. The mount must be THIS host's filesystem. A runtime driving a remote
//     daemon (DOCKER_HOST, a rootless socket forwarded from elsewhere) mounts
//     the DAEMON's filesystem instead, so the box directory the engine wrote
//     is not the one the container sees. Nothing errors; the agent simply
//     finds no brief, no credentials and no shim.
//
//  2. What the container writes there, the engine must be able to manage.
//     Under a ROOTFUL runtime a container's root is the host's root, so every
//     directory the box creates in the mount is root-owned: the engine, which
//     is not root, can then neither write into it — `Install` creating
//     .crewlet/bin from inside the box and then writing the ask shim from
//     outside failed with EACCES, which is exactly how this was found — nor
//     unlink from it, so `removeBox`'s RemoveAll leaves the checkout behind
//     and every box directory leaks on the engine host for good.

// containerProbeTimeout bounds the runtime queries around a box's creation:
// the rootless probe and the in-box half of the mount proof.
//
// Short on purpose, and shorter than [controlTimeout]. Both run against a
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
func containerUserArgs(ctx context.Context, runtime string) []string {
	if runtimeIsRootless(ctx, runtime) {
		return nil
	}
	return []string{"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())}
}

// runtimeIsRootless asks the runtime whether it maps container root onto this
// user.
//
// One query, decoded permissively, because Docker and Podman report it in
// different places and there is no format string that reads both: Docker
// carries `name=rootless` among its top-level SecurityOptions, Podman a
// boolean at host.security.rootless. Anything else — a daemon that is down, an
// `info` this build cannot parse — is not an answer, and the caller's default
// covers it.
func runtimeIsRootless(ctx context.Context, runtime string) bool {
	result, err := runHost(ctx, hostCommand{
		argv:    []string{runtime, "info", "--format", "{{json .}}"},
		timeout: containerProbeTimeout,
	})
	if err != nil || result.ExitCode != 0 {
		log.Debug("local_sandbox_rootless_probe_unanswered",
			"runtime", runtime, "exit", result.ExitCode)
		return false
	}
	var info struct {
		SecurityOptions []string `json:"SecurityOptions"` // Docker
		Host            struct { // Podman
			Security struct {
				Rootless bool `json:"rootless"`
			} `json:"security"`
		} `json:"host"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &info); err != nil {
		log.Debug("local_sandbox_rootless_probe_unparsed", "runtime", runtime)
		return false
	}
	if info.Host.Security.Rootless {
		return true
	}
	for _, option := range info.SecurityOptions {
		if strings.Contains(option, "rootless") {
			return true
		}
	}
	return false
}

// execer is the one thing the mount proof needs from a box.
type execer interface {
	Exec(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error)
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
func verifyMount(ctx context.Context, box execer, layout boxLayout) error {
	if err := os.MkdirAll(layout.meta(), hostbox.DirMode); err != nil {
		return localErrorf("container sandbox %s could not prepare %s: %v",
			layout.id, layout.meta(), err)
	}
	token := uuid.NewString()
	fromEngine := filepath.Join(layout.meta(), "mount-probe")
	if err := os.WriteFile(fromEngine, []byte(token), hostbox.FileMode); err != nil {
		return localErrorf("container sandbox %s could not write its mount probe: %v",
			layout.id, err)
	}
	defer func() { _ = os.Remove(fromEngine) }()

	// Unquoted, because every path here is a compile-time constant under
	// DefaultHome — there is no operator input in this command line.
	inBox := DefaultHome + "/.crewlet"
	result, err := box.Exec(ctx, "cat "+inBox+"/mount-probe"+
		" && mkdir -p "+inBox+"/mount-probe.d", ExecOptions{
		// Not the workspace: the proof must not depend on a working
		// directory a setup step could still be creating.
		Cwd:        DefaultHome,
		TimeoutSec: containerProbeTimeout.Seconds(),
	})
	if err != nil || result.ExitCode != 0 || strings.TrimSpace(result.Stdout) != token {
		return localErrorf("container sandbox %s cannot see its own box directory: %s is "+
			"mounted at %s but the container reads something else there. The runtime is "+
			"driving a daemon on another host (DOCKER_HOST, a forwarded socket), which "+
			"cannot bind-mount this filesystem — point providers.sandbox.local.runtime at "+
			"a local daemon, or use the %q sandbox provider for a remote runtime",
			layout.id, layout.root, DefaultHome, E2BKind)
	}

	// The box made this directory; the engine now has to live in it.
	made := filepath.Join(layout.meta(), "mount-probe.d")
	writeErr := os.WriteFile(filepath.Join(made, "from-engine"), []byte(token), hostbox.FileMode)
	removeErr := os.RemoveAll(made)
	if writeErr != nil || removeErr != nil {
		detail := writeErr
		if detail == nil {
			detail = removeErr
		}
		return localErrorf("container sandbox %s writes its box directory as a user this "+
			"engine cannot manage: the container created %s and this process can neither "+
			"write into it nor remove it (%v). The engine would be left unable to install "+
			"the agent's shim or reclaim the box's disk. Run the container as this user by "+
			"passing `--user %d:%d` in providers.sandbox.local.run_args, or use a rootless "+
			"container runtime",
			layout.id, made, detail, os.Getuid(), os.Getgid())
	}
	return nil
}
