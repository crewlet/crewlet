package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A SLOW GITHUB IS NOT A TORN-DOWN PASS, and only the CONTEXT can tell them
// apart.
//
// [AdoptSeatApps] abandons the roster when the pass is over, but leaves one
// seat as it was when GitHub could not answer. The guard tests
// `ctx.Err() != nil`, never the call's error: net/http gives a
// [http.Client.Timeout] a `context.DeadlineExceeded` under a live parent, so
// an `errors.Is` guard would fail every pass over a merely slow GitHub.
//
// It is an internal test because the timeout has to be real: [ClientTimeout]
// is ten seconds, and only shortening it produces the ACTUAL net/http error,
// which a hand-written stand-in cannot prove anything about.

// slowGitHubTimeout bounds the two requests this file makes.
//
// Fifty milliseconds: comfortably longer than a loopback connection takes to
// establish, so the request reaches the handler and times out waiting for an
// answer rather than for a socket, and short enough that both calls together
// cost a tenth of a second. A machine loaded enough to miss that margin
// produces the SAME error from the dial instead, so the value trades only
// fidelity of the scenario, never the outcome.
const slowGitHubTimeout = 50 * time.Millisecond

// appKey is an app private key, in the PEM the company document holds.
//
// A second copy of the external suite's `testKey` only because a package and
// its `_test` package cannot share one: the assertion signing has to happen
// in here, where [AppClient]'s http client is reachable.
func appKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

func TestAClientTimeoutIsNotReadAsATornDownPass(t *testing.T) {
	t.Parallel()
	key := appKey(t)

	// A GITHUB THAT NEVER ANSWERS. The handler holds the request until the
	// client gives up, which is what makes the client's own timeout — not
	// the server, and not this test's context — the thing that fails the
	// call.
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}))
	// LIFO, so the channel is closed BEFORE Close waits for the handlers
	// still parked on it.
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(done) })

	client := &AppClient{
		base: server.URL, appID: 11, pem: key,
		// The SERVER'S OWN transport. This request is parked rather than
		// idle, so it is not the bodyless-response hazard
		// [github.com/crewlet/crewlet/internal/httpx/httpxtest] is about
		// — but a nil Transport is still the process-global pool, and
		// one spelling of the rule is what keeps it true here.
		http: &http.Client{
			Transport: server.Client().Transport, Timeout: slowGitHubTimeout,
		},
		now: func() time.Time { return time.Now().UTC() },
	}

	// THE PREMISE, ASSERTED RATHER THAN ASSUMED. Everything below is about
	// what this package does with an error carrying context.DeadlineExceeded
	// under a live context, and a net/http that stopped producing one would
	// leave the rest of this test passing over a condition it no longer
	// constructs.
	ctx := t.Context()
	if _, err := client.Installations(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a client timeout no longer carries context.DeadlineExceeded "+
			"(%v), so this test constructs nothing and the guard it protects "+
			"needs re-reading against whatever net/http does now", err)
	}
	if ctx.Err() != nil {
		t.Fatal("the pass's own context died, so this is the cancellation case " +
			"rather than the timeout one")
	}

	// A TIMEOUT IS NOT EVIDENCE: nothing is recorded or forgotten on it.
	write := func(what string) {
		t.Errorf("a read GitHub never answered was taken as evidence: %s", what)
	}
	err := AdoptSeatApps(ctx, SeatAppOptions{
		APIBase: server.URL, WebBase: server.URL, Org: "acme",
		Seats:  []SeatApp{{Handle: "eng-lead", AppID: 11, Slug: "acme-eng-lead", Key: key}},
		Client: func(int64, string, string) (*AppClient, error) { return client, nil },
		Record: func(context.Context, string, int64) error { write("record"); return nil },
		Forget: func(context.Context, string) error { write("forget"); return nil },
	})
	if err != nil {
		t.Fatalf("a slow GitHub failed the whole pass: %v; the loop reads that "+
			"as a fault to retry, and every seat's record stays stale while "+
			"somebody's API is merely slow", err)
	}
}
