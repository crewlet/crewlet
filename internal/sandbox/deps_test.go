package sandbox_test

import (
	"bytes"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// THE SANDBOX LINKS NOTHING OF GITLET, NO CONFIGURATION AND NO STATE LOG, which
// would pull the engine's configuration or the log's whole graph into all that
// drives a box. What it never reads it carries as bytes (PendingRun.CodeFloor and
// PushRecord.At as a position's token, grants as the strings Gitlet resolved),
// and it seals answers through internal/recordseal (ADR-0031), not the log.
//
// Two halves, each able to fail alone: the LINK from the non-test build's `go
// list -deps`, so a log reached through another package is refused; the NAMES
// from every file, tests included, since a test building a statelog value marks
// an API that speaks it. Mutation: import internal/changefeed from a non-test
// file (red on the link), name statelog.ReadSession in pushed_test.go (names),
// or import internal/config for a default (red).
func TestTheSandboxLinksNoGitletConfigurationOrStateLog(t *testing.T) {
	t.Parallel()
	const module = "github.com/crewlet/crewlet/internal/"
	refused := []string{"gitlet", "config", "statelog"}
	under := func(pkg, root string) bool {
		return pkg == module+root || strings.HasPrefix(pkg, module+root+"/")
	}

	cmd := exec.CommandContext(t.Context(), "go", "list", "-deps", "-f", "{{.ImportPath}}", ".")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v: %s", err, stderr.String())
	}
	listed := 0
	for pkg := range strings.Lines(string(out)) {
		pkg = strings.TrimSpace(pkg)
		listed++
		for _, root := range refused {
			if under(pkg, root) {
				t.Errorf("internal/sandbox links %s", pkg)
			}
		}
	}
	if !strings.Contains(string(out), module+"sandbox\n") || listed < 10 {
		t.Fatalf("go list -deps named %d packages and not internal/sandbox among them: it read something else",
			listed)
	}

	// THE NAMES, tests included.
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	read := 0
	for _, name := range files {
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		read++
		for _, imp := range file.Imports {
			if path, _ := strconv.Unquote(imp.Path.Value); under(path, "statelog") {
				t.Errorf("%s: internal/sandbox names %s: carry a position as its token, and let the engine "+
					"read it; seal through internal/recordseal", fset.Position(imp.Pos()), path)
			}
		}
	}
	if read < 10 {
		t.Fatalf("the walk read %d of the package's files: it read something else", read)
	}
}
