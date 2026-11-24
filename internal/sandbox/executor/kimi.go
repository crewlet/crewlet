package executor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

func init() { Register(Kimi{}) }

// KimiName is this runner's config name.
const KimiName = "kimi-code"

// Kimi drives Moonshot's Kimi Code CLI headless through its own server (`kimi
// web --port 0 --json` and its API, KAP), measured on 2.7.2: the
// run's script starts a driver (kimi_driver.js) that creates one session,
// reports its id, posts the brief, hands the run its notes and streams the
// session's events ([kimiScript]). Its model is a metered key's env model
// ([Kimi.Pins]) or a login's alias, named in the session ([kimiSettings]); its
// state is the box home's `.kimi-code` ([Paths.KimiHome]); its workspace is
// trusted, as a person trusts their own, with the driver's guard on the run's
// own servers (kimi_driver.js, guard). Kimi names its session as it creates it
// ([sessionMinter]); its session is carried between boxes and its standing
// instructions renewed on a resume (kimicarry.go; code-sandbox.md, "Kimi Code").
type Kimi struct{}

var (
	_ Harness           = Kimi{}
	_ sessionMinter     = Kimi{}
	_ inboxDelivery     = Kimi{}
	_ recordConfirmer   = Kimi{}
	_ threadHarness     = Kimi{}
	_ mainThreadHarness = Kimi{}
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
// tool run again, and its model read "fetch failed" (measured on 2.7.2).
func (Kimi) MCPCallCeiling() time.Duration { return kimiMCPCallCeiling }

// kimiMCPCallCeiling is [Kimi.MCPCallCeiling]'s: undici's default
// headersTimeout, 300,000 milliseconds.
const kimiMCPCallCeiling = 300 * time.Second

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
const kimiSupported = "2.7.2"

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
// ([kimiUsage]); the home's own MCP servers put back ([kimiRestoreMCP]); and
// its session packed ([kimiCapture]), exiting with the driver's status.
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
	line(`%s`, kimiRestoreMCP(paths))
	b.WriteString(kimiSessionOf(paths))
	b.WriteString(kimiUsage(paths))
	b.WriteString(kimiCapture(paths))
	line(`exit "$code"`)
	return b.String()
}

// kimiRelease opens every Kimi run's script, as [copilotRelease] opens
// Copilot's: `kimi --version` read once as v (`2.7.2`), its numbers as n, and
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
// which KAP's turns leave out (measured on 2.7.2). A box with no awk adds no
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
// a provider or host of the box's own (2.7.2's bundle): the env model
// (KIMI_MODEL_*, which adds a provider and makes it the default), the API and
// OAuth hosts, the extra headers, and the web search and fetch services'
// hosts and keys. Set, it replaces the entry's sign-in or services.
// KIMI_CODE_DANGEROUS_COMMAND_GUARD is left alone: it only raises asks, which
// the driver approves in mode auto, so it switches nothing off (2.7.2).
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
// some two and a half minutes (measured on 2.7.2) and a spent plan's run should
// end and sit its entry out.
const kimiAttemptsPerStep = "3"

// kimiSwitchesOff is every variable that changes how a Kimi run's server is
// reached or how long it is held, removed whatever set it (2.7.2's bundle):
//   - KIMI_CODE_INFINITE_RETRY holds a failing request for ever;
//   - KIMI_LOOP_MAX_STEPS_PER_TURN caps a turn, pinned where the run has a cap;
//   - KIMI_CODE_PASSWORD, KIMI_CODE_DISABLE_HOST_CHECK, KIMI_CODE_ALLOWED_HOSTS,
//     KIMI_CODE_CORS_ORIGINS and KIMI_CODE_REMOTE_CONTROL_RELAY_URL change who
//     may reach the run's server, which a run keeps to its driver;
//   - KIMI_MCP_TOOL_TIMEOUT_MS sets a bridge call's wait, which the run's MCP
//     servers state ([kimiMCP]).
var kimiSwitchesOff = []string{
	"KIMI_CODE_INFINITE_RETRY", "KIMI_LOOP_MAX_STEPS_PER_TURN",
	"KIMI_CODE_PASSWORD", "KIMI_CODE_DISABLE_HOST_CHECK", "KIMI_CODE_ALLOWED_HOSTS", "KIMI_CODE_CORS_ORIGINS",
	"KIMI_CODE_REMOTE_CONTROL_RELAY_URL", "KIMI_MCP_TOOL_TIMEOUT_MS",
}

// Output is where a job writes: its stream is its result, decoded whole or
// followed ([kimiEvents]), with the lines the script adds after it
// ([kimiUsage]). The brief on stdin, which the driver posts.
func (Kimi) Output(paths Paths) Output {
	return Output{Stdout: paths.Result(), Events: true, BriefOnStdin: true, Unset: kimiSwitchesOff}
}

// Pins is the run's home and posture, over whatever the seat's env, the image
// or a profile set: KIMI_CODE_HOME ([Paths.KimiHome]); the workspace trusted
// (KIMI_CODE_TRUST_WORKSPACE), so a repository's MCP servers, permission
// rules and added directories load as in a person's trusted session; updates
// and telemetry off, its telemetry bearing the login's token (measured on
// 2.7.2); the attempts a request gets ([kimiAttemptsPerStep]); the turn cap
// (KIMI_LOOP_MAX_STEPS_PER_TURN, a model request a step); and the env model's
// name where a key signs the run in ([kimiModelPins]).
//
// THE CAP IS EACH TURN'S: a note opens a turn of its own, as a steer does on
// KAP, and that turn is held to the cap afresh, as OpenCode's `steps` holds
// each of its prompts.
func (Kimi) Pins(_ context.Context, req sandbox.RunRequest, paths Paths) []Pinned {
	pins := []Pinned{
		{Name: "KIMI_CODE_HOME", Value: paths.KimiHome()},
		{Name: "KIMI_CODE_TRUST_WORKSPACE", Value: "1"},
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
// effort; its notes route, "" for none; the files it writes; and the run's MCP
// servers by name with the file they are written in, which its guard holds
// them to (kimi_driver.js).
type kimiSettings struct {
	Profile   string   `json:"profile"`
	Model     string   `json:"model,omitempty"`
	Thinking  string   `json:"thinking,omitempty"`
	Notes     string   `json:"notes,omitempty"`
	Report    string   `json:"report"`
	Staging   string   `json:"reportStaging"`
	ServerPid string   `json:"serverPid"`
	Servers   []string `json:"servers"`
	MCPConfig string   `json:"mcpConfig"`
	// ServerStartMs is each server's startupTimeoutMs ([kimiServerStartMs]),
	// which bounds the driver's wait for their tools before the brief.
	ServerStartMs int `json:"serverStartMs"`
	// WebDenied has the driver post the brief with Kimi's web tools on the
	// session's disabled_tools ([kimiWebTools]).
	WebDenied bool `json:"webDenied,omitempty"`
}

// kimiProfileDir is the directory under the home's agents a run's profile is
// written in, which Kimi's discovery walks (measured on 2.7.2) and the clear
// removes whole ([Kimi.artifacts]).
const kimiProfileDir = "crewlet"

// kimiProfileName is a new run's agent profile name: `crewlet-` and 12 random
// hex digits. A NAME OF THE RUN'S OWN, since a repository's
// `.kimi-code/agents/<name>.md` shadows the home's profile of the same name even
// in an untrusted workspace (measured on 2.7.2): a repository cannot guess it.
func kimiProfileName() (string, error) {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("executor: naming the Kimi run's agent profile: %w", err)
	}
	return "crewlet-" + hex.EncodeToString(raw[:]), nil
}

// WriteConfig writes what a Kimi run reads: its agent profile
// ([kimiProfile]), told of notes where it is wired to them; its MCP servers in
// the home beside the home's own ([kimiMCPOver]), the home's own file held
// first ([Paths.KimiHomeMCPConfig]); the driver; and the driver's settings,
// whose path it returns for [Kimi.Command]; with a fresh session's copies of
// the servers and settings beside them ([sandbox.RunRequest.FreshWiring]).
// What an earlier run wrote there the clear removes first ([Kimi.artifacts]).
func (k Kimi) WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, _ Session,
	paths Paths) (string, error) {
	name, err := kimiProfileName()
	if err != nil {
		return "", err
	}
	home, err := kimiHomeMCP(ctx, box, paths)
	if err != nil {
		return "", err
	}
	ours := slices.Collect(maps.Keys(req.MCPServers))
	if f := req.FreshWiring; f != nil {
		ours = append(ours, slices.Collect(maps.Keys(f.MCPServers))...)
	}
	if err := writeJSON(ctx, box, paths.KimiMCPOurs(), slices.Compact(slices.Sorted(slices.Values(ours)))); err != nil {
		return "", fmt.Errorf("executor: recording the Kimi run's own MCP servers: %w", err)
	}
	system := req.System
	if req.Steer.Set() {
		system = withNoteInstruction(system, k.NoteInstruction())
	}
	profile := paths.KimiAgents() + "/" + kimiProfileDir + "/" + name + ".md"
	if err := box.WriteFile(ctx, profile, []byte(kimiProfile(name, system, req.WebDenied))); err != nil {
		return "", fmt.Errorf("executor: writing the Kimi run's agent profile: %w", err)
	}
	if err := writeKimiMCP(ctx, box, paths.KimiMCPConfig(), home, req.MCPServers); err != nil {
		return "", err
	}
	if err := box.WriteFile(ctx, paths.KimiDriver(), []byte(kimiDriverSource)); err != nil {
		return "", fmt.Errorf("executor: writing the Kimi run's driver: %w", err)
	}
	settings := kimiSettings{Profile: name, Thinking: req.Effort, Report: paths.SessionReport(),
		Staging: paths.sessionReportStaging(), ServerPid: paths.KimiServerPid(),
		Servers: slices.Sorted(maps.Keys(req.MCPServers)), MCPConfig: paths.KimiMCPConfig(), WebDenied: req.WebDenied,
		ServerStartMs: kimiServerStartMs}
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
		if err := writeKimiMCP(ctx, box, paths.KimiMCPConfigFresh(), home, f.MCPServers); err != nil {
			return "", err
		}
		settings.Notes = ""
		if f.Steer.Set() {
			settings.Notes = f.Steer.URL
		}
		settings.Servers = slices.Sorted(maps.Keys(f.MCPServers))
		if err := writeJSON(ctx, box, paths.KimiSettingsFresh(), settings); err != nil {
			return "", fmt.Errorf("executor: writing the Kimi run's settings for a new session: %w", err)
		}
	}
	return paths.KimiSettings(), nil
}

// kimiHomeMCP is the home's own MCP servers file ([Paths.KimiMCPConfig]),
// held for the script to put back ([kimiRestoreMCP]): the live file as the
// home left it, held now, or, where a held copy is there (an earlier run was
// cut off before it put the home's back), the live file less that run's
// servers ([kimiMCPLeftOut]), held in its place; "" for none.
func kimiHomeMCP(ctx context.Context, box sandbox.Sandbox, paths Paths) (string, error) {
	q := shellword.Quote
	held, live := q(paths.KimiHomeMCPConfig()), q(paths.KimiMCPConfig())
	hold := fmt.Sprintf(`if [ -e %[1]s ]; then echo cut; else mkdir -p "$(dirname %[1]s)" && { if [ -e %[2]s ]; then cp %[2]s %[1]s; else : > %[1]s; fi; }; fi`,
		held, live)
	res, err := box.Exec(ctx, hold, sandbox.ExecOptions{})
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("exit %d: %s", res.ExitCode, firstLine(res.Stderr, transcriptDetailLimit))
	}
	if err != nil {
		return "", fmt.Errorf("executor: holding the box home's own Kimi Code mcp.json: %w", err)
	}
	if strings.TrimSpace(res.Stdout) != "cut" {
		var raw []byte
		if raw, err = box.ReadFile(ctx, paths.KimiHomeMCPConfig()); err != nil {
			return "", fmt.Errorf("executor: reading the box home's own Kimi Code mcp.json: %w", err)
		}
		return string(raw), nil
	}
	var files [3][]byte
	for i, f := range []string{paths.KimiHomeMCPConfig(), paths.KimiMCPConfig(), paths.KimiMCPOurs()} {
		if files[i], err = box.ReadFile(ctx, f); err != nil {
			return "", fmt.Errorf("executor: reading what a cut-off Kimi Code run left in %s: %w", f, err)
		}
	}
	home, err := kimiMCPLeftOut(string(files[0]), string(files[1]), files[2])
	if err != nil {
		return "", err
	}
	if err := box.WriteFile(ctx, paths.KimiHomeMCPConfig(), []byte(home)); err != nil {
		return "", fmt.Errorf("executor: holding the box home's own Kimi Code mcp.json: %w", err)
	}
	return home, nil
}

var _ homeMender = Kimi{}

// mendHome takes a killed run's servers out of the home's `mcp.json` before
// the next run in the box, whichever harness it runs: where a held copy is
// there, the file as it stands less that run's servers ([kimiHomeMCP]) is put
// back ([kimiRestoreMCP]); nothing where none is held.
func (Kimi) mendHome(ctx context.Context, box sandbox.Sandbox, p Paths) error {
	held, err := heldInBox(ctx, box, p.KimiHomeMCPConfig())
	if err != nil || !held {
		return err
	}
	if _, err = kimiHomeMCP(ctx, box, p); err != nil {
		return err
	}
	return execOK(ctx, box, kimiRestoreMCP(p), "putting back the box home's own Kimi Code mcp.json")
}

// kimiMCPLeftOut is the home's MCP servers file live, as a run cut off before
// its script put the home's own back left it, less that run's servers: each
// name ours records ([Paths.KimiMCPOurs]) is given back the home's own entry
// held, or removed where held had none. Every other key stands as live holds
// it, an edit made since included; "" where live is gone, or held none and
// nothing but the run's was in it.
func kimiMCPLeftOut(held, live string, ours []byte) (string, error) {
	if strings.TrimSpace(live) == "" {
		return "", nil
	}
	var names []string
	if len(ours) > 0 {
		if err := json.Unmarshal(ours, &names); err != nil {
			return "", fmt.Errorf("executor: the record of a cut-off Kimi Code run's MCP servers is no list "+
				"of names (%v), so they cannot be taken out of ~/.kimi-code/mcp.json; remove the file or "+
				"the run's servers from it", err)
		}
	}
	file, servers, err := kimiMCPFile(live)
	if err != nil {
		return "", err
	}
	_, own, err := kimiMCPFile(held)
	if err != nil {
		return "", err
	}
	for _, name := range names {
		if s, ok := own[name]; ok {
			servers[name] = s
		} else {
			delete(servers, name)
		}
	}
	if strings.TrimSpace(held) == "" && len(file) == 1 && len(servers) == 0 {
		return "", nil
	}
	file["mcpServers"] = servers
	out, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return "", fmt.Errorf("executor: rendering the home's own Kimi MCP servers: %w", err)
	}
	return string(out) + "\n", nil
}

// kimiRestoreMCP is the shell putting the home's own MCP servers file back once
// Kimi has stopped: the held copy over the run's, or neither where the home had
// none, then the record of the run's servers removed; nothing where none is
// held, so it runs any number of times.
func kimiRestoreMCP(paths Paths) string {
	q := shellword.Quote
	return fmt.Sprintf(`if [ -e %[1]s ]; then if [ -s %[1]s ]; then mv -f %[1]s %[2]s; else rm -f %[1]s %[2]s; fi; fi; rm -f %[3]s`,
		q(paths.KimiHomeMCPConfig()), q(paths.KimiMCPConfig()), q(paths.KimiMCPOurs()))
}

// writeKimiMCP writes to path the home's own MCP servers file with the run's
// servers over it ([kimiMCPOver]).
func writeKimiMCP(ctx context.Context, box sandbox.Sandbox, path, home string,
	servers map[string]sandbox.MCPServer) error {
	merged, err := kimiMCPOver(home, servers)
	if err != nil {
		return err
	}
	if err = box.WriteFile(ctx, path, merged); err != nil {
		return fmt.Errorf("executor: writing Kimi's MCP servers: %w", err)
	}
	return nil
}

// kimiWebTools is Kimi Code's own web tools, which a web-denied run turns off
// twice (sandbox.RunRequest.WebDenied): left out of its profile's allowlist, so
// its main agent is not offered them, and posted with its brief as the
// session's disabled_tools (kimi_driver.js), which holds every agent of the
// session, a subagent of its own profile included, and its later turns.
// Measured on 2.7.2: the allowlist alone left a coder subagent both, and it
// fetched; disabled_tools refused both to the subagent and the main agent.
var kimiWebTools = []string{"FetchURL", "WebSearch"}

// kimiWebServices is the environment that points Kimi's web tools at a
// service ([kimiRouting]): unset on a web-denied run whoever set it.
var kimiWebServices = []string{
	"KIMI_WEB_SEARCH_BASE_URL", "KIMI_WEB_SEARCH_API_KEY", "KIMI_WEB_FETCH_BASE_URL", "KIMI_WEB_FETCH_API_KEY",
}

// kimiDefaultTools is every tool Kimi Code offers a session by default, as
// 2.7.2 lists them (`tool_select.initialized`), less AskUserQuestion
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
// ([kimiDefaultTools] and every MCP server's tools, the run's, the home's and
// the repository's alike, as a person's session offers them), and the body
// `${base_prompt}` then the run's standing instructions, so they are APPENDED to
// Kimi's own prompt. Kimi renders its template variables across the whole body
// with no escape, the engine's text included: a `${cwd}` in a seat's prompt
// reaches the model as the run's directory (the kimi-code profile's note). A
// web-denied run's allowlist leaves out [kimiWebTools].
func kimiProfile(name, system string, webDenied bool) string {
	var b strings.Builder
	b.WriteString("---\nname: " + name + "\n")
	b.WriteString("description: The Crewlet seat this run serves. Its own tools are the crewlet MCP server's.\n")
	b.WriteString("tools:\n")
	for _, tool := range kimiDefaultTools {
		if !webDenied || !slices.Contains(kimiWebTools, tool) {
			b.WriteString("  - " + tool + "\n")
		}
	}
	b.WriteString("  - " + kimiYAMLString(kimiMCPTools) + "\n")
	b.WriteString("---\n${base_prompt}\n")
	if system = strings.TrimSpace(system); system != "" {
		b.WriteString("\n" + system + "\n")
	}
	return b.String()
}

// kimiMCPTools is every MCP server's tools as a profile's allowlist names
// them: Kimi names a server's tool `mcp__<server>__<tool>`.
const kimiMCPTools = "mcp__*"

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

// kimiMCPOver is home, the home's own MCP servers file, with servers over it
// in Kimi's own vocabulary under `mcpServers` ([kimiMCP]): a person's servers
// and the file's other keys stay, and a server of the home's by one of the
// run's names gives way to the run's. A home holding no JSON object, or
// `mcpServers` that is not one, is refused by name, as Kimi refuses it.
func kimiMCPOver(home string, servers map[string]sandbox.MCPServer) ([]byte, error) {
	file, rendered, err := kimiMCPFile(home)
	if err != nil {
		return nil, err
	}
	for name, s := range servers {
		rendered[name] = kimiMCP(s)
	}
	file["mcpServers"] = rendered
	out, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("executor: rendering Kimi's MCP servers: %w", err)
	}
	return append(out, '\n'), nil
}

// kimiMCPFile is the home's MCP servers file text read as Kimi reads it: its
// object and its `mcpServers`, both empty for no text; an error naming the
// file where it holds no object, or `mcpServers` no object.
func kimiMCPFile(text string) (map[string]any, map[string]any, error) {
	file := map[string]any{}
	if strings.TrimSpace(text) != "" {
		if err := json.Unmarshal([]byte(text), &file); err != nil || file == nil {
			return nil, nil, fmt.Errorf("executor: the box home's ~/.kimi-code/mcp.json holds no JSON object, so "+
				"the run's MCP servers cannot be added to it; Kimi Code itself would refuse it (%v)", err)
		}
	}
	servers, ok := file["mcpServers"].(map[string]any)
	if !ok && file["mcpServers"] != nil {
		return nil, nil, fmt.Errorf("executor: the box home's ~/.kimi-code/mcp.json holds mcpServers as %T, not "+
			"an object, so the run's MCP servers cannot be added to it", file["mcpServers"])
	}
	if servers == nil {
		servers = map[string]any{}
	}
	return file, servers, nil
}

// kimiServerStartMs is each of the run's servers' startupTimeoutMs: Kimi's own
// default (DEFAULT_STARTUP_TIMEOUT_MS, 30 s in 2.7.2), written per server
// because Kimi reads it before the home's `[mcp] startup_timeout_ms` and
// KIMI_MCP_STARTUP_TIMEOUT_MS. Measured on 2.7.2: 1 ms in either left the run
// without its tools, the server never reached; 30000 per server restored them
// in both; the bridge's tools were listed within 10 ms of the session's
// creation.
const kimiServerStartMs = 30_000

// kimiMCP is one server as Kimi's mcp.json reads it (2.7.2): an HTTP server's
// url and headers, a command's command, args and env; every one `deferred:
// false`, so its tools are on every request rather than behind a tool search,
// its own start bound ([kimiServerStartMs]), and `toolTimeoutMs` where the
// engine sets a wait (a call is cancelled at it with `notifications/cancelled`,
// measured).
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
	out["startupTimeoutMs"] = kimiServerStartMs
	if ms := waitMillis(s.CallTimeout); ms > 0 {
		out["toolTimeoutMs"] = ms
	}
	return out
}

// artifactFiles is the files a Kimi run leaves in its box of its own: its
// driver and its settings and their fresh copies, which hold the notes route's
// token. Its MCP servers stand in the home's file until its script puts the
// home's own back ([kimiRestoreMCP]); its sessions and login are kept, and the
// session its driver reported goes with every run's ([Runner.ClearArtifacts]).
func (Kimi) artifactFiles(p Paths) []string {
	return []string{p.KimiDriver(), p.KimiSettings(), p.KimiServerPid(), p.KimiSettingsFresh(),
		p.KimiMCPConfigFresh(), p.KimiLinesBefore()}
}

// artifactDirs is the directory a Kimi run leaves in its box of its own: its
// agent profile's, which a later run must not find among its subagents.
func (Kimi) artifactDirs(p Paths) []string {
	return []string{p.KimiAgents() + "/" + kimiProfileDir}
}

// notesOffWhy is what switches Kimi's reading of notes off: a release before
// [kimiSupported], which the script refuses, or a driver that could not reach
// the notes route, which its error stream names.
func (Kimi) notesOffWhy() string {
	return "a driver that could not reach the run's notes route, which the run's error stream names"
}
