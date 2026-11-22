package executor

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A CODEX SESSION CARRIED BETWEEN BOXES is its rollout alone (codexrollout.go):
// a resume needs nothing else of CODEX_HOME, finds the file by its name
// anywhere under sessions/, and continues it in another cwd (measured on
// 0.171.0). So the archive is a gzipped tar of ONE member,
// [codexArchiveMember], laid out as Claude Code's is ([archiveTar]): the scrub
// rewrites a `.jsonl` member line by line, as it does a Claude transcript. A
// pack copies the rollout's whole lines only, never a line Codex is still
// writing; the rollout is append-only, so a copy beside a working Codex is a
// prefix of the session and needs no lock ([Codex.PackSave]).

// The capabilities the carry gives [Codex], held at compile time.
var (
	_ carrier      = Codex{}
	_ saver        = Codex{}
	_ resumer      = Codex{}
	_ resumeReader = Codex{}
)

// codexCarryFloor is the oldest Codex release a box continues a carried
// session on, and the oldest whose session it continues: 0.171.0, the release
// the rollout's name, its portability to a fresh CODEX_HOME and another cwd,
// and a resume after SIGTERM were measured on.
const codexCarryFloor = "0.171.0"

// codexArchiveFormat is the layout a Codex run packs its session in: a gzipped
// tar of the rollout's whole lines as one member ([codexArchiveMember]),
// restored to [codexRestoredRollout]. A change to what is packed is a new
// format, which a box of this build refuses rather than reads.
const codexArchiveFormat = 1

// codexArchiveMember is the one member of a Codex archive: the rollout under
// the name it is restored by, which a resume finds ([codexRestoredRollout]).
func codexArchiveMember(threadID string) string { return codexCarriedName(threadID) }

// Resumes says Codex continues a session it reported: `exec resume <id>`
// continues the rollout of that thread, under the same id.
func (Codex) Resumes() bool { return true }

// ArchiveFormat is the layout a Codex run packs its session in ([carrier]).
func (Codex) ArchiveFormat() int { return codexArchiveFormat }

// VersionCommand prints a box's Codex release ([carrier]): `codex-cli 0.171.0`.
func (Codex) VersionCommand() string { return "codex --version" }

// layout is a Codex archive's: a gzipped tar of one `.jsonl` member
// ([codexArchiveFormat]).
func (Codex) layout() archiveLayout { return archiveTar }

// Continues reports whether a session written by Codex release writer can be
// continued in a box whose Codex is release box ([carrier]): both at or after
// [codexCarryFloor], and never a box older than the writer. The writer is the
// release that LAST ran the session (the record's `--version`), never the
// rollout's session_meta.cli_version, which names the release that began it:
// a resume appends to the rollout and writes no new session_meta (measured on
// 0.171.0: one session_meta, at line 0, in every rollout, resumed or not).
func (Codex) Continues(box, writer string) (bool, string) {
	switch {
	case !releaseAtLeast(box, codexCarryFloor):
		return false, fmt.Sprintf("this box runs Codex %s, older than %s, the oldest that "+
			"continues a session carried from another box", box, codexCarryFloor)
	case !releaseAtLeast(writer, codexCarryFloor):
		return false, fmt.Sprintf("the session was written by Codex %s, older than %s, the "+
			"oldest whose session another box continues", writer, codexCarryFloor)
	case !releaseAtLeast(box, writer):
		return false, carriedOlder("Codex", box, writer)
	}
	return true, ""
}

// resumeRead reports whether release is one whose resume this build read: a
// SET, never a range, as Claude Code's is ([claudeResumeRead]). 0.171.0's
// resume was measured: it replays the rollout whole, appends the new turn to
// the same file, and replays a call cut off by SIGTERM as `aborted`.
func (Codex) resumeRead(release string) bool { return release == codexCarryFloor }

// Pack packs session now ([carrier]), as the run's script does once Codex
// exits ([codexCapture]): into staged and over dest, its digest kept;
// [packNothing] where the box holds no rollout of it with a whole line,
// [packFailed] where no archive was made. Whether Codex may still be writing
// changes nothing: the rollout is append-only and only its whole lines are
// copied.
func (Codex) Pack(paths Paths, session, staged, dest string, _ bool) string {
	return codexOnDemand(paths, session, staged, dest)
}

// PackSave packs a save of rec's session beside a Codex that may still be
// working ([saver]), as [Codex.Pack] does. A record naming no session is
// refused before any script runs: a minting run that reported none has
// nothing a save could be filed under.
func (Codex) PackSave(paths Paths, rec sessionRecord, staged, dest string) (string, error) {
	if !sandbox.ValidSessionID(rec.id) {
		return "", fmt.Errorf("executor: the run names no Codex session %q, so none is saved", rec.id)
	}
	return codexOnDemand(paths, rec.id, staged, dest), nil
}

// codexOnDemand is a pack the engine makes of session outside the run's own
// script: [packNothing] where the box holds nothing of it to pack,
// [packFailed] where no archive was made.
func codexOnDemand(paths Paths, session, staged, dest string) string {
	return "sid=" + sandbox.ShellQuote(session) + " packed=\n" +
		codexPack(paths, staged, dest, fmt.Sprintf("exit %d", packNothing)) +
		fmt.Sprintf("[ -n \"$packed\" ] || exit %d\n", packFailed)
}

// codexCapture is the part of a run's script that packs the session its Codex
// ran in, the id in sid (on a fresh run, the one Codex minted and the script
// reported), once `exec` has exited and before the app-server stops: over [Paths.SessionCapture],
// made in [Paths.SessionStaging]. Nothing to pack is said nowhere, as Claude
// Code's capture says nothing ([claudeCapture]); a failed pack is said on the
// error stream, and the run's status is never the pack's.
func codexCapture(paths Paths) string {
	return codexPack(paths, paths.SessionStaging(), paths.SessionCapture(), ":")
}

// codexPack is ONE PACK, the run's script's at its end, a pack on demand and a
// save alike: the rollout of the thread whose id is in sid copied to its whole
// lines (wc counts the newlines, head copies that many lines of a file that
// only grows) under [codexArchiveMember], tarred and gzipped in staged, its
// digest recorded ([packDigest]) and moved over dest; packed=1 where it was.
// nothing is the shell run where the box holds no rollout of sid with a whole
// line. An id with a character a pattern could misread finds no rollout.
func codexPack(paths Paths, staged, dest, nothing string) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := sandbox.ShellQuote
	member := `rollout-` + codexCarriedStamp + `-"$sid".jsonl`
	line(`pf= pn=0`)
	line(`case "$sid" in ''|*[!A-Za-z0-9_-]*) ;; *) if command -v find > /dev/null 2>&1; then pf=$(find %s -type f -name "rollout-*-$sid.jsonl" 2> /dev/null | LC_ALL=C sort | head -n 1); else echo 'crewlet: this box has no find, so the session was not packed to be continued in another box' >&2; fi;; esac`,
		q(paths.CodexSessions()))
	line(`if [ -n "$pf" ] && [ -f "$pf" ] && [ ! -L "$pf" ]; then pn=$(wc -l < "$pf" 2> /dev/null) || pn=0; fi`)
	line(`if [ "$pn" -gt 0 ] 2> /dev/null; then`)
	line(`  if command -v tar > /dev/null 2>&1 && command -v gzip > /dev/null 2>&1; then`)
	line(`    rm -rf %[1]s; if mkdir -p %[2]s && head -n "$pn" "$pf" > %[2]s/%[3]s 2> %[4]s && tar -czf %[5]s -C %[2]s %[3]s < /dev/null > /dev/null 2>> %[4]s && %[6]s && mv -f %[5]s %[7]s; then packed=1; else echo "crewlet: the session $sid was not packed to be continued in another box: $(cat %[4]s 2> /dev/null)" >&2; fi`,
		q(staged), q(staged+"/in"), member, q(staged+"/error"), q(staged+"/capture"),
		packDigest(paths, "sid", staged), q(dest))
	line(`    rm -rf %s`, q(staged))
	line(`  else`)
	line(`    echo 'crewlet: this box has no tar and gzip, so the session was not packed to be continued in another box' >&2`)
	line(`  fi`)
	line(`else`)
	line(`  %s`, nothing)
	line(`fi`)
	return b.String()
}

// CodexConfig is where the runner writes a Codex run's configuration, and
// CodexConfigFresh the same configuration wired to the fresh session a
// continued run may start instead ([sandbox.RunRequest.FreshWiring]); the
// session gate moves the one the run starts with to [Paths.CodexLiveConfig]
// ([codexSessionGate]). Under WorkDir, so a `git clean` cannot take them.
func (p Paths) CodexConfig() string { return p.WorkDir() + "/codex-config.toml" }

// CodexConfigFresh is [Paths.CodexConfig] wired to the fresh session.
func (p Paths) CodexConfigFresh() string { return p.WorkDir() + "/codex-config-fresh.toml" }

// CodexLiveConfig is the configuration Codex reads, `config.toml` in
// [Paths.CodexHome].
func (p Paths) CodexLiveConfig() string { return p.CodexHome() + "/config.toml" }

// codexSessionGate is the part of a run's script that names the session its
// Codex runs in, as sid ("" for one Codex mints as it starts), whether it is
// the one asked to continue, as k, and where it was asked and could not, why,
// as w; and installs the configuration the run starts with. The runner's
// script writes the record after it, resumes sid where k is yes, and
// otherwise starts a new thread whose minted id it reports ([reportSession]).
//
// A run that continues one continues the rollout its box HOLDS unless a
// carried archive ([Paths.SessionCarried]) holds what the copy does not (a
// digest not among the box's, [Paths.SessionDigests]) or the copy is one Codex
// was cut off in ([Session.Replaces]); then the archive is RESTORED
// ([codexRestore]) and its digest recorded before the run goes on.
func codexSessionGate(session Session, paths Paths, rewired bool) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := sandbox.ShellQuote
	if !session.Continues {
		line(`sid= k=no w=`)
		b.WriteString(codexInstallConfig(paths, paths.CodexConfig()))
		return b.String()
	}
	carried := q(paths.SessionCarried())
	line(`sid=%s k=yes w= h= d=`, q(session.ID))
	line(`rf=$(%s)`, codexRolloutPath(paths, session.ID))
	line(`if [ -n "$rf" ] && [ -s "$rf" ]; then h=1; fi`)
	line(`if [ -s %s ]; then d=$(cksum < %s 2> /dev/null) || d=; fi`, carried, carried)
	if session.Replaces {
		// A COPY CODEX WAS CUT OFF IN is replaced by the archive whatever it
		// holds, as Claude Code's is ([claudeSessionGate]).
		line(`if [ -n "$h" ] && [ -s %s ]; then h=; fi`, carried)
	} else {
		line(`if [ -n "$h" ] && [ -n "$d" ] && ! grep -qxF "$sid $d" %s 2> /dev/null; then h=; fi`,
			q(paths.SessionDigests()))
	}
	line(`if [ -z "$h" ]; then`)
	line(`  if [ ! -s %s ]; then w='it is not in this box, and no archive of it was carried here'`, carried)
	line(`  elif ! command -v tar > /dev/null 2>&1 || ! command -v gzip > /dev/null 2>&1 || ! command -v find > /dev/null 2>&1; then w='this box has no tar, gzip and find to restore its archive with'`)
	line(`  elif ! { %s; }; then w='its archive could not be restored'`, codexRestore(paths, session.ID))
	// CONTINUED ONLY ONCE ITS DIGEST IS RECORDED and read back, for the reason
	// Claude Code's restore is ([claudeSessionGate]).
	line(`  elif ! { %s; }; then w=%s`, recordDigest(paths, "sid", "d"), q(restoreDigestUnrecorded))
	line(`  fi`)
	line(`  rm -rf %s`, q(paths.SessionStaging()))
	line(`fi`)
	line(`rm -f %s`, carried)
	line(`if [ -z "$w" ]; then`)
	// A ROLLOUT CUT OFF MID-LINE is cut back to its last whole line. Codex
	// passes a fragment over and writes on past it (measured), but the
	// fragment stays in the file, and a pack copies whole lines alone: cut
	// here, the box's copy and every archive of it agree. Torn is where the
	// last byte is no newline, as wc counts it ([claudeSessionGate]'s torn).
	line(`  rf=$(%s)`, codexRolloutPath(paths, session.ID))
	line(`  if [ -n "$rf" ] && [ -s "$rf" ] && [ "$(tail -c 1 "$rf" | wc -l)" -eq 0 ]; then cl=$(wc -l < "$rf"); if [ "$cl" -gt 0 ] 2> /dev/null; then head -n "$cl" "$rf" > "$rf.cut" && mv -f "$rf.cut" "$rf"; fi; fi`)
	if rewired {
		line(`  rm -f %s`, q(paths.CodexConfigFresh()))
	}
	b.WriteString(codexIndented(codexInstallConfig(paths, paths.CodexConfig())))
	line(`else`)
	line(`  sid= k=no`)
	line(`  printf '%%s%%s\n' %s "$w" >&2`, q("crewlet: this run was asked to continue the Codex session "+
		session.ID+", and starts a new session instead: "))
	// A NEW SESSION IS HANDED THE BRIEF WRITTEN FOR ONE where the run was
	// given it ([sandbox.RunRequest.FreshBrief]), and, where it was wired for
	// one ([sandbox.RunRequest.FreshWiring]), the configuration whose bridge
	// token tells the bridge it started its own: never the continued
	// session's token, whose skill guard it would be held to.
	line(`  [ ! -s %s ] || exec < %s`, q(paths.FreshBrief()), q(paths.FreshBrief()))
	config := paths.CodexConfig()
	if rewired {
		line(`  rm -f %s`, q(paths.CodexConfig()))
		config = paths.CodexConfigFresh()
	}
	b.WriteString(codexIndented(codexInstallConfig(paths, config)))
	line(`fi`)
	return b.String()
}

// codexInstallConfig moves config over [Paths.CodexLiveConfig], ending the
// run before Codex starts where it cannot: a run on no configuration, or on
// another session's, is one the bridge cannot tell apart.
func codexInstallConfig(paths Paths, config string) string {
	q := sandbox.ShellQuote
	return fmt.Sprintf("mkdir -p %s && mv -f %s %s || { echo 'crewlet: the Codex configuration of the run could not be installed, so the run does not start' >&2; exit 1; }\n",
		q(paths.CodexHome()), q(config), q(paths.CodexLiveConfig()))
}

// codexIndented is lines, each indented by two spaces, as a branch of an if.
func codexIndented(lines string) string {
	return "  " + strings.ReplaceAll(strings.TrimSuffix(lines, "\n"), "\n", "\n  ") + "\n"
}

// codexRestore is the restore of a carried archive of thread: unpacked in
// staging, its one member required to be a plain file, every other rollout of
// the thread under sessions/ removed (a resume takes the first it finds), and
// the member moved to [codexRestoredRollout] LAST, so a box that holds it
// holds it whole.
func codexRestore(paths Paths, thread string) string {
	q := sandbox.ShellQuote
	in := paths.SessionStaging() + "/in"
	from := in + "/" + codexArchiveMember(thread)
	return strings.Join([]string{
		"rm -rf " + q(paths.SessionStaging()),
		"mkdir -p " + q(in) + " " + q(paths.CodexSessions()),
		"tar -xzf " + q(paths.SessionCarried()) + " -C " + q(in) + " < /dev/null > /dev/null 2>&1",
		"[ -f " + q(from) + " ]",
		"[ ! -L " + q(from) + " ]",
		"find " + q(paths.CodexSessions()) + " -type f -name " + q("rollout-*-"+thread+".jsonl") +
			" -exec rm -f {} +",
		"mv " + q(from) + " " + q(codexRestoredRollout(paths, thread)),
	}, " && ")
}
