package mcp

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

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

// mergedEnv is the child's environment: everything this process has, with the
// server's own declared variables layered on top.
//
// WHOLE-ENVIRONMENT INHERITANCE IS DELIBERATE. MCP servers routinely read
// undeclared conventional variables — PATH, HOME, the proxy variables, a
// vendor SDK's own key — so narrowing this to the declared set breaks servers
// that work today, in a way that surfaces as a server failing to start with an
// error from someone else's code.
//
// Note what it does NOT do: secret-store values are not poured in. spec.Env
// has already had its ${VAR} references resolved by the caller, so a server
// receives exactly the stored credentials its own config declares and no
// others. Injecting the whole store here would hand every seat's token to
// every subprocess in the company, which is strictly worse than the problem it
// would solve.
func mergedEnv(spec Spec, log *slog.Logger) []string {
	env := make(map[string]string, len(os.Environ())+len(spec.Env))
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
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
	sort.Strings(keys)
	sort.Strings(empty)

	if len(empty) > 0 {
		// Almost always an unresolved ${VAR}: the server will come up, fail
		// to authenticate, and report something unrelated. Say it here, where
		// the variable name is still in hand.
		log.Warn("empty_env_vars", "server", spec.Name, "empty_keys", empty)
	}
	// Keys only, never values — this line exists to debug a missing variable,
	// and the value is the credential.
	log.Debug("custom_env_keys", "server", spec.Name, "keys", keys)

	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	// Sorted so a child's environment is reproducible across runs, which is
	// the difference between a comparable log line and a new one every boot.
	slices.Sort(out)
	return out
}
