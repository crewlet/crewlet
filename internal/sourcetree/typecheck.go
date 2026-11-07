package sourcetree

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
)

// Listed is one package of the module as `go list` answers it for this
// platform: where its files are, and which of them each part of it compiles.
type Listed struct {
	// ImportPath is the package's own path, never a test variant's
	// bracketed name.
	ImportPath string
	// Dir is the package's directory, absolute.
	Dir string
	// GoFiles are the package's non-test files, TestGoFiles its in-package
	// test files and XTestGoFiles its external test package's, each a name
	// in Dir and only what this platform builds.
	GoFiles, TestGoFiles, XTestGoFiles []string
}

// Part is which files of a package a [Unit] type-checked.
type Part int

const (
	// Package is the package's non-test files: what an importer compiles
	// against.
	Package Part = iota
	// WithTests is the package with its in-package test files, as its test
	// binary compiles it.
	WithTests
	// External is the package's external test package (package p_test),
	// against the package as its test binary compiles it.
	External
)

// TypeCheck says what a gate type-checks of the module's own source, with the
// standard library alone: [TypeCheck.Run] asks `go list -export -deps` for the
// export data of the plain packages everything checked imports, and
// type-checks each one from source against it.
type TypeCheck struct {
	// Fset positions every file the check parses. Required.
	Fset *token.FileSet
	// Patterns are the packages to check, as `go list` reads them from the
	// root: "./..." when empty. Everything they import is read from export
	// data and never checked.
	Patterns []string
	// Tests checks each package as its test binary compiles it: one [Unit]
	// of the package with its in-package test files ([WithTests]) and one of
	// its external test package ([External]), in place of the package
	// alone. Without it each package is one [Package] unit.
	Tests bool
	// Select, when set, says which listed packages are checked; the others
	// are read only as export data. A gate that judges part of the module
	// leaves the rest unparsed.
	Select func(*Listed) bool
	// Parse reads one file of a unit, by its absolute path; nil parses it
	// with [parser.SkipObjectResolution] into Fset. A gate that has parsed
	// the file already hands back its own tree, parsed into the same Fset.
	Parse func(filename string) (*ast.File, error)
	// Info makes the record each unit is checked into; nil records Types,
	// Defs and Uses.
	Info func() *types.Info
}

// Unit is one set of files type-checked together.
type Unit struct {
	Package *Listed
	Part    Part
	Files   []*ast.File
	Types   *types.Package
	Info    *types.Info
}

// listedJSON is the part of `go list -json` the check reads.
type listedJSON struct {
	ImportPath   string
	Dir          string
	Export       string
	ForTest      string
	DepOnly      bool
	Module       *struct{ Main bool }
	GoFiles      []string
	TestGoFiles  []string
	XTestGoFiles []string
	Error        *struct{ Err string }
}

// Run type-checks the packages of the module rooted at root, which must hold
// its go.mod, and hands each unit to each as it is checked: a package after
// every package of the module it imports, its in-package tests before its
// external test package. An error from each ends the run and is returned.
//
// THE MODULE'S ONE WAY FOR A GATE TO TYPE-CHECK ITS SOURCE, for a gate whose
// subject is a value's type rather than its spelling — a set printed by a
// format, a time.Time's Sub — and that would rather pay a type checker than
// keep a walker of its own: [internal/sourcetree/decltype] is the other road,
// reading only the declarations a judged value reaches, for a gate that cannot
// afford to build what the judged packages import.
//
// THE EXPORT DATA IS THE BUILD CACHE'S: `go list -export` compiles what it
// does not find there, run by the toolchain that built this binary and with
// its -race and -tags ([buildFlags]), so a gate inside `go test` reads what
// the run compiled already. Cold, it compiles everything the patterns import.
//
// A TEST BINARY IS ITS OWN WORLD, and Tests reads each package in it as the
// binary compiles it. The in-package tests are checked with the package's own
// files against plain export data, since Go refuses an in-package test that
// imports what imports the package. The external test package is checked
// against the package's TEST VARIANT, `p [p.test]` in go list's naming, which
// holds what the in-package test files declare (an export_test.go's names),
// and against the variant of every package that imports p and is compiled
// again for the binary, `q [p.test]`.
//
// AND IT IS BUILT FROM SOURCE, never compiled for export data: `p [p.test]`
// is the package the in-package unit just checked, and every `q [p.test]` is
// a package of the module (only the module imports p), checked from its own
// files against it in dependency order. So the export data a run asks for is
// the plain packages' alone — what any importer reads, and what a build of
// the module compiles anyway — and never a test variant, an external test
// package or a test main, which `go list -export -test` would compile for
// every listed package: on two cores that took 169 s after a source change
// and 390 s from an empty cache for fixedwait's 52 packages, against the few
// seconds a warm cache answers in. The variants are known from a listing
// without -export, which compiles nothing.
func (c TypeCheck) Run(root string, each func(*Unit) error) error {
	if c.Fset == nil {
		return errors.New("sourcetree: a TypeCheck positions what it parses in a FileSet, and none was given")
	}
	listed, exports, binaries, err := c.list(root)
	if err != nil {
		return err
	}
	parse := c.Parse
	if parse == nil {
		parse = func(filename string) (*ast.File, error) {
			return parser.ParseFile(c.Fset, filename, nil, parser.SkipObjectResolution)
		}
	}
	newInfo := c.Info
	if newInfo == nil {
		newInfo = func() *types.Info {
			return &types.Info{Types: map[ast.Expr]types.TypeAndValue{},
				Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}}
		}
	}
	// ONE IMPORTER FOR THE WHOLE RUN: a package read from export data is
	// one *types.Package wherever it is imported, so a time.Time the
	// in-package unit types is the external package's time.Time too.
	plain := exportImporter(c.Fset, exports)
	check := func(p *Listed, path string, names []string, imports types.Importer,
		info *types.Info) (*types.Package, []*ast.File, error) {
		files := make([]*ast.File, 0, len(names))
		for _, name := range names {
			f, err := parse(filepath.Join(p.Dir, name))
			if err != nil {
				return nil, nil, fmt.Errorf("sourcetree: parsing %s: %w", filepath.Join(p.Dir, name), err)
			}
			files = append(files, f)
		}
		pkg, err := (&types.Config{Importer: imports}).Check(path, c.Fset, files, info)
		if err != nil {
			return nil, nil, fmt.Errorf("sourcetree: type-checking %s: %w", path, err)
		}
		return pkg, files, nil
	}
	unit := func(p *Listed, part Part, path string, names []string, imports types.Importer) (*types.Package, error) {
		if len(names) == 0 {
			return nil, nil
		}
		info := newInfo()
		pkg, files, err := check(p, path, names, imports, info)
		if err != nil {
			return nil, err
		}
		return pkg, each(&Unit{Package: p, Part: part, Files: files, Types: pkg, Info: info})
	}
	for _, p := range listed {
		if c.Select != nil && !c.Select(p) {
			continue
		}
		if !c.Tests {
			if _, err := unit(p, Package, p.ImportPath, p.GoFiles, plain); err != nil {
				return err
			}
			continue
		}
		withTests := append(append([]string(nil), p.GoFiles...), p.TestGoFiles...)
		variant, err := unit(p, WithTests, p.ImportPath, withTests, plain)
		if err != nil {
			return err
		}
		if len(p.XTestGoFiles) == 0 {
			continue
		}
		binary := binaryImporter{built: map[string]*types.Package{}, plain: plain}
		tb := binaries[p.ImportPath]
		if tb != nil && tb.self && variant != nil {
			// Only where go list says the binary compiles p again: without
			// in-package test files it links plain p, which a package read
			// from export data and handed to the external tests names too.
			binary.built[p.ImportPath] = variant
		}
		var again []*Listed
		if tb != nil {
			again = tb.again
		}
		for _, q := range again {
			pkg, _, err := check(q, q.ImportPath, q.GoFiles, binary, nil)
			if err != nil {
				return fmt.Errorf("%w (compiled again for %s's test binary)", err, p.ImportPath)
			}
			binary.built[q.ImportPath] = pkg
		}
		if _, err := unit(p, External, p.ImportPath+"_test", p.XTestGoFiles, binary); err != nil {
			return err
		}
	}
	return nil
}

// binaryImporter is a test binary's importer: a package the binary compiles
// again, which the run built from source, and every other package from the
// run's export data.
type binaryImporter struct {
	built map[string]*types.Package
	plain types.Importer
}

func (b binaryImporter) Import(path string) (*types.Package, error) {
	if pkg := b.built[path]; pkg != nil {
		return pkg, nil
	}
	return b.plain.Import(path)
}

// testBinary is what go list says one package's test binary compiles again.
type testBinary struct {
	// self is whether it compiles the package under test again, `p
	// [p.test]`, with its in-package test files.
	self bool
	// again is every other package it compiles again, `q [p.test]`, each a
	// package that imports p, in dependency order.
	again []*Listed
}

// exportImporter reads a package from the export data go list answered for
// it.
func exportImporter(fset *token.FileSet, exports map[string]string) types.Importer {
	return importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file := exports[path]
		if file == "" {
			return nil, fmt.Errorf("sourcetree: go list gave no export data for %s", path)
		}
		return os.Open(file) //nolint:gosec // a path go list answered for the build cache
	})
}

// list asks go list for the packages under c.Patterns and everything they
// import: the module's own packages the patterns name, in dependency order;
// every plain package's export data by its import path; and, with c.Tests,
// the packages each test binary compiles again, by the package under test,
// in dependency order, the package under test and its external test package
// left out (the in-package unit and the External unit are those) but its
// being compiled again recorded.
//
// With c.Tests it asks twice: once with -test and without -export, which
// compiles nothing and names the test binaries' imports and variants, and
// once for the export data of the plain packages that listing names — so no
// test variant, external test package or test main is compiled ([Run]).
//
// THE TOOLCHAIN THAT BUILT THIS BINARY, because export data is read by the
// go/types this binary carries and another go writes a version it may not
// read: the Makefile's (CREWLET_GO, which internal/solo's roster guard lists
// packages with for the same reason), or GOROOT's when the environment names
// one. Neither found is fatal rather than a guess at PATH's — and a plain `go
// test` sets neither, so outside make the caller names its go in CREWLET_GO.
func (c TypeCheck) list(root string) ([]*Listed, map[string]string, map[string]*testBinary, error) {
	goCmd := os.Getenv("CREWLET_GO")
	if goCmd == "" {
		goroot := os.Getenv("GOROOT")
		if goroot == "" {
			return nil, nil, nil, errors.New("sourcetree: neither CREWLET_GO nor GOROOT names the " +
				"toolchain that built this test: run it through make, which exports CREWLET_GO, " +
				"or set CREWLET_GO to the go command that built it")
		}
		goCmd = filepath.Join(goroot, "bin", "go")
	}
	patterns := c.Patterns
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	fields := "-json=ImportPath,Dir,Export,ForTest,DepOnly,Module,GoFiles,TestGoFiles,XTestGoFiles,Error"
	first := []string{"-export"}
	if c.Tests {
		first = []string{"-test"}
	}
	answer, err := goList(goCmd, root, append(append(first, fields), patterns...))
	if err != nil {
		return nil, nil, nil, err
	}
	var listed []*Listed
	var plain []string
	exports := map[string]string{}
	binaries := map[string]*testBinary{}
	for _, p := range answer {
		path, _, recompiled := strings.Cut(p.ImportPath, " ")
		switch {
		case recompiled && p.ForTest != "":
			// "q [p.test]": q compiled again for p's test binary. p's own
			// variant is the in-package unit and p_test the External one.
			tb := binaries[p.ForTest]
			if tb == nil {
				tb = &testBinary{}
				binaries[p.ForTest] = tb
			}
			switch path {
			case p.ForTest:
				tb.self = true
			case p.ForTest + "_test":
			default:
				tb.again = append(tb.again, &Listed{ImportPath: path, Dir: p.Dir, GoFiles: p.GoFiles})
			}
			continue
		case recompiled || strings.HasSuffix(p.ImportPath, ".test"):
			continue
		}
		exports[path] = p.Export
		plain = append(plain, path)
		if p.DepOnly || p.Module == nil || !p.Module.Main {
			continue
		}
		l := &Listed{ImportPath: path, Dir: p.Dir, GoFiles: p.GoFiles}
		if c.Tests {
			l.TestGoFiles, l.XTestGoFiles = p.TestGoFiles, p.XTestGoFiles
		}
		if len(l.GoFiles)+len(l.TestGoFiles)+len(l.XTestGoFiles) == 0 {
			// A directory this platform builds nothing of: go list -e
			// says so in its Error, and there is nothing to check.
			continue
		}
		if p.Error != nil {
			return nil, nil, nil, fmt.Errorf("sourcetree: go list could not load %s: %s", path, p.Error.Err)
		}
		listed = append(listed, l)
	}
	if c.Tests && len(plain) > 0 {
		answer, err := goList(goCmd, root, append([]string{"-export", "-json=ImportPath,Export"}, plain...))
		if err != nil {
			return nil, nil, nil, err
		}
		for _, p := range answer {
			exports[p.ImportPath] = p.Export
		}
	}
	return listed, exports, binaries, nil
}

// goList runs `go list -e -deps` with args, then this binary's build flags,
// then the patterns args ends with, from root, and decodes its answer.
func goList(goCmd, root string, args []string) ([]listedJSON, error) {
	// Every flag comes before the first argument that does not start with
	// a dash, and the build flags after them, then "--" and the patterns.
	at := 0
	for at < len(args) && strings.HasPrefix(args[at], "-") {
		at++
	}
	full := append([]string{"list", "-e", "-deps"}, args[:at]...)
	full = append(full, buildFlags()...)
	full = append(append(full, "--"), args[at:]...)
	cmd := exec.CommandContext(context.Background(), goCmd, full...) //nolint:gosec // the toolchain that built this test
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("sourcetree: go list: %w\n%s", err, stderr.String())
	}
	var answer []listedJSON
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listedJSON
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			return answer, nil
		} else if err != nil {
			return nil, fmt.Errorf("sourcetree: reading go list's answer: %w", err)
		}
		answer = append(answer, p)
	}
}

// buildFlags are the flags this test binary was built with that change what
// is compiled, so go list answers from the compiles the run already made
// rather than building the module again.
func buildFlags() []string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return nil
	}
	var flags []string
	for _, s := range info.Settings {
		switch s.Key {
		case "-race":
			if s.Value == "true" {
				flags = append(flags, "-race")
			}
		case "-tags":
			flags = append(flags, "-tags="+s.Value)
		}
	}
	return flags
}
