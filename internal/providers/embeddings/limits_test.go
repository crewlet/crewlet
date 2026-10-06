package embeddings_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/providers/embeddings"
)

// echoAPI is an OpenAI-compatible endpoint that answers every input it is sent
// with a vector MARKING that input — its first component is the input's length
// in bytes — so a test can say which input filled which slot, across however
// many requests a batch became. Requests may be told, in order, to fail with a
// status or to answer with a body of the test's own.
type echoAPI struct {
	url   string
	width int

	mu       sync.Mutex
	inputs   [][]string
	timeouts []string
	script   []echoAnswer
	delay    time.Duration
}

// echoAnswer is what one request is answered with instead of the echo.
type echoAnswer struct {
	status int
	body   string
}

func newEchoAPI(t *testing.T, width int) *echoAPI {
	t.Helper()
	e := &echoAPI{width: width}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input json.RawMessage `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		var input []string
		if err := json.Unmarshal(req.Input, &input); err != nil {
			var one string
			json.Unmarshal(req.Input, &one)
			input = []string{one}
		}
		e.mu.Lock()
		e.inputs = append(e.inputs, input)
		e.timeouts = append(e.timeouts, r.Header.Get("X-Stainless-Timeout"))
		var answer *echoAnswer
		if len(e.script) > 0 {
			answer = &e.script[0]
			e.script = e.script[1:]
		}
		delay := e.delay
		e.mu.Unlock()
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if answer != nil {
			w.WriteHeader(answer.status)
			w.Write([]byte(answer.body))
			return
		}
		items := make([]string, len(input))
		for i, text := range input {
			vector := make([]string, e.width)
			vector[0] = fmt.Sprint(len(text))
			for j := 1; j < e.width; j++ {
				vector[j] = "0.5"
			}
			items[i] = fmt.Sprintf(`{"object":"embedding","index":%d,"embedding":[%s]}`,
				i, strings.Join(vector, ","))
		}
		w.Write([]byte(`{"object":"list","data":[` + strings.Join(items, ",") + `],"model":"m"}`))
	}))
	t.Cleanup(srv.Close)
	e.url = srv.URL
	return e
}

// then queues what the next requests are answered with.
func (e *echoAPI) then(answers ...echoAnswer) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.script = append(e.script, answers...)
}

// sent is every request's inputs, in order.
func (e *echoAPI) sent() [][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.inputs)
}

// ceilings is the request timeout, in seconds, the SDK told the server each
// request was held to — which is how a test sees which ceiling a request had
// without waiting it out.
func (e *echoAPI) ceilings() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.timeouts)
}

func (e *echoAPI) provider(t *testing.T, limits embeddings.Limits) *embeddings.Provider {
	t.Helper()
	p, err := embeddings.New(embeddings.Config{
		Model: "bounded-model", Dimensions: e.width, APIKey: "sk-test",
		BaseURL: e.url, Limits: limits,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// lengths is the mark of every vector in a batch: the length of the input it
// answers, or -1 for an empty slot.
func lengths(got [][]float32) []int {
	out := make([]int, len(got))
	for i, v := range got {
		if len(v) == 0 {
			out[i] = -1
			continue
		}
		out[i] = int(v[0])
	}
	return out
}

// small is limits a test can reach with short strings.
var small = embeddings.Limits{InputBytes: 16, BatchInputs: 3, BatchBytes: 40}

// AN INPUT PAST THE BOUND IS REFUSED HERE, before any request, naming the
// bound and the model — so the provider is never sent what it would refuse,
// and the refusal is the same class a provider's own would be.
func TestAnInputPastTheBoundIsRefusedBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	e := newEchoAPI(t, 4)
	p := e.provider(t, small)
	_, err := p.Embed(t.Context(), strings.Repeat("x", 17))
	if !errors.Is(err, embeddings.ErrTooLong) || !errors.Is(err, embeddings.ErrRefused) {
		t.Fatalf("Embed past the bound = %v, want ErrTooLong and ErrRefused", err)
	}
	var long *embeddings.TooLongError
	if !errors.As(err, &long) || long.Bytes != 17 || long.Limit != 16 || long.Model != "bounded-model" {
		t.Fatalf("the refusal carries %+v, want 17 bytes against 16 for bounded-model", long)
	}
	for _, want := range []string{"bounded-model", "16", "17"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not say %q", err, want)
		}
	}
	if len(e.sent()) != 0 {
		t.Fatal("an input past the bound reached the network")
	}
	// AT the bound is inside it.
	if _, err := p.Embed(t.Context(), strings.Repeat("x", 16)); err != nil {
		t.Fatalf("an input at the bound was refused: %v", err)
	}
}

// THE BOUND IS MEASURED ON WHAT IS SENT — the prepared text — so whitespace
// the provider never receives does not spend it.
func TestTheBoundIsMeasuredOnThePreparedText(t *testing.T) {
	t.Parallel()
	e := newEchoAPI(t, 4)
	text := "  under \n\n\t sixteen   "
	if got := embeddings.Prepare(text); len(got) > 16 || len(text) <= 16 {
		t.Fatalf("the fixture is wrong: %d raw bytes prepare to %q", len(text), got)
	}
	if _, err := e.provider(t, small).Embed(t.Context(), text); err != nil {
		t.Fatalf("whitespace was counted against the bound: %v", err)
	}
	if got := e.sent()[0][0]; got != embeddings.Prepare(text) {
		t.Fatalf("sent %q, want the prepared %q", got, embeddings.Prepare(text))
	}
}

// ONE INPUT PAST THE BOUND COSTS THE BATCH NO REQUEST: every input is measured
// before the first is sent, so nothing already paid for is thrown away, and
// the refusal says WHICH input — so a caller can set that one aside rather
// than search the batch for it.
func TestABatchInputPastTheBoundSendsNothingAndNamesTheInput(t *testing.T) {
	t.Parallel()
	e := newEchoAPI(t, 4)
	_, err := e.provider(t, small).EmbedBatch(t.Context(),
		[]string{"short", "also short", "", "this one is far too long", "fine"})
	var long *embeddings.TooLongError
	if !errors.As(err, &long) {
		t.Fatalf("EmbedBatch = %v, want a TooLongError", err)
	}
	if long.Index != 3 || long.Model != "bounded-model" {
		t.Errorf("the refusal names input %d of %q, want input 3 of bounded-model",
			long.Index, long.Model)
	}
	if len(e.sent()) != 0 {
		t.Fatalf("%d requests went out for a batch that could not be sent whole", len(e.sent()))
	}
}

// A BATCH IS SENT IN AS MANY REQUESTS AS THE MODEL'S LIMITS NEED — by count
// and by the request's total — and answers as ONE ordered result, empty
// inputs keeping their slots and costing no request.
func TestABatchIsSentInAsManyRequestsAsTheModelsLimitsNeed(t *testing.T) {
	t.Parallel()
	e := newEchoAPI(t, 4)
	limits := embeddings.Limits{InputBytes: 16, BatchInputs: 3, BatchBytes: 40, InputOverhead: 2}
	texts := []string{
		"aaaa", "bbbbbbbb", "", "cc", // a full request by COUNT: 3 inputs
		"dddddddddddddddd", "eeeeeeeeeeeeeeee", // 18 + 18 = 36 ≤ 40
		"ffff",       // 36 + 6 = 42 > 40: a request by TOTAL
		"   ", "ggg", // an empty slot, and the tail
	}
	got, err := e.provider(t, limits).EmbedBatch(t.Context(), texts)
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	want := []int{4, 8, -1, 2, 16, 16, 4, -1, 3}
	if !slices.Equal(lengths(got), want) {
		t.Fatalf("the batch came back as %v, want %v", lengths(got), want)
	}
	sent := e.sent()
	wantSent := [][]string{
		{"aaaa", "bbbbbbbb", "cc"},
		{"dddddddddddddddd", "eeeeeeeeeeeeeeee"},
		{"ffff", "ggg"},
	}
	if len(sent) != len(wantSent) {
		t.Fatalf("%d requests, want %d: %q", len(sent), len(wantSent), sent)
	}
	for i := range wantSent {
		if !slices.Equal(sent[i], wantSent[i]) {
			t.Errorf("request %d carried %q, want %q", i, sent[i], wantSent[i])
		}
		if len(sent[i]) > limits.BatchInputs {
			t.Errorf("request %d carried %d inputs, past %d", i, len(sent[i]), limits.BatchInputs)
		}
		total := 0
		for _, input := range sent[i] {
			total += len(input) + limits.InputOverhead
		}
		if total > limits.BatchBytes {
			t.Errorf("request %d cost %d, past %d", i, total, limits.BatchBytes)
		}
	}

	// AND THE PLAN IS THE ONE RULE: what Requests says is what was sent.
	plan, err := limits.Requests(texts)
	if err != nil {
		t.Fatalf("Requests: %v", err)
	}
	for i, group := range plan {
		var planned []string
		for _, at := range group {
			planned = append(planned, embeddings.Prepare(texts[at]))
		}
		if i >= len(sent) || !slices.Equal(planned, sent[i]) {
			t.Errorf("Requests planned %q for request %d; the provider sent %q", planned, i, sent)
		}
	}
}

// A REQUEST THAT FAILS FAILS THE CALL: no vector the call already paid for
// comes back beside the error, and no request is sent after it.
func TestAFailedRequestFailsTheWholeBatch(t *testing.T) {
	t.Parallel()
	e := newEchoAPI(t, 4)
	limits := embeddings.Limits{InputBytes: 16, BatchInputs: 1, BatchBytes: 16}
	e.then(echoAnswer{status: 200, body: `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[1,0.5,0.5,0.5]}],"model":"m"}`},
		echoAnswer{status: 503, body: `{"error":{"message":"overloaded"}}`})
	got, err := e.provider(t, limits).EmbedBatch(t.Context(), []string{"a", "b", "c"})
	if err == nil {
		t.Fatalf("a batch whose second request failed came back as %v", lengths(got))
	}
	if got != nil {
		t.Fatalf("a partial batch came back beside the error: %v", lengths(got))
	}
	if !errors.Is(err, embeddings.ErrTransient) {
		t.Errorf("a 503 classified as %v, want ErrTransient", err)
	}
	if n := len(e.sent()); n != 2 {
		t.Fatalf("%d requests went out, want 2 — nothing after the one that failed", n)
	}
}

// EVERY REQUEST IS HELD TO THE BIJECTION, not only the first: a repeated index
// in a later request would file one document's vector onto another exactly as
// it would in a batch of one request.
func TestEveryRequestOfABatchIsHeldToTheBijection(t *testing.T) {
	t.Parallel()
	e := newEchoAPI(t, 4)
	limits := embeddings.Limits{InputBytes: 16, BatchInputs: 2, BatchBytes: 64}
	e.then(echoAnswer{status: 200, body: `{"object":"list","data":[` +
		`{"object":"embedding","index":0,"embedding":[1,0.5,0.5,0.5]},` +
		`{"object":"embedding","index":1,"embedding":[1,0.5,0.5,0.5]}],"model":"m"}`},
		echoAnswer{status: 200, body: `{"object":"list","data":[` +
			`{"object":"embedding","index":0,"embedding":[1,0.5,0.5,0.5]},` +
			`{"object":"embedding","index":0,"embedding":[2,0.5,0.5,0.5]}],"model":"m"}`})
	got, err := e.provider(t, limits).EmbedBatch(t.Context(), []string{"a", "b", "c", "d"})
	if err == nil || got != nil {
		t.Fatalf("a second request answering input 0 twice came back as %v, %v", lengths(got), err)
	}
	if !strings.Contains(err.Error(), "input 0 twice") {
		t.Errorf("the refusal does not say what was wrong: %v", err)
	}
}

// A FAILURE IS CLASSIFIED BY WHAT IT MEANS FOR THE INPUT: refused again
// unchanged, worth asking again later, or nothing will work until the
// operator acts. Exactly one class each, because a caller that isolates
// refused inputs reads a misfiled 401 as a poison document and a misfiled 400
// as a blip to resend for ever.
func TestAFailureIsClassifiedByWhatItMeansForTheInput(t *testing.T) {
	t.Parallel()
	classes := []error{embeddings.ErrRefused, embeddings.ErrTransient, embeddings.ErrConfiguration}
	for _, tc := range []struct {
		status int
		want   error
	}{
		{400, embeddings.ErrRefused},
		{413, embeddings.ErrRefused},
		{422, embeddings.ErrRefused},
		{408, embeddings.ErrTransient},
		{409, embeddings.ErrTransient},
		{425, embeddings.ErrTransient},
		{429, embeddings.ErrTransient},
		{500, embeddings.ErrTransient},
		{502, embeddings.ErrTransient},
		{503, embeddings.ErrTransient},
		{401, embeddings.ErrConfiguration},
		{402, embeddings.ErrConfiguration},
		{403, embeddings.ErrConfiguration},
		{404, embeddings.ErrConfiguration},
		{405, embeddings.ErrConfiguration},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			t.Parallel()
			for _, batch := range []bool{false, true} {
				e := newEchoAPI(t, 4)
				e.then(echoAnswer{status: tc.status, body: `{"error":{"message":"no"}}`})
				var err error
				if batch {
					_, err = e.provider(t, small).EmbedBatch(t.Context(), []string{"a", "b"})
				} else {
					_, err = e.provider(t, small).Embed(t.Context(), "a")
				}
				for _, class := range classes {
					if got := errors.Is(err, class); got != errors.Is(class, tc.want) {
						t.Errorf("batch=%v: HTTP %d errors.Is(%v) = %v, want only %v",
							batch, tc.status, class, got, tc.want)
					}
				}
				var classified *embeddings.Error
				if !errors.As(err, &classified) || classified.Status != tc.status {
					t.Errorf("batch=%v: HTTP %d did not carry its status: %v", batch, tc.status, err)
				}
			}
		})
	}
}

// A DEADLINE AND A DEAD SERVER ARE TRANSIENT; A CANCELLATION IS THE CALLER'S.
// Neither of the first two says anything about the input, and asking again
// later may succeed. A cancelled context is the caller stopping, which is not
// a verdict about the provider at all, so it carries no class.
func TestNoAnswerIsTransientAndACancellationIsTheCallers(t *testing.T) {
	t.Parallel()
	e := newEchoAPI(t, 4)
	e.delay = time.Minute
	p := e.provider(t, small)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := p.Embed(ctx, "slow")
	if !errors.Is(err, embeddings.ErrTransient) {
		t.Errorf("a deadline classified as %v, want ErrTransient", err)
	}

	// Cancelled BEFORE the call and DURING it: the second reaches the
	// transport, which wraps the cancellation in an error that is also a
	// network error — the case a classifier reading only the error's type
	// would call transient.
	for _, when := range []time.Duration{0, 30 * time.Millisecond} {
		ctx, cancel := context.WithCancel(t.Context())
		if when == 0 {
			cancel()
		} else {
			time.AfterFunc(when, cancel)
		}
		_, err = p.EmbedBatch(ctx, []string{"stopped"})
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled after %v: came back as %v, want context.Canceled through the wrap", when, err)
		}
		for _, class := range []error{embeddings.ErrRefused, embeddings.ErrTransient, embeddings.ErrConfiguration} {
			if errors.Is(err, class) {
				t.Errorf("cancelled after %v: classified as %v", when, class)
			}
		}
	}

	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	gone, err := embeddings.New(embeddings.Config{
		Model: "m", Dimensions: 4, BaseURL: dead.URL, Limits: small,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := gone.Embed(t.Context(), "anyone there"); !errors.Is(err, embeddings.ErrTransient) {
		t.Errorf("an unreachable server classified as %v, want ErrTransient", err)
	}
}

// A BATCH REQUEST HAS A CEILING OF ITS OWN. A single Embed is a query or a
// turn's ask and keeps the fifteen seconds a turn start can afford; a batch
// request carries up to the model's request total and is held to the batch
// ceiling — which the SDK tells the server, so the test can read it there
// rather than wait it out.
func TestABatchRequestHasACeilingOfItsOwn(t *testing.T) {
	t.Parallel()
	e := newEchoAPI(t, 4)
	p := e.provider(t, small)
	if _, err := p.Embed(t.Context(), "one"); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if _, err := p.EmbedBatch(t.Context(), []string{"one", "two"}); err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	want := []string{
		fmt.Sprint(int(embeddings.EmbedTimeout.Seconds())),
		fmt.Sprint(int(embeddings.BatchTimeout.Seconds())),
	}
	if got := e.ceilings(); !slices.Equal(got, want) {
		t.Fatalf("requests were held to %v seconds, want %v", got, want)
	}
	if embeddings.BatchTimeout <= embeddings.EmbedTimeout {
		t.Errorf("a batch request's ceiling (%v) is no longer than a single call's (%v)",
			embeddings.BatchTimeout, embeddings.EmbedTimeout)
	}
}

// PREPARE IS THE RULE THE PROVIDER HAS ALWAYS SENT, to the byte — so a vector
// already stored for an unchanged text is the vector computed again — and it
// is idempotent, so preparing twice is harmless.
func TestPrepareIsTheRuleThatWasAlwaysSent(t *testing.T) {
	t.Parallel()
	for text, want := range map[string]string{
		"":                             "",
		"   ":                          "",
		"one":                          "one",
		"  lead and trail  ":           "lead and trail",
		"tabs\tand\nnewlines\r\nmixed": "tabs and newlines mixed",
		"a no-break em":                "a no-break em",
		"Title\n\nBody text":           "Title Body text",
	} {
		got := embeddings.Prepare(text)
		if got != want {
			t.Errorf("Prepare(%q) = %q, want %q", text, got, want)
		}
		if again := embeddings.Prepare(got); again != got {
			t.Errorf("Prepare is not idempotent on %q: %q", got, again)
		}
		if old := strings.Join(strings.Fields(text), " "); got != old {
			t.Errorf("Prepare(%q) = %q, but the rule the provider always sent gives %q", text, got, old)
		}
	}
}

// AN OPENING IS EXACTLY WHAT IS SENT: within the bound, a prefix of the
// prepared text, valid UTF-8, and itself prepared — so a digest taken over it
// is a digest of the bytes the vector was computed from, never of one more.
func TestAnOpeningIsExactlyTheBytesSent(t *testing.T) {
	t.Parallel()
	text := "The   staging deploy\n\nkeeps failing — 失败 again and again"
	prepared := embeddings.Prepare(text)
	for n := 0; n <= len(prepared)+2; n++ {
		got := embeddings.Opening(text, n)
		switch {
		case len(got) > n:
			t.Errorf("Opening(%d) = %q is %d bytes", n, got, len(got))
		case !strings.HasPrefix(prepared, got):
			t.Errorf("Opening(%d) = %q is not the start of %q", n, got, prepared)
		case !utf8.ValidString(got):
			t.Errorf("Opening(%d) = %q is not valid UTF-8", n, got)
		case embeddings.Prepare(got) != got:
			t.Errorf("Opening(%d) = %q is not itself prepared — the embedder would send %q",
				n, got, embeddings.Prepare(got))
		}
	}
	if got := embeddings.Opening(text, len(prepared)); got != prepared {
		t.Errorf("a text within the bound opened as %q, want the whole of %q", got, prepared)
	}
	// The cut that lands just after a word drops the space the embedder
	// would have dropped.
	if got := embeddings.Opening("alpha beta gamma", 6); got != "alpha" {
		t.Errorf("Opening at a word boundary = %q, want %q", got, "alpha")
	}
}
