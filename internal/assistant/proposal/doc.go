// Package proposal is the pure logic of a Dashboard Assistant proposal: the
// digest that binds a person's approval to the request a server-side preview
// showed them, the legal moves between proposal states, and expiry. It does no
// I/O (ADR-0047).
//
// # The digest binds what the person saw to what will be sent
//
// A write tool's call becomes a proposal holding the exact request it would
// send (method, path, query and body, re-encoded by internal/api/opkey's one
// canonical encoding), the object's source revision, and the field table the
// preview read. Its digest is a SHA-256 over all of it and the owner, the
// conversation, the turn and the expiry; the card shows it, the approval
// echoes it, the stored fields are re-hashed against it, and the exec grant
// the approval derives is bound to the request digest of the one request it
// may carry. So a model cannot describe one change and have another sent, and
// a request that moved after it was approved is refused rather than executed.
//
// # States move one way, and unknown is not settled
//
// The states are internal/assistant/wire's ProposalState. A proposal moves
// from proposed to approved only through an arbitrated record (two approvals
// on two nodes, one wins), from approved to executing (a server write) or
// applying (a page change), and on to applied or failed; declined, expired and
// stale end it. Unknown is the act route's own third outcome, surfaced and
// never retried automatically: a Retry sends the SAME operation again, so the
// tracker still holds one record.
package proposal
