package sandbox_test

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// THE SANDBOX LINKS NO LOG, NO STORE AND NO CONFIGURATION. It carries what it
// never reads as bytes: a run's code floor as a statelog position's token
// (PendingRun.CodeFloor), its grants as the strings the code host resolved,
// its suspended conversation as raw JSON. Linking the code host, the state
// log, the store or the configuration to read any of them would bring the
// store's database driver and the engine's configuration into everything that
// drives a box, and decide nothing this package decides. Read from `go list
// -deps`: what its non-test build links.
//
// Mutation: parse the floor with statelog.ParsePosition in RaiseCodeFloor —
// red.
func TestTheSandboxLinksNoLogStoreOrConfiguration(t *testing.T) {
	t.Parallel()
	cmd := exec.CommandContext(t.Context(), "go", "list", "-deps", "-f", "{{.ImportPath}}", ".")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v: %s", err, stderr.String())
	}
	const module = "github.com/crewlet/crewlet/internal/"
	listed := 0
	for pkg := range strings.Lines(string(out)) {
		pkg = strings.TrimSpace(pkg)
		listed++
		for _, refused := range []string{"code", "statelog", "store", "config"} {
			if pkg == module+refused || strings.HasPrefix(pkg, module+refused+"/") {
				t.Errorf("internal/sandbox links %s", pkg)
			}
		}
	}
	if !strings.Contains(string(out), module+"sandbox\n") || listed < 10 {
		t.Fatalf("go list -deps named %d packages and not internal/sandbox among them: it read something else",
			listed)
	}
}
