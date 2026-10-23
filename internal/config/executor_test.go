package config

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
)

// stubModel is an llm.Provider that is never called: the registry below is
// built only to ask which keys a chain resolves to.
type stubModel struct{}

func (stubModel) Model() string { return "stub" }
func (stubModel) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	return nil, errors.New("stubModel is never called")
}

// THE EXECUTOR CHAIN VALIDATION JUDGES IS THE ONE A LAUNCH WALKS.
//
// A launch takes the first member of the seat's chain that can run and is not
// sitting out, so a chain validation resolved by a rule of its own would
// refuse seats whose launch runs and place boxes for members no launch
// reaches. The chain here is held to the phase registry's own Chain, built as
// the engine builds it — from the company's organization and its providers in
// declaration order — over every shape a seat can resolve through: its own
// keys in order, a key the company lacks, the entry called "default", and the
// first declared.
//
// PARSED, NOT VALIDATED: a key that misses is refused on a write, and still
// reaches a node in a revision a newer peer admitted — the registry falls
// back for it, and so must this.
func TestTheExecutorChainIsTheOneALaunchWalks(t *testing.T) {
	t.Parallel()
	for _, providers := range []string{
		"    first:\n      model: a\n    default:\n      model: b\n    named:\n      model: c\n",
		// No "default": a silent seat takes the first declared.
		"    zeta:\n      model: a\n    alpha:\n      model: b\n",
	} {
		cfg, err := ParseCompanyDocument([]byte("name: Acme\nproviders:\n  llm:\n" + providers +
			"roles:\n" +
			"  - name: Founder\n    kind: human\n    contact: {slack_user_id: U0FOUNDER}\n" +
			"  - name: Silent\n" +
			"  - name: Chain\n    llm: [named, nowhere, first, named]\n" +
			"  - name: Unknown\n    llm: nowhere\n" +
			"  - name: Mapped\n    llm: {default: alpha, review: zeta}\n"))
		if err != nil {
			t.Fatal(err)
		}
		o, err := cfg.Organization()
		if err != nil {
			t.Fatal(err)
		}
		var entries []phase.Entry
		for _, key := range cfg.Providers.ProviderOrder() {
			entries = append(entries, phase.Entry{Key: key, Provider: stubModel{}})
		}
		registry, err := phase.NewRegistry(entries)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string][]string{}
		for seat := range cfg.executorSeats() {
			got[seat.role.Name] = seat.chain
		}
		if _, ok := got["Founder"]; ok {
			t.Error("a human seat resolved an executor chain; it is never spawned and runs no phase")
		}
		for _, seat := range o.Roles {
			if !seat.IsAgent() {
				continue
			}
			members, err := registry.Chain(seat, phase.Execute)
			if err != nil {
				t.Fatal(err)
			}
			var want []string
			for _, m := range members {
				want = append(want, m.Key)
			}
			if !slices.Equal(got[seat.Name], want) {
				t.Errorf("%s: validation judges the chain %v, a launch walks %v", seat.Name, got[seat.Name], want)
			}
		}
	}

	// A company with no providers runs no executor, so it has no chain to
	// judge: an org chart written before its credentials.
	empty := mustCompany(t, "name: Acme\nroles:\n  - name: SWE\n")
	for seat := range empty.executorSeats() {
		t.Errorf("%s resolved an executor chain on a company with no providers", seat.role.Name)
	}
}

// THE RUNNERS CONFIG ADMITS ARE THE RUNNERS THAT EXIST.
//
// A seat's executor is its coding CLI's own run, which only a CLI with an
// in-box runner has. A runner added there and not here refuses entries that
// would run; one listed here with no runner admits chains every launch then
// refuses.
func TestTheExecutorCLIsAreTheInBoxRunners(t *testing.T) {
	t.Parallel()
	var got []string
	for _, cli := range CodingAgents {
		got = append(got, string(cli))
	}
	if want := codingagent.Names(); !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
		t.Errorf("config.CodingAgents = %v, the in-box runners are %v", got, want)
	}
	for _, cli := range CodingAgents {
		if !cli.Valid() {
			t.Errorf("%q can run an executor but is not a CLI an entry may name", cli)
		}
	}
}

// executorCompany is a company with a catalogue every executor can run in,
// the given providers.llm entries and the given seats.
func executorCompany(llmEntries, roles string) string {
	return "name: Acme\nproviders:\n  sandbox: {fake: true}\n  llm:\n" + llmEntries + "roles:\n" + roles
}

const (
	ccEntry    = "    cc:\n      model: sonnet\n"
	ocEntry    = "    oc:\n      model: anthropic/claude-sonnet-5\n      agent: opencode\n"
	codexEntry = "    codex:\n      model: gpt-5\n      agent: codex\n"
)

// A SEAT WHOSE EXECUTOR CHAIN HAS NO MEMBER THAT CAN RUN IS REFUSED, where the
// remedy is written, and a chain that holds one is not.
//
// Every executor is its coding CLI's own run, which only claude-code and
// opencode have: a launch skips any other member, so a chain of nothing else
// never launches — the seat takes its work and fails every turn. RUNNABLE,
// because no turn of that seat runs. A seat's own `llm` is refused at that
// `llm`; a seat naming nothing falls to the company's fallback entry, which is
// one entry for every such seat, so it is refused once, at that entry,
// counting them. And another CLI stays valid wherever a text phase reads it.
func TestAnExecutorChainWithNoRunnerIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, llm, roles string
		path             string // empty: the document is valid
		says             []string
	}{
		{
			name: "a seat's own chain of another CLI",
			llm:  ccEntry + codexEntry, roles: "  - name: SWE\n    llm: codex\n",
			path: "roles[0].llm",
			says: []string{"providers.llm.codex (codex)", `only "claude-code" and "opencode" have`,
				"llm_review", "move one of them"},
		},
		{
			name: "a unit's seat, at its own path",
			llm:  ccEntry + codexEntry,
			roles: "  - name: Founder\n    kind: human\n    contact: {slack_user_id: U0FOUNDER}\n" +
				"units:\n  - name: Platform\n    roles:\n      - name: SWE\n        llm: codex\n",
			path: "units[0].roles[0].llm",
		},
		{
			name: "silent seats falling to a default of another CLI",
			llm:  "    default:\n      model: gpt-5\n      agent: codex\n" + ccEntry,
			roles: "  - name: CEO\n  - name: PM\n  - name: Ops\n" +
				"  - name: SWE\n    llm: cc\n",
			path: "providers.llm.default.agent",
			says: []string{`the one called "default"`, "codex", `only "claude-code" and "opencode" have`,
				`seat "ceo"`, "2 more seats", "point those seats' `llm`"},
		},
		{
			name: "a silent seat falling to the first declared entry",
			llm:  codexEntry + ccEntry, roles: "  - name: CEO\n",
			path: "providers.llm.codex.agent",
			says: []string{"the first one declared", `seat "ceo"`},
		},
		{
			name: "a chain that holds a runner beside another CLI",
			llm:  ccEntry + codexEntry, roles: "  - name: SWE\n    llm: [codex, cc]\n",
		},
		{
			name: "another CLI on a text phase only",
			llm:  ccEntry + codexEntry, roles: "  - name: SWE\n    llm: cc\n    llm_review: codex\n",
		},
		{
			name: "opencode runs an executor too",
			llm:  ocEntry + codexEntry, roles: "  - name: SWE\n    llm: oc\n",
		},
		{
			name: "a person reaches no executor at all",
			llm:  codexEntry, roles: "  - name: Founder\n    kind: human\n    contact: {slack_user_id: U0FOUNDER}\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doc := executorCompany(tc.llm, tc.roles)
			if tc.path == "" {
				mustCompany(t, doc)
				return
			}
			err := rejects(t, doc, tc.path)
			if !errors.Is(err, ErrConflict) {
				t.Errorf("want ErrConflict, got %v", err)
			}
			for _, said := range tc.says {
				if !strings.Contains(err.Error(), said) {
					t.Errorf("the refusal does not say %q:\n%v", said, err)
				}
			}
			cfg, err := ParseCompanyDocument([]byte(doc))
			if err != nil {
				t.Fatal(err)
			}
			if err := cfg.ValidateRunnable(); err == nil || !strings.Contains(err.Error(), tc.path) {
				t.Errorf("ValidateRunnable() = %v, want a refusal at %s: no turn of the seat can run", err, tc.path)
			}
		})
	}
}

// A SEAT WHOSE `llm` NAMES ONLY KEYS THE COMPANY LACKS IS REFUSED THERE, AND
// ONLY THERE. It falls to the company's fallback entry like a seat that named
// nothing, but its `llm` is a typo, refused where it is written — counted
// among the seats that fall to the fallback as well, it was also reported as a
// seat that "names no `llm`", contradicting the refusal beside it and sending
// the author to an entry the seat never asked for.
func TestASeatNamingOnlyUnknownKeysIsNotCountedAsNamingNone(t *testing.T) {
	t.Parallel()
	err := rejects(t, executorCompany(codexEntry, "  - name: SWE\n    llm: nowhere\n"),
		"roles[0].llm")
	if !errors.Is(err, ErrUnknownValue) {
		t.Errorf("want ErrUnknownValue, got %v", err)
	}
	if strings.Contains(err.Error(), "providers.llm.codex.agent") ||
		strings.Contains(err.Error(), "names no `llm`") {
		t.Errorf("the seat that named a missing key is also counted as naming none:\n%v", err)
	}
	// AND A SEAT THAT NAMED NOTHING STILL IS, beside it.
	err = rejects(t, executorCompany(codexEntry,
		"  - name: SWE\n    llm: nowhere\n  - name: CEO\n"), "providers.llm.codex.agent")
	if !strings.Contains(err.Error(), `seat "ceo" names no `+"`llm`") ||
		strings.Contains(err.Error(), "1 more seat") {
		t.Errorf("the fallback refusal does not count the silent seat alone:\n%v", err)
	}
}

// A CHAIN THAT HOLDS ANOTHER CLI BESIDE A RUNNER IS ADVISED ABOUT, AT THE
// SEAT'S `llm`, naming the member a launch skips and the one it runs on.
//
// Valid, because the member still serves every text phase that falls back to
// `llm`; worth saying, because a chain written as "Claude, then Codex once
// Claude's plan is spent" falls back to nothing at all for the executor. A
// chain of runners only, and one whose other CLI sits on a text phase's own
// field, are said nothing about.
func TestAMixedExecutorChainIsAdvisedAbout(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, executorCompany(ccEntry+ocEntry+codexEntry,
		"  - name: Mixed\n    llm: [cc, codex, oc]\n"+
			"  - name: Runners\n    llm: [cc, oc]\n"+
			"  - name: Reviewer\n    llm: cc\n    llm_review: codex\n"))
	var found []Warning
	for _, w := range cfg.AdvisoryWarnings() {
		if strings.HasSuffix(w.Path, ".llm") {
			found = append(found, w)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want one advisory at a seat's llm, got %+v", found)
	}
	w := found[0]
	if w.Path != "roles[0].llm" || w.Seat != "mixed" || w.Kind != WarningAdvisory {
		t.Errorf("advisory = %+v, want one at roles[0].llm for seat mixed", w)
	}
	for _, said := range []string{"providers.llm.codex (codex)", `which only "claude-code" and "opencode" have`,
		"providers.llm.cc (claude-code), providers.llm.oc (opencode)"} {
		if !strings.Contains(w.Message, said) {
			t.Errorf("the advisory does not say %q: %s", said, w.Message)
		}
	}
}

// AN AGENT SEAT WITH A MODEL TO RUN ON NEEDS THE CATALOGUE ITS EXECUTOR RUNS
// IN, and nothing else does.
//
// Every executor is its coding CLI's own run, in a box, so without the
// catalogue every such seat takes its work and fails every turn at launch.
// One refusal, at the missing block, naming the first seat and counting the
// rest. A company with no providers.llm is an org chart written before its
// credentials and runs without one; so does a company with no agent seat.
// And a block that is present and empty is refused once, by its own rule.
func TestAnAgentSeatNeedsTheCatalogue(t *testing.T) {
	t.Parallel()
	const person = "  - name: Founder\n    kind: human\n    contact: {slack_user_id: U0FOUNDER}\n"
	doc := "name: Acme\nproviders:\n  llm:\n" + ccEntry +
		"roles:\n" + person + "  - name: CEO\n  - name: PM\n"
	err := rejects(t, doc, "providers.sandbox")
	if !errors.Is(err, ErrMissing) {
		t.Errorf("want ErrMissing, got %v", err)
	}
	var at []Problem
	for _, p := range Problems(err) {
		if p.Path == "providers.sandbox" {
			at = append(at, p)
		}
	}
	if len(at) != 1 || !strings.Contains(at[0].Message, `seat "ceo"`) ||
		!strings.Contains(at[0].Message, "1 more seat") {
		t.Errorf("want one problem naming the first seat and counting the rest, got %+v", at)
	}
	cfg, perr := ParseCompanyDocument([]byte(doc))
	if perr != nil {
		t.Fatal(perr)
	}
	if err := cfg.ValidateRunnable(); err == nil {
		t.Error("an apply admitted a company none of whose executors can run")
	}

	// No model yet: the seats hold their work, and need nothing to run in.
	mustCompany(t, "name: Acme\nroles:\n"+person+"  - name: CEO\n")
	// No agent seat: nothing runs an executor.
	mustCompany(t, "name: Acme\nproviders:\n  llm:\n"+ccEntry+"roles:\n"+person)

	empty := "name: Acme\nproviders:\n  sandbox: {}\n  llm:\n" + ccEntry + "roles:\n  - name: CEO\n"
	err = rejects(t, empty, "providers.sandbox")
	n := 0
	for _, p := range Problems(err) {
		if p.Path == "providers.sandbox" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("an empty block is refused %d times at providers.sandbox, want once:\n%v", n, err)
	}
}

// EVERY ENTRY A LAUNCH CAN LAND ON IS PLACED — the head of a seat's chain and
// each fallback member alike — and no other.
//
// A launch falls back to the next member when the first is sitting out, and
// that member runs in the cell its own `run_in` names. Asked of the head
// alone, a fallback member's cell would have no backend built and no image
// required, so the launch the fallback exists for would fail with a backend
// nobody had been asked to configure. A member no launch can pick — another CLI, or an entry
// no seat's executor holds — launches nothing, and its cell is reached by
// nothing.
func TestEveryEntryALaunchCanLandOnIsPlaced(t *testing.T) {
	t.Parallel()
	entry := func(key, runIn string) string {
		e := "    " + key + ":\n      model: sonnet\n"
		if runIn != "" {
			e += "      run_in: " + runIn + "\n"
		}
		return e
	}
	company := func(catalogue, llmEntries, roles string) string {
		return "name: Acme\nproviders:\n  sandbox:\n" + catalogue + "  llm:\n" + llmEntries + "roles:\n" + roles
	}

	t.Run("a cell the catalogue does not hold", func(t *testing.T) {
		t.Parallel()
		err := rejects(t, company("    local: {}\n    default_run_in: direct\n",
			entry("head", "")+entry("spare", "e2b"), "  - name: SWE\n    llm: [head, spare]\n"),
			"providers.llm.spare.run_in")
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("want ErrConflict, got %v", err)
		}
	})
	t.Run("a silent member on an ambiguous catalogue", func(t *testing.T) {
		t.Parallel()
		err := rejects(t, company("    local: {}\n", entry("head", "direct")+entry("spare", ""),
			"  - name: SWE\n    llm: [head, spare]\n"), "providers.llm.spare.run_in")
		if !errors.Is(err, ErrMissing) {
			t.Fatalf("want ErrMissing, got %v", err)
		}
	})
	t.Run("a fallback member's cell is reached and built for", func(t *testing.T) {
		t.Parallel()
		cfg := mustCompany(t, company("    local: {}\n    e2b: {api_key: k}\n    default_run_in: direct\n",
			entry("head", "")+entry("spare", "e2b"), "  - name: SWE\n    llm: [head, spare]\n"))
		if where := cfg.SandboxPlacements()[PlacementE2B]; where != "providers.llm.spare.run_in" {
			t.Errorf("the fallback member's cell is reached by %q, want the member itself", where)
		}
		// And what reaching a cell implies follows: a fallback member in a
		// container needs an image as surely as the head would.
		err := rejects(t, company("    local: {}\n    default_run_in: direct\n",
			entry("head", "")+entry("spare", "container"), "  - name: SWE\n    llm: [head, spare]\n"),
			"providers.sandbox.local.image")
		if !errors.Is(err, ErrMissing) {
			t.Fatalf("want ErrMissing, got %v", err)
		}
	})
	t.Run("a member no launch can pick reaches nothing", func(t *testing.T) {
		t.Parallel()
		// The codex member names no cell of its own — it may not (see
		// TestARunInOnACLIThatLaunchesNoRunIsRefused) — so counted it would
		// open a box in the catalogue's default beside the head's.
		cfg := mustCompany(t, company("    local: {}\n    default_run_in: direct\n",
			entry("head", "")+"    codex:\n      model: gpt-5\n      agent: codex\n",
			"  - name: SWE\n    llm: [head, codex]\n"))
		for box := range cfg.SeatBoxes() {
			if box.Executor != "head" {
				t.Errorf("a box opens for %s, a member a launch skips", box.Executor)
			}
		}
	})
	t.Run("an entry no seat's executor holds reaches nothing", func(t *testing.T) {
		t.Parallel()
		// The same entry with the seat on another model: it launches
		// nothing, so its cell is not built and its questions are not
		// asked — the day a seat points at it, they are.
		cfg := mustCompany(t, company("    local: {}\n    default_run_in: direct\n",
			entry("main", "")+entry("spare", "e2b"), "  - name: SWE\n    llm: main\n    llm_review: spare\n"))
		if _, reached := cfg.SandboxPlacements()[PlacementE2B]; reached {
			t.Error("an entry only the reviewer reads reached a cell")
		}
	})
}

// A HUMAN SEAT RUNS NO EXECUTOR, so it reaches no providers.llm entry.
//
// `llm` is one of the fields a human seat may not carry, so its chain would
// always fall through to the company's fallback — the entry called "default",
// else the first declared. Resolved rather than skipped, that made an entry
// declared first "reached" by a seat that is never spawned: a company was
// refused for a box its founder's seat would never run in.
func TestAHumanSeatReachesNoExecutorEntry(t *testing.T) {
	t.Parallel()
	// The entry naming e2b is declared FIRST, so it is the fallback; the
	// catalogue has no e2b backend.
	const providers = "name: Acme\nproviders:\n  sandbox:\n    local: {}\n    default_run_in: direct\n" +
		"  llm:\n    coder:\n      model: sonnet\n      run_in: e2b\n" +
		"    main:\n      model: claude-golden\n"
	const founder = "  - name: Founder\n    kind: human\n    contact: {slack_user_id: U0FOUNDER}\n"

	cfg := mustCompany(t, providers+"roles:\n"+founder+"  - name: SWE\n    llm: main\n")
	if _, reached := cfg.SandboxPlacements()[PlacementE2B]; reached {
		t.Errorf("a cell is reached by a seat nobody spawns: %v", cfg.SandboxPlacements())
	}
	// And an AGENT seat falling through the same fallback still reaches it,
	// or this would just be "the fallback never counts".
	rejects(t, providers+"roles:\n"+founder+"  - name: SWE\n", "providers.llm.coder.run_in")
}

// THE SEATS LAUNCHING ON AN ENTRY ARE THE AGENT SEATS WHOSE CHAIN HOLDS IT,
// resolved as a launch resolves them — a seat naming nothing falls to the
// company's fallback entry and launches there — and never a person's seat,
// whose chain would fall to that same entry. It is what `crewlet llm doctor`
// reports an entry as launching for.
func TestTheSeatsLaunchingOnAnEntryAreTheAgentSeatsWhoseChainHoldsIt(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, "name: Acme\nproviders:\n  sandbox:\n    local: {}\n    default_run_in: direct\n"+
		"  llm:\n    coder: {model: sonnet}\n    main: {model: claude-golden}\n"+
		"roles:\n  - name: Founder\n    kind: human\n    contact: {slack_user_id: U0FOUNDER}\n"+
		"  - name: SWE\n    llm: main\n  - name: QA\n  - name: Lead\n    llm: [main, coder]\n")
	for key, want := range map[string][]string{
		"coder": {"qa", "lead"},
		"main":  {"swe", "lead"},
	} {
		if got := cfg.ExecutorSeatsOn(key); !slices.Equal(got, want) {
			t.Errorf("ExecutorSeatsOn(%s) = %v, want %v", key, got, want)
		}
	}
	if got := cfg.ExecutorSeatsOn("absent"); len(got) != 0 {
		t.Errorf("an entry the company lacks launches for %v", got)
	}
}

// A RUN_IN IS ONE OF THE CATALOGUE'S CELLS, spelled as the catalogue spells
// them. `self` was a seat's answer to a question that no longer exists — "my
// code work rides my executor's run" — and every executor is that run now.
func TestAnEntrysRunInIsACatalogueCell(t *testing.T) {
	t.Parallel()
	err := rejects(t, executorCompany("    sub:\n      model: sonnet\n      run_in: self\n",
		"  - name: SWE\n    llm: sub\n"), "providers.llm.sub.run_in")
	if !errors.Is(err, ErrUnknownValue) {
		t.Fatalf("want ErrUnknownValue, got %v", err)
	}
	if want := `"self" (want "direct", "container" or "e2b")`; !strings.Contains(err.Error(), want) {
		t.Errorf("the refusal does not offer the cells as %s:\n%v", want, err)
	}
	for _, cell := range Placements {
		mustCompany(t, executorCompany("    sub:\n      model: sonnet\n      run_in: "+string(cell)+"\n",
			"  - name: SWE\n    llm: sub\n"))
	}
}

// A RUN_IN NO LAUNCH CAN READ IS REFUSED. Only claude-code and opencode have an
// in-box runner; an entry on any other CLI serves text phases, so every launch
// passes over it and its placement is read by nothing — whether or not it is a
// seat's executor-chain member, and whatever cell it names, configured or not.
// Admitted, it read as a setting that placed the entry's runs while it did
// nothing at all.
func TestARunInOnACLIThatLaunchesNoRunIsRefused(t *testing.T) {
	t.Parallel()
	for _, agent := range []string{"codex", "gemini-cli", "custom"} {
		t.Run(agent, func(t *testing.T) {
			t.Parallel()
			err := rejects(t, executorCompany(
				"    sub:\n      model: sonnet\n"+
					"    other:\n      agent: "+agent+"\n      model: m-1\n      run_in: e2b\n",
				"  - name: SWE\n    llm: [sub, other]\n    llm_review: other\n"),
				"providers.llm.other.run_in")
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("want ErrConflict, got %v", err)
			}
			for _, remedy := range []string{"Remove the field", `only "claude-code" and "opencode" launch`} {
				if !strings.Contains(err.Error(), remedy) {
					t.Errorf("the refusal does not name %q:\n%v", remedy, err)
				}
			}
		})
	}
}

// THE BRIDGE'S NAME IS RESERVED. The engine writes the seat's tool bridge into
// every executor box's server list under it, so a company server of the same
// name would be replaced there — its tools gone from the run with no error and
// no log line saying why.
func TestTheToolServerNameIsReserved(t *testing.T) {
	t.Parallel()
	err := rejects(t, "name: Acme\nmcp_servers:\n  - name: "+ToolServerName+"\n    command: npx\n",
		"mcp_servers[0].name")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}
