package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// A CELL PROBE answers "can a box start in this cell on this node, now?" for
// the Models page and for a person reading why a run did not start. Each asks
// the cell's own mechanism through the functions its backend uses, so probe
// and launch cannot disagree about what the cell needs:
//
//   - direct: [BoxRoot] is writable;
//   - container: the runtime resolves on PATH ([ResolveContainerRuntime]) and
//     its daemon answers a version read within [CellProbeBudget].
//
// A probe never makes a box; every refusal names the one thing to do.

// CellProbeBudget bounds the container probe's one daemon round trip. A
// healthy daemon answers a version read in milliseconds, so a daemon silent
// for five seconds is down or wedged; it sits under the mount probe's budget
// (containerProbeTimeout), which runs a command in a box rather than reading
// a version.
const CellProbeBudget = 5 * time.Second

// CellProbe is what one probe found.
type CellProbe struct {
	// Ready is whether a box can start in the cell here.
	Ready bool
	// Detail is what the probe found: "Docker 24.0.7", or the refusal.
	Detail string
	// Fix is the one thing to do about a cell that is not ready; "" if ready.
	Fix string
}

// ProbeDirect probes the direct cell: a box root the engine can write.
func ProbeDirect(stateDir string) CellProbe {
	root := BoxRoot(stateDir)
	if err := writable(root); err != nil {
		return CellProbe{Detail: fmt.Sprintf("the box root %s is not writable: %v", root, err),
			Fix: "Make providers.sandbox.local.state_dir a directory the engine can write on this node"}
	}
	return CellProbe{Ready: true, Detail: "box root " + root + " writable"}
}

// writable makes dir if it is missing and proves a file can be created in it.
func writable(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// ProbeContainer probes the container cell: the runtime the backend would
// drive, and its daemon's answer to a version read.
func ProbeContainer(ctx context.Context, runtime string) CellProbe {
	path, err := ResolveContainerRuntime(runtime)
	if err != nil {
		return CellProbe{Detail: err.Error(),
			Fix: "Install Docker or Podman on this node and restart the engine: PATH is read at start"}
	}
	return probeRuntime(ctx, path, CellProbeBudget)
}

// probeRuntime asks the runtime at path for its daemon's version within budget.
func probeRuntime(ctx context.Context, path string, budget time.Duration) CellProbe {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version", "--format", "{{.Server.Version}}")
	// A forked helper holding the output open would outlast the budget;
	// WaitDelay ends the read with the context.
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	answer := strings.TrimSpace(string(out))
	switch {
	case ctx.Err() != nil:
		return CellProbe{Detail: fmt.Sprintf("%s did not answer a version read within %s", path, budget),
			Fix: "Start the container daemon on this node, or check that the engine's user may reach it"}
	case err != nil || answer == "":
		return CellProbe{Detail: fmt.Sprintf("%s version: %s", path, firstLineOf(answer, err)),
			Fix: "Start the container daemon on this node, or check that the engine's user may reach it"}
	}
	name := "Docker"
	if strings.Contains(filepath.Base(path), "podman") {
		name = "Podman"
	}
	return CellProbe{Ready: true, Detail: name + " " + answer}
}

// firstLineOf is a command's first line of output, or its error where it said
// nothing.
func firstLineOf(out string, err error) string {
	if line, _, _ := strings.Cut(out, "\n"); strings.TrimSpace(line) != "" {
		return strings.TrimSpace(line)
	}
	if err != nil {
		return err.Error()
	}
	return "no answer"
}
