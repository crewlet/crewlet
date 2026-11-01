package modelcatalog_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/modelcatalog"
)

const catalogue = `{
  "anthropic": {"id": "anthropic", "name": "Anthropic", "models": {
    "claude-sonnet-5": {"id": "claude-sonnet-5", "name": "Claude Sonnet 5", "family": "claude-sonnet",
      "release_date": "2026-02-01", "tool_call": true},
    "claude-opus-5-5": {"id": "claude-opus-5-5", "name": "Claude Opus 5.5", "family": "claude-opus",
      "release_date": "2026-09-01", "tool_call": true}}},
  "openai": {"id": "openai", "models": {"gpt-6.1-sol": {"name": "GPT-6.1 Sol", "tool_call": true}}}
}`

// served is a catalogue a case controls: its body and ETag, whether it is
// down, and what each request asked.
type served struct {
	mu       sync.Mutex
	body     string
	etag     string
	down     bool
	requests []string // each request's If-None-Match, "" for none
}

func (s *served) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.Header.Get("If-None-Match"))
	switch {
	case s.down:
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	case r.Header.Get("If-None-Match") == s.etag:
		w.WriteHeader(http.StatusNotModified)
	default:
		w.Header().Set("ETag", s.etag)
		_, _ = w.Write([]byte(s.body))
	}
}

func (s *served) asked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func (s *served) set(f func(*served)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

// clock is a time a case moves by hand.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func catalogOver(t *testing.T, s *served) (*modelcatalog.Catalog, *clock) {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	c := &clock{at: time.Date(2026, 10, 28, 20, 0, 0, 0, time.UTC)}
	return modelcatalog.New(modelcatalog.Options{URL: srv.URL, Client: srv.Client(), Now: c.now}), c
}

// A COPY IS SERVED FOR A DAY AND THEN ASKED WHETHER IT CHANGED.
//
// The catalogue is some 6 MB and moves a few times a day, so a page view
// that fetched it each time would move it per view; one that never asked
// again would suggest last month's models for ever. Within [Fresh] nothing is
// asked, and past it the copy's ETag is sent and a 304 keeps it.
func TestACopyIsServedForADayThenRevalidated(t *testing.T) {
	t.Parallel()
	s := &served{body: catalogue, etag: `"v1"`}
	cat, c := catalogOver(t, s)

	got, err := cat.Provider(t.Context(), "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != modelcatalog.SourceModelsDev || len(got.Models) != 2 || got.Reason != "" {
		t.Fatalf("first answer = %+v", got)
	}
	// NEWEST FIRST, which is what a person picking a model wants on top.
	if got.Models[0].ID != "claude-opus-5-5" || got.Models[0].Family != "claude-opus" ||
		got.Models[0].Released != "2026-09-01" || !got.Models[0].ToolCall {
		t.Errorf("first model = %+v", got.Models[0])
	}
	c.advance(modelcatalog.Fresh - time.Minute)
	if _, err := cat.Provider(t.Context(), "openai"); err != nil {
		t.Fatal(err)
	}
	if asked := s.asked(); len(asked) != 1 {
		t.Fatalf("a fresh copy was fetched again: %q", asked)
	}

	c.advance(time.Minute)
	got, err = cat.Provider(t.Context(), "anthropic")
	if err != nil || len(got.Models) != 2 {
		t.Fatalf("after revalidation: (%+v, %v)", got, err)
	}
	if asked := s.asked(); len(asked) != 2 || asked[1] != `"v1"` {
		t.Fatalf("the revalidation asked %q, want If-None-Match with the copy's ETag", asked)
	}
	if !got.FetchedAt.Equal(c.now()) {
		t.Errorf("a 304 did not renew the copy: fetched_at %v", got.FetchedAt)
	}

	// A CATALOGUE THAT MOVED replaces the copy.
	s.set(func(s *served) {
		s.etag = `"v2"`
		s.body = `{"anthropic": {"models": {"claude-haiku-5": {"name": "Claude Haiku 5"}}}}`
	})
	c.advance(modelcatalog.Fresh)
	got, err = cat.Provider(t.Context(), "anthropic")
	if err != nil || len(got.Models) != 1 || got.Models[0].ID != "claude-haiku-5" {
		t.Errorf("after the catalogue moved: (%+v, %v)", got, err)
	}
	if _, err := cat.Provider(t.Context(), "openai"); !errors.Is(err, modelcatalog.ErrUnknownProvider) {
		t.Errorf("a provider the new copy dropped = %v, want ErrUnknownProvider", err)
	}
}

// A CATALOGUE THAT CANNOT BE READ KEEPS THE LAST COPY, AND SAYS SO.
//
// An offline node with no copy answers `source: none` with the reason, so
// the page offers the docs link and free text; one with a copy serves it with
// its age. Neither asks again inside [RetryAfter], so a blackholed egress
// costs one wait per window rather than one per page view.
func TestAnUnreadableCatalogueKeepsTheLastCopyAndSaysHowOld(t *testing.T) {
	t.Parallel()
	s := &served{body: catalogue, etag: `"v1"`, down: true}
	cat, c := catalogOver(t, s)

	got, err := cat.Provider(t.Context(), "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != modelcatalog.SourceNone || got.Models == nil || len(got.Models) != 0 ||
		!strings.Contains(got.Reason, "502") {
		t.Fatalf("with no copy = %+v, want source none naming why", got)
	}
	c.advance(modelcatalog.RetryAfter - time.Second)
	_, _ = cat.Provider(t.Context(), "anthropic")
	if asked := s.asked(); len(asked) != 1 {
		t.Errorf("a failure was retried inside RetryAfter: %d requests", len(asked))
	}

	s.set(func(s *served) { s.down = false })
	c.advance(time.Second)
	if got, _ := cat.Provider(t.Context(), "anthropic"); got.Source != modelcatalog.SourceModelsDev {
		t.Fatalf("after the catalogue came back = %+v", got)
	}

	s.set(func(s *served) { s.down = true })
	c.advance(modelcatalog.Fresh + time.Hour)
	got, err = cat.Provider(t.Context(), "anthropic")
	if err != nil || got.Source != modelcatalog.SourceModelsDev || len(got.Models) != 2 {
		t.Fatalf("a stale copy was not served: (%+v, %v)", got, err)
	}
	if !strings.Contains(got.Reason, "could not be refreshed") || !strings.Contains(got.Reason, "25h0m0s") {
		t.Errorf("reason = %q, want the failure and the copy's age", got.Reason)
	}
}

// A CATALOGUE PAST [modelcatalog.MaxBytes] IS REFUSED RATHER THAN READ.
//
// The body is a valid catalogue padded one byte past the cap with
// whitespace, so only the cap tells it apart from one to serve: a node whose
// egress is pointed somewhere that streams without end holds 16 MiB, not
// the stream.
func TestACatalogueOverTheCapIsRefused(t *testing.T) {
	t.Parallel()
	body := catalogue + strings.Repeat(" ", modelcatalog.MaxBytes+1-len(catalogue))
	cat, _ := catalogOver(t, &served{body: body, etag: `"big"`})
	got, err := cat.Provider(t.Context(), "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != modelcatalog.SourceNone || !strings.Contains(got.Reason, "over 16 MiB") {
		t.Errorf("a catalogue over the cap = %+v, want it refused naming the cap", got)
	}
}
