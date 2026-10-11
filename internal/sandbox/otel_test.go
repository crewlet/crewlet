package sandbox_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/secrets"
)

// What these tests protect.
//
// The receiver exists so a box running generated code never holds the
// telemetry backend's ingest credential. Every failure of that is silent in
// the same way: the export succeeds, the trace appears, and the credential is
// somewhere it should not be — or the token check is loose and anybody who
// can reach the endpoint writes into the company's telemetry.

// otelTokens builds a minter, failing the test on a keyring it refuses.
func otelTokens(t *testing.T, opts sandbox.OtelTokenOptions) *sandbox.OtelTokens {
	t.Helper()
	tokens, err := sandbox.NewOtelTokens(opts)
	if err != nil {
		t.Fatalf("NewOtelTokens: %v", err)
	}
	return tokens
}

// keyring builds a Tier A keyring from fixed material, so two calls with the
// same fill are two nodes holding the same key.
func keyring(active string, keys map[string]byte) secrets.Keyring {
	out := secrets.Keyring{ActiveID: active, Keys: map[string][]byte{}}
	for id, fill := range keys {
		out.Keys[id] = bytes.Repeat([]byte{fill}, 32)
	}
	return out
}

// A TOKEN MINTED ON ONE NODE VERIFIES ON ANOTHER — THROUGH A KEYRING
// ROTATION TOO.
//
// This is the property the whole design turns on. Minting and verifying
// happen on different nodes of a fleet, since a box exports to whichever node
// it can reach, and an in-memory store makes them the same process by
// assumption: every trace that reached another node would be answered 401,
// visible only as exporter retry noise inside a sandbox nobody watches.
//
// And a keyring rotates one restart at a time, so the receiver has to be built
// from the WHOLE ring rather than its active key: here the node that has added
// the new key mints under the old one, which the node still holding only the
// old one verifies; and once the new key is active, a box launched before the
// flip still exports to a node that flipped.
func TestAnOtelTokenVerifiesOnAnotherNodeThroughARotation(t *testing.T) {
	t.Parallel()
	env := func(name string) string {
		if name == sandbox.OtelReceiverURLVar {
			return "https://engine.internal"
		}
		return ""
	}
	build := func(keys secrets.Keyring) *sandbox.OtelReceiver {
		t.Helper()
		receiver, err := sandbox.BuildOtelReceiver(env, keys)
		if err != nil || receiver == nil {
			t.Fatalf("BuildOtelReceiver = %v, %v", receiver, err)
		}
		return receiver
	}
	token := func(r *sandbox.OtelReceiver, trace string) string {
		return strings.TrimPrefix(r.EndpointFor(trace, time.Hour), "https://engine.internal/otlp/")
	}
	before := build(keyring("k1", map[string]byte{"k1": 1}))
	added := build(keyring("k1", map[string]byte{"k1": 1, "k2": 2}))
	flipped := build(keyring("k2", map[string]byte{"k1": 1, "k2": 2}))

	if got := before.Validate(token(added, "trace-abc")); got != "trace-abc" {
		t.Errorf("a node that has not added the new key refused a token minted beside it: %q", got)
	}
	if got := added.Validate(token(before, "trace-abc")); got != "trace-abc" {
		t.Errorf("a node that added the new key refused a token minted before it did: %q", got)
	}
	if got := flipped.Validate(token(added, "trace-abc")); got != "trace-abc" {
		t.Errorf("a node that activated the new key refused a box launched before it: %q", got)
	}
	if got := added.Validate(token(flipped, "trace-abc")); got != "trace-abc" {
		t.Errorf("a node that has not activated the new key refused a token minted under it: %q", got)
	}

	// AND A DIFFERENT KEYRING DOES NOT VERIFY, or the keyring would be
	// decoration.
	other := build(keyring("k1", map[string]byte{"k1": 9}))
	if got := other.Validate(token(before, "trace-abc")); got != "" {
		t.Errorf("a token verified under an unrelated keyring as %q", got)
	}
}

// AN OTLP TOKEN IS THE RECEIVER'S ALONE. Under one keyring, a token signed for
// any other endpoint — the tool bridge's above all — is refused here, and one
// signed in this receiver's own domain is not: the domain is a constant of
// this receiver, never something a caller passes and could get wrong.
func TestAnOtelTokenIsKeyedToTheReceiversDomain(t *testing.T) {
	t.Parallel()
	keys := keyring("k1", map[string]byte{"k1": 1})
	tokens := otelTokens(t, sandbox.OtelTokenOptions{Keys: keys})
	mint := func(domain string) string {
		t.Helper()
		signer, err := runtoken.New(runtoken.Options{Domain: domain, Keys: keys})
		if err != nil {
			t.Fatal(err)
		}
		return signer.Mint("trace-abc", time.Hour)
	}
	if got := tokens.Validate(mint("another-endpoint")); got != "" {
		t.Errorf("a token signed for another endpoint validated as %q", got)
	}
	if got := tokens.Validate(mint(sandbox.OtelKeyDomain)); got != "trace-abc" {
		t.Errorf("a token signed in the receiver's own domain validated as %q", got)
	}
}

// A FORGED, MALFORMED OR EXPIRED TOKEN IS REFUSED — and the caller cannot
// tell which, because telling it tells an attacker the same. The format's own
// cases are runtoken's; these are the receiver's clock and keyring reaching
// the signer.
func TestOtelTokensAreRefusedEveryWayTheyCanBeWrong(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	clock := at
	tokens := otelTokens(t, sandbox.OtelTokenOptions{
		Keys: keyring("k1", map[string]byte{"k1": 1}),
		Now:  func() time.Time { return clock },
	})
	good := tokens.Mint("trace-abc", 10*time.Minute)
	if tokens.Validate(good) != "trace-abc" {
		t.Fatal("a fresh token did not validate")
	}

	for _, tc := range []struct{ name, token string }{
		{"empty", ""},
		{"not a token at all", "hello"},
		{"a truncated signature", good[:len(good)-4]},
		{"a per-process key's token", otelTokens(t, sandbox.OtelTokenOptions{}).Mint("trace-abc", time.Hour)},
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
	tokens := otelTokens(t, sandbox.OtelTokenOptions{})
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
		Tokens:           otelTokens(t, sandbox.OtelTokenOptions{}),
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
		Tokens:  otelTokens(t, sandbox.OtelTokenOptions{}),
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
		Tokens:           otelTokens(t, sandbox.OtelTokenOptions{}),
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
		Tokens:  otelTokens(t, sandbox.OtelTokenOptions{}),
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
		Tokens:  otelTokens(t, sandbox.OtelTokenOptions{}),
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
		Tokens: otelTokens(t, sandbox.OtelTokenOptions{}),
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

	got, err := sandbox.BuildOtelReceiver(lookup, secrets.Keyring{})
	if err != nil || got != nil {
		t.Fatalf("an unset receiver URL built %v (%v)", got, err)
	}

	env[sandbox.OtelReceiverURLVar] = "https://engine.internal"
	env[sandbox.OtelUpstreamEndpointVar] = "https://collector.example"
	env[sandbox.OtelUpstreamHeadersVar] = "api-key=abc"
	got, err = sandbox.BuildOtelReceiver(lookup, keyring("k1", map[string]byte{"k1": 1}))
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("a configured receiver URL built nothing")
	}
	if !got.Forwards() {
		t.Error("a configured upstream reports no forwarding")
	}

	// A KEYRING THAT CANNOT SAY WHICH KEY MINTS IS REFUSED, rather than
	// built into a receiver whose every endpoint nothing can verify.
	broken := keyring("k9", map[string]byte{"k1": 1})
	if got, err = sandbox.BuildOtelReceiver(lookup, broken); err == nil {
		t.Errorf("a keyring with no usable active key built %v", got)
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
		Tokens:  otelTokens(t, sandbox.OtelTokenOptions{}),
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
