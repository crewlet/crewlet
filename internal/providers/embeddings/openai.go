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
	owned  *http.Client
}

var _ Embedder = (*Provider)(nil)

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

	// A transport the caller supplied stays the caller's to close; one
	// built here belongs to this provider.
	var owned *http.Client
	if cfg.HTTPClient == nil {
		owned = httpapi.NewHTTPClient()
		opts = append(opts, option.WithHTTPClient(owned))
	} else {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	}

	return &Provider{
		client: sdk.NewClient(opts...),
		model:  cfg.Model, width: cfg.Dimensions, owned: owned,
	}, nil
}

// Width implements [Embedder].
func (p *Provider) Width() int { return p.width }

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

// Close releases a transport this provider owns.
func (p *Provider) Close() {
	if p != nil && p.owned != nil {
		p.owned.CloseIdleConnections()
	}
}
