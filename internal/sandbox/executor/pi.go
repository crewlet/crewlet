package executor

import (
	"context"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

func init() { Register(Pi{}) }

// PiName is this runner's config name.
const PiName = "pi"

// Pi drives the Pi Coding Agent headless (`pi --mode json -p`, the brief on
// stdin), measured on 1.6.0. Its model is reached through the run ENVIRONMENT
// (the provider's own key variable) or the login its agent directory keeps
// ([Paths.PiAgentDir]). Everything the run adds to pi is ONE
// EXTENSION (pi_extension.mjs, [Pi.WriteConfig]): the seat's MCP servers, its
// standing instructions, its turn cap, notes and skill loads. Its session is one
// file at one path ([Paths.PiSessionFile], [piSessionGate], [piPack]). A run
// loads what a person's session loads: its working directory trusted
// (`--approve`), the agent directory's own config beside the run's. Pi has no
// approval layer, so the run is fully permissioned by construction.
type Pi struct{}

var (
	_ Harness           = Pi{}
	_ resumer           = Pi{}
	_ carrier           = Pi{}
	_ saver             = Pi{}
	_ inboxDelivery     = Pi{}
	_ recordConfirmer   = Pi{}
	_ noteReader        = Pi{}
	_ notesOffExplainer = Pi{}
	_ artifactDirHolder = Pi{}
	_ inPlaceGate       = Pi{}
	_ sessionJudge      = Pi{}
	_ skillHolder       = Pi{}
)

// NewPi returns the runner.
func NewPi() *Runner { return New(Pi{}) }

// Name is the harness's key in the runner registry.
func (Pi) Name() string { return PiName }

// MCPCallCeiling is none: the server's `timeout` carries the bridge's own
// bound ([piMCP]), and a 1040-second call under a 1050-second one was answered
// whole (measured on 1.6.0).
func (Pi) MCPCallCeiling() time.Duration { return 0 }

// resumesOnLaunchRoute: measured on 1.6.0, the one release supported
// ([launchRouted]): created on a
// models.json provider whose baseUrl was stub A, `--session` on its file with
// it at stub B, with another provider name, and with both configured and
// `--model` naming B's; each resumed request (1935 bytes, the first run's
// prompt in it) reached B.
func (Pi) resumesOnLaunchRoute() bool { return true }

// Resumes says Pi continues a session it reported: `--session` on a file the
// box holds continues it (measured on 1.6.0).
func (Pi) Resumes() bool { return true }

// NoteInstruction is the standing instruction about notes ([PiSteerInstruction]).
func (Pi) NoteInstruction() string { return PiSteerInstruction }

// HandsSkills reports that a Pi run takes skills of its own: a directory per
// skill under [Paths.PiSkills], named `crewlet:<name>` (pi warns of the colon
// and loads it, measured on 1.6.0), whose loads the extension reports.
func (Pi) HandsSkills() bool { return true }

// piSupported is the one Pi release this build drives, every behaviour here
// measured on it (the boundary hooks the notes are handed over at, the
// extension's MCP registration, the session format): an older release is
// refused at the run's start, a newer one runs with a new session every time,
// and a session is continued only between boxes on exactly this release
// ([Pi.Continues], [piSessionGate]).
const piSupported = "1.6.0"

// Session is the session a Pi run works in ([Harness.Session]): the one it
// continues, or a new one under a UUID minted for it (pi takes the id its
// file's header names), and, for a run that continues one, a second UUID for
// the session it starts instead where its box holds that one neither way.
func (Pi) Session(req sandbox.RunRequest) Session {
	if req.Resume != "" {
		return Session{ID: req.Resume, Continues: true, Fresh: uuid.NewString(),
			Replaces: req.Carried != nil && req.Carried.Replace}
	}
	return Session{ID: uuid.NewString()}
}

// Command is the run as ONE SCRIPT ([piScript]), so the wrapper's redirections
// (the brief on stdin, the stream, the error file) reach each step.
func (Pi) Command(req sandbox.RunRequest, session Session, paths Paths, configPath string) string {
	return "sh -c " + shellword.Quote(piScript(req, session, paths, configPath, sandbox.JobStopGrace))
}

// piExtensionMark is the type of the line a run's script prints before its
// harness starts ([piScript]): the run loads the extension, which says it ran
// ([piArmedType]). A stream that has the line and lacks the extension's word
// ran without the seat's tools, notes or cap ([piEvents.Result]).
const piExtensionMark = "crewlet.extension"

// piScript is a Pi run's script, giving its harness stop to go once asked to
// end: the end protocol (end.go); the release read, a pre-release or one older
// than [piSupported] refused; the extension's mark ([piExtensionMark]); the
// session named, its file's header written or its archive restored
// ([piSessionGate]), and recorded ([Paths.SessionRecord]); the harness run
// under the end protocol ([launchHarness]); and its session packed
// ([piCapture]), exiting with the harness's status.
//
// THE BRIEF IS NOT ON ITS COMMAND LINE: print mode with no message reads stdin,
// which the wrapper points at the brief's file. THE SYSTEM TEXT IS NOT EITHER:
// the extension adds it as a prompt section of its own, on every run.
func piScript(req sandbox.RunRequest, session Session, paths Paths, extension string, stop time.Duration) string {
	q := shellword.Quote
	parts := []string{"pi", "--mode", "json", "-p",
		// THE WORKING DIRECTORY TRUSTED, as a person answering pi's prompt
		// trusts it: a repository's .pi settings, MCP servers, extensions,
		// skills, prompts and system-prompt files load beside the agent
		// directory's. Pi only creates the directory a sessionDir setting
		// names: the run's session is its own file.
		"--approve",
		// PI'S MCP SUPPORT NAMED, which the run's servers ride on: a path the
		// launch names is loaded whatever a settings file enables (1.6.0).
		"-e", "builtin:mcp"}
	// VERBATIM, as the pi entry names it in pi's own terms (provider/id).
	if req.Model != "" {
		parts = append(parts, "--model", q(req.Model))
	}
	if req.Effort != "" {
		parts = append(parts, "--thinking", q(req.Effort))
	}
	if extension != "" {
		parts = append(parts, "-e", q(extension))
	}
	if len(req.Skills) > 0 {
		parts = append(parts, "--skill", q(paths.PiSkills()))
	}
	parts = append(parts, `"$@"`)

	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	b.WriteString(endPrologue(paths, stop))
	b.WriteString(endHarness(""))
	b.WriteString(piReleaseRead)
	b.WriteString(piReleaseParse())
	b.WriteString(piReleaseRefusal())
	if extension != "" {
		line(`printf '%%s\n' %s`, q(piMarkLine()))
	}
	b.WriteString(piHeader)
	line(`set --`)
	b.WriteString(piSessionGate(session, paths, req.FreshWiring != nil && extension != ""))
	line(`printf 'id %%s\ncontinued %%s\nwhy %%s\nrelease %%s\n' "$s" "$k" "$w" "$v" > %s`,
		q(paths.SessionRecord()))
	b.WriteString(launchHarness(strings.Join(parts, " "), paths))
	b.WriteString(piCapture(paths))
	line(`exit "$code"`)
	return b.String()
}

// piMarkLine is the [piExtensionMark] line.
func piMarkLine() string { return fmt.Sprintf(`{"type":%q}`, piExtensionMark) }

// piReleaseRead reads the box's `pi --version` once, as v.
const piReleaseRead = "v=$(pi --version < /dev/null 2> /dev/null)\n"

// piReleaseParse reads v as [releaseOf] does ([bareReleaseShell]): n is the
// bare release, "" for a pre-release or none; its numbers are a, b and c; and
// `at`, [claudeRelease]'s, is false for a release that does not read. Built
// when a script is, never at init.
func piReleaseParse() string {
	return bareReleaseShell("n") + `a=${n%%.*}; r=${n#*.}; b=${r%%.*}; c=${r##*.}
at() { [ -n "$n" ] && { [ "$a" -gt "$1" ] || { [ "$a" -eq "$1" ] && { [ "$b" -gt "$2" ] || { [ "$b" -eq "$2" ] && [ "$c" -ge "$3" ]; }; }; }; }; }
`
}

// piReleaseRefusal ends the run before anything starts on a box whose Pi names
// no release ([piReleaseParse]: a pre-release is never the supported one) or
// one older than [piSupported], saying what it printed and which it needs. A
// newer release runs, with a new session every time ([piSessionGate]).
func piReleaseRefusal() string {
	return fmt.Sprintf(`[ -n "$n" ] || { echo "crewlet: this box's pi --version printed '$v', which names no release the pi runner reads (a bare MAJOR.MINOR.PATCH, never a pre-release); it supports Pi %[1]s: install that release in the box image" >&2; exit 1; }
at %[2]s || { echo "crewlet: this box runs Pi $n, older than %[1]s, the release this build supports: install %[1]s in the box image" >&2; exit 1; }
`, piSupported, floorArgs(piSupported))
}

// piHeader defines the script's `piheader ID`, which prints the header line a
// Pi session file opens with (session format version 3: type, version, id,
// timestamp, cwd), naming the run's working directory as pi names it: a
// session is continued only in the directory its header names, and `--session`
// on a file holding the header alone starts the session under that id
// (measured on 1.6.0). The directory as a JSON string, its backslashes and
// quotes escaped; one holding a control character, which no header names
// plainly, refuses the run.
const piHeader = `pd=$PWD
case "$pd" in *[[:cntrl:]]*) echo 'crewlet: the run'"'"'s working directory holds a control character, which a Pi session cannot name, so the run does not start' >&2; exit 1;; esac
pd=$(printf '%s' "$pd" | sed 's/\\/\\\\/g; s/"/\\"/g')
piheader() { printf '{"type":"session","version":3,"id":"%s","timestamp":"%s","cwd":"%s"}\n' "$1" "$(date -u +%Y-%m-%dT%H:%M:%S.000Z)" "$pd"; }
`

// piConversation is how a session file's first message line opens, which the
// gate looks for: a file holding no message (a header alone, from a run cut off
// before its brief was written) holds no conversation to continue.
const piConversation = `{"type":"message"`

// piSessionGate is the part of a run's script that names the session its
// harness runs in (s), whether it continues the one asked (k) and, where it
// could not, why (w), adding `--session` on its file (f). As
// [claudeSessionGate]: the box's copy is continued unless a carried archive
// holds what it does not; that archive is restored once it holds the session
// with a conversation in it, its digest recorded; else the minted session
// starts and says why. A new session's file is its header alone.
//
// A RESTORED FILE NAMES THIS BOX'S WORKING DIRECTORY in its header, written
// again: pi continues a session only where its header says it was, and a
// direct box's directory differs from any other's. NO LINE IS CUT: pi skips a
// torn line and ends the file with a break before it appends (1.6.0).
func piSessionGate(session Session, paths Paths, rewired bool) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := shellword.Quote
	start := func(why string) {
		line(`%sif ! { mkdir -p %s && piheader "$s" > "$f"; }; then echo 'crewlet: the Pi session'"'"'s file could not be written, so the run does not start' >&2; exit 1; fi`,
			why, q(paths.PiSessions()))
	}
	// THE RELEASE THAT WROTE A SESSION, beside its file, which a later run in
	// this box reads before it continues its copy.
	release := func() {
		line(`printf '%%s\n' "$n" > "$f.release" 2> /dev/null || :`)
	}
	if !session.Continues {
		line(`s=%s k=no w= f=%s`, q(session.ID), q(paths.PiSessionFile(session.ID)))
		start("")
		line(`set -- "$@" --session "$f"`)
		release()
		return b.String()
	}
	supported := " and a session is continued only between boxes on Pi " + piSupported +
		", the release this build supports"
	staged := paths.SessionStaging()
	carried := q(paths.SessionCarried())
	from := q(staged + "/in/.pi/sessions/" + session.ID + ".jsonl")
	line(`pirestore() {`)
	line(`  rm -rf %s && mkdir -p %s %s || return 1`, q(staged), q(staged+"/in"), q(paths.PiSessions()))
	line(`  tar -xzf %s -C %s < /dev/null > /dev/null 2>&1 || return 1`, carried, q(staged+"/in"))
	line(`  [ ! -L %s ] && [ ! -L %s ] && [ -f %s ] && [ ! -L %s ] || return 1`,
		q(staged+"/in/.pi"), q(staged+"/in/.pi/sessions"), from, from)
	line(`  grep -qF %s %s && head -n 1 %s | grep -qF %s || return 1`, q(piConversation), from, from,
		q(`"id":"`+session.ID+`"`))
	line(`  if ! head -n 1 %s | grep -qF "\"cwd\":\"$pd\"}"; then { piheader "$s" && tail -n +2 %s; } > %s.next && mv -f %s.next %s || return 1; fi`,
		from, from, from, from, from)
	// THE BOX'S OWN COPY GOES only now, once the archive is known to hold a
	// conversation.
	line(`  mv -f %s "$f"`, from)
	line(`}`)
	line(`s=%s k=yes w= h= d= x= f=%s`, q(session.ID), q(paths.PiSessionFile(session.ID)))
	// EXACTLY THE SUPPORTED RELEASE, the box's and the writer's of the copy
	// it holds; a carried archive's writer was judged before the launch
	// ([Pi.Continues]).
	line(`[ "$n" = %s ] || w="this box runs Pi ${n:-$v},"%s`, piSupported, q(supported))
	line(`if [ -z "$w" ] && grep -qF %s "$f" 2> /dev/null; then h=1; x=$(cat "$f.release" 2> /dev/null) || x=; [ "$x" = %s ] || h=; fi`,
		q(piConversation), piSupported)
	line(`if [ -s %s ]; then d=$(cksum < %s 2> /dev/null) || d=; fi`, carried, carried)
	if session.Replaces {
		line(`if [ -n "$h" ] && [ -s %s ]; then h=; fi`, carried)
	} else {
		line(`if [ -n "$h" ] && [ -n "$d" ] && ! grep -qxF "$s $d" %s 2> /dev/null; then h=; fi`,
			q(paths.SessionDigests()))
	}
	line(`if [ -z "$w" ] && [ -z "$h" ]; then`)
	line(`  if [ ! -s %s ] && [ -n "$x" ]; then w="its copy in this box was written by Pi $x,"%s`, carried, q(supported))
	line(`  elif [ ! -s %s ]; then w='it is not in this box, and no archive of it was carried here'`, carried)
	line(`  elif ! command -v tar > /dev/null 2>&1 || ! command -v gzip > /dev/null 2>&1; then w='this box has no tar and gzip to restore its archive with'`)
	line(`  elif ! pirestore; then w='its archive could not be restored'`)
	// Continued only once its digest is recorded and read back, for
	// [claudeSessionGate]'s reason.
	line(`  elif ! { %s; }; then w=%s`, recordDigest(paths, "s"), q(restoreDigestUnrecorded))
	line(`  fi`)
	line(`  rm -rf %s`, q(staged))
	line(`fi`)
	line(`rm -f %s`, carried)
	line(`if [ -z "$w" ]; then`)
	line(`  set -- "$@" --session "$f"`)
	if rewired {
		line(`  rm -f %s`, q(paths.PiExtensionFresh()))
	}
	line(`else`)
	line(`  s=%s k=no f=%s`, q(session.Fresh), q(paths.PiSessionFile(session.Fresh)))
	line(`  printf '%%s%%s\n' %s "$w" >&2`, q("crewlet: this run was asked to continue the Pi session "+
		session.ID+", and starts a new session, "+session.Fresh+", instead: "))
	start("  ")
	line(`  set -- "$@" --session "$f"`)
	line(`  [ ! -s %s ] || exec < %s`, q(paths.FreshBrief()), q(paths.FreshBrief()))
	if rewired {
		// THE NEW SESSION WIRED TO ITS OWN TOKEN, the bridge's, the notes
		// route's and the skill route's alike, or not started:
		// [claudeSessionGate]'s reason.
		line(`  mv -f %s %s || { echo 'crewlet: the new session could not be wired to the tools of its seat, so the run does not start' >&2; exit 1; }`,
			q(paths.PiExtensionFresh()), q(paths.PiExtension()))
	}
	line(`fi`)
	release()
	return b.String()
}

// piArchiveFormat is the layout a Pi run packs its session in ([piPack]) and
// the one it restores ([piSessionGate]): one gzipped tar of one member named
// relative to the run's home, `.pi/sessions/<session>.jsonl`, the whole
// session, which a resume takes up on its own (measured on 1.6.0). A change is
// a new format.
const piArchiveFormat = 1

// ArchiveFormat is the layout a Pi run packs its session in ([carrier]).
func (Pi) ArchiveFormat() int { return piArchiveFormat }

// VersionCommand prints a box's Pi release ([carrier]).
func (Pi) VersionCommand() string { return "pi --version" }

// layout is a Pi archive's: a gzipped tar of the session's file.
func (Pi) layout() archiveLayout { return archiveTar }

// charges is false: its meter counts nothing ([carrier.charges]).
func (Pi) charges(string, string) bool { return false }

// Continues reports whether a session written by Pi release writer can be
// continued in a box whose Pi is release box ([carrier]): only both exactly
// [piSupported]. A release that does not read is no release, the box's as
// much as the writer's.
func (p Pi) Continues(box, writer string) (bool, string) {
	if ok, why := p.ContinuesWriter(writer); !ok {
		return false, why
	}
	return p.continuesInPlace(box)
}

// ContinuesWriter is [Pi.Continues] judged of the writer alone ([carrier]):
// only a session [piSupported] wrote; one any other release wrote, or one
// whose writer named no release this build reads, is refused, saying so.
func (Pi) ContinuesWriter(writer string) (bool, string) {
	if writer == piSupported {
		return true, ""
	}
	wrote := fmt.Sprintf("Pi %.64s", writer)
	if writer == "" {
		wrote = "a Pi that named no release this build reads"
	}
	return false, fmt.Sprintf("the session was written by %s, and a session is continued only between "+
		"boxes on Pi %s, the release this build supports", wrote, piSupported)
}

// continuesInPlace reports whether a box whose Pi is release box continues a
// session at all, the copy it holds as much as one carried to it
// ([inPlaceGate]): only on [piSupported], as the run's own gate holds it
// ([piSessionGate]), so the engine asks before a relaunch hands the brief
// that continues the box's copy. A box release that does not read
// ([releaseReads]: none, a pre-release, a snapshot) is said to be none.
func (Pi) continuesInPlace(box string) (bool, string) {
	if box == piSupported {
		return true, ""
	}
	runs := fmt.Sprintf("this box runs Pi %.64s", box)
	if !releaseReads(box) {
		runs = "this box's Pi named no release this build reads"
	}
	return false, fmt.Sprintf("%s, and a session is continued only on Pi %s, the release this build supports",
		runs, piSupported)
}

// meter is the take's meter of a Pi archive ([carrier.meter]): no figure.
// This build reads no count or estimate of a Pi session's next request off
// its archive; a Pi run's window is read off its own records at collection.
// The archive is left to the caller, which drains it ([startMeter]).
func (Pi) meter(context.Context, io.Reader, string, string, *passBudget) (meterReading, error) {
	return meterReading{}, nil
}

// piCapture is the part of a run's script that packs its session once its
// harness has exited ([piPack]).
func piCapture(paths Paths) string {
	return piPack(paths, packAt{staged: paths.SessionStaging(), dest: paths.SessionCapture()})
}

// piPack packs the session s names into at.staged and moves it over at.dest,
// its digest kept ([packDigest]): its file alone. A pack beside a working
// harness is held to [claudeHeldTar]'s rule; the file is append-only.
func piPack(paths Paths, at packAt) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := shellword.Quote
	home := paths.Home
	line(`if [ -s %s/.pi/sessions/"$s".jsonl ]; then`, q(home))
	line(`  if command -v tar > /dev/null 2>&1 && command -v gzip > /dev/null 2>&1; then`)
	line(`    set -- .pi/sessions/"$s".jsonl`)
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
// exits, into staged and over dest: [packNothing] where the box holds no file
// of it, [packFailed] where no archive was made.
func (Pi) Pack(paths Paths, session, staged, dest string, working bool) string {
	return piOnDemand(paths, session, packAt{staged: staged, dest: dest, working: working})
}

// PackSave packs a save of rec's session beside a harness that may still be
// working ([saver]): the file is append-only, so the rule of a pack beside a
// working harness holds it ([claudeHeldTar]). The record refuses no save.
func (Pi) PackSave(paths Paths, rec sessionRecord, staged, dest string) (string, error) {
	return piOnDemand(paths, rec.id, packAt{staged: staged, dest: dest, working: true}), nil
}

// piOnDemand is a pack the engine makes of session outside the run's own
// script, at at.
func piOnDemand(paths Paths, session string, at packAt) string {
	return "s=" + shellword.Quote(session) + " packed=\n" +
		fmt.Sprintf("[ -s %s ] || exit %d\n", shellword.Quote(paths.PiSessionFile(session)), packNothing) +
		piPack(paths, at) +
		fmt.Sprintf("[ -n \"$packed\" ] || exit %d\n", packFailed)
}

// Output is where a job writes under `--mode json`: its stream is its result,
// decoded whole or followed ([piEvents]). The brief on stdin.
func (Pi) Output(paths Paths) Output {
	return Output{Stdout: paths.Result(), Events: true, BriefOnStdin: true}
}

// routing answers [router] with nothing: a Pi run's calls go where its
// models.json says, and any credential-named variable signs it in
// (env_sign_in). Measured on 1.6.0 with no network: ANTHROPIC_BASE_URL,
// OPENAI_BASE_URL and OPENROUTER_BASE_URL at a stand-in, its anthropic, openai
// and openrouter models still dialled api.anthropic.com, api.openai.com and
// openrouter.ai.
func (Pi) routing() []routeVar { return nil }

// Pins is the run's state directory and posture, over whatever the seat's env,
// the image or a profile set: PI_CODING_AGENT_DIR ([Paths.PiAgentDir]); and no
// network of pi's own at start (no catalogue refresh, version check or
// telemetry).
func (Pi) Pins(_ context.Context, _ sandbox.RunRequest, paths Paths) []Pinned {
	return []Pinned{
		{Name: "PI_CODING_AGENT_DIR", Value: paths.PiAgentDir()},
		{Name: "PI_OFFLINE", Value: "1"},
		{Name: "PI_SKIP_VERSION_CHECK", Value: "1"},
		{Name: "PI_TELEMETRY", Value: "0"},
	}
}

// Events is a new decoder for one stream, read whole or followed ([Decoder]).
func (Pi) Events() Decoder { return &piEvents{} }

// Parse decodes a whole stream already in hand: the result of a run whose
// result IS its stream.
func (Pi) Parse(stdout []byte) sandbox.Result { return decodeAll(&piEvents{}, stdout) }

// WriteConfig writes what a Pi run adds to pi, beside whatever the agent
// directory and the working directory hold: the run's skills
// ([Paths.PiSkills]) and its extension ([Paths.PiExtension]), returned as the
// file `-e` names, with a fresh session's copy beside it
// ([sandbox.RunRequest.FreshWiring]). What an earlier run wrote the clear
// removes first ([Pi.artifactDirs]).
func (p Pi) WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, _ Session,
	paths Paths) (string, error) {
	skills := make(map[string]string, len(req.Skills))
	for _, s := range req.Skills {
		file, err := skillFile(s.Listed(), s.Description, s.Body)
		if err != nil {
			return "", err
		}
		at := paths.PiSkills() + "/" + s.Listed() + "/SKILL.md"
		if err = box.WriteFile(ctx, at, file); err != nil {
			return "", fmt.Errorf("executor: writing the run's skill %q: %w", s.Listed(), err)
		}
		skills[at] = s.Listed()
	}
	if err := p.writeExtension(ctx, box, paths, paths.PiExtension(), req, req.MCPServers, req.Steer,
		req.SkillLoads, skills); err != nil {
		return "", err
	}
	if f := req.FreshWiring; f != nil {
		if err := p.writeExtension(ctx, box, paths, paths.PiExtensionFresh(), req, f.MCPServers, f.Steer,
			f.SkillLoads, skills); err != nil {
			return "", err
		}
	}
	return paths.PiExtension(), nil
}

// piMCPFiles is the `mcp.json` files pi 1.6.0 reads servers from, whose
// entry wins over a server an extension registers under its name: the agent
// directory's, and the trusted working directory's `.pi/mcp.json`, which the
// extension resolves against its own.
func piMCPFiles(paths Paths) []string {
	return []string{paths.PiAgentDir() + "/mcp.json", ".pi/mcp.json"}
}

// piSettingsFiles is the settings files pi 1.6.0 merges, the trusted working
// directory's `.pi/settings.json` over the agent directory's: the extension
// names the one whose `codemode.mode` keeps the seat's tools from the model,
// or whose `defaultTools` keeps its skills from it.
func piSettingsFiles(paths Paths) []string {
	return []string{paths.PiAgentDir() + "/settings.json", ".pi/settings.json"}
}

// writeExtension writes the extension, wired to servers, steer's notes route
// and loads' skill route, with req's cap, to file: the seat's system text told
// of notes where the run is wired to them.
func (p Pi) writeExtension(ctx context.Context, box sandbox.Sandbox, paths Paths, file string,
	req sandbox.RunRequest, servers map[string]sandbox.MCPServer, steer sandbox.SteerWiring, loads sandbox.SkillWiring,
	skills map[string]string) error {

	system := req.System
	if steer.Set() {
		system = withNoteInstruction(system, p.NoteInstruction())
	}
	settings := piExtensionSettings{System: system, Cap: max(req.Limits.MaxTurns, 0),
		Required: piRequired(servers), MCPFiles: piMCPFiles(paths), SettingsFiles: piSettingsFiles(paths),
		Listed: len(skills) > 0}
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		settings.Servers = append(settings.Servers, piServer{Name: name, Config: piMCP(servers[name])})
	}
	if steer.Set() {
		settings.Notes = steer.URL
	}
	if loads.Set() && len(skills) > 0 {
		settings.Skills, settings.SkillURL = skills, loads.URL
	}
	source, err := piExtension(settings)
	if err != nil {
		return err
	}
	if err = box.WriteFile(ctx, file, []byte(source)); err != nil {
		return fmt.Errorf("executor: writing the Pi run's extension: %w", err)
	}
	return nil
}

// piRequired is the servers a run fails without, the extension checking each
// offers a tool at its first turn: those the engine marks always loaded, the
// tool bridge's own server, which serves the seat's own tools. Another server
// may legitimately offer none.
func piRequired(servers map[string]sandbox.MCPServer) []string {
	out := []string{}
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		if servers[name].AlwaysLoaded {
			out = append(out, name)
		}
	}
	return out
}

// piMCP is one server as `pi.registerMcpServer` takes it (1.6.0): an HTTP
// server's url and headers (streamable HTTP), a command's command, args and
// env; every one declared to the model by name (`exposure: "direct"`; pi's
// default hides a server's tools behind one tool of its own); `timeout` in
// seconds where the engine sets a wait (a call is cancelled at it, measured).
func piMCP(s sandbox.MCPServer) map[string]any {
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
	out["exposure"] = "direct"
	if ms := waitMillis(s.CallTimeout); ms > 0 {
		out["timeout"] = int64(math.Ceil(float64(ms) / 1000))
	}
	return out
}

// artifactDirs is the directories a Pi run leaves in its box of its own,
// removed whole: its run's own (the extension and skills), and the agent
// directory's `bin`, which pi puts first on its shell's PATH (a planted `rg`
// ran for the model's `rg`, measured on 1.6.0). Pi runs without `bin` and,
// offline, never fills it again. The rest of the agent directory is the
// home's own config, which loads as a person's session does.
func (Pi) artifactDirs(p Paths) []string { return []string{p.PiRun(), p.PiAgentDir() + "/bin"} }

// notesOffWhy is what switches a Pi run's reading of notes off: the extension
// failing to load, which pi says on the run's error stream ("Extension error")
// and which fails the run ([piEvents.Result]). A release before [piSupported]
// runs nothing.
func (Pi) notesOffWhy() string {
	return "the run's extension failing to load, which pi names on the run's error stream"
}

// piChargeBytes is the fewest bytes a Pi session's brief is counted at a token:
// one, the most tokens any estimate can make of n bytes, since pi's own
// estimate is not read.
const piChargeBytes sandbox.Charge = 1

// sessionTraits is how a relaunch judges a Pi session, each value the one that
// never under-counts its next request, since pi compacts at a share of a model
// window it reads from its own catalogue, which this build does not:
//   - FirstRequestBox, with no point its box can say ([Runner.ReadBoxPoint]
//     marks it unread): the relaunch hands the summarized telling;
//   - CallKeyingNone: no reading of the session thins a telling;
//   - [piChargeBytes].
func (Pi) sessionTraits() sandbox.SessionTraits {
	return sandbox.SessionTraits{FirstRequest: sandbox.FirstRequestBox, CallKeying: sandbox.CallKeyingNone,
		Charge: piChargeBytes}
}
