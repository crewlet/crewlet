package version

import (
	"path/filepath"
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

// EVERY RELEASE TARGET CI CROSS-COMPILES, THE MAKEFILE CROSS-COMPILES TOO.
//
// This was the store-driver matrix — two drivers, one dialect, and a suite run
// on one certifying nothing about the other. There is one driver now
// (decisions/003) and the slot went to the thing that actually had no local
// gate: the release matrix. `go build ./...` compiles for the machine you are
// on, so a build tag or a platform-gated file that only breaks darwin reaches
// the tag, and a broken tag is a release to re-cut. windows/arm64 shipped
// broken for want of exactly this.
func TestTheCrossMatrixIsTheOneTheMakefileLoops(t *testing.T) {
	t.Parallel()
	makefile := expandMakeVars(releaseFile(t, "Makefile"))
	for _, pair := range ciMatrixPairs(t, releaseFile(t, ".github/workflows/ci.yml")) {
		if !strings.Contains(makefile, pair) {
			t.Errorf("ci.yml cross-compiles %q and the Makefile never names "+
				"it, so `make test-cross` leaves that platform unbuilt — and "+
				"nothing else builds for it before the tag", pair)
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
	rules := makeRules(releaseFile(t, "Makefile"))
	var guards []string
	for name, rule := range rules {
		if containsAny(rule.recipe, "command -v node") {
			guards = append(guards, name)
		}
	}
	if len(guards) == 0 {
		t.Fatal("no Makefile target checks for node, so `make test` on a " +
			"machine without one would go green having skipped every " +
			"dashboard assertion")
	}
	for _, target := range []string{"test", "test-norace", "test-e2e"} {
		prereqs := rules[target].prereqs
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

// ONE FILE PINS THE BROKER, AND THE CONFORMANCE JOB STARTS THAT FILE.
//
// A conformance pass is a claim about a BROKER — a close-driven handoff
// returning mail at redelivery count 0, a cursor surviving a change of owner
// — so `make pulsar-up` and the CI job have to mean the same build. This used
// to be two pins held equal by comparing them, which works right up until
// somebody moves one: both runs stay green while certifying different
// brokers, and nothing anywhere says which.
//
// Now there is one pin. ci.yml starts docker-compose.yml's own `pulsar`
// service, so the two cannot disagree, and Dependabot — which watches the
// compose file and not a version buried in a workflow's shell — moves it by
// opening a pull request that IS the re-certification.
//
// What is left to assert is that it stays one pin, because going back to two
// is a one-line edit whose only symptom is a suite quietly certifying a
// broker nobody chose.
func TestOnlyComposePinsTheBroker(t *testing.T) {
	t.Parallel()
	pinned := regexp.MustCompile(`apachepulsar/pulsar:([\w.-]+)`)

	compose := pinned.FindStringSubmatch(releaseFile(t, "docker-compose.yml"))
	if compose == nil {
		t.Fatal("docker-compose.yml names no pulsar image, so this test — and " +
			"the conformance job that starts the service — has no broker to pin")
	}
	// A floating tag is the same failure by another route: the claim is about
	// whatever was current at pull time, which nobody can name afterwards, and
	// Dependabot has no version to move.
	if tag := compose[1]; tag == "latest" || tag == "" {
		t.Errorf("docker-compose.yml runs apachepulsar/pulsar:%s — a conformance "+
			"pass against a floating tag names no build, and leaves the "+
			"docker-compose entry nothing to bump", tag)
	}

	var startsCompose bool
	for i, line := range strings.Split(releaseFile(t, filepath.Join(".github", "workflows", "ci.yml")), "\n") {
		trimmed := strings.TrimSpace(line)
		// The job explains the arrangement in prose, and that prose names the
		// image. Only what the job RUNS is a second pin.
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if pinned.MatchString(trimmed) {
			t.Errorf("ci.yml:%d pins a broker of its own (%s) — that is a second "+
				"version to move, and whichever one nobody moves still passes",
				i+1, trimmed)
		}
		if strings.Contains(trimmed, "docker compose") &&
			strings.Contains(trimmed, " up ") &&
			strings.Contains(trimmed, "pulsar") {
			startsCompose = true
		}
	}
	if !startsCompose {
		t.Error("no step in ci.yml brings the compose `pulsar` service up, so the " +
			"broker the suite certifies against is not the one docker-compose.yml " +
			"pins — and nothing else in this test would notice")
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
func ciMatrixPairs(t *testing.T, workflow string) []string {
	t.Helper()
	read := func(key string) []string {
		match := regexp.MustCompile(key + `:\s*\[([^]]*)]`).FindStringSubmatch(workflow)
		if match == nil {
			t.Fatalf("ci.yml has no `%s: [...]` matrix — either the "+
				"cross-compile job is gone, or this test is reading the "+
				"wrong shape", key)
		}
		var out []string
		for _, raw := range strings.Split(match[1], ",") {
			if name := strings.TrimSpace(raw); name != "" {
				out = append(out, name)
			}
		}
		return out
	}
	var pairs []string
	for _, os := range read("goos") {
		for _, arch := range read("goarch") {
			pairs = append(pairs, os+"/"+arch)
		}
	}
	return pairs
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

// --- what `make check` actually reaches ------------------------------------

// EVERY GATE CI RUNS IS ONE `make check` RUNS -- OR ONE IT NAMES.
//
// The other tests here prove a target exists with the right flags. None of
// them proves `check` still reaches it, and that is one `sed` away: drop
// `test-cross` from the prerequisite list and every assertion above stays
// green while the command a contributor is told to run before pushing stops
// building for three of the four platforms it ships. The pull request template makes this
// worse rather than better -- it now asks people to tick `make check`, so a
// weakened target is a gate every contributor passes in good faith.
//
// Two gates are deliberately outside `check` because both need a service CI
// starts for itself. That is allowed, and it is allowed only out loud: the
// recipe has to name the target that does cover them, which is what stops
// "did not run" from reading as "passed".
func TestMakeCheckRunsOrNamesEveryGate(t *testing.T) {
	t.Parallel()
	makefile := expandMakeVars(releaseFile(t, "Makefile"))
	rules := makeRules(makefile)
	reached := reachableFrom(rules, "check")
	if len(reached) < 2 {
		t.Fatalf("`check` reaches %v, so this test is checking nothing", reached)
	}
	var covered []string // every recipe line `make check` would run
	for name := range reached {
		covered = append(covered, rules[name].recipe...)
	}
	announced := strings.Join(rules["check"].recipe, "\n")

	workflow := releaseFile(t, ".github/workflows/ci.yml")

	// The gates that are not `go test`: a literal command, matched literally.
	// Losing one of these is the same silent weakening and has no flags to
	// compare, so the command itself is the assertion.
	for _, command := range plainGates(workflow) {
		if !containsAny(covered, command) {
			t.Errorf("ci.yml runs %q and `make check` reaches no target that "+
				"does — the local gate would pass on a check CI still makes",
				command)
		}
	}

	for _, want := range ciTestSteps(workflow) {
		if coveredBy(covered, want) {
			continue
		}
		runner := targetRunning(rules, want)
		switch {
		case runner == "":
			t.Errorf("no make target runs `go test %s`%s at all",
				want.packages, want.envNote())
		case !namesTarget(announced, runner):
			t.Errorf("`make check` does not run `go test %s`%s and does not "+
				"name `make %s`, which does — a green check would imply a "+
				"gate it never ran", want.packages, want.envNote(), runner)
		}
	}
}

// ciStep is one `go test` step of the workflow, with the CREWLET_* variables
// its step sets: a run selected by an environment variable is not covered by
// the plain `./...` run, however wide that pattern looks.
type ciStep struct {
	goTest
	env []string
}

func (s ciStep) envNote() string {
	if len(s.env) == 0 {
		return ""
	}
	return " with " + strings.Join(s.env, ", ")
}

// ciTestSteps reads every `go test` step out of the workflow together with
// the variables set beneath it, which is where a step's env lives in YAML.
func ciTestSteps(workflow string) []ciStep {
	lines := strings.Split(workflow, "\n")
	variable := regexp.MustCompile(`^\s*(CREWLET_[A-Z0-9_]+):`)
	var steps []ciStep
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		_, after, ok := strings.Cut(trimmed, "go test ")
		if !ok {
			continue
		}
		command, ok := parseGoTest(after)
		if !ok {
			continue
		}
		step := ciStep{goTest: command}
		// The step's own env block, which ends at the next step.
		for _, follower := range lines[i+1 : min(i+8, len(lines))] {
			if strings.HasPrefix(strings.TrimSpace(follower), "- ") {
				break
			}
			if match := variable.FindStringSubmatch(follower); match != nil {
				step.env = append(step.env, match[1])
			}
		}
		steps = append(steps, step)
	}
	return steps
}

// coveredBy reports whether some recipe line runs the step. `./...` covers a
// narrower pattern -- the end-to-end gates are ordinary packages, so the full
// suite runs them -- but only for a step no variable selects.
func coveredBy(recipes []string, want ciStep) bool {
	for _, line := range recipes {
		trimmed := strings.TrimSpace(line)
		_, after, ok := strings.Cut(trimmed, "go test ")
		if !ok {
			continue
		}
		got, ok := parseGoTest(after)
		if !ok {
			continue
		}
		// Written as what DOES cover rather than what does not: the
		// negated conjunction this replaces read as a double negative at
		// the one line where getting the direction wrong would pass a
		// weakened target.
		covers := got.packages == want.packages ||
			(got.packages == "./..." && len(want.env) == 0)
		if !covers {
			continue
		}
		// -v is output, not a gate: a run that certifies the same packages
		// under the same flags certifies them just as well quietly.
		gate := ciStep{goTest: goTest{packages: want.packages}}
		for _, flag := range want.flags {
			if flag != "-v" {
				gate.flags = append(gate.flags, flag)
			}
		}
		if len(gate.flagsMissingFrom(got)) > 0 {
			continue
		}
		missingEnv := false
		for _, name := range want.env {
			if !strings.Contains(line, name) {
				missingEnv = true
			}
		}
		if !missingEnv {
			return true
		}
	}
	return false
}

// targetRunning names a target whose own recipe runs the step, so a failure
// can say which `make` command the reader wanted. Sorted, because a map walk
// would name a different one of several runners each time it failed.
func targetRunning(rules map[string]makeRule, want ciStep) string {
	names := make([]string, 0, len(rules))
	for name := range rules {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if coveredBy(rules[name].recipe, want) {
			return name
		}
	}
	return ""
}

// namesTarget looks for a target NAME, not a substring of one: `test` is
// inside `test-pulsar`, so a plain Contains lets the notice about one gate
// stand in as the announcement of another -- which is exactly the silence
// this test exists to refuse.
func namesTarget(notice, target string) bool {
	bounded := regexp.MustCompile(`(^|[^a-z0-9-])` + regexp.QuoteMeta(target) + `([^a-z0-9-]|$)`)
	return bounded.MatchString(notice)
}

// plainGates are the workflow's checks that are not `go test`: the build, vet,
// the gofmt wrapper and the linter.
func plainGates(workflow string) []string {
	gates := map[string]string{
		"go build ./...":   "go build ./...",
		"go vet ./...":     "go vet ./...",
		"gofmt -l .":       "gofmt -l .",
		"golangci-lint":    "golangci-lint",
		"golangci-lint-ac": "", // the action's own name is not a command
	}
	var found []string
	for _, line := range strings.Split(workflow, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		for probe, command := range gates {
			if command != "" && strings.Contains(trimmed, probe) &&
				!containsAny(found, command) {
				found = append(found, command)
			}
		}
	}
	sort.Strings(found)
	return found
}

func containsAny(lines []string, probe string) bool {
	for _, line := range lines {
		if strings.Contains(line, probe) {
			return true
		}
	}
	return false
}

// makeRule is a target's prerequisites and its recipe.
type makeRule struct {
	prereqs []string
	recipe  []string
}

// makeRules parses the file into targets. A recipe is the run of TAB-indented
// lines under a rule head; comments and blank lines between them do not end
// it, which is how make reads them too.
func makeRules(makefile string) map[string]makeRule {
	rules := map[string]makeRule{}
	current := ""
	for _, line := range strings.Split(makefile, "\n") {
		if match := makeRuleHead.FindStringSubmatch(line); match != nil {
			current = match[1]
			prereqs, _, _ := strings.Cut(match[2], "##")
			rules[current] = makeRule{prereqs: strings.Fields(prereqs)}
			continue
		}
		if current == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "\t"):
			rule := rules[current]
			// A trailing backslash continues the LOGICAL line, and make hands
			// the joined result to one shell — which is where a recipe's
			// `VAR=value \` prefix lives, several physical lines above the
			// command it applies to. Reading the lines separately loses it.
			if n := len(rule.recipe); n > 0 && strings.HasSuffix(rule.recipe[n-1], "\\") {
				rule.recipe[n-1] = strings.TrimSuffix(rule.recipe[n-1], "\\") +
					" " + strings.TrimSpace(line)
			} else {
				rule.recipe = append(rule.recipe, strings.TrimRight(line, " "))
			}
			rules[current] = rule
		case strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#"):
		default:
			current = ""
		}
	}
	return rules
}

var makeRuleHead = regexp.MustCompile(`^([a-z][a-z0-9-]*):([^=]*)$`)

// reachableFrom walks the prerequisite graph, which is what `make check`
// actually runs.
func reachableFrom(rules map[string]makeRule, root string) map[string]bool {
	seen := map[string]bool{}
	var walk func(string)
	walk = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		for _, prereq := range rules[name].prereqs {
			if _, ok := rules[prereq]; ok {
				walk(prereq)
			}
		}
	}
	walk(root)
	return seen
}
