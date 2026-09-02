package embeddings_test

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/embeddings"
)

// server is a fake OpenAI-compatible embedding endpoint.
//
// EVERYTHING BUT THE URL IS UNDER THE LOCK. The handler runs on the HTTP
// server's own goroutines while the test body both writes what to answer
// with next and reads what was asked — two goroutines on one field, which
// is a data race whether or not the run that found it was unlucky.
type server struct {
	url string

	mu     sync.Mutex
	width  int
	body   string
	status int
	asked  []map[string]any
}

// answers sets the status and body the endpoint replies with next.
func (s *server) answers(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.body = status, body
}

// widens sets the width of the vector the endpoint returns, for the
// mid-deployment model change a caller has to notice.
func (s *server) widens(width int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.width = width
}

// requests is what the endpoint was asked for.
func (s *server) requests() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.asked)
}

func fakeAPI(t *testing.T, width int) *server {
	t.Helper()
	s := &server{width: width, status: http.StatusOK}
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			var req map[string]any
			json.NewDecoder(r.Body).Decode(&req)
			s.mu.Lock()
			s.asked = append(s.asked, req)
			status, body, width := s.status, s.body, s.width
			s.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if body != "" {
				w.Write([]byte(body))
				return
			}
			w.Write([]byte(vectorBody(width)))
		}))
	t.Cleanup(srv.Close)
	s.url = srv.URL
	return s
}

func vectorBody(width int) string {
	parts := make([]string, width)
	for i := range parts {
		parts[i] = "0.5"
	}
	return `{"object":"list","data":[{"object":"embedding","index":0,` +
		`"embedding":[` + strings.Join(parts, ",") + `]}],"model":"m"}`
}

func provider(t *testing.T, s *server, width int) *embeddings.Provider {
	t.Helper()
	p, err := embeddings.New(embeddings.Config{
		Model: "text-embedding-3-small", Dimensions: width,
		APIKey: "sk-test", BaseURL: s.url,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestAnEmbeddingComesBackAtTheConfiguredWidth(t *testing.T) {
	t.Parallel()
	s := fakeAPI(t, 8)
	got, err := provider(t, s, 8).Embed(t.Context(), "the deploy keeps failing")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got) != 8 {
		t.Fatalf("vector width = %d", len(got))
	}
}

// THE WIDTH IS ASKED FOR, not just checked: the third-generation models
// truncate on request, so a company that sized its store at 768 gets 768
// rather than a refusal.
func TestTheConfiguredWidthIsRequested(t *testing.T) {
	t.Parallel()
	s := fakeAPI(t, 8)
	if _, err := provider(t, s, 8).Embed(t.Context(), "text"); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if got := s.requests()[0]["dimensions"]; got != float64(8) {
		t.Fatalf("dimensions = %v, want the configured width", got)
	}
}

// A WIDTH MISMATCH IS REFUSED, on every call. The store's columns are sized
// from the config, so a wrong-width vector is not a degraded search — it is
// a row that can be written and never read back, silently and permanently.
func TestAWrongWidthVectorIsRefusedRatherThanStored(t *testing.T) {
	t.Parallel()
	s := fakeAPI(t, 4) // the server ignores the request and returns 4
	_, err := provider(t, s, 8).Embed(t.Context(), "text")
	if err == nil {
		t.Fatal("a 4-wide vector was accepted for an 8-wide store")
	}
	for _, want := range []string{"4-wide", "says 8", "never read back"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error does not say %q: %v", want, err)
		}
	}
}

// ON EVERY CALL, not just the first: a provider behind an aggregator can
// change model mid-deployment, and one length comparison is nothing beside
// the round trip that produced it.
func TestTheWidthIsCheckedOnEveryCall(t *testing.T) {
	t.Parallel()
	s := fakeAPI(t, 8)
	p := provider(t, s, 8)
	if _, err := p.Embed(t.Context(), "first"); err != nil {
		t.Fatalf("the first call failed: %v", err)
	}
	s.widens(4) // the aggregator moved to another model
	if _, err := p.Embed(t.Context(), "second"); err == nil {
		t.Fatal("a mid-deployment width change was accepted")
	}
}

// AN EMPTY TASK IS NOT A PROVIDER PROBLEM, and must not be logged as one —
// but it is still no vector, so it has its own error.
func TestEmptyTextIsItsOwnAnswer(t *testing.T) {
	t.Parallel()
	s := fakeAPI(t, 8)
	p := provider(t, s, 8)
	for _, text := range []string{"", "   ", "\n\t\n"} {
		if _, err := p.Embed(t.Context(), text); !errors.Is(err, embeddings.ErrEmpty) {
			t.Fatalf("Embed(%q) = %v, want ErrEmpty", text, err)
		}
	}
	if len(s.requests()) != 0 {
		t.Fatal("an empty task reached the network")
	}
}

// WHITESPACE IS COLLAPSED, and that is not cosmetic: the callers pass a
// rendered task and a rendered summary, both carrying the newlines of
// whatever produced them, and the same sentence formatted two ways would
// otherwise be two different vectors.
func TestFormattingDoesNotChangeTheVector(t *testing.T) {
	t.Parallel()
	s := fakeAPI(t, 8)
	p := provider(t, s, 8)
	for _, text := range []string{
		"fix the login redirect",
		"fix   the\n\tlogin\n  redirect  ",
	} {
		if _, err := p.Embed(t.Context(), text); err != nil {
			t.Fatalf("Embed: %v", err)
		}
	}
	if asked := s.requests(); asked[0]["input"] != asked[1]["input"] {
		t.Fatalf("two formattings sent %q and %q",
			asked[0]["input"], asked[1]["input"])
	}
}

func TestARefusedCallIsAnErrorNotAnEmptyVector(t *testing.T) {
	t.Parallel()
	s := fakeAPI(t, 8)
	s.answers(http.StatusUnauthorized, `{"error":{"message":"bad key"}}`)
	got, err := provider(t, s, 8).Embed(t.Context(), "text")
	if err == nil {
		t.Fatalf("a 401 came back as %v with no error", got)
	}
}

// A model and a width are REQUIRED: a default for either would be a width
// the store was not sized for.
func TestAProviderNeedsAModelAndAWidth(t *testing.T) {
	t.Parallel()
	if _, err := embeddings.New(embeddings.Config{Dimensions: 8}); err == nil {
		t.Fatal("a provider with no model was accepted")
	}
	if _, err := embeddings.New(embeddings.Config{Model: "m"}); err == nil {
		t.Fatal("a provider with no width was accepted")
	}
}

// ── the fake ──

// A REAL SIMILARITY FUNCTION, not a stub: a constant would make every memory
// equally similar to every task, which is the one answer that makes a recall
// test meaningless.
func TestTheFakeRanksSharedWordsAboveUnrelatedText(t *testing.T) {
	t.Parallel()
	f := embeddings.NewFake(64)
	task, err := f.Embed(t.Context(), "fix the staging login redirect loop")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	related, _ := f.Embed(t.Context(), "the staging redirect loop is behind the proxy")
	unrelated, _ := f.Embed(t.Context(), "quarterly budget review with finance")

	near, far := cosine(task, related), cosine(task, unrelated)
	if near <= far {
		t.Fatalf("related scored %.3f and unrelated %.3f", near, far)
	}
	if near < 0.2 {
		t.Fatalf("related text scored only %.3f — too low to rank on", near)
	}
}

// DETERMINISTIC, which is what lets a test assert a ranking at all.
func TestTheFakeAnswersTheSameWayEveryTime(t *testing.T) {
	t.Parallel()
	f := embeddings.NewFake(64)
	first, _ := f.Embed(t.Context(), "fix the login redirect")
	for range 5 {
		again, _ := f.Embed(t.Context(), "fix the login redirect")
		for i := range first {
			if first[i] != again[i] {
				t.Fatalf("the fake moved at index %d", i)
			}
		}
	}
}

// NORMALISED, because cosine reads these: an unnormalised bag-of-words makes
// a long text similar to everything by having a bigger magnitude.
func TestTheFakeReturnsUnitVectors(t *testing.T) {
	t.Parallel()
	f := embeddings.NewFake(64)
	for _, text := range []string{"short", strings.Repeat("many words here ", 50)} {
		v, err := f.Embed(t.Context(), text)
		if err != nil {
			t.Fatalf("Embed: %v", err)
		}
		if got := math.Abs(float64(magnitude(v)) - 1); got > 1e-5 {
			t.Fatalf("%q has magnitude off by %v", text[:5], got)
		}
	}
	if _, err := f.Embed(t.Context(), "  "); !errors.Is(err, embeddings.ErrEmpty) {
		t.Fatalf("the fake accepted empty text: %v", err)
	}
	if got := embeddings.NewFake(0).Width(); got <= 0 {
		t.Fatalf("a zero width produced %d", got)
	}
}

func cosine(a, b []float32) float64 {
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot / (float64(magnitude(a)) * float64(magnitude(b)))
}

func magnitude(v []float32) float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	return float32(math.Sqrt(sum))
}

// dialed is a transport that records where a request went and answers with a
// canned vector, so what the provider DIALS can be asserted without a
// network — which is the only way to see the default base URL at all.
type dialed struct {
	width int
	url   string
	auth  string
}

func (d *dialed) RoundTrip(r *http.Request) (*http.Response, error) {
	d.url = r.URL.String()
	d.auth = r.Header.Get("Authorization")
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(vectorBody(d.width))),
		Request:    r,
	}, nil
}

// THE DEFAULT BASE URL IS THE REAL API. A company that names no base URL has
// said "OpenAI", and the failure mode of getting this wrong is not an error
// — it is a provider that dials nothing and degrades every recall to
// recency, silently, for the life of the deployment.
func TestNoBaseURLDialsOpenAI(t *testing.T) {
	t.Parallel()
	d := &dialed{width: 4}
	p, err := embeddings.New(embeddings.Config{
		Model: "text-embedding-3-small", Dimensions: 4, APIKey: "sk-test",
		HTTPClient: &http.Client{Transport: d},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.Embed(t.Context(), "text"); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if !strings.HasPrefix(d.url, embeddings.DefaultBaseURL+"/") {
		t.Fatalf("dialled %q, want it under %q", d.url, embeddings.DefaultBaseURL)
	}
	if !strings.Contains(d.url, "api.openai.com") {
		t.Fatalf("dialled %q, which is not OpenAI", d.url)
	}
}

// A CONFIGURED BASE URL WINS, which is the whole of what makes an
// OpenAI-compatible server — a local embedder, a gateway — work with no code
// here.
func TestAConfiguredBaseURLIsDialledInstead(t *testing.T) {
	t.Parallel()
	d := &dialed{width: 4}
	p, err := embeddings.New(embeddings.Config{
		Model: "m", Dimensions: 4, APIKey: "sk-test",
		BaseURL: "https://embeddings.example.com/v1", HTTPClient: &http.Client{Transport: d},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.Embed(t.Context(), "text"); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if !strings.HasPrefix(d.url, "https://embeddings.example.com/v1/") {
		t.Fatalf("dialled %q, want the configured base URL", d.url)
	}
}

// THE CONVENTIONAL KEY IS CONSULTED when the config names none — the whole
// point of sharing OPENAI_API_KEY with the chat backend, and a fallback that
// fails silently: with no key the provider still builds and every call comes
// back 401, which the caller swallows as "no similarity search".
func TestAnUnnamedKeyFallsBackToTheConventionalVariable(t *testing.T) {
	t.Parallel()
	d := &dialed{width: 4}
	p, err := embeddings.New(embeddings.Config{
		Model: "m", Dimensions: 4, HTTPClient: &http.Client{Transport: d},
		LookupEnv: func(name string) string {
			if name == embeddings.KeyEnv {
				return "  sk-from-the-environment  "
			}
			return ""
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.Embed(t.Context(), "text"); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if d.auth != "Bearer sk-from-the-environment" {
		t.Fatalf("Authorization = %q, want the trimmed conventional key", d.auth)
	}
}

// A NAMED KEY WINS over the environment, so a company running two OpenAI
// accounts — one for chat, one for embeddings — gets the one it named.
func TestANamedKeyBeatsTheEnvironment(t *testing.T) {
	t.Parallel()
	d := &dialed{width: 4}
	p, err := embeddings.New(embeddings.Config{
		Model: "m", Dimensions: 4, APIKey: "sk-named",
		HTTPClient: &http.Client{Transport: d},
		LookupEnv:  func(string) string { return "sk-environment" },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.Embed(t.Context(), "text"); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if d.auth != "Bearer sk-named" {
		t.Fatalf("Authorization = %q, want the configured key", d.auth)
	}
}

// A 200 CARRYING NO VECTOR is an error, not an empty one. The caller reads a
// zero-length vector as "no similarity search" either way, but only the
// error reaches the log line that says why.
func TestASuccessfulResponseWithNoVectorIsRefused(t *testing.T) {
	t.Parallel()
	s := fakeAPI(t, 4)
	s.answers(http.StatusOK, `{"object":"list","data":[],"model":"m"}`)
	got, err := provider(t, s, 4).Embed(t.Context(), "text")
	if err == nil {
		t.Fatal("an empty data array was accepted")
	}
	if got != nil {
		t.Fatalf("vector = %v, want none alongside the error", got)
	}
	if !strings.Contains(err.Error(), "no vector") {
		t.Errorf("error %q does not say what was missing", err)
	}
}

// CASE-FOLDED, because the two texts being compared come from different
// producers: a task rendered from a webhook and a memory a model wrote. If
// "Deploy" and "deploy" hashed to different buckets, the fake would rank on
// capitalisation, which is not a similarity anything wants.
func TestTheFakeIgnoresCase(t *testing.T) {
	t.Parallel()
	f := embeddings.NewFake(64)
	lower, err := f.Embed(t.Context(), "the staging deploy keeps failing")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	upper, err := f.Embed(t.Context(), "The Staging Deploy Keeps Failing")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	for i := range lower {
		if lower[i] != upper[i] {
			t.Fatalf("capitalisation moved the vector at index %d", i)
		}
	}
}

// NOTHING RETRIES HERE. The SDK's defaults fire on the whole 429/5xx set,
// and this caller's answer to a failure is "no similarity search" — cheaper
// than any retry, and it belongs to the caller rather than being spent on
// its behalf inside a Plan-phase prefetch a person is waiting on.
func TestAFailedCallIsNotRetried(t *testing.T) {
	t.Parallel()
	s := fakeAPI(t, 8)
	s.answers(http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`)
	if _, err := provider(t, s, 8).Embed(t.Context(), "text"); err == nil {
		t.Fatal("a 429 came back with no error")
	}
	if asked := s.requests(); len(asked) != 1 {
		t.Fatalf("the endpoint was called %d times, want exactly 1", len(asked))
	}
}
