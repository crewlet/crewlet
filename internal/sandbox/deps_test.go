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

// THE SANDBOX LINKS NOTHING OF GITLET, NO CONFIGURATION AND NO STATE LOG. It
// carries what it never reads as bytes: a run's code floor and its push
// record's position as a statelog position's token (PendingRun.CodeFloor,
// PushRecord.At), its grants as the strings Gitlet resolved. Linking
// any of the three to read them would bring the engine's configuration or
// the state log's whole graph into everything that drives a box, and decide
// nothing this package decides. The answer seal signs and verifies through
// internal/recordseal, the leaf the state log seals its own records through
// (ADR-0031), so sealing an answer is no reason to link a log either.
//
// Two halves, each able to fail alone. The LINK is read from `go list -deps`,
// what the package's non-test build links, so a log reached through another
// package is refused as much as one imported here. The NAMES are read from
// every source file of the package, its tests included: no file imports the
// state log or anything under it, so a test that builds a statelog value to
// hand the package — the sign of an API that speaks the log's types — is
// refused though the non-test build never links it.
//
// Mutation: import internal/changefeed (which links the log) from a non-test
// file — red on the link; name statelog.ReadSession in pushed_test.go — red
// on the names; import internal/config for a default — red.
func TestTheSandboxLinksNoGitletConfigurationOrStateLog(t *testing.T) {
	t.Parallel()
	const module = "github.com/crewlet/crewlet/internal/"
	refused := []string{"code", "config", "statelog"}
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
