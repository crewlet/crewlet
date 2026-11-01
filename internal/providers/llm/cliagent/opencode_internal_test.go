package cliagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// The invariants the built-in opencode profile keeps, each measured against
// OpenCode 2.0.32 and 2.1.4 (see the profile's comments): a seat's phases keep
// OpenCode's own system prompt and a side pass replaces it, both through files
// in the directory OPENCODE_CONFIG_DIR names inside the call's own working
// directory, and a call is offered the two web tools and nothing else.

// openCode is the shipped profile, which every case here reads. Both of its
// channels are files: OpenCode has no flag and no variable for either.
func openCode(t *testing.T) cliprofile.Profile {
	t.Helper()
	p, ok := cliprofile.Builtin("opencode")
	if !ok {
		t.Fatal("no built-in opencode profile")
	}
	if p.SystemPrompt.File == nil || p.AppendSystemPrompt.File == nil {
		t.Fatalf("system_prompt.file = %+v, append_system_prompt.file = %+v: the opencode "+
			"profile declares both channels as files OpenCode finds by their location",
			p.SystemPrompt.File, p.AppendSystemPrompt.File)
	}
	return p
}

// BOTH PROMPT FILES LIE WHERE OPENCODE LOOKS, in the configuration directory
// the profile points OPENCODE_CONFIG_DIR at: the AGENTS.md it reads on every
// run in that directory, and the agent file `--agent` selects from its agents/
// directory. A file anywhere else is one OpenCode never reads — the seat would
// run on the vendor's prompt alone, and a side pass would fail with "Agent not
// found". Derived from the profile's own fields, so moving the directory
// without its files fails here as it fails at load.
func TestOpenCodesPromptFilesLieWhereItLooks(t *testing.T) {
	t.Parallel()
	p := openCode(t)
	dir, ok := p.WorkConfigEnv["OPENCODE_CONFIG_DIR"]
	if !ok {
		t.Fatalf("work_config_env = %v names no OPENCODE_CONFIG_DIR", p.WorkConfigEnv)
	}

	appendFile := p.AppendSystemPrompt.File
	if !appendFile.ReadAlways() {
		t.Fatalf("append_system_prompt.file = %+v, want a file OpenCode reads on every run", appendFile)
	}
	if want := filepath.Join(dir, "AGENTS.md"); filepath.Clean(appendFile.Name) != want {
		t.Errorf("append_system_prompt.file.name = %q, want %q: OpenCode reads the AGENTS.md "+
			"of its configuration directory and no other name", appendFile.Name, want)
	}
	if len(p.AppendSystemPrompt.Args) > 0 || p.AppendSystemPrompt.Env != "" {
		t.Errorf("append_system_prompt = %+v carries argv or a variable: the default agent "+
			"reads the file unasked, and --agent would replace OpenCode's prompt", p.AppendSystemPrompt)
	}

	replaceFile := p.SystemPrompt.File
	if replaceFile == nil || replaceFile.Discovered != cliprofile.DiscoverySelected {
		t.Fatalf("system_prompt.file = %+v, want an agent file --agent selects", replaceFile)
	}
	agent, ok := valueAfter(p.SystemPrompt.Args, "--agent")
	if !ok {
		t.Fatalf("system_prompt.args = %q selects no agent", p.SystemPrompt.Args)
	}
	if want := filepath.Join(dir, "agents", agent+".md"); filepath.Clean(replaceFile.Name) != want {
		t.Errorf("system_prompt.file.name = %q, want %q, the file --agent %s selects",
			replaceFile.Name, want, agent)
	}
	// The agent file is written on every side pass, a system prompt or none:
	// --agent naming a file that is not there fails the call.
	if !replaceFile.CarriesPolicy() {
		t.Errorf("system_prompt.file.template = %q says nothing without a system prompt, so a "+
			"side pass with none would select an agent that does not exist", replaceFile.Template)
	}
}

// A SEAT KEEPS OPENCODE'S OWN PROMPT AND A SIDE PASS REPLACES IT. Driven
// through Complete with the shipped profile, so what is checked is the argv a
// call is handed and the file the call's channel writes: a seat runs OpenCode's
// default agent, whose prompt stays in force, with the engine's text (behind the
// harness note) in AGENTS.md; a side pass selects the agent file, whose body is
// the whole system prompt. Never both: on 2.0.32 and 2.1.4 an AGENTS.md beside
// the selected agent is appended to that agent's prompt too.
func TestOpenCodeSeatAppendsAndASidePassReplaces(t *testing.T) {
	t.Parallel()
	const identity = "You are Agent PM at Nimbus."
	p := openCode(t)
	for name, tc := range map[string]struct {
		agent       bool
		selects     bool
		writes, not string
		body        string
	}{
		"a seat at work": {
			agent: true, writes: p.AppendSystemPrompt.File.Name, not: p.SystemPrompt.File.Name,
			body: harnessNote + "\n\n" + identity,
		},
		"a side pass": {
			agent: false, selects: true, writes: p.SystemPrompt.File.Name, not: p.AppendSystemPrompt.File.Name,
			body: p.SystemPrompt.File.Render(identity),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := t.TempDir()
			dir := t.TempDir()
			t.Cleanup(func() { forgetWorkspace(dir) })
			prov, err := New(Config{
				Key: "oc", Model: "anthropic/claude-sonnet-5", Agent: "opencode", StateDir: dir,
				Overrides: map[string]any{"binary": os.Args[0]},
				Timeout:   20 * time.Second, MaxConcurrent: 1,
				Env: fakeChildEnv(map[string]string{
					"FAKE_STDOUT":      `{"type":"text","part":{"text":"ok"}}`,
					"FAKE_RECORD_ARGV": rec,
				}),
				Auth: Auth{Mode: AuthSubscription},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			req := llm.Request{Agent: tc.agent, Messages: []llm.Message{
				{Role: llm.RoleSystem, Content: identity},
				{Role: llm.RoleUser, Content: "status?"},
			}}
			comp, err := ask(t, prov, req)
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if comp.Content != "ok" {
				t.Errorf("content = %q, want the text event the CLI printed", comp.Content)
			}
			var run []string
			for _, argv := range recordedArgv(t, rec) {
				if len(argv) > 0 && argv[0] == "run" {
					run = argv
				}
			}
			if run == nil {
				t.Fatalf("no `run` invocation was recorded: %q", recordedArgv(t, rec))
			}
			if got := slices.Contains(run, "--agent"); got != tc.selects {
				t.Errorf("argv = %q: --agent present = %v, want %v — selecting the agent "+
					"replaces OpenCode's prompt, and leaving it off keeps it", run, got, tc.selects)
			}
			for _, arg := range run {
				if strings.Contains(arg, "Nimbus") {
					t.Errorf("argv = %q carries the prompt, which travels in a file", run)
				}
			}

			// The file this kind of call's channel writes, and only that one,
			// rendered by the function Complete hands the route to.
			work := t.TempDir()
			route := p.RouteFor(tc.agent)
			system := identity
			if route.KeepsDefault {
				system, _ = SplitSystem(withHarnessNote(req))
			}
			args, err := systemArgs(route.Channel, system, work)
			if err != nil {
				t.Fatal(err)
			}
			if got := slices.Contains(args, "--agent"); got != tc.selects {
				t.Errorf("the channel's argv = %q, want --agent present = %v", args, tc.selects)
			}
			body, err := os.ReadFile(filepath.Join(work, tc.writes))
			if err != nil {
				t.Fatalf("the channel wrote no %s: %v", tc.writes, err)
			}
			if string(body) != tc.body {
				t.Errorf("%s holds %q, want %q", tc.writes, body, tc.body)
			}
			if _, err := os.Stat(filepath.Join(work, tc.not)); !os.IsNotExist(err) {
				t.Errorf("the call also wrote %s, which OpenCode would read beside the "+
					"channel's own file (stat: %v)", tc.not, err)
			}
		})
	}
}

// NO SYSTEM PROMPT, NO AGENTS.md. OpenCode reads that file on every run, and
// an empty one still reaches the model as an "Instructions from: <path>" line
// with nothing under it (measured on 2.0.32 and 2.1.4). A seat's call with no
// system prompt runs on OpenCode's own prompt and nothing else, which is what
// no system prompt means on every other channel. Asked from inside the child,
// because the call's directory is removed on release.
func TestOpenCodeWritesNoAGENTSmdForARequestWithNoSystemPrompt(t *testing.T) {
	t.Parallel()
	ch := openCode(t).AppendSystemPrompt
	p := fakeProvider(t, map[string]string{"FAKE_READ_DISCOVERED": ch.File.Name}, map[string]any{
		"append_system_prompt": map[string]any{
			"file": map[string]any{"name": ch.File.Name, "discovered": string(ch.File.Discovered)},
		},
	})
	comp, err := ask(t, p, llm.Request{Agent: true, Messages: []llm.Message{
		{Role: llm.RoleUser, Content: "status?"},
	}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !strings.HasPrefix(comp.Content, "UNREADABLE") {
		t.Errorf("a request with no system prompt left %s in the call's directory: %q",
			ch.File.Name, comp.Content)
	}
}

// THE SEED OFFERS THE WEB TOOLS AND NOTHING ELSE. OpenCode's permissions are an
// ordered list in which the last matching rule wins and whatever no rule denies
// is allowed, so the policy is an allowlist only while it opens by denying
// every action and then allows exactly webfetch and websearch: an allow for
// anything else reopens a tool, and a deny-list in place of the first rule
// lets through every tool a release adds. Measured on 2.0.32 and 2.1.4, this
// seed offers exactly those two. The seed also turns off the built-in
// `opencode.tools` plugin, which adds OpenCode's own session tools and a
// system-prompt sentence telling the model to call one through `execute`, a
// tool no call is offered.
func TestOpenCodesSeedOffersTheWebToolsAndNothingElse(t *testing.T) {
	t.Parallel()
	p := openCode(t)
	var content string
	for _, seed := range p.SeedFiles {
		if filepath.Base(seed.Path) == "opencode.json" && seed.Scope() == cliprofile.SeedWork {
			content = seed.Content
		}
	}
	if content == "" {
		t.Fatalf("seed_files = %+v carries no work-scoped opencode.json", p.SeedFiles)
	}
	var doc struct {
		Permissions []struct {
			Action, Resource, Effect string
		} `json:"permissions"`
		Plugins []string `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatalf("the seeded opencode.json is not JSON: %v", err)
	}
	if len(doc.Permissions) == 0 {
		t.Fatal("the seed carries no permissions, so OpenCode offers every tool it has")
	}
	first := doc.Permissions[0]
	if first.Action != "*" || first.Resource != "*" || first.Effect != "deny" {
		t.Errorf("the first rule is %+v, want every action denied before anything is allowed", first)
	}
	var allowed []string
	for i, rule := range doc.Permissions[1:] {
		if rule.Effect != "allow" || rule.Resource != "*" {
			t.Errorf("permissions[%d] = %+v, want an allow of one web tool", i+1, rule)
			continue
		}
		allowed = append(allowed, rule.Action)
	}
	slices.Sort(allowed)
	if want := []string{"webfetch", "websearch"}; !slices.Equal(allowed, want) {
		t.Errorf("the seed allows %q, want exactly %q", allowed, want)
	}
	if !slices.Contains(doc.Plugins, "-opencode.tools") {
		t.Errorf("plugins = %q leaves the built-in opencode.tools plugin on", doc.Plugins)
	}
}

// AN OPERATOR CAN TRADE OPENCODE'S PROMPT FOR A CACHEABLE ONE. The append
// channel puts a per-call path in front of a seat's text, so an operator who
// wants that text cached drops the channel in overrides, and seats then take
// the agent file a side pass takes, whose body opens the system prompt.
func TestDroppingOpenCodesAppendChannelSendsSeatsToTheAgentFile(t *testing.T) {
	t.Parallel()
	p, err := cliprofile.Load("opencode", map[string]any{"append_system_prompt": nil})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	route := p.RouteFor(true)
	if route.Field != "system_prompt" || route.KeepsDefault {
		t.Errorf("a seat's route = %+v, want the replacing agent file", route)
	}
}
