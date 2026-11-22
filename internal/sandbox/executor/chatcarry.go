package executor

import (
	"archive/tar"
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/words"
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
	// carries is a shell condition, true where the box's release ($v, as
	// `--version` printed it) continues a session carried from another box;
	// refused is the shell word the run says where it does not, double-quoted
	// so it can name "$v".
	carries, refused string
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
// record would be appended onto a partial one. Neither held nor restored, the
// run starts its minted session, on its other wiring and fresh brief, saying why.
func chatSessionGate(session Session, paths Paths, g chatGate) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := sandbox.ShellQuote
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
	line(`if [ -z "$h" ]; then`)
	line(`  if [ ! -s %s ]; then w='it is not in this box, and no archive of it was carried here'`, carried)
	line(`  elif ! { %s; }; then w=%s`, g.carries, g.refused)
	line(`  elif ! command -v tar > /dev/null 2>&1 || ! command -v gzip > /dev/null 2>&1; then w='this box has no tar and gzip to restore its archive with'`)
	line(`  elif ! { in=%s && rm -rf %s && mkdir -p "$in" && tar -xzf %s -C "$in" < /dev/null > /dev/null 2>&1 && %s; }; then w='its archive could not be restored'`,
		q(staged+"/in"), q(staged), carried, g.restore)
	// A RESTORE IS CONTINUED ONLY ONCE ITS DIGEST IS RECORDED, and read back
	// ([recordDigest]), for the reason Claude Code's gate gives.
	line(`  elif ! { %s; }; then w=%s`, recordDigest(paths, "s", "d"), q(restoreDigestUnrecorded))
	line(`  fi`)
	line(`  rm -rf %s`, q(staged))
	line(`fi`)
	line(`rm -f %s`, carried)
	line(`if [ -z "$w" ]; then`)
	line(`  %s`, g.locate)
	line(`  %s`, tornCut)
	line(`  [ -z "$cf" ] || torn "$cf"`)
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

// chatPack is the pack of the session "$s" names whose chat file is cm,
// relative to cfg (the shell has set cm, "" for none): one gzipped tar of
// that file alone, made in at.staged, its digest recorded ([packDigest]) and
// moved over at.dest whole, packed set where it was. A pack beside a harness
// that may still be writing is held to [claudeHeldTar]'s rule; one made once
// the harness is gone takes GNU tar's "file changed as we read it" as whole,
// the one writer left having outlived the harness. Nothing to pack is said
// nowhere; a failed pack is said on the error stream, never the run's status.
func chatPack(paths Paths, at packAt, cfg string) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := sandbox.ShellQuote
	line(`if [ -n "$cm" ]; then`)
	line(`  if command -v tar > /dev/null 2>&1 && command -v gzip > /dev/null 2>&1; then`)
	line(`    set -- "$cm"`)
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

// chatOnDemand is a pack the engine makes of session outside the run's own
// script, at at, its chat file found by locate (which sets cm): [packNothing]
// where the box holds no chat file of it, [packFailed] where no archive was made.
func chatOnDemand(paths Paths, session string, at packAt, cfg, locate string) string {
	return "s=" + sandbox.ShellQuote(session) + " packed=\n" + locate + "\n" +
		fmt.Sprintf("[ -n \"$cm\" ] || exit %d\n", packNothing) +
		chatPack(paths, at, cfg) +
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

// measuredCarry reports whether harness can continue, at release box, a
// session release writer wrote in another box, each as `--version` printed
// it, and if not, why in a person's words: both releases among the minors
// measured carrying each other's sessions both ways ("0.70" holds every
// 0.70.x). A release of a minor measured keeps that minor's session layout,
// which only a minor moves; any other is refused, never resumed on a layout
// nobody read. An unreadable release is taken for a current one, as the notes
// gate takes it.
func measuredCarry(harness, box, writer string, measured []string) (bool, string) {
	for _, r := range []struct{ release, who string }{{box, "this box runs"}, {writer, "the session was written by"}} {
		numbers, readable := releaseNumbers(r.release)
		if !readable {
			continue
		}
		if minor := fmt.Sprintf("%d.%d", numbers[0], numbers[1]); !slices.Contains(measured, minor) {
			return false, fmt.Sprintf("%s %s %s, and a carried session was measured continuing only "+
				"between %s %s", r.who, harness, r.release, harness, measuredMinors(measured))
		}
	}
	return true, ""
}

// measuredMinors is measured as a person reads it: "0.65.x and 0.70.x".
func measuredMinors(measured []string) string {
	out := make([]words.Said, len(measured))
	for i, m := range measured {
		out[i] = words.Said(m + ".x")
	}
	return words.And(out)
}

// measuredCase is the shell condition [measuredCarry] holds the box's release
// to in its script, on "$v": true for an unreadable release and for one of the
// minors measured.
func measuredCase(measured []string) string {
	patterns := make([]string, len(measured))
	for i, m := range measured {
		patterns[i] = m + ".*"
	}
	return `case "$v" in [0-9]*.[0-9]*.[0-9]*) case "$v" in ` + strings.Join(patterns, "|") +
		`) true;; *) false;; esac;; *) true;; esac`
}
