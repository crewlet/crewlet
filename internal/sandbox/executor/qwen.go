package executor

import (
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

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// QwenCodeName is this runner's config name.
const QwenCodeName = "qwen-code"

// QwenCode drives Qwen Code (`qwen`) over ACP, its Agent Client Protocol
// mode, from a helper in the box (qwen_helper.mjs), measured on 0.28.3 and
// 0.26.1 against a stub of an OpenAI-compatible API
// (docs/concepts/code-sandbox.md, "Qwen Code", says what a run does). ACP
// rather than `-p`: only ACP has a mid-run route for a note that Qwen Code
// records out of tool output (craft/drainMidTurnQueue; ruling N3' of #1818).
type QwenCode struct{}

func init() { Register(QwenCode{}) }

// NewQwenCode is a runner driving Qwen Code.
func NewQwenCode() *Runner { return New(QwenCode{}) }

// Name is this runner's config name.
func (QwenCode) Name() string { return QwenCodeName }

// Session is the session a Qwen Code run works in ([Harness.Session]): the one
// it continues, or a new one under a uuid minted for it — Qwen Code takes the
// id a new ACP session is created under (session/new's `_meta`
// `qwen-code/sessionId`) and refuses one it already holds
// (session_id_conflict, measured on 0.28.3) — and, for a run that continues
// one, a second uuid for the session it starts instead where its box holds
// that one neither way.
func (QwenCode) Session(req sandbox.RunRequest) Session {
	if req.Resume != "" {
		return Session{ID: req.Resume, Continues: true, Fresh: uuid.NewString(),
			Replaces: req.Carried != nil && req.Carried.Replace}
	}
	return Session{ID: uuid.NewString()}
}

// Command is the run's script: the end protocol (end.go), then the helper as
// the run's harness, reading the brief on stdin ([QwenCode.Output]). An end
// request reaches the helper as SIGTERM, which it answers with
// session/cancel, never passing the signal to Qwen Code (qwen_helper.mjs).
func (QwenCode) Command(req sandbox.RunRequest, session Session, paths Paths, _ string) string {
	return "sh -c " + sandbox.ShellQuote(qwenScript(req, session, paths, sandbox.JobStopGrace))
}

// qwenRelease is the Qwen Code release this build drives: the newest, whose
// ACP messages, chat file and session/load it was measured against
// (testdata/qwen/0.28.3). An older one is refused at the run's start, a newer
// one runs on sessions of its own, and only this one continues a carried
// session ([releaseGate], [exactCarry]).
const qwenRelease = "0.28.3"

// qwenScript is a Qwen Code run's script, giving its helper stop to go once
// asked to end: the end protocol, the box's release read and gated
// ([releaseGate]), the session named and recorded ([chatSessionGate],
// [Paths.SessionRecord]), the helper run on it, loading a session the run
// continues and creating one it starts, and the session packed to be
// continued in another box ([chatPack]). The session goes on the helper's
// command line, as the gate settles it; an id is no secret.
func qwenScript(req sandbox.RunRequest, session Session, paths Paths, stop time.Duration) string {
	var b strings.Builder
	line := func(text string) { b.WriteString(text + "\n") }
	q := sandbox.ShellQuote
	var fresh []freshCopy
	if req.FreshWiring != nil {
		fresh = qwenFreshCopies(paths)
	}
	b.WriteString(endPrologue(paths, stop))
	b.WriteString(endHarness(""))
	// THE RELEASE, read once for the gate, the record and the session's: an
	// older one never runs ([releaseGate]).
	line(`v=$(qwen --version < /dev/null 2> /dev/null | head -n 1)`)
	line(releaseGate("Qwen Code", qwenRelease))
	b.WriteString(chatSessionGate(session, paths, qwenGate(paths, fresh)))
	// THE RECORD, before the harness starts ([parseSessionRecord]).
	line(`printf 'id %s\ncontinued %s\nwhy %s\nrelease %s\n' "$s" "$k" "$w" "$v" > ` + q(paths.SessionRecord()))
	// WHERE THE RUN BEGINS IN ITS SESSION, which its notes are confirmed past: a
	// continued session holds an earlier job's records of notes, which a note
	// of the same text this run is handed was never delivered as.
	line(`n=0; [ -z "$cf" ] || n=$(wc -l < "$cf" | tr -d ' '); printf '%s %s\n' "$s" "$n" > ` + q(paths.QwenRunStart()))
	line(`if [ "$k" = yes ]; then m=load; else m=new; fi`)
	b.WriteString(launchHarness("node "+q(paths.QwenHelper())+" run "+q(paths.QwenHelperConfig())+` "$s" "$m"`, paths))
	line(qwenLocate(paths))
	b.WriteString(chatPack(paths, packAt{staged: paths.SessionStaging(), dest: paths.SessionCapture()}, paths.QwenDir()))
	line(`exit "$code"`)
	return b.String()
}

// Output is the helper's stream on stdout, which is also the result: the
// session/update lines Qwen Code sent and the helper's own, ending with how
// the turn ended ([qwenEndEvent]). The brief goes on stdin, which the helper
// reads and sends as the turn's prompt. Unset is what would move or replace
// what the run depends on: another home (QWEN_HOME, QWEN_RUNTIME_DIR) would
// hide the settings and the session from the helper, QWEN_SYSTEM_MD replaces
// the prompt the run's text is added to, OPENAI_API_KEY would reach the
// model's shell (the key is in the settings, [qwenSettings]), and
// QWEN_CODE_UNATTENDED_RETRY retries a refused request for up to six hours.
func (QwenCode) Output(paths Paths) Output {
	return Output{Stdout: paths.Result(), Events: true, BriefOnStdin: true,
		Unset: []string{"QWEN_HOME", "QWEN_RUNTIME_DIR", "QWEN_SYSTEM_MD", "OPENAI_API_KEY",
			"QWEN_CODE_UNATTENDED_RETRY"}}
}

// Pins are what a run needs whatever the image says: the run's text in place
// of Qwen Code's opening identity sentence, the rest of its prompt kept
// (QWEN_SYSTEM_IDENTITY_MD, the profile's channel, measured under ACP on
// 0.28.3); no catalogue fetch from models.dev, a connection of its own every
// run; and the entry's model, which Qwen Code reads from OPENAI_MODEL under
// its OpenAI sign-in (as measured under ACP).
func (QwenCode) Pins(_ context.Context, req sandbox.RunRequest, paths Paths) []Pinned {
	pins := []Pinned{{Name: "QWEN_SYSTEM_IDENTITY_MD", Value: paths.System()}, {Name: "QWEN_CODE_MODELS_DEV", Value: "off"}}
	if req.Model != "" {
		pins = append(pins, Pinned{Name: "OPENAI_MODEL", Value: req.Model})
	}
	return pins
}

// Events is a new decoder for one stream ([qwenEvents]).
func (QwenCode) Events() Decoder { return &qwenEvents{} }

// Parse decodes a whole stream: the result of a run whose result is its
// stream.
func (QwenCode) Parse(stdout []byte) sandbox.Result { return decodeAll(&qwenEvents{}, stdout) }

// artifacts is what a Qwen Code run leaves in a box beside the shared
// artefacts ([artifactHolder]): its settings, which hold its key, the helper
// and its configuration, and its skills.
func (QwenCode) artifacts(p Paths) (files, dirs []string) {
	return []string{p.QwenSettings(), p.QwenSettings() + ".fresh", p.QwenHelper(), p.QwenHelperConfig(),
		p.QwenHelperConfig() + ".fresh", p.QwenRunStart()}, []string{p.QwenSkills()}
}

// WriteConfig writes what a run's helper and harness read: the run's system
// text, the helper and its configuration, the settings in the box home, and
// the run's skills. It returns "": Qwen Code reads its settings from its
// home, and the helper hands it no MCP servers of its own.
func (q QwenCode) WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, _ Session,
	paths Paths) (string, error) {
	system := req.System
	if req.Steer.Set() {
		system = withNoteInstruction(system, q.NoteInstruction())
	}
	files := map[string][]byte{
		paths.System():     []byte(system),
		paths.QwenHelper(): qwenHelperScript,
	}
	if err := qwenWired(files, req, paths, paths.QwenSettings(), paths.QwenHelperConfig()); err != nil {
		return "", err
	}
	// AND THE SAME FILES UNDER THE JOB'S OTHER BRIDGE TOKEN, for a session the
	// script starts in place of the continued one, beside the first
	// ([sandbox.RunRequest.FreshWiring], [chatSessionGate]). A wiring that
	// changes what the run is wired for would need settings the script did not
	// choose, so it is refused.
	if f := req.FreshWiring; f != nil {
		if f.Steer.Set() != req.Steer.Set() || f.SkillLoads.Set() != req.SkillLoads.Set() {
			return "", errors.New("executor: the run's two wirings, for the session it continues and for " +
				"one of its own, are not wired alike, so the run cannot be wired to whichever session its " +
				"box starts")
		}
		rewired := req
		rewired.MCPServers, rewired.Steer, rewired.SkillLoads = f.MCPServers, f.Steer, f.SkillLoads
		if err := qwenWired(files, rewired, paths, paths.QwenSettings()+".fresh",
			paths.QwenHelperConfig()+".fresh"); err != nil {
			return "", err
		}
	}
	for _, s := range req.Skills {
		file, fileErr := skillFile(qwenSkillName(s), s.Description, s.Body)
		if fileErr != nil {
			return "", fileErr
		}
		files[paths.QwenSkills()+"/"+qwenSkillName(s)+"/SKILL.md"] = file
	}
	for _, path := range slices.Sorted(maps.Keys(files)) {
		if err := box.WriteFile(ctx, path, files[path]); err != nil {
			return "", fmt.Errorf("executor: writing %s for Qwen Code: %w", path, err)
		}
	}
	return "", nil
}

// qwenWired puts into files the two files of req's wiring: its settings, at
// settingsPath, and its helper's configuration, at helperPath.
func qwenWired(files map[string][]byte, req sandbox.RunRequest, paths Paths, settingsPath, helperPath string) error {
	helper, err := json.Marshal(qwenHelperConfigOf(req, paths))
	if err != nil {
		return fmt.Errorf("executor: rendering the Qwen Code helper's configuration: %w", err)
	}
	settings, err := json.MarshalIndent(qwenSettings(req, paths), "", "  ")
	if err != nil {
		return fmt.Errorf("executor: rendering Qwen Code's settings: %w", err)
	}
	files[helperPath], files[settingsPath] = helper, settings
	return nil
}

// qwenSkillName is the name a run's skill is written and listed under:
// [sandbox.Skill.Listed] with its colon a dash, a name of the shape Qwen Code
// was measured listing and loading (0.28.3); the helper maps it back as it
// reports a load.
func qwenSkillName(s sandbox.Skill) string { return sandbox.SkillNamespace + "-" + s.Name }

// qwenDenied is every tool of Qwen Code's a headless run must not reach,
// each removed outright by `permissions.deny` (measured under ACP on 0.28.3:
// none of the four offered): agent, a subagent whose loop Qwen Code never
// asks for notes in; ask_user_question, which asks nobody; and plan mode,
// which stops the run to wait for an approval nobody gives.
var qwenDenied = []string{"agent", "ask_user_question", "enter_plan_mode", "exit_plan_mode"}

// qwenSettings is the run's ~/.qwen/settings.json:
//   - the key, read by the OpenAI sign-in, from the settings rather than
//     OPENAI_API_KEY, which every shell of the run would inherit;
//   - every MCP server, each tool offered from the start ([qwenMCP]);
//   - off: Qwen Code's own co-author trailer (the run's git hook credits a
//     commit, attribution.go), the background memory requests and the
//     follow-up suggestion after a turn, usage statistics and the update
//     check (two connections home a run otherwise);
//   - the chat file kept, since a note is confirmed by its record there;
//   - the tools denied ([qwenDenied]);
//   - the run's skills alone, with the hook reporting each load.
func qwenSettings(req sandbox.RunRequest, paths Paths) map[string]any {
	settings := map[string]any{
		"general": map[string]any{
			"gitCoAuthor":      map[string]any{"commit": false, "pr": false},
			"enableAutoUpdate": false,
			"chatRecording":    true,
		},
		"memory": map[string]any{"enableManagedAutoMemory": false, "enableManagedAutoDream": false},
		// No follow-up suggestion: over ACP, 0.28.3 sends one more request
		// after a finished turn to suggest the user's next prompt, whose
		// counts no session/update carries (measured against the stub).
		"ui":          map[string]any{"enableFollowupSuggestions": false},
		"privacy":     map[string]any{"usageStatisticsEnabled": false},
		"permissions": map[string]any{"deny": qwenDenied},
		// A 429 reaches the run's end in about a minute (seven attempts, the
		// SDK's own backoff), as the text phases set it (the qwen-code
		// profile): Qwen Code's own policy holds a refused request for most
		// of an hour.
		"model": map[string]any{"generationConfig": map[string]any{"maxRetries": 0}},
	}
	if key := req.Env["OPENAI_API_KEY"]; key != "" {
		settings["security"] = map[string]any{"auth": map[string]any{"apiKey": key}}
	}
	if len(req.MCPServers) > 0 {
		servers := make(map[string]any, len(req.MCPServers))
		for name, s := range req.MCPServers {
			servers[name] = qwenMCP(s)
		}
		settings["mcpServers"] = servers
	}
	// ONLY THE RUN'S SKILLS: "user" stays enabled, since a directory the
	// settings name is listed at that level (measured: disabling it lists
	// nothing), and the home's own skills directories are left empty.
	skills := map[string]any{"disabledLevels": []string{"project", "extension", "bundled"}}
	if len(req.Skills) > 0 {
		skills["directories"] = []string{paths.QwenSkills()}
		settings["hooks"] = map[string]any{"PostToolUse": []any{map[string]any{
			"matcher": "skill",
			"hooks": []any{map[string]any{
				"type": "command", "timeout": int(qwenHookTimeout / time.Second),
				"command": "node " + sandbox.ShellQuote(paths.QwenHelper()) + " skill " +
					sandbox.ShellQuote(paths.QwenHelperConfig()),
			}},
		}}}
	}
	settings["skills"] = skills
	return settings
}

// qwenHookTimeout bounds one firing of the skill hook, in Qwen Code's whole
// seconds: the ten a hook is given in every runner (geminiHookTimeout), far
// past one POST to the skill route; Qwen Code goes on past a hook it kills.
const qwenHookTimeout = 10 * time.Second

// qwenMCP is one server in Qwen Code's settings vocabulary: `httpUrl` and
// `headers` for streamable HTTP (`url` is its SSE form, which never
// connected), `command`, `args` and `env` for stdio; `timeout` in
// milliseconds where the engine sets a wait, so a question the seat parks on
// is held to the engine's hold rather than Qwen Code's ten minutes; and
// `alwaysLoadTools` for a server whose tools are offered on every request,
// which 0.28.3 otherwise hides behind its tool search and refuses a direct
// call to.
func qwenMCP(s sandbox.MCPServer) map[string]any {
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
	if ms := waitMillis(s.CallTimeout); ms > 0 {
		out["timeout"] = ms
	}
	if s.AlwaysLoaded {
		out["alwaysLoadTools"] = true
	}
	return out
}

// qwenHelperConfig is what the helper reads ([Paths.QwenHelperConfig]).
type qwenHelperConfig struct {
	// AuthType is the sign-in Qwen Code is started with (`--auth-type`).
	AuthType string `json:"authType"`
	// QwenHome is where Qwen Code records the session.
	QwenHome string `json:"qwenHome"`
	// Servers are the run's MCP servers, waited for before the prompt; Bridge
	// is the one without which the run is not started.
	Servers   []string `json:"servers"`
	Bridge    string   `json:"bridge,omitempty"`
	MCPWaitMs int64    `json:"mcpWaitMs"`
	// Notes is the run's notes route, Skills its skill route, each "" for none.
	Notes     string `json:"notes,omitempty"`
	Skills    string `json:"skills,omitempty"`
	Namespace string `json:"namespace"`
}

// qwenMCPWait bounds the wait for the run's MCP servers to connect before its
// prompt goes in: the bridge connected 0.4 to 0.7 s after session/new
// (measured on 0.28.3), and sixty seconds is the room an OpenCode run's
// servers get for the same bridge (openCodeMCPWait), short of holding a seat
// for a bridge that is not coming.
const qwenMCPWait = 60 * time.Second

// qwenHelperConfigOf is req's [qwenHelperConfig].
func qwenHelperConfigOf(req sandbox.RunRequest, paths Paths) qwenHelperConfig {
	c := qwenHelperConfig{
		AuthType:  qwenAuthType(req),
		QwenHome:  paths.QwenDir(),
		Servers:   slices.Sorted(maps.Keys(req.MCPServers)),
		MCPWaitMs: qwenMCPWait.Milliseconds(),
		Namespace: sandbox.SkillNamespace,
	}
	if _, ok := req.MCPServers[config.ToolServerName]; ok {
		c.Bridge = config.ToolServerName
	}
	if req.Steer.Set() {
		c.Notes = req.Steer.URL
	}
	if req.SkillLoads.Set() {
		c.Skills = req.SkillLoads.URL
	}
	return c
}

// Qwen Code's two sign-ins a run takes: its OpenAI-compatible one, the key
// from the settings and the endpoint from OPENAI_BASE_URL, as the profile's
// text phases sign in; and its paid login, read from the home's
// subscription_credentials.json, its endpoint from QWEN_SUBSCRIPTION_BASE_URL
// where set (0.28's own names, NOT MEASURED against Qwen's service).
const (
	qwenAuthOpenAI       = "openai"
	qwenAuthSubscription = "qwen-subscription"
)

// qwenAuthType is the sign-in req's run takes: the key where it is handed
// one, else the login the box holds.
func qwenAuthType(req sandbox.RunRequest) string {
	if req.Env["OPENAI_API_KEY"] != "" {
		return qwenAuthOpenAI
	}
	return qwenAuthSubscription
}

// qwenHelperScript is qwen_helper.mjs. Embedded so the box runs the helper
// this build tests.
//
//go:embed qwen_helper.mjs
var qwenHelperScript []byte
