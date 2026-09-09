package integration

import "fmt"

// FindingKind is one thing a pass observed that is not "fine".
//
// A CLOSED SET, and that is the whole point of this file. Seven third-party apps
// converge seven different surfaces, but what they can find is the same short
// list every time: nothing to authenticate with, an app nobody installed, a
// delivery path that goes nowhere, a seat with no account, a grant the third-party app
// has not applied, a grant that is too small, a grant that is too large. A
// third-party app contributes WHICH of these it found and about what; it does not get
// to invent an eighth, and it does not get to decide what any of them means.
type FindingKind string

const (
	// FindingCredentialMissing is this deployment's own configuration: the
	// block is enabled and the credential its ${VAR} names resolved to
	// nothing, so the pass could not authenticate at all.
	FindingCredentialMissing FindingKind = "credential_missing"

	// FindingCredentialRejected is a credential that RESOLVED and that the
	// third-party app refused: a revoked token, a rotated key, an account that lost
	// the access it was issued with.
	//
	// Distinct from [FindingCredentialMissing] because the fix is
	// different (that one is a ${VAR} pointing at nothing, this one is a
	// value the third-party app will not accept), and distinct from a
	// transport fault because it NEVER clears on its own. Every pass will be
	// refused identically until a person changes the credential, so reporting
	// it as a wait leaves an operator watching a retry that cannot succeed.
	FindingCredentialRejected FindingKind = "credential_rejected"

	// FindingApprovalRequired is an app or scope a person must install or
	// approve at the third-party app. ActionURL carries where.
	FindingApprovalRequired FindingKind = "approval_required"

	// FindingIngressBlocked is a delivery path that cannot be established
	// and will not fix itself: a webhook the credential may not register,
	// a plan that does not offer group hooks, no public base URL to point
	// at.
	FindingIngressBlocked FindingKind = "ingress_blocked"

	// FindingIngressPending is a delivery path the pass could not finish
	// establishing this time, and will try again.
	FindingIngressPending FindingKind = "ingress_pending"

	// FindingIdentityMissing is a seat with no account at the third-party app yet.
	// The engine's own work.
	FindingIdentityMissing FindingKind = "identity_missing"

	// FindingIdentityFailed is a seat whose account could not be created.
	FindingIdentityFailed FindingKind = "identity_failed"

	// FindingGrantPending is access the third-party app accepted and has not applied
	// yet. Nobody has to act; it resolves on its own.
	FindingGrantPending FindingKind = "grant_pending"

	// FindingUnknownTier is an access tier the company document names that
	// this third-party app does not have. The engine falls back to the default tier,
	// so the seat under-grants rather than stopping, and the typo is
	// reported instead of silently landing on a default.
	FindingUnknownTier FindingKind = "unknown_tier"

	// FindingGrantShort is a seat holding LESS than its tier asks for, in a
	// way only a person at the third-party app can widen.
	FindingGrantShort FindingKind = "grant_short"

	// FindingGrantExcess is a seat holding MORE than its tier asks for.
	//
	// ALWAYS ADVISORY, and always last. The engine did not grant it and
	// cannot revoke it: it comes from the operator's own scheme, usually
	// inherited from a parent group or a second role. Agents keep working,
	// so the integration is READY with a note, not blocked.
	FindingGrantExcess FindingKind = "grant_excess"

	// FindingRegistrationOrphaned is something this engine registered at
	// the third-party app that it no longer manages, because the name it is
	// held under changed.
	//
	// ALSO ADVISORY, and for a sharper reason than the excess grant's: the
	// orphan still WORKS. A Datadog webhook definition is addressed by name,
	// and that name is also the handle a monitor writes (`@webhook-crewlet`),
	// so renaming `webhook_name` leaves the previous definition delivering to
	// this same engine with this same token — correctly, for every monitor
	// still naming it. Deleting it would silence exactly those monitors,
	// which is why the engine does not, and Datadog serves no listing, so
	// nothing can ever find it again.
	//
	// So it is REPORTED. Somebody has to repoint the monitors and remove the
	// old definition, and this is the only place they can learn it exists.
	FindingRegistrationOrphaned FindingKind = "registration_orphaned"
)

// severity ranks the kinds from "nothing works" to "everything works, with a
// note". [Classify] reports the lowest-ranked kind present.
//
// # The ordering IS the contract, and it is what the hand-written classifiers
// it replaces disagreed about
//
// The control plane this was ported from wrote one classifier per
// integration, five of them, each a switch over that integration's own
// result struct. They had already drifted on the question that matters
// most: WHERE THE EXCESS-ACCESS ADVISORY SITS. Because that advisory
// reports READY, anything ranked below it disappears when both are present.
//
// Only one of the five got it right. Atlassian's ran the advisory in a second
// pass, after every blocking condition. The other three that have one at all
// return it early:
//
//   - GitLab checks excess inside the per-seat loop, immediately BEFORE the
//     short-grant check, so a seat holding too much in one dimension and too
//     little in another reports ready with a note and the block vanishes.
//   - Datadog checks it before the failed-agent count, so a company with one
//     over-granted agent and one that could not be provisioned at all reports
//     ready.
//   - GitHub checks its installation-level excess before the WEBHOOK check
//     and before every per-seat check, so an app holding one spare permission
//     reports ready while its deliveries reach nobody.
//
// Two more disagreements sit in the same place. Whether an unknown access
// tier outranks a broken delivery path: GitLab says yes, Datadog says no.
// And where a seat that could not be provisioned goes: it is the fallthrough
// in all five, ranked below everything including the advisory.
//
// None of those is a hard bug to write. All of them are invisible from inside
// one integration's function, which is why the ordering lives here, once, with a
// test that pins it.
//
// The rule the order encodes: rank by HOW MUCH OF THE INTEGRATION IS NOT
// WORKING, from "the pass could not authenticate" down to "it works, but
// somebody should look". Within one level, engine-owned work is reported
// before person-owned work, because telling a person to fix something the
// engine is still mid-way through sends them to repair what is not broken.
func (f FindingKind) severity() int {
	switch f {
	case FindingCredentialMissing:
		return 0
	case FindingCredentialRejected:
		// Beside missing, and below everything else, for the same reason
		// missing is first: a pass that cannot authenticate observed
		// nothing, so every other finding it carries is a guess.
		return 1
	case FindingApprovalRequired:
		return 2
	case FindingIngressBlocked:
		return 3
	case FindingIngressPending:
		return 4
	case FindingIdentityMissing:
		return 5
	case FindingIdentityFailed:
		return 6
	case FindingGrantPending:
		return 7
	case FindingUnknownTier:
		return 8
	case FindingGrantShort:
		return 9
	case FindingGrantExcess:
		// The FIRST of the two advisories, and the reason is the whole
		// comment above: its verdict is ready, so anything it outranks is
		// a problem it hides.
		return 11
	case FindingRegistrationOrphaned:
		// LAST, beneath the other advisory. Both report ready; this one
		// is the more purely informational of the two, because what it
		// names is still working.
		return 12
	default:
		// A kind this build does not know, ranked ABOVE the advisory and
		// below every real problem. A peer on a newer build can write one
		// into the shared state, and the two wrong answers are opposite:
		// ranking it worst would let an older node report a healthy
		// company as broken, and ranking it below the advisory would let
		// one spare permission hide a finding this binary cannot read.
		return 10
	}
}

// Verdict is the phase and actor a kind implies. Fixed per kind: a third-party app
// says what it found, never what it means.
func (f FindingKind) Verdict() (Phase, Actor) {
	switch f {
	case FindingCredentialMissing:
		return PhaseUnconfigured, ActorOperator
	case FindingCredentialRejected:
		// UNCONFIGURED, which the phase doc defines as "cannot talk to
		// the surface at all" — true of a refused credential exactly as
		// it is of an absent one. And the OPERATOR's, not the engine's:
		// no retry fixes a token the third-party app will not accept.
		return PhaseUnconfigured, ActorOperator
	case FindingApprovalRequired:
		return PhaseAwaitingAdmin, ActorAdmin
	case FindingIngressBlocked:
		return PhaseDegraded, ActorAdmin
	case FindingIngressPending:
		return PhaseActivating, ActorEngine
	case FindingIdentityMissing:
		return PhaseProvisioning, ActorEngine
	case FindingIdentityFailed:
		return PhaseDegraded, ActorAdmin
	case FindingGrantPending:
		return PhaseActivating, ActorProvider
	case FindingUnknownTier:
		return PhaseDegraded, ActorOperator
	case FindingGrantShort:
		return PhaseDegraded, ActorAdmin
	case FindingGrantExcess:
		// READY, not degraded. It is a note on a working integration.
		return PhaseReady, ActorAdmin
	case FindingRegistrationOrphaned:
		// READY too, and owed by the ADMIN: what has to happen is at the
		// third-party app — repoint the monitors, then remove the
		// definition nothing points at any more.
		return PhaseReady, ActorAdmin
	default:
		// A kind this build does not know is reported as degraded rather
		// than ready, and pointed at the person who can read the peer's
		// logs. Claiming a surface is fine on the strength of a word this
		// binary cannot interpret is the one answer that is certainly
		// wrong.
		return PhaseDegraded, ActorOperator
	}
}

// sentence is the fallback prose for a finding that carried no detail of its
// own, so a report is never blank.
//
// Deliberately generic: a third-party app that can say something specific SHOULD, and
// [Finding.Detail] is where it does. This exists so that a third-party app which
// cannot still produces a sentence an operator can act on rather than a bare
// phase name.
func (f FindingKind) sentence(subject string) string {
	about := ""
	if subject != "" {
		about = " for " + subject
	}
	switch f {
	case FindingCredentialMissing:
		return "this integration has no usable credential" + about
	case FindingCredentialRejected:
		return "the third-party app refused this integration's credential" + about
	case FindingApprovalRequired:
		return "an administrator must approve this integration at the third-party app" + about
	case FindingIngressBlocked:
		return "events cannot be delivered to this engine" + about
	case FindingIngressPending:
		return "still pointing the third-party app at this engine" + about
	case FindingIdentityMissing:
		return "creating agent identities" + about
	case FindingIdentityFailed:
		return "an agent identity could not be created" + about
	case FindingGrantPending:
		return "the third-party app is applying agent access" + about
	case FindingUnknownTier:
		return "the company names an access tier this third-party app does not have" + about
	case FindingGrantShort:
		return "an agent holds less access than its role asks for" + about
	case FindingGrantExcess:
		return "an agent holds more access than its role asks for" + about
	case FindingRegistrationOrphaned:
		return "this engine registered something that it no longer manages" + about
	default:
		return "this integration reported " + string(f) + about
	}
}

// Finding is one observation from a pass.
//
// A third-party app emits these and nothing else. It does not build a [Report], does
// not choose a phase, and does not decide which of its findings matters most,
// because those three decisions are the ones that have to agree across every
// surface and cannot be checked from inside any one of them.
type Finding struct {
	Kind FindingKind `json:"kind"`
	// Subject is what the finding is about: a seat handle, a project, a
	// repository, a channel. Empty when the finding is about the
	// integration as a whole.
	Subject string `json:"subject,omitempty"`
	// Detail is one sentence naming what is outstanding, addressed to the
	// actor the kind implies. Empty falls back to [FindingKind.sentence].
	Detail string `json:"detail,omitempty"`
	// ActionURL is where the person named by the actor goes to do it. Only
	// meaningful for a kind whose actor is a person.
	ActionURL string `json:"action_url,omitempty"`
}

// worstOf is the finding [Classify] promotes, and where it sits.
//
// One implementation because two callers need the same answer: Classify makes
// the report from it, and [Promote] puts it first in the stored slice so no
// reader has to re-derive which finding the report is about.
func worstOf(findings []Finding) (Finding, int) {
	worst, at := findings[0], 0
	rank := worst.Kind.severity()
	for i, f := range findings[1:] {
		if s := f.Kind.severity(); s < rank {
			worst, rank, at = f, s, i+1
		}
	}
	return worst, at
}

// Promote returns findings with the one [Classify] reports FIRST.
//
// The order is the contract, and it exists because every reader wants the
// same thing: the findings the report did NOT summarise. Without an order
// they had to re-derive the winner, and the dashboard instead assumed it —
// dropping element zero and rendering the tail — so whenever the worst
// finding was not already first, a real finding was hidden and the headline
// was re-printed as "1 more finding".
//
// A STABLE ROTATION rather than a sort: the vendor's own order is meaningful
// below the headline (a pass walks its seats in a stable order, so an
// operator reading the list twice sees the same seats in the same places),
// and sorting by severity would shuffle equals on every pass.
func Promote(findings []Finding) []Finding {
	if len(findings) < 2 {
		return findings
	}
	_, at := worstOf(findings)
	if at == 0 {
		return findings
	}
	out := make([]Finding, 0, len(findings))
	out = append(out, findings[at])
	out = append(out, findings[:at]...)
	return append(out, findings[at+1:]...)
}

// Classify folds a pass's findings into the one report an operator reads.
//
// The worst finding wins, by [FindingKind.severity]. Ties keep the order the
// third-party app emitted them in, so one that walks its seats in a stable
// order reports a stable seat.
//
// When several findings share the winning kind, the count is named. "an agent
// needs maintainer on api-gateway" and "an agent needs maintainer on
// api-gateway (and 4 more)" send an operator to two very different jobs, and
// the difference is invisible from a single seat's sentence.
//
// No findings is [Ready]. That is the whole of the success path: a third-party app
// that converged reports nothing, rather than having to remember to say so.
func Classify(findings []Finding) Report {
	if len(findings) == 0 {
		return Ready()
	}

	worst, _ := worstOf(findings)

	// Counted over the WINNING KIND rather than over everything, because
	// "3 more" has to mean three more of the thing just described. A total
	// that swept in an unrelated advisory would read as three more seats
	// needing the same grant.
	same := 0
	for _, f := range findings {
		if f.Kind == worst.Kind {
			same++
		}
	}

	detail := worst.Detail
	if detail == "" {
		detail = worst.Kind.sentence(worst.Subject)
	}
	if same > 1 {
		detail = fmt.Sprintf("%s (and %d more)", detail, same-1)
	}

	phase, actor := worst.Kind.Verdict()
	report := Report{Phase: phase, Actor: actor, Detail: detail, ActionURL: worst.ActionURL}

	// A person's job needs the sentence and the link; nobody else's does.
	// Handing an operator a settings URL for "the third-party app is applying agent
	// access" invites them to go and interfere with a grant that is landing
	// on its own, and it is the one case where the honest answer is to say
	// nothing yet.
	if !actor.WaitsOnAPerson() {
		report.ActionURL = ""
	}
	return report
}
