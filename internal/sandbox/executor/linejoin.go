package executor

import "fmt"

// joinRule is how a whole-line read joins a list's parts into one text: the
// texts of the parts of kind, sep between them, a blank one dropped where
// dropBlank says so.
type joinRule struct {
	kind, sep string
	dropBlank bool
}

// textJoin is one text a whole-line read joins from a list's parts, each an
// object whose `type` and `text` it reads ([joinRule]): held while within the
// read's value bound, streamed past it where shaped ([streamedEntry]), and
// only counted where not. PAST THE BOUND A PART IS KEPT WHATEVER IT HOLDS (R2),
// where it is written in order, its `type` once and then its `text` once; a
// join holding a part whose text runs past the bound written otherwise is
// lost, and said in its place ([textJoin.entry]).
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
	if j.partPast && (j.partKinds != 1 || j.partTexts != 1) {
		j.lost, j.partStreaming, j.partKept = true, false, false
	}
}

// Write reads the part's text's next bytes: held to the bound, then kept past
// it as R2 says.
func (j *textJoin) Write(p []byte) (int, error) {
	j.partBlank = j.partBlank && blank(p)
	switch {
	case j.partStreaming:
		_, _ = j.stream.Write(p)
		return len(p), nil
	case j.partPast:
		return len(p), nil
	case len(j.part)+len(p) <= j.b.valueMax:
		j.part = append(j.part, p...)
		return len(p), nil
	}
	j.partPast = true
	switch {
	case j.partKinds != 1 || j.partTexts != 1:
		j.lost = true
	case j.partKind != j.rule.kind || j.lost:
	default:
		j.partKept = true
		switch {
		case j.shaped:
			if j.stream == nil {
				j.open()
			}
			j.element(j.part)
			_, _ = j.stream.Write(p)
			j.partStreaming = true
		default:
			j.over, j.held = true, lineScratch(j.held)
		}
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
	case j.partPast, j.lost, j.partKind != j.rule.kind, j.rule.dropBlank && j.partBlank:
		return
	}
	j.worded = j.worded || !j.partBlank
	sep := len(j.rule.sep)
	if j.kept == 0 {
		sep = 0
	}
	switch {
	case j.stream != nil:
		j.element(j.part)
	case j.over:
	case j.kept == 0:
		// THE FIRST PART IS THE JOIN SO FAR, taken over rather than copied:
		// a text held to the bound is held once.
		j.held, j.part = j.part, j.held[:0]
	case len(j.held)+sep+len(j.part) <= j.b.valueMax:
		j.held = append(j.held, j.rule.sep[:sep]...)
		j.held = append(j.held, j.part...)
	case j.shaped:
		j.open()
		j.element(j.part)
	default:
		j.over, j.held = true, lineScratch(j.held)
	}
	j.kept++
}

// open starts the join's stream, holding the parts joined so far.
func (j *textJoin) open() {
	j.stream, j.wrote = j.b.streamed(j.e), j.kept > 0
	_, _ = j.stream.Write(j.held)
	j.held = lineScratch(j.held)
}

// element writes a part's text to the stream, after sep where a part was kept
// before it.
func (j *textJoin) element(p []byte) {
	if j.wrote {
		_, _ = j.stream.Write([]byte(j.rule.sep))
	}
	_, _ = j.stream.Write(p)
	j.wrote = true
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
