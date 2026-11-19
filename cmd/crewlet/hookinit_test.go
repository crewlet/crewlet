package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
)

// WHAT EVERY PUSH PAYS BEFORE ITS HOOK RUNS. Gitlet's proc-receive hook is this
// binary, exec'd once a push (`crewlet gitlet hook proc-receive`), and every
// linked package's init runs before its dispatch, so whatever a package does at
// init (a template parsed, a table hashed, a pattern compiled) lands on every
// push's critical path. A package that needs more builds it at first use
// (sync.OnceValue).
//
// MEASURED IN BYTES, which a test can hold, not milliseconds (the benchmark
// below, docs/concepts/gitlet.md): GODEBUG=inittrace reports both. UNDER THE
// RACE DETECTOR a sync.Pool drops a quarter of what is put back, so one build
// allocates up to 74 KB more at init on some starts: the probe starts
// hookInitRuns times and charges each package its least.
const (
	// 256 KiB for one package: the largest at init is redact's, about 132 KB of
	// patterns in the plain build; the webhooks pages once parsed at init were
	// 315 KB, about 2 ms of every push.
	hookInitPackageBytes = 256 << 10
	// 1.5 MiB for the module: CI's race job last measured 1.43 MB. A second
	// webhooks-sized global fails it, a toolchain's change to what a compiled
	// pattern allocates does not; growth past the 140 KB left is built at first
	// use, not bought with a larger bound. internal/logging's
	// TestNoPackageLevelLoggerIsBoundToTheBootDefault refuses a package-level
	// logger, which would compile every redaction rule at init.
	hookInitBytes = 3 << 19
)

// hookInitRuns is how many times the probe starts, each package charged the
// least it allocated on any of them. FIVE: with the page that matched patterns
// at init built at first use, one start's total varies by about 13 KB from the
// next, and the least of five starts came within 6 KB of the least of fifty in
// every one of ten groups of five; a start costs about 35 ms under the race
// detector.
const hookInitRuns = 5

// hookInitNamed is how many of the largest packages an overrun of the module's
// bound names, so it says whose growth it was rather than only by how much.
const hookInitNamed = 10

// modulePrefix is the import path every package of this module carries.
const modulePrefix = "github.com/crewlet/crewlet/"

// THE HOOK PAYS LITTLE FOR THE ENGINE'S OWN INITIALISATION: every package of
// this module the binary links allocates under hookInitPackageBytes at init,
// and all of them together under hookInitBytes — read off this test binary,
// started as git starts the hook, through the runtime's own init trace, and
// counted for the packages the production binary links (`go list -deps`), so
// a test helper's init is no package's cost.
//
// Mutation: parse the webhooks pages at init again, and the package bound
// goes red.
func TestTheHookPaysLittleForTheEnginesInit(t *testing.T) {
	t.Parallel()
	linked := productionPackages(t)
	trace := initTrace(t)

	var total int64
	var heavy []string
	type cost struct {
		pkg   string
		bytes int64
	}
	var costs []cost
	for pkg, bytes := range trace {
		if !strings.HasPrefix(pkg, modulePrefix) || !linked[pkg] {
			continue
		}
		total += bytes
		costs = append(costs, cost{pkg, bytes})
		if bytes > hookInitPackageBytes {
			heavy = append(heavy, fmt.Sprintf("%s (%d bytes)", pkg, bytes))
		}
	}
	slices.Sort(heavy)
	if len(heavy) > 0 {
		t.Errorf("packages allocating more than %d bytes at init, on every push's hook: %s — "+
			"build what they hold at first use (sync.OnceValue)", hookInitPackageBytes,
			strings.Join(heavy, ", "))
	}
	if total > hookInitBytes {
		slices.SortFunc(costs, func(a, b cost) int { return int(b.bytes - a.bytes) })
		var largest []string
		for _, c := range costs[:min(len(costs), hookInitNamed)] {
			largest = append(largest, fmt.Sprintf("%s %d", strings.TrimPrefix(c.pkg, modulePrefix), c.bytes))
		}
		t.Errorf("this module's packages allocate %d bytes at init, past %d, on every push's "+
			"hook; the largest: %s — build what one of them holds at first use "+
			"(sync.OnceValue)", total, hookInitBytes, strings.Join(largest, ", "))
	}
	if total == 0 {
		t.Fatal("the init trace counted nothing of this module: the probe did not trace, or " +
			"the trace's format changed")
	}
}

// productionPackages is every package of this module the production binary
// links: `go list -deps` of this directory's own package, without its tests.
func productionPackages(t *testing.T) map[string]bool {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "go", "list", "-deps", "-f", "{{.ImportPath}}", ".")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v: %s", err, stderr.String())
	}
	linked := map[string]bool{}
	for line := range strings.Lines(string(out)) {
		linked[strings.TrimSpace(line)] = true
	}
	return linked
}

// initTrace is the bytes each package allocates at init in this binary started
// as git starts the hook: the least of hookInitRuns starts, for the race
// detector's reason above.
func initTrace(t *testing.T) map[string]int64 {
	t.Helper()
	least := initTraceOnce(t)
	for range hookInitRuns - 1 {
		for pkg, bytes := range initTraceOnce(t) {
			if held, ok := least[pkg]; !ok || bytes < held {
				least[pkg] = bytes
			}
		}
	}
	return least
}

// initTraceOnce is the bytes each package allocated at init in one start of
// this binary as git starts the hook, from GODEBUG=inittrace — `init <package>
// @<at> ms, <clock> ms clock, <bytes> bytes, <allocs> allocs` on stderr,
// written before main runs.
func initTraceOnce(t *testing.T) map[string]int64 {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "gitlet", "hook", "proc-receive")
	cmd.Env = []string{mainProbeEnv + "=1", "GODEBUG=inittrace=1",
		procgrouptest.GORACE + "=" + procgrouptest.StandInRaceOptions()}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	var exit *exec.ExitError
	if err := cmd.Run(); err != nil && !errors.As(err, &exit) {
		t.Fatalf("run the probe: %v", err)
	}
	out := map[string]int64{}
	scanner := bufio.NewScanner(&stderr)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		// init PKG @AT ms, CLOCK ms clock, BYTES bytes, ALLOCS allocs
		if len(fields) != 11 || fields[0] != "init" || fields[8] != "bytes," {
			continue
		}
		n, err := strconv.ParseInt(fields[7], 10, 64)
		if err != nil {
			t.Fatalf("an init trace line with no byte count: %q", scanner.Text())
		}
		out[fields[1]] += n
	}
	return out
}

// BenchmarkTheHookStartsUp is what every push pays to start its hook: this
// binary exec'd as git starts it, through package initialisation to main's
// first statement and out (the hand-run's refusal, where a real hook would dial
// the relay). It runs the TEST binary, so it slightly overstates the
// production hook's start.
//
// Measured 2026-10-14 on a 4-vCPU container without the race detector: about
// 14.5 ms a start, about 13 ms of it the runtime's init trace and under 4 ms
// this module's packages, beside about 2 ms for receive-pack's own start. The
// rest is other modules' initialisation, which no package here can defer.
func BenchmarkTheHookStartsUp(b *testing.B) {
	for b.Loop() {
		if _, _, status := probeHook(b); status != 2 {
			b.Fatalf("the probe exited %d, want the hand-run's 2", status)
		}
	}
}
