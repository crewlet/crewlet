package code_test

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// wantFault is one fault a refused file must hold: the authored path, the kind
// it is classified as, and the line it is located at (0 for none).
type wantFault struct {
	path string
	kind error
	line int
}

// faultsOf is a refused file's faults, holding the refusal to its contract on
// the way: a join of *config.Fault that IS code.ErrInvalid, every fault one of
// config's classified kinds — so `crewlet validate -json` reports each one as a
// config document's problem, never as `invalid`.
func faultsOf(t *testing.T, err error) []*config.Fault {
	t.Helper()
	var joined interface{ Unwrap() []error }
	switch {
	case err == nil:
		t.Fatal("the file parsed")
	case !errors.As(err, &joined):
		t.Fatalf("the refusal is a %T, not a join of faults: %v", err, err)
	case !errors.Is(err, code.ErrInvalid):
		t.Fatalf("the refusal is not code.ErrInvalid: %v", err)
	}
	var faults []*config.Fault
	for _, e := range joined.Unwrap() {
		f, ok := e.(*config.Fault)
		if !ok {
			t.Fatalf("the refusal joins a %T, not a *config.Fault: %v", e, e)
		}
		faults = append(faults, f)
	}
	if len(faults) == 0 {
		t.Fatalf("the refusal holds no fault: %v", err)
	}
	problems := config.Problems(err)
	if len(problems) != len(faults) {
		t.Fatalf("config.Problems reads %d problems from %d faults: %+v", len(problems), len(faults), problems)
	}
	for i, p := range problems {
		if p.Kind == "invalid" {
			t.Errorf("fault %d (%v) is no kind config classifies", i, faults[i])
		}
	}
	return faults
}

// listAtTheBound is a file of one valid check whose key holds a flow list of
// the items item spells, as many as fit in MaxChecksFileBytes: the most items
// one list of a file can hold.
func listAtTheBound(key string, item func(i int) string) string {
	var b strings.Builder
	b.WriteString("checks:\n  - name: unit\n    run: make test\n    timeout_seconds: 600\n    " + key + ": [")
	for i := 0; ; i++ {
		next := item(i)
		if i > 0 {
			next = ", " + next
		}
		if b.Len()+len(next)+len("]\n") > code.MaxChecksFileBytes {
			break
		}
		b.WriteString(next)
	}
	b.WriteString("]\n")
	return b.String()
}

// A CHECKS FILE PARSES STRICTLY: every check of a whole file reads into the
// definition it spells, and every way a file can say something other than
// what a reader takes it to say is refused — at its authored path, as the kind
// of mistake it is, on its line — and every fault in a file is reported, not
// the first: up to MaxChecksFileFaults of them, and a count of the rest.
//
// And a parse costs no more than the file's bound: a list past its bound is
// ONE fault, its count, whatever it holds and however many items a 64 KiB file
// packs into it — none of them read, judged or compared with another.
//
// Mutation: accept an unknown key of a check (drop its fault) and "an unknown
// key" goes red; let a key written twice through and "a key written twice"
// does; read a list's items before its count, and "numbers past the paths'
// bound" gets a fault an item; list every fault, and "faults past the listed
// ones" lists them all.
func TestAChecksFileParsesStrictly(t *testing.T) {
	t.Parallel()
	const whole = `# The repository's checks.
checks:
  - name: unit
    run: make test
    timeout_seconds: 600
    paths: ["**/*.go", go.mod]
    report: {junit: out/junit.xml, sarif: out/lint.sarif}
    run_in: container
    checkout: [cmd, internal]
    env: [NPM_TOKEN]
  - name: api-shape
    run: |
      curl -fsS "$CREWLET_CHECK_API/tree" >/dev/null
      echo ok
    timeout_seconds: 0x3c
    run_in: e2b
    checkout: none
`
	file, err := code.ParseChecks([]byte(whole))
	if err != nil {
		t.Fatalf("a whole file was refused: %v", err)
	}
	want := code.ChecksFile{Checks: []code.CheckDef{
		{Name: "unit", Run: "make test", TimeoutSeconds: 600, Paths: []string{"**/*.go", "go.mod"},
			Report: code.CheckReport{JUnit: "out/junit.xml", SARIF: "out/lint.sarif"}, RunIn: code.CellContainer,
			Checkout: code.CheckCheckout{Cone: []string{"cmd", "internal"}}, Env: []string{"NPM_TOKEN"}},
		{Name: "api-shape", Run: "curl -fsS \"$CREWLET_CHECK_API/tree\" >/dev/null\necho ok\n",
			TimeoutSeconds: 60, RunIn: code.CellE2B, Checkout: code.CheckCheckout{None: true}},
	}}
	if !reflect.DeepEqual(file, want) {
		t.Fatalf("the whole file read as\n%+v\nwant\n%+v", file, want)
	}
	for _, def := range file.Checks {
		if err := def.Validate(); err != nil {
			t.Errorf("%s parsed and does not validate: %v", def.Name, err)
		}
	}
	if got, err := code.ParseChecks([]byte("checks: []\n")); err != nil || len(got.Checks) != 0 {
		t.Errorf("a file defining no checks read as %+v, %v", got, err)
	}

	// valid is one check that holds up, for a case to break one thing of.
	const valid = "checks:\n  - name: unit\n    run: make test\n    timeout_seconds: 600\n"
	cone := make([]string, sandbox.MaxConePaths+1)
	for i := range cone {
		cone[i] = fmt.Sprintf("dir%02d", i)
	}
	// MORE FAULTS THAN A REFUSAL LISTS: a key the grammar does not know,
	// fifty more times than the refusal lists — each one listed in its turn
	// up to the bound, and the rest counted by one fault at no field.
	var unknown strings.Builder
	var listed []wantFault
	unknown.WriteString(valid)
	for i := range code.MaxChecksFileFaults + 50 {
		fmt.Fprintf(&unknown, "    k%04d: 1\n", i)
		if i < code.MaxChecksFileFaults {
			listed = append(listed, wantFault{fmt.Sprintf("checks[0].k%04d", i), config.ErrUnknownField, 5 + i})
		}
	}
	listed = append(listed, wantFault{"", config.ErrOutOfRange, 0})
	for _, tc := range []struct {
		name string
		doc  string
		want []wantFault
	}{
		{"an unknown key", valid + "    timout_seconds: 60\n",
			[]wantFault{{"checks[0].timout_seconds", config.ErrUnknownField, 5}}},
		{"an unknown key at the root", valid + "version: 2\n",
			[]wantFault{{"version", config.ErrUnknownField, 5}}},
		{"an unknown report", valid + "    report: {junit: a.xml, tap: a.tap}\n",
			[]wantFault{{"checks[0].report.tap", config.ErrUnknownField, 5}}},
		{"a duplicate name", valid + "  - name: unit\n    run: make lint\n    timeout_seconds: 60\n",
			[]wantFault{{"checks[1].name", config.ErrConflict, 5}}},
		{"a missing timeout", "checks:\n  - name: unit\n    run: make test\n",
			[]wantFault{{"checks[0].timeout_seconds", config.ErrMissing, 2}}},
		{"a missing name and run", "checks:\n  - timeout_seconds: 60\n",
			[]wantFault{{"checks[0].name", config.ErrMissing, 2}, {"checks[0].run", config.ErrMissing, 2}}},
		{"a cone past its directories", valid + "    checkout: [" + strings.Join(cone, ", ") + "]\n",
			[]wantFault{{"checks[0].checkout", config.ErrOutOfRange, 5}}},
		{"a key written twice", valid + "    run: make lint\n",
			[]wantFault{{"checks[0].run", config.ErrConflict, 5}}},
		{"a number where text belongs", "checks:\n  - name: 1.10\n    run: make\n    timeout_seconds: 60\n",
			[]wantFault{{"checks[0].name", config.ErrShape, 2}}},
		{"a boolean where text belongs", "checks:\n  - name: unit\n    run: true\n    timeout_seconds: 60\n",
			[]wantFault{{"checks[0].run", config.ErrShape, 3}}},
		{"a run with no value", "checks:\n  - name: unit\n    run:\n    timeout_seconds: 60\n",
			[]wantFault{{"checks[0].run", config.ErrMissing, 3}}},
		{"a blank run", "checks:\n  - name: unit\n    run: '  '\n    timeout_seconds: 60\n",
			[]wantFault{{"checks[0].run", config.ErrMissing, 3}}},
		{"a fraction of a second", "checks:\n  - name: unit\n    run: make\n    timeout_seconds: 1.5\n",
			[]wantFault{{"checks[0].timeout_seconds", config.ErrShape, 4}}},
		{"a timeout past the ceiling", "checks:\n  - name: unit\n    run: make\n    timeout_seconds: 21601\n",
			[]wantFault{{"checks[0].timeout_seconds", config.ErrOutOfRange, 4}}},
		{"a timeout of nothing", "checks:\n  - name: unit\n    run: make\n    timeout_seconds: 0\n",
			[]wantFault{{"checks[0].timeout_seconds", config.ErrOutOfRange, 4}}},
		{"a name outside the grammar", "checks:\n  - name: Unit\n    run: make\n    timeout_seconds: 60\n",
			[]wantFault{{"checks[0].name", config.ErrUnknownValue, 2}}},
		{"a path that is no glob", valid + "    paths: [/src/**]\n",
			[]wantFault{{"checks[0].paths[0]", config.ErrUnknownValue, 5}}},
		{"a glob named twice", valid + "    paths: [src/**, src/**]\n",
			[]wantFault{{"checks[0].paths[1]", config.ErrConflict, 5}}},
		{"a report outside the checkout", valid + "    report: {junit: ../junit.xml}\n",
			[]wantFault{{"checks[0].report.junit", config.ErrUnknownValue, 5}}},
		{"two reports of one file", valid + "    report: {junit: out.xml, sarif: out.xml}\n",
			[]wantFault{{"checks[0].report.sarif", config.ErrConflict, 5}}},
		{"an empty cone", valid + "    checkout: []\n",
			[]wantFault{{"checks[0].checkout", config.ErrShape, 5}}},
		{"a cone outside the repository", valid + "    checkout: [src, ../etc]\n",
			[]wantFault{{"checks[0].checkout[1]", config.ErrUnknownValue, 5}}},
		{"a checkout of some other word", valid + "    checkout: all\n",
			[]wantFault{{"checks[0].checkout", config.ErrShape, 5}}},
		{"a scalar where a list belongs", valid + "    env: NPM_TOKEN\n",
			[]wantFault{{"checks[0].env", config.ErrShape, 5}}},
		{"a variable no shell names", valid + "    env: [NPM-TOKEN]\n",
			[]wantFault{{"checks[0].env[0]", config.ErrUnknownValue, 5}}},
		{"a cell the sandbox does not have", valid + "    run_in: lambda\n",
			[]wantFault{{"checks[0].run_in", config.ErrUnknownValue, 5}}},
		{"an alias", "base: &base make test\nchecks:\n  - name: unit\n    run: *base\n    timeout_seconds: 60\n",
			[]wantFault{{"base", config.ErrUnknownField, 1}, {"checks[0].run", config.ErrShape, 4}}},
		{"a merge key", "checks:\n  - &one {name: unit, run: make, timeout_seconds: 60}\n" +
			"  - <<: *one\n    name: lint\n",
			[]wantFault{{"checks[1]", config.ErrShape, 3}, {"checks[1].run", config.ErrMissing, 3},
				{"checks[1].timeout_seconds", config.ErrMissing, 3}}},
		{"a list of checks that is no list", "checks: {unit: make}\n",
			[]wantFault{{"checks", config.ErrShape, 1}}},
		{"a check that is no mapping", "checks:\n  - make test\n",
			[]wantFault{{"checks[0]", config.ErrShape, 2}}},
		{"no checks at all", "{}\n", []wantFault{{"checks", config.ErrMissing, 1}}},
		{"a file of nothing", "", []wantFault{{"", config.ErrMissing, 0}}},
		{"a file that is no mapping", "- unit\n", []wantFault{{"", config.ErrShape, 1}}},
		{"two documents", valid + "---\n" + valid, []wantFault{{"", config.ErrShape, 0}}},
		{"text that is no YAML", "checks: [unit\n", []wantFault{{"", config.ErrShape, 0}}},
		{"a file past its bytes", "checks: []\n#" + strings.Repeat("x", code.MaxChecksFileBytes),
			[]wantFault{{"", config.ErrOutOfRange, 0}}},
		// EVERY FAULT, NOT THE FIRST — a typo'd key does not hide a name
		// outside the grammar in the same check, nor a fault in the next.
		{"every fault in a file", "checks:\n  - name: Unit\n    run: make\n    timeout_seconds: 60\n" +
			"    timout: 5\n  - name: lint\n    run: make lint\n    timeout_seconds: 99999\n",
			[]wantFault{{"checks[0].timout", config.ErrUnknownField, 5},
				{"checks[0].name", config.ErrUnknownValue, 2},
				{"checks[1].timeout_seconds", config.ErrOutOfRange, 8}}},
		{"faults past the listed ones", unknown.String(), listed},
		// A LIST PAST ITS BOUND IS ITS COUNT, at the most items a file holds:
		// numbers, each a fault of its own were it read; names no shell
		// takes, each a fault were it judged; and a cone of directories
		// outside the repository.
		{"numbers past the paths' bound", listAtTheBound("paths", strconv.Itoa),
			[]wantFault{{"checks[0].paths", config.ErrOutOfRange, 5}}},
		{"names past the env's bound", listAtTheBound("env", func(i int) string { return fmt.Sprint(i, "x") }),
			[]wantFault{{"checks[0].env", config.ErrOutOfRange, 5}}},
		{"directories past the cone's bound",
			listAtTheBound("checkout", func(i int) string { return fmt.Sprint("../", i) }),
			[]wantFault{{"checks[0].checkout", config.ErrOutOfRange, 5}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := code.ParseChecks([]byte(tc.doc))
			faults := faultsOf(t, err)
			var got []wantFault
			for _, f := range faults {
				got = append(got, wantFault{f.Path.String(), f.Kind, f.Line})
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("faults\n%v\nwant\n%v\n(%v)", got, tc.want, err)
			}
		})
	}
	_, counted := code.ParseChecks([]byte(unknown.String()))
	if faults := faultsOf(t, counted); !strings.Contains(faults[len(faults)-1].Detail, "50 more faults") {
		t.Errorf("the refusal's last fault says %q; want it to count the 50 faults it does not list",
			faults[len(faults)-1].Detail)
	}
}

// A DIRECT CHECK IS REFUSED (U1 a), and so is a check in a seat's own box: a
// check runs a change's own code, which `direct` would run as the engine's
// own user on the engine host, and `self` is an executor box no check has.
// Each is refused by name, with why, at its field — in a file and on a
// definition a record carries alike — while every cell a check runs in parses.
// And the cells are the sandbox's own placements that need a backend, less
// direct, BOTH WAYS, so a placement the sandbox adds is a decision here.
//
// Mutation: drop direct from refusedCells and it is refused as an unknown
// value with no reason — red; add direct to CheckCells and it parses — red.
func TestADirectCheckIsRefused(t *testing.T) {
	t.Parallel()
	for cell, why := range map[string]string{"direct": "engine host", "self": "executor box"} {
		doc := "checks:\n  - name: unit\n    run: make test\n    timeout_seconds: 60\n    run_in: " + cell + "\n"
		_, err := code.ParseChecks([]byte(doc))
		faults := faultsOf(t, err)
		if len(faults) != 1 || faults[0].Path.String() != "checks[0].run_in" ||
			!errors.Is(faults[0].Kind, config.ErrUnknownValue) || !strings.Contains(faults[0].Detail, why) ||
			faults[0].Line != 5 {
			t.Errorf("run_in: %s drew %v; want one unknown value at checks[0].run_in, line 5, saying %q",
				cell, err, why)
		}
		def := code.CheckDef{Name: "unit", Run: "make test", TimeoutSeconds: 60, RunIn: code.CheckCell(cell)}
		if err := def.Validate(); !errors.Is(err, code.ErrInvalid) || !strings.Contains(err.Error(), "run_in") ||
			!strings.Contains(err.Error(), why) {
			t.Errorf("a definition in %s validated as %v; want ErrInvalid naming run_in and why", cell, err)
		}
	}
	for _, cell := range code.CheckCells() {
		doc := "checks:\n  - name: unit\n    run: make test\n    timeout_seconds: 60\n    run_in: " +
			string(cell) + "\n"
		if _, err := code.ParseChecks([]byte(doc)); err != nil {
			t.Errorf("a check in %s was refused: %v", cell, err)
		}
	}
	for _, p := range config.Placements {
		runs := p.NeedsBackend() && p != config.PlacementDirect
		if code.CheckCell(p).Valid() != runs {
			t.Errorf("the sandbox's %s needs a backend: %v; a check runs there: %v — a check runs in "+
				"every cell a box is built for but the engine host's own", p, p.NeedsBackend(),
				code.CheckCell(p).Valid())
		}
	}
	for _, cell := range code.CheckCells() {
		if !slices.Contains(config.Placements, config.Placement(cell)) {
			t.Errorf("a check runs in %s, which is no sandbox placement", cell)
		}
	}
}

// checksYAML writes a parsed file back as the YAML that spells it — the round
// trip FuzzParseChecks holds a file to.
func checksYAML(t *testing.T, file code.ChecksFile) []byte {
	t.Helper()
	checks := make([]map[string]any, 0, len(file.Checks))
	for _, d := range file.Checks {
		c := map[string]any{"name": d.Name, "run": d.Run, "timeout_seconds": d.TimeoutSeconds}
		if d.Paths != nil {
			c["paths"] = d.Paths
		}
		report := map[string]string{}
		if d.Report.JUnit != "" {
			report["junit"] = d.Report.JUnit
		}
		if d.Report.SARIF != "" {
			report["sarif"] = d.Report.SARIF
		}
		if len(report) > 0 {
			c["report"] = report
		}
		if d.RunIn != "" {
			c["run_in"] = string(d.RunIn)
		}
		switch {
		case d.Checkout.None:
			c["checkout"] = "none"
		case d.Checkout.Cone != nil:
			c["checkout"] = d.Checkout.Cone
		}
		if d.Env != nil {
			c["env"] = d.Env
		}
		checks = append(checks, c)
	}
	out, err := yaml.Marshal(map[string]any{"checks": checks})
	if err != nil {
		t.Fatalf("write the file back: %v", err)
	}
	return out
}

// ANY BYTES, ONE OF TWO ANSWERS: a file every check of which holds to the
// grammar — each name once, each definition one Validate passes — and which
// reads back as itself once written out again; or a refusal holding at least
// one classified fault. Never a panic, never a file with a check no reader
// may run.
func FuzzParseChecks(f *testing.F) {
	for _, seed := range []string{
		"checks: []\n",
		"checks:\n  - name: unit\n    run: make test\n    timeout_seconds: 600\n",
		"checks:\n  - {name: a, run: x, timeout_seconds: 1, paths: ['**/*.go'], checkout: none}\n",
		"checks:\n  - name: b\n    run: |\n      x\n      y\n    timeout_seconds: 21600\n" +
			"    report: {junit: a/b.xml}\n    checkout: [src]\n    env: [A, B_2]\n    run_in: e2b\n",
		"checks:\n  - name: c\n    run: make\n    timeout_seconds: 60\n    run_in: direct\n",
		"base: &b x\nchecks:\n  - name: d\n    run: *b\n    timeout_seconds: 1\n",
		"checks:\n  - name: '123'\n    run: \"true\"\n    timeout_seconds: 0o17\n",
		"checks: [\n",
		"---\n---\n",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		file, err := code.ParseChecks(raw)
		if err != nil {
			faultsOf(t, err)
			return
		}
		if len(file.Checks) > code.MaxChecksPerFile {
			t.Fatalf("a file of %d checks parsed", len(file.Checks))
		}
		seen := map[string]bool{}
		for _, d := range file.Checks {
			if seen[d.Name] {
				t.Fatalf("%q named twice in one parsed file", d.Name)
			}
			seen[d.Name] = true
			if err := d.Validate(); err != nil {
				t.Fatalf("a parsed check does not validate: %v", err)
			}
		}
		again, err := code.ParseChecks(checksYAML(t, file))
		if err != nil {
			t.Fatalf("a parsed file written back does not parse: %v", err)
		}
		if !reflect.DeepEqual(again, file) {
			t.Fatalf("a parsed file written back reads as\n%+v\nnot\n%+v", again, file)
		}
	})
}

// BenchmarkParseChecks is ONE PARSE of a checks file at its bound — what the
// home git node pays for the file on every change it dispatches, and what
// `crewlet validate` pays for it on a laptop:
//
//   - whole: the most a VALID file makes a parse judge — as many checks as
//     fit in MaxChecksFileBytes, each with every list at its bound (32 path
//     globs, a cone of 64 directories, 32 variable names), every item read
//     and judged;
//   - paths, env and cone: one list holding as many items as fit in
//     MaxChecksFileBytes — numbers where globs belong, names no shell takes,
//     directories outside the repository — each refused by its count alone;
//   - keys: a check of as many keys the grammar does not know as fit in the
//     file, the refusal listing MaxChecksFileFaults of them.
//
// checksfile.go's package doc records the figures.
func BenchmarkParseChecks(b *testing.B) {
	// list is n items, the i-th spelled as format spells i, as a flow list.
	list := func(n int, format string) string {
		items := make([]string, n)
		for i := range items {
			items[i] = fmt.Sprintf(format, i)
		}
		return "[" + strings.Join(items, ", ") + "]"
	}
	var whole strings.Builder
	whole.WriteString("checks:\n")
	for c := range code.MaxChecksPerFile {
		check := fmt.Sprintf("  - name: check-%02d\n    run: make check-%02d\n    timeout_seconds: 600\n"+
			"    paths: %s\n    checkout: %s\n    env: %s\n", c, c, list(code.MaxCheckPaths, "src/p%02d/**"),
			list(sandbox.MaxConePaths, "d%02d"), list(code.MaxCheckEnv, "E%02d"))
		if whole.Len()+len(check) > code.MaxChecksFileBytes {
			break
		}
		whole.WriteString(check)
	}
	var keys strings.Builder
	keys.WriteString("checks:\n  - name: unit\n    run: make test\n    timeout_seconds: 600\n")
	for i := 0; keys.Len()+len("    k00000: 1\n") <= code.MaxChecksFileBytes; i++ {
		fmt.Fprintf(&keys, "    k%05d: 1\n", i)
	}
	for _, arm := range []struct {
		name  string
		doc   string
		valid bool
	}{
		{"whole", whole.String(), true},
		{"paths", listAtTheBound("paths", strconv.Itoa), false},
		{"env", listAtTheBound("env", func(i int) string { return fmt.Sprint(i, "x") }), false},
		{"cone", listAtTheBound("checkout", func(i int) string { return fmt.Sprint("../", i) }), false},
		{"keys", keys.String(), false},
	} {
		b.Run(arm.name, func(b *testing.B) {
			doc := []byte(arm.doc)
			if len(doc) > code.MaxChecksFileBytes {
				b.Fatalf("the %s file is %d bytes, past the bound", arm.name, len(doc))
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := code.ParseChecks(doc); (err == nil) != arm.valid {
					b.Fatalf("the %s file parsed to %v", arm.name, err)
				}
			}
		})
	}
}
