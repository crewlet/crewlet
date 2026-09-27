package oidc_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/iam/oidc"
)

// THE TOKEN ENDPOINT SEES AT MOST EIGHT REQUESTS FROM A NODE, AND NOBODY WHO
// WAITS IS REFUSED.
//
// A sign-in wave — or a burst an unauthenticated caller makes by starting
// flights and calling back, each costing an exchange — would otherwise put as
// many requests on the provider at once as there are callbacks in flight. The
// slots hold it at [oidc.ExchangeSlots] per provider, and a caller beyond them
// WAITS on its own context rather than being turned away, so every exchange
// here lands. The probe's refresh takes the same slots: with eight exchanges
// parked, it reaches the endpoint only when one of them is done.
//
// Mutation: drop the slot and the endpoint sees every caller at once; give the
// refresh a path of its own and it arrives beside the eight.
func TestThePerIssuerSlotsRefuseNothingLegitimate(t *testing.T) {
	t.Parallel()
	var inFlight, peak atomic.Int64
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			seen := peak.Load()
			if now <= seen || peak.CompareAndSwap(seen, now) {
				break
			}
		}
		<-release
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id_token": "not-verified-here", "refresh_token": "rotated",
		})
	}))
	t.Cleanup(server.Close)
	provider := oidc.NewProvider(testConfig(), server.Client(),
		func() time.Time { return at })

	const callers = 3 * oidc.ExchangeSlots
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = provider.Exchange(t.Context(), server.URL, "a-code", "a-verifier")
		}()
	}
	waitFor(t, func() bool { return inFlight.Load() == oidc.ExchangeSlots })
	refreshed := make(chan error, 1)
	go func() {
		_, err := provider.Refresh(t.Context(), server.URL, "a-refresh-token")
		refreshed <- err
	}()
	// ANYTHING PAST THE SLOTS WOULD HAVE ARRIVED BY NOW.
	time.Sleep(100 * time.Millisecond)
	if got := inFlight.Load(); got != oidc.ExchangeSlots {
		t.Errorf("the endpoint holds %d requests with every slot taken, want %d",
			got, oidc.ExchangeSlots)
	}
	close(release)
	wg.Wait()
	if err := <-refreshed; err != nil {
		t.Errorf("the probe's refresh was refused: %v", err)
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("exchange %d was refused rather than waiting its turn: %v", i, err)
		}
	}
	if got := peak.Load(); got != oidc.ExchangeSlots {
		t.Errorf("the endpoint saw %d requests at once, want at most (and, "+
			"under this load, exactly) %d", got, oidc.ExchangeSlots)
	}
}

// A CALLER WHOSE REQUEST ENDS WHILE IT WAITS HAS ASKED THE PROVIDER NOTHING.
func TestACallerThatGivesUpWaitingAsksNothing(t *testing.T) {
	t.Parallel()
	var served atomic.Int64
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		<-release
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": "x"})
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	provider := oidc.NewProvider(testConfig(), server.Client(),
		func() time.Time { return at })
	for range oidc.ExchangeSlots {
		go func() { _, _ = provider.Exchange(t.Context(), server.URL, "c", "v") }()
	}
	waitFor(t, func() bool { return served.Load() == oidc.ExchangeSlots })

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := provider.Exchange(ctx, server.URL, "c", "v"); err == nil {
		t.Fatal("an exchange whose request ended while it waited was answered")
	}
	if got := served.Load(); got != oidc.ExchangeSlots {
		t.Errorf("the endpoint was asked %d times, want the %d holding the slots",
			got, oidc.ExchangeSlots)
	}
}

// THE PROVIDER'S CONNECTION CAP NEVER MAKES AN ADMITTED REQUEST WAIT.
//
// internal/httpx caps the sockets this node holds to the identity provider,
// and a request waiting there for one counts its wait against the client's own
// timeout — so a cap below the slots plus the two single-flighted fetches
// beside them (the discovery document and the key set) would fail a sign-in
// the slots had admitted. The two numbers live in two packages; this holds
// them together.
func TestTheProvidersConnectionCapCoversItsSlots(t *testing.T) {
	t.Parallel()
	if httpx.IdentityProviderConns < oidc.ExchangeSlots+2 {
		t.Errorf("httpx.IdentityProviderConns = %d, below the %d slots and the "+
			"two fetches beside them", httpx.IdentityProviderConns, oidc.ExchangeSlots)
	}
}

// waitFor polls a condition for up to five seconds.
func waitFor(t *testing.T, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatal("the condition was never reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
