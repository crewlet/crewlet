package authz

import (
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
// whose tests build every refusal they read through [RefusalDetail]).
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
)

// RefusalDetail is the machine-readable half of a refusal the authority table
// made: the reason and the grants, under the keys every surface answers them
// with.
//
// VALUES, NOT A SENTENCE. "This needs admin" written at the call site is a
// second statement of the rule, and it is the one that goes stale the day the
// rule's permission moves; the grants here are what the deciding rule actually
// consulted. The sentence a person reads is the envelope's `message`, which
// belongs to the code and is chosen by the reason this carries.
//
// A pair rather than a [Decision], because not every refusal is one: a
// question's own declared permission is checked by internal/api/queries'
// registry, which has a reason and a permission to report and no decision to
// hand over.
func RefusalDetail(reason Reason, grants []iam.Grant) httpjson.Detail {
	names := make([]string, 0, len(grants))
	for _, g := range grants {
		names = append(names, string(g))
	}
	return httpjson.Detail{DetailReason: string(reason), DetailGrants: names}
}

// StepUpDetail is the machine-readable half of a [ReasonStepUp] refusal: the
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
func StepUpDetail(d Decision) httpjson.Detail {
	detail := RefusalDetail(ReasonStepUp, d.Grants)
	detail[DetailWindow] = string(d.Recency)
	return detail
}
