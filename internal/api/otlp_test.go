package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/config"
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

// otlpReceiver builds a receiver whose tokens a test can mint.
func otlpReceiver(t *testing.T, upstream string) *sandbox.OtelReceiver {
	t.Helper()
	receiver, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
		BaseURL:          "https://engine.internal",
		Tokens:           sandbox.NewOtelTokens(sandbox.OtelTokenOptions{Key: []byte("test-key")}),
		UpstreamEndpoint: upstream,
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
	endpoint := receiver.RunEnv(traceID, "span", "turn-1", "engineer", time.Hour)
	token := strings.TrimPrefix(
		endpoint["OTEL_EXPORTER_OTLP_ENDPOINT"], "https://engine.internal/otlp/")
	if token == "" {
		t.Fatal("the run got no endpoint")
	}
	return token
}

func postOTLP(a *api.App, path string, body string) *http.Response {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-protobuf")
	a.ServeHTTP(rec, req)
	return rec.Result()
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
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		got.path, got.body = r.URL.Path, string(buf)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	receiver := otlpReceiver(t, upstream.URL)
	a := newApp(t, api.Options{OtelReceiver: receiver})

	res := postOTLP(a, "/otlp/"+tokenFor(t, receiver, "trace-abc")+"/v1/traces", "spans")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if got.path != "/v1/traces" || got.body != "spans" {
		t.Errorf("the upstream received %q at %q", got.body, got.path)
	}
}

// EVERY WAY A TOKEN CAN BE WRONG IS ONE 401, and the body says nothing:
// forged, malformed and expired are three different facts, and telling the
// caller which one tells an attacker the same.
func TestAnExportWithoutAValidTokenIsRefused(t *testing.T) {
	t.Parallel()
	receiver := otlpReceiver(t, "")
	a := newApp(t, api.Options{OtelReceiver: receiver})
	good := tokenFor(t, receiver, "trace-abc")

	for _, tc := range []struct{ name, token string }{
		{"no token at all", "not-a-token"},
		{"a tampered trace", strings.Replace(good, "trace-abc", "trace-xyz", 1)},
		{"a truncated signature", good[:len(good)-4]},
		// A token minted with somebody else's key. This is the case that
		// matters most: anyone who can reach the endpoint can mint one of
		// their own, and only the signature stops it counting.
		{"another key's token", sandbox.NewOtelTokens(sandbox.OtelTokenOptions{
			Key: []byte("attacker-key")}).Mint("trace-abc", time.Hour)},
	} {
		res := postOTLP(a, "/otlp/"+tc.token+"/v1/traces", "spans")
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", tc.name, res.StatusCode)
		}
	}
}

// AN UNKNOWN SIGNAL IS REFUSED, because the segment is concatenated into the
// upstream URL — an unchecked one lets a caller choose part of the address
// the engine's own credential is sent to.
func TestAnUnknownSignalIsRefused(t *testing.T) {
	t.Parallel()
	receiver := otlpReceiver(t, "")
	a := newApp(t, api.Options{OtelReceiver: receiver})
	token := tokenFor(t, receiver, "trace-abc")

	for _, signal := range []string{"profiles", "admin"} {
		res := postOTLP(a, "/otlp/"+token+"/v1/"+signal, "x")
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("signal %q: status = %d, want 404", signal, res.StatusCode)
		}
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

	res := postOTLP(a, "/otlp/"+tokenFor(t, receiver, "trace-abc")+"/v1/traces", "spans")
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

	res := postOTLP(a, "/otlp/"+tokenFor(t, receiver, "trace-abc")+"/v1/traces",
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
