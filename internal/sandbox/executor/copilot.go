package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

func init() { Register(Copilot{}) }

// CopilotName is this runner's config name.
const CopilotName = "copilot"

// Copilot drives GitHub Copilot CLI headless (`copilot`, prompt mode, the brief
// on stdin, `--output-format json`), measured on 1.0.114. Its model is reached through the run ENVIRONMENT (COPILOT_GITHUB_TOKEN, or a
// provider's COPILOT_PROVIDER_* variables), never a file the run writes. Its
// state is the box home's `.copilot` ([Paths.CopilotHome]), where WriteConfig
// writes the run's settings, standing instructions and notes extension; its
// session is `session-state/<id>/`, packed and restored ([copilotSessionGate],
// [copilotPack]). Every tool is approved (`--yolo`), the folder trusted
// ([copilotTrust]) and prompt mode opened ([copilotPromptMode]), so the home's
// and a repository's hooks, MCP servers, extensions and settings load beside
// the run's, as in a person's session, short of what would switch the run's
// own off ([copilotGuard]). Its end on request is end.go's protocol.
type Copilot struct {
	// policy is the managed policy file the guard reads; the zero value is
	// the machine's ([copilotPolicy]).
	policy copilotPolicy
}

var (
	_ Harness           = Copilot{}
	_ carrier           = Copilot{}
	_ saver             = Copilot{}
	_ inboxDelivery     = Copilot{}
	_ artifactHolder    = Copilot{}
	_ artifactDirHolder = Copilot{}
	_ inPlaceGate       = Copilot{}
	_ notesOffExplainer = Copilot{}
	_ sessionJudge      = Copilot{}
	_ recordConfirmer   = Copilot{}
)

// NewCopilot returns the runner.
func NewCopilot() *Runner { return New(Copilot{}) }

// Name is the harness's key in the runner registry.
func (Copilot) Name() string { return CopilotName }

// MCPCallCeiling is none: the server's `timeout` carries the bridge's own
// bound ([copilotMCP]), and a 1040-second call under a 1050-second one was
// answered whole (measured on 1.0.114).
func (Copilot) MCPCallCeiling() time.Duration { return 0 }

// Resumes says Copilot continues a session it reported: `--session-id` on an
// id its home holds resumes it (measured on 1.0.114).
func (Copilot) Resumes() bool { return true }

// NoteInstruction is the standing instruction about notes: a run takes a
// person's notes between its steps through its extension (copilot_steer.js),
// which [Copilot.WriteConfig] writes where the run is wired to them.
func (Copilot) NoteInstruction() string { return CopilotSteerInstruction }

// copilotFloor is the one Copilot release supported, the newest measured, as
// the built-in profile's written_for names it (1.0.114): every behaviour here
// was measured on it. A run is started on no older one, and a session, the
// box's own copy as much as a carried one, continues only on exactly it. A
// release moved to re-reads its loader's environment (sea-loader.js) for every
// variable that runs other code than the pinned package ([copilotSwitchesOff]).
var copilotFloor = sync.OnceValue(func() string {
	p, _ := harnessprofile.Builtin(CopilotName)
	return harnessprofile.Release(p.WrittenFor)
})

// copilotSessionState is the directory under the run's home that Copilot files
// each session's directory in.
const copilotSessionState = "session-state"

// Session is the session a Copilot run works in ([Harness.Session]): the one it
// continues, or a new one under a UUID minted for it — `--session-id` starts a
// session under a UUID it is handed — and, for a run that continues one, a
// second UUID for the session it starts instead where its box holds that one
// neither way.
func (Copilot) Session(req sandbox.RunRequest) Session {
	if req.Resume != "" {
		return Session{ID: req.Resume, Continues: true, Fresh: uuid.NewString(),
			Replaces: req.Carried != nil && req.Carried.Replace}
	}
	return Session{ID: uuid.NewString()}
}

// Command is the run as ONE SCRIPT ([copilotScript]), so the wrapper's
// redirections (the brief on stdin, the stream, the error file) reach each step.
func (Copilot) Command(req sandbox.RunRequest, session Session, paths Paths, configPath string) string {
	return "sh -c " + shellword.Quote(copilotScript(req, session, paths, configPath, sandbox.JobStopGrace))
}

// copilotScript is a Copilot run's script, giving its harness stop to go once
// asked to end: the end protocol (end.go); the release read and held to
// [copilotFloor]; the provider variables the entry did not set removed
// ([copilotProviderOff]); its GitHub sign-in handed under one name
// ([copilotSignIn]); the session named and recorded ([copilotSessionGate],
// [Paths.SessionRecord]); the harness run under the end protocol
// ([launchHarness]); the run's own usage records added to the stream
// ([copilotUsage]); and its session packed ([copilotCapture]), exiting with the
// harness's status.
//
// THE BRIEF IS NOT ON ITS COMMAND LINE: prompt mode with no `-p` reads stdin,
// which the wrapper points at the brief's file, as Claude Code's does.
func copilotScript(req sandbox.RunRequest, session Session, paths Paths, configPath string,
	stop time.Duration) string {
	q := shellword.Quote
	parts := []string{
		"copilot",
		"--output-format", "json",
		// Every tool, path and URL approved: a headless run has nobody to
		// answer a prompt. A `--deny-tool` rule, an organisation's policy and
		// the home's disableBypassPermissionsMode still win ([copilotGuard]).
		"--yolo",
		// ask_user is not offered in prompt mode; the flag holds that whatever
		// a later release does. ask_and_wait is on the bridge.
		"--no-ask-user",
		"--no-auto-update", "--disable-client-telemetry",
		// The GitHub MCP server Copilot starts on its own is no tool of the
		// seat's: the seat's tools are its bridge's.
		"--disable-builtin-mcps",
		// The session stays in the box: synced, it would sit on the
		// account's history too.
		"--sync-session=off",
	}
	if configPath != "" {
		parts = append(parts, "--additional-mcp-config", q("@"+configPath))
		// EVERY ENGINE SERVER ENABLED BY NAME, over a trusted repository's
		// `disabledMcpServers`, which otherwise switches the bridge off
		// (measured on 1.0.114).
		for _, name := range slices.Sorted(maps.Keys(req.MCPServers)) {
			parts = append(parts, "--enable-mcp-server", q(name))
		}
	}
	// VERBATIM, as the copilot entry names it in Copilot's own terms.
	if req.Model != "" {
		parts = append(parts, "--model", q(req.Model))
	}
	if req.Effort != "" {
		parts = append(parts, "--reasoning-effort", q(req.Effort))
	}
	// THE ENTRY'S WEB TOOLS OFF where it denies them ([sandbox.RunRequest.WebDenied]),
	// and GitHub's code search, the one other tool that reached the network:
	// excluded, they are offered to no agent of the run. Measured on 1.0.114
	// under --yolo: a call was answered "does not exist" in the run, in its
	// general-purpose and research subagents and in a repository's own agent
	// listing them. --deny-tool would not do: --yolo allows every URL, and a
	// denied web_fetch still fetched. Each value after a flag of its own.
	if req.WebDenied {
		for _, tool := range []string{"web_fetch", "web_search", "semantic_code_search"} {
			parts = append(parts, "--excluded-tools", tool)
		}
	}
	parts = append(parts, `"$@"`)

	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	// THE HOME'S OWN SETTINGS AND INSTRUCTIONS BACK however the script exits;
	// a run killed first leaves them to the next run's clear ([homeKeeper]).
	line(`cput() { %s; }; trap cput EXIT`, Copilot{}.homeRestore(paths))
	b.WriteString(endPrologue(paths, stop))
	b.WriteString(endHarness(""))
	b.WriteString(copilotRelease)
	line(`if ! at %s; then printf 'crewlet: this box runs GitHub Copilot CLI %%s, older than %s, the oldest a run is started on\n' "$n" >&2; exit 1; fi`,
		floorArgs(copilotFloor()), copilotFloor())
	if off := copilotProviderOff(req.Env); len(off) > 0 {
		line(`unset %s`, strings.Join(off, " "))
	}
	b.WriteString(copilotSignIn())
	// THE CAP NAMED IN THE STREAM, for the decoder to fail a run whose
	// extension, which enforces it, never ran ([copilotEvents]).
	if req.Limits.MaxTurns > 0 {
		line(`printf '%%s\n' %s`, q(copilotCapLine(req.Limits.MaxTurns)))
	}
	line(`set --`)
	b.WriteString(copilotSessionGate(session, paths, req.FreshWiring != nil && configPath != ""))
	line(`printf 'id %%s\ncontinued %%s\nwhy %%s\nrelease %%s\n' "$s" "$k" "$w" "$v" > %s`,
		q(paths.SessionRecord()))
	// THE RECORD'S LENGTH BEFORE THE RUN, which the usage after it is read past.
	line(`ev=%s/"$s"/events.jsonl; el=0; if [ -f "$ev" ]; then el=$(wc -l < "$ev") || el=0; fi`,
		q(paths.CopilotHome()+"/"+copilotSessionState))
	b.WriteString(launchHarness(strings.Join(parts, " "), paths))
	b.WriteString(copilotUsage())
	b.WriteString(copilotCapture(paths))
	line(`exit "$code"`)
	return b.String()
}

// copilotRelease opens every Copilot run's script, as [claudeRelease] opens
// Claude Code's: `copilot --version` read once as v (`GitHub Copilot CLI
// 1.0.114.`), its numbers as n, and `at MAJOR MINOR PATCH`. A VERSION IT CANNOT
// READ IS TAKEN FOR A CURRENT RELEASE, and a missing harness fails at the run.
const copilotRelease = `v=$(copilot --version < /dev/null 2> /dev/null | head -n 1); n=${v#"${v%%[0-9]*}"}; n=${n%%[!0-9.]*}; n=${n%.}
a=${n%%.*}; r=${n#*.}; b=${r%%.*}; c=${r#*.}; c=${c%%.*}
case "$n" in [0-9]*.[0-9]*.[0-9]*) ;; *) a= ;; esac
at() { [ -z "$a" ] || [ "$a" -gt "$1" ] || { [ "$a" -eq "$1" ] && { [ "$b" -gt "$2" ] || { [ "$b" -eq "$2" ] && [ "$c" -ge "$3" ]; }; }; }; }
`

// copilotProviders is every variable that routes Copilot's model calls to a
// provider of the box's own (`copilot help environment`, 1.0.114): set, it
// replaces the entry's sign-in. COPILOT_PROVIDERS_CONFIG is a file's path (a
// document in its place is not read, measured): a providers file is the box's
// own; its path is the entry's identity, and its contents may change under it.
// An Azure project's path is part of the route, so two projects on one host
// are two routes. COPILOT_PROVIDER_HEADERS can carry routing, and is left out
// of the route as a sign variable, since it carries sign-in. The transport
// shapes calls to the same endpoint (`help environment`).
var copilotProviders = slices.Concat(
	routes(roleEndpoint, "COPILOT_PROVIDERS_CONFIG", "COPILOT_PROVIDER_BASE_URL", "COPILOT_PROVIDER_TYPE",
		"COPILOT_PROVIDER_WIRE_API"),
	routes(roleSigns, "COPILOT_PROVIDER_API_KEY", "COPILOT_PROVIDER_API_KEY_COMMAND",
		"COPILOT_PROVIDER_BEARER_TOKEN", "COPILOT_PROVIDER_HEADERS"),
	routes(roleShapes, "COPILOT_PROVIDER_TRANSPORT", "COPILOT_PROVIDER_AZURE_API_VERSION",
		"COPILOT_PROVIDER_MODEL_ID", "COPILOT_PROVIDER_WIRE_MODEL", "COPILOT_PROVIDER_MAX_PROMPT_TOKENS",
		"COPILOT_PROVIDER_MAX_OUTPUT_TOKENS", "COPILOT_PROVIDER_VISION"),
)

// copilotHosts is where Copilot's GitHub sign-in and API requests go
// (`copilot help environment`, 1.0.114), left as the box or the entry sets
// them: the agent's gh reads GH_HOST too.
var copilotHosts = routes(roleEndpoint, "COPILOT_GH_HOST", "GH_HOST")

// routing answers [router] with [copilotProviders] and [copilotHosts].
func (Copilot) routing() []routeVar { return slices.Concat(copilotProviders, copilotHosts) }

// copilotProviderOff is every provider variable ([copilotProviders]) the run's
// own environment (env, the entry's) does not set, which the script unsets:
// one from the image or a login profile would send the seat's model calls to a
// provider its entry never named. UNSET IN THE SCRIPT, not by [Output.Unset],
// which cannot tell the entry's from the profile's, and never pinned, which
// would write a key into the script.
func copilotProviderOff(env map[string]string) []string {
	var off []string
	for _, v := range copilotProviders {
		if _, set := env[v.Name]; !set {
			off = append(off, v.Name)
		}
	}
	return off
}

// copilotSignInNames is every variable Copilot signs in to GitHub by, in the
// order it takes them (`copilot help environment`, 1.0.114).
var copilotSignInNames = []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"}

// copilotHeldToken is Copilot's own names for the GitHub token it holds, which
// it sets itself and no entry is given.
var copilotHeldToken = []string{"GITHUB_COPILOT_GITHUB_TOKEN", "GITHUB_COPILOT_AGENT_GITHUB_TOKEN"}

// copilotSignIn is the part of a run's script that hands Copilot its GitHub
// sign-in under COPILOT_GITHUB_TOKEN: the first of [copilotSignInNames] set,
// in Copilot's own order, so the same token signs it in, with
// [copilotHeldToken] unset. The other two names stay as the run set them, so
// the model's shell holds them as a person's would.
func copilotSignIn() string {
	return "gt=${COPILOT_GITHUB_TOKEN:-${GH_TOKEN:-$GITHUB_TOKEN}}\n" +
		"unset " + strings.Join(copilotHeldToken, " ") + "\n" +
		`if [ -n "$gt" ]; then COPILOT_GITHUB_TOKEN=$gt; export COPILOT_GITHUB_TOKEN; fi; unset gt` + "\n"
}

// copilotSessionGate is the part of a run's script that names the session its
// harness runs in (s), whether it continues the one asked (k) and, where it
// could not, why (w), adding `--session-id`, which starts or resumes it. As
// [claudeSessionGate]: the box's copy is continued unless a carried archive
// holds what it does not (a digest not in [Paths.SessionDigests]); that archive
// is restored once it holds an events.jsonl, its digest recorded; neither held
// nor restored, the minted session starts and says why.
//
// NO LINE IS CUT: Copilot repairs a torn last line of events.jsonl itself,
// keeping a backup (measured on 1.0.114), and `--session-id` resumes in the
// launch's working directory whatever workspace.yaml recorded.
func copilotSessionGate(session Session, paths Paths, rewired bool) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := shellword.Quote
	if !session.Continues {
		line(`s=%s k=no w=`, q(session.ID))
		line(`set -- "$@" --session-id %s`, q(session.ID))
		return b.String()
	}
	home := paths.CopilotHome()
	held := paths.CopilotSessionDir(session.ID)
	staged := paths.SessionStaging()
	carried := q(paths.SessionCarried())
	in := staged + "/in"
	from := in + "/" + copilotSessionState + "/" + session.ID
	restore := strings.Join([]string{
		"rm -rf " + q(staged),
		"mkdir -p " + q(in) + " " + q(home+"/"+copilotSessionState),
		"tar -xzf " + carried + " -C " + q(in) + " < /dev/null > /dev/null 2>&1",
		"[ -f " + q(from+"/events.jsonl") + " ]",
		"[ ! -L " + q(from+"/events.jsonl") + " ]",
		"[ ! -L " + q(from) + " ]",
		// THE BOX'S OWN COPY GOES only now, once the archive is known to hold
		// a conversation.
		"rm -rf " + q(held),
		"mkdir -p " + q(held),
		"mv " + q(from+"/events.jsonl") + " " + q(held+"/events.jsonl"),
	}, " && ")
	line(`s=%s k=yes w= h= d=`, q(session.ID))
	line(`[ -s %s ] && h=1`, q(held+"/events.jsonl"))
	line(`if [ -s %s ]; then d=$(cksum < %s 2> /dev/null) || d=; fi`, carried, carried)
	if session.Replaces {
		line(`if [ -n "$h" ] && [ -s %s ]; then h=; fi`, carried)
	} else {
		line(`if [ -n "$h" ] && [ -n "$d" ] && ! grep -qxF "$s $d" %s 2> /dev/null; then h=; fi`,
			q(paths.SessionDigests()))
	}
	// THE BOX'S RELEASE, for the copy it holds as for an archive, held as the
	// engine's check holds it ([Copilot.continuesInPlace]): the bare release
	// (rv, [bareReleaseShell]) exactly [copilotFloor], else nothing continues.
	line(`%s[ "$rv" = %s ] || w="this box runs GitHub Copilot CLI ${rv:-a release it could not read}, and a session continues only on %s, the release supported, the box's own copy included"`,
		bareReleaseShell("rv"), copilotFloor(), copilotFloor())
	line(`if [ -z "$w" ] && [ -z "$h" ]; then`)
	line(`  if [ ! -s %s ]; then w='it is not in this box, and no archive of it was carried here'`, carried)
	line(`  elif ! command -v tar > /dev/null 2>&1 || ! command -v gzip > /dev/null 2>&1; then w='this box has no tar and gzip to restore its archive with'`)
	line(`  elif ! { %s; }; then w='its archive could not be restored'`, restore)
	// Continued only once its digest is recorded and read back, for
	// [claudeSessionGate]'s reason.
	line(`  elif ! { %s; }; then w=%s`, recordDigest(paths, "s"), q(restoreDigestUnrecorded))
	line(`  fi`)
	line(`  rm -rf %s`, q(staged))
	line(`fi`)
	line(`rm -f %s`, carried)
	line(`if [ -z "$w" ]; then`)
	line(`  set -- "$@" --session-id %s`, q(session.ID))
	if rewired {
		line(`  rm -f %s %s`, q(paths.MCPConfigFresh()), q(paths.CopilotExtensionFresh()))
	}
	line(`else`)
	line(`  s=%s k=no`, q(session.Fresh))
	line(`  printf '%%s%%s\n' %s "$w" >&2`, q("crewlet: this run was asked to continue the Copilot session "+
		session.ID+", and starts a new session, "+session.Fresh+", instead: "))
	line(`  set -- "$@" --session-id %s`, q(session.Fresh))
	line(`  [ ! -s %s ] || exec < %s`, q(paths.FreshBrief()), q(paths.FreshBrief()))
	if rewired {
		// THE NEW SESSION WIRED TO ITS OWN TOKEN, the bridge's and the notes
		// route's alike, or not started: [claudeSessionGate]'s reason.
		line(`  mv -f %s %s || { echo 'crewlet: the new session could not be wired to the tools of its seat, so the run does not start' >&2; exit 1; }`,
			q(paths.MCPConfigFresh()), q(paths.MCPConfig()))
		line(`  if [ -f %s ]; then mv -f %s %s || { echo 'crewlet: the new session could not be wired to its notes, so the run does not start' >&2; exit 1; }; fi`,
			q(paths.CopilotExtensionFresh()), q(paths.CopilotExtensionFresh()), q(paths.CopilotExtension()+"/extension.mjs"))
	}
	line(`fi`)
	return b.String()
}

// copilotUsageMark is the type of the line a run's script adds to its stream
// once its harness exits, after which every line is one of the run's own
// `session.usage_record`s ([copilotUsage]).
const copilotUsageMark = "crewlet.usage"

// copilotUsage is the part of a run's script that adds to its stream (the
// script's stdout) the run's own usage, once its harness has exited: the mark
// ([copilotUsageMark]), then every `session.usage_record` of the session's
// events.jsonl after the last `session.start` or `session.resume` — the one this
// process wrote. Copilot prints neither to its stream, and its result states no
// tokens, so these are the run's spend ([copilotEvents.runSpend]).
//
// ONLY A START THIS PROCESS WROTE: one at or before the line count kept before
// the launch (el) is an earlier process's, and a run whose harness wrote none
// is marked with no record, so it spent nothing rather than a run before it.
// A box with no awk adds no mark, and says so: its spend is not stated.
func copilotUsage() string {
	return `if command -v awk > /dev/null 2>&1; then
  printf '%s\n' '{"type":"` + copilotUsageMark + `"}'
  if [ -f "$ev" ]; then
    m=$(awk 'index($0, "{\"type\":\"session.start\"") == 1 || index($0, "{\"type\":\"session.resume\"") == 1 { n = NR } END { print n + 0 }' "$ev" 2> /dev/null) || m=0
    if [ "${m:-0}" -gt "$((el))" ]; then awk -v n="$m" 'NR > n && index($0, "{\"type\":\"session.usage_record\"") == 1' "$ev" 2> /dev/null; fi
  fi
else
  echo 'crewlet: this box has no awk, so the run'"'"'s own usage was not read from its session' >&2
fi
`
}

// copilotArchiveFormat is the layout a Copilot run packs its session in
// ([copilotPack]) and the one it restores ([copilotSessionGate]): one gzipped
// tar of one member named relative to the run's home,
// `session-state/<session>/events.jsonl`, the conversation, which a resume
// takes up whole on its own (measured on 1.0.114). A change is a new format.
const copilotArchiveFormat = 1

// ArchiveFormat is the layout a Copilot run packs its session in ([carrier]).
func (Copilot) ArchiveFormat() int { return copilotArchiveFormat }

// VersionCommand prints a box's Copilot release ([carrier]).
func (Copilot) VersionCommand() string { return "copilot --version" }

// layout is a Copilot archive's: a gzipped tar of the session's files.
func (Copilot) layout() archiveLayout { return archiveTar }

// charges is false: its meter counts nothing ([carrier.charges]).
func (Copilot) charges(string, string) bool { return false }

// Continues reports whether a session written by Copilot release writer can be
// continued in a box whose Copilot is release box ([carrier]): only where both
// are [copilotFloor], the one release supported. Any other pair, a release
// that cannot be read included, starts the session fresh.
func (c Copilot) Continues(box, writer string) (bool, string) {
	if !sameRelease(box, copilotFloor()) {
		return false, fmt.Sprintf("this box runs GitHub Copilot CLI %q, and a session carried from another "+
			"box continues only on %s, the release supported", box, copilotFloor())
	}
	return c.ContinuesWriter(writer)
}

// continuesInPlace reports whether a box whose Copilot is release box
// continues a session at all, the copy it holds as much as one carried to it
// ([inPlaceGate]): only on [copilotFloor], as the run's own gate holds it
// ([copilotSessionGate]), so the engine asks before a relaunch hands the
// brief that continues the box's copy.
func (Copilot) continuesInPlace(box string) (bool, string) {
	if !sameRelease(box, copilotFloor()) {
		return false, fmt.Sprintf(copilotBoxRefused, box, copilotFloor())
	}
	return true, ""
}

// copilotBoxRefused is why a box whose Copilot is not [copilotFloor]
// continues no session, its own copy nor a carried one: the engine's check and
// the run's gate say it alike.
const copilotBoxRefused = "this box runs GitHub Copilot CLI %q, and a session continues only on %s, the " +
	"release supported, the box's own copy included"

// ContinuesWriter is [Copilot.Continues] judged of the writer alone
// ([carrier]): only a session [copilotFloor] wrote.
func (Copilot) ContinuesWriter(writer string) (bool, string) {
	if !sameRelease(writer, copilotFloor()) {
		return false, fmt.Sprintf("the session was written by GitHub Copilot CLI %q, and only a session "+
			"%s wrote continues, the release supported", writer, copilotFloor())
	}
	return true, ""
}

// meter is the take's meter of a Copilot archive ([carrier.meter]): no figure.
// This build reads no count or estimate of a Copilot session's next request
// off its archive. The archive is left to the caller, which drains it
// ([startMeter]).
func (Copilot) meter(context.Context, io.Reader, string, string, *passBudget) (meterReading, error) {
	return meterReading{}, nil
}

// sameRelease reports whether version is release, false where it cannot be read.
func sameRelease(version, release string) bool {
	have, ok := releaseNumbers(version)
	want, _ := releaseNumbers(release)
	return ok && have == want
}

// copilotCapture is the part of a run's script that packs its session once its
// harness has exited ([copilotPack]).
func copilotCapture(paths Paths) string {
	return copilotPack(paths, packAt{staged: paths.SessionStaging(), dest: paths.SessionCapture()})
}

// copilotPack packs the session s names into at.staged and moves it over
// at.dest, its digest kept ([packDigest]): its events.jsonl alone. Copilot
// writes workspace.yaml and checkpoints/ again on a resume (measured on 1.0.114).
// NEVER session.db or its -journal, the sql tool's tables, so that tool's data
// does not carry to a continued session: a binary SQLite file cannot pass the
// archive's text scrub without being corrupted. A pack beside a working harness
// is held to [claudeHeldTar]'s rule.
func copilotPack(paths Paths, at packAt) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := shellword.Quote
	home := paths.CopilotHome()
	dir := copilotSessionState + `/"$s"`
	line(`if [ -s %s/%s/events.jsonl ]; then`, q(home), dir)
	line(`  if command -v tar > /dev/null 2>&1 && command -v gzip > /dev/null 2>&1; then`)
	line(`    set -- %s/events.jsonl`, dir)
	tar := fmt.Sprintf(`{ tar -czf %s -C %s "$@" < /dev/null > /dev/null 2> %s; t=$?; [ "$t" = 0 ] || { [ "$t" = 1 ] && grep -q 'changed as we read it' %s; }; }`,
		q(at.staged+"/capture"), q(home), q(at.staged+"/error"), q(at.staged+"/error"))
	if at.working {
		tar = claudeHeldTar(home, at.staged)
	}
	line(`    rm -rf %s; if mkdir -p %s && %s && %s && mv -f %s %s; then packed=1; else echo "crewlet: the session $s was not packed to be continued in another box: $(cat %s 2> /dev/null)" >&2; fi`,
		q(at.staged), q(at.staged), tar, packDigest(paths, "s", at.staged), q(at.staged+"/capture"), q(at.dest),
		q(at.staged+"/error"))
	line(`    rm -rf %s`, q(at.staged))
	line(`  else`)
	line(`    echo 'crewlet: this box has no tar and gzip, so the session was not packed to be continued in another box' >&2`)
	line(`  fi`)
	line(`fi`)
	return b.String()
}

// Pack packs session now ([carrier]), as a run's script does once its harness
// exits, into staged and over dest: [packNothing] where the box holds no
// events.jsonl of it, [packFailed] where no archive was made. Held to the rule
// of a pack beside a working harness where working says so.
func (Copilot) Pack(paths Paths, session, staged, dest string, working bool) string {
	return copilotOnDemand(paths, session, packAt{staged: staged, dest: dest, working: working})
}

// PackSave packs a save of rec's session beside a harness that may still be
// working ([saver]): events.jsonl is append-only, so the rule of a pack beside
// a working harness holds it ([claudeHeldTar]). The record refuses no save.
func (Copilot) PackSave(paths Paths, rec sessionRecord, staged, dest string) (string, error) {
	return copilotOnDemand(paths, rec.id, packAt{staged: staged, dest: dest, working: true}), nil
}

// copilotOnDemand is a pack the engine makes of session outside the run's own
// script, at at.
func copilotOnDemand(paths Paths, session string, at packAt) string {
	return "s=" + shellword.Quote(session) + " packed=\n" +
		fmt.Sprintf("[ -s %s ] || exit %d\n", shellword.Quote(paths.CopilotSessionDir(session)+"/events.jsonl"), packNothing) +
		copilotPack(paths, at) +
		fmt.Sprintf("[ -n \"$packed\" ] || exit %d\n", packFailed)
}

// copilotSwitchesOff is every variable that changes how a Copilot run reaches
// its model, how long it waits or what code runs, removed whatever set it
// (1.0.114): COPILOT_OFFLINE cuts it off its model; the compaction thresholds
// and COPILOT_TASK_WAIT_TIMEOUT_SECONDS move compaction and the wait for
// background agents; COPILOT_ASSISTED_APPROVAL puts a judge before approvals;
// and the binary's loader (sea-loader.js) runs code not its own package's
// where COPILOT_CLI_VERSION picks another cached release, COPILOT_CLI_DIST_DIR
// a directory whose index.js it runs, or COPILOT_SEA_EXTRACT_ONLY has it
// unpack and exit (measured). A person's instruction and skill directories
// and the loader's cache homes, which hold its own package, stay.
var copilotSwitchesOff = []string{
	"COPILOT_OFFLINE", "COPILOT_BACKGROUND_COMPACTION_THRESHOLD", "COPILOT_BUFFER_EXHAUSTION_THRESHOLD",
	"COPILOT_TASK_WAIT_TIMEOUT_SECONDS", "COPILOT_ASSISTED_APPROVAL",
	"COPILOT_CLI_VERSION", "COPILOT_CLI_DIST_DIR", "COPILOT_SEA_EXTRACT_ONLY",
}

// Output is where a job writes under `--output-format json`: its stream is its
// result, decoded whole or followed ([copilotEvents]), with the lines the
// script adds after it ([copilotUsage]). The brief on stdin.
func (Copilot) Output(paths Paths) Output {
	return Output{Stdout: paths.Result(), Events: true, BriefOnStdin: true, Unset: copilotSwitchesOff}
}

// copilotTrust is how a Copilot run's folder is trusted: COPILOT_ALLOW_ALL set
// to exactly `true`, which trusts the working directory and approves every
// tool. `--trust-folder` is refused beside `--session-id`, and trustedFolders
// would mean rewriting the login's config.json. MEASURED TO WIDEN NOTHING
// BEYOND `--yolo` on 1.0.114: a `--deny-tool` rule (an MCP tool's, a shell
// command's) and a `--deny-url` still refuse, and the repository's MCP servers,
// hooks and skills load exactly as under trustedFolders.
const copilotTrust = "true"

// copilotPromptMode is the variables that load in prompt mode what an
// interactive session loads from a trusted repository: its hooks, its MCP
// servers and its extensions. Each is read as exactly `true` (the 1.0.114
// bundle); the extensions' alone has no other switch in prompt mode, where
// COPILOT_ALLOW_ALL loads the other two as well.
var copilotPromptMode = []string{"GITHUB_COPILOT_PROMPT_MODE_REPO_HOOKS",
	"GITHUB_COPILOT_PROMPT_MODE_WORKSPACE_MCP", "GITHUB_COPILOT_PROMPT_MODE_EXTENSIONS"}

// Pins is the run's home and posture, over whatever the seat's env, the image
// or a profile set: COPILOT_HOME ([Paths.CopilotHome]); updates, the client's
// telemetry and the MCP tool cache off (a cached list showed the previous run's
// tools, measured); the folder trusted ([copilotTrust]); and a repository's
// hooks, MCP servers and extensions loaded ([copilotPromptMode]).
func (Copilot) Pins(_ context.Context, _ sandbox.RunRequest, paths Paths) []Pinned {
	pins := []Pinned{
		{Name: "COPILOT_HOME", Value: paths.CopilotHome()},
		{Name: "COPILOT_AUTO_UPDATE", Value: "false"},
		{Name: "COPILOT_TELEMETRY_DISABLED", Value: "true"},
		{Name: "COPILOT_MCP_TOOL_CACHE", Value: "false"},
		{Name: "COPILOT_ALLOW_ALL", Value: copilotTrust},
	}
	for _, name := range copilotPromptMode {
		pins = append(pins, Pinned{Name: name, Value: "true"})
	}
	return pins
}

// Events is a new decoder for one stream, read whole or followed ([Decoder]).
func (Copilot) Events() Decoder { return &copilotEvents{} }

// Parse decodes a whole stream already in hand: the result of a run whose
// result IS its stream.
func (Copilot) Parse(stdout []byte) sandbox.Result { return decodeAll(&copilotEvents{}, stdout) }

// copilotSettings is the keys a run sets in the home's user-level settings
// ([Paths.CopilotSettings], [copilotMergedSettings]): `experimental`, without
// which 1.0.114 loads no extension in prompt mode (measured), so neither the
// notes nor the turn cap would run. A repository's `experimental: false` does
// not reach it: extension settings are read from the user's settings alone
// (measured).
type copilotSettings struct {
	Experimental bool `json:"experimental"`
}

// WriteConfig writes what a Copilot run reads from its home and its work
// directory, once the guard has passed the run ([copilotGuard]): its
// settings' keys ([copilotMergedSettings]); its standing instructions
// ([Paths.CopilotInstructions]), told of notes where it is wired to them; its
// extension where it reads notes or has a turn cap
// ([Paths.CopilotExtension]); and its MCP servers, returning the file
// `--additional-mcp-config` names, which Copilot adds to the home's own
// `mcp-config.json`, with a fresh session's copies beside them
// ([sandbox.RunRequest.FreshWiring]). What an earlier run wrote there the
// clear removes first ([Copilot.artifactFiles], [Copilot.artifactDirs]).
func (c Copilot) WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, _ Session,
	paths Paths) (string, error) {
	settings, err := copilotMergedSettings(ctx, box, paths.CopilotSettings(), copilotSettings{Experimental: true})
	if err != nil {
		return "", err
	}
	// THE GUARD BEFORE ANY WRITE: a refused run leaves the home as it found it.
	if err = copilotGuard(ctx, box, req, paths, c.policy, settings); err != nil {
		return "", err
	}
	if _, err = holdHome(ctx, box, paths.CopilotSettings(), paths.CopilotHeldSettings()); err != nil {
		return "", err
	}
	if err = writeHome(ctx, box, paths, paths.CopilotSettings(), settings); err != nil {
		return "", fmt.Errorf("executor: writing Copilot's settings: %w", err)
	}
	system := req.System
	if req.Steer.Set() {
		system = withNoteInstruction(system, c.NoteInstruction())
	}
	if system != "" {
		// THE PERSON'S OWN INSTRUCTIONS STILL LOAD, after the run's, in the one
		// file Copilot reads them from; the home's file comes back as the run ends.
		own, err := holdHome(ctx, box, paths.CopilotInstructions(), paths.CopilotHeldInstructions())
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(own) != "" {
			system += "\n\n" + own
		}
		if err := writeHome(ctx, box, paths, paths.CopilotInstructions(), []byte(system)); err != nil {
			return "", fmt.Errorf("executor: writing Copilot's standing instructions: %w", err)
		}
	}
	if req.Steer.Set() || req.Limits.MaxTurns > 0 {
		if err := writeCopilotExtension(ctx, box, paths.CopilotExtension()+"/extension.mjs", req.Steer,
			req.Limits.MaxTurns); err != nil {
			return "", err
		}
		if f := req.FreshWiring; f != nil {
			if err := writeCopilotExtension(ctx, box, paths.CopilotExtensionFresh(), f.Steer,
				req.Limits.MaxTurns); err != nil {
				return "", err
			}
		}
	}
	if len(req.MCPServers) == 0 {
		return "", nil
	}
	if err := writeJSON(ctx, box, paths.MCPConfig(), copilotMCPConfig(req.MCPServers)); err != nil {
		return "", fmt.Errorf("executor: writing Copilot's MCP servers: %w", err)
	}
	if f := req.FreshWiring; f != nil {
		if err := writeJSON(ctx, box, paths.MCPConfigFresh(), copilotMCPConfig(f.MCPServers)); err != nil {
			return "", fmt.Errorf("executor: writing Copilot's MCP servers for a new session: %w", err)
		}
	}
	return paths.MCPConfig(), nil
}

// copilotMergedSettings is file, the home's user-level settings, with own's
// keys ([copilotSettings]) set in it, keeping every other key a person or the
// image put there: Copilot reads user settings from that one file and has no
// layer a run could add beside it (1.0.114), so the run's keys are merged in.
// A file holding no JSON object is refused, naming it, rather than replaced.
func copilotMergedSettings(ctx context.Context, box sandbox.Sandbox, file string,
	own copilotSettings) ([]byte, error) {
	raw, err := box.ReadFile(ctx, file)
	if err != nil {
		return nil, fmt.Errorf("executor: reading Copilot's settings %s: %w", file, err)
	}
	settings := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err = json.Unmarshal(raw, &settings); err != nil || settings == nil {
			return nil, fmt.Errorf("executor: Copilot's settings %s hold no JSON object (%v): fix the file, "+
				"or remove it from the box", file, err)
		}
	}
	keys, err := json.Marshal(own)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(keys, &settings); err != nil {
		return nil, err
	}
	return json.MarshalIndent(settings, "", "  ")
}

// writeCopilotExtension writes the extension, wired to steer's notes route
// where it is set and to the turn cap, to file.
func writeCopilotExtension(ctx context.Context, box sandbox.Sandbox, file string, steer sandbox.SteerWiring,
	turns int) error {
	settings := copilotExtensionSettings{Cap: turns}
	if steer.Set() {
		settings.URL = steer.URL
	}
	source, err := copilotExtension(settings)
	if err != nil {
		return err
	}
	if err := box.WriteFile(ctx, file, []byte(source)); err != nil {
		return fmt.Errorf("executor: writing the Copilot run's notes extension: %w", err)
	}
	return nil
}

// copilotMCPConfig is servers in Copilot's own MCP vocabulary, under
// `mcpServers` ([copilotMCP]).
func copilotMCPConfig(servers map[string]sandbox.MCPServer) map[string]any {
	rendered := make(map[string]any, len(servers))
	for name, s := range servers {
		rendered[name] = copilotMCP(s)
	}
	return map[string]any{"mcpServers": rendered}
}

// copilotMCP is one server as `--additional-mcp-config` reads it (1.0.114): an
// HTTP server `type: http` with its url and headers, a command `type: local`;
// every one with `tools: ["*"]`, since Copilot offers a server only the tools
// it lists; `timeout` in milliseconds where the engine sets a wait (a call is
// cancelled at it, measured). Copilot has no `alwaysLoad`: no server's tools
// were deferred under a provider of the box's own (measured); a hosted model's
// deferral was not measured against GitHub's service.
func copilotMCP(s sandbox.MCPServer) map[string]any {
	var out map[string]any
	if s.Transport == sandbox.TransportHTTP {
		out = map[string]any{"type": string(sandbox.TransportHTTP), "url": s.URL}
		if len(s.Headers) > 0 {
			out["headers"] = s.Headers
		}
	} else {
		out = map[string]any{"type": "local", "command": s.Command, "args": s.Args}
		if len(s.Env) > 0 {
			out["env"] = s.Env
		}
	}
	out["tools"] = []string{"*"}
	if ms := waitMillis(s.CallTimeout); ms > 0 {
		out["timeout"] = ms
	}
	return out
}

// exportsTelemetry: Copilot CLI turns its OpenTelemetry export on where
// OTEL_EXPORTER_OTLP_ENDPOINT is set, and files its spans under
// OTEL_RESOURCE_ATTRIBUTES (`copilot help environment`, 1.0.114).
func (Copilot) exportsTelemetry() bool { return true }

// artifactFiles is the files a Copilot run leaves in its box of its own: a
// fresh session's extension and the guard's settings beside the work
// directory. Everything in its home (the login, the sessions, a person's own
// settings, instructions, extensions, hooks, MCP servers, plugins, skills and
// agents) is kept, and loads as it would for them: the run's settings and
// instructions are put back to the home's own ([Copilot.homeRestore]).
func (Copilot) artifactFiles(p Paths) []string {
	return []string{p.CopilotExtensionFresh(), p.CopilotGuardSettings()}
}

var _ homeKeeper = Copilot{}

// homeRestore puts the home's own settings.json and copilot-instructions.md
// back over the run's ([putBackHome]), each whatever came of the other
// ([everyStep]), run by the script as it exits and by the clear before every
// run whichever harness it runs.
func (Copilot) homeRestore(p Paths) string {
	return everyStep(putBackHome(p.CopilotSettings(), p.CopilotHeldSettings()),
		putBackHome(p.CopilotInstructions(), p.CopilotHeldInstructions()))
}

// artifactDirs is the run's own notes extension ([Paths.CopilotExtension]),
// which a later run with neither notes nor a cap must not find; the home's
// other extensions stay beside it.
func (Copilot) artifactDirs(p Paths) []string { return []string{p.CopilotExtension()} }

// notesOffWhy is what switches Copilot's reading of notes off once the guard
// passed the run ([copilotGuard]): a policy GitHub serves the account, which no
// file in the box shows, locking extensions or turning --yolo off, or an
// extension Copilot failed to load, which its own log names.
// allowManagedHooksOnly stops no extension on 1.0.114 (measured). A release
// before [copilotFloor] runs nothing.
func (Copilot) notesOffWhy() string {
	return "a policy GitHub serves the account that locks extensions (strictPluginOnlyCustomization) or " +
		"turns --yolo off (disableBypassPermissionsMode), or an extension Copilot failed to load, which its " +
		"own log names"
}

// copilotChargeBytes is the fewest bytes a Copilot session's brief is counted
// at a token: one, the most tokens any estimate can make of n bytes, since
// Copilot's own estimate was not read.
const copilotChargeBytes sandbox.Charge = 1

// sessionTraits is how a relaunch judges a Copilot session, each value the one
// that never under-counts its next request, since Copilot compacts at a share
// of a model window this build cannot read (its catalogue's, behind a sign-in):
//   - FirstRequestBox, with no point its box can say ([Runner.ReadBoxPoint]
//     marks it unread) and no count of the session: the relaunch always hands
//     the summarized telling, retelling the round's calls from its start;
//   - CallKeyingNone: no reading of the session thins a telling, so every
//     call after the save point is told;
//   - [copilotChargeBytes].
func (Copilot) sessionTraits() sandbox.SessionTraits {
	return sandbox.SessionTraits{FirstRequest: sandbox.FirstRequestBox, CallKeying: sandbox.CallKeyingNone,
		Charge: copilotChargeBytes}
}
