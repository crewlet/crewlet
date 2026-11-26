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

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// QwenCodeName is this runner's config name.
const QwenCodeName = "qwen-code"

// QwenCode drives Qwen Code (`qwen`) over ACP, its Agent Client Protocol
// mode, from a helper in the box (qwen_helper.mjs), measured on 0.28.4
// against a stub of an OpenAI-compatible API
// (docs/concepts/code-sandbox.md, "Qwen Code", says what a run does). ACP
// rather than `-p`: only ACP has a mid-run route for a note that Qwen Code
// records out of tool output (craft/drainMidTurnQueue; ruling N3' of #1818).
type QwenCode struct{}

func init() { Register(QwenCode{}) }

// NewQwenCode is a runner driving Qwen Code.
func NewQwenCode() *Runner { return New(QwenCode{}) }

// Name is this runner's config name.
func (QwenCode) Name() string { return QwenCodeName }

// MCPCallCeiling is none: the server's `timeout` carries the bridge's own
// bound ([qwenMCP]), and a 1040-second call under a 1050-second one was
// answered whole (measured on 0.28.4).
func (QwenCode) MCPCallCeiling() time.Duration { return 0 }

// resumesOnLaunchRoute: measured on 0.28.4, the one release that continues a
// carried session ([launchRouted]): created over ACP
// with OPENAI_BASE_URL at stub A, `session/load` and `session/prompt` with it
// at stub B; all three resumed requests (61873 bytes the first, the first run's
// prompt in each) reached B.
func (QwenCode) resumesOnLaunchRoute() bool { return true }

// Session is the session a Qwen Code run works in ([Harness.Session]): the one
// it continues, or a new one under a uuid minted for it — Qwen Code takes the
// id a new ACP session is created under (session/new's `_meta`
// `qwen-code/sessionId`) and refuses one it already holds
// (session_id_conflict, measured on 0.28.4) — and, for a run that continues
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
	return "sh -c " + shellword.Quote(qwenScript(req, session, paths, sandbox.JobStopGrace))
}

// qwenRelease is the Qwen Code release this build drives: the newest, whose
// ACP messages, chat file and session/load it was measured against
// (testdata/qwen/0.28.4). An older one is refused at the run's start, a newer
// one runs on sessions of its own, and only this one continues a carried
// session ([releaseGate], [exactCarry]).
const qwenRelease = "0.28.4"

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
	q := shellword.Quote
	var fresh []freshCopy
	if req.FreshWiring != nil {
		fresh = qwenFreshCopies(paths)
	}
	b.WriteString(endPrologue(paths, stop))
	b.WriteString(endHarness(""))
	// THE RELEASE, read once for the gate, the record and the session's: an
	// older one never runs ([releaseGate]).
	line(releaseRead(QwenCode{}.VersionCommand()))
	line(releaseGate("Qwen Code", qwenRelease))
	// THE CHECKOUTS' INSTRUCTIONS AND SKILLS, and hk where the helper ran
	// ([qwenCheckoutsGate]).
	b.WriteString(qwenCheckoutsGate(paths))
	// NOTHING OF CREWLET'S OWN SWITCHED OFF by a list no tier undoes
	// ([qwenGuard]), checked before Qwen Code starts; the checkouts' hook
	// switched off only said in the skip log (D709).
	line("qk=" + q(qwenCheckoutsHook) + "; qs=" + q(QwenCode{}.checkoutHome(paths).skipped()))
	line("node " + q(paths.QwenHelper()) + " guard" + qwenGuard(req) +
		` ${hk:+--checkouts "$qk" --skipped "$qs"} < /dev/null || exit 1`)
	b.WriteString(chatSessionGate(session, paths, qwenGate(paths, fresh)))
	// WHERE THE RUN BEGINS IN ITS SESSION, its lines and their digest
	// ([chatRunStartFrom]), which its notes are confirmed past too: a continued
	// session holds an earlier job's records of notes, which a note of the
	// same text this run is handed was never delivered as.
	line(chatRunStartFrom)
	// THE RECORD, before the harness starts ([parseSessionRecord]).
	line(`{ printf 'id %s\ncontinued %s\nwhy %s\n' "$s" "$k" "$w"; ` + chatRecordFrom +
		`; printf 'release %s\n' "$v"; } > ` + q(paths.SessionRecord()))
	line(`printf '%s %s\n' "$s" "$n" > ` + q(paths.QwenRunStart()))
	line(`if [ "$k" = yes ]; then m=load; else m=new; fi`)
	// ITS CHAT FILE, named for the live count once Qwen Code writes it ([qwenPointer]).
	b.WriteString(qwenPointer(paths) + "\n")
	b.WriteString(launchHarness("node "+q(paths.QwenHelper())+" run "+q(paths.QwenHelperConfig())+` "$s" "$m"`, paths))
	line(`kill "$qwp" 2> /dev/null`)
	line(qwenLocate(paths))
	b.WriteString(chatCapture(paths, paths.QwenDir(), QwenCode{}))
	line(`exit "$code"`)
	return b.String()
}

// qwenPointer starts, in the background as qwp, a watcher that names in
// [Paths.CountRecord] the run's session chat file once Qwen Code writes it, as
// [qwenLocate] finds it, with its lines as the run began (n,
// [chatRunStartFrom]), for the live count ([qwenChatRecord]).
func qwenPointer(paths Paths) string {
	q := shellword.Quote
	pointer := q(paths.CountRecord())
	return `( while kill -0 $$ 2> /dev/null; do for f in ` + q(paths.QwenDir()) + `/projects/*/chats/"$s".jsonl; do ` +
		`if [ -s "$f" ] && [ ! -L "$f" ]; then printf '%s\n%s\n' "$f" "$n" > ` + pointer + `.next && mv -f ` +
		pointer + `.next ` + pointer + `; exit 0; fi; done; sleep 0.1; done ) & qwp=$!`
}

// Output is the helper's stream on stdout, which is also the result: the
// session/update lines Qwen Code sent and the helper's own, ending with how
// the turn ended ([qwenEndEvent]). The brief goes on stdin, which the helper
// reads and sends as the turn's prompt. Unset is what would move what the
// run depends on: another home (QWEN_HOME, QWEN_RUNTIME_DIR) would hide the
// settings and the session from the helper, QWEN_CODE_UNATTENDED_RETRY
// retries a refused request for up to six hours, and QWEN_CODE_SAFE_MODE
// switches every hook off (measured on 0.28.4), as Claude Code's own safe mode
// does ([claudeSwitchesOff]); a `.env` or a settings `env` setting it is
// refused instead ([qwenGuard]).
// The prompt's variables are pinned ([QwenCode.Pins]).
func (QwenCode) Output(paths Paths) Output {
	return Output{Stdout: paths.Result(), Events: true, BriefOnStdin: true,
		Unset: []string{"QWEN_HOME", "QWEN_RUNTIME_DIR", "QWEN_CODE_UNATTENDED_RETRY", "QWEN_CODE_SAFE_MODE"}}
}

// qwenDashScope is the endpoint Qwen Code's OpenAI-compatible sign-in sends a
// key to when OPENAI_BASE_URL names none (measured on 0.28.4).
const qwenDashScope = "https://dashscope.aliyuncs.com/compatible-mode/v1"

// Pins are what a run needs whatever the image says: the run's text in place
// of Qwen Code's opening identity sentence, the rest of its prompt kept
// (QWEN_SYSTEM_IDENTITY_MD, the profile's channel, measured under ACP on
// 0.28.4); no catalogue fetch from models.dev, a connection of its own every
// run; and the entry's model, which Qwen Code reads from OPENAI_MODEL under
// its OpenAI sign-in (as measured under ACP).
//
// AND THE VARIABLES THAT MOVE THE KEY OR REPLACE THE PROMPT, set whether or
// not the entry names one, since Qwen Code fills each variable its environment
// lacks from an `.env` file: OPENAI_BASE_URL the entry's or Qwen Code's own
// (its subscription sign-in reads none), the prompt files off (measured on
// 0.28.4); and the run's settings as its system tier ([Paths.QwenSettings]).
func (QwenCode) Pins(_ context.Context, req sandbox.RunRequest, paths Paths) []Pinned {
	pins := []Pinned{{Name: "QWEN_SYSTEM_IDENTITY_MD", Value: paths.System()}, {Name: "QWEN_CODE_MODELS_DEV", Value: "off"},
		{Name: "QWEN_SYSTEM_MD", Value: "false"}, {Name: "QWEN_WRITE_SYSTEM_MD", Value: "false"},
		{Name: "OPENAI_BASE_URL", Value: cmp.Or(req.Env["OPENAI_BASE_URL"], qwenDashScope)},
		{Name: "QWEN_CODE_SYSTEM_SETTINGS_PATH", Value: paths.QwenSettings()}}
	if req.Model != "" {
		pins = append(pins, Pinned{Name: "OPENAI_MODEL", Value: req.Model})
	}
	return pins
}

// routing answers [router] with the endpoint [QwenCode.Pins] fills from the
// entry's env.
func (QwenCode) routing() []routeVar { return routes(roleEndpoint, "OPENAI_BASE_URL") }

// Events is a new decoder for one stream ([qwenEvents]).
func (QwenCode) Events() Decoder { return &qwenEvents{} }

// Parse decodes a whole stream: the result of a run whose result is its
// stream.
func (QwenCode) Parse(stdout []byte) sandbox.Result { return decodeAll(&qwenEvents{}, stdout) }

// readsCommands marks Qwen Code as running a prompt that begins with `/` as one
// of its commands (`/init` wrote a QWEN.md over ACP, measured on 0.28.4).
func (QwenCode) readsCommands() {}

var (
	_ artifactHolder    = QwenCode{}
	_ artifactDirHolder = QwenCode{}
)

// artifactFiles is what a Qwen Code run leaves in a box beside the shared
// artefacts ([artifactHolder]): its settings ([Paths.QwenSettings], in its
// work directory, never the home's own), which hold its key, the helper
// and its configuration, a fresh session's copies of both, and where the run
// began in its session.
func (QwenCode) artifactFiles(p Paths) []string {
	return []string{p.QwenSettings(), p.QwenSettings() + ".fresh", p.QwenHelper(), p.QwenHelperConfig(),
		p.QwenHelperConfig() + ".fresh", p.QwenRunStart(), p.QwenNoteHelper(), p.QwenCheckoutLaunch(),
		p.QwenCheckoutSections(), p.QwenContextHomes()}
}

// artifactDirs is the directory a Qwen Code run leaves of its own
// ([artifactDirHolder]): its skills, removed whole.
func (QwenCode) artifactDirs(p Paths) []string { return []string{p.QwenSkills()} }

// WriteConfig writes what a run's helper and harness read: the run's system
// text, the helper and its configuration, the run's settings
// ([Paths.QwenSettings]) and its skills, all in its work directory, so the
// home is the person's and the image's as it stands. It returns "": Qwen Code
// finds its settings by a variable, and the helper hands it no MCP servers.
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
	// A FRESH WIRING THAT CHANGES WHAT THE RUN IS WIRED FOR would need settings
	// the script did not choose, so it is refused, before anything touches the
	// box.
	if f := req.FreshWiring; f != nil && (f.Steer.Set() != req.Steer.Set() || f.SkillLoads.Set() != req.SkillLoads.Set()) {
		return "", errors.New("executor: the run's two wirings, for the session it continues and for " +
			"one of its own, are not wired alike, so the run cannot be wired to whichever session its " +
			"box starts")
	}
	for _, s := range req.Skills {
		file, fileErr := skillFile(qwenSkillName(s), s.Description, s.Body)
		if fileErr != nil {
			return "", fileErr
		}
		files[paths.QwenSkills()+"/"+qwenSkillName(s)+"/SKILL.md"] = file
	}
	// THE NOTE HELPER'S WRITE IS THE FIRST COMMAND, after every refusal above.
	helped, err := writeQwenCheckouts(ctx, box, req, paths)
	if err != nil {
		return "", err
	}
	if err = qwenWired(files, req, paths, paths.QwenSettings(), paths.QwenHelperConfig(), helped); err != nil {
		return "", err
	}
	// AND THE SAME FILES UNDER THE JOB'S OTHER BRIDGE TOKEN, for a session the
	// script starts in place of the continued one, beside the first
	// ([sandbox.RunRequest.FreshWiring], [chatSessionGate]).
	if f := req.FreshWiring; f != nil {
		rewired := req
		rewired.MCPServers, rewired.Steer, rewired.SkillLoads = f.MCPServers, f.Steer, f.SkillLoads
		if err = qwenWired(files, rewired, paths, paths.QwenSettings()+".fresh",
			paths.QwenHelperConfig()+".fresh", helped); err != nil {
			return "", err
		}
	}
	for _, path := range slices.Sorted(maps.Keys(files)) {
		if err = box.WriteFile(ctx, path, files[path]); err != nil {
			return "", fmt.Errorf("executor: writing %s for Qwen Code: %w", path, err)
		}
	}
	return "", nil
}

// qwenWired puts into files the two files of req's wiring: its settings, at
// settingsPath, and its helper's configuration, at helperPath.
func qwenWired(files map[string][]byte, req sandbox.RunRequest, paths Paths, settingsPath, helperPath string,
	helped bool) error {
	helper, err := json.Marshal(qwenHelperConfigOf(req, paths))
	if err != nil {
		return fmt.Errorf("executor: rendering the Qwen Code helper's configuration: %w", err)
	}
	settings, err := json.MarshalIndent(qwenSettings(req, paths, helped), "", "  ")
	if err != nil {
		return fmt.Errorf("executor: rendering Qwen Code's settings: %w", err)
	}
	files[helperPath], files[settingsPath] = helper, settings
	return nil
}

// qwenSkillName is the name a run's skill is written and listed under:
// [sandbox.Skill.Listed] with its colon a dash, a name of the shape Qwen Code
// was measured listing and loading (0.28.4); the helper maps it back as it
// reports a load.
func qwenSkillName(s sandbox.Skill) string { return sandbox.SkillNamespace + "-" + s.Name }

// qwenDenied is every tool of Qwen Code's a headless run must not reach,
// each removed outright by `permissions.deny` (measured under ACP on 0.28.4:
// none of the four offered): agent, a subagent whose loop Qwen Code never
// asks for notes in; ask_user_question, which asks nobody; and plan mode,
// which stops the run to wait for an approval nobody gives.
var qwenDenied = []string{"agent", "ask_user_question", "enter_plan_mode", "exit_plan_mode"}

// qwenSettings is the run's settings, Qwen Code's system tier
// ([Paths.QwenSettings]):
//   - the key, read by the OpenAI sign-in, from the settings, OPENAI_API_KEY
//     staying in the run's environment as well;
//   - every MCP server, each tool offered from the start ([qwenMCP]);
//   - off: Qwen Code's own co-author trailer (the run's git hook credits a
//     commit, attribution.go), the background memory requests, the follow-up
//     suggestion, usage statistics and the update check (two connections home);
//   - the chat file kept, since a note is confirmed by its record there;
//   - the tools denied ([qwenDenied]); the round cap, as model.maxSessionTurns;
//   - the run's skills beside every other level's, with the hook reporting
//     each load.
func qwenSettings(req sandbox.RunRequest, paths Paths, helped bool) map[string]any {
	settings := map[string]any{
		"general": map[string]any{
			"gitCoAuthor":      map[string]any{"commit": false, "pr": false},
			"enableAutoUpdate": false,
			"chatRecording":    true,
		},
		"memory": map[string]any{"enableManagedAutoMemory": false, "enableManagedAutoDream": false},
		// No follow-up suggestion: over ACP, 0.28.4 sends one more request
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
	// THE RUN'S ROUND CAP, as Qwen Code caps a session's model requests: at
	// the cap the turn ends with ACP's stopReason max_turn_requests (measured
	// on 0.28.4: a cap of 2 and of 3 ended after 2 and 3 requests, a run done
	// in 3 under a cap of 5 ended end_turn). A run is a session of its own, so
	// the session's count is the run's.
	if req.Limits.MaxTurns > 0 {
		settings["model"].(map[string]any)["maxSessionTurns"] = req.Limits.MaxTurns
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
	// THE RUN'S SKILLS BESIDE EVERY OTHER LEVEL'S: Qwen Code unions skill
	// directories across its tiers, so the home's own stay listed.
	var used []any
	if len(req.Skills) > 0 {
		settings["skills"] = map[string]any{"directories": []string{paths.QwenSkills()}}
		used = append(used, map[string]any{
			"matcher": "skill",
			"hooks": []any{map[string]any{
				"type": "command", "timeout": int(qwenHookTimeout / time.Second),
				"command": "node " + shellword.Quote(paths.QwenHelper()) + " skill " +
					shellword.Quote(paths.QwenHelperConfig()),
			}},
		})
	}
	if helped {
		used = append(used, qwenCheckoutsEntry(paths))
	}
	if len(used) > 0 {
		settings["hooks"] = map[string]any{"PostToolUse": used}
	}
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
// which 0.28.4 otherwise hides behind its tool search (a request offered
// tool_search and tool_call, none of the server's tools; measured).
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
	Servers []string `json:"servers"`
	Bridge  string   `json:"bridge,omitempty"`
	// BridgeURL is the only address Bridge may be served from: Qwen Code
	// 0.28.4 merges servers by name, a system or repository settings file's
	// over the home's, so another `crewlet` would serve the bridge's names
	// unseen. Its status answer gives the connected server's configuration
	// and origin, which the helper holds to this before the prompt (its
	// `mcp__<server>__<tool>` grammar lets no other name take a bridge
	// tool's, [harnessprofile.BridgeToolName]).
	BridgeURL string `json:"bridgeURL,omitempty"`
	MCPWaitMs int64  `json:"mcpWaitMs"`
	// Notes is the run's notes route, Skills its skill route, each "" for none.
	Notes     string `json:"notes,omitempty"`
	Skills    string `json:"skills,omitempty"`
	Namespace string `json:"namespace"`
}

// qwenMCPWait bounds the wait for the run's MCP servers to connect before its
// prompt goes in: a server the settings name connected 0.3 s after
// session/new (measured on 0.28.4), and sixty seconds is the room an OpenCode
// run's servers get for the same bridge (openCodeMCPWait), short of holding a
// seat for a bridge that is not coming.
const qwenMCPWait = 60 * time.Second

// qwenHelperConfigOf is req's [qwenHelperConfig].
func qwenHelperConfigOf(req sandbox.RunRequest, paths Paths) qwenHelperConfig {
	c := qwenHelperConfig{
		AuthType:  qwenSignIn(req.Env),
		QwenHome:  paths.QwenDir(),
		Servers:   slices.Sorted(maps.Keys(req.MCPServers)),
		MCPWaitMs: qwenMCPWait.Milliseconds(),
		Namespace: sandbox.SkillNamespace,
	}
	if bridge, ok := req.MCPServers[config.ToolServerName]; ok {
		c.Bridge, c.BridgeURL = config.ToolServerName, bridge.URL
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
// subscription_credentials.json, its endpoint a constant of the bundle
// (0.28.4's own names, NOT MEASURED against Qwen's service).
const (
	qwenAuthOpenAI       = "openai"
	qwenAuthSubscription = "qwen-subscription"
)

// qwenSignIn is the sign-in a run handed env takes: the key where it is handed
// one, else the login the box holds. THE ONE RULE the run's settings and its
// box's point read ([QwenCode.readBoxPoint]) share.
func qwenSignIn(env map[string]string) string {
	if env["OPENAI_API_KEY"] != "" {
		return qwenAuthOpenAI
	}
	return qwenAuthSubscription
}

// qwenHelperScript is qwen_helper.mjs. Embedded so the box runs the helper
// this build tests.
//
//go:embed qwen_helper.mjs
var qwenHelperScript []byte
