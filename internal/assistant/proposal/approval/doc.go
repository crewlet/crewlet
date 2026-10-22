// Package approval carries a person's decision on a Dashboard Assistant
// proposal out: an approval, a decline, a Stop on the turn it parked, and a
// page's claim on and report of a page action (ADR-0051).
//
// # An approval is the person's, made once, and sends exactly what they saw
//
// [Approver.Approve] is decided for the principal the request resolved, never
// for an owner it is handed, so an approval is never another person's: the
// proposal is read owner-keyed and answers [ErrNotFound] for anybody else. It
// is still proposed and unexpired; the digest the card showed is echoed and
// the stored fields still hash to it (internal/assistant/proposal.Verify); a
// step-up the write asks for is asked of THIS request before anything is
// recorded, so a refusal records nothing and the dashboard's step-up dialog
// replays the approval; the object's revision is re-read as the person through
// a grant derived from this request, and one that moved is stale. Only then is
// the approval recorded, ARBITRATED on the proposal's subject, so of two
// approvals on two nodes exactly one executes.
//
// The write is sent through the API's own handler chain under an exec grant
// derived from this request, bound to the one request it may carry and
// usable once, with the proposal's own operation key as its Idempotency-Key.
// So a Retry of a write whose outcome nobody could confirm sends the SAME
// operation, and the tracker's ledger answers it with the first attempt
// rather than writing twice. It is sent detached from the approving request,
// so a closed tab never interrupts a write mid-flight, and bounded by the
// grant's own lifetime.
//
// # The assistant cannot reach any of this
//
// This package lives under internal/assistant, so the import walk keeps it off
// internal/api/auth: it holds grants it can name and hand to the dispatcher,
// never their tokens. The seams it calls ([Store], [Grants], [Turns]) are
// declared here and implemented by the API (the grants, over the person's own
// session) and the assistant's service (the turns).
//
// # A refusal is not an outcome in doubt
//
// A record the store refused, or a read of the assistant's records it could not
// make before anything was decided, wrote nothing, so it is returned as the
// store's own error, and the surface tells it as the condition of the node or
// the fault it is. [Unconfirmed] is kept for a record that was sent and never
// answered, and for one that states what already happened (a sent write's
// settlement, a decision sent again to learn whose it was), whose outcome
// stands whatever kept the record out. Told as unconfirmed, a refused approval
// read as one that may have landed, and a person was asked to approve again
// into a log a wait cannot clear.
//
// # A decision resumes the turn it parked
//
// Applied, failed, declined and stale proposals hand their outcome to the
// parked turn ([Turns.Resume]) on a read grant derived from the deciding
// request, so the model is told what became of its proposal in engine-authored
// words. An unknown outcome does not: the turn waits for the person's Retry. A
// page action's outcome is the page's own statement, which the engine cannot
// verify, so the model reads it as client-reported.
package approval
