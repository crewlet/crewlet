package executor

import (
	"bytes"
	"context"
	"io"
	"slices"
	"strconv"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/jsonscan"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// WHAT GEMINI CLI COUNTS A CONTINUED SESSION AT, at its first request (0.70.0,
// measured): it compresses where the floor of its estimate of the history it
// restores (the records, a fresh session context, the synthetic answers a
// resume adds) is at least half the model's window, the brief not counted;
// and it masks tool output past the newest 50,000 estimated tokens, and
// collapses a history of 256 KiB, before it sends. The count is an upper bound
// of that estimate, read whole off the kept chat file ([geminiCounting]); the
// session context is the box's ([GeminiCLI.readBoxPoint]); a masking or a
// collapse the history may meet is [sandbox.CountMasked].

// geminiMaskProtect is the most tool output, in Gemini CLI's estimate, that no
// first request masks: 0.70.0 masks only where the newest outputs pass 50,000
// estimated tokens (strictly) and what lies past that point is at least
// 30,000, so the least output that masks is 50,001 (one part over the line).
// A count whose outputs reach it is marked, never bounded.
const geminiMaskProtect = 50_000

// geminiCollapseBytes is the size of history at which Gemini CLI 0.70.0
// collapses older tool output to previews at a resumed first request: the
// UTF-8 bytes of its text parts and answers, its session context included.
const geminiCollapseBytes = 256 << 10

// geminiCollapseKeptTurns is how many of the newest tool turns a collapse
// leaves whole (0.70.0, read and measured: six of nine answers collapsed).
const geminiCollapseKeptTurns = 3

// geminiContextAllowance is the most session context a box may hand a
// continuation, its tree and GEMINI.md files with it, that the count allows
// for beside the history's own bytes against [geminiCollapseBytes]; a box
// whose context is larger is no point ([sandbox.PointBoxSources]). 64 KiB is
// six times the 10,143 bytes measured for a workspace of 300 files.
const geminiContextAllowance = 64 << 10

// geminiCountIDs bounds the ids one count keeps: past 1<<16 records, answers
// and calls, the session is unread rather than held in memory, and the count
// keeps no id after the line that passed it ([geminiCounting.full]).
const geminiCountIDs = 1 << 16

// geminiCountBaseIDs is the most ids a count keeps at its base, holding no
// slot, and the most one line holds until it ends: past it the count holds
// the large slot to its end ([chatFileLines]), and a line holding more is
// unread, which no record Gemini CLI writes is. Both at it, at the walk's
// 256-byte word bound, hold 2.8 MiB live (BenchmarkAChatCountAtItsBounds'
// ids_base), within the meter's base ([meterBaseHeldBytes]); 2^12 held 5 MiB.
const geminiCountBaseIDs = 1 << 11

// geminiCountLargeHeldBytes is what the take's meter's count holds at
// [geminiCountIDs] ids at the walk's word bound, past its base under the large
// slot: 24.97 MiB live at most over six runs (BenchmarkAChatCountAtItsBounds'
// meter_ids_long, to the byte as each GC marks it), rounded up. A take counts
// it past the meter's base ([takeLargeHeldBytes]); its meter reads no summary.
const geminiCountLargeHeldBytes = 25 << 20

// geminiSyntheticAnswerBytes is the JSON of the answer a resume hands a call
// with none (213 bytes, 0.70.0). The turn it closes a history ending on a
// person's record with is the request's alone, never the estimate's (measured).
const geminiSyntheticAnswerBytes = 213

// geminiAnswer is one answer as the count reads it: its estimate in tokens,
// its response's JSON bytes, and the records that carry it.
type geminiAnswer struct {
	tokens, bytes int64
	carriers      []string
}

// geminiCall is one call as the count reads it: its name's length, and the
// records that carry it (a reply's toolCalls, or a compression's copy of it
// as a content part).
type geminiCall struct {
	name     int
	carriers []string
}

// geminiCounting is one walk's account of a Gemini CLI chat file for its
// count ([geminiCounting.line]): each record's largest copy, in quarter
// tokens and its text's UTF-8 bytes; each answer's largest; each call; the
// records removed; the order records came in and whether each is a
// compression's summary; what the walk could not read; and the line it reads.
// Each id is kept by its key ([recordWord.key]). sum, where not nil, reads the
// newest summary on the same walk ([geminiSummaryCapture]).
type geminiCounting struct {
	quarters, bytes map[string]int64
	answers         map[string]*geminiAnswer
	calls           map[string]*geminiCall
	removed         map[string]bool
	order           []string
	snapshot        map[string]bool
	compressions    geminiCompressionLines[string]
	marks           countMarks
	// full says the ids passed [geminiCountIDs]: the count is unread, and
	// keeps no more of them.
	full bool
	sum  *geminiSummaryCapture
	l    geminiCountLine
	seps lineSeparators
	head lineFormHead
}

func newGeminiCounting() *geminiCounting {
	return &geminiCounting{quarters: map[string]int64{}, bytes: map[string]int64{},
		answers: map[string]*geminiAnswer{}, calls: map[string]*geminiCall{}, removed: map[string]bool{},
		snapshot: map[string]bool{}}
}

// lineFormHead is a line as its forms judge it ([lineForm.holds]), taken a
// piece at a time as the walk reads it: from its first character that is no
// white space, as bytes.TrimSpace reads one, to its first [lineFormMaxBytes]
// bytes and one more (a character split there whole), which no whole form
// holds; past says a character that is no white space follows that, and
// carry is a character split across two pieces. So the walk judges a line as
// the whole-line count did, the line trimmed whole, white space about it of
// any length (codexLines), and holds no more of it.
type lineFormHead struct {
	held        []byte
	begun, past bool
	carry       [utf8.UTFMax]byte
	carried     int
}

// reset starts the next line, keeping the head's room.
func (h *lineFormHead) reset() { h.held, h.begun, h.past, h.carried = h.held[:0], false, false, 0 }

func (h *lineFormHead) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 && !h.past {
		if room := lineFormMaxBytes + 1 - len(h.held); h.begun && h.carried == 0 && (room > 0 || h.split()) {
			k := max(room, 1)
			h.held, p = append(h.held, p[:min(len(p), k)]...), p[min(len(p), k):]
			continue
		}
		// A CHARACTER WHOLE, before the line's first that is no white space or
		// after its head: one split across two pieces is held until it ends.
		var char [utf8.UTFMax]byte
		var c []byte
		switch {
		case h.carried == 0 && utf8.FullRune(p):
			_, size := utf8.DecodeRune(p)
			c, p = p[:size], p[size:]
		default:
			k := copy(h.carry[h.carried:], p)
			if !utf8.FullRune(h.carry[:h.carried+k]) {
				h.carried += k
				return n, nil
			}
			// A split character may prove no character, ending before p.
			_, size := utf8.DecodeRune(h.carry[:h.carried+k])
			c = char[:copy(char[:], h.carry[:size])]
			if size < h.carried {
				h.carried = copy(h.carry[:], h.carry[size:h.carried])
			} else {
				p, h.carried = p[size-h.carried:], 0
			}
		}
		switch r, _ := utf8.DecodeRune(c); {
		case h.begun && len(h.held) <= lineFormMaxBytes, !h.begun && !unicode.IsSpace(r):
			h.begun, h.held = true, append(h.held, c...)
		case h.begun && !unicode.IsSpace(r):
			h.past = true
		}
	}
	return n, nil
}

// split reports whether the head ends inside a character, which it takes
// whole.
func (h *lineFormHead) split() bool {
	i := len(h.held) - 1
	for i > 0 && len(h.held)-i < utf8.UTFMax && !utf8.RuneStart(h.held[i]) {
		i--
	}
	return i >= 0 && !utf8.FullRune(h.held[i:])
}

// form is the line as the whole-line count judged a form on it ([codexLines]),
// so far as the walk has read it: trimmed at both ends, and past
// [lineFormMaxBytes] its first bytes alone, which no whole form holds. A
// character the line ends inside is none of its white space.
func (h *lineFormHead) form() []byte {
	switch {
	case h.past, h.carried > 0 && h.begun:
		return h.held
	case h.carried > 0:
		return h.carry[:h.carried]
	}
	return bytes.TrimRightFunc(h.held, unicode.IsSpace)
}

// geminiCountLine is what the count reads of one line on the walk: whether
// it is a patch, its updates and removals held until the line ends
// ([geminiCounting.patch]), a `$set`, a `$rewindTo` or a session's header, its
// type, and the record it is where none of those ([geminiCountRecord]); twice
// says a member the count reads was written twice ([twice]), unread a part of
// a patch that does not read, or more of them than [geminiCountBaseIDs];
// shapeless a `$patch` of no shape at all; took that the count took the
// line's record.
type geminiCountLine struct {
	patch, set, rewind, session bool
	typeAbsent, entry, user     bool
	twice, unread, shapeless    bool
	took                        bool
	record                      geminiCountRecord
	updates                     []geminiCountRecord
	removes                     []string
	held                        int
}

// geminiCountRecord is one copy of a record as the count reads it, held until
// its line or its patch's update ends: its id; its quarters and its text's
// bytes; the marks its parts and answers set; and its answers and calls, each by its call's id, or its place
// where it has none, to at most [geminiCountBaseIDs] of them (past says more).
type geminiCountRecord struct {
	id              recordWord
	idOK            bool
	quarters, bytes int64
	past            bool
	marks           countMarks
	answers         []geminiCountAnswer
	calls           []geminiCountCall
}

// geminiCountPlace is a call's or an answer's key before its record's id is known:
// its own id's, or with none its place in its record ([geminiCounting.call]).
type geminiCountPlace struct {
	id    recordWord
	keyed bool
	at    int
}

// key is the place's key in a record of key id.
func (p geminiCountPlace) key(id string) string {
	if p.keyed {
		return p.id.key()
	}
	return "\x00" + id + "\x00" + strconv.Itoa(p.at)
}

// geminiCountAnswer is one answer as a record's read holds it: its estimate
// in tokens and its response's JSON bytes.
type geminiCountAnswer struct {
	place         geminiCountPlace
	tokens, bytes int64
}

// geminiCountCall is one call as a record's read holds it: its name's length.
type geminiCountCall struct {
	place geminiCountPlace
	name  int
}

// reset starts a record's read, keeping its lists' room.
func (r *geminiCountRecord) reset() {
	*r = geminiCountRecord{answers: r.answers[:0], calls: r.calls[:0]}
}

// answer holds one answer, past the bound marking the record past.
func (r *geminiCountRecord) answer(a geminiCountAnswer) {
	if len(r.answers)+len(r.calls) >= geminiCountBaseIDs {
		r.past = true
		return
	}
	r.answers = append(r.answers, a)
}

// call holds one call, past the bound marking the record past.
func (r *geminiCountRecord) call(c geminiCountCall) {
	if len(r.answers)+len(r.calls) >= geminiCountBaseIDs {
		r.past = true
		return
	}
	r.calls = append(r.calls, c)
}

// pastBase reports whether the count keeps more ids than [geminiCountBaseIDs].
func (c *geminiCounting) pastBase() bool {
	return len(c.quarters)+len(c.answers)+len(c.calls)+len(c.removed) > geminiCountBaseIDs
}

// tee is the trim, the line's separators ([lineSeparators]) and its head, for
// its compression's forms ([geminiCompressionLines]).
func (c *geminiCounting) tee(trim *lineTrim) io.Writer {
	return teeTwo{trim, teeTwo{&c.seps, &c.head}}
}

// start starts the next line.
func (c *geminiCounting) start() {
	record, updates, removes := c.l.record, c.l.updates[:0], c.l.removes[:0]
	c.l = geminiCountLine{record: record, updates: updates, removes: removes}
	c.l.record.reset()
	c.seps.reset()
	c.head.reset()
	if c.sum != nil {
		c.sum.start(len(c.order))
	}
}

// hold counts n more of a patch's parts the line holds, past
// [geminiCountBaseIDs] leaving it unread, and reports whether it may hold them.
func (l *geminiCountLine) hold(n int) bool {
	l.held += n
	l.unread = l.unread || l.held > geminiCountBaseIDs
	return !l.unread
}

// The members a line's reader reads, a bit each for [twice].
const (
	geminiCountsID uint16 = 1 << iota
	geminiCountsType
	geminiCountsContent
	geminiCountsThoughts
	geminiCountsToolCalls
	geminiCountsPatch
	geminiCountsSet
	geminiCountsRewind
	geminiCountsSession
)

// record reads the line's members, holding its patch or its record until
// its line ends.
func (c *geminiCounting) record(w *lineWalk) error {
	l := &c.l
	l.typeAbsent = true
	var seen uint16
	return w.members(func(name string, k jsonscan.Kind) error {
		member := func(bit uint16) { l.twice = twice(&seen, bit) || l.twice }
		switch name {
		case "type":
			member(geminiCountsType)
			kind, took, err := w.wordField(k)
			l.typeAbsent, l.entry = k == jsonscan.Null, took && (kind == "user" || kind == "gemini")
			l.user = took && kind == "user"
			return err
		case "$patch":
			member(geminiCountsPatch)
			l.shapeless = l.shapeless || k != jsonscan.ObjectStart
			if l.patch = k != jsonscan.Null; l.patch {
				return c.patch(w, k)
			}
			return nil
		case "$set", "$rewindTo", "sessionId":
			bit, present := geminiCountsSet, &l.set
			switch name {
			case "$rewindTo":
				bit, present = geminiCountsRewind, &l.rewind
			case "sessionId":
				bit, present = geminiCountsSession, &l.session
			}
			member(bit)
			*present = k != jsonscan.Null
			if name == "$rewindTo" && c.sum != nil {
				return c.sum.rewindMember(w, k)
			}
			return w.skip(k)
		}
		handled, err := c.recordMember(w, name, k, &l.record, &seen, &l.twice)
		if !handled {
			err = w.skip(k)
		}
		return err
	})
}

// recordMember reads one member of a record into r where it is one the
// count reads, and reports whether it was.
func (c *geminiCounting) recordMember(w *lineWalk, name string, k jsonscan.Kind, r *geminiCountRecord,
	seen *uint16, again *bool) (bool, error) {

	var err error
	switch name {
	case "id":
		*again = twice(seen, geminiCountsID) || *again
		var ok bool
		r.id, ok, err = w.idField(k)
		r.idOK = ok && k == jsonscan.String && !r.id.empty()
	case "content":
		*again = twice(seen, geminiCountsContent) || *again
		err = w.geminiContent(k, r, c.summaryWords(r), again)
	case "thoughts":
		*again = twice(seen, geminiCountsThoughts) || *again
		err = w.geminiThoughts(k, r, again)
	case "toolCalls":
		*again = twice(seen, geminiCountsToolCalls) || *again
		err = w.geminiToolCalls(k, r, again)
	default:
		return false, nil
	}
	return true, err
}

// line takes one line ([geminiCounting.read]), and a user entry is a
// compression's summary by its lines ([geminiCompressionLines]), known a line
// late, as the whole-line count knew it, by the line's head ([lineFormHead]).
func (c *geminiCounting) line(object bool) {
	user := c.read(object)
	form := c.head.form()
	summary, ok := c.compressions.line(form, user, user != "")
	if ok {
		c.snapshot[summary] = true
	}
	if c.sum != nil {
		c.sum.line(c, object, form, summary, ok)
	}
}

// read takes one line: the header, a `$set` or a `$rewindTo` (which only
// over-counts what it rewinds) are passed over; a `$patch` was taken as it
// streamed: its updates as copies of their records, its removals, and one
// that orders the records (`orderIds`, whose effect on the history was not
// measured) unread; an entry is a copy of its record. Anything else is
// unread, and so is a line holding a separator ([lineSeparators]). It is the
// key of the user entry the line is, "" for none.
func (c *geminiCounting) read(object bool) string {
	l := &c.l
	user := ""
	switch {
	case c.seps.found, !object, l.twice, l.unread:
		c.marks = c.marks.with(sandbox.CountUnread)
		return ""
	case l.patch:
		for i := range l.updates {
			c.take(&l.updates[i])
		}
		for _, id := range l.removes {
			if !c.full {
				c.removed[id] = true
			}
		}
	case l.set, l.rewind, l.session && l.typeAbsent:
	case !l.entry:
		c.marks = c.marks.with(sandbox.CountUnread)
		return ""
	default:
		l.took = c.take(&l.record)
		if l.user && l.record.idOK {
			user = l.record.id.key()
		}
	}
	if len(c.quarters)+len(c.answers)+len(c.calls)+len(c.removed) > geminiCountIDs {
		c.marks = c.marks.with(sandbox.CountUnread)
		c.full = true
	}
	return user
}

// take takes one copy of a record, none once the count is full, and reports
// whether it took it.
func (c *geminiCounting) take(r *geminiCountRecord) bool {
	if !r.idOK || r.past {
		c.marks = c.marks.with(sandbox.CountUnread)
		return false
	}
	if c.full {
		return false
	}
	id := r.id.key()
	if _, seen := c.quarters[id]; !seen {
		c.order = append(c.order, id)
	}
	c.marks |= r.marks
	for _, a := range r.answers {
		key := a.place.key(id)
		got := c.answers[key]
		if got == nil {
			got = &geminiAnswer{}
			c.answers[key] = got
		}
		got.tokens, got.bytes = max(got.tokens, a.tokens), max(got.bytes, a.bytes)
		got.carriers = geminiCarried(got.carriers, id)
	}
	for _, call := range r.calls {
		key := call.place.key(id)
		got := c.calls[key]
		if got == nil {
			got = &geminiCall{}
			c.calls[key] = got
		}
		got.name = max(got.name, call.name)
		got.carriers = geminiCarried(got.carriers, id)
	}
	c.quarters[id] = max(c.quarters[id], r.quarters)
	c.bytes[id] = max(c.bytes[id], r.bytes)
	return true
}

// geminiCarried is carriers with id, the newest, where it is not already the
// newest: only whether a carrier is kept is read ([geminiCounting.count]), so
// one record copied over and over holds one place.
func geminiCarried(carriers []string, id string) []string {
	if n := len(carriers); n == 0 || carriers[n-1] != id {
		return append(carriers, id)
	}
	return carriers
}

// patch reads a `$patch`, the value k begins: each update, a copy of its
// record, and each removal, held until the line ends, as A TORN LAST LINE IS
// NEVER READ; a patch that does not decode, or orders the records, is unread.
func (c *geminiCounting) patch(w *lineWalk, k jsonscan.Kind) error {
	l := &c.l
	if k != jsonscan.ObjectStart {
		l.unread, l.shapeless = true, true
		return w.skip(k)
	}
	var seen uint16
	return w.members(func(name string, k jsonscan.Kind) error {
		switch name {
		case "updates":
			l.twice = twice(&seen, 1) || l.twice
			ok, err := w.listField(k, func(k jsonscan.Kind) error {
				if k != jsonscan.ObjectStart {
					l.unread, l.shapeless = true, true
					return w.skip(k)
				}
				if len(l.updates) < cap(l.updates) {
					l.updates = l.updates[:len(l.updates)+1]
				} else {
					l.updates = append(l.updates, geminiCountRecord{})
				}
				r := &l.updates[len(l.updates)-1]
				r.reset()
				if c.sum != nil {
					c.sum.updateStart()
				}
				var fields uint16
				err := w.members(func(name string, k jsonscan.Kind) error {
					if name == "type" && c.sum != nil {
						return c.sum.updateType(w, k)
					}
					handled, err := c.recordMember(w, name, k, r, &fields, &l.twice)
					if !handled {
						err = w.skip(k)
					}
					return err
				})
				if c.sum != nil {
					c.sum.updated(c, r)
				}
				if !l.hold(1 + len(r.answers) + len(r.calls)) {
					l.updates = l.updates[:len(l.updates)-1]
				}
				return err
			})
			l.unread, l.shapeless = l.unread || !ok, l.shapeless || !ok
			return err
		case "removeIds":
			l.twice = twice(&seen, 2) || l.twice
			ok, err := w.listField(k, func(k jsonscan.Kind) error {
				id, ok, err := w.idField(k)
				l.unread = l.unread || !ok
				if ok && c.sum != nil {
					c.sum.removal(c, id)
				}
				if ok && l.hold(1) {
					l.removes = append(l.removes, id.key())
				}
				return err
			})
			l.unread, l.shapeless = l.unread || !ok, l.shapeless || !ok
			return err
		case "orderIds":
			l.twice = twice(&seen, 4) || l.twice
			l.unread = l.unread || k != jsonscan.Null
			l.shapeless = l.shapeless || k != jsonscan.Null
		}
		return w.skip(k)
	})
}

// geminiContent reads a record's content, the value k begins, into r: a text
// or its parts (an object one part), its answers kept apart and its calls
// kept, and its texts' UTF-8 bytes; and its words into words where not nil, a
// string or a list's parts alone, as the transcript reads them
// ([chatReading.parts]).
func (w *lineWalk) geminiContent(k jsonscan.Kind, r *geminiCountRecord, words *geminiWords, again *bool) error {
	words.start()
	switch k {
	case jsonscan.Null:
		return nil
	case jsonscan.String:
		t, err := w.chatTextTo(false, words.partText())
		r.quarters, r.bytes = r.quarters+t.estimate(), r.bytes+t.bytes
		words.partEnd(words.untrimmed() > 0, false)
		return err
	case jsonscan.ObjectStart:
		return w.geminiPart(k, 0, r, nil, again)
	case jsonscan.ArrayStart:
		i := 0
		return w.elements(func(k jsonscan.Kind) (bool, error) {
			err := w.geminiPart(k, i, r, words, again)
			i++
			return true, err
		})
	}
	r.marks = r.marks.with(sandbox.CountUnread)
	return w.skip(k)
}

// The members of a part [lineWalk.geminiPart] reads, a bit each for [twice].
const (
	geminiCountPartText uint16 = 1 << iota
	geminiCountPartAnswer
	geminiCountPartCall
	geminiCountPartInline
	geminiCountPartFile
)

// geminiPart reads the i'th part of a record's content, the value k begins,
// into r: a text, an answer, media, or anything else at its JSON's length,
// its call kept; and its text into words where not nil ([geminiWords]).
func (w *lineWalk) geminiPart(k jsonscan.Kind, i int, r *geminiCountRecord, words *geminiWords,
	again *bool) error {
	if k != jsonscan.ObjectStart {
		r.marks = r.marks.with(sandbox.CountUnread)
		return w.skip(k)
	}
	var (
		seen                   uint16
		text                   chatText
		textOK, textOn         bool
		thought                bool
		answer                 geminiCountAnswer
		answerMarks            countMarks
		answerOn, inline, file bool
		callOn, callOK         bool
		call                   geminiCountCall
	)
	n, err := w.jsMembers(func(name string, k jsonscan.Kind) (int64, error) {
		null := k == jsonscan.Null
		switch name {
		case "text":
			*again = twice(&seen, geminiCountPartText) || *again
			if textOn, textOK = !null, k == jsonscan.String; !textOK {
				return 4, w.skip(k)
			}
			var err error
			text, err = w.chatTextTo(false, words.partText())
			return 0, err
		case "thought":
			thought = k == jsonscan.True
		case "functionResponse":
			*again = twice(&seen, geminiCountPartAnswer) || *again
			if answerOn = !null; null {
				return 4, nil
			}
			var err error
			answer, answerMarks, err = w.geminiAnswer(k, geminiCountPlace{at: i}, again)
			return 0, err
		case "inlineData", "fileData":
			bit, on := geminiCountPartInline, &inline
			if name == "fileData" {
				bit, on = geminiCountPartFile, &file
			}
			*again = twice(&seen, bit) || *again
			*on = !null
			if null {
				return 4, nil
			}
			if name == "inlineData" {
				return 0, w.skipInline(k)
			}
			return 0, w.skip(k)
		case "functionCall":
			*again = twice(&seen, geminiCountPartCall) || *again
			callOn, callOK, call = !null, k == jsonscan.ObjectStart, geminiCountCall{place: geminiCountPlace{at: i}}
			return w.geminiCall(k, &call, again)
		}
		return w.jsValue(k)
	})
	if err == nil {
		words.partEnd(textOK, thought)
	}
	switch {
	case err != nil:
		return err
	case textOn && !textOK:
		r.marks = r.marks.with(sandbox.CountUnread)
	case textOn:
		r.quarters += text.estimate()
		r.bytes += text.bytes
	case answerOn:
		r.marks |= answerMarks
		if answerMarks&countMarks(0).with(sandbox.CountUnread) == 0 {
			r.answer(answer)
		}
	case inline || file:
		r.marks = r.marks.with(sandbox.CountMedia)
	default:
		if callOn {
			if !callOK {
				r.marks = r.marks.with(sandbox.CountUnread)
			}
			r.call(call)
		}
		r.quarters += n
	}
	return nil
}

// geminiCall reads a call, the value k begins, into call: its id and its
// name's length where it is an object; and is its JSON's length.
func (w *lineWalk) geminiCall(k jsonscan.Kind, call *geminiCountCall, again *bool) (int64, error) {
	if k != jsonscan.ObjectStart {
		return w.jsValue(k)
	}
	var seen uint16
	return w.jsMembers(func(name string, k jsonscan.Kind) (int64, error) {
		switch name {
		case "id":
			*again = twice(&seen, 1) || *again
			n, id, err := w.geminiID(k)
			call.place.id, call.place.keyed = id, !id.empty()
			return n, err
		case "name":
			*again = twice(&seen, 2) || *again
			n, text, err := w.geminiName(k)
			call.name = int(text)
			return n, err
		}
		return w.jsValue(k)
	})
}

// geminiID reads an id, the value k begins, and is its JSON's length and the
// word it is where a string, else the empty word: a key falls back to its
// place where it is empty ([geminiCountPlace]).
func (w *lineWalk) geminiID(k jsonscan.Kind) (int64, recordWord, error) {
	if k != jsonscan.String {
		n, err := w.jsValue(k)
		return n, recordWord{}, err
	}
	t, err := w.chatText(true)
	return t.js, t.word, err
}

// geminiName reads a name, the value k begins, and is its JSON's length and
// its text's bytes, none where it is no string.
func (w *lineWalk) geminiName(k jsonscan.Kind) (int64, int64, error) {
	if k != jsonscan.String {
		n, err := w.jsValue(k)
		return n, 0, err
	}
	t, err := w.chatText(false)
	return t.js, t.bytes, err
}

// geminiAnswer reads an answer, the value k begins: its estimate,
// `(name + JSON(response)) / 4` rounded up, under its call's id (or, with
// none, its place), and the marks it sets: unread where it is no object,
// media where it carries parts.
func (w *lineWalk) geminiAnswer(k jsonscan.Kind, at geminiCountPlace, again *bool) (geminiCountAnswer,
	countMarks, error) {

	var marks countMarks
	if k != jsonscan.ObjectStart {
		return geminiCountAnswer{}, marks.with(sandbox.CountUnread), w.skip(k)
	}
	a := geminiCountAnswer{place: at}
	var name int64
	response := int64(4)
	var seen uint16
	err := w.members(func(member string, k jsonscan.Kind) error {
		var err error
		switch member {
		case "id":
			*again = twice(&seen, 1) || *again
			var id recordWord
			_, id, err = w.geminiID(k)
			a.place.id, a.place.keyed = id, !id.empty()
		case "name":
			*again = twice(&seen, 2) || *again
			_, name, err = w.geminiName(k)
		case "response":
			*again = twice(&seen, 4) || *again
			response = 4
			if k != jsonscan.Null {
				response, err = w.jsValue(k)
			}
		case "parts":
			*again = twice(&seen, 8) || *again
			if k != jsonscan.Null {
				// A TOOL'S MEDIA rides beside its response, estimated at no length.
				marks = marks.with(sandbox.CountMedia)
			}
			err = w.skip(k)
		default:
			err = w.skip(k)
		}
		return err
	})
	a.tokens, a.bytes = (name+response+3)/4, response
	return a, marks, err
}

// geminiThoughts reads a reply's thoughts, the value k begins, into r: each
// counted as the text `**subject** description` the history holds, though
// never sent, and its UTF-8 bytes.
func (w *lineWalk) geminiThoughts(k jsonscan.Kind, r *geminiCountRecord, again *bool) error {
	var q, n int64
	ok, err := w.listField(k, func(k jsonscan.Kind) error {
		var subject, description chatText
		var seen uint16
		took, err := w.objectField(k, func(name string, k jsonscan.Kind) error {
			var t *chatText
			switch name {
			case "subject":
				*again = twice(&seen, 1) || *again
				t = &subject
			case "description":
				*again = twice(&seen, 2) || *again
				t = &description
			default:
				return w.skip(k)
			}
			*t = chatText{}
			switch k {
			case jsonscan.Null:
				return nil
			case jsonscan.String:
				var err error
				*t, err = w.chatText(false)
				return err
			}
			r.marks = r.marks.with(sandbox.CountUnread)
			return w.skip(k)
		})
		if !took {
			r.marks = r.marks.with(sandbox.CountUnread)
		}
		text := description
		if subject.bytes > 0 {
			// `**` + subject + `** ` + description: the marker holds no `*`
			// and no space, so it is in the whole only where in a part.
			text.units += subject.units + 5
			text.quarters += subject.quarters + 5
			text.bytes += subject.bytes + 5
			text.marked = text.marked || subject.marked
		}
		q, n = q+text.estimate(), n+text.bytes
		return err
	})
	if !ok {
		r.marks = r.marks.with(sandbox.CountUnread)
	}
	r.quarters, r.bytes = r.quarters+q, r.bytes+n
	return err
}

// geminiToolCalls reads a reply's calls, the value k begins, into r: each as
// the JSON of `{functionCall:{id,name,args}}` at a token a quarter, its id
// counted; each call is kept and each result read as an answer.
func (w *lineWalk) geminiToolCalls(k jsonscan.Kind, r *geminiCountRecord, again *bool) error {
	i := 0
	ok, err := w.listField(k, func(k jsonscan.Kind) error {
		defer func() { i++ }()
		var seen uint16
		var id, name, args int64 = 4, 4, 4
		var key recordWord
		var nameBytes int64
		named := false
		took, err := w.objectField(k, func(member string, k jsonscan.Kind) error {
			var err error
			switch member {
			case "id":
				*again = twice(&seen, 1) || *again
				id, key, err = w.geminiID(k)
			case "name":
				*again = twice(&seen, 2) || *again
				named = k != jsonscan.Null
				name, nameBytes, err = w.geminiName(k)
			case "args":
				*again = twice(&seen, 4) || *again
				args, err = w.jsValue(k)
			case "result":
				*again = twice(&seen, 8) || *again
				j := 0
				listed, listErr := w.listField(k, func(k jsonscan.Kind) error {
					defer func() { j++ }()
					return w.geminiResult(k, geminiCountPlace{at: 1000*(i+1) + j}, r, again)
				})
				if !listed {
					r.marks = r.marks.with(sandbox.CountUnread)
				}
				return listErr
			default:
				err = w.skip(k)
			}
			return err
		})
		if !took {
			r.marks = r.marks.with(sandbox.CountUnread)
		}
		if named {
			r.call(geminiCountCall{place: geminiCountPlace{id: key, keyed: !key.empty(), at: -(i + 1)}, name: int(nameBytes)})
			r.quarters += int64(len(`{"functionCall":{"id":,"name":,"args":}}`)) + id + name + args
		}
		return err
	})
	if !ok {
		r.marks = r.marks.with(sandbox.CountUnread)
	}
	return err
}

// geminiResult reads one part of a call's result, the value k begins: its
// answer, where it is an object holding one, into r.
func (w *lineWalk) geminiResult(k jsonscan.Kind, at geminiCountPlace, r *geminiCountRecord, again *bool) error {
	if k != jsonscan.ObjectStart {
		return w.skip(k)
	}
	var seen uint16
	return w.members(func(name string, k jsonscan.Kind) error {
		if name != "functionResponse" {
			return w.skipMember(name, k)
		}
		*again = twice(&seen, 1) || *again
		if k == jsonscan.Null {
			return nil
		}
		a, marks, err := w.geminiAnswer(k, at, again)
		r.marks |= marks
		if marks&countMarks(0).with(sandbox.CountUnread) == 0 {
			r.answer(a)
		}
		return err
	})
}

// count is the count c read, under marks the read set before the walk, with
// taken bytes put back ([chatCounter]): the bound, rounded up;
// [sandbox.CountMasked] where the answers reach [geminiMaskProtect] or the
// history's bytes as 0.70.0 sizes it for its collapse (its texts' UTF-8 and
// its answers' JSON), with the context allowed, reach [geminiCollapseBytes];
// restarted at the newest summary kept. Only
// what a kept record carries is restored, so a kept call whose every answer
// sits in a removed record gets the synthetic answer a resume hands it.
func (c *geminiCounting) count(marks countMarks, taken int64) *sandbox.ResumedCount {
	marks |= c.marks
	var quarters, answers, view, responses int64
	restart := ""
	for _, id := range c.order {
		if c.removed[id] {
			continue
		}
		quarters += c.quarters[id]
		view += c.bytes[id]
		if c.snapshot[id] {
			restart = id
		}
	}
	kept := func(carriers []string) bool {
		return slices.ContainsFunc(carriers, func(id string) bool { return !c.removed[id] })
	}
	for _, a := range c.answers {
		if kept(a.carriers) {
			answers, view, responses = answers+a.tokens, view+a.bytes, responses+1
		}
	}
	for callID, call := range c.calls {
		if a := c.answers[callID]; kept(call.carriers) && (a == nil || !kept(a.carriers)) {
			answers += (int64(call.name) + geminiSyntheticAnswerBytes + 3) / 4
			view, responses = view+geminiSyntheticAnswerBytes, responses+1
		}
	}
	// WHAT THE SCRUB TOOK OUT may have been any answer's or text's: a token
	// a byte on the bound, its bytes on the history, a quarter on the answers.
	quarters += 4 * (answers + taken)
	view += taken
	// A COLLAPSE PREVIEWS ONLY ANSWERS older than the newest three tool turns,
	// so a history of three answers or fewer has none it changes.
	collapses := view+geminiContextAllowance >= geminiCollapseBytes && responses > geminiCollapseKeptTurns
	if answers+(taken+3)/4 >= geminiMaskProtect || collapses {
		marks = marks.with(sandbox.CountMasked)
	}
	// A RESTART NO COUNT CARRIES (past [sandbox.RestartIDMaxBytes]) leaves the
	// count unread as it is read, as a record would hold it
	// ([sandbox.ResumedCount]), never a bound beside a restart cut short.
	if len(restart) > sandbox.RestartIDMaxBytes {
		marks = marks.with(sandbox.CountUnread)
	}
	out := &sandbox.ResumedCount{Restart: &restart}
	switch mark := marks.first(); mark {
	case "":
		out.Tokens = (quarters + 3) / 4
	case sandbox.CountUnread, sandbox.CountRelease:
		out.Mark, out.Restart = mark, nil
	default:
		out.Mark = mark
	}
	return out
}

// meter reads a Gemini CLI archive for the take's meter ([carrier.meter]):
// the count of its one chat file, by the rules of [geminiRelease] alone, its
// bound no estimate of the next request (the launch's floor reads none).
func (GeminiCLI) meter(ctx context.Context, r io.Reader, _, release string, large *passBudget) (meterReading,
	error) {
	return chatMeter(ctx, newGeminiCounting(), r, geminiChatMember, release, geminiRelease, large)
}

// sessionTraits is how a relaunch judges a Gemini CLI session: in the box the
// launch gets, since a box's settings can move its point ([geminiPoint]);
// calls keyed by their place in the log, since its reading reads by content
// (geminiholdings.go); a brief at a byte a token, above the 0.75 a byte
// Gemini CLI's estimate reaches at most.
func (GeminiCLI) sessionTraits() sandbox.SessionTraits {
	return sandbox.SessionTraits{FirstRequest: sandbox.FirstRequestBox, CallKeying: sandbox.CallKeyingByLogPlace,
		Charge: 1}
}
