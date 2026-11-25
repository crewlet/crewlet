package executor

import (
	"fmt"
	"strconv"

	"github.com/crewlet/crewlet/internal/shellword"
)

// A CHAT FILE IS TRIMMED BEFORE ITS NEWEST COMPACTION, NEVER PAST ITS RUN'S
// START (Gemini CLI, Qwen Code; measured on 0.70.0 and 0.28.4,
// docs/concepts/code-sandbox.md): the cut keeps line 1, which each restore
// reads, and every line from the compaction's own first on, its harness's
// boundary ([geminiBoundaryAwk], [qwenBoundaryAwk]); a resume of it sent the
// request a resume of the whole file did, byte for byte, and so did one that
// kept any lines before the boundary too. So it leaves out only lines the
// session held before its run began ([trimStartVar]): a session the run began
// is never cut, and the lines the take checks as the run's start are
// restated over the cut copy ([trimCut]).

// trimStartVar is the shell variable a chat pack reads its run's start from:
// the lines its record held before the run began, 0 where the run began it,
// unset where that is not known, which cuts nothing. The run's script sets it
// ([chatRunStartFrom]), and [Runner.packNow] from the record.
const trimStartVar = "trs"

// trimStartOf is [trimStartVar]'s value for rec's run as h records it:
// its start's lines, 0 where it began its session, "" where neither is known.
func trimStartOf(rec sessionRecord, h Harness) string {
	start, prefix := rec.runStart(h)
	switch {
	case start.Began:
		return "0"
	case prefix.Lines > 0:
		return strconv.FormatInt(prefix.Lines, 10)
	}
	return ""
}

// chatTrimRecord is the cut of a chat file at src into dst ([trimmer]):
// boundary, an awk program printing the line its newest compaction opens on
// (failing where there is none), finds B; the cut leaves out lines 2 to
// C = min(B-1, [trimStartVar]) and keeps the rest, where C is 2 or more, so a
// start below 2 lines, or none, cuts nothing and reads nothing. It sets trp
// to the cut copy's first lines that held the run's start,
// `<lines> <crc> <bytes>` as [chatRunStartFrom] writes `from`, for the take
// to check in its place ([parseTrimMark]).
func chatTrimRecord(boundary, src, dst string) string {
	return fmt.Sprintf(`[ "$%[4]s" -ge 2 ] 2> /dev/null && trb=$(LC_ALL=C awk %[3]s %[1]s) && trc=$((trb - 1)) && `+
		`{ [ "$trc" -le "$%[4]s" ] || trc=$%[4]s; } && [ "$trc" -ge 2 ] && `+
		`{ head -n 1 %[1]s && tail -n "+$((trc + 1))" %[1]s; } > %[2]s && trl=$((%[4]s - trc + 1)) && `+
		`trp="$trl $(head -n "$trl" %[2]s | cksum)"`,
		src, dst, shellword.Quote(boundary), trimStartVar)
}

// geminiBoundaryAwk prints the line Gemini CLI 0.70.0's newest compaction
// opens on: the `$set` two lines before its reply, a summary's entry between
// ([geminiCompressionSet]), whose sync closed with the `$patch` naming what a
// resume drops before the next `$set` of its time. A sync a pack met unclosed
// would resume holding its old entries beside the new, and is no boundary.
var geminiBoundaryAwk = lineFormsAwk(namedForm{"set", geminiCompressionSet},
	namedForm{"user", geminiCompressionUser}, namedForm{"ack", geminiCompressionAck},
	namedForm{"closed", geminiSyncClosed}, namedForm{"touched", geminiSyncTouched}) +
	`{ if (s2 && u1 && holds($0, "ack")) pend = NR - 2
  else if (pend && holds($0, "closed")) { b = pend; pend = 0 }
  else if (pend && holds($0, "touched")) pend = 0
  s2 = s1; s1 = holds($0, "set"); u1 = holds($0, "user") }
END { if (b) print b; exit !b }`

// The lines that close a compression's sync and that follow one, as 0.70.0
// writes them ([geminiBoundaryAwk]).
var (
	geminiSyncClosed  = lineForm{pieces: []string{`{"$patch":{"removeIds":[`}}
	geminiSyncTouched = lineForm{pieces: []string{`{"$set":{"lastUpdated":"`}}
)

// qwenCompressions are the openings of a Qwen Code 0.28.4 `chat_compression`
// record: its top-level fields in the order createBaseRecord writes them,
// its parent null or an id, its git branch where the run had one, through
// to its payload's brace, so no payload names one (measured).
var qwenCompressions = func() []namedForm {
	var forms []namedForm
	for _, parent := range [][]string{{`","parentUuid":null,"sessionId":"`}, {`","parentUuid":"`, `","sessionId":"`}} {
		for _, branch := range [][]string{nil, {`","gitBranch":"`}} {
			pieces := append([]string{`{"uuid":"`}, parent...)
			pieces = append(pieces, `","timestamp":"`, `","type":"system","provenance":"system","cwd":"`,
				`","version":"`)
			pieces = append(pieces, branch...)
			pieces = append(pieces, `","subtype":"chat_compression","systemPayload":{`)
			forms = append(forms, namedForm{fmt.Sprint(len(forms)), lineForm{pieces: pieces}})
		}
	}
	return forms
}()

// qwenBoundaryAwk prints the line Qwen Code 0.28.4's newest compaction opens
// on: its newest `chat_compression` record, each of which carries the whole
// history a resume rebuilds from (measured).
var qwenBoundaryAwk = lineFormsAwk(qwenCompressions...) +
	`holds($0, "0") || holds($0, "1") || holds($0, "2") || holds($0, "3") { b = NR }
END { if (b) print b; exit !b }`

// trimRelease is the release Gemini CLI's trim was measured on ([trimmer]):
// the one release a session continues on.
func (GeminiCLI) trimRelease() string { return geminiRelease }

// trimRecord cuts a chat file before its newest compression
// ([chatTrimRecord], [geminiBoundaryAwk]).
func (GeminiCLI) trimRecord(src, dst string) string {
	return chatTrimRecord(geminiBoundaryAwk, src, dst)
}

// trimRelease is the release Qwen Code's trim was measured on ([trimmer]):
// the one release a session continues on.
func (QwenCode) trimRelease() string { return qwenRelease }

// trimRecord cuts a chat file before its newest compaction
// ([chatTrimRecord], [qwenBoundaryAwk]).
func (QwenCode) trimRecord(src, dst string) string { return chatTrimRecord(qwenBoundaryAwk, src, dst) }

var (
	_ trimmer = GeminiCLI{}
	_ trimmer = QwenCode{}
)
