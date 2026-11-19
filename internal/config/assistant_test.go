package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// assistantEntry is an ordinary providers.llm entry, as a seat runs it: Claude
// Code with its web tools on. A helper runs it with them off.
const assistantEntry = "    assistant:\n      harness: claude-code\n      model: sonnet\n"

// assistantCompany is a company whose providers are entries, with the
// assistant block's own lines appended.
func assistantCompany(entries, block string) string {
	return "name: Acme\nproviders:\n  llm:\n" + entries + "assistant:\n" + block
}

// ASK CREWLET IS OFF UNTIL A MODEL IS ASSIGNED. A company that never names it,
// and one that only caps a person's spend, run no assistant and are handed no
// model, so no upgrade can start a model reading a company's data for a person
// nobody chose.
func TestAskCrewletIsOffUntilAModelIsAssigned(t *testing.T) {
	t.Parallel()
	for name, doc := range map[string]string{
		"absent":      "name: Acme\n",
		"budget only": assistantCompany(assistantEntry, "  token_budget: {day: 200000}\n"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := mustCompany(t, doc)
			if cfg.Assistant.On() {
				t.Error("On() = true")
			}
			if _, err := cfg.AssistantModel(); !errors.Is(err, ErrAssistantOff) {
				t.Errorf("AssistantModel err = %v, want ErrAssistantOff", err)
			}
			if keys := cfg.HelperKeys(); len(keys) != 0 {
				t.Errorf("HelperKeys = %v, want none", keys)
			}
		})
	}
	raw, err := json.Marshal(mustCompany(t, "name: Acme\n"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"assistant"`) {
		t.Errorf("an unset assistant block was written: %s", raw)
	}
}

// ASK CREWLET IS CONFIGURED BY ONE MODEL: `assistant.model` and a person's
// spend ceiling are the block, and every setting it once had is refused as a
// key the document does not know, so none can quietly bound a turn again.
// Mutation: put any of them back on [Assistant] and its case loads.
func TestAskCrewletIsConfiguredByOneModel(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, assistantCompany(assistantEntry,
		"  model: assistant\n  token_budget: {day: 200000}\n"))
	if key, err := cfg.AssistantModel(); err != nil || key != "assistant" {
		t.Errorf("AssistantModel = %q, %v", key, err)
	}
	if keys := cfg.HelperKeys(); !slices.Equal(keys, []string{"assistant"}) {
		t.Errorf("HelperKeys = %v", keys)
	}
	for _, removed := range []string{
		"enabled: true", "llm: [assistant]", "max_steps: 30", "turn_timeout_seconds: 900",
		"proposal_ttl_seconds: 900", "retention_days: 30", "deliberate: {llm: [assistant]}",
		"tools: {deny: [writes]}",
	} {
		t.Run(removed, func(t *testing.T) {
			t.Parallel()
			key, _, _ := strings.Cut(removed, ":")
			rejects(t, assistantCompany(assistantEntry, "  model: assistant\n  "+removed+"\n"), key)
		})
	}
	// The per-person budget is a token budget, held to that type's rules.
	rejects(t, assistantCompany(assistantEntry, "  token_budget: {day: 0}\n"), "assistant.token_budget.day")
}

// A HELPER RUNS ON AN ENTRY THE COMPANY ALREADY HAS: an ordinary Claude Code
// or Pi entry, its web tools on for the seats, is assigned as it is, and the
// run a helper makes of it has its web tools off and no tool of its own,
// judged on that run's argv, while the entry's own run keeps its web. So a
// person assigns a model and makes no entry for it. Mutation: drop the forced
// web_tools from HelperOverrides and the helper's argv keeps WebFetch.
func TestAHelperRunsOnAnOrdinaryEntry(t *testing.T) {
	t.Parallel()
	for name, entry := range map[string]string{
		"claude-code": assistantEntry,
		"pi":          "    assistant:\n      harness: pi\n      model: sonnet\n",
		"claude-code that allows the web by name": "    assistant:\n      harness: claude-code\n" +
			"      model: sonnet\n      overrides: {web_tools: allowed}\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := mustCompany(t, assistantCompany(entry, "  model: assistant\n"))
			if _, err := cfg.AssistantModel(); err != nil {
				t.Fatalf("an ordinary entry was refused: %v", err)
			}
			if err := cfg.HelperModelProblem("assistant"); err != nil {
				t.Errorf("HelperModelProblem = %v", err)
			}
			spec := cfg.Providers.LLM["assistant"]
			own, err := harnessprofile.Load(spec.HarnessName(), spec.Overrides)
			if err != nil {
				t.Fatal(err)
			}
			if own.EffectiveWebTools() != harnessprofile.WebToolsAllowed {
				t.Errorf("the entry's own run lost its web tools: %s", own.EffectiveWebTools())
			}
			helper, err := harnessprofile.Load(spec.HarnessName(), spec.HelperOverrides())
			if err != nil {
				t.Fatal(err)
			}
			if helper.EffectiveWebTools() != harnessprofile.WebToolsDenied {
				t.Errorf("the helper's run keeps its web tools: %s", helper.EffectiveWebTools())
			}
			if problem := helper.ToolsOffProblem(); problem != "" {
				t.Errorf("the helper's run has a tool of its own: %s", problem)
			}
			for _, arg := range helper.CompleteArgs {
				if arg == "WebFetch,WebSearch" {
					t.Errorf("the helper's argv allows the web: %q", helper.CompleteArgs)
				}
			}
		})
	}
}

// A HELPER REFUSES ONLY A HARNESS IT CANNOT RUN WITH NO TOOL OF ITS OWN, never
// an entry's settings: Codex keeps apply_patch, which has no switch;
// OpenCode's deny-all policy has never been measured to leave a call offered
// no tool; Gemini has no measured web denial; a harness that keeps its local
// tools is refused; and a Claude Code entry whose override drops the empty
// allow list, or adds a tool server, claims a denial its argv no longer
// carries. Each is refused at `assistant.model` in a person's words, naming the
// harnesses that qualify, and AssistantModel and HelperModelProblem refuse the
// same entry, so the three can never disagree.
//
// Mutation: drop any one check from helperRunProblem and its case loads.
func TestAHelperRefusesOnlyAHarnessItCannotRunWithNoToolOfItsOwn(t *testing.T) {
	t.Parallel()
	denied := []string{"-p", "--output-format", "json", "--no-session-persistence",
		"--strict-mcp-config", "--disable-slash-commands", "--disallowedTools", "Bash", "Read",
		"WebFetch", "WebSearch"}
	argv := func(extra ...string) string {
		quoted := make([]string, 0, len(denied)+len(extra))
		for _, a := range append(slices.Clone(denied), extra...) {
			quoted = append(quoted, fmt.Sprintf("%q", a))
		}
		return "[" + strings.Join(quoted, ", ") + "]"
	}
	claude := func(args string) string {
		return "    assistant:\n      harness: claude-code\n      model: sonnet\n      overrides:\n" +
			"        web_denial: {complete_args: " + args + "}\n"
	}
	cases := map[string]struct{ entry, want string }{
		"codex keeps apply_patch": {
			entry: "    assistant:\n      harness: codex\n      model: gpt-5\n",
			want:  "no measured way to run the harness with no tool of its own",
		},
		"opencode is unmeasured": {
			entry: "    assistant:\n      harness: opencode\n      model: openrouter/qwen\n",
			want:  "no measured way to run the harness with no tool of its own",
		},
		"gemini cannot deny the web": {
			entry: "    assistant:\n      harness: gemini-cli\n      model: gemini-2.5-pro\n",
			want:  "no measured way to turn its own web tools off",
		},
		"local tools kept": {
			entry: "    assistant:\n      harness: custom\n      model: m\n      overrides:\n" +
				"        binary: mycli\n        complete_args: [run]\n        model_args: [--model, '{model}']\n" +
				"        output: text\n        local_tools: vendor-default\n" +
				"        local_tools_note: no denial flag\n" +
				"        web_denial: {seed_files: [{path: .mycli/policy.json, in: work, content: '{}'}]}\n",
			want: "keeps its own local tools",
		},
		"claude-code without the empty allow list": {
			entry: claude(argv()), want: `does not carry "--tools "`,
		},
		"claude-code with a tool server": {
			entry: claude(argv("--tools", "", "--mcp-config", "servers.json")),
			want:  "--mcp-config, which brings a tool back",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			doc := assistantCompany(tc.entry, "  model: assistant\n")
			err := rejects(t, doc, "assistant.model")
			if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not say %q:\n%v", tc.want, err)
			}
			if !strings.Contains(err.Error(), "claude-code, pi") {
				t.Errorf("the refusal does not name the harnesses that qualify:\n%v", err)
			}
			if said := Problems(err); len(said) == 0 || said[0].Said != helperRefusal {
				t.Errorf("the refusal is not in a person's words: %+v", said)
			}
			stored, decodeErr := ParseCompanyDocument([]byte(doc))
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if _, modelErr := stored.AssistantModel(); modelErr == nil ||
				!strings.Contains(modelErr.Error(), tc.want) {
				t.Errorf("AssistantModel handed out a model admission refuses: %v", modelErr)
			}
			if problem := stored.HelperModelProblem("assistant"); problem == nil ||
				!strings.Contains(problem.Error(), tc.want) {
				t.Errorf("HelperModelProblem passed a model admission refuses: %v", problem)
			}
		})
	}
	// The same Claude Code entry WITH the empty allow list is admitted.
	ok := mustCompany(t, assistantCompany(claude(argv("--tools", "")), "  model: assistant\n"))
	if _, err := ok.AssistantModel(); err != nil {
		t.Errorf("an entry whose argv removes every tool was refused: %v", err)
	}
}

// AN ASSIGNED MODEL IS ONE THE DOCUMENT HOLDS, refused naming the key where it
// is not, at admission and wherever it is asked.
func TestTheAssistantsModelMustBeAConfiguredEntry(t *testing.T) {
	t.Parallel()
	err := rejects(t, assistantCompany(assistantEntry, "  model: ghost\n"), "assistant.model")
	if !errors.Is(err, ErrUnknownValue) || !strings.Contains(err.Error(), `"ghost"`) {
		t.Errorf("an unknown key: want ErrUnknownValue naming it, got %v", err)
	}
	cfg := mustCompany(t, assistantCompany(assistantEntry, ""))
	if err := cfg.HelperModelProblem("ghost"); !errors.Is(err, ErrUnknownValue) {
		t.Errorf("HelperModelProblem(ghost) = %v", err)
	}
}

// THE BLOCK IS AN ADMISSION RULE. A stored revision whose model no helper can
// run on still APPLIES (every seat keeps running), and Ask Crewlet stays
// unavailable because AssistantModel refuses it.
func TestABrokenAssistantNeverStopsTheCompanyFromApplying(t *testing.T) {
	t.Parallel()
	stored, err := ParseCompanyDocument([]byte(assistantCompany(
		"    assistant:\n      harness: codex\n      model: gpt-5\n", "  model: assistant\n")))
	if err != nil {
		t.Fatal(err)
	}
	if err := stored.ValidateRunnable(); err != nil {
		t.Errorf("a broken assistant block refused the company's apply: %v", err)
	}
	if err := stored.ValidateAdmission(); err == nil {
		t.Error("admission accepted a model no helper can run on")
	}
	if _, err := stored.AssistantModel(); err == nil {
		t.Error("AssistantModel handed out a model no helper can run on")
	}
}

// Research lists bare host names: none, a scheme, a port, a path, a wildcard, an
// upper-case name or a repeat is refused naming the entry; absent, it is off.
func TestResearchListsBareHostNames(t *testing.T) {
	t.Parallel()
	ok, err := ParseCompany([]byte(assistantCompany("",
		"  research:\n    hosts: [developers.cloudflare.com, docs.github.com]\n")))
	if err != nil {
		t.Fatal(err)
	}
	if got := ok.Assistant.ResearchHosts(); len(got) != 2 {
		t.Errorf("research hosts read as %v", got)
	}
	off, err := ParseCompany([]byte("name: Acme\n"))
	if err != nil || off.Assistant.ResearchHosts() != nil {
		t.Errorf("a company with no research block reads hosts %v (%v)",
			off.Assistant.ResearchHosts(), err)
	}
	for name, tc := range map[string]struct{ hosts, path string }{
		"none":       {"[]", "assistant.research.hosts"},
		"a scheme":   {"[https://docs.github.com]", "assistant.research.hosts[0]"},
		"a port":     {"[docs.github.com:443]", "assistant.research.hosts[0]"},
		"a path":     {"[docs.github.com/en]", "assistant.research.hosts[0]"},
		"a wildcard": {"['*.github.com']", "assistant.research.hosts[0]"},
		"upper case": {"[Docs.GitHub.com]", "assistant.research.hosts[0]"},
		"a repeat":   {"[docs.github.com, docs.github.com]", "assistant.research.hosts[1]"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rejects(t, assistantCompany("", "  research:\n    hosts: "+tc.hosts+"\n"), tc.path)
		})
	}
}

// Research search names a shared, enabled Tavily entry whose search is not
// blocked; any other entry, or none the document holds, is refused naming it.
func TestResearchSearchNamesTheCompanysTavilyServer(t *testing.T) {
	t.Parallel()
	doc := func(servers, name string) string {
		return "name: Acme\nmcp_servers:\n" + servers + "assistant:\n  research:\n" +
			"    hosts: [docs.example.com]\n    search: {mcp_server: " + name + "}\n"
	}
	const tavily = "  - name: search\n    command: npm\n    args: [exec, --yes, --, tavily-mcp@0.2.22]\n" +
		"    env: {TAVILY_API_KEY: '${TAVILY_API_KEY}'}\n"
	if _, err := ParseCompany([]byte(doc(tavily, "search"))); err != nil {
		t.Fatalf("a shared Tavily entry was refused: %v", err)
	}
	for name, tc := range map[string]struct{ servers, server string }{
		"unknown entry": {tavily, "nothing"},
		"not Tavily": {"  - name: search\n    command: npx\n    args: [-y, some-other-server]\n",
			"search"},
		"per role":       {tavily + "    shared: false\n", "search"},
		"search blocked": {tavily + "    tools: {levels: {tavily_search: blocked}}\n", "search"},
		"turned off":     {tavily + "    enabled: false\n", "search"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rejects(t, doc(tc.servers, tc.server), "assistant.research.search.mcp_server")
		})
	}
}
