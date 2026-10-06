package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// THE LIMITS ARE THE MODEL'S, and an unset field takes them.
//
// OpenAI documents 8192 tokens an input, 2048 inputs and 300 000 tokens a
// request, and counts an input's tokens as cl100k_base counts them with
// nothing added — so every one of those carries over byte for byte, with no
// allowance taken off for a wrapper the server does not add.
func TestAnUnsetLimitComesFromTheModel(t *testing.T) {
	t.Parallel()
	want := config.EmbeddingLimits{
		InputBytes: 8192, BatchInputs: 2048, BatchBytes: 300_000, InputOverhead: 0,
	}
	for _, model := range []string{"text-embedding-3-large", "text-embedding-3-small"} {
		t.Run(model, func(t *testing.T) {
			e := &config.EmbeddingProvider{Type: config.EmbeddingOpenAI, Model: model}
			if got := e.Limits(); got != want {
				t.Errorf("Limits() = %+v, want %+v", got, want)
			}
			if err := embeddingCompany(t, e).Validate(); err != nil {
				t.Errorf("a known model with every limit documented was refused: %v", err)
			}
		})
	}
}

// OPENAI'S INPUT BOUND IS THE CORPUS'S OPENING, exactly.
//
// The knowledge corpus embeds a source's first 8 KiB and digests what it sent
// into a replicated vector record. A bound a byte under that would move the
// opening of every long document, change every one of those digests and
// re-embed the corpus — and during a rolling upgrade the duty moving between
// builds would do it on every lease move. So the bound for the default
// provider's models is pinned here, at the value that keeps it where it is.
func TestOpenAIsInputBoundKeepsTheCorpusOpeningWhereItIs(t *testing.T) {
	t.Parallel()
	for _, model := range []string{"text-embedding-3-large", "text-embedding-3-small"} {
		e := &config.EmbeddingProvider{Model: model}
		if got := e.Limits().InputBytes; got != 8<<10 {
			t.Errorf("%s: InputBytes = %d, want 8192", model, got)
		}
	}
}

// A LIMIT NOBODY DOCUMENTED IS NOT GUESSED. Google documents gemini-embedding-001's
// window but nothing per request for the endpoint this backend calls, so those
// two are refused until stated — and the refusal says who left them out.
func TestAnUndocumentedLimitIsRefusedUntilItIsStated(t *testing.T) {
	t.Parallel()
	e := &config.EmbeddingProvider{
		Type: config.EmbeddingOpenAICompatible, Model: "gemini-embedding-001",
		BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai/",
	}
	err := embeddingCompany(t, e).Validate()
	if err == nil {
		t.Fatal("a model with undocumented request limits was accepted with none stated")
	}
	if !errors.Is(err, config.ErrMissing) {
		t.Errorf("refusal %v is not ErrMissing", err)
	}
	for _, want := range []string{"max_batch_inputs", "max_batch_tokens", "Google"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %q, want it to say %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "max_input_tokens") {
		t.Errorf("refusal = %q asks for max_input_tokens, which Google documents", err)
	}
	if got := e.Limits(); got != (config.EmbeddingLimits{}) {
		t.Errorf("an incompletely known model resolved limits %+v; want the zero "+
			"value, which nothing can be built from", got)
	}

	e.MaxBatchInputs, e.MaxBatchTokens = 100, 20_000
	if err := embeddingCompany(t, e).Validate(); err != nil {
		t.Fatalf("stating the missing limits did not let it through: %v", err)
	}
	want := config.EmbeddingLimits{
		InputBytes:    2048 - config.EmbeddingWrapTokens,
		BatchInputs:   100,
		BatchBytes:    20_000,
		InputOverhead: config.EmbeddingWrapTokens,
	}
	if got := e.Limits(); got != want {
		t.Errorf("Limits() = %+v, want %+v", got, want)
	}
}

// A MODEL THIS BUILD DOES NOT KNOW states every limit, as it states its width.
func TestAnUnknownModelStatesEveryLimit(t *testing.T) {
	t.Parallel()
	e := &config.EmbeddingProvider{
		Type: config.EmbeddingOpenAICompatible, Model: "bge-m3",
		BaseURL: "http://embed.example.com/v1", Dimensions: 1024,
	}
	err := embeddingCompany(t, e).Validate()
	if err == nil {
		t.Fatal("an unknown model with no limits was accepted")
	}
	for _, want := range []string{"max_input_tokens", "max_batch_inputs", "max_batch_tokens", "bge-m3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %q, want it to say %q", err, want)
		}
	}

	e.MaxInputTokens, e.MaxBatchInputs, e.MaxBatchTokens = 8192, 32, 65_536
	if err := embeddingCompany(t, e).Validate(); err != nil {
		t.Fatalf("an unknown model with every limit stated was refused: %v", err)
	}
	want := config.EmbeddingLimits{
		InputBytes:    8192 - config.EmbeddingWrapTokens,
		BatchInputs:   32,
		BatchBytes:    65_536,
		InputOverhead: config.EmbeddingWrapTokens,
	}
	if got := e.Limits(); got != want {
		t.Errorf("Limits() = %+v, want %+v", got, want)
	}
}

// A STATED LIMIT MAY ONLY LOWER A DOCUMENTED ONE — it is for a gateway that
// accepts less than the model does. Raising one is refused naming the field,
// and the limits never exceed the documented ones even for a caller that never
// validated, because the provider would refuse whatever was sent past them.
func TestAStatedLimitMayOnlyLowerADocumentedOne(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		provider config.EmbeddingProvider
		field    string
	}{
		"input":  {config.EmbeddingProvider{MaxInputTokens: 8193}, "max_input_tokens"},
		"count":  {config.EmbeddingProvider{MaxBatchInputs: 2049}, "max_batch_inputs"},
		"tokens": {config.EmbeddingProvider{MaxBatchTokens: 300_001}, "max_batch_tokens"},
	} {
		t.Run(name, func(t *testing.T) {
			e := tc.provider
			e.Model = "text-embedding-3-small"
			err := embeddingCompany(t, &e).Validate()
			if err == nil {
				t.Fatalf("%s above the documented limit was accepted", tc.field)
			}
			if !errors.Is(err, config.ErrOutOfRange) {
				t.Errorf("refusal %v is not ErrOutOfRange", err)
			}
			for _, want := range []string{tc.field, "only lower"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal = %q, want it to say %q", err, want)
				}
			}
			documented := (&config.EmbeddingProvider{Model: e.Model}).Limits()
			if got := e.Limits(); got != documented {
				t.Errorf("a raised %s resolved to %+v, past the documented %+v",
					tc.field, got, documented)
			}
		})
	}

	e := &config.EmbeddingProvider{
		Model: "text-embedding-3-small", MaxInputTokens: 4096, MaxBatchInputs: 16,
		MaxBatchTokens: 100_000,
	}
	if err := embeddingCompany(t, e).Validate(); err != nil {
		t.Fatalf("lowering every documented limit was refused: %v", err)
	}
	want := config.EmbeddingLimits{InputBytes: 4096, BatchInputs: 16, BatchBytes: 100_000}
	if got := e.Limits(); got != want {
		t.Errorf("Limits() = %+v, want %+v", got, want)
	}
}

// ONE INPUT MUST FIT ONE REQUEST, so a gateway whose request is smaller than
// the model's window bounds the input too: otherwise the embedder would accept
// an input no request could carry.
func TestTheInputBoundFitsInsideOneRequest(t *testing.T) {
	t.Parallel()
	e := &config.EmbeddingProvider{
		Model: "bge-m3", Dimensions: 1024,
		MaxInputTokens: 8192, MaxBatchInputs: 8, MaxBatchTokens: 4096,
	}
	got := e.Limits()
	if want := 4096 - config.EmbeddingWrapTokens; got.InputBytes != want {
		t.Errorf("InputBytes = %d, want %d — the request's tokens less the wrap", got.InputBytes, want)
	}
	if got.InputBytes+got.InputOverhead > got.BatchBytes {
		t.Errorf("an input at its bound costs %d, past the %d one request takes",
			got.InputBytes+got.InputOverhead, got.BatchBytes)
	}
}

// ZERO IS "THE MODEL'S OWN", so a negative number is not a setting, and a
// window too small to hold a character past the wrap allowance is refused at
// the field rather than built into an embedder that refuses all text.
func TestALimitThatCannotBeASettingIsRefused(t *testing.T) {
	t.Parallel()
	base := config.EmbeddingProvider{
		Model: "bge-m3", Dimensions: 1024,
		MaxInputTokens: 512, MaxBatchInputs: 8, MaxBatchTokens: 4096,
	}
	floor := config.EmbeddingWrapTokens + config.MinEmbeddingInputBytes
	for name, tc := range map[string]struct {
		edit   func(*config.EmbeddingProvider)
		accept bool
		field  string
	}{
		"negative window":             {func(e *config.EmbeddingProvider) { e.MaxInputTokens = -1 }, false, "max_input_tokens"},
		"negative count":              {func(e *config.EmbeddingProvider) { e.MaxBatchInputs = -1 }, false, "max_batch_inputs"},
		"negative total":              {func(e *config.EmbeddingProvider) { e.MaxBatchTokens = -1 }, false, "max_batch_tokens"},
		"a window that is all wrap":   {func(e *config.EmbeddingProvider) { e.MaxInputTokens = config.EmbeddingWrapTokens }, false, "max_input_tokens"},
		"a window one short of text":  {func(e *config.EmbeddingProvider) { e.MaxInputTokens = floor - 1 }, false, "max_input_tokens"},
		"a window holding one char":   {func(e *config.EmbeddingProvider) { e.MaxInputTokens = floor }, true, ""},
		"a request that is all wrap":  {func(e *config.EmbeddingProvider) { e.MaxBatchTokens = config.EmbeddingWrapTokens }, false, "max_batch_tokens"},
		"a request holding one char":  {func(e *config.EmbeddingProvider) { e.MaxBatchTokens = floor }, true, ""},
		"a request of a single input": {func(e *config.EmbeddingProvider) { e.MaxBatchInputs = 1 }, true, ""},
	} {
		t.Run(name, func(t *testing.T) {
			e := base
			tc.edit(&e)
			err := embeddingCompany(t, &e).Validate()
			if tc.accept {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if got := e.Limits(); got.InputBytes < config.MinEmbeddingInputBytes {
					t.Errorf("accepted limits %+v cannot hold a character", got)
				}
				return
			}
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("refusal = %q, want it to name %q", err, tc.field)
			}
		})
	}
}

// EVERY ZERO IN THE TABLE SAYS WHY. A zero limit is "not documented", and the
// refusal an operator gets quotes the entry's reason — so an entry with a zero
// and no reason is a refusal that cannot say what to look up, and an entry
// with a reason and no zero is a stale note claiming a gap that was filled.
func TestEveryLimitTheTableLacksSaysWhy(t *testing.T) {
	t.Parallel()
	if len(config.EmbeddingModels) == 0 {
		t.Fatal("the model table is empty — this guard was certifying nothing")
	}
	for name, model := range config.EmbeddingModels {
		missing := model.InputTokens == 0 || model.BatchInputs == 0 || model.BatchTokens == 0
		switch {
		case missing && model.Undocumented == "":
			t.Errorf("%s lacks a limit and says nothing about why", name)
		case !missing && model.Undocumented != "":
			t.Errorf("%s documents every limit but still says %q", name, model.Undocumented)
		}
		if model.Width <= 0 {
			t.Errorf("%s has no width", name)
		}
		if model.InputTokens > 0 && model.InputTokens-model.WrapTokens < config.MinEmbeddingInputBytes {
			t.Errorf("%s's window cannot hold a character past its wrap allowance", name)
		}
	}
}
