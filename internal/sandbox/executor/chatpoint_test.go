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
// knob in the workspace's, the home's or the run's settings, a system file an
// environment variable names, a `.env` file (the workspace's, above it, the
// home's) or the environment, a model a `.env` names for an unpinned run, a sign-in but the
// measured key (Gemini CLI's Google login or Vertex AI, Qwen Code's paid
// login), a GEMINI.md import, a Gemini CLI launch naming no model, or another
// release; a box with none is its window's threshold, with what it hands over.
//
// Mutation: skip any one source but the /etc files (no test box writes there),
// search the run's own Qwen Code settings for "model", read a sign-in as the
// key, or let a hit pass, and its row goes red.
func TestAGeminiOrQwenPointIsMarkedWhereABoxSettingMovesIt(t *testing.T) {
	t.Parallel()
	key := map[string]string{"GEMINI_API_KEY": "a key"}
	elsewhere := map[string]string{"HOME": "elsewhere"}
	for _, tc := range []struct {
		name, harness, setup string
		ownSettings          string
		env                  map[string]string
		release, writer      string
		noModel, login       bool
		mark                 sandbox.PointMark
		tokens, boxBytes     int64
	}{
		{name: "gemini, a plain box", harness: "gemini", env: key, tokens: 524_288},
		{name: "gemini, workspace settings", harness: "gemini", env: key, mark: sandbox.PointSettings,
			setup: `mkdir -p .gemini && echo '{"model":{"compressionThreshold":0.1}}' > .gemini/settings.json`},
		{name: "gemini, the home's settings", harness: "gemini", env: key, mark: sandbox.PointSettings,
			setup: `mkdir -p "$HOME/.gemini" && echo '{"model":{"compressionThreshold":0.1}}' > ` +
				`"$HOME/.gemini/settings.json"`},
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
		{name: "qwen, the paid login", harness: "qwen", login: true, mark: sandbox.PointSettings},
		{name: "qwen, system defaults by the environment", harness: "qwen", mark: sandbox.PointSettings,
			setup: `echo '{"context":{"autoCompactThreshold":0.1}}' > "$HOME/sys.json"`,
			env:   map[string]string{"QWEN_CODE_SYSTEM_DEFAULTS_PATH": "sys.json"}},
		{name: "qwen, a knob in the run's own settings", harness: "qwen", mark: sandbox.PointSettings,
			ownSettings: `{"model":{"generationConfig":{"maxRetries":0}},"context":{"autoCompactThreshold":0.1}}`},
		{name: "qwen, a model in the home's settings", harness: "qwen", mark: sandbox.PointSettings,
			setup: `mkdir -p "$HOME/.qwen" && echo '{"model":{"name":"deepseek-chat"}}' > "$HOME/.qwen/settings.json"`},
		{name: "qwen, a .env file in the workspace", harness: "qwen", mark: sandbox.PointSettings,
			setup: `echo 'QWEN_COMPACT_SCREENSHOT_THRESHOLD=1' > .env`},
		// ELSEWHERE, so the walk up from the workspace passes the home's files.
		{name: "qwen, a .env file above the workspace", harness: "qwen", mark: sandbox.PointSettings,
			setup: `echo 'QWEN_COMPACT_SCREENSHOT_THRESHOLD=1' > ../.env`, env: elsewhere},
		{name: "qwen, the home's .qwen/.env", harness: "qwen", mark: sandbox.PointSettings, env: elsewhere,
			setup: `mkdir -p "$HOME/elsewhere/.qwen" && echo 'QWEN_COMPACT_SCREENSHOT_THRESHOLD=1' > ` +
				`"$HOME/elsewhere/.qwen/.env"`},
		{name: "qwen, the home's .env", harness: "qwen", mark: sandbox.PointSettings, env: elsewhere,
			setup: `mkdir -p "$HOME/elsewhere" && echo 'QWEN_COMPACT_SCREENSHOT_THRESHOLD=1' > "$HOME/elsewhere/.env"`},
		{name: "qwen, no model", harness: "qwen", noModel: true, tokens: 98_072, boxBytes: executor.QwenReminderBytes},
		{name: "qwen, a model in a .env for a run that pins none", harness: "qwen", noModel: true,
			mark: sandbox.PointSettings, setup: `echo 'OPENAI_MODEL=deepseek-chat' > .env`},
		{name: "qwen, a model in a .env under the pin", harness: "qwen", tokens: 167_000,
			boxBytes: executor.QwenReminderBytes, setup: `echo 'OPENAI_MODEL=deepseek-chat' > .env`},
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
				own := `{"model":{"generationConfig":{"maxRetries":0}}}`
				if tc.ownSettings != "" {
					own = tc.ownSettings
				}
				inBox(t, box, `mkdir -p "$(dirname `+shellword.Quote(executor.PathsFor(box).QwenSettings())+`)" && `+
					`echo `+shellword.Quote(own)+` > `+shellword.Quote(executor.PathsFor(box).QwenSettings()))
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
			for _, name := range []string{"QWEN_CODE_SYSTEM_SETTINGS_PATH", "QWEN_CODE_SYSTEM_DEFAULTS_PATH", "HOME"} {
				if path, ok := env[name]; ok {
					env[name] = box.Home() + "/" + path
				}
			}
			if tc.harness == "qwen" && !tc.login {
				env["OPENAI_API_KEY"] = "a key"
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
