// Package modelcatalog is what a model step suggests: the models each coding
// harness's vendor offers, read from models.dev's public catalogue.
//
// # Not a model vendor, and never handed a credential
//
// Harnesses are the only LLM backend and there is no HTTP client to a model
// vendor (`docs/concepts/llm-backends.md`). This package does not become one:
// https://models.dev/api.json is a public, keyless catalogue (the one OpenCode
// itself reads), so the list a person picks a model from is read without a
// credential leaving the engine, and whether a plan reaches a model it lists is
// what the test call answers. The alternative, a vendor's `GET /v1/models` with
// the key being typed, would have been the first vendor client in the tree and
// would have sent a credential somewhere only to name models.
//
// # Who has to agree on it
//
// Nobody but this node: the catalogue is a cache of a public document, held
// in memory, lost on restart, and refetched by whichever node is asked. Two
// nodes answering a day apart may list different models, which is what the
// document itself did in that day.
//
// # Fresh for a day, then asked whether it changed
//
// The document is some 6 MB and changes a few times a day at most, so a copy is
// served for [Fresh] and then revalidated with `If-None-Match`: a catalogue
// that did not move costs a 304. A fetch that fails keeps the last good copy
// and says how old it is; with no copy at all the answer is `source: none`
// with the reason, and the page falls back to the vendor's documentation and a
// free-text field. A failure is not retried for [RetryAfter], so a node whose
// egress is blackholed pays one [FetchBudget] wait per window rather than one
// per page view.
package modelcatalog

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("modelcatalog")

// URL is the catalogue every node reads.
const URL = "https://models.dev/api.json"

// SourceModelsDev and SourceNone are what an answer says it came from: the
// catalogue, or nothing because no copy could be had.
const (
	SourceModelsDev = "models.dev"
	SourceNone      = "none"
)

// Fresh is how long a copy is served before it is revalidated: the catalogue
// moves a few times a day at most, and a model a page does not suggest is
// still accepted as free text.
const Fresh = 24 * time.Hour

// FetchBudget bounds one fetch, the 6 MB body included: a few seconds on any
// link that reaches the internet at all, with room for a slow one, and short of
// the 60 s a person waits on a page before calling it broken.
const FetchBudget = 30 * time.Second

// RetryAfter is how long a failed fetch is not tried again: a node whose
// egress is cut pays one [FetchBudget] wait in this window instead of one per
// page view, and one that comes back online is listing models again within it.
const RetryAfter = 10 * time.Minute

// MaxBytes bounds the body read: the catalogue is some 6 MB, so 16 MiB is room
// for it to more than double before a fetch is refused rather than read.
const MaxBytes = 16 << 20

// ErrUnknownProvider is a provider the catalogue lists nothing under.
var ErrUnknownProvider = errors.New("modelcatalog: no such provider in the catalogue")

// Model is one model as the catalogue lists it, in the fields a model step
// shows.
type Model struct {
	// ID is what the harness takes as its model argument.
	ID   string `json:"id"`
	Name string `json:"name"`
	// Family groups a model with its siblings (`claude-opus`), empty where
	// the catalogue names none.
	Family string `json:"family,omitempty"`
	// Released is the catalogue's release date, as it spells it
	// (`2026-06-07`), empty where it names none.
	Released string `json:"released,omitempty"`
	// ToolCall is whether the model calls tools, which every phase needs.
	ToolCall bool `json:"tool_call"`
}

// Answer is one provider's models, and where they came from.
type Answer struct {
	// Source is [SourceModelsDev], or [SourceNone] when no copy could be
	// had, with Reason saying why.
	Source string
	// FetchedAt is when the copy answered was fetched or last revalidated;
	// zero with no copy.
	FetchedAt time.Time
	// Models are newest first, ties by id. Never nil.
	Models []Model
	// Reason says why the answer is not a fresh copy: the fetch that failed,
	// and how old the copy served instead is. Empty for a fresh copy.
	Reason string
}

// Options wire a catalogue.
type Options struct {
	// URL overrides [URL], for a test serving its own copy.
	URL string
	// Client overrides the shared transport's client, for a test.
	Client *http.Client
	// Now is injectable so a test can move the clock across [Fresh].
	Now func() time.Time
}

// Catalog is this node's copy of the catalogue. Safe for concurrent use; one
// fetch runs at a time, and a reader arriving during it waits for its answer
// rather than starting a second.
type Catalog struct {
	url    string
	client *http.Client
	now    func() time.Time

	mu        sync.Mutex
	providers map[string][]Model
	etag      string
	fetchedAt time.Time
	failedAt  time.Time
	failure   string
}

// New builds a catalogue that has fetched nothing yet: the first question
// fetches it.
func New(opts Options) *Catalog {
	c := &Catalog{url: cmp.Or(opts.URL, URL), client: opts.Client, now: opts.Now}
	if c.client == nil {
		c.client = httpx.Client(FetchBudget)
	}
	if c.now == nil {
		c.now = func() time.Time { return time.Now().UTC() }
	}
	return c
}

// Provider answers the models one provider offers, fetching or revalidating
// the catalogue first when the copy is older than [Fresh]. A provider the copy
// does not list is [ErrUnknownProvider]; with no copy at all every provider is
// answered `source: none`, since nothing can say it does not exist.
func (c *Catalog) Provider(ctx context.Context, provider string) (Answer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.providers == nil || now.Sub(c.fetchedAt) >= Fresh {
		if c.failedAt.IsZero() || now.Sub(c.failedAt) >= RetryAfter {
			c.refresh(ctx, now)
		}
	}
	if c.providers == nil {
		return Answer{Source: SourceNone, Models: []Model{},
			Reason: "the model catalogue (" + c.url + ") could not be read from this node: " +
				c.failure}, nil
	}
	models, ok := c.providers[provider]
	if !ok {
		return Answer{}, fmt.Errorf("%w: %q", ErrUnknownProvider, provider)
	}
	answer := Answer{Source: SourceModelsDev, FetchedAt: c.fetchedAt,
		Models: slices.Clone(models)}
	if now.Sub(c.fetchedAt) >= Fresh && c.failure != "" {
		answer.Reason = fmt.Sprintf("the model catalogue could not be refreshed (%s), so this "+
			"list is the copy fetched %s ago", c.failure,
			now.Sub(c.fetchedAt).Truncate(time.Minute))
	}
	return answer, nil
}

// refresh fetches the catalogue, conditionally when a copy is held, and keeps
// what it learned; a failure keeps the copy and records why. Called with mu
// held.
func (c *Catalog) refresh(ctx context.Context, now time.Time) {
	// THE FETCH'S OWN BUDGET, not the asker's: a page that gave up should
	// not leave the next one with nothing, and the budget bounds it anyway.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), FetchBudget)
	defer cancel()
	providers, etag, err := c.fetch(ctx)
	switch {
	case errors.Is(err, errNotModified):
		c.fetchedAt, c.failedAt, c.failure = now, time.Time{}, ""
	case err != nil:
		c.failedAt, c.failure = now, err.Error()
		log.WarnContext(ctx, "model_catalogue_unreadable", "url", c.url, "error", err,
			"held", c.providers != nil, "retry_after", RetryAfter)
	default:
		c.providers, c.etag = providers, etag
		c.fetchedAt, c.failedAt, c.failure = now, time.Time{}, ""
		log.InfoContext(ctx, "model_catalogue_fetched", "url", c.url, "providers", len(providers))
	}
}

// errNotModified is a revalidation the catalogue answered 304.
var errNotModified = errors.New("modelcatalog: not modified")

// fetch reads the catalogue, or [errNotModified] when it has not moved since
// the copy held.
func (c *Catalog) fetch(ctx context.Context) (map[string][]Model, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/json")
	if c.providers != nil && c.etag != "" {
		req.Header.Set("If-None-Match", c.etag)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch the catalogue: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotModified && c.providers != nil:
		return nil, "", errNotModified
	case resp.StatusCode != http.StatusOK:
		return nil, "", fmt.Errorf("the catalogue answered %s: %s", resp.Status,
			httpx.ReadRefusal(resp))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read the catalogue: %w", err)
	}
	if len(body) > MaxBytes {
		return nil, "", fmt.Errorf("the catalogue is over %d MiB, so it was not read", MaxBytes>>20)
	}
	providers, err := parse(body)
	if err != nil {
		return nil, "", err
	}
	return providers, resp.Header.Get("ETag"), nil
}

// rawProvider is one provider as the catalogue spells it, in the fields read.
type rawProvider struct {
	Models map[string]struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Family      string `json:"family"`
		ReleaseDate string `json:"release_date"`
		ToolCall    bool   `json:"tool_call"`
	} `json:"models"`
}

// parse reads the catalogue into each provider's models, newest first.
func parse(body []byte) (map[string][]Model, error) {
	var raw map[string]rawProvider
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("the catalogue is not the shape this build reads: %w", err)
	}
	if len(raw) == 0 {
		return nil, errors.New("the catalogue lists no provider")
	}
	out := make(map[string][]Model, len(raw))
	for id, p := range raw {
		models := make([]Model, 0, len(p.Models))
		for key, m := range p.Models {
			models = append(models, Model{ID: cmp.Or(m.ID, key), Name: cmp.Or(m.Name, key),
				Family: m.Family, Released: m.ReleaseDate, ToolCall: m.ToolCall})
		}
		slices.SortFunc(models, func(a, b Model) int {
			return cmp.Or(cmp.Compare(b.Released, a.Released), cmp.Compare(a.ID, b.ID))
		})
		out[id] = models
	}
	return out, nil
}
