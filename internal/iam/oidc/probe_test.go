package oidc_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam/oidc"
)

// sessionStore is the probe's seam, recording what it was asked to do.
type sessionStore struct {
	mu      sync.Mutex
	live    []oidc.LiveSession
	ended   map[string]string
	rotated map[string]string
	listErr error
	skipped []error
	endErr  error
}

func newSessions(live ...oidc.LiveSession) *sessionStore {
	return &sessionStore{
		live: live, ended: map[string]string{}, rotated: map[string]string{},
	}
}

func (s *sessionStore) LiveOIDC(context.Context) (oidc.Listing, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return oidc.Listing{Sessions: s.live, Skipped: s.skipped}, s.listErr
}

func (s *sessionStore) End(_ context.Context, session oidc.LiveSession, reason string) error {
	lineage := session.Lineage
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.endErr != nil {
		return s.endErr
	}
	s.ended[lineage] = reason
	return nil
}

func (s *sessionStore) Rotated(_ context.Context, lineage, refresh string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rotated[lineage] = refresh
	return nil
}

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// THE PROBE ENDS A SESSION THE PROVIDER NO LONGER RECOGNISES.
//
// A provider that suspends or deletes an account tells nobody. Every other
// revocation in this engine is a write somebody makes here and is felt within
// an applier's lag; a central deactivation is felt only when the absolute
// session lifetime runs out, which for a company that sets that generously is
// the rest of the week.
func TestTheProbeEndsASessionAsIdpRevokedOnInvalidGrant(t *testing.T) {
	t.Parallel()
	idp := newIssuer(t)
	idp.refusal = "invalid_grant"
	sessions := newSessions(oidc.LiveSession{
		Lineage: "lin-1", Person: "p-1", Refresh: "refresh-1",
	})
	prober := oidc.NewProber(
		oidc.NewProvider(idp.config(), idp.Client(), func() time.Time { return at }),
		sessions, quiet())

	pass, err := prober.Run(t.Context())
	checked, ended := pass.Checked, pass.Ended
	if err != nil {
		t.Fatalf("the pass failed: %v", err)
	}
	if checked != 1 || ended != 1 {
		t.Fatalf("the pass checked %d and ended %d, want 1 and 1", checked, ended)
	}
	if sessions.ended["lin-1"] != oidc.ReasonIdPRevoked {
		t.Errorf("the session ended as %q, want %q", sessions.ended["lin-1"],
			oidc.ReasonIdPRevoked)
	}
}

// AN UNREACHABLE PROVIDER IS NOT A DEACTIVATION.
//
// THE THREE-VALUED RULE, on the surface where getting it wrong is loudest:
// treating "could not ask" as "the account is gone" signs the entire company
// out the first time somebody else's service has an outage.
func TestAnUnreachableProviderEndsNothing(t *testing.T) {
	t.Parallel()
	idp := newIssuer(t)
	// Every other error a provider can answer with.
	for _, refusal := range []string{
		"temporarily_unavailable", "slow_down", "server_error", "invalid_client",
	} {
		t.Run(refusal, func(t *testing.T) {
			idp.refusal = refusal
			sessions := newSessions(oidc.LiveSession{
				Lineage: "lin-1", Person: "p-1", Refresh: "refresh-1",
			})
			prober := oidc.NewProber(
				oidc.NewProvider(idp.config(), idp.Client(), func() time.Time { return at }),
				sessions, quiet())
			pass, err := prober.Run(t.Context())
			checked, ended := pass.Checked, pass.Ended
			if err != nil {
				t.Fatalf("the pass failed: %v", err)
			}
			if checked != 1 {
				t.Errorf("the pass checked %d sessions", checked)
			}
			if ended != 0 || len(sessions.ended) != 0 {
				t.Errorf("a provider answering %q ended %d sessions — an "+
					"outage at the provider would sign the company out",
					refusal, ended)
			}
		})
	}
}

// A PROVIDER THAT ROTATES THE REFRESH TOKEN HAS INVALIDATED THE OLD ONE.
//
// Failing to record the new one makes the NEXT probe present a token the
// provider has already retired — which answers `invalid_grant`, which this
// engine reads as a deactivation. The session of somebody perfectly employed
// ends, one interval later, for no reason anybody can find.
func TestARotatedRefreshTokenIsRecorded(t *testing.T) {
	t.Parallel()
	idp := newIssuer(t)
	idp.rotate = "refresh-2"
	sessions := newSessions(oidc.LiveSession{
		Lineage: "lin-1", Person: "p-1", Refresh: "refresh-1",
	})
	prober := oidc.NewProber(
		oidc.NewProvider(idp.config(), idp.Client(), func() time.Time { return at }),
		sessions, quiet())

	if pass, err := prober.Run(t.Context()); err != nil || pass.Ended != 0 {
		t.Fatalf("the pass ended %d sessions: %v", pass.Ended, err)
	}
	if sessions.rotated["lin-1"] != "refresh-2" {
		t.Errorf("the rotated token reads %q, want %q — the next probe would "+
			"present a token the provider has already retired, read the "+
			"invalid_grant as a deactivation, and end the session of "+
			"somebody perfectly employed", sessions.rotated["lin-1"], "refresh-2")
	}
}

// A SESSION WITH NO REFRESH TOKEN IS NOT PROBED, AND IS NOT ENDED EITHER.
//
// A provider that never granted `offline_access` gives this nothing to ask
// with. Ending those sessions would make the absence of a scope a lockout, and
// counting them as checked would report a probe that runs against nothing.
func TestASessionWithNoRefreshTokenIsSkipped(t *testing.T) {
	t.Parallel()
	idp := newIssuer(t)
	idp.refusal = "invalid_grant"
	sessions := newSessions(
		oidc.LiveSession{Lineage: "lin-1", Person: "p-1"},
		oidc.LiveSession{Lineage: "lin-2", Person: "p-2", Refresh: "refresh-2"},
	)
	prober := oidc.NewProber(
		oidc.NewProvider(idp.config(), idp.Client(), func() time.Time { return at }),
		sessions, quiet())

	pass, err := prober.Run(t.Context())
	checked, ended := pass.Checked, pass.Ended
	if err != nil {
		t.Fatalf("the pass failed: %v", err)
	}
	if checked != 1 || ended != 1 {
		t.Errorf("the pass checked %d and ended %d, want 1 and 1", checked, ended)
	}
	if _, gone := sessions.ended["lin-1"]; gone {
		t.Error("a session with no refresh token was ended, so a provider " +
			"that does not grant offline_access would be a lockout")
	}
}

// ONE SESSION'S FAILURE DOES NOT STOP THE PASS.
//
// The sessions are independent, and a rate limit part-way through would
// otherwise mean every session after it in the list is never checked at all —
// silently, and in the same order every interval, so the same people are never
// checked.
func TestOneSessionsFailureDoesNotStopThePass(t *testing.T) {
	t.Parallel()
	idp := newIssuer(t)
	idp.refusal = "invalid_grant"
	sessions := newSessions(
		oidc.LiveSession{Lineage: "lin-1", Person: "p-1", Refresh: "r1"},
		oidc.LiveSession{Lineage: "lin-2", Person: "p-2", Refresh: "r2"},
		oidc.LiveSession{Lineage: "lin-3", Person: "p-3", Refresh: "r3"},
	)
	sessions.endErr = errors.New("the log is unavailable")
	prober := oidc.NewProber(
		oidc.NewProvider(idp.config(), idp.Client(), func() time.Time { return at }),
		sessions, quiet())

	pass, err := prober.Run(t.Context())
	checked, ended := pass.Checked, pass.Ended
	if err != nil {
		t.Fatalf("the pass failed outright: %v", err)
	}
	if checked != 3 {
		t.Errorf("the pass checked %d sessions, want 3 — one failure stopped "+
			"it, so every session after the first is never checked", checked)
	}
	if ended != 0 {
		t.Errorf("%d sessions were counted as ended although every write failed", ended)
	}
	if pass.Failed != 3 {
		t.Errorf("the pass counted %d failed closes, want 3 — a deactivation "+
			"that did not land is something an operator is told about", pass.Failed)
	}
}

// A SESSION THE LISTING COULD NOT HAND OVER DOES NOT STOP THE PASS.
//
// Each session's refresh token is its own row, so one that will not read — a
// newer peer's grant during a rolling upgrade, a store blip on one key — says
// nothing about the rest. The probe used to discard the whole listing whenever
// it carried an error, so one unreadable grant meant nobody in the company was
// asked about for as long as it stood, and everybody the provider had disabled
// kept their session. The listing's own error is still the whole-failure arm:
// a pass that could list nothing ends nothing.
func TestASkippedSessionDoesNotStopThePass(t *testing.T) {
	t.Parallel()
	idp := newIssuer(t)
	idp.refusal = "invalid_grant"
	sessions := newSessions(oidc.LiveSession{
		Lineage: "lin-1", Person: "p-1", Refresh: "refresh-1",
	})
	sessions.skipped = []error{
		errors.New("session lin-2's grant is a version this build cannot read"),
	}
	prober := oidc.NewProber(
		oidc.NewProvider(idp.config(), idp.Client(), func() time.Time { return at }),
		sessions, quiet())

	pass, err := prober.Run(t.Context())
	if err != nil {
		t.Fatalf("one unreadable grant failed the whole pass: %v", err)
	}
	if pass.Checked != 1 || pass.Ended != 1 {
		t.Errorf("the pass checked %d and ended %d, want 1 and 1 — the session "+
			"beside an unreadable grant was never asked about", pass.Checked,
			pass.Ended)
	}
	if pass.Skipped != 1 {
		t.Errorf("the pass counted %d skipped sessions, want 1", pass.Skipped)
	}
	if sessions.ended["lin-1"] != oidc.ReasonIdPRevoked {
		t.Errorf("the deactivated session ended as %q, want %q",
			sessions.ended["lin-1"], oidc.ReasonIdPRevoked)
	}

	// THE WHOLE-FAILURE ARM: a listing that failed outright ends nothing.
	sessions = newSessions(oidc.LiveSession{
		Lineage: "lin-1", Person: "p-1", Refresh: "refresh-1",
	})
	sessions.listErr = errors.New("the directory is unreadable")
	prober = oidc.NewProber(
		oidc.NewProvider(idp.config(), idp.Client(), func() time.Time { return at }),
		sessions, quiet())
	if pass, err := prober.Run(t.Context()); err == nil || pass.Ended != 0 {
		t.Errorf("a listing that failed whole answered %+v and %v, want nothing "+
			"ended and the error", pass, err)
	}
}

// THE INTERVAL IS THE CONFIGURED ONE, AND ZERO TAKES THE DEFAULT.
func TestTheProbeIntervalIsConfiguredOrTheDefault(t *testing.T) {
	t.Parallel()
	idp := newIssuer(t)
	config := idp.config()
	prober := oidc.NewProber(
		oidc.NewProvider(config, idp.Client(), func() time.Time { return at }), nil, quiet())
	if got := prober.Interval(); got != oidc.DefaultDeactivationProbe {
		t.Errorf("the default interval is %s, want %s", got,
			oidc.DefaultDeactivationProbe)
	}
	config.DeactivationProbe = 15 * time.Minute
	prober = oidc.NewProber(
		oidc.NewProvider(config, idp.Client(), func() time.Time { return at }), nil, quiet())
	if got := prober.Interval(); got != 15*time.Minute {
		t.Errorf("the configured interval reads %s", got)
	}
}
