package authz

import (
	"slices"

	"github.com/crewlet/crewlet/internal/iam"
)

// The fold: one gesture, several decisions.
//
// Some gestures are not one verb asked of one object. A configuration write is
// decided part by part — every seat or unit it moves, at every place it
// reaches, beside every setting and credential it changes — and the write
// lands only if every part is admitted. Its refusal has to say what [Decide]
// would have said had the whole gesture been one question: which rule shut it,
// and what would open all of it. [Combine] is that answer, and it asks the
// reasons in Decide's own order, because that order is what makes each answer
// one a caller can act on.

// foldClass is what a reason says about the decision that carries it, for
// [Combine]: an admission, a bar on the caller, a refusal on authority, or a
// proof that has gone stale.
type foldClass int

const (
	foldAdmission foldClass = iota + 1
	foldBar
	foldAuthority
	foldProof
)

// fold is the class of each reason this build decides by, and a reason in no
// class is one [Combine] cannot place — a gate in this package's tests holds
// every [Reasons] value to exactly one.
//
// THE BARS ARE IN [Decide]'s ORDER, because it is the order a caller clears
// them in: a stage that may not act is reported before a seat that may not
// take a person's verb, as Decide reports it.
var fold = map[Reason]foldClass{
	ReasonGrant:     foldAdmission,
	ReasonSelf:      foldAdmission,
	ReasonAuthor:    foldAdmission,
	ReasonLead:      foldAdmission,
	ReasonEverybody: foldAdmission,
	ReasonAsked:     foldAdmission,
	ReasonShared:    foldAdmission,
	ReasonSeatGrant: foldAdmission,

	ReasonStage:         foldBar,
	ReasonUnknownAction: foldBar,
	ReasonSeatRefused:   foldBar,
	ReasonSeatOnly:      foldBar,
	ReasonUnseated:      foldBar,
	ReasonTokenRefused:  foldBar,
	ReasonAssistRefused: foldBar,
	ReasonAssistScope:   foldBar,
	ReasonNotGranted:    foldBar,
	ReasonCeiling:       foldBar,

	ReasonNoGrant:   foldAuthority,
	ReasonNotSelf:   foldAuthority,
	ReasonNotLead:   foldAuthority,
	ReasonNotAuthor: foldAuthority,
	ReasonUnnamed:   foldAuthority,
	ReasonNotShared: foldAuthority,
	ReasonRole:      foldAuthority,

	ReasonStepUp: foldProof,
}

// barOrder is the order [Decide] asks its bars in, and last the requester's
// ceiling, which [DecideWithin] asks only once Decide has admitted the agent.
//
// THE DASHBOARD ASSISTANT'S TWO sit after a machine token's, where Decide
// asks them ([assistBar]), a verb it never takes before one its grant's
// scope does not reach: the person's approval lifts the second and only
// their own gesture lifts the first, so a gesture meeting both is told the
// one no approval would clear.
//
// AN AGENT'S ROLE GRANT sits after them, where Decide asks it: in place of
// the class, once every bar has passed ([decideSeatGrant]). No permission
// and no proof lifts it — the remedy is the role's, in the company document
// — so it is a bar rather than a refusal on authority, and it comes before
// the ceiling, which DecideWithin asks only of an agent Decide admitted.
var barOrder = []Reason{
	ReasonStage, ReasonUnknownAction, ReasonSeatRefused, ReasonSeatOnly,
	ReasonUnseated, ReasonTokenRefused, ReasonAssistRefused, ReasonAssistScope,
	ReasonNotGranted, ReasonCeiling,
}

// Admits reports a reason a rule ADMITS by — a permission held, the caller's
// own record or remark, a lead relation, everybody's overview, an open ask, a
// share of an artifact, a grant an agent's role carries — which no refusal
// carries.
func (r Reason) Admits() bool { return fold[r] == foldAdmission }

// Authority reports a refusal a permission or a relation decided: the caller
// holds no permission covering the verb, or the record is somebody else's, led
// by somebody else, written by somebody else, or not named at all, or the
// artifact is not shared with them or shared below the verb's role. These are
// the refusals the grants beside them can lift; a bar on the caller — a stage
// that may not act, a seat on a person's verb, a token where a person must be
// present, the Dashboard Assistant on a verb its grant does not reach, an
// agent whose role does not carry the row's grant, an agent whose requester
// could not — and a stale proof are not, whatever the caller holds.
func (r Reason) Authority() bool { return fold[r] == foldAuthority }

// Combine is the decision for a gesture that needs every one of ds, answered
// in the order [Decide] asks its own checks in.
//
//   - A BAR ON THE CALLER FIRST — a stage, a verb this build has no rule for,
//     a seat on a person's verb or a person on a seat's, a credential acting
//     as no seat, a machine token where a person must be present, the
//     Dashboard Assistant on a verb it never takes or one its grant's scope
//     does not reach, an agent whose role does not carry the row's grant
//     ([decideSeatGrant]), an agent whose requester could not ([DecideWithin]) —
//     the one [Decide] would have reported first, unchanged. It is about who
//     is asking rather than any part, and no permission and no proof lifts it.
//   - THEN THE REFUSALS ON AUTHORITY, whose answer is the permission that
//     admits EVERY such part: a gesture opens only when all of it does, so a
//     permission one part names and another does not is no remedy. The grants
//     are the smallest such permissions in [Decision.Grants]'s own sense —
//     any one of them, added to what the caller holds, admits every part, and
//     one that covers another through [iam.Expand] is left out — so a write
//     refused for a tracker setting and a knowledge one names admin, which
//     admits both. The reason is the parts' own where they agree and
//     [ReasonNoGrant] where they do not: there is no one relation to name.
//     And a part no permission lifts at all — somebody else's record with no
//     admin path, its grants empty — is the answer as it stands, since naming
//     a permission for the rest would promise what nothing can deliver.
//   - THEN AN UNKNOWN: this node could not decide some part. Asked only once
//     nothing refused, because no answer that part could get would admit a
//     gesture another part already shuts — the rule [Decide] itself keeps,
//     refusing a lead short of a row's app whatever the chart says.
//   - THE PROOF LAST, because every earlier answer is one a fresher proof
//     would not change: the parts refused only for a stale proof are a
//     [ReasonStepUp] naming the window — the strictest any part asks — and
//     the grants the rules admitted the caller on.
//   - Otherwise every part admits, and the gesture is the first part's
//     admission; a gesture of no parts asks nothing of anybody, which is
//     [ReasonEverybody]'s admission.
//
// ONE DECISION IS ITSELF, so a surface that folds one part answers exactly what
// [Decide] did.
func Combine(ds ...Decision) Decision {
	if len(ds) == 1 {
		return ds[0]
	}
	var (
		bar, unknown     *Decision
		authority, proof []Decision
	)
	for i := range ds {
		d := ds[i]
		switch {
		case d.Unknown():
			if unknown == nil {
				unknown = &ds[i]
			}
			continue
		case d.Allowed:
			continue
		}
		switch fold[d.Reason] {
		case foldBar:
			if bar == nil || barRank(d.Reason) < barRank(bar.Reason) {
				bar = &ds[i]
			}
		case foldProof:
			proof = append(proof, d)
		default:
			// A REFUSAL ON AUTHORITY, and so is one carrying a reason no
			// class holds — an admission's word on a refusal, or a word
			// this build does not know — since the grants beside it are
			// all it says about a remedy.
			authority = append(authority, d)
		}
	}
	switch {
	case bar != nil:
		return *bar
	case len(authority) > 0:
		return foldAuthorityRefusals(authority)
	case unknown != nil:
		return *unknown
	case len(proof) > 0:
		return foldProofs(proof)
	case len(ds) == 0:
		return Decision{Allowed: true, Reason: ReasonEverybody}
	}
	return ds[0]
}

// barRank is a bar's place in [Decide]'s order, and past every one for a word
// the order does not hold.
func barRank(r Reason) int {
	if i := slices.Index(barOrder, r); i >= 0 {
		return i
	}
	return len(barOrder)
}

// foldAuthorityRefusals is [Combine]'s answer for its refusals on authority.
func foldAuthorityRefusals(parts []Decision) Decision {
	for _, d := range parts {
		if len(d.Grants) == 0 {
			return d
		}
	}
	cover := admitsEvery(parts)
	if len(cover) == 0 {
		// NO ONE PERMISSION OPENS EVERY PART, which admin always does for
		// any part a permission lifts — so this is a part naming only
		// words this build does not read, and its own answer stands.
		return parts[0]
	}
	reason := parts[0].Reason
	for _, d := range parts[1:] {
		if d.Reason != reason {
			reason = ReasonNoGrant
			break
		}
	}
	return Decision{Reason: reason, Grants: cover}
}

// foldProofs is [Combine]'s answer for parts refused only for a stale proof.
func foldProofs(parts []Decision) Decision {
	out := Decision{Reason: ReasonStepUp, Recency: parts[0].Recency}
	for _, d := range parts {
		if d.Recency.Demands() {
			out.Recency = d.Recency
			break
		}
	}
	for _, d := range parts {
		if len(d.Grants) == 0 {
			return out
		}
	}
	out.Grants = admitsEvery(parts)
	return out
}

// admitsEvery is the smallest permissions any ONE of which admits every one
// of parts — a permission admits a part when it holds, through [iam.Expand],
// one of the grants the part names — in the order the parts name them, admin
// and the rest of [iam.AllGrants] after; one that covers another such
// permission is left out, as [Decision.Grants] leaves it out.
func admitsEvery(parts []Decision) []iam.Grant {
	var candidates []iam.Grant
	for _, d := range parts {
		for _, g := range d.Grants {
			if !slices.Contains(candidates, g) {
				candidates = append(candidates, g)
			}
		}
	}
	for _, g := range iam.AllGrants {
		if !slices.Contains(candidates, g) {
			candidates = append(candidates, g)
		}
	}
	admits := func(g iam.Grant) bool {
		holds := iam.Expand([]iam.Grant{g})
		for _, d := range parts {
			if !slices.ContainsFunc(d.Grants, func(want iam.Grant) bool {
				return slices.Contains(holds, want)
			}) {
				return false
			}
		}
		return true
	}
	var admitting []iam.Grant
	for _, g := range candidates {
		if admits(g) {
			admitting = append(admitting, g)
		}
	}
	var cover []iam.Grant
	for _, g := range admitting {
		holds := iam.Expand([]iam.Grant{g})
		covered := slices.ContainsFunc(admitting, func(h iam.Grant) bool {
			return h != g && slices.Contains(holds, h)
		})
		if !covered {
			cover = append(cover, g)
		}
	}
	return cover
}
