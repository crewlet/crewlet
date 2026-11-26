package redact

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"regexp/syntax"
	"slices"
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

// holdBytes is H, how far the keys' stage lets the text run past a window
// before it decides the window whatever the claims there say: the claims'
// lag, a step, a piece and the longest marker a rule writes, the term the
// stream's memory figure counts for the keys' stage waiting on its claims
// ([keyStage.frontier]). Every token's and AWS secret's claim is known within
// it ([ruleStage.markerToStepAt]), so a window decided early can be wrong only
// on what the password's reader, or an AWS secret's quoted key, decides past
// it. A smaller H refuses more texts ([ErrDecidedEarly]), a larger one fewer;
// at this one no text of the package's corpus, generators or benchmark is.
var holdBytes = sync.OnceValue(func() int {
	longest := 0
	for _, r := range rules() {
		longest = max(longest, len(r.with))
	}
	return len(rules()) * (stageStep + pieceBytes + longest)
})

// earlyWindows is how many windows decided early wait at once for the claims
// over them to be known ([keyStage.wait]): 64, 2 KiB, which cover 8 MiB of
// armoured text whose claims trail it past H, and only a text built for it
// reaches; one more merges the two oldest, which makes the check coarser and
// never blind.
const earlyWindows = 64

// pieceBytes is the most of a write a stream hands its stages at once, and the
// most a stage puts into the next one's input before that one takes it
// ([ruleStage.flush]), and a marker past it: so what a stage holds past its
// window waiting to step is at most this and a marker. The password's stage,
// the last, writes a value's escaped breaks as they stand, up to a window a
// step, but to the end, which holds none of it ([endSink]).
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

// ErrDecidedEarly is what a [Stream] answers, wrapped with the first byte of
// the text from which what it wrote may differ from [Secrets], once it finds
// it decided a window of its text before what the rules claim
// there was known, and a claim it read then came otherwise: what it wrote is
// not [Secrets] of the text and may hold what Secrets redacts, so the caller
// must neither keep nor show any of it. A text gets it only where a key's
// block is read beside a password's key, or an AWS secret's quoted key, whose
// claim is decided more than the claims' lag past it: texts no credential's
// format writes.
var ErrDecidedEarly = errors.New("redact: the stream decided a window before a claim it read was known")

// decidedEarly is [ErrDecidedEarly] and the first byte of the text from which
// what the stream wrote may differ from [Secrets] ([keyStage.miss]).
type decidedEarly struct{ at int }

func (e *decidedEarly) Error() string {
	return fmt.Sprintf("%v (from byte %d of the text): what the stream wrote is not the text redacted, and may hold what the redaction takes out, so neither keep nor show any of it", ErrDecidedEarly, e.at)
}

func (e *decidedEarly) Unwrap() error { return ErrDecidedEarly }

// errStreamClosed is what a write after [Stream.Close] answers.
var errStreamClosed = errors.New("redact: write to a closed stream")

// Stream redacts a text written to it in pieces, writing the redacted text to
// the writer it was made with, in one pass, or refuses it. However the text
// is cut into writes, it holds its stages' windows (a live stream also the
// text past its settled point, [liveHeldBytes] at most), whether it refuses
// depends on the text and its flushes alone ([Stream.Flush]), and where
// Close answers nil it wrote exactly [Secrets]. Writes cut at any byte.
//
// A refusal is [ErrDecidedEarly], from Close and every later write: the keys'
// stage decided a window before the rules' claims there were known. What was
// written may then hold what redaction takes out, so the caller must neither
// keep nor show it. Only a text built against the password's rule or an AWS
// secret's quoted key, beside a key's armour, is refused.
type Stream struct {
	w      io.Writer
	err    error
	closed bool
	// head is the text written while it is no longer than [wholeBytes],
	// which is redacted whole; p the stages once it is longer.
	head []byte
	p    *pipeline
	end  *endSink
	// observe is handed to the keys' stage the stream makes, and poison
	// to every stage's input ([feed]), where a test sets them
	// ([keyObserver]).
	observe *keyObserver
	poison  bool
	// hold is how far the keys' stage lets the text run past a window
	// before it decides it ([holdBytes]).
	hold int
	// live is a live stream's ([NewLiveStream]), nil in any other. final
	// and held are what Flush answered last, kept for after the stream
	// stops, and emitted how much the stream wrote of its head itself.
	live    *live
	final   int64
	held    int
	emitted int64
}

// live is what a live stream keeps beside its stages: the text past the
// settled point, which its stages have not been handed, until it trails.
type live struct {
	// settled is the settled point in head while the text is held whole,
	// up to which Flush wrote head's redaction, and view the settling that
	// asks of head past it: a settling, not a [growing], so nothing but
	// Write appends to head or moves its bytes. raw is the text past the
	// settled point once the stages run, a copy, so head's buffer goes.
	settled int
	view    settling[[]byte]
	raw     growing
	// trailing is whether raw ran past [liveHeldBytes] and went to the
	// stages, which hold all the text since ([Stream.Trailing]).
	trailing bool
}

// liveHeldBytes is the most text a live stream holds past its settled point
// once a write is taken, and half the most during one ([Stream.settleIn]):
// [LookBack], the reads that cross a line break summed along the stages,
// which is as far as any lines Settled holds back of a credential's format
// reach, so only a line or a run of holds no format writes passes it. Each
// ask rereads what is held, so a larger bound costs every write more; a
// smaller one would trail beside a key's block and a password's value below.
const liveHeldBytes = LookBack

// NewStream is a Stream that writes what it redacts to w.
func NewStream(w io.Writer) *Stream {
	return newStreamHolding(w, holdBytes())
}

// NewLiveStream is a Stream whose Flush writes the text's redaction as far as
// [Settled] settles it: its stages are handed only settled text, asked of as
// each line ends ([settling]).
func NewLiveStream(w io.Writer) *Stream {
	s := NewStream(w)
	s.live = &live{}
	return s
}

// newStreamHolding is a Stream whose keys' stage decides a window once the
// text has run h past it: [holdBytes] in every stream, and a test's own, so
// a shape that decides early costs it megabytes rather than the claims' lag.
func newStreamHolding(w io.Writer, h int) *Stream {
	return &Stream{w: w, hold: h}
}

// Write redacts p as the next piece of the text. It takes the whole of p,
// and answers the first error the writer gave, or [ErrDecidedEarly], after
// which the stream writes nothing more.
func (s *Stream) Write(p []byte) (int, error) {
	switch {
	case s.closed:
		return 0, errStreamClosed
	case s.err != nil:
		return 0, s.err
	case s.p != nil && s.live != nil && !s.live.trailing:
		s.settleIn(p)
		return len(p), s.err
	case s.p != nil:
		s.p.feed(p)
		s.endErr()
		s.stopOnMiss()
		return len(p), s.err
	}
	// Held only up to the byte that passes the length a text is redacted
	// whole below, and the rest handed on: a first write of any length is
	// copied no more than any other.
	n := min(len(p), wholeBytes+1-len(s.head))
	s.head = append(s.head, p[:n]...)
	if len(s.head) > wholeBytes {
		s.end = &endSink{w: s.w}
		s.p = newPipeline(s.end, true, s.hold)
		s.p.keys.observe = s.observe
		for _, f := range s.p.feeds() {
			f.poison = s.poison
		}
		if l := s.live; l != nil {
			// What Flush wrote of the head is the start of what the
			// stages write ([Settled]), so the end drops it.
			s.end.skip = s.emitted
			s.p.feed(s.head[:l.settled])
			l.raw = growing{l.view}
			l.raw.text = bytes.Clone(s.head[l.settled:])
			l.view, s.head = settling[[]byte]{}, nil
			s.settleIn(p[n:])
			return len(p), s.err
		}
		s.p.feed(s.head)
		s.head = nil
		s.p.feed(p[n:])
		s.endErr()
		s.stopOnMiss()
	}
	return len(p), s.err
}

// settleIn takes p as the next of a live stream's text, and hands its stages
// what is settled. It takes p [liveHeldBytes] at a time, asking after each,
// so what it holds stays within twice that and each ask reads no more; held
// past it once asked, the stream trails ([Stream.trail]).
func (s *Stream) settleIn(p []byte) {
	l := s.live
	for {
		n := min(len(p), liveHeldBytes)
		l.raw.write(p[:n])
		p = p[n:]
		s.settle()
		switch {
		case s.p == nil || s.err != nil:
			return
		case len(l.raw.text) > liveHeldBytes:
			s.trail(p)
			return
		case len(p) == 0:
			return
		}
	}
}

// trail hands a live stream's stages all it holds and rest, and from then on
// all it is written, as a [NewStream]'s, so it holds no more than their
// windows: past [liveHeldBytes], a line too long or a run of holds no
// credential's format writes. Flush then decides nothing as at an end, since
// the stages hold text Settled has not settled, and the stream never settles
// again (the package doc's "Text that is still being written").
func (s *Stream) trail(rest []byte) {
	l := s.live
	l.trailing = true
	s.p.feed(l.raw.text)
	l.raw.text = nil
	s.p.feed(rest)
	s.endErr()
	s.stopOnMiss()
}

// Trailing reports whether a live stream has stopped writing as far as
// [Settled] settles: past [liveHeldBytes] held, its stages write what they
// decide, trailing by their windows, and Flush's held counts the text not
// yet in what it wrote. It trails until Close, which writes the rest, and
// never refuses for it. A stream that is not live never trails.
func (s *Stream) Trailing() bool {
	return s.live != nil && s.live.trailing
}

// settle hands a live stream's stages the text [Settled] settled since it
// last asked.
func (s *Stream) settle() {
	s.live.raw.settle(s.p.feed)
	s.endErr()
	s.stopOnMiss()
}

// Flush writes the redaction of the text as far as it is settled, and answers
// how many bytes of all the stream has written are final (a prefix of Secrets
// of the text whatever is written after it or the stream answers later) and
// how many bytes of the text are not yet in what it wrote. On a Stream from
// NewStream, or a live one that trails ([Stream.Trailing]), it writes nothing
// and counts only what no window decided early waits on, and held is a lower
// bound. After an error it answers the counts it last had.
//
// Each stage is handed only settled text, and decides all it holds as it does
// at the text's end ([pipeline.decideHeld]): the redaction composes there. So
// later windows fall elsewhere, and only in a layout that can refuse
// ([ErrDecidedEarly]) can a flush spare a refusal, make one, or move its byte.
func (s *Stream) Flush() (final int64, held int, err error) {
	l := s.live
	switch {
	case s.err != nil:
		return s.final, s.held, s.err
	case s.closed:
		return s.written(), 0, nil
	case s.p == nil && l != nil:
		// One settling across flushes, so a flush with no new line
		// break asks nothing.
		l.view.text = s.head[l.settled:]
		if n := l.view.next(); n > 0 {
			s.emit(Secrets(string(s.head[l.settled : l.settled+n])))
			l.settled += n
			l.view.scanned -= n
		}
	case s.p != nil && l != nil && !l.trailing:
		if s.settle(); s.p != nil {
			s.p.decideHeld()
			s.endErr()
			s.stopOnMiss()
		}
	}
	if s.err != nil {
		return s.final, s.held, s.err
	}
	s.final, s.held = s.counts()
	return s.final, s.held, nil
}

// counts is what Flush answers of a stream that has not stopped.
func (s *Stream) counts() (final int64, held int) {
	l := s.live
	switch {
	case s.p == nil && l != nil:
		return s.emitted, len(s.head) - l.settled
	case s.p == nil:
		return 0, len(s.head)
	case l != nil && !l.trailing:
		return s.written(), len(l.raw.text)
	}
	return s.emitted + s.p.keys.finalWritten(), s.p.keys.arrived + len(s.p.carry) - s.p.lent - s.p.keys.pos
}

// written is how many bytes the stream has written.
func (s *Stream) written() int64 {
	n := s.emitted
	if s.end != nil {
		n += s.end.n
	}
	return n
}

// stopOnMiss stops the stream where its keys' stage found a window it decided
// early read a claim otherwise than it came: it writes nothing more, lets its
// stages go, and answers [ErrDecidedEarly] from then on. Running on would buy
// nothing, since what it wrote is wrong already and nothing reads the text
// again to correct it.
func (s *Stream) stopOnMiss() {
	if s.p != nil && s.p.keys != nil && s.p.keys.missed != nil {
		if s.err == nil {
			s.err = s.p.keys.missed
		}
		s.final = s.emitted + s.p.keys.finalWritten()
		s.p = nil
	}
}

// Close writes the rest of the redacted text, and answers nil, the first error
// the writer gave, or [ErrDecidedEarly], whose stream's text the caller must
// neither keep nor show.
func (s *Stream) Close() error {
	if s.closed {
		return s.err
	}
	s.closed = true
	if s.live != nil {
		s.live.view.text = nil
	}
	if s.err != nil {
		s.p, s.head = nil, nil
		return s.err
	}
	if s.p == nil {
		from := 0
		if s.live != nil {
			from = s.live.settled
		}
		s.emit(Secrets(string(s.head[from:])))
		s.head = nil
		return s.err
	}
	if l := s.live; l != nil {
		s.p.feed(l.raw.text)
		l.raw.text = nil
	}
	s.p.close()
	s.endErr()
	s.stopOnMiss()
	s.p = nil
	return s.err
}

// endErr is the first error the stream's writer gave its stages' end.
func (s *Stream) endErr() {
	if s.err == nil && s.end != nil {
		s.err = s.end.err
	}
}

// emit writes text to the stream's writer, counted in what it wrote.
func (s *Stream) emit(text string) {
	if s.err != nil || text == "" {
		return
	}
	n, err := io.WriteString(s.w, text)
	s.emitted += int64(n)
	s.err = err
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
	// ends ([pipeline.feed]), and lent how much of it a flush handed the
	// stages already ([pipeline.decideHeld]).
	carry []byte
	lent  int
}

// newPipeline is the stages, writing what they redact to out: the rules alone
// where keys is false, for a text that holds no key's armour; the keys' stage
// deciding a window once the text has run hold past it ([holdBytes]).
func newPipeline(out *endSink, keys bool, hold int) *pipeline {
	table := rules()
	// The rules over what the keys' stage writes, last first.
	var next sink = out
	for i := len(table) - 1; i >= 0; i-- {
		next = newRuleStage(ruleStage{r: table[i], to: next})
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
		if r.keyed == passwordRule() {
			break
		}
		if r.value != nil {
			src := &claimSource{}
			p.sources = append(p.sources, src)
			readers = append(readers, newRuleStage(ruleStage{r: r, mode: finds, raw: &rawMap{}, src: src}))
		}
	}
	src := &claimSource{}
	p.sources = append(p.sources, src)
	readers = append(readers, newRuleStage(ruleStage{r: table[len(table)-1], mode: reads, raw: &rawMap{}, src: src}))
	var claims sink = readers
	handed := new([]edit)
	for i := len(table) - 2; i >= 0; i-- {
		src := &claimSource{}
		p.sources = append(p.sources, src)
		stage := newRuleStage(ruleStage{r: table[i], to: claims, raw: &rawMap{}, src: src, handed: handed})
		claims = stage
		p.claims = stage
	}
	p.keys = &keyStage{to: next, sources: p.sources, hold: hold, out: out}
	p.keys.in.bound, p.keys.in.grow = keysBytes+hold, keysBytes
	for _, src := range p.sources {
		src.keys = p.keys
	}
	return p
}

// feeds is every stage's input in p.
func (p *pipeline) feeds() []*feed {
	var all []*feed
	var walk func(s sink)
	walk = func(s sink) {
		switch st := s.(type) {
		case *ruleStage:
			all = append(all, &st.in)
			walk(st.to)
		case fanout:
			for _, r := range st {
				walk(r)
			}
		}
	}
	walk(p.rules)
	if p.keys != nil {
		all = append(all, &p.keys.in)
		walk(p.claims)
	}
	return all
}

// newRuleStage is s with its input's bound set ([stageBytes]).
func newRuleStage(s ruleStage) *ruleStage {
	s.in.bound = stageBytes()
	return &s
}

// stageBytes is the most a rule's stage holds of its input, what it has been
// handed and what it is being handed: [stageReach] behind where it resumes,
// less than [stageStep] past it, and a piece and a marker past that.
var stageBytes = sync.OnceValue(func() int {
	longest := len(keyMarker)
	for _, m := range ruleMarkers() {
		longest = max(longest, len(m))
	}
	return stageReach + stageStep + pieceBytes + longest
})

// keysBytes is what the keys' stage holds besides what it waits on: a piece,
// four blocks' reach behind where it reads ([keyStage.opening] looks a block
// before a block's start) and a step past it. Its buffer doubles up to this
// and then jumps to this and the hold, so the jump holds this, about 484 KiB,
// beside the new buffer for a moment, and a stream that never waits on its
// claims never allocates the hold.
const keysBytes = pieceBytes + 4*keyReach + stageAdvance + keyAhead

// feed hands the stages b, the next of the text, a piece at a time
// ([pieceBytes]): the pieces of the text as written, each ending at an offset
// that is a multiple of pieceBytes, what is written past the last such offset
// carried until the next is written or the text ends ([pipeline.close]).
//
// AT THE TEXT'S OWN OFFSETS, NOT THE WRITE'S: cut from each write's start,
// when each stage stepped and what the next held would follow how the text was
// cut into writes, and whether a claims stage steps at a marker
// ([ruleStage.markerToStepAt]) or the keys' stage decides a window early turns
// on that, so two readings of one text could part.
func (p *pipeline) feed(b []byte) {
	if n := len(p.carry); n > 0 {
		k := min(len(b), pieceBytes-n)
		p.carry = append(p.carry, b[:k]...)
		b = b[k:]
		if len(p.carry) < pieceBytes {
			return
		}
		p.write(p.carry[p.lent:])
		p.carry, p.lent = p.carry[:0], 0
	}
	for len(b) >= pieceBytes && !p.missed() {
		p.write(b[:pieceBytes])
		b = b[pieceBytes:]
	}
	if p.missed() {
		return
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
func (p *pipeline) write(b []byte) {
	if p.keys == nil {
		p.rules.put(b)
		p.rules.took(nil)
		return
	}
	p.keys.arrived += len(b)
	p.claims.put(b)
	p.claims.took(nil)
	if p.missed() {
		return
	}
	p.keys.take(b)
}

// missed reports whether the keys' stage found a miss ([Stream.stopOnMiss]):
// the stages are handed nothing more.
func (p *pipeline) missed() bool {
	return p.keys != nil && p.keys.missed != nil
}

// close ends the text, handing the stages what it carried first.
func (p *pipeline) close() {
	if len(p.carry) > p.lent {
		p.write(p.carry[p.lent:])
	}
	p.carry, p.lent = nil, 0
	if p.keys == nil {
		p.rules.close()
		return
	}
	if p.missed() {
		return
	}
	p.claims.close()
	if p.missed() {
		return
	}
	p.keys.close()
}

// decideHeld hands the stages what it carried, and has each decide all it
// holds as at the text's end, in order, staying open for what follows: what a
// live stream's flush does once its stages hold only settled text. The rest
// of the carried piece is handed once the text reaches the piece's end, so
// every later piece ends at the text's own offset ([pipeline.feed]).
//
// Settled's promise is that the redaction composes at the settled point: cut
// there, the text redacts as the two parts do apart. So no stage's decision
// crosses it, and deciding each stage there as at an end decides what the
// whole text does, and no claim decided there changes later.
func (p *pipeline) decideHeld() {
	if len(p.carry) > p.lent {
		p.write(p.carry[p.lent:])
		p.lent = len(p.carry)
	}
	if p.missed() {
		return
	}
	if p.keys == nil {
		p.rules.decideHeld()
		return
	}
	p.claims.decideHeld()
	if !p.missed() {
		p.keys.decideHeld()
	}
}

// sink is what a stage writes to: the next stage, or the end.
//
// A STAGE WRITES STRAIGHT INTO THE NEXT ONE'S INPUT: put copies a stretch in
// past what the receiver has taken, and took makes all put since the last
// took its input, with the edits in it, and steps it. So each byte is copied
// once a stage, and what is put but not taken is in no window.
type sink interface {
	// put, putString and putRepeat (n of c) write the next of the
	// receiver's input, which it takes at the next took.
	put(b []byte)
	putString(s string)
	putRepeat(c byte, n int)
	// took is what was put since the last took, with the edits in it
	// ([edit]), placed in the receiver's input; nil where nothing upstream
	// tracks them.
	took(edits []edit)
	// closeEdit says where in the text as written the last edit handed ends,
	// where it was handed with its end to come ([edit]).
	closeEdit(rawEnd int)
	// close ends the input, and decideHeld decides all of it as at the
	// input's end, keeping it open ([pipeline.decideHeld]).
	close()
	decideHeld()
}

// endSink is the end: what the last stage writes, written straight to w,
// less the first skip bytes, n how many it wrote, and the first error w gave,
// after which it writes nothing. It keeps no output: w keeps none of a view
// it is handed (io.Writer's contract), and a string (a marker, a value's
// escaped break) is copied through scratch rather than handed to
// io.WriteString, which copies it to the heap for a writer that has no
// WriteString of its own.
type endSink struct {
	w       io.Writer
	err     error
	n       int64
	skip    int64
	scratch [512]byte
}

func (e *endSink) put(b []byte) {
	if e.skip > 0 {
		k := min(e.skip, int64(len(b)))
		e.skip -= k
		b = b[k:]
	}
	if e.err == nil && len(b) > 0 {
		var n int
		n, e.err = e.w.Write(b)
		e.n += int64(n)
	}
}

func (e *endSink) putString(s string) {
	for s != "" {
		n := copy(e.scratch[:], s)
		e.put(e.scratch[:n])
		s = s[n:]
	}
}

func (e *endSink) putRepeat(c byte, n int) {
	for n > 0 {
		k := min(n, len(e.scratch))
		for i := range k {
			e.scratch[i] = c
		}
		e.put(e.scratch[:k])
		n -= k
	}
}

func (e *endSink) took([]edit)   {}
func (e *endSink) closeEdit(int) {}
func (e *endSink) close()        {}
func (e *endSink) decideHeld()   {}

// fanout hands its input to several readers.
type fanout []sink

func (f fanout) put(b []byte) {
	for _, r := range f {
		r.put(b)
	}
}

func (f fanout) putString(s string) {
	for _, r := range f {
		r.putString(s)
	}
}

func (f fanout) putRepeat(c byte, n int) {
	for _, r := range f {
		r.putRepeat(c, n)
	}
}

func (f fanout) took(edits []edit) {
	for _, r := range f {
		r.took(edits)
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

func (f fanout) decideHeld() {
	for _, r := range f {
		r.decideHeld()
	}
}

// feed is a stage's input, in ONE buffer read in place: buf[off:] holds the
// input from base to end, then what was put and is still to be taken
// (staged). A window is a view of it, which lives only for the step that
// reads it: a feed is written only by its one upstream, never during its own
// step, and a step keeps only places and copies ([openRun]). The buffer is
// allocated at its bound, exactly (the keys' stage's doubles first, up to
// grow), and slides its input down when the tail lacks room, so it is
// reallocated only for a text that runs past the bound, which a test fails.
// poison, a test's, reallocates it at the first reserve after each take and
// fills the old one with 0xFF, so a view kept past its step reads bytes no
// text holds.
type feed struct {
	buf       []byte
	off       int
	base, end int
	staged    int
	eof       bool
	bound     int
	grow      int
	poison    bool
	// takes is how many stretches it has taken, largest the longest.
	takes, largest int
}

// reserve is room for the next n bytes put, past what it holds and what was
// put already.
func (f *feed) reserve(n int) []byte {
	held := f.end - f.base + f.staged
	fresh := f.poison && f.staged == 0
	if f.off+held+n > len(f.buf) || fresh {
		need, old := held+n, f.buf
		if need > len(f.buf) || fresh {
			c := max(f.bound, need)
			if f.grow > 0 && need <= f.grow {
				c = min(f.grow, max(need, 2*len(f.buf)))
			}
			f.buf = make([]byte, c)
		}
		copy(f.buf, old[f.off:f.off+held])
		if fresh {
			for i := range old {
				old[i] = 0xFF
			}
		}
		f.off = 0
	}
	i := f.off + held
	f.staged += n
	return f.buf[i : i+n : i+n]
}

func (f *feed) put(b []byte) { copy(f.reserve(len(b)), b) }

func (f *feed) putString(s string) { copy(f.reserve(len(s)), s) }

func (f *feed) putRepeat(c byte, n int) {
	b := f.reserve(n)
	for i := range b {
		b[i] = c
	}
}

// took makes what was put the feed's input.
func (f *feed) took() {
	if f.staged > 0 {
		f.end += f.staged
		f.takes, f.largest = f.takes+1, max(f.largest, f.staged)
		f.staged = 0
	}
}

// add puts b and takes it.
func (f *feed) add(b []byte) {
	f.put(b)
	f.took()
}

// window is the input from lo to hi, a view of the feed's buffer that lives
// for the step that reads it.
func (f *feed) window(lo, hi int) []byte {
	f.release(lo)
	if lo >= hi {
		return nil
	}
	i := f.off + lo - f.base
	return f.buf[i : i+hi-lo : i+hi-lo]
}

// release lets go of the input before lo.
func (f *feed) release(lo int) {
	if lo <= f.base {
		return
	}
	n := min(lo, f.end) - f.base
	f.off += n
	f.base += n
	if f.base == f.end && f.staged == 0 {
		f.off = 0
	}
}

// drop lets go of the buffer of a stage that is done.
func (f *feed) drop() {
	f.buf, f.off = nil, 0
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
//
// A CLAIM CHANGES where it is handed, or where one of its ends is set, and
// only the last a source holds is ever open: a stage reading a run on claims
// nothing else until it ends.
type claimSource struct {
	// frontier is where in the text as written its claims stop being known:
	// every claim it makes that begins before it is here, and each one's
	// end is too, or lies at it or past it where it is -1.
	frontier int
	tokens   [][2]int
	// words are each password's or AWS secret's key, its separator's end,
	// and its value's start and end, -1 while to come, its seams, its
	// second range and that range's seams ([aValue]). A start of
	// -1 stays so: only a word that names no value is claimed without one,
	// and the window reads it past its end, as none ([keyStage.claimsIn]).
	words [][12]int
	// runs are the stretches of a key's last run each word's value was read
	// past, beside the word's key ([claims.keyRuns]): the window that reads
	// where that key begins writes them as blocks ([keyStage.runsIn]).
	runs [][3]int
	// keys is the keys' stage that reads the claims: a claim that ends at
	// or before where its next window begins ([keyStage.floor]) is asked
	// of no window, and is not kept.
	keys *keyStage
}

// token claims t, a token's stretch, its end -1 while to come.
func (c *claimSource) token(t [2]int) {
	c.keys.handed(c, t[0], t[1], false, 0, false)
	if t[1] < 0 || t[1] > c.keys.floor {
		c.tokens = append(c.tokens, t)
	}
}

// word claims w, a key, its separator's end and its value's end, the last -1
// while to come: a key and separator with no value, or an AWS secret's, whose
// value is claimed as a token is.
func (c *claimSource) word(w [3]int) {
	c.valued(w, w[1], [6]int{}, [2]int{}, nil)
}

// valued claims k as [claimSource.word] does, its value beginning at start, -1
// while to come, and claimed apart from its key ([claims]): the keys' stage
// reads every line between the two as the whole text's keys read it.
//
// THE CHECK OF A WINDOW DECIDED EARLY READS IT AS ONE STRETCH, from its key to
// its value's end ([keyStage.handed]), which holds every part of it an answer
// can read: a claim that comes late over a window's footprint is a miss
// whichever part the answers read.
func (c *claimSource) valued(k [3]int, start int, seams [6]int, also [2]int, runs [][2]int) {
	w := [12]int{k[0], k[1], start, k[2], seams[0], seams[1], seams[2], also[0], also[1], seams[3], seams[4], seams[5]}
	end := max(w[1], w[3], w[8])
	if w[1] < 0 || w[3] < 0 {
		end = -1
	}
	c.keys.handed(c, w[0], end, true, w[3], len(runs) > 0)
	if w[3] < 0 || max(w[3], w[8]) > c.keys.floor {
		c.words = append(c.words, w)
	}
	for _, r := range runs {
		c.runs = append(c.runs, [3]int{w[0], r[0], r[1]})
	}
}

// endToken sets the end of the token claimed last, claimed while open.
func (c *claimSource) endToken(end int) {
	t := &c.tokens[len(c.tokens)-1]
	t[1] = end
	c.keys.ended(c, end)
}

// endWord sets the ends of the word claimed last, claimed while open: its
// separator's, where sep is not -1, and its value's.
func (c *claimSource) endWord(sep, end int) {
	w := &c.words[len(c.words)-1]
	if sep >= 0 {
		w[1] = sep
	}
	w[3] = end
	c.keys.ended(c, end)
}

// prune lets go of the claims that end at or before lo.
func (c *claimSource) prune(lo int) {
	c.tokens = slices.DeleteFunc(c.tokens, func(t [2]int) bool { return t[1] >= 0 && t[1] <= lo })
	c.words = slices.DeleteFunc(c.words, func(w [12]int) bool { return w[3] >= 0 && max(w[3], w[8]) <= lo })
	c.runs = slices.DeleteFunc(c.runs, func(r [3]int) bool { return r[0] < lo })
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
	// outLen is how much a step has put since the next stage last took,
	// outPos where it stands in what the stage has written, outEdits the
	// edits in it, and passed whether the last edit of the input, still
	// open, has been written on. handed is the room for outEdits every
	// claims stage shares ([ruleStage.hand]).
	outLen   int
	outPos   int
	outEdits []edit
	handed   *[]edit
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

func (s *ruleStage) put(b []byte)            { s.in.put(b) }
func (s *ruleStage) putString(str string)    { s.in.putString(str) }
func (s *ruleStage) putRepeat(c byte, n int) { s.in.putRepeat(c, n) }

// took hands the stage the next stretch of its input, put already.
func (s *ruleStage) took(edits []edit) {
	s.in.took()
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
	// Let go of its input before the next stage closes, so the stages'
	// close holds one window at a time rather than every stage's.
	s.in.drop()
	if s.to != nil {
		s.to.close()
	}
}

// decideHeld decides all the stage holds as at its input's end, then stays
// open: its claims are known to where it decided, not past it.
func (s *ruleStage) decideHeld() {
	s.in.eof = true
	for s.step() {
	}
	s.in.eof, s.done = false, false
	if s.raw != nil {
		s.src.frontier = s.raw.at(s.pos)
	}
	if s.to != nil {
		s.to.decideHeld()
	}
}

// step decides one window of the input, if the stage holds one: [stageStep]
// past where it resumes, or the rest of a text that has ended, or, short of
// that, up to the last marker it holds ([ruleStage.markerToStepAt]).
func (s *ruleStage) step() bool {
	end := s.in.end
	if s.done {
		return false
	}
	if !s.in.eof && end < s.pos+stageStep {
		m, ok := s.markerToStepAt()
		if ok {
			s.stepAtMarker(m)
		}
		return ok
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
	s.resume(lo+at, eof)
	return true
}

// resume is where a step ends: the stage resumes at pos, and lets go of what
// no later window reads.
func (s *ruleStage) resume(pos int, eof bool) {
	s.pos = pos
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
}

// markerToStepAt is the marker a claims stage, or an AWS secret's reader,
// steps at short of a step ([ruleStage.stepAtMarker]): the last it holds whole
// past where it resumes, where that marker is open (its run still being read
// upstream) or what the stage holds stands for more of the text as written
// than a step and a piece do. A marker stands for a run of any length, so a
// stage waiting for a step of its own input would wait on the whole run, and
// the keys' stage would hold the text meanwhile. Stepping here, every token's
// and AWS secret's claim is known within the claims' lag, and so is every
// run's end but a Google token's before its dots ([ruleStage.carryToken],
// [keyStage.ended]). NEVER THE PASSWORD'S READER: it reads [MaxReach] past a
// key in its own input, past a marker standing for any length, so what it
// holds before a marker is decided only with what follows it.
func (s *ruleStage) markerToStepAt() (edit, bool) {
	if s.raw == nil || s.mode == reads || s.in.eof {
		return edit{}, false
	}
	es := s.raw.edits
	k := len(es) - 1
	for k >= 0 && es[k].end > s.in.end {
		k--
	}
	if k < 0 || es[k].at < s.pos {
		return edit{}, false
	}
	m := es[k]
	if m.rawEnd < 0 || s.raw.at(s.in.end)-s.raw.at(s.pos) > stageStep+pieceBytes {
		return m, true
	}
	return edit{}, false
}

// stepAtMarker decides the stage's input up to the marker m, writes it on
// through m, open or closed, and resumes past it.
//
// A DECISION THAT BEGINS BEFORE THE MARKER'S `[` READS NOTHING AT IT OR PAST
// IT: no token's pattern takes a `[` (the run token's subject is anything but
// one), nor an AWS secret's separator or value ([awsSecretByte]), nor an
// armour's label, which a run's end and a value's read past their dashes
// ([pemArmour]); so the window up to the `[`, the bracket included, decides
// it as the whole input does. No decision begins inside a marker either: an
// AWS secret's key a marker spells is followed by the `]`, which no separator
// takes. A run the stage reads on ends at the `[` too.
func (s *ruleStage) stepAtMarker(m edit) {
	lo := max(s.in.base, s.pos-stageReach)
	text := s.in.window(lo, m.end)
	edge := m.at - lo
	at := s.pos - lo
	if s.run.on {
		at = s.carry(text, lo, at, len(text), false)
		if s.run.on {
			panic(fmt.Sprintf("redact: a stage read a run on through a marker's bracket at %d of its input", m.at))
		}
	}
	at = s.scanTo(text, lo, at, edge)
	if lo+at <= s.pos {
		panic(fmt.Sprintf("redact: a stage's step at a marker decided nothing at %d of its input", s.pos))
	}
	s.resume(lo+at, false)
}

// scanTo is [ruleStage.scan] for a window that ends at a marker whose `[` is
// at edge: every decision that begins before edge is trusted, and the window
// is written on through its end.
func (s *ruleStage) scanTo(text []byte, lo, at, edge int) int {
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
	done, from := at, at
	for {
		ev, found := s.decide(text, from, prior)
		if !found || ev.key[0] >= edge {
			c := len(text)
			if found {
				c = min(c, ev.key[0])
			}
			return s.commit(text, lo, done, c)
		}
		if ev.key[1] > edge || ev.v[1] > edge {
			panic(fmt.Sprintf("redact: a decision at %d of a stage's input read through a marker's bracket at %d", lo+ev.key[0], lo+edge))
		}
		done, from = s.apply(text, lo, done, ev, &prior)
	}
}

// flush writes on what the step wrote.
func (s *ruleStage) flush() {
	if s.mode != replaces || s.outLen == 0 && len(s.outEdits) == 0 {
		return
	}
	edits := s.outEdits
	s.outLen, s.outEdits = 0, nil
	s.to.took(edits)
	if s.handed != nil && cap(edits) > cap(*s.handed) {
		*s.handed = edits[:0]
	}
}

// hand adds e to the edits the stage hands on with what it wrote, in the room
// the claims stages share: ONE STAGE AT A TIME HOLDS ANY, since a stage steps
// only within the one before it handing on, which the next copies before it
// steps ([rawMap.add]), and each step hands on all it wrote ([ruleStage.flush]).
// So the claims hold one stage's room, a piece's edits, rather than each its own.
func (s *ruleStage) hand(e edit) {
	if s.outEdits == nil {
		s.outEdits = (*s.handed)[:0]
	}
	s.outEdits = append(s.outEdits, e)
}

// event is one decision of a stage's rule: where what decides it begins and
// ends (the match, a value rule's key and separator), what it replaces, empty
// where the key names nothing, and a password's value's seams and its second
// range's ([aValue]).
type event struct {
	key, v, also [2]int
	seams        [6]int
	// runs are the stretches of a key's last run the value was read past
	// ([aValue]).
	runs [][2]int
	// ran says a password's value is its run read in the whole text
	// ([aValue]), which [ruleStage.trusted] decides by: read with the
	// decision, never again, since a read of a value that ends at an
	// escaped break reads the key on the next line too.
	ran bool
}

// seamsAt is ev's value's seams in the text as written, each or 0
// ([aValue]).
func (s *ruleStage) seamsAt(lo int, ev event) [6]int {
	var at [6]int
	for i, seam := range ev.seams {
		if seam > 0 {
			at[i] = s.raw.at(lo + seam)
		}
	}
	return at
}

// runsAt is ev's value's key's runs in the text as written ([aValue]).
func (s *ruleStage) runsAt(lo int, ev event) [][2]int {
	var at [][2]int
	for _, r := range ev.runs {
		at = append(at, [2]int{s.raw.at(lo + r[0]), s.raw.at(lo + r[1])})
	}
	return at
}

// decide is the stage's rule's first decision at or after at.
func (s *ruleStage) decide(text []byte, at int, prior [][2]int) (event, bool) {
	if s.mode == finds {
		start, end, found := findIn(s.r, text, at)
		return event{key: [2]int{start, end}, v: [2]int{end, end}}, found
	}
	key, v, found := decideReadIn(s.r, text, at, prior)
	return event{key: key, v: [2]int{v.from, v.to}, seams: v.seams(), also: v.also, runs: v.runs, ran: v.ran}, found
}

// scan decides the window from at, limit being where the decisions it can
// trust end, and is where the stage resumes.
func (s *ruleStage) scan(text []byte, lo, at, limit int, eof bool) int {
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
		if pending >= 0 || s.mode == finds && found && !eof && awsSpacesOpen(text, ev.key[1]) {
			// An AWS secret's key whose separator's spaces run to the
			// window's end: whether its pattern matches there, and where
			// it ends, is read where they end.
			p := pending
			if found && awsSpacesOpen(text, ev.key[1]) && (p < 0 || ev.key[0] < p) {
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
		case eof || s.trusted(text, ev):
			done, from = s.apply(text, lo, done, ev, &prior)
		case ev.also[1] > ev.also[0]:
			return s.commit(text, lo, done, min(ev.key[0], limit))
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
//
// THE MIDDLE ONE SAYS WHEN: the values end in order, so the rest outnumber
// them exactly where the middle one is past the reach, and a value costs the
// same whether the reach holds five values or the nine thousand of `pwd=x`
// lines: TestAStageDenseInPasswordValuesCostsWhatItsValuesCost holds it.
func priorWith(prior [][2]int, v [2]int) [][2]int {
	prior, _ = priorVisits(prior, v)
	return prior
}

// priorVisits is [priorWith], and the values it visited to make it: the middle
// one it read, and on a copy, those it passed over and those it moved down. The
// count is what the test of a value's cost judges, not the time a run of them
// takes, which moved with how busy the runner was.
func priorVisits(prior [][2]int, v [2]int) ([][2]int, int) {
	prior = append(prior, v)
	floor := v[0] - stageReach
	if prior[len(prior)/2][1] >= floor {
		return prior, 1
	}
	k := 0
	for prior[k][1] < floor {
		k++
	}
	return append(prior[:0], prior[k:]...), 1 + len(prior)
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
func (s *ruleStage) commit(text []byte, lo, at, c int) int {
	c = max(c, at)
	s.pass(text, lo, at, c)
	return c
}

// trusted reports whether the window holds every read that decided ev.
func (s *ruleStage) trusted(text []byte, ev event) bool {
	switch {
	case s.mode == finds:
		// Its pattern's match ends at what is no space, read in the
		// window ([ruleStage.pendingSeparator]).
		return !awsSpacesOpen(text, ev.key[1])
	case s.r.value != nil && ev.v[1] <= ev.v[0]:
		// A key that names nothing: its value was looked for as far as
		// [MaxReach] past the key, and the first byte of one read as far
		// again ([valueEnd]).
		return ev.key[0]+2*stageReach <= len(text) && (s.mode != reads || ev.key[1]+stageReach <= len(text))
	}
	decided := max(ev.v[1], ev.also[1])
	switch {
	case s.r.value != nil && s.r.valueIn == nil:
		// A password's value [valueEnd] ends is decided where it stops
		// it, past the backslashes before an escaped break, which it
		// reads back over; one a wrapping's suffix cuts, by its key.
		if decided < len(text) && text[decided] == '\\' && ev.ran {
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
func (s *ruleStage) apply(text []byte, lo, done int, ev event, prior *[][2]int) (int, int) {
	switch s.mode {
	case finds:
		s.src.word([3]int{s.raw.at(lo + ev.key[0]), s.raw.at(lo + ev.key[1]), s.raw.at(lo + ev.key[1])})
		return ev.key[1], ev.key[1]
	case reads:
		if ev.v[1] > ev.v[0] {
			var also [2]int
			if ev.also[1] > ev.also[0] {
				also = [2]int{s.raw.at(lo + ev.also[0]), s.raw.at(lo + ev.also[1])}
			}
			s.src.valued([3]int{s.raw.at(lo + ev.key[0]), s.raw.at(lo + ev.key[1]), s.raw.at(lo + ev.v[1])}, s.raw.at(lo+ev.v[0]), s.seamsAt(lo, ev), also, s.runsAt(lo, ev))
			*prior = priorWith(*prior, ev.v)
			if ev.also[1] > ev.also[0] {
				*prior = priorWith(*prior, ev.also)
				return ev.also[1], ev.also[1]
			}
			return ev.v[1], ev.v[1]
		}
		s.src.valued([3]int{s.raw.at(lo + ev.key[0]), s.raw.at(lo + ev.key[1]), s.raw.at(lo + ev.key[1])}, s.raw.at(lo+ev.key[1]), s.seamsAt(lo, ev), [2]int{}, nil)
		return done, beyond(text, ev.key[0])
	}
	if ev.v[1] <= ev.v[0] {
		return done, beyond(text, ev.key[0])
	}
	s.pass(text, lo, done, ev.v[0])
	s.replace(lo+ev.v[0], lo+ev.v[1], func(b io.StringWriter) { writeRule(s.r, b, text[ev.v[0]:ev.v[1]]) }, true)
	if s.r.value != nil {
		*prior = priorWith(*prior, ev.v)
	}
	if a := ev.also; a[1] > a[0] {
		s.pass(text, lo, ev.v[1], a[0])
		s.replace(lo+a[0], lo+a[1], func(b io.StringWriter) { writeRule(s.r, b, text[a[0]:a[1]]) }, true)
		*prior = priorWith(*prior, a)
		return a[1], a[1]
	}
	return ev.v[1], ev.v[1]
}

// pass writes the window's input from a to b as it stands, with the edits in
// it, handing the next stage a piece at a time ([pieceBytes]).
func (s *ruleStage) pass(text []byte, lo, a, b int) {
	if s.mode != replaces {
		return
	}
	for a < b {
		c := min(b, a+pieceBytes-s.outLen)
		if c <= a {
			s.flush()
			continue
		}
		if s.raw != nil {
			k, _ := slices.BinarySearchFunc(s.raw.edits, lo+a, func(e edit, i int) int { return e.at - i })
			for ; k < len(s.raw.edits) && s.raw.edits[k].at < lo+c; k++ {
				e := s.raw.edits[k]
				s.hand(edit{s.outPos + e.at - lo - a, s.outPos + e.end - lo - a, e.raw, e.rawEnd})
				s.passed = e.rawEnd < 0
			}
		}
		s.to.put(text[a:c])
		s.outLen += c - a
		s.outPos += c - a
		a = c
	}
}

// replace writes what the rule makes of its input from start to end, written
// by write, and claims it; closed is whether end is where it ends, rather than
// where it is read to so far.
func (s *ruleStage) replace(start, end int, write func(io.StringWriter), closed bool) {
	n := s.outLen
	write(stageOut{s})
	if s.raw == nil {
		return
	}
	rawEnd := -1
	if closed {
		rawEnd = s.raw.at(end)
	}
	raw := s.raw.at(start)
	s.src.token([2]int{raw, rawEnd})
	written := s.outLen - n
	s.hand(edit{s.outPos, s.outPos + written, raw, rawEnd})
	s.outPos += written
	if s.outLen >= pieceBytes {
		s.flush()
	}
}

// stageOut writes what a rule makes of what it replaces on to the stage's
// next one ([ruleStage.replace]).
type stageOut struct{ s *ruleStage }

func (o stageOut) WriteString(str string) (int, error) {
	o.s.to.putString(str)
	o.s.outLen += len(str)
	return len(str), nil
}

// closeRun claims a run's end, end in the stage's input, where it was claimed
// while open.
func (s *ruleStage) closeRun(end int) {
	if s.raw == nil {
		return
	}
	rawEnd := s.raw.at(end)
	if s.mode == reads {
		s.src.endWord(-1, rawEnd)
		return
	}
	s.src.endToken(rawEnd)
	s.to.closeEdit(rawEnd)
}

// open begins reading on a run that ev, decided where it begins, runs on past
// the window: writes the input up to it, what it replaces so far, and is where
// the stage resumes, the end of what the window decides, and whether ev is a
// run the stage can read on.
func (s *ruleStage) open(text []byte, lo, at int, ev event, limit int) (int, bool) {
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
		s.src.valued([3]int{s.raw.at(lo + ev.key[0]), s.raw.at(lo + ev.key[1]), -1}, s.raw.at(lo+start), s.seamsAt(lo, ev), [2]int{}, s.runsAt(lo, ev))
	default:
		s.pass(text, lo, at, start)
		if s.r.value != nil {
			s.replace(lo+start, lo+limit, func(b io.StringWriter) { writeStretch(&s.run.w, b, "", text[start:limit-held]) }, false)
		} else {
			s.replace(lo+start, lo+limit, func(b io.StringWriter) { _, _ = b.WriteString(s.r.with) }, false)
		}
	}
	return limit, true
}

// carry reads on the stage's open run from at, and is where the stage
// resumes: where the run ends, or the end of what the window decides while it
// runs on.
func (s *ruleStage) carry(text []byte, lo, at, limit int, eof bool) int {
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
func (s *ruleStage) carryToken(text []byte, lo, at, limit int, eof bool) int {
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
func (s *ruleStage) carryValue(text []byte, lo, at, limit int, eof bool) int {
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
		n := s.outLen
		s.run.w.close(stageOut{s})
		s.outPos += s.outLen - n
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
		k := min(n, pieceBytes-s.outLen)
		if k <= 0 {
			s.flush()
			continue
		}
		s.to.putRepeat(c, k)
		s.outLen += k
		s.outPos += k
		n -= k
	}
}

// writeValue writes the next stretch of a value: the backslashes held before
// it, as few as decide its pieces, and rest.
func (s *ruleStage) writeValue(held int, rest []byte) {
	if s.mode != replaces {
		return
	}
	n := s.outLen
	writeStretch(&s.run.w, stageOut{s}, `\\`[:min(held, 2)], rest)
	s.outPos += s.outLen - n
}

// carryTrail reads on a reader's key whose whitespace runs on
// ([passwordTrail]), and claims it once it ends.
func (s *ruleStage) carryTrail(text []byte, lo, at int, eof bool) int {
	i := at + passwordTrailAt().FindIndex(text[at:])[1]
	if !eof && (i == len(text) || text[i] == '\\' && i+1 == len(text)) {
		return i
	}
	s.run = openRun{}
	end := s.raw.at(lo + i)
	s.src.endWord(end, end)
	return i
}

// pendingSeparator is where an AWS secret's key begins whose separator's
// spaces run to the window's end, so that whether the key's pattern matches
// there, and where, is still to be read, or -1.
func (s *ruleStage) pendingSeparator(text []byte, at int) int {
	loc := s.r.pending.FindIndex(text[at:])
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
func (s *ruleStage) openSeparator(text []byte, lo, p, limit int) int {
	space := awsSpacesBack(text, p)
	head := string(text[p:space])
	s.run = openRun{on: true, separator: true, start: lo + p, head: head}
	if loc := s.r.pattern.FindStringIndex(head + " "); loc != nil && loc[0] == 0 && loc[1] == len(head)+1 {
		s.run.claimed = true
		s.src.word([3]int{s.raw.at(lo + p), -1, -1})
	}
	return max(limit, space)
}

// carrySeparator reads on an AWS secret's separator's spaces, and claims the
// key and its separator once the pattern's match there is decided.
func (s *ruleStage) carrySeparator(text []byte, lo, at, limit int, eof bool) int {
	// A run resumed where a window ended reads an escaped break the end
	// parted from its backslashes, which are all a run holds before an `n`.
	for at > 0 && text[at-1] == '\\' {
		at--
	}
	i := awsSpaces(text, at)
	if awsSpacesOpen(text, i) && !eof {
		return max(i, limit)
	}
	if s.run.after {
		s.claimSeparator(lo + i)
		return i
	}
	// The pattern decides by where the spaces end alone, not how many
	// there are: a separator's spaces are none or more of [awsSpace], or
	// one or more.
	tail := text[i:min(len(text), i+8)]
	if len(tail) < 8 && !eof {
		return i
	}
	head := s.run.head + " "
	loc := s.r.pattern.FindStringIndex(head + string(tail))
	if loc == nil || loc[0] != 0 {
		// Claimed only where the pattern matches whatever follows, which
		// it then does.
		s.run = openRun{}
		return i
	}
	// The spaces after its operator end where the window's read of them
	// does, which the tail's may not.
	end := awsSpaces(text, i+loc[1]-len(head))
	if awsSpacesOpen(text, end) && !eof {
		// The spaces after its operator run on too.
		s.run.after = true
		return s.carrySeparator(text, lo, end, limit, eof)
	}
	s.claimSeparator(lo + end)
	return end
}

// awsSpaces is where the spaces of an AWS secret's separator that begin at i
// end ([awsSpace]).
func awsSpaces(text []byte, i int) int {
	for i < len(text) {
		switch {
		case text[i] == ' ' || text[i] == '\t':
			i++
		case text[i] == '\\':
			j := i
			for j < len(text) && text[j] == '\\' {
				j++
			}
			if j == len(text) || text[j] != 'n' && text[j] != 'r' {
				return i
			}
			i = j + 1
		default:
			return i
		}
	}
	return i
}

// awsSpacesOpen reports whether spaces of an AWS secret's separator that end
// at i may run on past the window: it ends there, or in backslashes an `n`
// written next makes an escaped line break of.
func awsSpacesOpen(text []byte, i int) bool {
	for i < len(text) && text[i] == '\\' {
		i++
	}
	return i == len(text)
}

// awsSpacesBack is where the spaces of an AWS secret's separator that run to
// text's end, a backslash that may open one included, begin, p at most.
func awsSpacesBack(text []byte, p int) int {
	i := len(text)
	for i > p && text[i-1] == '\\' {
		i--
	}
	for i > p {
		switch {
		case text[i-1] == ' ' || text[i-1] == '\t':
			i--
		case i-2 >= p && text[i-2] == '\\' && (text[i-1] == 'n' || text[i-1] == 'r'):
			i--
			for i > p && text[i-1] == '\\' {
				i--
			}
		default:
			return i
		}
	}
	return i
}

// claimSeparator claims an AWS secret's key and its separator, ending at end in
// the stage's input, once the pattern's match there is decided.
func (s *ruleStage) claimSeparator(end int) {
	rawEnd := s.raw.at(end)
	if s.run.claimed {
		s.src.endWord(rawEnd, rawEnd)
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
	// pending is the blocks read and not yet written, in order and apart,
	// as read: each is cut once it is whole, as it is written; forward every
	// block read forward from a BEGIN that a later read may ask of; closed
	// where each END such a block reached begins.
	pending [][2]int
	forward [][2]int
	closed  []int
	sources []*claimSource
	to      sink
	done    bool
	// floor is where its next window begins: no claim that ends at it or
	// before it is asked of again ([claimSource]).
	floor int
	// arrived is how much of the text the stages have been handed, the
	// piece the claims stages are being handed included: what a claim's
	// change trails the text by is arrived less where it lies.
	arrived int
	// hold is how far the text runs past a window before the stage decides
	// it whatever its claims say ([holdBytes]); early the windows it so
	// decided while the claims over them were still to be known, each its
	// start and end and the FOOTPRINT of what it asked of them, the hull
	// of the claims its answers read ([claims.asked]), waiting for them
	// ([keyStage.handed]).
	hold  int
	early [][4]int
	// decided is every window decided early whose claims are still to be
	// known, footprint or none: its end and what the end had written when
	// it was decided ([keyStage.finalWritten]); out is the end.
	decided []earlyWrite
	out     *endSink
	// beginsEarly are the windows decided early whose answers read where
	// claims begin, each its start and end and the hull of where it asked
	// ([claims.askedBegins]): a claim handed late that begins there is a
	// miss ([keyStage.handed]); an end set late is not, moving no claim's
	// beginning ([keyStage.ended]). Apart from early, so that an end set
	// late beside the line a block ends on is read against what the window
	// read of what claims cover alone.
	beginsEarly [][4]int
	// forgot is the furthest end of a block read forward that the stage
	// let go ([keyStage.write]), and wrote the last block it wrote, as read:
	// the cut of the next block reads where it ends ([claims.opensAlike]).
	forgot int
	wrote  [2]int
	// missed is the miss the stage found, after which it decides nothing.
	missed *decidedEarly
	// observe is told what the stage and its claims do, where a test sets
	// it; nil in every stream.
	observe *keyObserver
}

// keyObserver is what a test is told of a keys' stage: each claim that
// changes, the source and the place it changed at (where it begins, or the end
// set); each block it writes; and each window it decides early.
type keyObserver struct {
	changed func(c *claimSource, place int)
	wrote   func(block [2]int)
	decided func(lo, hi int)
}

// A WINDOW DECIDED EARLY IS CHECKED BY THE CLAIMS THAT COME AFTER IT. The keys'
// stage decides a window once the text has run [holdBytes] past it, whatever
// its claims' frontier says ([keyStage.frontier]), so the claims over it may
// still change: a claim handed, or an end set, more than the hold past where
// it lies is a LATE change, the only kind that can concern such a window. Its
// answers depend on the claims over its footprint alone, so a late change that
// meets it is a MISS and the stream is refused ([ErrDecidedEarly]). A claim
// the window read open read as running past it ([keyStage.claimsIn]), so its
// end set inside the window is checked over the stretch from that end to where
// the window read it to ([keyStage.ended]). One that meets none decided what
// the whole text does, by induction over the windows in order.

// handed checks a claim handed at a, its extent ending at end (-1 while to
// come), a word's value ending at value, against the windows decided early;
// a late word against the blocks read forward the stage let go; and a late
// word carrying a key's runs against where the stage has read to, the window
// that read its key having written none of them ([keyStage.runsIn]).
func (k *keyStage) handed(c *claimSource, a, end int, word bool, value int, runs bool) {
	if o := k.observe; o != nil && o.changed != nil {
		o.changed(c, a)
	}
	if k.arrived-a <= k.hold || k.missed != nil {
		return
	}
	if runs && a < k.read {
		k.miss(a)
		return
	}
	if word && a < k.forgot && (value < 0 || value > k.floor) {
		// A word whose key a block read forward may hold, which the
		// stage let go before it came: a later window reads its value
		// as claimed where the whole text's block cuts it ([claims.take]).
		k.miss(a)
		return
	}
	for _, windows := range [2][][4]int{k.early, k.beginsEarly} {
		for _, w := range windows {
			if a > w[1] {
				continue
			}
			to := w[1] + 1
			if end >= 0 {
				to = min(end, to)
			}
			if a < w[3] && w[2] < to {
				k.miss(w[0])
				return
			}
		}
	}
}

// ended checks the end e set on a claim against each window decided early that
// it ends inside, kept in k.early as its start, end hi and footprint. The
// window read the claim open, as running to one past hi ([keyStage.claimsIn]),
// so it is a miss where [e, hi+1), the stretch the claim was read to cover and
// does not, meets the footprint ([claims.asked]). One the claim ends past read
// it as the whole text does. Not from where the claim begins: before e the
// window answered as with the claim ending at e, since [claims.any],
// [claims.names] and [claims.shortOfNames] agree there. The exception is a
// password's value read forward from a BEGIN ([claims.build]), cut to key and
// separator when open, whole when it ends inside the block; that window asked
// at the block's end, so its footprint meets [e, hi+1) ([keyStage.settle]).
// Counting from a would refuse windows that asked only before e.
func (k *keyStage) ended(c *claimSource, e int) {
	if o := k.observe; o != nil && o.changed != nil {
		o.changed(c, e)
	}
	if k.arrived-e <= k.hold || k.missed != nil {
		return
	}
	for _, w := range k.early {
		if e > w[1] {
			continue
		}
		if e < w[3] && w[2] <= w[1] {
			k.miss(w[0])
			return
		}
	}
}

// miss is a miss found at, in the text (the window's start, or a late word's
// key): the first the stage keeps, named from the start of the first window
// it decided early that still waits on its claims where that comes before.
//
// NOT FROM at ALONE: the stream stops at the first miss it finds, and a window
// decided early before it whose late claim has yet to come may have decided
// otherwise too, unseen. Every window before the first that waits had its
// claims known when it was decided, or has had them since, so what the stream
// wrote is the whole text's up to the byte named, and from there on it is not
// known to be.
func (k *keyStage) miss(at int) {
	if k.missed != nil {
		return
	}
	for _, w := range k.early {
		at = min(at, w[0])
	}
	for _, w := range k.beginsEarly {
		at = min(at, w[0])
	}
	k.missed = &decidedEarly{at: at}
}

// earlyWrite is a window decided early: its end in the text, and how many
// bytes the end had written when it was decided.
type earlyWrite struct {
	end     int
	written int64
}

// decide keeps w until the claims over its window are known: the oldest two
// merged, keeping the earlier write, where [earlyWindows] wait already.
func (k *keyStage) decide(w earlyWrite) {
	if len(k.decided) == earlyWindows {
		a, b := k.decided[0], k.decided[1]
		k.decided[1] = earlyWrite{max(a.end, b.end), min(a.written, b.written)}
		k.decided = slices.Delete(k.decided, 0, 1)
	}
	k.decided = append(k.decided, w)
}

// finalWritten is how many bytes of what the end wrote are final whatever
// the claims still to come say: those it had written when the first window
// still waiting on its claims was decided, since what it wrote before then
// came from text the stages wrote before that window. Every window counts,
// not only one that asked of the claims: a late word can change a block it
// read forward and let go ([keyStage.handed]).
func (k *keyStage) finalWritten() int64 {
	n := k.out.n
	for _, w := range k.decided {
		n = min(n, w.written)
	}
	return n
}

// wait keeps a window decided early, w, until the claims over it are known:
// the oldest two merged where [earlyWindows] wait already.
func (k *keyStage) wait(w [4]int) {
	k.early = waitIn(k.early, w)
}

// waitIn is windows with w kept too, the oldest two merged where
// [earlyWindows] wait already.
func waitIn(windows [][4]int, w [4]int) [][4]int {
	if len(windows) == earlyWindows {
		a, b := windows[0], windows[1]
		windows[1] = [4]int{min(a[0], b[0]), max(a[1], b[1]), min(a[2], b[2]), max(a[3], b[3])}
		windows = slices.Delete(windows, 0, 1)
	}
	return append(windows, w)
}

// settle lets go of the windows decided early whose claims are all known.
func (k *keyStage) settle() {
	f := k.frontier()
	k.early = slices.DeleteFunc(k.early, func(w [4]int) bool { return w[1] < f })
	k.beginsEarly = slices.DeleteFunc(k.beginsEarly, func(w [4]int) bool { return w[1] < f })
	k.decided = slices.DeleteFunc(k.decided, func(w earlyWrite) bool { return w.end < f })
}

// take hands the stage the next stretch of the text.
func (k *keyStage) take(text []byte) {
	k.in.add(text)
	k.settle()
	for k.step() {
	}
}

// close ends the text.
func (k *keyStage) close() {
	k.in.eof = true
	for k.step() {
	}
	k.in.drop()
	if k.missed == nil {
		k.to.close()
	}
}

// decideHeld decides all the stage holds as at the text's end, its claims
// decided to there, and stays open ([pipeline.decideHeld]).
func (k *keyStage) decideHeld() {
	k.in.eof = true
	for k.step() {
	}
	k.in.eof, k.done = false, false
	k.settle()
	if k.missed == nil {
		k.to.decideHeld()
	}
}

// frontier is where what the rules claim stops being known ([claimSource]).
func (k *keyStage) frontier() int {
	f := math.MaxInt
	for _, s := range k.sources {
		f = min(f, s.frontier)
	}
	return f
}

// step reads the armours of one window, if the stage holds one, and writes the
// text up to where no block read later can reach, each block it writes cut
// apart from the gutter of a password key's line ([keyStage.write]). Either
// reads the claims over the window, so a step that reads an armour, or writes
// a block, waits for them, or decides early once the text has run the hold
// past the window ([keyStage.handed]).
func (k *keyStage) step() bool {
	end := k.in.end
	hi := k.read + stageAdvance + keyAhead
	if k.done || k.missed != nil || !k.in.eof && end < hi {
		return false
	}
	eof := k.in.eof && end <= hi
	if eof {
		hi = end
	}
	lo := max(k.in.base, min(k.pos, k.read-keyBehind, k.opening()))
	readTo := hi - keyAhead
	if eof {
		readTo = hi
	}
	writeTo := readTo - keyReach
	if eof {
		writeTo = hi
	}
	text := k.in.window(lo, hi)
	armour := bytes.Contains(text[k.read-lo:min(len(text), readTo-lo+len(keyArmour)+64)], keyArmourBytes)
	var c *claims
	early := false
	foot, starts := [2]int{len(text) + 1, -1}, [2]int{len(text) + 1, -1}
	runs := k.runsIn(k.read, readTo)
	if _, n := k.final(writeTo); armour || n > 0 || len(runs) > 0 {
		// At the input's end every claims stage has decided all it holds,
		// at a flush as at Close ([keyStage.decideHeld]).
		early = !k.in.eof && k.frontier() <= hi
		if early && k.arrived-k.hold <= hi {
			return false
		}
		// The keys' reading is the whole text's ([readKeys]), on a string:
		// a copy of the window, made only where it reads an armour or
		// writes a block.
		str := string(text)
		c = k.claimsIn(str, lo)
		if early {
			k.decide(earlyWrite{lo + len(text), k.out.n})
			c.footprint, c.begins = &foot, &starts
			if o := k.observe; o != nil && o.decided != nil {
				o.decided(lo, lo+len(text))
			}
		}
		if !armour {
			c.take(k.forwardIn(lo))
		} else {
			k.readArmours(str, lo, readTo, c, early)
			if k.missed != nil {
				return false
			}
		}
		for _, r := range runs {
			if r[0] < k.pos {
				// Written already, as no whole text's block is: a
				// reading the whole text does not have ([readArmours]).
				k.miss(lo)
				return false
			}
		}
		k.pending = mergeSpans(append(k.pending, runs...))
	}
	k.read = readTo
	k.write(text, lo, writeTo, c)
	if early && foot[0] < foot[1] {
		k.wait([4]int{lo, lo + len(text), lo + foot[0], lo + foot[1]})
	}
	if early && starts[0] < starts[1] {
		k.beginsEarly = waitIn(k.beginsEarly, [4]int{lo, lo + len(text), lo + starts[0], lo + starts[1]})
	}
	k.done = eof
	return true
}

// readArmours reads the blocks of the armours that begin in [k.read, readTo)
// beside c, early where it is still to be known, and keeps them as read.
//
// A span an early window reads that ends at or before its start, or begins
// before what the stage has written, is a reading of claims the whole text
// does not have (a token's run ends at an armour's dashes, so no claim covers
// a block's end from before its start; [ruleStage.carryToken]). It is a miss
// and is never written: no code reads one now, but the check stays at the one
// place such a span would be written, since it would slice the text backwards
// and panic the stage and the goroutine the stream runs in.
func (k *keyStage) readArmours(text string, lo, readTo int, c *claims, early bool) {
	r := keyReading{open: -1, forward: k.forwardIn(lo)}
	for _, e := range k.closed {
		r.closed = append(r.closed, e-lo)
	}
	from, to := k.read-lo, readTo-lo
	carried := len(r.forward)
	begins := keyBegin().FindAllStringIndex(text, -1)
	for _, b := range begins {
		if from <= b[0] && b[0] < to {
			r.begin(text, b, c)
		}
	}
	c.take(r.forward)
	spans := append(slices.Clone(r.forward[carried:]), r.ends(text, c, begins, keyEnd().FindAllStringIndex(text, -1), from, to)...)
	for _, s := range c.shortOfNames(spans) {
		if early && (s[1] <= s[0] || s[0]+lo < k.pos) {
			k.miss(lo)
			return
		}
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

// runsIn is every stretch of a key's last run the claims carry beside a key
// that begins in [from, to) ([claimSource]): the window that reads there writes
// each as a block, as the whole text's keys do ([claims.keyRuns]). Each lies
// within [MaxReach] of its key, so the window holds it, and past what the
// stage has written.
func (k *keyStage) runsIn(from, to int) [][2]int {
	var runs [][2]int
	for _, s := range k.sources {
		for _, r := range s.runs {
			if from <= r[0] && r[0] < to {
				runs = append(runs, [2]int{r[1], r[2]})
			}
		}
	}
	return runs
}

// forwardIn is the blocks read forward the stage keeps, placed in the window
// that begins at lo.
func (k *keyStage) forwardIn(lo int) [][2]int {
	var out [][2]int
	for _, f := range k.forward {
		out = append(out, [2]int{f[0] - lo, f[1] - lo})
	}
	return out
}

// claimsIn is what the rules claim in the window text, which begins at lo:
// every claim that reaches into it, placed in it, its end at most one past the
// window's where it is still to come, and a value that begins past the window
// beginning at its end, so that it reads as a value, not as none.
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
			if (w[3] < 0 || w[3] > lo) && w[0] <= hi {
				start := clip(w[2])
				if w[2] > hi && w[3] != w[2] {
					start = len(text)
				}
				var seams [6]int
				for i, seam := range [6]int{w[4], w[5], w[6], w[9], w[10], w[11]} {
					if seam > lo && seam <= hi {
						seams[i] = seam - lo
					}
				}
				var also [2]int
				if w[8] > lo && w[7] <= hi {
					also = [2]int{max(w[7]-lo, 0), clip(w[8])}
				}
				c.passwords = append(c.passwords, [12]int{w[0] - lo, clip(w[1]), start, clip(w[3]), seams[0], seams[1], seams[2], also[0], also[1], seams[3], seams[4], seams[5]})
			}
		}
	}
	c.tokens = mergeSpans(c.tokens)
	c.build()
	return c
}

// opening is where the line the first block still to be written begins on can
// begin at the furthest ([segmentStart]): the stage holds the text and the
// claims from there, which the cut of that block reads ([claims.opensAlike]).
func (k *keyStage) opening() int {
	if len(k.pending) == 0 {
		return math.MaxInt
	}
	return max(0, k.pending[0][0]-MaxKeyBlockBytes)
}

// final is writeTo, moved back to where a block that runs across it begins, and
// how many of the blocks still to be written end at or before it: those it
// writes, each whole now, since no block read later reaches before writeTo.
func (k *keyStage) final(writeTo int) (int, int) {
	for _, s := range k.pending {
		if s[0] < writeTo && writeTo < s[1] {
			writeTo = s[0]
		}
	}
	n := 0
	for n < len(k.pending) && k.pending[n][1] <= writeTo {
		n++
	}
	return writeTo, n
}

// write writes the text from k.pos to writeTo, moved back to where a block that
// runs across it begins, each block in it cut apart from the gutter of a
// password key's line as the whole text's are ([claims.apartIn]), beside c,
// the claims over the window, and every block read so far, and each piece
// replaced by [keyMarker], a piece of the text at a time ([pieceBytes]).
func (k *keyStage) write(text []byte, lo, writeTo int, c *claims) {
	writeTo, whole := k.final(writeTo)
	var cut [][2]int
	if whole > 0 {
		var final, known [][2]int
		if k.wrote[1] > lo {
			known = append(known, [2]int{max(k.wrote[0], lo) - lo, k.wrote[1] - lo})
		}
		for i, s := range k.pending {
			known = append(known, [2]int{s[0] - lo, s[1] - lo})
			if i < whole {
				final = append(final, [2]int{s[0] - lo, s[1] - lo})
			}
		}
		for _, s := range c.apartIn(final, known) {
			cut = append(cut, [2]int{s[0] + lo, s[1] + lo})
		}
		k.wrote = k.pending[whole-1]
	}
	put := 0
	piece := func(s []byte) {
		for len(s) > 0 {
			n := min(len(s), pieceBytes-put)
			k.to.put(s[:n])
			put += n
			s = s[n:]
			if put >= pieceBytes {
				k.to.took(nil)
				put = 0
			}
		}
	}
	at := k.pos
	for _, s := range cut {
		if o := k.observe; o != nil && o.wrote != nil {
			o.wrote(s)
		}
		piece(text[at-lo : s[0]-lo])
		piece(keyMarkerBytes)
		at = s[1]
	}
	if writeTo > at {
		piece(text[at-lo : writeTo-lo])
		at = writeTo
	}
	if put > 0 {
		k.to.took(nil)
	}
	k.pending = slices.Delete(k.pending, 0, whole)
	k.pos = max(k.pos, at)
	// Let go of what no later read asks of: the text and the claims behind
	// the next window and the opening of the next block's line, and the
	// blocks read forward behind it but the last, and any that holds a
	// password's key whose claim reaches into it.
	k.floor = min(k.pos, k.read-keyBehind, k.opening())
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
		k.forward = slices.DeleteFunc(k.forward[:len(k.forward)-1], func(f [2]int) bool {
			if f[1] <= keepFrom {
				k.forgot = max(k.forgot, f[1])
				return true
			}
			return false
		})
		k.forward = append(k.forward, last)
	}
}

// keyArmourBytes and keyMarkerBytes are [keyArmour] and [keyMarker] as the
// keys' stage reads and writes its window.
var (
	keyArmourBytes = []byte(keyArmour)
	keyMarkerBytes = []byte(keyMarker)
)
