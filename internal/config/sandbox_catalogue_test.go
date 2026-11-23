package config

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// THE SEATS INSIDE `units:` REACH THEIR ENTRIES, for every cross-field rule:
// an entry only a unit's seat runs on, placed in a container, needs the image
// as one a top-level seat runs on does. A walk of `roles:` alone would exempt
// most of a real company's seats from every cross-field rule there is.
//
// Mutation: walk `roles:` alone in executorSeats and the unit's container is
// never reached, so nothing is refused — red.
func TestAnEntryOnlyAUnitsSeatRunsOnIsHeldToTheCatalogue(t *testing.T) {
	t.Parallel()
	err := rejects(t, "name: Acme\nproviders:\n  sandbox:\n    local: {}\n    default_run_in: direct\n"+
		"  llm:\n    main: {model: sonnet}\n    boxed: {model: sonnet, run_in: container}\n"+
		"roles:\n  - name: CEO\n    llm: main\nunits:\n  - name: Platform\n    roles:\n"+
		"      - name: SWE\n        llm: boxed\n", "providers.sandbox.local.image")
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("want ErrMissing, got %v", err)
	}
	if !strings.Contains(err.Error(), "providers.llm.boxed.run_in runs in a container") {
		t.Errorf("the refusal does not name the unit's seat's entry as what runs in a container:\n%v", err)
	}
}

// `local:` HAS TO BE TOLD WHERE A SILENT ENTRY'S RUN HAPPENS — and only then.
//
// `local:` is TWO cells, not one: it serves `direct`, which runs the harness
// as the engine's user, and `container`, which does not. Picking either for an
// operator who wrote neither is the default that gets discovered afterwards.
// But the question is about the ENTRIES the seats' executors launch on: a
// catalogue whose every such entry names its own cell has nothing left to
// default. Refused on the block alone, the remedy its own message offered —
// "give every entry its run_in" — could never pass.
func TestAnAmbiguousCatalogueNeedsADefaultOnlyWhereAnEntryWouldReadIt(t *testing.T) {
	t.Parallel()
	const local = "name: Acme\nproviders:\n  sandbox:\n    local: {}\n"

	// A silent entry is refused where it is written, and only there: the
	// default is one of that refusal's remedies, so a second refusal at the
	// catalogue would say the one mistake twice.
	silent := "  llm:\n    main: {model: sonnet}\nroles:\n  - name: SWE\n"
	err := rejects(t, local+silent, "providers.llm.main.run_in")
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("want ErrMissing, got %v", err)
	}
	if places := `either of two cells ("direct" and "container")`; !strings.Contains(err.Error(), places) {
		t.Errorf("the refusal does not name the cells as %s:\n%v", places, err)
	}
	if problems := Problems(err); len(problems) != 1 {
		t.Errorf("want the one refusal at the entry, got %d:\n%v", len(problems), err)
	}

	// Every entry naming its cell needs no default, and each cell is reached.
	cfg := mustCompany(t, "name: Acme\nproviders:\n  sandbox:\n    local: {image: img}\n"+
		"  llm:\n    here: {model: sonnet, run_in: direct}\n"+
		"    there: {model: sonnet, run_in: container}\n"+
		"roles:\n  - name: SWE\n    llm: here\n  - name: Ops\n    llm: there\n")
	reached := cfg.SandboxPlacements()
	if _, ok := reached[PlacementDirect]; !ok {
		t.Errorf("direct is not reached: %v", reached)
	}
	if _, ok := reached[PlacementContainer]; !ok {
		t.Errorf("container is not reached: %v", reached)
	}
	if got := cfg.Providers.Sandbox.RunIn(); got != "" {
		t.Errorf("a catalogue with no default resolved one anyway: %q", got)
	}

	// THE DOUBLE RESOLVES ITSELF: it runs nowhere, so no cell is chosen on
	// the operator's behalf, and a demonstration config needs no default.
	cfg = mustCompany(t, "name: Acme\nproviders:\n  sandbox:\n    fake: true\n"+silent)
	if got := cfg.Providers.Sandbox.RunIn(); got != PlacementDirect {
		t.Fatalf("the double resolved to %q, want %q", got, PlacementDirect)
	}
}

// A CATALOGUE NOTHING REACHES IS REFUSED, and it is the one shape "require a
// default only where something reads it" must NOT let through.
//
// The engine builds a backend only for a cell something reaches, so `local:`
// with no default and no executor to place reaches nothing and builds
// nothing: a block that reads as configuration and configures nothing, which
// is why an empty block is refused too. Every catalogue that resolves a
// default reaches it precisely so an entry a seat is pointed at later has
// somewhere to go.
func TestACatalogueNothingReachesIsRefused(t *testing.T) {
	t.Parallel()
	err := rejects(t, "name: Acme\nproviders:\n  sandbox:\n    local: {}\n", "providers.sandbox.default_run_in")
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("want ErrMissing, got %v", err)
	}
	if places := `either of two cells ("direct" and "container") and nothing names one`; !strings.Contains(err.Error(), places) {
		t.Errorf("the refusal does not name the cells as %s:\n%v", places, err)
	}
	// The two ways to reach a cell both satisfy it, or the rule would be
	// "an ambiguous catalogue is refused" wearing a longer message.
	for _, tail := range []string{
		"    default_run_in: direct\n",
		"  llm:\n    sub:\n      model: sonnet\n      run_in: direct\n" +
			"roles:\n  - name: SWE\n    llm: sub\n",
	} {
		cfg := mustCompany(t, "name: Acme\nproviders:\n  sandbox:\n    local: {}\n"+tail)
		if len(cfg.SandboxPlacements()) == 0 {
			t.Errorf("nothing reached a cell, so no backend is built:\n%s", tail)
		}
	}
}

// AN EMPTY BLOCK CONFIGURES NOTHING, and it is the shape an unfinished edit
// leaves behind: it parses, and without this rule it would apply — every
// agent seat's executor launching into a box that is never there, and
// nothing anywhere saying why.
func TestAnEmptyCatalogueIsRefused(t *testing.T) {
	t.Parallel()
	err := rejects(t, "name: Acme\nproviders:\n  sandbox: {}\n", "providers.sandbox")
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("want ErrMissing, got %v", err)
	}
}

// THE DOUBLE REPLACES THE REAL BACKEND rather than joining it, so a real block
// underneath it reads as configuration and configures nothing.
func TestTheDoubleRefusesARealBackendBesideIt(t *testing.T) {
	t.Parallel()
	err := rejects(t, "name: Acme\nproviders:\n  sandbox:\n    fake: true\n    local: {}\n",
		"providers.sandbox.fake")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	mustCompany(t, "name: Acme\nproviders:\n  sandbox:\n    fake: true\n")
}

// A CONTAINER IMAGE IS REQUIRED WHERE A CONTAINER IS ACTUALLY RUN, and only
// there. Required unconditionally it would refuse a perfectly good direct-only
// company; unchecked, the seat's first launch fails at container create,
// minutes into a turn.
func TestTheContainerImageIsRequiredOnlyWhereAContainerRuns(t *testing.T) {
	t.Parallel()
	err := rejects(t, "name: Acme\nproviders:\n  sandbox:\n    local: {}\n"+
		"    default_run_in: container\n", "providers.sandbox.local.image")
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("want ErrMissing, got %v", err)
	}
	// A direct-only company needs none.
	mustCompany(t, "name: Acme\nproviders:\n  sandbox:\n    local: {}\n    default_run_in: direct\n")
	// And a container field nobody reads is reported rather than ignored:
	// it looks like configuration and configures nothing.
	err = rejects(t, "name: Acme\nproviders:\n  sandbox:\n"+
		"    local: {image: \"example.invalid/box:1\"}\n    default_run_in: direct\n",
		"providers.sandbox.local.image")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}

// THE RUNTIME'S OWN ENVIRONMENT IS DECLARED BY NAME, and a name no process can
// read back is refused rather than handed to the runtime: `A=B` as a key would
// set a variable called A. And it is a container field like the rest — a
// direct box drives no runtime, so there it configures nothing.
func TestRuntimeEnvTakesEnvironmentNamesOnlyAndOnlyWhereAContainerRuns(t *testing.T) {
	t.Parallel()
	const container = "name: Acme\nproviders:\n  sandbox:\n    local:\n" +
		"      image: \"example.invalid/box:1\"\n      runtime_env:\n%s" +
		"    default_run_in: container\n"
	mustCompany(t, fmt.Sprintf(container,
		"        SSH_AUTH_SOCK: \"${SSH_AUTH_SOCK}\"\n        AWS_PROFILE: registry-pull\n"))
	for _, bad := range []string{"A=B", "HAS SPACE", "1LEADING_DIGIT"} {
		err := rejects(t, fmt.Sprintf(container, fmt.Sprintf("        %q: x\n", bad)),
			"providers.sandbox.local.runtime_env."+bad)
		if !errors.Is(err, ErrShape) {
			t.Fatalf("runtime_env key %q: want ErrShape, got %v", bad, err)
		}
	}
	err := rejects(t, "name: Acme\nproviders:\n  sandbox:\n    local:\n"+
		"      runtime_env: {SSH_AUTH_SOCK: \"${SSH_AUTH_SOCK}\"}\n    default_run_in: direct\n",
		"providers.sandbox.local.runtime_env")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("runtime_env with no container seat: want ErrConflict, got %v", err)
	}
}

// A BARE `-e NAME` THE RUNTIME HAS NOTHING FOR IS SAID BEFORE IT IS APPLIED.
//
// The runtime's CLI copies NAME from its own environment — a fixed rule plus
// runtime_env, never the engine's — so a name neither carries starts a box in
// which it is simply unset, and nothing anywhere says why. Every spelling the
// runtimes accept is read; a name the fixed rule carries, one runtime_env
// declares, one given its own value and an env FILE are all fine, and each
// warning sits at the argument that names it.
func TestABareEnvArgNothingSuppliesIsAdvised(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, "name: Acme\n"+reachablePerson+"providers:\n  sandbox:\n    local:\n"+
		"      image: \"example.invalid/box:1\"\n"+
		"      runtime_env: {DECLARED: \"${DECLARED}\"}\n"+
		"      run_args: [\"-e\", \"FOO\", \"--env\", \"BAR\", \"--env=BAZ\", \"-eQUX\",\n"+
		"        \"-e\", \"DOCKER_HOST\", \"-e\", \"HTTPS_PROXY\", \"-e\", \"DECLARED\",\n"+
		"        \"-e\", \"SET=value\", \"--env-file\", \"/etc/crewlet/box.env\", \"--cpus\", \"2\"]\n"+
		"    default_run_in: container\n")
	got := map[string]string{}
	for _, w := range cfg.AdvisoryWarnings() {
		if w.Kind != WarningAdvisory {
			t.Errorf("warning %+v is not an advisory", w)
		}
		got[w.Path] = w.Message
	}
	want := map[string]string{
		"providers.sandbox.local.run_args[1]": "FOO",
		"providers.sandbox.local.run_args[3]": "BAR",
		"providers.sandbox.local.run_args[4]": "BAZ",
		"providers.sandbox.local.run_args[5]": "QUX",
	}
	for path, name := range want {
		message, ok := got[path]
		if !ok {
			t.Errorf("no advisory at %s for the bare `-e %s`: %v", path, name, got)
			continue
		}
		if !strings.Contains(message, "`-e "+name+"`") || !strings.Contains(message, "runtime_env") {
			t.Errorf("advisory at %s = %q, want it to name %s and runtime_env", path, message, name)
		}
	}
	if len(got) != len(want) {
		t.Errorf("advisories = %v, want exactly %v", got, want)
	}
	// And a company with nothing to say says nothing — as an empty list,
	// which a surface renders as "nothing to say" where a null reads as a
	// failure. NOTHING TO SAY includes somebody to invite: a company with no
	// human seat is advised that nobody can join it.
	if quiet := mustCompany(t, "name: Acme\n"+reachablePerson).AdvisoryWarnings(); quiet == nil || len(quiet) != 0 {
		t.Errorf("AdvisoryWarnings of a company with no sandbox = %#v, want an empty list", quiet)
	}
}

// reachablePerson is a `roles:` block declaring one human seat somebody can be
// reached at — what a fixture counting advisories declares, so the count is its
// subject's alone: a company with no human seat is advised that nobody can
// join it, and on a vendor chat surface a human seat with no account there
// that nobody can be mentioned at.
const reachablePerson = "roles:\n  - name: Founder\n    kind: human\n" +
	"    contact:\n      slack_user_id: U0FOUNDER\n"
