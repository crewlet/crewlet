package embeddings

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/respjson"

	"github.com/crewlet/crewlet/internal/providers/llm/httpapi"
	llmopenai "github.com/crewlet/crewlet/internal/providers/llm/openai"
)

// Defaults.
const (
	DefaultBaseURL = "https://api.openai.com/v1"

	// EmbedTimeout bounds one [Provider.Embed] request: the ceiling for a
	// caller that sets no tighter deadline of its own. An episode embedded
	// after its turn has ended is held to the same fifteen seconds, by a
	// deadline it sets for its own reason (learning.DefaultEmbedTimeout).
	//
	// SHORT, and much shorter than a completion's, because every caller of
	// a single embedding has something waiting behind it and a cheap answer
	// to a slow embedder — no vector, so no similarity search — while
	// waiting two minutes to avoid that would be the wrong trade.
	//
	// A caller with a tighter budget sets a deadline on its context, which
	// wins, and TWO BUDGETS of two seconds do. A search's query vector is
	// held to search.QueryEmbedBudget, and so, BY REFERENCE to it
	// (prefetch.EmbedBudget), are a turn's ask at turn start and the hint a
	// recall tool passes: a person or a turn is waiting on each. A note as
	// it is written is held to learning.DiaryEmbedBudget, a budget of its
	// OWN that shares only the figure: the reflect_and_persist call keeping
	// the note has a model waiting on it, and a note that misses the budget
	// is filled later rather than lost. Changing one leaves the other where
	// it is.
	EmbedTimeout = 15 * time.Second

	// BatchTimeout bounds one request of a [Provider.EmbedBatch] call.
	//
	// ITS OWN, because the two calls have nothing in common but the wire:
	// a single Embed carries one short text, while a batch request carries
	// up to the model's request total — 300 000 tokens on OpenAI — and a
	// ceiling chosen for one input is not a figure about how long a server
	// takes over that many.
	//
	// SIXTY SECONDS, derived from what the batch caller's tick allows. The
	// knowledge corpus duty (internal/search) plans its own requests and
	// sends each through a call of its own, at most thirty-two a tick on a
	// one-minute interval, and the engine cuts a tick off once it has gone
	// five minutes without progress (internal/engine's embedTickBudget),
	// counting each answered request as progress. A request at this
	// ceiling is a fifth of that budget, so one that runs to it is never
	// itself mistaken for a wedge.
	//
	// NOTHING HAS MEASURED how long a server takes to embed a full request
	// — not OpenAI's, and not a CPU-hosted one, which is the deployment
	// most likely to need longer. The lever for a slow server is not this
	// ceiling but the request's size: `max_batch_tokens` lowers what one
	// request carries, and with it how long the server takes over it,
	// without moving the bound a wedged call is held to. It is a timing
	// lever only while it stays at or above the model's per-input window:
	// one input must fit one request, so a request total below the window
	// is also the bound one input may hold
	// (config.EmbeddingProvider.InputBound), and lowering it there
	// shortens the opening the corpus embeds of every long source — a new
	// digest for each, and each embedded again.
	BatchTimeout = time.Minute
)

// Config builds an embedder.
type Config struct {
	// Model is the embedding model id. Required: a default here would be
	// a width the store was not sized for.
	Model string

	// Dimensions is the configured vector width, which the store's columns
	// are sized from. Required for the same reason.
	Dimensions int

	// OmitDimensions leaves the `dimensions` parameter out of every request.
	//
	// The zero value SENDS it, which is right for every endpoint but one
	// kind: an endpoint that shortens on request (OpenAI's third-generation
	// models) answers at the width the store was sized for, and one that
	// ignores the parameter is caught by the width check every response
	// passes. The one kind is an endpoint that documents the parameter as
	// unsupported, at the width it emits on its own — the configuration
	// decides that (config.EmbeddingProvider.SendsDimensions), where the
	// vendor's documentation is cited; nothing here knows which endpoint it
	// is talking to. The width check runs either way.
	OmitDimensions bool

	// APIKey is the credential, resolved. It is THE credential: nothing
	// here reads a variable, and the SDK's own OPENAI_API_KEY autoload is
	// overridden on every client, an empty key included. Which key an
	// embedder that names none runs on is a configuration rule
	// (config.EmbeddingProvider.ResolvedKey), decided where the document
	// and the secret store are both in reach.
	APIKey  string
	BaseURL string

	// Limits is what the model accepts, resolved from the configuration
	// (config.EmbeddingProvider.Limits). Required, for the reason the width
	// is: a default here would be a guess at somebody else's model, and
	// a guess too high is every long input refused by the provider.
	Limits Limits

	// HTTPClient is the caller's transport, or nil for one built here.
	HTTPClient *http.Client
}

// Provider is an OpenAI-compatible embedder.
//
// ONE BACKEND for both configured types, because the difference between
// `openai` and `openai-compatible` is a base URL rather than a protocol —
// the same reasoning the chat backend states, and the reason a local
// embedding server works with no code here at all.
type Provider struct {
	client   sdk.Client
	model    string
	width    int
	limits   Limits
	endpoint string

	// askWidth is whether a request carries `dimensions` ([Config.OmitDimensions]).
	askWidth bool
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
	if err := cfg.Limits.Validate(); err != nil {
		return nil, fmt.Errorf("embeddings: %s: %w", cfg.Model, err)
	}
	key := strings.TrimSpace(cfg.APIKey)

	baseURL := strings.TrimSpace(cfg.BaseURL)
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}

	// NOTHING FROM THE PROCESS ENVIRONMENT — the chat backend's rule and its
	// one implementation (an admin key, organization and project headers and
	// custom headers would otherwise reach whatever server this embedder
	// points at). Applied first, so the key below is the one that authorizes.
	opts := append(llmopenai.WithoutAmbientEnvironment(),
		option.WithBaseURL(baseURL),
		// The client's ceiling is the single call's; every request states
		// its own (EmbedTimeout, BatchTimeout), so this is never the one in
		// force — it is here so no request can go out with none.
		option.WithRequestTimeout(EmbedTimeout),
		// NO SDK RETRIES, for the reason the chat backend gives: its
		// defaults fire on the whole 429/5xx set, which is exactly what a
		// caller needs to see rather than have spent for it. The callers
		// here each have their own answer — a turn degrades, the corpus
		// asks again on its next tick — and the classified error is what
		// lets them choose it (see the package doc).
		option.WithMaxRetries(0),
		// ALWAYS, AN EMPTY KEY INCLUDED. The SDK loads OPENAI_API_KEY from
		// the process environment at construction, so skipping this for an
		// empty key sent the ambient credential to whatever endpoint this
		// embedder points at — a self-hosted server included — which is a
		// key nobody configured, reaching a host that should never see it.
		option.WithAPIKey(key),
	)

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
		model:  cfg.Model, width: cfg.Dimensions, limits: cfg.Limits,
		askWidth: !cfg.OmitDimensions,
		endpoint: baseURL,
	}, nil
}

// Width implements [Embedder].
func (p *Provider) Width() int { return p.width }

// Model implements [Embedder].
func (p *Provider) Model() string { return p.model }

// Limits implements [Embedder].
func (p *Provider) Limits() Limits { return p.limits }

// Endpoint is the base URL this provider sends to — [DefaultBaseURL] where the
// configuration named none.
//
// Not part of [Embedder]: what a caller embeds is the same at any endpoint,
// but what an endpoint REFUSES is not, so the knowledge corpus duty and the
// memory fill key their memories of refused inputs on it (internal/engine),
// beside the model, the width and the limits.
func (p *Provider) Endpoint() string { return p.endpoint }

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
//
// AS MANY REQUESTS AS THE MODEL'S LIMITS NEED ([Limits.Requests]), one after
// another, each under [BatchTimeout]: a batch is a caller's unit of work and a
// request is the provider's, and the corpus's 128 inputs of up to 8 KiB are
// over OpenAI's 300 000-token request total whenever the text runs under 3.5
// bytes a token — code, markup, most scripts that are not Latin — which sent
// as one request was a batch refused on every tick for ever.
func (p *Provider) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	prepared := make([]string, len(texts))
	sizes := make([]int, len(texts))
	for i, text := range texts {
		prepared[i] = Prepare(text)
		sizes[i] = len(prepared[i])
	}
	// EVERY INPUT IS MEASURED BEFORE ANYTHING IS SENT, so an input past
	// the bound costs no request — and no request the call has already paid
	// for is thrown away because a later input could never have been sent.
	groups, err := p.limits.requests(sizes)
	if err != nil {
		return nil, p.named(err)
	}
	for _, group := range groups {
		input := make([]string, len(group))
		for j, at := range group {
			input[j] = prepared[at]
		}
		vectors, err := p.request(ctx, input)
		if err != nil {
			return nil, err
		}
		// group maps each input of the request back to the caller's own
		// position, which differs from the request's whenever an empty
		// input was skipped or an earlier request took the inputs before.
		for j, vector := range vectors {
			out[group[j]] = vector
		}
	}
	return out, nil
}

// request sends one batch request — inputs already prepared, non-empty and
// inside the limits — and returns one vector per input, in input order.
func (p *Provider) request(ctx context.Context, input []string) ([][]float32, error) {
	res, err := p.client.Embeddings.New(ctx,
		p.params(sdk.EmbeddingNewParamsInputUnion{OfArrayOfStrings: input}),
		option.WithRequestTimeout(BatchTimeout))
	if err != nil {
		return nil, p.classify(err)
	}
	// THE WHOLE MAPPING IS RESOLVED BEFORE ONE SLOT IS WRITTEN — see
	// [Provider.assignment] for why a per-item decision cannot be made
	// safely.
	at, err := p.assignment(res.Data, len(input))
	if err != nil {
		return nil, err
	}
	out := make([][]float32, len(input))
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
		// at[i] is the INPUT this item answers.
		out[at[i]] = checked
	}
	return out, nil
}

// params is one request's parameters for input.
//
// THE WIDTH IS ASKED FOR, not just checked, wherever the endpoint takes it. The
// third-generation models support truncation to a shorter width, so a company
// that sized its store at 768 gets 768 rather than a refusal — and a model that
// ignores the parameter still fails the width check every response passes,
// which is the case asking cannot fix. An endpoint documented as not taking
// the parameter is not sent it ([Config.OmitDimensions]), and answers at the
// model's own width, which is then the configured one.
func (p *Provider) params(input sdk.EmbeddingNewParamsInputUnion) sdk.EmbeddingNewParams {
	params := sdk.EmbeddingNewParams{Model: p.model, Input: input}
	if p.askWidth {
		params.Dimensions = sdk.Int(int64(p.width))
	}
	return params
}

// named fills in the model on a [TooLongError] the limits raised, which know
// the bound but not whose it is.
func (p *Provider) named(err error) error {
	var long *TooLongError
	if errors.As(err, &long) {
		long.Model = p.model
	}
	return err
}

// classify turns an SDK failure into a classified [Error].
//
// A STATUS decides it where there is one ([classForStatus]). Without one, a
// deadline and a network failure are transient — the provider's own timeout
// and a caller's deadline alike, since asking again with more time may succeed
// — while a CANCELLED context is the caller's own doing and is left
// unclassified: it says nothing about the provider, and errors.Is(err,
// context.Canceled) still answers through the wrap.
func (p *Provider) classify(err error) error {
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		if class := classForStatus(apiErr.StatusCode); class != nil {
			return &Error{Model: p.model, Status: apiErr.StatusCode, Class: class, Err: err}
		}
		return fmt.Errorf("embeddings: %s: %w", p.model, err)
	}
	var netErr net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("embeddings: %s: %w", p.model, err)
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr):
		return &Error{Model: p.model, Class: ErrTransient, Err: err}
	}
	return fmt.Errorf("embeddings: %s: %w", p.model, err)
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
	prepared := Prepare(text)
	if prepared == "" {
		return nil, ErrEmpty
	}
	if len(prepared) > p.limits.InputBytes {
		return nil, &TooLongError{Model: p.model, Bytes: len(prepared), Limit: p.limits.InputBytes}
	}
	res, err := p.client.Embeddings.New(ctx,
		p.params(sdk.EmbeddingNewParamsInputUnion{OfString: sdk.String(prepared)}),
		option.WithRequestTimeout(EmbedTimeout))
	if err != nil {
		return nil, p.classify(err)
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
