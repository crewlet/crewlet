package api_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/otlpenv"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// What this route protects.
//
// It is deliberately reachable WITHOUT the API's own auth — the exporter
// inside a sandbox holds no API token, and giving it one would hand a box the
// credential that reads the whole company. So the per-run token in its path
// is the only thing standing between a coding sandbox and the company's
// telemetry backend, and every case below is about that token being checked
// before anything else happens.

// otlpReceiver builds a receiver whose tokens a test can mint, forwarding to
// the base endpoint upstream ("" forwards nothing).
func otlpReceiver(t *testing.T, upstream string) *sandbox.OtelReceiver {
	t.Helper()
	destination, err := otlpenv.Resolve(func(name string) string {
		if name == otlpenv.EndpointVar {
			return upstream
		}
		return ""
	}, otlpenv.Traces)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
		BaseURL:  "https://engine.internal",
		Tokens:   sandbox.NewOtelTokens(sandbox.OtelTokenOptions{Key: []byte("test-key")}),
		Upstream: destination,
	})
	if err != nil {
		t.Fatal(err)
	}
	return receiver
}

// tokenFor mints one run's token by reading it back out of its endpoint,
// which is the only way a caller ever gets one.
func tokenFor(t *testing.T, receiver *sandbox.OtelReceiver, traceID string) string {
	t.Helper()
	endpoint := receiver.RunEnv(t.Context(),
		events.TraceContext{TraceID: traceID, SpanID: "00f067aa0ba902b7"},
		"turn-1", "engineer", time.Hour)
	token := strings.TrimPrefix(
		endpoint[otlpenv.EndpointVar], "https://engine.internal/otlp/")
	if token == "" {
		t.Fatal("the run got no endpoint")
	}
	return token
}

func postOTLP(a *api.App, path string, body string) *http.Response {
	return postOTLPAs(a, path, body, "application/x-protobuf", "")
}

func postOTLPAs(a *api.App, path, body, contentType, encoding string) *http.Response {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	a.ServeHTTP(rec, req)
	return rec.Result()
}

// otlpTrace is the trace the tokens in these cases are minted for, and the
// trace the export below is in: the receiver forwards only what is in its
// token's trace.
var otlpTrace = strings.Repeat("ab", 16)

// spans is a traces export of one span in otlpTrace, as a box's exporter
// sends it.
func spans(t *testing.T) string {
	t.Helper()
	body, err := proto.Marshal(&coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{
			Spans: []*tracepb.Span{{
				TraceId: bytes.Repeat([]byte{0xab}, 16), SpanId: bytes.Repeat([]byte{0xcd}, 8),
				Name: "claude_code.interaction",
			}},
		}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// A VALID TOKEN IS ACCEPTED AND THE PAYLOAD REACHES THE BACKEND, with the
// backend's own credential added on this side of the hop.
func TestAnExportWithAValidTokenIsForwarded(t *testing.T) {
	t.Parallel()
	var got struct {
		path string
		body string
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		got.path, got.body = r.URL.Path, string(buf)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	receiver := otlpReceiver(t, upstream.URL)
	a := newApp(t, api.Options{OtelReceiver: receiver})

	body := spans(t)
	res := postOTLP(a, "/otlp/"+tokenFor(t, receiver, otlpTrace)+"/v1/traces", body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if got.path != "/v1/traces" || got.body != body {
		t.Errorf("the upstream received %q at %q", got.body, got.path)
	}
	// ANSWERED AS OTLP/HTTP ASKS: the signal's Export response, in the
	// media type the export came in.
	if ct := res.Header.Get("Content-Type"); ct != "application/x-protobuf" {
		t.Errorf("the box was answered as %q", ct)
	}
}

// AN EXPORT THE RECEIVER CANNOT READ IS REFUSED AS THE SPECIFICATION SAYS —
// each answer final, so the exporter drops the batch rather than retrying it:
// 415 for a media type or an encoding the route does not speak, 400 for a
// body that does not decode, 413 for one that inflates past the cap.
func TestAnExportTheReceiverCannotReadIsRefused(t *testing.T) {
	t.Parallel()
	receiver := otlpReceiver(t, "")
	a := newApp(t, api.Options{OtelReceiver: receiver})
	path := "/otlp/" + tokenFor(t, receiver, otlpTrace) + "/v1/traces"

	// Valid protobuf past the bound — a field the message does not know,
	// repeated — so it is its size that refuses it.
	var bomb bytes.Buffer
	zw := gzip.NewWriter(&bomb)
	field := protowire.AppendBytes(protowire.AppendTag(nil, 15, protowire.BytesType), make([]byte, 64<<10))
	for range sandbox.MaxOtelPayload/len(field) + 1 {
		_, _ = zw.Write(field)
	}
	_ = zw.Close()

	for _, tc := range []struct {
		name, body, contentType, encoding string
		want                              int
	}{
		{"a media type OTLP does not define", spans(t), "text/plain", "", http.StatusUnsupportedMediaType},
		{"an encoding the route does not read", spans(t), "application/x-protobuf", "br", http.StatusUnsupportedMediaType},
		{"a body that is not an export", "spans", "application/x-protobuf", "", http.StatusBadRequest},
		{"gzip inflating past the cap", bomb.String(), "application/x-protobuf", "gzip", http.StatusRequestEntityTooLarge},
	} {
		if res := postOTLPAs(a, path, tc.body, tc.contentType, tc.encoding); res.StatusCode != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, res.StatusCode, tc.want)
		}
	}
}

// EVERY WAY A TOKEN CAN BE WRONG IS ONE 401, and the body says nothing:
// forged, malformed and expired are three different facts, and telling the
// caller which one tells an attacker the same.
func TestAnExportWithoutAValidTokenIsRefused(t *testing.T) {
	t.Parallel()
	receiver := otlpReceiver(t, "")
	a := newApp(t, api.Options{OtelReceiver: receiver})
	good := tokenFor(t, receiver, otlpTrace)

	for _, tc := range []struct{ name, token string }{
		{"no token at all", "not-a-token"},
		{"a tampered trace", strings.Replace(good, otlpTrace, strings.Repeat("cd", 16), 1)},
		{"a truncated signature", good[:len(good)-4]},
		// A token minted with somebody else's key. This is the case that
		// matters most: anyone who can reach the endpoint can mint one of
		// their own, and only the signature stops it counting.
		{"another key's token", sandbox.NewOtelTokens(sandbox.OtelTokenOptions{
			Key: []byte("attacker-key")}).Mint(otlpTrace, time.Hour)},
	} {
		res := postOTLP(a, "/otlp/"+tc.token+"/v1/traces", "spans")
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", tc.name, res.StatusCode)
		}
	}
}

// A BOX TOKEN ADMITS TRACES AND NOTHING ELSE. The runners declare a box's
// export traces-only — Claude Code's metrics and logs are switched off, and
// OpenCode is minted no endpoint — so a metrics or logs export under a box
// token comes from something in the box other than its agent; the receiver
// used to forward it with the backend's credential, log records naming no
// trace and metric exemplars naming any. Every other path under a valid token
// is the mux's 404, a final answer an exporter does not retry, and reaches no
// backend.
//
// Mutation: serve `/v1/{signal}` again and the metrics and logs cases reach the
// upstream.
func TestABoxTokenAdmitsTracesAndNothingElse(t *testing.T) {
	t.Parallel()
	var reached []string
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reached = append(reached, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	receiver := otlpReceiver(t, upstream.URL)
	a := newApp(t, api.Options{OtelReceiver: receiver})
	token := tokenFor(t, receiver, otlpTrace)

	for _, signal := range []string{"metrics", "logs", "profiles", "admin", "TRACES"} {
		res := postOTLP(a, "/otlp/"+token+"/v1/"+signal, spans(t))
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("signal %q: status = %d, want 404", signal, res.StatusCode)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reached) != 0 {
		t.Errorf("a non-traces export reached the backend at %v", reached)
	}
}

// THE ROUTE IS EXEMPT FROM THE API'S OWN AUTH, which it has to be: the
// exporter inside the box holds no API token. What replaces it is the token
// in the path — so an export with a valid one works with no Authorization
// header at all, and that is the property, not an oversight.
func TestTheExportRouteNeedsNoAPIToken(t *testing.T) {
	t.Parallel()
	receiver := otlpReceiver(t, "")
	a := newApp(t, api.Options{
		OtelReceiver: receiver,
		// A guarded API: every other route answers 401 without a bearer.
		Bootstrap: closedToReads(),
	})

	res := postOTLP(a, "/otlp/"+tokenFor(t, receiver, otlpTrace)+"/v1/traces", spans(t))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d — a sandbox cannot carry an API token, so this "+
			"route has to authenticate by its own path", res.StatusCode)
	}
	// AND THE GUARD IS REALLY ON, or the assertion above proves nothing.
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agents", nil))
	if rec.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("the API is not guarded in this fixture (/agents = %d), so the "+
			"exemption above was never exercised", rec.Result().StatusCode)
	}
}

// A PAYLOAD PAST THE CAP IS REFUSED. The endpoint needs no other credential
// to reach, so an unbounded read is a box choosing this process's memory.
func TestAnOversizedExportIsRefused(t *testing.T) {
	t.Parallel()
	receiver := otlpReceiver(t, "")
	a := newApp(t, api.Options{OtelReceiver: receiver})

	res := postOTLP(a, "/otlp/"+tokenFor(t, receiver, otlpTrace)+"/v1/traces",
		strings.Repeat("x", (4<<20)+1))
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", res.StatusCode)
	}
}

// NO RECEIVER MEANS NO ROUTE, rather than one answering 503 to everything: an
// endpoint that exists and refuses reads to an operator as broken, while one
// that is not there matches what the config says.
func TestWithNoReceiverTheRouteIsAbsent(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{})
	res := postOTLP(a, "/otlp/any-token/v1/traces", "spans")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
}

// closedToReads is a bootstrap that requires a token for EVERYTHING the
// exemptions do not cover — unlike [guarded], whose anonymous reads would
// make the assertion above pass for the wrong reason.
func closedToReads() *config.Bootstrap {
	b := config.DefaultBootstrap()
	b.API.Auth.AllowAnonymousRead = false
	b.API.Auth.Tokens = []config.APIToken{{ID: "founder", Token: "secret"}}
	return &b
}

// EACH REFUSAL IS THE ANSWER OTLP/HTTP GIVES IT. A receiver at its decoding
// budget is the one refusal that is not the box's fault and that waiting
// fixes, so it is the one answer an exporter retries — a 503 with a
// Retry-After — while the rest are final.
//
// Mutation: answer a busy receiver 400 like a malformed export and the box
// drops a batch nothing was wrong with.
func TestARefusedExportIsAnsweredAsOTLPAsks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err        error
		status     int
		code       string
		retryAfter string
	}{
		{fmt.Errorf("decoding: %w", sandbox.ErrOtelBusy), http.StatusServiceUnavailable, "overloaded", "1"},
		{sandbox.ErrOtelUnsupported, http.StatusUnsupportedMediaType, "invalid_body", ""},
		{sandbox.ErrOtelTooLarge, http.StatusRequestEntityTooLarge, "body_too_large", ""},
		{sandbox.ErrOtelMalformed, http.StatusBadRequest, "invalid_body", ""},
	} {
		rec := httptest.NewRecorder()
		api.RefuseOTLP(rec, tc.err)
		var body struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != tc.status || body.Error != tc.code || rec.Header().Get("Retry-After") != tc.retryAfter {
			t.Errorf("%v: answered %d %q Retry-After %q, want %d %q %q", tc.err, rec.Code, body.Error,
				rec.Header().Get("Retry-After"), tc.status, tc.code, tc.retryAfter)
		}
	}
}

// deadlineRecorder is a recorder that can carry a read deadline, which is
// what http.NewResponseController looks for, and records the one it was set.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	mu       sync.Mutex
	deadline time.Time
}

func (d *deadlineRecorder) SetReadDeadline(t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deadline = t
	return nil
}

// AN EXPORT'S BODY IS READ UNDER A DEADLINE, as every other route's is. The
// route read its body with no bound in time — the server sets none, and each
// route bounds its own read — while a token is a day's credential readable in
// a box: a holder could open connections and send each body a byte at a time,
// holding a handler and a connection slot apiece for as long as it liked.
//
// Mutation: read the body straight off the request and no deadline is set.
func TestAnExportsBodyIsReadUnderADeadline(t *testing.T) {
	t.Parallel()
	receiver := otlpReceiver(t, "")
	a := newApp(t, api.Options{OtelReceiver: receiver})
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest(http.MethodPost,
		"/otlp/"+tokenFor(t, receiver, otlpTrace)+"/v1/traces", strings.NewReader(spans(t)))
	req.Header.Set("Content-Type", "application/x-protobuf")

	before := time.Now()
	a.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	rec.mu.Lock()
	deadline := rec.deadline
	rec.mu.Unlock()
	if deadline.IsZero() {
		t.Fatal("the export's body was read with no deadline")
	}
	if got := deadline.Sub(before); got < httpjson.BodyReadTimeout || got > httpjson.BodyReadTimeout+5*time.Second {
		t.Errorf("the read deadline was set %v out, want %v", got, httpjson.BodyReadTimeout)
	}
}
