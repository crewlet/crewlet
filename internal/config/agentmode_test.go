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

// AN AGENT-MODE ENTRY IS A RUN, placed by its own cli.run_in, and it gets the
// same three questions a seat's gate gets: is there a catalogue, is the cell
// it names configured, and does a silent entry have a default to fall to.
// Each was unasked, and each failed at the seat's first turn — every turn —
// with a launch error naming a backend no field in the file appeared to lack.
func TestAnAgentModeEntryIsPlacedLikeASeat(t *testing.T) {
	t.Parallel()
	entry := func(runIn string) string {
		cli := "cli: {agent: claude-code, mode: agent}"
		if runIn != "" {
			cli = "cli: {agent: claude-code, mode: agent, run_in: " + runIn + "}"
		}
		return "  llm:\n    sub:\n      type: cli-agent\n      model: sonnet\n      " + cli + "\n" +
			"roles:\n  - name: SWE\n    llm: sub\n"
	}

	t.Run("no catalogue at all", func(t *testing.T) {
		t.Parallel()
		err := rejects(t, "name: Acme\nproviders:\n"+entry(""), "providers.llm.sub.cli.mode")
		if !errors.Is(err, ErrMissing) {
			t.Fatalf("want ErrMissing, got %v", err)
		}
	})
	t.Run("a cell the catalogue does not hold", func(t *testing.T) {
		t.Parallel()
		err := rejects(t, "name: Acme\nproviders:\n  sandbox:\n    local: {}\n    default_run_in: direct\n"+
			entry("e2b"), "providers.llm.sub.cli.run_in")
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("want ErrConflict, got %v", err)
		}
	})
	t.Run("a silent entry on an ambiguous catalogue", func(t *testing.T) {
		t.Parallel()
		err := rejects(t, "name: Acme\nproviders:\n  sandbox:\n    local: {}\n"+entry(""),
			"providers.llm.sub.cli.run_in")
		if !errors.Is(err, ErrMissing) {
			t.Fatalf("want ErrMissing, got %v", err)
		}
	})
	t.Run("the cell it names is reached and built for", func(t *testing.T) {
		t.Parallel()
		cfg := mustCompany(t, "name: Acme\nproviders:\n  sandbox:\n    local: {}\n    e2b: {api_key: k}\n"+
			"    default_run_in: direct\n"+entry("e2b"))
		if where := cfg.SandboxPlacements()[PlacementE2B]; where != "providers.llm.sub.cli.run_in" {
			t.Errorf("the entry's cell is reached by %q, want the entry itself", where)
		}
		// And what reaching a cell implies follows: a container run
		// needs an image whether a seat or an entry asked for it.
		err := rejects(t, "name: Acme\nproviders:\n  sandbox:\n    local: {}\n    default_run_in: direct\n"+
			entry("container"), "providers.sandbox.local.image")
		if !errors.Is(err, ErrMissing) {
			t.Fatalf("want ErrMissing, got %v", err)
		}
	})
	t.Run("an entry no seat runs on reaches nothing", func(t *testing.T) {
		t.Parallel()
		// The same entry, with the seat on another model: it launches
		// nothing, so its cell is not built and its questions are not
		// asked — the day a seat points at it, they are.
		cfg := mustCompany(t, "name: Acme\nproviders:\n  sandbox:\n    local: {}\n    default_run_in: direct\n"+
			"  llm:\n    main:\n      type: anthropic\n      model: claude-golden\n"+
			"    sub:\n      type: cli-agent\n      model: sonnet\n"+
			"      cli: {agent: claude-code, mode: agent, run_in: e2b}\n"+
			"roles:\n  - name: SWE\n    llm: main\n")
		if _, reached := cfg.SandboxPlacements()[PlacementE2B]; reached {
			t.Error("an unused entry's cell was reached")
		}
	})
}

// `self` IS A SEAT'S ANSWER, NOT AN ENTRY'S. It means "my code work rides my
// executor's own run", and an agent-mode entry IS that run — it has no other
// run to ride and no backend resolves it. Accepted, it validated cleanly and
// failed at the seat's first turn, every turn.
func TestSelfIsNotACellAnAgentModeEntryCanName(t *testing.T) {
	t.Parallel()
	err := rejects(t, "name: Acme\nproviders:\n  sandbox: {fake: true}\n  llm:\n    sub:\n"+
		"      type: cli-agent\n      model: sonnet\n      cli: {agent: claude-code, mode: agent, run_in: self}\n",
		"providers.llm.sub.cli.run_in")
	if !errors.Is(err, ErrUnknownValue) {
		t.Fatalf("want ErrUnknownValue, got %v", err)
	}
}

// THE BRIDGE'S NAME IS RESERVED. The engine writes the seat's tool bridge into
// every agent-mode box's server list under it, so a company server of the
// same name would be replaced there — its tools gone from the run with no
// error and no log line saying why.
func TestTheBridgeServerNameIsReserved(t *testing.T) {
	t.Parallel()
	err := rejects(t, "name: Acme\nmcp_servers:\n  - name: "+BridgeServerName+"\n    command: npx\n",
		"mcp_servers[0].name")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}

// A HUMAN SEAT RUNS NO EXECUTOR, so it reaches no providers.llm entry.
//
// `llm` is one of the fields a human seat may not carry, so its chain is
// always empty and always falls through to the company-wide fallback — the
// entry called "default", else the first declared. Resolved rather than
// refused, that made an agent-mode entry declared first "reached" by a seat
// that is never spawned: a company was refused for a box its founder's seat
// would never run in, and one with a catalogue built a backend for a run that
// cannot happen.
func TestAHumanSeatReachesNoExecutorEntry(t *testing.T) {
	t.Parallel()
	// The agent-mode entry is declared FIRST, so it is the fallback.
	const providers = "name: Acme\nproviders:\n  llm:\n" +
		"    coder:\n      type: cli-agent\n      model: sonnet\n" +
		"      cli: {agent: claude-code, mode: agent, run_in: e2b}\n" +
		"    main:\n      type: anthropic\n      model: claude-golden\n"
	const founder = "  - name: Founder\n    kind: human\n    contact: {slack_user_id: U0FOUNDER}\n"

	// No catalogue, and none needed: no seat's executor is the agent-mode
	// entry, so nothing runs in a box.
	cfg := mustCompany(t, providers+"roles:\n"+founder+"  - name: SWE\n    llm: main\n")
	if _, _, resolved := cfg.ExecutorProvider(&cfg.Roles[0]); resolved {
		t.Error("a human seat resolved an executor; it is never spawned and runs no phase")
	}
	if _, _, resolved := cfg.ExecutorProvider(&cfg.Roles[1]); !resolved {
		t.Error("an agent seat resolved no executor")
	}
	if len(cfg.SandboxPlacements()) != 0 {
		t.Errorf("a cell is reached by a seat nobody spawns: %v", cfg.SandboxPlacements())
	}

	// And an AGENT seat falling through the same fallback still reaches it,
	// or this would just be "the fallback never counts".
	rejects(t, providers+"roles:\n"+founder+"  - name: SWE\n", agentModeEntryPath("coder"))
}

// THE FALLBACK IS THE COMPANY'S, NOT THE SEAT'S — the entry called "default",
// else the first declared — so it is resolved once for a walk over every seat
// rather than rebuilt per seat. Asserted because the two paths to it must
// keep answering the same thing: ExecutorProvider is what validation reads,
// and the walker takes the short one.
func TestBothPathsToTheExecutorEntryAgree(t *testing.T) {
	t.Parallel()
	for _, doc := range []string{
		"    first:\n      type: anthropic\n      model: a\n" +
			"    default:\n      type: anthropic\n      model: b\n",
		"    first:\n      type: anthropic\n      model: a\n" +
			"    second:\n      type: anthropic\n      model: b\n",
	} {
		cfg := mustCompany(t, "name: Acme\nproviders:\n  llm:\n"+doc+
			"roles:\n  - name: Picky\n    llm: first\n  - name: Silent\n")
		fallback, ok := cfg.executorFallback()
		if !ok {
			t.Fatal("no fallback for a company with providers")
		}
		for i := range cfg.Roles {
			wantKey, wantEntry, wantOK := cfg.ExecutorProvider(&cfg.Roles[i])
			gotKey, gotEntry, gotOK := cfg.executorProvider(&cfg.Roles[i], fallback)
			if gotKey != wantKey || gotOK != wantOK || gotEntry.Model != wantEntry.Model {
				t.Errorf("%s: short path = %q/%v, ExecutorProvider = %q/%v",
					cfg.Roles[i].Name, gotKey, gotOK, wantKey, wantOK)
			}
		}
	}
}
