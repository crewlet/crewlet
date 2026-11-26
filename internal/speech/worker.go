package speech

import (
	"bufio"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sync"
)

// WorkerCommand is the subcommand the recognizer runs as: `crewlet
// speech-worker`. One name for the binary's dispatch, the argv [Local]
// starts, and test binaries ([DispatchIfWorker]).
const WorkerCommand = "speech-worker"

// recognizer is what a worker serves streams from: the loaded library, or a
// test's script.
type recognizer interface {
	newStream() recognizerStream
	close()
}

// recognizerStream is one stream's decoder.
type recognizerStream interface {
	// accept feeds samples and answers what changed.
	accept(samples []float32) []Result
	// finish ends the input and answers the remaining finals.
	finish() []Result
	close()
}

// workerQueue is how many frames one stream may have waiting for its decoder
// before the worker stops reading: about six seconds of 100 ms frames, past
// any honest jitter, and backpressure on the pipe beyond it.
const workerQueue = 64

// RunWorker loads the recognizer the flags name and serves streams over
// stdin and stdout until stdin closes. It writes nothing but a load failure
// to stderr: audio and text never leave the frames.
func RunWorker(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet(WorkerCommand, flag.ContinueOnError)
	fs.SetOutput(stderr)
	library := fs.String("library", "", "path of the sherpa-onnx C API library")
	name := fs.String("model", "", "name of the pinned model to load")
	dir := fs.String("model-dir", "", "directory holding the model's files")
	if err := fs.Parse(args); err != nil {
		return err
	}
	out := &frameWriter{w: stdout}
	m, ok := models[*name]
	if !ok || *library == "" || *dir == "" {
		err := fmt.Errorf("speech: usage: crewlet %s -library PATH -model NAME -model-dir DIR "+
			"(Not for people: the engine starts it for dictation)", WorkerCommand)
		_ = out.write(frame{kind: frameFailed, payload: []byte(err.Error())})
		return err
	}
	rec, err := loadRecognizer(*library, m, *dir)
	if err != nil {
		_ = out.write(frame{kind: frameFailed, payload: []byte(err.Error())})
		fmt.Fprintln(stderr, "crewlet "+WorkerCommand+":", err)
		return err
	}
	defer rec.close()
	return serve(stdin, out, rec)
}

// serve announces readiness and runs every stream the engine opens, each on
// its own goroutine, until stdin ends.
func serve(stdin io.Reader, out *frameWriter, rec recognizer) error {
	if err := out.write(frame{kind: frameReady}); err != nil {
		return err
	}
	in := bufio.NewReader(stdin)
	streams := map[uint32]chan frame{}
	var running sync.WaitGroup
	defer func() {
		for _, q := range streams {
			close(q)
		}
		running.Wait()
	}()
	for {
		f, err := readFrame(in)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("speech: reading the engine's frames: %w", err)
		}
		switch f.kind {
		case frameOpen:
			if _, open := streams[f.id]; open {
				continue
			}
			q := make(chan frame, workerQueue)
			streams[f.id] = q
			running.Go(func() { runStream(f.id, q, rec, out) })
		case frameAudio, frameFinish, frameClose:
			q, open := streams[f.id]
			if !open {
				continue
			}
			q <- f
			if f.kind != frameAudio {
				close(q)
				delete(streams, f.id)
			}
		}
	}
}

// runStream decodes one stream's frames and reports frameDone when it ends,
// however it ends.
func runStream(id uint32, q <-chan frame, rec recognizer, out *frameWriter) {
	st := rec.newStream()
	defer st.close()
	defer func() { _ = out.write(frame{kind: frameDone, id: id}) }()
	for f := range q {
		switch f.kind {
		case frameAudio:
			out.results(id, st.accept(samples(f.payload)))
		case frameFinish:
			out.results(id, st.finish())
			return
		case frameClose:
			return
		}
	}
}

// samples converts 16-bit little-endian PCM to the [-1, 1] floats the
// recognizer takes.
func samples(pcm []byte) []float32 {
	out := make([]float32, len(pcm)/2)
	for i := range out {
		out[i] = float32(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / 32768
	}
	return out
}

// frameWriter writes whole frames from many goroutines.
type frameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (fw *frameWriter) write(f frame) error {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	return writeFrame(fw.w, f)
}

func (fw *frameWriter) results(id uint32, rs []Result) {
	for _, r := range rs {
		kind := framePartial
		if r.Final {
			kind = frameFinal
		}
		_ = fw.write(frame{kind: kind, id: id, payload: []byte(r.Text)})
	}
}

// DispatchIfWorker runs this process as the worker when it was started as
// one (`<binary> speech-worker ...`) and exits; otherwise it returns at once.
// For test binaries that stand in for the engine, as mcprelay's is.
func DispatchIfWorker() {
	if len(os.Args) < 2 || os.Args[1] != WorkerCommand {
		return
	}
	if err := RunWorker(os.Args[2:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
