package sandbox_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// What these tests protect.
//
// The receiver exists so a box running generated code never holds the
// telemetry backend's ingest credential. Every failure of that is silent in
// the same way: the export succeeds, the trace appears, and the credential is
// somewhere it should not be — or the token check is loose and anybody who
// can reach the endpoint writes into the company's telemetry.

// A TOKEN MINTED IN ONE PROCESS VERIFIES IN ANOTHER.
//
// This is the property the whole design turns on. Minting and verifying
// happen in different processes whenever the API runs on its own host, and an
// in-memory store makes them the same process by assumption — so the
// documented split deployment answers 401 to every trace from every coding
// run, visible only as exporter retry noise inside a sandbox nobody watches.
func TestAnOtelTokenVerifiesInAnotherProcess(t *testing.T) {
	t.Parallel()
	key := sandbox.OtelSigningKey([]string{"k1:material-one", "k2:material-two"})

	minter := sandbox.NewOtelTokens(sandbox.OtelTokenOptions{Key: key})
	verifier := sandbox.NewOtelTokens(sandbox.OtelTokenOptions{Key: key})

	token := minter.Mint("trace-abc", time.Hour)
	if got := verifier.Validate(token); got != "trace-abc" {
		t.Fatalf("a token minted elsewhere validated to %q", got)
	}

	// THE KEY IS ORDER-INDEPENDENT, because two processes read the same
	// keyring and nothing promises the same order — a map iteration or a
	// re-ordered document would otherwise split a fleet in two.
	reordered := sandbox.OtelSigningKey([]string{"k2:material-two", "k1:material-one"})
	if sandbox.NewOtelTokens(sandbox.OtelTokenOptions{Key: reordered}).
		Validate(token) != "trace-abc" {
		t.Error("the signing key depends on the keyring's order")
	}

	// AND A DIFFERENT KEYRING DOES NOT VERIFY, or the derivation would be
	// decoration.
	other := sandbox.OtelSigningKey([]string{"k1:someone-elses"})
	if sandbox.NewOtelTokens(sandbox.OtelTokenOptions{Key: other}).Validate(token) != "" {
		t.Error("a token verified under an unrelated keyring")
	}
}

// A FORGED, MALFORMED OR EXPIRED TOKEN IS REFUSED — and the caller cannot
// tell which, because telling it tells an attacker the same.
func TestOtelTokensAreRefusedEveryWayTheyCanBeWrong(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	clock := at
	tokens := sandbox.NewOtelTokens(sandbox.OtelTokenOptions{
		Key: []byte("a-shared-signing-key"),
		Now: func() time.Time { return clock },
	})
	good := tokens.Mint("trace-abc", 10*time.Minute)
	if tokens.Validate(good) != "trace-abc" {
		t.Fatal("a fresh token did not validate")
	}

	for _, tc := range []struct{ name, token string }{
		{"empty", ""},
		{"not a token at all", "hello"},
		{"too few parts", "v1.trace-abc.999"},
		{"an unknown version", "v2" + good[2:]},
		{"a tampered trace", strings.Replace(good, "trace-abc", "trace-xyz", 1)},
		{"a tampered expiry", strings.Replace(good, ".", ".9", 1)},
		{"a truncated signature", good[:len(good)-4]},
	} {
		if got := tokens.Validate(tc.token); got != "" {
			t.Errorf("%s validated to %q", tc.name, got)
		}
	}

	// EXPIRY RIDES IN THE TOKEN, so nothing is reaped and a restart does
	// not invalidate a live run's endpoint.
	clock = at.Add(11 * time.Minute)
	if got := tokens.Validate(good); got != "" {
		t.Errorf("an expired token validated to %q", got)
	}
}

// A TOKEN IS SCOPED TO ONE TRACE, so one run's endpoint is not another's.
func TestOtelTokensAreScopedToTheirRun(t *testing.T) {
	t.Parallel()
	tokens := sandbox.NewOtelTokens(sandbox.OtelTokenOptions{Key: []byte("k")})
	first := tokens.Mint("trace-one", time.Hour)
	second := tokens.Mint("trace-two", time.Hour)
	if first == second {
		t.Fatal("two runs got the same token")
	}
	if tokens.Validate(first) == tokens.Validate(second) {
		t.Fatal("two runs' tokens name the same trace")
	}
}

// otelUpstream is a stand-in for the real telemetry backend.
type otelUpstream struct {
	mu       sync.Mutex
	paths    []string
	headers  http.Header
	bodies   [][]byte
	status   int
	received chan struct{}
}

func newOtelUpstream() *otelUpstream {
	return &otelUpstream{received: make(chan struct{}, 8)}
}

func (u *otelUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := make([]byte, r.ContentLength)
	_, _ = r.Body.Read(body)
	u.mu.Lock()
	u.paths = append(u.paths, r.URL.Path)
	u.headers = r.Header.Clone()
	u.bodies = append(u.bodies, body)
	status := u.status
	u.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	w.WriteHeader(http.StatusOK)
	select {
	case u.received <- struct{}{}:
	default:
	}
}

func (u *otelUpstream) saw() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.paths...)
}

// THE BACKEND'S CREDENTIAL IS ADDED ON THE WAY OUT, and the payload reaches
// it unchanged. That hop is the whole reason the receiver exists.
func TestTheUpstreamCredentialIsAddedOutsideTheSandbox(t *testing.T) {
	t.Parallel()
	upstream := newOtelUpstream()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)

	receiver, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
		BaseURL:          "https://engine.internal",
		Tokens:           sandbox.NewOtelTokens(sandbox.OtelTokenOptions{Key: []byte("k")}),
		UpstreamEndpoint: server.URL,
		UpstreamHeaders:  map[string]string{"Authorization": "Bearer ingest-secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !receiver.Forwards() {
		t.Fatal("a receiver with an upstream reports it forwards nothing")
	}

	receiver.Forward(context.Background(), "traces", []byte("payload"), "application/x-protobuf")
	if got := upstream.saw(); len(got) != 1 || got[0] != "/v1/traces" {
		t.Fatalf("forwarded to %v", got)
	}
	upstream.mu.Lock()
	auth := upstream.headers.Get("Authorization")
	contentType := upstream.headers.Get("Content-Type")
	upstream.mu.Unlock()
	if auth != "Bearer ingest-secret" {
		t.Errorf("the upstream credential was not added: %q", auth)
	}
	if contentType != "application/x-protobuf" {
		t.Errorf("the payload's content type was not carried: %q", contentType)
	}
}

// A RECEIVER WITH NO UPSTREAM ACCEPTS AND DROPS, which is a working
// configuration rather than a broken one: the engine's own per-turn span
// still carries the trace, and a deployment with no backend should not have
// every export fail.
func TestAReceiverWithNoUpstreamAcceptsAndDrops(t *testing.T) {
	t.Parallel()
	receiver, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
		BaseURL: "https://engine.internal",
		Tokens:  sandbox.NewOtelTokens(sandbox.OtelTokenOptions{Key: []byte("k")}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if receiver.Forwards() {
		t.Fatal("a receiver with no upstream claims to forward")
	}
	// Must not panic or block.
	receiver.Forward(context.Background(), "traces", []byte("payload"), "")
}

// A FAILING BACKEND IS NOT THE BOX'S PROBLEM. An exporter that gets a 5xx
// retries, and retries against a backend that is down turn one outage into
// two: the coding run's own traffic multiplies while nothing it carries is
// recoverable anyway.
func TestAFailingUpstreamNeverRaises(t *testing.T) {
	t.Parallel()
	upstream := newOtelUpstream()
	upstream.status = http.StatusServiceUnavailable
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)

	receiver, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
		BaseURL:          "https://engine.internal",
		Tokens:           sandbox.NewOtelTokens(sandbox.OtelTokenOptions{Key: []byte("k")}),
		UpstreamEndpoint: server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Forward returns nothing at all: there is no error for a caller to
	// mistakenly report to the sandbox.
	receiver.Forward(context.Background(), "traces", []byte("payload"), "")
}

// THE RUN ENVIRONMENT CARRIES AN ENDPOINT AND NO CREDENTIAL.
//
// The headers variable is set EMPTY rather than left unset, because the
// exporter reads it from the ambient environment otherwise — and a box
// inherits whatever the engine host exported. Empty is the difference between
// "no credential" and "the engine's".
func TestTheRunEnvironmentCarriesNoCredential(t *testing.T) {
	t.Parallel()
	receiver, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
		BaseURL: "https://engine.internal/",
		Tokens:  sandbox.NewOtelTokens(sandbox.OtelTokenOptions{Key: []byte("k")}),
		// A real upstream, whose credential must NOT appear below.
		UpstreamEndpoint: "https://collector.example",
		UpstreamHeaders:  map[string]string{"Authorization": "Bearer ingest-secret"},
	})
	if err != nil {
		t.Fatal(err)
	}

	env := receiver.RunEnv("trace-abc", "span-def", "turn-1", "engineer", time.Hour)
	endpoint := env["OTEL_EXPORTER_OTLP_ENDPOINT"]
	if !strings.HasPrefix(endpoint, "https://engine.internal/otlp/") {
		t.Fatalf("the box exports to %q, which is not the engine", endpoint)
	}
	if strings.Contains(endpoint, "collector.example") {
		t.Fatal("the box was pointed at the real backend")
	}
	headers, set := env["OTEL_EXPORTER_OTLP_HEADERS"]
	if !set || headers != "" {
		t.Errorf("OTEL_EXPORTER_OTLP_HEADERS = %q (set: %v); an unset value is "+
			"inherited from the engine host's own environment", headers, set)
	}
	for key, value := range env {
		if strings.Contains(value, "ingest-secret") {
			t.Errorf("%s carries the backend's credential into the box", key)
		}
	}

	// THE TOKEN IN THE PATH IS THIS RUN'S, and validates.
	token := strings.TrimPrefix(endpoint, "https://engine.internal/otlp/")
	if got := receiver.Validate(token); got != "trace-abc" {
		t.Errorf("the endpoint's token names %q", got)
	}
	// TRACEPARENT is what nests the box's spans under the turn's. Without
	// it they form a second, unrelated trace that nobody finds.
	if env["TRACEPARENT"] != "00-trace-abc-span-def-01" {
		t.Errorf("TRACEPARENT = %q", env["TRACEPARENT"])
	}
	if attrs := env["OTEL_RESOURCE_ATTRIBUTES"]; !strings.Contains(attrs, "turn-1") ||
		!strings.Contains(attrs, "engineer") {
		t.Errorf("the run's spans name no turn or seat: %q", attrs)
	}
}

// A RUN WITH NO TRACE GETS NO ENDPOINT.
//
// A token scoped to an empty trace would authenticate every run's export as
// every other run's, which is the one property the scoping exists for.
func TestARunWithNoTraceGetsNoEndpoint(t *testing.T) {
	t.Parallel()
	receiver, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
		BaseURL: "https://engine.internal",
		Tokens:  sandbox.NewOtelTokens(sandbox.OtelTokenOptions{Key: []byte("k")}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if env := receiver.RunEnv("", "span", "turn-1", "engineer", time.Hour); len(env) != 0 {
		t.Fatalf("a trace-less run got %v", env)
	}
}

// A RECEIVER WITH NO REACHABLE ADDRESS IS REFUSED at construction: an
// endpoint minted against an address the box cannot resolve fails silently
// inside the box, which is the one place nobody is looking.
func TestAReceiverNeedsAnAddressTheBoxCanReach(t *testing.T) {
	t.Parallel()
	if _, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
		Tokens: sandbox.NewOtelTokens(sandbox.OtelTokenOptions{Key: []byte("k")}),
	}); err == nil {
		t.Fatal("a receiver with no base URL was built")
	}
	if _, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
		BaseURL: "https://engine.internal",
	}); err == nil {
		t.Fatal("a receiver with no token minter was built")
	}
}

// ONLY THE SIGNALS THIS RECEIVER FORWARDS ARE ACCEPTED.
//
// The signal is concatenated into the upstream URL, so an unchecked one lets
// a caller choose part of the address the engine's OWN credential is sent to.
func TestOnlyKnownSignalsAreAccepted(t *testing.T) {
	t.Parallel()
	for _, good := range []string{"traces", "metrics", "logs"} {
		if !sandbox.ValidSignal(good) {
			t.Errorf("%q is not accepted", good)
		}
	}
	for _, bad := range []string{"", "traces/../../admin", "TRACES", "profiles", ".."} {
		if sandbox.ValidSignal(bad) {
			t.Errorf("%q was accepted as a signal", bad)
		}
	}
}

// THE UPSTREAM HEADER FORM IS OTEL'S OWN, so an operator pastes what their
// collector's documentation gave them.
func TestUpstreamHeadersParseTheStandardForm(t *testing.T) {
	t.Parallel()
	got := sandbox.ParseOtelHeaders("api-key=abc123, x-tenant = acme ,,malformed")
	if got["api-key"] != "abc123" {
		t.Errorf("api-key = %q", got["api-key"])
	}
	if got["x-tenant"] != "acme" {
		t.Errorf("x-tenant = %q", got["x-tenant"])
	}
	if _, present := got["malformed"]; present {
		t.Error("a pair with no = was read as a header")
	}
	if len(sandbox.ParseOtelHeaders("")) != 0 {
		t.Error("an empty value produced headers")
	}
}

// THE RECEIVER IS BUILT FROM THE ENVIRONMENT, ONCE, and an unset address
// means no receiver rather than a broken one.
func TestTheReceiverIsBuiltFromTheEnvironment(t *testing.T) {
	t.Parallel()
	env := map[string]string{}
	lookup := func(name string) string { return env[name] }

	got, err := sandbox.BuildOtelReceiver(lookup, nil)
	if err != nil || got != nil {
		t.Fatalf("an unset receiver URL built %v (%v)", got, err)
	}

	env[sandbox.OtelReceiverURLVar] = "https://engine.internal"
	env[sandbox.OtelUpstreamEndpointVar] = "https://collector.example"
	env[sandbox.OtelUpstreamHeadersVar] = "api-key=abc"
	got, err = sandbox.BuildOtelReceiver(lookup, []string{"k1:material"})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("a configured receiver URL built nothing")
	}
	if !got.Forwards() {
		t.Error("a configured upstream reports no forwarding")
	}
}

// THE OPERATOR'S OWN ENVIRONMENT WINS.
//
// role.sandbox.env is where an operator declares what a box gets, and a
// deployment that points its coding agents at its own collector directly has
// said so deliberately. Overriding that silently would be the engine choosing
// where somebody else's telemetry goes — and the failure is invisible: the
// spans arrive somewhere, just not where the config said.
func TestTheOperatorsTelemetryEnvironmentWins(t *testing.T) {
	t.Parallel()
	receiver, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
		BaseURL: "https://engine.internal",
		Tokens:  sandbox.NewOtelTokens(sandbox.OtelTokenOptions{Key: []byte("k")}),
	})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers: map[sandbox.Placement]sandbox.Provider{
			sandbox.Direct: sandbox.NewFakeProvider(),
		},
		Runners:   map[string]sandbox.Runner{"claude-code": sandbox.NewFakeRunner("claude-code")},
		Telemetry: receiver,
	})
	if err != nil {
		t.Fatal(err)
	}

	env := sandbox.RunEnvFor(manager, sandbox.LaunchRequest{
		Turn: sandbox.TurnRef{
			TraceID: "trace-abc", SpanID: "span-def",
			TurnID: "turn-1", AgentHandle: "engineer",
		},
		Spec: sandbox.Spec{
			CodingAgent: "claude-code",
			TimeoutSec:  900,
			Env: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT": "https://our-own-collector.example",
				"ANTHROPIC_API_KEY":           "sk-test",
			},
		},
	})
	if got := env["OTEL_EXPORTER_OTLP_ENDPOINT"]; got != "https://our-own-collector.example" {
		t.Errorf("the engine overrode the operator's collector: %q", got)
	}
	// AND WHAT THE OPERATOR DID NOT SET IS STILL ADDED, or the run would
	// lose the trace context that nests its spans under the turn.
	if env["TRACEPARENT"] != "00-trace-abc-span-def-01" {
		t.Errorf("TRACEPARENT = %q", env["TRACEPARENT"])
	}
	if env["ANTHROPIC_API_KEY"] != "sk-test" {
		t.Errorf("the run environment lost a credential: %q", env["ANTHROPIC_API_KEY"])
	}
}
