package speech

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"
)

// scriptedWorkerCommand runs the test binary as a worker whose recognizer is
// [scripted]: the real worker loop and wire, with no library or model.
const scriptedWorkerCommand = "speech-test-worker"

// The scripted worker's behaviours, its one argument.
const (
	scriptedOK       = "ok"
	scriptedCrash    = "crash"     // exits mid-stream on its second chunk of audio
	scriptedFailLoad = "fail-load" // reports a load failure
	scriptedHang     = "hang"      // never reports ready
)

func dispatchScriptedWorker() {
	if len(os.Args) < 3 || os.Args[1] != scriptedWorkerCommand {
		return
	}
	out := &frameWriter{w: os.Stdout}
	switch os.Args[2] {
	case scriptedFailLoad:
		_ = out.write(frame{kind: frameFailed, payload: []byte("the scripted model is missing")})
		os.Exit(1)
	case scriptedHang:
		_, _ = bufio.NewReader(os.Stdin).ReadString(0)
		os.Exit(0)
	}
	if err := serve(os.Stdin, out, scripted{crash: os.Args[2] == scriptedCrash}); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// scripted is a recognizer that answers "word N" for each voiced chunk and
// a final "Said N words." at the finish.
type scripted struct{ crash bool }

func (s scripted) newStream() recognizerStream { return &scriptedStream{crash: s.crash} }

func (scripted) close() {}

type scriptedStream struct {
	crash  bool
	chunks int
	words  []string
}

func (s *scriptedStream) accept(samples []float32) []Result {
	s.chunks++
	if s.crash && s.chunks == 2 {
		os.Exit(3)
	}
	for _, v := range samples {
		if v > 0.01 || v < -0.01 {
			s.words = append(s.words, fmt.Sprintf("word%d", len(s.words)+1))
			return []Result{{Text: strings.Join(s.words, " ")}}
		}
	}
	return nil
}

func (s *scriptedStream) finish() []Result {
	return []Result{{Final: true, Text: fmt.Sprintf("Said %d words.", len(s.words))}}
}

func (*scriptedStream) close() {}

// waitTimeout bounds a test's wait for a signal that should come at once.
const waitTimeout = 30 * time.Second
