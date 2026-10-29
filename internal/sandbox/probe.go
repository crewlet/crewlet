package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// A CELL PROBE answers "can a box start in this cell on this node, now?" for
// the Models page's "where it runs" step and for a person reading why a run did
// not start. Each probe asks the cell's own mechanism, through the same
// functions its backend uses, so the probe and the launch cannot disagree about
// what the cell needs:
//
//   - direct: the platform runs the local backend, and the box root
//     ([BoxRoot]) is writable;
//   - container: the runtime resolves on PATH ([ResolveContainerRuntime]) and
//     its daemon answers a version read within [CellProbeBudget];
//   - e2b: the cell's API key resolved on this node, and one authenticated
//     read of the API answers within [CellProbeBudget].
//
// A probe never makes a box. Every refusal carries the one thing to do about
// it, in the operator's words.

// CellProbeBudget bounds a probe's one network or daemon round trip: FIVE
// SECONDS, past a healthy daemon's version read (milliseconds) and a cloud
// API's p99, and short enough that a probe of every cell on a node that has
// lost its network still finishes well inside one heartbeat.
const CellProbeBudget = 5 * time.Second

// CellProbe is what one probe found.
type CellProbe struct {
	// Ready is whether a box can start in the cell here.
	Ready bool
	// Detail is what the probe found, ready or not: "Docker 24.0.7", or the
	// refusal.
	Detail string
	// Fix is the one thing to do about a cell that is not ready; empty for
	// one that is.
	Fix string
}

// ProbeDirect probes the direct cell: a platform the local backend runs on,
// and a box root it can write.
//
// NOT THE HOST CONFINEMENT. An executor's direct box is not confined (its CLI
// runs fully permissioned, and the box is its home and working tree); only the
// assistant's deep work runs under [ProbeConfinement]'s mechanism. Probing it
// here would call a cell that launches fine not ready on every host without
// bubblewrap or Landlock.
func ProbeDirect(stateDir string) CellProbe {
	if !localSupported {
		return CellProbe{Detail: unsupportedReason,
			Fix: "Run the engine on Linux or macOS to use run_in: direct, or use another cell"}
	}
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
	// A runtime that forks a helper holding the output open would keep the
	// read going past the budget; WaitDelay ends it with the context.
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

// ProbeE2B probes the e2b cell: the key it would authenticate with, and one
// read of the API with it.
func ProbeE2B(ctx context.Context, opts E2BOptions) CellProbe {
	if strings.TrimSpace(opts.APIKey) == "" {
		return CellProbe{Detail: "providers.sandbox.e2b.api_key resolves to nothing on this node",
			Fix: "Set the e2b API key's variable under Settings › Secrets"}
	}
	ctx, cancel := context.WithTimeout(ctx, CellProbeBudget)
	defer cancel()
	api := newE2BAPI(opts.APIKey, opts.Domain, opts.HTTP)
	if err := api.listBoxes(ctx, 1); err != nil {
		var refused *E2BError
		switch {
		case errors.As(err, &refused) && (refused.Status == http.StatusUnauthorized || refused.Status == http.StatusForbidden):
			return CellProbe{Detail: err.Error(),
				Fix: "Replace the e2b API key under Settings › Secrets: the API refused it"}
		case ctx.Err() != nil:
			return CellProbe{Detail: fmt.Sprintf("the e2b API at %s did not answer within %s", api.base, CellProbeBudget),
				Fix: "Check this node's network route to the e2b API"}
		}
		return CellProbe{Detail: err.Error(), Fix: "Check this node's network route to the e2b API"}
	}
	return CellProbe{Ready: true, Detail: "the e2b API at " + api.domain + " answers with this key"}
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
