package cliagent

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// THE SEAT'S IDENTITY REACHES A STRUCTURED PROMPT CHANNEL INTACT.
//
// A vendor whose per-call prompt channel is an agent DEFINITION rather than a
// bare prompt needs an envelope around the text, and the envelope has to be
// per-profile data rather than something this package invents — see
// [SystemPromptFile]. Asked from inside the child, for the reason every other
// file probe here is: the per-call directory is removed on release, so a
// parent reading the path afterwards would be reading a file the CLI never
// saw.
func TestASystemPromptFileTemplateWrapsTheSeatsIdentity(t *testing.T) {
	p := fakeProvider(t, map[string]string{"FAKE_PROMPT_FILE": "--agent-file"}, map[string]any{
		// The leading "--" is for the FAKE, not for any real profile:
		// the fake CLI is this test binary and Go's flag package would
		// refuse --agent-file as an unknown test flag.
		"system_prompt_args": []any{"--", "--agent-file", "{file}"},
		"system_prompt_file": map[string]any{
			"name":     "crewlet-seat.md",
			"template": "---\nname: crewlet-seat\ndescription: a seat\ntools: []\n---\n{system}",
		},
	})
	identity := "You are Agent CTO at Nimbus, and the roster below is yours."
	got, err := ask(t, p, llm.Request{Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: identity},
		{Role: llm.RoleUser, Content: "status?"},
	}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	argv, mode, body := splitFileProbe(t, got.Content)
	if !strings.Contains(argv, "--agent-file") {
		t.Errorf("argv = %q, want the flag the profile declared", argv)
	}
	if strings.Contains(argv, identity) {
		t.Errorf("the seat's identity is on argv, where /proc/<pid>/cmdline exposes it: %q", argv)
	}
	if mode != "600" {
		t.Errorf("the agent file's mode = %s, want 600", mode)
	}
	if !strings.HasPrefix(body, "---\nname: crewlet-seat\n") {
		t.Errorf("the child read %q, which is not the profile's envelope", body)
	}
	if !strings.HasSuffix(body, identity) {
		t.Errorf("the child read %q, which does not end in the seat's identity", body)
	}
	// The name is the profile's, not this package's default: a vendor that
	// keys on the extension gets nothing from `crewlet-system-prompt.txt`.
	if base := fileArgAfter(argv, "--agent-file"); filepath.Base(base) != "crewlet-seat.md" {
		t.Errorf("the file was written as %q, want the profile's own name", base)
	}
}

// A PROFILE WHOSE PROMPT FILE CARRIES POLICY PASSES IT ON EVERY CALL.
//
// The envelope is not only the seat's identity: on the one vendor that needs
// it, the same frontmatter is the only per-call place its tools can be denied.
// So a request carrying no system prompt — `crewlet llm doctor`'s two
// isolation probes are exactly that — must still get the channel, or the
// calls that exist to prove the tools are off would be the calls that run
// with the vendor's defaults and every tool it has.
func TestThePromptFileChannelIsPassedEvenWithNoSystemPrompt(t *testing.T) {
	p := fakeProvider(t, map[string]string{"FAKE_PROMPT_FILE": "--agent-file"}, map[string]any{
		"system_prompt_args": []any{"--", "--agent-file", "{file}"},
		"system_prompt_file": map[string]any{
			"name":     "crewlet-seat.md",
			"template": "---\ntools: []\n---\n{system}",
		},
	})
	got, err := ask(t, p, llm.Request{Messages: []llm.Message{
		{Role: llm.RoleUser, Content: shellProbePrompt},
	}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if strings.HasPrefix(got.Content, "UNREADABLE") {
		t.Fatalf("no agent file was written for a request with no system prompt, "+
			"so the CLI ran with its own default agent: %q", got.Content)
	}
	argv, _, body := splitFileProbe(t, got.Content)
	if !strings.Contains(argv, "--agent-file") {
		t.Errorf("argv = %q, want the channel passed even with nothing to put in it", argv)
	}
	if !strings.Contains(body, "tools: []") {
		t.Errorf("the child read %q, which does not carry the profile's denial", body)
	}
}

// A profile that has no `{file}` channel has nothing to write the envelope
// to, so the template would silently configure nothing while the seat's
// identity went to argv bare.
func TestASystemPromptTemplateWithoutAFileChannelIsRefused(t *testing.T) {
	t.Parallel()
	for name, overrides := range map[string]map[string]any{
		"argv channel": {
			"system_prompt_args": []any{"--system-prompt", "{system}"},
			"system_prompt_file": map[string]any{"template": "{system}"},
		},
		"no channel at all": {
			"system_prompt_args": []any{},
			"system_prompt_file": map[string]any{"template": "{system}"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := Load("claude-code", overrides); err == nil ||
				!strings.Contains(err.Error(), "system_prompt_file") {
				t.Fatalf("err = %v, want a refusal naming system_prompt_file", err)
			}
		})
	}
}

// A template with no placeholder hands every seat on every phase the same
// fixed file and no identity at all — which looks exactly like a channel that
// works.
func TestASystemPromptTemplateWithoutThePlaceholderIsRefused(t *testing.T) {
	t.Parallel()
	_, err := Load("claude-code", map[string]any{
		"system_prompt_file": map[string]any{"template": "---\ntools: []\n---\nhello"},
	})
	if err == nil || !strings.Contains(err.Error(), "{system}") {
		t.Fatalf("err = %v, want a refusal naming the missing {system}", err)
	}
}

// The name is joined onto the per-call working directory and the field is
// operator-overridable, so it may not climb out of it.
func TestASystemPromptFileNameMayNotBeAPath(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"../escape.md", "sub/dir.md", "..", "."} {
		_, err := Load("claude-code", map[string]any{
			"system_prompt_file": map[string]any{"name": name, "template": "{system}"},
		})
		if err == nil || !strings.Contains(err.Error(), "system_prompt_file.name") {
			t.Errorf("name %q: err = %v, want a refusal naming the field", name, err)
		}
	}
}

// THE KIMI PROFILE'S AGENT FILE IS THE PROFILE'S TOOL DENIAL, so it has to be
// a file that CLI accepts: the vendor's reference says a file passed through
// `--agent-file` "must be valid, otherwise the CLI reports the error and
// exits", and `description` is the one required field.
//
// Stated once here so that an edit dropping the frontmatter — or the two web
// tools, or the allowlist that removes everything else — has to argue with
// this rather than with nothing.
func TestTheKimiProfilesAgentFileDeniesItsToolsAndKeepsTheWeb(t *testing.T) {
	t.Parallel()
	p, ok := Builtin("kimi-code")
	if !ok {
		t.Fatal("no built-in kimi-code profile")
	}
	if p.SystemPromptFile == nil {
		t.Fatal("kimi-code declares no system_prompt_file, so --agent-file would " +
			"be handed a bare prompt and the CLI would exit on it")
	}
	if got := p.SystemPromptFile.Name; filepath.Ext(got) != ".md" {
		t.Errorf("name = %q — agent files are Markdown and discovery scans for .md", got)
	}
	front, body, found := strings.Cut(
		strings.TrimPrefix(p.SystemPromptFile.Template, "---\n"), "---\n")
	if !found {
		t.Fatalf("the template carries no YAML frontmatter:\n%s", p.SystemPromptFile.Template)
	}
	var meta struct {
		Name            string   `yaml:"name"`
		Description     string   `yaml:"description"`
		Tools           []string `yaml:"tools"`
		DisallowedTools []string `yaml:"disallowedTools"`
	}
	if err := yaml.Unmarshal([]byte(front), &meta); err != nil {
		t.Fatalf("the frontmatter is not YAML the CLI can read: %v\n%s", err, front)
	}
	if meta.Description == "" {
		t.Error("description is empty, and it is the one REQUIRED frontmatter field — " +
			"the CLI reports the error and exits rather than running without it")
	}
	if meta.Name == "" || strings.ToLower(meta.Name) != meta.Name || strings.Contains(meta.Name, " ") {
		t.Errorf("name = %q, want a kebab-case identifier: a missing or non-kebab-case "+
			"name is skipped with a warning", meta.Name)
	}
	// An ALLOWLIST, not a denylist: on this CLI an entry it does not
	// recognise matches nothing and is reported with a warning, so a stale
	// name costs one tool here where a denylist would cost the restriction.
	if len(meta.Tools) == 0 {
		t.Fatal("tools is empty or absent — omitting it allows EVERY tool")
	}
	if len(meta.DisallowedTools) > 0 {
		t.Errorf("disallowedTools = %v: the allowlist is the denial here, and a "+
			"second list is one more thing to keep in step", meta.DisallowedTools)
	}
	// Web is the one local tool this backend never denies, and these are
	// what this CLI calls its two.
	for _, want := range []string{"WebSearch", "FetchURL"} {
		if !slices.Contains(meta.Tools, want) {
			t.Errorf("tools = %v, want %s kept: a subscription seat must not have "+
				"less reach than the same CLI at a terminal", meta.Tools, want)
		}
	}
	// Everything that reaches the filesystem, the shell, the network by
	// another route, or another agent.
	for _, denied := range []string{
		"Bash", "Read", "Write", "Edit", "Grep", "Glob", "ReadMediaFile",
		"Agent", "AgentSwarm", "TodoList", "Skill", "TaskList", "TaskOutput",
		"CronCreate", "AskUserQuestion", "EnterPlanMode",
	} {
		if slices.Contains(meta.Tools, denied) {
			t.Errorf("tools allows %s — Crewlet's tools ride the prompt envelope, and "+
				"a CLI acting on its own is invisible to the tool registry, the "+
				"permission model and redaction", denied)
		}
	}
	// NO ${base_prompt}: a body carrying it wraps the vendor's default
	// prompt instead of replacing it, which is the coding-agent identity
	// every other profile here replaces for the same reason.
	if strings.Contains(body, "${base_prompt}") {
		t.Error("the body embeds ${base_prompt}, so the vendor's own agent identity " +
			"sits over the top of whatever seat is being served")
	}
	if strings.TrimSpace(body) != "{system}" {
		t.Errorf("the body is %q, want the seat's prompt and nothing else", body)
	}
}

// THE KIMI PROFILE'S ARGV MUST PARSE, and only the real binary can say so.
//
// Two things here cannot be checked any other way. `--prompt` takes the
// prompt as its VALUE, so it lives in prompt_args and a build that moved it
// into complete_args would make the next flag the prompt — the failure the
// grok profile already records. And `--output-format` is refused unless
// `--prompt` is present, so the two have to arrive together.
//
// Skipped unless a `kimi` is on PATH, and it asserts on ARGUMENT PARSING
// alone: without a credential the CLI stops at its own auth failure, which is
// exactly far enough to prove the flags were understood.
func TestTheKimiProfileArgvParsesAgainstTheRealCLI(t *testing.T) {
	binary, err := exec.LookPath("kimi")
	if err != nil {
		t.Skip("no kimi on PATH")
	}
	p, ok := Builtin("kimi-code")
	if !ok {
		t.Fatal("no built-in kimi-code profile")
	}
	out, _ := exec.Command(binary, "--version").Output()
	version := strings.TrimSpace(string(out))
	if strings.HasPrefix(version, "1.") {
		// The unscoped `kimi-code` package on npm is a Claude Code
		// wrapper that also installs a `kimi`, and checking it answers
		// the wrong question. Its versions are 1.0.x; Moonshot's own are
		// 0.x.
		t.Skipf("this is not MoonshotAI's own kimi (%q) — the unscoped npm package "+
			"of the same name is a different program", version)
	}

	dir := t.TempDir()
	agentFile := filepath.Join(dir, p.SystemPromptFile.fileName())
	if err := os.WriteFile( //nolint:gosec // a test's own temp dir
		agentFile, []byte(p.SystemPromptFile.render("You are Agent CTO.")), 0o600,
	); err != nil {
		t.Fatal(err)
	}

	args := append([]string(nil), p.CompleteArgs...)
	for _, a := range p.ModelArgs {
		args = append(args, strings.ReplaceAll(a, "{model}", "kimi-code/k3"))
	}
	for _, a := range p.SystemPromptArgs {
		args = append(args, strings.ReplaceAll(a, "{file}", agentFile))
	}
	args = append(args, p.PromptArgs...)
	args = append(args, "say hello")

	cmd := exec.Command(binary, args...)
	cmd.Dir = dir
	cmd.Env = vendorCLIEnv(dir, map[string]string{"KIMI_CODE_HOME": filepath.Join(dir, ".kimi-code")})
	combined, _ := cmd.CombinedOutput()
	assertArgvReachedAuth(t, args, string(combined))
}

// THE KIMI PROFILE READS ONE ANSWER OUT OF ITS MESSAGE STREAM.
//
// These are the BYTES THE REAL CLI EMITTED, captured from 0.42.0 driven
// against a stub HTTP endpoint standing in for the model — not a stream
// written to match the profile. Two of the three lines are the point.
//
// `session.resume_hint` carries its own `content` field, holding the CLI's
// prose about how to resume. A profile reading `content` off every line would
// have appended "To resume this session: kimi -r session_…" to the model's
// reply, and the tool loop, the reviewer and the dashboard would all have
// read it as a sentence the agent said. `system.version` is the same hazard
// with no `content` to hit. The `role` discriminator is what keeps them out.
func TestTheKimiProfileReadsOnlyTheAssistantsLines(t *testing.T) {
	t.Parallel()
	p, ok := Builtin("kimi-code")
	if !ok {
		t.Fatal("no built-in kimi-code profile")
	}
	const captured = `{"role":"meta","type":"system.version","version":"0.42.0"}
{"role":"assistant","content":"THE-STUB-REPLY"}
{"role":"meta","type":"session.resume_hint","session_id":"session_4e0d7ff8-5f9a-42be-ab76-cd7321b34e48","command":"kimi -r session_4e0d7ff8-5f9a-42be-ab76-cd7321b34e48","content":"To resume this session: kimi -r session_4e0d7ff8-5f9a-42be-ab76-cd7321b34e48"}`

	got := extract(p, captured)
	if !got.located {
		t.Fatalf("the profile found no answer in its own CLI's real stream:\n%s", captured)
	}
	if got.text != "THE-STUB-REPLY" {
		t.Errorf("text = %q, want the assistant's own words alone — a `meta` line's "+
			"own `content` must not reach the conversation", got.text)
	}
}

// A SPENT PLAN REACHES THE FALLBACK CHAIN, and these are the real words.
//
// The classification prefix is the CLI's own and survives the vendor
// rewording the sentence after it; both were captured from 0.42.0 answering a
// stubbed 429 and 401. A marker that stopped matching would classify a spent
// plan FATAL, and the seat would never fall through to its metered key —
// silent until somebody hits their cap.
func TestTheKimiProfileClassifiesTheFailuresItsCLIActuallyPrints(t *testing.T) {
	t.Parallel()
	p, ok := Builtin("kimi-code")
	if !ok {
		t.Fatal("no built-in kimi-code profile")
	}
	for name, tc := range map[string]struct {
		stderr string
		want   llm.ErrorKind
	}{
		"a spent plan": {
			stderr: "error: failed to run prompt: provider.rate_limit: 429 You have " +
				"reached your 5-hour usage limit. Please try again later.",
			want: llm.KindRateLimit,
		},
		"a dead login": {
			stderr: "error: failed to run prompt: provider.auth_error: 401 Invalid Authentication",
			want:   llm.KindAuth,
		},
		// THE MESSAGE REWORDED AND THE PREFIX KEPT, which is the whole
		// case for matching the classification rather than the prose:
		// everything after the colon belongs to the upstream provider
		// and changes with the plan, the endpoint and the vendor's
		// copy. Without these two entries the structural sentinels
		// could be deleted and every assertion above would still pass
		// on the plan wording alone.
		"a spent plan the vendor reworded": {
			stderr: "error: failed to run prompt: provider.rate_limit: 429 quota " +
				"exhausted for this billing period",
			want: llm.KindRateLimit,
		},
		"a dead login the vendor reworded": {
			stderr: "error: failed to run prompt: provider.auth_error: 401 the " +
				"credential presented is no longer accepted",
			want: llm.KindAuth,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hit, ok := classifyMarkers(p, "", tc.stderr)
			if !ok {
				t.Fatalf("no marker fired on what the CLI really printed:\n%s", tc.stderr)
			}
			if hit.Kind != tc.want {
				t.Errorf("kind = %v, want %v", hit.Kind, tc.want)
			}
		})
	}
}

// splitFileProbe reads the fake CLI's `argv|mode|base64(body)` report.
func splitFileProbe(t *testing.T, reported string) (argv, mode, body string) {
	t.Helper()
	argv, rest, ok := strings.Cut(reported, "|")
	if !ok {
		t.Fatalf("the fake CLI did not report a readable file: %q", reported)
	}
	mode, encoded, ok := strings.Cut(rest, "|")
	if !ok {
		t.Fatalf("the fake CLI did not report a readable file: %q", reported)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decoding what the child read: %v", err)
	}
	return argv, mode, string(raw)
}

// fileArgAfter returns the value the fake reported after flag, or "".
func fileArgAfter(argv, flag string) string {
	fields := strings.Fields(argv)
	for i, f := range fields {
		if f == flag && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}
