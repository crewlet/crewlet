package executor

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// kimiArchiveFormat is the layout a Kimi run packs its session in
// ([kimiPack]) and the one it restores ([kimiSessionGate]): one gzipped tar
// whose members are named relative to the home's sessions directory,
// `wd_<key>/session_<id>/` and under it `state.json`, `agents/` (each agent's
// wire.jsonl, the conversation, and its tasks' output) and `notify/` where the
// session has one. Never `logs/`, nor anything outside the session: the login
// (`credentials/`, `config.toml`) and `device_id` stay in the box. A change is
// a new format.
const kimiArchiveFormat = 1

// ArchiveFormat is the layout a Kimi run packs its session in ([carrier]).
func (Kimi) ArchiveFormat() int { return kimiArchiveFormat }

// VersionCommand prints a box's Kimi Code release ([carrier]).
func (Kimi) VersionCommand() string { return "kimi --version" }

// layout is a Kimi archive's: a gzipped tar of the session's files.
func (Kimi) layout() archiveLayout { return archiveTar }

// charges is false: its meter counts nothing ([carrier.charges]).
func (Kimi) charges(string, string) bool { return false }

// Resumes says Kimi continues a session it reported: KAP opens one its home
// holds, in whatever directory under `sessions/` it is filed (measured on
// 2.7.3).
func (Kimi) Resumes() bool { return true }

// Continues reports whether a session written by Kimi Code release writer can
// be continued in a box whose Kimi is release box ([carrier]): only both on
// [kimiSupported], the one release a session's record and its renewed
// instructions were measured on. A release that cannot be read is no release.
func (Kimi) Continues(box, writer string) (bool, string) {
	if box == kimiSupported && writer == kimiSupported {
		return true, ""
	}
	return false, fmt.Sprintf("a Kimi Code session is continued only from %[1]s to %[1]s, the release Crewlet "+
		"supports; this box runs %.64[2]q and the session was written by %.64[3]q", kimiSupported, box, writer)
}

// ContinuesWriter is [Kimi.Continues] judged of the writer alone ([carrier]):
// only a session [kimiSupported] wrote, one written by any other release or by
// one that cannot be read refused, saying so.
func (Kimi) ContinuesWriter(writer string) (bool, string) {
	if writer == kimiSupported {
		return true, ""
	}
	return false, fmt.Sprintf("a Kimi Code session is continued only from %[1]s to %[1]s, the release Crewlet "+
		"supports; the session was written by %.64[2]q", kimiSupported, writer)
}

// meter is the take's meter of a Kimi archive ([carrier.meter]): no figure.
// This build reads no count or estimate of a Kimi session's next request off
// its archive; a Kimi run's window is read off its own records at collection.
// The archive is left to the caller, which drains it ([startMeter]).
func (Kimi) meter(context.Context, io.Reader, string, string, *passBudget) (meterReading, error) {
	return meterReading{}, nil
}

// continuesInPlace reports whether a box whose Kimi Code is release box
// continues a session at all, the copy it holds as much as one carried to it
// ([inPlaceGate]): only on [kimiSupported], as the run's own gate holds it
// ([kimiSessionGate]), so the engine asks before a relaunch hands the brief
// that continues the box's copy.
func (Kimi) continuesInPlace(box string) (bool, string) {
	if box == kimiSupported {
		return true, ""
	}
	if box == "" {
		box = "a release it could not read"
	}
	return false, fmt.Sprintf("this box runs Kimi Code %.64s, and a session is continued only on %s, the "+
		"release Crewlet supports", box, kimiSupported)
}

// kimiSessionGate is the part of a run's script that names the session its
// driver runs in (s), whether it continues the one asked (k) and, where it
// could not, why (w), setting the driver's arguments. As [copilotSessionGate]:
// the box's copy is continued unless a carried archive holds what it does not
// (a digest not in [Paths.SessionDigests]); that archive is restored once it
// holds the session's main record and no link, its digest recorded; either is
// then filed under the run's own directory by the driver (kimi_driver.js's
// rekey), since Kimi runs a session in the cwd its state.json names. On any
// box release but [kimiSupported] nothing is touched. Neither held nor
// restored nor filed, the run starts a session of its own, which its driver
// reports, and says why.
func kimiSessionGate(session Session, paths Paths, rewired bool) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := shellword.Quote
	if !session.Continues {
		line(`s= k=no w=`)
		return b.String()
	}
	sessions := q(paths.KimiSessions())
	staged := paths.SessionStaging()
	carried := q(paths.SessionCarried())
	in := q(staged + "/in")
	restore := strings.Join([]string{
		"rm -rf " + q(staged),
		"mkdir -p " + in + " " + sessions,
		"tar -xzf " + carried + " -C " + in + " < /dev/null > /dev/null 2>&1",
		`x=$(for y in ` + in + `/*/"$s"; do [ -d "$y" ] && printf '%s\n' "$y"; done)`,
		`[ -n "$x" ]`,
		`[ "$(printf '%s\n' "$x" | wc -l)" -eq 1 ]`,
		`[ ! -L "$x" ]`, `[ -f "$x/state.json" ]`, `[ -f "$x/agents/main/wire.jsonl" ]`,
		// NO LINK ANYWHERE IN IT: the session's files are moved into the
		// home and written by the harness, so one could reach past it.
		`[ -z "$(find "$x" -type l 2> /dev/null)" ]`,
		// THE BOX'S OWN COPY GOES only now, once the archive is known to hold
		// a conversation.
		`for y in ` + sessions + `/*/"$s"; do rm -rf "$y"; done`,
		`src=$x`,
	}, " && ")
	line(`s=%s k=yes w= h= d= src=`, q(session.ID))
	// THE SUPPORTED RELEASE ALONE CONTINUES ONE, read bare as the engine reads
	// it ([bareReleaseShell], [Kimi.continuesInPlace]): the run's loose n takes
	// 2.7.3-rc.1 for 2.7.3, which the engine reads as no release.
	line(`%s[ "$rv" = %s ] || w="this box runs Kimi Code ${rv:-a release it could not read}, and a session is continued only on %s, the release Crewlet supports"`,
		bareReleaseShell("rv"), kimiSupported, kimiSupported)
	line(`for y in %s/*/"$s"; do [ -s "$y/agents/main/wire.jsonl" ] && [ ! -L "$y" ] && src=$y && h=1; done`, sessions)
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
	// Continued only once its digest is recorded and read back, for
	// [claudeSessionGate]'s reason.
	line(`  elif ! { %s; }; then w=%s`, recordDigest(paths, "s"), q(restoreDigestUnrecorded))
	line(`  fi`)
	line(`fi`)
	line(`rm -f %s`, carried)
	// FILED UNDER THIS RUN'S DIRECTORY, or not continued: a session left in
	// the cwd its writer worked in would run its tools there.
	line(`if [ -z "$w" ] && ! node %s %s rekey "$s" "$src"; then w='its directory could not be filed under this run'"'"'s working directory'; fi`,
		q(paths.KimiDriver()), q(paths.KimiSettings()))
	line(`rm -rf %s`, q(staged))
	line(`if [ -z "$w" ]; then`)
	line(`  set -- resume "$s"`)
	if rewired {
		line(`  rm -f %s %s`, q(paths.KimiMCPConfigFresh()), q(paths.KimiSettingsFresh()))
	}
	line(`else`)
	line(`  printf '%%s%%s\n' %s "$w" >&2`, q("crewlet: this run was asked to continue the Kimi Code session "+
		session.ID+", and starts a new session instead: "))
	line(`  s= k=no`)
	line(`  [ ! -s %s ] || exec < %s`, q(paths.FreshBrief()), q(paths.FreshBrief()))
	if rewired {
		// THE NEW SESSION WIRED TO ITS OWN TOKEN, the bridge's and the notes
		// route's alike, or not started: [claudeSessionGate]'s reason.
		line(`  mv -f %s %s || { echo 'crewlet: the new session could not be wired to the tools of its seat, so the run does not start' >&2; exit 1; }`,
			q(paths.KimiMCPConfigFresh()), q(paths.KimiMCPConfig()))
		line(`  mv -f %s %s || { echo 'crewlet: the new session could not be wired to its notes, so the run does not start' >&2; exit 1; }`,
			q(paths.KimiSettingsFresh()), q(paths.KimiSettings()))
	}
	line(`fi`)
	return b.String()
}

// kimiLinesBefore is the part of a run's script that records, for a run that
// continues its session, how many lines each agent's wire.jsonl held before
// its driver started ([Paths.KimiLinesBefore]): what an earlier run wrote,
// which the usage after the run is read past ([kimiUsage]).
func kimiLinesBefore(paths Paths) string {
	q := shellword.Quote
	return `: > ` + q(paths.KimiLinesBefore()) + `; if [ -n "$s" ]; then for w in ` + q(paths.KimiSessions()) +
		`/*/"$s"/agents/*/wire.jsonl; do [ -f "$w" ] && printf '%s %s\n' "$(wc -l < "$w" | tr -d ' ')" "$w"; done >> ` +
		q(paths.KimiLinesBefore()) + `; fi
`
}

// kimiSessionOf is the shell that settles s, the session a pack takes, as the
// run's: the one it continued, else the one its driver reported, held to the
// shape of an id Kimi mints, and "" for neither.
func kimiSessionOf(paths Paths) string {
	return `[ -n "$s" ] || s=$(cat ` + shellword.Quote(paths.SessionReport()) + ` 2> /dev/null) || s=
case "$s" in session_*) case "$s" in *[!A-Za-z0-9_-]*) s= ;; esac ;; *) s= ;; esac
`
}

// kimiCapture is the part of a run's script that packs its session once its
// driver has exited ([kimiPack]).
func kimiCapture(paths Paths) string {
	return kimiPack(paths, packAt{staged: paths.SessionStaging(), dest: paths.SessionCapture()})
}

// kimiPack packs the session s names into at.staged and moves it over at.dest,
// its digest kept ([packDigest]): its state.json, agents/ and notify/, named
// relative to the home's sessions directory ([kimiArchiveFormat]). Never logs/.
// A pack beside a working harness is held to [claudeHeldTar]'s rule: every
// record is append-only.
func kimiPack(paths Paths, at packAt) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := shellword.Quote
	sessions := paths.KimiSessions()
	line(`x=; if [ -n "$s" ]; then for y in %s/*/"$s"; do [ -s "$y/agents/main/wire.jsonl" ] && x=$y; done; fi`, q(sessions))
	line(`if [ -n "$x" ]; then`)
	line(`  if command -v tar > /dev/null 2>&1 && command -v gzip > /dev/null 2>&1; then`)
	// NAMED FROM THE SESSIONS DIRECTORY DOWN, `wd_<key>/session_<id>/…`.
	line(`    r=${x#"${x%%/*/*}/"}; set -- "$r/state.json" "$r/agents"; [ ! -d "$x/notify" ] || set -- "$@" "$r/notify"`)
	tar := fmt.Sprintf(`{ tar -czf %s -C %s "$@" < /dev/null > /dev/null 2> %s; t=$?; [ "$t" = 0 ] || { [ "$t" = 1 ] && grep -q 'changed as we read it' %s; }; }`,
		q(at.staged+"/capture"), q(sessions), q(at.staged+"/error"), q(at.staged+"/error"))
	if at.working {
		tar = claudeHeldTar(sessions, at.staged)
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

// Pack packs session now ([carrier]), as a run's script does once its driver
// exits, into staged and over dest: [packNothing] where the box holds no
// record of it, [packFailed] where no archive was made. Held to the rule of a
// pack beside a working harness where working says so.
func (Kimi) Pack(paths Paths, session, staged, dest string, working bool) string {
	return kimiOnDemand(paths, session, packAt{staged: staged, dest: dest, working: working})
}

// PackSave packs a save of rec's session beside a harness that may still be
// working ([saver]): every record is append-only, so the rule of a pack beside
// a working harness holds it ([claudeHeldTar]). rec names the session bound
// to the job ([Runner.boundRecord]). The record refuses no save.
func (Kimi) PackSave(paths Paths, rec sessionRecord, staged, dest string) (string, error) {
	return kimiOnDemand(paths, rec.id, packAt{staged: staged, dest: dest, working: true}), nil
}

// kimiOnDemand is a pack the engine makes of session outside the run's own
// script, at at. ONLY THE SESSION THE ENGINE NAMES, the one bound to the job,
// never one the box reports now, which was never held to the bind's rules.
func kimiOnDemand(paths Paths, session string, at packAt) string {
	return "s=" + shellword.Quote(session) + " packed=\n" +
		fmt.Sprintf("[ -n \"$s\" ] || exit %d\n", packNothing) +
		kimiPack(paths, at) +
		fmt.Sprintf("[ -n \"$packed\" ] || { [ -n \"$x\" ] || exit %d; exit %d; }\n", packNothing, packFailed)
}

// kimiChargeBytes is the fewest bytes a Kimi session's brief is counted at a
// token: one, the most tokens any estimate can make of n bytes, since Kimi's
// own count is not read.
const kimiChargeBytes sandbox.Charge = 1

// sessionTraits is how a relaunch judges a Kimi session, each value the one
// that never under-counts its next request, for [Copilot.sessionTraits]'s
// reasons: Kimi compacts at a share of its alias's window this build does not
// read before the run (the run reads it after, [kimiEvents.reportedWindow]).
func (Kimi) sessionTraits() sandbox.SessionTraits {
	return sandbox.SessionTraits{FirstRequest: sandbox.FirstRequestBox, CallKeying: sandbox.CallKeyingNone,
		Charge: kimiChargeBytes}
}
