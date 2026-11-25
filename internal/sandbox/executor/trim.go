package executor

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// TRIMMED BEFORE THE BOUND (ADR-0073, "A kept session is what its harness's
// resume loads"): a pack whose session the take would refuse whole keeps only
// what its harness's resume loads: everything from its newest compaction on,
// and before it what that release reads there (Codex its first line, Copilot
// all but the turns the summary replaced, Muse Code nothing, Pi the turns the
// compaction kept and the settings and system messages it reads, re-linked).
// It cuts the pack's copy, never the live record, so it runs on every pack, a
// save's and a stop's as the end's, and costs no model call. Only the release a
// rule was measured on is trimmed ([trimmer]); every other harness and release
// packs whole, and a pack still past the bound once trimmed is refused as one
// never trimmed is.

// NO TRIM, BY HARNESS: Claude Code's own trim rewrites its transcript, and its
// subagent threads stay whole, since 2.1.310 continues one spawned before the
// newest compaction after a resume (measured); Kimi Code's replay folds every
// record before its compaction into the state it resumes; Cursor Agent carries
// no chat a compaction summarized. None is a [trimmer], so each packs whole.

// trimTarSlack is what the plaintext point leaves the tar's own bytes beside
// its members': a 512-byte header and up to 511 of padding a member, its long
// name's header pair, and the 1 KiB end. ONE MEBIBYTE holds every layout's
// members many times over, so the point is the members' bytes alone.
const trimTarSlack = 1 << 20

// trimPlainPoint is the members' bytes past which a pack trims: what the take's
// scrub reads at most ([scrubPlainMax]) less [trimTarSlack]. trimPackedPoint is
// the archive's bytes past which it trims: what a take keeps
// ([executorkeep.SessionMaxBytes]). AT THE BOUNDS THEMSELVES, never below them:
// every pack trims, so no growth is left between a trim and a take, and a trim
// at the bound keeps the most history Sessions and read_session can read.
const (
	trimPlainPoint  = scrubPlainMax - trimTarSlack
	trimPackedPoint = executorkeep.SessionMaxBytes
)

// trimPoints are the two points a pack trims past ([trimAt] in production; a
// test's own, small enough to reach).
type trimPoints struct{ plain, packed int64 }

// trimAt is every pack's: the bounds a take holds an archive to.
var trimAt = trimPoints{plain: trimPlainPoint, packed: trimPackedPoint}

// trimmer is a [carrier] whose pack trims its session's record past the bound
// ([trimStaged] and [trimPacked] where the pack stages its record, [trimTar]
// where it tars it in place): to what a harness's resume loads of it, as
// measured on one release.
type trimmer interface {
	// trimRelease is the release the rule was measured on: a session any other
	// release wrote is packed whole.
	trimRelease() string

	// trimRecord is the shell that writes into dst what of the record at src
	// the release's resume loads, true where it wrote a copy that leaves lines
	// out; false where src holds no compaction it reads past, or nothing
	// before it to leave out. Both are shell words.
	trimRecord(src, dst string) string
}

// trimRelVar is the shell variable a pack reads the release that wrote its
// session from: the run's script sets it from its own `--version`, and
// [Runner.packNow] from the record, for a pack on demand and a save.
const trimRelVar = "trel"

// trimCut is the shell that cuts the staged record at the shell word member to
// what t's resume loads, where it can, and says so in trw: `<bytes before>
// <bytes kept>`. Its status is never the pack's: a record it cannot cut is
// packed whole, for the take to refuse.
func trimCut(t trimmer, member string) string {
	cut := member + ".trim"
	return fmt.Sprintf(`trz=$(wc -c < %[1]s | tr -d ' ') && { %[3]s; } && trk=$(wc -c < %[2]s | tr -d ' ') && mv -f %[2]s %[1]s && trw="$trz $trk"; rm -f %[2]s`,
		member, cut, t.trimRecord(member, cut))
}

// trimStaged is the shell a pack runs once it has staged its record at the
// shell word member and before it tars it: where the release in [trimRelVar]
// is t's and the member is past at.plain bytes, the member is cut
// ([trimCut]). It sets trw, "" where nothing was cut, and is always true.
func trimStaged(t trimmer, at trimPoints, member string) string {
	return fmt.Sprintf(`{ trw=; if [ "$%s" = %s ] && [ "$(wc -c < %s | tr -d ' ')" -gt %d ] 2> /dev/null; then %s; fi; }`,
		trimRelVar, shellword.Quote(t.trimRelease()), member, at.plain, trimCut(t, member))
}

// trimPacked is the shell a pack runs once it has made its archive at the
// shell word archive: where the record is not cut yet, the release is t's and
// the archive is past at.packed bytes, the member is cut and repack run again.
// True unless that repack fails.
func trimPacked(t trimmer, at trimPoints, member, archive, repack string) string {
	return fmt.Sprintf(`{ [ -n "$trw" ] || [ "$%s" != %s ] || [ "$(wc -c < %s | tr -d ' ')" -le %d ] 2> /dev/null || { %s; [ -z "$trw" ] || %s; }; }`,
		trimRelVar, shellword.Quote(t.trimRelease()), archive, at.packed, trimCut(t, member), repack)
}

// trimTar is the tar of a pack that tars its members in place under root
// (a shell word) into staged/capture, its positional parameters the members,
// the first its record, rel: tar, the pack's own, unless the release in
// [trimRelVar] is t's and the record is past at.plain bytes. Then, and where
// tar's archive is past at.packed, the record's whole lines are copied under
// staged/in, cut ([trimCut]) and tarred under the same name beside the other
// members. ONLY PAST A POINT, so every other pack is the one it was; the copy is
// whole lines of a record its harness only appends to. A copy or a tar that
// fails is packed as tar packs it, untrimmed. True where an archive was made.
func trimTar(t trimmer, at trimPoints, root, rel, staged, tar string) string {
	q := shellword.Quote
	record, member, capture := root+"/"+rel, q(staged+"/in")+"/"+rel, q(staged+"/capture")
	stage := fmt.Sprintf(`trn=$(wc -l < %[1]s | tr -d ' ') && [ "$trn" -gt 0 ] && mkdir -p "$(dirname %[2]s)" && head -n "$trn" %[1]s > %[2]s`,
		record, member)
	retar := fmt.Sprintf(`( shift; tar -czf %[1]s -C %[2]s %[3]s ${1+-C} ${1+%[4]s} "$@" < /dev/null > /dev/null 2>> %[5]s ) || { trw=; %[6]s; }`,
		capture, q(staged+"/in"), rel, root, q(staged+"/error"), tar)
	ours := fmt.Sprintf(`[ "$%s" = %s ]`, trimRelVar, q(t.trimRelease()))
	return fmt.Sprintf(`{ trw=; if %[1]s && [ "$(wc -c < %[2]s | tr -d ' ')" -gt %[3]d ] 2> /dev/null && { %[4]s; } 2> /dev/null; then %[5]s; %[6]s; else %[7]s; fi; } && { [ -n "$trw" ] || ! %[1]s || [ "$(wc -c < %[8]s | tr -d ' ')" -le %[9]d ] 2> /dev/null || { { %[4]s; } 2> /dev/null && %[5]s; [ -z "$trw" ] || { %[6]s; }; }; }`,
		ours, record, at.plain, stage, trimCut(t, member), retar, tar, capture, at.packed)
}

// trimMarkOf is where a pack leaves its word that the archive at archive was
// trimmed ([trimMarked]): beside it, so whatever removes an archive's files by
// its name or its pattern removes it too.
func trimMarkOf(archive string) string { return archive + ".trimmed" }

// trimUnmark is the shell a pack runs right before it moves its archive over
// dest: any word an earlier pack left there goes first, so it never describes
// this one. Always true, since [Runner.trimOf] holds a word to the archive it
// names by its size.
func trimUnmark(dest string) string {
	return "{ rm -f " + shellword.Quote(trimMarkOf(dest)) + " 2> /dev/null || :; }"
}

// trimMarked is the shell a pack runs once its archive is at dest: where it
// was trimmed (trw), the word `<archive bytes> <record bytes> <kept bytes>`
// beside it. Best effort: an archive with no word is kept as one that was not
// trimmed, which loses only the saying so.
func trimMarked(dest string) string {
	q := shellword.Quote
	return fmt.Sprintf(`{ [ -z "$trw" ] || printf '%%s %%s\n' "$(wc -c < %s | tr -d ' ')" "$trw" > %s 2> /dev/null || :; }`,
		q(dest), q(trimMarkOf(dest)))
}

// trimMarkMaxBytes is the longest word [Runner.trimOf] reads: three decimals
// of an int64 and their separators. A longer file is no word of a pack's.
const trimMarkMaxBytes = 3*19 + 3

// trimOf is what the box says its pack trimmed of archive ([trimMarked]):
// read only for a [trimmer] on its release, and held to the archive it names
// by its size, so a word an earlier pack left beside another archive says
// nothing. The zero value where it says nothing this build reads, or the box
// could not be asked: the word says what was kept, never whether it is.
func (r *Runner) trimOf(ctx context.Context, box sandbox.Sandbox, archive sandbox.SessionArchive) sandbox.ArchiveTrim {
	t, ok := r.harness.(trimmer)
	if !ok || archive.Version != t.trimRelease() {
		return sandbox.ArchiveTrim{}
	}
	mark, err := box.ReadTail(ctx, trimMarkOf(archive.Path), trimMarkMaxBytes)
	if err != nil {
		log.WarnContext(ctx, "executor_session_trim_unread", "harness", r.harness.Name(),
			"session", archive.SessionID, "error", err.Error(), "detail", "the box's word on whether its "+
				"pack trimmed the session could not be read, so the capture is kept saying it was not")
		return sandbox.ArchiveTrim{}
	}
	if mark.Size > trimMarkMaxBytes {
		return sandbox.ArchiveTrim{}
	}
	return parseTrimMark(string(mark.Data), archive.Size)
}

// parseTrimMark reads a pack's word on its trim ([trimMarked]): exactly three
// decimals and a line break, the first the size of the archive it describes,
// which must be size, the record's bytes before the cut above its bytes kept,
// and those above zero. Anything else is no trim.
func parseTrimMark(mark string, size int64) sandbox.ArchiveTrim {
	line, ok := strings.CutSuffix(mark, "\n")
	fields := strings.Split(line, " ")
	if !ok || len(fields) != 3 {
		return sandbox.ArchiveTrim{}
	}
	var n [3]int64
	for i, f := range fields {
		v, err := strconv.ParseInt(f, 10, 64)
		if err != nil || v <= 0 || strconv.FormatInt(v, 10) != f {
			return sandbox.ArchiveTrim{}
		}
		n[i] = v
	}
	if n[0] != size || n[1] <= n[2] {
		return sandbox.ArchiveTrim{}
	}
	return sandbox.ArchiveTrim{RecordBytes: n[1], KeptBytes: n[2]}
}

// logTrim says what a take's archive was trimmed of, where it was
// (executor_session_trimmed): the record's bytes before the cut and kept, as
// the box counted them.
func logTrim(ctx context.Context, harness string, archive sandbox.SessionArchive) {
	if !archive.Trim.Trimmed() {
		return
	}
	log.InfoContext(ctx, "executor_session_trimmed", "harness", harness, "session", archive.SessionID,
		"release", archive.Version, "archive_bytes", archive.Size, "record_bytes", archive.Trim.RecordBytes,
		"kept_bytes", archive.Trim.KeptBytes)
}
