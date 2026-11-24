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
// without), or the double, and the seats' default cell.
type cells struct {
	local, image, fake bool
	defaultRunIn       string
}

// checksDoc is a company hosting code over the catalogue cat, its code.checks
// the given YAML lines, each written under `checks:`.
func checksDoc(cat cells, checks ...string) string {
	doc := "name: Acme\n" + catalogueDoc(cat)
	doc += "code:\n  backend: gitlet\n"
	if len(checks) > 0 {
		doc += "  checks:\n"
		for _, line := range checks {
			doc += "    " + line + "\n"
		}
	}
	return doc
}

// catalogueDoc is cat as a `providers:` block, or nothing for no catalogue.
func catalogueDoc(cat cells) string {
	if !cat.local && !cat.fake {
		return ""
	}
	doc := "providers:\n  sandbox:\n"
	switch {
	case cat.fake:
		doc += "    fake: true\n"
	case cat.image:
		doc += "    local: {image: \"ghcr.io/example/box:1\"}\n"
	default:
		doc += "    local: {}\n"
	}
	if cat.defaultRunIn != "" {
		doc += "    default_run_in: " + cat.defaultRunIn + "\n"
	}
	return doc
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

// THE CHECKS BLOCK IS HELD TO ITS RULES, each refused at the line an operator wrote, one mistake
// one line: timeout within a minute and six hours, a secret's name one a check can name. It names
// no cell: a check runs in a container made from the image, never `direct`, so nothing can place
// one on the engine host. Nothing of it on a company hosting no code. And the image a company
// hosting code sets is what its checks run in, so it is never refused as a field no box reads.
//
// Mutations: drop the checks from SandboxPlacements — an image on a direct-seat company hosting
// code is refused as unread, red; drop the hosting clause from the unread-field remedy — the
// runtime case does not name the image, red.
func TestTheChecksBlockIsHeldToItsRules(t *testing.T) {
	t.Parallel()
	direct := cells{local: true, defaultRunIn: "direct"}
	for name, tc := range map[string]struct {
		doc string
		// path and kind are the one problem the document must be refused
		// with; an empty path accepts it.
		path, kind string
		says       []string
	}{
		"nothing set": {doc: checksDoc(direct)},
		"every field at a bound": {doc: checksDoc(cells{local: true, image: true, defaultRunIn: "container"},
			"env: {NPM_TOKEN: \"${NPM_TOKEN}\", _A1: x}", "max_timeout_seconds: 21600")},
		"the timeout at its floor":          {doc: checksDoc(direct, "max_timeout_seconds: 60")},
		"a catalogue only a check reaches":  {doc: checksDoc(cells{local: true, image: true})},
		"an image on a direct-seat company": {doc: checksDoc(cells{local: true, image: true, defaultRunIn: "direct"})},
		"a timeout under a minute": {doc: checksDoc(direct, "max_timeout_seconds: 59"),
			path: "code.checks.max_timeout_seconds", kind: "out_of_range", says: []string{"60..21600", "3600"}},
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
		"a cell named for a check": {doc: checksDoc(direct, "run_in: direct"),
			path: "code.checks.run_in", kind: "unknown_field"},
		"checks with nothing hosting code": {doc: "name: Acme\ncode:\n  checks:\n    max_timeout_seconds: 600\n",
			path: "code.checks", kind: "conflict", says: []string{"code.backend: gitlet"}},
		"a container field with no image": {
			doc: "name: Acme\nproviders:\n  sandbox:\n    local: {runtime: docker}\n    default_run_in: direct\n" +
				"code:\n  backend: gitlet\n",
			path: "providers.sandbox.local.runtime", kind: "conflict",
			says: []string{"set providers.sandbox.local.image to run this company's checks"}},
		"a seat's container with no image": {doc: checksDoc(cells{local: true, defaultRunIn: "container"}),
			path: "providers.sandbox.local.image", kind: "missing",
			says: []string{"the harness installed", "git 2.40", "checks run in containers made from it"}},
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
		})
	}
}

// A COMPANY'S CHECKS RUN EXACTLY WHERE ITS IMAGE IS: on a company hosting code
// whose catalogue sets providers.sandbox.local.image (or is the double), the
// container cell is reached at the path that makes it, so the engine builds a
// check's backend as it builds a seat's, and the check runner runs them; with
// no image, no catalogue or no code hosted, nothing is reached for a check and
// the runner ends each one with the sentence naming what to change.
//
// Mutations: reach the container on every company hosting code — the no-image
// rows reach it, red; answer "" with no image — the no-image rows run, red.
func TestAChecksContainerIsReachedExactlyWhereTheImageIs(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		doc     string
		reached map[config.Placement]string
		why     string // "" is a company whose checks run
	}{
		"an image beside a direct default": {
			doc: checksDoc(cells{local: true, image: true, defaultRunIn: "direct"}),
			reached: map[config.Placement]string{config.PlacementDirect: "providers.sandbox.default_run_in",
				config.PlacementContainer: "providers.sandbox.local.image"},
		},
		"an image a seat's default reaches first": {
			doc:     checksDoc(cells{local: true, image: true, defaultRunIn: "container"}),
			reached: map[config.Placement]string{config.PlacementContainer: "providers.sandbox.default_run_in"},
		},
		"the double": {
			doc: checksDoc(cells{fake: true}),
			reached: map[config.Placement]string{config.PlacementDirect: "providers.sandbox",
				config.PlacementContainer: "providers.sandbox.fake"},
		},
		"no image": {
			doc:     checksDoc(cells{local: true, defaultRunIn: "direct"}),
			reached: map[config.Placement]string{config.PlacementDirect: "providers.sandbox.default_run_in"},
			why:     "providers.sandbox.local.image",
		},
		"no catalogue": {
			doc:     checksDoc(cells{}),
			reached: map[config.Placement]string{},
			why:     "providers.sandbox.local.image",
		},
		"a company hosting no code": {
			doc:     "name: Acme\n" + catalogueDoc(cells{fake: true}),
			reached: map[config.Placement]string{config.PlacementDirect: "providers.sandbox"},
			why:     "code.backend is not gitlet",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := mustParse(t, tc.doc)
			if got := c.SandboxPlacements(); !reflect.DeepEqual(got, tc.reached) {
				t.Errorf("SandboxPlacements() = %v, want %v", got, tc.reached)
			}
			why := c.ChecksRefusal()
			if (tc.why == "") != (why == "") || !strings.Contains(why, tc.why) {
				t.Errorf("ChecksRefusal() = %q, want a sentence holding %q", why, tc.why)
			}
		})
	}
	var noCompany *config.Company
	if why := noCompany.ChecksRefusal(); why == "" {
		t.Error("no company at all runs checks")
	}
}

// THE SCHEMA ADMITS EXACTLY WHAT THE VALIDATOR DOES for code.checks: its
// timeout's bounds are the constants validation holds it to, and the default
// its description states is the one an unset timeout reads as.
func TestTheChecksBlockIsTheSchemas(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[config.CodeChecks]()
	timeout, _ := typ.FieldByName("MaxTimeoutSeconds")
	want := "min=" + strconv.Itoa(int(config.CodeMinCheckMaxTimeout/time.Second)) + ";max=" +
		strconv.Itoa(int(config.CodeCheckTimeoutCeiling/time.Second))
	if got := timeout.Tag.Get("js"); got != want {
		t.Errorf("max_timeout_seconds' schema is %q, validation's bounds %q", got, want)
	}
	if says := "unset is " + strconv.Itoa(int(config.CodeDefaultCheckMaxTimeout/time.Second)); !strings.Contains(timeout.Tag.Get("desc"), says) {
		t.Errorf("max_timeout_seconds' description does not say %q: %q", says, timeout.Tag.Get("desc"))
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

// AN UNSET CHECK TIMEOUT IS THE CI'S DEFAULT JOB TIMEOUT: an hour, GitLab CI's
// and Azure Pipelines' alike, so a pipeline moved here keeps the default it ran
// under. The schema test above holds the constant only to itself and its
// bounds, so this is what keeps the stated reason true.
// Mutation: set CodeDefaultCheckMaxTimeout to 50 minutes, and this goes red.
func TestAnUnsetCheckTimeoutIsTheCIsDefaultJobTimeout(t *testing.T) {
	t.Parallel()
	const ciDefaultJobTimeout = time.Hour
	if got := (config.CodeChecks{}).MaxTimeout(); got != ciDefaultJobTimeout {
		t.Errorf("an unset code.checks.max_timeout_seconds is %s, want %s: GitLab CI's and "+
			"Azure Pipelines' default job timeout", got, ciDefaultJobTimeout)
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
