package version

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The developer entry point, and what keeps it honest.
//
// The Makefile exists so `make check` fails wherever CI would fail. Nothing
// makes that true: the two files are edited months apart, by people looking
// at one of them, and every way they drift is silent. A target that lost
// `-race` still passes. A store suite run on one driver still passes. A
// suite whose environment variable nobody set skips, and a skip is green.
// The pull request is where you find out, which is the one place a pre-push
// gate exists to save you from.
//
// So the agreement is asserted here, beside the other repository surfaces
// this package guards (the goreleaser stamp, the Dependabot entries, the
// unattended-merge conditions) — all of them files nothing else reads and
// no compiler checks.

// --- the gates ------------------------------------------------------------

// EVERY SUITE CI RUNS IS A SUITE `make` RUNS, WITH THE SAME FLAGS.
//
// This is the whole claim the Makefile makes. A `go test` line in ci.yml
// with no counterpart here is a gate `make check` skips; a counterpart that
// dropped a flag is worse, because it reports a pass on flags CI does not
// accept. `-count=1` is the quiet one: without it a cached PASS from before
// the change answers for the change.
func TestEveryTestCommandCIRunsHasAMakeTarget(t *testing.T) {
	t.Parallel()
	makefile := expandMakeVars(releaseFile(t, "Makefile"))
	local := goTestCommands(makefile)

	for _, want := range goTestCommands(releaseFile(t, ".github/workflows/ci.yml")) {
		// The nearest miss, not the last one: more than one target may run
		// the same packages (`make test` and `make test-norace` both run
		// ./...), and an error naming whichever came last sends the reader
		// to a target that was never the gate.
		matched := false
		var closest *goTest
		for i := range local {
			if local[i].packages != want.packages {
				continue
			}
			missing := want.flagsMissingFrom(local[i])
			if len(missing) == 0 {
				matched = true
				break
			}
			if closest == nil || len(missing) < len(want.flagsMissingFrom(*closest)) {
				closest = &local[i]
			}
		}
		if matched {
			continue
		}
		if closest == nil {
			t.Errorf("ci.yml runs `go test %s %s` and no make target does — "+
				"`make check` would report a pass on a gate it never ran",
				want.packages, strings.Join(want.flags, " "))
			continue
		}
		t.Errorf("ci.yml runs `go test %s` with %v; the Makefile runs it with "+
			"%v, missing %v — the local run would pass on flags CI does not "+
			"accept",
			want.packages, want.flags, closest.flags,
			want.flagsMissingFrom(*closest))
	}
}

// BOTH CERTIFIED DRIVERS RUN LOCALLY, because a suite run on one of them
// certifies nothing about the other: every statement in internal/store must
// parse on both, and Turso is currently the narrower dialect. ci.yml runs
// them as a matrix; a Makefile that hard-codes one leg is a local run that
// agrees with itself.
func TestTheStoreDriverMatrixIsTheOneTheMakefileLoops(t *testing.T) {
	t.Parallel()
	makefile := expandMakeVars(releaseFile(t, "Makefile"))
	drivers := ciMatrixDrivers(t, releaseFile(t, ".github/workflows/ci.yml"))
	for _, driver := range drivers {
		if !strings.Contains(makefile, driver) {
			t.Errorf("ci.yml certifies the store on %q and the Makefile never "+
				"names it, so `make test-stores` leaves that dialect uncertified",
				driver)
		}
	}
}

// EVERY GATE VARIABLE CI SETS, THE MAKEFILE SETS TOO.
//
// Each of these selects what a suite actually exercises, and every one of
// them fails open: unset, the store suite runs on the default driver alone
// and the Pulsar conformance suite skips outright. A skip is green, and that
// suite is the only place the Pulsar backend is certified at all.
func TestTheSuiteSelectingVariablesAreSetLocallyToo(t *testing.T) {
	t.Parallel()
	makefile := releaseFile(t, "Makefile")
	for _, name := range crewletVars(releaseFile(t, ".github/workflows/ci.yml")) {
		if !strings.Contains(makefile, name) {
			t.Errorf("ci.yml sets %s and the Makefile never does, so the suite "+
				"it selects runs against the wrong thing locally — or skips, "+
				"which is green", name)
		}
	}
}

// THE DASHBOARD CANNOT GO QUIET.
//
// static/dashboard/ has no Go code: its ~350 assertions run under plain
// `node`, driven from internal/api, and they SKIP when node is missing. So a
// machine without node runs the whole suite green having tested none of the
// dashboard. ci.yml installs node rather than tolerating that; a make target
// cannot install one, so it must refuse to pretend — which only works while
// the suite targets actually depend on the guard.
func TestTheSuiteTargetsRefuseToRunWithoutNode(t *testing.T) {
	t.Parallel()
	makefile := releaseFile(t, "Makefile")
	guards := targetsCheckingForNode(makefile)
	if len(guards) == 0 {
		t.Fatal("no Makefile target checks for node, so `make test` on a " +
			"machine without one would go green having skipped every " +
			"dashboard assertion")
	}
	for _, target := range []string{"test", "test-norace", "test-e2e"} {
		prereqs := prerequisitesOf(makefile, target)
		if !anyOf(prereqs, guards) {
			t.Errorf("`make %s` does not depend on the node guard (%v), so it "+
				"would run green with the dashboard suites skipped",
				target, guards)
		}
	}
}

// --- the local loops ------------------------------------------------------

// A COMPOSE PROFILE THE MAKEFILE NAMES IS ONE THE COMPOSE FILE HAS.
//
// `docker compose --profile typo up -d` exits 0 having started nothing, so a
// renamed profile turns a vendor loop into a target that succeeds and does
// nothing — and the bootstrap script that runs next fails against a stack
// that was never there.
func TestEveryComposeProfileTheMakefileStartsExists(t *testing.T) {
	t.Parallel()
	compose := releaseFile(t, "docker-compose.yml")
	named := regexp.MustCompile(`--profile ([a-z][a-z0-9-]*)`).
		FindAllStringSubmatch(releaseFile(t, "Makefile"), -1)
	if len(named) == 0 {
		t.Fatal("the Makefile starts no compose profile at all")
	}
	for _, match := range named {
		if !strings.Contains(compose, "profiles: ["+match[1]+"]") {
			t.Errorf("the Makefile starts compose profile %q, which "+
				"docker-compose.yml does not define — that command exits 0 "+
				"having started nothing", match[1])
		}
	}
}

// --- the target list ------------------------------------------------------

// EVERY TARGET IS IN `make help`, for the same reason every command the CLI
// dispatches is in its usage(): nothing connects the two, and a target
// nobody can find is a target nobody runs. The converse holds as well — a
// documented target missing from .PHONY is one make will skip the day a file
// of that name appears in the tree.
func TestEveryTargetIsListedAndPhony(t *testing.T) {
	t.Parallel()
	makefile := releaseFile(t, "Makefile")
	documented := map[string]bool{}
	// Same-line, and `[^=\n]` rather than `[^=]`: a greedy class that can
	// cross newlines swallows every rule between the first match and the
	// last `## ` in the file, which reports all but a handful of targets
	// undocumented.
	for _, match := range regexp.MustCompile(`(?m)^([a-z][a-z0-9-]*):[^=\n]*## `).
		FindAllStringSubmatch(makefile, -1) {
		documented[match[1]] = true
	}
	phony := map[string]bool{}
	for _, name := range phonyTargets(makefile) {
		phony[name] = true
	}
	if len(phony) == 0 || len(documented) == 0 {
		t.Fatalf("parsed %d .PHONY targets and %d documented ones, so this "+
			"test is checking nothing", len(phony), len(documented))
	}
	for name := range phony {
		if !documented[name] {
			t.Errorf("target %q has no `## ` description, so `make help` does "+
				"not list it", name)
		}
	}
	for name := range documented {
		if !phony[name] {
			t.Errorf("target %q is documented but not in .PHONY, so make "+
				"would skip it if a file of that name ever existed", name)
		}
	}
}

// --- reading the two files ------------------------------------------------

// goTest is one `go test` invocation, reduced to what must agree: which
// packages it runs and which flags it runs them under. Order is not part of
// it — `go test ./... -race` and `go test -race ./...` are the same run.
type goTest struct {
	packages string
	flags    []string
}

func (want goTest) flagsMissingFrom(got goTest) []string {
	have := map[string]bool{}
	for _, flag := range got.flags {
		have[flag] = true
	}
	var missing []string
	for _, flag := range want.flags {
		if !have[flag] {
			missing = append(missing, flag)
		}
	}
	return missing
}

// goTestCommands finds every `go test` invocation in a file, skipping
// comment lines — ci.yml's own comments discuss a `go test ./... -tags=integration`
// job that no longer exists, and a test that reads it would be asserting
// against prose.
func goTestCommands(body string) []goTest {
	var found []goTest
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		_, after, ok := strings.Cut(trimmed, "go test ")
		if !ok {
			continue
		}
		if command, ok := parseGoTest(after); ok {
			found = append(found, command)
		}
	}
	return found
}

// parseGoTest reads the tail of a `go test` line into packages and flags. It
// stops at the first shell operator: what follows `||` or `;` is the recipe's
// error handling, not part of the run.
func parseGoTest(tail string) (goTest, bool) {
	var command goTest
	fields := strings.Fields(tail)
	for i := 0; i < len(fields); i++ {
		field := fields[i]
		switch {
		case field == "||" || field == "&&" || field == "\\" ||
			strings.HasPrefix(field, ";"):
			i = len(fields)
		case strings.HasPrefix(field, "-"):
			// `-timeout 25m` and `-timeout=25m` are the same flag; join the
			// separated spelling so the two files may differ on style.
			if !strings.Contains(field, "=") && i+1 < len(fields) &&
				!strings.HasPrefix(fields[i+1], "-") &&
				!strings.HasPrefix(fields[i+1], ".") {
				command.flags = append(command.flags, field+"="+fields[i+1])
				i++
				continue
			}
			command.flags = append(command.flags, field)
		case strings.HasPrefix(field, "."):
			command.packages = field
		}
	}
	sort.Strings(command.flags)
	return command, command.packages != ""
}

// makeVar matches an assignment, including the exported and defaulted
// spellings the Makefile uses for the values CI sets as job env.
var makeVar = regexp.MustCompile(`(?m)^(?:export\s+)?([A-Z][A-Z0-9_]*)\s*(?::=|\?=|=)\s*(.*)$`)

// expandMakeVars substitutes the file's own variables so a recipe written as
// `$(GOTEST) ./...` can be compared against the command ci.yml spells out.
// Two passes is enough for the one level of nesting here ($(GOTEST) holds
// $(GO)); a third would only find the same fixed point.
func expandMakeVars(makefile string) string {
	values := map[string]string{}
	for _, match := range makeVar.FindAllStringSubmatch(makefile, -1) {
		values[match[1]] = strings.TrimSpace(match[2])
	}
	expanded := makefile
	for range 2 {
		for name, value := range values {
			expanded = strings.ReplaceAll(expanded, "$("+name+")", value)
		}
	}
	return expanded
}

// ciMatrixDrivers reads the store job's driver matrix out of ci.yml.
func ciMatrixDrivers(t *testing.T, workflow string) []string {
	t.Helper()
	match := regexp.MustCompile(`driver:\s*\[([^]]*)]`).FindStringSubmatch(workflow)
	if match == nil {
		t.Fatal("ci.yml has no `driver: [...]` matrix — either the dual-driver " +
			"job is gone, or this test is reading the wrong shape")
	}
	var drivers []string
	for _, raw := range strings.Split(match[1], ",") {
		if name := strings.TrimSpace(raw); name != "" {
			drivers = append(drivers, name)
		}
	}
	return drivers
}

// crewletVars collects the CREWLET_* variables a workflow sets, in either
// spelling: a `key: value` under env, or a `KEY=value` prefix on a command.
//
// Comment lines are skipped, and that is not tidiness: ci.yml's own comments
// explain a deleted job that read CREWLET_INTEGRATION, and a test that
// counted it would demand the Makefile set a variable nothing has read since.
func crewletVars(workflow string) []string {
	seen := map[string]bool{}
	var names []string
	name := regexp.MustCompile(`\bCREWLET_[A-Z0-9_]+`)
	for _, line := range strings.Split(workflow, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		for _, match := range name.FindAllString(line, -1) {
			if !seen[match] {
				seen[match] = true
				names = append(names, match)
			}
		}
	}
	sort.Strings(names)
	return names
}

// targetsCheckingForNode names every target whose recipe looks for node.
func targetsCheckingForNode(makefile string) []string {
	const probe = "command -v node"
	var found []string
	current := ""
	for _, line := range strings.Split(makefile, "\n") {
		if match := makeRule.FindStringSubmatch(line); match != nil {
			current = match[1]
			continue
		}
		if strings.HasPrefix(line, "\t") && current != "" && strings.Contains(line, probe) {
			found = append(found, current)
			current = ""
		}
	}
	return found
}

// makeRule matches a rule's head — `target: prereqs`, never a variable
// assignment (`NAME := value`) and never a recipe line (which is indented).
var makeRule = regexp.MustCompile(`^([a-z][a-z0-9-]*):([^=]*)$`)

func prerequisitesOf(makefile string, target string) []string {
	for _, line := range strings.Split(makefile, "\n") {
		match := makeRule.FindStringSubmatch(line)
		if match == nil || match[1] != target {
			continue
		}
		prereqs, _, _ := strings.Cut(match[2], "##")
		return strings.Fields(prereqs)
	}
	return nil
}

// phonyTargets reads the .PHONY list, backslash continuations included —
// which is where most of the names are.
func phonyTargets(makefile string) []string {
	_, after, ok := strings.Cut(makefile, ".PHONY:")
	if !ok {
		return nil
	}
	var declared []string
	for _, line := range strings.Split(after, "\n") {
		continued := strings.HasSuffix(strings.TrimSpace(line), "\\")
		declared = append(declared, strings.Fields(strings.TrimSuffix(strings.TrimSpace(line), "\\"))...)
		if !continued {
			break
		}
	}
	return declared
}

func anyOf(names []string, wanted []string) bool {
	for _, name := range names {
		for _, want := range wanted {
			if name == want {
				return true
			}
		}
	}
	return false
}
