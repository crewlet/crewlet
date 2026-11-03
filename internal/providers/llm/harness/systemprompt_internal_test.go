package harness

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// THE CHILD ACTUALLY RECEIVES THE VARIABLE, POINTING AT A FILE THAT EXISTS.
//
// Everything else about this channel is a claim until the process sees it: the
// env is assembled from an allowlist plus the profile's own `config_env`, and
// a name that lost to one of those, or a path written after the child was
// spawned, would leave the harness reading its built-in prompt while the engine
// believed it had been replaced — silently, and on every turn of every seat.
func TestTheSystemPromptEnvVarReachesTheChildPointingAtTheText(t *testing.T) {
	t.Parallel()
	p := fakeProvider(t, map[string]string{
		"FAKE_READ_SYSTEM_PROMPT": "GEMINI_SYSTEM_MD",
	}, map[string]any{"system_prompt": map[string]any{"env": "GEMINI_SYSTEM_MD"}})
	comp, err := ask(t, p, llm.Request{Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: "You are Agent CTO at Nimbus."},
		{Role: llm.RoleUser, Content: "hello"},
	}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	path, body, ok := strings.Cut(comp.Content, "|")
	if !ok {
		t.Fatalf("the child could not read the prompt: %s", comp.Content)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("GEMINI_SYSTEM_MD=%q is relative — the harness resolves it against "+
			"its own working directory, which is not necessarily ours", path)
	}
	if body != "You are Agent CTO at Nimbus." {
		t.Errorf("the child read %q, not the seat's system prompt", body)
	}
}

// The file behind that variable is 0600 and holds exactly the prompt.
//
// Asserted here rather than in the child, because the child cannot see its own
// file's MODE any more usefully than this can — and 0600 is the half that
// matters off the happy path: the box is a directory on a machine other
// accounts share, and the text is the org chart, the policies and the seat's
// own memory.
func TestTheSystemPromptFileBehindTheEnvVarIsPrivate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pair, err := systemEnv(harnessprofile.Channel{Env: "GEMINI_SYSTEM_MD"}, "the seat's whole identity", dir)
	if err != nil {
		t.Fatal(err)
	}
	name, path, ok := strings.Cut(pair, "=")
	if !ok || name != "GEMINI_SYSTEM_MD" {
		t.Fatalf("pair = %q, want NAME=path", pair)
	}
	if filepath.Dir(path) != dir {
		t.Errorf("the prompt landed at %q, outside the per-call directory %q", path, dir)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "the seat's whole identity" {
		t.Errorf("file holds %q", body)
	}
}

// AND IT IS LIFTED OUT OF THE TRANSCRIPT, not sent twice.
//
// The whole point of the channel is that the system prompt stops arriving as
// USER content. A profile that set the variable and still rendered the system
// section into the prompt would pay for the file and change nothing.
func TestTheSystemPromptEnvVarTakesTheTextOutOfThePrompt(t *testing.T) {
	t.Parallel()
	p := fakeProvider(t, map[string]string{"FAKE_ECHO_STDIN": "1"}, map[string]any{
		"system_prompt": map[string]any{"env": "QWEN_SYSTEM_MD"},
	})
	comp, err := ask(t, p, llm.Request{Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: "SEAT-IDENTITY-SENTINEL"},
		{Role: llm.RoleUser, Content: "hello"},
	}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(comp.Content))
	if err != nil {
		t.Fatalf("decoding the echoed prompt: %v", err)
	}
	if strings.Contains(string(decoded), "SEAT-IDENTITY-SENTINEL") {
		t.Error("the system prompt is in the transcript AND in the file — the " +
			"model is being told its identity twice, once as an ordinary message")
	}
	if !strings.Contains(string(decoded), "hello") {
		t.Errorf("the user turn did not survive the split:\n%s", decoded)
	}
}

// A channel may not carry the prompt both ways. Refused at load, because which
// copy a harness honours when handed the same prompt twice is the vendor's business.
func TestAChannelCannotCarryThePromptOnArgvAndInAVariable(t *testing.T) {
	t.Parallel()
	_, err := harnessprofile.Load("custom", map[string]any{
		"binary":        "x",
		"complete_args": []any{"run"},
		"output":        "text",
		"system_prompt": map[string]any{
			"args": []any{"--system-prompt", "{system}"},
			"env":  "SOME_SYSTEM_MD",
		},
	})
	if err == nil {
		t.Fatal("a channel carrying the prompt both ways loaded")
	}
	if !strings.Contains(err.Error(), "ONE channel") {
		t.Errorf("the error does not say what is wrong: %v", err)
	}
}

// A DISCOVERED AGENT FILE IS WHERE THE HARNESS LOOKS, PRIVATE, AND SELECTED — never
// carried — by the argv.
//
// OpenCode 2.0 has no system-prompt flag: it finds `agents/<name>.md` in its
// configuration directory, which its profile points inside the per-call working
// directory, and `--agent` picks one. So the file has to be in the per-call
// working directory at exactly that path when the child starts — which only the
// child can confirm, since the directory is removed on release — and nothing
// of the text may reach argv.
func TestADiscoveredAgentFileIsWhereTheHarnessLooks(t *testing.T) {
	t.Parallel()
	const name = ".crewlet-config/agents/crewlet-seat.md"
	p := fakeProvider(t, map[string]string{"FAKE_READ_DISCOVERED": name}, map[string]any{
		"system_prompt": map[string]any{
			"args": []any{"--agent", "crewlet-seat"},
			"file": map[string]any{
				"name": name, "discovered": "selected",
				"template": "---\nmode: primary\n---\n{system}\n",
			},
		},
	})
	comp, err := ask(t, p, llm.Request{Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: "You are Agent CTO at Nimbus."},
		{Role: llm.RoleUser, Content: "hello"},
	}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	parts := strings.SplitN(comp.Content, "|", 3)
	if len(parts) != 3 {
		t.Fatalf("the child could not read the agent file: %s", comp.Content)
	}
	argv, mode, encoded := parts[0], parts[1], parts[2]
	body, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "---\nmode: primary\n---\nYou are Agent CTO at Nimbus.\n" {
		t.Errorf("the agent file holds %q", body)
	}
	if mode != "600" {
		t.Errorf("mode = %s, want 600: the file is the org chart and the seat's memory", mode)
	}
	if !strings.Contains(argv, "--agent crewlet-seat") {
		t.Errorf("argv = %q, want the agent selected", argv)
	}
	if strings.Contains(argv, "Nimbus") || strings.Contains(argv, name) {
		t.Errorf("argv = %q carries the prompt or its path — a discovered file is "+
			"found by where it is", argv)
	}
}

// bothChannelsProvider is a fake harness declaring each system-prompt channel as a
// flag of its own, so a call's argv says which channel it took and what it put
// there. `{system}` rather than `{file}` only so the text can be read back off
// the record: the carrier is not what these cases are about.
func bothChannelsProvider(t *testing.T, env map[string]string) *Provider {
	t.Helper()
	return fakeProvider(t, env, map[string]any{
		"system_prompt":        map[string]any{"args": []any{"--replace-system-prompt", "{system}"}},
		"append_system_prompt": map[string]any{"args": []any{"--append-system-prompt", "{system}"}},
	})
}

// completionArgv is the one call a case made, as [fakeRecordArgv] recorded it.
func completionArgv(t *testing.T, dir string) []string {
	t.Helper()
	calls := recordedArgv(t, dir)
	if len(calls) != 1 {
		t.Fatalf("recorded %d invocations, want the one call: %q", len(calls), calls)
	}
	return calls[0]
}

// valueAfter returns the argument after flag in argv, and whether there is one.
func valueAfter(argv []string, flag string) (string, bool) {
	for i, arg := range argv {
		if arg == flag && i+1 < len(argv) {
			return argv[i+1], true
		}
	}
	return "", false
}

// A SEAT AT WORK ADDS TO THE HARNESS'S PROMPT AND A TEXT FUNCTION REPLACES IT, on a
// harness that offers both. The vendor's harness prompt is written for an agent,
// so a seat's call keeps it and adds its own after it, under the note saying
// which of the two is the role; a filter, a classifier or a judge has a system
// prompt that is its whole instruction, so its call replaces the vendor's and
// carries the text exactly — no note, which would be a harness's framing
// in front of an answer that is parsed. Never both channels on one call: the
// harness would be handed the prompt twice.
func TestASeatAddsToTheHarnessPromptAndATextFunctionReplacesIt(t *testing.T) {
	t.Parallel()
	const identity = "You are Agent CTO at Nimbus."
	for name, tc := range map[string]struct {
		agent      bool
		take, skip string
		want       string
	}{
		"a seat at work": {
			agent: true, take: "--append-system-prompt", skip: "--replace-system-prompt",
			want: harnessNote + "\n\n" + identity,
		},
		"a text function": {
			agent: false, take: "--replace-system-prompt", skip: "--append-system-prompt",
			want: identity,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := t.TempDir()
			p := bothChannelsProvider(t, map[string]string{"FAKE_STDOUT": "ok", "FAKE_RECORD_ARGV": rec})
			if _, err := ask(t, p, llm.Request{Agent: tc.agent, Messages: []llm.Message{
				{Role: llm.RoleSystem, Content: identity},
				{Role: llm.RoleUser, Content: "status?"},
			}}); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			argv := completionArgv(t, rec)
			got, ok := valueAfter(argv, tc.take)
			if !ok {
				t.Fatalf("argv = %q, want the system prompt on %s", argv, tc.take)
			}
			if got != tc.want {
				t.Errorf("%s carried %q, want %q", tc.take, got, tc.want)
			}
			if slices.Contains(argv, tc.skip) {
				t.Errorf("argv = %q also carries %s — the harness is handed the prompt twice", argv, tc.skip)
			}
		})
	}
}

// A FILE THE HARNESS READS ON EVERY RUN IS WRITTEN, AND ASKS NOTHING OF ARGV.
// OpenCode reads the AGENTS.md in its configuration directory without being
// told to, so a channel made of that file alone has to have it in place, private
// and holding the text, when the child starts — and argv gains nothing, since a
// flag there would be one the harness never declared. Asked from inside the child,
// because the per-call directory is removed on release.
func TestAFileTheHarnessAlwaysReadsIsWrittenAndAsksNothingOfArgv(t *testing.T) {
	t.Parallel()
	const name = ".crewlet-config/AGENTS.md"
	const identity = "You are Agent CTO at Nimbus."
	p := fakeProvider(t, map[string]string{"FAKE_READ_DISCOVERED": name}, map[string]any{
		"append_system_prompt": map[string]any{
			"file": map[string]any{"name": name, "discovered": "always"},
		},
	})
	comp, err := ask(t, p, llm.Request{Agent: true, Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: identity},
		{Role: llm.RoleUser, Content: "hello"},
	}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	parts := strings.SplitN(comp.Content, "|", 3)
	if len(parts) != 3 {
		t.Fatalf("the child could not read the file: %s", comp.Content)
	}
	argv, mode, encoded := parts[0], parts[1], parts[2]
	body, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if want := harnessNote + "\n\n" + identity; string(body) != want {
		t.Errorf("the file holds %q, want %q", body, want)
	}
	if mode != "600" {
		t.Errorf("mode = %s, want 600: the file is the org chart and the seat's memory", mode)
	}
	if want := "-test.run=TestFakeHarness"; argv != want {
		t.Errorf("argv = %q, want the profile's own %q and nothing for the channel", argv, want)
	}
}

// THE HARNESS NOTE OPENS A TRANSCRIPT'S ONE SYSTEM SECTION. With no channel the
// engine's text rides the transcript under the harness's own prompt, which says
// what the model is and says it first — so the note is needed on every call
// there, a text function's included, and it goes INTO the system message
// rather than beside it, keeping one `## system` section. The sentence about
// tools is there exactly when tools are offered: it points the model at
// Crewlet's tools, and a call offering none has none to point at.
func TestTheHarnessNoteOpensATranscriptsSystemSection(t *testing.T) {
	t.Parallel()
	const identity = "You are Agent PM at Nimbus."
	for name, tc := range map[string]struct {
		agent, tools bool
	}{
		"a seat offering tools":  {agent: true, tools: true},
		"a seat offering none":   {agent: true},
		"a text function":        {agent: false},
		"a text function, tools": {agent: false, tools: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req := llm.Request{Agent: tc.agent, Messages: []llm.Message{
				{Role: llm.RoleSystem, Content: identity},
				{Role: llm.RoleUser, Content: "status?"},
			}}
			if tc.tools {
				// A call offering tools renders a fenced transcript of its
				// own (see prompt_test.go), so the note is asked of the
				// system message it is written into.
				req.Tools = newTestHost(t)
				got := withHarnessNote(req).Messages[0].Content
				if want := harnessNote + harnessToolsNote + "\n\n" + identity; got != want {
					t.Errorf("the system message is %q, want the note, the tools sentence and "+
						"then the seat's prompt", got)
				}
				return
			}
			p := fakeProvider(t, map[string]string{"FAKE_ECHO_STDIN": "1"}, nil)
			comp, err := ask(t, p, req)
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			prompt := decodePrompt(t, comp.Content)
			if n := strings.Count(prompt, "## system"); n != 1 {
				t.Errorf("the transcript has %d system sections, want one:\n%s", n, prompt)
			}
			if want := "## system\n\n" + harnessNote + "\n\n" + identity + "\n"; !strings.HasPrefix(prompt, want) {
				t.Errorf("the transcript does not open with the note and then the seat's "+
					"prompt, in one section:\n%s", prompt)
			}
			if strings.Contains(prompt, harnessToolsNote) {
				t.Errorf("a call offering no tools is told to act through Crewlet's tools:\n%s", prompt)
			}
		})
	}
}

// NO SYSTEM PROMPT, NO NOTE. The note introduces the instructions that follow
// it, and a request carrying none has nothing for it to introduce: on the
// transcript it would be a system section of its own, and on the appending
// channel a flag handed text the request never had.
func TestARequestWithNoSystemPromptGetsNoHarnessNote(t *testing.T) {
	t.Parallel()
	user := []llm.Message{{Role: llm.RoleUser, Content: "status?"}}
	t.Run("in the transcript", func(t *testing.T) {
		t.Parallel()
		p := fakeProvider(t, map[string]string{"FAKE_ECHO_STDIN": "1"}, nil)
		comp, err := ask(t, p, llm.Request{Agent: true, Messages: user})
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if prompt := decodePrompt(t, comp.Content); strings.Contains(prompt, harnessNote) ||
			strings.Contains(prompt, "## system") {
			t.Errorf("a request with no system prompt was given one:\n%s", prompt)
		}
	})
	t.Run("on the appending channel", func(t *testing.T) {
		t.Parallel()
		rec := t.TempDir()
		p := bothChannelsProvider(t, map[string]string{"FAKE_STDOUT": "ok", "FAKE_RECORD_ARGV": rec})
		if _, err := ask(t, p, llm.Request{Agent: true, Messages: user}); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if argv := completionArgv(t, rec); slices.Contains(argv, "--append-system-prompt") {
			t.Errorf("argv = %q hands the harness a system prompt the request never had", argv)
		}
	})
}

// THE NOTE IS THE SAME BYTES ON EVERY CALL. It is the first thing in the
// engine's text, so it is the front of the prefix a vendor caches: the same
// request twice must carry the same text — the note added to a copy, never to
// the caller's messages, which the tool loop sends again every round — and two
// seats' prompts must share it to the byte.
func TestTheHarnessNoteIsTheSameBytesOnEveryCall(t *testing.T) {
	t.Parallel()
	const cto, pm = "You are Agent CTO at Nimbus.", "You are Agent PM at Nimbus."
	rec := t.TempDir()
	p := bothChannelsProvider(t, map[string]string{"FAKE_STDOUT": "ok", "FAKE_RECORD_ARGV": rec})
	req := llm.Request{Agent: true, Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: cto},
		{Role: llm.RoleUser, Content: "status?"},
	}}
	for range 2 {
		if _, err := ask(t, p, req); err != nil {
			t.Fatalf("Complete: %v", err)
		}
	}
	if req.Messages[0].Content != cto {
		t.Fatalf("the caller's system message became %q — the next round would carry "+
			"the note twice", req.Messages[0].Content)
	}
	if _, err := ask(t, p, llm.Request{Agent: true, Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: pm},
		{Role: llm.RoleUser, Content: "status?"},
	}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	prefixes := map[string]int{}
	seen := map[string]int{}
	for _, argv := range recordedArgv(t, rec) {
		text, ok := valueAfter(argv, "--append-system-prompt")
		if !ok {
			t.Fatalf("argv = %q, want the appending channel", argv)
		}
		seen[text]++
		prefix, found := strings.CutSuffix(text, cto)
		if !found {
			prefix, found = strings.CutSuffix(text, pm)
		}
		if !found {
			t.Fatalf("the channel carried %q, which ends in neither seat's prompt", text)
		}
		prefixes[prefix]++
	}
	if len(seen) != 2 {
		t.Errorf("three calls from two requests carried %d distinct texts, want 2: %v", len(seen), seen)
	}
	if want := harnessNote + "\n\n"; len(prefixes) != 1 || prefixes[want] != 3 {
		t.Errorf("the text in front of the seats' prompts = %v, want %q on all three calls", prefixes, want)
	}
}

// A CALL CHARGED ON AN ESTIMATE IS CHARGED FOR ITS SYSTEM PROMPT. A harness that
// reports no usage is billed by the characters the engine handed it, and the
// system prompt on its own channel is handed over as surely as the transcript:
// a budget counting the transcript alone undercounts every call by the largest
// stable thing in it — a seat's org chart, policies and memory.
func TestAnEstimatedCallIsChargedForItsSystemPrompt(t *testing.T) {
	t.Parallel()
	p := fakeProvider(t, map[string]string{"FAKE_STDOUT": "ok"}, map[string]any{
		"system_prompt": map[string]any{"args": []any{"--system-prompt-file", "{file}"}},
	})
	if p.profile.ReadsUsage() {
		t.Fatal("the fake reports usage, so nothing here is estimated")
	}
	req := llm.Request{Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: strings.Repeat("The roster and the policies. ", 200)},
		{Role: llm.RoleUser, Content: "status?"},
	}}
	comp, err := ask(t, p, req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	system, rest := SplitSystem(req)
	prompt, err := RenderPrompt(rest)
	if err != nil {
		t.Fatal(err)
	}
	if want := EstimateTokens(system + prompt); comp.InputTokens != want {
		t.Errorf("InputTokens = %d, want %d for the system prompt and the transcript "+
			"(the transcript alone is %d)", comp.InputTokens, want, EstimateTokens(prompt))
	}
}

// EVERY CALL IS ITS OWN PROJECT ROOT, before the harness starts. Vendor harnesses walk
// from their working directory up to the nearest repository root for project
// files (AGENTS.md, rules, skills, hooks), so a state_dir inside a checkout
// would hand every seat a repository's instructions. The engine writes a `.git`
// file naming a gitdir that never exists, which every measured harness stops at.
// Asked from inside the child, because the per-call directory is removed on
// release.
func TestEveryCallsDirectoryIsItsOwnProjectRoot(t *testing.T) {
	t.Parallel()
	p := fakeProvider(t, map[string]string{"FAKE_READ_DISCOVERED": harnessprofile.ProjectRootMarker}, nil)
	comp, err := ask(t, p, llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	parts := strings.SplitN(comp.Content, "|", 3)
	if len(parts) != 3 {
		t.Fatalf("the child found no %s in its working directory: %s", harnessprofile.ProjectRootMarker, comp.Content)
	}
	mode, encoded := parts[1], parts[2]
	body, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != harnessprofile.ProjectRootGitdir {
		t.Errorf("%s holds %q, want %q", harnessprofile.ProjectRootMarker, body, harnessprofile.ProjectRootGitdir)
	}
	if !strings.HasPrefix(string(body), "gitdir:") {
		t.Errorf("%s does not open with gitdir:, which Kimi Code needs to stop at it: %q",
			harnessprofile.ProjectRootMarker, body)
	}
	if mode != "600" {
		t.Errorf("%s is mode %s, want 600", harnessprofile.ProjectRootMarker, mode)
	}
}
