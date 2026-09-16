package embeddings

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/respjson"

	"github.com/crewlet/crewlet/internal/providers/llm/httpapi"
)

// KeyEnv is the conventional variable consulted when a config names no key.
//
// The same one the chat backend uses, deliberately: a company that
// configured OpenAI for its models has already exported it, and asking for a
// second variable holding the same key is a setup step that exists only to
// be forgotten.
const KeyEnv = "OPENAI_API_KEY"

// Defaults.
const (
	DefaultBaseURL = "https://api.openai.com/v1"

	// DefaultTimeout bounds one embedding call.
	//
	// SHORT, and much shorter than a completion's, because of where this
	// sits: a turn-start prefetch runs it before a person sees anything,
	// and the caller's fallback for a slow embedder — no similarity
	// search — is cheap. Waiting two minutes to avoid it would be the
	// wrong trade in the one place the trade is obvious.
	DefaultTimeout = 15 * time.Second
)

// Config builds an embedder.
type Config struct {
	// Model is the embedding model id. Required: a default here would be
	// a width the store was not sized for.
	Model string

	// Dimensions is the configured vector width, which the store's columns
	// are sized from. Required for the same reason.
	Dimensions int

	APIKey  string
	BaseURL string
	Timeout time.Duration

	// HTTPClient is the caller's transport, or nil for one built here.
	HTTPClient *http.Client

	// LookupEnv resolves the conventional key. Nil takes the process
	// environment.
	LookupEnv func(string) string
}

// Provider is an OpenAI-compatible embedder.
//
// ONE BACKEND for both configured types, because the difference between
// `openai` and `openai-compatible` is a base URL rather than a protocol —
// the same reasoning the chat backend states, and the reason a local
// embedding server works with no code here at all.
type Provider struct {
	client sdk.Client
	model  string
	width  int
}

// BatchEmbedder RATHER THAN Embedder, because the embed duty in internal/engine
// reaches EmbedBatch through a type assertion, and an assertion that fails does
// not fail loudly — it silently declines to run the duty at all, leaving a
// company's whole corpus unsearchable by meaning and nothing in the log to say
// why. Asserting only the narrower interface here would let this signature and
// that assertion drift apart with nothing to notice.
var _ BatchEmbedder = (*Provider)(nil)

// New builds the provider.
func New(cfg Config) (*Provider, error) {
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("embeddings: name the embedding model")
	}
	if cfg.Dimensions <= 0 {
		return nil, errors.New("embeddings: name the vector width; the store's " +
			"columns are sized from it")
	}
	key := strings.TrimSpace(cfg.APIKey)
	if key == "" {
		lookup := cfg.LookupEnv
		if lookup == nil {
			lookup = os.Getenv
		}
		key = strings.TrimSpace(lookup(KeyEnv))
	}

	baseURL := strings.TrimSpace(cfg.BaseURL)
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	opts := []option.RequestOption{
		option.WithBaseURL(baseURL),
		option.WithRequestTimeout(timeout),
		// NO SDK RETRIES, for the reason the chat backend gives: its
		// defaults fire on the whole 429/5xx set, which is exactly what a
		// caller needs to see rather than have spent for it. Here the
		// caller's answer is simply "no vector", which is cheaper than
		// any retry the SDK could do.
		option.WithMaxRetries(0),
	}
	if key != "" {
		opts = append(opts, option.WithAPIKey(key))
	}

	// A transport the caller supplied is used as given; otherwise the
	// engine's shared one. NOTHING HERE OWNS IT — see [httpapi.NewHTTPClient]
	// for why this provider has no Close.
	if cfg.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	} else {
		opts = append(opts, option.WithHTTPClient(httpapi.NewHTTPClient()))
	}

	return &Provider{
		client: sdk.NewClient(opts...),
		model:  cfg.Model, width: cfg.Dimensions,
	}, nil
}

// Width implements [Embedder].
func (p *Provider) Width() int { return p.width }

// EmbedBatch implements [BatchEmbedder].
//
// ONE-TO-ONE WITH THE INPUT, positionally, which is the contract's whole
// content: the caller matches vectors to documents by index, so an empty or
// unembeddable input must still occupy its slot. It does, as a nil vector,
// rather than being dropped — dropping one would re-file every document after
// it onto the wrong vector, silently, and the only symptom would be a search
// that returns the wrong answers.
//
// WHICH input an item answers is the provider's own index wherever the
// response carries one, since the API documents that results may come back
// out of order — and the mapping those indices describe is refused outright
// unless it covers every input exactly once. See [Provider.assignment].
func (p *Provider) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	// The inputs the provider is actually asked for, with the slot each
	// one came from — an empty input is not sent and not billed for.
	input := make([]string, 0, len(texts))
	slot := make([]int, 0, len(texts))
	for i, text := range texts {
		if normalized := normalize(text); normalized != "" {
			input = append(input, normalized)
			slot = append(slot, i)
		}
	}
	if len(input) == 0 {
		return out, nil
	}
	res, err := p.client.Embeddings.New(ctx, sdk.EmbeddingNewParams{
		Model:      p.model,
		Input:      sdk.EmbeddingNewParamsInputUnion{OfArrayOfStrings: input},
		Dimensions: sdk.Int(int64(p.width)),
	})
	if err != nil {
		return nil, fmt.Errorf("embeddings: %s: %w", p.model, err)
	}
	// THE WHOLE MAPPING IS RESOLVED BEFORE ONE SLOT IS WRITTEN — see
	// [Provider.assignment] for why a per-item decision cannot be made
	// safely.
	at, err := p.assignment(res.Data, len(input))
	if err != nil {
		return nil, err
	}
	for i, item := range res.Data {
		raw := item.Embedding
		vector := make([]float32, len(raw))
		for j, v := range raw {
			vector[j] = float32(v)
		}
		checked, err := checkedWidth(vector, p.width, p.model)
		if err != nil {
			return nil, err
		}
		// at[i] is the INPUT this item answers; slot maps that back to
		// the caller's own text, which is a different position whenever
		// an empty input was skipped on the way out.
		out[slot[at[i]]] = checked
	}
	return out, nil
}

// assignment resolves which input each item of a batch response answers.
//
// It returns one input position per item, in item order — a COMPLETE
// one-to-one mapping onto the inputs — or an error, and there is no third
// outcome: a response this cannot read as a bijection yields no vectors at
// all rather than the ones it could place.
//
// # Why the mapping is verified whole rather than applied per item
//
// Counting the items says the answer is the right SIZE and says nothing about
// where they go. Two items carrying the same index pass that count, and
// writing each one as it arrives overwrites the first input's vector with the
// second's and leaves some other input nil — a full-length slice with a hole
// in it, which is the worst shape this can return. The embed duty that calls
// this — search.Embedder — reads both halves of it as good news: it publishes
// the overwritten vector as that document's own, which nothing can ever detect
// afterwards because a stored vector carries no evidence of the text it came
// from, and it reads the hole as "a document with nothing to embed", which
// selects that document again on the next pass and every pass after it, for
// ever. So the mapping is checked to be a bijection before it is used — every
// index inside the batch, no index twice, and exactly as many items as inputs,
// which together leave no input uncovered.
//
// # Why a MISSING index is a property of the response, not of an item
//
// The field is required by the API, but this backend also serves
// OpenAI-compatible servers, and one that omits it decodes as index 0 on
// every item — filing an entire batch onto the first input. The JSON
// metadata is what separates "the server said 0" from "the server said
// nothing", so presence is read there rather than from the value. It is then
// read across the whole response: an item's arrival position and its index
// are two different authorities about which input it answers, and a response
// that offers one for some items and the other for the rest gives no way to
// tell which of the two is lying, so it is refused rather than resolved.
//
// # And the field has THREE states, not two
//
// [respjson.Field.Valid] is false for a field that was OMITTED, one that came
// back JSON `null`, and one whose value is not an index at all (an object, a
// string that is not a number) — and only the first is a server making no
// claim. Read as two states, a response whose every index is null or garbage
// counts as "no indices at all" and silently takes the positional fallback:
// the one outcome this whole function exists to refuse, arrived at by
// reading a claim the server DID make and this code could not parse.
// [respjson.Field.Raw] is what separates them — [respjson.Omitted] is the
// empty string, `null` and an unreadable value are not — so a present index
// that names no input is refused with the rest, and a server that simply
// leaves the field out keeps working.
func (p *Provider) assignment(data []sdk.Embedding, inputs int) ([]int, error) {
	if len(data) != inputs {
		return nil, fmt.Errorf("embeddings: %s returned %d vectors for %d "+
			"inputs — a caller matches them positionally, so a short answer "+
			"is not a partial result but a re-filing of every document after "+
			"the gap", p.model, len(data), inputs)
	}
	indexed, unreadable := 0, 0
	for _, item := range data {
		switch {
		case item.JSON.Index.Valid():
			indexed++
		case item.JSON.Index.Raw() != respjson.Omitted:
			// PRESENT AND UNREADABLE: the item carries an index
			// field whose value is not an input number — a JSON
			// null, or something the decoder could not read as one.
			unreadable++
		}
	}
	if unreadable > 0 {
		return nil, fmt.Errorf("embeddings: %s carried an index on %d of %d "+
			"vectors that does not name an input — a null or a non-integer is "+
			"still the server saying which input an item answers, and filing "+
			"those items by arrival position instead is exactly the silent "+
			"re-filing the index exists to prevent", p.model, unreadable,
			len(data))
	}
	if indexed != 0 && indexed != len(data) {
		return nil, fmt.Errorf("embeddings: %s gave %d of %d vectors an index "+
			"— position and index are two different claims about which input "+
			"an item answers, and a response that mixes them says nothing "+
			"about which claim to believe", p.model, indexed, len(data))
	}
	at := make([]int, len(data))
	taken := make([]bool, inputs)
	for i, item := range data {
		// int64 THROUGHOUT THE RANGE CHECK: int(item.Index) truncates on
		// a 32-bit build, where an index of 2^32 would land inside the
		// batch as 0 and be filed as a legitimate answer.
		idx := int64(i)
		if indexed > 0 {
			idx = item.Index
		}
		if idx < 0 || idx >= int64(inputs) {
			return nil, fmt.Errorf("embeddings: %s answered input %d of a "+
				"%d-input batch — an index outside the batch names no "+
				"document, and filing that item by its arrival position "+
				"instead is exactly the silent re-filing the index exists "+
				"to prevent", p.model, idx, inputs)
		}
		if taken[idx] {
			return nil, fmt.Errorf("embeddings: %s answered input %d twice in "+
				"one batch — one document would be stored holding another's "+
				"vector and a third would be left with none, and neither is "+
				"visible afterwards", p.model, idx)
		}
		taken[idx] = true
		at[i] = int(idx)
	}
	return at, nil
}

// Embed implements [Embedder].
func (p *Provider) Embed(ctx context.Context, text string) ([]float32, error) {
	normalized := normalize(text)
	if normalized == "" {
		return nil, ErrEmpty
	}
	res, err := p.client.Embeddings.New(ctx, sdk.EmbeddingNewParams{
		Model: p.model,
		Input: sdk.EmbeddingNewParamsInputUnion{OfString: sdk.String(normalized)},
		// THE WIDTH IS ASKED FOR, not just checked. The third-generation
		// models support truncation to a shorter width, so a company that
		// sized its store at 768 gets 768 rather than a refusal — and a
		// model that ignores the parameter still fails the check below,
		// which is the case this cannot fix.
		Dimensions: sdk.Int(int64(p.width)),
	})
	if err != nil {
		return nil, fmt.Errorf("embeddings: %s: %w", p.model, err)
	}
	if len(res.Data) == 0 {
		return nil, fmt.Errorf("embeddings: %s returned no vector", p.model)
	}
	// float64 on the wire, float32 in the store. The narrowing is lossless
	// for what these values are — unit-ish components with a handful of
	// significant digits — and halves what a company's whole episode
	// history costs to hold.
	raw := res.Data[0].Embedding
	vector := make([]float32, len(raw))
	for i, v := range raw {
		vector[i] = float32(v)
	}
	return checkedWidth(vector, p.width, p.model)
}
