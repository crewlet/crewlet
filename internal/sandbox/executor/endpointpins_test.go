package executor_test

import (
	"context"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// pinsOf is h's pins for req, by name.
func pinsOf(t *testing.T, h interface {
	Pins(ctx context.Context, req sandbox.RunRequest, paths executor.Paths) []executor.Pinned
}, req sandbox.RunRequest) map[string]string {
	t.Helper()
	pins := map[string]string{}
	for _, pin := range h.Pins(t.Context(), req, executor.Paths{Home: "/home/box"}) {
		pins[pin.Name] = pin.Value
	}
	return pins
}

// A GEMINI RUN'S REQUESTS GO WHERE ITS ENTRY SAYS, OR TO GEMINI CLI'S OWN
// ENDPOINTS, never where an env file says: a login's Code Assist endpoint, a
// key's base URL and Vertex AI's are pinned to the entry's value or, for an
// entry that names none, to Gemini CLI's own (the Code Assist URL named, the
// base URLs empty, as Gemini CLI reads an unset one); and the prompt
// variables are `false`, so no file replaces the prompt the script builds.
//
// Mutations, each red: pin only what the entry names; pin the Code Assist
// endpoint empty; leave a prompt variable unpinned.
func TestAGeminiRunsRequestsGoWhereItsEntrySays(t *testing.T) {
	t.Parallel()
	gateway := "https://gateway.example.com"
	for name, tc := range map[string]struct {
		env  map[string]string
		want map[string]string
	}{
		"a login naming no endpoint": {map[string]string{"GOOGLE_GENAI_USE_GCA": "true"},
			map[string]string{"CODE_ASSIST_ENDPOINT": "https://cloudcode-pa.googleapis.com"}},
		"a login naming one": {map[string]string{"CODE_ASSIST_ENDPOINT": gateway},
			map[string]string{"CODE_ASSIST_ENDPOINT": gateway}},
		"a key naming no base URL": {map[string]string{"GEMINI_API_KEY": "key-placeholder"},
			map[string]string{"GOOGLE_GEMINI_BASE_URL": ""}},
		"a key behind a gateway": {map[string]string{"GEMINI_API_KEY": "key-placeholder", "GOOGLE_GEMINI_BASE_URL": gateway},
			map[string]string{"GOOGLE_GEMINI_BASE_URL": gateway}},
		"vertex naming no base URL": {map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "true"},
			map[string]string{"GOOGLE_VERTEX_BASE_URL": ""}},
		"vertex behind a gateway": {map[string]string{"GOOGLE_VERTEX_BASE_URL": gateway},
			map[string]string{"GOOGLE_VERTEX_BASE_URL": gateway}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pins := pinsOf(t, executor.GeminiCLI{}, sandbox.RunRequest{Env: tc.env})
			want := map[string]string{"GEMINI_SYSTEM_MD": "false", "GEMINI_WRITE_SYSTEM_MD": "false"}
			for k, v := range tc.want {
				want[k] = v
			}
			for k, v := range want {
				if got, ok := pins[k]; !ok || got != v {
					t.Errorf("%s pinned %q (pinned: %t), want %q", k, got, ok, v)
				}
			}
		})
	}
	if unset := (executor.GeminiCLI{}).Output(executor.Paths{Home: "/home/box"}).Unset; slices.Contains(unset, "GEMINI_SYSTEM_MD") {
		t.Errorf("GEMINI_SYSTEM_MD is unset (%v), which leaves it for an env file to fill", unset)
	}
}

// A QWEN RUN'S KEY GOES WHERE ITS ENTRY SAYS, OR TO QWEN CODE'S OWN ENDPOINT,
// never where an env file says, and no file replaces its prompt.
//
// Mutations, each red: pin OPENAI_BASE_URL only where the entry names it;
// leave QWEN_SYSTEM_MD unset rather than pinned.
func TestAQwenRunsKeyGoesWhereItsEntrySays(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"naming no endpoint": {map[string]string{"OPENAI_API_KEY": "key-placeholder"},
			"https://dashscope.aliyuncs.com/compatible-mode/v1"},
		"naming one": {map[string]string{"OPENAI_API_KEY": "key-placeholder", "OPENAI_BASE_URL": "https://example.com/v1"},
			"https://example.com/v1"},
		"a subscription login": {nil, "https://dashscope.aliyuncs.com/compatible-mode/v1"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pins := pinsOf(t, executor.QwenCode{}, sandbox.RunRequest{Env: tc.env})
			if pins["OPENAI_BASE_URL"] != tc.want || pins["QWEN_SYSTEM_MD"] != "false" ||
				pins["QWEN_WRITE_SYSTEM_MD"] != "false" {
				t.Errorf("the run pins %v, want OPENAI_BASE_URL %q and the prompt variables false", pins, tc.want)
			}
		})
	}
}
