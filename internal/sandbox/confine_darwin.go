//go:build darwin

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// sandboxExec is where macOS ships sandbox-exec. An absolute path, never a
// PATH lookup: a directory earlier on the engine's PATH must not be able to
// stand in for the boundary.
const sandboxExec = "/usr/bin/sandbox-exec"

// confineProbeTimeout bounds the trial run a probe makes: one process start.
const confineProbeTimeout = 10 * time.Second

// ProbeConfinement reports the strongest confinement this host enforces: on
// macOS, sandbox-exec, proven by running a trivial job under a profile.
func ProbeConfinement() (*HostConfinement, error) {
	if _, err := os.Stat(sandboxExec); err != nil {
		return nil, &ConfinementUnavailableError{Reason: sandboxExec + " is not present"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), confineProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, sandboxExec, "-p",
		"(version 1)(allow default)(deny file-write* (subpath \"/\"))", "/usr/bin/true").CombinedOutput()
	if err != nil {
		return nil, &ConfinementUnavailableError{Reason: fmt.Sprintf(
			"sandbox-exec could not apply a profile: %s", strings.TrimSpace(string(out)+" "+err.Error()))}
	}
	return &HostConfinement{Mechanism: MechanismSandboxExec}, nil
}

// ConfineMain is the Landlock helper, which macOS does not have.
func ConfineMain(args []string) int {
	fmt.Fprintln(os.Stderr, "crewlet "+ConfineCommand+": Landlock is a Linux mechanism; "+
		"this host confines with sandbox-exec")
	return 2
}
