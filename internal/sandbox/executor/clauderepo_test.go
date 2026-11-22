package executor_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// plantRepository makes dir a repository checked out in the box, holding every
// kind of instruction a native session reads (each carrying a RESTORED- word)
// and every kind of setting (each carrying a planted- word).
func plantRepository(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir+"/.git", 0o700); err != nil {
		t.Fatal(err)
	}
	for file, body := range map[string]string{
		"CLAUDE.md":             "RESTORED-CLAUDEMD\n@docs/extra.md\n",
		"docs/extra.md":         "RESTORED-IMPORT\n",
		"CLAUDE.local.md":       "RESTORED-LOCAL\n",
		".claude/CLAUDE.md":     "RESTORED-DOTCLAUDE\n",
		".claude/rules/rule.md": "RESTORED-RULE\n",
		".claude/skills/deploy/SKILL.md": "---\nname: deploy\ndescription: RESTORED-SKILL\n" +
			"allowed-tools: RESTORED-LIMIT\nmodel: planted-model\nplanted-key: planted-unknown\nhooks:\n  Stop:\n" +
			"    - hooks:\n        - type: command\n          command: planted-skill-hook\n---\nRESTORED-SKILLBODY\n",
		".claude/commands/ship.md": "---\ndescription: RESTORED-COMMAND\n---\nShip it.\n",
		".claude/agents/review.md": "---\nname: review\ndescription: RESTORED-AGENT\nmcpServers:\n  planted-agent-server:\n" +
			"    command: planted-agent-mcp\npermissionMode: planted-permission\n---\nRESTORED-AGENTBODY\n",
		".claude/settings.json": `{"env":{"ANTHROPIC_BASE_URL":"http://planted-endpoint.example.com"},` +
			`"apiKeyHelper":"planted-key-helper","hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"planted-hook"}]}]}}`,
		".mcp.json": `{"mcpServers":{"planted-server":{"command":"planted-mcp"}}}`,
	} {
		plantFile(t, filepath.Join(dir, file), body)
	}
	// A link out of the repository, which no copy may follow.
	if err := os.Symlink("/etc", dir+"/.claude/skills/linked"); err != nil {
		t.Fatal(err)
	}
}

// A CLAUDE CODE RUN IS HANDED ITS REPOSITORIES' INSTRUCTIONS, NOT THEIR
// SETTINGS. For the working directory and each repository checked out under
// it: the CLAUDE.md family through `--add-dir` (its imports, .claude/CLAUDE.md,
// rules, and CLAUDE.local.md through a directory of the run's own), and its
// skills, commands and agents copied into a plugin named after it, frontmatter
// cut to what describes or limits them (a `model`, and a key nobody named, are
// dropped). No settings `env`, key helper, hook or MCP server comes back, a
// skill's or an agent's included, and no plugin takes the engine's own name. Played on a real box, the harness a stand-in reading
// what it is handed as 2.1.330 does.
//
// Mutation: drop the gate, the variable, a kind from the copy, or the
// frontmatter cut, and a word goes missing or a planted one is read.
func TestAClaudeCodeRunIsHandedItsRepositoriesInstructionsNotTheirSettings(t *testing.T) {
	t.Parallel()
	runner, b := realBox(t)
	writeBoxStandIn(t, b, executor.PathsFor(b).BinDir()+"/claude", sourcesHarness("2.1.330"))
	p := executor.PathsFor(b)
	work := filepath.Join(b.Home(), sandbox.WorkspaceSubdir)
	plantFile(t, work+"/CLAUDE.md", "RESTORED-WORKSPACE\n")
	plantRepository(t, work+"/app")
	plantRepository(t, work+"/crewlet")

	_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x", Skills: handedSkills(), SkillLoads: skillWiring()})
	if !res.Success {
		t.Fatalf("the run failed: %s\n%s", res.Text, boxFile(t, b, paths(b).Err()))
	}
	loaded := boxFile(t, b, b.Home()+"/loaded")
	for _, word := range []string{"RESTORED-WORKSPACE", "RESTORED-CLAUDEMD", "RESTORED-IMPORT", "RESTORED-LOCAL",
		"RESTORED-DOTCLAUDE", "RESTORED-RULE", "RESTORED-SKILL", "RESTORED-SKILLBODY", "RESTORED-COMMAND",
		"RESTORED-AGENT", "RESTORED-AGENTBODY", "RESTORED-LIMIT"} {
		if !strings.Contains(loaded, word) {
			t.Errorf("the run did not read %s:\n%s", word, loaded)
		}
	}
	for _, word := range []string{"planted-endpoint", "planted-key-helper", "planted-hook", "planted-server",
		"planted-skill-hook", "planted-agent-server", "planted-agent-mcp", "planted-permission", "== HOOK",
		"planted-model", "planted-unknown"} {
		if strings.Contains(loaded, word) {
			t.Errorf("the run read %s:\n%s", word, loaded)
		}
	}
	if !strings.Contains(loaded, "== "+p.ClaudeSkillsPlugin()+"/.claude-plugin/plugin.json") {
		t.Errorf("the run's own skills plugin was not read:\n%s", loaded)
	}
	if links, _ := filepath.Glob(p.ClaudeRepoPlugins() + "/*/skills/linked"); len(links) > 0 {
		t.Errorf("a link out of the repository was copied: %v", links)
	}
	manifests, _ := filepath.Glob(p.ClaudeRepoPlugins() + "/*/.claude-plugin/plugin.json")
	names := map[string]bool{}
	for _, m := range manifests {
		raw, _ := os.ReadFile(m) //nolint:gosec // the case's own box
		names[string(raw)] = true
	}
	if !names["{\"name\":\"app\"}\n"] || len(names) != 2 || names["{\"name\":\""+sandbox.SkillNamespace+"\"}\n"] {
		t.Errorf("the repositories' plugins are named %v, want app and one that is not %q", names,
			sandbox.SkillNamespace)
	}
}

// A CLAUDE CODE OLDER THAN 2.1.118 STARTS WITHOUT ITS REPOSITORIES'
// INSTRUCTIONS, saying so: it takes no `--plugin-dir` and reads no
// CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD.
//
// Mutation: drop the release check from the gate, and the older harness is
// handed directories it cannot read.
func TestAClaudeCodeOlderThanItsRepositoryFloorStartsWithoutThem(t *testing.T) {
	t.Parallel()
	runner, b := realBox(t)
	writeBoxStandIn(t, b, executor.PathsFor(b).BinDir()+"/claude", sourcesHarness("2.1.117"))
	plantRepository(t, filepath.Join(b.Home(), sandbox.WorkspaceSubdir, "app"))
	_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
	if !res.Success {
		t.Fatalf("the run failed: %s\n%s", res.Text, boxFile(t, b, paths(b).Err()))
	}
	if loaded := boxFile(t, b, b.Home()+"/loaded"); strings.Contains(loaded, "RESTORED-") {
		t.Errorf("Claude Code 2.1.117 was handed its repositories' instructions:\n%s", loaded)
	}
	if !strings.Contains(boxFile(t, b, paths(b).Err()), "older than 2.1.118") {
		t.Errorf("the error stream does not say why:\n%s", boxFile(t, b, paths(b).Err()))
	}
}

// THE NESTED HOOK HANDS BACK WHAT A NATIVE SESSION LOADS ON A READ, ONCE: for a
// file under a repository's root, each CLAUDE.md between the root and the file
// with its imports, and each rule whose `paths` globs match the file, as the
// hook's additional context; never the root's own (loaded at the start), a
// rule without `paths` or one that does not match, and nothing a second time
// or for a file outside every root. Played with the script the launch writes,
// on a box's own directories.
//
// Mutation: drop the claim, the glob's `**/`, or the walk's stop at the root,
// and a call hands too much or too little.
func TestTheNestedHookHandsBackWhatANativeSessionLoadsOnARead(t *testing.T) {
	t.Parallel()
	local, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Direct, StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	box, err := local.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { box.Close(t.Context()) })
	p := executor.PathsFor(box)
	if _, err = claude().WriteConfig(t.Context(), box, sandbox.RunRequest{Brief: "x"}, executor.Session{}, p); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	root := filepath.Join(box.Home(), sandbox.WorkspaceSubdir, "app")
	for file, body := range map[string]string{
		"CLAUDE.md":                  "ROOT-ITSELF\n",
		"src/CLAUDE.md":              "NESTED-SRC\n@notes.md\n",
		"src/notes.md":               "NESTED-IMPORT\n",
		"src/deep/CLAUDE.md":         "NESTED-DEEP\n",
		"src/deep/b.txt":             "x\n",
		"other/CLAUDE.md":            "NESTED-OTHER\n",
		".claude/rules/always.md":    "RULE-ALWAYS\n",
		".claude/rules/src.md":       "---\npaths:\n  - \"src/**\"\n---\nRULE-SRC\n",
		".claude/rules/text.md":      "---\npaths: [\"**/*.txt\", \"lib/*.go\"]\n---\nRULE-TXT\n",
		".claude/rules/sub/docs.md":  "---\npaths: docs/**\n---\nRULE-DOCS\n",
		".claude/rules/one-level.md": "---\npaths:\n  - src/*.txt\n---\nRULE-ONE-LEVEL\n",
	} {
		plantFile(t, filepath.Join(root, file), body)
	}
	rr := p.ClaudeRepoPlugins()
	plantFile(t, rr+"/roots", root+"\n")
	if err = os.MkdirAll(rr+"/seen", 0o700); err != nil {
		t.Fatal(err)
	}
	hook := func(file string) string {
		t.Helper()
		in := `{"session_id":"s","tool_name":"Read","tool_input":{"file_path":"` + file + `"},` +
			`"tool_response":{"content":"\"file_path\":\"/elsewhere\""}}`
		cmd := exec.Command("sh", p.ClaudeNestedHook(), rr) //nolint:gosec // the case's own box
		cmd.Stdin = strings.NewReader(in)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("the hook failed on %s: %v", file, err)
		}
		if len(out) == 0 {
			return ""
		}
		var doc struct {
			Out struct {
				Event   string `json:"hookEventName"`
				Context string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(out, &doc); err != nil || doc.Out.Event != "PostToolUse" {
			t.Fatalf("the hook's answer is not a PostToolUse context (%v): %s", err, out)
		}
		return doc.Out.Context
	}
	got := hook(root + "/src/deep/b.txt")
	for _, word := range []string{"NESTED-SRC", "NESTED-IMPORT", "NESTED-DEEP", "RULE-SRC", "RULE-TXT"} {
		if strings.Count(got, word) != 1 {
			t.Errorf("the first read was handed %s %d times, want once:\n%s", word, strings.Count(got, word), got)
		}
	}
	for _, word := range []string{"ROOT-ITSELF", "NESTED-OTHER", "RULE-ALWAYS", "RULE-DOCS", "RULE-ONE-LEVEL"} {
		if strings.Contains(got, word) {
			t.Errorf("the first read was handed %s:\n%s", word, got)
		}
	}
	if !strings.Contains(got, "Contents of "+root+"/src/CLAUDE.md:\n\nNESTED-SRC") ||
		strings.Index(got, "NESTED-SRC") > strings.Index(got, "NESTED-DEEP") {
		t.Errorf("the files are not named and outermost first:\n%s", got)
	}
	if again := hook(root + "/src/deep/b.txt"); again != "" {
		t.Errorf("a second read was handed the same files again:\n%s", again)
	}
	if got := hook(root + "/src/a.txt"); !strings.Contains(got, "RULE-ONE-LEVEL") || strings.Contains(got, "NESTED-SRC") {
		t.Errorf("a read of src/a.txt was handed %q, want the one-level rule alone", got)
	}
	if outside := hook(box.Home() + "/elsewhere/CLAUDE.md"); outside != "" {
		t.Errorf("a read outside every root was handed:\n%s", outside)
	}
}
