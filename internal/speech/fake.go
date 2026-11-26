package speech

import (
	"context"
	"encoding/binary"
	"strings"
	"sync"
	"time"
)

// Fake is a deterministic recognizer for tests and demonstrations.
//
// It hears levels, not words: each [FakeWordAudio] of audio whose peak
// reaches [FakeVoiced] reveals the next word of its script as a partial, a
// sentence whose words are all revealed becomes its punctuated final, and
// [Stream.Finish] makes the rest a final. Silence reveals nothing.
//
// It refuses what the real one refuses (a language other than [Lang], odd
// PCM, past its capacity), records what it was sent, and fails on request
// ([Fake.Fail]): busy at Open, a stream lost mid-dictation, a recognizer
// length limit, or a stall that never answers again.
type Fake struct {
	script []string

	mu        sync.Mutex
	capacity  int
	live      int
	opened    int
	bytes     int
	failure   FakeFailure
	afterN    int
	prepare   func(ctx context.Context, progress func(float64)) error
	available Availability
}

var _ Streamer = (*Fake)(nil)

// FakeWordAudio is how much voiced audio reveals one word: 300 ms, the
// cadence the default model's partials arrive at.
const FakeWordAudio = 3 * BytesPerSecond / 10

// FakeVoiced is the sample peak a chunk of audio needs to count as speech.
const FakeVoiced = 1000

// FakeFailure is a failure the fake produces on request.
type FakeFailure int

// The failures [Fake.Fail] arms.
const (
	// FakeNone fails nothing.
	FakeNone FakeFailure = iota
	// FakeBusy answers [ErrBusy] at Open.
	FakeBusy
	// FakeDrop loses the stream ([ErrLost]) after n results.
	FakeDrop
	// FakeLength ends the stream ([ErrTooLong]) once it was sent n bytes.
	FakeLength
	// FakeStall stops answering after n results: no result, no finish.
	FakeStall
)

// NewFake builds a fake reciting script, one sentence an entry.
func NewFake(script ...string) *Fake {
	return &Fake{script: script, available: Availability{Available: true}}
}

// SetCapacity caps live streams, as [Local]'s capacity does; 0 is no cap.
func (f *Fake) SetCapacity(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.capacity = n
}

// Fail arms a failure for every later Open; n is its count (results or
// bytes, see [FakeFailure]).
func (f *Fake) Fail(failure FakeFailure, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failure, f.afterN = failure, n
}

// PrepareWith runs prepare inside every later Open before the stream starts,
// as [Local] prepares its recognizer; nil prepares nothing.
func (f *Fake) PrepareWith(prepare func(ctx context.Context, progress func(float64)) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepare = prepare
}

// SetAvailability sets what [Fake.Availability] answers.
func (f *Fake) SetAvailability(a Availability) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.available = a
}

// Availability implements [Streamer].
func (f *Fake) Availability() Availability {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.available
}

// Opened is how many streams were opened.
func (f *Fake) Opened() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opened
}

// Live is how many streams are open now.
func (f *Fake) Live() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live
}

// Bytes is how much audio every stream was sent, in total.
func (f *Fake) Bytes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bytes
}

// Open implements [Streamer].
func (f *Fake) Open(ctx context.Context, opts Options) (Stream, error) {
	if err := checkOptions(opts); err != nil {
		return nil, err
	}
	f.mu.Lock()
	if f.failure == FakeBusy || (f.capacity > 0 && f.live >= f.capacity) {
		f.mu.Unlock()
		return nil, ErrBusy
	}
	f.live++
	f.opened++
	prepare, failure, afterN := f.prepare, f.failure, f.afterN
	f.mu.Unlock()
	if prepare != nil {
		progress := opts.Preparing
		if progress == nil {
			progress = func(float64) {}
		}
		if err := prepare(ctx, progress); err != nil {
			f.release()
			return nil, err
		}
	}
	s := &fakeStream{fake: f, failure: failure, afterN: afterN,
		results: make(chan Result, 256)}
	s.sentence = s.words(0)
	return s, nil
}

func (f *Fake) release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live--
}

// fakeStream is one [Fake] stream.
type fakeStream struct {
	fake    *Fake
	failure FakeFailure
	afterN  int

	mu       sync.Mutex
	results  chan Result
	closed   bool
	finished bool
	err      error
	sent     int
	answered int
	voiced   int
	index    int
	sentence []string
	revealed int
}

// words is sentence i of the script as a partial spells it, or nil past the
// end.
func (s *fakeStream) words(i int) []string {
	if i >= len(s.fake.script) {
		return nil
	}
	return strings.Fields(strings.ToLower(strings.Map(func(r rune) rune {
		if strings.ContainsRune(".,?!;:", r) {
			return -1
		}
		return r
	}, s.fake.script[i])))
}

func (s *fakeStream) Results() <-chan Result { return s.results }

func (s *fakeStream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *fakeStream) Send(pcm []byte) error {
	if err := checkPCM(pcm); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.finished {
		return ErrClosed
	}
	s.fake.mu.Lock()
	s.fake.bytes += len(pcm)
	s.fake.mu.Unlock()
	s.sent += len(pcm)
	if s.failure == FakeLength && s.sent > s.afterN {
		s.endLocked(ErrTooLong)
		return ErrTooLong
	}
	if !voiced(pcm) {
		return nil
	}
	s.voiced += len(pcm)
	for s.voiced >= FakeWordAudio && s.sentence != nil {
		s.voiced -= FakeWordAudio
		s.revealed++
		if s.revealed < len(s.sentence) {
			s.emitLocked(Result{Text: strings.Join(s.sentence[:s.revealed], " ")})
			continue
		}
		s.emitLocked(Result{Final: true, Text: s.fake.script[s.index]})
		s.index++
		s.sentence, s.revealed = s.words(s.index), 0
	}
	return nil
}

// emitLocked delivers r unless an armed failure takes the stream first.
func (s *fakeStream) emitLocked(r Result) {
	if s.closed {
		return
	}
	if s.answered >= s.afterN {
		switch s.failure {
		case FakeDrop:
			s.endLocked(ErrLost)
			return
		case FakeStall:
			return
		}
	}
	s.answered++
	s.results <- r
}

func (s *fakeStream) Finish() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.finished {
		return ErrClosed
	}
	s.finished = true
	if s.failure == FakeStall && s.answered >= s.afterN {
		return nil
	}
	if s.revealed > 0 {
		text := strings.Join(s.sentence[:s.revealed], " ")
		s.emitLocked(Result{Final: true, Text: strings.ToUpper(text[:1]) + text[1:] + "."})
	}
	s.endLocked(nil)
	return nil
}

func (s *fakeStream) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endLocked(nil)
}

// endLocked closes the results once, recording why, and gives back the
// capacity.
func (s *fakeStream) endLocked(err error) {
	if s.closed {
		return
	}
	s.closed = true
	if s.err == nil {
		s.err = err
	}
	close(s.results)
	s.fake.release()
}

// voiced reports whether any sample in pcm reaches [FakeVoiced].
func voiced(pcm []byte) bool {
	for i := 0; i+1 < len(pcm); i += 2 {
		v := int16(binary.LittleEndian.Uint16(pcm[i:]))
		if v >= FakeVoiced || v <= -FakeVoiced {
			return true
		}
	}
	return false
}

// FakeSpeech is d of audio the fake counts as speech, for tests.
func FakeSpeech(d time.Duration) []byte {
	pcm := make([]byte, int(d/time.Millisecond)*BytesPerSecond/1000&^1)
	for i := 0; i+1 < len(pcm); i += 2 {
		binary.LittleEndian.PutUint16(pcm[i:], uint16(4*FakeVoiced))
	}
	return pcm
}
