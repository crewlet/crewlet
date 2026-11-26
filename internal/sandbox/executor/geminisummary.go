package executor

import (
	"errors"
	"io"
	"slices"

	"github.com/crewlet/crewlet/internal/jsonscan"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// geminiSummaryCapture reads a Gemini CLI chat file's newest summary on its
// count's walk ([geminiCounting.summarize]) and keeps no id of its own: a
// record's place is where the count first took it ([geminiCounting.order]),
// and one the count holds removed, or a rewind took away, stays gone. It
// replays the summaries a compression's lines find ([geminiCompressionLines]),
// a patch's copy of one still one, a patch's removals and each `$rewindTo`:
// the newest is the latest snapshot standing at the file's end, the
// snapshots' words held to [summaryReadMaxBytes] in all, the earliest let go
// first. A summary on a line the count could not take, or whose record was
// gone (which Gemini CLI never writes), stands unsaid; a patch of no shape or
// a rewind to no record standing leaves every snapshot before it unsaid.
type geminiSummaryCapture struct {
	snaps []geminiSnap
	// held is the snapshots' words, which counted counts in the read's claim.
	held, counted int
	// first is the place of the first snapshot met, unknown where the replay
	// could not follow the file and why, -1 for neither ([geminiSnap.place]).
	first, unknown int
	why            string
	// gone is each span of records a rewind took away, by the count's order,
	// from and to, in order with a record standing between every two.
	gone [][2]int
	read summaryRead
	// pending is the summary the line before opened as, until this line says
	// whether it was one; refused says the count did not take it, at the
	// place pendingAt.
	pending            geminiSnapCopy
	pendingOn, refused bool
	pendingAt          int
	// The line read: the count's order before it; whether its record opens as
	// a summary would and its words; its rewind; and the copies of a summary
	// its patch updates, an update's words and type, and the summaries it removes.
	lineStart       int
	opened, rewound bool
	rewind          recordWord
	record, update  geminiWords
	updateUser      bool
	updates         []geminiSnapCopy
	removes         []string
}

// geminiSnap is a standing record that is a snapshot: its key, place and
// words, or why they cannot be said. A place is twice the record's in the
// count's order, and one; a summary the count holds no place for is placed
// at twice the place of the record after it, between the two.
type geminiSnap struct {
	id     string
	place  int
	text   string
	unsaid string
}

// geminiSnapCopy is one copy of a summary a line holds until it ends: its
// record's key, whether the copy is a user entry, and its words.
type geminiSnapCopy struct {
	key   string
	user  bool
	words summaryText
}

// errGeminiCountFull is a chat file past the ids a count keeps
// ([geminiCounting.full]), whose summary the walk cannot replay past them.
var errGeminiCountFull = errors.New("the chat file holds more records than a read holds")

// summarize has the count read the newest summary on its walk too
// ([geminiCounting.summary]), its words past [summaryBaseBytes] taking the
// read's large slot through claim ([summaryClaim]).
func (c *geminiCounting) summarize(claim *summaryClaim) {
	c.sum = &geminiSummaryCapture{first: -1, unknown: -1}
	c.sum.read.claim, c.sum.record.claim, c.sum.update.claim = claim, claim, claim
}

// summary is the newest summary the walk read, the walk ended with err:
// unread where it did not read the file to its end, or past the ids the count
// keeps, which the replay cannot follow past.
func (c *geminiCounting) summary(err error) sandbox.ThreadSummary {
	if err == nil && c.full {
		err = errGeminiCountFull
	}
	if err != nil {
		c.sum.read.fail("the session's chat file was not read to its end: " + err.Error())
	}
	return c.sum.reading()
}

// summaryWords is where the content of r, a record the line holds, is read
// for its words: the line's own where it opens as a compression's summary
// would, after its sync's `$set` ([geminiCompressionUser]), by the line's head
// as its content begins; a patch's update's unless it names a record that is
// no summary; nil where none is read.
func (c *geminiCounting) summaryWords(r *geminiCountRecord) *geminiWords {
	s := c.sum
	switch {
	case s == nil:
		return nil
	case r == &c.l.record:
		if s.opened = c.compressions.set && geminiCompressionUser.holds(c.head.form()); !s.opened {
			return nil
		}
		return &s.record
	case r.id.long || r.idOK && !c.snapshot[r.id.key()]:
		return nil
	}
	return &s.update
}

// start starts the next line, the count's order at order records.
func (s *geminiSummaryCapture) start(order int) {
	s.lineStart, s.opened, s.rewound, s.rewind = order, false, false, recordWord{}
	s.updates, s.removes = s.updates[:0], s.removes[:0]
}

// rewindMember reads a `$rewindTo`, the value k begins: the record it names,
// none where it is no string.
func (s *geminiSummaryCapture) rewindMember(w *lineWalk, k jsonscan.Kind) error {
	id, ok, err := w.idField(k)
	if !ok {
		id = recordWord{}
	}
	s.rewound, s.rewind = true, id
	return err
}

// updateStart starts a patch's update.
func (s *geminiSummaryCapture) updateStart() {
	s.update.start()
	s.updateUser = false
}

// updateType reads an update's type, the value k begins.
func (s *geminiSummaryCapture) updateType(w *lineWalk, k jsonscan.Kind) error {
	kind, took, err := w.wordField(k)
	s.updateUser = took && kind == "user"
	return err
}

// updated holds the update r just read where it copies a summary.
func (s *geminiSummaryCapture) updated(c *geminiCounting, r *geminiCountRecord) {
	if !r.idOK || r.id.long || !c.snapshot[r.id.key()] {
		return
	}
	s.updates = append(s.updates, geminiSnapCopy{key: r.id.key(), user: s.updateUser})
	u := &s.updates[len(s.updates)-1]
	u.words, s.update.text = s.update.text, u.words
}

// removal holds a patch's removal of id where it names a summary.
func (s *geminiSummaryCapture) removal(c *geminiCounting, id recordWord) {
	if id.long {
		return
	}
	key := id.key()
	if c.snapshot[key] || slices.ContainsFunc(s.snaps, func(n geminiSnap) bool { return n.id == key }) {
		s.removes = append(s.removes, key)
	}
}

// line takes the line just read ([geminiCounting.line]), form its head: first
// the summary the line before opened as, one where the count found it one
// (summary, its key prior); then the line's patch, rewind or copy of a record.
// A line that is no object holds none of them.
func (s *geminiSummaryCapture) line(c *geminiCounting, object bool, form []byte, prior string, summary bool) {
	if s.pendingOn {
		s.pendingOn = false
		switch {
		case !s.pending.words.set:
		case s.refused && geminiCompressionAck.holds(form):
			s.unsaid(s.pending.key, s.pendingAt, "a compression's summary is on a line the count could not read")
		case !s.refused && summary && prior == s.pending.key:
			s.snapshot(c, prior, &s.pending.words)
		}
	}
	l := &c.l
	switch {
	case !object:
	case l.shapeless:
		s.lost("a patch of the chat file is of no shape this read follows")
	case l.patch:
		for i := range s.updates {
			u := &s.updates[i]
			s.drop(func(n geminiSnap) bool { return n.id == u.key })
			if u.user {
				s.snapshot(c, u.key, &u.words)
			}
		}
		for _, key := range s.removes {
			s.drop(func(n geminiSnap) bool { return n.id == key })
		}
	case s.rewound:
		s.rewindTo(c)
	case l.record.idOK && !l.record.id.long:
		key := l.record.id.key()
		s.drop(func(n geminiSnap) bool { return n.id == key })
		if s.opened && (l.user || !l.took) {
			s.pending.key, s.pendingOn, s.refused, s.pendingAt = key, true, !l.took, 2*s.lineStart
			s.pending.words, s.record.text = s.record.text, s.pending.words
		}
	}
}

// standing is where the record key stands in the count's order, and whether
// it stands: taken, never removed, and in no span a rewind took away.
func (s *geminiSummaryCapture) standing(c *geminiCounting, key string) (int, bool) {
	if c.removed[key] {
		return -1, false
	}
	at := len(c.order) - 1
	for at >= 0 && c.order[at] != key {
		at--
	}
	gone := slices.ContainsFunc(s.gone, func(span [2]int) bool { return span[0] <= at && at < span[1] })
	return at, at >= 0 && !gone
}

// rewindTo is the line's `$rewindTo`: its record and every later one gone.
func (s *geminiSummaryCapture) rewindTo(c *geminiCounting) {
	at, ok := -1, false
	if !s.rewind.long && !s.rewind.empty() {
		at, ok = s.standing(c, s.rewind.key())
	}
	if !ok {
		s.lost("the chat file rewinds to a record its history does not hold")
		return
	}
	s.drop(func(n geminiSnap) bool { return n.place > 2*at })
	s.gone = slices.DeleteFunc(s.gone, func(span [2]int) bool { return span[0] > at })
	if n := len(s.gone); n > 0 && s.gone[n-1][1] == at {
		s.gone[n-1][1] = len(c.order)
		return
	}
	s.gone = append(s.gone, [2]int{at, len(c.order)})
}

// snapshot makes words, where a text was read, the snapshot of the record
// key, unsaid where the record was gone.
func (s *geminiSummaryCapture) snapshot(c *geminiCounting, key string, words *summaryText) {
	if !words.set {
		return
	}
	at, ok := s.standing(c, key)
	if !ok {
		s.unsaid(key, 2*s.lineStart, "a compression's summary is a record the chat file had let go of")
		return
	}
	snap := geminiSnap{id: key, place: 2*at + 1}
	switch {
	case s.read.waitFailed(words.claim):
		return
	case words.past:
		snap.unsaid = errSummaryPast.Error()
	default:
		snap.text = words.words()
	}
	s.add(snap)
}

// unsaid is a snapshot of the record key at place at that cannot be said, and why.
func (s *geminiSummaryCapture) unsaid(key string, at int, why string) {
	s.add(geminiSnap{id: key, place: at, unsaid: why})
}

// lost says the replay cannot follow the file from the line read on.
func (s *geminiSummaryCapture) lost(why string) { s.unknown, s.why = 2*s.lineStart, why }

// add adds snap, letting go of the earliest snapshots' words past the bound.
func (s *geminiSummaryCapture) add(snap geminiSnap) {
	if s.first < 0 {
		s.first = snap.place
	}
	s.snaps = append(s.snaps, snap)
	s.held += len(snap.text)
	for i := 0; s.held > summaryReadMaxBytes && i < len(s.snaps); i++ {
		if n := &s.snaps[i]; n.unsaid == "" {
			s.held -= len(n.text)
			n.text, n.unsaid = "", errSummaryPast.Error()
		}
	}
	s.read.claim.track(&s.counted, s.held)
}

// drop lets each snapshot gone go.
func (s *geminiSummaryCapture) drop(gone func(geminiSnap) bool) {
	s.snaps = slices.DeleteFunc(s.snaps, func(n geminiSnap) bool {
		if gone(n) {
			s.held -= len(n.text)
			return true
		}
		return false
	})
	s.read.claim.track(&s.counted, s.held)
}

// reading is the newest snapshot standing.
func (s *geminiSummaryCapture) reading() sandbox.ThreadSummary {
	newest := geminiSnap{place: -1}
	if len(s.snaps) > 0 {
		newest = slices.MaxFunc(s.snaps, func(a, b geminiSnap) int { return a.place - b.place })
	}
	switch {
	case s.unknown >= 0 && newest.place < s.unknown && s.first >= 0 && s.first < s.unknown:
		s.read.unshaped(s.why)
	case newest.place < 0:
	case newest.unsaid != "":
		s.read.unshaped(newest.unsaid)
	default:
		// The newest is the read's from here, every other let go of: its
		// words count once.
		s.read.claim.track(&s.counted, 0)
		s.read.take(newest.text)
	}
	return s.read.reading()
}

// geminiWords is a content's words as the transcript reads a summary's
// ([chatReading.partEnd]): its first text part a thought is not, trimmed
// ([textTrim]) and held to [summaryReadMaxBytes]. Each part's text is read
// into part, and becomes text where the part ends as that first one. A nil
// one reads nothing.
type geminiWords struct {
	text, part summaryText
	trim       textTrim
	first      bool
	// claim is the box read's claim its words count toward ([summaryClaim]).
	claim *summaryClaim
}

// start starts a content.
func (w *geminiWords) start() {
	if w != nil {
		w.text.reset()
		w.first = false
	}
}

// partText is where a part's text is written as it streams, nil for nowhere.
func (w *geminiWords) partText() io.Writer {
	if w == nil {
		return nil
	}
	w.part.reset()
	w.part.claim, w.trim.claim = w.claim, w.claim
	return w.trim.begin(&w.part, TranscriptValueMaxBytes)
}

// untrimmed is how many bytes the part's text held before its trim.
func (w *geminiWords) untrimmed() int {
	if w == nil {
		return 0
	}
	return w.trim.n
}

// partEnd ends a part: text says it held a text, thought that the text is
// the model's reasoning.
func (w *geminiWords) partEnd(text, thought bool) {
	if w == nil || !text || thought || w.first {
		return
	}
	w.first = true
	w.text, w.part = w.part, w.text
	w.text.set = true
}
