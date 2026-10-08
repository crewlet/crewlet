package config_test

import (
	"errors"
	"regexp"
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
// re-embed the corpus — a provider bill for every document, for nothing. So the
// bound for the default provider's models is pinned here, at the value that
// keeps it where it is.
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

// THE WAY OUT OF A MISSING LIMIT CLEARS IT. A model this build does not know
// is refused for each limit it leaves unstated, and the refusal offers naming a
// model the table carries instead — so every model it offers must be one whose
// limit the table carries, or the advice is a second refusal for the same
// field: two of the four models document no request limits at all.
func TestAMissingLimitSuggestsOnlyModelsThatResolveIt(t *testing.T) {
	t.Parallel()
	unknown := &config.EmbeddingProvider{
		Type: config.EmbeddingOpenAICompatible, Model: "bge-m3",
		BaseURL: "http://embed.example.com/v1", Dimensions: 1024,
	}
	suggestion := regexp.MustCompile(`name a model whose limit it carries \(([^)]+)\)`)
	for _, field := range []string{"max_input_tokens", "max_batch_inputs", "max_batch_tokens"} {
		t.Run(field, func(t *testing.T) {
			refusal, refused := limitRefusals(embeddingCompany(t, unknown).Validate())[field]
			if !refused {
				t.Fatalf("an unknown model with no %s was not refused for it", field)
			}
			found := suggestion.FindStringSubmatch(refusal)
			if found == nil {
				t.Fatalf("refusal %q names no model to switch to", refusal)
			}
			for _, model := range strings.Split(found[1], ", ") {
				switched := &config.EmbeddingProvider{Type: config.EmbeddingOpenAI, Model: model}
				if again, ok := limitRefusals(embeddingCompany(t, switched).Validate())[field]; ok {
					t.Errorf("the refusal suggests %s, which is refused for %s too: %s",
						model, field, again)
				}
			}
		})
	}
}

// limitRefusals is each embedding-limit field a validation refused, keyed by
// the field, with the refusal's message.
func limitRefusals(err error) map[string]string {
	out := map[string]string{}
	for _, p := range config.Problems(err) {
		if field, ok := strings.CutPrefix(p.Path, "providers.embeddings."); ok {
			out[field] = p.Message
		}
	}
	return out
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

// THE PER-INPUT BOUND IS RESOLVED ONCE, and the limits carry it.
//
// InputBound is the conversion from a window in tokens to a bound in bytes;
// Limits takes it rather than computing its own, so a reader that needs only
// the bound — the search eval, which knows the model and not the company's
// request limits — reports what the embedder enforces. It is known wherever
// the window is: a model documenting its window and nothing per request still
// has a bound, though it has no limits to build an embedder from.
func TestThePerInputBoundIsResolvedOnceAndTheLimitsCarryIt(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		provider config.EmbeddingProvider
		bound    int
		complete bool
	}{
		"OpenAI's own window": {
			config.EmbeddingProvider{Model: "text-embedding-3-small"}, 8192, true},
		"a lowered window": {
			config.EmbeddingProvider{Model: "text-embedding-3-small", MaxInputTokens: 2000}, 2000, true},
		"a request total under the window bounds the input too": {
			config.EmbeddingProvider{Model: "text-embedding-3-small", MaxBatchTokens: 4000}, 4000, true},
		"a documented window with no request limits": {
			config.EmbeddingProvider{Model: "gemini-embedding-001"}, 2048 - config.EmbeddingWrapTokens, false},
		"the same window once the request limits are stated": {
			config.EmbeddingProvider{Model: "gemini-embedding-001", MaxBatchInputs: 8, MaxBatchTokens: 20_000},
			2048 - config.EmbeddingWrapTokens, true},
		"a stated request total under that window": {
			config.EmbeddingProvider{Model: "gemini-embedding-001", MaxBatchInputs: 8, MaxBatchTokens: 1000},
			1000 - config.EmbeddingWrapTokens, true},
		"an unknown model's stated window": {
			config.EmbeddingProvider{Model: "bge-m3", MaxInputTokens: 512}, 512 - config.EmbeddingWrapTokens, false},
		"an unknown model stating nothing": {
			config.EmbeddingProvider{Model: "bge-m3"}, 0, false},
	} {
		t.Run(name, func(t *testing.T) {
			e := tc.provider
			if got := e.InputBound(); got != tc.bound {
				t.Errorf("InputBound() = %d, want %d", got, tc.bound)
			}
			limits := e.Limits()
			if tc.complete != (limits != config.EmbeddingLimits{}) {
				t.Fatalf("Limits() = %+v; complete should be %v", limits, tc.complete)
			}
			if tc.complete && limits.InputBytes != e.InputBound() {
				t.Errorf("Limits().InputBytes = %d but InputBound() = %d — two "+
					"conversions where there is one", limits.InputBytes, e.InputBound())
			}
		})
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
