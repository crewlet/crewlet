package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

func init() { Register(ClaudeCode{}) }

// ClaudeCodeName is this runner's config name.
const ClaudeCodeName = "claude-code"

// ClaudeCode drives Claude Code headless.
//
// It reaches its model through the run ENVIRONMENT, not a config file, so
// WriteConfig renders only MCP servers, settings and hooks: a credential in a
// file in the box would be readable by the agent.
//
// Its retention sweep is held off by every run's settings ([claudeSettings]),
// a box that switches its hooks off refuses a run that starts one
// ([claudeHooksOff], [claudeSwitchesOff]), its session is pinned, named, packed and restored
// ([claudeSessionLayout], [claudeSessionGate], [claudeCapture]; packs beside a
// working harness by [claudeHeldTar]), and its end on request is end.go's
// protocol: a SIGUSR1 trap, SIGTERM then SIGKILL past [sandbox.JobStopGrace].
type ClaudeCode struct{}

var (
	_ Harness = ClaudeCode{}
	_ saver   = ClaudeCode{}
)

// NewClaudeCode returns the runner.
func NewClaudeCode() *Runner { return New(ClaudeCode{}) }

// Name is the harness's key in the runner registry.
func (ClaudeCode) Name() string { return ClaudeCodeName }

// MCPCallCeiling is none: the server's `timeout`, the bridge's own bound
// ([claudeCodeMCP]), is also 2.1.310's budget for a response's first byte, and
// the bridge's progress answers its other clocks (measured: a 1040-second call
// answered whole under a 1050-second bound).
func (ClaudeCode) MCPCallCeiling() time.Duration { return 0 }

// resumesOnLaunchRoute: measured ([launchRouted]) on 2.1.310, the carry floor
// ([claudeCarryFloor]) and the release the box image recipe installs
// (code-sandbox.md), and on 2.1.330, the newest: created with
// ANTHROPIC_BASE_URL at stub A, `-p --resume` with it at stub B; the resumed
// request (84527 and 87709 bytes, the first run's prompt in it) reached B.
func (ClaudeCode) resumesOnLaunchRoute() bool { return true }

// Resumes says Claude Code continues a session it reported (`--resume`).
func (ClaudeCode) Resumes() bool { return true }

// NoteInstruction is the standing instruction about notes: a run takes a
// person's notes between its steps through two hooks ([Paths.ClaudeNotesSettings],
// PostToolBatch and Stop), each a call of the bridge's unlisted `steer_take` tool
// over the run's MCP connection. Measured on 2.1.263 and 2.1.310:
//   - a note after a batch reaches the model after that batch's tool results and
//     stays; WHERE depends on the model ([ClaudeCodeSteerInstruction]);
//   - a note at Stop continues the turn, and the result is the answer after it;
//   - the hook's `timeout` is in seconds, and a late answer is discarded
//     ([ClaudeHookTimeout]);
//   - an error answer makes the harness re-initialise the MCP server, so the
//     bridge answers every take with text; `systemMessage` reaches only the stream.
func (ClaudeCode) NoteInstruction() string { return ClaudeCodeSteerInstruction }

// Session is the session a Claude Code run works in ([Harness.Session]): the one
// it continues, or a new one under a UUID minted for it — `--session-id` takes
// a UUID — and, for a run that continues one, a second UUID for the session it
// starts instead where its box holds that one neither way.
func (ClaudeCode) Session(req sandbox.RunRequest) Session {
	if req.Resume != "" {
		return Session{ID: req.Resume, Continues: true, Fresh: uuid.NewString(),
			Replaces: req.Carried != nil && req.Carried.Replace}
	}
	return Session{ID: uuid.NewString()}
}

// Command builds the headless `claude -p` invocation as ONE SCRIPT, so the
// wrapper's redirections (brief on stdin, stream, error file) reach every step:
// the end protocol first (end.go); `claude --version` read once ([claudeRelease])
// to decide what the harness is started with; the session named and recorded
// ([Paths.SessionRecord]); the harness run in the background and waited for, so a
// request can end it ([launchHarness]); then its session packed ([claudeCapture]),
// exiting with the harness's status. Pure and deterministic, so a test can pin
// the flags; [claudeScript] explains them.
func (ClaudeCode) Command(req sandbox.RunRequest, session Session, paths Paths, configPath string) string {
	return "sh -c " + shellword.Quote(claudeScript(req, session, paths, configPath, sandbox.JobStopGrace))
}

// claudeScript is a Claude Code run's script ([ClaudeCode.Command]), giving its
// harness stop to go once asked to end: [sandbox.JobStopGrace], or a case's
// shorter one.
//
// THE BRIEF IS NOT ON ITS COMMAND LINE: `-p` with no prompt reads stdin, which
// the wrapper points at the brief's file ([ClaudeCode.Output]). A brief grows
// with the notes, one argument is capped at 128 KiB (MAX_ARG_STRLEN), and a
// command line is readable by every account on a `direct` box's host
// ([Paths.System]).
func claudeScript(req sandbox.RunRequest, session Session, paths Paths, configPath string,
	stop time.Duration) string {
	parts := []string{
		"claude", "-p",
		// stream-json prints every message as it happens, where `json` prints one
		// object at the end and nothing before it, leaving a failed run no
		// transcript; print mode refuses it without --verbose.
		"--output-format", "stream-json", "--verbose",
		// Permissions are bypassed in every cell, deliberately: a headless run has
		// nobody to answer a prompt and no per-tool allowlist. What the agent may
		// reach is its box's cell's to bound (`container` keeps it off the engine
		// host, `direct` does not) beside the run env's credentials.
		"--permission-mode", "bypassPermissions",
	}
	// THE ENTRY'S WEB TOOLS OFF where it denies them, as its runs on the engine
	// host have none: on 2.1.330, under bypassPermissions, both are refused as
	// "disabled for this session, in subagents as well as here", and a
	// repository's settings allowing them, or an agent of its own listing them,
	// do not bring them back. Before "$@", whose first value is a flag, so the
	// list takes these two alone.
	if req.WebDenied {
		parts = append(parts, "--disallowedTools", "WebFetch", "WebSearch")
	}
	parts = append(parts,
		// WHAT THE SCRIPT DECIDED BY THE HARNESS'S RELEASE: the run's settings, and
		// the session, NAMED ON EVERY RUN ([claudeSessionGate]): a new one under the
		// minted id (`--session-id`), a continued one with `--resume` and never
		// both, which the harness refuses (2.1.316). A resumed run RENDERS ITS
		// SYSTEM PROMPT AFRESH ([claudeSnapshotFloor]): by default the conversation
		// reuses the prompt its first request recorded, so a later launch's
		// standing instructions would never be read.
		`"$@"`,
	)
	// THE SYSTEM TEXT IS APPENDED, NOT SUBSTITUTED (never argv, [Paths.System]):
	// Claude Code's own prompt stays whole, being how to drive its tools. Passed
	// only for a run that has system text.
	if req.System != "" && !req.Steer.Set() {
		parts = append(parts, "--append-system-prompt-file", shellword.Quote(paths.System()))
	}
	// VERBATIM, because the only entry that reaches this runner is a
	// claude-code one, which already names the model in Claude Code's own
	// terms — its alias or id exactly as its runs on the engine host pass
	// it. A launch passes over every other harness's entry, so none reaches here.
	if req.Model != "" {
		parts = append(parts, "--model", shellword.Quote(req.Model))
	}
	if req.Limits.MaxTurns > 0 {
		parts = append(parts, "--max-turns", strconv.Itoa(req.Limits.MaxTurns))
	}
	if configPath != "" {
		parts = append(parts, "--mcp-config", shellword.Quote(configPath))
	}
	// NO --strict-mcp-config: beside the engine's servers the run starts what
	// Claude Code starts for a person, the box's, the repository's .mcp.json, a
	// plugin's and the signed-in account's connectors.

	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	// THE END PROTOCOL BEFORE ANYTHING ELSE, its pid published only once its
	// request is trapped (end.go): the release gate below runs a harness, which
	// a request may come during.
	b.WriteString(endPrologue(paths, stop))
	b.WriteString(endHarness(""))
	b.WriteString(claudeRelease)
	// THE TRANSCRIPT KEPT NEAR ITS SIZE, on a release whose trim loses nothing
	// ([claudeLocalGCFloor]) — and on an older one never, whatever set it.
	line(`if at %s; then export CLAUDE_CODE_TRANSCRIPT_LOCAL_GC=1; else unset CLAUDE_CODE_TRANSCRIPT_LOCAL_GC; fi`,
		floorArgs(claudeLocalGCFloor))
	// THE RUN'S OWN SETTINGS, ONE FILE, as cs: on every run the settings that
	// hold the harness's own retention sweep off ([claudeSettings]), unless the
	// notes' gate below starts their hooks, whose file holds them besides. hk
	// is set by each gate that starts a hook of the engine's.
	line(`set --; cs=%s; hk=`, shellword.Quote(paths.ClaudeSettings()))
	// THE RUN'S OWN SKILLS, and the hook that reports each load, in the
	// plugin [ClaudeCode.WriteConfig] wrote — on a release that runs the hook
	// ([claudeSkillsGate]).
	if len(req.Skills) > 0 {
		b.WriteString(claudeSkillsGate(paths.ClaudeSkillsPlugin(), anyRequired(req.Skills)))
	}
	// THE RUN'S OWN HOOKS, which hand it a person's notes between its steps —
	// only where its launch wired it to them ([ClaudeCode.WriteConfig]) and
	// its release reads what they answer, in the settings that hold them —
	// AND ITS SYSTEM TEXT, told of notes only where they are read: the gate
	// decides both; an older release refuses the run.
	if req.Steer.Set() {
		b.WriteString(claudeNotesGate(paths.ClaudeNotesSettings(), paths.ClaudeNotesSystem()))
	}
	// NO HOOK OF THE ENGINE'S STARTED SWITCHED OFF: refused, naming the file
	// and key, where the box's settings would run none ([claudeHooksOffGate]).
	if len(req.Skills) > 0 || req.Steer.Set() {
		b.WriteString(claudeHooksOffGate(paths))
	}
	// THE COMPACTION POINT AND A RESUME'S LOAD PINNED IN THE SETTINGS AS WELL,
	// on a release whose settings take the pin ([claudeSettingsPinFloor]): the
	// pinned copy of whichever file the gates above chose ([claudePinGate]), so
	// a repository's own settings can neither move the point the wrapper pinned
	// nor switch on again what the script unsets for a resume. After every gate
	// that chooses cs.
	b.WriteString(claudePinGate(paths, req.Steer.Set()))
	b.WriteString(claudeNoProxyMerge(paths, directHosts(req)))
	b.WriteString(claudeManagedNoProxy(paths, directHosts(req), claudeManagedSettings))
	line(`set -- "$@" --settings "$cs"`)
	// THE ENTRY'S EFFORT, as its runs on the engine host pass it, on a release
	// that takes the flag ([claudeEffortFloor]).
	if req.Effort != "" {
		line(`if at %s; then set -- "$@" --effort %s; fi`, floorArgs(claudeEffortFloor), shellword.Quote(req.Effort))
	}
	b.WriteString(claudeSessionGate(session, paths, req.FreshWiring != nil && configPath != ""))
	// THE RECORD, before the harness starts, so a run killed before it reported
	// anything still says which session it took up, and why it began again
	// where it did. The release last, as the harness printed it
	// ([parseSessionRecord]).
	line(`printf 'id %%s\ncontinued %%s\nwhy %%s\nrelease %%s\n' "$s" "$k" "$w" "$v" > %s`,
		shellword.Quote(paths.SessionRecord()))
	b.WriteString(claudeStateLink(paths))
	// THE RUN'S OWN SERVER CONNECTED AS IT STARTS, or the run ended saying
	// what switched it off ([claudeGuard]).
	watch := ""
	if guarded := crewletServers(req.MCPServers); len(guarded) > 0 {
		b.WriteString(claudeGuard(paths, guarded, req.MCPServers))
		watch = "claudeguard"
	}
	b.WriteString(launchWatched(strings.Join(parts, " "), watch, paths))
	b.WriteString(claudeCapture(paths))
	line(`exit "$code"`)
	return b.String()
}

// claudeRelease opens every Claude Code run's script: it reads the box's
// `claude --version` once, as v, its numbers as n, and defines `at MAJOR MINOR
// PATCH`, which answers whether that release is at or after the one given, as
// every gate asks, of the harness the box's PATH finds, which is the one that runs.
//
// A VERSION IT CANNOT READ IS TAKEN FOR A CURRENT RELEASE: every release prints
// `MAJOR.MINOR.PATCH (Claude Code)`, so anything else is newer than this script,
// and a missing harness fails at the run itself.
//
// POSIX expansion, case and test alone (dash, bash, busybox ash): no awk, no
// sort -V. The version is cut at its first non-digit, non-dot character and read
// only with three digit-led parts, else as current ([releaseAtLeast] does too).
const claudeRelease = `v=$(claude --version < /dev/null 2> /dev/null); n=${v%%[!0-9.]*}
a=${n%%.*}; r=${n#*.}; b=${r%%.*}; c=${r#*.}; c=${c%%.*}
case "$n" in [0-9]*.[0-9]*.[0-9]*) ;; *) a= ;; esac
at() { [ -z "$a" ] || [ "$a" -gt "$1" ] || { [ "$a" -eq "$1" ] && { [ "$b" -gt "$2" ] || { [ "$b" -eq "$2" ] && [ "$c" -ge "$3" ]; }; }; }; }
`

// claudeStateLink is the part of a run's script that has the harness keep its
// state in the home's own `.claude.json`, as a person's session does: the
// pinned CLAUDE_CONFIG_DIR moves that file into the directory
// ([claudeSessionLayout]), which then hides the home's user and local MCP
// servers, so the directory's copy is made a link to it (2.1.330 reads and
// writes through the link, and creates the file where it is missing). A file
// the directory holds of its own is left: a box that sets CLAUDE_CONFIG_DIR
// there itself reads that one.
func claudeStateLink(paths Paths) string {
	in := shellword.Quote(paths.ClaudeConfigDir() + "/.claude.json")
	return "if [ ! -e " + in + " ] && [ ! -L " + in + " ]; then mkdir -p " +
		shellword.Quote(paths.ClaudeConfigDir()) + " && ln -s " + shellword.Quote(paths.ClaudeState()) + " " + in +
		` || echo "crewlet: the run could not link Claude Code's .claude.json to the home's own, ` +
		`so it does not read that" >&2; fi` + "\n"
}

// claudeNotesFloor is the oldest Claude Code release a run wired to its notes
// starts on: 2.1.163, the first whose Stop hook's additional context reaches the
// model.
//
// An older release with mcp_tool hooks (2.1.118+) runs both hooks and reads the
// PostToolBatch answer but drops the Stop one (measured on 2.1.150): the note was
// taken at Stop, recorded read and published delivered, and no request carried
// it. Started without the hooks, its notes would be off unseen, so the run is
// refused and says the box's harness needs an upgrade ([claudeNotesGate]).
const claudeNotesFloor = "2.1.163"

// claudeEffortFloor is the oldest Claude Code release a run is handed the
// entry's `--effort` on: 2.1.163, the oldest measured to take the flag.
const claudeEffortFloor = "2.1.163"

// claudeSnapshotFloor is the oldest Claude Code release a resumed run is
// started with `--system-prompt-snapshot off` on: 2.1.267, which added the
// option ("Added `--system-prompt-snapshot off` to render the system prompt
// fresh on every request instead of reusing the conversation's recorded
// prompt"). 2.1.263, a release boxes were measured on, refuses it as an
// unknown option, which would fail every resumed run there.
const claudeSnapshotFloor = "2.1.267"

// claudeLocalGCFloor is the oldest Claude Code release a run is started with
// CLAUDE_CODE_TRANSCRIPT_LOCAL_GC on: 2.1.304, which fixed it "dropping messages
// from the saved transcript when the disk filled up while the file was being
// trimmed" (2.1.296 one with Unicode line separators). On, a `-p` run trims its
// transcript at 5 MiB with a compaction, after each compaction, and after 20 MiB
// appended since the last trim: it keeps the history from the last compaction on
// (all a resume restores), writes a new file and RENAMES it over the transcript
// (read in 2.1.310), so a carried session stays near that size and a pack whose
// tar a trim overlaps sees the inode change ([claudeHeldTar]). On an older
// release it is UNSET, whatever set it: a trim that loses messages loses them
// from the session a later run continues.
const claudeLocalGCFloor = "2.1.304"

// claudeCarryFloor is the oldest Claude Code release a box continues a session
// carried from ANOTHER box on: 2.1.310, the first whose results state the run's
// own spend per model, which a continued run is charged from whatever its
// session's totals say ([claudeResult.SinceLastResult]). It is above every
// release a carried continuation needs: 2.1.304, whose trim loses nothing
// ([claudeLocalGCFloor]); 2.1.299 (no approved tool call run twice on resume);
// 2.1.288 (no files dropped that a compaction restored); 2.1.269 (replies kept
// across a model switch); 2.1.267 ([claudeSnapshotFloor]); 2.1.265 (keeps a call
// the previous process died in); 2.1.234 ([claudeProjectDir]).
const claudeCarryFloor = "2.1.310"

// claudeCarriedWriterFloor is the oldest Claude Code release whose session
// another box continues: 2.1.300, from which a compaction's summary and the
// recent messages it kept survive a headless resume ("compactions from this
// version on", its changelog) — a session compacted by an older release comes
// back without its summary, or with the history before it.
const claudeCarriedWriterFloor = "2.1.300"

// claudeSettingsPinFloor is the oldest Claude Code release a run is started
// with its compaction point and what a resume of its session loads pinned in
// its settings as well as in its environment ([claudeSettingsPin],
// [claudePinGate]): 2.1.310, the oldest release read whose settings schema
// declares `env` ("Environment variables to set for Claude Code sessions",
// 2.1.310 and 2.1.322 alike). A settings file a release cannot validate is
// ignored whole in print mode, the retention with it ([claudeSettings]), so the
// pinned copy is handed only where the key was read. It is also the oldest
// release a box continues a session carried from another box on
// ([claudeCarryFloor]), so every carried session's run has the pin.
const claudeSettingsPinFloor = "2.1.310"

// claudePinGate is the part of a Claude Code run's script that hands its
// harness, on a release at or after [claudeSettingsPinFloor], the pinned copy
// of the settings file the gates before it chose ([Paths.ClaudeSettingsPinned],
// [Paths.ClaudeNotesSettingsPinned]), and on an older release the file as
// chosen. Each file the run's gates can choose has its arm: every run's, and
// the notes' for a run wired to its notes. Those are the files
// [ClaudeCode.WriteConfig] writes with their pinned copies, and no other file
// is named.
func claudePinGate(paths Paths, notes bool) string {
	files := [][2]string{{paths.ClaudeSettings(), paths.ClaudeSettingsPinned()}}
	if notes {
		files = append(files, [2]string{paths.ClaudeNotesSettings(), paths.ClaudeNotesSettingsPinned()})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "if at %s; then case $cs in\n", floorArgs(claudeSettingsPinFloor))
	for _, file := range files {
		fmt.Fprintf(&b, "  %s) cs=%s ;;\n", shellword.Quote(file[0]), shellword.Quote(file[1]))
	}
	b.WriteString("esac; fi\n")
	return b.String()
}

// claudeNotesGate is the part of a run's script wired to its notes that, by the
// harness's release ([claudeRelease]), at or after [claudeNotesFloor] chooses
// the settings with the notes' hooks ([Paths.ClaudeNotesSettings]) and the
// standing instructions told of notes ([Paths.ClaudeNotesSystem]), setting
// hk, and on an older one refuses the run, saying so: started there, its notes
// would be off unseen, every one waiting for its review.
func claudeNotesGate(hooked, notes string) string {
	return "if at " + floorArgs(claudeNotesFloor) + "; then hk=1; cs=" + shellword.Quote(hooked) +
		`; set -- "$@" --append-system-prompt-file ` + shellword.Quote(notes) + "; else " +
		`echo "crewlet: this box runs Claude Code $n, older than ` + claudeNotesFloor + `, the oldest ` +
		`that reads a note at the run's finish, so the run's notes would be off unseen and it does not ` +
		`start; upgrade Claude Code in the box image" >&2; exit 1; fi` + "\n"
}

// claudeSessionGate is the part of a run's script that names the session its
// harness runs in, as s, whether it is the one asked to continue, as k, and where
// it was asked and could not, why, as w, adding the flags that say so.
//
// A run that continues one continues the copy its box HOLDS (a transcript under
// its id in any project directory) unless a carried archive ([Paths.SessionCarried])
// holds what the copy does not: a digest not among the box's
// ([Paths.SessionDigests], carry.go). That archive, and one carried into a box
// with none, is RESTORED: unpacked in staging, its three parts moved under the
// pinned project directory ([claudeProjectDir]) once it holds a transcript, on a
// release that continues one ([claudeCarryFloor]), digest recorded ([recordDigest]).
// Neither held nor restored, the run starts the minted session and says why.
func claudeSessionGate(session Session, paths Paths, rewired bool) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := shellword.Quote
	if !session.Continues {
		line(`s=%s k=no w=`, q(session.ID))
		line(`set -- "$@" --session-id %s`, q(session.ID))
		return b.String()
	}
	cfg := paths.ClaudeConfigDir()
	project := cfg + "/projects/" + claudeProjectDir
	staged := paths.SessionStaging()
	carried := q(paths.SessionCarried())
	digests := q(paths.SessionDigests())
	in := staged + "/in"
	from := in + "/projects/" + claudeProjectDir + "/" + session.ID
	tasks := in + "/tasks/" + session.ID
	restore := strings.Join([]string{
		"rm -rf " + q(staged),
		"mkdir -p " + q(in) + " " + q(project) + " " + q(cfg+"/tasks"),
		"tar -xzf " + carried + " -C " + q(in) + " < /dev/null > /dev/null 2>&1",
		"[ -f " + q(from+".jsonl") + " ]",
		"[ ! -L " + q(from+".jsonl") + " ]",
		// THE BOX'S OWN COPY GOES only now, once the archive is known to
		// hold a transcript: wherever a release filed it, and its task list.
		"rm -rf " + q(cfg) + "/projects/*/" + q(session.ID+".jsonl") + " " + q(cfg) + "/projects/*/" +
			q(session.ID) + " " + q(cfg+"/tasks/"+session.ID),
		"{ [ ! -d " + q(from) + " ] || [ -L " + q(from) + " ] || mv " + q(from) + " " +
			q(project+"/"+session.ID) + "; }",
		"{ [ ! -d " + q(tasks) + " ] || [ -L " + q(tasks) + " ] || mv " + q(tasks) + " " +
			q(cfg+"/tasks/"+session.ID) + "; }",
		// THE TRANSCRIPT LAST, so a box that holds it holds the whole of
		// what was restored with it.
		"mv " + q(from+".jsonl") + " " + q(project+"/"+session.ID+".jsonl"),
	}, " && ")
	line(`s=%s k=yes w= h= d=`, q(session.ID))
	line(`for f in %s/projects/*/%s; do [ -s "$f" ] && h=1; done`, q(cfg), q(session.ID+".jsonl"))
	// THE ARCHIVE'S DIGEST, where one was carried in and the box can take it:
	// the box's copy is kept unless the archive is one it does not hold.
	line(`if [ -s %s ]; then d=$(cksum < %s 2> /dev/null) || d=; fi`, carried, carried)
	if session.Replaces {
		// A COPY ITS HARNESS WAS CUT OFF IN is replaced by the archive whatever
		// it holds: it may end in a record the harness never finished, and
		// the archive is the session as far as anything after it is told.
		line(`if [ -n "$h" ] && [ -s %s ]; then h=; fi`, carried)
	} else {
		line(`if [ -n "$h" ] && [ -n "$d" ] && ! grep -qxF "$s $d" %s 2> /dev/null; then h=; fi`, digests)
	}
	line(`if [ -z "$h" ]; then`)
	line(`  if [ ! -s %s ]; then w='it is not in this box, and no archive of it was carried here'`, carried)
	// A BOX RELEASE THAT DOES NOT READ continues nothing carried, as the
	// engine's check holds it ([boxNamedNoRelease]): no bare MAJOR.MINOR.PATCH,
	// as [releaseOf] reads one; `at` alone takes one for a current release,
	// which every other gate wants ([claudeRelease]).
	line(`  elif ! { %s[ -n "$rv" ]; }; then w=%s`, bareReleaseShell("rv"), q(boxNamedNoRelease("Claude Code")))
	line(`  elif ! at %s; then w="this box runs Claude Code $n, older than %s, the oldest that continues a session carried from another box"`,
		floorArgs(claudeCarryFloor), claudeCarryFloor)
	line(`  elif ! command -v tar > /dev/null 2>&1 || ! command -v gzip > /dev/null 2>&1; then w='this box has no tar and gzip to restore its archive with'`)
	line(`  elif ! { %s; }; then w='its archive could not be restored'`, restore)
	// A RESTORE IS CONTINUED ONLY ONCE ITS DIGEST IS RECORDED, and read back
	// ([recordDigest]): continued with no line of the archive in the list —
	// a full disk, a cksum that failed — the box's copy would go on past an
	// archive the box does not know it holds, and that archive carried in
	// again would be restored over it. So the run starts a session of its
	// own instead, and the copy restored is never gone on past. Recorded
	// before the restore, a restore that failed after the box's own copy
	// went would leave the line naming a copy that does not hold it.
	line(`  elif ! { %s; }; then w=%s`, recordDigest(paths, "s"), q(restoreDigestUnrecorded))
	line(`  fi`)
	line(`  rm -rf %s`, q(staged))
	line(`fi`)
	line(`rm -f %s`, carried)
	line(`if [ -z "$w" ]; then`)
	// A TRANSCRIPT CUT OFF MID-LINE is cut back to its last whole record: the
	// session's own and EVERY THREAD UNDER ITS SIDECAR DIRECTORY, at any depth. A
	// harness killed mid-append, or a pack made while it worked, leaves a line with
	// no newline onto which the next record is written, losing it too (measured on
	// 2.1.310). A FILE WITH NO WHOLE LINE IS LEFT AS IT IS: 2.1.310 continues such a
	// thread from nothing whatever the restore makes of it. A box with no find cuts
	// no thread, and says so.
	//
	// TORN IS WHERE THE LAST BYTE IS NO NEWLINE, AS WC COUNTS IT, never as a command
	// substitution reads it, which drops a NUL (measured). GNU tar NUL-pads a member
	// that shrank mid-read (2.1.310 rewrites a transcript in place to withdraw a
	// message); read as text, the padding would pass for a whole last line.
	line(`  torn() { if [ -s "$1" ] && [ "$(tail -c 1 "$1" | wc -l)" -eq 0 ]; then cl=$(wc -l < "$1"); if [ "$cl" -gt 0 ] 2> /dev/null; then head -n $cl "$1" > "$1.cut" && mv -f "$1.cut" "$1"; fi; fi; }`)
	line(`  for cf in %s/projects/*/%s; do torn "$cf"; done`, q(cfg), q(session.ID+".jsonl"))
	line(`  for sd in %s/projects/*/%s; do if [ -d "$sd" ] && [ ! -L "$sd" ]; then if command -v find > /dev/null 2>&1; then find "$sd" -type f -name '*.jsonl' 2> /dev/null | while IFS= read -r cf; do torn "$cf"; done; else echo "crewlet: this box has no find, so no subagent's thread of the session was cut back to its last whole line before the harness resumed it" >&2; fi; fi; done`,
		q(cfg), q(session.ID))
	// AND THE SESSION TAKEN UP AS IT STANDS, whatever set the switches that
	// move what a resume hands its model ([claudeResumeSwitches]): the turn
	// its last process was cut off in run again before this run's brief, and
	// a transcript past 5 MiB read by another rule.
	line(`  unset %s`, strings.Join(claudeResumeSwitches, " "))
	line(`  set -- "$@" --resume %s`, q(session.ID))
	line(`  if at %s; then set -- "$@" --system-prompt-snapshot off; fi`, floorArgs(claudeSnapshotFloor))
	// THE RUN IS WIRED TO THE SESSION IT STARTS where it was handed a wiring for
	// each (rewired, [sandbox.RunRequest.FreshWiring]): the MCP configuration
	// under the continued session's bridge token or the fresh one's moved over
	// it, the other removed before the harness starts. So the token the harness
	// dials is the box's word to the bridge on which session it started
	// (internal/api/mcpbridge), one the model can neither forge nor take back. A
	// configuration that cannot be moved ends the run before its harness starts,
	// rather than start a session on the continued one's token, whose skill guard
	// it would be held to.
	if rewired {
		line(`  rm -f %s`, q(paths.MCPConfigFresh()))
	}
	line(`else`)
	line(`  s=%s k=no`, q(session.Fresh))
	line(`  printf '%%s%%s\n' %s "$w" >&2`, q("crewlet: this run was asked to continue the Claude Code session "+session.ID+
		", and starts a new session, "+session.Fresh+", instead: "))
	line(`  set -- "$@" --session-id %s`, q(session.Fresh))
	// A NEW SESSION IS HANDED THE BRIEF WRITTEN FOR ONE, where the run was
	// given it: the brief on the script's stdin continues a session the
	// harness no longer holds ([sandbox.RunRequest.FreshBrief]).
	line(`  [ ! -s %s ] || exec < %s`, q(paths.FreshBrief()), q(paths.FreshBrief()))
	if rewired {
		line(`  mv -f %s %s || { echo 'crewlet: the new session could not be wired to the tools of its seat, so the run does not start' >&2; exit 1; }`,
			q(paths.MCPConfigFresh()), q(paths.MCPConfig()))
	}
	line(`fi`)
	return b.String()
}

// claudeCapture is the part of a run's script that packs the session its harness
// ran in once it has exited, to be continued in another box: one gzipped tar of
// the transcript, its sidecar directory and its task list, made in staging and
// moved over [Paths.SessionCapture] whole, its digest read back first
// ([packDigest]). NEVER ANYTHING ELSE: not the login (`.credentials.json`), not
// `.claude.json`, and not `file-history/`, the file-edit snapshots of the files
// the run edited, which carry the checkout rather than the conversation.
//
// NOTHING TO PACK IS SAID NOWHERE: a line would replace the harness's words on a
// run that crashed at its start. A failed pack is said on the error stream; the
// run's status is never the pack's.
func claudeCapture(paths Paths) string {
	return claudePack(paths, packAt{staged: paths.SessionStaging(), dest: paths.SessionCapture()})
}

// packAt is where one of [claudePack]'s packs is made and kept, and which rule
// it reads its files by.
type packAt struct {
	// staged is the directory the archive is made in, and dest the path it is
	// moved over whole once it is made: the run's script's own pair
	// ([Paths.SessionStaging], [Paths.SessionCapture]); a pack on demand's,
	// its own [Paths.SessionPackStaging] and its own [Paths.SessionPackHeld],
	// of a job that is over or still working; or a save's, its own
	// [Paths.SessionSaveStaging] and its own [Paths.SessionSave].
	staged, dest string

	// working says the pack is made beside a harness that may still be
	// working, which may rewrite a file as tar reads it: the pack is held to
	// [claudeHeldTar]'s rule. False is a pack made once the harness is gone:
	// the run's own end, and one made on demand of a job that is over.
	working bool
}

// claudePack is [claudeCapture]'s pack, made in at.staged and moved over
// at.dest: ONE PACK for the run's script at its end, an on-demand pack of a job
// that never packed or that its box would not end, and a save, so each is the
// same archive of the same files and keeps its digest.
//
// A pack made once the harness is gone (the run's own end, an on-demand pack of
// a job that is over) takes GNU tar's exit 1 with "file changed as we read it"
// as a whole archive: the only writer left outlived the harness. A pack of a job
// that may still be working is held to [claudeHeldTar]'s rule instead, which
// reads none of tar's words.
func claudePack(paths Paths, at packAt) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := shellword.Quote
	cfg := paths.ClaudeConfigDir()
	project := "projects/" + claudeProjectDir
	line(`if [ -s %s/"$s".jsonl ]; then`, q(cfg+"/"+project))
	line(`  if command -v tar > /dev/null 2>&1 && command -v gzip > /dev/null 2>&1; then`)
	// THE TRANSCRIPT FIRST: [claudeHeldTar] reads back its first operand, the
	// session's own transcript, on every pack it makes.
	line(`    set -- %s/"$s".jsonl`, q(project))
	line(`    if [ -d %s/"$s" ] && [ ! -L %s/"$s" ]; then set -- "$@" %s/"$s"; fi`,
		q(cfg+"/"+project), q(cfg+"/"+project), q(project))
	line(`    if [ -d %s/"$s" ] && [ ! -L %s/"$s" ]; then set -- "$@" tasks/"$s"; fi`,
		q(cfg+"/tasks"), q(cfg+"/tasks"))
	tar := fmt.Sprintf(`{ tar -czf %s -C %s "$@" < /dev/null > /dev/null 2> %s; t=$?; [ "$t" = 0 ] || { [ "$t" = 1 ] && grep -q 'changed as we read it' %s; }; }`,
		q(at.staged+"/capture"), q(cfg), q(at.staged+"/error"), q(at.staged+"/error"))
	if at.working {
		tar = claudeHeldTar(cfg, at.staged)
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

// claudeHeldTar is [claudePack]'s tar for a pack made beside a harness that may
// still be writing: it packs "$@" (relative to cfg) into staged/capture, or fails
// with the one check that refused it, on one line in staged/error.
//
// A SESSION BEING WRITTEN IS NOT APPEND-ONLY, and tar's word cannot be taken:
// withdrawing a message rewrites the transcript IN PLACE, possibly growing it
// back (2.1.310). Growing is GNU's exit 1 and busybox's exit 0, shrinking exit 1
// from both, and shrunk-and-regrown passes every size check (GNU tar 1.35,
// BusyBox 1.37.0). So the pack lists what it reads before the tar and holds it
// to that after, failing closed ([heldTools], [heldClockWaits], [heldPlainMax]).
// Only a `.jsonl` that grew is taken, its last line torn ([claudeSessionGate]).
// Reading only newer files rests on the box's clock not stepping back mid-pack.
func claudeHeldTar(cfg, staged string) string {
	q := shellword.Quote
	plain := strconv.FormatInt(heldPlainMax, 10)
	return strings.Join([]string{
		`( st=` + q(staged),
		`why() { printf '%s\n' "$*" > "$st/error"; exit 1; }`,
		// A LIST OR AN ANSWER IS KEPT BY THE SHELL AND READ BACK. lines is its
		// text as a file holds it, every line ended and nothing for none; keep
		// writes it to the file $1 and refuses the pack, saying $3, where the
		// file's cksum is not that of what was written: a write a full disk
		// lost, or cut short, though the writer was told it landed.
		`lines() { [ -z "$1" ] || printf '%s\n' "$1"; }`,
		`keep() { lines "$2" > "$1" && [ "$(cksum < "$1")" = "$(lines "$2" | cksum)" ] || why "$3"; }`,
		// A COMPARISON THAT CANNOT BE MADE REFUSES: grep's exit 1 is no
		// difference, and its 0 a difference whose answer names the lines that
		// differ; its 2 — a list it could not read — and a 0 that names no
		// line are neither, and a check that took them for "none" would hold a
		// pack it never compared. The answer, read through a pipe, is kept for
		// the check that reads it.
		`differs() { w=$1 o=$2; shift 2; a=$(grep "$@"); r=$?; case $r in ` +
			`0) [ -n "$a" ] || why "the pack could not compare $w: grep found a difference and named none"; ` +
			`keep "$o" "$a" "the pack could not compare $w: its answer could not be kept";; ` +
			`1) ;; *) why "the pack could not compare $w: grep exited $r";; esac; return "$r"; }`,
		// THE TOOLS.
		`for c in ` + heldTools + `; do command -v "$c" > /dev/null 2>&1 || why "this box has no $c, which a ` +
			`pack beside a working harness checks what it packed with"; done`,
		`cd ` + q(cfg) + ` || why 'the directory the session is filed under could not be entered'`,
		// NO FILE FROM THE BOX'S FUTURE, set to the box's present before the
		// marker — never to an earlier time, which could put a write behind
		// another pack's marker; what the walk could not set stays ahead, and
		// the checks read it as written.
		`: > "$st/now" || why "the pack could not write the reference it tells the box's future by"`,
		`find "$@" ! -type l -newer "$st/now" -exec touch -c {} + 2> /dev/null`,
		// THE MARKER, and the wait for the box's own find.
		`: > "$st/marker" && touch "$st/clock" || why 'the pack could not write its marker'`,
		`i=0; until [ -n "$(find "$st/clock" -newer "$st/marker" 2> /dev/null)" ]; do ` +
			`[ "$i" -lt ` + strconv.Itoa(heldClockWaits) + ` ] || why "the box's clock did not move: its find ` +
			`never read a file written now as newer than one written before it, so a file written as the ` +
			`session was packed could not be told from one written before"; sleep 0.1 || why "this box's ` +
			`sleep takes no tenth of a second, which the pack's wait for the box's clock counts in"; ` +
			`i=$((i + 1)); touch "$st/clock"; done`,
		// THE MANIFEST, and the plaintext it adds up to. The files as find
		// names them, as tar names its members; their inodes and sizes as ls
		// and wc print them, compared only with what the same command prints
		// after the tar. Each list is read through a pipe, refused where its
		// find exited otherwise than 0 — a find that could not walk the whole
		// session, or whose `-exec` failed, leaves a file out of every check
		// made of the list — or where it holds no line, and kept; as every
		// list below is.
		`v=$(find "$@" -type f 2> /dev/null) && [ -n "$v" ] || why 'the files of the session could not be listed'`,
		`keep "$st/files" "$v" 'the files of the session could not be listed'`,
		`v=$(find "$@" -type f -exec ls -i {} + 2> /dev/null) && v=$(lines "$v" | sed 's/^ *//') && ` +
			`[ -n "$v" ] || why 'the files of the session could not be listed'`,
		`keep "$st/inodes" "$v" 'the files of the session could not be listed'`,
		`v=$(find "$@" -type f -exec wc -c {} + 2> /dev/null) && v=$(lines "$v" | sed -e 's/^ *//' ` +
			`-e '/^[0-9]* total$/d') && [ -n "$v" ] || why 'the files of the session could not be listed'`,
		`keep "$st/sizes" "$v" 'the files of the session could not be listed'`,
		`p=0; while read -r n f; do case $n in ''|*[!0-9]*) why "a file's size could not be read: $f";; esac; ` +
			`p=$((p + n)); done < "$st/sizes"`,
		`[ "$p" -le ` + plain + ` ] || why "the files of the session are $p bytes, past the ` + plain +
			` a kept session's scrub reads, so it would be refused once packed"`,
		// THE TAR, its words read by nothing but the failure it reports, and
		// its archive listed. Every grep below reads a file of patterns that
		// is never empty: busybox's grep reads an empty one as matching every
		// line, GNU's as matching none, and a check that turned on which grep
		// a box has would pass on one of them.
		`tar -czf "$st/capture" "$@" < /dev/null > /dev/null 2> "$st/tar"; t=$?`,
		`[ "$t" = 0 ] || [ "$t" = 1 ] || why "tar exited $t: $(head -n 1 "$st/tar")"`,
		`v=$(tar -tzf "$st/capture" < /dev/null 2> /dev/null) || why 'the archive the pack made could not be read back'`,
		`[ -n "$v" ] || why 'the archive the pack made holds no file'`,
		`keep "$st/members" "$v" 'the archive the pack made could not be read back'`,
		// GONE: a file the manifest listed, before REPLACED reads its absence
		// as another file's inode.
		`while IFS= read -r f; do [ -e "$f" ] || [ -L "$f" ] || why "a file was removed while the session ` +
			`was packed: $f"; done < "$st/files" || why "the pack could not read back its list of the session's files"`,
		// REPLACED.
		`v=$(find "$@" -type f -exec ls -i {} + 2> /dev/null) && v=$(lines "$v" | sed 's/^ *//') && ` +
			`[ -n "$v" ] || why 'the files of the session could not be listed again'`,
		`keep "$st/inodes.after" "$v" 'the files of the session could not be listed again'`,
		`if differs "the files' inodes before and after tar" "$st/moved" -vxF -f "$st/inodes.after" "$st/inodes"; ` +
			`then read -r n f < "$st/moved"; why "a file was replaced by another while the session was packed: $f"; fi`,
		// SHRANK.
		`v=$(find "$@" -type f -exec wc -c {} + 2> /dev/null) && v=$(lines "$v" | sed -e 's/^ *//' ` +
			`-e '/^[0-9]* total$/d') && [ -n "$v" ] || why 'the files of the session could not be listed again'`,
		`keep "$st/sizes.after" "$v" 'the files of the session could not be listed again'`,
		`if differs "the files' sizes before and after tar" "$st/resized" -vxF -f "$st/sizes.after" "$st/sizes"; ` +
			`then while read -r n f; do c=$(wc -c < "$f") && [ "$((c))" -ge "$n" ] || why "a file shrank while ` +
			`the session was packed: $f"; done < "$st/resized" || why 'the pack could not read back the sizes it ` +
			`compared'; fi`,
		// MISSING.
		`if differs "the files it listed with what its archive holds" "$st/missing" -vxF -f "$st/members" ` +
			`"$st/files"; then read -r f < "$st/missing"; why "a file is not in the archive the pack made: $f"; fi`,
		// WRITTEN: a member that is not a transcript.
		`v=$(find "$@" -type f ! -name '*.jsonl' -newer "$st/marker" 2> /dev/null) || ` +
			`why 'the pack could not list the files written since its marker'`,
		`keep "$st/written" "$v" 'the pack could not list the files written since its marker'`,
		`if differs "the files written since its marker with what its archive holds" "$st/rewritten" ` +
			`-xF -f "$st/members" "$st/written"; then read -r f < "$st/rewritten"; why "a file was written ` +
			`while the session was packed: $f"; fi`,
		// NOT A PREFIX: the session's own transcript, the first operand, on
		// every pack whatever find or any list says of it, and then every
		// other transcript member written since the marker — their list
		// compared with the archive only where it names one, so that no grep
		// reads an empty file of patterns — each read back out of the archive
		// as a stream. cksum prints the bytes it read beside its sum, so the
		// member's length is its own word, and an extraction that failed is
		// told apart from an empty member by a mark its failure empties,
		// which a pipeline's status cannot say: written before the
		// extraction, and emptied rather than written by its failure, which
		// a full disk can never keep from happening.
		`prefix() { printf x > "$st/read" || why 'the pack could not mark a transcript it reads back'; ` +
			`a=$( { tar -xzOf "$st/capture" "$1" < /dev/null 2> /dev/null || : > "$st/read"; } | cksum ) && ` +
			`[ -s "$st/read" ] || why "a transcript could not be read back out of the archive the pack made: $1"; ` +
			`b=$(head -c "${a#* }" "$1" | cksum) && [ "$a" = "$b" ] || why "a transcript was rewritten in place ` +
			`while the session was packed, so what the archive holds of it is not the start of the file: $1"; }`,
		`prefix "$1"`,
		`v=$(find "$@" -type f -name '*.jsonl' -newer "$st/marker" 2> /dev/null) || ` +
			`why 'the pack could not list the transcripts it reads back'`,
		`keep "$st/transcripts" "$v" 'the pack could not list the transcripts it reads back'`,
		`if [ -n "$v" ] && differs "the transcripts it reads back with what its archive holds" "$st/prefixes" ` +
			`-xF -f "$st/transcripts" "$st/members"; then while IFS= read -r f; do [ "$f" = "$1" ] || prefix "$f"; ` +
			`done < "$st/prefixes" || why 'the pack could not read back the transcripts it listed'; fi`,
		// GONE, LAST: every member of the archive, once every check that
		// reads a live file has read it.
		`while IFS= read -r f; do case $f in */) ;; *) [ -e "$f" ] || [ -L "$f" ] || why "a file was ` +
			`removed while the session was packed: $f";; esac; done < "$st/members" || why 'the pack could not ` +
			`read back the list of what its archive holds'`,
		`) 2> /dev/null`,
	}, "\n")
}

// heldTools are the tools [claudeHeldTar] reads a session with beside tar and
// gzip, which [claudePack] asks the box for itself.
const heldTools = "find ls wc sed grep head cksum touch sleep"

// heldClockWaits is how many sleeps of a tenth of a second [claudeHeldTar]
// makes, at most, waiting for its box's find to read a file written after its
// marker as newer than it.
//
// FIFTY, five seconds: busybox's find compares whole seconds, so the wait is a
// second at most on a box whose clock moves, and five is room for a contended
// box's sleeps to run long; a box whose clock has not moved past that is one
// whose writes the pack could not order, so it fails rather than packs blind.
const heldClockWaits = 50

// heldPlainMax is the most plaintext a pack beside a working harness packs
// ([claudeHeldTar]), and the most an OpenCode pack on demand exports
// ([openCodePack]): the most one reading of an archive yields to a kept
// session's scrub ([scrubPlainMax]). A session
// whose files add up past it is refused by the take once packed, so it is not
// packed at all: its tar beside a working harness would cost that harness's box
// some seconds of a core — a pack runs at about 43 MiB/s on one, measured — and
// a moment of as much disk, for an archive nothing would keep. A file's size
// is its plaintext and not quite the tar's, which adds a header to each, so
// the bound refuses what the scrub would and never more.
const heldPlainMax = scrubPlainMax

// floorParts is a MAJOR.MINOR.PATCH version's three numbers, as text.
func floorParts(version string) (major, minor, patch string) {
	major, rest, _ := strings.Cut(version, ".")
	minor, patch, _ = strings.Cut(rest, ".")
	return major, minor, patch
}

// floorArgs is a MAJOR.MINOR.PATCH floor as the arguments the run's script's
// `at` takes ([claudeRelease]): its three numbers, space-separated.
func floorArgs(version string) string {
	major, minor, patch := floorParts(version)
	return major + " " + minor + " " + patch
}

// WriteConfig writes the run's system text and settings where
// [ClaudeCode.Command] names them, and renders the scoped MCP surface: every
// run's settings ([claudeSettings]) always, the rest where there is some, and
// for a run that may start a hook of the engine's why the box's settings would
// run none ([Paths.ClaudeHooksOff]). A RUN WIRED TO ITS NOTES HAS ITS SYSTEM
// TEXT AND SETTINGS TWICE: as they stand ([Paths.System],
// [Paths.ClaudeSettings]) and with what its notes need
// ([Paths.ClaudeNotesSystem], [Paths.ClaudeNotesSettings]); the box picks by
// the harness's release ([claudeNotesGate]). Its SKILLS are a plugin with a
// load-reporting hook ([writeClaudeSkills], [claudeSkillsGate]). EVERY
// SETTINGS FILE HAS A PINNED COPY BESIDE IT ([claudeSettingsPin]), handed in
// its place to a release that takes it ([claudePinGate]).
func (c ClaudeCode) WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, _ Session,
	paths Paths) (string, error) {
	if req.System != "" {
		if err := box.WriteFile(ctx, paths.System(), []byte(req.System)); err != nil {
			return "", fmt.Errorf("executor: writing the system prompt: %w", err)
		}
	}
	// THE RUN'S NO_PROXY OVER A SETTINGS FILE'S, both spellings held for the
	// script's merge ([claudeNoProxyMerge]) where a file sets either.
	entries, set, err := claudeProxyEntries(ctx, box, paths)
	if err != nil {
		return "", err
	}
	var bypass map[string]string
	list := ""
	if set && len(directHosts(req)) > 0 {
		bypass = map[string]string{"NO_PROXY": claudeNoProxyHolder, "no_proxy": claudeNoProxyHolder}
		list = strings.Join(entries, ",")
	}
	if err := box.WriteFile(ctx, paths.ClaudeNoProxy(), []byte(list)); err != nil {
		return "", fmt.Errorf("executor: writing the Claude Code settings' NO_PROXY entries: %w", err)
	}
	pin := claudeSettingsPin(claudeCodeWindow(req.Model))
	maps.Copy(pin, bypass)
	if err := writeClaudeSettingsPair(ctx, box, paths.ClaudeSettings(), paths.ClaudeSettingsPinned(),
		newClaudeSettings(bypass), newClaudeSettings(pin)); err != nil {
		return "", err
	}
	if len(req.Skills) > 0 {
		if err := writeClaudeSkills(ctx, box, req.Skills, req.SkillLoads, paths); err != nil {
			return "", err
		}
	}
	if len(req.Skills) > 0 || req.Steer.Set() {
		off, err := claudeHooksOff(ctx, box, paths, req.Steer.Set(), req.Skills)
		if err != nil {
			return "", err
		}
		if err = box.WriteFile(ctx, paths.ClaudeHooksOff(), []byte(off)); err != nil {
			return "", fmt.Errorf("executor: writing why the box's settings switch the run's hooks off: %w", err)
		}
	}
	if req.Steer.Set() {
		if err := writeClaudeSettingsPair(ctx, box, paths.ClaudeNotesSettings(), paths.ClaudeNotesSettingsPinned(),
			claudeSteerSettings(req.Steer, bypass), claudeSteerSettings(req.Steer, pin)); err != nil {
			return "", err
		}
		told := withNoteInstruction(req.System, c.NoteInstruction())
		if err := box.WriteFile(ctx, paths.ClaudeNotesSystem(), []byte(told)); err != nil {
			return "", fmt.Errorf("executor: writing the system prompt told of notes: %w", err)
		}
	}
	if len(req.MCPServers) == 0 {
		return "", nil
	}
	if err := writeClaudeMCP(ctx, box, paths.MCPConfig(), req.MCPServers); err != nil {
		return "", err
	}
	// AND UNDER THE JOB'S OTHER BRIDGE TOKEN, for a session the run's script
	// starts of its own in place of the one it continues
	// ([sandbox.RunRequest.FreshWiring]), which the script moves over the
	// first where it starts one ([claudeSessionGate]). The whole of what
	// differs: the run's hooks name the bridge by its server's name, never its
	// address.
	if f := req.FreshWiring; f != nil {
		if err := writeClaudeMCP(ctx, box, paths.MCPConfigFresh(), f.MCPServers); err != nil {
			return "", err
		}
	}
	return paths.MCPConfig(), nil
}

// writeClaudeSettingsPair writes one of a run's settings files twice: settings
// to file, which a release before [claudeSettingsPinFloor] is handed, and
// pinned, the same settings with the compaction point and a resume's load
// pinned ([claudeSettingsPin]), to pinnedFile, which a release at or after it
// is handed in file's place ([claudePinGate]).
func writeClaudeSettingsPair(ctx context.Context, box sandbox.Sandbox, file, pinnedFile string, settings, pinned any) error {
	if err := writeClaudeSettings(ctx, box, file, settings); err != nil {
		return err
	}
	return writeClaudeSettings(ctx, box, pinnedFile, pinned)
}

// writeClaudeSettings renders settings into file: every run's settings
// ([claudeSettings]), or those with the notes' hooks besides
// ([claudeNotesSettings]).
func writeClaudeSettings(ctx context.Context, box sandbox.Sandbox, file string, settings any) error {
	blob, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("executor: rendering Claude Code's settings: %w", err)
	}
	if err = box.WriteFile(ctx, file, blob); err != nil {
		return fmt.Errorf("executor: writing Claude Code's settings: %w", err)
	}
	return nil
}

// writeClaudeMCP renders servers into file, in the harness's own .mcp.json
// vocabulary ([claudeCodeMCP]).
func writeClaudeMCP(ctx context.Context, box sandbox.Sandbox, file string,
	servers map[string]sandbox.MCPServer) error {

	rendered := make(map[string]any, len(servers))
	for name, s := range servers {
		rendered[name] = claudeCodeMCP(s)
	}
	blob, err := json.MarshalIndent(map[string]any{"mcpServers": rendered}, "", "  ")
	if err != nil {
		return err
	}
	return box.WriteFile(ctx, file, blob)
}

// claudeCodeMCP is one server in this harness's own .mcp.json vocabulary. THE
// KEY NAMES ARE THIS RUNNER'S: what `claude --mcp-config` reads. An empty `env`
// or `headers` is OMITTED.
//
// `alwaysLoad` ONLY WHERE THE ENGINE ASKS ([sandbox.MCPServer.AlwaysLoaded]):
// Claude Code defers every server's tools behind tool search, which would make a
// run search for submit_work, the call that ends it.
//
// `timeout` ONLY WHERE THE ENGINE SETS A WAIT ([sandbox.MCPServer.CallTimeout]):
// milliseconds; the harness cancels a call at it whatever progress flows
// (measured on 2.1.310). A release whose schema has no such key drops it.
func claudeCodeMCP(s sandbox.MCPServer) map[string]any {
	var out map[string]any
	if s.Transport == sandbox.TransportHTTP {
		out = map[string]any{"type": string(sandbox.TransportHTTP), "url": s.URL}
		if len(s.Headers) > 0 {
			out["headers"] = s.Headers
		}
	} else {
		out = map[string]any{"command": s.Command, "args": s.Args}
		if len(s.Env) > 0 {
			out["env"] = s.Env
		}
	}
	if s.AlwaysLoaded {
		out["alwaysLoad"] = true
	}
	if ms := waitMillis(s.CallTimeout); ms > 0 {
		out["timeout"] = ms
	}
	return out
}

// Output is where a job writes under stream-json, and what it reads.
//
// The EVENT STREAM goes to its own file, read as a stream and from its end; the
// RESULT is the stream's last line, which the wrapper copies to the result file
// after the harness exits, so it is read whole however long the stream grew. The
// done marker is the only completion signal.
//
// THE BRIEF ON STDIN, for the reasons [ClaudeCode.Command] gives. The variables
// that switch hooks and the bridge off, start the hosted mode, or switch
// compaction off or move its point are UNSET before the harness starts
// ([claudeSwitchesOff], [claudeHostedOff], [claudeCompactionOff]), for
// [ClaudeCode.Pins] to pin the session's layout over.
func (ClaudeCode) Output(paths Paths) Output {
	return Output{Stdout: paths.Stream(), Events: true, Result: paths.Result(),
		BriefOnStdin: true, Unset: slices.Concat(claudeSwitchesOff, claudeHostedOff, claudeCompactionOff)}
}

// claudeVertexRegions is the per-model Vertex region variables 2.1.330 reads
// (its binary names each), every one an endpoint of the model it names.
var claudeVertexRegions = routes(roleEndpoint,
	"VERTEX_REGION_CLAUDE_3_5_HAIKU", "VERTEX_REGION_CLAUDE_3_5_SONNET", "VERTEX_REGION_CLAUDE_3_7_SONNET",
	"VERTEX_REGION_CLAUDE_4_0_OPUS", "VERTEX_REGION_CLAUDE_4_0_SONNET", "VERTEX_REGION_CLAUDE_4_1_OPUS",
	"VERTEX_REGION_CLAUDE_4_5_OPUS", "VERTEX_REGION_CLAUDE_4_5_SONNET", "VERTEX_REGION_CLAUDE_4_6_OPUS",
	"VERTEX_REGION_CLAUDE_4_6_SONNET", "VERTEX_REGION_CLAUDE_4_7_OPUS", "VERTEX_REGION_CLAUDE_4_8_OPUS",
	"VERTEX_REGION_CLAUDE_5_5_OPUS", "VERTEX_REGION_CLAUDE_5_5_SONNET", "VERTEX_REGION_CLAUDE_5_OPUS",
	"VERTEX_REGION_CLAUDE_5_SONNET", "VERTEX_REGION_CLAUDE_FABLE_5", "VERTEX_REGION_CLAUDE_FABLE_5_1",
	"VERTEX_REGION_CLAUDE_FABLE_5_2", "VERTEX_REGION_CLAUDE_HAIKU_4_5", "VERTEX_REGION_CLAUDE_HAIKU_5_5")

// routing answers [router] with what moves, signs or shapes a Claude Code
// run's requests from the entry's env (2.1.330's binary names each): its
// profile's route (a gateway's or a cloud's address, ANTHROPIC_BASE_URL
// measured; the cloud switches and the region, profile, project and resource
// each picks), and the gateway's token, the key, the headers and the switches
// that skip a cloud's own signing, which decide how a request is signed.
func (ClaudeCode) routing() []routeVar {
	return slices.Concat(routes(roleEndpoint, "ANTHROPIC_BASE_URL", "ANTHROPIC_BEDROCK_BASE_URL",
		"ANTHROPIC_VERTEX_BASE_URL", "ANTHROPIC_FOUNDRY_BASE_URL", "ANTHROPIC_FOUNDRY_RESOURCE",
		"ANTHROPIC_VERTEX_PROJECT_ID", "CLOUD_ML_REGION", "AWS_REGION", "AWS_PROFILE", "CLAUDE_CODE_USE_BEDROCK",
		"CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
		"ANTHROPIC_AWS_BASE_URL", "ANTHROPIC_BEDROCK_MANTLE_BASE_URL", "ANTHROPIC_GOOGLE_CLOUD_BASE_URL",
		"ANTHROPIC_BEDROCK_REGION", "ANTHROPIC_SMALL_FAST_MODEL_AWS_REGION", "CLAUDE_CODE_USE_ANTHROPIC_AWS",
		"CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD", "CLAUDE_CODE_USE_MANTLE", "CLAUDE_CODE_USE_GATEWAY"),
		claudeVertexRegions,
		routes(roleSigns, "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN",
			"ANTHROPIC_CUSTOM_HEADERS",
			"CLAUDE_CODE_SKIP_ANTHROPIC_AWS_AUTH", "CLAUDE_CODE_SKIP_ANTHROPIC_GOOGLE_CLOUD_AUTH",
			"CLAUDE_CODE_SKIP_BEDROCK_AUTH", "CLAUDE_CODE_SKIP_FOUNDRY_AUTH", "CLAUDE_CODE_SKIP_MANTLE_AUTH",
			"CLAUDE_CODE_SKIP_VERTEX_AUTH"))
}

// Pins is the session's layout ([claudeSessionLayout]) under the window of the
// model req's run works under, logging a model whose window this build does not know.
func (ClaudeCode) Pins(ctx context.Context, req sandbox.RunRequest, paths Paths) []Pinned {
	window := claudeCodeWindow(req.Model)
	if !window.Known {
		log.WarnContext(ctx, "executor_context_window_unknown", "model", req.Model,
			"window_tokens", window.Tokens, "detail", "this build does not know the model's context "+
				"window, so the run compacts at the fallback window; the harness lowers it to the "+
				"model's own window where that is smaller")
	}
	return claudeSessionLayout(paths, window)
}

// claudeProjectDir is the name every Claude Code run in a box files its
// sessions under — `<config>/projects/crewlet/<session>.jsonl` — pinned as
// CLAUDE_CODE_PROJECT_DIR_NAME ([claudeSessionLayout]). Unpinned, the harness names
// that directory after the run's working directory, which in a `direct` box is
// a path holding the box's own id: so a session restored into another box would
// sit in a directory no run there looks in first, and its capture would have to
// find it by search. One constant for every run, since every run in a box is
// the engine's; it matches the `^[A-Za-z0-9_-]{1,64}$` the harness holds the name
// to, and a release before 2.1.234, which reads no such variable, files the
// session where it always did.
const claudeProjectDir = "crewlet"

// claudeSessionLayout is what the wrapper pins for every Claude Code run in a
// box, over whatever the seat's role.sandbox.env, the image or a profile set
// ([Harness.Pins]): CLAUDE_CONFIG_DIR, the box home's `.claude`
// ([Paths.ClaudeConfigDir]), because the project directory is read only where it
// is set (it moves `.claude.json` there, which the script links back to the
// home's own, [claudeStateLink]; the login stays at `.claude/.credentials.json`,
// where a local box is seeded); CLAUDE_CODE_PROJECT_DIR_NAME, [claudeProjectDir],
// so a transcript is `<config>/projects/crewlet/<session>.jsonl` whatever the
// working directory (2.1.234+); auto memory off; the compaction window; and
// DISABLE_AUTOUPDATER 1 (the package doc's "A box's release is pinned"). The
// transcript's trim is the script's, by release ([claudeLocalGCFloor]); the
// retention sweep is held off by [claudeSettings], since no variable reaches it.
func claudeSessionLayout(paths Paths, window sandbox.ExecutorWindow) []Pinned {
	return []Pinned{
		{Name: "CLAUDE_CONFIG_DIR", Value: paths.ClaudeConfigDir()},
		{Name: "CLAUDE_CODE_PROJECT_DIR_NAME", Value: claudeProjectDir},
		// Auto memory writes notes a later run reads back unhanded: the invisible
		// second memory the engine's calls are kept from (docs/concepts/llm-backends.md,
		// "Between turns").
		{Name: "CLAUDE_CODE_DISABLE_AUTO_MEMORY", Value: "1"},
		// The model's window ([claudeCodeWindow]), compacted at less the
		// harness's headroom; a subagent's `autoCompactWindow` can only LOWER it
		// (2.1.312+), so no request carries more. The switches that move it are
		// unset ([claudeCompactionOff]) and pinned in the settings.
		{Name: "CLAUDE_CODE_AUTO_COMPACT_WINDOW", Value: strconv.Itoa(window.Tokens)},
		{Name: "DISABLE_AUTOUPDATER", Value: "1"},
	}
}

// claudeRetentionDays is the `cleanupPeriodDays` every Claude Code run is
// started with ([claudeSettings]): how old a file under the box's configuration
// directory must be before the harness's retention sweep deletes it.
// THE HARNESS MUST NOT SWEEP WHAT THE ENGINE CARRIES. Every harness process
// sweeps that directory about five seconds in, by each file's own mtime. A carried
// session keeps its packed mtimes ([claudeSessionGate] extracts without `-m`) and
// is as old as its conversation.
//
// 36,500 DAYS, because nothing finite bounds a session's age and the harness
// takes no "never": 0 is refused from 2.1.89 (the file is then ignored silently).
// The cutoff, in 1926, is inside a JavaScript Date's range; past it an invalid
// Date keeps nothing (guarded from 2.1.310).
const claudeRetentionDays = 36500

// claudeSettings is the settings file every Claude Code run is started with
// (`--settings`, [Paths.ClaudeSettings]): the retention sweep held off
// ([claudeRetentionDays]) and, in its pinned copy, the settings pin
// ([claudeSettingsPin]). A run that starts the notes' hooks gets these settings
// in [claudeNotesSettings]'s place.
//
// EVERY RELEASE THAT RUNS THIS COMMAND READS IT: the field is in the schema of
// 0.2.116 and `--settings` is from 1.0.61, which refuses the
// `--append-system-prompt-file` every run is handed ([finalSystem]); the value is
// a whole number at least 0 (1.0.x) or 1 (2.1.112+). A settings file the harness
// cannot validate is ignored silently, so it holds no other field; the pinned
// copy goes only to a release that takes it ([claudeSettingsPinFloor]).
type claudeSettings struct {
	CleanupPeriodDays int `json:"cleanupPeriodDays"`

	// Env is the settings pin ([claudeSettingsPin]) in a file's pinned copy,
	// and nil, so left out, in the file itself.
	Env map[string]string `json:"env,omitempty"`
}

// newClaudeSettings is every Claude Code run's settings, with env as its `env`
// block: the pin in a pinned copy, nil otherwise.
func newClaudeSettings(env map[string]string) claudeSettings {
	return claudeSettings{CleanupPeriodDays: claudeRetentionDays, Env: env}
}

// claudeSettingsPin is the `env` block of every pinned settings file
// ([claudeSettingsPinFloor]): CLAUDE_CODE_AUTO_COMPACT_WINDOW at the window
// [claudeSessionLayout] pins, the two compaction overrides and every resume
// switch ([claudeResumeSwitches]) blank, and DISABLE_AUTOUPDATER 1.
//
// THE WRAPPER'S AND SCRIPT'S PINS AGAIN: the harness assigns every settings
// source's `env` after it starts, a later source winning (2.1.310, 2.1.322), so a
// repository's `.claude/settings.json` would set them again; the run's
// `--settings` comes after it. BLANK, NOT LEFT OUT: a missing key leaves the
// repository's value, and a blank reads as unset. NOT pinned: a managed settings
// file, and DISABLE_AUTO_COMPACT and DISABLE_COMPACT, left to the repository.
func claudeSettingsPin(window sandbox.ExecutorWindow) map[string]string {
	pin := map[string]string{
		"CLAUDE_CODE_AUTO_COMPACT_WINDOW":     strconv.Itoa(window.Tokens),
		"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":     "",
		"CLAUDE_CODE_BLOCKING_LIMIT_OVERRIDE": "",
		"DISABLE_AUTOUPDATER":                 "1",
	}
	for _, name := range claudeResumeSwitches {
		pin[name] = ""
	}
	return pin
}

// claudeArchiveFormat is the layout a Claude Code run packs its session in
// ([claudeCapture]), and the one it restores ([claudeSessionGate]): one
// gzipped tar whose members are named relative to the harness's configuration
// directory — `projects/crewlet/<session>.jsonl`, `projects/crewlet/<session>/`
// and `tasks/<session>/`. A change to what is packed, or where it is restored
// to, is a new format, which a box of this build refuses rather than reads.
const claudeArchiveFormat = 1

// ArchiveFormat is the layout a Claude Code run packs its session in
// ([carrier]).
func (ClaudeCode) ArchiveFormat() int { return claudeArchiveFormat }

// VersionCommand prints a box's Claude Code release ([carrier]).
func (ClaudeCode) VersionCommand() string { return "claude --version" }

// Pack packs session now, as a run's script packs it once the harness exits
// ([carrier], [claudeCapture]): the same members, into staged, over dest (the
// pack's own place, held for its read ([Paths.SessionPackHeld]), never the job's
// capture), its digest kept; [packNothing] where the harness wrote no
// transcript, [packFailed] where no archive was made ([Runner.PackCapture]).
//
// HELD TO THE RULE OF A PACK BESIDE A WORKING HARNESS ([claudeHeldTar]) where
// working says the job may still be writing as tar reads, and to the run's own
// end's where the job is over ([sandbox.SessionTake.Pack]): no harness is left
// to write, and the working rule would cost the wait for the box's clock and
// refuse a file an outliving child writes as it packs.
func (ClaudeCode) Pack(paths Paths, session, staged, dest string, working bool) string {
	return claudeOnDemand(paths, session, packAt{staged: staged, dest: dest, working: working})
}

// PackSave packs a save of session now, beside a harness that may still be
// working ([saver]): the same members as every pack, held to the rule of a
// pack beside a working harness ([claudeHeldTar]), into staged, this save's
// own staging directory, and over dest, this save's own place — never the
// job's capture, which every read of a job that is over takes as its final
// archive — its digest kept, ending as [ClaudeCode.Pack] does
// ([Runner.PackSave]). The record refuses no Claude Code save, so it never
// errs.
func (ClaudeCode) PackSave(paths Paths, rec sessionRecord, staged, dest string) (string, error) {
	return claudeOnDemand(paths, rec.id, packAt{staged: staged, dest: dest, working: true}), nil
}

// claudeOnDemand is a pack the engine makes of session, outside the run's own
// script, at at: [packNothing] where the box holds no transcript of it,
// [packFailed] where no archive was made.
func claudeOnDemand(paths Paths, session string, at packAt) string {
	transcript := paths.ClaudeConfigDir() + "/projects/" + claudeProjectDir + "/" + session + ".jsonl"
	return "s=" + shellword.Quote(session) + " packed=\n" +
		fmt.Sprintf("[ -s %s ] || exit %d\n", shellword.Quote(transcript), packNothing) +
		claudePack(paths, at) +
		fmt.Sprintf("[ -n \"$packed\" ] || exit %d\n", packFailed)
}

// layout is a Claude Code archive's: a gzipped tar of the session's files
// ([claudeArchiveFormat]).
func (ClaudeCode) layout() archiveLayout { return archiveTar }

// charges reports whether member is session's main thread, whose lines after
// the counted reply the count charges ([carrier.charges], [meterArchive]).
func (ClaudeCode) charges(session, member string) bool {
	m, ok := claudeMemberOf(session, path.Clean(member))
	return ok && m.Kind == memberMainThread
}

// Continues reports whether a session written by Claude Code release writer
// can be continued in a box whose Claude Code is release box ([carrier]): the
// box's release read and at or after [claudeCarryFloor], the writer as
// [ClaudeCode.ContinuesWriter] holds it, and never a box older than the
// release that wrote the session, which reads only what it and the releases
// before it wrote.
func (c ClaudeCode) Continues(box, writer string) (bool, string) {
	switch {
	case !releaseReads(box):
		return false, boxNamedNoRelease("Claude Code")
	case !releaseAtLeast(box, claudeCarryFloor):
		return false, fmt.Sprintf("this box runs Claude Code %s, older than %s, the oldest that "+
			"continues a session carried from another box", box, claudeCarryFloor)
	}
	if ok, why := c.ContinuesWriter(writer); !ok {
		return false, why
	}
	if !releaseAtLeast(box, writer) {
		return false, carriedOlder("Claude Code", box, writer)
	}
	return true, ""
}

// ContinuesWriter reports whether a session Claude Code release writer wrote
// can be continued at all ([carrier]): the writer read and at or after
// [claudeCarriedWriterFloor]. A writer that does not read continues nowhere
// ([writerNamedNoRelease]).
func (ClaudeCode) ContinuesWriter(writer string) (bool, string) {
	if !releaseReads(writer) {
		return false, writerNamedNoRelease("Claude Code")
	}
	if !releaseAtLeast(writer, claudeCarriedWriterFloor) {
		return false, fmt.Sprintf("the session was written by Claude Code %s, older than %s, the "+
			"first whose compaction summaries survive a resume", writer, claudeCarriedWriterFloor)
	}
	return true, ""
}

// claudeSwitchesOff is every variable that starts Claude Code without the hooks
// and the bridge its run is given: bare mode (CLAUDE_CODE_SIMPLE) drops every
// hook, CLAUDE.md, subagents and a plan's login; safe mode (CLAUDE_CODE_SAFE_MODE)
// drops hooks and every --mcp-config server. Either can arrive from the seat's
// role.sandbox.env, the image or a profile the login shell sources, which only
// the wrapper, running after all three, reaches: so they are UNSET, not refused
// in config, on every run, since the bridge is every run's tools. A settings
// file's `disableAllHooks` is the box's own, never overruled: a run starting a
// hook it switches off is refused ([claudeHooksOff]). No --setting-sources (it drops CLAUDE.md) and no --include-hook-events.
var claudeSwitchesOff = []string{"CLAUDE_CODE_SIMPLE", "CLAUDE_CODE_SAFE_MODE"}

// claudeHostedOff is every variable that starts Claude Code in the mode of
// Anthropic's own hosted environment, which no box is: CLAUDE_CODE_REMOTE.
// Switched on with CLAUDE_CODE_ENVIRONMENT_KIND absent or `byoc`, it changes
// what a resume hands its model: the load can give a record another parent than
// the transcript holds (up to 16 times), so the run continues a conversation its
// session never recorded, and 2.1.322 can take another of the conversation's
// ends as the one it continues. No settings file can set it (both releases
// ignore it in `env`), so only the starting environment can.
//
// UNSET RATHER THAN REFUSED, as [claudeSwitchesOff] is, AND ON EVERY RUN, not
// only a resume: a relaunch continues the session its job ran, and a job
// started in the hosted mode would otherwise be continued outside it.
var claudeHostedOff = []string{"CLAUDE_CODE_REMOTE"}

// claudeCompactionOff is every variable that, set in a run's environment,
// switches its harness's compaction off or moves the point it compacts at:
//   - DISABLE_AUTO_COMPACT and DISABLE_COMPACT let the context grow past the
//     model's window;
//   - CLAUDE_AUTOCOMPACT_PCT_OVERRIDE and CLAUDE_CODE_BLOCKING_LIMIT_OVERRIDE move
//     the point away from the window [claudeSessionLayout] pins less the headroom.
//
// Unset rather than refused, as [claudeSwitchesOff] is. A repository's settings
// `env` can set them again, so the pin blanks the two that move the point
// ([claudeSettingsPin]) and leaves the other two to its `autoCompactEnabled: false`.
// CLAUDE_CODE_MAX_CONTEXT_TOKENS is not among them: it is a real window.
var claudeCompactionOff = []string{"DISABLE_AUTO_COMPACT", "DISABLE_COMPACT", "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE",
	"CLAUDE_CODE_BLOCKING_LIMIT_OVERRIDE"}

// claudeResumeSwitches is every variable that, set when a run resumes, changes
// the session its harness hands its model; the script unsets them on every
// resume ([claudeSessionGate]):
//   - CLAUDE_CODE_RESUME_INTERRUPTED_TURN reruns the turn the last process was
//     cut off in, repeating a call that may have run, which the brief says to check;
//   - CLAUDE_CODE_DISABLE_PRECOMPACT_SKIP has a transcript past 5 MiB read whole
//     and pruned by its own rule, which can hand the model different records.
//
// Unset rather than refused, as [claudeSwitchesOff] is, on a resume alone. A
// repository's settings `env` can set either again after the harness starts, so
// on a release that takes the pin the settings blank both ([claudeSettingsPin]).
var claudeResumeSwitches = []string{"CLAUDE_CODE_RESUME_INTERRUPTED_TURN", "CLAUDE_CODE_DISABLE_PRECOMPACT_SKIP"}

// Events is a new decoder for one stream, read whole or followed ([Decoder]).
func (ClaudeCode) Events() Decoder { return &claudeEvents{} }

// Parse maps the harness's result, the stream's last line, onto a result,
// decoded from the bytes as read ([wireAlias]), never a copy of them.
//
// TOLERANT BY DESIGN: non-JSON or partial output yields a FAILED result
// carrying an account of it, never an error, so a crashed harness surfaces as
// "did not deliver" and the turn continues with what the executor can report.
//
// A RESULT IS AN OBJECT OF TYPE `result`, and nothing else is. The result file
// holds the stream's LAST LINE, whatever it was: a run that died part-way leaves
// an assistant message or tool result there, which has no subtype and no
// is_error and, read as a result, was a run that SUCCEEDED and said nothing.
func (ClaudeCode) Parse(result []byte) sandbox.Result {
	text := bytes.TrimSpace(result)
	if len(text) == 0 {
		return sandbox.Result{Error: "the harness produced no output"}
	}
	msg, ok := decodeClaudeResult(text)
	if !ok {
		if last := lastLine(text); len(last) > 0 && last[0] == '{' {
			// A LINE THAT STARTS AN OBJECT AND IS NOT ONE is a message the
			// process stopped writing part-way, not an account of anything:
			// its fragment — of a file it read, as like as not — is no use to
			// a reader, and it is what the error stream beside it explains.
			return sandbox.Result{Error: fmt.Sprintf("the harness's output ends in a line "+
				"that is not a whole JSON object (%s), so it never reported how its run ended",
				humanSize(int64(len(last))))}
		}
		// Carried as the Error rather than as a report, because it is not
		// one: the coordinator condenses a failure keeping its cause.
		return sandbox.Result{Error: unparsedFailure(text)}
	}
	if msg.Type != "result" {
		what := "an object with no type"
		if msg.Type != "" {
			what = fmt.Sprintf("a %q message", msg.Type)
		}
		return sandbox.Result{Error: "the harness stopped before it reported how its run ended: " +
			"the last thing it printed was " + what + ", not its result"}
	}

	// TRIMMED, as a report is, and so the very string a failure's why is
	// ([claudeResult.why]), which a collection redacts once for both.
	said := strings.TrimSpace(msg.Result)
	res := sandbox.Result{
		Text:          said,
		Success:       msg.succeeded(),
		SessionID:     msg.SessionID,
		CostUSD:       msg.TotalCostUSD,
		DeliveredRefs: deliveredRefs(said),
		// The run's own model turns, as its loop counted them: the
		// provider calls the run made, which the engine never saw.
		Turns: msg.NumTurns,
	}
	if u := msg.Usage; u != nil {
		// INPUT TOKENS ARE A SUM: the vendor's input_tokens is only the
		// UNCACHED remainder, and a run is almost entirely cached
		// rounds — so reading it alone put a fraction of every run's
		// prompt on the budget counter and the spend rollup.
		res.CacheReadTokens = u.CacheRead
		res.CacheWriteTokens = u.CacheCreate
		res.InputTokens = u.Input + res.CacheReadTokens + res.CacheWriteTokens
		res.OutputTokens = u.Output
	}
	if !res.Success {
		res.Error = msg.failure()
		res.ContextExhausted = msg.contextExhausted()
		res.RequestRefused = msg.requestRefused()
		res.RoundCapped = msg.Subtype == claudeMaxTurns
		// THE LINE ITSELF, for the reader that classifies how the run
		// ended by the vendor's status rather than its words: a spent plan
		// is `is_error` with `api_error_status` 429, whatever the sentence
		// in `result` says this release ([sandbox.Result.Event]). Only one
		// the record carries: a longer one is dropped where the box is read
		// ([sandbox.MaxRunEventBytes]), and copied here it would hold the line again.
		if len(text) <= sandbox.MaxRunEventBytes {
			res.Event = string(text)
		}
	}
	return res
}

// unparsedShownBytes is the most of an unparseable output a failure quotes:
// half of what a runner's failure may be ([sandbox.MaxFailureBytes]), the
// error stream's end, which says the cause as the output's end does, given the
// rest ([failureDetail]).
const unparsedShownBytes = sandbox.MaxFailureBytes / 2

// unparsedFailure is the failure of output that is no result: the output whole
// within [unparsedShownBytes], since the useful part (the error after the banner
// and the warnings) is at its END; past it, that end as [streamEnd] shows the
// error stream's, redacted before it is cut, and the output's size. Never more:
// a failure's head is never cut ([failureDetail]), so a whole output past the
// bound would ride the failure at up to [sandbox.MaxFileBytes].
func unparsedFailure(text []byte) string {
	const opening = "the harness's output could not be parsed"
	if len(text) <= unparsedShownBytes {
		return opening + ":\n" + string(text)
	}
	size := humanSize(int64(len(text)))
	window := string(text[len(text)-unparsedShownBytes-redact.LookBack:])
	if shown, _ := settledEnd(window, redact.LookBack, unparsedShownBytes); shown != "" {
		return opening + " (" + size + "; its end shown):\n" + shown
	}
	return fmt.Sprintf("%s: it is %s, and its last %s are one line begun before them, so none of it is shown",
		opening, size, humanSize(unparsedShownBytes))
}

// claudeMaxTurns is the result subtype of a run that reached its --max-turns.
const claudeMaxTurns = "error_max_turns"

// claudeResult is the harness's result message, the stream's last line and the
// one object `--output-format json` printed, as the vendor's Agent SDK
// documents it: `subtype` names HOW the run ended, `is_error` whether it
// failed, `result` the final text, `errors` what went wrong on a subtype that
// is not success (which carries no `result`).
//
// Named fields, so the stream's decoder and [ClaudeCode.Parse] read one shape
// off one decode ([claudeResultWire]), keys exactly as written and TOLERANTLY:
// a field of an unexpected type is left zero and the rest still read, so a
// reshaped field costs that field, not the run's whole account.
type claudeResult struct {
	Type    string
	Subtype string
	IsError bool
	Result  string
	// Errors is the list of what went wrong, raw: read an error at a time
	// ([claudeResult.errorsSaid]), never decoded whole, and only where a
	// result that did not succeed is read, since every line of the stream is
	// decoded for a result's keys.
	Errors       json.RawMessage
	SessionID    string
	TotalCostUSD float64
	// NumTurns is how many turns the run's agent loop took — one model
	// call each — on every subtype, a run stopped by its turn cap included.
	NumTurns int
	// TerminalReason is why the run's loop ended, beside the subtype:
	// "completed" for a loop that finished, and [claudeHookStopped] for one a
	// hook stopped. Absent on a harness that does not report it.
	TerminalReason string
	// APIErrorStatus is the HTTP status the provider answered the run's last
	// request with, where that request failed; zero otherwise.
	APIErrorStatus int
	// Usage is the session's tokens, in the keys a message's usage has
	// ([claudeUsage]); nil where the result carries none, or none that is an
	// object.
	Usage *claudeUsage
	// SinceLastResult is, per model, what the calls THIS PROCESS counted since
	// it wrote its previous result (since it started, for its first); absent
	// before 2.1.310. The SDK schema says to sum it over a conversation's results
	// rather than subtract totals: nothing is carried in from another process,
	// so the sum is the run's own spend whether or not it continued a session
	// ([claudeEvents.runSpend]). `usage` and `total_cost_usd` beside it are the
	// session's ([ClaudeCode.TotalsRestored]).
	//
	// RAW, read only by [claudeResult.sinceLastResult] on a result line of the
	// stream, never by [ClaudeCode.Parse] nor on a line of another type.
	SinceLastResult json.RawMessage
}

// claudeResultWire is the result message as its one decode leaves it
// ([wireAlias]): each key a [claudeResult] holds, by its exact name and raw,
// read into the result by [claudeResultWire.result].
type claudeResultWire struct {
	Type            json.RawMessage `json:"type"`
	Subtype         json.RawMessage `json:"subtype"`
	IsError         json.RawMessage `json:"is_error"`
	Result          json.RawMessage `json:"result"`
	Errors          json.RawMessage `json:"errors"`
	SessionID       json.RawMessage `json:"session_id"`
	TotalCostUSD    json.RawMessage `json:"total_cost_usd"`
	NumTurns        json.RawMessage `json:"num_turns"`
	TerminalReason  json.RawMessage `json:"terminal_reason"`
	APIErrorStatus  json.RawMessage `json:"api_error_status"`
	Usage           json.RawMessage `json:"usage"`
	SinceLastResult json.RawMessage `json:"model_usage_since_last_result"`
}

// result is the result w holds, DECODED LOOSELY, as the whole decode it
// replaced read one: a key of another kind than its field's reads as that
// field's zero value, a usage that is no object as none, and the rest of the
// result as written.
func (w claudeResultWire) result() claudeResult {
	r := claudeResult{
		Type: looseString(w.Type), Subtype: looseString(w.Subtype), IsError: looseTrue(w.IsError),
		Result: looseString(w.Result), Errors: w.Errors, SessionID: looseString(w.SessionID),
		TotalCostUSD: looseFloat(w.TotalCostUSD), NumTurns: int(looseInt(w.NumTurns)),
		TerminalReason: looseString(w.TerminalReason), SinceLastResult: w.SinceLastResult,
		APIErrorStatus: int(looseInt(w.APIErrorStatus)),
	}
	if wireObject(w.Usage) {
		usage := looseUsage(w.Usage)
		r.Usage = &usage
	}
	return r
}

// claudeResultModelsMax is the most models a result's spend model by model is
// read for ([claudeResult.sinceLastResult]); a result naming more is read for
// none of them.
//
// SIXTY-FOUR. A result names the models its process called, and every result
// measured that states its spend names one model or none (117, of 2.1.310 and
// 2.1.322, each naming claude-sonnet-4-5 where it names any): a process calls
// a handful at most, its own model, the one it falls back to, the small one
// its helpers run on and the one a subagent runs on (each a setting 2.1.322's
// shipped code reads). Sixty-four leaves a later release and a repository's
// own agents room many times over, and holds what a result costs its reader
// to that many models' spends, however many the line names.
const claudeResultModelsMax = 64

// errResultModels is a result whose spend model by model names more models
// than [claudeResultModelsMax]: read for none of them.
var errResultModels = fmt.Errorf("executor: a result states its spend for more than %d models",
	claudeResultModelsMax)

// sinceLastResult is the result's per-run spend, model by model
// ([claudeResult.SinceLastResult]), and whether the result states it: present,
// even empty, IS STATED; an absent key states nothing (before 2.1.310). A model
// named twice is read by its last spend, as JSON.parse reads it.
//
// A value there and no object is unread (null included), never "states nothing":
// the harness writes an object or no key, so it would leave its calls out of the sum.
//
// The walk goes a model at a time ([eachKey]), keeping one copy of each model's
// last spend, at most [claudeResultModelsMax]. One past is errResultModels and NO
// spend is read, never the sum before it, which would charge some of the run's
// calls and not the rest ([claudeEvents.runSpend] then reads none stated).
func (r claudeResult) sinceLastResult() (map[string]claudeModelUsage, bool, error) {
	switch {
	case r.SinceLastResult == nil:
		return nil, false, nil
	case !wireObject(r.SinceLastResult):
		return nil, false, fmt.Errorf("executor: a result states its spend as %s, not an object",
			wireKind(r.SinceLastResult))
	}
	var (
		// place is each model met, by its name, its place in names and last.
		place = map[string]int{}
		names []string
		// last is the last spend written for each model met, its own copy:
		// the walk's bytes are valid only while it hands them over.
		last [][]byte
	)
	err := eachKey(r.SinceLastResult, func(name []byte, spend json.RawMessage, _ int) error {
		if i, met := place[string(name)]; met {
			last[i] = append(last[i][:0], spend...)
			return nil
		}
		if len(names) == claudeResultModelsMax {
			return errResultModels
		}
		model := string(name)
		place[model] = len(names)
		names = append(names, model)
		last = append(last, append([]byte(nil), spend...))
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	spent := make(map[string]claudeModelUsage, len(names))
	for i, model := range names {
		spent[model] = looseModelUsage(last[i])
	}
	return spent, true, nil
}

// claudeModelUsage is one model's spend as Claude Code's result states it per
// model, in camelCase as `modelUsage` and its per-run sibling print it
// ([claudeModelUsageWire]), and the context window the process held the model
// to.
type claudeModelUsage struct {
	InputTokens              int
	OutputTokens             int
	CacheReadInputTokens     int
	CacheCreationInputTokens int
	CostUSD                  float64
	// ContextWindow is the window the process computed for the model, the
	// one it compacts under (`contextWindow`, 2.1.310 and 2.1.322 alike): the
	// model's own as the release's catalog, an alias's retarget, an account's
	// default model and a switch that holds every model to 200,000 tokens
	// made it in that box. 0 where it is absent or no number. What a
	// continuation of the copy is judged under, where it is less than the
	// engine's own reading of the model's window
	// ([claudeEvents.reportedWindow], internal/sandbox's SendsWhole).
	ContextWindow int64
}

// claudeModelUsageWire is one model's spend as its decode leaves it, each key
// by its exact name and raw ([looseModelUsage]).
type claudeModelUsageWire struct {
	InputTokens              json.RawMessage `json:"inputTokens"`
	OutputTokens             json.RawMessage `json:"outputTokens"`
	CacheReadInputTokens     json.RawMessage `json:"cacheReadInputTokens"`
	CacheCreationInputTokens json.RawMessage `json:"cacheCreationInputTokens"`
	CostUSD                  json.RawMessage `json:"costUSD"`
	ContextWindow            json.RawMessage `json:"contextWindow"`
}

// looseModelUsage is the spend raw holds, DECODED LOOSELY, as the whole decode
// it replaced read one: a figure of another kind than its field's reads as
// none, the others as written, and a spend that is no object as none at all.
func looseModelUsage(raw json.RawMessage) claudeModelUsage {
	var w claudeModelUsageWire
	if !wireObject(raw) || wireDecode(raw, &w) != nil {
		return claudeModelUsage{}
	}
	return claudeModelUsage{
		InputTokens: int(looseInt(w.InputTokens)), OutputTokens: int(looseInt(w.OutputTokens)),
		CacheReadInputTokens:     int(looseInt(w.CacheReadInputTokens)),
		CacheCreationInputTokens: int(looseInt(w.CacheCreationInputTokens)), CostUSD: looseFloat(w.CostUSD),
		ContextWindow: looseInt(w.ContextWindow),
	}
}

// claudeTotalsRestored is the first Claude Code release whose continued
// session starts from the totals the job before it saved, so that its result
// states the session's spend: 2.1.277, whose changelog reads "Fixed a headless
// resume (claude -p --resume, …) starting the session's cost and usage totals
// at zero; headless sessions now save their totals at exit".
const claudeTotalsRestored = "2.1.277"

// TotalsRestored reports whether release version restores a continued
// session's totals ([sessionTotals]): from [claudeTotalsRestored]. A version
// that cannot be read is taken for a current release, as the notes gate takes
// it ([claudeNotesGate]): every release prints `MAJOR.MINOR.PATCH (Claude
// Code)`, so one that prints anything else is newer than this code.
func (ClaudeCode) TotalsRestored(version string) bool {
	return releaseAtLeast(version, claudeTotalsRestored)
}

// releaseAtLeast reports whether version, as a harness's `--version` printed it,
// is at or after floor. A version this code cannot read ([releaseNumbers]) is
// taken for a current release.
func releaseAtLeast(version, floor string) bool {
	have, ok := releaseNumbers(version)
	if !ok {
		return true
	}
	want, _ := releaseNumbers(floor)
	for i := range 3 {
		if have[i] != want[i] {
			return have[i] > want[i]
		}
	}
	return true
}

// releaseNumbers is the three numbers of the bare release version names, as
// a harness's `--version` printed it, read as [harnessprofile.Release] reads
// it, the one reading the runners, the sandbox and the doctor share: a
// pre-release, a fourth number (`1.4.25.1`) or anything but three runs of
// digits names none. False for a version that names none, or one of a number
// past what an int holds.
func releaseNumbers(version string) ([3]int, bool) {
	var out [3]int
	release := harnessprofile.Release(version)
	if release == "" {
		return out, false
	}
	for i, field := range strings.SplitN(release, ".", 3) {
		n, err := strconv.Atoi(field)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// claudeHookStopped is the terminal reason of a run whose loop a hook stopped.
const claudeHookStopped = "hook_stopped"

// claudeContextReasons are the terminal reasons of a run whose conversation no
// longer fits its model's context: the three the 2.1.319 bundle itself files
// under the cause "context_limit". A prompt refused as too long, a context
// past its blocking limit, and a loop whose automatic compaction thrashed (the
// context refilled to the limit right after each compaction, so the harness
// stopped it) all end the same way, and continuing the session repeats it.
var claudeContextReasons = []string{"prompt_too_long", "blocking_limit", "rapid_refill_breaker"}

// claudeContextSentences are that same ending as Claude Code words it, for a
// result that carries no terminal reason: the sentence the bundle prints for a
// refused prompt, the two API errors it files under the same cause, and the
// sentence it prints when automatic compaction thrashes.
var claudeContextSentences = []string{
	"Prompt is too long",
	"Input is too long for requested model",
	"exceed context limit",
	"Autocompact is thrashing",
}

// contextExhausted reports whether a failed run ended on its context, by the
// field first and its sentence where the release states no such field. Read on
// a failed result only: a run that succeeded and wrote about a long prompt
// ended on nothing.
func (r claudeResult) contextExhausted() bool {
	if r.TerminalReason != "" {
		return slices.Contains(claudeContextReasons, r.TerminalReason)
	}
	// The errors' text TRIMMED reads as their whole text does: a sentence is
	// found within a line, and none opens or closes with a blank.
	said := r.Result
	if errs := r.errorsSaid(); errs != "" {
		said += "\n" + errs
	}
	return slices.ContainsFunc(claudeContextSentences, func(sentence string) bool {
		return strings.Contains(said, sentence)
	})
}

// claudeRefusedStatus reports whether status is one a provider answers a
// request it will not take as it was built: invalid (400), too large (413) or
// unprocessable (422). The same request is refused again, so no retry of it
// cures one.
func claudeRefusedStatus(status int) bool {
	return status == 400 || status == 413 || status == 422
}

// requestRefused is whether the run ended on a request its provider refused as
// built ([sandbox.Result.RequestRefused]): the harness's own failure flag with
// a status [claudeRefusedStatus] names, and not a conversation past its context,
// which is a 400 too and has its own word ([claudeResult.contextExhausted]).
// Measured on 2.1.322: a tool definition whose name breaks the provider's
// pattern ends `is_error` with `api_error_status` 400, its result the sentence.
func (r claudeResult) requestRefused() bool {
	return r.IsError && claudeRefusedStatus(r.APIErrorStatus) && !r.contextExhausted()
}

// succeeded is whether the run finished its work. EVERY FIELD MUST SAY SO: a
// run that hit its turn cap reports no is_error but did not finish, one whose
// model call failed is a success subtype carrying is_error, and one a hook
// stopped is a success subtype with no error and an empty result, its loop
// ended part-way ([claudeHookStopped]; measured on 2.1.263 and 2.1.310, a
// repository's PostToolBatch hook that exited 2). Read as it reports itself, a
// hook's stop was a run that succeeded and said nothing.
func (r claudeResult) succeeded() bool {
	return (r.Subtype == "" || r.Subtype == "success") && !r.IsError &&
		r.TerminalReason != claudeHookStopped
}

// failure says how a run that did not succeed ended: the subtype that names
// how, and whatever the message says about why — its errors, or the final
// text it got as far as — or, for a loop a hook stopped, that.
func (r claudeResult) failure() string {
	if r.TerminalReason == claudeHookStopped {
		stopped := "a hook in the run stopped its loop before it finished (Claude Code " +
			"terminal_reason hook_stopped): a hook of the repository's, the image's or the box " +
			"home's own settings exited 2 or returned a block; the run's own steering hooks " +
			"never block"
		if why := r.why(); why != "" {
			stopped += ": " + why
		}
		return stopped
	}
	how := r.Subtype
	if how == "success" {
		how = ""
	}
	why := r.why()
	switch {
	case how != "" && why != "":
		return how + ": " + why
	case how != "":
		return how
	case why != "":
		return why
	}
	return "it reported an error and said nothing about it"
}

// why is why the run did not succeed, as its result says, trimmed: what its
// errors say ([claudeResult.errorsSaid]), else its final text, else nothing.
func (r claudeResult) why() string {
	if errs := r.errorsSaid(); errs != "" {
		return errs
	}
	return strings.TrimSpace(r.Result)
}

// errorsSaid is what the result's errors say, a line apart, trimmed: each
// error that is a string as written and each of another kind as nothing, as
// the list decoded into strings and joined read once trimmed, and "" for
// errors that are no list.
//
// AN ERROR AT A TIME ([eachElement]), and never the whole list first: decoded
// whole, the list held a string's 16 bytes for every element it listed,
// however small the element, on every line of the stream, since every line is
// decoded into a result. And the line apart is owed only before an error that
// says something, so a run of errors that say nothing writes nothing at
// either end of the text, which trimming would drop, and a line each between
// two that say something, as their whole text did.
func (r claudeResult) errorsSaid() string {
	var (
		said  strings.Builder
		owed  int
		first = true
	)
	if eachElement(r.Errors, func(element json.RawMessage, _ int) error {
		if !first {
			owed++
		}
		first = false
		text := looseString(element)
		if text == "" {
			return nil
		}
		for ; owed > 0 && said.Len() > 0; owed-- {
			said.WriteByte('\n')
		}
		owed = 0
		said.WriteString(text)
		return nil
	}) != nil {
		return ""
	}
	return strings.TrimSpace(said.String())
}

// decodeClaudeResult reads the result message: the one line the result file
// holds, which the wrapper copied from the end of the stream.
func decodeClaudeResult(text []byte) (claudeResult, bool) {
	if len(text) == 0 || text[0] != '{' {
		return claudeResult{}, false
	}
	// A WHOLE OBJECT IS A RESULT'S ACCOUNT, whatever its keys hold: its decode
	// fails only where the line is no whole JSON object, and a key of another
	// kind than its field's is read loosely ([claudeResultWire.result]).
	var w claudeResultWire
	if wireAlias(text, &w) != nil {
		return claudeResult{}, false
	}
	return w.result(), true
}

// lastLine is the text's last line, trimmed.
func lastLine(text []byte) []byte {
	if i := bytes.LastIndexByte(text, '\n'); i >= 0 {
		text = text[i+1:]
	}
	return bytes.TrimSpace(text)
}

// threadTraits is what Claude Code's main-thread read settles: a cut-off copy
// is counted (its thread is the copy itself), its hooks' serves show in the
// thread, and a capture is raised only where its release's resume was read.
func (ClaudeCode) threadTraits() sandbox.ThreadTraits {
	return sandbox.ThreadTraits{Reads: true, CountsCutOff: true, Receipts: true, PlacesWithResumeRead: true,
		Summaries: true}
}

// sessionTraits is how a relaunch judges a Claude Code session: by the run's
// window, its calls keyed by their own ids, a brief at
// [claudeChargeBytes].
func (ClaudeCode) sessionTraits() sandbox.SessionTraits {
	return sandbox.SessionTraits{FirstRequest: sandbox.FirstRequestWindow, CallKeying: sandbox.CallKeyingByCallID,
		Charge: claudeChargeBytes}
}

// resumeRead reports whether release is one whose resume this build read
// ([claudeResumeRead]).
func (ClaudeCode) resumeRead(release string) bool { return claudeResumeRead(release) }

// artifactFiles is the files a Claude Code run leaves in its box of its own.
func (ClaudeCode) artifactFiles(p Paths) []string {
	return []string{p.ClaudeNotesSystem(), p.ClaudeSettings(), p.ClaudeNotesSettings(), p.ClaudeHooksOff(),
		p.ClaudeSettingsPinned(), p.ClaudeNotesSettingsPinned(), p.ClaudeNoProxy(),
		// Each one's copy with the NO_PROXY list written in ([claudeNoProxyMerge]).
		p.ClaudeSettings() + ".np", p.ClaudeNotesSettings() + ".np",
		p.ClaudeSettingsPinned() + ".np", p.ClaudeNotesSettingsPinned() + ".np"}
}

// artifactDirs is the directories a Claude Code run leaves in its box of its
// own.
func (ClaudeCode) artifactDirs(p Paths) []string { return []string{p.ClaudeSkillsPlugin()} }
