// Package speech is dictation's speech-to-text: streaming recognition that
// turns a person's microphone audio into provisional partials and punctuated
// finals, with no setup, no key and no vendor.
//
// # The recognizer is local, and audio never leaves the engine
//
// [Local] runs k2-fsa's sherpa-onnx (Apache-2.0) over onnxruntime (MIT) with
// a streaming Zipformer transducer and an English punctuation and casing
// model, the pinned [DefaultModel]. Nothing of it is in the binary: the
// library is this platform's official release archive (native.go) and the
// model its files on Hugging Face at a pinned commit, each verified against
// its SHA-256. Both are read from `dictation.recognizer_dir`, or from beside
// the binary where the container image bakes them ([BesideBinary]), or are
// fetched once: the library into the engine's native-library cache
// (internal/nativelib), the model into the data directory. Swapping either is
// a new pin, never a code change.
//
// # In a child process, not in the engine
//
// The library is native code, so a crash in it (a segfault, an abort, an
// allocation the kernel kills) would end the engine and every seat it runs.
// [Local] therefore runs the recognizer in a worker: this same binary
// re-executed as `crewlet speech-worker` ([WorkerCommand]), speaking a framed
// protocol over its stdin and stdout (wire.go). A worker that dies fails only
// the dictations it was serving, is logged as `speech_worker_exited` with its
// status, and is started again by the next dictation. The worker also makes
// the idle unload real: when no stream has been open for [UnloadAfter] the
// worker exits and the operating system takes back every page of the model,
// which freeing it inside a long-lived process does not guarantee.
//
// # One recognizer, a capacity, and a lazy start
//
// A worker loads the model once and decodes every stream against it, one
// thread per decode (measured: 0.19 of a core per live stream, flat from one
// stream to four, on an EPYC vCPU). [Local] admits at most its capacity of
// live streams ([DefaultCapacity]: four per available core) and answers
// [ErrBusy] past it. Nothing loads until the first dictation; [Options]'
// Preparing hears the progress of a download and a load.
//
// # Nothing is stored or logged
//
// Audio and text live only in memory, for the life of a stream. Nothing here
// writes either to a log, an event, a file or a metric; the worker writes
// nothing but its own load errors to stderr. [Fake] is the deterministic
// double the relay's tests drive.
package speech

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("speech")

// Rate is the one sample rate a stream takes: 16 kHz, what every model in the
// table was trained on and what a browser's AudioWorklet resamples to.
const Rate = 16000

// BytesPerSecond is the size of one second of 16-bit mono audio at [Rate].
const BytesPerSecond = Rate * 2

// Lang is the one language the default model recognises.
const Lang = "en"

// Streamer opens recognition streams: [Local], [Fake], and any later vendor.
type Streamer interface {
	// Open starts a stream, preparing the recognizer first when it is not
	// ready. It answers [ErrBusy] at capacity and an [UnavailableError]
	// when the recognizer cannot be prepared on this node.
	Open(ctx context.Context, opts Options) (Stream, error)

	// Availability says whether a stream could be opened here, and why not.
	Availability() Availability
}

// Stream is one dictation's recognition.
type Stream interface {
	// Send feeds audio: 16-bit little-endian mono PCM at [Rate], an even
	// number of bytes.
	Send(pcm []byte) error

	// Results carries partials and finals in order. It is closed after
	// [Stream.Finish] delivered the last final, or when the stream failed
	// ([Stream.Err]) or was closed.
	Results() <-chan Result

	// Finish ends the audio: the recognizer decodes what it holds and
	// Results delivers the remaining finals, then closes.
	Finish() error

	// Close abandons the stream and releases its capacity. Safe to call
	// more than once, and after Finish.
	Close()

	// Err is why Results closed early, nil after a clean finish or Close.
	Err() error
}

// Result is one recognition update.
type Result struct {
	// Final is true for a stable, punctuated segment, which the next
	// partial follows; a partial replaces the provisional segment.
	Final bool

	// Text is the segment's words.
	Text string
}

// Options configure one stream.
type Options struct {
	// Lang is the language spoken; only [Lang] is recognised.
	Lang string

	// Preparing hears progress from 0 to 1 while the recognizer is
	// prepared for this stream, and is never called when it is ready.
	Preparing func(progress float64)
}

// Reason is why dictation is not available here.
type Reason string

// The reasons dictation is unavailable. A screen draws the microphone
// disabled with the reason, or uses the browser's own recognition instead.
const (
	// ReasonDisabled is `dictation.enabled: false` on this node.
	ReasonDisabled Reason = "disabled"

	// ReasonUnsupportedPlatform is a build with no recognizer library.
	ReasonUnsupportedPlatform Reason = "unsupported_platform"

	// ReasonModelUnavailable is a library or model this node could not
	// read, verify or fetch; the next dictation after [RetryAfter] tries
	// again.
	ReasonModelUnavailable Reason = "model_unavailable"

	// ReasonFailed is a recognizer that would not start; the next
	// dictation after [RetryAfter] tries again.
	ReasonFailed Reason = "failed"
)

// Valid reports whether r is one of the declared reasons.
func (r Reason) Valid() bool {
	switch r {
	case ReasonDisabled, ReasonUnsupportedPlatform, ReasonModelUnavailable, ReasonFailed:
		return true
	}
	return false
}

// Availability is whether dictation can be offered here.
type Availability struct {
	Available bool   `json:"available"`
	Reason    Reason `json:"reason,omitempty"`
}

// Errors a [Streamer] and a [Stream] answer.
var (
	// ErrBusy is a recognizer at capacity.
	ErrBusy = errors.New("speech: the recognizer is at capacity")

	// ErrUnavailable is what an [UnavailableError] is to errors.Is.
	ErrUnavailable = errors.New("speech: dictation is unavailable here")

	// ErrUnsupported is a language or format the recognizer does not take.
	ErrUnsupported = errors.New("speech: unsupported audio or language")

	// ErrLost is a stream whose recognizer went away mid-dictation.
	ErrLost = errors.New("speech: the recognizer stopped mid-dictation")

	// ErrTooLong is a stream past the recognizer's own length limit.
	ErrTooLong = errors.New("speech: the dictation is longer than the recognizer takes")

	// ErrClosed is a Send or Finish on a stream already finished or closed.
	ErrClosed = errors.New("speech: the stream is closed")
)

// UnavailableError is [ErrUnavailable] with the reason a screen shows and the
// cause an operator reads in the log.
type UnavailableError struct {
	Reason Reason
	Err    error
}

func (e *UnavailableError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("speech: dictation is unavailable here (%s)", e.Reason)
	}
	return fmt.Sprintf("speech: dictation is unavailable here (%s): %v", e.Reason, e.Err)
}

// Unwrap makes an UnavailableError [ErrUnavailable] to errors.Is, and keeps
// its cause reachable.
func (e *UnavailableError) Unwrap() []error { return []error{ErrUnavailable, e.Err} }

// checkPCM refuses audio a stream cannot take.
func checkPCM(pcm []byte) error {
	if len(pcm)%2 != 0 {
		return fmt.Errorf("%w: %d bytes is not whole 16-bit samples", ErrUnsupported, len(pcm))
	}
	return nil
}

// checkOptions refuses a stream the recognizer cannot serve.
func checkOptions(opts Options) error {
	if opts.Lang != Lang {
		return fmt.Errorf("%w: language %q (this recognizer takes %q)", ErrUnsupported, opts.Lang, Lang)
	}
	return nil
}
