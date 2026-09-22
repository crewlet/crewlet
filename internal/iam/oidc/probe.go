package oidc

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// THE DEACTIVATION PROBE: how an off-boarding at the identity provider reaches
// this engine.
//
// A provider that suspends or deletes an account tells nobody. Every other
// revocation path in this engine is a write somebody makes HERE — a
// revocation, a removal, a suspension — and each is felt within an applier's
// lag. A central deactivation is felt only when the session's absolute
// lifetime runs out, which for a company that sets that generously is the rest
// of the week.
//
// So the engine ASKS. The only thing it has to ask with is the refresh token
// the login obtained, and the only question a refresh token answers is "is
// this grant still good" — which is exactly the question. A provider whose
// account is gone answers `invalid_grant`.
//
// # It is a duty, and the duty is a fleet singleton
//
// N nodes each probing every live session is N times somebody else's rate
// limit for one answer. It is a SINGLETON rather than a per-node sweep for
// that reason alone — the work is stateless and a lease flap costs a skipped
// interval, not correctness.
//
// # What it deliberately does NOT do
//
// It does not ride the cookie rotation. The design this replaces exchanged the
// refresh token on every hourly rotation and re-sealed the result into the
// log: ten records per person per working day, and an eleven-fold amplifier on
// the log's bytes, for a value that changes only at an exchange. The probe is
// its own interval and writes nothing unless the answer is that somebody is
// gone.
//
// It also does not probe a session with no refresh token, and that is not a
// gap it papers over: a provider that never granted `offline_access` gives
// this nothing to ask with, and validation says so where an operator will read
// it rather than leaving them believing an off-boarding is felt sooner than it
// is.

// Verdict is what one probe concluded.
type Verdict string

const (
	// VerdictLive is a grant the provider still recognises.
	VerdictLive Verdict = "live"

	// VerdictDeactivated is `invalid_grant`: the account is gone,
	// suspended, or its consent has been withdrawn. The session ends.
	VerdictDeactivated Verdict = "deactivated"

	// VerdictUnknown is everything else — the provider was unreachable,
	// rate-limited, or answered something this engine cannot read.
	//
	// IT IS NOT "deactivated", and the distinction is the whole of the
	// three-valued rule applied here: treating an unreachable provider as
	// a deactivation would sign the entire company out the first time
	// somebody else's service had an outage.
	VerdictUnknown Verdict = "unknown"
)

// LiveSession is one session the probe has something to ask about.
type LiveSession struct {
	// Lineage identifies the session, and Person whose it is.
	Lineage string
	Person  string

	// Refresh is the token to exchange. A session with none is not
	// handed to the probe at all.
	Refresh string
}

// Sessions is where the probe gets its work and how it reports a conclusion.
//
// DEFINED HERE, by the caller, and three methods wide. The identity estate has
// a reader and a writer with dozens between them; what a probe needs is the
// live OIDC sessions, a way to end one, and a way to record a refresh token
// the provider rotated.
type Sessions interface {
	// LiveOIDC lists the sessions this probe may ask about — open,
	// opened through this provider, and holding a refresh token.
	LiveOIDC(ctx context.Context) ([]LiveSession, error)

	// End closes one session as `idp_revoked`.
	End(ctx context.Context, lineage, reason string) error

	// Rotated records a refresh token the provider replaced. A provider
	// that rotates has INVALIDATED the old one, so failing to record the
	// new one makes the next probe report a deactivation that has not
	// happened.
	Rotated(ctx context.Context, lineage, refresh string) error
}

// ReasonIdPRevoked is what a session ended by the probe records.
const ReasonIdPRevoked = "idp_revoked"

// Prober runs the duty.
type Prober struct {
	provider *Provider
	sessions Sessions
	logger   *slog.Logger
}

// NewProber builds one.
func NewProber(provider *Provider, sessions Sessions, logger *slog.Logger) *Prober {
	if logger == nil {
		logger = slog.Default()
	}
	return &Prober{provider: provider, sessions: sessions, logger: logger}
}

// Interval is how often the duty should run.
func (p *Prober) Interval() time.Duration { return p.provider.Config().Probe() }

// Run performs one pass and reports how many sessions it ended.
//
// A PASS IS BEST EFFORT PER SESSION. One provider error must not stop the
// pass: the sessions are independent, and a rate limit part-way through would
// otherwise mean every session after it in the list is never checked at all.
func (p *Prober) Run(ctx context.Context) (checked, ended int, err error) {
	if p.provider == nil || p.sessions == nil {
		return 0, 0, errors.New("oidc: the probe has no provider or no sessions")
	}
	metadata, err := p.provider.Metadata(ctx)
	if err != nil {
		// NO METADATA IS NOT A DEACTIVATION. Without the token endpoint
		// there is nothing to ask, and the honest answer is to do
		// nothing this interval.
		return 0, 0, err
	}
	live, err := p.sessions.LiveOIDC(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, session := range live {
		if ctx.Err() != nil {
			return checked, ended, ctx.Err()
		}
		if session.Refresh == "" {
			continue
		}
		checked++
		verdict, rotated := p.check(ctx, metadata.TokenEndpoint, session)
		switch verdict {
		case VerdictDeactivated:
			if err := p.sessions.End(ctx, session.Lineage, ReasonIdPRevoked); err != nil {
				p.logger.WarnContext(ctx, "oidc_probe_end_failed",
					"lineage", session.Lineage, "error", err.Error())
				continue
			}
			ended++
			p.logger.InfoContext(ctx, "iam_session_ended",
				"lineage", session.Lineage, "person", session.Person,
				"reason", ReasonIdPRevoked)
		case VerdictLive:
			if rotated != "" && rotated != session.Refresh {
				if err := p.sessions.Rotated(ctx, session.Lineage, rotated); err != nil {
					p.logger.WarnContext(ctx, "oidc_probe_rotation_unrecorded",
						"lineage", session.Lineage, "error", err.Error(),
						"detail", "the provider replaced this session's refresh "+
							"token and the new one was not stored, so the next "+
							"probe will read the old one as a deactivation")
				}
			}
		}
	}
	return checked, ended, nil
}

// check asks the provider about one session.
func (p *Prober) check(ctx context.Context, tokenEndpoint string,
	session LiveSession) (Verdict, string) {

	tokens, err := p.provider.Config().Refresh(ctx, p.provider.client,
		tokenEndpoint, session.Refresh)
	switch {
	case errors.Is(err, ErrDeactivated):
		return VerdictDeactivated, ""
	case err != nil:
		// UNKNOWN, AND LOGGED ONCE PER SESSION RATHER THAN SWALLOWED.
		// An unreachable provider is not a deactivation, and a pass
		// that silently concluded nothing would look exactly like one
		// where everybody is still employed.
		p.logger.WarnContext(ctx, "oidc_probe_unknown",
			"lineage", session.Lineage, "error", err.Error())
		return VerdictUnknown, ""
	}
	return VerdictLive, tokens.Refresh
}
