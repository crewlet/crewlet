package mcp

import (
	"fmt"
	"log/slog"
	"os/exec"
	"slices"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/procgroup"
)

// childProcess is the stdio half of a client: the process and the pipe its
// last words come out of.
type childProcess struct {
	cmd   *exec.Cmd
	relay *stderrRelay
}

// groupPID is the process group to signal, read at signal time.
//
// DERIVED rather than captured, because the one path where a package runner's
// grandchild is most likely to have survived is a server that never came up —
// and a field assigned only after a successful handshake is zero exactly
// there. procgroup.Kill(0) is refused by design and returns nil, so the reap
// logged success while signalling nothing at all.
//
// The group leader is the child itself: procgroup.Set puts it in its own
// group at fork, so its pid IS the group id. Zero when the process never
// started, which is the one case there is genuinely nothing to signal.
func (c *childProcess) groupPID() int {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

// newStdioTransport builds the child and the transport that will start it.
//
// It does NOT start anything: the SDK's CommandTransport calls Start inside
// Connect, which is also what makes the startup deadline meaningful (spawning
// the process is inside the window, not before it).
func newStdioTransport(spec Spec, log *slog.Logger) (sdk.Transport, *childProcess, error) {
	relay, err := newStderrRelay(spec.Name, log)
	if err != nil {
		return nil, nil, fmt.Errorf("mcp: server %q: stderr pipe: %w", spec.Name, err)
	}

	// exec.Command, never exec.CommandContext. The context here carries the
	// STARTUP deadline, and CommandContext would kill the server the moment
	// that deadline passed — that is, kill every healthy server 120 seconds
	// after it came up. The child's lifetime is owned by Stop.
	cmd := exec.Command(spec.Command, spec.Args...) //nolint:gosec,noctx // the command IS the operator's config; the deliberate lack of a context is explained above
	cmd.Env = mergedEnv(spec, log)
	cmd.Stderr = relay.writer()
	procgroup.Set(cmd)

	return &sdk.CommandTransport{
		Command:           cmd,
		TerminateDuration: shutdownGrace,
	}, &childProcess{cmd: cmd, relay: relay}, nil
}

// mergedEnv is the child's environment: the allowlisted host environment, the
// engine user's own locations, and the server's declared variables on top.
//
// AN ALLOWLIST, NOT THE ENGINE'S ENVIRONMENT. This used to be os.Environ() with
// the declared variables layered over it, on the reasoning that servers read
// undeclared conventional variables and narrowing the set would break them.
// What that handed every tool server was the engine's keyring (which signs
// every session cookie and every state-log record), every Tier A token value
// and the identity provider's client secret — to a process the company pulled
// off a package registry, which logs its environment on a crash, forwards it
// to its own children and reports it in telemetry. So a child gets exactly
// what [hostbox] gives every child the engine starts — where to find binaries,
// how to render text and talk TLS, how to reach the network — plus
// [hostbox.HostUserEnv], because a server launched through `npx` or `uvx` runs
// as the engine's user in no box of its own and keeps its package cache under
// that user's HOME. Anything else a server reads is DECLARED in its `env:` (or
// a seat's `mcp_env`), and a `${VAR}` there resolves from the secret store and
// then the process environment, so passing one of the host's own variables
// through is one line of configuration that says so.
//
// IT IS NOT ISOLATION, and nothing here pretends it is: the child runs as the
// engine's user, and a process running as that user can read what that user
// can — the Tier A file, and the engine's own /proc/<pid>/environ. What the
// allowlist removes is the engine HANDING its secrets to code that never asked
// for them; keeping a hostile server away from them is a different user or a
// container.
//
// A SERVER WHOSE COMMAND IS `docker` OR `podman` IS THE RUNTIME'S CLI, and it
// gets what the runtime's CLI gets everywhere else ([hostbox.ContainerRuntime]):
// the DOCKER_ family, Podman's CONTAINER_/CONTAINERS_/PODMAN_ families and
// the rest. A tool server shipped as an image (`docker run -i --rm …`) is the
// commonest way one is published, and handed only the host user's locations
// a rootless runtime lost DOCKER_HOST and dialled the system socket — a
// permission error at the daemon that named nothing the operator had set.
//
// Note what it does NOT do either: secret-store values are not poured in.
// spec.Env has already had its ${VAR} references resolved by the caller, so a
// server receives exactly the stored credentials its own config declares and
// no others. Injecting the whole store here would hand every seat's token to
// every subprocess in the company.
func mergedEnv(spec Spec, log *slog.Logger) []string {
	env := hostbox.Inherit(hostbox.HostUserEnv...)
	if hostbox.IsContainerRuntime(spec.Command) {
		env = hostbox.ContainerRuntime()
	}
	keys := make([]string, 0, len(spec.Env))
	var empty []string
	for k, v := range spec.Env {
		env[k] = v
		keys = append(keys, k)
		if v == "" {
			empty = append(empty, k)
		}
	}
	slices.Sort(keys)
	slices.Sort(empty)

	if len(empty) > 0 {
		// Almost always an unresolved ${VAR}: the server will come up, fail
		// to authenticate, and report something unrelated. Say it here, where
		// the variable name is still in hand.
		log.Warn("empty_env_vars", "server", spec.Name, "empty_keys", empty)
	}
	// Keys only, never values — this line exists to debug a missing variable,
	// and the value is the credential.
	log.Debug("custom_env_keys", "server", spec.Name, "keys", keys)

	return hostbox.Environ(env)
}
