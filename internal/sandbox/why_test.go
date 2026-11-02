package sandbox_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"

	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// What these cases protect: every failure a launch can meet is CLASSED where it
// is met, by what clears it, so whatever tells the failure, to a model or a
// person, can say whether a wait clears it or a person's change does, without
// reading the error's text, which is the operator's. A failure that lost its
// class reads as a fault of the node; one that took the wrong class sends its
// reader to retry a key nobody has fixed, or to give up on an outage that
// clears in a minute.

// e2bAnswering is a provider whose control plane answers every request with
// status, or — when status is zero — whose server is gone before it is asked.
func e2bAnswering(t *testing.T, status int) *sandbox.E2BProvider {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message": "the provider's own words"}`))
	}))
	t.Cleanup(server.Close)
	provider, err := sandbox.NewE2B(sandbox.E2BOptions{
		APIKey: "k", Domain: "test.invalid",
		HTTP: &http.Client{Transport: httpxtest.Rewrite(t, server)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if status == 0 {
		server.Close()
	}
	return provider
}

// A CREATE E2B REFUSES IS CLASSED BY ITS STATUS: the company's key refused, the
// account unpaid or at a limit, E2B failing on its side, or the template the
// request names — and a status that says none of those is left unclassed
// rather than filed under the nearest. An unpaid account is not a limit: no
// wait clears it, so it is a class of its own that offers none.
//
// Mutation: drop the status table, or the create's template arm, and a row
// goes red.
func TestAnE2BCreateIsClassedByWhatClearsIt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status int
		want   sandbox.Why
	}{
		{http.StatusUnauthorized, sandbox.WhyProviderRefused},
		{http.StatusForbidden, sandbox.WhyProviderRefused},
		{http.StatusPaymentRequired, sandbox.WhyProviderBilling},
		{http.StatusTooManyRequests, sandbox.WhyProviderLimit},
		{http.StatusRequestTimeout, sandbox.WhyProviderUnavailable},
		{http.StatusInternalServerError, sandbox.WhyProviderUnavailable},
		{http.StatusServiceUnavailable, sandbox.WhyProviderUnavailable},
		{http.StatusBadRequest, sandbox.WhyConfiguration},
		{http.StatusNotFound, sandbox.WhyConfiguration},
		{http.StatusUnprocessableEntity, sandbox.WhyConfiguration},
		{http.StatusConflict, ""},
		// No answer at all: E2B not answering.
		{0, sandbox.WhyProviderUnavailable},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			t.Parallel()
			_, err := e2bAnswering(t, tc.status).Create(context.Background(), sandbox.Spec{})
			if err == nil {
				t.Fatal("a refused create handed out a box")
			}
			why, ok := sandbox.WhyOf(err)
			if tc.want == "" {
				if ok {
					t.Errorf("a %d is classed %q, which it says nothing about", tc.status, why)
				}
				return
			}
			if !ok || why != tc.want {
				t.Errorf("a %d is classed %q (%v), want %q: %v", tc.status, why, ok, tc.want, err)
			}
			// THE MARK COSTS THE ERROR NOTHING: the operator's log line
			// still carries the provider's own account.
			var refused *sandbox.E2BError
			if tc.status != 0 && !errors.As(err, &refused) {
				t.Errorf("the E2B refusal is gone from the chain: %v", err)
			}
		})
	}
}

// A REQUEST THAT REACHED NO HOST IS CLASSED BY WHY: a domain that doesn't
// resolve, or a certificate that doesn't match it, is the deployment's
// providers.sandbox.e2b domain, which no wait changes; a timeout or a reset is
// E2B, or the way to it, not answering, which clears when it does. Every one
// was E2B not answering, so a mistyped domain read as an outage that would
// clear by itself.
//
// Mutation: class every transport failure unavailable and the first two rows
// fail.
func TestAnE2BRequestThatReachedNoHostIsClassedByWhy(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err  error
		want sandbox.Why
	}{
		"a domain that doesn't resolve": {&net.DNSError{Err: "no such host",
			Name: "api.e2b.example.com", IsNotFound: true}, sandbox.WhyConfiguration},
		"a certificate that doesn't match it": {&tls.CertificateVerificationError{
			Err: x509.UnknownAuthorityError{}}, sandbox.WhyConfiguration},
		"a resolver that didn't answer": {&net.DNSError{Err: "i/o timeout",
			Name: "api.e2b.example.com", IsTimeout: true}, sandbox.WhyProviderUnavailable},
		"a reset": {syscall.ECONNRESET, sandbox.WhyProviderUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			provider, err := sandbox.NewE2B(sandbox.E2BOptions{
				APIKey: "k", Domain: "e2b.example.com",
				HTTP: &http.Client{Transport: failingTransport{tc.err}},
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = provider.Create(context.Background(), sandbox.Spec{})
			if why, ok := sandbox.WhyOf(err); !ok || why != tc.want {
				t.Errorf("%v is classed %q (%v), want %q", err, why, ok, tc.want)
			}
		})
	}
}

// A REQUEST ITS CALLER CANCELLED IS NO CLASS: a drain, a lost lease or a turn
// stopped cancels it, which says nothing about E2B, and marked it was told as
// the provider not answering. A request its own bound ended while the caller
// still waits (an idle watchdog cancelling the exchange it guards) is still
// E2B not answering.
//
// Mutation: mark a transport failure whatever the caller's context says, and
// the first half goes red; leave every cancellation unmarked, and the second.
func TestAnE2BRequestItsCallerCancelledIsNoClass(t *testing.T) {
	t.Parallel()
	provider, err := sandbox.NewE2B(sandbox.E2BOptions{
		APIKey: "k", Domain: "e2b.example.com",
		HTTP: &http.Client{Transport: failingTransport{context.Canceled}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = provider.Create(cancelled, sandbox.Spec{})
	if why, ok := sandbox.WhyOf(err); ok || !errors.Is(err, context.Canceled) {
		t.Errorf("a create its caller cancelled = %v, classed %q (%v); want the "+
			"cancellation, unclassed", err, why, ok)
	}
	_, err = provider.Create(t.Context(), sandbox.Spec{})
	if why, ok := sandbox.WhyOf(err); !ok || why != sandbox.WhyProviderUnavailable {
		t.Errorf("a create its own bound ended = %v, classed %q (%v); want %q", err, why,
			ok, sandbox.WhyProviderUnavailable)
	}
}

// failingTransport fails every request with err, before any host answers.
type failingTransport struct{ err error }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, f.err
}

// A BOX THAT IS GONE IS NOT A CLASS: it is its own sentinel, settled at once,
// and the 404 that found it is not a template E2B does not have.
func TestAGoneBoxIsNoLaunchClass(t *testing.T) {
	t.Parallel()
	_, err := e2bAnswering(t, http.StatusNotFound).Connect(context.Background(), "sbx-gone")
	if !errors.Is(err, sandbox.ErrBoxGone) {
		t.Fatalf("connecting to a vanished box = %v; want ErrBoxGone", err)
	}
	if why, ok := sandbox.WhyOf(err); ok {
		t.Errorf("a gone box is classed %q", why)
	}
}

// A CREATE THAT NAMED NO BOX is E2B answering wrong, which clears when it
// answers right: nothing a person changes in the company's configuration.
func TestACreateNamingNoBoxIsTheProviders(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"clientID": "cl1"}`))
	}))
	t.Cleanup(server.Close)
	provider, err := sandbox.NewE2B(sandbox.E2BOptions{
		APIKey: "k", Domain: "test.invalid",
		HTTP: &http.Client{Transport: httpxtest.Rewrite(t, server)},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Create(context.Background(), sandbox.Spec{})
	if why, ok := sandbox.WhyOf(err); !ok || why != sandbox.WhyProviderUnavailable {
		t.Errorf("a create naming no box is classed %q (%v), want %q", why, ok,
			sandbox.WhyProviderUnavailable)
	}
}

// A BOX'S OWN AGENT IS CLASSED AS THE PROVIDER IS, except where it refuses the
// box's own access token: that credential was handed to the engine at the
// create, so no person's key is wrong, and it is left for the caller to read as
// the fault it is.
func TestABoxsAgentIsClassedByWhatClearsIt(t *testing.T) {
	t.Parallel()
	for status, want := range map[int]sandbox.Why{
		http.StatusInternalServerError: sandbox.WhyProviderUnavailable,
		http.StatusBadGateway:          sandbox.WhyProviderUnavailable,
		http.StatusTooManyRequests:     sandbox.WhyProviderLimit,
		http.StatusUnauthorized:        "",
	} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Parallel()
			stub := newE2BStub()
			stub.startStatus = status
			provider, _ := newE2B(t, stub)
			box, err := provider.Create(context.Background(), sandbox.Spec{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = box.Exec(context.Background(), "true", sandbox.ExecOptions{})
			why, ok := sandbox.WhyOf(err)
			if want == "" {
				if ok {
					t.Errorf("envd's %d is classed %q", status, why)
				}
				return
			}
			if !ok || why != want {
				t.Errorf("envd's %d is classed %q (%v), want %q", status, why, ok, want)
			}
		})
	}

	// A COMMAND WHOSE BOX WENT AWAY mid-stream is the provider's too.
	stub := newE2BStub()
	stub.frames = []any{startEvent(9), dataEvent("half a log", "")}
	provider, _ := newE2B(t, stub)
	box, err := provider.Create(context.Background(), sandbox.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = box.Exec(context.Background(), "make", sandbox.ExecOptions{})
	if why, ok := sandbox.WhyOf(err); !ok || why != sandbox.WhyProviderUnavailable {
		t.Errorf("a box that went away mid-command is classed %q (%v)", why, ok)
	}
}

// A SETUP STEP IS THE COMPANY'S RECIPE WHERE A COMMAND OF IT EXITED, and the
// box's where the box never ran it to an exit: the recipe is a person's to
// change, a box that failed under it is not. The one status that is not the
// recipe's to change is 75, its own word that a download it makes failed for
// now, which a wait clears; its own timeout's 124 is still the recipe's. The
// command's account is never read: every step here prints the same 503.
//
// Mutation: class every [sandbox.SetupError] as configuration, or drop its
// Unwrap, or class 75 as configuration or 124 as temporary, and a case goes
// red.
func TestASetupFailureIsClassedByWhoseItIs(t *testing.T) {
	t.Parallel()
	steps := []sandbox.SetupStep{{Name: "deps", Commands: []string{"npm ci"}}}

	for status, want := range map[int]sandbox.Why{
		3:   sandbox.WhyConfiguration,
		124: sandbox.WhyConfiguration,
		75:  sandbox.WhySetupUnavailable,
	} {
		exited := sandbox.NewFakeSandbox("b1")
		exited.ExecFunc = func(context.Context, string, sandbox.ExecOptions) (sandbox.ExecResult, error) {
			return sandbox.ExecResult{ExitCode: status, Stderr: "npm error code E503"}, nil
		}
		err := sandbox.ApplySetup(context.Background(), exited, steps, nil)
		if why, ok := sandbox.WhyOf(err); !ok || why != want {
			t.Errorf("a step that exited %d is classed %q (%v), want %q", status, why, ok, want)
		}
	}

	gone := sandbox.NewFakeSandbox("b2")
	gone.ExecFunc = func(context.Context, string, sandbox.ExecOptions) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{}, sandbox.Because(sandbox.WhyProviderUnavailable,
			errors.New("e2b: start: dial tcp: connection refused"))
	}
	err := sandbox.ApplySetup(context.Background(), gone, steps, nil)
	if why, ok := sandbox.WhyOf(err); !ok || why != sandbox.WhyProviderUnavailable {
		t.Errorf("a step the box never ran is classed %q (%v), want the box's own %q", why, ok,
			sandbox.WhyProviderUnavailable)
	}
	var setup *sandbox.SetupError
	if !errors.As(err, &setup) || setup.Exited {
		t.Errorf("the failure lost its step: %v", err)
	}

	broken := sandbox.NewFakeSandbox("b3")
	broken.ExecFunc = func(context.Context, string, sandbox.ExecOptions) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{}, errors.New("fork: resource temporarily unavailable")
	}
	err = sandbox.ApplySetup(context.Background(), broken, steps, nil)
	if why, ok := sandbox.WhyOf(err); ok {
		t.Errorf("a step the host could not run is classed %q, a failure nothing classed", why)
	}
}

// A CELL OR A CODING AGENT THE CATALOGUE DOES NOT HAVE is configuration: a
// person's providers.sandbox, which no wait changes.
func TestACatalogueGapIsConfiguration(t *testing.T) {
	t.Parallel()
	m, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers:        map[sandbox.Placement]sandbox.Provider{sandbox.E2B: sandbox.NewFakeProvider()},
		DefaultPlacement: sandbox.E2B,
		Runners:          map[string]sandbox.Runner{"claude-code": sandbox.NewFakeRunner("claude-code")},
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, spec := range map[string]sandbox.Spec{
		"a cell nobody configured": {Placement: sandbox.Direct, CodingAgent: "claude-code"},
		"a runner nobody has":      {Placement: sandbox.E2B, CodingAgent: "unheard-of"},
	} {
		_, _, err := m.Acquire(context.Background(), spec, nil)
		if why, ok := sandbox.WhyOf(err); !ok || why != sandbox.WhyConfiguration {
			t.Errorf("%s is classed %q (%v), want %q: %v", name, why, ok, sandbox.WhyConfiguration, err)
		}
	}
}

// THE CLASS TABLE IS WHOLE: every class is valid, says whether a wait clears
// it, and an unknown one is not a class.
func TestEveryClassIsKnown(t *testing.T) {
	t.Parallel()
	waits := map[sandbox.Why]bool{
		sandbox.WhyConfiguration: false, sandbox.WhySetupUnavailable: true,
		sandbox.WhyProviderRefused: false,
		sandbox.WhyProviderBilling: false, sandbox.WhyProviderLimit: true,
		sandbox.WhyProviderUnavailable: true,
	}
	if len(sandbox.Whys) != len(waits) {
		t.Fatalf("%d classes declared, %d judged here", len(sandbox.Whys), len(waits))
	}
	for _, why := range sandbox.Whys {
		if !why.Valid() {
			t.Errorf("%q is declared and not valid", why)
		}
		if why.Waits() != waits[why] {
			t.Errorf("%q waits %t, want %t", why, why.Waits(), waits[why])
		}
	}
	if sandbox.Why("from_a_newer_build").Valid() {
		t.Error("an unknown class is valid")
	}
	if why, ok := sandbox.WhyOf(errors.New("plain")); ok {
		t.Errorf("an unmarked error is classed %q", why)
	}
	if sandbox.Because(sandbox.WhyConfiguration, nil) != nil {
		t.Error("marking no error made one")
	}
}
