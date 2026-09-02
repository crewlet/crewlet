package config

import (
	"errors"
	"testing"
)

// AGENT MODE IS NAMED, NEVER INFERRED.
//
// Both modes are defensible for the same CLI on the same seat — text mode is
// predictable and works with no reachable API, agent mode gets the vendor's
// own harness — so a default would be a decision made for the operator on a
// question they have to answer.
func TestTheCLIModeIsAClosedSet(t *testing.T) {
	t.Parallel()
	err := rejects(t, "name: Acme\nproviders:\n  llm:\n    sub:\n"+
		"      type: cli-agent\n      cli: {agent: claude-code, mode: agentic}\n",
		"providers.llm.sub.cli.mode")
	if !errors.Is(err, ErrUnknownValue) {
		t.Fatalf("want ErrUnknownValue, got %v", err)
	}
	// Both spellings load, and an entry that names none is text — which is
	// what every cli-agent entry meant before agent mode existed.
	for _, mode := range []string{"", "text", "agent"} {
		block := "cli: {agent: claude-code}"
		if mode != "" {
			block = "cli: {agent: claude-code, mode: " + mode + "}"
		}
		cfg := mustCompany(t, "name: Acme\nproviders:\n  sandbox: {fake: true}\n  llm:\n    sub:\n"+
			"      type: cli-agent\n      model: sonnet\n      "+block+"\n")
		want := mode == "agent"
		if got := cfg.Providers.LLM["sub"].CLI.AgentMode(); got != want {
			t.Errorf("mode %q reports AgentMode() = %v", mode, got)
		}
	}
}

// A CELL FOR A RUNTIME THAT RUNS NOWHERE configures nothing: a text-mode CLI
// is a subprocess of this engine, and there is no box to place.
func TestARunInOnATextModeCLIIsRefused(t *testing.T) {
	t.Parallel()
	err := rejects(t, "name: Acme\nproviders:\n  llm:\n    sub:\n"+
		"      type: cli-agent\n      cli: {agent: claude-code, run_in: e2b}\n",
		"providers.llm.sub.cli.run_in")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}

// `self` MEANS THE EXECUTOR'S OWN RUN, so it needs an executor that has one.
//
// On any other runtime the seat has no shell of its own, and `self` would read
// as a working choice while turning code work off — a seat planning around a
// sandbox it is never offered, with nothing anywhere saying why.
func TestSelfNeedsAnAgentModeExecutor(t *testing.T) {
	t.Parallel()
	api := "name: Acme\nproviders:\n  sandbox: {fake: true}\n  llm:\n    main:\n" +
		"      type: anthropic\n      model: claude-golden\n" +
		"roles:\n  - name: SWE\n    llm: main\n    sandbox: {enabled: true, run_in: self}\n"
	err := rejects(t, api, "roles[0].sandbox.run_in")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}

	// The same seat on an agent-mode entry loads.
	mustCompany(t, "name: Acme\nproviders:\n  sandbox: {fake: true}\n  llm:\n    sub:\n"+
		"      type: cli-agent\n      model: sonnet\n      cli: {agent: claude-code, mode: agent}\n"+
		"roles:\n  - name: SWE\n    llm: sub\n    sandbox: {enabled: true, run_in: self}\n")
}

// A COMPANY DEFAULT CANNOT BE `self`.
//
// It is meaningful only for a seat whose executor is a coding CLI in agent
// mode, and as a company-wide answer it would silently turn code work off for
// every seat that is not.
func TestSelfIsNotACompanyDefault(t *testing.T) {
	t.Parallel()
	err := rejects(t, "name: Acme\nproviders:\n  sandbox:\n    local: {}\n    default_run_in: self\n",
		"providers.sandbox.default_run_in")
	if !errors.Is(err, ErrUnknownValue) {
		t.Fatalf("want ErrUnknownValue, got %v", err)
	}
}

// `self` NEEDS NO BACKEND, which is the whole reason it is exempt from every
// walk that decides what to build and what to require.
func TestSelfIsTheOneCellWithNoBackend(t *testing.T) {
	t.Parallel()
	catalogue := &SandboxProvider{Fake: true}
	for _, p := range Placements {
		if p.NeedsBackend() != (p != PlacementSelf) {
			t.Errorf("%q disagrees with itself about needing a backend", p)
		}
		// The double answers every real cell and still not this one: it
		// stands in for a backend, and `self` is the absence of one.
		if got := catalogue.Configured(p); got != (p != PlacementSelf) {
			t.Errorf("the double reports Configured(%q) = %v", p, got)
		}
	}
	if len(BackendPlacements()) != len(Placements)-1 {
		t.Errorf("BackendPlacements() = %v, want every cell but %q",
			BackendPlacements(), PlacementSelf)
	}
}

// THE EXECUTOR'S ENTRY IS RESOLVED THE WAY THE PHASE REGISTRY RESOLVES IT, or
// the rule above refuses the wrong seats.
//
// Two resolutions of one question is normally the mistake this package spends
// its rules on; this one exists because the registry's needs BUILT providers,
// which do not exist during validation. So the fallbacks are asserted here,
// against the same order: the seat's own keys, then "default", then the first
// provider declared.
func TestTheExecutorEntryResolvesLikeThePhaseRegistry(t *testing.T) {
	t.Parallel()
	doc := "name: Acme\nproviders:\n  llm:\n    first:\n      type: anthropic\n      model: a\n" +
		"    default:\n      type: anthropic\n      model: b\n" +
		"    named:\n      type: anthropic\n      model: c\n"
	cfg := mustCompany(t, doc+"roles:\n  - name: Picky\n    llm: named\n  - name: Silent\n")
	for _, tc := range []struct{ seat, want string }{
		{"Picky", "named"},
		// A seat that names nothing takes the entry called "default",
		// never the first declared — which is what the registry does.
		{"Silent", "default"},
	} {
		var role *Role
		for i := range cfg.Roles {
			if cfg.Roles[i].Name == tc.seat {
				role = &cfg.Roles[i]
			}
		}
		key, _, ok := cfg.ExecutorProvider(role)
		if !ok || key != tc.want {
			t.Errorf("%s resolves to %q (ok=%v), want %q", tc.seat, key, ok, tc.want)
		}
	}

	// With no entry called "default", the first DECLARED one wins — which
	// is why declaration order is preserved rather than left to the map.
	noDefault := mustCompany(t, "name: Acme\nproviders:\n  llm:\n    zeta:\n      type: anthropic\n      model: a\n"+
		"    alpha:\n      type: anthropic\n      model: b\nroles:\n  - name: Silent\n")
	if key, _, ok := noDefault.ExecutorProvider(&noDefault.Roles[0]); !ok || key != "zeta" {
		t.Errorf("with no default the executor resolved to %q, want the first declared", key)
	}

	// A company with no providers at all answers false rather than
	// crashing: the registry refuses that company at construction, and
	// validation has to survive long enough to say so.
	empty := &Company{}
	if _, _, ok := empty.ExecutorProvider(&Role{}); ok {
		t.Error("a company with no providers resolved an executor entry")
	}
}
