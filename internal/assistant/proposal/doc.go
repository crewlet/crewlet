// Package proposal is the pure logic of a Dashboard Assistant proposal: the
// digest that binds a person's approval to the request a server-side preview
// showed them, the legal moves between proposal states, expiry, and the change
// rows a card is drawn with. It does no I/O (ADR-0051); the approval path that
// carries a decision out is internal/assistant/proposal/approval.
//
// # The card is the server's preview, never the model's words
//
// A write tool's descriptor provides a [Previewer], which reads the object's
// current values as the person, through the turn's own grant, and answers a
// [Preview]: the rows of decision D10 (a fact, a value changed from one value
// to another, added, removed, or a long text as a word diff, built by [Fact],
// [Changed], [Added], [Removed], [Text] and [Value]), the audience of a write
// that changes who sees something (decision D09), whether its approval asks a
// fresh proof, the object's [Revision], and the exact request it sends. [New]
// draws the card from that and nothing else, so what the model said about a
// change is never what the person approves.
//
// # The digest binds what the person saw to what will be sent
//
// A write tool's call becomes a proposal holding the exact request it would
// send (method, path, query and body, re-encoded by internal/api/opkey's one
// canonical encoding), the object's source revision, and the field table the
// preview read. Its digest ([Digest]) is a SHA-256 over all of it and the
// owner, the conversation, the turn, the verb, whether its approval asks a
// step-up and the expiry; the card shows it, the approval
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
// stale end it ([Next], one table). Unknown is the act route's own third outcome, surfaced and
// never retried automatically: a Retry sends the SAME operation again, so the
// tracker still holds one record.
package proposal
