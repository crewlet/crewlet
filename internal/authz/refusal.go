package authz

import (
	"net/http"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/iam"
)

// The two keys a refusal on authority carries beside the envelope's own
// `error` and `message`. Named once, because every surface that refuses on
// authority writes them and a client reads them from all of them: a screen
// that learned `missing_grant` from one route and `grants` from another would
// branch twice on one fact.
//
// AND THE ENVELOPE READS THEM TOO: `unauthorized`'s sentence follows the
// reason, and names the grants only where a permission is a way past the rule
// and an artifact's share is not what clears it (internal/api/httpjson, which
// spells both keys once, for its decoder and its sentence table alike, and
// whose tests build every refusal they read through [RefusalFields]).
const (
	// DetailReason is the rule's own [Reason]. The envelope's key,
	// spelled once in internal/api/httpjson, whose decoder reads it back.
	DetailReason = httpjson.KeyReason
	// DetailGrants is [Decision.Grants]: the smallest permissions any ONE
	// of which would have admitted the caller — an app, or admin. Always
	// present, and an EMPTY list is an answer rather than an omission: no
	// permission would, and what the deciding rule asks is something else —
	// a relation the chart does not hold, an agent's own seat, a seat to act
	// as, a person present rather than a token — which [DetailReason] names.
	DetailGrants = httpjson.KeyGrants
	// DetailWindow is [Decision.Recency] on a step-up refusal: the window
	// the verb asks a proof inside, spelled as the `api.auth.session` key
	// that sets it (`step_up`), so the word a client reads and the setting
	// an operator tunes are one string.
	DetailWindow = "window"
	// DetailRole is [Decision.Role] on a [ReasonRole] refusal: the least
	// role on the artifact the verb needs, so a client can say whom to ask
	// for what — the artifact's owner, for that role.
	DetailRole = "role"
)

// refusalDetail is the machine-readable half of a refusal the authority table
// made: the reason and the grants, under the keys every surface answers them
// with.
//
// VALUES, NOT A SENTENCE. "This needs admin" written at the call site is a
// second statement of the rule, and it is the one that goes stale the day the
// rule's permission moves; the grants here are what the deciding rule actually
// consulted. The sentence a person reads is the envelope's `message`, which
// belongs to the code and is chosen by the reason this carries.
//
// UNEXPORTED, with [stepUpDetail], so the only way to the fields is
// [RefusalFields], which picks the CODE in the same breath. Exported, each half
// was a way to write a refusal's fields beside a code spelled at the call site
// — and every surface that did so answered `unauthorized`, whatever the
// mapping would have said: a question's REST answer, the socket's frames, the
// overview's mirrors, the sign-in surface's step-up. Every refusal reaches a
// surface as a [Decision] now, so a call site has nothing to hand a pair to.
func refusalDetail(reason Reason, grants []iam.Grant) httpjson.Detail {
	names := make([]string, 0, len(grants))
	for _, g := range grants {
		names = append(names, string(g))
	}
	return httpjson.Detail{DetailReason: string(reason), DetailGrants: names}
}

// stepUpDetail is the machine-readable half of a [ReasonStepUp] refusal: the
// reason, the window the verb asks a proof inside, and the grants the rule
// admitted the caller on — under the same keys every refusal uses, plus the
// one that names the window.
//
// WHAT A CLIENT NEEDS TO REPLAY THE REQUEST AND NOTHING ELSE. The remedy is
// the caller's own — confirm who they are (`POST /auth/step-up`) and send the
// same request again — so
// the answer names which proof is missing rather than a grant nobody is
// missing. It deliberately does not carry the deadline the proof passed: that
// is the session's own `reauth_at`, which `GET /auth/session` already serves,
// and a second copy here is a second clock a screen could disagree with.
func stepUpDetail(d Decision) httpjson.Detail {
	detail := refusalDetail(ReasonStepUp, d.Grants)
	detail[DetailWindow] = string(d.Recency)
	return detail
}

// RefusalFields is how one refusal the authority table made is answered: its
// status, its code and the machine-readable fields that say why.
//
//   - A STALE PROOF is `step_up_required`, carrying the window it needs
//     ([stepUpDetail]): the rule admitted the caller, and confirming who they
//     are and replaying the request is the whole remedy.
//   - A MACHINE TOKEN on a verb that needs a person present is
//     `token_refused`: no permission on the token would admit it.
//   - A caller whose ROLE on an artifact is below the verb's is
//     `unauthorized` naming the role it needs ([DetailRole]) beside the
//     reason and the grants.
//   - Every other refusal is `unauthorized`, carrying the rule's reason and
//     the grants that would have admitted the caller ([refusalDetail]).
//   - Except a caller with NO RELATION to an artifact ([Decision.NotFound]),
//     which is `404 not_found` with no detail at all: byte for byte the
//     answer for an id nothing holds, so it cannot say whether the artifact
//     exists, and a surface adds nothing to it.
//
// ONE MAPPING FOR EVERY SURFACE, [EnvelopeRefusal] included, and the reason it
// is a function of its own: a surface that answers a refusal with fields of its
// own beside the rule's — the parts of a write it refused, the sentence a
// tool's gate worded — used to spell the code itself, and the copies drifted:
// the operator surface answered a token's refusal `unauthorized`, whose
// sentence sends a caller to ask for a permission no token could carry. Such a
// surface takes the three values here and adds its fields to the detail. A
// walk in internal/api refuses any of the three codes handed to a call
// anywhere under it, so a surface cannot spell one beside this.
//
// FOR A REFUSAL, never an unknown: a decision this node could not reach is not
// a 403 at all, and every caller answers [Decision.Unknown] first — see
// [EnvelopeRefusal].
func RefusalFields(d Decision) (status int, code httpjson.Code, detail httpjson.Detail) {
	if d.NotFound() {
		return http.StatusNotFound, httpjson.CodeNotFound, nil
	}
	switch d.Reason {
	case ReasonStepUp:
		return http.StatusForbidden, httpjson.CodeStepUpRequired, stepUpDetail(d)
	case ReasonTokenRefused:
		return http.StatusForbidden, httpjson.CodeTokenRefused,
			refusalDetail(d.Reason, d.Grants)
	case ReasonRole:
		detail := refusalDetail(d.Reason, d.Grants)
		if d.Role.Valid() {
			detail[DetailRole] = string(d.Role)
		}
		return http.StatusForbidden, httpjson.CodeUnauthorized, detail
	default:
		return http.StatusForbidden, httpjson.CodeUnauthorized,
			refusalDetail(d.Reason, d.Grants)
	}
}
