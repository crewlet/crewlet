package harness

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// The invariants the built-in qwen-code profile keeps, each measured against
// Qwen Code 0.28.3 (see the profile's comments): a call offers the
// web tools and nothing else, a seat's phases keep the harness's own system prompt
// and a side pass replaces it, the answer and its token counts are read from
// the result event, a key or the paid login signs the harness in, nothing above the call's
// directory configures it, and nothing a call leaves in the seat's home
// reaches the next one.

// qwenCode is the shipped profile, which every case here reads.
func qwenCode(t *testing.T) harnessprofile.Profile {
	t.Helper()
	p, ok := harnessprofile.Builtin("qwen-code")
	if !ok {
		t.Fatal("no built-in qwen-code profile")
	}
	return p
}

// qwenSettings is the settings file the profile seeds in the seat's home,
// decoded.
func qwenSettings(t *testing.T) map[string]any {
	t.Helper()
	for _, f := range qwenCode(t).SeedFiles {
		if f.Path == ".qwen/settings.json" && f.Scope() == harnessprofile.SeedHome {
			var doc map[string]any
			if err := json.Unmarshal([]byte(f.Content), &doc); err != nil {
				t.Fatalf(".qwen/settings.json is not JSON: %v", err)
			}
			return doc
		}
	}
	t.Fatal("the qwen-code profile seeds no .qwen/settings.json in the seat's home")
	return nil
}

// qwenSetting walks a dotted path into the seeded settings.
func qwenSetting(t *testing.T, doc map[string]any, path string) (any, bool) {
	t.Helper()
	var node any = doc
	for step := range strings.SplitSeq(path, ".") {
		m, ok := node.(map[string]any)
		if !ok {
			return nil, false
		}
		if node, ok = m[step]; !ok {
			return nil, false
		}
	}
	return node, true
}

// qwenStrings reads a settings list as strings, sorted.
func qwenStrings(t *testing.T, doc map[string]any, path string) []string {
	t.Helper()
	v, ok := qwenSetting(t, doc, path)
	if !ok {
		t.Fatalf("the seeded settings carry no %s", path)
	}
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("%s = %v, want a list", path, v)
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("%s holds %v, want names", path, item)
		}
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

// qwenWebTools are the harness's own web tools, the only ones a call keeps.
var qwenWebTools = []string{"web_fetch", "web_search"}

// qwenMeasuredTools is every tool name Qwen Code 0.28.3 registers (its
// ToolNames table), the web tools included.
var qwenMeasuredTools = []string{
	"advisor", "agent", "artifact", "ask_user_question", "autofix_pr", "create_sub_session",
	"cron_create", "cron_delete", "cron_list", "display_image", "edit",
	"enter_plan_mode", "enter_worktree", "exec", "exit_plan_mode", "exit_worktree",
	"get_goal", "glob", "grep_search", "image_gen", "list_agents", "list_directory",
	"loop_wakeup", "lsp", "manage_memory", "monitor", "notebook_edit",
	"omni_caption_audio", "omni_caption_image", "omni_clip_audio", "omni_clip_image",
	"omni_clip_video", "omni_convert_image", "omni_downsample_audio",
	"omni_downsample_image", "omni_downscale_video", "omni_extract_audio",
	"omni_extract_keyframes", "omni_ocr_image", "omni_recall_media_memory",
	"omni_transcribe_audio", "omni_understand_video_segments", "propose_goal",
	"read_file", "read_mcp_resource", "record_artifact", "record_source",
	"report_findings", "request_shutdown", "run_shell_command", "save_memory",
	"search_memory", "send_message", "skill", "structured_output", "task_create",
	"task_list", "task_stop", "task_update", "team_create", "team_delete",
	"team_plan_approval", "todo_write", "tool_call", "tool_search", "update_goal",
	"web_fetch", "web_search", "workflow", "write_file", "zoom_image",
}

// A CALL OFFERS THE WEB TOOLS AND NOTHING ELSE. `tools.core` is the allowlist,
// and it gates only the tools Qwen Code calls core: its agent, skill, worktree,
// goal, messaging and tool-search tools bypass it, and measured, the profile
// that only excluded the file and shell tools left them on offer — an
// enter_worktree the model asked for ran without a prompt and made a worktree
// in the repository above the state directory. So every other tool the
// measured builds register is also denied by name, which is what removes a
// tool outright, and the web tools are allowed, because a headless run cannot
// answer a confirmation and a fetch that waited for one would fail.
func TestQwenCodeOffersTheWebToolsAndNothingElse(t *testing.T) {
	t.Parallel()
	doc := qwenSettings(t)
	if core := qwenStrings(t, doc, "tools.core"); !slices.Equal(core, qwenWebTools) {
		t.Errorf("tools.core = %q, want exactly %q", core, qwenWebTools)
	}
	if allow := qwenStrings(t, doc, "permissions.allow"); !slices.Equal(allow, qwenWebTools) {
		t.Errorf("permissions.allow = %q, want exactly %q", allow, qwenWebTools)
	}
	deny := qwenStrings(t, doc, "permissions.deny")
	for _, name := range qwenMeasuredTools {
		web := slices.Contains(qwenWebTools, name)
		if denied := slices.Contains(deny, name); denied == web {
			t.Errorf("permissions.deny holds %s = %v, want %v", name, denied, !web)
		}
	}
}

// NOTHING IS CARRIED FROM ONE CALL TO THE NEXT. Qwen Code records a call's transcript
// under `.qwen/projects/<slug>/chats` and extracts memories, in requests of its own, into
// `.qwen/memories` and `.qwen/projects/*/memory`. The switches stop each at the source (no
// recording, extraction, consolidation or skill review; measured on 0.28.3, no projects
// directory and no extra request) and the volatile paths prune what a build ignoring a
// switch leaves, with the per-project scratch, the usage record 0.28.3 writes per call,
// and the global QWEN.md, AGENTS.md and rules it reads into the system prompt.
//
// The request-log directory is pinned in the seat's home for the same reason: the
// harness keys a housekeeping marker in the home by that directory, which otherwise lies
// under each call's own working directory, so the home gained a marker per call.
func TestQwenCodeKeepsNothingFromOneCallForTheNext(t *testing.T) {
	t.Parallel()
	doc := qwenSettings(t)
	for _, path := range []string{
		"general.chatRecording",
		"memory.enableManagedAutoMemory",
		"memory.enableManagedAutoDream",
		"memory.enableAutoSkill",
	} {
		if v, ok := qwenSetting(t, doc, path); !ok || v != false {
			t.Errorf("%s = %v (set %v), want false", path, v, ok)
		}
	}
	if dir, _ := qwenSetting(t, doc, "model.openAILoggingDir"); dir != "~/.qwen/openai-logs" {
		t.Errorf("model.openAILoggingDir = %v, want ~/.qwen/openai-logs, one directory "+
			"for every call of the seat and pruned with the rest", dir)
	}

	p, err := New(Config{
		Key: "qwen", Harness: "qwen-code", StateDir: t.TempDir(),
		Timeout: time.Second, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { forgetWorkspace(p.ws.StateDir()) })
	left := []string{
		".qwen/projects/-work-call/chats/session.jsonl",
		".qwen/projects/-work-call/memory/MEMORY.md",
		".qwen/memories/MEMORY.md",
		".qwen/usage_record.jsonl",
		".qwen/QWEN.md",
		".qwen/AGENTS.md",
		".qwen/rules/house.md",
		".qwen/tmp/0f3c/scheduled_tasks.json",
		".qwen/openai-logs/openai-2026-10-25.json",
	}
	co, err := p.ws.Acquire(t.Context(), "pm", "call-1", p)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	for _, rel := range left {
		writeFile(t, filepath.Join(co.Home, rel), "from the first call")
	}
	if err := co.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	next, err := p.ws.Acquire(t.Context(), "pm", "call-2", p)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { _ = next.Release(t.Context()) })
	for _, rel := range left {
		if _, err := os.Stat(filepath.Join(next.Home, rel)); !os.IsNotExist(err) {
			t.Errorf("%s survived into the next call (stat: %v)", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(next.Home, ".qwen", "settings.json")); err != nil {
		t.Errorf("the next call's home has no seeded settings: %v", err)
	}
}

// NOTHING ABOVE THE CALL'S DIRECTORY CONFIGURES IT, AND NOTHING BUT THE CALL LEAVES THE
// MACHINE. Qwen Code reads `.qwen/.env`, else `.env`, from the first directory that has
// one on the walk from its working directory to the root (the engine's `.git` file does
// not stop it) and applies every variable the environment leaves unset: measured on both
// builds, an `.env` between a checkout and the state directory set QWEN_SYSTEM_MD for a
// seat's call, replacing its system prompt with a file of the checkout's. A `.qwen/.env`
// in the call's own directory is the first the walk meets.
//
// The other switches keep the harness to the one connection the call is for (usage
// statistics, update checks and the model catalogue each opened their own) and stop it
// starting a process that holds the host awake, reading hooks or listing skills.
func TestQwenCodeIsConfiguredByTheCallAlone(t *testing.T) {
	t.Parallel()
	doc := qwenSettings(t)
	for path, want := range map[string]any{
		"privacy.usageStatisticsEnabled":    false,
		"general.enableAutoUpdate":          false,
		"general.preventSystemSleep":        false,
		"disableAllHooks":                   true,
		"model.generationConfig.maxRetries": float64(0),
	} {
		if v, ok := qwenSetting(t, doc, path); !ok || v != want {
			t.Errorf("%s = %v (set %v), want %v", path, v, ok, want)
		}
	}
	levels := qwenStrings(t, doc, "skills.disabledLevels")
	if want := []string{"bundled", "extension", "project", "user"}; !slices.Equal(levels, want) {
		t.Errorf("skills.disabledLevels = %q, want %q", levels, want)
	}

	env := childEnv(qwenCode(t), &Checkout{Home: t.TempDir(), Cache: t.TempDir(), Work: t.TempDir()}, nil, Auth{})
	if got := env["QWEN_CODE_MODELS_DEV"]; got != "off" {
		t.Errorf("QWEN_CODE_MODELS_DEV = %q, want off: 0.28.3 refreshes the models.dev catalogue otherwise", got)
	}

	p, err := New(Config{
		Key: "qwen", Harness: "qwen-code", StateDir: t.TempDir(),
		Timeout: time.Second, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { forgetWorkspace(p.ws.StateDir()) })
	co, err := p.ws.Acquire(t.Context(), "pm", "call-1", p)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { _ = co.Release(t.Context()) })
	info, err := os.Lstat(filepath.Join(co.Work, ".qwen", ".env"))
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("the call's directory holds no .qwen/.env file (%v), so the walk "+
			"for one goes on above it", err)
	}
}

// qwenPlainOutput reads the fake harness's stdout as the answer, for a case whose
// fake reports what it was handed rather than printing the harness's events — and
// so drops what reads those events: the request record, the failure events and
// the limit events, which a text stream does not carry.
var qwenPlainOutput = map[string]any{"output": "text", "event_type_path": []any{}, "text_events": []any{},
	"requests": nil, "mcp": map[string]any{"failure_events": []any{}}, "limit_events": []any{}}

// qwenProvider is the shipped profile with the suite's fake harness in its place.
func qwenProvider(t *testing.T, env map[string]string, overrides map[string]any, auth Auth) *Provider {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	o := map[string]any{"binary": os.Args[0]}
	for k, v := range overrides {
		o[k] = v
	}
	p, err := New(Config{
		Key: "qwen", Model: "qwen3-coder-plus", Harness: "qwen-code", StateDir: dir,
		Overrides: o, Timeout: 20 * time.Second, MaxConcurrent: 1,
		Env: fakeChildEnv(env), Auth: auth,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// A SEAT KEEPS THE HARNESS'S OWN PROMPT AND A SIDE PASS REPLACES IT, each through a
// file a variable names. QWEN_SYSTEM_IDENTITY_MD swaps the default's opening
// identity sentence for the file and keeps the rest of it byte for byte;
// QWEN_SYSTEM_MD replaces the whole default. A call sets one and never both:
// with QWEN_SYSTEM_MD set, the harness ignores the identity file. The seat's text
// opens with the note that says which prompt is the role, and the side pass's
// is exactly its instruction. Driven through Complete, so what is checked is
// the environment a call is handed.
func TestQwenCodeSeatAppendsAndASidePassReplaces(t *testing.T) {
	t.Parallel()
	const identity = "You are Agent PM at Nimbus."
	for name, tc := range map[string]struct {
		agent      bool
		take, skip string
		want       string
	}{
		"a seat at work": {agent: true, take: "QWEN_SYSTEM_IDENTITY_MD", skip: "QWEN_SYSTEM_MD",
			want: harnessNote + "\n\n" + identity},
		"a side pass": {agent: false, take: "QWEN_SYSTEM_MD", skip: "QWEN_SYSTEM_IDENTITY_MD",
			want: identity},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req := llm.Request{Agent: tc.agent, Messages: []llm.Message{
				{Role: llm.RoleSystem, Content: identity},
				{Role: llm.RoleUser, Content: "status?"},
			}}
			// The fake prints the file the variable names, from inside the
			// child: the call's directory is gone once the call returns.
			read := qwenProvider(t, map[string]string{"FAKE_READ_SYSTEM_PROMPT": tc.take},
				qwenPlainOutput, Auth{Mode: AuthAPIKey, APIKey: "sk-test"})
			comp, err := ask(t, read, req)
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			path, body, ok := strings.Cut(comp.Content, "|")
			if !ok {
				t.Fatalf("%s did not name a readable file: %s", tc.take, comp.Content)
			}
			if !filepath.IsAbs(path) || filepath.Base(path) != harnessprofile.DefaultSystemPromptFile {
				t.Errorf("%s = %q, want the call's own system-prompt file", tc.take, path)
			}
			if body != tc.want {
				t.Errorf("%s holds %q, want %q", tc.take, body, tc.want)
			}

			dump := qwenProvider(t, map[string]string{"FAKE_DUMP_ENV": "1"},
				qwenPlainOutput, Auth{Mode: AuthAPIKey, APIKey: "sk-test"})
			comp, err = ask(t, dump, req)
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			env := strings.Split(comp.Content, "\n")
			if slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, tc.skip+"=") }) {
				t.Errorf("the call also set %s — with QWEN_SYSTEM_MD set the harness drops the "+
					"identity file, so a seat would lose the harness's prompt", tc.skip)
			}
		})
	}
}

// A KEY OR THE PAID LOGIN SIGNS THE HARNESS IN, EACH CHOSEN ON ARGV. A key
// runs `--auth-type openai` where it always was (argv byte for byte as before
// the login), its key in OPENAI_API_KEY; the login runs `--auth-type
// qwen-subscription` in a home holding the adopted
// subscription_credentials.json, whole. Without the flag the harness exits
// "No auth type is selected" unless OPENAI_MODEL or QWEN_MODEL is set. The
// login's file is its documented shape (0.28.3), NOT MEASURED against Qwen's
// service.
//
// Mutations, each red: leave the sign-in flag in complete_args (a login call
// signs in as openai); swap key_args and subscription_args; drop
// credential_paths (no login is seeded); put the flag after the model.
func TestQwenCodeSignsInByItsModeChosenOnArgv(t *testing.T) {
	t.Parallel()
	p := qwenCode(t)
	if p.APIKeyEnv != "OPENAI_API_KEY" {
		t.Errorf("api_key_env = %q, want OPENAI_API_KEY", p.APIKeyEnv)
	}
	if !slices.Equal(p.CredentialPaths, []string{".qwen/subscription_credentials.json"}) {
		t.Errorf("credential_paths = %q, want the subscription login's file", p.CredentialPaths)
	}
	keyed := qwenProvider(t, map[string]string{"FAKE_REPORT_ENV": "OPENAI_API_KEY"},
		qwenPlainOutput, Auth{Mode: AuthAPIKey, APIKey: "sk-test"})
	if got, want := keyed.argv(false), []string{"--output-format", "stream-json", "--approval-mode", "default",
		"--allowed-mcp-server-names", "crewlet", "--auth-type", "openai", "--model", "qwen3-coder-plus"}; !slices.Equal(got, want) {
		t.Errorf("a key's argv is %q, want %q", got, want)
	}
	comp, err := ask(t, keyed, llm.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.Content != "OPENAI_API_KEY present=true" {
		t.Errorf("the key did not reach the harness: %s", comp.Content)
	}

	login, err := os.ReadFile(filepath.Join("testdata", "qwen-code", "subscription_credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	sub := qwenProvider(t, map[string]string{"FAKE_READ_UNDER_ENV": "HOME:.qwen/subscription_credentials.json"},
		qwenPlainOutput, Auth{Mode: AuthSubscription})
	if got, want := sub.argv(false), []string{"--output-format", "stream-json", "--approval-mode", "default",
		"--allowed-mcp-server-names", "crewlet", "--auth-type", "qwen-subscription", "--model", "qwen3-coder-plus"}; !slices.Equal(got, want) {
		t.Errorf("a login's argv is %q, want %q", got, want)
	}
	shared := sub.Workspace().CredentialsDir()
	if err := os.MkdirAll(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "subscription_credentials.json"), login, 0o600); err != nil {
		t.Fatal(err)
	}
	comp, err = ask(t, sub, llm.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if want := ".qwen/subscription_credentials.json=" + base64.StdEncoding.EncodeToString(login); !strings.Contains(comp.Content, want) {
		t.Errorf("the login's call found %q, want the adopted login copied whole", comp.Content)
	}
}

// NO MCP SERVER BUT THE ENGINE'S STARTS, ON ANY CALL. Qwen Code starts every
// server it finds, an extension's in the seat's home among them, unless
// --allowed-mcp-server-names names the ones it may: measured on 0.28.3, a call
// with no tools (each of doctor's probes) started the home extension's server,
// and a call with the flag and no crewlet server started none and ran. So the
// flag is in complete_args, on every call, and a run with tools adds it no
// second time.
//
// Mutations, each red: move the flag back to mcp.args (a tool-less call lacks
// it); keep it in both (a run with tools names it twice).
func TestQwenCodeStartsNoMCPServerButTheEnginesOnAnyCall(t *testing.T) {
	t.Parallel()
	admits := func(args []string) int {
		n := 0
		for i := range len(args) - 1 {
			if args[i] == "--allowed-mcp-server-names" && args[i+1] == "crewlet" {
				n++
			}
		}
		return n
	}
	p := qwenProvider(t, nil, nil, Auth{Mode: AuthAPIKey, APIKey: "sk-test"})
	if got := p.argv(false); admits(got) != 1 {
		t.Errorf("a call with no tools runs %q: it admits every MCP server it finds, a home extension's among them", got)
	}
	route := qwenCode(t).MCP
	if got := slices.Concat(p.argv(true), route.Args, route.AllowArgs); admits(got) != 1 {
		t.Errorf("a run with tools runs %q, want --allowed-mcp-server-names crewlet once", got)
	}
}

// What Qwen Code 0.28.3 printed with --output-format stream-json for a call
// that fetched a page (refused, with no network) and then answered, trimmed of
// fields nothing reads: its init event, the response's prose and its call as
// two turns, the tool's result, the turn that answered, and the result event
// with the call's totals.
const qwenStream = `{"type":"system","subtype":"init","session_id":"s1","tools":["web_fetch"],"model":"qwen3-coder-plus","qwen_code_version":"0.28.3"}
{"type":"assistant","session_id":"s1","parent_tool_use_id":null,"message":{"type":"message","role":"assistant","model":"qwen3-coder-plus","content":[{"type":"text","text":"Let me look."}],"stop_reason":null,"usage":{"input_tokens":0,"output_tokens":0}}}
{"type":"assistant","session_id":"s1","parent_tool_use_id":null,"message":{"type":"message","role":"assistant","model":"qwen3-coder-plus","content":[{"type":"tool_use","id":"call_3","name":"web_fetch","input":{"url":"https://example.com/","prompt":"summarise"}}],"stop_reason":"tool_use","usage":{"input_tokens":1003,"output_tokens":50,"cache_read_input_tokens":600,"total_tokens":1053}}}
{"type":"user","session_id":"s1","parent_tool_use_id":null,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_3","is_error":true,"content":"Error during fetch for https://example.com/: fetch failed (cause: getaddrinfo EAI_AGAIN example.com)"}]}}
{"type":"assistant","session_id":"s1","parent_tool_use_id":null,"message":{"type":"message","role":"assistant","model":"qwen3-coder-plus","content":[{"type":"text","text":"The answer."}],"stop_reason":null,"usage":{"input_tokens":1004,"output_tokens":50,"cache_read_input_tokens":600,"total_tokens":1054}}}
{"type":"result","subtype":"success","session_id":"s1","is_error":false,"num_turns":2,"result":"The answer.","usage":{"input_tokens":2007,"output_tokens":100,"cache_read_input_tokens":1200,"total_tokens":2107},"permission_denials":[]}
`

// THE ANSWER IS THE RESULT EVENT'S, AND SO ARE THE COUNTS. --output-format json
// prints one JSON ARRAY of these events, which a document reader that wants an
// object cannot open, so the whole array came back as the seat's reply; the
// stream prints one event a line. The text of the turns before the last — "Let
// me look." — and a tool's output are not the answer. The result's
// input_tokens is the whole prompt over every request of the call, the cached
// part included (the OpenAI-compatible prompt_tokens it sums), so it is the
// input count as it stands: adding cache_read_input_tokens to it would charge
// the cached tokens twice.
func TestQwenCodeReadsTheAnswerFromTheResultEvent(t *testing.T) {
	t.Parallel()
	p := qwenProvider(t, map[string]string{"FAKE_STDOUT": qwenStream}, nil, Auth{Mode: AuthAPIKey, APIKey: "sk-test"})
	comp, err := ask(t, p, llm.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.Content != "The answer." {
		t.Errorf("content = %q, want the result event's answer alone", comp.Content)
	}
	if comp.InputTokens != 2007 || comp.OutputTokens != 100 {
		t.Errorf("tokens = %d in, %d out, want the call's totals 2007 and 100",
			comp.InputTokens, comp.OutputTokens)
	}
}

// A REFUSED REQUEST IS NAMED FOR WHAT IT IS. Measured on 0.28.3
// against a stub answering every request with the status: the harness exits 1 and
// its result event's error carries "[API Error: <status> <the provider's
// message>]" — on a 429 within about a minute, because the profile turns the
// harness's own retry rounds off (left on, they held the call past the engine's
// timeout and the spent quota came back as a timeout) — and a call with no key
// at all is refused before any request. Each is recognised by the harness's own
// words, so a spent quota sits the entry out and a refused key names the
// sign-in.
func TestQwenCodeRecognisesASpentQuotaAndARefusedKey(t *testing.T) {
	t.Parallel()
	result := func(message string) string {
		raw, _ := json.Marshal(message)
		return `{"type":"system","subtype":"init","session_id":"s1","tools":["web_fetch"]}` + "\n" +
			`{"type":"result","subtype":"error_during_execution","session_id":"s1","is_error":true,"num_turns":1,` +
			`"usage":{"input_tokens":0,"output_tokens":0},"permission_denials":[],"error":{"message":` + string(raw) + `}}` + "\n"
	}
	for name, tc := range map[string]struct {
		stdout string
		want   llm.ErrorKind
	}{
		"a 429": {result("[API Error: 429 You exceeded your current quota.]\n" +
			"Possible quota limitations in place or slow response times detected. Please wait and try again later."),
			llm.KindRateLimit},
		"a 401": {result("[API Error: 401 Incorrect API key provided.]"), llm.KindAuth},
		"no key": {result("Missing API key for OpenAI-compatible auth. Set settings.security.auth.apiKey, " +
			"or set the 'OPENAI_API_KEY' environment variable."), llm.KindAuth},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := qwenProvider(t, map[string]string{"FAKE_STDOUT": tc.stdout, "FAKE_EXIT": "1"}, nil,
				Auth{Mode: AuthAPIKey, APIKey: "sk-test"})
			_, err := ask(t, p, llm.Request{})
			if got := llm.KindOf(err); got != tc.want {
				t.Errorf("kind = %v, want %v: %v", got, tc.want, err)
			}
		})
	}
}
