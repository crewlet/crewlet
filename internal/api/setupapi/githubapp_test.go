package setupapi_test

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/setup"
)

// THE STATE IS THE ONLY THING GUARDING AN UNAUTHENTICATED WRITE.
//
// GitHub's redirect carries no engine credential, so what stands between the
// callback and anybody who can reach the engine is the state alone. It names
// the seat, it expires, and it is signed: a forged one, a stale one and one
// from another endpoint all have to be refused, and Slack's landing checks
// none of these because it only ever prints a code.
//
// Each case is presented to the callback itself. The company is nil there, so
// a state the flow accepts fails a step LATER with a different error, which is
// what tells "refused" from "accepted".
func TestTheAppCallbackStateIsSignedScopedAndExpiring(t *testing.T) {
	t.Parallel()
	flow := newService(t, setupapi.Options{StateKeys: stateKeys}).AppFlow()
	subject := setupapi.StateSubject("sre-lead", "founder")
	good := stateSigner(t, setupapi.StateDomain, stateKeys, nil).Mint(subject, 15*time.Minute)

	// TAMPERED: changing the seat invalidates it, which is what stops one
	// operator's link writing into another seat.
	parts := strings.Split(good, ".")
	if len(parts) != 5 {
		t.Fatalf("a state of %d fields, where the case looks for the seat in the third", len(parts))
	}
	parts[2] = base64.RawURLEncoding.EncodeToString([]byte(setupapi.StateSubject("other-seat", "founder")))
	tampered := strings.Join(parts, ".")

	for _, tc := range []struct{ name, state string }{
		// FORGED: the signature is checked, so an attacker naming a seat
		// gets nothing.
		{"a forged state", "v1.azE.c3JlLWxlYWQ.9999999999.deadbeef"},
		// FROM ANOTHER ENDPOINT: the domain separates these keys, so a
		// token minted for the telemetry receiver cannot be replayed here
		// although it is signed under the same keyring.
		{"another endpoint's token", stateSigner(t, "crewlet.otlp.v1", stateKeys, nil).
			Mint(subject, 15*time.Minute)},
		// EXPIRED: the state lives fifteen minutes, the span of the task.
		{"an expired state", stateSigner(t, setupapi.StateDomain, stateKeys,
			func() time.Time { return time.Now().Add(-time.Hour) }).Mint(subject, 15*time.Minute)},
		{"a state pointed at another seat", tampered},
	} {
		if _, err := flow.Complete(t.Context(), "code", tc.state); !errors.Is(err, setupapi.ErrStateRefused) {
			t.Errorf("%s: Complete = %v, want %v", tc.name, err, setupapi.ErrStateRefused)
		}
	}
	if _, err := flow.Complete(t.Context(), "code", good); errors.Is(err, setupapi.ErrStateRefused) {
		t.Errorf("a state the fleet signed was refused: %v", err)
	}
}

// A CREATION BEGUN ON ONE NODE FINISHES ON ANOTHER — THROUGH A KEYRING
// ROTATION TOO — or a fleet where the begin and the callback land on
// different nodes refuses every completion, with a message about the link
// rather than about the deployment.
//
// A keyring rotates one restart at a time, so for the length of one the node
// that begins and the node that finishes can hold different rings: the state
// is minted through the begin route on one and presented to the other's
// callback, in both directions of every step a rolling restart puts side by
// side.
func TestAStateBegunOnOneNodeFinishesOnAnotherThroughARotation(t *testing.T) {
	t.Parallel()
	before := keyring("k1", map[string]byte{"k1": 1})
	added := keyring("k1", map[string]byte{"k1": 1, "k2": 2})
	flipped := keyring("k2", map[string]byte{"k1": 1, "k2": 2})
	dropped := keyring("k2", map[string]byte{"k2": 2})
	for _, tc := range []struct {
		name          string
		begin, finish secrets.Keyring
	}{
		{"one keyring", before, before},
		{"begun before the new key was added, finished after", before, added},
		{"begun beside the new key, finished where it is not yet", added, before},
		{"begun before the flip, finished after", added, flipped},
		{"begun after the flip, finished where it has not happened", flipped, added},
		{"begun before the old key was dropped, finished after", flipped, dropped},
		{"begun after the old key was dropped, finished before", dropped, flipped},
	} {
		s := newKeyedSurface(t, tc.begin, nil)
		s.seedGitHubApps(t)
		if res := s.do(t, http.MethodPatch, "/config",
			`{"integrations":{"public_base_url":"https://engine.example.com"}}`,
			map[string]string{"Content-Type": "application/merge-patch+json", "X-Summary": "public url"},
		); res.Code != http.StatusCreated {
			t.Fatalf("%s: set the public url = %d: %s", tc.name, res.Code, res.Body)
		}
		begin := decode(t, s.do(t, http.MethodPost, "/setup/integrations/github/app",
			`{"seat":"sre-lead"}`, nil))
		state, _ := begin["state"].(string)
		if state == "" {
			t.Fatalf("%s: the begin route minted no state: %v", tc.name, begin)
		}
		// On the beginning node's clock, which the surface pins: the
		// two nodes of a fleet agree on the time, and a state read on
		// another day is one that expired.
		finish := newService(t, setupapi.Options{
			StateKeys: tc.finish, Now: func() time.Time { return pinned },
		}).AppFlow()
		if _, err := finish.Complete(t.Context(), "code", state); errors.Is(err, setupapi.ErrStateRefused) {
			t.Errorf("%s: the other node refused the state: %v", tc.name, err)
		}
	}
}

// A NIL CLOCK MUST NOT REACH A REQUEST, and this one reached the worst
// possible request.
//
// Every caller but the tests leaves Options.Now nil, and the service stored
// it unguarded. The GitHub App callback then panicked on its first real use:
// GitHub had already created the app, and the panic landed BEFORE the private
// key was sealed. That key is issued once and never reissued, so the app was
// unrecoverable and had to be deleted at GitHub by hand.
//
// The constructor defaults it now, which fixes every path at once rather than
// the one that happened to be found.
func TestTheServiceAlwaysHasAClock(t *testing.T) {
	t.Parallel()
	s := newService(t, setupapi.Options{
		Company:   func() *config.Company { return &config.Company{} },
		StateKeys: stateKeys,
	})
	// The flow mints a state, which reads the clock. A nil one panics
	// here rather than in a request nobody can retry.
	flow := s.AppFlow()
	if flow == nil {
		t.Fatal("no app flow")
	}
	if got := flow.InstallURL("nobody"); got != "" {
		t.Errorf("a seat that does not exist has an install URL: %q", got)
	}
}

// A STATE IS SPENT THE FIRST TIME IT IS PRESENTED, and refused afterwards.
//
// Validation is a pure signature-and-expiry check, so without this the state
// is a BEARER CREDENTIAL that works as many times as it is presented for the
// whole fifteen minutes it lives — and it is the only authorization on a
// route the webhooks mux serves unauthenticated. It travels in a query
// string, which is exactly where a browser history and an ingress access log
// keep it.
//
// The company is nil here, so a state that survives the spend fails a step
// LATER with a different error. That is what separates the two answers: the
// first call gets past the spend, the second does not.
func TestACallbackStateIsRefusedTheSecondTime(t *testing.T) {
	t.Parallel()
	flow := newService(t, setupapi.Options{StateKeys: stateKeys}).AppFlow()
	state := stateSigner(t, setupapi.StateDomain, stateKeys, nil).Mint(setupapi.StateSubject("sre-lead", "founder"), 15*time.Minute)

	if _, err := flow.Complete(t.Context(), "code-1", state); errors.Is(err, setupapi.ErrStateRefused) {
		t.Fatalf("the first use of a fresh state was refused: %v", err)
	}
	_, err := flow.Complete(t.Context(), "code-2", state)
	if !errors.Is(err, setupapi.ErrStateRefused) {
		t.Errorf("Complete twice = %v, want %v: a replayed link must not seal "+
			"another app over this seat's", err, setupapi.ErrStateRefused)
	}
}

// AND A REGISTRY THAT CANNOT ANSWER REFUSES, rather than assuming unspent.
//
// This inverts the policy [coord.Claims] documents for webhook dedupe, on
// purpose. A push suppressed by a store blip is a wake nobody notices, so
// that caller fails open; this is an authorization check, and a store that
// could not answer is not evidence the link is unused. Failing open here
// would mean an outage re-opened replay for as long as it lasted.
func TestAnUnreadableClaimRegistryRefusesTheCallback(t *testing.T) {
	t.Parallel()
	flow := newService(t, setupapi.Options{
		StateKeys: stateKeys, StateClaims: blindClaims{},
	}).AppFlow()
	state := stateSigner(t, setupapi.StateDomain, stateKeys, nil).Mint(setupapi.StateSubject("sre-lead", "founder"), 15*time.Minute)

	_, err := flow.Complete(t.Context(), "code-1", state)
	if !errors.Is(err, setupapi.ErrStateRefused) {
		t.Errorf("Complete = %v, want %v: an unreadable registry has not said "+
			"this state is unspent", err, setupapi.ErrStateRefused)
	}
}

// blindClaims is a registry that cannot answer.
type blindClaims struct{}

func (blindClaims) Claim(context.Context, string, time.Duration, time.Time) (bool, error) {
	return false, errors.New("the coordination store could not be reached")
}

// A HANDLE THAT BEGINS WITH A DIGIT STILL GETS A REFERENCEABLE NAME.
//
// The org model accepts `7th-engineer`, and the name built for that seat used
// to lead with the handle: `7TH_ENGINEER_GITHUB_APP_KEY`. envref's
// whole-reference grammar requires a leading letter or underscore, so the
// `${VAR}` written beside it resolved to nothing — and the value it pointed at
// was the app's private key, which GitHub issues once and never reissues. The
// app was unusable and unrecoverable from the moment it was created.
func TestASecretNameIsReferenceableForEverySeatHandle(t *testing.T) {
	t.Parallel()
	for _, handle := range []string{
		"7th-engineer", "1", "sre-lead", "_leading", "n0va", "Ops Lead",
	} {
		for _, field := range []string{"APP_KEY", "APP_WEBHOOK_SECRET"} {
			name := setup.SecretNameFor(integration.KindGitHub, setup.Requirement{
				Field: field, Seat: handle,
			})
			if !setup.ValidSecretName(name) {
				t.Errorf("the name for %q/%s is %q, which no ${VAR} can reference",
					handle, field, name)
			}
		}
	}
}

// AND TWO SEATS NEVER SHARE ONE. These are per-seat credentials: one shared
// name would have the second agent's key overwrite the first's, and both
// seats would then authenticate as whichever app was created last.
func TestTwoSeatsDoNotShareASecretName(t *testing.T) {
	t.Parallel()
	first := setup.SecretNameFor(integration.KindGitHub,
		setup.Requirement{Field: "APP_KEY", Seat: "sre-lead"})
	second := setup.SecretNameFor(integration.KindGitHub,
		setup.Requirement{Field: "APP_KEY", Seat: "platform-lead"})
	if first == second {
		t.Errorf("both seats seal their app key under %q", first)
	}
}
