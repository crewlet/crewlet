//go:build linux || darwin

package speech

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// The sherpa-onnx C API (sherpa-onnx/c-api/c-api.h at [SherpaVersion]),
// mirrored field for field: purego passes these structs by pointer, so their
// layout IS the ABI. A bump re-reads the header; the known-answer test
// (TestTheRealRecognizerTranscribesTheFixture) is what catches a drift.

type cFeatureConfig struct{ sampleRate, featureDim int32 }

type cTransducer struct{ encoder, decoder, joiner *byte }

// The fields named _ are ones this package never sets: there for the layout,
// commented with their C names.

type cParaformer struct{ _, _ *byte } // encoder, decoder

type cOneModel struct{ _ *byte } // model

type cModelConfig struct {
	transducer cTransducer
	_          cParaformer // paraformer
	_          cOneModel   // zipformer2_ctc
	tokens     *byte
	numThreads int32
	provider   *byte
	_          int32     // debug
	_          *byte     // model_type
	_          *byte     // modeling_unit
	_          *byte     // bpe_vocab
	_          *byte     // tokens_buf
	_          int32     // tokens_buf_size
	_          cOneModel // nemo_ctc
	_          cOneModel // t_one_ctc
}

type cCtcFst struct {
	_ *byte // graph
	_ int32 // max_active
}

type cHomophone struct{ _, _, _ *byte } // dict_dir, lexicon, rule_fsts

type cRecognizerConfig struct {
	feat           cFeatureConfig
	model          cModelConfig
	decodingMethod *byte
	maxActivePaths int32
	enableEndpoint int32
	rule1          float32
	rule2          float32
	rule3          float32
	_              *byte      // hotwords_file
	_              float32    // hotwords_score
	_              cCtcFst    // ctc_fst_decoder_config
	_              *byte      // rule_fsts
	_              *byte      // rule_fars
	_              float32    // blank_penalty
	_              *byte      // hotwords_buf
	_              int32      // hotwords_buf_size
	_              cHomophone // hr
}

type cPunctConfig struct {
	cnnBilstm  *byte
	bpeVocab   *byte
	numThreads int32
	_          int32 // debug
	provider   *byte
}

// cResult is the head of SherpaOnnxOnlineRecognizerResult: only text is read.
type cResult struct{ text *byte }

// The endpoint rules: a segment ends after 2.4 s of silence with nothing
// decoded, 0.8 s after decoded words (what the bench measured: a final
// 1.15 s after speech ends), or at 20 s of speech.
const (
	endpointSilenceEmpty  = 2.4
	endpointSilenceSpoken = 0.8
	endpointLongest       = 20
)

// finishPadding is the silence fed before a stream's input ends, so the last
// word's frames reach the encoder: 0.3 s, as the bench fed.
const finishPadding = 3 * Rate / 10

// sherpaAPI is the library's functions this package calls.
type sherpaAPI struct {
	version            func() *byte
	createRecognizer   func(*cRecognizerConfig) uintptr
	destroyRecognizer  func(uintptr)
	createStream       func(uintptr) uintptr
	destroyStream      func(uintptr)
	acceptWaveform     func(uintptr, int32, *float32, int32)
	isReady            func(uintptr, uintptr) int32
	decode             func(uintptr, uintptr)
	result             func(uintptr, uintptr) *cResult
	destroyResult      func(*cResult)
	isEndpoint         func(uintptr, uintptr) int32
	reset              func(uintptr, uintptr)
	inputFinished      func(uintptr)
	createPunctuation  func(*cPunctConfig) uintptr
	destroyPunctuation func(uintptr)
	addPunct           func(uintptr, *byte) *byte
	freeText           func(*byte)
}

// bind resolves every function, refusing a library that lacks one rather
// than letting purego panic on the first call.
func bind(handle uintptr) (*sherpaAPI, error) {
	api := &sherpaAPI{}
	for name, fn := range map[string]any{
		"SherpaOnnxGetVersionStr":                 &api.version,
		"SherpaOnnxCreateOnlineRecognizer":        &api.createRecognizer,
		"SherpaOnnxDestroyOnlineRecognizer":       &api.destroyRecognizer,
		"SherpaOnnxCreateOnlineStream":            &api.createStream,
		"SherpaOnnxDestroyOnlineStream":           &api.destroyStream,
		"SherpaOnnxOnlineStreamAcceptWaveform":    &api.acceptWaveform,
		"SherpaOnnxIsOnlineStreamReady":           &api.isReady,
		"SherpaOnnxDecodeOnlineStream":            &api.decode,
		"SherpaOnnxGetOnlineStreamResult":         &api.result,
		"SherpaOnnxDestroyOnlineRecognizerResult": &api.destroyResult,
		"SherpaOnnxOnlineStreamIsEndpoint":        &api.isEndpoint,
		"SherpaOnnxOnlineStreamReset":             &api.reset,
		"SherpaOnnxOnlineStreamInputFinished":     &api.inputFinished,
		"SherpaOnnxCreateOnlinePunctuation":       &api.createPunctuation,
		"SherpaOnnxDestroyOnlinePunctuation":      &api.destroyPunctuation,
		"SherpaOnnxOnlinePunctuationAddPunct":     &api.addPunct,
		"SherpaOnnxOnlinePunctuationFreeText":     &api.freeText,
	} {
		if _, err := purego.Dlsym(handle, name); err != nil {
			return nil, fmt.Errorf("the library has no %s: %w", name, err)
		}
		purego.RegisterLibFunc(fn, handle, name)
	}
	return api, nil
}

// sherpa is one loaded recognizer and punctuation model, shared by every
// stream of the worker.
type sherpa struct {
	api   *sherpaAPI
	rec   uintptr
	punct uintptr
	// punctMu serialises the punctuation model, which sherpa-onnx does not
	// document as safe to call concurrently; a call is milliseconds.
	punctMu sync.Mutex
}

var _ recognizer = (*sherpa)(nil)

// loadRecognizer loads the library and the model m keeps in dir.
func loadRecognizer(library string, m Model, dir string) (recognizer, error) {
	return loadSherpa(library, m, dir)
}

// loadSherpa loads library and the model m keeps in dir.
func loadSherpa(library string, m Model, dir string) (*sherpa, error) {
	handle, err := purego.Dlopen(library, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil, fmt.Errorf("loading %s: %w", library, err)
	}
	api, err := bind(handle)
	if err != nil {
		return nil, err
	}
	if got := goString(api.version()); strings.TrimPrefix(got, "v") != strings.TrimPrefix(SherpaVersion, "v") {
		return nil, fmt.Errorf("%s is sherpa-onnx %s, want %s", library, got, SherpaVersion)
	}
	var keep cStrings
	cfg := cRecognizerConfig{
		feat:           cFeatureConfig{sampleRate: Rate, featureDim: 80},
		decodingMethod: keep.c("greedy_search"),
		maxActivePaths: 4,
		enableEndpoint: 1,
		rule1:          endpointSilenceEmpty,
		rule2:          endpointSilenceSpoken,
		rule3:          endpointLongest,
	}
	cfg.model = cModelConfig{
		transducer: cTransducer{
			encoder: keep.c(m.File(dir, RoleEncoder)),
			decoder: keep.c(m.File(dir, RoleDecoder)),
			joiner:  keep.c(m.File(dir, RoleJoiner)),
		},
		tokens:     keep.c(m.File(dir, RoleTokens)),
		numThreads: 1,
		provider:   keep.c("cpu"),
	}
	s := &sherpa{api: api}
	s.rec = api.createRecognizer(&cfg)
	runtime.KeepAlive(&cfg)
	if s.rec == 0 {
		return nil, fmt.Errorf("sherpa-onnx refused the model in %s", dir)
	}
	punct := cPunctConfig{
		cnnBilstm:  keep.c(m.File(dir, RolePunctuation)),
		bpeVocab:   keep.c(m.File(dir, RolePunctuationVocab)),
		numThreads: 1,
		provider:   keep.c("cpu"),
	}
	s.punct = api.createPunctuation(&punct)
	runtime.KeepAlive(&punct)
	runtime.KeepAlive(&keep)
	if s.punct == 0 {
		api.destroyRecognizer(s.rec)
		return nil, fmt.Errorf("sherpa-onnx refused the punctuation model in %s", dir)
	}
	return s, nil
}

func (s *sherpa) newStream() recognizerStream {
	return &sherpaStream{s: s, h: s.api.createStream(s.rec)}
}

func (s *sherpa) close() {
	s.api.destroyPunctuation(s.punct)
	s.api.destroyRecognizer(s.rec)
}

// punctuate cases and punctuates a segment the model spelled in capitals.
func (s *sherpa) punctuate(text string) string {
	in := cString(strings.ToLower(text))
	s.punctMu.Lock()
	out := s.api.addPunct(s.punct, &in[0])
	s.punctMu.Unlock()
	runtime.KeepAlive(in)
	if out == nil {
		return text
	}
	defer s.api.freeText(out)
	return goString(out)
}

// sherpaStream is one stream of a [sherpa].
type sherpaStream struct {
	s    *sherpa
	h    uintptr
	last string
}

func (st *sherpaStream) accept(samples []float32) []Result {
	if len(samples) > 0 {
		st.s.api.acceptWaveform(st.h, Rate, &samples[0], int32(len(samples))) //nolint:gosec // a frame is bounded by maxFramePayload
		runtime.KeepAlive(samples)
	}
	return st.step()
}

// step decodes what the stream holds and answers what changed: a final at an
// endpoint, else a new partial.
func (st *sherpaStream) step() []Result {
	api, rec := st.s.api, st.s.rec
	for api.isReady(rec, st.h) == 1 {
		api.decode(rec, st.h)
	}
	text := st.text()
	if api.isEndpoint(rec, st.h) == 1 {
		api.reset(rec, st.h)
		st.last = ""
		if text == "" {
			return nil
		}
		return []Result{{Final: true, Text: st.s.punctuate(text)}}
	}
	if text == st.last {
		return nil
	}
	st.last = text
	if text == "" {
		return nil
	}
	return []Result{{Text: strings.ToLower(text)}}
}

func (st *sherpaStream) text() string {
	r := st.s.api.result(st.s.rec, st.h)
	if r == nil {
		return ""
	}
	defer st.s.api.destroyResult(r)
	return strings.TrimSpace(goString(r.text))
}

func (st *sherpaStream) finish() []Result {
	out := st.accept(make([]float32, finishPadding))
	st.s.api.inputFinished(st.h)
	api, rec := st.s.api, st.s.rec
	for api.isReady(rec, st.h) == 1 {
		api.decode(rec, st.h)
	}
	if text := st.text(); text != "" {
		out = append(out, Result{Final: true, Text: st.s.punctuate(text)})
	}
	return out
}

func (st *sherpaStream) close() { st.s.api.destroyStream(st.h) }

// cStrings keeps every C string of one call alive until the call returns.
type cStrings [][]byte

func (k *cStrings) c(s string) *byte {
	b := cString(s)
	*k = append(*k, b)
	return &b[0]
}

func cString(s string) []byte { return append([]byte(s), 0) }

// goString copies a NUL-terminated C string.
func goString(p *byte) string {
	if p == nil {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Add(unsafe.Pointer(p), n)) != 0 {
		n++
	}
	return string(unsafe.Slice(p, n))
}
