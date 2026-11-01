package harness

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
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// THE SEAT'S IDENTITY REACHES A STRUCTURED PROMPT CHANNEL INTACT.
//
// A vendor whose per-call prompt channel is an agent DEFINITION rather than a
// bare prompt needs an envelope around the text, and the envelope has to be
// per-profile data rather than something this package invents — see
// [harnessprofile.SystemPromptFile]. Asked from inside the child, for the reason every other
// file probe here is: the per-call directory is removed on release, so a
// parent reading the path afterwards would be reading a file the harness never
// saw.
func TestASystemPromptFileTemplateWrapsTheSeatsIdentity(t *testing.T) {
	t.Parallel()
	p := fakeProvider(t, map[string]string{"FAKE_PROMPT_FILE": "--agent-file"}, map[string]any{
		"system_prompt": map[string]any{
			"args": []any{"--agent-file", "{file}"},
			"file": map[string]any{
				"name":     "crewlet-seat.md",
				"template": "---\nname: crewlet-seat\ndescription: a seat\ntools: []\n---\n{system}",
			},
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
// So a request carrying no system prompt must still get the channel, or the
// calls that never notice would be the ones that run with the vendor's
// defaults and every tool it has.
func TestThePromptFileChannelIsPassedEvenWithNoSystemPrompt(t *testing.T) {
	t.Parallel()
	p := fakeProvider(t, map[string]string{"FAKE_PROMPT_FILE": "--agent-file"}, map[string]any{
		"system_prompt": map[string]any{
			"args": []any{"--agent-file", "{file}"},
			"file": map[string]any{
				"name":     "crewlet-seat.md",
				"template": "---\ntools: []\n---\n{system}",
			},
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
			"so the harness ran with its own default agent: %q", got.Content)
	}
	argv, _, body := splitFileProbe(t, got.Content)
	if !strings.Contains(argv, "--agent-file") {
		t.Errorf("argv = %q, want the channel passed even with nothing to put in it", argv)
	}
	if !strings.Contains(body, "tools: []") {
		t.Errorf("the child read %q, which does not carry the profile's denial", body)
	}
}

// A CALL WITH NO SYSTEM PROMPT TAKES A SEAT'S ROUTE, WHATEVER ITS KIND.
//
// A side pass takes the replacing channel because its system prompt is the
// whole instruction. With none, the replacing form of a file that is passed
// for its policy renders an EMPTY system prompt — on Kimi Code an agent file
// with no body, which the harness refuses ("Missing prompt body", exit 1). The
// appending form keeps the vendor's own prompt under the same frontmatter,
// which is what a call with no system prompt asks for; a call that has one
// still takes the channel its kind wants.
func TestACallWithNoSystemPromptTakesASeatsRoute(t *testing.T) {
	t.Parallel()
	agentFile := func(body string) map[string]any {
		return map[string]any{
			"args": []any{"--agent-file", "{file}"},
			"file": map[string]any{"name": "crewlet-seat.md", "template": "---\ntools: []\n---\n" + body},
		}
	}
	p := fakeProvider(t, map[string]string{"FAKE_PROMPT_FILE": "--agent-file"}, map[string]any{
		"system_prompt":        agentFile("REPLACING {system}"),
		"append_system_prompt": agentFile("APPENDING {system}"),
	})
	for name, tc := range map[string]struct {
		agent  bool
		system string
		want   string // the body the harness reads
	}{
		"a side pass with a system prompt": {agent: false, system: "Classify.", want: "REPLACING Classify."},
		"a side pass with none":            {agent: false, want: "APPENDING "},
		"a seat with none":                 {agent: true, want: "APPENDING "},
	} {
		msgs := []llm.Message{{Role: llm.RoleUser, Content: "the build is red"}}
		if tc.system != "" {
			msgs = append([]llm.Message{{Role: llm.RoleSystem, Content: tc.system}}, msgs...)
		}
		got, err := ask(t, p, llm.Request{Agent: tc.agent, Messages: msgs})
		if err != nil {
			t.Fatalf("%s: Complete: %v", name, err)
		}
		_, _, file := splitFileProbe(t, got.Content)
		if body := strings.TrimPrefix(file, "---\ntools: []\n---\n"); body != tc.want {
			t.Errorf("%s: the harness read the body %q, want %q", name, body, tc.want)
		}
	}
}

// A profile that has no `{file}` channel has nothing to write the envelope
// to, so the template would silently configure nothing while the seat's
// identity went to argv bare.
func TestASystemPromptTemplateWithoutAFileChannelIsRefused(t *testing.T) {
	t.Parallel()
	for name, overrides := range map[string]map[string]any{
		"argv channel": {"system_prompt": map[string]any{
			"args": []any{"--system-prompt", "{system}"},
			"file": map[string]any{"template": "{system}"},
		}},
		"no channel at all": {"system_prompt": map[string]any{
			"args": []any{},
			"file": map[string]any{"template": "{system}"},
		}},
		"an appending argv channel": {"append_system_prompt": map[string]any{
			"args": []any{"--append-system-prompt", "{system}"},
			"file": map[string]any{"template": "{system}"},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			field := "system_prompt"
			if _, ok := overrides[field]; !ok {
				field = "append_system_prompt"
			}
			if _, err := harnessprofile.Load("claude-code", overrides); err == nil ||
				!strings.Contains(err.Error(), field+".file is set but the channel writes no file") {
				t.Fatalf("err = %v, want a refusal naming %s.file", err, field)
			}
		})
	}
}

// A template with no placeholder hands every seat on every phase the same
// fixed file and no identity at all — which looks exactly like a channel that
// works.
func TestASystemPromptTemplateWithoutThePlaceholderIsRefused(t *testing.T) {
	t.Parallel()
	_, err := harnessprofile.Load("claude-code", map[string]any{
		"system_prompt": map[string]any{"file": map[string]any{"template": "---\ntools: []\n---\nhello"}},
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
		_, err := harnessprofile.Load("claude-code", map[string]any{
			"system_prompt": map[string]any{"file": map[string]any{"name": name, "template": "{system}"}},
		})
		if err == nil || !strings.Contains(err.Error(), "system_prompt.file.name") {
			t.Errorf("name %q: err = %v, want a refusal naming the field", name, err)
		}
	}
}

// THE KIMI PROFILE'S AGENT FILE IS THE PROFILE'S TOOL DENIAL, on both of its
// channels, so each has to be a file that harness accepts: the vendor's reference
// says a file passed through `--agent-file` "must be valid, otherwise the harness
// reports the error and exits", and `description` is the one required field.
//
// The two channels are one file whose body differs. The replacing body is the
// engine's text alone, so a side pass gets nothing of the vendor's; the
// appending body is the engine's text and THEN `${base_prompt}`, the vendor's
// default — in that order because the default carries the call's directory,
// its listing and the AGENTS.md chain, and anything per call ahead of a seat's
// prompt makes every call's copy of it a cache miss.
//
// Stated once here so that an edit dropping the frontmatter — or the two web
// tools, or the allowlist that removes everything else, or the order — has to
// argue with this rather than with nothing.
func TestTheKimiProfilesAgentFilesDenyTheirToolsAndKeepTheWeb(t *testing.T) {
	t.Parallel()
	p, ok := harnessprofile.Builtin("kimi-code")
	if !ok {
		t.Fatal("no built-in kimi-code profile")
	}
	var fronts []string
	for _, tc := range []struct {
		field string
		ch    harnessprofile.Channel
		body  string
	}{
		{"system_prompt", p.SystemPrompt, "{system}"},
		{"append_system_prompt", p.AppendSystemPrompt, "{system}\n\n${base_prompt}"},
	} {
		spec := tc.ch.File
		if spec == nil {
			t.Errorf("kimi-code declares no %s.file, so --agent-file would be handed a "+
				"bare prompt and the harness would exit on it", tc.field)
			continue
		}
		if !slices.Equal(tc.ch.Args, []string{"--agent-file", "{file}"}) {
			t.Errorf("%s.args = %q, want the agent file by its path", tc.field, tc.ch.Args)
		}
		if got := spec.Name; filepath.Ext(got) != ".md" {
			t.Errorf("%s.file.name = %q — agent files are Markdown and discovery scans for .md",
				tc.field, got)
		}
		front, body, found := strings.Cut(
			strings.TrimPrefix(spec.Template, "---\n"), "---\n")
		if !found {
			t.Errorf("%s.file.template carries no YAML frontmatter:\n%s", tc.field, spec.Template)
			continue
		}
		fronts = append(fronts, front)
		// The harness trims the body before it reads it.
		if got := strings.TrimSpace(body); got != tc.body {
			t.Errorf("%s body = %q, want %q", tc.field, got, tc.body)
		}
		holdsKimiAllowlist(t, tc.field, front)
	}
	// One file, one policy: a seat and a side pass on this harness are offered
	// the same tools, whichever channel the call takes.
	if len(fronts) == 2 && fronts[0] != fronts[1] {
		t.Errorf("the two channels' frontmatter differ:\n%s\n---\n%s", fronts[0], fronts[1])
	}
	if p.SystemPrompt.File != nil && p.AppendSystemPrompt.File != nil &&
		p.SystemPrompt.File.Name != p.AppendSystemPrompt.File.Name {
		t.Errorf("the channels write %q and %q — one agent file per call, under one name",
			p.SystemPrompt.File.Name, p.AppendSystemPrompt.File.Name)
	}
}

// holdsKimiAllowlist checks one agent file's frontmatter keeps the two web
// tools and the crewlet server's, and nothing else.
func holdsKimiAllowlist(t *testing.T, field, front string) {
	t.Helper()
	var meta struct {
		Name            string   `yaml:"name"`
		Description     string   `yaml:"description"`
		Tools           []string `yaml:"tools"`
		DisallowedTools []string `yaml:"disallowedTools"`
	}
	if err := yaml.Unmarshal([]byte(front), &meta); err != nil {
		t.Errorf("%s: the frontmatter is not YAML the harness can read: %v\n%s", field, err, front)
		return
	}
	if meta.Description == "" {
		t.Errorf("%s: description is empty, and it is the one REQUIRED frontmatter field — "+
			"the harness reports the error and exits rather than running without it", field)
	}
	if meta.Name == "" || strings.ToLower(meta.Name) != meta.Name || strings.Contains(meta.Name, " ") {
		t.Errorf("%s: name = %q, want a kebab-case identifier: a missing or non-kebab-case "+
			"name is skipped with a warning", field, meta.Name)
	}
	// An ALLOWLIST, not a denylist: on this harness an entry it does not
	// recognise matches nothing and is reported with a warning, so a stale
	// name costs one tool here where a denylist would cost the restriction.
	if len(meta.Tools) == 0 {
		t.Errorf("%s: tools is empty or absent — omitting it allows EVERY tool", field)
	}
	if len(meta.DisallowedTools) > 0 {
		t.Errorf("%s: disallowedTools = %v: the allowlist is the denial here, and a "+
			"second list is one more thing to keep in step", field, meta.DisallowedTools)
	}
	// Web is the one local tool this backend never denies, and these are
	// what this harness calls its two. The seat's own tools are the crewlet
	// server's, which this harness offers its model as mcp__crewlet__<name> — and
	// only where the allowlist names them; a call with no such server (a
	// text function) matches the entry to nothing.
	tools := slices.Clone(meta.Tools)
	slices.Sort(tools)
	if want := []string{"FetchURL", "WebSearch", "mcp__crewlet__*"}; !slices.Equal(tools, want) {
		t.Errorf("%s: tools = %v, want exactly %v: a subscription seat must not have "+
			"less reach than the same harness at a terminal, and anything else reaches the "+
			"filesystem, the shell or another agent — a harness acting on its own is invisible "+
			"to the tool registry and the permission model", field, meta.Tools, want)
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
// alone: without a credential the harness stops at its own auth failure, which is
// exactly far enough to prove the flags were understood.
func TestTheKimiProfileArgvParsesAgainstTheRealHarness(t *testing.T) {
	t.Parallel()
	binary, err := exec.LookPath("kimi")
	if err != nil {
		t.Skip("no kimi on PATH")
	}
	p, ok := harnessprofile.Builtin("kimi-code")
	if !ok {
		t.Fatal("no built-in kimi-code profile")
	}
	// THE SAME FILTERED ENVIRONMENT AND DIRECTORY as the invocation below.
	// A bare exec.Command leaves cmd.Env nil, which hands the child this
	// process's WHOLE environment — so the step that decides whether the
	// binary on PATH is even the right program would give an unidentified
	// executable every key the environment carries, before any identity check
	// has run. Same repair as the grok case.
	probeDir := t.TempDir()
	probe := exec.CommandContext(t.Context(), binary, "--version")
	probe.Dir = probeDir
	probe.Env = vendorHarnessEnv(p, probeDir, nil)
	out, _ := probe.Output()
	version := strings.TrimSpace(string(out))
	if strings.HasPrefix(version, "1.") {
		// The unscoped `kimi-code` package on npm is a Claude Code
		// wrapper that also installs a `kimi`, and checking it answers
		// the wrong question. Its versions are 1.0.x; Moonshot's own went
		// from 0.43 to 2.0 without one.
		t.Skipf("this is not MoonshotAI's own kimi (%q) — the unscoped npm package "+
			"of the same name is a different program", version)
	}

	// Both channels, because each is an agent file the harness parses before
	// anything else: a frontmatter or a body it refused would stop it short
	// of the auth failure this asserts on.
	for field, ch := range map[string]harnessprofile.Channel{
		"system_prompt":        p.SystemPrompt,
		"append_system_prompt": p.AppendSystemPrompt,
	} {
		dir := t.TempDir()
		// A MODEL THE HARNESS KNOWS, ON A PROVIDER WITH NO CREDENTIAL. With no
		// config.toml at all, 0.42.0 and 2.3.1 both stop at "Model … is not
		// configured in config.toml" — before the credential, so the case
		// proved nothing. Declared with no key, the model takes them on to
		// "provider … has no credential configured", which is past every
		// flag and the agent file. The base URL is a closed local port: a
		// build that tried the network anyway would fail there, not reach
		// anyone.
		if err := os.MkdirAll(filepath.Join(dir, ".kimi-code"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".kimi-code", "config.toml"), []byte(
			"default_model = \"kimi-code/k3\"\n\n"+
				"[providers.probe]\ntype = \"kimi\"\nbase_url = \"http://127.0.0.1:9/v1\"\n\n"+
				"[models.\"kimi-code/k3\"]\nprovider = \"probe\"\nmodel = \"k3\"\nmax_context_size = 131072\n",
		), 0o600); err != nil {
			t.Fatal(err)
		}
		agentFile := filepath.Join(dir, ch.File.FileName())
		if err := os.WriteFile( //nolint:gosec // a test's own temp dir
			agentFile, []byte(ch.File.Render("You are Agent CTO.")), 0o600,
		); err != nil {
			t.Fatal(err)
		}

		args := append([]string(nil), p.CompleteArgs...)
		for _, a := range p.ModelArgs {
			args = append(args, strings.ReplaceAll(a, "{model}", "kimi-code/k3"))
		}
		for _, a := range ch.Args {
			args = append(args, strings.ReplaceAll(a, "{file}", agentFile))
		}
		args = append(args, p.PromptArgs...)
		args = append(args, "say hello")

		cmd := exec.Command(binary, args...)
		cmd.Dir = dir
		// No KIMI_CODE_HOME here: the profile declares it in config_env and
		// vendorHarnessEnv derives it, which is the point of deriving rather than
		// re-stating what profiles.yaml already says.
		cmd.Env = vendorHarnessEnv(p, dir, nil)
		combined, _ := cmd.CombinedOutput()
		t.Run(field, func(t *testing.T) { assertArgvReachedAuth(t, args, string(combined)) })
	}
}

// THE KIMI PROFILE READS ONE ANSWER OUT OF ITS MESSAGE STREAM.
//
// These are the BYTES THE REAL HARNESS EMITTED, captured from 0.42.0 driven
// against a stub HTTP endpoint standing in for the model — not a stream
// written to match the profile. Two of the three lines are the point.
//
// `session.resume_hint` carries its own `content` field, holding the harness's
// prose about how to resume. A profile reading `content` off every line would
// have appended "To resume this session: kimi -r session_…" to the model's
// reply, and the tool loop, the reviewer and the dashboard would all have
// read it as a sentence the agent said. `system.version` is the same hazard
// with no `content` to hit. The `role` discriminator is what keeps them out.
func TestTheKimiProfileReadsOnlyTheAssistantsLines(t *testing.T) {
	t.Parallel()
	p, ok := harnessprofile.Builtin("kimi-code")
	if !ok {
		t.Fatal("no built-in kimi-code profile")
	}
	const captured = `{"role":"meta","type":"system.version","version":"0.42.0"}
{"role":"assistant","content":"THE-STUB-REPLY"}
{"role":"meta","type":"session.resume_hint","session_id":"session_4e0d7ff8-5f9a-42be-ab76-cd7321b34e48","command":"kimi -r session_4e0d7ff8-5f9a-42be-ab76-cd7321b34e48","content":"To resume this session: kimi -r session_4e0d7ff8-5f9a-42be-ab76-cd7321b34e48"}`

	got := extract(p, captured)
	if !got.located {
		t.Fatalf("the profile found no answer in its own harness's real stream:\n%s", captured)
	}
	if got.text != "THE-STUB-REPLY" {
		t.Errorf("text = %q, want the assistant's own words alone — a `meta` line's "+
			"own `content` must not reach the conversation", got.text)
	}
}

// A SPENT PLAN REACHES THE FALLBACK CHAIN, and these are the real words.
//
// The classification prefix is the harness's own and survives the vendor
// rewording the sentence after it; both were captured from 0.42.0 answering a
// stubbed 429 and 401. A marker that stopped matching would leave a spent plan
// unrecognised: the failed run would still carry the seat to its metered key,
// as a `server` failure, but the entry would never sit out, so every call would
// launch the spent harness first to hear the same sentence again — silent until
// somebody reads why the plan's calls all fail.
func TestTheKimiProfileClassifiesTheFailuresItsHarnessActuallyPrints(t *testing.T) {
	t.Parallel()
	p, ok := harnessprofile.Builtin("kimi-code")
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
			hit, ok := classifyMarkers(p, harnessOutput{stderr: tc.stderr, failed: true})
			if !ok {
				t.Fatalf("no marker fired on what the harness really printed:\n%s", tc.stderr)
			}
			if hit.Kind != tc.want {
				t.Errorf("kind = %v, want %v", hit.Kind, tc.want)
			}
		})
	}
}

// splitFileProbe reads the fake harness's `argv|mode|base64(body)` report.
func splitFileProbe(t *testing.T, reported string) (argv, mode, body string) {
	t.Helper()
	argv, rest, ok := strings.Cut(reported, "|")
	if !ok {
		t.Fatalf("the fake harness did not report a readable file: %q", reported)
	}
	mode, encoded, ok := strings.Cut(rest, "|")
	if !ok {
		t.Fatalf("the fake harness did not report a readable file: %q", reported)
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
