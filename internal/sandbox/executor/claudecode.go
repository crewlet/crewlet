package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// ClaudeCodeName is this runner's config name.
const ClaudeCodeName = sandbox.ClaudeCodeHarness

// ClaudeCode drives Claude Code headless.
//
// It reaches its model through the run ENVIRONMENT rather than a config file,
// which is why WriteConfig here renders only MCP servers, settings and hooks:
// the credential comes from providers.llm via the sandbox env, and duplicating
// it into a file inside the box would put the secret somewhere the agent can
// read it back.
//
// # Its own retention sweep, held off
//
// Every run is started with a settings file of its own ([claudeSettings],
// [Paths.ClaudeSettings]) that holds the harness's own retention sweep off
// ([claudeRetentionDays]), on every release and whatever else the run is
// handed: the box is ephemeral and the session carried out of it is the
// engine's to keep and to delete, so the box's harness deletes nothing it
// restores, however old. A run that starts a hook of the engine's — its
// skills' plugin's, or its notes' — is started with those settings and hooks
// switched on in that file's place ([claudeHooksOnSettings]), with the notes'
// hooks beside them where it starts those ([claudeNotesSettings]).
//
// # A person's notes, between its steps
//
// A run whose launch wired it to its turn's notes ([sandbox.RunRequest.Steer])
// is started with a settings file of its own ([Paths.ClaudeNotesSettings])
// holding every run's settings and two hooks, after each batch of the main
// agent's tool calls (PostToolBatch) and as it is about to finish (Stop), each
// a call of the bridge's unlisted `steer_take` tool over the run's own MCP
// connection; the bridge answers with the notes as the hook's additional
// context, or with nothing. Measured on
// 2.1.263 and 2.1.310 against a stub model API:
//
//   - a note handed back after a batch reaches the model after that batch's tool
//     results, and stays in the conversation for the rest of the run — WHERE
//     depends on the model: on 2.1.310 a system message for claude-opus-5-5 and
//     claude-sonnet-5, and a system reminder at the end of the batch's last
//     tool result for claude-sonnet-4-5, each headed "PostToolBatch hook
//     additional context:" ([ClaudeCodeSteerInstruction] says so, and tells the
//     model a note from what a tool produced by that alone);
//   - a note handed back at Stop continues the turn, and the run's result is the
//     answer the model gives after it;
//   - the hook's `timeout` is in seconds, and at it the harness cancels the call and
//     discards a late answer ([ClaudeHookTimeout]);
//   - a call the bridge answers with an error makes the harness re-initialise the
//     whole MCP server and tell the model its tools are still connecting — so
//     the bridge answers every take with text, never an error;
//   - the hook's `systemMessage` reaches only the run's own stream, as a
//     `system/informational` line the transcript shows ([claudeEvents]), never
//     a request.
//
// THE POSTURE IS THE SETTINGS FILE'S, and two settings beside it would undo it
// silently. `disableAllHooks` is written false ([claudeHooksOnSettings]), because a
// repository's own `true` switches every hook off with no error, and an
// explicit false wins over it. And the wrapper UNSETS CLAUDE_CODE_SIMPLE and
// CLAUDE_CODE_SAFE_MODE before the harness starts ([ClaudeCode.Output]): bare mode
// drops every hook (with CLAUDE.md, subagents and a plan's login), safe mode
// drops hooks and the --mcp-config bridge, and either can arrive from the seat's
// role.sandbox.env, the image, or a profile the login shell sources — of which
// only the wrapper, running after all three, reaches every one. Unset rather
// than refused in config for that reason, and for every run, not only one wired
// to notes: the bridge is every run's tools.
//
// NOTHING ELSE MOVES. No --setting-sources, which would drop the repository's
// CLAUDE.md along with its settings; no --include-hook-events, since the engine
// counts what the bridge hands over and a hook that stopped the loop is read
// off the result line.
//
// A RELEASE OLDER THAN 2.1.163 IS STARTED WITHOUT THE HOOKS ([claudeNotesFloor]),
// on the settings every run has: it runs them, and drops what a Stop hook
// answers, so a note taken as the run finishes would be recorded read by a run
// that never saw it. What switches the hooks off otherwise — a managed policy
// that allows only managed hooks (allowManagedHooksOnly,
// strictPluginOnlyCustomization) or disables every hook (disableAllHooks) —
// leaves, like that older release, a run that succeeded without asking once,
// which the collection says (internal/sandbox, steer_reader_never_ran).
//
// # Its session, where another box can take it up
//
// Every run in a box PINS where the harness keeps its sessions
// ([claudeSessionLayout]) — the box home's `.claude`, one project directory
// whatever the box's working directory — with auto memory off; NAMES the
// session it runs in before the harness starts, starting one under a minted id or
// resuming one, never both ([claudeSessionGate]); RECORDS it
// ([Paths.SessionRecord]); and once the harness has exited, PACKS it
// ([claudeCapture]): the transcript, its sidecars and its task list, never the
// login or the harness's own state. A run handed a session packed in another box
// restores it before the harness resumes it, on a release that continues one
// ([claudeCarryFloor]); every resumed run has its transcript and every
// subagent's thread cut back to its last whole line ([claudeSessionGate]), and
// renders its system prompt afresh ([claudeSnapshotFloor]), so a later
// launch's standing instructions are the ones it reads. The engine packs the
// session beside a harness that may still be working by the same pack, held to
// a stricter rule ([claudeHeldTar]) that first sets every file of the session
// stamped in the box's future to the box's present: a job its box would not
// end ([ClaudeCode.Pack]), for the read that packs it alone, and a save, to its
// taker's own path ([ClaudeCode.PackSave]).
//
// # Its end, on request
//
// A run's script records its pid before anything else, once it traps SIGUSR1
// (end.go), and runs the harness in the background, waited for: the request
// ends the harness — SIGTERM, then SIGKILL past [sandbox.JobStopGrace] — and the
// script packs the session as its own exit does ([claudeCapture]), records that
// it was ended ([Paths.JobEnded]) and exits with the harness's status. A
// SIGTERM to the job's group is still a teardown's, which the script does not
// trap: it goes with its harness, as it always did.
type ClaudeCode struct{}

var (
	_ Harness = ClaudeCode{}
	_ saver   = ClaudeCode{}
)

// NewClaudeCode returns the runner.
func NewClaudeCode() *Runner { return New(ClaudeCode{}) }

// Name is the harness's key in the runner registry.
func (ClaudeCode) Name() string { return ClaudeCodeName }

// Resumes says Claude Code continues a session it reported (`--resume`).
func (ClaudeCode) Resumes() bool { return true }

// HoldsToItsMCPServers says a Claude Code run can be held to the MCP servers
// it is handed (`--strict-mcp-config`), which the assistant's deep work asks.
func (ClaudeCode) HoldsToItsMCPServers() bool { return true }

// NoteInstruction is the standing instruction about notes a Claude Code run is
// given: it takes a person's notes between its steps, through the hooks its
// settings file holds — see [ClaudeCode].
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

// Command builds the headless `claude -p` invocation: ONE SCRIPT, so the
// wrapper's redirections — the brief on stdin, the stream, the error file —
// reach every step of it alike. It opens with the end protocol (end.go): its
// pid recorded once it traps the request that ends its job. It reads the box's
// `claude --version` once ([claudeRelease]) and decides by it what the harness
// is started with; names the session the harness runs in and records it
// ([Paths.SessionRecord]); runs the harness, in the background and waited for,
// so a request can end it ([launchHarness]); and once the harness has exited —
// by itself or ended — packs its session to be carried to another box
// ([claudeCapture]), exiting with the harness's own status.
//
// Pure and deterministic given its session, so the exact flags can be pinned by
// a test. The turn cap is appended only when set, so a minimal call stays
// minimal.
//
// THE BRIEF IS NOT ON IT: `-p` with no prompt reads the prompt on stdin, which
// the wrapper points at the brief's file ([ClaudeCode.Output]) — verbatim,
// measured on 2.1.263 and 2.1.310. A brief carries every note the turn has
// read, so it grows; one argument is capped at 128 KiB (MAX_ARG_STRLEN); and a
// command line is readable by every account on a `direct` box's host, the
// reason [Paths.System] gives for the system text's file.
//
// It runs with permissions bypassed, in every cell, and that is a deliberate
// consequence of how the run is driven rather than an oversight: a headless
// run has nobody to answer a permission prompt, so a gate left to ask would
// hang the run or refuse the step, and there is no per-tool allowlist inside
// it. What the agent may reach is its box's CELL's to bound — `container` and
// `e2b` keep it off the engine host, while `direct` runs it on the engine host
// as the engine's own user — beside the credentials the run env carries; the
// MCP servers it starts are the ones the engine hands it and, for a seat's
// executor, whatever its own configuration declares, as for a person running
// it ([sandbox.RunRequest.OnlyMCPServers]).
//
// STREAM-JSON, AND --verbose WITH IT. `--output-format json` prints ONE object
// when the run ends and nothing before it, and in print mode this harness writes
// nothing to stderr either — so a run on Claude Code showed a person nothing
// while it worked and left no transcript when it was done. stream-json prints
// every message as it happens, one JSON object a line, ending — as the vendor
// documents the stream — in the result message, the same object json printed
// alone. In print mode the harness refuses stream-json without --verbose ("When
// using --print, --output-format=stream-json requires --verbose").
//
// THE SYSTEM TEXT IS APPENDED, NOT SUBSTITUTED: `--append-system-prompt-file`
// adds the run's standing instructions after Claude Code's own system prompt,
// which stays whole (measured on 2.1.310, off the request the harness sent). That
// prompt is the harness this run exists to use — how to drive its shell and
// editor and work through a task — which a run on the engine host, denying
// every one of those tools, has no use for. The FILE variant, written by [ClaudeCode.WriteConfig]:
// see [Paths.System] for why never argv. Passed only for a run that has system
// text, so a run with none runs on the vendor's prompt alone.
//
// THE SESSION IS NAMED ON EVERY RUN ([claudeSessionGate]). A run that starts a
// session is started under the id minted for it (`--session-id`), so its
// record names its session from the start — a run killed before its result
// line used to name none. A run that continues one is started with `--resume`
// and never with both: the harness refuses `--session-id` beside `--resume` unless
// the session is forked ("session-id can only be used with --continue or
// --resume if --fork-session is also specified", 2.1.316). And a resumed run
// RENDERS ITS SYSTEM PROMPT AFRESH (`--system-prompt-snapshot off`, from
// [claudeSnapshotFloor]): by default a conversation records the system prompt
// its first request rendered — the first launch's appended text, its prefetch
// blocks and its box's findings path among it — and every later request and
// resume sends that record as it is until the conversation is compacted,
// whatever a later launch appends; so every resumed run, the same box's and
// the assistant's deep work's included, is told what THIS launch's standing
// instructions say.
func (ClaudeCode) Command(req sandbox.RunRequest, session Session, paths Paths, configPath string) string {
	return "sh -c " + sandbox.ShellQuote(claudeScript(req, session, paths, configPath, sandbox.JobStopGrace))
}

// claudeScript is a Claude Code run's script — see [ClaudeCode.Command] —
// giving its harness stop to go once it is asked to end: [sandbox.JobStopGrace],
// or a case's shorter one.
func claudeScript(req sandbox.RunRequest, session Session, paths Paths, configPath string,
	stop time.Duration) string {
	parts := []string{
		"claude", "-p",
		"--output-format", "stream-json", "--verbose",
		"--permission-mode", "bypassPermissions",
		// WHAT THE SCRIPT DECIDED BY THE HARNESS'S RELEASE, handed in as the
		// script's own arguments: the run's settings, with its hooks and its
		// system text told of notes where its launch wired it to them, and its
		// session.
		`"$@"`,
	}
	if req.System != "" && !req.Steer.Set() {
		parts = append(parts, "--append-system-prompt-file", sandbox.ShellQuote(paths.System()))
	}
	// VERBATIM, because the only entry that reaches this runner is a
	// claude-code one, which already names the model in Claude Code's own
	// terms — its alias or id exactly as its runs on the engine host pass
	// it. A launch passes over every other harness's entry, so none reaches here.
	if req.Model != "" {
		parts = append(parts, "--model", sandbox.ShellQuote(req.Model))
	}
	if req.Limits.MaxTurns > 0 {
		parts = append(parts, "--max-turns", strconv.Itoa(req.Limits.MaxTurns))
	}
	if configPath != "" {
		parts = append(parts, "--mcp-config", sandbox.ShellQuote(configPath))
	}
	// THE SERVERS IT IS HANDED ALONE only for a run held to them
	// ([sandbox.RunRequest.OnlyMCPServers]): the assistant's deep work, with
	// or without a file of servers. A seat's executor starts what Claude Code
	// starts for a person beside the engine's servers: the box's own user- and
	// local-scope servers, the repository's .mcp.json — which a print-mode run
	// connects without the approval it has nobody to ask for (the vendor's MCP
	// page, "Project scope") — a plugin's, and the signed-in account's claude.ai
	// connectors. It used to run strict everywhere, which kept a repository's
	// own tool servers and the box's from every seat.
	if req.OnlyMCPServers {
		parts = append(parts, "--strict-mcp-config")
	}

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
	// hold the harness's own retention sweep off ([claudeSettings]), unless a
	// gate below starts a hook of the engine's, in which case the file that
	// holds the same settings with hooks switched on ([claudeHooksOnSettings])
	// — and the notes' hooks besides, where they are the hooks it starts.
	line(`set --; cs=%s`, sandbox.ShellQuote(paths.ClaudeSettings()))
	// THE RUN'S OWN SKILLS, and the hook that reports each load, in the
	// plugin [ClaudeCode.WriteConfig] wrote — on a release that runs the hook
	// ([claudeSkillsGate]), which names the settings that keep the hook on.
	if len(req.Skills) > 0 {
		b.WriteString(claudeSkillsGate(paths.ClaudeSkillsPlugin(), paths.ClaudeSkillsSettings(),
			anyRequired(req.Skills)))
	}
	// THE RUN'S OWN HOOKS, which hand it a person's notes between its steps —
	// only where its launch wired it to them ([ClaudeCode.WriteConfig]) and
	// its release reads what they answer, in the settings that hold them —
	// AND ITS SYSTEM TEXT, told of notes only where they are read: the gate
	// decides both. After the skills' gate, since the notes' settings switch
	// hooks on as well.
	if req.Steer.Set() {
		plain := ""
		if req.System != "" {
			plain = paths.System()
		}
		b.WriteString(claudeNotesGate(paths.ClaudeNotesSettings(), paths.ClaudeNotesSystem(), plain))
	}
	// THE COMPACTION POINT AND A RESUME'S LOAD PINNED IN THE SETTINGS AS WELL,
	// on a release whose settings take the pin ([claudeSettingsPinFloor]): the
	// pinned copy of whichever file the gates above chose ([claudePinGate]), so
	// a repository's own settings can neither move the point the wrapper pinned
	// nor switch on again what the script unsets for a resume. After every gate
	// that chooses cs.
	b.WriteString(claudePinGate(paths, len(req.Skills) > 0, req.Steer.Set()))
	line(`set -- "$@" --settings "$cs"`)
	b.WriteString(claudeSessionGate(session, paths, req.FreshWiring != nil && configPath != ""))
	// THE RECORD, before the harness starts, so a run killed before it reported
	// anything still says which session it took up, and why it began again
	// where it did. The release last, as the harness printed it
	// ([parseSessionRecord]).
	line(`printf 'id %%s\ncontinued %%s\nwhy %%s\nrelease %%s\n' "$s" "$k" "$w" "$v" > %s`,
		sandbox.ShellQuote(paths.SessionRecord()))
	b.WriteString(launchHarness(strings.Join(parts, " "), paths))
	b.WriteString(claudeCapture(paths))
	line(`exit "$code"`)
	return b.String()
}

// claudeRelease opens every Claude Code run's script: it reads the box's
// `claude --version` once, as v, its numbers as n, and defines `at MAJOR MINOR
// PATCH`, which answers whether that release is at or after the one given —
// what every gate the script makes asks ([claudeNotesGate],
// [claudeSessionGate], the transcript's trim). Asked of the harness the box's PATH
// finds, in the box, because that is the one that runs.
//
// A VERSION IT CANNOT READ IS TAKEN FOR A CURRENT RELEASE: every release prints
// `MAJOR.MINOR.PATCH (Claude Code)`, so one that prints anything else is newer
// than this script, and a harness that is not there fails at the run itself.
//
// POSIX parameter expansion, case and test alone, which every box's sh has
// (dash, bash, busybox ash): no awk, no sort -V. The version is cut at its first
// character that is neither a digit nor a dot, so what is left is digits and
// dots, and it is read as a release only where it has three parts, each
// beginning with a digit — anything else reads as a current release, as
// [releaseAtLeast] reads it on the engine's side. (Its parts were checked once
// cut: a two-part `2.1` cut into 2, 1 and the 1 again, an old release.)
const claudeRelease = `v=$(claude --version < /dev/null 2> /dev/null); n=${v%%[!0-9.]*}
a=${n%%.*}; r=${n#*.}; b=${r%%.*}; c=${r#*.}; c=${c%%.*}
case "$n" in [0-9]*.[0-9]*.[0-9]*) ;; *) a= ;; esac
at() { [ -z "$a" ] || [ "$a" -gt "$1" ] || { [ "$a" -eq "$1" ] && { [ "$b" -gt "$2" ] || { [ "$b" -eq "$2" ] && [ "$c" -ge "$3" ]; }; }; }; }
`

// claudeNotesFloor is the oldest Claude Code release a run is started with its
// notes hooks on: 2.1.163, the first whose Stop hook's additional context
// reaches the model (its changelog: "Stop and SubagentStop hooks can now return
// hookSpecificOutput.additionalContext").
//
// An older release that has mcp_tool hooks (2.1.118 onwards) runs both hooks,
// and reads what the PostToolBatch one answers, but drops what the Stop one does
// — measured on 2.1.150: the note was taken at Stop, so recorded read and
// published delivered, and no request carried it, the run's answer the one from
// before the note. Started without the hooks, that release takes nothing at a
// boundary it drops: the notes wait for the review, the run is not recorded as
// having asked ([sandbox.RunSteer.Pulled]), and its collection says so
// (steer_reader_never_ran) — which tells an operator the box's harness needs an
// upgrade, where taking the notes would have told them nothing at all.
const claudeNotesFloor = "2.1.163"

// claudeSnapshotFloor is the oldest Claude Code release a resumed run is
// started with `--system-prompt-snapshot off` on: 2.1.267, which added the
// option ("Added `--system-prompt-snapshot off` to render the system prompt
// fresh on every request instead of reusing the conversation's recorded
// prompt"). 2.1.263, a release boxes were measured on, refuses it as an
// unknown option, which would fail every resumed run there.
const claudeSnapshotFloor = "2.1.267"

// claudeLocalGCFloor is the oldest Claude Code release a run is started with
// CLAUDE_CODE_TRANSCRIPT_LOCAL_GC on: 2.1.304, the release that fixed it
// "dropping messages from the saved transcript when the disk filled up while
// the file was being trimmed" (and 2.1.296 one "when its text held a Unicode
// line or paragraph separator"). On, a `-p` run trims its transcript once it
// is 5 MiB or more and holds a compaction, after each compaction and once 20
// MiB has been appended since the last trim (a backstop that doubles, up to
// 160 MiB, after a trim that freed little): it keeps the history from the last
// compaction on — which is all a resume restores, trimmed or not — writes it
// to a new file and RENAMES that over the transcript (read in 2.1.310's
// shipped code), so a session carried between boxes stays near that size
// rather than growing with every compaction it outlives, and a pack whose tar
// a trim overlaps sees the transcript's inode change ([claudeHeldTar]). On an
// older release it is UNSET, whatever set it: a trim that loses messages loses
// them from the session a later run continues.
const claudeLocalGCFloor = "2.1.304"

// claudeCarryFloor is the oldest Claude Code release a box continues a session
// carried from ANOTHER box on: 2.1.310, the first whose results state the
// run's own spend per model (`model_usage_since_last_result`), which a
// continued run is charged from, whatever its session's totals say
// ([claudeResult.SinceLastResult]) — and above every release a carried
// session's continuation needs: 2.1.304, whose transcript trim loses nothing
// ([claudeLocalGCFloor]); 2.1.299, which no longer runs an approved tool call
// twice on a resume after a restart; 2.1.288, which no longer drops files a
// compaction restored; 2.1.269, whose resumed headless sessions keep a turn's
// replies across a model switch; 2.1.267, which renders a resumed system prompt
// afresh ([claudeSnapshotFloor]); 2.1.265, which keeps a tool call the previous
// process died in; and 2.1.234, which names the project directory a transcript
// is filed under whatever the box's working directory ([claudeProjectDir]).
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
// [Paths.ClaudeSkillsSettingsPinned], [Paths.ClaudeNotesSettingsPinned]), and on
// an older release the file as chosen. Each file the run's gates can choose has
// its arm: every run's, the skills' for a run handed skills, and the notes' for
// a run wired to its notes. Those are the files [ClaudeCode.WriteConfig] writes
// with their pinned copies, and no other file is named.
func claudePinGate(paths Paths, skills, notes bool) string {
	files := [][2]string{{paths.ClaudeSettings(), paths.ClaudeSettingsPinned()}}
	if skills {
		files = append(files, [2]string{paths.ClaudeSkillsSettings(), paths.ClaudeSkillsSettingsPinned()})
	}
	if notes {
		files = append(files, [2]string{paths.ClaudeNotesSettings(), paths.ClaudeNotesSettingsPinned()})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "if at %s; then case $cs in\n", floorArgs(claudeSettingsPinFloor))
	for _, file := range files {
		fmt.Fprintf(&b, "  %s) cs=%s ;;\n", sandbox.ShellQuote(file[0]), sandbox.ShellQuote(file[1]))
	}
	b.WriteString("esac; fi\n")
	return b.String()
}

// claudeNotesGate is the part of a run's script wired to its notes that
// decides, by the harness's release ([claudeRelease]), its settings (cs) and
// its system text: on a release at or after [claudeNotesFloor] the settings
// with the notes' hooks (hooked, [Paths.ClaudeNotesSettings]) and the run's
// standing instructions told of notes (`--append-system-prompt-file <notes>`,
// [Paths.ClaudeNotesSystem]); on an older one, which the run's error stream
// says, the settings the script had chosen without it — every run's, or those
// that keep the skills' hook on ([claudeSkillsGate]) — and the standing
// instructions alone (plain, [Paths.System]), none for a run that has none.
//
// THE SYSTEM TEXT IS CHOSEN WITH THE HOOKS, because only this script knows
// whether a note can reach the run: told of notes, a run started without the
// hooks that read them would be told of a channel that never opens.
func claudeNotesGate(hooked, notes, plain string) string {
	older := ""
	if plain != "" {
		older = `set -- "$@" --append-system-prompt-file ` + sandbox.ShellQuote(plain) + "; "
	}
	return "if at " + floorArgs(claudeNotesFloor) + "; then cs=" + sandbox.ShellQuote(hooked) +
		`; set -- "$@" --append-system-prompt-file ` + sandbox.ShellQuote(notes) + "; else " + older +
		`echo "crewlet: this box runs Claude Code $n, older than ` + claudeNotesFloor + `, the oldest ` +
		`that reads a note at the run's finish; the run starts without the hooks that read its notes, ` +
		`is not told of them, and they wait for its review" >&2; fi` + "\n"
}

// claudeSessionGate is the part of a run's script that names the session its
// harness runs in, as s, whether that is the one the run was asked to continue, as
// k, and — where it was asked and could not — why, as w; and adds the flags
// that say so to the script's arguments.
//
// A RUN THAT STARTS A SESSION is started under the id minted for it.
//
// A RUN THAT CONTINUES ONE continues the copy its box HOLDS — a transcript
// under its id in any project directory of the box's configuration, the same
// box's last run's included, whichever directory a release filed it under —
// unless an archive of the session was carried in ([Paths.SessionCarried])
// that the copy does not hold: one whose digest is not among the box's for
// the session ([Paths.SessionDigests], and carry.go for why). That archive,
// and one carried into a box that holds no copy, is RESTORED — unpacked in the
// staging directory, and only its three parts moved under the pinned project
// directory ([claudeProjectDir]), once the archive is known to hold a
// transcript and the box's own copy of each is removed: the transcript, a
// regular file, and the sidecar and task-list directories where it has them,
// a link in place of any left behind — on a release that continues a carried
// session ([claudeCarryFloor]), with tar and gzip in the box; and its digest
// is kept: recorded, and read back, once it is restored ([recordDigest]), and a
// restore whose digest could not be is not continued. Restored or not, the
// archive is removed: nothing reads it again.
// Where the session is neither held nor restored, the run starts the session
// minted for it in its place and says why, on its error stream and in its
// record, rather than resume a session the harness does not hold, which fails the
// run.
//
// AND THE RUN IS WIRED TO THE SESSION IT STARTS, where it was handed a wiring
// for each (rewired, [sandbox.RunRequest.FreshWiring]): the MCP configuration
// under the continued session's bridge token where it takes that session up, and
// the one under the token of a session of its own moved over it where it starts
// one — the other removed from the box either way, before the harness starts.
// So the token the harness dials is the box's word to the bridge on which
// session it started (internal/api/mcpbridge), one the run's model can neither
// forge nor take back: the box holds no other token by the time the model can
// read a file. A configuration that cannot be moved into place ends the run
// before its harness starts, rather than start a session of its own on the
// continued one's token, whose skill guard it would be held to.
func claudeSessionGate(session Session, paths Paths, rewired bool) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := sandbox.ShellQuote
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
	line(`  elif ! { %s; }; then w=%s`, recordDigest(paths, "s", "d"), q(restoreDigestUnrecorded))
	line(`  fi`)
	line(`  rm -rf %s`, q(staged))
	line(`fi`)
	line(`rm -f %s`, carried)
	line(`if [ -z "$w" ]; then`)
	// A TRANSCRIPT CUT OFF MID-LINE is cut back to its last whole record —
	// the session's own, and EVERY THREAD UNDER ITS SIDECAR DIRECTORY, the
	// subagents' at any depth: a harness killed while it appended, or a pack
	// made while it still worked, leaves a line with no newline, a record no
	// reader of a JSON-lines transcript can use and onto which the harness's
	// next record to that file is written, losing that record too. Measured
	// on 2.1.310 against a scripted model: a subagent's thread resumed torn
	// had the record its CLI next wrote to it run into the torn line, and one
	// cut back to its last whole line was continued (SendMessage) through
	// its last whole call. A FILE WITH NO WHOLE LINE AT ALL IS LEFT AS IT
	// IS: the session's own transcript, which the script chose to resume
	// because it holds bytes, and a subagent's thread the pack read as its
	// subagent wrote its first record, which 2.1.310 continues from nothing
	// the restore could make of it — measured, a fragment, an emptied file
	// and a removed one alike answer SendMessage that no transcript was found
	// for the agent, write the thread nothing and leave the run going on — so
	// no record is ever written onto that fragment, and emptying or removing
	// it would change nothing the harness reads. A box with no find cuts no
	// thread, and says so. A resume is documented for a transcript that
	// ends in a whole record — one ending in a tool call with no result
	// among them included, which 2.1.310's resume answers with an error
	// result saying the call's outcome is unknown, and then "No response
	// requested.", before it reads the run's brief (measured).
	//
	// A FILE ENDS TORN WHERE ITS LAST BYTE IS NO NEWLINE, AS WC COUNTS IT,
	// never as a command substitution reads it, which drops a NUL (measured:
	// dash's and bash's, bash saying so on the error stream). GNU tar pads a
	// member that shrank while it read it with NULs, to the size it first
	// saw (measured on 1.35), and the session's transcript can shrink while
	// it is read: 2.1.310 rewrites it in place to withdraw a message (read in
	// its shipped code). Read as text, that padding would pass for a whole
	// last line, and the harness would write its next record onto it.
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
// ran in (s) once the harness has exited, to be continued in another box: one
// gzipped tar of the paths under the harness's configuration directory — the
// transcript, its sidecar directory (the subagents' transcripts and the tool
// results spilled to files) and its task list, the last two where they are
// there and are directories rather than links — packed in the staging
// directory and moved over [Paths.SessionCapture] whole, its digest kept as
// one the box's copy of the session holds ([Paths.SessionDigests]): recorded,
// and read back, before the archive is moved, so no archive stands there whose
// digest the box does not hold ([packDigest]).
//
// NEVER ANYTHING ELSE: not the login (`.credentials.json`) or the harness's own
// state (`.claude.json`), which the pinned configuration directory holds beside
// them, and not `file-history/`, the file-edit snapshots of the files the run
// edited, which carry the checkout rather than the conversation and are off.
//
// NOTHING TO PACK IS SAID NOWHERE: a harness that wrote no transcript — one that
// failed as it started, a release that files it elsewhere — has no session to
// carry, and a line saying so would end the failure of a run that crashed at
// its start with the engine's words rather than the harness's. A box without tar
// and gzip, or a pack that failed, is said on the error stream, and the run's
// own status is never the pack's.
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
// at.dest. ONE PACK for every one of them — the run's script's at its end, one
// made on demand of a job that never packed or that its box would not end, and
// a save — so each is the same archive of the same files, and each keeps its
// digest.
//
// A PACK MADE ONCE ITS HARNESS IS GONE — the run's own end, and one on demand
// of a job that is over — takes GNU tar's exit 1 with "file changed as we
// read it" as a whole archive of what it read: the only writer left is one
// the harness started that outlived it. A pack of a job that may still be
// working — one its box would not end, and a save — is made where the harness
// may still be writing, and is held to [claudeHeldTar]'s rule instead, which
// reads none of tar's words.
func claudePack(paths Paths, at packAt) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := sandbox.ShellQuote
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
// still be working — a job its box would not end, packed on demand, and a save
// — packing the operands the pack set ("$@", relative to cfg, the session's
// own transcript first) into staged/capture, or failing with the one check that
// refused it, said on one line in staged/error.
//
// A SESSION ITS HARNESS IS STILL WRITING IS NOT APPEND-ONLY, and tar's own
// word cannot be taken for what it read. Claude Code rewrites a task file as
// its task tool changes it, a subagent's metadata as the subagent ends, and
// its own transcript too: a trim renames a new file over it
// ([claudeLocalGCFloor]), and withdrawing a message already written — an
// abandoned attempt, a refusal, a malformed tool call retried — rewrites it
// IN PLACE, in the same inode, and can grow it back past its old size
// (2.1.310's removeTranscriptMessage, read in its shipped code). And tar says
// little of it: measured on GNU tar 1.35 and BusyBox 1.37.0, a file that grows
// as it is read is GNU's exit 1 ("file changed as we read it") and busybox's
// exit 0 with no word, each archive holding the file as tar first saw it; one
// that SHRINKS is exit 1 from both — GNU padding the member it read with NULs
// to the size it first saw, busybox cutting the archive inside it and losing
// every member after — and one shrunk and grown back within the read passed
// every check of size. So the run's own end's rule — GNU's exit 1 taken
// wherever "changed as we read it" stands anywhere in its error file — would
// keep beside a working harness a task file read torn, or padded with NULs,
// whenever another file's growth said those words, and a file busybox read
// torn as it was rewritten in place whenever tar said nothing (its cut
// archive, exit 1 with "short read", that rule refuses); and the scrub keeps
// a `.json` that is not one value as the text it is.
//
// SO THE PACK LISTS WHAT IT READS, BEFORE THE TAR, AND HOLDS IT TO THAT AFTER:
//
//   - THE TOOLS it reads the session with beside tar and gzip ([heldTools]),
//     each named where the box lacks it: without find, say, the wait below
//     would read the box's clock as never moving, and refuse on that word.
//   - NO FILE FROM THE BOX'S FUTURE, before the marker: a reference written
//     in the pack's own staging directory, and every file and directory of
//     the session that the box's find reads as newer than it — never a link,
//     which touch would follow out of the session — set to the box's present
//     (`touch -c`). A box cannot hold a file from its own future, and every
//     check below would read one as written since the marker until the box's
//     clock passed it — a task file failing every pack for that long, a
//     thread read back on each — whatever stamped it: a box whose clock ran
//     ahead, the session carried here with the times tar restores, or this
//     box before its clock stepped back. So the pack sets it, the one step
//     that reads a session file's time, rather than the resume, after which a
//     clock that steps back would leave every file stamped before the step
//     ahead of the clock. Once set, such a file is read as every other is: as
//     written where the harness writes it after the marker, and whole where
//     its last write came before the marker, tar reading it after. THE
//     PRESENT, NEVER AN EARLIER TIME: a file the walk sets reads as no older
//     than any write made before the walk set it, so the walk hides no write
//     from a marker — not from this pack's, written after the walk, nor from
//     that of another pack of the session running beside this one, as two
//     takers' saves, a save and the take of a job still working, or two takes
//     of one job at once may, each in a staging directory of its own. Set to
//     the reference's time instead, a walk that ran late, its find held up on
//     a busy box, would set a file the harness rewrote under the other pack's
//     tar back behind that pack's marker, and that pack would keep the file
//     torn. What another pack's walk sets after this pack's marker reads as
//     written here, which costs this pack, never a torn file. Every earlier
//     time is left as it is, which costs the session nothing, the harness's
//     own retention sweep being held off ([claudeRetentionDays]). A find that
//     compares whole seconds, as busybox's does, leaves a file stamped later
//     than the reference within the reference's own second, which it never
//     reads as newer than the marker either, the marker being written in that
//     second or after it. A file the walk could not set is left in the
//     future, where the checks below read it as written.
//   - A MARKER, and then a wait until this box's own find reads a file written
//     now as newer than it — busybox's find compares whole seconds (measured
//     on 1.37.0: a file written 200 ms after the marker, in the same second,
//     is not newer to it), so up to a second there, and a moment on GNU's,
//     which compares the clock's own grain; past [heldClockWaits] sleeps of a
//     tenth of a second the box's clock is taken not to move, and the pack
//     fails. A file written after that wait is newer than the marker to the
//     very find every check below asks, so no write while tar reads is missed;
//     one written during the wait may be named too, which costs one pack,
//     never a torn file.
//   - A MANIFEST of every file of the session: its inode and its size, from
//     `ls -i` and `wc -c` — and the sum of the sizes, which past what a kept
//     session's scrub reads ([heldPlainMax]) fails the pack before the tar,
//     since the take would refuse that session once packed.
//   - THE TAR, whose exit 0 or 1 alike is held to every check below, whatever
//     its error file says; any other exit is the pack's failure, with tar's
//     first line. Its archive is then listed, through gunzip as every reader
//     of it will — which refuses busybox's archive cut inside a member it read
//     shrinking, as busybox's own listing does ("short read") and GNU's
//     ("Unexpected EOF"); a listing of no file at all, tar's exit 1 over files
//     it could not read, is refused too.
//   - THE CHECKS, in this order, the first that fails naming its check and its
//     file: a file the manifest listed is GONE (the harness's own sweep, or a
//     trim, can unlink one); a file's inode changed (REPLACED: a trim's
//     rename, any file moved over another); a file SHRANK; a file the manifest
//     listed is not in the archive (MISSING: a file tar could not read, which
//     busybox's tar leaves out with exit 1); a member that is not a `.jsonl`
//     transcript was WRITTEN since the marker (find's -newer, held to files —
//     a directory gaining a thread or a spilled result is newer too, and read
//     so every pack during one would fail wherever a box's tar lists a
//     directory as find names it: GNU's and busybox's end one in a slash
//     (measured), which the filter by members alone keeps out, and -type f
//     keeps out any tar's — and to the archive's members, since a file made
//     after tar read its directory is not in it); the session's own
//     transcript, or another `.jsonl` member written since the marker, is not
//     A BYTE PREFIX of the live file — the member's cksum, read back out of
//     the archive, against the live file's first as many bytes — which
//     catches the in-place rewrite grown back, and the shrink grown back that
//     GNU pads with NULs; and, LAST, a member of the archive is GONE — so a
//     file removed while the checks ran is refused too: one made after the
//     manifest, archived and removed, which no check of the manifest reads,
//     and one written as tar read it and removed before WRITTEN or the
//     prefix's check looked, neither of which reads a file that is gone.
//   - EVERY CHECK FAILS CLOSED, and none rests on a tool's word that what it
//     wrote landed. A tool need not check a write it makes, and one that does
//     not exits 0 having written nothing, or half, to a full disk — the disk
//     a harness still writing can fill under the pack. So every list and
//     every answer a check reads is held in the shell first, read from its
//     tool through a pipe, which no full disk cuts, and each find's own exit
//     is held, never that of a pipeline's last command (a find that could
//     not walk the whole session, an `-exec` that failed among them, leaves a
//     file out of every check made of its list); then the shell writes it to
//     the staging directory and reads it back (keep), refusing the pack where
//     the file's cksum is not that of what was written. A list that cannot
//     be made or kept, and a comparison grep cannot make — its exit 2, or an
//     exit 0 whose answer names no line — refuse the pack as a difference
//     would, never read as nothing having changed: a check resting on a
//     list's holding a line, or on grep's exit alone, would hold a pack whose
//     checks compared nothing. And the lists are this pack's alone, in a
//     staging directory named for it — a pack on demand's
//     ([Paths.SessionPackStaging]), a save's ([Paths.SessionSaveStaging]) —
//     so no other pack of the box, another save of its taker's among them,
//     clears them under it.
//
// A `.jsonl` that only grew is the one change taken: a transcript or a
// subagent's thread appended to while tar read it is archived as tar first
// saw it, a whole prefix of what the harness wrote, at most its last line torn,
// which the restore cuts back ([claudeSessionGate]). A pack that fails leaves
// nothing of its own: its staging directory goes with it, and nothing is moved
// to its destination, which is that pack's own ([Paths.SessionPackHeld],
// [Paths.SessionSave]) — no pack on demand or save moves anything over the
// job's capture, or over another's archive, at all.
//
// THE SESSION'S OWN TRANSCRIPT IS READ BACK ON EVERY PACK, whatever any list
// says of it — its proof rests on no list — and of the other `.jsonl` members
// only one written since the marker. The transcript is the one `.jsonl` of
// the session Claude Code rewrites in place: withdrawing a message
// rewrites the session's own file and never a subagent's thread, which it only
// appends to (2.1.310's removeTranscriptMessage, read in its shipped code). So
// its proof cannot rest on the box's clock: a clock that steps back while a
// pack runs can stamp that rewrite before the marker, and a proof made only of
// what find reads as written would keep the transcript torn mid-file. A thread
// find reads as older than the marker is the file tar read, unwritten since —
// a rename over one, a removal and a shrink are refused above on no clock —
// and a long session holds a hundred subagents' threads, each read back by a
// decompression of the whole archive: every one would run the pack far past
// its bound ([sandbox.SessionPackWait]), whose spare minute covers some ten.
// Like WRITTEN, that narrowing rests on the box's clock: it does not step back
// while a pack runs, and it stamps the marker, in the work directory, and the
// session's files, in the configuration directory, at one grain — both
// directories the engine keeps under the box's home. A CLOCK THAT DOES STEP
// BACK while a pack runs costs at most a subagent's thread appended to as it
// was packed and missed by the check, kept with a partial last line — as a
// thread that only grew always is — which the resume's cut of a torn `.jsonl`
// removes ([claudeSessionGate]); and, since WRITTEN rests on the same clock, a
// file that is no transcript written as it was packed — rewritten in place (a
// task file, a subagent's `.meta.json`) or still being written (a spilled tool
// result) — kept torn or cut short. Neither WRITTEN nor the narrowing reads as
// written a file stamped in the box's future before the pack began, which the
// pack sets to the box's present before its marker (above) — unless the clock
// steps back after the pack set it and before the marker: then both read it as
// written, and that one pack refuses it where it is no transcript and reads it
// back where it is a thread; the next pack sets it to its own present.
//
// THE TOOLS ARE POSIX'S, and three POSIX does not name that every box's GNU,
// BSD or busybox userland has: head -c, tar's -O, and `sleep 0.1`. A box's
// sleep is required to take a tenth of a second — every wait the engine's own
// scripts make sleeps a fraction of a second at a time (end.go's grace, the
// kill of a job's group, OpenCode's waits for its server) — and a box whose
// sleep refuses one fails the pack, saying so, rather than run its wait out at
// once. No awk, and no stat, whose options differ between GNU and BSD. A
// transcript member is read back out of the archive as a stream, never onto
// the box's disk: a pack beside a harness that is still writing must not fill
// the disk under it, and the archive already holds the session once.
func claudeHeldTar(cfg, staged string) string {
	q := sandbox.ShellQuote
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
// ([claudeHeldTar]): the most one reading of an archive yields to a kept
// session's scrub, [scrubExpansion] times the bound a kept session's archive is
// held to (executorkeep.SessionMaxBytes), a quarter of a gibibyte. A session
// whose files add up past it is refused by the take once packed, so it is not
// packed at all: its tar beside a working harness would cost that harness's box
// some seconds of a core — a pack runs at about 43 MiB/s on one, measured — and
// a moment of as much disk, for an archive nothing would keep. A file's size
// is its plaintext and not quite the tar's, which adds a header to each, so
// the bound refuses what the scrub would and never more.
const heldPlainMax = scrubExpansion * executorkeep.SessionMaxBytes

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

// WriteConfig writes the run's system text and its settings where
// [ClaudeCode.Command] names them, and renders the scoped MCP surface: every
// run's settings ([claudeSettings]) always, and the rest only where there is
// some — the settings with the notes' hooks for a run wired to its turn's notes
// ([sandbox.RunRequest.Steer]), which every seat's executor run is, and the
// assistant's deep work is not.
//
// A RUN WIRED TO ITS NOTES HAS ITS SYSTEM TEXT AND ITS SETTINGS TWICE: as they
// stand ([Paths.System], [Paths.ClaudeSettings]), and with what its notes need
// ([Paths.ClaudeNotesSystem], [Paths.ClaudeNotesSettings]) — which of the two
// it is started with is its box's to decide, by its harness's release
// ([claudeNotesGate]).
//
// A RUN HANDED SKILLS OF ITS OWN ([sandbox.RunRequest.Skills]) has them as a
// plugin, with the hook that reports each load ([writeClaudeSkills]), which
// its box adds to the harness's arguments by its release ([claudeSkillsGate]),
// and its settings a third time, with hooks switched on so that hook runs
// whatever the repository says ([Paths.ClaudeSkillsSettings]).
//
// EVERY SETTINGS FILE IS WRITTEN WITH ITS PINNED COPY BESIDE IT: the same
// settings with the compaction point and a resume's load pinned
// ([claudeSettingsPin]) under the window the wrapper pins ([ClaudeCode.Pins]),
// which a release that takes the pin is handed in the file's place
// ([claudePinGate]).
//
// The settings files are written afresh, at the mode a box writes every file
// with (0600 on a local box): a box write replaces a file whole
// ([sandbox.Sandbox.WriteFile]), and the run's artefacts are cleared before it
// starts besides ([Runner.ClearArtifacts]), so a previous run's wider file is
// never kept.
func (c ClaudeCode) WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths) (string, error) {
	if req.System != "" {
		if err := box.WriteFile(ctx, paths.System(), []byte(req.System)); err != nil {
			return "", fmt.Errorf("executor: writing the system prompt: %w", err)
		}
	}
	pin := claudeSettingsPin(sandbox.ClaudeCodeWindow(req.Model))
	if err := writeClaudeSettingsPair(ctx, box, paths.ClaudeSettings(), paths.ClaudeSettingsPinned(),
		newClaudeSettings(nil), newClaudeSettings(pin)); err != nil {
		return "", err
	}
	if len(req.Skills) > 0 {
		if err := writeClaudeSkills(ctx, box, req.Skills, req.SkillLoads, paths); err != nil {
			return "", err
		}
		if err := writeClaudeSettingsPair(ctx, box, paths.ClaudeSkillsSettings(), paths.ClaudeSkillsSettingsPinned(),
			newClaudeHooksOnSettings(nil), newClaudeHooksOnSettings(pin)); err != nil {
			return "", err
		}
	}
	if req.Steer.Set() {
		if err := writeClaudeSettingsPair(ctx, box, paths.ClaudeNotesSettings(), paths.ClaudeNotesSettingsPinned(),
			claudeSteerSettings(req.Steer, nil), claudeSteerSettings(req.Steer, pin)); err != nil {
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
// ([claudeSettings]), those with hooks switched on ([claudeHooksOnSettings]),
// or those with the notes' hooks besides ([claudeNotesSettings]).
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

// claudeCodeMCP is one server in this harness's own .mcp.json vocabulary.
//
// THE KEY NAMES ARE THIS RUNNER'S, which is why they live here: they are what
// `claude --mcp-config` reads, and nothing above this file should have to know
// them. internal/sandbox used to spell them out and then say in its own doc
// that they belonged to a runner.
//
// An empty `env` or `headers` is OMITTED rather than written as an empty
// object, which is what the harness's own examples show and what this wrote
// before.
//
// `alwaysLoad` ONLY WHERE THE ENGINE ASKS ([sandbox.MCPServer.AlwaysLoaded]):
// the engine's own tools. Claude Code defers every MCP server's tools behind
// its tool search by default (the vendor's MCP page, "Configure tool search"),
// which is what keeps a seat's MCP servers out of each request — and kept
// submit_work there too, so a run had to search for the call that ends it.
// The key exempts the one server ("Exempt a server from deferral"), and is
// available on every transport.
//
// `timeout` ONLY WHERE THE ENGINE SETS A WAIT ([sandbox.MCPServer.CallTimeout]):
// the bridge's servers. In milliseconds, the server's own tool timeout, at
// which the harness gives a call up and cancels it, whatever progress flows
// (measured on 2.1.310: the entry's `"timeout": 360000` cut a call the bridge
// kept going at 360 seconds with `notifications/cancelled`), and on 2.1.310
// also its idle check on a call (by reading its bundle) and its HTTP
// transport's budget for a response's first byte (measured: 360000 in place
// of 60000). A release whose schema declares no such key, as 2.1.118's does
// not, drops it and keeps its own (by reading its bundle).
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
// The EVENT STREAM goes to its own file, read as a stream and from its end,
// and the RESULT is the stream's last line, which the wrapper copies to the
// result file after the harness exits — so the result is read whole from a file of
// one line however long the stream grew. It exits cleanly, so the done marker
// is its only completion signal and the poll reads nothing of the stream.
//
// THE BRIEF ON STDIN, for the reasons [ClaudeCode.Command] gives; the two
// variables that switch hooks and the bridge off, the one that starts the
// harness in its hosted mode, and the four that switch its compaction off or
// move the point it compacts at, UNSET before the harness starts
// ([claudeSwitchesOff], [claudeHostedOff], [claudeCompactionOff]), for
// [ClaudeCode.Pins] to pin the session's layout over.
func (ClaudeCode) Output(paths Paths) Output {
	return Output{Stdout: paths.Stream(), Events: true, Result: paths.Result(),
		BriefOnStdin: true, Unset: slices.Concat(claudeSwitchesOff, claudeHostedOff, claudeCompactionOff)}
}

// Pins is the session's layout ([claudeSessionLayout]) under the window of the
// model req's run works under, logging a model whose window this build does not know.
func (ClaudeCode) Pins(ctx context.Context, req sandbox.RunRequest, paths Paths) []Pinned {
	window := sandbox.ClaudeCodeWindow(req.Model)
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
// box, over whatever the seat's role.sandbox.env, the image or a profile the
// login shell sources set ([Harness.Pins]):
//
//   - CLAUDE_CONFIG_DIR, the box home's `.claude` ([Paths.ClaudeConfigDir]):
//     where the harness keeps its state by default, pinned because the project
//     directory below is read only where it is set. Set, it moves the harness's
//     own `.claude.json` from the home into that directory, and an image's
//     own `~/.claude.json` is no longer read; the login stays where a local
//     box is seeded with it and writes it back from
//     (`.claude/.credentials.json`), whatever an image or a seat set the
//     variable to;
//   - CLAUDE_CODE_PROJECT_DIR_NAME, [claudeProjectDir], so a session's
//     transcript is `<config>/projects/crewlet/<session>.jsonl` whatever the
//     box's working directory (2.1.234 and later);
//   - CLAUDE_CODE_DISABLE_AUTO_MEMORY, 1: auto memory writes notes under the
//     session's project directory that a later run reads back without being
//     handed them — the second, invisible memory the engine's own calls are
//     kept from (docs/concepts/llm-backends.md, "Between turns"), which a
//     session carried between boxes would carry too;
//   - CLAUDE_CODE_AUTO_COMPACT_WINDOW, window ([sandbox.ClaudeCodeWindow]): the
//     window of the model the run works under, which the harness compacts at
//     less its own headroom. The variables that would switch compaction off
//     altogether or move the point it compacts at are unset beside it
//     ([claudeCompactionOff]), and on a release that takes it the run's
//     settings pin the same window and blank the same two overrides
//     ([claudeSettingsPin]), over what a repository's own settings set. No
//     agent definition caps the main thread's window, whatever a repository,
//     an image or a managed directory ships. A subagent's or an in-process
//     teammate's can be lower: from 2.1.312 an agent definition's own
//     `autoCompactWindow` (a repository's `.claude/agents/<name>.md`, for one)
//     lowers the pinned window for the conversation that definition runs,
//     where 2.1.310 drops it wherever the variable is set ("Changed a
//     subagent's `autoCompactWindow` to lower the window set by
//     `CLAUDE_CODE_AUTO_COMPACT_WINDOW`", the 2.1.312 changelog; read in
//     2.1.310 and 2.1.322). Lower only, so no request carries more than the
//     window.
//
// The transcript's trim (CLAUDE_CODE_TRANSCRIPT_LOCAL_GC) is the script's to
// set, by release ([claudeLocalGCFloor]); and the harness's own retention sweep
// is held off by the settings every run is started with ([claudeSettings]),
// since no variable reaches it.
func claudeSessionLayout(paths Paths, window sandbox.ExecutorWindow) []Pinned {
	return []Pinned{
		{Name: "CLAUDE_CONFIG_DIR", Value: paths.ClaudeConfigDir()},
		{Name: "CLAUDE_CODE_PROJECT_DIR_NAME", Value: claudeProjectDir},
		{Name: "CLAUDE_CODE_DISABLE_AUTO_MEMORY", Value: "1"},
		{Name: "CLAUDE_CODE_AUTO_COMPACT_WINDOW", Value: strconv.Itoa(window.Tokens)},
	}
}

// claudeRetentionDays is the `cleanupPeriodDays` every Claude Code run is
// started with ([claudeSettings]): how old a file under the box's
// configuration directory must be before the harness's own retention sweep
// deletes it.
//
// THE BOX'S HARNESS MUST NOT SWEEP WHAT THE ENGINE CARRIES. Every harness
// process, print mode included, sweeps that directory about five seconds into
// its run, judging each file by its own mtime: a transcript past the cutoff
// with its sidecar directory, a living transcript's spilled tool results one by
// one, and a task list whole, by its directory's mtime. A session carried into
// a box keeps the mtimes it was packed with ([claudeSessionGate] restores it
// with `tar -xzf`, no `-m`), and it is as old as its conversation: the engine
// continues one for as long as its turns come within
// turn_engine.conversation_session.retention_days of each other — thirty by
// default and bounded only below (config.ConversationSession) — so its first
// files can be any age. At the harness's default of thirty days, a continued
// conversation lost its oldest tool results and its task list to its own box
// five seconds into a turn. Measured on 2.1.310 against a stub model API: a
// resumed session's tool result and task list forty days old, deleted without
// this setting and kept with it.
//
// 36,500 DAYS, a hundred years, because nothing finite the engine keeps bounds
// a session's age, and the harness takes no "never": 0 is refused from 2.1.89,
// which in print mode then ignores the whole file without a word, and before
// that it stopped every transcript being written. The cutoff it makes, in 1926,
// is older than any file a box holds and well inside a JavaScript Date's range,
// so it is never the harness's guard for a cutoff past that range that keeps
// the files. Before 2.1.310 there was no such guard, and a cutoff past the
// range is an invalid Date, which a sweep keeping only what is at or after its
// cutoff keeps nothing of (2.1.112's sweep of its worktrees, read; "Fixed
// background cleanup deleting some recent files ... when cleanupPeriodDays is
// set to a very large value", 2.1.310's changelog).
const claudeRetentionDays = 36500

// claudeSettings is the settings file every Claude Code run is started with
// (`--settings`, [Paths.ClaudeSettings]): the harness's own retention sweep held
// off ([claudeRetentionDays]), and nothing else but, in its pinned copy, the
// settings pin ([claudeSettingsPin]). A run that starts a hook of
// the engine's is started with these settings and hooks switched on in this
// file's place ([claudeHooksOnSettings]), and the notes' hooks beside them where
// those are among its hooks ([claudeNotesSettings]), so every file a run is
// started with holds them.
//
// EVERY RELEASE THAT RUNS THIS RUNNER'S COMMAND READS IT. The field is in the
// settings schema of 0.2.116 already, the oldest read, and `--settings` is
// from 1.0.61, and every run is handed
// `--append-system-prompt-file` ([finalSystem] gives every run standing
// instructions), which 1.0.61 does not know — read in the shipped code of
// 1.0.60, 1.0.61 and 2.1.112, whose commands refuse an option they do not
// know, and which take the value as a whole number at least 0 (1.0.x) or 1
// (2.1.112, as 2.1.310 does). A settings file the harness cannot validate is
// ignored without a word in print mode, so it holds no other field, and its
// pinned copy, which adds the settings pin, is handed only to a release that
// takes it ([claudeSettingsPinFloor]).
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
// ([claudeSettingsPinFloor]), which pins the point the run compacts at and
// what a resume of its session loads: CLAUDE_CODE_AUTO_COMPACT_WINDOW at the
// window [claudeSessionLayout] pins, CLAUDE_AUTOCOMPACT_PCT_OVERRIDE and
// CLAUDE_CODE_BLOCKING_LIMIT_OVERRIDE blank, and every switch a resume is
// started without ([claudeResumeSwitches]) blank.
//
// THE WRAPPER'S AND THE SCRIPT'S PINS AGAIN, BECAUSE A REPOSITORY'S SETTINGS
// CAN UNDO THEM. The harness assigns every settings source's `env` block to its
// own environment once it has started, and before print mode resumes a
// session, in the order user, project, local, `--settings`, managed, a later
// source's value winning (read in 2.1.310 and 2.1.322). So a checkout's own
// `.claude/settings.json` or `.claude/settings.local.json` that sets any of
// them sets it again after the wrapper or the script unset or pinned it
// ([claudeCompactionOff], [claudeSessionLayout], [claudeResumeSwitches]): the
// two overrides lower the point the run compacts at or set the count it
// refuses a request past, the window replaces the pin, and a switch changes
// what the resume hands its model. The run's `--settings` is assigned after
// the repository's, so the same names there win over it.
//
// BLANK, NOT LEFT OUT: a key the run's settings leave out leaves the
// repository's value standing, and the harness reads each blank one as unset:
// an override as none (`h?parseFloat(h):void 0`), and a switch as off, since
// only "1", "true", "yes" and "on" switch one on (2.1.310 and 2.1.322 alike).
//
// What is NOT pinned, and why: a managed settings file, which the harness
// ranks last, still wins over the run's, as an image's or a host's policy is
// its operator's own. DISABLE_AUTO_COMPACT and DISABLE_COMPACT are left to the
// repository, as its `autoCompactEnabled: false` is: either only switches
// compaction off, which leaves nothing summarized, and the run's requests then
// grow to its model's own window, as docs/concepts/code-sandbox.md states.
func claudeSettingsPin(window sandbox.ExecutorWindow) map[string]string {
	pin := map[string]string{
		"CLAUDE_CODE_AUTO_COMPACT_WINDOW":     strconv.Itoa(window.Tokens),
		"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":     "",
		"CLAUDE_CODE_BLOCKING_LIMIT_OVERRIDE": "",
	}
	for _, name := range claudeResumeSwitches {
		pin[name] = ""
	}
	return pin
}

// claudeHooksOnSettings is the settings of a Claude Code run that starts a hook
// of the engine's: every run's ([claudeSettings]) and hooks switched on
// explicitly. A run whose only such hook is its skills' plugin's, which reports
// each load, is started with them ([Paths.ClaudeSkillsSettings],
// [claudeSkillsGate]); the notes' settings hold them beside the notes' hooks
// ([claudeNotesSettings]).
//
// ONLY WHERE THE RELEASE KNOWS THE KEY: the settings schema declares
// disableAllHooks from 1.0.68 (read in the shipped code: 1.0.67's declares no
// such key), and a file holding a key its harness does not know is ignored
// whole in print mode, the retention with it — so a file holding it is named
// only at or after the floor of the hook it keeps on, 2.1.118 for the skills'
// ([claudeSkillsFloor]) and 2.1.163 for the notes' ([claudeNotesFloor]), both
// far later.
type claudeHooksOnSettings struct {
	claudeSettings

	// DisableAllHooks is written FALSE, never left out — the posture. A
	// repository's own `.claude/settings.json` that sets it true switches
	// every hook off with no error, a plugin's among them, and an explicit
	// false in the run's settings wins over it: the harness merges its
	// sources user, project, local, `--settings`, managed, a later one's
	// value winning (read on 2.1.118; measured on 2.1.263 and 2.1.310). It
	// wins for the repository's own hooks too, which is why a hook of theirs
	// that stops the loop is read as the failure it is ([ClaudeCode.Parse]).
	DisableAllHooks bool `json:"disableAllHooks"`
}

// newClaudeHooksOnSettings is the settings of a run that starts a hook of the
// engine's, with env as their `env` block ([newClaudeSettings]).
func newClaudeHooksOnSettings(env map[string]string) claudeHooksOnSettings {
	return claudeHooksOnSettings{claudeSettings: newClaudeSettings(env), DisableAllHooks: false}
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

// Pack packs session now, as a run's script packs the session its harness ran
// in once the harness exits ([carrier], [claudeCapture]): the same members,
// into staged, this pack's own staging directory, and over dest — the pack's
// own place, held for its read ([Paths.SessionPackHeld]), never the job's
// capture — its digest kept, ending [packNothing] where the harness wrote no
// transcript, and [packFailed] where it did and no archive was made, its error
// stream saying why ([Runner.PackCapture]).
//
// HELD TO THE RULE OF A PACK BESIDE A WORKING HARNESS ([claudeHeldTar]) where
// working says the job may still be working — one its box would not end, its
// harness perhaps writing as tar reads — and to the run's own end's where the
// job is over: killed before its script could pack, lost with its engine, its
// own pack failed. The take knows which ([sandbox.SessionTake.Pack]); a job
// that is over has no harness left to write under a pack, and the working
// rule would cost it the wait for the box's clock, and refuse a file that a
// process its harness started, outliving it, writes as it packs.
func (ClaudeCode) Pack(paths Paths, session, staged, dest string, working bool) string {
	return claudeOnDemand(paths, session, packAt{staged: staged, dest: dest, working: working})
}

// PackSave packs a save of session now, beside a harness that may still be
// working ([saver]): the same members as every pack, held to the rule of a
// pack beside a working harness ([claudeHeldTar]), into staged, this save's
// own staging directory, and over dest, this save's own place — never the
// job's capture, which every read of a job that is over takes as its final
// archive — its digest kept, ending as [ClaudeCode.Pack] does
// ([Runner.PackSave]).
func (ClaudeCode) PackSave(paths Paths, session, staged, dest string) string {
	return claudeOnDemand(paths, session, packAt{staged: staged, dest: dest, working: true})
}

// claudeOnDemand is a pack the engine makes of session, outside the run's own
// script, at at: [packNothing] where the box holds no transcript of it,
// [packFailed] where no archive was made.
func claudeOnDemand(paths Paths, session string, at packAt) string {
	transcript := paths.ClaudeConfigDir() + "/projects/" + claudeProjectDir + "/" + session + ".jsonl"
	return "s=" + sandbox.ShellQuote(session) + " packed=\n" +
		fmt.Sprintf("[ -s %s ] || exit %d\n", sandbox.ShellQuote(transcript), packNothing) +
		claudePack(paths, at) +
		fmt.Sprintf("[ -n \"$packed\" ] || exit %d\n", packFailed)
}

// layout is a Claude Code archive's: a gzipped tar of the session's files
// ([claudeArchiveFormat]).
func (ClaudeCode) layout() archiveLayout { return archiveTar }

// Continues reports whether a session written by Claude Code release writer
// can be continued in a box whose Claude Code is release box ([carrier]): the
// box at or after [claudeCarryFloor], the writer at or after
// [claudeCarriedWriterFloor], and never a box older than the release that wrote
// the session, which reads only what it and the releases before it wrote.
func (ClaudeCode) Continues(box, writer string) (bool, string) {
	switch {
	case !releaseAtLeast(box, claudeCarryFloor):
		return false, fmt.Sprintf("this box runs Claude Code %s, older than %s, the oldest that "+
			"continues a session carried from another box", box, claudeCarryFloor)
	case !releaseAtLeast(writer, claudeCarriedWriterFloor):
		return false, fmt.Sprintf("the session was written by Claude Code %s, older than %s, the "+
			"first whose compaction summaries survive a resume", writer, claudeCarriedWriterFloor)
	case !releaseAtLeast(box, writer):
		return false, carriedOlder("Claude Code", box, writer)
	}
	return true, ""
}

// claudeSwitchesOff is every variable that, set in a run's environment, starts
// Claude Code without the hooks and the bridge its run is given: bare mode
// (CLAUDE_CODE_SIMPLE), which also drops CLAUDE.md, subagents and a plan's
// login, and safe mode (CLAUDE_CODE_SAFE_MODE), which drops hooks and every
// --mcp-config server. See [ClaudeCode] for why they are unset rather than
// refused.
var claudeSwitchesOff = []string{"CLAUDE_CODE_SIMPLE", "CLAUDE_CODE_SAFE_MODE"}

// claudeHostedOff is every variable that, set in a run's environment, starts
// Claude Code in the mode of Anthropic's own hosted environment (Claude Code on
// the web), which no box is: CLAUDE_CODE_REMOTE. Switched on, with
// CLAUDE_CODE_ENVIRONMENT_KIND absent or `byoc`, it changes what a resume hands
// its model: unless a flag the harness fetches turns it off, the resume's load
// gives a record whose parent the transcript does not hold another parent, an
// earlier record it picks by timestamp, up to 16 times a load, so the run
// continues a conversation its session never recorded; and 2.1.322 can take
// another of the conversation's ends as the one it continues. 2.1.322 also
// raises its heap (`--max-old-space-size=8192`) and admits its coordinator and
// hermetic modes, which other variables ask for (read in 2.1.310's and
// 2.1.322's shipped code). No settings file can set it: both releases ignore
// it in a settings file's `env`, so only the environment the harness starts
// in can.
//
// UNSET RATHER THAN REFUSED, as [claudeSwitchesOff] is: it can arrive from the
// seat's role.sandbox.env, the image or a profile the login shell sources, and
// only the wrapper, running after all three, reaches every one. AND ON EVERY
// RUN, not only on a resume, whose load it moves: a relaunch continues the
// session its job ran, and unset on the resume alone, it would leave a job
// started in the hosted mode to be continued outside it, by a run that does
// not run as the job it continues, wherever an operator set the variable.
var claudeHostedOff = []string{"CLAUDE_CODE_REMOTE"}

// claudeCompactionOff is every variable that, set in a run's environment,
// switches its harness's compaction off or moves the point it compacts at:
//   - DISABLE_AUTO_COMPACT, automatic and idle compaction, and
//     DISABLE_COMPACT, every compaction (the vendor's environment reference),
//     either of which would let a run's context grow past its model's window
//     rather than compact;
//   - CLAUDE_AUTOCOMPACT_PCT_OVERRIDE, which lowers the point the harness
//     compacts at to that share of its window, and
//     CLAUDE_CODE_BLOCKING_LIMIT_OVERRIDE, which sets the count past which it
//     refuses a request until the conversation is compacted (both read in the
//     harness's own threshold code, 2.1.310 and 2.1.322 alike), either of which
//     would have it compact a session at a point the engine does not reckon
//     with: the window [claudeSessionLayout] pins, less the harness's own
//     headroom, which docs/concepts/code-sandbox.md states and every floor the
//     engine continues a session by is held to.
//
// Set by a seat's role.sandbox.env or by an image, any of them is unset rather
// than refused, as [claudeSwitchesOff] is: the run's compaction is the
// engine's to decide. A repository's own settings can set them again once the
// harness has started, by its `env` block; on a release that takes the pin, the
// run's settings blank the two that move the point over it
// ([claudeSettingsPin]), and leave the two that switch compaction off to the
// repository, as its `autoCompactEnabled: false` is. CLAUDE_CODE_MAX_CONTEXT_TOKENS is not among them: it is
// the real window of a model the harness does not know, which it reads for
// such a model alone.
var claudeCompactionOff = []string{"DISABLE_AUTO_COMPACT", "DISABLE_COMPACT", "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE",
	"CLAUDE_CODE_BLOCKING_LIMIT_OVERRIDE"}

// claudeResumeSwitches is every variable that, set when a run resumes its
// session, switches on what changes the session its harness hands its model,
// and that the run's script therefore unsets on every resume
// ([claudeSessionGate]):
//   - CLAUDE_CODE_RESUME_INTERRUPTED_TURN, which has the resume run the turn
//     its last process was cut off in again before it reads this run's brief,
//     repeating a call that may have run already, which the brief tells the
//     run to check rather than repeat;
//   - CLAUDE_CODE_DISABLE_PRECOMPACT_SKIP, which has the resume of a
//     transcript past 5 MiB read the whole file and prune it by a rule of its
//     own, in place of the reader the harness runs without it, and the two
//     can hand the model different records (read in 2.1.310's and 2.1.322's
//     shipped code).
//
// Unset rather than refused, as [claudeSwitchesOff] is, and on a resume alone:
// a run that starts its session has none to take up. A repository's own
// settings can set either again once the harness has started, by their `env`
// block, which the harness applies before it resumes the session; so on a
// release that takes the pin, the run's settings blank both over it
// ([claudeSettingsPin]), which the harness reads as off.
var claudeResumeSwitches = []string{"CLAUDE_CODE_RESUME_INTERRUPTED_TURN", "CLAUDE_CODE_DISABLE_PRECOMPACT_SKIP"}

// Events is a new decoder for one stream, read whole or followed ([Decoder]).
func (ClaudeCode) Events() Decoder { return &claudeEvents{tools: map[string]string{}} }

// Parse maps the harness's result — the stream's last line — onto a result.
//
// TOLERANT BY DESIGN: non-JSON or partial output yields a FAILED result
// carrying an account of it, never an error. A harness that crashed should
// surface as "did not deliver" and let the turn continue, not blow the turn up
// — the executor can still report what happened, which is more use to the
// requester than a failed turn.
//
// A RESULT IS AN OBJECT OF TYPE `result`, and nothing else is. Under the
// stream the result file holds the stream's LAST LINE, whatever it was: a run
// that died part-way leaves an assistant message or a tool result there, and
// an object of another type has no subtype and no is_error — read as a
// result, it was a run that SUCCEEDED and said nothing.
func (ClaudeCode) Parse(stdout string) sandbox.Result {
	text := strings.TrimSpace(stdout)
	if text == "" {
		return sandbox.Result{Error: "the harness produced no output"}
	}
	msg, ok := decodeClaudeResult(text)
	if !ok {
		if last := lastLine(text); strings.HasPrefix(last, "{") {
			// A LINE THAT STARTS AN OBJECT AND IS NOT ONE is a message the
			// process stopped writing part-way, not an account of anything:
			// its fragment — of a file it read, as like as not — is no use to
			// a reader, and it is what the error stream beside it explains.
			return sandbox.Result{Error: fmt.Sprintf("the harness's output ends in a line "+
				"that is not a whole JSON object (%s), so it never reported how its run ended",
				humanSize(int64(len(last))))}
		}
		// THE OUTPUT IS THE FAILURE'S DETAIL, WHOLE. Unparseable output is
		// the case where the text IS the account — there is no structured
		// field to fall back to — and the useful part of it (the actual
		// error, after the banner and the warnings) is at the END, which a
		// head cut discarded and a tail cut kept only by luck of size. It
		// is carried as the Error rather than as a report, because it is
		// not one: the coordinator condenses a failure past the record's
		// bound keeping its cause, where a report keeps its findings.
		return sandbox.Result{
			Error: "the harness's output could not be parsed:\n" + text,
		}
	}
	if msg.Type != "result" {
		what := "an object with no type"
		if msg.Type != "" {
			what = fmt.Sprintf("a %q message", msg.Type)
		}
		return sandbox.Result{Error: "the harness stopped before it reported how its run ended: " +
			"the last thing it printed was " + what + ", not its result"}
	}

	res := sandbox.Result{
		Text:          msg.Result,
		Success:       msg.succeeded(),
		SessionID:     msg.SessionID,
		CostUSD:       msg.TotalCostUSD,
		DeliveredRefs: deliveredRefs(msg.Result),
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
		// THE LINE ITSELF, for the reader that classifies how the run
		// ended by the vendor's status rather than its words: a spent plan
		// is `is_error` with `api_error_status` 429, whatever the sentence
		// in `result` says this release ([sandbox.Result.Event]).
		res.Event = text
	}
	return res
}

// claudeResult is the harness's result message — the stream's last line, and the
// one object `--output-format json` printed — as the vendor's Agent SDK
// documents it: `subtype` names HOW the run ended ("success",
// "error_max_turns", "error_during_execution", "error_max_budget_usd", …),
// `is_error` whether it failed, `result` the final text of a run that got as
// far as one, and `errors` what went wrong on a subtype that is not success,
// which carries no `result` at all.
//
// Read into named fields so the stream's decoder and [ClaudeCode.Parse] read
// one shape, off one decode of the line ([claudeResultWire]) by its keys
// exactly as written, and TOLERANTLY (a field of an unexpected type is left
// zero and the rest still read), because a field the vendor reshapes must
// cost that field rather than the run's whole account.
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
	// SinceLastResult is, per model, what the calls THIS PROCESS counted
	// since it wrote its previous result cost — since it started, for its
	// first — on 2.1.310 and later, absent before. The harness's own SDK schema
	// says to sum it over a conversation's results rather than subtract its
	// totals, "whatever a resume or a kill does to modelUsage": nothing is
	// carried into it from another process, so the sum over one run's
	// results is that run's own spend whether or not it continued a session
	// ([claudeEvents.runSpend]). `usage` and `total_cost_usd` beside it are
	// the session's, which from 2.1.277 a continued session starts from
	// ([ClaudeCode.TotalsRestored]).
	//
	// RAW, and read where it is asked for ([claudeResult.sinceLastResult]): on
	// a result line of the stream, by the reader that sums it, a model at a
	// time and for at most [claudeResultModelsMax] models, and never by
	// [ClaudeCode.Parse], which reads none of it, nor on a line of another
	// type that carries the key.
	SinceLastResult json.RawMessage
}

// claudeResultWire is the result message as its one decode leaves it
// ([wireDecode]): each key a [claudeResult] holds, by its exact name and raw,
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
// ([claudeResult.SinceLastResult]), and whether the result states it:
// PRESENT, EVEN EMPTY, IS STATED, and an absent key states nothing, as on a
// release before 2.1.310, which never writes it. Each model's spend is read
// by its keys exactly as written and loosely by type ([looseModelUsage]), and
// a model the result names twice by its last, as JSON.parse reads it.
//
// A VALUE THAT IS THERE AND NO OBJECT IS UNREAD, null among it, never a
// result that states nothing: both releases measured write the key as an
// object or not at all (their result schema declares it an optional record
// of objects), so another kind is a reshape or a line no harness wrote, never
// a spend of nothing. Read as nothing, it would leave that result's calls out
// of the run's sum, as a result past the bound would (below).
//
// A MODEL AT A TIME ([eachKey]), never the whole object first, and AT MOST
// [claudeResultModelsMax] OF THEM. The walk keeps each model it meets and a
// copy of the last spend written for it, made into the room the one before
// left, and reads each kept spend once the walk is over: so a model named
// over and over costs one spend's copy and one read, never one for each time
// it is named, and a line naming any number of models costs no more than one
// naming that many.
//
// A RESULT NAMING MORE IS UNREAD, errResultModels: the walk stops at the
// first model past the bound, and no spend of the result is read, never the
// sum of the models before it, which would charge the run for some of its
// calls and not the rest. So is an object the walk cannot read to its end,
// its own error, which no line the stream decoded whole holds. Its one
// reader, the run's event stream, then reads the run's own spend as stated by
// none of its results ([claudeEvents.runSpend]).
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
	err := eachKey(r.SinceLastResult, func(name []byte, spend json.RawMessage) error {
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
// ([claudeModelUsageWire]).
type claudeModelUsage struct {
	InputTokens              int
	OutputTokens             int
	CacheReadInputTokens     int
	CacheCreationInputTokens int
	CostUSD                  float64
}

// claudeModelUsageWire is one model's spend as its decode leaves it, each key
// by its exact name and raw ([looseModelUsage]).
type claudeModelUsageWire struct {
	InputTokens              json.RawMessage `json:"inputTokens"`
	OutputTokens             json.RawMessage `json:"outputTokens"`
	CacheReadInputTokens     json.RawMessage `json:"cacheReadInputTokens"`
	CacheCreationInputTokens json.RawMessage `json:"cacheCreationInputTokens"`
	CostUSD                  json.RawMessage `json:"costUSD"`
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
// is at or after floor. Its numbers are read up to the first character that is
// neither a digit nor a dot; a version with fewer than three numbers is one
// this code cannot read, and is taken for a current release.
func releaseAtLeast(version, floor string) bool {
	read := func(v string) ([3]int, bool) {
		if cut := strings.IndexFunc(v, func(r rune) bool { return (r < '0' || r > '9') && r != '.' }); cut >= 0 {
			v = v[:cut]
		}
		fields := strings.Split(v, ".")
		var out [3]int
		if len(fields) < 3 {
			return out, false
		}
		for i := range 3 {
			n, err := strconv.Atoi(fields[i])
			if err != nil {
				return out, false
			}
			out[i] = n
		}
		return out, true
	}
	have, ok := read(strings.TrimSpace(version))
	if !ok {
		return true
	}
	want, _ := read(floor)
	for i := range 3 {
		if have[i] != want[i] {
			return have[i] > want[i]
		}
	}
	return true
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
	if eachElement(r.Errors, func(element json.RawMessage) error {
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
func decodeClaudeResult(text string) (claudeResult, bool) {
	if !strings.HasPrefix(text, "{") {
		return claudeResult{}, false
	}
	// A WHOLE OBJECT IS A RESULT'S ACCOUNT, whatever its keys hold: its decode
	// fails only where the line is no whole JSON object, and a key of another
	// kind than its field's is read loosely ([claudeResultWire.result]).
	var w claudeResultWire
	if wireDecode([]byte(text), &w) != nil {
		return claudeResult{}, false
	}
	return w.result(), true
}

// lastLine is the text's last line, trimmed.
func lastLine(text string) string {
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		text = text[i+1:]
	}
	return strings.TrimSpace(text)
}
