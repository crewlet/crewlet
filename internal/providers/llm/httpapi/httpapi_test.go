package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/llm"

	"github.com/crewlet/crewlet/internal/httpx"
)

func TestMain(m *testing.M) {
	logging.Configure(slog.LevelError, logging.FormatText, io.Discard)
	// The package logger bound its handler at package-var init, which
	// runs before this. Rebind it or every case prints its own log.
	log = logging.Get("providers.llm")
	os.Exit(m.Run())
}

func header(pairs ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(pairs); i += 2 {
		h.Set(pairs[i], pairs[i+1])
	}
	return h
}

// --- RetryHint ---------------------------------------------------------

func TestRetryHint(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		header http.Header
		want   time.Duration
		ok     bool
	}{
		{"nil headers", nil, 0, false},
		{"no headers", http.Header{}, 0, false},

		// RFC 9110 form one: delta-seconds.
		{"retry-after seconds", header("Retry-After", "20"), 20 * time.Second, true},
		{"retry-after fractional", header("Retry-After", "1.5"), 1500 * time.Millisecond, true},
		// RFC 9110 form two: an HTTP-date. A client that reads only the
		// integer form falls back to a guess exactly when the server has
		// told it the answer.
		{"retry-after http-date", header("Retry-After", "Sat, 22 Aug 2026 12:00:30 GMT"), 30 * time.Second, true},
		{"retry-after rfc850 date", header("Retry-After", "Saturday, 22-Aug-26 12:01:00 GMT"), time.Minute, true},

		// A date already past is not a cooldown; the scan moves on.
		{"retry-after in the past", header("Retry-After", "Sat, 22 Aug 2026 11:59:00 GMT"), 0, false},
		{"retry-after zero", header("Retry-After", "0"), 0, false},
		{"retry-after garbage", header("Retry-After", "soon"), 0, false},

		// OpenAI's reset family: a Go-style duration, which a naive
		// trailing-"s" strip cannot read at all.
		{"reset requests duration", header("x-ratelimit-reset-requests", "6m0s"), 6 * time.Minute, true},
		{"reset requests fractional", header("x-ratelimit-reset-requests", "1.5s"), 1500 * time.Millisecond, true},
		{"reset requests bare seconds", header("x-ratelimit-reset-requests", "20"), 20 * time.Second, true},
		{"reset tokens", header("x-ratelimit-reset-tokens", "45s"), 45 * time.Second, true},

		// A zero reset means that bucket is not the one that tripped, so
		// the scan keeps going rather than benching for nothing.
		{"zero reset falls through to the next header",
			header("x-ratelimit-reset-requests", "0s", "x-ratelimit-reset-tokens", "12s"),
			12 * time.Second, true},
		{"every reset zero", header("x-ratelimit-reset-requests", "0s", "x-ratelimit-reset-tokens", "0s"), 0, false},

		// Anthropic's reset family is an RFC 3339 instant.
		{"anthropic requests reset", header("anthropic-ratelimit-requests-reset", "2026-08-22T12:00:10Z"), 10 * time.Second, true},
		{"anthropic tokens reset", header("anthropic-ratelimit-tokens-reset", "2026-08-22T12:02:00Z"), 2 * time.Minute, true},
		{"anthropic reset in the past", header("anthropic-ratelimit-requests-reset", "2026-08-22T11:00:00Z"), 0, false},

		// Retry-After wins over the reset family.
		{"retry-after beats reset",
			header("Retry-After", "5", "x-ratelimit-reset-requests", "600s"),
			5 * time.Second, true},
		// ... but an unusable Retry-After does not shadow a usable reset.
		{"unusable retry-after falls through",
			header("Retry-After", "soon", "x-ratelimit-reset-tokens", "7s"),
			7 * time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := RetryHint(tc.header, now)
			if ok != tc.ok {
				t.Fatalf("RetryHint ok = %v, want %v (got %v)", ok, tc.ok, got)
			}
			if ok && got != tc.want {
				t.Fatalf("RetryHint = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- FromStatus --------------------------------------------------------

func TestFromStatusClassifiesThroughTheContract(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status int
		want   llm.ErrorKind
	}{
		{400, llm.KindFatal},
		{401, llm.KindAuth},
		{402, llm.KindRateLimit},
		{403, llm.KindAuth},
		{404, llm.KindFatal},
		{408, llm.KindTimeout},
		{422, llm.KindFatal},
		{425, llm.KindTimeout},
		{429, llm.KindRateLimit},
		{500, llm.KindServer},
		{503, llm.KindServer},
		{529, llm.KindServer},
		{600, llm.KindFatal},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			t.Parallel()
			cause := errors.New("boom")
			e := FromStatus(cause, "openai", "gpt", tc.status, nil)
			if e.Kind != tc.want {
				t.Fatalf("status %d classified %s, want %s", tc.status, e.Kind, tc.want)
			}
			if e.Status != tc.status {
				t.Fatalf("Status = %d, want %d", e.Status, tc.status)
			}
			if e.Provider != "openai" || e.Model != "gpt" {
				t.Fatalf("error names %s/%s", e.Provider, e.Model)
			}
			if !errors.Is(e, cause) {
				t.Fatal("the cause was not wrapped")
			}
		})
	}
}

func TestFromStatusReadsTheHintOnlyForBenchingKinds(t *testing.T) {
	t.Parallel()
	h := header("Retry-After", "30")
	for _, tc := range []struct {
		status int
		want   time.Duration
	}{
		{429, 30 * time.Second},
		{402, 30 * time.Second},
		{401, 30 * time.Second},
		{403, 30 * time.Second},
		// Nothing consumes a RetryAfter on these, and a populated one
		// invites a reader to think something waits.
		{500, 0},
		{408, 0},
		{400, 0},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			t.Parallel()
			e := FromStatus(errors.New("x"), "p", "m", tc.status, h)
			if e.RetryAfter != tc.want {
				t.Fatalf("RetryAfter = %v, want %v", e.RetryAfter, tc.want)
			}
		})
	}
}

// --- FromTransport -----------------------------------------------------

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestFromTransport(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want llm.ErrorKind
	}{
		{"deadline", context.DeadlineExceeded, llm.KindTimeout},
		{"wrapped deadline", fmt.Errorf("post: %w", context.DeadlineExceeded), llm.KindTimeout},
		{"url error around a deadline",
			&url.Error{Op: "Post", URL: "https://x", Err: context.DeadlineExceeded}, llm.KindTimeout},
		{"dial failure",
			&url.Error{Op: "Post", URL: "https://x", Err: &net.OpError{Op: "dial", Err: errors.New("refused")}},
			llm.KindTimeout},
		{"bare net timeout", timeoutError{}, llm.KindTimeout},
		// THE case that makes the explicit cancellation branch load-bearing
		// rather than decorative: an http.Client cancelled mid-flight returns
		// a *url.Error, and *url.Error satisfies net.Error — so without the
		// cancellation check running FIRST, a cancelled call classifies as a
		// timeout and the chain walks every remaining model to be cancelled
		// too. A mutation that disabled the branch went unnoticed until this
		// row existed.
		{"url error around a cancel",
			&url.Error{Op: "Post", URL: "https://x", Err: context.Canceled}, llm.KindFatal},
		// A cancelled call is the caller's own doing. Fatal is what stops
		// the chain trying members that would be cancelled too, and stops
		// the pool benching a key that did nothing wrong.
		{"cancelled", context.Canceled, llm.KindFatal},
		{"wrapped cancel", fmt.Errorf("post: %w", context.Canceled), llm.KindFatal},
		{"unrecognised", errors.New("decode failed"), llm.KindFatal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := FromTransport(tc.err, "p", "m")
			if e.Kind != tc.want {
				t.Fatalf("classified %s, want %s", e.Kind, tc.want)
			}
			if e.Kind.ExhaustsCredential() {
				t.Fatal("a transport failure benched a credential")
			}
			if !errors.Is(e, tc.err) {
				t.Fatal("the cause was not wrapped")
			}
		})
	}
}

// A cancelled context must still answer errors.Is through the classified
// error, or a caller cannot tell its own shutdown from a provider failure.
func TestCancellationSurvivesClassification(t *testing.T) {
	t.Parallel()
	e := FromTransport(fmt.Errorf("call: %w", context.Canceled), "p", "m")
	if !errors.Is(e, context.Canceled) {
		t.Fatal("errors.Is(err, context.Canceled) no longer answers")
	}
}

// --- the transport -----------------------------------------------------

func TestNewHTTPClientRaisesTheIdleConnectionCeiling(t *testing.T) {
	t.Parallel()
	client := NewHTTPClient()
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want an *http.Transport", client.Transport)
	}
	if transport.MaxIdleConnsPerHost != httpx.MaxIdleConnsPerHost {
		t.Fatalf("MaxIdleConnsPerHost = %d, want %d",
			transport.MaxIdleConnsPerHost, httpx.MaxIdleConnsPerHost)
	}
	if transport.MaxIdleConnsPerHost == http.DefaultMaxIdleConnsPerHost {
		t.Fatal("the stdlib default of 2 would churn a TLS handshake per call " +
			"on any HTTP/1.1 endpoint past the second concurrent turn")
	}
	// Cloning rather than hand-building is what keeps the proxy the process
	// was started with. A hand-built transport drops it silently, and the
	// deployment simply stops being able to reach anything.
	if transport.Proxy == nil {
		t.Fatal("the transport carries no proxy function")
	}
	if !transport.ForceAttemptHTTP2 {
		t.Fatal("HTTP/2 was lost, which is the multiplexing the vendor endpoints rely on")
	}
	// A client-level timeout would also cap a future streaming read, and it
	// surfaces as a bare error rather than the context.DeadlineExceeded this
	// package classifies as KindTimeout.
	if client.Timeout != 0 {
		t.Fatalf("client.Timeout = %v, want the per-request deadline to own it", client.Timeout)
	}
}

// --- tool arguments ----------------------------------------------------

func TestDecodeArgs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		raw  string
		want map[string]any
	}{
		{"object", `{"a":1,"b":"x"}`, map[string]any{"a": float64(1), "b": "x"}},
		{"empty object", `{}`, map[string]any{}},
		{"empty input", ``, map[string]any{}},
		{"whitespace", `   `, map[string]any{}},
		{"null", `null`, map[string]any{}},
		{"array", `[1,2]`, map[string]any{}},
		{"bare string", `"hello"`, map[string]any{}},
		{"truncated", `{"a":`, map[string]any{}},
		{"nested", `{"a":{"b":[1,null,true]}}`,
			map[string]any{"a": map[string]any{"b": []any{float64(1), nil, true}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := DecodeArgs([]byte(tc.raw), "some_tool")
			if got == nil {
				t.Fatal("DecodeArgs returned a nil map; the contract's map is never nil")
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("DecodeArgs = %v, want %v", got, tc.want)
			}
		})
	}
}

// The named landmine: encoding/json populates what it managed BEFORE it
// failed, so `1e1000` yields an error and a map holding +Inf — a value that
// marshals back out as nothing at all. Keeping it would put a round-trip
// failure in the conversation that fires a round later, on a request that has
// nothing to do with the tool that caused it.
func TestDecodeArgsAlwaysProducesAReSerialisableMap(t *testing.T) {
	t.Parallel()
	// The property that matters: whatever a model sends, what comes out can
	// be marshalled back onto the wire. A map that cannot poisons the
	// conversation — the assistant turn replaying that tool call fails to
	// encode, and every subsequent round of the turn fails with it.
	//
	// 1e1000 is the case that found this. Plain unmarshalling HALF-decodes
	// it: an error is returned but the map already holds +Inf, which
	// json.Marshal then refuses. Decoding with UseNumber accepts it as an
	// exact json.Number and it round-trips unchanged — so the guard is now
	// "it survives", not "it is discarded".
	var direct map[string]any
	if err := json.Unmarshal([]byte(`{"n":1e1000}`), &direct); err == nil {
		t.Fatal("premise broken: 1e1000 unmarshalled cleanly without UseNumber")
	}
	if !math.IsInf(direct["n"].(float64), 1) {
		t.Fatalf("premise broken: the partial decode left %v, want +Inf", direct["n"])
	}
	if _, err := json.Marshal(direct); err == nil {
		t.Fatal("premise broken: the partial decode marshalled back out")
	}

	got := DecodeArgs([]byte(`{"n":1e1000}`), "some_tool")
	blob, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("DecodeArgs produced a map that cannot be re-serialised: %v", err)
	}
	if string(blob) != `{"n":1e1000}` {
		t.Errorf("round trip = %s, want the value unchanged", blob)
	}
}

func TestLargeIntegerArgumentsStayExact(t *testing.T) {
	t.Parallel()
	// The failure this decoder exists to prevent, and it is silent: a
	// 19-digit id — a Jira issue id, a Slack timestamp, a GitHub node id —
	// decoded as float64 comes back as 1.2345678901234568e+18 and
	// re-encodes as 1234567890123456800. The tool call then reaches the
	// server naming a DIFFERENT entity and succeeds against the wrong row,
	// with nothing anywhere reporting an error.
	got := DecodeArgs([]byte(`{"issue_id":1234567890123456789,"amount":100.50}`), "jira_get")
	blob, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(blob), "1234567890123456789") {
		t.Errorf("round trip = %s, want the id digit-for-digit", blob)
	}
	// The counterfactual, so this cannot pass for a decoder that stringifies
	// everything: a genuine decimal is still a number on the way out.
	if !strings.Contains(string(blob), "100.50") && !strings.Contains(string(blob), "100.5") {
		t.Errorf("round trip = %s, want the decimal preserved", blob)
	}
	if strings.Contains(string(blob), `"1234567890123456789"`) {
		t.Errorf("round trip = %s, want a number and not a string", blob)
	}
}

func TestEncodeArgs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		// json.Marshal spells a nil map "null", and an assistant turn
		// replaying `"arguments": "null"` is a message the model wrote
		// turning into one it did not.
		{"nil", nil, "{}"},
		{"empty", map[string]any{}, "{}"},
		{"object", map[string]any{"a": 1}, `{"a":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := EncodeArgs(tc.args, "tool")
			if err != nil {
				t.Fatalf("EncodeArgs: %v", err)
			}
			if got != tc.want {
				t.Fatalf("EncodeArgs = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEncodeArgsRefusesUnserialisableValues(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"infinity", map[string]any{"n": math.Inf(1)}},
		{"nan", map[string]any{"n": math.NaN()}},
		{"channel", map[string]any{"c": make(chan int)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := EncodeArgs(tc.args, "deliver")
			if err == nil {
				t.Fatalf("EncodeArgs = %q, want an error rather than silently empty args", got)
			}
			if got != "" {
				t.Fatalf("EncodeArgs returned %q alongside its error", got)
			}
		})
	}
}
