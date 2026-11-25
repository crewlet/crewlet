package executor

import (
	"archive/tar"
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/crewlet/crewlet/internal/shellword"
)

// A SESSION THAT IS ONE CHAT FILE, carried between boxes: Gemini CLI's and Qwen
// Code's each keep a session as one append-only JSON-lines file
// ([GeminiCLI], [QwenCode]), so one gate restores either, one pack packs
// either, and one walk reads either's archive: a gzipped tar holding that file
// alone, named relative to a directory of the harness's own. The package
// doc's "A run's harness session, carried between boxes" has the protocol
// (digests, a cut-off copy replaced, a session of the run's own where none is
// held or restored); what each harness packs and how it restores is at its
// own carry file (geminicarry.go, qwencarry.go).

// chatGate is what one harness tells [chatSessionGate].
type chatGate struct {
	// harness is the harness's name in a person's words: "Gemini CLI".
	harness string
	// locate is shell that sets cf to the box's copy of the session the shell
	// variable s names, a non-empty plain file the harness resumes from this
	// run's working directory, or to "" where it holds none.
	locate string
	// restore is an && chain moving the session out of the carried archive,
	// unpacked under "$in", into the place locate finds it, the box's own copy
	// replaced: true only once the session is in place.
	restore string
	// cut is shell cutting the continued chat file "$cf" ("" where none is
	// continued) back to what the harness may load, run once it is whole and
	// before anything counts its lines: "" for no cut.
	cut string
	// release is the one release of the harness this build continues a
	// carried session on, which the box's ($v, as `--version` printed it)
	// must be exactly ([exactCarry]).
	release string
	// fresh are the files a run writes twice where it may start a session of
	// its own ([sandbox.RunRequest.FreshWiring]): each copy, under the job's
	// other bridge token, beside the file it replaces.
	fresh []freshCopy
}

// freshCopy is one file of a run's other wiring: the copy, and the file the
// harness reads, which the copy is moved over where the run starts a session
// of its own and which stays where it continues ([chatSessionGate]).
type freshCopy struct{ copy, file string }

// chatSessionGate is the script part naming the harness's session (s), whether
// it is the one asked for (k), why not (w), and the chat file continued (cf):
// [claudeSessionGate]'s protocol for a one-file session. The box's copy is
// continued unless the carried archive holds what it does not (a digest not
// among the box's) or the copy is one its harness was cut off in
// ([Session.Replaces]); that archive, or one carried into a box with none, is
// restored on a release that continues it, its digest recorded and read back
// first. A file torn mid-line is cut back to its last whole line: the next
// record would be appended onto a partial one; then the harness's cut runs
// ([chatGate].cut). Neither held nor restored, the run starts its minted
// session, on its other wiring and fresh brief, saying why.
func chatSessionGate(session Session, paths Paths, g chatGate) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := shellword.Quote
	if !session.Continues {
		line(`s=%s k=no w= cf=`, q(session.ID))
		return b.String()
	}
	staged := paths.SessionStaging()
	carried := q(paths.SessionCarried())
	digests := q(paths.SessionDigests())
	line(`s=%s k=yes w= h= d= cf=`, q(session.ID))
	line(`%s`, g.locate)
	line(`[ -z "$cf" ] || h=1`)
	line(`if [ -s %s ]; then d=$(cksum < %s 2> /dev/null) || d=; fi`, carried, carried)
	if session.Replaces {
		// A COPY ITS HARNESS WAS CUT OFF IN is replaced by the archive whatever
		// it holds: the archive is the session as far as anything after it is told.
		line(`if [ -n "$h" ] && [ -s %s ]; then h=; fi`, carried)
	} else {
		line(`if [ -n "$h" ] && [ -n "$d" ] && ! grep -qxF "$s $d" %s 2> /dev/null; then h=; fi`, digests)
	}
	// ONLY THE ONE RELEASE CONTINUES, its own copy as much as a carried one,
	// read bare as the engine reads it ([bareReleaseShell], [exactInPlace]): a
	// newer release runs on sessions of its own.
	line(`%sif [ "$rv" != %s ]; then w=%s"${rv:-${v:-a release it could not read}}"`, bareReleaseShell("rv"), q(g.release),
		q("this build continues "+g.harness+" sessions only on "+g.release+", and this box runs "))
	line(`elif [ -z "$h" ]; then`)
	line(`  if [ ! -s %s ]; then w='it is not in this box, and no archive of it was carried here'`, carried)
	line(`  elif ! command -v tar > /dev/null 2>&1 || ! command -v gzip > /dev/null 2>&1; then w='this box has no tar and gzip to restore its archive with'`)
	line(`  elif ! { in=%s && rm -rf %s && mkdir -p "$in" && tar -xzf %s -C "$in" < /dev/null > /dev/null 2>&1 && %s; }; then w='its archive could not be restored'`,
		q(staged+"/in"), q(staged), carried, g.restore)
	// A RESTORE IS CONTINUED ONLY ONCE ITS DIGEST IS RECORDED, and read back
	// ([recordDigest]), for the reason Claude Code's gate gives.
	line(`  elif ! { %s; }; then w=%s`, recordDigest(paths, "s"), q(restoreDigestUnrecorded))
	line(`  fi`)
	line(`  rm -rf %s`, q(staged))
	line(`fi`)
	line(`rm -f %s`, carried)
	line(`if [ -z "$w" ]; then`)
	line(`  %s`, g.locate)
	line(`  %s`, tornCut)
	line(`  [ -z "$cf" ] || torn "$cf"`)
	if g.cut != "" {
		line(`  %s`, g.cut)
	}
	for _, f := range g.fresh {
		line(`  rm -f %s`, q(f.copy))
	}
	line(`else`)
	line(`  s=%s k=no cf=`, q(session.Fresh))
	line(`  printf '%%s%%s\n' %s "$w" >&2`, q("crewlet: this run was asked to continue the "+g.harness+" session "+
		session.ID+", and starts a new session, "+session.Fresh+", instead: "))
	// A NEW SESSION IS HANDED THE BRIEF WRITTEN FOR ONE, where the run was
	// given it ([sandbox.RunRequest.FreshBrief]).
	line(`  [ ! -s %s ] || exec < %s`, q(paths.FreshBrief()), q(paths.FreshBrief()))
	// AND IS WIRED TO THE SEAT'S TOOLS UNDER THE OTHER TOKEN, whose dial is the
	// box's word to the bridge on which session it started; a copy that cannot
	// be moved ends the run before its harness starts, never one on the
	// continued session's token.
	for _, f := range g.fresh {
		line(`  mv -f %s %s || { echo 'crewlet: the new session could not be wired to the tools of its seat, so the run does not start' >&2; exit 1; }`,
			q(f.copy), q(f.file))
	}
	line(`fi`)
	return b.String()
}

// tornCut defines torn, which cuts the file it is handed back to its last
// whole line where its last byte is no newline, as wc counts it (a command
// substitution drops a NUL, which GNU tar pads a shrunk member with), and
// leaves a file with no whole line as it is ([claudeSessionGate]).
const tornCut = `torn() { if [ -s "$1" ] && [ "$(tail -c 1 "$1" | wc -l)" -eq 0 ]; then cl=$(wc -l < "$1"); ` +
	`if [ "$cl" -gt 0 ] 2> /dev/null; then head -n $cl "$1" > "$1.cut" && mv -f "$1.cut" "$1"; fi; fi; }`

// chatCapture is the part of a run's script that packs its session once its
// harness has exited ([chatPack]), trimmed by the box's bare release
// ([trimRelVar], read off v as [releaseOf] reads it) and its run's start
// ([chatRunStartFrom]).
func chatCapture(paths Paths, cfg string, t recordTrimmer) string {
	return bareReleaseShell(trimRelVar) + chatPack(paths, packAt{staged: paths.SessionStaging(),
		dest: paths.SessionCapture()}, cfg, t)
}

// chatPack is ONE PACK of t's chat file, the run's end's, a pack on demand's
// and a save's, at the bounds a take keeps ([trimAt]): [chatPackAt].
func chatPack(paths Paths, at packAt, cfg string, t recordTrimmer) string {
	return chatPackAt(paths, at, cfg, t, trimAt)
}

// chatPackAt is the pack of the session "$s" names whose chat file is cm,
// relative to cfg (the shell has set cm, "" for none): one gzipped tar of
// that file alone, made in at.staged, its digest recorded ([packDigest]) and
// moved over at.dest whole, packed set where it was, and its trim past trim
// said beside it ([trimTar], [trimMarked]). A pack beside a harness that may
// still be writing is held to [claudeHeldTar]'s rule; one made once the
// harness is gone takes GNU tar's "file changed as we read it" as whole, the
// one writer left having outlived the harness. Nothing to pack is said
// nowhere; a failed pack is said on the error stream, never the run's status.
func chatPackAt(paths Paths, at packAt, cfg string, t recordTrimmer, trim trimPoints) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := shellword.Quote
	line(`if [ -n "$cm" ]; then`)
	line(`  if command -v tar > /dev/null 2>&1 && command -v gzip > /dev/null 2>&1; then`)
	line(`    set -- "$cm"`)
	tar := fmt.Sprintf(`{ tar -czf %s -C %s "$@" < /dev/null > /dev/null 2> %s; t=$?; [ "$t" = 0 ] || { [ "$t" = 1 ] && grep -q 'changed as we read it' %s; }; }`,
		q(at.staged+"/capture"), q(cfg), q(at.staged+"/error"), q(at.staged+"/error"))
	if at.working {
		tar = claudeHeldTar(cfg, at.staged)
	}
	tar = trimTar(t, trim, q(cfg), `"$cm"`, at.staged, tar)
	line(`    rm -rf %s; if mkdir -p %s && %s && %s && %s && mv -f %s %s; then packed=1; %s; else echo "crewlet: the session $s was not packed to be continued in another box: $(cat %s 2> /dev/null)" >&2; fi`,
		q(at.staged), q(at.staged), tar, packDigest(paths, "s", at.staged), trimUnmark(at.dest),
		q(at.staged+"/capture"), q(at.dest), trimMarked(at.dest), q(at.staged+"/error"))
	line(`    rm -rf %s`, q(at.staged))
	line(`  else`)
	line(`    echo 'crewlet: this box has no tar and gzip, so the session was not packed to be continued in another box' >&2`)
	line(`  fi`)
	line(`fi`)
	return b.String()
}

// chatOnDemand is a pack the engine makes of session outside the run's own
// script, at at, its chat file found by locate (which sets cm) and trimmed as
// t's ([chatPack]): [packNothing] where the box holds no chat file of it,
// [packFailed] where no archive was made.
func chatOnDemand(paths Paths, session string, at packAt, cfg, locate string, t recordTrimmer) string {
	return "s=" + shellword.Quote(session) + " packed=\n" + locate + "\n" +
		fmt.Sprintf("[ -n \"$cm\" ] || exit %d\n", packNothing) +
		chatPack(paths, at, cfg, t) +
		fmt.Sprintf("[ -n \"$packed\" ] || exit %d\n", packFailed)
}

// chatArchive reads an archive of one chat file: its first member member
// names, a plain file, read by read, every other member passed over. A member
// that names the file and is no plain file (a link, a directory) is not read
// through, and is said where it was.
func (b *transcriptBuilder) chatArchive(ctx context.Context, archive io.Reader, member func(string) bool,
	read func(context.Context, io.Reader) error) error {

	tr := tar.NewReader(archive)
	for {
		h, err := tr.Next()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return fmt.Errorf("%w: %w", errNotTar, err)
		}
		if !member(h.Name) {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			if unreadErr := b.unread("the session's chat file is not a plain file in the archive, " +
				"and was not read"); unreadErr != nil {
				return unreadErr
			}
			continue
		}
		return read(ctx, bufio.NewReaderSize(tr, lineChunkBytes))
	}
}

// exactCarry reports whether harness continues, in a box of release box, a
// session release writer wrote in another box, and if not, why in a person's
// words: only where both are supported, the one release this build drives the
// harness at, whose session layout and resume were read ([carrier.Continues]).
// A newer box runs on sessions of its own, an older one not at all
// ([releaseGate]); a box or a writer that named no release this build reads
// continues nothing.
func exactCarry(harness, supported, box, writer string) (bool, string) {
	if box == "" {
		return false, boxNamedNoRelease(harness)
	}
	if ok, why := exactInPlace(harness, supported, box); !ok {
		return false, why
	}
	return exactWriter(harness, supported, writer)
}

// exactWriter is [exactCarry] judged of the writer alone
// ([carrier.ContinuesWriter]).
func exactWriter(harness, supported, writer string) (bool, string) {
	switch writer {
	case supported:
		return true, ""
	case "":
		return false, writerNamedNoRelease(harness)
	}
	return false, fmt.Sprintf("this build continues %s sessions only on %s, and the session was written by %s",
		harness, supported, writer)
}

// exactInPlace is [exactCarry] judged of the box alone: whether a box of
// release box continues any session, the copy it holds as much as a carried
// one ([inPlaceGate]), as the run's session gate holds it ([chatSessionGate]).
func exactInPlace(harness, supported, box string) (bool, string) {
	switch box {
	case supported:
		return true, ""
	case "":
		return false, fmt.Sprintf("this box's %s named no release this build reads, so it continues no "+
			"session, its own copy nor one carried from another box", harness)
	}
	return false, fmt.Sprintf("this build continues %s sessions only on %s, and this box runs %s",
		harness, supported, box)
}

// releaseRead is the shell reading into v what command prints, whole, as one
// line: line ends and tabs as spaces, its ends trimmed, as the engine reads
// the same output ([releaseOf]), so a release printed on a CRLF line reads as
// itself and the gate, the session's gate and the engine judge one word.
func releaseRead(command string) string {
	return `v=$(` + command + ` < /dev/null 2> /dev/null | tr '\r\n\t' '   '); v=${v#"${v%%[! ]*}"}; v=${v%"${v##*[! ]}"}`
}

// releaseGate is the shell a run's script starts with, once it has read the
// box's release as v: a release older than supported ends the run before
// anything else happens, saying so on the error stream, because this build
// read none of an older release's output. It judges the word the engine reads
// ([bareReleaseShell]: the first that starts with a digit, a leading v and one
// closing period off); one whose digits and dots leave no MAJOR.MINOR.PATCH
// is refused too, as it cannot be shown not to be older. What follows the
// numbers is a pre-release's suffix, which comes before its release: so a
// suffixed supported release is older and refused, and a suffixed later one
// is newer and runs, on sessions of its own like every newer one ([exactCarry]).
func releaseGate(harness, supported string) string {
	n, _ := releaseNumbers(supported)
	say := shellword.Quote("crewlet: this box runs "+harness+" ") + `"${v:-that prints no release}"` +
		shellword.Quote(", and this build runs "+harness+" "+supported+" or newer (a release it cannot "+
			"read counts as older): install "+harness+" "+supported)
	return `tk=; set -f; for rw in $v; do rw=${rw#v}; rw=${rw%.}; case "$rw" in [0-9]*) tk=$rw; break;; esac; done; set +f` + "\n" +
		`r=${tk%%[!0-9.]*} ro=1; rM=${r%%.*} rr=${r#*.}; rn=${rr%%.*} rp=${rr#*.}` + "\n" +
		`case "$r" in *.*.*) ;; *) ro= ;; esac` + "\n" +
		`for x in "$rM" "$rn" "$rp"; do case "$x" in ''|*[!0-9]*) ro= ;; esac; done` + "\n" +
		fmt.Sprintf(`if [ -z "$ro" ] || [ "$rM" -lt %d ] || { [ "$rM" -eq %d ] && { [ "$rn" -lt %d ] || `+
			`{ [ "$rn" -eq %d ] && { [ "$rp" -lt %d ] || { [ "$rp" -eq %d ] && [ "$tk" != "$r" ]; }; }; }; }; }; `+
			`then printf '%%s\n' %s >&2; exit 1; fi`,
			n[0], n[0], n[1], n[1], n[2], n[2], say)
}
