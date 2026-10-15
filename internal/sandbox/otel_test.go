package sandbox_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/otlpenv"
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
// happen on different nodes of a fleet, since a box exports to whichever node
// it can reach, and an in-memory store makes them the same process by
// assumption: every trace that reached another node would be answered 401,
// visible only as exporter retry noise inside a sandbox nobody watches.
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

// upstreamOf resolves a receiver's upstream from a literal environment, the
// way BuildOtelReceiver resolves it from the process's.
func upstreamOf(t *testing.T, kv map[string]string) map[otlpenv.Signal]otlpenv.Destination {
	t.Helper()
	upstream, err := otlpenv.ResolveAll(func(name string) string { return kv[name] })
	if err != nil {
		t.Fatal(err)
	}
	return upstream
}

// otelUpstream is a stand-in for the real telemetry backend.
type otelUpstream struct {
	mu       sync.Mutex
	paths    []string
	headers  http.Header
	seen     []http.Header
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
	u.seen = append(u.seen, r.Header.Clone())
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
//
// The credential is written as the documentation writes it, percent-encoded,
// and must arrive DECODED: the receiver once sent `Bearer%20ingest-secret`
// verbatim and the backend refused every forwarded export.
func TestTheUpstreamCredentialIsAddedOutsideTheSandbox(t *testing.T) {
	t.Parallel()
	upstream := newOtelUpstream()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)

	receiver, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
		BaseURL: "https://engine.internal",
		Tokens:  sandbox.NewOtelTokens(sandbox.OtelTokenOptions{Key: []byte("k")}),
		Upstream: upstreamOf(t, map[string]string{
			otlpenv.EndpointVar: server.URL,
			otlpenv.HeadersVar:  "Authorization=Bearer%20ingest-secret",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !receiver.Forwards(otlpenv.Traces) {
		t.Fatal("a receiver with an upstream reports it forwards nothing")
	}

	receiver.Forward(context.Background(), otlpenv.Traces, []byte("payload"), "application/x-protobuf")
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
	for _, signal := range otlpenv.Signals {
		if receiver.Forwards(signal) {
			t.Fatalf("a receiver with no upstream claims to forward %s", signal)
		}
		// Must not panic or block.
		receiver.Forward(context.Background(), signal, []byte("payload"), "")
	}
}

// EACH SIGNAL GOES WHERE THE ENGINE'S OWN EXPORTER SENDS IT, with that
// signal's headers. The receiver used to append `/v1/<signal>` to the base
// endpoint whatever else was set, so a deployment that configured only
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT exported the engine's spans and dropped
// every box's — accepted, answered 200, and sent nowhere.
//
// Mutation: forward to the base with the signal path appended and the traces
// export lands on /base/v1/traces; merge a signal's headers into the base set
// and x-tenant reaches the metrics request.
func TestEachSignalIsForwardedToItsOwnDestination(t *testing.T) {
	t.Parallel()
	upstream := newOtelUpstream()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)

	receiver, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
		BaseURL: "https://engine.internal",
		Tokens:  sandbox.NewOtelTokens(sandbox.OtelTokenOptions{Key: []byte("k")}),
		Upstream: upstreamOf(t, map[string]string{
			otlpenv.EndpointVar:          server.URL + "/base/",
			otlpenv.Traces.EndpointVar(): server.URL + "/custom/traces",
			otlpenv.HeadersVar:           "x-tenant=acme",
			otlpenv.Metrics.HeadersVar(): "x-metrics-key=m%201",
			otlpenv.Logs.ExporterVar():   "none",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	receiver.Forward(ctx, otlpenv.Traces, []byte("spans"), "application/x-protobuf")
	receiver.Forward(ctx, otlpenv.Metrics, []byte("points"), "application/x-protobuf")
	receiver.Forward(ctx, otlpenv.Logs, []byte("records"), "application/x-protobuf")

	if got := upstream.saw(); len(got) != 2 || got[0] != "/custom/traces" ||
		got[1] != "/base/v1/metrics" {
		t.Fatalf("forwarded to %v, want [/custom/traces /base/v1/metrics] — and "+
			"no logs, which OTEL_LOGS_EXPORTER=none declined", got)
	}
	upstream.mu.Lock()
	traces, metrics := upstream.seen[0], upstream.seen[1]
	upstream.mu.Unlock()
	if traces.Get("X-Tenant") != "acme" || traces.Get("X-Metrics-Key") != "" {
		t.Errorf("traces carried %v, want the base headers alone", traces)
	}
	if metrics.Get("X-Metrics-Key") != "m 1" || metrics.Get("X-Tenant") != "" {
		t.Errorf("metrics carried %v, want their own headers alone, decoded", metrics)
	}
	if got := receiver.Forwarding(); len(got) != 2 {
		t.Errorf("Forwarding() = %v, want traces and metrics", got)
	}
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
		BaseURL:  "https://engine.internal",
		Tokens:   sandbox.NewOtelTokens(sandbox.OtelTokenOptions{Key: []byte("k")}),
		Upstream: upstreamOf(t, map[string]string{otlpenv.EndpointVar: server.URL}),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Forward returns nothing at all: there is no error for a caller to
	// mistakenly report to the sandbox.
	receiver.Forward(context.Background(), otlpenv.Traces, []byte("payload"), "")
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
		Upstream: upstreamOf(t, map[string]string{
			otlpenv.EndpointVar: "https://collector.example",
			otlpenv.HeadersVar:  "Authorization=Bearer%20ingest-secret",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	env := receiver.RunEnv(t.Context(), launchedFrom, "turn-1", "engineer", time.Hour)
	endpoint := env[otlpenv.EndpointVar]
	if !strings.HasPrefix(endpoint, "https://engine.internal/otlp/") {
		t.Fatalf("the box exports to %q, which is not the engine", endpoint)
	}
	if strings.Contains(endpoint, "collector.example") {
		t.Fatal("the box was pointed at the real backend")
	}
	headers, set := env[otlpenv.HeadersVar]
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
	if got := receiver.Validate(token); got != launchedFrom.TraceID {
		t.Errorf("the endpoint's token names %q", got)
	}
	if attrs := env["OTEL_RESOURCE_ATTRIBUTES"]; !strings.Contains(attrs, "turn-1") ||
		!strings.Contains(attrs, "engineer") {
		t.Errorf("the run's spans name no turn or seat: %q", attrs)
	}
}

// launchedFrom is the span a run in these cases is launched from.
var launchedFrom = events.TraceContext{
	TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7",
}

// THE BOX'S TRACEPARENT NESTS ITS SPANS UNDER THE TURN'S, AND CARRIES THE
// TURN'S SAMPLING DECISION.
//
// Without TRACEPARENT the box's spans form a second, unrelated trace that
// nobody finds. With it hardcoded to `-01`, as it was, a run launched from a
// turn the operator's sampler had decided not to record was exported from
// inside the box anyway — children under a parent nobody exported. So the flag
// is the turn's own, both ways, and a run whose launching span has no id hands
// the box no parent at all rather than a malformed one.
//
// Mutation: write `-01` whatever the turn decided and the unsampled case goes
// red.
func TestTheBoxsTraceparentFollowsTheTurnsDecision(t *testing.T) {
	t.Parallel()
	receiver, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
		BaseURL: "https://engine.internal",
		Tokens:  sandbox.NewOtelTokens(sandbox.OtelTokenOptions{Key: []byte("k")}),
	})
	if err != nil {
		t.Fatal(err)
	}
	unsampled := launchedFrom
	unsampled.Unsampled = true
	noSpan := events.TraceContext{TraceID: launchedFrom.TraceID}
	for _, tc := range []struct {
		name  string
		trace events.TraceContext
		want  string
	}{
		{"sampled", launchedFrom, "00-" + launchedFrom.TraceID + "-" + launchedFrom.SpanID + "-01"},
		{"unsampled", unsampled, "00-" + launchedFrom.TraceID + "-" + launchedFrom.SpanID + "-00"},
		{"no span to nest under", noSpan, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := receiver.RunEnv(t.Context(), tc.trace, "turn-1", "engineer", time.Hour)
			got, set := env[sandbox.TraceparentVar]
			if got != tc.want || set != (tc.want != "") {
				t.Errorf("%s = %q (set: %v), want %q", sandbox.TraceparentVar, got, set, tc.want)
			}
			if env[otlpenv.EndpointVar] == "" {
				t.Error("a run with a trace got no endpoint")
			}
		})
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
	if env := receiver.RunEnv(t.Context(), events.TraceContext{SpanID: launchedFrom.SpanID},
		"turn-1", "engineer", time.Hour); len(env) != 0 {
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
	env[otlpenv.Traces.EndpointVar()] = "https://collector.example/v1/traces"
	env[otlpenv.HeadersVar] = "api-key=abc"
	got, err = sandbox.BuildOtelReceiver(lookup, []string{"k1:material"})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("a configured receiver URL built nothing")
	}
	// ONLY THE SIGNAL THAT HAS A DESTINATION FORWARDS: the traces endpoint
	// is not a base, so metrics and logs have nowhere to go.
	if !got.Forwards(otlpenv.Traces) {
		t.Error("a configured traces endpoint reports no forwarding")
	}
	if got.Forwards(otlpenv.Metrics) || got.Forwards(otlpenv.Logs) {
		t.Errorf("signals with no destination forward: %v", got.Forwarding())
	}
	if h := got.Upstream(otlpenv.Traces).Headers; h["api-key"] != "abc" {
		t.Errorf("the upstream headers are %v", h)
	}
}

// THE RECEIVER KEEPS ITS OWN COPY OF THE CREDENTIAL. The upstream's headers
// are the backend's ingest credential, and a map shared with the caller — in
// either direction — is one a later write elsewhere changes under every box's
// forward.
func TestTheReceiverKeepsItsOwnCopyOfTheCredential(t *testing.T) {
	t.Parallel()
	upstream := upstreamOf(t, map[string]string{
		otlpenv.EndpointVar: "https://collector.example",
		otlpenv.HeadersVar:  "authorization=Bearer%20ingest-secret",
	})
	receiver, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
		BaseURL:  "https://engine.internal",
		Tokens:   sandbox.NewOtelTokens(sandbox.OtelTokenOptions{Key: []byte("k")}),
		Upstream: upstream,
	})
	if err != nil {
		t.Fatal(err)
	}
	upstream[otlpenv.Traces].Headers["authorization"] = "changed by the caller"
	receiver.Upstream(otlpenv.Traces).Headers["authorization"] = "changed by a reader"
	if got := receiver.Upstream(otlpenv.Traces).Headers["authorization"]; got != "Bearer ingest-secret" {
		t.Errorf("the receiver's credential is now %q", got)
	}
}

// AN UPSTREAM THAT CAN NEVER WORK REFUSES THE BOOT, naming the variable — the
// same refusal the engine's own exporter gives for the same value, because the
// two resolve through one function. Accepting it would answer every box's
// export 200 and forward it nowhere.
func TestAnUnusableUpstreamRefusesTheReceiver(t *testing.T) {
	t.Parallel()
	for name, value := range map[string]string{
		otlpenv.EndpointVar:        "localhost:4318",
		otlpenv.ProtocolVar:        "http",
		otlpenv.Logs.ExporterVar(): "console",
	} {
		env := map[string]string{
			sandbox.OtelReceiverURLVar: "https://engine.internal",
			name:                       value,
		}
		_, err := sandbox.BuildOtelReceiver(func(k string) string { return env[k] }, nil)
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s=%q built a receiver, or was refused without its name: %v",
				name, value, err)
		}
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

	env := sandbox.RunEnvFor(t.Context(), manager, sandbox.LaunchRequest{
		// LAUNCHED FROM AN UNSAMPLED TURN, so the TRACEPARENT below is
		// read off the launch's own record of the decision — the path
		// every real launch takes — rather than off the receiver alone.
		Turn: sandbox.TurnRef{
			TraceID: launchedFrom.TraceID, SpanID: launchedFrom.SpanID,
			TraceUnsampled: true,
			TurnID:         "turn-1", AgentHandle: "engineer",
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
	// lose the trace context that nests its spans under the turn — with the
	// turn's own decision as its flag.
	want := "00-" + launchedFrom.TraceID + "-" + launchedFrom.SpanID + "-00"
	if got := env[sandbox.TraceparentVar]; got != want {
		t.Errorf("TRACEPARENT = %q, want %q", got, want)
	}
	if env["ANTHROPIC_API_KEY"] != "sk-test" {
		t.Errorf("the run environment lost a credential: %q", env["ANTHROPIC_API_KEY"])
	}
}
