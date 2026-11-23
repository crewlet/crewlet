package executor_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// inBox runs shell in box's working directory, the one the read runs in.
func inBox(t *testing.T, box sandbox.Sandbox, shell string) {
	t.Helper()
	res, err := box.Exec(t.Context(), shell, sandbox.ExecOptions{})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("%s: %v (exit %d) %s", shell, err, res.ExitCode, res.Stderr)
	}
}

// A GEMINI CLI OR QWEN CODE POINT IS MARKED WHERE A BOX SETTING MOVES IT: a
// knob's name in the workspace's settings, a system settings file an
// environment variable names, a `.env` file or the environment itself, a
// sign-in but the measured key (a Google login, whose server sets Gemini
// CLI's masking, or Vertex AI, never measured), a GEMINI.md that imports
// another, a launch naming no model (whose window was not measured), or a
// release other than the measured one or the writer's;
// and a box with none, the run's own settings beside it, is the threshold of
// its window, with what the box hands over beside the brief.
//
// Mutation: skip any one source, search the run's own Qwen Code settings for
// "model", or let a hit pass, and its row goes red.
func TestAGeminiOrQwenPointIsMarkedWhereABoxSettingMovesIt(t *testing.T) {
	t.Parallel()
	key := map[string]string{"GEMINI_API_KEY": "a key"}
	for _, tc := range []struct {
		name, harness, setup string
		env                  map[string]string
		release, writer      string
		noModel              bool
		mark                 sandbox.PointMark
		tokens, boxBytes     int64
	}{
		{name: "gemini, a plain box", harness: "gemini", env: key, tokens: 524_288},
		{name: "gemini, workspace settings", harness: "gemini", env: key, mark: sandbox.PointSettings,
			setup: `mkdir -p .gemini && echo '{"model":{"compressionThreshold":0.1}}' > .gemini/settings.json`},
		{name: "gemini, system settings by the environment", harness: "gemini", mark: sandbox.PointSettings,
			setup: `echo '{"experimental":{"contextManagement":true}}' > "$HOME/sys.json"`,
			env:   map[string]string{"GEMINI_API_KEY": "a key", "GEMINI_CLI_SYSTEM_SETTINGS_PATH": "sys.json"}},
		{name: "gemini, a .env file", harness: "gemini", env: key, mark: sandbox.PointSettings,
			setup: `echo 'GEMINI_EXP=/x.json' > .env`},
		{name: "gemini, the environment", harness: "gemini", mark: sandbox.PointSettings,
			env: map[string]string{"GEMINI_API_KEY": "a key", "GEMINI_EXP": "/x.json"}},
		{name: "gemini, a login", harness: "gemini", mark: sandbox.PointSettings},
		{name: "gemini, Vertex AI", harness: "gemini", mark: sandbox.PointSettings,
			env: map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "true", "GOOGLE_API_KEY": "a key"}},
		{name: "gemini, no model", harness: "gemini", env: key, noModel: true, mark: sandbox.PointNoModel},
		{name: "gemini, a memory import", harness: "gemini", env: key, mark: sandbox.PointBoxSources,
			setup: `echo '@notes.md' > GEMINI.md`},
		{name: "gemini, another release", harness: "gemini", env: key, release: "0.71.0",
			mark: sandbox.PointRelease},
		{name: "qwen, a plain box", harness: "qwen", tokens: 167_000, boxBytes: executor.QwenReminderBytes},
		{name: "qwen, a model in the workspace", harness: "qwen", mark: sandbox.PointSettings,
			setup: `mkdir -p .qwen && echo '{"model":{"name":"deepseek-chat"}}' > .qwen/settings.json`},
		{name: "qwen, system settings by the environment", harness: "qwen", mark: sandbox.PointSettings,
			setup: `echo '{"context":{"autoCompactThreshold":0.1}}' > "$HOME/sys.json"`,
			env:   map[string]string{"QWEN_CODE_SYSTEM_SETTINGS_PATH": "sys.json"}},
		{name: "qwen, the environment", harness: "qwen", mark: sandbox.PointSettings,
			env: map[string]string{"QWEN_COMPACT_SCREENSHOT_THRESHOLD": "1"}},
		{name: "qwen, a .env file", harness: "qwen", mark: sandbox.PointSettings,
			setup: `mkdir -p .qwen && echo 'QWEN_COMPACT_SCREENSHOT_TRIGGER=0' > .qwen/.env`},
		{name: "qwen, a pair", harness: "qwen", writer: "0.28.3", mark: sandbox.PointPair},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var (
				runner *executor.Runner
				box    sandbox.Sandbox
			)
			release, model := "0.70.0", "gemini-2.5-pro"
			if tc.harness == "qwen" {
				release, model = "0.28.4", "stub-model"
				runner, box = qwenBox(t, "exit 0")
				// THE RUN'S OWN SETTINGS name a model block of their own.
				inBox(t, box, `mkdir -p "$(dirname `+shellword.Quote(executor.PathsFor(box).QwenSettings())+`)" && `+
					`echo '{"model":{"generationConfig":{"maxRetries":0}}}' > `+
					shellword.Quote(executor.PathsFor(box).QwenSettings()))
			} else {
				runner, box = geminiBoxAt(t, release, "exit 0")
			}
			if tc.setup != "" {
				inBox(t, box, tc.setup)
			}
			env := map[string]string{}
			for k, v := range tc.env {
				env[k] = v
			}
			if path, ok := env["GEMINI_CLI_SYSTEM_SETTINGS_PATH"]; ok {
				env["GEMINI_CLI_SYSTEM_SETTINGS_PATH"] = box.Home() + "/" + path
			}
			if path, ok := env["QWEN_CODE_SYSTEM_SETTINGS_PATH"]; ok {
				env["QWEN_CODE_SYSTEM_SETTINGS_PATH"] = box.Home() + "/" + path
			}
			ask := sandbox.PointAsk{Model: model, Release: release, Writer: release, Env: env}
			if tc.release != "" {
				ask.Release = tc.release
			}
			if tc.writer != "" {
				ask.Writer = tc.writer
			}
			if tc.noModel {
				ask.Model = ""
			}
			point, err := runner.ReadBoxPoint(t.Context(), box, ask)
			if err != nil {
				t.Fatalf("ReadBoxPoint: %v", err)
			}
			if point.Mark != tc.mark {
				t.Fatalf("the point reads %+v, want mark %q", point, tc.mark)
			}
			if tc.mark != "" {
				return
			}
			if point.Tokens != tc.tokens || point.Charge != 1 || point.Anchored {
				t.Errorf("the point reads %+v, want %d at a byte a token, unanchored", point, tc.tokens)
			}
			if tc.harness == "qwen" && point.BoxBytes != tc.boxBytes {
				t.Errorf("the box hands over %d bytes beside the brief, want the reminder's %d", point.BoxBytes,
					tc.boxBytes)
			}
			if tc.harness == "gemini" && point.BoxBytes < executor.GeminiContextBase {
				t.Errorf("the box hands over %d bytes of session context, below its fixed %d", point.BoxBytes,
					executor.GeminiContextBase)
			}
		})
	}
}
