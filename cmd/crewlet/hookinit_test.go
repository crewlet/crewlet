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

// WHAT EVERY PUSH PAYS BEFORE ITS HOOK RUNS. The code host's proc-receive hook
// is this binary, exec'd once a push (`crewlet code hook proc-receive`), and Go
// runs every linked package's initialisation before main's first statement —
// the hook's dispatch — so whatever a package of this engine does at init, a
// template parsed, a table hashed, a pattern compiled, it does on every push's
// critical path, for a process that needs none of it.
//
// MEASURED IN BYTES, which a test can hold, rather than in milliseconds, which
// it cannot: GODEBUG=inittrace reports both for every package, and what a
// package allocates at init is a property of the build, the same on a loaded
// runner and an idle laptop. The milliseconds are what BenchmarkTheHookStartsUp
// measures, and docs/concepts/code-host.md records.
//
// The two bounds, and why each:
//
//   - hookInitPackageBytes, 256 KiB, for one package: the largest any package
//     of this module allocates at init today is redact's, about 195 KB of
//     patterns (10 KB of it the engine's own run-token shape), with config's
//     beside it at 185, and the webhooks pages parsed at init were 315 KB —
//     about 2 ms of every push — before they were built at their first
//     request. A package that needs more builds it at first use
//     (sync.OnceValue), as the password blocklist does (140 KB) and the code
//     cache's file-name patterns do (215 KB).
//   - hookInitBytes, 1.5 MiB, for the module: about 1.37 MB today, 1.41 under
//     the race detector (it was 1.1 MB before the artifact packages and the
//     larger redaction tables were linked beside the code host, and the brand
//     and the drawing allowlist came in with their tables built at first use;
//     it had reached 1.62 under the race detector, past this bound, before the
//     code cache's file-name patterns were compiled at first use too), so a
//     second webhooks-sized global anywhere fails it, while a toolchain's
//     change to what a compiled pattern allocates, within the 160 KB left
//     under the race detector, does not.
const (
	hookInitPackageBytes = 256 << 10
	hookInitBytes        = 3 << 19
)

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
	for pkg, bytes := range trace {
		if !strings.HasPrefix(pkg, modulePrefix) || !linked[pkg] {
			continue
		}
		total += bytes
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
		t.Errorf("this module's packages allocate %d bytes at init, past %d, on every push's "+
			"hook", total, hookInitBytes)
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

// initTrace is the bytes each package allocated at init in this binary started
// as git starts the hook, from GODEBUG=inittrace — `init <package> @<at> ms,
// <clock> ms clock, <bytes> bytes, <allocs> allocs` on stderr, written before
// main runs.
func initTrace(t *testing.T) map[string]int64 {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "code", "hook", "proc-receive")
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
// binary exec'd as git starts it, through its packages' initialisation, to
// main's first statement and out — the hand-run's refusal, which is where a
// real hook would dial the relay. It runs the TEST binary, which links what
// the production binary does and this package's test helpers besides, so it
// is a slight overstatement of the production hook's start.
//
// Measured 2026-10-14 on a 4-vCPU container, without the race detector: about
// 14.5 ms a start, of which the runtime's own init trace (GODEBUG=inittrace)
// counts about 13 ms and this module's packages under 4 ms of that — 2 ms less
// once the webhooks pages stopped being built at init — beside about 2 ms for
// receive-pack's own start. The rest is other modules' initialisation, which
// no package here can defer.
func BenchmarkTheHookStartsUp(b *testing.B) {
	for b.Loop() {
		if _, _, status := probeHook(b); status != 2 {
			b.Fatalf("the probe exited %d, want the hand-run's 2", status)
		}
	}
}
