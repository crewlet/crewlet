package speech_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/speech"
)

func results(s speech.Stream) []speech.Result {
	var out []speech.Result
	for r := range s.Results() {
		out = append(out, r)
	}
	return out
}

// THE FAKE IS DETERMINISTIC: a word a partial per 300 ms of speech, the
// sentence as written at its last word, the rest at the finish; silence
// reveals nothing.
func TestTheFakeRecitesItsScriptByTheAudioItHears(t *testing.T) {
	t.Parallel()
	f := speech.NewFake("Hello there.", "Second one")
	s, err := f.Open(context.Background(), speech.Options{Lang: speech.Lang})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Send(make([]byte, speech.BytesPerSecond))
	_ = s.Send(speech.FakeSpeech(900 * time.Millisecond))
	_ = s.Finish()
	got := results(s)
	want := []speech.Result{{Text: "hello"}, {Final: true, Text: "Hello there."}, {Text: "second"},
		{Final: true, Text: "Second."}}
	if len(got) != len(want) {
		t.Fatalf("results = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("result %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if f.Bytes() != speech.BytesPerSecond+speech.FakeWordAudio*3 || f.Live() != 0 {
		t.Errorf("the fake recorded %d bytes and %d live streams", f.Bytes(), f.Live())
	}
}

// AND FAILS ON REQUEST, in the classes the real one fails in.
func TestTheFakeFailsOnRequest(t *testing.T) {
	t.Parallel()
	open := func(f *speech.Fake) speech.Stream {
		t.Helper()
		s, err := f.Open(context.Background(), speech.Options{Lang: speech.Lang})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	busy := speech.NewFake("x")
	busy.Fail(speech.FakeBusy, 0)
	if _, err := busy.Open(context.Background(), speech.Options{Lang: speech.Lang}); !errors.Is(err, speech.ErrBusy) {
		t.Errorf("busy: %v", err)
	}
	drop := speech.NewFake("one two three four")
	drop.Fail(speech.FakeDrop, 1)
	s := open(drop)
	_ = s.Send(speech.FakeSpeech(time.Second))
	if results(s); !errors.Is(s.Err(), speech.ErrLost) {
		t.Errorf("drop: %v", s.Err())
	}
	length := speech.NewFake("one")
	length.Fail(speech.FakeLength, 10)
	s = open(length)
	if err := s.Send(speech.FakeSpeech(time.Second)); !errors.Is(err, speech.ErrTooLong) {
		t.Errorf("length: %v", err)
	}
	stall := speech.NewFake("one two three")
	stall.Fail(speech.FakeStall, 0)
	s = open(stall)
	_ = s.Send(speech.FakeSpeech(time.Second))
	_ = s.Finish()
	select {
	case r, ok := <-s.Results():
		t.Errorf("a stalled stream answered %+v (open %v)", r, ok)
	default:
	}
	s.Close()
	if _, err := speech.NewFake("x").Open(context.Background(), speech.Options{Lang: "fr"}); !errors.Is(err, speech.ErrUnsupported) {
		t.Errorf("a French stream opened: %v", err)
	}
}
