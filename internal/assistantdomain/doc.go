// Package assistantdomain is the Dashboard Assistant's state-log domain: a
// person's conversations, their turns, the messages, steps and cards the
// person saw, the files they attached or were delivered, and the proposals
// they decided, on the `CREWLET_ASSISTANT_LOG`
// stream and applied into every data node's replicated estate (ADR-0053).
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
// # Subjects
//
// The subject is the arbitration unit ([Subject]). A conversation's subject
// orders its create, its title, its purge and the OPENING OF EACH TURN, so one
// turn runs or waits on an approval per conversation whichever node opens it;
// a turn's subject orders its park, resume and close, and carries its
// messages, steps and cards as additive records under ids derived from the
// turn ([MessageID], [StepID], [CardID]), so a duplicate publish writes one
// row; a proposal's subject orders its moves, each decided from the state it
// names ([ProposalMove]), so two approvals arbitrate to one. A turn's and a
// proposal's subject id leads with its conversation's, so a conversation's
// purge gates every record about it from the subject alone. An owner's
// subject carries their erasure. Which proposal moves are legal is
// internal/assistant/proposal's state machine; this package records them.
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
// the trash reaper, decided again at the apply against activity the
// conversation's arbitration cannot see; a purge installs a gate so nothing
// later recreates it; and a person's erasure is ONE record that takes every
// conversation they own, published by the engine when the identity applier
// reports the removal and by the reaper's next pass as the backstop
// ([Writer.EraseOwner]).
//
// # Files
//
// The images a person attaches and the files a deep-work run delivers are
// kept beside the conversation ([FileStore]): each file's bytes are sealed
// under the same keyring, with associated data naming the conversation and the
// file, and uploaded as one object of the fleet's object store BEFORE the
// record naming it is published on the conversation's subject (an attachment
// exists before the turn it is sent with); the row is declared in
// internal/objstore/references, so the collector deletes the object once a
// purge or an erasure has removed the row. A conversation keeps at most
// [AttachmentsPerConversation] attachments and [DeliveredPerConversation]
// delivered files, decided in the writer's snapshot and again at the apply.
//
// Every bound on what is stored is this package's
// ([CardMaxBytes], [ParkedMaxBytes] and the rest), and the largest record they
// admit is held under the domain's declaration (spike S4).
package assistantdomain
