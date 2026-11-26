package speech

import (
	"bufio"
	"io"
	"testing"
)

// THE WORKER LOOP, IN PROCESS: frames in, a stream per id on its own decoder,
// its results in order, and `done` however the stream ends, a close included,
// so the engine's reader always learns a stream is over.
func TestTheWorkerAnswersEveryStreamAndEndsEachWithDone(t *testing.T) {
	t.Parallel()
	toWorker, engineSide := io.Pipe()
	fromWorker, workerOut := io.Pipe()
	served := make(chan error, 1)
	go func() { served <- serve(toWorker, &frameWriter{w: workerOut}, scripted{}) }()
	out := bufio.NewReader(fromWorker)
	in := &frameWriter{w: engineSide}

	expect := func(kind byte, id uint32, payload string) {
		t.Helper()
		f, err := readFrame(out)
		if err != nil {
			t.Fatal(err)
		}
		if f.kind != kind || f.id != id || string(f.payload) != payload {
			t.Fatalf("frame = {%d %d %q}, want {%d %d %q}", f.kind, f.id, f.payload, kind, id, payload)
		}
	}
	expect(frameReady, 0, "")
	voiced := FakeSpeech(100_000_000)
	for _, f := range []frame{
		{kind: frameOpen, id: 1}, {kind: frameAudio, id: 1, payload: voiced}, {kind: frameFinish, id: 1},
	} {
		if err := in.write(f); err != nil {
			t.Fatal(err)
		}
	}
	expect(framePartial, 1, "word1")
	expect(frameFinal, 1, "Said 1 words.")
	expect(frameDone, 1, "")

	for _, f := range []frame{{kind: frameOpen, id: 2}, {kind: frameClose, id: 2}} {
		if err := in.write(f); err != nil {
			t.Fatal(err)
		}
	}
	expect(frameDone, 2, "")
	_ = engineSide.Close()
	if err := <-served; err != nil {
		t.Errorf("the worker ended its input with %v, want a clean exit", err)
	}
}

// A FRAME LONGER THAN THE LIMIT IS REFUSED rather than allocated.
func TestAnOversizedFrameIsRefused(t *testing.T) {
	t.Parallel()
	head := []byte{frameAudio, 0, 0, 0, 1, 0xff, 0xff, 0xff, 0xff}
	if _, err := readFrame(bufio.NewReader(&byteReader{b: head})); err == nil {
		t.Error("a 4 GiB frame header was accepted")
	}
}

type byteReader struct{ b []byte }

func (r *byteReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}
