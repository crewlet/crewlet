package sandbox_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// These tests protect the receiver's job: a box running generated code never
// holds the telemetry backend's ingest credential, and only a run's own token
// writes into the company's telemetry. Every failure of that is silent: the
// export succeeds and the trace appears.

// otelTokens is [sandbox.NewOtelTokens] for a keyring a case expects to be
// usable.
func otelTokens(t *testing.T, opts sandbox.OtelTokenOptions) *sandbox.OtelTokens {
	t.Helper()
	tokens, err := sandbox.NewOtelTokens(opts)
	if err != nil {
		t.Fatalf("NewOtelTokens: %v", err)
	}
	return tokens
}

// minted is tokens' token for traceID, valid for ttl, failing t where none is
// minted.
func minted(t *testing.T, tokens *sandbox.OtelTokens, traceID string, ttl time.Duration) string {
	t.Helper()
	token, err := tokens.Mint(traceID, ttl)
	if err != nil {
		t.Fatalf("Mint(%q): %v", traceID, err)
	}
	return token
}

// runEnv is the environment receiver hands a run
// ([sandbox.OtelReceiver.RunEnv]), failing t where it refuses one.
func runEnv(t *testing.T, receiver *sandbox.OtelReceiver, traceID, spanID string, onHost bool) map[string]string {
	t.Helper()
	env, err := receiver.RunEnv(traceID, spanID, "turn-1", "engineer", onHost)
	if err != nil {
		t.Fatalf("RunEnv(%q): %v", traceID, err)
	}
	return env
}

// runEnvFor is [sandbox.RunEnvFor], failing t where it refuses.
func runEnvFor(t *testing.T, m *sandbox.Manager, req sandbox.LaunchRequest, box sandbox.Sandbox) map[string]string {
	t.Helper()
	env, err := sandbox.RunEnvFor(m, req, box)
	if err != nil {
		t.Fatalf("RunEnvFor: %v", err)
	}
	return env
}

// exportingRunner is a runner whose harness exports by the run's telemetry,
// which a launch alone hands such a runner.
func exportingRunner() *sandbox.FakeRunner {
	runner := sandbox.NewFakeRunner("claude-code")
	runner.Exports = true
	return runner
}

// ONLY A RUNNER WHOSE HARNESS EXPORTS IS HANDED CREWLET'S TELEMETRY: any other
// runner's launch carries the seat's own environment exactly, an operator's
// own exporter settings included, and none of Crewlet's names, so its model's
// shell never holds the run's endpoint or trace context for nothing.
//
// Mutation: hand the telemetry whatever the runner's trait, or drop the
// operator's own values for a runner without it: this goes red.
func TestOnlyARunnerWhoseHarnessExportsIsHandedTheTelemetry(t *testing.T) {
	t.Parallel()
	receiver, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
		BaseURL: "https://engine.internal",
		Tokens:  otelTokens(t, sandbox.OtelTokenOptions{Domain: sandbox.OtelKeyDomain, Material: runtoken.OneKey("k", "k")}),
	})
	if err != nil {
		t.Fatal(err)
	}
	own := map[string]string{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "https://collector.example.com/v1/logs",
		"OTEL_SERVICE_NAME": "seat-own", "ANTHROPIC_API_KEY": "sk-test"}
	for name, exports := range map[string]bool{"a runner that exports": true, "a runner that does not": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner := sandbox.NewFakeRunner("claude-code")
			runner.Exports = exports
			manager, err := sandbox.NewManager(sandbox.ManagerOptions{
				Providers: map[sandbox.Placement]sandbox.Provider{sandbox.Direct: sandbox.NewFakeProvider()},
				Runners:   map[string]sandbox.Runner{"claude-code": runner},
				Telemetry: receiver,
			})
			if err != nil {
				t.Fatal(err)
			}
			env := runEnvFor(t, manager, sandbox.LaunchRequest{
				Turn: sandbox.TurnRef{TraceID: "trace-abc", SpanID: "span-def", TurnID: "turn-1", AgentHandle: "engineer"},
				Spec: sandbox.Spec{Harness: "claude-code", TimeoutSec: 900, Env: own},
			}, sandbox.NewFakeSandbox("box"))
			for k, v := range own {
				if env[k] != v {
					t.Errorf("the launch hands %s=%q, want the seat's own %q", k, env[k], v)
				}
			}
			for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_PROTOCOL",
				"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_RESOURCE_ATTRIBUTES", "TRACEPARENT"} {
				if _, handed := env[k]; handed != exports {
					t.Errorf("the launch hands %s: %v, want %v", k, handed, exports)
				}
			}
		})
	}
}

// A TOKEN MINTED IN ONE PROCESS VERIFIES IN ANOTHER: a box exports to whichever
// node it reaches, so a per-process key would answer every trace reaching
// another node 401, seen only as exporter retries in a sandbox nobody watches.
func TestAnOtelTokenVerifiesInAnotherProcess(t *testing.T) {
	t.Parallel()
	material := runtoken.Material{
		ActiveID: "k2",
		Keys: []runtoken.KeyMaterial{
			{ID: "k1", Material: "material-one"},
			{ID: "k2", Material: "material-two"},
		},
	}
	opts := func(m runtoken.Material) sandbox.OtelTokenOptions {
		return sandbox.OtelTokenOptions{Domain: sandbox.OtelKeyDomain, Material: m}
	}

	minter := otelTokens(t, opts(material))
	verifier := otelTokens(t, opts(material))

	token := minted(t, minter, "trace-abc", time.Hour)
	if got := verifier.Validate(token); got != "trace-abc" {
		t.Fatalf("a token minted elsewhere validated to %q", got)
	}

	// THE KEY IS ORDER-INDEPENDENT, because two processes read the same
	// keyring and nothing promises the same order — a map iteration or a
	// re-ordered document would otherwise split a fleet in two.
	reordered := runtoken.Material{ActiveID: "k2", Keys: []runtoken.KeyMaterial{
		{ID: "k2", Material: "material-two"}, {ID: "k1", Material: "material-one"},
	}}
	if otelTokens(t, opts(reordered)).Validate(token) != "trace-abc" {
		t.Error("the signing key depends on the keyring's order")
	}

	// A ROTATION DOES NOT INVALIDATE A LIVE RUN'S TOKEN: the token names the key
	// that signed it, the tag's whole reason, and k2 is still on the ring when the
	// operator adds k3 and makes it active.
	rotated := runtoken.Material{ActiveID: "k3", Keys: []runtoken.KeyMaterial{
		{ID: "k1", Material: "material-one"},
		{ID: "k2", Material: "material-two"},
		{ID: "k3", Material: "material-three"},
	}}
	if got := otelTokens(t, opts(rotated)).Validate(token); got != "trace-abc" {
		t.Errorf("a rotation invalidated a token minted under a key still on the ring: %q", got)
	}

	// AND DROPPING THE KEY THAT SIGNED IT DOES invalidate it, or the drop
	// would do nothing: an unknown tag is refused, never retried against
	// whichever key happens to be active.
	dropped := runtoken.OneKey("k3", "material-three")
	if otelTokens(t, opts(dropped)).Validate(token) != "" {
		t.Error("a token verified after the key that signed it left the ring")
	}

	// AND A DIFFERENT KEYRING DOES NOT VERIFY, or the derivation would be
	// decoration.
	other := runtoken.OneKey("k2", "someone-elses")
	if otelTokens(t, opts(other)).Validate(token) != "" {
		t.Error("a token verified under an unrelated keyring")
	}
}

// A FORGED, MALFORMED OR EXPIRED TOKEN IS REFUSED — and the caller cannot
// tell which, because telling it tells an attacker the same.
func TestOtelTokensAreRefusedEveryWayTheyCanBeWrong(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	clock := at
	tokens := otelTokens(t, sandbox.OtelTokenOptions{
		Material: runtoken.OneKey("k", "a-shared-signing-key"),
		Now:      func() time.Time { return clock },
	})
	good := minted(t, tokens, "trace-abc", 10*time.Minute)
	if tokens.Validate(good) != "trace-abc" {
		t.Fatal("a fresh token did not validate")
	}

	for _, tc := range []struct{ name, token string }{
		{"empty", ""},
		{"not a token at all", "hello"},
		{"too few parts", "v1.trace-abc.999"},
		{"an unknown version", "v9" + good[2:]},
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
	tokens := otelTokens(t, sandbox.OtelTokenOptions{Domain: sandbox.OtelKeyDomain, Material: runtoken.OneKey("k", "k")})
	first := minted(t, tokens, "trace-one", time.Hour)
	second := minted(t, tokens, "trace-two", time.Hour)
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
		Tokens:           otelTokens(t, sandbox.OtelTokenOptions{Domain: sandbox.OtelKeyDomain, Material: runtoken.OneKey("k", "k")}),
		TokenTTL:         time.Hour,
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
		BaseURL:  "https://engine.internal",
		Tokens:   otelTokens(t, sandbox.OtelTokenOptions{Domain: sandbox.OtelKeyDomain, Material: runtoken.OneKey("k", "k")}),
		TokenTTL: time.Hour,
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
// two: the box's own traffic multiplies while nothing it carries is
// recoverable anyway.
func TestAFailingUpstreamNeverRaises(t *testing.T) {
	t.Parallel()
	upstream := newOtelUpstream()
	upstream.status = http.StatusServiceUnavailable
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)

	receiver, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
		BaseURL:          "https://engine.internal",
		Tokens:           otelTokens(t, sandbox.OtelTokenOptions{Domain: sandbox.OtelKeyDomain, Material: runtoken.OneKey("k", "k")}),
		TokenTTL:         time.Hour,
		UpstreamEndpoint: server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Forward returns nothing at all: there is no error for a caller to
	// mistakenly report to the sandbox.
	receiver.Forward(context.Background(), "traces", []byte("payload"), "")
}

// THE RUN ENVIRONMENT CARRIES AN ENDPOINT AND NO CREDENTIAL. The headers
// variable is set EMPTY rather than unset, since the exporter otherwise reads
// the one the box inherited from the engine host.
func TestTheRunEnvironmentCarriesNoCredential(t *testing.T) {
	t.Parallel()
	receiver, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
		BaseURL:  "https://engine.internal/",
		Tokens:   otelTokens(t, sandbox.OtelTokenOptions{Domain: sandbox.OtelKeyDomain, Material: runtoken.OneKey("k", "k")}),
		TokenTTL: time.Hour,
		// A real upstream, whose credential must NOT appear below.
		UpstreamEndpoint: "https://collector.example",
		UpstreamHeaders:  map[string]string{"Authorization": "Bearer ingest-secret"},
	})
	if err != nil {
		t.Fatal(err)
	}

	env := runEnv(t, receiver, "trace-abc", "span-def", false)
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

// A RUN WITH NO TRACE GETS NO ENDPOINT: a token scoped to an empty trace would
// authenticate every run's export as every other run's.
func TestARunWithNoTraceGetsNoEndpoint(t *testing.T) {
	t.Parallel()
	receiver, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
		BaseURL:  "https://engine.internal",
		Tokens:   otelTokens(t, sandbox.OtelTokenOptions{Domain: sandbox.OtelKeyDomain, Material: runtoken.OneKey("k", "k")}),
		TokenTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if env := runEnv(t, receiver, "", "span", false); len(env) != 0 {
		t.Fatalf("a trace-less run got %v", env)
	}
}

// A TRACE NO TOKEN CAN CARRY REDACTED GETS NO ENDPOINT, AND ITS JOB NO START: a
// trace id holding a dot would mint a token internal/redact does not know by
// its shape, persisted wherever a box prints its environment. The refusal
// reaches the run's and the job's environment by name, never as an endpoint.
//
// Mutation: drop the error on its way out of the receiver, and the run is
// handed an environment with no endpoint, as though no address reached it.
func TestATraceNoTokenCanCarryGetsNoEndpoint(t *testing.T) {
	t.Parallel()
	receiver, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
		BaseURL:  "https://engine.internal",
		Tokens:   otelTokens(t, sandbox.OtelTokenOptions{Domain: sandbox.OtelKeyDomain, Material: runtoken.OneKey("k", "k")}),
		TokenTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if env, err := receiver.RunEnv("trace.abc", "span-def", "turn-1", "engineer", false); !errors.Is(err,
		runtoken.ErrSubject) || env != nil {
		t.Errorf("a trace id holding a dot was handed %v (%v), want runtoken.ErrSubject", env, err)
	}
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers: map[sandbox.Placement]sandbox.Provider{sandbox.Direct: sandbox.NewFakeProvider()},
		Runners:   map[string]sandbox.Runner{"claude-code": exportingRunner()},
		Telemetry: receiver,
	})
	if err != nil {
		t.Fatal(err)
	}
	if env, err := sandbox.RunEnvFor(manager, sandbox.LaunchRequest{
		Turn: sandbox.TurnRef{TraceID: "trace.abc", SpanID: "span-def", TurnID: "turn-1", AgentHandle: "engineer"},
		Spec: sandbox.Spec{Harness: "claude-code"},
	}, sandbox.NewFakeSandbox("d")); !errors.Is(err, runtoken.ErrSubject) || env != nil {
		t.Errorf("a job whose trace id holds a dot was handed %v (%v), want runtoken.ErrSubject", env, err)
	}
}

// A RUN'S ENDPOINT LIVES ITS RECEIVER'S TOKEN TTL
// ([sandbox.OtelReceiverOptions.TokenTTL], [sandbox.OtelTokenTTL] for every
// engine's receiver): valid to the last second of it and refused past it. A
// receiver told no lifetime is refused at construction, never given one of its
// own.
//
// Mutation: mint with any other span, or default a zero TTL, and a check fails.
func TestARunsEndpointLivesItsTokenTTL(t *testing.T) {
	t.Parallel()
	tokens := otelTokens(t, sandbox.OtelTokenOptions{Domain: sandbox.OtelKeyDomain, Material: runtoken.OneKey("k", "k")})
	if _, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
		BaseURL: "https://engine.internal", Tokens: tokens,
	}); err == nil {
		t.Fatal("a receiver told no token lifetime was built")
	}
	synctest.Test(t, func(t *testing.T) {
		const ttl = 4 * time.Hour
		receiver, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
			BaseURL: "https://engine.internal", Tokens: tokens, TokenTTL: ttl,
		})
		if err != nil {
			t.Fatal(err)
		}
		endpoint := runEnv(t, receiver, "trace-abc", "span-def", false)["OTEL_EXPORTER_OTLP_ENDPOINT"]
		token := strings.TrimPrefix(endpoint, "https://engine.internal/otlp/")
		time.Sleep(ttl - time.Second)
		if got := receiver.Validate(token); got != "trace-abc" {
			t.Fatalf("a second before its TTL the endpoint's token names %q, want the run's trace", got)
		}
		time.Sleep(2 * time.Second)
		if got := receiver.Validate(token); got != "" {
			t.Fatalf("a second past its TTL the endpoint's token still names %q", got)
		}
	})
}

// A RECEIVER WITH NO REACHABLE ADDRESS IS REFUSED at construction: an
// endpoint minted against an address the box cannot resolve fails silently
// inside the box, which is the one place nobody is looking.
func TestAReceiverNeedsAnAddressTheBoxCanReach(t *testing.T) {
	t.Parallel()
	if _, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
		Tokens:   otelTokens(t, sandbox.OtelTokenOptions{Domain: sandbox.OtelKeyDomain, Material: runtoken.OneKey("k", "k")}),
		TokenTTL: time.Hour,
	}); err == nil {
		t.Fatal("a receiver with no base URL was built")
	}
	if _, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
		BaseURL: "https://engine.internal", TokenTTL: time.Hour,
	}); err == nil {
		t.Fatal("a receiver with no token minter was built")
	}
}

// ONLY THE SIGNALS THIS RECEIVER FORWARDS ARE ACCEPTED: the signal is joined
// into the upstream URL, so an unchecked one lets a caller choose part of the
// address the engine's OWN credential is sent to.
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

	got, err := sandbox.BuildOtelReceiver(lookup, runtoken.Material{}, "")
	if err != nil || got != nil {
		t.Fatalf("an unset receiver URL built %v (%v)", got, err)
	}

	env[sandbox.OtelReceiverURLVar] = "https://engine.internal"
	env[sandbox.OtelUpstreamEndpointVar] = "https://collector.example"
	env[sandbox.OtelUpstreamHeadersVar] = "api-key=abc"

	// A KEYRING THAT CANNOT SIGN FOR THE FLEET IS REFUSED, not given a key of
	// this process's own, which validates only at the minting node. Mutation: fall
	// back to a per-process key and this builds a receiver.
	if got, err = sandbox.BuildOtelReceiver(lookup, runtoken.Material{}, ""); !errors.Is(err, runtoken.ErrNoKeyring) || got != nil {
		t.Fatalf("a receiver with no keyring = %v, %v, want the refusal", got, err)
	}

	got, err = sandbox.BuildOtelReceiver(lookup, runtoken.OneKey("k1", "material"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("a configured receiver URL built nothing")
	}
	if !got.Forwards() {
		t.Error("a configured upstream reports no forwarding")
	}

	// AND A SET ADDRESS NO EXPORTER CAN USE IS AN ERROR, not a receiver that
	// mints every run an endpoint with no scheme.
	env[sandbox.OtelReceiverURLVar] = "engine.internal:443"
	got, err = sandbox.BuildOtelReceiver(lookup, runtoken.OneKey("k1", "material"), "")
	if err == nil || got != nil {
		t.Fatalf("a receiver URL with no scheme built %v (%v)", got, err)
	}
	if !strings.Contains(err.Error(), sandbox.OtelReceiverURLVar) {
		t.Errorf("the refusal does not name %s: %v", sandbox.OtelReceiverURLVar, err)
	}
}

// A BOX ON ITS NODE'S HOST EXPORTS TO THAT NODE, WHERE THE NODE FORWARDS. The
// node knows where a `direct` box reaches its listener, as the tool bridge
// does, so a deployment setting OTEL_EXPORTER_OTLP_ENDPOINT for the engine's
// own spans gets its direct boxes' telemetry with no receiver address
// published. A node with no upstream would drop what it received, so
// it hands its boxes the fleet's address where one is set, else no endpoint.
// A box off the host dials the fleet's address alone.
//
// Mutation: take the host address whatever the upstream, and "no upstream"
// builds a receiver that drops every export while "the fleet's address, no
// upstream" sends a direct box away from a node that forwards; prefer the
// fleet's address on the host, and "both" exports a direct box off the host.
func TestABoxOnTheHostExportsToItsNodeWhereTheNodeForwards(t *testing.T) {
	t.Parallel()
	const host, fleet = "http://127.0.0.1:8080", "https://engine.example.com"
	for name, tc := range map[string]struct {
		env             map[string]string
		onHost, offHost string // the endpoint's base, "" for none
		builds          bool
	}{
		"an upstream, no fleet address": {
			env:    map[string]string{sandbox.OtelUpstreamEndpointVar: "https://collector.example"},
			builds: true, onHost: host,
		},
		"no upstream, no fleet address": {
			env: map[string]string{},
		},
		"the fleet's address, no upstream": {
			env:    map[string]string{sandbox.OtelReceiverURLVar: fleet},
			builds: true, onHost: fleet, offHost: fleet,
		},
		"both": {
			env: map[string]string{sandbox.OtelReceiverURLVar: fleet,
				sandbox.OtelUpstreamEndpointVar: "https://collector.example"},
			builds: true, onHost: host, offHost: fleet,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			receiver, err := sandbox.BuildOtelReceiver(func(name string) string { return tc.env[name] },
				runtoken.OneKey("k1", "material"), host)
			if err != nil {
				t.Fatal(err)
			}
			if (receiver != nil) != tc.builds {
				t.Fatalf("built a receiver: %t, want %t", receiver != nil, tc.builds)
			}
			if receiver == nil {
				return
			}
			for _, side := range []struct {
				onHost bool
				want   string
			}{{true, tc.onHost}, {false, tc.offHost}} {
				endpoint := runEnv(t, receiver, "trace-abc", "span-def", side.onHost)["OTEL_EXPORTER_OTLP_ENDPOINT"]
				switch {
				case side.want == "" && endpoint != "":
					t.Errorf("a box on the host: %t was handed %q, want no endpoint", side.onHost, endpoint)
				case side.want != "" && !strings.HasPrefix(endpoint, side.want+"/otlp/"):
					t.Errorf("a box on the host: %t exports to %q, want %s", side.onHost, endpoint, side.want)
				}
			}
		})
	}
}

// THE CELL PICKS THE ADDRESS, AS THE BRIDGE'S DOES. A `direct` box is a process
// of the host and exports to its node's own listener; a `container` box is not
// on the host's loopback, whatever its runtime delivers, and dials the fleet's
// address, written as the box reaches it — and where there is none, it is
// handed no endpoint at all rather than one it cannot dial.
//
// Mutation: choose by the box rather than the cell, or hand a container box the
// host address, and the container rows read the node's loopback.
func TestTheRunsCellPicksTheAddressItExportsTo(t *testing.T) {
	t.Parallel()
	tokens := otelTokens(t, sandbox.OtelTokenOptions{Domain: sandbox.OtelKeyDomain, Material: runtoken.OneKey("k", "k")})
	both, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
		BaseURL: "http://127.0.0.1:9000", HostURL: "http://127.0.0.1:8080", Tokens: tokens, TokenTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	hostOnly, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
		HostURL: "http://127.0.0.1:8080", Tokens: tokens, TokenTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	container := aliasedBox{FakeSandbox: sandbox.NewFakeSandbox("c"), alias: "host.docker.internal"}
	for name, tc := range map[string]struct {
		receiver *sandbox.OtelReceiver
		cell     sandbox.Placement
		box      sandbox.Sandbox
		want     string // the endpoint's base, "" for none
	}{
		"a direct box":                      {both, sandbox.Direct, sandbox.NewFakeSandbox("d"), "http://127.0.0.1:8080"},
		"a container box":                   {both, sandbox.Container, container, "http://host.docker.internal:9000"},
		"a direct box, its node's alone":    {hostOnly, sandbox.Direct, sandbox.NewFakeSandbox("d"), "http://127.0.0.1:8080"},
		"a container box, no fleet address": {hostOnly, sandbox.Container, container, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			manager, err := sandbox.NewManager(sandbox.ManagerOptions{
				Providers: map[sandbox.Placement]sandbox.Provider{sandbox.Direct: sandbox.NewFakeProvider()},
				Runners:   map[string]sandbox.Runner{"claude-code": exportingRunner()},
				Telemetry: tc.receiver,
			})
			if err != nil {
				t.Fatal(err)
			}
			env := runEnvFor(t, manager, sandbox.LaunchRequest{
				Turn: sandbox.TurnRef{TraceID: "trace-abc", SpanID: "span-def", TurnID: "turn-1", AgentHandle: "engineer"},
				Spec: sandbox.Spec{Placement: tc.cell, Harness: "claude-code"},
			}, tc.box)
			endpoint, set := env["OTEL_EXPORTER_OTLP_ENDPOINT"]
			switch {
			case tc.want == "" && set:
				t.Errorf("the run was handed %q, want no endpoint", endpoint)
			case tc.want != "" && !strings.HasPrefix(endpoint, tc.want+"/otlp/"):
				t.Errorf("the run exports to %q, want %s", endpoint, tc.want)
			}
		})
	}
}

// THE OPERATOR'S OWN ENVIRONMENT WINS: role.sandbox.env is where an operator
// declares what a box gets, and overriding a collector set there would send
// somebody's spans somewhere other than the config says, invisibly.
func TestTheOperatorsTelemetryEnvironmentWins(t *testing.T) {
	t.Parallel()
	receiver, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
		BaseURL:  "https://engine.internal",
		Tokens:   otelTokens(t, sandbox.OtelTokenOptions{Domain: sandbox.OtelKeyDomain, Material: runtoken.OneKey("k", "k")}),
		TokenTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers: map[sandbox.Placement]sandbox.Provider{
			sandbox.Direct: sandbox.NewFakeProvider(),
		},
		Runners:   map[string]sandbox.Runner{"claude-code": exportingRunner()},
		Telemetry: receiver,
	})
	if err != nil {
		t.Fatal(err)
	}

	env := runEnvFor(t, manager, sandbox.LaunchRequest{
		Turn: sandbox.TurnRef{
			TraceID: "trace-abc", SpanID: "span-def",
			TurnID: "turn-1", AgentHandle: "engineer",
		},
		Spec: sandbox.Spec{
			Harness: "claude-code",
			Env: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT": "https://our-own-collector.example",
				"ANTHROPIC_API_KEY":           "sk-test",
			},
		},
	}, sandbox.NewFakeSandbox("box"))
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

// aliasedBox is a box whose network is not the engine host's, as a container
// box's is: it names the host by alias.
type aliasedBox struct {
	*sandbox.FakeSandbox
	alias string
}

func (b aliasedBox) EngineHost() string { return b.alias }

// THE RECEIVER IS REACHED AS THE BOX DIALS IT: its base is written in the
// engine host's network, whose loopback inside a container is the container.
// An operator's own endpoint is in the box's network already, and is left
// exactly as written.
func TestTheRunsEndpointIsTheReceiverAsTheBoxDialsIt(t *testing.T) {
	t.Parallel()
	receiver, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
		BaseURL:  "http://127.0.0.1:8000",
		Tokens:   otelTokens(t, sandbox.OtelTokenOptions{Domain: sandbox.OtelKeyDomain, Material: runtoken.OneKey("k", "k")}),
		TokenTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers: map[sandbox.Placement]sandbox.Provider{sandbox.Direct: sandbox.NewFakeProvider()},
		Runners:   map[string]sandbox.Runner{"claude-code": exportingRunner()},
		Telemetry: receiver,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := sandbox.LaunchRequest{
		Turn: sandbox.TurnRef{TraceID: "trace-abc", SpanID: "span-def", TurnID: "turn-1", AgentHandle: "engineer"},
		Spec: sandbox.Spec{Harness: "claude-code"},
	}
	container := aliasedBox{FakeSandbox: sandbox.NewFakeSandbox("c"), alias: "host.docker.internal"}

	got := runEnvFor(t, manager, req, container)["OTEL_EXPORTER_OTLP_ENDPOINT"]
	if !strings.HasPrefix(got, "http://host.docker.internal:8000/otlp/") {
		t.Errorf("a container box's endpoint = %q, want the receiver by the engine host's alias", got)
	}
	direct := runEnvFor(t, manager, req, sandbox.NewFakeSandbox("d"))["OTEL_EXPORTER_OTLP_ENDPOINT"]
	if !strings.HasPrefix(direct, "http://127.0.0.1:8000/otlp/") {
		t.Errorf("a box on the host's network was handed %q, want the receiver as written", direct)
	}

	req.Spec.Env = map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318"}
	if own := runEnvFor(t, manager, req, container)["OTEL_EXPORTER_OTLP_ENDPOINT"]; own != "http://127.0.0.1:4318" {
		t.Errorf("the operator's own endpoint was rewritten to %q", own)
	}
}
