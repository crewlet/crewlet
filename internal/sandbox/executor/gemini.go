package executor

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// GeminiCLIName is this runner's config name.
const GeminiCLIName = "gemini-cli"

// GeminiCLI drives Gemini CLI (`gemini`) headless, its event stream on stdout
// (measured on 0.70.0 and 0.65.0 against a stub of the Gemini API;
// docs/concepts/code-sandbox.md, "Gemini CLI", says what a run does).
type GeminiCLI struct{}

func init() { Register(GeminiCLI{}) }

// NewGeminiCLI is a runner driving Gemini CLI.
func NewGeminiCLI() *Runner { return New(GeminiCLI{}) }

// Name is this runner's config name.
func (GeminiCLI) Name() string { return GeminiCLIName }

// Session mints the run's own session: Gemini CLI names a new session by any
// word of letters, digits, dashes and underscores (`--session-id`) and refuses
// one it already holds (exit 42), so every run gets a fresh uuid.
func (GeminiCLI) Session(sandbox.RunRequest) Session { return Session{ID: uuid.NewString()} }

// geminiFlags is what both the prompt's export and the run itself are started
// with: the export depends on the tools the approval mode offers, on the model
// family and on the servers admitted, so it is made with the run's own. Each
// of the run's servers is admitted by name, so an extension the image
// installed starts none of its own (the text phases measured the flag).
func geminiFlags(req sandbox.RunRequest) []string {
	flags := []string{"-p", `""`, "--output-format", "stream-json", "--approval-mode", "yolo"}
	if req.Model != "" {
		flags = append(flags, "-m", sandbox.ShellQuote(req.Model))
	}
	for _, name := range slices.Sorted(maps.Keys(req.MCPServers)) {
		flags = append(flags, "--allowed-mcp-server-names", sandbox.ShellQuote(name))
	}
	return flags
}

// Command is the run's script ([geminiScript]).
func (GeminiCLI) Command(req sandbox.RunRequest, session Session, paths Paths, _ string) string {
	return "sh -c " + sandbox.ShellQuote(geminiScript(req, session, paths))
}

// geminiExportExit is what `gemini` exits with once GEMINI_WRITE_SYSTEM_MD
// has written its default prompt: it writes the file and stops before any
// model request (measured on 0.70.0 and 0.65.0, zero requests at the stub).
const geminiExportExit = 42

// geminiScript is a Gemini CLI run's script. It pins the project slug, exports
// the harness's own system prompt and adds the run's text after it, runs the
// harness on the brief (stdin, [GeminiCLI.Output]), and appends the run's own
// spend read off its session file ([geminiUsageEvent]).
//
// THE ADD-TO CHANNEL IS BUILT, since Gemini CLI has none: GEMINI_SYSTEM_MD
// replaces its prompt, so the script has it write the prompt out
// (GEMINI_WRITE_SYSTEM_MD, byte for byte the systemInstruction it would send,
// tools, hook context and skills resolved) and names the export plus the run's
// text. Made per run, in the run's box with the run's flags, because the
// export depends on the box's home, skills, policy and model.
func geminiScript(req sandbox.RunRequest, session Session, paths Paths) string {
	flags := strings.Join(geminiFlags(req), " ")
	q := sandbox.ShellQuote
	lines := []string{
		"node " + q(paths.GeminiHelper()) + " pin " + q(paths.GeminiProjects()) + " " + GeminiProjectSlug + " || exit 1",
	}
	run := "gemini " + flags + " --session-id " + q(session.ID) + "; code=$?"
	// A RUN WITH NO STANDING INSTRUCTIONS keeps Gemini CLI's own prompt as it
	// is, so nothing is exported or named.
	if geminiSystemText(GeminiCLI{}, req) != "" {
		lines = append(lines,
			"rm -f "+q(paths.GeminiDefaultSystem()),
			"GEMINI_WRITE_SYSTEM_MD="+q(paths.GeminiDefaultSystem())+" gemini "+flags+
				" < /dev/null > /dev/null 2> "+q(paths.GeminiExportErr())+"; exported=$?",
			fmt.Sprintf(`if [ "$exported" != %d ] || [ ! -s %s ]; then `+
				`echo "crewlet: Gemini CLI wrote no system prompt to add the run's text to `+
				`(GEMINI_WRITE_SYSTEM_MD exited $exported, not %d):" >&2; cat %s >&2; exit 1; fi`,
				geminiExportExit, q(paths.GeminiDefaultSystem()), geminiExportExit, q(paths.GeminiExportErr())),
			"{ cat "+q(paths.GeminiDefaultSystem())+"; printf '\\n\\n'; cat "+q(paths.System())+
				"; } > "+q(paths.GeminiSystem())+" || exit 1")
		run = "GEMINI_SYSTEM_MD=" + q(paths.GeminiSystem()) + " " + run
	}
	return strings.Join(append(lines, run,
		"node "+q(paths.GeminiHelper())+" usage "+q(paths.GeminiChats())+" "+q(session.ID),
		"exit $code"), "\n")
}

// geminiSystemText is the system text a run of req adds to Gemini CLI's own
// prompt: its standing instructions, with h's instruction about notes last
// where its notes reach it; "" for none.
func geminiSystemText(h GeminiCLI, req sandbox.RunRequest) string {
	if req.Steer.Set() {
		return withNoteInstruction(req.System, h.NoteInstruction())
	}
	return req.System
}

// Output is the event stream on stdout, which is also the result: Gemini CLI
// ends it with a `result` event, and the script appends the run's spend after
// it. The brief goes on stdin (`-p ""` reads it there), so no brief is ever on
// a command line. Unset is what moves the harness's state or prompt from
// where the script put them: an image or profile setting either would send
// the run to another home or replace the prompt the script built.
func (GeminiCLI) Output(paths Paths) Output {
	return Output{Stdout: paths.Result(), Events: true, BriefOnStdin: true,
		Unset: []string{"GEMINI_CLI_HOME", "GEMINI_SYSTEM_MD", "GEMINI_WRITE_SYSTEM_MD"}}
}

// Pins are what a headless run needs whatever the image says: a trusted
// workspace (an untrusted one exits 55 before any request) and no browser (a
// login the run cannot use exits 41 at once instead of asking to open one and
// reading the brief as the answer; the profile's text phases measured both).
func (GeminiCLI) Pins(context.Context, sandbox.RunRequest, Paths) []Pinned {
	return []Pinned{{Name: "GEMINI_CLI_TRUST_WORKSPACE", Value: "true"}, {Name: "NO_BROWSER", Value: "true"}}
}

// Events is a new decoder for one stream ([geminiEvents]).
func (GeminiCLI) Events() Decoder { return &geminiEvents{} }

// Parse decodes a whole stream: the result of a run whose result is its
// stream.
func (GeminiCLI) Parse(stdout []byte) sandbox.Result { return decodeAll(&geminiEvents{}, stdout) }

// WriteConfig writes what a run's script and harness read: the run's system
// text, the helper the script runs, the settings and policy in the box home,
// the run's skills, and, where notes or skills are wired, the take script and
// its configuration. It returns "": Gemini CLI reads its settings from its
// home, never from a path it is handed.
func (g GeminiCLI) WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths) (string, error) {
	files := map[string][]byte{
		paths.GeminiHelper(): geminiHelperScript,
		paths.GeminiPolicy(): []byte(geminiPolicy),
	}
	if system := geminiSystemText(g, req); system != "" {
		files[paths.System()] = []byte(system)
	}
	hooked := req.Steer.Set() || len(req.Skills) > 0
	if hooked {
		take, err := geminiTakeConfig(req, paths)
		if err != nil {
			return "", err
		}
		files[paths.GeminiTake()] = geminiTakeScript
		files[paths.GeminiTakeConfig()] = take
	}
	settings, err := json.MarshalIndent(geminiSettings(req, paths, hooked), "", "  ")
	if err != nil {
		return "", fmt.Errorf("executor: rendering Gemini CLI's settings: %w", err)
	}
	files[paths.GeminiSettings()] = settings
	for _, s := range req.Skills {
		file, err := skillFile(geminiSkillName(s), s.Description, s.Body)
		if err != nil {
			return "", err
		}
		files[paths.GeminiSkills()+"/"+geminiSkillName(s)+"/SKILL.md"] = file
	}
	// THE LAST RUN'S SKILLS GO FIRST: Gemini CLI reads skills only from the
	// home's skills directory, which may hold the image's own, so the run's
	// are named apart ([geminiSkillName]) and only those are removed.
	res, err := box.Exec(ctx, "rm -rf "+sandbox.ShellQuote(paths.GeminiSkills())+"/"+sandbox.SkillNamespace+"-*",
		sandbox.ExecOptions{})
	if err != nil {
		return "", fmt.Errorf("executor: clearing the last run's skills for Gemini CLI: %w", err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("executor: clearing the last run's skills for Gemini CLI exited %d: %s",
			res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	for path, content := range files {
		if err := box.WriteFile(ctx, path, content); err != nil {
			return "", fmt.Errorf("executor: writing %s for Gemini CLI: %w", path, err)
		}
	}
	return "", nil
}

// geminiSkillName is the name a run's skill is written and listed under:
// [sandbox.Skill.Listed] with its colon a dash, as Gemini CLI would rewrite it
// (it lists and activates `crewlet:x` only as `crewlet-x`, measured on
// 0.70.0). Written so rather than left to the harness, and mapped back by the
// take script as it reports a load ([geminiSkillReported]).
func geminiSkillName(s sandbox.Skill) string { return sandbox.SkillNamespace + "-" + s.Name }

// geminiSettings is the run's ~/.gemini/settings.json: the key sign-in for a
// run handed GEMINI_API_KEY and Vertex for one handed GOOGLE_GENAI_USE_VERTEXAI
// (a gateway base URL would otherwise select the gateway sign-in, which the
// harness refuses; the profile's key_seed_files), none for a login, which
// GOOGLE_GENAI_USE_GCA selects; usage statistics and the update check off (two
// connections to the vendor a run otherwise); subagents off, since the hooks
// fire inside a subagent's loop as in the main agent's, so a note would land
// in delegated work; and the take script's hooks where wired: AfterTool for
// skill loads, AfterAgent for notes ([GeminiCLISteerInstruction]).
func geminiSettings(req sandbox.RunRequest, paths Paths, hooked bool) map[string]any {
	settings := map[string]any{
		"privacy":      map[string]any{"usageStatisticsEnabled": false},
		"general":      map[string]any{"enableAutoUpdate": false, "enableAutoUpdateNotification": false},
		"experimental": map[string]any{"enableAgents": false},
	}
	// The run's round cap, as Gemini CLI caps a session's turns: it ends the
	// run exit 53, a FatalTurnLimitedError (the profile's cap, measured).
	if req.Limits.MaxTurns > 0 {
		settings["model"] = map[string]any{"maxSessionTurns": req.Limits.MaxTurns}
	}
	switch {
	case req.Env["GEMINI_API_KEY"] != "":
		settings["security"] = map[string]any{"auth": map[string]any{"selectedType": "gemini-api-key"}}
	case req.Env["GOOGLE_GENAI_USE_VERTEXAI"] == "true":
		settings["security"] = map[string]any{"auth": map[string]any{"selectedType": "vertex-ai"}}
	}
	if len(req.MCPServers) > 0 {
		servers := make(map[string]any, len(req.MCPServers))
		for name, s := range req.MCPServers {
			servers[name] = geminiMCP(s)
		}
		settings["mcpServers"] = servers
	}
	if hooked {
		hook := func() []any {
			return []any{map[string]any{"matcher": "", "hooks": []any{map[string]any{
				"type": "command", "name": "crewlet", "timeout": geminiHookTimeout.Milliseconds(),
				"command": "node " + sandbox.ShellQuote(paths.GeminiTake()) + " " + sandbox.ShellQuote(paths.GeminiTakeConfig()),
			}}}}
		}
		events := map[string]any{}
		if len(req.Skills) > 0 {
			events["AfterTool"] = hook()
		}
		if req.Steer.Set() {
			events["AfterAgent"] = hook()
		}
		settings["hooks"] = events
	}
	return settings
}

// geminiHookTimeout bounds one firing of the take script: the ten seconds the
// note contract gives a hook (ADR-0176, section 2), twice the bridge's own
// five-second answer budget. Gemini CLI kills a hook at it (SIGTERM, SIGKILL
// five seconds on) and carries on; it sends no cancel (measured on 0.70.0).
const geminiHookTimeout = 10 * time.Second

// geminiMCP is one server in Gemini CLI's settings vocabulary: `httpUrl` and
// `headers` for streamable HTTP, `command`, `args` and `env` for stdio, and
// `timeout` in milliseconds where the engine sets a wait
// ([sandbox.MCPServer.CallTimeout]): the harness cuts a call at it (600,000
// by default; 4,000 cut an 8 s call, measured on 0.70.0), so a question the
// seat parks on is held to the engine's hold, not Gemini's ten minutes.
func geminiMCP(s sandbox.MCPServer) map[string]any {
	var out map[string]any
	if s.Transport == sandbox.TransportHTTP {
		out = map[string]any{"httpUrl": s.URL}
		if len(s.Headers) > 0 {
			out["headers"] = s.Headers
		}
	} else {
		out = map[string]any{"command": s.Command, "args": s.Args}
		if len(s.Env) > 0 {
			out["env"] = s.Env
		}
	}
	if s.CallTimeout > 0 {
		out["timeout"] = s.CallTimeout.Milliseconds()
	}
	return out
}

// geminiPolicy is the run's policy file, in the home's policies directory,
// denying the two tools a headless run must not reach: invoke_agent, a
// subagent whose loop fires the hooks as the main agent's does, and
// enter_plan_mode, which asks nobody. A denied tool is not offered at all
// (measured on 0.70.0). Everything else stays: the run is the seat's coder.
const geminiPolicy = `# Written by Crewlet: tools a headless run must not reach.
[[rule]]
toolName = ["invoke_agent", "enter_plan_mode"]
decision = "deny"
priority = 999
`

// geminiHelperScript is gemini_helper.mjs, which a run's script runs to pin
// the project slug and read the run's spend off its session file.
// Embedded so the box runs the helper this build tests.
//
//go:embed gemini_helper.mjs
var geminiHelperScript []byte

var _ artifactHolder = GeminiCLI{}

// artifacts is what a Gemini CLI run leaves in the box's work directory, each
// written afresh by the next ([Runner.ClearArtifacts]). The home's settings,
// policy and project registry are rewritten by every Gemini CLI run and left
// alone by every other harness's, since the image may keep its own there.
func (GeminiCLI) artifacts(p Paths) (files, dirs []string) {
	return []string{p.GeminiHelper(), p.GeminiTake(), p.GeminiTakeConfig(), p.GeminiTakes(),
		p.GeminiDefaultSystem(), p.GeminiExportErr(), p.GeminiSystem()}, nil
}
