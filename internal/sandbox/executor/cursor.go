package executor

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

func init() { Register(CursorAgent{}) }

// CursorAgentName is this runner's config name.
const CursorAgentName = "cursor-agent"

// CursorAgent drives Cursor Agent headless (`cursor-agent -p --output-format
// stream-json`, the brief on stdin), measured on [cursorSupported]. A run
// loads what a person's session of Cursor loads in the run's directory and
// home; the run adds its standing instructions as a rule in its directory,
// its MCP servers beside the home's, and the instructions and skills of each
// checkout below it. A session is carried as Cursor's own conversation
// history ([cursorArchiveFormat]) and continued by handing it back, which
// also hands the run its current instructions (docs/concepts/code-sandbox.md,
// "Cursor Agent").
type CursorAgent struct{}

var (
	_ Harness           = CursorAgent{}
	_ resumer           = CursorAgent{}
	_ carrier           = CursorAgent{}
	_ saver             = CursorAgent{}
	_ artifactHolder    = CursorAgent{}
	_ artifactDirHolder = CursorAgent{}
	_ inPlaceGate       = CursorAgent{}
	_ sessionJudge      = CursorAgent{}
)

// NewCursorAgent returns the runner.
func NewCursorAgent() *Runner { return New(CursorAgent{}) }

// Name is the harness's key in the runner registry.
func (CursorAgent) Name() string { return CursorAgentName }

// Resumes says Cursor continues a session it reported: the run hands its
// conversation back under the same id ([cursorSessionGate]).
func (CursorAgent) Resumes() bool { return true }

// cursorSupported is the one Cursor Agent release this build drives, every
// behaviour here measured on it (the stream, the chat store the export reads,
// the history the import takes): an older release is refused at the run's
// start, a newer one runs with a new session every time, and a session is
// continued only between boxes on exactly this release.
const cursorSupported = "2026.11.22-e9042b2"

// cursorSupportedDate is [cursorSupported]'s date as the script compares it.
const cursorSupportedDate = "20261122"

// cursorReleaseName is [cursorSupported] as the engine names a release, its date
// alone (MAJOR.MINOR.PATCH, [sandbox.ValidRelease]): what the run's record
// names and [CursorAgent.VersionCommand] prints. The build travels with the
// session's archive ([cursorPack]) and is held on restore, the box's own copy
// by its release file ([cursorSessionGate]): a session continues only on
// [cursorSupported] whole.
const cursorReleaseName = "2026.11.22"

// cursorRouting is Cursor's variables that send its model calls, its sign-in
// or its feature flags elsewhere than the entry names (read from the
// 2026.11.22 bundle), each with its role. One the run's own environment does
// not set is removed: an image's or a login profile's would route the seat's
// calls where its entry never named.
var cursorRouting = slices.Concat(
	routes(roleEndpoint, "CURSOR_API_ENDPOINT", "CURSOR_API_BASE_URL", "CURSOR_AGENT_CLI_LOCAL_MODE"),
	routes(roleSigns, "CURSOR_AUTH_TOKEN", "CURSOR_AGENT_CLI_AUTHLESS_MODE"),
	routes(roleShapes, "CURSOR_STATSIG_OVERRIDES"))

// routing answers [router]: [cursorRouting] and the key Cursor signs in with,
// CURSOR_API_KEY, which a run keeps from its box as a person's session would.
func (CursorAgent) routing() []routeVar {
	return slices.Concat(cursorRouting, routes(roleSigns, "CURSOR_API_KEY"))
}

// cursorRoutingOff is every [cursorRouting] name req's own environment does
// not set, which the script unsets.
func cursorRoutingOff(req sandbox.RunRequest) []string {
	var off []string
	for _, name := range routeNames(cursorRouting) {
		if _, set := req.Env[name]; !set {
			off = append(off, name)
		}
	}
	return off
}

// Session is the session a Cursor run works in ([Harness.Session]): the one it
// continues, or a new one under a UUID minted for it (--new-session-id), and,
// for a run that continues one, a second UUID for the session it starts
// instead where neither its box nor a carried archive holds that one.
func (CursorAgent) Session(req sandbox.RunRequest) Session {
	if req.Resume != "" {
		return Session{ID: req.Resume, Continues: true, Fresh: uuid.NewString(),
			Replaces: req.Carried != nil && req.Carried.Replace}
	}
	return Session{ID: uuid.NewString()}
}

// Command is the run as ONE SCRIPT ([cursorScript]), so the wrapper's
// redirections (the brief on stdin, the stream, the error file) reach each
// step.
func (CursorAgent) Command(req sandbox.RunRequest, session Session, paths Paths, configPath string) string {
	return "sh -c " + shellword.Quote(cursorScript(req, session, paths, configPath != "", sandbox.JobStopGrace))
}

// cursorCapType is the type of the line a run's script adds to its stream
// once the turn cap stopped the run ([cursorCapLine]).
const cursorCapType = "crewlet.cursor.cap"

// cursorCapLine is the line for a cap of turns.
func cursorCapLine(turns int) string {
	return fmt.Sprintf(`{"type":%q,"cap":%d}`, cursorCapType, turns)
}

// cursorRule is the rule a run's standing instructions are handed in, in its
// directory ([Paths.System]): alwaysApply, so Cursor puts it in every request.
const cursorRule = ".cursor/rules/crewlet-standing.mdc"

// cursorScript is a Cursor run's script, giving its harness stop to go once
// asked to end: the end protocol (end.go); the release read and an older one
// refused; Cursor's node found; the routing the run does not set removed
// ([cursorRoutingOff]); its rules written ([cursorRules]); the session settled
// and recorded ([cursorSessionGate]); its MCP servers laid into the home's;
// the harness run with its cap's follower ([cursorCapFollow]); Cursor's own end
// added to the stream ([cursorTurnEnded]); the servers and rules taken out
// again; and the session packed ([cursorCapture]).
//
// THE BRIEF IS NOT ON ITS COMMAND LINE: print mode with no prompt argument
// reads stdin, the brief's file (measured on [cursorSupported]).
func cursorScript(req sandbox.RunRequest, session Session, paths Paths, configured bool, stop time.Duration) string {
	q := shellword.Quote
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	b.WriteString(endPrologue(paths, stop))
	b.WriteString(endHarness(""))
	b.WriteString(cursorReleaseRead)
	b.WriteString(cursorNode(1))
	if off := cursorRoutingOff(req); len(off) > 0 {
		line(`unset %s`, strings.Join(off, " "))
	}
	line(`mkdir -p %s || exit 1`, q(paths.CursorSessions()))
	line(`set --`)
	b.WriteString(cursorRules(paths))
	b.WriteString(cursorSessionGate(session, paths, configured, req.FreshWiring != nil && configured))
	line(`printf 'id %%s\ncontinued %%s\nwhy %%s\nrelease %%s\n' "$s" "$k" "$w" "${n%%%%-*}" > %s`,
		q(paths.SessionRecord()))
	add := q(paths.CursorMCPAdded())
	line(`"$cn" %s mcp-remove %s %s || exit 1`, q(paths.CursorHelper()), q(paths.CursorMCP()), add)
	if configured {
		line(`"$cn" %s mcp-add %s %s %s || exit 1`, q(paths.CursorHelper()), q(paths.CursorMCP()),
			q(paths.MCPConfig()), add)
	}
	line(`rm -f %s %s`, q(paths.MCPConfig()), q(paths.MCPConfigFresh()))
	parts := []string{"-p", "--output-format", "stream-json", "--trust", "--force", "--approve-mcps",
		"--sandbox", "disabled"}
	if req.Model != "" {
		parts = append(parts, "--model", q(req.Model))
	}
	harness := "sh -c " + q(`printf '%s' "$$" > `+q(paths.CursorPID())+` && exec cursor-agent "$@"`) +
		" cursor-agent " + strings.Join(parts, " ") + ` "$@"`
	line(`rm -f %s %s`, q(paths.CursorPID()), q(paths.CursorCapped()))
	turns := max(req.Limits.MaxTurns, 0)
	if turns > 0 {
		b.WriteString(cursorCapFollow(paths, turns))
	}
	line(`tf=$(ls -t "$HOME"/.cursor/projects/*/agent-transcripts/"$s"/"$s".jsonl 2> /dev/null | head -n 1); ts=0; [ -z "$tf" ] || ts=$(wc -c < "$tf" 2> /dev/null) || ts=0`)
	b.WriteString(launchHarness(harness, paths))
	if turns > 0 {
		line(`kill "$cf" 2> /dev/null; wait "$cf" 2> /dev/null`)
		line(`"$cn" %s capped %s %d %s < /dev/null > /dev/null 2>&1 || :`, q(paths.CursorHelper()), q(paths.Result()),
			turns, q(paths.CursorCapped()))
	}
	b.WriteString(cursorWorkersEnd)
	b.WriteString(cursorTurnEnded)
	if turns > 0 {
		line(`[ ! -s %s ] || printf '\n%%s\n' %s`, q(paths.CursorCapped()), q(cursorCapLine(turns)))
	}
	line(`"$cn" %s mcp-remove %s %s || echo 'crewlet: the run'"'"'s MCP servers could not be taken out of ~/.cursor/mcp.json; the next run takes them out' >&2`,
		q(paths.CursorHelper()), q(paths.CursorMCP()), add)
	line(`rm -f .cursor/rules/crewlet-*.mdc`)
	b.WriteString(cursorCapture(paths))
	line(`exit "$code"`)
	return b.String()
}

// cursorReleaseRead reads the box's `cursor-agent --version` once as v, its dated
// release (`2026.11.22-e9042b2`) as n, "" for one that names none, and refuses
// the run on a box that names none or one older than [cursorSupported]. A
// newer release runs, with a new session every time ([cursorSessionGate]).
var cursorReleaseRead = fmt.Sprintf(`v=$(cursor-agent --version < /dev/null 2> /dev/null | head -n 1); n=
case "$v" in [0-9][0-9][0-9][0-9].[0-9][0-9].[0-9][0-9]-[0-9a-f]*) n=$v;; esac
case "$n" in *[!0-9a-f.-]*) n=;; esac
[ -n "$n" ] || { echo "crewlet: this box's cursor-agent --version printed '$v', which names no dated release (YYYY.MM.DD-build) the cursor-agent runner reads; it supports Cursor Agent %[1]s: install that release in the box image" >&2; exit 1; }
nd=$(printf '%%s' "${n%%%%-*}" | tr -d .)
[ "$nd" -ge %[2]s ] || { echo "crewlet: this box runs Cursor Agent $n, older than %[1]s, the release this build supports: install %[1]s in the box image" >&2; exit 1; }
`, cursorSupported, cursorSupportedDate)

// cursorNode finds, as cn, the node Cursor's package ships beside its binary,
// which the helper runs on: the package's launcher runs Cursor on it, so a box
// that runs Cursor holds it, its SQLite included. A box without it exits code.
func cursorNode(code int) string {
	return fmt.Sprintf(`cn=$(command -v cursor-agent 2> /dev/null) && cn=$(readlink -f "$cn" 2> /dev/null) && cn=${cn%%/*}/node
[ -x "$cn" ] || { echo "crewlet: this box's cursor-agent has no node beside it ($cn), which Cursor's own package ships and the run's helper runs on: install Cursor Agent from its package" >&2; exit %d; }
`, code)
}

// cursorRules is the part of a run's script that writes the rules the run is
// handed in its directory's .cursor/rules, an earlier run's taken out first:
// its standing instructions ([cursorRule]), which Cursor puts in every
// request's first user message; and for each checkout directly below (a
// `.git` in it), its instructions (AGENTS.md, CLAUDE.md, .cursorrules) in one
// rule and its own rules, nested ones too, named by path, each glob prefixed
// with the checkout's directory, as Cursor reads rules only where a run starts
// and matches globs from there; and `--add-dir` on the checkout, for its
// skills. Its MCP servers, hooks and permissions are not handed: Cursor reads
// them only where a run starts. All measured on [cursorSupported].
func cursorRules(paths Paths) string {
	q := shellword.Quote
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line(`rd=.cursor/rules; mkdir -p "$rd" || exit 1; rm -f "$rd"/crewlet-*.mdc`)
	line(`if [ -s %s ]; then { printf -- '---\ndescription: The instructions this run works under\nalwaysApply: true\n---\n'; cat %s; } > %s || exit 1; fi`,
		q(paths.System()), q(paths.System()), q(cursorRule))
	line(`for c in ./*/; do c=${c%%/}; c=${c#./}; [ -e "$c/.git" ] || continue`)
	line(`  set -- "$@" --add-dir "$PWD/$c"`)
	line(`  t=$(printf '%%s' "$c" | tr 'A-Z' 'a-z' | tr -c 'a-z0-9' '-'); u=$t; i=1`)
	line(`  while [ -e "$rd/crewlet-checkout-$u.mdc" ]; do i=$((i + 1)); u=$t-$i; done`)
	line(`  r="$rd/crewlet-checkout-$u.mdc"; h=`)
	line(`  for f in AGENTS.md CLAUDE.md .cursorrules; do [ -f "$c/$f" ] || continue`)
	line(`    if [ -z "$h" ]; then printf -- '---\ndescription: The instructions of the checkout %%s\nalwaysApply: true\n---\nThe checkout %%s, at %%s, brings these instructions of its own.\n' "$c" "$c" "$PWD/$c" > "$r" || exit 1; h=1; fi`)
	line(`    { printf '\n# %%s\n\n' "$f"; cat "$c/$f"; } >> "$r" || exit 1`)
	line(`  done`)
	line(`  fl=$(find "$c/.cursor/rules" -type f -name '*.mdc' 2> /dev/null | sort); set -f; o=$IFS; IFS='`)
	line(`'`)
	line(`  for f in $fl; do IFS=$o; n=$(printf '%%s' "${f#"$c/.cursor/rules/"}" | sed 's|/|--|g')`)
	line(`    P="$c/" awk %s "$f" > "$rd/crewlet-checkout-$u--$n" || exit 1`, q(cursorGlobs))
	line(`  done; IFS=$o; set +f`)
	line(`done`)
	return b.String()
}

// cursorGlobs is the awk program that copies a checkout's rule with each glob
// of its frontmatter's `globs:` (a comma-separated list, each item quoted or
// not) prefixed with P, the checkout's directory ([cursorRules]).
const cursorGlobs = `NR == 1 && /^---$/ { fm = 1; print; next }
fm && /^---$/ { fm = 0; print; next }
fm && /^globs:/ {
  n = split(substr($0, 7), g, ","); out = ""
  for (i = 1; i <= n; i++) { v = g[i]; gsub(/^[ \t"']+|[ \t"']+$/, "", v); if (v != "") out = out (out == "" ? "" : ",") ENVIRON["P"] v }
  print "globs: " out; next
}
{ print }`

// cursorCapFollow starts, as cf, the follower that holds the run to its cap
// of turns: the helper's `cap` counts the model responses that call a tool
// off the stream and interrupts Cursor at the turns-th, leaving
// [Paths.CursorCapped]. SIGINT, as a person's Ctrl-C: Cursor leaves its chat
// whole (measured on [cursorSupported]), so the session carries.
func cursorCapFollow(paths Paths, turns int) string {
	q := shellword.Quote
	return fmt.Sprintf("\"$cn\" %s cap %s %s %d %s < /dev/null > /dev/null 2>&1 & cf=$!\n",
		q(paths.CursorHelper()), q(paths.Result()), q(paths.CursorPID()), turns, q(paths.CursorCapped()))
}

// cursorTurnEnded is the part of a run's script that adds to its stream
// Cursor's own end of the turn: the `turn_ended` line its transcript records
// for a turn that finished or failed (and never for one interrupted or
// killed, measured on [cursorSupported]), AS RECORDED, and only one this run
// wrote, after the size the script read before the launch (ts): a continued
// session's transcript ends in the earlier run's.
const cursorTurnEnded = `tg=$(ls -t "$HOME"/.cursor/projects/*/agent-transcripts/"$s"/"$s".jsonl 2> /dev/null | head -n 1)
if [ -n "$tg" ]; then [ "$tg" = "$tf" ] || ts=0; e=$(tail -c +$((ts + 1)) "$tg" 2> /dev/null | grep -F '"type":"turn_ended"' | tail -n 1); [ -z "$e" ] || printf '\n%s\n' "$e"; fi
`

// cursorWorkersEnd is the part of a run's script that ends the worker Cursor
// starts beside itself (`<package>/index.js worker-server`), which outlives
// cursor-agent finished, interrupted or terminated alike (measured on
// [cursorSupported]), held to the run's own process group: through end
// ([endFunction]), as OpenCode's script ends its server, before the session is
// packed from a store the worker could still hold.
const cursorWorkersEnd = `g=$(sed 's/.*) //' /proc/$$/stat 2> /dev/null | cut -d' ' -f3)
for d in /proc/[0-9]*; do c=$(tr '\000' ' ' < "$d/cmdline" 2> /dev/null) || continue
  case "$c" in *"${cn%/*}/index.js worker-server"*) [ -n "$g" ] && [ "$(sed 's/.*) //' "$d/stat" 2> /dev/null | cut -d' ' -f3)" = "$g" ] && end "${d#/proc/}";; esac
done
`

// cursorHeld finds, as hc, the box's chat of session s, filed under any
// directory's digest.
const cursorHeld = `hc=$(ls -d "$HOME"/.cursor/chats/*/"$s" 2> /dev/null | head -n 1)
`

// cursorSessionGate names the session the harness runs in (s), whether it
// continues the one asked (k) and, where it could not, why (w). As
// [claudeSessionGate], the box's chat is continued unless a carried archive
// holds what it does not, and an archive once its digest is recorded.
//
// A CONTINUED SESSION IS HANDED BACK, never resumed: the box's chat exported
// afresh, or a carried archive's history, goes in with
// --conversation-history-file under the same id, the old chat removed, so
// Cursor writes its rules afresh for it, where a resumed chat would keep them
// as they were; `--resume` of a chat it does not hold starts an empty
// conversation, exit 0 (measured). A conversation the export cannot read
// whole starts a new session, saying why.
func cursorSessionGate(session Session, paths Paths, configured, rewired bool) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := shellword.Quote
	helper := q(paths.CursorHelper())
	if !session.Continues {
		line(`s=%s k=no w=`, q(session.ID))
		line(`set -- "$@" --new-session-id "$s"`)
		line(`printf '%%s\n' "$n" > %s 2> /dev/null || :`, q(cursorReleaseFile(paths, session.ID)))
		return b.String()
	}
	supported := " and a session is continued only between boxes on Cursor Agent " + cursorSupported +
		", the release this build supports"
	f := q(paths.CursorSession(session.ID))
	staged := paths.SessionStaging()
	carried := q(paths.SessionCarried())
	member := q(staged + "/in/" + cursorArchiveDir + "/" + session.ID + ".jsonl")
	writer := q(staged + "/in/" + cursorArchiveDir + "/" + session.ID + ".release")
	// THE WRITER'S BUILD, which the archive carries beside the conversation
	// (cr), read before the conversation is taken: another build of the
	// supported date continues nothing.
	line(`cursorrestore() {`)
	line(`  rm -rf %s && mkdir -p %s || return 1`, q(staged), q(staged+"/in"))
	line(`  tar -xzf %s -C %s < /dev/null > /dev/null 2>&1 || return 1`, carried, q(staged+"/in"))
	line(`  [ ! -L %s ] && [ -f %s ] && [ ! -L %s ] && [ -s %s ] && [ -f %s ] && [ ! -L %s ] || return 1`,
		q(staged+"/in/"+cursorArchiveDir), member, member, member, writer, writer)
	line(`  cr=$(head -n 1 %s | head -c 64 | tr -cd '0-9a-z.-')`, writer)
	line(`  [ "$cr" = %s ] || return 1`, cursorSupported)
	line(`  mv -f %s %s`, member, f)
	line(`}`)
	line(`s=%s k=yes w= h= d= x= cr=`, q(session.ID))
	b.WriteString(cursorHeld)
	line(`[ "$n" = %s ] || w="this box runs Cursor Agent $n,"%s`, cursorSupported, q(supported))
	// THE BOX'S OWN COPY, its chat or the conversation an earlier run here
	// kept, continued only where the release that wrote it is the one
	// supported.
	line(`x=$(cat %s 2> /dev/null) || x=`, q(cursorReleaseFile(paths, session.ID)))
	line(`if [ -z "$w" ] && [ "$x" = %s ] && { [ -n "$hc" ] || [ -s %s ]; }; then h=1; fi`, cursorSupported, f)
	line(`if [ -s %s ]; then d=$(cksum < %s 2> /dev/null) || d=; fi`, carried, carried)
	if session.Replaces {
		line(`if [ -n "$h" ] && [ -s %s ]; then h=; fi`, carried)
	} else {
		line(`if [ -n "$h" ] && [ -n "$d" ] && ! grep -qxF "$s $d" %s 2> /dev/null; then h=; fi`,
			q(paths.SessionDigests()))
	}
	line(`if [ -z "$w" ] && [ -n "$h" ] && [ -n "$hc" ]; then`)
	line(`  "$cn" %s export "$hc/store.db" %s.next 2> %s.error && mv -f %s.next %s || w="its chat in this box could not be carried on: $(cat %s.error 2> /dev/null)"`,
		helper, f, f, f, f, f)
	line(`elif [ -z "$w" ] && [ -z "$h" ]; then`)
	line(`  if [ ! -s %s ] && [ -n "$x" ] && [ "$x" != %s ]; then w="its copy in this box was written by Cursor Agent $x,"%s`,
		carried, cursorSupported, q(supported))
	line(`  elif [ ! -s %s ]; then w='it is not in this box, and no archive of it was carried here'`, carried)
	line(`  elif ! command -v tar > /dev/null 2>&1 || ! command -v gzip > /dev/null 2>&1; then w='this box has no tar and gzip to restore its archive with'`)
	line(`  elif ! cursorrestore; then if [ -n "$cr" ] && [ "$cr" != %s ]; then w="its archive was written by Cursor Agent $cr,"%s; else w='its archive could not be restored'; fi`,
		cursorSupported, q(supported))
	line(`  elif ! { %s; }; then w=%s`, recordDigest(paths, "s"), q(restoreDigestUnrecorded))
	line(`  fi`)
	line(`  rm -rf %s`, q(staged))
	line(`fi`)
	line(`rm -f %s`, carried)
	servers := ""
	if configured {
		servers = " " + q(paths.MCPConfig())
	}
	line(`if [ -z "$w" ]; then "$cn" %s doc %s %s%s 2> %s.error || w="its conversation could not be handed to Cursor Agent: $(cat %s.error 2> /dev/null)"; fi`,
		helper, f, q(paths.CursorHistory()), servers, f, f)
	line(`rm -f %s.error`, f)
	line(`if [ -z "$w" ]; then`)
	line(`  [ -z "$hc" ] || rm -rf "$hc" || exit 1`)
	line(`  set -- "$@" --conversation-history-file %s --new-session-id "$s"`, q(paths.CursorHistory()))
	if rewired {
		line(`  rm -f %s`, q(paths.MCPConfigFresh()))
	}
	line(`else`)
	line(`  s=%s k=no`, q(session.Fresh))
	line(`  printf '%%s%%s\n' %s "$w" >&2`, q("crewlet: this run was asked to continue the Cursor Agent session "+
		session.ID+", and starts a new session, "+session.Fresh+", instead: "))
	line(`  set -- "$@" --new-session-id "$s"`)
	line(`  [ ! -s %s ] || exec < %s`, q(paths.FreshBrief()), q(paths.FreshBrief()))
	if rewired {
		// THE NEW SESSION WIRED TO ITS OWN TOKEN, or not started:
		// [claudeSessionGate]'s reason.
		line(`  mv -f %s %s || { echo 'crewlet: the new session could not be wired to the tools of its seat, so the run does not start' >&2; exit 1; }`,
			q(paths.MCPConfigFresh()), q(paths.MCPConfig()))
	}
	line(`fi`)
	line(`printf '%%s\n' "$n" > %s/"$s".release 2> /dev/null || :`, q(paths.CursorSessions()))
	return b.String()
}

// cursorReleaseFile is where a box keeps the release that last wrote
// session, beside its conversation ([Paths.CursorSession]).
func cursorReleaseFile(paths Paths, session string) string {
	return paths.CursorSessions() + "/" + session + ".release"
}

// cursorArchiveDir is the directory an archive's one member is filed under.
const cursorArchiveDir = "cursor-history"

// cursorArchiveFormat is the layout a Cursor run packs its session in
// ([cursorPack]) and the one it restores ([cursorSessionGate]): one gzipped tar
// of two members, `cursor-history/<session>.release`, the build that wrote it,
// and `cursor-history/<session>.jsonl`, the session's conversation
// as the messages of Cursor's own ConversationHistory, one a line
// (cursor_helper.mjs's export): text a scrub rewrites a line at a time, where
// Cursor's chat store is content-addressed, so no value can be taken out of it
// and the store still read. A change is a new format.
const cursorArchiveFormat = 1

// ArchiveFormat is the layout a Cursor run packs its session in ([carrier]).
func (CursorAgent) ArchiveFormat() int { return cursorArchiveFormat }

// VersionCommand prints a box's Cursor release ([carrier]) as the engine names
// one ([cursorReleaseName]): the date of `--version`'s first line, as
// [cursorReleaseRead] reads it, its build dropped.
func (CursorAgent) VersionCommand() string {
	return "sh -c " + shellword.Quote(`v=$(cursor-agent --version < /dev/null) && v=$(printf '%s\n' "$v" | head -n 1) && printf '%s\n' "${v%%-*}"`)
}

// layout is a Cursor archive's: a gzipped tar of the conversation's file.
func (CursorAgent) layout() archiveLayout { return archiveTar }

// charges is false: its meter counts nothing ([carrier.charges]).
func (CursorAgent) charges(string, string) bool { return false }

// Continues reports whether a session written by Cursor release writer can be
// continued in a box whose Cursor is release box ([carrier]): only both
// exactly [cursorSupported].
func (c CursorAgent) Continues(box, writer string) (bool, string) {
	if ok, why := c.ContinuesWriter(writer); !ok {
		return false, why
	}
	return c.continuesInPlace(box)
}

// ContinuesWriter is [CursorAgent.Continues] judged of the writer alone
// ([carrier]).
func (CursorAgent) ContinuesWriter(writer string) (bool, string) {
	if writer == cursorReleaseName {
		return true, ""
	}
	wrote := fmt.Sprintf("Cursor Agent %.64s", writer)
	if writer == "" {
		wrote = "a Cursor Agent that named no release this build reads"
	}
	return false, fmt.Sprintf("the session was written by %s, and a session is continued only between "+
		"boxes on Cursor Agent %s, the release this build supports", wrote, cursorSupported)
}

// continuesInPlace reports whether a box whose Cursor is release box continues
// a session at all ([inPlaceGate]): only on [cursorSupported], as the run's own
// gate holds it.
func (CursorAgent) continuesInPlace(box string) (bool, string) {
	if box == cursorReleaseName {
		return true, ""
	}
	runs := fmt.Sprintf("this box runs Cursor Agent %.64s", box)
	if box == "" {
		runs = "this box's Cursor Agent named no release this build reads"
	}
	return false, fmt.Sprintf("%s, and a session is continued only on Cursor Agent %s, the release this "+
		"build supports", runs, cursorSupported)
}

// meter is the take's meter of a Cursor archive ([carrier.meter]): no figure.
// The history holds no count of the tokens a request carried; a Cursor run's
// spend is read off its own stream at collection.
func (CursorAgent) meter(context.Context, io.Reader, string, string, *passBudget) (meterReading, error) {
	return meterReading{}, nil
}

// cursorCapture is the part of a run's script that packs its session once its
// harness has exited ([cursorPack]).
func cursorCapture(paths Paths) string {
	return cursorPack(paths, packAt{staged: paths.SessionStaging(), dest: paths.SessionCapture()})
}

// cursorPack packs the session s names into at.staged and moves it over
// at.dest, its digest kept ([packDigest]): its chat exported
// (cursor_helper.mjs), live beside a harness that may still be writing it,
// with the build that wrote it (its release file, [cursorReleaseFile]).
// An export refused, a chat holding a message the export does not know, or a
// writer not recorded, packs nothing and says why: never a conversation with
// a part left out.
func cursorPack(paths Paths, at packAt) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := shellword.Quote
	live := ""
	if at.working {
		live = " live"
	}
	b.WriteString(cursorHeld)
	line(`if [ -n "$hc" ] && [ -f "$hc/store.db" ]; then`)
	line(`  if command -v tar > /dev/null 2>&1 && command -v gzip > /dev/null 2>&1; then`)
	rel := q(paths.CursorSessions()) + `/"$s".release`
	line(`    rm -rf %s; if mkdir -p %s && { [ -s %s ] && cp -f %s %s/"$s".release || { echo 'the Cursor Agent build that wrote it is not recorded in this box' > %s; false; }; } && "$cn" %s export "$hc/store.db" %s/"$s".jsonl%s 2> %s && { tar -czf %s -C %s %s < /dev/null > /dev/null 2> %s; } && %s && mv -f %s %s; then packed=1; else echo "crewlet: the session $s was not packed to be continued in another box: $(cat %s 2> /dev/null)" >&2; fi`,
		q(at.staged), q(at.staged+"/in/"+cursorArchiveDir), rel, rel, q(at.staged+"/in/"+cursorArchiveDir),
		q(at.staged+"/error"), q(paths.CursorHelper()), q(at.staged+"/in/"+cursorArchiveDir),
		live, q(at.staged+"/error"), q(at.staged+"/capture"), q(at.staged+"/in"), cursorArchiveDir, q(at.staged+"/error"),
		packDigest(paths, "s", at.staged), q(at.staged+"/capture"), q(at.dest), q(at.staged+"/error"))
	line(`    rm -rf %s`, q(at.staged))
	line(`  else`)
	line(`    echo 'crewlet: this box has no tar and gzip, so the session was not packed to be continued in another box' >&2`)
	line(`  fi`)
	line(`fi`)
	return b.String()
}

// Pack packs session now ([carrier]), as a run's script does once its harness
// exits: [packNothing] where the box holds no chat of it, [packFailed] where
// no archive was made.
func (CursorAgent) Pack(paths Paths, session, staged, dest string, working bool) string {
	return cursorOnDemand(paths, session, packAt{staged: staged, dest: dest, working: working})
}

// PackSave packs a save of rec's session beside a harness that may still be
// working ([saver]): the export reads the store as it stands, a reader SQLite
// serves beside its writer. The record refuses no save.
func (CursorAgent) PackSave(paths Paths, rec sessionRecord, staged, dest string) (string, error) {
	return cursorOnDemand(paths, rec.id, packAt{staged: staged, dest: dest, working: true}), nil
}

// cursorOnDemand is a pack the engine makes of session outside the run's own
// script, at at.
func cursorOnDemand(paths Paths, session string, at packAt) string {
	return "s=" + shellword.Quote(session) + " packed=\n" + cursorNode(packFailed) + cursorHeld +
		fmt.Sprintf("[ -n \"$hc\" ] && [ -f \"$hc/store.db\" ] || exit %d\n", packNothing) +
		cursorPack(paths, at) +
		fmt.Sprintf("[ -n \"$packed\" ] || exit %d\n", packFailed)
}

// Output is where a job writes under `--output-format stream-json`: its stream
// is its result, decoded whole or followed ([cursorEvents]). The brief on
// stdin.
func (CursorAgent) Output(paths Paths) Output {
	return Output{Stdout: paths.Result(), Events: true, BriefOnStdin: true}
}

// Pins is the run's own Cursor directory and posture, over whatever the
// seat's env, the image or a profile set: CURSOR_CONFIG_DIR ([Paths.CursorDir],
// where Cursor's cli-config.json lives) and NO_OPEN_BROWSER, so a sign-in it
// asks for never tries a browser the box does not have.
func (CursorAgent) Pins(_ context.Context, _ sandbox.RunRequest, paths Paths) []Pinned {
	return []Pinned{
		{Name: "CURSOR_CONFIG_DIR", Value: paths.CursorDir()},
		{Name: "NO_OPEN_BROWSER", Value: "1"},
	}
}

// MCPCallCeiling is sixty seconds, which no setting moves: Cursor's MCP server
// schema holds no timeout, and a call held 200 seconds was cancelled at 60.1
// with `notifications/cancelled` and "MCP error -32001: Request timed out"
// (measured on 2026.11.22-e9042b2). The bridge answers a call still running
// before it ([Harness.MCPCallCeiling]).
func (CursorAgent) MCPCallCeiling() time.Duration { return cursorMCPCallCeiling }

// cursorMCPCallCeiling is [CursorAgent.MCPCallCeiling]'s.
const cursorMCPCallCeiling = 60 * time.Second

// Events is a new decoder for one stream, read whole or followed ([Decoder]).
func (CursorAgent) Events() Decoder { return &cursorEvents{} }

// Parse decodes a whole stream already in hand: the result of a run whose
// result IS its stream.
func (CursorAgent) Parse(stdout []byte) sandbox.Result { return decodeAll(&cursorEvents{}, stdout) }

// cursorHelperScript is cursor_helper.mjs, embedded so the box runs the
// helper this build tests.
//
//go:embed cursor_helper.mjs
var cursorHelperScript []byte

// errCursorWebDenied refuses a run whose entry denies the web: Cursor offers
// its web tools from its service and approves every fetch and search a run
// under --force makes, with no setting that turns them off (read from the
// 2026.11.22 bundle; the local-loop build offers none to measure).
var errCursorWebDenied = errors.New("executor: Cursor Agent has no setting that turns its web tools off, " +
	"so a run cannot honour web_tools: denied; allow the web on this entry or give the seat another harness")

// WriteConfig writes what a Cursor run reads: its helper, its standing
// instructions ([Paths.System]), and its MCP servers ([cursorServers]) at
// [Paths.MCPConfig], with a fresh session's copy beside it
// ([sandbox.RunRequest.FreshWiring]), returned as the run's config, "" for a
// run handed none. The script lays them into the home's own.
func (CursorAgent) WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, _ Session,
	paths Paths) (string, error) {
	if req.WebDenied {
		return "", errCursorWebDenied
	}
	if req.Effort != "" {
		return "", fmt.Errorf("executor: Cursor Agent takes no reasoning effort of its own (%q): name a model "+
			"variant that carries it, as `cursor-agent --list-models` lists them", req.Effort)
	}
	if err := box.WriteFile(ctx, paths.CursorHelper(), cursorHelperScript); err != nil {
		return "", fmt.Errorf("executor: writing the Cursor run's helper: %w", err)
	}
	if req.System != "" {
		if err := box.WriteFile(ctx, paths.System(), []byte(req.System)); err != nil {
			return "", fmt.Errorf("executor: writing the run's standing instructions: %w", err)
		}
	}
	if len(req.MCPServers) == 0 {
		return "", nil
	}
	nonce, err := cursorNonce()
	if err != nil {
		return "", err
	}
	if err = writeCursorServers(ctx, box, paths.MCPConfig(), req.MCPServers, nonce); err != nil {
		return "", err
	}
	if f := req.FreshWiring; f != nil {
		if err = writeCursorServers(ctx, box, paths.MCPConfigFresh(), f.MCPServers, nonce); err != nil {
			return "", err
		}
	}
	return paths.MCPConfig(), nil
}

// cursorNonce is the run's own suffix to its servers' names: four random
// bytes, hex. A server of the home's or a repository's under the same name
// would replace the run's (measured on 2026.11.19), and none can be named
// for one it cannot know.
func cursorNonce() (string, error) {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("executor: naming the run's MCP servers: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// cursorServerName is server's name in the home's mcp.json for the run whose
// suffix is nonce.
func cursorServerName(server, nonce string) string { return server + "-" + nonce }

// writeCursorServers writes servers as Cursor's mcp.json reads them, each
// under its run's name ([cursorServerName]): an HTTP server's url and headers,
// a command's command, args and env. Cursor's schema holds no timeout
// ([CursorAgent.MCPCallCeiling]).
func writeCursorServers(ctx context.Context, box sandbox.Sandbox, file string,
	servers map[string]sandbox.MCPServer, nonce string) error {
	out := map[string]map[string]any{}
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		s := servers[name]
		entry := map[string]any{}
		if s.Transport == sandbox.TransportHTTP {
			entry["url"] = s.URL
			if len(s.Headers) > 0 {
				entry["headers"] = s.Headers
			}
		} else {
			entry["command"] = s.Command
			entry["args"] = s.Args
			if len(s.Env) > 0 {
				entry["env"] = s.Env
			}
		}
		out[cursorServerName(name, nonce)] = entry
	}
	raw, err := json.Marshal(map[string]any{"mcpServers": out})
	if err != nil {
		return fmt.Errorf("executor: rendering the Cursor run's MCP servers: %w", err)
	}
	if err = box.WriteFile(ctx, file, raw); err != nil {
		return fmt.Errorf("executor: writing the Cursor run's MCP servers: %w", err)
	}
	return nil
}

// artifactFiles is the files a Cursor run leaves in its box of its own,
// removed before every run: its servers' copies, its standing instructions
// and the history it hands a continued run.
func (CursorAgent) artifactFiles(p Paths) []string {
	return []string{p.MCPConfig(), p.MCPConfigFresh(), p.System()}
}

// artifactDirs is the directories a Cursor run leaves in its box of its own,
// removed whole: its run's own (the helper, its pid and cap marks, the
// history). The home's .cursor is a person's, and the box's copies of its
// sessions are kept.
func (CursorAgent) artifactDirs(p Paths) []string { return []string{p.CursorRun()} }

// cursorChargeBytes is the fewest bytes a Cursor session's brief is counted at
// a token: one, the most tokens any estimate can make of n bytes, since no
// count of Cursor's is read.
const cursorChargeBytes sandbox.Charge = 1

// sessionTraits is how a relaunch judges a Cursor session, each value the one
// that never under-counts its next request, as Pi's ([Pi.sessionTraits]):
// Cursor reports no window, and compacts where its service decides.
func (CursorAgent) sessionTraits() sandbox.SessionTraits {
	return sandbox.SessionTraits{FirstRequest: sandbox.FirstRequestBox, CallKeying: sandbox.CallKeyingNone,
		Charge: cursorChargeBytes}
}
