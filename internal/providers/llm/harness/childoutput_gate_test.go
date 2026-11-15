package harness

import (
	"fmt"
	"go/ast"
	gotoken "go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// No harness child is handed a pipe, and only startChild starts one. Both are
// gates over the source, held by type: a pipe that comes back, or a second
// place that starts a harness, would bring back the answer a pipe was measured
// cutting (capture.go), and nothing about either looks wrong at the call.

// harnessPkg and binaryPkg are the import paths the gates judge by.
const (
	harnessPkg = "github.com/crewlet/crewlet/internal/providers/llm/harness"
	binaryPkg  = harnessPkg + "/harnessprofile"
)

// sanctioned are the two functions of this package that may start a harness:
// startChild, the one place a child's output is captured, and
// runInCredentialHome, the operator's terminal commands, whose stdout and
// stderr are the CLI's own and which the engine reads nothing of.
var sanctioned = []string{"startChild", "runInCredentialHome"}

// judged is what a walk of the source found.
type judged struct {
	offences []string
	// found counts each sanctioned site's starts: the gate fails unless
	// both were found, so a matcher gone inert cannot pass.
	found map[string]int
	units int
}

// judge walks files, type-checked into info, for the harness gates: starts of
// a program typed as a profile's Binary outside the sanctioned functions of
// pkg; and, in pkg itself (pipes), every pipe, os/exec output helper, write to
// a command's Stdout or Stderr and start outside them.
func judge(fset *gotoken.FileSet, info *types.Info, files []*ast.File, pkg, harness, binary string, pipes bool,
	out *judged) {
	isBinary := func(t types.Type) bool {
		named, ok := types.Unalias(t).(*types.Named)
		return ok && named.Obj().Name() == "Binary" && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == binary
	}
	var program func(e ast.Expr) bool
	program = func(e ast.Expr) bool {
		e = ast.Unparen(e)
		if tv, ok := info.Types[e]; ok && tv.Type != nil && isBinary(tv.Type) {
			return true
		}
		if call, ok := e.(*ast.CallExpr); ok && len(call.Args) == 1 {
			if tv, ok := info.Types[call.Fun]; ok && tv.IsType() {
				return program(call.Args[0])
			}
		}
		return false
	}
	pkgFunc := func(e ast.Expr) (string, string) {
		sel, ok := ast.Unparen(e).(*ast.SelectorExpr)
		if !ok {
			return "", ""
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return "", ""
		}
		name, ok := info.Uses[id].(*types.PkgName)
		if !ok {
			return "", ""
		}
		return name.Imported().Path(), sel.Sel.Name
	}
	isCmd := func(t types.Type) bool {
		if p, ok := t.(*types.Pointer); ok {
			t = p.Elem()
		}
		named, ok := types.Unalias(t).(*types.Named)
		return ok && named.Obj().Name() == "Cmd" && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "os/exec"
	}
	cmdMember := func(e ast.Expr) string {
		sel, ok := ast.Unparen(e).(*ast.SelectorExpr)
		if !ok {
			return ""
		}
		if s, ok := info.Selections[sel]; ok && isCmd(s.Recv()) {
			return sel.Sel.Name
		}
		return ""
	}
	at := func(n ast.Node) string {
		pos := fset.Position(n.Pos())
		return fmt.Sprintf("%s:%d", filepath.Base(pos.Filename), pos.Line)
	}
	for _, file := range files {
		for _, decl := range file.Decls {
			fn := ""
			if d, ok := decl.(*ast.FuncDecl); ok {
				fn = d.Name.Name
			}
			allowed := pkg == harness && slices.Contains(sanctioned, fn)
			ast.Inspect(decl, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CallExpr:
					path, name := pkgFunc(n.Fun)
					arg := -1
					switch {
					case path == "os/exec" && name == "Command", path == "os" && name == "StartProcess",
						path == "syscall" && (name == "Exec" || name == "ForkExec" || name == "StartProcess"),
						path == "golang.org/x/sys/unix" && name == "Exec":
						arg = 0
					case path == "os/exec" && name == "CommandContext":
						arg = 1
					}
					if arg >= 0 && len(n.Args) > arg && program(n.Args[arg]) {
						if allowed {
							out.found[fn]++
						} else {
							out.offences = append(out.offences, fmt.Sprintf("%s starts a harness's binary in %s, "+
								"outside startChild", at(n), orTop(fn)))
						}
					}
					if !pipes {
						return true
					}
					switch {
					case path == "os" && name == "Pipe":
						out.offences = append(out.offences, fmt.Sprintf("%s makes a pipe in %s: a harness "+
							"child's output is a capture, never a pipe", at(n), orTop(fn)))
					case path == "os/exec" && (name == "Command" || name == "CommandContext") && !allowed:
						out.offences = append(out.offences, fmt.Sprintf("%s builds a command in %s, "+
							"outside startChild", at(n), orTop(fn)))
					}
					switch member := cmdMember(n.Fun); member {
					case "StdoutPipe", "StderrPipe", "Output", "CombinedOutput":
						out.offences = append(out.offences, fmt.Sprintf("%s calls exec.Cmd's %s in %s, which "+
							"hands the child a pipe", at(n), member, orTop(fn)))
					case "Start", "Run":
						if !allowed {
							out.offences = append(out.offences, fmt.Sprintf("%s starts a command in %s, "+
								"outside startChild", at(n), orTop(fn)))
						}
					}
				case *ast.AssignStmt:
					if !pipes {
						return true
					}
					for i, lhs := range n.Lhs {
						member := cmdMember(lhs)
						if member != "Stdout" && member != "Stderr" {
							continue
						}
						switch {
						case !allowed:
							out.offences = append(out.offences, fmt.Sprintf("%s sets a command's %s in %s, "+
								"outside startChild", at(n), member, orTop(fn)))
						case fn == "startChild" && len(n.Rhs) == len(n.Lhs):
							if t := info.TypeOf(n.Rhs[i]); t == nil || t.String() != "*os.File" {
								out.offences = append(out.offences, fmt.Sprintf("%s hands a child's %s a %v, "+
									"not a capture's *os.File: os/exec would make a pipe for it", at(n), member, t))
							}
						}
					}
				case *ast.CompositeLit:
					if !pipes || !isCmd(info.TypeOf(n)) {
						return true
					}
					for _, elt := range n.Elts {
						if kv, ok := elt.(*ast.KeyValueExpr); ok {
							if key, ok := kv.Key.(*ast.Ident); ok && (key.Name == "Stdout" || key.Name == "Stderr") {
								out.offences = append(out.offences, fmt.Sprintf("%s sets a command's %s in a "+
									"literal in %s", at(n), key.Name, orTop(fn)))
							}
						}
					}
				}
				return true
			})
		}
	}
}

func orTop(fn string) string {
	if fn == "" {
		return "a package-level declaration"
	}
	return fn
}

// walkGates type-checks the module under root (patterns, every package when
// empty) and judges every package's non-test files, harness's for pipes too
// when pipes is set: binary is the import path of the package declaring
// Binary.
func walkGates(t *testing.T, root string, patterns []string, harness, binary string, pipes bool) judged {
	t.Helper()
	out := judged{found: map[string]int{}}
	fset := gotoken.NewFileSet()
	err := sourcetree.TypeCheck{Fset: fset, Patterns: patterns, Info: func() *types.Info {
		return &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Uses: map[*ast.Ident]types.Object{},
			Defs: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	}}.Run(root, func(u *sourcetree.Unit) error {
		out.units++
		pkg := u.Package.ImportPath
		judge(fset, u.Info, u.Files, pkg, harness, binary, pipes && pkg == harness, &out)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// NO HARNESS CHILD IS HANDED A PIPE. This package's own files make no pipe,
// call none of os/exec's helpers that make one, set no command's Stdout or
// Stderr but in startChild — to a capture's *os.File, which os/exec hands over
// as it is — and in runInCredentialHome, the operator's terminal commands; and
// build and start a command nowhere else. It fails unless it found both.
//
// Mutation: make a pipe again anywhere in the package (the device sign-in's
// os.Pipe), and it fails naming the line.
func TestNoHarnessChildIsHandedAPipe(t *testing.T) {
	t.Parallel()
	got := walkGates(t, sourcetree.Root(t), []string{"./internal/providers/llm/harness"}, harnessPkg, binaryPkg, true)
	for _, o := range got.offences {
		t.Error(o)
	}
	for _, fn := range sanctioned {
		if got.found[fn] == 0 {
			t.Errorf("found no harness started in %s, so this gate is certifying nothing", fn)
		}
	}
}

// ONLY startChild STARTS A HARNESS, across the whole module. A program typed as
// a profile's Binary keeps its type through every field and variable that
// holds it, so a harness started from any package, through any name, is seen:
// os/exec's Command and CommandContext, os.StartProcess, syscall's Exec,
// ForkExec and StartProcess, and unix.Exec, outside startChild and
// runInCredentialHome. The matcher is held to known verdicts on a sample
// module first, and the gate fails unless it found both sanctioned sites.
//
// Mutation: a package outside harness that runs exec.CommandContext(ctx,
// string(p.Binary)) is refused, named by its line; Binary made a plain string
// again leaves the gate finding no sanctioned site.
func TestOnlyStartChildStartsAHarness(t *testing.T) {
	t.Parallel()
	sample := t.TempDir()
	for name, src := range map[string]string{
		"go.mod":                          "module example.com/m\n\ngo 1.25\n",
		"harnessprofile/binary_sample.go": "package harnessprofile\n\ntype Binary string\n\ntype Profile struct{ Binary Binary }\n",
		"harness/start_sample.go": `package harness

import (
	"context"
	"os/exec"

	"example.com/m/harnessprofile"
)

func startChild(ctx context.Context, b harnessprofile.Binary) { _ = exec.CommandContext(ctx, string(b)) }

func runInCredentialHome(p harnessprofile.Profile) { _ = exec.Command(string(p.Binary)) }
`,
		"p/p_sample.go": `package p

import (
	"context"
	"os"
	"os/exec"
	"syscall"

	"example.com/m/harnessprofile"
)

type spec struct{ binary harnessprofile.Binary }

func a(ctx context.Context, p harnessprofile.Profile) { _ = exec.CommandContext(ctx, string(p.Binary)) }
func b(s spec)                                       { _ = exec.Command(string(s.binary), "x") }
func c(p harnessprofile.Profile)                     { _, _ = os.StartProcess(string(p.Binary), nil, nil) }
func d(p harnessprofile.Profile)                     { _ = syscall.Exec(string((p.Binary)), nil, nil) }
func e(path string)                                  { _ = exec.Command(path) }
func f()                                             { _ = exec.Command("git") }
`,
	} {
		path := filepath.Join(sample, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := walkGates(t, sample, nil, "example.com/m/harness", "example.com/m/harnessprofile", false)
	want := []string{"a", "b", "c", "d"}
	var flagged []string
	for _, o := range got.offences {
		for _, fn := range want {
			if strings.Contains(o, " in "+fn+",") {
				flagged = append(flagged, fn)
			}
		}
	}
	slices.Sort(flagged)
	if !slices.Equal(flagged, want) || len(got.offences) != len(want) {
		t.Errorf("on the sample the gate refused %q, want exactly the starts in %v", got.offences, want)
	}
	if got.found["startChild"] != 1 || got.found["runInCredentialHome"] != 1 {
		t.Errorf("on the sample the gate found the sanctioned sites %v, want one each", got.found)
	}

	tree := walkGates(t, sourcetree.Root(t), nil, harnessPkg, binaryPkg, false)
	for _, o := range tree.offences {
		t.Error(o)
	}
	for _, fn := range sanctioned {
		if tree.found[fn] == 0 {
			t.Errorf("found no harness started in %s across %d packages, so this gate is certifying nothing",
				fn, tree.units)
		}
	}
}
