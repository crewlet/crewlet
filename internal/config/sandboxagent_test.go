package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
)

// sandboxAgentCompany is a one-seat company whose seat runs code, on the given
// providers and seat lines.
func sandboxAgentCompany(sandbox, seat string) string {
	return "name: Acme\nproviders:\n  sandbox: {fake: true" + sandbox + "}\n  llm:\n" +
		"    oc:\n      model: anthropic/claude-sonnet-5\n      agent: opencode\n" +
		"    cc:\n      model: sonnet\n      agent: claude-code\n" +
		"    agent:\n      model: anthropic/claude-sonnet-5\n" +
		"      agent: opencode\n      mode: agent\n" +
		"roles:\n  - name: SWE\n" + seat
}

// A SEAT'S CODE WORK NEVER HANDS AN ENTRY TO ANOTHER CLI.
//
// An entry's model is written in its own CLI's grammar and its
// sign-in is that CLI's, so a runner of another CLI can read neither: an
// OpenCode text seat that enabled a sandbox ran Claude Code on
// `--model anthropic/claude-sonnet-5`, which it does not resolve, with an
// OpenCode key it never reads. The runner resolves as the launch resolves it
// — the seat's coding_agent, the catalogue's default, then Claude Code — and
// the entry as the phase registry resolves the sandbox phase.
func TestASeatsCodeWorkMustRunOnItsEntrysOwnCLI(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, sandbox, seat string
		refused             bool
	}{
		{"an opencode entry under the claude-code default", "",
			"    llm: oc\n    sandbox: {enabled: true}\n", true},
		{"an opencode entry under the catalogue's claude-code default", ", default_coding_agent: claude-code",
			"    llm: oc\n    sandbox: {enabled: true}\n", true},
		{"a claude-code entry under the catalogue's opencode default", ", default_coding_agent: opencode",
			"    llm: cc\n    sandbox: {enabled: true}\n", true},
		{"llm_sandbox wins over llm", "",
			"    llm: cc\n    llm_sandbox: oc\n    sandbox: {enabled: true}\n", true},
		{"the mapping form's sandbox key too", "",
			"    llm: {default: cc, sandbox: oc}\n    sandbox: {enabled: true}\n", true},
		{"the company's fallback, the first declared entry", "",
			"    sandbox: {enabled: true}\n", true},
		{"an opencode entry under its own runner", "",
			"    llm: oc\n    sandbox: {enabled: true, coding_agent: opencode}\n", false},
		{"the catalogue's default naming the entry's own runner", ", default_coding_agent: opencode",
			"    llm: oc\n    sandbox: {enabled: true}\n", false},
		{"a claude-code entry under the default", "",
			"    llm: cc\n    sandbox: {enabled: true}\n", false},
		{"llm_sandbox naming an entry of the default's own CLI", "",
			"    llm: oc\n    llm_sandbox: cc\n    sandbox: {enabled: true}\n", false},
		{"a seat that runs no code", "",
			"    llm: oc\n", false},
		// `self` rides the executor's own agent-mode run, whose runner is
		// always its entry's own CLI: the seat's coding_agent never drives
		// it, so its default is no pairing at all.
		{"code work that rides the executor's own run", "",
			"    llm: agent\n    sandbox: {enabled: true, run_in: self}\n", false},
		// Nor does an enabled gate on that seat with no `self`: a seat
		// whose executor runs in agent mode is never offered run_sandbox,
		// so the gate hands nobody's code work to any runner. Refused, it
		// was a pairing the engine never makes.
		{"an agent-mode seat's enabled gate, which offers it no box", "",
			"    llm: agent\n    sandbox: {enabled: true}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doc := sandboxAgentCompany(tc.sandbox, tc.seat)
			if !tc.refused {
				mustCompany(t, doc)
				return
			}
			err := rejects(t, doc, "roles[0].llm_sandbox")
			if !errors.Is(err, ErrConflict) {
				t.Errorf("want %v, got %v", ErrConflict, err)
			}
			if !strings.Contains(err.Error(), "can read neither") {
				t.Errorf("the refusal does not say why:\n%v", err)
			}
			assertAdmissionOnly(t, doc, "roles[0].llm_sandbox")
		})
	}
}

// THE REFUSAL OFFERS THE ENTRY'S OWN RUNNER ONLY WHERE ONE EXISTS: hermes has
// none, so telling its seat to set coding_agent: hermes would send it at a
// value the field refuses.
func TestTheCodingAgentRemedyNamesOnlyARunnerThatExists(t *testing.T) {
	t.Parallel()
	if got := CodingAgentMismatch("claude-code", "oc", "opencode"); !strings.Contains(got,
		"role.sandbox.coding_agent: opencode") {
		t.Errorf("an opencode entry is not offered its own runner: %s", got)
	}
	if got := CodingAgentMismatch("claude-code", "herm", "hermes"); strings.Contains(got,
		"coding_agent: hermes") {
		t.Errorf("a hermes entry is offered a runner that does not exist: %s", got)
	}
}

// THE SANDBOX ENTRY IS RESOLVED AS THE PHASE REGISTRY RESOLVES IT. Validation
// cannot build a provider, so it walks the fallbacks itself — and a rule here
// the registry does not have would refuse a seat over an entry its code work
// never runs on.
func TestBothPathsToTheSandboxEntryAgree(t *testing.T) {
	t.Parallel()
	providers := "name: Acme\nproviders:\n  llm:\n" +
		"    first:\n      model: a\n" +
		"    default:\n      model: b\n" +
		"    coder:\n      model: c\n"
	// PARSED, NOT VALIDATED: a key that misses is refused on a write, and
	// still reaches a node in a revision a newer peer admitted — the registry
	// falls back for it, and so must this.
	cfg, err := ParseCompanyDocument([]byte(providers + "roles:\n" +
		"  - name: Silent\n" +
		"  - name: Own\n    llm: first\n" +
		"  - name: Coder\n    llm: first\n    llm_sandbox: coder\n" +
		"  - name: Mapped\n    llm: {default: first, sandbox: coder}\n" +
		"  - name: Unknown\n    llm: first\n    llm_sandbox: nowhere\n" +
		"  - name: Chain\n    llm: first\n    llm_sandbox: [nowhere, coder]\n"))
	if err != nil {
		t.Fatal(err)
	}
	o, err := cfg.Organization()
	if err != nil {
		t.Fatal(err)
	}
	order := cfg.Providers.ProviderOrder()
	for i := range cfg.Roles {
		role := &cfg.Roles[i]
		got, _, ok := cfg.SandboxProvider(role)
		if !ok {
			t.Fatalf("%s: no sandbox entry", role.Name)
		}
		var want string
		for _, seat := range o.Roles {
			if seat.Name == role.Name {
				want = phase.Resolve(seat, phase.Sandbox, order)[0]
			}
		}
		if got != want {
			t.Errorf("%s: SandboxProvider = %q, the registry runs on %q", role.Name, got, want)
		}
	}
}
