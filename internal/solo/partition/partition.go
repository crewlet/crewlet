// Command partition prints one half of the test-package partition, or one
// shard of a half, so that `make test` and `make test-solo` — and each CI job
// running a shard of them — run exactly the packages they own.
//
//	go run ./internal/solo/partition parallel   # everything that shares a runner
//	go run ./internal/solo/partition solo       # everything that needs one to itself
//	go run ./internal/solo/partition -weights test-weights.tsv -shard 2/2 parallel
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
// # Weights decide ORDER and PLACEMENT, never MEMBERSHIP
//
// `-weights FILE` names the seconds each package's test binary took on a
// measured run (`importpath<TAB>seconds`, what internal/skipgate's -report
// writes). They change two things and nothing else:
//
//   - The ORDER a half is printed in, longest first. `go test` starts package
//     runs strictly in command-line order — cmd/go chains a barrier action
//     between consecutive runs, so run i starts only once run i-1 has — and go
//     list's order is alphabetical. Measured on CI before this existed:
//     internal/search (472s), internal/store (203s) and internal/tracker
//     (412s) started at +752s, +802s and +823s of a 1235s step, and the step
//     ended on those three with the rest of the runner idle.
//   - Which SHARD a package lands in, under `-shard I/N`: longest-processing-
//     time-first, each package to the shard with the least measured time so
//     far. One runner per half was the floor when this was added — the
//     parallel half held a four-vCPU runner 90-99% busy for fifteen minutes,
//     and the solo half ran internal/e2e plus five minutes of everything
//     else, serially. ci.yml's plan step says how many shards each half has
//     today and the measurement that number rests on.
//
// Which packages are in the half is go list plus the marker, exactly as without
// weights, and main ASSERTS that the shards cover the half exactly once before
// it prints any of them: every way a weight can be wrong — missing, zero,
// stale, naming a package that no longer exists — moves a package to another
// runner or later in a queue, and none of them can drop one. A package the
// file does not name weighs the median of the half's measured packages, so a
// new package lands mid-queue rather than first or last; a file naming none of
// the half weighs every package alike, which is go list's order and an even
// split by count, and says so on stderr.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
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
// STRICT, because the only producer is internal/skipgate's -report and a line
// it did not write is a bug somewhere upstream that would otherwise surface
// as a mysteriously unbalanced run. A blank line is skipped (a file that ends
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
// could save one it accepts — only a run whose shards all pass saves at all.
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
				"the file internal/skipgate -report writes is the only format read here", name, line, text)
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
// only, so the other half's durations (internal/e2e's, in particular) cannot
// drag it. With nothing measured at all every package weighs one, and the
// result is go list's order and an even split by count.
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

// shard deals an ordered half into n shards, longest first, each package to
// the shard with the least weight so far.
//
// LPT — longest processing time first — and the tie-break is part of the
// rule rather than an afterthought: least total, then FEWEST PACKAGES, then
// lowest index. Without the middle term a run of zero weights (a half of
// packages with no tests, or a measurement that recorded zeros) would all tie
// at a total of 0 and all go to the first shard, leaving the others empty.
// With it, an empty shard always wins, so no shard is empty while n ≤
// len(ordered). Each shard keeps the order it was dealt in, which is longest
// first.
func shard(ordered []string, weights map[string]float64, n int) [][]string {
	shards := make([][]string, n)
	totals := make([]float64, n)
	for _, p := range ordered {
		best := 0
		for i := 1; i < n; i++ {
			switch {
			case totals[i] < totals[best]:
				best = i
			case totals[i] == totals[best] && len(shards[i]) < len(shards[best]):
				best = i
			}
		}
		shards[best] = append(shards[best], p)
		totals[best] += weights[p]
	}
	return shards
}

// cover reports how the shards fail to cover the half exactly once, or nil.
//
// The in-process half of the exact-cover guarantee; ci.yml's `tests` job
// checks the same thing again across the jobs, from what each shard's run
// actually executed. Unreachable while shard deals each package once, and
// asserted anyway, because the one thing no single shard's caller can notice
// is a package that went to none of them.
func cover(half []string, shards [][]string) error {
	seen := make(map[string]int, len(half))
	for i, s := range shards {
		if len(s) == 0 {
			return fmt.Errorf("shard %d/%d is empty", i+1, len(shards))
		}
		for _, p := range s {
			if prev, dup := seen[p]; dup {
				return fmt.Errorf("%s is in shard %d/%d and shard %d/%d", p, prev, len(shards), i+1, len(shards))
			}
			seen[p] = i + 1
		}
	}
	for _, p := range half {
		if _, ok := seen[p]; !ok {
			return fmt.Errorf("%s is in no shard", p)
		}
		delete(seen, p)
	}
	if len(seen) > 0 {
		extra := slices.Sorted(maps.Keys(seen))
		return fmt.Errorf("%s is in a shard but not in the half", strings.Join(extra, ", "))
	}
	return nil
}

// parseShard reads `I/N`: run shard I of N, both counted from one.
func parseShard(spec string) (i, n int, err error) {
	a, b, ok := strings.Cut(spec, "/")
	if ok {
		i, err = strconv.Atoi(a)
		if err == nil {
			n, err = strconv.Atoi(b)
		}
	}
	if !ok || err != nil || n < 1 || i < 1 || i > n {
		return 0, 0, fmt.Errorf("-shard %q is not I/N with 1 ≤ I ≤ N (1/1 is the whole half)", spec)
	}
	return i, n, nil
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
	weightsPath := fs.String("weights", "", "measured `file` of importpath<TAB>seconds: run the longest first, and place shards by it")
	shardSpec := fs.String("shard", "1/1", "print only shard `I/N` of the half")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: %s [-weights FILE] [-shard I/N] parallel|solo\n", os.Args[0])
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
	index, n, err := parseShard(*shardSpec)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
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
	if n > len(half) {
		fmt.Fprintf(os.Stderr, "-shard %s: the %s half has %d package(s), so it cannot be split %d ways\n",
			*shardSpec, which, len(half), n)
		os.Exit(1)
	}

	weights, unmeasured := weigh(half, measured)
	switch {
	case measured == nil:
	case unmeasured == len(half):
		fmt.Fprintf(os.Stderr, "partition: %s times none of the %d packages in the %s half, so every "+
			"package weighs the same: go list's order, split by count\n", *weightsPath, len(half), which)
	case unmeasured > 0:
		fmt.Fprintf(os.Stderr, "partition: %d of the %d packages in the %s half are not in %s, and "+
			"each weighs the half's median\n", unmeasured, len(half), which, *weightsPath)
	}

	shards := shard(order(half, weights), weights, n)
	if err := cover(half, shards); err != nil {
		fmt.Fprintf(os.Stderr, "partition lost a package between shards: %v\n", err)
		os.Exit(1)
	}
	for _, p := range shards[index-1] {
		fmt.Println(p)
	}
}
