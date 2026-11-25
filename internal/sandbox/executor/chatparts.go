package executor

import (
	"fmt"
	"io"
	"slices"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// THE GEMINI API'S CONTENT PARTS are how Qwen Code's and Gemini CLI's chat
// files record a turn (a text, a thought, a function call, a function's
// response), each wrapped in its harness's own record. A person's read of
// either walks them on the bounded line walk ([chatReading.parts]) as their
// whole-line decode read them, loosely: a member of another kind than its
// reading takes reads as none, and a part's entries are shaped into each
// reading its record may take once the part ends, since its `thought` or its
// call can be written after its text. A text is trimmed as strings.TrimSpace
// trims it ([textTrim]).

// chatReading is a person's read of a chat file's records on the walk: the
// texts of the part being read, and what its members say.
type chatReading struct {
	lineWalk
	// text is a part's text, trimmed on its way in; ended its stream's entry
	// once ended, the same in every reading.
	text     valueText
	trim     textTrim
	ended    pendingEntry
	endedSet bool
	// args is a call's input, name a tool's; output, failure and body a
	// function response's output, error and whole response, failedRaw its
	// error as written where that is no string.
	args, name, output, failure, body valueText
	compactor                         jsonCompactor
	failedRaw                         []byte
	part                              chatPart
	// firstSeen says a content's first text a thought is not was read.
	firstSeen bool
}

// chatPart is what one part's members say, each by the last of its name: a
// text, the model's reasoning, a call (its id, tool and whether its input was
// read into args), and a function's response (its id, the tool it names as
// shown, its entry and whether it failed).
type chatPart struct {
	text, thought      bool
	call, callArgs     bool
	callID             string
	callTool           toolName
	result, failed     bool
	resultID, resultAs string
	answer             pendingEntry
}

// chatSinks are the readings a content's parts are shaped into, nil for one
// its record does not take.
type chatSinks struct {
	// model is the model's words and calls in parts order (a Qwen Code
	// response).
	model *entryShape
	// user is each call's response, and each text as the brief or the notes,
	// late calls and wakes it carries ([headedIn]), but one opening with own,
	// the harness's own words ("" for none).
	user *entryShape
	own  string
	// summary takes the content's first text a thought is not, and snap its
	// words as held, for a record its reader may find a compression's summary
	// (nil for none; [geminiCompressionSet]), a box read's newest summary
	// among them ([geminiSummaryReading]).
	summary *entryShape
	snap    *summaryText
	// words take a content written as one string, as the model's words (each
	// reading of a Gemini CLI response).
	words []*entryShape
}

// parts reads the content value k begins into sk: a string is one text part,
// "" none; a list's elements that are objects are parts, the rest passed
// over; any other value holds none.
func (r *chatReading) parts(k jsonscan.Kind, sk *chatSinks) error {
	r.firstSeen = false
	switch k {
	case jsonscan.String:
		r.part = chatPart{}
		if err := r.readText(sk); err != nil {
			return err
		}
		r.part.text = r.trim.n > 0
		r.partEnd(sk, true)
		return nil
	case jsonscan.ArrayStart:
		return r.elements(func(k jsonscan.Kind) (bool, error) {
			if k != jsonscan.ObjectStart {
				return true, r.scan.Skip()
			}
			return true, r.partObject(sk)
		})
	}
	return r.scan.Skip()
}

// partObject reads one part, an object, and shapes what it makes.
func (r *chatReading) partObject(sk *chatSinks) error {
	r.part = chatPart{}
	err := r.members(func(name string, k jsonscan.Kind) error {
		switch name {
		case "text":
			if r.part.text = k == jsonscan.String; r.part.text {
				return r.readText(sk)
			}
		case "thought":
			r.part.thought = k == jsonscan.True
		case "functionCall":
			return r.call(k, sk)
		case "functionResponse":
			return r.response(k, sk)
		}
		return r.scan.Skip()
	})
	if err != nil {
		return err
	}
	r.partEnd(sk, false)
	return nil
}

// readText reads the current string as the part's text, trimmed, streamed
// past the value bound where a reading that may take it shapes it.
func (r *chatReading) readText(sk *chatSinks) error {
	b := r.b
	shaped := sk.model != nil && sk.model.shapes(b, EntryModel) || sk.user != nil && sk.user.shapes(b, EntryBrief) ||
		sk.summary != nil && !r.firstSeen && sk.summary.shapes(b, EntrySummary)
	for _, s := range sk.words {
		shaped = shaped || s.shapes(b, EntryModel)
	}
	r.endedSet = false
	r.trim.begin(r.text.begin(b, TranscriptEntry{Kind: EntryModel}, shaped), b.valueMax)
	return r.read(&r.trim)
}

// call reads a part's function call, where a reading takes calls: its id, its
// tool's name and its input, as written and compacted.
func (r *chatReading) call(k jsonscan.Kind, sk *chatSinks) error {
	p := &r.part
	p.call, p.callArgs, p.callID, p.callTool = k == jsonscan.ObjectStart, false, "", toolName{}
	if !p.call || sk.model == nil {
		return r.scan.Skip()
	}
	return r.members(func(name string, k jsonscan.Kind) error {
		var err error
		switch name {
		case "id":
			p.callID, err = r.wordValue(k)
		case "name":
			p.callTool, _, err = r.toolName(&r.name, k)
		case "args":
			p.callArgs = true
			t := r.args.begin(r.b, TranscriptEntry{Kind: EntryToolCall}, sk.model.shapes(r.b, EntryToolCall))
			r.compactor = jsonCompactor{w: t, buf: r.compactor.buf}
			if err = r.scan.Tee(&r.compactor); err == nil {
				err = r.scan.Skip()
			}
		default:
			err = r.scan.Skip()
		}
		return err
	})
}

// response reads a part's function response, where a reading takes results:
// its id, its own tool's name and its body ([chatReading.answer]); one with
// no body answered nothing.
func (r *chatReading) response(k jsonscan.Kind, sk *chatSinks) error {
	p := &r.part
	p.result, p.failed, p.resultID, p.resultAs = k == jsonscan.ObjectStart, false, "", ""
	if !p.result || sk.user == nil {
		return r.scan.Skip()
	}
	s := sk.user
	p.answer = r.b.held(TranscriptEntry{Kind: EntryToolResult}, "", s.next())
	return r.members(func(name string, k jsonscan.Kind) error {
		var err error
		switch name {
		case "id":
			p.resultID, err = r.wordValue(k)
		case "name":
			// A NAME WHOSE REDACTION THE STREAM REFUSED is shown as none: the
			// result is named by its call alone (R6).
			var tool toolName
			tool, _, err = r.toolName(&r.name, k)
			p.resultAs = tool.shown
		case "response":
			p.answer, p.failed, err = r.answer(k, s)
		default:
			err = r.scan.Skip()
		}
		return err
	})
}

// chatValue is the kind of a body's member the answer is read by.
type chatValue uint8

const (
	chatAbsent chatValue = iota
	chatNull
	chatString
	chatOther
	chatOtherPast
)

// answer reads the response body k begins as its result's entry at s's next
// ordinal, as the harness writes a tool's result: its `error` where that is
// set, a string as it is and any other value as written and compacted, and
// failed; else its `output` where that is a string of any length; else the
// whole body, as written and compacted. Each is read as the body passes,
// ONLY THE ONE THE ANSWER STANDS TO PICK KEPT ([chatAnswer]): an error of
// another kind than a string written past the bound, or a member written
// twice that picks one let go of, is said in its place.
func (r *chatReading) answer(k jsonscan.Kind, s *entryShape) (pendingEntry, bool, error) {
	b := r.b
	e := TranscriptEntry{Kind: EntryToolResult}
	ordinal, shaped := s.next(), s.shapes(b, EntryToolResult)
	a := chatAnswer{texts: [3]*valueText{&r.body, &r.output, &r.failure}}
	r.body.begin(b, e, shaped)
	r.compactor = jsonCompactor{w: a.writer(0), buf: r.compactor.buf}
	if err := r.scan.Tee(&r.compactor); err != nil {
		return pendingEntry{}, false, err
	}
	output, failure := chatAbsent, chatAbsent
	r.output.begin(b, e, shaped)
	r.failure.begin(b, e, shaped)
	var err error
	if k == jsonscan.ObjectStart {
		err = r.members(func(name string, k jsonscan.Kind) error {
			rank, kind := 1, &output
			switch name {
			case "output":
			case "error":
				rank, kind = 2, &failure
			default:
				return r.scan.Skip()
			}
			switch k {
			case jsonscan.String:
				*kind = chatString
				a.restart(rank)
				a.texts[rank].begin(b, e, shaped)
				return r.read(a.writer(rank))
			case jsonscan.Null:
				*kind = chatNull
				return nil
			}
			*kind = chatOther
			if kind != &failure {
				return r.scan.Skip()
			}
			raw, whole, captureErr := r.scan.Capture(b.valueMax)
			r.failedRaw = append(r.failedRaw[:0], raw...)
			if !whole {
				failure = chatOtherPast
			}
			return captureErr
		})
	} else {
		err = r.scan.Skip()
	}
	defer func() { r.output.drop(); r.failure.drop(); r.body.drop() }()
	if err != nil {
		return pendingEntry{}, false, err
	}
	pick := func(rank int, failed bool) (pendingEntry, bool, error) {
		if a.lost[rank] {
			return b.held(TranscriptEntry{Kind: EntryUnread}, fmt.Sprintf("a tool's result of the harness's "+
				"record names its output or its error twice past %s, and was not read",
				humanSize(int64(b.valueMax))), ordinal), false, nil
		}
		return r.textEntry(a.texts[rank], e, ordinal), failed, nil
	}
	switch {
	case failure == chatString && (r.failure.past || len(r.failure.held) > 0):
		return pick(2, true)
	case failure == chatString:
		return b.held(e, `""`, ordinal), true, nil
	case failure == chatOther:
		return b.held(e, compactJSON(r.failedRaw), ordinal), true, nil
	case failure == chatOtherPast:
		return b.held(TranscriptEntry{Kind: EntryUnread}, fmt.Sprintf("a tool's failure of the harness's record "+
			"runs past %s, and was not read", humanSize(int64(b.valueMax))), ordinal), false, nil
	case output == chatString && (r.output.past || len(r.output.held) > 0):
		return pick(1, false)
	}
	return pick(0, false)
}

// chatAnswer is a response body's three texts (the body as written, its
// output and its error, by rank) as they are written: the first byte of one
// claims the answer, since a body's error with words, or else its output with
// words, is its answer, and a lower one is then held only within the value
// bound, let go of (lost) past it. So no two run past it.
type chatAnswer struct {
	texts         [3]*valueText
	lost, claimed [3]bool
}

// restart starts the text of rank again, a member written twice.
func (a *chatAnswer) restart(rank int) { a.lost[rank], a.claimed[rank] = false, false }

// writer writes the text of rank ([chatAnswer]).
func (a *chatAnswer) writer(rank int) io.Writer { return chatAnswerText{a: a, rank: rank} }

// lose lets go of the text of rank.
func (a *chatAnswer) lose(rank int) {
	a.lost[rank], a.claimed[rank], a.texts[rank].s = true, false, nil
	a.texts[rank].drop()
}

// chatAnswerText is one text of a [chatAnswer] as written.
type chatAnswerText struct {
	a    *chatAnswer
	rank int
}

func (c chatAnswerText) Write(p []byte) (int, error) {
	a, t := c.a, c.a.texts[c.rank]
	if a.lost[c.rank] || len(p) == 0 {
		return len(p), nil
	}
	higher := slices.Contains(a.claimed[c.rank+1:], true)
	switch {
	case !higher && !a.claimed[c.rank]:
		a.claimed[c.rank] = true
		for lower := range c.rank {
			if a.texts[lower].past {
				a.lose(lower)
			}
		}
	case higher && !t.past && len(t.held)+len(p) > t.b.valueMax:
		a.lose(c.rank)
		return len(p), nil
	}
	_, _ = t.Write(p)
	return len(p), nil
}

// partEnd shapes what the part just read makes into each reading: in the
// model's, its call or else its words; in a user record's, its response or
// else its words; as a summary, its text where it is the content's first a
// thought is not and holds words; and as a response's words, the content's
// one string (str).
func (r *chatReading) partEnd(sk *chatSinks, str bool) {
	b, p := r.b, &r.part
	words := p.text && !p.thought && !r.text.blank
	if s := sk.model; s != nil {
		switch {
		case p.call && p.callArgs:
			addCall(b, &r.args, p.callTool, p.callID, s)
		case p.call:
			addCall(b, nil, p.callTool, p.callID, s)
		case words:
			s.add(b, r.textAs(s, EntryModel), time.Time{})
		}
	}
	if s := sk.user; s != nil {
		switch {
		case p.result:
			a := p.answer
			if a.entry.Kind == EntryToolResult {
				a.entry.Failed = p.failed
			}
			s.addEvent(b, a, time.Time{}, claudeEvent{kind: eventResult, id: p.resultID, tool: p.resultAs})
		case !words, sk.own != "" && r.trim.opens(sk.own):
		case !r.text.past:
			b.proseInto(s, EntryBrief, r.text.text())
		default:
			// A PERSON'S WORDS PAST THE BOUND ARE ONE ENTRY (R1).
			s.add(b, r.textAs(s, EntryBrief), time.Time{})
		}
	}
	if (sk.summary != nil || sk.snap != nil) && p.text && !p.thought && !r.firstSeen {
		r.firstSeen = true
		if s := sk.summary; s != nil && !r.text.blank {
			s.add(b, r.textAs(s, EntrySummary), time.Time{})
		}
		if sk.snap != nil {
			sk.snap.heldFrom(&r.text)
		}
	}
	for _, s := range sk.words {
		if str && words {
			s.add(b, r.textAs(s, EntryModel), time.Time{})
		}
	}
	r.text.drop()
	r.args.drop()
}

// textAs is the part's text as an entry of kind at s's next ordinal: held,
// its stream's, ended once and of kind, or one only counted where s does not
// shape it.
func (r *chatReading) textAs(s *entryShape, kind EntryKind) pendingEntry {
	b, e := r.b, TranscriptEntry{Kind: kind}
	switch {
	case !s.shapes(b, kind) || r.text.past && r.text.s == nil:
		return b.held(e, "", s.next())
	case !r.text.past:
		return b.held(e, r.text.text(), s.next())
	}
	if !r.endedSet {
		r.ended, r.endedSet = r.text.s.end(s.next()), true
	}
	return withKind(r.ended, kind)
}

// chatStamp is a record's timestamp as written, zero where it does not read
// as RFC 3339 or no answer can carry it.
func chatStamp(word string) time.Time {
	at, err := time.Parse(time.RFC3339Nano, word)
	if err != nil {
		return time.Time{}
	}
	return carriedInstant(at)
}

// textTrim writes a text to w as strings.TrimSpace trims it, a piece of whole
// characters at a time: white space before its first other character is
// dropped, and a run of it after one is held until another follows, so the
// run that ends the text is never written. A RUN PAST runMax IS WRITTEN AS IT
// GROWS, so the trim holds no line: a text within the value bound (runMax) is
// trimmed exactly, and one past it keeps a run longer than the bound that
// ends it.
type textTrim struct {
	w      io.Writer
	runMax int
	state  trimState
	run    []byte
	// n is how many bytes the text holds untrimmed, head the first headN of
	// it trimmed.
	n     int
	head  [textDigestHeadBytes]byte
	headN int
}

// trimState is where a [textTrim] stands: before the text's first character
// other than white space, after one, inside a run it holds, or inside one it
// gave up holding.
type trimState uint8

const (
	trimLead trimState = iota
	trimText
	trimRun
	trimSpilled
)

// begin starts a text trimmed into w, holding a run to runMax.
func (t *textTrim) begin(w io.Writer, runMax int) *textTrim {
	*t = textTrim{w: w, runMax: runMax, run: lineScratch(t.run)}
	return t
}

func (t *textTrim) Write(p []byte) (int, error) {
	t.n += len(p)
	from := 0
	for i := 0; i < len(p); {
		size, space := spaceAt(p[i:])
		switch {
		case t.state == trimLead && space:
			from = i + size
		case t.state == trimLead, t.state == trimSpilled && !space:
			t.state = trimText
		case t.state == trimText && space:
			t.emit(p[from:i])
			t.run, t.state = append(t.run[:0], p[i:i+size]...), trimRun
		case t.state == trimRun && !space:
			t.emit(t.run)
			t.run, t.state, from = t.run[:0], trimText, i
		case t.state == trimRun:
			if t.run = append(t.run, p[i:i+size]...); len(t.run) > t.runMax {
				t.emit(t.run)
				t.run, t.state, from = t.run[:0], trimSpilled, i+size
			}
		}
		i += size
	}
	if t.state == trimText || t.state == trimSpilled {
		t.emit(p[from:])
	}
	return len(p), nil
}

// emit writes p, the trimmed text's next bytes.
func (t *textTrim) emit(p []byte) {
	if len(p) == 0 {
		return
	}
	t.headN += copy(t.head[t.headN:], p)
	_, _ = t.w.Write(p)
}

// opens reports whether the text trimmed opens with prefix, which holds no
// white space and is no longer than [textDigestHeadBytes].
func (t *textTrim) opens(prefix string) bool {
	return t.headN >= len(prefix) && string(t.head[:len(prefix)]) == prefix
}

// spaceAt is the size of the character p opens with, whole, and whether it
// is white space as strings.TrimSpace reads it.
func spaceAt(p []byte) (int, bool) {
	if c := p[0]; c < utf8.RuneSelf {
		return 1, c == ' ' || '\t' <= c && c <= '\r'
	}
	r, size := utf8.DecodeRune(p)
	return size, unicode.IsSpace(r)
}
