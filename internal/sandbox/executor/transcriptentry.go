package executor

import (
	"io"
	"slices"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/textindex"
)

// AN ENTRY IS SHAPED, THEN COMMITTED ([transcriptBuilder.commit]): its text is
// redacted and cut where the page can hold it, held to the read's value bound
// ([transcriptBuilder.valueMax]) or streamed past it ([streamedEntry]), and only
// then counted, so a reader that learns a record's kind after its texts can
// shape them before it knows which it keeps. Streamed, a text is redacted from
// its first byte to its last, never held, so its opening and its size are the
// whole redaction's at any length, and a match is decided on its own words
// wherever they fall: what the held path makes of it.

// pendingEntry is one entry as the page would hold it, not yet counted: its
// text cut from its redaction and where that begins, or passed, the page
// passing it over (a kind the filter does not keep, or a text holding not every
// term). An entry before the page is neither: it is only counted.
type pendingEntry struct {
	entry    TranscriptEntry
	textFrom int
	passed   bool
}

// commit counts p, the transcript's next entry, and keeps it on the page where
// the page holds it, answering errTranscriptPage, the cut said, once the page
// is full. THE FIRST ENTRY OF A PAGE IS ALWAYS KEPT: one entry is bounded on its
// own, and a page that could not hold it would be a page no read gets past.
func (b *transcriptBuilder) commit(p pendingEntry) error {
	ordinal := b.seen
	b.seen++
	switch {
	case ordinal < b.from:
		return nil
	case p.passed:
		b.next = ordinal + 1
		return nil
	case len(b.entries) == b.bounds.Entries:
		b.cut = CutEntries
		return errTranscriptPage
	case len(b.entries) > 0 && b.bytes+len(p.entry.Text) > b.bounds.Bytes:
		b.cut = CutBytes
		return errTranscriptPage
	}
	b.entries = append(b.entries, p.entry)
	b.places = append(b.places, EntryPlace{Ordinal: ordinal, TextFrom: p.textFrom})
	b.bytes += len(p.entry.Text)
	b.next = ordinal + 1
	return nil
}

// matching reports whether an entry of kind is kept only where its text holds
// every term of the filter: [EntryUnread] is kept, never searched, its text
// this package's account of the gap.
func (b *transcriptBuilder) matching(kind EntryKind) bool {
	return len(b.filter.Terms) > 0 && kind != EntryUnread
}

// shapes reports whether an entry of kind at ordinal is shaped: one on the
// page, of a kind the filter keeps. Every other entry is only counted, and its
// text is never redacted, the expensive part, which is for what a reader is
// shown.
func (b *transcriptBuilder) shapes(kind EntryKind, ordinal int) bool {
	return ordinal >= b.from && b.filter.keepsKind(kind)
}

// held is e, its recorded text raw and held whole, as the page would hold it at
// ordinal: its opening ([entryOpening]), or, under a filter's terms, the window
// around the first ([windowAround]) where its whole redaction's own words hold
// every one ([redact.OwnWords]). A MATCH IS DECIDED ON WHAT ITS READER IS SHOWN,
// never on the recorded text, where a term inside a credential confirmed a
// guess at it; MayHold on the recorded text, failing no hit, picks what is
// redacted.
func (b *transcriptBuilder) held(e TranscriptEntry, raw string, ordinal int) pendingEntry {
	switch {
	case ordinal < b.from:
		return pendingEntry{}
	case !b.filter.keepsKind(e.Kind):
		return pendingEntry{passed: true}
	case !b.matching(e.Kind):
		var whole int
		e.Text, e.Cut, whole = entryOpening(raw, b.bounds.EntryBytes)
		return pendingEntry{entry: shape(e, whole)}
	}
	reading := readingOf(e.Kind)
	if !reading.MayHold(raw, b.filter.Terms) {
		return pendingEntry{passed: true}
	}
	shown := redact.Secrets(raw)
	first, all := reading.FindIn(redact.OwnWords(shown), b.filter.Terms)
	if !all {
		return pendingEntry{passed: true}
	}
	var p pendingEntry
	e.Text, p.textFrom, e.Cut = windowAround(shown, first, b.bounds.EntryBytes)
	p.entry = shape(e, len(shown))
	return p
}

// add shapes e, its recorded text raw, and commits it: held where raw is
// within the value bound, streamed past it.
func (b *transcriptBuilder) add(e TranscriptEntry, raw string) error {
	ordinal := b.seen
	if len(raw) <= b.valueMax || !b.shapes(e.Kind, ordinal) {
		return b.commit(b.held(e, raw, ordinal))
	}
	s := b.streamed(e)
	writeRunes(s, raw)
	return b.commit(s.end(ordinal))
}

// writeRunes writes text to w in pieces of at most [textDigestBytes], each
// whole characters, as the scanner hands a string's.
func writeRunes(w io.Writer, text string) {
	buf := make([]byte, 0, min(len(text), textDigestBytes))
	for len(text) > 0 {
		k := min(len(text), textDigestBytes)
		for k < len(text) && !utf8.RuneStart(text[k]) {
			k--
		}
		buf = append(buf[:0], text[:k]...)
		_, _ = w.Write(buf)
		text = text[k:]
	}
}

// entryRedaction is what a text past the value bound is redacted through as
// it is written: a [redact.Stream], or a test's own ([transcriptBuilder.redactor]).
type entryRedaction interface {
	io.Writer
	Close() error
}

// streamedEntry is one entry's text past the value bound, redacted as it is
// written and never held. Its writes never fail: a redaction the stream
// refuses ([redact.ErrDecidedEarly]) writes nothing more, and its entry ends
// as one saying so ([streamedEntry.end]), so a refusal never ends the read.
type streamedEntry struct {
	b       *transcriptBuilder
	e       TranscriptEntry
	r       entryRedaction
	out     *streamedText
	refused error
}

// streamed is a streamedEntry of e, which the page holds ([transcriptBuilder.shapes]).
func (b *transcriptBuilder) streamed(e TranscriptEntry) *streamedEntry {
	var f *textindex.Finder
	if b.matching(e.Kind) {
		f = readingOf(e.Kind).Finder(b.filter.Terms)
	}
	out := newStreamedText(b.bounds.EntryBytes, f)
	s := &streamedEntry{b: b, e: e, out: out}
	if b.redactor != nil {
		s.r = b.redactor(out)
	} else {
		s.r = redact.NewStream(out)
	}
	return s
}

// streamedOpening is a streamedEntry of a text whose redaction's opening alone
// is read ([streamedEntry.opening]): a name past the value bound.
func (b *transcriptBuilder) streamedOpening() *streamedEntry {
	out := newStreamedText(b.bounds.EntryBytes, nil)
	s := &streamedEntry{b: b, out: out}
	if b.redactor != nil {
		s.r = b.redactor(out)
	} else {
		s.r = redact.NewStream(out)
	}
	return s
}

// opening is the text's redaction cut to bound, at most the entry's, once the
// text has ended ([entryOpening]); the stream's refusal where it refused.
func (s *streamedEntry) opening(bound int) (string, error) {
	if s.refused == nil {
		s.refused = s.r.Close()
	}
	if s.refused != nil {
		return "", s.refused
	}
	return textcut.Within(string(s.out.head[:min(len(s.out.head), bound+1)]), bound), nil
}

// Write redacts p, the text's next bytes.
func (s *streamedEntry) Write(p []byte) (int, error) {
	if s.refused == nil {
		_, s.refused = s.r.Write(p)
	}
	return len(p), nil
}

// end is the entry at ordinal once its text has ended: its opening and the
// whole redaction's size, or, under a filter's terms, the window around the
// first where its own words hold every one; and an [EntryUnread] in its place
// where the stream refused the redaction, what it wrote neither kept nor shown.
func (s *streamedEntry) end(ordinal int) pendingEntry {
	if s.refused == nil {
		s.refused = s.r.Close()
	}
	if s.refused != nil {
		return s.b.held(TranscriptEntry{Kind: EntryUnread, Thread: s.e.Thread}, unredactedText, ordinal)
	}
	e, out := s.e, s.out
	if out.win == nil {
		e.Text, e.Cut = textcut.Within(string(out.head), s.b.bounds.EntryBytes), out.n > s.b.bounds.EntryBytes
		return pendingEntry{entry: shape(e, out.n)}
	}
	out.closeWords()
	if _, all := out.win.f.Found(); !all {
		return pendingEntry{passed: true}
	}
	var p pendingEntry
	if out.n <= s.b.bounds.EntryBytes {
		e.Text = string(out.head)
	} else {
		e.Text, p.textFrom, e.Cut = out.win.window(out.n)
	}
	p.entry = shape(e, out.n)
	return p
}

// unredactedText is what an entry whose redaction the stream refused says in
// its place.
const unredactedText = "a text of the harness's record could not be redacted as it was read, " +
	"and is not shown"

// windowSlice is the most of a redaction the window holds before the own
// words are split from it: what bounds how far past a hit the redaction has
// run once the hit is known ([streamWindow.settle]).
const windowSlice = 4 << 10

// streamedText is a redaction written to it as it arrives: its first bound+1
// bytes kept (the opening [entryOpening] cuts), every byte counted, and, under
// a filter's terms, its own words read for them ([redact.OwnWordsWriter]) with
// the window around the first hit kept as it passes ([streamWindow]).
type streamedText struct {
	bound int
	head  []byte
	n     int
	words *redact.OwnWordsWriter
	win   *streamWindow
}

// newStreamedText keeps the opening of an entry of bound, and finds f's terms
// where f is not nil.
func newStreamedText(bound int, f *textindex.Finder) *streamedText {
	t := &streamedText{bound: bound}
	if f != nil {
		t.win = newStreamWindow(bound, f)
		t.words = redact.NewOwnWordsWriter(t.win)
	}
	return t
}

func (t *streamedText) Write(p []byte) (int, error) {
	if keep := t.bound + 1 - len(t.head); keep > 0 {
		t.head = append(t.head, p[:min(len(p), keep)]...)
	}
	if t.win == nil {
		t.n += len(p)
		return len(p), nil
	}
	for rest := p; len(rest) > 0; {
		k := min(len(rest), windowSlice)
		t.win.hold(rest[:k])
		t.n += k
		_, _ = t.words.Write(rest[:k])
		rest = rest[k:]
	}
	return len(p), nil
}

// closeWords ends the redaction's own words: a marker's opening the splitter
// held is the text's own words, and so is a run still undecided; and the
// first hit, a word the text ends inside among them.
func (t *streamedText) closeWords() {
	_ = t.words.Close()
	t.win.settle(true)
}

// streamWindow is [windowAround] of a redaction written to it in pieces, in
// bounded memory. It is the [redact.StretchReader] the redaction's own words
// are split into, handing every call on to the [textindex.Finder] that reads
// them, and so it knows which bytes are a marker's as the splitter decides
// them. It keeps a ring of the redaction's last bytes and, once the first hit
// is known, the bytes the window around it can take. A HIT INSIDE A RUN NOT YET
// DECIDED is known only tentatively: the run closing as a marker takes it back,
// as it takes back the finder's reading.
type streamWindow struct {
	f           *textindex.Finder
	bound, room int
	// ring holds the redaction's last len(ring) bytes, of n written.
	ring []byte
	n    int
	// handed is how many bytes the splitter has handed over; run where the
	// run it holds undecided begins, -1 where none does; markers the decided
	// markers the ring or buf may hold, in order.
	handed, run int
	markers     []span
	// at is the first hit, -1 until it is known, tentative while inside the
	// undecided run; start where the window would begin before a marker moves
	// it; buf the redaction from base on, as far as a window around at reaches.
	at, start, base int
	tentative       bool
	buf             []byte
}

// span is a stretch of a redaction, from its byte offset from up to to.
type span struct{ from, to int }

// newStreamWindow is a window of at most bound bytes around the first of f's
// terms.
func newStreamWindow(bound int, f *textindex.Finder) *streamWindow {
	// THE RING reaches back a third of the room before the hit, past the
	// slice and the word in progress the hit is known after.
	room := max(bound-len(textcut.Opening("", bound)), 1)
	return &streamWindow{f: f, bound: bound, room: room, ring: make([]byte, room/3+2*windowSlice), run: -1,
		at: -1}
}

// reach is one past the last offset a window around the hit reads: room past
// it, and the byte after, which says whether the window's end cuts a rune.
func (w *streamWindow) reach() int { return w.at + w.room + 1 }

// hold keeps p, the redaction's next bytes: in buf while the window around
// the hit reaches them, and in the ring.
func (w *streamWindow) hold(p []byte) {
	if w.at >= 0 {
		if want := w.reach() - w.base - len(w.buf); want > 0 {
			w.buf = append(w.buf, p[:min(len(p), want)]...)
		}
	}
	if len(p) > len(w.ring) {
		w.n += len(p) - len(w.ring)
		p = p[len(p)-len(w.ring):]
	}
	for len(p) > 0 {
		k := copy(w.ring[w.n%len(w.ring):], p)
		w.n += k
		p = p[k:]
	}
}

// settle takes the first hit once no byte written next can move it: it
// begins before where the finder has settled, or the text has ended (final).
func (w *streamWindow) settle(final bool) {
	if w.at >= 0 {
		return
	}
	first, _ := w.f.Found()
	if first < 0 || !final && first >= w.f.Settled() {
		return
	}
	w.at, w.tentative = first, w.run >= 0 && first >= w.run
	w.start = max(first-w.room/3, 0)
	for w.start > 0 && !utf8.RuneStart(w.ring[w.start%len(w.ring)]) {
		w.start--
	}
	w.base = max(w.start-1, 0)
	w.buf = w.buf[:0]
	for o := w.base; o < min(w.n, w.reach()); o++ {
		w.buf = append(w.buf, w.ring[o%len(w.ring)])
	}
	w.prune()
}

// prune lets go of the markers before what the ring or buf holds.
func (w *streamWindow) prune() {
	oldest := w.n - len(w.ring)
	if w.at >= 0 {
		oldest = w.base
	}
	drop := 0
	for drop < len(w.markers) && w.markers[drop].to <= oldest {
		drop++
	}
	w.markers = slices.Delete(w.markers, 0, drop)
}

func (w *streamWindow) Stretch(at int) { w.f.Stretch(at) }

// Write reads p, the next own words or bytes of the undecided run, and takes
// the first hit AS SOON AS IT SETTLES, never at a slice's end alone: words
// rewound by a run of markers each time one closes would otherwise wait out
// slice after slice ending inside a marker's opening the splitter holds.
func (w *streamWindow) Write(p []byte) (int, error) {
	w.handed += len(p)
	n, err := w.f.Write(p)
	w.settle(false)
	return n, err
}

func (w *streamWindow) Mark() {
	w.f.Mark()
	w.run = w.handed
}

func (w *streamWindow) Commit() {
	w.f.Commit()
	w.run, w.tentative = -1, false
}

// Rewind is the undecided run decided a marker: its bytes are a marker's, and
// a hit inside it is none.
func (w *streamWindow) Rewind() {
	w.f.Rewind()
	if w.at < 0 || w.run < w.reach() {
		w.markers = append(w.markers, span{w.run, w.handed})
	}
	if w.tentative {
		w.at, w.tentative, w.buf = -1, false, w.buf[:0]
	}
	w.run = -1
	w.prune()
}

// marker reports whether the byte at offset o is a marker's.
func (w *streamWindow) marker(o int) bool {
	i, _ := slices.BinarySearchFunc(w.markers, o, func(s span, o int) int {
		switch {
		case s.to <= o:
			return -1
		case s.from > o:
			return 1
		}
		return 0
	})
	return i < len(w.markers) && w.markers[i].from <= o && o < w.markers[i].to
}

// inside reports whether offset o of a redaction of n bytes falls strictly
// inside a run of markers ([redact.MarkersAround]): the bytes on either side
// of it are both a marker's.
func (w *streamWindow) inside(o, n int) bool {
	return o > 0 && o < n && w.marker(o-1) && w.marker(o)
}

// window is [windowAround] of the redaction, n bytes long and past the
// entry's bound, around the first hit: its edges moved off markers, the
// opening past its end marked.
func (w *streamWindow) window(n int) (string, int, bool) {
	start := w.start
	if w.inside(start, n) {
		for start < n && w.marker(start) {
			start++
		}
	}
	end := min(start+w.room, n)
	for end < n && !utf8.RuneStart(w.buf[end-w.base]) {
		end--
	}
	if w.inside(end, n) {
		for end > start && w.marker(end-1) {
			end--
		}
	}
	shown := string(w.buf[start-w.base : end-w.base])
	if end == n {
		return shown, start, start > 0
	}
	return textcut.Opening(shown, w.bound), start, true
}
