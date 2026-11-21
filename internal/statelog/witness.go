package statelog

import (
	"context"
	"sync"

	"github.com/crewlet/crewlet/internal/recordseal"
)

// Refusal is one record this node would not authenticate, as the audit trail
// states it. The framework reports it, not the domain: every log is verified
// by the same code, so the verdict is one fact and the domain is a field.
type Refusal struct {
	// Domain is whose log the record was on.
	Domain string

	// Verdict is [recordseal.KeyUnknown] or [recordseal.Tampered], and never [recordseal.Verified].
	Verdict recordseal.Verdict

	// KeyID is the key the FRAME names, empty when the bytes are not a signed
	// frame (itself a [recordseal.Tampered] verdict). Anything reaching a
	// cluster port chooses it, hence [MaxWitnessedKeys].
	KeyID string

	// Held is every key id this node's verifier holds: the half of the
	// comparison an operator acts on.
	Held []string

	// Position is where the record sat on the log.
	Position Position
}

// Witness is told about a record this node refused to authenticate. The
// runner has already logged and counted it; a witness adds the company's
// audit feed. Nil witnesses nothing (a runner in a test or a tool).
//
// CALLED OUTSIDE EVERY APPLY TRANSACTION (from a batch's decode or a retained
// record's re-verification, never from a body the store may run twice), at
// most once per verdict and key id per runner, up to [MaxWitnessedKeys].
type Witness interface {
	RecordRefused(ctx context.Context, r Refusal)
}

// MaxWitnessedKeys is how many distinct (verdict, key id) pairs one runner
// tells its witness about in its life.
//
// SIXTEEN, a wide margin over what a fleet legitimately produces: a rotation
// has at most three ids in play and each verdict is its own pair, so a fleet
// mid-rotation with a misconfigured node reaches about six. Past sixteen,
// whoever writes frames is choosing the ids, and internal/events admits a
// type the engine claims to author only if no such writer can pace it.
// Refusals past the cap are still logged and counted per record.
const MaxWitnessedKeys = 16

// witnessed is the runner's memory of what it has already told its witness.
type witnessed struct {
	mu        sync.Mutex
	seen      map[witnessKey]struct{}
	saturated bool
}

type witnessKey struct {
	verdict recordseal.Verdict
	keyID   string
}

// claim reports whether (verdict, id) is new and within the cap, remembering
// it if so, and whether this claim is the first to find the cap full.
func (w *witnessed) claim(verdict recordseal.Verdict, id string) (fresh, firstRefused bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	key := witnessKey{verdict: verdict, keyID: id}
	if _, seen := w.seen[key]; seen {
		return false, false
	}
	if len(w.seen) >= MaxWitnessedKeys {
		first := !w.saturated
		w.saturated = true
		return false, first
	}
	if w.seen == nil {
		w.seen = map[witnessKey]struct{}{}
	}
	w.seen[key] = struct{}{}
	return true, false
}

// witness tells the runner's witness about one refused record, at most once
// per verdict and key id.
func (r *Runner) witness(ctx context.Context, verdict recordseal.Verdict, framed []byte, at Position) {
	if r.witnessTo == nil {
		return
	}
	id := recordseal.ClaimedKeyID(framed)
	fresh, firstRefused := r.witnessedKeys.claim(verdict, id)
	if firstRefused {
		r.logger.WarnContext(ctx, "statelog_refusals_unwitnessed",
			"domain", r.domain.Name(), "cap", MaxWitnessedKeys,
			"detail", "this node has refused records under more distinct key "+
				"ids than a keyring rotation produces; further ids are logged "+
				"and counted per record but no longer reach the audit feed, "+
				"because whoever is choosing them is writing to the broker")
	}
	if !fresh {
		return
	}
	r.witnessTo.RecordRefused(ctx, Refusal{
		Domain: r.domain.Name(), Verdict: verdict, KeyID: id,
		Held: r.verifier.KeyIDs(), Position: at,
	})
}
