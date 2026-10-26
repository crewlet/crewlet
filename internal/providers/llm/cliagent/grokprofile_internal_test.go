package cliagent

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// The invariants the built-in grok profile keeps, each measured against xAI's
// Grok Build CLI 1.0.70 (see the profile's comments): a call offers the web
// tools and nothing else, a seat's phases keep the CLI's own system prompt and
// a side pass replaces it, the transcript reaches the CLI in a file, the side
// requests that can be switched off are, and nothing a call leaves in the
// seat's home outlives the calls that share it.

// grok is the shipped profile, which every case here reads.
func grok(t *testing.T) cliprofile.Profile {
	t.Helper()
	p, ok := cliprofile.Builtin("grok")
	if !ok {
		t.Fatal("no built-in grok profile")
	}
	return p
}

// A CALL OFFERS THE WEB TOOLS AND NOTHING ELSE. The allowlist names the two web
// tools and nothing it might not know — one unknown name and 1.0.70 drops the
// whole list — and the deny list names every other built-in, which a denial
// removes even where the allowlist was dropped: on the deny list alone, the
// twelve listed last below were still offered beside the web tools. web_fetch
// ships switched off, so the allowlist would name a tool that is not there
// without the switch.
func TestGrokOffersTheWebToolsAndNothingElse(t *testing.T) {
	t.Parallel()
	p := grok(t)
	allowed, ok := flagValues(p.CompleteArgs, "--tools")
	if !ok {
		t.Fatalf("complete_args = %q carries no --tools", p.CompleteArgs)
	}
	if !slices.Equal(allowed, []string{"web_search", "web_fetch"}) {
		t.Errorf("--tools = %q, want exactly web_search and web_fetch", allowed)
	}
	denied, ok := flagValues(p.CompleteArgs, "--disallowed-tools")
	if !ok {
		t.Fatalf("complete_args = %q carries no --disallowed-tools", p.CompleteArgs)
	}
	for _, name := range []string{
		"run_terminal_cmd", "read_file", "search_replace", "list_dir", "grep", "task",
		"workflow", "todo_write", "scheduler_create",
		// Offered on 1.0.70 beside the web tools until denied by name.
		"write", "image_gen", "image_edit", "image_to_video", "reference_to_video",
		"search_tool", "use_tool", "send_feedback",
		"enter_plan_mode", "exit_plan_mode", "ask_user_question",
	} {
		if !slices.Contains(denied, name) {
			t.Errorf("--disallowed-tools lacks %s, which 1.0.70 offers otherwise", name)
		}
	}
	for _, web := range []string{"web_search", "web_fetch"} {
		if slices.Contains(denied, web) {
			t.Errorf("--disallowed-tools denies %s — web is the one tool every seat keeps", web)
		}
	}
	if got := p.Env["GROK_WEB_FETCH"]; got != "1" {
		t.Errorf("GROK_WEB_FETCH = %q, want 1: web_fetch ships switched off", got)
	}
}

// THE FETCH A CALL IS OFFERED IS ALLOWED, NOT MERELY OFFERED. web_fetch asks
// first, a headless run cannot ask, and 1.0.70 then cancels the WHOLE run — exit
// 0, nothing on stdout, "was not run, as a headless run cannot ask for approval"
// on stderr — so a call that fetched a page failed (measured through the
// engine). --permission-mode dontAsk did not change that; the allow rule did,
// and the fetch ran.
//
// Mutation: drop the allow rule from complete_args, and this fails.
func TestGrokAllowsTheFetchItOffers(t *testing.T) {
	t.Parallel()
	p := grok(t)
	var allowed []string
	for i, arg := range p.CompleteArgs {
		if arg == "--allow" && i+1 < len(p.CompleteArgs) {
			allowed = append(allowed, p.CompleteArgs[i+1])
		}
	}
	if !slices.Contains(allowed, "WebFetch") {
		t.Errorf("complete_args = %q allows %q — web_fetch is offered and not allowed, and a headless "+
			"run cancels itself at the fetch's approval", p.CompleteArgs, allowed)
	}
}

// grokProvider is the shipped profile driven against the fake CLI.
func grokProvider(t *testing.T, env map[string]string) *Provider {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	p, err := New(Config{
		Key: "grok", Model: "grok-4", Agent: "grok", StateDir: dir,
		Overrides: map[string]any{"binary": os.Args[0]},
		Timeout:   20 * time.Second, MaxConcurrent: 1,
		Env:  fakeChildEnv(env),
		Auth: Auth{Mode: AuthSubscription},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// A SEAT KEEPS THE CLI'S OWN PROMPT AND A SIDE PASS REPLACES IT: --rules lands
// the seat's text after the vendor's static prompt, under the note that says
// which of the two is the role, and --system-prompt-override makes a side
// pass's text the whole system message. Never both flags on one call. Driven
// through Complete, so what is checked is the argv a call is handed.
func TestGrokSeatAppendsAndASidePassReplaces(t *testing.T) {
	t.Parallel()
	const identity = "You are Agent PM at Nimbus."
	for name, tc := range map[string]struct {
		agent      bool
		take, skip string
		want       string
	}{
		"a seat at work": {
			agent: true, take: "--rules", skip: "--system-prompt-override",
			want: harnessNote + "\n\n" + identity,
		},
		"a side pass": {
			agent: false, take: "--system-prompt-override", skip: "--rules",
			want: identity,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := t.TempDir()
			p := grokProvider(t, map[string]string{"FAKE_STDOUT": "ok", "FAKE_RECORD_ARGV": rec})
			comp, err := ask(t, p, llm.Request{Agent: tc.agent, Messages: []llm.Message{
				{Role: llm.RoleSystem, Content: identity},
				{Role: llm.RoleUser, Content: "status?"},
			}})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if comp.Content != "ok" {
				t.Errorf("content = %q, want the answer the CLI printed", comp.Content)
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
				t.Errorf("argv = %q also carries %s — the CLI is handed the prompt twice", argv, tc.skip)
			}
		})
	}
}

// THE TRANSCRIPT REACHES THE CLI IN A FILE: --prompt-file is a headless single
// turn like -p, with the transcript private in the call's directory rather than
// on argv, where ARG_MAX bounds it and every account on the machine can read
// it. Asked from inside the child, which prints its argv, the file's mode and
// its bytes.
func TestGrokTakesTheTranscriptFromAFile(t *testing.T) {
	t.Parallel()
	p := grokProvider(t, map[string]string{"FAKE_PROMPT_FILE": "--prompt-file"})
	comp, err := ask(t, p, llm.Request{Agent: true, Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: "You are Agent PM at Nimbus."},
		{Role: llm.RoleUser, Content: "what is the status of the launch?"},
	}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	parts := strings.SplitN(comp.Content, "|", 3)
	if len(parts) != 3 {
		t.Fatalf("the child could not read a prompt file: %s", comp.Content)
	}
	argv, mode, encoded := parts[0], parts[1], parts[2]
	body, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "what is the status of the launch?") {
		t.Errorf("the prompt file holds %q, want the transcript", body)
	}
	if strings.Contains(string(body), "Agent PM") {
		t.Errorf("the prompt file holds %q — the system prompt rides --rules, not the transcript", body)
	}
	if mode != "600" {
		t.Errorf("mode = %s, want 600", mode)
	}
	if strings.Contains(argv, "status of the launch") {
		t.Errorf("argv = %q carries the transcript", argv)
	}
	if slices.Contains(strings.Fields(argv), "-p") {
		t.Errorf("argv = %q carries -p, which takes the next argument as the prompt", argv)
	}
}

// EVERY COMMAND CARRIES THE SWITCHES THAT KEEP A CALL TO ITSELF: no background
// workflows (the list of them left every request), no second model request
// after the answer to summarise the turn, and no cross-session memory whatever
// the account's remote settings say.
func TestGrokCallsCarryTheVendorsSwitches(t *testing.T) {
	t.Parallel()
	env := childEnv(grok(t), &Checkout{Home: t.TempDir(), Cache: t.TempDir(), Work: t.TempDir()}, nil, Auth{})
	for name, want := range map[string]string{
		"GROK_WORKFLOWS":    "0",
		"GROK_TURN_SUMMARY": "0",
		"GROK_MEMORY":       "0",
	} {
		if got := env[name]; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// NOTHING A CALL LEAVES IN THE SEAT'S HOME OUTLIVES ITS GENERATION: the session
// directory holding the conversation, the rendered system prompt and the
// prompt history, the session-search database, and the log that grows on every
// call — all measured on 1.0.70.
func TestGrokPrunesWhatACallLeaves(t *testing.T) {
	t.Parallel()
	p := grok(t)
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	ws, err := Shared(dir, "grok", p)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	const session = ".grok/sessions/%2Ftmp%2Fstate%2Fgrok%2Fseats%2Fagent-pm%2Fwork%2Fcall-1"
	left := []string{
		session + "/019a1b2c-0000-7000-8000-000000000000/chat_history.jsonl",
		session + "/019a1b2c-0000-7000-8000-000000000000/system_prompt.txt",
		session + "/prompt_history.jsonl",
		".grok/sessions/session_search.sqlite",
		".grok/logs/unified.jsonl",
	}
	for _, rel := range left {
		path := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := ws.prune(home); err != nil {
		t.Fatalf("prune: %v", err)
	}
	for _, rel := range left {
		if _, err := os.Stat(filepath.Join(home, rel)); !os.IsNotExist(err) {
			t.Errorf("%s outlived the prune (%v): the next call of the seat would find it", rel, err)
		}
	}
}

// WHAT THE API REFUSED IS NAMED, in the CLI's own words as 1.0.70 printed them:
// signed out and a key the API refuses are the login, and a 402 is a spent
// account, which sits the entry out rather than failing every seat on it as a
// server error.
func TestGrokNamesWhatTheAPIRefused(t *testing.T) {
	t.Parallel()
	p := grok(t)
	for name, tc := range map[string]struct {
		stderr string
		want   llm.ErrorKind
	}{
		"signed out": {
			stderr: "Not signed in. To authenticate without a browser, run:\n  grok login --device-code\n",
			want:   llm.KindAuth,
		},
		"a key the API refuses": {
			stderr: "Internal error: {\n  \"message\": \"Unauthorized (401) from https://api.x.ai/v1/chat/completions: " +
				"invalid_api_key\",\n  \"http_status\": 401\n}\n",
			want: llm.KindAuth,
		},
		"a spent account": {
			stderr: "Internal error: {\n  \"message\": \"API error (status 402 Payment Required): " +
				"out of credits\",\n  \"http_status\": 402\n}\n",
			want: llm.KindRateLimit,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hit, ok := classifyMarkers(p, cliOutput{stderr: tc.stderr, failed: true})
			if !ok || hit.Kind != tc.want {
				t.Errorf("classified %+v (matched %v), want %v for:\n%s", hit, ok, tc.want, tc.stderr)
			}
		})
	}
}
