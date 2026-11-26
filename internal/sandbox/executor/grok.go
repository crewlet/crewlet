package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/BurntSushi/toml"
	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

func init() { Register(Grok{}) }

// GrokName is this runner's config name.
const GrokName = "grok"

// Grok drives xAI's Grok Build headless (`grok --prompt-file
// --output-format streaming-json`), measured on 1.0.92.
//
// A run loads what a person's Grok loads from its home and repository, its
// sign-in in its shell as in a person's session; the box is the boundary. Its
// model is reached by XAI_API_KEY or the login a local box is seeded with
// (`.grok/auth.json`). Its settings are the box's with its own tables laid in
// ([grokMergeConfig]), and its standing instructions are an agent file's body
// ([grokAgentFile]). Its session is a directory, packed with its subagents'
// and restored by its script ([grokSessionGate], [grokPack]). Every run is
// `--always-approve`: a headless run cannot ask. Its end on request is
// end.go's protocol: SIGTERM ends it with 143, its session kept.
type Grok struct{}

var (
	_ Harness        = Grok{}
	_ carrier        = Grok{}
	_ saver          = Grok{}
	_ artifactHolder = Grok{}
	_ sessionJudge   = Grok{}
	_ inPlaceGate    = Grok{}
	_ trimmer        = Grok{}
)

// NewGrok returns the runner.
func NewGrok() *Runner { return New(Grok{}) }

// Name is the harness's key in the runner registry.
func (Grok) Name() string { return GrokName }

// MCPCallCeiling is none: `tool_timeout_sec` carries the bridge's own bound
// ([grokConfig]), honoured whatever progress flows, and a 1040-second call
// under a 1050-second one was answered whole (measured on 1.0.92).
func (Grok) MCPCallCeiling() time.Duration { return 0 }

// Resumes says Grok Build continues a session it ran: `-r` on an id its
// sessions directory holds under the run's directory resumes it (measured on
// 1.0.92).
func (Grok) Resumes() bool { return true }

// grokSupported is the one Grok Build release this runner is written for, every
// behaviour here measured on it: the release the profile's written_for names
// ([harnessprofile.Release]), so the profile and the runner never name two. An
// older box, or one whose release cannot be read, is refused before anything
// runs; a newer one runs a session of its own and never continues one; and a
// session continues only where its writer and the box both run it exactly
// ([Grok.Continues], [grokSessionGate]).
var grokSupported = sync.OnceValue(func() string {
	p, _ := harnessprofile.Builtin(GrokName)
	return harnessprofile.Release(p.WrittenFor)
})

// Session is the session a Grok Build run works in ([Harness.Session]): the one
// it continues, or a new one under a UUID minted for it, which `-s` starts a
// session under; and, for a run that continues one, a second UUID for the
// session it starts instead where its box holds that one neither way.
func (Grok) Session(req sandbox.RunRequest) Session {
	if req.Resume != "" {
		return Session{ID: req.Resume, Continues: true, Fresh: uuid.NewString(),
			Replaces: req.Carried != nil && req.Carried.Replace}
	}
	return Session{ID: uuid.NewString()}
}

// Command is the run as ONE SCRIPT ([grokScript]), so the wrapper's
// redirections (the stream, the error file) reach each step.
func (Grok) Command(req sandbox.RunRequest, session Session, paths Paths, _ string) string {
	return "sh -c " + shellword.Quote(grokScript(req, session, paths, sandbox.JobStopGrace))
}

// grokDenied is every tool of Grok Build 1.0.92 a run removes by name: those a
// headless run cannot use (ask_user_question, the plan-mode pair) or whose
// calls drop what the run's hooks hand over (workflow); and the scheduler's
// three. Measured on 1.0.92, a scheduled job fires inside a live run, its lines
// land untagged in the run's one stream so its text can replace the run's
// answer, and its turn is a sibling session the run's record does not hold.
// Every other tool stays, spawn_subagent included ([grokPack]). A DENY LIST:
// 1.0.92 applies none of a `--tools` list that names a tool the build lacks
// (measured); the names stay current because a run refuses every release
// older than [grokSupported].
var grokDenied = []string{
	"ask_user_question", "enter_plan_mode", "exit_plan_mode", "workflow",
	"scheduler_create", "scheduler_delete", "scheduler_list",
}

// grokWebTools are Grok Build's own web tools, removed where the entry denies
// web ([sandbox.RunRequest.WebDenied]).
var grokWebTools = []string{"web_search", "web_fetch"}

// grokRouting is every variable measured on 1.0.92 to move or sign a Grok
// run's requests, which reaches `grok` as the box or the entry sets it
// (TestAGrokRunsRequestsGoWhereAPersonsWould).
var grokRouting = slices.Concat(routes(roleEndpoint, "GROK_XAI_API_BASE_URL", "GROK_MODELS_BASE_URL"),
	routes(roleSigns, "GROK_CODE_XAI_API_KEY", "GROK_AUTH_PROVIDER_COMMAND"))

// routing answers [router] with [grokRouting].
func (Grok) routing() []routeVar { return grokRouting }

// grokScript is a Grok Build run's script, giving its harness stop to go once
// asked to end: the end protocol (end.go); the release read and held to
// [grokSupported]; the session named, restored where it was carried, handed
// this launch's standing instructions where it continues, and recorded
// ([grokSessionGate], [Paths.SessionRecord]); the harness run in the box home
// under the end protocol ([launchHarness]); and its session packed
// ([grokCapture]), exiting with the harness's status.
//
// THE BRIEF IS A FILE: `--prompt-file` reads it byte for byte.
func grokScript(req sandbox.RunRequest, session Session, paths Paths, stop time.Duration) string {
	q := shellword.Quote
	denied := grokDenied
	if req.WebDenied {
		denied = append(append([]string(nil), denied...), grokWebTools...)
	}
	parts := []string{
		"grok", "--output-format", "streaming-json",
		// No run asks: a headless one that would is cancelled whole.
		"--always-approve", "--no-auto-update",
		"--agent", q(paths.GrokAgent()),
		"--disallowed-tools", q(strings.Join(denied, ",")),
	}
	if req.Model != "" {
		parts = append(parts, "-m", q(req.Model))
	}
	if req.Effort != "" {
		parts = append(parts, "--reasoning-effort", q(req.Effort))
	}
	// THE TURN CAP AS GROK'S OWN: past it a `max_turns_reached` event, then
	// exit 1 ([grokEvents.Result]). Each agent's own: a subagent stops at its
	// cap and its parent goes on (measured on 1.0.92).
	if req.Limits.MaxTurns > 0 {
		parts = append(parts, "--max-turns", strconv.Itoa(req.Limits.MaxTurns))
	}
	parts = append(parts, `--prompt-file "$p" "$sf" "$s"`)

	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	b.WriteString(endPrologue(paths, stop))
	b.WriteString(endHarness(""))
	b.WriteString(grokRelease)
	line(`if [ -z "$n" ] || ! at %s; then printf 'crewlet: this box runs Grok Build %%s, and this runner runs Grok Build %s or newer (a release it cannot read counts as older): install Grok Build %s\n' "${r:-that prints no release}" >&2; exit 1; fi`,
		floorArgs(grokSupported()), grokSupported(), grokSupported())
	line(`p=%s`, q(paths.Brief()))
	b.WriteString(grokSessionGate(session, paths, freshSessionWiring(req, session) != nil))
	line(`printf 'id %%s\ncontinued %%s\nwhy %%s\nrelease %%s\n' "$s" "$k" "$w" "$v" > %s`,
		q(paths.SessionRecord()))
	// THE MAIN SESSION NAMED for the note hooks, which fire in a subagent's
	// session too and take or report only in this one ([Paths.GrokThread]).
	if req.Steer.Set() || req.SkillLoads.Set() {
		line(`printf '%%s\n' "$s" > %s || echo 'crewlet: the session could not be named to the note hooks, so the run takes no notes and reports no skill load' >&2`,
			q(paths.GrokThread()))
	}
	// THE NOTE HELPER EXECUTABLE, which every hook runs by its path: the box
	// writes it as a plain file ([writeNoteHelper]).
	if req.Steer.Set() || req.SkillLoads.Set() {
		line(`chmod 0755 %s || echo 'crewlet: the note helper could not be made executable, so the run takes no notes and reports no skill load' >&2`,
			q(paths.GrokNoteHelper()))
	}
	// IN THE BOX HOME, whose key its sessions are filed under ([Paths.GrokSessionsKey]).
	line(`cd %s || { echo 'crewlet: the box home could not be entered' >&2; exit 1; }`, q(paths.Home))
	// THE GUARD, in the run's directory and environment, before Grok starts: a
	// refused run puts the box's settings back as an ended one does.
	b.WriteString(grokGuard(paths))
	b.WriteString(`grokguard || { ` + strings.TrimSuffix(grokRestoreConfig(paths), "\n") + "; exit 1; }\n")
	// THE CHECKOUTS BELOW THE START DIRECTORY ([Grok.checkoutHome]), and once
	// grok has stopped, on a failure before and on a teardown's signal,
	// ~/.grok/AGENTS.md back, the copies gone, what a firing left out said
	// and the box's own settings back ([grokRestoreConfig]), each whatever
	// came of the others ([everyStep]); a run killed outright leaves the
	// put-back to the next run's clear ([homeKeeper]) and the settings to the
	// next run, which starts from the box's own kept copy.
	home := Grok{}.checkoutHome(paths)
	line(`grokput() { %s; }`, everyStep(home.restore(), home.saySkipped(),
		strings.TrimSuffix(grokRestoreConfig(paths), "\n")))
	line(`grokquit() { trap '' USR1; trap - EXIT TERM INT HUP; end "$harness"; harness=; grokput; exit "$1"; }`)
	line(`trap grokput EXIT; trap 'grokquit 143' TERM; trap 'grokquit 130' INT; trap 'grokquit 129' HUP`)
	line(`mkdir -p %s || exit 1`, q(paths.GrokHome()))
	b.WriteString(home.gate())
	b.WriteString(grokNestedOff())
	b.WriteString(launchHarness(strings.Join(parts, " "), paths))
	line(`grokput; trap - EXIT TERM INT HUP`)
	b.WriteString(grokMainRecord(paths))
	b.WriteString(grokCapture(paths))
	line(`exit "$code"`)
	return b.String()
}

// grokRestoreConfig is the part of a run's script that puts the box's own
// ~/.grok/config.toml back once its harness has exited ([grokBoxConfig]): the
// file kept, or none where it is empty, and the kept copy then removed, so no
// bridge of the run's stays in the box's settings and an edit to them between
// runs stands. A run cut off before it leaves the kept copy, which the next
// run starts from.
func grokRestoreConfig(paths Paths) string {
	q := shellword.Quote
	return fmt.Sprintf(`if [ -f %[1]s ]; then { if [ -s %[1]s ]; then cp %[1]s %[2]s && mv -f %[2]s %[3]s; else rm -f %[3]s; fi; } && rm -f %[1]s %[4]s; fi
`, q(paths.GrokConfigBox()), q(paths.GrokConfig()+".next"), q(paths.GrokConfig()), q(paths.GrokConfigOurs()))
}

// grokMainRecord is the part of a run's script that adds to its stream, once
// its harness has exited, the mark ([grokRecordMark]) and the main session's
// record lines its answer is read from ([grokEvents.record]): each request
// and each prompt. The stream's lines carry no agent, so a subagent's words
// there can stand last (measured on 1.0.92); the record is the main agent's
// alone. Nothing where the session left no record; the mark on a line of its
// own, after a stream a killed harness left mid-line.
func grokMainRecord(paths Paths) string {
	return fmt.Sprintf(`x=%s/"$s"
if [ -f "$x/chat_history.jsonl" ] && [ ! -L "$x" ]; then printf '\n%%s\n' %s; grep -F -e '"type":"assistant"' -e '"prompt_index"' "$x/chat_history.jsonl" 2> /dev/null; fi
`, shellword.Quote(paths.GrokSessionsKey()), shellword.Quote(`{"type":"`+grokRecordMark+`"}`))
}

// grokRelease opens every Grok Build run's script: `grok --version` read once
// as v (`grok 1.0.92 (b06756fd6256)`), its second word as r, the numbers that
// open r as n, and `at MAJOR MINOR PATCH`, true where n is that release or
// newer. A suffix after the numbers is a pre-release's, older than its release,
// so `at` holds n with one to the next release down. n is empty where r opens
// with no MAJOR.MINOR.PATCH.
const grokRelease = `v=$(grok --version < /dev/null 2> /dev/null | head -n 1); r=${v#grok }; r=${r%% *}
n=${r%%[!0-9.]*}; a=${n%%.*}; t=${n#*.}; b=${t%%.*}; c=${t#*.}
case "$n" in [0-9]*.[0-9]*.[0-9]*) ;; *) n= ;; esac
for z in "$a" "$b" "$c"; do case "$z" in ''|*[!0-9]*) n= ;; esac; done
at() { [ "$a" -gt "$1" ] || { [ "$a" -eq "$1" ] && { [ "$b" -gt "$2" ] || { [ "$b" -eq "$2" ] && { [ "$c" -gt "$3" ] || { [ "$c" -eq "$3" ] && [ "$r" = "$n" ]; }; }; }; }; }; }
`

// grokSessionGate is the part of a run's script that names the session its
// harness runs in (s), with `-s` for one it starts or `-r` for one it
// continues (sf), whether it continues the one asked (k) and, where it could
// not, why (w). As [museSessionGate]: the box's copy is continued unless a
// carried archive holds what it does not (a digest not in
// [Paths.SessionDigests]); that archive is restored under THIS box's key
// ([Paths.GrokSessionsKey]: 1.0.92 resumes a session in the directory its key
// names, measured) once it holds the session's record and no link, its digest
// recorded; the session held, its record is handed this launch's standing
// instructions ([grokRenewRules]); neither held nor restored, or not renewed,
// the minted session starts and says why, on the brief a new session is handed.
func grokSessionGate(session Session, paths Paths, rewired bool) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := shellword.Quote
	if !session.Continues {
		line(`s=%s sf=-s k=no w=`, q(session.ID))
		return b.String()
	}
	staged := paths.SessionStaging()
	carried := q(paths.SessionCarried())
	in := staged + "/in"
	key, sessions := q(paths.GrokSessionsKey()), q(paths.GrokSessions())
	restore := strings.Join([]string{
		"rm -rf " + q(staged),
		"mkdir -p " + q(in) + " " + key,
		"tar -xzf " + carried + " -C " + q(in) + " < /dev/null > /dev/null 2>&1",
		// THE MAIN SESSION'S GROUP is its first member's, the writer's key.
		`mg=$(tar -tzf ` + carried + ` 2> /dev/null | head -n 1)`, `mg=${mg%%/*}`,
		`case $mg in ''|.|..|*[!0-9A-Za-z%._~-]*) false;; esac`,
		`y=` + q(in) + `/"$mg/$s"`,
		`[ -d "$y" ]`, `[ ! -L "$y" ]`, `[ -f "$y/chat_history.jsonl" ]`,
		// NO LINK ANYWHERE IN IT: the session's files are moved into the home
		// and written by the harness, so one could reach past it.
		`[ -z "$(find ` + q(in) + ` -type l 2> /dev/null)" ]`,
		// THE BOX'S OWN COPY GOES only now, once the archive is known to hold
		// a conversation; then each subagent's session it carries ([grokPack]),
		// under this box's key where it was filed beside the main one, else
		// under its own group.
		`rm -rf ` + key + `/"$s"`,
		`mv "$y" ` + key + `/"$s"`,
		`ge=`,
		`for gc in ` + q(in) + `/*/*; do [ -d "$gc" ] || continue; gn=${gc##*/}; cg=${gc%/*}; cg=${cg##*/}; ` +
			`case $cg/$gn in .*|*/.*|*[!0-9A-Za-z%._~/-]*) ge=1; break;; esac; ` +
			`if [ "$cg" = "$mg" ]; then gt=` + key + `; else gt=` + sessions + `/"$cg"; fi; ` +
			`{ mkdir -p "$gt" && rm -rf "$gt/$gn" && mv "$gc" "$gt/$gn"; } || { ge=1; break; }; done`,
		`[ -z "$ge" ]`,
	}, " && ")
	line(`s=%s sf=-r k=yes w= h= d=`, q(session.ID))
	line(`x=%s/"$s"; [ -s "$x/chat_history.jsonl" ] && [ ! -L "$x" ] && h=1`, key)
	// A SESSION CONTINUES ON THE SUPPORTED RELEASE ALONE, read bare as the
	// engine reads it ([releaseOf]): any other, or none that reads, starts a
	// session of its own, saying so as [Grok.continuesInPlace] does.
	line(`%s[ "$rv" = %s ] || w="this box runs Grok Build ${rv:-a release it could not read}, and a session continues only on %s, the release this runner is written for"`,
		bareReleaseShell("rv"), q(grokSupported()), grokSupported())
	line(`if [ -s %s ]; then d=$(cksum < %s 2> /dev/null) || d=; fi`, carried, carried)
	if session.Replaces {
		line(`if [ -n "$h" ] && [ -s %s ]; then h=; fi`, carried)
	} else {
		line(`if [ -n "$h" ] && [ -n "$d" ] && ! grep -qxF "$s $d" %s 2> /dev/null; then h=; fi`,
			q(paths.SessionDigests()))
	}
	line(`if [ -z "$w" ] && [ -z "$h" ]; then`)
	line(`  if [ ! -s %s ]; then w='it is not in this box, and no archive of it was carried here'`, carried)
	line(`  elif ! command -v tar > /dev/null 2>&1 || ! command -v gzip > /dev/null 2>&1; then w='this box has no tar and gzip to restore its archive with'`)
	line(`  elif ! { %s; }; then w='its archive could not be restored'`, restore)
	line(`  elif ! { %s; }; then w=%s`, recordDigest(paths, "s"), q(restoreDigestUnrecorded))
	line(`  fi`)
	line(`  rm -rf %s`, q(staged))
	line(`fi`)
	b.WriteString(grokRenewRules(paths))
	line(`rm -f %s`, carried)
	line(`if [ -z "$w" ]; then`)
	if rewired {
		line(`  rm -f %s %s %s`, q(paths.GrokConfigFresh()), q(paths.GrokTakeConfigFresh()), q(paths.GrokAgentFresh()))
	} else {
		line(`  :`)
	}
	line(`else`)
	line(`  s=%s sf=-s k=no`, q(session.Fresh))
	line(`  printf '%%s%%s\n' %s "$w" >&2`, q("crewlet: this run was asked to continue the Grok Build session "+
		session.ID+", and starts a new session, "+session.Fresh+", instead: "))
	line(`  [ ! -s %s ] || p=%s`, q(paths.FreshBrief()), q(paths.FreshBrief()))
	if rewired {
		// THE NEW SESSION WIRED TO ITS OWN TOKEN, or not started:
		// [claudeSessionGate]'s reason.
		line(`  { mv -f %s %s && mv -f %s %s; } || { echo 'crewlet: the new session could not be wired to the tools of its seat, so the run does not start' >&2; exit 1; }`,
			q(paths.GrokConfigFresh()), q(paths.GrokConfig()), q(paths.GrokAgentFresh()), q(paths.GrokAgent()))
		line(`  if [ -f %s ]; then mv -f %s %s || { echo 'crewlet: the new session could not be wired to its notes, so the run does not start' >&2; exit 1; }; fi`,
			q(paths.GrokTakeConfigFresh()), q(paths.GrokTakeConfigFresh()), q(paths.GrokTakeConfig()))
	}
	line(`fi`)
	return b.String()
}

// grokRulesOpen and grokRulesClose enclose a run's standing instructions at
// the end of Grok Build's system prompt ([grokAgentFile]): the bounds a
// continuing run's script replaces them within ([grokRenewRules]).
const (
	grokRulesOpen  = "<crewlet_seat_instructions>"
	grokRulesClose = "</crewlet_seat_instructions>"
)

// grokRenewRules is the part of a continuing run's gate that hands the session
// it holds this launch's standing instructions, or says why it cannot (w):
// 1.0.92 resumes a session on the system message its record's first line
// holds, `{"type":"system","content":…}`, whatever the launch's agent file says
// (measured), so that line's content is replaced from [grokRulesOpen] on with
// [Paths.GrokRules], this launch's, already a JSON string's content. A line
// with no such bounds, or a record that cannot be rewritten, would resume on
// an earlier launch's instructions, naming servers this run does not have, so
// the gate starts the minted session instead.
func grokRenewRules(paths Paths) string {
	q := shellword.Quote
	record := `"$x/chat_history.jsonl"`
	return fmt.Sprintf(`if [ -z "$w" ]; then
  x=%[1]s/"$s"; o=%[2]s
  if ! head -n 1 %[3]s | grep -q '^{"type":"system","content":".*%[4]s'; then w=%[7]s
  elif ! { u=$(cat %[6]s) && [ -n "$u" ] && { head -n 1 %[3]s | sed 's/%[4]s.*$//' | tr -d '\n'; printf '%%s%%s"}\n' %[5]s "$u"; tail -n +2 %[3]s; } > "$o" && mv -f "$o" %[3]s; }; then rm -f "$o"; w=%[8]s
  fi
fi
`, q(paths.GrokSessionsKey()), q(paths.WorkDir()+"/grok-renewed.jsonl"), record, grokRulesOpen, q(grokRulesOpen),
		q(paths.GrokRules()), q(grokRulesMissing), q(grokRulesUnwritten))
}

// The reasons a continued session is not renewed ([grokRenewRules]), which its
// run says as it starts a session of its own.
const (
	grokRulesMissing = "its record's first line holds no standing instructions of this runner's to replace " +
		"with this launch's"
	grokRulesUnwritten = "its record could not be rewritten with this launch's standing instructions"
)

// grokArchiveFormat is the layout a Grok Build run packs its session in
// ([grokPack]) and the one it restores ([grokSessionGate]): one gzipped tar of
// members named from [Paths.GrokSessions] down, the session's directory,
// `<key>/<session>/`, its record (chat_history.jsonl) first and every other
// regular file under it but the harness's locks (`*.lock`), its `terminal/`
// call logs included, past the bound those of [grokTrimmedDirs] left out; then,
// alike, each subagent's session directory its updates.jsonl names
// (`subagent_spawned`), under the key of the directory it was started in. The
// restore puts the session, and each subagent's filed beside it, under its own
// box's key. A resume needs no file outside it, and one from such an archive
// continues (measured on 1.0.92). A layout an older reader cannot read is a new
// format.
const grokArchiveFormat = 2

// ArchiveFormat is the layout a Grok Build run packs its session in ([carrier]).
func (Grok) ArchiveFormat() int { return grokArchiveFormat }

// VersionCommand prints a box's Grok Build release ([carrier]), `grok 1.0.92
// (b06756fd6256)`, read bare as [releaseOf] reads it.
func (Grok) VersionCommand() string { return "grok --version" }

// layout is a Grok Build archive's: a gzipped tar of the session's files.
func (Grok) layout() archiveLayout { return archiveTar }

// charges is false: its meter counts nothing ([carrier.charges]).
func (Grok) charges(string, string) bool { return false }

// Continues reports whether a session written by Grok Build release writer can
// be continued in a box whose Grok Build is release box ([carrier]): only where
// the box continues any session ([Grok.continuesInPlace]) and the writer is as
// [Grok.ContinuesWriter] holds it.
func (g Grok) Continues(box, writer string) (bool, string) {
	if ok, why := g.continuesInPlace(box); !ok {
		return false, why
	}
	return g.ContinuesWriter(writer)
}

// continuesInPlace reports whether a box whose Grok Build is release box
// continues a session at all, the copy it holds as much as one carried to it
// ([inPlaceGate]): only on [grokSupported], as the run's own gate holds it
// ([grokSessionGate]), so the engine asks before a relaunch hands the brief
// that continues the box's copy.
func (Grok) continuesInPlace(box string) (bool, string) {
	if box == grokSupported() {
		return true, ""
	}
	if box == "" {
		box = "a release it could not read"
	}
	return false, fmt.Sprintf(grokBoxRefused, box, grokSupported())
}

// grokBoxRefused is why a box whose Grok Build is not [grokSupported]
// continues no session, its own copy nor a carried one: the engine's check
// and the run's gate say it alike.
const grokBoxRefused = "this box runs Grok Build %.64s, and a session continues only on %s, the release this " +
	"runner is written for"

// ContinuesWriter is [Grok.Continues] judged of the writer alone ([carrier]):
// only a session [grokSupported] wrote; one written by any other release, or
// by one that named no release this build reads, is refused, saying so.
func (Grok) ContinuesWriter(writer string) (bool, string) {
	switch writer {
	case grokSupported():
		return true, ""
	case "":
		return false, writerNamedNoRelease("Grok Build")
	}
	return false, fmt.Sprintf("the session was written by Grok Build %.64s, and only a session %s wrote "+
		"continues, the release this runner is written for", writer, grokSupported())
}

// meter is the take's meter of a Grok Build archive ([carrier.meter]): no
// figure. This build reads no count or estimate of a Grok session's next
// request off its archive ([Grok.sessionTraits]). The archive is left to the
// caller, which drains it ([startMeter]).
func (Grok) meter(context.Context, io.Reader, string, string, *passBudget) (meterReading, error) {
	return meterReading{}, nil
}

// grokCapture is the part of a run's script that packs its session once its
// harness has exited ([grokPack]), trimmed by the box's bare release
// ([trimRelVar], read off v as [releaseOf] reads it).
func grokCapture(paths Paths) string {
	return bareReleaseShell(trimRelVar) + grokPack(paths, packAt{staged: paths.SessionStaging(), dest: paths.SessionCapture()})
}

// grokPack is ONE PACK, the run's end's, a pack on demand's and a save's, at
// the bounds a take keeps ([trimAt]): [grokPackAt].
func grokPack(paths Paths, at packAt) string { return grokPackAt(paths, at, trimAt) }

// grokPackAt packs the session s names into at.staged and moves it over
// at.dest, its digest kept ([packDigest]) and its trim past trim said beside it
// ([grokTrimTar], [trimMarked]): the members [grokArchiveFormat] names, its
// record first, each named on tar's command line so no lock or link is
// packed. A pack beside a working harness is held to [claudeHeldTar]'s rule,
// which reads the first member as the session's transcript.
func grokPackAt(paths Paths, at packAt, trim trimPoints) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := shellword.Quote
	sessions := paths.GrokSessions()
	line(`x=%s/"$s"`, q(paths.GrokSessionsKey()))
	line(`if [ -s "$x/chat_history.jsonl" ] && [ ! -L "$x" ]; then`)
	line(`  if command -v tar > /dev/null 2>&1 && command -v gzip > /dev/null 2>&1; then`)
	// THE FILES, ONE PER LINE, as find names them under the sessions directory:
	// its names are Grok's own (a key, the session id, its files,
	// `terminal/call_<id>.log`). Split on newlines alone, unglobbed, then IFS
	// back to the shell's default. Each subagent the record's updates name
	// follows, by an id of Grok's own characters, once, from the main
	// session's key or else the key of the directory it was started in, and
	// only where its directory holds a record.
	list := fmt.Sprintf(`g=%[3]s && { l=$(cd %[1]s && find "$g/$s" -type f ! -name '*.lock' ! -path "$g/$s/chat_history.jsonl" 2> /dev/null) || `+
		`{ echo "the session's files could not be listed" > %[2]s; false; }; } && `+
		`{ gs=; [ ! -f "$x/updates.jsonl" ] || gs=$(sed -n 's/.*"sessionUpdate":"subagent_spawned".*"child_session_id":"\([^"]*\)".*/\1/p' "$x/updates.jsonl" 2> /dev/null) || `+
		`{ echo "the session's subagents could not be listed" > %[2]s; false; }; } && set -- "$g/$s/chat_history.jsonl" && `+
		`IFS=$(printf '\n.') && IFS=${IFS%%.} && set -f && for y in $l; do set -- "$@" "$y"; done && `+
		`ge= && gu= && for gc in $gs; do case $gc in ''|"$s"|*[!0-9A-Za-z_-]*) continue;; esac; `+
		`case $gu in *"/$gc/"*) continue;; esac; gu="$gu/$gc/"; `+
		`if [ -d %[1]s/"$g/$gc" ]; then gd="$g/$gc"; else gd=$(cd %[1]s && find . -mindepth 2 -maxdepth 2 -type d -name "$gc" 2> /dev/null | head -n 1); gd=${gd#./}; fi; `+
		`[ -n "$gd" ] && [ -f %[1]s/"$gd/chat_history.jsonl" ] && [ ! -L %[1]s/"$gd" ] || continue; `+
		`gm=$(cd %[1]s && find "$gd" -type f ! -name '*.lock' ! -path "$gd/chat_history.jsonl" 2> /dev/null) || `+
		`{ echo "the files of the session's subagent $gc could not be listed" > %[2]s; ge=1; break; }; `+
		`set -- "$@" "$gd/chat_history.jsonl"; for y in $gm; do set -- "$@" "$y"; done; done && [ -z "$ge" ] && `+
		`set +f && IFS=$(printf ' \t\n.') && IFS=${IFS%%.}`,
		q(sessions), q(at.staged+"/error"), q(grokPathKey(paths.Home)))
	tar := fmt.Sprintf(`{ tar -czf %s -C %s "$@" < /dev/null > /dev/null 2> %s; t=$?; [ "$t" = 0 ] || { [ "$t" = 1 ] && grep -q 'changed as we read it' %s; }; }`,
		q(at.staged+"/capture"), q(sessions), q(at.staged+"/error"), q(at.staged+"/error"))
	if at.working {
		tar = claudeHeldTar(sessions, at.staged)
	}
	tar = grokTrimTar(trim, q(sessions), q(at.staged+"/capture"), tar)
	line(`    rm -rf %s; if mkdir -p %s && %s && %s && %s && %s && mv -f %s %s; then packed=1; %s; else echo "crewlet: the session $s was not packed to be continued in another box: $(cat %s 2> /dev/null)" >&2; fi`,
		q(at.staged), q(at.staged), list, tar, packDigest(paths, "s", at.staged), trimUnmark(at.dest),
		q(at.staged+"/capture"), q(at.dest), trimMarked(at.dest), q(at.staged+"/error"))
	line(`    rm -rf %s`, q(at.staged))
	line(`  else`)
	line(`    echo 'crewlet: this box has no tar and gzip, so the session was not packed to be continued in another box' >&2`)
	line(`  fi`)
	line(`fi`)
	return b.String()
}

// Pack packs session now ([carrier]), as a run's script does once its harness
// exits, into staged and over dest: [packNothing] where the box holds no
// record of it, [packFailed] where no archive was made. Held to the rule of a
// pack beside a working harness where working says so.
func (Grok) Pack(paths Paths, session, staged, dest string, working bool) string {
	return grokOnDemand(paths, session, packAt{staged: staged, dest: dest, working: working})
}

// PackSave packs a save of rec's session beside a harness that may still be
// working ([saver]), held to the rule of a pack beside a working harness: a
// file rewritten as it packs refuses that save, which the next one retries.
// The record refuses no save.
func (Grok) PackSave(paths Paths, rec sessionRecord, staged, dest string) (string, error) {
	return grokOnDemand(paths, rec.id, packAt{staged: staged, dest: dest, working: true}), nil
}

// grokOnDemand is a pack the engine makes of session outside the run's own
// script, at at.
func grokOnDemand(paths Paths, session string, at packAt) string {
	return "s=" + shellword.Quote(session) + " packed=\n" + grokPack(paths, at) +
		fmt.Sprintf("[ -s \"$x/chat_history.jsonl\" ] || exit %d\n[ -n \"$packed\" ] || exit %d\n",
			packNothing, packFailed)
}

// grokSwitchesOff is every variable that would take a Grok Build run from what
// its runner drives and reads, removed whatever set it: GROK_HOME moves its
// settings and sessions away from the paths this runner writes, packs and
// seeds ([Paths.GrokHome]); GROK_CONFIG and GROK_CONFIG_PATH move or lay
// settings over the file that holds the run's bridge and hooks; and
// GROK_AUTO_PERMISSION_MODE and GROK_LEADER_SOCKET change who decides a call,
// which a headless run cannot ask, and which process runs it (the harness's
// user guide).
var grokSwitchesOff = []string{"GROK_HOME", "GROK_CONFIG", "GROK_CONFIG_PATH", "GROK_AUTO_PERMISSION_MODE",
	"GROK_LEADER_SOCKET"}

// Output is where a job writes under `--output-format streaming-json`: its
// stream is its result, decoded whole or followed ([grokEvents]).
func (Grok) Output(paths Paths) Output {
	return Output{Stdout: paths.Result(), Events: true, Unset: grokSwitchesOff}
}

// Pins is what the runner needs of a run, over whatever the seat's env, the
// image or a profile set: no update, since a session continues only on the
// release that wrote it ([grokSupported]); and no turn summary, a model
// request after the answer whose spend the stream states nowhere (each
// measured on 1.0.92). Grok keeps every other default, its fetch's among them:
// a web-denied run removes the web tools by name ([grokWebTools]).
func (Grok) Pins(context.Context, sandbox.RunRequest, Paths) []Pinned {
	return []Pinned{{Name: "GROK_DISABLE_AUTOUPDATER", Value: "1"}, {Name: "GROK_TURN_SUMMARY", Value: "0"}}
}

// Events is a new decoder for one stream, read whole or followed ([Decoder]).
func (Grok) Events() Decoder { return &grokEvents{} }

// Parse decodes a whole stream already in hand: the result of a run whose
// result IS its stream.
func (Grok) Parse(stdout []byte) sandbox.Result { return decodeAll(&grokEvents{}, stdout) }

// grokServerName is name with its session's nonce ([sessionNonce]), which no
// file a repository or image holds can name beforehand (a repository's
// `.grok/config.toml` naming a server replaces it, measured on 1.0.92), as
// Grok Build names a server's tools (`<server>__<tool>`), and false for a
// name it would skip: one that opens with neither a letter nor `_`, holds
// anything but letters, digits, `-` and `_`, or holds `__`, which would split
// the catalogue key twice.
func grokServerName(name, nonce string) (string, bool) {
	full := name + "_" + nonce
	if name == "" || strings.Contains(full, "__") {
		return "", false
	}
	for i, r := range full {
		letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_'
		if i == 0 && !letter {
			return "", false
		}
		if !letter && (r < '0' || r > '9') && r != '-' {
			return "", false
		}
	}
	return full, true
}

// grokNames is each of servers' names with nonce, as the run's settings
// write them ([grokServerName]).
func grokNames(servers map[string]sandbox.MCPServer, nonce string) []string {
	var named []string
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		if full, ok := grokServerName(name, nonce); ok {
			named = append(named, full)
		}
	}
	return named
}

// grokToolsInstruction is what a Grok Build run is told of its MCP servers:
// 1.0.92 offers no server's tool by name, only search_tool and use_tool, and
// tells its model no more than that a server connected (measured).
func grokToolsInstruction(servers []string) string {
	if len(servers) == 0 {
		return ""
	}
	var named []string
	for _, s := range servers {
		named = append(named, "`"+s+"`")
	}
	return "## Your seat's tools\n\nYour seat's tools are on the MCP server" + plural(len(named)) + " " +
		strings.Join(named, ", ") + ". Grok Build reaches them through two tools of its own: call `search_tool` " +
		"to find one and read its input, then `use_tool` with its name as `<server>__<tool>` (for instance `" +
		servers[0] + "__<tool>`)."
}

// plural is "s" for more than one.
func plural(n int) string {
	if n > 1 {
		return "s"
	}
	return ""
}

// grokTOMLString is s as a TOML basic string: a quote, a backslash and every
// control character escaped, the rest as it is.
func grokTOMLString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// grokTOMLList is values as a TOML array of strings.
func grokTOMLList(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = grokTOMLString(v)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// grokConfig is the run's own tables of its ~/.grok/config.toml, which
// [grokMergeConfig] lays into the box's: the key 1.0.92 adds on a home's first
// call, so no call rewrites the file; each of the run's MCP servers under its
// name with the run's nonce; and the note hooks ([grokHooks]).
func grokConfig(servers map[string]sandbox.MCPServer, nonce string, hooks string) (string, error) {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line("[marketplace]\ndefault_skills_installs_purged = true\n")
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		full, ok := grokServerName(name, nonce)
		if !ok {
			return "", fmt.Errorf("executor: the MCP server %q is not named as Grok Build reads a server's name: "+
				"a letter or '_' first, then letters, digits, '-' and '_', never '__' (rename it in the seat's "+
				"MCP servers)", name)
		}
		s := servers[name]
		line("[mcp_servers.%s]", full)
		if s.Transport == sandbox.TransportHTTP {
			line("url = %s", grokTOMLString(s.URL))
		} else {
			line("command = %s", grokTOMLString(s.Command))
			line("args = %s", grokTOMLList(s.Args))
		}
		if s.CallTimeout > 0 {
			// WHOLE SECONDS, rounded up, so the harness never cancels a call
			// the engine still waits for.
			line("tool_timeout_sec = %d", int64((s.CallTimeout+time.Second-1)/time.Second))
		}
		if len(s.Headers) > 0 {
			line("[mcp_servers.%s.headers]", full)
			for _, k := range slices.Sorted(maps.Keys(s.Headers)) {
				line("%s = %s", grokTOMLString(k), grokTOMLString(s.Headers[k]))
			}
		}
		if s.Transport != sandbox.TransportHTTP && len(s.Env) > 0 {
			line("[mcp_servers.%s.env]", full)
			for _, k := range slices.Sorted(maps.Keys(s.Env)) {
				line("%s = %s", grokTOMLString(k), grokTOMLString(s.Env[k]))
			}
		}
		line("")
	}
	b.WriteString(hooks)
	return b.String(), nil
}

// grokBoxConfig is the box's own ~/.grok/config.toml, as it stood before this
// run wrote it, kept at [Paths.GrokConfigBox] (empty where the box had none)
// until the run puts it back ([grokRestoreConfig]). Where a kept copy is
// already there, an earlier run was cut off before it put the box's back: the
// settings as they now stand less that run's tables ([grokLeftOut]) are kept
// in its place. So every run lays its tables into the box's settings and none
// into an earlier run's, and an edit made since stands.
func grokBoxConfig(ctx context.Context, box sandbox.Sandbox, paths Paths) ([]byte, error) {
	q := shellword.Quote
	kept, next := q(paths.GrokConfigBox()), q(paths.GrokConfigBox()+".next")
	res, err := box.Exec(ctx, fmt.Sprintf(`if [ -e %[1]s ]; then echo cut; else mkdir -p %[3]s && if [ -f %[4]s ]; then cp %[4]s %[2]s; else : > %[2]s; fi && mv -f %[2]s %[1]s; fi`,
		kept, next, q(paths.WorkDir()), q(paths.GrokConfig())), sandbox.ExecOptions{})
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	if err != nil {
		return nil, fmt.Errorf("executor: keeping the box's own Grok Build settings: %w", err)
	}
	files := []string{paths.GrokConfigBox()}
	if strings.TrimSpace(res.Stdout) == "cut" {
		files = append(files, paths.GrokConfig(), paths.GrokConfigOurs())
	}
	read := make([][]byte, len(files))
	for i, f := range files {
		if read[i], err = box.ReadFile(ctx, f); err != nil {
			return nil, fmt.Errorf("executor: reading the box's own Grok Build settings: %w", err)
		}
	}
	if len(read) == 1 {
		return read[0], nil
	}
	own, err := grokLeftOut(read[0], read[1], strings.Fields(string(read[2])), paths.GrokNoteHelper())
	if err != nil {
		return nil, err
	}
	if err = box.WriteFile(ctx, paths.GrokConfigBox(), own); err != nil {
		return nil, fmt.Errorf("executor: keeping the box's own Grok Build settings: %w", err)
	}
	return own, nil
}

var _ homeMender = Grok{}

// mendHome takes a killed run's tables out of the box's ~/.grok/config.toml
// before the next run in the box, whichever harness it runs: where a kept copy
// is there, the settings as they stand less that run's tables ([grokBoxConfig])
// are put back ([grokRestoreConfig]); nothing where none is kept.
func (Grok) mendHome(ctx context.Context, box sandbox.Sandbox, p Paths) error {
	held, err := heldInBox(ctx, box, p.GrokConfigBox())
	if err != nil || !held {
		return err
	}
	if _, err = grokBoxConfig(ctx, box, p); err != nil {
		return err
	}
	return execOK(ctx, box, grokRestoreConfig(p), "putting back the box's own Grok Build settings")
}

// grokLeftOut is the box's ~/.grok/config.toml live, as a run cut off before
// its script put the box's own back left it, less that run's tables: each
// server it recorded (names) given back the box's own of that name kept, or
// removed, and each hook running the note helper (helper) removed, an event
// left with none removed too. Every other key stands as live holds it, an
// edit made since included; nothing where live is gone.
func grokLeftOut(kept, live []byte, names []string, helper string) ([]byte, error) {
	if strings.TrimSpace(string(live)) == "" {
		return nil, nil
	}
	settings, own := map[string]any{}, map[string]any{}
	if _, err := toml.Decode(string(live), &settings); err != nil {
		return nil, fmt.Errorf("executor: the box's Grok Build settings (~/.grok/config.toml) do not read as TOML, "+
			"so a cut-off run's servers cannot be taken out of them: %w; fix the file in the box", err)
	}
	if _, err := toml.Decode(string(kept), &own); err != nil {
		return nil, fmt.Errorf("executor: the box's own Grok Build settings kept by a cut-off run do not read as "+
			"TOML: %w; remove that run's work directory's grok-config-box.toml", err)
	}
	if servers, ok := settings["mcp_servers"].(map[string]any); ok {
		theirs, _ := own["mcp_servers"].(map[string]any)
		for _, name := range names {
			if s, ok := theirs[name]; ok {
				servers[name] = s
			} else {
				delete(servers, name)
			}
		}
		if _, had := own["mcp_servers"]; len(servers) == 0 && !had {
			delete(settings, "mcp_servers")
		}
	}
	if hooks, ok := settings["hooks"].(map[string]any); ok {
		ran := shellword.Quote(helper) + " "
		for event, entries := range hooks {
			list, ok := grokHookList(entries)
			if !ok {
				continue
			}
			list = slices.DeleteFunc(list, func(e map[string]any) bool {
				inner, _ := grokHookList(e["hooks"])
				return slices.ContainsFunc(inner, func(h map[string]any) bool {
					command, _ := h["command"].(string)
					return strings.HasPrefix(command, ran)
				})
			})
			if len(list) == 0 {
				delete(hooks, event)
			} else {
				hooks[event] = list
			}
		}
		if _, had := own["hooks"]; len(hooks) == 0 && !had {
			delete(settings, "hooks")
		}
	}
	var b strings.Builder
	if err := toml.NewEncoder(&b).Encode(settings); err != nil {
		return nil, fmt.Errorf("executor: writing the box's own Grok Build settings: %w", err)
	}
	return []byte(b.String()), nil
}

// grokMergeConfig is a run's ~/.grok/config.toml: the box's own settings (boxOwn,
// [grokBoxConfig]) with the run's tables (own, [grokConfig]) laid in, and the
// ids Grok gives the run's hooks ([Paths.GrokHookIDs]). Each of its servers
// under its own name; its hooks after the box's own in each event, whatever
// their spelling, so an id the box's disabled-hooks names keeps naming the
// box's hook ([grokGuard]); the marketplace key where the box set none. Every
// other key stays as the box wrote it; comments and layout do not, as none
// survive Grok's own saves either.
func grokMergeConfig(boxOwn []byte, own string) ([]byte, []string, error) {
	var ids []string
	merged := map[string]any{}
	if _, err := toml.Decode(string(boxOwn), &merged); err != nil {
		return nil, nil, fmt.Errorf("executor: the box's Grok Build settings (~/.grok/config.toml) do not read as TOML, "+
			"so the run's cannot be added to them: %w; fix the file in the box's image", err)
	}
	ours := map[string]any{}
	if _, err := toml.Decode(own, &ours); err != nil {
		return nil, nil, fmt.Errorf("executor: the run's own Grok Build settings do not read as TOML: %w", err)
	}
	table := func(m map[string]any, key string) (map[string]any, error) {
		v, ok := m[key]
		if !ok {
			t := map[string]any{}
			m[key] = t
			return t, nil
		}
		t, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("executor: the box's Grok Build settings hold %q as no table; make it one", key)
		}
		return t, nil
	}
	for _, key := range []string{"mcp_servers", "marketplace"} {
		from, _ := ours[key].(map[string]any)
		if len(from) == 0 {
			continue
		}
		into, err := table(merged, key)
		if err != nil {
			return nil, nil, err
		}
		for k, v := range from {
			if _, set := into[k]; key == "mcp_servers" || !set {
				into[k] = v
			}
		}
	}
	if from, _ := ours["hooks"].(map[string]any); len(from) > 0 {
		into, err := table(merged, "hooks")
		if err != nil {
			return nil, nil, err
		}
		for _, event := range slices.Sorted(maps.Keys(from)) {
			var list []map[string]any
			for _, k := range slices.Sorted(maps.Keys(into)) {
				if grokEventKey(k) != grokEventKey(event) {
					continue
				}
				theirs, ok := grokHookList(into[k])
				if !ok {
					return nil, nil, fmt.Errorf("executor: the box's Grok Build settings hold hooks.%s as no list of "+
						"tables; make it one", k)
				}
				list = append(list, theirs...)
				delete(into, k)
			}
			own, _ := grokHookList(from[event])
			for i, entry := range own {
				hooks, _ := grokHookList(entry["hooks"])
				for j := range hooks {
					ids = append(ids, fmt.Sprintf("user:%s[%d].hooks[%d]", grokSnake(event), len(list)+i, j))
				}
			}
			into[event] = append(list, own...)
		}
	}
	var b strings.Builder
	if err := toml.NewEncoder(&b).Encode(merged); err != nil {
		return nil, nil, fmt.Errorf("executor: writing Grok Build's settings: %w", err)
	}
	slices.Sort(ids)
	return []byte(b.String()), ids, nil
}

// grokSnake is a hook event as Grok Build 1.0.92 names it in a hook's id:
// snake case (`PostToolUse` as `post_tool_use`; measured).
func grokSnake(event string) string {
	var b strings.Builder
	for i, r := range event {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// grokEventKey is a hook event's name as Grok Build matches it across
// spellings (`PostToolUse`, `post_tool_use`, `postToolUse`).
func grokEventKey(name string) string { return strings.ToLower(strings.ReplaceAll(name, "_", "")) }

// grokHookList is a hook event's entries as TOML decodes them, a table array
// or an inline array of tables, and false for anything else.
func grokHookList(v any) ([]map[string]any, bool) {
	switch l := v.(type) {
	case []map[string]any:
		return l, true
	case []any:
		out := make([]map[string]any, 0, len(l))
		for _, e := range l {
			m, ok := e.(map[string]any)
			if !ok {
				return nil, false
			}
			out = append(out, m)
		}
		return out, true
	}
	return nil, false
}

// grokAgentFile is the run's agent file ([Paths.GrokAgent]): a name of its own
// and, as its body, the run's standing instructions between [grokRulesOpen]
// and [grokRulesClose], which 1.0.92 appends to the very end of its own system
// prompt, leaving that prompt whole but its default agent's browser
// verification (measured). A file, never argv, for the reason Claude Code's
// are one.
func grokAgentFile(system string) []byte {
	return []byte("---\nname: crewlet-agent\ndescription: The Crewlet seat this run serves.\n---\n" +
		grokRulesOpen + "\n" + system + "\n" + grokRulesClose + "\n")
}

// grokRulesJSON is a continuing run's renewed standing instructions as
// [grokRenewRules] splices them: the rest of the agent file's body after
// [grokRulesOpen], as the content of a JSON string, without its quotes.
func grokRulesJSON(system string) ([]byte, error) {
	raw, err := json.Marshal("\n" + system + "\n" + grokRulesClose)
	if err != nil {
		return nil, err
	}
	return raw[1 : len(raw)-1], nil
}

// WriteConfig writes what a Grok Build run reads beside what its box already
// holds, which Grok loads as a person's would: its settings, the box's own with
// the run's tables laid in ([grokMergeConfig]), with a fresh session's beside
// them ([freshSessionWiring]), each naming its servers for its session; its
// agent files and the renewal a continued session is handed; its skills; its
// take config where it is wired to its notes or its skills' loads; the note
// helper and its checkouts' launch config ([Grok.checkoutHome]); and, once the
// box's Grok runs the run's servers as they are written ([grokServersHeld]),
// its brief, and the brief a new session is handed in a continued one's
// place, each as words ([briefNotACommand]).
func (g Grok) WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, session Session,
	paths Paths) (string, error) {
	if session.ID == "" || (session.Continues && session.Fresh == "") {
		return "", errors.New("executor: a Grok Build run names no session to name its MCP servers for")
	}
	if err := grokSkillsHidden(ctx, box, req.Skills, paths); err != nil {
		return "", err
	}
	nonce := sessionNonce(session.ID)
	system := g.system(req, req.MCPServers, nonce)
	if err := box.WriteFile(ctx, paths.GrokAgent(), grokAgentFile(system)); err != nil {
		return "", fmt.Errorf("executor: writing Grok Build's agent file: %w", err)
	}
	rules, err := grokRulesJSON(system)
	if err != nil {
		return "", err
	}
	if err = box.WriteFile(ctx, paths.GrokRules(), rules); err != nil {
		return "", fmt.Errorf("executor: writing the standing instructions a continued session is handed: %w", err)
	}
	notes, loads := req.Steer.Set(), req.SkillLoads.Set()
	boxOwn, err := grokBoxConfig(ctx, box, paths)
	if err != nil {
		return "", err
	}
	if err = grokSkillsSwitchedOff(boxOwn, req.Skills, paths); err != nil {
		return "", err
	}
	ours := grokNames(req.MCPServers, nonce)
	if f := freshSessionWiring(req, session); f != nil {
		ours = append(ours, grokNames(f.MCPServers, sessionNonce(session.Fresh))...)
	}
	var record strings.Builder
	for _, name := range slices.Compact(slices.Sorted(slices.Values(ours))) {
		record.WriteString(name + "\n")
	}
	if err = box.WriteFile(ctx, paths.GrokConfigOurs(), []byte(record.String())); err != nil {
		return "", fmt.Errorf("executor: recording the Grok Build run's own MCP servers: %w", err)
	}
	// THE NOTE HELPER IN EVERY BOX ITS MACHINE HAS A BUILD FOR, which hands
	// the checkouts below the start directory theirs ([Grok.checkoutHome]),
	// with the nested hook; a box with none starts without them, its gate
	// saying so, unless notes or loads need it.
	nested := true
	if err = writeNoteHelper(ctx, box, paths.GrokNoteHelper()); errors.Is(err, errNoHelperBuild) && !notes && !loads {
		nested = false
	} else if err != nil {
		return "", err
	}
	launch := Grok{}.checkoutHome(paths).launchConfig(grokCheckouts(paths), req.ReportedRepos)
	if err = box.WriteFile(ctx, paths.GrokCheckoutLaunch(), launch); err != nil {
		return "", fmt.Errorf("executor: writing what hands the run's repositories their instructions: %w", err)
	}
	contract := grokHooks(paths, notes, loads, req.Skills)
	own, err := grokConfig(req.MCPServers, nonce, contract+grokNestedHook(paths, nested))
	if err != nil {
		return "", err
	}
	config, all, err := grokMergeConfig(boxOwn, own)
	if err != nil {
		return "", err
	}
	// THE CONTRACT HOOKS' IDS APART FROM THE NESTED HOOK'S: only the former's
	// loss refuses a run ([grokGuard]).
	ownContract, err := grokConfig(req.MCPServers, nonce, contract)
	if err != nil {
		return "", err
	}
	_, ids, err := grokMergeConfig(boxOwn, ownContract)
	if err != nil {
		return "", err
	}
	nestedIDs := slices.DeleteFunc(slices.Clone(all), func(id string) bool { return slices.Contains(ids, id) })
	if err = box.WriteFile(ctx, paths.GrokConfig(), config); err != nil {
		return "", fmt.Errorf("executor: writing Grok Build's settings: %w", err)
	}
	if err = writeGrokSkills(ctx, box, req.Skills, paths); err != nil {
		return "", err
	}
	if notes || loads {
		take, terr := hookTakeConfig(req.MCPServers, req.Steer, req.SkillLoads, paths.GrokSkills())
		if terr != nil {
			return "", terr
		}
		if err = box.WriteFile(ctx, paths.GrokTakeConfig(), take); err != nil {
			return "", fmt.Errorf("executor: writing the note helper's config: %w", err)
		}
	}
	for file, list := range map[string][]string{paths.GrokHookIDs(): ids, paths.GrokNestedHookID(): nestedIDs} {
		if len(list) == 0 {
			continue
		}
		if err = box.WriteFile(ctx, file, []byte(strings.Join(list, "\n")+"\n")); err != nil {
			return "", fmt.Errorf("executor: recording the ids of the Grok Build run's hooks: %w", err)
		}
	}
	var freshNames []string
	if f := freshSessionWiring(req, session); f != nil {
		freshNonce := sessionNonce(session.Fresh)
		freshNames = grokNames(f.MCPServers, freshNonce)
		agent := grokAgentFile(g.system(req, f.MCPServers, freshNonce))
		if err = box.WriteFile(ctx, paths.GrokAgentFresh(), agent); err != nil {
			return "", fmt.Errorf("executor: writing Grok Build's agent file for a new session: %w", err)
		}
		freshOwn, ferr := grokConfig(f.MCPServers, freshNonce,
			grokHooks(paths, notes, loads, req.Skills)+grokNestedHook(paths, nested))
		if ferr != nil {
			return "", ferr
		}
		fresh, _, ferr := grokMergeConfig(boxOwn, freshOwn)
		if ferr != nil {
			return "", ferr
		}
		if err = box.WriteFile(ctx, paths.GrokConfigFresh(), fresh); err != nil {
			return "", fmt.Errorf("executor: writing Grok Build's settings for a new session: %w", err)
		}
		if notes || loads {
			take, terr := hookTakeConfig(f.MCPServers, f.Steer, f.SkillLoads, paths.GrokSkills())
			if terr != nil {
				return "", terr
			}
			if err = box.WriteFile(ctx, paths.GrokTakeConfigFresh(), take); err != nil {
				return "", fmt.Errorf("executor: writing the note helper's config for a new session: %w", err)
			}
		}
	}
	if err = grokServersHeld(ctx, box, req, paths, grokNames(req.MCPServers, nonce), freshNames); err != nil {
		return "", err
	}
	if err = box.WriteFile(ctx, paths.Brief(), []byte(briefNotACommand(req.Brief))); err != nil {
		return "", fmt.Errorf("executor: writing the brief: %w", err)
	}
	if req.Resume != "" && req.FreshBrief != "" {
		if err = box.WriteFile(ctx, paths.FreshBrief(), []byte(briefNotACommand(req.FreshBrief))); err != nil {
			return "", fmt.Errorf("executor: writing the brief a new session is handed: %w", err)
		}
	}
	return paths.GrokConfig(), nil
}

// system is a session's standing instructions: the request's, the tool
// instruction naming servers under nonce ([grokToolsInstruction]), and the
// note instruction where the run takes notes.
func (g Grok) system(req sandbox.RunRequest, servers map[string]sandbox.MCPServer, nonce string) string {
	system := strings.TrimSpace(strings.Join([]string{req.System, grokToolsInstruction(grokNames(servers, nonce))},
		"\n\n"))
	if req.Steer.Set() {
		system = withNoteInstruction(system, g.NoteInstruction())
	}
	return system
}

// readsCommands marks Grok Build as running a brief that begins with `/` as one
// of its commands: `/session-info` answered with the session's details and
// `/compact` compacted, whatever white space came first, through
// `--prompt-file` (measured on 1.0.92).
func (Grok) readsCommands() {}

// writeGrokSkills writes each of a run's skills as `<skills>/<listed
// name>/SKILL.md` ([Paths.GrokSkills]), once every skill an earlier run wrote
// there (`crewlet:*`) is gone, so the run lists its own alone beside the box's.
func writeGrokSkills(ctx context.Context, box sandbox.Sandbox, skills []sandbox.Skill, paths Paths) error {
	res, err := box.Exec(ctx, "rm -rf "+shellword.Quote(paths.GrokSkills())+"/"+
		shellword.Quote(sandbox.SkillNamespace+":")+"*", sandbox.ExecOptions{})
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	if err != nil {
		return fmt.Errorf("executor: removing an earlier run's skills from Grok Build's skills: %w", err)
	}
	for _, s := range skills {
		file, err := skillFile(s.Listed(), s.Description, s.Body)
		if err != nil {
			return err
		}
		if err = box.WriteFile(ctx, paths.GrokSkills()+"/"+s.Listed()+"/SKILL.md", file); err != nil {
			return fmt.Errorf("executor: writing the run's skill %q: %w", s.Listed(), err)
		}
	}
	return nil
}

// artifactFiles is the files a Grok Build run leaves in its box of its own
// beside its sessions, which a later run must not find ([artifactHolder]): its
// agent files, renewal, a fresh session's settings, the note helper, its
// configs, its hooks' ids, the session it names, and its checkouts' launch
// config and sections ([Grok.checkoutHome]). Never ~/.grok/config.toml, the
// box's own settings, which the run puts back ([grokRestoreConfig]), nor the
// copy kept of them while a run runs, nor its sessions and login.
func (Grok) artifactFiles(p Paths) []string {
	return []string{p.GrokAgent(), p.GrokAgentFresh(), p.GrokRules(), p.GrokConfigFresh(), p.GrokTakeConfig(),
		p.GrokTakeConfigFresh(), p.GrokThread(), p.GrokNoteHelper(), p.GrokHookIDs(), p.GrokNestedHookID(),
		p.GrokCheckoutLaunch(), p.GrokCheckoutSections()}
}

// grokChargeBytes is the fewest bytes a Grok Build session's brief is counted
// at a token: one, the most tokens any estimate can make of n bytes, since
// Grok's own estimate was not read.
const grokChargeBytes sandbox.Charge = 1

// sessionTraits is how a relaunch judges a Grok Build session, each value the
// one that never under-counts its next request, since Grok compacts at a window
// its record does not hold (measured on 1.0.92): FirstRequestBox, with no
// point its box can say; CallKeyingNone; [grokChargeBytes].
func (Grok) sessionTraits() sandbox.SessionTraits {
	return sandbox.SessionTraits{FirstRequest: sandbox.FirstRequestBox, CallKeying: sandbox.CallKeyingNone,
		Charge: grokChargeBytes}
}
