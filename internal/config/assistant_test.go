package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// assistantEntry is a providers.llm entry the assistant may run on: Claude
// Code in text mode with its web tools turned off.
const assistantEntry = "    assistant:\n" +
	"      agent: claude-code\n      model: sonnet\n" +
	"      overrides: {web_tools: denied}\n"

// assistantCompany is a company running the assistant on entries, with the
// assistant block's own lines appended.
func assistantCompany(entries, block string) string {
	return "name: Acme\nproviders:\n  llm:\n" + entries + "assistant:\n" + block
}

// THE ASSISTANT IS OFF UNTIL SOMEBODY TURNS IT ON. A company that never names
// it, and one that wrote `enabled: false`, run no assistant and are handed no
// chain, so no upgrade can start a model reading a company's data for a
// person nobody chose.
func TestTheAssistantIsOffUntilTurnedOn(t *testing.T) {
	t.Parallel()
	for name, doc := range map[string]string{
		"absent":   "name: Acme\n",
		"disabled": assistantCompany(assistantEntry, "  enabled: false\n  llm: [assistant]\n"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := mustCompany(t, doc)
			if cfg.Assistant.On() {
				t.Error("On() = true")
			}
			if _, err := cfg.AssistantChain(); !errors.Is(err, ErrAssistantOff) {
				t.Errorf("AssistantChain err = %v, want ErrAssistantOff", err)
			}
		})
	}
	// And an unset block leaves no trace in the stored JSON.
	raw, err := json.Marshal(mustCompany(t, "name: Acme\n"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"assistant"`) {
		t.Errorf("an unset assistant block was written: %s", raw)
	}
}

// A CHAIN THAT KEEPS EVERY RULE IS HANDED OUT, in order.
func TestASafeAssistantChainIsHandedOut(t *testing.T) {
	t.Parallel()
	second := "    backup:\n      agent: pi\n      model: sonnet\n      overrides: {web_tools: denied}\n"
	cfg := mustCompany(t, assistantCompany(assistantEntry+second, "  enabled: true\n  llm: [assistant, backup]\n"))
	chain, err := cfg.AssistantChain()
	if err != nil {
		t.Fatalf("AssistantChain: %v", err)
	}
	if !slices.Equal(chain, ProviderKeys{"assistant", "backup"}) {
		t.Errorf("chain = %v", chain)
	}
}

// THE ASSISTANT REFUSES AN ENTRY THAT REACHES THE WEB, or anything else the
// person's own requests did not return: a CLI in agent mode (its own tool
// loop never passes the guard), one whose local tools are not denied (a shell
// reads what the engine user can), one that keeps its web tools (a page no
// authority decision saw, and a way out for the person's data), and one on a
// CLI with no measured way to turn them off. Each is refused at the chain
// member, naming the entry's own field to change, and AssistantChain refuses
// the same document, so the two can never disagree.
//
// Mutation: drop any one check from assistantMemberProblems and its case
// loads.
func TestTheAssistantRefusesAnEntryThatReachesTheWeb(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		entry string
		kind  error
		want  string
	}{
		"agent mode": {
			entry: "    assistant:\n      agent: claude-code\n      model: sonnet\n      mode: agent\n" +
				"      overrides: {web_tools: denied}\n",
			kind: ErrConflict, want: "providers.llm.assistant.mode",
		},
		"local tools not denied": {
			entry: "    assistant:\n      agent: custom\n      model: m\n      overrides:\n" +
				"        binary: mycli\n        complete_args: [run]\n        model_args: [--model, '{model}']\n" +
				"        output: text\n        local_tools: vendor-default\n" +
				"        local_tools_note: no denial flag\n        web_tools: denied\n" +
				"        web_denial: {seed_files: [{path: .mycli/policy.json, in: work, content: '{}'}]}\n",
			kind: ErrConflict, want: "local_tools: denied",
		},
		"web allowed": {
			entry: "    assistant:\n      agent: claude-code\n      model: sonnet\n",
			kind:  ErrConflict, want: "providers.llm.assistant.overrides: {web_tools: denied}",
		},
		"web undeniable": {
			entry: "    assistant:\n      agent: gemini-cli\n      model: gemini-2.5-pro\n",
			kind:  ErrConflict, want: "no measured web denial",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			doc := assistantCompany(tc.entry, "  enabled: true\n  llm: [assistant]\n")
			err := rejects(t, doc, "assistant.llm[0]")
			if !errors.Is(err, tc.kind) {
				t.Errorf("kind: want %v, got %v", tc.kind, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not name %q:\n%v", tc.want, err)
			}
			stored, decodeErr := ParseCompanyDocument([]byte(doc))
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if _, chainErr := stored.AssistantChain(); chainErr == nil || !strings.Contains(chainErr.Error(), tc.want) {
				t.Errorf("AssistantChain handed out a chain admission refuses: %v", chainErr)
			}
		})
	}
}

// AN ENABLED ASSISTANT NAMES A MODEL, and every key it names is configured.
func TestTheAssistantChainMustNameConfiguredEntries(t *testing.T) {
	t.Parallel()
	err := rejects(t, assistantCompany(assistantEntry, "  enabled: true\n"), "assistant.llm")
	if !errors.Is(err, ErrMissing) {
		t.Errorf("an enabled assistant with no llm: want ErrMissing, got %v", err)
	}
	err = rejects(t, assistantCompany(assistantEntry, "  enabled: true\n  llm: [assistant, ghost]\n"), "assistant.llm[1]")
	if !errors.Is(err, ErrUnknownValue) || !strings.Contains(err.Error(), `"ghost"`) {
		t.Errorf("an unknown key: want ErrUnknownValue naming it, got %v", err)
	}
	// A key listed while the assistant is off is still a typo, and is
	// still refused where it is written.
	rejects(t, assistantCompany(assistantEntry, "  llm: [ghost]\n"), "assistant.llm[0]")
}

// THE ASSISTANT BLOCK VALIDATES EVERY RANGE: each numeric field refuses a
// value outside its range naming itself, takes its bounds, and reads zero as
// its default. A value admission would refuse that a stored revision still
// carries is clamped by the accessor rather than acted on as written.
func TestTheAssistantBlockValidatesEveryRange(t *testing.T) {
	t.Parallel()
	type read func(Assistant) int
	fields := []struct {
		bound assistantBound
		set   func(*Assistant, int)
		read  read
	}{
		{assistantSteps, func(a *Assistant, v int) { a.MaxSteps = v }, func(a Assistant) int { return a.Steps() }},
		{assistantTimeout, func(a *Assistant, v int) { a.TurnTimeoutSeconds = v },
			func(a Assistant) int { return int(a.TurnTimeout() / time.Second) }},
		{assistantProposal, func(a *Assistant, v int) { a.ProposalTTLSeconds = v },
			func(a Assistant) int { return int(a.ProposalTTL() / time.Second) }},
		{assistantRetention, func(a *Assistant, v int) { a.RetentionDays = v },
			func(a Assistant) int { return int(a.Retention() / (24 * time.Hour)) }},
	}
	wantBounds := map[string][3]int{
		"max_steps": {1, 16, 8}, "turn_timeout_seconds": {30, 600, 120},
		"proposal_ttl_seconds": {60, 3600, 900}, "retention_days": {1, 365, 30},
	}
	for _, f := range fields {
		t.Run(f.bound.key, func(t *testing.T) {
			t.Parallel()
			if got := [3]int{f.bound.min, f.bound.max, f.bound.def}; got != wantBounds[f.bound.key] {
				t.Errorf("bounds = %v, want %v", got, wantBounds[f.bound.key])
			}
			doc := func(v int) string {
				return assistantCompany(assistantEntry, fmt.Sprintf("  llm: [assistant]\n  %s: %d\n", f.bound.key, v))
			}
			for _, bad := range []int{f.bound.min - 1, f.bound.max + 1, -1} {
				if bad == 0 {
					continue
				}
				err := rejects(t, doc(bad), "assistant."+f.bound.key)
				if !errors.Is(err, ErrOutOfRange) {
					t.Errorf("%d: want ErrOutOfRange, got %v", bad, err)
				}
			}
			for _, good := range []int{f.bound.min, f.bound.max} {
				cfg := mustCompany(t, doc(good))
				if got := f.read(cfg.Assistant); got != good {
					t.Errorf("%d reads as %d", good, got)
				}
			}
			var a Assistant
			if got := f.read(a); got != f.bound.def {
				t.Errorf("zero reads as %d, want the default %d", got, f.bound.def)
			}
			f.set(&a, f.bound.max+100)
			if got := f.read(a); got != f.bound.max {
				t.Errorf("a stored %d reads as %d, want it clamped to %d", f.bound.max+100, got, f.bound.max)
			}
			f.set(&a, -5)
			if got := f.read(a); got != f.bound.min {
				t.Errorf("a stored -5 reads as %d, want it clamped to %d", got, f.bound.min)
			}
		})
	}
	// The per-person budget is a token budget, held to that type's rules.
	rejects(t, assistantCompany(assistantEntry, "  token_budget: {day: 0}\n"), "assistant.token_budget.day")
	cfg := mustCompany(t, assistantCompany(assistantEntry, "  token_budget: {day: 200000}\n"))
	if day := cfg.Assistant.TokenBudget.Day; day == nil || *day != 200000 {
		t.Errorf("token_budget.day = %v", day)
	}
}

// THE BLOCK IS AN ADMISSION RULE. A stored revision whose assistant breaks a
// rule still APPLIES (every seat keeps running), and the assistant stays
// unavailable because AssistantChain refuses it.
func TestABrokenAssistantNeverStopsTheCompanyFromApplying(t *testing.T) {
	t.Parallel()
	stored, err := ParseCompanyDocument([]byte(assistantCompany(
		"    assistant:\n      agent: claude-code\n      model: sonnet\n",
		"  enabled: true\n  llm: [assistant]\n  max_steps: 99\n")))
	if err != nil {
		t.Fatal(err)
	}
	if err := stored.ValidateRunnable(); err != nil {
		t.Errorf("a broken assistant block refused the company's apply: %v", err)
	}
	if err := stored.ValidateAdmission(); err == nil {
		t.Error("admission accepted a chain that keeps the web and an out-of-range step count")
	}
	if _, err := stored.AssistantChain(); err == nil {
		t.Error("AssistantChain handed out a chain that keeps the web")
	}
}
