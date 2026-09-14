// Command partition prints one half of the test-package partition, so that
// `make test` and `make test-solo` can each run exactly the packages it owns.
//
//	go run ./internal/solo/partition parallel   # everything that shares a runner
//	go run ./internal/solo/partition solo       # everything that needs one to itself
//
// The rule is one line: a package is solo iff its tests import
// [github.com/crewlet/crewlet/internal/solo], whose doc says why that is the
// mechanism and what every obvious alternative cost when it was measured. This
// command reads DECLARATIONS and nothing else — whether the declarations are
// COMPLETE is internal/solo's own roster guard, which fails the build when a
// package that stands up a multi-member broker has not declared one.
//
// It exists as a Go program rather than a shell pipeline because the guards
// below are the point, and a pipeline gets them wrong in ways that pass: the
// `go list | grep -v` this replaced discarded go list's exit status through
// $(shell ...), so a listing that failed halfway ran a SUBSET of the suite and
// reported success.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
)

// goCommand is the toolchain to discover packages with.
//
// THE ONE THE CALLER SELECTED, not whatever `go` PATH resolves to. The
// Makefile lets a caller pick with `make GO=/path/to/go` and uses $(GO) for
// every other command, so a hardcoded `go` here would discover packages with
// one toolchain and run the tests with another — or fail outright where the
// selected toolchain is not on PATH at all. The Makefile exports this
// alongside $(GO); a bare `go` is the fallback for anyone running the command
// by hand.
func goCommand() string {
	if selected := os.Getenv("CREWLET_GO"); selected != "" {
		return selected
	}
	return "go"
}

// marker is the package whose import declares "this one runs alone".
const marker = "github.com/crewlet/crewlet/internal/solo"

// pkg is the slice of `go list -json` this needs.
//
// XTestImports is not decoration and it is the field a hand-written filter
// would have missed: internal/node and internal/statelog are EXTERNAL test
// packages (`package node_test`), so their imports land here and not in
// TestImports. A query reading only TestImports would put two of the heavy
// packages back in the shared runner, which is the bug this command was
// written to fix.
type pkg struct {
	ImportPath   string
	TestImports  []string
	XTestImports []string
}

// declaresSolo reports whether p's tests import the marker.
func declaresSolo(p pkg) bool {
	return slices.Contains(p.TestImports, marker) || slices.Contains(p.XTestImports, marker)
}

// partition splits listed packages into the two sets, preserving go list's
// order so the printed lists are stable between runs.
//
// Pure over values, in the tradition of internal/textindex and internal/search:
// the arithmetic of a partition is exactly the part worth testing, and a rule
// that can only be exercised by shelling out to the toolchain is a rule nobody
// re-reads.
func partition(pkgs []pkg) (parallel, solo []string) {
	for _, p := range pkgs {
		if declaresSolo(p) {
			solo = append(solo, p.ImportPath)
			continue
		}
		parallel = append(parallel, p.ImportPath)
	}
	return parallel, solo
}

// list runs `go list -json` over the module and decodes the stream.
//
// NEVER with -e. That flag turns a broken package into a record with an Error
// field and exit 0, which is precisely the shape that would let this command
// hand the Makefile a short list and call it the suite.
func list(ctx context.Context) ([]pkg, error) {
	// CommandContext, and the context is Background at the call site: this is
	// a short-lived command with no caller to cancel it, and a timeout here
	// would be a worse bug than the hang it guards against — `go list` on a
	// cold module cache downloads the module graph, and a budget sized for a
	// warm CI checkout would fail a fresh clone for no reason.
	cmd := exec.CommandContext(ctx, goCommand(), "list",
		"-json=ImportPath,TestImports,XTestImports", "./...")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list ./...: %w (the partition is unknown, so no tests were run)", err)
	}

	var pkgs []pkg
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p pkg
		switch err := dec.Decode(&p); {
		case err == io.EOF:
			return pkgs, nil
		case err != nil:
			return nil, fmt.Errorf("decode go list output: %w", err)
		}
		pkgs = append(pkgs, p)
	}
}

func main() {
	if len(os.Args) != 2 || (os.Args[1] != "parallel" && os.Args[1] != "solo") {
		fmt.Fprintf(os.Stderr, "usage: %s parallel|solo\n", os.Args[0])
		os.Exit(2)
	}

	pkgs, err := list(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	parallel, solo := partition(pkgs)

	// Four guards, and each one covers a way a partition reports a pass over
	// nothing. The filter this replaced had only the first.
	switch {
	case len(parallel) == 0:
		fmt.Fprintln(os.Stderr, "the parallel partition is empty: go list returned no ordinary packages")
		os.Exit(1)
	case len(solo) == 0:
		fmt.Fprintf(os.Stderr, "the solo partition is empty: no package imports %s\n", marker)
		os.Exit(1)
	case len(parallel)+len(solo) != len(pkgs):
		// Unreachable while partition appends each package exactly once, and
		// asserted anyway: the one thing neither half's caller can notice is a
		// package that fell out of both.
		fmt.Fprintf(os.Stderr, "partition lost a package: %d + %d != %d listed\n",
			len(parallel), len(solo), len(pkgs))
		os.Exit(1)
	}

	want := parallel
	if os.Args[1] == "solo" {
		want = solo
	}
	for _, p := range want {
		fmt.Println(p)
	}
}
