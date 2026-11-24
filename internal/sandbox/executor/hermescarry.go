package executor

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A HERMES SESSION CARRIED BETWEEN BOXES is the session's rows of Hermes's
// SQLite store, as TEXT: the archive is a gzipped tar of ONE member,
// [hermesArchiveMember], JSON lines holding the store's schema and the
// session's rows with its compaction lineage ([hermesCarryScript]), and a
// restore builds a new store from them, the full-text index refilled by the
// store's own triggers. Text, because the scrub rewrites a member's text and
// would break a database file's pages. Measured on 0.23.4: a store restored
// in a fresh home at the same path resumes the session with its whole history.
// Under another home path Hermes refuses it (the frozen launch's credentials
// belong to another home), but the gate holds the working directory frozen
// with the session too, so such a box starts afresh before that.

// The capabilities the carry gives [Hermes], held at compile time.
var (
	_ carrier     = Hermes{}
	_ saver       = Hermes{}
	_ resumer     = Hermes{}
	_ inPlaceGate = Hermes{}
)

//go:embed hermes_carry.py
var hermesCarryScript []byte

// hermesArchiveFormat is the layout a Hermes run packs its session in
// ([hermesArchiveMember]). A change to what is packed is a new format, which a
// box of this build refuses rather than reads.
const hermesArchiveFormat = 1

// hermesArchiveMember is the one member of a Hermes archive.
const hermesArchiveMember = "session.jsonl"

// charges is false: its meter counts nothing ([carrier.charges]).
func (Hermes) charges(string, string) bool { return false }

// Resumes says Hermes continues a session the runner named: `-c <title>`
// continues the session of that title, and its compaction lineage.
func (Hermes) Resumes() bool { return true }

// ArchiveFormat is the layout a Hermes run packs its session in ([carrier]).
func (Hermes) ArchiveFormat() int { return hermesArchiveFormat }

// VersionCommand prints a box's Hermes release ([carrier]).
func (Hermes) VersionCommand() string { return "hermes --version" }

// layout is a Hermes archive's: a gzipped tar of one `.jsonl` member.
func (Hermes) layout() archiveLayout { return archiveTar }

// Continues reports whether a session written by Hermes release writer can be
// continued in a box whose Hermes is release box ([carrier]): only where the
// box continues any session and the writer is as [Hermes.ContinuesWriter]
// holds it.
func (h Hermes) Continues(box, writer string) (bool, string) {
	if ok, why := h.continuesInPlace(box); !ok {
		return false, why
	}
	return h.ContinuesWriter(writer)
}

// continuesInPlace reports whether a box whose Hermes is release box continues
// a session at all, its own copy as much as a carried one ([inPlaceGate]):
// only on exactly [hermesFloor]. Any other, older or newer or none that
// reads, starts fresh, saying which it found.
func (Hermes) continuesInPlace(box string) (bool, string) {
	switch {
	case !releaseReads(box):
		return false, hermesNoRelease
	case box != hermesFloor():
		return false, fmt.Sprintf(hermesBoxRefused, hermesReleaseFound(box), hermesFloor())
	}
	return true, ""
}

// ContinuesWriter reports whether a session Hermes release writer wrote can be
// continued at all ([carrier]): only where the writer is exactly
// [hermesFloor], the release whose store this build reads and writes.
func (Hermes) ContinuesWriter(writer string) (bool, string) {
	if writer != hermesFloor() {
		return false, fmt.Sprintf("the session was written by %s, and only a session Hermes Agent %s wrote "+
			"is continued", hermesReleaseFound(writer), hermesFloor())
	}
	return true, ""
}

// hermesBoxRefused is why a box whose Hermes is not [hermesFloor] continues no
// session, the release found and the one needed: the engine's checks and the
// run's own gate say it alike ([hermesSessionGate]).
const hermesBoxRefused = "this box runs %s, and only Hermes Agent %s continues a session, the box's own " +
	"copy or one carried from another box"

// hermesNoRelease is why a box whose Hermes named no release this build reads
// continues no session.
const hermesNoRelease = "this box's Hermes named no release this build reads, so it continues no session, " +
	"its own copy nor one carried from another box"

// hermesReleaseFound is release as a reason names the Hermes found.
func hermesReleaseFound(release string) string {
	if release == "" {
		return "a Hermes that named no release"
	}
	return "Hermes Agent " + release
}

// Pack packs session now ([carrier]), as the run's script does once Hermes
// exits ([hermesFinish]). Whether Hermes may still be working changes
// nothing: the store is read in its own WAL mode, a consistent snapshot
// beside a writing gateway.
func (Hermes) Pack(paths Paths, session, staged, dest string, _ bool) string {
	return hermesOnDemand(paths, session, staged, dest)
}

// PackSave packs a save of rec's session beside a Hermes that may still be
// working ([saver]), as [Hermes.Pack] does.
func (Hermes) PackSave(paths Paths, rec sessionRecord, staged, dest string) (string, error) {
	if !sandbox.ValidSessionID(rec.id) {
		return "", fmt.Errorf("executor: the run names no Hermes session %q, so none is saved", rec.id)
	}
	return hermesOnDemand(paths, rec.id, staged, dest), nil
}

// hermesOnDemand is a pack the engine makes of session outside the run's own
// script: [packNothing] where the box holds nothing of it, [packFailed] where
// no archive was made. The carry script is the one the box's last run wrote.
func hermesOnDemand(paths Paths, session, staged, dest string) string {
	return "sid=" + shellword.Quote(session) + " packed=\n" + hermesCarryRun(paths) +
		hermesPack(paths, staged, dest, fmt.Sprintf("exit %d", packNothing)) +
		fmt.Sprintf("[ -n \"$packed\" ] || exit %d\n", packFailed)
}

// hermesFinish defines the script's `finish`, run once Hermes and its gateway
// have stopped, on the run's own end and a teardown's alike: the session
// packed over [Paths.SessionCapture] ([hermesPack]). Nothing to pack is said
// nowhere; a failed pack is said on the error stream, and the run's status is
// never the pack's.
func hermesFinish(paths Paths) string {
	return "finish() {\npacked=\n" + hermesPack(paths, paths.SessionStaging(), paths.SessionCapture(), ":") + "}\n"
}

// hermesPack is ONE PACK, the run's script's, a pack on demand and a save
// alike: the session titled sid dumped by the carry script ([hermesCarryRun])
// under [hermesArchiveMember] with the launch it was created under, tarred and
// gzipped in staged, its digest recorded ([packDigest]) and moved over dest;
// packed=1 where it was. nothing is the shell run where the store holds no
// session of that title.
func hermesPack(paths Paths, staged, dest, nothing string) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := shellword.Quote
	line(`pe=0`)
	line(`case "$sid" in ''|*[!A-Za-z0-9_-]*) pe=3;; esac`)
	line(`if [ "$pe" = 0 ] && ! [ -s %s ]; then pe=3; fi`, q(paths.HermesStateDB()))
	line(`if [ "$pe" = 0 ]; then`)
	line(`  if command -v tar > /dev/null 2>&1 && command -v gzip > /dev/null 2>&1; then`)
	line(`    rm -rf %[1]s; mkdir -p %[2]s || pe=1`, q(staged), q(staged+"/in"))
	line(`    [ "$pe" != 0 ] || case "$(hcarry dump %[1]s "$sid" %[2]s-"$sid" %[3]s/%[4]s)" in "crewlet-carry ok") ;; "crewlet-carry none") pe=3;; *) pe=1; cp %[5]s %[6]s 2> /dev/null;; esac`,
		q(paths.HermesStateDB()), q(paths.WorkDir()+"/hermes-policy"), q(staged+"/in"), hermesArchiveMember,
		q(paths.HermesCarryErrors()), q(staged+"/error"))
	line(`    if [ "$pe" = 0 ]; then if tar -czf %[1]s -C %[2]s %[3]s < /dev/null > /dev/null 2>> %[4]s && %[5]s && mv -f %[1]s %[6]s; then packed=1; else pe=1; fi; fi`,
		q(staged+"/capture"), q(staged+"/in"), hermesArchiveMember, q(staged+"/error"),
		packDigest(paths, "sid", staged), q(dest))
	line(`    [ "$pe" = 0 ] || [ "$pe" = 3 ] || echo "crewlet: the session $sid was not packed to be continued in another box: $(cat %s 2> /dev/null)" >&2`,
		q(staged+"/error"))
	line(`    rm -rf %s`, q(staged))
	line(`  else`)
	line(`    echo 'crewlet: this box has no tar and gzip, so the session was not packed to be continued in another box' >&2`)
	line(`  fi`)
	line(`fi`)
	line(`if [ "$pe" = 3 ]; then %s; fi`, nothing)
	return b.String()
}

// hermesSessionGate is the part of a run's script that names the session its
// Hermes runs in, as sid, whether it is the one asked to continue, as k, and
// why not, as w; and installs the run's overlay. The script creates sid where
// k is no (`--create-if-missing`) and continues it where k is yes: only on a
// box whose Hermes (n) is exactly [hermesFloor], in the session its box HOLDS
// ([Paths.HermesSessions]) unless a carried archive holds what the copy does
// not (a digest not among [Paths.SessionDigests]) or Hermes was cut off in it
// ([Session.Replaces]), when the archive is RESTORED ([hermesRestore]); only
// under the launch and model route Hermes froze with it ([hermesPolicy],
// `route`), and once it holds the run's own standing instructions (`renew`).
func hermesSessionGate(req sandbox.RunRequest, session Session, paths Paths, rewired bool) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := shellword.Quote
	// THE LAUNCH KEPT AS IT STARTS, not once it exits, so a session whose
	// first launch was cut off still continues under it.
	keep := fmt.Sprintf(`printf '%%s%%s\n' %s "$PWD" > %s-"$sid"`, q(hermesPolicy(req)), q(paths.WorkDir()+"/hermes-policy"))
	if !session.Continues {
		line(`sid=%s k=no w=`, q(session.ID))
		line(`%s`, keep)
		b.WriteString(hermesInstallOverlay(paths, paths.HermesOverlayStaged(), directHosts(req)))
		return b.String()
	}
	carried := q(paths.SessionCarried())
	line(`sid=%s k=yes w= h= d=`, q(session.ID))
	line(`if grep -q "^$sid " %s 2> /dev/null && [ -s %s ]; then h=1; fi`, q(paths.HermesSessions()), q(paths.HermesStateDB()))
	line(`if [ -s %s ]; then d=$(cksum < %s 2> /dev/null) || d=; fi`, carried, carried)
	if session.Replaces {
		line(`if [ -n "$h" ] && [ -s %s ]; then h=; fi`, carried)
	} else {
		line(`if [ -n "$h" ] && [ -n "$d" ] && ! grep -qxF "$sid $d" %s 2> /dev/null; then h=; fi`,
			q(paths.SessionDigests()))
	}
	line(`if [ -z "$h" ] && [ ! -s %s ]; then w='it is not in this box, and no archive of it was carried here'`, carried)
	line(`elif [ -z "$n" ]; then w=%s`, q(hermesNoRelease))
	line(`elif [ "$n" != %s ]; then w="%s"`, hermesFloor(), fmt.Sprintf(hermesBoxRefused, "Hermes Agent $n", hermesFloor()))
	line(`elif [ -z "$h" ]; then`)
	line(`  if ! command -v tar > /dev/null 2>&1 || ! command -v gzip > /dev/null 2>&1; then w='this box has no tar and gzip to restore its archive with'`)
	line(`  elif ! { %s; }; then w='its archive could not be restored'`, hermesRestore(paths))
	line(`  elif ! { %s; }; then w=%s`, recordDigest(paths, "sid"), q(restoreDigestUnrecorded))
	line(`  fi`)
	line(`  rm -rf %s`, q(paths.SessionStaging()))
	line(`fi`)
	line(`rm -f %s`, carried)
	// THE LAUNCH HERMES FROZE WITH THE SESSION: a resume naming another model,
	// effort or round cap, or from another working directory, is refused or
	// runs in the old one (measured on 0.23.4), so it starts a session of its own.
	line(`if [ -z "$w" ] && [ ! -f %s-"$sid" ]; then w='no launch of it was kept to continue it under'; fi`,
		q(paths.WorkDir()+"/hermes-policy"))
	line(`if [ -z "$w" ] && [ "$(cat %s-"$sid" 2> /dev/null)" != "$(printf '%%s%%s' %s "$PWD")" ]; then w='its launch (model, effort, round cap or working directory) is not the one Hermes froze with the session'; fi`,
		q(paths.WorkDir()+"/hermes-policy"), q(hermesPolicy(req)))
	// THE MODEL ROUTE HERMES FROZE, which a resume keeps whatever the box's
	// configuration says (measured on 0.23.4), so a continued session would
	// keep a route its entry no longer names.
	line(`if [ -z "$w" ]; then case "$(hcarry route %s "$sid" %s)" in 'crewlet-carry ok') ;; 'crewlet-carry moved') w='its model route (provider, base URL or model) is not the one Hermes froze with the session';; *) w='the model route Hermes froze with the session could not be read';; esac; fi`,
		q(paths.HermesStateDB()), q(req.Model))
	// THE RUN'S OWN STANDING INSTRUCTIONS, in place of the ones Hermes froze
	// with the session (measured on 0.23.4: the resume then carries them).
	line(`if [ -z "$w" ] && [ "$(hcarry renew %s "$sid" %s)" != 'crewlet-carry ok' ]; then w='its standing instructions could not be renewed to this run'"'"'s'; fi`,
		q(paths.HermesStateDB()), q(paths.System()))
	line(`if [ -z "$w" ]; then`)
	if rewired {
		line(`  rm -f %s`, q(paths.HermesOverlayFresh()))
	}
	b.WriteString(codexIndented(hermesInstallOverlay(paths, paths.HermesOverlayStaged(), directHosts(req))))
	line(`else`)
	line(`  sid=%s k=no`, q(session.Fresh))
	line(`  %s`, keep)
	line(`  printf '%%s%%s\n' %s "$w" >&2`, q("crewlet: this run was asked to continue the Hermes session "+
		session.ID+", and starts a new session instead: "))
	line(`  [ ! -s %s ] || exec < %s`, q(paths.FreshBrief()), q(paths.FreshBrief()))
	overlay := paths.HermesOverlayStaged()
	if rewired {
		line(`  rm -f %s`, q(paths.HermesOverlayStaged()))
		overlay = paths.HermesOverlayFresh()
		b.WriteString(codexIndented(hermesFreshRoutes(req, paths)))
	}
	b.WriteString(codexIndented(hermesInstallOverlay(paths, overlay, directHosts(req))))
	line(`fi`)
	return b.String()
}

// hermesFreshRoutes hands the plugin of a continued run that started a session
// of its own the notes and skill-load routes under the job's other token, as
// the gate hands Hermes that token's overlay.
func hermesFreshRoutes(req sandbox.RunRequest, paths Paths) string {
	q := shellword.Quote
	var b strings.Builder
	move := func(fresh, url string) {
		fmt.Fprintf(&b, "mv -f %s %s || { echo 'crewlet: the new session could not be wired to its seat, so the run does not start' >&2; exit 1; }\n",
			q(fresh), q(url))
	}
	if req.Steer.Set() {
		move(paths.HermesNotesURLFresh(), paths.HermesNotesURL())
	}
	if len(req.Skills) > 0 {
		move(paths.HermesSkillsURLFresh(), paths.HermesSkillsURL())
	}
	return b.String()
}

// hermesInstallOverlay writes the run's managed scope from overlay: the box's
// own (`/etc/hermes`, which the run's HERMES_MANAGED_DIR hides) with overlay's
// keys over it, through Hermes's own loader (the carry script's `overlay`),
// and its .env stating NO_PROXY and no_proxy with hosts ([directHosts]) after
// the process's, the home's and the box's entries, since Hermes loads that
// .env last. It ends the run before Hermes starts where it cannot: a run on no
// overlay reaches none of the seat's tools, and one on another session's is
// one the bridge cannot tell apart.
func hermesInstallOverlay(paths Paths, overlay string, hosts []string) string {
	q := shellword.Quote
	return fmt.Sprintf("if [ \"$(hcarry overlay %s %s %s)\" = 'crewlet-carry ok' ]; then rm -f %s; else echo 'crewlet: the Hermes overlay of the run could not be installed, so the run does not start' >&2; exit 1; fi\n",
		q(overlay), q(paths.HermesManaged()), q(strings.Join(hosts, ",")), q(overlay))
}

// hermesRestore is the restore of a carried archive of session sid: unpacked in
// staging, its one member required to be a plain file, the box's gateway
// stopped, and a NEW store built from it beside the old ([hermesCarryScript])
// and moved over it LAST, with the old store's write-ahead log removed, so a
// box that holds it holds it whole; the restored session indexed.
func hermesRestore(paths Paths) string {
	q := shellword.Quote
	in := paths.SessionStaging() + "/in"
	from := in + "/" + hermesArchiveMember
	next := paths.HermesStateDB() + ".restored"
	return strings.Join([]string{
		"rm -rf " + q(paths.SessionStaging()),
		"mkdir -p " + q(in) + " " + q(paths.HermesHome()),
		"tar -xzf " + q(paths.SessionCarried()) + " -C " + q(in) + " < /dev/null > /dev/null 2>&1",
		"[ -f " + q(from) + " ]",
		"[ ! -L " + q(from) + " ]",
		"endgw",
		"rs=$(hcarry restore " + q(from) + " " + q(next) + " " + q(paths.WorkDir()+"/hermes-policy") + "-\"$sid\" \"$sid\")",
		"case \"$rs\" in 'crewlet-carry ok '?*) rs=${rs#crewlet-carry ok };; *) false;; esac",
		"rm -f " + q(paths.HermesStateDB()+"-wal") + " " + q(paths.HermesStateDB()+"-shm"),
		"mv -f " + q(next) + " " + q(paths.HermesStateDB()),
		// The store is the archive's alone now: no other session's index line or launch stays.
		"printf '%s %s\\n' \"$sid\" \"$rs\" > " + q(paths.HermesSessions()+".next"),
		"for f in " + q(paths.WorkDir()+"/hermes-policy") + "-*; do [ \"$f\" = " + q(paths.WorkDir()+"/hermes-policy") +
			"-\"$sid\" ] || rm -f \"$f\"; done",
		"mv -f " + q(paths.HermesSessions()+".next") + " " + q(paths.HermesSessions()),
	}, " && ")
}

// hermesCarryRun defines the script's `hcarry`: the carry script run under
// Hermes's own interpreter, through its launcher's --run-module, since it
// reads Hermes's own modules (its configuration loader, managed scope and
// route) and a python3 on PATH need not be Hermes's: the docker image's is
// (measured on 0.23.4), install.sh's managed runtime puts none there (untested
// on 0.23.4, as it needs the network). The launcher runs modules, so the
// script goes through the standard library's cProfile, whose output is
// discarded; it keeps no exit status, so the answer is the last line printed.
func hermesCarryRun(paths Paths) string {
	return fmt.Sprintf("hcarry() { hermes --run-module cProfile -o /dev/null %s \"$@\" < /dev/null 2> %s | tail -n 1; }\n",
		shellword.Quote(paths.HermesCarry()), shellword.Quote(paths.HermesCarryErrors()))
}

// meter is the take's meter of a Hermes archive ([carrier.meter]): no figure.
// This build reads no count or estimate of a Hermes session's next request off
// its archive, as for Kimi Code ([Kimi.meter]); a Hermes run's window is read
// off the plugin's record at collection ([hermesEvents.reportedWindow]).
func (Hermes) meter(context.Context, io.Reader, string, string, *passBudget) (meterReading, error) {
	return meterReading{}, nil
}

// hermesChargeBytes is the fewest bytes a Hermes session's brief is counted at
// a token: one, the most tokens any estimate can make of n bytes, since
// Hermes's own count is not read ([kimiChargeBytes]).
const hermesChargeBytes sandbox.Charge = 1

// sessionTraits is how a relaunch judges a Hermes session, each value the one
// that never under-counts its next request: Hermes compacts at a share of its
// model's window this build does not read before the run (the run reads the
// window after, [hermesEvents.reportedWindow]).
func (Hermes) sessionTraits() sandbox.SessionTraits {
	return sandbox.SessionTraits{FirstRequest: sandbox.FirstRequestBox, CallKeying: sandbox.CallKeyingNone,
		Charge: hermesChargeBytes}
}
