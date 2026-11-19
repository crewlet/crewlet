// Command partition prints one half of the test-package partition, so that
// `make test` and `make test-solo` — and the CI job that runs each of them —
// run exactly the packages they own.
//
//	go run ./internal/solo/partition parallel   # everything that shares a runner
//	go run ./internal/solo/partition solo       # everything that needs one to itself
//	go run ./internal/solo/partition -weights test-weights.tsv parallel
//	go run ./internal/solo/partition -weights test-weights.tsv -shard 2/4 parallel
//	go run ./internal/solo/partition merge shard-1.tsv shard-2.tsv > test-weights.tsv
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
// `-weights FILE` names each package's weight on a measured run
// (`importpath<TAB>seconds<TAB>cpu`, what internal/skipgate's -timings writes,
// with a line per top-level test under each package that this reads strictly
// and does not deal by), and it changes the ORDER a half is printed in,
// heaviest first, and nothing else.
//
// THE PARALLEL HALF IS WEIGHED BY CPU, the solo half by wall time. The
// parallel half's runners are CPU-bound (skipgate's CPU line read 3.2-3.7 of
// 4 CPUs busy on every race shard), so a package's wall time there measures
// what shared the machine with it as much as its own work: internal/engine
// read 804 s beside 1,440 s of other packages, and a deal balancing walls put
// the most work beside the package that could least afford it, whose shard
// ran 15.6 minutes while its peers ran 10-12. Balanced by CPU, every shard
// carries the same work, and the heaviest package's shard carries the least
// beside it. The solo half runs one package at a time and mostly waits (0.7
// of 4 CPUs busy), so its shard takes the SUM of its packages' walls, and
// that is what it is dealt by. `go test` starts package runs strictly in command-line order —
// cmd/go chains a barrier action between consecutive runs, so run i starts
// only once run i-1 has — and go list's order is alphabetical. Measured on CI
// before this existed: internal/search (472s), internal/store (203s) and
// internal/worklet (412s) started at +752s, +802s and +823s of a 1235s step,
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
// Order matters to the parallel half only. The solo half runs at -p 1, where
// cmd/go's one worker links and runs every test binary in turn, so it takes
// their sum in any order and a weight could not end it sooner.
//
// # Shards: weights decide WHICH RUNNER, never WHETHER
//
// `-shard I/N` prints the I-th of N shards of a half, so that ci.yml can run
// one half on N runners at once. The half is dealt longest first, each package
// to the shard carrying the least weight so far (ties to the lower shard): the
// longest-processing-time rule, under which no shard carries more than an even
// split plus the longest single package. Without weights every package weighs
// one and the deal is round-robin over go list's order, a split by count.
//
// Membership is still the half's, exactly. Every invocation deals all N shards
// and prints nothing unless each package of the half is in exactly one, and
// each shard prints longest first, so the order rule above holds per runner.
// For the solo half the weights decide the whole wall time: at -p 1 a shard
// takes the sum of its packages.
//
// Every runner must deal from the SAME weights file, or two runners can
// disagree about a package: ci.yml restores the measurement once, in the job
// that plans the shards, and hands every shard that one file.
//
// # Merge: one weights file from N runners
//
// `merge FILE...` joins the -timings files a sharded run's runners wrote into
// one weights file, each read by the strict reader -weights uses. A package
// two files name is refused, because the deal gives each package to one shard
// and two timings of it mean two runners ran it. Lines are written as read,
// sorted by import path with each package's tests under it, as
// internal/skipgate writes them.
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

// measurement is one weights file: each package's wall time and CPU, and the
// seconds of each top-level test under it.
//
// tests is read and held to the same rules as packages, and is not dealt by
// here: the partition deals whole packages, by packages alone. The test lines
// travel with the file into the merged measurement, which is where a person
// reads which tests a package's time goes to (the `test-weights` artifact).
type measurement struct {
	packages map[string]weight
	tests    map[string]map[string]float64
}

// weight is one package's line: its test binary's wall time, and its share of
// the CPU its run used (internal/skipgate's cpuShares).
type weight struct{ wall, cpu float64 }

// of is each package's weight of the kind a half is dealt by: CPU for the
// parallel half and wall time for the solo half ([dealtBy]).
func (m measurement) of(kind string) map[string]float64 {
	if m.packages == nil {
		return nil
	}
	out := make(map[string]float64, len(m.packages))
	for p, w := range m.packages {
		if kind == "solo" {
			out[p] = w.wall
		} else {
			out[p] = w.cpu
		}
	}
	return out
}

// topLevelTest reports whether name is one `go test -json` gives a top-level
// test, fuzz target or example: the three kinds a package's binary runs and
// times. A subtest's name carries a slash, and a name with a space is not one
// cmd/go prints.
func topLevelTest(name string) bool {
	if strings.ContainsAny(name, " /") {
		return false
	}
	for _, kind := range []string{"Test", "Fuzz", "Example"} {
		if strings.HasPrefix(name, kind) {
			return true
		}
	}
	return false
}

// readWeights parses a weights file: one `importpath<TAB>seconds<TAB>cpu` line
// per package (wall time, share of the run's CPU; finite, non-negative) and one
// `importpath<TAB>Test<TAB>seconds` line per top-level test under it.
//
// STRICT: the only producer is internal/skipgate's -timings, so any other line
// is an upstream bug. Blank lines are skipped; a malformed line (a subtest is
// never written, see [topLevelTest]), a package or test named twice (two runs
// spliced together), or a test whose package has no line is refused naming it.
// The rule may change freely: ci.yml keys the kept measurement on a hash of this
// package's and skipgate's source, so a tighter rule never meets a file it saved.
//
// Pure over a reader so the refusals can be stated directly.
func readWeights(r io.Reader, name string) (measurement, error) {
	m := measurement{packages: map[string]weight{}, tests: map[string]map[string]float64{}}
	firstTestLine := map[string]int{}
	seconds := func(line int, text string) (float64, error) {
		w, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsNaN(w) || math.IsInf(w, 0) || w < 0 {
			return 0, fmt.Errorf("%s:%d: %q is not a finite, non-negative number of seconds", name, line, text)
		}
		return w, nil
	}
	sc := bufio.NewScanner(r)
	for line := 1; sc.Scan(); line++ {
		text := sc.Text()
		if text == "" {
			continue
		}
		fields := strings.Split(text, "\t")
		if len(fields) != 3 || fields[0] == "" || strings.ContainsAny(fields[0], " ") {
			return measurement{}, fmt.Errorf("%s:%d: %q is neither `importpath<TAB>seconds<TAB>cpu` nor "+
				"`importpath<TAB>Test<TAB>seconds` — the file internal/skipgate -timings writes is the only format read here",
				name, line, text)
		}
		path := fields[0]
		if !topLevelTest(fields[1]) {
			wall, err := seconds(line, fields[1])
			if err != nil {
				return measurement{}, fmt.Errorf("%w; nor is it a top-level test's name "+
					"(Test…, Fuzz… or Example…, with no space or slash)", err)
			}
			cpu, err := seconds(line, fields[2])
			if err != nil {
				return measurement{}, err
			}
			if _, dup := m.packages[path]; dup {
				return measurement{}, fmt.Errorf("%s:%d: %s is named twice; a measured run times each package "+
					"once, so this file is two runs spliced together", name, line, path)
			}
			m.packages[path] = weight{wall: wall, cpu: cpu}
			continue
		}
		test := fields[1]
		w, err := seconds(line, fields[2])
		if err != nil {
			return measurement{}, err
		}
		if _, dup := m.tests[path][test]; dup {
			return measurement{}, fmt.Errorf("%s:%d: %s %s is named twice; a measured run times each test "+
				"once, so this file is two runs spliced together", name, line, path, test)
		}
		if m.tests[path] == nil {
			m.tests[path] = map[string]float64{}
			firstTestLine[path] = line
		}
		m.tests[path][test] = w
	}
	if err := sc.Err(); err != nil {
		return measurement{}, fmt.Errorf("read %s: %w", name, err)
	}
	for _, path := range slices.Sorted(maps.Keys(m.tests)) {
		if _, ok := m.packages[path]; !ok {
			return measurement{}, fmt.Errorf("%s:%d: %s times tests of a package it does not time; "+
				"skipgate writes a test's line only under its package's", name, firstTestLine[path], path)
		}
	}
	return m, nil
}

// weigh gives every package of a half its weight, and says how many the
// measurement did not name.
//
// A named package keeps its own seconds. An unnamed one (new or renamed) weighs
// the MEDIAN of the half's measured packages: neither first nor last, the honest
// position for a package nothing is known about. The median is of THIS half
// only, so a file that also times the other half (internal/e2e) cannot drag it.
// With nothing measured every package weighs one, giving go list's order;
// measured == nil (no weights asked for) is the same answer.
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

// shard names one of N shards of a half, numbered from 1 as `-shard I/N` reads.
type shard struct{ index, count int }

// parseShard reads `-shard I/N`; empty is the whole half, shard 1 of 1.
func parseShard(s string) (shard, error) {
	if s == "" {
		return shard{1, 1}, nil
	}
	i, n, ok := strings.Cut(s, "/")
	index, errI := strconv.Atoi(i)
	count, errN := strconv.Atoi(n)
	if !ok || errI != nil || errN != nil || count < 1 || index < 1 || index > count {
		return shard{}, fmt.Errorf("-shard %q is not I/N with 1 <= I <= N (it is the TEST_SHARD make variable)", s)
	}
	return shard{index, count}, nil
}

// deal splits a half into n shards by the longest-processing-time rule.
//
// Packages go out in [order]'s sequence, each to the least loaded shard, so
// each shard is itself longest first. A tie in load goes to the shard with
// fewer packages, so zero-weight packages spread rather than pile onto shard 1,
// then to the lower index, so the answer depends on the list and weights alone.
func deal(half []string, weights map[string]float64, n int) [][]string {
	shards := make([][]string, n)
	loads := make([]float64, n)
	for _, p := range order(half, weights) {
		least := 0
		for i := 1; i < n; i++ {
			if loads[i] < loads[least] || (loads[i] == loads[least] && len(shards[i]) < len(shards[least])) {
				least = i
			}
		}
		shards[least] = append(shards[least], p)
		loads[least] += weights[p]
	}
	return shards
}

// dealtOnce reports a package of the half that is in no shard or in two, or a
// shard left empty, which would run nothing and pass.
func dealtOnce(half []string, shards [][]string) error {
	seen := make(map[string]int, len(half))
	for i, s := range shards {
		if len(s) == 0 {
			return fmt.Errorf("shard %d/%d is empty: %d packages cannot fill %d shards", i+1, len(shards), len(half), len(shards))
		}
		for _, p := range s {
			if prev, dup := seen[p]; dup {
				return fmt.Errorf("%s is in shards %d and %d", p, prev, i+1)
			}
			seen[p] = i + 1
		}
	}
	for _, p := range half {
		if _, ok := seen[p]; !ok {
			return fmt.Errorf("%s is in no shard", p)
		}
	}
	if len(seen) != len(half) {
		return fmt.Errorf("the shards hold %d packages, the half %d", len(seen), len(half))
	}
	return nil
}

// timings is one -timings file, by the name its errors cite.
type timings struct {
	name string
	data []byte
}

// merge joins -timings files into one weights file, refusing a package two of
// them name.
//
// Each input is held to [readWeights] whole and its lines are kept as read, so
// the output is exactly what internal/skipgate wrote: sorted by import path,
// each package's line before its tests' lines, those in name order. A test
// line travels with its package, which the reader holds to the same file, so
// a package two inputs name is the one overlap there is to refuse.
func merge(inputs []timings) ([]byte, error) {
	from := map[string]string{}
	var lines []string
	for _, in := range inputs {
		m, err := readWeights(bytes.NewReader(in.data), in.name)
		if err != nil {
			return nil, err
		}
		for _, p := range slices.Sorted(maps.Keys(m.packages)) {
			if other, dup := from[p]; dup {
				return nil, fmt.Errorf("%s is timed by both %s and %s: two runners ran it, "+
					"so their shards were not dealt from one partition", p, other, in.name)
			}
			from[p] = in.name
		}
		for line := range strings.Lines(string(in.data)) {
			if line = strings.TrimSuffix(line, "\n"); line != "" {
				lines = append(lines, line)
			}
		}
	}
	// By path, then the package's own line (no test field) before its tests.
	key := func(line string) (path, test string) {
		fields := strings.Split(line, "\t")
		if topLevelTest(fields[1]) {
			return fields[0], fields[1]
		}
		return fields[0], ""
	}
	slices.SortFunc(lines, func(a, b string) int {
		pa, ta := key(a)
		pb, tb := key(b)
		if c := strings.Compare(pa, pb); c != 0 {
			return c
		}
		return strings.Compare(ta, tb)
	})
	var out bytes.Buffer
	for _, line := range lines {
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
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

// loadWeights reads the -weights file, or answers an empty measurement when
// none was named.
func loadWeights(path string) (measurement, error) {
	if path == "" {
		return measurement{}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return measurement{}, fmt.Errorf("-weights: %w (it is the TEST_WEIGHTS make variable; leave it empty to run without one)", err)
	}
	defer func() { _ = f.Close() }() // read-only: nothing a close could lose
	return readWeights(f, path)
}

// runMerge is `partition merge FILE...`: the merged file goes to stdout.
func runMerge(paths []string) int {
	inputs := make([]timings, 0, len(paths))
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "partition merge: %v\n", err)
			return 1
		}
		inputs = append(inputs, timings{name: p, data: data})
	}
	out, err := merge(inputs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "partition merge: %v\n", err)
		return 1
	}
	if _, err := os.Stdout.Write(out); err != nil {
		fmt.Fprintf(os.Stderr, "partition merge: write: %v\n", err)
		return 1
	}
	return 0
}

func main() {
	fs := flag.NewFlagSet("partition", flag.ContinueOnError)
	weightsPath := fs.String("weights", "", "measured `file` of importpath<TAB>seconds: print the longest first")
	shardFlag := fs.String("shard", "", "print shard `I/N` of the half, dealt by weight across N runners")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: %s [-weights FILE] [-shard I/N] parallel|solo\n", os.Args[0])
		fmt.Fprintf(fs.Output(), "       %s merge FILE...\n", os.Args[0])
		fs.PrintDefaults()
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if fs.Arg(0) == "merge" {
		if fs.NArg() < 2 || *weightsPath != "" || *shardFlag != "" {
			fs.Usage()
			os.Exit(2)
		}
		os.Exit(runMerge(fs.Args()[1:]))
	}
	if fs.NArg() != 1 || (fs.Arg(0) != "parallel" && fs.Arg(0) != "solo") {
		fs.Usage()
		os.Exit(2)
	}
	which := fs.Arg(0)
	want, err := parseShard(*shardFlag)
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

	byKind := measured.of(which)
	weights, unmeasured := weigh(half, byKind)
	switch {
	case byKind == nil:
	case unmeasured == len(half):
		fmt.Fprintf(os.Stderr, "partition: %s times none of the %d packages in the %s half, so every "+
			"package weighs the same: go list's order\n", *weightsPath, len(half), which)
	case unmeasured > 0:
		fmt.Fprintf(os.Stderr, "partition: %d of the %d packages in the %s half are not in %s, and "+
			"each weighs the half's median\n", unmeasured, len(half), which, *weightsPath)
	}

	// Every shard is dealt and checked, whichever one is printed: the other
	// runners deal the same file, so a package lost here is lost there too.
	shards := deal(half, weights, want.count)
	if err := dealtOnce(half, shards); err != nil {
		fmt.Fprintf(os.Stderr, "partition: the %s half cannot be dealt into %d shards: %v\n", which, want.count, err)
		os.Exit(1)
	}
	mine := shards[want.index-1]
	if want.count > 1 {
		var total, share float64
		for _, p := range half {
			total += weights[p]
		}
		for _, p := range mine {
			share += weights[p]
		}
		fmt.Fprintf(os.Stderr, "partition: shard %d/%d of the %s half: %d of %d packages, weighing %.0f of %.0f\n",
			want.index, want.count, which, len(mine), len(half), share, total)
	}
	for _, p := range mine {
		fmt.Println(p)
	}
}
