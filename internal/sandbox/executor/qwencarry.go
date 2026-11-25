package executor

import (
	"context"
	"io"
	"path"
	"strings"

	"github.com/crewlet/crewlet/internal/shellword"
)

// A QWEN CODE SESSION CARRIED BETWEEN BOXES is its one chat file
// (chatcarry.go): `~/.qwen/projects/<key>/chats/<id>.jsonl`, the key the
// working directory's realpath with every character but a letter or a digit a
// dash, which nothing pins. A resume checks only the first line's `cwd`, so
// the restore ([qwenHelperScript]'s `restore`) rewrites that field and files
// the session under the box's key; the run loads it over ACP (`session/load`),
// history whole (0.28.4 elsewhere, 1.4 to 1.7 s for 5 MB; the one release it
// continues on: [qwenRelease]). Writes are asynchronous, so a pack beside the harness
// can end mid-line, which the restore cuts back ([chatSessionGate]).

// NO RECORD OF TAKES TRAVELS: a note is confirmed by the chat file's own
// `mid_turn_user_message` record, which is part of the session. A note the
// session recorded with no reply after it is cut before the load
// ([qwenUnansweredCut]), since the run serves it again.

// qwenArchiveFormat is the layout a Qwen Code run packs its session in: one
// gzipped tar of the chat file, named `projects/<key>/chats/<id>.jsonl`
// relative to [Paths.QwenDir], the key the packing box's own. A change to what
// is packed, or where it is restored to, is a new format, which a box of this
// build refuses.
const qwenArchiveFormat = 1

var (
	_ resumer = QwenCode{}
	_ carrier = QwenCode{}
	_ saver   = QwenCode{}
)

// Resumes says Qwen Code continues a session it reported (`session/load`).
func (QwenCode) Resumes() bool { return true }

// ArchiveFormat is the layout a Qwen Code run packs its session in ([carrier]).
func (QwenCode) ArchiveFormat() int { return qwenArchiveFormat }

// VersionCommand prints a box's Qwen Code release ([carrier]).
func (QwenCode) VersionCommand() string { return "qwen --version" }

// Continues reports whether a session Qwen Code release writer wrote in another
// box can be continued by release box: both [qwenRelease] ([carrier],
// [exactCarry]).
func (QwenCode) Continues(box, writer string) (bool, string) {
	return exactCarry("Qwen Code", qwenRelease, box, writer)
}

// ContinuesWriter is [QwenCode.Continues] judged of the writer alone
// ([carrier]): only a session [qwenRelease] wrote.
func (QwenCode) ContinuesWriter(writer string) (bool, string) {
	return exactWriter("Qwen Code", qwenRelease, writer)
}

// continuesInPlace reports whether a box whose Qwen Code is release box
// continues a session at all, the copy it holds as much as one carried to it
// ([inPlaceGate]): only on [qwenRelease].
func (QwenCode) continuesInPlace(box string) (bool, string) {
	return exactInPlace("Qwen Code", qwenRelease, box)
}

// Pack packs session now, as a run's script packs it once the harness exits
// ([carrier]), held to the rule of a pack beside a working harness where
// working says so.
func (QwenCode) Pack(paths Paths, session, staged, dest string, working bool) string {
	return chatOnDemand(paths, session, packAt{staged: staged, dest: dest, working: working},
		paths.QwenDir(), qwenLocate(paths), QwenCode{})
}

// PackSave packs a save of rec's session beside a harness that may still be
// working ([saver]). The record refuses no Qwen Code save, so it never errs.
func (QwenCode) PackSave(paths Paths, rec sessionRecord, staged, dest string) (string, error) {
	return chatOnDemand(paths, rec.id, packAt{staged: staged, dest: dest, working: true},
		paths.QwenDir(), qwenLocate(paths), QwenCode{}), nil
}

// startsByLines says a Qwen Code run records where it began in its chat file as
// a line count and their digest ([lineStarter]).
func (QwenCode) startsByLines() {}

// layout is a Qwen Code archive's: a gzipped tar of the chat file.
func (QwenCode) layout() archiveLayout { return archiveTar }

// charges reports whether member is a chat file, the one the meter counts
// ([carrier.charges], [chatMemberLines]).
func (QwenCode) charges(_, member string) bool { return qwenChatMember(member) }

// transcript reads a Qwen Code archive ([qwenArchiveFormat]): its chat file,
// as a session file is read ([transcriptBuilder.qwenSession]).
func (QwenCode) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	return b.chatArchive(ctx, archive, qwenChatMember, b.qwenSession)
}

// qwenChatMember reports whether an archive's member named name is a Qwen
// Code chat file: `projects/<key>/chats/<id>.jsonl`.
func qwenChatMember(name string) bool {
	parts := strings.Split(path.Clean(name), "/")
	return len(parts) == 4 && parts[0] == "projects" && parts[1] != "" && parts[2] == "chats" &&
		strings.HasSuffix(parts[3], ".jsonl") && parts[3] != ".jsonl"
}

// qwenLocate is shell that sets cf to the chat file of the session "$s" names,
// and cm to its member's name in an archive, "" for none: the file under any
// key, since a pack made outside the run's script knows no working directory,
// and a session's id is a uuid no other session takes.
func qwenLocate(paths Paths) string {
	dir := shellword.Quote(paths.QwenDir())
	return `cf= cm=; for f in ` + dir + `/projects/*/chats/"$s".jsonl; do if [ -s "$f" ] && [ ! -L "$f" ]; ` +
		`then cf=$f cm=${f#` + dir + `/}; fi; done`
}

// qwenHeld is shell that sets cf to the box's copy of the session "$s" names
// that a run in this working directory resumes, "" for none: under the
// directory's own key, as Qwen Code keys it by the realpath.
func qwenHeld(paths Paths) string {
	return `cf=` + shellword.Quote(paths.QwenDir()) + `/projects/"$(pwd -P | sed 's/[^A-Za-z0-9]/-/g')"/chats/"$s".jsonl; ` +
		`if [ ! -s "$cf" ] || [ -L "$cf" ]; then cf=; fi`
}

// qwenGate is a Qwen Code run's part of the session gate ([chatSessionGate]),
// its other wiring's copies fresh where the run was handed one.
func qwenGate(paths Paths, fresh []freshCopy) chatGate {
	q := shellword.Quote
	restore := strings.Join([]string{
		`rf=`,
		`for f in "$in"/projects/*/chats/"$s".jsonl; do if [ -z "$rf" ] && [ -f "$f" ] && [ ! -L "$f" ]; then rf=$f; fi; done`,
		`[ -n "$rf" ]`,
		// FILED UNDER THIS DIRECTORY'S KEY, its first line's cwd rewritten to
		// the realpath, every other key's copy of it removed.
		`node ` + q(paths.QwenHelper()) + ` restore ` + q(paths.QwenHelperConfig()) + ` "$s" "$rf"`,
	}, " && ")
	return chatGate{
		harness: "Qwen Code",
		locate:  qwenHeld(paths),
		restore: restore,
		cut:     qwenUnansweredCut,
		release: qwenRelease,
		fresh:   fresh,
	}
}

// qwenUnansweredCut cuts the continued chat file "$cf" back to before the
// first note record (`user`, subtype `mid_turn_user_message`) after its last
// assistant record. session/load replays a note no reply followed, after a
// failed request and after a kill alike (measured on 0.28.4), and the run's
// notes route serves it again, never confirmed: kept, it would be delivered
// twice. A type is read only off the fields 0.28.4 writes first,
// which nothing escaped precedes; a record whose type does not read is kept
// with everything before it, as an assistant record is. No awk, no cut, said.
const qwenUnansweredCut = `if [ -z "$cf" ]; then :; elif command -v awk > /dev/null 2>&1; then ` +
	`qc=$(awk '{ t = "" } ` +
	`match($0, /^\{"uuid":"[^"\\]*","parentUuid":(null|"[^"\\]*"),"sessionId":"[^"\\]*","timestamp":"[^"\\]*","type":"[a-z_]*"/) ` +
	`{ t = substr($0, 1, RLENGTH); sub(/^.*"type":"/, "", t); sub(/"$/, "", t) } ` +
	`t == "user" { h = $0; i = index(h, ",\"message\":"); if (i > 0) h = substr(h, 1, i); ` +
	`if (index(h, ",\"subtype\":\"mid_turn_user_message\"") > 0) { if (c == 0) c = NR; next } } ` +
	`t == "" || t == "assistant" { c = 0 } END { print c + 0 }' "$cf" 2> /dev/null) || qc=0; ` +
	`if [ "${qc:-0}" -gt 1 ] 2> /dev/null; then head -n "$((qc - 1))" "$cf" > "$cf.cut" && mv -f "$cf.cut" "$cf"; fi; ` +
	`else echo 'crewlet: this box has no awk, so a note the continued session recorded with no reply after it is loaded with it' >&2; fi`

// qwenFreshCopies are a Qwen Code run's other wiring's files
// ([sandbox.RunRequest.FreshWiring]): its settings, which name the bridge's
// token in each server's headers, and its helper's configuration, which names
// the notes and skill routes.
func qwenFreshCopies(paths Paths) []freshCopy {
	return []freshCopy{
		{copy: paths.QwenSettings() + ".fresh", file: paths.QwenSettings()},
		{copy: paths.QwenHelperConfig() + ".fresh", file: paths.QwenHelperConfig()},
	}
}
