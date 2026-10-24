package statelog

import (
	"context"
	"sync"
)

// Refusal is one record this node would not authenticate, as the audit trail
// states it.
//
// # Why the framework says it and no domain does
//
// Every domain's log is signed and verified by the same code, so a record the
// tracker's applier cannot authenticate is the same fact as one on the
// identity log. A domain that reported its own would report it under its own
// name and leave the other four silent; the verdict is the framework's, and
// the domain is a field.
type Refusal struct {
	// Domain is whose log the record was on.
	Domain string

	// Verdict is [KeyUnknown] or [Tampered], and never [Verified].
	Verdict Verdict

	// KeyID is the key the FRAME names. Empty when the bytes are not a
	// signed frame at all, which is itself a [Tampered] verdict.
	//
	// WHATEVER THE FRAME SAYS, which is the reason the witness is bounded:
	// anything that can reach a cluster port can write a frame, and the id
	// it carries is theirs to choose.
	KeyID string

	// Held is every key id this node's verifier does hold — the half of
	// the comparison an operator needs to act on it.
	Held []string

	// Position is where the record sat on the log.
	Position Position
}

// Witness is told about a record this node refused to authenticate.
//
// CONSUMER-DEFINED here, where it is called, and one method. The runner has
// already logged the refusal and counted it; what a witness adds is the
// company's audit feed, so an administrator who never reads a node's log sees
// that something signed under an unknown key reached the broker. Nil witnesses
// nothing, which is a runner in a test or a tool.
//
// CALLED OUTSIDE EVERY APPLY TRANSACTION, from the decode of a batch or the
// re-verification of a retained record — never from a body the store may run
// twice — and at most once per verdict and key id per runner, up to
// [MaxWitnessedKeys].
type Witness interface {
	RecordRefused(ctx context.Context, r Refusal)
}

// MaxWitnessedKeys is how many distinct (verdict, key id) pairs one runner
// tells its witness about in its life.
//
// SIXTEEN, and the number is what a fleet can legitimately produce times a
// wide margin. A rotation is add, restart, flip, restart, drop — at most two
// ids are in play at once, three while an old one drains — and each verdict is
// a separate pair, so a fleet mid-rotation with a misconfigured node reaches
// perhaps six. Past sixteen the ids are being chosen by whoever is writing
// frames, and every further row would be a row an unauthenticated writer
// authored: the admission rule internal/events enforces on a type whose rate
// the engine claims to author is exactly that no such writer can pace it. The
// refusals past the cap are still logged and counted per record, and the
// sixteen rows already published say plainly what is happening.
const MaxWitnessedKeys = 16

// witnessed is the runner's memory of what it has already told its witness.
type witnessed struct {
	mu        sync.Mutex
	seen      map[witnessKey]struct{}
	saturated bool
}

type witnessKey struct {
	verdict Verdict
	keyID   string
}

// claim reports whether (verdict, id) is new and within the cap, remembering
// it if so, and whether this claim is the one that found the cap full for the
// first time.
func (w *witnessed) claim(verdict Verdict, id string) (fresh, firstRefused bool) {
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
func (r *Runner) witness(ctx context.Context, verdict Verdict, framed []byte, at Position) {
	if r.witnessTo == nil {
		return
	}
	id := frameKeyID(framed)
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
