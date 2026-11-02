package sandbox_test

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// THE SANDBOX LINKS NO CODE HOST AND NO CONFIGURATION, AND READS THE STATE LOG
// FOR ITS ANSWER SEAL ALONE. It carries what it never reads as bytes: a run's
// code floor as a statelog position's token (PendingRun.CodeFloor), its grants
// as the strings the code host resolved. Linking the code host or the
// configuration to read any of them would bring the engine's configuration
// into everything that drives a box, and decide nothing this package decides.
//
// The state log IS linked, for one thing: an answer handed to a run is sealed
// under the fleet's record keyring ([sandbox.AnswerSeal], ADR-0031), whose
// signer and verifier the state log owns, so a person's answer is signed the
// way every record is. So the guard on the floor is the other half of the
// test: the package's own source names nothing of the state log but the
// seal's keyring, signer, verifier and verdicts, and a floor parsed or
// compared here is a name outside that list. Read from `go list -deps` (what
// its non-test build links) and from the package's non-test files.
//
// Mutation: parse the floor with statelog.ParsePosition in RaiseCodeFloor —
// red; import internal/config for a default — red.
func TestTheSandboxLinksNoCodeHostOrConfigurationAndReadsTheLogOnlyToSeal(t *testing.T) {
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
		for _, refused := range []string{"code", "config"} {
			if pkg == module+refused || strings.HasPrefix(pkg, module+refused+"/") {
				t.Errorf("internal/sandbox links %s", pkg)
			}
		}
	}
	if !strings.Contains(string(out), module+"sandbox\n") || listed < 10 {
		t.Fatalf("go list -deps named %d packages and not internal/sandbox among them: it read something else",
			listed)
	}

	// THE SEAL'S NAMES, and nothing else of the state log.
	seal := []string{"Keyring", "NewSigner", "NewVerifier", "Signer", "Verifier", "Verified", "KeyUnknown"}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	read := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		read++
		local := ""
		for _, imp := range file.Imports {
			if path, _ := strconv.Unquote(imp.Path.Value); path == module+"statelog" {
				local = "statelog"
				if imp.Name != nil {
					local = imp.Name.Name
				}
			}
		}
		if local == "" {
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == local && !slices.Contains(seal, sel.Sel.Name) {
				t.Errorf("%s: internal/sandbox reads statelog.%s, which is no part of the answer seal: carry "+
					"the value as bytes, and let the engine read it", fset.Position(sel.Pos()), sel.Sel.Name)
			}
			return true
		})
	}
	if read < 10 {
		t.Fatalf("the walk read %d of the package's files: it read something else", read)
	}
}
