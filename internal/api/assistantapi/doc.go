// Package assistantapi is the Dashboard Assistant over HTTP: `/assistant/`,
// the routes a person's drawer creates conversations, asks questions, reads
// the live stream, stops a turn, approves or declines a proposal and reports a
// page change through, every one guarded, mounted on the authority table and
// refusing in the engine's envelope.
//
// # An approval is the person's, from their own browser (ADR-0047)
//
// A proposal executes only from its owner's approval of the server-rendered
// preview its card showed: a request carrying the person's session cookie and
// a matching Origin, echoing the digest the card showed, against a proposal
// still proposed, unexpired, whose stored fields still hash to that digest
// and whose object has not moved since its preview. The decision is an
// arbitrated record, so it is made once across the fleet; the request derives
// a single-use exec grant from the approver's own session, bound to the one
// request it may carry and lasting sixty seconds; and the write is sent under
// the proposal's own operation key, so a retry after an unknown outcome is the
// same write. A step-up the write's verb asks for is answered on the approval
// request itself, and the dashboard's step-up dialog replays it.
//
// # Nobody else's conversation exists here
//
// Every route answers 404 for a conversation, a turn, a card or a proposal the
// caller does not own, never 403. And no route here is reachable through an
// assistant grant: the guard refuses `/assistant/` to one, so the assistant can
// never approve its own proposal.
//
// # The stream
//
// A turn streams as server-sent events read over fetch, resumable by
// Last-Event-ID (`<turn_id>.<seq>`), with a keepalive comment every fifteen
// seconds, in internal/assistant/wire's one encoding.
package assistantapi
