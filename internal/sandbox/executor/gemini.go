package executor

import (
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// GeminiCLIName is this runner's config name.
const GeminiCLIName = "gemini-cli"

// GeminiCLI drives Gemini CLI (`gemini`) headless, its event stream on stdout
// (measured on 0.70.0 against a stub of the Gemini API;
// docs/concepts/code-sandbox.md, "Gemini CLI", says what a run does).
type GeminiCLI struct{}

func init() { Register(GeminiCLI{}) }

// NewGeminiCLI is a runner driving Gemini CLI.
func NewGeminiCLI() *Runner { return New(GeminiCLI{}) }

// Name is this runner's config name.
func (GeminiCLI) Name() string { return GeminiCLIName }

// MCPCallCeiling is none: the server's `timeout` carries the bridge's own
// bound ([geminiMCP]), and a 1040-second call under a 1050-second one was
// answered whole (measured on 0.70.0).
func (GeminiCLI) MCPCallCeiling() time.Duration { return 0 }

// resumesOnLaunchRoute: measured on 0.70.0, the one release that continues a
// carried session ([launchRouted]): created with
// GOOGLE_GEMINI_BASE_URL at stub A, `--resume` with it at stub B; the resumed
// request (31267 bytes, the first run's prompt in it) reached B.
func (GeminiCLI) resumesOnLaunchRoute() bool { return true }

// Session is the session a Gemini CLI run works in ([Harness.Session]): the one
// it continues, or a new one under a uuid minted for it — Gemini CLI names a
// new session by any word of letters, digits, dashes and underscores
// (`--session-id`) and refuses one it already holds (exit 42) — and, for a run
// that continues one, a second uuid for the session it starts instead where
// its box holds that one neither way.
func (GeminiCLI) Session(req sandbox.RunRequest) Session {
	if req.Resume != "" {
		return Session{ID: req.Resume, Continues: true, Fresh: uuid.NewString(),
			Replaces: req.Carried != nil && req.Carried.Replace}
	}
	return Session{ID: uuid.NewString()}
}

// geminiFlags is what both the prompt's export and the run itself are started
// with: the export depends on the tools the approval mode offers, on the model
// family and on the servers admitted, so it is made with the run's own. No
// extension is loaded (`--extensions none`: an extension in the home adds its
// context file, skills and hooks, measured on 0.70.0), and each of the run's
// servers is admitted by name.
func geminiFlags(req sandbox.RunRequest) []string {
	flags := []string{"-p", `""`, "--output-format", "stream-json", "--approval-mode", "yolo",
		"--extensions", "none"}
	if req.Model != "" {
		flags = append(flags, "-m", shellword.Quote(req.Model))
	}
	for _, name := range slices.Sorted(maps.Keys(req.MCPServers)) {
		flags = append(flags, "--allowed-mcp-server-names", shellword.Quote(name))
	}
	return flags
}

// Command is the run's script ([geminiScript]).
func (GeminiCLI) Command(req sandbox.RunRequest, session Session, paths Paths, _ string) string {
	return "sh -c " + shellword.Quote(geminiScript(req, session, paths))
}

// geminiExportExit is what `gemini` exits with once GEMINI_WRITE_SYSTEM_MD
// has written its default prompt: it writes the file and stops before any
// model request (measured on 0.70.0, zero requests at the stub).
const geminiExportExit = 42

// geminiRelease is the Gemini CLI release this build drives: the newest, whose
// stream, chat file, hooks and resume it was measured against
// (testdata/gemini/0.70.0). An older one is refused at the run's start, a
// newer one runs on sessions of its own, and only this one continues a
// carried session ([releaseGate], [exactCarry]).
const geminiRelease = "0.70.0"

// geminiScript is a Gemini CLI run's script: gate the release ([releaseGate]),
// pin the slug, name and record the session ([chatSessionGate]), export the
// harness's prompt with the run's text after it, run the brief (stdin), append
// the run's own spend off its session file ([geminiUsageEvent]), and pack the
// session for another box ([chatPack]).
//
// THE ADD-TO CHANNEL IS BUILT, since Gemini CLI has none: GEMINI_SYSTEM_MD
// replaces its prompt, so the script has it write the prompt out
// (GEMINI_WRITE_SYSTEM_MD, byte for byte the systemInstruction it would send,
// tools, hook context and skills resolved) and names the export plus the run's
// text. Made per run, in the run's box with the run's flags, because the
// export depends on the box's home, skills, policy and model.
func geminiScript(req sandbox.RunRequest, session Session, paths Paths) string {
	flags := strings.Join(geminiFlags(req), " ")
	q := shellword.Quote
	var fresh []freshCopy
	if req.FreshWiring != nil {
		fresh = geminiFreshCopies(paths, geminiHooked(req))
	}
	lines := []string{
		// THE TELEMETRY'S NAMES GO before Gemini CLI starts at all, its release
		// read included: the run turns no Gemini CLI telemetry on, so it starts
		// without them ([otelExportUnset]). The sign-in's stay
		// ([sandbox.RunRequest.SignInEnv] is not applied): Gemini CLI signs in by
		// them, and offers no way to withhold a name from its tools alone but its
		// redaction, which would also take the seat's own token-named variables.
		otelExportUnset,
		// THE RELEASE, read once for the gate, the record and the session's,
		// before anything else: an older one never runs ([releaseGate]).
		releaseRead(GeminiCLI{}.VersionCommand()),
		releaseGate("Gemini CLI", geminiRelease),
		"node " + q(paths.GeminiHelper()) + " pin " + q(paths.GeminiProjects()) + " " + GeminiProjectSlug + " || exit 1",
		strings.TrimSuffix(chatSessionGate(session, paths, geminiGate(paths, fresh)), "\n"),
		// WHERE THE RUN BEGINS IN ITS SESSION, once the gate cut it back: its
		// lines and their digest ([chatRunStartFrom]).
		chatRunStartFrom,
		// THE RECORD, before the harness starts, so a run killed before it
		// reported anything still says which session it took up, and why it
		// began again where it did; the release last ([parseSessionRecord]).
		`{ printf 'id %s\ncontinued %s\nwhy %s\n' "$s" "$k" "$w"; ` + chatRecordFrom +
			`; printf 'release %s\n' "$v"; } > ` + q(paths.SessionRecord()),
	}
	// THE SESSION'S RECORD OF TAKES OPENS WITH ITS START LINE BEFORE THE RUN,
	// the count of the session's lines as the run starts ([geminiConfirmed]): a
	// record holding it settles a take never recorded (its hook killed first)
	// as absent once the job ends, and one without it, lost with its box, waits
	// (a line, as a box reads a missing file as empty). The count keeps a take
	// from being confirmed by a delivery an earlier job's run made.
	if req.Steer.Set() {
		lines = append(lines, `mkdir -p `+q(paths.GeminiTakesDir())+
			` && printf '{"from":%s}\n' "$n" >> `+q(paths.GeminiTakesDir())+`/"$s".jsonl || exit 1`)
	}
	run := `gemini ` + flags + ` "$@"; code=$?`
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
	before := q(paths.GeminiUsageBefore())
	return strings.Join(append(lines,
		// THE RUN'S OWN SPEND, NOT THE SESSION'S: the responses the chat file
		// holds as the run starts are listed first and left out of the sum
		// ([GeminiCLI.Parse]).
		// A LIST THAT FAILED IS NO FILE, never an empty one, which would charge the
		// earlier jobs' requests to this run: the usage line then says why it
		// read none ([geminiEvents.runSpend]).
		`rm -f `+before+`; if [ "$k" = yes ]; then node `+q(paths.GeminiHelper())+` ids `+q(paths.GeminiChats())+
			` "$s" > `+before+`.part 2> /dev/null && mv -f `+before+`.part `+before+`; else : > `+before+`; fi`,
		`if [ "$k" = yes ]; then set -- --resume "$s"; else set -- --session-id "$s"; fi`,
		run,
		"node "+q(paths.GeminiHelper())+" usage "+q(paths.GeminiChats())+` "$s" `+before,
		geminiLocate(paths),
		strings.TrimSuffix(chatPack(paths, packAt{staged: paths.SessionStaging(), dest: paths.SessionCapture()},
			paths.GeminiSlugDir()), "\n"),
		"exit $code"), "\n")
}

// geminiHooked reports whether a run of req starts the take script: for its
// notes or its skills' loads.
func geminiHooked(req sandbox.RunRequest) bool { return req.Steer.Set() || len(req.Skills) > 0 }

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
// a command line. Unset is GEMINI_CLI_HOME, which would send the run to
// another home; the prompt's variables are pinned ([GeminiCLI.Pins]).
func (GeminiCLI) Output(paths Paths) Output {
	return Output{Stdout: paths.Result(), Events: true, BriefOnStdin: true, Unset: []string{"GEMINI_CLI_HOME"}}
}

// geminiCodeAssist is Gemini CLI's own Code Assist endpoint, which a login's
// requests go to unless CODE_ASSIST_ENDPOINT names another (read from 0.70.0).
const geminiCodeAssist = "https://cloudcode-pa.googleapis.com"

// Pins are what a headless run needs whatever the image says: a trusted
// workspace (an untrusted one exits 55 before any request) and no browser (a
// login the run cannot use exits 41 at once instead of asking to open one and
// reading the brief as the answer; the profile's text phases measured both).
//
// AND EVERY VARIABLE THAT MOVES A REQUEST OR REPLACES THE PROMPT, set whether
// or not the entry names one: Gemini CLI fills each variable its environment
// lacks from an `.env` file. The base URLs keep the entry's value or are
// empty, which Gemini CLI reads as its own endpoint; the Code Assist endpoint
// is named, since an empty one is read as a URL; the prompt files are `false`
// until the script names its own (measured on 0.70.0).
func (GeminiCLI) Pins(_ context.Context, req sandbox.RunRequest, _ Paths) []Pinned {
	return []Pinned{{Name: "GEMINI_CLI_TRUST_WORKSPACE", Value: "true"}, {Name: "NO_BROWSER", Value: "true"},
		{Name: "GEMINI_SYSTEM_MD", Value: "false"}, {Name: "GEMINI_WRITE_SYSTEM_MD", Value: "false"},
		{Name: "CODE_ASSIST_ENDPOINT", Value: cmp.Or(req.Env["CODE_ASSIST_ENDPOINT"], geminiCodeAssist)},
		{Name: "GOOGLE_GEMINI_BASE_URL", Value: req.Env["GOOGLE_GEMINI_BASE_URL"]},
		{Name: "GOOGLE_VERTEX_BASE_URL", Value: req.Env["GOOGLE_VERTEX_BASE_URL"]}}
}

// routing answers [router] with the endpoints [GeminiCLI.Pins] fills from the
// entry's env.
func (GeminiCLI) routing() []routeVar {
	return routes(roleEndpoint, "CODE_ASSIST_ENDPOINT", "GOOGLE_GEMINI_BASE_URL", "GOOGLE_VERTEX_BASE_URL")
}

// readsCommands marks Gemini CLI as running a brief that begins with `/` as
// one of its commands (`/init` wrote a GEMINI.md, measured on 0.70.0).
func (GeminiCLI) readsCommands() {}

// geminiHome is what a Gemini CLI run may load from the box that its launch
// did not write ([runHome]): the home's skills (the run's own are written
// after), policies (the run's file is written after), commands, memory file
// and environment file. Extensions stay on disk, unloaded ([geminiFlags]);
// agents are off in the settings.
var geminiHome = runHome{State: ".gemini", Owned: []string{"skills", "policies", "commands", "GEMINI.md", ".env"},
	Memory: []string{"GEMINI.md"}, WalksEnv: true}

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
func (g GeminiCLI) WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, session Session,
	paths Paths) (string, error) {
	files := map[string][]byte{
		paths.GeminiHelper(): geminiHelperScript,
		paths.GeminiPolicy(): []byte(geminiPolicy),
	}
	if system := geminiSystemText(g, req); system != "" {
		files[paths.System()] = []byte(system)
	}
	hooked := geminiHooked(req)
	if hooked {
		files[paths.GeminiTake()] = geminiTakeScript
	}
	if err := geminiWired(files, req, paths, paths.GeminiSettings(), paths.GeminiTakeConfig(),
		paths.GeminiTakes(session.ID), hooked); err != nil {
		return "", err
	}
	// AND THE SAME FILES UNDER THE JOB'S OTHER BRIDGE TOKEN, for a session the
	// script starts in place of the continued one, beside the first
	// ([sandbox.RunRequest.FreshWiring], [chatSessionGate]): the take
	// configuration naming that session's record of takes. A wiring that
	// changes what the run is hooked for would need another settings file
	// than the one the script chose, so it is refused.
	if f := req.FreshWiring; f != nil {
		if f.Steer.Set() != req.Steer.Set() || f.SkillLoads.Set() != req.SkillLoads.Set() {
			return "", errors.New("executor: the run's two wirings, for the session it continues and for " +
				"one of its own, are not hooked alike, so the run cannot be wired to whichever session its " +
				"box starts")
		}
		rewired := req
		rewired.MCPServers, rewired.Steer, rewired.SkillLoads = f.MCPServers, f.Steer, f.SkillLoads
		if err := geminiWired(files, rewired, paths, paths.GeminiSettings()+".fresh",
			paths.GeminiTakeConfig()+".fresh", paths.GeminiTakes(session.Fresh), hooked); err != nil {
			return "", err
		}
	}
	for _, s := range req.Skills {
		file, fileErr := skillFile(geminiSkillName(s), s.Description, s.Body)
		if fileErr != nil {
			return "", fileErr
		}
		files[paths.GeminiSkills()+"/"+geminiSkillName(s)+"/SKILL.md"] = file
	}
	// EVERYTHING AN EARLIER RUN OR THE IMAGE LEFT GOES FIRST, the home's
	// skills and policies among it, so the run loads only what it writes.
	if err := geminiHome.prune(ctx, box, paths.Home, "Gemini CLI"); err != nil {
		return "", err
	}
	for path, content := range files {
		if err := box.WriteFile(ctx, path, content); err != nil {
			return "", fmt.Errorf("executor: writing %s for Gemini CLI: %w", path, err)
		}
	}
	return "", nil
}

// geminiWired puts into files the two files of req's wiring: its settings, at
// settingsPath, and where the run is hooked, its take script's configuration,
// at takePath, naming takes as the session's record of takes.
func geminiWired(files map[string][]byte, req sandbox.RunRequest, paths Paths, settingsPath, takePath,
	takes string, hooked bool) error {

	if hooked {
		take, err := geminiTakeConfig(req, takes)
		if err != nil {
			return err
		}
		files[takePath] = take
	}
	settings, err := json.MarshalIndent(geminiSettings(req, paths, hooked), "", "  ")
	if err != nil {
		return fmt.Errorf("executor: rendering Gemini CLI's settings: %w", err)
	}
	files[settingsPath] = settings
	return nil
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
	// run with a FatalTurnLimitedError, which reads as the cap
	// ([geminiTurnLimitError]).
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
				"command": "node " + shellword.Quote(paths.GeminiTake()) + " " + shellword.Quote(paths.GeminiTakeConfig()),
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
// note contract gives a hook (ADR-0177, section 2), twice the bridge's own
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

// artifactFiles is what a Gemini CLI run leaves in the box's work directory,
// and a fresh session's copy of its settings in the home, each written afresh
// by the next ([Runner.ClearArtifacts]): files alone, so it is no
// [artifactDirHolder]. The home's settings, policy and project
// registry are rewritten by every Gemini CLI run, its skills, policies and
// commands removed first, an image's included ([geminiHome]); every other
// harness's run leaves them alone.
func (GeminiCLI) artifactFiles(p Paths) []string {
	return []string{p.GeminiHelper(), p.GeminiTake(), p.GeminiTakeConfig(), p.GeminiTakeConfig() + ".fresh",
		p.GeminiSettings() + ".fresh", p.GeminiDefaultSystem(), p.GeminiExportErr(), p.GeminiSystem(),
		p.GeminiUsageBefore(), p.GeminiUsageBefore() + ".part"}
}
