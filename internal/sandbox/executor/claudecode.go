package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// ClaudeCodeName is this runner's config name.
const ClaudeCodeName = "claude-code"

// ClaudeCode drives Claude Code headless.
//
// It reaches its model through the run ENVIRONMENT rather than a config file,
// which is why WriteConfig here renders only MCP servers and hooks: the
// credential comes from providers.llm via the sandbox env, and duplicating it
// into a file inside the box would put the secret somewhere the agent can read
// it back.
//
// # A person's notes, between its steps
//
// A run whose launch wired it to its turn's notes ([sandbox.RunRequest.Steer])
// is started with a settings file of its own ([Paths.ClaudeSettings]) holding
// two hooks, after each batch of the main agent's tool calls (PostToolBatch)
// and as it is about to finish (Stop), each a call of the bridge's unlisted
// `steer_take` tool over the run's own MCP connection; the bridge answers with
// the notes as the hook's additional context, or with nothing. Measured on
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
// silently. `disableAllHooks` is written false ([claudeSettings]), because a
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
// A RELEASE OLDER THAN 2.1.163 IS STARTED WITHOUT THE HOOKS ([claudeNotesFloor]):
// it runs them, and drops what a Stop hook answers, so a note taken as the run
// finishes would be recorded read by a run that never saw it. What switches the
// hooks off otherwise — a managed policy that allows only managed hooks
// (allowManagedHooksOnly, strictPluginOnlyCustomization) or disables every hook
// (disableAllHooks) — leaves, like that older release, a run that succeeded
// without asking once, which the collection says (internal/sandbox,
// steer_reader_never_ran).
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
// ([claudeCarryFloor]); and every resumed run renders its system prompt afresh
// ([claudeSnapshotFloor]), so a later launch's standing instructions are the
// ones it reads.
type ClaudeCode struct{}

var _ Harness = ClaudeCode{}

// NewClaudeCode returns the runner.
func NewClaudeCode() *Runner { return New(ClaudeCode{}) }

// Name is the harness's key in the runner registry.
func (ClaudeCode) Name() string { return ClaudeCodeName }

// Resumes says Claude Code continues a session it reported (`--resume`).
func (ClaudeCode) Resumes() bool { return true }

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
		return Session{ID: req.Resume, Continues: true, Fresh: uuid.NewString()}
	}
	return Session{ID: uuid.NewString()}
}

// Command builds the headless `claude -p` invocation: ONE SCRIPT, so the
// wrapper's redirections — the brief on stdin, the stream, the error file —
// reach every step of it alike. It reads the box's `claude --version` once
// ([claudeRelease]) and decides by it what the harness is started with; names the
// session the harness runs in and records it ([Paths.SessionRecord]); runs the harness;
// and once the harness has exited, packs its session to be carried to another box
// ([claudeCapture]), exiting with the harness's own status.
//
// Pure and deterministic given its session, so the exact flags can be pinned by
// a test. Budget caps are appended only when set, so a minimal call stays
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
// it. What the agent may reach is decided before it starts, engine-side: by
// its box's CELL — `container` and `e2b` keep it off the engine host, while
// `direct` runs it on the engine host as the engine's own user — by which MCP
// servers were rendered into its config, and by which credentials the run
// env carries.
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
	parts := []string{
		"claude", "-p",
		"--output-format", "stream-json", "--verbose",
		"--permission-mode", "bypassPermissions",
		// WHAT THE SCRIPT DECIDED BY THE HARNESS'S RELEASE, handed in as the
		// script's own arguments: the run's hooks and its system text told of
		// notes, where its launch wired it to them, and its session.
		`"$@"`,
	}
	if req.System != "" && !req.Steer.Set() {
		parts = append(parts, "--append-system-prompt-file", shellQuote(paths.System()))
	}
	// VERBATIM, because the only entry that reaches this runner is a
	// claude-code one, which already names the model in Claude Code's own
	// terms — its alias or id exactly as its runs on the engine host pass
	// it. A launch passes over every other harness's entry, so none reaches here.
	if req.Model != "" {
		parts = append(parts, "--model", shellQuote(req.Model))
	}
	if req.Limits.MaxTurns > 0 {
		parts = append(parts, "--max-turns", strconv.Itoa(req.Limits.MaxTurns))
	}
	if req.Limits.MaxBudgetUSD > 0 {
		parts = append(parts, "--max-budget-usd",
			strconv.FormatFloat(req.Limits.MaxBudgetUSD, 'f', -1, 64))
	}
	if configPath != "" {
		// --strict-mcp-config so the agent gets EXACTLY the scoped surface:
		// without it the harness also loads whatever config the box's home
		// happens to carry, which on a reused box is the previous run's.
		parts = append(parts, "--mcp-config", shellQuote(configPath), "--strict-mcp-config")
	}

	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	b.WriteString(claudeRelease)
	// THE TRANSCRIPT KEPT NEAR ITS SIZE, on a release whose trim loses nothing
	// ([claudeLocalGCFloor]) — and on an older one never, whatever set it.
	line(`if at %s; then export CLAUDE_CODE_TRANSCRIPT_LOCAL_GC=1; else unset CLAUDE_CODE_TRANSCRIPT_LOCAL_GC; fi`,
		floorArgs(claudeLocalGCFloor))
	// THE RUN'S OWN HOOKS, which hand it a person's notes between its steps
	// — only where its launch wired it to them ([ClaudeCode.WriteConfig]), and
	// only on a release that reads what they answer — AND ITS SYSTEM TEXT,
	// told of notes only where they are read: the gate decides both.
	if req.Steer.Set() {
		plain := ""
		if req.System != "" {
			plain = paths.System()
		}
		b.WriteString(claudeNotesGate(paths.ClaudeSettings(), paths.ClaudeNotesSystem(), plain))
	} else {
		line(`set --`)
	}
	b.WriteString(claudeSessionGate(session, paths))
	// THE RECORD, before the harness starts, so a run killed before it reported
	// anything still says which session it took up, and why it began again
	// where it did. The release last, as the harness printed it
	// ([parseSessionRecord]).
	line(`printf 'id %%s\ncontinued %%s\nwhy %%s\nrelease %%s\n' "$s" "$k" "$w" "$v" > %s`,
		shellQuote(paths.SessionRecord()))
	line(`%s`, strings.Join(parts, " "))
	line(`code=$?`)
	b.WriteString(claudeCapture(paths))
	line(`exit "$code"`)
	return "sh -c " + shellQuote(b.String())
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
// line or paragraph separator"). On, a `-p` transcript past 5 MB loses the
// history before its last compaction — which a resume restores whether or not
// the file was trimmed — so a session carried between boxes stays near that
// size rather than growing with every compaction it outlives. On an older
// release it is UNSET, whatever set it: a trim that loses messages loses them
// from the session a later run continues.
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

// claudeNotesGate is the part of a run's script wired to its notes that sets
// the script's arguments by the harness's release ([claudeRelease]): on a release
// at or after [claudeNotesFloor] they are `--settings <settings>` and the run's
// standing instructions told of notes (`--append-system-prompt-file <notes>`,
// [Paths.ClaudeNotesSystem]); on an older one, which the run's error stream
// says, the standing instructions alone (plain, [Paths.System]) — nothing for a
// run that has none.
//
// THE SYSTEM TEXT IS CHOSEN WITH THE HOOKS, because only this script knows
// whether a note can reach the run: told of notes, a run started without the
// hooks that read them would be told of a channel that never opens.
func claudeNotesGate(settings, notes, plain string) string {
	older := "set --"
	if plain != "" {
		older = "set -- --append-system-prompt-file " + shellQuote(plain)
	}
	return "if at " + floorArgs(claudeNotesFloor) + "; then set -- --settings " + shellQuote(settings) +
		" --append-system-prompt-file " + shellQuote(notes) + "; else " + older + "; " +
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
// is kept. Restored or not, the archive is removed: nothing reads it again.
// Where the session is neither held nor restored, the run starts the session
// minted for it in its place and says why, on its error stream and in its
// record, rather than resume a session the harness does not hold, which fails the
// run.
func claudeSessionGate(session Session, paths Paths) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := shellQuote
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
	line(`if [ -n "$h" ] && [ -n "$d" ] && ! grep -qxF "$s $d" %s 2> /dev/null; then h=; fi`, digests)
	line(`if [ -z "$h" ]; then`)
	line(`  if [ ! -s %s ]; then w='it is not in this box, and no archive of it was carried here'`, carried)
	line(`  elif ! at %s; then w="this box runs Claude Code $n, older than %s, the oldest that continues a session carried from another box"`,
		floorArgs(claudeCarryFloor), claudeCarryFloor)
	line(`  elif ! command -v tar > /dev/null 2>&1 || ! command -v gzip > /dev/null 2>&1; then w='this box has no tar and gzip to restore its archive with'`)
	line(`  elif ! { %s; }; then w='its archive could not be restored'`, restore)
	line(`  else [ -z "$d" ] || printf '%%s %%s\n' "$s" "$d" >> %s`, digests)
	line(`  fi`)
	line(`  rm -rf %s`, q(staged))
	line(`fi`)
	line(`rm -f %s`, carried)
	line(`if [ -z "$w" ]; then`)
	line(`  set -- "$@" --resume %s`, q(session.ID))
	line(`  if at %s; then set -- "$@" --system-prompt-snapshot off; fi`, floorArgs(claudeSnapshotFloor))
	line(`else`)
	line(`  s=%s k=no`, q(session.Fresh))
	line(`  printf '%%s%%s\n' %s "$w" >&2`, q("crewlet: this run was asked to continue the Claude Code session "+session.ID+
		", and starts a new session, "+session.Fresh+", instead: "))
	line(`  set -- "$@" --session-id %s`, q(session.Fresh))
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
// one the box's copy of the session holds ([Paths.SessionDigests]).
//
// NEVER ANYTHING ELSE: not the login (`.credentials.json`) or the harness's own
// state (`.claude.json`), which the pinned configuration directory holds beside
// them, and not `file-history/`, a checkpoint of the files the run edited,
// which carries the checkout rather than the conversation and is off.
//
// NOTHING TO PACK IS SAID NOWHERE: a harness that wrote no transcript — one that
// failed as it started, a release that files it elsewhere — has no session to
// carry, and a line saying so would end the failure of a run that crashed at
// its start with the engine's words rather than the harness's. A box without tar
// and gzip, or a pack that failed, is said on the error stream, and the run's
// own status is never the pack's.
func claudeCapture(paths Paths) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := shellQuote
	cfg := paths.ClaudeConfigDir()
	project := "projects/" + claudeProjectDir
	staged := paths.SessionStaging()
	line(`if [ -s %s/"$s".jsonl ]; then`, q(cfg+"/"+project))
	line(`  if command -v tar > /dev/null 2>&1 && command -v gzip > /dev/null 2>&1; then`)
	line(`    set -- %s/"$s".jsonl`, q(project))
	line(`    if [ -d %s/"$s" ] && [ ! -L %s/"$s" ]; then set -- "$@" %s/"$s"; fi`,
		q(cfg+"/"+project), q(cfg+"/"+project), q(project))
	line(`    if [ -d %s/"$s" ] && [ ! -L %s/"$s" ]; then set -- "$@" tasks/"$s"; fi`,
		q(cfg+"/tasks"), q(cfg+"/tasks"))
	line(`    rm -rf %s; if mkdir -p %s && tar -czf %s -C %s "$@" < /dev/null > /dev/null 2> %s && mv -f %s %s; then d=$(cksum < %s 2> /dev/null) && printf '%%s %%s\n' "$s" "$d" >> %s; else echo "crewlet: the session $s was not packed to be continued in another box: $(cat %s 2> /dev/null)" >&2; fi`,
		q(staged), q(staged), q(staged+"/capture"), q(cfg), q(staged+"/error"),
		q(staged+"/capture"), q(paths.SessionCapture()), q(paths.SessionCapture()),
		q(paths.SessionDigests()), q(staged+"/error"))
	line(`    rm -rf %s`, q(staged))
	line(`  else`)
	line(`    echo 'crewlet: this box has no tar and gzip, so the session was not packed to be continued in another box' >&2`)
	line(`  fi`)
	line(`fi`)
	return b.String()
}

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

// WriteConfig writes the run's system text and its settings file where
// [ClaudeCode.Command] names them, and renders the scoped MCP surface — each
// only when there is one: the settings for a run wired to its turn's notes
// ([sandbox.RunRequest.Steer]), which every seat's executor run is, and the
// assistant's deep work is not.
//
// A RUN WIRED TO ITS NOTES HAS ITS SYSTEM TEXT TWICE: as it stands
// ([Paths.System]), and told of notes ([Paths.ClaudeNotesSystem]) — which of the
// two it is started with is its box's to decide, by its harness's release
// ([claudeNotesGate]).
//
// The settings file is written afresh, at the mode a box writes every file with
// (0600 on a local box): a box write replaces a file whole
// ([sandbox.Sandbox.WriteFile]), and the run's artefacts are cleared before it
// starts besides ([Runner.ClearArtifacts]), so a previous run's wider file is
// never kept.
func (c ClaudeCode) WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths) (string, error) {
	if req.System != "" {
		if err := box.WriteFile(ctx, paths.System(), []byte(req.System)); err != nil {
			return "", fmt.Errorf("executor: writing the system prompt: %w", err)
		}
	}
	if req.Steer.Set() {
		settings, err := claudeSteerSettings(req.Steer)
		if err != nil {
			return "", fmt.Errorf("executor: rendering Claude Code's settings: %w", err)
		}
		if err := box.WriteFile(ctx, paths.ClaudeSettings(), settings); err != nil {
			return "", fmt.Errorf("executor: writing Claude Code's settings: %w", err)
		}
		told := withNoteInstruction(req.System, c.NoteInstruction())
		if err := box.WriteFile(ctx, paths.ClaudeNotesSystem(), []byte(told)); err != nil {
			return "", fmt.Errorf("executor: writing the system prompt told of notes: %w", err)
		}
	}
	if len(req.MCPServers) == 0 {
		return "", nil
	}
	servers := make(map[string]any, len(req.MCPServers))
	for name, s := range req.MCPServers {
		servers[name] = claudeCodeMCP(s)
	}
	blob, err := json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
	if err != nil {
		return "", err
	}
	if err := box.WriteFile(ctx, paths.MCPConfig(), blob); err != nil {
		return "", err
	}
	return paths.MCPConfig(), nil
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
func claudeCodeMCP(s sandbox.MCPServer) map[string]any {
	if s.Transport == sandbox.TransportHTTP {
		out := map[string]any{"type": string(sandbox.TransportHTTP), "url": s.URL}
		if len(s.Headers) > 0 {
			out["headers"] = s.Headers
		}
		return out
	}
	out := map[string]any{"command": s.Command, "args": s.Args}
	if len(s.Env) > 0 {
		out["env"] = s.Env
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
// variables that switch hooks and the bridge off UNSET before the harness starts
// ([claudeSwitchesOff]); and the session's layout PINNED
// ([claudeSessionLayout]).
func (ClaudeCode) Output(paths Paths) Output {
	return Output{Stdout: paths.Stream(), Events: true, Result: paths.Result(),
		BriefOnStdin: true, Unset: claudeSwitchesOff, Pin: claudeSessionLayout(paths)}
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
// login shell sources set ([Output.Pin]):
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
//     session carried between boxes would carry too.
//
// The transcript's trim (CLAUDE_CODE_TRANSCRIPT_LOCAL_GC) is the script's to
// set, by release ([claudeLocalGCFloor]).
func claudeSessionLayout(paths Paths) []Pinned {
	return []Pinned{
		{Name: "CLAUDE_CONFIG_DIR", Value: paths.ClaudeConfigDir()},
		{Name: "CLAUDE_CODE_PROJECT_DIR_NAME", Value: claudeProjectDir},
		{Name: "CLAUDE_CODE_DISABLE_AUTO_MEMORY", Value: "1"},
	}
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
		res.CacheReadTokens = u.CacheReadInputTokens
		res.CacheWriteTokens = u.CacheCreationInputTokens
		res.InputTokens = u.InputTokens + res.CacheReadTokens + res.CacheWriteTokens
		res.OutputTokens = u.OutputTokens
	}
	if !res.Success {
		res.Error = msg.failure()
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
// Decoded into named fields so the stream's decoder and [ClaudeCode.Parse] read
// one shape, and TOLERANTLY (a field of an unexpected type is left zero and
// the rest still read), because a field the vendor reshapes must cost that
// field rather than the run's whole account.
type claudeResult struct {
	Type         string   `json:"type"`
	Subtype      string   `json:"subtype"`
	IsError      bool     `json:"is_error"`
	Result       string   `json:"result"`
	Errors       []string `json:"errors"`
	SessionID    string   `json:"session_id"`
	TotalCostUSD float64  `json:"total_cost_usd"`
	// NumTurns is how many turns the run's agent loop took — one model
	// call each — on every subtype, a run stopped by its turn cap included.
	NumTurns int `json:"num_turns"`
	// TerminalReason is why the run's loop ended, beside the subtype:
	// "completed" for a loop that finished, and [claudeHookStopped] for one a
	// hook stopped. Absent on a harness that does not report it.
	TerminalReason string `json:"terminal_reason"`
	Usage          *struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
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
	SinceLastResult map[string]claudeModelUsage `json:"model_usage_since_last_result"`
}

// claudeModelUsage is one model's spend as Claude Code's result states it per
// model, in camelCase as `modelUsage` and its per-run sibling print it.
type claudeModelUsage struct {
	InputTokens              int     `json:"inputTokens"`
	OutputTokens             int     `json:"outputTokens"`
	CacheReadInputTokens     int     `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int     `json:"cacheCreationInputTokens"`
	CostUSD                  float64 `json:"costUSD"`
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
		if why := strings.TrimSpace(firstNonBlank(strings.Join(r.Errors, "\n"), r.Result)); why != "" {
			stopped += ": " + why
		}
		return stopped
	}
	how := r.Subtype
	if how == "success" {
		how = ""
	}
	why := strings.TrimSpace(firstNonBlank(strings.Join(r.Errors, "\n"), r.Result))
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

// decodeClaudeResult reads the result message: the one line the result file
// holds, which the wrapper copied from the end of the stream.
func decodeClaudeResult(text string) (claudeResult, bool) {
	if !strings.HasPrefix(text, "{") {
		return claudeResult{}, false
	}
	var msg claudeResult
	err := json.Unmarshal([]byte(text), &msg)
	var mismatch *json.UnmarshalTypeError
	// A syntax error is reported before any field is decoded, so a mismatch
	// means a whole object with one field of another type.
	if err == nil || errors.As(err, &mismatch) {
		return msg, true
	}
	return claudeResult{}, false
}

// lastLine is the text's last line, trimmed.
func lastLine(text string) string {
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		text = text[i+1:]
	}
	return strings.TrimSpace(text)
}
