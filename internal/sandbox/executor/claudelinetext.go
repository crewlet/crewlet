package executor

import (
	"bytes"
	"crypto/sha256"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A text a thread line holds (a prompt, a tool's result, a task notification,
// a skill's name) has no bound: Claude Code keeps none on a line. Readers ask
// only properties of one, so each property is read by an automaton that takes
// the text's DECODED characters in pieces, in order, holds a bounded state, and
// answers exactly as its whole-text rule at any length ([textSays],
// [textNotice], [textHandback], [textResult], [textUnits]; held to those rules
// by TestATextsFactsAreReadAsItsWholeTextReadsThem).
//
// A piece never ends inside a UTF-8 sequence, as jsonscan's Scanner.Read never
// does: [textSays] and [textUnits] read characters, the others bytes. A whole
// text already held is handed over through [textCut], one fixed buffer, never a
// copy of it.

// textPieceBytes is the most one piece of [textCut] holds: 4 KiB, a page, so a
// cut costs one fixed buffer however long the text; at least [utf8.UTFMax], so
// every piece holds a character.
const textPieceBytes = 4 << 10

// textCut hands a whole text out in pieces of whole characters, each copied
// into its one buffer.
type textCut struct {
	text string
	buf  [textPieceBytes]byte
}

// next is the text's next piece, valid until the next call; nil at its end. A
// piece that would end inside a character ends before that character's first
// byte instead. Where the byte at the cut and the three before it are all
// continuation bytes, which no valid text holds, no character spans the cut
// (none is longer than [utf8.UTFMax]), so the piece ends there.
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
// JavaScript's trim(): whether it holds a character [jsTrimKeeps] keeps
// ([threadMessage.Says]).
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

	// noticeLineMax is the longest notification line compared whole: the
	// header, the agent's at [claudeAgentMaxBytes], the leg's at
	// [sandbox.MaxBridgeCallIDBytes], and the status. A longer line is none of
	// them; an output file's line is read by its ends and its length.
	noticeLineMax = max(len(noticeHeader), len(noticeAgentOpen)+claudeAgentMaxBytes+len(noticeAgentClose),
		len(noticeLegOpen)+sandbox.MaxBridgeCallIDBytes+len(noticeLegClose), len(claudeNotificationCompleted))
)

// textNotice reads the subagent and the leg a TASK NOTIFICATION's text names:
// its lines, split at a line feed alone (a carriage return stays its line's),
// are `<task-notification>`, `<task-id>A</task-id>`,
// `<tool-use-id>L</tool-use-id>`, any number of `<output-file>…</output-file>`
// (opening and closing not overlapping), then `<status>completed</status>`,
// nothing after it read. None for any other text (one ending before its
// status), or an agent or leg [validClaudeAgent] or [sandbox.ValidCallID] refuses.
//
// It holds only the first [noticeLineMax] bytes of the current line, its last
// len(noticeFileClose) bytes and its length, so output files cost nothing more
// at any length or number; the agent and leg are strings of its own (64 bytes
// at most each), never a piece of the text.
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

// take reads seg, the current line's next bytes, which hold no line feed.
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

// endLine decides the current line, ended by a line feed or the text's end.
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
		// An output file's line, read by its ends, which may not overlap.
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

// end reads the text's end: its last line, then, where the answer is still
// open, the empty line past it, which no awaited line is.
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

// noticeEnclosed is what line holds between opening and closing, where it is
// held whole and is exactly that.
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

// textHandback reads whether a task notification's text ([textNotice]) carries
// a handback ([lineDecoder.toolResult]): whether it names [claudeToolHandback]
// anywhere. It carries the piece before's last len(name)-1 bytes, so a name a
// cut splits is still met.
//
// THE WORDS ALONE MARK A HANDBACK: both releases build a completion's
// notification with the subagent's answer as its `<result>` and no handback tag
// (2.1.322's MXt @298314707 through Pi @292640802, 2.1.310's W0t @251577375
// through Di), and a handback's result is the harness's words, all three forms
// naming the tool (2.1.322's H3i, tOr, iOr @288119298, @287819913; 2.1.310's
// Eqs, kqs, jQn @245869660, @242470305). A report of the subagent's own naming
// it costs only its leg, the safe direction.
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

// [textResult] hashes its opening out of the bytes it keeps: this fails to
// compile where [sandbox.MaxBridgeOutputBytes] is shorter than
// [sandbox.HoldingsOpeningBytes].
var _ [sandbox.MaxBridgeOutputBytes - sandbox.HoldingsOpeningBytes]struct{}

// threadResultText is a tool_result's text as compared with a log's output
// ([threadCall.Text]): its decoded length ([threadResultText.Bytes]), Opening
// the SHA-256 of its first [sandbox.HoldingsOpeningBytes], and Whole the
// SHA-256 of all of it where it is at most [sandbox.MaxBridgeOutputBytes], else
// zero, since a log keeps no longer output whole. A call holds these 72 bytes
// by value, whatever the text's length.
//
// THE ZERO VALUE IS NO TEXT (a result whose content holds none,
// [lineDecoder.resultContent], and every tool_use's call), never a text of no
// bytes, whose digests are the empty text's.
type threadResultText struct {
	// size is the text's decoded length plus one; 0 where there is no text.
	size           int64
	Opening, Whole [sha256.Size]byte
}

// Bytes is the text's decoded length; -1 where there is no text.
func (t threadResultText) Bytes() int64 { return t.size - 1 }

// textResult reads a tool_result's text into a [threadResultText], keeping its
// first [sandbox.MaxBridgeOutputBytes] bytes and no more.
type textResult struct {
	n    int64
	kept [sandbox.MaxBridgeOutputBytes]byte
}

// reset makes r read a text from its start, its buffer kept: [textResult.sum]
// reads only what of the buffer the text since wrote.
func (r *textResult) reset() { r.n = 0 }

// write reads the text's next piece.
func (r *textResult) write(p []byte) {
	if r.n < int64(len(r.kept)) {
		copy(r.kept[r.n:], p)
	}
	r.n += int64(len(p))
}

// sum is the text read so far, as a [threadResultText].
func (r *textResult) sum() threadResultText {
	held := r.kept[:min(r.n, int64(len(r.kept)))]
	t := threadResultText{size: r.n + 1, Opening: sha256.Sum256(held[:min(len(held), sandbox.HoldingsOpeningBytes)])}
	if r.n <= int64(len(r.kept)) {
		t.Whole = sha256.Sum256(held)
	}
	return t
}

// textUnits counts a text's UTF-16 code units, as JavaScript counts a string's
// length: two for a character past the basic plane, one for every other. It
// stops once past [skillListingMaxNameUnits], the only bound its reader
// ([lineDecoder.skillNames]) checks.
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
