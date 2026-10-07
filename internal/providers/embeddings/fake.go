package embeddings

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"slices"
	"strings"
	"sync"
)

// Fake is a deterministic embedder for tests.
//
// # It is a real similarity function, not a stub
//
// A stub returning a constant makes every memory equally similar to every
// task, which is the one answer that makes a recall test meaningless: the
// floor admits everything and the ranking is arbitrary. This hashes TOKENS
// into a bag-of-words vector, so text that shares words scores high and text
// that shares none scores near zero — enough for a test to assert that the
// right memory came back and the wrong one did not.
//
// It is emphatically NOT a semantic embedder: "car" and "automobile" score
// zero here. That is the honest limit, and it is why this is not offered as
// a configured provider type — a company running on it would find its recall
// silently worse in exactly the cases recall exists for.
//
// # And it refuses what the real one refuses
//
// A twin that accepted anything would certify a caller that sends what every
// real provider refuses, so it holds the same [Limits] and enforces them by
// the same code: an input past the bound is a [TooLongError] before anything
// is "sent", a batch is packed into requests by [Limits.Requests], and every
// request is recorded ([Fake.Requests]) so a caller's test can see how its
// work was divided. It starts at the limits of the default provider's models
// (OpenAI's text-embedding-3, as config.EmbeddingModels states them), so a
// test sending what production would refuse is refused here too; a test
// about a smaller model sets its own ([Fake.SetLimits]).
//
// It FAILS on request, too, in the provider's own classes: [Fake.Refuse]
// makes every request carrying a marked input an [ErrRefused] — a whole
// request, because that is what a provider refuses over one input it cannot
// take — [Fake.FailTransiently] makes the next few an [ErrTransient], and
// [Fake.FailConfiguration] the next few an [ErrConfiguration]. So a caller
// that isolates a poison input, retries a transient one or stops on a refused
// credential can be tested failing, which a twin that only ever succeeded
// could not. And it answers what a provider answers that is no failure at all
// and still nothing a caller can keep: [Fake.AnswerZero] makes a marked
// input's vector all zeros, in an accepted request.
type Fake struct {
	width int

	mu            sync.Mutex
	model         string
	limits        Limits
	refused       []string
	zeroed        []string
	failing       map[string]int
	misconfigured map[string]int
	requests      [][]string
}

var _ BatchEmbedder = (*Fake)(nil)

// fakeLimits is where a [Fake] starts: OpenAI's text-embedding-3 limits, which
// internal/engine holds against what the configuration resolves for those
// models, so the twin and the default provider cannot drift apart unseen.
var fakeLimits = Limits{InputBytes: 8192, BatchInputs: 2048, BatchBytes: 300_000}

// NewFake builds a fake at the given width. Zero takes a small one, because
// a test asserting a store round trip cares about the width matching, not
// about what it is.
func NewFake(width int) *Fake {
	if width <= 0 {
		width = 64
	}
	return &Fake{width: width, model: "fake-embedding", limits: fakeLimits}
}

// Width implements [Embedder].
func (f *Fake) Width() int { return f.width }

// Model implements [Embedder].
func (f *Fake) Model() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.model
}

// Limits implements [Embedder].
func (f *Fake) Limits() Limits {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.limits
}

// SetModel names the model the fake reports.
func (f *Fake) SetModel(model string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.model = model
}

// SetLimits replaces the limits the fake enforces. Limits nothing could be
// sent under are a mistake in the test, and panic rather than turn into a
// fake that refuses everything.
func (f *Fake) SetLimits(l Limits) {
	if err := l.Validate(); err != nil {
		panic(fmt.Sprintf("embeddings: Fake.SetLimits: %v", err))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limits = l
}

// Refuse makes every request carrying an input whose prepared text contains
// marker fail as [ErrRefused] — every one, as a provider refuses the same
// input every time it is sent.
func (f *Fake) Refuse(marker string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refused = append(f.refused, marker)
}

// AnswerZero makes every input whose prepared text contains marker answered
// with a vector of zeros, in a request the fake accepts — every time, as a
// server that has no representation of a text answers it the same way each
// time it is sent. Such a vector has no direction: no cosine can compare it
// and no pool can weigh it, and the real provider passes it through as the
// fake does, since the answer is a well-formed one of the right width.
func (f *Fake) AnswerZero(marker string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.zeroed = append(f.zeroed, marker)
}

// FailTransiently makes the next times requests carrying an input whose
// prepared text contains marker fail as [ErrTransient], and the ones after
// them succeed.
func (f *Fake) FailTransiently(marker string, times int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing == nil {
		f.failing = map[string]int{}
	}
	f.failing[marker] += times
}

// FailConfiguration makes the next times requests carrying an input whose
// prepared text contains marker fail as [ErrConfiguration] — the class a
// revoked key, a missing model or an endpoint that is not there answers with —
// and the ones after them succeed.
func (f *Fake) FailConfiguration(marker string, times int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.misconfigured == nil {
		f.misconfigured = map[string]int{}
	}
	f.misconfigured[marker] += times
}

// Requests is every request the fake answered or failed, in order, as the
// prepared inputs it carried. A refusal made before any request — an input
// past the bound — is not one.
func (f *Fake) Requests() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.requests))
	for i, request := range f.requests {
		out[i] = slices.Clone(request)
	}
	return out
}

// Embed implements [Embedder].
func (f *Fake) Embed(ctx context.Context, text string) ([]float32, error) {
	prepared := Prepare(text)
	if prepared == "" {
		return nil, ErrEmpty
	}
	limits, model := f.Limits(), f.Model()
	if len(prepared) > limits.InputBytes {
		return nil, &TooLongError{Model: model, Bytes: len(prepared), Limit: limits.InputBytes}
	}
	if err := f.send(ctx, []string{prepared}); err != nil {
		return nil, err
	}
	return f.answer(prepared), nil
}

// EmbedBatch implements [BatchEmbedder].
//
// The real provider's contract, without the network: inputs measured before
// any request, packed by [Limits.Requests], one ordered result or an error —
// and an input with nothing to embed keeping its slot as a nil vector rather
// than being dropped, because a caller matching results to documents by index
// is what the contract is for, and a fake that quietly compacted its answer
// would be a fake that only ever tested the happy corpus.
func (f *Fake) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	prepared := make([]string, len(texts))
	sizes := make([]int, len(texts))
	for i, text := range texts {
		prepared[i] = Prepare(text)
		sizes[i] = len(prepared[i])
	}
	groups, err := f.Limits().requests(sizes)
	if err != nil {
		var long *TooLongError
		if errors.As(err, &long) {
			long.Model = f.Model()
		}
		return nil, err
	}
	out := make([][]float32, len(texts))
	for _, group := range groups {
		input := make([]string, len(group))
		for j, at := range group {
			input[j] = prepared[at]
		}
		if err := f.send(ctx, input); err != nil {
			return nil, err
		}
		for _, at := range group {
			out[at] = f.answer(prepared[at])
		}
	}
	return out, nil
}

// send is one request: refused or failed as the test asked, or recorded.
//
// A DONE CONTEXT fails it as the real provider's does — a deadline as
// [ErrTransient], a cancellation unclassified — because a twin that answered
// a caller who had stopped waiting would certify a caller that never checks.
func (f *Fake) send(ctx context.Context, input []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return &Error{Model: f.model, Class: ErrTransient, Err: err}
		}
		return fmt.Errorf("embeddings: %s: %w", f.model, err)
	}
	f.requests = append(f.requests, slices.Clone(input))
	for _, marker := range f.refused {
		if carries(input, marker) {
			return &Error{Model: f.model, Status: 400, Class: ErrRefused,
				Err: fmt.Errorf("the fake refuses inputs containing %q", marker)}
		}
	}
	for marker, left := range f.failing {
		if left > 0 && carries(input, marker) {
			f.failing[marker] = left - 1
			return &Error{Model: f.model, Status: 503, Class: ErrTransient,
				Err: fmt.Errorf("the fake is failing inputs containing %q", marker)}
		}
	}
	for marker, left := range f.misconfigured {
		if left > 0 && carries(input, marker) {
			f.misconfigured[marker] = left - 1
			return &Error{Model: f.model, Status: 401, Class: ErrConfiguration,
				Err: fmt.Errorf("the fake refuses the credential for inputs containing %q", marker)}
		}
	}
	return nil
}

// carries reports whether any input contains marker.
func carries(input []string, marker string) bool {
	for _, text := range input {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// answer is what the fake answers prepared, non-empty text with: its
// [Fake.vector], or zeros for a text [Fake.AnswerZero] marked.
func (f *Fake) answer(prepared string) []float32 {
	f.mu.Lock()
	zero := slices.ContainsFunc(f.zeroed,
		func(marker string) bool { return strings.Contains(prepared, marker) })
	f.mu.Unlock()
	if zero {
		return make([]float32, f.width)
	}
	return f.vector(prepared)
}

// vector is the bag-of-words embedding of prepared, non-empty text.
//
// NORMALISED to unit length, because cosine similarity is what reads these
// and an unnormalised bag-of-words makes a long text similar to everything
// by having a bigger magnitude than anything.
func (f *Fake) vector(prepared string) []float32 {
	vector := make([]float32, f.width)
	for word := range strings.FieldsSeq(strings.ToLower(prepared)) {
		h := fnv.New32a()
		h.Write([]byte(word))
		// THE MODULO IS UNSIGNED. int(h.Sum32()) is non-negative on a
		// 64-bit int and can be negative on a 32-bit one, where the
		// index would panic — a platform difference in a helper whose
		// whole job is being boring.
		vector[h.Sum32()%uint32(f.width)] += 1
	}
	// Normalising needs no zero guard: prepared is non-empty, so Fields
	// yields at least one word, so at least one bucket holds 1 and the sum
	// is at least 1.
	var sum float64
	for _, v := range vector {
		sum += float64(v) * float64(v)
	}
	norm := float32(math.Sqrt(sum))
	for i := range vector {
		vector[i] /= norm
	}
	return vector
}
