// Package assistantdomain is the Dashboard Assistant's state-log domain: a
// person's conversations, their turns, the messages, steps and cards the
// person saw, and the proposals they decided, on the `CREWLET_ASSISTANT_LOG`
// stream and applied into every data node's replicated estate (ADR-0049).
//
// # Who has to agree on it
//
// A person opens the drawer on any node and a reconnect may land on another,
// so a conversation is read from every data node: the replicated estate, which
// only this domain's applier writes. An approval has to be decided exactly
// once across the fleet, so a proposal's decision is an ARBITRATED record on
// the proposal's own subject, and a second approval on any node loses at the
// broker. A turn's live frames are not here at all (internal/assistant/live).
//
// # A conversation belongs to its owner
//
// Every read is keyed on the owner: a conversation, a transcript, a card's
// export or a proposal another person owns is not found, never forbidden,
// because a caller with no relation is told it does not exist (ADR-0034's
// rule). The one exception is an administrator's compliance read (decision
// D06), a separate verb that is audited as its own event naming the reader and
// the reason, recorded beside the conversation so its owner sees it.
//
// # Sealed, bounded, and gone in thirty days
//
// The estate is not encrypted at rest, so every message, card, preview and
// parked turn is sealed under the fleet keyring (internal/secrets' AES-256-GCM)
// with associated data naming the conversation and the record, as the
// identity estate seals a person's name; the applier never opens a value, so
// every node writes identical rows. What is stored is what the person saw,
// never a raw tool result (outside the bounded parked turn), a grant, a
// request header or a rendered prompt. A conversation untouched for its
// retention (thirty days by default) is purged through its own purge record by
// the trash reaper, a purge installs a gate so nothing later recreates it, and
// a person's erasure takes their conversations in the same pass.
package assistantdomain
