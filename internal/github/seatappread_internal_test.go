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

	"github.com/crewlet/crewlet/internal/integration"
)

// A SLOW GITHUB IS NOT A TORN-DOWN PASS, and only the CONTEXT can tell them
// apart.
//
// [ReconcileSeatApps] abandons the whole roster when this pass is over, and
// leaves one seat exactly as it was when GitHub could not answer. Those are
// opposite reactions to what looks like one condition, and the guard that
// chooses between them tests `ctx.Err() != nil` — never the error a call came
// back with.
//
// The distinction is not theoretical, which is what this file is here to
// keep true. net/http gives a [http.Client.Timeout] a `context.DeadlineExceeded`
// of its own, under a parent context that is perfectly alive, so a guard
// written as `errors.Is(err, context.DeadlineExceeded)` fires on every slow
// GitHub — and the seat below, whose app has never been installed, is then
// silently dropped from the report instead of being told about. The card
// reads Connected over an agent that can do nothing.
//
// # It is an internal test because the timeout has to be real
//
// [AppClient] is built with [ClientTimeout], which is ten seconds: long
// enough that no suite can wait for it. Reaching in to shorten it is the only
// way to produce the ACTUAL error net/http produces rather than a
// hand-written stand-in — and a hand-written one would prove nothing here,
// since the whole claim is about what net/http does.

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
		http: &http.Client{Timeout: slowGitHubTimeout},
		now:  func() time.Time { return time.Now().UTC() },
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

	res, err := ReconcileSeatApps(ctx, SeatAppOptions{
		APIBase: server.URL, WebBase: server.URL, Org: "acme",
		Seats: []SeatApp{{
			Handle: "eng-lead", AppID: 11, Slug: "acme-eng-lead",
			Key: key, Tier: TierReadOnly,
		}},
		Client: func(int64, string, string) (*AppClient, error) { return client, nil },
	})
	if err != nil {
		t.Fatalf("a slow GitHub failed the whole pass: %v — the loop reads that "+
			"as a fault to retry, and every seat this pass could have reported "+
			"on goes unreported while somebody's API is merely slow: %v", err, res)
	}

	// AND THE SEAT IS STILL REPORTED. Nothing has installed this app —
	// that is a fact the COMPANY DOCUMENT holds, not one GitHub has to
	// confirm — so a read that failed changes nothing about it. Under a
	// guard keyed on the error rather than on the context this finding
	// disappears, and the card reports Connected over an agent that sees no
	// repository at all.
	if len(res.Findings) != 1 ||
		res.Findings[0].Kind != integration.FindingApprovalRequired ||
		res.Findings[0].Subject != "eng-lead" {
		t.Fatalf("a seat with no installation recorded was reported as %+v, want "+
			"one approval_required on eng-lead", res.Findings)
	}
	if res.Findings[0].ActionURL == "" {
		t.Error("the finding names no install link, so the person it is owed by " +
			"is told what is wrong and not where to go")
	}
	if len(res.Ready) != 0 {
		t.Errorf("a seat whose installation could not be read was reported ready: %v",
			res.Ready)
	}
}
