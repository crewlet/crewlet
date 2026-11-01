package config_test

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
)

// cells is a company's sandbox catalogue: the local backend (with an image or
// without), the remote one, and the seats' default cell.
type cells struct {
	local, image, e2b bool
	defaultRunIn      string
}

// checksDoc is a company hosting code over the catalogue cat, its code.checks
// the given YAML lines, each written under `checks:`.
func checksDoc(cat cells, checks ...string) string {
	doc := "name: Acme\n"
	if cat.local || cat.e2b {
		doc += "providers:\n  sandbox:\n"
		switch {
		case cat.local && cat.image:
			doc += "    local: {image: \"ghcr.io/example/box:1\"}\n"
		case cat.local:
			doc += "    local: {}\n"
		}
		if cat.e2b {
			doc += "    e2b: {api_key: \"${E2B_API_KEY}\"}\n"
		}
		if cat.defaultRunIn != "" {
			doc += "    default_run_in: " + cat.defaultRunIn + "\n"
		}
	}
	doc += "code:\n  native: true\n"
	if len(checks) > 0 {
		doc += "  checks:\n"
		for _, line := range checks {
			doc += "    " + line + "\n"
		}
	}
	return doc
}

// A DIRECT CHECK CELL IS REFUSED (M3 decision U1 a), by name and with why: a
// check runs a change's own code, which a `direct` box would run as the
// engine's own user on the engine host, reading what the engine reads.
// Refused at code.checks.run_in and at each of allow_run_in alike, as a RUNNABLE rule —
// a stored revision carrying one is refused to run, not merely warned about —
// and such a cell reaches no backend.
//
// Mutations: drop direct from config.CodeCheckCellRefusal — refused with no
// reason, red; add direct to config.CodeCheckCells — taken, red.
func TestADirectCheckCellIsRefused(t *testing.T) {
	t.Parallel()
	local := cells{local: true, defaultRunIn: "direct"}
	for name, tc := range map[string]struct {
		lines []string
		path  string
		says  string
	}{
		"run_in: direct":       {[]string{"run_in: direct"}, "code.checks.run_in", "engine host"},
		"direct allowed":       {[]string{"allow_run_in: [direct]"}, "code.checks.allow_run_in[0]", "engine host"},
		"direct allowed later": {[]string{"allow_run_in: [e2b, direct]"}, "code.checks.allow_run_in[1]", "engine host"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cat := local
			if strings.Contains(name, "later") {
				cat = cells{local: true, e2b: true, defaultRunIn: "e2b"}
			}
			doc := checksDoc(cat, tc.lines...)
			_, err := config.ParseCompany([]byte(doc))
			problems := config.Problems(err)
			if len(problems) != 1 || problems[0].Path != tc.path || problems[0].Kind != "unknown_value" ||
				!strings.Contains(problems[0].Message, "a check never runs in") ||
				!strings.Contains(problems[0].Message, tc.says) {
				t.Fatalf("problems = %+v, want one unknown value at %s saying why (%q)", problems, tc.path, tc.says)
			}
			stored, err := config.ParseCompanyDocument([]byte(doc))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if err := stored.ValidateRunnable(); err == nil {
				t.Error("a stored revision whose checks run on the engine host runs")
			}
			for cell, at := range stored.SandboxPlacements() {
				if strings.HasPrefix(at, "code.checks") {
					t.Errorf("a refused check cell reaches the %s backend (%s)", cell, at)
				}
			}
		})
	}
	for _, cell := range config.CodeCheckCells {
		mustParse(t, checksDoc(cells{local: true, image: true, e2b: true, defaultRunIn: "container"},
			"run_in: "+string(cell)))
	}
}

// mustParse parses a company document the case expects to be valid.
func mustParse(t *testing.T, doc string) *config.Company {
	t.Helper()
	c, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("refused:\n%v\n%s", err, doc)
	}
	return c
}

// THE CHECKS BLOCK IS HELD TO ITS RULES, each refused at the line an operator
// wrote, one mistake one line: its timeout within a minute and six hours, a
// secret's name one a check can name, each cell one a check runs in and
// allowed once, every cell it names served by a backend the catalogue
// configures — a check's box is made from it, on a git node — the cells it
// allows holding the one a check naming none runs in, and the image a check's
// container needs where only a check reaches one. And nothing of it on a
// company hosting no code.
//
// Mutations: compare the allowed cells with code.checks.run_in alone — the
// catalogue's default left out passes, red; drop the checks from
// SandboxPlacements — the image a check's container needs is never asked
// for, red.
func TestTheChecksBlockIsHeldToItsRules(t *testing.T) {
	t.Parallel()
	direct := cells{local: true, defaultRunIn: "direct"}
	both := cells{local: true, image: true, e2b: true, defaultRunIn: "container"}
	for name, tc := range map[string]struct {
		doc string
		// path and kind are the one problem the document must be refused
		// with; an empty path accepts it.
		path, kind string
		says       []string
		not        []string
	}{
		"nothing set": {doc: checksDoc(direct)},
		"every field at a bound": {doc: checksDoc(both, "run_in: container", "allow_run_in: [container, e2b]",
			"env: {NPM_TOKEN: \"${NPM_TOKEN}\", _A1: x}", "max_timeout_seconds: 21600")},
		"the timeout at its floor": {doc: checksDoc(direct, "max_timeout_seconds: 60")},
		"a catalogue only a check reaches": {doc: checksDoc(cells{local: true, image: true},
			"run_in: container")},
		"a timeout under a minute": {doc: checksDoc(direct, "max_timeout_seconds: 59"),
			path: "code.checks.max_timeout_seconds", kind: "out_of_range", says: []string{"60..21600", "3000"}},
		"a timeout past six hours": {doc: checksDoc(direct, "max_timeout_seconds: 21601"),
			path: "code.checks.max_timeout_seconds", kind: "out_of_range", says: []string{"21601", "60..21600"}},
		"a negative timeout": {doc: checksDoc(direct, "max_timeout_seconds: -1"),
			path: "code.checks.max_timeout_seconds", kind: "out_of_range"},
		"a secret no shell exports": {doc: checksDoc(direct, "env: {1PASSWORD: x}"),
			path: "code.checks.env.1PASSWORD", kind: "shape", says: []string{"a letter or an underscore"}},
		"a secret's name past its bound": {
			doc:  checksDoc(direct, "env: {"+strings.Repeat("A", config.CodeMaxCheckEnvNameBytes+1)+": x}"),
			path: "code.checks.env." + strings.Repeat("A", config.CodeMaxCheckEnvNameBytes+1), kind: "shape",
			says: []string{"128 bytes"}},
		"a cell no check runs in": {doc: checksDoc(direct, "run_in: gpu"),
			path: "code.checks.run_in", kind: "unknown_value", says: []string{`"container" or "e2b"`}},
		"a cell allowed twice": {doc: checksDoc(both, "allow_run_in: [container, container]"),
			path: "code.checks.allow_run_in[1]", kind: "conflict", says: []string{"allow_run_in[0]"}},
		"checks with nothing hosting code": {doc: "name: Acme\ncode:\n  checks:\n    max_timeout_seconds: 600\n",
			path: "code.checks", kind: "conflict", says: []string{"code.native"}},
		"a cell no backend serves": {doc: checksDoc(direct, "run_in: e2b"),
			path: "code.checks.run_in", kind: "conflict", says: []string{"`e2b:`", `"direct" or "container"`}},
		"an allowed cell no backend serves": {
			doc:  checksDoc(cells{local: true, image: true}, "run_in: container", "allow_run_in: [container, e2b]"),
			path: "code.checks.allow_run_in[1]", kind: "conflict", says: []string{"`e2b:`"}},
		"a cell and no catalogue": {doc: checksDoc(cells{}, "run_in: container"),
			path: "code.checks.run_in", kind: "missing", says: []string{"configures no sandbox", "`local:`"}},
		"allowed cells leaving run_in out": {doc: checksDoc(both, "run_in: container", "allow_run_in: [e2b]"),
			path: "code.checks.allow_run_in", kind: "conflict", says: []string{"runs in container", "code.checks.run_in"}},
		"allowed cells leaving the catalogue's default out": {
			doc:  checksDoc(cells{local: true, image: true, e2b: true, defaultRunIn: "e2b"}, "allow_run_in: [container]"),
			path: "code.checks.allow_run_in", kind: "conflict", says: []string{"runs in e2b", "providers.sandbox's default"}},
		"a check's container with no image": {doc: checksDoc(direct, "run_in: container"),
			path: "providers.sandbox.local.image", kind: "missing",
			says: []string{"code.checks.run_in runs in a container", "git 2.40"}, not: []string{"coding-agent"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := config.ParseCompany([]byte(tc.doc))
			if tc.path == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			problems := config.Problems(err)
			if len(problems) != 1 {
				t.Fatalf("problems = %+v, want the one at %s: one mistake, one line", problems, tc.path)
			}
			p := problems[0]
			if p.Path != tc.path || p.Kind != tc.kind {
				t.Errorf("refused at %s (%s), want %s (%s): %s", p.Path, p.Kind, tc.path, tc.kind, p.Message)
			}
			for _, want := range tc.says {
				if !strings.Contains(p.Message, want) {
					t.Errorf("message = %q, want it to name %q", p.Message, want)
				}
			}
			for _, not := range tc.not {
				if strings.Contains(p.Message, not) {
					t.Errorf("message = %q, which says %q of a box no coding agent runs in", p.Message, not)
				}
			}
		})
	}
}

// A CHECK'S CELL IS REACHED FOR ITS BACKEND, from the one resolution of where a
// check goes: code.checks.run_in, else the catalogue's own default where a
// check runs there, else none; every allowed cell reached where any is, each
// at the path that named it first — so the engine builds a check's backend as
// it builds a seat's, and a check's cell reaches nothing on a company hosting
// no code.
//
// Mutation: drop the checks from SandboxPlacements — the allowed cells reach
// no backend, red.
func TestACheckCellIsReachedForItsBackend(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		doc     string
		runIn   config.Placement
		reached map[config.Placement]string
	}{
		"allowed cells beside a seat default": {
			doc: checksDoc(cells{local: true, image: true, e2b: true, defaultRunIn: "direct"},
				"run_in: container", "allow_run_in: [container, e2b]"),
			runIn: config.PlacementContainer,
			reached: map[config.Placement]string{config.PlacementDirect: "providers.sandbox.default_run_in",
				config.PlacementContainer: "code.checks.allow_run_in[0]", config.PlacementE2B: "code.checks.allow_run_in[1]"},
		},
		"a check's own cell": {
			doc:   checksDoc(cells{local: true, image: true, defaultRunIn: "direct"}, "run_in: container"),
			runIn: config.PlacementContainer,
			reached: map[config.Placement]string{config.PlacementDirect: "providers.sandbox.default_run_in",
				config.PlacementContainer: "code.checks.run_in"},
		},
		"the catalogue's default, a check's cell": {
			doc:     checksDoc(cells{e2b: true}),
			runIn:   config.PlacementE2B,
			reached: map[config.Placement]string{config.PlacementE2B: "providers.sandbox"},
		},
		"the catalogue's default, no check's cell": {
			doc:     checksDoc(cells{local: true, defaultRunIn: "direct"}),
			reached: map[config.Placement]string{config.PlacementDirect: "providers.sandbox.default_run_in"},
		},
		"a company hosting no code": {
			doc:     "name: Acme\nproviders:\n  sandbox:\n    e2b: {api_key: \"${E2B_API_KEY}\"}\n",
			reached: map[config.Placement]string{config.PlacementE2B: "providers.sandbox"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := mustParse(t, tc.doc)
			if got := c.CheckRunIn(); got != tc.runIn {
				t.Errorf("a check naming no cell runs in %q, want %q", got, tc.runIn)
			}
			if got := c.SandboxPlacements(); !reflect.DeepEqual(got, tc.reached) {
				t.Errorf("SandboxPlacements() = %v, want %v", got, tc.reached)
			}
		})
	}
}

// A CHECK RUNS IN THE CELL ITS FILE NAMES ONLY WHERE THE COMPANY ALLOWS IT: a
// file naming none runs in the company's own cell, a file naming an allowed
// one runs there, and a file naming one the company does not allow — or a
// company resolving no cell at all — runs nowhere, with the sentence its
// ending is recorded with naming what to change.
//
// Mutation: answer a named cell as it is, whatever the company allows, and
// the file naming e2b runs on a company allowing containers alone, red.
func TestACheckRunsOnlyInACellTheCompanyAllows(t *testing.T) {
	t.Parallel()
	both := mustParse(t, checksDoc(cells{local: true, image: true, e2b: true, defaultRunIn: "direct"},
		"run_in: container", "allow_run_in: [container, e2b]"))
	containers := mustParse(t, checksDoc(cells{local: true, image: true, e2b: true, defaultRunIn: "direct"},
		"run_in: container"))
	none := mustParse(t, checksDoc(cells{local: true, defaultRunIn: "direct"}))
	for name, tc := range map[string]struct {
		c     *config.Company
		named config.Placement
		want  config.Placement
		why   string
	}{
		"naming none, the company's":    {c: containers, want: config.PlacementContainer},
		"naming an allowed cell":        {c: both, named: config.PlacementE2B, want: config.PlacementE2B},
		"naming a cell not allowed":     {c: containers, named: config.PlacementE2B, why: "does not allow"},
		"a company resolving no cell":   {c: none, why: "names no cell a check runs in"},
		"naming a cell, no cell at all": {c: none, named: config.PlacementContainer, why: "does not allow"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, why := tc.c.CheckCell(tc.named)
			if got != tc.want || (tc.why == "") != (why == "") || !strings.Contains(why, tc.why) {
				t.Errorf("CheckCell(%q) = %q, %q; want %q, a sentence holding %q", tc.named, got, why, tc.want,
					tc.why)
			}
		})
	}
	var noCode *config.Company
	if got, why := noCode.CheckCell(""); got != "" || why == "" {
		t.Errorf("a company hosting no code runs a check in %q (%q)", got, why)
	}
}

// THE SCHEMA ADMITS EXACTLY WHAT THE VALIDATOR DOES for code.checks: its
// timeout's bounds are the constants validation holds it to, and each cell
// field's enum is the one list of cells a check runs in.
func TestTheChecksBlockIsTheSchemas(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[config.CodeChecks]()
	timeout, _ := typ.FieldByName("MaxTimeoutSeconds")
	want := "min=" + strconv.Itoa(int(config.CodeMinCheckMaxTimeout/time.Second)) + ";max=" +
		strconv.Itoa(int(config.CodeCheckTimeoutCeiling/time.Second))
	if got := timeout.Tag.Get("js"); got != want {
		t.Errorf("max_timeout_seconds' schema is %q, validation's bounds %q", got, want)
	}
	cellNames := make([]string, len(config.CodeCheckCells))
	for i, cell := range config.CodeCheckCells {
		cellNames[i] = string(cell)
	}
	for _, name := range []string{"RunIn", "AllowRunIn"} {
		field, _ := typ.FieldByName(name)
		if got, want := field.Tag.Get("js"), "enum="+strings.Join(cellNames, "|"); got != want {
			t.Errorf("%s's schema admits %q, validation %q", name, got, want)
		}
	}
	if config.CodeDefaultCheckMaxTimeout < config.CodeMinCheckMaxTimeout ||
		config.CodeDefaultCheckMaxTimeout > config.CodeCheckTimeoutCeiling {
		t.Errorf("the default %s is outside the bounds it is held to", config.CodeDefaultCheckMaxTimeout)
	}
	if got := (config.CodeChecks{}).MaxTimeout(); got != config.CodeDefaultCheckMaxTimeout {
		t.Errorf("an unset timeout is %s, want the default %s", got, config.CodeDefaultCheckMaxTimeout)
	}
	if got := (config.CodeChecks{MaxTimeoutSeconds: 600}).MaxTimeout(); got != 10*time.Minute {
		t.Errorf("max_timeout_seconds: 600 is %s", got)
	}
}

// A CHECK'S SECRET IS NAMED BY ONE RULE, the company's code.checks.env keys
// and a checks file's env names alike: a letter or an underscore, then
// letters, digits and underscores, at most CodeMaxCheckEnvNameBytes.
func TestACheckSecretIsNamedByOneRule(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"NPM_TOKEN": true, "_": true, "a1": true, "DATABASE_URL_2": true,
		strings.Repeat("A", config.CodeMaxCheckEnvNameBytes): true,
		"": false, "1PASSWORD": false, "NPM-TOKEN": false, "NPM TOKEN": false, "A=B": false, "ÄPI": false,
		strings.Repeat("A", config.CodeMaxCheckEnvNameBytes+1): false,
	} {
		if got := config.ValidCodeCheckEnvName(name); got != want {
			t.Errorf("ValidCodeCheckEnvName(%q) = %v, want %v", name, got, want)
		}
	}
}
