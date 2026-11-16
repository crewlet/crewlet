package redact

import (
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"sync"
)

// stageReach is how far a stage reads past a place to decide there, and how
// far back: [MaxReach], and the bytes a format spells around what it bounds
// (85 at most), a line break's few and an armour's, with room to spare.
const stageReach = MaxReach + 512

// stageAdvance is how far a stage moves on at each step, and stageStep how much
// past where it resumes it holds to step: each byte is read about
// stageStep/stageAdvance times.
//
// TWICE stageReach, the least that lets every stage decide the first thing in
// each window, whatever it is: a key's value begins as far as [MaxReach] past
// the key and is trusted once a reach of its own follows it, and a key that
// names nothing once two reaches follow the key ([ruleStage.trusted]). So a
// stage holds four reaches, and reads each byte one and a half times: the
// memory a stream holds is the stages times what each holds ([Stream]), and a
// larger advance buys a smaller share of reads at a larger share of memory.
const (
	stageAdvance = 2 * stageReach
	stageStep    = stageAdvance + stageReach
)

// pieceBytes is the most a stage hands the next at once, and the most of a
// write a stream hands its stages at once: what a stage holds past its window
// waiting to step is at most this.
const pieceBytes = 32 << 10

// The keys' stage's geometry: an armour's block is read up to keyAhead past
// where the armour begins (a BEGIN's lines, an END line's rest) and keyBehind
// before it (an END's lines back, each line's start looked for that far
// again), and a block lies within keyReach of its armour ([MaxKeyBlockBytes]).
const (
	keyAhead  = MaxKeyBlockBytes + 512
	keyBehind = 2*MaxKeyBlockBytes + 512
	keyReach  = MaxKeyBlockBytes + 512
)

// wholeBytes is the longest text a [Stream] holds and redacts whole, as
// [Secrets] does: a text that fits in a stage's first window is redacted by
// every stage at once, with no window held.
const wholeBytes = stageStep

// errStreamClosed is what a write after [Stream.Close] answers.
var errStreamClosed = errors.New("redact: write to a closed stream")

// Stream redacts a text written to it in pieces, writing the redacted text to
// the writer it was made with: what it writes, put together, is exactly
// [Secrets] of everything written to it, however the text is cut into writes,
// and redacting that again changes nothing.
//
// HOW THE TEXT IS CUT INTO WRITES CHANGES NOTHING IT DOES: it hands its stages
// the text a piece at a time at the text's own offsets, each piece ending at a
// multiple of [pieceBytes], and carries what a write leaves short of the next
// ([pipeline.feed]), so every stage is handed the same pieces, and steps where
// it steps, however the text arrives.
//
// ITS MEMORY IS ITS STAGES' WINDOWS and the piece it carries, but for the two
// texts the package doc's "A text that arrives in pieces" names, which hold a
// run, so a text of any length, a whole PDF's base64 on one line of a
// transcript, is redacted without being held whole. What it writes trails what
// it is written by those windows: nothing is written until a text passes
// [wholeBytes], which a shorter one is redacted whole by, and then each stage
// writes on what it decides as it decides it, the last some MiB behind. Close
// writes the rest.
//
// A WRITE TO ITS WRITER IS CUT AT ANY BYTE, inside a rune too: each is a piece
// of the redacted text, and the pieces put together are text. A writer that
// reads each as text of its own, a JSON string's encoder, holds a rune's first
// bytes until the rest come.
type Stream struct {
	w      io.Writer
	err    error
	closed bool
	// head is the text written while it is no longer than [wholeBytes],
	// which is redacted whole; p the stages once it is longer.
	head []byte
	p    *pipeline
}

// NewStream is a Stream that writes what it redacts to w.
func NewStream(w io.Writer) *Stream {
	return &Stream{w: w}
}

// Write redacts p as the next piece of the text. It takes the whole of p,
// and answers the first error the writer gave, after which the stream writes
// nothing more.
func (s *Stream) Write(p []byte) (int, error) {
	switch {
	case s.closed:
		return 0, errStreamClosed
	case s.err != nil:
		return 0, s.err
	case s.p != nil:
		s.p.feed(p)
		return len(p), s.err
	}
	// Held only up to the byte that passes the length a text is redacted
	// whole below, and the rest handed on: a first write of any length is
	// copied no more than any other.
	n := min(len(p), wholeBytes+1-len(s.head))
	s.head = append(s.head, p[:n]...)
	if len(s.head) > wholeBytes {
		s.p = newPipeline(s.emit, true)
		s.p.feed(s.head)
		s.head = nil
		s.p.feed(p[n:])
	}
	return len(p), s.err
}

// Close writes the rest of the redacted text, and answers the first error the
// writer gave.
func (s *Stream) Close() error {
	if s.closed {
		return s.err
	}
	s.closed = true
	if s.p == nil {
		s.emit(Secrets(string(s.head)))
		s.head = nil
		return s.err
	}
	s.p.close()
	return s.err
}

// emit writes text to the stream's writer.
func (s *Stream) emit(text string) {
	if s.err != nil || text == "" {
		return
	}
	_, s.err = io.WriteString(s.w, text)
}

// pipeline is the stages a text is redacted by: the claims (every rule but the
// password's over the text as written, each tracking where what it writes
// stands in that text, and the readers of what the rules claim there), the
// keys' stage, and the rules over what the keys' stage writes.
type pipeline struct {
	claims  *ruleStage
	sources []*claimSource
	keys    *keyStage
	// rules is the first of the rules over what the keys' stage writes,
	// the pipeline itself where nothing comes before them.
	rules sink
	// carry is the text written past the last offset that is a multiple of
	// [pieceBytes], held until the next multiple is written or the text
	// ends ([pipeline.feed]).
	carry []byte
}

// newPipeline is the stages, writing what they redact to out: the rules alone
// where keys is false, for a text that holds no key's armour.
func newPipeline(out func(string), keys bool) *pipeline {
	table := rules()
	// The rules over what the keys' stage writes, last first.
	var next sink = outSink(out)
	for i := len(table) - 1; i >= 0; i-- {
		next = &ruleStage{r: table[i], to: next}
	}
	p := &pipeline{rules: next}
	if !keys {
		return p
	}
	// The readers of what the rules claim, behind every rule but the
	// password's: each AWS secret's key and separator, and the password's
	// keys with the values they name ([claimed]).
	var readers fanout
	for _, r := range table {
		if r.keyed == passwordRule {
			break
		}
		if r.value != nil {
			src := &claimSource{}
			p.sources = append(p.sources, src)
			readers = append(readers, &ruleStage{r: r, mode: finds, raw: &rawMap{}, src: src})
		}
	}
	src := &claimSource{}
	p.sources = append(p.sources, src)
	readers = append(readers, &ruleStage{r: table[len(table)-1], mode: reads, raw: &rawMap{}, src: src})
	var claims sink = readers
	for i := len(table) - 2; i >= 0; i-- {
		src := &claimSource{}
		p.sources = append(p.sources, src)
		stage := &ruleStage{r: table[i], to: claims, raw: &rawMap{}, src: src}
		claims = stage
		p.claims = stage
	}
	p.keys = &keyStage{to: next, sources: p.sources}
	for _, src := range p.sources {
		src.floor = &p.keys.floor
	}
	return p
}

// feed hands the stages b, the next of the text, a piece at a time
// ([pieceBytes]): the pieces of the text as written, each ending at an offset
// that is a multiple of pieceBytes, what is written past the last such offset
// carried until the next is written or the text ends ([pipeline.close]).
//
// AT THE TEXT'S OWN OFFSETS, NOT THE WRITE'S. Cut from each write's start, the
// pieces were the caller's: where a stage's input ended after each, and so
// when each stage stepped and what the next held when it stepped, followed how
// the text was cut into writes. What a stage decides never did, but whether a
// claims stage steps at a marker ([ruleStage.markerToStepAt]) and whether the
// keys' stage decides a window before its claims are known turn on what a
// stage holds when it is handed a piece, so cut by the writes, two readings
// of one text, a gzip reader's reads following a pipe's, could part on them.
// Cut here, every stage is handed the same pieces of a text however it
// arrives.
func (p *pipeline) feed(b []byte) {
	if n := len(p.carry); n > 0 {
		k := min(len(b), pieceBytes-n)
		p.carry = append(p.carry, b[:k]...)
		b = b[k:]
		if len(p.carry) < pieceBytes {
			return
		}
		p.write(string(p.carry))
		p.carry = p.carry[:0]
	}
	for len(b) >= pieceBytes {
		p.write(string(b[:pieceBytes]))
		b = b[pieceBytes:]
	}
	if len(b) > 0 {
		if p.carry == nil {
			p.carry = make([]byte, 0, pieceBytes)
		}
		p.carry = append(p.carry, b...)
	}
}

// write hands the stages the next piece of the text: the claims first, so the
// keys' stage reads them as far as they are decided.
func (p *pipeline) write(s string) {
	if p.keys == nil {
		p.rules.take(s, nil)
		return
	}
	p.claims.take(s, nil)
	p.keys.take(s)
}

// close ends the text, handing the stages what it carried first.
func (p *pipeline) close() {
	if len(p.carry) > 0 {
		p.write(string(p.carry))
		p.carry = nil
	}
	if p.keys == nil {
		p.rules.close()
		return
	}
	p.claims.close()
	p.keys.close()
}

// sink is what a stage writes to: the next stage, or the end.
type sink interface {
	// take is the next stretch of the receiver's input, with the edits in it
	// ([edit]), placed in the receiver's input; nil where nothing upstream
	// tracks them.
	take(s string, edits []edit)
	// closeEdit says where in the text as written the last edit handed ends,
	// where it was handed with its end to come ([edit]).
	closeEdit(rawEnd int)
	// close ends the input.
	close()
}

// outSink is the end: what the last stage writes, written out.
type outSink func(string)

func (o outSink) take(s string, _ []edit) { o(s) }
func (o outSink) closeEdit(int)           {}
func (o outSink) close()                  {}

// fanout hands its input to several readers.
type fanout []sink

func (f fanout) take(s string, edits []edit) {
	for _, r := range f {
		r.take(s, edits)
	}
}

func (f fanout) closeEdit(rawEnd int) {
	for _, r := range f {
		r.closeEdit(rawEnd)
	}
}

func (f fanout) close() {
	for _, r := range f {
		r.close()
	}
}

// feed is a stage's input: the stretches it has been handed from base on.
type feed struct {
	chunks []string
	base   int
	end    int
	eof    bool
}

// add hands the feed s.
func (f *feed) add(s string) {
	if s != "" {
		f.chunks = append(f.chunks, s)
		f.end += len(s)
	}
}

// window is the input from lo to hi: a stretch where one holds it whole, and
// the stretches joined where not, which the feed does not keep: it holds the
// stretches it was handed, no larger than [pieceBytes] past a stream's first
// ([feed.release]), and a window lives for the step that reads it.
func (f *feed) window(lo, hi int) string {
	f.release(lo)
	if lo >= hi {
		return ""
	}
	at := f.base
	k := 0
	for at+len(f.chunks[k]) <= lo {
		at += len(f.chunks[k])
		k++
	}
	if at+len(f.chunks[k]) >= hi {
		return f.chunks[k][lo-at : hi-at]
	}
	var b strings.Builder
	b.Grow(hi - lo)
	for ; at < hi; k++ {
		c := f.chunks[k]
		b.WriteString(c[max(0, lo-at):min(len(c), hi-at)])
		at += len(c)
	}
	return b.String()
}

// release lets go of every stretch that ends at or before lo.
func (f *feed) release(lo int) {
	drop := 0
	for drop < len(f.chunks) && f.base+len(f.chunks[drop]) <= lo {
		f.base += len(f.chunks[drop])
		drop++
	}
	if drop > 0 {
		f.chunks = slices.Delete(f.chunks, 0, drop)
	}
}

// held is how many bytes the feed holds.
func (f *feed) held() int { return f.end - f.base }

// edit is a marker in a stage's input, where it stands, [at, end), and the
// stretch of the text as written it stands for, [raw, rawEnd): what [written]
// keeps of each, in a text that arrives in pieces. rawEnd is -1 while the run
// the marker replaces is still being read.
type edit struct {
	at, end, raw, rawEnd int
}

// rawMap is where each place of a stage's input stands in the text as written,
// as [written.at] has it, for the places it may still be asked of.
type rawMap struct {
	edits []edit
	// shift is how far a place before every edit kept stands from its place
	// in the text as written.
	shift int
}

// add keeps the edits handed with the next stretch of the input.
func (m *rawMap) add(edits []edit) {
	m.edits = append(m.edits, edits...)
}

// at is where place i of the input was written from: in the text a marker
// replaced, where what it replaced began.
func (m *rawMap) at(i int) int {
	k, _ := slices.BinarySearchFunc(m.edits, i+1, func(e edit, i int) int { return e.at - i })
	if k == 0 {
		return i + m.shift
	}
	e := m.edits[k-1]
	if i < e.end || e.rawEnd < 0 {
		return e.raw
	}
	return i - e.end + e.rawEnd
}

// prune lets go of the edits that end at or before lo.
func (m *rawMap) prune(lo int) {
	k := 0
	for k < len(m.edits) && m.edits[k].end <= lo && m.edits[k].rawEnd >= 0 {
		m.shift = m.edits[k].rawEnd - m.edits[k].end
		k++
	}
	if k > 0 {
		m.edits = slices.Delete(m.edits, 0, k)
	}
}

// claimSource is what one claims stage, or one reader, claims in the text as
// written ([claims]): its tokens, or its passwords' keys, separators and
// values, in order, and how far it has decided them.
type claimSource struct {
	// frontier is where in the text as written its claims stop being known:
	// every claim it makes that begins before it is here, and each one's
	// end is too, or lies at it or past it where it is -1.
	frontier int
	tokens   [][2]int
	words    [][3]int
	// floor is where the keys' stage's next window begins: a claim that
	// ends there or before it is asked of no window, and is not kept.
	floor *int
}

// token claims t, a token's stretch, its end -1 while to come.
func (c *claimSource) token(t [2]int) {
	if t[1] < 0 || t[1] > *c.floor {
		c.tokens = append(c.tokens, t)
	}
}

// word claims w, a key, its separator's end and its value's end, the last -1
// while to come.
func (c *claimSource) word(w [3]int) {
	if w[2] < 0 || w[2] > *c.floor {
		c.words = append(c.words, w)
	}
}

// prune lets go of the claims that end at or before lo.
func (c *claimSource) prune(lo int) {
	c.tokens = slices.DeleteFunc(c.tokens, func(t [2]int) bool { return t[1] >= 0 && t[1] <= lo })
	c.words = slices.DeleteFunc(c.words, func(w [3]int) bool { return w[2] >= 0 && w[2] <= lo })
}

// stageMode is what a rule's stage does with what it decides.
type stageMode uint8

const (
	// replaces writes its input on, what the rule replaces replaced.
	replaces stageMode = iota
	// reads writes nothing and claims each key the rule reads, and the value
	// it names ([claimed]).
	reads
	// finds writes nothing and claims each match of the rule's pattern.
	finds
)

// ruleStage is one rule streamed over its input ([pipeline]).
type ruleStage struct {
	r    rule
	mode stageMode
	in   feed
	// pos is where its input is decided to and written: where it resumes.
	pos   int
	prior [][2]int
	run   openRun
	done  bool
	to    sink
	// raw and src are a claims stage's: where its input stands in the text
	// as written, and what it claims there.
	raw *rawMap
	src *claimSource
	// out is what a step writes, outPos where it stands in what the stage
	// has written, outEdits the edits in it, and passed whether the last
	// edit of the input, still open, has been written on.
	out      strings.Builder
	outPos   int
	outEdits []edit
	passed   bool
}

// openRun is a run a stage is reading on: a match or a value decided where it
// begins and still running on past the window, or a key's whitespace a reader
// claims once it ends.
type openRun struct {
	on bool
	// start is where it begins in the stage's input, and key a reader's key
	// and separator.
	start int
	key   [2]int
	// held is how many bytes before where the stage resumes are read past
	// and not yet decided: a Google token's dots, a value's backslashes.
	held int
	// w writes a value's pieces.
	w valueWriter
	// trail is a reader's run of a key's whitespace and escaped breaks after
	// its separator ([passwordTrail]); separator an AWS secret's key whose
	// separator's spaces run on, head what it holds before them, and after
	// whether the separator is read and the spaces run on after it.
	trail, separator, after bool
	head                    string
	// claimed is whether a separator's key is claimed already, its end to
	// come ([ruleStage.openSeparator]).
	claimed bool
}

// take hands the stage the next stretch of its input.
func (s *ruleStage) take(text string, edits []edit) {
	s.in.add(text)
	if s.raw != nil {
		s.raw.add(edits)
	}
	for s.step() {
	}
}

// closeEdit closes the edit of its input handed last.
func (s *ruleStage) closeEdit(rawEnd int) {
	e := &s.raw.edits[len(s.raw.edits)-1]
	e.rawEnd = rawEnd
	if s.passed {
		s.passed = false
		s.to.closeEdit(rawEnd)
	}
}

// close ends the stage's input.
func (s *ruleStage) close() {
	s.in.eof = true
	for s.step() {
	}
	if s.to != nil {
		s.to.close()
	}
}

// step decides one window of the input, if the stage holds one: [stageStep]
// past where it resumes, or the rest of a text that has ended.
func (s *ruleStage) step() bool {
	end := s.in.end
	if s.done || !s.in.eof && end < s.pos+stageStep {
		return false
	}
	hi := min(end, s.pos+stageStep)
	eof := s.in.eof && hi == end
	lo := max(s.in.base, s.pos-stageReach)
	text := s.in.window(lo, hi)
	limit := len(text)
	if !eof {
		limit -= stageReach
	}
	at := s.pos - lo
	wasOn := s.run.on
	if s.run.on {
		at = s.carry(text, lo, at, limit, eof)
	}
	if !s.run.on {
		at = s.scan(text, lo, at, limit, eof)
	}
	if !eof && lo+at == s.pos && !wasOn && !s.run.on {
		// A window that decides nothing would be stepped again for ever,
		// or read on to the text's end, which holds the rest of the text.
		// No window can: it holds two reaches past where the stage
		// resumes and a reach beyond, so what begins there, whose reads
		// [MaxReach] bounds, is decided in it, or begins a run read on
		// ([ruleStage.open]), and what begins further on is written up to
		// ([ruleStage.commit]). Stopped here, as the bug it would be,
		// rather than answered by holding the rest of the text.
		panic(fmt.Sprintf("redact: a stage's window decided nothing at %d of its input", s.pos))
	}
	s.pos = lo + at
	s.done = eof
	s.prior = slices.DeleteFunc(s.prior, func(v [2]int) bool { return v[1] < s.pos-stageReach })
	s.in.release(s.pos - stageReach)
	if s.raw != nil {
		s.raw.prune(s.pos)
		s.src.frontier = s.raw.at(s.pos)
		if s.run.separator && !s.run.claimed {
			s.src.frontier = s.raw.at(s.run.start)
		}
		if s.done {
			s.src.frontier = math.MaxInt
		}
	}
	s.flush()
	return true
}

// flush writes on what the step wrote.
func (s *ruleStage) flush() {
	if s.mode != replaces || s.out.Len() == 0 && len(s.outEdits) == 0 {
		return
	}
	out, edits := s.out.String(), s.outEdits
	s.out.Reset()
	s.outEdits = nil
	s.to.take(out, edits)
}

// event is one decision of a stage's rule: where what decides it begins and
// ends (the match, a value rule's key and separator), and what it replaces,
// empty where the key names nothing.
type event struct {
	key, v [2]int
}

// decide is the stage's rule's first decision at or after at.
func (s *ruleStage) decide(text string, at int, prior [][2]int) (event, bool) {
	if s.mode == finds {
		start, end, found := s.r.find(text, at)
		return event{key: [2]int{start, end}, v: [2]int{end, end}}, found
	}
	key, v, found := s.r.decide(text, at, prior)
	return event{key: key, v: v}, found
}

// scan decides the window from at, limit being where the decisions it can
// trust end, and is where the stage resumes.
func (s *ruleStage) scan(text string, lo, at, limit int, eof bool) int {
	prior := make([][2]int, len(s.prior))
	for i, v := range s.prior {
		prior[i] = [2]int{v[0] - lo, v[1] - lo}
	}
	defer func() {
		s.prior = s.prior[:0]
		for _, v := range prior {
			s.prior = append(s.prior, [2]int{v[0] + lo, v[1] + lo})
		}
	}()
	pending := -1
	if s.mode == finds && !eof {
		pending = s.pendingSeparator(text, at)
	}
	// done is where the window's input is written to, and from where the
	// rule looks next: past a key that names nothing, the two part.
	done, from := at, at
	for {
		ev, found := s.decide(text, from, prior)
		if pending >= 0 || s.mode == finds && found && !eof && ev.key[1] == len(text) {
			// An AWS secret's key whose separator's spaces run to the
			// window's end: whether its pattern matches there, and where
			// it ends, is read where they end.
			p := pending
			if found && ev.key[1] == len(text) && (p < 0 || ev.key[0] < p) {
				p = ev.key[0]
			}
			if !found || p <= ev.key[0] {
				if p+stageReach <= limit {
					return s.openSeparator(text, lo, p, limit)
				}
				return s.commit(text, lo, done, min(p, limit))
			}
		}
		if !found {
			return s.commit(text, lo, done, limit)
		}
		switch {
		case eof || s.trusted(text, ev, prior):
			done, from = s.apply(text, lo, done, ev, &prior)
		case ev.key[0]+stageReach <= limit && (ev.v[1] <= ev.v[0] || ev.v[0]+stageReach <= limit):
			if next, ok := s.open(text, lo, done, ev, limit); ok {
				return next
			}
			return s.commit(text, lo, done, min(ev.key[0], limit))
		default:
			return s.commit(text, lo, done, min(ev.key[0], limit))
		}
	}
}

// priorWith is prior with v, the value the rule replaced last, after it: only the
// values a later read can still ask of, those ending within [stageReach] of
// v's start ([pastMarkup], [keyPrefix]), copied down once the rest outnumber
// them, so the values a window holds are those its reach does.
func priorWith(prior [][2]int, v [2]int) [][2]int {
	prior = append(prior, v)
	k := 0
	for k < len(prior) && prior[k][1] < v[0]-stageReach {
		k++
	}
	if k > len(prior)/2 {
		prior = append(prior[:0], prior[k:]...)
	}
	return prior
}

// commit writes the window's input from at to c as it stands, and is c.
//
// C AS IT STANDS, even inside a rune: nothing begins in the rune c falls
// inside, so the next window, scanning on from c, finds what the whole text's
// scan does. A cut the window decided past is where a decision begins, where
// a match or a key does, at the start of a rune; and one at the end of what
// the window decides is cut only where the window found nothing at or after
// what it wrote to, or nothing before that end, so what begins next begins
// past it. Moved back to where a rune begins instead, a cut inside a run of
// bytes that begin none (invalid UTF-8) longer than a window went back to
// where the window's input began, the window decided nothing, and the stage
// held the rest of the text.
func (s *ruleStage) commit(text string, lo, at, c int) int {
	c = max(c, at)
	s.pass(text, lo, at, c)
	return c
}

// trusted reports whether the window holds every read that decided ev, prior
// being the values the rule replaced before it, placed in the window.
func (s *ruleStage) trusted(text string, ev event, prior [][2]int) bool {
	switch {
	case s.mode == finds:
		// Its pattern's match ends at what is no space, read in the
		// window ([ruleStage.pendingSeparator]).
		return ev.key[1] < len(text)
	case s.r.value != nil && ev.v[1] <= ev.v[0]:
		// A key that names nothing: its value was looked for as far as
		// [MaxReach] past the key, and the first byte of one read as far
		// again ([valueEnd]).
		return ev.key[0]+2*stageReach <= len(text) && (s.mode != reads || ev.key[1]+stageReach <= len(text))
	}
	decided := ev.v[1]
	switch {
	case s.r.value != nil && s.r.valueIn == nil:
		// A password's value [valueEnd] ends is decided where it stops
		// it, past the backslashes before an escaped break, which it
		// reads back over; one a wrapping's suffix cuts, by its key.
		if decided < len(text) && text[decided] == '\\' && readValue(text, ev.key[0], ev.key[1], prior).ran {
			for decided < len(text) && text[decided] == '\\' {
				decided++
			}
		}
	case s.r.run != nil && s.r.run.held != 0:
		// A Google token is decided past the dots it may end before.
		for decided < len(text) && text[decided] == s.r.run.held {
			decided++
		}
	}
	return decided+stageReach <= len(text)
}

// apply writes the window's input from done up to what ev replaces, and that,
// claims what it claims, and is where the input is written to and where the
// rule looks next.
func (s *ruleStage) apply(text string, lo, done int, ev event, prior *[][2]int) (int, int) {
	switch s.mode {
	case finds:
		s.src.word([3]int{s.raw.at(lo + ev.key[0]), s.raw.at(lo + ev.key[1]), s.raw.at(lo + ev.key[1])})
		return ev.key[1], ev.key[1]
	case reads:
		if ev.v[1] > ev.v[0] {
			s.src.word([3]int{s.raw.at(lo + ev.key[0]), s.raw.at(lo + ev.key[1]), s.raw.at(lo + ev.v[1])})
			*prior = priorWith(*prior, ev.v)
			return ev.v[1], ev.v[1]
		}
		s.src.word([3]int{s.raw.at(lo + ev.key[0]), s.raw.at(lo + ev.key[1]), s.raw.at(lo + ev.key[1])})
		return done, beyond(text, ev.key[0])
	}
	if ev.v[1] <= ev.v[0] {
		return done, beyond(text, ev.key[0])
	}
	s.pass(text, lo, done, ev.v[0])
	s.replace(lo+ev.v[0], lo+ev.v[1], func(b *strings.Builder) { s.r.write(b, text[ev.v[0]:ev.v[1]]) }, true)
	if s.r.value != nil {
		*prior = priorWith(*prior, ev.v)
	}
	return ev.v[1], ev.v[1]
}

// pass writes the window's input from a to b as it stands, with the edits in
// it, handing the next stage a piece at a time ([pieceBytes]).
func (s *ruleStage) pass(text string, lo, a, b int) {
	if s.mode != replaces {
		return
	}
	for a < b {
		c := min(b, a+pieceBytes-s.out.Len())
		if c <= a {
			s.flush()
			continue
		}
		if s.raw != nil {
			k, _ := slices.BinarySearchFunc(s.raw.edits, lo+a, func(e edit, i int) int { return e.at - i })
			for ; k < len(s.raw.edits) && s.raw.edits[k].at < lo+c; k++ {
				e := s.raw.edits[k]
				s.outEdits = append(s.outEdits, edit{s.outPos + e.at - lo - a, s.outPos + e.end - lo - a, e.raw, e.rawEnd})
				s.passed = e.rawEnd < 0
			}
		}
		s.out.WriteString(text[a:c])
		s.outPos += c - a
		a = c
	}
}

// replace writes what the rule makes of its input from start to end, written
// by write, and claims it; closed is whether end is where it ends, rather than
// where it is read to so far.
func (s *ruleStage) replace(start, end int, write func(*strings.Builder), closed bool) {
	n := s.out.Len()
	write(&s.out)
	if s.raw == nil {
		return
	}
	rawEnd := -1
	if closed {
		rawEnd = s.raw.at(end)
	}
	raw := s.raw.at(start)
	s.src.token([2]int{raw, rawEnd})
	written := s.out.Len() - n
	s.outEdits = append(s.outEdits, edit{s.outPos, s.outPos + written, raw, rawEnd})
	s.outPos += written
	if s.out.Len() >= pieceBytes {
		s.flush()
	}
}

// closeRun claims a run's end, end in the stage's input, where it was claimed
// while open.
func (s *ruleStage) closeRun(end int) {
	if s.raw == nil {
		return
	}
	rawEnd := s.raw.at(end)
	if s.mode == reads {
		s.src.words[len(s.src.words)-1][2] = rawEnd
		return
	}
	s.src.tokens[len(s.src.tokens)-1][1] = rawEnd
	s.to.closeEdit(rawEnd)
}

// open begins reading on a run that ev, decided where it begins, runs on past
// the window: writes the input up to it, what it replaces so far, and is where
// the stage resumes, the end of what the window decides, and whether ev is a
// run the stage can read on.
func (s *ruleStage) open(text string, lo, at int, ev event, limit int) (int, bool) {
	switch {
	case s.mode == finds:
		return 0, false
	case s.mode == reads && ev.v[1] <= ev.v[0]:
		// A key whose whitespace runs on: claimed once it ends.
		if ev.key[1] < limit {
			return 0, false
		}
		// The key and what follows it are claimed as far as the whitespace
		// runs, wherever it ends ([claims.names]).
		s.run = openRun{on: true, trail: true, start: lo + ev.key[0], key: [2]int{lo + ev.key[0], lo + ev.key[1]}}
		s.src.word([3]int{s.raw.at(lo + ev.key[0]), -1, -1})
		return ev.key[1], true
	case s.r.value == nil && s.r.run == nil, ev.v[1] <= ev.v[0]:
		return 0, false
	}
	start := ev.v[0]
	held := 0
	switch {
	case s.r.value != nil && s.r.valueIn == nil:
		for held < limit-start && text[limit-1-held] == '\\' {
			held++
		}
		if held == limit-start {
			// A value of backslashes so far: whether it is one at all is
			// decided where they end.
			return 0, false
		}
	case s.r.run != nil && s.r.run.held != 0:
		for held < limit-start && text[limit-1-held] == s.r.run.held {
			held++
		}
	}
	s.run = openRun{on: true, start: lo + start, key: [2]int{lo + ev.key[0], lo + ev.key[1]}, held: held, w: valueWriter{with: s.r.with}}
	switch s.mode {
	case reads:
		s.src.word([3]int{s.raw.at(lo + ev.key[0]), s.raw.at(lo + ev.key[1]), -1})
	default:
		s.pass(text, lo, at, start)
		if s.r.value != nil {
			s.replace(lo+start, lo+limit, func(b *strings.Builder) { s.run.w.write(b, text[start:limit-held]) }, false)
		} else {
			s.replace(lo+start, lo+limit, func(b *strings.Builder) { b.WriteString(s.r.with) }, false)
		}
	}
	return limit, true
}

// carry reads on the stage's open run from at, and is where the stage
// resumes: where the run ends, or the end of what the window decides while it
// runs on.
func (s *ruleStage) carry(text string, lo, at, limit int, eof bool) int {
	switch {
	case s.run.trail:
		return s.carryTrail(text, lo, at, eof)
	case s.run.separator:
		return s.carrySeparator(text, lo, at, limit, eof)
	case s.r.value != nil:
		return s.carryValue(text, lo, at, limit, eof)
	}
	return s.carryToken(text, lo, at, limit, eof)
}

// carryToken reads on a token's run ([tokenRun]).
func (s *ruleStage) carryToken(text string, lo, at, limit int, eof bool) int {
	t, held := s.r.run, s.run.held
	i := at
	for ; i < limit; i++ {
		c := text[i]
		if !t.in[c] || c == '-' && armourAt(text, i) {
			break
		}
		if c == t.held {
			held++
		} else {
			held = 0
		}
	}
	if i == limit && !eof {
		s.run.held = held
		return limit
	}
	end, next := i-held, i
	if held == 0 && t.padMax > 0 && (i == len(text) || text[i] != '-' || !armourAt(text, i)) {
		for n := 0; n < t.padMax && next < len(text) && text[next] == t.pad; n++ {
			next++
		}
		end = next
	}
	s.run = openRun{}
	s.closeRun(lo + end)
	s.repeat(t.held, held)
	return next
}

// carryValue reads on a value's run: an AWS secret's ([awsSecretByte]), or a
// password's ([valueStop]).
func (s *ruleStage) carryValue(text string, lo, at, limit int, eof bool) int {
	i, stopped := at, false
	if in := s.r.valueIn; in != nil {
		for i < limit && in(text[i]) {
			i++
		}
		stopped = i < limit
	} else {
		for ; i < limit; i++ {
			if valueStop(text, i) {
				stopped = true
				break
			}
		}
	}
	held := s.run.held
	if !stopped && !eof {
		// It runs on past what the window decides. The backslashes it
		// ends in are held, since a break after them can end the value
		// before them ([valueEnd]).
		t := 0
		for t < limit-at && text[limit-1-t] == '\\' {
			t++
		}
		if t == limit-at {
			s.run.held += t
			return limit
		}
		s.writeValue(held, text[at:limit-t])
		s.run.held = t
		return limit
	}
	end, literal := i, 0
	if stopped && s.r.valueIn == nil && text[i] == '\\' {
		end = valueBack(text, at, i)
		if end == at {
			// The backslashes held are the string's too.
			literal = held
		}
	}
	if literal == 0 {
		s.writeValue(held, text[at:end])
	}
	if s.mode == replaces {
		n := s.out.Len()
		s.run.w.close(&s.out)
		s.outPos += s.out.Len() - n
	}
	valueEnd := lo + end - literal
	s.prior = append(s.prior, [2]int{s.run.start, valueEnd})
	s.run = openRun{}
	s.closeRun(valueEnd)
	s.repeat('\\', literal)
	return end
}

// repeat writes n of c on, a piece at a time ([pieceBytes]): a run's dots or
// backslashes, counted rather than held while it is read on, are written out
// as any other stretch is, so the next stage holds no more of them at once
// than of anything else.
func (s *ruleStage) repeat(c byte, n int) {
	if s.mode != replaces {
		return
	}
	for n > 0 {
		k := min(n, pieceBytes-s.out.Len())
		if k <= 0 {
			s.flush()
			continue
		}
		for range k {
			s.out.WriteByte(c)
		}
		s.outPos += k
		n -= k
	}
}

// writeValue writes the next stretch of a value: the backslashes held before
// it, as few as decide its pieces, and rest.
func (s *ruleStage) writeValue(held int, rest string) {
	if s.mode != replaces {
		return
	}
	n := s.out.Len()
	s.run.w.write(&s.out, strings.Repeat(`\`, min(held, 2))+rest)
	s.outPos += s.out.Len() - n
}

// carryTrail reads on a reader's key whose whitespace runs on
// ([passwordTrail]), and claims it once it ends.
func (s *ruleStage) carryTrail(text string, lo, at int, eof bool) int {
	i := at + passwordTrailAt().FindStringIndex(text[at:])[1]
	if !eof && (i == len(text) || text[i] == '\\' && i+1 == len(text)) {
		return i
	}
	s.run = openRun{}
	end := s.raw.at(lo + i)
	w := &s.src.words[len(s.src.words)-1]
	w[1], w[2] = end, end
	return i
}

// pendingSeparator is where an AWS secret's key begins whose separator's
// spaces run to the window's end, so that whether the key's pattern matches
// there, and where, is still to be read, or -1.
func (s *ruleStage) pendingSeparator(text string, at int) int {
	loc := s.r.pending.FindStringIndex(text[at:])
	if loc == nil {
		return -1
	}
	return at + loc[0]
}

// openSeparator begins reading on the spaces of an AWS secret's key at p, and
// is where the stage resumes: past what the window decides, inside them.
//
// The key is claimed at once, as far as its spaces run, where its pattern
// matches whatever follows them: where the key is written straight before
// them, which its separator's spaces alone match. Where a quote or markup
// stands between, the pattern matches only once an operator follows them, and
// the stage claims nothing until it reads where they end.
func (s *ruleStage) openSeparator(text string, lo, p, limit int) int {
	space := p + len(strings.TrimRight(text[p:], " \t"))
	head := text[p:space]
	s.run = openRun{on: true, separator: true, start: lo + p, head: strings.Clone(head)}
	if loc := s.r.pattern.FindStringIndex(head + " "); loc != nil && loc[0] == 0 && loc[1] == len(head)+1 {
		s.run.claimed = true
		s.src.word([3]int{s.raw.at(lo + p), -1, -1})
	}
	return max(limit, space)
}

// carrySeparator reads on an AWS secret's separator's spaces, and claims the
// key and its separator once the pattern's match there is decided.
func (s *ruleStage) carrySeparator(text string, lo, at, limit int, eof bool) int {
	i := at
	for i < len(text) && (text[i] == ' ' || text[i] == '\t') {
		i++
	}
	if i == len(text) && !eof {
		return max(i, limit)
	}
	if s.run.after {
		s.claimSeparator(lo + i)
		return i
	}
	// The pattern decides by where the spaces end alone, not how many
	// there are: a separator's spaces are `[ \t]*` or `[ \t]+`.
	tail := text[i:min(len(text), i+8)]
	if len(tail) < 8 && !eof {
		return i
	}
	head := s.run.head + " "
	loc := s.r.pattern.FindStringIndex(head + tail)
	if loc == nil || loc[0] != 0 {
		// Claimed only where the pattern matches whatever follows, which
		// it then does.
		s.run = openRun{}
		return i
	}
	end := i + loc[1] - len(head)
	if loc[1] == len(head)+len(tail) && (text[end-1] == ' ' || text[end-1] == '\t') {
		// The spaces after its operator run on too.
		s.run.after = true
		return s.carrySeparator(text, lo, end, limit, eof)
	}
	s.claimSeparator(lo + end)
	return end
}

// claimSeparator claims an AWS secret's key and its separator, ending at end in
// the stage's input, once the pattern's match there is decided.
func (s *ruleStage) claimSeparator(end int) {
	rawEnd := s.raw.at(end)
	if s.run.claimed {
		w := &s.src.words[len(s.src.words)-1]
		w[1], w[2] = rawEnd, rawEnd
	} else {
		s.src.word([3]int{s.raw.at(s.run.start), rawEnd, rawEnd})
	}
	s.run = openRun{}
}

// tokenRun is how a token's match runs on past where it is decided: the bytes
// its run is written in, the one its end is read across (a Google token's
// dots), and the padding after it (an SSH certificate's `=`). Derived from the
// rule's own pattern ([tokenRunOf]), so the two cannot drift apart.
type tokenRun struct {
	in     [256]bool
	held   byte
	pad    byte
	padMax int
}

// tokenRunOf is the run of a token's pattern, or nil where the pattern bounds
// its match: its last part a class repeated without bound, or such a run and
// then one byte of a narrower class, the bytes it lacks held (`[….]{19,}[…]`),
// and after either a byte repeated a few times at most (`={0,3}`).
func tokenRunOf(pattern string) *tokenRun {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		panic(err)
	}
	subs := []*syntax.Regexp{re}
	if re.Op == syntax.OpConcat {
		subs = re.Sub
	}
	t := &tokenRun{}
	last := subs[len(subs)-1]
	if last.Op == syntax.OpRepeat && last.Max >= 0 && last.Max <= 3 && last.Sub[0].Op == syntax.OpLiteral && len(last.Sub[0].Rune) == 1 {
		t.pad, t.padMax = byte(last.Sub[0].Rune[0]), last.Max
		subs = subs[:len(subs)-1]
		last = subs[len(subs)-1]
	}
	if in, ok := unboundedClass(last); ok {
		t.in = in
		return t
	}
	if last.Op == syntax.OpCharClass && len(subs) >= 2 {
		if in, ok := unboundedClass(subs[len(subs)-2]); ok {
			end := classBytes(last)
			for c := range 256 {
				if in[c] && !end[c] {
					if t.held != 0 {
						panic("redact: a token's run ends on all but more than one of its bytes: " + pattern)
					}
					t.held = byte(c)
				}
			}
			t.in = in
			return t
		}
	}
	return nil
}

// unboundedClass is the bytes of re's class where re repeats one without
// bound.
func unboundedClass(re *syntax.Regexp) ([256]bool, bool) {
	switch {
	case re.Op == syntax.OpStar, re.Op == syntax.OpPlus, re.Op == syntax.OpRepeat && re.Max < 0:
		if re.Sub[0].Op == syntax.OpCharClass {
			return classBytes(re.Sub[0]), true
		}
	}
	return [256]bool{}, false
}

// classBytes is the bytes a class of runes below 256 holds.
func classBytes(re *syntax.Regexp) [256]bool {
	var in [256]bool
	for i := 0; i+1 < len(re.Rune); i += 2 {
		for r := re.Rune[i]; r <= re.Rune[i+1] && r < 256; r++ {
			in[r] = true
		}
	}
	return in
}

// passwordTrail is what a password's key's pattern ends in: the whitespace and
// escaped breaks after its separator ([passwordRule]).
const passwordTrail = `(?:\s|\\[nr])*`

// passwordTrailAt is [passwordTrail] held to begin where it is tried, read as
// the key's pattern reads it, in any case; built at first use, as the rules
// are ([rules]).
var passwordTrailAt = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^(?i:` + passwordTrail + `)`)
})

// keyStage is the keys' blocks streamed over the text as written ([readKeys]):
// it reads each armour's block in a window that holds all the block reads,
// once what the rules claim there is decided ([claimSource]), and writes the
// text on with each block replaced by [keyMarker].
type keyStage struct {
	in feed
	// pos is where the text is written to, and read where the armours are
	// read to: every armour that begins before it.
	pos, read int
	// pending is the blocks read and not yet written, in order and apart;
	// forward every block read forward from a BEGIN that a later read may
	// ask of; closed where each END such a block reached begins.
	pending [][2]int
	forward [][2]int
	closed  []int
	sources []*claimSource
	to      sink
	done    bool
	// floor is where its next window begins ([claimSource.floor]).
	floor int
}

// take hands the stage the next stretch of the text.
func (k *keyStage) take(text string) {
	k.in.add(text)
	for k.step() {
	}
}

// close ends the text.
func (k *keyStage) close() {
	k.in.eof = true
	for k.step() {
	}
	k.to.close()
}

// frontier is where what the rules claim stops being known ([claimSource]).
func (k *keyStage) frontier() int {
	f := math.MaxInt
	for _, s := range k.sources {
		f = min(f, s.frontier)
	}
	return f
}

// step reads the armours of one window, if the stage holds one and the claims
// in it are decided, and writes the text up to where no block read later can
// reach.
func (k *keyStage) step() bool {
	end := k.in.end
	hi := k.read + stageAdvance + keyAhead
	if k.done || !k.in.eof && end < hi {
		return false
	}
	eof := k.in.eof && end <= hi
	if eof {
		hi = end
	}
	lo := max(k.in.base, min(k.pos, k.read-keyBehind))
	readTo := hi - keyAhead
	if eof {
		readTo = hi
	}
	text := k.in.window(lo, hi)
	if strings.Contains(text[k.read-lo:min(len(text), readTo-lo+len(keyArmour)+64)], keyArmour) {
		if k.frontier() <= hi {
			return false
		}
		k.readArmours(text, lo, readTo)
	}
	k.read = readTo
	writeTo := readTo - keyReach
	if eof {
		writeTo = hi
	}
	k.write(text, lo, writeTo)
	k.done = eof
	return true
}

// readArmours reads the blocks of the armours that begin in [k.read, readTo).
func (k *keyStage) readArmours(text string, lo, readTo int) {
	c := k.claimsIn(text, lo)
	r := keyReading{open: -1}
	for _, f := range k.forward {
		r.forward = append(r.forward, [2]int{f[0] - lo, f[1] - lo})
	}
	for _, e := range k.closed {
		r.closed = append(r.closed, e-lo)
	}
	from, to := k.read-lo, readTo-lo
	carried := len(r.forward)
	begins := keyBegin.FindAllStringIndex(text, -1)
	for _, b := range begins {
		if from <= b[0] && b[0] < to {
			r.begin(text, b, c)
		}
	}
	c.take(r.forward)
	spans := append(slices.Clone(r.forward[carried:]), r.ends(text, c, begins, keyEnd.FindAllStringIndex(text, -1), from, to)...)
	for _, s := range c.shortOfNames(spans) {
		k.pending = append(k.pending, [2]int{s[0] + lo, s[1] + lo})
	}
	k.pending = mergeSpans(k.pending)
	for _, f := range r.forward[carried:] {
		k.forward = append(k.forward, [2]int{f[0] + lo, f[1] + lo})
	}
	k.closed = k.closed[:0]
	for _, e := range r.closed {
		if e+lo >= readTo {
			k.closed = append(k.closed, e+lo)
		}
	}
}

// claimsIn is what the rules claim in the window text, which begins at lo:
// every claim that reaches into it, placed in it, its end at most one past the
// window's where it is still to come.
func (k *keyStage) claimsIn(text string, lo int) *claims {
	hi := lo + len(text)
	clip := func(end int) int {
		if end < 0 || end > hi {
			return hi + 1 - lo
		}
		return end - lo
	}
	c := &claims{text: text, read: true}
	for _, s := range k.sources {
		for _, t := range s.tokens {
			if (t[1] < 0 || t[1] > lo) && t[0] <= hi {
				c.tokens = append(c.tokens, [2]int{t[0] - lo, clip(t[1])})
			}
		}
		for _, w := range s.words {
			if (w[2] < 0 || w[2] > lo) && w[0] <= hi {
				c.passwords = append(c.passwords, [3]int{w[0] - lo, clip(w[1]), clip(w[2])})
			}
		}
	}
	c.tokens = mergeSpans(c.tokens)
	c.build()
	return c
}

// write writes the text from k.pos to writeTo, moved back to where a block that
// runs across it begins, each block in it replaced by [keyMarker], a piece at a
// time ([pieceBytes]).
func (k *keyStage) write(text string, lo, writeTo int) {
	for _, s := range k.pending {
		if s[0] < writeTo && writeTo < s[1] {
			writeTo = s[0]
		}
	}
	var b strings.Builder
	piece := func(s string) {
		for s != "" {
			n := min(len(s), pieceBytes-b.Len())
			b.WriteString(s[:n])
			s = s[n:]
			if b.Len() >= pieceBytes {
				k.to.take(b.String(), nil)
				b = strings.Builder{}
			}
		}
	}
	at, n := k.pos, 0
	for ; n < len(k.pending) && k.pending[n][1] <= writeTo; n++ {
		s := k.pending[n]
		piece(text[at-lo : s[0]-lo])
		piece(keyMarker)
		at = s[1]
	}
	if writeTo > at {
		piece(text[at-lo : writeTo-lo])
		at = writeTo
	}
	if b.Len() > 0 {
		k.to.take(b.String(), nil)
	}
	k.pending = slices.Delete(k.pending, 0, n)
	k.pos = max(k.pos, at)
	// Let go of what no later read asks of: the text and the claims behind
	// the next window, and the blocks read forward behind it but the last,
	// and any that holds a password's key whose claim reaches into it.
	k.floor = min(k.pos, k.read-keyBehind)
	k.in.release(k.floor)
	keepFrom := k.floor
	for _, s := range k.sources {
		s.prune(k.floor)
		for _, w := range s.words {
			keepFrom = min(keepFrom, w[0])
		}
	}
	if len(k.forward) > 1 {
		last := k.forward[len(k.forward)-1]
		k.forward = slices.DeleteFunc(k.forward[:len(k.forward)-1], func(f [2]int) bool { return f[1] <= keepFrom })
		k.forward = append(k.forward, last)
	}
}
