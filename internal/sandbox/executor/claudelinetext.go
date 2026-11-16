package executor

import (
	"bytes"
	"crypto/sha256"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE TEXTS A THREAD'S READERS READ, READ AS THEY STREAM.
//
// A text a thread line holds (a prompt, a tool's result, a task notification,
// a skill's name) is as long as whatever wrote it made it: Claude Code keeps no
// bound on a line. What a reader asks of one is a property of it, never the
// text itself: whether it says anything, the subagent and the leg a task
// notification names, whether it names the handback tool, its length and
// digests, its length in UTF-16 code units. Each of those is read here by an
// automaton that takes the text's DECODED characters in pieces, in order, and
// answers exactly as the whole-text rule it replaces answers, at any length,
// holding a bounded state: [textSays], [textNotice], [textHandback],
// [textResult] and [textUnits].
//
// IN PIECES OF WHOLE CHARACTERS: a piece never ends inside a UTF-8 sequence,
// as jsonscan's Scanner.Read never ends one, and as [textCut] cuts a whole
// text. Two automata read characters ([textSays], [textUnits]); the others read
// bytes, which any cut leaves in order.
//
// A whole text a reader already holds is handed to them through [textCut],
// one fixed buffer, so reading one costs no copy of it.

// textPieceBytes is the most of a text one piece holds ([textCut]): 4 KiB, a
// page, so a cut costs its caller one fixed buffer however long the text, and
// at least [utf8.UTFMax], so every piece holds a character.
const textPieceBytes = 4 << 10

// textCut hands a whole text out in pieces of whole characters, each copied
// into its one buffer: next is the next piece, valid until the next call, and
// nil once the text is out.
type textCut struct {
	text string
	buf  [textPieceBytes]byte
}

// next is the text's next piece, nil at its end. NEVER INSIDE A CHARACTER: a
// piece that would end inside one ends before its first byte instead, a byte
// that is no UTF-8 continuation byte, so every character a reader decodes from
// the pieces is one it decodes from the whole text. Where the byte at the cut
// and the three before it are all continuation bytes, which no valid text
// holds, no character spans the cut (none is longer than [utf8.UTFMax] bytes),
// and the piece ends there.
func (c *textCut) next() []byte {
	if len(c.text) == 0 {
		return nil
	}
	n := copy(c.buf[:], c.text)
	if n < len(c.text) {
		for k := n; k > n-utf8.UTFMax && k > 0; k-- {
			if utf8.RuneStart(c.text[k]) {
				n = k
				break
			}
		}
	}
	c.text = c.text[n:]
	return c.buf[:n]
}

// textSays reads whether a text SAYS ANYTHING, as Claude Code reads it with
// JavaScript's trim(): whether it holds a character [jsTrimKeeps] keeps,
// anywhere ([threadMessage.Says]). Once one is met the rest is passed over.
type textSays struct{ any bool }

// write reads the text's next piece.
func (s *textSays) write(p []byte) {
	if !s.any {
		s.any = bytes.ContainsFunc(p, jsTrimKeeps)
	}
}

// The lines of a task notification [textNotice] reads, and the longest of
// them it reads whole.
const (
	noticeHeader     = "<task-notification>"
	noticeAgentOpen  = "<task-id>"
	noticeAgentClose = "</task-id>"
	noticeLegOpen    = "<tool-use-id>"
	noticeLegClose   = "</tool-use-id>"
	noticeFileOpen   = "<output-file>"
	noticeFileClose  = "</output-file>"

	// noticeLineMax is the longest line of a notification that is compared
	// whole: the header, the agent's line at [claudeAgentMaxBytes], the leg's
	// at [sandbox.MaxBridgeCallIDBytes], and the status. A longer line is none
	// of them; an output file's line is read by its ends and its length.
	noticeLineMax = max(len(noticeHeader), len(noticeAgentOpen)+claudeAgentMaxBytes+len(noticeAgentClose),
		len(noticeLegOpen)+sandbox.MaxBridgeCallIDBytes+len(noticeLegClose), len(claudeNotificationCompleted))
)

// textNotice reads the subagent and the leg a TASK NOTIFICATION's text names:
// its lines, split at a line feed alone (a carriage return stays its line's),
// in order, `<task-notification>`, `<task-id>A</task-id>`,
// `<tool-use-id>L</tool-use-id>`, any number of `<output-file>…</output-file>`
// (an opening and a closing that do not overlap), and
// `<status>completed</status>`, nothing after the status read. None for any
// other text, an agent outside [validClaudeAgent]'s rule, or a leg outside the
// call-id rule ([sandbox.ValidCallID]); and none for a text that ends before
// its status, whose next line, past its end, is empty.
//
// IT HOLDS NO LINE BUT ONE IT COMPARES WHOLE: the first [noticeLineMax] bytes
// of the line it is in, the last bytes of it as long as an output file's
// closing, and its length, so an output file's line of any length, and any
// number of them, cost it nothing more. The agent and the leg it names it
// keeps as strings of its own, at most 64 bytes each, never a piece of the
// text.
type textNotice struct {
	line int   // the line it is in: 0 the header, 1 the agent's, 2 the leg's, and on
	n    int64 // the bytes of it so far
	head [noticeLineMax]byte
	tail [len(noticeFileClose)]byte
	// done is whether the answer is decided, and ok what it is.
	done, ok   bool
	agent, leg string
}

// write reads the text's next piece.
func (a *textNotice) write(p []byte) {
	for !a.done && len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		if end < 0 {
			a.take(p)
			return
		}
		a.take(p[:end])
		a.endLine()
		p = p[end+1:]
	}
}

// take reads seg, the next bytes of the line it is in, no line feed among
// them.
func (a *textNotice) take(seg []byte) {
	if a.n < int64(len(a.head)) {
		copy(a.head[a.n:], seg)
	}
	a.n += int64(len(seg))
	if len(seg) >= len(a.tail) {
		copy(a.tail[:], seg[len(seg)-len(a.tail):])
		return
	}
	copy(a.tail[:], a.tail[len(seg):])
	copy(a.tail[len(a.tail)-len(seg):], seg)
}

// endLine decides the line it is in, which a line feed or the text's end
// ended.
func (a *textNotice) endLine() {
	var whole []byte // the line, where it is short enough to be compared whole
	held := a.n <= int64(len(a.head))
	if held {
		whole = a.head[:a.n]
	}
	var ok bool // whether the line is the one it waits for
	switch a.line {
	case 0:
		ok = held && string(whole) == noticeHeader
	case 1:
		var agent []byte
		if agent, ok = noticeEnclosed(whole, held, noticeAgentOpen, noticeAgentClose); ok && validClaudeAgent(string(agent)) {
			a.agent = string(agent)
		} else {
			ok = false
		}
	case 2:
		var leg []byte
		if leg, ok = noticeEnclosed(whole, held, noticeLegOpen, noticeLegClose); ok && sandbox.ValidCallID(string(leg)) {
			a.leg = string(leg)
		} else {
			ok = false
		}
	default:
		// An output file's line, of any length, by its ends: an opening and a
		// closing that do not overlap.
		if a.n >= int64(len(noticeFileOpen)+len(noticeFileClose)) &&
			string(a.head[:len(noticeFileOpen)]) == noticeFileOpen && string(a.tail[:]) == noticeFileClose {
			ok = true
			break
		}
		// Else the status, which decides the text whatever follows it.
		if held && string(whole) == claudeNotificationCompleted {
			a.done, a.ok = true, true
			return
		}
	}
	if !ok {
		a.done, a.agent, a.leg = true, "", ""
		return
	}
	a.line, a.n = a.line+1, 0
}

// end reads the text's end: its last line, and where that leaves the answer
// open, the empty line past it, which no line it waits for is.
func (a *textNotice) end() {
	if !a.done {
		a.endLine()
	}
	if !a.done {
		a.done, a.agent, a.leg = true, "", ""
	}
}

// notice is the subagent and the leg the text names, once [textNotice.end]
// has read its end.
func (a *textNotice) notice() (agent, leg string, ok bool) {
	if !a.ok {
		return "", "", false
	}
	return a.agent, a.leg, true
}

// noticeEnclosed is what line, where whole says it is held whole, holds
// between opening and closing, where line is exactly that.
func noticeEnclosed(line []byte, whole bool, opening, closing string) ([]byte, bool) {
	if !whole {
		return nil, false
	}
	inner, ok := bytes.CutPrefix(line, []byte(opening))
	if !ok {
		return nil, false
	}
	return bytes.CutSuffix(inner, []byte(closing))
}

// textHandback reads whether a task notification's text, whose leading lines
// name a subagent's leg ([textNotice]), carries a handback
// ([carriesHandback]): where it names [claudeToolHandback] anywhere, read
// through the last bytes of the piece before, one fewer than the name, so a
// name a cut splits is met.
//
// THE NOTIFICATION MARKS A HANDBACK BY ITS WORDS ALONE. Both releases build a
// completion's notification (2.1.322's MXt @298314707 through Pi @292640802,
// 2.1.310's W0t @251577375 through Di) with the subagent's answer, joined, as
// its `<result>`, and no tag of a handback's own: so under a handback its
// result is the harness's words in the report's place, and each of their
// three forms names the tool (2.1.322's H3i, tOr and iOr @288119298 and
// @287819913, 2.1.310's Eqs, kqs and jQn @245869660 and @242470305, the tool
// SubagentHandback in each). A report of the subagent's own that names it
// costs only its leg, the safe direction.
type textHandback struct {
	carry [len(claudeToolHandback) - 1]byte
	held  int // how much of carry the text has filled
	named bool
}

// write reads the text's next piece.
func (h *textHandback) write(p []byte) {
	if h.named || len(p) == 0 {
		return
	}
	// A name that starts in the carry ends within p's first len(carry) bytes.
	var joined [2 * len(h.carry)]byte
	n := copy(joined[:], h.carry[:h.held])
	n += copy(joined[n:], p)
	if bytes.Contains(joined[:n], []byte(claudeToolHandback)) || bytes.Contains(p, []byte(claudeToolHandback)) {
		h.named = true
		return
	}
	if len(p) >= len(h.carry) {
		h.held = copy(h.carry[:], p[len(p)-len(h.carry):])
		return
	}
	// p is shorter than the carry, so joined holds the carry and all of p.
	keep := joined[max(0, n-len(h.carry)):n]
	h.held = copy(h.carry[:], keep)
}

// A log's whole output is at least as long as its opening, so the bytes
// [textResult] keeps for the one hold the other: this does not compile where
// [sandbox.MaxBridgeOutputBytes] is shorter than [sandbox.HoldingsOpeningBytes].
var _ [sandbox.MaxBridgeOutputBytes - sandbox.HoldingsOpeningBytes]struct{}

// threadResultText is a tool_result's text as the reading compares it with a
// log's output ([threadCall.Text]): Bytes its decoded length, -1 where the
// result's content holds no text ([resultText]); Opening the SHA-256 of its
// first [sandbox.HoldingsOpeningBytes]; Whole the SHA-256 of all of it, where
// Bytes is at most [sandbox.MaxBridgeOutputBytes], else zero, as a log keeps
// no longer output whole to compare. A call holds it by value: a text of any
// length costs a call these 72 bytes.
type threadResultText struct {
	Bytes          int64
	Opening, Whole [sha256.Size]byte
}

// textResult reads a tool_result's text into its length and its two digests
// ([threadResultText]), keeping its first [sandbox.MaxBridgeOutputBytes] bytes
// and no more, so a text of any length costs it that.
type textResult struct {
	n    int64
	kept [sandbox.MaxBridgeOutputBytes]byte
}

// write reads the text's next piece.
func (r *textResult) write(p []byte) {
	if r.n < int64(len(r.kept)) {
		copy(r.kept[r.n:], p)
	}
	r.n += int64(len(p))
}

// sum is the text read so far, as the reading compares it.
func (r *textResult) sum() threadResultText {
	held := r.kept[:min(r.n, int64(len(r.kept)))]
	t := threadResultText{Bytes: r.n, Opening: sha256.Sum256(held[:min(len(held), sandbox.HoldingsOpeningBytes)])}
	// WHOLE ONLY WHERE A LOG COULD HAVE KEPT IT WHOLE: a longer text is
	// compared by its opening alone, so its whole is never hashed.
	if r.n <= int64(len(r.kept)) {
		t.Whole = sha256.Sum256(held)
	}
	return t
}

// textUnits counts a text's UTF-16 code units, as JavaScript counts a string's
// length: two for a character past the basic plane, one for every other. It
// stops counting once the count passes [skillListingMaxNameUnits], the one
// bound its reader holds a count to ([skillNamesKept]).
type textUnits struct{ n int }

// write reads the text's next piece.
func (u *textUnits) write(p []byte) {
	for len(p) > 0 && u.n <= skillListingMaxNameUnits {
		r, size := utf8.DecodeRune(p)
		u.n++
		if r > 0xffff {
			u.n++
		}
		p = p[size:]
	}
}

// units is the text's length in UTF-16 code units, or one more than
// [skillListingMaxNameUnits] where it is longer than that.
func (u *textUnits) units() int { return min(u.n, skillListingMaxNameUnits+1) }
