package executor

import (
	"cmp"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
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
// with: the export depends on the tools the approval mode offers and on the
// model family, so it is made with the run's own. Everything a person's
// session loads loads, the home's and the working directory's extensions and
// MCP servers among it. A run with servers of Crewlet's own names its
// admin-tier policy ([geminiAdminPolicy]).
func geminiFlags(req sandbox.RunRequest, paths Paths) []string {
	flags := []string{"-p", `""`, "--output-format", "stream-json", "--approval-mode", "yolo"}
	if len(crewletServers(req.MCPServers)) > 0 {
		flags = append(flags, "--admin-policy", shellword.Quote(paths.GeminiAdminPolicy()))
	}
	if req.Model != "" {
		flags = append(flags, "-m", shellword.Quote(req.Model))
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
// pin the slug, name the session ([chatSessionGate]), put the run's settings
// into the home's and guard what outranks them ([geminiGuard]), record the
// session, export the harness's prompt with the run's text after it, run the
// brief (stdin), append the run's spend ([geminiUsageEvent]), pack the session
// ([chatCapture]), and put the home's settings back ([homeKeeper]).
//
// THE ADD-TO CHANNEL IS BUILT, since Gemini CLI has none: GEMINI_SYSTEM_MD
// replaces its prompt, so the script has it write the prompt out
// (GEMINI_WRITE_SYSTEM_MD, byte for byte, tools, hook context and skills
// resolved) and names the export plus the run's text. Made per run, in the
// run's box with its flags: the export depends on the box's home and model.
func geminiScript(req sandbox.RunRequest, session Session, paths Paths) string {
	flags := strings.Join(geminiFlags(req, paths), " ")
	q := shellword.Quote
	var fresh []freshCopy
	if req.FreshWiring != nil {
		fresh = geminiFreshCopies(paths, geminiHooked(req))
	}
	lines := []string{
		// THE HOME'S OWN SETTINGS BACK, and the checkouts' skills out, however
		// the script ends, a teardown's signal included: dash runs no EXIT trap
		// for a signal it dies of, so each is trapped, and runs once the
		// foreground command the signal also reached returns. A run killed
		// outright leaves them to the next run's clear ([homeKeeper]).
		// What the checkouts' hook left out said first ([checkoutHome.saySkipped]).
		"restore() { " + geminiCheckoutHome(paths).saySkipped() + "; " + GeminiCLI{}.homeRestore(paths) +
			"; }; trap restore EXIT",
		`trap 'restore; trap - EXIT; exit 143' TERM; trap 'restore; trap - EXIT; exit 130' INT; ` +
			`trap 'restore; trap - EXIT; exit 129' HUP`,
		// THE RELEASE, read once for the gate, the record and the session's,
		// before anything else: an older one never runs ([releaseGate]).
		releaseRead(GeminiCLI{}.VersionCommand()),
		releaseGate("Gemini CLI", geminiRelease),
		"node " + q(paths.GeminiHelper()) + " pin " + q(paths.GeminiProjects()) + " " + GeminiProjectSlug + " || exit 1",
		// NO SERVER OF A FILE'S TAKES THE BRIDGE'S NAMES OR ITS PLACE, said
		// before Gemini CLI starts ([geminiShadowCheck]); the BeforeTool hook
		// checks each call at run time ([geminiBridgedHook]).
		geminiShadowCheck(req, paths),
		"rm -f " + q(paths.GeminiUnbridged()),
		strings.TrimSuffix(chatSessionGate(session, paths, geminiGate(paths, fresh)), "\n"),
		// WHERE THE RUN BEGINS IN ITS SESSION, once the gate cut it back: its
		// lines and their digest ([chatRunStartFrom]).
		chatRunStartFrom,
		// THE RUN'S SETTINGS IN THE HOME'S OWN FILE ([Paths.GeminiHomeSettings]),
		// once the gate has chosen the wiring.
		"node " + q(paths.GeminiHelper()) + " settings " + q(paths.GeminiSettings()) + " " +
			q(paths.GeminiHomeSettings()) + " " + q(paths.GeminiHeldSettings()) + " || exit 1",
		// THE CHECKOUTS' SKILLS, and hk where the helper ran ([geminiCheckoutsGate]).
		strings.TrimSuffix(geminiCheckoutsGate(paths), "\n"),
		// NOTHING OF CREWLET'S OWN SWITCHED OFF OR REPLACED where no tier of the
		// run's undoes it ([geminiGuard]), checked before Gemini CLI first starts;
		// the checkouts' hook switched off only said in the skip log (D709).
		"gk=" + q(geminiCheckoutsHook) + "; gs=" + q(geminiCheckoutHome(paths).skipped()),
		"node " + q(paths.GeminiHelper()) + " guard" + geminiGuard(req) +
			` ${hk:+--checkouts "$gk" --skipped "$gs"} || exit 1`,
		// WHETHER NO OTHER SERVER CAN SPELL THE BRIDGE'S TOOLS, read before the
		// harness reads its settings ([geminiShadowGuard]).
		geminiShadowGuard(paths),
		// THE RECORD, before the harness starts, so a run killed before it
		// reported anything still says which session it took up, and why it
		// began again where it did; the release last ([parseSessionRecord]).
		`{ printf 'id %s\ncontinued %s\nwhy %s\n' "$s" "$k" "$w"; ` + chatRecordFrom +
			`; [ "$u" != yes ] || printf 'unshadowed yes\n'; printf 'release %s\n' "$v"; } > ` +
			q(paths.SessionRecord()),
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
	// A CALL THE HOOK STOPPED FAILS THE RUN: Gemini CLI ends the turn as a
	// success, exit 0 (measured on 0.70.0), so the hook's line joins the
	// stream ([geminiUnbridgedEvent]).
	run := `gemini ` + flags + ` "$@"; code=$?; [ ! -s ` + q(paths.GeminiUnbridged()) + ` ] || cat ` +
		q(paths.GeminiUnbridged())
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
		geminiIncludeRoots(paths),
		// ITS CHAT FILE, named for the live count once Gemini CLI writes it
		// ([geminiPointer]).
		geminiPointer(paths),
		run,
		`kill "$gwp" 2> /dev/null`,
		"node "+q(paths.GeminiHelper())+" usage "+q(paths.GeminiChats())+` "$s" `+before,
		geminiLocate(paths),
		strings.TrimSuffix(chatCapture(paths, paths.GeminiSlugDir(), GeminiCLI{}), "\n"),
		"exit $code"), "\n")
}

// geminiPointer starts, in the background as gwp, a watcher that names in
// [Paths.CountRecord] the chat file of the run's session once Gemini CLI writes
// it, as the helper finds it (gemini_helper.mjs, sessionFile): a file of the
// chats directory named for the session's first eight characters whose first
// line names the session ([GeminiCLI.liveRecord]). None where the list of the
// responses the session held was never written: the helper counts none then.
func geminiPointer(paths Paths) string {
	q := shellword.Quote
	pointer := q(paths.CountRecord())
	return `g8=$(printf '%.8s' "$s"); ( [ -f ` + q(paths.GeminiUsageBefore()) + ` ] || exit 0; while kill -0 $$ 2> /dev/null; do for f in ` + q(paths.GeminiChats()) +
		`/session-*-"$g8".jsonl; do if [ -f "$f" ] && [ ! -L "$f" ] && head -n 1 "$f" | grep -qF "$s"; then ` +
		`printf '%s\n0\n' "$f" > ` + pointer + `.next && mv -f ` + pointer + `.next ` + pointer + `; exit 0; fi; done; ` +
		`sleep 0.1; done ) & gwp=$!`
}

// geminiBridgedPrefix opens the name Gemini CLI gives each tool of the
// engine's own MCP server (`mcp_crewlet_`), as its built-in profile states it
// ([harnessprofile.Profile.ModelToolPrefix]); "" where none is stated, which
// [bridgedToolName] reads as naming no tool, so every call is told.
var geminiBridgedPrefix = sync.OnceValue(func() string {
	p, ok := harnessprofile.Builtin(GeminiCLIName)
	if !ok {
		return ""
	}
	return p.ModelToolPrefix()
})

// geminiBridgeServer is the engine's server's name inside
// [geminiBridgedPrefix]: Gemini CLI names a tool `mcp_<server>_<tool>`.
func geminiBridgeServer() string {
	server, ok := strings.CutPrefix(geminiBridgedPrefix(), "mcp_")
	if !ok {
		return ""
	}
	server, ok = strings.CutSuffix(server, "_")
	if !ok {
		return ""
	}
	return server
}

// geminiShadowGuard is the shell setting u to `yes` where the helper finds
// no MCP server the box's Gemini CLI could load that spells a tool as it
// spells the bridge's (`mcp_crewlet_<tool>`): 0.70.0 registers a later one
// over the bridge's under the same name, and the chat file cannot tell them
// apart (measured), so a reading reads only a run that passed it
// ([GeminiCLI.checkHoldings]). The helper's mode `unshadowed` has the
// sources and the rule.
func geminiShadowGuard(paths Paths) string {
	server := geminiBridgeServer()
	if server == "" {
		return "u="
	}
	return "u=; node " + shellword.Quote(paths.GeminiHelper()) + " unshadowed " + shellword.Quote(server) +
		" > /dev/null 2>&1 && u=yes"
}

// guardsShadows says a Gemini CLI run's script records whether its box passed
// [geminiShadowGuard] ([shadowGuarded]).
func (GeminiCLI) guardsShadows() {}

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

// Events is a new decoder for one stream ([geminiEvents]).
func (GeminiCLI) Events() Decoder { return &geminiEvents{} }

// Parse decodes a whole stream: the result of a run whose result is its
// stream.
func (GeminiCLI) Parse(stdout []byte) sandbox.Result { return decodeAll(&geminiEvents{}, stdout) }

// WriteConfig writes what a run's script and harness read: the run's system
// text, the helper the script runs, the run's settings ([Paths.GeminiSettings]),
// which the script puts into the home's own, its policy and skills in the box
// home beside the home's own, and, where notes or skills are wired, the take
// script and its configuration. It returns "": Gemini CLI reads its settings
// from the home, never an argument.
func (g GeminiCLI) WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, session Session,
	paths Paths) (string, error) {
	files := map[string][]byte{
		paths.GeminiHelper(): geminiHelperScript,
		paths.GeminiPolicy(): []byte(geminiPolicy),
	}
	if servers := crewletServers(req.MCPServers); len(servers) > 0 {
		files[paths.GeminiAdminPolicy()+"/crewlet.toml"] = geminiAdminPolicy(servers)
	}
	if system := geminiSystemText(g, req); system != "" {
		files[paths.System()] = []byte(system)
	}
	hooked := geminiHooked(req)
	if hooked {
		files[paths.GeminiTake()] = geminiTakeScript
	}
	// THE RUN'S WIRING, AND THE SAME FILES UNDER THE JOB'S OTHER BRIDGE TOKEN,
	// for a session the script starts in place of the continued one, beside
	// the first ([sandbox.RunRequest.FreshWiring], [chatSessionGate]): the take
	// configuration naming that session's record of takes. One that changes
	// what the run is hooked for would need another settings file than the
	// one the script chose, so it is refused. Each refusal comes before the
	// box is touched: the note helper's write below is the first command.
	wirings := []geminiWiring{{req: req, settings: paths.GeminiSettings(), take: paths.GeminiTakeConfig(),
		takes: paths.GeminiTakes(session.ID)}}
	if f := req.FreshWiring; f != nil {
		if f.Steer.Set() != req.Steer.Set() || f.SkillLoads.Set() != req.SkillLoads.Set() {
			return "", errors.New("executor: the run's two wirings, for the session it continues and for " +
				"one of its own, are not hooked alike, so the run cannot be wired to whichever session its " +
				"box starts")
		}
		rewired := req
		rewired.MCPServers, rewired.Steer, rewired.SkillLoads = f.MCPServers, f.Steer, f.SkillLoads
		wirings = append(wirings, geminiWiring{req: rewired, settings: paths.GeminiSettings() + ".fresh",
			take: paths.GeminiTakeConfig() + ".fresh", takes: paths.GeminiTakes(session.Fresh)})
	}
	for _, w := range wirings {
		if hooked {
			take, takeErr := geminiTakeConfig(w.req, w.takes)
			if takeErr != nil {
				return "", takeErr
			}
			files[w.take] = take
		}
	}
	for _, s := range req.Skills {
		file, fileErr := skillFile(geminiSkillName(s), s.Description, s.Body)
		if fileErr != nil {
			return "", fileErr
		}
		files[paths.GeminiSkills()+"/"+geminiSkillName(s)+"/SKILL.md"] = file
	}
	helped, err := writeGeminiCheckouts(ctx, box, req, paths)
	if err != nil {
		return "", err
	}
	for _, w := range wirings {
		settings, renderErr := json.MarshalIndent(geminiSettings(w.req, paths, hooked, helped), "", "  ")
		if renderErr != nil {
			return "", fmt.Errorf("executor: rendering Gemini CLI's settings: %w", renderErr)
		}
		files[w.settings] = settings
	}
	// THE LAST RUN'S SKILLS GO FIRST, and only those: the home's skills
	// directory is the person's and the image's too, so the run's are named
	// apart ([geminiSkillName]).
	res, err := box.Exec(ctx, "rm -rf -- "+shellword.Quote(paths.GeminiSkills())+"/"+sandbox.SkillNamespace+"-*",
		sandbox.ExecOptions{})
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	if err != nil {
		return "", fmt.Errorf("executor: clearing the last run's skills for Gemini CLI: %w", err)
	}
	for path, content := range files {
		if err = box.WriteFile(ctx, path, content); err != nil {
			return "", fmt.Errorf("executor: writing %s for Gemini CLI: %w", path, err)
		}
	}
	return "", nil
}

// geminiWiring is one of a run's wirings ([GeminiCLI.WriteConfig]): its
// request, the paths of its settings and its take script's configuration,
// and the session's record of takes that configuration names.
type geminiWiring struct {
	req                   sandbox.RunRequest
	settings, take, takes string
}

// geminiSkillName is the name a run's skill is written and listed under:
// [sandbox.Skill.Listed] with its colon a dash, as Gemini CLI would rewrite it
// (it lists and activates `crewlet:x` only as `crewlet-x`, measured on
// 0.70.0). Written so rather than left to the harness, and mapped back by the
// take script as it reports a load ([geminiSkillReported]).
func geminiSkillName(s sandbox.Skill) string { return sandbox.SkillNamespace + "-" + s.Name }

// geminiSettings is the run's settings, which the script puts into the home's
// own ([Paths.GeminiHomeSettings]): the run's sign-in ([geminiAuth]); usage
// statistics and the update check off (two
// connections to the vendor a run otherwise); subagents off, since the hooks
// fire inside a subagent's loop as in the main agent's, so a note would land
// in delegated work; the bridge's BeforeTool hook ([geminiBridgedHook]); and
// the take script's hooks where wired: AfterTool for
// skill loads, AfterAgent for notes ([GeminiCLISteerInstruction]).
func geminiSettings(req sandbox.RunRequest, paths Paths, hooked, helped bool) map[string]any {
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
	if auth := geminiAuth(req); auth != "" {
		settings["security"] = map[string]any{"auth": map[string]any{"selectedType": auth}}
	}
	if len(req.MCPServers) > 0 {
		servers := make(map[string]any, len(req.MCPServers))
		for name, s := range req.MCPServers {
			servers[name] = geminiMCP(s)
		}
		settings["mcpServers"] = servers
	}
	events := map[string]any{}
	var guards []any
	for _, name := range crewletServers(req.MCPServers) {
		guards = append(guards, geminiBridgedHook(name, req.MCPServers[name].URL, paths))
	}
	if len(guards) > 0 {
		events["BeforeTool"] = guards
	}
	if hooked {
		hook := func() []any {
			return []any{map[string]any{"matcher": "", "hooks": []any{map[string]any{
				"type": "command", "name": geminiHookName, "timeout": geminiHookTimeout.Milliseconds(),
				"command": "node " + shellword.Quote(paths.GeminiTake()) + " " + shellword.Quote(paths.GeminiTakeConfig()),
			}}}}
		}
		if len(req.Skills) > 0 {
			events["AfterTool"] = hook()
		}
		if req.Steer.Set() {
			events["AfterAgent"] = hook()
		}
	}
	if helped {
		after, _ := events["AfterTool"].([]any)
		events["AfterTool"] = append(after, geminiCheckoutsEntry(paths))
	}
	if len(events) > 0 {
		settings["hooks"] = events
	}
	return settings
}

// geminiBridgedHook is the BeforeTool hook on every tool named inside bridge's
// names (`mcp_<bridge>_`): the helper stops the run before the call unless
// Gemini CLI says bridge serves it at url under its own name for it
// (gemini_helper.mjs, `bridged`). GEMINI CLI NAMES A TOOL `mcp_<server>_<tool>`
// and the bridge's names hold `_`, so a server `crewlet_x`, one `mcp` listing
// `crewlet_x`, or another `crewlet` can take a name of the bridge's, from a
// file or a login's admin settings no file shows (MEASURED on 0.70.0). Only the
// call says which server serves it. The address goes as its digest.
func geminiBridgedHook(bridge, url string, paths Paths) map[string]any {
	digest := sha256.Sum256([]byte(url))
	return map[string]any{"matcher": "^mcp_" + bridge + "_", "hooks": []any{map[string]any{
		"type": "command", "name": "crewlet", "timeout": geminiHookTimeout.Milliseconds(),
		"command": "node " + shellword.Quote(paths.GeminiHelper()) + " bridged " + shellword.Quote(bridge) + " " +
			hex.EncodeToString(digest[:]) + " " + shellword.Quote(paths.GeminiUnbridged()),
	}}}
}

// geminiShadowCheck is the script's line refusing, before Gemini CLI starts, a
// run whose settings files or extensions declare a server that would take the
// bridge's place or a name inside its names, naming the file and the server
// (gemini_helper.mjs, `shadows`); "" for a run with no bridge. The run's own
// servers replace the home settings' entries of their names
// ([Paths.GeminiHomeSettings]), so those are passed over.
func geminiShadowCheck(req sandbox.RunRequest, paths Paths) string {
	bridges := crewletServers(req.MCPServers)
	if len(bridges) == 0 {
		return ":"
	}
	var named []string
	for _, name := range slices.Sorted(maps.Keys(req.MCPServers)) {
		named = append(named, "--own", shellword.Quote(name))
	}
	var b strings.Builder
	for i, bridge := range bridges {
		if i > 0 {
			b.WriteString(" && ")
		}
		b.WriteString("node " + shellword.Quote(paths.GeminiHelper()) + " shadows " + shellword.Quote(bridge) + " " +
			strings.Join(named, " "))
	}
	return b.String() + " || exit 1"
}

// geminiAuth is the sign-in a run of req selects: the key's for a run handed
// GEMINI_API_KEY, Vertex for one handed GOOGLE_GENAI_USE_VERTEXAI (a gateway
// base URL would otherwise select the gateway sign-in, which the harness
// refuses; the profile's key_seed_files), "" for a login, which
// GOOGLE_GENAI_USE_GCA selects.
func geminiAuth(req sandbox.RunRequest) string {
	switch {
	case req.Env["GEMINI_API_KEY"] != "":
		return "gemini-api-key"
	case req.Env["GOOGLE_GENAI_USE_VERTEXAI"] == "true":
		return "vertex-ai"
	}
	return ""
}

var _ homeKeeper = GeminiCLI{}

// homeRestore is the shell putting the home's own settings back once Gemini
// CLI has stopped, by the script's end or the next run's clear: the held copy
// written over the run's, through a link as the home keeps it, or neither
// where the home had none; nothing where none is held, so it runs any number
// of times; and the checkouts' copies removed and a skip log no script
// printed dropped ([checkoutHome.restore], [checkoutHome.dropSkipped]).
func (GeminiCLI) homeRestore(paths Paths) string {
	h := geminiCheckoutHome(paths)
	return everyStep(fmt.Sprintf(`if [ -e %[1]s ]; then if [ -s %[1]s ]; then cat %[1]s > %[2]s && rm -f %[1]s; else rm -f %[1]s %[2]s; fi; fi`,
		shellword.Quote(paths.GeminiHeldSettings()), shellword.Quote(paths.GeminiHomeSettings())), h.restore(), h.dropSkipped())
}

// geminiHookName is the name of the run's hook, which a home's or a
// repository's `hooksConfig.disabled` would have to list ([geminiGuard]).
const geminiHookName = "crewlet"

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
// its settings with their tokens among it, each written afresh by the next
// ([Runner.ClearArtifacts]): files alone, so it is no [artifactDirHolder].
// The home's held settings are not among them: the script puts them back, or
// the next run's clear does where this one was killed first ([homeKeeper]). The home's policy file
// and project registry are rewritten by every Gemini CLI run, its `crewlet-`
// skills removed first; the rest of the home is the person's and the image's,
// which no run removes.
func (GeminiCLI) artifactFiles(p Paths) []string {
	return []string{p.GeminiHelper(), p.GeminiTake(), p.GeminiTakeConfig(), p.GeminiTakeConfig() + ".fresh",
		p.GeminiAdminPolicy() + "/crewlet.toml",
		p.GeminiSettings(), p.GeminiSettings() + ".fresh", p.GeminiDefaultSystem(), p.GeminiExportErr(), p.GeminiSystem(),
		p.GeminiUsageBefore(), p.GeminiUsageBefore() + ".part", p.GeminiNoteHelper(), p.GeminiCheckoutLaunch(),
		p.GeminiCheckoutSections(), p.GeminiCheckoutRoots()}
}
