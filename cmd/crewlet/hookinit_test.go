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

// WHAT EVERY PUSH PAYS BEFORE ITS HOOK RUNS. Gitlet's proc-receive hook
// is this binary, exec'd once a push (`crewlet gitlet hook proc-receive`), and Go
// runs every linked package's initialisation before main's first statement —
// the hook's dispatch — so whatever a package of this engine does at init, a
// template parsed, a table hashed, a pattern compiled, it does on every push's
// critical path, for a process that needs none of it.
//
// MEASURED IN BYTES, which a test can hold, rather than in milliseconds, which
// it cannot: GODEBUG=inittrace reports both for every package, and what a
// package allocates at init is a property of the build, the same on a loaded
// runner and an idle laptop. The milliseconds are what BenchmarkTheHookStartsUp
// measures, and docs/concepts/gitlet.md records.
//
// EXCEPT UNDER THE RACE DETECTOR, which every CI run of this test uses: there a
// sync.Pool drops a quarter of what is put back, at random, and a Get the drop
// left empty allocates afresh — a pattern's matcher, a formatter's buffer — so
// the same build allocates more at init on some starts than on others, and
// never less than its own initialisation does with every Put kept. One build
// measured 1.57 MB to 1.65 MB across six starts, 74 KB of it in one package
// that matched a rendered page against patterns at init. So the probe starts
// hookInitRuns times and each package is charged its least.
//
// The two bounds, and why each:
//
//   - hookInitPackageBytes, 256 KiB, for one package: the largest any package
//     of this module allocated at init when last measured is redact's, about
//     132 KB of patterns in the plain build — 195 before its token rules, the
//     engine's own run-token shape 10 KB of them, were built at first use —
//     with the event types' next at 93, and the webhooks pages parsed at init
//     were 315 KB —
//     about 2 ms of every push — before they were built at their first
//     request. A package that needs more builds it at first use
//     (sync.OnceValue), as the password blocklist does (140 KB) and the code
//     cache's file-name patterns do (215 KB).
//   - hookInitBytes, 1.5 MiB, for the module: about 1.43 MB under the race
//     detector when CI's race job last measured it, the least of
//     hookInitRuns starts. It was 1.1 MB
//     before the artifact packages and the larger redaction tables were
//     linked beside Gitlet, and the brand and the drawing allowlist
//     came in with their tables built at first use; it had reached 1.62 under
//     the race detector, past this bound, before the code cache's file-name
//     patterns were compiled at first use too. Merged beside main's Dashboard Assistant, Chatlet and artifacts
//     it reached 2.47 MB, 2.55 under the race detector, with config and the
//     assistant's wire each past the package bound: 26 grammars in 11
//     packages compiled at init, most a bounded repeat (`{0,63}`), which
//     compiles to a program holding every count. Compiled at first use, they
//     left 1.31 MB. Gitlet's own growth since took it to 1.44, and
//     main's Cloudflare validation then compiled its account and zone patterns
//     at init — 100 KB in config, 13 in cloudflare — for 1.56 MB, 1.63 in
//     CI's race job, until those too were built at first use. Main's Scaleway
//     and DigitalOcean checks then compiled their identifier and token
//     patterns at init — 24 KB in config, 24 in scaleway, 24 in setup, 19 in
//     digitalocean — for 1.53 MB, 1.62 in CI's race job, until those too were
//     built at first use, which left 1.45 in the plain build. CI's race job
//     still measured 0.07 to 0.13 MB over that — two trees within 1.3 KB of
//     each other measured 1.62 and 1.66 MB there — and most of what moved
//     from one start to the next was the MCP sign-in page, still rendered and
//     matched against its policy at init, whose matchers the race detector's
//     pools dropped at random: up to 74 KB between two starts of one build.
//     Built at its first request, and each package charged the least of
//     several starts, the module came to 1.43 MB under the race detector. So
//     a second webhooks-sized global anywhere fails it, while a toolchain's
//     change to what a compiled pattern allocates, within the 140 KB left,
//     does not; growth past that is built at first use rather than bought
//     with a larger bound, as redact's token rules were, which took 62 KB off
//     its init in the plain build. The session's, the JSON Web Token's and
//     DigitalOcean's rules after them added none only once the tree's one
//     package-level logger built out of slog's boot default was a
//     logging.Get one: the root's redaction reads an attribute as it is
//     bound, so that logger compiled every rule during its own package's
//     init, 189 KB in the plain build, for 1.60 MB there and 1.64 in CI's
//     race job. Without it the module came to 1.41 MB in the plain build,
//     and internal/logging's TestNoPackageLevelLoggerIsBoundToTheBootDefault
//     refuses another. The tree's growth since took CI's race job to 1.574 MB,
//     a kilobyte past the bound on a change that added nothing at init, with
//     Confluence's eight storage-format patterns still compiled there: 53 KB,
//     built at first use since.
const (
	hookInitPackageBytes = 256 << 10
	hookInitBytes        = 3 << 19
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
