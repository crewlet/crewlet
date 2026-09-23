package oidc

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
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

// Listing is one pass's work: the sessions to ask about, and the ones that
// could not be.
//
// # Partial is the ordinary shape, and it is carried rather than joined
//
// Each session's refresh token is its own row in its own custody, so one that
// will not read — a newer peer's grant during a rolling upgrade, a row whose
// name and content disagree, a store blip on one key — says nothing about the
// others. The contract used to be a list and a joined error, and the probe
// threw the list away whenever the error was set: one unreadable grant meant
// every session in the company went unasked for as long as it stood, and
// everybody an identity provider had disabled kept their session until its
// absolute deadline, with a warning as the only sign. A skipped session is
// COUNTED and the pass goes on.
type Listing struct {
	// Sessions are the live sessions the probe may ask about.
	Sessions []LiveSession

	// Skipped is one error per session this pass could not ask about,
	// each naming which.
	Skipped []error
}

// Pass is what one probe pass did.
type Pass struct {
	// Checked is how many sessions the provider was asked about.
	Checked int

	// Ended is how many it said were gone, and were closed here.
	Ended int

	// Skipped is how many sessions the listing could not hand over — see
	// [Listing.Skipped]. Each was logged by name.
	Skipped int

	// Failed is how many sessions were asked about and then could not be
	// acted on: a deactivation whose close did not land, or a rotated token
	// that could not be recorded. Each was logged by name, and the next
	// pass asks again.
	Failed int
}

// Sessions is where the probe gets its work and how it reports a conclusion.
//
// DEFINED HERE, by the caller, and three methods wide. The identity estate has
// a reader and a writer with dozens between them; what a probe needs is the
// live OIDC sessions, a way to end one, and a way to record a refresh token
// the provider rotated.
type Sessions interface {
	// LiveOIDC lists the sessions this probe may ask about — open,
	// opened through this provider, and holding a refresh token — and
	// every one it could not.
	//
	// THE ERROR IS FOR A LISTING THAT FAILED WHOLE, and nothing else: a
	// grant that would not read is ONE session this pass cannot ask about
	// and belongs in [Listing.Skipped] beside the rest. Tidying away the
	// grant of a session that ended some other way is NOT the listing's —
	// the probe runs only while a provider is configured, and a grant must
	// not outlive its session on a deployment that dropped one.
	LiveOIDC(ctx context.Context) (Listing, error)

	// End closes one session as `idp_revoked`. It takes the SESSION rather
	// than its lineage because a close is filed under its person's bucket.
	End(ctx context.Context, session LiveSession, reason string) error

	// Rotated records a refresh token the provider replaced. A provider
	// that rotates has INVALIDATED the old one, so failing to record the
	// new one makes the next probe report a deactivation that has not
	// happened.
	Rotated(ctx context.Context, lineage, refresh string) error
}

// ReasonIdPRevoked is what a session ended by the probe records.
//
// THE AUDIT TRAIL'S OWN SPELLING, taken from it rather than written twice: the
// record's reason and the event's are one fact, and a reader joining
// `iam_history` to the live feed matches on the word.
const ReasonIdPRevoked = string(types.EndIdPRevoked)

// Audit is where the probe announces a session it ended.
//
// CONSUMER-DEFINED and one method, the only part of the node's audit trail a
// probe uses: its conclusion is authored by the engine's own duty, never by a
// caller, so there is nothing here to count — only an ending to say out loud.
type Audit interface {
	Emit(ctx context.Context, payload events.Payload)
}

// Prober runs the duty.
type Prober struct {
	provider *Provider
	sessions Sessions
	logger   *slog.Logger
	audit    Audit
}

// NewProber builds one.
func NewProber(provider *Provider, sessions Sessions, logger *slog.Logger) *Prober {
	if logger == nil {
		logger = slog.Default()
	}
	return &Prober{provider: provider, sessions: sessions, logger: logger}
}

// WithAudit installs where an ended session is announced, and returns the
// prober for chaining.
//
// CALLED ONCE, BY WHOEVER ARMS THE DUTY, before the first pass. Nil announces
// nothing, which leaves the ending in `iam_history` alone — the record the
// probe writes is what every node applies, and the event is the live feed
// beside it.
func (p *Prober) WithAudit(a Audit) *Prober {
	p.audit = a
	return p
}

// Interval is how often the duty should run.
func (p *Prober) Interval() time.Duration { return p.provider.Config().Probe() }

// Run performs one pass and reports what it did.
//
// A PASS IS BEST EFFORT PER SESSION, from the listing to the last close. One
// provider error must not stop the pass: the sessions are independent, and a
// rate limit part-way through would otherwise mean every session after it in
// the list is never checked at all. The same holds one step earlier — a
// session the listing could not hand over is counted in [Pass.Skipped] and
// the rest are asked about. The error is for a pass that could do NOTHING:
// no provider metadata, a listing that failed whole, a cancelled context.
func (p *Prober) Run(ctx context.Context) (Pass, error) {
	var pass Pass
	if p.provider == nil || p.sessions == nil {
		return pass, errors.New("oidc: the probe has no provider or no sessions")
	}
	metadata, err := p.provider.Metadata(ctx)
	if err != nil {
		// NO METADATA IS NOT A DEACTIVATION. Without the token endpoint
		// there is nothing to ask, and the honest answer is to do
		// nothing this interval.
		return pass, err
	}
	listing, err := p.sessions.LiveOIDC(ctx)
	if err != nil {
		return pass, err
	}
	for _, skipped := range listing.Skipped {
		pass.Skipped++
		p.logger.WarnContext(ctx, "oidc_probe_session_skipped",
			"error", skipped.Error(),
			"detail", "this session is not asked about this pass; every "+
				"other one is, and the next pass tries it again")
	}
	for _, session := range listing.Sessions {
		if ctx.Err() != nil {
			return pass, ctx.Err()
		}
		if session.Refresh == "" {
			continue
		}
		pass.Checked++
		verdict, rotated := p.check(ctx, metadata.TokenEndpoint, session)
		switch verdict {
		case VerdictDeactivated:
			if err := p.sessions.End(ctx, session, ReasonIdPRevoked); err != nil {
				pass.Failed++
				p.logger.WarnContext(ctx, "oidc_probe_end_failed",
					"lineage", session.Lineage, "error", err.Error())
				continue
			}
			pass.Ended++
			p.logger.InfoContext(ctx, "iam_session_ended",
				"lineage", session.Lineage, "person", session.Person,
				"reason", ReasonIdPRevoked)
			// ONLY ONCE THE RECORD LANDED, and naming nobody as its
			// author: the provider said the account is gone, and the
			// engine's duty acted on it.
			if p.audit != nil {
				p.audit.Emit(ctx, types.IAMSessionEnded{
					Person: session.Person, Lineage: session.Lineage,
					Reason: types.EndIdPRevoked,
				})
			}
		case VerdictLive:
			if rotated != "" && rotated != session.Refresh {
				if err := p.sessions.Rotated(ctx, session.Lineage, rotated); err != nil {
					pass.Failed++
					p.logger.WarnContext(ctx, "oidc_probe_rotation_unrecorded",
						"lineage", session.Lineage, "error", err.Error(),
						"detail", "the provider replaced this session's refresh "+
							"token and the new one was not stored, so the next "+
							"probe will read the old one as a deactivation")
				}
			}
		}
	}
	return pass, nil
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
