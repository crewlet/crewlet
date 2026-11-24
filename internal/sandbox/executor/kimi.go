package executor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

func init() { Register(Kimi{}) }

// KimiName is this runner's config name.
const KimiName = "kimi-code"

// Kimi drives Moonshot's Kimi Code CLI headless through its own server (`kimi
// web --port 0 --json` and its API, KAP), measured on 2.7.3: the
// run's script starts a driver (kimi_driver.js) that creates one session,
// reports its id, posts the brief, hands the run its notes and streams the
// session's events ([kimiScript]). Its model is a metered key's env model
// ([Kimi.Pins]) or a login's alias, named in the session ([kimiSettings]); its
// state is the box home's `.kimi-code` ([Paths.KimiHome]); its workspace is
// never trusted ([kimiSwitchesOff]). Kimi names its session as it creates it
// ([sessionMinter]); its session is carried between boxes and its standing
// instructions renewed on a resume (kimicarry.go; code-sandbox.md, "Kimi Code").
type Kimi struct{}

var (
	_ Harness           = Kimi{}
	_ sessionMinter     = Kimi{}
	_ inboxDelivery     = Kimi{}
	_ recordConfirmer   = Kimi{}
	_ artifactHolder    = Kimi{}
	_ artifactDirHolder = Kimi{}
	_ inPlaceGate       = Kimi{}
	_ notesOffExplainer = Kimi{}
	_ carrier           = Kimi{}
	_ saver             = Kimi{}
	_ resumer           = Kimi{}
	_ sessionJudge      = Kimi{}
)

// NewKimi returns the runner.
func NewKimi() *Runner { return New(Kimi{}) }

// Name is the harness's key in the runner registry.
func (Kimi) Name() string { return KimiName }

// MCPCallCeiling is 300 seconds, which no setting moves. Its calls carry no
// progress token, so a response is silent until its answer, and Node's fetch,
// which its MCP client uses unchanged, gives up a response whose headers have
// not come in five minutes (undici's headersTimeout): a 310-second call under a
// `toolTimeoutMs` of 1,050,000 was cut at 300.1 seconds, then sent again, its
// tool run again, and its model read "fetch failed" (measured on 2.7.3).
func (Kimi) MCPCallCeiling() time.Duration { return kimiMCPCallCeiling }

// kimiMCPCallCeiling is [Kimi.MCPCallCeiling]'s: undici's default
// headersTimeout, 300,000 milliseconds.
const kimiMCPCallCeiling = 300 * time.Second

// Kimi Code signs in by the names of sandbox.RunRequest.SignInEnv, which reach
// its environment, as code-sandbox.md ("Kimi Code") documents.

// mintsSession says Kimi names its own session (`session_<uuid>`, which
// neither the CLI nor KAP takes from a caller): the driver reports the id KAP
// answers the session's creation with, before the brief ([sessionMinter]).
func (Kimi) mintsSession() bool { return true }

// NoteInstruction is the standing instruction about notes: the driver steers
// a person's notes into the run's session (kimi_driver.js).
func (Kimi) NoteInstruction() string { return KimiSteerInstruction }

// kimiSupported is the one Kimi Code release Crewlet supports, every behaviour
// here measured on it: a box below it is refused at the start, a newer one runs
// sessions of its own alone, and a session is continued only on it
// ([Kimi.Continues], [kimiSessionGate]). Print mode takes no note (it drops a
// Stop hook's continuation and discards PostToolUse's output), so the run is
// `kimi web` and its API.
const kimiSupported = "2.7.3"

// Session is the session a Kimi run works in ([Harness.Session]): the one it
// continues, or a fresh one with no id, which Kimi mints and the run's driver
// reports ([sessionMinter]). A run that continues one and starts its own
// instead is handed no id for it either: its driver reports the one Kimi minted.
func (Kimi) Session(req sandbox.RunRequest) Session {
	if req.Resume != "" {
		return Session{ID: req.Resume, Continues: true, Replaces: req.Carried != nil && req.Carried.Replace}
	}
	return Session{}
}

// Command is the run as ONE SCRIPT ([kimiScript]).
func (Kimi) Command(req sandbox.RunRequest, session Session, paths Paths, configPath string) string {
	return "sh -c " + shellword.Quote(kimiScript(req, session, paths, configPath, sandbox.JobStopGrace))
}

// kimiScript is a Kimi run's script, giving its driver stop to go once asked to
// end: the end protocol (end.go); the release read, and one older than
// [kimiSupported] refused;
// node required, which the driver runs on; the routing variables the entry did
// not set removed ([kimiRoutingOff]); the session settled and recorded
// ([kimiSessionGate], [Paths.SessionRecord]); the driver run under the end
// protocol ([launchHarness]), its brief on stdin; the server it started ended
// where it outlived it; the run's own usage added to the stream
// ([kimiUsage]); and its session packed ([kimiCapture]), exiting with the
// driver's status.
func kimiScript(req sandbox.RunRequest, session Session, paths Paths, settings string, stop time.Duration) string {
	q := shellword.Quote
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	b.WriteString(endPrologue(paths, stop))
	b.WriteString(endHarness(""))
	b.WriteString(kimiRelease)
	line(`if ! at %s; then printf 'crewlet: this box runs Kimi Code %%s, older than %s, the release Crewlet supports\n' "$n" >&2; exit 1; fi`,
		floorArgs(kimiSupported), kimiSupported)
	line(`command -v node > /dev/null 2>&1 || { echo 'crewlet: this box has no node on PATH, which Kimi Code and its run'"'"'s driver run on' >&2; exit 1; }`)
	if off := kimiRoutingOff(req); len(off) > 0 {
		line(`unset %s`, strings.Join(off, " "))
	}
	line(`set --`)
	b.WriteString(kimiSessionGate(session, paths, req.FreshWiring != nil))
	line(`printf 'id %%s\ncontinued %%s\nwhy %%s\nrelease %%s\n' "$s" "$k" "$w" "$v" > %s`, q(paths.SessionRecord()))
	b.WriteString(kimiLinesBefore(paths))
	b.WriteString(launchHarness("node "+q(paths.KimiDriver())+" "+q(settings)+` "$@"`, paths))
	// THE SERVER, ENDED WHERE IT OUTLIVED ITS DRIVER: one the end had to kill
	// never got to stop it. Asked at once, so its pid is the driver's child's.
	line(`p=$(cat %s 2> /dev/null) || p=; case "$p" in ''|*[!0-9]*) ;; *) kill -0 "$p" 2> /dev/null && end "$p" ;; esac`,
		q(paths.KimiServerPid()))
	b.WriteString(kimiSessionOf(paths))
	b.WriteString(kimiUsage(paths))
	b.WriteString(kimiCapture(paths))
	line(`exit "$code"`)
	return b.String()
}

// kimiRelease opens every Kimi run's script, as [copilotRelease] opens
// Copilot's: `kimi --version` read once as v (`2.7.3`), its numbers as n, and
// `at MAJOR MINOR PATCH`. A VERSION IT CANNOT READ IS TAKEN FOR A CURRENT
// RELEASE, and a missing harness fails at the run.
const kimiRelease = `v=$(kimi --version < /dev/null 2> /dev/null | head -n 1); n=${v#"${v%%[0-9]*}"}; n=${n%%[!0-9.]*}; n=${n%.}
a=${n%%.*}; r=${n#*.}; b=${r%%.*}; c=${r#*.}; c=${c%%.*}
case "$n" in [0-9]*.[0-9]*.[0-9]*) ;; *) a= ;; esac
at() { [ -z "$a" ] || [ "$a" -gt "$1" ] || { [ "$a" -eq "$1" ] && { [ "$b" -gt "$2" ] || { [ "$b" -eq "$2" ] && [ "$c" -ge "$3" ]; }; }; }; }
`

// kimiUsageMark is the type of the line a run's script adds to its stream once
// its driver exits, after which every line is one of the run's own
// `usage.record`s ([kimiUsage]).
const kimiUsageMark = "crewlet.usage"

// kimiUsage is the part of a run's script that adds to its stream the run's own
// record of its spend and its turns' ends, once its driver has exited: the mark
// ([kimiUsageMark]), then every `usage.record` and `turn.ended` line of every
// agent's wire.jsonl in the run's session ([kimiSessionOf]) past the lines it
// held before the run ([kimiLinesBefore]) — the main agent's requests
// (`usageScope: turn`), its compactions' (`session`) and each subagent's,
// which KAP's turns leave out (measured on 2.7.3). A box with no awk adds no
// mark, and says so.
func kimiUsage(paths Paths) string {
	q := shellword.Quote
	return `if command -v awk > /dev/null 2>&1; then
  printf '%s\n' '{"type":"` + kimiUsageMark + `"}'
  if [ -n "$s" ]; then
    for w in ` + q(paths.KimiSessions()) + `/*/"$s"/agents/*/wire.jsonl; do
      [ -f "$w" ] || continue
      before=$(awk -v f="$w" '{ n = $1; sub(/^[0-9]+ /, ""); if ($0 == f) { print n; exit } }' ` +
		q(paths.KimiLinesBefore()) + ` 2> /dev/null); before=${before:-0}
      awk -v n="$before" 'NR > n && (index($0, "{\"type\":\"usage.record\"") == 1 || index($0, "{\"type\":\"turn.ended\"") == 1)' "$w" 2> /dev/null
    done
  fi
else
  echo 'crewlet: this box has no awk, so the run'"'"'s own usage was not read from its session' >&2
fi
`
}

// kimiRouting is every variable that routes Kimi's model calls or its login to
// a provider or host of the box's own (2.7.3's bundle): the env model
// (KIMI_MODEL_*, which adds a provider and makes it the default), the API and
// OAuth hosts, the extra headers, and the web search and fetch services'
// hosts and keys. Set, it replaces the entry's sign-in or services.
// KIMI_CODE_DANGEROUS_COMMAND_GUARD is left alone: it only raises asks, which
// the driver approves in mode auto, so it switches nothing off (measured on
// 2.7.3). So is KIMI_CODE_FEEDBACK_BASE_URL, new in 2.7.3: it points only the
// feedback a person posts to the server's API, which a run never posts.
var kimiRouting = slices.Concat(
	routes(roleEndpoint, "KIMI_MODEL_PROVIDER_TYPE", "KIMI_MODEL_BASE_URL", "KIMI_CODE_BASE_URL", "KIMI_BASE_URL",
		"KIMI_CODE_OAUTH_HOST", "KIMI_OAUTH_HOST"),
	// The headers can carry routing, and are left out of the route as a sign
	// variable, since they carry sign-in.
	routes(roleSigns, "KIMI_MODEL_API_KEY", "KIMI_API_KEY", "KIMI_CODE_CUSTOM_HEADERS"),
	routes(roleShapes, "KIMI_MODEL_NAME", "KIMI_MODEL_MAX_CONTEXT_SIZE", "KIMI_MODEL_MAX_OUTPUT_SIZE",
		"KIMI_MODEL_CAPABILITIES", "KIMI_MODEL_THINKING_EFFORT", "KIMI_MODEL_ADAPTIVE_THINKING",
		"KIMI_MODEL_THINKING_KEEP", "KIMI_MODEL_DISPLAY_NAME", "KIMI_MODEL_REASONING_KEY", "KIMI_MODEL_OUTPUT_FORMAT",
		"KIMI_MODEL_TOP_P", "KIMI_MODEL_TEMPERATURE", "KIMI_MODEL_MAX_TOKENS", "KIMI_MODEL_MAX_COMPLETION_TOKENS"),
	routes(roleService, kimiWebServices...),
)

// routing answers [router] with [kimiRouting].
func (Kimi) routing() []routeVar { return kimiRouting }

// kimiNoTelemetry is every OTLP exporter variable, the box's and any an image
// or a profile sets per signal, which the wrapper removes ([Output.Unset]):
// Kimi Code exports no OTLP (2.7.3's bundle reads no OTEL_ variable), so the
// run and its tools are handed only what they need.
var kimiNoTelemetry = otlpExporterVars()

// otlpExporterVars is OTEL_EXPORTER_OTLP_{,TRACES_,METRICS_,LOGS_} with each of
// ENDPOINT, HEADERS and PROTOCOL: the box's own ([sandbox.OtelUpstreamEndpointVar])
// and the per-signal forms that override them.
func otlpExporterVars() []string {
	var out []string
	for _, signal := range []string{"", "TRACES_", "METRICS_", "LOGS_"} {
		for _, part := range []string{"ENDPOINT", "HEADERS", "PROTOCOL"} {
			out = append(out, "OTEL_EXPORTER_OTLP_"+signal+part)
		}
	}
	return out
}

// kimiRoutingOff is every routing variable ([kimiRouting]) neither the run's own
// environment (env, the entry's) sets nor [Kimi.Pins] pins, which the script
// unsets: one from the image or a login profile would send the seat's model
// calls where its entry never named. A web-denied run unsets the web services
// ([kimiWebServices]) whoever set them. In the script for
// [copilotProviderOff]'s reason.
func kimiRoutingOff(req sandbox.RunRequest) []string {
	pinned := kimiModelPins(req)
	var off []string
	for _, name := range routeNames(kimiRouting) {
		_, set := req.Env[name]
		if req.WebDenied && slices.Contains(kimiWebServices, name) {
			set = false
		}
		if !set && !slices.ContainsFunc(pinned, func(p Pinned) bool { return p.Name == name }) {
			off = append(off, name)
		}
	}
	return off
}

// kimiKeyEnv is the variable a metered key signs Kimi in by, and kimiModelEnv
// the one its model goes in beside it (the kimi-code profile's api_key_env and
// api_key_model_env): KIMI_MODEL_NAME and KIMI_MODEL_API_KEY add a provider of
// their own that becomes the default.
const (
	kimiKeyEnv   = "KIMI_MODEL_API_KEY"
	kimiModelEnv = "KIMI_MODEL_NAME"
)

// kimiKeySignsIn reports whether req's run is signed in by a metered key: its
// entry's environment carries the key, as an api-key entry's box env does.
func kimiKeySignsIn(req sandbox.RunRequest) bool {
	_, ok := req.Env[kimiKeyEnv]
	return ok
}

// kimiModelPins is the model a key-signed run's env model is named by, the
// entry's model verbatim; nothing for a run signed in otherwise, whose model
// the session names ([kimiSettings]).
func kimiModelPins(req sandbox.RunRequest) []Pinned {
	if !kimiKeySignsIn(req) || req.Model == "" {
		return nil
	}
	return []Pinned{{Name: kimiModelEnv, Value: req.Model}}
}

// kimiAttemptsPerStep is how many times Kimi sends one request before the turn
// fails: three, the text profile's, since its default of ten held a 429 for
// some two and a half minutes (measured on 2.7.3) and a spent plan's run should
// end and sit its entry out.
const kimiAttemptsPerStep = "3"

// kimiSwitchesOff is every variable that changes what a Kimi run loads, how its
// server is reached or how long it is held, removed whatever set it (2.7.3's
// bundle):
//   - KIMI_CODE_TRUST_WORKSPACE trusts the checkout, whose deny rule or broken
//     MCP file then displaces the bridge (code-sandbox.md, "Kimi Code");
//   - KIMI_CODE_INFINITE_RETRY holds a failing request for ever;
//   - KIMI_LOOP_MAX_STEPS_PER_TURN caps a turn, pinned where the run has a cap;
//   - KIMI_CODE_PASSWORD, KIMI_CODE_DISABLE_HOST_CHECK, KIMI_CODE_ALLOWED_HOSTS,
//     KIMI_CODE_CORS_ORIGINS and KIMI_CODE_REMOTE_CONTROL_RELAY_URL change who
//     may reach the run's server, which a run keeps to its driver;
//   - KIMI_MCP_TOOL_TIMEOUT_MS sets a bridge call's wait, which the run's MCP
//     servers state ([kimiMCP]).
var kimiSwitchesOff = []string{
	"KIMI_CODE_TRUST_WORKSPACE", "KIMI_CODE_INFINITE_RETRY", "KIMI_LOOP_MAX_STEPS_PER_TURN",
	"KIMI_CODE_PASSWORD", "KIMI_CODE_DISABLE_HOST_CHECK", "KIMI_CODE_ALLOWED_HOSTS", "KIMI_CODE_CORS_ORIGINS",
	"KIMI_CODE_REMOTE_CONTROL_RELAY_URL", "KIMI_MCP_TOOL_TIMEOUT_MS",
}

// Output is where a job writes: its stream is its result, decoded whole or
// followed ([kimiEvents]), with the lines the script adds after it
// ([kimiUsage]). The brief on stdin, which the driver posts.
func (Kimi) Output(paths Paths) Output {
	return Output{Stdout: paths.Result(), Events: true, BriefOnStdin: true, Unset: slices.Concat(kimiSwitchesOff, kimiNoTelemetry)}
}

// Pins is the run's home and posture, over whatever the seat's env, the image
// or a profile set: KIMI_CODE_HOME ([Paths.KimiHome]); updates and telemetry
// off, its telemetry bearing the login's token (read in 2.7.3's bundle); the
// attempts a request gets ([kimiAttemptsPerStep]); the turn cap
// (KIMI_LOOP_MAX_STEPS_PER_TURN, a model request a step); and the env model's
// name where a key signs the run in ([kimiModelPins]).
//
// THE CAP IS EACH TURN'S: a note opens a turn of its own, as a steer does on
// KAP, and that turn is held to the cap afresh, as OpenCode's `steps` holds
// each of its prompts.
func (Kimi) Pins(_ context.Context, req sandbox.RunRequest, paths Paths) []Pinned {
	pins := []Pinned{
		{Name: "KIMI_CODE_HOME", Value: paths.KimiHome()},
		{Name: "KIMI_CODE_NO_AUTO_UPDATE", Value: "1"},
		{Name: "KIMI_DISABLE_TELEMETRY", Value: "1"},
		{Name: "KIMI_LOOP_MAX_ATTEMPTS_PER_STEP", Value: kimiAttemptsPerStep},
	}
	if req.Limits.MaxTurns > 0 {
		pins = append(pins, Pinned{Name: "KIMI_LOOP_MAX_STEPS_PER_TURN", Value: fmt.Sprint(req.Limits.MaxTurns)})
	}
	return append(pins, kimiModelPins(req)...)
}

// Events is a new decoder for one stream, read whole or followed ([Decoder]).
func (Kimi) Events() Decoder { return &kimiEvents{} }

// Parse decodes a whole stream already in hand: the result of a run whose
// result IS its stream.
func (Kimi) Parse(stdout []byte) sandbox.Result { return decodeAll(&kimiEvents{}, stdout) }

// kimiSettings is what the driver is written with ([Paths.KimiSettings]): the
// run's profile; the model the session names, for a run not signed in by a key
// (an alias of config.toml's, as `-m` takes it); its thinking, the entry's
// effort; its notes route, "" for none; and the files it writes.
type kimiSettings struct {
	Profile   string `json:"profile"`
	Model     string `json:"model,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
	Notes     string `json:"notes,omitempty"`
	Report    string `json:"report"`
	Staging   string `json:"reportStaging"`
	ServerPid string `json:"serverPid"`
	// WebDenied has the driver post the brief with Kimi's web tools on the
	// session's disabled_tools ([kimiWebTools]).
	WebDenied bool `json:"webDenied,omitempty"`
}

// kimiProfileDir is the directory under the home's agents a run's profile is
// written in, which Kimi's discovery walks (measured on 2.7.3) and the clear
// removes whole ([Kimi.artifacts]).
const kimiProfileDir = "crewlet"

// kimiProfileName is a new run's agent profile name: `crewlet-` and 12 random
// hex digits. A NAME OF THE RUN'S OWN, since a repository's
// `.kimi-code/agents/<name>.md` shadows the home's profile of the same name even
// in an untrusted workspace (measured on 2.7.3): a repository cannot guess it.
func kimiProfileName() (string, error) {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("executor: naming the Kimi run's agent profile: %w", err)
	}
	return "crewlet-" + hex.EncodeToString(raw[:]), nil
}

// WriteConfig writes what a Kimi run reads: its agent profile
// ([kimiProfile]), told of notes where it is wired to them; its MCP servers in
// the home ([Paths.KimiMCPConfig]); the driver; and the driver's settings,
// whose path it returns for [Kimi.Command]; with a fresh session's copies of
// the servers and settings beside them ([sandbox.RunRequest.FreshWiring]). What an earlier run wrote there the
// clear removes first ([Kimi.artifacts]).
func (k Kimi) WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, _ Session,
	paths Paths) (string, error) {
	name, err := kimiProfileName()
	if err != nil {
		return "", err
	}
	if err = writeKimiLoginConfig(ctx, box, paths); err != nil {
		return "", err
	}
	system := req.System
	if req.Steer.Set() {
		system = withNoteInstruction(system, k.NoteInstruction())
	}
	profile := paths.KimiAgents() + "/" + kimiProfileDir + "/" + name + ".md"
	if err := box.WriteFile(ctx, profile, []byte(kimiProfile(name, system, req.MCPServers, req.WebDenied))); err != nil {
		return "", fmt.Errorf("executor: writing the Kimi run's agent profile: %w", err)
	}
	if err := writeJSON(ctx, box, paths.KimiMCPConfig(), kimiMCPConfig(req.MCPServers)); err != nil {
		return "", fmt.Errorf("executor: writing Kimi's MCP servers: %w", err)
	}
	if err := box.WriteFile(ctx, paths.KimiDriver(), []byte(kimiDriverSource)); err != nil {
		return "", fmt.Errorf("executor: writing the Kimi run's driver: %w", err)
	}
	settings := kimiSettings{Profile: name, Thinking: req.Effort, Report: paths.SessionReport(),
		Staging: paths.sessionReportStaging(), ServerPid: paths.KimiServerPid(), WebDenied: req.WebDenied}
	if !kimiKeySignsIn(req) {
		settings.Model = req.Model
	}
	if req.Steer.Set() {
		settings.Notes = req.Steer.URL
	}
	if err := writeJSON(ctx, box, paths.KimiSettings(), settings); err != nil {
		return "", fmt.Errorf("executor: writing the Kimi run's settings: %w", err)
	}
	if f := req.FreshWiring; f != nil {
		if err := writeJSON(ctx, box, paths.KimiMCPConfigFresh(), kimiMCPConfig(f.MCPServers)); err != nil {
			return "", fmt.Errorf("executor: writing Kimi's MCP servers for a new session: %w", err)
		}
		settings.Notes = ""
		if f.Steer.Set() {
			settings.Notes = f.Steer.URL
		}
		if err := writeJSON(ctx, box, paths.KimiSettingsFresh(), settings); err != nil {
			return "", fmt.Errorf("executor: writing the Kimi run's settings for a new session: %w", err)
		}
	}
	return paths.KimiSettings(), nil
}

// kimiWebTools is Kimi Code's own web tools, which a web-denied run turns off
// twice (sandbox.RunRequest.WebDenied): left out of its profile's allowlist, so
// its main agent is not offered them, and posted with its brief as the
// session's disabled_tools (kimi_driver.js), which holds every agent of the
// session, a subagent of its own profile included, and its later turns.
// Measured on 2.7.3: the allowlist alone left a coder subagent both, and it
// fetched; disabled_tools refused both to the subagent and the main agent.
var kimiWebTools = []string{"FetchURL", "WebSearch"}

// kimiWebServices is the environment that points Kimi's web tools at a
// service ([kimiRouting]): unset on a web-denied run whoever set it.
var kimiWebServices = []string{
	"KIMI_WEB_SEARCH_BASE_URL", "KIMI_WEB_SEARCH_API_KEY", "KIMI_WEB_FETCH_BASE_URL", "KIMI_WEB_FETCH_API_KEY",
}

// writeKimiLoginConfig rewrites the box's config.toml ([Paths.KimiConfig]) to
// the tables its login is held under, as the kimi-code profile names them
// ([harnessprofile.FileRule.Seed]): Kimi reads hooks, which it runs,
// plugins, permission rules and tool settings from the same file (measured on
// 2.7.3), so whatever a seed or an earlier run left there beside the login is
// gone before the run starts. A box holding none keeps none; one holding a
// file that is not TOML is refused, since its login cannot be told apart.
func writeKimiLoginConfig(ctx context.Context, box sandbox.Sandbox, paths Paths) error {
	path := paths.KimiConfig()
	held, err := box.Exec(ctx, "test -f "+shellword.Quote(path), sandbox.ExecOptions{})
	if err != nil {
		return fmt.Errorf("executor: looking for the box's Kimi Code config.toml: %w", err)
	}
	if held.ExitCode != 0 {
		return nil
	}
	content, err := box.ReadFile(ctx, path)
	if err != nil {
		return fmt.Errorf("executor: reading the box's Kimi Code config.toml: %w", err)
	}
	profile, ok := harnessprofile.Builtin(KimiName)
	if !ok {
		return fmt.Errorf("executor: no built-in %s profile names the login's tables of config.toml", KimiName)
	}
	login, err := profile.FileRule(filepath.Base(path)).Seed(content)
	if err != nil {
		return fmt.Errorf("executor: the box's Kimi Code config.toml (%s) holds no login it can hand the run: "+
			"%w; sign the entry in again with `crewlet llm login`", path, err)
	}
	if bytes.Equal(login, content) {
		return nil
	}
	if err = box.WriteFile(ctx, path, login); err != nil {
		return fmt.Errorf("executor: rewriting the box's Kimi Code config.toml to its login: %w", err)
	}
	return nil
}

// kimiDefaultTools is every tool Kimi Code offers a session by default, as
// 2.7.3 lists them (`tool_select.initialized`), less AskUserQuestion
// (a headless run has nobody to answer; ask_and_wait is on the bridge) and
// ExitPlanMode (it asks for a plan's approval the same way), with WebSearch,
// which a login's search service registers. An allowlist, so a tool a later
// release adds is left out rather than let in unread; a name Kimi does not know
// is only warned about.
var kimiDefaultTools = []string{
	"Agent", "AgentSwarm", "Bash", "CreateGoal", "CronCreate", "CronDelete", "CronList", "Edit", "FetchURL",
	"GetGoal", "Glob", "Grep", "Read", "ReadMediaFile", "SetGoalBudget", "Skill", "TaskList", "TaskOutput",
	"TaskStop", "TodoList", "UpdateGoal", "WaitFor", "WebSearch", "Write", "select_tools",
}

// kimiProfile is a run's agent profile: its name, the allowlist
// ([kimiDefaultTools] and every tool of each engine server), and the body
// `${base_prompt}` then the run's standing instructions, so they are APPENDED to
// Kimi's own prompt. Kimi renders its template variables across the whole body
// with no escape, the engine's text included: a `${cwd}` in a seat's prompt
// reaches the model as the run's directory (the kimi-code profile's note). A
// web-denied run's allowlist leaves out [kimiWebTools].
func kimiProfile(name, system string, servers map[string]sandbox.MCPServer, webDenied bool) string {
	var b strings.Builder
	b.WriteString("---\nname: " + name + "\n")
	b.WriteString("description: The Crewlet seat this run serves. Its own tools are the crewlet MCP server's.\n")
	b.WriteString("tools:\n")
	for _, tool := range kimiDefaultTools {
		if !webDenied || !slices.Contains(kimiWebTools, tool) {
			b.WriteString("  - " + tool + "\n")
		}
	}
	for _, server := range slices.Sorted(maps.Keys(servers)) {
		b.WriteString("  - " + kimiYAMLString("mcp__"+server+"__*") + "\n")
	}
	b.WriteString("---\n${base_prompt}\n")
	if system = strings.TrimSpace(system); system != "" {
		b.WriteString("\n" + system + "\n")
	}
	return b.String()
}

// kimiYAMLString is s as one YAML double-quoted scalar, which a server's name
// never ends early: JSON's escaping is YAML's.
func kimiYAMLString(s string) string {
	quoted := []byte{'"'}
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			quoted = append(quoted, '\\', byte(r))
		case r < 0x20:
			quoted = fmt.Appendf(quoted, `\u%04x`, r)
		default:
			quoted = append(quoted, string(r)...)
		}
	}
	return string(append(quoted, '"'))
}

// kimiMCPConfig is servers in Kimi's own MCP vocabulary, under `mcpServers`
// ([kimiMCP]), written whole: an earlier run's servers never stay.
func kimiMCPConfig(servers map[string]sandbox.MCPServer) map[string]any {
	rendered := make(map[string]any, len(servers))
	for name, s := range servers {
		rendered[name] = kimiMCP(s)
	}
	return map[string]any{"mcpServers": rendered}
}

// kimiMCP is one server as Kimi's mcp.json reads it (2.7.3): an HTTP server's
// url and headers, a command's command, args and env; every one `deferred:
// false`, so its tools are on every request rather than behind a tool search,
// and `toolTimeoutMs` where the engine sets a wait (a call is cancelled at it
// with `notifications/cancelled`, measured).
func kimiMCP(s sandbox.MCPServer) map[string]any {
	var out map[string]any
	if s.Transport == sandbox.TransportHTTP {
		out = map[string]any{"url": s.URL}
		if len(s.Headers) > 0 {
			out["headers"] = s.Headers
		}
	} else {
		out = map[string]any{"command": s.Command, "args": s.Args}
		if len(s.Env) > 0 {
			out["env"] = s.Env
		}
	}
	out["deferred"] = false
	if ms := waitMillis(s.CallTimeout); ms > 0 {
		out["toolTimeoutMs"] = ms
	}
	return out
}

// artifactFiles is the files a Kimi run leaves in its box of its own: its
// driver and its settings and their fresh copies, which hold the notes route's
// token, and the personal AGENTS.md files Kimi loads as the user's own,
// whatever wrote them (measured on 2.7.3), so a run loads only the config its
// launch writes; its config.toml is rewritten to its login
// ([writeKimiLoginConfig]). Its MCP servers are written whole by every run,
// its sessions and login are kept, and the session its driver reported goes
// with every run's ([Runner.ClearArtifacts]).
func (Kimi) artifactFiles(p Paths) []string {
	return append([]string{p.KimiDriver(), p.KimiSettings(), p.KimiServerPid(), p.KimiSettingsFresh(),
		p.KimiMCPConfigFresh(), p.KimiLinesBefore(), p.KimiInstructions()}, p.KimiUserInstructions()...)
}

// artifactDirs is the directories a Kimi run leaves in its box of its own, or
// that its home holds for Kimi to load as the user's own: its agent profile's,
// which a later run must not find among its subagents, the skills, the
// installed plugins and the saved workspace trust.
func (Kimi) artifactDirs(p Paths) []string {
	return []string{p.KimiAgents() + "/" + kimiProfileDir, p.KimiSkills(), p.KimiUserSkills(),
		p.KimiPlugins(), p.KimiTrust()}
}

// notesOffWhy is what switches Kimi's reading of notes off: a release before
// [kimiSupported], which the script refuses, or a driver that could not reach
// the notes route, which its error stream names.
func (Kimi) notesOffWhy() string {
	return "a driver that could not reach the run's notes route, which the run's error stream names"
}
