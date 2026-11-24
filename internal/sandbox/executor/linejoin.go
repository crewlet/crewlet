package executor

import (
	"bytes"
	"fmt"
	"io"
	"unicode/utf8"
)

// joinRule is how a whole-line read joins a list's parts into one text: the
// texts of the parts of kind, or of every part where anyKind says its decode
// reads no `type`, sep between them, a blank one dropped where dropBlank says
// so.
type joinRule struct {
	kind, sep          string
	anyKind, dropBlank bool
}

// textJoin is one text a whole-line read joins from a list's parts, each an
// object whose `type` and `text` it reads ([joinRule]): held while within the
// read's value bound, streamed past it where shaped ([streamedEntry]), and
// only counted where not. PAST THE BOUND A PART IS KEPT WHATEVER IT HOLDS (R2),
// where it is written in order, its `type` once and then its `text` once (its
// `text` once, for a rule of any kind); a join holding a part whose text runs
// past the bound written otherwise is lost, and said in its place
// ([textJoin.entry]). Past the bound a framed join is written through its
// frame ([textFrame]), which sees every byte joined from there on.
type textJoin struct {
	b      *transcriptBuilder
	e      TranscriptEntry
	shaped bool
	rule   joinRule
	// held is the parts joined while within the bound, kept how many; over
	// says the join ran past it unshaped, stream is its text past it shaped;
	// worded says a part kept holds more than white space, lost that a part
	// ran past the bound out of order.
	held   []byte
	kept   int
	over   bool
	stream *streamedEntry
	wrote  bool
	worded bool
	lost   bool
	// endedP is the stream's entry once ended, the same in every reading.
	endedP pendingEntry
	ended  bool
	// frame, where set, is what the join is written through past the bound.
	frame *textFrame
	// The part being read: its kind and how often each member was written, its
	// text, and how that text stands against the bound.
	partKind                                     string
	partKinds, partTexts                         int
	part                                         []byte
	partPast, partKept, partStreaming, partBlank bool
}

// start starts a join by rule that makes e past the bound, streamed where
// shaped.
func (j *textJoin) start(b *transcriptBuilder, e TranscriptEntry, shaped bool, rule joinRule) {
	*j = textJoin{b: b, e: e, shaped: shaped, rule: rule, held: lineScratch(j.held), part: lineScratch(j.part)}
}

// partStart starts the next part.
func (j *textJoin) partStart() {
	j.partKind, j.partKinds, j.partTexts, j.part = "", 0, 0, lineScratch(j.part)
	j.partPast, j.partKept, j.partStreaming, j.partBlank = false, false, false, true
}

// partType keeps the part's kind, "" for none.
func (j *textJoin) partType(kind string) {
	j.partKinds++
	j.partKind = kind
	j.outOfOrder()
}

// partText starts the part's text, written next ([textJoin.Write]); a text
// that is null holds nothing.
func (j *textJoin) partText() {
	j.partTexts++
	j.outOfOrder()
	j.part, j.partBlank = j.part[:0], true
}

// outOfOrder loses the join where the part's text ran past the bound and the
// part is no longer written in order.
func (j *textJoin) outOfOrder() {
	if j.partPast && !j.inOrder() {
		j.lost, j.partStreaming, j.partKept = true, false, false
	}
}

// inOrder reports whether the part is written in order so far: its `type`
// once and its `text` once, or its `text` once for a rule of any kind.
func (j *textJoin) inOrder() bool {
	return j.partTexts == 1 && (j.rule.anyKind || j.partKinds == 1)
}

// ofKind reports whether the part is of the kind the rule joins.
func (j *textJoin) ofKind() bool { return j.rule.anyKind || j.partKind == j.rule.kind }

// Write reads the part's text's next bytes: held to the bound, then kept past
// it as R2 says.
func (j *textJoin) Write(p []byte) (int, error) {
	j.partBlank = j.partBlank && blank(p)
	switch {
	case j.partStreaming:
		j.out(p)
		return len(p), nil
	case j.partPast:
		return len(p), nil
	case len(j.part)+len(p) <= j.b.valueMax:
		j.part = append(j.part, p...)
		return len(p), nil
	}
	j.partPast = true
	switch {
	case !j.inOrder():
		j.lost = true
	case !j.ofKind() || j.lost:
	default:
		j.partKept, j.partStreaming = true, true
		j.leave()
		j.element(j.part)
		j.out(p)
		j.kept++
	}
	j.part = lineScratch(j.part)
	return len(p), nil
}

// partEnd joins the part just read: its text where it is of the rule's kind
// and not a blank one the rule drops.
func (j *textJoin) partEnd() {
	switch {
	case j.partKept:
		j.partStreaming = false
		j.worded = j.worded || !j.partBlank
		return
	case j.partPast, j.lost, !j.ofKind(), j.rule.dropBlank && j.partBlank:
		return
	}
	j.worded = j.worded || !j.partBlank
	sep := len(j.rule.sep)
	if j.kept == 0 {
		sep = 0
	}
	switch {
	case j.stream != nil, j.over:
		j.element(j.part)
	case j.kept == 0:
		// THE FIRST PART IS THE JOIN SO FAR, taken over rather than copied:
		// a text held to the bound is held once.
		j.held, j.part = j.part, j.held[:0]
	case len(j.held)+sep+len(j.part) <= j.b.valueMax:
		j.held = append(j.held, j.rule.sep[:sep]...)
		j.held = append(j.held, j.part...)
	default:
		j.leave()
		j.element(j.part)
	}
	j.kept++
}

// leave takes the join past the bound, once: into its stream where shaped,
// only counted (over) where not, the parts joined so far written on.
func (j *textJoin) leave() {
	if j.stream != nil || j.over {
		return
	}
	j.wrote = j.kept > 0
	if j.over = !j.shaped; !j.over {
		j.stream = j.b.streamed(j.e)
		if j.frame != nil {
			j.frame.w = j.stream
		}
	}
	j.out(j.held)
	j.held = lineScratch(j.held)
}

// element writes a part's text on past the bound, after sep where a part was
// kept before it.
func (j *textJoin) element(p []byte) {
	if j.wrote {
		j.out([]byte(j.rule.sep))
	}
	j.out(p)
	j.wrote = true
}

// out writes p, the join's next bytes past the bound, through its frame where
// it has one, to its stream where it has one.
func (j *textJoin) out(p []byte) {
	switch {
	case j.frame != nil:
		_, _ = j.frame.Write(p)
	case j.stream != nil:
		_, _ = j.stream.Write(p)
	}
}

// blank reports whether the join holds white space alone, or nothing.
func (j *textJoin) blank() bool { return !j.worded }

// said reports whether a join that drops its blank parts makes an entry: it
// holds words, or is lost and said in its place.
func (j *textJoin) said() bool { return j.worded || j.lost }

// text is the join while held within the bound, false once it is not.
func (j *textJoin) text() (string, bool) {
	return string(j.held), !j.lost && !j.over && j.stream == nil
}

// entry is the join's entry at ordinal: an [EntryUnread] where it is lost,
// which every filter keeps; its stream's, ended once, past the bound shaped;
// one only counted past it unshaped; else its text held.
func (j *textJoin) entry(ordinal int) pendingEntry {
	switch {
	case j.lost:
		return j.b.held(TranscriptEntry{Kind: EntryUnread, Thread: j.e.Thread}, fmt.Sprintf("a text of the "+
			"harness's record runs past %s in a part written out of order, and was not read",
			humanSize(int64(j.b.valueMax))), ordinal)
	case j.stream != nil:
		if !j.ended {
			j.endedP, j.ended = j.stream.end(ordinal), true
		}
		return j.endedP
	case j.over:
		return j.b.held(j.e, "", ordinal)
	}
	return j.b.held(j.e, string(j.held), ordinal)
}

// textFrame is a join's text past the bound written through to w, nil for
// none, as strings.TrimSuffix(strings.TrimPrefix(text, open), close) reads it:
// its opening held until it is known to be open or not, and its last bytes,
// close's length and a character they split, held until the text ends
// ([textFrame.end]). worded says a byte written on is no white space.
type textFrame struct {
	w           io.Writer
	open, close string
	head, tail  []byte
	opened      bool
	worded      bool
}

// textFramePiece is the most of a write a frame takes into its tail at once.
const textFramePiece = 4 << 10

// frame starts a frame of open and close.
func (f *textFrame) frame(open, close string) {
	*f = textFrame{open: open, close: close, head: f.head[:0], tail: f.tail[:0]}
}

func (f *textFrame) Write(p []byte) (int, error) {
	n := len(p)
	if !f.opened {
		k := min(len(f.open)-len(f.head), len(p))
		f.head, p = append(f.head, p[:k]...), p[k:]
		switch {
		case !bytes.HasPrefix([]byte(f.open), f.head):
			f.opened = true
			f.pass(f.head)
		case len(f.head) == len(f.open):
			f.opened = true
		default:
			return n, nil
		}
		f.head = f.head[:0]
	}
	f.pass(p)
	return n, nil
}

// pass writes p on but the tail it holds back.
func (f *textFrame) pass(p []byte) {
	for len(p) > 0 {
		k := min(len(p), textFramePiece)
		f.tail, p = append(f.tail, p[:k]...), p[k:]
		cut := len(f.tail) - len(f.close)
		for back := 0; cut > 0 && back < utf8.UTFMax-1 && !utf8.RuneStart(f.tail[cut]); back++ {
			cut--
		}
		if cut > 0 {
			f.emit(f.tail[:cut])
			f.tail = f.tail[:copy(f.tail, f.tail[cut:])]
		}
	}
}

// emit writes p on.
func (f *textFrame) emit(p []byte) {
	f.worded = f.worded || !blank(p)
	if f.w != nil {
		_, _ = f.w.Write(p)
	}
}

// end ends the text: an opening never told is written on, and the tail held
// back but close where it ends the text.
func (f *textFrame) end() {
	if !f.opened {
		f.opened = true
		f.pass(f.head)
		f.head = f.head[:0]
	}
	tail := f.tail
	if bytes.HasSuffix(tail, []byte(f.close)) {
		tail = tail[:len(tail)-len(f.close)]
	}
	f.emit(tail)
	f.tail = f.tail[:0]
}
