package executor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

func init() { Register(Hermes{}) }

// HermesName is this runner's config name.
const HermesName = "hermes"

// Hermes drives Nous Research's Hermes Agent headless: `hermes chat
// --query-file … --format stream-json` in the box's own Hermes home, so the
// box's configuration, skills, plugins and memories load as a person's
// session loads them ([Hermes.Command]). Hermes runs every turn in a gateway
// it starts per home in a session of its own, which the script stops at the
// run's end and on every teardown ([hermesStopGateway]). The runner's plugin,
// loaded in that gateway, takes the run's notes, reports its skill loads and
// records each response's usage ([hermesPluginFiles]). The runner names the
// session itself, as its title ([Hermes.Session]). docs/concepts/
// code-sandbox.md, "Hermes Agent", holds the measurements.
type Hermes struct{}

var _ Harness = Hermes{}

// NewHermes returns the runner.
func NewHermes() *Runner { return New(Hermes{}) }

// Name is the harness's key in the runner registry.
func (Hermes) Name() string { return HermesName }

// MCPCallCeiling is none: the server's `timeout` carries the bridge's own
// bound ([hermesOverlay]), honoured whatever progress flows, and a 1040-second
// call under a 1050-second one was answered whole (measured on 0.23.3). Its
// transport gives up a response silent for 300 seconds, but its calls carry a
// progress token, so the bridge's progress keeps the response from it.
func (Hermes) MCPCallCeiling() time.Duration { return 0 }

// HandsSkills reports that a Hermes run takes skills of its own, installed in
// [Paths.HermesSkills] beside the box's.
func (Hermes) HandsSkills() bool { return true }

// Session is the session a Hermes run works in ([Harness.Session]): a new one
// titled with an id the runner mints (`-c <id> --create-if-missing`), or the
// one it continues, by that title, with a fresh id beside it for a box that
// can take up neither its copy nor a carried one.
func (Hermes) Session(req sandbox.RunRequest) Session {
	if req.Resume != "" {
		return Session{ID: req.Resume, Continues: true, Fresh: uuid.NewString(),
			Replaces: req.Carried != nil && req.Carried.Replace}
	}
	return Session{ID: uuid.NewString()}
}

// hermesFloor is the one Hermes release the runner drives, its built-in
// profile's executor floor (harnessprofile.Executor.Floor), which the doctor
// reads too: the release every figure the runner rests on was measured on.
func hermesFloor() string {
	runner, _ := harnessprofile.ExecutorOf(HermesName)
	return runner.Floor
}

// hermesGatewayStop bounds the wait for Hermes's gateway to go once the script
// sends its process group SIGTERM ([hermesStopGateway]): TEN SECONDS. The
// gateway interrupts its turn, kills its tools and closes its store; measured
// on 0.23.3 from its SIGTERM to its exit: 0.8 to 1.6 s on an idle 2-core box
// (19 runs), 2.4 to 3.1 s with that box's cores both busy (5), and 5.4 and 5.5 s,
// the worst seen, with three busy processes a core. Ten leaves 4.5 s past the
// worst. One cut short by SIGKILL leaves its turn unsettled, which the next
// resume discards first ([hermesDiscard]). The end path stays inside
// [sandbox.JobEndWait]: the gateway's wait, the client's grace and the pack.
const hermesGatewayStop = 10 * time.Second

// hermesTimes is the waits a run's script runs at: a value, so a case about a
// wait runs it at a length it can tell apart ([openCodeTimes]).
type hermesTimes struct{ stop, gateway time.Duration }

// hermesRunTimes is the waits every run's script runs at.
func hermesRunTimes() hermesTimes {
	return hermesTimes{stop: sandbox.JobStopGrace, gateway: hermesGatewayStop}
}

// Command is the run's script ([hermesScript]), run under sh as every runner's
// is, so the wrapper's redirections reach every step.
func (Hermes) Command(req sandbox.RunRequest, session Session, paths Paths, _ string) string {
	return "sh -c " + shellword.Quote(hermesScript(req, session, paths, hermesRunTimes()))
}

// hermesReleaseRead reads the box's `hermes --version` once, as v: `Hermes
// Agent v0.23.3 (2026.9.24) · upstream …` and lines about the install.
const hermesReleaseRead = "v=$(hermes --version < /dev/null 2> /dev/null)\n"

// hermesReleaseRefusal ends the run before anything is written into the Hermes
// home on a box whose Hermes names no release ([bareReleaseShell]) or one below
// [hermesFloor], saying which it printed and which it needs. A newer release
// runs, in a session of its own ([hermesSessionGate]).
func hermesReleaseRefusal() string {
	floor := hermesFloor()
	return bareReleaseShell("n") + `a=${n%%.*}; r=${n#*.}; b=${r%%.*}; c=${r##*.}
at() { [ -n "$n" ] && { [ "$a" -gt "$1" ] || { [ "$a" -eq "$1" ] && { [ "$b" -gt "$2" ] || { [ "$b" -eq "$2" ] && [ "$c" -ge "$3" ]; }; }; }; }; }
` + fmt.Sprintf(`[ -n "$n" ] || { echo "crewlet: this box's hermes --version printed '$(printf '%%s' "$v" | head -n 1)', which names no release the hermes runner reads (a bare MAJOR.MINOR.PATCH, never a pre-release); it drives Hermes Agent %[1]s — install it in the box image" >&2; exit 1; }
at %[2]s || { echo "crewlet: this box runs Hermes Agent $n; the hermes runner drives %[1]s or later — install %[1]s in the box image" >&2; exit 1; }
`, floor, floorArgs(floor))
}

// hermesScript is a Hermes run's script ([Hermes.Command]): the end protocol
// (end.go), whose end request stops the gateway before the client; the
// release, refused below [hermesFloor]; the plugin and skills installed
// ([hermesInstall]); any gateway an earlier run left stopped, so the run's
// own starts with its environment; the checkouts handed in the run's system
// prompt ([hermesCheckouts]); the session settled and the overlay installed
// ([hermesSessionGate]); the run refused where the box would switch its own
// server, plugin or hook off ([hermesGuard]); the session recorded; the client
// under the end protocol ([launchHarness]), again after a discard Hermes asks
// for ([hermesDiscard]); the gateway stopped, the checkouts' copies removed
// and their skips said; the session indexed, the plugin's record and the
// round cap's line added to the stream; the session packed ([hermesFinish]).
func hermesScript(req sandbox.RunRequest, session Session, paths Paths, times hermesTimes) string {
	q := shellword.Quote
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	b.WriteString(endPrologue(paths, times.stop))
	b.WriteString(hermesStopGateway(paths, times.gateway))
	b.WriteString(endHarness("endgw"))
	line(`command -v hermes > /dev/null || { echo 'crewlet: no hermes on the box PATH' >&2; exit 127; }`)
	b.WriteString(hermesReleaseRead)
	b.WriteString(hermesReleaseRefusal())
	b.WriteString(hermesCarryRun(paths))
	b.WriteString(hermesInstall(req, paths))
	line(`st= code=1`)
	b.WriteString(hermesFinish(paths))
	// THE CHECKOUTS' COPIES OUT OF THE HOME'S SKILLS once the gateway, where
	// their firings run, has stopped, and what those firings left out said
	// ([Hermes.checkoutHome]), each step whatever came of the other; a run
	// killed outright leaves them to the next run's clear ([homeKeeper]).
	home := hermesLaunchHome(paths, req.Model)
	line(`hput() { %s; }`, everyStep(home.restore(), home.saySkipped()))
	line(`quit() { trap '' USR1; trap - EXIT TERM INT HUP; endgw; end "$harness"; harness=; hput; finish; exit "$1"; }`)
	line(`trap 'endgw; hput' EXIT`)
	line(`trap 'quit 143' TERM; trap 'quit 130' INT; trap 'quit 129' HUP`)
	// NO GATEWAY WHILE THE STORE IS RESTORED OR RENEWED, nor one an earlier run
	// left serving the next client with its own environment and plugin
	// settings (measured on 0.23.3).
	line(`endgw`)
	// THE CHECKOUTS BELOW THE START DIRECTORY, as Hermes's own code reads them,
	// in the system prompt the session gate renews and the client launches
	// with ([hermesCheckouts]).
	b.WriteString(home.gate())
	b.WriteString(hermesSystemWithCheckouts(paths))
	b.WriteString(hermesSessionGate(req, session, paths, req.FreshWiring != nil))
	b.WriteString(hermesGuard(req))
	line(`printf 'id %%s\ncontinued %%s\nwhy %%s\nrelease %%s\n' "$sid" "$k" "$w" "$v" > %s`, q(paths.SessionRecord()))
	line(`endbefore() { endedas=before; %s; exit 143; }`, strings.TrimSuffix(recordEnded(paths), "\n"))
	line(`[ -z "$endasked" ] || endbefore`)
	// THE BRIEF KEPT, so the relaunch after a discard hands the same words.
	line(`cat > %s || { echo 'crewlet: the brief could not be kept for Hermes, so the run does not start' >&2; exit 1; }`,
		q(paths.HermesBrief()))
	line(`rm -f %s %s`, q(paths.HermesRecord()), q(paths.HermesStderr()))
	line(`sp=; [ ! -s %s ] || sp=$(cat %s)`, q(paths.HermesSystem()), q(paths.HermesSystem()))
	// WHERE THIS LAUNCH'S ROWS BEGIN in the store, for its notes' confirm
	// ([Hermes.notesHeld]); none written where it cannot be read.
	line(`rm -f %[1]s; rs=$(hcarry top %[2]s); case "$rs" in 'crewlet-carry ok '[0-9]*) printf '%%s %%s\n' "$sid" "${rs#crewlet-carry ok }" > %[1]s;; esac`,
		q(paths.HermesRunStart()), q(paths.HermesStateDB()))
	line(`set -- -c "$sid"; [ "$k" = yes ] || set -- "$@" --create-if-missing`)
	launch := hermesLaunch(req, paths)
	b.WriteString(launchHarness(launch, paths))
	b.WriteString(hermesDiscard(paths, launch))
	line(`endgw; hput`)
	line(`st=$(sed -n 's/^Session: *\([A-Za-z0-9_-]*\)$/\1/p' %s 2> /dev/null | head -n 1)`, q(paths.HermesStderr()))
	b.WriteString(hermesIndex(paths))
	line(`[ ! -s %s ] || sed 's/^/{"type":"%s","line":/; s/$/}/' %s`, q(paths.HermesRecord()), hermesRequestEvent,
		q(paths.HermesRecord()))
	if limit := req.Limits.MaxTurns; limit > 0 {
		// THE CAP ENDED THE RUN only where Hermes said so as it exited 1;
		// the result reads it beside the record's tool rounds
		// ([hermesEvents.roundCapped]).
		line(`if [ "$code" = 1 ] && grep -qF -e %s -e %s %s; then printf '{"type":"%s","rounds":%d}\n'; fi`,
			q(hermesCapSentence), q(hermesCapNoSummary), q(paths.Result()), hermesCappedEvent, limit)
	}
	line(`finish`)
	line(`trap - EXIT TERM INT HUP`)
	line(`[ "$code" = 0 ] || [ ! -s %[1]s ] || { echo 'crewlet: hermes said:' >&2; grep -v '^Session: ' %[1]s | tail -c %[2]d >&2; }`,
		q(paths.HermesStderr()), hermesStderrShown)
	line(`exit "$code"`)
	return b.String()
}

// hermesLaunch is the client's command line: the brief from its kept file, the
// stream on stdout, its error stream kept ([Paths.HermesStderr]), and the
// launch flags Hermes freezes with a session ([hermesPolicy]). The standing
// instructions ride HERMES_EPHEMERAL_SYSTEM_PROMPT, the client's alone.
func hermesLaunch(req sandbox.RunRequest, paths Paths) string {
	q := shellword.Quote
	flags := strings.Join(hermesFlags(req), " ")
	if flags != "" {
		flags = " " + flags
	}
	return fmt.Sprintf(`HERMES_EPHEMERAL_SYSTEM_PROMPT="$sp" hermes chat --query-file %s --format stream-json "$@"%s 2> %s`,
		q(paths.HermesBrief()), flags, q(paths.HermesStderr()))
}

// hermesFlags is the launch flags of req's run, each shell-quoted: its round
// cap, model and effort as its entry writes them (the hermes profile's
// model_args and effort). Hermes freezes all three with a session and refuses
// a resume that names others, so the session gate holds a continuation to
// the launch that created it ([hermesPolicy]).
func hermesFlags(req sandbox.RunRequest) []string {
	var out []string
	if n := req.Limits.MaxTurns; n > 0 {
		out = append(out, "--max-turns", fmt.Sprint(n))
	}
	if req.Model != "" {
		out = append(out, "-m", shellword.Quote(req.Model))
	}
	if req.Effort != "" {
		out = append(out, "--reasoning", shellword.Quote(req.Effort))
	}
	return out
}

// hermesPolicy is the launch Hermes freezes with a session (its model, effort,
// round cap and working directory), as the file the session gate compares
// ([Paths.HermesPolicy]); the working directory is the script's own, so it is
// filled in there.
func hermesPolicy(req sandbox.RunRequest) string {
	return fmt.Sprintf("model %s\nreasoning %s\nmax_turns %d\ncwd ", req.Model, req.Effort, req.Limits.MaxTurns)
}

// hermesStderrShown is how much of the client's error stream, from its end, a
// failed run's error stream is given: 4 KiB, as Codex's server log
// ([codexServerLogShown]), since a failure is read from that stream's end.
const hermesStderrShown = 4 << 10

// hermesCapSentence is what Hermes 0.23.3 ends a run's answer with where its
// --max-turns cut the turn short and the one request it then makes for a
// summary answered, and hermesCapNoSummary its whole answer where that
// request called a tool instead; it exits 1 either way (measured).
const (
	hermesCapSentence  = "the maximum tool-iteration limit was reached before a final answer"
	hermesCapNoSummary = "I reached the iteration limit and couldn't generate a summary."
)

// hermesCappedEvent is the line a run's script adds to its stream where Hermes
// exited 1 on its round cap ([hermesCapSentence]), naming the cap.
const hermesCappedEvent = "crewlet.round_cap"

// hermesRequestEvent is the type of the lines a run's script adds to its stream
// once Hermes has exited: each one line of the plugin's record of a response
// ([Paths.HermesRecord]), wrapped as `{"type":…,"line":<the record line>}`.
const hermesRequestEvent = "crewlet.hermes_request"

// hermesStopGateway defines the script's `endgw`: the gateway the home's pid
// file names, where that process is Hermes's gateway, sent SIGTERM as a
// process group (it runs in a session of its own) and given gateway to go,
// then SIGKILL. 0 where there was none. A pid file naming another process (a
// gateway killed and its pid reused) is left alone: the kernel's start time of
// the pid must be the one the file records, or, with no /proc, its command
// line must be a `gateway run`. A gateway that exited is gone though its
// parent, the client, has not reaped it yet, and one whose main thread alone
// has exited is not (`gwgone`, reading its stat as internal/procgroup does).
func hermesStopGateway(paths Paths, gateway time.Duration) string {
	return fmt.Sprintf(`endgw() { gp=$(sed -n 's/.*"pid": *\([0-9][0-9]*\).*/\1/p' %[1]s 2> /dev/null | head -n 1); [ -n "$gp" ] || return 0; gs=$(sed -n 's/.*"start_time": *\([0-9][0-9]*\).*/\1/p' %[1]s 2> /dev/null | head -n 1); if [ -r /proc/"$gp"/stat ]; then [ -n "$gs" ] && [ "$(sed 's/.*) //' /proc/"$gp"/stat | cut -d' ' -f20)" = "$gs" ] || return 0; else case "$(ps -o args= -p "$gp" 2> /dev/null)" in *'gateway run'*) ;; *) return 0;; esac; fi; kill -s TERM -- "-$gp" 2> /dev/null || kill -s TERM "$gp" 2> /dev/null || return 0; i=0; while ! gwgone && [ "$i" -lt %[2]d ]; do sleep 0.1; i=$((i+1)); done; gwgone || kill -s KILL -- "-$gp" 2> /dev/null || kill -s KILL "$gp" 2> /dev/null; return 0; }
gwgone() { ! kill -0 "$gp" 2> /dev/null || if [ -r /proc/self/stat ]; then { read -r gt < /proc/"$gp"/stat; } 2> /dev/null || return 0; set -- ${gt##*) }; case "$1" in [Xx]) true;; Z) [ "${18}" -le 1 ] 2> /dev/null;; *) false;; esac; else case "$(ps -o stat= -p "$gp" 2> /dev/null)" in Z*) true;; *) false;; esac; fi; }
`, shellword.Quote(paths.HermesGatewayPID()), int(gateway/(100*time.Millisecond)))
}

// hermesDiscard relaunches the client once where Hermes refused to run the
// brief because the session holds a turn a killed gateway left unsettled:
// exit 3, its answer naming `hermes sessions discard` (measured on 0.23.3 after
// a SIGKILL). The brief was not sent, so it is sent again once the lost turn
// is discarded.
func hermesDiscard(paths Paths, launch string) string {
	q := shellword.Quote
	return fmt.Sprintf(`if [ "$code" = 3 ] && [ "$k" = yes ] && [ -z "$endasked" ] && grep -qF 'hermes sessions discard' %[1]s; then
st=$(sed -n "s/^$sid //p" %[2]s 2> /dev/null | head -n 1)
if [ -n "$st" ] && hermes sessions discard "$st" --yes < /dev/null > /dev/null 2>&1; then
endgw
`, q(paths.Result()), q(paths.HermesSessions())) + launchHarness(launch, paths) + "fi\nfi\n"
}

// hermesIndex records the session the box now holds: its stored id against
// the runner's ([Paths.HermesSessions]). The launch Hermes froze with a session
// the run creates is kept as the launch starts ([hermesSessionGate]).
func hermesIndex(paths Paths) string {
	q := shellword.Quote
	index := q(paths.HermesSessions())
	return fmt.Sprintf(`if [ -n "$st" ]; then { grep -v "^$sid " %[1]s 2> /dev/null; printf '%%s %%s\n' "$sid" "$st"; } > %[1]s.next && mv -f %[1]s.next %[1]s; fi
`, index)
}

// Output is where a Hermes run's stream lands, which is its result: its
// stdout, `--format stream-json`, with the lines its script adds after it
// ([hermesEvents]).
func (Hermes) Output(paths Paths) Output {
	return Output{Stdout: paths.Result(), Events: true, BriefOnStdin: true}
}

// hermesRouting is every variable of a Hermes run's environment that names
// where its model calls go, read from 0.23.3 with its own interpreter: each
// provider's base_url_env_var in PROVIDER_REGISTRY; those its own code reads
// apart (OpenRouter's, the Nous, Qwen and xAI logins', a local Ollama's); and
// those the Anthropic and OpenAI SDKs it bundles read for a cloud client.
// ANTHROPIC_BASE_URL was measured routing a run (code-sandbox.md, "Hermes
// Agent"); its carry gate also holds a continued session to the route it froze.
var hermesRouting = routes(roleEndpoint,
	"ACTUAL_BASE_URL", "AI_GATEWAY_BASE_URL", "ALIBABA_CODING_PLAN_BASE_URL",
	"ALIBABA_CODING_PLAN_CN_BASE_URL", "ANTHROPIC_BASE_URL", "ARCEE_BASE_URL", "AZURE_FOUNDRY_BASE_URL", "BEDROCK_BASE_URL",
	"COMMANDCODE_ANTHROPIC_BASE_URL", "COMMANDCODE_BASE_URL", "DASHSCOPE_BASE_URL", "DASHSCOPE_CN_BASE_URL",
	"DEEPINFRA_BASE_URL", "DEEPSEEK_BASE_URL", "GEMINI_BASE_URL", "GLM_BASE_URL", "GMI_BASE_URL",
	"HF_BASE_URL", "KILOCODE_BASE_URL", "KIMI_BASE_URL", "LM_BASE_URL", "META_BASE_URL", "MINIMAX_BASE_URL",
	"MINIMAX_CN_BASE_URL", "NEBIUS_BASE_URL", "NOVITA_BASE_URL", "NVIDIA_BASE_URL", "OLLAMA_BASE_URL",
	"OPENAI_BASE_URL", "OPENCODE_GO_BASE_URL", "OPENCODE_ZEN_BASE_URL", "RAMP_ROUTER_BASE_URL",
	"STEPFUN_BASE_URL", "TOKENHUB_BASE_URL", "TOKENPLAN_BASE_URL", "UPSTAGE_BASE_URL", "XAI_BASE_URL",
	"XIAOMI_BASE_URL", "OPENROUTER_BASE_URL", "NOUS_INFERENCE_BASE_URL", "NOUS_PORTAL_BASE_URL",
	"HERMES_PORTAL_BASE_URL", "HERMES_QWEN_BASE_URL", "HERMES_XAI_BASE_URL", "OLLAMA_HOST",
	"ANTHROPIC_BEDROCK_BASE_URL", "ANTHROPIC_BEDROCK_MANTLE_BASE_URL", "ANTHROPIC_FOUNDRY_BASE_URL",
	"ANTHROPIC_GOOGLE_CLOUD_BASE_URL", "ANTHROPIC_VERTEX_BASE_URL", "AWS_BEDROCK_BASE_URL",
	"AZURE_OPENAI_ENDPOINT")

// hermesTokenPlanRouting is two more such variables, named like credentials
// (a TOKEN component), so under env_sign_in they sign Hermes in: their values
// are the entry's sign-in ([harnessprofile.IsCredentialName]), never its route.
var hermesTokenPlanRouting = routes(roleSigns, "ALIBABA_TOKEN_PLAN_BASE_URL", "ALIBABA_TOKEN_PLAN_CN_BASE_URL")

// routing answers [router] with [hermesRouting] and [hermesTokenPlanRouting].
func (Hermes) routing() []routeVar { return slices.Concat(hermesRouting, hermesTokenPlanRouting) }

// Pins is HERMES_HOME, the box's own [Paths.HermesHome], whatever a profile
// sets; HERMES_MANAGED_DIR, the run's overlay ([Paths.HermesManaged]); and
// the plugin's settings file ([Paths.HermesPluginConfig]), which the gateway
// the client starts inherits.
func (Hermes) Pins(_ context.Context, _ sandbox.RunRequest, paths Paths) []Pinned {
	return []Pinned{
		{Name: "HERMES_HOME", Value: paths.HermesHome()},
		{Name: "HERMES_MANAGED_DIR", Value: paths.HermesManaged()},
		{Name: hermesPluginSettingsEnv, Value: paths.HermesPluginConfig()},
	}
}

// Events is a new decoder for one stream, read whole or followed ([Decoder]).
func (Hermes) Events() Decoder { return &hermesEvents{} }

// Parse decodes a whole stream already in hand ([hermesEvents]).
func (Hermes) Parse(stdout []byte) sandbox.Result { return decodeAll(&hermesEvents{}, stdout) }

// artifactFiles is what a Hermes run leaves in its box beside the session it
// continues: the overlays with their tokens and the managed .env beside them,
// the plugin's settings and record, the routes, the kept brief, the client's
// error stream, the carry script, and the note helper with its launch config
// and the system prompt with the checkouts' sections.
func (Hermes) artifactFiles(p Paths) []string {
	return []string{p.HermesOverlay(), p.HermesManagedEnv(), p.HermesOverlayStaged(), p.HermesOverlayFresh(), p.HermesPluginConfig(),
		p.HermesRecord(), p.HermesNotesURL(), p.HermesNotesURLFresh(), p.HermesSkillsURL(),
		p.HermesSkillsURLFresh(), p.HermesBrief(), p.HermesStderr(), p.HermesCarry(), p.HermesCarryErrors(),
		p.HermesRecordRead(), p.HermesRunStart(), p.HermesNoteHelper(), p.HermesCheckoutLaunch(),
		p.HermesCheckoutSections(), p.HermesSystem()}
}

// artifactDirs is the staging directory a Hermes run installs its plugin and
// skills from ([Paths.HermesStage]).
func (Hermes) artifactDirs(p Paths) []string { return []string{p.HermesStage()} }
