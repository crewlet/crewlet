package executor

import (
	"context"
	"io"
	"path"
	"strings"

	"github.com/crewlet/crewlet/internal/shellword"
)

// A GEMINI CLI SESSION CARRIED BETWEEN BOXES is its one chat file
// (chatcarry.go): `~/.gemini/tmp/crewlet/chats/session-<time>-<id8>.jsonl`,
// filed under the slug the run's helper pins before every run, so it lies at
// one path whatever the box's working directory is called. Restored there
// after the pin, it resumes (`--resume <id>`) in a fresh home and another
// working directory, its MCP call pairs whole (measured on 0.70.0, the one
// release it continues on: [geminiRelease]). Never `.project_root`: a foreign one makes the
// resume refuse the project. The file is append-only, and 0.70.0 appends a
// call's second copy as it runs it, so a pack beside the harness can end
// mid-line, which the restore cuts back ([chatSessionGate]).

// THE RECORD OF TAKES STAYS IN ITS BOX ([Paths.GeminiTakes]), never packed: a
// job's receipts are read in the box it ran in, and a run continuing the
// session elsewhere opens a record of its own there, marked with the line the
// session stood at ([geminiConfirmed]), so no delivery the carried session
// holds confirms a take of its.

// geminiArchiveFormat is the layout a Gemini CLI run packs its session in: one
// gzipped tar of the chat file, named `chats/<file>` relative to the pinned
// slug's directory ([Paths.GeminiSlugDir]). A change to what is packed, or
// where it is restored to, is a new format, which a box of this build refuses.
const geminiArchiveFormat = 1

var (
	_ resumer = GeminiCLI{}
	_ carrier = GeminiCLI{}
	_ saver   = GeminiCLI{}
)

// Resumes says Gemini CLI continues a session it reported (`--resume`).
func (GeminiCLI) Resumes() bool { return true }

// ArchiveFormat is the layout a Gemini CLI run packs its session in ([carrier]).
func (GeminiCLI) ArchiveFormat() int { return geminiArchiveFormat }

// VersionCommand prints a box's Gemini CLI release ([carrier]).
func (GeminiCLI) VersionCommand() string { return "gemini --version" }

// Continues reports whether a session Gemini CLI release writer wrote in
// another box can be continued by release box: both [geminiRelease]
// ([carrier], [exactCarry]).
func (GeminiCLI) Continues(box, writer string) (bool, string) {
	return exactCarry("Gemini CLI", geminiRelease, box, writer)
}

// ContinuesWriter is [GeminiCLI.Continues] judged of the writer alone
// ([carrier]): only a session [geminiRelease] wrote.
func (GeminiCLI) ContinuesWriter(writer string) (bool, string) {
	return exactWriter("Gemini CLI", geminiRelease, writer)
}

// continuesInPlace reports whether a box whose Gemini CLI is release box
// continues a session at all, the copy it holds as much as one carried to it
// ([inPlaceGate]): only on [geminiRelease].
func (GeminiCLI) continuesInPlace(box string) (bool, string) {
	return exactInPlace("Gemini CLI", geminiRelease, box)
}

// Pack packs session now, as a run's script packs it once the harness exits
// ([carrier]), held to the rule of a pack beside a working harness where
// working says so.
func (GeminiCLI) Pack(paths Paths, session, staged, dest string, working bool) string {
	return chatOnDemand(paths, session, packAt{staged: staged, dest: dest, working: working},
		paths.GeminiSlugDir(), geminiLocate(paths), GeminiCLI{})
}

// PackSave packs a save of rec's session beside a harness that may still be
// working ([saver]). The record refuses no Gemini CLI save, so it never errs.
func (GeminiCLI) PackSave(paths Paths, rec sessionRecord, staged, dest string) (string, error) {
	return chatOnDemand(paths, rec.id, packAt{staged: staged, dest: dest, working: true},
		paths.GeminiSlugDir(), geminiLocate(paths), GeminiCLI{}), nil
}

// startsByLines says a Gemini CLI run records where it began in its chat file as
// a line count and their digest ([lineStarter]).
func (GeminiCLI) startsByLines() {}

// layout is a Gemini CLI archive's: a gzipped tar of the chat file.
func (GeminiCLI) layout() archiveLayout { return archiveTar }

// charges reports whether member is a chat file, the one the meter counts
// ([carrier.charges], [chatMemberLines]).
func (GeminiCLI) charges(_, member string) bool { return geminiChatMember(member) }

// transcript reads a Gemini CLI archive ([geminiArchiveFormat]): its chat
// file, as a session file is read ([transcriptBuilder.geminiSession]).
func (GeminiCLI) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	return b.chatArchive(ctx, archive, geminiChatMember, b.geminiSession)
}

// geminiChatMember reports whether an archive's member named name is a
// Gemini CLI chat file: `chats/<file>.jsonl`.
func geminiChatMember(name string) bool {
	rest, ok := strings.CutPrefix(path.Clean(name), "chats/")
	return ok && rest != "" && !strings.Contains(rest, "/") && strings.HasSuffix(rest, ".jsonl")
}

// geminiLocate is shell that sets cf to the chat file of the session "$s"
// names, and cm to its member's name in an archive, "" for none: the file
// under the pinned slug named after the session's first eight characters
// whose first line names the session, since two sessions may share the eight.
func geminiLocate(paths Paths) string {
	return `cf= cm=; i8=${s%"${s#????????}"}; for f in ` + shellword.Quote(paths.GeminiChats()) +
		`/session-*-"$i8".jsonl; do if [ -s "$f" ] && [ ! -L "$f" ] && ` + geminiNamesSession(`"$f"`) +
		`; then cf=$f cm=chats/${f##*/}; fi; done`
}

// geminiNamesSession is the shell condition that the chat file file's first
// line names the session "$s": Gemini CLI writes it as compact JSON, its
// first key the session's id.
func geminiNamesSession(file string) string {
	return `head -n 1 ` + file + ` 2> /dev/null | grep -qF "\"sessionId\":\"$s\""`
}

// geminiGate is a Gemini CLI run's part of the session gate ([chatSessionGate]),
// its other wiring's copies fresh where the run was handed one.
func geminiGate(paths Paths, fresh []freshCopy) chatGate {
	q := shellword.Quote
	chats := q(paths.GeminiChats())
	restore := strings.Join([]string{
		`rf=`,
		`for f in "$in"/chats/*.jsonl; do if [ -z "$rf" ] && [ -f "$f" ] && [ ! -L "$f" ] && ` +
			geminiNamesSession(`"$f"`) + `; then rf=$f; fi; done`,
		`[ -n "$rf" ]`,
		`mkdir -p ` + chats,
		// THE BOX'S OWN COPY GOES only now, once the archive is known to hold
		// the session: every file of the slug that names it.
		`i8=${s%"${s#????????}"}`,
		`for f in ` + chats + `/session-*-"$i8".jsonl; do if [ -f "$f" ] && ` + geminiNamesSession(`"$f"`) +
			`; then rm -f "$f"; fi; done`,
		`mv "$rf" ` + chats + `/"${rf##*/}"`,
	}, " && ")
	return chatGate{
		harness: "Gemini CLI",
		locate:  geminiLocate(paths),
		restore: restore,
		release: geminiRelease,
		fresh:   fresh,
	}
}

// geminiFreshCopies are a Gemini CLI run's other wiring's files
// ([sandbox.RunRequest.FreshWiring]): its settings, which name the bridge's
// token in each server's headers, and, where the run is hooked, its take
// script's configuration, which names the notes and skill routes and the
// record of takes of the session it starts.
func geminiFreshCopies(paths Paths, hooked bool) []freshCopy {
	copies := []freshCopy{{copy: paths.GeminiSettings() + ".fresh", file: paths.GeminiSettings()}}
	if hooked {
		copies = append(copies, freshCopy{copy: paths.GeminiTakeConfig() + ".fresh", file: paths.GeminiTakeConfig()})
	}
	return copies
}
