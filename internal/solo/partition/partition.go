// Command partition prints one half of the test-package partition, so that
// `make test` and `make test-solo` — and the CI job that runs each of them —
// run exactly the packages they own.
//
//	go run ./internal/solo/partition parallel   # everything that shares a runner
//	go run ./internal/solo/partition solo       # everything that needs one to itself
//	go run ./internal/solo/partition -weights test-weights.tsv parallel
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
//
// # Weights decide ORDER, never MEMBERSHIP
//
// `-weights FILE` names the seconds each package's test binary took on a
// measured run (`importpath<TAB>seconds`, what internal/skipgate's -timings
// writes), and it changes the ORDER a half is printed in, longest first, and
// nothing else. `go test` starts package runs strictly in command-line order —
// cmd/go chains a barrier action between consecutive runs, so run i starts
// only once run i-1 has — and go list's order is alphabetical. Measured on CI
// before this existed: internal/search (472s), internal/store (203s) and
// internal/tracker (412s) started at +752s, +802s and +823s of a 1235s step,
// and the step ended on those three with the rest of the runner idle.
//
// Which packages are in the half is go list plus the marker, exactly as without
// weights: every way a weight can be wrong — missing, zero, stale, naming a
// package that no longer exists — moves a package earlier or later in the
// queue, and none of them can drop one. A package the file does not name
// weighs the median of the half's measured packages, so a new package lands
// mid-queue rather than first or last; a file naming none of the half weighs
// every package alike, which is go list's order, and says so on stderr.
//
// The Makefile weighs the PARALLEL half only. The solo half runs at -p 1,
// where cmd/go's one worker links and runs every test binary in turn, so it
// takes their sum in any order and a weight could not end it sooner.
//
// There is no flag that prints part of a half. Each half runs whole on one
// runner, and ci.yml's `test` job says what dealing it across several cost
// and bought when that was tried.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
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

// readWeights parses a weights file: one `importpath<TAB>seconds` per line.
//
// STRICT, because the only producer is internal/skipgate's -timings and a line
// it did not write is a bug somewhere upstream that would otherwise surface
// as a mysteriously ordered run. A blank line is skipped (a file that ends
// in one is still that file); anything else that is not a path, one tab and a
// finite, non-negative number of seconds is refused naming the line, and so
// is a path named twice — one run measures each package once, so two entries
// are two runs spliced together and neither number is the measurement.
//
// The rule can be tightened or loosened freely, because ci.yml keys the
// measurement it keeps on a hash of this package's source and
// internal/skipgate's: a CI run whose reader or writer differs from the ones
// that produced a saved file never restores it. Keyed on anything less, a
// tighter rule here would refuse the saved file on every later run, and no run
// could save one it accepts — only a run whose half passed saves at all.
//
// Pure over a reader so the refusals can be stated directly.
func readWeights(r io.Reader, name string) (map[string]float64, error) {
	weights := map[string]float64{}
	sc := bufio.NewScanner(r)
	for line := 1; sc.Scan(); line++ {
		text := sc.Text()
		if text == "" {
			continue
		}
		path, secs, ok := strings.Cut(text, "\t")
		if !ok || path == "" || strings.ContainsAny(path, " \t") {
			return nil, fmt.Errorf("%s:%d: %q is not `importpath<TAB>seconds` — "+
				"the file internal/skipgate -timings writes is the only format read here", name, line, text)
		}
		w, err := strconv.ParseFloat(secs, 64)
		if err != nil || math.IsNaN(w) || math.IsInf(w, 0) || w < 0 {
			return nil, fmt.Errorf("%s:%d: %q is not a finite, non-negative number of seconds", name, line, secs)
		}
		if _, dup := weights[path]; dup {
			return nil, fmt.Errorf("%s:%d: %s is named twice; a measured run times each package "+
				"once, so this file is two runs spliced together", name, line, path)
		}
		weights[path] = w
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	return weights, nil
}

// weigh gives every package of a half its weight, and says how many the
// measurement did not name.
//
// A package the measurement names keeps its own seconds. One it does not — new
// since the measured run, or renamed — weighs the MEDIAN of the half's measured
// packages: neither the first thing started nor the last, which is the honest
// position for a package nothing is known about. The median is of THIS half
// only, so a file that also times the other half (internal/e2e, in
// particular) cannot drag it. With nothing measured at all every package
// weighs one, and the result is go list's order.
//
// measured == nil means no weights were asked for, and is the same answer as
// a measurement that names nothing.
func weigh(half []string, measured map[string]float64) (weights map[string]float64, unmeasured int) {
	var known []float64
	for _, p := range half {
		if w, ok := measured[p]; ok {
			known = append(known, w)
		}
	}
	fallback := 1.0
	if len(known) > 0 {
		fallback = median(known)
	}
	weights = make(map[string]float64, len(half))
	for _, p := range half {
		w, ok := measured[p]
		if !ok {
			w = fallback
			unmeasured++
		}
		weights[p] = w
	}
	return weights, unmeasured
}

// median of a non-empty slice, which it sorts.
func median(xs []float64) float64 {
	sort.Float64s(xs)
	mid := len(xs) / 2
	if len(xs)%2 == 1 {
		return xs[mid]
	}
	return (xs[mid-1] + xs[mid]) / 2
}

// order prints the longest first, ties in the listed order.
//
// A STABLE sort on weight alone, so the tie-break is go list's own order: with
// no weights every package ties and the half comes out exactly as listed,
// which is what `make test` printed before weights existed, and with weights
// the answer is still a function of the list and the file and nothing else —
// no map iteration reaches it.
func order(half []string, weights map[string]float64) []string {
	out := slices.Clone(half)
	slices.SortStableFunc(out, func(a, b string) int {
		switch wa, wb := weights[a], weights[b]; {
		case wa > wb:
			return -1
		case wa < wb:
			return 1
		}
		return 0
	})
	return out
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

// loadWeights reads the -weights file, or answers nil when none was named.
func loadWeights(path string) (map[string]float64, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("-weights: %w (it is the TEST_WEIGHTS make variable; leave it empty to run without one)", err)
	}
	defer func() { _ = f.Close() }() // read-only: nothing a close could lose
	return readWeights(f, path)
}

func main() {
	fs := flag.NewFlagSet("partition", flag.ContinueOnError)
	weightsPath := fs.String("weights", "", "measured `file` of importpath<TAB>seconds: print the longest first")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: %s [-weights FILE] parallel|solo\n", os.Args[0])
		fs.PrintDefaults()
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if fs.NArg() != 1 || (fs.Arg(0) != "parallel" && fs.Arg(0) != "solo") {
		fs.Usage()
		os.Exit(2)
	}
	which := fs.Arg(0)
	measured, err := loadWeights(*weightsPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	pkgs, err := list(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	parallel, solo := partition(pkgs)

	// Three guards, and with list's refusal of a failed `go list` above them
	// each covers a way a partition reports a pass over nothing. The filter
	// this replaced had only the first of the three.
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

	half := parallel
	if which == "solo" {
		half = solo
	}

	weights, unmeasured := weigh(half, measured)
	switch {
	case measured == nil:
	case unmeasured == len(half):
		fmt.Fprintf(os.Stderr, "partition: %s times none of the %d packages in the %s half, so every "+
			"package weighs the same: go list's order\n", *weightsPath, len(half), which)
	case unmeasured > 0:
		fmt.Fprintf(os.Stderr, "partition: %d of the %d packages in the %s half are not in %s, and "+
			"each weighs the half's median\n", unmeasured, len(half), which, *weightsPath)
	}

	for _, p := range order(half, weights) {
		fmt.Println(p)
	}
}
