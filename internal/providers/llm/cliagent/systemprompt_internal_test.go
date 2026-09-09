package cliagent

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// THE CHILD ACTUALLY RECEIVES THE VARIABLE, POINTING AT A FILE THAT EXISTS.
//
// Everything else about this channel is a claim until the process sees it: the
// env is assembled from an allowlist plus the profile's own `config_env`, and
// a name that lost to one of those, or a path written after the child was
// spawned, would leave the CLI reading its built-in prompt while the engine
// believed it had been replaced — silently, and on every turn of every seat.
func TestTheSystemPromptEnvVarReachesTheChildPointingAtTheText(t *testing.T) {
	p := fakeProvider(t, map[string]string{
		"FAKE_READ_SYSTEM_PROMPT": "GEMINI_SYSTEM_MD",
	}, map[string]any{"system_prompt_env": "GEMINI_SYSTEM_MD"})
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
		t.Errorf("GEMINI_SYSTEM_MD=%q is relative — the CLI resolves it against "+
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
	pair, err := systemEnv("GEMINI_SYSTEM_MD", "the seat's whole identity", dir)
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
	p := fakeProvider(t, map[string]string{"FAKE_ECHO_STDIN": "1"}, map[string]any{
		"system_prompt_env": "QWEN_SYSTEM_MD",
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

// A profile may not declare both channels. Refused at load, because which copy
// a CLI honours when handed the same prompt twice is the vendor's business.
func TestAProfileCannotDeclareBothSystemPromptChannels(t *testing.T) {
	t.Parallel()
	_, err := Load("custom", map[string]any{
		"binary":             "x",
		"complete_args":      []any{"run"},
		"output":             "text",
		"system_prompt_args": []any{"--system-prompt", "{system}"},
		"system_prompt_env":  "SOME_SYSTEM_MD",
	})
	if err == nil {
		t.Fatal("a profile declaring both channels loaded")
	}
	if !strings.Contains(err.Error(), "ONE channel") {
		t.Errorf("the error does not say what is wrong: %v", err)
	}
}
